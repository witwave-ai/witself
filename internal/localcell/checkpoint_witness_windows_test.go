//go:build windows

package localcell

import (
	"errors"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// A witness held by a handle without read sharing is never read as
// incomplete: checkpointComplete returns errCheckpointWitnessUnread, Inspect
// and Checkpoint fail with the operation-failed class and no reason, before
// any control run, the tree unchanged; once the handle is closed a Checkpoint
// succeeds.
func TestCheckpointWitnessSharing(t *testing.T) {
	f, r := checkpointFixture(t, "legacy", false)
	c := f.cell
	if c.Checkpoint(t.Context(), checkpointOptions(f)) != nil {
		t.Fatal("cannot make the checkpoint whose witness is held")
	}
	id := checkpointIDs(t, c)[0]
	dir, before, controls := c.path("recovery", id), checkpointTreeState(t, c), r.calls["control"]
	pointer, err := windows.UTF16PtrFromString(filepath.Join(dir, checkpointWitnessName))
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
	_, complete, read := checkpointComplete(dir, id)
	_, inspected := c.Inspect(t.Context())
	created := c.Checkpoint(t.Context(), checkpointOptions(f))
	release()
	want := CheckpointFailure{Class: "local_checkpoint_operation_failed"}
	if complete || !errors.Is(read, errCheckpointWitnessUnread) || ClassifyCheckpointFailure(inspected) != want || ClassifyCheckpointFailure(created) != want {
		t.Fatal("a witness held without read sharing was read as incomplete or refused with another class")
	}
	if checkpointUnchanged(t, c, before); r.calls["control"] != controls {
		t.Fatal("a refusal beside a held witness ran the control")
	}
	if c.Checkpoint(t.Context(), checkpointOptions(f)) != nil || len(checkpointIDs(t, c)) != 2 {
		t.Fatal("the released witness did not let a checkpoint be made")
	}
}
