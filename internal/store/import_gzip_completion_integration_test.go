package store

import (
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

// A valid logical archive can still have a corrupt enclosing gzip footer.
// Exercise the real transaction through rejection, durable absence and retry.
func TestImportAccountGzipCompletionRollbackPostgres(t *testing.T) {
	// Registered first so this runs after the migration fixture's cleanup.
	// The external runner must still verify actual schema/database absence.
	t.Cleanup(func() { t.Log("import gzip completion: fixture cleanup callbacks finished") })
	st, _ := newMigrationTestStore(t, testenv.RequirePostgres(t))
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	fixture := prepareImportContentionFixture(ctx, t, st, "gzip-completion")
	bystander := prepareImportContentionFixture(ctx, t, st, "gzip-completion-bystander")
	bystanderBefore := snapshotImportGzipCompletionRows(t, st, bystander.accountID)
	t.Log("import gzip completion: nonempty source and bystander verified")

	corrupt := corruptImportGzipCompletionFooter(t, fixture.archive)
	t.Log("import gzip completion: identical TAR payload and invalid outer CRC verified")
	counts := make(map[string]int, len(fixture.rows))
	empty := make(importContentionRows, len(fixture.rows))
	for table, rows := range fixture.rows {
		counts[table] = len(rows)
		empty[table] = []string{}
	}
	removed, err := removeMemoryArchiveLoadAccount(ctx, st, fixture.accountID, counts)
	if err != nil || !removed {
		t.Fatalf("import gzip completion fixture removal failed: %v", err)
	}
	assertImportContentionRows(t, "gzip fixture removal changed bystander", bystanderBefore,
		snapshotImportGzipCompletionRows(t, st, bystander.accountID))
	assertImportContentionRows(t, "gzip fixture destination is not empty", empty,
		snapshotImportGzipCompletionRows(t, st, fixture.accountID))
	t.Log("import gzip completion: empty destination and preserved bystander verified")

	// Do not preflight corrupt through export.Read: the actual store call must
	// reach the database even when a control removes gzip completion checking.
	manifest, importErr := st.ImportAccount(ctx, fixture.accountID, bytes.NewReader(corrupt))
	bystanderAfter := snapshotImportGzipCompletionRows(t, st, bystander.accountID)
	targetAfter := snapshotImportGzipCompletionRows(t, st, fixture.accountID)
	// Check preservation before either target oracle can stop this history.
	assertImportContentionRows(t, "gzip import changed bystander", bystanderBefore, bystanderAfter)
	if !errors.Is(importErr, archiveexport.ErrCorrupt) {
		if importErr == nil && importGzipCompletionCountsMatch(counts, targetAfter) {
			assertImportContentionManifest(t, manifest, fixture.accountID)
			t.Log("import gzip completion: corrupt archive committed a durable graph")
		}
		t.Fatal("import gzip completion: corrupt archive was not rejected with ErrCorrupt")
	}
	if !reflect.DeepEqual(manifest, archiveexport.Manifest{}) {
		t.Fatal("import gzip completion: rejected archive returned a manifest")
	}
	t.Log("import gzip completion: corrupt import returned ErrCorrupt with no manifest")
	if !reflect.DeepEqual(empty, targetAfter) {
		if importGzipCompletionCountsMatch(counts, targetAfter) {
			// Deferred-commit controls must leave the complete nonempty graph.
			// A commit rejected by a deferred constraint cannot emit this.
			t.Log("import gzip completion: rejected archive left a durable graph")
		}
		t.Fatal("import gzip completion: rejected archive left durable target rows")
	}
	t.Log("import gzip completion: rollback left no target rows and preserved bystander")

	manifest, err = st.ImportAccount(ctx, fixture.accountID, bytes.NewReader(fixture.archive))
	if err != nil {
		t.Fatalf("import gzip completion intact recovery failed: %v", err)
	}
	assertImportContentionManifest(t, manifest, fixture.accountID)
	var restored bytes.Buffer
	if err := st.ExportAccount(ctx, fixture.accountID, "import-gzip-completion", "test", &restored); err != nil {
		t.Fatalf("import gzip completion restored export failed: %v", err)
	}
	manifest, restoredRows, err := readImportContentionArchive(ctx, restored.Bytes())
	if err != nil {
		t.Fatalf("import gzip completion restored archive verification failed: %v", err)
	}
	assertImportContentionManifest(t, manifest, fixture.accountID)
	assertImportContentionRows(t, "gzip recovery changed portable graph", fixture.rows, restoredRows)
	assertImportContentionBookkeeping(ctx, t, st, fixture)
	t.Log("import gzip completion: valid retry restored exact graph and bookkeeping")

	beforeRetry := snapshotImportGzipCompletionRows(t, st, fixture.accountID)
	_, retryErr := st.ImportAccount(ctx, fixture.accountID, bytes.NewReader(fixture.archive))
	assertImportContentionRows(t, "gzip occupied retry changed bystander", bystanderBefore,
		snapshotImportGzipCompletionRows(t, st, bystander.accountID))
	assertImportContentionRows(t, "gzip occupied retry changed durable rows", beforeRetry,
		snapshotImportGzipCompletionRows(t, st, fixture.accountID))
	if !errors.Is(retryErr, ErrAccountExists) {
		t.Fatal("import gzip completion: occupied retry did not return ErrAccountExists")
	}
	t.Log("import gzip completion: occupied retry and bystander stayed unchanged")
}

// Each post-return observation uses a fresh context outside the import's
// transaction, including when an import operation exhausted its own deadline.
func snapshotImportGzipCompletionRows(t *testing.T, st *Store, accountID string) importContentionRows {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	return importContentionDatabaseRows(ctx, t, st, accountID)
}

func importGzipCompletionCountsMatch(want map[string]int, got importContentionRows) bool {
	if want["accounts"] != 1 || len(got) != len(want) {
		return false
	}
	for table, count := range want {
		rows, exists := got[table]
		if !exists || len(rows) != count {
			return false
		}
	}
	return true
}

func corruptImportGzipCompletionFooter(t *testing.T, original []byte) []byte {
	t.Helper()
	if len(original) < 8 {
		t.Fatal("import gzip completion fixture has no complete gzip footer")
	}
	corrupt := append([]byte(nil), original...)
	crcByte := len(corrupt) - 8
	corrupt[crcByte] ^= 1
	if len(corrupt) != len(original) || !bytes.Equal(corrupt[:crcByte], original[:crcByte]) ||
		!bytes.Equal(corrupt[crcByte+1:], original[crcByte+1:]) || corrupt[crcByte]^original[crcByte] != 1 {
		t.Fatal("import gzip completion fixture changed more than one CRC bit")
	}
	decode := func(raw []byte) ([]byte, error) {
		gz, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("import gzip completion fixture gzip header failed: %v", err)
		}
		decoded, readErr := io.ReadAll(gz)
		if err := gz.Close(); err != nil {
			t.Fatalf("import gzip completion fixture gzip close failed: %v", err)
		}
		return decoded, readErr
	}
	canonicalTAR, err := decode(original)
	if err != nil || len(canonicalTAR) == 0 {
		t.Fatalf("import gzip completion canonical gzip fixture failed: %v", err)
	}
	corruptTAR, err := decode(corrupt)
	if !errors.Is(err, gzip.ErrChecksum) || !bytes.Equal(canonicalTAR, corruptTAR) {
		t.Fatal("import gzip completion fixture must preserve decoded TAR and fail only its outer CRC")
	}
	return corrupt
}
