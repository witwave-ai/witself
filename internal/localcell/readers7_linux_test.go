//go:build linux

package localcell

import (
	"context"
	"os"
	"path"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// A read or write lease held on the log when it is opened is waited out
// (LC-LEASE-1): a holder that lets go at the break costs a moment and the log
// opens; one kept past the ceiling (300 ms here) gives the log's open text when
// the wait ends, nothing written, and its lease reads F_UNLCK.
func TestLogLeaseWaitedOut(t *testing.T) {
	for name, lease := range map[string]int{"read/lets_go": unix.F_RDLCK, "read/kept": unix.F_RDLCK, "write/lets_go": unix.F_WRLCK, "write/kept": unix.F_WRLCK} {
		t.Run(name, func(t *testing.T) {
			c, _ := unitCell(t)
			log, l, held := c.path("logs", "server.log"), leaseHolder(t, path.Base(name), lease), -1
			if os.WriteFile(log, []byte(readerJournalLine), 0o600) != nil {
				t.Fatal("cannot prepare the leased log")
			}
			started := time.Now()
			leaseTaken(t, l, log)
			got := readerWatchdog(t, func() error { f, err := c.logFile(context.Background(), "server"); _ = f.Close(); return err }, l.release)
			l.do(func(fd uintptr) { held, _ = unix.FcntlInt(fd, unix.F_GETLEASE, 0) })
			if path.Base(name) == "lets_go" && (got != nil || !l.broken()) || path.Base(name) == "kept" && (got == nil || got.Error() != "cannot open local cell log" ||
				writerSize(log) != int64(len(readerJournalLine)) || held != unix.F_UNLCK || time.Since(started) < leaseKeptAfter) {
				t.Fatal("the leased log open was not waited out, or a kept lease not refused with its open text and nothing written")
			}
		})
	}
}
