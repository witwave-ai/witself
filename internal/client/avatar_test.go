package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/avatar"
)

func TestAvatarSelfClientContract(t *testing.T) {
	now := time.Date(2026, 7, 17, 14, 15, 16, 0, time.UTC)
	view := testAvatarView(now)
	style := testAvatarStyleView(now)
	receipt := AvatarMutationReceipt{
		Operation: "propose", Actor: AvatarActor{Kind: "agent", ID: "agent_1"},
		RequestHash: "hash_1", ResultRevision: 3, ResultVersion: 2, ResultLineageGeneration: 1, CreatedAt: now,
	}
	wantKeys := map[string]string{
		"/v1/self/avatar/proposals":         "proposal-1",
		"/v1/self/avatar:activate":          "activate-1",
		"/v1/self/avatar:rollback":          "rollback-1",
		"/v1/self/avatar:reset":             "reset-1",
		"/v1/self/avatar:generation-failed": "failure-1",
	}
	calls := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls[r.Method+" "+r.URL.Path]++
		if got := r.Header.Get("Authorization"); got != "Bearer agent-token" {
			t.Fatalf("authorization = %q", got)
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/self/avatar":
			writeAvatarClientJSON(t, w, map[string]any{"avatar": view})
		case "GET /v1/self/avatar/history":
			writeAvatarClientJSON(t, w, AvatarHistoryPage{SchemaVersion: "witself.v0", Versions: []AvatarVersionSummary{testAvatarSummary(*view.Active)}})
		case "GET /v1/self/avatar/versions/1":
			writeAvatarClientJSON(t, w, map[string]any{"schema_version": "witself.v0", "version": view.Active})
		case "GET /v1/self/avatar/style":
			writeAvatarClientJSON(t, w, map[string]any{"style": style})
		case "POST /v1/self/avatar/proposals":
			body := readAvatarClientBody(t, r)
			assertAvatarClientMutationHeaders(t, r, wantKeys[r.URL.Path], body)
			for field, want := range map[string]string{
				"expected_profile_revision": "2", "parent_version": "1",
				"style_pack_id": `"witself-flat-portrait"`, "style_pack_version": "1",
				"subject_form": `"animal"`, "description": `"A curious fox"`,
				"visual_spec": `{"expression":"curious"}`,
			} {
				if got := string(body[field]); got != want {
					t.Errorf("proposal %s = %s, want %s", field, got, want)
				}
			}
			var svg string
			if err := json.Unmarshal(body["svg"], &svg); err != nil || svg != "<svg></svg>" {
				t.Errorf("proposal svg = %q / %v", svg, err)
			}
			writeAvatarClientJSON(t, w, AvatarMutationResult{Avatar: view, Receipt: receipt})
		case "POST /v1/self/avatar:activate":
			body := readAvatarClientBody(t, r)
			assertAvatarClientMutationHeaders(t, r, wantKeys[r.URL.Path], body)
			assertAvatarClientVersionMutation(t, body, 2, 3)
			writeAvatarClientJSON(t, w, AvatarMutationResult{Avatar: view, Receipt: receipt})
		case "POST /v1/self/avatar:rollback":
			body := readAvatarClientBody(t, r)
			assertAvatarClientMutationHeaders(t, r, wantKeys[r.URL.Path], body)
			assertAvatarClientVersionMutation(t, body, 1, 4)
			writeAvatarClientJSON(t, w, AvatarMutationResult{Avatar: view, Receipt: receipt})
		case "POST /v1/self/avatar:reset":
			body := readAvatarClientBody(t, r)
			assertAvatarClientMutationHeaders(t, r, wantKeys[r.URL.Path], body)
			if string(body["expected_profile_revision"]) != "6" || string(body["reason_code"]) != `"user_requested"` {
				t.Errorf("reset body = %s", mustAvatarClientJSON(t, body))
			}
			writeAvatarClientJSON(t, w, AvatarMutationResult{Avatar: view, Receipt: receipt})
		case "POST /v1/self/avatar:generation-failed":
			body := readAvatarClientBody(t, r)
			assertAvatarClientMutationHeaders(t, r, wantKeys[r.URL.Path], body)
			if string(body["expected_profile_revision"]) != "5" || string(body["reason_code"]) != `"model_unavailable"` {
				t.Errorf("failure body = %s", mustAvatarClientJSON(t, body))
			}
			writeAvatarClientJSON(t, w, AvatarMutationResult{Avatar: view, Receipt: receipt})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	ctx := context.Background()
	got, err := GetSelfAvatar(ctx, srv.URL+"/", "agent-token")
	if err != nil || got.Profile.AgentID != "agent_1" || got.Profile.LineageGeneration != 1 || got.Active == nil ||
		got.Active.SubjectForm != avatar.SubjectAnimal || got.Active.LineageGeneration != 1 {
		t.Fatalf("GetSelfAvatar = %#v, %v", got, err)
	}
	history, err := GetSelfAvatarHistory(ctx, srv.URL, "agent-token")
	if err != nil || history.SchemaVersion != "witself.v0" || len(history.Versions) != 1 || history.Versions[0].LineageGeneration != 1 {
		t.Fatalf("GetSelfAvatarHistory = %#v, %v", history, err)
	}
	exactVersion, err := GetSelfAvatarVersion(ctx, srv.URL, "agent-token", 1)
	if err != nil || exactVersion.Version != 1 || exactVersion.LineageGeneration != 1 || exactVersion.SVG == "" || len(exactVersion.VisualSpec) == 0 {
		t.Fatalf("GetSelfAvatarVersion = %#v, %v", exactVersion, err)
	}
	gotStyle, err := GetSelfAvatarStyle(ctx, srv.URL, "agent-token")
	if err != nil || gotStyle.StylePack.ID != avatar.DefaultStylePackID || gotStyle.StyleRevision != 1 {
		t.Fatalf("GetSelfAvatarStyle = %#v, %v", gotStyle, err)
	}
	proposal, err := ProposeSelfAvatar(ctx, srv.URL, "agent-token", ProposeAvatarInput{
		ExpectedProfileRevision: 2, ParentVersion: 1,
		StylePackID: avatar.DefaultStylePackID, StylePackVersion: 1,
		SubjectForm: avatar.SubjectAnimal, Description: "A curious fox",
		VisualSpec: json.RawMessage(`{"expression":"curious"}`), SVG: "<svg></svg>",
		Provenance:     AvatarClientProvenance{Runtime: "codex", Model: "gpt-test"},
		IdempotencyKey: "proposal-1",
	})
	if err != nil || proposal.Receipt.ResultVersion != 2 || proposal.Receipt.ResultLineageGeneration != 1 || proposal.Avatar.Profile.ProfileRevision != 2 {
		t.Fatalf("ProposeSelfAvatar = %#v, %v", proposal, err)
	}
	if _, err := ActivateSelfAvatar(ctx, srv.URL, "agent-token", ActivateAvatarInput{Version: 2, ExpectedProfileRevision: 3, IdempotencyKey: "activate-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := RollbackSelfAvatar(ctx, srv.URL, "agent-token", RollbackAvatarInput{Version: 1, ExpectedProfileRevision: 4, IdempotencyKey: "rollback-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ReportSelfAvatarGenerationFailure(ctx, srv.URL, "agent-token", AvatarGenerationFailureInput{ExpectedProfileRevision: 5, ReasonCode: "model_unavailable", IdempotencyKey: "failure-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ResetSelfAvatar(ctx, srv.URL, "agent-token", ResetAvatarInput{ExpectedProfileRevision: 6, ReasonCode: "user_requested", IdempotencyKey: "reset-1"}); err != nil {
		t.Fatal(err)
	}

	for _, route := range []string{
		"GET /v1/self/avatar", "GET /v1/self/avatar/history", "GET /v1/self/avatar/versions/1",
		"GET /v1/self/avatar/style",
		"POST /v1/self/avatar/proposals", "POST /v1/self/avatar:activate",
		"POST /v1/self/avatar:rollback", "POST /v1/self/avatar:reset", "POST /v1/self/avatar:generation-failed",
	} {
		if calls[route] != 1 {
			t.Errorf("%s calls = %d, want 1", route, calls[route])
		}
	}
}

func TestAvatarOperatorClientContract(t *testing.T) {
	now := time.Date(2026, 7, 17, 14, 15, 16, 0, time.UTC)
	view := testAvatarView(now)
	style := testAvatarStyleView(now)
	wantKeys := map[string]string{
		"/v1/agents/agent_1/avatar/proposals":      "operator-proposal-1",
		"/v1/agents/agent_1/avatar:activate":       "operator-activate-1",
		"/v1/agents/agent_1/avatar:reject":         "operator-reject-1",
		"/v1/agents/agent_1/avatar:rollback":       "operator-rollback-1",
		"/v1/agents/agent_1/avatar:reset":          "operator-reset-1",
		"/v1/agents/agent_1/avatar-policy":         "operator-policy-1",
		"/v1/realms/realm_1/avatar-style/versions": "style-version-1",
	}
	calls := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls[r.Method+" "+r.URL.Path]++
		if got := r.Header.Get("Authorization"); got != "Bearer operator-token" {
			t.Fatalf("authorization = %q", got)
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/agents/agent_1/avatar":
			writeAvatarClientJSON(t, w, map[string]any{"avatar": view})
		case "GET /v1/agents/agent_1/avatar/history":
			writeAvatarClientJSON(t, w, AvatarHistoryPage{SchemaVersion: "witself.v0", Versions: []AvatarVersionSummary{testAvatarSummary(*view.Active)}})
		case "GET /v1/agents/agent_1/avatar/versions/1":
			writeAvatarClientJSON(t, w, map[string]any{"schema_version": "witself.v0", "version": view.Active})
		case "POST /v1/agents/agent_1/avatar/proposals":
			body := readAvatarClientBody(t, r)
			assertAvatarClientMutationHeaders(t, r, wantKeys[r.URL.Path], body)
			if string(body["expected_profile_revision"]) != "4" || string(body["subject_form"]) != `"human"` {
				t.Errorf("operator proposal body = %s", mustAvatarClientJSON(t, body))
			}
			writeAvatarClientJSON(t, w, AvatarMutationResult{Avatar: view})
		case "POST /v1/agents/agent_1/avatar:activate":
			body := readAvatarClientBody(t, r)
			assertAvatarClientMutationHeaders(t, r, wantKeys[r.URL.Path], body)
			assertAvatarClientVersionMutation(t, body, 2, 5)
			writeAvatarClientJSON(t, w, AvatarMutationResult{Avatar: view})
		case "POST /v1/agents/agent_1/avatar:reject":
			body := readAvatarClientBody(t, r)
			assertAvatarClientMutationHeaders(t, r, wantKeys[r.URL.Path], body)
			if string(body["version"]) != "2" || string(body["expected_profile_revision"]) != "6" || string(body["reason_code"]) != `"off_style"` {
				t.Errorf("reject body = %s", mustAvatarClientJSON(t, body))
			}
			writeAvatarClientJSON(t, w, AvatarMutationResult{Avatar: view})
		case "POST /v1/agents/agent_1/avatar:rollback":
			body := readAvatarClientBody(t, r)
			assertAvatarClientMutationHeaders(t, r, wantKeys[r.URL.Path], body)
			assertAvatarClientVersionMutation(t, body, 1, 7)
			writeAvatarClientJSON(t, w, AvatarMutationResult{Avatar: view})
		case "POST /v1/agents/agent_1/avatar:reset":
			body := readAvatarClientBody(t, r)
			assertAvatarClientMutationHeaders(t, r, wantKeys[r.URL.Path], body)
			if string(body["expected_profile_revision"]) != "8" || string(body["reason_code"]) != `"new_direction"` {
				t.Errorf("operator reset body = %s", mustAvatarClientJSON(t, body))
			}
			writeAvatarClientJSON(t, w, AvatarMutationResult{Avatar: view})
		case "PATCH /v1/agents/agent_1/avatar-policy":
			body := readAvatarClientBody(t, r)
			assertAvatarClientMutationHeaders(t, r, wantKeys[r.URL.Path], body)
			if string(body["policy"]) != `"agent_proposes"` || string(body["expected_profile_revision"]) != "8" {
				t.Errorf("policy body = %s", mustAvatarClientJSON(t, body))
			}
			writeAvatarClientJSON(t, w, AvatarMutationResult{Avatar: view})
		case "GET /v1/realms/realm_1/avatar-style":
			writeAvatarClientJSON(t, w, map[string]any{"style": style})
		case "POST /v1/realms/realm_1/avatar-style/versions":
			body := readAvatarClientBody(t, r)
			assertAvatarClientMutationHeaders(t, r, wantKeys[r.URL.Path], body)
			if string(body["expected_style_revision"]) != "1" || len(body["style_pack"]) == 0 {
				t.Errorf("style body = %s", mustAvatarClientJSON(t, body))
			}
			writeAvatarClientJSON(t, w, AvatarStyleMutationResult{Style: style})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	ctx := context.Background()
	if got, err := GetAgentAvatar(ctx, srv.URL, "operator-token", "agent_1"); err != nil || got.Profile.AgentID != "agent_1" {
		t.Fatalf("GetAgentAvatar = %#v, %v", got, err)
	}
	if got, err := GetAgentAvatarHistory(ctx, srv.URL, "operator-token", "agent_1"); err != nil || got.SchemaVersion != "witself.v0" || len(got.Versions) != 1 {
		t.Fatalf("GetAgentAvatarHistory = %#v, %v", got, err)
	}
	if got, err := GetAgentAvatarVersion(ctx, srv.URL, "operator-token", "agent_1", 1); err != nil || got.Version != 1 || got.SVG == "" {
		t.Fatalf("GetAgentAvatarVersion = %#v, %v", got, err)
	}
	if _, err := ProposeAgentAvatar(ctx, srv.URL, "operator-token", "agent_1", ProposeAvatarInput{
		ExpectedProfileRevision: 4, StylePackID: avatar.DefaultStylePackID, StylePackVersion: 1,
		SubjectForm: avatar.SubjectHuman, Description: "A calm teammate",
		VisualSpec: json.RawMessage(`{"expression":"calm"}`), SVG: "<svg></svg>",
		IdempotencyKey: "operator-proposal-1",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := ActivateAgentAvatar(ctx, srv.URL, "operator-token", "agent_1", ActivateAvatarInput{Version: 2, ExpectedProfileRevision: 5, IdempotencyKey: "operator-activate-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := RejectAgentAvatar(ctx, srv.URL, "operator-token", "agent_1", RejectAvatarInput{Version: 2, ExpectedProfileRevision: 6, ReasonCode: "off_style", IdempotencyKey: "operator-reject-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := RollbackAgentAvatar(ctx, srv.URL, "operator-token", "agent_1", RollbackAvatarInput{Version: 1, ExpectedProfileRevision: 7, IdempotencyKey: "operator-rollback-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ResetAgentAvatar(ctx, srv.URL, "operator-token", "agent_1", ResetAvatarInput{ExpectedProfileRevision: 8, ReasonCode: "new_direction", IdempotencyKey: "operator-reset-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := UpdateAgentAvatarPolicy(ctx, srv.URL, "operator-token", "agent_1", UpdateAvatarPolicyInput{Policy: avatar.AutonomyAgentProposes, ExpectedProfileRevision: 8, IdempotencyKey: "operator-policy-1"}); err != nil {
		t.Fatal(err)
	}
	if got, err := GetRealmAvatarStyle(ctx, srv.URL, "operator-token", "realm_1"); err != nil || got.StylePack.ID != avatar.DefaultStylePackID {
		t.Fatalf("GetRealmAvatarStyle = %#v, %v", got, err)
	}
	if got, err := CreateRealmAvatarStyleVersion(ctx, srv.URL, "operator-token", "realm_1", CreateAvatarStyleVersionInput{ExpectedStyleRevision: 1, StylePack: avatar.BuiltInFlatVectorStylePack(), IdempotencyKey: "style-version-1"}); err != nil || got.Style.StyleRevision != 1 {
		t.Fatalf("CreateRealmAvatarStyleVersion = %#v, %v", got, err)
	}

	for _, route := range []string{
		"GET /v1/agents/agent_1/avatar", "GET /v1/agents/agent_1/avatar/history",
		"GET /v1/agents/agent_1/avatar/versions/1",
		"POST /v1/agents/agent_1/avatar/proposals",
		"POST /v1/agents/agent_1/avatar:activate",
		"POST /v1/agents/agent_1/avatar:reject", "POST /v1/agents/agent_1/avatar:rollback", "POST /v1/agents/agent_1/avatar:reset",
		"PATCH /v1/agents/agent_1/avatar-policy", "GET /v1/realms/realm_1/avatar-style",
		"POST /v1/realms/realm_1/avatar-style/versions",
	} {
		if calls[route] != 1 {
			t.Errorf("%s calls = %d, want 1", route, calls[route])
		}
	}
}

func TestAvatarClientNormalizesNullHistoryAndPreservesNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/self/avatar/history" {
			_, _ = w.Write([]byte(`{"schema_version":"witself.v0","versions":null}`))
			return
		}
		if r.URL.Path == "/v1/agents/agent_projection/avatar/history" {
			_, _ = w.Write([]byte(`{"schema_version":"witself.v0","versions":[` +
				`{"version":3,"is_active":true,"is_proposed":false,"was_activated":true,"rollback_eligible":false,"rejected":false,"last_activated_at":"2026-07-17T20:02:00Z"},` +
				`{"version":2,"is_active":false,"is_proposed":false,"was_activated":true,"rollback_eligible":true,"rejected":false},` +
				`{"version":1,"is_active":false,"is_proposed":false,"was_activated":false,"rollback_eligible":false,"rejected":true,"rejected_at":"2026-07-17T20:03:00Z"}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	history, err := GetSelfAvatarHistory(context.Background(), srv.URL, "token")
	if err != nil || history.Versions == nil || len(history.Versions) != 0 {
		t.Fatalf("history = %#v, %v", history, err)
	}
	projection, err := GetAgentAvatarHistory(context.Background(), srv.URL, "token", "agent_projection")
	if err != nil || len(projection.Versions) != 3 || !projection.Versions[0].IsActive ||
		!projection.Versions[0].WasActivated || projection.Versions[0].LastActivatedAt == nil ||
		!projection.Versions[1].RollbackEligible || !projection.Versions[2].Rejected || projection.Versions[2].RejectedAt == nil {
		t.Fatalf("history projection = %#v, %v", projection, err)
	}
	_, err = GetSelfAvatar(context.Background(), srv.URL, "token")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetSelfAvatar error = %v, want ErrNotFound", err)
	}
}

func TestAvatarClientHistoryPaginationUsesExclusiveVersionCursor(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v1/self/avatar/history" || r.URL.Query().Get("limit") != "2" {
			t.Fatalf("request URI = %s", r.URL.RequestURI())
		}
		before := r.URL.Query().Get("before_version")
		if before == "" {
			writeAvatarClientJSON(t, w, map[string]any{
				"versions":            []any{map[string]any{"version": 5}, map[string]any{"version": 4}},
				"next_before_version": 4,
			})
			return
		}
		if before != "4" {
			t.Fatalf("before_version = %q", before)
		}
		writeAvatarClientJSON(t, w, map[string]any{
			"versions":            []any{map[string]any{"version": 3}, map[string]any{"version": 2}},
			"next_before_version": 2,
		})
	}))
	defer srv.Close()

	first, err := GetSelfAvatarHistoryPage(context.Background(), srv.URL, "token", AvatarHistoryOptions{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	second, err := GetSelfAvatarHistoryPage(context.Background(), srv.URL, "token", AvatarHistoryOptions{
		Limit: 2, BeforeVersion: first.NextBeforeVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || first.NextBeforeVersion != 4 || second.NextBeforeVersion != 2 ||
		len(first.Versions) != 2 || len(second.Versions) != 2 ||
		first.Versions[1].Version == second.Versions[0].Version || second.Versions[0].Version != 3 {
		t.Fatalf("pagination continuity = first:%+v second:%+v", first, second)
	}
	if _, err := GetSelfAvatarHistoryPage(context.Background(), srv.URL, "token", AvatarHistoryOptions{Limit: 101}); err == nil {
		t.Fatal("client accepted over-limit avatar history page")
	}
	if _, err := GetSelfAvatarVersion(context.Background(), srv.URL, "token", 0); err == nil {
		t.Fatal("client accepted non-positive avatar version")
	}
}

func testAvatarView(now time.Time) AvatarView {
	style := avatar.StylePackRef{RealmID: "realm_1", StylePackID: avatar.DefaultStylePackID, Version: 1}
	version := AvatarVersion{
		ID: "avv_1", AccountID: "acc_1", RealmID: "realm_1", AgentID: "agent_1",
		Version: 1, LineageGeneration: 1, SubjectForm: avatar.SubjectAnimal,
		Description: "A curious fox", VisualSpec: json.RawMessage(`{"expression":"curious"}`),
		SVG: "<svg></svg>", SVGSHA256: "hash", Style: style,
		Provenance: AvatarClientProvenance{Runtime: "codex", Model: "gpt-test"},
		ProposedBy: AvatarActor{Kind: "agent", ID: "agent_1"}, ProposedAt: now,
	}
	return AvatarView{
		Profile: AvatarProfile{
			AccountID: "acc_1", RealmID: "realm_1", AgentID: "agent_1",
			SubjectForm: avatar.SubjectAnimal, AutonomyPolicy: avatar.AutonomyAgentSelfManaged,
			Status: avatar.StatusActive, Style: style, LineageGeneration: 1, ProfileRevision: 2, ActiveVersion: 1,
			LatestVersion: 1, FallbackSeed: "agent_1", CreatedAt: now, UpdatedAt: now,
		},
		Active: &version,
	}
}

func testAvatarSummary(version AvatarVersion) AvatarVersionSummary {
	return AvatarVersionSummary{
		ID: version.ID, AccountID: version.AccountID, RealmID: version.RealmID,
		AgentID: version.AgentID, Version: version.Version, ParentVersion: version.ParentVersion,
		LineageGeneration: version.LineageGeneration,
		SubjectForm:       version.SubjectForm, SVGSHA256: version.SVGSHA256, Style: version.Style,
		ProposedBy: version.ProposedBy, ProposedAt: version.ProposedAt,
		IsActive: version.IsActive, IsProposed: version.IsProposed,
		WasActivated: version.WasActivated, RollbackEligible: version.RollbackEligible,
		Rejected: version.Rejected, LastActivatedAt: version.LastActivatedAt, RejectedAt: version.RejectedAt,
	}
}

func testAvatarStyleView(now time.Time) AvatarStyleView {
	return AvatarStyleView{
		RealmID: "realm_1", StyleRevision: 1,
		StylePack: avatar.BuiltInFlatVectorStylePack(), CreatedAt: now, UpdatedAt: now,
	}
}

func readAvatarClientBody(t *testing.T, r *http.Request) map[string]json.RawMessage {
	t.Helper()
	var body map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body
}

func assertAvatarClientMutationHeaders(t *testing.T, r *http.Request, idempotencyKey string, body map[string]json.RawMessage) {
	t.Helper()
	if got := r.Header.Get("Idempotency-Key"); got != idempotencyKey {
		t.Errorf("Idempotency-Key = %q, want %q", got, idempotencyKey)
	}
	if got := r.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
	if _, leaked := body["idempotency_key"]; leaked {
		t.Errorf("body leaked idempotency_key: %s", mustAvatarClientJSON(t, body))
	}
	for _, forbidden := range []string{"account_id", "realm_id", "agent_id"} {
		if _, leaked := body[forbidden]; leaked {
			t.Errorf("body leaked identity selector %s: %s", forbidden, mustAvatarClientJSON(t, body))
		}
	}
}

func assertAvatarClientVersionMutation(t *testing.T, body map[string]json.RawMessage, version, revision int64) {
	t.Helper()
	if string(body["version"]) != mustAvatarClientJSON(t, version) ||
		string(body["expected_profile_revision"]) != mustAvatarClientJSON(t, revision) {
		t.Errorf("version mutation body = %s", mustAvatarClientJSON(t, body))
	}
}

func writeAvatarClientJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatal(err)
	}
}

func mustAvatarClientJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestAvatarWireTypesUseDomainEnums(t *testing.T) {
	profile := AvatarProfile{
		SubjectForm: avatar.SubjectInsect, AutonomyPolicy: avatar.AutonomyOperatorOnly,
		Status: avatar.StatusGenerationDue,
	}
	raw, err := json.Marshal(profile)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"subject_form": "insect", "autonomy_policy": "operator_only", "status": "generation_due",
	}
	for key, value := range want {
		if !reflect.DeepEqual(decoded[key], value) {
			t.Errorf("%s = %#v, want %#v", key, decoded[key], value)
		}
	}
}
