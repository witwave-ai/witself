package store

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	archiveexport "github.com/witwave-ai/witself/internal/export"
)

// TestExportAccountSelfActivePostgres is opt-in because it needs a disposable
// real PostgreSQL database. It pins the self-export contract: an active account
// can be exported without taking the evacuation path's account-row lock, the
// archive verifies through the shared checksum reader, and its manifest carries
// only the self-service purpose (no backup or evacuation identity).
func TestExportAccountSelfActivePostgres(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}

	ctx := context.Background()
	st, _ := newMigrationTestStore(t, baseDSN)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}

	provisioned, err := st.ProvisionAccount(
		ctx,
		"self-export@witwave.ai",
		"self export",
		time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate account = %v, %v", activated, err)
	}

	gate := &selfExportWriteGate{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(gate.release) }) }
	defer release()

	exportErr := make(chan error, 1)
	go func() {
		exportErr <- st.ExportAccountSelf(
			ctx,
			provisioned.AccountID,
			"self-export-source",
			"test-version",
			gate,
		)
	}()

	select {
	case <-gate.started:
	case <-time.After(10 * time.Second):
		t.Fatal("self export did not reach the archive writer")
	}

	// The stream is paused with the export transaction open. FOR UPDATE NOWAIT
	// must still acquire the account row, proving self export did not inherit
	// the frozen evacuation export's row lock.
	var lockedAccountID string
	if err := st.pool.QueryRow(ctx, `
		SELECT id FROM accounts WHERE id=$1 FOR UPDATE NOWAIT`,
		provisioned.AccountID,
	).Scan(&lockedAccountID); err != nil {
		t.Fatalf("self export held account row lock: %v", err)
	}
	if lockedAccountID != provisioned.AccountID {
		t.Fatalf("locked account id = %q, want %q", lockedAccountID, provisioned.AccountID)
	}

	release()
	select {
	case err := <-exportErr:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("self export did not complete after writer release")
	}

	archive := gate.Bytes()
	rowCounts := map[string]int{}
	manifest, err := archiveexport.Read(
		ctx,
		bytes.NewReader(archive),
		archiveexport.ImportOptions{
			CurrentSchema: SchemaVersion(),
			Row: func(table string, _ []byte) error {
				rowCounts[table]++
				return nil
			},
		},
	)
	if err != nil {
		t.Fatalf("verify self export checksums: %v", err)
	}
	if manifest.FormatVersion != archiveexport.FormatVersion ||
		manifest.Purpose != archiveexport.PurposeSelf ||
		manifest.BackupID != "" ||
		manifest.EvacuationID != "" ||
		manifest.AccountID != provisioned.AccountID ||
		manifest.Status != "active" ||
		manifest.Cell != "self-export-source" ||
		manifest.ServerVersion != "test-version" {
		t.Fatalf("self export manifest = %+v", manifest)
	}
	if rowCounts["accounts"] != 1 {
		t.Fatalf("self export account rows = %d, want 1", rowCounts["accounts"])
	}

	manifestFields := readSelfExportManifestFields(t, archive)
	if _, ok := manifestFields["backup_id"]; ok {
		t.Fatal("self export manifest carried backup_id")
	}
	if _, ok := manifestFields["evacuation_id"]; ok {
		t.Fatal("self export manifest carried evacuation_id")
	}

	var status string
	if err := st.pool.QueryRow(ctx,
		`SELECT status FROM accounts WHERE id=$1`,
		provisioned.AccountID,
	).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "active" {
		t.Fatalf("source account status after self export = %q, want active", status)
	}
}

type selfExportWriteGate struct {
	bytes.Buffer
	once    sync.Once
	started chan struct{}
	release chan struct{}
}

func (w *selfExportWriteGate) Write(p []byte) (int, error) {
	w.once.Do(func() {
		close(w.started)
		<-w.release
	})
	return w.Buffer.Write(p)
}

func readSelfExportManifestFields(t *testing.T, archive []byte) map[string]json.RawMessage {
	t.Helper()
	gzr, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gzr.Close() }()

	tr := tar.NewReader(gzr)
	hdr, err := tr.Next()
	if err != nil {
		t.Fatal(err)
	}
	if hdr.Name != "manifest.json" {
		t.Fatalf("first self export entry = %q, want manifest.json", hdr.Name)
	}
	raw, err := io.ReadAll(tr)
	if err != nil {
		t.Fatal(err)
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	return fields
}
