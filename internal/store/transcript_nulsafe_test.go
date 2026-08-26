package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// nulEscapeJSON is the six-character JSON NUL escape, built by concatenation
// so no tool or transport layer can collapse it into a real NUL byte.
var nulEscapeJSON = "\\" + "u0000"

func TestNormalizeAppendTranscriptEntrySanitizesNUL(t *testing.T) {
	in, err := normalizeAppendTranscriptEntryInput(AppendTranscriptEntryInput{
		Role:    TranscriptRoleTool,
		Body:    "out\x00put",
		Payload: json.RawMessage(`{"data":"a` + nulEscapeJSON + `b","nested":{"deep":"` + nulEscapeJSON + `"}}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if in.Body != "out�put" {
		t.Fatalf("body = %q, want NUL replaced", in.Body)
	}
	if strings.Contains(string(in.Payload), nulEscapeJSON) {
		t.Fatalf("payload still contains NUL escape: %s", in.Payload)
	}
	var decoded map[string]any
	if err := json.Unmarshal(in.Payload, &decoded); err != nil {
		t.Fatalf("sanitized payload not JSON: %v", err)
	}
	if decoded["data"] != "a�b" {
		t.Fatalf("payload data = %q, want sanitized", decoded["data"])
	}
}

func TestNormalizeAppendTranscriptEntryRejectsNULIdentifiers(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   AppendTranscriptEntryInput
	}{
		{name: "external id", in: AppendTranscriptEntryInput{Role: TranscriptRoleUser, Body: "b", ExternalID: "evt\x001"}},
		{name: "model", in: AppendTranscriptEntryInput{Role: TranscriptRoleUser, Body: "b", Model: "m\x00"}},
		{name: "reply external id", in: AppendTranscriptEntryInput{Role: TranscriptRoleUser, Body: "b", ReplyToExternalID: "evt\x002"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := normalizeAppendTranscriptEntryInput(tc.in); !errors.Is(err, ErrTranscriptInputInvalid) {
				t.Fatalf("error = %v, want ErrTranscriptInputInvalid", err)
			}
		})
	}
}

func TestNormalizeCreateTranscriptSanitizesNUL(t *testing.T) {
	in, err := normalizeCreateTranscriptInput(CreateTranscriptInput{
		Title:    "note\x00title",
		Metadata: json.RawMessage(`{"k":"v` + nulEscapeJSON + `"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if in.Title != "note�title" {
		t.Fatalf("title = %q, want NUL replaced", in.Title)
	}
	if strings.Contains(string(in.Metadata), nulEscapeJSON) {
		t.Fatalf("metadata still contains NUL escape: %s", in.Metadata)
	}
	if _, err := normalizeCreateTranscriptInput(CreateTranscriptInput{ExternalID: "x\x00y"}); !errors.Is(err, ErrTranscriptInputInvalid) {
		t.Fatalf("NUL external_id error = %v, want ErrTranscriptInputInvalid", err)
	}
}
