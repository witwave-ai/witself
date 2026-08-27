package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAgentEmailRealmAliasSystemEndpoints(t *testing.T) {
	now := time.Date(2026, 8, 1, 18, 0, 0, 0, time.UTC)
	want := AgentEmailRealmAlias{
		ClaimID: "era_aaaaaaaaaaaaaaaa", AccountID: "acc_1",
		RealmID: "realm_abcdefghijkl2345", Domain: "agent-mail.witwave.ai",
		RealmLabel: "founder", State: "applied", ControllerRevision: 7,
		CreatedAt: now, UpdatedAt: now,
	}
	var gotAccount string
	var gotRequest AgentEmailRealmAliasApplyRequest
	applyCalls := 0
	cfg := Config{
		ProvisionToken: "witself_prv_test",
		ProvisionAccountExact: func(context.Context, string, string, string, string, string) (ProvisionedAccount, error) {
			return ProvisionedAccount{}, nil
		},
		ApplyAgentEmailRealmAlias: func(
			_ context.Context,
			accountID string,
			in AgentEmailRealmAliasApplyRequest,
		) (AgentEmailRealmAlias, error) {
			applyCalls++
			gotAccount, gotRequest = accountID, in
			switch accountID {
			case "acc_bad":
				return AgentEmailRealmAlias{}, ErrBadInput
			case "acc_missing":
				return AgentEmailRealmAlias{}, ErrNotFound
			case "acc_stale":
				return AgentEmailRealmAlias{}, ErrConflict
			}
			return want, nil
		},
		GetAgentEmailRealmAlias: func(_ context.Context, accountID, claimID string) (AgentEmailRealmAlias, error) {
			if accountID == "acc_missing" || claimID != want.ClaimID {
				return AgentEmailRealmAlias{}, ErrNotFound
			}
			return want, nil
		},
		GetAgentEmailRealmAliasTarget: func(_ context.Context, accountID, realmID string) (AgentEmailRealmAliasTarget, error) {
			if accountID == "acc_bad" {
				return AgentEmailRealmAliasTarget{}, ErrBadInput
			}
			if accountID == "acc_missing" || realmID != want.RealmID {
				return AgentEmailRealmAliasTarget{}, ErrNotFound
			}
			return AgentEmailRealmAliasTarget{
				AccountID: accountID,
				RealmID:   realmID,
				Exists:    true,
			}, nil
		},
		ListAgentEmailRealmAliases: func(_ context.Context, accountID string) ([]AgentEmailRealmAlias, error) {
			if accountID == "acc_missing" {
				return nil, ErrNotFound
			}
			return []AgentEmailRealmAlias{want}, nil
		},
	}
	srv := httptest.NewServer(apiMux(cfg))
	t.Cleanup(srv.Close)
	body := `{"claim_id":"era_aaaaaaaaaaaaaaaa","realm_id":"realm_abcdefghijkl2345","domain":"agent-mail.witwave.ai","realm_label":"founder","state":"applied","controller_revision":7}`

	post := func(accountID, token, requestBody string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(
			"POST", srv.URL+"/v1/accounts/"+accountID+":email-realm-alias",
			bytes.NewBufferString(requestBody),
		)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = resp.Body.Close() })
		return resp
	}
	resp := post("acc_1", "witself_prv_test", body)
	if resp.StatusCode != http.StatusOK || gotAccount != "acc_1" ||
		gotRequest.ClaimID != want.ClaimID || gotRequest.ControllerRevision != 7 {
		t.Fatalf("apply status/callback = %d / %q / %+v", resp.StatusCode, gotAccount, gotRequest)
	}
	var applied AgentEmailRealmAlias
	if err := json.NewDecoder(resp.Body).Decode(&applied); err != nil ||
		applied.ClaimID != want.ClaimID || applied.ControllerRevision != 7 {
		t.Fatalf("apply response = %+v / %v", applied, err)
	}
	if resp := post("acc_1", "wrong", body); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token status = %d", resp.StatusCode)
	}
	if resp := post("acc_1", "witself_prv_test", body[:len(body)-1]+`,"extra":true}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown field status = %d", resp.StatusCode)
	}
	if applyCalls != 1 {
		t.Fatalf("apply calls after boundary refusals = %d", applyCalls)
	}
	for accountID, wantStatus := range map[string]int{
		"acc_bad": http.StatusBadRequest, "acc_missing": http.StatusNotFound,
		"acc_stale": http.StatusConflict,
	} {
		if resp := post(accountID, "witself_prv_test", body); resp.StatusCode != wantStatus {
			t.Fatalf("%s status = %d; want %d", accountID, resp.StatusCode, wantStatus)
		}
	}

	get := func(path, token string) *http.Response {
		t.Helper()
		req, err := http.NewRequest("GET", srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = resp.Body.Close() })
		return resp
	}
	resp = get("/v1/accounts/acc_1:email-realm-alias?claim_id="+want.ClaimID, "witself_prv_test")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get fence status = %d", resp.StatusCode)
	}
	if resp := get("/v1/accounts/acc_1:email-realm-alias", "witself_prv_test"); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing claim status = %d", resp.StatusCode)
	}
	resp = get("/v1/accounts/acc_1:email-realm-alias-target?realm_id="+want.RealmID, "witself_prv_test")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("target preflight status = %d", resp.StatusCode)
	}
	var target map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&target); err != nil ||
		len(target) != 3 || target["account_id"] != "acc_1" ||
		target["realm_id"] != want.RealmID || target["exists"] != true {
		t.Fatalf("target preflight response = %#v / %v", target, err)
	}
	if resp := get("/v1/accounts/acc_1:email-realm-alias-target", "witself_prv_test"); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing target realm status = %d", resp.StatusCode)
	}
	if resp := get("/v1/accounts/acc_bad:email-realm-alias-target?realm_id="+want.RealmID, "witself_prv_test"); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid target status = %d", resp.StatusCode)
	}
	if resp := get("/v1/accounts/acc_1:email-realm-alias-target?realm_id=realm_bbbbbbbbbbbbbbbb", "witself_prv_test"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown target realm status = %d", resp.StatusCode)
	}
	resp = get("/v1/accounts/acc_1:email-realm-aliases", "witself_prv_test")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d", resp.StatusCode)
	}
	var listed struct {
		SchemaVersion string                 `json:"schema_version"`
		AccountID     string                 `json:"account_id"`
		Aliases       []AgentEmailRealmAlias `json:"aliases"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil ||
		listed.SchemaVersion != "witself.v0" || listed.AccountID != "acc_1" ||
		len(listed.Aliases) != 1 || listed.Aliases[0].ClaimID != want.ClaimID {
		t.Fatalf("list response = %+v / %v", listed, err)
	}
}
