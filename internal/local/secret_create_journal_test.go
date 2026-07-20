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

func TestReplaceSecretCreateJournalAfterVaultKeyAdvanceDurablySwapsExactBytes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITSELF_HOME", home)
	original := []byte("{\n  \"request\": \"old-wrapped-dek\"\n}\n")
	replacement := []byte("{\n  \"request\": \"new-wrapped-dek\"\n}\n")
	if err := CreateSecretCreateJournal("default", "default", "scott", testSecretCreateJournalHash, original); err != nil {
		t.Fatal(err)
	}
	path, err := SecretCreateJournalPath("default", "default", "scott", testSecretCreateJournalHash)
	if err != nil {
		t.Fatal(err)
	}
	if err := ReplaceSecretCreateJournalAfterVaultKeyAdvance(
		"default", "default", "scott", testSecretCreateJournalHash, original, replacement,
	); err != nil {
		t.Fatal(err)
	}
	got, err := ReadSecretCreateJournal("default", "default", "scott", testSecretCreateJournalHash)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, replacement) {
		t.Fatalf("replacement bytes = %q, want exact %q", got, replacement)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("replacement file mode = %v, want regular 0600", info.Mode())
	}
	lockPath := filepath.Join(filepath.Dir(path), secretCreateJournalLockFile)
	lockInfo, err := os.Lstat(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if !privateRegularSecretCreateJournalFile(lockInfo) {
		t.Fatalf("lock file mode = %v, want regular 0600", lockInfo.Mode())
	}
	assertSecretCreateJournalDirectoriesPrivate(t, home, filepath.Dir(path))
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	wantEntries := []string{secretCreateJournalLockFile, filepath.Base(path)}
	if gotEntries := entryNames(entries); fmt.Sprint(gotEntries) != fmt.Sprint(wantEntries) {
		t.Fatalf("journal directory entries = %v, want %v", gotEntries, wantEntries)
	}
}

func TestReplaceSecretCreateJournalAfterVaultKeyAdvanceRequiresExactExpectedBytes(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	original := []byte(`{"request":"original-private-marker"}`)
	replacement := []byte(`{"request":"replacement-private-marker"}`)
	if err := CreateSecretCreateJournal("default", "default", "scott", testSecretCreateJournalHash, original); err != nil {
		t.Fatal(err)
	}
	err := ReplaceSecretCreateJournalAfterVaultKeyAdvance(
		"default", "default", "scott", testSecretCreateJournalHash,
		[]byte(`{"request":"stale-private-marker"}`), replacement,
	)
	if !errors.Is(err, ErrSecretCreateJournalConflict) {
		t.Fatalf("stale replace error = %v, want ErrSecretCreateJournalConflict", err)
	}
	if strings.Contains(err.Error(), "stale-private-marker") || strings.Contains(err.Error(), testSecretCreateJournalHash) {
		t.Fatal("conflict error exposed request bytes or scoped hash")
	}
	got, readErr := ReadSecretCreateJournal("default", "default", "scott", testSecretCreateJournalHash)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("conflicting replace changed entry: got %q, want %q", got, original)
	}
}

func TestReplaceSecretCreateJournalAfterVaultKeyAdvanceSerializesContenders(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	original := []byte(`{"request":"original"}`)
	if err := CreateSecretCreateJournal("default", "default", "scott", testSecretCreateJournalHash, original); err != nil {
		t.Fatal(err)
	}
	const contenders = 32
	replacements := make([][]byte, contenders)
	errs := make([]error, contenders)
	for i := range replacements {
		replacements[i] = []byte(fmt.Sprintf(`{"replacement":%d}`, i))
	}
	start := make(chan struct{})
	var wait sync.WaitGroup
	wait.Add(contenders)
	for i := range replacements {
		go func() {
			defer wait.Done()
			<-start
			errs[i] = ReplaceSecretCreateJournalAfterVaultKeyAdvance(
				"default", "default", "scott", testSecretCreateJournalHash, original, replacements[i],
			)
		}()
	}
	close(start)
	wait.Wait()

	winner := -1
	for i, err := range errs {
		if err == nil {
			if winner >= 0 {
				t.Fatalf("multiple CAS contenders succeeded: %d and %d", winner, i)
			}
			winner = i
			continue
		}
		if !errors.Is(err, ErrSecretCreateJournalConflict) {
			t.Fatalf("contender %d error = %v, want conflict", i, err)
		}
	}
	if winner < 0 {
		t.Fatal("no CAS contender succeeded")
	}
	got, err := ReadSecretCreateJournal("default", "default", "scott", testSecretCreateJournalHash)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, replacements[winner]) {
		t.Fatalf("stored replacement = %q, want winner %q", got, replacements[winner])
	}
	path, _ := SecretCreateJournalPath("default", "default", "scott", testSecretCreateJournalHash)
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".secret-create-replace-") {
			t.Fatalf("temporary replacement survived: %q", entry.Name())
		}
	}
}

func TestReplaceSecretCreateJournalAfterVaultKeyAdvanceSupportsSuccessiveEpochs(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	first := []byte(`{"epoch":1}`)
	second := []byte(`{"epoch":2}`)
	third := []byte(`{"epoch":3}`)
	if err := CreateSecretCreateJournal("default", "default", "scott", testSecretCreateJournalHash, first); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceSecretCreateJournalAfterVaultKeyAdvance(
		"default", "default", "scott", testSecretCreateJournalHash, first, second,
	); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceSecretCreateJournalAfterVaultKeyAdvance(
		"default", "default", "scott", testSecretCreateJournalHash, first, third,
	); !errors.Is(err, ErrSecretCreateJournalConflict) {
		t.Fatalf("stale second advance error = %v, want conflict", err)
	}
	path, _ := SecretCreateJournalPath("default", "default", "scott", testSecretCreateJournalHash)
	lockBefore, err := os.Lstat(filepath.Join(filepath.Dir(path), secretCreateJournalLockFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := ReplaceSecretCreateJournalAfterVaultKeyAdvance(
		"default", "default", "scott", testSecretCreateJournalHash, second, third,
	); err != nil {
		t.Fatal(err)
	}
	lockAfter, err := os.Lstat(filepath.Join(filepath.Dir(path), secretCreateJournalLockFile))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(lockBefore, lockAfter) {
		t.Fatal("successive replacements did not use the stable lock inode")
	}
	got, err := ReadSecretCreateJournal("default", "default", "scott", testSecretCreateJournalHash)
	if err != nil || !bytes.Equal(got, third) {
		t.Fatalf("third epoch = %q / %v, want %q", got, err, third)
	}
}

func TestReplaceSecretCreateJournalAfterVaultKeyAdvanceRejectsInvalidAndMissingState(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	valid := []byte(`{"request":"valid"}`)
	for name, test := range map[string]struct {
		expected    []byte
		replacement []byte
	}{
		"invalid expected":    {[]byte(`{"request":`), valid},
		"invalid replacement": {valid, []byte(`{"request":`)},
		"empty expected":      {nil, valid},
		"empty replacement":   {valid, nil},
	} {
		t.Run(name, func(t *testing.T) {
			err := ReplaceSecretCreateJournalAfterVaultKeyAdvance(
				"default", "default", "scott", testSecretCreateJournalHash, test.expected, test.replacement,
			)
			if !errors.Is(err, ErrSecretCreateJournalInvalid) {
				t.Fatalf("replace error = %v, want invalid", err)
			}
		})
	}
	if err := ReplaceSecretCreateJournalAfterVaultKeyAdvance(
		"default", "default", "scott", testSecretCreateJournalHash, valid, []byte(`{"request":"next"}`),
	); !errors.Is(err, ErrSecretCreateJournalUnavailable) {
		t.Fatalf("missing replace error = %v, want unavailable", err)
	}
	if err := ReplaceSecretCreateJournalAfterVaultKeyAdvance(
		"../other", "default", "scott", testSecretCreateJournalHash, valid, valid,
	); !errors.Is(err, ErrSecretCreateJournalScope) {
		t.Fatalf("invalid scope error = %v, want scope", err)
	}
}

func TestReplaceSecretCreateJournalAfterVaultKeyAdvanceRejectsUnsafeState(t *testing.T) {
	t.Run("journal permissions", func(t *testing.T) {
		t.Setenv("WITSELF_HOME", t.TempDir())
		original := []byte(`{"request":"original"}`)
		if err := CreateSecretCreateJournal("default", "default", "scott", testSecretCreateJournalHash, original); err != nil {
			t.Fatal(err)
		}
		path, _ := SecretCreateJournalPath("default", "default", "scott", testSecretCreateJournalHash)
		if err := os.Chmod(path, 0o640); err != nil {
			t.Fatal(err)
		}
		if err := ReplaceSecretCreateJournalAfterVaultKeyAdvance(
			"default", "default", "scott", testSecretCreateJournalHash, original, []byte(`{"request":"next"}`),
		); !errors.Is(err, ErrSecretCreateJournalUnsafe) {
			t.Fatalf("unsafe journal replace error = %v, want unsafe", err)
		}
	})

	t.Run("directory permissions", func(t *testing.T) {
		t.Setenv("WITSELF_HOME", t.TempDir())
		original := []byte(`{"request":"original"}`)
		if err := CreateSecretCreateJournal("default", "default", "scott", testSecretCreateJournalHash, original); err != nil {
			t.Fatal(err)
		}
		path, _ := SecretCreateJournalPath("default", "default", "scott", testSecretCreateJournalHash)
		if err := os.Chmod(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := ReplaceSecretCreateJournalAfterVaultKeyAdvance(
			"default", "default", "scott", testSecretCreateJournalHash, original, []byte(`{"request":"next"}`),
		); !errors.Is(err, ErrSecretCreateJournalUnsafe) {
			t.Fatalf("unsafe directory replace error = %v, want unsafe", err)
		}
	})

	t.Run("symlink lock", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("WITSELF_HOME", home)
		original := []byte(`{"request":"original"}`)
		if err := CreateSecretCreateJournal("default", "default", "scott", testSecretCreateJournalHash, original); err != nil {
			t.Fatal(err)
		}
		path, _ := SecretCreateJournalPath("default", "default", "scott", testSecretCreateJournalHash)
		target := filepath.Join(home, "lock-target")
		if err := os.WriteFile(target, []byte("must-not-change"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(filepath.Dir(path), secretCreateJournalLockFile)); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if err := ReplaceSecretCreateJournalAfterVaultKeyAdvance(
			"default", "default", "scott", testSecretCreateJournalHash, original, []byte(`{"request":"next"}`),
		); !errors.Is(err, ErrSecretCreateJournalUnsafe) {
			t.Fatalf("symlink-lock replace error = %v, want unsafe", err)
		}
		got, err := os.ReadFile(target)
		if err != nil || string(got) != "must-not-change" {
			t.Fatalf("lock symlink target = %q / %v", got, err)
		}
		journal, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(journal, original) {
			t.Fatalf("journal changed through unsafe lock = %q / %v", journal, err)
		}
	})

	t.Run("public lock permissions", func(t *testing.T) {
		t.Setenv("WITSELF_HOME", t.TempDir())
		original := []byte(`{"request":"original"}`)
		if err := CreateSecretCreateJournal("default", "default", "scott", testSecretCreateJournalHash, original); err != nil {
			t.Fatal(err)
		}
		path, _ := SecretCreateJournalPath("default", "default", "scott", testSecretCreateJournalHash)
		lockPath := filepath.Join(filepath.Dir(path), secretCreateJournalLockFile)
		if err := os.WriteFile(lockPath, nil, 0o640); err != nil {
			t.Fatal(err)
		}
		if err := ReplaceSecretCreateJournalAfterVaultKeyAdvance(
			"default", "default", "scott", testSecretCreateJournalHash, original, []byte(`{"request":"next"}`),
		); !errors.Is(err, ErrSecretCreateJournalUnsafe) {
			t.Fatalf("public-lock replace error = %v, want unsafe", err)
		}
	})
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
