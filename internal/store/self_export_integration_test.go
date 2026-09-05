package store

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"sync"
	"testing"
	"time"

	archiveexport "github.com/witwave-ai/witself/internal/export"
	"github.com/witwave-ai/witself/internal/testenv"
)

// TestExportAccountSelfActivePostgres is opt-in because it needs a disposable
// real PostgreSQL database. It pins the self-export contract: an active account
// can be exported without taking the evacuation path's account-row lock, the
// archive verifies through the shared checksum reader, and its manifest carries
// only the self-service purpose (no backup or evacuation identity).
func TestExportAccountSelfActivePostgres(t *testing.T) {
	baseDSN := testenv.RequirePostgres(t)

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

// TestExportAccountSelfIgnoresExpiredEnrollmentsPostgres pins the review fix:
// the read-only self path cannot reap lazily-expired vault-key enrollments,
// so a time-expired pending enrollment must not refuse the export forever,
// while a live pending enrollment still fails closed.
func TestExportAccountSelfIgnoresExpiredEnrollmentsPostgres(t *testing.T) {
	baseDSN := testenv.RequirePostgres(t)

	ctx := context.Background()
	st, _ := newMigrationTestStore(t, baseDSN)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	provisioned, err := st.ProvisionAccount(
		ctx, "self-export-enrollment@witwave.ai", "self export enrollment", time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}

	// Build the minimal real hierarchy the enrollment FKs demand — realm,
	// agent, current vault key — then seed the pending enrollment directly
	// and age it past expiry. Direct SQL for the leaf rows keeps the fixture
	// independent of the full enrollment handshake.
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate = %v / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "default")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "exporter")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO agent_vault_keys
		  (id, account_id, realm_id, owner_agent_id, key_version,
		   algorithm, fingerprint, lifecycle_state)
		VALUES ('avk_selfexportabcd22', $1, $2, $3, 1,
		        'AES_256_GCM_RANDOM_NONCE_V1', 'abababababababababababababababababababababababababababababababab', 'current')`,
		provisioned.AccountID, realm.ID, agent.ID); err != nil {
		t.Fatal(err)
	}
	var enrollmentID string
	if err := st.pool.QueryRow(ctx, `
		INSERT INTO agent_vault_key_enrollments
		  (id, account_id, realm_id, owner_agent_id, vault_key_id,
		   vault_key_version, target_location_id, target_public_key,
		   target_key_algorithm, pairing_commitment, lifecycle_state,
		   expires_at)
		VALUES ('enr_selfexportabcd22', $1, $2, $3, 'avk_selfexportabcd22',
		        1, 'loc_selfexportabcd22', 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA',
		        'X25519_RAW_32_BASE64URL_V1', 'cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd', 'pending',
		        now() + interval '1 hour')
		RETURNING id`,
		provisioned.AccountID, realm.ID, agent.ID).Scan(&enrollmentID); err != nil {
		t.Fatalf("seed enrollment fixture: %v", err)
	}

	var live bytes.Buffer
	err = st.ExportAccountSelf(ctx, provisioned.AccountID, "self-export-cell", "test", &live)
	if err == nil || !errorsIsVaultLifecycleInProgress(err) {
		t.Fatalf("live pending enrollment: export err = %v, want ErrVaultLifecycleInProgress", err)
	}

	if _, err := st.pool.Exec(ctx, `
		UPDATE agent_vault_key_enrollments
		   SET created_at = now() - interval '2 hours',
		       expires_at = now() - interval '1 hour'
		 WHERE id = $1`, enrollmentID); err != nil {
		t.Fatal(err)
	}

	var expired bytes.Buffer
	if err := st.ExportAccountSelf(
		ctx, provisioned.AccountID, "self-export-cell", "test", &expired,
	); err != nil {
		t.Fatalf("expired enrollment must not block self export: %v", err)
	}
	if expired.Len() == 0 {
		t.Fatal("expired-enrollment export produced no archive bytes")
	}
}

func errorsIsVaultLifecycleInProgress(err error) bool {
	return err != nil && errorsIs(err, ErrVaultLifecycleInProgress)
}

func errorsIs(err, target error) bool {
	for e := err; e != nil; {
		if e == target {
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := e.(unwrapper)
		if !ok {
			return false
		}
		e = u.Unwrap()
	}
	return false
}
