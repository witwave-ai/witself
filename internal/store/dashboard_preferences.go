package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	// DashboardPreferencesSchema is the exact schema marker every stored
	// dashboard-preferences document must carry. v1 is deliberately strict —
	// unknown keys are refused — so later growth happens by versioning the
	// marker, never by silently accumulating unvalidated keys.
	DashboardPreferencesSchema = "witself.dashboard-prefs.v1"

	// maxDashboardPreferencesBytes caps the canonical prefs document. The
	// migration enforces the same bound as a CHECK; this validator is the
	// authoritative gate.
	maxDashboardPreferencesBytes = 4096
	// maxDashboardThemeBytes bounds the theme name. Theme names are embedded
	// CSS pack basenames plus the client-side "auto" mode, never URLs.
	maxDashboardThemeBytes = 64
)

// dashboardThemePattern matches the dashboard UI's own theme-name whitelist
// (static/app.js). The UI additionally validates any stored name against the
// embedded theme list before it can become a stylesheet URL; this store-side
// pattern keeps garbage out of the row in the first place.
var dashboardThemePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

var (
	// ErrDashboardPreferencesForbidden reports a preferences operation by a
	// principal other than a full agent identity. The row is agent-scoped:
	// operators and curator profiles have no preferences surface.
	ErrDashboardPreferencesForbidden = errors.New("dashboard preferences access forbidden")
	// ErrDashboardPreferencesInvalid reports a prefs document outside the
	// strict v1 contract (wrong shape, unknown keys, oversized, bad theme).
	ErrDashboardPreferencesInvalid = errors.New("invalid dashboard preferences")
)

// DashboardPreferences is one agent's dashboard UI preference row. Prefs is
// the strictly validated document as re-serialized by jsonb (which owns key
// order and spacing); UpdatedAt is stamped by Postgres.
type DashboardPreferences struct {
	AgentID   string          `json:"agent_id"`
	Prefs     json.RawMessage `json:"prefs"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// dashboardPreferencesDocument is the strict v1 prefs shape. Both keys are
// required; canonical serialization is exactly this field order.
type dashboardPreferencesDocument struct {
	Schema string `json:"schema"`
	Theme  string `json:"theme"`
}

// validateDashboardPreferences enforces the strict v1 contract — a flat JSON
// object {"schema":"witself.dashboard-prefs.v1","theme":<string<=64>} with
// unknown keys rejected — and returns the canonical serialized form.
func validateDashboardPreferences(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 || len(raw) > maxDashboardPreferencesBytes {
		return nil, fmt.Errorf("%w: prefs must be a JSON object of at most %d bytes",
			ErrDashboardPreferencesInvalid, maxDashboardPreferencesBytes)
	}
	keyed := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &keyed); err != nil {
		return nil, fmt.Errorf("%w: prefs must be a JSON object", ErrDashboardPreferencesInvalid)
	}
	for key := range keyed {
		if key != "schema" && key != "theme" {
			return nil, fmt.Errorf("%w: unknown key %q", ErrDashboardPreferencesInvalid, key)
		}
	}
	var doc dashboardPreferencesDocument
	if json.Unmarshal(keyed["schema"], &doc.Schema) != nil || doc.Schema != DashboardPreferencesSchema {
		return nil, fmt.Errorf("%w: schema must be %q", ErrDashboardPreferencesInvalid, DashboardPreferencesSchema)
	}
	if json.Unmarshal(keyed["theme"], &doc.Theme) != nil ||
		len(doc.Theme) > maxDashboardThemeBytes || !dashboardThemePattern.MatchString(doc.Theme) {
		return nil, fmt.Errorf("%w: theme must be a clean name of at most %d bytes",
			ErrDashboardPreferencesInvalid, maxDashboardThemeBytes)
	}
	canonical, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDashboardPreferencesInvalid, err)
	}
	return canonical, nil
}

func requireSelfDashboardPrincipal(p Principal) error {
	if p.Kind != PrincipalAgent ||
		(strings.TrimSpace(p.AccessProfile) != "" && p.AccessProfile != AccessProfileFull) ||
		p.AccountID == "" || p.RealmID == "" || p.ID == "" {
		return ErrDashboardPreferencesForbidden
	}
	return nil
}

// GetDashboardPreferences returns the authenticated agent's own dashboard
// preference row, or nil when the agent has never stored one — a valid
// default state the caller renders as empty preferences. Pure SELECT: no
// usage recording, no audit event, so the dashboard may read it on boot.
func (s *Store) GetDashboardPreferences(ctx context.Context, p Principal) (*DashboardPreferences, error) {
	if err := requireSelfDashboardPrincipal(p); err != nil {
		return nil, err
	}
	var out DashboardPreferences
	err := s.pool.QueryRow(ctx, `
		SELECT dp.agent_id, dp.prefs, dp.updated_at
		  FROM agent_dashboard_preferences dp
		 WHERE dp.agent_id = $1 AND dp.realm_id = $2 AND dp.account_id = $3`,
		p.ID, p.RealmID, p.AccountID,
	).Scan(&out.AgentID, &out.Prefs, &out.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read dashboard preferences: %w", err)
	}
	return &out, nil
}

// PutDashboardPreferences upserts the authenticated agent's own dashboard
// preference row after strict validation. Last-write-wins by design — UI
// preferences are low stakes, so there is deliberately no revision fence or
// idempotency machinery; the newest theme choice simply sticks.
//
// No account_events verb is written here, by the ledger's own bar
// (events.go: "the finite namespace of things worth remembering ... do
// owners need to see it?"): a theme flip is value-free UI state that would
// spam the audit ledger on every toggle, and the repo's precedent for
// per-agent housekeeping writes — agent_activity's TouchAgentActivity — also
// carries no verb. Content-plane sibling writes (secrets, memories, facts)
// keep theirs; this row is not agent content.
func (s *Store) PutDashboardPreferences(ctx context.Context, p Principal, prefs json.RawMessage) (DashboardPreferences, error) {
	if err := requireSelfDashboardPrincipal(p); err != nil {
		return DashboardPreferences{}, err
	}
	canonical, err := validateDashboardPreferences(prefs)
	if err != nil {
		return DashboardPreferences{}, err
	}
	var out DashboardPreferences
	err = s.pool.QueryRow(ctx, `
		INSERT INTO agent_dashboard_preferences
		       (agent_id, account_id, realm_id, prefs, updated_at)
		SELECT a.id, r.account_id, a.realm_id, $4::jsonb, clock_timestamp()
		  FROM agents a
		  JOIN realms r ON r.id = a.realm_id
		 WHERE a.id = $1 AND a.realm_id = $2 AND r.account_id = $3
		   AND a.deleted_at IS NULL AND r.deleted_at IS NULL
		ON CONFLICT (agent_id) DO UPDATE
		   SET prefs = EXCLUDED.prefs,
		       updated_at = clock_timestamp()
		RETURNING agent_id, prefs, updated_at`,
		p.ID, p.RealmID, p.AccountID, canonical,
	).Scan(&out.AgentID, &out.Prefs, &out.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return DashboardPreferences{}, ErrAgentNotFound
	}
	if err != nil {
		return DashboardPreferences{}, fmt.Errorf("write dashboard preferences: %w", err)
	}
	return out, nil
}
