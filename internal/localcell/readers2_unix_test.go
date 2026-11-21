//go:build linux || darwin

package localcell

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/witwave-ai/witself/internal/store"
)

func init() { reader2Rows = append(reader2Rows, reader2UnixRows...) }

// These readers' fixtures need POSIX modes. The service manager readers keep
// their pathname checks; their existing inspection seam plants just after the
// check of the file itself.
var reader2UnixRows = []reader2Row{
	// An empty pidfile is an identity mismatch, never "not running".
	{"postmaster", "invalid PostgreSQL process record", "invalid PostgreSQL process record", "PostgreSQL data directory identity mismatch", func(t *testing.T, plant func(string)) (string, func() error) {
		c, _ := unitCell(t)
		path := reader2PostgresFile(t, c, "postmaster.pid", "4242\n"+c.path("pg")+"\n1700000000\n")
		return path, func() error {
			plant(path)
			pid, err := c.postmaster(context.Background())
			if err == nil && pid != 4242 {
				return errors.New("postmaster content lost")
			}
			return err
		}
	}},
	{"reinstallPGVersion", errReinstallState.Error(), errReinstallState.Error(), errReinstallVersion.Error(), func(t *testing.T, plant func(string)) (string, func() error) {
		c, m := unitCell(t)
		m.RemovalRetained, m.AccountID, m.LastVerifiedSchemaVersion = true, "acc_aaaaaaaaaaaaaaaa", store.SchemaVersion()
		path := reader2PostgresFile(t, c, "PG_VERSION", "16\n")
		return path, func() error { plant(path); return c.reinstallAvailable(m, journalProgress{}) }
	}},
	{"launchdPlist", "plist_file:identity", "plist_file:open", "plist_file:content", func(t *testing.T, plant func(string)) (string, func() error) {
		a, d, _ := launchdFixture(t)
		want, err := RenderLaunchdAgentPlist(d)
		if err != nil || a.inspectDirectories(true) != nil || os.WriteFile(a.plistPath(d), want, 0o600) != nil {
			t.Fatal("cannot prepare the reader plist")
		}
		a.inspectFile = reader2PlantAfter(a.plistPath(d), plant)
		return a.plistPath(d), func() error { _, err := a.ownedPlist(d, want); return err }
	}},
	{"systemdUnit", "unit_file:type", "unit_file:read", "unit_file:content", func(t *testing.T, plant func(string)) (string, func() error) {
		a, d, _ := systemdFixture(t)
		want, err := RenderSystemdUserUnit(d)
		if err != nil || a.inspectDirectories(true) != nil || os.WriteFile(a.unitPath(d), want, 0o600) != nil {
			t.Fatal("cannot prepare the reader unit")
		}
		a.inspectFile = reader2PlantAfter(a.unitPath(d), plant)
		return a.unitPath(d), func() error {
			owned, err := a.ownedUnit(d, want)
			if err == nil && !owned {
				return errors.New("unit content lost")
			}
			return err
		}
	}},
}

// reader2PostgresFile writes one regular file in a new private pg directory.
func reader2PostgresFile(t *testing.T, c *Cell, name, content string) string {
	t.Helper()
	if os.Mkdir(c.path("pg"), 0o700) != nil || os.WriteFile(c.path("pg", name), []byte(content), 0o600) != nil {
		t.Fatal("cannot prepare the reader postgres file")
	}
	return c.path("pg", name)
}

// reader2PlantAfter inspects like Lstat and plants once, after the first
// inspection of path has already returned what it saw.
func reader2PlantAfter(path string, plant func(string)) func(string) (os.FileInfo, error) {
	planted := false
	return func(name string) (os.FileInfo, error) {
		info, err := os.Lstat(name)
		if name == path && !planted {
			planted = true
			plant(name)
		}
		return info, err
	}
}

// A FIFO planted where each reader's own check gives way to its open is refused
// at once. No writer is opened unless the read has already blocked: a blocked
// reader counts as a reader, so that open succeeds, releases it and the row
// still fails.
func TestReaders2RefuseFIFOWithoutWaiting(t *testing.T) {
	for _, row := range reader2Rows {
		t.Run(row.name, func(t *testing.T) {
			planted := false
			path, read := row.setup(t, func(name string) {
				planted = os.Remove(name) == nil && syscall.Mkfifo(name, 0o600) == nil
			})
			err := readerWatchdog(t, read, func() {
				if writer, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
					_ = writer.Close()
				}
			})
			if !planted || err == nil || !strings.Contains(err.Error(), row.fifo) {
				t.Fatalf("reader did not refuse a fifo as irregular: %v", err)
			}
		})
	}
}

// A link planted at the name is refused, never followed, though it names the
// valid file itself.
func TestReaders2RefuseLinkAtTheName(t *testing.T) {
	for _, row := range reader2Rows {
		t.Run(row.name, func(t *testing.T) {
			planted := false
			_, read := row.setup(t, func(name string) {
				planted = os.Rename(name, name+".target") == nil && os.Symlink(name+".target", name) == nil
			})
			err := readerWatchdog(t, read, func() {})
			if !planted || err == nil || !strings.Contains(err.Error(), row.link) {
				t.Fatalf("reader did not refuse a link at its name: %v", err)
			}
		})
	}
}

// A secret that is not private is refused as today, whether it opens (judged
// on the handle) or cannot be opened (judged by the Lstat after the failure).
func TestReaders2NotPrivateStaysRefused(t *testing.T) {
	for _, row := range reader2Rows {
		if row.name != "password" && row.name != "serviceControlSecret" {
			continue
		}
		for _, mode := range []os.FileMode{0o644, 0} {
			t.Run(fmt.Sprintf("%s/mode=%04o", row.name, mode), func(t *testing.T) {
				if mode == 0 && os.Geteuid() == 0 {
					t.Skip("root opens a mode-0 file")
				}
				path, read := row.setup(t, func(string) {})
				if os.Chmod(path, mode) != nil {
					t.Fatal("cannot change the reader file mode")
				}
				if err := readerWatchdog(t, read, func() {}); err == nil || !strings.Contains(err.Error(), row.fifo) {
					t.Fatalf("file that is not private was not refused: %v", err)
				}
			})
		}
	}
}
