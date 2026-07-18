package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/avatar"
)

func TestAvatarMutationReceiptResultLineageJSONContract(t *testing.T) {
	tests := []struct {
		name      string
		operation string
		lineage   int64
		wantJSON  string
		wantField bool
	}{
		{name: "non-reset omits unset lineage", operation: "propose", wantField: false},
		{name: "reset includes positive lineage", operation: "reset", lineage: 3, wantJSON: "3", wantField: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := json.Marshal(AvatarMutationReceipt{
				Operation: tt.operation, ResultLineageGeneration: tt.lineage,
			})
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatal(err)
			}
			got, ok := fields["result_lineage_generation"]
			if ok != tt.wantField || ok && string(got) != tt.wantJSON {
				t.Fatalf("result_lineage_generation = %s, present = %v; JSON=%s", got, ok, raw)
			}
		})
	}
}

func TestSelfAvatarHTTPRoutes(t *testing.T) {
	wantPrincipal := testAvatarAgentPrincipal()
	view := testServerAvatarView("agent_self")
	style := testServerAvatarStyle()
	result := testServerAvatarMutation("propose", view)
	var calls atomic.Int64
	cfg := Config{
		AuthenticatePrincipal: testAvatarPrincipalAuth,
		GetSelfAvatar: func(_ context.Context, p DomainPrincipal) (AvatarView, error) {
			calls.Add(1)
			if !sameAvatarPrincipal(p, wantPrincipal) {
				return AvatarView{}, ErrBadInput
			}
			return view, nil
		},
		GetSelfAvatarHistory: func(_ context.Context, p DomainPrincipal, opts AvatarHistoryOptions) (AvatarHistoryPage, error) {
			calls.Add(1)
			if !sameAvatarPrincipal(p, wantPrincipal) || opts.Limit != defaultAvatarHistoryLimit || opts.BeforeVersion != 0 {
				return AvatarHistoryPage{}, ErrBadInput
			}
			return AvatarHistoryPage{Versions: []AvatarVersionSummary{testServerAvatarSummary(*view.Active)}}, nil
		},
		GetSelfAvatarVersion: func(_ context.Context, p DomainPrincipal, version int64) (AvatarVersion, error) {
			calls.Add(1)
			if !sameAvatarPrincipal(p, wantPrincipal) || version != 1 {
				return AvatarVersion{}, ErrBadInput
			}
			return *view.Active, nil
		},
		GetSelfAvatarStyle: func(_ context.Context, p DomainPrincipal) (AvatarStyleView, error) {
			calls.Add(1)
			if !sameAvatarPrincipal(p, wantPrincipal) {
				return AvatarStyleView{}, ErrBadInput
			}
			return style, nil
		},
		ProposeSelfAvatar: func(_ context.Context, p DomainPrincipal, in ProposeAvatarRequest) (AvatarMutationResult, error) {
			calls.Add(1)
			if !sameAvatarPrincipal(p, wantPrincipal) || in.IdempotencyKey != "proposal-key" ||
				in.ExpectedProfileRevision != 4 || in.SubjectForm != avatar.SubjectHuman ||
				in.Description != "Calm human teammate." || string(in.VisualSpec) != `{"expression":"calm"}` ||
				strings.Contains(in.SVG, "comment") || in.Provenance.Runtime != "codex" ||
				in.Provenance.Model != "GPT-5.6 Sol" {
				return AvatarMutationResult{}, ErrBadInput
			}
			return result, nil
		},
		ActivateSelfAvatar: func(_ context.Context, p DomainPrincipal, in ActivateAvatarRequest) (AvatarMutationResult, error) {
			calls.Add(1)
			if !sameAvatarPrincipal(p, wantPrincipal) || in.Version != 2 || in.ExpectedProfileRevision != 4 || in.IdempotencyKey != "activate-key" {
				return AvatarMutationResult{}, ErrBadInput
			}
			return result, nil
		},
		RollbackSelfAvatar: func(_ context.Context, p DomainPrincipal, in RollbackAvatarRequest) (AvatarMutationResult, error) {
			calls.Add(1)
			if !sameAvatarPrincipal(p, wantPrincipal) || in.Version != 1 || in.ExpectedProfileRevision != 5 || in.IdempotencyKey != "rollback-key" {
				return AvatarMutationResult{}, ErrBadInput
			}
			return result, nil
		},
		ResetSelfAvatar: func(_ context.Context, p DomainPrincipal, in ResetAvatarRequest) (AvatarMutationResult, error) {
			calls.Add(1)
			if !sameAvatarPrincipal(p, wantPrincipal) || in.ExpectedProfileRevision != 7 || in.ReasonCode != "user_requested" || in.IdempotencyKey != "reset-key" {
				return AvatarMutationResult{}, ErrBadInput
			}
			return result, nil
		},
		ReportSelfAvatarGenerationFailure: func(_ context.Context, p DomainPrincipal, in AvatarGenerationFailureRequest) (AvatarMutationResult, error) {
			calls.Add(1)
			if !sameAvatarPrincipal(p, wantPrincipal) || in.ExpectedProfileRevision != 6 || in.ReasonCode != "render_failed" || in.IdempotencyKey != "failure-key" {
				return AvatarMutationResult{}, ErrBadInput
			}
			return result, nil
		},
	}
	srv := httptest.NewServer(apiMux(cfg))
	defer srv.Close()

	body := avatarRequest(t, srv.URL, http.MethodGet, "/v1/self/avatar", "agent-token", "", "", http.StatusOK)
	var avatarEnvelope struct {
		SchemaVersion string     `json:"schema_version"`
		Avatar        AvatarView `json:"avatar"`
	}
	decodeAvatarTestJSON(t, body, &avatarEnvelope)
	if avatarEnvelope.SchemaVersion != "witself.v0" || avatarEnvelope.Avatar.Profile.AgentID != "agent_self" ||
		avatarEnvelope.Avatar.Profile.LineageGeneration != 1 || avatarEnvelope.Avatar.Active == nil || avatarEnvelope.Avatar.Active.LineageGeneration != 1 {
		t.Fatalf("avatar envelope = %+v", avatarEnvelope)
	}

	body = avatarRequest(t, srv.URL, http.MethodGet, "/v1/self/avatar/history", "agent-token", "", "", http.StatusOK)
	var history struct {
		SchemaVersion string                 `json:"schema_version"`
		Versions      []AvatarVersionSummary `json:"versions"`
	}
	decodeAvatarTestJSON(t, body, &history)
	if history.SchemaVersion != "witself.v0" || len(history.Versions) != 1 || history.Versions[0].Version != 1 || history.Versions[0].LineageGeneration != 1 {
		t.Fatalf("history = %+v", history)
	}
	for _, forbidden := range []string{"svg", "visual_spec", "description", "provenance"} {
		if strings.Contains(string(body), `"`+forbidden+`"`) {
			t.Fatalf("history leaked %s: %s", forbidden, body)
		}
	}
	body = avatarRequest(t, srv.URL, http.MethodGet, "/v1/self/avatar/versions/1", "agent-token", "", "", http.StatusOK)
	var versionEnvelope struct {
		Version AvatarVersion `json:"version"`
	}
	decodeAvatarTestJSON(t, body, &versionEnvelope)
	if versionEnvelope.Version.Version != 1 || versionEnvelope.Version.LineageGeneration != 1 || versionEnvelope.Version.SVG == "" || len(versionEnvelope.Version.VisualSpec) == 0 {
		t.Fatalf("exact version = %+v", versionEnvelope.Version)
	}

	body = avatarRequest(t, srv.URL, http.MethodGet, "/v1/self/avatar/style", "agent-token", "", "", http.StatusOK)
	if strings.Contains(string(body), `"rollout"`) {
		t.Fatalf("self style leaked operator rollout progress: %s", body)
	}
	var styleEnvelope struct {
		Style AvatarStyleView `json:"style"`
	}
	decodeAvatarTestJSON(t, body, &styleEnvelope)
	if styleEnvelope.Style.RealmID != "realm_self" || len(styleEnvelope.Style.StylePack.References) != 3 ||
		styleEnvelope.Style.StylePack.References[0].SVG == "" {
		t.Fatalf("style response omitted full references: %+v", styleEnvelope.Style)
	}

	proposal := validServerAvatarProposal()
	proposalRaw, _ := json.Marshal(proposal)
	body = avatarRequest(t, srv.URL, http.MethodPost, "/v1/self/avatar/proposals", "agent-token", "proposal-key", string(proposalRaw), http.StatusCreated)
	assertAvatarMutationEnvelope(t, body)
	body = avatarRequest(t, srv.URL, http.MethodPost, "/v1/self/avatar:activate", "agent-token", "activate-key", `{"version":2,"expected_profile_revision":4}`, http.StatusOK)
	assertAvatarMutationEnvelope(t, body)
	body = avatarRequest(t, srv.URL, http.MethodPost, "/v1/self/avatar:rollback", "agent-token", "rollback-key", `{"version":1,"expected_profile_revision":5}`, http.StatusOK)
	assertAvatarMutationEnvelope(t, body)
	body = avatarRequest(t, srv.URL, http.MethodPost, "/v1/self/avatar:generation-failed", "agent-token", "failure-key", `{"expected_profile_revision":6,"reason_code":"render_failed"}`, http.StatusOK)
	assertAvatarMutationEnvelope(t, body)
	body = avatarRequest(t, srv.URL, http.MethodPost, "/v1/self/avatar:reset", "agent-token", "reset-key", `{"expected_profile_revision":7,"reason_code":" user_requested "}`, http.StatusOK)
	assertAvatarMutationEnvelope(t, body)

	if got := calls.Load(); got != 9 {
		t.Fatalf("callback calls = %d, want 9", got)
	}
}

func TestAvatarHistoryHTTPPreservesLifecycleProjection(t *testing.T) {
	activatedAt := time.Date(2026, 7, 17, 20, 2, 0, 0, time.UTC)
	rejectedAt := activatedAt.Add(time.Minute)
	base := testServerAvatarSummary(*testServerAvatarView("agent_self").Active)
	active := base
	active.Version = 4
	active.IsActive = true
	active.WasActivated = true
	active.LastActivatedAt = &activatedAt
	proposed := base
	proposed.Version = 3
	proposed.IsProposed = true
	rollback := base
	rollback.Version = 2
	rollback.WasActivated = true
	rollback.RollbackEligible = true
	rollback.LastActivatedAt = &activatedAt
	rejected := base
	rejected.Version = 1
	rejected.Rejected = true
	rejected.RejectedAt = &rejectedAt

	srv := httptest.NewServer(apiMux(Config{
		AuthenticatePrincipal: testAvatarPrincipalAuth,
		GetSelfAvatarHistory: func(context.Context, DomainPrincipal, AvatarHistoryOptions) (AvatarHistoryPage, error) {
			return AvatarHistoryPage{Versions: []AvatarVersionSummary{active, proposed, rollback, rejected}}, nil
		},
	}))
	defer srv.Close()
	body := avatarRequest(t, srv.URL, http.MethodGet, "/v1/self/avatar/history", "agent-token", "", "", http.StatusOK)
	var page AvatarHistoryPage
	decodeAvatarTestJSON(t, body, &page)
	if len(page.Versions) != 4 || !page.Versions[0].IsActive || !page.Versions[0].WasActivated ||
		page.Versions[0].LastActivatedAt == nil || !page.Versions[1].IsProposed ||
		!page.Versions[2].RollbackEligible || !page.Versions[3].Rejected || page.Versions[3].RejectedAt == nil {
		t.Fatalf("history lifecycle projection = %+v", page.Versions)
	}
	for _, field := range []string{`"is_active"`, `"is_proposed"`, `"was_activated"`, `"rollback_eligible"`, `"rejected"`, `"payload_state"`, `"payload_bytes"`, `"locked_layers_sha256"`} {
		if !bytes.Contains(body, []byte(field)) {
			t.Errorf("history JSON omitted %s: %s", field, body)
		}
	}
}

func TestAvatarCompactedVersionHTTPRepresentation(t *testing.T) {
	compactedAt := time.Date(2026, 7, 18, 9, 30, 0, 0, time.UTC)
	version := *testServerAvatarView("agent_self").Active
	version.Version = 2
	version.SVG = ""
	version.Description = ""
	version.VisualSpec = nil
	version.PayloadState = avatar.PayloadCompacted
	version.PayloadCompactedAt = &compactedAt
	version.PayloadCompactionReason = "quota"
	version.IsActive = false
	version.RollbackEligible = false

	srv := httptest.NewServer(apiMux(Config{
		AuthenticatePrincipal: testAvatarPrincipalAuth,
		GetSelfAvatarVersion: func(context.Context, DomainPrincipal, int64) (AvatarVersion, error) {
			return version, nil
		},
	}))
	defer srv.Close()
	body := avatarRequest(t, srv.URL, http.MethodGet,
		"/v1/self/avatar/versions/2", "agent-token", "", "", http.StatusOK)
	var envelope struct {
		Version map[string]json.RawMessage `json:"version"`
	}
	decodeAvatarTestJSON(t, body, &envelope)
	for _, absent := range []string{"svg", "description", "visual_spec"} {
		if _, exists := envelope.Version[absent]; exists {
			t.Fatalf("compacted exact version retained %s: %s", absent, body)
		}
	}
	for _, present := range []string{
		"id", "svg_sha256", "locked_layers_sha256", "provenance",
		"payload_state", "payload_bytes", "payload_compacted_at",
		"payload_compaction_reason",
	} {
		if _, exists := envelope.Version[present]; !exists {
			t.Fatalf("compacted exact version omitted %s: %s", present, body)
		}
	}
}

func TestAvatarHistoryHTTPPaginationForwardsExclusiveCursor(t *testing.T) {
	base := testServerAvatarSummary(*testServerAvatarView("agent_self").Active)
	callbackCalls := 0
	srv := httptest.NewServer(apiMux(Config{
		AuthenticatePrincipal: testAvatarPrincipalAuth,
		GetSelfAvatarHistory: func(_ context.Context, _ DomainPrincipal, opts AvatarHistoryOptions) (AvatarHistoryPage, error) {
			callbackCalls++
			page := AvatarHistoryPage{}
			switch {
			case opts.Limit == 2 && opts.BeforeVersion == 0:
				first, second := base, base
				first.Version, second.Version = 5, 4
				page.Versions = []AvatarVersionSummary{first, second}
				page.NextBeforeVersion = 4
			case opts.Limit == 2 && opts.BeforeVersion == 4:
				first, second := base, base
				first.Version, second.Version = 3, 2
				page.Versions = []AvatarVersionSummary{first, second}
				page.NextBeforeVersion = 2
			default:
				return AvatarHistoryPage{}, ErrBadInput
			}
			return page, nil
		},
	}))
	defer srv.Close()
	read := func(path string) AvatarHistoryPage {
		t.Helper()
		body := avatarRequest(t, srv.URL, http.MethodGet, path, "agent-token", "", "", http.StatusOK)
		var page AvatarHistoryPage
		decodeAvatarTestJSON(t, body, &page)
		return page
	}
	first := read("/v1/self/avatar/history?limit=2")
	second := read("/v1/self/avatar/history?limit=2&before_version=4")
	if first.NextBeforeVersion != 4 || second.NextBeforeVersion != 2 ||
		len(first.Versions) != 2 || len(second.Versions) != 2 ||
		first.Versions[1].Version == second.Versions[0].Version || second.Versions[0].Version != 3 {
		t.Fatalf("pagination continuity = first:%+v second:%+v", first, second)
	}
	for _, path := range []string{
		"/v1/self/avatar/history?limit=0",
		"/v1/self/avatar/history?limit=101",
		"/v1/self/avatar/history?before_version=-1",
		"/v1/self/avatar/history?limit=2&limit=3",
		"/v1/self/avatar/history?unknown=1",
	} {
		avatarRequest(t, srv.URL, http.MethodGet, path, "agent-token", "", "", http.StatusBadRequest)
	}
	if callbackCalls != 2 {
		t.Fatalf("history callback calls = %d, want 2", callbackCalls)
	}
}

func TestOperatorAvatarHTTPRoutesAreAccountScoped(t *testing.T) {
	view := testServerAvatarView("agent_target")
	style := testServerAvatarStyle()
	result := testServerAvatarMutation("operator", view)
	var calls atomic.Int64
	check := func(accountID, operatorID, target string) error {
		if accountID != "acc_self" || operatorID != "opr_self" || target == "" {
			return ErrBadInput
		}
		calls.Add(1)
		return nil
	}
	cfg := Config{
		Authenticate: testAvatarOperatorAuth,
		GetAgentAvatar: func(_ context.Context, accountID, operatorID, agentID string) (AvatarView, error) {
			if err := check(accountID, operatorID, agentID); err != nil || agentID != "agent_target" {
				return AvatarView{}, ErrBadInput
			}
			return view, nil
		},
		GetAgentAvatarHistory: func(_ context.Context, accountID, operatorID, agentID string, opts AvatarHistoryOptions) (AvatarHistoryPage, error) {
			if err := check(accountID, operatorID, agentID); err != nil || agentID != "agent_target" || opts.Limit != defaultAvatarHistoryLimit {
				return AvatarHistoryPage{}, ErrBadInput
			}
			return AvatarHistoryPage{Versions: []AvatarVersionSummary{testServerAvatarSummary(*view.Active)}}, nil
		},
		GetAgentAvatarVersion: func(_ context.Context, accountID, operatorID, agentID string, version int64) (AvatarVersion, error) {
			if err := check(accountID, operatorID, agentID); err != nil || agentID != "agent_target" || version != 1 {
				return AvatarVersion{}, ErrBadInput
			}
			return *view.Active, nil
		},
		ProposeAgentAvatar: func(_ context.Context, accountID, operatorID, agentID string, in ProposeAvatarRequest) (AvatarMutationResult, error) {
			if err := check(accountID, operatorID, agentID); err != nil || in.IdempotencyKey != "proposal-key" || in.ExpectedProfileRevision != 4 {
				return AvatarMutationResult{}, ErrBadInput
			}
			return result, nil
		},
		ActivateAgentAvatar: func(_ context.Context, accountID, operatorID, agentID string, in ActivateAvatarRequest) (AvatarMutationResult, error) {
			if err := check(accountID, operatorID, agentID); err != nil || in.Version != 2 || in.IdempotencyKey != "activate-key" {
				return AvatarMutationResult{}, ErrBadInput
			}
			return result, nil
		},
		RejectAgentAvatar: func(_ context.Context, accountID, operatorID, agentID string, in RejectAvatarRequest) (AvatarMutationResult, error) {
			if err := check(accountID, operatorID, agentID); err != nil || in.ReasonCode != "off_style" || in.IdempotencyKey != "reject-key" {
				return AvatarMutationResult{}, ErrBadInput
			}
			return result, nil
		},
		RollbackAgentAvatar: func(_ context.Context, accountID, operatorID, agentID string, in RollbackAvatarRequest) (AvatarMutationResult, error) {
			if err := check(accountID, operatorID, agentID); err != nil || in.Version != 1 || in.IdempotencyKey != "rollback-key" {
				return AvatarMutationResult{}, ErrBadInput
			}
			return result, nil
		},
		ResetAgentAvatar: func(_ context.Context, accountID, operatorID, agentID string, in ResetAvatarRequest) (AvatarMutationResult, error) {
			if err := check(accountID, operatorID, agentID); err != nil || in.ExpectedProfileRevision != 6 || in.ReasonCode != "" || in.IdempotencyKey != "reset-key" {
				return AvatarMutationResult{}, ErrBadInput
			}
			return result, nil
		},
		UpdateAgentAvatarPolicy: func(_ context.Context, accountID, operatorID, agentID string, in UpdateAvatarPolicyRequest) (AvatarMutationResult, error) {
			if err := check(accountID, operatorID, agentID); err != nil || in.Policy != avatar.AutonomyOperatorOnly || in.IdempotencyKey != "policy-key" {
				return AvatarMutationResult{}, ErrBadInput
			}
			return result, nil
		},
		UpdateAgentAvatarQuota: func(_ context.Context, accountID, operatorID, agentID string, in UpdateAvatarQuotaRequest) (AvatarMutationResult, error) {
			if err := check(accountID, operatorID, agentID); err != nil ||
				in.RetainedPayloadCountLimit != 8 || in.RetainedPayloadByteLimit != 1_048_576 ||
				in.ExpectedProfileRevision != 6 || in.IdempotencyKey != "quota-key" {
				return AvatarMutationResult{}, ErrBadInput
			}
			return result, nil
		},
		GetRealmAvatarStyle: func(_ context.Context, accountID, operatorID, realmID string) (AvatarStyleView, error) {
			if err := check(accountID, operatorID, realmID); err != nil || realmID != "realm_self" {
				return AvatarStyleView{}, ErrBadInput
			}
			return style, nil
		},
		CreateRealmAvatarStyleVersion: func(_ context.Context, accountID, operatorID, realmID string, in CreateAvatarStyleVersionRequest) (AvatarStyleMutationResult, error) {
			if err := check(accountID, operatorID, realmID); err != nil || in.ExpectedStyleRevision != 2 || in.IdempotencyKey != "style-key" || in.StylePack.Validate() != nil {
				return AvatarStyleMutationResult{}, ErrBadInput
			}
			return AvatarStyleMutationResult{Style: style, Receipt: result.Receipt}, nil
		},
	}
	srv := httptest.NewServer(apiMux(cfg))
	defer srv.Close()

	avatarRequest(t, srv.URL, http.MethodGet, "/v1/agents/agent_target/avatar", "operator-token", "", "", http.StatusOK)
	historyBody := avatarRequest(t, srv.URL, http.MethodGet, "/v1/agents/agent_target/avatar/history", "operator-token", "", "", http.StatusOK)
	var history AvatarHistoryPage
	decodeAvatarTestJSON(t, historyBody, &history)
	if history.SchemaVersion != "witself.v0" || len(history.Versions) != 1 || history.Versions[0].AgentID != "agent_target" {
		t.Fatalf("operator history = %+v", history)
	}
	avatarRequest(t, srv.URL, http.MethodGet, "/v1/agents/agent_target/avatar/versions/1", "operator-token", "", "", http.StatusOK)
	proposalRaw, _ := json.Marshal(validServerAvatarProposal())
	avatarRequest(t, srv.URL, http.MethodPost, "/v1/agents/agent_target/avatar/proposals", "operator-token", "proposal-key", string(proposalRaw), http.StatusCreated)
	avatarRequest(t, srv.URL, http.MethodPost, "/v1/agents/agent_target/avatar:activate", "operator-token", "activate-key", `{"version":2,"expected_profile_revision":4}`, http.StatusOK)
	avatarRequest(t, srv.URL, http.MethodPost, "/v1/agents/agent_target/avatar:reject", "operator-token", "reject-key", `{"version":2,"expected_profile_revision":4,"reason_code":"off_style"}`, http.StatusOK)
	avatarRequest(t, srv.URL, http.MethodPost, "/v1/agents/agent_target/avatar:rollback", "operator-token", "rollback-key", `{"version":1,"expected_profile_revision":5}`, http.StatusOK)
	avatarRequest(t, srv.URL, http.MethodPost, "/v1/agents/agent_target/avatar:reset", "operator-token", "reset-key", `{"expected_profile_revision":6}`, http.StatusOK)
	avatarRequest(t, srv.URL, http.MethodPatch, "/v1/agents/agent_target/avatar-policy", "operator-token", "policy-key", `{"policy":"operator_only","expected_profile_revision":6}`, http.StatusOK)
	avatarRequest(t, srv.URL, http.MethodPatch, "/v1/agents/agent_target/avatar-quota", "operator-token", "quota-key", `{"retained_payload_count_limit":8,"retained_payload_byte_limit":1048576,"expected_profile_revision":6}`, http.StatusOK)
	operatorStyleBody := avatarRequest(t, srv.URL, http.MethodGet, "/v1/realms/realm_self/avatar-style", "operator-token", "", "", http.StatusOK)
	var operatorStyleEnvelope struct {
		Style AvatarStyleView `json:"style"`
	}
	decodeAvatarTestJSON(t, operatorStyleBody, &operatorStyleEnvelope)
	if operatorStyleEnvelope.Style.Rollout == nil || operatorStyleEnvelope.Style.Rollout.Status != "running" {
		t.Fatalf("operator style omitted rollout progress: %+v", operatorStyleEnvelope.Style)
	}
	styleRequest, _ := json.Marshal(CreateAvatarStyleVersionRequest{ExpectedStyleRevision: 2, StylePack: avatar.BuiltInFlatVectorStylePack()})
	body := avatarRequest(t, srv.URL, http.MethodPost, "/v1/realms/realm_self/avatar-style/versions", "operator-token", "style-key", string(styleRequest), http.StatusCreated)
	var styleMutation struct {
		Style   AvatarStyleView       `json:"style"`
		Receipt AvatarMutationReceipt `json:"receipt"`
	}
	decodeAvatarTestJSON(t, body, &styleMutation)
	if styleMutation.Style.RealmID != "realm_self" || styleMutation.Receipt.RequestHash == "" {
		t.Fatalf("style mutation = %+v", styleMutation)
	}
	if got := calls.Load(); got != 12 {
		t.Fatalf("callback calls = %d, want 12", got)
	}
}

func TestAvatarAuthProfilesAndCacheControl(t *testing.T) {
	cfg := Config{
		AuthenticatePrincipal: testAvatarPrincipalAuth,
		Authenticate:          testAvatarOperatorAuth,
		GetSelfAvatar: func(context.Context, DomainPrincipal) (AvatarView, error) {
			return testServerAvatarView("agent_self"), nil
		},
		GetAgentAvatar: func(context.Context, string, string, string) (AvatarView, error) {
			return testServerAvatarView("agent_target"), nil
		},
		GetAgentAvatarHistory: func(context.Context, string, string, string, AvatarHistoryOptions) (AvatarHistoryPage, error) {
			return AvatarHistoryPage{}, nil
		},
		GetAgentAvatarVersion: func(context.Context, string, string, string, int64) (AvatarVersion, error) {
			view := testServerAvatarView("agent_target")
			return *view.Active, nil
		},
		ResetSelfAvatar: func(context.Context, DomainPrincipal, ResetAvatarRequest) (AvatarMutationResult, error) {
			return testServerAvatarMutation("reset", testServerAvatarView("agent_self")), nil
		},
		ResetAgentAvatar: func(context.Context, string, string, string, ResetAvatarRequest) (AvatarMutationResult, error) {
			return testServerAvatarMutation("reset", testServerAvatarView("agent_target")), nil
		},
	}
	srv := httptest.NewServer(apiMux(cfg))
	defer srv.Close()
	tests := []struct {
		name, path, token string
		status            int
	}{
		{"missing token", "/v1/self/avatar", "", http.StatusUnauthorized},
		{"bad token", "/v1/self/avatar", "bad-token", http.StatusUnauthorized},
		{"operator on self", "/v1/self/avatar", "domain-operator-token", http.StatusForbidden},
		{"curator on self", "/v1/self/avatar", "curator-token", http.StatusForbidden},
		{"suspended agent", "/v1/self/avatar", "suspended-token", http.StatusForbidden},
		{"agent token on operator route", "/v1/agents/agent_target/avatar", "agent-token", http.StatusUnauthorized},
		{"agent token on operator history", "/v1/agents/agent_target/avatar/history", "agent-token", http.StatusUnauthorized},
		{"agent token on operator version", "/v1/agents/agent_target/avatar/versions/1", "agent-token", http.StatusUnauthorized},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			avatarRequest(t, srv.URL, http.MethodGet, test.path, test.token, "", "", test.status)
		})
	}
	avatarRequest(t, srv.URL, http.MethodPost, "/v1/self/avatar:reset", "domain-operator-token", "reset-key", `{"expected_profile_revision":4}`, http.StatusForbidden)
	avatarRequest(t, srv.URL, http.MethodPost, "/v1/agents/agent_target/avatar:reset", "agent-token", "reset-key", `{"expected_profile_revision":4}`, http.StatusUnauthorized)
}

func TestAvatarVersionRoutesRejectNonPositiveOrNonCanonicalPaths(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(apiMux(Config{
		AuthenticatePrincipal: testAvatarPrincipalAuth,
		GetSelfAvatarVersion: func(context.Context, DomainPrincipal, int64) (AvatarVersion, error) {
			calls.Add(1)
			return AvatarVersion{}, nil
		},
	}))
	defer srv.Close()
	for _, version := range []string{"0", "-1", "01", "x", "99999999999999999999"} {
		avatarRequest(t, srv.URL, http.MethodGet, "/v1/self/avatar/versions/"+version, "agent-token", "", "", http.StatusNotFound)
	}
	avatarRequest(t, srv.URL, http.MethodGet, "/v1/self/avatar/versions/1?extra=true", "agent-token", "", "", http.StatusBadRequest)
	if calls.Load() != 0 {
		t.Fatalf("invalid version paths reached callback %d times", calls.Load())
	}
}

func TestAvatarMutationStrictJSONBoundsAndIdentity(t *testing.T) {
	var calls atomic.Int64
	cfg := Config{
		AuthenticatePrincipal: testAvatarPrincipalAuth,
		ProposeSelfAvatar: func(_ context.Context, p DomainPrincipal, _ ProposeAvatarRequest) (AvatarMutationResult, error) {
			calls.Add(1)
			if p.ID != "agent_self" || p.AccountID != "acc_self" || p.RealmID != "realm_self" {
				return AvatarMutationResult{}, ErrBadInput
			}
			return testServerAvatarMutation("propose", testServerAvatarView("agent_self")), nil
		},
		ActivateSelfAvatar: func(context.Context, DomainPrincipal, ActivateAvatarRequest) (AvatarMutationResult, error) {
			calls.Add(1)
			return testServerAvatarMutation("activate", testServerAvatarView("agent_self")), nil
		},
		ReportSelfAvatarGenerationFailure: func(context.Context, DomainPrincipal, AvatarGenerationFailureRequest) (AvatarMutationResult, error) {
			calls.Add(1)
			return testServerAvatarMutation("failure", testServerAvatarView("agent_self")), nil
		},
		ResetSelfAvatar: func(context.Context, DomainPrincipal, ResetAvatarRequest) (AvatarMutationResult, error) {
			calls.Add(1)
			return testServerAvatarMutation("reset", testServerAvatarView("agent_self")), nil
		},
	}
	srv := httptest.NewServer(apiMux(cfg))
	defer srv.Close()
	valid, _ := json.Marshal(validServerAvatarProposal())
	unknownIdentity := strings.TrimSuffix(string(valid), "}") + `,"agent_id":"agent_other"}`
	invalidSVG := validServerAvatarProposal()
	invalidSVG.SVG = `<svg><script></script></svg>`
	invalidSVGRaw, _ := json.Marshal(invalidSVG)
	invalidSpec := validServerAvatarProposal()
	invalidSpec.VisualSpec = json.RawMessage(`[]`)
	invalidSpecRaw, _ := json.Marshal(invalidSpec)
	overLimit := `{"expected_profile_revision":4,"svg":"` + strings.Repeat("x", int(maxAvatarProposalRequestBytes)) + `"}`
	tests := []struct {
		name, path, key, body string
	}{
		{"unknown identity field", "/v1/self/avatar/proposals", "key", unknownIdentity},
		{"query identity", "/v1/self/avatar/proposals?account=other", "key", string(valid)},
		{"multiple values", "/v1/self/avatar/proposals", "key", string(valid) + `{}`},
		{"missing idempotency", "/v1/self/avatar/proposals", "", string(valid)},
		{"long idempotency", "/v1/self/avatar/proposals", strings.Repeat("k", maxAvatarIdempotencyKeyBytes+1), string(valid)},
		{"unsafe svg", "/v1/self/avatar/proposals", "key", string(invalidSVGRaw)},
		{"invalid spec", "/v1/self/avatar/proposals", "key", string(invalidSpecRaw)},
		{"over body limit", "/v1/self/avatar/proposals", "key", overLimit},
		{"unknown action field", "/v1/self/avatar:activate", "key", `{"version":2,"expected_profile_revision":4,"agent_id":"other"}`},
		{"missing revision", "/v1/self/avatar:activate", "key", `{"version":2}`},
		{"body idempotency", "/v1/self/avatar:activate", "key", `{"version":2,"expected_profile_revision":4,"idempotency_key":"body"}`},
		{"invalid failure reason", "/v1/self/avatar:generation-failed", "key", `{"expected_profile_revision":4,"reason_code":"free form reason"}`},
		{"missing reset revision", "/v1/self/avatar:reset", "key", `{"reason_code":"user_requested"}`},
		{"invalid reset reason", "/v1/self/avatar:reset", "key", `{"expected_profile_revision":4,"reason_code":"free form reason"}`},
		{"reset body idempotency", "/v1/self/avatar:reset", "key", `{"expected_profile_revision":4,"idempotency_key":"body"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			avatarRequest(t, srv.URL, http.MethodPost, test.path, "agent-token", test.key, test.body, http.StatusBadRequest)
		})
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("invalid requests reached callbacks %d times", got)
	}
}

func TestAvatarOperatorPolicyAndStyleValidation(t *testing.T) {
	var calls atomic.Int64
	cfg := Config{
		Authenticate: testAvatarOperatorAuth,
		UpdateAgentAvatarPolicy: func(context.Context, string, string, string, UpdateAvatarPolicyRequest) (AvatarMutationResult, error) {
			calls.Add(1)
			return testServerAvatarMutation("policy", testServerAvatarView("agent_target")), nil
		},
		UpdateAgentAvatarQuota: func(context.Context, string, string, string, UpdateAvatarQuotaRequest) (AvatarMutationResult, error) {
			calls.Add(1)
			return testServerAvatarMutation("quota", testServerAvatarView("agent_target")), nil
		},
		CreateRealmAvatarStyleVersion: func(context.Context, string, string, string, CreateAvatarStyleVersionRequest) (AvatarStyleMutationResult, error) {
			calls.Add(1)
			return AvatarStyleMutationResult{Style: testServerAvatarStyle()}, nil
		},
	}
	srv := httptest.NewServer(apiMux(cfg))
	defer srv.Close()
	tests := []struct {
		name, method, path, key, body string
	}{
		{"invalid policy", http.MethodPatch, "/v1/agents/agent_target/avatar-policy", "key", `{"policy":"anything_goes","expected_profile_revision":4}`},
		{"missing policy revision", http.MethodPatch, "/v1/agents/agent_target/avatar-policy", "key", `{"policy":"operator_only"}`},
		{"unknown policy field", http.MethodPatch, "/v1/agents/agent_target/avatar-policy", "key", `{"policy":"operator_only","expected_profile_revision":4,"agent_id":"other"}`},
		{"missing policy key", http.MethodPatch, "/v1/agents/agent_target/avatar-policy", "", `{"policy":"operator_only","expected_profile_revision":4}`},
		{"quota below count floor", http.MethodPatch, "/v1/agents/agent_target/avatar-quota", "key", `{"retained_payload_count_limit":3,"retained_payload_byte_limit":1048576,"expected_profile_revision":4}`},
		{"quota below byte floor", http.MethodPatch, "/v1/agents/agent_target/avatar-quota", "key", `{"retained_payload_count_limit":4,"retained_payload_byte_limit":524287,"expected_profile_revision":4}`},
		{"quota body idempotency", http.MethodPatch, "/v1/agents/agent_target/avatar-quota", "key", `{"retained_payload_count_limit":4,"retained_payload_byte_limit":524288,"expected_profile_revision":4,"idempotency_key":"body"}`},
		{"invalid style", http.MethodPost, "/v1/realms/realm_self/avatar-style/versions", "key", `{"expected_style_revision":2,"style_pack":{}}`},
		{"missing style revision", http.MethodPost, "/v1/realms/realm_self/avatar-style/versions", "key", `{"style_pack":{}}`},
		{"unknown nested style field", http.MethodPost, "/v1/realms/realm_self/avatar-style/versions", "key", `{"expected_style_revision":2,"style_pack":{"id":"flat","version":1,"unknown":true}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			avatarRequest(t, srv.URL, test.method, test.path, "operator-token", test.key, test.body, http.StatusBadRequest)
		})
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("invalid operator requests reached callbacks %d times", got)
	}
}

func TestAvatarErrorMapping(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		status    int
		wantError string
	}{
		{name: "bad input", err: ErrBadInput, status: http.StatusBadRequest},
		{name: "domain invalid", err: avatar.ErrInvalidSVG, status: http.StatusBadRequest},
		{name: "forbidden", err: ErrForbidden, status: http.StatusForbidden},
		{name: "not found", err: ErrNotFound, status: http.StatusNotFound},
		{name: "revision conflict", err: ErrConflict, status: http.StatusConflict},
		{name: "idempotency conflict", err: ErrIdempotencyConflict, status: http.StatusConflict},
		{name: "payload quota", err: ErrAvatarPayloadQuotaExceeded, status: http.StatusConflict,
			wantError: "avatar_payload_quota_exceeded"},
		{name: "payload compaction activation", err: ErrAvatarPayloadCompactionDisabled,
			status: http.StatusConflict, wantError: "avatar_payload_compaction_not_active"},
		{name: "internal", err: errors.New("database unavailable"), status: http.StatusInternalServerError},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			srv := httptest.NewServer(apiMux(Config{
				AuthenticatePrincipal: testAvatarPrincipalAuth,
				GetSelfAvatar: func(context.Context, DomainPrincipal) (AvatarView, error) {
					return AvatarView{}, test.err
				},
			}))
			defer srv.Close()
			body := avatarRequest(t, srv.URL, http.MethodGet, "/v1/self/avatar", "agent-token", "", "", test.status)
			if test.wantError != "" {
				var response map[string]string
				decodeAvatarTestJSON(t, body, &response)
				if response["error"] != test.wantError {
					t.Fatalf("error = %q, want %q", response["error"], test.wantError)
				}
			}
		})
	}
}

func TestAvatarRoutesAreCallbackGatedAndAlwaysPrivate(t *testing.T) {
	srv := httptest.NewServer(apiMux(Config{AuthenticatePrincipal: testAvatarPrincipalAuth, Authenticate: testAvatarOperatorAuth}))
	defer srv.Close()
	for _, path := range []string{
		"/v1/self/avatar",
		"/v1/self/avatar/versions/1",
		"/v1/agents/agent_target/avatar",
		"/v1/agents/agent_target/avatar/history",
		"/v1/agents/agent_target/avatar/versions/1",
		"/v1/realms/realm_self/avatar-style",
	} {
		avatarRequest(t, srv.URL, http.MethodGet, path, "agent-token", "", "", http.StatusNotFound)
	}

	srvWithRead := httptest.NewServer(apiMux(Config{
		AuthenticatePrincipal: testAvatarPrincipalAuth,
		GetSelfAvatar: func(context.Context, DomainPrincipal) (AvatarView, error) {
			return testServerAvatarView("agent_self"), nil
		},
	}))
	defer srvWithRead.Close()
	avatarRequest(t, srvWithRead.URL, http.MethodPost, "/v1/self/avatar", "agent-token", "", `{}`, http.StatusMethodNotAllowed)
}

func TestCapabilitiesReportsAvatarSupport(t *testing.T) {
	base := Config{AuthenticatePrincipal: testAvatarPrincipalAuth}
	check := func(t *testing.T, cfg Config, want bool) {
		t.Helper()
		srv := httptest.NewServer(apiMux(cfg))
		defer srv.Close()
		response, err := http.Get(srv.URL + "/v1/capabilities")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = response.Body.Close() }()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusOK {
			t.Fatalf("capabilities status = %d; body=%s", response.StatusCode, body)
		}
		var out capabilities
		decodeAvatarTestJSON(t, body, &out)
		if got := out.Features["avatars"].Supported; got != want {
			t.Fatalf("avatars supported = %t, want %t", got, want)
		}
	}
	check(t, base, false)
	base.GetSelfAvatar = func(context.Context, DomainPrincipal) (AvatarView, error) { return AvatarView{}, nil }
	base.GetSelfAvatarHistory = func(context.Context, DomainPrincipal, AvatarHistoryOptions) (AvatarHistoryPage, error) {
		return AvatarHistoryPage{}, nil
	}
	base.GetSelfAvatarVersion = func(context.Context, DomainPrincipal, int64) (AvatarVersion, error) {
		return AvatarVersion{}, nil
	}
	base.GetSelfAvatarStyle = func(context.Context, DomainPrincipal) (AvatarStyleView, error) { return AvatarStyleView{}, nil }
	base.ProposeSelfAvatar = func(context.Context, DomainPrincipal, ProposeAvatarRequest) (AvatarMutationResult, error) {
		return AvatarMutationResult{}, nil
	}
	base.ActivateSelfAvatar = func(context.Context, DomainPrincipal, ActivateAvatarRequest) (AvatarMutationResult, error) {
		return AvatarMutationResult{}, nil
	}
	base.RollbackSelfAvatar = func(context.Context, DomainPrincipal, RollbackAvatarRequest) (AvatarMutationResult, error) {
		return AvatarMutationResult{}, nil
	}
	base.ResetSelfAvatar = func(context.Context, DomainPrincipal, ResetAvatarRequest) (AvatarMutationResult, error) {
		return AvatarMutationResult{}, nil
	}
	base.ReportSelfAvatarGenerationFailure = func(context.Context, DomainPrincipal, AvatarGenerationFailureRequest) (AvatarMutationResult, error) {
		return AvatarMutationResult{}, nil
	}
	check(t, base, true)
}

func validServerAvatarProposal() ProposeAvatarRequest {
	return ProposeAvatarRequest{
		ExpectedProfileRevision: 4,
		ParentVersion:           1,
		StylePackID:             avatar.DefaultStylePackID,
		StylePackVersion:        avatar.BuiltInStylePackVersion,
		SubjectForm:             avatar.SubjectHuman,
		Description:             "  Calm human teammate.  ",
		VisualSpec:              json.RawMessage(`{"expression":"calm"}`),
		SVG:                     `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 512 512" width="512" height="512"><!--comment--><circle cx="256" cy="256" r="100" fill="#DCEAF5"></circle></svg>`,
		Provenance:              AvatarClientProvenance{Runtime: " codex ", Model: " GPT-5.6 Sol ", Recipe: "initial", RecipeVersion: "1"},
	}
}

func testServerAvatarView(agentID string) AvatarView {
	now := time.Date(2026, 7, 17, 20, 0, 0, 0, time.UTC)
	version := AvatarVersion{
		ID: "avv_1", AccountID: "acc_self", RealmID: "realm_self", AgentID: agentID,
		Version: 1, LineageGeneration: 1, SubjectForm: avatar.SubjectHuman,
		Description: "Calm human teammate.", VisualSpec: json.RawMessage(`{"expression":"calm"}`),
		SVG: `<svg xmlns="http://www.w3.org/2000/svg"></svg>`, SVGSHA256: strings.Repeat("a", 64),
		LockedLayersSHA256: strings.Repeat("b", 64), PayloadState: avatar.PayloadFull,
		PayloadBytes: 128,
		Style:        avatar.StylePackRef{RealmID: "realm_self", StylePackID: avatar.DefaultStylePackID, Version: 1},
		Provenance:   AvatarClientProvenance{Runtime: "codex"},
		ProposedBy:   AvatarActor{Kind: PrincipalKindAgent, ID: agentID, Name: "Juniper"}, ProposedAt: now,
	}
	return AvatarView{
		Profile: AvatarProfile{
			AccountID: "acc_self", RealmID: "realm_self", AgentID: agentID,
			SubjectForm: avatar.SubjectHuman, AutonomyPolicy: avatar.AutonomyAgentSelfManaged,
			Status: avatar.StatusActive, LineageGeneration: 1,
			Style:           avatar.StylePackRef{RealmID: "realm_self", StylePackID: avatar.DefaultStylePackID, Version: 1},
			ProfileRevision: 4, LatestVersion: 1, ActiveVersion: 1, AttemptCount: 0,
			FallbackSeed: "seed", RetainedPayloadCountLimit: 20,
			RetainedPayloadByteLimit: 2 * 1024 * 1024, RetainedPayloadCount: 1,
			RetainedPayloadBytes: 128, RollbackPayloadFloor: 2,
			CreatedAt: now, UpdatedAt: now,
		},
		Active: &version,
	}
}

func testServerAvatarSummary(version AvatarVersion) AvatarVersionSummary {
	return AvatarVersionSummary{
		ID: version.ID, AccountID: version.AccountID, RealmID: version.RealmID,
		AgentID: version.AgentID, Version: version.Version, ParentVersion: version.ParentVersion,
		LineageGeneration: version.LineageGeneration,
		SubjectForm:       version.SubjectForm, SVGSHA256: version.SVGSHA256,
		LockedLayersSHA256: version.LockedLayersSHA256, Style: version.Style,
		ProposedBy: version.ProposedBy, ProposedAt: version.ProposedAt,
		IsActive: version.IsActive, IsProposed: version.IsProposed,
		WasActivated: version.WasActivated, RollbackEligible: version.RollbackEligible,
		Rejected: version.Rejected, LastActivatedAt: version.LastActivatedAt, RejectedAt: version.RejectedAt,
		PayloadState: version.PayloadState, PayloadBytes: version.PayloadBytes,
		PayloadCompactedAt:      version.PayloadCompactedAt,
		PayloadCompactionReason: version.PayloadCompactionReason,
	}
}

func testServerAvatarStyle() AvatarStyleView {
	now := time.Date(2026, 7, 17, 20, 0, 0, 0, time.UTC)
	return AvatarStyleView{
		RealmID: "realm_self", StyleRevision: 2,
		StylePack: avatar.BuiltInFlatVectorStylePack(),
		Rollout: &AvatarStyleRollout{
			StyleRevision: 2, StylePackID: avatar.DefaultStylePackID, StylePackVersion: 1,
			Status: "running", ProcessedProfileCount: 3,
			BatchCount: 1, LastBatchSize: 3, CreatedAt: now, StartedAt: &now, UpdatedAt: now,
		},
		CreatedAt: now, UpdatedAt: now,
	}
}

func testServerAvatarMutation(operation string, view AvatarView) AvatarMutationResult {
	return AvatarMutationResult{
		Avatar: view,
		Receipt: AvatarMutationReceipt{
			Operation: operation, Actor: AvatarActor{Kind: PrincipalKindAgent, ID: "agent_self"},
			RequestHash: strings.Repeat("b", 64), ResultRevision: 5, ResultVersion: 2,
			ResultLineageGeneration: 1,
			CreatedAt:               time.Date(2026, 7, 17, 20, 1, 0, 0, time.UTC),
		},
	}
}

func testAvatarAgentPrincipal() DomainPrincipal {
	return DomainPrincipal{
		Kind: PrincipalKindAgent, ID: "agent_self", AccountID: "acc_self", RealmID: "realm_self",
		AgentName: "Juniper", RealmName: "Default", AccountStatus: "active", AccessProfile: AccessProfileFull,
	}
}

func testAvatarPrincipalAuth(_ context.Context, token string) (DomainPrincipal, bool, error) {
	switch token {
	case "agent-token":
		return testAvatarAgentPrincipal(), true, nil
	case "domain-operator-token":
		return DomainPrincipal{Kind: PrincipalKindOperator, ID: "opr_self", AccountID: "acc_self", AccountStatus: "active", AccessProfile: AccessProfileFull}, true, nil
	case "curator-token":
		p := testAvatarAgentPrincipal()
		p.AccessProfile = AccessProfileCuratorApply
		return p, true, nil
	case "suspended-token":
		p := testAvatarAgentPrincipal()
		p.AccountStatus = "suspended"
		return p, true, nil
	default:
		return DomainPrincipal{}, false, nil
	}
}

func testAvatarOperatorAuth(_ context.Context, token string) (string, string, string, bool, error) {
	if token == "operator-token" {
		return "opr_self", "acc_self", "active", true, nil
	}
	return "", "", "", false, nil
}

func sameAvatarPrincipal(a, b DomainPrincipal) bool {
	return a.Kind == b.Kind && a.ID == b.ID && a.AccountID == b.AccountID && a.RealmID == b.RealmID &&
		a.AgentName == b.AgentName && a.RealmName == b.RealmName && a.AccountStatus == b.AccountStatus && a.AccessProfile == b.AccessProfile
}

func avatarRequest(t *testing.T, baseURL, method, path, token, idempotencyKey, body string, wantStatus int) []byte {
	t.Helper()
	request, err := http.NewRequest(method, baseURL+path, bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != wantStatus {
		t.Fatalf("%s %s status = %d, want %d; body=%s", method, path, response.StatusCode, wantStatus, raw)
	}
	if got := response.Header.Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("%s %s Cache-Control = %q", method, path, got)
	}
	return raw
}

func decodeAvatarTestJSON(t *testing.T, raw []byte, dst any) {
	t.Helper()
	if err := json.Unmarshal(raw, dst); err != nil {
		t.Fatalf("decode JSON: %v; body=%s", err, raw)
	}
}

func assertAvatarMutationEnvelope(t *testing.T, raw []byte) {
	t.Helper()
	var out struct {
		SchemaVersion string                `json:"schema_version"`
		Avatar        AvatarView            `json:"avatar"`
		Receipt       AvatarMutationReceipt `json:"receipt"`
	}
	decodeAvatarTestJSON(t, raw, &out)
	if out.SchemaVersion != "witself.v0" || out.Avatar.Profile.AgentID == "" || out.Avatar.Profile.LineageGeneration != 1 ||
		out.Receipt.RequestHash == "" || out.Receipt.ResultLineageGeneration != 1 {
		t.Fatalf("avatar mutation envelope = %+v", out)
	}
}
