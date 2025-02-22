/**
 * @jest-environment node
 */

import { Driver } from '../../../shared/src/e2e/driver'
import { getConfig } from '../../../shared/src/e2e/config'
import { getTestFixtures } from './util/init'
import * as GQL from '../../../shared/src/graphql/schema'
import { GraphQLClient } from './util/GraphQLClient'
import { ensureTestExternalService } from './util/api'
import { ensureLoggedInOrCreateTestUser } from './util/helpers'
import { buildSearchURLQuery } from '../../../shared/src/util/url'
import { TestResourceManager } from './util/TestResourceManager'

/**
 * Reads the number of results from the text at the top of the results page
 */
function getNumResults() {
    // eslint-disable-next-line @typescript-eslint/no-non-null-assertion
    const matches = document.querySelector('body')!.textContent!.match(/([0-9]+)\+?\sresults?/)
    if (!matches || matches.length < 2) {
        return null
    }
    const numResults = parseInt(matches[1], 10)
    return isNaN(numResults) ? null : numResults
}

/**
 * Returns true if "No results" message is present, throws an error if there are search results,
 * and returns false otherwise.
 */
function hasNoResultsOrError(): boolean {
    if (document.querySelectorAll('.e2e-search-result').length > 0) {
        throw new Error('Expected "No results", but there were search results.')
    }

    const resultsElem = document.querySelector('.e2e-search-results')
    if (!resultsElem) {
        return false
    }
    const resultsText = (resultsElem as HTMLElement).innerText
    if (!resultsText) {
        return false
    }
    if (resultsText.includes('No results')) {
        return true
    }
    return false
}

describe('Search regression test suite', () => {
    /**
     * Test data
     */
    const testUsername = 'test-search'
    const testExternalServiceInfo = {
        kind: GQL.ExternalServiceKind.GITHUB,
        uniqueDisplayName: '[TEST] GitHub (search.test.ts)',
    }
    const testRepoSlugs = [
        'auth0/go-jwt-middleware',
        'kyoshidajp/ghkw',
        'PalmStoneGames/kube-cert-manager',
        'adjust/go-wrk',
        'P3GLEG/Whaler',
        'sajari/docconv',
        'marianogappa/chart',
        'divan/gobenchui',
        'tuna/tunasync',
        'mthbernardes/GTRS',
        'antonmedv/expr',
        'kshvakov/clickhouse',
        'xwb1989/sqlparser',
        'henrylee2cn/pholcus_lib',
        'itcloudy/ERP',
        'iovisor/kubectl-trace',
        'minio/highwayhash',
        'matryer/moq',
        'vkuznecovas/mouthful',
        'DirectXMan12/k8s-prometheus-adapter',
        'stephens2424/php',
        'ericchiang/k8s',
        'jonmorehouse/terraform-provisioner-ansible',
        'solo-io/supergloo',
        'intel-go/bytebuf',
        'xtaci/smux',
        'MatchbookLab/local-persist',
        'ossrs/go-oryx',
        'yep/eth-tweet',
        'deckarep/gosx-notifier',
        'zentures/sequence',
        'nishanths/license',
        'beego/mux',
        'status-im/status-go',
        'antonmedv/countdown',
        'lonng/nanoserver',
        'vbauerster/mpb',
        'evilsocket/sg1',
        'zhenghaoz/gorse',
        'nsf/godit',
        '3xxx/engineercms',
        'howtowhale/dvm',
        'gosuri/uitable',
        'github/vulcanizer',
        'metaparticle-io/package',
        'bwmarrin/snowflake',
        'wyh267/FalconEngine',
        'moul/sshportal',
        'fogleman/fauxgl',
        'DataDog/datadog-agent',
        'line/line-bot-sdk-go',
        'pinterest/bender',
        'esimov/diagram',
        'nytimes/openapi2proto',
        'iris-contrib/examples',
        'munnerz/kube-plex',
        'inbucket/inbucket',
        'golangci/awesome-go-linters',
        'htcat/htcat',
        'tidwall/pinhole',
        'gocraft/health',
        'ivpusic/grpool',
        'Antonito/gfile',
        'yinqiwen/gscan',
        'facebookarchive/httpcontrol',
        'josharian/impl',
        'salihciftci/liman',
        'kelseyhightower/konfd',
        'mohanson/daze',
        'google/ko',
        'freedomofdevelopers/fod',
        'sgtest/mux',
        'facebook/react',
    ]
    const config = getConfig(
        'sudoToken',
        'sudoUsername',
        'gitHubToken',
        'sourcegraphBaseUrl',
        'noCleanup',
        'testUserPassword',
        'logBrowserConsole',
        'slowMo',
        'headless',
        'keepBrowser'
    )

    describe('Search over a dozen repositories', () => {
        let driver: Driver
        let gqlClient: GraphQLClient
        let resourceManager: TestResourceManager
        beforeAll(
            async () => {
                ;({ driver, gqlClient, resourceManager } = await getTestFixtures(config))
                resourceManager.add(
                    'User',
                    testUsername,
                    await ensureLoggedInOrCreateTestUser(driver, gqlClient, { username: testUsername, ...config })
                )
                resourceManager.add(
                    'External service',
                    testExternalServiceInfo.uniqueDisplayName,
                    await ensureTestExternalService(gqlClient, {
                        ...testExternalServiceInfo,
                        config: {
                            url: 'https://github.com',
                            token: config.gitHubToken,
                            repos: testRepoSlugs,
                            repositoryQuery: ['none'],
                        },
                        waitForRepos: testRepoSlugs.map(slug => 'github.com/' + slug),
                    })
                )
            },
            // Cloning the repositories takes ~1 minute, so give initialization 2 minutes
            2 * 60 * 1000
        )

        afterAll(async () => {
            if (!config.noCleanup) {
                await resourceManager.destroyAll()
            }
            if (driver) {
                await driver.close()
            }
        })

        test('Global text search (asdfalksdjflaksjdflkasjdf) with 0 results.', async () => {
            await driver.page.goto(config.sourcegraphBaseUrl + '/search?q=asdfalksdjflaksjdflkasjdf')
            await driver.page.waitForFunction(hasNoResultsOrError)
        })
        test('Global text search with double-quoted string constant ("error type:") with a few results.', async () => {
            await driver.page.goto(config.sourcegraphBaseUrl + '/search?q="error+type:%5Cn"')
            await driver.page.waitForFunction(() => document.querySelectorAll('.e2e-search-result').length >= 3)
        })
        test('Global text search excluding repository ("error type:") with a few results.', async () => {
            await driver.page.goto(config.sourcegraphBaseUrl + '/search?q="error+type:%5Cn"+-repo:google')
            await driver.page.waitForFunction(() => document.querySelectorAll('.e2e-search-result').length > 0)
            await driver.page.waitForFunction(() => {
                const results = Array.from(document.querySelectorAll('.e2e-search-result'))
                if (results.length === 0) {
                    return false
                }
                const hasExcludedRepo = results.some(el => el.textContent && el.textContent.includes('google'))
                if (hasExcludedRepo) {
                    throw new Error('Results contain excluded repository')
                }
                return true
            })
        })
        test('Global text search (error) with many results.', async () => {
            await driver.page.goto(config.sourcegraphBaseUrl + '/search?q=error')
            await driver.page.waitForFunction(() => document.querySelectorAll('.e2e-search-result').length > 10)
        })
        test('Global text search (error count:>1000), expect many results.', async () => {
            await driver.page.goto(config.sourcegraphBaseUrl + '/search?q=error+count:1000')
            await driver.page.waitForFunction(() => document.querySelectorAll('.e2e-search-result').length > 10)
        })
        test('Global text search (repohasfile:copying), expect many results.', async () => {
            await driver.page.goto(config.sourcegraphBaseUrl + '/search?q=repohasfile:copying')
            await driver.page.waitForFunction(() => document.querySelectorAll('.e2e-search-result').length >= 2)
        })
        test('Global text search for something with more than 1000 results and use "count:1000".', async () => {
            await driver.page.goto(config.sourcegraphBaseUrl + '/search?q=.+count:1000')
            await driver.page.waitForFunction(() => document.querySelectorAll('.e2e-search-result').length > 10)
            await driver.page.addScriptTag({ content: `${getNumResults}` })
            await driver.page.waitForFunction(() => getNumResults() !== null)
            await driver.page.waitForFunction(
                () => {
                    const numResults = getNumResults()
                    return numResults !== null && numResults > 1000
                },
                { timeout: 500 }
            )
        })
        test('Global text search for a regular expression without indexed search: (index:no ^func.*$), expect many results.', async () => {
            await driver.page.goto(config.sourcegraphBaseUrl + '/search?q=index:no+^func.*$')
            await driver.page.waitForFunction(() => document.querySelectorAll('.e2e-search-result').length > 10)
        })
        test('Global text search for a regular expression with only indexed search: (index:only ^func.*$), expect many results.', async () => {
            await driver.page.goto(config.sourcegraphBaseUrl + '/search?q=index:only+^func.*$')
            await driver.page.waitForFunction(() => document.querySelectorAll('.e2e-search-result').length > 10)
        })
        test('Search for a repository by name.', async () => {
            await driver.page.goto(config.sourcegraphBaseUrl + '/search?q=repo:auth0/go-jwt-middleware$')
            await driver.page.waitForFunction(() => {
                const results = document.querySelectorAll('.e2e-search-result')
                return results.length === 1 && (results.item(0).textContent || '').includes('go-jwt-middleware')
            })
        })
        test('Single repository, case-sensitive search.', async () => {
            await driver.page.goto(
                config.sourcegraphBaseUrl + '/search?q=repo:%5Egithub.com/adjust/go-wrk%24+String+case:yes'
            )
            await driver.page.waitForFunction(() => document.querySelectorAll('.e2e-search-result').length === 2)
        })
        test('Global text search, fork:only, few results', async () => {
            await driver.page.goto(config.sourcegraphBaseUrl + '/search?q=fork:only+router')
            await driver.page.waitForFunction(() => document.querySelectorAll('.e2e-search-result').length >= 5)
        })
        test('Global text search, fork:only, 1 result', async () => {
            await driver.page.goto(config.sourcegraphBaseUrl + '/search?q=fork:only+FORK_SENTINEL')
            await driver.page.waitForFunction(() => document.querySelectorAll('.e2e-search-result').length === 1)
        })
        test('Global text search, fork:no, 0 results', async () => {
            await driver.page.goto(config.sourcegraphBaseUrl + '/search?q=fork:only+FORK_SENTINEL')
            await driver.page.waitForFunction(() => document.querySelectorAll('.e2e-search-result').length === 0)
        })
        test('Text search non-master branch, large repository, many results', async () => {
            // The string `var ExecutionEnvironment = require('ExecutionEnvironment');` occurs 10 times on this old branch, but 0 times in current master.
            await driver.page.goto(
                config.sourcegraphBaseUrl +
                    '/search?q=repo:%5Egithub%5C.com/facebook/react%24%400.3-stable+"var+ExecutionEnvironment+%3D+require%28%27ExecutionEnvironment%27%29%3B"'
            )
            await driver.page.waitForFunction(() => document.querySelectorAll('.e2e-search-result').length === 10)
        })
        test('Global text search filtering by language', async () => {
            await driver.page.goto(config.sourcegraphBaseUrl + '/search?q=%5Cbfunc%5Cb+lang:js')
            await driver.page.waitForFunction(() => document.querySelectorAll('.e2e-search-result').length > 0)
            const filenames: string[] = await driver.page.evaluate(
                () =>
                    Array.from(document.querySelectorAll('.e2e-search-result'))
                        .map(el => {
                            const header = el.querySelector('[data-testid="result-container-header"')
                            if (!header || !header.textContent) {
                                return null
                            }
                            const components = header.textContent.split(/\s/)
                            return components[components.length - 1]
                        })
                        .filter(el => el !== null) as string[]
            )
            if (!filenames.every(filename => filename.endsWith('.js'))) {
                throw new Error('found Go results when filtering for JavaScript')
            }
        })
        test('Global search for a filename with 0 results', async () => {
            await driver.page.goto(config.sourcegraphBaseUrl + '/search?q=file:asdfasdf.go')
            await driver.page.waitForFunction(() => document.querySelectorAll('.e2e-search-result').length === 0)
        })
        test('Global search for a filename with a few results', async () => {
            await driver.page.goto(config.sourcegraphBaseUrl + '/search?q=file:router.go')
            await driver.page.waitForFunction(() => document.querySelectorAll('.e2e-search-result').length > 5)
        })
        test('Global search for a filename with many results', async () => {
            await driver.page.goto(config.sourcegraphBaseUrl + '/search?q=file:doc.go')
            await driver.page.waitForFunction(() => document.querySelectorAll('.e2e-search-result').length > 10)
            await driver.page.addScriptTag({ content: `${getNumResults}` })
            await driver.page.waitForFunction(() => getNumResults() !== null)
            await driver.page.waitForFunction(
                () => {
                    const numResults = getNumResults()
                    return numResults !== null && numResults > 25
                },
                { timeout: 500 }
            )
        })
        test('Global symbol search with many results', async () => {
            await driver.page.goto(config.sourcegraphBaseUrl + '/search?q=type:symbol+test+count:100')
            await driver.page.waitForFunction(() => document.querySelectorAll('.e2e-search-result').length > 10)
            await driver.page.addScriptTag({ content: `${getNumResults}` })
            await driver.page.waitForFunction(() => (getNumResults() || 0) >= 100)
        })
        test('Global symbol search with 0 results', async () => {
            await driver.page.goto(config.sourcegraphBaseUrl + '/search?q=type:symbol+asdfasdf')
            await driver.page.waitForFunction(hasNoResultsOrError)
        })
        test('Global symbol search ("type:symbol ^newroute count:100") with a few results', async () => {
            await driver.page.goto(config.sourcegraphBaseUrl + '/search?q=type:symbol+%5Enewroute+count:100')
            await driver.page.waitForFunction(() => document.querySelectorAll('.e2e-search-result').length > 2)
        })
        test('Indexed multiline search, many results', async () => {
            const urlQuery = buildSearchURLQuery(
                'repo:^github\\.com/facebook/react$ componentDidMount\\(\\) {\\n\\s*this',
                GQL.SearchPatternType.regexp
            )
            await driver.page.goto(config.sourcegraphBaseUrl + '/search?' + urlQuery)
            await driver.page.waitForFunction(() => document.querySelectorAll('.e2e-search-result').length > 10)
        })
        test('Non-indexed multiline search, many results', async () => {
            const urlQuery = buildSearchURLQuery(
                'repo:^github\\.com/facebook/react$ componentDidMount\\(\\) {\\n\\s*this index:no',
                GQL.SearchPatternType.regexp
            )
            await driver.page.goto(config.sourcegraphBaseUrl + '/search?' + urlQuery)
            await driver.page.waitForFunction(() => document.querySelectorAll('.e2e-search-result').length > 10)
        })
        test('Indexed multiline search, 0 results', async () => {
            const urlQuery = buildSearchURLQuery(
                'repo:^github\\.com/facebook/react$ componentDidMount\\(\\) {\\n\\s*this\\.props\\.sourcegraph\\(',
                GQL.SearchPatternType.regexp
            )
            await driver.page.goto(config.sourcegraphBaseUrl + '/search?' + urlQuery)
            await driver.page.waitForFunction(() => document.querySelectorAll('.e2e-search-result').length === 0)
        })
        test('Non-indexed multiline search, 0 results', async () => {
            const urlQuery = buildSearchURLQuery(
                'repo:^github\\.com/facebook/react$ componentDidMount\\(\\) {\\n\\s*this\\.props\\.sourcegraph\\( index:no',
                GQL.SearchPatternType.regexp
            )
            await driver.page.goto(config.sourcegraphBaseUrl + '/search' + urlQuery)
            await driver.page.waitForFunction(() => document.querySelectorAll('.e2e-search-result').length === 0)
        })
    })
})
