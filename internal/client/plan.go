package client

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// ApplyAccountPlan pushes a plan snapshot to a cell via the provision-token
// system endpoint POST {endpoint}/v1/accounts/{id}:plan — the control plane's
// half of "the CP computes, the cell enforces". provisionToken is the CP↔cell
// trust link (witself_prv_*), never an operator token.
func ApplyAccountPlan(ctx context.Context, endpoint, provisionToken, accountID, plan string, limits map[string]int64, features []string) error {
	body, err := json.Marshal(map[string]any{
		"plan":     plan,
		"limits":   limits,
		"features": features,
	})
	if err != nil {
		return fmt.Errorf("encode plan snapshot: %w", err)
	}
	url := strings.TrimRight(endpoint, "/") + "/v1/accounts/" + accountID + ":plan"
	return doJSON(ctx, "POST", url, provisionToken, body, nil)
}
