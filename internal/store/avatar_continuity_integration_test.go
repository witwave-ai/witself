package store

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	avatardomain "github.com/witwave-ai/witself/internal/avatar"
)

func TestAvatarSameStyleSelfContinuityOperatorOverrideAndStyleMigrationPostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	provisioned, err := st.ProvisionAccount(ctx, "avatar-continuity@witwave.ai", "avatar continuity", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = deleteAccountForIntegrationTest(context.Background(), st, provisioned.AccountID) }()
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate = %t / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "continuity")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "continuity-agent")
	if err != nil {
		t.Fatal(err)
	}
	agentPrincipal := Principal{Kind: PrincipalAgent, ID: agent.ID,
		AccountID: provisioned.AccountID, RealmID: realm.ID, AgentName: agent.Name,
		AccountStatus: "active"}
	operator := Principal{Kind: PrincipalOperator, ID: provisioned.OperatorID,
		AccountID: provisioned.AccountID, AccountStatus: "active"}
	style, err := st.GetRealmAvatarStyle(ctx, agentPrincipal, "")
	if err != nil {
		t.Fatal(err)
	}
	humanSVG, animalSVG := "", ""
	for _, reference := range style.StylePack.References {
		switch reference.SubjectForm {
		case avatardomain.SubjectHuman:
			humanSVG = reference.SVG
		case avatardomain.SubjectAnimal:
			animalSVG = reference.SVG
		}
	}
	if humanSVG == "" || animalSVG == "" {
		t.Fatal("built-in continuity fixtures are missing")
	}
	initial, err := st.ProposeAvatar(ctx, agentPrincipal, ProposeAvatarInput{
		ExpectedProfileRevision: 1, StylePackID: style.StylePack.ID,
		StylePackVersion: style.StylePack.Version, SubjectForm: avatardomain.SubjectHuman,
		Description: "Initial human continuity portrait.", VisualSpec: []byte(`{"phase":"initial"}`),
		SVG: humanSVG, IdempotencyKey: "continuity-initial",
	})
	if err != nil {
		t.Fatal(err)
	}
	active, err := st.ActivateAvatar(ctx, agentPrincipal, ActivateAvatarInput{
		Version:                 initial.Avatar.Profile.ProposedVersion,
		ExpectedProfileRevision: initial.Avatar.Profile.ProfileRevision,
		IdempotencyKey:          "continuity-initial-activate",
	})
	if err != nil {
		t.Fatal(err)
	}

	expressionSVG := strings.Replace(humanSVG,
		`<circle cx="214" cy="236" r="10" fill="#203247"></circle>`,
		`<circle cx="214" cy="236" r="12" fill="#203247"></circle>`, 1)
	validEvolution := ProposeAvatarInput{
		ExpectedProfileRevision: active.Avatar.Profile.ProfileRevision,
		ParentVersion:           active.Avatar.Profile.ActiveVersion,
		StylePackID:             style.StylePack.ID, StylePackVersion: style.StylePack.Version,
		SubjectForm: avatardomain.SubjectHuman,
		Description: "Expression-only same-style evolution.", VisualSpec: []byte(`{"phase":"expression"}`),
		SVG: expressionSVG, IdempotencyKey: "continuity-expression",
	}
	validResult, err := st.ProposeAvatar(ctx, agentPrincipal, validEvolution)
	if err != nil {
		t.Fatalf("valid same-style evolution: %v", err)
	}
	activeEvolution, err := st.ActivateAvatar(ctx, agentPrincipal, ActivateAvatarInput{
		Version:                 validResult.Avatar.Profile.ProposedVersion,
		ExpectedProfileRevision: validResult.Avatar.Profile.ProfileRevision,
		IdempotencyKey:          "continuity-expression-activate",
	})
	if err != nil {
		t.Fatal(err)
	}
	secondExpressionSVG := strings.Replace(expressionSVG,
		`<circle cx="214" cy="236" r="12" fill="#203247"></circle>`,
		`<circle cx="214" cy="236" r="14" fill="#203247"></circle>`, 1)
	pendingEvolution := validEvolution
	pendingEvolution.ExpectedProfileRevision = activeEvolution.Avatar.Profile.ProfileRevision
	pendingEvolution.ParentVersion = activeEvolution.Avatar.Profile.ActiveVersion
	pendingEvolution.SVG = secondExpressionSVG
	pendingEvolution.IdempotencyKey = "continuity-second-expression"
	pendingResult, err := st.ProposeAvatar(ctx, agentPrincipal, pendingEvolution)
	if err != nil {
		t.Fatal(err)
	}
	replacement := pendingEvolution
	replacement.ExpectedProfileRevision = pendingResult.Avatar.Profile.ProfileRevision
	replacement.IdempotencyKey = "continuity-pending-self-replacement"
	if _, err := st.ProposeAvatar(ctx, agentPrincipal, replacement); !errors.Is(err, ErrAvatarConflict) {
		t.Fatalf("self replacement while proposal pending = %v, want ErrAvatarConflict", err)
	}
	replacement.IdempotencyKey = "continuity-pending-operator-replacement"
	if _, err := st.ProposeAgentAvatar(ctx, operator, agent.ID, replacement); !errors.Is(err, ErrAvatarConflict) {
		t.Fatalf("operator replacement while proposal pending = %v, want ErrAvatarConflict", err)
	}
	if _, err := st.RollbackAgentAvatar(ctx, operator, agent.ID, RollbackAvatarInput{
		Version: 1, ExpectedProfileRevision: pendingResult.Avatar.Profile.ProfileRevision,
		IdempotencyKey: "continuity-pending-rollback",
	}); !errors.Is(err, ErrAvatarConflict) {
		t.Fatalf("rollback while proposal pending = %v, want ErrAvatarConflict", err)
	}
	rejected, err := st.RejectAgentAvatar(ctx, operator, agent.ID, RejectAvatarInput{
		Version:                 pendingResult.Avatar.Profile.ProposedVersion,
		ExpectedProfileRevision: pendingResult.Avatar.Profile.ProfileRevision,
		ReasonCode:              "continuity_test", IdempotencyKey: "continuity-expression-reject",
	})
	if err != nil {
		t.Fatal(err)
	}

	occluded := pendingEvolution
	occluded.ExpectedProfileRevision = rejected.Avatar.Profile.ProfileRevision
	occluded.SVG = strings.Replace(expressionSVG,
		`<g id="experience" data-layer="experience"></g>`,
		`<g id="experience" data-layer="experience"><circle cx="256" cy="230" r="136" fill="#F7FAFC"></circle></g>`, 1)
	occluded.IdempotencyKey = "continuity-identity-occlusion"
	if _, err := st.ProposeAvatar(ctx, agentPrincipal, occluded); !errors.Is(err, ErrAvatarInputInvalid) ||
		!strings.Contains(err.Error(), "perceptual continuity") {
		t.Fatalf("same-style identity occlusion = %v, want perceptual ErrAvatarInputInvalid", err)
	}

	lockedChanged := pendingEvolution
	lockedChanged.ExpectedProfileRevision = rejected.Avatar.Profile.ProfileRevision
	lockedChanged.SVG = strings.Replace(humanSVG, `r="220" fill="#DCEAF5"`, `r="210" fill="#DCEAF5"`, 1)
	lockedChanged.IdempotencyKey = "continuity-locked-change"
	if _, err := st.ProposeAvatar(ctx, agentPrincipal, lockedChanged); !errors.Is(err, ErrAvatarInputInvalid) {
		t.Fatalf("same-style locked-layer change = %v, want ErrAvatarInputInvalid", err)
	}
	subjectChanged := lockedChanged
	subjectChanged.SubjectForm = avatardomain.SubjectAnimal
	subjectChanged.SVG = animalSVG
	subjectChanged.IdempotencyKey = "continuity-subject-change"
	if _, err := st.ProposeAvatar(ctx, agentPrincipal, subjectChanged); !errors.Is(err, ErrAvatarInputInvalid) {
		t.Fatalf("same-style subject change = %v, want ErrAvatarInputInvalid", err)
	}

	operatorOverride, err := st.ProposeAgentAvatar(ctx, operator, agent.ID, subjectChanged)
	if err != nil {
		t.Fatalf("operator continuity override: %v", err)
	}
	if operatorOverride.Avatar.Proposed == nil ||
		operatorOverride.Avatar.Proposed.SubjectForm != avatardomain.SubjectAnimal {
		t.Fatalf("operator override = %#v", operatorOverride.Avatar.Proposed)
	}

	styleV2 := avatardomain.BuiltInFlatVectorStylePack()
	styleV2.Version = 2
	styleV2.Description = "A second style version used to verify explicit style migration."
	styleUpdate, err := st.SetRealmAvatarStyle(ctx, operator, realm.ID,
		CreateAvatarStyleVersionInput{ExpectedStyleRevision: 1, StylePack: styleV2,
			IdempotencyKey: "continuity-style-v2"})
	if err != nil {
		t.Fatal(err)
	}
	afterStyle, err := st.GetAvatar(ctx, agentPrincipal)
	if err != nil {
		t.Fatal(err)
	}
	migration := subjectChanged
	migration.ExpectedProfileRevision = afterStyle.Profile.ProfileRevision
	migration.StylePackVersion = styleUpdate.Style.StylePack.Version
	migration.IdempotencyKey = "continuity-style-migration"
	if _, err := st.ProposeAvatar(ctx, agentPrincipal, migration); err != nil {
		t.Fatalf("operator-selected style migration should be continuity-exempt: %v", err)
	}
}
