package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/local"
	"github.com/witwave-ai/witself/internal/sealed"
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
			if strings.Contains(string(rendered), canary) {
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
		case r.Method == http.MethodPost && len(created.Fields) == 2 &&
			r.URL.Path == "/v1/secrets/"+created.ID+"/fields/"+created.Fields[1].ID+":access":
			sealedField := created.Fields[1].Sealed
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version": "witself.v0",
				"material": client.SecretMaterial{
					SecretID: created.ID, FieldID: created.Fields[1].ID,
					FieldName: created.Fields[1].Name, FieldKind: created.Fields[1].Kind,
					Encoding: created.Fields[1].Encoding, ValueVersion: created.Fields[1].ValueVersion,
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
	document := secretCreateDocument{
		Name: "GitHub", Template: "login", Tags: []string{"github"},
		Fields: []secretCreateFieldDocument{
			{Name: "username", Kind: "username", Sensitive: &public, Value: stringPointer("scott")},
			{Name: "password", Kind: "password", Value: stringPointer(canary)},
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
	if code != 0 || strings.Contains(stdout, canary) || strings.Contains(stderr, canary) {
		t.Fatalf("create code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if keyBinding == nil || len(created.Fields) != 2 || created.Fields[1].Sealed == nil {
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
