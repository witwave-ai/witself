//go:build linux || darwin

package localcell

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// A FIFO at the log's name, with no writer or one held, yields no diagnostic at
// once; a link there is refused though its target, outside the cell, has one.
func init() {
	readers4LogRows["fifo"] = func(_ *testing.T, log string) (string, bool) { return "", syscall.Mkfifo(log, 0o600) == nil }
	readers4LogRows["fifo_writer_held"] = func(t *testing.T, log string) (string, bool) {
		_ = syscall.Mkfifo(log, 0o600)
		reader, err := os.OpenFile(log, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return "", false
		}
		writer, err := os.OpenFile(log, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			t.Cleanup(func() { _ = writer.Close() })
		}
		return "", reader.Close() == nil && err == nil
	}
	readers4LogRows["link"] = func(t *testing.T, log string) (string, bool) {
		target := filepath.Join(t.TempDir(), "outside.log")
		return "", os.WriteFile(target, []byte(readers4Diagnostic+"\n"), 0o600) == nil && os.Symlink(target, log) == nil
	}
}
