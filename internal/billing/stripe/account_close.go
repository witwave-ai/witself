package stripe

import (
	"context"
	"errors"
	"net/url"

	"github.com/witwave-ai/witself/internal/billing"
)

var _ billing.AccountClosureCanceller = (*Provider)(nil)

var errClosureUnconfirmed = errors.New("stripe: account billing cancellation is not confirmed; retry account closure")

type closureCheckout struct {
	ID           string            `json:"id"`
	Customer     string            `json:"customer"`
	Mode         string            `json:"mode"`
	Status       string            `json:"status"`
	Subscription string            `json:"subscription"`
	Metadata     map[string]string `json:"metadata"`
}

// CancelForAccountClosure ends renewal immediately, without issuing a final
// invoice, proration, or refund. The lifecycle manager durably freezes new
// purchases before calling this method. Provider readback, rather than a local
// success flag, makes retries safe when cancellation succeeded but its reply
// was lost. No provider error bodies or customer values escape this boundary.
func (p *Provider) CancelForAccountClosure(ctx context.Context, customerID, accountID, operationID string) error {
	if validateStripeResourceID(customerID, "cus_") != nil || accountID == "" || billing.ValidateOperationID(operationID) != nil {
		return errClosureUnconfirmed
	}
	var customer struct {
		ID       string            `json:"id"`
		Deleted  bool              `json:"deleted"`
		Metadata map[string]string `json:"metadata"`
	}
	if err := p.call(ctx, "GET", "/v1/customers/"+url.PathEscape(customerID), nil, "", &customer); err != nil ||
		customer.ID != customerID || customer.Deleted || customer.Metadata["witself_account"] != accountID {
		return errClosureUnconfirmed
	}
	// Disarm every outstanding checkout first. A checkout completed concurrently
	// with expiry must have an identified subscription, which is cancelled below.
	checkouts, err := p.closureOpenCheckouts(ctx, customerID)
	if err != nil {
		return err
	}
	completed := make(map[string]bool)
	for _, session := range checkouts {
		path := "/v1/checkout/sessions/" + url.PathEscape(session.ID)
		var result closureCheckout
		err := p.call(ctx, "POST", path+"/expire", url.Values{},
			childIdempotencyKey(operationID, "close-checkout", session.ID), &result)
		if err != nil {
			// Expiry can race completion, or lose a successful response. Only an
			// authoritative terminal read can turn either into a safe continuation.
			if p.call(ctx, "GET", path, nil, "", &result) != nil {
				return errClosureUnconfirmed
			}
		}
		if result.ID != session.ID || result.Customer != customerID || result.Mode != session.Mode {
			return errClosureUnconfirmed
		}
		switch result.Status {
		case "expired":
		case "complete":
			if session.Mode == "subscription" {
				if validateStripeResourceID(result.Subscription, "sub_") != nil {
					return errClosureUnconfirmed
				}
				completed[result.Subscription] = true
			}
		default:
			return errClosureUnconfirmed
		}
	}
	if err := p.cancelClosureSubscriptions(ctx, customerID, operationID, completed); err != nil {
		return err
	}
	// A final complete projection is required: unknown statuses, truncated lists,
	// and new sessions/subscriptions never count as proof of cancellation.
	remaining, err := p.closureOpenCheckouts(ctx, customerID)
	if err != nil || len(remaining) != 0 {
		return errClosureUnconfirmed
	}
	subs, err := p.closureSubscriptions(ctx, customerID)
	if err != nil {
		return err
	}
	for _, sub := range subs {
		if !closureTerminalSubscription(sub.Status) {
			return errClosureUnconfirmed
		}
	}
	return nil
}

func (p *Provider) closureOpenCheckouts(ctx context.Context, customerID string) ([]closureCheckout, error) {
	var list struct {
		Data    *[]closureCheckout `json:"data"`
		HasMore *bool              `json:"has_more"`
	}
	query := url.Values{"customer": {customerID}, "status": {"open"}, "limit": {"100"}}
	if p.call(ctx, "GET", "/v1/checkout/sessions?"+query.Encode(), nil, "", &list) != nil ||
		list.Data == nil || list.HasMore == nil || *list.HasMore {
		return nil, errClosureUnconfirmed
	}
	seen := make(map[string]bool)
	for _, session := range *list.Data {
		if validateStripeResourceID(session.ID, "cs_") != nil || seen[session.ID] ||
			session.Customer != customerID || session.Status != "open" {
			return nil, errClosureUnconfirmed
		}
		seen[session.ID] = true
		switch session.Mode {
		case "setup":
		case "subscription":
			plan, ok := p.cfg.Catalog.Get(session.Metadata["witself_plan"])
			if !ok || !plan.Paid() {
				return nil, errClosureUnconfirmed
			}
		default:
			return nil, errClosureUnconfirmed
		}
	}
	return *list.Data, nil
}

func closureTerminalSubscription(status string) bool {
	return status == "canceled" || status == "incomplete_expired"
}

func closureCancellableSubscription(status string) bool {
	return liveStripeSubscriptionStatus(status) || status == "incomplete" || status == "paused"
}

func (p *Provider) closureSubscriptions(ctx context.Context, customerID string) ([]stripeSubscription, error) {
	var list struct {
		Data    *[]stripeSubscription `json:"data"`
		HasMore *bool                 `json:"has_more"`
	}
	query := url.Values{"customer": {customerID}, "status": {"all"}, "limit": {"100"}}
	if p.call(ctx, "GET", "/v1/subscriptions?"+query.Encode(), nil, "", &list) != nil ||
		list.Data == nil || list.HasMore == nil || *list.HasMore {
		return nil, errClosureUnconfirmed
	}
	seen := make(map[string]bool)
	for _, sub := range *list.Data {
		if validateStripeResourceID(sub.ID, "sub_") != nil || seen[sub.ID] || sub.Customer != customerID ||
			(!closureTerminalSubscription(sub.Status) && !closureCancellableSubscription(sub.Status)) {
			return nil, errClosureUnconfirmed
		}
		seen[sub.ID] = true
	}
	return *list.Data, nil
}

func (p *Provider) cancelClosureSubscriptions(ctx context.Context, customerID, operationID string, completed map[string]bool) error {
	subs, err := p.closureSubscriptions(ctx, customerID)
	if err != nil {
		return err
	}
	for _, sub := range subs {
		if !closureTerminalSubscription(sub.Status) {
			completed[sub.ID] = true
		}
	}
	// Read and validate every target before cancelling any subscription. The
	// customer's account metadata alone is not permission to cancel foreign plans.
	targets := make([]stripeSubscription, 0, len(completed))
	for id := range completed {
		var sub stripeSubscription
		path := "/v1/subscriptions/" + url.PathEscape(id)
		if p.call(ctx, "GET", path+"?expand[]=items.data.price.product", nil, "", &sub) != nil ||
			sub.ID != id || sub.Customer != customerID {
			return errClosureUnconfirmed
		}
		if closureTerminalSubscription(sub.Status) {
			continue
		}
		if !closureCancellableSubscription(sub.Status) {
			return errClosureUnconfirmed
		}
		if _, err := p.subscriptionPlan(sub); err != nil {
			return errClosureUnconfirmed
		}
		targets = append(targets, sub)
	}
	for _, sub := range targets {
		var result stripeSubscription
		path := "/v1/subscriptions/" + url.PathEscape(sub.ID)
		err := p.call(ctx, "DELETE", path, url.Values{"invoice_now": {"false"}, "prorate": {"false"}},
			childIdempotencyKey(operationID, "close-subscription", sub.ID), &result)
		if err != nil && p.call(ctx, "GET", path, nil, "", &result) != nil {
			return errClosureUnconfirmed
		}
		if result.ID != sub.ID || result.Customer != customerID || !closureTerminalSubscription(result.Status) {
			return errClosureUnconfirmed
		}
	}
	return nil
}
