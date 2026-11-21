//go:build linux

package localcell

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// The start command under a kept lease (ceiling 2 s) on the real start, stop and
// repair hooks: observe 0.3 s after the holder's first break is answered within a
// tenth of the ceiling as busy; the start's own outcome is HEAD's (LC-LEASE-2-SPEC1
// H36, H18, H45, H54).
func TestServiceWorkerLease(t *testing.T) {
	for _, row := range []struct {
		name, file, state, start string
		lease, failures          int
		refused, faulted         bool
	}{
		{"manifest", "manifest.json", "stopped", "", unix.F_WRLCK, 0, true, false},
		{"journal", "journal/operations.jsonl", "faulted", "cannot open local cell journal", unix.F_RDLCK, 0, false, true},
		{"owner_lock", ".owner.lock", "stopped", "", unix.F_RDLCK, 0, true, false},
		{"password", "run/postgres.password", "backoff", "local cell password file is invalid", unix.F_WRLCK, 1, false, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			_, m := unitCell(t)
			root, err := os.MkdirTemp("/tmp", "lc-worker-")
			if err != nil {
				t.Fatal("cannot create the short worker lease root")
			}
			t.Cleanup(func() { _ = os.RemoveAll(root) })
			c, err := New(Config{Root: root, Timeout: 30 * time.Second})
			if err != nil || c.layout() != nil || writeAtomic(c.path("manifest.json"), m, nil) != nil || selectedFixtureDataDirectory(c) != nil || c.makePassword() != nil {
				t.Fatal("cannot prepare the worker lease cell")
			}
			prepareSupervisorTest(t, c)
			admissionRecordStart(c, &[]string{}, nil)
			var mu sync.Mutex
			starts := []string{}
			hooks, _, _, _, _ := supervisorTestHooks()
			hooks.ticks, hooks.boot, hooks.stop, hooks.repair = make(chan time.Time), func() (string, error) { return "synthetic-boot", nil }, c.stopServiceChildren, c.Repair
			hooks.start = func(ctx context.Context) error {
				err := c.Start(ctx)
				mu.Lock()
				defer mu.Unlock()
				starts = append(starts, fmt.Sprint(err))
				return err
			}
			l, ceiling := leaseHolder(t, "kept", row.lease), 2*time.Second
			leaseWait.ceiling = ceiling // Before Run: no host reads it while it changes.
			ctx, cancel := context.WithCancel(t.Context())
			done := StartJoinedTestRunForTest(t, cancel, serviceControlBudget(c.cfg.Timeout, 4), func() error {
				return c.Run(ctx, SupervisorPolicy{hooks: &hooks, ShutdownTimeout: time.Second})
			})
			(&serviceWorkerHost{cell: c}).ready(t)
			if status, err := c.Control(t.Context(), "stop"); err != nil || status.State != "stopped" {
				t.Fatal("the worker lease host did not stop cleanly before the lease")
			}
			mu.Lock()
			starts = nil
			mu.Unlock()
			if l.take(c.path(strings.Split(row.file, "/")...)); !l.do(func(uintptr) {}) {
				t.Fatal("this kernel or sandbox refused the lease the worker row needs")
			}
			started := make(chan serviceReply, 1)
			go func() {
				status, err := c.Control(t.Context(), "start")
				started <- serviceReply{status: status, err: err}
			}()
			for asked := time.Now(); !l.broken(); time.Sleep(time.Millisecond) {
				if time.Since(asked) > ceiling {
					t.Fatal("the start command never broke the held lease")
				}
			}
			time.Sleep(ceiling * 3 / 20)
			asked := time.Now()
			status, err := c.Control(t.Context(), "observe")
			if time.Since(asked) >= ceiling/10 {
				t.Fatal("observe during the lease wait was not answered within a tenth of the ceiling")
			}
			if err != nil || status.State != "busy" || status.Reason != "service_operation_in_progress" {
				t.Fatal("observe during the lease wait did not read busy")
			}
			reply := <-started
			l.release()
			l.close()
			if row.refused != (reply.err != nil) || reply.err != nil && reply.err.Error() != "localcell: service_control_action_failed" || !row.refused && reply.status.State != row.state {
				t.Fatal("the start under the kept lease changed its reply")
			}
			mu.Lock()
			got := slices.Clone(starts)
			mu.Unlock()
			if !slices.Equal(got, slices.DeleteFunc([]string{row.start}, func(s string) bool { return s == "" })) {
				t.Fatal("the start under the kept lease changed its start hook outcome")
			}
			if status, err := c.Control(t.Context(), "observe"); err != nil || status.State != row.state {
				t.Fatal("the host after the kept lease changed its observed state")
			}
			cancel()
			if <-done != nil {
				t.Fatal("the worker lease host did not shut down cleanly")
			}
			var record supervisorRecord
			if _, err := c.ServiceDescriptor(); err != nil || readJSON(c.path("service", "supervisor.json"), &record) != nil {
				t.Fatal("the worker lease host did not shut down to readable records")
			}
			if d, _ := c.ServiceDescriptor(); d.Faulted != row.faulted {
				t.Fatal("the start under the kept lease changed the descriptor's fault")
			}
			if len(record.Failures) != row.failures {
				t.Fatal("the start under the kept lease changed the failure count")
			}
		})
	}
}
