package secretclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/local"
	"github.com/witwave-ai/witself/internal/sealed"
)

const (
	testAccountID   = "acc_aaaaaaaaaaaaaaaa"
	testRealmID     = "realm_bbbbbbbbbbbbbbbb"
	testAgentID     = "agent_cccccccccccccccc"
	testAccountName = "primary"
	testRealmName   = "work"
	testAgentName   = "alice"
	testOtherField  = "fld_ffffffffffffffff"
)

var testIdentity = client.SelfIdentity{
	AccountID: testAccountID,
	RealmID:   testRealmID,
	AgentID:   testAgentID,
	RealmName: testRealmName,
	AgentName: testAgentName,
}

func TestReconcileVaultKeyStateMachine(t *testing.T) {
	ctx := context.Background()

	t.Run("both absent creates locally before registration", func(t *testing.T) {
		remote := newFakeRemote()
		service := newTestService(t, remote)
		localExistedAtRegistration := false
		remote.registerCheck = func(in client.RegisterVaultKeyInput) error {
			key, err := local.ReadAgentVaultKeyEpoch(
				testAccountName, testRealmName, testAgentName, in.ID, uint64(in.KeyVersion),
			)
			if err == nil && key.ID() == in.ID && key.Fingerprint() == in.Fingerprint {
				localExistedAtRegistration = true
			}
			return err
		}

		key, err := service.ReconcileVaultKey(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if remote.registerCalls != 1 {
			t.Fatalf("register calls = %d, want 1", remote.registerCalls)
		}
		if !localExistedAtRegistration {
			t.Fatal("registration happened before exclusive local creation")
		}
		if remote.registered.ID != key.ID() || remote.registered.Fingerprint != key.Fingerprint() {
			t.Fatal("registered metadata does not describe returned local key")
		}
		stored := readLocalKeyEpoch(t, key.ID(), key.Version())
		if stored.ID() != key.ID() || stored.Fingerprint() != key.Fingerprint() {
			t.Fatal("registered key was not durably created locally")
		}
	})

	t.Run("local only registers existing metadata", func(t *testing.T) {
		remote := newFakeRemote()
		service := newTestService(t, remote)
		localKey := generateKey(t)
		createLocalKey(t, localKey)

		key, err := service.ReconcileVaultKey(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if remote.registerCalls != 1 || key.ID() != localKey.ID() ||
			key.Fingerprint() != localKey.Fingerprint() || remote.registered.ID != localKey.ID() {
			t.Fatal("reconciliation did not register the existing local key")
		}
		stored := readLocalKey(t)
		if stored.ID() != localKey.ID() || stored.Fingerprint() != localKey.Fingerprint() {
			t.Fatal("local key changed during registration")
		}
	})

	t.Run("backend only fails closed without local creation", func(t *testing.T) {
		remote := newFakeRemote()
		backendKey := generateKey(t)
		remote.current = bindingFor(backendKey, remote.identity)
		service := newTestService(t, remote)

		if _, err := service.ReconcileVaultKey(ctx); !errors.Is(err, ErrKeyUnavailable) {
			t.Fatalf("error = %v, want key_unavailable", err)
		}
		if remote.registerCalls != 0 {
			t.Fatalf("register calls = %d, want 0", remote.registerCalls)
		}
		assertNoLocalKey(t)
	})

	t.Run("mismatch fails closed without overwrite", func(t *testing.T) {
		remote := newFakeRemote()
		service := newTestService(t, remote)
		localKey := generateKey(t)
		backendKey := generateKey(t)
		createLocalKey(t, localKey)
		remote.current = bindingFor(backendKey, remote.identity)

		if _, err := service.ReconcileVaultKey(ctx); !errors.Is(err, ErrKeyMismatch) {
			t.Fatalf("error = %v, want key_mismatch", err)
		}
		if remote.registerCalls != 0 {
			t.Fatalf("register calls = %d, want 0", remote.registerCalls)
		}
		stored := readLocalKey(t)
		if stored.ID() != localKey.ID() || stored.Fingerprint() != localKey.Fingerprint() {
			t.Fatal("mismatched local key was overwritten")
		}
	})

	t.Run("matching state is stable", func(t *testing.T) {
		remote := newFakeRemote()
		service := newTestService(t, remote)
		key := generateKey(t)
		createLocalKey(t, key)
		remote.current = bindingFor(key, remote.identity)

		got, err := service.ReconcileVaultKey(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got.ID() != key.ID() || remote.registerCalls != 0 {
			t.Fatal("matching state was not returned without mutation")
		}
	})

	t.Run("backend read failure never authorizes generation", func(t *testing.T) {
		remote := newFakeRemote()
		remote.currentErr = errors.New("backend unavailable")
		service := newTestService(t, remote)

		if _, err := service.ReconcileVaultKey(ctx); !errors.Is(err, ErrOperation) {
			t.Fatalf("error = %v, want generic operation error", err)
		}
		if remote.registerCalls != 0 {
			t.Fatalf("register calls = %d, want 0", remote.registerCalls)
		}
		assertNoLocalKey(t)
	})

	t.Run("cross-scope backend binding fails before local use", func(t *testing.T) {
		remote := newFakeRemote()
		remote.current = bindingFor(generateKey(t), remote.identity)
		remote.current.OwnerAgentID = "agent_dddddddddddddddd"
		service := newTestService(t, remote)

		if _, err := service.ReconcileVaultKey(ctx); !errors.Is(err, ErrIdentityMismatch) {
			t.Fatalf("error = %v, want identity mismatch", err)
		}
		if remote.registerCalls != 0 {
			t.Fatalf("register calls = %d, want 0", remote.registerCalls)
		}
		assertNoLocalKey(t)
	})
}

func TestVaultKeyStatusIsValueFreeAndReadOnly(t *testing.T) {
	remote := newFakeRemote()
	service := newTestService(t, remote)
	key := generateKey(t)
	createLocalKey(t, key)

	status, err := service.VaultKeyStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.State != VaultKeyStateLocalOnly || !status.LocalPresent || status.BackendPresent || status.Match {
		t.Fatalf("unexpected status: %+v", status)
	}
	if status.LocalMetadata == nil || !reflect.DeepEqual(*status.LocalMetadata, key.Metadata()) {
		t.Fatal("status local metadata mismatch")
	}
	if status.BackendBinding != nil || remote.registerCalls != 0 {
		t.Fatal("status mutated backend state")
	}
	stored := readLocalKey(t)
	if stored.ID() != key.ID() || stored.Fingerprint() != key.Fingerprint() {
		t.Fatal("status mutated local state")
	}
}

func TestIdentityMismatchPrecedesLocalKeyUse(t *testing.T) {
	remote := newFakeRemote()
	remote.identity.AgentName = "mallory"
	service := newTestService(t, remote)

	if _, err := service.ReconcileVaultKey(context.Background()); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("error = %v, want identity mismatch", err)
	}
	if remote.currentCalls != 0 || remote.registerCalls != 0 {
		t.Fatal("identity mismatch reached vault-key endpoints")
	}
	assertNoLocalKey(t)
}

func TestAccountIdentityMismatchPrecedesLocalKeyUse(t *testing.T) {
	remote := newFakeRemote()
	remote.identity.AccountID = "acc_dddddddddddddddd"
	service := newTestService(t, remote)

	if _, err := service.ReconcileVaultKey(context.Background()); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("error = %v, want identity mismatch", err)
	}
	if remote.currentCalls != 0 || remote.registerCalls != 0 {
		t.Fatal("account identity mismatch reached vault-key endpoints")
	}
	assertNoLocalKey(t)
}

func TestNewRequiresImmutableAccountIdentityPin(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	_, err := New(Config{
		Endpoint: "https://example.invalid", Token: "agent-token",
		AccountName: testAccountName, RealmName: testRealmName, AgentName: testAgentName,
	})
	if !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("error = %v, want invalid configuration", err)
	}
}

func TestCreatePublicOnlyDoesNotNeedVaultKey(t *testing.T) {
	remote := newFakeRemote()
	service := newTestService(t, remote)
	publicValue := []byte("alice@example.test")

	result, err := service.Create(context.Background(), CreateInput{
		Name: "Example login",
		Tags: []string{"Work", "login"},
		Fields: []FieldInput{{
			Name: "username", Kind: "username", Value: publicValue,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if remote.currentCalls != 0 || remote.registerCalls != 0 {
		t.Fatal("public-only create consulted AVK state")
	}
	assertNoLocalKey(t)
	if remote.createCalls != 1 || !validGeneratedID(remote.created.ID, "sec") || len(remote.created.Fields) != 1 ||
		!validGeneratedID(remote.created.Fields[0].ID, "fld") || remote.created.Fields[0].Sealed != nil ||
		remote.created.Fields[0].PublicValue == nil || *remote.created.Fields[0].PublicValue != string(publicValue) {
		t.Fatalf("unexpected public create wire shape: %+v", remote.created)
	}
	if result.Secret.Fields[0].PublicValue == nil || *result.Secret.Fields[0].PublicValue != string(publicValue) {
		t.Fatal("public field was not returned")
	}
	if !bytes.Equal(publicValue, []byte("alice@example.test")) {
		t.Fatal("public input was unexpectedly consumed")
	}
}

func TestSensitiveCreateAndRevealStayClientSide(t *testing.T) {
	remote := newFakeRemote()
	service := newTestService(t, remote)
	want := []byte("correct horse battery staple")
	inputValue := append([]byte(nil), want...)

	result, err := service.Create(context.Background(), CreateInput{
		Name: "Production login",
		Fields: []FieldInput{{
			Name: "password", Kind: "password", Sensitive: true, Value: inputValue,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if remote.registerCalls != 1 || remote.createCalls != 1 {
		t.Fatalf("register/create calls = %d/%d, want 1/1", remote.registerCalls, remote.createCalls)
	}
	if !allZero(inputValue) {
		t.Fatal("sensitive input buffer was not cleared")
	}
	if len(remote.created.Fields) != 1 || remote.created.Fields[0].Sealed == nil ||
		remote.created.Fields[0].PublicValue != nil || !remote.created.Fields[0].Sensitive {
		t.Fatalf("unexpected sensitive create wire shape: %+v", remote.created.Fields)
	}
	wireJSON, err := json.Marshal(remote.created)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(wireJSON, want) {
		t.Fatal("plaintext appeared in backend create structure")
	}
	if result.Secret.Fields[0].PublicValue != nil || !result.Secret.Fields[0].Redacted {
		t.Fatal("sensitive create response was not redacted")
	}

	remote.material = materialFromCreate(remote.created, 0)
	plaintext, err := service.RevealField(context.Background(), remote.created.ID,
		remote.created.Fields[0].ID, "reveal-once")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(plaintext, want) {
		t.Fatalf("revealed value = %q, want %q", plaintext, want)
	}
	clear(plaintext)
	if remote.accessCalls != 1 || remote.accessSecretID != remote.created.ID ||
		remote.accessFieldID != remote.created.Fields[0].ID || remote.accessIdempotencyKey != "reveal-once" {
		t.Fatal("reveal did not request exactly one selected field")
	}
}

func TestCreateHandlesAliasedSensitiveInputBuffers(t *testing.T) {
	remote := newFakeRemote()
	service := newTestService(t, remote)
	want := []byte("one shared plaintext buffer")
	shared := append([]byte(nil), want...)

	if _, err := service.Create(context.Background(), CreateInput{
		Name: "Aliased buffers",
		Fields: []FieldInput{
			{Name: "first", Kind: "text", Sensitive: true, Value: shared},
			{Name: "second", Kind: "note", Sensitive: true, Value: shared},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if !allZero(shared) {
		t.Fatal("aliased sensitive input was not cleared after sealing completed")
	}
	for index := range remote.created.Fields {
		material := materialFromCreate(remote.created, index)
		remote.material = material
		plaintext, err := service.RevealField(context.Background(), material.SecretID, material.FieldID, "alias-reveal")
		if err != nil {
			t.Fatalf("field %d reveal: %v", index, err)
		}
		if !bytes.Equal(plaintext, want) {
			t.Fatalf("field %d revealed %q, want %q", index, plaintext, want)
		}
		clear(plaintext)
	}
}

func TestCreateRetryReusesDurableExactSealedRequest(t *testing.T) {
	remote := newFakeRemote()
	service := newTestService(t, remote)
	retryKey := "durable-create-retry"
	want := []byte("journal-sensitive-plaintext-marker")
	firstValue := append([]byte(nil), want...)
	remote.createErr = errors.New("response lost")

	_, err := service.Create(context.Background(), CreateInput{
		Name: "Original operation", IdempotencyKey: retryKey,
		Fields: []FieldInput{{Name: "password", Kind: "password", Sensitive: true, Value: firstValue}},
	})
	if !errors.Is(err, ErrOperation) {
		t.Fatalf("first create error = %v, want generic operation error", err)
	}
	if !allZero(firstValue) {
		t.Fatal("first sensitive input was not cleared")
	}
	firstWire, err := json.Marshal(remote.created)
	if err != nil {
		t.Fatal(err)
	}
	journalHash := createJournalHash(remote.identity, retryKey)
	journalPath, err := local.SecretCreateJournalPath(testAccountName, testRealmName, testAgentName, journalHash)
	if err != nil {
		t.Fatal(err)
	}
	journalRaw, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(journalRaw)
	if bytes.Contains(journalRaw, want) || bytes.Contains(journalRaw, []byte(retryKey)) {
		t.Fatal("journal contains sensitive plaintext or the raw idempotency key")
	}
	encodedAVK, err := sealed.EncodeAgentVaultKey(readLocalKeyEpoch(
		t, remote.current.ID, uint64(remote.current.KeyVersion),
	))
	if err != nil {
		t.Fatal(err)
	}
	defer clear(encodedAVK)
	if bytes.Contains(journalRaw, encodedAVK) {
		t.Fatal("journal contains AVK material")
	}
	info, err := os.Stat(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("journal mode = %v, want 0600", info.Mode().Perm())
	}

	remote.createErr = nil
	secondValue := []byte("a different value must lose")
	result, err := service.Create(context.Background(), CreateInput{
		Name: "Different operation", IdempotencyKey: retryKey,
		Fields: []FieldInput{{Name: "token", Kind: "token", Sensitive: true, Value: secondValue}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !allZero(secondValue) {
		t.Fatal("ignored retry plaintext was not cleared")
	}
	secondWire, err := json.Marshal(remote.created)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstWire, secondWire) {
		t.Fatalf("retry did not reuse exact request\nfirst:  %s\nsecond: %s", firstWire, secondWire)
	}
	if result.Secret.Name != "Original operation" || result.Secret.ID != remote.created.ID {
		t.Fatalf("retry did not preserve original operation: %+v", result.Secret)
	}
	if _, err := os.Stat(journalPath); err != nil {
		t.Fatalf("journal was removed immediately after success: %v", err)
	}
	remote.material = materialFromCreate(remote.created, 0)
	plaintext, err := service.RevealField(context.Background(), remote.created.ID,
		remote.created.Fields[0].ID, "journal-reveal")
	if err != nil || !bytes.Equal(plaintext, want) {
		t.Fatalf("retried envelope revealed %q, %v; want original value", plaintext, err)
	}
	clear(plaintext)
}

func TestCreateJournalRebasesOnlyAfterCommittedRotationMismatch(t *testing.T) {
	remote := newFakeRemote()
	service := newTestService(t, remote)
	source := generateKey(t)
	defer source.Clear()
	createLocalKey(t, source)
	remote.current = bindingFor(source, remote.identity)

	retryKey := "create-across-committed-rotation"
	want := []byte("generated-password-survives-rotation")
	firstValue := append([]byte(nil), want...)
	var requests []client.CreateSecretInput
	remote.createFunc = func(in client.CreateSecretInput) (*client.SecretMutationResult, error) {
		requests = append(requests, cloneCreateRequest(in))
		return nil, errors.New("secret state conflict") // open rotation: never rebase
	}
	if _, err := service.Create(context.Background(), CreateInput{
		Name: "Rotation-raced account", IdempotencyKey: retryKey,
		Fields: []FieldInput{{Name: "password", Kind: "password", Sensitive: true, Value: firstValue}},
	}); !errors.Is(err, ErrOperation) {
		t.Fatalf("create while rotation open error = %v, want ErrOperation", err)
	}
	if !allZero(firstValue) || len(requests) != 1 {
		t.Fatal("open-rotation create did not journal once and clear its plaintext")
	}
	sourceRequest := cloneCreateRequest(requests[0])

	target, err := sealed.GenerateAgentVaultKey(source.Version() + 1)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Clear()
	if err := local.CreateAgentVaultKeyEpoch(testAccountName, testRealmName, testAgentName, target); err != nil {
		t.Fatal(err)
	}
	remote.current = bindingFor(target, remote.identity)
	remote.createFunc = func(in client.CreateSecretInput) (*client.SecretMutationResult, error) {
		requests = append(requests, cloneCreateRequest(in))
		keyID, _, ok := createRequestWrappingEpoch(requestWithoutIdempotency(in))
		if !ok {
			return nil, errors.New("missing wrapping epoch")
		}
		if keyID == source.ID() {
			return nil, client.ErrSecretVaultKeyMismatch
		}
		if keyID != target.ID() {
			return nil, errors.New("unexpected wrapping epoch")
		}
		return createResultForInput(in, remote.identity), nil
	}
	retryValue := []byte("must be ignored")
	result, err := service.Create(context.Background(), CreateInput{
		Name: "Different retry input", IdempotencyKey: retryKey,
		Fields: []FieldInput{{Name: "token", Kind: "token", Sensitive: true, Value: retryValue}},
	})
	if err != nil || result == nil {
		t.Fatalf("create after rotation = %#v, %v", result, err)
	}
	if !allZero(retryValue) || len(requests) != 3 {
		t.Fatalf("retry input/calls = cleared:%v calls:%d, want true/3", allZero(retryValue), len(requests))
	}
	if !reflect.DeepEqual(sourceRequest, requests[1]) {
		t.Fatal("retry did not submit the exact old-epoch journal before rebasing")
	}
	rebased := requests[2]
	if !sameCreateRequestLogicalValue(sourceRequest, rebased) ||
		!bytes.Equal(sourceRequest.Fields[0].Sealed.Ciphertext, rebased.Fields[0].Sealed.Ciphertext) ||
		sourceRequest.ID != rebased.ID || sourceRequest.Fields[0].ID != rebased.Fields[0].ID ||
		sourceRequest.Fields[0].Sealed.DEK.ID != rebased.Fields[0].Sealed.DEK.ID ||
		rebased.Fields[0].Sealed.DEK.WrapRevision != sourceRequest.Fields[0].Sealed.DEK.WrapRevision+1 ||
		rebased.Fields[0].Sealed.DEK.WrappingKeyID != target.ID() ||
		bytes.Equal(sourceRequest.Fields[0].Sealed.DEK.WrappedDEK, rebased.Fields[0].Sealed.DEK.WrappedDEK) {
		t.Fatalf("rebase changed logical value or failed to advance only wrapper: %#v", rebased.Fields[0])
	}
	assertCreateFieldOpens(t, rebased, 0, remote.identity, target, want)
	journalHash := createJournalHash(remote.identity, retryKey)
	durable, found, err := service.readCreateJournal(remote.identity, journalHash, retryKey)
	if err != nil || !found || !reflect.DeepEqual(durable, requestWithoutIdempotency(rebased)) {
		t.Fatalf("durable rebased journal = %#v, %v, %v", durable, found, err)
	}
}

func TestCreateJournalExactCommittedReplayWinsAfterRotation(t *testing.T) {
	remote := newFakeRemote()
	service := newTestService(t, remote)
	source := generateKey(t)
	defer source.Clear()
	createLocalKey(t, source)
	remote.current = bindingFor(source, remote.identity)
	retryKey := "committed-before-rotation-response-lost"
	want := []byte("already-committed-generated-password")
	var committed client.CreateSecretInput
	remote.createFunc = func(in client.CreateSecretInput) (*client.SecretMutationResult, error) {
		committed = cloneCreateRequest(in)
		return nil, errors.New("response lost after commit")
	}
	if _, err := service.Create(context.Background(), CreateInput{
		Name: "Committed account", IdempotencyKey: retryKey,
		Fields: []FieldInput{{Name: "password", Kind: "password", Sensitive: true, Value: append([]byte(nil), want...)}},
	}); !errors.Is(err, ErrOperation) {
		t.Fatalf("first create error = %v", err)
	}
	hash := createJournalHash(remote.identity, retryKey)
	before, err := local.ReadSecretCreateJournal(testAccountName, testRealmName, testAgentName, hash)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(before)
	target, err := sealed.GenerateAgentVaultKey(source.Version() + 1)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Clear()
	if err := local.CreateAgentVaultKeyEpoch(testAccountName, testRealmName, testAgentName, target); err != nil {
		t.Fatal(err)
	}
	remote.current = bindingFor(target, remote.identity)
	currentCalls := remote.currentCalls
	remote.createFunc = func(in client.CreateSecretInput) (*client.SecretMutationResult, error) {
		if !reflect.DeepEqual(committed, in) {
			return nil, errors.New("retry changed committed request")
		}
		return createResultForInput(in, remote.identity), nil // canonical receipt replay
	}
	result, err := service.Create(context.Background(), CreateInput{
		Name: "ignored", IdempotencyKey: retryKey,
		Fields: []FieldInput{{Name: "token", Kind: "token", Sensitive: true, Value: []byte("ignored")}},
	})
	if err != nil || result == nil {
		t.Fatalf("exact replay after rotation = %#v, %v", result, err)
	}
	if remote.currentCalls != currentCalls {
		t.Fatal("exact committed replay unnecessarily reconciled or rebased to current key")
	}
	after, err := local.ReadSecretCreateJournal(testAccountName, testRealmName, testAgentName, hash)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(after)
	if !bytes.Equal(before, after) {
		t.Fatal("committed exact replay replaced its journal")
	}
	assertCreateFieldOpens(t, committed, 0, remote.identity, source, want)
}

func TestCreateJournalDoesNotRebaseOnUnprovenFailures(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "open rotation conflict", err: errors.New("secret state conflict")},
		{name: "transport", err: errors.New("connection reset")},
		{name: "idempotency conflict", err: errors.New("idempotency key was reused")},
	} {
		t.Run(test.name, func(t *testing.T) {
			remote := newFakeRemote()
			service := newTestService(t, remote)
			source := generateKey(t)
			defer source.Clear()
			createLocalKey(t, source)
			remote.current = bindingFor(source, remote.identity)
			remote.createErr = errors.New("initial open rotation conflict")
			retryKey := "no-rebase-" + strings.ReplaceAll(test.name, " ", "-")
			if _, err := service.Create(context.Background(), CreateInput{
				Name: "No unsafe rebase", IdempotencyKey: retryKey,
				Fields: []FieldInput{{Name: "password", Kind: "password", Sensitive: true, Value: []byte("preserved")}},
			}); !errors.Is(err, ErrOperation) {
				t.Fatalf("initial create error = %v", err)
			}
			hash := createJournalHash(remote.identity, retryKey)
			before, err := local.ReadSecretCreateJournal(testAccountName, testRealmName, testAgentName, hash)
			if err != nil {
				t.Fatal(err)
			}
			defer clear(before)
			target, err := sealed.GenerateAgentVaultKey(source.Version() + 1)
			if err != nil {
				t.Fatal(err)
			}
			defer target.Clear()
			if err := local.CreateAgentVaultKeyEpoch(testAccountName, testRealmName, testAgentName, target); err != nil {
				t.Fatal(err)
			}
			remote.current = bindingFor(target, remote.identity)
			remote.createErr = test.err
			currentCalls := remote.currentCalls
			if _, err := service.Create(context.Background(), CreateInput{
				Name: "ignored", IdempotencyKey: retryKey,
				Fields: []FieldInput{{Name: "token", Kind: "token", Sensitive: true, Value: []byte("ignored")}},
			}); !errors.Is(err, ErrOperation) {
				t.Fatalf("retry error = %v, want ErrOperation", err)
			}
			if remote.currentCalls != currentCalls {
				t.Fatal("unproven failure reached current-key reconciliation")
			}
			after, err := local.ReadSecretCreateJournal(testAccountName, testRealmName, testAgentName, hash)
			if err != nil {
				t.Fatal(err)
			}
			defer clear(after)
			if !bytes.Equal(before, after) {
				t.Fatal("unproven failure mutated durable journal")
			}
		})
	}
}

func TestCreatePreservesRemoteResultIdentityMismatch(t *testing.T) {
	remote := newFakeRemote()
	service := newTestService(t, remote)
	remote.createFunc = func(in client.CreateSecretInput) (*client.SecretMutationResult, error) {
		result := createResultForInput(in, remote.identity)
		result.Secret.OwnerAgentID = "agent_dddddddddddddddd"
		return result, nil
	}
	result, err := service.Create(context.Background(), CreateInput{
		Name: "Wrong response owner", IdempotencyKey: "identity-mismatch-create",
		Fields: []FieldInput{{Name: "password", Kind: "password", Sensitive: true, Value: []byte("cleared")}},
	})
	if result != nil || !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("result/error = %#v/%v, want nil ErrIdentityMismatch", result, err)
	}
}

func TestCreateJournalRebasedLostResponseReplaysBeforeLaterRotation(t *testing.T) {
	remote := newFakeRemote()
	service := newTestService(t, remote)
	source := generateKey(t)
	defer source.Clear()
	createLocalKey(t, source)
	remote.current = bindingFor(source, remote.identity)
	retryKey := "rebased-commit-response-lost"
	remote.createErr = errors.New("rotation open")
	if _, err := service.Create(context.Background(), CreateInput{
		Name: "Lost rebase response", IdempotencyKey: retryKey,
		Fields: []FieldInput{{Name: "password", Kind: "password", Sensitive: true, Value: []byte("durable-generated-value")}},
	}); !errors.Is(err, ErrOperation) {
		t.Fatalf("initial create error = %v", err)
	}

	target2, err := sealed.GenerateAgentVaultKey(2)
	if err != nil {
		t.Fatal(err)
	}
	defer target2.Clear()
	if err := local.CreateAgentVaultKeyEpoch(testAccountName, testRealmName, testAgentName, target2); err != nil {
		t.Fatal(err)
	}
	remote.current = bindingFor(target2, remote.identity)
	var committedV2 client.CreateSecretInput
	remote.createFunc = func(in client.CreateSecretInput) (*client.SecretMutationResult, error) {
		keyID, _, _ := createRequestWrappingEpoch(requestWithoutIdempotency(in))
		if keyID == source.ID() {
			return nil, client.ErrSecretVaultKeyMismatch
		}
		if keyID == target2.ID() {
			committedV2 = cloneCreateRequest(in)
			return nil, errors.New("response lost after rebased commit")
		}
		return nil, errors.New("unexpected epoch")
	}
	if _, err := service.Create(context.Background(), CreateInput{
		Name: "ignored", IdempotencyKey: retryKey,
		Fields: []FieldInput{{Name: "token", Kind: "token", Sensitive: true, Value: []byte("ignored")}},
	}); !errors.Is(err, ErrOperation) || committedV2.ID == "" {
		t.Fatalf("rebased lost-response create = %#v, %v", committedV2, err)
	}
	hash := createJournalHash(remote.identity, retryKey)
	before, err := local.ReadSecretCreateJournal(testAccountName, testRealmName, testAgentName, hash)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(before)

	target3, err := sealed.GenerateAgentVaultKey(3)
	if err != nil {
		t.Fatal(err)
	}
	defer target3.Clear()
	if err := local.CreateAgentVaultKeyEpoch(testAccountName, testRealmName, testAgentName, target3); err != nil {
		t.Fatal(err)
	}
	remote.current = bindingFor(target3, remote.identity)
	currentCalls := remote.currentCalls
	remote.createFunc = func(in client.CreateSecretInput) (*client.SecretMutationResult, error) {
		if !reflect.DeepEqual(committedV2, in) {
			return nil, errors.New("did not retry exact committed v2 request")
		}
		return createResultForInput(in, remote.identity), nil
	}
	if _, err := service.Create(context.Background(), CreateInput{
		Name: "ignored again", IdempotencyKey: retryKey,
		Fields: []FieldInput{{Name: "token", Kind: "token", Sensitive: true, Value: []byte("ignored again")}},
	}); err != nil {
		t.Fatal(err)
	}
	if remote.currentCalls != currentCalls {
		t.Fatal("committed v2 receipt replay attempted a v3 rebase")
	}
	after, err := local.ReadSecretCreateJournal(testAccountName, testRealmName, testAgentName, hash)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(after)
	if !bytes.Equal(before, after) {
		t.Fatal("exact committed v2 replay changed journal after v3 rotation")
	}
}

func TestCreateJournalCanRebaseAcrossSuccessiveUncommittedEpochs(t *testing.T) {
	remote := newFakeRemote()
	service := newTestService(t, remote)
	source := generateKey(t)
	defer source.Clear()
	createLocalKey(t, source)
	remote.current = bindingFor(source, remote.identity)
	retryKey := "successive-uncommitted-rebases"
	remote.createErr = errors.New("rotation open")
	if _, err := service.Create(context.Background(), CreateInput{
		Name: "Repeated rebase", IdempotencyKey: retryKey,
		Fields: []FieldInput{{Name: "password", Kind: "password", Sensitive: true, Value: []byte("repeat-preserved")}},
	}); !errors.Is(err, ErrOperation) {
		t.Fatal(err)
	}

	keys := []*sealed.AgentVaultKey{source}
	for version := uint64(2); version <= 3; version++ {
		target, err := sealed.GenerateAgentVaultKey(version)
		if err != nil {
			t.Fatal(err)
		}
		defer target.Clear()
		if err := local.CreateAgentVaultKeyEpoch(testAccountName, testRealmName, testAgentName, target); err != nil {
			t.Fatal(err)
		}
		remote.current = bindingFor(target, remote.identity)
		previous := keys[len(keys)-1]
		final := version == 3
		remote.createFunc = func(in client.CreateSecretInput) (*client.SecretMutationResult, error) {
			keyID, _, _ := createRequestWrappingEpoch(requestWithoutIdempotency(in))
			switch keyID {
			case previous.ID():
				return nil, client.ErrSecretVaultKeyMismatch
			case target.ID():
				if final {
					return createResultForInput(in, remote.identity), nil
				}
				return nil, errors.New("definitive response absent; request did not commit")
			default:
				return nil, errors.New("unexpected epoch")
			}
		}
		result, err := service.Create(context.Background(), CreateInput{
			Name: "ignored", IdempotencyKey: retryKey,
			Fields: []FieldInput{{Name: "token", Kind: "token", Sensitive: true, Value: []byte("ignored")}},
		})
		if final && (err != nil || result == nil) {
			t.Fatalf("final v%d rebase = %#v, %v", version, result, err)
		}
		if !final && !errors.Is(err, ErrOperation) {
			t.Fatalf("intermediate v%d rebase error = %v", version, err)
		}
		keys = append(keys, target)
	}
	durable, found, err := service.readCreateJournal(remote.identity, createJournalHash(remote.identity, retryKey), retryKey)
	if err != nil || !found {
		t.Fatalf("durable final journal = %#v, %v, %v", durable, found, err)
	}
	field := durable.Fields[0]
	if field.Sealed.DEK.WrappingKeyVersion != 3 || field.Sealed.DEK.WrapRevision != 3 {
		t.Fatalf("final epoch/revision = %d/%d, want 3/3", field.Sealed.DEK.WrappingKeyVersion, field.Sealed.DEK.WrapRevision)
	}
	assertCreateFieldOpens(t, durable, 0, remote.identity, keys[len(keys)-1], []byte("repeat-preserved"))
}

func TestCreateJournalMissingRetiredSourceKeyFailsWithoutMutation(t *testing.T) {
	remote := newFakeRemote()
	service := newTestService(t, remote)
	source := generateKey(t)
	createLocalKey(t, source)
	remote.current = bindingFor(source, remote.identity)
	retryKey := "missing-retired-create-source"
	remote.createErr = errors.New("rotation open")
	if _, err := service.Create(context.Background(), CreateInput{
		Name: "Missing source", IdempotencyKey: retryKey,
		Fields: []FieldInput{{Name: "password", Kind: "password", Sensitive: true, Value: []byte("preserve-me")}},
	}); !errors.Is(err, ErrOperation) {
		t.Fatal(err)
	}
	hash := createJournalHash(remote.identity, retryKey)
	before, err := local.ReadSecretCreateJournal(testAccountName, testRealmName, testAgentName, hash)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(before)
	target, err := sealed.GenerateAgentVaultKey(2)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Clear()
	if err := local.CreateAgentVaultKeyEpoch(testAccountName, testRealmName, testAgentName, target); err != nil {
		t.Fatal(err)
	}
	remote.current = bindingFor(target, remote.identity)
	epochPath, err := local.AgentVaultKeyEpochPath(testAccountName, testRealmName, testAgentName, source.ID(), source.Version())
	if err != nil {
		t.Fatal(err)
	}
	legacyPath, err := local.AgentVaultKeyPath(testAccountName, testRealmName, testAgentName)
	if err != nil {
		t.Fatal(err)
	}
	source.Clear()
	for _, path := range []string{epochPath, legacyPath} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	createCalls := remote.createCalls
	if _, err := service.Create(context.Background(), CreateInput{
		Name: "ignored", IdempotencyKey: retryKey,
		Fields: []FieldInput{{Name: "token", Kind: "token", Sensitive: true, Value: []byte("ignored")}},
	}); !errors.Is(err, ErrKeyUnavailable) {
		t.Fatalf("retry error = %v, want ErrKeyUnavailable", err)
	}
	if remote.createCalls != createCalls {
		t.Fatal("missing retired source reached backend create")
	}
	after, err := local.ReadSecretCreateJournal(testAccountName, testRealmName, testAgentName, hash)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(after)
	if !bytes.Equal(before, after) {
		t.Fatal("missing retired source changed journal")
	}
}

func TestCreateJournalWrapRevisionOverflowFailsBeforeCAS(t *testing.T) {
	remote := newFakeRemote()
	service := newTestService(t, remote)
	source := generateKey(t)
	defer source.Clear()
	createLocalKey(t, source)
	target, err := sealed.GenerateAgentVaultKey(2)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Clear()
	if err := local.CreateAgentVaultKeyEpoch(testAccountName, testRealmName, testAgentName, target); err != nil {
		t.Fatal(err)
	}
	remote.current = bindingFor(target, remote.identity)
	const secretID = "sec_dddddddddddddddd"
	const fieldID = "fld_eeeeeeeeeeeeeeee"
	envelope, err := sealed.SealSensitiveField(source, []byte("overflow-preserved"), sealed.SensitiveFieldOptions{
		Scope: sealed.FieldScope{
			Domain: sealed.FieldValueDomain, AccountID: testAccountID, RealmID: testRealmID,
			OwnerAgentID: testAgentID, SecretID: secretID, FieldID: fieldID,
		},
		ValueVersion: 1, DEKGeneration: 1, ValueEncoding: sealed.ValueEncodingUTF8,
		WrapRevision: uint64(1<<63 - 1),
	})
	if err != nil {
		t.Fatal(err)
	}
	sealedField, err := toClientSealedField(envelope)
	if err != nil {
		t.Fatal(err)
	}
	request := client.CreateSecretInput{
		ID: secretID, Name: "Overflow fence", Template: "generic",
		Fields: []client.CreateSecretFieldInput{{
			ID: fieldID, Name: "password", Kind: "password", Sensitive: true,
			Encoding: sealed.ValueEncodingUTF8, ValueVersion: 1, Sealed: &sealedField,
		}},
	}
	retryKey := "overflow-rebase-fence"
	hash := createJournalHash(remote.identity, retryKey)
	if _, err := service.publishCreateJournal(remote.identity, hash, retryKey, request); err != nil {
		t.Fatal(err)
	}
	before, err := local.ReadSecretCreateJournal(testAccountName, testRealmName, testAgentName, hash)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(before)
	remote.createFunc = func(client.CreateSecretInput) (*client.SecretMutationResult, error) {
		return nil, client.ErrSecretVaultKeyMismatch
	}
	if _, err := service.Create(context.Background(), CreateInput{
		Name: "ignored", IdempotencyKey: retryKey,
		Fields: []FieldInput{{Name: "token", Kind: "token", Sensitive: true, Value: []byte("ignored")}},
	}); !errors.Is(err, ErrOperation) {
		t.Fatalf("overflow retry error = %v, want ErrOperation", err)
	}
	if remote.createCalls != 1 {
		t.Fatalf("create calls = %d, want exact old request only", remote.createCalls)
	}
	after, err := local.ReadSecretCreateJournal(testAccountName, testRealmName, testAgentName, hash)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(after)
	if !bytes.Equal(before, after) {
		t.Fatal("overflow changed durable journal")
	}
}

func TestCreateJournalCorruptionFailsClosed(t *testing.T) {
	t.Run("noncanonical record", func(t *testing.T) {
		remote := newFakeRemote()
		service := newTestService(t, remote)
		retryKey := "corrupt-create-journal"
		hash := createJournalHash(remote.identity, retryKey)
		if err := local.CreateSecretCreateJournal(testAccountName, testRealmName, testAgentName, hash,
			[]byte(`{"unexpected":true}`)); err != nil {
			t.Fatal(err)
		}

		_, err := service.Create(context.Background(), CreateInput{
			Name: "Must not run", IdempotencyKey: retryKey,
			Fields: []FieldInput{{Name: "username", Kind: "username", Value: []byte("alice")}},
		})
		if !errors.Is(err, ErrIntegrity) {
			t.Fatalf("error = %v, want integrity error", err)
		}
		if remote.currentCalls != 0 || remote.registerCalls != 0 || remote.createCalls != 0 {
			t.Fatal("corrupt journal reached AVK or create operations")
		}
	})

	t.Run("malformed JSON", func(t *testing.T) {
		remote := newFakeRemote()
		service := newTestService(t, remote)
		retryKey := "malformed-create-journal"
		hash := createJournalHash(remote.identity, retryKey)
		path, err := local.SecretCreateJournalPath(testAccountName, testRealmName, testAgentName, hash)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(`{"request":`), 0o600); err != nil {
			t.Fatal(err)
		}

		_, err = service.Create(context.Background(), CreateInput{
			Name: "Must not run", IdempotencyKey: retryKey,
			Fields: []FieldInput{{Name: "username", Kind: "username", Value: []byte("alice")}},
		})
		if !errors.Is(err, ErrIntegrity) {
			t.Fatalf("error = %v, want integrity error", err)
		}
		if remote.createCalls != 0 {
			t.Fatal("malformed journal reached create operation")
		}
	})

	t.Run("authenticated public semantics", func(t *testing.T) {
		remote := newFakeRemote()
		service := newTestService(t, remote)
		retryKey := "public-create-journal-mac"
		remote.createErr = errors.New("response lost")
		input := CreateInput{
			Name: "Original public request", IdempotencyKey: retryKey,
			Fields: []FieldInput{{Name: "username", Kind: "username", Value: []byte("alice")}},
		}
		if _, err := service.Create(context.Background(), input); !errors.Is(err, ErrOperation) {
			t.Fatalf("first create error = %v", err)
		}
		hash := createJournalHash(remote.identity, retryKey)
		path, err := local.SecretCreateJournalPath(testAccountName, testRealmName, testAgentName, hash)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var record createJournalRecord
		if err := json.Unmarshal(raw, &record); err != nil {
			clear(raw)
			t.Fatal(err)
		}
		clear(raw)
		record.Request.Name = "Altered public request"
		raw, err = json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			clear(raw)
			t.Fatal(err)
		}
		clear(raw)

		remote.createErr = nil
		if _, err := service.Create(context.Background(), input); !errors.Is(err, ErrIntegrity) {
			t.Fatalf("tampered retry error = %v, want integrity error", err)
		}
		if remote.createCalls != 1 {
			t.Fatalf("create calls = %d, tampered public request reached backend", remote.createCalls)
		}
	})

	t.Run("invalid sealed-plane type", func(t *testing.T) {
		remote := newFakeRemote()
		service := newTestService(t, remote)
		retryKey := "typed-create-journal"
		hash := createJournalHash(remote.identity, retryKey)
		leak := "must not be public"
		record := createJournalRecord{
			SchemaVersion: createJournalSchema, OperationHash: hash,
			AccountID: testAccountID, RealmID: testRealmID, OwnerAgentID: testAgentID,
			Request: client.CreateSecretInput{
				ID: "sec_dddddddddddddddd", Name: "Invalid cached request", Template: "generic",
				Fields: []client.CreateSecretFieldInput{{
					ID: "fld_eeeeeeeeeeeeeeee", Name: "password", Kind: "password",
					Encoding: sealed.ValueEncodingUTF8, ValueVersion: 1, PublicValue: &leak,
				}},
			},
		}
		raw, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		if err := local.CreateSecretCreateJournal(testAccountName, testRealmName, testAgentName, hash, raw); err != nil {
			t.Fatal(err)
		}

		_, err = service.Create(context.Background(), CreateInput{
			Name: "Must not run", IdempotencyKey: retryKey,
			Fields: []FieldInput{{Name: "username", Kind: "username", Value: []byte("alice")}},
		})
		if !errors.Is(err, ErrIntegrity) {
			t.Fatalf("error = %v, want integrity error", err)
		}
		if remote.currentCalls != 0 || remote.registerCalls != 0 || remote.createCalls != 0 {
			t.Fatal("invalid cached request reached AVK or create operations")
		}
	})
}

func TestCachedCreateAuthenticatesCiphertextAndWrappedDEK(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*createJournalRecord)
	}{
		{
			name: "ciphertext",
			mutate: func(record *createJournalRecord) {
				record.Request.Fields[0].Sealed.Ciphertext[0] ^= 0xff
			},
		},
		{
			name: "wrapped DEK",
			mutate: func(record *createJournalRecord) {
				record.Request.Fields[0].Sealed.DEK.WrappedDEK[0] ^= 0xff
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			remote := newFakeRemote()
			service := newTestService(t, remote)
			retryKey := "tampered-journal-" + test.name
			remote.createErr = errors.New("response lost")
			firstValue := []byte("original sensitive value")
			if _, err := service.Create(context.Background(), CreateInput{
				Name: "Tamper target", IdempotencyKey: retryKey,
				Fields: []FieldInput{{Name: "password", Kind: "password", Sensitive: true, Value: firstValue}},
			}); !errors.Is(err, ErrOperation) {
				t.Fatalf("first create error = %v", err)
			}

			hash := createJournalHash(remote.identity, retryKey)
			path, err := local.SecretCreateJournalPath(testAccountName, testRealmName, testAgentName, hash)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var record createJournalRecord
			if err := json.Unmarshal(raw, &record); err != nil {
				clear(raw)
				t.Fatal(err)
			}
			clear(raw)
			test.mutate(&record)
			record.RequestMAC, err = createRequestMAC(remote.identity, hash, retryKey, record.Request)
			if err != nil {
				t.Fatal(err)
			}
			raw, err = json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				clear(raw)
				t.Fatal(err)
			}
			clear(raw)

			remote.createErr = nil
			retryValue := []byte("ignored retry value")
			result, err := service.Create(context.Background(), CreateInput{
				Name: "Tamper retry", IdempotencyKey: retryKey,
				Fields: []FieldInput{{Name: "token", Kind: "token", Sensitive: true, Value: retryValue}},
			})
			if !errors.Is(err, ErrIntegrity) || result != nil {
				t.Fatalf("result/error = %+v/%v, want nil integrity error", result, err)
			}
			if !allZero(retryValue) {
				t.Fatal("retry plaintext was not cleared")
			}
			if remote.createCalls != 1 {
				t.Fatalf("create calls = %d, tampered cache reached backend", remote.createCalls)
			}
		})
	}
}

func TestConcurrentCreateWithSameKeyPublishesOneExactRequest(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	key := generateKey(t)
	createLocalKey(t, key)
	remote := &concurrentCreateRemote{identity: testIdentity, current: bindingFor(key, testIdentity)}
	services := []*Service{
		{remote: remote, accountID: testAccountID, accountName: testAccountName, realmName: testRealmName, agentName: testAgentName},
		{remote: remote, accountID: testAccountID, accountName: testAccountName, realmName: testRealmName, agentName: testAgentName},
	}
	values := [][]byte{[]byte("concurrent value one"), []byte("concurrent value two")}
	results := make([]*client.SecretMutationResult, len(services))
	errs := make([]error, len(services))
	start := make(chan struct{})
	var wait sync.WaitGroup
	for index := range services {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			results[index], errs[index] = services[index].Create(context.Background(), CreateInput{
				Name: "Concurrent operation", IdempotencyKey: "one-concurrent-key",
				Fields: []FieldInput{{
					Name: "password", Kind: "password", Sensitive: true, Value: values[index],
				}},
			})
		}(index)
	}
	close(start)
	wait.Wait()
	for index := range services {
		if errs[index] != nil {
			t.Fatalf("create %d: %v", index, errs[index])
		}
		if !allZero(values[index]) {
			t.Fatalf("create %d did not clear plaintext", index)
		}
	}
	requests := remote.requestBytes()
	if len(requests) != 2 || !bytes.Equal(requests[0], requests[1]) {
		t.Fatalf("concurrent requests differ: %q / %q", requests[0], requests[1])
	}
	if results[0].Secret.ID != results[1].Secret.ID {
		t.Fatalf("concurrent result IDs differ: %s / %s", results[0].Secret.ID, results[1].Secret.ID)
	}
}

func TestConcurrentCreateRebaseCASContendersConvergeOnWinner(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	source := generateKey(t)
	defer source.Clear()
	createLocalKey(t, source)
	initial := newFakeRemote()
	initial.current = bindingFor(source, initial.identity)
	initial.createErr = errors.New("rotation open")
	initialService := &Service{remote: initial, accountID: testAccountID, accountName: testAccountName, realmName: testRealmName, agentName: testAgentName}
	const retryKey = "concurrent-rebase-cas"
	if _, err := initialService.Create(context.Background(), CreateInput{
		Name: "Concurrent rebase", IdempotencyKey: retryKey,
		Fields: []FieldInput{{Name: "password", Kind: "password", Sensitive: true, Value: []byte("one durable generated value")}},
	}); !errors.Is(err, ErrOperation) {
		t.Fatalf("initial create error = %v", err)
	}
	target, err := sealed.GenerateAgentVaultKey(2)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Clear()
	if err := local.CreateAgentVaultKeyEpoch(testAccountName, testRealmName, testAgentName, target); err != nil {
		t.Fatal(err)
	}
	remote := newConcurrentRebaseRemote(source, target)
	services := []*Service{
		{remote: remote, accountID: testAccountID, accountName: testAccountName, realmName: testRealmName, agentName: testAgentName},
		{remote: remote, accountID: testAccountID, accountName: testAccountName, realmName: testRealmName, agentName: testAgentName},
	}
	values := [][]byte{[]byte("ignored contender one"), []byte("ignored contender two")}
	results := make([]*client.SecretMutationResult, len(services))
	errs := make([]error, len(services))
	start := make(chan struct{})
	var wait sync.WaitGroup
	for index := range services {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			results[index], errs[index] = services[index].Create(context.Background(), CreateInput{
				Name: "ignored", IdempotencyKey: retryKey,
				Fields: []FieldInput{{Name: "token", Kind: "token", Sensitive: true, Value: values[index]}},
			})
		}(index)
	}
	close(start)
	wait.Wait()
	for index := range services {
		if errs[index] != nil || results[index] == nil {
			t.Fatalf("contender %d result/error = %#v/%v", index, results[index], errs[index])
		}
		if !allZero(values[index]) {
			t.Fatalf("contender %d did not clear retry plaintext", index)
		}
	}
	sourceRequests, targetRequests := remote.requestsByEpoch()
	if len(sourceRequests) != 2 || len(targetRequests) != 2 ||
		!sameCreateRequestExact(sourceRequests[0], sourceRequests[1]) ||
		!sameCreateRequestExact(targetRequests[0], targetRequests[1]) {
		t.Fatalf("requests did not converge: source=%d target=%d", len(sourceRequests), len(targetRequests))
	}
	if results[0].Secret.ID != results[1].Secret.ID {
		t.Fatalf("result IDs differ: %s / %s", results[0].Secret.ID, results[1].Secret.ID)
	}
	assertCreateFieldOpens(t, targetRequests[0], 0, testIdentity, target, []byte("one durable generated value"))
}

func TestRevealRejectsTamperAndScopeMismatch(t *testing.T) {
	remote := newFakeRemote()
	service := newTestService(t, remote)
	want := []byte("do not disclose")
	if _, err := service.Create(context.Background(), CreateInput{
		Name:   "Scoped secret",
		Fields: []FieldInput{{Name: "token", Kind: "token", Sensitive: true, Value: append([]byte(nil), want...)}},
	}); err != nil {
		t.Fatal(err)
	}
	original := materialFromCreate(remote.created, 0)

	t.Run("ciphertext tamper", func(t *testing.T) {
		material := cloneMaterial(original)
		material.Ciphertext[0] ^= 0xff
		remote.material = material
		if plaintext, err := service.RevealField(context.Background(), material.SecretID, material.FieldID, "tamper"); !errors.Is(err, ErrIntegrity) || plaintext != nil {
			t.Fatalf("plaintext/error = %q/%v, want nil integrity error", plaintext, err)
		}
	})

	t.Run("authenticated field scope mismatch", func(t *testing.T) {
		material := cloneMaterial(original)
		material.FieldID = testOtherField
		remote.material = material
		if plaintext, err := service.RevealField(context.Background(), material.SecretID, material.FieldID, "scope"); !errors.Is(err, ErrIntegrity) || plaintext != nil {
			t.Fatalf("plaintext/error = %q/%v, want nil integrity error", plaintext, err)
		}
	})
}

func TestTOTPUsesDistinctAuthenticatedDomain(t *testing.T) {
	remote := newFakeRemote()
	service := newTestService(t, remote)
	want := []byte(`{"secret":"JBSWY3DPEHPK3PXP"}`)
	if _, err := service.Create(context.Background(), CreateInput{
		Name: "Authenticator",
		Fields: []FieldInput{{
			Name: "totp", Kind: "totp", Encoding: sealed.ValueEncodingJSON,
			Sensitive: true, Value: append([]byte(nil), want...),
		}},
	}); err != nil {
		t.Fatal(err)
	}
	material := materialFromCreate(remote.created, 0)
	remote.material = material
	plaintext, err := service.RevealField(context.Background(), material.SecretID, material.FieldID, "totp-good")
	if err != nil || !bytes.Equal(plaintext, want) {
		t.Fatalf("TOTP reveal = %q, %v", plaintext, err)
	}
	clear(plaintext)

	// The same envelope cannot be opened under the ordinary field domain.
	material.FieldKind = "password"
	remote.material = material
	if plaintext, err := service.RevealField(context.Background(), material.SecretID, material.FieldID, "totp-domain"); !errors.Is(err, ErrIntegrity) || plaintext != nil {
		t.Fatalf("cross-domain reveal = %q, %v", plaintext, err)
	}
}

func TestListAndGetAlwaysRedactSensitiveFieldsWithoutAVK(t *testing.T) {
	remote := newFakeRemote()
	service := newTestService(t, remote)
	leak := "server-supplied-sensitive-value"
	public := "alice"
	secret := client.Secret{
		ID:           "sec_dddddddddddddddd",
		AccountID:    testAccountID,
		RealmID:      testRealmID,
		OwnerAgentID: testAgentID,
		Tags:         nil,
		Fields: []client.SecretField{
			{ID: "fld_eeeeeeeeeeeeeeee", Sensitive: true, PublicValue: &leak},
			{ID: testOtherField, Kind: "username", Sensitive: false, PublicValue: &public},
			{ID: "fld_gggggggggggggggg", Kind: "password", Sensitive: false, PublicValue: &leak},
		},
	}
	remote.page = &client.SecretPage{Items: []client.Secret{secret}}
	remote.gotSecret = &secret

	page, err := service.List(context.Background(), client.SecretListOptions{IncludeFields: true})
	if err != nil {
		t.Fatal(err)
	}
	assertRedactedView(t, page.Items[0], public)
	got, err := service.Get(context.Background(), "  "+secret.ID+"  ")
	if err != nil {
		t.Fatal(err)
	}
	assertRedactedView(t, *got, public)
	if remote.currentCalls != 0 || remote.registerCalls != 0 {
		t.Fatal("redacted reads consulted AVK state")
	}
	assertNoLocalKey(t)
}

func TestInvalidSensitiveCreateClearsBufferAndReturnsGenericError(t *testing.T) {
	remote := newFakeRemote()
	service := newTestService(t, remote)
	value := []byte("plaintext-marker")
	_, err := service.Create(context.Background(), CreateInput{
		Name:   "Invalid",
		Fields: []FieldInput{{Name: "NOT_VALID", Kind: "password", Sensitive: true, Value: value}},
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("error = %v, want invalid input", err)
	}
	if !allZero(value) || bytes.Contains([]byte(err.Error()), []byte("plaintext-marker")) {
		t.Fatal("invalid create retained plaintext or exposed it in the error")
	}
	if remote.selfCalls != 0 || remote.createCalls != 0 {
		t.Fatal("invalid create reached remote calls")
	}
}

func TestCreateRejectsControlCharactersBeforeJournalPublication(t *testing.T) {
	remote := newFakeRemote()
	service := newTestService(t, remote)
	value := []byte("alice")

	_, err := service.Create(context.Background(), CreateInput{
		Name: "Invalid retry key", IdempotencyKey: "retry\x00key",
		Fields: []FieldInput{{Name: "username", Kind: "username", Value: value}},
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("error = %v, want invalid input", err)
	}
	if remote.selfCalls != 0 || remote.createCalls != 0 {
		t.Fatal("invalid retry key reached remote operations")
	}
	journalRoot, err := local.Home()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(journalRoot, "journal")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid retry key published journal state: %v", err)
	}
}

func TestRevealValidatesRetryKeyBeforeVaultMutation(t *testing.T) {
	remote := newFakeRemote()
	service := newTestService(t, remote)
	tooLong := string(bytes.Repeat([]byte{'x'}, maxIdempotencyBytes+1))

	plaintext, err := service.RevealField(context.Background(), "sec_dddddddddddddddd",
		"fld_eeeeeeeeeeeeeeee", tooLong)
	if !errors.Is(err, ErrInvalidInput) || plaintext != nil {
		t.Fatalf("plaintext/error = %q/%v, want nil invalid input", plaintext, err)
	}
	if remote.selfCalls != 0 || remote.currentCalls != 0 || remote.registerCalls != 0 || remote.accessCalls != 0 {
		t.Fatal("invalid reveal retry key reached identity, key, or material operations")
	}
	assertNoLocalKey(t)
}

type fakeRemote struct {
	identity client.SelfIdentity

	selfErr     error
	currentErr  error
	registerErr error
	createErr   error
	listErr     error
	getErr      error
	accessErr   error

	selfCalls     int
	currentCalls  int
	registerCalls int
	createCalls   int
	listCalls     int
	getCalls      int
	accessCalls   int

	current       *client.VaultKeyBinding
	registerCheck func(client.RegisterVaultKeyInput) error
	createFunc    func(client.CreateSecretInput) (*client.SecretMutationResult, error)
	registered    client.RegisterVaultKeyInput
	created       client.CreateSecretInput
	page          *client.SecretPage
	gotSecret     *client.Secret
	material      *client.SecretMaterial

	accessSecretID       string
	accessFieldID        string
	accessIdempotencyKey string
}

func newFakeRemote() *fakeRemote {
	return &fakeRemote{identity: testIdentity}
}

func (f *fakeRemote) self(context.Context) (client.SelfDigest, error) {
	f.selfCalls++
	if f.selfErr != nil {
		return client.SelfDigest{}, f.selfErr
	}
	return client.SelfDigest{Identity: f.identity}, nil
}

func (f *fakeRemote) currentVaultKey(context.Context) (*client.VaultKeyBinding, error) {
	f.currentCalls++
	if f.currentErr != nil {
		return nil, f.currentErr
	}
	if f.current == nil {
		return nil, nil
	}
	value := *f.current
	return &value, nil
}

func (f *fakeRemote) registerVaultKey(_ context.Context, in client.RegisterVaultKeyInput) (*client.VaultKeyMutationResult, error) {
	f.registerCalls++
	f.registered = in
	if f.registerCheck != nil {
		if err := f.registerCheck(in); err != nil {
			return nil, err
		}
	}
	if f.registerErr != nil {
		return nil, f.registerErr
	}
	binding := client.VaultKeyBinding{
		ID: in.ID, AccountID: f.identity.AccountID, RealmID: f.identity.RealmID,
		OwnerAgentID: f.identity.AgentID, KeyVersion: in.KeyVersion,
		Algorithm: in.Algorithm, Fingerprint: in.Fingerprint, LifecycleState: "current",
	}
	f.current = &binding
	return &client.VaultKeyMutationResult{KeyEpoch: binding}, nil
}

func (f *fakeRemote) createVaultKeyEnrollment(context.Context, client.CreateVaultKeyEnrollmentInput) (*client.VaultKeyEnrollment, error) {
	return nil, errors.New("unexpected enrollment create")
}

func (f *fakeRemote) listVaultKeyEnrollments(context.Context, client.VaultKeyEnrollmentListOptions) ([]client.VaultKeyEnrollment, error) {
	return nil, errors.New("unexpected enrollment list")
}

func (f *fakeRemote) getVaultKeyEnrollment(context.Context, string) (*client.VaultKeyEnrollment, error) {
	return nil, errors.New("unexpected enrollment get")
}

func (f *fakeRemote) approveVaultKeyEnrollment(context.Context, string, client.ApproveVaultKeyEnrollmentInput) (*client.VaultKeyEnrollment, error) {
	return nil, errors.New("unexpected enrollment approve")
}

func (f *fakeRemote) receiveVaultKeyEnrollment(context.Context, string, client.ReceiveVaultKeyEnrollmentInput) (*client.VaultKeyEnrollmentTransfer, error) {
	return nil, errors.New("unexpected enrollment receive")
}

func (f *fakeRemote) consumeVaultKeyEnrollment(context.Context, string, client.ConsumeVaultKeyEnrollmentInput) (*client.VaultKeyEnrollment, error) {
	return nil, errors.New("unexpected enrollment consume")
}

func (f *fakeRemote) cancelVaultKeyEnrollment(context.Context, string, client.CancelVaultKeyEnrollmentInput) (*client.VaultKeyEnrollment, error) {
	return nil, errors.New("unexpected enrollment cancel")
}

func (f *fakeRemote) createSecret(_ context.Context, in client.CreateSecretInput) (*client.SecretMutationResult, error) {
	f.createCalls++
	f.created = in
	if f.createFunc != nil {
		return f.createFunc(in)
	}
	if f.createErr != nil {
		return nil, f.createErr
	}
	return createResultForInput(in, f.identity), nil
}

func createResultForInput(in client.CreateSecretInput, identity client.SelfIdentity) *client.SecretMutationResult {
	fields := make([]client.SecretField, len(in.Fields))
	sensitiveCount := 0
	for index, field := range in.Fields {
		fields[index] = client.SecretField{
			ID: field.ID, Name: field.Name, Kind: field.Kind, Sensitive: field.Sensitive,
			Encoding: field.Encoding, ValueVersion: field.ValueVersion, PublicValue: field.PublicValue,
			Redacted: field.Sensitive,
		}
		if field.Sensitive {
			sensitiveCount++
			fields[index].DEKGeneration = field.Sealed.DEK.Generation
		}
	}
	return &client.SecretMutationResult{Secret: client.Secret{
		ID: in.ID, AccountID: identity.AccountID, RealmID: identity.RealmID,
		OwnerAgentID: identity.AgentID, Name: in.Name, Description: in.Description,
		Template: in.Template, Tags: append([]string(nil), in.Tags...), Fields: fields,
		Lifecycle: "active", SensitiveCount: sensitiveCount,
	}}
}

func (f *fakeRemote) listSecrets(context.Context, client.SecretListOptions) (*client.SecretPage, error) {
	f.listCalls++
	return f.page, f.listErr
}

func (f *fakeRemote) getSecret(_ context.Context, secretID string) (*client.Secret, error) {
	f.getCalls++
	if f.gotSecret != nil && f.gotSecret.ID != secretID {
		return nil, errors.New("unexpected secret id")
	}
	return f.gotSecret, f.getErr
}

func (f *fakeRemote) accessSecretField(_ context.Context, secretID, fieldID, idempotencyKey string) (*client.SecretMaterial, error) {
	f.accessCalls++
	f.accessSecretID = secretID
	f.accessFieldID = fieldID
	f.accessIdempotencyKey = idempotencyKey
	return f.material, f.accessErr
}

type concurrentCreateRemote struct {
	identity client.SelfIdentity
	current  *client.VaultKeyBinding

	mu       sync.Mutex
	requests [][]byte
}

type concurrentRebaseRemote struct {
	*fakeRemote
	sourceID string
	targetID string
	target   *client.VaultKeyBinding

	mu            sync.Mutex
	requests      []client.CreateSecretInput
	canonical     *client.CreateSecretInput
	sourceSeen    int
	sourceBarrier chan struct{}
}

func newConcurrentRebaseRemote(source, target *sealed.AgentVaultKey) *concurrentRebaseRemote {
	base := newFakeRemote()
	return &concurrentRebaseRemote{
		fakeRemote: base, sourceID: source.ID(), targetID: target.ID(),
		target: bindingFor(target, base.identity), sourceBarrier: make(chan struct{}),
	}
}

func (f *concurrentRebaseRemote) self(context.Context) (client.SelfDigest, error) {
	return client.SelfDigest{Identity: f.identity}, nil
}

func (f *concurrentRebaseRemote) currentVaultKey(context.Context) (*client.VaultKeyBinding, error) {
	value := *f.target
	return &value, nil
}

func (f *concurrentRebaseRemote) createSecret(_ context.Context, in client.CreateSecretInput) (*client.SecretMutationResult, error) {
	keyID, _, ok := createRequestWrappingEpoch(requestWithoutIdempotency(in))
	if !ok {
		return nil, errors.New("invalid request epoch")
	}
	f.mu.Lock()
	f.requests = append(f.requests, cloneCreateRequest(in))
	if f.canonical != nil {
		canonical := cloneCreateRequest(*f.canonical)
		f.mu.Unlock()
		if sameCreateRequestExact(canonical, in) {
			return createResultForInput(in, f.identity), nil
		}
		return nil, errors.New("idempotency key was reused")
	}
	if keyID == f.sourceID {
		f.sourceSeen++
		if f.sourceSeen == 2 {
			close(f.sourceBarrier)
		}
		barrier := f.sourceBarrier
		f.mu.Unlock()
		<-barrier
		return nil, client.ErrSecretVaultKeyMismatch
	}
	if keyID != f.targetID {
		f.mu.Unlock()
		return nil, errors.New("unexpected rebase epoch")
	}
	canonical := cloneCreateRequest(in)
	f.canonical = &canonical
	f.mu.Unlock()
	return createResultForInput(in, f.identity), nil
}

func (f *concurrentRebaseRemote) requestsByEpoch() ([]client.CreateSecretInput, []client.CreateSecretInput) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var source, target []client.CreateSecretInput
	for _, request := range f.requests {
		keyID, _, _ := createRequestWrappingEpoch(requestWithoutIdempotency(request))
		switch keyID {
		case f.sourceID:
			source = append(source, cloneCreateRequest(request))
		case f.targetID:
			target = append(target, cloneCreateRequest(request))
		}
	}
	return source, target
}

func (f *concurrentCreateRemote) self(context.Context) (client.SelfDigest, error) {
	return client.SelfDigest{Identity: f.identity}, nil
}

func (f *concurrentCreateRemote) currentVaultKey(context.Context) (*client.VaultKeyBinding, error) {
	value := *f.current
	return &value, nil
}

func (f *concurrentCreateRemote) registerVaultKey(context.Context, client.RegisterVaultKeyInput) (*client.VaultKeyMutationResult, error) {
	return nil, errors.New("unexpected key registration")
}

func (f *concurrentCreateRemote) createVaultKeyEnrollment(context.Context, client.CreateVaultKeyEnrollmentInput) (*client.VaultKeyEnrollment, error) {
	return nil, errors.New("unexpected enrollment create")
}

func (f *concurrentCreateRemote) listVaultKeyEnrollments(context.Context, client.VaultKeyEnrollmentListOptions) ([]client.VaultKeyEnrollment, error) {
	return nil, errors.New("unexpected enrollment list")
}

func (f *concurrentCreateRemote) getVaultKeyEnrollment(context.Context, string) (*client.VaultKeyEnrollment, error) {
	return nil, errors.New("unexpected enrollment get")
}

func (f *concurrentCreateRemote) approveVaultKeyEnrollment(context.Context, string, client.ApproveVaultKeyEnrollmentInput) (*client.VaultKeyEnrollment, error) {
	return nil, errors.New("unexpected enrollment approve")
}

func (f *concurrentCreateRemote) receiveVaultKeyEnrollment(context.Context, string, client.ReceiveVaultKeyEnrollmentInput) (*client.VaultKeyEnrollmentTransfer, error) {
	return nil, errors.New("unexpected enrollment receive")
}

func (f *concurrentCreateRemote) consumeVaultKeyEnrollment(context.Context, string, client.ConsumeVaultKeyEnrollmentInput) (*client.VaultKeyEnrollment, error) {
	return nil, errors.New("unexpected enrollment consume")
}

func (f *concurrentCreateRemote) cancelVaultKeyEnrollment(context.Context, string, client.CancelVaultKeyEnrollmentInput) (*client.VaultKeyEnrollment, error) {
	return nil, errors.New("unexpected enrollment cancel")
}

func (f *concurrentCreateRemote) createSecret(_ context.Context, in client.CreateSecretInput) (*client.SecretMutationResult, error) {
	raw, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.requests = append(f.requests, raw)
	f.mu.Unlock()
	return createResultForInput(in, f.identity), nil
}

func (f *concurrentCreateRemote) listSecrets(context.Context, client.SecretListOptions) (*client.SecretPage, error) {
	return nil, errors.New("unexpected list")
}

func (f *concurrentCreateRemote) getSecret(context.Context, string) (*client.Secret, error) {
	return nil, errors.New("unexpected get")
}

func (f *concurrentCreateRemote) accessSecretField(context.Context, string, string, string) (*client.SecretMaterial, error) {
	return nil, errors.New("unexpected access")
}

func (f *concurrentCreateRemote) requestBytes() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := make([][]byte, len(f.requests))
	for index := range f.requests {
		result[index] = append([]byte(nil), f.requests[index]...)
	}
	return result
}

func newTestService(t *testing.T, remote *fakeRemote) *Service {
	t.Helper()
	t.Setenv("WITSELF_HOME", t.TempDir())
	return &Service{
		remote: remote, accountID: testAccountID,
		accountName: testAccountName, realmName: testRealmName, agentName: testAgentName,
	}
}

func generateKey(t *testing.T) *sealed.AgentVaultKey {
	t.Helper()
	key, err := sealed.GenerateAgentVaultKey(sealed.InitialAgentVaultKeyVersion)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func bindingFor(key *sealed.AgentVaultKey, identity client.SelfIdentity) *client.VaultKeyBinding {
	return &client.VaultKeyBinding{
		ID: key.ID(), AccountID: identity.AccountID, RealmID: identity.RealmID,
		OwnerAgentID: identity.AgentID, KeyVersion: int64(key.Version()),
		Algorithm: key.Algorithm(), Fingerprint: key.Fingerprint(), LifecycleState: "current",
	}
}

func createLocalKey(t *testing.T, key *sealed.AgentVaultKey) {
	t.Helper()
	if err := local.CreateAgentVaultKey(testAccountName, testRealmName, testAgentName, key); err != nil {
		t.Fatal(err)
	}
}

func readLocalKey(t *testing.T) *sealed.AgentVaultKey {
	t.Helper()
	key, err := local.ReadAgentVaultKey(testAccountName, testRealmName, testAgentName)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func readLocalKeyEpoch(t *testing.T, keyID string, keyVersion uint64) *sealed.AgentVaultKey {
	t.Helper()
	key, err := local.ReadAgentVaultKeyEpoch(testAccountName, testRealmName, testAgentName, keyID, keyVersion)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func assertNoLocalKey(t *testing.T) {
	t.Helper()
	if _, err := local.ReadAgentVaultKey(testAccountName, testRealmName, testAgentName); !errors.Is(err, local.ErrAgentVaultKeyUnavailable) {
		t.Fatalf("local key error = %v, want unavailable", err)
	}
}

func materialFromCreate(in client.CreateSecretInput, fieldIndex int) *client.SecretMaterial {
	field := in.Fields[fieldIndex]
	return &client.SecretMaterial{
		SecretID: in.ID, FieldID: field.ID, FieldName: field.Name, FieldKind: field.Kind,
		Encoding: field.Encoding, ValueVersion: field.ValueVersion,
		EnvelopeVersion: field.Sealed.EnvelopeVersion,
		Ciphertext:      append([]byte(nil), field.Sealed.Ciphertext...),
		Algorithm:       field.Sealed.Algorithm, AADVersion: field.Sealed.AADVersion,
		DEK: client.SealedDEK{
			ID: field.Sealed.DEK.ID, Generation: field.Sealed.DEK.Generation,
			WrappedDEK:    append([]byte(nil), field.Sealed.DEK.WrappedDEK...),
			WrapAlgorithm: field.Sealed.DEK.WrapAlgorithm, AADVersion: field.Sealed.DEK.AADVersion,
			WrapRevision: field.Sealed.DEK.WrapRevision, WrappingKeyID: field.Sealed.DEK.WrappingKeyID,
			WrappingKeyVersion: field.Sealed.DEK.WrappingKeyVersion,
		},
	}
}

func requestWithoutIdempotency(in client.CreateSecretInput) client.CreateSecretInput {
	out := cloneCreateRequest(in)
	out.IdempotencyKey = ""
	return out
}

func assertCreateFieldOpens(t *testing.T, in client.CreateSecretInput, fieldIndex int, identity client.SelfIdentity, key *sealed.AgentVaultKey, want []byte) {
	t.Helper()
	field := in.Fields[fieldIndex]
	envelope, err := fromClientSecretMaterial(*materialFromCreate(in, fieldIndex))
	if err != nil {
		t.Fatal(err)
	}
	domain, ok := fieldDomain(field.Kind)
	if !ok {
		t.Fatal("invalid field domain")
	}
	plaintext, err := sealed.OpenSensitiveField(key, sealed.FieldScope{
		Domain: domain, AccountID: identity.AccountID, RealmID: identity.RealmID,
		OwnerAgentID: identity.AgentID, SecretID: in.ID, FieldID: field.ID,
	}, envelope)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(plaintext)
	if !bytes.Equal(plaintext, want) {
		t.Fatalf("opened create field = %q, want %q", plaintext, want)
	}
}

func cloneMaterial(in *client.SecretMaterial) *client.SecretMaterial {
	value := *in
	value.Ciphertext = append([]byte(nil), in.Ciphertext...)
	value.DEK.WrappedDEK = append([]byte(nil), in.DEK.WrappedDEK...)
	return &value
}

func assertRedactedView(t *testing.T, secret client.Secret, public string) {
	t.Helper()
	if secret.Tags == nil || secret.Fields[0].PublicValue != nil || !secret.Fields[0].Redacted ||
		secret.Fields[1].PublicValue == nil || *secret.Fields[1].PublicValue != public ||
		len(secret.Fields) > 2 && (secret.Fields[2].PublicValue != nil || !secret.Fields[2].Redacted) {
		t.Fatalf("unexpected redacted view: %+v", secret)
	}
}

func allZero(value []byte) bool {
	for _, b := range value {
		if b != 0 {
			return false
		}
	}
	return true
}
