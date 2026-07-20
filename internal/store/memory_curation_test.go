package store

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestNormalizeRequestMemoryCurationInputIsCanonical(t *testing.T) {
	due := time.Date(2026, 7, 14, 9, 30, 0, 0, time.FixedZone("local", -6*60*60))
	in, err := normalizeRequestMemoryCurationInput(RequestMemoryCurationInput{
		Scope: MemoryCurationScope{
			Sources:      []string{"transcript", "memory", "memory", "evidence"},
			MemoryStates: []string{MemoryStateForgotten, MemoryStateActive},
		},
		TriggerReason: "session_end", DueAt: &due, IdempotencyKey: "request-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if in.CoalescingKey != "owner" || in.MaxAttempts != defaultMemoryCurationAttempts {
		t.Fatalf("defaults = %#v", in)
	}
	if got, want := in.Scope.Sources, []string{"evidence", "memory", "transcript"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("sources = %v, want %v", got, want)
	}
	if got, want := in.Scope.MemoryStates, []string{MemoryStateActive, MemoryStateForgotten}; !reflect.DeepEqual(got, want) {
		t.Fatalf("states = %v, want %v", got, want)
	}
	if in.Scope.MaxMemories != defaultMemoryCurationMemories ||
		in.Scope.MaxEvidence != defaultMemoryCurationEvidence ||
		in.Scope.MaxTranscriptEntries != defaultMemoryCurationTranscriptItems {
		t.Fatalf("scope limits = %#v", in.Scope)
	}
	if in.DueAt == nil || in.DueAt.Location() != time.UTC || !in.DueAt.Equal(due) {
		t.Fatalf("due_at = %v", in.DueAt)
	}

	invalid := []RequestMemoryCurationInput{
		{TriggerReason: "", IdempotencyKey: "key"},
		{TriggerReason: "session_end", CoalescingKey: "contains spaces", IdempotencyKey: "key"},
		{TriggerReason: "session_end", Priority: 101, IdempotencyKey: "key"},
		{TriggerReason: "session_end", Scope: MemoryCurationScope{Sources: []string{"model"}}, IdempotencyKey: "key"},
		{TriggerReason: "session_end", IdempotencyKey: ""},
	}
	for i, candidate := range invalid {
		if _, err := normalizeRequestMemoryCurationInput(candidate); !errors.Is(err, ErrMemoryCurationInputInvalid) {
			t.Fatalf("invalid[%d] error = %v", i, err)
		}
	}
}

func TestNormalizeStartMemoryCurationInputCanonicalizesBudgetsAndBounds(t *testing.T) {
	in, err := normalizeStartMemoryCurationInput(StartMemoryCurationInput{
		RequestID: "mcrq_abcdefghijklmnop", IdempotencyKey: "start-1",
		Budgets: json.RawMessage(`{"tokens":1000,"actions":10}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if in.LeaseDuration != defaultMemoryCurationLease || string(in.Budgets) != `{"actions":10,"tokens":1000}` {
		t.Fatalf("normalized = %#v", in)
	}
	if _, err := normalizeStartMemoryCurationInput(StartMemoryCurationInput{
		RequestID: "mcrq_abcdefghijklmnop", IdempotencyKey: "start-2",
		LeaseDuration: minMemoryCurationLease - time.Second,
	}); !errors.Is(err, ErrMemoryCurationInputInvalid) {
		t.Fatalf("short lease error = %v", err)
	}
	if _, err := normalizeStartMemoryCurationInput(StartMemoryCurationInput{
		RequestID: "mcrq_abcdefghijklmnop", IdempotencyKey: "start-3",
		Caps: MemoryCurationInputCaps{MaxEvidence: maxMemoryCurationEvidence + 1},
	}); !errors.Is(err, ErrMemoryCurationInputInvalid) {
		t.Fatalf("oversized cap error = %v", err)
	}
	if _, err := normalizeStartMemoryCurationInput(StartMemoryCurationInput{
		RequestID: "mcrq_abcdefghijklmnop", IdempotencyKey: "start-4",
		Budgets: json.RawMessage(`{"tokens":10,"tokens":20}`),
	}); !errors.Is(err, ErrMemoryCurationInputInvalid) {
		t.Fatalf("duplicate budget key error = %v", err)
	}
	if validCurationID("mcrq_abcdefghijklmnop_extra", "mcrq") ||
		validCurationID("mcrq_abcdefghijklmno1", "mcrq") {
		t.Fatal("curation id validator accepted a noncanonical id")
	}
}

func TestMemoryCurationInputCursorIsBoundToRunAndFence(t *testing.T) {
	cursor, err := encodeMemoryCurationInputCursor("mrun_abcdefghijklmnop", 7, 42)
	if err != nil {
		t.Fatal(err)
	}
	after, err := decodeMemoryCurationInputCursor(cursor, "mrun_abcdefghijklmnop", 7)
	if err != nil || after != 42 {
		t.Fatalf("decode = %d / %v", after, err)
	}
	for _, tc := range []struct {
		run   string
		fence int64
	}{
		{"mrun_other", 7},
		{"mrun_abcdefghijklmnop", 8},
	} {
		if _, err := decodeMemoryCurationInputCursor(cursor, tc.run, tc.fence); !errors.Is(err, ErrMemoryCurationInputInvalid) {
			t.Fatalf("cross-binding decode error = %v", err)
		}
	}
	if _, err := decodeMemoryCurationInputCursor("not-base64", "mrun_abcdefghijklmnop", 7); !errors.Is(err, ErrMemoryCurationInputInvalid) {
		t.Fatalf("malformed cursor error = %v", err)
	}
}

func TestRestrictedMemoryCuratorsTreatTranscriptScopesAsSensitive(t *testing.T) {
	full := Principal{Kind: PrincipalAgent, AccessProfile: AccessProfileFull}
	preview := full
	preview.AccessProfile = AccessProfileCuratorPreview
	apply := full
	apply.AccessProfile = AccessProfileCuratorApply

	memoryRequest := MemoryCurationRequest{Scope: MemoryCurationScope{
		Sources: []string{MemoryCurationSourceMemory}, IncludeSensitive: false,
	}}
	transcriptRequest := MemoryCurationRequest{Scope: MemoryCurationScope{
		Sources: []string{MemoryCurationSourceTranscript}, IncludeSensitive: false,
	}}
	sensitiveRequest := MemoryCurationRequest{Scope: MemoryCurationScope{
		Sources: []string{MemoryCurationSourceMemory}, IncludeSensitive: true,
	}}

	if err := authorizeMemoryCurationRequestScope(full, transcriptRequest); err != nil {
		t.Fatalf("full transcript authorization = %v", err)
	}
	for _, restricted := range []Principal{preview, apply} {
		if err := authorizeMemoryCurationRequestScope(restricted, memoryRequest); err != nil {
			t.Fatalf("%s memory authorization = %v", restricted.AccessProfile, err)
		}
		if err := authorizeMemoryCurationRequestScope(restricted, transcriptRequest); !errors.Is(err, ErrMemoryCurationForbidden) {
			t.Fatalf("%s transcript authorization = %v", restricted.AccessProfile, err)
		}
		if err := authorizeMemoryCurationRequestScope(restricted, sensitiveRequest); !errors.Is(err, ErrMemoryCurationForbidden) {
			t.Fatalf("%s sensitive authorization = %v", restricted.AccessProfile, err)
		}
	}
}

func TestEffectiveMemoryCurationCapsCanOnlyNarrowRequest(t *testing.T) {
	scope := MemoryCurationScope{MaxMemories: 100, MaxEvidence: 200, MaxTranscriptEntries: 300}
	got := effectiveMemoryCurationCaps(scope, MemoryCurationInputCaps{
		MaxMemories: 50, MaxEvidence: 400,
	})
	want := MemoryCurationInputCaps{MaxMemories: 50, MaxEvidence: 200, MaxTranscriptEntries: 300}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("caps = %#v, want %#v", got, want)
	}
}

func TestMemoryCurationBackoffIsBounded(t *testing.T) {
	if got := curationBackoff(1); got != time.Minute {
		t.Fatalf("first backoff = %s", got)
	}
	if got := curationBackoff(100); got > maxMemoryCurationBackoff {
		t.Fatalf("large backoff = %s", got)
	}
}

func TestChunkMemoryCurationTranscriptWindowPreservesCoverage(t *testing.T) {
	size := func(sequence, bytes int64) memoryCurationTranscriptEntrySize {
		return memoryCurationTranscriptEntrySize{Sequence: sequence, Bytes: bytes}
	}
	cases := []struct {
		name        string
		sizes       []memoryCurationTranscriptEntrySize
		from, until int64
		want        []memoryCurationTranscriptSlice
	}{
		{
			name: "no sizes keeps one window",
			from: 3, until: 9,
			want: []memoryCurationTranscriptSlice{{From: 3, Until: 9}},
		},
		{
			name:  "small entries keep one window",
			sizes: []memoryCurationTranscriptEntrySize{size(1, 10), size(2, 10)},
			from:  1, until: 2,
			want: []memoryCurationTranscriptSlice{{From: 1, Until: 2}},
		},
		{
			name: "budget splits at sized entries",
			sizes: []memoryCurationTranscriptEntrySize{
				size(1, 60), size(2, 60), size(3, 60), size(4, 10), size(5, 10),
			},
			from: 1, until: 5,
			want: []memoryCurationTranscriptSlice{
				{From: 1, Until: 1}, {From: 2, Until: 2}, {From: 3, Until: 5},
			},
		},
		{
			name:  "an entry over budget still gets one slice",
			sizes: []memoryCurationTranscriptEntrySize{size(1, 250), size(2, 10)},
			from:  1, until: 2,
			want: []memoryCurationTranscriptSlice{
				{From: 1, Until: 1}, {From: 2, Until: 2},
			},
		},
		{
			name:  "gaps stay covered by adjacent slices",
			sizes: []memoryCurationTranscriptEntrySize{size(3, 200), size(7, 200)},
			from:  1, until: 10,
			want: []memoryCurationTranscriptSlice{
				{From: 1, Until: 3}, {From: 4, Until: 10},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := chunkMemoryCurationTranscriptWindow(tc.sizes, tc.from, tc.until, 100)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("slices = %#v, want %#v", got, tc.want)
			}
			next := tc.from
			for _, slice := range got {
				if slice.From != next || slice.Until < slice.From {
					t.Fatalf("slices are not contiguous: %#v", got)
				}
				next = slice.Until + 1
			}
			if next != tc.until+1 {
				t.Fatalf("slices end at %d, want %d", next-1, tc.until)
			}
		})
	}
}

func TestBoundMemoryCurationTranscriptEntriesElidesOversizedContent(t *testing.T) {
	t.Run("small entries are untouched", func(t *testing.T) {
		entries := []TranscriptEntry{{
			Sequence: 1, Body: "hello",
			Payload:   json.RawMessage(`{"tool":"ok"}`),
			Artifacts: json.RawMessage(`[]`),
		}}
		boundMemoryCurationTranscriptEntries(entries)
		if entries[0].Body != "hello" || string(entries[0].Payload) != `{"tool":"ok"}` ||
			string(entries[0].Artifacts) != `[]` {
			t.Fatalf("entry mutated: %#v", entries[0])
		}
	})
	t.Run("an oversized body keeps a valid prefix and a note", func(t *testing.T) {
		body := strings.Repeat("é", 20000)
		entries := []TranscriptEntry{{Sequence: 1, Body: body}}
		boundMemoryCurationTranscriptEntries(entries)
		bounded := entries[0].Body
		if len(bounded) > maxMemoryCurationEntryBodyBytes+256 {
			t.Fatalf("bounded body bytes = %d", len(bounded))
		}
		if !utf8.ValidString(bounded) {
			t.Fatal("bounded body is not valid UTF-8")
		}
		if !strings.Contains(bounded, "witself:elided") {
			t.Fatalf("bounded body lacks elision note: %q", bounded[len(bounded)-120:])
		}
	})
	t.Run("a spent budget leaves only notes and stubs", func(t *testing.T) {
		entries := make([]TranscriptEntry, 24)
		for i := range entries {
			entries[i] = TranscriptEntry{
				Sequence:  int64(i + 1),
				Body:      strings.Repeat("b", 60000),
				Payload:   json.RawMessage(`{"tool":"` + strings.Repeat("p", 15000) + `"}`),
				Artifacts: json.RawMessage(`[{"a":"` + strings.Repeat("x", 1000) + `"}]`),
			}
		}
		boundMemoryCurationTranscriptEntries(entries)
		total := 0
		for _, entry := range entries {
			total += len(entry.Body) + len(entry.Payload) + len(entry.Artifacts)
		}
		if limit := maxMemoryCurationInputBytes + len(entries)*512; total > limit {
			t.Fatalf("bounded input bytes = %d, want <= %d", total, limit)
		}
		last := entries[len(entries)-1]
		if !strings.Contains(last.Body, "witself:elided") ||
			!strings.Contains(string(last.Payload), "witself_elided") ||
			!strings.Contains(string(last.Artifacts), "witself_elided") {
			t.Fatalf("late entry not elided: %#v", last)
		}
		if len(entries) != 24 || entries[11].Sequence != 12 {
			t.Fatal("entry membership changed")
		}
	})
}
