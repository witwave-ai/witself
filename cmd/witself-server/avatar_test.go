package main

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	avatardomain "github.com/witwave-ai/witself/internal/avatar"
	"github.com/witwave-ai/witself/internal/server"
	"github.com/witwave-ai/witself/internal/store"
)

func TestConfigureAvatarRegistersCompleteProductionSurface(t *testing.T) {
	var cfg server.Config
	configureAvatar(&cfg, nil)
	callbacks := map[string]any{
		"checkpoint": cfg.GetSelfAvatarCheckpoint,
		"self show":  cfg.GetSelfAvatar, "self history": cfg.GetSelfAvatarHistory,
		"self version": cfg.GetSelfAvatarVersion,
		"self style":   cfg.GetSelfAvatarStyle, "self propose": cfg.ProposeSelfAvatar,
		"self activate": cfg.ActivateSelfAvatar, "self rollback": cfg.RollbackSelfAvatar,
		"self reset": cfg.ResetSelfAvatar, "self failure": cfg.ReportSelfAvatarGenerationFailure,
		"operator show": cfg.GetAgentAvatar, "operator history": cfg.GetAgentAvatarHistory,
		"operator version":  cfg.GetAgentAvatarVersion,
		"operator propose":  cfg.ProposeAgentAvatar,
		"operator activate": cfg.ActivateAgentAvatar, "operator reject": cfg.RejectAgentAvatar,
		"operator rollback": cfg.RollbackAgentAvatar, "operator reset": cfg.ResetAgentAvatar,
		"operator policy": cfg.UpdateAgentAvatarPolicy,
		"operator style":  cfg.GetRealmAvatarStyle, "operator style version": cfg.CreateRealmAvatarStyleVersion,
	}
	for name, callback := range callbacks {
		if callback == nil {
			t.Errorf("%s callback is nil", name)
		}
	}
}

func TestAvatarCheckpointConversionPreservesLineageGeneration(t *testing.T) {
	checkpoint := toServerSelfAvatarCheckpoint(store.SelfAvatarCheckpoint{
		Pending: true, Status: "generation_due", Reason: "avatar_reset",
		ProfileRevision: 8, LineageGeneration: 3, StylePackID: "witself-flat-portrait", StylePackVersion: 1,
	})
	if checkpoint == nil || checkpoint.ProfileRevision != 8 || checkpoint.LineageGeneration != 3 || checkpoint.Reason != "avatar_reset" {
		t.Fatalf("converted checkpoint = %#v", checkpoint)
	}
}

func TestAvatarStoreToServerConversionPreservesCanonicalFields(t *testing.T) {
	now := time.Date(2026, time.July, 17, 12, 0, 0, 0, time.UTC)
	retry := now.Add(time.Minute)
	parent := int64(1)
	style := avatardomain.StylePackRef{RealmID: "realm_one", StylePackID: avatardomain.DefaultStylePackID, Version: 1}
	view := store.AvatarView{
		Profile: store.AvatarProfile{
			AccountID: "acct_one", RealmID: "realm_one", AgentID: "agent_one",
			SubjectForm: avatardomain.SubjectAnimal, AutonomyPolicy: avatardomain.AutonomyAgentSelfManaged,
			Status: avatardomain.StatusProposed, Style: style, LineageGeneration: 3, ProfileRevision: 4,
			LatestVersion: 2, ActiveVersion: 1, ProposedVersion: 2, AttemptCount: 3,
			RetryAfter: &retry, FallbackSeed: "agent_one", FailureCode: "renderer_unavailable",
			CreatedAt: now, UpdatedAt: now,
		},
		Active: &store.AvatarVersion{
			ID: "avver_active", AccountID: "acct_one", RealmID: "realm_one", AgentID: "agent_one",
			Version: 1, LineageGeneration: 2, SubjectForm: avatardomain.SubjectHuman, Description: "active",
			VisualSpec: json.RawMessage(`{"form":"human"}`), SVG: "<svg></svg>", SVGSHA256: "active-hash",
			RendererProfile: avatardomain.RendererProfilePerceptualV1,
			Style:           style, ProposedBy: store.AvatarActor{Kind: "agent", ID: "agent_one", Name: "One"}, ProposedAt: now,
			IsActive: true, WasActivated: true, LastActivatedAt: &now,
		},
		Proposed: &store.AvatarVersion{
			ID: "avver_proposed", AccountID: "acct_one", RealmID: "realm_one", AgentID: "agent_one",
			Version: 2, ParentVersion: &parent, LineageGeneration: 3, SubjectForm: avatardomain.SubjectAnimal,
			Description: "proposed", VisualSpec: json.RawMessage(`{"form":"animal"}`),
			SVG: "<svg></svg>", SVGSHA256: "proposed-hash", RendererProfile: avatardomain.RendererProfilePerceptualV1, Style: style,
			Provenance: store.AvatarClientProvenance{Runtime: "codex", Model: "gpt"},
			ProposedBy: store.AvatarActor{Kind: "agent", ID: "agent_one", Name: "One"}, ProposedAt: now,
			IsProposed: true,
		},
	}

	got := toServerAvatarView(view)
	if got.Profile.AgentID != view.Profile.AgentID || got.Profile.ProfileRevision != 4 || got.Profile.LineageGeneration != 3 ||
		got.Profile.RetryAfter == nil || !got.Profile.RetryAfter.Equal(retry) ||
		got.Active == nil || got.Active.SubjectForm != avatardomain.SubjectHuman || got.Active.LineageGeneration != 2 || !got.Active.IsActive ||
		!got.Active.WasActivated || got.Active.LastActivatedAt == nil || !got.Active.LastActivatedAt.Equal(now) ||
		got.Proposed == nil || got.Proposed.ParentVersion == nil || *got.Proposed.ParentVersion != parent || got.Proposed.LineageGeneration != 3 ||
		got.Active.RendererProfile != avatardomain.RendererProfilePerceptualV1 ||
		got.Proposed.Provenance.Runtime != "codex" || got.Proposed.RendererProfile != avatardomain.RendererProfilePerceptualV1 || !got.Proposed.IsProposed {
		t.Fatalf("converted avatar = %#v", got)
	}
	rejectedAt := now.Add(time.Minute)
	historyVersion := toServerAvatarVersion(store.AvatarVersion{
		RollbackEligible: true, Rejected: true, RejectedAt: &rejectedAt,
	})
	if !historyVersion.RollbackEligible || !historyVersion.Rejected || historyVersion.RejectedAt == nil || !historyVersion.RejectedAt.Equal(rejectedAt) {
		t.Fatalf("converted lifecycle projection = %#v", historyVersion)
	}
	summary := toServerAvatarVersionSummary(store.AvatarVersionSummary{
		ID: "avver_history", AgentID: "agent_one", Version: 2,
		LineageGeneration: 3, SubjectForm: avatardomain.SubjectAnimal, SVGSHA256: "summary-hash", Style: style,
		RendererProfile: avatardomain.RendererProfilePerceptualV1,
		ProposedBy:      store.AvatarActor{Kind: "agent", ID: "agent_one"}, ProposedAt: now,
		WasActivated: true, RollbackEligible: true, LastActivatedAt: &now,
	})
	if summary.ID != "avver_history" || summary.AgentID != "agent_one" || summary.LineageGeneration != 3 || summary.SVGSHA256 != "summary-hash" ||
		summary.RendererProfile != avatardomain.RendererProfilePerceptualV1 ||
		!summary.WasActivated || !summary.RollbackEligible || summary.LastActivatedAt == nil {
		t.Fatalf("converted history summary = %#v", summary)
	}
	receipt := toServerAvatarReceipt(store.AvatarMutationReceipt{
		Operation: "reset", ResultRevision: 5, ResultLineageGeneration: 4,
	})
	if receipt.ResultLineageGeneration != 4 {
		t.Fatalf("converted receipt = %#v", receipt)
	}
}

func TestMapAvatarErrorUsesStableHTTPContract(t *testing.T) {
	tests := []struct {
		input error
		want  error
	}{
		{store.ErrAvatarInputInvalid, server.ErrBadInput},
		{store.ErrAvatarForbidden, server.ErrForbidden},
		{store.ErrAvatarNotFound, server.ErrNotFound},
		{store.ErrAvatarVersionNotFound, server.ErrNotFound},
		{store.ErrAvatarStyleNotFound, server.ErrNotFound},
		{store.ErrAvatarIdempotencyConflict, server.ErrIdempotencyConflict},
		{store.ErrAvatarPayloadQuotaExceeded, server.ErrAvatarPayloadQuotaExceeded},
		{store.ErrAvatarPayloadCompactionDisabled, server.ErrAvatarPayloadCompactionDisabled},
		{store.ErrAvatarConflict, server.ErrConflict},
	}
	for _, test := range tests {
		if got := mapAvatarError(test.input); !errors.Is(got, test.want) {
			t.Errorf("mapAvatarError(%v) = %v, want %v", test.input, got, test.want)
		}
	}
}
