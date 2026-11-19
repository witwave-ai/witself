//go:build linux

package localcell

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// leaseLayerRow is one LC-LEASE-2B declaration on a leased name in a cell
// that holds a fence, a complete checkpoint and both lock files; call says
// whether no handle, completeness, fence or lock came with its error; refusal
// is HEAD's text when the wait ends at the ceiling; prefix wraps a cut, else
// it is returned as it is.
type leaseLayerRow struct {
	name, leased, refusal, prefix string
	lease                         int
	call                          func(ctx context.Context, c *Cell, name string) (bool, error)
}

func leaseLayerRows() []leaseLayerRow {
	inspect, irregular, open, failed := errors.New("layer row inspect"), errors.New("layer row irregular"), errors.New("layer row open"), errors.New("layer row failed")
	handle := func(f *os.File, err error) (bool, error) { return f == nil, err }
	locked := func(lock *operationLock, err error) (bool, error) { return lock == nil, err }
	const fence, witness, owner, operation = "journal/maintenance.json", "recovery/" + leaseLayerID + "/" + checkpointWitnessName, "localcell: open owner lock: ", "localcell: open operation lock: "
	return []leaseLayerRow{
		{"openRegular", "tmp/leased", open.Error(), "", unix.F_WRLCK, func(ctx context.Context, _ *Cell, name string) (bool, error) {
			return handle(openRegular(ctx, name, inspect, irregular, open))
		}},
		{"openServiceRecord", "tmp/leased", open.Error(), "", unix.F_WRLCK, func(ctx context.Context, _ *Cell, name string) (bool, error) {
			return handle(openServiceRecord(ctx, name, open))
		}},
		{"openRegularWriter", "tmp/leased", failed.Error(), "", unix.F_RDLCK, func(ctx context.Context, _ *Cell, name string) (bool, error) {
			return handle(openRegularWriter(ctx, name, os.O_WRONLY|os.O_APPEND, 0o600, irregular, failed))
		}},
		{"checkpointComplete", witness, errCheckpointWitnessUnread.Error(), "", unix.F_WRLCK, func(ctx context.Context, _ *Cell, name string) (bool, error) {
			_, complete, err := checkpointComplete(ctx, filepath.Dir(name), leaseLayerID)
			return !complete, err
		}},
		{"fence_json", fence, "cannot open local cell record", "", unix.F_WRLCK, func(ctx context.Context, _ *Cell, name string) (bool, error) {
			var read maintenanceFence
			err := readMaintenanceFenceJSON(ctx, name, &read)
			return read == maintenanceFence{}, err
		}},
		{"fence", fence, errMaintenanceFenceActive.Error(), "", unix.F_WRLCK, func(ctx context.Context, c *Cell, _ string) (bool, error) {
			read, err := c.readMaintenanceFence(ctx)
			return read == nil, err
		}},
		{"acquireLock_owner", ".owner.lock", owner + "resource temporarily unavailable", owner, unix.F_RDLCK, func(ctx context.Context, c *Cell, _ string) (bool, error) {
			return locked(acquireLock(ctx, c.cfg.Root, false))
		}},
		{"acquireLock_operation", "run/operation.lock", operation + "resource temporarily unavailable", operation, unix.F_RDLCK, func(ctx context.Context, c *Cell, _ string) (bool, error) {
			return locked(acquireLock(ctx, c.cfg.Root, true))
		}},
	}
}

// Each LC-LEASE-2B declaration on a lease never let go (ceiling 2 s here)
// whose context is cancelled 200 ms in returns the cut soon after, as it is
// or wrapped by its own text: never its refusal, an absence, an incomplete
// checkpoint, an active fence or (nil, nil); nothing written, and a cut
// operation lock leaves no owner lock held. background: context.Background()
// with the kept holder (ceiling 300 ms) keeps HEAD's refusal at the ceiling.
func TestLeaseWaitLayer(t *testing.T) {
	for _, row := range leaseLayerRows() {
		t.Run(row.name, func(t *testing.T) {
			t.Run("background", func(t *testing.T) { leaseLayerRun(t, row, true) })
			leaseLayerRun(t, row, false)
		})
	}
}

func leaseLayerRun(t *testing.T, row leaseLayerRow, background bool) {
	c, _ := unitCell(t)
	updateTestFence(t, c)
	checkpointPlant(t, c, leaseLayerID, leaseLayerID)
	name := c.path(filepath.FromSlash(row.leased))
	if os.WriteFile(c.path("tmp", "leased"), []byte(readerJournalLine), 0o600) != nil || !leaseLayerRelocked(c) {
		t.Fatal("cannot prepare the layer row cell")
	}
	before, readErr := os.ReadFile(name)
	l := leaseHolder(t, "kept", row.lease)
	if !background {
		leaseWait.ceiling = 2 * time.Second
	}
	leaseTaken(t, l, name)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	point := make(chan time.Time, 1)
	if background {
		ctx = context.Background()
	} else {
		time.AfterFunc(200*time.Millisecond, func() { point <- time.Now(); cancel() })
	}
	empty, started := false, time.Now()
	err := readerWatchdog(t, func() (err error) { empty, err = row.call(ctx, c, name); return err }, l.release)
	returned, cut := time.Now(), (*leaseWaitCut)(nil)
	if _, broken := leaseContextBroken(l); !broken {
		t.Fatal("the holder never saw the break of the layer row lease")
	}
	l.close() // a plain read of a write-leased file would wait out the kernel's break
	if now, nowErr := os.ReadFile(name); readErr != nil || nowErr != nil || !bytes.Equal(before, now) {
		t.Fatal("the leased file changed during the layer row")
	}
	if !empty {
		t.Fatal("the layer returned a handle, completeness, fence or lock beside its error")
	}
	// A cut operation lock left no owner lock held: a second acquireLock gets both.
	if _, statErr := os.Stat(c.path("run")); row.prefix != "" && (statErr != nil || !leaseLayerRelocked(c)) {
		t.Fatal("the layer row left its owner lock held or its run directory gone")
	}
	if background {
		if err == nil || errors.As(err, &cut) || err.Error() != row.refusal || returned.Sub(started) < leaseKeptAfter {
			t.Fatal("the placeholder context did not keep the layer refusal at the ceiling")
		}
		return
	}
	if !errors.As(err, &cut) {
		t.Fatal("the layer did not return the lease wait cut")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatal("the layer cut does not carry the context error")
	}
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, os.ErrNotExist) {
		t.Fatal("the layer cut reads as a lease refusal or an absent file")
	}
	if row.prefix == "" && err != error(cut) || !strings.HasPrefix(err.Error(), row.prefix) {
		t.Fatal("the layer did not return its cut as it is or wrapped by its own text")
	}
	if at := <-point; returned.Before(at) || returned.After(at.Add(leaseWait.longest+150*time.Millisecond)) {
		t.Fatal("the layer did not return soon after its context was done")
	}
}

func leaseLayerRelocked(c *Cell) bool {
	lock, err := acquireLock(context.Background(), c.cfg.Root, true)
	return err == nil && lock != nil && lock.Close() == nil
}
