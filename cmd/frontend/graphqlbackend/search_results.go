package graphqlbackend

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sourcegraph/sourcegraph/cmd/frontend/internal/inventory"
	"github.com/sourcegraph/sourcegraph/cmd/frontend/types"

	"github.com/hashicorp/go-multierror"
	"github.com/neelance/parallel"
	"github.com/opentracing/opentracing-go"
	"github.com/opentracing/opentracing-go/ext"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"

	"gopkg.in/inconshreveable/log15.v2"

	"github.com/sourcegraph/sourcegraph/cmd/frontend/internal/goroutine"
	"github.com/sourcegraph/sourcegraph/cmd/frontend/internal/pkg/search"
	"github.com/sourcegraph/sourcegraph/cmd/frontend/internal/pkg/search/query"
	"github.com/sourcegraph/sourcegraph/internal/api"
	"github.com/sourcegraph/sourcegraph/internal/gitserver"
	"github.com/sourcegraph/sourcegraph/internal/rcache"
	"github.com/sourcegraph/sourcegraph/internal/trace"
	"github.com/sourcegraph/sourcegraph/internal/vcs/git"
)

// searchResultsCommon contains fields that should be returned by all funcs
// that contribute to the overall search result set.
type searchResultsCommon struct {
	limitHit bool // whether the limit on results was hit

	repos    []*types.Repo             // repos that were matched by the repo-related filters
	searched []*types.Repo             // repos that were searched
	indexed  []*types.Repo             // repos that were searched using an index
	cloning  []*types.Repo             // repos that could not be searched because they were still being cloned
	missing  []*types.Repo             // repos that could not be searched because they do not exist
	partial  map[api.RepoName]struct{} // repos that were searched, but have results that were not returned due to exceeded limits

	maxResultsCount, resultCount int32

	// timedout usually contains repos that haven't finished being fetched yet.
	// This should only happen for large repos and the searcher caches are
	// purged.
	timedout []*types.Repo

	indexUnavailable bool // True if indexed search is enabled but was not available during this search.
}

func (c *searchResultsCommon) LimitHit() bool {
	return c.limitHit || c.resultCount > c.maxResultsCount
}

func (c *searchResultsCommon) Repositories() []*RepositoryResolver {
	return RepositoryResolvers(c.repos)
}

func (c *searchResultsCommon) RepositoriesSearched() []*RepositoryResolver {
	return RepositoryResolvers(c.searched)
}

func (c *searchResultsCommon) IndexedRepositoriesSearched() []*RepositoryResolver {
	return RepositoryResolvers(c.indexed)
}

func (c *searchResultsCommon) Cloning() []*RepositoryResolver {
	return RepositoryResolvers(c.cloning)
}

func (c *searchResultsCommon) Missing() []*RepositoryResolver {
	return RepositoryResolvers(c.missing)
}

func (c *searchResultsCommon) Timedout() []*RepositoryResolver {
	return RepositoryResolvers(c.timedout)
}

func (c *searchResultsCommon) IndexUnavailable() bool {
	return c.indexUnavailable
}

func RepositoryResolvers(repos types.Repos) []*RepositoryResolver {
	dedupSort(&repos)
	return toRepositoryResolvers(repos)
}

// update updates c with the other data, deduping as necessary. It modifies c but
// does not modify other.
func (c *searchResultsCommon) update(other searchResultsCommon) {
	c.limitHit = c.limitHit || other.limitHit
	c.indexUnavailable = c.indexUnavailable || other.indexUnavailable

	c.repos = append(c.repos, other.repos...)
	c.searched = append(c.searched, other.searched...)
	c.indexed = append(c.indexed, other.indexed...)
	c.cloning = append(c.cloning, other.cloning...)
	c.missing = append(c.missing, other.missing...)
	c.timedout = append(c.timedout, other.timedout...)
	c.resultCount += other.resultCount

	if c.partial == nil {
		c.partial = make(map[api.RepoName]struct{})
	}

	for repo := range other.partial {
		c.partial[repo] = struct{}{}
	}
}

// dedupSort sorts (by ID in ascending order) and deduplicates
// the given repos in-place.
func dedupSort(repos *types.Repos) {
	if len(*repos) == 0 {
		return
	}

	sort.Sort(*repos)

	j := 0
	for i := 1; i < len(*repos); i++ {
		if (*repos)[j].ID != (*repos)[i].ID {
			j++
			(*repos)[j] = (*repos)[i]
		}
	}

	*repos = (*repos)[:j+1]
}

// searchResultsResolver is a resolver for the GraphQL type `SearchResults`
type searchResultsResolver struct {
	results []searchResultResolver
	searchResultsCommon
	alert *searchAlert
	start time.Time // when the results started being computed

	// cursor to return for paginated search requests, or nil if the request
	// wasn't paginated.
	cursor *searchCursor
}

func (sr *searchResultsResolver) Results() []searchResultResolver {
	return sr.results
}

func (sr *searchResultsResolver) MatchCount() int32 {
	var totalResults int32
	for _, result := range sr.results {
		totalResults += result.resultCount()
	}
	return totalResults
}

func (sr *searchResultsResolver) ResultCount() int32 { return sr.MatchCount() }

func (sr *searchResultsResolver) ApproximateResultCount() string {
	count := sr.ResultCount()
	if sr.LimitHit() || len(sr.cloning) > 0 || len(sr.timedout) > 0 {
		return fmt.Sprintf("%d+", count)
	}
	return strconv.Itoa(int(count))
}

func (sr *searchResultsResolver) Alert() *searchAlert { return sr.alert }

func (sr *searchResultsResolver) ElapsedMilliseconds() int32 {
	return int32(time.Since(sr.start).Nanoseconds() / int64(time.Millisecond))
}

// commonFileFilters are common filters used. It is used by DynamicFilters to
// propose them if they match shown results.
var commonFileFilters = []struct {
	Regexp *regexp.Regexp
	Filter string
}{
	// Exclude go tests
	{
		Regexp: regexp.MustCompile(`_test\.go$`),
		Filter: `-file:_test\.go$`,
	},
	// Exclude go vendor
	{
		Regexp: regexp.MustCompile(`(^|/)vendor/`),
		Filter: `-file:(^|/)vendor/`,
	},
	// Exclude node_modules
	{
		Regexp: regexp.MustCompile(`(^|/)node_modules/`),
		Filter: `-file:(^|/)node_modules/`,
	},
}

func (sr *searchResultsResolver) DynamicFilters() []*searchFilterResolver {
	filters := map[string]*searchFilterResolver{}
	repoToMatchCount := make(map[string]int)
	add := func(value string, label string, count int, limitHit bool, kind string) {
		sf, ok := filters[value]
		if !ok {
			sf = &searchFilterResolver{
				value:    value,
				label:    label,
				count:    int32(count),
				limitHit: limitHit,
				kind:     kind,
			}
			filters[value] = sf
		} else {
			sf.count = int32(count)
		}

		sf.score++
	}

	addRepoFilter := func(uri string, rev string, lineMatchCount int) {
		filter := fmt.Sprintf(`repo:^%s$`, regexp.QuoteMeta(uri))
		if rev != "" {
			filter = filter + fmt.Sprintf(`@%s`, regexp.QuoteMeta(rev))
		}
		_, limitHit := sr.searchResultsCommon.partial[api.RepoName(uri)]
		// Increment number of matches per repo. Add will override previous entry for uri
		repoToMatchCount[uri] += lineMatchCount
		add(filter, uri, repoToMatchCount[uri], limitHit, "repo")
	}

	addFileFilter := func(fileMatchPath string, lineMatchCount int, limitHit bool) {
		for _, ff := range commonFileFilters {
			if ff.Regexp.MatchString(fileMatchPath) {
				add(ff.Filter, ff.Filter, lineMatchCount, limitHit, "file")
			}
		}
	}

	addLangFilter := func(fileMatchPath string, lineMatchCount int, limitHit bool) {
		extensionToLanguageLookup := func(path string) string {
			language, _ := inventory.GetLanguageByFilename(path)
			return strings.ToLower(language)
		}
		if ext := path.Ext(fileMatchPath); ext != "" {
			language := extensionToLanguageLookup(fileMatchPath)
			if language != "" {
				value := fmt.Sprintf(`lang:%s`, language)
				add(value, value, lineMatchCount, limitHit, "lang")
			}
		}
	}

	for _, result := range sr.results {
		if fm, ok := result.ToFileMatch(); ok {
			rev := ""
			if fm.inputRev != nil {
				rev = *fm.inputRev
			}
			addRepoFilter(string(fm.repo.Name), rev, len(fm.LineMatches()))
			addLangFilter(fm.JPath, len(fm.LineMatches()), fm.JLimitHit)
			addFileFilter(fm.JPath, len(fm.LineMatches()), fm.JLimitHit)

			if len(fm.symbols) > 0 {
				add("type:symbol", "type:symbol", 1, fm.JLimitHit, "symbol")
			}
		} else if r, ok := result.ToRepository(); ok {
			// It should be fine to leave this blank since revision specifiers
			// can only be used with the 'repo:' scope. In that case,
			// we shouldn't be getting any repositoy name matches back.
			addRepoFilter(r.Name(), "", 1)
		}
		// Add `case:yes` filter to offer easier access to search results matching with case sensitive set to yes
		// We use count == 0 and limitHit == false since we can't determine that information without
		// running the search query. This causes it to display as just `case:yes`.
		add("case:yes", "case:yes", 0, false, "case")
	}

	filterSlice := make([]*searchFilterResolver, 0, len(filters))
	repoFilterSlice := make([]*searchFilterResolver, 0, len(filters)/2) // heuristic - half of all filters are repo filters.
	for _, f := range filters {
		if f.kind == "repo" {
			repoFilterSlice = append(repoFilterSlice, f)
		} else {
			filterSlice = append(filterSlice, f)
		}
	}
	sort.Slice(filterSlice, func(i, j int) bool {
		return filterSlice[j].score < filterSlice[i].score
	})
	// limit amount of non-repo filters to be rendered arbitrarily to 12
	if len(filterSlice) > 12 {
		filterSlice = filterSlice[:12]
	}

	allFilters := append(filterSlice, repoFilterSlice...)
	sort.Slice(allFilters, func(i, j int) bool {
		return allFilters[j].score < allFilters[i].score
	})

	return allFilters
}

type searchFilterResolver struct {
	value string

	// the string to be displayed in the UI
	label string

	// the number of matches in a particular repository. Only used for `repo:` filters.
	count int32

	// whether the results returned for a repository are incomplete
	limitHit bool

	// the kind of filter. Should be "repo", "file", or "lang".
	kind string

	// score is used to select potential filters
	score int
}

func (sf *searchFilterResolver) Value() string {
	return sf.value
}

func (sf *searchFilterResolver) Label() string {
	return sf.label
}

func (sf *searchFilterResolver) Count() int32 {
	return sf.count
}

func (sf *searchFilterResolver) LimitHit() bool {
	return sf.limitHit
}

func (sf *searchFilterResolver) Kind() string {
	return sf.kind
}

// blameFileMatch blames the specified file match to produce the time at which
// the first line match inside of it was authored.
func (sr *searchResultsResolver) blameFileMatch(ctx context.Context, fm *fileMatchResolver) (t time.Time, err error) {
	span, ctx := opentracing.StartSpanFromContext(ctx, "blameFileMatch")
	defer func() {
		if err != nil {
			ext.Error.Set(span, true)
			span.SetTag("err", err.Error())
		}
		span.Finish()
	}()

	// Blame the first line match.
	lineMatches := fm.LineMatches()
	if len(lineMatches) == 0 {
		// No line match
		return time.Time{}, nil
	}
	lm := fm.LineMatches()[0]
	hunks, err := git.BlameFile(ctx, gitserver.Repo{Name: fm.repo.Name}, fm.JPath, &git.BlameOptions{
		NewestCommit: fm.commitID,
		StartLine:    int(lm.LineNumber()),
		EndLine:      int(lm.LineNumber()),
	})
	if err != nil {
		return time.Time{}, err
	}

	return hunks[0].Author.Date, nil
}

func (sr *searchResultsResolver) Sparkline(ctx context.Context) (sparkline []int32, err error) {
	var (
		days     = 30                 // number of days the sparkline represents
		maxBlame = 100                // maximum number of file results to blame for date/time information.
		run      = parallel.NewRun(8) // number of concurrent blame ops
	)

	var (
		sparklineMu sync.Mutex
		blameOps    = 0
	)
	sparkline = make([]int32, days)
	addPoint := func(t time.Time) {
		// Check if the author date of the search result is inside of our sparkline
		// timerange.
		now := time.Now()
		if t.Before(now.Add(-time.Duration(len(sparkline)) * 24 * time.Hour)) {
			// Outside the range of the sparkline.
			return
		}
		sparklineMu.Lock()
		defer sparklineMu.Unlock()
		for n := range sparkline {
			d1 := now.Add(-time.Duration(n) * 24 * time.Hour)
			d2 := now.Add(-time.Duration(n-1) * 24 * time.Hour)
			if t.After(d1) && t.Before(d2) {
				sparkline[n]++ // on the nth day
			}
		}
	}

	// Consider all of our search results as a potential data point in our
	// sparkline.
loop:
	for _, r := range sr.results {
		r := r // shadow so it doesn't change in the goroutine
		switch m := r.(type) {
		case *RepositoryResolver:
			// We don't care about repo results here.
			continue
		case *commitSearchResultResolver:
			// Diff searches are cheap, because we implicitly have author date info.
			addPoint(m.commit.author.date)
		case *fileMatchResolver:
			// File match searches are more expensive, because we must blame the
			// (first) line in order to know its placement in our sparkline.
			blameOps++
			if blameOps > maxBlame {
				// We have exceeded our budget of blame operations for
				// calculating this sparkline, so don't do any more file match
				// blaming.
				continue loop
			}

			run.Acquire()
			goroutine.Go(func() {
				defer run.Release()

				// Blame the file match in order to retrieve date informatino.
				var err error
				t, err := sr.blameFileMatch(ctx, m)
				if err != nil {
					log15.Warn("failed to blame fileMatch during sparkline generation", "error", err)
					return
				}
				addPoint(t)
			})
		case *codemodResultResolver:
			continue
		default:
			panic("SearchResults.Sparkline unexpected union type state")
		}
	}
	span := opentracing.SpanFromContext(ctx)
	span.SetTag("blame_ops", blameOps)
	return sparkline, nil
}

func (r *searchResolver) Results(ctx context.Context) (*searchResultsResolver, error) {
	// If the request is a paginated one, we handle it separately. See
	// paginatedResults for more details.
	if r.pagination != nil {
		return r.paginatedResults(ctx)
	}

	rr, err := r.resultsWithTimeoutSuggestion(ctx)
	if err != nil {
		return nil, err
	}

	return rr, nil
}

// resultsWithTimeoutSuggestion calls doResults, and in case of deadline
// exceeded returns a search alert with a did-you-mean link for the same
// query with a longer timeout.
func (r *searchResolver) resultsWithTimeoutSuggestion(ctx context.Context) (*searchResultsResolver, error) {
	start := time.Now()
	rr, err := r.doResults(ctx, "")
	if err != nil {
		if err == context.DeadlineExceeded {
			dt := time.Since(start)
			dt2 := longer(2, dt)
			rr = &searchResultsResolver{
				alert: &searchAlert{
					title:       "Timeout",
					description: fmt.Sprintf("Deadline exceeded after about %s.", roundStr(dt.String())),
					proposedQueries: []*searchQueryDescription{
						{
							description: "query with longer timeout",
							query:       fmt.Sprintf("timeout:%v %s", dt2, omitQueryFields(r, query.FieldTimeout)),
						},
					},
				},
			}
			return rr, nil
		}
		return nil, err
	}
	return rr, nil
}

// longer returns a suggested longer time to wait if the given duration wasn't long enough.
func longer(N int, dt time.Duration) time.Duration {
	dt2 := func() time.Duration {
		Ndt := time.Duration(N) * dt
		dceil := func(x float64) time.Duration {
			return time.Duration(math.Ceil(x))
		}
		switch {
		case math.Floor(Ndt.Hours()) > 0:
			return dceil(Ndt.Hours()) * time.Hour
		case math.Floor(Ndt.Minutes()) > 0:
			return dceil(Ndt.Minutes()) * time.Minute
		case math.Floor(Ndt.Seconds()) > 0:
			return dceil(Ndt.Seconds()) * time.Second
		default:
			return 0
		}
	}()
	lowest := 2 * time.Second
	if dt2 < lowest {
		return lowest
	}
	return dt2
}

var decimalRx = regexp.MustCompile(`\d+\.\d+`)

// roundStr rounds the first number containing a decimal within a string
func roundStr(s string) string {
	return decimalRx.ReplaceAllStringFunc(s, func(ns string) string {
		f, err := strconv.ParseFloat(ns, 64)
		if err != nil {
			return s
		}
		f = math.Round(f)
		return fmt.Sprintf("%d", int(f))
	})
}

type searchResultsStats struct {
	JApproximateResultCount string
	JSparkline              []int32
}

func (srs *searchResultsStats) ApproximateResultCount() string { return srs.JApproximateResultCount }
func (srs *searchResultsStats) Sparkline() []int32             { return srs.JSparkline }

var (
	searchResultsStatsCache   = rcache.NewWithTTL("search_results_stats", 3600) // 1h
	searchResultsStatsCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "src",
		Subsystem: "graphql",
		Name:      "search_results_stats_cache_hit",
		Help:      "Counts cache hits and misses for search results stats (e.g. sparklines).",
	}, []string{"type"})
)

func init() {
	prometheus.MustRegister(searchResultsStatsCounter)
}

func (r *searchResolver) Stats(ctx context.Context) (stats *searchResultsStats, err error) {
	// Override user context to ensure that stats for this query are cached
	// regardless of the user context's cancellation. For example, if
	// stats/sparklines are slow to load on the homepage and all users navigate
	// away from that page before they load, no user would ever see them and we
	// would never cache them. This fixes that by ensuring the first request
	// 'kicks off loading' and places the result into cache regardless of
	// whether or not the original querier of this information still wants it.
	originalCtx := ctx
	ctx = context.Background()
	ctx = opentracing.ContextWithSpan(ctx, opentracing.SpanFromContext(originalCtx))

	cacheKey := r.rawQuery()
	// Check if value is in the cache.
	jsonRes, ok := searchResultsStatsCache.Get(cacheKey)
	if ok {
		searchResultsStatsCounter.WithLabelValues("hit").Inc()
		if err := json.Unmarshal(jsonRes, &stats); err != nil {
			return nil, err
		}
		return stats, nil
	}

	// Calculate value from scratch.
	searchResultsStatsCounter.WithLabelValues("miss").Inc()
	attempts := 0
	var v *searchResultsResolver
	for {
		// Query search results.
		var err error
		v, err = r.doResults(ctx, "")
		if err != nil {
			return nil, err // do not cache errors.
		}
		if v.ResultCount() > 0 {
			break
		}

		cloning := len(v.Cloning())
		timedout := len(v.Timedout())
		if cloning == 0 && timedout == 0 {
			break // zero results, but no cloning or timed out repos. No point in retrying.
		}

		if attempts > 5 {
			log15.Error("failed to generate sparkline due to cloning or timed out repos", "cloning", len(v.Cloning()), "timedout", len(v.Timedout()))
			return nil, fmt.Errorf("failed to generate sparkline due to %d cloning %d timedout repos", len(v.Cloning()), len(v.Timedout()))
		}

		// We didn't find any search results. Some repos are cloning or timed
		// out, so try again in a few seconds.
		attempts++
		log15.Warn("sparkline generation found 0 search results due to cloning or timed out repos (retrying in 5s)", "cloning", len(v.Cloning()), "timedout", len(v.Timedout()))
		time.Sleep(5 * time.Second)
	}

	sparkline, err := v.Sparkline(ctx)
	if err != nil {
		return nil, err // sparkline generation failed, so don't cache.
	}
	stats = &searchResultsStats{
		JApproximateResultCount: v.ApproximateResultCount(),
		JSparkline:              sparkline,
	}

	// Store in the cache if we got non-zero results. If we got zero results,
	// it should be quick and caching is not desired because e.g. it could be
	// a query for a repo that has not been added by the user yet.
	if v.ResultCount() > 0 {
		jsonRes, err = json.Marshal(stats)
		if err != nil {
			return nil, err
		}
		searchResultsStatsCache.Set(cacheKey, jsonRes)
	}
	return stats, nil
}

type getPatternInfoOptions struct {
	// forceFileSearch, when true, specifies that the search query should be
	// treated as if every default term had `file:` before it. This can be used
	// to allow users to jump to files by just typing their name.
	forceFileSearch bool
}

// getPatternInfo gets the search pattern info for the query in the resolver.
func (r *searchResolver) getPatternInfo(opts *getPatternInfoOptions) (*search.PatternInfo, error) {
	var patternsToCombine []string
	if opts == nil || !opts.forceFileSearch {
		for _, v := range r.query.Values(query.FieldDefault) {
			// Treat quoted strings as literal strings to match, not regexps.
			var pattern string
			switch {
			case v.String != nil:
				pattern = regexp.QuoteMeta(*v.String)
			case v.Regexp != nil:
				pattern = v.Regexp.String()
			}
			if pattern == "" {
				continue
			}
			patternsToCombine = append(patternsToCombine, pattern)
		}
	} else {
		// TODO: We must have some pattern that always matches here, or else
		// cmd/searcher/search/matcher.go:97 would cause a nil regexp panic
		// when not using indexed search. I am unsure what the right solution
		// is here. Would this code path go away when we switch fully to
		// indexed search @keegan? This workaround is OK for now though.
		patternsToCombine = append(patternsToCombine, ".")
	}

	// Handle file: and -file: filters.
	includePatterns, excludePatterns := r.query.RegexpPatterns(query.FieldFile)
	filePatternsReposMustInclude, filePatternsReposMustExclude := r.query.RegexpPatterns(query.FieldRepoHasFile)

	if opts != nil && opts.forceFileSearch {
		for _, v := range r.query.Values(query.FieldDefault) {
			includePatterns = append(includePatterns, asString(v))
		}
	}

	// Handle lang: and -lang: filters.
	langIncludePatterns, langExcludePatterns, err := langIncludeExcludePatterns(r.query.StringValues(query.FieldLang))
	if err != nil {
		return nil, err
	}
	includePatterns = append(includePatterns, langIncludePatterns...)
	excludePatterns = append(excludePatterns, langExcludePatterns...)

	patternInfo := &search.PatternInfo{
		IsRegExp:                     true,
		IsCaseSensitive:              r.query.IsCaseSensitive(),
		FileMatchLimit:               r.maxResults(),
		Pattern:                      regexpPatternMatchingExprsInOrder(patternsToCombine),
		IncludePatterns:              includePatterns,
		FilePatternsReposMustInclude: filePatternsReposMustInclude,
		FilePatternsReposMustExclude: filePatternsReposMustExclude,
		PathPatternsAreRegExps:       true,
		PathPatternsAreCaseSensitive: r.query.IsCaseSensitive(),
	}
	if len(excludePatterns) > 0 {
		patternInfo.ExcludePattern = unionRegExps(excludePatterns)
	}
	return patternInfo, nil
}

var (
	// The default timeout to use for queries.
	defaultTimeout = 10 * time.Second
	// The max timeout to use for queries.
	maxTimeout = time.Minute
)

func (r *searchResolver) searchTimeoutFieldSet() bool {
	timeout, _ := r.query.StringValue(query.FieldTimeout)
	return timeout != "" || r.countIsSet()
}

func (r *searchResolver) withTimeout(ctx context.Context) (context.Context, context.CancelFunc, error) {
	d := defaultTimeout
	timeout, _ := r.query.StringValue(query.FieldTimeout)
	if timeout != "" {
		var err error
		d, err = time.ParseDuration(timeout)
		if err != nil {
			return nil, nil, errors.WithMessage(err, `invalid "timeout:" value (examples: "timeout:2s", "timeout:200ms")`)
		}
	} else if r.countIsSet() {
		// If `count:` is set but `timeout:` is not explicitely set, use the max timeout
		d = maxTimeout
	}
	// don't run queries longer than 1 minute.
	if d.Minutes() > 1 {
		d = maxTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, d)
	return ctx, cancel, nil
}

func (r *searchResolver) determineResultTypes(args search.Args, forceOnlyResultType string) (resultTypes []string, seenResultTypes map[string]struct{}) {
	// Determine which types of results to return.
	if forceOnlyResultType != "" {
		resultTypes = []string{forceOnlyResultType}
	} else if len(r.query.Values(query.FieldReplace)) > 0 {
		resultTypes = []string{"codemod"}
	} else {
		resultTypes, _ = r.query.StringValues(query.FieldType)
		if len(resultTypes) == 0 {
			resultTypes = []string{"file", "path", "repo", "ref"}
		}
	}
	seenResultTypes = make(map[string]struct{}, len(resultTypes))
	for _, resultType := range resultTypes {
		if resultType == "file" {
			args.Pattern.PatternMatchesContent = true
		} else if resultType == "path" {
			args.Pattern.PatternMatchesPath = true
		}
	}
	return resultTypes, seenResultTypes
}

func (r *searchResolver) determineRepos(ctx context.Context, tr *trace.Trace, start time.Time) (repos, missingRepoRevs []*search.RepositoryRevisions, res *searchResultsResolver, err error) {
	repos, missingRepoRevs, overLimit, err := r.resolveRepositories(ctx, nil)
	if err != nil {
		return nil, nil, nil, err
	}
	tr.LazyPrintf("searching %d repos, %d missing", len(repos), len(missingRepoRevs))
	if len(repos) == 0 {
		alert, err := r.alertForNoResolvedRepos(ctx)
		if err != nil {
			return nil, nil, nil, err
		}
		return nil, nil, &searchResultsResolver{alert: alert, start: start}, nil
	}
	if overLimit {
		alert, err := r.alertForOverRepoLimit(ctx)
		if err != nil {
			return nil, nil, nil, err
		}
		return nil, nil, &searchResultsResolver{alert: alert, start: start}, nil
	}
	return repos, missingRepoRevs, nil, nil
}

func (r *searchResolver) doResults(ctx context.Context, forceOnlyResultType string) (res *searchResultsResolver, err error) {
	tr, ctx := trace.New(ctx, "graphql.SearchResults", r.rawQuery())
	defer func() {
		tr.SetError(err)
		tr.Finish()
	}()

	start := time.Now()

	ctx, cancel, err := r.withTimeout(ctx)
	if err != nil {
		return nil, err
	}
	defer cancel()

	repos, missingRepoRevs, alertResult, err := r.determineRepos(ctx, tr, start)
	if err != nil {
		return nil, err
	}
	if alertResult != nil {
		return alertResult, nil
	}

	p, err := r.getPatternInfo(nil)
	if err != nil {
		return nil, err
	}
	args := search.Args{
		Pattern:         p,
		Repos:           repos,
		Query:           r.query,
		UseFullDeadline: r.searchTimeoutFieldSet(),
		Zoekt:           r.zoekt,
		SearcherURLs:    r.searcherURLs,
	}
	if err := args.Pattern.Validate(); err != nil {
		return nil, &badRequestError{err}
	}

	err = validateRepoHasFileUsage(r.query)
	if err != nil {
		return nil, err
	}

	resultTypes, seenResultTypes := r.determineResultTypes(args, forceOnlyResultType)
	tr.LazyPrintf("resultTypes: %v", resultTypes)

	var (
		requiredWg sync.WaitGroup
		optionalWg sync.WaitGroup
		results    []searchResultResolver
		resultsMu  sync.Mutex
		common     = searchResultsCommon{maxResultsCount: r.maxResults()}
		commonMu   sync.Mutex
		multiErr   *multierror.Error
		multiErrMu sync.Mutex
		// fileMatches is a map from git:// URI of the file to FileMatch resolver
		// to merge multiple results of different types for the same file
		fileMatches   = make(map[string]*fileMatchResolver)
		fileMatchesMu sync.Mutex
	)

	waitGroup := func(required bool) *sync.WaitGroup {
		if args.UseFullDeadline {
			// When a custom timeout is specified, all searches are required and get the full timeout.
			return &requiredWg
		}
		if required {
			return &requiredWg
		}
		return &optionalWg
	}

	searchedFileContentsOrPaths := false
	for _, resultType := range resultTypes {
		resultType := resultType // shadow so it doesn't change in the goroutine
		if _, seen := seenResultTypes[resultType]; seen {
			continue
		}
		seenResultTypes[resultType] = struct{}{}
		switch resultType {
		case "repo":
			// Search for repos
			wg := waitGroup(true)
			wg.Add(1)
			goroutine.Go(func() {
				defer wg.Done()

				repoResults, repoCommon, err := searchRepositories(ctx, &args, r.maxResults())
				// Timeouts are reported through searchResultsCommon so don't report an error for them
				if err != nil && !isContextError(ctx, err) {
					multiErrMu.Lock()
					multiErr = multierror.Append(multiErr, errors.Wrap(err, "repository search failed"))
					multiErrMu.Unlock()
				}
				if repoResults != nil {
					resultsMu.Lock()
					results = append(results, repoResults...)
					resultsMu.Unlock()
				}
				if repoCommon != nil {
					commonMu.Lock()
					common.update(*repoCommon)
					commonMu.Unlock()
				}
			})
		case "symbol":
			wg := waitGroup(len(resultTypes) == 1)
			wg.Add(1)
			goroutine.Go(func() {
				defer wg.Done()

				symbolFileMatches, symbolsCommon, err := searchSymbols(ctx, &args, int(r.maxResults()))
				// Timeouts are reported through searchResultsCommon so don't report an error for them
				if err != nil && !isContextError(ctx, err) {
					multiErrMu.Lock()
					multiErr = multierror.Append(multiErr, errors.Wrap(err, "symbol search failed"))
					multiErrMu.Unlock()
				}
				for _, symbolFileMatch := range symbolFileMatches {
					key := symbolFileMatch.uri
					fileMatchesMu.Lock()
					if m, ok := fileMatches[key]; ok {
						m.symbols = symbolFileMatch.symbols
					} else {
						fileMatches[key] = symbolFileMatch
						resultsMu.Lock()
						results = append(results, symbolFileMatch)
						resultsMu.Unlock()
					}
					fileMatchesMu.Unlock()
				}
				if symbolsCommon != nil {
					commonMu.Lock()
					common.update(*symbolsCommon)
					commonMu.Unlock()
				}
			})
		case "file", "path":
			if searchedFileContentsOrPaths {
				// type:file and type:path use same searchFilesInRepos, so don't call 2x.
				continue
			}
			searchedFileContentsOrPaths = true
			wg := waitGroup(true)
			wg.Add(1)
			goroutine.Go(func() {
				defer wg.Done()

				fileResults, fileCommon, err := searchFilesInRepos(ctx, &args)
				// Timeouts are reported through searchResultsCommon so don't report an error for them
				if err != nil && !(err == context.DeadlineExceeded || err == context.Canceled) {
					multiErrMu.Lock()
					multiErr = multierror.Append(multiErr, errors.Wrap(err, "text search failed"))
					multiErrMu.Unlock()
				}
				for _, r := range fileResults {
					key := r.uri
					fileMatchesMu.Lock()
					m, ok := fileMatches[key]
					if ok {
						// merge line match results with an existing symbol result
						m.JLimitHit = m.JLimitHit || r.JLimitHit
						m.JLineMatches = r.JLineMatches
					} else {
						fileMatches[key] = r
						resultsMu.Lock()
						results = append(results, r)
						resultsMu.Unlock()
					}
					fileMatchesMu.Unlock()
				}
				if fileCommon != nil {
					commonMu.Lock()
					common.update(*fileCommon)
					commonMu.Unlock()
				}
			})
		case "diff":
			wg := waitGroup(len(resultTypes) == 1)
			wg.Add(1)
			goroutine.Go(func() {
				defer wg.Done()
				diffResults, diffCommon, err := searchCommitDiffsInRepos(ctx, &args)
				// Timeouts are reported through searchResultsCommon so don't report an error for them
				if err != nil && !isContextError(ctx, err) {
					multiErrMu.Lock()
					multiErr = multierror.Append(multiErr, errors.Wrap(err, "diff search failed"))
					multiErrMu.Unlock()
				}
				if diffResults != nil {
					resultsMu.Lock()
					results = append(results, diffResults...)
					resultsMu.Unlock()
				}
				if diffCommon != nil {
					commonMu.Lock()
					common.update(*diffCommon)
					commonMu.Unlock()
				}
			})
		case "commit":
			wg := waitGroup(len(resultTypes) == 1)
			wg.Add(1)
			goroutine.Go(func() {
				defer wg.Done()

				commitResults, commitCommon, err := searchCommitLogInRepos(ctx, &args)
				// Timeouts are reported through searchResultsCommon so don't report an error for them
				if err != nil && !isContextError(ctx, err) {
					multiErrMu.Lock()
					multiErr = multierror.Append(multiErr, errors.Wrap(err, "commit search failed"))
					multiErrMu.Unlock()
				}
				if commitResults != nil {
					resultsMu.Lock()
					results = append(results, commitResults...)
					resultsMu.Unlock()
				}
				if commitCommon != nil {
					commonMu.Lock()
					common.update(*commitCommon)
					commonMu.Unlock()
				}
			})
		case "codemod":
			wg := waitGroup(true)
			wg.Add(1)
			goroutine.Go(func() {
				defer wg.Done()

				codemodResults, codemodCommon, err := performCodemod(ctx, &args)
				// Timeouts are reported through searchResultsCommon so don't report an error for them
				if err != nil && !isContextError(ctx, err) {
					multiErrMu.Lock()
					multiErr = multierror.Append(multiErr, errors.Wrap(err, "codemod search failed"))
					multiErrMu.Unlock()
				}
				if codemodResults != nil {
					resultsMu.Lock()
					results = append(results, codemodResults...)
					resultsMu.Unlock()
				}
				if codemodCommon != nil {
					commonMu.Lock()
					common.update(*codemodCommon)
					commonMu.Unlock()
				}
			})
		}
	}

	// Wait for required searches.
	requiredWg.Wait()

	// Give optional searches some minimum budget in case required searches return quickly.
	// Cancel all remaining searches after this minimum budget.
	budget := 100 * time.Millisecond
	elapsed := time.Since(start)
	timer := time.AfterFunc(budget-elapsed, cancel)

	// Wait for remaining optional searches to finish or get cancelled.
	optionalWg.Wait()

	timer.Stop()

	tr.LazyPrintf("results=%d limitHit=%v cloning=%d missing=%d timedout=%d", len(results), common.limitHit, len(common.cloning), len(common.missing), len(common.timedout))

	// Alert is a potential alert shown to the user.
	var alert *searchAlert

	if len(missingRepoRevs) > 0 {
		alert = r.alertForMissingRepoRevs(missingRepoRevs)
	}

	if len(results) == 0 && strings.Contains(r.originalQuery, `"`) && r.patternType == "literal" {
		alert, err = r.alertForQuotesInQueryInLiteralMode(ctx)
	}

	// If we have some results, only log the error instead of returning it,
	// because otherwise the client would not receive the partial results
	if len(results) > 0 && multiErr != nil {
		log15.Error("Errors during search", "error", multiErr)
		multiErr = nil
	}

	sortResults(results)

	resultsResolver := searchResultsResolver{
		start:               start,
		searchResultsCommon: common,
		results:             results,
		alert:               alert,
	}

	return &resultsResolver, multiErr.ErrorOrNil()
}

// isContextError returns true if ctx.Err() is not nil or if err
// is an error caused by context cancelation or timeout.
func isContextError(ctx context.Context, err error) bool {
	return ctx.Err() != nil || err == context.Canceled || err == context.DeadlineExceeded
}

// searchResultResolver is a resolver for the GraphQL union type `SearchResult`.
//
// Supported types:
//
//   - *RepositoryResolver         // repo name match
//   - *fileMatchResolver          // text match
//   - *commitSearchResultResolver // diff or commit match
//   - *codemodResultResolver      // code modification
//
// Note: Any new result types added here also need to be handled properly in search_results.go:301 (sparklines)
type searchResultResolver interface {
	ToRepository() (*RepositoryResolver, bool)
	ToFileMatch() (*fileMatchResolver, bool)
	ToCommitSearchResult() (*commitSearchResultResolver, bool)
	ToCodemodResult() (*codemodResultResolver, bool)

	// SearchResultURIs returns the repo name and file uri respectiveley
	searchResultURIs() (string, string)
	resultCount() int32
}

// compareSearchResults checks to see if a is less than b.
// It is implemented separately for easier testing.
func compareSearchResults(a, b searchResultResolver) bool {
	arepo, afile := a.searchResultURIs()
	brepo, bfile := b.searchResultURIs()

	if arepo == brepo {
		return afile < bfile
	}

	return arepo < brepo
}

func sortResults(r []searchResultResolver) {
	sort.Slice(r, func(i, j int) bool { return compareSearchResults(r[i], r[j]) })
}

// regexpPatternMatchingExprsInOrder returns a regexp that matches lines that contain
// non-overlapping matches for each pattern in order.
func regexpPatternMatchingExprsInOrder(patterns []string) string {
	if len(patterns) == 0 {
		return ""
	}
	if len(patterns) == 1 {
		return patterns[0]
	}
	return "(" + strings.Join(patterns, ").*?(") + ")" // "?" makes it prefer shorter matches
}

// Validates usage of the `repohasfile` filter
func validateRepoHasFileUsage(q *query.Query) error {
	// Query only contains "repohasfile:" and "type:symbol"
	if len(q.Fields) == 2 && q.Fields["repohasfile"] != nil && q.Fields["type"] != nil && len(q.Fields["type"]) == 1 && q.Fields["type"][0].Value() == "symbol" {
		return errors.New("repohasfile does not currently return symbol results. Support for symbol results is coming soon. Subscribe to https://github.com/sourcegraph/sourcegraph/issues/4610 for updates")
	}
	return nil
}
