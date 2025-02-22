package httpapi

import (
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"reflect"
	"strconv"
	"time"

	"github.com/gorilla/mux"
	"github.com/gorilla/schema"
	"github.com/graph-gophers/graphql-go"
	opentracing "github.com/opentracing/opentracing-go"
	"github.com/sourcegraph/sourcegraph/cmd/frontend/envvar"
	"github.com/sourcegraph/sourcegraph/cmd/frontend/internal/app/pkg/updatecheck"
	apirouter "github.com/sourcegraph/sourcegraph/cmd/frontend/internal/httpapi/router"
	"github.com/sourcegraph/sourcegraph/cmd/frontend/internal/pkg/handlerutil"
	"github.com/sourcegraph/sourcegraph/cmd/frontend/registry"
	"github.com/sourcegraph/sourcegraph/internal/env"
	"github.com/sourcegraph/sourcegraph/internal/trace"
	log15 "gopkg.in/inconshreveable/log15.v2"
)

var lsifServerURLFromEnv = env.Get("LSIF_SERVER_URL", "http://lsif-server:3186", "URL at which the lsif-server service can be reached")

// NewHandler returns a new API handler that uses the provided API
// router, which must have been created by httpapi/router.New, or
// creates a new one if nil.
//
// 🚨 SECURITY: The caller MUST wrap the returned handler in middleware that checks authentication
// and sets the actor in the request context.
func NewHandler(m *mux.Router, schema *graphql.Schema, githubWebhook http.Handler) http.Handler {
	if m == nil {
		m = apirouter.New(nil)
	}
	m.StrictSlash(true)

	// Set handlers for the installed routes.
	m.Get(apirouter.RepoShield).Handler(trace.TraceRoute(handler(serveRepoShield)))

	m.Get(apirouter.RepoRefresh).Handler(trace.TraceRoute(handler(serveRepoRefresh)))

	m.Get(apirouter.Telemetry).Handler(trace.TraceRoute(telemetryHandler))

	if githubWebhook != nil {
		m.Get(apirouter.GitHubWebhooks).Handler(trace.TraceRoute(githubWebhook))
	}

	if envvar.SourcegraphDotComMode() {
		m.Path("/updates").Methods("GET").Name("updatecheck").Handler(trace.TraceRoute(http.HandlerFunc(updatecheck.Handler)))
	}

	m.Get(apirouter.GraphQL).Handler(trace.TraceRoute(handler(serveGraphQL(schema))))

	lsifServerURL, err := url.Parse(lsifServerURLFromEnv)
	if err != nil {
		log15.Error("skipping initialization of the LSIF HTTP API because the environment variable LSIF_SERVER_URL is not a valid URL", "parse_error", err, "value", lsifServerURLFromEnv)
	} else {
		proxy := httputil.NewSingleHostReverseProxy(lsifServerURL)
		m.Get(apirouter.LSIFUpload).Handler(trace.TraceRoute(http.HandlerFunc(lsifUploadProxyHandler(proxy))))
		m.Get(apirouter.LSIF).Handler(trace.TraceRoute(http.HandlerFunc(lsifProxyHandler(proxy))))
	}

	m.Get(apirouter.Registry).Handler(trace.TraceRoute(handler(registry.HandleRegistry)))

	m.NotFoundHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("API no route: %s %s from %s", r.Method, r.URL, r.Referer())
		http.Error(w, "no route", http.StatusNotFound)
	})

	return m
}

// NewInternalHandler returns a new API handler for internal endpoints that uses
// the provided API router, which must have been created by httpapi/router.NewInternal.
//
// 🚨 SECURITY: This handler should not be served on a publicly exposed port. 🚨
// This handler is not guaranteed to provide the same authorization checks as
// public API handlers.
func NewInternalHandler(m *mux.Router, schema *graphql.Schema) http.Handler {
	if m == nil {
		m = apirouter.New(nil)
	}
	m.StrictSlash(true)

	m.Get(apirouter.ExternalServiceConfigs).Handler(trace.TraceRoute(handler(serveExternalServiceConfigs)))
	m.Get(apirouter.ExternalServicesList).Handler(trace.TraceRoute(handler(serveExternalServicesList)))
	m.Get(apirouter.PhabricatorRepoCreate).Handler(trace.TraceRoute(handler(servePhabricatorRepoCreate)))
	m.Get(apirouter.ReposCreateIfNotExists).Handler(trace.TraceRoute(handler(serveReposCreateIfNotExists)))
	m.Get(apirouter.ReposUpdateMetadata).Handler(trace.TraceRoute(handler(serveReposUpdateMetadata)))
	m.Get(apirouter.ReposList).Handler(trace.TraceRoute(handler(serveReposList)))
	m.Get(apirouter.ReposListEnabled).Handler(trace.TraceRoute(handler(serveReposListEnabled)))
	m.Get(apirouter.ReposGetByName).Handler(trace.TraceRoute(handler(serveReposGetByName)))
	m.Get(apirouter.SettingsGetForSubject).Handler(trace.TraceRoute(handler(serveSettingsGetForSubject)))
	m.Get(apirouter.SavedQueriesListAll).Handler(trace.TraceRoute(handler(serveSavedQueriesListAll)))
	m.Get(apirouter.SavedQueriesGetInfo).Handler(trace.TraceRoute(handler(serveSavedQueriesGetInfo)))
	m.Get(apirouter.SavedQueriesSetInfo).Handler(trace.TraceRoute(handler(serveSavedQueriesSetInfo)))
	m.Get(apirouter.SavedQueriesDeleteInfo).Handler(trace.TraceRoute(handler(serveSavedQueriesDeleteInfo)))
	m.Get(apirouter.OrgsListUsers).Handler(trace.TraceRoute(handler(serveOrgsListUsers)))
	m.Get(apirouter.OrgsGetByName).Handler(trace.TraceRoute(handler(serveOrgsGetByName)))
	m.Get(apirouter.UsersGetByUsername).Handler(trace.TraceRoute(handler(serveUsersGetByUsername)))
	m.Get(apirouter.UserEmailsGetEmail).Handler(trace.TraceRoute(handler(serveUserEmailsGetEmail)))
	m.Get(apirouter.ExternalURL).Handler(trace.TraceRoute(handler(serveExternalURL)))
	m.Get(apirouter.CanSendEmail).Handler(trace.TraceRoute(handler(serveCanSendEmail)))
	m.Get(apirouter.SendEmail).Handler(trace.TraceRoute(handler(serveSendEmail)))
	m.Get(apirouter.GitResolveRevision).Handler(trace.TraceRoute(handler(serveGitResolveRevision)))
	m.Get(apirouter.GitTar).Handler(trace.TraceRoute(handler(serveGitTar)))
	m.Get(apirouter.Telemetry).Handler(trace.TraceRoute(telemetryHandler))
	m.Get(apirouter.GraphQL).Handler(trace.TraceRoute(handler(serveGraphQL(schema))))
	m.Get(apirouter.Configuration).Handler(trace.TraceRoute(handler(serveConfiguration)))
	m.Get(apirouter.SearchConfiguration).Handler(trace.TraceRoute(handler(serveSearchConfiguration)))
	m.Path("/ping").Methods("GET").Name("ping").HandlerFunc(handlePing)

	m.NotFoundHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("API no route: %s %s from %s", r.Method, r.URL, r.Referer())
		http.Error(w, "no route", http.StatusNotFound)
	})

	return m
}

// handler is a wrapper func for API handlers.
func handler(h func(http.ResponseWriter, *http.Request) error) http.Handler {
	return handlerutil.HandlerWithErrorReturn{
		Handler: func(w http.ResponseWriter, r *http.Request) error {
			w.Header().Set("Content-Type", "application/json")
			return h(w, r)
		},
		Error: handleError,
	}
}

var schemaDecoder = schema.NewDecoder()

func init() {
	schemaDecoder.IgnoreUnknownKeys(true)

	// Register a converter for unix timestamp strings -> time.Time values
	// (needed for Appdash PageLoadEvent type).
	schemaDecoder.RegisterConverter(time.Time{}, func(s string) reflect.Value {
		ms, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return reflect.ValueOf(err)
		}
		return reflect.ValueOf(time.Unix(0, ms*int64(time.Millisecond)))
	})
}

func handleError(w http.ResponseWriter, r *http.Request, status int, err error) {
	// Handle custom errors
	if ee, ok := err.(*handlerutil.URLMovedError); ok {
		err := handlerutil.RedirectToNewRepoName(w, r, ee.NewRepo)
		if err != nil {
			log15.Error("error redirecting to new URI", "err", err, "new_url", ee.NewRepo)
		}
		return
	}

	// Never cache error responses.
	w.Header().Set("cache-control", "no-cache, max-age=0")

	errBody := err.Error()

	var displayErrBody string
	if env.InsecureDev {
		// Only display error message to admins when in debug mode, since it may
		// contain sensitive info (like API keys in net/http error messages).
		displayErrBody = string(errBody)
	}
	http.Error(w, displayErrBody, status)
	traceSpan := opentracing.SpanFromContext(r.Context())
	var spanURL string
	if traceSpan != nil {
		spanURL = trace.SpanURL(traceSpan)
	}
	if status < 200 || status >= 500 {
		log15.Error("API HTTP handler error response", "method", r.Method, "request_uri", r.URL.RequestURI(), "status_code", status, "error", err, "trace", spanURL)
	}
}
