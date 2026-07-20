package local

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/witwave-ai/witself/internal/sealed"
)

func TestAgentVaultKeyPathUsesWitselfHomeAndValidatesScope(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITSELF_HOME", home)

	got, err := AgentVaultKeyPath("default", "work-realm", "scott")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "keys", "accounts", "default", "realms", "work-realm", "agents", "scott.key")
	if got != want {
		t.Fatalf("AgentVaultKeyPath() = %q, want %q", got, want)
	}

	for _, scope := range [][3]string{
		{"../other", "work-realm", "scott"},
		{"default", "../other", "scott"},
		{"default", "work-realm", "../other"},
		{"default", "work_realm", "scott"},
		{"default", "work-realm", ""},
	} {
		if _, err := AgentVaultKeyPath(scope[0], scope[1], scope[2]); !errors.Is(err, ErrAgentVaultKeyScope) {
			t.Fatalf("AgentVaultKeyPath(%q, %q, %q) error = %v, want ErrAgentVaultKeyScope", scope[0], scope[1], scope[2], err)
		}
	}
}

func TestAgentVaultKeyEpochPathUsesImmutableVersionAndID(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITSELF_HOME", home)
	key := newTestAgentVaultKeyVersion(t, 12)

	got, err := AgentVaultKeyEpochPath("default", "work-realm", "scott", key.ID(), key.Version())
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "keys", "accounts", "default", "realms", "work-realm",
		"agents", "scott", "epochs", "12-"+key.ID()+".key")
	if got != want {
		t.Fatalf("AgentVaultKeyEpochPath() = %q, want %q", got, want)
	}

	for _, test := range []struct {
		name    string
		keyID   string
		version uint64
	}{
		{name: "zero version", keyID: key.ID(), version: 0},
		{name: "missing prefix", keyID: strings.TrimPrefix(key.ID(), "avk_"), version: 1},
		{name: "traversal", keyID: "avk_../../../../evil", version: 1},
		{name: "uppercase", keyID: "avk_ABCDEFGHIJKLMNOP", version: 1},
		{name: "bad alphabet", keyID: "avk_1111111111111111", version: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := AgentVaultKeyEpochPath("default", "default", "scott", test.keyID, test.version); !errors.Is(err, ErrAgentVaultKeyScope) {
				t.Fatalf("error = %v, want ErrAgentVaultKeyScope", err)
			}
		})
	}
}

func TestCreateAndReadAgentVaultKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITSELF_HOME", home)
	key := newTestAgentVaultKey(t)

	if err := CreateAgentVaultKey("default", "default", "scott", key); err != nil {
		t.Fatal(err)
	}
	path, err := AgentVaultKeyPath("default", "default", "scott")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode = %v, want regular 0600", info.Mode())
	}
	assertAgentVaultKeyDirectoriesPrivate(t, home, filepath.Dir(path))

	got, err := ReadAgentVaultKey("default", "default", "scott")
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata() != key.Metadata() {
		t.Fatalf("read metadata = %+v, want %+v", got.Metadata(), key.Metadata())
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	wantRaw, err := sealed.EncodeAgentVaultKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, wantRaw) {
		t.Fatal("persisted key record is not the canonical sealed encoding")
	}
}

func TestCreateAndReadAgentVaultKeyEpoch(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITSELF_HOME", home)
	key := newTestAgentVaultKeyVersion(t, 2)

	if err := CreateAgentVaultKeyEpoch("default", "default", "scott", key); err != nil {
		t.Fatal(err)
	}
	path, err := AgentVaultKeyEpochPath("default", "default", "scott", key.ID(), key.Version())
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("epoch file mode = %v, want regular 0600", info.Mode())
	}
	assertAgentVaultKeyDirectoriesPrivate(t, home, filepath.Dir(path))

	got, err := ReadAgentVaultKeyEpoch("default", "default", "scott", key.ID(), key.Version())
	if err != nil {
		t.Fatal(err)
	}
	defer got.Clear()
	if got.Metadata() != key.Metadata() {
		t.Fatalf("read metadata = %+v, want %+v", got.Metadata(), key.Metadata())
	}
	if _, err := ReadAgentVaultKey("default", "default", "scott"); !errors.Is(err, ErrAgentVaultKeyUnavailable) {
		t.Fatalf("legacy read after epoch create = %v, want unavailable", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	wantRaw, err := sealed.EncodeAgentVaultKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, wantRaw) {
		t.Fatal("persisted epoch is not the canonical sealed encoding")
	}
}

func TestReadAgentVaultKeyEpochFallsBackThenPublishesMatchingLegacyCanonically(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITSELF_HOME", home)
	key := newTestAgentVaultKey(t)
	if err := CreateAgentVaultKey("default", "default", "scott", key); err != nil {
		t.Fatal(err)
	}
	legacyPath, _ := AgentVaultKeyPath("default", "default", "scott")
	legacyBefore, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatal(err)
	}

	got, err := ReadAgentVaultKeyEpoch("default", "default", "scott", key.ID(), key.Version())
	if err != nil {
		t.Fatal(err)
	}
	defer got.Clear()
	if got.Metadata() != key.Metadata() {
		t.Fatalf("fallback metadata = %+v, want %+v", got.Metadata(), key.Metadata())
	}
	epochPath, _ := AgentVaultKeyEpochPath("default", "default", "scott", key.ID(), key.Version())
	if _, err := os.Lstat(epochPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy fallback created an epoch file: %v", err)
	}
	if err := CreateAgentVaultKeyEpoch("default", "default", "scott", key); err != nil {
		t.Fatalf("publish exact epoch beside matching legacy: %v", err)
	}
	epochRaw, err := os.ReadFile(epochPath)
	if err != nil {
		t.Fatal(err)
	}
	legacyRaw, err := sealed.EncodeAgentVaultKey(key)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(legacyRaw)
	if !bytes.Equal(epochRaw, legacyRaw) {
		t.Fatal("canonical epoch differs from matching legacy key")
	}
	if err := CreateAgentVaultKeyEpoch("default", "default", "scott", key); !errors.Is(err, ErrAgentVaultKeyExists) {
		t.Fatalf("second exact create = %v, want ErrAgentVaultKeyExists", err)
	}
	legacyAfter, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(legacyAfter, legacyBefore) {
		t.Fatal("exact epoch operations changed the legacy key")
	}
}

func TestAgentVaultKeyEpochsCoexistWithLegacyAndEachOther(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	legacy := newTestAgentVaultKey(t)
	epochTwoA := newTestAgentVaultKeyVersion(t, 2)
	epochTwoB := newTestAgentVaultKeyVersion(t, 2)
	if err := CreateAgentVaultKey("default", "default", "scott", legacy); err != nil {
		t.Fatal(err)
	}
	for _, key := range []*sealed.AgentVaultKey{epochTwoA, epochTwoB} {
		if err := CreateAgentVaultKeyEpoch("default", "default", "scott", key); err != nil {
			t.Fatal(err)
		}
	}
	for _, key := range []*sealed.AgentVaultKey{legacy, epochTwoA, epochTwoB} {
		got, err := ReadAgentVaultKeyEpoch("default", "default", "scott", key.ID(), key.Version())
		if err != nil {
			t.Fatal(err)
		}
		if got.Metadata() != key.Metadata() {
			got.Clear()
			t.Fatalf("read metadata = %+v, want %+v", got.Metadata(), key.Metadata())
		}
		got.Clear()
	}
	missing := newTestAgentVaultKeyVersion(t, 3)
	if _, err := ReadAgentVaultKeyEpoch("default", "default", "scott", missing.ID(), missing.Version()); !errors.Is(err, ErrAgentVaultKeyUnavailable) {
		t.Fatalf("missing exact epoch error = %v, want unavailable", err)
	}
}

func TestReadAgentVaultKeyEpochNeverFallsBackPastInvalidCanonicalRecord(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITSELF_HOME", home)
	legacy := newTestAgentVaultKey(t)
	wrong := newTestAgentVaultKey(t)
	if err := CreateAgentVaultKey("default", "default", "scott", legacy); err != nil {
		t.Fatal(err)
	}
	epochPath, _ := AgentVaultKeyEpochPath("default", "default", "scott", legacy.ID(), legacy.Version())
	if err := os.MkdirAll(filepath.Dir(epochPath), 0o700); err != nil {
		t.Fatal(err)
	}
	wrongRaw, err := sealed.EncodeAgentVaultKey(wrong)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(epochPath, wrongRaw, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := ReadAgentVaultKeyEpoch("default", "default", "scott", legacy.ID(), legacy.Version()); !errors.Is(err, ErrAgentVaultKeyInvalid) {
		t.Fatalf("wrong canonical record error = %v, want invalid", err)
	}
}

func TestCreateAgentVaultKeyNeverOverwrites(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	first := newTestAgentVaultKey(t)
	second := newTestAgentVaultKey(t)
	if err := CreateAgentVaultKey("default", "default", "scott", first); err != nil {
		t.Fatal(err)
	}
	if err := CreateAgentVaultKey("default", "default", "scott", second); !errors.Is(err, ErrAgentVaultKeyExists) {
		t.Fatalf("duplicate create error = %v, want ErrAgentVaultKeyExists", err)
	} else if strings.Contains(err.Error(), second.ID()) || strings.Contains(err.Error(), second.Fingerprint()) {
		t.Fatal("duplicate error exposed key metadata")
	}
	got, err := ReadAgentVaultKey("default", "default", "scott")
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata() != first.Metadata() {
		t.Fatalf("duplicate create replaced key: got %+v, want %+v", got.Metadata(), first.Metadata())
	}
}

func TestCreateAgentVaultKeyConcurrentExclusive(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	const contenders = 24
	keys := make([]*sealed.AgentVaultKey, contenders)
	for i := range keys {
		keys[i] = newTestAgentVaultKey(t)
	}

	start := make(chan struct{})
	errs := make([]error, contenders)
	var wait sync.WaitGroup
	wait.Add(contenders)
	for i := range keys {
		go func() {
			defer wait.Done()
			<-start
			errs[i] = CreateAgentVaultKey("default", "default", "scott", keys[i])
		}()
	}
	close(start)
	wait.Wait()

	winner := -1
	for i, err := range errs {
		if err == nil {
			if winner >= 0 {
				t.Fatalf("multiple concurrent creators succeeded: %d and %d", winner, i)
			}
			winner = i
			continue
		}
		if !errors.Is(err, ErrAgentVaultKeyExists) {
			t.Fatalf("contender %d error = %v, want ErrAgentVaultKeyExists", i, err)
		}
	}
	if winner < 0 {
		t.Fatal("no concurrent creator succeeded")
	}
	got, err := ReadAgentVaultKey("default", "default", "scott")
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata() != keys[winner].Metadata() {
		t.Fatalf("stored key = %+v, want winner %+v", got.Metadata(), keys[winner].Metadata())
	}
}

func TestCreateAgentVaultKeyEpochConcurrentExclusive(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	key := newTestAgentVaultKeyVersion(t, 2)
	const contenders = 24

	start := make(chan struct{})
	errs := make([]error, contenders)
	var wait sync.WaitGroup
	wait.Add(contenders)
	for index := range errs {
		go func() {
			defer wait.Done()
			<-start
			errs[index] = CreateAgentVaultKeyEpoch("default", "default", "scott", key)
		}()
	}
	close(start)
	wait.Wait()

	succeeded := 0
	for index, err := range errs {
		if err == nil {
			succeeded++
			continue
		}
		if !errors.Is(err, ErrAgentVaultKeyExists) {
			t.Fatalf("contender %d error = %v, want ErrAgentVaultKeyExists", index, err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("successful creators = %d, want 1", succeeded)
	}
	got, err := ReadAgentVaultKeyEpoch("default", "default", "scott", key.ID(), key.Version())
	if err != nil {
		t.Fatal(err)
	}
	defer got.Clear()
	if got.Metadata() != key.Metadata() {
		t.Fatalf("stored epoch = %+v, want %+v", got.Metadata(), key.Metadata())
	}
}

func TestReadAgentVaultKeyRejectsUnsafePermissionsAndTypes(t *testing.T) {
	t.Run("file permissions", func(t *testing.T) {
		t.Setenv("WITSELF_HOME", t.TempDir())
		key := newTestAgentVaultKey(t)
		if err := CreateAgentVaultKey("default", "default", "scott", key); err != nil {
			t.Fatal(err)
		}
		path, _ := AgentVaultKeyPath("default", "default", "scott")
		if err := os.Chmod(path, 0o640); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadAgentVaultKey("default", "default", "scott"); !errors.Is(err, ErrAgentVaultKeyUnsafe) {
			t.Fatalf("read error = %v, want ErrAgentVaultKeyUnsafe", err)
		}
		if err := CreateAgentVaultKey("default", "default", "scott", newTestAgentVaultKey(t)); !errors.Is(err, ErrAgentVaultKeyUnsafe) {
			t.Fatalf("create error = %v, want ErrAgentVaultKeyUnsafe", err)
		}
	})

	t.Run("directory permissions", func(t *testing.T) {
		t.Setenv("WITSELF_HOME", t.TempDir())
		key := newTestAgentVaultKey(t)
		if err := CreateAgentVaultKey("default", "default", "scott", key); err != nil {
			t.Fatal(err)
		}
		path, _ := AgentVaultKeyPath("default", "default", "scott")
		if err := os.Chmod(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadAgentVaultKey("default", "default", "scott"); !errors.Is(err, ErrAgentVaultKeyUnsafe) {
			t.Fatalf("read error = %v, want ErrAgentVaultKeyUnsafe", err)
		}
	})

	t.Run("non-regular final path", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("WITSELF_HOME", home)
		path, _ := AgentVaultKeyPath("default", "default", "scott")
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadAgentVaultKey("default", "default", "scott"); !errors.Is(err, ErrAgentVaultKeyUnsafe) {
			t.Fatalf("read error = %v, want ErrAgentVaultKeyUnsafe", err)
		}
		if err := CreateAgentVaultKey("default", "default", "scott", newTestAgentVaultKey(t)); !errors.Is(err, ErrAgentVaultKeyUnsafe) {
			t.Fatalf("create error = %v, want ErrAgentVaultKeyUnsafe", err)
		}
	})
}

func TestCreateAgentVaultKeyRejectsSymlinkedKeyDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITSELF_HOME", home)
	target := t.TempDir()
	keys := filepath.Join(home, "keys")
	if err := os.Symlink(target, keys); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if err := CreateAgentVaultKey("default", "default", "scott", newTestAgentVaultKey(t)); !errors.Is(err, ErrAgentVaultKeyUnsafe) {
		t.Fatalf("create through symlinked directory error = %v, want ErrAgentVaultKeyUnsafe", err)
	}
	written := filepath.Join(target, "accounts", "default", "realms", "default", "agents", "scott.key")
	if _, err := os.Lstat(written); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("key file created through symlinked directory: %v", err)
	}
}

func TestAgentVaultKeyEpochRejectsUnsafePermissionsAndSymlinks(t *testing.T) {
	t.Run("file permissions", func(t *testing.T) {
		t.Setenv("WITSELF_HOME", t.TempDir())
		key := newTestAgentVaultKeyVersion(t, 2)
		if err := CreateAgentVaultKeyEpoch("default", "default", "scott", key); err != nil {
			t.Fatal(err)
		}
		path, _ := AgentVaultKeyEpochPath("default", "default", "scott", key.ID(), key.Version())
		if err := os.Chmod(path, 0o640); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadAgentVaultKeyEpoch("default", "default", "scott", key.ID(), key.Version()); !errors.Is(err, ErrAgentVaultKeyUnsafe) {
			t.Fatalf("read error = %v, want ErrAgentVaultKeyUnsafe", err)
		}
		if err := CreateAgentVaultKeyEpoch("default", "default", "scott", key); !errors.Is(err, ErrAgentVaultKeyUnsafe) {
			t.Fatalf("create error = %v, want ErrAgentVaultKeyUnsafe", err)
		}
	})

	t.Run("epochs directory permissions", func(t *testing.T) {
		t.Setenv("WITSELF_HOME", t.TempDir())
		key := newTestAgentVaultKeyVersion(t, 2)
		if err := CreateAgentVaultKeyEpoch("default", "default", "scott", key); err != nil {
			t.Fatal(err)
		}
		path, _ := AgentVaultKeyEpochPath("default", "default", "scott", key.ID(), key.Version())
		if err := os.Chmod(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadAgentVaultKeyEpoch("default", "default", "scott", key.ID(), key.Version()); !errors.Is(err, ErrAgentVaultKeyUnsafe) {
			t.Fatalf("read error = %v, want ErrAgentVaultKeyUnsafe", err)
		}
	})

	t.Run("final path symlink", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("WITSELF_HOME", home)
		key := newTestAgentVaultKeyVersion(t, 2)
		raw, err := sealed.EncodeAgentVaultKey(key)
		if err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(home, "target.key")
		if err := os.WriteFile(target, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		path, _ := AgentVaultKeyEpochPath("default", "default", "scott", key.ID(), key.Version())
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if _, err := ReadAgentVaultKeyEpoch("default", "default", "scott", key.ID(), key.Version()); !errors.Is(err, ErrAgentVaultKeyUnsafe) {
			t.Fatalf("read symlink error = %v, want unsafe", err)
		}
		if err := CreateAgentVaultKeyEpoch("default", "default", "scott", key); !errors.Is(err, ErrAgentVaultKeyUnsafe) {
			t.Fatalf("create over symlink error = %v, want unsafe", err)
		}
		got, err := os.ReadFile(target)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, raw) {
			t.Fatal("epoch symlink target changed")
		}
	})

	t.Run("epochs directory symlink", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("WITSELF_HOME", home)
		key := newTestAgentVaultKeyVersion(t, 2)
		path, _ := AgentVaultKeyEpochPath("default", "default", "scott", key.ID(), key.Version())
		epochs := filepath.Dir(path)
		if err := os.MkdirAll(filepath.Dir(epochs), 0o700); err != nil {
			t.Fatal(err)
		}
		target := t.TempDir()
		if err := os.Symlink(target, epochs); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if err := CreateAgentVaultKeyEpoch("default", "default", "scott", key); !errors.Is(err, ErrAgentVaultKeyUnsafe) {
			t.Fatalf("create through symlinked epochs directory = %v, want unsafe", err)
		}
		if _, err := os.Lstat(filepath.Join(target, filepath.Base(path))); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("epoch created through symlinked directory: %v", err)
		}
	})

	t.Run("unsafe legacy blocks canonical create", func(t *testing.T) {
		t.Setenv("WITSELF_HOME", t.TempDir())
		legacy := newTestAgentVaultKey(t)
		if err := CreateAgentVaultKey("default", "default", "scott", legacy); err != nil {
			t.Fatal(err)
		}
		legacyPath, _ := AgentVaultKeyPath("default", "default", "scott")
		if err := os.Chmod(legacyPath, 0o640); err != nil {
			t.Fatal(err)
		}
		key := newTestAgentVaultKeyVersion(t, 2)
		if err := CreateAgentVaultKeyEpoch("default", "default", "scott", key); !errors.Is(err, ErrAgentVaultKeyUnsafe) {
			t.Fatalf("canonical create beside unsafe legacy = %v, want unsafe", err)
		}
		path, _ := AgentVaultKeyEpochPath("default", "default", "scott", key.ID(), key.Version())
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("canonical epoch created despite unsafe legacy: %v", err)
		}
	})
}

func TestCreateAgentVaultKeyEpochAtomicallyPublishesCompleteRecord(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITSELF_HOME", home)
	key := newTestAgentVaultKeyVersion(t, 2)
	path, _ := AgentVaultKeyEpochPath("default", "default", "scott", key.ID(), key.Version())
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	// Model a process that died before publishing its temporary record. A stale
	// private temporary file is not a key epoch and cannot block or contaminate
	// publication of the canonical name.
	interrupted := filepath.Join(directory, ".avk-write-interrupted.tmp")
	if err := os.WriteFile(interrupted, []byte("partial-private-record"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CreateAgentVaultKeyEpoch("default", "default", "scott", key); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := sealed.ParseAgentVaultKey(raw)
	if err != nil {
		t.Fatalf("published canonical record is incomplete: %v", err)
	}
	defer parsed.Clear()
	if parsed.Metadata() != key.Metadata() {
		t.Fatalf("published metadata = %+v, want %+v", parsed.Metadata(), key.Metadata())
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".avk-write-") && entry.Name() != filepath.Base(interrupted) {
			t.Fatalf("successful publication left writer temporary file %q", entry.Name())
		}
	}
	leftover, err := os.ReadFile(interrupted)
	if err != nil || string(leftover) != "partial-private-record" {
		t.Fatalf("unrelated interrupted state was modified: %q / %v", leftover, err)
	}
}

func TestAgentVaultKeyFinalPathSymlinkIsNeverFollowed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITSELF_HOME", home)
	key := newTestAgentVaultKey(t)
	raw, err := sealed.EncodeAgentVaultKey(key)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(home, "target.key")
	if err := os.WriteFile(target, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	path, _ := AgentVaultKeyPath("default", "default", "scott")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if _, err := ReadAgentVaultKey("default", "default", "scott"); !errors.Is(err, ErrAgentVaultKeyUnsafe) {
		t.Fatalf("read symlink error = %v, want ErrAgentVaultKeyUnsafe", err)
	}
	if err := CreateAgentVaultKey("default", "default", "scott", newTestAgentVaultKey(t)); !errors.Is(err, ErrAgentVaultKeyUnsafe) {
		t.Fatalf("create over symlink error = %v, want ErrAgentVaultKeyUnsafe", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatal("symlink target changed")
	}
}

func TestReadAgentVaultKeyRejectsMalformedAndOversizedRecords(t *testing.T) {
	valid, err := sealed.EncodeAgentVaultKey(newTestAgentVaultKey(t))
	if err != nil {
		t.Fatal(err)
	}
	corrupt := append([]byte(nil), valid...)
	corrupt[len(corrupt)-1] ^= 1

	for name, raw := range map[string][]byte{
		"empty":     {},
		"malformed": []byte("private-key-marker-not-a-record"),
		"trailing":  append(append([]byte(nil), valid...), '\n', '\n'),
		"checksum":  corrupt,
		"oversized": bytes.Repeat([]byte("private-key-marker"), maxAgentVaultKeyFileBytes),
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("WITSELF_HOME", home)
			path, _ := AgentVaultKeyPath("default", "default", "scott")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := ReadAgentVaultKey("default", "default", "scott")
			if !errors.Is(err, ErrAgentVaultKeyInvalid) {
				t.Fatalf("read error = %v, want ErrAgentVaultKeyInvalid", err)
			}
			if strings.Contains(err.Error(), "private-key-marker") {
				t.Fatal("malformed-file error exposed record content")
			}
		})
	}
}

func TestReadAgentVaultKeyMissingIsUnavailable(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	if _, err := ReadAgentVaultKey("default", "default", "scott"); !errors.Is(err, ErrAgentVaultKeyUnavailable) {
		t.Fatalf("read missing error = %v, want ErrAgentVaultKeyUnavailable", err)
	}
}

func TestCreateAgentVaultKeyRejectsNil(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	if err := CreateAgentVaultKey("default", "default", "scott", nil); !errors.Is(err, ErrAgentVaultKeyInvalid) {
		t.Fatalf("nil create error = %v, want ErrAgentVaultKeyInvalid", err)
	}
}

func TestCreateAgentVaultKeyEpochRejectsNil(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	if err := CreateAgentVaultKeyEpoch("default", "default", "scott", nil); !errors.Is(err, ErrAgentVaultKeyInvalid) {
		t.Fatalf("nil epoch create error = %v, want ErrAgentVaultKeyInvalid", err)
	}
}

func newTestAgentVaultKey(t *testing.T) *sealed.AgentVaultKey {
	t.Helper()
	return newTestAgentVaultKeyVersion(t, sealed.InitialAgentVaultKeyVersion)
}

func newTestAgentVaultKeyVersion(t *testing.T, version uint64) *sealed.AgentVaultKey {
	t.Helper()
	key, err := sealed.GenerateAgentVaultKey(version)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func assertAgentVaultKeyDirectoriesPrivate(t *testing.T, home, directory string) {
	t.Helper()
	for _, path := range agentVaultKeyDirectories(home, directory) {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Fatalf("directory %q mode = %v, want directory 0700", path, info.Mode())
		}
	}
}
