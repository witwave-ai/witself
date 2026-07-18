package main

import (
	"context"
	"errors"

	"github.com/witwave-ai/witself/internal/server"
	"github.com/witwave-ai/witself/internal/store"
)

// configureAvatar connects the transport-neutral avatar contract to the
// Postgres implementation. Generation itself deliberately remains in the
// active AI client; the service owns policy, validation, versioning, and
// durable lifecycle state.
func configureAvatar(cfg *server.Config, st *store.Store) {
	cfg.GetSelfAvatarCheckpoint = func(ctx context.Context, p server.DomainPrincipal) (*server.SelfAvatarCheckpoint, error) {
		checkpoint, err := st.GetSelfAvatarCheckpoint(ctx, toStorePrincipal(p))
		if err != nil {
			return nil, mapAvatarError(err)
		}
		return toServerSelfAvatarCheckpoint(checkpoint), nil
	}
	cfg.GetSelfAvatar = func(ctx context.Context, p server.DomainPrincipal) (server.AvatarView, error) {
		view, err := st.GetAvatar(ctx, toStorePrincipal(p))
		return toServerAvatarView(view), mapAvatarError(err)
	}
	cfg.GetSelfAvatarHistory = func(ctx context.Context, p server.DomainPrincipal, opts server.AvatarHistoryOptions) (server.AvatarHistoryPage, error) {
		page, err := st.GetAvatarHistoryPage(ctx, toStorePrincipal(p), store.AvatarHistoryOptions{
			Limit: opts.Limit, BeforeVersion: opts.BeforeVersion,
		})
		if err != nil {
			return server.AvatarHistoryPage{}, mapAvatarError(err)
		}
		versions := make([]server.AvatarVersionSummary, len(page.Versions))
		for i, version := range page.Versions {
			versions[i] = toServerAvatarVersionSummary(version)
		}
		return server.AvatarHistoryPage{Versions: versions, NextBeforeVersion: page.NextBeforeVersion}, nil
	}
	cfg.GetSelfAvatarVersion = func(ctx context.Context, p server.DomainPrincipal, version int64) (server.AvatarVersion, error) {
		avatarVersion, err := st.GetAvatarVersion(ctx, toStorePrincipal(p), version)
		return toServerAvatarVersion(avatarVersion), mapAvatarError(err)
	}
	cfg.GetSelfAvatarStyle = func(ctx context.Context, p server.DomainPrincipal) (server.AvatarStyleView, error) {
		style, err := st.GetRealmAvatarStyle(ctx, toStorePrincipal(p), "")
		return toServerAvatarStyle(style), mapAvatarError(err)
	}
	cfg.ProposeSelfAvatar = func(ctx context.Context, p server.DomainPrincipal, in server.ProposeAvatarRequest) (server.AvatarMutationResult, error) {
		result, err := st.ProposeAvatar(ctx, toStorePrincipal(p), toStoreAvatarProposal(in))
		return toServerAvatarMutation(result), mapAvatarError(err)
	}
	cfg.ActivateSelfAvatar = func(ctx context.Context, p server.DomainPrincipal, in server.ActivateAvatarRequest) (server.AvatarMutationResult, error) {
		result, err := st.ActivateAvatar(ctx, toStorePrincipal(p), store.ActivateAvatarInput{
			Version: in.Version, ExpectedProfileRevision: in.ExpectedProfileRevision, IdempotencyKey: in.IdempotencyKey,
		})
		return toServerAvatarMutation(result), mapAvatarError(err)
	}
	cfg.RollbackSelfAvatar = func(ctx context.Context, p server.DomainPrincipal, in server.RollbackAvatarRequest) (server.AvatarMutationResult, error) {
		result, err := st.RollbackAvatar(ctx, toStorePrincipal(p), store.RollbackAvatarInput{
			Version: in.Version, ExpectedProfileRevision: in.ExpectedProfileRevision, IdempotencyKey: in.IdempotencyKey,
		})
		return toServerAvatarMutation(result), mapAvatarError(err)
	}
	cfg.ResetSelfAvatar = func(ctx context.Context, p server.DomainPrincipal, in server.ResetAvatarRequest) (server.AvatarMutationResult, error) {
		result, err := st.ResetAvatar(ctx, toStorePrincipal(p), store.ResetAvatarInput{
			ExpectedProfileRevision: in.ExpectedProfileRevision, ReasonCode: in.ReasonCode, IdempotencyKey: in.IdempotencyKey,
		})
		return toServerAvatarMutation(result), mapAvatarError(err)
	}
	cfg.ReportSelfAvatarGenerationFailure = func(ctx context.Context, p server.DomainPrincipal, in server.AvatarGenerationFailureRequest) (server.AvatarMutationResult, error) {
		result, err := st.ReportAvatarGenerationFailure(ctx, toStorePrincipal(p), store.AvatarGenerationFailureInput{
			ExpectedProfileRevision: in.ExpectedProfileRevision, ReasonCode: in.ReasonCode, IdempotencyKey: in.IdempotencyKey,
		})
		return toServerAvatarMutation(result), mapAvatarError(err)
	}

	operator := func(accountID, operatorID string) store.Principal {
		return store.Principal{Kind: store.PrincipalOperator, AccountID: accountID, ID: operatorID}
	}
	cfg.GetAgentAvatar = func(ctx context.Context, accountID, operatorID, agentID string) (server.AvatarView, error) {
		view, err := st.GetAgentAvatar(ctx, operator(accountID, operatorID), agentID)
		return toServerAvatarView(view), mapAvatarError(err)
	}
	cfg.GetAgentAvatarHistory = func(ctx context.Context, accountID, operatorID, agentID string, opts server.AvatarHistoryOptions) (server.AvatarHistoryPage, error) {
		page, err := st.GetAgentAvatarHistoryPage(ctx, operator(accountID, operatorID), agentID, store.AvatarHistoryOptions{
			Limit: opts.Limit, BeforeVersion: opts.BeforeVersion,
		})
		if err != nil {
			return server.AvatarHistoryPage{}, mapAvatarError(err)
		}
		versions := make([]server.AvatarVersionSummary, len(page.Versions))
		for i, version := range page.Versions {
			versions[i] = toServerAvatarVersionSummary(version)
		}
		return server.AvatarHistoryPage{Versions: versions, NextBeforeVersion: page.NextBeforeVersion}, nil
	}
	cfg.GetAgentAvatarVersion = func(ctx context.Context, accountID, operatorID, agentID string, version int64) (server.AvatarVersion, error) {
		avatarVersion, err := st.GetAgentAvatarVersion(ctx, operator(accountID, operatorID), agentID, version)
		return toServerAvatarVersion(avatarVersion), mapAvatarError(err)
	}
	cfg.ProposeAgentAvatar = func(ctx context.Context, accountID, operatorID, agentID string, in server.ProposeAvatarRequest) (server.AvatarMutationResult, error) {
		result, err := st.ProposeAgentAvatar(ctx, operator(accountID, operatorID), agentID, toStoreAvatarProposal(in))
		return toServerAvatarMutation(result), mapAvatarError(err)
	}
	cfg.ActivateAgentAvatar = func(ctx context.Context, accountID, operatorID, agentID string, in server.ActivateAvatarRequest) (server.AvatarMutationResult, error) {
		result, err := st.ActivateAgentAvatar(ctx, operator(accountID, operatorID), agentID, store.ActivateAvatarInput{
			Version: in.Version, ExpectedProfileRevision: in.ExpectedProfileRevision, IdempotencyKey: in.IdempotencyKey,
		})
		return toServerAvatarMutation(result), mapAvatarError(err)
	}
	cfg.RejectAgentAvatar = func(ctx context.Context, accountID, operatorID, agentID string, in server.RejectAvatarRequest) (server.AvatarMutationResult, error) {
		result, err := st.RejectAgentAvatar(ctx, operator(accountID, operatorID), agentID, store.RejectAvatarInput{
			Version: in.Version, ExpectedProfileRevision: in.ExpectedProfileRevision,
			ReasonCode: in.ReasonCode, IdempotencyKey: in.IdempotencyKey,
		})
		return toServerAvatarMutation(result), mapAvatarError(err)
	}
	cfg.RollbackAgentAvatar = func(ctx context.Context, accountID, operatorID, agentID string, in server.RollbackAvatarRequest) (server.AvatarMutationResult, error) {
		result, err := st.RollbackAgentAvatar(ctx, operator(accountID, operatorID), agentID, store.RollbackAvatarInput{
			Version: in.Version, ExpectedProfileRevision: in.ExpectedProfileRevision, IdempotencyKey: in.IdempotencyKey,
		})
		return toServerAvatarMutation(result), mapAvatarError(err)
	}
	cfg.ResetAgentAvatar = func(ctx context.Context, accountID, operatorID, agentID string, in server.ResetAvatarRequest) (server.AvatarMutationResult, error) {
		result, err := st.ResetAgentAvatar(ctx, operator(accountID, operatorID), agentID, store.ResetAvatarInput{
			ExpectedProfileRevision: in.ExpectedProfileRevision, ReasonCode: in.ReasonCode, IdempotencyKey: in.IdempotencyKey,
		})
		return toServerAvatarMutation(result), mapAvatarError(err)
	}
	cfg.UpdateAgentAvatarPolicy = func(ctx context.Context, accountID, operatorID, agentID string, in server.UpdateAvatarPolicyRequest) (server.AvatarMutationResult, error) {
		result, err := st.SetAvatarPolicy(ctx, operator(accountID, operatorID), agentID, store.UpdateAvatarPolicyInput{
			Policy: in.Policy, ExpectedProfileRevision: in.ExpectedProfileRevision, IdempotencyKey: in.IdempotencyKey,
		})
		return toServerAvatarMutation(result), mapAvatarError(err)
	}
	cfg.GetRealmAvatarStyle = func(ctx context.Context, accountID, operatorID, realmID string) (server.AvatarStyleView, error) {
		style, err := st.GetRealmAvatarStyle(ctx, operator(accountID, operatorID), realmID)
		return toServerAvatarStyle(style), mapAvatarError(err)
	}
	cfg.CreateRealmAvatarStyleVersion = func(ctx context.Context, accountID, operatorID, realmID string, in server.CreateAvatarStyleVersionRequest) (server.AvatarStyleMutationResult, error) {
		result, err := st.SetRealmAvatarStyle(ctx, operator(accountID, operatorID), realmID, store.CreateAvatarStyleVersionInput{
			ExpectedStyleRevision: in.ExpectedStyleRevision, StylePack: in.StylePack, IdempotencyKey: in.IdempotencyKey,
		})
		return server.AvatarStyleMutationResult{
			Style: toServerAvatarStyle(result.Style), Receipt: toServerAvatarReceipt(result.Receipt),
		}, mapAvatarError(err)
	}
}

func toServerSelfAvatarCheckpoint(checkpoint store.SelfAvatarCheckpoint) *server.SelfAvatarCheckpoint {
	return &server.SelfAvatarCheckpoint{
		Pending: checkpoint.Pending, Status: checkpoint.Status, Reason: checkpoint.Reason,
		ProfileRevision: checkpoint.ProfileRevision, LineageGeneration: checkpoint.LineageGeneration,
		StylePackID: checkpoint.StylePackID, StylePackVersion: checkpoint.StylePackVersion,
		ActiveVersion: checkpoint.ActiveVersion, ProposedVersion: checkpoint.ProposedVersion,
		AttemptCount: checkpoint.AttemptCount, RetryAfter: checkpoint.RetryAfter,
	}
}

func toStoreAvatarProposal(in server.ProposeAvatarRequest) store.ProposeAvatarInput {
	return store.ProposeAvatarInput{
		ExpectedProfileRevision: in.ExpectedProfileRevision,
		ParentVersion:           in.ParentVersion,
		StylePackID:             in.StylePackID,
		StylePackVersion:        in.StylePackVersion,
		SubjectForm:             in.SubjectForm,
		Description:             in.Description,
		VisualSpec:              in.VisualSpec,
		SVG:                     in.SVG,
		Provenance: store.AvatarClientProvenance{
			Runtime: in.Provenance.Runtime, Model: in.Provenance.Model,
			Recipe: in.Provenance.Recipe, RecipeVersion: in.Provenance.RecipeVersion,
		},
		IdempotencyKey: in.IdempotencyKey,
	}
}

func toServerAvatarView(view store.AvatarView) server.AvatarView {
	out := server.AvatarView{Profile: toServerAvatarProfile(view.Profile)}
	if view.Active != nil {
		active := toServerAvatarVersion(*view.Active)
		out.Active = &active
	}
	if view.Proposed != nil {
		proposed := toServerAvatarVersion(*view.Proposed)
		out.Proposed = &proposed
	}
	return out
}

func toServerAvatarProfile(profile store.AvatarProfile) server.AvatarProfile {
	return server.AvatarProfile{
		AccountID: profile.AccountID, RealmID: profile.RealmID, AgentID: profile.AgentID,
		SubjectForm: profile.SubjectForm, AutonomyPolicy: profile.AutonomyPolicy,
		Status: profile.Status, Style: profile.Style, LineageGeneration: profile.LineageGeneration,
		ProfileRevision: profile.ProfileRevision,
		LatestVersion:   profile.LatestVersion, ActiveVersion: profile.ActiveVersion,
		ProposedVersion: profile.ProposedVersion, AttemptCount: profile.AttemptCount,
		RetryAfter: profile.RetryAfter, FallbackSeed: profile.FallbackSeed,
		FailureCode: profile.FailureCode, CreatedAt: profile.CreatedAt, UpdatedAt: profile.UpdatedAt,
	}
}

func toServerAvatarVersion(version store.AvatarVersion) server.AvatarVersion {
	return server.AvatarVersion{
		ID: version.ID, AccountID: version.AccountID, RealmID: version.RealmID,
		AgentID: version.AgentID, Version: version.Version, ParentVersion: version.ParentVersion,
		LineageGeneration: version.LineageGeneration,
		SubjectForm:       version.SubjectForm, Description: version.Description,
		VisualSpec: version.VisualSpec, SVG: version.SVG, SVGSHA256: version.SVGSHA256,
		Style: version.Style,
		Provenance: server.AvatarClientProvenance{
			Runtime: version.Provenance.Runtime, Model: version.Provenance.Model,
			Recipe: version.Provenance.Recipe, RecipeVersion: version.Provenance.RecipeVersion,
		},
		ProposedBy: server.AvatarActor{Kind: version.ProposedBy.Kind, ID: version.ProposedBy.ID, Name: version.ProposedBy.Name},
		ProposedAt: version.ProposedAt,
		IsActive:   version.IsActive, IsProposed: version.IsProposed,
		WasActivated: version.WasActivated, RollbackEligible: version.RollbackEligible,
		Rejected: version.Rejected, LastActivatedAt: version.LastActivatedAt, RejectedAt: version.RejectedAt,
	}
}

func toServerAvatarVersionSummary(version store.AvatarVersionSummary) server.AvatarVersionSummary {
	return server.AvatarVersionSummary{
		ID: version.ID, AccountID: version.AccountID, RealmID: version.RealmID,
		AgentID: version.AgentID, Version: version.Version, ParentVersion: version.ParentVersion,
		LineageGeneration: version.LineageGeneration,
		SubjectForm:       version.SubjectForm, SVGSHA256: version.SVGSHA256, Style: version.Style,
		ProposedBy: server.AvatarActor{
			Kind: version.ProposedBy.Kind, ID: version.ProposedBy.ID, Name: version.ProposedBy.Name,
		},
		ProposedAt: version.ProposedAt,
		IsActive:   version.IsActive, IsProposed: version.IsProposed,
		WasActivated: version.WasActivated, RollbackEligible: version.RollbackEligible,
		Rejected: version.Rejected, LastActivatedAt: version.LastActivatedAt, RejectedAt: version.RejectedAt,
	}
}

func toServerAvatarStyle(style store.AvatarStyleView) server.AvatarStyleView {
	return server.AvatarStyleView{
		RealmID: style.RealmID, StyleRevision: style.StyleRevision, StylePack: style.StylePack,
		CreatedAt: style.CreatedAt, UpdatedAt: style.UpdatedAt,
	}
}

func toServerAvatarReceipt(receipt store.AvatarMutationReceipt) server.AvatarMutationReceipt {
	return server.AvatarMutationReceipt{
		Operation:   receipt.Operation,
		Actor:       server.AvatarActor{Kind: receipt.Actor.Kind, ID: receipt.Actor.ID, Name: receipt.Actor.Name},
		RequestHash: receipt.RequestHash, ResultRevision: receipt.ResultRevision,
		ResultVersion: receipt.ResultVersion, ResultLineageGeneration: receipt.ResultLineageGeneration,
		Replayed: receipt.Replayed, CreatedAt: receipt.CreatedAt,
	}
}

func toServerAvatarMutation(result store.AvatarMutationResult) server.AvatarMutationResult {
	return server.AvatarMutationResult{
		Avatar: toServerAvatarView(result.Avatar), Receipt: toServerAvatarReceipt(result.Receipt),
	}
}

func mapAvatarError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrAvatarInputInvalid):
		return server.ErrBadInput
	case errors.Is(err, store.ErrAvatarForbidden):
		return server.ErrForbidden
	case errors.Is(err, store.ErrAvatarNotFound),
		errors.Is(err, store.ErrAvatarVersionNotFound),
		errors.Is(err, store.ErrAvatarStyleNotFound):
		return server.ErrNotFound
	case errors.Is(err, store.ErrAvatarIdempotencyConflict):
		return server.ErrIdempotencyConflict
	case errors.Is(err, store.ErrAvatarConflict):
		return server.ErrConflict
	default:
		return err
	}
}
