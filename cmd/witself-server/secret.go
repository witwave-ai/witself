package main

import (
	"context"
	"errors"

	"github.com/witwave-ai/witself/internal/server"
	"github.com/witwave-ai/witself/internal/store"
)

// configureSecrets binds the ciphertext-only HTTP contract to PostgreSQL. All
// encryption, unwrap, and plaintext use remain in the active client.
func configureSecrets(cfg *server.Config, st *store.Store) {
	cfg.GetCurrentVaultKey = func(ctx context.Context, p server.DomainPrincipal) (*server.VaultKeyBinding, error) {
		binding, err := st.GetCurrentVaultKey(ctx, toStorePrincipal(p))
		if err != nil || binding == nil {
			return nil, mapSecretError(err)
		}
		out := toServerVaultKey(*binding)
		return &out, nil
	}
	cfg.RegisterVaultKey = func(ctx context.Context, p server.DomainPrincipal, in server.RegisterVaultKeyRequest) (server.VaultKeyMutationResult, error) {
		binding, receipt, err := st.RegisterVaultKey(ctx, toStorePrincipal(p), store.RegisterVaultKeyInput{
			ID: in.ID, KeyVersion: in.KeyVersion, Algorithm: in.Algorithm,
			Fingerprint: in.Fingerprint, IdempotencyKey: in.IdempotencyKey,
		})
		return server.VaultKeyMutationResult{
			KeyEpoch: toServerVaultKey(binding), Receipt: toServerSecretReceipt(receipt),
		}, mapSecretError(err)
	}
	cfg.CreateVaultKeyEnrollment = func(ctx context.Context, p server.DomainPrincipal, in server.CreateVaultKeyEnrollmentRequest) (server.VaultKeyEnrollment, error) {
		value, err := st.CreateVaultKeyEnrollment(ctx, toStorePrincipal(p), store.CreateVaultKeyEnrollmentInput{
			ID: in.ID, TargetLocationID: in.TargetLocationID, TargetLocationName: in.TargetLocationName,
			TargetPublicKey: in.TargetPublicKey, TargetKeyAlgorithm: in.TargetKeyAlgorithm,
			PairingCommitment: in.PairingCommitment, ExpiresAt: in.ExpiresAt, IdempotencyKey: in.IdempotencyKey,
		})
		return toServerVaultKeyEnrollment(value), mapSecretError(err)
	}
	cfg.ListVaultKeyEnrollments = func(ctx context.Context, p server.DomainPrincipal, opts server.VaultKeyEnrollmentListOptions) ([]server.VaultKeyEnrollment, error) {
		values, err := st.ListVaultKeyEnrollments(ctx, toStorePrincipal(p), store.VaultKeyEnrollmentListOptions{State: opts.State, Limit: opts.Limit})
		if err != nil {
			return nil, mapSecretError(err)
		}
		out := make([]server.VaultKeyEnrollment, len(values))
		for index := range values {
			out[index] = toServerVaultKeyEnrollment(values[index])
		}
		return out, nil
	}
	cfg.GetVaultKeyEnrollment = func(ctx context.Context, p server.DomainPrincipal, enrollmentID string) (server.VaultKeyEnrollment, error) {
		value, err := st.GetVaultKeyEnrollment(ctx, toStorePrincipal(p), enrollmentID)
		return toServerVaultKeyEnrollment(value), mapSecretError(err)
	}
	cfg.ApproveVaultKeyEnrollment = func(ctx context.Context, p server.DomainPrincipal, enrollmentID string, in server.ApproveVaultKeyEnrollmentRequest) (server.VaultKeyEnrollment, error) {
		value, err := st.ApproveVaultKeyEnrollment(ctx, toStorePrincipal(p), enrollmentID, store.ApproveVaultKeyEnrollmentInput{
			ExpectedRowVersion: in.ExpectedRowVersion, SourceLocationID: in.SourceLocationID,
			SourceEphemeralPublicKey: in.SourceEphemeralPublicKey, TransferCiphertext: in.TransferCiphertext,
			TransferAlgorithm: in.TransferAlgorithm, ConsumeCommitment: in.ConsumeCommitment,
			IdempotencyKey: in.IdempotencyKey,
		})
		return toServerVaultKeyEnrollment(value), mapSecretError(err)
	}
	cfg.ReceiveVaultKeyEnrollment = func(ctx context.Context, p server.DomainPrincipal, enrollmentID, targetLocationID string) (server.VaultKeyEnrollmentTransfer, error) {
		value, err := st.AccessVaultKeyEnrollmentTransfer(ctx, toStorePrincipal(p), enrollmentID, targetLocationID)
		return server.VaultKeyEnrollmentTransfer{
			Enrollment:               toServerVaultKeyEnrollment(value.Enrollment),
			SourceEphemeralPublicKey: value.SourceEphemeralPublicKey,
			Ciphertext:               value.Ciphertext, ConsumeCommitment: value.ConsumeCommitment,
		}, mapSecretError(err)
	}
	cfg.ConsumeVaultKeyEnrollment = func(ctx context.Context, p server.DomainPrincipal, enrollmentID string, in server.ConsumeVaultKeyEnrollmentRequest) (server.VaultKeyEnrollment, error) {
		value, err := st.ConsumeVaultKeyEnrollment(ctx, toStorePrincipal(p), enrollmentID, store.ConsumeVaultKeyEnrollmentInput{
			ExpectedRowVersion: in.ExpectedRowVersion, TargetLocationID: in.TargetLocationID,
			ConsumeProof: in.ConsumeProof, IdempotencyKey: in.IdempotencyKey,
		})
		return toServerVaultKeyEnrollment(value), mapSecretError(err)
	}
	cfg.CancelVaultKeyEnrollment = func(ctx context.Context, p server.DomainPrincipal, enrollmentID string, in server.CancelVaultKeyEnrollmentRequest) (server.VaultKeyEnrollment, error) {
		value, err := st.CancelVaultKeyEnrollment(ctx, toStorePrincipal(p), enrollmentID, store.CancelVaultKeyEnrollmentInput{
			ExpectedRowVersion: in.ExpectedRowVersion, IdempotencyKey: in.IdempotencyKey,
		})
		return toServerVaultKeyEnrollment(value), mapSecretError(err)
	}
	cfg.CreateSecret = func(ctx context.Context, p server.DomainPrincipal, in server.CreateSecretRequest) (server.SecretMutationResult, error) {
		fields := make([]store.CreateSecretFieldInput, len(in.Fields))
		for index := range in.Fields {
			fields[index] = toStoreCreateSecretField(in.Fields[index])
		}
		result, err := st.CreateSecret(ctx, toStorePrincipal(p), store.CreateSecretInput{
			ID: in.ID, Name: in.Name, Description: in.Description, Template: in.Template,
			Tags: in.Tags, Fields: fields, IdempotencyKey: in.IdempotencyKey,
		})
		return server.SecretMutationResult{
			Secret: toServerSecret(result.Secret), Receipt: toServerSecretReceipt(result.Receipt),
		}, mapSecretError(err)
	}
	cfg.GetSecretLimitStatus = func(ctx context.Context, p server.DomainPrincipal) (server.SecretLimitStatus, error) {
		status, err := st.GetSecretLimitStatus(ctx, toStorePrincipal(p))
		return toServerSecretLimitStatus(status), mapSecretError(err)
	}
	cfg.ListSecrets = func(ctx context.Context, p server.DomainPrincipal, opts server.SecretListOptions) (server.SecretPage, error) {
		page, err := st.ListSecrets(ctx, toStorePrincipal(p), store.SecretListOptions{
			Query: opts.Query, Lifecycle: opts.Lifecycle, Template: opts.Template,
			Tags: opts.Tags, Limit: opts.Limit, Cursor: opts.Cursor, IncludeFields: opts.IncludeFields,
		})
		if err != nil {
			return server.SecretPage{}, mapSecretError(err)
		}
		items := make([]server.Secret, len(page.Secrets))
		for index := range page.Secrets {
			items[index] = toServerSecret(page.Secrets[index])
		}
		return server.SecretPage{Items: items, NextCursor: page.NextCursor}, nil
	}
	cfg.GetSecret = func(ctx context.Context, p server.DomainPrincipal, secretID string) (server.Secret, error) {
		value, err := st.GetSecret(ctx, toStorePrincipal(p), secretID)
		return toServerSecret(value), mapSecretError(err)
	}
	cfg.ArchiveSecret = func(ctx context.Context, p server.DomainPrincipal, secretID string, in server.SecretLifecycleRequest) (server.SecretMutationResult, error) {
		result, err := st.ArchiveSecret(ctx, toStorePrincipal(p), secretID, store.SecretLifecycleInput{
			ExpectedRowVersion: in.ExpectedRowVersion, IdempotencyKey: in.IdempotencyKey,
		})
		return server.SecretMutationResult{
			Secret: toServerSecret(result.Secret), Receipt: toServerSecretReceipt(result.Receipt),
		}, mapSecretError(err)
	}
	cfg.RestoreSecret = func(ctx context.Context, p server.DomainPrincipal, secretID string, in server.SecretLifecycleRequest) (server.SecretMutationResult, error) {
		result, err := st.RestoreSecret(ctx, toStorePrincipal(p), secretID, store.SecretLifecycleInput{
			ExpectedRowVersion: in.ExpectedRowVersion, IdempotencyKey: in.IdempotencyKey,
		})
		return server.SecretMutationResult{
			Secret: toServerSecret(result.Secret), Receipt: toServerSecretReceipt(result.Receipt),
		}, mapSecretError(err)
	}
	cfg.DeleteSecret = func(ctx context.Context, p server.DomainPrincipal, secretID string, in server.SecretLifecycleRequest) (server.SecretMutationResult, error) {
		result, err := st.DeleteSecret(ctx, toStorePrincipal(p), secretID, store.SecretLifecycleInput{
			ExpectedRowVersion: in.ExpectedRowVersion, IdempotencyKey: in.IdempotencyKey,
		})
		return server.SecretMutationResult{
			Secret: toServerSecret(result.Secret), Receipt: toServerSecretReceipt(result.Receipt),
		}, mapSecretError(err)
	}
	cfg.AccessSecretField = func(ctx context.Context, p server.DomainPrincipal, secretID, fieldID string, in server.AccessSecretFieldRequest) (server.SecretMaterial, error) {
		material, err := st.AccessSecretField(ctx, toStorePrincipal(p), secretID, fieldID, store.AccessSecretFieldInput{
			IdempotencyKey: in.IdempotencyKey,
		})
		return toServerSecretMaterial(material), mapSecretError(err)
	}
	configureVaultKeyRotations(cfg, st)
}

func toStoreCreateSecretField(in server.CreateSecretFieldRequest) store.CreateSecretFieldInput {
	out := store.CreateSecretFieldInput{
		ID: in.ID, Name: in.Name, Kind: in.Kind, Sensitive: in.Sensitive,
		Encoding: in.Encoding, ValueVersion: in.ValueVersion, PublicValue: in.PublicValue,
	}
	if in.Sealed != nil {
		out.Sealed = &store.SealedFieldInput{
			EnvelopeVersion: in.Sealed.EnvelopeVersion, Ciphertext: in.Sealed.Ciphertext,
			Algorithm: in.Sealed.Algorithm, AADVersion: in.Sealed.AADVersion,
			DEK: store.SealedDEKInput{
				ID: in.Sealed.DEK.ID, Generation: in.Sealed.DEK.Generation,
				WrappedDEK: in.Sealed.DEK.WrappedDEK, WrapAlgorithm: in.Sealed.DEK.WrapAlgorithm,
				AADVersion: in.Sealed.DEK.AADVersion, WrapRevision: in.Sealed.DEK.WrapRevision,
				WrappingKeyID:      in.Sealed.DEK.WrappingKeyID,
				WrappingKeyVersion: in.Sealed.DEK.WrappingKeyVersion,
			},
		}
	}
	return out
}

func toServerVaultKey(value store.VaultKeyBinding) server.VaultKeyBinding {
	return server.VaultKeyBinding{
		ID: value.ID, AccountID: value.AccountID, RealmID: value.RealmID,
		OwnerAgentID: value.OwnerAgentID, KeyVersion: value.KeyVersion,
		Algorithm: value.Algorithm, Fingerprint: value.Fingerprint,
		LifecycleState: value.LifecycleState, RowVersion: value.RowVersion,
		CreatedAt: value.CreatedAt, RetiredAt: value.RetiredAt,
	}
}

func toServerVaultKeyEnrollment(value store.VaultKeyEnrollment) server.VaultKeyEnrollment {
	return server.VaultKeyEnrollment{
		ID: value.ID, AccountID: value.AccountID, RealmID: value.RealmID,
		OwnerAgentID: value.OwnerAgentID, VaultKeyID: value.VaultKeyID,
		VaultKeyVersion: value.VaultKeyVersion, VaultKeyAlgorithm: value.VaultKeyAlgorithm,
		VaultKeyFingerprint: value.VaultKeyFingerprint, TargetLocationID: value.TargetLocationID,
		TargetLocationName: value.TargetLocationName, TargetPublicKey: value.TargetPublicKey,
		TargetKeyAlgorithm: value.TargetKeyAlgorithm, PairingCommitment: value.PairingCommitment,
		LifecycleState: value.LifecycleState, SourceLocationID: value.SourceLocationID,
		TransferAlgorithm: value.TransferAlgorithm, RowVersion: value.RowVersion,
		CreatedAt: value.CreatedAt, ExpiresAt: value.ExpiresAt, ApprovedAt: value.ApprovedAt,
		ConsumedAt: value.ConsumedAt, CancelledAt: value.CancelledAt, ExpiredAt: value.ExpiredAt,
	}
}

func toServerSecret(value store.Secret) server.Secret {
	fields := make([]server.SecretField, len(value.Fields))
	for index := range value.Fields {
		field := value.Fields[index]
		fields[index] = server.SecretField{
			ID: field.ID, Name: field.Name, Kind: field.Kind, Sensitive: field.Sensitive,
			Encoding: field.Encoding, ValueVersion: field.ValueVersion,
			PublicValue: field.PublicValue, Redacted: field.Redacted,
			RowVersion: field.RowVersion, DEKGeneration: field.DEKGeneration,
		}
	}
	return server.Secret{
		ID: value.ID, AccountID: value.AccountID, RealmID: value.RealmID,
		OwnerAgentID: value.OwnerAgentID, Name: value.Name, Description: value.Description,
		Template: value.Template, Tags: value.Tags, Fields: fields,
		Lifecycle: value.Lifecycle, RowVersion: value.RowVersion,
		CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt, ArchivedAt: value.ArchivedAt,
		DeletedAt:      value.DeletedAt,
		SensitiveCount: value.SensitiveCount,
	}
}

func toServerSecretLimitStatus(value store.SecretLimitStatus) server.SecretLimitStatus {
	return server.SecretLimitStatus{
		Used: value.Used, Max: value.Max, Remaining: value.Remaining,
		Unlimited: value.Unlimited, OverLimit: value.OverLimit,
	}
}

func toServerSecretMaterial(value store.SecretMaterial) server.SecretMaterial {
	return server.SecretMaterial{
		SecretID: value.SecretID, FieldID: value.FieldID, FieldName: value.FieldName,
		FieldKind: value.FieldKind, Encoding: value.Encoding, ValueVersion: value.ValueVersion,
		EnvelopeVersion: value.EnvelopeVersion, Ciphertext: value.Ciphertext,
		Algorithm: value.Algorithm, AADVersion: value.AADVersion,
		DEK: server.SealedDEK{
			ID: value.DEK.ID, Generation: value.DEK.Generation, WrappedDEK: value.DEK.WrappedDEK,
			WrapAlgorithm: value.DEK.WrapAlgorithm, AADVersion: value.DEK.AADVersion,
			WrapRevision: value.DEK.WrapRevision, WrappingKeyID: value.DEK.WrappingKeyID,
			WrappingKeyVersion: value.DEK.WrappingKeyVersion,
		},
		SecretRevision: value.SecretRevision, FieldRevision: value.FieldRevision,
	}
}

func toServerSecretReceipt(value store.SecretMutationReceipt) server.SecretMutationReceipt {
	return server.SecretMutationReceipt{
		Operation: value.Operation, RequestHash: value.RequestHash,
		TargetKind: value.TargetKind, TargetID: value.TargetID,
		ResultRevision: value.ResultRevision, ResultValueVersion: value.ResultValueVersion,
		Replayed: value.Replayed, CreatedAt: value.CreatedAt,
	}
}

func mapSecretError(err error) error {
	var limitErr *store.SecretLimitError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrSecretInputInvalid):
		return server.ErrBadInput
	case errors.Is(err, store.ErrSecretForbidden), errors.Is(err, store.ErrAccountNotActive), errors.Is(err, store.ErrAgentNotFound):
		return server.ErrForbidden
	case errors.Is(err, store.ErrSecretNotFound), errors.Is(err, store.ErrSecretFieldNotFound), errors.Is(err, store.ErrAccountNotFound):
		return server.ErrNotFound
	case errors.Is(err, store.ErrVaultEnrollmentNotFound), errors.Is(err, store.ErrVaultKeyRotationNotFound):
		return server.ErrNotFound
	case errors.Is(err, store.ErrSecretIdempotencyConflict):
		return server.ErrIdempotencyConflict
	case errors.Is(err, store.ErrVaultKeyUnavailable):
		return server.ErrSecretVaultKeyUnavailable
	case errors.Is(err, store.ErrVaultKeyMismatch):
		return server.ErrSecretVaultKeyMismatch
	case errors.As(err, &limitErr):
		return &server.SecretLimitError{Status: toServerSecretLimitStatus(limitErr.Status)}
	case errors.Is(err, store.ErrSecretConflict), errors.Is(err, store.ErrVaultKeyConflict),
		errors.Is(err, store.ErrVaultEnrollmentConflict), errors.Is(err, store.ErrVaultEnrollmentExpired),
		errors.Is(err, store.ErrVaultEnrollmentLimit), errors.Is(err, store.ErrVaultEnrollmentProof),
		errors.Is(err, store.ErrVaultLifecycleInProgress),
		errors.Is(err, store.ErrVaultKeyRotationInProgress), errors.Is(err, store.ErrVaultKeyRotationConflict),
		errors.Is(err, store.ErrVaultKeyRotationIncomplete):
		return server.ErrConflict
	default:
		return err
	}
}
