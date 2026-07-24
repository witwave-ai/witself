package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/local"
	"github.com/witwave-ai/witself/internal/sealed"
	"github.com/witwave-ai/witself/internal/secretclient"
)

func TestSecretCreateDocumentRejectsDuplicateFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret.json")
	raw := `{"name":"first","name":"second","fields":[]}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	_, loaded, err := readSecretCreateDocument(path, false)
	if loaded != nil {
		defer clear(loaded)
	}
	if err == nil || strings.Contains(err.Error(), "first") || strings.Contains(err.Error(), "second") {
		t.Fatalf("duplicate error = %v", err)
	}
}

func TestSecretCreateFieldConversionHandlesPublicGeneratedAndTOTP(t *testing.T) {
	public := false
	value := "scott"
	generated := secretCreateFieldDocument{
		Name: "password", Kind: "password", GeneratePassword: true,
		PasswordPolicy: &secretPasswordPolicyDocument{Length: 48},
	}
	otpauth := "otpauth://totp/GitHub:scott%40example.com?secret=JBSWY3DPEHPK3PXP&issuer=GitHub"
	fields, err := toSecretClientFields([]secretCreateFieldDocument{
		{Name: "username", Kind: "username", Sensitive: &public, Value: &value},
		generated,
		{Name: "two_factor", Kind: "totp", OTPAuthURI: &otpauth},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer clearSecretClientFields(fields)
	if fields[0].Sensitive || string(fields[0].Value) != "scott" {
		t.Fatalf("public field = %#v", fields[0])
	}
	if !fields[1].Sensitive || len(fields[1].Value) != 48 || fields[1].Encoding != sealed.ValueEncodingUTF8 {
		t.Fatalf("generated field shape = sensitive %t length %d encoding %q", fields[1].Sensitive, len(fields[1].Value), fields[1].Encoding)
	}
	payload, err := sealed.ParseTOTPPayload(fields[2].Value)
	if err != nil || payload.Metadata().Issuer != "GitHub" || fields[2].Encoding != sealed.ValueEncodingJSON {
		t.Fatalf("TOTP field = metadata %+v encoding %q error %v", payload.Metadata(), fields[2].Encoding, err)
	}
}

func TestSecretCreateRequiresExplicitIdempotencyKey(t *testing.T) {
	stdout, stderr, code := captureFactDeleteCLI(t, func() int {
		return run([]string{"secret", "create", "--file", filepath.Join(t.TempDir(), "not-read.json")})
	})
	if code != 2 || stdout != "" || !strings.Contains(stderr, "--idempotency-key KEY") {
		t.Fatalf("create code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestSecretCommandHelpIsSuccessfulAndSideEffectFree(t *testing.T) {
	commands := [][]string{
		{"secret", "--help"},
		{"secret", "create", "--help"},
		{"secret", "status", "--help"},
		{"secret", "list", "--help"},
		{"secret", "search", "--help"},
		{"secret", "show", "--help"},
		{"secret", "reveal", "--help"},
		{"secret", "archive", "--help"},
		{"secret", "restore", "--help"},
		{"secret", "delete", "--help"},
		{"vault", "--help"},
		{"vault", "key", "--help"},
		{"vault", "key", "init", "--help"},
		{"vault", "key", "status", "--help"},
		{"vault", "key", "enroll", "--help"},
		{"vault", "key", "enroll", "begin", "--help"},
		{"vault", "key", "enroll", "approve", "--help"},
		{"vault", "key", "enroll", "complete", "--help"},
		{"vault", "key", "enroll", "list", "--help"},
		{"vault", "key", "enroll", "status", "--help"},
		{"vault", "key", "enroll", "cancel", "--help"},
		{"vault", "key", "recovery", "--help"},
		{"vault", "key", "recovery", "export", "--help"},
		{"vault", "key", "recovery", "inspect", "--help"},
		{"vault", "key", "recovery", "import", "--help"},
		{"vault", "key", "rotate", "--help"},
		{"vault", "key", "rotation", "--help"},
		{"totp", "--help"},
		{"totp", "show", "--help"},
		{"totp", "code", "--help"},
		{"password", "--help"},
		{"password", "generate", "--help"},
	}
	for _, args := range commands {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			stdout, stderr, code := captureFactDeleteCLI(t, func() int { return run(args) })
			if code != 0 || stdout != "" || !strings.Contains(strings.ToLower(stderr), "usage:") {
				t.Fatalf("run(%q) = %d stdout=%q stderr=%q", args, code, stdout, stderr)
			}
		})
	}
}

func TestSecretDeleteExactRetryBypassesHiddenTombstoneResolution(t *testing.T) {
	const (
		accountID = "acc_abcdefghijklmnop"
		secretID  = "sec_bbbbbbbbbbbbbbbb"
		retryKey  = "delete-response-loss-retry"
	)
	var requests, unexpected int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodPost ||
			r.URL.Path != "/v1/secrets/"+secretID+":delete" {
			unexpected++
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer witself_agt_delete_test" {
			t.Errorf("authorization = %q", got)
		}
		if got := r.Header.Get("Idempotency-Key"); got != retryKey {
			t.Errorf("idempotency key = %q", got)
		}
		var input client.SecretLifecycleInput
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		if input.ExpectedRowVersion != 7 {
			t.Errorf("expected row version = %d", input.ExpectedRowVersion)
		}
		if requests == 1 {
			http.Error(w, "committed response was lost", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(client.SecretMutationResult{
			Secret: client.Secret{
				ID: secretID, AccountID: accountID,
				RealmID: "realm_abcdefghijklmnop", OwnerAgentID: "agent_abcdefghijklmnop",
				Name: secretID, Template: "generic", Tags: []string{},
				Lifecycle: "deleted", RowVersion: 8,
			},
			Receipt: client.SecretMutationReceipt{
				Operation: "secret_delete", TargetKind: "secret",
				TargetID: secretID, ResultRevision: 8, Replayed: true,
			},
		})
	}))
	defer srv.Close()

	t.Setenv("WITSELF_HOME", t.TempDir())
	if err := local.Save("default", local.Account{ID: accountID}, "operator-token"); err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(t.TempDir(), "agent.token")
	if err := os.WriteFile(tokenFile, []byte("witself_agt_delete_test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{
		"secret", "delete", secretID,
		"--expected-row-version", "7", "--idempotency-key", retryKey,
		"--account", "default", "--realm", "default", "--agent", "scott",
		"--endpoint", srv.URL, "--token-file", tokenFile, "--json",
	}
	stdout, stderr, code := captureFactDeleteCLI(t, func() int { return run(args) })
	if code != 1 || stdout != "" || !strings.Contains(stderr, "502 Bad Gateway") {
		t.Fatalf("first delete code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	stdout, stderr, code = captureFactDeleteCLI(t, func() int { return run(args) })
	if code != 0 || stderr != "" ||
		!strings.Contains(stdout, `"lifecycle":"deleted"`) ||
		!strings.Contains(stdout, `"replayed":true`) {
		t.Fatalf("retry delete code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if requests != 2 || unexpected != 0 {
		t.Fatalf("delete retry requests=%d unexpected=%d", requests, unexpected)
	}
}

func TestVaultKeyCredentialsCannotBeSuppliedByFlag(t *testing.T) {
	const canary = "never-place-this-vault-credential-in-argv"
	tests := [][]string{
		{"vault", "key", "enroll", "approve", "enr_abcdefghijklmnop", "--pairing-secret", canary},
		{"vault", "key", "recovery", "export", "--out", filepath.Join(t.TempDir(), "key.recovery"), "--passphrase", canary},
		{"vault", "key", "recovery", "import", "--file", filepath.Join(t.TempDir(), "key.recovery"), "--passphrase", canary},
		{"vault", "key", "rotate", "--recovery-out", filepath.Join(t.TempDir(), "rotated.recovery"), "--passphrase", canary},
	}
	for _, args := range tests {
		stdout, stderr, code := captureFactDeleteCLI(t, func() int { return run(args) })
		if code != 2 || stdout != "" || strings.Contains(stdout, canary) || strings.Contains(stderr, canary) {
			t.Fatalf("run(%q) = %d stdout=%q stderr=%q", args[:len(args)-1], code, stdout, stderr)
		}
	}
}

func TestVaultKeyRotateRequiresExactlyOneRecoveryDecision(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rotated.recovery")
	tests := [][]string{
		{"vault", "key", "rotate"},
		{"vault", "key", "rotate", "--recovery-out", path, "--accept-unrecoverable-key-loss"},
		{"vault", "key", "rotate", "--recovery-out", "   "},
	}
	for _, args := range tests {
		stdout, stderr, code := captureFactDeleteCLI(t, func() int { return run(args) })
		if code != 2 || stdout != "" ||
			!strings.Contains(stderr, "(--recovery-out FILE|--accept-unrecoverable-key-loss)") {
			t.Fatalf("run(%q) = %d stdout=%q stderr=%q", args, code, stdout, stderr)
		}
	}
}

func TestVaultKeyRotationFileRecoverySinkIsDurableNoReplace(t *testing.T) {
	key, err := sealed.GenerateAgentVaultKey(2)
	if err != nil {
		t.Fatal(err)
	}
	defer key.Clear()
	passphrase := []byte("rotation recovery passphrase for CLI test")
	defer clear(passphrase)
	artifact, err := sealed.ExportAgentVaultKeyRecovery(key, passphrase, sealed.AVKRecoveryScope{
		AccountID: "acc_abcdefghijklmnop", RealmID: "realm_abcdefghijklmnop",
		OwnerAgentID: "agent_abcdefghijklmnop",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer clear(artifact)
	metadata, err := sealed.InspectAgentVaultKeyRecovery(artifact)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sink := vaultKeyRotationFileRecoverySink{path: filepath.Join(parent, "rotation.recovery")}
	if value, err := sink.ReadBack(context.Background()); value != nil ||
		!errors.Is(err, secretclient.ErrVaultKeyRotationRecoveryUnavailable) {
		t.Fatalf("missing read = %q, %v", value, err)
	}
	if err := sink.PutIfAbsent(context.Background(), metadata, artifact); err != nil {
		t.Fatal(err)
	}
	read, err := sink.ReadBack(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer clear(read)
	if !bytes.Equal(read, artifact) {
		t.Fatal("file recovery sink changed durable artifact bytes")
	}
	if err := sink.PutIfAbsent(context.Background(), metadata, artifact); !errors.Is(err, secretclient.ErrVaultKeyRotationRecoveryExists) {
		t.Fatalf("second put error = %v", err)
	}
}

func TestPrintVaultKeyRotationShowsOnlyCommittedRecoveryDisposition(t *testing.T) {
	committed := &client.VaultKeyRotation{
		ID: "vkr_abcdefghijklmnop", LifecycleState: client.VaultKeyRotationCommitted,
		SourceKeyID: "avk_abcdefghijklmnop", SourceKeyVersion: 1,
		TargetKeyID: "avk_ponmlkjihgfedcba", TargetKeyVersion: 2,
		ItemCount: 3, StagedCount: 3, RowVersion: 4,
		RecoveryDispositionMode: client.VaultKeyRotationRecoveryArtifact,
		RecoveryArtifactSHA256:  strings.Repeat("a", 64),
	}
	stdout, stderr, code := captureFactDeleteCLI(t, func() int {
		return printVaultKeyRotation("status", committed, false)
	})
	if code != 0 || stderr != "" ||
		!strings.Contains(stdout, "recovery disposition:\trecovery_artifact") ||
		!strings.Contains(stdout, "recovery artifact sha256:\t"+strings.Repeat("a", 64)) {
		t.Fatalf("committed output code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}

	open := *committed
	open.LifecycleState = client.VaultKeyRotationOpen
	open.RecoveryDispositionMode = ""
	open.RecoveryArtifactSHA256 = ""
	stdout, stderr, code = captureFactDeleteCLI(t, func() int {
		return printVaultKeyRotation("status", &open, false)
	})
	if code != 0 || stderr != "" || strings.Contains(stdout, "recovery disposition") ||
		strings.Contains(stdout, "recovery artifact sha256") {
		t.Fatalf("open output code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestVaultKeyRecoveryInspectIsOfflineAndValueFree(t *testing.T) {
	key, err := sealed.GenerateAgentVaultKey(7)
	if err != nil {
		t.Fatal(err)
	}
	defer key.Clear()
	passphrase := []byte("long recovery passphrase for test only")
	defer clear(passphrase)
	artifact, err := sealed.ExportAgentVaultKeyRecovery(key, passphrase, sealed.AVKRecoveryScope{
		AccountID: "acc_abcdefghijklmnop", RealmID: "realm_abcdefghijklmnop",
		OwnerAgentID: "agent_abcdefghijklmnop",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer clear(artifact)
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "scott.recovery")
	if err := local.WriteRecoveryArtifact(path, artifact); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := captureFactDeleteCLI(t, func() int {
		return run([]string{"vault", "key", "recovery", "inspect", "--file", path, "--json"})
	})
	if code != 0 || stderr != "" || !strings.Contains(stdout, key.ID()) ||
		strings.Contains(stdout, string(passphrase)) || strings.Contains(stdout, string(artifact)) {
		t.Fatalf("inspect code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestSecretCLIEncryptsBeforeHTTPAndRevealsLocally(t *testing.T) {
	const (
		accountID = "acc_abcdefghijklmnop"
		realmID   = "realm_abcdefghijklmnop"
		agentID   = "agent_abcdefghijklmnop"
		canary    = "cobalt-secret-canary-714"
	)
	var keyBinding *client.VaultKeyBinding
	var created client.CreateSecretInput

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if got := r.Header.Get("Authorization"); got != "Bearer witself_agt_secret_test" {
			t.Errorf("authorization = %q", got)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/self":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version": "witself.v0",
				"identity": client.SelfIdentity{
					AccountID: accountID, RealmID: realmID, AgentID: agentID,
					RealmName: "default", AgentName: "scott",
				},
				"primary_facts": []any{}, "salient_memories": []any{},
				"index": map[string]any{"kinds": []any{}, "tags": []any{}, "counts": map[string]int{}},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/vault/key-epochs/current":
			_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "key_epoch": keyBinding})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/vault/key-epochs":
			var input client.RegisterVaultKeyInput
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
			}
			keyBinding = &client.VaultKeyBinding{
				ID: input.ID, AccountID: accountID, RealmID: realmID, OwnerAgentID: agentID,
				KeyVersion: input.KeyVersion, Algorithm: input.Algorithm,
				Fingerprint: input.Fingerprint, LifecycleState: "current", RowVersion: 1,
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version": "witself.v0", "key_epoch": keyBinding,
				"receipt": client.SecretMutationReceipt{Operation: "key_register", TargetKind: "key_epoch", TargetID: input.ID, ResultRevision: 1},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/secrets":
			body := json.NewDecoder(r.Body)
			if err := body.Decode(&created); err != nil {
				t.Error(err)
			}
			rendered, _ := json.Marshal(created)
			if strings.Contains(string(rendered), canary) || strings.Contains(string(rendered), testTOTPSeed) ||
				strings.Contains(string(rendered), "otpauth://") {
				t.Error("plaintext crossed the HTTP boundary")
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version": "witself.v0", "secret": testCreatedSecret(created, accountID, realmID, agentID),
				"receipt": client.SecretMutationReceipt{Operation: "secret_create", TargetKind: "secret", TargetID: created.ID, ResultRevision: 1},
			})
		case r.Method == http.MethodGet && created.ID != "" && r.URL.Path == "/v1/secrets/"+created.ID:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version": "witself.v0", "secret": testCreatedSecret(created, accountID, realmID, agentID),
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/secrets":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version": "witself.v0",
				"items":          []client.Secret{testCreatedSecret(created, accountID, realmID, agentID)},
			})
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/secrets/"+created.ID+"/fields/") &&
			strings.HasSuffix(r.URL.Path, ":access"):
			var selected *client.CreateSecretFieldInput
			for index := range created.Fields {
				path := "/v1/secrets/" + created.ID + "/fields/" + created.Fields[index].ID + ":access"
				if r.URL.Path == path {
					selected = &created.Fields[index]
					break
				}
			}
			if selected == nil || selected.Sealed == nil {
				t.Errorf("unexpected field access: %s", r.URL.Path)
				w.WriteHeader(http.StatusNotFound)
				return
			}
			sealedField := selected.Sealed
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version": "witself.v0",
				"material": client.SecretMaterial{
					SecretID: created.ID, FieldID: selected.ID,
					FieldName: selected.Name, FieldKind: selected.Kind,
					Encoding: selected.Encoding, ValueVersion: selected.ValueVersion,
					EnvelopeVersion: sealedField.EnvelopeVersion, Ciphertext: sealedField.Ciphertext,
					Algorithm: sealedField.Algorithm, AADVersion: sealedField.AADVersion,
					DEK: sealedField.DEK, SecretRevision: 1, FieldRevision: 1,
				},
			})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.RequestURI())
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	home := filepath.Join(t.TempDir(), ".witself")
	t.Setenv("WITSELF_HOME", home)
	if err := local.Save("default", local.Account{ID: accountID}, "operator-token"); err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(t.TempDir(), "scott.token")
	if err := os.WriteFile(tokenFile, []byte("witself_agt_secret_test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	public := false
	otpauthURI := "otpauth://totp/GitHub:scott?secret=" + testTOTPSeed + "&issuer=GitHub"
	document := secretCreateDocument{
		Name: "GitHub", Template: "login", Tags: []string{"github"},
		Fields: []secretCreateFieldDocument{
			{Name: "username", Kind: "username", Sensitive: &public, Value: stringPointer("scott")},
			{Name: "password", Kind: "password", Value: stringPointer(canary)},
			{Name: "two_factor", Kind: "totp", OTPAuthURI: &otpauthURI},
		},
	}
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	documentFile := filepath.Join(t.TempDir(), "github.json")
	if err := os.WriteFile(documentFile, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	clear(raw)

	stdout, stderr, code := captureFactDeleteCLI(t, func() int {
		return run([]string{"secret", "create", "--file", documentFile,
			"--idempotency-key", "create-github-secret-1",
			"--account", "default", "--realm", "default", "--agent", "scott",
			"--endpoint", srv.URL, "--token-file", tokenFile})
	})
	if code != 0 || strings.Contains(stdout, canary) || strings.Contains(stderr, canary) ||
		strings.Contains(stdout, testTOTPSeed) || strings.Contains(stderr, testTOTPSeed) ||
		strings.Contains(stdout, "otpauth://") || strings.Contains(stderr, "otpauth://") {
		t.Fatalf("create code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if keyBinding == nil || len(created.Fields) != 3 || created.Fields[1].Sealed == nil || created.Fields[2].Sealed == nil {
		t.Fatalf("key=%#v created=%#v", keyBinding, created)
	}
	keyPath, err := local.AgentVaultKeyPath("default", "default", "scott")
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(keyPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("key file = %v / %v", info, err)
	}

	stdout, stderr, code = captureFactDeleteCLI(t, func() int {
		return run([]string{"secret", "reveal", created.ID, "password",
			"--account", "default", "--realm", "default", "--agent", "scott",
			"--endpoint", srv.URL, "--token-file", tokenFile})
	})
	if code != 0 || strings.TrimSpace(stdout) != canary || stderr != "" {
		t.Fatalf("reveal code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}

	stdout, stderr, code = captureFactDeleteCLI(t, func() int {
		return run([]string{"totp", "show", "GitHub", "two_factor", "--json",
			"--account", "default", "--realm", "default", "--agent", "scott",
			"--endpoint", srv.URL, "--token-file", tokenFile})
	})
	if code != 0 || stderr != "" || strings.Contains(stdout, testTOTPSeed) || strings.Contains(stdout, otpauthURI) {
		t.Fatalf("TOTP show code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	var metadata sealed.TOTPPayloadMetadata
	if err := json.Unmarshal([]byte(stdout), &metadata); err != nil || metadata.Issuer != "GitHub" || metadata.Account != "scott" {
		t.Fatalf("TOTP metadata = %+v / %v", metadata, err)
	}
}

func TestSecretFlagParseOrderSupportsDocumentedPositionalsFirst(t *testing.T) {
	tests := []struct {
		name            string
		args            []string
		positionalCount int
		want            []string
	}{
		{name: "reveal", args: []string{"github", "password", "--json"}, positionalCount: 2, want: []string{"--json", "github", "password"}},
		{name: "search", args: []string{"github", "--tag", "login"}, positionalCount: 1, want: []string{"--tag", "login", "github"}},
		{name: "flags first unchanged", args: []string{"--json", "github"}, positionalCount: 1, want: []string{"--json", "github"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := secretFlagParseOrder(test.args, test.positionalCount)
			if strings.Join(got, "\x00") != strings.Join(test.want, "\x00") {
				t.Fatalf("order = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestVaultKeyInitRejectsSwappedTokenBeforeLocalOrRemoteKeyUse(t *testing.T) {
	const (
		localAccountID = "acc_abcdefghijklmnop"
		otherAccountID = "acc_ponmlkjihgfedcba"
	)
	var nonIdentityRequests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet || r.URL.Path != "/v1/self" {
			nonIdentityRequests++
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": "witself.v0",
			"identity": client.SelfIdentity{
				AccountID: otherAccountID, RealmID: "realm_abcdefghijklmnop", AgentID: "agent_abcdefghijklmnop",
				RealmName: "default", AgentName: "scott",
			},
			"primary_facts": []any{}, "salient_memories": []any{},
			"index": map[string]any{"kinds": []any{}, "tags": []any{}, "counts": map[string]int{}},
		})
	}))
	defer srv.Close()

	t.Setenv("WITSELF_HOME", t.TempDir())
	if err := local.Save("default", local.Account{ID: localAccountID}, "operator-token"); err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(t.TempDir(), "swapped.token")
	if err := os.WriteFile(tokenFile, []byte("witself_agt_swapped_test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := captureFactDeleteCLI(t, func() int {
		return run([]string{"vault", "key", "init",
			"--account", "default", "--realm", "default", "--agent", "scott",
			"--endpoint", srv.URL, "--token-file", tokenFile})
	})
	if code != 1 || stdout != "" || !strings.Contains(stderr, "authenticated secret identity does not match") {
		t.Fatalf("init code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if nonIdentityRequests != 0 {
		t.Fatalf("swapped token reached %d key endpoint(s)", nonIdentityRequests)
	}
	keyPath, err := local.AgentVaultKeyPath("default", "default", "scott")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatalf("swapped token created or exposed a local key: %v", err)
	}
}

func testCreatedSecret(input client.CreateSecretInput, accountID, realmID, agentID string) client.Secret {
	fields := make([]client.SecretField, len(input.Fields))
	sensitiveCount := 0
	for index, field := range input.Fields {
		fields[index] = client.SecretField{
			ID: field.ID, Name: field.Name, Kind: field.Kind, Sensitive: field.Sensitive,
			Encoding: field.Encoding, ValueVersion: field.ValueVersion, PublicValue: field.PublicValue,
			Redacted: field.Sensitive, RowVersion: 1,
		}
		if field.Sealed != nil {
			fields[index].DEKGeneration = field.Sealed.DEK.Generation
		}
		if field.Sensitive {
			sensitiveCount++
		}
	}
	return client.Secret{
		ID: input.ID, AccountID: accountID, RealmID: realmID, OwnerAgentID: agentID,
		Name: input.Name, Description: input.Description, Template: input.Template,
		Tags: input.Tags, Fields: fields, Lifecycle: "active", RowVersion: 1,
		SensitiveCount: sensitiveCount,
	}
}

func stringPointer(value string) *string { return &value }
