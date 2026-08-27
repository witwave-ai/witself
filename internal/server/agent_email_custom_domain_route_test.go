package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAgentEmailCustomDomainRouteSystemEndpoints(t *testing.T) {
	want := AgentEmailCustomDomainRoute{
		SchemaVersion: "witself.v0", AccountID: "acc_1",
		DomainRequestID:          "aedr_aaaaaaaaaaaaaaaa",
		DomainAllocationRevision: 3, DomainStateRevision: 8,
		RealmAliasClaimID: "era_bbbbbbbbbbbbbbbb", RealmAliasRevision: 5,
		RealmID: "realm_cccccccccccccccc", Domain: "agents.example.com",
		RealmLabel: "founder", State: "suspended",
		SuspensionDisposition: "retry", ControllerRevision: 11,
	}
	var applied AgentEmailCustomDomainRouteApplyRequest
	applyCalls := 0
	cfg := Config{
		ProvisionToken: "witself_prv_test",
		ProvisionAccountExact: func(context.Context, string, string, string, string, string) (ProvisionedAccount, error) {
			return ProvisionedAccount{}, nil
		},
		ApplyAgentEmailCustomDomainRoute: func(
			_ context.Context, accountID string,
			in AgentEmailCustomDomainRouteApplyRequest,
		) (AgentEmailCustomDomainRoute, error) {
			applyCalls++
			applied = in
			switch accountID {
			case "acc_bad":
				return AgentEmailCustomDomainRoute{}, ErrBadInput
			case "acc_missing":
				return AgentEmailCustomDomainRoute{}, ErrNotFound
			case "acc_stale":
				return AgentEmailCustomDomainRoute{}, ErrConflict
			}
			return want, nil
		},
		GetAgentEmailCustomDomainRoute: func(
			_ context.Context, accountID, domainRequestID, realmAliasClaimID string,
		) (AgentEmailCustomDomainRoute, error) {
			if accountID == "acc_bad" {
				return AgentEmailCustomDomainRoute{}, ErrBadInput
			}
			if accountID != want.AccountID || domainRequestID != want.DomainRequestID ||
				realmAliasClaimID != want.RealmAliasClaimID {
				return AgentEmailCustomDomainRoute{}, ErrNotFound
			}
			return want, nil
		},
	}
	srv := httptest.NewServer(apiMux(cfg))
	t.Cleanup(srv.Close)
	body, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}

	post := func(accountID, token string, raw []byte) *http.Response {
		t.Helper()
		req, err := http.NewRequest(
			http.MethodPost,
			srv.URL+"/v1/accounts/"+accountID+":email-custom-domain-route",
			bytes.NewReader(raw),
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
	if resp.StatusCode != http.StatusOK || applied.DomainRequestID != want.DomainRequestID ||
		applied.RealmAliasRevision != want.RealmAliasRevision {
		t.Fatalf("apply status/request = %d / %+v", resp.StatusCode, applied)
	}
	var response map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil ||
		len(response) != 13 || response["schema_version"] != "witself.v0" ||
		response["domain_request_id"] != want.DomainRequestID ||
		response["suspension_disposition"] != "retry" {
		t.Fatalf("apply response = %#v / %v", response, err)
	}
	if resp := post("acc_2", "witself_prv_test", body); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("path/body account mismatch status = %d", resp.StatusCode)
	}
	var wrongSchema map[string]any
	if err := json.Unmarshal(body, &wrongSchema); err != nil {
		t.Fatal(err)
	}
	wrongSchema["schema_version"] = "witself.future"
	wrongSchemaBody, _ := json.Marshal(wrongSchema)
	if resp := post("acc_1", "witself_prv_test", wrongSchemaBody); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("wrong schema status = %d", resp.StatusCode)
	}
	delete(wrongSchema, "schema_version")
	missingSchemaBody, _ := json.Marshal(wrongSchema)
	if resp := post("acc_1", "witself_prv_test", missingSchemaBody); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing schema status = %d", resp.StatusCode)
	}
	wrongSchema["schema_version"] = "witself.v0"
	wrongSchema["unknown"] = true
	unknownBody, _ := json.Marshal(wrongSchema)
	if resp := post("acc_1", "witself_prv_test", unknownBody); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown field status = %d", resp.StatusCode)
	}
	if resp := post("acc_1", "wrong", body); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token status = %d", resp.StatusCode)
	}
	request, err := http.NewRequest(
		http.MethodPost,
		srv.URL+"/v1/accounts/acc_1/email-custom-domain-route",
		bytes.NewReader(body),
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer witself_prv_test")
	if resp, err := http.DefaultClient.Do(request); err != nil {
		t.Fatal(err)
	} else {
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("slash-form apply status = %d", resp.StatusCode)
		}
	}
	if applyCalls != 1 {
		t.Fatalf("apply calls after boundary refusals = %d", applyCalls)
	}
	for accountID, wantStatus := range map[string]int{
		"acc_bad": http.StatusBadRequest, "acc_missing": http.StatusNotFound,
		"acc_stale": http.StatusConflict,
	} {
		var request AgentEmailCustomDomainRoute
		if err := json.Unmarshal(body, &request); err != nil {
			t.Fatal(err)
		}
		request.AccountID = accountID
		raw, _ := json.Marshal(request)
		if resp := post(accountID, "witself_prv_test", raw); resp.StatusCode != wantStatus {
			t.Fatalf("%s status = %d; want %d", accountID, resp.StatusCode, wantStatus)
		}
	}

	get := func(path, token string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
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
	path := "/v1/accounts/acc_1:email-custom-domain-route?domain_request_id=" +
		want.DomainRequestID + "&realm_alias_claim_id=" + want.RealmAliasClaimID
	if resp := get(path, "witself_prv_test"); resp.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d", resp.StatusCode)
	}
	if resp := get("/v1/accounts/acc_1:email-custom-domain-route", "witself_prv_test"); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing query status = %d", resp.StatusCode)
	}
	if resp := get(path, "wrong"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("get wrong token status = %d", resp.StatusCode)
	}
	if resp := get(
		"/v1/accounts/acc_1/email-custom-domain-route?domain_request_id="+
			want.DomainRequestID+"&realm_alias_claim_id="+want.RealmAliasClaimID,
		"witself_prv_test",
	); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("slash-form get status = %d", resp.StatusCode)
	}
}
