package store

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMemorySupersessionMembershipDigest(t *testing.T) {
	refs := []MemoryVersionReference{
		{MemoryID: "mem_b", Version: 1},
		{MemoryID: "mem_a", Version: 3},
	}
	reordered := []MemoryVersionReference{refs[1], refs[0]}
	got := memorySupersessionMembershipDigest(refs)
	if got != memorySupersessionMembershipDigest(reordered) || !isSHA256Hex(got) {
		t.Fatalf("membership digest = %q / %q", got,
			memorySupersessionMembershipDigest(reordered))
	}
	changed := append([]MemoryVersionReference(nil), refs...)
	changed[1].Version++
	if memorySupersessionMembershipDigest(changed) == got {
		t.Fatal("membership digest ignored an exact version change")
	}
	if validMemorySupersessionReceipt(Memory{
		Operation: "superseded", SupersessionSetID: "mset_1",
		SupersessionSetRevision: 1, SupersessionReplacementCount: 2,
		SupersessionReplacementDigest: strings.Repeat("a", 64),
	}) != true {
		t.Fatal("valid supersession receipt was rejected")
	}
}

func TestNormalizeSupersedeMemoryInput(t *testing.T) {
	in, err := normalizeSupersedeMemoryInput(SupersedeMemoryInput{
		MemoryID:        " mem_source ",
		ExpectedVersion: 2,
		Replacements: []CaptureMemoryInput{{
			Content: "replacement narrative",
			Evidence: []MemoryEvidenceInput{{
				ResolutionState:    MemoryEvidenceUnavailable,
				TerminalReasonCode: "runtime_did_not_record",
			}},
			IdempotencyKey: " replacement-1 ",
		}},
		Reason:         " consolidated ",
		IdempotencyKey: " supersede-1 ",
	})
	if err != nil {
		t.Fatal(err)
	}
	if in.MemoryID != "mem_source" || in.Reason != "consolidated" ||
		in.IdempotencyKey != "supersede-1" || len(in.Replacements) != 1 {
		t.Fatalf("normalized supersession = %#v", in)
	}
	if in.Replacements[0].Kind != "episodic" ||
		in.Replacements[0].IdempotencyKey != "replacement-1" ||
		in.Replacements[0].Salience == nil {
		t.Fatalf("normalized replacement = %#v", in.Replacements[0])
	}
}

func TestNormalizeSupersedeMemoryInputConcurrentReuseDoesNotMutateCaller(t *testing.T) {
	salience := 0.75
	zone := time.FixedZone("test", -7*60*60)
	occurredFrom := time.Date(2026, time.July, 14, 8, 30, 0, 0, zone)
	occurredUntil := occurredFrom.Add(15 * time.Minute)
	in := SupersedeMemoryInput{
		MemoryID:        " mem_source ",
		ExpectedVersion: 2,
		Replacements: []CaptureMemoryInput{{
			Content:       "replacement narrative",
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
			Client: MemoryClientProvenance{
				Runtime: " codex ",
			},
			IdempotencyKey: " replacement-1 ",
		}},
		Reason:         " consolidated ",
		Client:         MemoryClientProvenance{Runtime: " codex "},
		IdempotencyKey: " supersede-1 ",
	}

	const workers = 16
	start := make(chan struct{})
	results := make(chan SupersedeMemoryInput, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			<-start
			out, err := normalizeSupersedeMemoryInput(in)
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
		if out.MemoryID != "mem_source" || out.Replacements[0].Evidence[0].Type != "artifact" {
			t.Fatalf("normalized shared input = %#v", out)
		}
		// The normalized result must not retain aliases to caller-owned nested
		// slices or pointers.
		out.Replacements[0].Tags[0] = "changed"
		out.Replacements[0].Links[0] = "changed"
		*out.Replacements[0].Salience = 0
		*out.Replacements[0].OccurredFrom = time.Time{}
		*out.Replacements[0].OccurredUntil = time.Time{}
		out.Replacements[0].Evidence[0].ArtifactExcerpt[0] = 'X'
	}

	replacement := in.Replacements[0]
	if in.MemoryID != " mem_source " || in.Reason != " consolidated " ||
		in.Client.Runtime != " codex " || in.IdempotencyKey != " supersede-1 " {
		t.Fatalf("top-level caller input was mutated: %#v", in)
	}
	if replacement.IdempotencyKey != " replacement-1 " ||
		replacement.Client.Runtime != " codex " ||
		replacement.Evidence[0].Type != " artifact " ||
		replacement.Evidence[0].ResolvedKind != " artifact " {
		t.Fatalf("replacement caller input was mutated: %#v", replacement)
	}
	if got := replacement.Tags; len(got) != 2 || got[0] != " beta " || got[1] != "alpha" {
		t.Fatalf("caller tags = %#v", got)
	}
	if got := replacement.Links; len(got) != 2 || got[0] != " https://example.com/b " || got[1] != "https://example.com/a" {
		t.Fatalf("caller links = %#v", got)
	}
	if *replacement.Salience != 0.75 || !replacement.OccurredFrom.Equal(occurredFrom) ||
		!replacement.OccurredUntil.Equal(occurredUntil) ||
		string(replacement.Evidence[0].ArtifactExcerpt) != "source excerpt" {
		t.Fatalf("caller nested values were mutated: %#v", replacement)
	}
}

func TestNormalizeSupersedeMemoryInputRejectsInvalidSets(t *testing.T) {
	validReplacement := CaptureMemoryInput{
		Content: "replacement",
		Evidence: []MemoryEvidenceInput{{
			ResolutionState:    MemoryEvidenceUnavailable,
			TerminalReasonCode: "runtime_did_not_record",
		}},
		IdempotencyKey: "replacement-1",
	}
	tests := []struct {
		name string
		in   SupersedeMemoryInput
	}{
		{
			name: "missing source",
			in: SupersedeMemoryInput{ExpectedVersion: 1,
				Replacements: []CaptureMemoryInput{validReplacement}, IdempotencyKey: "supersede-1"},
		},
		{
			name: "missing expected version",
			in: SupersedeMemoryInput{MemoryID: "mem_source",
				Replacements: []CaptureMemoryInput{validReplacement}, IdempotencyKey: "supersede-1"},
		},
		{
			name: "empty replacement set",
			in: SupersedeMemoryInput{MemoryID: "mem_source", ExpectedVersion: 1,
				IdempotencyKey: "supersede-1"},
		},
		{
			name: "operation key reused by replacement",
			in: SupersedeMemoryInput{MemoryID: "mem_source", ExpectedVersion: 1,
				Replacements: []CaptureMemoryInput{func() CaptureMemoryInput {
					out := validReplacement
					out.IdempotencyKey = "supersede-1"
					return out
				}()}, IdempotencyKey: "supersede-1"},
		},
		{
			name: "duplicate replacement keys",
			in: SupersedeMemoryInput{MemoryID: "mem_source", ExpectedVersion: 1,
				Replacements:   []CaptureMemoryInput{validReplacement, validReplacement},
				IdempotencyKey: "supersede-1"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := normalizeSupersedeMemoryInput(tc.in); !errors.Is(err, ErrMemoryInputInvalid) {
				t.Fatalf("error = %v, want ErrMemoryInputInvalid", err)
			}
		})
	}
}
