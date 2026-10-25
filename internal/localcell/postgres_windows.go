//go:build windows

package localcell

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Launch postgres itself: pg_ctl start creates a cmd.exe intermediary on
// Windows. Its PID cannot provide the actual postmaster's pre-execution fence.
// An elevated token unsupported by postgres is refused by the executable; this
// adapter does not silently grant service rights or emulate pg_ctl's token strip.
func (c *Cell) startPostgres(ctx context.Context, _ *Manifest) error {
	if err := c.verifyAll(); err != nil {
		return err
	}
	state, err := c.processState("postgres")
	if err != nil || state.Alive {
		return err
	}
	stale, err := c.windowsStalePostmaster()
	if err != nil {
		return err
	}
	if !filepath.IsAbs(c.cfg.Binaries.PostgresDir) {
		return errors.New("explicit PostgreSQL binary directory is required")
	}
	environment, err := c.postgresEnvironment()
	if err != nil {
		return err
	}
	environment = appendEnvironment(environment, "LC_ALL=C", "LANG=C")
	log, err := c.logFile("postgres")
	if err != nil {
		return err
	}
	defer func() { _ = log.Close() }()
	ctx, cancel := c.deadline(ctx)
	defer cancel()
	return c.launchWindows(filepath.Join(c.cfg.Binaries.PostgresDir, "postgres.exe"), []string{"-D", c.path("pg")}, "postgres", environment, log, func(record ProcessRecord) error {
		for {
			identity, alive, err := processIdentity(record.PID)
			if err != nil || !alive || identity != record.StartTime {
				return errors.New("local cell PostgreSQL exited before readiness")
			}
			ready, err := c.windowsPostgresReady(record, stale)
			if err != nil {
				return err
			}
			if ready {
				return nil
			}
			select {
			case <-ctx.Done():
				return errors.New("local cell PostgreSQL start deadline exceeded")
			case <-time.After(25 * time.Millisecond):
			}
		}
	})
}

// A postmaster writes its pidfile incrementally. Incomplete publication is not
// readiness, while a complete conflicting PID or directory is an immediate
// refusal. Inspect identity and status from the same private bounded read.
func (c *Cell) windowsPostgresReady(record ProcessRecord, stale []byte) (bool, error) {
	path := c.path("pg", "postmaster.pid")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > 8192 {
		return false, errors.New("invalid PostgreSQL process record")
	}
	data, err := readPrivateFile(path)
	if err != nil || len(data) > 8192 {
		return false, errors.New("cannot read PostgreSQL process record")
	}
	// PostgreSQL itself removes a stale pidfile. Give the newly resumed
	// postmaster time to replace exactly the verified-dead prelaunch snapshot;
	// never adopt it or ignore a changed, conflicting identity.
	if len(stale) != 0 && bytes.Equal(data, stale) {
		return false, nil
	}
	lines := postmasterLines(data)
	if len(lines) < 3 {
		return false, nil
	}
	pid, err := strconv.Atoi(lines[0])
	if err != nil || pid != record.PID || filepath.Clean(lines[1]) != c.path("pg") {
		return false, errors.New("PostgreSQL process ownership is unproven")
	}
	if len(lines) < 4 {
		return false, nil
	}
	if epoch, err := strconv.ParseInt(lines[2], 10, 64); err != nil || epoch <= 0 {
		return false, errors.New("invalid PostgreSQL process start time")
	}
	return len(lines) >= 8 && strings.TrimSpace(lines[7]) == "ready", nil
}

func (c *Cell) windowsStalePostmaster() ([]byte, error) {
	data, err := readPrivateFile(c.path("pg", "postmaster.pid"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil || len(data) > 8192 {
		return nil, errors.New("cannot read previous PostgreSQL process record")
	}
	if pid, handled, err := c.pendingPostmasterPID(data); handled || err != nil {
		if err != nil || pid != 0 {
			return nil, errors.New("previous PostgreSQL process ownership is unproven")
		}
		return data, nil
	}
	lines := postmasterLines(data)
	if len(lines) < 3 || filepath.Clean(lines[1]) != c.path("pg") {
		return nil, errors.New("invalid previous PostgreSQL process record")
	}
	pid, err := strconv.Atoi(lines[0])
	if err != nil || pid <= 1 {
		return nil, errors.New("invalid previous PostgreSQL process identity")
	}
	if _, alive, err := processIdentity(pid); err != nil || alive {
		return nil, errors.New("previous PostgreSQL process ownership is unproven")
	}
	return data, nil
}

func (c *Cell) pendingPostgresWithoutPID(state ProcessState, pid int) bool {
	var record ProcessRecord
	return pid == 0 && readJSON(c.path("run", "postgres.json"), &record) == nil && record.LaunchPending && record.PID == state.PID && record.Root == c.cfg.Root && record.Component == "postgres"
}

func postmasterLines(data []byte) []string {
	return strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
}

// Unlike pg_ctl recovery, a suspended direct launch already has a durable
// creation fence before PostgreSQL writes even the first byte of postmaster.pid.
// That pending fence can reconcile partial publication after a crash, but only
// if every pidfile byte published so far agrees with the recorded process/root.
func (c *Cell) pendingPostmasterPID(data []byte) (int, bool, error) {
	var record ProcessRecord
	err := readJSON(c.path("run", "postgres.json"), &record)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, true, errors.New("cannot read pending PostgreSQL identity")
	}
	if !record.LaunchPending {
		return 0, false, nil
	}
	if record.PID <= 1 || record.StartTime == "" || record.Root != c.cfg.Root || record.Component != "postgres" {
		return 0, true, errors.New("invalid pending PostgreSQL identity")
	}
	lines := postmasterLines(data)
	expected := strconv.Itoa(record.PID)
	valid := false
	if len(lines) == 1 {
		valid = strings.HasPrefix(expected, strings.TrimSuffix(lines[0], "\r"))
	} else if lines[0] == expected {
		if len(lines) == 2 {
			valid = strings.HasPrefix(c.path("pg"), strings.TrimSuffix(lines[1], "\r"))
		} else {
			valid = filepath.Clean(lines[1]) == c.path("pg")
		}
	}
	if !valid {
		return 0, true, errors.New("pending PostgreSQL pidfile identity mismatch")
	}
	identity, alive, err := processIdentity(record.PID)
	if err != nil || (alive && identity != record.StartTime) {
		return 0, true, errors.New("pending PostgreSQL process identity mismatch")
	}
	if !alive {
		return 0, true, nil
	}
	return record.PID, true, nil
}
