package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TestMemoryCurationOversizedTranscriptInputPagingPostgres freezes a
// transcript whose window is far larger than one input byte budget and proves
// the run stays pageable end to end: freeze chunks the window into contiguous
// inputs, hydration elides what a legacy unchunked input still over-carries,
// every page stays transport-sized, and an empty plan still applies and
// advances the transcript cursor.
func TestMemoryCurationOversizedTranscriptInputPagingPostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, _ := newMigrationTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	provisioned, err := st.ProvisionAccount(ctx,
		"curation-oversized@witwave.ai", "curation oversized", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate = %v / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "default")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "primary")
	if err != nil {
		t.Fatal(err)
	}
	p := Principal{Kind: PrincipalAgent, ID: agent.ID, AccountID: provisioned.AccountID,
		RealmID: realm.ID, AccountStatus: "active"}

	transcript, err := st.CreateTranscript(ctx, p.AccountID, p.RealmID, p.ID,
		CreateTranscriptInput{ExternalID: "oversized-thread"})
	if err != nil {
		t.Fatal(err)
	}
	// Every entry sits near the schema caps: a 60KB body plus a 15KB embedded
	// tool payload. The whole window is ~1.8MB, the shape that used to
	// materialize as one input and break the returning transport.
	const entryCount = 24
	body := strings.Repeat("b", 60000)
	payload := json.RawMessage(`{"tool_result":"` + strings.Repeat("p", 15000) + `"}`)
	for i := range entryCount {
		if _, err := st.AppendTranscriptEntry(ctx, p.AccountID, p.RealmID, p.ID,
			transcript.ID, AppendTranscriptEntryInput{
				ExternalID: fmt.Sprintf("turn-%d", i+1),
				Role:       TranscriptRoleAssistant, Body: body, Payload: payload,
			}); err != nil {
			t.Fatal(err)
		}
	}

	requested, err := st.RequestCuration(ctx, p, RequestMemoryCurationInput{
		Scope:         MemoryCurationScope{Sources: []string{MemoryCurationSourceTranscript}},
		TriggerReason: "manual_refine", IdempotencyKey: "oversized-request",
	})
	if err != nil {
		t.Fatal(err)
	}
	started, err := st.StartCuration(ctx, p, StartMemoryCurationInput{
		RequestID: requested.Request.ID, LeaseDuration: time.Minute,
		IdempotencyKey: "oversized-start",
	})
	if err != nil {
		t.Fatal(err)
	}

	// One hydrated page may finish the input that crossed the page budget, so
	// the transport bound is one page budget plus one input budget plus the
	// bounded per-entry elision notes.
	const pageByteBound = maxMemoryCurationPageBytes + maxMemoryCurationInputBytes + 64*1024
	pageAll := func(label string) []MemoryCurationRunInput {
		t.Helper()
		var transcripts []MemoryCurationRunInput
		cursor := ""
		for {
			page, err := st.GetCurationRunInputs(ctx, p, started.Run.ID,
				started.Run.FencingGeneration, cursor, maxMemoryCurationPageSize)
			if err != nil {
				t.Fatalf("%s page: %v", label, err)
			}
			encoded, err := json.Marshal(page)
			if err != nil {
				t.Fatal(err)
			}
			if len(encoded) > pageByteBound {
				t.Fatalf("%s page bytes = %d, want <= %d", label, len(encoded), pageByteBound)
			}
			for _, input := range page.Inputs {
				if input.Kind == MemoryCurationInputTranscript {
					transcripts = append(transcripts, input)
				}
			}
			if page.NextCursor == "" {
				return transcripts
			}
			cursor = page.NextCursor
		}
	}

	chunked := pageAll("chunked")
	if len(chunked) < 2 {
		t.Fatalf("chunked transcript inputs = %d, want the window split", len(chunked))
	}
	next := int64(1)
	for _, input := range chunked {
		if input.TranscriptID != transcript.ID || input.SequenceFrom != next {
			t.Fatalf("chunked inputs are not contiguous: %#v", chunked)
		}
		if got, want := int64(len(input.TranscriptEntries)),
			input.SequenceUntil-input.SequenceFrom+1; got != want {
			t.Fatalf("hydrated entries = %d, want %d", got, want)
		}
		for _, entry := range input.TranscriptEntries {
			if len(entry.Body) > maxMemoryCurationEntryBodyBytes+256 {
				t.Fatalf("entry %d body bytes = %d", entry.Sequence, len(entry.Body))
			}
			if !strings.Contains(entry.Body, "witself:elided") {
				t.Fatalf("entry %d body lacks elision note", entry.Sequence)
			}
		}
		next = input.SequenceUntil + 1
	}
	if next != entryCount+1 {
		t.Fatalf("chunked coverage ends at %d, want %d", next-1, entryCount)
	}

	// Rewrite the frozen membership into the pre-chunking shape: one input
	// spanning the entire window, exactly what a stuck legacy run holds.
	if _, err := st.pool.Exec(ctx, `
		DELETE FROM memory_curation_run_inputs
		WHERE run_id=$1 AND input_kind='transcript'`, started.Run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO memory_curation_run_inputs
		  (run_id,ordinal,account_id,realm_id,owner_kind,owner_id,input_kind,
		   order_key,transcript_id,sequence_from,sequence_until)
		VALUES ($1,1000,$2,$3,'agent',$4,'transcript','99/legacy',$5,1,$6)`,
		started.Run.ID, p.AccountID, p.RealmID, p.ID, transcript.ID,
		int64(entryCount)); err != nil {
		t.Fatal(err)
	}
	legacy := pageAll("legacy")
	if len(legacy) != 1 || legacy[0].SequenceFrom != 1 ||
		legacy[0].SequenceUntil != entryCount ||
		len(legacy[0].TranscriptEntries) != entryCount {
		t.Fatalf("legacy input = %#v", legacy)
	}
	encoded, err := json.Marshal(legacy[0])
	if err != nil {
		t.Fatal(err)
	}
	if limit := maxMemoryCurationInputBytes + entryCount*1024; len(encoded) > limit {
		t.Fatalf("legacy hydrated input bytes = %d, want <= %d", len(encoded), limit)
	}
	last := legacy[0].TranscriptEntries[entryCount-1]
	if !strings.Contains(last.Body, "witself:elided") ||
		!strings.Contains(string(last.Payload), "witself_elided") {
		t.Fatalf("legacy tail entry not elided: %#v", last)
	}

	planned, err := st.PlanCuration(ctx, p, started.Run.ID, PlanMemoryCurationInput{
		FencingGeneration: started.Run.FencingGeneration,
		Draft:             marshalEmptyCurationPlanForAccessProfile(t),
		IdempotencyKey:    "oversized-plan",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ApplyCuration(ctx, p, started.Run.ID, ApplyMemoryCurationInput{
		FencingGeneration: started.Run.FencingGeneration,
		PlanRevision:      planned.Plan.PlanRevision,
		PlanHash:          planned.Receipt.PlanHash,
		IdempotencyKey:    "oversized-apply",
	}); err != nil {
		t.Fatal(err)
	}
	var position int64
	if err := st.pool.QueryRow(ctx, `
		SELECT position FROM memory_curation_cursors
		WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3
		  AND source_kind='transcript' AND source_stream_id=$4`,
		p.AccountID, p.RealmID, p.ID, transcript.ID).Scan(&position); err != nil {
		t.Fatal(err)
	}
	if position != entryCount {
		t.Fatalf("transcript cursor = %d, want %d", position, entryCount)
	}
}
