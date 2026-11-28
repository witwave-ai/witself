//go:build darwin || linux || windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/localcell"
)

const (
	lcrLog   = "journal/operations.jsonl"
	lcrBegin = "{\"operation\":\"checkpoint_delete\",\"step\":\"operation\",\"state\":\"started\",\"at\":\"2026-01-01T00:00:00Z\"}\n"
	lcrState = "local_checkpoint_state_unsupported"
	lcrIO    = "local_checkpoint_operation_failed"
)

func lcrCheck(t *testing.T, ok bool, failure string) {
	t.Helper()
	if !ok {
		t.Fatal(failure)
	}
}

func lcrArgs(root string) []string {
	return []string{"repair-journal", "--checkpoint-mode=cold-v1", "--root=" + root, "--confirm=discard-tail"}
}

func lcrFixture(t *testing.T, journal *string) string {
	t.Helper()
	_, root := lcaRoot(t)
	lcrCheck(t, lcaPrivateDir(filepath.Join(root, "journal")) == nil, "cannot prepare private journal directory")
	lcrCheck(t, lcaPrivateFile(filepath.Join(root, ".owner.lock"), nil) == nil, "cannot prepare owner lock")
	if journal != nil {
		lcrCheck(t, lcaPrivateFile(filepath.Join(root, lcrLog), []byte(*journal)) == nil, "cannot prepare journal")
	}
	return root
}

// Repeat runs can share one prebuilt command. Each invocation gets an isolated
// home in lcrBinary; the Go builder retains its toolchain and module cache.
func lcrExecutable(t *testing.T) string {
	t.Helper()
	if supplied := os.Getenv(installedCommandAcceptanceBinaryEnv); supplied != "" {
		lcrCheck(t, filepath.IsAbs(supplied), "expected an absolute path to the checkpoint binary")
		return supplied
	}
	return buildInstalledCommandTestWitself(t, t.TempDir())
}

func lcrBinary(t *testing.T, exe string, args []string, stdout io.Writer) (int, string, string) {
	t.Helper()
	return lcrBinaryHome(t, exe, args, t.TempDir(), stdout)
}

func lcrBinaryHome(t *testing.T, exe string, args []string, home string, stdout io.Writer) (int, string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	var out, errOut bytes.Buffer
	child := exec.CommandContext(ctx, exe, append([]string{"local", "checkpoint"}, args...)...)
	child.Env = []string{"HOME=" + home, "USERPROFILE=" + home, "XDG_CONFIG_HOME=" + home, "WITSELF_HOME=" + home, "DSH_HOME=" + home, "PATH=" + home}
	child.Stdout, child.Stderr = &out, &errOut
	if stdout != nil {
		child.Stdout = stdout
	}
	err := child.Run()
	var exit *exec.ExitError
	lcrCheck(t, ctx.Err() == nil && (err == nil || errors.As(err, &exit)), "checkpoint binary did not finish")
	return child.ProcessState.ExitCode(), out.String(), errOut.String()
}

// Check presence on the wire as well as values: even an empty or null snapshot,
// target, reason or status would widen the published receipt.
func lcrReceipt(t *testing.T, raw string, code int, status, class string) {
	t.Helper()
	wantCode, outcome := 0, "succeeded"
	if class != "" {
		wantCode, outcome = 1, "failed"
	}
	lcrCheck(t, code == wantCode, "repair exit status changed")
	var fields map[string]json.RawMessage
	lcrCheck(t, json.Unmarshal([]byte(raw), &fields) == nil && strings.Count(raw, "\n") == 1 && strings.HasSuffix(raw, "\n"), "repair receipt framing changed")
	lcrCheck(t, lcaUniqueKeys(json.NewDecoder(strings.NewReader(raw))), "repair receipt repeats a key")
	lcrCheck(t, string(fields["operation"]) == `"repair-journal"`, "repair operation changed")
	lcrCheck(t, string(fields["outcome"]) == `"`+outcome+`"`, "repair outcome changed")
	lcrCheck(t, string(fields["verification"]) == `"not_performed"`, "repair claimed checkpoint verification")
	lcrCheck(t, string(fields["notice"]) == `"`+lcNotice+`"`, "repair notice changed")
	lcrCheck(t, fields["before"] == nil && fields["after"] == nil, "repair receipt included a checkpoint snapshot")
	lcrCheck(t, fields["target"] == nil && fields["presence"] == nil, "repair receipt included a target")
	if class == "" {
		lcrCheck(t, string(fields["journal_status"]) == `"`+status+`"`, "eligible repair status changed")
		lcrCheck(t, fields["failure"] == nil, "successful repair carried a diagnostic")
	} else {
		lcrCheck(t, fields["journal_status"] == nil, "repair reported status on failure")
		var failure map[string]json.RawMessage
		lcrCheck(t, json.Unmarshal(fields["failure"], &failure) == nil && string(failure["class"]) == `"`+class+`"`, "repair diagnostic class changed")
		lcrCheck(t, failure["reason"] == nil, "repair diagnostic exposed a reason")
		lcrCheck(t, string(failure["cleanup_incomplete"]) == "false", "repair claimed incomplete cleanup")
		lcrCheck(t, len(failure) == 2, "repair emitted private material")
	}
	lcrCheck(t, len(fields) == 5, "repair emitted private material")
}

func TestLocalCheckpointRepairJournal(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	exe := lcrExecutable(t)
	t.Run("usage", func(t *testing.T) {
		dir := t.TempDir()
		root := filepath.Join(dir, lcSentinel)
		args := lcrArgs(root)
		const disabled, invalid, consent = "local_checkpoint_command_disabled", "local_checkpoint_arguments_invalid", "local_checkpoint_confirmation_required"
		for _, row := range []struct {
			name, class string
			args        []string
		}{
			{"missing-mode", disabled, slices.Delete(slices.Clone(args), 1, 2)}, {"wrong-mode", disabled, lcReplace(args, "checkpoint-mode", "cold-v2")},
			{"missing-confirm", consent, args[:3]}, {"empty-confirm", consent, lcReplace(args, "confirm", "")},
			{"wrong-confirm", invalid, lcReplace(args, "confirm", "discard")}, {"duplicate-confirm", invalid, append(slices.Clone(args), args[3])},
			{"missing-root", invalid, slices.Delete(slices.Clone(args), 2, 3)}, {"relative-root", invalid, lcReplace(args, "root", "relative")},
			{"timeout", invalid, append(slices.Clone(args), "--timeout=bad")}, {"zero-timeout", invalid, append(slices.Clone(args), "--timeout=0s")},
			{"extra", invalid, append(slices.Clone(args), "operand")}, {"duplicate-mode", invalid, append(slices.Clone(args), args[1])},
			{"duplicate-root", invalid, append(slices.Clone(args), args[2])}, {"duplicate-timeout", invalid, append(slices.Clone(args), "--timeout=1s", "--timeout=2s")},
			{"force", invalid, append(slices.Clone(args), "--force")}, {"pgbin", invalid, append(slices.Clone(args), "--pgbin="+dir)},
			{"installation-id", invalid, append(slices.Clone(args), "--installation-id="+lcIdentity)}, {"checkpoint-id", invalid, append(slices.Clone(args), "--checkpoint-id="+lcID)},
			{"alias", invalid, append([]string{"repair"}, args[1:]...)},
			{"create-confirm", invalid, append(lcReplace(lcArgs("create"), "root", root), args[3])},
			{"inspect-confirm", invalid, append(lcReplace(lcArgs("inspect"), "root", root), args[3])},
			{"delete-confirm", invalid, append(lcReplace(lcArgs("delete"), "root", root), args[3])},
		} {
			t.Run(row.name, func(t *testing.T) {
				before := lcaTree(t, dir)
				open := func(localCheckpointMode, localCheckpointRequest) (localCheckpointCell, error) {
					t.Fatal("usage refusal constructed a cell")
					return nil, lcaError{}
				}
				var out, stderr bytes.Buffer
				code := localCheckpointRun(t.Context(), row.args, open, &out, &stderr)
				check := func(code int, raw, stderr string) {
					t.Helper()
					lcrCheck(t, code == 2, "usage exit changed")
					var fields map[string]json.RawMessage
					lcrCheck(t, json.Unmarshal([]byte(raw), &fields) == nil, "usage receipt was not a JSON object")
					lcrCheck(t, slices.Equal(slices.Sorted(maps.Keys(fields)), []string{"failure", "notice", "operation", "outcome", "verification"}), "usage refusal field set changed")
					wantOperation := row.args[0]
					if row.name == "alias" {
						wantOperation = "unknown"
					}
					r := lcaReceipt(t, raw)
					lcrCheck(t, r.Operation == wantOperation, "usage refusal operation changed")
					lcrCheck(t, r.Failure != nil && r.Failure.Class == row.class, "usage class changed")
					lcrCheck(t, r.Outcome == "refused", "repair outcome changed")
					lcrCheck(t, stderr == "", "repair emitted private material")
					lcrCheck(t, maps.Equal(before, lcaTree(t, dir)), "usage refusal created a name")
				}
				check(code, out.String(), stderr.String())
				check(lcrBinary(t, exe, row.args, nil))
			})
		}
	})
	t.Run("recorder", func(t *testing.T) {
		for _, row := range []struct {
			name  string
			err   error
			class string
		}{
			{"unknown-status", nil, lcrIO}, {"raw-error", lcaError{}, lcrIO}, {"failed-construction", lcaError{}, lcrIO},
			{"locked", localcell.ErrLocked, "local_cell_locked"}, {"cancelled", context.Canceled, "local_checkpoint_interrupted"},
		} {
			t.Run(row.name, func(t *testing.T) {
				cell := &lcaCell{t: t, mutation: row.err}
				count := 0
				open := func(mode localCheckpointMode, r localCheckpointRequest) (localCheckpointCell, error) {
					count++
					lcrCheck(t, mode == "cold-v1" && r.confirm == "discard-tail" && r.timeout == 7*time.Second, "repair lost its parsed consent or budget")
					if row.name == "failed-construction" {
						return nil, row.err
					}
					return cell, nil
				}
				var out, stderr bytes.Buffer
				code := localCheckpointRun(t.Context(), append(lcrArgs(t.TempDir()), "--timeout=7s"), open, &out, &stderr)
				lcrReceipt(t, out.String(), code, "", row.class)
				lcrCheck(t, count == 1, "repair opened a second cell")
				want := []string{"repair-journal"}
				if row.name == "failed-construction" {
					want = nil
				}
				lcrCheck(t, slices.Equal(cell.calls, want), "the command reached whole-cell Repair")
				lcrCheck(t, stderr.Len() == 0, "repair emitted private material")
			})
		}
	})
	for _, route := range []string{"binary", "adapter"} {
		call := func(t *testing.T, args []string) (int, string, string) {
			t.Helper()
			if route == "binary" {
				return lcrBinary(t, exe, args, nil)
			}
			var out, stderr bytes.Buffer
			code := localCheckpointRun(t.Context(), args, localCheckpointOpen, &out, &stderr)
			return code, out.String(), stderr.String()
		}
		t.Run(route, func(t *testing.T) {
			for _, row := range []struct{ name, data, status, class string }{
				{"torn", lcrBegin + "{", "tail_discarded", ""}, {"complete-line", lcrBegin, "clean", ""}, {"empty", "", "clean", ""}, {"absent", "", "absent", ""},
				{"header", "42", "", lcrState}, {"invalid-record", "{\n", "", lcrIO}, {"missing-owner", "{", "", lcrState}, {"maintenance", "{", "", "local_maintenance_fence_active"},
				{"postmaster", "{", "", lcrState}, {"unproven", "{", "", lcrIO}, {"layout", "{", "", "local_cell_layout_refused"}, {"permission", "{", "", lcrIO},
				{"deadline", "{", "", "local_checkpoint_interrupted"}, {"held-lock", "{", "", "local_cell_locked"},
			} {
				t.Run(row.name, func(t *testing.T) {
					data := &row.data
					if row.name == "absent" {
						data = nil
					}
					root := lcrFixture(t, data)
					args, log := lcrArgs(root), filepath.Join(root, lcrLog)
					prep := func(err error) { t.Helper(); lcrCheck(t, err == nil, "cannot prepare refusal fixture") }
					switch row.name {
					case "missing-owner":
						prep(os.Remove(filepath.Join(root, ".owner.lock")))
					case "maintenance":
						prep(lcaPrivateFile(filepath.Join(root, "journal", "maintenance.json"), []byte("{}")))
					case "postmaster":
						prep(lcaPrivateDir(filepath.Join(root, "pg")))
						prep(lcaPrivateFile(filepath.Join(root, "pg", "postmaster.pid"), []byte("unknown")))
					case "unproven":
						prep(lcaPrivateDir(filepath.Join(root, "run")))
						canonical, err := localcell.CanonicalRoot(root)
						prep(err)
						body, err := json.Marshal(localcell.ProcessRecord{Root: canonical, PID: os.Getpid(), StartTime: "unverified", Component: "server"})
						prep(err)
						prep(lcaPrivateFile(filepath.Join(root, "run", "server.json"), body))
					case "layout":
						prep(lcaPrivateFile(filepath.Join(root, "tmp"), nil))
					case "permission":
						prep(os.Chmod(log, 0400))
						t.Cleanup(func() { _ = os.Chmod(log, 0600) })
					case "deadline":
						args = append(args, "--timeout=1ns")
					}
					tree := lcaTree(t, filepath.Dir(root))
					if row.name == "torn" {
						tree[log] = strings.TrimSuffix(tree[log], "{")
					}
					unlock := func() {}
					if row.name == "held-lock" {
						f, err := os.OpenFile(filepath.Join(root, ".owner.lock"), os.O_RDWR, 0)
						prep(err)
						defer func() { _ = f.Close() }()
						ok, err := tryIntegrationOperationLock(f)
						prep(err)
						lcrCheck(t, ok, "cannot hold the owner lock")
						unlock = func() { prep(unlockIntegrationOperationLock(f)) }
					}
					code, raw, stderr := call(t, args)
					unlock()
					lcrReceipt(t, raw, code, row.status, row.class)
					lcrCheck(t, stderr == "", "repair emitted private material")
					if data != nil {
						got, err := os.ReadFile(log)
						prep(err)
						expect := row.data
						if row.name == "torn" {
							expect = lcrBegin
						}
						lcrCheck(t, bytes.HasPrefix(got, []byte(expect)), "repair changed a validated byte")
						lcrCheck(t, len(got) == len(expect), "repair retained bytes beyond the cutoff")
					}
					lcrCheck(t, maps.Equal(tree, lcaTree(t, filepath.Dir(root))), "repair changed a nonjournal object")
				})
			}
			for _, control := range []struct {
				name, data string
				code       int
			}{{"committed-invalid", "{\n", 1}, {"committed-line", lcrBegin, 0}} {
				t.Run(control.name, func(t *testing.T) {
					root := lcrFixture(t, &control.data)
					tree := lcaTree(t, filepath.Dir(root))
					code, raw, stderr := call(t, lcReplace(lcArgs("inspect"), "root", root))
					lcrCheck(t, code == control.code, "mutation changed the baseline refusal")
					lcrCheck(t, !strings.Contains(raw, `"journal_`), "old CLI unexpectedly exposed a journal field")
					r := lcaReceipt(t, raw)
					if code == 1 {
						lcrCheck(t, r.Failure != nil && r.Failure.Class == lcrIO, "mutation changed the baseline refusal")
					} else {
						lcrCheck(t, r.After != nil && r.After.Status == "listed", "listing hid the surviving target")
					}
					lcrCheck(t, stderr == "", "repair emitted private material")
					lcrCheck(t, maps.Equal(tree, lcaTree(t, filepath.Dir(root))), "baseline refusal changed journal bytes")
				})
			}
			t.Run("baseline", func(t *testing.T) { lcrBaseline(t, call) })
		})
	}
	for _, name := range []string{"read-only", "closed-pipe"} {
		t.Run("stdout/"+name, func(t *testing.T) {
			root := lcrFixture(t, new(lcrBegin+"{"))
			r, w, err := os.Pipe()
			lcrCheck(t, err == nil, "cannot create output pipe")
			if name == "read-only" {
				r, w = w, r // pass the read end as the child's stdout
			}
			lcrCheck(t, r.Close() == nil, "cannot close the opposite pipe end")
			defer func() { _ = w.Close() }()
			code, _, stderr := lcrBinary(t, exe, lcrArgs(root), w)
			lcrCheck(t, code == 1, "failed receipt write did not exit one")
			lcrCheck(t, stderr == "local_checkpoint_operation_failed\n", "stdout failure did not use the fixed stderr text")
			got, err := os.ReadFile(filepath.Join(root, lcrLog))
			lcrCheck(t, err == nil, "cannot read journal after failed receipt write")
			lcrCheck(t, string(got) == lcrBegin, "failed receipt write did not leave the journal settled")
		})
	}
	for _, row := range []struct {
		name string
		args []string
		exit int
	}{
		{"missing-mode", []string{"repair-journal", "--confirm=discard-tail"}, 2},
		{"missing-confirm", []string{"repair-journal", "--checkpoint-mode=cold-v1"}, 2},
		{"consented", []string{"repair-journal", "--checkpoint-mode=cold-v1", "--confirm=discard-tail"}, 0},
		{"inspect", []string{"inspect", "--checkpoint-mode=cold-v1"}, 0},
	} {
		t.Run("startup/"+row.name, func(t *testing.T) {
			root := lcrFixture(t, new(lcrBegin+"{"))
			home := filepath.Join(root, "home")
			for _, dir := range []string{home, filepath.Join(home, "message-runners"), filepath.Join(home, "message-runners", "codex")} {
				lcrCheck(t, lcaPrivateDir(dir) == nil, "cannot prepare pending CLI migration")
			}
			if runtime.GOOS == "darwin" {
				lcrCheck(t, os.WriteFile(filepath.Join(home, "launchctl"), []byte("#!/bin/sh\necho 'service not found' >&2\nexit 1\n"), 0700) == nil, "cannot prepare synthetic launchctl")
			}
			tree := lcaTree(t, root)
			if row.name == "consented" {
				log := filepath.Join(root, lcrLog)
				tree[log] = strings.TrimSuffix(tree[log], "{")
			}
			code, _, stderr := lcrBinaryHome(t, exe, append(row.args, "--root="+root), home, nil)
			lcrCheck(t, code == row.exit, "overlapping home changed checkpoint exit")
			if row.name == "inspect" && runtime.GOOS == "windows" {
				lcrCheck(t, strings.Contains(stderr, "legacy message runner cleanup is unsupported on windows"), "inspect skipped Windows startup cleanup")
				return
			}
			lcrCheck(t, stderr == "", "overlapping home produced a startup warning")
			if row.name == "inspect" {
				marker, err := os.ReadFile(filepath.Join(home, "migrations", "foreground-messaging-v1"))
				lcrCheck(t, err == nil, "inspect skipped the pending home migration")
				lcrCheck(t, string(marker) == "completed\n", "inspect changed the migration marker")
			} else {
				lcrCheck(t, maps.Equal(tree, lcaTree(t, root)), "repair changed the overlapping cell tree")
			}
		})
	}
}

func lcrBaseline(t *testing.T, call func(*testing.T, []string) (int, string, string)) {
	for _, kind := range []string{"none", "partial", "complete", "purged"} {
		t.Run(kind, func(t *testing.T) {
			prefix := lcrBegin + strings.ReplaceAll(strings.ReplaceAll(lcrBegin, `"step":"operation"`, `"step":"remove"`), `"started"`, `"done"`)
			torn := prefix + "{"
			root := lcrFixture(t, &torn)
			prep := func(err error) { t.Helper(); lcrCheck(t, err == nil, "cannot prepare baseline fixture") }
			if kind != "purged" {
				m := localcell.Manifest{LayoutVersion: 1, InstallationID: lcIdentity, Database: "witself", Role: "localcell", Ports: localcell.Ports{Postgres: 1, ServerAPI: 2, ServerHealth: 3, ServerMetrics: 4, WorkerHealth: 5, WorkerMetrics: 6}, Installed: true, AccountID: "acc_abc", LastVerifiedSchemaVersion: 1}
				body, err := json.Marshal(m)
				prep(err)
				prep(lcaPrivateFile(filepath.Join(root, "manifest.json"), body))
			}
			prep(lcaPrivateDir(filepath.Join(root, "recovery")))
			if kind == "partial" || kind == "complete" {
				dir := filepath.Join(root, "recovery", lcID)
				prep(lcaPrivateDir(dir))
				prep(lcaPrivateFile(filepath.Join(dir, "inventory.json"), []byte("{}")))
				if kind == "complete" {
					prep(lcaPrivateFile(filepath.Join(dir, "witness.json"), []byte(`{"format":1,"checkpoint_id":"ckpt_abcdefghijklmnop","installation_id":"installation_A","created_at":"2026-01-01T00:00:00Z","bytes":5}`)))
				}
			}
			cell, err := localcell.New(localcell.Config{Root: root})
			prep(err)
			inspect, err := cell.Inspect(t.Context())
			prep(err)
			lcrCheck(t, inspect.JournalTailIncomplete, "API inspection missed the torn suffix")
			old := func(op string, want int) lcWireReceipt {
				t.Helper()
				code, raw, stderr := call(t, lcReplace(lcArgs(op), "root", root))
				lcrCheck(t, code == want, "mutation changed the baseline refusal")
				lcrCheck(t, !strings.Contains(raw, `"journal_`), "old CLI unexpectedly exposed a journal field")
				lcrCheck(t, stderr == "", "repair emitted private material")
				return lcaReceipt(t, raw)
			}
			tree := lcaTree(t, filepath.Dir(root))
			r := old("inspect", 0)
			lcrCheck(t, r.Outcome == "succeeded", "listing hid the surviving target")
			lcrCheck(t, r.After != nil && r.After.Status == "listed", "listing hid the surviving target")
			if kind == "partial" || kind == "complete" {
				lcrCheck(t, len(r.After.Entries) == 1, "listing hid the surviving target")
				e := r.After.Entries[0]
				lcrCheck(t, e.ID == lcID, "listing hid the surviving target")
				lcrCheck(t, e.Complete != nil && *e.Complete == (kind == "complete"), "listing hid the surviving target")
			} else {
				lcrCheck(t, len(r.After.Entries) == 0, "listing hid the surviving target")
			}
			for _, op := range []string{"create", "delete"} {
				if kind == "purged" && op == "create" {
					continue
				} // no manifest ID to authorize this create
				r := old(op, 1)
				lcrCheck(t, r.Outcome == "failed", "mutation changed the baseline refusal")
				lcrCheck(t, r.Failure != nil && r.Failure.Class == lcrState, "mutation changed the baseline refusal")
				lcrCheck(t, r.Failure.Reason == "", "repair diagnostic exposed a reason")
				lcrCheck(t, r.Failure.CleanupIncomplete != nil && !*r.Failure.CleanupIncomplete, "repair claimed incomplete cleanup")
				lcrCheck(t, maps.Equal(tree, lcaTree(t, filepath.Dir(root))), "baseline refusal changed journal bytes")
			}
			code, raw, stderr := call(t, lcrArgs(root))
			lcrReceipt(t, raw, code, "tail_discarded", "")
			lcrCheck(t, stderr == "", "repair emitted private material")
			log := filepath.Join(root, lcrLog)
			tree[log] = strings.TrimSuffix(tree[log], "{")
			lcrCheck(t, maps.Equal(tree, lcaTree(t, filepath.Dir(root))), "repair changed a nonjournal object")
			if kind == "complete" || kind == "partial" {
				r := old("delete", 0)
				lcrCheck(t, r.Outcome == "succeeded", "the admitted verb did not finish after repair")
				lcrCheck(t, r.Presence == "absent", "the admitted verb did not finish after repair")
			} else {
				if kind == "purged" {
					r := old("create", 2)
					lcrCheck(t, r.Failure != nil && r.Failure.Class == "local_checkpoint_identity_mismatch", "repair concealed the pending purged deletion")
				}
				r := old("delete", 1)
				lcrCheck(t, r.Failure != nil && r.Failure.Class == "local_checkpoint_identity_mismatch", "mutation changed the baseline refusal")
				inspect, err := cell.Inspect(t.Context())
				prep(err)
				lcrCheck(t, !inspect.JournalTailIncomplete, "repair retained bytes beyond the cutoff")
				lcrCheck(t, inspect.Interrupted != nil && inspect.Interrupted.Operation == "checkpoint_delete" && inspect.Interrupted.Step == "remove", "repair concealed the pending purged deletion")
				lcrCheck(t, maps.Equal(tree, lcaTree(t, filepath.Dir(root))), "settlement closed the previous operation")
				if kind == "none" {
					lcrCreateAfterRepair(t, root, cell, call)
				}
			}
		})
	}
}

func lcrCreateAfterRepair(t *testing.T, root string, cell *localcell.Cell, call func(*testing.T, []string) (int, string, string)) {
	t.Helper()
	for _, name := range []string{"pg", "pg/global", "run"} {
		lcrCheck(t, lcaPrivateDir(filepath.Join(root, name)) == nil, "cannot prepare admitted create")
	}
	for name, data := range map[string]string{"pg/PG_VERSION": "16\n", "pg/global/pg_control": "synthetic control", "run/postgres.password": strings.Repeat("x", 32)} {
		lcrCheck(t, lcaPrivateFile(filepath.Join(root, name), []byte(data)) == nil, "cannot prepare admitted create")
	}
	bin := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	build := exec.CommandContext(ctx, "go", "build", "-o", bin+string(filepath.Separator), "./testdata/pg_controldata")
	lcrCheck(t, build.Run() == nil, "cannot build synthetic control reader")
	args := lcReplace(lcReplace(lcArgs("create"), "root", root), "pgbin", bin)
	code, raw, stderr := call(t, args)
	lcrCheck(t, code == 0, "the admitted verb did not finish after repair")
	lcrCheck(t, lcaReceipt(t, raw).Outcome == "succeeded", "the admitted verb did not finish after repair")
	lcrCheck(t, stderr == "", "repair emitted private material")
	inspection, err := cell.Inspect(t.Context())
	lcrCheck(t, err == nil, "cannot inspect after admitted create")
	lcrCheck(t, inspection.Interrupted == nil, "admitted create left a pending operation")
	lcrCheck(t, len(inspection.Checkpoints) == 1 && inspection.Checkpoints[0].Complete, "admitted create left no complete checkpoint")
}
