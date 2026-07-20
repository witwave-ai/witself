package local

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/witwave-ai/witself/internal/sealed"
)

func TestAgentVaultKeyEnrollmentPathScopesAndValidates(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITSELF_HOME", home)
	path, err := AgentVaultKeyEnrollmentPath("default", "work-realm", "scott", localEnrollmentRequestID)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "keys", "accounts", "default", "realms", "work-realm",
		"agents", "scott", "enrollments", localEnrollmentRequestID)
	if path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}

	for _, test := range []struct {
		account, realm, agent, requestID string
	}{
		{"../other", "default", "scott", localEnrollmentRequestID},
		{"default", "../other", "scott", localEnrollmentRequestID},
		{"default", "default", "../other", localEnrollmentRequestID},
		{"default", "default", "scott", "../other"},
		{"default", "default", "scott", "enr_1111111111111111"},
		{"default", "default", "scott", ""},
	} {
		if _, err := AgentVaultKeyEnrollmentPath(test.account, test.realm, test.agent, test.requestID); !errors.Is(err, ErrAgentVaultKeyEnrollmentScope) {
			t.Fatalf("invalid scope %#v error = %v", test, err)
		}
	}
}

func TestListAgentVaultKeyEnrollmentStateIDsIsBoundedAndValueFree(t *testing.T) {
	t.Run("missing and sorted durable preflights", func(t *testing.T) {
		t.Setenv("WITSELF_HOME", t.TempDir())
		ids, err := ListAgentVaultKeyEnrollmentStateIDs("default", "default", "scott")
		if err != nil || ids == nil || len(ids) != 0 {
			t.Fatalf("missing list = %#v, %v", ids, err)
		}
		for _, requestID := range []string{"enr_bbbbbbbbbbbbbbbb", "enr_aaaaaaaaaaaaaaaa"} {
			recipient, err := sealed.GenerateAVKEnrollmentRecipientKey()
			if err != nil {
				t.Fatal(err)
			}
			pairing, err := sealed.GenerateAVKEnrollmentPairingSecret()
			if err != nil {
				t.Fatal(err)
			}
			if err := CreateAgentVaultKeyEnrollmentPreflight(
				"default", "default", "scott", requestID, recipient, pairing,
			); err != nil {
				t.Fatal(err)
			}
			recipient.Clear()
			pairing.Clear()
		}
		ids, err = ListAgentVaultKeyEnrollmentStateIDs("default", "default", "scott")
		if err != nil || fmt.Sprint(ids) != "[enr_aaaaaaaaaaaaaaaa enr_bbbbbbbbbbbbbbbb]" {
			t.Fatalf("listed ids = %#v, %v", ids, err)
		}
	})

	t.Run("entry bound fails closed", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("WITSELF_HOME", home)
		parent := filepath.Join(home, "keys", "accounts", "default", "realms", "default",
			"agents", "scott", "enrollments")
		if err := os.MkdirAll(parent, 0o700); err != nil {
			t.Fatal(err)
		}
		alphabet := "abcdefghijklmnopqrstuvwxyz234567"
		for index := 0; index < MaxAgentVaultKeyEnrollmentStates+1; index++ {
			requestID := "enr_" + strings.Repeat("a", 14) +
				string(alphabet[index/len(alphabet)]) + string(alphabet[index%len(alphabet)])
			if err := os.Mkdir(filepath.Join(parent, requestID), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := ListAgentVaultKeyEnrollmentStateIDs("default", "default", "scott"); !errors.Is(err, ErrAgentVaultKeyEnrollmentInvalid) {
			t.Fatalf("bounded list error = %v, want invalid", err)
		}
	})

	t.Run("unknown entry fails closed", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("WITSELF_HOME", home)
		parent := filepath.Join(home, "keys", "accounts", "default", "realms", "default",
			"agents", "scott", "enrollments")
		if err := os.MkdirAll(filepath.Join(parent, "not-an-enrollment"), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := ListAgentVaultKeyEnrollmentStateIDs("default", "default", "scott"); !errors.Is(err, ErrAgentVaultKeyEnrollmentUnsafe) {
			t.Fatalf("unknown entry error = %v, want unsafe", err)
		}
	})
}

func TestAgentVaultKeyEnrollmentPreflightReadAndRedaction(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITSELF_HOME", home)
	recipient, pairing, _ := newLocalEnrollmentMaterial(t, localEnrollmentRequestID, localEnrollmentExpiresAt)
	if err := CreateAgentVaultKeyEnrollmentPreflight("default", "default", "scott", localEnrollmentRequestID, recipient, pairing); err != nil {
		t.Fatal(err)
	}
	directory, _ := AgentVaultKeyEnrollmentPath("default", "default", "scott", localEnrollmentRequestID)
	privatePath := filepath.Join(directory, agentVaultKeyEnrollmentPrivateFile)
	assertLocalEnrollmentPrivateLayout(t, home, directory, privatePath)
	if _, err := os.Lstat(filepath.Join(directory, agentVaultKeyEnrollmentRequestFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("preflight unexpectedly finalized a request: %v", err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != agentVaultKeyEnrollmentPrivateFile {
		t.Fatalf("preflight directory entries = %#v", entries)
	}

	state, err := ReadAgentVaultKeyEnrollmentState("default", "default", "scott", localEnrollmentRequestID)
	if err != nil {
		t.Fatal(err)
	}
	if state.Finalized() || state.Request != nil || state.RequestID != localEnrollmentRequestID {
		t.Fatalf("unexpected preflight state: %#v", state)
	}
	wantPublic, _ := recipient.PublicKey()
	gotPublic, _ := state.RecipientKey.PublicKey()
	wantPairing, _ := sealed.EncodeAVKEnrollmentPairingSecret(pairing)
	gotPairing, _ := sealed.EncodeAVKEnrollmentPairingSecret(state.PairingSecret)
	if gotPublic != wantPublic || gotPairing != wantPairing {
		t.Fatal("read preflight private values changed")
	}
	for _, rendered := range []string{fmt.Sprint(state), fmt.Sprintf("%#v", state), fmt.Sprint(*state)} {
		if strings.Contains(rendered, wantPairing) || strings.Contains(rendered, wantPublic) ||
			!strings.Contains(rendered, "private=redacted") {
			t.Fatalf("state formatting disclosed private values: %q", rendered)
		}
	}
	if raw, err := json.Marshal(state); len(raw) != 0 || !errors.Is(err, ErrAgentVaultKeyEnrollmentDisclosure) {
		t.Fatalf("state JSON = %q, %v", raw, err)
	}
	state.Clear()
	if state.RecipientKey != nil || state.PairingSecret != nil || state.Request != nil {
		t.Fatal("Clear retained enrollment values")
	}
}

func TestAgentVaultKeyEnrollmentPreflightIsAtomicNoReplace(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	recipient, pairing, _ := newLocalEnrollmentMaterial(t, localEnrollmentRequestID, localEnrollmentExpiresAt)
	const workers = 20
	errorsByWorker := make([]error, workers)
	var wg sync.WaitGroup
	for i := range errorsByWorker {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			errorsByWorker[index] = CreateAgentVaultKeyEnrollmentPreflight(
				"default", "default", "scott", localEnrollmentRequestID, recipient, pairing)
		}(i)
	}
	wg.Wait()
	successes, collisions := 0, 0
	for _, err := range errorsByWorker {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrAgentVaultKeyEnrollmentExists):
			collisions++
		default:
			t.Fatalf("unexpected concurrent create error: %v", err)
		}
	}
	if successes != 1 || collisions != workers-1 {
		t.Fatalf("successes/collisions = %d/%d", successes, collisions)
	}
	state, err := ReadAgentVaultKeyEnrollmentState("default", "default", "scott", localEnrollmentRequestID)
	if err != nil {
		t.Fatal(err)
	}
	state.Clear()
}

func TestFinalizeAgentVaultKeyEnrollmentRequestIsImmutableAndIdempotent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITSELF_HOME", home)
	recipient, pairing, request := newLocalEnrollmentMaterial(t, localEnrollmentRequestID, localEnrollmentExpiresAt)
	if err := CreateAgentVaultKeyEnrollmentPreflight("default", "default", "scott", localEnrollmentRequestID, recipient, pairing); err != nil {
		t.Fatal(err)
	}
	if err := FinalizeAgentVaultKeyEnrollmentRequest("default", "default", "scott", localEnrollmentRequestID, request); err != nil {
		t.Fatal(err)
	}
	if err := FinalizeAgentVaultKeyEnrollmentRequest("default", "default", "scott", localEnrollmentRequestID, request); err != nil {
		t.Fatalf("exact finalize retry: %v", err)
	}
	directory, _ := AgentVaultKeyEnrollmentPath("default", "default", "scott", localEnrollmentRequestID)
	requestPath := filepath.Join(directory, agentVaultKeyEnrollmentRequestFile)
	assertLocalEnrollmentPrivateLayout(t, home, directory, requestPath)

	state, err := ReadAgentVaultKeyEnrollmentState("default", "default", "scott", localEnrollmentRequestID)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Clear()
	if !state.Finalized() || state.Request == nil || *state.Request != request {
		t.Fatalf("finalized state = %#v", state)
	}
	requestRaw, err := os.ReadFile(requestPath)
	if err != nil {
		t.Fatal(err)
	}
	recipientRaw, _ := sealed.EncodeAVKEnrollmentRecipientKey(recipient)
	pairingRaw, _ := sealed.EncodeAVKEnrollmentPairingSecret(pairing)
	if bytes.Contains(requestRaw, recipientRaw) || bytes.Contains(requestRaw, []byte(pairingRaw)) {
		t.Fatal("public request sidecar duplicated private preflight material")
	}
	clear(recipientRaw)
	clear(requestRaw)

	_, _, different := newLocalEnrollmentMaterialWithExistingSecrets(t, recipient, pairing,
		localEnrollmentRequestID, localEnrollmentExpiresAt+1)
	if err := FinalizeAgentVaultKeyEnrollmentRequest("default", "default", "scott", localEnrollmentRequestID, different); !errors.Is(err, ErrAgentVaultKeyEnrollmentConflict) {
		t.Fatalf("different finalized request error = %v", err)
	}
}

func TestFinalizeAgentVaultKeyEnrollmentRejectsMismatchedPrivateState(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	recipient, pairing, request := newLocalEnrollmentMaterial(t, localEnrollmentRequestID, localEnrollmentExpiresAt)
	if err := CreateAgentVaultKeyEnrollmentPreflight("default", "default", "scott", localEnrollmentRequestID, recipient, pairing); err != nil {
		t.Fatal(err)
	}

	otherRecipient, _, recipientMismatch := newLocalEnrollmentMaterialWithExistingSecrets(t, nil, pairing,
		localEnrollmentRequestID, localEnrollmentExpiresAt)
	_ = otherRecipient
	otherPairing, err := sealed.GenerateAVKEnrollmentPairingSecret()
	if err != nil {
		t.Fatal(err)
	}
	_, _, pairingMismatch := newLocalEnrollmentMaterialWithExistingSecrets(t, recipient, otherPairing,
		localEnrollmentRequestID, localEnrollmentExpiresAt)
	_, _, requestIDMismatch := newLocalEnrollmentMaterialWithExistingSecrets(t, recipient, pairing,
		"enr_bbbbbbbbbbbbbbbb", localEnrollmentExpiresAt)

	for name, candidate := range map[string]sealed.AVKEnrollmentRequest{
		"recipient":  recipientMismatch,
		"pairing":    pairingMismatch,
		"request id": requestIDMismatch,
	} {
		t.Run(name, func(t *testing.T) {
			if err := FinalizeAgentVaultKeyEnrollmentRequest("default", "default", "scott", localEnrollmentRequestID, candidate); !errors.Is(err, ErrAgentVaultKeyEnrollmentConflict) {
				t.Fatalf("error = %v, want conflict", err)
			}
		})
	}
	if err := FinalizeAgentVaultKeyEnrollmentRequest("default", "default", "scott", localEnrollmentRequestID, request); err != nil {
		t.Fatalf("valid finalize after rejected candidates: %v", err)
	}
}

func TestFinalizeAgentVaultKeyEnrollmentConcurrentRetry(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	recipient, pairing, request := newLocalEnrollmentMaterial(t, localEnrollmentRequestID, localEnrollmentExpiresAt)
	if err := CreateAgentVaultKeyEnrollmentPreflight("default", "default", "scott", localEnrollmentRequestID, recipient, pairing); err != nil {
		t.Fatal(err)
	}
	const workers = 16
	errorsByWorker := make([]error, workers)
	var wg sync.WaitGroup
	for i := range errorsByWorker {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			errorsByWorker[index] = FinalizeAgentVaultKeyEnrollmentRequest(
				"default", "default", "scott", localEnrollmentRequestID, request)
		}(i)
	}
	wg.Wait()
	for _, err := range errorsByWorker {
		if err != nil {
			t.Fatalf("concurrent exact finalization error: %v", err)
		}
	}
}

func TestDeleteAgentVaultKeyEnrollmentAfterConsumeRequiresFinalizedStateAndIsIdempotent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITSELF_HOME", home)
	recipient, pairing, request := newLocalEnrollmentMaterial(t, localEnrollmentRequestID, localEnrollmentExpiresAt)
	if err := CreateAgentVaultKeyEnrollmentPreflight("default", "default", "scott", localEnrollmentRequestID, recipient, pairing); err != nil {
		t.Fatal(err)
	}
	if err := DeleteAgentVaultKeyEnrollmentAfterConsume("default", "default", "scott", localEnrollmentRequestID); !errors.Is(err, ErrAgentVaultKeyEnrollmentConflict) {
		t.Fatalf("preflight delete error = %v", err)
	}
	if _, err := ReadAgentVaultKeyEnrollmentState("default", "default", "scott", localEnrollmentRequestID); err != nil {
		t.Fatalf("preflight disappeared after rejected delete: %v", err)
	}
	if err := FinalizeAgentVaultKeyEnrollmentRequest("default", "default", "scott", localEnrollmentRequestID, request); err != nil {
		t.Fatal(err)
	}
	directory, _ := AgentVaultKeyEnrollmentPath("default", "default", "scott", localEnrollmentRequestID)
	// Simulate a crash after canonical hard-link publication but before the
	// temporary link was removed. Consume cleanup must remove this second link
	// to private material as well as the canonical file.
	if err := os.Link(filepath.Join(directory, agentVaultKeyEnrollmentPrivateFile),
		filepath.Join(directory, ".enrollment-write-crash.tmp")); err != nil {
		t.Fatal(err)
	}
	if err := DeleteAgentVaultKeyEnrollmentAfterConsume("default", "default", "scott", localEnrollmentRequestID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("enrollment directory survived delete: %v", err)
	}
	if _, err := ReadAgentVaultKeyEnrollmentState("default", "default", "scott", localEnrollmentRequestID); !errors.Is(err, ErrAgentVaultKeyEnrollmentUnavailable) {
		t.Fatalf("read after delete = %v", err)
	}
	if err := DeleteAgentVaultKeyEnrollmentAfterConsume("default", "default", "scott", localEnrollmentRequestID); err != nil {
		t.Fatalf("delete retry = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(home, "keys", "accounts", "default")); err != nil {
		t.Fatalf("delete removed broader key scope: %v", err)
	}
}

func TestAgentVaultKeyEnrollmentRejectsSymlinksAndUnsafeModes(t *testing.T) {
	t.Run("intermediate symlink", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("WITSELF_HOME", home)
		outside := t.TempDir()
		if err := os.Mkdir(filepath.Join(home, "keys"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(home, "keys", "accounts")); err != nil {
			t.Fatal(err)
		}
		recipient, pairing, _ := newLocalEnrollmentMaterial(t, localEnrollmentRequestID, localEnrollmentExpiresAt)
		err := CreateAgentVaultKeyEnrollmentPreflight("default", "default", "scott", localEnrollmentRequestID, recipient, pairing)
		if !errors.Is(err, ErrAgentVaultKeyEnrollmentUnsafe) {
			t.Fatalf("symlink create error = %v", err)
		}
		entries, err := os.ReadDir(outside)
		if err != nil || len(entries) != 0 {
			t.Fatalf("symlink target changed: %#v, %v", entries, err)
		}
	})

	t.Run("private file symlink", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("WITSELF_HOME", home)
		directory, _ := AgentVaultKeyEnrollmentPath("default", "default", "scott", localEnrollmentRequestID)
		makeLocalEnrollmentDirectories(t, home, directory)
		target := filepath.Join(t.TempDir(), "target")
		if err := os.WriteFile(target, []byte("unchanged"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(directory, agentVaultKeyEnrollmentPrivateFile)); err != nil {
			t.Fatal(err)
		}
		recipient, pairing, _ := newLocalEnrollmentMaterial(t, localEnrollmentRequestID, localEnrollmentExpiresAt)
		if err := CreateAgentVaultKeyEnrollmentPreflight("default", "default", "scott", localEnrollmentRequestID, recipient, pairing); !errors.Is(err, ErrAgentVaultKeyEnrollmentUnsafe) {
			t.Fatalf("symlink create error = %v", err)
		}
		raw, _ := os.ReadFile(target)
		if string(raw) != "unchanged" {
			t.Fatal("symlink target was modified")
		}
	})

	t.Run("request file symlink", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("WITSELF_HOME", home)
		recipient, pairing, request := newLocalEnrollmentMaterial(t, localEnrollmentRequestID, localEnrollmentExpiresAt)
		if err := CreateAgentVaultKeyEnrollmentPreflight("default", "default", "scott", localEnrollmentRequestID, recipient, pairing); err != nil {
			t.Fatal(err)
		}
		directory, _ := AgentVaultKeyEnrollmentPath("default", "default", "scott", localEnrollmentRequestID)
		if err := os.Symlink(filepath.Join(t.TempDir(), "missing"), filepath.Join(directory, agentVaultKeyEnrollmentRequestFile)); err != nil {
			t.Fatal(err)
		}
		if err := FinalizeAgentVaultKeyEnrollmentRequest("default", "default", "scott", localEnrollmentRequestID, request); !errors.Is(err, ErrAgentVaultKeyEnrollmentUnsafe) {
			t.Fatalf("request symlink finalize error = %v", err)
		}
	})

	t.Run("file mode", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("WITSELF_HOME", home)
		recipient, pairing, _ := newLocalEnrollmentMaterial(t, localEnrollmentRequestID, localEnrollmentExpiresAt)
		if err := CreateAgentVaultKeyEnrollmentPreflight("default", "default", "scott", localEnrollmentRequestID, recipient, pairing); err != nil {
			t.Fatal(err)
		}
		directory, _ := AgentVaultKeyEnrollmentPath("default", "default", "scott", localEnrollmentRequestID)
		if err := os.Chmod(filepath.Join(directory, agentVaultKeyEnrollmentPrivateFile), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadAgentVaultKeyEnrollmentState("default", "default", "scott", localEnrollmentRequestID); !errors.Is(err, ErrAgentVaultKeyEnrollmentUnsafe) {
			t.Fatalf("unsafe mode read error = %v", err)
		}
	})
}

func TestAgentVaultKeyEnrollmentRejectsCorruptionAndCrossScopeCopy(t *testing.T) {
	t.Run("corrupt canonical record", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("WITSELF_HOME", home)
		recipient, pairing, _ := newLocalEnrollmentMaterial(t, localEnrollmentRequestID, localEnrollmentExpiresAt)
		if err := CreateAgentVaultKeyEnrollmentPreflight("default", "default", "scott", localEnrollmentRequestID, recipient, pairing); err != nil {
			t.Fatal(err)
		}
		directory, _ := AgentVaultKeyEnrollmentPath("default", "default", "scott", localEnrollmentRequestID)
		path := filepath.Join(directory, agentVaultKeyEnrollmentPrivateFile)
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		raw[len(raw)-2] = differentJSONByte(raw[len(raw)-2])
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadAgentVaultKeyEnrollmentState("default", "default", "scott", localEnrollmentRequestID); !errors.Is(err, ErrAgentVaultKeyEnrollmentInvalid) {
			t.Fatalf("corrupt read error = %v", err)
		}
	})

	t.Run("noncanonical json", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("WITSELF_HOME", home)
		recipient, pairing, _ := newLocalEnrollmentMaterial(t, localEnrollmentRequestID, localEnrollmentExpiresAt)
		if err := CreateAgentVaultKeyEnrollmentPreflight("default", "default", "scott", localEnrollmentRequestID, recipient, pairing); err != nil {
			t.Fatal(err)
		}
		directory, _ := AgentVaultKeyEnrollmentPath("default", "default", "scott", localEnrollmentRequestID)
		path := filepath.Join(directory, agentVaultKeyEnrollmentPrivateFile)
		raw, _ := os.ReadFile(path)
		raw = append(raw, '\n')
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadAgentVaultKeyEnrollmentState("default", "default", "scott", localEnrollmentRequestID); !errors.Is(err, ErrAgentVaultKeyEnrollmentInvalid) {
			t.Fatalf("noncanonical read error = %v", err)
		}
	})

	t.Run("cross-agent copy", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("WITSELF_HOME", home)
		recipient, pairing, _ := newLocalEnrollmentMaterial(t, localEnrollmentRequestID, localEnrollmentExpiresAt)
		if err := CreateAgentVaultKeyEnrollmentPreflight("default", "default", "scott", localEnrollmentRequestID, recipient, pairing); err != nil {
			t.Fatal(err)
		}
		source, _ := AgentVaultKeyEnrollmentPath("default", "default", "scott", localEnrollmentRequestID)
		target, _ := AgentVaultKeyEnrollmentPath("default", "default", "bob", localEnrollmentRequestID)
		makeLocalEnrollmentDirectories(t, home, target)
		raw, _ := os.ReadFile(filepath.Join(source, agentVaultKeyEnrollmentPrivateFile))
		if err := os.WriteFile(filepath.Join(target, agentVaultKeyEnrollmentPrivateFile), raw, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadAgentVaultKeyEnrollmentState("default", "default", "bob", localEnrollmentRequestID); !errors.Is(err, ErrAgentVaultKeyEnrollmentInvalid) {
			t.Fatalf("cross-scope read error = %v", err)
		}
	})
}

func newLocalEnrollmentMaterial(t testing.TB, requestID string, expiresAt int64) (*sealed.AVKEnrollmentRecipientKey, *sealed.AVKEnrollmentPairingSecret, sealed.AVKEnrollmentRequest) {
	t.Helper()
	return newLocalEnrollmentMaterialWithExistingSecrets(t, nil, nil, requestID, expiresAt)
}

func newLocalEnrollmentMaterialWithExistingSecrets(t testing.TB, recipient *sealed.AVKEnrollmentRecipientKey, pairing *sealed.AVKEnrollmentPairingSecret, requestID string, expiresAt int64) (*sealed.AVKEnrollmentRecipientKey, *sealed.AVKEnrollmentPairingSecret, sealed.AVKEnrollmentRequest) {
	t.Helper()
	var err error
	if recipient == nil {
		recipient, err = sealed.GenerateAVKEnrollmentRecipientKey()
		if err != nil {
			t.Fatal(err)
		}
	}
	if pairing == nil {
		pairing, err = sealed.GenerateAVKEnrollmentPairingSecret()
		if err != nil {
			t.Fatal(err)
		}
	}
	request, err := sealed.NewAVKEnrollmentRequest(recipient, pairing, sealed.AVKEnrollmentRequestOptions{
		AccountID:            "acc_aaaaaaaaaaaaaaaa",
		RealmID:              "realm_aaaaaaaaaaaaaaaa",
		OwnerAgentID:         "agent_aaaaaaaaaaaaaaaa",
		EnrollmentRequestID:  requestID,
		TargetInstallationID: "loc_aaaaaaaaaaaaaaaa",
		ExpiresAt:            expiresAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	return recipient, pairing, request
}

func assertLocalEnrollmentPrivateLayout(t testing.TB, home, directory, path string) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("state file mode = %v, want regular 0600", info.Mode())
	}
	for _, component := range agentVaultKeyEnrollmentDirectories(home, directory) {
		info, err := os.Lstat(component)
		if err != nil {
			t.Fatal(err)
		}
		if !privateAgentVaultKeyEnrollmentDirectory(info) {
			t.Fatalf("directory %q mode = %v, want real 0700", component, info.Mode())
		}
	}
}

func makeLocalEnrollmentDirectories(t testing.TB, home, directory string) {
	t.Helper()
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range agentVaultKeyEnrollmentDirectories(home, directory) {
		if err := os.Chmod(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
}

func differentJSONByte(value byte) byte {
	if value == 'a' {
		return 'b'
	}
	return 'a'
}

const (
	localEnrollmentRequestID       = "enr_aaaaaaaaaaaaaaaa"
	localEnrollmentExpiresAt int64 = 1_800_000_000
)
