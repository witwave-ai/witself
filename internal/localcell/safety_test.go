//go:build darwin || linux || windows

package localcell

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// signalSafetyChild is disposable so a mutation that removes a signal guard
// cannot terminate the test runner. Its creation identity is equivalent to an
// unrelated live test process for the Stop ownership decision.
func signalSafetyChild(t *testing.T) ProcessRecord {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestProcessIdentityHelperProcess$", "--", "localcell-process-helper")
	cmd.Env = []string{}
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = io.Discard
	configureProcessCommand(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = input.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	line, err := bufio.NewReader(output).ReadString('\n')
	if err != nil || line != "ready\n" {
		t.Fatal("signal safety child did not become ready")
	}
	identity, alive, err := processIdentity(cmd.Process.Pid)
	if err != nil || !alive || identity == "" {
		t.Fatal("signal safety child identity unavailable")
	}
	return ProcessRecord{PID: cmd.Process.Pid, StartTime: identity}
}

func requireSafetyChildAlive(t *testing.T, record ProcessRecord) {
	t.Helper()
	identity, alive, err := processIdentity(record.PID)
	if err != nil || !alive || identity != record.StartTime {
		t.Fatal("Stop signaled a process before proving every required ownership check")
	}
}

func TestStopRefusesUnrelatedLiveProcessIdentity(t *testing.T) {
	c, _ := unitCell(t)
	child := signalSafetyChild(t)
	fabricated := child
	fabricated.Root, fabricated.Component = c.cfg.Root, "server"
	fabricated.StartTime = "unrelated-creation-identity"
	if err := writeAtomic(c.path("run", "server.json"), fabricated, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Stop(t.Context()); err == nil {
		t.Fatal("Stop accepted an unrelated live process identity")
	}
	requireSafetyChildAlive(t, child)
}

func TestStopRefusesCurrentTestProcessIdentity(t *testing.T) {
	c, _ := unitCell(t)
	fabricated := ProcessRecord{PID: os.Getpid(), StartTime: "unrelated-creation-identity", Root: c.cfg.Root, Component: "server"}
	if err := writeAtomic(c.path("run", "server.json"), fabricated, nil); err != nil {
		t.Fatal(err)
	}
	// The mutation witness uses the disposable child above. This assertion
	// additionally covers the specifically requested current-test-process PID.
	if err := c.Stop(t.Context()); err == nil {
		t.Fatal("Stop accepted the current test process as a cell component")
	}
}

func TestStopRefusesForeignPostmasterDirectoryBeforeSignals(t *testing.T) {
	c, m := unitCell(t)
	server := signalSafetyChild(t)
	server.Root, server.Component = c.cfg.Root, "server"
	if err := writeAtomic(c.path("run", "server.json"), server, nil); err != nil {
		t.Fatal(err)
	}
	postgres := signalSafetyChild(t)
	postgres.Root, postgres.Component = c.cfg.Root, "postgres"
	if err := writeAtomic(c.path("run", "postgres.json"), postgres, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(c.path("pg"), 0o700); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(t.TempDir(), "foreign-pg")
	pidFile := fmt.Sprintf("%d\n%s\n1\n%s\n\n127.0.0.1\n1\nready\n", postgres.PID, foreign, strconv.Itoa(m.Ports.Postgres))
	if err := os.WriteFile(c.path("pg", "postmaster.pid"), []byte(pidFile), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.Stop(t.Context()); err == nil {
		t.Fatal("Stop accepted a foreign PostgreSQL data directory")
	}
	requireSafetyChildAlive(t, server)
	requireSafetyChildAlive(t, postgres)
}

func TestPostgresRecoveryRequiresNativeStartEpoch(t *testing.T) {
	c, m := unitCell(t)
	child := signalSafetyChild(t)
	started, err := processStartedAt(child.PID)
	if err != nil {
		t.Fatal("cannot obtain recovery witness creation time")
	}
	if err := os.Mkdir(c.path("pg"), 0o700); err != nil {
		t.Fatal(err)
	}
	writePostmaster := func(epoch int64) {
		t.Helper()
		data := fmt.Sprintf("%d\n%s\n%d\n%d\n\n127.0.0.1\n1\nready\n", child.PID, c.path("pg"), epoch, m.Ports.Postgres)
		if err := os.WriteFile(c.path("pg", "postmaster.pid"), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writePostmaster(started.Unix() + 60)
	inspection, err := c.Inspect(t.Context())
	state := inspection.Processes["postgres"]
	if err != nil || !state.Alive || state.Matches || state.PID != child.PID {
		t.Fatal("Inspect did not distinguish an unfenced live PostgreSQL PID")
	}
	if err := c.recoverPostgresRecord(); err == nil {
		t.Fatal("PostgreSQL recovery accepted an unrelated native start epoch")
	}
	if _, err := os.Lstat(c.path("run", "postgres.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("rejected PostgreSQL recovery published a PID fence")
	}
	requireSafetyChildAlive(t, child)
	writePostmaster(started.Unix())
	if err := c.recoverPostgresRecord(); err != nil {
		t.Fatal(err)
	}
	state, err = c.processState("postgres")
	if err != nil || !state.Alive || !state.Matches || state.PID != child.PID {
		t.Fatal("matching native PostgreSQL start identity was not recovered")
	}
	requireSafetyChildAlive(t, child)
}

func TestRepairSettlesDeadUnrecordedPostgresAndPreservesData(t *testing.T) {
	c, m := unitCell(t)
	child := signalSafetyChild(t)
	started, err := processStartedAt(child.PID)
	if err != nil {
		t.Fatal("cannot obtain recovery witness creation time")
	}
	if err := os.Mkdir(c.path("pg"), 0o700); err != nil {
		t.Fatal(err)
	}
	const retained = "retained synthetic database bytes"
	if err := os.WriteFile(c.path("pg", "retained"), []byte(retained), 0o600); err != nil {
		t.Fatal(err)
	}
	data := fmt.Sprintf("%d\n%s\n%d\n%d\n\n127.0.0.1\n1\nready\n", child.PID, c.path("pg"), started.Unix(), m.Ports.Postgres)
	if err := os.WriteFile(c.path("pg", "postmaster.pid"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, step := range []string{"operation", "postgres"} {
		if err := c.journal("start", step, "started"); err != nil {
			t.Fatal(err)
		}
	}
	// This subprocess belongs to this test; recheck its exact identity before
	// simulating a database crash that leaves postmaster.pid behind.
	requireSafetyChildAlive(t, child)
	process, err := os.FindProcess(child.PID)
	if err != nil {
		t.Fatal("cannot open owned recovery witness")
	}
	defer func() { _ = process.Release() }()
	if err := process.Kill(); err != nil {
		t.Fatal("cannot stop owned recovery witness")
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, alive, err := processIdentity(child.PID)
		if err != nil {
			t.Fatal("cannot verify stopped recovery witness")
		}
		if !alive {
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatal("recovery witness did not exit")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := c.Repair(t.Context()); err != nil {
		t.Fatalf("already-stopped PostgreSQL prevented repair: %v", err)
	}
	inspection, err := c.Inspect(t.Context())
	if err != nil || inspection.Interrupted != nil || !inspection.PGDataPresent || inspection.Processes["postgres"].Alive {
		t.Fatal("repair did not settle the stopped cell while retaining PGDATA")
	}
	if _, err := os.Lstat(c.path("run", "postgres.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("repair published ownership for a dead PostgreSQL process")
	}
	retainedBytes, err := os.ReadFile(c.path("pg", "retained"))
	if err != nil || string(retainedBytes) != retained {
		t.Fatal("repair modified retained database data")
	}
	before, err := os.ReadFile(c.path("journal", "operations.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Repair(t.Context()); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(c.path("journal", "operations.jsonl"))
	if err != nil || string(before) != string(after) {
		t.Fatal("second repair changed the settled journal")
	}
}
