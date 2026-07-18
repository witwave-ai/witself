package store

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	avatardomain "github.com/witwave-ai/witself/internal/avatar"
)

func TestDeterministicAvatarPlaceholderUsesSelectedCustomStyle(t *testing.T) {
	pack := avatardomain.BuiltInFlatVectorStylePack()
	pack.ID = "operator-flat"
	pack.Name = "Operator Flat"
	style := avatardomain.StylePackRef{
		RealmID: "realm_default", StylePackID: pack.ID, Version: pack.Version,
	}
	placeholder, err := deterministicAvatarPlaceholder(avatarTarget{
		accountID: "acc_test", realmID: style.RealmID, agentID: "agent_test",
	}, "Juniper", style, pack, 1, time.Unix(1, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if placeholder.Style != style || placeholder.SubjectForm != avatardomain.SubjectHuman {
		t.Fatalf("placeholder identity = %#v", placeholder)
	}
	if err := avatardomain.ValidateSVGForStylePack([]byte(placeholder.SVG), pack); err != nil {
		t.Fatalf("placeholder does not satisfy selected custom style: %v", err)
	}
	var spec map[string]any
	if err := json.Unmarshal(placeholder.VisualSpec, &spec); err != nil {
		t.Fatal(err)
	}
	if spec["source"] != "style_reference" || spec["style_reference_id"] != pack.References[0].ID {
		t.Fatalf("placeholder provenance spec = %#v", spec)
	}
}

func TestCustomStylePlaceholderIDsRemainAgentScoped(t *testing.T) {
	pack := avatardomain.BuiltInFlatVectorStylePack()
	pack.ID = "operator-flat"
	pack.Name = "Operator Flat"
	style := avatardomain.StylePackRef{
		RealmID: "realm_default", StylePackID: pack.ID, Version: pack.Version,
	}
	createdAt := time.Unix(1, 0).UTC()
	first, err := deterministicAvatarPlaceholder(avatarTarget{
		accountID: "acc_test", realmID: style.RealmID, agentID: "agent_one",
	}, "Shared Name", style, pack, 1, createdAt)
	if err != nil {
		t.Fatal(err)
	}
	second, err := deterministicAvatarPlaceholder(avatarTarget{
		accountID: "acc_test", realmID: style.RealmID, agentID: "agent_two",
	}, "Shared Name", style, pack, 1, createdAt)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := deterministicAvatarPlaceholder(avatarTarget{
		accountID: "acc_test", realmID: style.RealmID, agentID: "agent_one",
	}, "Shared Name", style, pack, 1, createdAt)
	if err != nil {
		t.Fatal(err)
	}
	if first.SVG != second.SVG || first.SVGSHA256 != second.SVGSHA256 {
		t.Fatal("custom neutral placeholder artwork should share a content hash")
	}
	if first.ID == second.ID || first.ID != repeated.ID {
		t.Fatalf("placeholder IDs are not stable and agent-scoped: first=%q second=%q repeated=%q", first.ID, second.ID, repeated.ID)
	}
}

func TestDeterministicAvatarPlaceholderFallsBackFromLegacyInvalidNameOnly(t *testing.T) {
	pack := avatardomain.BuiltInFlatVectorStylePack()
	style := avatardomain.StylePackRef{
		RealmID: "realm_default", StylePackID: pack.ID, Version: pack.Version,
	}
	target := avatarTarget{accountID: "acc_test", realmID: style.RealmID, agentID: "agent_test"}
	createdAt := time.Unix(1, 0).UTC()
	placeholder, err := deterministicAvatarPlaceholder(target, "legacy\nname", style, pack, 1, createdAt)
	if err != nil {
		t.Fatal(err)
	}
	want, err := deterministicAvatarPlaceholder(target, target.agentID, style, pack, 1, createdAt)
	if err != nil {
		t.Fatal(err)
	}
	if placeholder.SVG != want.SVG || placeholder.SVGSHA256 != want.SVGSHA256 {
		t.Fatal("legacy-name fallback was not derived from the stable agent ID")
	}

	invalidPack := pack
	invalidPack.References = nil
	if _, err := deterministicAvatarPlaceholder(target, "legacy\nname", style, invalidPack, 1, createdAt); !errors.Is(err, avatardomain.ErrInvalidStylePack) {
		t.Fatalf("invalid style error = %v, want ErrInvalidStylePack", err)
	}
}

func TestDeterministicAvatarPlaceholderIdentityIncludesLineage(t *testing.T) {
	pack := avatardomain.BuiltInFlatVectorStylePack()
	style := avatardomain.StylePackRef{
		RealmID: "realm_default", StylePackID: pack.ID, Version: pack.Version,
	}
	target := avatarTarget{accountID: "acc_test", realmID: style.RealmID, agentID: "agent_test"}
	first, err := deterministicAvatarPlaceholder(target, "Juniper", style, pack, 1, time.Unix(1, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	reset, err := deterministicAvatarPlaceholder(target, "Juniper", style, pack, 2, time.Unix(2, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == reset.ID || first.LineageGeneration != 1 || reset.LineageGeneration != 2 {
		t.Fatalf("placeholder lineage identity did not change: first=%#v reset=%#v", first, reset)
	}
	if first.SVG != reset.SVG || first.SVGSHA256 != reset.SVGSHA256 {
		t.Fatal("lineage changed deterministic placeholder artwork")
	}
	if !reset.ProposedAt.After(first.ProposedAt) {
		t.Fatalf("reset placeholder timestamp = %v, want after %v", reset.ProposedAt, first.ProposedAt)
	}
}

func TestClassifySelfAvatarCheckpointHonorsAutonomy(t *testing.T) {
	tests := []struct {
		name     string
		status   avatardomain.Status
		policy   avatardomain.AutonomyPolicy
		retryDue bool
		pending  bool
		reason   string
	}{
		{"initial self managed", avatardomain.StatusGenerationDue, avatardomain.AutonomyAgentSelfManaged, false, true, "initial_avatar"},
		{"initial proposes", avatardomain.StatusGenerationDue, avatardomain.AutonomyAgentProposes, false, true, "initial_avatar"},
		{"initial operator only", avatardomain.StatusGenerationDue, avatardomain.AutonomyOperatorOnly, false, false, "awaiting_operator"},
		{"evolution operator only", avatardomain.StatusEvolutionDue, avatardomain.AutonomyOperatorOnly, false, false, "awaiting_operator"},
		{"rejected operator only", avatardomain.StatusRejected, avatardomain.AutonomyOperatorOnly, false, false, "awaiting_operator"},
		{"future retry", avatardomain.StatusGenerationFailed, avatardomain.AutonomyAgentSelfManaged, false, false, ""},
		{"due retry", avatardomain.StatusGenerationFailed, avatardomain.AutonomyAgentSelfManaged, true, true, "retry_due"},
		{"due retry operator only", avatardomain.StatusGenerationFailed, avatardomain.AutonomyOperatorOnly, true, false, "awaiting_operator"},
		{"proposal self managed", avatardomain.StatusProposed, avatardomain.AutonomyAgentSelfManaged, false, true, "activation_due"},
		{"proposal awaiting operator", avatardomain.StatusProposed, avatardomain.AutonomyAgentProposes, false, false, "awaiting_operator"},
		{"active", avatardomain.StatusActive, avatardomain.AutonomyAgentSelfManaged, false, false, ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pending, reason := classifySelfAvatarCheckpoint(test.status, test.policy, test.retryDue, 1)
			if pending != test.pending || reason != test.reason {
				t.Fatalf("classifySelfAvatarCheckpoint(%q, %q, %t) = (%t, %q), want (%t, %q)",
					test.status, test.policy, test.retryDue, pending, reason, test.pending, test.reason)
			}
		})
	}
	pending, reason := classifySelfAvatarCheckpoint(avatardomain.StatusGenerationDue,
		avatardomain.AutonomyAgentSelfManaged, false, 2)
	if !pending || reason != "avatar_reset" {
		t.Fatalf("reset checkpoint = (%t, %q), want (true, avatar_reset)", pending, reason)
	}
}
