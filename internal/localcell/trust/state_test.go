package trust

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"testing"
)

func verifierStateDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal("private fixture directory creation failed")
	}
	return dir
}

func verifierWindowsStateRefusal(t *testing.T) bool {
	t.Helper()
	if runtime.GOOS != "windows" {
		return false
	}
	if state, err := NewFileState(filepath.Join(t.TempDir(), "private")); state != nil || !errors.Is(err, ReasonStateIO) {
		t.Fatal("Windows private ACL capability must fail closed")
	}
	return true
}

func TestMemoryStateTransactionsIsolateAndSerialize(t *testing.T) {
	state := NewMemoryState()
	const writers = 12
	var wg sync.WaitGroup
	errCh := make(chan error, writers)
	for range writers {
		wg.Go(func() { errCh <- state.Update(func(s *StateSnapshot) error { s.RootSequence++; return nil }) })
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal("memory transaction failed")
		}
	}
	loaded, err := state.Load()
	if err != nil || loaded.RootSequence != writers {
		t.Fatal("lost state transaction")
	}
	loaded.Releases["stable"] = ReleaseRecord{Sequence: 99}
	loaded, err = state.Load()
	if err != nil || len(loaded.Releases) != 0 {
		t.Fatal("Load exposed mutable state")
	}
	if err := state.Update(func(s *StateSnapshot) error { s.RootSequence = 999; return ReasonReplay }); !errors.Is(err, ReasonReplay) {
		t.Fatal("transaction error was not retained")
	}
	loaded, err = state.Load()
	if err != nil || loaded.RootSequence != writers {
		t.Fatal("failed transaction committed")
	}
}

func TestFileStateAtomicCrashPreservesPreviousState(t *testing.T) {
	if verifierWindowsStateRefusal(t) {
		return
	}
	dir := verifierStateDir(t)
	state, err := NewFileState(dir)
	if err != nil {
		t.Fatal("file state construction failed")
	}
	if err := state.Update(func(s *StateSnapshot) error { s.RootSequence = 1; return nil }); err != nil {
		t.Fatal("initial state write failed")
	}
	before, err := os.ReadFile(filepath.Join(dir, "trust-state.json"))
	if err != nil {
		t.Fatal("state read failed")
	}
	state.beforeRename = func() error { return errors.New("simulated interruption") }
	if err := state.Update(func(s *StateSnapshot) error { s.RootSequence = 2; return nil }); !errors.Is(err, ReasonStateIO) {
		t.Fatal("interruption did not fail closed")
	}
	after, err := os.ReadFile(filepath.Join(dir, "trust-state.json"))
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("interrupted write changed committed bytes")
	}
	reopened, err := NewFileState(dir)
	if err != nil {
		t.Fatal("reopening state failed")
	}
	loaded, err := reopened.Load()
	if err != nil || loaded.RootSequence != 1 {
		t.Fatal("reopened state lost prior generation")
	}
	info, err := os.Stat(filepath.Join(dir, "trust-state.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("state file is not private")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "trust-state.json" {
		t.Fatal("interrupted write left ordinary temp artifacts")
	}
}

func TestFileStateCrossAdapterWriterFence(t *testing.T) {
	if verifierWindowsStateRefusal(t) {
		return
	}
	dir := verifierStateDir(t)
	first, err := NewFileState(dir)
	if err != nil {
		t.Fatal("first file state construction failed")
	}
	second, err := NewFileState(dir)
	if err != nil {
		t.Fatal("second file state construction failed")
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		finished <- first.Update(func(s *StateSnapshot) error { close(entered); <-release; s.RootSequence = 1; return nil })
	}()
	<-entered
	err = second.Update(func(s *StateSnapshot) error { s.RootSequence = 2; return nil })
	close(release)
	if !errors.Is(err, ReasonStateIO) {
		t.Fatal("concurrent writer bypassed fence")
	}
	if err := <-finished; err != nil {
		t.Fatal("first writer failed")
	}
	loaded, err := second.Load()
	if err != nil || loaded.RootSequence != 1 {
		t.Fatal("concurrent writer replaced committed state")
	}
}

func TestFileStateRejectsUnsafeStorage(t *testing.T) {
	if verifierWindowsStateRefusal(t) {
		return
	}
	t.Run("public directory", func(t *testing.T) {
		dir := verifierStateDir(t)
		if err := os.Chmod(dir, 0755); err != nil {
			t.Fatal("chmod fixture failed")
		}
		if _, err := NewFileState(dir); !errors.Is(err, ReasonStateIO) {
			t.Fatal("accepted public state directory")
		}
	})
	t.Run("symlink directory", func(t *testing.T) {
		dir := verifierStateDir(t)
		link := filepath.Join(t.TempDir(), "state")
		if err := os.Symlink(dir, link); err != nil {
			t.Fatal("symlink fixture failed")
		}
		if _, err := NewFileState(link); !errors.Is(err, ReasonStateIO) {
			t.Fatal("accepted symlink state directory")
		}
	})
	t.Run("symlink state", func(t *testing.T) {
		dir := verifierStateDir(t)
		target := filepath.Join(t.TempDir(), "other")
		if err := os.WriteFile(target, []byte("{}"), 0600); err != nil {
			t.Fatal("fixture write failed")
		}
		if err := os.Symlink(target, filepath.Join(dir, "trust-state.json")); err != nil {
			t.Fatal("symlink fixture failed")
		}
		state, err := NewFileState(dir)
		if err != nil {
			t.Fatal("state construction failed")
		}
		if _, err := state.Load(); !errors.Is(err, ReasonStateIO) {
			t.Fatal("accepted symlink state file")
		}
	})
	t.Run("malformed state", func(t *testing.T) {
		dir := verifierStateDir(t)
		if err := os.WriteFile(filepath.Join(dir, "trust-state.json"), []byte("{}"), 0600); err != nil {
			t.Fatal("fixture write failed")
		}
		state, err := NewFileState(dir)
		if err != nil {
			t.Fatal("state construction failed")
		}
		if _, err := state.Load(); !errors.Is(err, ReasonStateInvalid) {
			t.Fatal("accepted malformed persisted state")
		}
	})
	t.Run("abandoned writer fence", func(t *testing.T) {
		dir := verifierStateDir(t)
		if err := os.WriteFile(filepath.Join(dir, ".trust-state.lock"), nil, 0600); err != nil {
			t.Fatal("fixture write failed")
		}
		state, err := NewFileState(dir)
		if err != nil {
			t.Fatal("state construction failed")
		}
		if err := state.Update(func(*StateSnapshot) error { t.Fatal("entered transaction past abandoned fence"); return nil }); !errors.Is(err, ReasonStateIO) {
			t.Fatal("guessed stale fence ownership")
		}
	})
}

func TestVerifierFileStatePersistsAuthorityAndInstallation(t *testing.T) {
	if verifierWindowsStateRefusal(t) {
		return
	}
	root, public := verifierFixtureKey(t, "root-a")
	release, releasePublic := verifierFixtureKey(t, "release-1")
	dir := verifierStateDir(t)
	state, err := NewFileState(dir)
	if err != nil {
		t.Fatal("state construction failed")
	}
	v := NewVerifier(verifierOptions(public, state))
	verifierDelegate(t, v, root, releasePublic, nil)
	raw, envelope := verifierSign(t, verifierFixtureSet(), "component_set", release)
	set, err := ParseComponentSet(raw)
	if err != nil {
		t.Fatal("fixture parse failed")
	}
	verifierAssertReason(t, v.AcceptSet(raw, envelope, set.Platforms[0].Components), ReasonAccepted)
	reopened, err := NewFileState(dir)
	if err != nil {
		t.Fatal("state reopening failed")
	}
	restarted := NewVerifier(verifierOptions(public, reopened))
	verifierAssertReason(t, restarted.AcceptForAcquisition(raw, envelope), ReasonAccepted)
	verifierAssertReason(t, restarted.AcceptForOfflineStart(raw, envelope), ReasonAccepted)
}

func TestFileStateRevalidatesPrivateDirectory(t *testing.T) {
	if verifierWindowsStateRefusal(t) {
		return
	}
	for _, change := range []string{"chmod", "symlink"} {
		t.Run(change, func(t *testing.T) {
			dir := verifierStateDir(t)
			state, err := NewFileState(dir)
			if err != nil {
				t.Fatal("state construction failed")
			}
			if change == "chmod" {
				if err := os.Chmod(dir, 0755); err != nil {
					t.Fatal("fixture chmod failed")
				}
			} else {
				if err := os.Rename(dir, dir+"-old"); err != nil {
					t.Fatal("fixture rename failed")
				}
				if err := os.Symlink(dir+"-old", dir); err != nil {
					t.Fatal("fixture symlink failed")
				}
			}
			if _, err := state.Load(); !errors.Is(err, ReasonStateIO) {
				t.Fatal("Load accepted changed private directory")
			}
			if err := state.Update(func(*StateSnapshot) error { t.Fatal("Update entered changed directory"); return nil }); !errors.Is(err, ReasonStateIO) {
				t.Fatal("Update accepted changed private directory")
			}
		})
	}
}
