package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func TestCreateSecretMapsOnlyStableVaultKeyMismatchCode(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		status int
		want   bool
	}{
		{name: "typed mismatch", status: http.StatusConflict, body: `{"schema_version":"witself.v0","code":"secret_vault_key_mismatch","error":"agent vault key mismatch"}`, want: true},
		{name: "code on bad request is not proof", status: http.StatusBadRequest, body: `{"schema_version":"witself.v0","code":"secret_vault_key_mismatch","error":"agent vault key mismatch"}`},
		{name: "code on server error is not proof", status: http.StatusInternalServerError, body: `{"schema_version":"witself.v0","code":"secret_vault_key_mismatch","error":"agent vault key mismatch"}`},
		{name: "message alone is not authority", status: http.StatusConflict, body: `{"schema_version":"witself.v0","error":"agent vault key mismatch"}`},
		{name: "generic conflict", status: http.StatusConflict, body: `{"schema_version":"witself.v0","error":"secret state conflict"}`},
		{name: "idempotency conflict", status: http.StatusConflict, body: `{"schema_version":"witself.v0","error":"idempotency key was reused for a different secret operation"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer srv.Close()
			_, err := CreateSecret(context.Background(), srv.URL, "agent-token", CreateSecretInput{})
			if got := errors.Is(err, ErrSecretVaultKeyMismatch); got != test.want {
				t.Fatalf("errors.Is(..., ErrSecretVaultKeyMismatch) = %v, want %v; error=%v", got, test.want, err)
			}
		})
	}
}

func TestCreateSecretMapsStableLimitCodeAndStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"schema_version":"witself.v0","code":"stored_secret_limit_reached","error":"stored secret limit reached","retryable":false,"limit":{"used":3,"max":2,"remaining":0,"unlimited":false,"over_limit":true}}`))
	}))
	defer srv.Close()
	_, err := CreateSecret(context.Background(), srv.URL, "agent-token", CreateSecretInput{})
	if !errors.Is(err, ErrSecretLimitReached) {
		t.Fatalf("create error = %v", err)
	}
	var limitErr *SecretLimitError
	if !errors.As(err, &limitErr) || limitErr.Status.Used != 3 ||
		limitErr.Status.Max == nil || *limitErr.Status.Max != 2 ||
		!limitErr.Status.OverLimit {
		t.Fatalf("typed limit error = %#v", err)
	}
}

func TestSecretClientVerticalContract(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	publicValue := "octocat"
	maliciousSensitiveProjection := "plaintext-password"
	key := VaultKeyBinding{
		ID: "avk_abcdefghijklmnop", AccountID: "acc_1", RealmID: "realm_1", OwnerAgentID: "agent_1",
		KeyVersion: 1, Algorithm: "AES_256_GCM_RANDOM_NONCE_V1", Fingerprint: "fingerprint",
		LifecycleState: "current", RowVersion: 1, CreatedAt: now,
	}
	secret := Secret{
		ID: "sec_abcdefghijklmnop", AccountID: "acc_1", RealmID: "realm_1", OwnerAgentID: "agent_1",
		Name: "github", Template: "login", Tags: []string{"github"}, Lifecycle: "active",
		RowVersion: 1, CreatedAt: now, UpdatedAt: now, SensitiveCount: 1,
		Fields: []SecretField{
			{ID: "fld_bcdefghijklmnopq", Name: "username", Kind: "username", Encoding: "utf8", ValueVersion: 1, PublicValue: &publicValue},
			{ID: "fld_abcdefghijklmnop", Name: "password", Kind: "password", Sensitive: true, Encoding: "utf8", ValueVersion: 1, PublicValue: &maliciousSensitiveProjection},
		},
	}
	receipt := SecretMutationReceipt{Operation: "secret_create", TargetKind: "secret", TargetID: secret.ID, ResultRevision: 1, CreatedAt: now}
	material := SecretMaterial{
		SecretID: secret.ID, FieldID: "fld_abcdefghijklmnop", FieldName: "password", FieldKind: "password",
		Encoding: "utf8", ValueVersion: 1, EnvelopeVersion: 1, Ciphertext: []byte("ciphertext"),
		Algorithm: "AES_256_GCM_RANDOM_NONCE_V1", AADVersion: 1,
		DEK:            SealedDEK{ID: "dek_abcdefghijklmnop", Generation: 1, WrappedDEK: []byte("wrapped-dek"), WrapAlgorithm: "AES_256_GCM_RANDOM_NONCE_V1", AADVersion: 1, WrapRevision: 1, WrappingKeyID: key.ID, WrappingKeyVersion: 1},
		SecretRevision: 1, FieldRevision: 1,
	}

	calls := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls[r.Method+" "+r.URL.Path]++
		if got := r.Header.Get("Authorization"); got != "Bearer agent-token" {
			t.Fatalf("Authorization = %q", got)
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/vault/key-epochs/current":
			writeSecretClientJSON(t, w, map[string]any{"schema_version": "witself.v0", "key_epoch": key})
		case "POST /v1/vault/key-epochs":
			assertSecretClientIdempotency(t, r, "register-1")
			var body map[string]json.RawMessage
			decodeSecretClientBody(t, r, &body)
			if string(body["id"]) != `"avk_abcdefghijklmnop"` || string(body["key_version"]) != "1" {
				t.Fatalf("register body = %#v", body)
			}
			if _, present := body["idempotency_key"]; present {
				t.Fatal("register body exposed idempotency_key")
			}
			writeSecretClientJSON(t, w, VaultKeyMutationResult{KeyEpoch: key, Receipt: receipt})
		case "POST /v1/secrets":
			assertSecretClientIdempotency(t, r, "create-1")
			var body map[string]json.RawMessage
			decodeSecretClientBody(t, r, &body)
			if string(body["id"]) != `"sec_abcdefghijklmnop"` || string(body["name"]) != `"github"` {
				t.Fatalf("create body = %#v", body)
			}
			if _, present := body["idempotency_key"]; present {
				t.Fatal("create body exposed idempotency_key")
			}
			writeSecretClientJSON(t, w, SecretMutationResult{Secret: secret, Receipt: receipt})
		case "GET /v1/secrets:status":
			maximum, remaining := int64(10), int64(9)
			writeSecretClientJSON(t, w, map[string]any{"schema_version": "witself.v0", "limit": SecretLimitStatus{
				Used: 1, Max: &maximum, Remaining: &remaining,
			}})
		case "GET /v1/secrets":
			query := r.URL.Query()
			if query.Get("q") != "git hub" || query.Get("lifecycle") != "active" || query.Get("template") != "login" ||
				query.Get("limit") != "25" || query.Get("cursor") != "cursor+1" || query.Get("include_fields") != "true" ||
				!reflect.DeepEqual(query["tag"], []string{"github", "work"}) {
				t.Fatalf("list query = %#v", query)
			}
			writeSecretClientJSON(t, w, SecretPage{Items: []Secret{secret}, NextCursor: "cursor_2"})
		case "GET /v1/secrets/sec_abcdefghijklmnop":
			writeSecretClientJSON(t, w, map[string]any{"schema_version": "witself.v0", "secret": secret})
		case "POST /v1/secrets/sec_abcdefghijklmnop:archive":
			assertSecretClientIdempotency(t, r, "archive-1")
			var body map[string]json.RawMessage
			decodeSecretClientBody(t, r, &body)
			if len(body) != 1 || string(body["expected_row_version"]) != "1" {
				t.Fatalf("archive body = %#v", body)
			}
			archived := secret
			archived.Lifecycle = "archived"
			archived.RowVersion = 2
			archiveReceipt := receipt
			archiveReceipt.Operation = "secret_archive"
			archiveReceipt.ResultRevision = 2
			writeSecretClientJSON(t, w, SecretMutationResult{Secret: archived, Receipt: archiveReceipt})
		case "POST /v1/secrets/sec_abcdefghijklmnop:restore":
			assertSecretClientIdempotency(t, r, "restore-1")
			var body map[string]json.RawMessage
			decodeSecretClientBody(t, r, &body)
			if len(body) != 1 || string(body["expected_row_version"]) != "2" {
				t.Fatalf("restore body = %#v", body)
			}
			restored := secret
			restored.RowVersion = 3
			restoreReceipt := receipt
			restoreReceipt.Operation = "secret_restore"
			restoreReceipt.ResultRevision = 3
			writeSecretClientJSON(t, w, SecretMutationResult{Secret: restored, Receipt: restoreReceipt})
		case "POST /v1/secrets/sec_abcdefghijklmnop:delete":
			assertSecretClientIdempotency(t, r, "delete-1")
			var body map[string]json.RawMessage
			decodeSecretClientBody(t, r, &body)
			if len(body) != 1 || string(body["expected_row_version"]) != "3" {
				t.Fatalf("delete body = %#v", body)
			}
			deleted := secret
			deleted.Lifecycle = "deleted"
			deleted.RowVersion = 4
			deleteReceipt := receipt
			deleteReceipt.Operation = "secret_delete"
			deleteReceipt.ResultRevision = 4
			writeSecretClientJSON(t, w, SecretMutationResult{Secret: deleted, Receipt: deleteReceipt})
		case "POST /v1/secrets/sec_abcdefghijklmnop/fields/fld_abcdefghijklmnop:access":
			assertSecretClientIdempotency(t, r, "access-1")
			body, err := io.ReadAll(r.Body)
			if err != nil || len(body) != 0 {
				t.Fatalf("access body = %q / %v, want empty", body, err)
			}
			writeSecretClientJSON(t, w, map[string]any{"schema_version": "witself.v0", "material": material})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	ctx := context.Background()
	gotKey, err := GetCurrentVaultKey(ctx, srv.URL+"/", "agent-token")
	if err != nil || gotKey == nil || gotKey.ID != key.ID {
		t.Fatalf("GetCurrentVaultKey = %#v, %v", gotKey, err)
	}
	registered, err := RegisterVaultKey(ctx, srv.URL, "agent-token", RegisterVaultKeyInput{
		ID: key.ID, KeyVersion: 1, Algorithm: key.Algorithm, Fingerprint: key.Fingerprint, IdempotencyKey: "register-1",
	})
	if err != nil || registered.KeyEpoch.ID != key.ID {
		t.Fatalf("RegisterVaultKey = %#v, %v", registered, err)
	}
	created, err := CreateSecret(ctx, srv.URL, "agent-token", CreateSecretInput{
		ID: secret.ID, Name: secret.Name, Template: secret.Template, Tags: secret.Tags,
		Fields:         []CreateSecretFieldInput{{ID: "fld_bcdefghijklmnopq", Name: "username", Kind: "username", Encoding: "utf8", ValueVersion: 1, PublicValue: &publicValue}},
		IdempotencyKey: "create-1",
	})
	if err != nil || created.Secret.ID != secret.ID {
		t.Fatalf("CreateSecret = %#v, %v", created, err)
	}
	assertSecretClientRedaction(t, created.Secret)
	status, err := GetSecretLimitStatus(ctx, srv.URL, "agent-token")
	if err != nil || status.Used != 1 || status.Max == nil || *status.Max != 10 {
		t.Fatalf("GetSecretLimitStatus = %#v, %v", status, err)
	}
	page, err := ListSecrets(ctx, srv.URL, "agent-token", SecretListOptions{
		Query: "git hub", Lifecycle: "active", Template: "login", Tags: []string{"github", "work"},
		Limit: 25, Cursor: "cursor+1", IncludeFields: true,
	})
	if err != nil || len(page.Items) != 1 || page.NextCursor != "cursor_2" {
		t.Fatalf("ListSecrets = %#v, %v", page, err)
	}
	assertSecretClientRedaction(t, page.Items[0])
	detail, err := GetSecret(ctx, srv.URL, "agent-token", secret.ID)
	if err != nil || detail.ID != secret.ID {
		t.Fatalf("GetSecret = %#v, %v", detail, err)
	}
	assertSecretClientRedaction(t, *detail)
	archived, err := ArchiveSecret(ctx, srv.URL, "agent-token", secret.ID, SecretLifecycleInput{
		ExpectedRowVersion: 1, IdempotencyKey: "archive-1",
	})
	if err != nil || archived.Secret.Lifecycle != "archived" ||
		archived.Receipt.Operation != "secret_archive" {
		t.Fatalf("ArchiveSecret = %#v, %v", archived, err)
	}
	assertSecretClientRedaction(t, archived.Secret)
	restored, err := RestoreSecret(ctx, srv.URL, "agent-token", secret.ID, SecretLifecycleInput{
		ExpectedRowVersion: 2, IdempotencyKey: "restore-1",
	})
	if err != nil || restored.Secret.Lifecycle != "active" ||
		restored.Receipt.Operation != "secret_restore" {
		t.Fatalf("RestoreSecret = %#v, %v", restored, err)
	}
	assertSecretClientRedaction(t, restored.Secret)
	deleted, err := DeleteSecret(ctx, srv.URL, "agent-token", secret.ID, SecretLifecycleInput{
		ExpectedRowVersion: 3, IdempotencyKey: "delete-1",
	})
	if err != nil || deleted.Secret.Lifecycle != "deleted" ||
		deleted.Receipt.Operation != "secret_delete" {
		t.Fatalf("DeleteSecret = %#v, %v", deleted, err)
	}
	assertSecretClientRedaction(t, deleted.Secret)
	gotMaterial, err := AccessSecretField(ctx, srv.URL, "agent-token", secret.ID, material.FieldID, "access-1")
	if err != nil || !bytes.Equal(gotMaterial.Ciphertext, material.Ciphertext) || !bytes.Equal(gotMaterial.DEK.WrappedDEK, material.DEK.WrappedDEK) {
		t.Fatalf("AccessSecretField = %#v, %v", gotMaterial, err)
	}

	for _, route := range []string{
		"GET /v1/vault/key-epochs/current", "POST /v1/vault/key-epochs", "POST /v1/secrets",
		"GET /v1/secrets:status",
		"GET /v1/secrets", "GET /v1/secrets/sec_abcdefghijklmnop",
		"POST /v1/secrets/sec_abcdefghijklmnop:archive",
		"POST /v1/secrets/sec_abcdefghijklmnop:restore",
		"POST /v1/secrets/sec_abcdefghijklmnop:delete",
		"POST /v1/secrets/sec_abcdefghijklmnop/fields/fld_abcdefghijklmnop:access",
	} {
		if calls[route] != 1 {
			t.Errorf("%s calls = %d, want 1", route, calls[route])
		}
	}
}

func TestGetCurrentVaultKeyAllowsUninitializedState(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeSecretClientJSON(t, w, map[string]any{"schema_version": "witself.v0", "key_epoch": nil})
	}))
	defer srv.Close()
	key, err := GetCurrentVaultKey(context.Background(), srv.URL, "agent-token")
	if err != nil || key != nil {
		t.Fatalf("GetCurrentVaultKey = %#v, %v; want nil, nil", key, err)
	}
}

func assertSecretClientRedaction(t *testing.T, secret Secret) {
	t.Helper()
	if len(secret.Fields) != 2 || secret.Fields[0].PublicValue == nil || *secret.Fields[0].PublicValue != "octocat" ||
		secret.Fields[1].PublicValue != nil || !secret.Fields[1].Redacted {
		t.Fatalf("secret redaction = %#v", secret.Fields)
	}
}

func assertSecretClientIdempotency(t *testing.T, r *http.Request, want string) {
	t.Helper()
	if got := r.Header.Get("Idempotency-Key"); got != want {
		t.Fatalf("Idempotency-Key = %q, want %q", got, want)
	}
}

func decodeSecretClientBody(t *testing.T, r *http.Request, dst any) {
	t.Helper()
	defer func() { _ = r.Body.Close() }()
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		t.Fatal(err)
	}
}

func writeSecretClientJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatal(err)
	}
}

func TestSecretMaterialJSONHasNoPlaintextValueField(t *testing.T) {
	raw, err := json.Marshal(SecretMaterial{Ciphertext: []byte("ciphertext"), DEK: SealedDEK{WrappedDEK: []byte("wrapped")}})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"value", "plaintext", "password", "agent_vault_key"} {
		if _, present := fields[forbidden]; present {
			t.Fatalf("SecretMaterial JSON contains forbidden %q field: %s", forbidden, raw)
		}
	}
}
