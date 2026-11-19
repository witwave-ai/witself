//go:build windows

package localcell

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/witwave-ai/witself/internal/runtimeconfig"
	"golang.org/x/sys/windows"
)

// A witness held by a handle without read sharing is never read as
// incomplete. checkpointComplete returns errCheckpointWitnessUnread. Inspect
// and Checkpoint never reach the listing: the layout check of recovery/ opens
// the witness itself (the path guard asks FILE_TRAVERSE, which is FILE_EXECUTE
// on a file and takes part in sharing), the holder refuses that open as a
// sharing violation, and both refuse with descendant_refused, as a mode that
// refuses the open is refused on Linux and macOS. Nothing is written or run;
// once the handle is closed a Checkpoint succeeds. One subtest per expectation.
func TestCheckpointWitnessSharing(t *testing.T) {
	f, r := checkpointFixture(t, "legacy", false)
	c := f.cell
	if c.Checkpoint(t.Context(), checkpointOptions(f)) != nil {
		t.Fatal("cannot make the checkpoint whose witness is held")
	}
	id := checkpointIDs(t, c)[0]
	dir, before, controls := c.path("recovery", id), checkpointTreeState(t, c), r.calls["control"]
	witness := filepath.Join(dir, checkpointWitnessName)
	pointer, err := windows.UTF16PtrFromString(witness)
	if err != nil {
		t.Fatal("cannot prepare the witness sharing refusal")
	}
	held, err := windows.CreateFile(pointer, windows.GENERIC_READ, windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatal("cannot hold the witness without read sharing")
	}
	release := func() {
		if held != windows.InvalidHandle {
			_ = windows.CloseHandle(held)
			held = windows.InvalidHandle
		}
	}
	t.Cleanup(release)
	t.Run("unread", func(t *testing.T) {
		_, complete, err := checkpointComplete(dir, id)
		if complete {
			t.Fatal("a witness held without read sharing was read as complete")
		}
		if err == nil {
			t.Fatal("a witness held without read sharing was read as incomplete")
		}
		if !errors.Is(err, errCheckpointWitnessUnread) {
			t.Fatal("a witness held without read sharing failed with another error")
		}
	})
	t.Run("layout", func(t *testing.T) {
		switch runtimeconfig.WindowsPrivateFailureClass(validatePrivate(witness, false)) {
		case "sharing_violation":
		case "none":
			t.Fatal("the private check passed a witness held without read sharing")
		case "access_denied":
			t.Fatal("the private check refused a held witness as access denied")
		default:
			t.Fatal("the private check refused a held witness by its validation")
		}
	})
	t.Run("inspect", func(t *testing.T) {
		_, err := c.Inspect(t.Context())
		checkpointWitnessHeld(t, err)
	})
	t.Run("create", func(t *testing.T) { checkpointWitnessHeld(t, c.Checkpoint(t.Context(), checkpointOptions(f))) })
	release()
	t.Run("unchanged", func(t *testing.T) {
		checkpointUnchanged(t, c, before)
		if r.calls["control"] != controls {
			t.Fatal("a refusal beside a held witness ran the control")
		}
	})
	t.Run("released", func(t *testing.T) {
		if c.Checkpoint(t.Context(), checkpointOptions(f)) != nil {
			t.Fatal("the released witness did not let a checkpoint be made")
		}
		if len(checkpointIDs(t, c)) != 2 {
			t.Fatal("the released witness did not leave two checkpoints")
		}
	})
}

// checkpointWitnessHeld requires the layout check's refusal of a call beside
// the held witness and names any other result by one fixed text per class.
func checkpointWitnessHeld(t *testing.T, err error) {
	t.Helper()
	switch ClassifyCheckpointFailure(err) {
	case CheckpointFailure{Class: "local_checkpoint_state_unsupported", Reason: "descendant_refused"}:
	case CheckpointFailure{}:
		t.Fatal("beside a held witness the call succeeded")
	case CheckpointFailure{Class: "local_checkpoint_operation_failed"}:
		t.Fatal("beside a held witness the call failed with operation failed and no reason")
	case CheckpointFailure{Class: "local_checkpoint_state_unsupported", Reason: "tree_unreadable"}:
		t.Fatal("beside a held witness the call was refused as tree unreadable")
	case CheckpointFailure{Class: "local_checkpoint_state_unsupported"}:
		t.Fatal("beside a held witness the call was refused as state unsupported with no reason")
	case CheckpointFailure{Class: "local_checkpoint_incomplete"}:
		t.Fatal("beside a held witness the call was refused as incomplete")
	case CheckpointFailure{Class: "local_cell_layout_refused"}:
		t.Fatal("beside a held witness the call was refused by the cell layout check")
	default:
		t.Fatal("beside a held witness the call returned a class outside the known list")
	}
}
