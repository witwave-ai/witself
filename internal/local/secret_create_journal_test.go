package local

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const testSecretCreateJournalHash = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestSecretCreateJournalPathUsesCanonicalLayout(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITSELF_HOME", home)

	got, err := SecretCreateJournalPath("default", "work-realm", "scott", testSecretCreateJournalHash)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "journal", "accounts", "default", "realms", "work-realm",
		"agents", "scott", "secret-create", testSecretCreateJournalHash+".json")
	if got != want {
		t.Fatalf("SecretCreateJournalPath() = %q, want %q", got, want)
	}

	for _, scope := range [][3]string{
		{"../other", "work-realm", "scott"},
		{"default", "../other", "scott"},
		{"default", "work-realm", "../other"},
		{"default", "work_realm", "scott"},
		{"default", "work-realm", ""},
	} {
		if _, err := SecretCreateJournalPath(scope[0], scope[1], scope[2], testSecretCreateJournalHash); !errors.Is(err, ErrSecretCreateJournalScope) {
			t.Fatalf("SecretCreateJournalPath(%q, %q, %q, valid hash) error = %v, want ErrSecretCreateJournalScope",
				scope[0], scope[1], scope[2], err)
		}
	}
	for _, hash := range []string{
		"", "abc", strings.Repeat("A", 64), strings.Repeat("g", 64),
		strings.Repeat("0", 63), strings.Repeat("0", 65), "../" + strings.Repeat("0", 61),
	} {
		if _, err := SecretCreateJournalPath("default", "default", "scott", hash); !errors.Is(err, ErrSecretCreateJournalInvalid) {
			t.Fatalf("SecretCreateJournalPath(valid scope, %q) error = %v, want ErrSecretCreateJournalInvalid", hash, err)
		}
	}
}

func TestCreateAndReadSecretCreateJournalPreservesRawBytes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITSELF_HOME", home)
	raw := []byte("{\n  \"id\": \"sec_aaaaaaaaaaaaaaaa\",\n  \"fields\": []\n}\n")

	if err := CreateSecretCreateJournal("default", "default", "scott", testSecretCreateJournalHash, raw); err != nil {
		t.Fatal(err)
	}
	path, err := SecretCreateJournalPath("default", "default", "scott", testSecretCreateJournalHash)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("journal file mode = %v, want regular 0600", info.Mode())
	}
	assertSecretCreateJournalDirectoriesPrivate(t, home, filepath.Dir(path))

	got, err := ReadSecretCreateJournal("default", "default", "scott", testSecretCreateJournalHash)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatalf("journal bytes = %q, want exact bytes %q", got, raw)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(path) {
		t.Fatalf("journal directory entries = %v, want only published entry", entryNames(entries))
	}
}

func TestCreateSecretCreateJournalNeverOverwrites(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	first := []byte(`{"request":"first"}`)
	second := []byte(`{"request":"second"}`)
	if err := CreateSecretCreateJournal("default", "default", "scott", testSecretCreateJournalHash, first); err != nil {
		t.Fatal(err)
	}
	err := CreateSecretCreateJournal("default", "default", "scott", testSecretCreateJournalHash, second)
	if !errors.Is(err, ErrSecretCreateJournalExists) {
		t.Fatalf("duplicate create error = %v, want ErrSecretCreateJournalExists", err)
	}
	if strings.Contains(err.Error(), "second") || strings.Contains(err.Error(), testSecretCreateJournalHash) {
		t.Fatal("duplicate error exposed request or scoped hash")
	}
	got, err := ReadSecretCreateJournal("default", "default", "scott", testSecretCreateJournalHash)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, first) {
		t.Fatalf("duplicate create replaced original: got %q, want %q", got, first)
	}
}

func TestCreateSecretCreateJournalConcurrentExclusive(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	const contenders = 32
	requests := make([][]byte, contenders)
	for i := range requests {
		requests[i] = []byte(fmt.Sprintf(`{"contender":%d}`, i))
	}

	start := make(chan struct{})
	errs := make([]error, contenders)
	var wait sync.WaitGroup
	wait.Add(contenders)
	for i := range requests {
		go func() {
			defer wait.Done()
			<-start
			errs[i] = CreateSecretCreateJournal("default", "default", "scott", testSecretCreateJournalHash, requests[i])
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
		if !errors.Is(err, ErrSecretCreateJournalExists) {
			t.Fatalf("contender %d error = %v, want ErrSecretCreateJournalExists", i, err)
		}
	}
	if winner < 0 {
		t.Fatal("no concurrent creator succeeded")
	}
	got, err := ReadSecretCreateJournal("default", "default", "scott", testSecretCreateJournalHash)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, requests[winner]) {
		t.Fatalf("stored request = %q, want winner %q", got, requests[winner])
	}
}

func TestSecretCreateJournalRejectsInvalidRequestBytes(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	validAtLimit := append([]byte{'"'}, bytes.Repeat([]byte{'x'}, MaxSecretCreateJournalBytes-2)...)
	validAtLimit = append(validAtLimit, '"')
	for name, raw := range map[string][]byte{
		"nil":       nil,
		"empty":     {},
		"malformed": []byte(`{"secret":`),
		"oversized": append(validAtLimit, ' '),
	} {
		t.Run(name, func(t *testing.T) {
			err := CreateSecretCreateJournal("default", "default", "scott", testSecretCreateJournalHash, raw)
			if !errors.Is(err, ErrSecretCreateJournalInvalid) {
				t.Fatalf("create error = %v, want ErrSecretCreateJournalInvalid", err)
			}
		})
	}

	if err := CreateSecretCreateJournal("default", "default", "scott", testSecretCreateJournalHash, validAtLimit); err != nil {
		t.Fatalf("maximum-size valid request rejected: %v", err)
	}
	got, err := ReadSecretCreateJournal("default", "default", "scott", testSecretCreateJournalHash)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != MaxSecretCreateJournalBytes {
		t.Fatalf("maximum-size read length = %d, want %d", len(got), MaxSecretCreateJournalBytes)
	}
}

func TestReadSecretCreateJournalRejectsCorruptAndOversizedEntries(t *testing.T) {
	for name, raw := range map[string][]byte{
		"empty":     {},
		"malformed": []byte(`{"private":"request-marker"`),
		"oversized": append([]byte{'"'}, append(bytes.Repeat([]byte{'x'}, MaxSecretCreateJournalBytes), '"')...),
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("WITSELF_HOME", home)
			path, err := SecretCreateJournalPath("default", "default", "scott", testSecretCreateJournalHash)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			_, err = ReadSecretCreateJournal("default", "default", "scott", testSecretCreateJournalHash)
			if !errors.Is(err, ErrSecretCreateJournalInvalid) {
				t.Fatalf("read error = %v, want ErrSecretCreateJournalInvalid", err)
			}
			if strings.Contains(err.Error(), "request-marker") {
				t.Fatal("corrupt-entry error exposed request bytes")
			}
		})
	}
}

func TestSecretCreateJournalRejectsUnsafePermissionsAndTypes(t *testing.T) {
	t.Run("file permissions", func(t *testing.T) {
		t.Setenv("WITSELF_HOME", t.TempDir())
		raw := []byte(`{"request":"private-marker"}`)
		if err := CreateSecretCreateJournal("default", "default", "scott", testSecretCreateJournalHash, raw); err != nil {
			t.Fatal(err)
		}
		path, _ := SecretCreateJournalPath("default", "default", "scott", testSecretCreateJournalHash)
		if err := os.Chmod(path, 0o640); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadSecretCreateJournal("default", "default", "scott", testSecretCreateJournalHash); !errors.Is(err, ErrSecretCreateJournalUnsafe) {
			t.Fatalf("read error = %v, want ErrSecretCreateJournalUnsafe", err)
		}
		if err := CreateSecretCreateJournal("default", "default", "scott", testSecretCreateJournalHash, raw); !errors.Is(err, ErrSecretCreateJournalUnsafe) {
			t.Fatalf("create error = %v, want ErrSecretCreateJournalUnsafe", err)
		}
	})

	t.Run("directory permissions", func(t *testing.T) {
		t.Setenv("WITSELF_HOME", t.TempDir())
		if err := CreateSecretCreateJournal("default", "default", "scott", testSecretCreateJournalHash, []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
		path, _ := SecretCreateJournalPath("default", "default", "scott", testSecretCreateJournalHash)
		if err := os.Chmod(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadSecretCreateJournal("default", "default", "scott", testSecretCreateJournalHash); !errors.Is(err, ErrSecretCreateJournalUnsafe) {
			t.Fatalf("read error = %v, want ErrSecretCreateJournalUnsafe", err)
		}
	})

	t.Run("non-regular final path", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("WITSELF_HOME", home)
		path, _ := SecretCreateJournalPath("default", "default", "scott", testSecretCreateJournalHash)
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadSecretCreateJournal("default", "default", "scott", testSecretCreateJournalHash); !errors.Is(err, ErrSecretCreateJournalUnsafe) {
			t.Fatalf("read error = %v, want ErrSecretCreateJournalUnsafe", err)
		}
		if err := CreateSecretCreateJournal("default", "default", "scott", testSecretCreateJournalHash, []byte(`{}`)); !errors.Is(err, ErrSecretCreateJournalUnsafe) {
			t.Fatalf("create error = %v, want ErrSecretCreateJournalUnsafe", err)
		}
	})
}

func TestCreateSecretCreateJournalRejectsSymlinkedDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITSELF_HOME", home)
	target := t.TempDir()
	journal := filepath.Join(home, "journal")
	if err := os.Symlink(target, journal); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	err := CreateSecretCreateJournal("default", "default", "scott", testSecretCreateJournalHash, []byte(`{"request":"private-marker"}`))
	if !errors.Is(err, ErrSecretCreateJournalUnsafe) {
		t.Fatalf("create through symlinked directory error = %v, want ErrSecretCreateJournalUnsafe", err)
	}
	written := filepath.Join(target, "accounts", "default", "realms", "default", "agents", "scott",
		"secret-create", testSecretCreateJournalHash+".json")
	if _, err := os.Lstat(written); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal file created through symlinked directory: %v", err)
	}
}

func TestSecretCreateJournalFinalPathSymlinkIsNeverFollowed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITSELF_HOME", home)
	original := []byte(`{"request":"original-private-marker"}`)
	target := filepath.Join(home, "target.json")
	if err := os.WriteFile(target, original, 0o600); err != nil {
		t.Fatal(err)
	}
	path, _ := SecretCreateJournalPath("default", "default", "scott", testSecretCreateJournalHash)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if _, err := ReadSecretCreateJournal("default", "default", "scott", testSecretCreateJournalHash); !errors.Is(err, ErrSecretCreateJournalUnsafe) {
		t.Fatalf("read symlink error = %v, want ErrSecretCreateJournalUnsafe", err)
	}
	if err := CreateSecretCreateJournal("default", "default", "scott", testSecretCreateJournalHash, []byte(`{"request":"replacement"}`)); !errors.Is(err, ErrSecretCreateJournalUnsafe) {
		t.Fatalf("create over symlink error = %v, want ErrSecretCreateJournalUnsafe", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatal("symlink target changed")
	}
}

func TestReadSecretCreateJournalMissingIsUnavailable(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	if _, err := ReadSecretCreateJournal("default", "default", "scott", testSecretCreateJournalHash); !errors.Is(err, ErrSecretCreateJournalUnavailable) {
		t.Fatalf("read missing error = %v, want ErrSecretCreateJournalUnavailable", err)
	}
}

func assertSecretCreateJournalDirectoriesPrivate(t *testing.T, home, directory string) {
	t.Helper()
	for _, path := range secretCreateJournalDirectories(home, directory) {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Fatalf("directory %q mode = %v, want directory 0700", path, info.Mode())
		}
	}
}

func entryNames(entries []os.DirEntry) []string {
	names := make([]string, len(entries))
	for i, entry := range entries {
		names[i] = entry.Name()
	}
	return names
}
