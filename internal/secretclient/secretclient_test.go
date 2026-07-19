package secretclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
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
			key, err := local.ReadAgentVaultKey(testAccountName, testRealmName, testAgentName)
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
		stored := readLocalKey(t)
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
	encodedAVK, err := sealed.EncodeAgentVaultKey(readLocalKey(t))
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

func (f *fakeRemote) createSecret(_ context.Context, in client.CreateSecretInput) (*client.SecretMutationResult, error) {
	f.createCalls++
	f.created = in
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
