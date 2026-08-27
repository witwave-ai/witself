package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRealmEmailRouteSystemEndpoints(t *testing.T) {
	const (
		token   = "provision-secret"
		account = "acc_routes"
		realm   = "realm_aaaaaaaaaaaaaaaa"
	)
	live := RealmEmailRouteLifecycle{
		AccountID: account, RealmID: realm, State: "live", Generation: 1,
	}
	closing := RealmEmailRouteLifecycle{
		AccountID: account, RealmID: realm, State: "closing", Generation: 2,
		OperationID: "close:customer.request-1",
	}
	retired := closing
	retired.State = "retired"
	var listedCursor string
	var listedLimit int
	var prepared, committed RealmEmailRouteRetirementRequest
	cfg := Config{
		ProvisionToken: token,
		ProvisionAccountExact: func(context.Context, string, string, string, string, string) (ProvisionedAccount, error) {
			return ProvisionedAccount{}, nil
		},
		GetRealmEmailRouteLifecycle: func(_ context.Context, accountID, realmID string) (RealmEmailRouteLifecycle, error) {
			switch {
			case accountID == "bad":
				return RealmEmailRouteLifecycle{}, ErrBadInput
			case accountID != account || realmID != realm:
				return RealmEmailRouteLifecycle{}, ErrNotFound
			default:
				return live, nil
			}
		},
		ListRealmEmailRouteLifecycles: func(_ context.Context, accountID, cursor string, limit int) (RealmEmailRouteLifecyclePage, error) {
			if accountID == "bad" {
				return RealmEmailRouteLifecyclePage{}, ErrBadInput
			}
			if accountID != account {
				return RealmEmailRouteLifecyclePage{}, ErrNotFound
			}
			listedCursor, listedLimit = cursor, limit
			return RealmEmailRouteLifecyclePage{
				Routes: []RealmEmailRouteLifecycle{live}, NextCursor: "next-page",
			}, nil
		},
		PrepareRealmEmailRouteRetirement: func(_ context.Context, accountID string, in RealmEmailRouteRetirementRequest) (RealmEmailRouteLifecycle, error) {
			prepared = in
			if accountID == "bad" {
				return RealmEmailRouteLifecycle{}, ErrBadInput
			}
			if accountID == "missing" {
				return RealmEmailRouteLifecycle{}, ErrNotFound
			}
			if accountID == "stale" {
				return RealmEmailRouteLifecycle{}, ErrConflict
			}
			return closing, nil
		},
		CommitRealmEmailRouteRetirement: func(_ context.Context, accountID string, in RealmEmailRouteRetirementRequest) (RealmEmailRouteLifecycle, error) {
			committed = in
			if accountID == "stale" {
				return RealmEmailRouteLifecycle{}, ErrConflict
			}
			return retired, nil
		},
	}
	srv := httptest.NewServer(apiMux(cfg))
	t.Cleanup(srv.Close)

	request := func(method, path, bearer, body string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, srv.URL+path, bytes.NewBufferString(body))
		if err != nil {
			t.Fatal(err)
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = resp.Body.Close() })
		return resp
	}

	resp := request(http.MethodGet,
		"/v1/accounts/"+account+":email-realm-route?realm_id="+realm,
		token, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("exact get status = %d", resp.StatusCode)
	}
	var got RealmEmailRouteLifecycle
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil || got != live {
		t.Fatalf("exact route = %+v / %v", got, err)
	}
	if resp := request(http.MethodGet,
		"/v1/accounts/"+account+":email-realm-route", token, ""); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing realm status = %d", resp.StatusCode)
	}
	if resp := request(http.MethodGet,
		"/v1/accounts/"+account+":email-realm-route?realm_id=realm_legacy", token, ""); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid realm status = %d", resp.StatusCode)
	}
	if resp := request(http.MethodGet,
		"/v1/accounts/missing:email-realm-route?realm_id="+realm, token, ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing exact status = %d", resp.StatusCode)
	}

	resp = request(http.MethodGet,
		"/v1/accounts/"+account+":email-realm-routes?limit=17&cursor=opaque",
		token, "")
	if resp.StatusCode != http.StatusOK || listedLimit != 17 || listedCursor != "opaque" {
		t.Fatalf("inventory status/options = %d / %d / %q", resp.StatusCode, listedLimit, listedCursor)
	}
	var page struct {
		SchemaVersion string                     `json:"schema_version"`
		AccountID     string                     `json:"account_id"`
		Routes        []RealmEmailRouteLifecycle `json:"routes"`
		NextCursor    *string                    `json:"next_cursor"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil ||
		page.SchemaVersion != "witself.v0" || page.AccountID != account ||
		len(page.Routes) != 1 || page.NextCursor == nil || *page.NextCursor != "next-page" {
		t.Fatalf("inventory response = %+v / %v", page, err)
	}
	for _, path := range []string{
		"/v1/accounts/" + account + ":email-realm-routes?limit=0",
		"/v1/accounts/" + account + ":email-realm-routes?limit=101",
	} {
		if resp := request(http.MethodGet, path, token, ""); resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("bad limit %q status = %d", path, resp.StatusCode)
		}
	}

	prepareBody := `{"realm_id":"` + realm + `","operation_id":"close:customer.request-1","expected_generation":1}`
	resp = request(http.MethodPost,
		"/v1/accounts/"+account+":prepare-email-realm-route-retirement",
		token, prepareBody)
	if resp.StatusCode != http.StatusOK || prepared.RealmID != realm ||
		prepared.OperationID != "close:customer.request-1" || prepared.ExpectedGeneration != 1 {
		t.Fatalf("prepare status/input = %d / %+v", resp.StatusCode, prepared)
	}
	commitBody := `{"realm_id":"` + realm + `","operation_id":"close:customer.request-1","expected_generation":2}`
	resp = request(http.MethodPost,
		"/v1/accounts/"+account+":commit-email-realm-route-retirement",
		token, commitBody)
	if resp.StatusCode != http.StatusOK || committed.ExpectedGeneration != 2 {
		t.Fatalf("commit status/input = %d / %+v", resp.StatusCode, committed)
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil || got != retired {
		t.Fatalf("commit route = %+v / %v", got, err)
	}
	for accountID, wantStatus := range map[string]int{
		"bad": http.StatusBadRequest, "missing": http.StatusNotFound,
		"stale": http.StatusConflict,
	} {
		if resp := request(http.MethodPost,
			"/v1/accounts/"+accountID+":prepare-email-realm-route-retirement",
			token, prepareBody); resp.StatusCode != wantStatus {
			t.Fatalf("prepare %s status = %d, want %d", accountID, resp.StatusCode, wantStatus)
		}
	}
	if resp := request(http.MethodPost,
		"/v1/accounts/"+account+":prepare-email-realm-route-retirement",
		"wrong", prepareBody); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token status = %d", resp.StatusCode)
	}
	if resp := request(http.MethodPost,
		"/v1/accounts/"+account+":prepare-email-realm-route-retirement",
		token, prepareBody[:len(prepareBody)-1]+`,"extra":true}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown field status = %d", resp.StatusCode)
	}
	if resp := request(http.MethodPost,
		"/v1/accounts/"+account+":prepare-email-realm-route-retirement",
		token, `{"realm_id":"realm_legacy","operation_id":"close:customer.request-1","expected_generation":1}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid realm retirement status = %d", resp.StatusCode)
	}
}

func TestManagedRealmDeleteReturnsSpecificConflict(t *testing.T) {
	auth := func(context.Context, string) (string, string, string, bool, error) {
		return "opr_owner", "acc_1", "active", true, nil
	}
	srv := httptest.NewServer(apiMux(Config{
		Authenticate: auth,
		DeleteRealm: func(context.Context, string, string) error {
			return ErrRealmEmailRouteRetirementRequired
		},
	}))
	t.Cleanup(srv.Close)
	req, _ := http.NewRequest(http.MethodDelete,
		srv.URL+"/v1/realms/realm_aaaaaaaaaaaaaaaa", nil)
	req.Header.Set("Authorization", "Bearer operator")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("managed delete status = %d", resp.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil ||
		body["error"] != "realm deletion requires managed email route retirement" {
		t.Fatalf("managed delete body = %#v / %v", body, err)
	}
}
