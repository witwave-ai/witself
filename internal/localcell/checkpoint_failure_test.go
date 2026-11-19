package localcell

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type checkpointFailureUnreadableText struct{}

func (checkpointFailureUnreadableText) Error() string { panic("classifier read error text") }

func TestCheckpointFailureClassification(t *testing.T) {
	rows := []struct {
		name, class, text string
		err               error
	}{
		{"state", "local_checkpoint_state_unsupported", "local_checkpoint_state_unsupported", errCheckpointState},
		{"identity", "local_checkpoint_identity_mismatch", "local_checkpoint_identity_mismatch", errCheckpointIdentity},
		{"source", "local_checkpoint_source_unsupported", "local_checkpoint_source_unsupported", errCheckpointSource},
		{"storage", "local_checkpoint_storage_insufficient", "local_checkpoint_storage_insufficient", errCheckpointStorage},
		{"limit", "local_checkpoint_limit_reached", "local_checkpoint_limit_reached", errCheckpointLimit},
		{"incomplete", "local_checkpoint_incomplete", "local_checkpoint_incomplete", errCheckpointIncomplete},
		{"witness", "local_checkpoint_witness_mismatch", "local_checkpoint_witness_mismatch", errCheckpointWitness},
		{"copy", "local_checkpoint_copy_failed", "local_checkpoint_copy_failed", errCheckpointCopy},
		{"maintenance", "local_maintenance_fence_active", "local_maintenance_fence_active", errMaintenanceFenceActive},
		{"locked", "local_cell_locked", "localcell: another operation owns this cell", ErrLocked},
		{"canceled", "local_checkpoint_interrupted", "context canceled", context.Canceled},
		{"deadline", "local_checkpoint_interrupted", "context deadline exceeded", context.DeadlineExceeded},
		{"layout", "local_cell_layout_refused", "cannot inspect private local cell layout", errPrivateLayout},
		{"directory", "local_cell_layout_refused", "local cell directory is missing or not a real directory", errLocalDirectory},
		{"private", "local_cell_layout_refused", "local cell directory must be private and owned by the current user", errLocalDirectoryPrivate},
		{"parent", "local_cell_layout_refused", "local cell parent directory is unavailable", errLocalParentUnavailable},
		{"alias", "local_cell_layout_refused", "local cell parent directory identity changed", errLocalParentChanged},
		{"fallback", "local_checkpoint_operation_failed", "cannot allocate local cell checkpoint identity", errCheckpointAllocation},
	}
	if len(checkpointFailureClasses) != len(rows)-1 {
		t.Fatal("classification table lacks a test row")
	}
	for _, class := range checkpointFailureClasses {
		found := false
		for _, row := range rows {
			found = found || class.cause == row.err && class.class == row.class
		}
		if !found {
			t.Fatal("classification table lacks a test row")
		}
	}
	for _, group := range []string{"known", "wrapped"} {
		t.Run(group, func(t *testing.T) {
			if got := ClassifyCheckpointFailure(nil); got != (CheckpointFailure{}) {
				t.Fatal("nil failure changed")
			}
			for _, row := range rows {
				t.Run(row.name, func(t *testing.T) {
					err := row.err
					if group == "wrapped" {
						err = fmt.Errorf("%w", err)
					}
					if !errors.Is(err, row.err) || err.Error() != row.text || ClassifyCheckpointFailure(err) != (CheckpointFailure{Class: row.class}) {
						t.Fatal("fixed cause identity text or classification changed")
					}
				})
			}
		})
	}
	t.Run("unknown", func(t *testing.T) {
		if ClassifyCheckpointFailure(checkpointFailureUnreadableText{}) != (CheckpointFailure{Class: "local_checkpoint_operation_failed"}) ||
			ClassifyCheckpointFailure(checkpointStateError{reason: "untrusted"}) != (CheckpointFailure{Class: "local_checkpoint_state_unsupported"}) {
			t.Fatal("classifier exposed an unknown value")
		}
		for _, row := range rows {
			if got := ClassifyCheckpointFailure(errors.New(row.text)); got != (CheckpointFailure{Class: "local_checkpoint_operation_failed"}) {
				t.Fatal("classifier matched untrusted error text")
			}
		}
	})
	t.Run("cleanup", func(t *testing.T) {
		cause := checkpointStateError{reason: "descendant_refused"}
		err := fmt.Errorf("%w", checkpointCleanupError{cause: cause})
		want := CheckpointFailure{Class: "local_checkpoint_state_unsupported", Reason: "descendant_refused", CleanupIncomplete: true}
		if !errors.Is(err, errCheckpointState) || err.Error() != "local cell checkpoint failed; cleanup incomplete" || ClassifyCheckpointFailure(err) != want {
			t.Fatal("cleanup lost its underlying refusal or flag")
		}
	})
}

func TestCheckpointSafeMetadata(t *testing.T) {
	valid := CheckpointSummary{ID: "ckpt_aaaaaaaaaaaaaaaa", InstallationID: "lc_test", CreatedAt: time.Now().UTC(), Complete: true}
	t.Run("identifier", func(t *testing.T) {
		for _, value := range []string{"A", strings.Repeat("Z", 64), "A_z-09"} {
			summary := valid
			summary.InstallationID = value
			if !summary.Printable() {
				t.Fatal("safe installation identity refused")
			}
		}
		for _, value := range []string{"", "ckpt_AAAAAAAAAAAAAAAA", "ckpt_aaaaaaaaaaaaaaa", "ckpt_aaaaaaaaaaaaaaaaa"} {
			summary := valid
			summary.ID = value
			if summary.Printable() {
				t.Fatal("unsafe checkpoint identity accepted")
			}
		}
		if !(CheckpointSummary{ID: valid.ID}).Printable() {
			t.Fatal("incomplete summary refused")
		}
		summary := valid
		summary.InstallationID = ""
		if summary.Printable() {
			t.Fatal("complete summary without installation accepted")
		}
	})
	t.Run("bad-time", func(t *testing.T) {
		for _, year := range []int{0, 10000} {
			summary := valid
			summary.CreatedAt = time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC)
			if summary.Printable() {
				t.Fatal("out of range time accepted")
			}
		}
	})
	for _, row := range []struct {
		name string
		edit func(*checkpointWitness)
	}{
		{"identifier-overlong", func(w *checkpointWitness) { w.InstallationID = strings.Repeat("z", 65) }},
		{"identifier-control", func(w *checkpointWitness) { w.InstallationID = "lc_\x01test" }},
		{"negative-bytes", func(w *checkpointWitness) { w.Bytes = -1 }},
		{"bad-time-zero", func(w *checkpointWitness) { w.CreatedAt = time.Time{} }},
	} {
		t.Run(row.name, func(t *testing.T) {
			f, _ := checkpointFixture(t, "legacy", false)
			c := f.cell
			dir := checkpointPlant(t, c, valid.ID, valid.ID)
			witness, complete, err := checkpointComplete(context.Background(), dir, valid.ID)
			if err != nil || !complete {
				t.Fatal("initial witness incomplete")
			}
			row.edit(&witness)
			if writeAtomic(filepath.Join(dir, checkpointWitnessName), witness, nil) != nil {
				t.Fatal("cannot write unsafe witness")
			}
			if got, complete, err := checkpointComplete(context.Background(), dir, valid.ID); err != nil || !complete || got != witness {
				t.Fatal("safe metadata changed completeness")
			}
			summary := checkpointSummaryOf(t, c, valid.ID)
			if !summary.Complete || summary.Printable() || summary.InstallationID != witness.InstallationID || summary.Bytes != witness.Bytes || summary.CreatedAt != witness.CreatedAt {
				t.Fatal("safe metadata predicate changed witness summary")
			}
			if c.Checkpoint(t.Context(), checkpointOptions(f)) != nil {
				t.Fatal("safe metadata changed checkpoint admission")
			}
			if !errors.Is(c.Checkpoint(t.Context(), checkpointOptions(f)), errCheckpointLimit) {
				t.Fatal("safe metadata changed checkpoint limit")
			}
			if c.DeleteCheckpoint(t.Context(), DeleteCheckpointOptions{CheckpointID: valid.ID}) != nil {
				t.Fatal("safe metadata changed exact checkpoint deletion")
			}
		})
	}
}
