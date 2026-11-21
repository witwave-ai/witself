//go:build linux

package localcell

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// leaseJournalRow is one LC-LEASE-2C1 declaration on a cell planted by
// leaseJournalPlant (prepare), met by a lease never let go on leased (taken by
// the call itself when inside); call says whether no progress came beside its
// error; kept checks what the cut left, from the journal before the call;
// refusal is HEAD's text at the ceiling; repair: Repair must then finish.
type leaseJournalRow struct {
	name, prepare, leased, refusal string
	lease                          int
	inside, repair                 bool
	call                           func(ctx context.Context, c *Cell, m *Manifest, p journalProgress, l *writerLease) (bool, error)
	kept                           func(c *Cell, before []byte, err error) bool
}

func leaseJournalRows() []leaseJournalRow {
	const journal, opened, read, torn = "journal/operations.jsonl", "cannot open local cell journal", "cannot read local cell journal", "cannot open torn local cell journal"
	refused := errors.New("journal row scratch open refused")
	same := func(c *Cell, before []byte, _ error) bool {
		now, err := os.ReadFile(readerJournalPath(c))
		return err == nil && bytes.Equal(now, before)
	}
	unbegun := func(c *Cell, before []byte, err error) bool {
		inspection, inspectErr := c.Inspect(context.Background())
		return same(c, before, err) && inspectErr == nil && inspection.Interrupted == nil
	}
	ends := func(tail string) func(*Cell, []byte, error) bool {
		return func(c *Cell, _ []byte, _ error) bool {
			records := leaseJournalRecords(c)
			return strings.HasSuffix(strings.Join(records, " "), tail) && !slices.Contains(records, "stop:done")
		}
	}
	plain := func(call func(ctx context.Context, c *Cell, m *Manifest, p journalProgress, l *writerLease) error) func(context.Context, *Cell, *Manifest, journalProgress, *writerLease) (bool, error) {
		return func(ctx context.Context, c *Cell, m *Manifest, p journalProgress, l *writerLease) (bool, error) {
			return true, call(ctx, c, m, p, l)
		}
	}
	return []leaseJournalRow{
		{"append", "", journal, opened, unix.F_RDLCK, false, false, plain(func(ctx context.Context, c *Cell, _ *Manifest, _ journalProgress, _ *writerLease) error {
			return c.journal(ctx, "stop", "operation", "started")
		}), same},
		{"read", "", journal, read, unix.F_WRLCK, false, false, func(ctx context.Context, c *Cell, _ *Manifest, _ journalProgress, _ *writerLease) (bool, error) {
			progress, err := c.readJournal(ctx)
			return reflect.DeepEqual(progress, journalProgress{}), err
		}, same},
		{"begin", "", journal, opened, unix.F_RDLCK, false, false, plain(func(ctx context.Context, c *Cell, _ *Manifest, _ journalProgress, _ *writerLease) error {
			return c.begin(ctx, "stop")
		}), unbegun},
		{"discard", "torn", journal, torn, unix.F_RDLCK, false, true, plain(func(ctx context.Context, c *Cell, _ *Manifest, p journalProgress, _ *writerLease) error {
			return c.discardJournalTail(ctx, p)
		}), same},
		{"step_fn_cut", "stop", "tmp/leased", "local cell stop: stop failed", unix.F_WRLCK, false, true, plain(func(ctx context.Context, c *Cell, _ *Manifest, _ journalProgress, _ *writerLease) error {
			return c.step(ctx, "stop", "stop", func() error {
				f, err := openRegular(ctx, c.path("tmp", "leased"), refused, refused, refused)
				if err == nil {
					_ = f.Close()
				}
				return err
			})
		}), func(c *Cell, before []byte, err error) bool {
			return err.Error() == "local cell stop: stop failed: local cell lease wait ended: context canceled" && ends("operation:started stop:started stop:failed")(c, before, err)
		}},
		{"step_done_cut", "stop", journal, opened, unix.F_RDLCK, true, true, plain(func(ctx context.Context, c *Cell, _ *Manifest, _ journalProgress, l *writerLease) error {
			return c.step(ctx, "stop", "stop", func() error { l.take(readerJournalPath(c)); return nil })
		}), ends("operation:started stop:started")},
		{"finish_after_manifest", "stop", journal, opened, unix.F_RDLCK, false, true, plain(func(ctx context.Context, c *Cell, m *Manifest, _ journalProgress, _ *writerLease) error {
			return c.finish(ctx, m, "stop")
		}), func(c *Cell, before []byte, err error) bool {
			m, manifestErr := readManifest(context.Background(), c.cfg.Root)
			progress, readErr := c.readJournal(context.Background())
			return same(c, before, err) && manifestErr == nil && m.LastOperation == "stop" && readErr == nil && progress.pending != nil && progress.pending.Operation == "stop"
		}},
		{"retained_removal_after_checkpoint", "remove", journal, opened, unix.F_RDLCK, false, true, plain(func(ctx context.Context, c *Cell, m *Manifest, _ journalProgress, _ *writerLease) error {
			return c.finishRetainedRemoval(ctx, m)
		}), func(c *Cell, before []byte, err error) bool {
			m, manifestErr := readManifest(context.Background(), c.cfg.Root)
			return same(c, before, err) && manifestErr == nil && m.Cleanup != nil && m.Cleanup.Operation == "remove" && m.Cleanup.Step == "runtime_cleanup"
		}},
	}
}

// leaseJournalRecords lists the journal's records as step:state.
func leaseJournalRecords(c *Cell) (records []string) {
	data, _ := os.ReadFile(readerJournalPath(c))
	for line := range strings.Lines(string(data)) {
		var entry journalEntry
		if json.Unmarshal([]byte(line), &entry) == nil {
			records = append(records, entry.Step+":"+entry.State)
		}
	}
	return records
}

// Each LC-LEASE-2C1 declaration on a lease never let go (ceiling 2 s here)
// whose context is cancelled 200 ms in returns the cut soon after (step wraps
// it), never a refusal, an absence or a progress, and leaves what a crash at
// that point leaves: nothing written, or the step started, or failed, never
// done; then Repair finishes. background: context.Background() with the kept
// holder (ceiling 300 ms) keeps HEAD's refusal at the ceiling.
func TestLeaseWaitJournal(t *testing.T) {
	for _, row := range leaseJournalRows() {
		t.Run(row.name, func(t *testing.T) {
			t.Run("background", func(t *testing.T) { leaseJournalRun(t, row, true) })
			leaseJournalRun(t, row, false)
		})
	}
}

func leaseJournalRun(t *testing.T, row leaseJournalRow, background bool) {
	c, m := unitCell(t)
	progress, err := leaseJournalPlant(c, m, row.prepare)
	if err != nil || os.WriteFile(c.path("tmp", "leased"), []byte(readerJournalLine), 0o600) != nil {
		t.Fatal("cannot prepare the journal row cell")
	}
	before, readErr := os.ReadFile(readerJournalPath(c))
	l := leaseHolder(t, "kept", row.lease)
	if !background {
		leaseWait.ceiling = 2 * time.Second
	}
	if !row.inside {
		leaseTaken(t, l, c.path(filepath.FromSlash(row.leased)))
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	point := make(chan time.Time, 1)
	if background {
		ctx = context.Background()
	} else {
		time.AfterFunc(200*time.Millisecond, func() { point <- time.Now(); cancel() })
	}
	empty, started := false, time.Now()
	err = readerWatchdog(t, func() (err error) { empty, err = row.call(ctx, c, m, progress, l); return err }, l.release)
	returned, cut := time.Now(), (*leaseWaitCut)(nil)
	if !l.do(func(uintptr) {}) {
		t.Fatal("this kernel or sandbox refused the lease the row needs")
	}
	if _, broken := leaseContextBroken(l); !broken {
		t.Fatal("the holder never saw the break of the journal row lease")
	}
	l.close() // a plain read of a write-leased file would wait out the kernel's break
	if !empty {
		t.Fatal("the journal layer returned a progress beside its error")
	}
	if background {
		if err == nil || errors.As(err, &cut) || err.Error() != row.refusal || returned.Sub(started) < leaseKeptAfter {
			t.Fatal("the placeholder context did not keep the journal refusal at the ceiling")
		}
		return
	}
	if !errors.As(err, &cut) {
		t.Fatal("the journal layer did not return the lease wait cut")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatal("the journal layer cut does not carry the context error")
	}
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, os.ErrNotExist) {
		t.Fatal("the journal layer cut reads as a lease refusal or an absent file")
	}
	if at := <-point; returned.Before(at) || returned.After(at.Add(leaseWait.longest+150*time.Millisecond)) {
		t.Fatal("the journal layer did not return soon after its context was done")
	}
	if readErr != nil || !row.kept(c, before, err) {
		t.Fatal("the journal row cut did not leave what a crash at that point leaves")
	}
	if row.repair {
		if c.Repair(context.Background()) != nil {
			t.Fatal("repair did not finish what the journal row cut left open")
		}
		if inspection, inspectErr := c.Inspect(context.Background()); inspectErr != nil || inspection.Interrupted != nil || inspection.JournalTailIncomplete {
			t.Fatal("the cell still reports an interruption after the journal row repair")
		}
	}
}
