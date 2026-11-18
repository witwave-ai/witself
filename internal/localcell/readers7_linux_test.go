//go:build linux

package localcell

import (
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

// A read or write lease held on the log when it is opened is refused at once
// with the log's open text, where HEAD waited for the break; nothing is
// written, and the holder's lease reads F_UNLCK: the break has begun.
func TestLogLeaseRefusedAtOnce(t *testing.T) {
	for name, lease := range map[string]int{"read": unix.F_RDLCK, "write": unix.F_WRLCK} {
		t.Run(name, func(t *testing.T) {
			c, _ := unitCell(t)
			log, l, held := c.path("logs", "server.log"), &writerLease{}, -1
			t.Cleanup(l.close)
			if os.WriteFile(log, []byte(readerJournalLine), 0o600) != nil {
				t.Fatal("cannot prepare the leased log")
			}
			f, err := os.Open(log)
			_, leaseErr := unix.FcntlInt(f.Fd(), unix.F_SETLEASE, lease)
			if l.file = f; err != nil || leaseErr != nil {
				t.Fatal("this kernel or sandbox refused the lease the log row needs")
			}
			got := readerWatchdog(t, func() error { f, err := c.logFile("server"); _ = f.Close(); return err }, l.release)
			l.do(func(fd uintptr) { held, _ = unix.FcntlInt(fd, unix.F_GETLEASE, 0) })
			if got == nil || got.Error() != "cannot open local cell log" || writerSize(log) != int64(len(readerJournalLine)) || held != unix.F_UNLCK {
				t.Fatal("the leased log open was not refused at once with its open text and nothing written")
			}
		})
	}
}
