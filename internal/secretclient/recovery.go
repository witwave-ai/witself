package secretclient

import (
	"context"
	"errors"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/local"
	"github.com/witwave-ai/witself/internal/sealed"
)

// ErrRecoveryBindingUnavailable means no exact current local/backend AVK pair exists.
var ErrRecoveryBindingUnavailable = errors.New("vault key recovery requires an existing backend key binding")

// ExportVaultKeyRecovery creates a passphrase-encrypted offline package for
// the exact current local/backend AVK epoch. It never returns an artifact from
// a local-only, backend-only, or mismatched custody state.
func (s *Service) ExportVaultKeyRecovery(ctx context.Context, passphrase []byte) ([]byte, sealed.AVKRecoveryMetadata, error) {
	identity, err := s.identity(ctx)
	if err != nil {
		return nil, sealed.AVKRecoveryMetadata{}, err
	}
	observation, err := s.observeVaultKey(ctx, identity)
	if err != nil {
		return nil, sealed.AVKRecoveryMetadata{}, err
	}
	defer observation.local.Clear()
	if observation.status.State != VaultKeyStateMatch || observation.local == nil {
		switch observation.status.State {
		case VaultKeyStateBackendOnly:
			return nil, sealed.AVKRecoveryMetadata{}, ErrKeyUnavailable
		case VaultKeyStateMismatch:
			return nil, sealed.AVKRecoveryMetadata{}, ErrKeyMismatch
		default:
			return nil, sealed.AVKRecoveryMetadata{}, ErrRecoveryBindingUnavailable
		}
	}
	artifact, err := sealed.ExportAgentVaultKeyRecovery(observation.local, passphrase, recoveryScope(identity))
	if errors.Is(err, sealed.ErrInvalidRecoveryPassphrase) {
		return nil, sealed.AVKRecoveryMetadata{}, ErrInvalidInput
	}
	if err != nil {
		return nil, sealed.AVKRecoveryMetadata{}, ErrOperation
	}
	metadata, err := sealed.InspectAgentVaultKeyRecovery(artifact)
	if err != nil {
		clear(artifact)
		return nil, sealed.AVKRecoveryMetadata{}, ErrOperation
	}
	return artifact, metadata, nil
}

// ImportVaultKeyRecovery restores one exact current epoch to this local
// installation. The destination backend binding is authoritative: an artifact
// for any other epoch or owner is refused, and no backend key is registered or
// replaced by recovery import.
func (s *Service) ImportVaultKeyRecovery(ctx context.Context, artifact, passphrase []byte) (sealed.AgentVaultKeyMetadata, error) {
	identity, err := s.identity(ctx)
	if err != nil {
		return sealed.AgentVaultKeyMetadata{}, err
	}
	backend, err := s.remote.currentVaultKey(ctx)
	if err != nil {
		return sealed.AgentVaultKeyMetadata{}, ErrOperation
	}
	if backend == nil {
		return sealed.AgentVaultKeyMetadata{}, ErrRecoveryBindingUnavailable
	}
	if !vaultBindingMatchesIdentity(backend, identity) {
		return sealed.AgentVaultKeyMetadata{}, ErrIdentityMismatch
	}
	metadata, err := sealed.InspectAgentVaultKeyRecovery(artifact)
	if err != nil || metadata.Scope != recoveryScope(identity) || !recoveryMetadataMatchesBinding(metadata.AVK, backend) {
		return sealed.AgentVaultKeyMetadata{}, ErrKeyMismatch
	}
	key, err := sealed.OpenAgentVaultKeyRecovery(artifact, passphrase, recoveryScope(identity))
	if errors.Is(err, sealed.ErrInvalidRecoveryPassphrase) {
		return sealed.AgentVaultKeyMetadata{}, ErrInvalidInput
	}
	if err != nil || key == nil {
		return sealed.AgentVaultKeyMetadata{}, ErrIntegrity
	}
	defer key.Clear()
	if !vaultKeyMatches(key, backend, identity) {
		return sealed.AgentVaultKeyMetadata{}, ErrKeyMismatch
	}
	if err := installRecoveredVaultKeyEpoch(s, key); err != nil {
		return sealed.AgentVaultKeyMetadata{}, err
	}
	return key.Metadata(), nil
}

func installRecoveredVaultKeyEpoch(s *Service, key *sealed.AgentVaultKey) error {
	current, err := local.ReadAgentVaultKeyEpoch(s.accountName, s.realmName, s.agentName, key.ID(), key.Version())
	if err == nil {
		defer current.Clear()
		if current.Fingerprint() != key.Fingerprint() || current.Algorithm() != key.Algorithm() {
			return ErrKeyMismatch
		}
		return nil
	}
	if !errors.Is(err, local.ErrAgentVaultKeyUnavailable) {
		return ErrOperation
	}
	if err := local.CreateAgentVaultKeyEpoch(s.accountName, s.realmName, s.agentName, key); err != nil &&
		!errors.Is(err, local.ErrAgentVaultKeyExists) {
		return ErrOperation
	}
	current, err = local.ReadAgentVaultKeyEpoch(s.accountName, s.realmName, s.agentName, key.ID(), key.Version())
	if err != nil {
		return ErrOperation
	}
	defer current.Clear()
	if current.Fingerprint() != key.Fingerprint() || current.Algorithm() != key.Algorithm() {
		return ErrKeyMismatch
	}
	return nil
}

func recoveryScope(identity client.SelfIdentity) sealed.AVKRecoveryScope {
	return sealed.AVKRecoveryScope{
		AccountID: identity.AccountID, RealmID: identity.RealmID, OwnerAgentID: identity.AgentID,
	}
}

func recoveryMetadataMatchesBinding(metadata sealed.AgentVaultKeyMetadata, binding *client.VaultKeyBinding) bool {
	return binding != nil && metadata.ID == binding.ID && metadata.Version <= uint64(1<<63-1) &&
		int64(metadata.Version) == binding.KeyVersion && metadata.Algorithm == binding.Algorithm &&
		metadata.Fingerprint == binding.Fingerprint && binding.LifecycleState == "current"
}
