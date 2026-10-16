package cpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/witwave-ai/witself/internal/client"
)

const accountCloseBridgeHeader = "X-Witself-Account-Close-Bridge"

// accountBillingCloseRoute is a cell-only callback, not a standalone customer
// cancellation API. The Worker validates the routed cell provision credential
// and substitutes its private bridge credential; owner authorization remains
// independently mandatory. The normal purchase cohort gate does not apply.
func accountBillingCloseRoute(cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if cfg.CloseAuthenticate == nil || cfg.InternalAuthenticate == nil {
			writeError(w, http.StatusServiceUnavailable, "account billing closure is unavailable")
			return
		}
		bridge := r.Header.Get(accountCloseBridgeHeader)
		if bridge == "" {
			writeError(w, http.StatusForbidden, "account closure requires the cell callback")
			return
		}
		trusted, err := cfg.InternalAuthenticate(r.Context(), bridge)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "account closure authorization is unavailable")
			return
		}
		if !trusted {
			writeError(w, http.StatusForbidden, "account closure requires the cell callback")
			return
		}
		closeCfg := cfg
		closeCfg.Authenticate = cfg.CloseAuthenticate
		withAccount(closeCfg, AccountPermissionAccountClose, accountBillingClose)(w, r)
	}
}

func accountBillingClose(
	cfg Config, w http.ResponseWriter, r *http.Request, accountID string, access AccountAccess,
) {
	if access.Role != "account_owner" {
		writeError(w, http.StatusForbidden, "only the account owner can close billing")
		return
	}
	var request struct{}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "an empty account closure object is required")
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeError(w, http.StatusBadRequest, "a single account closure object is required")
		return
	}
	if err := cfg.Manager.CloseAccountBilling(r.Context(), accountID); err != nil {
		writeBillingMutationManagerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"schema_version": "witself.v0",
		"account_id":     accountID,
		"status":         "billing_closed",
	})
}

// CellCloseAuthenticate authorizes the callback against the cell's owner-only
// eligibility surface, which also supports pending and suspended accounts.
func CellCloseAuthenticate(resolve CellResolver) AuthFunc {
	return func(ctx context.Context, accountID, bearer string, permission AccountPermission) (AccountAccess, bool, error) {
		if permission != AccountPermissionAccountClose {
			return AccountAccess{}, false, nil
		}
		endpoint, _, err := resolve(ctx, accountID)
		if err != nil {
			return AccountAccess{}, false, err
		}
		eligibility, err := client.GetAccountCloseEligibility(ctx, endpoint, bearer)
		if err != nil {
			if errors.Is(err, client.ErrUnauthorized) || errors.Is(err, client.ErrForbidden) {
				return AccountAccess{}, false, nil
			}
			return AccountAccess{}, false, err
		}
		if eligibility.AccountID != accountID || eligibility.AccountRole != "account_owner" ||
			strings.TrimSpace(eligibility.OperatorID) == "" {
			return AccountAccess{}, false, nil
		}
		return AccountAccess{
			ActorID: eligibility.OperatorID, Role: eligibility.AccountRole, Permission: permission,
		}, true, nil
	}
}
