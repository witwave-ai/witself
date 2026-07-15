package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

const (
	maxAgentActivityLabelBytes    = 128
	maxAgentActivityLocationBytes = 256
)

var (
	// ErrAgentActivityForbidden reports an activity operation attempted by a
	// principal other than a full agent identity.
	ErrAgentActivityForbidden = errors.New("agent activity access forbidden")
	// ErrAgentActivityInputInvalid reports malformed or unbounded client
	// provenance. Account, realm, agent, and public activity time are never
	// client inputs.
	ErrAgentActivityInputInvalid = errors.New("invalid agent activity input")
)

// AgentActivityInput is one client-observed lifecycle event. EventOccurredAt
// is used only to order retries and delayed hook delivery within one
// runtime/location projection. LastActivityAt is always stamped by Postgres.
type AgentActivityInput struct {
	Runtime         string
	LocationID      string
	Location        string
	Event           string
	EventID         string
	EventOccurredAt time.Time
}

// AgentActivity is the latest accepted event for one agent runtime at one
// installation. EventID and EventOccurredAt are internal retry/order fields;
// peer views intentionally omit them.
type AgentActivity struct {
	AgentID         string
	Runtime         string
	LocationID      string
	Location        string
	Event           string
	EventID         string
	EventOccurredAt time.Time
	LastActivityAt  time.Time
}

// AgentPeer is one live, same-realm peer and its newest server-observed
// activity projection. Activity fields are empty when no hook has reported an
// event for that peer.
type AgentPeer struct {
	ID             string
	Name           string
	LastActivityAt *time.Time
	LastRuntime    string
	LastLocation   string
	LastEvent      string
}

// TouchAgentActivity conditionally advances one latest-only activity projection.
// The token-derived principal is the only source of account, realm, and agent
// identity. Replaying the same event, or delivering an older event after a
// newer one, is a no-op and returns the already-current projection.
func (s *Store) TouchAgentActivity(ctx context.Context, p Principal, in AgentActivityInput) (AgentActivity, error) {
	if !fullAgentActivityPrincipal(p) {
		return AgentActivity{}, ErrAgentActivityForbidden
	}
	var err error
	in, err = normalizeAgentActivityInput(in)
	if err != nil {
		return AgentActivity{}, err
	}

	var activity AgentActivity
	err = s.pool.QueryRow(ctx, `
		WITH observation AS MATERIALIZED (
			SELECT clock_timestamp() AS observed_at
		)
		INSERT INTO agent_activity
		       (agent_id, runtime, location_id, location, last_event,
		        last_event_id, last_event_occurred_at, last_activity_at)
		SELECT a.id, $4, $5, $6, $7, $8,
		       LEAST($9, observation.observed_at), observation.observed_at
		  FROM agents a
		  JOIN realms r ON r.id = a.realm_id
		 CROSS JOIN observation
		 WHERE a.id = $1 AND a.realm_id = $2 AND r.account_id = $3
		   AND a.deleted_at IS NULL AND r.deleted_at IS NULL
		ON CONFLICT (agent_id, runtime, location_id) DO UPDATE
		   SET location = EXCLUDED.location,
		       last_event = EXCLUDED.last_event,
		       last_event_id = EXCLUDED.last_event_id,
		       last_event_occurred_at = EXCLUDED.last_event_occurred_at,
		       last_activity_at = GREATEST(
		           agent_activity.last_activity_at,
		           clock_timestamp()
		       )
		 WHERE agent_activity.last_event_id <> EXCLUDED.last_event_id
		   AND agent_activity.last_event_occurred_at < EXCLUDED.last_event_occurred_at
		RETURNING agent_id, runtime, location_id, location, last_event,
		          last_event_id, last_event_occurred_at, last_activity_at`,
		p.ID, p.RealmID, p.AccountID, in.Runtime, in.LocationID, in.Location,
		in.Event, in.EventID, in.EventOccurredAt,
	).Scan(
		&activity.AgentID, &activity.Runtime, &activity.LocationID,
		&activity.Location, &activity.Event, &activity.EventID,
		&activity.EventOccurredAt, &activity.LastActivityAt,
	)
	if err == nil {
		return activity, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return AgentActivity{}, fmt.Errorf("touch agent activity: %w", err)
	}

	// A conflict whose event was a retry or was not newer intentionally has no
	// RETURNING row. Read the current scoped projection so callers receive the
	// same result without moving the server-observed timestamp.
	err = s.pool.QueryRow(ctx, `
		SELECT aa.agent_id, aa.runtime, aa.location_id, aa.location,
		       aa.last_event, aa.last_event_id, aa.last_event_occurred_at,
		       aa.last_activity_at
		  FROM agent_activity aa
		  JOIN agents a ON a.id = aa.agent_id
		  JOIN realms r ON r.id = a.realm_id
		 WHERE aa.agent_id = $1 AND aa.runtime = $2 AND aa.location_id = $3
		   AND a.realm_id = $4 AND r.account_id = $5
		   AND a.deleted_at IS NULL AND r.deleted_at IS NULL`,
		p.ID, in.Runtime, in.LocationID, p.RealmID, p.AccountID,
	).Scan(
		&activity.AgentID, &activity.Runtime, &activity.LocationID,
		&activity.Location, &activity.Event, &activity.EventID,
		&activity.EventOccurredAt, &activity.LastActivityAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentActivity{}, ErrAgentNotFound
	}
	if err != nil {
		return AgentActivity{}, fmt.Errorf("read current agent activity: %w", err)
	}
	return activity, nil
}

// ListAgentPeers returns the requesting agent's live same-realm peers, oldest
// created first. Identity scope comes only from p; no account, realm, or agent
// selectors are accepted from the request surface.
func (s *Store) ListAgentPeers(ctx context.Context, p Principal) ([]AgentPeer, error) {
	if !fullAgentActivityPrincipal(p) {
		return nil, ErrAgentActivityForbidden
	}
	rows, err := s.pool.Query(ctx, `
		SELECT a.id, a.name, latest.last_activity_at,
		       COALESCE(latest.runtime, ''), COALESCE(latest.location, ''),
		       COALESCE(latest.last_event, '')
		  FROM agents a
		  JOIN realms r ON r.id = a.realm_id
		  LEFT JOIN LATERAL (
		       SELECT aa.last_activity_at, aa.runtime, aa.location, aa.last_event
		         FROM agent_activity aa
		        WHERE aa.agent_id = a.id
		        ORDER BY aa.last_activity_at DESC, aa.runtime, aa.location_id
		        LIMIT 1
		  ) latest ON true
		 WHERE a.realm_id = $1 AND r.account_id = $2 AND a.id <> $3
		   AND a.deleted_at IS NULL AND r.deleted_at IS NULL
		   AND EXISTS (
		       SELECT 1 FROM agents self
		        WHERE self.id = $3 AND self.realm_id = $1
		          AND self.deleted_at IS NULL
		   )
		 ORDER BY a.created_at, a.id`, p.RealmID, p.AccountID, p.ID)
	if err != nil {
		return nil, fmt.Errorf("list agent peers: %w", err)
	}
	defer rows.Close()

	peers := make([]AgentPeer, 0)
	for rows.Next() {
		var peer AgentPeer
		if err := rows.Scan(
			&peer.ID, &peer.Name, &peer.LastActivityAt, &peer.LastRuntime,
			&peer.LastLocation, &peer.LastEvent,
		); err != nil {
			return nil, fmt.Errorf("scan agent peer: %w", err)
		}
		peers = append(peers, peer)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list agent peers: %w", err)
	}
	return peers, nil
}

func fullAgentActivityPrincipal(p Principal) bool {
	return p.Kind == PrincipalAgent &&
		(strings.TrimSpace(p.AccessProfile) == "" || p.AccessProfile == AccessProfileFull)
}

func normalizeAgentActivityInput(in AgentActivityInput) (AgentActivityInput, error) {
	var err error
	if in.Runtime, err = cleanAgentActivityLabel("runtime", in.Runtime, maxAgentActivityLabelBytes, true); err != nil {
		return AgentActivityInput{}, err
	}
	if in.LocationID, err = cleanAgentActivityLabel("location_id", in.LocationID, maxAgentActivityLabelBytes, true); err != nil {
		return AgentActivityInput{}, err
	}
	if in.Location, err = cleanAgentActivityLabel("location", in.Location, maxAgentActivityLocationBytes, false); err != nil {
		return AgentActivityInput{}, err
	}
	if in.Event, err = cleanAgentActivityLabel("event", in.Event, maxAgentActivityLabelBytes, true); err != nil {
		return AgentActivityInput{}, err
	}
	if in.EventID, err = cleanAgentActivityLabel("event_id", in.EventID, maxAgentActivityLabelBytes, true); err != nil {
		return AgentActivityInput{}, err
	}
	if in.EventOccurredAt.IsZero() || in.EventOccurredAt.Year() < 1 || in.EventOccurredAt.Year() > 9999 {
		return AgentActivityInput{}, fmt.Errorf("%w: event_occurred_at is required", ErrAgentActivityInputInvalid)
	}
	in.EventOccurredAt = in.EventOccurredAt.UTC()
	return in, nil
}

func cleanAgentActivityLabel(name, value string, maximum int, required bool) (string, error) {
	value = strings.TrimSpace(value)
	if (required && value == "") || len(value) > maximum || !utf8.ValidString(value) {
		return "", fmt.Errorf("%w: %s must be a clean label no longer than %d bytes", ErrAgentActivityInputInvalid, name, maximum)
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return "", fmt.Errorf("%w: %s contains control characters", ErrAgentActivityInputInvalid, name)
		}
	}
	return value, nil
}
