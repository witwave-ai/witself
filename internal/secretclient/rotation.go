package secretclient

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash"
	"math"
	"strings"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/id"
	"github.com/witwave-ai/witself/internal/local"
	"github.com/witwave-ai/witself/internal/sealed"
)

const vaultKeyRotationPageSize = 100

// vaultKeyRotationRemote is deliberately separate from remote so existing
// secret operations and provider adapters do not acquire rotation methods they
// never use. httpRemote implements both interfaces.
type vaultKeyRotationRemote interface {
	getOpenVaultKeyRotation(context.Context) (*client.VaultKeyRotation, error)
	getVaultKeyRotation(context.Context, string) (*client.VaultKeyRotation, error)
	listVaultKeyRotationItems(context.Context, string, client.VaultKeyRotationItemListOptions) (*client.VaultKeyRotationItemPage, error)
	startVaultKeyRotation(context.Context, client.StartVaultKeyRotationInput) (*client.VaultKeyRotationMutationResult, error)
	stageVaultKeyRotation(context.Context, string, client.StageVaultKeyRotationInput) (*client.VaultKeyRotationMutationResult, error)
	commitVaultKeyRotation(context.Context, string, client.CommitVaultKeyRotationInput) (*client.VaultKeyRotationMutationResult, error)
	cancelVaultKeyRotation(context.Context, string, client.CancelVaultKeyRotationInput) (*client.VaultKeyRotationMutationResult, error)
}

// RotateVaultKey starts or resumes the authenticated agent's one open AVK
// rotation. Every DEK remains opaque: the client authenticates and rewraps the
// DEK locally, the backend stages only encrypted wrappers, and commit is fenced
// by the exact fully staged plan hash. Both old and new local AVK epochs remain
// on disk after commit.
func (s *Service) RotateVaultKey(ctx context.Context, options RotateVaultKeyOptions) (*client.VaultKeyRotation, error) {
	if err := options.validate(); err != nil {
		return nil, err
	}
	identity, err := s.identity(ctx)
	if err != nil {
		return nil, err
	}
	remote, err := s.vaultKeyRotationRemote()
	if err != nil {
		return nil, err
	}
	_, rotation, err := s.resolveVaultKeyRotationIntent(ctx, remote, identity)
	if err != nil {
		return nil, err
	}

	source, target, err := s.loadVaultKeyRotationKeys(rotation)
	if err != nil {
		return nil, err
	}
	defer source.Clear()
	defer target.Clear()
	if err := validateVaultKeyRotationKeys(rotation, source, target); err != nil {
		return nil, err
	}
	if rotation.LifecycleState == client.VaultKeyRotationCommitted {
		if err := s.validateVaultKeyRotationCurrentBinding(ctx, identity, rotation, target); err != nil {
			return nil, err
		}
		return rotation, nil
	}
	if err := s.validateVaultKeyRotationCurrentBinding(ctx, identity, rotation, source); err != nil {
		return nil, err
	}
	if rotation.LifecycleState == client.VaultKeyRotationCancelled {
		return rotation, nil
	}
	if rotation.LifecycleState != client.VaultKeyRotationOpen {
		return nil, ErrIntegrity
	}

	rotation, err = s.stageAllVaultKeyRotationItems(ctx, remote, identity, rotation, source, target)
	if err != nil {
		return nil, err
	}
	// Refresh the exact run before the independent final pass. Its revision and
	// plan hash form the commit fence; no earlier mutation response is trusted
	// as a complete snapshot.
	rotation, err = remote.getVaultKeyRotation(ctx, rotation.ID)
	if err != nil || rotation == nil {
		return nil, ErrOperation
	}
	if err := validateVaultKeyRotationIdentity(rotation, identity); err != nil {
		return nil, err
	}
	if err := validateVaultKeyRotationKeys(rotation, source, target); err != nil {
		return nil, err
	}
	if rotation.LifecycleState != client.VaultKeyRotationOpen ||
		rotation.StagedCount != rotation.ItemCount || !validVaultKeyRotationHash(rotation.StagedPlanHash) {
		return nil, ErrIntegrity
	}
	if err := verifyAllVaultKeyRotationItems(ctx, remote, identity, rotation, source, target); err != nil {
		return nil, err
	}
	recovery, err := s.prepareVaultKeyRotationRecovery(ctx, identity, target, options)
	if err != nil {
		return nil, err
	}
	return s.commitVaultKeyRotation(ctx, remote, identity, rotation, target, recovery)
}

// OpenVaultKeyRotation returns the canonical open rotation or nil when there
// is none. It performs no key generation, staging, cancellation, or commit.
func (s *Service) OpenVaultKeyRotation(ctx context.Context) (*client.VaultKeyRotation, error) {
	remote, err := s.vaultKeyRotationRemote()
	if err != nil {
		return nil, err
	}
	rotation, err := remote.getOpenVaultKeyRotation(ctx)
	if err != nil {
		return nil, ErrOperation
	}
	if rotation == nil {
		return nil, nil
	}
	if _, err := s.validateVaultKeyRotationConfiguredScope(rotation); err != nil {
		return nil, err
	}
	return rotation, nil
}

// VaultKeyRotationStatus reads one exact value-free rotation lifecycle record.
func (s *Service) VaultKeyRotationStatus(ctx context.Context, rotationID string) (*client.VaultKeyRotation, error) {
	rotationID = strings.TrimSpace(rotationID)
	if !validGeneratedID(rotationID, "vkr") {
		return nil, ErrInvalidInput
	}
	remote, err := s.vaultKeyRotationRemote()
	if err != nil {
		return nil, err
	}
	rotation, err := remote.getVaultKeyRotation(ctx, rotationID)
	if err != nil || rotation == nil {
		return nil, ErrOperation
	}
	if _, err := s.validateVaultKeyRotationConfiguredScope(rotation); err != nil {
		return nil, err
	}
	return rotation, nil
}

// CancelVaultKeyRotation cancels one open run without reading either AVK. An
// empty rotationID selects the canonical open run; nil,nil means none exists.
// A lost response is resolved through the exact lifecycle record before any
// retry, using the same idempotency key and optimistic revision.
func (s *Service) CancelVaultKeyRotation(ctx context.Context, rotationID string) (*client.VaultKeyRotation, error) {
	rotationID = strings.TrimSpace(rotationID)
	if rotationID != "" && !validGeneratedID(rotationID, "vkr") {
		return nil, ErrInvalidInput
	}
	remote, err := s.vaultKeyRotationRemote()
	if err != nil {
		return nil, err
	}
	var rotation *client.VaultKeyRotation
	intent, intentErr := local.ReadAgentVaultKeyRotationIntent(s.accountName, s.realmName, s.agentName)
	switch {
	case intentErr == nil:
		preparedIntent := intent.ExpectedSourceKeyRowVersion > 0 && intent.StartIdempotencyKey != ""
		requestedDifferentRotation := rotationID != "" && rotationID != intent.RotationID
		if requestedDifferentRotation && !preparedIntent {
			return nil, ErrIntegrity
		}
		rotation, err = remote.getVaultKeyRotation(ctx, intent.RotationID)
		if errors.Is(err, client.ErrNotFound) && preparedIntent {
			rotation, err = remote.getOpenVaultKeyRotation(ctx)
			if err != nil || rotation == nil {
				return nil, ErrOperation
			}
			if rotationID != "" && rotation.ID != rotationID {
				return nil, ErrIntegrity
			}
			if _, err := s.validateVaultKeyRotationConfiguredScope(rotation); err != nil {
				return nil, err
			}
			if err := s.replacePreparedVaultKeyRotationIntentWithCanonicalOpen(intent, rotation); err != nil {
				return nil, err
			}
		} else if err != nil || rotation == nil {
			return nil, ErrOperation
		} else if requestedDifferentRotation {
			return nil, ErrIntegrity
		}
		if !vaultKeyRotationMatchesIntent(rotation, intent) {
			currentIntent, readErr := local.ReadAgentVaultKeyRotationIntent(s.accountName, s.realmName, s.agentName)
			if readErr != nil || !vaultKeyRotationMatchesIntent(rotation, currentIntent) {
				return nil, ErrIntegrity
			}
		}
	case errors.Is(intentErr, local.ErrAgentVaultKeyRotationIntentUnavailable):
		if rotationID == "" {
			rotation, err = remote.getOpenVaultKeyRotation(ctx)
		} else {
			rotation, err = remote.getVaultKeyRotation(ctx, rotationID)
		}
		if err != nil {
			return nil, ErrOperation
		}
	default:
		return nil, mapVaultKeyRotationIntentError(intentErr)
	}
	if rotation == nil {
		return nil, nil
	}
	identity, err := s.validateVaultKeyRotationConfiguredScope(rotation)
	if err != nil {
		return nil, err
	}
	switch rotation.LifecycleState {
	case client.VaultKeyRotationCancelled:
		return rotation, nil
	case client.VaultKeyRotationCommitted:
		// A commit may win the race with cancellation. Returning the exact
		// immutable terminal record lets the CLI report what happened and then
		// acknowledge its local journal instead of stranding that installation.
		return rotation, nil
	case client.VaultKeyRotationOpen:
	default:
		return nil, ErrIntegrity
	}
	if err := s.adoptVaultKeyRotationIntent(rotation); err != nil {
		return nil, err
	}
	idempotencyKey, err := operationKey("")
	if err != nil {
		return nil, err
	}
	input := client.CancelVaultKeyRotationInput{
		ExpectedRotationRowVersion: rotation.RowVersion,
		IdempotencyKey:             idempotencyKey,
	}
	result, mutationErr := remote.cancelVaultKeyRotation(ctx, rotation.ID, input)
	if mutationErr == nil {
		return validateCancelledVaultKeyRotationResult(result, identity, rotation)
	}
	observed, getErr := remote.getVaultKeyRotation(ctx, rotation.ID)
	if getErr != nil || observed == nil {
		return nil, ErrOperation
	}
	if err := validateVaultKeyRotationIdentity(observed, identity); err != nil {
		return nil, err
	}
	if !sameVaultKeyRotationEpochs(rotation, observed) {
		return nil, ErrIntegrity
	}
	if observed.LifecycleState == client.VaultKeyRotationCancelled {
		return observed, nil
	}
	if observed.LifecycleState == client.VaultKeyRotationCommitted {
		return observed, nil
	}
	if observed.LifecycleState != client.VaultKeyRotationOpen || observed.RowVersion != rotation.RowVersion {
		return nil, ErrOperation
	}
	result, mutationErr = remote.cancelVaultKeyRotation(ctx, rotation.ID, input)
	if mutationErr == nil {
		return validateCancelledVaultKeyRotationResult(result, identity, rotation)
	}
	observed, getErr = remote.getVaultKeyRotation(ctx, rotation.ID)
	if getErr != nil || observed == nil {
		return nil, ErrOperation
	}
	if err := validateVaultKeyRotationIdentity(observed, identity); err != nil {
		return nil, err
	}
	if sameVaultKeyRotationEpochs(rotation, observed) &&
		(observed.LifecycleState == client.VaultKeyRotationCancelled ||
			observed.LifecycleState == client.VaultKeyRotationCommitted) {
		return observed, nil
	}
	return nil, ErrOperation
}

// AcknowledgeVaultKeyRotation removes the exact crash-recovery intent only
// after the caller has durably handled a canonical terminal result. It is
// intentionally separate from RotateVaultKey: a process crash may duplicate a
// terminal display, but can never silently begin a second rotation.
func (s *Service) AcknowledgeVaultKeyRotation(ctx context.Context, rotationID string) error {
	rotationID = strings.TrimSpace(rotationID)
	if !validGeneratedID(rotationID, "vkr") {
		return ErrInvalidInput
	}
	intent, err := local.ReadAgentVaultKeyRotationIntent(s.accountName, s.realmName, s.agentName)
	if errors.Is(err, local.ErrAgentVaultKeyRotationIntentUnavailable) {
		return nil
	}
	if err != nil {
		return mapVaultKeyRotationIntentError(err)
	}
	if intent.RotationID != rotationID {
		return ErrIntegrity
	}
	remote, err := s.vaultKeyRotationRemote()
	if err != nil {
		return err
	}
	rotation, err := remote.getVaultKeyRotation(ctx, rotationID)
	if err != nil || rotation == nil {
		return ErrOperation
	}
	if _, err := s.validateVaultKeyRotationConfiguredScope(rotation); err != nil {
		return err
	}
	if !vaultKeyRotationMatchesIntent(rotation, intent) {
		return ErrIntegrity
	}
	if rotation.LifecycleState != client.VaultKeyRotationCommitted &&
		rotation.LifecycleState != client.VaultKeyRotationCancelled {
		return ErrOperation
	}
	if err := local.DeleteAgentVaultKeyRotationIntentAfterAcknowledge(
		s.accountName, s.realmName, s.agentName, rotationID,
	); err != nil {
		return mapVaultKeyRotationIntentError(err)
	}
	return nil
}

func (s *Service) resolveVaultKeyRotationIntent(ctx context.Context, remote vaultKeyRotationRemote, identity client.SelfIdentity) (*local.AgentVaultKeyRotationIntent, *client.VaultKeyRotation, error) {
	for attempt := 0; attempt < 3; attempt++ {
		intent, err := local.ReadAgentVaultKeyRotationIntent(s.accountName, s.realmName, s.agentName)
		switch {
		case err == nil:
			if err := validateVaultKeyRotationIntentIdentity(intent, identity); err != nil {
				return nil, nil, err
			}
			rotation, err := s.reconcileVaultKeyRotationIntent(ctx, remote, identity, intent)
			return intent, rotation, err
		case !errors.Is(err, local.ErrAgentVaultKeyRotationIntentUnavailable):
			return nil, nil, mapVaultKeyRotationIntentError(err)
		}

		open, err := remote.getOpenVaultKeyRotation(ctx)
		if err != nil {
			return nil, nil, ErrOperation
		}
		if open != nil {
			if err := validateVaultKeyRotationIdentity(open, identity); err != nil {
				return nil, nil, err
			}
			intent = vaultKeyRotationIntentFromRotation(open, false, 0, "")
		} else {
			intent, err = s.prepareVaultKeyRotationIntent(ctx, identity)
			if err != nil {
				return nil, nil, err
			}
		}
		err = local.CreateAgentVaultKeyRotationIntent(s.accountName, s.realmName, s.agentName, *intent)
		if errors.Is(err, local.ErrAgentVaultKeyRotationIntentExists) {
			continue
		}
		if err != nil {
			return nil, nil, mapVaultKeyRotationIntentError(err)
		}
		rotation, err := s.reconcileVaultKeyRotationIntent(ctx, remote, identity, intent)
		return intent, rotation, err
	}
	return nil, nil, ErrOperation
}

func (s *Service) prepareVaultKeyRotationIntent(ctx context.Context, identity client.SelfIdentity) (*local.AgentVaultKeyRotationIntent, error) {
	observation, err := s.observeVaultKey(ctx, identity)
	if err != nil {
		return nil, err
	}
	defer observation.local.Clear()
	switch observation.status.State {
	case VaultKeyStateMatch:
	case VaultKeyStateBackendOnly, VaultKeyStateAbsent:
		return nil, ErrKeyUnavailable
	case VaultKeyStateMismatch, VaultKeyStateLocalOnly:
		return nil, ErrKeyMismatch
	default:
		return nil, ErrOperation
	}
	if observation.local == nil || observation.backend == nil || observation.backend.RowVersion < 1 ||
		observation.local.Version() >= math.MaxInt64 {
		return nil, ErrIntegrity
	}
	// Ensure the current source remains readable through the epoch-aware local
	// API before creating its successor. A matching legacy v1 file remains a
	// supported immutable source and is never rewritten merely for rotation.
	if err := s.publishVaultKeyEpoch(observation.local); err != nil {
		return nil, err
	}
	target, err := sealed.GenerateAgentVaultKey(observation.local.Version() + 1)
	if err != nil {
		return nil, ErrOperation
	}
	defer target.Clear()
	if err := local.CreateAgentVaultKeyEpoch(s.accountName, s.realmName, s.agentName, target); err != nil {
		if !errors.Is(err, local.ErrAgentVaultKeyExists) {
			return nil, ErrOperation
		}
		existing, readErr := local.ReadAgentVaultKeyEpoch(
			s.accountName, s.realmName, s.agentName, target.ID(), target.Version(),
		)
		if readErr != nil {
			return nil, ErrOperation
		}
		matches := existing.Metadata() == target.Metadata()
		existing.Clear()
		if !matches {
			return nil, ErrKeyMismatch
		}
	}
	rotationID, err := id.New("vkr")
	if err != nil {
		return nil, ErrOperation
	}
	idempotencyKey, err := operationKey("")
	if err != nil {
		return nil, err
	}
	return &local.AgentVaultKeyRotationIntent{
		RotationID: rotationID, AccountID: identity.AccountID, RealmID: identity.RealmID,
		OwnerAgentID: identity.AgentID, Source: observation.local.Metadata(), Target: target.Metadata(),
		ExpectedSourceKeyRowVersion: observation.backend.RowVersion,
		StartIdempotencyKey:         idempotencyKey,
	}, nil
}

func (s *Service) reconcileVaultKeyRotationIntent(ctx context.Context, remote vaultKeyRotationRemote, identity client.SelfIdentity, intent *local.AgentVaultKeyRotationIntent) (*client.VaultKeyRotation, error) {
	rotation, getErr := remote.getVaultKeyRotation(ctx, intent.RotationID)
	if getErr == nil && rotation != nil {
		if err := validateVaultKeyRotationIdentity(rotation, identity); err != nil {
			return nil, err
		}
		if !vaultKeyRotationMatchesIntent(rotation, intent) {
			return nil, ErrIntegrity
		}
		return rotation, nil
	}
	if getErr == nil || !errors.Is(getErr, client.ErrNotFound) {
		return nil, ErrOperation
	}
	if intent.ExpectedSourceKeyRowVersion < 1 || intent.StartIdempotencyKey == "" {
		return nil, ErrOperation
	}
	source, target, err := s.loadVaultKeyRotationIntentKeys(intent)
	if err != nil {
		return nil, err
	}
	defer source.Clear()
	defer target.Clear()
	if err := validateVaultKeyRotationIntentKeys(intent, source, target); err != nil {
		return nil, err
	}
	currentMetadata, err := s.currentVaultKeyMetadata(ctx, identity)
	if err != nil {
		return nil, err
	}
	if currentMetadata != intent.Source {
		if currentMetadata.Version <= intent.Source.Version {
			return nil, ErrKeyMismatch
		}
		// The exact prepared ID is absent, but the authenticated current epoch
		// has advanced. This pristine intent never became canonical. Adopt a
		// newer open run only when its source is that exact current epoch;
		// otherwise retire the stale local fence and require a fresh invocation.
		open, openErr := remote.getOpenVaultKeyRotation(ctx)
		if openErr != nil {
			return nil, ErrOperation
		}
		if open != nil {
			if err := validateVaultKeyRotationIdentity(open, identity); err != nil {
				return nil, err
			}
			if vaultKeyRotationSourceMetadata(open) != currentMetadata {
				return nil, ErrIntegrity
			}
			if err := s.replacePreparedVaultKeyRotationIntentWithCanonicalOpen(intent, open); err != nil {
				return nil, err
			}
			return open, nil
		}
		if err := local.RetireAgentVaultKeyRotationIntentAfterCanonicalAdvance(
			s.accountName, s.realmName, s.agentName, *intent,
		); err != nil {
			if errors.Is(err, local.ErrAgentVaultKeyRotationIntentConflict) {
				current, readErr := local.ReadAgentVaultKeyRotationIntent(s.accountName, s.realmName, s.agentName)
				if errors.Is(readErr, local.ErrAgentVaultKeyRotationIntentUnavailable) {
					return nil, ErrKeyMismatch
				}
				if readErr == nil && current != nil {
					return nil, ErrIntegrity
				}
			}
			return nil, mapVaultKeyRotationIntentError(err)
		}
		return nil, ErrKeyMismatch
	}
	open, err := remote.getOpenVaultKeyRotation(ctx)
	if err != nil {
		return nil, ErrOperation
	}
	if open != nil {
		if err := validateVaultKeyRotationIdentity(open, identity); err != nil {
			return nil, err
		}
		if !vaultKeyRotationMatchesIntent(open, intent) {
			if err := s.replacePreparedVaultKeyRotationIntentWithCanonicalOpen(intent, open); err != nil {
				return nil, err
			}
		}
		return open, nil
	}
	input := vaultKeyRotationStartInput(intent)
	result, startErr := remote.startVaultKeyRotation(ctx, input)
	if startErr == nil {
		return validateStartedVaultKeyRotationResult(result, identity, intent.RotationID, source, target)
	}
	rotation, getErr = remote.getVaultKeyRotation(ctx, intent.RotationID)
	if getErr == nil && rotation != nil {
		if err := validateVaultKeyRotationIdentity(rotation, identity); err != nil {
			return nil, err
		}
		if !vaultKeyRotationMatchesIntent(rotation, intent) {
			return nil, ErrIntegrity
		}
		return rotation, nil
	}
	if getErr == nil || !errors.Is(getErr, client.ErrNotFound) {
		return nil, ErrOperation
	}
	open, openErr := remote.getOpenVaultKeyRotation(ctx)
	if openErr != nil {
		return nil, ErrOperation
	}
	if open != nil {
		if err := validateVaultKeyRotationIdentity(open, identity); err != nil {
			return nil, err
		}
		if !vaultKeyRotationMatchesIntent(open, intent) {
			if err := s.replacePreparedVaultKeyRotationIntentWithCanonicalOpen(intent, open); err != nil {
				return nil, err
			}
		}
		return open, nil
	}
	result, startErr = remote.startVaultKeyRotation(ctx, input)
	if startErr == nil {
		return validateStartedVaultKeyRotationResult(result, identity, intent.RotationID, source, target)
	}
	rotation, getErr = remote.getVaultKeyRotation(ctx, intent.RotationID)
	if errors.Is(getErr, client.ErrNotFound) {
		open, openErr = remote.getOpenVaultKeyRotation(ctx)
		if openErr != nil || open == nil {
			return nil, ErrOperation
		}
		if err := validateVaultKeyRotationIdentity(open, identity); err != nil {
			return nil, err
		}
		if !vaultKeyRotationMatchesIntent(open, intent) {
			if err := s.replacePreparedVaultKeyRotationIntentWithCanonicalOpen(intent, open); err != nil {
				return nil, err
			}
		}
		return open, nil
	}
	if getErr != nil || rotation == nil {
		return nil, ErrOperation
	}
	if err := validateVaultKeyRotationIdentity(rotation, identity); err != nil {
		return nil, err
	}
	if !vaultKeyRotationMatchesIntent(rotation, intent) {
		return nil, ErrIntegrity
	}
	return rotation, nil
}

func (s *Service) adoptVaultKeyRotationIntent(rotation *client.VaultKeyRotation) error {
	intent := vaultKeyRotationIntentFromRotation(rotation, false, 0, "")
	err := local.CreateAgentVaultKeyRotationIntent(s.accountName, s.realmName, s.agentName, *intent)
	if err == nil {
		return nil
	}
	if !errors.Is(err, local.ErrAgentVaultKeyRotationIntentExists) {
		return mapVaultKeyRotationIntentError(err)
	}
	existing, readErr := local.ReadAgentVaultKeyRotationIntent(s.accountName, s.realmName, s.agentName)
	if readErr != nil {
		return mapVaultKeyRotationIntentError(readErr)
	}
	if !vaultKeyRotationMatchesIntent(rotation, existing) {
		return ErrIntegrity
	}
	return nil
}

func (s *Service) replacePreparedVaultKeyRotationIntentWithCanonicalOpen(expected *local.AgentVaultKeyRotationIntent, open *client.VaultKeyRotation) error {
	if expected == nil || open == nil || open.LifecycleState != client.VaultKeyRotationOpen ||
		expected.ExpectedSourceKeyRowVersion < 1 || expected.StartIdempotencyKey == "" {
		return ErrIntegrity
	}
	replacement := vaultKeyRotationIntentFromRotation(open, false, 0, "")
	if replacement.AccountID != expected.AccountID || replacement.RealmID != expected.RealmID ||
		replacement.OwnerAgentID != expected.OwnerAgentID ||
		(replacement.Source != expected.Source && replacement.Source.Version <= expected.Source.Version) {
		return ErrIntegrity
	}
	err := local.ReplaceAgentVaultKeyRotationIntentAfterCanonicalConflict(
		s.accountName, s.realmName, s.agentName, *expected, *replacement,
	)
	if err == nil {
		return nil
	}
	if !errors.Is(err, local.ErrAgentVaultKeyRotationIntentConflict) {
		return mapVaultKeyRotationIntentError(err)
	}
	// Another local process may have converged first. Accept only that exact
	// canonical result; never overwrite or delete a different intent.
	current, readErr := local.ReadAgentVaultKeyRotationIntent(s.accountName, s.realmName, s.agentName)
	if readErr != nil || !vaultKeyRotationMatchesIntent(open, current) {
		return ErrIntegrity
	}
	return nil
}

func (s *Service) loadVaultKeyRotationIntentKeys(intent *local.AgentVaultKeyRotationIntent) (*sealed.AgentVaultKey, *sealed.AgentVaultKey, error) {
	if intent == nil {
		return nil, nil, ErrIntegrity
	}
	source, err := local.ReadAgentVaultKeyEpoch(s.accountName, s.realmName, s.agentName,
		intent.Source.ID, intent.Source.Version)
	if errors.Is(err, local.ErrAgentVaultKeyUnavailable) {
		return nil, nil, ErrKeyUnavailable
	}
	if err != nil {
		return nil, nil, ErrOperation
	}
	target, err := local.ReadAgentVaultKeyEpoch(s.accountName, s.realmName, s.agentName,
		intent.Target.ID, intent.Target.Version)
	if errors.Is(err, local.ErrAgentVaultKeyUnavailable) {
		source.Clear()
		return nil, nil, ErrKeyUnavailable
	}
	if err != nil {
		source.Clear()
		return nil, nil, ErrOperation
	}
	return source, target, nil
}

func (s *Service) currentVaultKeyMetadata(ctx context.Context, identity client.SelfIdentity) (sealed.AgentVaultKeyMetadata, error) {
	binding, err := s.remote.currentVaultKey(ctx)
	if err != nil || binding == nil {
		return sealed.AgentVaultKeyMetadata{}, ErrOperation
	}
	if !vaultBindingMatchesIdentity(binding, identity) {
		return sealed.AgentVaultKeyMetadata{}, ErrIdentityMismatch
	}
	if !validGeneratedID(binding.ID, "avk") || binding.KeyVersion < 1 || binding.RowVersion < 1 ||
		binding.Algorithm != sealed.AES256GCMAlgorithm || !validVaultKeyRotationHash(binding.Fingerprint) ||
		binding.LifecycleState != "current" || binding.RetiredAt != nil {
		return sealed.AgentVaultKeyMetadata{}, ErrIntegrity
	}
	return sealed.AgentVaultKeyMetadata{
		ID: binding.ID, Version: uint64(binding.KeyVersion),
		Algorithm: binding.Algorithm, Fingerprint: binding.Fingerprint,
	}, nil
}

func vaultKeyRotationSourceMetadata(rotation *client.VaultKeyRotation) sealed.AgentVaultKeyMetadata {
	if rotation == nil || rotation.SourceKeyVersion < 1 {
		return sealed.AgentVaultKeyMetadata{}
	}
	return sealed.AgentVaultKeyMetadata{
		ID: rotation.SourceKeyID, Version: uint64(rotation.SourceKeyVersion),
		Algorithm: rotation.SourceKeyAlgorithm, Fingerprint: rotation.SourceKeyFingerprint,
	}
}

func (s *Service) validateVaultKeyRotationConfiguredScope(rotation *client.VaultKeyRotation) (client.SelfIdentity, error) {
	if rotation == nil {
		return client.SelfIdentity{}, ErrIntegrity
	}
	if rotation.AccountID != s.accountID {
		return client.SelfIdentity{}, ErrIdentityMismatch
	}
	identity := client.SelfIdentity{
		AccountID: rotation.AccountID,
		RealmID:   rotation.RealmID,
		AgentID:   rotation.OwnerAgentID,
	}
	if err := validateVaultKeyRotationIdentity(rotation, identity); err != nil {
		return client.SelfIdentity{}, err
	}
	return identity, nil
}

func validateVaultKeyRotationIntentIdentity(intent *local.AgentVaultKeyRotationIntent, identity client.SelfIdentity) error {
	if intent == nil {
		return ErrIntegrity
	}
	if intent.AccountID != identity.AccountID || intent.RealmID != identity.RealmID ||
		intent.OwnerAgentID != identity.AgentID {
		return ErrIdentityMismatch
	}
	return nil
}

func validateVaultKeyRotationIntentKeys(intent *local.AgentVaultKeyRotationIntent, source, target *sealed.AgentVaultKey) error {
	if intent == nil || source == nil || target == nil || source.Metadata() != intent.Source ||
		target.Metadata() != intent.Target {
		return ErrKeyMismatch
	}
	return nil
}

func vaultKeyRotationIntentFromRotation(rotation *client.VaultKeyRotation, prepared bool, sourceRowVersion int64, idempotencyKey string) *local.AgentVaultKeyRotationIntent {
	if rotation == nil {
		return nil
	}
	intent := &local.AgentVaultKeyRotationIntent{
		RotationID: rotation.ID, AccountID: rotation.AccountID, RealmID: rotation.RealmID,
		OwnerAgentID: rotation.OwnerAgentID,
		Source: sealed.AgentVaultKeyMetadata{
			ID: rotation.SourceKeyID, Version: uint64(rotation.SourceKeyVersion),
			Algorithm: rotation.SourceKeyAlgorithm, Fingerprint: rotation.SourceKeyFingerprint,
		},
		Target: sealed.AgentVaultKeyMetadata{
			ID: rotation.TargetKeyID, Version: uint64(rotation.TargetKeyVersion),
			Algorithm: rotation.TargetKeyAlgorithm, Fingerprint: rotation.TargetKeyFingerprint,
		},
	}
	if prepared {
		intent.ExpectedSourceKeyRowVersion = sourceRowVersion
		intent.StartIdempotencyKey = idempotencyKey
	}
	return intent
}

func vaultKeyRotationMatchesIntent(rotation *client.VaultKeyRotation, intent *local.AgentVaultKeyRotationIntent) bool {
	if rotation == nil || intent == nil || rotation.SourceKeyVersion < 1 || rotation.TargetKeyVersion < 1 {
		return false
	}
	return rotation.ID == intent.RotationID && rotation.AccountID == intent.AccountID &&
		rotation.RealmID == intent.RealmID && rotation.OwnerAgentID == intent.OwnerAgentID &&
		rotation.SourceKeyID == intent.Source.ID && uint64(rotation.SourceKeyVersion) == intent.Source.Version &&
		rotation.SourceKeyAlgorithm == intent.Source.Algorithm &&
		rotation.SourceKeyFingerprint == intent.Source.Fingerprint &&
		rotation.TargetKeyID == intent.Target.ID && uint64(rotation.TargetKeyVersion) == intent.Target.Version &&
		rotation.TargetKeyAlgorithm == intent.Target.Algorithm &&
		rotation.TargetKeyFingerprint == intent.Target.Fingerprint
}

func vaultKeyRotationStartInput(intent *local.AgentVaultKeyRotationIntent) client.StartVaultKeyRotationInput {
	return client.StartVaultKeyRotationInput{
		ID:                  intent.RotationID,
		ExpectedSourceKeyID: intent.Source.ID, ExpectedSourceKeyVersion: int64(intent.Source.Version),
		ExpectedSourceKeyRowVersion: intent.ExpectedSourceKeyRowVersion,
		TargetKeyID:                 intent.Target.ID, TargetKeyVersion: int64(intent.Target.Version),
		TargetAlgorithm: intent.Target.Algorithm, TargetFingerprint: intent.Target.Fingerprint,
		IdempotencyKey: intent.StartIdempotencyKey,
	}
}

func mapVaultKeyRotationIntentError(err error) error {
	switch {
	case errors.Is(err, local.ErrAgentVaultKeyRotationIntentInvalid),
		errors.Is(err, local.ErrAgentVaultKeyRotationIntentUnsafe),
		errors.Is(err, local.ErrAgentVaultKeyRotationIntentConflict),
		errors.Is(err, local.ErrAgentVaultKeyRotationIntentScope):
		return ErrIntegrity
	default:
		return ErrOperation
	}
}

func (s *Service) stageAllVaultKeyRotationItems(ctx context.Context, remote vaultKeyRotationRemote, identity client.SelfIdentity, rotation *client.VaultKeyRotation, source, target *sealed.AgentVaultKey) (*client.VaultKeyRotation, error) {
	for {
		cursor := ""
		seenCursors := map[string]bool{"": true}
		seenDEKs := make(map[string]bool)
		lastDEKID := ""
		var itemCount int64
		restart := false
		for {
			page, err := remote.listVaultKeyRotationItems(ctx, rotation.ID, client.VaultKeyRotationItemListOptions{
				Limit: vaultKeyRotationPageSize, Cursor: cursor,
			})
			if err != nil || page == nil || len(page.Items) > vaultKeyRotationPageSize {
				return nil, ErrOperation
			}
			stageItems := make([]client.StageVaultKeyRotationItemInput, 0, len(page.Items))
			for index := range page.Items {
				item := &page.Items[index]
				if seenDEKs[item.DEKID] || (lastDEKID != "" && item.DEKID <= lastDEKID) {
					clearVaultKeyRotationStageItems(stageItems)
					return nil, ErrIntegrity
				}
				seenDEKs[item.DEKID] = true
				lastDEKID = item.DEKID
				itemCount++
				sourceWrapper, scope, err := vaultKeyRotationSourceWrapper(identity, rotation, item)
				if err != nil {
					clearVaultKeyRotationStageItems(stageItems)
					return nil, err
				}
				if item.StagedAt != nil {
					targetWrapper, err := vaultKeyRotationTargetWrapper(rotation, item)
					if err != nil || sealed.VerifyDEKRewrap(source, target, scope, sourceWrapper, targetWrapper) != nil {
						clear(targetWrapper.WrappedDEK)
						clearVaultKeyRotationStageItems(stageItems)
						return nil, ErrIntegrity
					}
					clear(targetWrapper.WrappedDEK)
					continue
				}
				targetWrapper, err := sealed.RewrapDEK(source, target, scope, sourceWrapper, uint64(item.SourceWrapRevision+1))
				if err != nil {
					clearVaultKeyRotationStageItems(stageItems)
					return nil, ErrIntegrity
				}
				stageItems = append(stageItems, client.StageVaultKeyRotationItemInput{
					DEKID: item.DEKID, ExpectedSourceDEKRowVersion: item.SourceDEKRowVersion,
					ExpectedSourceWrapRevision: item.SourceWrapRevision,
					TargetWrappedDEK:           append([]byte(nil), targetWrapper.WrappedDEK...),
					TargetWrapRevision:         int64(targetWrapper.WrapRevision),
				})
				clear(targetWrapper.WrappedDEK)
			}
			if len(stageItems) > 0 {
				beforeStaged := rotation.StagedCount
				next, ambiguous, err := stageVaultKeyRotationBatch(ctx, remote, identity, rotation, stageItems)
				clearVaultKeyRotationStageItems(stageItems)
				if err != nil {
					return nil, err
				}
				rotation = next
				if err := validateVaultKeyRotationKeys(rotation, source, target); err != nil {
					return nil, err
				}
				if ambiguous {
					if rotation.StagedCount <= beforeStaged {
						return nil, ErrOperation
					}
					restart = true
					break
				}
			}
			if page.NextCursor == "" {
				break
			}
			if seenCursors[page.NextCursor] {
				return nil, ErrIntegrity
			}
			seenCursors[page.NextCursor] = true
			cursor = page.NextCursor
		}
		if restart {
			continue
		}
		if itemCount != rotation.ItemCount || rotation.StagedCount != rotation.ItemCount ||
			!validVaultKeyRotationHash(rotation.StagedPlanHash) {
			return nil, ErrIntegrity
		}
		return rotation, nil
	}
}

func stageVaultKeyRotationBatch(ctx context.Context, remote vaultKeyRotationRemote, identity client.SelfIdentity, rotation *client.VaultKeyRotation, items []client.StageVaultKeyRotationItemInput) (*client.VaultKeyRotation, bool, error) {
	idempotencyKey, err := operationKey("")
	if err != nil {
		return nil, false, err
	}
	input := client.StageVaultKeyRotationInput{
		ExpectedRotationRowVersion: rotation.RowVersion,
		Items:                      items,
		IdempotencyKey:             idempotencyKey,
	}
	result, stageErr := remote.stageVaultKeyRotation(ctx, rotation.ID, input)
	if stageErr == nil {
		next, err := validateStagedVaultKeyRotationResult(result, identity, rotation, int64(len(items)))
		return next, false, err
	}
	observed, getErr := remote.getVaultKeyRotation(ctx, rotation.ID)
	if getErr != nil || observed == nil {
		return nil, false, ErrOperation
	}
	if err := validateVaultKeyRotationIdentity(observed, identity); err != nil {
		return nil, false, err
	}
	if !sameVaultKeyRotationEpochs(rotation, observed) || observed.LifecycleState != client.VaultKeyRotationOpen {
		return nil, false, ErrIntegrity
	}
	if observed.RowVersion != rotation.RowVersion || observed.StagedCount != rotation.StagedCount {
		if observed.RowVersion <= rotation.RowVersion || observed.StagedCount <= rotation.StagedCount {
			return nil, false, ErrIntegrity
		}
		return observed, true, nil
	}
	// No mutation is visible, so retry the exact request and idempotency key once.
	result, stageErr = remote.stageVaultKeyRotation(ctx, rotation.ID, input)
	if stageErr == nil {
		next, err := validateStagedVaultKeyRotationResult(result, identity, rotation, int64(len(items)))
		return next, false, err
	}
	observed, getErr = remote.getVaultKeyRotation(ctx, rotation.ID)
	if getErr != nil || observed == nil {
		return nil, false, ErrOperation
	}
	if err := validateVaultKeyRotationIdentity(observed, identity); err != nil {
		return nil, false, err
	}
	if !sameVaultKeyRotationEpochs(rotation, observed) || observed.LifecycleState != client.VaultKeyRotationOpen {
		return nil, false, ErrIntegrity
	}
	if observed.RowVersion == rotation.RowVersion && observed.StagedCount == rotation.StagedCount {
		return nil, false, ErrOperation
	}
	if observed.RowVersion <= rotation.RowVersion || observed.StagedCount <= rotation.StagedCount {
		return nil, false, ErrIntegrity
	}
	return observed, true, nil
}

func verifyAllVaultKeyRotationItems(ctx context.Context, remote vaultKeyRotationRemote, identity client.SelfIdentity, rotation *client.VaultKeyRotation, source, target *sealed.AgentVaultKey) error {
	cursor := ""
	seenCursors := map[string]bool{"": true}
	seenDEKs := make(map[string]bool)
	lastDEKID := ""
	var count int64
	plan := newClientVaultKeyRotationPlanHasher(rotation)
	for {
		page, err := remote.listVaultKeyRotationItems(ctx, rotation.ID, client.VaultKeyRotationItemListOptions{
			Limit: vaultKeyRotationPageSize, Cursor: cursor,
		})
		if err != nil || page == nil || len(page.Items) > vaultKeyRotationPageSize {
			return ErrOperation
		}
		for index := range page.Items {
			item := &page.Items[index]
			if item.StagedAt == nil || seenDEKs[item.DEKID] || (lastDEKID != "" && item.DEKID <= lastDEKID) {
				return ErrIntegrity
			}
			seenDEKs[item.DEKID] = true
			lastDEKID = item.DEKID
			count++
			sourceWrapper, scope, err := vaultKeyRotationSourceWrapper(identity, rotation, item)
			if err != nil {
				return err
			}
			targetWrapper, err := vaultKeyRotationTargetWrapper(rotation, item)
			if err != nil || sealed.VerifyDEKRewrap(source, target, scope, sourceWrapper, targetWrapper) != nil {
				clear(targetWrapper.WrappedDEK)
				return ErrIntegrity
			}
			clear(targetWrapper.WrappedDEK)
			plan.Add(*item)
		}
		if page.NextCursor == "" {
			break
		}
		if seenCursors[page.NextCursor] {
			return ErrIntegrity
		}
		seenCursors[page.NextCursor] = true
		cursor = page.NextCursor
	}
	if count != rotation.ItemCount || plan.Sum() != rotation.StagedPlanHash {
		return ErrIntegrity
	}
	return nil
}

func (s *Service) commitVaultKeyRotation(ctx context.Context, remote vaultKeyRotationRemote, identity client.SelfIdentity, rotation *client.VaultKeyRotation, target *sealed.AgentVaultKey, recovery preparedVaultKeyRotationRecovery) (*client.VaultKeyRotation, error) {
	idempotencyKey, err := operationKey("")
	if err != nil {
		return nil, err
	}
	input := client.CommitVaultKeyRotationInput{
		ExpectedRotationRowVersion: rotation.RowVersion,
		ExpectedItemCount:          rotation.ItemCount,
		ExpectedPlanHash:           rotation.StagedPlanHash,
		RecoveryDisposition: client.VaultKeyRotationRecoveryDisposition{
			Mode: recovery.mode, ArtifactSHA256: recovery.artifactSHA256,
		},
		IdempotencyKey: idempotencyKey,
	}
	result, commitErr := remote.commitVaultKeyRotation(ctx, rotation.ID, input)
	if commitErr == nil {
		committed, err := validateCommittedVaultKeyRotationResult(result, identity, rotation, recovery)
		if err != nil {
			return nil, err
		}
		if err := s.validateVaultKeyRotationCurrentBinding(ctx, identity, committed, target); err != nil {
			return nil, err
		}
		return committed, nil
	}
	observed, getErr := remote.getVaultKeyRotation(ctx, rotation.ID)
	if getErr != nil || observed == nil {
		return nil, ErrOperation
	}
	if err := validateVaultKeyRotationIdentity(observed, identity); err != nil {
		return nil, err
	}
	if !sameVaultKeyRotationEpochs(rotation, observed) {
		return nil, ErrIntegrity
	}
	if observed.LifecycleState == client.VaultKeyRotationCommitted {
		if !vaultKeyRotationRecoveryMatches(observed, recovery) {
			return nil, ErrIntegrity
		}
		if err := s.validateVaultKeyRotationCurrentBinding(ctx, identity, observed, target); err != nil {
			return nil, err
		}
		return observed, nil
	}
	if observed.LifecycleState != client.VaultKeyRotationOpen || observed.RowVersion != rotation.RowVersion ||
		observed.StagedCount != rotation.ItemCount || observed.StagedPlanHash != rotation.StagedPlanHash {
		return nil, ErrOperation
	}
	result, commitErr = remote.commitVaultKeyRotation(ctx, rotation.ID, input)
	if commitErr == nil {
		committed, err := validateCommittedVaultKeyRotationResult(result, identity, rotation, recovery)
		if err != nil {
			return nil, err
		}
		if err := s.validateVaultKeyRotationCurrentBinding(ctx, identity, committed, target); err != nil {
			return nil, err
		}
		return committed, nil
	}
	observed, getErr = remote.getVaultKeyRotation(ctx, rotation.ID)
	if getErr != nil || observed == nil {
		return nil, ErrOperation
	}
	if err := validateVaultKeyRotationIdentity(observed, identity); err != nil {
		return nil, err
	}
	if !sameVaultKeyRotationEpochs(rotation, observed) || observed.LifecycleState != client.VaultKeyRotationCommitted {
		return nil, ErrOperation
	}
	if !vaultKeyRotationRecoveryMatches(observed, recovery) {
		return nil, ErrIntegrity
	}
	if err := s.validateVaultKeyRotationCurrentBinding(ctx, identity, observed, target); err != nil {
		return nil, err
	}
	return observed, nil
}

func (s *Service) loadVaultKeyRotationKeys(rotation *client.VaultKeyRotation) (*sealed.AgentVaultKey, *sealed.AgentVaultKey, error) {
	if rotation == nil || rotation.SourceKeyVersion < 1 || rotation.TargetKeyVersion < 1 {
		return nil, nil, ErrIntegrity
	}
	source, err := local.ReadAgentVaultKeyEpoch(s.accountName, s.realmName, s.agentName,
		rotation.SourceKeyID, uint64(rotation.SourceKeyVersion))
	if errors.Is(err, local.ErrAgentVaultKeyUnavailable) {
		return nil, nil, ErrKeyUnavailable
	}
	if err != nil {
		return nil, nil, ErrOperation
	}
	target, err := local.ReadAgentVaultKeyEpoch(s.accountName, s.realmName, s.agentName,
		rotation.TargetKeyID, uint64(rotation.TargetKeyVersion))
	if errors.Is(err, local.ErrAgentVaultKeyUnavailable) {
		source.Clear()
		return nil, nil, ErrKeyUnavailable
	}
	if err != nil {
		source.Clear()
		return nil, nil, ErrOperation
	}
	return source, target, nil
}

func (s *Service) validateVaultKeyRotationCurrentBinding(ctx context.Context, identity client.SelfIdentity, rotation *client.VaultKeyRotation, expected *sealed.AgentVaultKey) error {
	if rotation == nil || expected == nil {
		return ErrIntegrity
	}
	switch rotation.LifecycleState {
	case client.VaultKeyRotationOpen, client.VaultKeyRotationCancelled:
		if expected.ID() != rotation.SourceKeyID || expected.Version() != uint64(rotation.SourceKeyVersion) {
			return ErrIntegrity
		}
	case client.VaultKeyRotationCommitted:
		if expected.ID() != rotation.TargetKeyID || expected.Version() != uint64(rotation.TargetKeyVersion) {
			return ErrIntegrity
		}
	default:
		return ErrIntegrity
	}
	binding, err := s.remote.currentVaultKey(ctx)
	if err != nil || binding == nil {
		return ErrOperation
	}
	if !vaultBindingMatchesIdentity(binding, identity) {
		return ErrIdentityMismatch
	}
	if !vaultKeyMatches(expected, binding, identity) {
		return ErrKeyMismatch
	}
	return nil
}

func validateVaultKeyRotationIdentity(rotation *client.VaultKeyRotation, identity client.SelfIdentity) error {
	if rotation == nil {
		return ErrIntegrity
	}
	if rotation.AccountID != identity.AccountID || rotation.RealmID != identity.RealmID ||
		rotation.OwnerAgentID != identity.AgentID {
		return ErrIdentityMismatch
	}
	if !validGeneratedID(rotation.ID, "vkr") || !validGeneratedID(rotation.SourceKeyID, "avk") ||
		!validGeneratedID(rotation.TargetKeyID, "avk") || rotation.SourceKeyID == rotation.TargetKeyID ||
		rotation.SourceKeyVersion < 1 || rotation.SourceKeyVersion == math.MaxInt64 ||
		rotation.TargetKeyVersion != rotation.SourceKeyVersion+1 ||
		rotation.SourceKeyAlgorithm != sealed.AES256GCMAlgorithm ||
		rotation.TargetKeyAlgorithm != sealed.AES256GCMAlgorithm ||
		!validVaultKeyRotationHash(rotation.SourceKeyFingerprint) ||
		!validVaultKeyRotationHash(rotation.TargetKeyFingerprint) ||
		rotation.ItemCount < 0 || rotation.StagedCount < 0 || rotation.StagedCount > rotation.ItemCount ||
		rotation.RowVersion < 1 || rotation.CreatedAt.IsZero() || rotation.UpdatedAt.Before(rotation.CreatedAt) {
		return ErrIntegrity
	}
	switch rotation.LifecycleState {
	case client.VaultKeyRotationOpen:
		if rotation.CommittedAt != nil || rotation.CancelledAt != nil ||
			rotation.RecoveryDispositionMode != "" || rotation.RecoveryArtifactSHA256 != "" {
			return ErrIntegrity
		}
		if rotation.StagedCount == rotation.ItemCount {
			if !validVaultKeyRotationHash(rotation.StagedPlanHash) {
				return ErrIntegrity
			}
		} else if rotation.StagedPlanHash != "" {
			return ErrIntegrity
		}
	case client.VaultKeyRotationCommitted:
		if rotation.StagedCount != rotation.ItemCount || rotation.StagedPlanHash != "" ||
			rotation.CommittedAt == nil || rotation.CancelledAt != nil ||
			rotation.CommittedAt.Before(rotation.CreatedAt) || rotation.CommittedAt.After(rotation.UpdatedAt) ||
			!validVaultKeyRotationRecoveryDisposition(
				rotation.RecoveryDispositionMode, rotation.RecoveryArtifactSHA256,
			) {
			return ErrIntegrity
		}
	case client.VaultKeyRotationCancelled:
		if rotation.StagedPlanHash != "" || rotation.CommittedAt != nil || rotation.CancelledAt == nil ||
			rotation.CancelledAt.Before(rotation.CreatedAt) || rotation.CancelledAt.After(rotation.UpdatedAt) ||
			rotation.RecoveryDispositionMode != "" || rotation.RecoveryArtifactSHA256 != "" {
			return ErrIntegrity
		}
	default:
		return ErrIntegrity
	}
	return nil
}

func validateVaultKeyRotationKeys(rotation *client.VaultKeyRotation, source, target *sealed.AgentVaultKey) error {
	if rotation == nil || source == nil || target == nil {
		return ErrIntegrity
	}
	sourceMetadata, targetMetadata := source.Metadata(), target.Metadata()
	if rotation.SourceKeyID != sourceMetadata.ID || uint64(rotation.SourceKeyVersion) != sourceMetadata.Version ||
		rotation.SourceKeyAlgorithm != sourceMetadata.Algorithm ||
		rotation.SourceKeyFingerprint != sourceMetadata.Fingerprint ||
		rotation.TargetKeyID != targetMetadata.ID || uint64(rotation.TargetKeyVersion) != targetMetadata.Version ||
		rotation.TargetKeyAlgorithm != targetMetadata.Algorithm ||
		rotation.TargetKeyFingerprint != targetMetadata.Fingerprint {
		return ErrKeyMismatch
	}
	return nil
}

func validateStartedVaultKeyRotationResult(result *client.VaultKeyRotationMutationResult, identity client.SelfIdentity, expectedRotationID string, source, target *sealed.AgentVaultKey) (*client.VaultKeyRotation, error) {
	if result == nil {
		return nil, ErrOperation
	}
	rotation := &result.Rotation
	if err := validateVaultKeyRotationIdentity(rotation, identity); err != nil {
		return nil, err
	}
	if rotation.ID != expectedRotationID || rotation.LifecycleState != client.VaultKeyRotationOpen {
		return nil, ErrIntegrity
	}
	if err := validateVaultKeyRotationKeys(rotation, source, target); err != nil {
		return nil, err
	}
	return rotation, nil
}

func validateStagedVaultKeyRotationResult(result *client.VaultKeyRotationMutationResult, identity client.SelfIdentity, previous *client.VaultKeyRotation, staged int64) (*client.VaultKeyRotation, error) {
	if result == nil || previous == nil || staged < 1 {
		return nil, ErrOperation
	}
	rotation := &result.Rotation
	if err := validateVaultKeyRotationIdentity(rotation, identity); err != nil {
		return nil, err
	}
	if rotation.LifecycleState != client.VaultKeyRotationOpen || !sameVaultKeyRotationEpochs(previous, rotation) ||
		rotation.RowVersion != previous.RowVersion+1 || rotation.StagedCount != previous.StagedCount+staged ||
		rotation.ItemCount != previous.ItemCount {
		return nil, ErrIntegrity
	}
	return rotation, nil
}

func validateCommittedVaultKeyRotationResult(result *client.VaultKeyRotationMutationResult, identity client.SelfIdentity, previous *client.VaultKeyRotation, recovery preparedVaultKeyRotationRecovery) (*client.VaultKeyRotation, error) {
	if result == nil || previous == nil {
		return nil, ErrOperation
	}
	rotation := &result.Rotation
	if err := validateVaultKeyRotationIdentity(rotation, identity); err != nil {
		return nil, err
	}
	if rotation.LifecycleState != client.VaultKeyRotationCommitted || !sameVaultKeyRotationEpochs(previous, rotation) ||
		rotation.RowVersion != previous.RowVersion+1 || rotation.ItemCount != previous.ItemCount ||
		rotation.StagedCount != previous.ItemCount || !vaultKeyRotationRecoveryMatches(rotation, recovery) {
		return nil, ErrIntegrity
	}
	return rotation, nil
}

func vaultKeyRotationRecoveryMatches(rotation *client.VaultKeyRotation, recovery preparedVaultKeyRotationRecovery) bool {
	return rotation != nil && validVaultKeyRotationRecoveryDisposition(recovery.mode, recovery.artifactSHA256) &&
		rotation.RecoveryDispositionMode == recovery.mode &&
		rotation.RecoveryArtifactSHA256 == recovery.artifactSHA256
}

func validateCancelledVaultKeyRotationResult(result *client.VaultKeyRotationMutationResult, identity client.SelfIdentity, previous *client.VaultKeyRotation) (*client.VaultKeyRotation, error) {
	if result == nil || previous == nil {
		return nil, ErrOperation
	}
	rotation := &result.Rotation
	if err := validateVaultKeyRotationIdentity(rotation, identity); err != nil {
		return nil, err
	}
	if rotation.LifecycleState != client.VaultKeyRotationCancelled || !sameVaultKeyRotationEpochs(previous, rotation) ||
		rotation.RowVersion != previous.RowVersion+1 || rotation.ItemCount != previous.ItemCount ||
		rotation.StagedCount != previous.StagedCount {
		return nil, ErrIntegrity
	}
	return rotation, nil
}

func sameVaultKeyRotationEpochs(first, second *client.VaultKeyRotation) bool {
	return first != nil && second != nil && first.ID == second.ID && first.AccountID == second.AccountID &&
		first.RealmID == second.RealmID && first.OwnerAgentID == second.OwnerAgentID &&
		first.SourceKeyID == second.SourceKeyID && first.SourceKeyVersion == second.SourceKeyVersion &&
		first.SourceKeyAlgorithm == second.SourceKeyAlgorithm &&
		first.SourceKeyFingerprint == second.SourceKeyFingerprint &&
		first.TargetKeyID == second.TargetKeyID && first.TargetKeyVersion == second.TargetKeyVersion &&
		first.TargetKeyAlgorithm == second.TargetKeyAlgorithm &&
		first.TargetKeyFingerprint == second.TargetKeyFingerprint
}

func vaultKeyRotationSourceWrapper(identity client.SelfIdentity, rotation *client.VaultKeyRotation, item *client.VaultKeyRotationItem) (sealed.DEKWrapper, sealed.FieldScope, error) {
	if rotation == nil || item == nil || item.RotationID != rotation.ID ||
		!validGeneratedID(item.SecretID, "sec") || !validGeneratedID(item.FieldID, "fld") ||
		!validGeneratedID(item.DEKID, "dek") || item.DEKGeneration < 1 || item.SourceDEKRowVersion < 1 ||
		item.SourceWrapRevision < 1 || item.SourceWrapRevision == math.MaxInt64 ||
		item.SourceWrapAlgorithm != sealed.AES256GCMAlgorithm || item.SourceAADVersion != int64(sealed.AADVersionV1) ||
		item.SourceWrappingKeyID != rotation.SourceKeyID ||
		item.SourceWrappingKeyVersion != rotation.SourceKeyVersion ||
		item.TargetWrappingKeyID != rotation.TargetKeyID ||
		item.TargetWrappingKeyVersion != rotation.TargetKeyVersion ||
		len(item.SourceWrappedDEK) != sealed.WrappedDEKBytes {
		return sealed.DEKWrapper{}, sealed.FieldScope{}, ErrIntegrity
	}
	domain, ok := fieldDomain(item.FieldKind)
	if !ok {
		return sealed.DEKWrapper{}, sealed.FieldScope{}, ErrIntegrity
	}
	scope := sealed.FieldScope{
		Domain: domain, AccountID: identity.AccountID, RealmID: identity.RealmID,
		OwnerAgentID: identity.AgentID, SecretID: item.SecretID, FieldID: item.FieldID,
	}
	return sealed.DEKWrapper{
		DEKID: item.DEKID, DEKGeneration: uint64(item.DEKGeneration),
		WrappedDEK: item.SourceWrappedDEK, WrapAlgorithm: item.SourceWrapAlgorithm,
		AADVersion: uint32(item.SourceAADVersion), WrapRevision: uint64(item.SourceWrapRevision),
		WrappingKeyID: item.SourceWrappingKeyID, WrappingKeyVersion: uint64(item.SourceWrappingKeyVersion),
	}, scope, nil
}

func vaultKeyRotationTargetWrapper(rotation *client.VaultKeyRotation, item *client.VaultKeyRotationItem) (sealed.DEKWrapper, error) {
	if rotation == nil || item == nil || item.StagedAt == nil ||
		item.TargetWrapRevision != item.SourceWrapRevision+1 ||
		len(item.TargetWrappedDEK) != sealed.WrappedDEKBytes ||
		!validVaultKeyRotationHash(item.TargetWrapperSHA256) ||
		vaultKeyRotationWrapperHash(item.TargetWrappedDEK) != item.TargetWrapperSHA256 {
		return sealed.DEKWrapper{}, ErrIntegrity
	}
	return sealed.DEKWrapper{
		DEKID: item.DEKID, DEKGeneration: uint64(item.DEKGeneration),
		WrappedDEK: append([]byte(nil), item.TargetWrappedDEK...), WrapAlgorithm: sealed.AES256GCMAlgorithm,
		AADVersion: sealed.AADVersionV1, WrapRevision: uint64(item.TargetWrapRevision),
		WrappingKeyID: rotation.TargetKeyID, WrappingKeyVersion: uint64(rotation.TargetKeyVersion),
	}, nil
}

func validVaultKeyRotationHash(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	raw, err := hex.DecodeString(value)
	valid := err == nil && len(raw) == 32
	clear(raw)
	return valid
}

func validVaultKeyRotationRecoveryDisposition(mode, artifactSHA256 string) bool {
	switch mode {
	case client.VaultKeyRotationRecoveryArtifact:
		return validVaultKeyRotationHash(artifactSHA256)
	case client.VaultKeyRotationRiskAccepted:
		return artifactSHA256 == ""
	default:
		return false
	}
}

func vaultKeyRotationWrapperHash(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

type clientVaultKeyRotationPlanHasher struct {
	h hash.Hash
}

func newClientVaultKeyRotationPlanHasher(rotation *client.VaultKeyRotation) *clientVaultKeyRotationPlanHasher {
	h := sha256.New()
	_, _ = h.Write([]byte("witself/vault-key-rotation-plan/v1\x00"))
	writeClientVaultKeyRotationHashString(h, rotation.ID)
	writeClientVaultKeyRotationHashString(h, rotation.SourceKeyID)
	writeClientVaultKeyRotationHashInt(h, rotation.SourceKeyVersion)
	writeClientVaultKeyRotationHashString(h, rotation.TargetKeyID)
	writeClientVaultKeyRotationHashInt(h, rotation.TargetKeyVersion)
	writeClientVaultKeyRotationHashInt(h, rotation.ItemCount)
	return &clientVaultKeyRotationPlanHasher{h: h}
}

func (h *clientVaultKeyRotationPlanHasher) Add(item client.VaultKeyRotationItem) {
	writeClientVaultKeyRotationHashString(h.h, item.DEKID)
	writeClientVaultKeyRotationHashString(h.h, item.SecretID)
	writeClientVaultKeyRotationHashString(h.h, item.FieldID)
	writeClientVaultKeyRotationHashString(h.h, item.FieldKind)
	writeClientVaultKeyRotationHashInt(h.h, item.DEKGeneration)
	writeClientVaultKeyRotationHashInt(h.h, item.SourceDEKRowVersion)
	writeClientVaultKeyRotationHashInt(h.h, item.SourceWrapRevision)
	writeClientVaultKeyRotationHashString(h.h, vaultKeyRotationWrapperHash(item.SourceWrappedDEK))
	writeClientVaultKeyRotationHashString(h.h, item.SourceWrapAlgorithm)
	writeClientVaultKeyRotationHashInt(h.h, item.SourceAADVersion)
	writeClientVaultKeyRotationHashString(h.h, item.SourceWrappingKeyID)
	writeClientVaultKeyRotationHashInt(h.h, item.SourceWrappingKeyVersion)
	writeClientVaultKeyRotationHashInt(h.h, item.TargetWrapRevision)
	writeClientVaultKeyRotationHashString(h.h, item.TargetWrapperSHA256)
}

func (h *clientVaultKeyRotationPlanHasher) Sum() string {
	return hex.EncodeToString(h.h.Sum(nil))
}

func writeClientVaultKeyRotationHashString(h hash.Hash, value string) {
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(value)))
	_, _ = h.Write(length[:])
	_, _ = h.Write([]byte(value))
}

func writeClientVaultKeyRotationHashInt(h hash.Hash, value int64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], uint64(value))
	_, _ = h.Write(encoded[:])
}

func clearVaultKeyRotationStageItems(items []client.StageVaultKeyRotationItemInput) {
	for index := range items {
		clear(items[index].TargetWrappedDEK)
	}
}

func (s *Service) vaultKeyRotationRemote() (vaultKeyRotationRemote, error) {
	remote, ok := s.remote.(vaultKeyRotationRemote)
	if !ok {
		return nil, ErrOperation
	}
	return remote, nil
}

func (r httpRemote) getOpenVaultKeyRotation(ctx context.Context) (*client.VaultKeyRotation, error) {
	return client.GetOpenVaultKeyRotation(ctx, r.endpoint, r.token)
}

func (r httpRemote) getVaultKeyRotation(ctx context.Context, rotationID string) (*client.VaultKeyRotation, error) {
	return client.GetVaultKeyRotation(ctx, r.endpoint, r.token, rotationID)
}

func (r httpRemote) listVaultKeyRotationItems(ctx context.Context, rotationID string, options client.VaultKeyRotationItemListOptions) (*client.VaultKeyRotationItemPage, error) {
	return client.ListVaultKeyRotationItems(ctx, r.endpoint, r.token, rotationID, options)
}

func (r httpRemote) startVaultKeyRotation(ctx context.Context, input client.StartVaultKeyRotationInput) (*client.VaultKeyRotationMutationResult, error) {
	return client.StartVaultKeyRotation(ctx, r.endpoint, r.token, input)
}

func (r httpRemote) stageVaultKeyRotation(ctx context.Context, rotationID string, input client.StageVaultKeyRotationInput) (*client.VaultKeyRotationMutationResult, error) {
	return client.StageVaultKeyRotation(ctx, r.endpoint, r.token, rotationID, input)
}

func (r httpRemote) commitVaultKeyRotation(ctx context.Context, rotationID string, input client.CommitVaultKeyRotationInput) (*client.VaultKeyRotationMutationResult, error) {
	return client.CommitVaultKeyRotation(ctx, r.endpoint, r.token, rotationID, input)
}

func (r httpRemote) cancelVaultKeyRotation(ctx context.Context, rotationID string, input client.CancelVaultKeyRotationInput) (*client.VaultKeyRotationMutationResult, error) {
	return client.CancelVaultKeyRotation(ctx, r.endpoint, r.token, rotationID, input)
}
