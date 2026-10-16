package lifecycle

import (
	"context"
	"errors"
	"strings"

	"github.com/witwave-ai/witself/internal/billing"
	"github.com/witwave-ai/witself/internal/plans"
)

// ErrAccountBillingClosed means closure has irreversibly disabled purchases.
// Provider cleanup can still be pending: only CloseAccountBilling may retry it.
var ErrAccountBillingClosed = errors.New("account billing closure has started")

// The account lane recognizes this exact identity as a permanent reservation,
// including the crash gap before the lifecycle Record fence is persisted.
func accountClosureOperationID(accountID string) string {
	return derivedBillingOperationID(accountID, "account_closure")
}

func (m *Manager) requireOpenBillingAccount(ctx context.Context, accountID string) error {
	r, err := m.load(ctx, accountID, "")
	if err != nil {
		return err
	}
	if r.BillingClosureOperationID != "" {
		return ErrAccountBillingClosed
	}
	if store, ok := m.cfg.Store.(BillingMutationStore); ok {
		lane, found, err := store.GetBillingMutationAccount(ctx, accountID)
		if err != nil {
			return err
		}
		if found && lane.OperationID == accountClosureOperationID(accountID) {
			return ErrAccountBillingClosed
		}
	}
	return nil
}

// claimOpenBillingAccount puts legacy in-process mutations and portal creation
// in the same lane as durable HTTP mutations. Stores without the durable lane
// cannot support account closure, but retain their historical legacy APIs.
func (m *Manager) claimOpenBillingAccount(
	ctx context.Context, accountID, operationID string,
) (func(), error) {
	if err := m.requireOpenBillingAccount(ctx, accountID); err != nil {
		return nil, err
	}
	store, ok := m.cfg.Store.(BillingMutationStore)
	if !ok {
		return func() {}, nil
	}
	token, err := newBillingMutationClaimToken()
	if err != nil {
		return nil, err
	}
	now := m.cfg.Now().UTC()
	lease, acquired, err := store.ClaimBillingMutationAccount(
		ctx, accountID, operationID, 0, token, now,
		now.Add(billingMutationClaimLease))
	if err != nil {
		return nil, err
	}
	if !acquired {
		return nil, ErrBillingMutationInProgress
	}
	release := func() {
		_ = store.ReleaseBillingMutationAccount(ctx, lease, m.cfg.Now().UTC())
	}
	if err := m.requireOpenBillingAccount(ctx, accountID); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

// CloseAccountBilling irreversibly stops new purchases and confirms provider
// cancellation before the cell can revoke account access. The account billing
// lane prevents it from racing a subscription/customer mutation. An unresolved
// durable mutation must settle first; closure never guesses whether an
// ambiguous purchase succeeded and never supersedes its recovery receipt.
//
// The closure fence remains after every failure. Retrying the same account
// resumes the same provider operation; completion is durable before success.
// This method deliberately never applies a plan to the cell.
func (m *Manager) CloseAccountBilling(ctx context.Context, accountID string) error {
	ctx, cancel := context.WithTimeout(ctx, billingMutationExecutionTimeout)
	defer cancel()
	accountID = strings.TrimSpace(accountID)
	if !validBillingMutationText(accountID, 255) {
		return billingMutationInput("invalid account id")
	}
	store, ok := m.cfg.Store.(BillingMutationStore)
	if !ok {
		return errors.New("lifecycle: durable billing mutation store is required for account closure")
	}
	r, err := m.load(ctx, accountID, "")
	if err != nil {
		return err
	}
	if !r.BillingClosedAt.IsZero() && r.BillingClosureOperationID != "" {
		return nil
	}
	operationID := accountClosureOperationID(accountID)
	token, err := newBillingMutationClaimToken()
	if err != nil {
		return err
	}
	now := m.cfg.Now().UTC()
	lease, acquired, err := store.ClaimBillingMutationAccount(
		ctx, accountID, operationID, 0, token, now,
		now.Add(billingMutationClaimLease))
	if err != nil {
		return err
	}
	if !acquired {
		return ErrBillingMutationInProgress
	}
	defer func() {
		_ = store.ReleaseBillingMutationAccount(ctx, lease, m.cfg.Now().UTC())
	}()
	r, err = m.mutate(ctx, accountID, "", func(r *Record) error {
		if r.BillingClosureOperationID != "" {
			if r.BillingClosureOperationID != operationID {
				return ErrBillingMutationConflict
			}
			return errSkipWrite
		}
		// Legacy callers can leave an ambiguous purchase without a durable
		// receipt. Do not race a subscription that may still be created.
		if p := r.Pending; p != nil && p.Kind == PendingUpgrade &&
			!p.CancelPrevious && p.ProviderObjectID == "" && p.URL == "" {
			return ErrBillingMutationInProgress
		}
		// Without a customer, existing paid/provider state is ambiguous. Do
		// not erase it or claim that it has been cancelled.
		if r.CustomerID == "" && (r.ManagedSubscriptionID != "" ||
			(r.Entitled != "" && r.Entitled != plans.Free) ||
			(r.Pending != nil && (r.Pending.Kind != PendingContact || r.Pending.CancelPrevious || r.Pending.CancelPreviousTarget != nil))) {
			return refuse("billing state requires reconciliation before account closure")
		}
		if r.CustomerID != "" {
			_, provider, err := m.providerFor(*r)
			if err != nil {
				return err
			}
			if _, ok := provider.(billing.AccountClosureCanceller); !ok {
				return refuse("billing provider does not support automatic account closure")
			}
		}
		r.BillingClosureOperationID = operationID
		r.BillingClosureStartedAt = now
		return nil
	})
	if err != nil || !r.BillingClosedAt.IsZero() {
		return err
	}
	if r.CustomerID != "" {
		_, provider, err := m.providerFor(r)
		if err != nil {
			return err
		}
		canceller, ok := provider.(billing.AccountClosureCanceller)
		if !ok {
			return refuse("billing provider does not support automatic account closure")
		}
		if err := canceller.CancelForAccountClosure(
			ctx, r.CustomerID, accountID, operationID); err != nil {
			return wrapProviderRequest(err)
		}
	}
	_, err = m.mutate(ctx, accountID, "", func(current *Record) error {
		if current.BillingClosureOperationID != operationID ||
			current.Provider != r.Provider || current.CustomerID != r.CustomerID {
			return ErrBillingMutationConflict
		}
		if !current.BillingClosedAt.IsZero() {
			return errSkipWrite
		}
		current.BillingClosedAt = m.cfg.Now().UTC()
		current.Pending = nil
		current.Entitled = plans.Free
		current.ManagedSubscriptionID = ""
		current.PastDueSince = nil
		return nil
	})
	return err
}
