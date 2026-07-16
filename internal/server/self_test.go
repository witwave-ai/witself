package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSelfDigestUsesAgentTokenIdentity(t *testing.T) {
	auth := func(_ context.Context, token string) (DomainPrincipal, bool, error) {
		switch token {
		case "agent-token":
			return DomainPrincipal{
				Kind: PrincipalKindAgent, ID: "agent_1", AgentName: "scott",
				AccountID: "acc_1", RealmID: "realm_1", RealmName: "default",
				AccountStatus: "active",
			}, true, nil
		case "operator-token":
			return DomainPrincipal{Kind: PrincipalKindOperator, ID: "opr_1", AccountID: "acc_1", AccountStatus: "active"}, true, nil
		case "suspended-token":
			return DomainPrincipal{Kind: PrincipalKindAgent, ID: "agent_1", AccountID: "acc_1", AccountStatus: "suspended"}, true, nil
		default:
			return DomainPrincipal{}, false, nil
		}
	}
	srv := httptest.NewServer(apiMux(Config{AuthenticatePrincipal: auth}))
	defer srv.Close()

	resp := selfRequest(t, srv.URL+"/v1/self?include_facts=true&include_salient=true&salient_limit=10&max_bytes=8192", "agent-token")
	defer closeBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("agent self = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("self Cache-Control = %q", got)
	}
	var digest SelfDigest
	if err := json.NewDecoder(resp.Body).Decode(&digest); err != nil {
		t.Fatal(err)
	}
	if digest.SchemaVersion != "witself.v0" {
		t.Errorf("schema_version = %q", digest.SchemaVersion)
	}
	if digest.Identity != (SelfIdentity{AccountID: "acc_1", AgentID: "agent_1", AgentName: "scott", RealmID: "realm_1", RealmName: "default"}) {
		t.Errorf("identity = %+v", digest.Identity)
	}
	if digest.PrimaryFacts == nil || digest.SalientMemories == nil || digest.Index.Kinds == nil || digest.Index.Tags == nil {
		t.Fatalf("empty collections must be JSON arrays: %+v", digest)
	}
	if digest.Index.Counts["facts"] != 0 || digest.Index.Counts["memories"] != 0 || digest.Elided {
		t.Errorf("identity-only digest = %+v", digest)
	}
}

func TestSelfDigestIncludesValueFreeMemoryCheckpoint(t *testing.T) {
	auth := func(context.Context, string) (DomainPrincipal, bool, error) {
		return DomainPrincipal{
			Kind: PrincipalKindAgent, ID: "agent_1", AgentName: "scott",
			AccountID: "acc_1", RealmID: "realm_1", RealmName: "default", AccountStatus: "active",
		}, true, nil
	}
	dueAt := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	var callbackPrincipal DomainPrincipal
	srv := httptest.NewServer(apiMux(Config{
		AuthenticatePrincipal: auth,
		GetSelfMemoryCheckpoint: func(_ context.Context, p DomainPrincipal) (*SelfMemoryCheckpoint, error) {
			callbackPrincipal = p
			return &SelfMemoryCheckpoint{
				Pending: true, RequestID: "mcrq_1", RequestGeneration: 9, DueAt: &dueAt,
			}, nil
		},
	}))
	defer srv.Close()

	resp := selfRequest(t, srv.URL+"/v1/self", "agent-token")
	defer closeBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("self status = %d", resp.StatusCode)
	}
	var digest SelfDigest
	if err := json.NewDecoder(resp.Body).Decode(&digest); err != nil {
		t.Fatal(err)
	}
	if callbackPrincipal.ID != "agent_1" || callbackPrincipal.AccountID != "acc_1" || callbackPrincipal.RealmID != "realm_1" {
		t.Fatalf("checkpoint callback principal = %+v", callbackPrincipal)
	}
	if digest.MemoryCheckpoint == nil || !digest.MemoryCheckpoint.Pending ||
		digest.MemoryCheckpoint.RequestID != "mcrq_1" || digest.MemoryCheckpoint.RequestGeneration != 9 ||
		digest.MemoryCheckpoint.DueAt == nil || !digest.MemoryCheckpoint.DueAt.Equal(dueAt) {
		t.Fatalf("memory checkpoint = %+v", digest.MemoryCheckpoint)
	}
}

func TestSelfDigestFailsOpenWhenMemoryCheckpointIsUnavailable(t *testing.T) {
	auth := func(context.Context, string) (DomainPrincipal, bool, error) {
		return DomainPrincipal{
			Kind: PrincipalKindAgent, ID: "agent_1", AgentName: "scott",
			AccountID: "acc_1", RealmID: "realm_1", RealmName: "default", AccountStatus: "active",
		}, true, nil
	}
	srv := httptest.NewServer(apiMux(Config{
		AuthenticatePrincipal: auth,
		GetSelfMemoryCheckpoint: func(context.Context, DomainPrincipal) (*SelfMemoryCheckpoint, error) {
			return nil, errors.New("checkpoint store unavailable")
		},
	}))
	defer srv.Close()

	resp := selfRequest(t, srv.URL+"/v1/self", "agent-token")
	defer closeBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("self status = %d", resp.StatusCode)
	}
	var digest SelfDigest
	if err := json.NewDecoder(resp.Body).Decode(&digest); err != nil {
		t.Fatal(err)
	}
	if digest.Identity.AgentID != "agent_1" || digest.MemoryCheckpoint == nil ||
		!digest.MemoryCheckpoint.Unavailable || digest.MemoryCheckpoint.Pending {
		t.Fatalf("fail-open digest = %+v", digest)
	}
}

func TestSelfDigestIncludesContentFreeMessageCheckpoint(t *testing.T) {
	auth := func(context.Context, string) (DomainPrincipal, bool, error) {
		return DomainPrincipal{
			Kind: PrincipalKindAgent, ID: "agent_1", AgentName: "scott",
			AccountID: "acc_1", RealmID: "realm_1", RealmName: "default", AccountStatus: "active",
		}, true, nil
	}
	var callbackPrincipal DomainPrincipal
	srv := httptest.NewServer(apiMux(Config{
		AuthenticatePrincipal: auth,
		GetSelfMessageCheckpoint: func(_ context.Context, p DomainPrincipal) (*SelfMessageCheckpoint, error) {
			callbackPrincipal = p
			return &SelfMessageCheckpoint{
				Pending: true, MailboxPending: true, CoordinatorSelectionPending: true,
			}, nil
		},
	}))
	defer srv.Close()

	resp := selfRequest(t, srv.URL+"/v1/self", "agent-token")
	defer closeBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("self status = %d", resp.StatusCode)
	}
	var digest SelfDigest
	if err := json.NewDecoder(resp.Body).Decode(&digest); err != nil {
		t.Fatal(err)
	}
	if callbackPrincipal.ID != "agent_1" || callbackPrincipal.AccountID != "acc_1" || callbackPrincipal.RealmID != "realm_1" {
		t.Fatalf("message checkpoint callback principal = %+v", callbackPrincipal)
	}
	if digest.MessageCheckpoint == nil || !digest.MessageCheckpoint.Pending ||
		!digest.MessageCheckpoint.MailboxPending || !digest.MessageCheckpoint.CoordinatorSelectionPending ||
		digest.MessageCheckpoint.CandidateOfferPending || digest.MessageCheckpoint.CandidateAssignmentPending {
		t.Fatalf("message checkpoint = %+v", digest.MessageCheckpoint)
	}
}

func TestSelfDigestFailsOpenWhenMessageCheckpointIsUnavailable(t *testing.T) {
	auth := func(context.Context, string) (DomainPrincipal, bool, error) {
		return DomainPrincipal{
			Kind: PrincipalKindAgent, ID: "agent_1", AgentName: "scott",
			AccountID: "acc_1", RealmID: "realm_1", RealmName: "default", AccountStatus: "active",
		}, true, nil
	}
	srv := httptest.NewServer(apiMux(Config{
		AuthenticatePrincipal: auth,
		GetSelfMessageCheckpoint: func(context.Context, DomainPrincipal) (*SelfMessageCheckpoint, error) {
			return nil, errors.New("message checkpoint store unavailable")
		},
	}))
	defer srv.Close()

	resp := selfRequest(t, srv.URL+"/v1/self", "agent-token")
	defer closeBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("self status = %d", resp.StatusCode)
	}
	var digest SelfDigest
	if err := json.NewDecoder(resp.Body).Decode(&digest); err != nil {
		t.Fatal(err)
	}
	if digest.Identity.AgentID != "agent_1" || digest.MessageCheckpoint == nil ||
		!digest.MessageCheckpoint.Unavailable || digest.MessageCheckpoint.Pending {
		t.Fatalf("fail-open digest = %+v", digest)
	}
}

func TestSelfDigestAuthorizationAndBounds(t *testing.T) {
	auth := func(_ context.Context, token string) (DomainPrincipal, bool, error) {
		switch token {
		case "agent-token":
			return DomainPrincipal{Kind: PrincipalKindAgent, ID: "agent_1", AccountID: "acc_1", RealmID: "realm_1", AccountStatus: "active"}, true, nil
		case "operator-token":
			return DomainPrincipal{Kind: PrincipalKindOperator, ID: "opr_1", AccountID: "acc_1", AccountStatus: "active"}, true, nil
		case "suspended-token":
			return DomainPrincipal{Kind: PrincipalKindAgent, ID: "agent_1", AccountID: "acc_1", AccountStatus: "suspended"}, true, nil
		default:
			return DomainPrincipal{}, false, nil
		}
	}
	srv := httptest.NewServer(apiMux(Config{AuthenticatePrincipal: auth}))
	defer srv.Close()

	cases := []struct {
		name  string
		path  string
		token string
		want  int
	}{
		{name: "missing token", path: "/v1/self", want: http.StatusUnauthorized},
		{name: "invalid token", path: "/v1/self", token: "invalid", want: http.StatusUnauthorized},
		{name: "operator", path: "/v1/self", token: "operator-token", want: http.StatusForbidden},
		{name: "suspended account", path: "/v1/self", token: "suspended-token", want: http.StatusForbidden},
		{name: "bad boolean", path: "/v1/self?include_facts=perhaps", token: "agent-token", want: http.StatusBadRequest},
		{name: "bad count boolean", path: "/v1/self?include_counts=perhaps", token: "agent-token", want: http.StatusBadRequest},
		{name: "bad checkpoint boolean", path: "/v1/self?include_checkpoint=perhaps", token: "agent-token", want: http.StatusBadRequest},
		{name: "bad message checkpoint boolean", path: "/v1/self?include_message_checkpoint=perhaps", token: "agent-token", want: http.StatusBadRequest},
		{name: "bad sensitive boolean", path: "/v1/self?include_sensitive=perhaps", token: "agent-token", want: http.StatusBadRequest},
		{name: "bad salient limit", path: "/v1/self?salient_limit=101", token: "agent-token", want: http.StatusBadRequest},
		{name: "bad max bytes", path: "/v1/self?max_bytes=100", token: "agent-token", want: http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := selfRequest(t, srv.URL+tc.path, tc.token)
			defer closeBody(t, resp)
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

func TestSelfDigestCanSkipInventoryCountsForCheckpointReads(t *testing.T) {
	auth := func(context.Context, string) (DomainPrincipal, bool, error) {
		return DomainPrincipal{
			Kind: PrincipalKindAgent, ID: "agent_1", AgentName: "scott",
			AccountID: "acc_1", RealmID: "realm_1", RealmName: "default", AccountStatus: "active",
		}, true, nil
	}
	factCountCalls := 0
	memoryCountCalls := 0
	srv := httptest.NewServer(apiMux(Config{
		AuthenticatePrincipal: auth,
		CountSelfFacts: func(context.Context, DomainPrincipal) (int, error) {
			factCountCalls++
			return 99, nil
		},
		CountSelfMemories: func(context.Context, DomainPrincipal) (int, error) {
			memoryCountCalls++
			return 99, nil
		},
		GetSelfMemoryCheckpoint: func(context.Context, DomainPrincipal) (*SelfMemoryCheckpoint, error) {
			return &SelfMemoryCheckpoint{Pending: true, RequestID: "mcrq_1", RequestGeneration: 2}, nil
		},
	}))
	defer srv.Close()

	resp := selfRequest(t, srv.URL+"/v1/self?include_facts=false&include_salient=false&include_counts=false&include_checkpoint=true", "token")
	defer closeBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("self status = %d", resp.StatusCode)
	}
	var digest SelfDigest
	if err := json.NewDecoder(resp.Body).Decode(&digest); err != nil {
		t.Fatal(err)
	}
	if factCountCalls != 0 || memoryCountCalls != 0 {
		t.Fatalf("inventory count calls = facts:%d memories:%d, want zero", factCountCalls, memoryCountCalls)
	}
	if len(digest.Index.Counts) != 0 || digest.MemoryCheckpoint == nil || !digest.MemoryCheckpoint.Pending {
		t.Fatalf("checkpoint-only digest = %+v", digest)
	}
}

func TestSelfDigestCanSkipCheckpointForIdentityReads(t *testing.T) {
	auth := func(context.Context, string) (DomainPrincipal, bool, error) {
		return DomainPrincipal{
			Kind: PrincipalKindAgent, ID: "agent_1", AgentName: "scott",
			AccountID: "acc_1", RealmID: "realm_1", RealmName: "default", AccountStatus: "active",
		}, true, nil
	}
	checkpointCalls := 0
	srv := httptest.NewServer(apiMux(Config{
		AuthenticatePrincipal: auth,
		GetSelfMemoryCheckpoint: func(context.Context, DomainPrincipal) (*SelfMemoryCheckpoint, error) {
			checkpointCalls++
			return &SelfMemoryCheckpoint{Pending: true}, nil
		},
	}))
	defer srv.Close()

	resp := selfRequest(t, srv.URL+"/v1/self?include_facts=false&include_salient=false&include_counts=false&include_checkpoint=false", "token")
	defer closeBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("self status = %d", resp.StatusCode)
	}
	var digest SelfDigest
	if err := json.NewDecoder(resp.Body).Decode(&digest); err != nil {
		t.Fatal(err)
	}
	if checkpointCalls != 0 || digest.MemoryCheckpoint != nil {
		t.Fatalf("identity-only checkpoint calls/digest = %d / %+v", checkpointCalls, digest.MemoryCheckpoint)
	}
}

func TestSelfDigestCanSkipMessageCheckpointForIdentityReads(t *testing.T) {
	auth := func(context.Context, string) (DomainPrincipal, bool, error) {
		return DomainPrincipal{
			Kind: PrincipalKindAgent, ID: "agent_1", AgentName: "scott",
			AccountID: "acc_1", RealmID: "realm_1", RealmName: "default", AccountStatus: "active",
		}, true, nil
	}
	checkpointCalls := 0
	srv := httptest.NewServer(apiMux(Config{
		AuthenticatePrincipal: auth,
		GetSelfMessageCheckpoint: func(context.Context, DomainPrincipal) (*SelfMessageCheckpoint, error) {
			checkpointCalls++
			return &SelfMessageCheckpoint{Pending: true, MailboxPending: true}, nil
		},
	}))
	defer srv.Close()

	resp := selfRequest(t, srv.URL+"/v1/self?include_facts=false&include_salient=false&include_counts=false&include_checkpoint=false&include_message_checkpoint=false", "token")
	defer closeBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("self status = %d", resp.StatusCode)
	}
	var digest SelfDigest
	if err := json.NewDecoder(resp.Body).Decode(&digest); err != nil {
		t.Fatal(err)
	}
	if checkpointCalls != 0 || digest.MessageCheckpoint != nil {
		t.Fatalf("identity-only message checkpoint calls/digest = %d / %+v", checkpointCalls, digest.MessageCheckpoint)
	}
}

func TestSelfDigestHydratesFacts(t *testing.T) {
	auth := func(context.Context, string) (DomainPrincipal, bool, error) {
		return DomainPrincipal{Kind: PrincipalKindAgent, ID: "agent_1", AccountID: "acc_1", RealmID: "realm_1", AccountStatus: "active"}, true, nil
	}
	srv := httptest.NewServer(apiMux(Config{AuthenticatePrincipal: auth, GetSelfFacts: func(_ context.Context, _ DomainPrincipal, limit int, includeCount bool) ([]SelfFact, int, error) {
		if limit != 50 || !includeCount {
			t.Fatalf("limit/includeCount=%d/%t", limit, includeCount)
		}
		return []SelfFact{{ID: "fact_1", Name: "preferences/editor", Value: "vim"}}, 3, nil
	}}))
	defer srv.Close()
	resp := selfRequest(t, srv.URL+"/v1/self?include_facts=true", "token")
	defer closeBody(t, resp)
	var digest SelfDigest
	if err := json.NewDecoder(resp.Body).Decode(&digest); err != nil {
		t.Fatal(err)
	}
	if len(digest.PrimaryFacts) != 1 || !digest.PrimaryFacts[0].Primary || digest.Index.Counts["facts"] != 3 || !digest.Elided {
		t.Fatalf("digest=%+v", digest)
	}
}

func TestSelfDigestEnforcesEncodedByteBudget(t *testing.T) {
	auth := func(context.Context, string) (DomainPrincipal, bool, error) {
		return DomainPrincipal{Kind: PrincipalKindAgent, ID: "agent_1", AccountID: "acc_1", RealmID: "realm_1", AccountStatus: "active"}, true, nil
	}
	facts := make([]SelfFact, 8)
	for i := range facts {
		facts[i] = SelfFact{ID: "fact_" + strings.Repeat("x", 20), Name: "profile/description", Value: strings.Repeat("v", 300)}
	}
	srv := httptest.NewServer(apiMux(Config{
		AuthenticatePrincipal: auth,
		GetSelfFacts: func(context.Context, DomainPrincipal, int, bool) ([]SelfFact, int, error) {
			return facts, len(facts), nil
		},
		GetSelfMemoryCheckpoint: func(context.Context, DomainPrincipal) (*SelfMemoryCheckpoint, error) {
			return &SelfMemoryCheckpoint{Pending: true, RequestID: "mcrq_bounded", RequestGeneration: 2}, nil
		},
		GetSelfMessageCheckpoint: func(context.Context, DomainPrincipal) (*SelfMessageCheckpoint, error) {
			return &SelfMessageCheckpoint{Pending: true, MailboxPending: true}, nil
		},
	}))
	defer srv.Close()

	resp := selfRequest(t, srv.URL+"/v1/self?max_bytes=1024", "token")
	defer closeBody(t, resp)
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) > 1024 {
		t.Fatalf("encoded digest is %d bytes, want <= 1024", len(body))
	}
	var digest SelfDigest
	if err := json.Unmarshal(body, &digest); err != nil {
		t.Fatal(err)
	}
	if !digest.Elided || len(digest.PrimaryFacts) >= len(facts) || digest.Index.Counts["facts"] != len(facts) {
		t.Fatalf("bounded digest = %+v", digest)
	}
	if digest.MemoryCheckpoint == nil || digest.MemoryCheckpoint.RequestID != "mcrq_bounded" {
		t.Fatalf("bounded digest dropped checkpoint = %+v", digest.MemoryCheckpoint)
	}
	if digest.MessageCheckpoint == nil || !digest.MessageCheckpoint.MailboxPending {
		t.Fatalf("bounded digest dropped message checkpoint = %+v", digest.MessageCheckpoint)
	}
}

func TestSelfDigestPassesCountProjectionIntentToIncludedLoaders(t *testing.T) {
	auth := func(context.Context, string) (DomainPrincipal, bool, error) {
		return DomainPrincipal{Kind: PrincipalKindAgent, ID: "agent_1", AccountID: "acc_1", RealmID: "realm_1", AccountStatus: "active"}, true, nil
	}
	factCountIntent := true
	memoryCountIntent := true
	srv := httptest.NewServer(apiMux(Config{
		AuthenticatePrincipal: auth,
		GetSelfFacts: func(_ context.Context, _ DomainPrincipal, _ int, includeCount bool) ([]SelfFact, int, error) {
			factCountIntent = includeCount
			return []SelfFact{{ID: "fact_1", Name: "profile/name", Value: "Scott"}}, 1, nil
		},
		GetSelfMemories: func(_ context.Context, _ DomainPrincipal, _ int, includeCount bool) ([]SelfMemory, int, error) {
			memoryCountIntent = includeCount
			return []SelfMemory{{ID: "mem_1", Snippet: "Use PostgreSQL", Kind: "decision"}}, 1, nil
		},
	}))
	defer srv.Close()

	resp := selfRequest(t, srv.URL+"/v1/self?include_counts=false&include_checkpoint=false", "token")
	defer closeBody(t, resp)
	var digest SelfDigest
	if err := json.NewDecoder(resp.Body).Decode(&digest); err != nil {
		t.Fatal(err)
	}
	if factCountIntent || memoryCountIntent || len(digest.Index.Counts) != 0 {
		t.Fatalf("count projection intent/digest = %t/%t/%+v", factCountIntent, memoryCountIntent, digest.Index)
	}
}

func TestSelfDigestPreservesBackendAndNonPlainRedactionWhenSensitiveIncluded(t *testing.T) {
	auth := func(context.Context, string) (DomainPrincipal, bool, error) {
		return DomainPrincipal{Kind: PrincipalKindAgent, ID: "agent_1", AccountID: "acc_1", RealmID: "realm_1", AccountStatus: "active"}, true, nil
	}
	srv := httptest.NewServer(apiMux(Config{
		AuthenticatePrincipal: auth,
		GetSelfFacts: func(context.Context, DomainPrincipal, int, bool) ([]SelfFact, int, error) {
			return []SelfFact{{ID: "fact_1", Name: "private", Value: "must-not-leak", Sensitive: true, Redacted: true}}, 1, nil
		},
		GetSelfMemories: func(context.Context, DomainPrincipal, int, bool) ([]SelfMemory, int, error) {
			return []SelfMemory{{
				ID: "mem_1", Snippet: "c2VjcmV0", ContentEncoding: "base64", Kind: "profile", Sensitive: true,
			}}, 1, nil
		},
	}))
	defer srv.Close()

	resp := selfRequest(t, srv.URL+"/v1/self?include_sensitive=true", "token")
	defer closeBody(t, resp)
	var digest SelfDigest
	if err := json.NewDecoder(resp.Body).Decode(&digest); err != nil {
		t.Fatal(err)
	}
	if len(digest.PrimaryFacts) != 1 || digest.PrimaryFacts[0].Value != nil || !digest.PrimaryFacts[0].Redacted ||
		len(digest.SalientMemories) != 1 || digest.SalientMemories[0].Snippet != "" || !digest.SalientMemories[0].Redacted {
		t.Fatalf("pre-redacted/non-plain values escaped redaction: %+v", digest)
	}
}

func TestSelfDigestReportsCountWhenFactsExcludedAndRedactsSensitiveValues(t *testing.T) {
	auth := func(context.Context, string) (DomainPrincipal, bool, error) {
		return DomainPrincipal{Kind: PrincipalKindAgent, ID: "agent_1", AccountID: "acc_1", RealmID: "realm_1", AccountStatus: "active"}, true, nil
	}
	getCalls := 0
	getFacts := func(context.Context, DomainPrincipal, int, bool) ([]SelfFact, int, error) {
		getCalls++
		return []SelfFact{{ID: "fact_secret", Name: "identity/private", Value: "must-not-leak", Sensitive: true}}, 7, nil
	}
	srv := httptest.NewServer(apiMux(Config{
		AuthenticatePrincipal: auth,
		GetSelfFacts:          getFacts,
		CountSelfFacts: func(context.Context, DomainPrincipal) (int, error) {
			return 7, nil
		},
	}))
	defer srv.Close()

	resp := selfRequest(t, srv.URL+"/v1/self?include_facts=true", "token")
	defer closeBody(t, resp)
	var digest SelfDigest
	if err := json.NewDecoder(resp.Body).Decode(&digest); err != nil {
		t.Fatal(err)
	}
	if len(digest.PrimaryFacts) != 1 || digest.PrimaryFacts[0].Value != nil || !digest.PrimaryFacts[0].Redacted {
		t.Fatalf("sensitive fact was not redacted: %+v", digest.PrimaryFacts)
	}
	if digest.Index.Counts["facts"] != 7 {
		t.Fatalf("fact count = %d, want 7", digest.Index.Counts["facts"])
	}

	resp = selfRequest(t, srv.URL+"/v1/self?include_facts=false", "token")
	defer closeBody(t, resp)
	digest = SelfDigest{}
	if err := json.NewDecoder(resp.Body).Decode(&digest); err != nil {
		t.Fatal(err)
	}
	if len(digest.PrimaryFacts) != 0 || digest.Index.Counts["facts"] != 7 || digest.Elided {
		t.Fatalf("excluded-facts digest = %+v", digest)
	}
	if getCalls != 1 {
		t.Fatalf("fact values loaded %d times, want only the included request", getCalls)
	}

	resp = selfRequest(t, srv.URL+"/v1/self?include_facts=true&include_sensitive=true", "token")
	defer closeBody(t, resp)
	digest = SelfDigest{}
	if err := json.NewDecoder(resp.Body).Decode(&digest); err != nil {
		t.Fatal(err)
	}
	if len(digest.PrimaryFacts) != 1 || digest.PrimaryFacts[0].Value != "must-not-leak" || digest.PrimaryFacts[0].Redacted {
		t.Fatalf("authorized sensitive fact was not hydrated: %+v", digest.PrimaryFacts)
	}
}

func TestSelfDigestHydratesSalientMemoriesDeterministically(t *testing.T) {
	auth := func(context.Context, string) (DomainPrincipal, bool, error) {
		return DomainPrincipal{
			Kind: PrincipalKindAgent, ID: "agent_1", AccountID: "acc_1",
			RealmID: "realm_1", AccountStatus: "active",
		}, true, nil
	}
	getCalls := 0
	srv := httptest.NewServer(apiMux(Config{
		AuthenticatePrincipal: auth,
		GetSelfMemories: func(_ context.Context, _ DomainPrincipal, limit int, includeCount bool) ([]SelfMemory, int, error) {
			getCalls++
			if limit != 2 || !includeCount {
				t.Fatalf("limit/includeCount=%d/%t, want 2/true", limit, includeCount)
			}
			return []SelfMemory{
				{ID: "mem_1", Snippet: "picked postgres", Kind: "decision", Tags: []string{"storage", "architecture"}, Salience: 0.9},
				{ID: "mem_2", Snippet: "private", Kind: "lesson", Tags: []string{"secret"}, Salience: 0.8, Sensitive: true},
			}, 3, nil
		},
		CountSelfMemories: func(context.Context, DomainPrincipal) (int, error) {
			return 3, nil
		},
	}))
	defer srv.Close()

	resp := selfRequest(t, srv.URL+"/v1/self?include_facts=false&include_salient=true&salient_limit=2", "token")
	defer closeBody(t, resp)
	var digest SelfDigest
	if err := json.NewDecoder(resp.Body).Decode(&digest); err != nil {
		t.Fatal(err)
	}
	if len(digest.SalientMemories) != 2 || digest.Index.Counts["memories"] != 3 || !digest.Elided {
		t.Fatalf("memory digest=%+v", digest)
	}
	if got := digest.Index.Kinds; len(got) != 2 || got[0] != "decision" || got[1] != "lesson" {
		t.Fatalf("kinds=%v", got)
	}
	if got := digest.Index.Tags; len(got) != 2 || got[0] != "architecture" || got[1] != "storage" {
		t.Fatalf("tags=%v", got)
	}
	if sensitive := digest.SalientMemories[1]; sensitive.Snippet != "" || len(sensitive.Tags) != 0 || !sensitive.Redacted {
		t.Fatalf("sensitive memory was not redacted: %+v", sensitive)
	}

	resp = selfRequest(t, srv.URL+"/v1/self?include_facts=false&include_salient=false", "token")
	defer closeBody(t, resp)
	digest = SelfDigest{}
	if err := json.NewDecoder(resp.Body).Decode(&digest); err != nil {
		t.Fatal(err)
	}
	if len(digest.SalientMemories) != 0 || digest.Index.Counts["memories"] != 3 {
		t.Fatalf("excluded memory digest=%+v", digest)
	}
	if getCalls != 1 {
		t.Fatalf("memory values loaded %d times, want one included request", getCalls)
	}

	resp = selfRequest(t, srv.URL+"/v1/self?include_facts=false&include_salient=true&include_sensitive=true&salient_limit=2", "token")
	defer closeBody(t, resp)
	digest = SelfDigest{}
	if err := json.NewDecoder(resp.Body).Decode(&digest); err != nil {
		t.Fatal(err)
	}
	if len(digest.SalientMemories) != 2 || digest.SalientMemories[1].Snippet != "private" || digest.SalientMemories[1].Redacted {
		t.Fatalf("authorized sensitive memory was not hydrated: %+v", digest.SalientMemories)
	}
}

func selfRequest(t *testing.T, url, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}
