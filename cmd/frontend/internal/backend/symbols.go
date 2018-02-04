package backend

import (
	"context"

	"github.com/sourcegraph/go-langserver/pkg/lsp"
	"github.com/sourcegraph/go-langserver/pkg/lspext"
	"sourcegraph.com/sourcegraph/sourcegraph/pkg/api"
	"sourcegraph.com/sourcegraph/sourcegraph/xlang"
)

// Symbols backend.
var Symbols = &symbols{}

type symbols struct{}

// List resolves a symbol in a repository.
//
// Use the (lspext.WorkspaceSymbolParams).Symbol field to resolve symbols given a global ID. This is how Go symbol
// URLs (e.g., from godoc.org) are resolved.
func (symbols) List(ctx context.Context, repo api.RepoURI, commitID api.CommitID, mode string, params lspext.WorkspaceSymbolParams) ([]lsp.SymbolInformation, error) {
	if Mocks.Symbols.List != nil {
		return Mocks.Symbols.List(ctx, repo, commitID, mode, params)
	}

	var symbols []lsp.SymbolInformation
	rootURI := lsp.DocumentURI("git://" + string(repo) + "?" + string(commitID))
	err := xlang.UnsafeOneShotClientRequest(ctx, mode, rootURI, "workspace/symbol", params, &symbols)
	return symbols, err
}

// MockSymbols is used by tests to mock Symbols backend methods.
type MockSymbols struct {
	List func(ctx context.Context, repo api.RepoURI, commitID api.CommitID, mode string, params lspext.WorkspaceSymbolParams) ([]lsp.SymbolInformation, error)
}
