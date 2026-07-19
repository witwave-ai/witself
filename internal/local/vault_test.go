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

func newTestAgentVaultKey(t *testing.T) *sealed.AgentVaultKey {
	t.Helper()
	key, err := sealed.GenerateAgentVaultKey(sealed.InitialAgentVaultKeyVersion)
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
