package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// syncPreferencesFault is an unexported test seam. The only production call
// constructs its zero value; there is no option, environment variable or API
// through which a caller can select an omission.
type syncPreferencesFault struct {
	omitTouch   bool
	omitSeal    bool
	emptySeal   bool
	second      *Principal
	repeatWrite bool
}

// The selected path preserves the original upsert, including PostgreSQL's
// response bytes and its deliberate absence of an audit event. Even inactive
// selected writers hold the shared gate through commit, closing the race with
// an activation that occurs between a control read and the first write.
func (s *Store) putDashboardPreferencesSync(ctx context.Context, p Principal, canonical json.RawMessage, fault syncPreferencesFault) (DashboardPreferences, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return DashboardPreferences{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock_shared(capture_lock_key($1))`, p.AccountID); err != nil {
		return DashboardPreferences{}, syncCaptureError(err)
	}
	var active bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sync_writer_capabilities w
		WHERE w.account_id=c.account_id AND w.enabled AND w.revoked_generation=0 AND w.capture_epoch=c.capture_epoch)
		FROM sync_capture c WHERE c.account_id=$1 FOR SHARE`, p.AccountID).Scan(&active)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return DashboardPreferences{}, syncCaptureError(err)
	}
	var admitted *SyncTx
	dependency := struct {
		AgentID       string `json:"agent_id"`
		RealmID       string `json:"realm_id"`
		PriorRevision string `json:"prior_revision"`
	}{p.ID, p.RealmID, "absent"}
	if active {
		admitted, err = admitSyncTx(ctx, tx, p.AccountID, SyncPreferencesPrincipal, SyncPreferencesCapability)
		if err != nil {
			return DashboardPreferences{}, err
		}
		// Serialize this resource before deriving the causal predecessor, including
		// the no-preference-row case. This is still last-write-wins: no caller
		// revision precondition is introduced.
		var agentID string
		if err := tx.QueryRow(ctx, `SELECT a.id FROM agents a JOIN realms r ON r.id=a.realm_id
			WHERE a.id=$1 AND a.realm_id=$2 AND r.account_id=$3
			AND a.deleted_at IS NULL AND r.deleted_at IS NULL FOR UPDATE OF a`,
			p.ID, p.RealmID, p.AccountID).Scan(&agentID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return DashboardPreferences{}, ErrAgentNotFound
			}
			return DashboardPreferences{}, syncCaptureError(err)
		}
		var prior, priorDocument string
		var priorTime time.Time
		err = tx.QueryRow(ctx, `SELECT xmin::text,prefs::text,updated_at FROM agent_dashboard_preferences
			WHERE agent_id=$1 AND realm_id=$2 AND account_id=$3 FOR UPDATE`,
			p.ID, p.RealmID, p.AccountID).Scan(&prior, &priorDocument, &priorTime)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return DashboardPreferences{}, syncCaptureError(err)
		}
		if err == nil {
			// The baseline marker is private evidence, not a sequence maximum.
			// Subsequent captured writes depend on the durable operation identity.
			baseline, _ := json.Marshal([]string{p.AccountID, p.RealmID, p.ID, prior, priorDocument, priorTime.UTC().Format(time.RFC3339Nano)})
			digest := sha256.Sum256(baseline)
			dependency.PriorRevision = "baseline:" + hex.EncodeToString(digest[:])
			var operationID []byte
			err = tx.QueryRow(ctx, `SELECT t.operation_id FROM sync_tx_touches t
				JOIN sync_bundles b ON b.account_id=t.account_id AND b.tx_id=t.tx_id
				WHERE t.account_id=$1 AND b.capture_epoch=$2 AND t.dependencies->>'agent_id'=$3
				AND t.dependencies->>'realm_id'=$4 ORDER BY b.finalized DESC,t.touch_id DESC LIMIT 1`,
				p.AccountID, admitted.epoch, p.ID, p.RealmID).Scan(&operationID)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return DashboardPreferences{}, syncCaptureError(err)
			}
			if err == nil {
				dependency.PriorRevision = "operation:" + hex.EncodeToString(operationID)
			}
		}
	}
	dependencies, err := json.Marshal(dependency)
	if err != nil {
		return DashboardPreferences{}, ErrSyncWriterNotAdmitted
	}
	out, err := putSyncDashboardPreferencesSQL(ctx, tx, p, canonical)
	if err != nil {
		return DashboardPreferences{}, err
	}
	if admitted != nil {
		if fault.second != nil {
			if _, err := putSyncDashboardPreferencesSQL(ctx, tx, *fault.second, canonical); err != nil {
				return DashboardPreferences{}, err
			}
		}
		if fault.repeatWrite {
			if _, err := putSyncDashboardPreferencesSQL(ctx, tx, p, canonical); err != nil {
				return DashboardPreferences{}, err
			}
		}
		if !fault.omitTouch && !fault.emptySeal {
			if err := admitted.Touch(ctx, SyncPreferencesCapability, "preferences", canonical, dependencies); err != nil {
				return DashboardPreferences{}, err
			}
		}
		if !fault.omitSeal {
			if err := admitted.Seal(ctx); err != nil {
				return DashboardPreferences{}, err
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return DashboardPreferences{}, syncCaptureError(err)
	}
	return out, nil
}

func putSyncDashboardPreferencesSQL(ctx context.Context, tx pgx.Tx, p Principal, canonical json.RawMessage) (DashboardPreferences, error) {
	var out DashboardPreferences
	err := tx.QueryRow(ctx, `
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
		return DashboardPreferences{}, fmt.Errorf("write dashboard preferences: %w", syncCaptureError(err))
	}
	return out, nil
}
