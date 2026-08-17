package cpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"

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

// BridgeCell resolves an account through the directory-owning Worker. The
// endpoint is non-secret and is used only for owner-token introspection; the
// cell provision token never leaves the Worker.
func BridgeCell(bridgeURL, bridgeToken string) CellResolver {
	return func(ctx context.Context, accountID string) (string, string, error) {
		endpoint, err := client.ResolveAccountViaBridge(
			ctx, bridgeURL, bridgeToken, accountID)
		return endpoint, "", err
	}
}

// Apply implements lifecycle.Applier.
func (a cellApplier) Apply(
	ctx context.Context,
	accountID string,
	request lifecycle.ApplyRequest,
) (lifecycle.ApplyAck, error) {
	endpoint, provisionToken, err := a.resolve(ctx, accountID)
	if err != nil {
		return lifecycle.ApplyAck{}, fmt.Errorf("resolve cell for %s: %w", accountID, err)
	}
	ack, err := client.ApplyAccountPlan(
		ctx, endpoint, provisionToken, accountID,
		request.Revision, request.Hash, request.Plan,
		request.Limits, request.Policies, request.Features,
	)
	if err != nil {
		return lifecycle.ApplyAck{}, err
	}
	return lifecycle.ApplyAck{Revision: ack.Revision, Hash: ack.SnapshotHash}, nil
}

func (a cellApplier) ReadApplyFence(
	ctx context.Context,
	accountID string,
) (lifecycle.ApplyFence, error) {
	endpoint, provisionToken, err := a.resolve(ctx, accountID)
	if err != nil {
		return lifecycle.ApplyFence{}, fmt.Errorf("resolve cell for %s: %w", accountID, err)
	}
	fence, err := client.GetAccountPlanFence(
		ctx, endpoint, provisionToken, accountID)
	if err != nil {
		return lifecycle.ApplyFence{}, err
	}
	return lifecycle.ApplyFence{Revision: fence.Revision, Hash: fence.Hash}, nil
}

type bridgeApplier struct {
	url   string
	token string
}

// NewBridgeApplier pushes snapshots through the Worker's authenticated
// directory/apply bridge. The Worker resolves the current cell and adds its
// provision credential, so the Go container never holds fleet-wide cell
// secrets.
func NewBridgeApplier(bridgeURL, bridgeToken string) lifecycle.Applier {
	return bridgeApplier{url: bridgeURL, token: bridgeToken}
}

func (a bridgeApplier) Apply(
	ctx context.Context,
	accountID string,
	request lifecycle.ApplyRequest,
) (lifecycle.ApplyAck, error) {
	ack, err := client.ApplyAccountPlanViaBridge(
		ctx, a.url, a.token, accountID,
		request.Revision, request.Hash, request.Plan,
		request.Limits, request.Policies, request.Features,
	)
	if err != nil {
		return lifecycle.ApplyAck{}, err
	}
	return lifecycle.ApplyAck{Revision: ack.Revision, Hash: ack.SnapshotHash}, nil
}

func (a bridgeApplier) ReadApplyFence(
	ctx context.Context,
	accountID string,
) (lifecycle.ApplyFence, error) {
	fence, err := client.GetAccountPlanFenceViaBridge(
		ctx, a.url, a.token, accountID)
	if err != nil {
		return lifecycle.ApplyFence{}, err
	}
	return lifecycle.ApplyFence{Revision: fence.Revision, Hash: fence.Hash}, nil
}

type bridgeFitChecker struct {
	url   string
	token string
}

// NewBridgeFitChecker returns the production downgrade checker. The Worker
// resolves the active cell, reads only value-free durable usage, and merges
// the control-plane-owned alias/domain allocation authorities before Go makes
// a plan decision.
func NewBridgeFitChecker(bridgeURL, bridgeToken string) lifecycle.FitChecker {
	return bridgeFitChecker{url: bridgeURL, token: bridgeToken}
}

func (checker bridgeFitChecker) Fit(
	ctx context.Context,
	accountID string,
	target lifecycle.PlanSnapshot,
) ([]string, error) {
	report, err := client.CheckAccountPlanFitViaBridge(
		ctx, checker.url, checker.token, accountID,
		client.AccountPlanFitTarget{
			Plan: target.Plan, SnapshotHash: target.Hash,
			Limits: target.Limits, Policies: target.Policies,
			Features: target.Features,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("read account downgrade fit: %w", err)
	}
	violations := make([]string, 0, len(report.Violations))
	for _, violation := range report.Violations {
		dimension := strings.ReplaceAll(violation.Dimension, "_", " ")
		if violation.Code == "authority_incomplete" {
			violations = append(violations, fmt.Sprintf(
				"cannot verify %s because its control-plane authority is unavailable",
				dimension,
			))
			continue
		}
		switch violation.Scope {
		case "account":
			violations = append(violations, fmt.Sprintf(
				"%s usage is %d; target maximum is %d",
				dimension, violation.Used, violation.Max,
			))
		case "realm", "agent":
			violations = append(violations, fmt.Sprintf(
				"%d %s scopes exceed the %s target maximum of %d; highest usage is %d",
				violation.SubjectCount, violation.Scope, dimension,
				violation.Max, violation.Used,
			))
		default:
			return nil, fmt.Errorf("read account downgrade fit: invalid violation scope")
		}
	}
	return violations, nil
}

// CellAccountExists verifies an admin target against cell truth before the
// control plane creates or mutates an override record.
func CellAccountExists(resolve CellResolver) func(context.Context, string) (bool, error) {
	return func(ctx context.Context, accountID string) (bool, error) {
		endpoint, provisionToken, err := resolve(ctx, accountID)
		if err != nil {
			return false, err
		}
		_, err = client.GetAccountPlanSnapshot(ctx, endpoint, provisionToken, accountID)
		if errors.Is(err, client.ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return true, nil
	}
}

// BridgeAccountExists verifies the target against the routed cell without
// reading tenant payload or holding a cell provision token. The Worker's
// apply-plan GET returns only the cell's current revision/hash fence, which is
// sufficient proof that the account exists there; a directory resolution
// alone could accept a stale acct: pointer and create a phantom override.
func BridgeAccountExists(bridgeURL, bridgeToken string) func(context.Context, string) (bool, error) {
	return func(ctx context.Context, accountID string) (bool, error) {
		_, err := client.GetAccountPlanFenceViaBridge(
			ctx, bridgeURL, bridgeToken, accountID)
		if errors.Is(err, client.ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return true, nil
	}
}

// CellAuthenticate is the production AuthFunc: the CP cannot validate
// operator tokens locally (their hashes live in the cell's database), so it
// introspects the bearer against the account's cell — GET /v1/whoami with the
// caller's own token — and authorizes only when the token resolves to the SAME
// account and its authenticated account role carries the requested billing
// permission. Older cells that omit account_role fail closed.
//
// Failure classification: a token the cell rejects as invalid reads as "not
// authorized" (false, nil → HTTP 403); transport failures and cell 5xx
// propagate as errors (→ HTTP 500), so a cell blip does not present to every
// user in the cell as a fleet-wide auth incident.
func CellAuthenticate(resolve CellResolver) AuthFunc {
	return func(
		ctx context.Context,
		accountID, bearer string,
		permission AccountPermission,
	) (AccountAccess, bool, error) {
		endpoint, _, err := resolve(ctx, accountID)
		if err != nil {
			return AccountAccess{}, false, err
		}
		principal, err := client.GetWhoamiOperator(ctx, endpoint, bearer)
		if err != nil {
			if errors.Is(err, client.ErrUnauthorized) {
				return AccountAccess{}, false, nil // invalid operator token
			}
			return AccountAccess{}, false, err // transport / cell 5xx / timeout — bubble up
		}
		if principal.AccountID != accountID ||
			!accountRoleAllows(principal.AccountRole, permission) {
			return AccountAccess{}, false, nil
		}
		return AccountAccess{
			ActorID:    principal.OperatorID,
			Role:       principal.AccountRole,
			Permission: permission,
		}, true, nil
	}
}

func accountRoleAllows(role string, permission AccountPermission) bool {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "account_owner", "account_billing":
		return permission == AccountPermissionBillingRead ||
			permission == AccountPermissionBillingManage
	case "account_admin":
		return permission == AccountPermissionBillingRead
	default:
		// Includes today's least-privilege account_operator plus the planned
		// account_member role and unknown future values.
		return false
	}
}
