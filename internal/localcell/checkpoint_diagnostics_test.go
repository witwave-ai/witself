package localcell

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func checkpointDiagnosticReason(t *testing.T, err error, reason string) {
	t.Helper()
	if !errors.Is(err, errCheckpointState) || err.Error() != "local_checkpoint_state_unsupported" ||
		ClassifyCheckpointFailure(err) != (CheckpointFailure{Class: "local_checkpoint_state_unsupported", Reason: reason}) {
		t.Fatal("checkpoint refusal lost its safe reason or fixed cause")
	}
}

func checkpointDiagnosticPublic(t *testing.T, k *checkpointCase, reason string) (Inspection, []error) {
	t.Helper()
	inspection, inspectErr := k.c.Inspect(t.Context())
	errs := []error{k.c.Checkpoint(t.Context(), *k.options),
		k.c.DeleteCheckpoint(t.Context(), DeleteCheckpointOptions{CheckpointID: "ckpt_aaaaaaaaaaaaaaaa"}), inspectErr}
	for _, err := range errs {
		checkpointDiagnosticReason(t, err, reason)
	}
	if inspection.CheckpointListing != "listed" || len(inspection.Checkpoints) != 0 {
		t.Fatal("refused recovery tree was exposed as a successful listing")
	}
	return inspection, errs
}

func TestCheckpointRecoveryDiagnostics(t *testing.T) {
	t.Run("boundary", func(t *testing.T) {
		t.Run("noncanonical", func(t *testing.T) {
			k := checkpointInspectCase(t)
			if privateDir(filepath.Join(k.c.cfg.Root, "Recovery"), true) != nil {
				t.Fatal("cannot prepare boundary spelling fixture")
			}
			if _, err := os.Lstat(k.c.recoveryDir()); errors.Is(err, os.ErrNotExist) {
				t.Skip("case sensitive volume cannot resolve the noncanonical boundary")
			}
			checkpointDiagnosticReason(t, k.c.validateRecovery(), "boundary_noncanonical")
		})
		t.Run("not-private", func(t *testing.T) {
			k := checkpointInspectCase(t)
			checkpointWrite(t, k.c, "recovery", "synthetic")
			checkpointDiagnosticReason(t, k.c.validateRecovery(), "boundary_not_private")
		})
		t.Run("unreadable", func(t *testing.T) {
			k := checkpointInspectCase(t)
			k.c.cfg.Root += string(rune(0))
			checkpointDiagnosticReason(t, k.c.validateRecovery(), "boundary_unreadable")
		})
	})
	t.Run("descendant", func(t *testing.T) {
		k := checkpointInspectCase(t)
		dir := checkpointPlant(t, k.c, "ckpt_aaaaaaaaaaaaaaaa", "ckpt_aaaaaaaaaaaaaaaa", "object")
		recoveryBoundaryNonPrivate(t, filepath.Join(dir, "object"), false)
		checkpointRefusedEverywhere(t, k.f)
		checkpointDiagnosticReason(t, k.c.validateRecovery(), "descendant_refused")
		checkpointDiagnosticPublic(t, k, "descendant_refused")
	})
	t.Run("unknown", func(t *testing.T) {
		k := checkpointInspectCase(t)
		checkpointPlant(t, k.c, "DISTINCTIVE-UNKNOWN-ENTRY", "", "object")
		inspection := checkpointInspectState(t, k.c, "listed")
		data, err := json.Marshal(inspection)
		if err != nil || inspection.CheckpointUnknown != 1 || len(inspection.Checkpoints) != 0 ||
			!strings.Contains(string(data), `"checkpoint_unknown":1`) || strings.Contains(string(data), "DISTINCTIVE-UNKNOWN-ENTRY") {
			t.Fatal("unknown recovery count was lost or exposed a name")
		}
		checkpointDiagnosticReason(t, k.c.Checkpoint(t.Context(), *k.options), "unknown_entries")
	})
	t.Run("read-error", func(t *testing.T) {
		for _, row := range []string{"walk-error", "nil-entry", "vanished-leaf"} {
			t.Run(row, func(t *testing.T) {
				k := checkpointInspectCase(t)
				dir := checkpointPlant(t, k.c, "ckpt_aaaaaaaaaaaaaaaa", "", "object")
				passes := 0
				walk := func(root string, visit fs.WalkDirFunc) error {
					passes++
					if row == "walk-error" {
						return visit(root, nil, os.ErrPermission)
					}
					if row == "nil-entry" {
						return visit(root, nil, nil)
					}
					return filepath.WalkDir(root, visit)
				}
				validate := func(name string, directory bool) error {
					if name == filepath.Join(dir, "object") {
						return os.ErrNotExist
					}
					return validateRecoveryObject(name, directory)
				}
				err := validatePrivateTreeSnapshot(k.c.recoveryDir(), walk, validate, os.Lstat)
				checkpointDiagnosticReason(t, recoveryTreeFailure(err), "tree_unreadable")
				if row == "vanished-leaf" && passes != 3 || row != "vanished-leaf" && passes != 1 {
					t.Fatal("diagnostic changed the snapshot retry boundary")
				}
			})
		}
		// The existing ownership barrier proves Inspect propagates a failed RV.
		// A real walk failure cannot be scheduled after strict private validation
		// without an additional hook or a racing mutation of the filesystem.
		k := checkpointInspectCase(t)
		root := k.c.cfg.Root
		k.c.inspectCheckpointListing = func(state string) {
			if state == "owned" {
				k.c.cfg.Root += string(rune(0))
			}
		}
		inspection, err := k.c.Inspect(t.Context())
		k.c.cfg.Root = root
		checkpointDiagnosticReason(t, err, "boundary_unreadable")
		if inspection.CheckpointListing != "listed" || len(inspection.Checkpoints) != 0 {
			t.Fatal("inspection lost its failed owned listing state")
		}
		// Normalize CRLF because this source contract also runs on Windows.
		source, readErr := os.ReadFile("checkpoint.go")
		if readErr != nil {
			t.Fatal("cannot read checkpoint source")
		}
		body := strings.SplitN(strings.SplitN(strings.ReplaceAll(string(source), "\r\n", "\n"), "func (c *Cell) checkpointSummaries(", 2)[1], "// validateRecovery", 2)[0]
		if !strings.Contains(body, "if err := c.validateRecovery(); err != nil {\n\t\treturn err\n\t}") {
			t.Fatal("inspection no longer propagates recovery validation failure")
		}
	})
	t.Run("redaction", func(t *testing.T) {
		k := checkpointInspectCase(t)
		const marker = "DISTINCTIVE-REFUSED-MARKER"
		dir := checkpointPlant(t, k.c, "ckpt_aaaaaaaaaaaaaaaa", "ckpt_aaaaaaaaaaaaaaaa", marker)
		recoveryBoundaryNonPrivate(t, filepath.Join(dir, marker), false)
		inspection, errs := checkpointDiagnosticPublic(t, k, "descendant_refused")
		errs = append(errs, k.c.validateRecovery())
		data, marshalErr := json.Marshal(inspection)
		if marshalErr != nil {
			t.Fatal("cannot encode safe inspection")
		}
		texts := []string{string(data)}
		for _, err := range errs {
			classification, marshalErr := json.Marshal(ClassifyCheckpointFailure(err))
			if marshalErr != nil {
				t.Fatal("cannot encode safe classification")
			}
			texts = append(texts, err.Error(), fmt.Sprintf("%v", err), fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", err), string(classification))
		}
		for _, text := range texts {
			if strings.Contains(text, marker) || strings.Contains(text, k.c.cfg.Root) {
				t.Fatal("recovery diagnostic exposed private fixture identity")
			}
		}
	})
}

func TestDeleteCheckpointUnrelatedRefusal(t *testing.T) {
	t.Run("unsafe", func(t *testing.T) {
		k := checkpointInspectCase(t)
		checkpointPlant(t, k.c, "ckpt_aaaaaaaaaaaaaaaa", "ckpt_aaaaaaaaaaaaaaaa", "object")
		dir := checkpointPlant(t, k.c, "ckpt_bbbbbbbbbbbbbbbb", "ckpt_bbbbbbbbbbbbbbbb", "object")
		recoveryBoundaryNonPrivate(t, filepath.Join(dir, "object"), false)
		before := checkpointInspectInventory(t, k.c)
		err := k.c.DeleteCheckpoint(t.Context(), DeleteCheckpointOptions{CheckpointID: "ckpt_aaaaaaaaaaaaaaaa"})
		checkpointDiagnosticReason(t, err, "descendant_refused")
		if !reflect.DeepEqual(before, checkpointInspectInventory(t, k.c)) {
			t.Fatal("unrelated refusal changed the root inventory")
		}
	})
	t.Run("private-unknown", func(t *testing.T) {
		k := checkpointInspectCase(t)
		target := checkpointPlant(t, k.c, "ckpt_aaaaaaaaaaaaaaaa", "ckpt_aaaaaaaaaaaaaaaa", "object")
		sibling := checkpointPlant(t, k.c, "ckpt_bbbbbbbbbbbbbbbb", "ckpt_bbbbbbbbbbbbbbbb", "object")
		unknown := checkpointPlant(t, k.c, "private-unknown", "", "object")
		before := checkpointInspectInventory(t, k.c)
		if k.c.DeleteCheckpoint(t.Context(), DeleteCheckpointOptions{CheckpointID: "ckpt_aaaaaaaaaaaaaaaa"}) != nil {
			t.Fatal("private unknown entry prevented exact identifier deletion")
		}
		after := checkpointInspectInventory(t, k.c)
		for _, entries := range []map[string]retainedEntry{before, after} {
			for name := range entries {
				if name != unknown && !strings.HasPrefix(name, unknown+string(filepath.Separator)) &&
					name != sibling && !strings.HasPrefix(name, sibling+string(filepath.Separator)) {
					delete(entries, name)
				}
			}
		}
		if !reflect.DeepEqual(before, after) {
			t.Fatal("exact identifier deletion changed an unrelated entry")
		}
		if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("exact identifier deletion did not remove its target")
		}
	})
}
