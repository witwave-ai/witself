package store

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	archiveexport "github.com/witwave-ai/witself/internal/export"
)

func TestValidateImportedCurationRunInputPrunedTranscriptMetadata(t *testing.T) {
	const (
		accountID = "acc_retention"
		realmID   = "rlm_retention"
		ownerID   = "agent_retention"
	)
	prunedAt := "2026-07-23T01:00:00Z"
	owner := memoryOwnerImportKey{realmID: realmID, ownerKind: "agent", ownerID: ownerID}
	terminal := memoryCurationRunImportScope{owner: owner, state: MemoryCurationRunAbandoned}
	active := memoryCurationRunImportScope{owner: owner, state: MemoryCurationRunOpen}
	base := func(kind string) map[string]any {
		return map[string]any{
			"input_kind": kind, "order_key": "retention/input",
			"sequence_from": float64(1), "sequence_until": float64(2),
			"transcript_id": nil, "transcript_pruned_at": prunedAt,
		}
	}
	tests := []struct {
		name string
		run  memoryCurationRunImportScope
		row  map[string]any
		want string
	}{
		{
			name: "terminal transcript detached",
			run:  terminal,
			row:  base(MemoryCurationInputTranscript),
		},
		{
			name: "terminal transcript coverage detached",
			run:  terminal,
			row: func() map[string]any {
				row := base(MemoryCurationInputTranscriptCoverage)
				row["coverage_counts"] = map[string]any{
					"tool_calls": float64(0), "tool_results": float64(0), "signal": float64(2),
				}
				return row
			}(),
		},
		{
			name: "terminal transcript cursor detached",
			run:  terminal,
			row: map[string]any{
				"input_kind":            MemoryCurationInputCursor,
				"order_key":             "retention/cursor",
				"cursor_source_kind":    MemoryCurationSourceTranscript,
				"cursor_stream_id":      nil,
				"cursor_expected_prior": float64(0),
				"cursor_upper":          float64(2),
				"transcript_pruned_at":  prunedAt,
			},
		},
		{
			name: "active transcript cannot detach",
			run:  active,
			row:  base(MemoryCurationInputTranscript),
			want: "terminal run",
		},
		{
			name: "detached transcript requires marker",
			run:  terminal,
			row: func() map[string]any {
				row := base(MemoryCurationInputTranscript)
				row["transcript_pruned_at"] = nil
				return row
			}(),
			want: "pruning metadata",
		},
		{
			name: "detached cursor requires marker",
			run:  terminal,
			row: map[string]any{
				"input_kind":            MemoryCurationInputCursor,
				"order_key":             "retention/cursor-no-marker",
				"cursor_source_kind":    MemoryCurationSourceTranscript,
				"cursor_stream_id":      nil,
				"cursor_expected_prior": float64(0),
				"cursor_upper":          float64(2),
				"transcript_pruned_at":  nil,
			},
			want: "pruning metadata",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ic := newImportCtx(accountID)
			ic.exportedAt = time.Date(2026, 7, 23, 2, 0, 0, 0, time.UTC)
			err := ic.validateImportedCurationRunInput(tc.row, tc.run, tc.row["input_kind"].(string))
			if tc.want == "" {
				if err != nil {
					t.Fatalf("valid detached input: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v; want substring %q", err, tc.want)
			}
		})
	}
}

func TestTranscriptRetentionEnforcedArchiveParityPostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	source, _ := newMigrationTestStore(t, dsn)
	destination, _ := newMigrationTestStore(t, dsn)
	if err := source.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := destination.Migrate(); err != nil {
		t.Fatal(err)
	}

	provisioned, err := source.ProvisionAccount(ctx,
		"retention-archive@witwave.ai", "retention archive", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if activated, err := source.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate = %v / %v", activated, err)
	}
	realm, err := source.CreateRealm(ctx, provisioned.AccountID, "default")
	if err != nil {
		t.Fatal(err)
	}
	newPrincipal := func(name string) Principal {
		t.Helper()
		agent, err := source.CreateAgent(ctx, provisioned.AccountID, realm.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		return Principal{
			Kind: PrincipalAgent, ID: agent.ID, AccountID: provisioned.AccountID,
			RealmID: realm.ID, AccountStatus: "active",
		}
	}
	unheldOwner := newPrincipal("unheld")
	recentOwner := newPrincipal("recent")
	evidenceOwner := newPrincipal("evidence-held")
	activeOwner := newPrincipal("active-curation-held")
	terminalOwner := newPrincipal("terminal-curation")

	type transcriptFixture struct {
		transcript Transcript
		entry      TranscriptEntry
	}
	createTranscript := func(p Principal, key string) transcriptFixture {
		t.Helper()
		transcript, err := source.CreateTranscript(ctx, p.AccountID, p.RealmID, p.ID,
			CreateTranscriptInput{ExternalID: key, Title: key})
		if err != nil {
			t.Fatal(err)
		}
		entry, err := source.AppendTranscriptEntry(ctx, p.AccountID, p.RealmID, p.ID,
			transcript.ID, AppendTranscriptEntryInput{
				ExternalID: key + "-entry", Role: TranscriptRoleUser, Body: key,
			})
		if err != nil {
			t.Fatal(err)
		}
		return transcriptFixture{transcript: transcript, entry: entry}
	}
	unheld := createTranscript(unheldOwner, "expired-unheld")
	recent := createTranscript(recentOwner, "recent")
	evidenceHeld := createTranscript(evidenceOwner, "expired-evidence-held")
	activeHeld := createTranscript(activeOwner, "expired-active-curation-held")
	terminal := createTranscript(terminalOwner, "expired-terminal-curation")

	if _, err := source.CaptureMemory(ctx, evidenceOwner, CaptureMemoryInput{
		Content: "Evidence keeps its source conversation portable.",
		Kind:    "decision",
		Evidence: []MemoryEvidenceInput{{
			ResolutionState:     MemoryEvidenceResolved,
			ResolvedKind:        MemoryCurationSourceTranscript,
			SourceTranscriptID:  evidenceHeld.transcript.ID,
			SourceSequenceFrom:  evidenceHeld.entry.Sequence,
			SourceSequenceUntil: evidenceHeld.entry.Sequence,
		}},
		IdempotencyKey: "retention-archive-evidence",
	}); err != nil {
		t.Fatal(err)
	}

	startTranscriptCuration := func(p Principal, key string) StartMemoryCurationResult {
		t.Helper()
		requested, err := source.RequestCuration(ctx, p, RequestMemoryCurationInput{
			Scope: MemoryCurationScope{
				Sources:              []string{MemoryCurationSourceTranscript},
				MaxTranscriptEntries: 10,
			},
			CoalescingKey: key, TriggerReason: "retention_archive",
			IdempotencyKey: key + "-request",
		})
		if err != nil {
			t.Fatal(err)
		}
		started, err := source.StartCuration(ctx, p, StartMemoryCurationInput{
			RequestID: requested.Request.ID,
			Caps: MemoryCurationInputCaps{
				MaxTranscriptEntries: 10,
			},
			LeaseDuration: 30 * time.Minute, IdempotencyKey: key + "-start",
		})
		if err != nil {
			t.Fatal(err)
		}
		return started
	}
	activeRun := startTranscriptCuration(activeOwner, "archive-active")
	terminalRun := startTranscriptCuration(terminalOwner, "archive-terminal")
	if _, err := source.CancelCuration(ctx, terminalOwner, terminalRun.Run.ID,
		FinishMemoryCurationInput{
			FencingGeneration: terminalRun.Run.FencingGeneration,
			Reason:            "fixture_terminal",
			IdempotencyKey:    "archive-terminal-cancel",
		}); err != nil {
		t.Fatal(err)
	}

	expiredIDs := []string{
		unheld.transcript.ID,
		evidenceHeld.transcript.ID,
		activeHeld.transcript.ID,
		terminal.transcript.ID,
	}
	if _, err := source.pool.Exec(ctx, `
		UPDATE transcript_conversations
		   SET updated_at=transaction_timestamp() - interval '31 days'
		 WHERE id=ANY($1::text[])`, expiredIDs); err != nil {
		t.Fatal(err)
	}
	if _, err := source.SetAccountPlan(ctx, provisioned.AccountID, 0, "", "free",
		map[string]int64{}, map[string]int64{TranscriptRetentionDaysPolicy: 30}, nil); err != nil {
		t.Fatal(err)
	}

	canonicalInputMetadata := func(object map[string]any) string {
		t.Helper()
		clone := make(map[string]any, len(object))
		for key, value := range object {
			clone[key] = value
		}
		delete(clone, "transcript_id")
		delete(clone, "cursor_stream_id")
		delete(clone, "transcript_pruned_at")
		encoded, err := json.Marshal(clone)
		if err != nil {
			t.Fatal(err)
		}
		return string(encoded)
	}
	sourceTerminalMetadata := map[int64]string{}
	rows, err := source.pool.Query(ctx, `
		SELECT ordinal, to_jsonb(i)::text
		  FROM memory_curation_run_inputs i
		 WHERE run_id=$1
		   AND (
		     input_kind IN ('transcript','transcript_coverage')
		     OR (input_kind='cursor' AND cursor_source_kind='transcript')
		   )
		 ORDER BY ordinal`, terminalRun.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var ordinal int64
		var raw string
		if err := rows.Scan(&ordinal, &raw); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		var object map[string]any
		if err := json.Unmarshal([]byte(raw), &object); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		sourceTerminalMetadata[ordinal] = canonicalInputMetadata(object)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	rows.Close()
	sourceTerminalInputs := int64(len(sourceTerminalMetadata))
	if sourceTerminalInputs < 2 {
		t.Fatalf("terminal curation fixture has %d transcript inputs; want transcript and cursor", sourceTerminalInputs)
	}

	enforced, err := source.ProcessTranscriptRetentionBatch(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if enforced.Deleted != 2 ||
		enforced.ReleasedCurationInputs != sourceTerminalInputs ||
		enforced.DeletedCurationCursors != 1 {
		t.Fatalf("enforcement deleted=%d released_inputs=%d deleted_cursors=%d; want 2/%d/1",
			enforced.Deleted, enforced.ReleasedCurationInputs,
			enforced.DeletedCurationCursors, sourceTerminalInputs)
	}

	// An expired row created after the bounded enforcement batch represents
	// ordinary cleanup lag. Preview and export must preserve the current
	// physical database state rather than silently enforcing retention.
	unswept := createTranscript(unheldOwner, "expired-unswept-after-batch")
	if _, err := source.pool.Exec(ctx, `
		UPDATE transcript_conversations
		   SET updated_at=transaction_timestamp() - interval '31 days'
		 WHERE id=$1`, unswept.transcript.ID); err != nil {
		t.Fatal(err)
	}
	preview, err := source.PreviewTranscriptRetentionBatch(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Eligible != 1 || preview.Deleted != 0 {
		t.Fatalf("preview eligible=%d deleted=%d; want 1/0",
			preview.Eligible, preview.Deleted)
	}

	if err := source.SuspendAccountSystem(ctx, provisioned.AccountID,
		"evacuation", "retention archive parity"); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := source.ExportAccount(ctx, provisioned.AccountID,
		"retention-source", "test", &archive); err != nil {
		t.Fatal(err)
	}

	archivedTranscripts := map[string]bool{}
	archivedEntries := map[string]bool{}
	var archivedTerminalInputs int64
	var detachedTerminalInputs int64
	var activeAttachedInputs int64
	archivedCursors := map[string]bool{}
	archivedTerminalMetadata := map[int64]string{}
	if _, err := archiveexport.Read(ctx, bytes.NewReader(archive.Bytes()), archiveexport.ImportOptions{
		CurrentSchema: SchemaVersion(),
		Row: func(table string, row []byte) error {
			var object map[string]any
			if err := json.Unmarshal(row, &object); err != nil {
				return err
			}
			switch table {
			case "transcript_conversations":
				archivedTranscripts[object["id"].(string)] = true
			case "transcript_entries":
				archivedEntries[object["transcript_id"].(string)] = true
			case "memory_curation_cursors":
				if object["source_kind"] == MemoryCurationSourceTranscript {
					archivedCursors[object["source_stream_id"].(string)] = true
				}
			case "memory_curation_run_inputs":
				runID, _ := object["run_id"].(string)
				kind, _ := object["input_kind"].(string)
				if runID == terminalRun.Run.ID &&
					(kind == MemoryCurationInputTranscript ||
						kind == MemoryCurationInputTranscriptCoverage ||
						kind == MemoryCurationInputCursor &&
							object["cursor_source_kind"] == MemoryCurationSourceTranscript) {
					archivedTerminalInputs++
					if object["transcript_pruned_at"] != nil &&
						object["transcript_id"] == nil &&
						(kind != MemoryCurationInputCursor || object["cursor_stream_id"] == nil) {
						detachedTerminalInputs++
					}
					ordinal := int64(object["ordinal"].(float64))
					archivedTerminalMetadata[ordinal] = canonicalInputMetadata(object)
				}
				if runID == activeRun.Run.ID && object["transcript_pruned_at"] == nil {
					switch kind {
					case MemoryCurationInputTranscript, MemoryCurationInputTranscriptCoverage:
						if object["transcript_id"] == activeHeld.transcript.ID {
							activeAttachedInputs++
						}
					case MemoryCurationInputCursor:
						if object["cursor_source_kind"] == MemoryCurationSourceTranscript &&
							object["cursor_stream_id"] == activeHeld.transcript.ID {
							activeAttachedInputs++
						}
					}
				}
			}
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	for _, fixture := range []transcriptFixture{recent, evidenceHeld, activeHeld, unswept} {
		if !archivedTranscripts[fixture.transcript.ID] ||
			!archivedEntries[fixture.transcript.ID] {
			t.Fatalf("retained transcript %s or its entry was omitted", fixture.transcript.ID)
		}
	}
	for _, fixture := range []transcriptFixture{unheld, terminal} {
		if archivedTranscripts[fixture.transcript.ID] ||
			archivedEntries[fixture.transcript.ID] {
			t.Fatalf("physically deleted transcript %s remained in archive", fixture.transcript.ID)
		}
	}
	if archivedTerminalInputs != sourceTerminalInputs ||
		detachedTerminalInputs != sourceTerminalInputs {
		t.Fatalf("terminal inputs archived=%d detached=%d source=%d",
			archivedTerminalInputs, detachedTerminalInputs, sourceTerminalInputs)
	}
	for ordinal, want := range sourceTerminalMetadata {
		if got, exists := archivedTerminalMetadata[ordinal]; !exists || got != want {
			t.Fatalf("terminal input ordinal %d metadata changed during detachment:\nsource:  %s\narchive: %s",
				ordinal, want, got)
		}
	}
	if activeAttachedInputs < 2 || !archivedCursors[activeHeld.transcript.ID] {
		t.Fatalf("active curation hold was not preserved: inputs=%d cursor=%v",
			activeAttachedInputs, archivedCursors[activeHeld.transcript.ID])
	}
	if archivedCursors[terminal.transcript.ID] {
		t.Fatal("terminal pruned transcript cursor remained live in archive")
	}

	for _, fixture := range []transcriptFixture{unheld, terminal, unswept} {
		var exists bool
		if err := source.pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM transcript_conversations WHERE id=$1)`,
			fixture.transcript.ID).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		wantExists := fixture.transcript.ID == unswept.transcript.ID
		if exists != wantExists {
			t.Fatalf("source transcript %s exists=%v; want %v after enforcement and export",
				fixture.transcript.ID, exists, wantExists)
		}
	}

	if _, err := destination.ImportAccount(ctx, provisioned.AccountID,
		bytes.NewReader(archive.Bytes())); err != nil {
		t.Fatal(err)
	}
	var destinationTranscripts, destinationTerminalInputs, destinationDetachedInputs int64
	if err := destination.pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM transcript_conversations WHERE account_id=$1),
		  (SELECT count(*) FROM memory_curation_run_inputs WHERE run_id=$2),
		  (SELECT count(*) FROM memory_curation_run_inputs
		    WHERE run_id=$2 AND transcript_pruned_at IS NOT NULL
		      AND transcript_id IS NULL
		      AND (input_kind <> 'cursor' OR cursor_stream_id IS NULL))`,
		provisioned.AccountID, terminalRun.Run.ID).Scan(
		&destinationTranscripts, &destinationTerminalInputs, &destinationDetachedInputs,
	); err != nil {
		t.Fatal(err)
	}
	if destinationTranscripts != 4 ||
		destinationTerminalInputs != sourceTerminalInputs ||
		destinationDetachedInputs != sourceTerminalInputs {
		t.Fatalf("destination transcripts=%d terminal=%d detached=%d; want 4/%d/%d",
			destinationTranscripts, destinationTerminalInputs, destinationDetachedInputs,
			sourceTerminalInputs, sourceTerminalInputs)
	}
}
