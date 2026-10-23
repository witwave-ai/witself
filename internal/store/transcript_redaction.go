package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	// ErrTranscriptRedactionConflict reports a stale preview or conflicting replay.
	ErrTranscriptRedactionConflict = errors.New("transcript entries changed since redaction preview")
	// ErrTranscriptEntriesAlreadyRedacted reports a range with no remaining content.
	ErrTranscriptEntriesAlreadyRedacted = errors.New("every transcript entry in the range is already redacted")
)

// RedactTranscriptEntriesInput selects only identities, never entry content.
// Apply requires the exact preview revision and a bounded retry key.
type RedactTranscriptEntriesInput struct {
	TranscriptID                                 string
	FromSequence, UntilSequence                  int64
	ExpectedRedactionSetRevision, IdempotencyKey string
	Apply                                        bool
}

// TranscriptRedactionEntry describes an entry without exposing its content.
type TranscriptRedactionEntry struct {
	EntryID   string    `json:"entry_id"`
	Sequence  int64     `json:"sequence"`
	Role      string    `json:"role"`
	Kind      string    `json:"kind,omitempty"`
	Redacted  bool      `json:"redacted"`
	CreatedAt time.Time `json:"created_at"`
}

// TranscriptRedactionResult is the value-free preview, apply or replay receipt.
type TranscriptRedactionResult struct {
	TranscriptID              string                     `json:"transcript_id"`
	FromSequence              int64                      `json:"from_sequence"`
	UntilSequence             int64                      `json:"until_sequence"`
	RedactionSetRevision      string                     `json:"redaction_set_revision"`
	EntryCount                int64                      `json:"entry_count"`
	RedactableCount           int64                      `json:"redactable_count"`
	AlreadyRedactedCount      int64                      `json:"already_redacted_count"`
	PartialCaptureEvent       bool                       `json:"partial_capture_event"`
	MemoryEvidenceCount       int64                      `json:"memory_evidence_count"`
	EvidenceMemoryIDs         []string                   `json:"evidence_memory_ids"`
	CurationRunsToCancel      int64                      `json:"curation_runs_to_cancel"`
	CancelledCurationRunCount int64                      `json:"cancelled_curation_run_count"`
	Entries                   []TranscriptRedactionEntry `json:"entries"`
	ReceiptID                 string                     `json:"receipt_id,omitempty"`
	RedactedCount             int64                      `json:"redacted_count"`
	RedactedAt                *time.Time                 `json:"redacted_at,omitempty"`
	Applied                   bool                       `json:"applied"`
	Replayed                  bool                       `json:"replayed"`
}

func normalizeRedactTranscriptEntriesInput(in RedactTranscriptEntriesInput) (RedactTranscriptEntriesInput, error) {
	in.TranscriptID = strings.TrimSpace(in.TranscriptID)
	if in.TranscriptID == "" {
		return in, fmt.Errorf("%w: transcript id is required", ErrTranscriptInputInvalid)
	}
	if in.FromSequence < 1 || in.UntilSequence < in.FromSequence || in.UntilSequence-in.FromSequence >= maxTranscriptAppendBatch {
		return in, fmt.Errorf("%w: sequence range must contain 1-100 entries", ErrTranscriptInputInvalid)
	}
	if !in.Apply {
		if in.ExpectedRedactionSetRevision != "" || in.IdempotencyKey != "" {
			return in, fmt.Errorf("%w: preview accepts transcript id and sequence range only", ErrTranscriptInputInvalid)
		}
		return in, nil
	}
	if !isSHA256Hex(in.ExpectedRedactionSetRevision) {
		return in, fmt.Errorf("%w: expected_redaction_set_revision must be a lowercase SHA-256 revision", ErrTranscriptInputInvalid)
	}
	in.IdempotencyKey = strings.TrimSpace(in.IdempotencyKey)
	if len(in.IdempotencyKey) < 1 || len(in.IdempotencyKey) > 512 {
		return in, fmt.Errorf("%w: Idempotency-Key must contain 1-512 bytes", ErrTranscriptInputInvalid)
	}
	return in, nil
}

func transcriptRedactionSetRevision(in RedactTranscriptEntriesInput, entries []TranscriptRedactionEntry) string {
	digest := sha256.New()
	for _, field := range []string{"witself.transcript-redact.set.v1", in.TranscriptID, strconv.FormatInt(in.FromSequence, 10), strconv.FormatInt(in.UntilSequence, 10)} {
		writeMemoryDeleteRevisionField(digest, field)
	}
	for _, entry := range entries {
		writeMemoryDeleteRevisionField(digest, entry.EntryID)
		writeMemoryDeleteRevisionField(digest, strconv.FormatInt(entry.Sequence, 10))
		flag := "0"
		if entry.Redacted {
			flag = "1"
		}
		writeMemoryDeleteRevisionField(digest, flag)
	}
	return hex.EncodeToString(digest.Sum(nil))
}

func transcriptRedactionReceiptID(accountID string, in RedactTranscriptEntriesInput) string {
	digest := sha256.New()
	for _, field := range []string{"witself.transcript-redact.receipt.v1", accountID, in.TranscriptID,
		strconv.FormatInt(in.FromSequence, 10), strconv.FormatInt(in.UntilSequence, 10),
		in.ExpectedRedactionSetRevision, strings.TrimSpace(in.IdempotencyKey)} {
		writeMemoryDeleteRevisionField(digest, field)
	}
	return "trd_" + hex.EncodeToString(digest.Sum(nil))[:32]
}

// RedactTranscriptEntries permits only a live account-owner operator. Preview
// is a read-only snapshot with no row locks. Apply follows the safety-write,
// operator, owner-lane, conversation, entry lock order; it records only an audit
// event, without usage, activity, curation-due or conversation timestamp writes.
func (s *Store) RedactTranscriptEntries(ctx context.Context, p Principal, in RedactTranscriptEntriesInput) (TranscriptRedactionResult, error) {
	if p.Kind != PrincipalOperator {
		return TranscriptRedactionResult{}, ErrTranscriptForbidden
	}
	in, err := normalizeRedactTranscriptEntriesInput(in)
	if err != nil {
		return TranscriptRedactionResult{}, err
	}
	var tx pgx.Tx
	if in.Apply {
		tx, err = s.beginMemoryCurationMetricsTx(ctx)
	} else {
		tx, err = s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	}
	if err != nil {
		return TranscriptRedactionResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if in.Apply {
		if err := lockAccountForSafetyWrite(ctx, tx, p.AccountID); err != nil {
			return TranscriptRedactionResult{}, err
		}
	}
	var status string
	var evacuating bool
	if err := tx.QueryRow(ctx, `SELECT status, evacuation_id IS NOT NULL FROM accounts WHERE id=$1`, p.AccountID).Scan(&status, &evacuating); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return TranscriptRedactionResult{}, ErrAccountNotFound
		}
		return TranscriptRedactionResult{}, err
	}
	if status != "active" && status != "suspended" {
		return TranscriptRedactionResult{}, ErrAccountNotActive
	}
	if evacuating {
		return TranscriptRedactionResult{}, ErrAccountEvacuationInProgress
	}
	ownerSQL := `SELECT (is_root OR role='account_owner') FROM operators WHERE account_id=$1 AND id=$2 AND deleted_at IS NULL`
	if in.Apply {
		ownerSQL += ` FOR SHARE`
	}
	var owner bool
	if err := tx.QueryRow(ctx, ownerSQL, p.AccountID, p.ID).Scan(&owner); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return TranscriptRedactionResult{}, ErrTranscriptForbidden
		}
		return TranscriptRedactionResult{}, err
	}
	if !owner {
		return TranscriptRedactionResult{}, ErrTranscriptForbidden
	}
	var agentID, realmID string
	if err := tx.QueryRow(ctx, `SELECT owner_agent_id, realm_id FROM transcript_conversations WHERE id=$1 AND account_id=$2`, in.TranscriptID, p.AccountID).Scan(&agentID, &realmID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return TranscriptRedactionResult{}, ErrTranscriptNotFound
		}
		return TranscriptRedactionResult{}, err
	}
	if in.Apply {
		_, err := loadMemoryCurationLaneTx(ctx, tx, Principal{Kind: PrincipalAgent, AccountID: p.AccountID, RealmID: realmID, ID: agentID}, true)
		if err != nil && !errors.Is(err, ErrMemoryCurationNotFound) {
			return TranscriptRedactionResult{}, err
		}
	}
	conversationSQL := `SELECT next_sequence FROM transcript_conversations WHERE id=$1 AND account_id=$2`
	if in.Apply {
		conversationSQL += ` FOR NO KEY UPDATE`
	}
	var next int64
	if err := tx.QueryRow(ctx, conversationSQL, in.TranscriptID, p.AccountID).Scan(&next); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return TranscriptRedactionResult{}, ErrTranscriptNotFound
		}
		return TranscriptRedactionResult{}, err
	}
	if in.UntilSequence >= next {
		return TranscriptRedactionResult{}, fmt.Errorf("%w: sequence range is outside the transcript", ErrTranscriptInputInvalid)
	}
	out, markers, err := loadTranscriptRedactionEntries(ctx, tx, p.AccountID, in)
	if err != nil {
		return TranscriptRedactionResult{}, err
	}
	if err := transcriptRedactionImpactTx(ctx, tx, p.AccountID, in, &out); err != nil {
		return TranscriptRedactionResult{}, err
	}
	if !in.Apply {
		if err := tx.Commit(ctx); err != nil {
			return TranscriptRedactionResult{}, err
		}
		return out, nil
	}
	out.ReceiptID = transcriptRedactionReceiptID(p.AccountID, in)
	for _, marker := range markers {
		if marker.ReceiptID == out.ReceiptID {
			out.RedactedCount++
			at := marker.RedactedAt
			out.RedactedAt = &at
		}
	}
	if out.RedactedCount > 0 {
		if out.RedactableCount != 0 {
			return TranscriptRedactionResult{}, ErrTranscriptRedactionConflict
		}
		out.Applied, out.Replayed = true, true
		out.RedactableCount = out.RedactedCount
		out.AlreadyRedactedCount = out.EntryCount - out.RedactedCount
		out.RedactionSetRevision = in.ExpectedRedactionSetRevision
		if err := tx.Commit(ctx); err != nil {
			return TranscriptRedactionResult{}, err
		}
		return out, nil
	}
	if out.RedactionSetRevision != in.ExpectedRedactionSetRevision {
		return TranscriptRedactionResult{}, ErrTranscriptRedactionConflict
	}
	if out.RedactableCount == 0 {
		return TranscriptRedactionResult{}, ErrTranscriptEntriesAlreadyRedacted
	}
	var at time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&at); err != nil {
		return TranscriptRedactionResult{}, err
	}
	at = at.UTC().Truncate(time.Microsecond)
	ids, payloads := []string{}, []string{}
	for i := range out.Entries {
		entry := &out.Entries[i]
		if entry.Redacted {
			continue
		}
		payload, err := transcriptRedactionPayload(entry.Kind, out.ReceiptID, at)
		if err != nil {
			return TranscriptRedactionResult{}, err
		}
		ids = append(ids, entry.EntryID)
		payloads = append(payloads, string(payload))
		entry.Redacted = true
	}
	updated, err := tx.Exec(ctx, `UPDATE transcript_entries e
		SET body=$1, payload=v.payload::jsonb, artifacts='[]'::jsonb
		FROM unnest($2::text[], $3::text[]) v(id,payload)
		WHERE e.account_id=$4 AND e.transcript_id=$5 AND e.id=v.id`, TranscriptRedactedBody, ids, payloads, p.AccountID, in.TranscriptID)
	if err != nil {
		return TranscriptRedactionResult{}, err
	}
	if updated.RowsAffected() != out.RedactableCount {
		return TranscriptRedactionResult{}, ErrTranscriptRedactionConflict
	}
	if out.CancelledCurationRunCount, err = cancelTranscriptRedactionCurationTx(ctx, tx, p.AccountID, in); err != nil {
		return TranscriptRedactionResult{}, err
	}
	out.Applied, out.RedactedCount, out.RedactedAt = true, out.RedactableCount, &at
	if err := s.logEventTx(ctx, tx, EventInput{AccountID: p.AccountID, ActorKind: ActorOperator, ActorID: p.ID,
		Verb: VerbTranscriptEntriesRedacted, Metadata: map[string]any{
			"transcript_id": in.TranscriptID, "receipt_id": out.ReceiptID,
			"from_sequence": strconv.FormatInt(in.FromSequence, 10), "until_sequence": strconv.FormatInt(in.UntilSequence, 10),
			"entry_count": strconv.FormatInt(out.EntryCount, 10), "redacted_count": strconv.FormatInt(out.RedactedCount, 10),
		}}); err != nil {
		return TranscriptRedactionResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return TranscriptRedactionResult{}, err
	}
	return out, nil
}

func loadTranscriptRedactionEntries(ctx context.Context, tx pgx.Tx, accountID string, in RedactTranscriptEntriesInput) (TranscriptRedactionResult, []TranscriptEntryRedaction, error) {
	out := TranscriptRedactionResult{TranscriptID: in.TranscriptID, FromSequence: in.FromSequence, UntilSequence: in.UntilSequence,
		Entries: []TranscriptRedactionEntry{}, EvidenceMemoryIDs: []string{}}
	query := `SELECT e.id,e.sequence,e.role,e.created_at,` + transcriptRedactionProbeSQL("$5", "$6") + `,
		CASE WHEN jsonb_typeof(e.payload->'chunk_index')='number' AND e.payload->>'chunk_index' ~ '^[0-9]{1,6}$' THEN (e.payload->>'chunk_index')::bigint END,
		CASE WHEN jsonb_typeof(e.payload->'chunk_count')='number' AND e.payload->>'chunk_count' ~ '^[0-9]{1,6}$' THEN (e.payload->>'chunk_count')::bigint END
		FROM transcript_entries e WHERE e.account_id=$1 AND e.transcript_id=$2 AND e.sequence BETWEEN $3 AND $4 ORDER BY e.sequence`
	if in.Apply {
		query += ` FOR NO KEY UPDATE OF e`
	}
	rows, err := tx.Query(ctx, query, accountID, in.TranscriptID, in.FromSequence, in.UntilSequence, TranscriptRedactedBody, transcriptRedactionKindVocabulary())
	if err != nil {
		return out, nil, err
	}
	defer rows.Close()
	markers := []TranscriptEntryRedaction{}
	for rows.Next() {
		var entry TranscriptRedactionEntry
		var probe transcriptEntryRedactionProbe
		var index, count *int64
		if err := rows.Scan(&entry.EntryID, &entry.Sequence, &entry.Role, &entry.CreatedAt,
			&probe.BodyMatches, &probe.ArtifactsEmpty, &probe.PayloadKeys, &probe.KindType, &probe.KindText, &probe.Marker, &index, &count); err != nil {
			return out, nil, err
		}
		marker, redacted := transcriptEntryRedactionFromProbe(probe)
		entry.Redacted = redacted
		if probe.KindType == "string" {
			entry.Kind = transcriptCaptureKind(probe.KindText)
		}
		if redacted {
			out.AlreadyRedactedCount++
		} else {
			out.RedactableCount++
		}
		if !redacted && index != nil && count != nil && *count > 0 && *index < *count {
			if (entry.Sequence == in.FromSequence && *index > 0) || (entry.Sequence == in.UntilSequence && *index < *count-1) {
				out.PartialCaptureEvent = true
			}
		}
		out.Entries = append(out.Entries, entry)
		markers = append(markers, marker)
	}
	if err := rows.Err(); err != nil {
		return out, nil, err
	}
	out.EntryCount = int64(len(out.Entries))
	if out.EntryCount != in.UntilSequence-in.FromSequence+1 {
		return out, nil, fmt.Errorf("%w: sequence range is outside the transcript", ErrTranscriptInputInvalid)
	}
	out.RedactionSetRevision = transcriptRedactionSetRevision(in, out.Entries)
	return out, markers, nil
}

const transcriptRedactionCurationOverlapSQL = `r.account_id=$1 AND r.state IN ('open','planned') AND EXISTS (
	SELECT 1 FROM memory_curation_run_inputs i WHERE i.run_id=r.id AND i.account_id=r.account_id AND (
	 (i.input_kind IN ('transcript','transcript_coverage') AND i.transcript_id=$2 AND i.sequence_from <= $4 AND i.sequence_until >= $3)
	 OR (i.input_kind='cursor' AND i.cursor_source_kind='transcript' AND i.cursor_stream_id=$2 AND i.cursor_expected_prior < $4 AND i.cursor_upper >= $3)))`

func transcriptRedactionImpactTx(ctx context.Context, tx pgx.Tx, accountID string, in RedactTranscriptEntriesInput, out *TranscriptRedactionResult) error {
	if err := tx.QueryRow(ctx, `SELECT count(*), COALESCE((array_agg(memory_id ORDER BY memory_id COLLATE "C"))[1:20], ARRAY[]::text[]) FROM (
		SELECT DISTINCT memory_id FROM memory_evidence WHERE account_id=$1 AND source_transcript_id=$2
		AND resolution_state='resolved' AND resolved_kind='transcript' AND source_sequence_from <= $4 AND source_sequence_until >= $3) m`,
		accountID, in.TranscriptID, in.FromSequence, in.UntilSequence).Scan(&out.MemoryEvidenceCount, &out.EvidenceMemoryIDs); err != nil {
		return err
	}
	return tx.QueryRow(ctx, `SELECT count(*) FROM memory_curation_runs r WHERE `+transcriptRedactionCurationOverlapSQL,
		accountID, in.TranscriptID, in.FromSequence, in.UntilSequence).Scan(&out.CurationRunsToCancel)
}

// Inline finishCuration's non-requeue transition, without a mutation receipt or
// curation audit event. The owner lane is already locked before the conversation.
func cancelTranscriptRedactionCurationTx(ctx context.Context, tx pgx.Tx, accountID string, in RedactTranscriptEntriesInput) (int64, error) {
	rows, err := tx.Query(ctx, `WITH prior AS (
		SELECT r.id,r.state FROM memory_curation_runs r WHERE `+transcriptRedactionCurationOverlapSQL+` FOR UPDATE OF r
	) UPDATE memory_curation_runs r SET state='abandoned',lease_expires_at=NULL,
		terminal_reason_code='transcript_redacted', terminal_at=clock_timestamp(),updated_at=clock_timestamp()
		FROM prior WHERE r.id=prior.id AND r.state IN ('open','planned')
		RETURNING r.id,r.request_id,prior.state`, accountID, in.TranscriptID, in.FromSequence, in.UntilSequence)
	if err != nil {
		return 0, err
	}
	type cancelledRun struct{ id, requestID, priorState string }
	runs := []cancelledRun{}
	for rows.Next() {
		var run cancelledRun
		if err := rows.Scan(&run.id, &run.requestID, &run.priorState); err != nil {
			rows.Close()
			return 0, err
		}
		runs = append(runs, run)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, run := range runs {
		if _, err := tx.Exec(ctx, `UPDATE memory_curation_requests SET state='cancelled',cancelled_at=clock_timestamp(),updated_at=clock_timestamp()
			WHERE account_id=$1 AND id=$2 AND state='claimed' AND claimed_run_id=$3`, accountID, run.requestID, run.id); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(ctx, `UPDATE memory_curation_lanes SET active_run_id=NULL,fencing_generation=fencing_generation+1,updated_at=clock_timestamp()
			WHERE account_id=$1 AND active_run_id=$2`, accountID, run.id); err != nil {
			return 0, err
		}
		observeMemoryCurationTransitionTx(tx, run.priorState, MemoryCurationRunAbandoned)
	}
	return int64(len(runs)), nil
}
