//go:build darwin || linux || windows

package localcell

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

const controlScriptName = "checkpoint-control-script.json"

type controlScript struct {
	Out, Err string
	Code     int
	Block    bool
	Poll     time.Duration
}

type controlRecord struct {
	Args, Names []string
	Locales     map[string][]string
	Directory   string
	PID         int
	Identity    string
}

// Only the renamed test executable dispatches. A misplaced script beside an
// ordinarily named binary fails closed; ordinary helper/test runs have neither.
func dispatchCheckpointControlHelper() {
	dir := filepath.Dir(os.Args[0])
	script := filepath.Join(dir, controlScriptName)
	if filepath.Base(os.Args[0]) != executable("pg_controldata") {
		if _, err := os.Stat(script); err == nil || (len(os.Args) == 3 && os.Args[1] == "-D") {
			os.Exit(125)
		}
		return
	}
	os.Exit(runCheckpointControlHelper(dir, script))
}

func runCheckpointControlHelper(dir, script string) int {
	var steps []controlScript
	data, err := os.ReadFile(script)
	if err != nil || json.Unmarshal(data, &steps) != nil {
		return 125
	}
	call := 1
	for ; call <= len(steps)+1; call++ {
		claim, err := os.OpenFile(filepath.Join(dir, "call-"+strconv.Itoa(call)), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return 125
		}
		_ = claim.Close()
		break
	}
	if call > len(steps) {
		return 125
	}
	step := steps[call-1]
	identity, alive, err := processIdentity(os.Getpid())
	if err != nil || !alive {
		return 125
	}
	wd, err := os.Getwd()
	if err != nil {
		return 125
	}
	record := controlRecord{Args: os.Args[1:], Directory: wd, PID: os.Getpid(), Identity: identity, Locales: map[string][]string{}}
	for _, entry := range os.Environ() {
		name, value, _ := strings.Cut(entry, "=")
		record.Names = append(record.Names, name)
		if slices.Contains([]string{"LC_ALL", "LC_MESSAGES", "LANG"}, name) {
			record.Locales[name] = append(record.Locales[name], value)
		}
	}
	if writeAtomic(filepath.Join(dir, "record-"+strconv.Itoa(call)), record, nil) != nil {
		return 125
	}
	if step.Block {
		// Production stdin is null. This lock follows the parent's lifetime
		// without requiring a restricted Windows child to query its parent.
		// CommandContext kills this child, so no exit marker is promised.
		parent, err := openLockFile(filepath.Join(dir, "parent"), false)
		if err != nil {
			return 125
		}
		defer func() { _ = parent.Close() }()
		if step.Poll <= 0 {
			return 125
		}
		if !errors.Is(lockFile(parent), ErrLocked) {
			_ = unlockFile(parent)
			return 125
		}
		if testWriteFile(filepath.Join(dir, "blocked-"+strconv.Itoa(call)), nil, 0o600) != nil {
			return 125
		}
		for errors.Is(lockFile(parent), ErrLocked) {
			time.Sleep(step.Poll)
		}
		_ = unlockFile(parent)
		return 125
	}
	if _, err := io.WriteString(os.Stdout, step.Out); err != nil {
		return 125
	}
	if _, err := io.WriteString(os.Stderr, step.Err); err != nil {
		return 125
	}
	if testWriteFile(filepath.Join(dir, "exit-"+strconv.Itoa(call)), nil, 0o600) != nil {
		return 125
	}
	return step.Code
}

func controlReading(data pgControlData) string {
	return fmt.Sprintf("pg_control version number:    %s\nCatalog version number:   %s\nDatabase system identifier:   %s\nDatabase cluster state:   %s\nLatest checkpoint location:    %s\nIgnored field: synthetic\nAnother ignored field: synthetic\n",
		data.ControlVersion, data.CatalogVersion, data.SystemIdentifier, data.State, data.CheckpointLSN)
}

func controlFixture(t *testing.T, steps ...controlScript) *retainedFixture {
	t.Helper()
	f, recorder := checkpointFixture(t, "legacy", false)
	recorder.production = true
	dir := testPrivateRoot(t)
	f.cell.cfg.Binaries.PostgresDir = dir
	parent, err := openLockFile(filepath.Join(dir, "parent"), true)
	if err != nil {
		t.Fatal("cannot create stand-in parent lock")
	}
	t.Cleanup(func() { _ = unlockFile(parent); _ = parent.Close() })
	if lockFile(parent) != nil {
		t.Fatal("cannot hold stand-in parent lock")
	}
	source, err := os.Open(os.Args[0])
	if err != nil {
		t.Fatal("cannot open stand-in executable source")
	}
	defer func() { _ = source.Close() }()
	target, err := openPrivateFile(filepath.Join(dir, executable("pg_controldata")), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		t.Fatal("cannot create stand-in executable")
	}
	_, copyErr := io.Copy(target, source)
	closeErr := target.Close()
	if copyErr != nil || closeErr != nil {
		t.Fatal("cannot copy stand-in executable")
	}
	if writeAtomic(filepath.Join(dir, controlScriptName), steps, nil) != nil {
		t.Fatal("cannot write stand-in script")
	}
	return f
}

func controlWait(bound, poll time.Duration, ready func() bool) bool {
	deadline := time.Now().Add(bound)
	for {
		if ready() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(poll)
	}
}

func controlRecords(t *testing.T, c *Cell, count int, bound, poll time.Duration, exited bool) []controlRecord {
	t.Helper()
	dir := c.cfg.Binaries.PostgresDir
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal("cannot count stand-in calls")
	}
	claims := 0
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "call-") {
			claims++
		}
	}
	if claims != count {
		t.Fatal("stand-in run count differs")
	}
	records := make([]controlRecord, count)
	for i := range records {
		name := strconv.Itoa(i + 1)
		if readJSON(filepath.Join(dir, "record-"+name), &records[i]) != nil {
			t.Fatal("stand-in invocation record missing")
		}
		r := records[i]
		if !controlWait(bound, poll, func() bool {
			identity, alive, err := processIdentity(r.PID)
			return err == nil && (!alive || identity != r.Identity)
		}) {
			t.Fatal("stand-in process remained alive")
		}
		if exited {
			if _, err := os.Stat(filepath.Join(dir, "exit-"+name)); err != nil {
				t.Fatal("stand-in did not finish writing output")
			}
		}
	}
	return records
}

func TestCheckpointControlRunner(t *testing.T) {
	a := controlReading(checkpointSyntheticControl)
	calibration := controlFixture(t, controlScript{Out: a})
	// Calibration has only the enclosing test deadline; subsequent command and
	// polling budgets derive from its measured launch, including race shutdown.
	calibration.cell.cfg.Timeout = time.Until(mustControlDeadline(t))
	started := time.Now()
	got, err := calibration.cell.checkpointControl(t.Context())
	launch := time.Since(started)
	if err != nil || got != checkpointSyntheticControl {
		t.Fatal("stand-in calibration failed")
	}
	bound, poll := max(50*launch, 2*time.Second), max(launch/4, time.Millisecond)
	controlRecords(t, calibration.cell, 1, bound, poll, true)
	t.Logf("launch_elapsed=%s", launch)
	for _, guard := range []struct {
		name           string
		missing, wrong bool
	}{{"dispatch_wrong_name", false, true}, {"dispatch_missing_script", true, false}, {"dispatch_both_missing", true, true}} {
		t.Run(guard.name, func(t *testing.T) {
			c := controlFixture(t, controlScript{Out: a}).cell
			dir := c.cfg.Binaries.PostgresDir
			binary := filepath.Join(dir, executable("pg_controldata"))
			if guard.missing {
				if os.Remove(filepath.Join(dir, controlScriptName)) != nil {
					t.Fatal("cannot remove stand-in script")
				}
			}
			if guard.wrong {
				other := filepath.Join(dir, executable("wrong-name"))
				if os.Rename(binary, other) != nil {
					t.Fatal("cannot rename stand-in")
				}
				binary = other
			}
			ctx, cancel := context.WithTimeout(t.Context(), bound)
			defer cancel()
			cmd, err := c.command(ctx, binary, "-D", c.path("pg"))
			if err != nil {
				t.Fatal("cannot construct dispatch guard command")
			}
			var output bytes.Buffer
			cmd.Stdout, cmd.Stderr = &output, &output
			var exit *exec.ExitError
			if !errors.As(cmd.Run(), &exit) || exit.ExitCode() != 125 || ctx.Err() != nil || output.Len() != 0 {
				t.Fatal("stand-in dispatch guard did not refuse silently")
			}
		})
	}
	b := checkpointSyntheticControl
	b.CheckpointLSN = "0/1548F49"
	for _, row := range []struct {
		name, refusal string
		second        controlScript
	}{
		{"T1_changed_reading", "local_checkpoint_witness_mismatch", controlScript{Out: controlReading(b)}},
		{"T2_equal_readings_T4_invocation", "", controlScript{Out: a}},
		{"T3_second_run_fails", "local_checkpoint_copy_failed", controlScript{Code: 3}},
	} {
		t.Run(row.name, func(t *testing.T) {
			f := controlFixture(t, controlScript{Out: a}, row.second)
			c := f.cell
			c.cfg.Timeout = bound
			err := c.Checkpoint(t.Context(), checkpointOptions(f))
			records := controlRecords(t, c, 2, bound, poll, true)
			if row.refusal != "" {
				if !checkpointSameError(err, row.refusal) {
					t.Fatal("checkpoint refusal differs")
				}
				checkpointRetryDiscarded(t, c)
				return
			}
			ids := checkpointIDs(t, c)
			if err != nil || len(ids) != 1 {
				t.Fatal("equal readings did not publish one checkpoint")
			}
			var witness checkpointWitness
			if readJSON(c.path("recovery", ids[0], checkpointWitnessName), &witness) != nil || witness.Control != checkpointSyntheticControl {
				t.Fatal("published witness control fields differ")
			}
			for _, record := range records {
				if !slices.Equal(record.Args, []string{"-D", c.path("pg")}) || record.Directory != c.cfg.Root {
					t.Fatal("stand-in invocation contract differs")
				}
				for _, key := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
					if !slices.Equal(record.Locales[key], []string{"C"}) {
						t.Fatal("stand-in locale contract differs")
					}
				}
				names := []string{"PATH", "HOME", "TMPDIR", "TEMP", "TMP", "LC_ALL", "LC_MESSAGES", "LANG"}
				if runtime.GOOS == "windows" {
					names = append(names, "SystemRoot", "USERPROFILE", "COMSPEC")
				}
				slices.Sort(names)
				slices.Sort(record.Names)
				if !slices.Equal(record.Names, names) {
					t.Fatal("stand-in environment names differ")
				}
			}
		})
	}
	for _, row := range []struct {
		name, refusal string
		step          controlScript
	}{
		{"T5_bounded_stdout", "local_checkpoint_state_unsupported", controlScript{Out: a + strings.Repeat("ignored: padding\n", (1<<20)/16+1)}},
		{"T6_discarded_stderr", "", controlScript{Out: a, Err: "WARNING: synthetic\n"}},
		{"T6_refused_stdout_warning", "local_checkpoint_state_unsupported", controlScript{Out: a + "WARNING: synthetic\n"}},
		{"T7_nonzero_with_valid_stdout", "PostgreSQL command exited with failure", controlScript{Out: a, Code: 3}},
	} {
		t.Run(row.name, func(t *testing.T) {
			c := controlFixture(t, row.step).cell
			c.cfg.Timeout = bound
			started := time.Now()
			got, err := c.checkpointControl(t.Context())
			t.Logf("elapsed=%s", time.Since(started))
			controlRecords(t, c, 1, bound, poll, true)
			if row.refusal == "" {
				if err != nil || got != checkpointSyntheticControl {
					t.Fatal("stderr changed control reading")
				}
			} else if !checkpointSameError(err, row.refusal) {
				t.Fatal("direct runner refusal differs")
			}
		})
	}
	for _, timeout := range []bool{false, true} {
		t.Run(map[bool]string{false: "T7_cancelled", true: "T7_timed_out"}[timeout], func(t *testing.T) {
			c := controlFixture(t, controlScript{Block: true, Poll: poll}).cell
			c.cfg.Timeout = bound
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if timeout {
				c.cfg.Timeout = max(5*launch, 200*time.Millisecond)
			}
			ready := make(chan bool, 1)
			go func() {
				ok := controlWait(bound, poll, func() bool {
					_, err := os.Stat(filepath.Join(c.cfg.Binaries.PostgresDir, "blocked-1"))
					return err == nil
				})
				if !timeout {
					cancel()
				}
				ready <- ok
			}()
			_, err := c.checkpointControl(ctx)
			if !<-ready {
				t.Fatal("blocked stand-in did not launch")
			}
			controlRecords(t, c, 1, bound, poll, false)
			want := "PostgreSQL command cancelled"
			if timeout {
				want = "PostgreSQL command timed out"
			}
			if err == nil || err.Error() != want {
				t.Fatal("blocked runner fixed text differs")
			}
		})
	}
}

func mustControlDeadline(t *testing.T) time.Time {
	t.Helper()
	deadline, ok := t.Deadline()
	if !ok {
		t.Fatal("stand-in calibration requires a test deadline")
	}
	return deadline
}
