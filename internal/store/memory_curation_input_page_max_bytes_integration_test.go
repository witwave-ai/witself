package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/testenv"
)

// TestMemoryCurationInputPageMaxBytesPostgres pins the client-chosen page
// byte bound of curation input reads (issue #649): inputs that fit one page
// under the server budget arrive in several pages under a 16 KiB bound and in
// at least one page per transcript input under an 8 KiB bound; every page
// keeps its bound unless it carries one larger input; every input is
// byte-identical across the bounded and the unbounded reads; the bound is per
// request and never sticks to the run; the range is closed at 8192 and 65536;
// and an input larger than the smallest bound is still delivered alone, so
// the cursor advances past it.
func TestMemoryCurationInputPageMaxBytesPostgres(t *testing.T) {
	ctx, st, p := newMemoryCurationAccessProfileStore(t, testenv.RequirePostgres(t))
	// Literal bounds: the range is a published contract, not a constant to
	// read back.
	const (
		smallest   = 8192
		largest    = 65536
		sixteenKiB = 16 * 1024
		// Over the 16 KiB entry body cap, so the entry is elided as always
		// and the input still exceeds the largest explicit bound this test
		// reads under (16 KiB).
		oversized = 20000
	)

	// Six short conversations of three signal entries each, then one whose
	// single entry is larger than any bound of this test. Each conversation
	// freezes as its own cursor input plus one transcript input.
	appendEntries := func(name string, bodies ...string) string {
		t.Helper()
		transcript, err := st.CreateTranscript(ctx, p.AccountID, p.RealmID, p.ID,
			CreateTranscriptInput{ExternalID: name})
		if err != nil {
			t.Fatal(err)
		}
		for i, body := range bodies {
			role, kind := TranscriptRoleUser, "message.user"
			if i%2 == 1 {
				role, kind = TranscriptRoleAssistant, "message.assistant"
			}
			if _, err := st.AppendTranscriptEntry(ctx, p.AccountID, p.RealmID, p.ID,
				transcript.ID, AppendTranscriptEntryInput{
					ExternalID: fmt.Sprintf("%s-%d", name, i+1), Role: role, Body: body,
					Payload: json.RawMessage(fmt.Sprintf(`{"kind":%q}`, kind)),
				}); err != nil {
				t.Fatalf("append %s entry %d: %v", name, i+1, err)
			}
		}
		return transcript.ID
	}
	for i := range 6 {
		body := strings.Repeat(string(rune('a'+i)), 1400)
		appendEntries(fmt.Sprintf("page-bound-small-%d", i+1), body, body, body)
	}
	large := appendEntries("page-bound-large", strings.Repeat("z", oversized))

	requested, err := st.RequestCuration(ctx, p, RequestMemoryCurationInput{
		Scope:         MemoryCurationScope{Sources: []string{MemoryCurationSourceTranscript}},
		CoalescingKey: "page_bound", TriggerReason: "manual_refine",
		IdempotencyKey: "page-bound-request",
	})
	if err != nil {
		t.Fatal(err)
	}
	// About twenty page reads follow; a five-minute lease keeps a loaded
	// host from expiring it under -race.
	started, err := st.StartCuration(ctx, p, StartMemoryCurationInput{
		RequestID: requested.Request.ID, LeaseDuration: 5 * time.Minute,
		IdempotencyKey: "page-bound-start",
	})
	if err != nil {
		t.Fatal(err)
	}
	if started.Run.TranscriptInputCount != 7 {
		t.Fatalf("transcript inputs = %d, want 7, one per conversation", started.Run.TranscriptInputCount)
	}
	encode := func(input MemoryCurationRunInput) []byte {
		t.Helper()
		encoded, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}

	// readAll pages the run to exhaustion under one bound (zero is the
	// server budget) and returns every input in order, the page count, and
	// the pages that exceeded the bound. A page over the bound with more
	// than one input is the server breaking the bound, so it fails here.
	readAll := func(maxBytes int) ([]MemoryCurationRunInput, int, [][]MemoryCurationRunInput) {
		t.Helper()
		bound := maxBytes
		if bound == 0 {
			bound = maxMemoryCurationPageBytes
		}
		var inputs []MemoryCurationRunInput
		var over [][]MemoryCurationRunInput
		cursor := ""
		for pages := 1; ; pages++ {
			if pages > started.Run.InputCount {
				t.Fatalf("max_bytes %d: did not page to exhaustion in %d pages", maxBytes, started.Run.InputCount)
			}
			page, err := st.GetCurationRunInputPage(ctx, p, started.Run.ID, MemoryCurationRunInputOptions{
				FencingGeneration: started.Run.FencingGeneration, Cursor: cursor,
				Limit: maxMemoryCurationPageSize, MaxBytes: maxBytes,
			})
			if err != nil {
				t.Fatalf("max_bytes %d page %d: %v", maxBytes, pages, err)
			}
			if len(page.Inputs) == 0 {
				t.Fatalf("max_bytes %d page %d is empty", maxBytes, pages)
			}
			size := 0
			for _, input := range page.Inputs {
				size += len(encode(input))
			}
			if size > bound {
				if len(page.Inputs) > 1 {
					t.Fatalf("max_bytes %d page %d carries %d input bytes in %d inputs, over the %d-byte bound",
						maxBytes, pages, size, len(page.Inputs), bound)
				}
				over = append(over, page.Inputs)
			}
			inputs = append(inputs, page.Inputs...)
			if page.NextCursor != "" {
				cursor = page.NextCursor
				continue
			}
			if len(inputs) != started.Run.InputCount {
				t.Fatalf("max_bytes %d delivered %d of %d inputs", maxBytes, len(inputs), started.Run.InputCount)
			}
			return inputs, pages, over
		}
	}
	sameInputs := func(label string, got, want []MemoryCurationRunInput) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s: %d inputs, want %d", label, len(got), len(want))
		}
		for i := range got {
			if got[i].Ordinal != want[i].Ordinal || !bytes.Equal(encode(got[i]), encode(want[i])) {
				t.Fatalf("%s: input %d (ordinal %d, kind %s) differs from the unbounded read",
					label, i+1, got[i].Ordinal, got[i].Kind)
			}
		}
	}

	unbounded, unboundedPages, _ := readAll(0)
	if unboundedPages != 1 {
		t.Fatalf("fixture premise: the run needs %d pages under the server budget, want 1", unboundedPages)
	}
	sixteen, sixteenPages, sixteenOver := readAll(sixteenKiB)
	if sixteenPages < 2 {
		t.Fatalf("pages under a %d-byte bound = %d, want at least 2", sixteenKiB, sixteenPages)
	}
	sameInputs("16 KiB bound", sixteen, unbounded)
	eight, eightPages, eightOver := readAll(smallest)
	if eightPages < started.Run.TranscriptInputCount || eightPages <= sixteenPages {
		t.Fatalf("pages under a %d-byte bound = %d, want at least %d and more than the %d under %d bytes",
			smallest, eightPages, started.Run.TranscriptInputCount, sixteenPages, sixteenKiB)
	}
	sameInputs("8 KiB bound", eight, unbounded)

	// The one input larger than the bound arrives alone, elided to the
	// entry body cap as always, and paging went on past it (readAll reached
	// an empty cursor with every input delivered).
	for _, test := range []struct {
		label string
		over  [][]MemoryCurationRunInput
	}{{"16 KiB bound", sixteenOver}, {"8 KiB bound", eightOver}} {
		if len(test.over) != 1 || len(test.over[0]) != 1 {
			t.Fatalf("%s: %d pages over the bound, want exactly one page carrying one input", test.label, len(test.over))
		}
		input := test.over[0][0]
		if input.Kind != MemoryCurationInputTranscript || input.TranscriptID != large ||
			len(input.TranscriptEntries) != 1 {
			t.Fatalf("%s: the page over the bound is a %s input with %d entries from the large conversation=%t, want its one large entry",
				test.label, input.Kind, len(input.TranscriptEntries), input.TranscriptID == large)
		}
		body := input.TranscriptEntries[0].Body
		if len(body) > 16*1024+128 || !strings.Contains(body, "witself:elided") {
			t.Fatalf("%s: the large entry kept %d body bytes, want at most the 16 KiB prefix plus an elision note",
				test.label, len(body))
		}
	}

	// The bound is per request: an unbounded read after the bounded ones is
	// one page again, and the run carries nothing of it.
	_, againPages, _ := readAll(0)
	if againPages != 1 {
		t.Fatalf("pages under the server budget after bounded reads = %d, want 1", againPages)
	}
	run, err := st.GetCurationRun(ctx, p, started.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.FencingGeneration != started.Run.FencingGeneration || run.InputCount != started.Run.InputCount ||
		!bytes.Equal(run.Budgets, started.Run.Budgets) || strings.Contains(string(run.Budgets), "max_bytes") {
		t.Fatalf("run changed after bounded reads: fence %d inputs %d budgets %d bytes",
			run.FencingGeneration, run.InputCount, len(run.Budgets))
	}

	// Both ends of the range are closed; outside it the read is refused
	// before any page is built.
	for _, maxBytes := range []int{smallest - 1, largest + 1, -1} {
		if _, err := st.GetCurationRunInputPage(ctx, p, started.Run.ID, MemoryCurationRunInputOptions{
			FencingGeneration: started.Run.FencingGeneration, Limit: maxMemoryCurationPageSize,
			MaxBytes: maxBytes,
		}); !errors.Is(err, ErrMemoryCurationInputInvalid) {
			t.Fatalf("max_bytes %d: error = %v, want invalid input", maxBytes, err)
		}
	}
	for _, maxBytes := range []int{smallest, largest} {
		page, err := st.GetCurationRunInputPage(ctx, p, started.Run.ID, MemoryCurationRunInputOptions{
			FencingGeneration: started.Run.FencingGeneration, Limit: maxMemoryCurationPageSize,
			MaxBytes: maxBytes,
		})
		if err != nil || len(page.Inputs) == 0 {
			t.Fatalf("max_bytes %d: %d inputs, error = %v, want a page", maxBytes, len(page.Inputs), err)
		}
	}
}
