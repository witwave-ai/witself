package fake

import (
	"context"
	"errors"
	"time"

	"github.com/witwave-ai/witself/internal/billing"
)

var _ billing.AccountClosureCanceller = (*Fake)(nil)

// CancelForAccountClosure disarms checkout and renewal atomically. Existing
// invoice/payment history stays intact; closing is not a refund operation.
func (f *Fake) CancelForAccountClosure(_ context.Context, customerID, accountID, operationID string) error {
	if err := billing.ValidateOperationID(operationID); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	c, err := f.cust(customerID)
	if err != nil || accountID == "" || c.accountID != accountID {
		return errors.New("fake billing: account closure customer mismatch")
	}
	c.pending = nil
	c.plan = ""
	c.periodEnd = time.Time{}
	return nil
}
