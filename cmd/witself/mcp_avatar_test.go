package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/witwave-ai/witself/internal/avatar"
	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

type fakeAvatarMCPBackend struct {
	*fakeMCPBackend
	showCalls       int
	historyCalls    int
	historyOptions  client.AvatarHistoryOptions
	versionCalls    int
	lastVersion     int64
	styleCalls      int
	lastProposal    client.ProposeAvatarInput
	lastActivate    client.ActivateAvatarInput
	lastRollback    client.RollbackAvatarInput
	resetCalls      int
	lastReset       client.ResetAvatarInput
	generationCalls int
	lastGeneration  client.AvatarGenerationFailureInput
}

func newFakeAvatarMCPBackend() *fakeAvatarMCPBackend {
	return &fakeAvatarMCPBackend{fakeMCPBackend: &fakeMCPBackend{}}
}

func (b *fakeAvatarMCPBackend) ShowAvatar(context.Context) (client.AvatarView, error) {
	b.showCalls++
	return fakeAvatarMCPView(), nil
}

func (b *fakeAvatarMCPBackend) AvatarHistory(_ context.Context, opts client.AvatarHistoryOptions) (client.AvatarHistoryPage, error) {
	b.historyCalls++
	b.historyOptions = opts
	activatedAt := time.Date(2026, 7, 17, 20, 2, 0, 0, time.UTC)
	if opts.BeforeVersion == 3 {
		rejectedAt := activatedAt.Add(time.Minute)
		return client.AvatarHistoryPage{SchemaVersion: "witself.v0", Versions: []client.AvatarVersionSummary{
			{Version: 2, LineageGeneration: 1, WasActivated: true, RollbackEligible: true, LastActivatedAt: &activatedAt},
			{Version: 1, LineageGeneration: 1, Rejected: true, RejectedAt: &rejectedAt},
		}}, nil
	}
	return client.AvatarHistoryPage{SchemaVersion: "witself.v0", Versions: []client.AvatarVersionSummary{
		{Version: 4, LineageGeneration: 2, IsActive: true, WasActivated: true, LastActivatedAt: &activatedAt},
		{Version: 3, LineageGeneration: 2, WasActivated: true, RollbackEligible: true, LastActivatedAt: &activatedAt},
	}, NextBeforeVersion: 3}, nil
}

func (b *fakeAvatarMCPBackend) ShowAvatarVersion(_ context.Context, version int64) (client.AvatarVersion, error) {
	b.versionCalls++
	b.lastVersion = version
	return client.AvatarVersion{
		Version: version, LineageGeneration: 2, Description: "Exact portrait", VisualSpec: json.RawMessage(`{"expression":"calm"}`),
		SVG: `<svg xmlns="http://www.w3.org/2000/svg"></svg>`, SVGSHA256: strings.Repeat("a", 64),
	}, nil
}

func (b *fakeAvatarMCPBackend) ShowAvatarStyle(context.Context) (client.AvatarStyleView, error) {
	b.styleCalls++
	return client.AvatarStyleView{RealmID: "realm_1", StyleRevision: 1, StylePack: avatar.BuiltInFlatVectorStylePack()}, nil
}

func (b *fakeAvatarMCPBackend) ProposeAvatar(_ context.Context, in client.ProposeAvatarInput) (client.AvatarMutationResult, error) {
	b.lastProposal = in
	return client.AvatarMutationResult{Avatar: fakeAvatarMCPView(), Receipt: client.AvatarMutationReceipt{Operation: "propose", ResultRevision: 2, ResultLineageGeneration: 2}}, nil
}

func (b *fakeAvatarMCPBackend) ActivateAvatar(_ context.Context, in client.ActivateAvatarInput) (client.AvatarMutationResult, error) {
	b.lastActivate = in
	return client.AvatarMutationResult{Avatar: fakeAvatarMCPView(), Receipt: client.AvatarMutationReceipt{Operation: "activate", ResultRevision: 3, ResultLineageGeneration: 2}}, nil
}

func (b *fakeAvatarMCPBackend) RollbackAvatar(_ context.Context, in client.RollbackAvatarInput) (client.AvatarMutationResult, error) {
	b.lastRollback = in
	return client.AvatarMutationResult{Avatar: fakeAvatarMCPView(), Receipt: client.AvatarMutationReceipt{Operation: "rollback", ResultRevision: 4, ResultLineageGeneration: 2}}, nil
}

func (b *fakeAvatarMCPBackend) ResetAvatar(_ context.Context, in client.ResetAvatarInput) (client.AvatarMutationResult, error) {
	b.resetCalls++
	b.lastReset = in
	view := fakeAvatarMCPView()
	view.Profile.LineageGeneration = 3
	return client.AvatarMutationResult{Avatar: view, Receipt: client.AvatarMutationReceipt{Operation: "reset", ResultRevision: 5, ResultLineageGeneration: 3}}, nil
}

func (b *fakeAvatarMCPBackend) FailAvatarGeneration(_ context.Context, in client.AvatarGenerationFailureInput) (client.AvatarMutationResult, error) {
	b.generationCalls++
	b.lastGeneration = in
	return client.AvatarMutationResult{Avatar: fakeAvatarMCPView(), Receipt: client.AvatarMutationReceipt{Operation: "generation_fail", ResultRevision: 5, ResultLineageGeneration: 2}}, nil
}

func fakeAvatarMCPView() client.AvatarView {
	return client.AvatarView{Profile: client.AvatarProfile{
		AccountID: "acc_1", RealmID: "realm_1", AgentID: "agent_1",
		SubjectForm: avatar.SubjectAnimal, AutonomyPolicy: avatar.AutonomyAgentSelfManaged,
		Status: avatar.StatusActive, LineageGeneration: 2, ProfileRevision: 1,
	}}
}

func TestAvatarMCPRegistersAgentTokenToolsAndCallsBackend(t *testing.T) {
	ctx := context.Background()
	backend := newFakeAvatarMCPBackend()
	server := newWitselfMCPServer(backend)
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = serverSession.Close() }()
	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "avatar-test", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = clientSession.Close() }()

	page, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	tools := make(map[string]*mcp.Tool, len(page.Tools))
	for _, tool := range page.Tools {
		tools[tool.Name] = tool
	}
	reads := []string{"witself.avatar.show", "witself.avatar.history", "witself.avatar.version.show", "witself.avatar.style.show"}
	writes := []struct {
		name        string
		destructive bool
	}{
		{name: "witself.avatar.propose"},
		{name: "witself.avatar.activate", destructive: true},
		{name: "witself.avatar.rollback", destructive: true},
		{name: "witself.avatar.reset", destructive: true},
		{name: "witself.avatar.generation.fail", destructive: true},
	}
	for _, name := range reads {
		assertMCPToolAnnotations(t, tools[name], name, true, false, true)
	}
	for _, write := range writes {
		assertMCPToolAnnotations(t, tools[write.name], write.name, false, write.destructive, true)
	}
	for _, name := range append(reads, "witself.avatar.propose") {
		description := tools[name].Description
		if !strings.Contains(description, "untrusted") || !strings.Contains(description, "no ") && !strings.Contains(description, "performs no") {
			t.Errorf("%s description lacks untrusted/no-inference boundary: %q", name, description)
		}
	}
	for _, want := range []string{
		"current user's explicit request", "first read avatar.show", "no durable active or proposed version",
		"do not call reset", "already at a fresh start", "bounded generation-due flow", "exactly one call",
		"agent_self_managed", "agent_proposes and operator_only require an operator",
		"Vague dissatisfaction is not reset authority", "reopen the agent-owned initial fitting flow",
		"broad freedom to revise form, palette, and defining details locally",
		"submit only its one chosen final candidate", "lineage retirement, never purge",
	} {
		if !strings.Contains(tools["witself.avatar.reset"].Description, want) {
			t.Errorf("avatar reset description omitted %q: %q", want, tools["witself.avatar.reset"].Description)
		}
	}
	for _, want := range []string{
		"active agent may inspect and substantially revise ephemeral local drafts from its own perspective",
		"without asking the user or operator to choose the design",
		"Do not put those drafts in repository or project files",
		"clean up temporary artifacts",
		"Submit only the agent-chosen final candidate",
		"never send intermediate or discarded drafts",
		"every accepted proposal is immutable server state",
		"one bounded submission attempt after local review",
		"preserve the user's work",
	} {
		if !strings.Contains(tools["witself.avatar.propose"].Description, want) {
			t.Errorf("avatar propose description omitted %q: %q", want, tools["witself.avatar.propose"].Description)
		}
	}
	for _, want := range []string{
		"agent_self_managed initial proposal",
		"activation records the active agent's acceptance and settles its chosen avatar",
		"Under agent_proposes, creative selection is complete but identity remains unsettled until operator activation",
	} {
		if !strings.Contains(tools["witself.avatar.activate"].Description, want) {
			t.Errorf("avatar activate description omitted %q: %q", want, tools["witself.avatar.activate"].Description)
		}
	}

	call := func(name string, args map[string]any) *mcp.CallToolResult {
		t.Helper()
		result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if result.IsError {
			t.Fatalf("%s returned error: %#v", name, result.Content)
		}
		return result
	}
	showResult := call("witself.avatar.show", map[string]any{})
	historyResult := call("witself.avatar.history", map[string]any{"limit": 2, "before_version": 5})
	versionResult := call("witself.avatar.version.show", map[string]any{"version": 2})
	call("witself.avatar.style.show", map[string]any{})
	call("witself.avatar.propose", map[string]any{
		"expected_profile_revision": 1, "parent_version": 1,
		"style_pack_id": avatar.DefaultStylePackID, "style_pack_version": 1,
		"subject_form": "animal", "description": "A curious fox",
		"visual_spec":     map[string]any{"expression": "curious"},
		"svg":             `<svg xmlns="http://www.w3.org/2000/svg"></svg>`,
		"provenance":      map[string]any{"runtime": "codex", "model": "gpt-test"},
		"idempotency_key": "proposal-1",
	})
	call("witself.avatar.activate", map[string]any{
		"version": 2, "expected_profile_revision": 2, "idempotency_key": "activate-1",
	})
	call("witself.avatar.rollback", map[string]any{
		"version": 1, "expected_profile_revision": 3, "idempotency_key": "rollback-1",
	})
	resetResult := call("witself.avatar.reset", map[string]any{
		"expected_profile_revision": 4, "reason_code": "user_requested", "idempotency_key": "reset-1",
	})
	validReason := "a." + strings.Repeat("b", maxAvatarReasonCodeBytes-2)
	call("witself.avatar.generation.fail", map[string]any{
		"expected_profile_revision": 5, "reason_code": validReason, "idempotency_key": "fail-1",
	})
	historyJSON, err := json.Marshal(historyResult.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var history client.AvatarHistoryPage
	if err := json.Unmarshal(historyJSON, &history); err != nil {
		t.Fatalf("decode structured history: %v; JSON=%s", err, historyJSON)
	}
	if len(history.Versions) != 2 || history.NextBeforeVersion != 3 || !history.Versions[0].IsActive ||
		history.Versions[0].LineageGeneration != 2 ||
		!history.Versions[0].WasActivated || history.Versions[0].LastActivatedAt == nil ||
		!history.Versions[1].RollbackEligible {
		t.Fatalf("MCP history projection = %+v", history.Versions)
	}
	nextHistoryResult := call("witself.avatar.history", map[string]any{
		"limit": 2, "before_version": history.NextBeforeVersion,
	})
	nextHistoryJSON, err := json.Marshal(nextHistoryResult.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var nextHistory client.AvatarHistoryPage
	if err := json.Unmarshal(nextHistoryJSON, &nextHistory); err != nil || len(nextHistory.Versions) != 2 ||
		nextHistory.Versions[0].Version != 2 || history.Versions[1].Version == nextHistory.Versions[0].Version {
		t.Fatalf("MCP history pagination = first:%+v next:%+v err=%v", history, nextHistory, err)
	}
	versionJSON, err := json.Marshal(versionResult.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var versionOutput struct {
		Version client.AvatarVersion `json:"version"`
	}
	if err := json.Unmarshal(versionJSON, &versionOutput); err != nil || versionOutput.Version.Version != 2 || versionOutput.Version.LineageGeneration != 2 || versionOutput.Version.SVG == "" {
		t.Fatalf("MCP exact version = %+v / %v", versionOutput, err)
	}
	showJSON, err := json.Marshal(showResult.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var showOutput mcpAvatarOutput
	if err := json.Unmarshal(showJSON, &showOutput); err != nil || showOutput.Avatar.Profile.LineageGeneration != 2 {
		t.Fatalf("MCP avatar lineage = %+v / %v", showOutput, err)
	}
	resetJSON, err := json.Marshal(resetResult.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var resetOutput client.AvatarMutationResult
	if err := json.Unmarshal(resetJSON, &resetOutput); err != nil || resetOutput.Avatar.Profile.LineageGeneration != 3 ||
		resetOutput.Receipt.ResultLineageGeneration != 3 {
		t.Fatalf("MCP reset lineage = %+v / %v", resetOutput, err)
	}
	invalidVersion, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name: "witself.avatar.version.show", Arguments: map[string]any{"version": 0},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !invalidVersion.IsError {
		t.Fatal("MCP accepted a non-positive avatar version")
	}
	invalidHistory, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name: "witself.avatar.history", Arguments: map[string]any{"limit": 101},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !invalidHistory.IsError {
		t.Fatal("MCP accepted an over-limit avatar history page")
	}
	invalidFailure, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name: "witself.avatar.generation.fail",
		Arguments: map[string]any{
			"expected_profile_revision": 5,
			"reason_code":               "a" + strings.Repeat("b", maxAvatarReasonCodeBytes),
			"idempotency_key":           "fail-2",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !invalidFailure.IsError {
		t.Fatal("MCP accepted a 129-byte avatar reason code")
	}
	invalidReset, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name: "witself.avatar.reset",
		Arguments: map[string]any{
			"expected_profile_revision": 4,
			"reason_code":               "a" + strings.Repeat("b", maxAvatarReasonCodeBytes),
			"idempotency_key":           "reset-2",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !invalidReset.IsError {
		t.Fatal("MCP accepted a 129-byte avatar reset reason code")
	}

	if backend.showCalls != 1 || backend.historyCalls != 2 || backend.versionCalls != 1 || backend.styleCalls != 1 ||
		backend.historyOptions.Limit != 2 || backend.historyOptions.BeforeVersion != 3 || backend.lastVersion != 2 {
		t.Fatalf("read calls/options = show %d history %d version %d style %d opts=%+v last_version=%d",
			backend.showCalls, backend.historyCalls, backend.versionCalls, backend.styleCalls, backend.historyOptions, backend.lastVersion)
	}
	if backend.lastProposal.ExpectedProfileRevision != 1 || backend.lastProposal.ParentVersion != 1 ||
		backend.lastProposal.SubjectForm != avatar.SubjectAnimal || backend.lastProposal.IdempotencyKey != "proposal-1" ||
		backend.lastProposal.Provenance.Runtime != "codex" {
		t.Fatalf("proposal = %#v", backend.lastProposal)
	}
	var spec map[string]any
	if err := json.Unmarshal(backend.lastProposal.VisualSpec, &spec); err != nil || spec["expression"] != "curious" {
		t.Fatalf("visual spec = %#v / %v", spec, err)
	}
	if backend.lastActivate.Version != 2 || backend.lastActivate.ExpectedProfileRevision != 2 || backend.lastActivate.IdempotencyKey != "activate-1" {
		t.Fatalf("activate = %#v", backend.lastActivate)
	}
	if backend.lastRollback.Version != 1 || backend.lastRollback.ExpectedProfileRevision != 3 || backend.lastRollback.IdempotencyKey != "rollback-1" {
		t.Fatalf("rollback = %#v", backend.lastRollback)
	}
	if backend.resetCalls != 1 || backend.lastReset.ExpectedProfileRevision != 4 || backend.lastReset.ReasonCode != "user_requested" || backend.lastReset.IdempotencyKey != "reset-1" {
		t.Fatalf("reset = %#v", backend.lastReset)
	}
	if backend.generationCalls != 1 || backend.lastGeneration.ReasonCode != validReason || backend.lastGeneration.ExpectedProfileRevision != 5 || backend.lastGeneration.IdempotencyKey != "fail-1" {
		t.Fatalf("generation failure = %#v", backend.lastGeneration)
	}
}

func TestAvatarMCPReadOnlyKeepsReadsAndRemovesWrites(t *testing.T) {
	ctx := context.Background()
	backend := newFakeAvatarMCPBackend()
	server := newWitselfMCPServerForRuntimeOptions(backend, transcriptcapture.RuntimeCursor, mcpServerOptions{ReadOnly: true})
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = serverSession.Close() }()
	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "avatar-test", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = clientSession.Close() }()
	page, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]*mcp.Tool{}
	for _, tool := range page.Tools {
		found[tool.Name] = tool
	}
	for _, name := range []string{"witself.avatar.show", "witself.avatar.history", "witself.avatar.version.show", "witself.avatar.style.show"} {
		if found[name] == nil {
			t.Errorf("read-only MCP omitted %s", name)
		}
	}
	for _, name := range []string{"witself.avatar.propose", "witself.avatar.activate", "witself.avatar.rollback", "witself.avatar.reset", "witself.avatar.generation.fail"} {
		if found[name] != nil {
			t.Errorf("read-only MCP retained %s", name)
		}
	}
	instructions := clientSession.InitializeResult().Instructions
	for _, want := range []string{"avatar_checkpoint", "witself.avatar.show", "witself.avatar.style.show", "requires a full MCP profile", "without claiming the avatar was established"} {
		if !strings.Contains(instructions, want) {
			t.Errorf("read-only MCP instructions omitted %q", want)
		}
	}
}

func TestAvatarMCPGrokUsesPortableNamesAndInstructions(t *testing.T) {
	ctx := context.Background()
	server := newWitselfMCPServerForRuntime(newFakeAvatarMCPBackend(), transcriptcapture.RuntimeGrokBuild)
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = serverSession.Close() }()
	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "avatar-test", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = clientSession.Close() }()
	page, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]*mcp.Tool{}
	for _, tool := range page.Tools {
		found[tool.Name] = tool
	}
	for _, dotted := range []string{
		"witself.avatar.show", "witself.avatar.history", "witself.avatar.version.show", "witself.avatar.style.show",
		"witself.avatar.propose", "witself.avatar.activate", "witself.avatar.rollback", "witself.avatar.reset", "witself.avatar.generation.fail",
	} {
		portable := strings.ReplaceAll(dotted, ".", "_")
		if found[portable] == nil {
			t.Errorf("Grok MCP omitted %s", portable)
		}
		if found[dotted] != nil {
			t.Errorf("Grok MCP retained dotted name %s", dotted)
		}
	}
	assertMCPToolAnnotations(t, found["witself_avatar_reset"], "witself_avatar_reset", false, true, true)
	instructions := clientSession.InitializeResult().Instructions
	for _, portable := range []string{"witself_avatar_show", "witself_avatar_style_show", "witself_avatar_propose", "witself_avatar_reset", "witself_avatar_generation_fail"} {
		if !strings.Contains(instructions, portable) {
			t.Errorf("Grok instructions omitted %s", portable)
		}
	}
	if strings.Contains(instructions, "witself.avatar.") {
		t.Fatalf("Grok instructions retained dotted avatar names: %q", instructions)
	}
}

func TestAvatarManagedProviderInstructionsNameRuntimeTools(t *testing.T) {
	providers := map[string]string{
		"codex":  string(codexMemoryRoutingBlock),
		"claude": string(claudeMemoryRoutingBlock),
		"cursor": string(cursorMemoryRoutingBlock),
		"grok":   string(grokMemoryRoutingBlock),
	}
	for provider, instructions := range providers {
		t.Run(provider, func(t *testing.T) {
			if !strings.Contains(instructions, "avatar_checkpoint") {
				t.Fatal("managed instructions omitted avatar checkpoint routing")
			}
			if provider == "grok" {
				if !strings.Contains(instructions, "witself_avatar_propose") || !strings.Contains(instructions, "witself_avatar_reset") || strings.Contains(instructions, "witself.avatar.propose") {
					t.Fatalf("Grok avatar tool names are not portable: %q", instructions)
				}
				return
			}
			if !strings.Contains(instructions, "witself.avatar.propose") || !strings.Contains(instructions, "witself.avatar.reset") || strings.Contains(instructions, "witself_avatar_propose") {
				t.Fatalf("%s avatar tool names are not dotted", provider)
			}
		})
	}
}
