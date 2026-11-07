package localcell_test

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/localcell"
)

func assertLifecycleFixedCause(t *testing.T, root string, err error, operation, step, want string) {
	t.Helper()
	if err == nil || err.Error() != "local cell "+operation+": "+step+" failed: "+want {
		t.Fatalf("lifecycle fixed cause mismatch in returned error: %v", err)
	}
	data, readErr := os.ReadFile(filepath.Join(root, "journal", "operations.jsonl"))
	if readErr != nil {
		t.Fatal("cannot read lifecycle fixed cause journal")
	}
	found := false
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var entry struct{ Operation, Step, State, Cause string }
		if json.Unmarshal([]byte(line), &entry) != nil {
			t.Fatal("cannot decode lifecycle fixed cause journal")
		}
		if entry.Operation == operation && entry.Step == step && entry.State == "failed" {
			found = true
			if entry.Cause != want {
				t.Fatal("lifecycle journal retained the wrong fixed cause")
			}
		}
	}
	if !found {
		t.Fatal("lifecycle journal omitted the failed fixed cause")
	}
}

func testEmptyPostgresRecordCause(t *testing.T, binaries localcell.Binaries) {
	cell, root := disposableCell(t, binaries, nil)
	if err := lifecycleWriteFile(filepath.Join(root, "fixture-postgres-empty-record"), nil); err != nil {
		t.Fatal("cannot stage empty PostgreSQL record fixture")
	}
	t.Cleanup(func() {
		// This negative fixture exits immediately after publishing its empty
		// record. Remove that intentionally invalid record before normal repair.
		if err := os.Remove(filepath.Join(root, "pg", "postmaster.pid")); err != nil && !os.IsNotExist(err) {
			t.Error("cannot remove empty PostgreSQL fixture record")
		}
	})
	err := cell.Install(t.Context(), localcell.InstallOptions{})
	want := "PostgreSQL did not publish process ownership"
	if runtime.GOOS == "windows" {
		// The direct Windows launch retains its prelaunch identity and waits
		// through partial records, then observes this negative fixture's exit.
		want = "PostgreSQL exited before readiness"
	}
	assertLifecycleFixedCause(t, root, err, "install", "postgres", want)
}

func occupyLifecyclePort(t *testing.T, port int) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal("cannot occupy lifecycle fixture port")
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener
}

func testSavedPortPreflight(t *testing.T, binaries localcell.Binaries) {
	for _, tc := range []struct {
		name      string
		component string
		port      func(localcell.Ports) int
	}{
		{"postgres", "postgres", func(p localcell.Ports) int { return p.Postgres }},
		{"server_api", "server", func(p localcell.Ports) int { return p.ServerAPI }},
		{"server_health", "server", func(p localcell.Ports) int { return p.ServerHealth }},
		{"server_metrics", "server", func(p localcell.Ports) int { return p.ServerMetrics }},
		{"worker_health", "worker", func(p localcell.Ports) int { return p.WorkerHealth }},
		{"worker_metrics", "worker", func(p localcell.Ports) int { return p.WorkerMetrics }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cell, root := disposableCell(t, binaries, nil)
			if err := cell.Install(t.Context(), localcell.InstallOptions{}); err != nil {
				t.Fatalf("saved port fixture install failed: %v", err)
			}
			before := lostPortManifest(t, cell).Ports
			occupyLifecyclePort(t, tc.port(before))
			if err := cell.Start(t.Context()); err != nil {
				t.Fatalf("occupied saved port prevented recovered startup: %v", err)
			}
			after := assertReady(t, cell).Ports
			assertOnlyComponentPortsChanged(t, before, after, tc.component)
			assertPortRecoveryCauses(t, root, tc.component, false)
			if err := cell.Start(t.Context()); err != nil {
				t.Fatalf("owned running ports prevented idempotent startup: %v", err)
			}
			assertReady(t, cell)
			if got := lostPortManifest(t, cell).Ports; got != after {
				t.Fatal("idempotent startup changed already-running component ports")
			}
		})
	}
}

func testInstallPortReallocation(t *testing.T, binaries localcell.Binaries) {
	t.Run("ImmediateConflict", func(t *testing.T) {
		cell, _ := disposableCell(t, binaries, nil)
		allocations, occupied := 0, 0
		localcell.SetPortAllocationFailpointForTest(cell, func(ports localcell.Ports) {
			allocations++
			if allocations == 1 {
				occupied = ports.Postgres
				occupyLifecyclePort(t, occupied)
			}
		})
		if err := cell.Install(t.Context(), localcell.InstallOptions{LeaveRunning: true}); err != nil {
			t.Fatalf("immediate allocation conflict was not recovered: %v", err)
		}
		inspection, err := cell.Inspect(t.Context())
		if allocations != 2 || err != nil || inspection.Manifest == nil || inspection.Manifest.Ports.Postgres == occupied {
			t.Fatal("install did not replace its first occupied allocation")
		}
		if err := cell.Start(t.Context()); err != nil {
			t.Fatalf("owned PostgreSQL port prevented remaining startup: %v", err)
		}
		assertReady(t, cell)
	})
	t.Run("ThreeAttemptsOnly", func(t *testing.T) {
		cell, root := disposableCell(t, binaries, nil)
		allocations := 0
		localcell.SetPortAllocationFailpointForTest(cell, func(ports localcell.Ports) {
			allocations++
			occupyLifecyclePort(t, ports.Postgres)
		})
		err := cell.Install(t.Context(), localcell.InstallOptions{})
		if err == nil || err.Error() != "cannot allocate local cell loopback ports" || allocations != 3 {
			t.Fatal("install allocation did not stop after three conflicts")
		}
		if _, err := os.Stat(filepath.Join(root, "manifest.json")); !os.IsNotExist(err) {
			t.Fatal("failed allocation published an installation manifest")
		}
		if _, err := os.Stat(filepath.Join(root, "fixture-events")); !os.IsNotExist(err) {
			t.Fatal("failed allocation launched a lifecycle fixture process")
		}
	})
}
