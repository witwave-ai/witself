package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestNormalizeCreateTranscriptInput(t *testing.T) {
	in, err := normalizeCreateTranscriptInput(CreateTranscriptInput{
		ExternalID: "  vendor-thread-1  ",
		Title:      "  Incident review  ",
	})
	if err != nil {
		t.Fatal(err)
	}
	if in.ExternalID != "vendor-thread-1" || in.Title != "Incident review" {
		t.Fatalf("normalized strings = %q / %q", in.ExternalID, in.Title)
	}
	if string(in.Metadata) != "{}" {
		t.Fatalf("default metadata = %s, want {}", in.Metadata)
	}

	for _, tc := range []struct {
		name string
		in   CreateTranscriptInput
	}{
		{name: "metadata array", in: CreateTranscriptInput{Metadata: json.RawMessage(`[]`)}},
		{name: "metadata null", in: CreateTranscriptInput{Metadata: json.RawMessage(`null`)}},
		{name: "title too large", in: CreateTranscriptInput{Title: strings.Repeat("x", maxTranscriptTitleBytes+1)}},
		{name: "external id too large", in: CreateTranscriptInput{ExternalID: strings.Repeat("x", maxTranscriptExternalIDBytes+1)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := normalizeCreateTranscriptInput(tc.in)
			if !errors.Is(err, ErrTranscriptInputInvalid) {
				t.Fatalf("error = %v, want ErrTranscriptInputInvalid", err)
			}
		})
	}
}

func TestNormalizeAppendTranscriptEntryInput(t *testing.T) {
	in, err := normalizeAppendTranscriptEntryInput(AppendTranscriptEntryInput{
		ExternalID: "  vendor-message-1  ",
		Role:       " assistant ",
		Payload:    json.RawMessage(`{"b":2,"a":1}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if in.Role != TranscriptRoleAssistant {
		t.Fatalf("role = %q", in.Role)
	}
	if in.ExternalID != "vendor-message-1" {
		t.Fatalf("external id = %q", in.ExternalID)
	}
	if string(in.Payload) != `{"a":1,"b":2}` {
		t.Fatalf("canonical payload = %s", in.Payload)
	}
	if string(in.Artifacts) != "[]" {
		t.Fatalf("default artifacts = %s", in.Artifacts)
	}
	withReply, err := normalizeAppendTranscriptEntryInput(AppendTranscriptEntryInput{
		Role: "assistant", Body: "done", ReplyToExternalID: "  evt_prompt:0  ",
	})
	if err != nil {
		t.Fatal(err)
	}
	if withReply.ReplyToExternalID != "evt_prompt:0" {
		t.Fatalf("reply external id = %q", withReply.ReplyToExternalID)
	}

	tests := []struct {
		name string
		in   AppendTranscriptEntryInput
		want string
	}{
		{name: "unknown role", in: AppendTranscriptEntryInput{Role: "developer", Body: "x"}, want: "role must be"},
		{name: "no content", in: AppendTranscriptEntryInput{Role: "user"}, want: "body or payload"},
		{name: "payload array", in: AppendTranscriptEntryInput{Role: "user", Payload: json.RawMessage(`[]`)}, want: "payload must be"},
		{name: "null artifacts", in: AppendTranscriptEntryInput{Role: "user", Body: "x", Artifacts: json.RawMessage(`null`)}, want: "artifacts must be"},
		{name: "nonempty artifacts", in: AppendTranscriptEntryInput{Role: "user", Body: "x", Artifacts: json.RawMessage(`[{"name":"report.pdf"}]`)}, want: "object storage"},
		{name: "body too large", in: AppendTranscriptEntryInput{Role: "user", Body: strings.Repeat("x", maxTranscriptBodyBytes+1)}, want: "body exceeds"},
		{name: "external id too large", in: AppendTranscriptEntryInput{ExternalID: strings.Repeat("x", maxTranscriptExternalIDBytes+1), Role: "user", Body: "x"}, want: "external_id exceeds"},
		{name: "two reply targets", in: AppendTranscriptEntryInput{Role: "assistant", Body: "x", ReplyToEntryID: "ent_1", ReplyToExternalID: "evt_1"}, want: "one reply target"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := normalizeAppendTranscriptEntryInput(tc.in)
			if !errors.Is(err, ErrTranscriptInputInvalid) {
				t.Fatalf("error = %v, want ErrTranscriptInputInvalid", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestNormalizeTranscriptPageOptions(t *testing.T) {
	opts, err := normalizeTranscriptPageOptions(TranscriptPageOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if opts.Limit != defaultTranscriptPageSize {
		t.Fatalf("default limit = %d", opts.Limit)
	}
	for _, opts := range []TranscriptPageOptions{
		{AfterSequence: -1},
		{Limit: maxTranscriptPageSize + 1},
		{AfterSequence: 1, Tail: true},
	} {
		if _, err := normalizeTranscriptPageOptions(opts); !errors.Is(err, ErrTranscriptInputInvalid) {
			t.Fatalf("options %#v error = %v", opts, err)
		}
	}
}

func TestTranscriptEntryRetryComparisonUsesJSONSemantics(t *testing.T) {
	entry := TranscriptEntry{
		Role: "tool", Body: "done", Model: "model",
		Payload: json.RawMessage(`{"a":1,"b":2}`), Artifacts: json.RawMessage(`[]`),
	}
	in := AppendTranscriptEntryInput{
		Role: "tool", Body: "done", Model: "model",
		Payload: json.RawMessage(`{"b":2,"a":1}`), Artifacts: json.RawMessage(`[]`),
	}
	if !transcriptEntryMatches(entry, in) {
		t.Fatal("semantically identical JSON did not match")
	}
	in.Body = "different"
	if transcriptEntryMatches(entry, in) {
		t.Fatal("different retry content matched")
	}
}
