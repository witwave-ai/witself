package cpserver

import (
	"context"
	"fmt"

	"github.com/witwave-ai/witself/internal/billing/lifecycle"
	"github.com/witwave-ai/witself/internal/client"
)

// CellResolver maps an account to its cell: the API endpoint and the
// provision token the control plane holds for that cell. StaticCell serves
// the single-cell fleet; the directory-backed resolver replaces it when the
// fleet grows.
type CellResolver func(ctx context.Context, accountID string) (endpoint, provisionToken string, err error)

// StaticCell resolves every account to one cell — correct while the fleet is
// a single cell, and the seam the directory slots into later.
func StaticCell(endpoint, provisionToken string) CellResolver {
	return func(context.Context, string) (string, string, error) {
		return endpoint, provisionToken, nil
	}
}

// cellApplier implements lifecycle.Applier by POSTing the snapshot to the
// account's cell (:plan, provision-token authorized). Failures propagate so
// Reconcile keeps retrying — entitled != applied never rests.
type cellApplier struct {
	resolve CellResolver
}

// NewCellApplier returns the production Applier: the CP pushing resolved
// snapshots to cells.
func NewCellApplier(resolve CellResolver) lifecycle.Applier {
	return cellApplier{resolve: resolve}
}

// Apply implements lifecycle.Applier.
func (a cellApplier) Apply(ctx context.Context, accountID, plan string, limits map[string]int64, features []string) error {
	endpoint, provisionToken, err := a.resolve(ctx, accountID)
	if err != nil {
		return fmt.Errorf("resolve cell for %s: %w", accountID, err)
	}
	return client.ApplyAccountPlan(ctx, endpoint, provisionToken, accountID, plan, limits, features)
}

// CellAuthenticate is the production AuthFunc: the CP cannot validate
// operator tokens locally (their hashes live in the cell's database), so it
// introspects the bearer against the account's cell — GET /v1/whoami with the
// caller's own token — and authorizes only when the token resolves to the
// SAME account the request targets.
func CellAuthenticate(resolve CellResolver) AuthFunc {
	return func(ctx context.Context, accountID, bearer string) (bool, error) {
		endpoint, _, err := resolve(ctx, accountID)
		if err != nil {
			return false, err
		}
		_, tokenAccount, err := client.Whoami(ctx, endpoint, bearer)
		if err != nil {
			// An invalid token reads as "not authorized", not a server fault:
			// whoami's 401 arrives here as an error either way, and refusing
			// is the safe shape for both.
			return false, nil
		}
		return tokenAccount == accountID, nil
	}
}
