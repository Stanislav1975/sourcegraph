package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/sourcegraph/sourcegraph/cmd/frontend/backend"
	"github.com/sourcegraph/sourcegraph/internal/api"
)

func editorRev(ctx context.Context, repoName api.RepoName, rev string, beExplicit bool) (string, error) {
	if beExplicit {
		return "@" + rev, nil
	}
	if rev == "HEAD" {
		return "", nil // Detached head state
	}
	repo, err := backend.Repos.GetByName(ctx, repoName)
	if err != nil {
		// We weren't able to fetch the repo. This means it either doesn't
		// exist (unlikely) or that the user is not logged in (most likely). In
		// this case, the best user experience is to send them to the branch
		// they asked for. The front-end will inform them if the branch does
		// not exist.
		return "@" + rev, nil
	}
	// If we are on the default branch we want to return a clean URL without a
	// branch. If we fail its best to return the full URL and allow the
	// front-end to inform them of anything that is wrong.
	defaultBranchCommitID, err := backend.Repos.ResolveRev(ctx, repo, "")
	if err != nil {
		return "@" + rev, nil
	}
	branchCommitID, err := backend.Repos.ResolveRev(ctx, repo, rev)
	if err != nil {
		return "@" + rev, nil
	}
	if defaultBranchCommitID == branchCommitID {
		return "", nil // default branch, so make a clean URL without a branch.
	}
	return "@" + rev, nil
}

func serveEditor(w http.ResponseWriter, r *http.Request) error {
	// Required query parameters:
	q := r.URL.Query()
	editor := q.Get("editor")   // Editor name: "Atom", "Sublime", etc.
	version := q.Get("version") // Editor extension version.

	// JetBrains-specific query parameters:
	utmProductName := q.Get("utm_product_name")    // Editor product name, for JetBrains only (e.g. "IntelliJ", "Gogland").
	utmProductVersion := q.Get("utm_product_name") // Editor product version, for JetBrains only.

	// Repo query parameters (required for open-file requests, but optional for
	// search requests):
	remoteURL := q.Get("remote_url") // Git repository remote URL.
	branch := q.Get("branch")        // Git branch name.
	revision := q.Get("revision")    // Git revision.
	file := q.Get("file")            // File relative to repository root.

	// search query parameters. Only present if it is a search request.
	search := q.Get("search")

	// open-file parameters. Only present if it is a open-file request.
	startRow, _ := strconv.Atoi(q.Get("start_row")) // zero-based
	startCol, _ := strconv.Atoi(q.Get("start_col")) // zero-based
	endRow, _ := strconv.Atoi(q.Get("end_row"))     // zero-based
	endCol, _ := strconv.Atoi(q.Get("end_col"))     // zero-based

	if search != "" {
		// Search request. The search is intentionally not scoped to a repository, because it's assumed the
		// user prefers to perform the search in their last-used search scope. Searching in their current
		// repo is not actually very useful, since they can usually do that better in their editor.
		u := &url.URL{Path: "/search"}
		q := u.Query()
		// Escape double quotes in search query.
		search = strings.Replace(search, `"`, `\"`, -1)
		// Search as a string literal
		q.Add("q", `"`+search+`"`)
		q.Add("utm_source", editor+"-"+version)
		if utmProductName != "" {
			q.Add("utm_product_name", utmProductName)
		}
		if utmProductVersion != "" {
			q.Add("utm_product_version", utmProductVersion)
		}
		u.RawQuery = q.Encode()
		http.Redirect(w, r, u.String(), http.StatusSeeOther)
		return nil
	}

	// Open-file request.

	// Determine the repo name and branch.
	//
	// TODO(sqs): This used to hit gitserver, which would be more accurate in case of nonstandard
	// clone URLs.  It now generates the guessed repo name statically, which means in some cases it
	// won't work, but it is worth the increase in simplicity (plus there is an error message for
	// users). In the future we can let users specify a custom mapping to the Sourcegraph repo in
	// their local Git repo (instead of having them pass it here).
	var hostnameToPattern map[string]string
	if hostnameToPatternStr := q.Get("hostname_patterns"); hostnameToPatternStr != "" {
		if err := json.Unmarshal([]byte(hostnameToPatternStr), &hostnameToPattern); err != nil {
			return err
		}
	}
	repoName := guessRepoNameFromRemoteURL(remoteURL, hostnameToPattern)
	if repoName == "" {
		// Any error here is a problem with the user's configured git remote
		// URL. We want them to actually read this error message.
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, "Git remote URL %q not supported", remoteURL)
		return nil
	}

	inputRev, beExplicit := revision, true
	if inputRev == "" {
		inputRev, beExplicit = branch, false
	}
	rev, err := editorRev(r.Context(), repoName, inputRev, beExplicit)
	if err != nil {
		return err
	}

	u := &url.URL{Path: path.Join("/", string(repoName)+rev, "/-/blob/", file)}
	q = u.Query()
	q.Add("utm_source", editor+"-"+version)
	if utmProductName != "" {
		q.Add("utm_product_name", utmProductName)
	}
	if utmProductVersion != "" {
		q.Add("utm_product_version", utmProductVersion)
	}
	u.RawQuery = q.Encode()
	if startRow == endRow && startCol == endCol {
		u.Fragment = fmt.Sprintf("L%d:%d", startRow+1, startCol+1)
	} else {
		u.Fragment = fmt.Sprintf("L%d:%d-%d:%d", startRow+1, startCol+1, endRow+1, endCol+1)
	}
	http.Redirect(w, r, u.String(), http.StatusSeeOther)
	return nil
}

// gitProtocolRegExp is a regular expression that matches any URL that looks like it has a git protocol
var gitProtocolRegExp = regexp.MustCompile("^(git|(git+)?(https?|ssh))://")

// guessRepoNameFromRemoteURL return a guess at the repo name for the given remote URL.
//
// It first normalizes the remote URL (ensuring a scheme exists, stripping any "git@" username in
// the host, stripping any trailing ".git" from the path, etc.). It then returns the repo name as
// templatized by the pattern specified, which references the hostname and path of the normalized
// URL. Patterns are keyed by hostname in the hostnameToPattern parameter. The default pattern is
// "{hostname}/{path}".
//
// For example, given "https://github.com/foo/bar.git" and an empty hostnameToPattern, it returns
// "github.com/foo/bar". Given the same remote URL and hostnametoPattern
// `map[string]string{"github.com": "{path}"}`, it returns "foo/bar".
func guessRepoNameFromRemoteURL(urlStr string, hostnameToPattern map[string]string) api.RepoName {
	if !gitProtocolRegExp.MatchString(urlStr) {
		urlStr = "ssh://" + strings.Replace(strings.TrimPrefix(urlStr, "git@"), ":", "/", 1)
	}
	urlStr = strings.TrimSuffix(urlStr, ".git")
	u, _ := url.Parse(urlStr)
	if u == nil {
		return ""
	}

	pattern := "{hostname}/{path}"
	if hostnameToPattern != nil {
		if p, ok := hostnameToPattern[u.Hostname()]; ok {
			pattern = p
		}
	}

	return api.RepoName(strings.NewReplacer(
		"{hostname}", u.Hostname(),
		"{path}", strings.TrimPrefix(u.Path, "/"),
	).Replace(pattern))
}
