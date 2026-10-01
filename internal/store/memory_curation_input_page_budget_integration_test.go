package store

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/testenv"
)

// TestMemoryCurationInputPageBudgetPostgres pins the byte bounds of curation
// input pages for a tool-heavy session (issue #627): every page stays well
// under the tool results MCP clients accept, no later input takes a page past
// its budget, tool calls and results keep only a short prefix while every other
// entry keeps its content, the Go classification matches
// memoryCurationObservationalKindsSQL, the stored transcript is unchanged, and
// an empty plan still advances the cursor over every entry.
func TestMemoryCurationInputPageBudgetPostgres(t *testing.T) {
	ctx, st, p := newMemoryCurationAccessProfileStore(t, testenv.RequirePostgres(t))
	// Literal bounds, so this test also fails on a store without them: a page
	// of at most 72 KiB (64 KiB of inputs plus run metadata), and a tool entry
	// body of at most a 2 KiB prefix plus its elision note.
	const pageTransportBound = 72 * 1024
	const toolPrefix = 2 * 1024
	const toolBodyBound = toolPrefix + 128

	transcript, err := st.CreateTranscript(ctx, p.AccountID, p.RealmID, p.ID,
		CreateTranscriptInput{ExternalID: "page-budget-thread"})
	if err != nil {
		t.Fatal(err)
	}
	type entrySpec struct {
		role    string
		payload string
		size    int
	}
	var specs []entrySpec
	// Eight turns shaped like a captured coding session: a prompt, a tool
	// call, a large tool result and an assistant answer.
	for range 8 {
		specs = append(specs,
			entrySpec{TranscriptRoleUser, `{"kind":"message.user"}`, 200},
			entrySpec{TranscriptRoleTool, `{"kind":"tool.call"}`, 300},
			entrySpec{TranscriptRoleTool, `{"kind":"tool.result"}`, 24000},
			entrySpec{TranscriptRoleAssistant, `{"kind":"message.assistant"}`, 10000},
		)
	}
	// Payload shapes on both sides of the observational denylist.
	for _, payload := range []string{
		`{"kind":"tool.call"}`, `{"kind":"tool.result"}`, `{"kind":"tool.error"}`,
		`{"Kind":"tool.result"}`, `{"kind":5}`, `{"kind":{"name":"tool.result"}}`, "",
	} {
		specs = append(specs, entrySpec{TranscriptRoleTool, payload, 4000})
	}
	for i, spec := range specs {
		input := AppendTranscriptEntryInput{
			ExternalID: fmt.Sprintf("page-budget-%d", i+1), Role: spec.role,
			Body: strings.Repeat(string(rune('a'+i%26)), spec.size),
		}
		if spec.payload != "" {
			input.Payload = json.RawMessage(spec.payload)
		}
		if _, err := st.AppendTranscriptEntry(ctx, p.AccountID, p.RealmID, p.ID,
			transcript.ID, input); err != nil {
			t.Fatalf("append entry %d: %v", i+1, err)
		}
	}
	entryCount := int64(len(specs))
	_, stored, err := st.GetTranscript(ctx, p, transcript.ID)
	if err != nil || int64(len(stored)) != entryCount {
		t.Fatalf("stored entries = %d, want %d: %v", len(stored), entryCount, err)
	}

	// The SQL denylist is the authority; hydration must agree with it.
	observational := make(map[int64]bool, len(stored))
	rows, err := st.pool.Query(ctx, `
		SELECT sequence, `+memoryCurationObservationalKindsSQL+`
		FROM transcript_entries
		WHERE transcript_id=$1 AND account_id=$2`, transcript.ID, p.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var sequence int64
		var isObservational bool
		if err := rows.Scan(&sequence, &isObservational); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		observational[sequence] = isObservational
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	requested, err := st.RequestCuration(ctx, p, RequestMemoryCurationInput{
		Scope:         MemoryCurationScope{Sources: []string{MemoryCurationSourceTranscript}},
		CoalescingKey: "page_budget", TriggerReason: "manual_refine",
		IdempotencyKey: "page-budget-request",
	})
	if err != nil {
		t.Fatal(err)
	}
	started, err := st.StartCuration(ctx, p, StartMemoryCurationInput{
		RequestID: requested.Request.ID, LeaseDuration: time.Minute,
		IdempotencyKey: "page-budget-start",
	})
	if err != nil {
		t.Fatal(err)
	}

	var inputs []MemoryCurationRunInput
	pages := 0
	cursor := ""
	for {
		page, err := st.GetCurationRunInputs(ctx, p, started.Run.ID,
			started.Run.FencingGeneration, cursor, maxMemoryCurationPageSize)
		if err != nil {
			t.Fatalf("page %d: %v", pages+1, err)
		}
		pages++
		inputBytes := 0
		for _, input := range page.Inputs {
			encoded, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			inputBytes += len(encoded)
		}
		if len(page.Inputs) > 1 && inputBytes > maxMemoryCurationPageBytes {
			t.Fatalf("page %d carries %d input bytes in %d inputs, over the %d-byte page budget",
				pages, inputBytes, len(page.Inputs), maxMemoryCurationPageBytes)
		}
		encodedPage, err := json.Marshal(page)
		if err != nil {
			t.Fatal(err)
		}
		if len(encodedPage) > pageTransportBound {
			t.Fatalf("page %d is %d bytes, over the %d-byte transport bound",
				pages, len(encodedPage), pageTransportBound)
		}
		inputs = append(inputs, page.Inputs...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if pages < 2 || len(inputs) != started.Run.InputCount {
		t.Fatalf("pages = %d, inputs = %d of %d: want the byte budget to split the run",
			pages, len(inputs), started.Run.InputCount)
	}

	next := int64(1)
	for _, input := range inputs {
		if input.Kind != MemoryCurationInputTranscript {
			continue
		}
		for _, entry := range input.TranscriptEntries {
			if entry.Sequence != next {
				t.Fatalf("transcript entries are not contiguous: got %d, want %d", entry.Sequence, next)
			}
			next++
			original := stored[entry.Sequence-1]
			if observational[entry.Sequence] && len(original.Body) > toolBodyBound {
				if len(entry.Body) > toolBodyBound ||
					!strings.HasPrefix(entry.Body, original.Body[:toolPrefix]) ||
					!strings.Contains(entry.Body, "witself:elided") {
					t.Fatalf("observational entry %d kept %d body bytes", entry.Sequence, len(entry.Body))
				}
				continue
			}
			if entry.Body != original.Body || string(entry.Payload) != string(original.Payload) {
				t.Fatalf("entry %d (observational=%t) was elided to %d body bytes",
					entry.Sequence, observational[entry.Sequence], len(entry.Body))
			}
		}
	}
	if next != entryCount+1 {
		t.Fatalf("transcript inputs end at %d, want %d", next-1, entryCount)
	}

	// Elision is a view: the stored transcript is unchanged.
	_, after, err := st.GetTranscript(ctx, p, transcript.ID)
	if err != nil || len(after) != len(stored) {
		t.Fatalf("re-read transcript = %d entries: %v", len(after), err)
	}
	for i := range after {
		if after[i].Body != stored[i].Body || string(after[i].Payload) != string(stored[i].Payload) {
			t.Fatalf("stored entry %d changed", after[i].Sequence)
		}
	}

	planned, err := st.PlanCuration(ctx, p, started.Run.ID, PlanMemoryCurationInput{
		FencingGeneration: started.Run.FencingGeneration,
		Draft:             marshalEmptyCurationPlanForAccessProfile(t),
		IdempotencyKey:    "page-budget-plan",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ApplyCuration(ctx, p, started.Run.ID, ApplyMemoryCurationInput{
		FencingGeneration: started.Run.FencingGeneration,
		PlanRevision:      planned.Plan.PlanRevision,
		PlanHash:          planned.Receipt.PlanHash,
		IdempotencyKey:    "page-budget-apply",
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
