package store

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const maxMemoryRecallPageSize = 100

// MemoryRecallOptions is a structured, deterministic retrieval request. The
// backend does not interpret natural-language dates or invoke a model; callers
// resolve those into these filters before calling it.
type MemoryRecallOptions struct {
	Query            string
	Kind             string
	Tags             []string
	Links            []string
	Origin           string
	CaptureReason    string
	IncludeSensitive bool
	OccurredFrom     *time.Time
	OccurredUntil    *time.Time
	CapturedFrom     *time.Time
	CapturedUntil    *time.Time
	Limit            int
	Cursor           string
	VectorProfileID  string
	QueryVector      []float64

	// Internal deterministic page keys. Public callers use Cursor.
	AsOf                       *time.Time
	SnapshotChangeSeq          *int64
	SnapshotDeletedMemoryCount *int64
	AfterScore                 *float64
	AfterUpdatedAt             *time.Time
	AfterID                    string
}

// MemoryRecallScore exposes every signal used by the first lexical ranker.
// Total = 0.60*Lexical + 0.25*Salience + 0.15*Recency.
type MemoryRecallScore struct {
	Similarity float64 `json:"similarity"`
	VectorUsed bool    `json:"vector_used"`
	Lexical    float64 `json:"lexical"`
	Salience   float64 `json:"salience"`
	Recency    float64 `json:"recency"`
	Total      float64 `json:"total"`
}

// MemoryRecallHit pairs one recalled memory with its deterministic score.
type MemoryRecallHit struct {
	Memory Memory            `json:"memory"`
	Score  MemoryRecallScore `json:"score"`
}

// MemoryRecallPage contains a stable page of ranked memories and recall metadata.
type MemoryRecallPage struct {
	Hits             []MemoryRecallHit `json:"hits"`
	NextCursor       string            `json:"next_cursor,omitempty"`
	RetrievalMode    string            `json:"retrieval_mode"`
	VectorCoverage   float64           `json:"vector_coverage"`
	VectorProfileID  string            `json:"vector_profile_id,omitempty"`
	VectorCandidates int               `json:"vector_candidates,omitempty"`
	VectorMatches    int               `json:"vector_matches,omitempty"`
	// CandidateTruncated means this cursor traverses only the deterministic
	// first CandidateLimit candidates selected at its pinned snapshot. Later
	// pages reconstruct that same bounded universe; they never widen it.
	CandidateTruncated bool   `json:"candidate_truncated,omitempty"`
	CandidateLimit     int    `json:"candidate_limit,omitempty"`
	Degraded           bool   `json:"degraded"`
	DegradedReason     string `json:"degraded_reason,omitempty"`
}

type memoryRecallCursor struct {
	Version                    int       `json:"v"`
	QueryHash                  string    `json:"query_hash"`
	AsOf                       time.Time `json:"as_of"`
	SnapshotChangeSeq          int64     `json:"snapshot_change_seq"`
	SnapshotDeletedMemoryCount int64     `json:"snapshot_deleted_memory_count"`
	AfterScore                 float64   `json:"after_score"`
	UpdatedAt                  time.Time `json:"updated_at"`
	MemoryID                   string    `json:"memory_id"`
}

type memoryRecallCursorBinding struct {
	Query            string     `json:"query"`
	Kind             string     `json:"kind,omitempty"`
	Tags             []string   `json:"tags,omitempty"`
	Links            []string   `json:"links,omitempty"`
	Origin           string     `json:"origin,omitempty"`
	CaptureReason    string     `json:"capture_reason,omitempty"`
	IncludeSensitive bool       `json:"include_sensitive"`
	OccurredFrom     *time.Time `json:"occurred_from,omitempty"`
	OccurredUntil    *time.Time `json:"occurred_until,omitempty"`
	CapturedFrom     *time.Time `json:"captured_from,omitempty"`
	CapturedUntil    *time.Time `json:"captured_until,omitempty"`
	VectorProfileID  string     `json:"vector_profile_id,omitempty"`
	QueryVectorHash  string     `json:"query_vector_hash,omitempty"`
}

// RecallMemories returns active heads from one committed owner-lane watermark,
// ranked by deterministic lexical, salience, and recency signals. Later pages
// reconstruct those heads from immutable versions; no model or embedding
// provider is involved.
func (s *Store) RecallMemories(ctx context.Context, p Principal, opts MemoryRecallOptions) (MemoryRecallPage, error) {
	if p.Kind != PrincipalAgent {
		return MemoryRecallPage{}, ErrMemoryForbidden
	}
	var err error
	opts, err = normalizeMemoryRecallOptions(opts)
	if err != nil {
		return MemoryRecallPage{}, err
	}
	bindingHash, err := memoryRecallBindingHash(opts)
	if err != nil {
		return MemoryRecallPage{}, err
	}
	if opts.Cursor != "" {
		cursor, err := decodeMemoryRecallCursor(opts.Cursor)
		if err != nil {
			return MemoryRecallPage{}, err
		}
		if cursor.QueryHash != bindingHash {
			return MemoryRecallPage{}, fmt.Errorf("%w: recall cursor does not match filters", ErrMemoryInputInvalid)
		}
		opts.AsOf = &cursor.AsOf
		opts.SnapshotChangeSeq = &cursor.SnapshotChangeSeq
		opts.SnapshotDeletedMemoryCount = &cursor.SnapshotDeletedMemoryCount
		opts.AfterScore = &cursor.AfterScore
		opts.AfterUpdatedAt = &cursor.UpdatedAt
		opts.AfterID = cursor.MemoryID
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return MemoryRecallPage{}, fmt.Errorf("begin memory recall snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := verifyLiveAgentScope(ctx, tx, p.AccountID, p.RealmID, p.ID); err != nil {
		return MemoryRecallPage{}, err
	}
	var databaseAsOf time.Time
	var currentChangeSeq int64
	var currentDeletedMemoryCount int64
	if err := tx.QueryRow(ctx, `
		SELECT clock_timestamp(), COALESCE((
		  SELECT last_change_seq FROM memory_change_clocks
		  WHERE account_id=$1 AND realm_id=$2
		    AND owner_kind='agent' AND owner_id=$3
		),0), (SELECT count(*) FROM memories
		  WHERE account_id=$1 AND realm_id=$2
		    AND owner_kind='agent' AND owner_id=$3
		    AND current_version IS NULL)`, p.AccountID, p.RealmID, p.ID).
		Scan(&databaseAsOf, &currentChangeSeq, &currentDeletedMemoryCount); err != nil {
		return MemoryRecallPage{}, fmt.Errorf("read memory recall snapshot watermark: %w", err)
	}
	if opts.AsOf == nil {
		opts.AsOf = &databaseAsOf
		opts.SnapshotChangeSeq = &currentChangeSeq
		opts.SnapshotDeletedMemoryCount = &currentDeletedMemoryCount
	} else if opts.SnapshotChangeSeq == nil || *opts.SnapshotChangeSeq < 0 ||
		opts.SnapshotDeletedMemoryCount == nil || *opts.SnapshotDeletedMemoryCount < 0 ||
		*opts.SnapshotChangeSeq > currentChangeSeq || opts.AsOf.After(databaseAsOf) {
		return MemoryRecallPage{}, fmt.Errorf("%w: recall cursor snapshot is invalid", ErrMemoryInputInvalid)
	}
	// Permanent deletion intentionally purges the immutable versions needed to
	// reconstruct an older page. Tombstones are monotonic within an owner lane;
	// a changed count therefore fails closed instead of shrinking a traversal.
	if opts.Cursor != "" && *opts.SnapshotDeletedMemoryCount != currentDeletedMemoryCount {
		return MemoryRecallPage{}, ErrMemoryDeleted
	}
	if opts.VectorProfileID != "" {
		page, err := recallMemoriesHybrid(ctx, tx, p, opts, bindingHash)
		if err != nil {
			return MemoryRecallPage{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return MemoryRecallPage{}, fmt.Errorf("commit hybrid memory recall snapshot: %w", err)
		}
		return page, nil
	}

	args := []any{p.AccountID, p.RealmID, p.ID, opts.Query, *opts.AsOf, *opts.SnapshotChangeSeq}
	where := []string{
		"h.state='active'",
		"(q.query IS NULL OR h.search_document @@ q.query)",
	}
	addArg := func(value any) string {
		args = append(args, value)
		return fmt.Sprintf("$%d", len(args))
	}
	if opts.Kind != "" {
		where = append(where, "h.kind="+addArg(opts.Kind))
	}
	if opts.Origin != "" {
		where = append(where, "h.origin="+addArg(opts.Origin))
	}
	if opts.CaptureReason != "" {
		where = append(where, "h.capture_reason="+addArg(opts.CaptureReason))
	}
	if len(opts.Tags) > 0 {
		raw, _ := json.Marshal(opts.Tags)
		where = append(where, "h.tags @> "+addArg(string(raw))+"::jsonb")
	}
	if len(opts.Links) > 0 {
		raw, _ := json.Marshal(opts.Links)
		where = append(where, "h.links @> "+addArg(string(raw))+"::jsonb")
	}
	if opts.OccurredFrom != nil {
		where = append(where, "COALESCE(h.occurred_until,h.occurred_from) >= "+addArg(*opts.OccurredFrom))
	}
	if opts.OccurredUntil != nil {
		where = append(where, "COALESCE(h.occurred_from,h.occurred_until) <= "+addArg(*opts.OccurredUntil))
	}
	if opts.CapturedFrom != nil {
		where = append(where, "h.memory_created_at >= "+addArg(*opts.CapturedFrom))
	}
	if opts.CapturedUntil != nil {
		where = append(where, "h.memory_created_at <= "+addArg(*opts.CapturedUntil))
	}
	afterClause := ""
	if opts.AfterScore != nil {
		scoreArg := addArg(*opts.AfterScore)
		updatedArg := addArg(*opts.AfterUpdatedAt)
		idArg := addArg(opts.AfterID)
		afterClause = fmt.Sprintf("WHERE (total_score, updated_at, memory_id) < (%s,%s,%s)", scoreArg, updatedArg, idArg)
	}
	limitArg := addArg(opts.Limit + 1)
	rows, err := tx.Query(ctx, `
		WITH q AS (
		  SELECT CASE WHEN $4='' THEN NULL
		              ELSE websearch_to_tsquery('simple'::regconfig,$4) END AS query
		), as_of_versions AS (
		  SELECT DISTINCT ON (v.memory_id)
		    m.id AS memory_id, m.origin, m.capture_reason,
		    m.created_at AS memory_created_at, v.version, v.change_seq,
		    v.search_document, v.kind, v.tags, v.links, v.salience,
		    v.occurred_from, v.occurred_until, v.state,
		    v.created_at AS version_updated_at
		  FROM memories m
		  JOIN memory_versions v ON v.memory_id=m.id
		  WHERE m.account_id=$1 AND m.realm_id=$2
		    AND m.owner_kind='agent' AND m.owner_id=$3
		    AND v.account_id=$1 AND v.realm_id=$2
		    AND v.owner_kind='agent' AND v.owner_id=$3
		    AND v.change_seq <= $6
		  ORDER BY v.memory_id, v.change_seq DESC
		), evidence_activity AS (
		  SELECT e.memory_id, max(e.created_at) AS updated_at
		  FROM memory_evidence e
		  WHERE e.account_id=$1 AND e.realm_id=$2
		    AND e.owner_kind='agent' AND e.owner_id=$3
		    AND e.evidence_change_seq <= $6
		    AND e.pending_evidence_id IS NOT NULL
		  GROUP BY e.memory_id
		), as_of_heads AS (
		  SELECT v.*,
		    GREATEST(v.version_updated_at,
		             COALESCE(e.updated_at,v.version_updated_at)) AS updated_at
		  FROM as_of_versions v
		  LEFT JOIN evidence_activity e ON e.memory_id=v.memory_id
		), signals AS (
		  SELECT h.memory_id, h.version, h.updated_at,
		    CASE WHEN q.query IS NULL THEN 0::double precision
		         ELSE ts_rank_cd(h.search_document,q.query,32)::double precision END AS lexical_score,
		    h.salience AS salience_score,
		    (1.0 / (1.0 + GREATEST(EXTRACT(EPOCH FROM ($5::timestamptz-h.updated_at)),0.0) / 2592000.0))::double precision AS recency_score
		  FROM as_of_heads h
		  CROSS JOIN q
		  WHERE `+strings.Join(where, " AND ")+`
		), ranked AS (
		  SELECT *, (0.60*lexical_score + 0.25*salience_score + 0.15*recency_score)::double precision AS total_score
		  FROM signals
		)
		SELECT memory_id, version, lexical_score, salience_score, recency_score, total_score, updated_at
		FROM ranked `+afterClause+`
		ORDER BY total_score DESC, updated_at DESC, memory_id DESC
		LIMIT `+limitArg, args...)
	if err != nil {
		return MemoryRecallPage{}, fmt.Errorf("recall memories: %w", err)
	}
	type rankedMemory struct {
		id        string
		version   int64
		score     MemoryRecallScore
		updatedAt time.Time
	}
	ranked := make([]rankedMemory, 0, opts.Limit+1)
	for rows.Next() {
		var item rankedMemory
		if err := rows.Scan(&item.id, &item.version, &item.score.Lexical, &item.score.Salience,
			&item.score.Recency, &item.score.Total, &item.updatedAt); err != nil {
			rows.Close()
			return MemoryRecallPage{}, err
		}
		ranked = append(ranked, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return MemoryRecallPage{}, fmt.Errorf("recall memories: %w", err)
	}
	rows.Close()

	page := MemoryRecallPage{
		Hits: []MemoryRecallHit{}, RetrievalMode: "lexical",
		VectorCoverage: 0, Degraded: false,
	}
	if len(ranked) > opts.Limit {
		ranked = ranked[:opts.Limit]
		last := ranked[len(ranked)-1]
		page.NextCursor, err = encodeMemoryRecallCursor(memoryRecallCursor{
			Version: 2, QueryHash: bindingHash, AsOf: *opts.AsOf,
			SnapshotChangeSeq:          *opts.SnapshotChangeSeq,
			SnapshotDeletedMemoryCount: *opts.SnapshotDeletedMemoryCount,
			AfterScore:                 last.score.Total, UpdatedAt: last.updatedAt, MemoryID: last.id,
		})
		if err != nil {
			return MemoryRecallPage{}, err
		}
	}
	if len(ranked) == 0 {
		if err := tx.Commit(ctx); err != nil {
			return MemoryRecallPage{}, fmt.Errorf("commit memory recall snapshot: %w", err)
		}
		return page, nil
	}
	ids := make([]string, len(ranked))
	versions := make([]int64, len(ranked))
	for i := range ranked {
		ids[i] = ranked[i].id
		versions[i] = ranked[i].version
	}
	payloadRows, err := tx.Query(ctx, memorySelectSQL+`
		JOIN unnest($1::text[],$2::bigint[]) AS selected(memory_id,memory_version)
		  ON selected.memory_id=m.id AND selected.memory_version=v.version
		WHERE m.account_id=$3 AND m.realm_id=$4
		  AND m.owner_kind='agent' AND m.owner_id=$5`,
		ids, versions, p.AccountID, p.RealmID, p.ID)
	if err != nil {
		return MemoryRecallPage{}, fmt.Errorf("load recalled memories: %w", err)
	}
	memories := make(map[string]Memory, len(ids))
	for payloadRows.Next() {
		memory, err := scanMemory(payloadRows)
		if err != nil {
			payloadRows.Close()
			return MemoryRecallPage{}, err
		}
		if memory.Sensitive && !opts.IncludeSensitive {
			redactMemoryForBroadRead(&memory)
		}
		memory.Evidence = nil
		memories[memory.ID] = memory
	}
	if err := payloadRows.Err(); err != nil {
		payloadRows.Close()
		return MemoryRecallPage{}, fmt.Errorf("load recalled memories: %w", err)
	}
	payloadRows.Close()
	page.Hits = make([]MemoryRecallHit, 0, len(ranked))
	for _, item := range ranked {
		memory, ok := memories[item.id]
		if !ok {
			return MemoryRecallPage{}, fmt.Errorf("recall snapshot lost memory version %s/%d", item.id, item.version)
		}
		memory.UpdatedAt = item.updatedAt
		page.Hits = append(page.Hits, MemoryRecallHit{Memory: memory, Score: item.score})
	}
	if err := tx.Commit(ctx); err != nil {
		return MemoryRecallPage{}, fmt.Errorf("commit memory recall snapshot: %w", err)
	}
	return page, nil
}

func normalizeMemoryRecallOptions(opts MemoryRecallOptions) (MemoryRecallOptions, error) {
	opts.Query = strings.TrimSpace(opts.Query)
	opts.VectorProfileID = strings.TrimSpace(opts.VectorProfileID)
	if len(opts.Query) > 4096 {
		return MemoryRecallOptions{}, fmt.Errorf("%w: recall query is too large", ErrMemoryInputInvalid)
	}
	if (opts.VectorProfileID == "") != (len(opts.QueryVector) == 0) {
		return MemoryRecallOptions{}, fmt.Errorf("%w: vector_profile_id and query_vector must be supplied together", ErrMemoryInputInvalid)
	}
	if len(opts.QueryVector) > maxMemoryVectorDimensions {
		return MemoryRecallOptions{}, fmt.Errorf("%w: query vector exceeds %d dimensions", ErrMemoryInputInvalid, maxMemoryVectorDimensions)
	}
	if len(opts.QueryVector) > 0 {
		opts.QueryVector = append([]float64(nil), opts.QueryVector...)
		for i, component := range opts.QueryVector {
			if math.IsNaN(component) || math.IsInf(component, 0) || math.Abs(component) > maxMemoryVectorComponent {
				return MemoryRecallOptions{}, fmt.Errorf("%w: query vector component %d is not finite and bounded", ErrMemoryInputInvalid, i)
			}
			if component == 0 {
				opts.QueryVector[i] = 0
			}
		}
	}
	if opts.Kind != "" && !memoryKindPattern.MatchString(opts.Kind) {
		return MemoryRecallOptions{}, fmt.Errorf("%w: invalid recall kind", ErrMemoryInputInvalid)
	}
	opts.Origin = strings.TrimSpace(opts.Origin)
	opts.CaptureReason = strings.TrimSpace(opts.CaptureReason)
	if opts.Origin != "" && !memoryCodePattern.MatchString(opts.Origin) ||
		opts.CaptureReason != "" && !memoryCodePattern.MatchString(opts.CaptureReason) {
		return MemoryRecallOptions{}, fmt.Errorf("%w: invalid recall origin or capture reason", ErrMemoryInputInvalid)
	}
	var err error
	opts.Tags, err = normalizeMemoryStrings(opts.Tags, 64, 128, "tag")
	if err != nil {
		return MemoryRecallOptions{}, err
	}
	opts.Links, err = normalizeMemoryStrings(opts.Links, 256, 2048, "link")
	if err != nil {
		return MemoryRecallOptions{}, err
	}
	if opts.Limit == 0 {
		opts.Limit = 20
	}
	if opts.Limit < 1 || opts.Limit > maxMemoryRecallPageSize {
		return MemoryRecallOptions{}, fmt.Errorf("%w: recall limit must be 1-%d", ErrMemoryInputInvalid, maxMemoryRecallPageSize)
	}
	for _, item := range []**time.Time{&opts.OccurredFrom, &opts.OccurredUntil, &opts.CapturedFrom, &opts.CapturedUntil, &opts.AsOf, &opts.AfterUpdatedAt} {
		if *item != nil {
			value := (*item).UTC()
			*item = &value
		}
	}
	if opts.OccurredFrom != nil && opts.OccurredUntil != nil && opts.OccurredUntil.Before(*opts.OccurredFrom) {
		return MemoryRecallOptions{}, fmt.Errorf("%w: occurred range is reversed", ErrMemoryInputInvalid)
	}
	if opts.CapturedFrom != nil && opts.CapturedUntil != nil && opts.CapturedUntil.Before(*opts.CapturedFrom) {
		return MemoryRecallOptions{}, fmt.Errorf("%w: captured range is reversed", ErrMemoryInputInvalid)
	}
	if opts.Cursor != "" && (opts.AsOf != nil || opts.SnapshotChangeSeq != nil ||
		opts.SnapshotDeletedMemoryCount != nil ||
		opts.AfterScore != nil || opts.AfterUpdatedAt != nil || opts.AfterID != "") {
		return MemoryRecallOptions{}, fmt.Errorf("%w: cursor cannot be combined with internal recall page keys", ErrMemoryInputInvalid)
	}
	if (opts.AsOf == nil) != (opts.SnapshotChangeSeq == nil) {
		return MemoryRecallOptions{}, fmt.Errorf("%w: incomplete recall snapshot key", ErrMemoryInputInvalid)
	}
	if (opts.AsOf == nil) != (opts.SnapshotDeletedMemoryCount == nil) {
		return MemoryRecallOptions{}, fmt.Errorf("%w: incomplete recall deletion snapshot key", ErrMemoryInputInvalid)
	}
	if opts.SnapshotChangeSeq != nil && *opts.SnapshotChangeSeq < 0 {
		return MemoryRecallOptions{}, fmt.Errorf("%w: invalid recall snapshot watermark", ErrMemoryInputInvalid)
	}
	if opts.SnapshotDeletedMemoryCount != nil && *opts.SnapshotDeletedMemoryCount < 0 {
		return MemoryRecallOptions{}, fmt.Errorf("%w: invalid recall deletion snapshot", ErrMemoryInputInvalid)
	}
	if (opts.AfterScore == nil) != (opts.AfterUpdatedAt == nil) || (opts.AfterScore == nil) != (opts.AfterID == "") {
		return MemoryRecallOptions{}, fmt.Errorf("%w: incomplete recall page key", ErrMemoryInputInvalid)
	}
	if opts.AfterScore != nil && (math.IsNaN(*opts.AfterScore) || math.IsInf(*opts.AfterScore, 0)) {
		return MemoryRecallOptions{}, fmt.Errorf("%w: invalid recall page score", ErrMemoryInputInvalid)
	}
	if opts.Query == "" && opts.Kind == "" && len(opts.Tags) == 0 && len(opts.Links) == 0 &&
		opts.Origin == "" && opts.CaptureReason == "" && opts.OccurredFrom == nil &&
		opts.OccurredUntil == nil && opts.CapturedFrom == nil && opts.CapturedUntil == nil && opts.VectorProfileID == "" {
		return MemoryRecallOptions{}, fmt.Errorf("%w: recall requires query text or a structured filter", ErrMemoryInputInvalid)
	}
	return opts, nil
}

func memoryRecallBindingHash(opts MemoryRecallOptions) (string, error) {
	queryVectorHash := ""
	if len(opts.QueryVector) > 0 {
		raw, err := json.Marshal(opts.QueryVector)
		if err != nil {
			return "", err
		}
		sum := sha256.Sum256(raw)
		queryVectorHash = hex.EncodeToString(sum[:])
	}
	return memoryRequestHash(memoryRecallCursorBinding{
		Query: opts.Query, Kind: opts.Kind, Tags: opts.Tags, Links: opts.Links,
		Origin: opts.Origin, CaptureReason: opts.CaptureReason,
		IncludeSensitive: opts.IncludeSensitive,
		OccurredFrom:     opts.OccurredFrom, OccurredUntil: opts.OccurredUntil,
		CapturedFrom: opts.CapturedFrom, CapturedUntil: opts.CapturedUntil,
		VectorProfileID: opts.VectorProfileID, QueryVectorHash: queryVectorHash,
	})
}

func encodeMemoryRecallCursor(cursor memoryRecallCursor) (string, error) {
	if cursor.Version != 2 || !isSHA256Hex(cursor.QueryHash) || cursor.AsOf.IsZero() ||
		cursor.SnapshotChangeSeq < 1 || cursor.SnapshotDeletedMemoryCount < 0 ||
		cursor.UpdatedAt.IsZero() || !validMemoryID(cursor.MemoryID) ||
		math.IsNaN(cursor.AfterScore) || math.IsInf(cursor.AfterScore, 0) {
		return "", fmt.Errorf("%w: invalid recall cursor key", ErrMemoryInputInvalid)
	}
	raw, err := json.Marshal(cursor)
	if err != nil {
		return "", fmt.Errorf("encode recall cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeMemoryRecallCursor(raw string) (memoryRecallCursor, error) {
	data, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(raw))
	if err != nil {
		return memoryRecallCursor{}, fmt.Errorf("%w: invalid recall cursor", ErrMemoryInputInvalid)
	}
	var cursor memoryRecallCursor
	if err := json.Unmarshal(data, &cursor); err != nil || cursor.Version != 2 ||
		!isSHA256Hex(cursor.QueryHash) || cursor.AsOf.IsZero() || cursor.UpdatedAt.IsZero() ||
		cursor.SnapshotChangeSeq < 1 || cursor.SnapshotDeletedMemoryCount < 0 ||
		!validMemoryID(cursor.MemoryID) ||
		math.IsNaN(cursor.AfterScore) || math.IsInf(cursor.AfterScore, 0) {
		return memoryRecallCursor{}, fmt.Errorf("%w: invalid recall cursor", ErrMemoryInputInvalid)
	}
	return cursor, nil
}
