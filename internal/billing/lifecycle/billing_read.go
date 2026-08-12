package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/witwave-ai/witself/internal/billing"
)

// ErrProviderRequest identifies a failure returned by the configured billing
// provider. HTTP callers can map it to a generic dependency failure without
// disclosing provider-specific error detail. Store and lifecycle failures are
// deliberately not wrapped in this sentinel.
var ErrProviderRequest = errors.New("billing provider request failed")

// BillingSummary is the provider-neutral, customer-safe billing read model.
// Configured means a provider customer/billing relationship has been durably
// pinned for the account; it does not report current provider availability.
// The summary deliberately exposes neither the provider name nor customer id.
// PaymentMethod is display-only metadata supplied by the provider, and
// NextCharge is the provider's actual renewal preview rather than a catalog
// price that could be wrong for a grandfathered subscription.
type BillingSummary struct {
	Configured    bool
	PaymentMethod *billing.PaymentMethod
	NextCharge    *billing.UpcomingCharge
}

// ReadBillingSummary returns the provider-backed billing summary for an
// account. Reading an unknown/free account is side-effect free: no lifecycle
// record or provider customer is created. A providerless manager can therefore
// answer cleanly while the account has no billing customer.
func (m *Manager) ReadBillingSummary(
	ctx context.Context,
	accountID, email string,
) (BillingSummary, error) {
	r, err := m.load(ctx, accountID, email)
	if err != nil {
		return BillingSummary{}, err
	}
	summary := BillingSummary{
		Configured: strings.TrimSpace(r.CustomerID) != "",
	}
	if !summary.Configured {
		return summary, nil
	}
	_, provider, err := m.providerFor(r)
	if err != nil {
		return BillingSummary{}, err
	}
	paymentMethod, err := provider.PaymentMethodOnFile(ctx, r.CustomerID)
	if err != nil {
		return BillingSummary{}, wrapProviderRequest(err)
	}
	nextCharge, err := provider.NextCharge(ctx, r.CustomerID)
	if err != nil {
		return BillingSummary{}, wrapProviderRequest(err)
	}
	if paymentMethod != nil {
		value := *paymentMethod
		summary.PaymentMethod = &value
	}
	if nextCharge != nil {
		value := *nextCharge
		summary.NextCharge = &value
	}
	return summary, nil
}

// ListBillingInvoices returns provider invoice history newest first. Unknown
// and free accounts have an empty history and do not create billing objects.
func (m *Manager) ListBillingInvoices(
	ctx context.Context,
	accountID, email string,
) ([]billing.Invoice, error) {
	r, err := m.load(ctx, accountID, email)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(r.CustomerID) == "" {
		return []billing.Invoice{}, nil
	}
	_, provider, err := m.providerFor(r)
	if err != nil {
		return nil, err
	}
	invoices, err := provider.ListInvoices(ctx, r.CustomerID)
	if err != nil {
		return nil, wrapProviderRequest(err)
	}
	return append([]billing.Invoice{}, invoices...), nil
}

// ListBillingPayments returns provider payment history newest first. Unknown
// and free accounts have an empty history and do not create billing objects.
func (m *Manager) ListBillingPayments(
	ctx context.Context,
	accountID, email string,
) ([]billing.Payment, error) {
	r, err := m.load(ctx, accountID, email)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(r.CustomerID) == "" {
		return []billing.Payment{}, nil
	}
	_, provider, err := m.providerFor(r)
	if err != nil {
		return nil, err
	}
	payments, err := provider.ListPayments(ctx, r.CustomerID)
	if err != nil {
		return nil, wrapProviderRequest(err)
	}
	return append([]billing.Payment{}, payments...), nil
}

// CreateBillingSetup creates a provider-hosted payment-method setup action.
// It is the one read-surface operation allowed to create a provider customer:
// the provider's idempotent EnsureCustomer call is folded into the lifecycle
// record with CAS, and a concurrent winner is always re-read and used rather
// than overwritten. SetupLink itself may return a fresh hosted session on each
// call; only the provider-customer establishment is idempotent here.
func (m *Manager) CreateBillingSetup(
	ctx context.Context,
	accountID, email string,
) (billing.Action, error) {
	r, provider, err := m.ensureBillingCustomer(ctx, accountID, email)
	if err != nil {
		return billing.Action{}, err
	}
	action, err := provider.SetupLink(ctx, r.CustomerID)
	if err != nil {
		return billing.Action{}, wrapProviderRequest(err)
	}
	validURL := action.URL != "" && validBillingMutationURL(action.URL)
	switch {
	case action.Done && action.URL == "":
	case !action.Done && validURL:
	default:
		return billing.Action{}, invalidProviderAction("setup")
	}
	return action, nil
}

// createBillingSetupMutation creates or replays one hosted setup flow for an
// already-claimed schema-2 receipt. Keeping this private prevents callers from
// bypassing the durable envelope, approval, queue, and retry-horizon guards.
func (m *Manager) createBillingSetupMutation(
	ctx context.Context,
	accountID, email, operationID string,
) (billing.Action, error) {
	if !validBillingOperationID(operationID) {
		return billing.Action{}, errors.New("lifecycle: invalid billing operation id")
	}
	r, provider, err := m.ensureBillingCustomer(ctx, accountID, email)
	if err != nil {
		return billing.Action{}, err
	}
	idempotent, ok := provider.(billing.IdempotentSetupper)
	if !ok {
		name, _, providerErr := m.providerFor(r)
		if providerErr != nil {
			return billing.Action{}, providerErr
		}
		return billing.Action{}, fmt.Errorf(
			"billing provider %q does not support idempotent setup operations",
			name)
	}
	action, err := idempotent.SetupLinkIdempotent(
		ctx, r.CustomerID, operationID)
	if err != nil {
		return billing.Action{}, wrapProviderRequest(err)
	}
	validURL := action.URL != "" && validBillingMutationURL(action.URL)
	switch {
	case action.Done && action.URL == "":
	case !action.Done && validURL:
	default:
		return billing.Action{}, invalidProviderAction("setup")
	}
	return action, nil
}

// CreateBillingPortal returns a provider-hosted customer portal URL. Portal
// creation never manufactures a customer; callers use CreateBillingSetup for
// the explicit first-contact flow.
func (m *Manager) CreateBillingPortal(
	ctx context.Context,
	accountID, email string,
) (string, error) {
	if !m.BillingAvailable() {
		return "", ErrBillingUnavailable
	}
	r, err := m.load(ctx, accountID, email)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(r.CustomerID) == "" {
		return "", refuse("billing portal is unavailable until payment setup has started")
	}
	_, provider, err := m.providerFor(r)
	if err != nil {
		return "", err
	}
	url, err := provider.PortalLink(ctx, r.CustomerID)
	if err != nil {
		return "", wrapProviderRequest(err)
	}
	if url == "" || url != strings.TrimSpace(url) {
		return "", invalidProviderAction("portal")
	}
	return url, nil
}

// ensureBillingCustomer returns the authoritative persisted customer and its
// pinned provider. EnsureCustomer is contractually idempotent per account, but
// the CAS fold still treats the stored record as authority if concurrent calls
// return different ids: a loser never overwrites the winner and every caller
// creates its setup action for the persisted customer.
func (m *Manager) ensureBillingCustomer(
	ctx context.Context,
	accountID, email string,
) (Record, billing.Provider, error) {
	if !m.BillingAvailable() {
		return Record{}, nil, ErrBillingUnavailable
	}
	for range casAttempts {
		seed, err := m.load(ctx, accountID, email)
		if err != nil {
			return Record{}, nil, err
		}
		if strings.TrimSpace(seed.CustomerID) != "" {
			_, provider, err := m.providerFor(seed)
			return seed, provider, err
		}

		name, provider, err := m.providerFor(seed)
		if err != nil {
			return Record{}, nil, err
		}
		customerID, err := provider.EnsureCustomer(ctx, accountID, email)
		if err != nil {
			return Record{}, nil, wrapProviderRequest(err)
		}
		customerID = strings.TrimSpace(customerID)
		if customerID == "" {
			return Record{}, nil, invalidProviderAction("customer")
		}

		folded, err := m.mutate(ctx, accountID, email, func(r *Record) error {
			if strings.TrimSpace(r.CustomerID) != "" {
				return errSkipWrite
			}
			// A provider can be pre-pinned before its customer is known. If a
			// concurrent writer changed that pin while EnsureCustomer ran,
			// re-read and route through the new authority instead of clobbering.
			if r.Provider != "" && r.Provider != name {
				return errSkipWrite
			}
			r.Provider = name
			r.CustomerID = customerID
			return nil
		})
		if err != nil {
			return Record{}, nil, err
		}
		if strings.TrimSpace(folded.CustomerID) == "" {
			continue
		}
		_, authoritativeProvider, err := m.providerFor(folded)
		if err != nil {
			return Record{}, nil, err
		}
		return folded, authoritativeProvider, nil
	}
	return Record{}, nil, fmt.Errorf(
		"lifecycle: account %s: billing customer authority changed too often",
		accountID,
	)
}

func wrapProviderRequest(err error) error {
	return fmt.Errorf("%w: %w", ErrProviderRequest, err)
}

func invalidProviderAction(kind string) error {
	return fmt.Errorf("%w: billing provider returned an invalid %s action",
		ErrProviderRequest, kind)
}
