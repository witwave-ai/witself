package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
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
	u, err := s.GetAgentUsage(ctx, p, UsageQuery{Since: q.Since, Until: q.Until, Bucket: UsageBucketHour, Dimensions: activity.Dimensions(), activityOnly: true})
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

// A marker is cached only after commit, never after an attempted or rolled-back
// insertion. Store-local scope keeps separate databases and test schemas apart.
type activityReadMarker struct {
	initializing chan struct{}
	known        atomic.Bool
	since        time.Time
}
type activityReadMarkerKey struct{}

// ActivityMeteringFailures is a process-local, value-free count of failed read
// metering transactions. Domain writes still meter atomically or fail.
func (s *Store) ActivityMeteringFailures() uint64 { return s.activityMeteringFailures.Load() }

// One limiter across all Stores prevents a database outage from logging per read.
var activityReadWarnings activityReadWarningLimiter

type activityReadWarningLimiter struct {
	mu   sync.Mutex
	last time.Time
}

func (l *activityReadWarningLimiter) allow(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.last.IsZero() && now.Sub(l.last) < time.Minute {
		return false
	}
	l.last = now
	return true
}

func (l *activityReadWarningLimiter) warn(now time.Time, operation string, err error) {
	if l.allow(now) {
		fmt.Fprintf(os.Stderr, "witself: activity read metering failed: operation=%s: %v\n", operation, err)
	}
}

// beginActivityRead excludes nested helpers. Metering failure never replaces a
// successful domain read. Warnings include the cause and are process-rate-limited.
func (s *Store) beginActivityRead(ctx context.Context, p Principal, operation string) (context.Context, func(int64, *error)) {
	meterCtx := activity.DefaultOperation(ctx, operation)
	return activity.WithOperation(meterCtx, ""), func(records int64, resultErr *error) {
		if *resultErr != nil || !activityEligible(meterCtx, p, operation, false) {
			return
		}
		if err := s.recordActivityRead(meterCtx, p, operation, records); err != nil {
			s.activityMeteringFailures.Add(1)
			activityReadWarnings.warn(time.Now(), operation, err)
		}
	}
}
func (s *Store) recordActivityRead(ctx context.Context, p Principal, operation string, records int64) error {
	key := [3]string{p.AccountID, p.RealmID, p.ID}
	v, ok := s.activityMarkers.Load(key)
	if !ok {
		v, _ = s.activityMarkers.LoadOrStore(key, &activityReadMarker{initializing: make(chan struct{}, 1)})
	}
	marker := v.(*activityReadMarker)
	// Serialize only first activation, not steady-state per-agent reads.
	if !marker.known.Load() {
		select {
		case marker.initializing <- struct{}{}:
			defer func() { <-marker.initializing }()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// The domain read already checked access. No redundant scope query here.
	ctx = context.WithValue(ctx, activityReadMarkerKey{}, marker)
	if err := recordActivityOperationTx(ctx, tx, p, operation, activity.RequestID(ctx), records); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	marker.known.Store(true)
	return nil
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
	now, err := activityEventTimeTx(ctx, tx, p)
	if err != nil {
		return err
	}
	in := usageEventInput{AccountID: p.AccountID, RealmID: p.RealmID, AgentID: p.ID, Dimension: dimension, Unit: "operation", Quantity: 1, SubjectType: "activity", SubjectID: activityHash(effect), IdempotencyKey: key, Metadata: metadata, OccurredAt: now}
	inserted, err := recordUsageEventTx(ctx, tx, in)
	if err != nil || !inserted || records == 0 {
		return err
	}
	// Separate operation/record quantities and event counts are part of the
	// summary contract. One event has one dimension/unit; folding these rows
	// requires a schema and archive contract change, not just a write shortcut.
	in.Dimension += "_record"
	in.Unit = "record"
	in.Quantity = records
	in.IdempotencyKey += ":records"
	_, err = recordUsageEventTx(ctx, tx, in)
	return err
}

// activityEventTimeTx avoids even calling marker initialization on cached
// reads. Writes keep initialization inside their domain transaction.
func activityEventTimeTx(ctx context.Context, tx pgx.Tx, p Principal) (time.Time, error) {
	marker, _ := ctx.Value(activityReadMarkerKey{}).(*activityReadMarker)
	if marker != nil && marker.known.Load() {
		var now time.Time
		err := tx.QueryRow(ctx, `SELECT GREATEST(clock_timestamp(), $1::timestamptz)`, marker.since).Scan(&now)
		return now, err
	}
	now, err := ensureActivityMarkerTx(ctx, tx, p)
	if err == nil && marker != nil {
		marker.since = now
	}
	return now, err
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
	now, err := activityEventTimeTx(ctx, tx, p)
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

// RetireActivityEvents deletes at most limit old nonbilling activity events.
// The caller is the existing rate-bucket maintenance loop; concurrent replicas
// divide work via SKIP LOCKED. Rollups and tracking markers remain authoritative.
func (s *Store) RetireActivityEvents(ctx context.Context, now time.Time, limit int) (int64, error) {
	if limit < 1 || limit > 1000 || now.IsZero() {
		return 0, ErrUsageInputInvalid
	}
	tag, err := s.pool.Exec(ctx, `WITH retired AS (
 SELECT id FROM usage_events
 WHERE dimension IN ('operation_read','operation_read_record','operation_write','operation_write_record',
 'memory_created','memory_revised','memory_archived','memory_restored','memory_deleted')
 AND occurred_at < $1
 ORDER BY occurred_at, id LIMIT $2 FOR UPDATE SKIP LOCKED
 ) DELETE FROM usage_events u USING retired r WHERE u.id=r.id`, now.UTC().Add(-35*24*time.Hour), limit)
	return tag.RowsAffected(), err
}
