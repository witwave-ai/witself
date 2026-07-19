package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const (
	testVaultKeyID = "avk_abcdefghijklmnop"
	testSecretID   = "sec_abcdefghijklmnop"
	testFieldID    = "fld_abcdefghijklmnop"
	testDEKID      = "dek_abcdefghijklmnop"
)

func TestSecretRoutesCiphertextOnlyAndRedacted(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	publicUsername := "octocat"
	shouldNeverRender := "plaintext-password"
	key := VaultKeyBinding{
		ID: testVaultKeyID, AccountID: "acc_1", RealmID: "realm_1", OwnerAgentID: "agent_1",
		KeyVersion: 1, Algorithm: "AES_256_GCM_RANDOM_NONCE_V1",
		Fingerprint: strings.Repeat("a", 64), LifecycleState: "current", RowVersion: 1, CreatedAt: now,
	}
	secret := Secret{
		ID: testSecretID, AccountID: "acc_1", RealmID: "realm_1", OwnerAgentID: "agent_1",
		Name: "github", Template: "login", Tags: []string{"github"}, Lifecycle: "active",
		RowVersion: 1, CreatedAt: now, UpdatedAt: now, SensitiveCount: 1,
		Fields: []SecretField{
			{ID: "fld_bcdefghijklmnopq", Name: "username", Kind: "username", Encoding: "utf8", ValueVersion: 1, PublicValue: &publicUsername, RowVersion: 1},
			{ID: testFieldID, Name: "password", Kind: "password", Sensitive: true, Encoding: "utf8", ValueVersion: 1, PublicValue: &shouldNeverRender, RowVersion: 1},
		},
	}
	receipt := SecretMutationReceipt{Operation: "secret_create", RequestHash: strings.Repeat("b", 64), TargetKind: "secret", TargetID: testSecretID, ResultRevision: 1, CreatedAt: now}
	material := SecretMaterial{
		SecretID: testSecretID, FieldID: testFieldID, FieldName: "password", FieldKind: "password",
		Encoding: "utf8", ValueVersion: 1, EnvelopeVersion: 1,
		Ciphertext: []byte("opaque-ciphertext"), Algorithm: "AES_256_GCM_RANDOM_NONCE_V1", AADVersion: 1,
		DEK:            SealedDEK{ID: testDEKID, Generation: 1, WrappedDEK: []byte("opaque-wrapped-dek"), WrapAlgorithm: "AES_256_GCM_RANDOM_NONCE_V1", AADVersion: 1, WrapRevision: 1, WrappingKeyID: testVaultKeyID, WrappingKeyVersion: 1},
		SecretRevision: 1, FieldRevision: 1,
	}

	var registered RegisterVaultKeyRequest
	var created CreateSecretRequest
	var listed SecretListOptions
	var accessed AccessSecretFieldRequest
	var archivedRequest, restoredRequest SecretLifecycleRequest
	srv := httptest.NewServer(apiMux(Config{
		AuthenticatePrincipal: secretTestAuth,
		GetCurrentVaultKey: func(_ context.Context, p DomainPrincipal) (*VaultKeyBinding, error) {
			assertSecretTestPrincipal(t, p)
			return &key, nil
		},
		RegisterVaultKey: func(_ context.Context, p DomainPrincipal, in RegisterVaultKeyRequest) (VaultKeyMutationResult, error) {
			assertSecretTestPrincipal(t, p)
			registered = in
			return VaultKeyMutationResult{KeyEpoch: key, Receipt: receipt}, nil
		},
		CreateSecret: func(_ context.Context, p DomainPrincipal, in CreateSecretRequest) (SecretMutationResult, error) {
			assertSecretTestPrincipal(t, p)
			created = in
			return SecretMutationResult{Secret: secret, Receipt: receipt}, nil
		},
		ListSecrets: func(_ context.Context, p DomainPrincipal, opts SecretListOptions) (SecretPage, error) {
			assertSecretTestPrincipal(t, p)
			listed = opts
			return SecretPage{Items: []Secret{secret}, NextCursor: "cursor_2"}, nil
		},
		GetSecret: func(_ context.Context, p DomainPrincipal, id string) (Secret, error) {
			assertSecretTestPrincipal(t, p)
			if id != testSecretID {
				t.Fatalf("get secret id = %q", id)
			}
			return secret, nil
		},
		ArchiveSecret: func(_ context.Context, p DomainPrincipal, id string, in SecretLifecycleRequest) (SecretMutationResult, error) {
			assertSecretTestPrincipal(t, p)
			if id != testSecretID {
				t.Fatalf("archive secret id = %q", id)
			}
			archivedRequest = in
			archived := secret
			archived.Lifecycle = "archived"
			archived.RowVersion = 2
			archiveReceipt := receipt
			archiveReceipt.Operation = "secret_archive"
			archiveReceipt.ResultRevision = 2
			return SecretMutationResult{Secret: archived, Receipt: archiveReceipt}, nil
		},
		RestoreSecret: func(_ context.Context, p DomainPrincipal, id string, in SecretLifecycleRequest) (SecretMutationResult, error) {
			assertSecretTestPrincipal(t, p)
			if id != testSecretID {
				t.Fatalf("restore secret id = %q", id)
			}
			restoredRequest = in
			restored := secret
			restored.RowVersion = 3
			restoreReceipt := receipt
			restoreReceipt.Operation = "secret_restore"
			restoreReceipt.ResultRevision = 3
			return SecretMutationResult{Secret: restored, Receipt: restoreReceipt}, nil
		},
		AccessSecretField: func(_ context.Context, p DomainPrincipal, secretID, fieldID string, in AccessSecretFieldRequest) (SecretMaterial, error) {
			assertSecretTestPrincipal(t, p)
			if secretID != testSecretID || fieldID != testFieldID {
				t.Fatalf("access ids = %q / %q", secretID, fieldID)
			}
			accessed = in
			return material, nil
		},
	}))
	defer srv.Close()

	resp := secretTestRequest(t, srv.URL, http.MethodGet, "/v1/vault/key-epochs/current", "agent-token", "", "")
	assertSecretTestResponse(t, resp, http.StatusOK, false)

	registerBody := `{"id":"` + testVaultKeyID + `","key_version":1,"algorithm":"AES_256_GCM_RANDOM_NONCE_V1","fingerprint":"` + strings.Repeat("a", 64) + `"}`
	resp = secretTestRequest(t, srv.URL, http.MethodPost, "/v1/vault/key-epochs", "agent-token", registerBody, "register-1")
	assertSecretTestResponse(t, resp, http.StatusCreated, false)
	if registered.ID != testVaultKeyID || registered.IdempotencyKey != "register-1" {
		t.Fatalf("register callback input = %#v", registered)
	}

	createBody := `{"id":"` + testSecretID + `","name":"github","template":"login","tags":["github"],"fields":[{"id":"fld_bcdefghijklmnopq","name":"username","kind":"username","sensitive":false,"encoding":"utf8","value_version":1,"public_value":"octocat"}]}`
	resp = secretTestRequest(t, srv.URL, http.MethodPost, "/v1/secrets", "agent-token", createBody, "create-1")
	assertSecretTestResponse(t, resp, http.StatusCreated, true)
	if created.ID != testSecretID || created.IdempotencyKey != "create-1" || len(created.Fields) != 1 || created.Fields[0].PublicValue == nil || *created.Fields[0].PublicValue != "octocat" {
		t.Fatalf("create callback input = %#v", created)
	}

	resp = secretTestRequest(t, srv.URL, http.MethodGet, "/v1/secrets?q=git&lifecycle=active&template=login&tag=github&limit=20&cursor=cursor_1&include_fields=true", "agent-token", "", "")
	assertSecretTestResponse(t, resp, http.StatusOK, true)
	if listed.Query != "git" || listed.Lifecycle != "active" || listed.Template != "login" || len(listed.Tags) != 1 || listed.Tags[0] != "github" || listed.Limit != 20 || listed.Cursor != "cursor_1" || !listed.IncludeFields {
		t.Fatalf("list callback options = %#v", listed)
	}

	resp = secretTestRequest(t, srv.URL, http.MethodGet, "/v1/secrets/"+testSecretID, "agent-token", "", "")
	assertSecretTestResponse(t, resp, http.StatusOK, true)

	resp = secretTestRequest(t, srv.URL, http.MethodPost, "/v1/secrets/"+testSecretID+":archive",
		"agent-token", `{"expected_row_version":1}`, "archive-1")
	archiveBody := assertSecretTestResponse(t, resp, http.StatusOK, true)
	if archivedRequest.ExpectedRowVersion != 1 || archivedRequest.IdempotencyKey != "archive-1" {
		t.Fatalf("archive callback input = %#v", archivedRequest)
	}
	var archiveEnvelope SecretMutationResult
	if err := json.Unmarshal(archiveBody, &archiveEnvelope); err != nil ||
		archiveEnvelope.Secret.Lifecycle != "archived" ||
		archiveEnvelope.Receipt.Operation != "secret_archive" {
		t.Fatalf("archive response = %#v / %v", archiveEnvelope, err)
	}

	resp = secretTestRequest(t, srv.URL, http.MethodPost, "/v1/secrets/"+testSecretID+":restore",
		"agent-token", `{"expected_row_version":2}`, "restore-1")
	restoreBody := assertSecretTestResponse(t, resp, http.StatusOK, true)
	if restoredRequest.ExpectedRowVersion != 2 || restoredRequest.IdempotencyKey != "restore-1" {
		t.Fatalf("restore callback input = %#v", restoredRequest)
	}
	var restoreEnvelope SecretMutationResult
	if err := json.Unmarshal(restoreBody, &restoreEnvelope); err != nil ||
		restoreEnvelope.Secret.Lifecycle != "active" ||
		restoreEnvelope.Receipt.Operation != "secret_restore" {
		t.Fatalf("restore response = %#v / %v", restoreEnvelope, err)
	}

	resp = secretTestRequest(t, srv.URL, http.MethodPost, "/v1/secrets/"+testSecretID+"/fields/"+testFieldID+":access", "agent-token", "", "access-1")
	accessBody := assertSecretTestResponse(t, resp, http.StatusOK, false)
	if accessed.IdempotencyKey != "access-1" {
		t.Fatalf("access callback input = %#v", accessed)
	}
	var accessEnvelope struct {
		Material SecretMaterial `json:"material"`
	}
	if err := json.Unmarshal(accessBody, &accessEnvelope); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(accessEnvelope.Material.Ciphertext, material.Ciphertext) || !bytes.Equal(accessEnvelope.Material.DEK.WrappedDEK, material.DEK.WrappedDEK) {
		t.Fatalf("access material = %#v", accessEnvelope.Material)
	}
}

func TestSecretRoutesAuthorizationStrictnessAndErrors(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(apiMux(Config{
		AuthenticatePrincipal: secretTestAuth,
		RegisterVaultKey: func(_ context.Context, _ DomainPrincipal, _ RegisterVaultKeyRequest) (VaultKeyMutationResult, error) {
			calls++
			return VaultKeyMutationResult{}, nil
		},
		CreateSecret: func(_ context.Context, _ DomainPrincipal, _ CreateSecretRequest) (SecretMutationResult, error) {
			calls++
			return SecretMutationResult{}, ErrSecretVaultKeyUnavailable
		},
		ListSecrets: func(_ context.Context, p DomainPrincipal, _ SecretListOptions) (SecretPage, error) {
			calls++
			if p.ID != "agent_1" {
				return SecretPage{}, ErrForbidden
			}
			return SecretPage{}, nil
		},
		GetSecret: func(_ context.Context, p DomainPrincipal, _ string) (Secret, error) {
			calls++
			if p.ID != "agent_1" {
				return Secret{}, ErrForbidden
			}
			return Secret{}, ErrNotFound
		},
		ArchiveSecret: func(_ context.Context, _ DomainPrincipal, _ string, _ SecretLifecycleRequest) (SecretMutationResult, error) {
			calls++
			return SecretMutationResult{}, ErrConflict
		},
		RestoreSecret: func(_ context.Context, _ DomainPrincipal, _ string, _ SecretLifecycleRequest) (SecretMutationResult, error) {
			calls++
			return SecretMutationResult{}, ErrNotFound
		},
		AccessSecretField: func(_ context.Context, _ DomainPrincipal, _, _ string, _ AccessSecretFieldRequest) (SecretMaterial, error) {
			calls++
			return SecretMaterial{}, ErrIdempotencyConflict
		},
	}))
	defer srv.Close()

	tests := []struct {
		name, method, path, token, body, key string
		want                                 int
	}{
		{name: "missing auth", method: http.MethodGet, path: "/v1/secrets", want: http.StatusUnauthorized},
		{name: "operator", method: http.MethodGet, path: "/v1/secrets", token: "operator-token", want: http.StatusForbidden},
		{name: "restricted", method: http.MethodGet, path: "/v1/secrets", token: "curator-token", want: http.StatusForbidden},
		{name: "suspended", method: http.MethodGet, path: "/v1/secrets", token: "suspended-token", want: http.StatusForbidden},
		{name: "cross principal", method: http.MethodGet, path: "/v1/secrets/" + testSecretID, token: "other-agent-token", want: http.StatusForbidden},
		{name: "identity selector", method: http.MethodGet, path: "/v1/secrets?owner_agent_id=agent_2", token: "agent-token", want: http.StatusBadRequest},
		{name: "invalid path", method: http.MethodGet, path: "/v1/secrets/sec_bad", token: "agent-token", want: http.StatusNotFound},
		{name: "unknown create field", method: http.MethodPost, path: "/v1/secrets", token: "agent-token", body: `{"owner_agent_id":"agent_2"}`, key: "create-1", want: http.StatusBadRequest},
		{name: "missing create key", method: http.MethodPost, path: "/v1/secrets", token: "agent-token", body: `{}`, want: http.StatusBadRequest},
		{name: "operator archive", method: http.MethodPost, path: "/v1/secrets/" + testSecretID + ":archive", token: "operator-token", body: `{"expected_row_version":1}`, key: "archive-op", want: http.StatusForbidden},
		{name: "restricted restore", method: http.MethodPost, path: "/v1/secrets/" + testSecretID + ":restore", token: "curator-token", body: `{"expected_row_version":1}`, key: "restore-curator", want: http.StatusForbidden},
		{name: "lifecycle unknown field", method: http.MethodPost, path: "/v1/secrets/" + testSecretID + ":archive", token: "agent-token", body: `{"expected_row_version":1,"owner_agent_id":"agent_2"}`, key: "archive-unknown", want: http.StatusBadRequest},
		{name: "lifecycle missing revision", method: http.MethodPost, path: "/v1/secrets/" + testSecretID + ":archive", token: "agent-token", body: `{}`, key: "archive-no-revision", want: http.StatusBadRequest},
		{name: "lifecycle missing key", method: http.MethodPost, path: "/v1/secrets/" + testSecretID + ":restore", token: "agent-token", body: `{"expected_row_version":1}`, want: http.StatusBadRequest},
		{name: "lifecycle query", method: http.MethodPost, path: "/v1/secrets/" + testSecretID + ":archive?force=true", token: "agent-token", body: `{"expected_row_version":1}`, key: "archive-query", want: http.StatusBadRequest},
		{name: "lifecycle invalid action", method: http.MethodPost, path: "/v1/secrets/" + testSecretID + ":delete", token: "agent-token", body: `{"expected_row_version":1}`, key: "delete-1", want: http.StatusNotFound},
		{name: "key body trailing", method: http.MethodPost, path: "/v1/vault/key-epochs", token: "agent-token", body: `{} {}`, key: "key-1", want: http.StatusBadRequest},
		{name: "access body authority", method: http.MethodPost, path: "/v1/secrets/" + testSecretID + "/fields/" + testFieldID + ":access", token: "agent-token", body: `{"agent_id":"agent_2"}`, key: "access-1", want: http.StatusBadRequest},
		{name: "create missing vault key", method: http.MethodPost, path: "/v1/secrets", token: "agent-token", body: `{}`, key: "create-2", want: http.StatusConflict},
		{name: "missing secret", method: http.MethodGet, path: "/v1/secrets/" + testSecretID, token: "agent-token", want: http.StatusNotFound},
		{name: "archive conflict", method: http.MethodPost, path: "/v1/secrets/" + testSecretID + ":archive", token: "agent-token", body: `{"expected_row_version":1}`, key: "archive-conflict", want: http.StatusConflict},
		{name: "restore missing", method: http.MethodPost, path: "/v1/secrets/" + testSecretID + ":restore", token: "agent-token", body: `{"expected_row_version":1}`, key: "restore-missing", want: http.StatusNotFound},
		{name: "access idempotency conflict", method: http.MethodPost, path: "/v1/secrets/" + testSecretID + "/fields/" + testFieldID + ":access", token: "agent-token", body: `{}`, key: "access-2", want: http.StatusConflict},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resp := secretTestRequest(t, srv.URL, test.method, test.path, test.token, test.body, test.key)
			defer closeBody(t, resp)
			if resp.StatusCode != test.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, test.want)
			}
			if got := resp.Header.Get("Cache-Control"); got != "private, no-store" {
				t.Fatalf("Cache-Control = %q", got)
			}
		})
	}
	if calls != 6 {
		t.Fatalf("callback calls = %d, want 6", calls)
	}
}

func TestSecretCapabilityRequiresCompleteVertical(t *testing.T) {
	auth := secretTestAuth
	complete := Config{
		AuthenticatePrincipal: auth,
		GetCurrentVaultKey:    func(context.Context, DomainPrincipal) (*VaultKeyBinding, error) { return nil, nil },
		RegisterVaultKey: func(context.Context, DomainPrincipal, RegisterVaultKeyRequest) (VaultKeyMutationResult, error) {
			return VaultKeyMutationResult{}, nil
		},
		CreateSecret: func(context.Context, DomainPrincipal, CreateSecretRequest) (SecretMutationResult, error) {
			return SecretMutationResult{}, nil
		},
		ListSecrets: func(context.Context, DomainPrincipal, SecretListOptions) (SecretPage, error) {
			return SecretPage{}, nil
		},
		GetSecret: func(context.Context, DomainPrincipal, string) (Secret, error) { return Secret{}, nil },
		ArchiveSecret: func(context.Context, DomainPrincipal, string, SecretLifecycleRequest) (SecretMutationResult, error) {
			return SecretMutationResult{}, nil
		},
		RestoreSecret: func(context.Context, DomainPrincipal, string, SecretLifecycleRequest) (SecretMutationResult, error) {
			return SecretMutationResult{}, nil
		},
		AccessSecretField: func(context.Context, DomainPrincipal, string, string, AccessSecretFieldRequest) (SecretMaterial, error) {
			return SecretMaterial{}, nil
		},
	}
	for _, test := range []struct {
		name      string
		cfg       Config
		supported bool
	}{{name: "complete", cfg: complete, supported: true}, {name: "partial", cfg: Config{AuthenticatePrincipal: auth, GetSecret: complete.GetSecret}}} {
		t.Run(test.name, func(t *testing.T) {
			srv := httptest.NewServer(apiMux(test.cfg))
			defer srv.Close()
			resp, err := http.Get(srv.URL + "/v1/capabilities")
			if err != nil {
				t.Fatal(err)
			}
			defer closeBody(t, resp)
			var out capabilities
			if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
				t.Fatal(err)
			}
			if got := out.Features["secrets"].Supported; got != test.supported {
				t.Fatalf("secrets supported = %v, want %v", got, test.supported)
			}
		})
	}
}

func TestSecretCreateRequestBodyLimit(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(apiMux(Config{
		AuthenticatePrincipal: secretTestAuth,
		CreateSecret: func(context.Context, DomainPrincipal, CreateSecretRequest) (SecretMutationResult, error) {
			calls++
			return SecretMutationResult{}, nil
		},
	}))
	defer srv.Close()
	body := `{"id":"` + testSecretID + `","name":"` + strings.Repeat("x", int(maxSecretCreateRequestBytes)) + `"}`
	resp := secretTestRequest(t, srv.URL, http.MethodPost, "/v1/secrets", "agent-token", body, "oversized-1")
	defer closeBody(t, resp)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body=%s", resp.StatusCode, secretTestResponseBody(t, resp))
	}
	if calls != 0 {
		t.Fatalf("create callback calls = %d, want 0", calls)
	}
}

func secretTestAuth(_ context.Context, token string) (DomainPrincipal, bool, error) {
	switch token {
	case "agent-token":
		return DomainPrincipal{Kind: PrincipalKindAgent, ID: "agent_1", AccountID: "acc_1", RealmID: "realm_1", AccountStatus: "active", AccessProfile: AccessProfileFull}, true, nil
	case "other-agent-token":
		return DomainPrincipal{Kind: PrincipalKindAgent, ID: "agent_2", AccountID: "acc_1", RealmID: "realm_1", AccountStatus: "active", AccessProfile: AccessProfileFull}, true, nil
	case "operator-token":
		return DomainPrincipal{Kind: PrincipalKindOperator, ID: "operator_1", AccountID: "acc_1", AccountStatus: "active", AccessProfile: AccessProfileFull}, true, nil
	case "curator-token":
		return DomainPrincipal{Kind: PrincipalKindAgent, ID: "agent_1", AccountID: "acc_1", RealmID: "realm_1", AccountStatus: "active", AccessProfile: AccessProfileCuratorApply}, true, nil
	case "suspended-token":
		return DomainPrincipal{Kind: PrincipalKindAgent, ID: "agent_1", AccountID: "acc_1", RealmID: "realm_1", AccountStatus: "suspended", AccessProfile: AccessProfileFull}, true, nil
	default:
		return DomainPrincipal{}, false, nil
	}
}

func assertSecretTestPrincipal(t *testing.T, p DomainPrincipal) {
	t.Helper()
	if p.Kind != PrincipalKindAgent || p.ID != "agent_1" || p.AccountID != "acc_1" || p.RealmID != "realm_1" {
		t.Fatalf("callback principal = %#v", p)
	}
}

func secretTestRequest(t *testing.T, base, method, path, token, body, key string) *http.Response {
	t.Helper()
	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	} else {
		reader = strings.NewReader("")
	}
	req, err := http.NewRequest(method, base+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func assertSecretTestResponse(t *testing.T, resp *http.Response, wantStatus int, assertRedacted bool) []byte {
	t.Helper()
	defer closeBody(t, resp)
	if resp.StatusCode != wantStatus {
		t.Fatalf("status = %d, want %d; body=%s", resp.StatusCode, wantStatus, secretTestResponseBody(t, resp))
	}
	if got := resp.Header.Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
	body := secretTestResponseBody(t, resp)
	if bytes.Contains(body, []byte("plaintext-password")) {
		t.Fatalf("response exposed a sensitive public_value: %s", body)
	}
	if assertRedacted && (!bytes.Contains(body, []byte(`"redacted":true`)) || !bytes.Contains(body, []byte(`"public_value":"octocat"`))) {
		t.Fatalf("response redaction/public projection = %s", body)
	}
	return body
}

func secretTestResponseBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	return body
}
