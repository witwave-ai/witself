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

	"github.com/witwave-ai/witself/internal/activity"
)

// ActivityQuery selects a bounded UTC hourly reporting window.
type ActivityQuery struct {
	Since, Until time.Time
	Bucket       string
}

// ActivityReport contains recorded quantities and an immutable tracking boundary.
type ActivityReport struct {
	Schema        string       `json:"schema"`
	Catalog       string       `json:"catalog"`
	AccountID     string       `json:"account_id"`
	RealmID       string       `json:"realm_id"`
	AgentID       string       `json:"agent_id"`
	Since         time.Time    `json:"since"`
	Until         time.Time    `json:"until"`
	Bucket        string       `json:"bucket"`
	TrackingSince *time.Time   `json:"tracking_since"`
	Points        []UsagePoint `json:"points"`
	Totals        []UsageTotal `json:"totals"`
	Truncated     bool         `json:"truncated"`
}

func activityHash(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		_, _ = fmt.Fprintf(h, "%d:%s", len(p), p)
	}
	return hex.EncodeToString(h.Sum(nil))
}
func activityMarkerKey(p Principal) string {
	return "activity:" + activity.CatalogVersion + ":started:" + p.ID
}

// GetActivity is strictly read-only. The immutable marker uses the existing
// unique account/idempotency index; quantities use bounded indexed rollups.
func (s *Store) GetActivity(ctx context.Context, p Principal, q ActivityQuery) (ActivityReport, error) {
	if p.Kind != PrincipalAgent {
		return ActivityReport{}, ErrUsageForbidden
	}
	if q.Bucket != "" && q.Bucket != UsageBucketHour {
		return ActivityReport{}, ErrUsageInputInvalid
	}
	if q.Until.IsZero() {
		q.Until = time.Now().UTC()
	}
	if q.Since.IsZero() {
		q.Since = q.Until.UTC().Truncate(time.Hour).Add(-23 * time.Hour)
	}
	// Nine dimensions in hourly bins fit the fixed 10,000-point report ceiling.
	if q.Until.Sub(q.Since.UTC().Truncate(time.Hour)) > 31*24*time.Hour {
		return ActivityReport{}, ErrUsageInputInvalid
	}
	u, err := s.GetAgentUsage(ctx, p, UsageQuery{Since: q.Since, Until: q.Until, Bucket: UsageBucketHour, Dimensions: activity.Dimensions()})
	if err != nil {
		return ActivityReport{}, err
	}
	out := ActivityReport{Schema: activity.Schema, Catalog: activity.CatalogVersion, AccountID: p.AccountID, RealmID: p.RealmID, AgentID: p.ID, Since: u.Since, Until: u.Until, Bucket: u.Bucket, Points: u.Points, Totals: u.Totals}
	var started time.Time
	err = s.pool.QueryRow(ctx, `SELECT occurred_at FROM usage_events WHERE account_id=$1 AND idempotency_key=$2 AND realm_id=$3 AND agent_id=$4 AND dimension='activity_tracking_started' AND unit='activation' AND quantity=1`, p.AccountID, activityMarkerKey(p), p.RealmID, p.ID).Scan(&started)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return ActivityReport{}, err
	}
	if err == nil {
		out.TrackingSince = &started
	}
	for _, v := range out.Points {
		if activity.Unit(v.Dimension) != v.Unit || v.Quantity <= 0 || v.EventCount <= 0 {
			return ActivityReport{}, ErrUsageInputInvalid
		}
	}
	return out, nil
}

// beginActivityRead excludes nested helpers and preserves the original scope
// for completion. Completion failures replace success before the API responds.
func (s *Store) beginActivityRead(ctx context.Context, p Principal, operation string) (context.Context, func(int64, *error)) {
	meterCtx := activity.DefaultOperation(ctx, operation)
	return activity.WithOperation(meterCtx, ""), func(records int64, resultErr *error) {
		if *resultErr != nil || !activityEligible(meterCtx, p, operation, false) {
			return
		}
		tx, err := s.pool.Begin(meterCtx)
		if err != nil {
			*resultErr = err
			return
		}
		defer func() { _ = tx.Rollback(meterCtx) }()
		if err = verifyLiveAgentScope(meterCtx, tx, p.AccountID, p.RealmID, p.ID); err == nil {
			err = recordActivityOperationTx(meterCtx, tx, p, operation, activity.RequestID(meterCtx), records)
		}
		if err == nil {
			err = tx.Commit(meterCtx)
		}
		*resultErr = err
	}
}
func activityEligible(ctx context.Context, p Principal, operation string, write bool) bool {
	if p.Kind != PrincipalAgent || activity.IsObservation(ctx) {
		return false
	}
	d, ok := activity.Lookup(operation)
	if !ok || d.Write != write {
		return false
	}
	selected, present := activity.Operation(ctx)
	if present && selected != operation {
		return false
	}
	return write || activity.ValidRequestID(activity.RequestID(ctx))
}

func recordActivityOperationTx(ctx context.Context, tx pgx.Tx, p Principal, operation, effect string, records int64) error {
	d, ok := activity.Lookup(operation)
	if !ok || !activityEligible(ctx, p, operation, d.Write) {
		return nil
	}
	if effect == "" || records < 0 {
		return ErrUsageInputInvalid
	}
	dimension := "operation_read"
	if d.Write {
		dimension = "operation_write"
	}
	// Keys include canonical scope and the fixed operation. No caller values or
	// payload-derived data are retained, including externally chosen domain keys.
	key := "activity:" + activity.CatalogVersion + ":" + activityHash(p.RealmID, p.ID, operation, effect)
	metadata, err := json.Marshal(map[string]string{"category": d.Category, "action": d.Action})
	if err != nil {
		return err
	}
	now, err := ensureActivityMarkerTx(ctx, tx, p)
	if err != nil {
		return err
	}
	in := usageEventInput{AccountID: p.AccountID, RealmID: p.RealmID, AgentID: p.ID, Dimension: dimension, Unit: "operation", Quantity: 1, SubjectType: "activity", SubjectID: activityHash(effect), IdempotencyKey: key, Metadata: metadata, OccurredAt: now}
	inserted, err := recordUsageEventTx(ctx, tx, in)
	if err != nil || !inserted || records == 0 {
		return err
	}
	in.Dimension += "_record"
	in.Unit = "record"
	in.Quantity = records
	in.IdempotencyKey += ":records"
	_, err = recordUsageEventTx(ctx, tx, in)
	return err
}
func ensureActivityMarkerTx(ctx context.Context, tx pgx.Tx, p Principal) (time.Time, error) {
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return now, err
	}
	_, err := recordUsageEventTx(ctx, tx, usageEventInput{AccountID: p.AccountID, RealmID: p.RealmID, AgentID: p.ID, Dimension: "activity_tracking_started", Unit: "activation", Quantity: 1, SubjectType: "activity", SubjectID: p.ID, IdempotencyKey: activityMarkerKey(p), Metadata: json.RawMessage(`{"catalog":"core-records.v1"}`), OccurredAt: now})
	if err != nil {
		return now, err
	}
	// A competing first writer may have persisted a later timestamp while this
	// transaction waited at the unique index. Read that immutable marker after
	// conflict resolution so no operation/change or companion row predates it.
	err = tx.QueryRow(ctx, `SELECT GREATEST(clock_timestamp(), occurred_at) FROM usage_events WHERE account_id=$1 AND idempotency_key=$2`, p.AccountID, activityMarkerKey(p)).Scan(&now)
	return now, err
}
func recordMemoryActivityTx(ctx context.Context, tx pgx.Tx, p Principal, dimension, effect string) error {
	if activity.IsObservation(ctx) || p.Kind != PrincipalAgent {
		return nil
	}
	if activity.Unit(dimension) != "change" {
		return ErrUsageInputInvalid
	}
	now, err := ensureActivityMarkerTx(ctx, tx, p)
	if err != nil {
		return err
	}
	_, err = recordUsageEventTx(ctx, tx, usageEventInput{AccountID: p.AccountID, RealmID: p.RealmID, AgentID: p.ID, Dimension: dimension, Unit: "change", Quantity: 1, SubjectType: "activity", SubjectID: activityHash(effect), IdempotencyKey: "activity:change:" + activityHash(p.RealmID, p.ID, dimension, effect), Metadata: json.RawMessage(`{"category":"memories"}`), OccurredAt: now})
	return err
}
func recordMemoryVersionActivityTx(ctx context.Context, tx pgx.Tx, m Memory) error {
	dimension := "memory_revised"
	switch {
	case m.Operation == "restored" || m.Operation == "reactivated" || m.Operation == "reverted" && m.State == MemoryStateActive:
		dimension = "memory_restored"
	case m.Operation == "forgotten" || m.State == MemoryStateReverted:
		dimension = "memory_archived"
	case m.Version == 1:
		dimension = "memory_created"
	}
	return recordMemoryActivityTx(ctx, tx, Principal{Kind: PrincipalAgent, AccountID: m.AccountID, RealmID: m.RealmID, ID: m.OwnerID}, dimension, fmt.Sprintf("%s:%d", m.ID, m.Version))
}
