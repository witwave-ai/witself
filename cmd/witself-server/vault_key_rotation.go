package main

import (
	"context"

	"github.com/witwave-ai/witself/internal/server"
	"github.com/witwave-ai/witself/internal/store"
)

func configureVaultKeyRotations(cfg *server.Config, st *store.Store) {
	cfg.StartVaultKeyRotation = func(ctx context.Context, p server.DomainPrincipal, in server.StartVaultKeyRotationRequest) (server.VaultKeyRotationMutationResult, error) {
		rotation, receipt, err := st.StartVaultKeyRotation(ctx, toStorePrincipal(p), store.StartVaultKeyRotationInput{
			ID: in.ID, ExpectedSourceKeyID: in.ExpectedSourceKeyID,
			ExpectedSourceKeyVersion:    in.ExpectedSourceKeyVersion,
			ExpectedSourceKeyRowVersion: in.ExpectedSourceKeyRowVersion,
			TargetKeyID:                 in.TargetKeyID, TargetKeyVersion: in.TargetKeyVersion,
			TargetAlgorithm: in.TargetAlgorithm, TargetFingerprint: in.TargetFingerprint,
			IdempotencyKey: in.IdempotencyKey,
		})
		return server.VaultKeyRotationMutationResult{
			Rotation: toServerVaultKeyRotation(rotation),
			Receipt:  toServerVaultKeyRotationReceipt(receipt),
		}, mapSecretError(err)
	}
	cfg.GetOpenVaultKeyRotation = func(ctx context.Context, p server.DomainPrincipal) (*server.VaultKeyRotation, error) {
		rotation, err := st.GetOpenVaultKeyRotation(ctx, toStorePrincipal(p))
		if err != nil || rotation == nil {
			return nil, mapSecretError(err)
		}
		out := toServerVaultKeyRotation(*rotation)
		return &out, nil
	}
	cfg.GetVaultKeyRotation = func(ctx context.Context, p server.DomainPrincipal, rotationID string) (server.VaultKeyRotation, error) {
		rotation, err := st.GetVaultKeyRotation(ctx, toStorePrincipal(p), rotationID)
		return toServerVaultKeyRotation(rotation), mapSecretError(err)
	}
	cfg.ListVaultKeyRotationItems = func(ctx context.Context, p server.DomainPrincipal, rotationID string, opts server.VaultKeyRotationItemListOptions) (server.VaultKeyRotationItemPage, error) {
		page, err := st.ListVaultKeyRotationItems(ctx, toStorePrincipal(p), rotationID,
			store.VaultKeyRotationItemListOptions{Limit: opts.Limit, Cursor: opts.Cursor})
		if err != nil {
			return server.VaultKeyRotationItemPage{}, mapSecretError(err)
		}
		items := make([]server.VaultKeyRotationItem, len(page.Items))
		for index := range page.Items {
			items[index] = toServerVaultKeyRotationItem(page.Items[index])
		}
		return server.VaultKeyRotationItemPage{Items: items, NextCursor: page.NextCursor}, nil
	}
	cfg.StageVaultKeyRotation = func(ctx context.Context, p server.DomainPrincipal, rotationID string, in server.StageVaultKeyRotationRequest) (server.VaultKeyRotationMutationResult, error) {
		items := make([]store.StageVaultKeyRotationItemInput, len(in.Items))
		for index := range in.Items {
			items[index] = store.StageVaultKeyRotationItemInput{
				DEKID:                       in.Items[index].DEKID,
				ExpectedSourceDEKRowVersion: in.Items[index].ExpectedSourceDEKRowVersion,
				ExpectedSourceWrapRevision:  in.Items[index].ExpectedSourceWrapRevision,
				TargetWrappedDEK:            in.Items[index].TargetWrappedDEK,
				TargetWrapRevision:          in.Items[index].TargetWrapRevision,
			}
		}
		rotation, receipt, err := st.StageVaultKeyRotation(ctx, toStorePrincipal(p), rotationID,
			store.StageVaultKeyRotationInput{
				ExpectedRotationRowVersion: in.ExpectedRotationRowVersion,
				Items:                      items, IdempotencyKey: in.IdempotencyKey,
			})
		return server.VaultKeyRotationMutationResult{
			Rotation: toServerVaultKeyRotation(rotation),
			Receipt:  toServerVaultKeyRotationReceipt(receipt),
		}, mapSecretError(err)
	}
	cfg.CommitVaultKeyRotation = func(ctx context.Context, p server.DomainPrincipal, rotationID string, in server.CommitVaultKeyRotationRequest) (server.VaultKeyRotationMutationResult, error) {
		rotation, receipt, err := st.CommitVaultKeyRotation(ctx, toStorePrincipal(p), rotationID,
			store.CommitVaultKeyRotationInput{
				ExpectedRotationRowVersion: in.ExpectedRotationRowVersion,
				ExpectedItemCount:          in.ExpectedItemCount, ExpectedPlanHash: in.ExpectedPlanHash,
				RecoveryDisposition: store.VaultKeyRotationRecoveryDisposition{
					Mode: in.RecoveryDisposition.Mode, ArtifactSHA256: in.RecoveryDisposition.ArtifactSHA256,
				},
				IdempotencyKey: in.IdempotencyKey,
			})
		return server.VaultKeyRotationMutationResult{
			Rotation: toServerVaultKeyRotation(rotation),
			Receipt:  toServerVaultKeyRotationReceipt(receipt),
		}, mapSecretError(err)
	}
	cfg.CancelVaultKeyRotation = func(ctx context.Context, p server.DomainPrincipal, rotationID string, in server.CancelVaultKeyRotationRequest) (server.VaultKeyRotationMutationResult, error) {
		rotation, receipt, err := st.CancelVaultKeyRotation(ctx, toStorePrincipal(p), rotationID,
			store.CancelVaultKeyRotationInput{
				ExpectedRotationRowVersion: in.ExpectedRotationRowVersion,
				IdempotencyKey:             in.IdempotencyKey,
			})
		return server.VaultKeyRotationMutationResult{
			Rotation: toServerVaultKeyRotation(rotation),
			Receipt:  toServerVaultKeyRotationReceipt(receipt),
		}, mapSecretError(err)
	}
}

func toServerVaultKeyRotation(value store.VaultKeyRotation) server.VaultKeyRotation {
	return server.VaultKeyRotation{
		ID: value.ID, AccountID: value.AccountID, RealmID: value.RealmID,
		OwnerAgentID: value.OwnerAgentID, SourceKeyID: value.SourceKeyID,
		SourceKeyVersion: value.SourceKeyVersion, SourceKeyAlgorithm: value.SourceKeyAlgorithm,
		SourceKeyFingerprint: value.SourceKeyFingerprint, TargetKeyID: value.TargetKeyID,
		TargetKeyVersion: value.TargetKeyVersion, TargetKeyAlgorithm: value.TargetKeyAlgorithm,
		TargetKeyFingerprint: value.TargetKeyFingerprint, LifecycleState: value.LifecycleState,
		RecoveryDispositionMode: value.RecoveryDispositionMode,
		RecoveryArtifactSHA256:  value.RecoveryArtifactSHA256,
		ItemCount:               value.ItemCount, StagedCount: value.StagedCount, RowVersion: value.RowVersion,
		StagedPlanHash: value.StagedPlanHash, CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt,
		CommittedAt: value.CommittedAt, CancelledAt: value.CancelledAt,
	}
}

func toServerVaultKeyRotationItem(value store.VaultKeyRotationItem) server.VaultKeyRotationItem {
	return server.VaultKeyRotationItem{
		RotationID: value.RotationID, SecretID: value.SecretID,
		FieldID: value.FieldID, FieldKind: value.FieldKind, DEKID: value.DEKID,
		DEKGeneration: value.DEKGeneration, SourceDEKRowVersion: value.SourceDEKRowVersion,
		SourceWrapRevision: value.SourceWrapRevision, SourceWrappedDEK: value.SourceWrappedDEK,
		SourceWrapAlgorithm: value.SourceWrapAlgorithm, SourceAADVersion: value.SourceAADVersion,
		SourceWrappingKeyID:      value.SourceWrappingKeyID,
		SourceWrappingKeyVersion: value.SourceWrappingKeyVersion,
		TargetWrappingKeyID:      value.TargetWrappingKeyID,
		TargetWrappingKeyVersion: value.TargetWrappingKeyVersion,
		TargetWrappedDEK:         value.TargetWrappedDEK, TargetWrapRevision: value.TargetWrapRevision,
		TargetWrapperSHA256: value.TargetWrapperSHA256, StagedAt: value.StagedAt,
	}
}

func toServerVaultKeyRotationReceipt(value store.VaultKeyRotationReceipt) server.VaultKeyRotationReceipt {
	return server.VaultKeyRotationReceipt{
		Operation: value.Operation, RequestHash: value.RequestHash,
		RotationID: value.RotationID, ResultRevision: value.ResultRevision,
		Replayed: value.Replayed, CreatedAt: value.CreatedAt,
	}
}
