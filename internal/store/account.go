package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/witwave-ai/witself/internal/placement"
)

// ErrAccountNotFound is returned when an account id does not exist.
var ErrAccountNotFound = errors.New("account not found")

// ErrAccountNotActive is returned when credential minting is attempted on an
// account that is not active. The HTTP layer normally refuses such requests
// at auth time; this store-level check exists for the race where CloseAccount
// commits while a mint is in flight.
var ErrAccountNotActive = errors.New("account is not active")

// lockAccountForMint share-locks the account row and verifies its status
// permits credential minting. The lock serializes minting against
// CloseAccount: a concurrent close's tombstone UPDATE waits for this
// transaction to commit, so its token sweep always sees what was minted here;
// and once a close has committed, this re-read sees 'closed' and refuses.
// Bootstrap exchange passes allowPending — a pending account's owner claims
// their credential to watch for activation; every other mint requires active.
func lockAccountForMint(ctx context.Context, tx pgx.Tx, accountID string, allowPending bool) error {
	var status string
	err := tx.QueryRow(ctx,
		`SELECT status FROM accounts WHERE id = $1 FOR SHARE`, accountID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrAccountNotFound
	}
	if err != nil {
		return fmt.Errorf("lock account: %w", err)
	}
	if status == "active" || (allowPending && status == "pending") {
		return nil
	}
	return ErrAccountNotActive
}

// lockAccountForSafetyWrite is the same lock as lockAccountForMint but also
// permits suspended accounts — used for writes that are HARM-REDUCING
// regardless of the freeze: revoking a possibly-leaked token, closing, and
// other lifecycle terminals, including disabling an agent-email receive
// layer. If suspend means "no state changes I'd regret while frozen," these
// are the state changes an owner NEEDS in order to protect the account, so
// they slip past the gate. Re-enabling receive is not a safety write and
// remains active-account-only.
func lockAccountForSafetyWrite(ctx context.Context, tx pgx.Tx, accountID string) error {
	var status string
	err := tx.QueryRow(ctx,
		`SELECT status FROM accounts WHERE id = $1 FOR SHARE`, accountID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrAccountNotFound
	}
	if err != nil {
		return fmt.Errorf("lock account: %w", err)
	}
	if status == "active" || status == "suspended" {
		return nil
	}
	return ErrAccountNotActive
}

// Account is the stored view of an account's lifecycle record.
type Account struct {
	ID              string
	Email           string
	DisplayName     string
	Status          string
	CreatedAt       time.Time
	ClosedAt        *time.Time
	ClosedReason    string
	SuspendedAt     *time.Time
	SuspendedFor    string
	SuspendedReason string
	SupportPolicy   string
	// Plan snapshot, as applied by the control plane (see migration 0017):
	// the plan label, the resolved account-wide limits (missing key =
	// unlimited), and the included features.
	Plan         string
	PlanLimits   map[string]int64
	PlanFeatures []string

	PlacementPolicy placement.Policy
}

// GetAccount reads one account's lifecycle record. Closed accounts are
// returned too — the record is a tombstone, not an absence.
func (s *Store) GetAccount(ctx context.Context, accountID string) (Account, error) {
	var a Account
	var email, closedReason, suspendedFor, suspendedReason *string
	var planLimits, planFeatures, placementPolicy []byte
	err := s.pool.QueryRow(ctx,
		`SELECT id, email, display_name, status, created_at,
		        closed_at, closed_reason, suspended_at, suspended_for, suspended_reason,
		        support_policy, plan, plan_limits, plan_features, placement_policy
		 FROM accounts WHERE id = $1`, accountID).
		Scan(&a.ID, &email, &a.DisplayName, &a.Status, &a.CreatedAt,
			&a.ClosedAt, &closedReason, &a.SuspendedAt, &suspendedFor, &suspendedReason,
			&a.SupportPolicy, &a.Plan, &planLimits, &planFeatures, &placementPolicy)
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, ErrAccountNotFound
	}
	if err != nil {
		return Account{}, fmt.Errorf("get account: %w", err)
	}
	if err := json.Unmarshal(planLimits, &a.PlanLimits); err != nil {
		return Account{}, fmt.Errorf("decode plan limits: %w", err)
	}
	if err := json.Unmarshal(planFeatures, &a.PlanFeatures); err != nil {
		return Account{}, fmt.Errorf("decode plan features: %w", err)
	}
	a.PlacementPolicy, err = placement.FromJSON(placementPolicy)
	if err != nil {
		return Account{}, fmt.Errorf("decode placement policy: %w", err)
	}
	if email != nil {
		a.Email = *email
	}
	if closedReason != nil {
		a.ClosedReason = *closedReason
	}
	if suspendedFor != nil {
		a.SuspendedFor = *suspendedFor
	}
	if suspendedReason != nil {
		a.SuspendedReason = *suspendedReason
	}
	return a, nil
}
