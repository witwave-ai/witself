package secretclient

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/id"
	"github.com/witwave-ai/witself/internal/local"
	"github.com/witwave-ai/witself/internal/sealed"
	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

const defaultVaultKeyEnrollmentLifetime = 10 * time.Minute

// BeginVaultKeyEnrollmentInput selects this installation's stable location and
// the server-governed request expiry. A zero expiry selects a ten-minute
// request; caller-provided instants are canonicalized to a whole UTC second.
type BeginVaultKeyEnrollmentInput struct {
	LocationName string
	ExpiresAt    time.Time
}

// BeginVaultKeyEnrollmentResult contains the public request plus the one
// deliberate out-of-band credential needed by an already enrolled machine.
// Ordinary and JSON formatting are redacted; a deliberate controlling-terminal
// presenter must read PairingSecret directly.
type BeginVaultKeyEnrollmentResult struct {
	Enrollment    client.VaultKeyEnrollment `json:"enrollment"`
	PairingSecret string                    `json:"pairing_secret"`
	SAS           string                    `json:"sas"`
}

func (r BeginVaultKeyEnrollmentResult) String() string {
	return fmt.Sprintf("<vault-key-enrollment-begin id=%s state=%s pairing=redacted>",
		r.Enrollment.ID, r.Enrollment.LifecycleState)
}

// GoString returns the same redacted diagnostic representation as String.
func (r BeginVaultKeyEnrollmentResult) GoString() string { return r.String() }

// MarshalJSON returns a public view with the pairing secret redacted.
func (r BeginVaultKeyEnrollmentResult) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Enrollment    client.VaultKeyEnrollment `json:"enrollment"`
		PairingSecret string                    `json:"pairing_secret"`
		SAS           string                    `json:"sas"`
	}{Enrollment: r.Enrollment, PairingSecret: "[redacted]", SAS: r.SAS})
}

// ApproveVaultKeyEnrollmentInput supplies the request id and the deliberate
// out-of-band pairing credential received from the target installation.
type ApproveVaultKeyEnrollmentInput struct {
	EnrollmentID       string
	PairingSecret      string
	SourceLocationName string
}

func (in ApproveVaultKeyEnrollmentInput) String() string {
	return fmt.Sprintf("<vault-key-enrollment-approve id=%s pairing=redacted>", strings.TrimSpace(in.EnrollmentID))
}

// GoString returns the same redacted diagnostic representation as String.
func (in ApproveVaultKeyEnrollmentInput) GoString() string { return in.String() }

// MarshalJSON returns a public view with the pairing secret redacted.
func (in ApproveVaultKeyEnrollmentInput) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		EnrollmentID       string `json:"enrollment_id"`
		PairingSecret      string `json:"pairing_secret"`
		SourceLocationName string `json:"source_location_name,omitempty"`
	}{
		EnrollmentID: in.EnrollmentID, PairingSecret: "[redacted]",
		SourceLocationName: in.SourceLocationName,
	})
}

// CompleteVaultKeyEnrollmentInput selects the pending request on the target
// installation. TargetLocationName may update the stable local display label.
type CompleteVaultKeyEnrollmentInput struct {
	EnrollmentID       string
	TargetLocationName string
}

// BeginVaultKeyEnrollment creates private recipient state durably before the
// backend request, validates the canonical response byte-for-byte at the
// logical request level, and only then finalizes the local public sidecar.
func (s *Service) BeginVaultKeyEnrollment(ctx context.Context, in BeginVaultKeyEnrollmentInput) (*BeginVaultKeyEnrollmentResult, error) {
	identity, err := s.identity(ctx)
	if err != nil {
		return nil, err
	}
	location, err := transcriptcapture.EnsureLocation(strings.TrimSpace(in.LocationName))
	if err != nil || !validGeneratedID(location.ID, "loc") {
		return nil, ErrInvalidInput
	}
	binding, err := s.currentEnrollmentVaultKey(ctx, identity)
	if err != nil {
		return nil, err
	}
	expiresAt := in.ExpiresAt
	if expiresAt.IsZero() {
		expiresAt = time.Now().Add(defaultVaultKeyEnrollmentLifetime)
	}
	expiresAt = time.Unix(expiresAt.Unix(), 0).UTC()
	if expiresAt.Unix() <= 0 || expiresAt.Unix() > sealed.MaxAVKEnrollmentExpiresAtUnix {
		return nil, ErrInvalidInput
	}
	state, value, request, found, err := s.discoverVaultKeyEnrollment(ctx, identity, binding, location.ID)
	if err != nil {
		return nil, err
	}
	if found {
		defer state.Clear()
		if value != nil {
			return beginVaultKeyEnrollmentResult(value, state.PairingSecret, request)
		}
	} else {
		enrollmentID, err := id.New("enr")
		if err != nil {
			return nil, ErrOperation
		}
		recipient, err := sealed.GenerateAVKEnrollmentRecipientKey()
		if err != nil {
			return nil, ErrOperation
		}
		pairing, err := sealed.GenerateAVKEnrollmentPairingSecret()
		if err != nil {
			recipient.Clear()
			return nil, ErrOperation
		}
		state = &local.AgentVaultKeyEnrollmentState{
			RequestID: enrollmentID, RecipientKey: recipient, PairingSecret: pairing,
		}
		defer state.Clear()
		if err := local.CreateAgentVaultKeyEnrollmentPreflight(
			s.accountName, s.realmName, s.agentName, enrollmentID, recipient, pairing,
		); err != nil {
			return nil, ErrOperation
		}
	}
	if state == nil || state.RecipientKey == nil || state.PairingSecret == nil {
		return nil, ErrIntegrity
	}
	if request == (sealed.AVKEnrollmentRequest{}) {
		request, err = sealed.NewAVKEnrollmentRequest(state.RecipientKey, state.PairingSecret, sealed.AVKEnrollmentRequestOptions{
			AccountID: identity.AccountID, RealmID: identity.RealmID, OwnerAgentID: identity.AgentID,
			EnrollmentRequestID: state.RequestID, TargetInstallationID: location.ID, ExpiresAt: expiresAt.Unix(),
		})
		if err != nil {
			return nil, ErrOperation
		}
	}

	value, createErr := s.remote.createVaultKeyEnrollment(ctx, client.CreateVaultKeyEnrollmentInput{
		ID: state.RequestID, TargetLocationID: location.ID, TargetLocationName: location.Name,
		TargetPublicKey: request.TargetPublicKey, TargetKeyAlgorithm: request.KeyAgreementAlgorithm,
		PairingCommitment: request.PairingCommitment, ExpiresAt: time.Unix(request.ExpiresAt, 0).UTC(),
		IdempotencyKey: enrollmentMutationKey("begin", state.RequestID),
	})
	if createErr != nil || value == nil {
		// A response can be lost after commit. Resolve the exact generated id
		// from canonical state instead of creating a second private request.
		value, err = s.remote.getVaultKeyEnrollment(ctx, state.RequestID)
		if err != nil || value == nil {
			return nil, ErrOperation
		}
	}
	actual, err := validateExactEnrollment(value, identity, binding)
	if err != nil || actual != request || value.TargetLocationName != location.Name {
		return nil, ErrIntegrity
	}
	if err := local.FinalizeAgentVaultKeyEnrollmentRequest(
		s.accountName, s.realmName, s.agentName, state.RequestID, actual,
	); err != nil {
		return nil, ErrOperation
	}
	return beginVaultKeyEnrollmentResult(value, state.PairingSecret, actual)
}

// ApproveVaultKeyEnrollment authenticates the pairing commitment before
// opening the exact locally held AVK epoch, then uploads only a recipient-bound
// encrypted package. Replays resolve canonical approved state before resealing.
func (s *Service) ApproveVaultKeyEnrollment(ctx context.Context, in ApproveVaultKeyEnrollmentInput) (*client.VaultKeyEnrollment, error) {
	enrollmentID, ok := normalizedEnrollmentID(in.EnrollmentID)
	if !ok || strings.TrimSpace(in.PairingSecret) == "" {
		return nil, ErrInvalidInput
	}
	pairing, err := sealed.ParseAVKEnrollmentPairingSecret(strings.TrimSpace(in.PairingSecret))
	if err != nil {
		return nil, ErrIntegrity
	}
	defer pairing.Clear()
	identity, err := s.identity(ctx)
	if err != nil {
		return nil, err
	}
	location, err := transcriptcapture.EnsureLocation(strings.TrimSpace(in.SourceLocationName))
	if err != nil || !validGeneratedID(location.ID, "loc") {
		return nil, ErrInvalidInput
	}
	value, binding, request, err := s.loadExactEnrollment(ctx, identity, enrollmentID)
	if err != nil {
		return nil, err
	}
	if value.TargetLocationID == location.ID || sealed.VerifyAVKEnrollmentPairingSecret(pairing, request) != nil {
		return nil, ErrIntegrity
	}

	// Pairing authentication deliberately precedes reading the AVK.
	avk, err := local.ReadAgentVaultKeyEpoch(
		s.accountName, s.realmName, s.agentName, value.VaultKeyID, uint64(value.VaultKeyVersion),
	)
	if errors.Is(err, local.ErrAgentVaultKeyUnavailable) {
		return nil, ErrKeyUnavailable
	}
	if err != nil {
		return nil, ErrOperation
	}
	defer avk.Clear()
	if !vaultKeyMatchesEnrollment(avk, value, binding, identity) {
		return nil, ErrKeyMismatch
	}
	if value.LifecycleState == client.VaultEnrollmentStateApproved || value.LifecycleState == client.VaultEnrollmentStateConsumed {
		if value.SourceLocationID != location.ID {
			return nil, ErrIntegrity
		}
		return value, nil
	}
	if value.LifecycleState != client.VaultEnrollmentStatePending {
		return nil, ErrInvalidInput
	}
	packageValue, err := sealed.SealAVKEnrollmentPackage(avk, pairing, request, location.ID)
	if err != nil {
		return nil, ErrIntegrity
	}
	defer clear(packageValue.Ciphertext)
	approved, approveErr := s.remote.approveVaultKeyEnrollment(ctx, enrollmentID, client.ApproveVaultKeyEnrollmentInput{
		ExpectedRowVersion: value.RowVersion, SourceLocationID: location.ID,
		SourceEphemeralPublicKey: packageValue.SourceEphemeralPublicKey,
		TransferCiphertext:       packageValue.Ciphertext, TransferAlgorithm: client.VaultEnrollmentTransferAlgorithm,
		ConsumeCommitment: packageValue.ConsumeCommitment,
		IdempotencyKey:    enrollmentMutationKey("approve", enrollmentID),
	})
	if approveErr != nil || approved == nil {
		approved, err = s.remote.getVaultKeyEnrollment(ctx, enrollmentID)
		if err != nil || approved == nil {
			return nil, ErrOperation
		}
	}
	_, err = validateExactEnrollment(approved, identity, binding)
	if err != nil {
		return nil, err
	}
	if (approved.LifecycleState != client.VaultEnrollmentStateApproved &&
		approved.LifecycleState != client.VaultEnrollmentStateConsumed) || approved.SourceLocationID != location.ID {
		if approveErr != nil {
			return nil, ErrOperation
		}
		return nil, ErrIntegrity
	}
	return approved, nil
}

// CompleteVaultKeyEnrollment receives and opens the opaque package, publishes
// the exact immutable AVK epoch, rereads it, and only then proves consume.
// Pending private state is deleted only after canonical consumed state exists.
func (s *Service) CompleteVaultKeyEnrollment(ctx context.Context, in CompleteVaultKeyEnrollmentInput) (*client.VaultKeyEnrollment, error) {
	enrollmentID, ok := normalizedEnrollmentID(in.EnrollmentID)
	if !ok {
		return nil, ErrInvalidInput
	}
	identity, err := s.identity(ctx)
	if err != nil {
		return nil, err
	}
	location, err := transcriptcapture.EnsureLocation(strings.TrimSpace(in.TargetLocationName))
	if err != nil || !validGeneratedID(location.ID, "loc") {
		return nil, ErrInvalidInput
	}
	value, binding, request, err := s.loadExactEnrollment(ctx, identity, enrollmentID)
	if err != nil {
		return nil, err
	}
	if value.TargetLocationID != location.ID || (location.Name != "" && value.TargetLocationName != location.Name) {
		return nil, ErrIdentityMismatch
	}
	if value.LifecycleState == client.VaultEnrollmentStateConsumed {
		if err := s.verifyInstalledEnrollmentEpoch(value, binding, identity); err != nil {
			return nil, err
		}
		if err := local.DeleteAgentVaultKeyEnrollmentAfterConsume(s.accountName, s.realmName, s.agentName, enrollmentID); err != nil {
			return nil, ErrOperation
		}
		return value, nil
	}
	if value.LifecycleState != client.VaultEnrollmentStateApproved {
		return nil, ErrInvalidInput
	}
	state, err := local.ReadAgentVaultKeyEnrollmentState(s.accountName, s.realmName, s.agentName, enrollmentID)
	if errors.Is(err, local.ErrAgentVaultKeyEnrollmentUnavailable) {
		return nil, ErrKeyUnavailable
	}
	if err != nil {
		return nil, ErrOperation
	}
	defer func() { state.Clear() }()
	if state.Request == nil {
		if err := local.FinalizeAgentVaultKeyEnrollmentRequest(
			s.accountName, s.realmName, s.agentName, enrollmentID, request,
		); err != nil {
			return nil, ErrIntegrity
		}
		state.Clear()
		state, err = local.ReadAgentVaultKeyEnrollmentState(s.accountName, s.realmName, s.agentName, enrollmentID)
		if err != nil {
			return nil, ErrOperation
		}
	}
	if state.Request == nil || *state.Request != request {
		return nil, ErrIntegrity
	}
	transfer, err := s.remote.receiveVaultKeyEnrollment(ctx, enrollmentID, client.ReceiveVaultKeyEnrollmentInput{
		TargetLocationID: location.ID,
	})
	if err != nil || transfer == nil {
		return nil, ErrOperation
	}
	transferRequest, err := validateExactEnrollment(&transfer.Enrollment, identity, binding)
	if err != nil || transferRequest != request || transfer.Enrollment.LifecycleState != client.VaultEnrollmentStateApproved ||
		transfer.Enrollment.RowVersion != value.RowVersion {
		return nil, ErrIntegrity
	}
	packageValue := sealed.AVKEnrollmentPackage{
		PackageVersion: sealed.AVKEnrollmentPackageVersionV1, AADVersion: sealed.AVKEnrollmentAADVersionV1,
		KDFAlgorithm: sealed.AVKEnrollmentKDFAlgorithm, AEADAlgorithm: sealed.AES256GCMAlgorithm,
		ConsumeCommitmentAlgorithm: sealed.AVKEnrollmentConsumeCommitmentAlgorithm,
		Request:                    request, SourceInstallationID: transfer.Enrollment.SourceLocationID,
		SourceEphemeralPublicKey: transfer.SourceEphemeralPublicKey,
		AVK: sealed.AgentVaultKeyMetadata{
			ID: transfer.Enrollment.VaultKeyID, Version: uint64(transfer.Enrollment.VaultKeyVersion),
			Algorithm: transfer.Enrollment.VaultKeyAlgorithm, Fingerprint: transfer.Enrollment.VaultKeyFingerprint,
		},
		ConsumeCommitment: transfer.ConsumeCommitment,
		Ciphertext:        append([]byte(nil), transfer.Ciphertext...),
	}
	defer clear(packageValue.Ciphertext)
	avk, proof, err := sealed.OpenAVKEnrollmentPackage(state.RecipientKey, state.PairingSecret, request, packageValue)
	if err != nil || avk == nil || proof == nil {
		if avk != nil {
			avk.Clear()
		}
		if proof != nil {
			proof.Clear()
		}
		return nil, ErrIntegrity
	}
	defer avk.Clear()
	defer proof.Clear()
	if !vaultKeyMatchesEnrollment(avk, &transfer.Enrollment, binding, identity) {
		return nil, ErrIntegrity
	}
	if err := local.CreateAgentVaultKeyEpoch(s.accountName, s.realmName, s.agentName, avk); err != nil &&
		!errors.Is(err, local.ErrAgentVaultKeyExists) {
		return nil, ErrOperation
	}
	if err := s.verifyInstalledEnrollmentEpoch(&transfer.Enrollment, binding, identity); err != nil {
		return nil, err
	}
	proofBytes, err := proof.Bytes()
	if err != nil {
		return nil, ErrIntegrity
	}
	defer clear(proofBytes)
	consumed, consumeErr := s.remote.consumeVaultKeyEnrollment(ctx, enrollmentID, client.ConsumeVaultKeyEnrollmentInput{
		ExpectedRowVersion: transfer.Enrollment.RowVersion, TargetLocationID: location.ID,
		ConsumeProof: proofBytes, IdempotencyKey: enrollmentMutationKey("consume", enrollmentID),
	})
	if consumeErr != nil || consumed == nil {
		consumed, err = s.remote.getVaultKeyEnrollment(ctx, enrollmentID)
		if err != nil || consumed == nil {
			return nil, ErrOperation
		}
	}
	_, err = validateExactEnrollment(consumed, identity, binding)
	if err != nil {
		return nil, err
	}
	if consumed.LifecycleState != client.VaultEnrollmentStateConsumed || consumed.TargetLocationID != location.ID {
		if consumeErr != nil {
			return nil, ErrOperation
		}
		return nil, ErrIntegrity
	}
	if err := local.DeleteAgentVaultKeyEnrollmentAfterConsume(s.accountName, s.realmName, s.agentName, enrollmentID); err != nil {
		return nil, ErrOperation
	}
	return consumed, nil
}

// GetVaultKeyEnrollment returns one account-pinned, value-free public
// enrollment without consulting /self, the current AVK, or local private state.
// It therefore remains usable for safety inspection while suspended.
func (s *Service) GetVaultKeyEnrollment(ctx context.Context, enrollmentID string) (*client.VaultKeyEnrollment, error) {
	enrollmentID, ok := normalizedEnrollmentID(enrollmentID)
	if !ok {
		return nil, ErrInvalidInput
	}
	value, _, err := s.loadSafetyEnrollment(ctx, enrollmentID)
	return value, err
}

// ListVaultKeyEnrollments returns bounded account-pinned, value-free enrollment
// records and remains usable for safety discovery while suspended.
func (s *Service) ListVaultKeyEnrollments(ctx context.Context, options client.VaultKeyEnrollmentListOptions) ([]client.VaultKeyEnrollment, error) {
	items, err := s.remote.listVaultKeyEnrollments(ctx, options)
	if err != nil {
		return nil, ErrOperation
	}
	if items == nil {
		items = []client.VaultKeyEnrollment{}
	}
	for index := range items {
		if _, err := s.validateSafetyEnrollment(&items[index], items[index].ID); err != nil {
			return nil, err
		}
	}
	return items, nil
}

// CancelVaultKeyEnrollment cancels canonical pending or approved state without
// consulting /self, the current AVK binding, or any local AVK. That makes this
// harm-reducing operation usable while an account is suspended for evacuation.
// The exact public request/key coordinates remain authenticated by the token,
// pinned to the configured account, and are checked across every retry.
func (s *Service) CancelVaultKeyEnrollment(ctx context.Context, enrollmentID string) (*client.VaultKeyEnrollment, error) {
	enrollmentID, ok := normalizedEnrollmentID(enrollmentID)
	if !ok {
		return nil, ErrInvalidInput
	}
	value, request, err := s.loadSafetyEnrollment(ctx, enrollmentID)
	if err != nil {
		return nil, err
	}
	if value.LifecycleState == client.VaultEnrollmentStateCancelled {
		if err := s.cleanupTerminalEnrollmentState(enrollmentID, request); err != nil {
			return nil, err
		}
		return value, nil
	}
	if value.LifecycleState != client.VaultEnrollmentStatePending && value.LifecycleState != client.VaultEnrollmentStateApproved {
		return nil, ErrInvalidInput
	}
	cancelled, cancelErr := s.remote.cancelVaultKeyEnrollment(ctx, enrollmentID, client.CancelVaultKeyEnrollmentInput{
		ExpectedRowVersion: value.RowVersion, IdempotencyKey: enrollmentMutationKey("cancel", enrollmentID),
	})
	if cancelErr == nil && cancelled != nil {
		return s.finishCancelledEnrollment(enrollmentID, value, request, cancelled)
	}

	// The mutation may have committed after its response was lost. Re-read the
	// exact lifecycle fence before retrying the identical idempotent request.
	observed, observedRequest, err := s.loadSafetyEnrollment(ctx, enrollmentID)
	if err != nil {
		return nil, err
	}
	if !sameVaultKeyEnrollmentCoordinates(value, observed) || request != observedRequest {
		return nil, ErrIntegrity
	}
	if observed.LifecycleState == client.VaultEnrollmentStateCancelled {
		return s.finishCancelledEnrollment(enrollmentID, value, request, observed)
	}
	if observed.LifecycleState != value.LifecycleState || observed.RowVersion != value.RowVersion {
		return nil, ErrOperation
	}

	cancelled, cancelErr = s.remote.cancelVaultKeyEnrollment(ctx, enrollmentID, client.CancelVaultKeyEnrollmentInput{
		ExpectedRowVersion: value.RowVersion, IdempotencyKey: enrollmentMutationKey("cancel", enrollmentID),
	})
	if cancelErr == nil && cancelled != nil {
		return s.finishCancelledEnrollment(enrollmentID, value, request, cancelled)
	}
	observed, observedRequest, err = s.loadSafetyEnrollment(ctx, enrollmentID)
	if err != nil {
		return nil, err
	}
	if !sameVaultKeyEnrollmentCoordinates(value, observed) || request != observedRequest {
		return nil, ErrIntegrity
	}
	if observed.LifecycleState != client.VaultEnrollmentStateCancelled {
		return nil, ErrOperation
	}
	return s.finishCancelledEnrollment(enrollmentID, value, request, observed)
}

func (s *Service) loadSafetyEnrollment(ctx context.Context, enrollmentID string) (*client.VaultKeyEnrollment, sealed.AVKEnrollmentRequest, error) {
	value, err := s.remote.getVaultKeyEnrollment(ctx, enrollmentID)
	if err != nil || value == nil {
		return nil, sealed.AVKEnrollmentRequest{}, ErrOperation
	}
	request, err := s.validateSafetyEnrollment(value, enrollmentID)
	if err != nil {
		return nil, sealed.AVKEnrollmentRequest{}, err
	}
	return value, request, nil
}

func (s *Service) finishCancelledEnrollment(enrollmentID string, previous *client.VaultKeyEnrollment, request sealed.AVKEnrollmentRequest, cancelled *client.VaultKeyEnrollment) (*client.VaultKeyEnrollment, error) {
	validated, err := s.validateSafetyEnrollment(cancelled, enrollmentID)
	if err != nil {
		return nil, err
	}
	if !sameVaultKeyEnrollmentCoordinates(previous, cancelled) || validated != request ||
		cancelled.LifecycleState != client.VaultEnrollmentStateCancelled ||
		cancelled.RowVersion != previous.RowVersion+1 || cancelled.CancelledAt == nil {
		return nil, ErrIntegrity
	}
	if err := s.cleanupTerminalEnrollmentState(enrollmentID, request); err != nil {
		return nil, err
	}
	return cancelled, nil
}

func (s *Service) validateSafetyEnrollment(value *client.VaultKeyEnrollment, enrollmentID string) (sealed.AVKEnrollmentRequest, error) {
	if value == nil || value.ID != enrollmentID {
		return sealed.AVKEnrollmentRequest{}, ErrIntegrity
	}
	if value.AccountID != s.accountID {
		return sealed.AVKEnrollmentRequest{}, ErrIdentityMismatch
	}
	if !validGeneratedID(value.RealmID, "realm") || !validGeneratedID(value.OwnerAgentID, "agent") {
		return sealed.AVKEnrollmentRequest{}, ErrIntegrity
	}
	request, err := enrollmentRequestProjection(value, client.SelfIdentity{
		AccountID: value.AccountID, RealmID: value.RealmID, AgentID: value.OwnerAgentID,
	})
	if err != nil || value.RowVersion < 1 || value.CreatedAt.IsZero() || !value.ExpiresAt.After(value.CreatedAt) ||
		!validSafetyEnrollmentLifecycle(value) {
		if err != nil {
			return sealed.AVKEnrollmentRequest{}, err
		}
		return sealed.AVKEnrollmentRequest{}, ErrIntegrity
	}
	return request, nil
}

func validSafetyEnrollmentLifecycle(value *client.VaultKeyEnrollment) bool {
	if value == nil {
		return false
	}
	approvedCoordinatesMatch := value.SourceLocationID == "" && value.ApprovedAt == nil ||
		value.SourceLocationID != "" && value.ApprovedAt != nil
	switch value.LifecycleState {
	case client.VaultEnrollmentStatePending:
		return value.SourceLocationID == "" && value.TransferAlgorithm == "" && value.ApprovedAt == nil &&
			value.ConsumedAt == nil && value.CancelledAt == nil && value.ExpiredAt == nil
	case client.VaultEnrollmentStateApproved:
		return value.SourceLocationID != "" && value.TransferAlgorithm == client.VaultEnrollmentTransferAlgorithm &&
			value.ApprovedAt != nil && value.ConsumedAt == nil && value.CancelledAt == nil && value.ExpiredAt == nil
	case client.VaultEnrollmentStateConsumed:
		return value.SourceLocationID != "" && value.TransferAlgorithm == "" && value.ApprovedAt != nil &&
			value.ConsumedAt != nil && value.CancelledAt == nil && value.ExpiredAt == nil
	case client.VaultEnrollmentStateCancelled:
		return approvedCoordinatesMatch && value.TransferAlgorithm == "" && value.ConsumedAt == nil &&
			value.CancelledAt != nil && value.ExpiredAt == nil
	case client.VaultEnrollmentStateExpired:
		return approvedCoordinatesMatch && value.TransferAlgorithm == "" && value.ConsumedAt == nil &&
			value.CancelledAt == nil && value.ExpiredAt != nil
	default:
		return false
	}
}

func sameVaultKeyEnrollmentCoordinates(first, second *client.VaultKeyEnrollment) bool {
	return first != nil && second != nil && first.ID == second.ID && first.AccountID == second.AccountID &&
		first.RealmID == second.RealmID && first.OwnerAgentID == second.OwnerAgentID &&
		first.VaultKeyID == second.VaultKeyID && first.VaultKeyVersion == second.VaultKeyVersion &&
		first.VaultKeyAlgorithm == second.VaultKeyAlgorithm && first.VaultKeyFingerprint == second.VaultKeyFingerprint &&
		first.TargetLocationID == second.TargetLocationID && first.TargetLocationName == second.TargetLocationName &&
		first.TargetPublicKey == second.TargetPublicKey && first.TargetKeyAlgorithm == second.TargetKeyAlgorithm &&
		first.PairingCommitment == second.PairingCommitment && first.SourceLocationID == second.SourceLocationID &&
		first.CreatedAt.Equal(second.CreatedAt) && first.ExpiresAt.Equal(second.ExpiresAt) &&
		sameEnrollmentTime(first.ApprovedAt, second.ApprovedAt)
}

func sameEnrollmentTime(first, second *time.Time) bool {
	return first == nil && second == nil || first != nil && second != nil && first.Equal(*second)
}

func (s *Service) discoverVaultKeyEnrollment(ctx context.Context, identity client.SelfIdentity, binding *client.VaultKeyBinding, targetLocationID string) (*local.AgentVaultKeyEnrollmentState, *client.VaultKeyEnrollment, sealed.AVKEnrollmentRequest, bool, error) {
	ids, err := local.ListAgentVaultKeyEnrollmentStateIDs(s.accountName, s.realmName, s.agentName)
	if err != nil {
		return nil, nil, sealed.AVKEnrollmentRequest{}, false, ErrOperation
	}
	var selected *local.AgentVaultKeyEnrollmentState
	var selectedValue *client.VaultKeyEnrollment
	var selectedRequest sealed.AVKEnrollmentRequest
	selectedActive := false
	fail := func(result error) (*local.AgentVaultKeyEnrollmentState, *client.VaultKeyEnrollment, sealed.AVKEnrollmentRequest, bool, error) {
		if selected != nil {
			selected.Clear()
		}
		return nil, nil, sealed.AVKEnrollmentRequest{}, false, result
	}
	for _, enrollmentID := range ids {
		state, err := local.ReadAgentVaultKeyEnrollmentState(s.accountName, s.realmName, s.agentName, enrollmentID)
		if err != nil {
			return fail(ErrOperation)
		}
		value, getErr := s.remote.getVaultKeyEnrollment(ctx, enrollmentID)
		if getErr != nil && !errors.Is(getErr, client.ErrNotFound) {
			state.Clear()
			return fail(ErrOperation)
		}
		if errors.Is(getErr, client.ErrNotFound) || value == nil {
			request := sealed.AVKEnrollmentRequest{}
			if state.Request != nil {
				request = *state.Request
				if !enrollmentRequestMatchesScope(request, identity, enrollmentID, targetLocationID) {
					state.Clear()
					return fail(ErrIntegrity)
				}
			}
			if selected != nil {
				state.Clear()
				if selectedActive {
					continue
				}
				return fail(ErrOperation)
			}
			selected, selectedRequest = state, request
			continue
		}
		request, err := enrollmentRequestProjection(value, identity)
		if err != nil {
			state.Clear()
			return fail(err)
		}
		if value.TargetLocationID != targetLocationID || value.ID != enrollmentID ||
			(state.Request != nil && *state.Request != request) {
			state.Clear()
			return fail(ErrIntegrity)
		}
		if state.Request == nil {
			if err := local.FinalizeAgentVaultKeyEnrollmentRequest(
				s.accountName, s.realmName, s.agentName, enrollmentID, request,
			); err != nil {
				state.Clear()
				return fail(ErrIntegrity)
			}
			state.Request = &request
		}
		active := value.LifecycleState == client.VaultEnrollmentStatePending ||
			value.LifecycleState == client.VaultEnrollmentStateApproved
		if !active {
			terminalCleanup := value.LifecycleState == client.VaultEnrollmentStateCancelled ||
				value.LifecycleState == client.VaultEnrollmentStateExpired
			state.Clear()
			if terminalCleanup {
				if err := local.DeleteAgentVaultKeyEnrollmentAfterTerminal(
					s.accountName, s.realmName, s.agentName, enrollmentID,
				); err != nil {
					return fail(ErrOperation)
				}
			}
			continue
		}
		if !enrollmentMatchesBinding(value, binding) {
			state.Clear()
			return fail(ErrKeyMismatch)
		}
		if selected != nil {
			if selectedActive {
				state.Clear()
				return fail(ErrOperation)
			}
			selected.Clear()
		}
		selected, selectedValue, selectedRequest, selectedActive = state, value, request, true
	}
	if selected == nil {
		return nil, nil, sealed.AVKEnrollmentRequest{}, false, nil
	}
	return selected, selectedValue, selectedRequest, true, nil
}

func (s *Service) cleanupTerminalEnrollmentState(enrollmentID string, request sealed.AVKEnrollmentRequest) error {
	state, err := local.ReadAgentVaultKeyEnrollmentState(s.accountName, s.realmName, s.agentName, enrollmentID)
	if errors.Is(err, local.ErrAgentVaultKeyEnrollmentUnavailable) {
		return nil
	}
	if err != nil {
		return ErrOperation
	}
	if state.Request == nil {
		state.Clear()
		if err := local.FinalizeAgentVaultKeyEnrollmentRequest(
			s.accountName, s.realmName, s.agentName, enrollmentID, request,
		); err != nil {
			return ErrIntegrity
		}
	} else {
		if *state.Request != request {
			state.Clear()
			return ErrIntegrity
		}
		state.Clear()
	}
	if err := local.DeleteAgentVaultKeyEnrollmentAfterTerminal(
		s.accountName, s.realmName, s.agentName, enrollmentID,
	); err != nil {
		return ErrOperation
	}
	return nil
}

func beginVaultKeyEnrollmentResult(value *client.VaultKeyEnrollment, pairing *sealed.AVKEnrollmentPairingSecret, request sealed.AVKEnrollmentRequest) (*BeginVaultKeyEnrollmentResult, error) {
	if value == nil || pairing == nil {
		return nil, ErrIntegrity
	}
	pairingEncoded, err := sealed.EncodeAVKEnrollmentPairingSecret(pairing)
	if err != nil {
		return nil, ErrOperation
	}
	sas, err := sealed.AVKEnrollmentSAS(pairing, request)
	if err != nil {
		return nil, ErrOperation
	}
	return &BeginVaultKeyEnrollmentResult{Enrollment: *value, PairingSecret: pairingEncoded, SAS: sas}, nil
}

func (s *Service) loadExactEnrollment(ctx context.Context, identity client.SelfIdentity, enrollmentID string) (*client.VaultKeyEnrollment, *client.VaultKeyBinding, sealed.AVKEnrollmentRequest, error) {
	value, err := s.remote.getVaultKeyEnrollment(ctx, enrollmentID)
	if err != nil || value == nil {
		return nil, nil, sealed.AVKEnrollmentRequest{}, ErrOperation
	}
	binding, err := s.currentEnrollmentVaultKey(ctx, identity)
	if err != nil {
		return nil, nil, sealed.AVKEnrollmentRequest{}, err
	}
	request, err := validateExactEnrollment(value, identity, binding)
	if err != nil {
		return nil, nil, sealed.AVKEnrollmentRequest{}, err
	}
	return value, binding, request, nil
}

func (s *Service) currentEnrollmentVaultKey(ctx context.Context, identity client.SelfIdentity) (*client.VaultKeyBinding, error) {
	binding, err := s.remote.currentVaultKey(ctx)
	if err != nil {
		return nil, ErrOperation
	}
	if binding == nil {
		return nil, ErrKeyUnavailable
	}
	if !vaultBindingMatchesIdentity(binding, identity) {
		return nil, ErrIdentityMismatch
	}
	if !validGeneratedID(binding.ID, "avk") || binding.KeyVersion < 1 ||
		binding.Algorithm != sealed.AES256GCMAlgorithm || !validLowerHexSHA256(binding.Fingerprint) ||
		binding.LifecycleState != "current" {
		return nil, ErrIntegrity
	}
	return binding, nil
}

func validateExactEnrollment(value *client.VaultKeyEnrollment, identity client.SelfIdentity, binding *client.VaultKeyBinding) (sealed.AVKEnrollmentRequest, error) {
	request, err := enrollmentRequestProjection(value, identity)
	if err != nil {
		return sealed.AVKEnrollmentRequest{}, err
	}
	if enrollmentRequiresCurrentVaultKey(value.LifecycleState) && !enrollmentMatchesBinding(value, binding) {
		return sealed.AVKEnrollmentRequest{}, ErrKeyMismatch
	}
	return request, nil
}

func enrollmentRequestProjection(value *client.VaultKeyEnrollment, identity client.SelfIdentity) (sealed.AVKEnrollmentRequest, error) {
	if value == nil || !validGeneratedID(value.ID, "enr") || !validGeneratedID(value.VaultKeyID, "avk") ||
		value.VaultKeyVersion < 1 || !validGeneratedID(value.TargetLocationID, "loc") ||
		(value.SourceLocationID != "" && !validGeneratedID(value.SourceLocationID, "loc")) {
		return sealed.AVKEnrollmentRequest{}, ErrIntegrity
	}
	if value.AccountID != identity.AccountID || value.RealmID != identity.RealmID || value.OwnerAgentID != identity.AgentID {
		return sealed.AVKEnrollmentRequest{}, ErrIdentityMismatch
	}
	if value.ExpiresAt.IsZero() || !value.ExpiresAt.Equal(time.Unix(value.ExpiresAt.Unix(), 0)) {
		return sealed.AVKEnrollmentRequest{}, ErrIntegrity
	}
	request := sealed.AVKEnrollmentRequest{
		RequestVersion:             sealed.AVKEnrollmentRequestVersionV1,
		KeyAgreementAlgorithm:      value.TargetKeyAlgorithm,
		PairingCommitmentAlgorithm: sealed.AVKEnrollmentPairingCommitmentAlgorithm,
		SASAlgorithm:               sealed.AVKEnrollmentSASAlgorithm,
		AccountID:                  value.AccountID, RealmID: value.RealmID, OwnerAgentID: value.OwnerAgentID,
		EnrollmentRequestID: value.ID, TargetInstallationID: value.TargetLocationID,
		TargetPublicKey: value.TargetPublicKey, ExpiresAt: value.ExpiresAt.Unix(),
		PairingCommitment: value.PairingCommitment,
	}
	// The public verifier checks every canonical request field without needing
	// private material. A dummy failure is surfaced only as value-free integrity.
	if value.TargetKeyAlgorithm != sealed.AVKEnrollmentKeyAgreementAlgorithm ||
		value.VaultKeyAlgorithm != sealed.AES256GCMAlgorithm || !validLowerHexSHA256(value.VaultKeyFingerprint) ||
		!validLowerHexSHA256(value.PairingCommitment) || !validEnrollmentPublicKey(value.TargetPublicKey) {
		return sealed.AVKEnrollmentRequest{}, ErrIntegrity
	}
	return request, nil
}

func enrollmentMatchesBinding(value *client.VaultKeyEnrollment, binding *client.VaultKeyBinding) bool {
	return value != nil && binding != nil && value.VaultKeyID == binding.ID &&
		value.VaultKeyVersion == binding.KeyVersion && value.VaultKeyAlgorithm == binding.Algorithm &&
		value.VaultKeyFingerprint == binding.Fingerprint
}

func enrollmentRequestMatchesScope(request sealed.AVKEnrollmentRequest, identity client.SelfIdentity, enrollmentID, targetLocationID string) bool {
	return request.AccountID == identity.AccountID && request.RealmID == identity.RealmID &&
		request.OwnerAgentID == identity.AgentID && request.EnrollmentRequestID == enrollmentID &&
		request.TargetInstallationID == targetLocationID &&
		request.RequestVersion == sealed.AVKEnrollmentRequestVersionV1 &&
		request.KeyAgreementAlgorithm == sealed.AVKEnrollmentKeyAgreementAlgorithm &&
		request.PairingCommitmentAlgorithm == sealed.AVKEnrollmentPairingCommitmentAlgorithm &&
		request.SASAlgorithm == sealed.AVKEnrollmentSASAlgorithm && request.ExpiresAt > 0 &&
		validEnrollmentPublicKey(request.TargetPublicKey) && validLowerHexSHA256(request.PairingCommitment)
}

func vaultKeyMatchesEnrollment(key *sealed.AgentVaultKey, value *client.VaultKeyEnrollment, binding *client.VaultKeyBinding, identity client.SelfIdentity) bool {
	metadataMatches := key != nil && value != nil && value.AccountID == identity.AccountID &&
		value.RealmID == identity.RealmID && value.OwnerAgentID == identity.AgentID &&
		value.VaultKeyID == key.ID() && value.VaultKeyVersion == int64(key.Version()) &&
		value.VaultKeyAlgorithm == key.Algorithm() && value.VaultKeyFingerprint == key.Fingerprint()
	if !metadataMatches {
		return false
	}
	return !enrollmentRequiresCurrentVaultKey(value.LifecycleState) || vaultKeyMatches(key, binding, identity)
}

func enrollmentRequiresCurrentVaultKey(state string) bool {
	return state == client.VaultEnrollmentStatePending || state == client.VaultEnrollmentStateApproved
}

func (s *Service) verifyInstalledEnrollmentEpoch(value *client.VaultKeyEnrollment, binding *client.VaultKeyBinding, identity client.SelfIdentity) error {
	if value == nil || value.VaultKeyVersion < 1 {
		return ErrIntegrity
	}
	key, err := local.ReadAgentVaultKeyEpoch(
		s.accountName, s.realmName, s.agentName, value.VaultKeyID, uint64(value.VaultKeyVersion),
	)
	if errors.Is(err, local.ErrAgentVaultKeyUnavailable) {
		return ErrKeyUnavailable
	}
	if err != nil {
		return ErrOperation
	}
	defer key.Clear()
	if !vaultKeyMatchesEnrollment(key, value, binding, identity) {
		return ErrIntegrity
	}
	return nil
}

func normalizedEnrollmentID(value string) (string, bool) {
	value = strings.TrimSpace(value)
	return value, validGeneratedID(value, "enr")
}

func enrollmentMutationKey(operation, enrollmentID string) string {
	return "witself-enrollment-" + operation + "-" + enrollmentID
}

func validLowerHexSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func validEnrollmentPublicKey(value string) bool {
	if len(value) != base64.RawURLEncoding.EncodedLen(sealed.AVKEnrollmentPublicKeyBytes) {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(raw) != sealed.AVKEnrollmentPublicKeyBytes {
		return false
	}
	return base64.RawURLEncoding.EncodeToString(raw) == value
}
