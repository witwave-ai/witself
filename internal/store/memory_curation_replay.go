package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
)

// materializeMemoryCurationReplayInputsTx clones the immutable, non-cursor
// membership of a rolled-back run and adds the current heads of every memory
// implicated by that accepted plan. Replay never inherits old cursor intervals:
// it may produce a new plan, but applying that plan cannot acknowledge source
// work on behalf of the historical run.
func materializeMemoryCurationReplayInputsTx(
	ctx context.Context,
	tx pgx.Tx,
	p Principal,
	runID, replayRunID string,
	scope MemoryCurationScope,
) (memoryCurationInputCounts, int64, int64, error) {
	counts := memoryCurationInputCounts{}
	original, err := loadMemoryCurationRun(ctx, tx, p, replayRunID, false)
	if err != nil {
		return counts, 0, 0, err
	}
	if original.State != MemoryCurationRunRolledBack || original.RollbackReceiptID == "" {
		return counts, 0, 0, ErrMemoryCurationConflict
	}
	var transcriptInputsPruned bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
		  SELECT 1
		    FROM memory_curation_run_inputs
		   WHERE run_id=$1 AND account_id=$2 AND realm_id=$3
		     AND owner_kind='agent' AND owner_id=$4
		     AND transcript_pruned_at IS NOT NULL
		)`, original.ID, p.AccountID, p.RealmID, p.ID).Scan(&transcriptInputsPruned); err != nil {
		return counts, 0, 0, fmt.Errorf("check original replay transcript inputs: %w", err)
	}
	if transcriptInputsPruned {
		return counts, 0, 0, fmt.Errorf(
			"%w: source run transcript inputs were pruned by transcript retention",
			ErrMemoryCurationConflict,
		)
	}
	stored, err := loadMemoryCurationStoredPlan(ctx, tx, p, original)
	if err != nil {
		return counts, 0, 0, err
	}

	ordinal := int64(0)
	seenVersions := make(map[string]struct{})
	memoryIDs := make(map[string]struct{})
	appendInput := func(input MemoryCurationRunInput, orderKey string) error {
		if counts.total >= 10000 {
			return fmt.Errorf("%w: replay input ceiling exceeded", ErrMemoryCurationConflict)
		}
		ordinal++
		input.RunID, input.Ordinal = runID, ordinal
		if err := insertMemoryCurationRunInputTx(ctx, tx, p, &input, orderKey); err != nil {
			return err
		}
		counts.total++
		switch input.Kind {
		case MemoryCurationInputMemory:
			counts.memories++
			seenVersions[memoryCurationPlanVersionKey(input.MemoryID, input.MemoryVersion)] = struct{}{}
			memoryIDs[input.MemoryID] = struct{}{}
		case MemoryCurationInputEvidence:
			counts.evidence++
		case MemoryCurationInputTranscript:
			counts.transcripts++
		default:
			return ErrMemoryCurationConflict
		}
		return nil
	}

	rows, err := tx.Query(ctx, `
		SELECT ordinal,input_kind,COALESCE(memory_id,''),COALESCE(memory_version,0),
		       COALESCE(evidence_id,''),COALESCE(transcript_id,''),
		       COALESCE(sequence_from,0),COALESCE(sequence_until,0),coverage_counts
		FROM memory_curation_run_inputs
		WHERE run_id=$1 AND account_id=$2 AND realm_id=$3
		  AND owner_kind='agent' AND owner_id=$4 AND input_kind<>'cursor'
		ORDER BY ordinal`, original.ID, p.AccountID, p.RealmID, p.ID)
	if err != nil {
		return counts, 0, 0, fmt.Errorf("load original replay inputs: %w", err)
	}
	type clonedInput struct {
		input           MemoryCurationRunInput
		originalOrdinal int64
	}
	cloned := make([]clonedInput, 0)
	evidenceIDs := make([]string, 0)
	for rows.Next() {
		var originalOrdinal int64
		var input MemoryCurationRunInput
		var coverage []byte
		if err := rows.Scan(&originalOrdinal, &input.Kind, &input.MemoryID,
			&input.MemoryVersion, &input.EvidenceID, &input.TranscriptID,
			&input.SequenceFrom, &input.SequenceUntil, &coverage); err != nil {
			rows.Close()
			return counts, 0, 0, err
		}
		if len(coverage) > 0 {
			decoded := MemoryCurationCoverageCounts{}
			if err := json.Unmarshal(coverage, &decoded); err != nil {
				rows.Close()
				return counts, 0, 0, fmt.Errorf("decode replay coverage counts: %w", err)
			}
			input.CoverageCounts = &decoded
		}
		if input.Kind == MemoryCurationInputEvidence {
			evidenceIDs = append(evidenceIDs, input.EvidenceID)
		}
		cloned = append(cloned, clonedInput{input: input, originalOrdinal: originalOrdinal})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return counts, 0, 0, err
	}
	rows.Close()
	for _, item := range cloned {
		if err := appendInput(item.input,
			fmt.Sprintf("01/replay/clone/%020d", item.originalOrdinal)); err != nil {
			return counts, 0, 0, err
		}
	}

	if len(evidenceIDs) > 0 {
		rows, err := tx.Query(ctx, `
			SELECT DISTINCT source_memory_id
			FROM memory_evidence
			WHERE id=ANY($1::text[]) AND account_id=$2 AND realm_id=$3
			  AND owner_kind='agent' AND owner_id=$4 AND source_memory_id IS NOT NULL
			ORDER BY source_memory_id`, evidenceIDs, p.AccountID, p.RealmID, p.ID)
		if err != nil {
			return counts, 0, 0, err
		}
		for rows.Next() {
			var memoryID string
			if err := rows.Scan(&memoryID); err != nil {
				rows.Close()
				return counts, 0, 0, err
			}
			memoryIDs[memoryID] = struct{}{}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return counts, 0, 0, err
		}
		rows.Close()
	}
	for _, action := range stored.Actions {
		for _, ref := range action.InputRefs {
			if ref.MemoryID != "" {
				memoryIDs[ref.MemoryID] = struct{}{}
			}
		}
		for _, head := range action.ExpectedHeads {
			memoryIDs[head.MemoryID] = struct{}{}
		}
		if action.Action.Create != nil {
			memoryIDs[action.Action.Create.MemoryID] = struct{}{}
		}
	}

	orderedMemoryIDs := make([]string, 0, len(memoryIDs))
	for memoryID := range memoryIDs {
		orderedMemoryIDs = append(orderedMemoryIDs, memoryID)
	}
	sort.Strings(orderedMemoryIDs)
	for _, memoryID := range orderedMemoryIDs {
		memory, err := loadCurrentMemory(ctx, tx, p, memoryID, false)
		if errors.Is(err, ErrMemoryNotFound) {
			continue
		}
		if err != nil {
			return counts, 0, 0, err
		}
		if memory.Sensitive && !scope.IncludeSensitive {
			continue
		}
		key := memoryCurationPlanVersionKey(memory.ID, memory.Version)
		if _, exists := seenVersions[key]; exists {
			continue
		}
		if err := appendInput(MemoryCurationRunInput{
			Kind: MemoryCurationInputMemory, MemoryID: memory.ID,
			MemoryVersion: memory.Version,
		}, fmt.Sprintf("02/replay/current/%s/%020d", memory.ID, memory.Version)); err != nil {
			return counts, 0, 0, err
		}
	}

	var upper int64
	err = tx.QueryRow(ctx, `
		SELECT last_change_seq FROM memory_change_clocks
		WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3
		FOR SHARE`, p.AccountID, p.RealmID, p.ID).Scan(&upper)
	if errors.Is(err, pgx.ErrNoRows) {
		upper = 0
	} else if err != nil {
		return counts, 0, 0, err
	}
	return counts, upper, upper, nil
}
