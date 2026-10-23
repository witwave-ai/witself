package store

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	archiveexport "github.com/witwave-ai/witself/internal/export"
	"github.com/witwave-ai/witself/internal/testenv"
)

func TestTranscriptRedactionArchiveRoundTripPostgres(t *testing.T) {
	dsn := testenv.RequirePostgres(t)
	source, _ := newMigrationTestStore(t, dsn)
	destination, _ := newMigrationTestStore(t, dsn)
	for _, st := range []*Store{source, destination} {
		if err := st.Migrate(); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	p := newTranscriptStorageTestPrincipal(t, source, "redaction-archive")
	tr := createTranscriptStorageTestConversation(t, source, p, "redaction-archive")
	const canary = "WITSELF-CANARY-160B-ARCHIVE"
	in := AppendTranscriptEntryInput{ExternalID: "redacted-source", Role: TranscriptRoleTool, Body: canary,
		Payload: json.RawMessage(`{"kind":"tool.result","data":"WITSELF-CANARY-160B-ARCHIVE"}`)}
	entry := appendTranscriptStorageTestEntry(t, source, p, tr, in)
	owner := transcriptRedactionTestOwner(t, source, p)
	preview, err := source.RedactTranscriptEntries(ctx, owner, RedactTranscriptEntriesInput{
		TranscriptID: tr.ID, FromSequence: entry.Sequence, UntilSequence: entry.Sequence,
	})
	if err != nil {
		t.Fatal(err)
	}
	applied, err := source.RedactTranscriptEntries(ctx, owner, RedactTranscriptEntriesInput{
		TranscriptID: tr.ID, FromSequence: entry.Sequence, UntilSequence: entry.Sequence,
		Apply: true, ExpectedRedactionSetRevision: preview.RedactionSetRevision, IdempotencyKey: "archive-redaction",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := source.SuspendAccountSystem(ctx, p.AccountID, "evacuation", "redaction archive fixture"); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := source.ExportAccount(ctx, p.AccountID, "redaction-source", "test", &archive); err != nil {
		t.Fatal(err)
	}
	markerCount, eventCount := 0, 0
	if _, err := archiveexport.Read(ctx, bytes.NewReader(archive.Bytes()), archiveexport.ImportOptions{
		CurrentSchema: SchemaVersion(),
		Row: func(table string, row []byte) error {
			if bytes.Contains(row, []byte(canary)) {
				t.Fatalf("pre-redaction canary remains in archived table %s", table)
			}
			switch table {
			case "transcript_entries":
				var archived struct {
					ID        string          `json:"id"`
					Body      string          `json:"body"`
					Payload   json.RawMessage `json:"payload"`
					Artifacts json.RawMessage `json:"artifacts"`
				}
				if err := json.Unmarshal(row, &archived); err != nil {
					return err
				}
				if archived.ID == entry.ID {
					marker, ok := TranscriptEntryRedactionOf(TranscriptEntry{
						Body: archived.Body, Payload: archived.Payload, Artifacts: archived.Artifacts,
					})
					if !ok || marker.ReceiptID != applied.ReceiptID {
						t.Fatal("archive did not preserve the redaction marker")
					}
					markerCount++
				}
			case "account_events":
				var event struct {
					Verb string `json:"verb"`
				}
				if err := json.Unmarshal(row, &event); err != nil {
					return err
				}
				if event.Verb == VerbTranscriptEntriesRedacted {
					eventCount++
				}
			}
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if markerCount != 1 || eventCount != 1 {
		t.Fatalf("archive marker/event rows = %d/%d, want 1/1", markerCount, eventCount)
	}
	if _, err := destination.ImportAccount(ctx, p.AccountID, bytes.NewReader(archive.Bytes())); err != nil {
		t.Fatal(err)
	}
	// Import intentionally leaves the account suspended; match the archive
	// tests' lifecycle helper before exercising the active-only append route.
	if err := destination.ResumeAccountSystem(ctx, p.AccountID, "evacuation"); err != nil {
		t.Fatal(err)
	}
	replayed, err := destination.AppendTranscriptEntry(ctx, p.AccountID, p.RealmID, p.ID, tr.ID, in)
	if err != nil {
		t.Fatalf("imported redacted append replay: %v", err)
	}
	marker, ok := TranscriptEntryRedactionOf(replayed)
	if !ok || marker.ReceiptID != applied.ReceiptID || replayed.ID != entry.ID || replayed.Sequence != entry.Sequence {
		t.Fatal("imported append retry did not preserve the redacted entry identity and marker")
	}
	assertTranscriptStorageUsageMatchesRecount(t, source, p.AccountID)
}
