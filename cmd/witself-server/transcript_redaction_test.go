package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/server"
	"github.com/witwave-ai/witself/internal/store"
)

func TestMapTranscriptRedactionError(t *testing.T) {
	for _, tt := range []struct {
		name string
		in   error
		want error
	}{
		{"input", store.ErrTranscriptInputInvalid, server.ErrBadInput},
		{"owner", store.ErrTranscriptForbidden, server.ErrForbidden},
		{"foreign", store.ErrTranscriptNotFound, server.ErrNotFound},
		{"revision", store.ErrTranscriptRedactionConflict, server.ErrConflict},
		{"already redacted", store.ErrTranscriptEntriesAlreadyRedacted, server.ErrTranscriptEntriesRedacted},
		{"evacuation", store.ErrAccountEvacuationInProgress, server.ErrAccountMoveInProgress},
		{"inactive", store.ErrAccountNotActive, server.ErrForbidden},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if !errors.Is(mapTranscriptError(tt.in), tt.want) {
				t.Fatal("transcript redaction error lost its public sentinel")
			}
		})
	}
	if !errors.Is(mapTranscriptError(store.ErrAccountEvacuationInProgress), server.ErrConflict) {
		t.Fatal("account evacuation must remain a conflict")
	}
	if mapTranscriptError(nil) != nil {
		t.Fatal("successful operation mapped to an error")
	}
}

func TestTranscriptRedactionReceiptProjectionPreservesEveryField(t *testing.T) {
	for _, pair := range []struct {
		name     string
		from, to reflect.Type
	}{
		{"receipt", reflect.TypeFor[store.TranscriptRedactionResult](), reflect.TypeFor[server.TranscriptRedactionReceipt]()},
		{"entry", reflect.TypeFor[store.TranscriptRedactionEntry](), reflect.TypeFor[server.TranscriptRedactionEntry]()},
		{"request", reflect.TypeFor[server.RedactTranscriptEntriesRequest](), reflect.TypeFor[store.RedactTranscriptEntriesInput]()},
	} {
		if pair.from.NumField() != pair.to.NumField() {
			t.Fatalf("%s projection has %d fields, source has %d", pair.name, pair.to.NumField(), pair.from.NumField())
		}
	}
	assertJSONEqual := func(t *testing.T, want, got any) {
		t.Helper()
		wantJSON, err := json.Marshal(want)
		if err != nil {
			t.Fatal(err)
		}
		gotJSON, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(wantJSON, gotJSON) {
			t.Fatal("projection changed serialized fields")
		}
	}
	created := time.Date(2026, time.October, 23, 4, 5, 6, 7000, time.UTC)
	redacted := created.Add(2 * time.Hour)
	result := store.TranscriptRedactionResult{
		TranscriptID: "trn_projection", FromSequence: 11, UntilSequence: 12,
		RedactionSetRevision: "projection-revision", EntryCount: 13,
		RedactableCount: 14, AlreadyRedactedCount: 15, PartialCaptureEvent: true,
		MemoryEvidenceCount: 16, EvidenceMemoryIDs: []string{"mem_evidence_one", "mem_evidence_two"},
		CurationRunsToCancel: 17, CancelledCurationRunCount: 18,
		Entries: []store.TranscriptRedactionEntry{
			{EntryID: "ent_projection_one", Sequence: 19, Role: "tool", Kind: "tool.result", Redacted: true, CreatedAt: created},
			{EntryID: "ent_projection_two", Sequence: 20, Role: "assistant", Kind: "message.assistant", Redacted: false, CreatedAt: created.Add(time.Hour)},
		},
		ReceiptID: "trd_0123456789abcdef0123456789abcdef", RedactedCount: 21,
		RedactedAt: &redacted, Applied: false, Replayed: false,
	}
	t.Run("receipt", func(t *testing.T) {
		assertJSONEqual(t, result, toServerTranscriptRedactionReceipt(result))
	})
	t.Run("receipt alternate booleans", func(t *testing.T) {
		// (true,false,false) then (false,true,true): every pair of flags differs
		// in at least one fixture, so any swap changes the bytes.
		result.PartialCaptureEvent, result.Applied, result.Replayed = false, true, true
		assertJSONEqual(t, result, toServerTranscriptRedactionReceipt(result))
	})
	t.Run("request", func(t *testing.T) {
		request := server.RedactTranscriptEntriesRequest{
			TranscriptID: "trn_request", FromSequence: 31, UntilSequence: 32,
			ExpectedRedactionSetRevision: "request-revision", IdempotencyKey: "request-key", Apply: true,
		}
		assertJSONEqual(t, request, toStoreRedactTranscriptEntriesInput(request))
	})
}
