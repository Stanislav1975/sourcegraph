package resolvers

import (
	"context"

	"github.com/sourcegraph/sourcegraph/cmd/frontend/graphqlbackend"
	ee "github.com/sourcegraph/sourcegraph/enterprise/pkg/a8n"
	"github.com/sourcegraph/sourcegraph/internal/a8n"
	"github.com/sourcegraph/sourcegraph/internal/api"
)

type codeModResolver struct {
	store   *ee.Store
	codeMod *a8n.CodeMod
}

func (r *codeModResolver) Spec() string { return r.codeMod.CodeModSpec }
func (r *codeModResolver) Arguments() []graphqlbackend.CodeModArgResolver {
	resolvers := make([]graphqlbackend.CodeModArgResolver, 0, len(r.codeMod.Arguments))
	for n, v := range r.codeMod.Arguments {
		resolvers = append(resolvers, codeModArgResolver{name: n, value: v})
	}
	return resolvers
}
func (r *codeModResolver) CreatedAt() graphqlbackend.DateTime {
	return graphqlbackend.DateTime{Time: r.codeMod.CreatedAt}
}
func (r *codeModResolver) UpdatedAt() graphqlbackend.DateTime {
	return graphqlbackend.DateTime{Time: r.codeMod.UpdatedAt}
}

func (r *codeModResolver) Jobs(ctx context.Context) ([]graphqlbackend.CodeModJobResolver, error) {
	opts := ee.ListCodeModJobsOpts{Limit: 50000, CodeModID: r.codeMod.ID}
	jobs, _, err := r.store.ListCodeModJobs(ctx, opts)
	if err != nil {
		return nil, err
	}

	resolvers := make([]graphqlbackend.CodeModJobResolver, len(jobs))
	for i, j := range jobs {
		resolvers[i] = &codeModJobResolver{
			store:      r.store,
			codeMod:    r.codeMod,
			codeModJob: j,
		}
	}

	return resolvers, nil
}

type codeModArgResolver struct{ name, value string }

func (r codeModArgResolver) Name() string  { return r.name }
func (r codeModArgResolver) Value() string { return r.value }

type codeModJobResolver struct {
	store      *ee.Store
	codeMod    *a8n.CodeMod
	codeModJob *a8n.CodeModJob
}

func (r *codeModJobResolver) CodeMod(context.Context) (graphqlbackend.CodeModResolver, error) {
	return &codeModResolver{}, nil
}

func (r *codeModJobResolver) Repo(ctx context.Context) (*graphqlbackend.RepositoryResolver, error) {
	return graphqlbackend.RepositoryByIDInt32(ctx, api.RepoID(r.codeModJob.RepoID))
}

func (r *codeModJobResolver) Revision() graphqlbackend.GitObjectID {
	return graphqlbackend.GitObjectID(string(r.codeModJob.Rev))
}

func (r *codeModJobResolver) Diff() *string {
	if r.codeModJob.Diff != "" {
		return &r.codeModJob.Diff
	}
	return nil
}
func (r *codeModJobResolver) StartedAt() graphqlbackend.DateTime {
	return graphqlbackend.DateTime{Time: r.codeModJob.StartedAt}
}
func (r *codeModJobResolver) FinishedAt() graphqlbackend.DateTime {
	return graphqlbackend.DateTime{Time: r.codeModJob.FinishedAt}
}

func (r *codeModJobResolver) Error() *string {
	if r.codeModJob.Error != "" {
		return &r.codeModJob.Error
	}
	return nil
}
