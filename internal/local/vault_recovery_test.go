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

func TestAgentVaultKeyRecoveryPathUsesImmutableEpochAndValidatesScope(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITSELF_HOME", home)
	key := newTestAgentVaultKeyVersion(t, 9)
	defer key.Clear()

	got, err := AgentVaultKeyRecoveryPath("default", "work-realm", "scott", key.ID(), key.Version())
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "keys", "accounts", "default", "realms", "work-realm",
		"agents", "scott", "recovery", "9-"+key.ID()+".recovery")
	if got != want {
		t.Fatalf("AgentVaultKeyRecoveryPath() = %q, want %q", got, want)
	}

	for _, test := range []struct {
		account, realm, agent, keyID string
		version                      uint64
	}{
		{account: "../other", realm: "default", agent: "scott", keyID: key.ID(), version: 9},
		{account: "default", realm: "../other", agent: "scott", keyID: key.ID(), version: 9},
		{account: "default", realm: "default", agent: "../other", keyID: key.ID(), version: 9},
		{account: "default", realm: "default", agent: "scott", keyID: "avk_1111111111111111", version: 9},
		{account: "default", realm: "default", agent: "scott", keyID: key.ID(), version: 0},
	} {
		if _, err := AgentVaultKeyRecoveryPath(test.account, test.realm, test.agent, test.keyID, test.version); !errors.Is(err, ErrAgentVaultKeyRecoveryScope) {
			t.Fatalf("invalid scope %#v error = %v", test, err)
		}
	}
}

func TestCreateAndReadAgentVaultKeyRecovery(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITSELF_HOME", home)
	artifact, metadata := newLocalRecoveryArtifact(t, 3)
	original := append([]byte(nil), artifact...)

	path, err := CreateAgentVaultKeyRecovery("default", "default", "scott", artifact)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(artifact, original) {
		t.Fatal("CreateAgentVaultKeyRecovery modified caller-owned artifact")
	}
	wantPath, _ := AgentVaultKeyRecoveryPath("default", "default", "scott", metadata.AVK.ID, metadata.AVK.Version)
	if path != wantPath {
		t.Fatalf("created path = %q, want %q", path, wantPath)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("artifact mode = %v, want regular 0600", info.Mode())
	}
	assertAgentVaultKeyDirectoriesPrivate(t, home, filepath.Dir(path))
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(path) {
		t.Fatalf("recovery directory entries = %#v, want only canonical artifact", entries)
	}

	got, gotMetadata, err := ReadAgentVaultKeyRecovery(
		"default", "default", "scott", metadata.AVK.ID, metadata.AVK.Version,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(got)
	if !bytes.Equal(got, artifact) || gotMetadata != metadata {
		t.Fatalf("managed read changed artifact or metadata: metadata=%+v want=%+v", gotMetadata, metadata)
	}

	if _, err := CreateAgentVaultKeyRecovery("default", "default", "scott", artifact); !errors.Is(err, ErrAgentVaultKeyRecoveryExists) {
		t.Fatalf("second create error = %v, want ErrAgentVaultKeyRecoveryExists", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, artifact) {
		t.Fatal("collision replaced existing recovery artifact")
	}
}

func TestCreateAgentVaultKeyRecoveryIsAtomicNoReplace(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	artifact, metadata := newLocalRecoveryArtifact(t, 4)
	const workers = 16
	errorsByWorker := make([]error, workers)
	var wait sync.WaitGroup
	wait.Add(workers)
	start := make(chan struct{})
	for worker := range workers {
		go func() {
			defer wait.Done()
			<-start
			_, errorsByWorker[worker] = CreateAgentVaultKeyRecovery("default", "default", "scott", artifact)
		}()
	}
	close(start)
	wait.Wait()
	successes, collisions := 0, 0
	for _, err := range errorsByWorker {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrAgentVaultKeyRecoveryExists):
			collisions++
		default:
			t.Fatalf("concurrent create error = %v", err)
		}
	}
	if successes != 1 || collisions != workers-1 {
		t.Fatalf("concurrent results: successes=%d collisions=%d", successes, collisions)
	}
	got, _, err := ReadAgentVaultKeyRecovery("default", "default", "scott", metadata.AVK.ID, metadata.AVK.Version)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(got)
	if !bytes.Equal(got, artifact) {
		t.Fatal("concurrent publication persisted partial or changed bytes")
	}
}

func TestManagedAgentVaultKeyRecoveryRejectsUnsafeAndMismatchedPaths(t *testing.T) {
	t.Run("final symlink", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("WITSELF_HOME", home)
		artifact, metadata := newLocalRecoveryArtifact(t, 1)
		_, directory, path, err := agentVaultKeyRecoveryLocation("default", "default", "scott", metadata.AVK.ID, metadata.AVK.Version)
		if err != nil {
			t.Fatal(err)
		}
		if err := ensureAgentVaultKeyRecoveryDirectories(home, directory); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(t.TempDir(), "target")
		if err := os.WriteFile(target, []byte("unchanged"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
		if _, err := CreateAgentVaultKeyRecovery("default", "default", "scott", artifact); !errors.Is(err, ErrAgentVaultKeyRecoveryUnsafe) {
			t.Fatalf("create through final symlink error = %v, want unsafe", err)
		}
		got, _ := os.ReadFile(target)
		if string(got) != "unchanged" {
			t.Fatal("final symlink target was modified")
		}
	})

	t.Run("intermediate symlink", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("WITSELF_HOME", home)
		artifact, _ := newLocalRecoveryArtifact(t, 1)
		if err := os.Mkdir(filepath.Join(home, "keys"), 0o700); err != nil {
			t.Fatal(err)
		}
		outside := t.TempDir()
		if err := os.Symlink(outside, filepath.Join(home, "keys", "accounts")); err != nil {
			t.Fatal(err)
		}
		if _, err := CreateAgentVaultKeyRecovery("default", "default", "scott", artifact); !errors.Is(err, ErrAgentVaultKeyRecoveryUnsafe) {
			t.Fatalf("create through parent symlink error = %v, want unsafe", err)
		}
	})

	t.Run("renamed epoch", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("WITSELF_HOME", home)
		artifact, _ := newLocalRecoveryArtifact(t, 2)
		_, otherMetadata := newLocalRecoveryArtifact(t, 7)
		_, directory, wrongPath, err := agentVaultKeyRecoveryLocation(
			"default", "default", "scott", otherMetadata.AVK.ID, otherMetadata.AVK.Version,
		)
		if err != nil {
			t.Fatal(err)
		}
		if err := ensureAgentVaultKeyRecoveryDirectories(home, directory); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(wrongPath, artifact, 0o600); err != nil {
			t.Fatal(err)
		}
		got, _, err := ReadAgentVaultKeyRecovery(
			"default", "default", "scott", otherMetadata.AVK.ID, otherMetadata.AVK.Version,
		)
		if got != nil || !errors.Is(err, ErrAgentVaultKeyRecoveryInvalid) {
			t.Fatalf("renamed recovery read = %q, %v, want nil invalid", got, err)
		}
	})

	t.Run("permission drift", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("WITSELF_HOME", home)
		artifact, metadata := newLocalRecoveryArtifact(t, 1)
		path, err := CreateAgentVaultKeyRecovery("default", "default", "scott", artifact)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		got, _, err := ReadAgentVaultKeyRecovery("default", "default", "scott", metadata.AVK.ID, metadata.AVK.Version)
		if got != nil || !errors.Is(err, ErrAgentVaultKeyRecoveryUnsafe) {
			t.Fatalf("permission-drift read = %q, %v, want nil unsafe", got, err)
		}
	})
}

func TestWriteAndReadExplicitRecoveryArtifact(t *testing.T) {
	directory := resolvedRecoveryTempDir(t)
	if err := os.Chmod(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	artifact, metadata := newLocalRecoveryArtifact(t, 5)
	path := filepath.Join(directory, "scott-portable.recovery")
	if err := WriteRecoveryArtifact(path, artifact); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 || !info.Mode().IsRegular() {
		t.Fatalf("explicit artifact mode = %v, want regular 0600", info.Mode())
	}
	directoryInfo, err := os.Lstat(directory)
	if err != nil {
		t.Fatal(err)
	}
	if directoryInfo.Mode().Perm() != 0o755 {
		t.Fatalf("explicit export changed parent mode to %o", directoryInfo.Mode().Perm())
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(path) {
		t.Fatalf("explicit directory entries = %#v, want only final artifact", entries)
	}
	got, gotMetadata, err := ReadRecoveryArtifact(path)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(got)
	if !bytes.Equal(got, artifact) || gotMetadata != metadata {
		t.Fatalf("explicit read changed artifact or metadata: metadata=%+v want=%+v", gotMetadata, metadata)
	}
	if err := WriteRecoveryArtifact(path, artifact); !errors.Is(err, ErrAgentVaultKeyRecoveryExists) {
		t.Fatalf("explicit no-replace error = %v, want exists", err)
	}
}

func TestExplicitRecoveryArtifactRejectsUnsafeAndInvalidFiles(t *testing.T) {
	artifact, _ := newLocalRecoveryArtifact(t, 1)

	t.Run("parent symlink", func(t *testing.T) {
		realDirectory := resolvedRecoveryTempDir(t)
		linkParent := filepath.Join(filepath.Dir(realDirectory), "recovery-link-"+filepath.Base(realDirectory))
		if err := os.Symlink(realDirectory, linkParent); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.Remove(linkParent) }()
		if err := WriteRecoveryArtifact(filepath.Join(linkParent, "artifact"), artifact); !errors.Is(err, ErrAgentVaultKeyRecoveryUnsafe) {
			t.Fatalf("write through parent symlink error = %v, want unsafe", err)
		}
	})

	t.Run("final symlink", func(t *testing.T) {
		directory := resolvedRecoveryTempDir(t)
		target := filepath.Join(directory, "target")
		if err := os.WriteFile(target, artifact, 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(directory, "link")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if err := WriteRecoveryArtifact(link, artifact); !errors.Is(err, ErrAgentVaultKeyRecoveryUnsafe) {
			t.Fatalf("write to final symlink error = %v, want unsafe", err)
		}
		if got, _, err := ReadRecoveryArtifact(link); got != nil || !errors.Is(err, ErrAgentVaultKeyRecoveryUnsafe) {
			t.Fatalf("read final symlink = %q, %v, want nil unsafe", got, err)
		}
	})

	t.Run("wrong mode", func(t *testing.T) {
		directory := resolvedRecoveryTempDir(t)
		path := filepath.Join(directory, "artifact")
		if err := os.WriteFile(path, artifact, 0o644); err != nil {
			t.Fatal(err)
		}
		if got, _, err := ReadRecoveryArtifact(path); got != nil || !errors.Is(err, ErrAgentVaultKeyRecoveryUnsafe) {
			t.Fatalf("read mode 0644 = %q, %v, want nil unsafe", got, err)
		}
	})

	t.Run("oversized", func(t *testing.T) {
		directory := resolvedRecoveryTempDir(t)
		path := filepath.Join(directory, "artifact")
		if err := os.WriteFile(path, bytes.Repeat([]byte{'x'}, sealed.MaxAVKRecoveryPackageBytes+1), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, _, err := ReadRecoveryArtifact(path); got != nil || !errors.Is(err, ErrAgentVaultKeyRecoveryInvalid) {
			t.Fatalf("read oversized = %q, %v, want nil invalid", got, err)
		}
	})

	t.Run("malformed", func(t *testing.T) {
		directory := resolvedRecoveryTempDir(t)
		path := filepath.Join(directory, "artifact")
		marker := "private-recovery-marker"
		if err := os.WriteFile(path, []byte(marker), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, _, err := ReadRecoveryArtifact(path); got != nil || !errors.Is(err, ErrAgentVaultKeyRecoveryInvalid) || strings.Contains(err.Error(), marker) {
			t.Fatalf("read malformed = %q, %v, want redacted invalid", got, err)
		}
	})

	t.Run("missing parent", func(t *testing.T) {
		directory := resolvedRecoveryTempDir(t)
		path := filepath.Join(directory, "missing", "artifact")
		if err := WriteRecoveryArtifact(path, artifact); !errors.Is(err, ErrAgentVaultKeyRecoveryUnavailable) {
			t.Fatalf("write with missing parent error = %v, want unavailable", err)
		}
	})
}

func TestWriteExplicitRecoveryArtifactIsAtomicNoReplace(t *testing.T) {
	directory := resolvedRecoveryTempDir(t)
	path := filepath.Join(directory, "portable.recovery")
	artifact, _ := newLocalRecoveryArtifact(t, 6)
	const workers = 16
	errorsByWorker := make([]error, workers)
	var wait sync.WaitGroup
	wait.Add(workers)
	start := make(chan struct{})
	for worker := range workers {
		go func() {
			defer wait.Done()
			<-start
			errorsByWorker[worker] = WriteRecoveryArtifact(path, artifact)
		}()
	}
	close(start)
	wait.Wait()
	successes, collisions := 0, 0
	for _, err := range errorsByWorker {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrAgentVaultKeyRecoveryExists):
			collisions++
		default:
			t.Fatalf("concurrent explicit write error = %v", err)
		}
	}
	if successes != 1 || collisions != workers-1 {
		t.Fatalf("concurrent explicit results: successes=%d collisions=%d", successes, collisions)
	}
	got, _, err := ReadRecoveryArtifact(path)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(got)
	if !bytes.Equal(got, artifact) {
		t.Fatal("concurrent explicit publication persisted partial or changed bytes")
	}
}

func TestRecoveryArtifactWriteRejectsInvalidWithoutCreatingAFile(t *testing.T) {
	directory := resolvedRecoveryTempDir(t)
	path := filepath.Join(directory, "portable.recovery")
	marker := []byte("private-recovery-marker")
	if err := WriteRecoveryArtifact(path, marker); !errors.Is(err, ErrAgentVaultKeyRecoveryInvalid) || strings.Contains(err.Error(), string(marker)) {
		t.Fatalf("invalid write error = %v, want redacted invalid", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid write created final file: %v", err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("invalid write left files: %#v", entries)
	}
}

func newLocalRecoveryArtifact(t *testing.T, version uint64) ([]byte, sealed.AVKRecoveryMetadata) {
	t.Helper()
	key := newTestAgentVaultKeyVersion(t, version)
	defer key.Clear()
	artifact, err := sealed.ExportAgentVaultKeyRecovery(key, []byte("local recovery test passphrase"), sealed.AVKRecoveryScope{
		AccountID: "acc_abcdefghijklmnop", RealmID: "realm_abcdefghijklmnop", OwnerAgentID: "agent_abcdefghijklmnop",
	})
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := sealed.InspectAgentVaultKeyRecovery(artifact)
	if err != nil {
		clear(artifact)
		t.Fatal(err)
	}
	return artifact, metadata
}

func resolvedRecoveryTempDir(t *testing.T) string {
	t.Helper()
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Clean(directory)
}
