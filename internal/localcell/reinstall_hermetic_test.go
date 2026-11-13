package localcell_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/localcell"
	"github.com/witwave-ai/witself/internal/store"
)

// Real fixture processes prove Reinstall's PostgreSQL start, rollback stop and
// Repair. The synthetic psql reports schema 1, so the verification query is a
// test helper here; this makes no PostgreSQL storage or migration claim.
func TestReinstallHermeticProcesses(t *testing.T) {
	binaries := buildLifecycleFixture(t)
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	t.Run("InterruptedPostgresRepair", func(t *testing.T) {
		interrupt := false
		cell, root, options := reinstallHermeticRemoved(t, binaries, func(operation, step string) error {
			if interrupt && operation == "reinstall" && step == "postgres" {
				interrupt = false
				return errors.New("injected reinstall interruption")
			}
			return nil
		})
		starts := reinstallHermeticStarts(t, root)
		interrupt = true
		if err := cell.Reinstall(t.Context(), options); err == nil || interrupt || reinstallHermeticStarts(t, root) != starts+1 {
			t.Fatal("reinstall did not stop at its started PostgreSQL interruption")
		}
		// The postmaster is live but its durable ownership record was lost.
		path := filepath.Join(root, "run", "postgres.json")
		record, err := os.ReadFile(path)
		if err != nil || os.Remove(path) != nil {
			t.Fatal("cannot remove interrupted PostgreSQL ownership record")
		}
		if err := cell.Repair(t.Context()); err != nil {
			if lifecycleWriteFile(path, record) != nil {
				t.Error("cannot restore interrupted PostgreSQL cleanup authority")
			}
			t.Fatalf("repair could not recover synthetic PostgreSQL ownership: %v", err)
		}
		assertStopped(t, cell)
		reinstallHermeticAssertRemoved(t, cell)
		if err := cell.Reinstall(t.Context(), options); err != nil {
			t.Fatalf("reinstall after repair: %v", err)
		}
		// Success stops the proof's PostgreSQL; Start would adopt a live one.
		assertStopped(t, cell)
		for _, action := range []func(context.Context) error{cell.Start, cell.Stop, func(ctx context.Context) error {
			return cell.Remove(ctx, localcell.RemoveOptions{Purge: true})
		}} {
			if err := action(t.Context()); err != nil {
				t.Fatalf("readmitted synthetic lifecycle: %v", err)
			}
		}
	})
	t.Run("VerificationFailureStopsPostgres", func(t *testing.T) {
		cell, root, options := reinstallHermeticRemoved(t, binaries, nil)
		localcell.ReinstallQueryForTest(cell, reinstallHermeticQuery("acc_bbbbbbbbbbbbbbbb"))
		starts := reinstallHermeticStarts(t, root)
		if err := cell.Reinstall(t.Context(), options); !errors.Is(err, localcell.ErrReinstallIdentityMismatchForTest) || reinstallHermeticStarts(t, root) != starts+1 {
			t.Fatal("foreign account was not refused after the PostgreSQL proof started")
		}
		assertStopped(t, cell)
		reinstallHermeticAssertRemoved(t, cell)
	})
	// The rollback stop uses its own deadline: a caller that gives up during the
	// proof still gets the proof's refusal, with PostgreSQL stopped.
	t.Run("CallerCancelDuringProof", func(t *testing.T) {
		cell, root, options := reinstallHermeticRemoved(t, binaries, nil)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		foreign := reinstallHermeticQuery("acc_bbbbbbbbbbbbbbbb")
		localcell.ReinstallQueryForTest(cell, func(queryCtx context.Context, m *localcell.Manifest, query string) (string, error) {
			cancel()
			return foreign(queryCtx, m, query)
		})
		starts := reinstallHermeticStarts(t, root)
		if err := cell.Reinstall(ctx, options); !errors.Is(err, localcell.ErrReinstallIdentityMismatchForTest) || reinstallHermeticStarts(t, root) != starts+1 {
			t.Fatal("a caller cancelled during the proof lost the proof's refusal or its rollback")
		}
		assertStopped(t, cell)
		reinstallHermeticAssertRemoved(t, cell)
	})
	// A service-described cell's proof launch is bound to this boot, as every
	// service launch is, so the next boot can reconcile its record.
	t.Run("ServiceProofBindsBoot", func(t *testing.T) {
		t.Setenv("WITSELF_RUNTIME", "local")
		host, err := os.Executable()
		if err != nil {
			t.Fatal("test executable unavailable")
		}
		cell, root, options := reinstallHermeticRemoved(t, binaries, nil, func(ctx context.Context, prepared *localcell.Cell, dir string) error {
			return prepared.PrepareService(ctx, localcell.ServiceDescriptor{Adapter: "foreground", Payload: localcell.ServicePayload{Host: host, Args: []string{"run", "--root", dir}}})
		})
		bound := false
		localcell.ReinstallQueryForTest(cell, func(queryCtx context.Context, m *localcell.Manifest, query string) (string, error) {
			// The proof's live PostgreSQL record must carry this boot's binding.
			var record localcell.ProcessRecord
			data, err := os.ReadFile(filepath.Join(root, "run", "postgres.json"))
			bound = bound || err == nil && json.Unmarshal(data, &record) == nil && localcell.NativeServiceEndpointBootMatches(record.BootID)
			return reinstallHermeticQuery(m.AccountID)(queryCtx, m, query)
		})
		if err := cell.Reinstall(t.Context(), options); err != nil || !bound {
			t.Fatal("the service-described proof launch was not bound to this boot")
		}
		assertStopped(t, cell)
	})
}

// reinstallHermeticRemoved installs, starts and stops a synthetic cell, runs any
// extra steps, removes it with retained data, then seeds this build's verified
// schema.
func reinstallHermeticRemoved(t *testing.T, binaries localcell.Binaries, failpoint func(string, string) error, extra ...func(context.Context, *localcell.Cell, string) error) (*localcell.Cell, string, localcell.ReinstallOptions) {
	t.Helper()
	cell, root := disposableCell(t, binaries, failpoint)
	steps := []func(context.Context) error{func(ctx context.Context) error {
		return cell.Install(ctx, localcell.InstallOptions{})
	}, cell.Start, cell.Stop}
	for _, step := range extra {
		steps = append(steps, func(ctx context.Context) error { return step(ctx, cell, root) })
	}
	for _, action := range append(steps, func(ctx context.Context) error { return cell.Remove(ctx, localcell.RemoveOptions{}) }) {
		if err := action(t.Context()); err != nil {
			t.Fatalf("synthetic retained lifecycle: %v", err)
		}
	}
	if localcell.SeedVerifiedSchemaForTest(cell, store.SchemaVersion()) != nil {
		t.Fatal("cannot seed the verified schema")
	}
	inspection, err := cell.Inspect(t.Context())
	if err != nil || inspection.Manifest == nil || !inspection.Manifest.RemovalRetained {
		t.Fatal("synthetic removal did not retain the installation")
	}
	localcell.ReinstallQueryForTest(cell, reinstallHermeticQuery(inspection.Manifest.AccountID))
	return cell, root, localcell.ReinstallOptions{InstallationID: inspection.Manifest.InstallationID}
}

func reinstallHermeticQuery(account string) func(context.Context, *localcell.Manifest, string) (string, error) {
	return func(_ context.Context, _ *localcell.Manifest, query string) (string, error) {
		if strings.Contains(query, "FROM accounts") {
			return account, nil
		}
		return strconv.Itoa(store.SchemaVersion()), nil
	}
}

// reinstallHermeticStarts counts synthetic postmaster launches in this root.
func reinstallHermeticStarts(t *testing.T, root string) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "fixture-events"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cannot read synthetic PostgreSQL events")
	}
	return bytes.Count(data, []byte("postgres-start\n"))
}

func reinstallHermeticAssertRemoved(t *testing.T, cell *localcell.Cell) {
	t.Helper()
	inspection, err := cell.Inspect(t.Context())
	if err != nil || inspection.Manifest == nil || !inspection.Manifest.RemovalRetained || inspection.Interrupted != nil {
		t.Fatal("reinstall Repair or rollback lost the retained marker or left an open operation")
	}
}
