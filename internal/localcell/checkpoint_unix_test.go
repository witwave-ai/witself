//go:build linux || darwin

package localcell

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestCheckpointSourceOpenRefusesLinks(t *testing.T) {
	// The walk-type barrier: a PGDATA link to a file or directory refuses.
	for _, link := range []string{"pg/base/1/link", "pg/linked"} {
		t.Run(filepath.Base(link), func(t *testing.T) {
			f, r := checkpointFixture(t, "legacy", false)
			if os.Symlink(map[string]string{"pg/base/1/link": "1259", "pg/linked": "base"}[link], f.cell.path(filepath.FromSlash(link))) != nil {
				t.Fatal("cannot prepare a PGDATA link")
			}
			before := checkpointTreeState(t, f.cell)
			err := f.cell.Checkpoint(t.Context(), checkpointOptions(f))
			if checkpointUnchanged(t, f.cell, before); !checkpointSameError(err, "local_checkpoint_source_unsupported") || r.calls["control"] != 0 {
				t.Fatal("a PGDATA link was admitted to the copy plan")
			}
		})
	}
	// The opener: os.Root refuses a final or intermediate link out of pg/.
	t.Run("opener-escapes", func(t *testing.T) {
		f, _ := checkpointFixture(t, "legacy", false)
		outside := testPrivateRoot(t)
		if os.WriteFile(filepath.Join(outside, "file"), []byte("outside bytes"), 0o600) != nil ||
			os.Symlink(filepath.Join(outside, "file"), f.cell.path("pg", "escape")) != nil || os.Symlink(outside, f.cell.path("pg", "outside")) != nil {
			t.Fatal("cannot prepare escaping links")
		}
		open, release, err := f.cell.openCheckpointSource()
		if err != nil {
			t.Fatal("cannot open the PGDATA source")
		}
		defer release()
		for _, rel := range []string{"escape", "outside/file"} {
			if file, err := open(rel); err == nil {
				_ = file.Close()
				t.Fatal("the source opener followed a link out of pg")
			}
		}
		if file, err := open("base/1/1259"); err != nil || file.Close() != nil {
			t.Fatal("the source opener refused an ordinary PGDATA file")
		}
	})
	// A recovery link or a non-private recovery fails own() with E0's text.
	for _, row := range []struct{ name, want string }{
		{"recovery-link", "local cell directory is missing or not a real directory"},
		{"recovery-0755", "local cell directory must be private and owned by the current user"},
	} {
		t.Run(row.name, func(t *testing.T) {
			f, r := checkpointFixture(t, "legacy", false)
			c := f.cell
			if row.name == "recovery-link" && os.Symlink(testPrivateRoot(t), c.recoveryDir()) != nil ||
				row.name != "recovery-link" && (privateDir(c.recoveryDir(), true) != nil || os.Chmod(c.recoveryDir(), 0o755) != nil) {
				t.Fatal("cannot prepare the recovery boundary")
			}
			before := checkpointTreeState(t, c)
			err := c.Checkpoint(t.Context(), checkpointOptions(f))
			if checkpointUnchanged(t, c, before); err == nil || err.Error() != row.want || r.calls["control"] != 0 {
				t.Fatal("ownership did not refuse the recovery boundary with its exact text")
			}
		})
	}
}

func TestCheckpointRecoveryValidatorUnix(t *testing.T) {
	target := filepath.Join(testPrivateRoot(t), "target")
	if os.WriteFile(target, []byte("private target"), 0o600) != nil {
		t.Fatal("cannot prepare a link target")
	}
	for _, row := range []struct {
		name  string
		build func(string) error
	}{
		{"file-0644-deep", func(dir string) error { return os.Chmod(filepath.Join(dir, "pg", "base", "1", "1259"), 0o644) }},
		{"directory-0755", func(dir string) error { return os.Chmod(filepath.Join(dir, "pg"), 0o755) }},
		{"symlink-to-private-file", func(dir string) error { return os.Symlink(target, filepath.Join(dir, "pg", "link")) }},
		{"fifo", func(dir string) error { return syscall.Mkfifo(filepath.Join(dir, "pg", "fifo"), 0o600) }},
	} {
		t.Run(row.name, func(t *testing.T) {
			f, _ := checkpointFixture(t, "legacy", false)
			dir := checkpointPlant(t, f.cell, "ckpt_aaaaaaaaaaaaaaaa", "ckpt_aaaaaaaaaaaaaaaa", "pg/base/1/1259")
			if row.build(dir) != nil {
				t.Fatal("cannot prepare the non-private object")
			}
			// A validator that opened the FIFO would block until a writer opens it.
			result := StartJoinedTestRunForTest(t, func() {
				if writer, err := os.OpenFile(filepath.Join(dir, "pg", "fifo"), os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
					_ = writer.Close()
				}
			}, reinstallContentionBound, f.cell.validateRecovery)
			select {
			case err := <-result:
				if !checkpointSameError(err, "local_checkpoint_state_unsupported") {
					t.Fatal("the recovery validator admitted a non-private object")
				}
			case <-time.After(reinstallContentionBound):
				t.Fatal("the recovery validator blocked on an object")
			}
			checkpointRefusedEverywhere(t, f)
			if info, err := os.Lstat(target); err != nil || info.Mode() != 0o600 || info.Size() != int64(len("private target")) {
				t.Fatal("the validator changed a link target")
			}
		})
	}
}

// A failed discard (OA-2): the copy is detected corrupt, and a read-only copied
// directory keeps the witness-less leftover from being removed.
func TestCheckpointFailureCrashRepairCleanupIncomplete(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root deletes from a non-writable directory")
	}
	f, r := checkpointFixture(t, "legacy", false)
	c := f.cell
	var copied string
	r.after = func(dir string) {
		copied = filepath.Join(dir, "pg")
		checkpointRewrite(t, filepath.Join(copied, "base", "1", "1259"))
		if os.Chmod(copied, 0o500) != nil {
			t.Fatal("cannot make the copied directory read-only")
		}
		t.Cleanup(func() { _ = os.Chmod(copied, 0o700) })
	}
	err := c.Checkpoint(t.Context(), checkpointOptions(f))
	if err == nil || err.Error() != "local cell checkpoint failed; cleanup incomplete" || !errors.Is(err, errCheckpointWitness) ||
		!checkpointSameError(err, "local_checkpoint_witness_mismatch") {
		t.Fatal("an incomplete discard was not reported with the step cause")
	}
	if p, err := c.readJournal(); err != nil || p.pending != nil || p.tornTail || os.Chmod(copied, 0o700) != nil {
		t.Fatal("an incomplete discard left the operation open")
	}
	leftover := filepath.Base(filepath.Dir(copied))
	if checkpointSummaryOf(t, c, leftover).Complete || !checkpointSameError(c.Checkpoint(t.Context(), checkpointOptions(f)), "local_checkpoint_incomplete") {
		t.Fatal("the leftover is listed complete or did not refuse the next checkpoint")
	}
	if c.DeleteCheckpoint(t.Context(), DeleteCheckpointOptions{CheckpointID: leftover}) != nil || len(checkpointIDs(t, c)) != 0 {
		t.Fatal("the leftover was not deleted by its exact identifier")
	}
}
