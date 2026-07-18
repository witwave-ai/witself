package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	avatardomain "github.com/witwave-ai/witself/internal/avatar"
)

func TestMigration50BackfillsAndRollsBackAvatarStatePostgres(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	st, dsn := newMigrationTestStore(t, baseDSN)
	migrationTestUpTo(t, dsn, 41)
	insertMigrationTestMemoryPrincipals(t, st)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	assertMigrationTestVersion(t, dsn, 50)
	ctx := context.Background()
	var styles, selections, profiles int
	if err := st.pool.QueryRow(ctx, `SELECT COUNT(*) FROM avatar_style_pack_versions`).Scan(&styles); err != nil {
		t.Fatal(err)
	}
	if err := st.pool.QueryRow(ctx, `SELECT COUNT(*) FROM realm_avatar_styles`).Scan(&selections); err != nil {
		t.Fatal(err)
	}
	if err := st.pool.QueryRow(ctx, `SELECT COUNT(*) FROM agent_avatar_profiles`).Scan(&profiles); err != nil {
		t.Fatal(err)
	}
	if styles != 1 || selections != 1 || profiles != 3 {
		t.Fatalf("avatar backfill counts = styles:%d selections:%d profiles:%d", styles, selections, profiles)
	}
	view, err := st.GetAvatar(ctx, Principal{Kind: PrincipalAgent,
		ID: "agent_memory_owner", AccountID: "acc_memory_trigger",
		RealmID: "realm_memory_trigger", AgentName: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if view.Profile.Status != avatardomain.StatusGenerationDue ||
		view.Profile.AutonomyPolicy != avatardomain.AutonomyAgentSelfManaged ||
		view.Active == nil || view.Active.Version != 0 {
		t.Fatalf("backfilled avatar = %#v", view)
	}
	if err := migrationTestDown(t, dsn, false); err != nil {
		t.Fatal(err)
	}
	assertMigrationTestVersion(t, dsn, 41)
	assertMigrationTestTable(t, st, "agent_avatar_profiles", false)
	assertMigrationTestTable(t, st, "avatar_style_packs", false)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	assertMigrationTestVersion(t, dsn, 50)
	if err := st.pool.QueryRow(ctx, `SELECT COUNT(*) FROM agent_avatar_profiles`).Scan(&profiles); err != nil {
		t.Fatal(err)
	}
	if profiles != 3 {
		t.Fatalf("avatar profiles after re-upgrade = %d, want 3", profiles)
	}
}

func TestAvatarLifecycleIsolationIdempotencyAndStylePropagationPostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}

	provisioned, err := st.ProvisionAccount(ctx, "avatar-store@witwave.ai", "avatar store", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = deleteAccountForIntegrationTest(context.Background(), st, provisioned.AccountID) }()
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate = %t / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "default")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "portrait-test")
	if err != nil {
		t.Fatal(err)
	}
	agentPrincipal := Principal{Kind: PrincipalAgent, ID: agent.ID,
		AccountID: provisioned.AccountID, RealmID: realm.ID, AgentName: agent.Name,
		AccountStatus: "active"}
	operator := Principal{Kind: PrincipalOperator, ID: provisioned.OperatorID,
		AccountID: provisioned.AccountID, AccountStatus: "active"}

	initial, err := st.GetAvatar(ctx, agentPrincipal)
	if err != nil {
		t.Fatal(err)
	}
	if initial.Profile.Status != avatardomain.StatusGenerationDue ||
		initial.Profile.AutonomyPolicy != avatardomain.AutonomyAgentSelfManaged ||
		initial.Profile.ProfileRevision != 1 || initial.Profile.FallbackSeed != agent.ID ||
		initial.Active == nil || initial.Active.ProposedBy.Kind != ActorSystem ||
		initial.Active.Version != 0 || !initial.Active.IsActive ||
		initial.Profile.ActiveVersion != 0 {
		t.Fatalf("initial avatar = %#v", initial)
	}
	again, err := st.GetAvatar(ctx, agentPrincipal)
	if err != nil {
		t.Fatal(err)
	}
	if again.Active == nil || again.Active.SVG != initial.Active.SVG ||
		again.Active.SVGSHA256 != initial.Active.SVGSHA256 || again.Active.ID != initial.Active.ID {
		t.Fatal("deterministic placeholder changed between reads")
	}
	checkpoint, err := st.GetSelfAvatarCheckpoint(ctx, agentPrincipal)
	if err != nil {
		t.Fatal(err)
	}
	if !checkpoint.Pending || checkpoint.Reason != "initial_avatar" ||
		checkpoint.ProfileRevision != 1 || checkpoint.StylePackVersion != 1 {
		t.Fatalf("initial avatar checkpoint = %#v", checkpoint)
	}
	style, err := st.GetRealmAvatarStyle(ctx, agentPrincipal, "")
	if err != nil {
		t.Fatal(err)
	}
	if style.StyleRevision != 1 || style.StylePack.ID != avatardomain.DefaultStylePackID ||
		style.StylePack.Version != 1 || len(style.StylePack.References) < 3 {
		t.Fatalf("initial style = %#v", style)
	}

	restricted := agentPrincipal
	restricted.AccessProfile = AccessProfileCuratorPreview
	if _, err := st.GetAvatar(ctx, restricted); !errors.Is(err, ErrAvatarForbidden) {
		t.Fatalf("restricted avatar read = %v", err)
	}
	if _, err := st.ProposeAvatar(ctx, restricted, ProposeAvatarInput{}); !errors.Is(err, ErrAvatarForbidden) {
		t.Fatalf("restricted avatar proposal = %v", err)
	}

	proposal := ProposeAvatarInput{
		ExpectedProfileRevision: 1,
		StylePackID:             style.StylePack.ID,
		StylePackVersion:        style.StylePack.Version,
		SubjectForm:             avatardomain.SubjectHuman,
		Description:             "A calm human teammate in the shared flat portrait style.",
		VisualSpec:              []byte(`{"identity":{"expression":"calm"}}`),
		SVG:                     style.StylePack.References[0].SVG,
		Provenance: AvatarClientProvenance{Runtime: "codex", Model: "gpt-5.6",
			Recipe: "avatar-initial", RecipeVersion: "1"},
		IdempotencyKey: "avatar-proposal-1",
	}
	proposed, err := st.ProposeAvatar(ctx, agentPrincipal, proposal)
	if err != nil {
		t.Fatal(err)
	}
	if proposed.Avatar.Profile.ProfileRevision != 2 ||
		proposed.Avatar.Profile.Status != avatardomain.StatusProposed ||
		proposed.Avatar.Proposed == nil || proposed.Avatar.Proposed.Version != 1 ||
		!proposed.Avatar.Proposed.IsProposed || proposed.Avatar.Proposed.WasActivated ||
		proposed.Avatar.Active == nil || proposed.Avatar.Active.Version != 0 {
		t.Fatalf("proposed avatar = %#v", proposed.Avatar)
	}
	replayedProposal, err := st.ProposeAvatar(ctx, agentPrincipal, proposal)
	if err != nil || !replayedProposal.Receipt.Replayed || replayedProposal.Receipt.ResultVersion != 1 {
		t.Fatalf("proposal replay = %#v / %v", replayedProposal.Receipt, err)
	}
	conflictingProposal := proposal
	conflictingProposal.Description = "Different semantics under the same retry key."
	if _, err := st.ProposeAvatar(ctx, agentPrincipal, conflictingProposal); !errors.Is(err, ErrAvatarIdempotencyConflict) {
		t.Fatalf("proposal idempotency conflict = %v", err)
	}
	staleProposal := proposal
	staleProposal.IdempotencyKey = "avatar-proposal-stale"
	if _, err := st.ProposeAvatar(ctx, agentPrincipal, staleProposal); !errors.Is(err, ErrAvatarConflict) {
		t.Fatalf("stale proposal = %v", err)
	}

	activation := ActivateAvatarInput{Version: 1, ExpectedProfileRevision: 2,
		IdempotencyKey: "avatar-activate-1"}
	active, err := st.ActivateAvatar(ctx, agentPrincipal, activation)
	if err != nil {
		t.Fatal(err)
	}
	if active.Avatar.Profile.ProfileRevision != 3 ||
		active.Avatar.Profile.ActiveVersion != 1 || active.Avatar.Proposed != nil ||
		active.Avatar.Active == nil || active.Avatar.Active.Version != 1 ||
		!active.Avatar.Active.IsActive || !active.Avatar.Active.WasActivated ||
		active.Avatar.Active.RollbackEligible || active.Avatar.Active.LastActivatedAt == nil {
		t.Fatalf("active avatar = %#v", active.Avatar)
	}
	checkpoint, err = st.GetSelfAvatarCheckpoint(ctx, agentPrincipal)
	if err != nil || checkpoint.Pending {
		t.Fatalf("active avatar checkpoint = %#v / %v", checkpoint, err)
	}
	replayedActivation, err := st.ActivateAvatar(ctx, agentPrincipal, activation)
	if err != nil || !replayedActivation.Receipt.Replayed {
		t.Fatalf("activation replay = %#v / %v", replayedActivation.Receipt, err)
	}
	// Exact retry keys preserve their original value-free receipt while reads
	// return the resource's current projection. A replay never rolls mutable
	// profile state back to the historical post-mutation view.
	replayedProposal, err = st.ProposeAvatar(ctx, agentPrincipal, proposal)
	if err != nil || !replayedProposal.Receipt.Replayed ||
		replayedProposal.Receipt.ResultRevision != 2 ||
		replayedProposal.Avatar.Profile.ProfileRevision != 3 ||
		replayedProposal.Avatar.Profile.Status != avatardomain.StatusActive {
		t.Fatalf("proposal replay after activation = %#v / %#v / %v",
			replayedProposal.Receipt, replayedProposal.Avatar.Profile, err)
	}

	policyResult, err := st.SetAvatarPolicy(ctx, operator, agent.ID, UpdateAvatarPolicyInput{
		Policy: avatardomain.AutonomyAgentProposes, ExpectedProfileRevision: 3,
		IdempotencyKey: "avatar-policy-proposes",
	})
	if err != nil {
		t.Fatal(err)
	}
	if policyResult.Avatar.Profile.ProfileRevision != 4 ||
		policyResult.Avatar.Profile.AutonomyPolicy != avatardomain.AutonomyAgentProposes {
		t.Fatalf("policy result = %#v", policyResult.Avatar.Profile)
	}

	evolution := proposal
	evolution.ExpectedProfileRevision = 4
	evolution.ParentVersion = 1
	evolution.SubjectForm = avatardomain.SubjectAnimal
	evolution.Description = "A fox teammate evolved from the active portrait while retaining the team grammar."
	evolution.SVG = style.StylePack.References[1].SVG
	evolution.IdempotencyKey = "avatar-evolution-2"
	// Operators may deliberately override same-style locked-layer and subject
	// continuity; the restriction applies only to self-authored evolution.
	evolvedProposal, err := st.ProposeAgentAvatar(ctx, operator, agent.ID, evolution)
	if err != nil {
		t.Fatal(err)
	}
	if evolvedProposal.Avatar.Proposed == nil || evolvedProposal.Avatar.Proposed.Version != 2 ||
		evolvedProposal.Avatar.Proposed.ParentVersion == nil || *evolvedProposal.Avatar.Proposed.ParentVersion != 1 {
		t.Fatalf("evolution proposal = %#v", evolvedProposal.Avatar.Proposed)
	}
	if _, err := st.ActivateAvatar(ctx, agentPrincipal, ActivateAvatarInput{Version: 2,
		ExpectedProfileRevision: 5, IdempotencyKey: "self-activation-denied"}); !errors.Is(err, ErrAvatarForbidden) {
		t.Fatalf("agent_proposes self activation = %v", err)
	}
	operatorActive, err := st.ActivateAgentAvatar(ctx, operator, agent.ID, ActivateAvatarInput{
		Version: 2, ExpectedProfileRevision: 5, IdempotencyKey: "operator-activate-2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if operatorActive.Avatar.Profile.ActiveVersion != 2 ||
		operatorActive.Avatar.Profile.SubjectForm != avatardomain.SubjectAnimal {
		t.Fatalf("operator activation = %#v", operatorActive.Avatar.Profile)
	}
	rolledBack, err := st.RollbackAgentAvatar(ctx, operator, agent.ID, RollbackAvatarInput{
		Version: 1, ExpectedProfileRevision: 6, IdempotencyKey: "operator-rollback-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rolledBack.Avatar.Profile.ActiveVersion != 1 ||
		rolledBack.Avatar.Profile.ProfileRevision != 7 {
		t.Fatalf("rollback = %#v", rolledBack.Avatar.Profile)
	}
	rolledForward, err := st.RollbackAgentAvatar(ctx, operator, agent.ID, RollbackAvatarInput{
		Version: 2, ExpectedProfileRevision: 7, IdempotencyKey: "operator-rollback-forward-2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rolledForward.Avatar.Profile.ActiveVersion != 2 ||
		rolledForward.Avatar.Profile.ProfileRevision != 8 {
		t.Fatalf("rollback to later historical activation = %#v", rolledForward.Avatar.Profile)
	}
	rolledBack, err = st.RollbackAgentAvatar(ctx, operator, agent.ID, RollbackAvatarInput{
		Version: 1, ExpectedProfileRevision: 8, IdempotencyKey: "operator-rollback-1-again",
	})
	if err != nil {
		t.Fatal(err)
	}
	history, err := st.GetAvatarHistory(ctx, agentPrincipal, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(history.Versions) != 2 {
		t.Fatalf("activation history versions = %d, want 2", len(history.Versions))
	}
	byVersion := map[int64]AvatarVersionSummary{}
	for _, version := range history.Versions {
		byVersion[version.Version] = version
	}
	versionOne, versionTwo := byVersion[1], byVersion[2]
	if !versionOne.IsActive || !versionOne.WasActivated || versionOne.RollbackEligible ||
		versionOne.LastActivatedAt == nil || versionOne.Rejected ||
		versionTwo.IsActive || !versionTwo.WasActivated || !versionTwo.RollbackEligible ||
		versionTwo.LastActivatedAt == nil || versionTwo.Rejected {
		t.Fatalf("projected lifecycle history = v1:%#v v2:%#v", versionOne, versionTwo)
	}
	firstHistoryPage, err := st.GetAvatarHistoryPage(ctx, agentPrincipal, AvatarHistoryOptions{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	secondHistoryPage, err := st.GetAvatarHistoryPage(ctx, agentPrincipal, AvatarHistoryOptions{
		Limit: 1, BeforeVersion: firstHistoryPage.NextBeforeVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(firstHistoryPage.Versions) != 1 || firstHistoryPage.Versions[0].Version != 2 || firstHistoryPage.NextBeforeVersion != 2 ||
		len(secondHistoryPage.Versions) != 1 || secondHistoryPage.Versions[0].Version != 1 || secondHistoryPage.NextBeforeVersion != 0 ||
		firstHistoryPage.Versions[0].Version == secondHistoryPage.Versions[0].Version {
		t.Fatalf("history pagination continuity = first:%#v second:%#v", firstHistoryPage, secondHistoryPage)
	}
	exactVersion, err := st.GetAvatarVersion(ctx, agentPrincipal, 2)
	if err != nil || exactVersion.Version != 2 || exactVersion.SVG == "" || len(exactVersion.VisualSpec) == 0 ||
		exactVersion.SVGSHA256 == "" || !exactVersion.WasActivated || !exactVersion.RollbackEligible {
		t.Fatalf("exact avatar version = %#v / %v", exactVersion, err)
	}
	if _, err := st.GetAvatarVersion(ctx, agentPrincipal, 0); !errors.Is(err, ErrAvatarInputInvalid) {
		t.Fatalf("version zero error = %v, want ErrAvatarInputInvalid", err)
	}
	if _, err := st.GetAvatarVersion(ctx, agentPrincipal, 999); !errors.Is(err, ErrAvatarVersionNotFound) {
		t.Fatalf("missing version error = %v, want ErrAvatarVersionNotFound", err)
	}

	selfManaged, err := st.SetAvatarPolicy(ctx, operator, agent.ID, UpdateAvatarPolicyInput{
		Policy: avatardomain.AutonomyAgentSelfManaged, ExpectedProfileRevision: 9,
		IdempotencyKey: "avatar-policy-self-managed",
	})
	if err != nil {
		t.Fatal(err)
	}

	staleStyleProposal := proposal
	staleStyleProposal.ExpectedProfileRevision = selfManaged.Avatar.Profile.ProfileRevision
	staleStyleProposal.ParentVersion = 1
	staleStyleProposal.SubjectForm = avatardomain.SubjectAnimal
	staleStyleProposal.Description = "An animal evolution awaiting review under the first team style version."
	staleStyleProposal.SVG = style.StylePack.References[1].SVG
	staleStyleProposal.IdempotencyKey = "avatar-stale-style-proposal"
	staleStyleResult, err := st.ProposeAgentAvatar(ctx, operator, agent.ID, staleStyleProposal)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReportAvatarGenerationFailure(ctx, agentPrincipal,
		AvatarGenerationFailureInput{ExpectedProfileRevision: staleStyleResult.Avatar.Profile.ProfileRevision,
			ReasonCode: "renderer_unavailable", IdempotencyKey: "failure-with-proposal"}); !errors.Is(err, ErrAvatarConflict) {
		t.Fatalf("generation failure with pending proposal = %v", err)
	}

	styleV2 := avatardomain.BuiltInFlatVectorStylePack()
	styleV2.Version = 2
	styleV2.Description = "The second immutable version of the shared flat-vector team portrait grammar."
	styleUpdate, err := st.SetRealmAvatarStyle(ctx, operator, realm.ID,
		CreateAvatarStyleVersionInput{ExpectedStyleRevision: 1, StylePack: styleV2,
			IdempotencyKey: "realm-avatar-style-2"})
	if err != nil {
		t.Fatal(err)
	}
	if styleUpdate.Style.StyleRevision != 2 || styleUpdate.Style.StylePack.Version != 2 {
		t.Fatalf("style update = %#v", styleUpdate.Style)
	}
	afterStyle, err := st.GetAvatar(ctx, agentPrincipal)
	if err != nil {
		t.Fatal(err)
	}
	if afterStyle.Profile.Style.Version != 2 ||
		afterStyle.Profile.Status != avatardomain.StatusEvolutionDue ||
		afterStyle.Profile.RetryAfter != nil || afterStyle.Profile.AttemptCount != 0 ||
		afterStyle.Profile.ProposedVersion != 0 || afterStyle.Proposed != nil ||
		afterStyle.Profile.SubjectForm != avatardomain.SubjectHuman ||
		afterStyle.Profile.ActiveVersion != 1 {
		t.Fatalf("profile after style update = %#v", afterStyle.Profile)
	}
	failure, err := st.ReportAvatarGenerationFailure(ctx, agentPrincipal,
		AvatarGenerationFailureInput{ExpectedProfileRevision: afterStyle.Profile.ProfileRevision,
			ReasonCode: "renderer_unavailable", IdempotencyKey: "avatar-failure-1"})
	if err != nil {
		t.Fatal(err)
	}
	if failure.Avatar.Profile.Status != avatardomain.StatusGenerationFailed ||
		failure.Avatar.Profile.AttemptCount != 1 || failure.Avatar.Profile.RetryAfter == nil ||
		!failure.Avatar.Profile.RetryAfter.After(time.Now().Add(30*time.Second)) {
		t.Fatalf("failure checkpoint = %#v", failure.Avatar.Profile)
	}
	checkpoint, err = st.GetSelfAvatarCheckpoint(ctx, agentPrincipal)
	if err != nil || checkpoint.Pending || checkpoint.RetryAfter == nil {
		t.Fatalf("future retry checkpoint = %#v / %v", checkpoint, err)
	}
	blockedRetryProposal := proposal
	blockedRetryProposal.ExpectedProfileRevision = failure.Avatar.Profile.ProfileRevision
	blockedRetryProposal.ParentVersion = failure.Avatar.Profile.ActiveVersion
	blockedRetryProposal.StylePackVersion = styleV2.Version
	blockedRetryProposal.Description = "A retry proposal that must wait for the server-stamped backoff."
	blockedRetryProposal.SVG = styleV2.References[0].SVG
	blockedRetryProposal.IdempotencyKey = "avatar-proposal-before-retry-due"
	if _, err := st.ProposeAvatar(ctx, agentPrincipal, blockedRetryProposal); !errors.Is(err, ErrAvatarConflict) {
		t.Fatalf("proposal before retry due = %v", err)
	}
	if _, err := st.ReportAvatarGenerationFailure(ctx, agentPrincipal,
		AvatarGenerationFailureInput{ExpectedProfileRevision: failure.Avatar.Profile.ProfileRevision,
			ReasonCode: "renderer_unavailable", IdempotencyKey: "failure-before-retry-due"}); !errors.Is(err, ErrAvatarConflict) {
		t.Fatalf("repeated failure before retry due = %v", err)
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE agent_avatar_profiles SET retry_after=clock_timestamp()-interval '1 second'
		 WHERE agent_id=$1`, agent.ID); err != nil {
		t.Fatal(err)
	}
	checkpoint, err = st.GetSelfAvatarCheckpoint(ctx, agentPrincipal)
	if err != nil || !checkpoint.Pending || checkpoint.Reason != "retry_due" {
		t.Fatalf("due retry checkpoint = %#v / %v", checkpoint, err)
	}
	retryEvolution := proposal
	retryEvolution.ExpectedProfileRevision = failure.Avatar.Profile.ProfileRevision
	retryEvolution.ParentVersion = failure.Avatar.Profile.ActiveVersion
	retryEvolution.StylePackVersion = styleV2.Version
	retryEvolution.Description = "A style-v2 evolution used to preserve the checkpoint after rejection."
	retryEvolution.SVG = styleV2.References[0].SVG
	retryEvolution.IdempotencyKey = "avatar-style-v2-retry-proposal"
	retryProposal, err := st.ProposeAvatar(ctx, agentPrincipal, retryEvolution)
	if err != nil {
		t.Fatal(err)
	}
	rejectedEvolution, err := st.RejectAgentAvatar(ctx, operator, agent.ID,
		RejectAvatarInput{Version: retryProposal.Avatar.Profile.ProposedVersion,
			ExpectedProfileRevision: retryProposal.Avatar.Profile.ProfileRevision,
			ReasonCode:              "operator_declined", IdempotencyKey: "reject-style-v2-evolution"})
	if err != nil {
		t.Fatal(err)
	}
	if rejectedEvolution.Avatar.Profile.Status != avatardomain.StatusEvolutionDue ||
		rejectedEvolution.Avatar.Profile.ActiveVersion != failure.Avatar.Profile.ActiveVersion ||
		rejectedEvolution.Avatar.Profile.Style.Version != styleV2.Version {
		t.Fatalf("rejected style evolution = %#v", rejectedEvolution.Avatar.Profile)
	}
	checkpoint, err = st.GetSelfAvatarCheckpoint(ctx, agentPrincipal)
	if err != nil || !checkpoint.Pending || checkpoint.Reason != "style_changed" {
		t.Fatalf("rejected style evolution checkpoint = %#v / %v", checkpoint, err)
	}
	acceptedEvolution := retryEvolution
	acceptedEvolution.ExpectedProfileRevision = rejectedEvolution.Avatar.Profile.ProfileRevision
	acceptedEvolution.IdempotencyKey = "avatar-style-v2-accepted-proposal"
	acceptedProposal, err := st.ProposeAvatar(ctx, agentPrincipal, acceptedEvolution)
	if err != nil {
		t.Fatal(err)
	}
	acceptedActive, err := st.ActivateAvatar(ctx, agentPrincipal, ActivateAvatarInput{
		Version:                 acceptedProposal.Avatar.Profile.ProposedVersion,
		ExpectedProfileRevision: acceptedProposal.Avatar.Profile.ProfileRevision,
		IdempotencyKey:          "avatar-style-v2-accepted-activation",
	})
	if err != nil {
		t.Fatal(err)
	}
	offStyleRollback, err := st.RollbackAgentAvatar(ctx, operator, agent.ID, RollbackAvatarInput{
		Version:                 1,
		ExpectedProfileRevision: acceptedActive.Avatar.Profile.ProfileRevision,
		IdempotencyKey:          "avatar-off-style-rollback",
	})
	if err != nil {
		t.Fatal(err)
	}
	if offStyleRollback.Avatar.Profile.Status != avatardomain.StatusEvolutionDue {
		t.Fatalf("off-style rollback profile = %#v", offStyleRollback.Avatar.Profile)
	}
	events, err := st.ListAccountEvents(ctx, provisioned.AccountID,
		provisioned.OperatorID, EventFilter{Verb: VerbAvatarRolledBack, Limit: 1})
	if err != nil || len(events.Events) != 1 {
		t.Fatalf("off-style rollback event = %#v / %v", events, err)
	}
	var rollbackMetadata map[string]string
	if err := json.Unmarshal(events.Events[0].Metadata, &rollbackMetadata); err != nil {
		t.Fatal(err)
	}
	if rollbackMetadata["status"] != string(avatardomain.StatusEvolutionDue) {
		t.Fatalf("off-style rollback event metadata = %#v", rollbackMetadata)
	}

	newAgent, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "style-v2-agent")
	if err != nil {
		t.Fatal(err)
	}
	newAgentView, err := st.GetAvatar(ctx, Principal{Kind: PrincipalAgent,
		ID: newAgent.ID, AccountID: provisioned.AccountID, RealmID: realm.ID,
		AgentName: newAgent.Name})
	if err != nil {
		t.Fatal(err)
	}
	if newAgentView.Profile.Style.Version != 2 {
		t.Fatalf("new agent inherited style version %d, want 2", newAgentView.Profile.Style.Version)
	}
	newAgentPrincipal := Principal{Kind: PrincipalAgent, ID: newAgent.ID,
		AccountID: provisioned.AccountID, RealmID: realm.ID, AgentName: newAgent.Name,
		AccountStatus: "active"}
	initialAnimal := proposal
	initialAnimal.ExpectedProfileRevision = 1
	initialAnimal.ParentVersion = 0
	initialAnimal.StylePackVersion = 2
	initialAnimal.SubjectForm = avatardomain.SubjectAnimal
	initialAnimal.Description = "An animal proposal used to verify rejection projection behavior."
	initialAnimal.SVG = styleV2.References[1].SVG
	initialAnimal.IdempotencyKey = "new-agent-animal-1"
	if _, err := st.ProposeAvatar(ctx, newAgentPrincipal, initialAnimal); err != nil {
		t.Fatal(err)
	}
	rejectedWithoutActive, err := st.RejectAgentAvatar(ctx, operator, newAgent.ID,
		RejectAvatarInput{Version: 1, ExpectedProfileRevision: 2,
			ReasonCode: "operator_declined", IdempotencyKey: "reject-new-agent-1"})
	if err != nil {
		t.Fatal(err)
	}
	if rejectedWithoutActive.Avatar.Profile.Status != avatardomain.StatusRejected ||
		rejectedWithoutActive.Avatar.Profile.SubjectForm != avatardomain.SubjectHuman ||
		rejectedWithoutActive.Avatar.Profile.ProposedVersion != 0 ||
		rejectedWithoutActive.Avatar.Proposed != nil {
		t.Fatalf("rejection without active avatar = %#v", rejectedWithoutActive.Avatar.Profile)
	}
	initialHuman := initialAnimal
	initialHuman.ExpectedProfileRevision = 3
	initialHuman.SubjectForm = avatardomain.SubjectHuman
	initialHuman.Description = "A human proposal that becomes the active projection."
	initialHuman.SVG = styleV2.References[0].SVG
	initialHuman.IdempotencyKey = "new-agent-human-2"
	initialHumanResult, err := st.ProposeAvatar(ctx, newAgentPrincipal, initialHuman)
	if err != nil {
		t.Fatal(err)
	}
	if initialHumanResult.Avatar.Proposed == nil || initialHumanResult.Avatar.Proposed.Version != 2 {
		t.Fatalf("second initial proposal = %#v", initialHumanResult.Avatar.Proposed)
	}
	if _, err := st.ActivateAvatar(ctx, newAgentPrincipal, ActivateAvatarInput{
		Version: 2, ExpectedProfileRevision: 4, IdempotencyKey: "activate-new-agent-2",
	}); err != nil {
		t.Fatal(err)
	}
	pendingAnimal := initialAnimal
	pendingAnimal.ExpectedProfileRevision = 5
	pendingAnimal.ParentVersion = 2
	pendingAnimal.IdempotencyKey = "new-agent-animal-3"
	if _, err := st.ProposeAgentAvatar(ctx, operator, newAgent.ID, pendingAnimal); err != nil {
		t.Fatal(err)
	}
	rejectedWithActive, err := st.RejectAgentAvatar(ctx, operator, newAgent.ID,
		RejectAvatarInput{Version: 3, ExpectedProfileRevision: 6,
			ReasonCode: "operator_declined", IdempotencyKey: "reject-new-agent-3"})
	if err != nil {
		t.Fatal(err)
	}
	if rejectedWithActive.Avatar.Profile.Status != avatardomain.StatusActive ||
		rejectedWithActive.Avatar.Profile.SubjectForm != avatardomain.SubjectHuman ||
		rejectedWithActive.Avatar.Profile.ActiveVersion != 2 ||
		rejectedWithActive.Avatar.Profile.ProposedVersion != 0 ||
		rejectedWithActive.Avatar.Proposed != nil {
		t.Fatalf("rejection with active avatar = %#v", rejectedWithActive.Avatar.Profile)
	}
	newAgentHistory, err := st.GetAvatarHistory(ctx, newAgentPrincipal, 10)
	if err != nil {
		t.Fatal(err)
	}
	newAgentByVersion := map[int64]AvatarVersionSummary{}
	for _, version := range newAgentHistory.Versions {
		newAgentByVersion[version.Version] = version
	}
	if rejected := newAgentByVersion[3]; !rejected.Rejected || rejected.RejectedAt == nil ||
		rejected.WasActivated || rejected.RollbackEligible || rejected.IsProposed {
		t.Fatalf("rejected lifecycle projection = %#v", rejected)
	}

	retryAgent, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "rejected-retry-agent")
	if err != nil {
		t.Fatal(err)
	}
	retryPrincipal := Principal{Kind: PrincipalAgent, ID: retryAgent.ID,
		AccountID: provisioned.AccountID, RealmID: realm.ID, AgentName: retryAgent.Name,
		AccountStatus: "active"}
	retryInitial := initialAnimal
	retryInitial.IdempotencyKey = "rejected-retry-proposal"
	retryInitialResult, err := st.ProposeAvatar(ctx, retryPrincipal, retryInitial)
	if err != nil {
		t.Fatal(err)
	}
	retryRejected, err := st.RejectAgentAvatar(ctx, operator, retryAgent.ID,
		RejectAvatarInput{Version: retryInitialResult.Avatar.Profile.ProposedVersion,
			ExpectedProfileRevision: retryInitialResult.Avatar.Profile.ProfileRevision,
			ReasonCode:              "operator_declined", IdempotencyKey: "rejected-retry-rejection"})
	if err != nil {
		t.Fatal(err)
	}
	if retryRejected.Avatar.Profile.Status != avatardomain.StatusRejected {
		t.Fatalf("retry fixture rejection = %#v", retryRejected.Avatar.Profile)
	}
	retryFailure, err := st.ReportAvatarGenerationFailure(ctx, retryPrincipal,
		AvatarGenerationFailureInput{ExpectedProfileRevision: retryRejected.Avatar.Profile.ProfileRevision,
			ReasonCode: "renderer_unavailable", IdempotencyKey: "rejected-retry-failure"})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err = st.GetSelfAvatarCheckpoint(ctx, retryPrincipal)
	if err != nil || checkpoint.Pending || retryFailure.Avatar.Profile.RetryAfter == nil {
		t.Fatalf("rejected failure backoff = %#v / %#v / %v", retryFailure.Avatar.Profile, checkpoint, err)
	}

	other, err := st.ProvisionAccount(ctx, "avatar-other@witwave.ai", "avatar other", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = deleteAccountForIntegrationTest(context.Background(), st, other.AccountID) }()
	if activated, err := st.ActivateAccount(ctx, other.AccountID); err != nil || !activated {
		t.Fatalf("activate other = %t / %v", activated, err)
	}
	otherOperator := Principal{Kind: PrincipalOperator, ID: other.OperatorID,
		AccountID: other.AccountID, AccountStatus: "active"}
	if _, err := st.GetAgentAvatar(ctx, otherOperator, agent.ID); !errors.Is(err, ErrAvatarNotFound) {
		t.Fatalf("cross-account operator avatar lookup = %v", err)
	}

	if err := st.DeleteAgent(ctx, provisioned.AccountID, realm.ID, agent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetAvatar(ctx, agentPrincipal); !errors.Is(err, ErrAvatarNotFound) {
		t.Fatalf("deleted avatar lookup = %v", err)
	}
}
