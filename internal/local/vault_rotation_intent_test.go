package local

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/witwave-ai/witself/internal/sealed"
)

const (
	localRotationID      = "vkr_aaaaaaaaaaaaaaaa"
	localRotationOtherID = "vkr_bbbbbbbbbbbbbbbb"
	localRotationOpID    = "op_aaaaaaaaaaaaaaaa"
	localRotationAccount = "acc_aaaaaaaaaaaaaaaa"
	localRotationRealm   = "realm_aaaaaaaaaaaaaaaa"
	localRotationAgent   = "agent_aaaaaaaaaaaaaaaa"
)

func TestAgentVaultKeyRotationIntentRoundTripAndPermissions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITSELF_HOME", home)
	intent := localRotationIntent(t, true)
	if err := CreateAgentVaultKeyRotationIntent("default", "default", "scott", intent); err != nil {
		t.Fatal(err)
	}
	path, err := AgentVaultKeyRotationIntentPath("default", "default", "scott")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 || !info.Mode().IsRegular() {
		t.Fatalf("intent mode = %v, want owner-only regular file", info.Mode())
	}
	for directory := filepath.Dir(path); directory != home; directory = filepath.Dir(directory) {
		info, err := os.Lstat(directory)
		if err != nil {
			t.Fatal(err)
		}
		if !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Fatalf("directory %q mode = %v", directory, info.Mode())
		}
	}
	got, err := ReadAgentVaultKeyRotationIntent("default", "default", "scott")
	if err != nil {
		t.Fatal(err)
	}
	if *got != intent {
		t.Fatalf("round trip = %#v, want %#v", got, intent)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"private", "secret", "wrapped_dek", "token", "passphrase"} {
		if strings.Contains(strings.ToLower(string(raw)), forbidden) {
			t.Fatalf("intent contains forbidden private-material marker %q", forbidden)
		}
	}
}

func TestAgentVaultKeyRotationIntentSupportsAdoptedOpenRotation(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	intent := localRotationIntent(t, false)
	if err := CreateAgentVaultKeyRotationIntent("default", "default", "scott", intent); err != nil {
		t.Fatal(err)
	}
	got, err := ReadAgentVaultKeyRotationIntent("default", "default", "scott")
	if err != nil || *got != intent {
		t.Fatalf("adopted intent = %#v, %v", got, err)
	}
}

func TestAgentVaultKeyRotationIntentIsAtomicNoReplace(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	intent := localRotationIntent(t, true)
	const workers = 20
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for index := range errs {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			errs[index] = CreateAgentVaultKeyRotationIntent("default", "default", "scott", intent)
		}(index)
	}
	wg.Wait()
	successes, collisions := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrAgentVaultKeyRotationIntentExists):
			collisions++
		default:
			t.Fatalf("unexpected create error: %v", err)
		}
	}
	if successes != 1 || collisions != workers-1 {
		t.Fatalf("success/collision = %d/%d", successes, collisions)
	}
}

func TestAgentVaultKeyRotationIntentRejectsCrossScopeCopyAndUnsafeFile(t *testing.T) {
	t.Run("scope-bound checksum", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("WITSELF_HOME", home)
		if err := CreateAgentVaultKeyRotationIntent("first", "default", "scott", localRotationIntent(t, true)); err != nil {
			t.Fatal(err)
		}
		source, _ := AgentVaultKeyRotationIntentPath("first", "default", "scott")
		raw, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		target, _ := AgentVaultKeyRotationIntentPath("second", "default", "scott")
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadAgentVaultKeyRotationIntent("second", "default", "scott"); !errors.Is(err, ErrAgentVaultKeyRotationIntentInvalid) {
			t.Fatalf("cross-scope copy error = %v", err)
		}
	})

	t.Run("symlink", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("WITSELF_HOME", home)
		path, _ := AgentVaultKeyRotationIntentPath("default", "default", "scott")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(home, "target")
		if err := os.WriteFile(target, []byte("do-not-read"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadAgentVaultKeyRotationIntent("default", "default", "scott"); !errors.Is(err, ErrAgentVaultKeyRotationIntentUnsafe) {
			t.Fatalf("symlink read error = %v", err)
		}
		if err := CreateAgentVaultKeyRotationIntent("default", "default", "scott", localRotationIntent(t, true)); !errors.Is(err, ErrAgentVaultKeyRotationIntentUnsafe) {
			t.Fatalf("symlink create error = %v", err)
		}
	})

	t.Run("symlink lock", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("WITSELF_HOME", home)
		path, _ := AgentVaultKeyRotationIntentPath("default", "default", "scott")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(home, "lock-target")
		marker := []byte("must-remain-unchanged")
		if err := os.WriteFile(target, marker, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(filepath.Dir(path), agentVaultKeyRotationLockFile)); err != nil {
			t.Fatal(err)
		}
		if err := CreateAgentVaultKeyRotationIntent("default", "default", "scott", localRotationIntent(t, true)); !errors.Is(err, ErrAgentVaultKeyRotationIntentUnsafe) {
			t.Fatalf("symlink lock create error = %v", err)
		}
		got, err := os.ReadFile(target)
		if err != nil || string(got) != string(marker) {
			t.Fatalf("symlink lock target changed: %q, %v", got, err)
		}
	})
}

func TestDeleteAgentVaultKeyRotationIntentRequiresExactAcknowledgement(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	intent := localRotationIntent(t, true)
	if err := CreateAgentVaultKeyRotationIntent("default", "default", "scott", intent); err != nil {
		t.Fatal(err)
	}
	if err := DeleteAgentVaultKeyRotationIntentAfterAcknowledge("default", "default", "scott", localRotationOtherID); !errors.Is(err, ErrAgentVaultKeyRotationIntentConflict) {
		t.Fatalf("mismatched acknowledgement error = %v", err)
	}
	if _, err := ReadAgentVaultKeyRotationIntent("default", "default", "scott"); err != nil {
		t.Fatalf("mismatched acknowledgement deleted intent: %v", err)
	}
	if err := DeleteAgentVaultKeyRotationIntentAfterAcknowledge("default", "default", "scott", localRotationID); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadAgentVaultKeyRotationIntent("default", "default", "scott"); !errors.Is(err, ErrAgentVaultKeyRotationIntentUnavailable) {
		t.Fatalf("deleted intent read error = %v", err)
	}
	if err := DeleteAgentVaultKeyRotationIntentAfterAcknowledge("default", "default", "scott", localRotationID); err != nil {
		t.Fatalf("idempotent acknowledgement: %v", err)
	}
}

func TestReplaceAgentVaultKeyRotationIntentAfterCanonicalConflictIsNarrowAndAtomic(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	expected := localRotationIntent(t, true)
	replacement := expected
	replacement.RotationID = localRotationOtherID
	replacement.ExpectedSourceKeyRowVersion = 0
	replacement.StartIdempotencyKey = ""
	replacement.Target = localRotationIntent(t, false).Target
	if replacement.Target == expected.Target {
		t.Fatal("test target unexpectedly reused metadata")
	}
	if err := CreateAgentVaultKeyRotationIntent("default", "default", "scott", expected); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceAgentVaultKeyRotationIntentAfterCanonicalConflict(
		"default", "default", "scott", expected, replacement,
	); err != nil {
		t.Fatal(err)
	}
	got, err := ReadAgentVaultKeyRotationIntent("default", "default", "scott")
	if err != nil || *got != replacement {
		t.Fatalf("replacement = %#v, %v", got, err)
	}
	info, err := os.Lstat(mustLocalRotationIntentPath(t))
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("replacement file mode = %v", info.Mode())
	}

	if err := ReplaceAgentVaultKeyRotationIntentAfterCanonicalConflict(
		"default", "default", "scott", expected, replacement,
	); !errors.Is(err, ErrAgentVaultKeyRotationIntentConflict) {
		t.Fatalf("stale expected error = %v", err)
	}
	preparedReplacement := replacement
	preparedReplacement.ExpectedSourceKeyRowVersion = 9
	preparedReplacement.StartIdempotencyKey = localRotationOpID
	if err := ReplaceAgentVaultKeyRotationIntentAfterCanonicalConflict(
		"default", "default", "scott", replacement, preparedReplacement,
	); !errors.Is(err, ErrAgentVaultKeyRotationIntentConflict) {
		t.Fatalf("prepared replacement error = %v", err)
	}
	differentSource := replacement
	differentSource.RotationID = localRotationID
	differentSource.Source = localRotationIntent(t, false).Source
	if differentSource.Source == replacement.Source {
		t.Fatal("test source unexpectedly reused metadata")
	}
	if err := ReplaceAgentVaultKeyRotationIntentAfterCanonicalConflict(
		"default", "default", "scott", replacement, differentSource,
	); !errors.Is(err, ErrAgentVaultKeyRotationIntentConflict) {
		t.Fatalf("different source error = %v", err)
	}
}

func TestAgentVaultKeyRotationIntentMutationsShareStableProcessLock(t *testing.T) {
	expected := localRotationIntent(t, true)
	replacement := expected
	replacement.RotationID = localRotationOtherID
	replacement.ExpectedSourceKeyRowVersion = 0
	replacement.StartIdempotencyKey = ""
	replacement.Target = localRotationIntent(t, false).Target
	for iteration := 0; iteration < 24; iteration++ {
		t.Setenv("WITSELF_HOME", t.TempDir())
		if err := CreateAgentVaultKeyRotationIntent("default", "default", "scott", expected); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		errs := make(chan error, 2)
		go func() {
			<-start
			errs <- ReplaceAgentVaultKeyRotationIntentAfterCanonicalConflict(
				"default", "default", "scott", expected, replacement,
			)
		}()
		go func() {
			<-start
			errs <- DeleteAgentVaultKeyRotationIntentAfterAcknowledge(
				"default", "default", "scott", expected.RotationID,
			)
		}()
		close(start)
		first, second := <-errs, <-errs
		if first == nil && second == nil {
			t.Fatal("replace and stale delete both succeeded")
		}
		intent, readErr := ReadAgentVaultKeyRotationIntent("default", "default", "scott")
		switch {
		case first == nil || second == nil:
			// Exactly one mutation won. The only valid durable outcomes are the
			// replacement or no journal; a stale delete can never remove a
			// replacement published by the other process.
			if readErr == nil && *intent != replacement {
				t.Fatalf("unexpected concurrent winner: %#v", intent)
			}
			if readErr != nil && !errors.Is(readErr, ErrAgentVaultKeyRotationIntentUnavailable) {
				t.Fatalf("concurrent final read error = %v", readErr)
			}
		default:
			t.Fatalf("neither concurrent mutation succeeded: %v / %v", first, second)
		}
		path := filepath.Join(filepath.Dir(mustLocalRotationIntentPath(t)), agentVaultKeyRotationLockFile)
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			t.Fatalf("stable lock mode = %v", info.Mode())
		}
	}
}

func TestRetireAgentVaultKeyRotationIntentAfterCanonicalAdvanceRequiresExactPreparedRecord(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	expected := localRotationIntent(t, true)
	if err := CreateAgentVaultKeyRotationIntent("default", "default", "scott", expected); err != nil {
		t.Fatal(err)
	}
	stale := expected
	stale.Target.Fingerprint = strings.Repeat("f", 64)
	if err := RetireAgentVaultKeyRotationIntentAfterCanonicalAdvance(
		"default", "default", "scott", stale,
	); !errors.Is(err, ErrAgentVaultKeyRotationIntentConflict) {
		t.Fatalf("stale retirement error = %v", err)
	}
	if _, err := ReadAgentVaultKeyRotationIntent("default", "default", "scott"); err != nil {
		t.Fatalf("stale retirement removed current record: %v", err)
	}
	if err := RetireAgentVaultKeyRotationIntentAfterCanonicalAdvance(
		"default", "default", "scott", expected,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadAgentVaultKeyRotationIntent("default", "default", "scott"); !errors.Is(err, ErrAgentVaultKeyRotationIntentUnavailable) {
		t.Fatalf("retired intent read error = %v", err)
	}
	adopted := localRotationIntent(t, false)
	if err := CreateAgentVaultKeyRotationIntent("default", "default", "scott", adopted); err != nil {
		t.Fatal(err)
	}
	if err := RetireAgentVaultKeyRotationIntentAfterCanonicalAdvance(
		"default", "default", "scott", adopted,
	); !errors.Is(err, ErrAgentVaultKeyRotationIntentConflict) {
		t.Fatalf("adopted retirement error = %v", err)
	}
}

func TestAgentVaultKeyRotationIntentErrorsAreRedacted(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	intent := localRotationIntent(t, true)
	marker := "cobalt-rotation-marker"
	intent.StartIdempotencyKey = marker
	err := CreateAgentVaultKeyRotationIntent("default", "default", "scott", intent)
	if !errors.Is(err, ErrAgentVaultKeyRotationIntentInvalid) || strings.Contains(err.Error(), marker) {
		t.Fatalf("invalid error = %v", err)
	}
}

func localRotationIntent(t *testing.T, prepared bool) AgentVaultKeyRotationIntent {
	t.Helper()
	source, err := sealed.GenerateAgentVaultKey(1)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Clear()
	target, err := sealed.GenerateAgentVaultKey(2)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Clear()
	intent := AgentVaultKeyRotationIntent{
		RotationID: localRotationID, AccountID: localRotationAccount, RealmID: localRotationRealm,
		OwnerAgentID: localRotationAgent, Source: source.Metadata(), Target: target.Metadata(),
	}
	if prepared {
		intent.ExpectedSourceKeyRowVersion = 7
		intent.StartIdempotencyKey = localRotationOpID
	}
	return intent
}

func mustLocalRotationIntentPath(t *testing.T) string {
	t.Helper()
	path, err := AgentVaultKeyRotationIntentPath("default", "default", "scott")
	if err != nil {
		t.Fatal(err)
	}
	return path
}
