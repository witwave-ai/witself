package localcell

import (
	"context"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
)

// LC-LEASE-2A R3: with its context already done, each waiting open still makes
// its first attempt, so an unleased regular file opens on every system; one
// check per expectation, each with its own text (Windows and macOS run it in
// CI only).
func TestLeaseWaitContextUnleased(t *testing.T) {
	done, cancel := context.WithCancel(context.Background())
	cancel()
	for _, row := range []struct {
		name string
		open func(t *testing.T) (*os.File, error)
	}{
		{"reader", func(t *testing.T) (*os.File, error) {
			c, _ := unitCell(t)
			return openPrivateReader(done, c.path("manifest.json"))
		}},
		{"writer", func(t *testing.T) (*os.File, error) {
			return openPrivateWriter(done, readerJournalPath(readerCell(t)), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		}},
		{"lock", func(t *testing.T) (*os.File, error) {
			c, _ := unitCell(t)
			return openLockFile(done, c.path(".owner.lock"), true)
		}},
		{"checkpoint", func(t *testing.T) (*os.File, error) {
			f, _ := checkpointFixture(t, "legacy", false)
			open, release, err := f.cell.openCheckpointSource(done)
			if err != nil {
				t.Fatal("the checkpoint source refused to open under a done context")
			}
			t.Cleanup(release)
			return open("base/1/1259")
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			f, err := row.open(t)
			if err != nil {
				t.Fatal("a waiting open refused an unleased regular file under a done context")
			}
			if f == nil {
				t.Fatal("a waiting open returned no handle for an unleased regular file")
			}
			if f.Close() != nil {
				t.Fatal("the handle opened under a done context did not close")
			}
		})
	}
}

// The LC-LEASE-2A rows of the lease wait contract, with LF and CRLF endings:
// each mutation reports its key with both.
func TestLeaseWaitContextSourceContract(t *testing.T) {
	sources := map[string]string{}
	for name, source := range admissionReadSources(t) {
		sources[name] = strings.ReplaceAll(source, "\r\n", "\n")
	}
	const pause = "\t\tpause := time.NewTimer(delay)\n\t\tselect {\n\t\tcase <-leaseWaitDone(ctx):\n\t\t\tpause.Stop()\n\t\t\treturn result, &leaseWaitCut{cause: ctx.Err()}\n\t\tcase <-pause.C:\n\t\t}\n"
	const cut = "\tif cut := (*leaseWaitCut)(nil); errors.As(err, &cut) {\n\t\treturn nil, cut\n\t}\n"
	const classified = "\tif err != nil {\n\t\tinfo, err := os.Lstat(path)\n\t\tswitch {\n\t\tcase errors.Is(err, os.ErrNotExist) && absentBelowDirectory(path):\n\t\t\treturn nil, os.ErrNotExist\n" +
		"\t\tcase err != nil:\n\t\t\treturn nil, inspect\n\t\tcase !regularFile(info, accept):\n\t\t\treturn nil, irregular\n\t\t}\n\t\treturn nil, open\n\t}\n"
	const reader = "func openPrivateReader(ctx context.Context, path string) (*os.File, error) {\n\treturn waitOutLease("
	for _, row := range []struct{ name, file, before, after, want string }{
		{"wait_sleep_restored", "files_other.go", pause, "\t\ttime.Sleep(delay)\n", "lease_wait_body"},
		{"wait_cut_dropped", "files_other.go", "return result, &leaseWaitCut{cause: ctx.Err()}", "return result, err", "lease_wait_body"},
		{"reader_context_dropped", "files_other.go", reader + "ctx,", reader + "context.Background(),", "lease_wait_caller:files_other.go:openPrivateReader"},
		{"lock_context_dropped", "lock_unix.go", "waitOutLease(ctx, path, os.Lstat,", "waitOutLease(context.Background(), path, os.Lstat,", "lease_wait_lock"},
		{"placeholder_added", "lifecycle.go", "func (c *Cell) Start(ctx context.Context) error {\n", "func (c *Cell) Start(ctx context.Context) error {\n" +
			"\tif f, err := openPrivateReader(context.Background(), c.path(\"manifest.json\")); err == nil {\n\t\t_ = f.Close()\n\t}\n", "lease_wait_placeholder:lifecycle.go:(*Cell).Start"},
		{"regular_by_classifies_cut", "files.go", cut + classified, classified + cut, "lease_wait_cut"},
	} {
		t.Run(row.name, func(t *testing.T) {
			if strings.Count(sources[row.file], row.before) != 1 {
				t.Fatal("lease wait context mutation anchor is not unique")
			}
			mutated := maps.Clone(sources)
			mutated[row.file] = strings.Replace(sources[row.file], row.before, row.after, 1)
			for _, ending := range []string{"\n", "\r\n"} {
				ended := map[string]string{}
				for name, source := range mutated {
					ended[name] = strings.ReplaceAll(source, "\n", ending)
				}
				if failures := leaseWaitContract(ended); !slices.Contains(failures, row.want) {
					t.Fatalf("lease wait context contract reported keys %v without the row key", failures)
				}
			}
		})
	}
}
