//go:build linux || darwin

package localcell

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const readers7ChildLine = "readers7 child output blocking\n"

// TestLogChildHelper is the child: one write saying whether the standard
// output it inherited is blocking, then exit before the framework writes.
func TestLogChildHelper(_ *testing.T) {
	if os.Args[len(os.Args)-1] == "readers7-log-child" {
		flags, err := unix.FcntlInt(1, unix.F_GETFL, 0)
		_, _ = os.Stdout.WriteString(map[bool]string{true: readers7ChildLine, false: "readers7 child output nonblocking\n"}[err == nil && flags&unix.O_NONBLOCK == 0])
		os.Exit(0)
	}
}

// What is not a regular file at the log's name is refused at once with the
// log's text, nothing written: a link's target keeps its size, a dangling
// one stays absent (its own Lstat, logs/ a directory), a FIFO drains nothing.
func TestLogOpenNeverWaits(t *testing.T) {
	for _, row := range writerNeverWaitRows {
		t.Run(row.name, func(t *testing.T) {
			c, _ := unitCell(t)
			name, target := c.path("logs", "server.log"), c.path("logs", "target")
			reader, filled, err := row.plant(name, target)
			t.Cleanup(func() { _ = reader.Close() })
			size := writerSize(target)
			got := readerWatchdog(t, func() error { f, err := c.logFile(context.Background(), "server"); _ = f.Close(); return err }, func() {
				for _, fifo := range []string{name, target} {
					opened, _ := os.OpenFile(fifo, os.O_RDONLY|syscall.O_NONBLOCK, 0)
					_ = opened.Close()
				}
			})
			logs, logsErr := os.Lstat(c.path("logs"))
			_, targetErr := os.Lstat(target)
			absent := row.name != "link_dangling" || logsErr == nil && logs.IsDir() && errors.Is(targetErr, os.ErrNotExist)
			if drained := writerDrained(reader) - filled; err != nil || got == nil || got.Error() != "local cell log must be a regular file" || writerSize(target) != size || !absent || drained != 0 {
				t.Fatal("the planted object at the log's name was not refused at once with nothing written through it")
			}
		})
	}
}

// The log's handle is a regular file in blocking mode, and a child started
// with it as standard output and error (the test binary) finds it blocking
// and appends its one line whole after what the log held.
func TestLogChildInheritsBlocking(t *testing.T) {
	for name, held := range map[string]string{"absent": "", "existing": "earlier line\n"} {
		t.Run(name, func(t *testing.T) {
			c, _ := unitCell(t)
			log, f, err := c.path("logs", "server.log"), (*os.File)(nil), error(nil)
			if held != "" {
				err = os.WriteFile(log, []byte(held), 0o600)
			}
			err = errors.Join(err, readerWatchdog(t, func() (err error) { f, err = c.logFile(context.Background(), "server"); return err }, func() {}))
			info, statErr := f.Stat()
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLogChildHelper$", "--", "readers7-log-child")
			// A test binary built with coverage warns on its standard error at exit
			// when it has no coverage directory; that error stream is the log here.
			child.Env, child.Stdout, child.Stderr = []string{"GOCOVERDIR=" + t.TempDir()}, f, f
			blocking := err == nil && !writerNonblocking(f)
			runErr := child.Run()
			data, readErr := os.ReadFile(log)
			if errors.Join(err, statErr, runErr, f.Close(), readErr) != nil || !info.Mode().IsRegular() || !blocking || string(data) != held+readers7ChildLine {
				t.Fatal("the log handle or the child's inherited output was not a blocking regular file, or the child's line was not appended whole")
			}
		})
	}
}
