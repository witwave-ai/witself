package client

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestVaultKeyEnrollmentClientTransportContract(t *testing.T) {
	createdAt := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	expiresAt := createdAt.Add(10 * time.Minute)
	approvedAt := createdAt.Add(time.Minute)
	consumedAt := createdAt.Add(2 * time.Minute)
	cancelledAt := createdAt.Add(30 * time.Second)
	targetPublicKey := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x21}, vaultEnrollmentPublicKeyBytes))
	pairingCommitment := strings.Repeat("2", 2*vaultEnrollmentCommitmentBytes)
	sourcePublicKey := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x23}, vaultEnrollmentPublicKeyBytes))
	transferCiphertext := bytes.Repeat([]byte{0x24}, vaultEnrollmentMinCiphertextBytes)
	consumeCommitment := strings.Repeat("5", 2*vaultEnrollmentCommitmentBytes)
	consumeProof := bytes.Repeat([]byte{0x26}, vaultEnrollmentCommitmentBytes)

	pending := VaultKeyEnrollment{
		ID: "enr_abcdefghijklmnop", AccountID: "acc_1", RealmID: "realm_1", OwnerAgentID: "agent_1",
		VaultKeyID: "avk_abcdefghijklmnop", VaultKeyVersion: 1,
		VaultKeyAlgorithm: VaultEnrollmentVaultKeyAlgorithm, VaultKeyFingerprint: strings.Repeat("a", 64),
		TargetLocationID: "loc_abcdefghijklmnop", TargetLocationName: "work",
		TargetPublicKey: targetPublicKey, TargetKeyAlgorithm: VaultEnrollmentTargetKeyAlgorithm,
		PairingCommitment: pairingCommitment, LifecycleState: VaultEnrollmentStatePending,
		RowVersion: 1, CreatedAt: createdAt, ExpiresAt: expiresAt,
	}
	approved := pending
	approved.LifecycleState = VaultEnrollmentStateApproved
	approved.SourceLocationID = "loc_ponmlkjihgfedcba"
	approved.TransferAlgorithm = VaultEnrollmentTransferAlgorithm
	approved.RowVersion = 2
	approved.ApprovedAt = &approvedAt
	consumed := approved
	consumed.LifecycleState = VaultEnrollmentStateConsumed
	consumed.TransferAlgorithm = ""
	consumed.RowVersion = 3
	consumed.ConsumedAt = &consumedAt
	cancelled := pending
	cancelled.LifecycleState = VaultEnrollmentStateCancelled
	cancelled.RowVersion = 2
	cancelled.CancelledAt = &cancelledAt
	transfer := VaultKeyEnrollmentTransfer{
		Enrollment: approved, SourceEphemeralPublicKey: sourcePublicKey,
		Ciphertext: transferCiphertext, ConsumeCommitment: consumeCommitment,
	}

	calls := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route := r.Method + " " + r.URL.Path
		calls[route]++
		if got := r.Header.Get("Authorization"); got != "Bearer agent-token" {
			t.Fatalf("%s Authorization = %q", route, got)
		}
		if r.Method == http.MethodGet {
			if got := r.Header.Get("Content-Type"); got != "" {
				t.Fatalf("%s Content-Type = %q, want empty", route, got)
			}
		} else if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("%s Content-Type = %q", route, got)
		}
		switch route {
		case "POST /v1/vault/enrollments":
			assertSecretClientIdempotency(t, r, "enroll-1")
			body := decodeVaultEnrollmentClientMap(t, r)
			assertVaultEnrollmentBodyKeys(t, body,
				"id", "target_location_id", "target_location_name", "target_public_key",
				"target_key_algorithm", "pairing_commitment", "expires_at")
			if string(body["id"]) != `"enr_abcdefghijklmnop"` ||
				string(body["target_location_id"]) != `"loc_abcdefghijklmnop"` ||
				string(body["target_key_algorithm"]) != `"X25519_RAW_32_BASE64URL_V1"` ||
				string(body["expires_at"]) != `"2026-07-19T12:10:00Z"` {
				t.Fatalf("create body = %s", mustVaultEnrollmentJSON(t, body))
			}
			w.WriteHeader(http.StatusCreated)
			writeSecretClientJSON(t, w, map[string]any{"schema_version": "witself.v0", "enrollment": pending})
		case "GET /v1/vault/enrollments":
			if got := r.Header.Get("Idempotency-Key"); got != "" {
				t.Fatalf("list Idempotency-Key = %q", got)
			}
			if r.URL.Query().Get("state") != "pending" || r.URL.Query().Get("limit") != "5" || len(r.URL.Query()) != 2 {
				t.Fatalf("list query = %#v", r.URL.Query())
			}
			writeSecretClientJSON(t, w, map[string]any{"schema_version": "witself.v0", "items": []VaultKeyEnrollment{pending}})
		case "GET /v1/vault/enrollments/enr_abcdefghijklmnop":
			if got := r.Header.Get("Idempotency-Key"); got != "" {
				t.Fatalf("get Idempotency-Key = %q", got)
			}
			writeSecretClientJSON(t, w, map[string]any{"schema_version": "witself.v0", "enrollment": pending})
		case "POST /v1/vault/enrollments/enr_abcdefghijklmnop:approve":
			assertSecretClientIdempotency(t, r, "approve-1")
			body := decodeVaultEnrollmentClientMap(t, r)
			assertVaultEnrollmentBodyKeys(t, body,
				"expected_row_version", "source_location_id", "source_ephemeral_public_key",
				"transfer_ciphertext", "transfer_algorithm", "consume_commitment")
			if string(body["expected_row_version"]) != "1" ||
				string(body["source_location_id"]) != `"loc_ponmlkjihgfedcba"` ||
				string(body["transfer_algorithm"]) != `"X25519_HKDF_SHA256_AES_256_GCM_V1"` {
				t.Fatalf("approve body = %s", mustVaultEnrollmentJSON(t, body))
			}
			writeSecretClientJSON(t, w, map[string]any{"schema_version": "witself.v0", "enrollment": approved})
		case "POST /v1/vault/enrollments/enr_abcdefghijklmnop:receive":
			if got := r.Header.Get("Idempotency-Key"); got != "" {
				t.Fatalf("receive Idempotency-Key = %q", got)
			}
			body := decodeVaultEnrollmentClientMap(t, r)
			assertVaultEnrollmentBodyKeys(t, body, "target_location_id")
			if string(body["target_location_id"]) != `"loc_abcdefghijklmnop"` {
				t.Fatalf("receive body = %s", mustVaultEnrollmentJSON(t, body))
			}
			writeSecretClientJSON(t, w, map[string]any{"schema_version": "witself.v0", "transfer": transfer})
		case "POST /v1/vault/enrollments/enr_abcdefghijklmnop:consume":
			assertSecretClientIdempotency(t, r, "consume-1")
			body := decodeVaultEnrollmentClientMap(t, r)
			assertVaultEnrollmentBodyKeys(t, body, "expected_row_version", "target_location_id", "consume_proof")
			if string(body["expected_row_version"]) != "2" ||
				string(body["target_location_id"]) != `"loc_abcdefghijklmnop"` {
				t.Fatalf("consume body = %s", mustVaultEnrollmentJSON(t, body))
			}
			writeSecretClientJSON(t, w, map[string]any{"schema_version": "witself.v0", "enrollment": consumed})
		case "POST /v1/vault/enrollments/enr_abcdefghijklmnop:cancel":
			assertSecretClientIdempotency(t, r, "cancel-1")
			body := decodeVaultEnrollmentClientMap(t, r)
			assertVaultEnrollmentBodyKeys(t, body, "expected_row_version")
			if string(body["expected_row_version"]) != "1" {
				t.Fatalf("cancel body = %s", mustVaultEnrollmentJSON(t, body))
			}
			writeSecretClientJSON(t, w, map[string]any{"schema_version": "witself.v0", "enrollment": cancelled})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	ctx := context.Background()
	created, err := CreateVaultKeyEnrollment(ctx, srv.URL+"/", "agent-token", CreateVaultKeyEnrollmentInput{
		ID: pending.ID, TargetLocationID: pending.TargetLocationID, TargetLocationName: pending.TargetLocationName,
		TargetPublicKey: targetPublicKey, TargetKeyAlgorithm: VaultEnrollmentTargetKeyAlgorithm,
		PairingCommitment: pairingCommitment, ExpiresAt: expiresAt.Add(987 * time.Millisecond), IdempotencyKey: "enroll-1",
	})
	if err != nil || created.ID != pending.ID || created.LifecycleState != VaultEnrollmentStatePending {
		t.Fatalf("CreateVaultKeyEnrollment = %#v, %v", created, err)
	}
	listed, err := ListVaultKeyEnrollments(ctx, srv.URL, "agent-token", VaultKeyEnrollmentListOptions{State: "pending", Limit: 5})
	if err != nil || len(listed) != 1 || listed[0].ID != pending.ID {
		t.Fatalf("ListVaultKeyEnrollments = %#v, %v", listed, err)
	}
	got, err := GetVaultKeyEnrollment(ctx, srv.URL, "agent-token", pending.ID)
	if err != nil || got.ID != pending.ID {
		t.Fatalf("GetVaultKeyEnrollment = %#v, %v", got, err)
	}
	approvedResult, err := ApproveVaultKeyEnrollment(ctx, srv.URL, "agent-token", pending.ID, ApproveVaultKeyEnrollmentInput{
		ExpectedRowVersion: 1, SourceLocationID: approved.SourceLocationID,
		SourceEphemeralPublicKey: sourcePublicKey, TransferCiphertext: transferCiphertext,
		TransferAlgorithm: VaultEnrollmentTransferAlgorithm, ConsumeCommitment: consumeCommitment,
		IdempotencyKey: "approve-1",
	})
	if err != nil || approvedResult.LifecycleState != VaultEnrollmentStateApproved || approvedResult.RowVersion != 2 {
		t.Fatalf("ApproveVaultKeyEnrollment = %#v, %v", approvedResult, err)
	}
	received, err := ReceiveVaultKeyEnrollment(ctx, srv.URL, "agent-token", pending.ID,
		ReceiveVaultKeyEnrollmentInput{TargetLocationID: pending.TargetLocationID})
	if err != nil || !bytes.Equal(received.Ciphertext, transferCiphertext) ||
		received.SourceEphemeralPublicKey != sourcePublicKey {
		t.Fatalf("ReceiveVaultKeyEnrollment = %#v, %v", received, err)
	}
	consumedResult, err := ConsumeVaultKeyEnrollment(ctx, srv.URL, "agent-token", pending.ID, ConsumeVaultKeyEnrollmentInput{
		ExpectedRowVersion: 2, TargetLocationID: pending.TargetLocationID,
		ConsumeProof: consumeProof, IdempotencyKey: "consume-1",
	})
	if err != nil || consumedResult.LifecycleState != VaultEnrollmentStateConsumed || consumedResult.RowVersion != 3 {
		t.Fatalf("ConsumeVaultKeyEnrollment = %#v, %v", consumedResult, err)
	}
	cancelledResult, err := CancelVaultKeyEnrollment(ctx, srv.URL, "agent-token", pending.ID,
		CancelVaultKeyEnrollmentInput{ExpectedRowVersion: 1, IdempotencyKey: "cancel-1"})
	if err != nil || cancelledResult.LifecycleState != VaultEnrollmentStateCancelled || cancelledResult.RowVersion != 2 {
		t.Fatalf("CancelVaultKeyEnrollment = %#v, %v", cancelledResult, err)
	}

	for _, route := range []string{
		"POST /v1/vault/enrollments", "GET /v1/vault/enrollments",
		"GET /v1/vault/enrollments/enr_abcdefghijklmnop",
		"POST /v1/vault/enrollments/enr_abcdefghijklmnop:approve",
		"POST /v1/vault/enrollments/enr_abcdefghijklmnop:receive",
		"POST /v1/vault/enrollments/enr_abcdefghijklmnop:consume",
		"POST /v1/vault/enrollments/enr_abcdefghijklmnop:cancel",
	} {
		if calls[route] != 1 {
			t.Errorf("%s calls = %d, want 1", route, calls[route])
		}
	}
}

func TestVaultKeyEnrollmentListNormalizesNullAndEscapesPath(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch calls {
		case 1:
			if r.URL.Path != "/v1/vault/enrollments" {
				t.Fatalf("list path = %q", r.URL.Path)
			}
			_, _ = io.WriteString(w, `{"schema_version":"witself.v0","items":null}`)
		case 2:
			if r.RequestURI != "/v1/vault/enrollments/enr_bad%2Fpath" {
				t.Fatalf("escaped request URI = %q", r.RequestURI)
			}
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	items, err := ListVaultKeyEnrollments(context.Background(), srv.URL, "agent-token", VaultKeyEnrollmentListOptions{})
	if err != nil || items == nil || len(items) != 0 {
		t.Fatalf("null list = %#v, %v; want non-nil empty", items, err)
	}
	if _, err := GetVaultKeyEnrollment(context.Background(), srv.URL, "agent-token", "enr_bad/path"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("escaped get error = %v, want not found", err)
	}
}

func TestVaultKeyEnrollmentResponsesRejectUnknownOrValueBearingFields(t *testing.T) {
	const forbiddenValue = "private-avk-canary-714"
	for name, response := range map[string]string{
		"top level AVK":    `{"schema_version":"witself.v0","enrollment":{},"agent_vault_key":"` + forbiddenValue + `"}`,
		"nested plaintext": `{"schema_version":"witself.v0","enrollment":{"plaintext":"` + forbiddenValue + `"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, response)
			}))
			defer srv.Close()
			_, err := GetVaultKeyEnrollment(context.Background(), srv.URL, "agent-token", "enr_abcdefghijklmnop")
			if err == nil {
				t.Fatal("value-bearing response was accepted")
			}
			if strings.Contains(err.Error(), forbiddenValue) {
				t.Fatal("response error exposed forbidden value")
			}
		})
	}
}

func TestVaultKeyEnrollmentOutputTypesHaveNoAVKOrPlaintextField(t *testing.T) {
	values := []any{
		VaultKeyEnrollment{},
		VaultKeyEnrollmentTransfer{},
		[]VaultKeyEnrollment{{}},
	}
	for _, value := range values {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var walk any
		if err := json.Unmarshal(raw, &walk); err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{"avk", "agent_vault_key", "vault_key_material", "plaintext", "pairing_secret", "consume_proof"} {
			if vaultEnrollmentJSONHasKey(walk, forbidden) {
				t.Fatalf("%T JSON contains forbidden field %q: %s", value, forbidden, raw)
			}
		}
	}
}

func TestVaultKeyEnrollmentResponseValidationRejectsMalformedCryptoProjection(t *testing.T) {
	createdAt := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	value := VaultKeyEnrollment{
		ID: "enr_abcdefghijklmnop", AccountID: "acc_1", RealmID: "realm_1", OwnerAgentID: "agent_1",
		VaultKeyID: "avk_abcdefghijklmnop", VaultKeyVersion: 1,
		VaultKeyAlgorithm: VaultEnrollmentVaultKeyAlgorithm, VaultKeyFingerprint: strings.Repeat("a", 64),
		TargetLocationID:   "loc_abcdefghijklmnop",
		TargetPublicKey:    base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 31)),
		TargetKeyAlgorithm: VaultEnrollmentTargetKeyAlgorithm,
		PairingCommitment:  strings.Repeat("2", 2*vaultEnrollmentCommitmentBytes),
		LifecycleState:     VaultEnrollmentStatePending, RowVersion: 1,
		CreatedAt: createdAt, ExpiresAt: createdAt.Add(time.Minute),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeSecretClientJSON(t, w, map[string]any{"schema_version": "witself.v0", "enrollment": value})
	}))
	defer srv.Close()
	if _, err := GetVaultKeyEnrollment(context.Background(), srv.URL, "agent-token", value.ID); !errors.Is(err, ErrInvalidVaultEnrollmentResponse) {
		t.Fatalf("malformed projection error = %v, want ErrInvalidVaultEnrollmentResponse", err)
	}
}

func decodeVaultEnrollmentClientMap(t *testing.T, r *http.Request) map[string]json.RawMessage {
	t.Helper()
	var body map[string]json.RawMessage
	decodeSecretClientBody(t, r, &body)
	return body
}

func assertVaultEnrollmentBodyKeys(t *testing.T, body map[string]json.RawMessage, expected ...string) {
	t.Helper()
	if len(body) != len(expected) {
		t.Fatalf("body fields = %v, want %v", vaultEnrollmentMapKeys(body), expected)
	}
	for _, key := range expected {
		if _, ok := body[key]; !ok {
			t.Fatalf("body fields = %v, missing %q", vaultEnrollmentMapKeys(body), key)
		}
	}
	for _, forbidden := range []string{"idempotency_key", "avk", "agent_vault_key", "vault_key_material", "plaintext", "pairing_secret"} {
		if _, ok := body[forbidden]; ok {
			t.Fatalf("request body contains forbidden field %q", forbidden)
		}
	}
}

func vaultEnrollmentMapKeys(body map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(body))
	for key := range body {
		keys = append(keys, key)
	}
	return keys
}

func mustVaultEnrollmentJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func vaultEnrollmentJSONHasKey(value any, target string) bool {
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			if key == target || vaultEnrollmentJSONHasKey(child, target) {
				return true
			}
		}
	case []any:
		for _, child := range value {
			if vaultEnrollmentJSONHasKey(child, target) {
				return true
			}
		}
	}
	return false
}
