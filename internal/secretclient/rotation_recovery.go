package secretclient

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/sealed"
)

var (
	// ErrVaultKeyRotationRecoveryUnavailable tells the service that a recovery
	// sink has no artifact at its configured destination. Sink implementations
	// must not use this error for malformed, unreadable, or unsafe artifacts.
	ErrVaultKeyRotationRecoveryUnavailable = errors.New("vault key rotation recovery artifact is unavailable")

	// ErrVaultKeyRotationRecoveryExists tells the service that PutIfAbsent did
	// not publish because the configured destination already exists. The
	// service never trusts this result by itself; it reads, decrypts, and
	// verifies the existing artifact before treating the operation as a replay.
	ErrVaultKeyRotationRecoveryExists = errors.New("vault key rotation recovery artifact already exists")
)

// VaultKeyRotationRecoverySink is the client-custodied durability boundary for
// an encrypted recovery artifact. Implementations are bound to one destination
// and must durably publish without replacement before PutIfAbsent returns. The
// artifact passed to PutIfAbsent is borrowed and must not be retained or
// mutated; ReadBack returns a caller-owned copy that the service will clear.
// Neither this interface nor its artifact ever crosses the Witself backend API.
type VaultKeyRotationRecoverySink interface {
	PutIfAbsent(context.Context, sealed.AVKRecoveryMetadata, []byte) error
	ReadBack(context.Context) ([]byte, error)
}

// RotateVaultKeyOptions makes the recovery decision explicit. Exactly one
// branch is valid for an open rotation: a sink plus recovery passphrase, or an
// affirmative acceptance that loss of the only target-key copy can permanently
// destroy access to the sealed plane. The zero value is deliberately invalid.
type RotateVaultKeyOptions struct {
	RecoverySink               VaultKeyRotationRecoverySink
	RecoveryPassphrase         []byte
	AcceptUnrecoverableKeyLoss bool
}

func (options RotateVaultKeyOptions) String() string {
	return fmt.Sprintf("<vault-key-rotation-options recovery_sink=%t recovery_passphrase=<redacted> accept_unrecoverable_key_loss=%t>",
		options.RecoverySink != nil, options.AcceptUnrecoverableKeyLoss)
}

// GoString returns the same redacted diagnostic representation as String.
func (options RotateVaultKeyOptions) GoString() string { return options.String() }

type preparedVaultKeyRotationRecovery struct {
	mode           string
	artifactSHA256 string
}

func (options RotateVaultKeyOptions) validate() error {
	hasRecovery := options.RecoverySink != nil
	hasPassphrase := len(options.RecoveryPassphrase) > 0
	if options.AcceptUnrecoverableKeyLoss {
		if hasRecovery || hasPassphrase {
			return ErrInvalidInput
		}
		return nil
	}
	if !hasRecovery || !hasPassphrase || len(options.RecoveryPassphrase) < sealed.MinAVKRecoveryPassphraseBytes ||
		len(options.RecoveryPassphrase) > sealed.MaxAVKRecoveryPassphraseBytes {
		return ErrInvalidInput
	}
	return nil
}

func (s *Service) prepareVaultKeyRotationRecovery(
	ctx context.Context,
	identity client.SelfIdentity,
	target *sealed.AgentVaultKey,
	options RotateVaultKeyOptions,
) (preparedVaultKeyRotationRecovery, error) {
	if err := options.validate(); err != nil {
		return preparedVaultKeyRotationRecovery{}, err
	}
	if options.AcceptUnrecoverableKeyLoss {
		return preparedVaultKeyRotationRecovery{mode: client.VaultKeyRotationRiskAccepted}, nil
	}
	if target == nil {
		return preparedVaultKeyRotationRecovery{}, ErrIntegrity
	}
	passphrase := append([]byte(nil), options.RecoveryPassphrase...)
	defer clear(passphrase)

	scope := recoveryScope(identity)
	existing, err := options.RecoverySink.ReadBack(ctx)
	switch {
	case err == nil:
		defer clear(existing)
		return verifyVaultKeyRotationRecoveryArtifact(existing, passphrase, scope, target)
	case !errors.Is(err, ErrVaultKeyRotationRecoveryUnavailable):
		return preparedVaultKeyRotationRecovery{}, ErrOperation
	}

	artifact, err := sealed.ExportAgentVaultKeyRecovery(target, passphrase, scope)
	if errors.Is(err, sealed.ErrInvalidRecoveryPassphrase) {
		return preparedVaultKeyRotationRecovery{}, ErrInvalidInput
	}
	if err != nil {
		return preparedVaultKeyRotationRecovery{}, ErrOperation
	}
	defer clear(artifact)
	metadata, err := sealed.InspectAgentVaultKeyRecovery(artifact)
	if err != nil || metadata.Scope != scope || metadata.AVK != target.Metadata() {
		return preparedVaultKeyRotationRecovery{}, ErrIntegrity
	}

	// PutIfAbsent may report a collision, or its acknowledgement may be lost
	// after durable publication. In every case ReadBack is authoritative: only
	// the exact artifact that decrypts under this passphrase and target epoch can
	// satisfy the gate.
	_ = options.RecoverySink.PutIfAbsent(ctx, metadata, artifact)
	if ctx.Err() != nil {
		return preparedVaultKeyRotationRecovery{}, ErrOperation
	}
	published, readErr := options.RecoverySink.ReadBack(ctx)
	if readErr != nil {
		return preparedVaultKeyRotationRecovery{}, ErrOperation
	}
	defer clear(published)
	verified, verifyErr := verifyVaultKeyRotationRecoveryArtifact(
		published, passphrase, scope, target,
	)
	if verifyErr != nil {
		return preparedVaultKeyRotationRecovery{}, verifyErr
	}
	// A sink error is safely recoverable only when the subsequent read-back
	// proves that the exact artifact is durable. This covers both an existing
	// valid artifact and a lost Put acknowledgement without weakening fail-close
	// behavior for unsafe or mismatched destinations.
	return verified, nil
}

func verifyVaultKeyRotationRecoveryArtifact(
	artifact, passphrase []byte,
	expectedScope sealed.AVKRecoveryScope,
	expectedTarget *sealed.AgentVaultKey,
) (preparedVaultKeyRotationRecovery, error) {
	if expectedTarget == nil {
		return preparedVaultKeyRotationRecovery{}, ErrIntegrity
	}
	metadata, err := sealed.InspectAgentVaultKeyRecovery(artifact)
	if err != nil || metadata.Scope != expectedScope || metadata.AVK != expectedTarget.Metadata() {
		return preparedVaultKeyRotationRecovery{}, ErrIntegrity
	}
	recovered, err := sealed.OpenAgentVaultKeyRecovery(artifact, passphrase, expectedScope)
	if errors.Is(err, sealed.ErrInvalidRecoveryPassphrase) {
		return preparedVaultKeyRotationRecovery{}, ErrInvalidInput
	}
	if err != nil || recovered == nil {
		return preparedVaultKeyRotationRecovery{}, ErrIntegrity
	}
	defer recovered.Clear()
	if recovered.Metadata() != expectedTarget.Metadata() {
		return preparedVaultKeyRotationRecovery{}, ErrIntegrity
	}
	expectedRecord, err := sealed.EncodeAgentVaultKey(expectedTarget)
	if err != nil {
		return preparedVaultKeyRotationRecovery{}, ErrIntegrity
	}
	defer clear(expectedRecord)
	recoveredRecord, err := sealed.EncodeAgentVaultKey(recovered)
	if err != nil {
		return preparedVaultKeyRotationRecovery{}, ErrIntegrity
	}
	defer clear(recoveredRecord)
	if len(expectedRecord) != len(recoveredRecord) ||
		subtle.ConstantTimeCompare(expectedRecord, recoveredRecord) != 1 {
		return preparedVaultKeyRotationRecovery{}, ErrIntegrity
	}
	digest := sha256.Sum256(artifact)
	return preparedVaultKeyRotationRecovery{
		mode: client.VaultKeyRotationRecoveryArtifact, artifactSHA256: hex.EncodeToString(digest[:]),
	}, nil
}
