package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSuspendedAgentMayReadAndCancelVaultKeyEnrollmentOnly(t *testing.T) {
	const enrollmentID = "enr_abcdefghijklmnop"
	wrongCalls := 0
	cancelCalls := 0
	getCalls := 0
	listCalls := 0
	srv := httptest.NewServer(apiMux(Config{
		AuthenticatePrincipal: secretTestAuth,
		CreateVaultKeyEnrollment: func(context.Context, DomainPrincipal, CreateVaultKeyEnrollmentRequest) (VaultKeyEnrollment, error) {
			wrongCalls++
			return VaultKeyEnrollment{}, nil
		},
		GetVaultKeyEnrollment: func(_ context.Context, p DomainPrincipal, id string) (VaultKeyEnrollment, error) {
			getCalls++
			if p.AccountStatus != "suspended" || id != enrollmentID {
				t.Fatalf("get callback = principal %#v, id %q", p, id)
			}
			return VaultKeyEnrollment{ID: id, AccountID: p.AccountID, RealmID: p.RealmID,
				OwnerAgentID: p.ID, LifecycleState: "pending", RowVersion: 2}, nil
		},
		ListVaultKeyEnrollments: func(_ context.Context, p DomainPrincipal, options VaultKeyEnrollmentListOptions) ([]VaultKeyEnrollment, error) {
			listCalls++
			if p.AccountStatus != "suspended" || options.State != "pending" || options.Limit != 5 {
				t.Fatalf("list callback = principal %#v, options %#v", p, options)
			}
			return []VaultKeyEnrollment{{ID: enrollmentID, AccountID: p.AccountID,
				RealmID: p.RealmID, OwnerAgentID: p.ID, LifecycleState: "pending", RowVersion: 2}}, nil
		},
		ApproveVaultKeyEnrollment: func(context.Context, DomainPrincipal, string, ApproveVaultKeyEnrollmentRequest) (VaultKeyEnrollment, error) {
			wrongCalls++
			return VaultKeyEnrollment{}, nil
		},
		ReceiveVaultKeyEnrollment: func(context.Context, DomainPrincipal, string, string) (VaultKeyEnrollmentTransfer, error) {
			wrongCalls++
			return VaultKeyEnrollmentTransfer{}, nil
		},
		ConsumeVaultKeyEnrollment: func(context.Context, DomainPrincipal, string, ConsumeVaultKeyEnrollmentRequest) (VaultKeyEnrollment, error) {
			wrongCalls++
			return VaultKeyEnrollment{}, nil
		},
		CancelVaultKeyEnrollment: func(_ context.Context, p DomainPrincipal, id string, in CancelVaultKeyEnrollmentRequest) (VaultKeyEnrollment, error) {
			cancelCalls++
			if p.AccountStatus != "suspended" || id != enrollmentID ||
				in.ExpectedRowVersion != 2 || in.IdempotencyKey != "enrollment-cancel-1" {
				t.Fatalf("cancel callback = principal %#v, id %q, input %#v", p, id, in)
			}
			return VaultKeyEnrollment{ID: id, LifecycleState: "cancelled", RowVersion: 3}, nil
		},
	}))
	defer srv.Close()

	resp := secretTestRequest(t, srv.URL, http.MethodPost,
		"/v1/vault/enrollments", "suspended-token", `{}`, "create-1")
	defer closeBody(t, resp)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("suspended create status = %d, want 403", resp.StatusCode)
	}
	for _, action := range []string{"approve", "receive", "consume"} {
		resp = secretTestRequest(t, srv.URL, http.MethodPost,
			"/v1/vault/enrollments/"+enrollmentID+":"+action,
			"suspended-token", `{}`, action+"-1")
		defer closeBody(t, resp)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("suspended %s status = %d, want 403", action, resp.StatusCode)
		}
	}
	resp = secretTestRequest(t, srv.URL, http.MethodGet,
		"/v1/vault/enrollments/"+enrollmentID, "suspended-token", "", "")
	_ = assertSecretTestResponse(t, resp, http.StatusOK, false)
	resp = secretTestRequest(t, srv.URL, http.MethodGet,
		"/v1/vault/enrollments?state=pending&limit=5", "suspended-token", "", "")
	_ = assertSecretTestResponse(t, resp, http.StatusOK, false)
	if getCalls != 1 || listCalls != 1 {
		t.Fatalf("suspended value-free reads = get %d, list %d", getCalls, listCalls)
	}
	resp = secretTestRequest(t, srv.URL, http.MethodPost,
		"/v1/vault/enrollments/"+enrollmentID+":cancel",
		"suspended-token", `{"expected_row_version":2}`, "enrollment-cancel-1")
	body := assertSecretTestResponse(t, resp, http.StatusOK, false)
	if string(body) == "" || wrongCalls != 0 || cancelCalls != 1 {
		t.Fatalf("response/callbacks = %q, wrong %d, cancel %d", body, wrongCalls, cancelCalls)
	}

	resp = secretTestRequest(t, srv.URL, http.MethodPost,
		"/v1/vault/enrollments/"+enrollmentID+":cancel",
		"curator-token", `{"expected_row_version":2}`, "enrollment-cancel-2")
	defer closeBody(t, resp)
	if resp.StatusCode != http.StatusForbidden || cancelCalls != 1 {
		t.Fatalf("restricted cancel status/calls = %d/%d", resp.StatusCode, cancelCalls)
	}
	resp = secretTestRequest(t, srv.URL, http.MethodGet,
		"/v1/vault/enrollments/"+enrollmentID, "curator-token", "", "")
	defer closeBody(t, resp)
	if resp.StatusCode != http.StatusForbidden || getCalls != 1 {
		t.Fatalf("restricted get status/calls = %d/%d", resp.StatusCode, getCalls)
	}
}
