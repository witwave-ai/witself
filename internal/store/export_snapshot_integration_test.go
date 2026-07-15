package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	archiveexport "github.com/witwave-ai/witself/internal/export"
)

// TestExportAccountUsesOnePostgresSnapshot is opt-in because it needs a
// disposable real Postgres database. The writer pauses immediately after the
// export transaction has read and exclusively locked the frozen account status (and
// therefore fixed its REPEATABLE READ snapshot), then attempts a change through
// another session. The change must remain blocked until the archive completes,
// and every streamed table must retain the pre-change view.
func TestExportAccountUsesOnePostgresSnapshot(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}

	const originalName = "snapshot before concurrent update"
	provisioned, err := st.ProvisionAccount(ctx,
		"export-snapshot@witwave.ai", originalName, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = deleteAccountForIntegrationTest(ctx, st, provisioned.AccountID) }()
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate = %v / %v", activated, err)
	}

	// The consistency transaction must not weaken the existing operational
	// freeze gate.
	if err := st.ExportAccount(ctx, provisioned.AccountID, "test-source", "test", io.Discard); !errors.Is(err, ErrAccountNotExportable) {
		t.Fatalf("active account export = %v, want ErrAccountNotExportable", err)
	}
	if err := st.SuspendAccountSystem(ctx, provisioned.AccountID, "evacuation", "snapshot test"); err != nil {
		t.Fatal(err)
	}

	gate := &firstWriteGate{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(gate.release) }) }
	defer release()

	exportErr := make(chan error, 1)
	go func() {
		exportErr <- st.ExportAccount(ctx, provisioned.AccountID, "test-source", "test", gate)
	}()

	select {
	case <-gate.started:
	case <-time.After(10 * time.Second):
		t.Fatal("export did not reach the archive writer")
	}

	secondExportErr := make(chan error, 1)
	go func() {
		secondExportErr <- st.ExportAccount(ctx, provisioned.AccountID, "test-source-2", "test", io.Discard)
	}()
	select {
	case err := <-secondExportErr:
		t.Fatalf("concurrent export was not serialized by the account lock: %v", err)
	case <-time.After(150 * time.Millisecond):
		// Expected: only one export snapshot can own the account row at a time.
	}

	const concurrentName = "snapshot after concurrent update"
	updateErr := make(chan error, 1)
	go func() {
		_, err := st.pool.Exec(ctx,
			`UPDATE accounts SET display_name = $2 WHERE id = $1`,
			provisioned.AccountID, concurrentName)
		updateErr <- err
	}()
	select {
	case err := <-updateErr:
		t.Fatalf("account update was not held by export freeze lock: %v", err)
	case <-time.After(150 * time.Millisecond):
		// Expected: export owns the exclusive account-row lock until the stream ends.
	}
	release()

	select {
	case err := <-exportErr:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("export did not complete after writer release")
	}
	select {
	case err := <-secondExportErr:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serialized export did not complete after the first export")
	}
	select {
	case err := <-updateErr:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("account update remained blocked after export commit")
	}

	var archivedName string
	if _, err := archiveexport.Read(ctx, bytes.NewReader(gate.Bytes()), archiveexport.ImportOptions{
		CurrentSchema: SchemaVersion(),
		Row: func(table string, row []byte) error {
			if table != "accounts" {
				return nil
			}
			var account struct {
				DisplayName string `json:"display_name"`
			}
			if err := json.Unmarshal(row, &account); err != nil {
				return err
			}
			archivedName = account.DisplayName
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if archivedName != originalName {
		t.Fatalf("archived display_name = %q, want snapshot value %q (database now contains %q)",
			archivedName, originalName, concurrentName)
	}
}

type firstWriteGate struct {
	bytes.Buffer
	once    sync.Once
	started chan struct{}
	release chan struct{}
}

func (w *firstWriteGate) Write(p []byte) (int, error) {
	w.once.Do(func() {
		close(w.started)
		<-w.release
	})
	return w.Buffer.Write(p)
}
