package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestRedactTranscriptEntriesInputValidation(t *testing.T) {
	valid := RedactTranscriptEntriesInput{TranscriptID: " trn_fixture ", FromSequence: 1, UntilSequence: 100}
	if got, err := normalizeRedactTranscriptEntriesInput(valid); err != nil || got.TranscriptID != "trn_fixture" {
		t.Fatal("valid preview was not normalized")
	}
	for _, tc := range []struct {
		name   string
		change func(*RedactTranscriptEntriesInput)
	}{
		{"empty-id", func(in *RedactTranscriptEntriesInput) { in.TranscriptID = " " }},
		{"zero", func(in *RedactTranscriptEntriesInput) { in.FromSequence = 0 }},
		{"backward", func(in *RedactTranscriptEntriesInput) { in.UntilSequence = 0 }},
		{"over-cap", func(in *RedactTranscriptEntriesInput) { in.UntilSequence = 101 }},
		{"preview-key", func(in *RedactTranscriptEntriesInput) { in.IdempotencyKey = " " }},
		{"preview-revision", func(in *RedactTranscriptEntriesInput) { in.ExpectedRedactionSetRevision = "x" }},
		{"apply-no-key", func(in *RedactTranscriptEntriesInput) {
			in.Apply = true
			in.ExpectedRedactionSetRevision = strings.Repeat("a", 64)
		}},
		{"apply-space-key", func(in *RedactTranscriptEntriesInput) {
			in.Apply = true
			in.ExpectedRedactionSetRevision = strings.Repeat("a", 64)
			in.IdempotencyKey = " "
		}},
		{"apply-long-key", func(in *RedactTranscriptEntriesInput) {
			in.Apply = true
			in.ExpectedRedactionSetRevision = strings.Repeat("a", 64)
			in.IdempotencyKey = strings.Repeat("x", 513)
		}},
		{"apply-bad-revision", func(in *RedactTranscriptEntriesInput) {
			in.Apply = true
			in.ExpectedRedactionSetRevision = "bad"
			in.IdempotencyKey = "key"
		}},
		{"apply-upper-revision", func(in *RedactTranscriptEntriesInput) {
			in.Apply = true
			in.ExpectedRedactionSetRevision = strings.Repeat("A", 64)
			in.IdempotencyKey = "key"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := valid
			tc.change(&in)
			if _, err := normalizeRedactTranscriptEntriesInput(in); !errors.Is(err, ErrTranscriptInputInvalid) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	valid.Apply = true
	valid.ExpectedRedactionSetRevision = strings.Repeat("b", 64)
	valid.IdempotencyKey = " key "
	if got, err := normalizeRedactTranscriptEntriesInput(valid); err != nil || got.IdempotencyKey != "key" {
		t.Fatal("apply key was not trimmed")
	}
	// Agent refusal precedes normalization and every database lookup.
	if _, err := (*Store)(nil).RedactTranscriptEntries(context.Background(), Principal{Kind: PrincipalAgent}, RedactTranscriptEntriesInput{}); !errors.Is(err, ErrTranscriptForbidden) {
		t.Fatal("agent did not fail before lookup")
	}
}

func TestTranscriptRedactionRevisionAndReceiptID(t *testing.T) {
	in := RedactTranscriptEntriesInput{TranscriptID: "trn_fixture", FromSequence: 1, UntilSequence: 2, IdempotencyKey: "key"}
	entries := []TranscriptRedactionEntry{{EntryID: "ent_a", Sequence: 1}, {EntryID: "ent_b", Sequence: 2}}
	revision := transcriptRedactionSetRevision(in, entries)
	if !isSHA256Hex(revision) || revision != transcriptRedactionSetRevision(in, entries) {
		t.Fatal("revision is malformed or nondeterministic")
	}
	entries[0].Redacted = true
	if transcriptRedactionSetRevision(in, entries) == revision {
		t.Fatal("revision ignored redaction state")
	}
	entries[0].Redacted = false
	entries[0].EntryID = "ent_c"
	if transcriptRedactionSetRevision(in, entries) == revision {
		t.Fatal("revision ignored identity")
	}
	in.ExpectedRedactionSetRevision = revision
	receipt := transcriptRedactionReceiptID("acc_fixture", in)
	if !transcriptRedactionReceiptPattern.MatchString(receipt) || receipt != transcriptRedactionReceiptID("acc_fixture", in) {
		t.Fatal("receipt is malformed or nondeterministic")
	}
	in.IdempotencyKey = " key "
	if receipt != transcriptRedactionReceiptID("acc_fixture", in) {
		t.Fatal("receipt did not trim retry key")
	}
	in.IdempotencyKey = "other"
	if receipt == transcriptRedactionReceiptID("acc_fixture", in) {
		t.Fatal("receipt ignored key")
	}
	in.IdempotencyKey = "key"
	in.ExpectedRedactionSetRevision = strings.Repeat("d", 64)
	if receipt == transcriptRedactionReceiptID("acc_fixture", in) {
		t.Fatal("receipt ignored revision")
	}
}
