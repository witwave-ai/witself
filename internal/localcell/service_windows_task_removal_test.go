package localcell

import (
	"bytes"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
)

func windowsTaskRecordedLiveChild(t *testing.T, c *Cell, component string) ProcessRecord {
	t.Helper()
	identity, alive, err := processIdentity(os.Getpid())
	if err != nil || !alive || identity == "" {
		t.Fatal("live child fixture identity unavailable")
	}
	record := ProcessRecord{PID: os.Getpid(), StartTime: identity, Root: c.cfg.Root, Component: component}
	if writeAtomic(c.path("run", component+".json"), record, nil) != nil {
		t.Fatal("live child fixture publication failed")
	}
	return record
}

// A surviving process after a host crash must preserve registration and custody.
// The test process is the recorded child, so no scheduler or signal is needed.
func TestReviewNoHostRemovalMustRefuseLiveRecord(t *testing.T) {
	for _, component := range []string{"worker", "server", "postgres"} {
		t.Run(component, func(t *testing.T) {
			c, _ := descriptorTestCell(t)
			windowsTaskRecordedLiveChild(t, c, component)
			path := c.path("run", component+".json")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal("cannot snapshot child fixture")
			}
			err = c.exitServiceHostForRemoval(t.Context())
			if !errors.Is(err, ErrServiceControlRefused) || err.Error() != "localcell: service_control_refused: host_exit:child_tree_live" {
				t.Fatal("no host lease authorized removal with a live recorded child")
			}
			d, err := c.ServiceDescriptor()
			if err != nil || d.DesiredState != "stopped" {
				t.Fatal("live child refusal did not persist stopped intent")
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("live child refusal changed recovery records")
			}
		})
	}
}

func TestWindowsTaskNoHostRemovalUnknownTree(t *testing.T) {
	for _, name := range []string{"malformed", "wrong_root", "wrong_component", "invalid_pid", "missing_start", "pending_launch", "reused_pid"} {
		t.Run(name, func(t *testing.T) {
			c, _ := descriptorTestCell(t)
			record := windowsTaskRecordedLiveChild(t, c, "worker")
			path := c.path("run", "worker.json")
			want := "child_tree_unknown"
			switch name {
			case "wrong_root":
				record.Root += "-foreign"
			case "wrong_component":
				record.Component = "server"
			case "invalid_pid":
				record.PID = 1
			case "missing_start":
				record.StartTime = ""
			case "pending_launch":
				record.LaunchPending = true
			case "reused_pid":
				record.StartTime = "different-incarnation"
				want = "child_tree_live"
			}
			if writeAtomic(path, record, nil) != nil {
				t.Fatal("unknown child fixture publication failed")
			}
			if name == "malformed" && writeAtomicBytes(path, []byte("{"), nil) != nil {
				t.Fatal("malformed child fixture publication failed")
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal("cannot snapshot unknown child fixture")
			}
			err = c.exitServiceHostForRemoval(t.Context())
			if !errors.Is(err, ErrServiceControlRefused) || err.Error() != "localcell: service_control_refused: host_exit:"+want {
				t.Fatal("unknown child tree authorized removal")
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("unknown child refusal changed recovery records")
			}
		})
	}
	t.Run("empty_tree", func(t *testing.T) {
		c, _ := descriptorTestCell(t)
		if c.exitServiceHostForRemoval(t.Context()) != nil {
			t.Fatal("empty recorded child tree refused removal")
		}
		d, err := c.ServiceDescriptor()
		if err != nil || d.DesiredState != "stopped" {
			t.Fatal("empty tree removal lost stopped intent")
		}
	})
}

func TestWindowsTaskNoHostRemovalPostmasterRecoveryRecord(t *testing.T) {
	for _, malformed := range []bool{false, true} {
		name := "live_without_run_record"
		if malformed {
			name = "unknown_without_run_record"
		}
		t.Run(name, func(t *testing.T) {
			c, _ := descriptorTestCell(t)
			if testMkdir(c.path("pg"), 0o700) != nil {
				t.Fatal("postmaster recovery directory fixture failed")
			}
			raw := []byte(strconv.Itoa(os.Getpid()) + "\n" + c.path("pg") + "\n1\n")
			want := "child_tree_live"
			if malformed {
				raw, want = []byte("unproven\n"), "child_tree_unknown"
			}
			path := c.path("pg", "postmaster.pid")
			if testWriteFile(path, raw, 0o600) != nil {
				t.Fatal("postmaster recovery record fixture failed")
			}
			if _, err := os.Stat(c.path("run", "postgres.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("postmaster recovery fixture unexpectedly has a run record")
			}
			err := c.exitServiceHostForRemoval(t.Context())
			if !errors.Is(err, ErrServiceControlRefused) || err.Error() != "localcell: service_control_refused: host_exit:"+want {
				t.Fatal("postmaster recovery record did not prevent no-host removal")
			}
			after, err := readPostgresFile(path)
			if err != nil || !bytes.Equal(after, raw) {
				t.Fatal("no-host refusal changed postmaster recovery authority")
			}
			d, err := c.ServiceDescriptor()
			if err != nil || d.DesiredState != "stopped" {
				t.Fatal("postmaster refusal did not persist stopped intent")
			}
		})
	}
}

func TestWindowsTaskRemovalUsesProductionHostExit(t *testing.T) {
	c, _ := descriptorTestCell(t)
	windowsTaskRecordedLiveChild(t, c, "worker")
	f := newWindowsTaskFake(t)
	f.apply()
	f.a.drain = nil
	constructed := false
	f.a.removalCell = func(ServiceDescriptor) (*Cell, error) {
		constructed = true
		return c, nil
	}
	// Let every later fence pass if production host exit is bypassed. This
	// isolates the drain call, rather than crediting a later descriptor refusal.
	f.current.DesiredState = "stopped"
	f.drained = true
	err := f.a.Remove(t.Context(), f.d)
	var failure *windowsTaskFailure
	if !constructed || !errors.As(err, &failure) || failure.step != "remove:host_exit" || failure.reason != "host_exit_failed" || f.count("/Delete") != 0 {
		t.Fatal("production host-exit refusal failed to prevent task deletion")
	}
	d, err := c.ServiceDescriptor()
	if err != nil || d.DesiredState != "stopped" {
		t.Fatal("production drain did not persist stopped intent")
	}
	for _, path := range []string{f.a.xmlPath(f.d), f.a.receiptPath(f.d), c.path("run", "worker.json")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatal("production drain refusal discarded registration or recovery records")
		}
	}
	if f.xml == nil {
		t.Fatal("production drain refusal unregistered task")
	}
}

func TestWindowsTaskApplyRechecksPrivateRecordsBeforeCreate(t *testing.T) {
	f := newWindowsTaskFake(t)
	plan, err := f.a.Plan(t.Context(), f.d)
	if err != nil {
		t.Fatal("private recheck fixture plan failed")
	}
	staged := false
	f.afterQuery = func() {
		if f.count("/Query") == 3 {
			staged = true
			if os.Remove(f.a.receiptPath(f.d)) != nil {
				t.Fatal("private recheck fixture did not reach durable staging")
			}
		}
	}
	err = f.a.Apply(t.Context(), plan)
	if !staged || !errors.Is(err, ErrServiceOwnershipConflict) || !strings.Contains(err.Error(), "reason=private_pair_required") || f.count("/Create") != 0 {
		t.Fatal("private record recheck did not refuse before manager create")
	}
	if _, err := os.Stat(f.a.xmlPath(f.d)); err != nil || f.xml != nil {
		t.Fatal("private record recheck changed recovery XML or registered task")
	}
}
