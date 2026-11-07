package localcell_test

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/localcell"
)

func TestLostPortLifecycle(t *testing.T) {
	binaries := buildLifecycleFixture(t)
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	for _, component := range []string{"server", "worker"} {
		t.Run(component+"/ExistingChildDiesDuringStart", func(t *testing.T) {
			testExistingChildDiesDuringStart(t, binaries, component)
		})
		t.Run(component+"/FirstConflict", func(t *testing.T) {
			cell, root := installedLostPortCell(t, binaries)
			before := lostPortManifest(t, cell).Ports
			writeLostPortMarker(t, root, "fixture-steal-port-"+component, "once")
			if err := cell.Start(t.Context()); err != nil {
				t.Fatalf("lost-port first failure did not recover: %v", err)
			}
			after := assertReady(t, cell).Ports
			assertOnlyComponentPortsChanged(t, before, after, component)
			assertFixtureLaunches(t, root, component, 2)
			other := "server"
			if component == other {
				other = "worker"
			}
			assertFixtureLaunches(t, root, other, 1)
			assertPortRecoveryCauses(t, root, component, true)
			assertFixtureBindLog(t, root, component, 1)
		})
		t.Run(component+"/ConflictExhausted", func(t *testing.T) {
			cell, root := installedLostPortCell(t, binaries)
			writeLostPortMarker(t, root, "fixture-steal-port-"+component, "always")
			err := cell.Start(t.Context())
			want := "local cell start: " + component + "_ready failed: local cell " + component + " exited before readiness (exit status 1): witself-" + component + ": listener bind failed: address in use; wait_observation=wait_completed recovered_alive=false" // owner merge adaptation: C2a2 readinessExitError carries the observation diagnostic
			if err == nil || err.Error() != want {
				t.Fatalf("lost-port exhaustion discarded attributable error: %v", err)
			}
			assertFixtureLaunches(t, root, component, 2)
			assertPortRecoveryCauses(t, root, component, true)
			assertFixtureBindLog(t, root, component, 2)
			assertStopped(t, cell)
		})
		t.Run(component+"/OtherBindFailure", func(t *testing.T) {
			cell, root := installedLostPortCell(t, binaries)
			before := lostPortManifest(t, cell).Ports
			writeLostPortMarker(t, root, "fixture-bind-other-"+component, "")
			err := cell.Start(t.Context())
			want := "local cell start: " + component + "_ready failed: local cell " + component + " exited before readiness (exit status 1): witself-" + component + ": listener bind failed: other; wait_observation=wait_completed recovered_alive=false" // owner merge adaptation: C2a2 readinessExitError carries the observation diagnostic
			if err == nil || err.Error() != want {
				t.Fatalf("other bind failure discarded attributable error: %v", err)
			}
			assertFixtureLaunches(t, root, component, 1)
			if lostPortManifest(t, cell).Ports != before {
				t.Fatal("other bind failure changed saved ports")
			}
			journal := readLostPortJournal(t, root)
			if strings.Contains(journal, "port_reallocated=") || strings.Contains(journal, "port_conflict_relaunch=") {
				t.Fatal("other bind failure recorded a port recovery cause")
			}
			assertStopped(t, cell)
		})
	}
	t.Run("NonAttributableExit", func(t *testing.T) {
		cell, root := installedLostPortCell(t, binaries)
		before := lostPortManifest(t, cell).Ports
		writeLostPortMarker(t, root, "fixture-server-exit", "")
		err := cell.Start(t.Context())
		if err == nil || !strings.Contains(err.Error(), "exit status 7") || !strings.Contains(err.Error(), "migration wait exceeded") {
			t.Fatalf("non-bind fixture exit lost its existing attribution: %v", err)
		}
		assertFixtureLaunches(t, root, "server", 1)
		if lostPortManifest(t, cell).Ports != before {
			t.Fatal("non-attributable exit changed saved ports")
		}
		if strings.Contains(readLostPortJournal(t, root), "port_conflict_relaunch=") {
			t.Fatal("non-attributable exit was relaunched")
		}
		assertStopped(t, cell)
	})
	t.Run("SavedServerPortThief", func(t *testing.T) {
		cell, root := installedLostPortCell(t, binaries)
		before := lostPortManifest(t, cell).Ports
		occupyLifecyclePort(t, before.ServerHealth)
		if err := cell.Start(t.Context()); err != nil {
			t.Fatalf("saved-port thief was not recovered before launch: %v", err)
		}
		assertOnlyComponentPortsChanged(t, before, assertReady(t, cell).Ports, "server")
		assertFixtureLaunches(t, root, "server", 1)
		assertPortRecoveryCauses(t, root, "server", false)
	})
	t.Run("ReadinessBudget", func(t *testing.T) { testLostPortReadinessBudget(t, binaries) })
}

// Start's entry snapshot is not a lifetime guarantee. Kill an owned child only
// after that snapshot to require attributable recovery and replacement rollback
// for a child that was running when Start entered.
func testExistingChildDiesDuringStart(t *testing.T, binaries localcell.Binaries, component string) {
	cell, root := installedLostPortCell(t, binaries)
	started := time.Now()
	if err := cell.Start(t.Context()); err != nil {
		t.Fatalf("existing-child calibration start failed: %v", err)
	}
	singleStart := time.Since(started)
	before := assertReady(t, cell).Ports
	records := make(map[string]localcell.ProcessRecord)
	for _, peer := range []string{"postgres", "server", "worker"} {
		records[peer] = lostPortProcessRecord(t, root, peer)
	}
	step := "postgres"
	if component == "worker" {
		step = "account"
	}
	writeLostPortMarker(t, root, "fixture-steal-port-"+component, "always")
	killed := false
	restarted, err := localcell.New(localcell.Config{Root: root, Binaries: binaries, Timeout: 20 * singleStart,
		Failpoint: func(operation, completed string) error {
			if operation != "start" || completed != step || killed {
				return nil
			}
			previous := records[component]
			if alive, err := localcell.RecordAliveForTest(previous); err != nil || !alive {
				return errors.New("existing-child fixture lost its owned identity before the kill seam")
			}
			process, err := os.FindProcess(previous.PID)
			if err != nil {
				return errors.New("cannot locate existing owned fixture child")
			}
			defer func() { _ = process.Release() }()
			if err := process.Kill(); err != nil {
				return errors.New("cannot kill existing owned fixture child")
			}
			deadline := time.Now().Add(20 * singleStart)
			for {
				alive, err := localcell.RecordAliveForTest(previous)
				if err != nil {
					return errors.New("cannot observe killed fixture child identity")
				}
				if !alive {
					break
				}
				if !time.Now().Before(deadline) {
					return errors.New("killed fixture child exceeded calibrated exit bound")
				}
				time.Sleep(singleStart / 20)
			}
			killed = true
			return nil
		}})
	if err != nil {
		t.Fatal("cannot construct existing-child recovery cell")
	}
	err = restarted.Start(t.Context())
	want := "local cell start: " + component + "_ready failed: local cell " + component + " exited before readiness (exit status 1): witself-" + component + ": listener bind failed: address in use; wait_observation=wait_completed recovered_alive=false" // owner merge adaptation: C2a2 readinessExitError carries the observation diagnostic
	if !killed || err == nil || err.Error() != want {
		t.Fatalf("existing-child death did not retain replacement failure attribution: %v", err)
	}
	assertFixtureLaunches(t, root, component, 3)
	inspection, err := restarted.Inspect(t.Context())
	if err != nil || inspection.Manifest == nil || inspection.Processes[component].Alive {
		t.Fatal("failed replacement child survived startup rollback")
	}
	assertOnlyComponentPortsChanged(t, before, inspection.Manifest.Ports, component)
	for _, peer := range []string{"postgres", "server", "worker"} {
		if peer == component {
			continue
		}
		if alive, err := localcell.RecordAliveForTest(records[peer]); err != nil || !alive || lostPortProcessRecord(t, root, peer) != records[peer] {
			t.Fatalf("existing-child recovery disturbed an untouched owned peer: %s", peer)
		}
	}
	counts := make(map[string]int)
	for _, line := range strings.Split(strings.TrimSpace(readLostPortJournal(t, root)), "\n") {
		var entry struct{ Cause string }
		if json.Unmarshal([]byte(line), &entry) != nil {
			t.Fatal("cannot decode existing-child recovery journal")
		}
		if strings.HasPrefix(entry.Cause, "port_reallocated=") || strings.HasPrefix(entry.Cause, "port_conflict_relaunch=") {
			counts[entry.Cause]++
		}
	}
	// Another test can acquire the old listener after the owned child exits.
	// That permits one preflight allocation in addition to the required retry.
	reallocations := counts["port_reallocated="+component]
	if len(counts) != 2 || reallocations < 1 || reallocations > 2 || counts["port_conflict_relaunch="+component] != 1 {
		t.Fatal("existing-child replacement has invalid recovery cause counts")
	}
}

func lostPortProcessRecord(t *testing.T, root, component string) localcell.ProcessRecord {
	t.Helper()
	// Match New's parent canonicalization without accepting an aliased record.
	parent, err := filepath.EvalSymlinks(filepath.Dir(root))
	if err != nil {
		t.Fatal("cannot canonicalize owned fixture root")
	}
	root = filepath.Join(parent, filepath.Base(root))
	data, err := os.ReadFile(filepath.Join(root, "run", component+".json"))
	var record localcell.ProcessRecord
	if err != nil || json.Unmarshal(data, &record) != nil || record.Component != component || record.Root != root || record.PID <= 1 || record.StartTime == "" {
		t.Fatal("cannot retain owned fixture process identity")
	}
	return record
}

// Calibrate using a complete Start of the same fixture, including its process
// publication and HTTP readiness. The first bind failure consumes over half the
// readiness budget; the second launch cannot become ready within the remainder.
// A fresh retry budget crosses the elapsed bound, even if that retry succeeds.
func testLostPortReadinessBudget(t *testing.T, binaries localcell.Binaries) {
	calibration, _ := installedLostPortCell(t, binaries)
	start := time.Now()
	if err := calibration.Start(t.Context()); err != nil {
		t.Fatalf("lost-port timing calibration failed: %v", err)
	}
	singleStart := time.Since(start)
	assertReady(t, calibration)
	if err := calibration.Stop(t.Context()); err != nil {
		t.Fatal("lost-port timing calibration did not stop")
	}
	budget := 21 * singleStart
	var readinessStarted time.Time
	cell, root := disposableCell(t, binaries, nil)
	if err := cell.Install(t.Context(), localcell.InstallOptions{}); err != nil {
		t.Fatal("lost-port budget fixture install failed")
	}
	cell, err := localcell.New(localcell.Config{Root: root, Binaries: binaries, Timeout: singleStart, MigrationWait: 20 * singleStart,
		Failpoint: func(operation, step string) error {
			if operation == "start" && step == "server" {
				readinessStarted = time.Now()
			}
			return nil
		}})
	if err != nil {
		t.Fatal("cannot construct lost-port budget cell")
	}
	writeLostPortMarker(t, root, "fixture-steal-port-server", "once")
	writeLostPortMarker(t, root, "fixture-steal-port-delay-server", (12 * singleStart).String())
	writeLostPortMarker(t, root, "fixture-retry-delay-server", (18 * singleStart).String())
	err = cell.Start(t.Context())
	elapsed := time.Since(readinessStarted)
	if readinessStarted.IsZero() {
		t.Fatalf("lost-port budget fixture never reached server readiness: %v", err)
	}
	if elapsed > budget+5*singleStart {
		t.Fatalf("lost-port retry extended the readiness budget: elapsed=%s budget=%s single-start=%s", elapsed, budget, singleStart)
	}
	if err == nil {
		t.Fatal("lost-port retry became ready outside the original readiness budget")
	}
	assertFixtureLaunches(t, root, "server", 2)
	assertPortRecoveryCauses(t, root, "server", true)
	assertStopped(t, cell)
	t.Logf("lost-port retry budget: single-start=%s budget=%s elapsed=%s", singleStart, budget, elapsed)
}

func TestFixtureListenerDiagnostics(t *testing.T) {
	binaries := buildLifecycleFixture(t)
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	for _, component := range []string{"server", "worker"} {
		for _, category := range []string{"address in use", "other", "non-bind"} {
			t.Run(component+"/"+category, func(t *testing.T) {
				cell, root := installedLostPortCell(t, binaries)
				manifest := lostPortManifest(t, cell)
				listener, err := net.Listen("tcp4", "127.0.0.1:0")
				if err != nil {
					t.Fatal("cannot bind fixture diagnostic test listener")
				}
				defer func() { _ = listener.Close() }()
				address := listener.Addr().String()
				database := manifest.Database
				want := "witself-" + component + ": listener bind failed: " + category + "\n"
				if category == "other" {
					address = "invalid-fixture-listen-address"
				}
				if category == "non-bind" {
					database = "wrong-fixture-database"
					want = "synthetic lifecycle fixture failed\n"
				}
				binary := binaries.Server
				if component == "worker" {
					binary = binaries.Worker
				}
				command := exec.CommandContext(t.Context(), binary)
				command.Dir = root
				command.Env = []string{"WITSELF_DATABASE_URL=postgres://fixture/" + database, "WITSELF_HEALTH_ADDR=" + address,
					"WITSELF_METRICS_ADDR=127.0.0.1:0", "WITSELF_API_ADDR=127.0.0.1:0"}
				output, err := command.CombinedOutput()
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 1 {
					t.Fatal("fixture listener failure did not exit nonzero")
				}
				if string(output) != want {
					t.Fatal("fixture listener diagnostic was not the exact single fixed line")
				}
			})
		}
	}
}

func installedLostPortCell(t *testing.T, binaries localcell.Binaries) (*localcell.Cell, string) {
	t.Helper()
	cell, root := disposableCell(t, binaries, nil)
	if err := cell.Install(t.Context(), localcell.InstallOptions{}); err != nil {
		t.Fatalf("lost-port fixture install failed: %v", err)
	}
	return cell, root
}

func lostPortManifest(t *testing.T, cell *localcell.Cell) *localcell.Manifest {
	t.Helper()
	inspection, err := cell.Inspect(t.Context())
	if err != nil || inspection.Manifest == nil {
		t.Fatal("cannot inspect lost-port fixture manifest")
	}
	return inspection.Manifest
}

func writeLostPortMarker(t *testing.T, root, name, value string) {
	t.Helper()
	if err := lifecycleWriteFile(filepath.Join(root, name), []byte(value)); err != nil {
		t.Fatal("cannot write lost-port fixture marker")
	}
}

func assertFixtureLaunches(t *testing.T, root, component string, want int) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "run", "fixture-launches-"+component))
	if err != nil || string(data) != strconv.Itoa(want) {
		t.Fatalf("lost-port %s launch count differs from %d", component, want)
	}
}

func assertOnlyComponentPortsChanged(t *testing.T, before, after localcell.Ports, component string) {
	t.Helper()
	groups := []struct {
		name   string
		before []int
		after  []int
	}{
		{"postgres", []int{before.Postgres}, []int{after.Postgres}},
		{"server", []int{before.ServerAPI, before.ServerHealth, before.ServerMetrics}, []int{after.ServerAPI, after.ServerHealth, after.ServerMetrics}},
		{"worker", []int{before.WorkerHealth, before.WorkerMetrics}, []int{after.WorkerHealth, after.WorkerMetrics}},
	}
	for _, group := range groups {
		for i := range group.before {
			changed := group.before[i] != group.after[i]
			if changed != (group.name == component) {
				t.Fatalf("lost-port recovery changed the wrong component port set: %s", group.name)
			}
		}
	}
}

func readLostPortJournal(t *testing.T, root string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "journal", "operations.jsonl"))
	if err != nil {
		t.Fatal("cannot read lost-port journal")
	}
	return string(data)
}

func assertPortRecoveryCauses(t *testing.T, root, component string, relaunch bool) {
	t.Helper()
	counts := make(map[string]int)
	for _, line := range strings.Split(strings.TrimSpace(readLostPortJournal(t, root)), "\n") {
		var entry struct{ Cause string }
		if json.Unmarshal([]byte(line), &entry) != nil {
			t.Fatal("cannot decode lost-port journal entry")
		}
		if strings.HasPrefix(entry.Cause, "port_reallocated=") || strings.HasPrefix(entry.Cause, "port_conflict_relaunch=") {
			counts[entry.Cause]++
		}
	}
	if counts["port_reallocated="+component] != 1 {
		t.Fatal("lost-port journal did not record exactly one component reallocation")
	}
	wantRelaunch := 0
	if relaunch {
		wantRelaunch = 1
	}
	if counts["port_conflict_relaunch="+component] != wantRelaunch || len(counts) != 1+wantRelaunch {
		t.Fatal("lost-port journal did not retain the exact value-free recovery causes")
	}
}

func assertFixtureBindLog(t *testing.T, root, component string, count int) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "logs", component+".log"))
	want := strings.Repeat("witself-"+component+": listener bind failed: address in use\n", count)
	if err != nil || string(data) != want {
		t.Fatal("lost-port fixture log did not contain exactly the attributable failures")
	}
}
