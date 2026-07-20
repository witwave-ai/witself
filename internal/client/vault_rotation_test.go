package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const (
	testRotationID = "vkr_aaaaaaaaaaaaaaaa"
	testSourceAVK  = "avk_bbbbbbbbbbbbbbbb"
	testTargetAVK  = "avk_cccccccccccccccc"
)

func TestVaultKeyRotationHTTPTransport(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	rotation := testClientVaultRotation(now, VaultKeyRotationOpen)
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Fatalf("authorization header = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/vault/rotations":
			if r.Header.Get("Idempotency-Key") != "start-key" {
				t.Fatalf("start idempotency header = %q", r.Header.Get("Idempotency-Key"))
			}
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["idempotency_key"] != nil {
				t.Fatalf("start body leaked idempotency key: %#v err=%v", body, err)
			}
			writeClientRotationMutation(t, w, rotation, "rotation_start", now)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/vault/rotations/open":
			_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "rotation": rotation})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/vault/rotations/"+testRotationID:
			_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "rotation": rotation})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/vault/rotations/"+testRotationID+"/items":
			if r.URL.Query().Get("limit") != "25" || r.URL.Query().Get("cursor") != "next cursor" {
				t.Fatalf("unexpected item query: %s", r.URL.RawQuery)
			}
			item := testClientVaultRotationItem(now)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version": "witself.v0", "items": []VaultKeyRotationItem{item}, "next_cursor": "more",
			})
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/vault/rotations/"+testRotationID+":"):
			action := strings.TrimPrefix(r.URL.Path, "/v1/vault/rotations/"+testRotationID+":")
			if r.Header.Get("Idempotency-Key") != action+"-key" {
				t.Fatalf("%s idempotency header = %q", action, r.Header.Get("Idempotency-Key"))
			}
			result := rotation
			operation := "rotation_" + action
			switch action {
			case "commit":
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatalf("decode commit body: %v", err)
				}
				disposition, ok := body["recovery_disposition"].(map[string]any)
				if !ok || disposition["mode"] != VaultKeyRotationRecoveryArtifact ||
					disposition["artifact_sha256"] != strings.Repeat("f", 64) {
					t.Fatalf("commit recovery disposition = %#v", body)
				}
				for _, forbidden := range []string{"artifact", "path", "passphrase", "key", "key_material"} {
					if _, leaked := body[forbidden]; leaked {
						t.Fatalf("commit body leaked %q: %#v", forbidden, body)
					}
				}
				result = testClientVaultRotation(now, VaultKeyRotationCommitted)
			case "cancel":
				result = testClientVaultRotation(now, VaultKeyRotationCancelled)
			}
			writeClientRotationMutation(t, w, result, operation, now)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	start, err := StartVaultKeyRotation(context.Background(), server.URL, "token", StartVaultKeyRotationInput{
		ID: testRotationID, ExpectedSourceKeyID: testSourceAVK, ExpectedSourceKeyVersion: 1,
		ExpectedSourceKeyRowVersion: 1, TargetKeyID: testTargetAVK, TargetKeyVersion: 2,
		TargetAlgorithm: VaultEnrollmentVaultKeyAlgorithm, TargetFingerprint: strings.Repeat("d", 64),
		IdempotencyKey: "start-key",
	})
	if err != nil || start.Rotation.ID != testRotationID {
		t.Fatalf("start = %#v, %v", start, err)
	}
	open, err := GetOpenVaultKeyRotation(context.Background(), server.URL, "token")
	if err != nil || open == nil || open.ID != testRotationID {
		t.Fatalf("open = %#v, %v", open, err)
	}
	got, err := GetVaultKeyRotation(context.Background(), server.URL, "token", testRotationID)
	if err != nil || got.ID != testRotationID {
		t.Fatalf("get = %#v, %v", got, err)
	}
	page, err := ListVaultKeyRotationItems(context.Background(), server.URL, "token", testRotationID,
		VaultKeyRotationItemListOptions{Limit: 25, Cursor: "next cursor"})
	if err != nil || len(page.Items) != 1 || page.NextCursor != "more" {
		t.Fatalf("items = %#v, %v", page, err)
	}
	stage, err := StageVaultKeyRotation(context.Background(), server.URL, "token", testRotationID,
		StageVaultKeyRotationInput{ExpectedRotationRowVersion: 1, Items: []StageVaultKeyRotationItemInput{{
			DEKID: "dek_ffffffffffffffff", ExpectedSourceDEKRowVersion: 1,
			ExpectedSourceWrapRevision: 1, TargetWrappedDEK: make([]byte, 60), TargetWrapRevision: 2,
		}}, IdempotencyKey: "stage-key"})
	if err != nil || stage.Receipt.Operation != "rotation_stage" {
		t.Fatalf("stage = %#v, %v", stage, err)
	}
	commit, err := CommitVaultKeyRotation(context.Background(), server.URL, "token", testRotationID,
		CommitVaultKeyRotationInput{ExpectedRotationRowVersion: 1, ExpectedItemCount: 1,
			ExpectedPlanHash: strings.Repeat("e", 64),
			RecoveryDisposition: VaultKeyRotationRecoveryDisposition{
				Mode: VaultKeyRotationRecoveryArtifact, ArtifactSHA256: strings.Repeat("f", 64),
			},
			IdempotencyKey: "commit-key"})
	if err != nil || commit.Rotation.LifecycleState != VaultKeyRotationCommitted {
		t.Fatalf("commit = %#v, %v", commit, err)
	}
	cancel, err := CancelVaultKeyRotation(context.Background(), server.URL, "token", testRotationID,
		CancelVaultKeyRotationInput{ExpectedRotationRowVersion: 1, IdempotencyKey: "cancel-key"})
	if err != nil || cancel.Rotation.LifecycleState != VaultKeyRotationCancelled {
		t.Fatalf("cancel = %#v, %v", cancel, err)
	}
	if requestCount != 7 {
		t.Fatalf("request count = %d", requestCount)
	}
}

func TestGetOpenVaultKeyRotationAcceptsOnlyNullOrOpenState(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for _, test := range []struct {
		name     string
		rotation any
		wantNil  bool
		wantErr  bool
	}{
		{name: "none", rotation: nil, wantNil: true},
		{name: "open", rotation: testClientVaultRotation(now, VaultKeyRotationOpen)},
		{name: "terminal rejected", rotation: testClientVaultRotation(now, VaultKeyRotationCommitted), wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/vault/rotations/open" {
					t.Fatalf("open rotation path = %q", r.URL.Path)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"schema_version": "witself.v0", "rotation": test.rotation,
				})
			}))
			defer server.Close()
			rotation, err := GetOpenVaultKeyRotation(context.Background(), server.URL, "token")
			if (err != nil) != test.wantErr || (!test.wantErr && (rotation == nil) != test.wantNil) {
				t.Fatalf("GetOpenVaultKeyRotation = %#v / %v", rotation, err)
			}
		})
	}
}

func TestVaultKeyRotationRejectsMalformedResponses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"schema_version":"witself.v0","rotation":{"id":"vkr_aaaaaaaaaaaaaaaa"},"unknown":true}`))
	}))
	defer server.Close()
	if _, err := GetVaultKeyRotation(context.Background(), server.URL, "token", testRotationID); err == nil {
		t.Fatal("malformed response was accepted")
	}
}

func TestVaultKeyRotationRejectsInvalidKeyMetadata(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for _, test := range []struct {
		name   string
		mutate func(*VaultKeyRotation)
	}{
		{name: "missing source algorithm", mutate: func(value *VaultKeyRotation) { value.SourceKeyAlgorithm = "" }},
		{name: "unsupported source algorithm", mutate: func(value *VaultKeyRotation) { value.SourceKeyAlgorithm = "AES_128_GCM" }},
		{name: "invalid source fingerprint", mutate: func(value *VaultKeyRotation) { value.SourceKeyFingerprint = strings.Repeat("g", 64) }},
		{name: "missing target algorithm", mutate: func(value *VaultKeyRotation) { value.TargetKeyAlgorithm = "" }},
		{name: "unsupported target algorithm", mutate: func(value *VaultKeyRotation) { value.TargetKeyAlgorithm = "AES_128_GCM" }},
		{name: "invalid target fingerprint", mutate: func(value *VaultKeyRotation) { value.TargetKeyFingerprint = strings.Repeat("f", 63) }},
		{name: "non-contiguous target version", mutate: func(value *VaultKeyRotation) { value.TargetKeyVersion = value.SourceKeyVersion + 2 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			rotation := testClientVaultRotation(now, VaultKeyRotationOpen)
			test.mutate(&rotation)
			if err := validateVaultKeyRotation(&rotation); !errors.Is(err, ErrInvalidVaultKeyRotationResponse) {
				t.Fatalf("validateVaultKeyRotation error = %v", err)
			}
		})
	}
}

func TestVaultKeyRotationCommittedResponseRequiresExactRecoveryDisposition(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	tests := []struct {
		name   string
		mode   string
		digest string
		valid  bool
	}{
		{name: "artifact", mode: VaultKeyRotationRecoveryArtifact, digest: strings.Repeat("a", 64), valid: true},
		{name: "risk accepted", mode: VaultKeyRotationRiskAccepted, valid: true},
		{name: "missing"},
		{name: "unknown", mode: "backup_exists"},
		{name: "artifact missing digest", mode: VaultKeyRotationRecoveryArtifact},
		{name: "artifact malformed digest", mode: VaultKeyRotationRecoveryArtifact, digest: strings.Repeat("g", 64)},
		{name: "artifact uppercase digest", mode: VaultKeyRotationRecoveryArtifact, digest: strings.Repeat("A", 64)},
		{name: "risk with digest", mode: VaultKeyRotationRiskAccepted, digest: strings.Repeat("a", 64)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rotation := testClientVaultRotation(now, VaultKeyRotationCommitted)
			rotation.RecoveryDispositionMode = test.mode
			rotation.RecoveryArtifactSHA256 = test.digest
			err := validateVaultKeyRotation(&rotation)
			if (err == nil) != test.valid {
				t.Fatalf("validate error = %v, valid=%t", err, test.valid)
			}
		})
	}
	for _, state := range []string{VaultKeyRotationOpen, VaultKeyRotationCancelled} {
		rotation := testClientVaultRotation(now, state)
		rotation.RecoveryDispositionMode = VaultKeyRotationRiskAccepted
		if err := validateVaultKeyRotation(&rotation); !errors.Is(err, ErrInvalidVaultKeyRotationResponse) {
			t.Fatalf("%s disposition error = %v", state, err)
		}
	}
}

func testClientVaultRotation(now time.Time, state string) VaultKeyRotation {
	out := VaultKeyRotation{
		ID: testRotationID, AccountID: "acc_aaaaaaaaaaaaaaaa", RealmID: "realm_bbbbbbbbbbbbbbbb",
		OwnerAgentID: "agent_cccccccccccccccc", SourceKeyID: testSourceAVK, SourceKeyVersion: 1,
		SourceKeyAlgorithm: VaultEnrollmentVaultKeyAlgorithm, SourceKeyFingerprint: strings.Repeat("b", 64),
		TargetKeyID: testTargetAVK, TargetKeyVersion: 2,
		TargetKeyAlgorithm: VaultEnrollmentVaultKeyAlgorithm, TargetKeyFingerprint: strings.Repeat("c", 64),
		LifecycleState: state,
		ItemCount:      1, StagedCount: 1, RowVersion: 2, CreatedAt: now, UpdatedAt: now,
	}
	switch state {
	case VaultKeyRotationOpen:
		out.StagedPlanHash = strings.Repeat("a", 64)
	case VaultKeyRotationCommitted:
		out.CommittedAt = &now
		out.RecoveryDispositionMode = VaultKeyRotationRecoveryArtifact
		out.RecoveryArtifactSHA256 = strings.Repeat("d", 64)
	case VaultKeyRotationCancelled:
		out.CancelledAt = &now
	}
	return out
}

func testClientVaultRotationItem(now time.Time) VaultKeyRotationItem {
	targetWrapper := make([]byte, 60)
	return VaultKeyRotationItem{
		RotationID: testRotationID, SecretID: "sec_dddddddddddddddd", FieldID: "fld_eeeeeeeeeeeeeeee",
		FieldKind: "password", DEKID: "dek_ffffffffffffffff", DEKGeneration: 1,
		SourceDEKRowVersion: 1, SourceWrapRevision: 1, SourceWrappedDEK: make([]byte, 60),
		SourceWrapAlgorithm: VaultEnrollmentVaultKeyAlgorithm, SourceAADVersion: 1,
		SourceWrappingKeyID: testSourceAVK, SourceWrappingKeyVersion: 1,
		TargetWrappingKeyID: testTargetAVK, TargetWrappingKeyVersion: 2,
		TargetWrappedDEK: targetWrapper, TargetWrapRevision: 2,
		TargetWrapperSHA256: vaultRotationWrapperHash(targetWrapper), StagedAt: &now,
	}
}

func writeClientRotationMutation(t *testing.T, w http.ResponseWriter, rotation VaultKeyRotation, operation string, now time.Time) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(map[string]any{
		"schema_version": "witself.v0", "rotation": rotation,
		"receipt": VaultKeyRotationReceipt{Operation: operation, RequestHash: strings.Repeat("a", 64),
			RotationID: rotation.ID, ResultRevision: rotation.RowVersion, CreatedAt: now},
	}); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}
