package main

import (
	"context"
	"errors"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/server"
	"github.com/witwave-ai/witself/internal/store"
)

type accountCloseStore interface {
	CheckAccountClose(context.Context, string, string) error
	CloseAccount(context.Context, string, string, string) error
}

// closeAccountWithBilling never holds a database transaction across the
// provider request. The final close rechecks all eligibility fences; a vault
// operation started during cancellation can still refuse closure. Billing
// remains frozen in that case so the owner can settle that operation and retry.
func closeAccountWithBilling(ctx context.Context, st accountCloseStore, endpoint, provisionToken, accountID, operatorID, reason, ownerBearer string) error {
	if endpoint != "" {
		if err := st.CheckAccountClose(ctx, accountID, operatorID); err != nil {
			return accountCloseStoreError(err)
		}
		if err := client.CloseAccountBilling(ctx, endpoint, accountID, ownerBearer, provisionToken); err != nil {
			return server.ErrAccountBillingClosePending
		}
	}
	return accountCloseStoreError(st.CloseAccount(ctx, accountID, operatorID, reason))
}

func accountCloseStoreError(err error) error {
	switch {
	case errors.Is(err, store.ErrNotAccountOwner):
		return server.ErrNotAccountOwner
	case errors.Is(err, store.ErrCannotCloseDefault):
		return server.ErrCannotCloseDefault
	case errors.Is(err, store.ErrVaultLifecycleInProgress):
		return server.ErrConflict
	default:
		return err
	}
}
