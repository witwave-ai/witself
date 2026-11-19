//go:build linux || darwin

package localcell

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A witness whose mode refuses the open (chmod 0) is never read as incomplete:
// checkpointComplete returns errCheckpointWitnessUnread. An effective root user
// opens it anyway: complete, asserted instead of skipped. Inspect still
// refuses the mode first at the layout check, before any read.
func TestCheckpointWitnessUnopenable(t *testing.T) {
	f, _ := checkpointFixture(t, "legacy", false)
	c := f.cell
	if c.Checkpoint(t.Context(), checkpointOptions(f)) != nil {
		t.Fatal("cannot make the checkpoint whose witness is unopenable")
	}
	id := checkpointIDs(t, c)[0]
	dir := c.path("recovery", id)
	name := filepath.Join(dir, checkpointWitnessName)
	if os.Chmod(name, 0) != nil {
		t.Fatal("cannot remove the witness permissions")
	}
	t.Cleanup(func() { _ = os.Chmod(name, 0o600) })
	_, complete, err := checkpointComplete(context.Background(), dir, id)
	if root := os.Geteuid() == 0; root && (!complete || err != nil) || !root && (complete || !errors.Is(err, errCheckpointWitnessUnread)) {
		t.Fatal("an unopenable witness was read as incomplete, or an effective root user's open as unread")
	}
	if _, err := c.Inspect(t.Context()); ClassifyCheckpointFailure(err) != (CheckpointFailure{Class: "local_checkpoint_state_unsupported", Reason: "descendant_refused"}) {
		t.Fatal("the layout check no longer refuses an unopenable witness before any read")
	}
}
