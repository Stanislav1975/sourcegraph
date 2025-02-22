package search

import (
	"errors"
	"strings"
	"sync"

	"github.com/google/zoekt/rpc"
	"github.com/sourcegraph/sourcegraph/internal/conf"
	"github.com/sourcegraph/sourcegraph/internal/endpoint"
	"github.com/sourcegraph/sourcegraph/internal/env"
	"github.com/sourcegraph/sourcegraph/internal/search/backend"
)

var (
	zoektAddr   = env.Get("ZOEKT_HOST", "indexed-search:80", "host:port of the zoekt instance")
	searcherURL = env.Get("SEARCHER_URL", "k8s+http://searcher:3181", "searcher server URL")

	searcherURLsOnce sync.Once
	searcherURLs     *endpoint.Map

	indexedSearchOnce sync.Once
	indexedSearch     *backend.Zoekt
)

func SearcherURLs() *endpoint.Map {
	searcherURLsOnce.Do(func() {
		if len(strings.Fields(searcherURL)) == 0 {
			searcherURLs = endpoint.Empty(errors.New("a searcher service has not been configured"))
		} else {
			searcherURLs = endpoint.New(searcherURL)
		}
	})
	return searcherURLs
}

func Indexed() *backend.Zoekt {
	indexedSearchOnce.Do(func() {
		indexedSearch = &backend.Zoekt{}
		if zoektAddr != "" {
			indexedSearch.Client = rpc.Client(zoektAddr)
		}
		conf.Watch(func() {
			indexedSearch.SetEnabled(conf.SearchIndexEnabled())
		})
	})
	return indexedSearch
}
