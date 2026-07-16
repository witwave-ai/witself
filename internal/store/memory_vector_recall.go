package store

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	memoryHybridSimilarityWeight = 0.50
	memoryHybridLexicalWeight    = 0.30
	memoryHybridSalienceWeight   = 0.12
	memoryHybridRecencyWeight    = 0.08
	maxMemoryHybridCandidates    = 256
)

type hybridRankedMemory struct {
	id        string
	version   int64
	score     MemoryRecallScore
	updatedAt time.Time
}

// recallMemoriesHybrid performs exact portable scoring inside a deterministic,
// explicitly reported candidate budget. PostgreSQL/JSONB is the correctness
// baseline; a pgvector/ANN projection may later improve candidate generation
// without changing this score, pagination, or degradation contract.
func recallMemoriesHybrid(ctx context.Context, tx pgx.Tx, p Principal, opts MemoryRecallOptions, bindingHash string) (MemoryRecallPage, error) {
	profile, err := getMemoryVectorProfileTx(ctx, tx, p, opts.VectorProfileID)
	if err != nil {
		return MemoryRecallPage{}, err
	}
	queryVector, _, err := normalizeMemoryVector(opts.QueryVector, profile)
	if err != nil {
		return MemoryRecallPage{}, err
	}

	args := []any{p.AccountID, p.RealmID, p.ID, opts.Query, *opts.AsOf, *opts.SnapshotChangeSeq, profile.ID}
	where := []string{"h.state='active'"}
	if opts.ExcludeSensitive {
		where = append(where, "NOT h.sensitive")
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
	candidateLimitArg := addArg(maxMemoryHybridCandidates + 1)
	rows, err := tx.Query(ctx, `
		WITH q AS (
		  SELECT CASE WHEN $4='' THEN NULL
		              ELSE websearch_to_tsquery('simple'::regconfig,$4) END AS query
		), as_of_versions AS (
		  SELECT DISTINCT ON (v.memory_id)
		    m.id AS memory_id,m.origin,m.capture_reason,
		    m.created_at AS memory_created_at,v.version,v.change_seq,
		    v.content_hash,v.search_document,v.kind,v.tags,v.links,v.salience,v.sensitive,
		    v.occurred_from,v.occurred_until,v.state,
		    v.created_at AS version_updated_at
		  FROM memories m JOIN memory_versions v ON v.memory_id=m.id
		  WHERE m.account_id=$1 AND m.realm_id=$2
		    AND m.owner_kind='agent' AND m.owner_id=$3
		    AND v.account_id=$1 AND v.realm_id=$2
		    AND v.owner_kind='agent' AND v.owner_id=$3
		    AND v.change_seq <= $6
		  ORDER BY v.memory_id,v.change_seq DESC
		), evidence_activity AS (
		  SELECT e.memory_id,max(e.created_at) AS updated_at
		  FROM memory_evidence e
		  WHERE e.account_id=$1 AND e.realm_id=$2
		    AND e.owner_kind='agent' AND e.owner_id=$3
		    AND e.evidence_change_seq <= $6 AND e.pending_evidence_id IS NOT NULL
		  GROUP BY e.memory_id
		), as_of_heads AS (
		  SELECT v.*,GREATEST(v.version_updated_at,
		         COALESCE(e.updated_at,v.version_updated_at)) AS updated_at
		  FROM as_of_versions v LEFT JOIN evidence_activity e ON e.memory_id=v.memory_id
		)
		SELECT h.memory_id,h.version,h.updated_at,
		  CASE WHEN q.query IS NULL THEN 0::double precision
		       ELSE ts_rank_cd(h.search_document,q.query,32)::double precision END,
		  h.salience,
		  (1.0/(1.0+GREATEST(EXTRACT(EPOCH FROM ($5::timestamptz-h.updated_at)),0.0)/2592000.0))::double precision,
		  mv.vector
		FROM as_of_heads h CROSS JOIN q
		LEFT JOIN memory_vectors mv
		  ON mv.profile_id=$7 AND mv.memory_id=h.memory_id
		 AND mv.memory_version=h.version AND mv.content_hash=h.content_hash
		 AND mv.account_id=$1 AND mv.realm_id=$2
		 AND mv.owner_kind='agent' AND mv.owner_id=$3 AND mv.created_at <= $5
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY 4 DESC,5 DESC,6 DESC,h.memory_id DESC
		LIMIT `+candidateLimitArg, args...)
	if err != nil {
		return MemoryRecallPage{}, fmt.Errorf("hybrid recall candidates: %w", err)
	}
	defer rows.Close()
	ranked := []hybridRankedMemory{}
	vectorMatches := 0
	for rows.Next() {
		var item hybridRankedMemory
		var rawVector []byte
		if err := rows.Scan(&item.id, &item.version, &item.updatedAt,
			&item.score.Lexical, &item.score.Salience, &item.score.Recency,
			&rawVector); err != nil {
			return MemoryRecallPage{}, err
		}
		if len(rawVector) > 0 {
			var candidate []float64
			if err := json.Unmarshal(rawVector, &candidate); err != nil {
				return MemoryRecallPage{}, fmt.Errorf("decode stored memory vector: %w", err)
			}
			candidate, _, err = normalizeMemoryVector(candidate, profile)
			if err != nil {
				return MemoryRecallPage{}, fmt.Errorf("stored memory vector violates profile: %w", err)
			}
			item.score.Similarity, err = memoryVectorSimilarity(profile, queryVector, candidate)
			if err != nil {
				return MemoryRecallPage{}, err
			}
			item.score.VectorUsed = true
			vectorMatches++
		}
		item.score.Total = memoryHybridSimilarityWeight*item.score.Similarity +
			memoryHybridLexicalWeight*item.score.Lexical +
			memoryHybridSalienceWeight*item.score.Salience +
			memoryHybridRecencyWeight*item.score.Recency
		if math.IsNaN(item.score.Total) || math.IsInf(item.score.Total, 0) {
			return MemoryRecallPage{}, fmt.Errorf("hybrid recall produced a non-finite score")
		}
		ranked = append(ranked, item)
	}
	if err := rows.Err(); err != nil {
		return MemoryRecallPage{}, err
	}
	// Cap before cursor filtering. Every page recomputes this same deterministic
	// snapshot subset and then advances inside it, so pagination can neither
	// admit an omitted candidate nor disguise a non-exhaustive result.
	ranked, candidateTruncated := applyMemoryHybridCandidateBudget(ranked)
	vectorMatches = 0
	for i := range ranked {
		if ranked[i].score.VectorUsed {
			vectorMatches++
		}
	}
	// A requested profile with zero compatible rows uses the real lexical score,
	// not hybrid weights with a different label. An untruncated universe exactly
	// matches ordinary lexical recall; a truncated universe retains its explicit
	// candidate_budget_exceeded degradation contract.
	if len(ranked) > 0 && vectorMatches == 0 {
		lexicalFallback := ranked[:0]
		for i := range ranked {
			if opts.Query != "" && ranked[i].score.Lexical == 0 {
				continue
			}
			ranked[i].score.Total = 0.60*ranked[i].score.Lexical +
				0.25*ranked[i].score.Salience + 0.15*ranked[i].score.Recency
			lexicalFallback = append(lexicalFallback, ranked[i])
		}
		ranked = lexicalFallback
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].score.Total != ranked[j].score.Total {
			return ranked[i].score.Total > ranked[j].score.Total
		}
		if !ranked[i].updatedAt.Equal(ranked[j].updatedAt) {
			return ranked[i].updatedAt.After(ranked[j].updatedAt)
		}
		return ranked[i].id > ranked[j].id
	})
	vectorCandidates := len(ranked)
	coverage := 0.0
	if vectorCandidates > 0 {
		coverage = float64(vectorMatches) / float64(vectorCandidates)
	}
	if opts.AfterScore != nil {
		filtered := ranked[:0]
		for _, item := range ranked {
			if item.score.Total < *opts.AfterScore ||
				(item.score.Total == *opts.AfterScore && item.updatedAt.Before(*opts.AfterUpdatedAt)) ||
				(item.score.Total == *opts.AfterScore && item.updatedAt.Equal(*opts.AfterUpdatedAt) && item.id < opts.AfterID) {
				filtered = append(filtered, item)
			}
		}
		ranked = filtered
	}
	page := MemoryRecallPage{
		Hits: []MemoryRecallHit{}, RetrievalMode: "hybrid",
		VectorCoverage: coverage, VectorProfileID: profile.ID,
		VectorCandidates: vectorCandidates, VectorMatches: vectorMatches,
		CandidateTruncated: candidateTruncated, CandidateLimit: maxMemoryHybridCandidates,
	}
	if candidateTruncated {
		page.Degraded = true
		page.DegradedReason = "candidate_budget_exceeded"
		if vectorMatches == 0 {
			page.RetrievalMode = "lexical"
		}
	} else if vectorCandidates > 0 && vectorMatches < vectorCandidates {
		page.Degraded = true
		if vectorMatches == 0 {
			page.RetrievalMode = "lexical"
			page.DegradedReason = "no_compatible_vectors"
		} else {
			page.DegradedReason = "partial_vector_coverage"
		}
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
		return page, nil
	}
	ids := make([]string, len(ranked))
	versions := make([]int64, len(ranked))
	for i := range ranked {
		ids[i], versions[i] = ranked[i].id, ranked[i].version
	}
	payloadRows, err := tx.Query(ctx, memorySelectSQL+`
		JOIN unnest($1::text[],$2::bigint[]) AS selected(memory_id,memory_version)
		  ON selected.memory_id=m.id AND selected.memory_version=v.version
		WHERE m.account_id=$3 AND m.realm_id=$4
		  AND m.owner_kind='agent' AND m.owner_id=$5`,
		ids, versions, p.AccountID, p.RealmID, p.ID)
	if err != nil {
		return MemoryRecallPage{}, fmt.Errorf("load hybrid recalled memories: %w", err)
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
		return MemoryRecallPage{}, err
	}
	payloadRows.Close()
	for _, item := range ranked {
		memory, ok := memories[item.id]
		if !ok {
			return MemoryRecallPage{}, fmt.Errorf("hybrid recall snapshot lost memory version %s/%d", item.id, item.version)
		}
		memory.UpdatedAt = item.updatedAt
		page.Hits = append(page.Hits, MemoryRecallHit{Memory: memory, Score: item.score})
	}
	return page, nil
}

func applyMemoryHybridCandidateBudget(items []hybridRankedMemory) ([]hybridRankedMemory, bool) {
	if len(items) <= maxMemoryHybridCandidates {
		return items, false
	}
	return items[:maxMemoryHybridCandidates], true
}
