package dashboard

import (
	"encoding/json"
	"net/http"
)

// ViewerSchema identifies the console session identity contract.
const ViewerSchema = "witself.console.viewer.v1"

// ViewerBinding describes the immutable original session, not current authority.
// It contains no credential and remains present after revocation.
type ViewerBinding struct {
	AccountID  string `json:"account_id"`
	OperatorID string `json:"operator_id"`
	Role       string `json:"role"`
}

// ViewerContext reports the original session binding and current manager availability.
type ViewerContext struct {
	SchemaVersion string         `json:"schema_version"`
	AccountID     string         `json:"account_id"`
	RealmID       string         `json:"realm_id"`
	AgentID       string         `json:"agent_id"`
	Manager       *ViewerBinding `json:"manager"`
	Available     bool           `json:"available"`
}

func viewerContextHandler(cfg Config, accounts *accountCollector) http.Handler {
	snapshot := ViewerContext{SchemaVersion: ViewerSchema, AccountID: cfg.Identity.AccountID, RealmID: cfg.Identity.RealmID, AgentID: cfg.Identity.AgentID}
	// A claimed but invalid manager must never look like an agent-only session.
	if cfg.AccountManager != nil {
		p := cfg.AccountManager.Identity
		snapshot.Manager = &ViewerBinding{AccountID: p.AccountID, OperatorID: p.OperatorID, Role: p.Role}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		out := snapshot
		if out.Manager != nil {
			_, _, err := accounts.authority(r.Context())
			out.Available = err == nil
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
}
