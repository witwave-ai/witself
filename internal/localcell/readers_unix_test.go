//go:build linux || darwin

package localcell

import (
	"os"
	"syscall"
	"testing"
)

// A FIFO at the name is refused at once as not a regular file. No writer is
// opened unless the read has already blocked: the blocked reader counts as a
// reader, so that open succeeds, releases it and the row still fails.
func TestReadersRefuseFIFOWithoutWaiting(t *testing.T) {
	for _, row := range readerRows {
		t.Run(row.name, func(t *testing.T) {
			c := readerCell(t)
			path := row.path(c)
			if os.Remove(path) != nil || syscall.Mkfifo(path, 0o600) != nil {
				t.Fatal("cannot prepare the reader fifo")
			}
			err := readerWatchdog(t, func() error { return row.read(c) }, func() {
				if writer, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
					_ = writer.Close()
				}
			})
			if err == nil || err.Error() != row.refusal {
				t.Fatalf("reader did not refuse a fifo as irregular: %v", err)
			}
		})
	}
}

// A link at the name is refused, never followed, though it names a valid file.
func TestReadersRefuseLinkAtTheName(t *testing.T) {
	for _, row := range readerRows {
		t.Run(row.name, func(t *testing.T) {
			c := readerCell(t)
			path := row.path(c)
			if os.Rename(path, path+".target") != nil || os.Symlink(path+".target", path) != nil {
				t.Fatal("cannot prepare the reader link")
			}
			err := readerWatchdog(t, func() error { return row.read(c) }, func() {})
			if err == nil || err.Error() != row.refusal {
				t.Fatalf("reader did not refuse a link as irregular: %v", err)
			}
		})
	}
}
