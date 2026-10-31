package export

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestSyncExtensionCoreArchiveBytesUnchanged(t *testing.T) {
	m := Manifest{SchemaVersion: 101, ServerVersion: "test", AccountID: "acc_golden", Status: "suspended", ExportedAt: time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)}
	var out bytes.Buffer
	if err := Write(context.Background(), &out, m, []RowSource{
		&fixtureSource{table: "accounts", rows: []map[string]any{{"id": "acc_golden", "status": "suspended"}}},
		&fixtureSource{table: "operators"},
	}); err != nil {
		t.Fatal(err)
	}
	// Captured using HEAD's pre-extension export.go via a Go source overlay.
	golden, err := os.ReadFile("testdata/core_archive_v1.golden")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), golden) {
		t.Fatal("core gzip archive bytes changed")
	}
	raw, err := json.Marshal(m)
	if err != nil || bytes.Contains(raw, []byte("sync_extension")) {
		t.Fatal("core manifest gained sync extension")
	}
}

func TestSyncExtensionManifestRoundTrip(t *testing.T) {
	m := Manifest{SchemaVersion: 1, AccountID: "acc_sync", SyncExtension: &SyncExtension{Version: 1, SchemaVersion: 1, RequiredStreams: []string{"sync_receipts"}}}
	var out bytes.Buffer
	if err := Write(context.Background(), &out, m, []RowSource{&fixtureSource{table: "sync_receipts"}}); err != nil {
		t.Fatal(err)
	}
	got, err := Read(context.Background(), &out, ImportOptions{CurrentSchema: 1})
	if err != nil || got.SyncExtension == nil || got.SyncExtension.Version != 1 || got.SyncExtension.SchemaVersion != 1 || len(got.SyncExtension.RequiredStreams) != 1 || got.SyncExtension.RequiredStreams[0] != "sync_receipts" {
		t.Fatal("sync extension did not survive archive framing")
	}
}
