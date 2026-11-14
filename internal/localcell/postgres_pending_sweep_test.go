package localcell

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func postgresPendingTemp(t *testing.T, dir string) string {
	t.Helper()
	f, err := createPrivateTemp(dir)
	if err != nil {
		t.Fatal("cannot create synthetic pending file")
	}
	_, err = f.Write([]byte("synthetic pending bytes"))
	if closeErr := f.Close(); err != nil || closeErr != nil {
		t.Fatal("cannot persist synthetic pending file")
	}
	return f.Name()
}

func postgresPendingName() string {
	if runtime.GOOS == "windows" {
		return ".pending-" + strings.Repeat("a", 24)
	}
	return ".pending-456"
}

func postgresPendingInventory(t *testing.T, c *Cell) map[string]retainedEntry {
	t.Helper()
	result := checkpointTreeState(t, c)
	for name, entry := range result {
		if entry.mode&os.ModeSymlink != 0 {
			target, err := os.Readlink(c.path(filepath.FromSlash(name)))
			if err != nil {
				t.Fatal("cannot inspect synthetic link")
			}
			entry.data, entry.size = target, int64(len(target))
			result[name] = entry
		}
	}
	return result
}

func postgresPendingConfigure(t *testing.T, c *Cell, m *Manifest) {
	t.Helper()
	if c.configurePostgres(m) != nil {
		t.Fatal("configuration failed during best-effort sweep")
	}
	want := "listen_addresses = '127.0.0.1'\nport = 21101\nunix_socket_directories = ''\npassword_encryption = 'scram-sha-256'\nlogging_collector = off\nlog_destination = 'stderr'\n"
	if string(retainedRead(t, c, "pg/postgresql.conf")) != want {
		t.Fatal("configuration differs from expected text")
	}
}

func TestPostgresPendingSweepPreservesAndQuiets(t *testing.T) {
	c, m := unitCell(t)
	for _, dir := range []string{"pg", "pg/base", "pg/base/1", "pg/pg_wal", "pg/global", "credentials", "pg/.pending-123", "pg/" + postgresPendingName()} {
		if privateDir(c.path(filepath.FromSlash(dir)), true) != nil {
			t.Fatal("cannot prepare synthetic directory")
		}
	}
	other := ".pending-" + strings.Repeat("b", 24)
	if runtime.GOOS == "windows" {
		other = ".pending-1234"
	}
	for _, name := range []string{"pg/.pending-notmine", "pg/" + other, "pg/.pending-123/child", "pg/PG_VERSION", "pg/postgresql.auto.conf", "pg/pg_wal/000000010000000000000001", "pg/global/pg_control", "sentinel", "journal/operations.jsonl"} {
		checkpointWrite(t, c, name, "synthetic preserved bytes")
	}
	for _, dir := range []string{"", "credentials", "pg/base/1"} {
		postgresPendingTemp(t, c.path(filepath.FromSlash(dir)))
	}
	if runtime.GOOS != "windows" {
		if os.Symlink(c.path("sentinel"), c.path("pg", ".pending-789")) != nil {
			t.Fatal("cannot prepare synthetic symlink")
		}
	}
	removed := []string{postgresPendingTemp(t, c.path("pg")), postgresPendingTemp(t, c.path("pg"))}
	before := postgresPendingInventory(t, c)
	postgresPendingConfigure(t, c, m)
	for _, path := range removed {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("own top-level residue survived configuration")
		}
		delete(before, "pg/"+filepath.Base(path))
	}
	after := postgresPendingInventory(t, c)
	delete(after, "pg/postgresql.conf")
	if runtime.GOOS != "windows" && (before["pg/.pending-789"] != after["pg/.pending-789"] || before["sentinel"] != after["sentinel"]) {
		t.Fatal("sweep changed an own-shape symlink or its target")
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("sweep changed a protected entry or journal")
	}
	before = postgresPendingInventory(t, c)
	postgresPendingConfigure(t, c, m)
	if !reflect.DeepEqual(before, postgresPendingInventory(t, c)) {
		t.Fatal("second configuration changed the tree or journal")
	}
}

func TestPostgresPendingShapes(t *testing.T) {
	// Rows are named by index: several shapes carry bytes that the CI stream
	// checker rejects in a test name.
	for index, row := range []struct {
		name          string
		unix, windows bool
	}{
		{".pending-0", true, false}, {".pending-1234567890", true, false},
		{".pending-" + strings.Repeat("1", 24), true, true},
		{".pending-abcdef0123456789abcdef01", false, true},
		{".pending-", false, false}, {".pending-+1", false, false},
		{".pending--1", false, false}, {".pending- 1", false, false},
		{".pending-" + strings.Repeat("A", 24), false, false},
		{".pending-" + strings.Repeat("a", 23), false, false},
		{".pending-" + strings.Repeat("a", 25), false, false},
		{".pending-" + strings.Repeat("1", 23), true, false},
		{".pending-" + strings.Repeat("1", 25), true, false},
		{".pending-١", false, false}, {".pending-１", false, false},
		{".pending-1/2", false, false}, {".pending-1\\2", false, false},
		{".pending-1.", false, false}, {"other.pending-1", false, false}, {"", false, false},
	} {
		t.Run(strconv.Itoa(index), func(t *testing.T) {
			if unixPrivateTempName(row.name) != row.unix || windowsPrivateTempName(row.name) != row.windows {
				t.Fatal("private temporary shape predicate disagrees with table")
			}
		})
	}
	c, _ := unitCell(t)
	for range 100 {
		name := filepath.Base(postgresPendingTemp(t, c.path("tmp")))
		if !privateTempName(name) {
			t.Fatal("real generator name was not recognized")
		}
	}
}

func TestPostgresPendingBestEffort(t *testing.T) {
	for _, failure := range []string{"list", "remove"} {
		t.Run(failure, func(t *testing.T) {
			c, m := unitCell(t)
			if privateDir(c.path("pg"), true) != nil {
				t.Fatal("cannot prepare synthetic PGDATA")
			}
			postgresPendingTemp(t, c.path("pg"))
			checkpointWrite(t, c, "journal/operations.jsonl", "synthetic journal")
			before := postgresPendingInventory(t, c)
			lists, removes := 0, 0
			c.postgresPendingTest = &postgresPendingOperations{
				readDir: func(path string) ([]os.DirEntry, error) {
					lists++
					if failure == "list" {
						return nil, os.ErrPermission
					}
					return os.ReadDir(path)
				},
				remove: func(string) error { removes++; return os.ErrPermission },
			}
			postgresPendingConfigure(t, c, m)
			after := postgresPendingInventory(t, c)
			delete(after, "pg/postgresql.conf")
			if !reflect.DeepEqual(before, after) {
				t.Fatal("best-effort failure changed residue or journal")
			}
			if lists != 1 || (failure == "list" && removes != 0) || (failure == "remove" && removes != 1) {
				t.Fatal("best-effort sweep retried or bypassed the failure seam")
			}
		})
	}
}

func TestPostgresPendingCheckpointObligation(t *testing.T) {
	for _, row := range []string{"top-level", "nested", "directory"} {
		t.Run(row, func(t *testing.T) {
			f, r := checkpointFixture(t, "legacy", false)
			options := checkpointOptions(f)
			k := checkpointCase{t, f, f.cell, r, &options}
			switch row {
			case "top-level":
				postgresPendingTemp(t, k.c.path("pg"))
			case "nested":
				postgresPendingTemp(t, k.c.path("pg", "base", "1"))
			case "directory":
				if privateDir(k.c.path("pg", postgresPendingName()), true) != nil {
					t.Fatal("cannot prepare pending directory")
				}
			}
			before := checkpointTreeState(t, k.c)
			if !checkpointSameError(k.c.Checkpoint(t.Context(), *k.options), "local_checkpoint_source_unsupported") {
				t.Fatal("pending residue did not refuse checkpoint")
			}
			checkpointUnchanged(t, k.c, before)
			postgresPendingConfigure(t, k.c, &k.f.manifest)
			before = checkpointTreeState(t, k.c)
			err := k.c.Checkpoint(t.Context(), *k.options)
			if row == "top-level" {
				if err != nil || len(checkpointIDs(t, k.c)) != 1 {
					t.Fatal("swept own residue did not unblock checkpoint")
				}
			} else {
				if !checkpointSameError(err, "local_checkpoint_source_unsupported") {
					t.Fatal("protected pending entry no longer refuses checkpoint")
				}
				checkpointUnchanged(t, k.c, before)
			}
		})
	}
}
