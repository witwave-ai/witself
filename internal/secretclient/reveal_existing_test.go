package secretclient

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/witwave-ai/witself/internal/local"
)

func TestRevealExistingFieldNeverInitializesOrRepairsVault(t *testing.T) {
	for _, state := range []VaultKeyState{VaultKeyStateAbsent, VaultKeyStateLocalOnly, VaultKeyStateBackendOnly, VaultKeyStateMismatch} {
		t.Run(string(state), func(t *testing.T) {
			remote := newFakeRemote()
			service := newTestService(t, remote)
			localKey, backendKey := generateKey(t), generateKey(t)
			defer localKey.Clear()
			defer backendKey.Clear()
			if state == VaultKeyStateLocalOnly || state == VaultKeyStateMismatch {
				createLocalKey(t, localKey)
			}
			if state == VaultKeyStateBackendOnly || state == VaultKeyStateMismatch {
				remote.current = bindingFor(backendKey, remote.identity)
			}
			before := vaultFileSnapshot(t)
			value, err := service.RevealExistingField(context.Background(), "sec_aaaaaaaaaaaaaaaa", testOtherField, "explicit-view")
			defer clear(value)
			if err == nil || value != nil || remote.registerCalls != 0 || remote.accessCalls != 0 {
				t.Fatal("unready vault accessed material or registered a key")
			}
			if state == VaultKeyStateMismatch && !errors.Is(err, ErrKeyMismatch) {
				t.Fatalf("mismatch error=%v", err)
			}
			after := vaultFileSnapshot(t)
			if len(before) != len(after) {
				t.Fatal("view changed local vault files")
			}
			for path, content := range before {
				if !bytes.Equal(content, after[path]) {
					t.Fatal("view modified an existing vault file")
				}
			}
		})
	}
}

func TestRevealExistingFieldDecryptsOnlySelectedPackageWithoutPublishingKey(t *testing.T) {
	remote := newFakeRemote()
	service := newTestService(t, remote)
	result, err := service.Create(context.Background(), CreateInput{Name: "Synthetic TUI credential", Fields: []FieldInput{{Name: "password", Kind: "password", Sensitive: true, Value: []byte("synthetic-tui-value")}}})
	if err != nil {
		t.Fatal(err)
	}
	remote.material = materialFromCreate(remote.created, 0)
	// Removing only the current convenience pointer leaves the epoch installed.
	// Existing-only reveal must not republish that pointer as reconciliation does.
	path, err := local.AgentVaultKeyPath(testAccountName, testRealmName, testAgentName)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	before := vaultFileSnapshot(t)
	registers := remote.registerCalls
	value, err := service.RevealExistingField(context.Background(), result.Secret.ID, result.Secret.Fields[0].ID, "explicit-view")
	defer clear(value)
	if err != nil || !bytes.Equal(value, []byte("synthetic-tui-value")) {
		t.Fatal("matching existing vault failed exact decryption")
	}
	if remote.registerCalls != registers || remote.accessCalls != 1 || remote.accessFieldID != result.Secret.Fields[0].ID {
		t.Fatal("reveal performed unexpected remote work")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("reveal published the current key pointer")
	}
	after := vaultFileSnapshot(t)
	if len(before) != len(after) {
		t.Fatal("reveal changed local file set")
	}
	for path, content := range before {
		if !bytes.Equal(content, after[path]) {
			t.Fatal("reveal changed local file bytes")
		}
	}
	remote.material.FieldKind = "totp"
	if value, err := service.RevealExistingField(context.Background(), result.Secret.ID, result.Secret.Fields[0].ID, "seed-refused"); !errors.Is(err, ErrInvalidInput) || value != nil {
		clear(value)
		t.Fatal("TOTP material was not refused before decryption")
	}
	remote.material.FieldKind = "password"
	remote.material.FieldID = testOtherField
	if value, err := service.RevealExistingField(context.Background(), result.Secret.ID, result.Secret.Fields[0].ID, "wrong-package"); err == nil || value != nil {
		clear(value)
		t.Fatal("wrong field package was accepted")
	}
}

func vaultFileSnapshot(t *testing.T) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	root := os.Getenv("WITSELF_HOME")
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			files[path] = data
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}
