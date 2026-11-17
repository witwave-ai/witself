package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
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
const lcNotice = "Checkpoint journal history may remain, including after purged-root delete; another purge does not prune it. Older readers may refuse that history; downgrade compatibility is unsupported."

type lcWireReceipt struct {
	Operation    string            `json:"operation"`
	Outcome      string            `json:"outcome"`
	Failure      *lcWireDiagnostic `json:"failure,omitempty"`
	Before       *lcWireSnapshot   `json:"before,omitempty"`
	After        *lcWireSnapshot   `json:"after,omitempty"`
	Target       string            `json:"target,omitempty"`
	Presence     string            `json:"presence,omitempty"`
	Verification string            `json:"verification"`
	Notice       string            `json:"notice"`
}
type lcWireSnapshot struct {
	Status         string            `json:"status"`
	Failure        *lcWireDiagnostic `json:"failure,omitempty"`
	InstallationID string            `json:"installation_id,omitempty"`
	Count          *int              `json:"count,omitempty"`
	UnknownCount   *int              `json:"unknown_count,omitempty"`
	Entries        []lcWireEntry     `json:"entries,omitempty"`
}
type lcWireEntry struct {
	ID             string `json:"id"`
	InstallationID string `json:"installation_id,omitempty"`
	Complete       *bool  `json:"complete"`
	Verification   string `json:"verification"`
}
type lcWireDiagnostic struct {
	Class             string `json:"class"`
	Reason            string `json:"reason,omitempty"`
	CleanupIncomplete *bool  `json:"cleanup_incomplete"`
}

var lcFailureClasses = []string{"local_checkpoint_state_unsupported", "local_checkpoint_identity_mismatch", "local_checkpoint_source_unsupported", "local_checkpoint_storage_insufficient", "local_checkpoint_limit_reached", "local_checkpoint_incomplete", "local_checkpoint_witness_mismatch", "local_checkpoint_copy_failed", "local_maintenance_fence_active", "local_cell_locked", "local_checkpoint_interrupted", "local_cell_layout_refused", "local_checkpoint_operation_failed"}
var lcClasses = append(slices.Clone(lcFailureClasses), "local_checkpoint_metadata_unsupported", "local_checkpoint_command_disabled", "local_checkpoint_arguments_invalid", "local_checkpoint_confirmation_required")
var lcReasons = []string{"", "boundary_unreadable", "boundary_noncanonical", "boundary_not_private", "descendant_refused", "tree_unreadable", "unknown_entries"}

type lcBrokenWriter struct{}

func (lcBrokenWriter) Write([]byte) (int, error) { return 0, lcPanicError{} }

type lcPanicError struct{}

func (lcPanicError) Error() string { panic("raw error was consulted") }

type lcFixture struct {
	golden                       string
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
	a := []string{verb, "--checkpoint-mode=cold-v1", "--root=" + filepath.Join(os.TempDir(), lcSentinel+"-root")}
	if verb == "create" {
		a = append(a, "--pgbin="+filepath.Join(os.TempDir(), lcSentinel+"-bin"), "--installation-id="+lcIdentity)
	}
	if verb == "delete" {
		a = append(a, "--checkpoint-id="+lcID)
	}
	return a
}
func (f *lcFixture) run(t *testing.T, args []string, want int, calls string) lcWireReceipt {
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
		if caught := recover(); caught != nil {
			if caught == "raw error was consulted" {
				t.Fatal("raw error was consulted")
			}
			t.Fatal("unexpected handler panic")
		}
	}()
	code := localCheckpointCommand(context.Background(), args, cb, &out, &stderr)
	if code != want || strings.Join(f.calls, ",") != calls {
		t.Fatal("exit or callback sequence differs")
	}
	if stderr.Len() != 0 || strings.Contains(out.String(), lcSentinel) {
		t.Fatal("private output escaped")
	}
	raw := out.String()
	if strings.Count(raw, "\n") != 1 || !strings.HasSuffix(raw, "\n") || strings.Contains(raw, "\r") {
		t.Fatal("receipt framing differs")
	}
	if f.golden != "" && raw != strings.ReplaceAll(f.golden, "NOTICE", lcNotice)+"\n" {
		t.Fatal("golden receipt differs")
	}
	var receipt lcWireReceipt
	decoder := json.NewDecoder(&out)
	decoder.DisallowUnknownFields()
	if decoder.Decode(&receipt) != nil || decoder.Decode(new(any)) != io.EOF {
		t.Fatal("receipt projection differs")
	}
	op := "unknown"
	if len(args) > 0 && slices.Contains([]string{"create", "inspect", "delete"}, args[0]) {
		op = args[0]
	}
	if receipt.Operation != op || receipt.Verification != "not_performed" || receipt.Notice != lcNotice {
		t.Fatal("receipt identity differs")
	}
	d := receipt.Failure
	if (code == 0 && (receipt.Outcome != "succeeded" || d != nil)) || (receipt.Outcome == "failed" && (code != 1 || d == nil)) {
		t.Fatal("receipt outcome differs")
	}
	if code == 2 && receipt.Outcome != "refused" {
		t.Fatal("refusal outcome differs")
	}
	if code == 2 && (d == nil || !slices.Contains([]string{"local_checkpoint_command_disabled", "local_checkpoint_arguments_invalid", "local_checkpoint_confirmation_required", "local_checkpoint_identity_mismatch"}, d.Class)) {
		t.Fatal("receipt refusal differs")
	}
	if code == 2 && d.Reason != "" {
		t.Fatal("refusal reason present")
	}
	if code == 2 && (d.CleanupIncomplete == nil || *d.CleanupIncomplete) {
		t.Fatal("refusal cleanup differs")
	}
	if code == 2 && receipt.After != nil {
		t.Fatal("refusal after-read present")
	}
	mutated := slices.Contains(f.calls, "create") || slices.Contains(f.calls, "delete")
	if mutated {
		if !slices.Contains([]string{"succeeded", "failed"}, receipt.Outcome) || receipt.Before == nil || receipt.After == nil {
			t.Fatal("mutation observations differ")
		}
	} else {
		outcome := "refused"
		if op == "inspect" && code == 0 {
			outcome = "succeeded"
		}
		if receipt.Outcome != outcome {
			t.Fatal("unmutated outcome differs")
		}
		if (receipt.Before != nil) != ((op == "create" || op == "delete") && len(f.calls) > 0) {
			t.Fatal("before-read presence differs")
		}
		if (receipt.After != nil) != (op == "inspect" && len(f.calls) == 1) {
			t.Fatal("observation presence differs")
		}
	}
	if op == "delete" && len(f.calls) > 0 {
		presence := "unknown"
		if receipt.After != nil && receipt.After.Status == "listed" {
			presence = "absent"
			for _, e := range receipt.After.Entries {
				if e.ID == f.requests[0].checkpointID {
					presence = "present"
				}
			}
		}
		if receipt.Target != f.requests[0].checkpointID || receipt.Presence != presence {
			t.Fatal("target observation differs")
		}
	} else if receipt.Target != "" || receipt.Presence != "" {
		t.Fatal("observation attributed to invocation")
	}
	var snapshots map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &snapshots) != nil {
		t.Fatal("suppressed field present")
	}
	diagnostics := []*lcWireDiagnostic{d}
	for index, snapshot := range []*lcWireSnapshot{receipt.Before, receipt.After} {
		if snapshot == nil {
			continue
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(snapshots[[]string{"before", "after"}[index]], &fields) != nil {
			t.Fatal("suppressed field present")
		}
		if snapshot.Status != "listed" {
			for key := range fields {
				if key != "status" && (key != "failure" || snapshot.Status != "unavailable") {
					t.Fatal("suppressed field present")
				}
			}
		} else {
			identities := []string{snapshot.InstallationID}
			for _, entry := range snapshot.Entries {
				identities = append(identities, entry.InstallationID)
			}
			for _, identity := range identities {
				if !regexp.MustCompile(`^[A-Za-z0-9_-]{0,64}$`).MatchString(identity) {
					t.Fatal("installation identity differs")
				}
			}
		}
		diagnostics = append(diagnostics, snapshot.Failure)
		if !slices.Contains([]string{"listed", "busy", "unowned", "unavailable"}, snapshot.Status) {
			t.Fatal("snapshot status differs")
		}
		if (snapshot.Failure != nil) != (snapshot.Status == "unavailable") {
			t.Fatal("snapshot failure presence differs")
		}
		if snapshot.Status != "listed" {
			if snapshot.InstallationID != "" || snapshot.Count != nil || snapshot.UnknownCount != nil || len(snapshot.Entries) != 0 {
				t.Fatal("partial observation escaped")
			}
		} else if snapshot.Count == nil || *snapshot.Count != len(snapshot.Entries) {
			t.Fatal("snapshot counts differ")
		} else if snapshot.UnknownCount == nil || *snapshot.UnknownCount < 0 {
			t.Fatal("snapshot unknown count differs")
		}
		for _, entry := range snapshot.Entries {
			if !regexp.MustCompile(`^ckpt_[a-z2-7]{16}$`).MatchString(entry.ID) || entry.Verification != "not_performed" || entry.Complete == nil {
				t.Fatal("entry projection differs")
			}
		}
	}
	for _, diagnostic := range diagnostics {
		if diagnostic != nil && (diagnostic.CleanupIncomplete == nil || !slices.Contains(lcClasses, diagnostic.Class) || !slices.Contains(lcReasons, diagnostic.Reason)) {
			t.Fatal("diagnostic projection differs")
		}
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
		(&lcFixture{golden: `{"operation":"delete","outcome":"refused","failure":{"class":"local_checkpoint_command_disabled","cleanup_incomplete":false},"verification":"not_performed","notice":"NOTICE"}`}).run(t, []string{"delete"}, 2, "")
		for _, args := range [][]string{nil, {"inspect"}, {lcSentinel}, {"--checkpoint-mode=cold-v1", "inspect"}} {
			if (&lcFixture{}).run(t, args, 2, "").Failure.Class != "local_checkpoint_command_disabled" {
				t.Fatal("selector refusal differs")
			}
		}
	})
	t.Run("unknown", func(t *testing.T) {
		t.Setenv("WITSELF_CHECKPOINT_MODE", "cold-v1")
		for _, mode := range []string{"", lcSentinel, "cold-v1x", "cold-v10", "cold-v1-next", "COLD-V1", "cold-v", " cold-v1", "cold-v1 "} {
			r := (&lcFixture{}).run(t, lcReplace(lcArgs("inspect"), "checkpoint-mode", mode), 2, "")
			if r.Failure.Class != "local_checkpoint_command_disabled" {
				t.Fatal("selector refusal differs")
			}
		}
		for _, args := range [][]string{{"inspect", "--foreign", "--checkpoint-mode=cold-v1"}, {lcSentinel, "--pgbin=", "--checkpoint-mode=cold-v1"}, {"inspect", "--checkpoint-mode=other", "--checkpoint-mode=cold-v1"}} {
			if (&lcFixture{}).run(t, args, 2, "").Failure.Class != "local_checkpoint_command_disabled" {
				t.Fatal("selector refusal differs")
			}
		}
	})
	t.Run("explicit", func(t *testing.T) {
		if (&lcFixture{}).run(t, lcArgs(lcSentinel), 2, "").Failure.Class != "local_checkpoint_arguments_invalid" {
			t.Fatal("argument refusal differs")
		}
		for verb, calls := range map[string]string{"create": "read,create,read", "inspect": "read", "delete": "read,delete,read"} {
			f := &lcFixture{}
			if verb == "create" {
				f.golden = `{"operation":"create","outcome":"succeeded","before":{"status":"listed","installation_id":"installation_A","count":0,"unknown_count":0},"after":{"status":"listed","installation_id":"installation_A","count":0,"unknown_count":0},"verification":"not_performed","notice":"NOTICE"}`
			}
			if verb == "delete" {
				f.golden = `{"operation":"delete","outcome":"succeeded","before":{"status":"listed","installation_id":"installation_A","count":0,"unknown_count":0},"after":{"status":"listed","installation_id":"installation_A","count":0,"unknown_count":0},"target":"ckpt_abcdefghijklmnop","presence":"absent","verification":"not_performed","notice":"NOTICE"}`
			}
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
				if (&lcFixture{}).run(t, append(lcArgs(verb), extra), 2, "").Failure.Class != "local_checkpoint_arguments_invalid" {
					t.Fatal("argument refusal differs")
				}
			}
		}
		if (&lcFixture{}).run(t, append(lcArgs("create"), "--checkpoint-id="+lcID), 2, "").Failure.Class != "local_checkpoint_arguments_invalid" {
			t.Fatal("argument refusal differs")
		}
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
		for _, value := range []string{"latest", "oldest", "all", "*", "ckpt_abc", lcID + "*", "bad space", strings.Repeat("a", 65), "ckpt_abcdefghijklmno0", "ckpt_abcdefghijklmno1", "ckpt_abcdefghijklmno8", "ckpt_abcdefghijklmno9", "ckpt_Abcdefghijklmnop", "ckpt_abcdefghijklmno", "ckpt_abcdefghijklmnopq", "copy_abcdefghijklmnop"} {
			if (&lcFixture{}).run(t, lcReplace(lcArgs("delete"), "checkpoint-id", value), 2, "").Failure.Class != "local_checkpoint_arguments_invalid" {
				t.Fatal("argument refusal differs")
			}
		}
		for _, value := range []string{"latest", "oldest", "all", "*", "bad space", strings.Repeat("a", 65)} {
			if (&lcFixture{}).run(t, lcReplace(lcArgs("create"), "installation-id", value), 2, "").Failure.Class != "local_checkpoint_arguments_invalid" {
				t.Fatal("argument refusal differs")
			}
		}
		for _, verb := range []string{"create", "delete", "inspect"} {
			args := lcArgs(verb)
			for _, flag := range append(slices.Clone(args[1:]), "--timeout=30s") {
				name, value, _ := strings.Cut(strings.TrimPrefix(flag, "--"), "=")
				for _, duplicate := range [][]string{{flag}, {"-" + name, value}, {"--" + name, value}, {"-" + name + "=" + value}} {
					base := args
					if name == "timeout" {
						base = append(slices.Clone(args), flag)
					}
					if (&lcFixture{}).run(t, append(slices.Clone(base), duplicate...), 2, "").Failure.Class != "local_checkpoint_arguments_invalid" {
						t.Fatal("repeated flag admitted")
					}
				}
			}
		}
		for _, args := range [][]string{append(lcArgs("create"), "--installation-id=different"), append(lcArgs("delete"), "--checkpoint-id="+lcOtherID)} {
			if (&lcFixture{}).run(t, args, 2, "").Failure.Class != "local_checkpoint_arguments_invalid" {
				t.Fatal("repeated consent admitted")
			}
		}
		for _, value := range []string{"installation", "different"} {
			r := (&lcFixture{}).run(t, lcReplace(lcArgs("create"), "installation-id", value), 2, "read")
			if r.Failure.Class != "local_checkpoint_identity_mismatch" {
				t.Fatal("exact consent guard differs")
			}
		}
	})
}
func lcAfter(t *testing.T, snapshot *lcWireSnapshot, status, class, reason string) {
	t.Helper()
	if status == "error" || status == "invalid" {
		if status == "invalid" {
			class, reason = "local_checkpoint_metadata_unsupported", ""
		}
		if snapshot.Status != "unavailable" || snapshot.Failure == nil || snapshot.Failure.Class != class || snapshot.Failure.Reason != reason || *snapshot.Failure.CleanupIncomplete {
			t.Fatal("after diagnostic differs")
		}
	} else if snapshot.Status != status || snapshot.Failure != nil {
		t.Fatal("after status differs")
	}
}
func TestLocalCheckpointOutput(t *testing.T) {
	t.Run("raw-error", func(t *testing.T) {
		classes := lcFailureClasses
		reasons := lcReasons
		for _, class := range classes {
			for _, reason := range reasons {
				for _, cleanup := range []bool{false, true} {
					f := &lcFixture{mutationFailure: localCheckpointFailure{lcPanicError{}, class, reason, cleanup}}
					r := f.run(t, lcArgs("create"), 1, "read,create,read")
					if r.Failure.Class != class || r.Failure.Reason != reason || *r.Failure.CleanupIncomplete != cleanup {
						t.Fatal("bounded failure changed")
					}
				}
			}
		}
		var stderr bytes.Buffer
		if localCheckpointCommand(context.Background(), nil, localCheckpointCallbacks{}, lcBrokenWriter{}, &stderr) != 1 || stderr.String() != "local_checkpoint_operation_failed\n" {
			t.Fatal("writer failure differs")
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
		o.entries = []localCheckpointSummary{lcEntry(), lcEntry()}
		o.entries[1].id = lcOtherID
		r := (&lcFixture{observations: []localCheckpointObservation{o}, golden: `{"operation":"inspect","outcome":"succeeded","after":{"status":"listed","installation_id":"installation_A","count":2,"unknown_count":0,"entries":[{"id":"ckpt_abcdefghijklmnop","installation_id":"installation_A","complete":true,"verification":"not_performed"},{"id":"ckpt_bcdefghijklmnopq","installation_id":"installation_A","complete":true,"verification":"not_performed"}]},"verification":"not_performed","notice":"NOTICE"}`}).run(t, lcArgs("inspect"), 0, "read")
		if r.After.InstallationID != lcIdentity || (r.After.Entries[0].ID != lcID || r.After.Entries[0].InstallationID != lcIdentity || !*r.After.Entries[0].Complete) || *r.After.Count != 2 {
			t.Fatal("identifier exception differs")
		}
		for _, alter := range []func(*localCheckpointObservation){
			func(o *localCheckpointObservation) { o.installationID = lcSentinel + "!" }, func(o *localCheckpointObservation) { o.installationID = "" }, func(o *localCheckpointObservation) { o.unknown = -1 },
			func(o *localCheckpointObservation) { o.entries[0].id = lcSentinel }, func(o *localCheckpointObservation) { o.entries[0].id = "ckpt_abcdefghijklmno8" }, func(o *localCheckpointObservation) { o.entries[0].installationID = "" }, func(o *localCheckpointObservation) { o.entries[0].installationID = strings.Repeat("a", 65) },
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

	})
	t.Run("partial-observation", func(t *testing.T) {
		o := lcListed()
		o.unknown = 3
		r := (&lcFixture{observations: []localCheckpointObservation{o}}).run(t, lcArgs("inspect"), 0, "read")
		if *r.After.UnknownCount != 3 {
			t.Fatal("unknown count lost")
		}
		for _, status := range []string{"busy", "unowned", "error", "invalid"} {
			o.status = status
			o.entries = []localCheckpointSummary{lcEntry()}
			f := &lcFixture{observations: []localCheckpointObservation{o}}
			want := status
			if status == "error" {
				f.failRead, f.readFailure = 1, localCheckpointFailure{lcPanicError{}, "local_checkpoint_state_unsupported", "tree_unreadable", false}
				want = "unavailable"
			}
			if status == "invalid" {
				want = "unavailable"
			}
			r := f.run(t, lcArgs("inspect"), 1, "read")
			lcAfter(t, r.After, status, "local_checkpoint_state_unsupported", "tree_unreadable")
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
		(&lcFixture{failRead: 2, readFailure: localCheckpointFailure{lcPanicError{}, "local_cell_locked", "tree_unreadable", false}, mutationFailure: localCheckpointFailure{lcPanicError{}, "local_checkpoint_copy_failed", "", true}, golden: `{"operation":"create","outcome":"failed","failure":{"class":"local_checkpoint_copy_failed","cleanup_incomplete":true},"before":{"status":"listed","installation_id":"installation_A","count":0,"unknown_count":0},"after":{"status":"unavailable","failure":{"class":"local_cell_locked","reason":"tree_unreadable","cleanup_incomplete":false}},"verification":"not_performed","notice":"NOTICE"}`}).run(t, lcArgs("create"), 1, "read,create,read")
		after := lcListed()
		after.entries = []localCheckpointSummary{lcEntry()}
		f := &lcFixture{observations: []localCheckpointObservation{lcListed(), after}, mutationFailure: localCheckpointFailure{lcPanicError{}, "local_checkpoint_copy_failed", "", true}}
		r := f.run(t, lcArgs("create"), 1, "read,create,read")
		if r.Outcome != "failed" || r.Failure.Class != "local_checkpoint_copy_failed" || !*r.Failure.CleanupIncomplete || r.After == nil || !*r.After.Entries[0].Complete {
			t.Fatal("surviving witness lost")
		}
	})
	t.Run("post-busy", func(t *testing.T) {
		for _, failed := range []bool{false, true} {
			for _, status := range []string{"busy", "unowned", "error", "invalid"} {
				after := lcListed()
				after.status = status
				after.entries = []localCheckpointSummary{lcEntry()}
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
				lcAfter(t, r.After, status, "local_cell_locked", "")
				if r.Presence != "unknown" || r.Target != lcID || (failed && (r.Failure.Class != "local_checkpoint_copy_failed" || r.Outcome != "failed")) || (!failed && (r.Outcome != "succeeded" || r.Failure != nil)) {
					t.Fatal("mutation outcome overwritten")
				}
			}
		}
	})
	t.Run("concurrent-change", func(t *testing.T) {
		before, after := lcListed(), lcListed()
		before.entries = []localCheckpointSummary{lcEntry()}
		after.entries = []localCheckpointSummary{lcEntry(), lcEntry()}
		after.entries[0].complete = false
		after.entries[1].id = "ckpt_cdefghijklmnopqr"
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
				if len(r.After.Entries) != 2 {
					t.Fatal("entry completion differs")
				}
				for i, entry := range r.After.Entries {
					if entry.ID != after.entries[i].id || *entry.Complete != after.entries[i].complete {
						t.Fatal("entry completion differs")
					}
				}
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

// The module guard also rejects directives hidden in comments or string data.
func lcModuleDisconnected(sources map[string]string) bool {
	for _, source := range sources {
		if strings.Contains(source, "go:"+"linkname") {
			return false
		}
	}
	return true
}
func lcStringConstant(n ast.Node) (string, bool) {
	switch n := n.(type) {
	case *ast.BasicLit:
		s, err := strconv.Unquote(n.Value)
		return s, err == nil && n.Kind == token.STRING
	case *ast.ParenExpr:
		return lcStringConstant(n.X)
	case *ast.BinaryExpr:
		left, l := lcStringConstant(n.X)
		right, r := lcStringConstant(n.Y)
		return left + right, l && r && n.Op == token.ADD
	}
	return "", false
}

func lcModuleSources(root string) (module, sources map[string]string, err error) {
	module, sources = map[string]string{}, map[string]string{}
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path != root && (strings.HasPrefix(entry.Name(), ".") || slices.Contains([]string{"node_modules", "vendor", "testdata"}, entry.Name())) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(filepath.ToSlash(path), filepath.ToSlash(root)+"/")
		module[rel] = strings.ReplaceAll(string(b), "\r\n", "\n")
		if filepath.Dir(path) == filepath.Join(root, "cmd", "witself") {
			sources[entry.Name()] = module[rel]
		}
		return nil
	})
	return module, sources, err
}

func TestLocalCheckpointNeverStopsOrRepairs(t *testing.T) {
	t.Run("recorder", func(t *testing.T) {
		for verb, calls := range map[string]string{"create": "read,create,read", "inspect": "read", "delete": "read,delete,read"} {
			(&lcFixture{}).run(t, lcArgs(verb), 0, calls)
			(&lcFixture{}).run(t, lcReplace(lcArgs(verb), "checkpoint-mode", ""), 2, "")
		}
	})
	t.Run("source-contract", func(t *testing.T) {
		root := filepath.Join("..", "..")
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
			t.Fatal("module root unavailable")
		}
		module, _, err := lcModuleSources(root)
		if err != nil {
			t.Fatal("module walk failed")
		}
		for _, name := range []string{"cmd/witself/main.go", "cmd/witself/local_checkpoint_command.go", "cmd/witself/local_checkpoint_adapter.go", "internal/localcell/checkpoint.go"} {
			if module[name] == "" {
				t.Fatal("known module source missing")
			}
		}
		for failure := range lcFence(module) {
			switch failure {
			case "linkname":
				t.Fatal("module disconnection violated")
			case "isolation":
				t.Fatal("handler isolation violated")
			case "prefix":
				t.Fatal("reserved prefix violated")
			case "outside":
				t.Fatal("reserved name or local cell import outside the two files")
			case "references":
				t.Fatal("adapter references outside its closed set")
			case "allowlist":
				t.Fatal("adapter import or selector outside the allowlist")
			case "init":
				t.Fatal("init function in handler or adapter")
			case "variable":
				t.Fatal("package variable in the adapter")
			case "route":
				t.Fatal("command route or help line differs from its pin")
			case "entry":
				t.Fatal("an entry function differs from its pinned text")
			default:
				t.Fatal("unclassified fence failure")
			}
		}
		if _, _, err := lcModuleSources(filepath.Join(t.TempDir(), "missing")); err == nil {
			t.Fatal("module walk error swallowed")
		}
		fixture := t.TempDir()
		for _, name := range []string{"kept.v2/a.go", "cmd/witself/b.go", ".hidden/c.go", "vendor/d.go", "node_modules/e.go", "testdata/f.go", "kept.v2/g_test.go", "kept.v2/h.txt"} {
			path := filepath.Join(fixture, filepath.FromSlash(name))
			if os.MkdirAll(filepath.Dir(path), 0o700) != nil || os.WriteFile(path, []byte("package fixture\n"), 0o600) != nil {
				t.Fatal("module walk skip set differs")
			}
		}
		kept, command, err := lcModuleSources(fixture)
		if err != nil || len(kept) != 2 || kept["kept.v2/a.go"] == "" || kept["cmd/witself/b.go"] == "" || len(command) != 1 || command["b.go"] == "" {
			t.Fatal("module walk skip set differs")
		}
		const handler, adapter = "local_checkpoint_command.go", "local_checkpoint_adapter.go"
		linked := `package main
import("context"; "io"; "time"; _ "unsafe")
type reviewRequest struct { operation, root, pgbin, installationID, checkpointID string; timeout time.Duration }
type reviewFailure struct { err error; class, reason string; cleanup bool }
type reviewSummary struct { id, installationID string; created time.Time; bytes int64; complete bool }
type reviewObservation struct { status, installationID string; installationPresent bool; unknown int; entries []reviewSummary }
type reviewCallbacks struct { create func(context.Context, reviewRequest) reviewFailure; read func(context.Context, reviewRequest) (reviewObservation, reviewFailure); delete func(context.Context, reviewRequest) reviewFailure }
` + "//go:" + "linkname reviewLinkedCommand main.localCheckpointCommand\n" + `func reviewLinkedCommand(context.Context, []string, reviewCallbacks, io.Writer, io.Writer) int
func reviewInvokeCommand() int { return reviewLinkedCommand(context.Background(), nil, reviewCallbacks{}, io.Discard, io.Discard) }
func init() { reviewInvokeCommand() }
`
		base := map[string]string{}
		for name, source := range module {
			if name == "cmd/witself/main.go" || strings.Contains(source, "localCheckpoint") || strings.Contains(source, "witself/internal/localcell\"") {
				base[name] = source
			}
		}
		for _, newline := range []string{"\n", "\r\n"} {
			for _, row := range []struct{ file, old, replacement, want string }{
				{"linked.go", "", linked, "linkname"},
				{handler, "func localCheckpointCommand", "//go:" + "linkname localCheckpointCommand\nfunc localCheckpointCommand", "linkname"},
				{"internal/example/linked.go", "", strings.Replace(linked, "package main", "package example", 1), "linkname"},
				{"main.go", "func run(args []string) int {", "func run(args []string) int {\n _ = \"check\" + \"point\"", "route"},
				{"main.go", "func run(args []string) int {", "func run(args []string) int {\n _ = \"loc\" + \"al\"", "route"},
				{"main.go", "func usage(w io.Writer) {", "func usage(w io.Writer) {\n _ = (\"check\" + (\"po\" + \"int\"))", "route"},
				{handler, "import (\n", "import (\n alias \"github.com/witwave-ai/witself/internal/localcell\"\n", "isolation"},
				{handler, "import (\n", "import (\n . \"github.com/witwave-ai/witself/internal/localcell\"\n", "isolation"},
				{handler, "import (\n", "import (\n runner \"os/exec\"\n", "isolation"},
				{handler, "func localCheckpointIdentity", "var _ = cell.Stop\nfunc localCheckpointIdentity", "isolation"}, {handler, "func localCheckpointIdentity", "var _ = cell.Repair\nfunc localCheckpointIdentity", "isolation"},
				{handler, "func localCheckpointIdentity", "var _ = runner.Command\nfunc localCheckpointIdentity", "isolation"}, {handler, "func localCheckpointIdentity", "var _ = os.StartProcess\nfunc localCheckpointIdentity", "isolation"},
				{handler, "func localCheckpointIdentity", "var _ = localcell.Cell{}\nfunc localCheckpointIdentity", "isolation"}, {handler, "func localCheckpointIdentity", "var _ = run\nfunc localCheckpointIdentity", "isolation"},
				{handler, "func localCheckpointIdentity", "var _ = filepath.Walk\nfunc localCheckpointIdentity", "isolation"},
				{handler, "func localCheckpointIdentity", "var _ = (run).arbitrary\nfunc localCheckpointIdentity", "isolation"},
				{handler, "func localCheckpointIdentity", "func init() {}\nfunc localCheckpointIdentity", "init"},
				{"main.go", "func run(args []string) int {", "func run(args []string) int {\n _ = localCheckpointCommand", "outside"},
				{"main.go", "func run(args []string) int {", "func run(args []string) int {\n _ = localCheckpointCallbacks{}", "outside"},
				{"main.go", "func run(args []string) int {", "func run(args []string) int {\n _ = \"local\"", "route"},
				{"main.go", "func usage(w io.Writer) {", "func usage(w io.Writer) {\n _ = \"witself local checkpoint\"", "route"},
				{handler, "func localCheckpointIdentity", "var _ = usage\nfunc localCheckpointIdentity", "isolation"},
				{adapter, "func localCheckpointFailed", "var _ = (*localcell.Cell).Stop\nfunc localCheckpointFailed", "allowlist"}, {adapter, "func localCheckpointFailed", "var _ = (*localcell.Cell).Repair\nfunc localCheckpointFailed", "allowlist"},
				{adapter, "\t\"strings\"\n", "\t\"os/exec\"\n\t\"strings\"\n", "allowlist"}, {adapter, "func localCheckpointFailed", "var _ = exec.CommandContext\nfunc localCheckpointFailed", "allowlist"},
				{adapter, "\t\"strings\"\n", "\t\"os/user\"\n\t\"strings\"\n", "allowlist"}, {adapter, "func localCheckpointFailed", "var _ localCheckpointReceipt\nfunc localCheckpointFailed", "references"},
				{adapter, "func localCheckpointFailed", "func localCheckpointProbe() { signal.Reset() }\nfunc localCheckpointFailed", "allowlist"}, {adapter, "\tcontext.AfterFunc(ctx, stop)\n", "", "entry"},
				{adapter, "func localCheckpointFailed", "func localCheckpointProbe() { _ = syscall.Kill }\nfunc localCheckpointFailed", "allowlist"},
				{adapter, "failure := localcell.ClassifyCheckpointFailure(err)\n", "failure := localcell.ClassifyCheckpointFailure(err)\n\t_ = syscall.SIGTERM\n", "allowlist"},
				{adapter, "func localCheckpointFailed", "var _ = run\nfunc localCheckpointFailed", "references"}, {adapter, "func localCheckpointFailed", "func checkpointHelper() {}\nfunc localCheckpointFailed", "prefix"},
				{adapter, "func localCheckpointFailed", "func init() {}\nfunc localCheckpointFailed", "init"}, {"route.go", "", "package main\n\nvar _ = localCheckpointRun\n", "outside"},
				{"main.go", "import (\n", "import (\n\t\"github.com/witwave-ai/witself/internal/localcell\"\n", "outside"}, {"unrelated.go", "", "package main\n\nvar mode, status, timeout string\n", ""},
				{adapter, "func localCheckpointFailed", "var _ = func() error { cell, _ := localcell.New(localcell.Config{}); return cell.DeleteCheckpoint(nil, localcell.DeleteCheckpointOptions{}) }()\nfunc localCheckpointFailed", "variable"}, {adapter, "func localCheckpointFailed", "var _ = func() error { cell, _ := localCheckpointOpen(localCheckpointColdV1, localCheckpointRequest{}); return cell.Checkpoint(nil, localcell.CheckpointOptions{}) }()\nfunc localCheckpointFailed", "variable"},
				{adapter, "func localCheckpointFailed", "var localCheckpointCreate = localCheckpointCell.Checkpoint\nfunc localCheckpointFailed", "variable"}, {adapter, "func localCheckpointFailed", "var localCheckpointUnguarded = func(_ localCheckpointMode, r localCheckpointRequest) (localCheckpointCell, error) { return localcell.New(localcell.Config{Root: r.root}) }\nfunc localCheckpointFailed", "variable"},
				{adapter, "err = cell.DeleteCheckpoint(", "_ = cell.(interface{ Stop(context.Context) error }).Stop(ctx)\n\t\t\terr = cell.DeleteCheckpoint(", "allowlist"}, {adapter, "err = cell.DeleteCheckpoint(", "_ = cell.(interface{ Repair(context.Context) error }).Repair(ctx)\n\t\t\terr = cell.DeleteCheckpoint(", "allowlist"},
				{handler, "import (\n", "import (\n _ \"os/exec\"\n", "isolation"}, {adapter, "PostgresDir: r.pgbin}", "PostgresDir: r.pgbin, Server: r.pgbin}", "allowlist"},
				{"main.go", "func usage(w io.Writer) {", "func usageRenamed(w io.Writer) {\n _ = \"witself local checkpoint\"", "route"}, {"other.go", "", "package main\n\nimport cellpkg `github.com/witwave-ai/witself/internal/localcell`\n\nvar _ = (*cellpkg.Cell).Stop\n", "outside"},
				{"main.go", "\tcase \"local\":\n", "\tcase \"local\", \"lcl\":\n", "route"}, {"main.go", "\tcase \"local\":\n", "\tcase \"lcl\":\n\t\treturn localCheckpointNamespace(args[1:])\n\tcase \"local\":\n", "outside"},
				{"main.go", "\tcliout.Line(w, \"  witself help", "\tcliout.Line(w, \"" + lcHelp + "\")\n\tcliout.Line(w, \"  witself help", "route"}, {"main.go", "func usage(w io.Writer) {", "func usage(w io.Writer) {\n\t_ = localCheckpointNamespace", "outside"},
				{"main.go", "\t\"os\"\n", "\t\"os\"\n\t\"os/signal\"\n", "route"}, {adapter, "localCheckpointRun(ctx, args[1:],", "localCheckpointRun(ctx, append(args[1:], \"--checkpoint-mode=cold-v1\"),", "entry"},
				{adapter, "ctx, stop := localCheckpointSignals()", "_, stop := localCheckpointSignals()\n\tctx := context.Background()", "entry"}, {adapter, "err = cell.DeleteCheckpoint(", "_ = os.Stdout\n\t\t\terr = cell.DeleteCheckpoint(", "allowlist"},
				{adapter, "err = cell.DeleteCheckpoint(", "_, _ = localCheckpointSignals()\n\t\t\terr = cell.DeleteCheckpoint(", "references"},
				{"main.go", "\tcase \"local\":\n", "\tcase \"lcl\":\n\t\t_ = \"local\"\n", "route"}, {"main.go", "localCheckpointNamespace(args[1:])", "localCheckpointNamespace(args)", "route"},
			} {
				input := map[string]string{}
				for name, source := range base {
					input[name] = strings.ReplaceAll(source, "\n", newline)
				}
				file := row.file
				if !strings.Contains(file, "/") {
					file = "cmd/witself/" + file
				}
				original := strings.ReplaceAll(input[file], "\r\n", "\n")
				changed := strings.Replace(original, row.old, row.replacement, 1)
				if changed == original {
					t.Fatal("source mutation did not apply")
				}
				input[file] = strings.ReplaceAll(changed, "\n", newline)
				if _, err := parser.ParseFile(token.NewFileSet(), file, input[file], 0); err != nil {
					t.Fatal("source mutation is invalid")
				}
				if failures := lcFence(input); row.want == "" && len(failures) != 0 {
					t.Fatal("unrelated declaration rejected")
				} else if row.want != "" && !failures[row.want] {
					t.Fatal("source mutation escaped fence")
				}
			}
		}
	})
}
