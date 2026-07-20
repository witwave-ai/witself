package client

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// DashboardPreferences is the agent's dashboard UI preference row. Prefs is
// the canonical strictly validated document the server accepted.
type DashboardPreferences struct {
	AgentID   string          `json:"agent_id"`
	Prefs     json.RawMessage `json:"prefs"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// GetDashboardPreferences reads the authenticated agent's own dashboard
// preference row. A nil result is the valid state where the agent has never
// stored preferences.
func GetDashboardPreferences(ctx context.Context, endpoint, token string) (*DashboardPreferences, error) {
	var out struct {
		Preferences *DashboardPreferences `json:"preferences"`
	}
	if err := doJSON(ctx, http.MethodGet, dashboardPreferencesURL(endpoint), token, nil, &out); err != nil {
		return nil, err
	}
	return out.Preferences, nil
}

// PutDashboardPreferences upserts the authenticated agent's own dashboard
// preference row. The server validates the strict v1 document shape; prefs is
// forwarded as-is.
func PutDashboardPreferences(ctx context.Context, endpoint, token string, prefs json.RawMessage) (*DashboardPreferences, error) {
	body, err := json.Marshal(map[string]json.RawMessage{"prefs": prefs})
	if err != nil {
		return nil, err
	}
	var out struct {
		Preferences *DashboardPreferences `json:"preferences"`
	}
	if err := doJSON(ctx, http.MethodPut, dashboardPreferencesURL(endpoint), token, body, &out); err != nil {
		return nil, err
	}
	return out.Preferences, nil
}

func dashboardPreferencesURL(endpoint string) string {
	return strings.TrimRight(endpoint, "/") + "/v1/self/dashboard-preferences"
}
