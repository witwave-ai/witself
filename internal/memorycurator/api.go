// Package memorycurator drives one bounded client-side narrative-memory
// curation run. It performs no inference itself and gives planners no Witself
// credentials.
package memorycurator

import (
	"context"

	"github.com/witwave-ai/witself/internal/client"
)

// API is the provider-neutral curation protocol used by Runner. The HTTPAPI
// adapter below is intentionally thin so tests and future MCP-backed clients
// can provide the same contract without changing orchestration semantics.
type API interface {
	ListRequests(context.Context, client.MemoryCurationRequestListOptions) (*client.MemoryCurationRequestPage, error)
	Start(context.Context, client.StartMemoryCurationInput) (*client.StartMemoryCurationResult, error)
	GetInputs(context.Context, string, int64, string, int) (*client.MemoryCurationRunInputPage, error)
	Renew(context.Context, client.RenewMemoryCurationInput) (*client.RenewMemoryCurationResult, error)
	Plan(context.Context, client.PlanMemoryCurationInput) (*client.PlanMemoryCurationResult, error)
	Apply(context.Context, client.ApplyMemoryCurationInput) (*client.ApplyMemoryCurationResult, error)
	Abandon(context.Context, client.FinishMemoryCurationInput) (*client.FinishMemoryCurationResult, error)
	GetRun(context.Context, string) (*client.MemoryCurationRun, error)
	Status(context.Context, string) (*client.MemoryCurationStatus, error)
}

// HTTPAPI adapts the existing Go HTTP client. Endpoint and Token remain in the
// runner process and are never included in PlannerEnvelope.
type HTTPAPI struct {
	Endpoint string
	Token    string
}

// ListRequests returns curation requests matching the supplied filters.
func (a HTTPAPI) ListRequests(ctx context.Context, opts client.MemoryCurationRequestListOptions) (*client.MemoryCurationRequestPage, error) {
	return client.ListMemoryCurationRequests(ctx, a.Endpoint, a.Token, opts)
}

// Start begins a curation run through the configured HTTP client.
func (a HTTPAPI) Start(ctx context.Context, in client.StartMemoryCurationInput) (*client.StartMemoryCurationResult, error) {
	return client.StartMemoryCuration(ctx, a.Endpoint, a.Token, in)
}

// GetInputs returns one page of frozen inputs for a fenced curation run.
func (a HTTPAPI) GetInputs(ctx context.Context, runID string, fence int64, cursor string, limit int) (*client.MemoryCurationRunInputPage, error) {
	return client.GetMemoryCurationRunInputs(ctx, a.Endpoint, a.Token, runID, fence, cursor, limit)
}

// Renew extends the lease for a fenced curation run.
func (a HTTPAPI) Renew(ctx context.Context, in client.RenewMemoryCurationInput) (*client.RenewMemoryCurationResult, error) {
	return client.RenewMemoryCuration(ctx, a.Endpoint, a.Token, in)
}

// Plan submits a draft plan for a fenced curation run.
func (a HTTPAPI) Plan(ctx context.Context, in client.PlanMemoryCurationInput) (*client.PlanMemoryCurationResult, error) {
	return client.PlanMemoryCuration(ctx, a.Endpoint, a.Token, in)
}

// Apply applies an approved plan for a fenced curation run.
func (a HTTPAPI) Apply(ctx context.Context, in client.ApplyMemoryCurationInput) (*client.ApplyMemoryCurationResult, error) {
	return client.ApplyMemoryCuration(ctx, a.Endpoint, a.Token, in)
}

// Abandon abandons a curation run without applying its plan.
func (a HTTPAPI) Abandon(ctx context.Context, in client.FinishMemoryCurationInput) (*client.FinishMemoryCurationResult, error) {
	return client.AbandonMemoryCuration(ctx, a.Endpoint, a.Token, in)
}

// GetRun returns a curation run by ID.
func (a HTTPAPI) GetRun(ctx context.Context, runID string) (*client.MemoryCurationRun, error) {
	return client.GetMemoryCurationRun(ctx, a.Endpoint, a.Token, runID)
}

// Status returns the current status projection for a curation run.
func (a HTTPAPI) Status(ctx context.Context, runID string) (*client.MemoryCurationStatus, error) {
	return client.GetMemoryCurationStatus(ctx, a.Endpoint, a.Token, runID)
}
