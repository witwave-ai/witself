package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

const lcIdentity = "installation_A"
const lcID = "ckpt_abcdefghijklmnop"
const lcOtherID = "ckpt_bcdefghijklmnopq"
const lcSentinel = "PRIVATE_SENTINEL"

type lcPanicError struct{}

func (lcPanicError) Error() string { panic("raw error was consulted") }

type lcFixture struct {
	calls                        []string
	observations                 []localCheckpointObservation
	readFailure, mutationFailure localCheckpointFailure
	failRead                     int
	requests                     []localCheckpointRequest
}

func lcListed() localCheckpointObservation {
	return localCheckpointObservation{status: "listed", installationID: lcIdentity, installationPresent: true}
}
func lcEntry() localCheckpointSummary {
	return localCheckpointSummary{id: lcID, installationID: lcIdentity, created: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), complete: true}
}
func lcArgs(verb string) []string {
	a := []string{verb, "--checkpoint-mode=cold-v1", "--root=" + filepath.Join(os.TempDir(), "unopened-checkpoint-root")}
	if verb == "create" {
		a = append(a, "--pgbin="+filepath.Join(os.TempDir(), "unopened-binary-root"), "--installation-id="+lcIdentity)
	}
	if verb == "delete" {
		a = append(a, "--checkpoint-id="+lcID)
	}
	return a
}
func (f *lcFixture) run(t *testing.T, args []string, want int, calls string) localCheckpointReceipt {
	t.Helper()
	f.calls, f.requests = nil, nil
	reads := 0
	record := func(ctx context.Context, r localCheckpointRequest, call string) {
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > r.timeout || r.timeout <= 0 {
			t.Fatal("context budget missing")
		}
		f.calls, f.requests = append(f.calls, call), append(f.requests, r)
	}
	cb := localCheckpointCallbacks{read: func(ctx context.Context, r localCheckpointRequest) (localCheckpointObservation, localCheckpointFailure) {
		record(ctx, r, "read")
		reads++
		o := lcListed()
		if len(f.observations) >= reads {
			o = f.observations[reads-1]
		}
		if f.failRead == reads {
			return o, f.readFailure
		}
		return o, localCheckpointFailure{}
	}}
	cb.create = func(ctx context.Context, r localCheckpointRequest) localCheckpointFailure {
		record(ctx, r, "create")
		return f.mutationFailure
	}
	cb.delete = func(ctx context.Context, r localCheckpointRequest) localCheckpointFailure {
		record(ctx, r, "delete")
		return f.mutationFailure
	}
	var out, stderr bytes.Buffer
	defer func() {
		if recover() != nil {
			t.Fatal("raw error was consulted")
		}
	}()
	code := localCheckpointCommand(context.Background(), args, cb, &out, &stderr)
	if code != want || strings.Join(f.calls, ",") != calls {
		t.Fatal("exit or callback sequence differs")
	}
	if stderr.Len() != 0 || strings.Contains(out.String(), lcSentinel) {
		t.Fatal("private output escaped")
	}
	for _, key := range regexp.MustCompile(`"((?:\\.|[^"\\])*)":`).FindAllSubmatch(out.Bytes(), -1) {
		if !strings.Contains(" operation outcome failure class reason cleanup_incomplete before after status installation_id count unknown_count entries id complete verification notice target presence ", " "+string(key[1])+" ") {
			t.Fatal("receipt keys differ")
		}
	}
	var receipt localCheckpointReceipt
	decoder := json.NewDecoder(&out)
	decoder.DisallowUnknownFields()
	if decoder.Decode(&receipt) != nil || strings.TrimSpace(out.String()) != "" {
		t.Fatal("receipt projection differs")
	}
	if receipt.Verification != "not_performed" || receipt.Notice != "Checkpoint journal history may remain, including after purged-root delete; another purge does not prune it. Older readers may refuse that history; downgrade compatibility is unsupported." {
		t.Fatal("verification or downgrade disclosure differs")
	}
	return receipt
}
func lcReplace(args []string, flag, value string) []string {
	result := slices.Clone(args)
	for i, a := range result {
		if strings.HasPrefix(a, "--"+flag+"=") {
			result[i] = "--" + flag + "=" + value
			return result
		}
	}
	return append(result, "--"+flag+"="+value)
}
func TestLocalCheckpointSelection(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		f := &lcFixture{}
		r := f.run(t, []string{"inspect"}, 2, "")
		if r.Failure.Class != "local_checkpoint_command_disabled" {
			t.Fatal("selector refusal differs")
		}
	})
	t.Run("unknown", func(t *testing.T) {
		t.Setenv("WITSELF_CHECKPOINT_MODE", "cold-v1")
		for _, mode := range []string{"", lcSentinel} {
			r := (&lcFixture{}).run(t, lcReplace(lcArgs("inspect"), "checkpoint-mode", mode), 2, "")
			if r.Failure.Class != "local_checkpoint_command_disabled" {
				t.Fatal("selector refusal differs")
			}
		}
	})
	t.Run("explicit", func(t *testing.T) {
		for verb, calls := range map[string]string{"create": "read,create,read", "inspect": "read", "delete": "read,delete,read"} {
			f := &lcFixture{}
			f.run(t, lcArgs(verb), 0, calls)
			if f.requests[0].timeout != 30*time.Second {
				t.Fatal("default timeout differs")
			}
		}
	})
}
func TestLocalCheckpointArguments(t *testing.T) {
	t.Run("relative-root", func(t *testing.T) {
		for _, flag := range []string{"root", "pgbin", "timeout"} {
			for _, value := range []string{"", "relative", "0", "-1s"} {
				r := (&lcFixture{}).run(t, lcReplace(lcArgs("create"), flag, value), 2, "")
				if r.Failure.Class != "local_checkpoint_arguments_invalid" {
					t.Fatal("argument refusal differs")
				}
			}
		}
	})
	t.Run("wrong-verb-flag", func(t *testing.T) {
		for _, verb := range []string{"inspect", "delete", lcSentinel} {
			for _, extra := range []string{"--pgbin=", "--installation-id=", "--foreign=" + lcSentinel, "operand", "--yes", "--force", "--stop", "--repair", "--replace"} {
				(&lcFixture{}).run(t, append(lcArgs(verb), extra), 2, "")
			}
		}
		(&lcFixture{}).run(t, append(lcArgs("create"), "--checkpoint-id="+lcID), 2, "")
	})
	t.Run("missing-consent", func(t *testing.T) {
		for _, verb := range []string{"create", "delete"} {
			a := lcArgs(verb)
			r := (&lcFixture{}).run(t, a[:len(a)-1], 2, "")
			if r.Failure.Class != "local_checkpoint_confirmation_required" {
				t.Fatal("consent refusal differs")
			}
		}
	})
	t.Run("exact-id", func(t *testing.T) {
		for _, verb := range []string{"create", "delete"} {
			f := &lcFixture{}
			f.run(t, lcArgs(verb), 0, "read,"+verb+",read")
			if (verb == "create" && f.requests[1].installationID != lcIdentity) || (verb == "delete" && f.requests[1].checkpointID != lcID) {
				t.Fatal("exact consent changed")
			}
		}
		for _, value := range []string{"latest", "oldest", "all", "*", "ckpt_abc", lcID + "*", "bad space", strings.Repeat("a", 65)} {
			(&lcFixture{}).run(t, lcReplace(lcArgs("delete"), "checkpoint-id", value), 2, "")
		}
		for _, value := range []string{"latest", "oldest", "all", "*", "bad space", strings.Repeat("a", 65)} {
			(&lcFixture{}).run(t, lcReplace(lcArgs("create"), "installation-id", value), 2, "")
		}
		for _, value := range []string{"installation", "different"} {
			r := (&lcFixture{}).run(t, lcReplace(lcArgs("create"), "installation-id", value), 2, "read")
			if r.Failure.Class != "local_checkpoint_identity_mismatch" {
				t.Fatal("exact consent guard differs")
			}
		}
	})
}
func TestLocalCheckpointOutput(t *testing.T) {
	t.Run("raw-error", func(t *testing.T) {
		classes := []string{"local_checkpoint_state_unsupported", "local_checkpoint_identity_mismatch", "local_checkpoint_source_unsupported", "local_checkpoint_storage_insufficient", "local_checkpoint_limit_reached", "local_checkpoint_incomplete", "local_checkpoint_witness_mismatch", "local_checkpoint_copy_failed", "local_maintenance_fence_active", "local_cell_locked", "local_checkpoint_interrupted", "local_cell_layout_refused", "local_checkpoint_operation_failed"}
		reasons := []string{"", "boundary_unreadable", "boundary_noncanonical", "boundary_not_private", "descendant_refused", "tree_unreadable", "unknown_entries"}
		for _, class := range classes {
			for _, reason := range reasons {
				f := &lcFixture{mutationFailure: localCheckpointFailure{lcPanicError{}, class, reason, true}}
				r := f.run(t, lcArgs("create"), 1, "read,create,read")
				if r.Failure.Class != class || r.Failure.Reason != reason || !r.Failure.CleanupIncomplete {
					t.Fatal("bounded failure changed")
				}
			}
		}
		for _, err := range []error{lcPanicError{}, errors.New("local_checkpoint_copy_failed"), errors.New(lcSentinel)} {
			f := &lcFixture{mutationFailure: localCheckpointFailure{err, lcSentinel, lcSentinel, true}}
			r := f.run(t, lcArgs("create"), 1, "read,create,read")
			if r.Failure.Class != "local_checkpoint_operation_failed" || r.Failure.Reason != "" {
				t.Fatal("unknown failure was trusted")
			}
		}
	})
	t.Run("metadata", func(t *testing.T) {
		o := lcListed()
		o.entries = []localCheckpointSummary{lcEntry()}
		r := (&lcFixture{observations: []localCheckpointObservation{o}}).run(t, lcArgs("inspect"), 0, "read")
		if r.After.InstallationID != lcIdentity || r.After.Entries[0] != (localCheckpointEntry{lcID, lcIdentity, true, "not_performed"}) || *r.After.Count != 1 {
			t.Fatal("identifier exception differs")
		}
		for _, alter := range []func(*localCheckpointObservation){
			func(o *localCheckpointObservation) { o.installationID = lcSentinel + "!" }, func(o *localCheckpointObservation) { o.installationID = "" }, func(o *localCheckpointObservation) { o.unknown = -1 },
			func(o *localCheckpointObservation) { o.entries[0].id = lcSentinel }, func(o *localCheckpointObservation) { o.entries[0].installationID = "" }, func(o *localCheckpointObservation) { o.entries[0].installationID = strings.Repeat("a", 65) },
			func(o *localCheckpointObservation) { o.entries[0].bytes = -1 }, func(o *localCheckpointObservation) { o.entries[0].created = time.Time{} }, func(o *localCheckpointObservation) {
				o.entries[0].created = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
			}, func(o *localCheckpointObservation) { o.entries[0].created = time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC) },
		} {
			bad := o
			bad.entries = slices.Clone(o.entries)
			alter(&bad)
			r := (&lcFixture{observations: []localCheckpointObservation{bad}}).run(t, lcArgs("create"), 1, "read")
			if r.Failure.Class != "local_checkpoint_metadata_unsupported" || r.Before.Count != nil || len(r.Before.Entries) != 0 {
				t.Fatal("unsafe metadata admitted")
			}
		}
		o.installationPresent, o.installationID = false, ""
		o.entries[0] = localCheckpointSummary{id: lcID}
		(&lcFixture{observations: []localCheckpointObservation{o}}).run(t, lcArgs("inspect"), 0, "read")
		// Extra inspection fields are outside the adapter projection and output DTO.
		full := struct {
			Projection              localCheckpointObservation
			Manifest, Process, Path string
		}{o, lcSentinel, lcSentinel, lcSentinel}
		(&lcFixture{observations: []localCheckpointObservation{full.Projection}}).run(t, lcArgs("inspect"), 0, "read")
	})
	t.Run("partial-observation", func(t *testing.T) {
		o := lcListed()
		o.unknown = 3
		r := (&lcFixture{observations: []localCheckpointObservation{o}}).run(t, lcArgs("inspect"), 0, "read")
		if *r.After.UnknownCount != 3 {
			t.Fatal("unknown count lost")
		}
		for _, status := range []string{"busy", "unowned", "listed"} {
			o.status = status
			o.entries = []localCheckpointSummary{{id: lcSentinel}}
			f := &lcFixture{observations: []localCheckpointObservation{o}}
			want := status
			if status == "listed" {
				f.failRead, f.readFailure = 1, localCheckpointFailure{lcPanicError{}, "local_checkpoint_state_unsupported", "tree_unreadable", false}
				want = "unavailable"
			}
			r := f.run(t, lcArgs("inspect"), 1, "read")
			if r.After.Status != want || r.After.Count != nil || r.After.UnknownCount != nil || len(r.After.Entries) != 0 || r.Outcome == "succeeded" {
				t.Fatal("partial observation escaped")
			}
			r = f.run(t, lcArgs("delete"), 1, "read")
			if r.Target != lcID || r.Presence != "unknown" {
				t.Fatal("target observation differs")
			}
		}
	})
}
func TestLocalCheckpointErrorObservation(t *testing.T) {
	t.Run("witness-survives", func(t *testing.T) {
		after := lcListed()
		after.entries = []localCheckpointSummary{lcEntry()}
		f := &lcFixture{observations: []localCheckpointObservation{lcListed(), after}, mutationFailure: localCheckpointFailure{lcPanicError{}, "local_checkpoint_copy_failed", "", true}}
		r := f.run(t, lcArgs("create"), 1, "read,create,read")
		if r.Outcome != "failed" || r.Failure.Class != "local_checkpoint_copy_failed" || !r.Failure.CleanupIncomplete || r.After == nil || !r.After.Entries[0].Complete {
			t.Fatal("surviving witness lost")
		}
	})
	t.Run("post-busy", func(t *testing.T) {
		for _, failed := range []bool{false, true} {
			for _, status := range []string{"busy", "unowned", "error", "invalid"} {
				after := lcListed()
				after.status = status
				f := &lcFixture{observations: []localCheckpointObservation{lcListed(), after}}
				code := 1
				if failed {
					f.mutationFailure = localCheckpointFailure{lcPanicError{}, "local_checkpoint_copy_failed", "", true}
				}
				if status == "error" {
					f.failRead, f.readFailure = 2, localCheckpointFailure{lcPanicError{}, "local_cell_locked", "", false}
					if !failed {
						code = 0
					}
				}
				r := f.run(t, lcArgs("delete"), code, "read,delete,read")
				if r.Presence != "unknown" || r.Target != lcID || (failed && (r.Failure.Class != "local_checkpoint_copy_failed" || r.Outcome != "failed")) || (!failed && (r.Outcome != "succeeded" || r.Failure != nil)) {
					t.Fatal("mutation outcome overwritten")
				}
			}
		}
	})
	t.Run("concurrent-change", func(t *testing.T) {
		before, after := lcListed(), lcListed()
		before.entries = []localCheckpointSummary{lcEntry()}
		after.entries = []localCheckpointSummary{lcEntry()}
		after.entries[0].id = lcOtherID
		for _, verb := range []string{"create", "delete"} {
			for _, present := range []bool{false, true} {
				if present {
					after.entries[0].id = lcID
				} else {
					after.entries[0].id = lcOtherID
				}
				f := &lcFixture{observations: []localCheckpointObservation{before, after}}
				r := f.run(t, lcArgs(verb), 0, "read,"+verb+",read")
				if verb == "create" {
					if r.Target != "" || r.Presence != "" {
						t.Fatal("observation attributed to invocation")
					}
				} else {
					want := "absent"
					if present {
						want = "present"
					}
					if r.Target != lcID || r.Presence != want {
						t.Fatal("target observation differs")
					}
				}
			}
		}
	})
}

// Fence references in both directions, including wrappers and function values.
func lcDisconnected(sources map[string]string) bool {
	files, owners := map[string]*ast.File{}, map[string]string{}
	const handler = "local_checkpoint_command.go"
	for name, source := range sources {
		f, err := parser.ParseFile(token.NewFileSet(), name, source, 0)
		if err != nil {
			return false
		}
		files[name] = f
		for symbol := range f.Scope.Objects {
			owners[symbol] = name
		}
	}
	valid := files[handler] != nil && files["main.go"] != nil
	for name, f := range files {
		inside := name == handler
		if inside {
			for _, imp := range f.Imports {
				p, _ := strconv.Unquote(imp.Path.Value)
				if !slices.Contains([]string{"context", "encoding/json", "flag", "io", "path/filepath", "regexp", "strings", "time"}, p) {
					return false
				}
			}
		}
		var visit func(ast.Node) bool
		visit = func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.FuncDecl:
				if inside && n.Name.Name == "init" {
					valid = false
				}
				if name == "main.go" && (n.Name.Name == "run" || n.Name.Name == "usage") {
					ast.Inspect(n.Body, func(child ast.Node) bool {
						if s, ok := child.(*ast.BasicLit); ok && s.Kind == token.STRING {
							value, _ := strconv.Unquote(s.Value)
							if value == "local" || strings.Contains(value, "checkpoint") {
								valid = false
							}
						}
						return true
					})
				}
			case *ast.SelectorExpr:
				if inside && slices.Contains([]string{"Cell", "Checkpoint", "DeleteCheckpoint", "Inspect", "Stop", "Repair", "Start", "Install", "Remove", "Command", "CommandContext", "StartProcess", "ForkExec", "Syscall", "Glob", "Walk", "WalkDir", "EvalSymlinks"}, n.Sel.Name) {
					valid = false
				}
				// Visit the receiver, not selector member names as package symbols.
				ast.Inspect(n.X, visit)
				return false
			case *ast.Ident:
				if owner := owners[n.Name]; n != f.Name && owner != "" && inside != (owner == handler) {
					valid = false
				}
			}
			return true
		}
		ast.Inspect(f, visit)
	}
	return valid
}
func TestLocalCheckpointNeverStopsOrRepairs(t *testing.T) {
	t.Run("recorder", func(t *testing.T) {
		for verb, calls := range map[string]string{"create": "read,create,read", "inspect": "read", "delete": "read,delete,read"} {
			(&lcFixture{}).run(t, lcArgs(verb), 0, calls)
			(&lcFixture{}).run(t, lcReplace(lcArgs(verb), "checkpoint-mode", ""), 2, "")
		}
	})
	t.Run("source-contract", func(t *testing.T) {
		sources := map[string]string{}
		entries, err := os.ReadDir(".")
		if err != nil {
			t.Fatal("command sources unavailable")
		}
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".go") && !strings.HasSuffix(entry.Name(), "_test.go") {
				b, err := os.ReadFile(entry.Name())
				if err != nil {
					t.Fatal("command source unavailable")
				}
				sources[entry.Name()] = strings.ReplaceAll(string(b), "\r\n", "\n")
			}
		}
		if !lcDisconnected(sources) {
			t.Fatal("command disconnection violated")
		}
		const handler = "local_checkpoint_command.go"
		for _, newline := range []string{"\n", "\r\n"} {
			for _, row := range []struct{ file, old, replacement string }{
				{handler, "import (\n", "import (\n alias \"github.com/witwave-ai/witself/internal/localcell\"\n"},
				{handler, "import (\n", "import (\n . \"github.com/witwave-ai/witself/internal/localcell\"\n"},
				{handler, "import (\n", "import (\n runner \"os/exec\"\n"},
				{handler, "func localCheckpointIdentity", "var _ = cell.Stop\nfunc localCheckpointIdentity"}, {handler, "func localCheckpointIdentity", "var _ = cell.Repair\nfunc localCheckpointIdentity"},
				{handler, "func localCheckpointIdentity", "var _ = runner.Command\nfunc localCheckpointIdentity"}, {handler, "func localCheckpointIdentity", "var _ = os.StartProcess\nfunc localCheckpointIdentity"},
				{handler, "func localCheckpointIdentity", "var _ = localcell.Cell{}\nfunc localCheckpointIdentity"}, {handler, "func localCheckpointIdentity", "var _ = run\nfunc localCheckpointIdentity"},
				{handler, "func localCheckpointIdentity", "var _ = filepath.Walk\nfunc localCheckpointIdentity"},
				{handler, "func localCheckpointIdentity", "var _ = (run).arbitrary\nfunc localCheckpointIdentity"},
				{handler, "func localCheckpointIdentity", "func init() {}\nfunc localCheckpointIdentity"},
				{"main.go", "func run(args []string) int {", "func run(args []string) int {\n _ = localCheckpointCommand"},
				{"main.go", "func run(args []string) int {", "func run(args []string) int {\n _ = localCheckpointCallbacks{}"},
				{"main.go", "func run(args []string) int {", "func run(args []string) int {\n _ = \"local\""},
				{"main.go", "func usage(w io.Writer) {", "func usage(w io.Writer) {\n _ = \"witself local checkpoint\""},
			} {
				copySources := map[string]string{}
				for name, source := range sources {
					copySources[name] = strings.ReplaceAll(source, "\n", newline)
				}
				original := strings.ReplaceAll(copySources[row.file], "\r\n", "\n")
				changed := strings.Replace(original, row.old, row.replacement, 1)
				if changed == original {
					t.Fatal("source mutation did not apply")
				}
				copySources[row.file] = strings.ReplaceAll(changed, "\n", newline)
				if _, err := parser.ParseFile(token.NewFileSet(), row.file, copySources[row.file], 0); err != nil {
					t.Fatal("source mutation is invalid")
				}
				if lcDisconnected(copySources) {
					t.Fatal("source mutation escaped fence")
				}
			}
		}
	})
}
