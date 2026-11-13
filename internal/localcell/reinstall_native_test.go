package localcell_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/localcell"
	"github.com/witwave-ai/witself/internal/store"
)

// testRetainedReinstallNative readmits a retained real cluster in both layouts.
// Custody bytes stay in memory for comparison only; never print or hash them.
func testRetainedReinstallNative(t *testing.T, binaries localcell.Binaries) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	t.Setenv("WITSELF_RUNTIME", "local")
	t.Run("Legacy", func(t *testing.T) { testRetainedReinstallNativeLayout(t, binaries, false) })
	t.Run("Selected", func(t *testing.T) { testRetainedReinstallNativeLayout(t, binaries, true) })
}

func testRetainedReinstallNativeLayout(t *testing.T, binaries localcell.Binaries, selected bool) {
	interrupt := false
	cell, root := disposableCell(t, binaries, func(operation, step string) error {
		if interrupt && operation == "reinstall" && step == "verified" {
			interrupt = false
			return errors.New("injected reinstall interruption")
		}
		return nil
	})
	ctx := t.Context()
	if err := cell.Install(ctx, localcell.InstallOptions{ManagedPrincipals: selected}); err != nil {
		t.Fatalf("install: %v", err)
	}
	if err := startLifecycle(t, cell, root); err != nil {
		t.Fatalf("start: %v", err)
	}
	before := *assertReady(t, cell)
	account := ""
	if !selected {
		account = readStoreState(t, root, &before)
	}
	if before.AccountID == "" || before.LastVerifiedSchemaVersion != store.SchemaVersion() {
		t.Fatal("native installation did not verify its account and schema")
	}
	custody := reinstallNativeCustody(t, root, selected)
	if err := cell.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := cell.Remove(ctx, localcell.RemoveOptions{}); err != nil {
		t.Fatalf("remove retaining data: %v", err)
	}
	assertStopped(t, cell)
	retained := reinstallNativeSnapshot(t, root)
	options := localcell.ReinstallOptions{InstallationID: before.InstallationID}
	wrong := localcell.ReinstallOptions{InstallationID: before.InstallationID + "-other"}
	if err := cell.Reinstall(ctx, wrong); !errors.Is(err, localcell.ErrReinstallIdentityMismatchForTest) || !reflect.DeepEqual(retained, reinstallNativeSnapshot(t, root)) {
		t.Fatal("wrong-identity reinstall was not refused without changes")
	}
	if selected {
		interrupt = true
		if err := cell.Reinstall(ctx, options); err == nil || interrupt {
			t.Fatal("native reinstall did not reach its verified interruption")
		}
		if err := cell.Repair(ctx); err != nil {
			t.Fatalf("repair interrupted reinstall: %v", err)
		}
		assertStopped(t, cell)
		if inspection, err := cell.Inspect(ctx); err != nil || inspection.Manifest == nil || !inspection.Manifest.RemovalRetained || inspection.Interrupted != nil {
			t.Fatal("repaired native reinstall lost the retained marker or stayed open")
		}
	}
	if err := cell.Reinstall(ctx, options); err != nil {
		t.Fatalf("reinstall: %v", err)
	}
	// Success stops the proof's PostgreSQL before Start, which would adopt it.
	assertStopped(t, cell)
	if inspection, err := cell.Inspect(ctx); err != nil || inspection.Manifest == nil || inspection.Manifest.RemovalRetained || inspection.Interrupted != nil {
		t.Fatal("readmitted native cell is still removed, interrupted or not private")
	}
	if !reflect.DeepEqual(custody, reinstallNativeCustody(t, root, selected)) {
		t.Fatal("native reinstall changed retained custody")
	}
	if err := startLifecycle(t, cell, root); err != nil {
		t.Fatalf("start after reinstall: %v", err)
	}
	after := assertReady(t, cell)
	if _, err := cell.Inspect(ctx); err != nil || after.InstallationID != before.InstallationID || after.AccountID != before.AccountID || after.LastVerifiedSchemaVersion != store.SchemaVersion() {
		t.Fatal("readmitted native cell changed identity, account or schema")
	}
	if !selected && readStoreState(t, root, after) != account {
		t.Fatal("readmitted native cell changed its default account")
	}
	if err := cell.Stop(ctx); err != nil {
		t.Fatalf("stop after reinstall: %v", err)
	}
	if _, err := cell.Inspect(ctx); err != nil {
		t.Fatal("stopped readmitted native cell is not private")
	}
	if err := cell.Remove(ctx, localcell.RemoveOptions{Purge: true}); err != nil {
		t.Fatalf("purge: %v", err)
	}
	for _, name := range []string{"pg", "manifest.json"} {
		if _, err := os.Stat(filepath.Join(root, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("purge retained %s", name)
		}
	}
}

// reinstallNativeCustody reads the bootstrap and selected credentials into memory.
func reinstallNativeCustody(t *testing.T, root string, selected bool) map[string]string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(root, "run", "credentials", "*"))
	if err != nil {
		t.Fatal("cannot inventory native custody")
	}
	custody := map[string]string{}
	for _, path := range append(paths, filepath.Join(root, "run", "postgres.password")) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal("retained native custody is unreadable")
		}
		custody[filepath.Base(path)] = string(data)
	}
	if want := map[bool]int{false: 1, true: 7}[selected]; len(custody) != want {
		t.Fatal("native custody inventory is incomplete")
	}
	return custody
}

type reinstallNativeEntry struct {
	mode     fs.FileMode
	size     int64
	modified int64
	data     string
}

// reinstallNativeSnapshot records every entry's mode, size and modification
// time, plus the bytes of everything outside PGDATA except held lock files.
func reinstallNativeSnapshot(t *testing.T, root string) map[string]reinstallNativeEntry {
	t.Helper()
	result := map[string]reinstallNativeEntry{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		name, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		name = filepath.ToSlash(name)
		value := reinstallNativeEntry{mode: info.Mode(), modified: info.ModTime().UnixNano()}
		if !entry.IsDir() {
			value.size = info.Size()
		}
		if !entry.IsDir() && name != "pg" && !strings.HasPrefix(name, "pg/") && !strings.HasSuffix(name, ".lock") {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			value.data = string(data)
		}
		result[name] = value
		return nil
	})
	if err != nil {
		t.Fatal("cannot snapshot the retained native tree")
	}
	return result
}
