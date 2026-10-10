package store

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"reflect"
	"testing"
	"time"

	archiveexport "github.com/witwave-ai/witself/internal/export"
	"github.com/witwave-ai/witself/internal/testenv"
)

// Complete rows of a truncated entry now reach the transaction. Both a cut
// between rows and a cut within a row must still roll back every staged row.
func TestImportAccountTruncatedEntryRollsBackPostgres(t *testing.T) {
	st, _ := newMigrationTestStore(t, testenv.RequirePostgres(t))
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	fixture := prepareImportContentionFixture(ctx, t, st, "streaming-truncation")
	cutTable := firstImportStreamingMultirowTable(t, fixture)
	if cutTable == "" {
		fixture = prepareImportStreamingMultirowFixture(ctx, t, st)
		cutTable = firstImportStreamingMultirowTable(t, fixture)
	}
	if cutTable == "" {
		t.Fatal("streaming truncation fixture has no table with two rows")
	}
	bystander := prepareImportContentionFixture(ctx, t, st, "streaming-truncation-bystander")
	bystanderBefore := snapshotImportGzipCompletionRows(t, st, bystander.accountID)
	corrupt := cutImportStreamingEntry(t, fixture.archive, cutTable)
	counts := make(map[string]int, len(fixture.rows))
	empty := make(importContentionRows, len(fixture.rows))
	for table, rows := range fixture.rows {
		counts[table] = len(rows)
		empty[table] = []string{}
	}
	removed, err := removeMemoryArchiveLoadAccount(ctx, st, fixture.accountID, counts)
	if err != nil || !removed {
		t.Fatalf("streaming truncation fixture removal failed: %v", err)
	}
	assertImportContentionRows(t, "streaming fixture removal changed bystander", bystanderBefore,
		snapshotImportGzipCompletionRows(t, st, bystander.accountID))
	assertImportContentionRows(t, "streaming fixture destination is not empty", empty,
		snapshotImportGzipCompletionRows(t, st, fixture.accountID))

	for _, tc := range []struct {
		name    string
		archive []byte
	}{
		{"after_first_row", corrupt[0]},
		{"within_second_row", corrupt[1]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rows, completed int
			_, readErr := archiveexport.Read(ctx, bytes.NewReader(tc.archive), archiveexport.ImportOptions{
				CurrentSchema: SchemaVersion(),
				Row: func(table string, _ []byte) error {
					if table == cutTable {
						rows++
					}
					return nil
				},
				OnEntry: func(entry archiveexport.EntryStats) {
					if entry.Table == cutTable {
						completed++
					}
				},
			})
			if !errors.Is(readErr, archiveexport.ErrCorrupt) || rows != 1 || completed != 0 {
				t.Fatalf("streaming truncation preflight: corrupt=%v rows=%d completed=%d; want true, 1, 0",
					errors.Is(readErr, archiveexport.ErrCorrupt), rows, completed)
			}
			manifest, importErr := st.ImportAccount(ctx, fixture.accountID, bytes.NewReader(tc.archive))
			assertImportContentionRows(t, "streaming corrupt import changed bystander", bystanderBefore,
				snapshotImportGzipCompletionRows(t, st, bystander.accountID))
			assertImportContentionRows(t, "streaming corrupt import left durable target rows", empty,
				snapshotImportGzipCompletionRows(t, st, fixture.accountID))
			if !errors.Is(importErr, archiveexport.ErrCorrupt) {
				t.Fatal("streaming truncated entry was not rejected with ErrCorrupt")
			}
			if !reflect.DeepEqual(manifest, archiveexport.Manifest{}) {
				t.Fatal("streaming truncated entry returned a manifest")
			}
		})
	}

	manifest, err := st.ImportAccount(ctx, fixture.accountID, bytes.NewReader(fixture.archive))
	if err != nil {
		t.Fatalf("streaming truncation intact recovery failed: %v", err)
	}
	assertImportContentionManifest(t, manifest, fixture.accountID)
	var restored bytes.Buffer
	if err := st.ExportAccount(ctx, fixture.accountID, "import-streaming-truncation", "test", &restored); err != nil {
		t.Fatalf("streaming truncation restored export failed: %v", err)
	}
	manifest, restoredRows, err := readImportContentionArchive(ctx, restored.Bytes())
	if err != nil {
		t.Fatalf("streaming truncation restored archive verification failed: %v", err)
	}
	assertImportContentionManifest(t, manifest, fixture.accountID)
	assertImportContentionRows(t, "streaming recovery changed portable graph", fixture.rows, restoredRows)
	assertImportContentionRows(t, "streaming recovery changed bystander", bystanderBefore,
		snapshotImportGzipCompletionRows(t, st, bystander.accountID))
}

func firstImportStreamingMultirowTable(t *testing.T, fixture importContentionFixture) string {
	t.Helper()
	manifest, err := archiveexport.Read(context.Background(), bytes.NewReader(fixture.archive),
		archiveexport.ImportOptions{CurrentSchema: SchemaVersion()})
	if err != nil {
		t.Fatalf("streaming truncation source manifest failed: %v", err)
	}
	for _, table := range manifest.Tables {
		if len(fixture.rows[table]) >= 2 {
			return table
		}
	}
	return ""
}

// Keep the shared contention fixture's exact one-memory assertions intact.
// If it has no multirow table, reproduce its setup with two memory captures.
func prepareImportStreamingMultirowFixture(ctx context.Context, t *testing.T, st *Store) importContentionFixture {
	t.Helper()
	account, err := st.ProvisionAccount(ctx, "streaming-multirow@example.invalid", "import streaming", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if activated, err := st.ActivateAccount(ctx, account.AccountID); err != nil || !activated {
		t.Fatalf("activate streaming fixture failed: %v", err)
	}
	realm, err := st.CreateRealm(ctx, account.AccountID, "default")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := st.CreateAgent(ctx, account.AccountID, realm.ID, "import-agent")
	if err != nil {
		t.Fatal(err)
	}
	p := Principal{Kind: PrincipalAgent, ID: agent.ID, AccountID: account.AccountID,
		RealmID: realm.ID, AccountStatus: "active"}
	for _, key := range []string{"streaming-memory-first", "streaming-memory-second"} {
		created, err := st.CaptureMemory(ctx, p, CaptureMemoryInput{
			Content: "Synthetic note retained across truncated account imports.", Kind: "note",
			IdempotencyKey: key,
			Evidence: []MemoryEvidenceInput{{ResolutionState: MemoryEvidenceUnavailable,
				TerminalReasonCode: "synthetic_fixture"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if created.Receipt.Replayed || created.Memory.ID == "" {
			t.Fatal("streaming fixture did not create two distinct memories")
		}
	}
	if err := st.SuspendAccountSystem(ctx, p.AccountID, "evacuation", "synthetic streaming fixture"); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := st.ExportAccount(ctx, p.AccountID, "import-streaming-truncation", "test", &archive); err != nil {
		t.Fatal(err)
	}
	manifest, rows, err := readImportContentionArchive(ctx, archive.Bytes())
	if err != nil {
		t.Fatalf("streaming source archive verification failed: %v", err)
	}
	assertImportContentionManifest(t, manifest, p.AccountID)
	return importContentionFixture{accountID: p.AccountID, realmID: p.RealmID, agentID: p.ID,
		archive: archive.Bytes(), rows: rows}
}

// Cut decoded TAR bytes, not a reconstructed TAR, so the declared entry size
// remains intact and the tar reader reports truncation after the delivered row.
func cutImportStreamingEntry(t *testing.T, archive []byte, table string) [2][]byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := io.ReadAll(gz)
	if err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	source := bytes.NewReader(decoded)
	tr := tar.NewReader(source)
	for {
		header, err := tr.Next()
		if err != nil {
			t.Fatalf("streaming truncation entry not found: %v", err)
		}
		if header.Name != table+"/000001.ndjson" {
			continue
		}
		entryStart := len(decoded) - source.Len()
		payload, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		firstEnd := bytes.IndexByte(payload, '\n') + 1
		if firstEnd <= 0 {
			t.Fatal("streaming truncation entry has no complete first row")
		}
		secondLength := bytes.IndexByte(payload[firstEnd:], '\n')
		if secondLength < 2 {
			t.Fatal("streaming truncation entry has no second row with an interior cut")
		}
		var result [2][]byte
		for i, end := range []int{firstEnd, firstEnd + secondLength/2} {
			var compressed bytes.Buffer
			writer := gzip.NewWriter(&compressed)
			if _, err := writer.Write(decoded[:entryStart+end]); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			result[i] = compressed.Bytes()
		}
		return result
	}
}
