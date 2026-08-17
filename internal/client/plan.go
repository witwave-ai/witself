package client

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	neturl "net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/plans"
)

var bridgeAccountIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var planSnapshotHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// AccountPlanSnapshot is the exact snapshot acknowledgement returned by a
// cell after its monotonic revision fence accepts an apply.
type AccountPlanSnapshot struct {
	AccountID    string           `json:"account_id"`
	Revision     int64            `json:"revision"`
	SnapshotHash string           `json:"snapshot_hash"`
	Plan         string           `json:"plan"`
	Limits       map[string]int64 `json:"limits"`
	Policies     map[string]int64 `json:"policies"`
	Features     []string         `json:"features"`
	AppliedAt    *time.Time       `json:"applied_at"`
}

// AccountPlanFence is the value-minimal acknowledgement used to recover when
// control-plane lifecycle storage is restored behind a cell.
type AccountPlanFence struct {
	Revision int64
	Hash     string
}

// AccountPlanFitTarget is a complete resolved target snapshot. Nil maps or a
// nil feature slice are rejected instead of being normalized to accidental
// unlimited behavior at this downgrade-safety boundary.
type AccountPlanFitTarget struct {
	Plan         string           `json:"plan"`
	SnapshotHash string           `json:"snapshot_hash"`
	Limits       map[string]int64 `json:"limits"`
	Policies     map[string]int64 `json:"policies"`
	Features     []string         `json:"features"`
}

// AccountPlanFitViolation is one value-free aggregate target-cap refusal.
type AccountPlanFitViolation struct {
	Code         string `json:"code"`
	Dimension    string `json:"dimension"`
	Scope        string `json:"scope"`
	Used         int64  `json:"used"`
	Max          int64  `json:"max"`
	SubjectCount int64  `json:"subject_count"`
}

// AccountPlanFitReport is returned by the cell directly or through the
// directory bridge. It contains counts only, never tenant resource ids.
type AccountPlanFitReport struct {
	SchemaVersion      string                    `json:"schema_version"`
	AccountID          string                    `json:"account_id"`
	TargetPlan         string                    `json:"target_plan"`
	TargetSnapshotHash string                    `json:"target_snapshot_hash"`
	Violations         []AccountPlanFitViolation `json:"violations"`
}

const (
	AccountPlanFitApplyStateApplied = "applied"
	AccountPlanFitApplyStateBlocked = "blocked"
)

// AccountPlanFitApplyTarget is a complete resolved target plus the positive
// control-plane snapshot revision the cell must atomically acknowledge.
type AccountPlanFitApplyTarget struct {
	Revision     int64            `json:"revision"`
	Plan         string           `json:"plan"`
	SnapshotHash string           `json:"snapshot_hash"`
	Limits       map[string]int64 `json:"limits"`
	Policies     map[string]int64 `json:"policies"`
	Features     []string         `json:"features"`
}

// AccountPlanFitApplyResult is a strict mutually exclusive response. Applied
// carries only AppliedSnapshot and an empty violation list. Blocked carries at
// least one violation and the exact CurrentSnapshot read under the fit lock.
type AccountPlanFitApplyResult struct {
	SchemaVersion      string                    `json:"schema_version"`
	State              string                    `json:"state"`
	AccountID          string                    `json:"account_id"`
	TargetRevision     int64                     `json:"target_revision"`
	TargetPlan         string                    `json:"target_plan"`
	TargetSnapshotHash string                    `json:"target_snapshot_hash"`
	Violations         []AccountPlanFitViolation `json:"violations"`
	CurrentSnapshot    *AccountPlanSnapshot      `json:"current_snapshot,omitempty"`
	AppliedSnapshot    *AccountPlanSnapshot      `json:"applied_snapshot,omitempty"`
}

// ApplyAccountPlan pushes a plan snapshot to a cell via the provision-token
// system endpoint POST {endpoint}/v1/accounts/{id}:plan — the control plane's
// half of "the CP computes, the cell enforces". provisionToken is the CP↔cell
// trust link (witself_prv_*), never an operator token.
func ApplyAccountPlan(
	ctx context.Context,
	endpoint, provisionToken, accountID string,
	revision int64,
	snapshotHash, plan string,
	limits, policies map[string]int64,
	features []string,
) (AccountPlanSnapshot, error) {
	url := strings.TrimRight(endpoint, "/") + "/v1/accounts/" + accountID + ":plan"
	return applyAccountPlanAtURL(
		ctx, url, provisionToken, accountID, revision, snapshotHash, plan,
		limits, policies, features,
	)
}

// ApplyAccountPlanViaBridge asks the directory-owning Worker to resolve the
// account and forward the fenced snapshot to its current cell.
func ApplyAccountPlanViaBridge(
	ctx context.Context,
	bridgeURL, bridgeToken, accountID string,
	revision int64,
	snapshotHash, plan string,
	limits, policies map[string]int64,
	features []string,
) (AccountPlanSnapshot, error) {
	url := strings.TrimRight(bridgeURL, "/") +
		"/v1/internal/accounts/" + accountID + ":apply-plan"
	return applyAccountPlanAtURL(
		ctx, url, bridgeToken, accountID, revision, snapshotHash, plan,
		limits, policies, features,
	)
}

// ApplyAccountPlanIfFits asks a cell to lock the account, recheck every finite
// durable capacity, and apply the exact fenced target in one transaction.
func ApplyAccountPlanIfFits(
	ctx context.Context,
	endpoint, provisionToken, accountID string,
	target AccountPlanFitApplyTarget,
) (AccountPlanFitApplyResult, error) {
	url := strings.TrimRight(endpoint, "/") +
		"/v1/accounts/" + accountID + ":plan-fit-apply"
	return applyAccountPlanIfFitsAtURL(
		ctx, url, provisionToken, accountID, target,
	)
}

// ApplyAccountPlanIfFitsViaBridge sends the same strict request through the
// directory-owning Worker, which resolves the current cell and holds the
// provision credential.
func ApplyAccountPlanIfFitsViaBridge(
	ctx context.Context,
	bridgeURL, bridgeToken, accountID string,
	target AccountPlanFitApplyTarget,
) (AccountPlanFitApplyResult, error) {
	url := strings.TrimRight(bridgeURL, "/") +
		"/v1/internal/accounts/" + accountID + ":plan-fit-apply"
	return applyAccountPlanIfFitsAtURL(
		ctx, url, bridgeToken, accountID, target,
	)
}

func applyAccountPlanIfFitsAtURL(
	ctx context.Context,
	url, bearer, accountID string,
	target AccountPlanFitApplyTarget,
) (AccountPlanFitApplyResult, error) {
	if err := validateAccountPlanFitApplyTarget(target); err != nil {
		return AccountPlanFitApplyResult{}, err
	}
	body, err := json.Marshal(struct {
		SchemaVersion string                    `json:"schema_version"`
		Target        AccountPlanFitApplyTarget `json:"target"`
	}{SchemaVersion: "witself.v0", Target: target})
	if err != nil {
		return AccountPlanFitApplyResult{}, fmt.Errorf(
			"encode plan-fit apply target: %w", err,
		)
	}
	var result AccountPlanFitApplyResult
	if err := doJSON(ctx, http.MethodPost, url, bearer, body, &result); err != nil {
		return AccountPlanFitApplyResult{}, err
	}
	if err := validateAccountPlanFitApplyResult(accountID, target, result); err != nil {
		return AccountPlanFitApplyResult{}, err
	}
	return result, nil
}

func applyAccountPlanAtURL(
	ctx context.Context,
	url, bearer, accountID string,
	revision int64,
	snapshotHash, plan string,
	limits, policies map[string]int64,
	features []string,
) (AccountPlanSnapshot, error) {
	body, err := json.Marshal(map[string]any{
		"revision":      revision,
		"snapshot_hash": snapshotHash,
		"plan":          plan,
		"limits":        limits,
		"policies":      policies,
		"features":      features,
	})
	if err != nil {
		return AccountPlanSnapshot{}, fmt.Errorf("encode plan snapshot: %w", err)
	}
	var ack AccountPlanSnapshot
	if err := doJSON(ctx, http.MethodPost, url, bearer, body, &ack); err != nil {
		return AccountPlanSnapshot{}, err
	}
	if ack.AccountID != accountID || ack.Revision != revision ||
		ack.SnapshotHash != snapshotHash || ack.Plan != plan ||
		!maps.Equal(ack.Limits, limits) || !maps.Equal(ack.Policies, policies) ||
		!slices.Equal(ack.Features, features) {
		return AccountPlanSnapshot{}, fmt.Errorf("cell returned a mismatched plan snapshot acknowledgement")
	}
	return ack, nil
}

// CheckAccountPlanFit asks one cell to compare its current durable usage with
// a complete resolved target snapshot. The RPC is POST because the target is
// structured, but it is a read-only operation.
func CheckAccountPlanFit(
	ctx context.Context,
	endpoint, provisionToken, accountID string,
	target AccountPlanFitTarget,
) (AccountPlanFitReport, error) {
	url := strings.TrimRight(endpoint, "/") +
		"/v1/accounts/" + accountID + ":plan-fit"
	return checkAccountPlanFitAtURL(ctx, url, provisionToken, accountID, target)
}

// CheckAccountPlanFitViaBridge lets the directory-owning Worker resolve the
// account's current cell and merge cell usage with control-plane-owned alias
// and custom-domain allocation authorities.
func CheckAccountPlanFitViaBridge(
	ctx context.Context,
	bridgeURL, bridgeToken, accountID string,
	target AccountPlanFitTarget,
) (AccountPlanFitReport, error) {
	url := strings.TrimRight(bridgeURL, "/") +
		"/v1/internal/accounts/" + accountID + ":plan-fit"
	return checkAccountPlanFitAtURL(ctx, url, bridgeToken, accountID, target)
}

func checkAccountPlanFitAtURL(
	ctx context.Context,
	url, bearer, accountID string,
	target AccountPlanFitTarget,
) (AccountPlanFitReport, error) {
	if err := validateAccountPlanFitTarget(target); err != nil {
		return AccountPlanFitReport{}, err
	}
	body, err := json.Marshal(struct {
		SchemaVersion string               `json:"schema_version"`
		Target        AccountPlanFitTarget `json:"target"`
	}{SchemaVersion: "witself.v0", Target: target})
	if err != nil {
		return AccountPlanFitReport{}, fmt.Errorf("encode plan-fit target: %w", err)
	}
	var report AccountPlanFitReport
	if err := doJSON(ctx, http.MethodPost, url, bearer, body, &report); err != nil {
		return AccountPlanFitReport{}, err
	}
	if err := validateAccountPlanFitReport(accountID, target, report); err != nil {
		return AccountPlanFitReport{}, err
	}
	return report, nil
}

func validateAccountPlanFitTarget(target AccountPlanFitTarget) error {
	if target.Plan == "" || target.Plan != strings.TrimSpace(target.Plan) ||
		target.SnapshotHash == "" || target.Limits == nil ||
		target.Policies == nil || target.Features == nil {
		return fmt.Errorf("complete resolved plan-fit target snapshot is required")
	}
	if err := plans.ValidateLimits(target.Limits); err != nil {
		return fmt.Errorf("invalid plan-fit target limits: %w", err)
	}
	if err := plans.ValidatePolicies(target.Policies); err != nil {
		return fmt.Errorf("invalid plan-fit target policies: %w", err)
	}
	if err := plans.ValidateFeatures(target.Features); err != nil {
		return fmt.Errorf("invalid plan-fit target features: %w", err)
	}
	expected, err := plans.SnapshotHash(
		target.Plan, target.Limits, target.Policies, target.Features,
	)
	if err != nil || expected != target.SnapshotHash {
		return fmt.Errorf("plan-fit target snapshot hash does not match payload")
	}
	return nil
}

func validateAccountPlanFitApplyTarget(target AccountPlanFitApplyTarget) error {
	if target.Revision < 1 {
		return fmt.Errorf("positive plan-fit apply target revision is required")
	}
	return validateAccountPlanFitTarget(AccountPlanFitTarget{
		Plan: target.Plan, SnapshotHash: target.SnapshotHash,
		Limits: target.Limits, Policies: target.Policies, Features: target.Features,
	})
}

func validateAccountPlanFitApplyResult(
	accountID string,
	target AccountPlanFitApplyTarget,
	result AccountPlanFitApplyResult,
) error {
	if result.SchemaVersion != "witself.v0" || result.AccountID != accountID ||
		result.TargetRevision != target.Revision || result.TargetPlan != target.Plan ||
		result.TargetSnapshotHash != target.SnapshotHash || result.Violations == nil {
		return fmt.Errorf("cell returned an invalid plan-fit apply envelope")
	}
	fitTarget := AccountPlanFitTarget{
		Plan: target.Plan, SnapshotHash: target.SnapshotHash,
		Limits: target.Limits, Policies: target.Policies, Features: target.Features,
	}
	switch result.State {
	case AccountPlanFitApplyStateApplied:
		if len(result.Violations) != 0 || result.CurrentSnapshot != nil ||
			result.AppliedSnapshot == nil ||
			!accountPlanSnapshotMatchesApplyTarget(
				accountID, target, *result.AppliedSnapshot,
			) {
			return fmt.Errorf("cell returned an invalid applied plan-fit result")
		}
	case AccountPlanFitApplyStateBlocked:
		if len(result.Violations) == 0 || result.CurrentSnapshot == nil ||
			result.AppliedSnapshot != nil ||
			result.CurrentSnapshot.Revision >= target.Revision ||
			validateCurrentAccountPlanSnapshot(
				accountID, *result.CurrentSnapshot,
			) != nil {
			return fmt.Errorf("cell returned an invalid blocked plan-fit result")
		}
		if err := validateAccountPlanFitReport(accountID, fitTarget, AccountPlanFitReport{
			SchemaVersion: "witself.v0", AccountID: accountID,
			TargetPlan: target.Plan, TargetSnapshotHash: target.SnapshotHash,
			Violations: result.Violations,
		}); err != nil {
			return fmt.Errorf("cell returned an invalid blocked plan-fit result: %w", err)
		}
		for _, violation := range result.Violations {
			if violation.Code != "limit_exceeded" {
				return fmt.Errorf("cell returned a non-cell plan-fit apply violation")
			}
		}
	default:
		return fmt.Errorf("cell returned an unknown plan-fit apply state")
	}
	return nil
}

func accountPlanSnapshotMatchesApplyTarget(
	accountID string,
	target AccountPlanFitApplyTarget,
	snapshot AccountPlanSnapshot,
) bool {
	wantFeatures := append([]string{}, target.Features...)
	slices.Sort(wantFeatures)
	return snapshot.AccountID == accountID && snapshot.Revision == target.Revision &&
		snapshot.SnapshotHash == target.SnapshotHash && snapshot.Plan == target.Plan &&
		maps.Equal(snapshot.Limits, target.Limits) &&
		maps.Equal(snapshot.Policies, target.Policies) &&
		slices.Equal(snapshot.Features, wantFeatures) && snapshot.AppliedAt != nil
}

func validateCurrentAccountPlanSnapshot(
	accountID string,
	snapshot AccountPlanSnapshot,
) error {
	if snapshot.AccountID != accountID || snapshot.Revision < 0 ||
		snapshot.Plan == "" || snapshot.Plan != strings.TrimSpace(snapshot.Plan) ||
		snapshot.Limits == nil ||
		snapshot.Policies == nil || snapshot.Features == nil {
		return fmt.Errorf("invalid current plan snapshot")
	}
	if err := plans.ValidateLimits(snapshot.Limits); err != nil {
		return fmt.Errorf("invalid current plan limits: %w", err)
	}
	if err := plans.ValidatePolicies(snapshot.Policies); err != nil {
		return fmt.Errorf("invalid current plan policies: %w", err)
	}
	if err := plans.ValidateFeatures(snapshot.Features); err != nil {
		return fmt.Errorf("invalid current plan features: %w", err)
	}
	if snapshot.Revision == 0 {
		if snapshot.SnapshotHash != "" {
			return fmt.Errorf("invalid unfenced current plan snapshot")
		}
		return nil
	}
	if snapshot.AppliedAt == nil {
		return fmt.Errorf("fenced current plan snapshot has no applied time")
	}
	expected, err := plans.SnapshotHash(
		snapshot.Plan, snapshot.Limits, snapshot.Policies, snapshot.Features,
	)
	if err != nil || expected != snapshot.SnapshotHash {
		return fmt.Errorf("current plan snapshot hash does not match payload")
	}
	return nil
}

func validateAccountPlanFitReport(
	accountID string,
	target AccountPlanFitTarget,
	report AccountPlanFitReport,
) error {
	if report.SchemaVersion != "witself.v0" || report.AccountID != accountID ||
		report.TargetPlan != target.Plan ||
		report.TargetSnapshotHash != target.SnapshotHash ||
		report.Violations == nil {
		return fmt.Errorf("cell returned an invalid plan-fit report envelope")
	}
	seen := make(map[string]bool, len(report.Violations))
	for _, violation := range report.Violations {
		maximum, finite := target.Limits[violation.Dimension]
		if !finite || maximum != violation.Max || seen[violation.Dimension] {
			return fmt.Errorf("cell returned an invalid plan-fit violation")
		}
		seen[violation.Dimension] = true
		expectedScope, known := planFitDimensionScope(violation.Dimension)
		if !known || violation.SubjectCount < 1 {
			return fmt.Errorf("cell returned an invalid plan-fit violation")
		}
		switch violation.Code {
		case "limit_exceeded":
			if violation.Scope != expectedScope || violation.Used <= violation.Max ||
				(expectedScope == "account" && violation.SubjectCount != 1) {
				return fmt.Errorf("cell returned an invalid plan-fit violation")
			}
		case "authority_incomplete":
			if violation.Scope != "authority" || violation.Used != 0 ||
				(violation.Dimension != plans.AgentEmailRealmAliasesPerRealmLimit &&
					violation.Dimension != plans.AgentEmailCustomDomainsPerAccountLimit) {
				return fmt.Errorf("cell returned an invalid plan-fit authority violation")
			}
		default:
			return fmt.Errorf("cell returned an unknown plan-fit violation code")
		}
	}
	return nil
}

func planFitDimensionScope(dimension string) (string, bool) {
	switch dimension {
	case plans.RealmLimit, plans.AgentLimit,
		plans.AgentEmailAttachmentStorageBytesLimit,
		plans.AgentEmailCustomDomainsPerAccountLimit:
		return "account", true
	case plans.AgentPerRealmLimit, plans.AgentEmailRealmAliasesPerRealmLimit:
		return "realm", true
	case plans.StoredMemoryLimit, plans.StoredFactLimit, plans.StoredSecretLimit:
		return "agent", true
	default:
		return "", false
	}
}

// ResolveAccountViaBridge returns the current cell endpoint for an account.
// The bridge owns directory truth; a 404 remains client.ErrNotFound.
func ResolveAccountViaBridge(
	ctx context.Context,
	bridgeURL, bridgeToken, accountID string,
) (string, error) {
	var out struct {
		SchemaVersion string `json:"schema_version"`
		AccountID     string `json:"account_id"`
		State         string `json:"state"`
		Cell          string `json:"cell"`
		Endpoint      string `json:"endpoint"`
	}
	url := strings.TrimRight(bridgeURL, "/") +
		"/v1/internal/accounts/" + accountID + ":resolve"
	if err := doJSON(ctx, http.MethodGet, url, bridgeToken, nil, &out); err != nil {
		return "", err
	}
	if out.SchemaVersion != "witself.v0" || out.AccountID != accountID ||
		out.State != "active" || out.Cell == "" || out.Endpoint == "" {
		return "", fmt.Errorf("bridge returned an invalid account resolution")
	}
	parsed, err := neturl.Parse(out.Endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("bridge returned an invalid cell endpoint")
	}
	return strings.TrimRight(out.Endpoint, "/"), nil
}

// ActiveAccountPage is one bounded page from the Worker's authoritative
// active-account directory. The cursor is opaque and must be passed back
// unchanged.
type ActiveAccountPage struct {
	AccountIDs []string
	NextCursor string
}

// ListActiveAccountsViaBridge returns active (not pending or archived)
// directory account ids. It validates every value before the control plane
// can use it as a lifecycle storage key or URL segment.
func ListActiveAccountsViaBridge(
	ctx context.Context,
	bridgeURL, bridgeToken, cursor string,
	limit int,
) (ActiveAccountPage, error) {
	if limit < 1 || limit > 500 {
		return ActiveAccountPage{}, fmt.Errorf("account page limit must be 1-500")
	}
	u, err := neturl.Parse(strings.TrimRight(bridgeURL, "/") + "/v1/internal/accounts")
	if err != nil {
		return ActiveAccountPage{}, fmt.Errorf("parse bridge URL: %w", err)
	}
	query := u.Query()
	query.Set("limit", strconv.Itoa(limit))
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	u.RawQuery = query.Encode()
	var out struct {
		SchemaVersion string   `json:"schema_version"`
		AccountIDs    []string `json:"account_ids"`
		NextCursor    *string  `json:"next_cursor"`
	}
	if err := doJSON(ctx, http.MethodGet, u.String(), bridgeToken, nil, &out); err != nil {
		return ActiveAccountPage{}, err
	}
	if out.SchemaVersion != "witself.v0" || out.AccountIDs == nil {
		return ActiveAccountPage{}, fmt.Errorf("bridge returned an invalid account page")
	}
	seen := make(map[string]struct{}, len(out.AccountIDs))
	for _, accountID := range out.AccountIDs {
		if !bridgeAccountIDPattern.MatchString(accountID) {
			return ActiveAccountPage{}, fmt.Errorf("bridge returned an invalid account id")
		}
		if _, duplicate := seen[accountID]; duplicate {
			return ActiveAccountPage{}, fmt.Errorf("bridge returned a duplicate account id")
		}
		seen[accountID] = struct{}{}
	}
	next := ""
	if out.NextCursor != nil {
		next = *out.NextCursor
		if next == "" || len(next) > 2048 || next == cursor {
			return ActiveAccountPage{}, fmt.Errorf("bridge returned an invalid account cursor")
		}
	}
	return ActiveAccountPage{
		AccountIDs: append([]string(nil), out.AccountIDs...),
		NextCursor: next,
	}, nil
}

// GetAccountPlanSnapshot verifies that accountID exists on a cell and returns
// the exact snapshot fence currently persisted there.
func GetAccountPlanSnapshot(
	ctx context.Context,
	endpoint, provisionToken, accountID string,
) (AccountPlanSnapshot, error) {
	var snapshot AccountPlanSnapshot
	url := strings.TrimRight(endpoint, "/") + "/v1/accounts/" + accountID + ":plan"
	if err := doJSON(ctx, http.MethodGet, url, provisionToken, nil, &snapshot); err != nil {
		return AccountPlanSnapshot{}, err
	}
	if snapshot.AccountID != accountID {
		return AccountPlanSnapshot{}, fmt.Errorf("cell returned a plan snapshot for the wrong account")
	}
	return snapshot, nil
}

// GetAccountPlanFence reads only a cell's revision/hash acknowledgement. The
// returned plan payload is deliberately discarded: cells enforce snapshots
// but never become entitlement authority.
func GetAccountPlanFence(
	ctx context.Context,
	endpoint, provisionToken, accountID string,
) (AccountPlanFence, error) {
	var snapshot AccountPlanSnapshot
	url := strings.TrimRight(endpoint, "/") + "/v1/accounts/" + accountID + ":plan"
	if err := doJSON(ctx, http.MethodGet, url, provisionToken, nil, &snapshot); err != nil {
		return AccountPlanFence{}, err
	}
	return validateAccountPlanFence(accountID, snapshot)
}

// GetAccountPlanFenceViaBridge asks the Worker to resolve the account and read
// the current cell fence with the Worker-held provision credential.
func GetAccountPlanFenceViaBridge(
	ctx context.Context,
	bridgeURL, bridgeToken, accountID string,
) (AccountPlanFence, error) {
	var snapshot AccountPlanSnapshot
	url := strings.TrimRight(bridgeURL, "/") +
		"/v1/internal/accounts/" + accountID + ":apply-plan"
	if err := doJSON(ctx, http.MethodGet, url, bridgeToken, nil, &snapshot); err != nil {
		return AccountPlanFence{}, err
	}
	return validateAccountPlanFence(accountID, snapshot)
}

func validateAccountPlanFence(
	accountID string,
	snapshot AccountPlanSnapshot,
) (AccountPlanFence, error) {
	if snapshot.AccountID != accountID || snapshot.Revision < 0 {
		return AccountPlanFence{}, fmt.Errorf("cell returned an invalid plan snapshot fence")
	}
	if snapshot.Revision == 0 {
		if snapshot.SnapshotHash != "" {
			return AccountPlanFence{}, fmt.Errorf("cell returned an invalid plan snapshot fence")
		}
	} else if !planSnapshotHashPattern.MatchString(snapshot.SnapshotHash) {
		return AccountPlanFence{}, fmt.Errorf("cell returned an invalid plan snapshot fence")
	}
	return AccountPlanFence{
		Revision: snapshot.Revision,
		Hash:     snapshot.SnapshotHash,
	}, nil
}
