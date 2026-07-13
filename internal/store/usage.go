package store

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/witwave-ai/witself/internal/id"
)

// Initial product-usage dimensions. Additional features can add dimensions
// without changing the ledger schema.
const (
	UsageDimensionTranscriptCreated    = "transcript_created"
	UsageDimensionTranscriptEntryWrite = "transcript_entry_write"
	UsageDimensionTranscriptEntryRead  = "transcript_entry_read"
	UsageDimensionTranscriptStorage    = "transcript_storage_byte"

	UsageBucketHour = "hour"
	UsageBucketDay  = "day"

	UsageUnitTranscript = "transcript"
	UsageUnitEntry      = "entry"
	UsageUnitByte       = "byte"
)

var (
	usageDimensionPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	usageShortNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
)

var (
	// ErrUsageForbidden reports a non-agent attempt to read per-agent usage.
	ErrUsageForbidden = errors.New("usage access forbidden")
	// ErrUsageInputInvalid reports an invalid usage query.
	ErrUsageInputInvalid = errors.New("invalid usage query")
)

// UsageQuery selects transactionally maintained per-agent rollups.
type UsageQuery struct {
	Since      time.Time
	Until      time.Time
	Bucket     string
	Dimensions []string
}

// UsagePoint is one dimension total in an hourly or daily UTC bucket.
type UsagePoint struct {
	Dimension   string    `json:"dimension"`
	Unit        string    `json:"unit"`
	BucketStart time.Time `json:"bucket_start"`
	Quantity    int64     `json:"quantity"`
	EventCount  int64     `json:"event_count"`
}

// UsageTotal is a dimension total across the report window.
type UsageTotal struct {
	Dimension  string `json:"dimension"`
	Unit       string `json:"unit"`
	Quantity   int64  `json:"quantity"`
	EventCount int64  `json:"event_count"`
}

// UsageReport is the bounded, token-scoped usage view returned to an agent.
type UsageReport struct {
	AccountID string       `json:"account_id"`
	RealmID   string       `json:"realm_id"`
	RealmName string       `json:"realm_name,omitempty"`
	AgentID   string       `json:"agent_id"`
	AgentName string       `json:"agent_name,omitempty"`
	Since     time.Time    `json:"since"`
	Until     time.Time    `json:"until"`
	Bucket    string       `json:"bucket"`
	Points    []UsagePoint `json:"points"`
	Totals    []UsageTotal `json:"totals"`
}

type usageEventInput struct {
	AccountID      string
	RealmID        string
	AgentID        string
	Dimension      string
	Quantity       int64
	Unit           string
	SubjectType    string
	SubjectID      string
	IdempotencyKey string
	Metadata       json.RawMessage
	OccurredAt     time.Time
}

// GetAgentUsage reads only the authenticated agent's own rollups. Account and
// realm ids are retained on every row so the data moves with account archives.
func (s *Store) GetAgentUsage(ctx context.Context, p Principal, query UsageQuery) (UsageReport, error) {
	if p.Kind != PrincipalAgent {
		return UsageReport{}, ErrUsageForbidden
	}
	query, err := normalizeUsageQuery(query)
	if err != nil {
		return UsageReport{}, err
	}

	args := []any{p.AccountID, p.RealmID, p.ID, query.Bucket, query.Since, query.Until}
	sql := `
		SELECT dimension, unit, bucket_start, quantity, event_count
		FROM usage_rollups
		WHERE account_id = $1 AND realm_id = $2 AND agent_id = $3
		  AND bucket = $4 AND bucket_start >= $5 AND bucket_start < $6`
	if len(query.Dimensions) > 0 {
		sql += ` AND dimension = ANY($7)`
		args = append(args, query.Dimensions)
	}
	sql += ` ORDER BY bucket_start, dimension, unit`

	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return UsageReport{}, fmt.Errorf("query usage rollups: %w", err)
	}
	defer rows.Close()

	report := UsageReport{
		AccountID: p.AccountID, RealmID: p.RealmID, RealmName: p.RealmName,
		AgentID: p.ID, AgentName: p.AgentName,
		Since: query.Since, Until: query.Until, Bucket: query.Bucket,
		Points: []UsagePoint{}, Totals: []UsageTotal{},
	}
	totals := map[string]*UsageTotal{}
	for rows.Next() {
		var point UsagePoint
		if err := rows.Scan(&point.Dimension, &point.Unit, &point.BucketStart, &point.Quantity, &point.EventCount); err != nil {
			return UsageReport{}, err
		}
		report.Points = append(report.Points, point)
		key := point.Dimension + "\x00" + point.Unit
		total := totals[key]
		if total == nil {
			total = &UsageTotal{Dimension: point.Dimension, Unit: point.Unit}
			totals[key] = total
		}
		total.Quantity += point.Quantity
		total.EventCount += point.EventCount
	}
	if err := rows.Err(); err != nil {
		return UsageReport{}, err
	}
	keys := make([]string, 0, len(totals))
	for key := range totals {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		report.Totals = append(report.Totals, *totals[key])
	}
	return report, nil
}

func normalizeUsageQuery(query UsageQuery) (UsageQuery, error) {
	query.Bucket = normalizeUsageBucket(query.Bucket)
	if query.Bucket == "" {
		return UsageQuery{}, fmt.Errorf("%w: bucket must be hour or day", ErrUsageInputInvalid)
	}
	if query.Until.IsZero() {
		query.Until = time.Now().UTC()
	} else {
		query.Until = query.Until.UTC()
	}
	if query.Since.IsZero() {
		query.Since = query.Until.Add(-30 * 24 * time.Hour)
	} else {
		query.Since = query.Since.UTC()
	}
	query.Since = usageBucketStart(query.Since, query.Bucket)
	if !query.Since.Before(query.Until) {
		return UsageQuery{}, fmt.Errorf("%w: since must be before until", ErrUsageInputInvalid)
	}
	if query.Bucket == UsageBucketHour && query.Until.Sub(query.Since) > 90*24*time.Hour {
		return UsageQuery{}, fmt.Errorf("%w: hourly reports are limited to 90 days", ErrUsageInputInvalid)
	}
	if query.Bucket == UsageBucketDay && query.Until.Sub(query.Since) > 5*366*24*time.Hour {
		return UsageQuery{}, fmt.Errorf("%w: daily reports are limited to 5 years", ErrUsageInputInvalid)
	}
	seen := map[string]bool{}
	dimensions := make([]string, 0, len(query.Dimensions))
	for _, dimension := range query.Dimensions {
		if !usageDimensionPattern.MatchString(dimension) {
			return UsageQuery{}, fmt.Errorf("%w: invalid dimension %q", ErrUsageInputInvalid, dimension)
		}
		if !seen[dimension] {
			dimensions = append(dimensions, dimension)
			seen[dimension] = true
		}
	}
	sort.Strings(dimensions)
	query.Dimensions = dimensions
	return query, nil
}

func normalizeUsageBucket(bucket string) string {
	switch bucket {
	case "", UsageBucketDay:
		return UsageBucketDay
	case UsageBucketHour:
		return UsageBucketHour
	default:
		return ""
	}
}

func usageBucketStart(t time.Time, bucket string) time.Time {
	t = t.UTC()
	if bucket == UsageBucketHour {
		return t.Truncate(time.Hour)
	}
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func usageBatchKey(ids []string) string {
	ordered := append([]string(nil), ids...)
	sort.Strings(ordered)
	sum := sha256.Sum256([]byte(strings.Join(ordered, "\x00")))
	return fmt.Sprintf("%x", sum[:])
}

func (s *Store) recordUsage(ctx context.Context, in usageEventInput) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := verifyLiveAgentScope(ctx, tx, in.AccountID, in.RealmID, in.AgentID); err != nil {
		return err
	}
	if _, err := recordUsageEventTx(ctx, tx, in); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// recordUsageEventTx inserts an idempotent fact and updates both projections
// in the caller's transaction. It returns false when the key already exists.
func recordUsageEventTx(ctx context.Context, tx pgx.Tx, in usageEventInput) (bool, error) {
	if err := validateUsageEventInput(&in); err != nil {
		return false, err
	}
	eventID, err := id.New("usg")
	if err != nil {
		return false, err
	}
	var insertedID string
	err = tx.QueryRow(ctx, `
		INSERT INTO usage_events
		  (id, account_id, realm_id, agent_id, dimension, quantity, unit,
		   subject_type, subject_id, idempotency_key, metadata, occurred_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11::jsonb, $12)
		ON CONFLICT (account_id, idempotency_key) DO NOTHING
		RETURNING id`, eventID, in.AccountID, in.RealmID, in.AgentID,
		in.Dimension, in.Quantity, in.Unit, in.SubjectType, in.SubjectID,
		in.IdempotencyKey, string(in.Metadata), in.OccurredAt).Scan(&insertedID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("record usage event: %w", err)
	}

	for _, bucket := range []string{UsageBucketHour, UsageBucketDay} {
		bucketStart := usageBucketStart(in.OccurredAt, bucket)
		if _, err := tx.Exec(ctx, `
			INSERT INTO usage_rollups
			  (account_id, realm_id, agent_id, dimension, unit, bucket,
			   bucket_start, quantity, event_count)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 1)
			ON CONFLICT (agent_id, dimension, unit, bucket, bucket_start)
			DO UPDATE SET
			  quantity = usage_rollups.quantity + EXCLUDED.quantity,
			  event_count = usage_rollups.event_count + 1,
			  updated_at = now()`,
			in.AccountID, in.RealmID, in.AgentID, in.Dimension, in.Unit,
			bucket, bucketStart, in.Quantity); err != nil {
			return false, fmt.Errorf("update usage %s rollup: %w", bucket, err)
		}
	}
	return true, nil
}

func validateUsageEventInput(in *usageEventInput) error {
	if in.AccountID == "" || in.RealmID == "" || in.AgentID == "" {
		return fmt.Errorf("usage event scope is required")
	}
	if !usageDimensionPattern.MatchString(in.Dimension) || !usageShortNamePattern.MatchString(in.Unit) ||
		!usageShortNamePattern.MatchString(in.SubjectType) {
		return fmt.Errorf("invalid usage event dimension, unit, or subject type")
	}
	if in.Quantity <= 0 || len(in.SubjectID) == 0 || len(in.SubjectID) > 256 ||
		len(in.IdempotencyKey) == 0 || len(in.IdempotencyKey) > 512 {
		return fmt.Errorf("invalid usage event quantity, subject, or idempotency key")
	}
	if in.OccurredAt.IsZero() {
		in.OccurredAt = time.Now().UTC()
	} else {
		in.OccurredAt = in.OccurredAt.UTC()
	}
	if len(in.Metadata) == 0 {
		in.Metadata = json.RawMessage(`{}`)
	}
	var metadata map[string]any
	if len(in.Metadata) > 4096 || json.Unmarshal(in.Metadata, &metadata) != nil || metadata == nil {
		return fmt.Errorf("usage event metadata must be a JSON object no larger than 4096 bytes")
	}
	return nil
}
