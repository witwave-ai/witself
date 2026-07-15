package store

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNormalizeCaptureMemoryInput(t *testing.T) {
	in, err := normalizeCaptureMemoryInput(CaptureMemoryInput{
		Content: "decision narrative",
		Tags:    []string{" architecture ", "decision", "architecture"},
		Links:   []string{"witself://fact/fact_1"},
		Evidence: []MemoryEvidenceInput{{
			ResolutionState: MemoryEvidencePending,
			ExternalLocator: " vendor://conversation/1 ",
		}},
		IdempotencyKey: " capture-1 ",
	})
	if err != nil {
		t.Fatal(err)
	}
	if in.Kind != "episodic" || in.Origin != "self" ||
		in.ContentEncoding != "plain" || *in.Salience != 0.5 {
		t.Fatalf("defaults = %#v", in)
	}
	if len(in.Tags) != 2 || in.Tags[0] != "architecture" ||
		in.IdempotencyKey != "capture-1" {
		t.Fatalf("normalized capture = %#v", in)
	}
	if in.Evidence[0].Type != "conversation" ||
		in.Evidence[0].Role != MemoryEvidenceSupports {
		t.Fatalf("evidence defaults = %#v", in.Evidence[0])
	}

	for _, tc := range []struct {
		name string
		in   CaptureMemoryInput
	}{
		{name: "missing content", in: CaptureMemoryInput{IdempotencyKey: "x", Evidence: []MemoryEvidenceInput{{Type: "conversation", ResolutionState: MemoryEvidenceUnavailable, TerminalReasonCode: "not_recorded"}}}},
		{name: "missing evidence", in: CaptureMemoryInput{Content: "x", IdempotencyKey: "x"}},
		{name: "missing key", in: CaptureMemoryInput{Content: "x", Evidence: []MemoryEvidenceInput{{Type: "conversation", ResolutionState: MemoryEvidenceUnavailable, TerminalReasonCode: "not_recorded"}}}},
		{name: "unresolvable capture", in: CaptureMemoryInput{Content: "x", IdempotencyKey: "x", Evidence: []MemoryEvidenceInput{{Type: "conversation", ResolutionState: MemoryEvidenceUnresolvable, TerminalReasonCode: "gone"}}}},
		{name: "bad base64", in: CaptureMemoryInput{Content: "%%%", ContentEncoding: "base64", IdempotencyKey: "x", Evidence: []MemoryEvidenceInput{{Type: "conversation", ResolutionState: MemoryEvidenceUnavailable, TerminalReasonCode: "not_recorded"}}}},
		{name: "noncanonical base64", in: CaptureMemoryInput{Content: "Zh==", ContentEncoding: "base64", IdempotencyKey: "x", Evidence: []MemoryEvidenceInput{{Type: "conversation", ResolutionState: MemoryEvidenceUnavailable, TerminalReasonCode: "not_recorded"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := normalizeCaptureMemoryInput(tc.in); !errors.Is(err, ErrMemoryInputInvalid) {
				t.Fatalf("error = %v, want ErrMemoryInputInvalid", err)
			}
		})
	}
}

func TestNormalizeCaptureMemoryInputConcurrentReuseDoesNotMutateCaller(t *testing.T) {
	salience := 0.75
	zone := time.FixedZone("test", -7*60*60)
	occurredFrom := time.Date(2026, time.July, 14, 8, 30, 0, 0, zone)
	occurredUntil := occurredFrom.Add(15 * time.Minute)
	in := CaptureMemoryInput{
		Content:       "decision narrative",
		Tags:          []string{" beta ", "alpha"},
		Links:         []string{" https://example.com/b ", "https://example.com/a"},
		Salience:      &salience,
		OccurredFrom:  &occurredFrom,
		OccurredUntil: &occurredUntil,
		Evidence: []MemoryEvidenceInput{{
			Type:              " artifact ",
			ResolutionState:   MemoryEvidenceResolved,
			ResolvedKind:      " artifact ",
			ArtifactExcerpt:   []byte("source excerpt"),
			ArtifactSensitive: true,
		}},
		Client:         MemoryClientProvenance{Runtime: " codex "},
		IdempotencyKey: " capture-1 ",
	}

	const workers = 16
	start := make(chan struct{})
	results := make(chan CaptureMemoryInput, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			<-start
			out, err := normalizeCaptureMemoryInput(in)
			results <- out
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("normalize shared input: %v", err)
		}
	}
	for out := range results {
		if out.IdempotencyKey != "capture-1" || out.Evidence[0].Type != "artifact" {
			t.Fatalf("normalized shared input = %#v", out)
		}
		// The normalized result must not retain aliases to caller-owned nested
		// slices or pointers.
		out.Tags[0] = "changed"
		out.Links[0] = "changed"
		*out.Salience = 0
		*out.OccurredFrom = time.Time{}
		*out.OccurredUntil = time.Time{}
		out.Evidence[0].ArtifactExcerpt[0] = 'X'
	}

	if in.IdempotencyKey != " capture-1 " || in.Client.Runtime != " codex " ||
		in.Evidence[0].Type != " artifact " || in.Evidence[0].ResolvedKind != " artifact " {
		t.Fatalf("caller input was mutated: %#v", in)
	}
	if got := in.Tags; len(got) != 2 || got[0] != " beta " || got[1] != "alpha" {
		t.Fatalf("caller tags = %#v", got)
	}
	if got := in.Links; len(got) != 2 || got[0] != " https://example.com/b " || got[1] != "https://example.com/a" {
		t.Fatalf("caller links = %#v", got)
	}
	if *in.Salience != 0.75 || !in.OccurredFrom.Equal(occurredFrom) ||
		!in.OccurredUntil.Equal(occurredUntil) ||
		string(in.Evidence[0].ArtifactExcerpt) != "source excerpt" {
		t.Fatalf("caller nested values were mutated: %#v", in)
	}
}

func TestNormalizeMemoryEvidenceShapes(t *testing.T) {
	tests := []struct {
		name string
		in   MemoryEvidenceInput
		ok   bool
	}{
		{name: "pending", in: MemoryEvidenceInput{Type: "conversation", ResolutionState: MemoryEvidencePending, ExternalLocator: "vendor://1"}, ok: true},
		{name: "unavailable", in: MemoryEvidenceInput{Type: "conversation", ResolutionState: MemoryEvidenceUnavailable, TerminalReasonCode: "not_recorded"}, ok: true},
		{name: "resolved transcript", in: MemoryEvidenceInput{Type: "conversation", ResolutionState: MemoryEvidenceResolved, ResolvedKind: "transcript", SourceTranscriptID: "trn_1", SourceSequenceFrom: 1, SourceSequenceUntil: 2}, ok: true},
		{name: "resolved memory", in: MemoryEvidenceInput{Type: "memory", ResolutionState: MemoryEvidenceResolved, ResolvedKind: "memory", SourceMemoryID: "mem_1", SourceMemoryVersion: 1}, ok: true},
		{name: "pending plus source", in: MemoryEvidenceInput{Type: "conversation", ResolutionState: MemoryEvidencePending, ExternalLocator: "vendor://1", SourceMessageID: "msg_1"}},
		{name: "resolved two sources", in: MemoryEvidenceInput{Type: "conversation", ResolutionState: MemoryEvidenceResolved, ResolvedKind: "transcript", SourceTranscriptID: "trn_1", SourceSequenceFrom: 1, SourceSequenceUntil: 1, SourceMessageID: "msg_1"}},
		{name: "reversed transcript range", in: MemoryEvidenceInput{Type: "conversation", ResolutionState: MemoryEvidenceResolved, ResolvedKind: "transcript", SourceTranscriptID: "trn_1", SourceSequenceFrom: 2, SourceSequenceUntil: 1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := normalizeMemoryEvidenceInput(tc.in)
			if tc.ok && err != nil {
				t.Fatal(err)
			}
			if !tc.ok && !errors.Is(err, ErrMemoryInputInvalid) {
				t.Fatalf("error = %v, want ErrMemoryInputInvalid", err)
			}
		})
	}
}

func TestMemoryListCursorRoundTrip(t *testing.T) {
	wantTime := time.Date(2026, 7, 14, 7, 1, 2, 345, time.FixedZone("offset", -6*60*60))
	cursor, err := encodeMemoryListCursor(wantTime, "mem_abc")
	if err != nil {
		t.Fatal(err)
	}
	gotTime, gotID, bySalience, salience, err := decodeMemoryListCursor(cursor)
	if err != nil {
		t.Fatal(err)
	}
	if !gotTime.Equal(wantTime) || gotID != "mem_abc" || bySalience || salience != nil {
		t.Fatalf("cursor = %v / %q", gotTime, gotID)
	}
	if _, _, _, _, err := decodeMemoryListCursor("not a cursor"); !errors.Is(err, ErrMemoryInputInvalid) {
		t.Fatalf("bad cursor error = %v", err)
	}
}

func TestMemoryHistoryCursorAndOptions(t *testing.T) {
	cursor, err := encodeMemoryHistoryCursor("mem_abc", 42)
	if err != nil {
		t.Fatal(err)
	}
	memoryID, afterVersion, err := decodeMemoryHistoryCursor(cursor)
	if err != nil {
		t.Fatal(err)
	}
	if memoryID != "mem_abc" || afterVersion != 42 {
		t.Fatalf("cursor = %q / %d", memoryID, afterVersion)
	}

	opts, err := normalizeMemoryHistoryOptions("mem_abc", MemoryHistoryOptions{
		Cursor: cursor,
	})
	if err != nil {
		t.Fatal(err)
	}
	if opts.Limit != defaultMemoryPageSize || opts.AfterVersion != 42 {
		t.Fatalf("normalized options = %#v", opts)
	}
	for _, tc := range []MemoryHistoryOptions{
		{Limit: maxMemoryPageSize + 1},
		{Cursor: "not-a-cursor"},
		{Cursor: cursor, AfterVersion: 1},
		{AfterVersion: -1},
	} {
		if _, err := normalizeMemoryHistoryOptions("mem_abc", tc); !errors.Is(err, ErrMemoryInputInvalid) {
			t.Fatalf("normalize %#v error = %v, want ErrMemoryInputInvalid", tc, err)
		}
	}
	if _, err := normalizeMemoryHistoryOptions("mem_other", MemoryHistoryOptions{Cursor: cursor}); !errors.Is(err, ErrMemoryInputInvalid) {
		t.Fatalf("cross-memory cursor error = %v, want ErrMemoryInputInvalid", err)
	}
}

func TestApplyMemoryAdjustment(t *testing.T) {
	current := Memory{
		Content: "before", ContentEncoding: "plain", Kind: "decision",
		Tags: []string{"old"}, Links: []string{"witself://memory/mem_old"},
		Salience: 0.5, State: MemoryStateActive,
	}
	content := "after"
	salience := 0.9
	if err := applyMemoryAdjustment(&current, AdjustMemoryInput{
		Content: &content, AddTags: []string{"new"}, RemoveTags: []string{"old"},
		Salience: &salience,
	}); err != nil {
		t.Fatal(err)
	}
	if current.Content != "after" || current.Salience != 0.9 ||
		len(current.Tags) != 1 || current.Tags[0] != "new" {
		t.Fatalf("adjusted memory = %#v", current)
	}

	tooLarge := strings.Repeat("x", maxMemoryContentBytes+1)
	if err := applyMemoryAdjustment(&current, AdjustMemoryInput{Content: &tooLarge}); !errors.Is(err, ErrMemoryInputInvalid) {
		t.Fatalf("oversize adjustment error = %v", err)
	}
}

func TestRedactMemoryForBroadReadClearsFreeFormValues(t *testing.T) {
	memory := Memory{
		Content: "private body", ContentHash: strings.Repeat("a", 64),
		Tags: []string{"private-tag"}, Links: []string{"witself://private"},
		CaptureReason: "manual", LifecycleReason: "contains private context",
		OccurredFrom: ptrTime(time.Unix(1, 0)), OccurredUntil: ptrTime(time.Unix(2, 0)),
		IdempotencyKey: "private-retry-key", RequestHash: strings.Repeat("b", 64),
		Client:            MemoryClientProvenance{Runtime: "private-runtime", Recipe: "private-recipe"},
		Evidence:          []MemoryEvidence{{ExternalLocator: "private://locator"}},
		SupersessionSetID: "mset_receipt", SupersessionSetRevision: 2,
		SupersessionReplacementCount: 3, SupersessionReplacementDigest: strings.Repeat("c", 64),
		ActiveSupersessionSetID: "mset_active", ActiveSupersessionSetRevision: 4,
	}
	redactMemoryForBroadRead(&memory)
	if !memory.Redacted || memory.Content != "" || memory.ContentHash != "" ||
		len(memory.Tags) != 0 || len(memory.Links) != 0 || memory.CaptureReason != "" ||
		memory.LifecycleReason != "" || memory.OccurredFrom != nil || memory.OccurredUntil != nil ||
		memory.IdempotencyKey != "" || memory.RequestHash != "" ||
		memory.Client != (MemoryClientProvenance{}) || len(memory.Evidence) != 0 {
		t.Fatalf("redacted memory retained a broad-read value = %#v", memory)
	}
	if memory.SupersessionSetID != "mset_receipt" || memory.SupersessionSetRevision != 2 ||
		memory.SupersessionReplacementCount != 3 ||
		memory.SupersessionReplacementDigest != strings.Repeat("c", 64) ||
		memory.ActiveSupersessionSetID != "mset_active" || memory.ActiveSupersessionSetRevision != 4 {
		t.Fatalf("redaction removed value-free supersession metadata = %#v", memory)
	}
}

func ptrTime(value time.Time) *time.Time { return &value }
