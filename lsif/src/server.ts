import { Queue } from 'bull'
import * as fs from 'mz/fs'
import * as path from 'path'
import bodyParser from 'body-parser'
import express from 'express'
import onFinished from 'on-finished'
import promClient from 'prom-client'
import uuid from 'uuid'
import { httpUploadDurationHistogram, httpQueryDurationHistogram, queueSizeGauge } from './server.metrics'
import {
    connectionCacheCapacityGauge,
    documentCacheCapacityGauge,
    resultChunkCacheCapacityGauge,
} from './cache.metrics'
import { dbFilename, dbFilenameOld, ensureDirectory, readEnvInt } from './util'
import { createGzip } from 'mz/zlib'
import { createPostgresConnection } from './connection'
import { Backend } from './backend'
import { logger as loggingMiddleware } from 'express-winston'
import { Logger } from 'winston'
import { pipeline as _pipeline, Readable } from 'stream'
import { promisify } from 'util'
import { readGzippedJsonElements, stringifyJsonLines, validateLsifElements } from './input'
import { wrap } from 'async-middleware'
import { XrepoDatabase } from './xrepo'
import { createTracer, logAndTraceCall, TracingContext, addTags } from './tracing'
import { Span, Tracer } from 'opentracing'
import { default as tracingMiddleware } from 'express-opentracing'
import { waitForConfiguration, ConfigurationFetcher } from './config'
import { createLogger } from './logging'
import { enqueue, createQueue, ensureOnlyRepeatableJob } from './queue'
import { Connection } from 'typeorm'
import { LsifDump } from './xrepo.models'
import * as constants from './constants'

const pipeline = promisify(_pipeline)

/**
 * Which port to run the LSIF server on. Defaults to 3186.
 */
const HTTP_PORT = readEnvInt('HTTP_PORT', 3186)

/**
 * The host and port running the redis instance containing work queues.
 *
 * Set addresses. Prefer in this order:
 *   - Specific envvar REDIS_STORE_ENDPOINT
 *   - Fallback envvar REDIS_ENDPOINT
 *   - redis-store:6379
 *
 *  Additionally keep this logic in sync with pkg/redispool/redispool.go and cmd/server/redis.go
 */
const REDIS_ENDPOINT = process.env.REDIS_STORE_ENDPOINT || process.env.REDIS_ENDPOINT || 'redis-store:6379'

/**
 * Where on the file system to store LSIF files.
 */
const STORAGE_ROOT = process.env.LSIF_STORAGE_ROOT || 'lsif-storage'

/**
 * The interval (in seconds) to schedule the heads job.
 */
const HEADS_JOB_SCHEDULE_INTERVAL = readEnvInt('HEADS_JOB_SCHEDULE_INTERVAL', 30)

/**
 * Middleware function used to convert uncaught exceptions into 500 responses.
 *
 * @param logger The logger instance.
 */
const errorHandler = (
    logger: Logger
): ((error: any, req: express.Request, res: express.Response, next: express.NextFunction) => void) => (
    error: any,
    req: express.Request,
    res: express.Response,
    next: express.NextFunction
): void => {
    if (!error || !error.status) {
        // Only log errors that don't have a status attached
        logger.error('uncaught exception', { error })
    }

    if (!res.headersSent) {
        res.status((error && error.status) || 500).send({ message: (error && error.message) || 'Unknown error' })
    }
}

/**
 * Runs the HTTP server which accepts LSIF dump uploads and responds to LSIF requests.
 *
 * @param logger The logger instance.
 */
async function main(logger: Logger): Promise<void> {
    // Collect process metrics
    promClient.collectDefaultMetrics({ prefix: 'lsif_' })

    // Read configuration from frontend
    const fetchConfiguration = await waitForConfiguration(logger)

    // Configure distributed tracing
    const tracer = createTracer('lsif-server', fetchConfiguration())

    // Update cache capacities on startup
    connectionCacheCapacityGauge.set(constants.CONNECTION_CACHE_CAPACITY)
    documentCacheCapacityGauge.set(constants.DOCUMENT_CACHE_CAPACITY)
    resultChunkCacheCapacityGauge.set(constants.RESULT_CHUNK_CACHE_CAPACITY)

    // Ensure storage roots exist
    await ensureDirectory(STORAGE_ROOT)
    await ensureDirectory(path.join(STORAGE_ROOT, 'tmp'))
    await ensureDirectory(path.join(STORAGE_ROOT, 'uploads'))

    // Create queue to publish convert
    const queue = createQueue('lsif', REDIS_ENDPOINT, logger)

    // Schedule update-tips to run on a timer
    await ensureOnlyRepeatableJob(queue, 'update-tips', {}, HEADS_JOB_SCHEDULE_INTERVAL)

    // Update queue size metric on a timer
    setInterval(() => queue.count().then(count => queueSizeGauge.set(count)), 1000)

    const app = express()

    if (tracer !== undefined) {
        app.use(tracingMiddleware({ tracer }))
    }

    app.use(
        loggingMiddleware({
            winstonInstance: logger,
            level: 'debug',
            ignoredRoutes: ['/ping', '/healthz', '/metrics'],
            requestWhitelist: ['method', 'url', 'query'],
            msg: 'request',
        })
    )
    app.use(metricsMiddleware)

    // Register endpoints
    app.use(metaEndpoints())
    app.use(await lsifEndpoints(queue, fetchConfiguration, logger, tracer))

    // Error handler must be registered last
    app.use(errorHandler(logger))

    app.listen(HTTP_PORT, () => logger.debug('listening', { port: HTTP_PORT }))
}

/**
 * If it hasn't been done already, migrate from the old pre-3.9 filename format
 * `$REPO@$COMMIT.lsif.db` to the new format `$ID.lsif.db`.
 */
async function ensureFilenamesAreIDs(db: Connection): Promise<void> {
    const doneFile = path.join(STORAGE_ROOT, 'id-based-filenames')
    if (await fs.exists(doneFile)) {
        // Already migrated.
        return
    }

    for (const dump of await db.getRepository(LsifDump).find()) {
        const oldFile = dbFilenameOld(STORAGE_ROOT, dump.repository, dump.commit)
        const newFile = dbFilename(STORAGE_ROOT, dump.id, dump.repository, dump.commit)
        if (!(await fs.exists(oldFile))) {
            continue
        }
        await fs.rename(oldFile, newFile)
    }

    // Create an empty done file to record that all files have been renamed.
    await fs.close(await fs.open(doneFile, 'w'))
}

/**
 * Middleware function used to emit HTTP durations for LSIF functions. Originally
 * we used an express bundle, but that did not allow us to have different histogram
 * bucket for different endpoints, which makes half of the metrics useless in the
 * presence of large uploads.
 */
function metricsMiddleware(req: express.Request, res: express.Response, next: express.NextFunction): void {
    let histogram: promClient.Histogram | undefined
    switch (req.path) {
        case '/upload':
            histogram = httpUploadDurationHistogram
            break

        case '/exists':
        case '/request':
            histogram = httpQueryDurationHistogram
    }

    if (histogram !== undefined) {
        const labels = { code: 0 }
        const end = histogram.startTimer(labels)

        onFinished(res, () => {
            labels.code = res.statusCode
            end()
        })
    }

    next()
}

/**
 * Create a router containing health endpoint.
 */
function metaEndpoints(): express.Router {
    const router = express.Router()
    router.get('/ping', (_, res) => res.send('ok'))
    router.get('/healthz', (_, res) => res.send('ok'))
    router.get('/metrics', (_, res) => {
        res.writeHead(200, { 'Content-Type': 'text/plain' })
        res.end(promClient.register.metrics())
    })

    return router
}

/**
 * Create a router containing the LSIF upload and query endpoints.
 *
 * @param queue The queue containing LSIF jobs.
 * @param fetchConfiguration A function that returns the current configuration.
 * @param logger The logger instance.
 * @param tracer The tracer instance.
 */
async function lsifEndpoints(
    queue: Queue,
    fetchConfiguration: ConfigurationFetcher,
    logger: Logger,
    tracer: Tracer | undefined
): Promise<express.Router> {
    const router = express.Router()

    // Create cross-repo database
    const connection = await createPostgresConnection(fetchConfiguration(), logger)
    const xrepoDatabase = new XrepoDatabase(connection)

    await ensureFilenamesAreIDs(connection)

    const backend = new Backend(STORAGE_ROOT, xrepoDatabase, fetchConfiguration)

    /**
     * Create a tracing context from the request logger and tracing span
     * tagged with the given values.
     *
     * @param req The express request.
     * @param tags The tags to apply to the logger and span.
     */
    const createTracingContext = (req: express.Request & { span?: Span }, tags: { [K: string]: any }): TracingContext =>
        addTags({ logger, span: req.span }, tags)

    router.post(
        '/upload',
        wrap(
            async (
                req: express.Request & { span?: Span },
                res: express.Response,
                next: express.NextFunction
            ): Promise<void> => {
                const { repository, commit, root, skipValidation: skipValidationRaw } = req.query
                const skipValidation = skipValidationRaw === 'true'
                checkRepository(repository)
                checkCommit(commit)

                const ctx = createTracingContext(req, { repository, commit, root })
                const filename = path.join(STORAGE_ROOT, 'uploads', uuid.v4())
                const output = fs.createWriteStream(filename)

                try {
                    await logAndTraceCall(ctx, 'uploading dump', async () => {
                        await pipeline(
                            skipValidation
                                ? req
                                : Readable.from(
                                      stringifyJsonLines(validateLsifElements(readGzippedJsonElements(req)))
                                  ).pipe(createGzip()),
                            output
                        )
                    })
                } catch (e) {
                    throw Object.assign(e, { status: 422 })
                }

                // Enqueue convert job
                logger.debug('enqueueing convert job', { repository, commit, root })
                const args = { repository, commit, root: root || '', filename }
                await enqueue(queue, 'convert', args, {}, tracer, ctx.span)
                res.send('Upload successful, queued for processing.\n')
            }
        )
    )

    router.post(
        '/exists',
        wrap(
            async (req: express.Request & { span?: Span }, res: express.Response): Promise<void> => {
                const { repository, commit, file } = req.query
                checkRepository(repository)
                checkCommit(commit)
                checkFile(file)

                const ctx = createTracingContext(req, { repository, commit })
                res.json(await backend.exists(repository, commit, file, ctx))
            }
        )
    )

    router.post(
        '/request',
        bodyParser.json({ limit: '1mb' }),
        wrap(
            async (req: express.Request & { span?: Span }, res: express.Response): Promise<void> => {
                const { repository, commit } = req.query
                const { path, position, method } = req.body
                checkRepository(repository)
                checkCommit(commit)
                checkMethod(method, ['definitions', 'references', 'hover'])
                const cleanMethod = method as 'definitions' | 'references' | 'hover'

                const ctx = createTracingContext(req, { repository, commit })
                res.json(await backend[cleanMethod](repository, commit, path, position, ctx))
            }
        )
    )

    return router
}

/**
 * Throws an error with status 400 if the repository string is invalid.
 */
export function checkRepository(repository: any): void {
    if (typeof repository !== 'string') {
        throw Object.assign(new Error('Must specify the repository (usually of the form github.com/user/repo)'), {
            status: 400,
        })
    }
}

/**
 * Throws an error with status 400 if the commit string is invalid.
 */
export function checkCommit(commit: any): void {
    if (typeof commit !== 'string' || commit.length !== 40 || !/^[0-9a-f]+$/.test(commit)) {
        throw Object.assign(new Error(`Must specify the commit as a 40 character hash ${commit}`), { status: 400 })
    }
}

/**
 * Throws an error with status 400 if the file is not present.
 */
export function checkFile(file: any): void {
    if (typeof file !== 'string') {
        throw Object.assign(new Error(`Must specify a file ${file}`), { status: 400 })
    }
}

/**
 * Throws an error with status 422 if the requested method is not supported.
 */
export function checkMethod(method: string, supportedMethods: string[]): void {
    if (!supportedMethods.includes(method)) {
        throw Object.assign(new Error(`Method must be one of ${Array.from(supportedMethods).join(', ')}`), {
            status: 422,
        })
    }
}

// Initialize logger
const appLogger = createLogger('lsif-server')

// Run app!
main(appLogger).catch(error => {
    appLogger.error('failed to start process', { error })
    appLogger.on('finish', () => process.exit(1))
    appLogger.end()
})
