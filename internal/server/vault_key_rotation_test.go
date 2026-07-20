package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const (
	testRotationID  = "vkr_abcdefghijklmnop"
	testTargetKeyID = "avk_bcdefghijklmnopq"
)

func TestVaultKeyRotationRoutesCarryExactFencesAndOpaqueWrappers(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	wrapper := bytes.Repeat([]byte{0x5a}, vaultKeyRotationWrappedDEKBytes)
	rotation := VaultKeyRotation{
		ID: testRotationID, AccountID: "acc_1", RealmID: "realm_1", OwnerAgentID: "agent_1",
		SourceKeyID: testVaultKeyID, SourceKeyVersion: 1,
		SourceKeyAlgorithm: "AES_256_GCM_RANDOM_NONCE_V1", SourceKeyFingerprint: strings.Repeat("a", 64),
		TargetKeyID: testTargetKeyID, TargetKeyVersion: 2,
		TargetKeyAlgorithm: "AES_256_GCM_RANDOM_NONCE_V1", TargetKeyFingerprint: strings.Repeat("b", 64),
		LifecycleState: "open", ItemCount: 1, StagedCount: 1, RowVersion: 2,
		StagedPlanHash: strings.Repeat("c", 64), CreatedAt: now, UpdatedAt: now,
	}
	receipt := VaultKeyRotationReceipt{
		Operation: "rotation_stage", RequestHash: strings.Repeat("d", 64),
		RotationID: testRotationID, ResultRevision: 2, CreatedAt: now,
	}
	item := VaultKeyRotationItem{
		RotationID: testRotationID, SecretID: testSecretID, FieldID: testFieldID,
		FieldKind: "password", DEKID: testDEKID, DEKGeneration: 1,
		SourceDEKRowVersion: 1, SourceWrapRevision: 1, SourceWrappedDEK: wrapper,
		SourceWrapAlgorithm: "AES_256_GCM_RANDOM_NONCE_V1", SourceAADVersion: 1,
		SourceWrappingKeyID: testVaultKeyID, SourceWrappingKeyVersion: 1,
		TargetWrappingKeyID: testTargetKeyID, TargetWrappingKeyVersion: 2,
	}

	var (
		started   StartVaultKeyRotationRequest
		openReads int
		listed    VaultKeyRotationItemListOptions
		staged    StageVaultKeyRotationRequest
		committed CommitVaultKeyRotationRequest
		cancelled CancelVaultKeyRotationRequest
	)
	srv := httptest.NewServer(apiMux(Config{
		AuthenticatePrincipal: secretTestAuth,
		StartVaultKeyRotation: func(_ context.Context, p DomainPrincipal, in StartVaultKeyRotationRequest) (VaultKeyRotationMutationResult, error) {
			assertSecretTestPrincipal(t, p)
			started = in
			return VaultKeyRotationMutationResult{Rotation: rotation, Receipt: receipt}, nil
		},
		GetOpenVaultKeyRotation: func(_ context.Context, p DomainPrincipal) (*VaultKeyRotation, error) {
			assertSecretTestPrincipal(t, p)
			openReads++
			value := rotation
			return &value, nil
		},
		GetVaultKeyRotation: func(_ context.Context, p DomainPrincipal, id string) (VaultKeyRotation, error) {
			assertSecretTestPrincipal(t, p)
			if id != testRotationID {
				t.Fatalf("get rotation id = %q", id)
			}
			return rotation, nil
		},
		ListVaultKeyRotationItems: func(_ context.Context, p DomainPrincipal, id string, opts VaultKeyRotationItemListOptions) (VaultKeyRotationItemPage, error) {
			assertSecretTestPrincipal(t, p)
			if id != testRotationID {
				t.Fatalf("list rotation id = %q", id)
			}
			listed = opts
			return VaultKeyRotationItemPage{Items: []VaultKeyRotationItem{item}, NextCursor: "next-cursor"}, nil
		},
		StageVaultKeyRotation: func(_ context.Context, p DomainPrincipal, id string, in StageVaultKeyRotationRequest) (VaultKeyRotationMutationResult, error) {
			assertSecretTestPrincipal(t, p)
			if id != testRotationID {
				t.Fatalf("stage rotation id = %q", id)
			}
			staged = in
			return VaultKeyRotationMutationResult{Rotation: rotation, Receipt: receipt}, nil
		},
		CommitVaultKeyRotation: func(_ context.Context, p DomainPrincipal, id string, in CommitVaultKeyRotationRequest) (VaultKeyRotationMutationResult, error) {
			assertSecretTestPrincipal(t, p)
			if id != testRotationID {
				t.Fatalf("commit rotation id = %q", id)
			}
			committed = in
			return VaultKeyRotationMutationResult{Rotation: rotation, Receipt: receipt}, nil
		},
		CancelVaultKeyRotation: func(_ context.Context, p DomainPrincipal, id string, in CancelVaultKeyRotationRequest) (VaultKeyRotationMutationResult, error) {
			assertSecretTestPrincipal(t, p)
			if id != testRotationID {
				t.Fatalf("cancel rotation id = %q", id)
			}
			cancelled = in
			return VaultKeyRotationMutationResult{Rotation: rotation, Receipt: receipt}, nil
		},
	}))
	defer srv.Close()

	startBody := mustVaultKeyRotationJSON(t, StartVaultKeyRotationRequest{
		ID: testRotationID, ExpectedSourceKeyID: testVaultKeyID,
		ExpectedSourceKeyVersion: 1, ExpectedSourceKeyRowVersion: 1,
		TargetKeyID: testTargetKeyID, TargetKeyVersion: 2,
		TargetAlgorithm:   "AES_256_GCM_RANDOM_NONCE_V1",
		TargetFingerprint: strings.Repeat("a", 64),
	})
	resp := secretTestRequest(t, srv.URL, http.MethodPost, "/v1/vault/rotations", "agent-token", startBody, "rotation-start-1")
	startResponse := assertSecretTestResponse(t, resp, http.StatusCreated, false)
	assertVaultKeyRotationEnvelope(t, startResponse, true)
	if started.ID != testRotationID || started.ExpectedSourceKeyRowVersion != 1 ||
		started.TargetKeyID != testTargetKeyID || started.IdempotencyKey != "rotation-start-1" {
		t.Fatalf("start callback input = %#v", started)
	}
	resp = secretTestRequest(t, srv.URL, http.MethodGet, "/v1/vault/rotations/open", "agent-token", "", "")
	assertVaultKeyRotationEnvelope(t, assertSecretTestResponse(t, resp, http.StatusOK, false), false)
	if openReads != 1 {
		t.Fatalf("open rotation callback calls = %d, want 1", openReads)
	}

	resp = secretTestRequest(t, srv.URL, http.MethodGet, "/v1/vault/rotations/"+testRotationID, "agent-token", "", "")
	getResponse := assertSecretTestResponse(t, resp, http.StatusOK, false)
	assertVaultKeyRotationEnvelope(t, getResponse, false)

	resp = secretTestRequest(t, srv.URL, http.MethodGet,
		"/v1/vault/rotations/"+testRotationID+"/items?limit=25&cursor=cursor-1", "agent-token", "", "")
	listResponse := assertSecretTestResponse(t, resp, http.StatusOK, false)
	if listed.Limit != 25 || listed.Cursor != "cursor-1" {
		t.Fatalf("list callback options = %#v", listed)
	}
	var listEnvelope struct {
		SchemaVersion string                 `json:"schema_version"`
		Items         []VaultKeyRotationItem `json:"items"`
		NextCursor    string                 `json:"next_cursor"`
	}
	if err := json.Unmarshal(listResponse, &listEnvelope); err != nil ||
		listEnvelope.SchemaVersion != "witself.v0" || len(listEnvelope.Items) != 1 ||
		!bytes.Equal(listEnvelope.Items[0].SourceWrappedDEK, wrapper) || listEnvelope.NextCursor != "next-cursor" {
		t.Fatalf("list response = %#v / %v", listEnvelope, err)
	}

	stageBody := mustVaultKeyRotationJSON(t, StageVaultKeyRotationRequest{
		ExpectedRotationRowVersion: 1,
		Items: []StageVaultKeyRotationItemRequest{{
			DEKID: testDEKID, ExpectedSourceDEKRowVersion: 1,
			ExpectedSourceWrapRevision: 1, TargetWrappedDEK: wrapper, TargetWrapRevision: 2,
		}},
	})
	resp = secretTestRequest(t, srv.URL, http.MethodPost,
		"/v1/vault/rotations/"+testRotationID+":stage", "agent-token", stageBody, "rotation-stage-1")
	assertVaultKeyRotationEnvelope(t, assertSecretTestResponse(t, resp, http.StatusOK, false), true)
	if staged.ExpectedRotationRowVersion != 1 || staged.IdempotencyKey != "rotation-stage-1" ||
		len(staged.Items) != 1 || !bytes.Equal(staged.Items[0].TargetWrappedDEK, wrapper) {
		t.Fatalf("stage callback input = %#v", staged)
	}

	commitBody := mustVaultKeyRotationJSON(t, CommitVaultKeyRotationRequest{
		ExpectedRotationRowVersion: 2, ExpectedItemCount: 1,
		ExpectedPlanHash: strings.Repeat("c", 64),
		RecoveryDisposition: VaultKeyRotationRecoveryDisposition{
			Mode: VaultKeyRotationRecoveryArtifact, ArtifactSHA256: strings.Repeat("e", 64),
		},
	})
	resp = secretTestRequest(t, srv.URL, http.MethodPost,
		"/v1/vault/rotations/"+testRotationID+":commit", "agent-token", commitBody, "rotation-commit-1")
	assertVaultKeyRotationEnvelope(t, assertSecretTestResponse(t, resp, http.StatusOK, false), true)
	if committed.ExpectedRotationRowVersion != 2 || committed.ExpectedItemCount != 1 ||
		committed.ExpectedPlanHash != strings.Repeat("c", 64) ||
		committed.RecoveryDisposition.Mode != VaultKeyRotationRecoveryArtifact ||
		committed.RecoveryDisposition.ArtifactSHA256 != strings.Repeat("e", 64) ||
		committed.IdempotencyKey != "rotation-commit-1" {
		t.Fatalf("commit callback input = %#v", committed)
	}

	cancelBody := `{"expected_rotation_row_version":2}`
	resp = secretTestRequest(t, srv.URL, http.MethodPost,
		"/v1/vault/rotations/"+testRotationID+":cancel", "agent-token", cancelBody, "rotation-cancel-1")
	assertVaultKeyRotationEnvelope(t, assertSecretTestResponse(t, resp, http.StatusOK, false), true)
	if cancelled.ExpectedRotationRowVersion != 2 || cancelled.IdempotencyKey != "rotation-cancel-1" {
		t.Fatalf("cancel callback input = %#v", cancelled)
	}
}

func TestOpenVaultKeyRotationRouteReturnsNullWithoutParsingLiteralAsID(t *testing.T) {
	openCalls := 0
	idCalls := 0
	srv := httptest.NewServer(apiMux(Config{
		AuthenticatePrincipal: secretTestAuth,
		GetOpenVaultKeyRotation: func(_ context.Context, p DomainPrincipal) (*VaultKeyRotation, error) {
			assertSecretTestPrincipal(t, p)
			openCalls++
			return nil, nil
		},
		GetVaultKeyRotation: func(context.Context, DomainPrincipal, string) (VaultKeyRotation, error) {
			idCalls++
			return VaultKeyRotation{}, nil
		},
	}))
	defer srv.Close()
	resp := secretTestRequest(t, srv.URL, http.MethodGet, "/v1/vault/rotations/open", "agent-token", "", "")
	body := assertSecretTestResponse(t, resp, http.StatusOK, false)
	var envelope struct {
		SchemaVersion string            `json:"schema_version"`
		Rotation      *VaultKeyRotation `json:"rotation"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.SchemaVersion != "witself.v0" || envelope.Rotation != nil {
		t.Fatalf("empty open rotation response = %#v / %v", envelope, err)
	}
	if openCalls != 1 || idCalls != 0 {
		t.Fatalf("open/id callback calls = %d/%d, want 1/0", openCalls, idCalls)
	}
}

func TestValidVaultKeyRotationRecoveryDisposition(t *testing.T) {
	for _, test := range []struct {
		name string
		in   VaultKeyRotationRecoveryDisposition
		want bool
	}{
		{name: "artifact", in: VaultKeyRotationRecoveryDisposition{
			Mode: VaultKeyRotationRecoveryArtifact, ArtifactSHA256: strings.Repeat("a", 64),
		}, want: true},
		{name: "risk", in: VaultKeyRotationRecoveryDisposition{Mode: VaultKeyRotationRiskAccepted}, want: true},
		{name: "missing"},
		{name: "unknown", in: VaultKeyRotationRecoveryDisposition{Mode: "backup_exists"}},
		{name: "artifact no digest", in: VaultKeyRotationRecoveryDisposition{Mode: VaultKeyRotationRecoveryArtifact}},
		{name: "artifact uppercase", in: VaultKeyRotationRecoveryDisposition{
			Mode: VaultKeyRotationRecoveryArtifact, ArtifactSHA256: strings.Repeat("A", 64),
		}},
		{name: "risk with digest", in: VaultKeyRotationRecoveryDisposition{
			Mode: VaultKeyRotationRiskAccepted, ArtifactSHA256: strings.Repeat("a", 64),
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := validVaultKeyRotationRecoveryDisposition(test.in); got != test.want {
				t.Fatalf("valid = %t, want %t", got, test.want)
			}
		})
	}
}

func TestVaultKeyRotationRoutesAreStrictAndSuspendedMayReadLifecycleAndCancel(t *testing.T) {
	calls := 0
	cancelCalls := 0
	mutation := VaultKeyRotationMutationResult{Rotation: VaultKeyRotation{ID: testRotationID}}
	srv := httptest.NewServer(apiMux(Config{
		AuthenticatePrincipal: secretTestAuth,
		StartVaultKeyRotation: func(context.Context, DomainPrincipal, StartVaultKeyRotationRequest) (VaultKeyRotationMutationResult, error) {
			calls++
			return mutation, nil
		},
		GetVaultKeyRotation: func(context.Context, DomainPrincipal, string) (VaultKeyRotation, error) {
			calls++
			return VaultKeyRotation{}, nil
		},
		GetOpenVaultKeyRotation: func(context.Context, DomainPrincipal) (*VaultKeyRotation, error) {
			calls++
			return nil, nil
		},
		ListVaultKeyRotationItems: func(context.Context, DomainPrincipal, string, VaultKeyRotationItemListOptions) (VaultKeyRotationItemPage, error) {
			calls++
			return VaultKeyRotationItemPage{}, nil
		},
		StageVaultKeyRotation: func(context.Context, DomainPrincipal, string, StageVaultKeyRotationRequest) (VaultKeyRotationMutationResult, error) {
			calls++
			return mutation, nil
		},
		CommitVaultKeyRotation: func(context.Context, DomainPrincipal, string, CommitVaultKeyRotationRequest) (VaultKeyRotationMutationResult, error) {
			calls++
			return mutation, nil
		},
		CancelVaultKeyRotation: func(_ context.Context, p DomainPrincipal, _ string, _ CancelVaultKeyRotationRequest) (VaultKeyRotationMutationResult, error) {
			calls++
			cancelCalls++
			if p.AccountStatus != "suspended" {
				t.Fatalf("cancel principal status = %q", p.AccountStatus)
			}
			return mutation, nil
		},
	}))
	defer srv.Close()

	validStart := mustVaultKeyRotationJSON(t, StartVaultKeyRotationRequest{
		ID: testRotationID, ExpectedSourceKeyID: testVaultKeyID,
		ExpectedSourceKeyVersion: 1, ExpectedSourceKeyRowVersion: 1,
		TargetKeyID: testTargetKeyID, TargetKeyVersion: 2,
		TargetAlgorithm: "AES_256_GCM_RANDOM_NONCE_V1", TargetFingerprint: strings.Repeat("a", 64),
	})
	validStage := mustVaultKeyRotationJSON(t, StageVaultKeyRotationRequest{
		ExpectedRotationRowVersion: 1,
		Items: []StageVaultKeyRotationItemRequest{{
			DEKID: testDEKID, ExpectedSourceDEKRowVersion: 1,
			ExpectedSourceWrapRevision: 1,
			TargetWrappedDEK:           bytes.Repeat([]byte{1}, vaultKeyRotationWrappedDEKBytes),
			TargetWrapRevision:         2,
		}},
	})
	duplicateItem := StageVaultKeyRotationItemRequest{
		DEKID: testDEKID, ExpectedSourceDEKRowVersion: 1,
		ExpectedSourceWrapRevision: 1,
		TargetWrappedDEK:           bytes.Repeat([]byte{1}, vaultKeyRotationWrappedDEKBytes),
		TargetWrapRevision:         2,
	}
	duplicateStage := mustVaultKeyRotationJSON(t, StageVaultKeyRotationRequest{
		ExpectedRotationRowVersion: 1,
		Items:                      []StageVaultKeyRotationItemRequest{duplicateItem, duplicateItem},
	})
	validCancel := `{"expected_rotation_row_version":1}`
	tests := []struct {
		name, method, path, token, body, key string
		want                                 int
	}{
		{name: "missing auth", method: http.MethodGet, path: "/v1/vault/rotations/" + testRotationID, want: http.StatusUnauthorized},
		{name: "operator", method: http.MethodGet, path: "/v1/vault/rotations/" + testRotationID, token: "operator-token", want: http.StatusForbidden},
		{name: "restricted", method: http.MethodPost, path: "/v1/vault/rotations", token: "curator-token", body: validStart, key: "start-1", want: http.StatusForbidden},
		{name: "suspended exact read", method: http.MethodGet, path: "/v1/vault/rotations/" + testRotationID, token: "suspended-token", want: http.StatusOK},
		{name: "suspended open read", method: http.MethodGet, path: "/v1/vault/rotations/open", token: "suspended-token", want: http.StatusOK},
		{name: "suspended stage", method: http.MethodPost, path: "/v1/vault/rotations/" + testRotationID + ":stage", token: "suspended-token", body: validStage, key: "stage-1", want: http.StatusForbidden},
		{name: "suspended cancel", method: http.MethodPost, path: "/v1/vault/rotations/" + testRotationID + ":cancel", token: "suspended-token", body: validCancel, key: "cancel-1", want: http.StatusOK},
		{name: "start identity authority", method: http.MethodPost, path: "/v1/vault/rotations", token: "agent-token", body: strings.TrimSuffix(validStart, "}") + `,"owner_agent_id":"agent_2"}`, key: "start-2", want: http.StatusBadRequest},
		{name: "start missing key", method: http.MethodPost, path: "/v1/vault/rotations", token: "agent-token", body: validStart, want: http.StatusBadRequest},
		{name: "start bad id", method: http.MethodPost, path: "/v1/vault/rotations", token: "agent-token", body: strings.Replace(validStart, testRotationID, "vkr_bad", 1), key: "start-3", want: http.StatusBadRequest},
		{name: "start skips logical version", method: http.MethodPost, path: "/v1/vault/rotations", token: "agent-token", body: strings.Replace(validStart, `"target_key_version":2`, `"target_key_version":3`, 1), key: "start-4", want: http.StatusBadRequest},
		{name: "get query", method: http.MethodGet, path: "/v1/vault/rotations/" + testRotationID + "?owner_agent_id=agent_2", token: "agent-token", want: http.StatusBadRequest},
		{name: "list bad path", method: http.MethodGet, path: "/v1/vault/rotations/vkr_bad/items", token: "agent-token", want: http.StatusNotFound},
		{name: "list unknown query", method: http.MethodGet, path: "/v1/vault/rotations/" + testRotationID + "/items?owner_agent_id=agent_2", token: "agent-token", want: http.StatusBadRequest},
		{name: "list duplicate limit", method: http.MethodGet, path: "/v1/vault/rotations/" + testRotationID + "/items?limit=1&limit=2", token: "agent-token", want: http.StatusBadRequest},
		{name: "list empty cursor", method: http.MethodGet, path: "/v1/vault/rotations/" + testRotationID + "/items?cursor=", token: "agent-token", want: http.StatusBadRequest},
		{name: "stage duplicate item", method: http.MethodPost, path: "/v1/vault/rotations/" + testRotationID + ":stage", token: "agent-token", body: duplicateStage, key: "stage-2", want: http.StatusBadRequest},
		{name: "stage unknown field", method: http.MethodPost, path: "/v1/vault/rotations/" + testRotationID + ":stage", token: "agent-token", body: `{"expected_rotation_row_version":1,"items":[],"agent_id":"agent_2"}`, key: "stage-3", want: http.StatusBadRequest},
		{name: "commit invalid hash", method: http.MethodPost, path: "/v1/vault/rotations/" + testRotationID + ":commit", token: "agent-token", body: `{"expected_rotation_row_version":2,"expected_item_count":1,"expected_plan_hash":"bad","recovery_disposition":{"mode":"risk_accepted"}}`, key: "commit-1", want: http.StatusBadRequest},
		{name: "commit missing disposition", method: http.MethodPost, path: "/v1/vault/rotations/" + testRotationID + ":commit", token: "agent-token", body: `{"expected_rotation_row_version":2,"expected_item_count":1,"expected_plan_hash":"` + strings.Repeat("a", 64) + `"}`, key: "commit-2", want: http.StatusBadRequest},
		{name: "commit unknown disposition", method: http.MethodPost, path: "/v1/vault/rotations/" + testRotationID + ":commit", token: "agent-token", body: `{"expected_rotation_row_version":2,"expected_item_count":1,"expected_plan_hash":"` + strings.Repeat("a", 64) + `","recovery_disposition":{"mode":"backup_exists"}}`, key: "commit-3", want: http.StatusBadRequest},
		{name: "commit artifact missing digest", method: http.MethodPost, path: "/v1/vault/rotations/" + testRotationID + ":commit", token: "agent-token", body: `{"expected_rotation_row_version":2,"expected_item_count":1,"expected_plan_hash":"` + strings.Repeat("a", 64) + `","recovery_disposition":{"mode":"recovery_artifact"}}`, key: "commit-4", want: http.StatusBadRequest},
		{name: "commit artifact uppercase digest", method: http.MethodPost, path: "/v1/vault/rotations/" + testRotationID + ":commit", token: "agent-token", body: `{"expected_rotation_row_version":2,"expected_item_count":1,"expected_plan_hash":"` + strings.Repeat("a", 64) + `","recovery_disposition":{"mode":"recovery_artifact","artifact_sha256":"` + strings.Repeat("A", 64) + `"}}`, key: "commit-5", want: http.StatusBadRequest},
		{name: "commit risk with digest", method: http.MethodPost, path: "/v1/vault/rotations/" + testRotationID + ":commit", token: "agent-token", body: `{"expected_rotation_row_version":2,"expected_item_count":1,"expected_plan_hash":"` + strings.Repeat("a", 64) + `","recovery_disposition":{"mode":"risk_accepted","artifact_sha256":"` + strings.Repeat("a", 64) + `"}}`, key: "commit-6", want: http.StatusBadRequest},
		{name: "cancel missing revision", method: http.MethodPost, path: "/v1/vault/rotations/" + testRotationID + ":cancel", token: "agent-token", body: `{}`, key: "cancel-2", want: http.StatusBadRequest},
		{name: "action query", method: http.MethodPost, path: "/v1/vault/rotations/" + testRotationID + ":cancel?force=true", token: "agent-token", body: validCancel, key: "cancel-3", want: http.StatusBadRequest},
		{name: "invalid action", method: http.MethodPost, path: "/v1/vault/rotations/" + testRotationID + ":delete", token: "agent-token", body: `{}`, key: "delete-1", want: http.StatusNotFound},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resp := secretTestRequest(t, srv.URL, test.method, test.path, test.token, test.body, test.key)
			defer closeBody(t, resp)
			if resp.StatusCode != test.want {
				t.Fatalf("status = %d, want %d; body=%s", resp.StatusCode, test.want, secretTestResponseBody(t, resp))
			}
			if got := resp.Header.Get("Cache-Control"); got != "private, no-store" {
				t.Fatalf("Cache-Control = %q", got)
			}
		})
	}
	if calls != 3 || cancelCalls != 1 {
		t.Fatalf("callback calls = %d, cancel calls = %d, want 3 / 1", calls, cancelCalls)
	}
}

func mustVaultKeyRotationJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func assertVaultKeyRotationEnvelope(t *testing.T, raw []byte, wantReceipt bool) {
	t.Helper()
	var envelope struct {
		SchemaVersion string                   `json:"schema_version"`
		Rotation      VaultKeyRotation         `json:"rotation"`
		Receipt       *VaultKeyRotationReceipt `json:"receipt"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.SchemaVersion != "witself.v0" ||
		envelope.Rotation.ID != testRotationID || (wantReceipt && envelope.Receipt == nil) ||
		(!wantReceipt && envelope.Receipt != nil) ||
		envelope.Rotation.SourceKeyAlgorithm != "AES_256_GCM_RANDOM_NONCE_V1" ||
		envelope.Rotation.SourceKeyFingerprint != strings.Repeat("a", 64) ||
		envelope.Rotation.TargetKeyAlgorithm != "AES_256_GCM_RANDOM_NONCE_V1" ||
		envelope.Rotation.TargetKeyFingerprint != strings.Repeat("b", 64) {
		t.Fatalf("rotation response = %#v / %v", envelope, err)
	}
}
