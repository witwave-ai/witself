//go:build linux || darwin

package localcell

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

var errWriterIrregular, errWriterFailed = errors.New("writer row irregular"), errors.New("writer row failed")

func writerOpenRow(name string) error {
	f, err := openRegularWriter(name, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600, errWriterIrregular, errWriterFailed)
	if err == nil {
		_ = f.Close()
	}
	return err
}

func writerSize(name string) int64 {
	if info, err := os.Lstat(name); err == nil && info.Mode().IsRegular() {
		return info.Size()
	}
	return -1
}

// writerDrained reads what a FIFO reader holds, each read bounded.
func writerDrained(reader *os.File) (total int) {
	for n, buf := 1, make([]byte, 1<<16); reader != nil && n > 0; total += n {
		_ = reader.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		n, _ = reader.Read(buf)
	}
	return total
}

// writerFIFO makes a FIFO with a reader held and, when full, a filled pipe.
func writerFIFO(name string, full bool) (reader *os.File, filled int, err error) {
	if err = syscall.Mkfifo(name, 0o600); err == nil {
		reader, err = os.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	}
	if err != nil || !full {
		return reader, 0, err
	}
	writer, err := os.OpenFile(name, os.O_WRONLY|syscall.O_NONBLOCK, 0)
	for n := 0; err == nil; filled += n {
		_ = writer.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
		n, err = writer.Write(make([]byte, 4096))
	}
	return reader, filled, writer.Close()
}

type writerPlantRow struct {
	name  string
	plant func(name, target string) (reader *os.File, filled int, err error)
}

// writerNeverWaitRows plant at name or link to target; linux adds socket_node.
var writerNeverWaitRows = []writerPlantRow{
	{"fifo_no_reader", func(name, _ string) (*os.File, int, error) { return nil, 0, syscall.Mkfifo(name, 0o600) }},
	{"fifo_reader_held", func(name, _ string) (*os.File, int, error) { return writerFIFO(name, false) }},
	{"fifo_pipe_full", func(name, _ string) (*os.File, int, error) { return writerFIFO(name, true) }},
	{"link_to_file", func(name, target string) (*os.File, int, error) {
		return nil, 0, errors.Join(os.WriteFile(target, []byte(readerJournalLine), 0o600), os.Symlink(target, name))
	}},
	{"link_dangling", func(name, target string) (*os.File, int, error) { return nil, 0, os.Symlink(target, name) }},
	{"link_to_fifo", func(name, target string) (*os.File, int, error) {
		return nil, 0, errors.Join(syscall.Mkfifo(target, 0o600), os.Symlink(target, name))
	}},
	{"directory", func(name, _ string) (*os.File, int, error) { return nil, 0, os.Mkdir(name, 0o700) }},
}

// What is not a regular file at the name is irregular at once and nothing is
// written: a link's target keeps its size and a dangling one stays absent
// (each by its own Lstat, the parent a directory); a FIFO reader drains nothing.
func TestWriterOpenNeverWaits(t *testing.T) {
	for _, row := range writerNeverWaitRows {
		t.Run(row.name, func(t *testing.T) {
			dir := t.TempDir()
			name, target := filepath.Join(dir, "journal"), filepath.Join(dir, "target")
			reader, filled, err := row.plant(name, target)
			t.Cleanup(func() { _ = reader.Close() })
			if err != nil {
				t.Fatal("cannot plant the object the writer row opens")
			}
			size := writerSize(target)
			got := readerWatchdog(t, func() error { return writerOpenRow(name) }, func() {
				for _, fifo := range []string{name, target} {
					opened, _ := os.OpenFile(fifo, os.O_RDONLY|syscall.O_NONBLOCK, 0)
					_ = opened.Close()
				}
			})
			parent, parentErr := os.Lstat(dir)
			_, targetErr := os.Lstat(target)
			absent := row.name != "link_dangling" || parentErr == nil && parent.IsDir() && errors.Is(targetErr, os.ErrNotExist)
			if drained := writerDrained(reader) - filled; got != errWriterIrregular || writerSize(target) != size || !absent || drained != 0 {
				t.Fatalf("the writer open of the planted object was irregular %t, failed %t: target kept %t, dangling target absent %t, drained %d",
					got == errWriterIrregular, got == errWriterFailed, writerSize(target) == size, absent, drained)
			}
		})
	}
}

// writerNonblocking reads O_NONBLOCK on the handle's descriptor (Fd would clear it).
func writerNonblocking(f *os.File) bool {
	flags, flagsErr := 0, error(nil)
	raw, err := f.SyscallConn()
	if err == nil {
		err = raw.Control(func(fd uintptr) { flags, flagsErr = unix.FcntlInt(fd, unix.F_GETFL, 0) })
	}
	return err != nil || flagsErr != nil || flags&unix.O_NONBLOCK != 0
}

// A regular file, absent or existing, opens as a regular handle in blocking
// mode; an absent one is created private, and the helper writes nothing.
func TestWriterOpenRegular(t *testing.T) {
	for _, row := range []string{"absent", "existing"} {
		t.Run(row, func(t *testing.T) {
			name, want := filepath.Join(t.TempDir(), "journal"), int64(0)
			if row == "existing" {
				if want = int64(len(readerJournalLine)); os.WriteFile(name, []byte(readerJournalLine), 0o600) != nil {
					t.Fatal("cannot prepare the existing journal")
				}
			}
			var f *os.File
			err := readerWatchdog(t, func() (err error) {
				f, err = openRegularWriter(name, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600, errWriterIrregular, errWriterFailed)
				return err
			}, func() {})
			defer func() { _ = f.Close() }()
			info, statErr := f.Stat()
			named, namedErr := os.Lstat(name)
			if err != nil || statErr != nil || !info.Mode().IsRegular() || writerNonblocking(f) || namedErr != nil || named.Mode().Perm()&0o077 != 0 || named.Size() != want {
				t.Fatal("the writer open did not return a private regular file in blocking mode, with nothing written")
			}
		})
	}
}

// writerSwap moves the journal aside to name.saved and puts kind at its name:
// a FIFO (a reader held for fifo_reader_held), a link to it, a directory.
type writerSwap struct {
	mu         sync.Mutex
	kind, name string
	reader     *os.File
	err        error
}

func (s *writerSwap) put(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.name, s.err = name, os.Rename(name, name+".saved"); s.err == nil && s.kind == "fifo" {
		s.err = syscall.Mkfifo(name, 0o600)
	} else if s.err == nil && s.kind == "fifo_reader_held" {
		s.reader, _, s.err = writerFIFO(name, false)
	} else if s.err == nil && s.kind == "link_to_journal" {
		s.err = os.Symlink(name+".saved", name)
	} else if s.err == nil {
		s.err = os.Mkdir(name, 0o700)
	}
}

func (s *writerSwap) unblock() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reader == nil && s.name != "" {
		s.reader, _ = os.OpenFile(s.name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	}
}

// check: the site's open text, the moved journal's size kept, nothing drained;
// then the object goes and the journal moves back.
func (s *writerSwap) check(t *testing.T, got error, want string, size int64) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	moved, drained := writerSize(s.name+".saved"), writerDrained(s.reader)
	_ = s.reader.Close()
	restored := s.err == nil && s.name != "" && os.Remove(s.name) == nil && os.Rename(s.name+".saved", s.name) == nil
	if got == nil || got.Error() != want || moved != size || drained != 0 || !restored {
		t.Fatalf("the swapped site gave its open text %t: moved journal kept %t, drained %d, restored %t", got != nil && got.Error() == want, moved == size, drained, restored)
	}
}

// What is put at the journal's name after Repair's read is refused at once by
// the truncate and the retained sync with each site's one open text, nothing
// reaching the moved journal (HEAD acted through a link, waited on a FIFO).
func TestWriterRepairSitesRefuseSwapped(t *testing.T) {
	for _, kind := range []string{"fifo", "fifo_reader_held", "link_to_journal", "directory"} {
		t.Run("discard_tail/"+kind, func(t *testing.T) {
			c, progress := writerTornCell(t)
			name := readerJournalPath(c)
			size, s := writerSize(name), &writerSwap{kind: kind}
			s.put(name)
			s.check(t, readerWatchdog(t, func() error { return c.discardJournalTail(progress) }, s.unblock), "cannot open torn local cell journal", size)
			if c.discardJournalTail(progress) != nil || writerSize(name) != progress.durableBytes {
				t.Fatal("the journal moved back was not truncated to its durable prefix")
			}
		})
		t.Run("retained_sync/"+kind, func(t *testing.T) {
			c := writerRetainedCleanup(t)
			name := readerJournalPath(c)
			size, s := writerSize(name), &writerSwap{kind: kind}
			c.cfg.Failpoint = func(op, step string) error {
				if op == "remove" && step == "cleanup_journal" {
					s.put(name)
				}
				return nil
			}
			s.check(t, readerWatchdog(t, func() error { return c.Repair(t.Context()) }, s.unblock), "cannot open local cell journal", size)
			c.cfg.Failpoint = nil
			if m, err := readManifest(c.cfg.Root); err != nil || m.Cleanup == nil || c.Repair(t.Context()) != nil {
				t.Fatal("the refused retained sync cleared its cleanup, or repair did not settle with the journal back")
			}
		})
	}
}

// Two appenders racing through the writer open, beside a reader, keep every
// record whole and in per-writer order; the reader sees only growing prefixes
// and never an invalid journal (the records are steps of one open operation).
func TestWriterAppendsStayWhole(t *testing.T) {
	c := readerCell(t)
	name, ready, last, invalid := readerJournalPath(c), make(chan struct{}), []byte(nil), error(nil)
	ctx, stop := context.WithCancel(t.Context())
	var reader, writers sync.WaitGroup
	closeReady := sync.OnceFunc(func() { close(ready) })
	reader.Go(func() {
		defer closeReady()
		for invalid == nil && ctx.Err() == nil {
			if data, err := os.ReadFile(name); err != nil || !bytes.HasPrefix(data, last) {
				invalid = errors.Join(err, errors.New("a read was not a prefix of the next"))
			} else {
				last = data
				_, invalid = c.readJournal()
			}
			closeReady()
		}
	})
	<-ready // the reader runs before the appenders start
	errs := make([]error, 2)
	err := readerWatchdog(t, func() error {
		for w := range 2 {
			writers.Go(func() {
				for n := 0; n < 50 && errs[w] == nil; n++ {
					errs[w] = c.journalCause("install", "configure", "started", fmt.Sprintf("w%d:%d", w, n))
				}
			})
		}
		writers.Wait()
		return errors.Join(errs...)
	}, func() {})
	stop()
	reader.Wait()
	data, readErr := os.ReadFile(name)
	next, lines := []int{0, 0}, bytes.Split(bytes.TrimSuffix(data, []byte("\n")), []byte("\n"))
	for _, line := range lines[1:] {
		var entry journalEntry
		var w, n int
		if json.Unmarshal(line, &entry) != nil {
			break
		} else if _, err := fmt.Sscanf(entry.Cause, "w%d:%d", &w, &n); err == nil && w >= 0 && w < 2 && n == next[w] {
			next[w]++
		}
	}
	if err != nil || invalid != nil || readErr != nil || !bytes.HasPrefix(data, last) || !bytes.HasSuffix(data, []byte("\n")) || len(lines) != 101 || !slices.Equal(next, []int{50, 50}) {
		t.Fatal("appenders beside a reader lost, tore or reordered a record, or the reader saw an invalid journal")
	}
}
