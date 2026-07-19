package sealed

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"

	"github.com/witwave-ai/witself/internal/id"
)

const (
	// EnvelopeVersionV1 is the only accepted sensitive-field envelope version.
	EnvelopeVersionV1 uint32 = 1

	// DataEncryptionKeyBytes is the v1 per-field-generation DEK size.
	DataEncryptionKeyBytes = 32

	// MaxSensitiveValueBytes is the cleartext v1 inline field limit.
	MaxSensitiveValueBytes = 64 << 10

	// RandomNonceGCMOverhead is the prepended 96-bit nonce plus GCM tag.
	RandomNonceGCMOverhead = 12 + 16

	// MinSensitiveCiphertextBytes is the smallest persisted ciphertext for a
	// non-empty inline sensitive field.
	MinSensitiveCiphertextBytes = 1 + RandomNonceGCMOverhead

	// MaxSensitiveCiphertextBytes is the largest persisted ciphertext for an
	// inline sensitive field.
	MaxSensitiveCiphertextBytes = MaxSensitiveValueBytes + RandomNonceGCMOverhead

	// WrappedDEKBytes is the exact v1 wrapped 32-byte DEK length.
	WrappedDEKBytes = DataEncryptionKeyBytes + RandomNonceGCMOverhead
)

// SensitiveFieldOptions supplies caller-owned versions and immutable scope for
// a newly sealed sensitive value. SealSensitiveField creates the random DEK and
// its dek_ identifier.
type SensitiveFieldOptions struct {
	Scope         FieldScope
	ValueVersion  uint64
	DEKGeneration uint64
	ValueEncoding string
	WrapRevision  uint64
}

// SensitiveFieldEnvelope is the complete ciphertext plus wrapped-DEK package
// persisted by the backend. It never contains plaintext or an unwrapped key.
type SensitiveFieldEnvelope struct {
	EnvelopeVersion    uint32 `json:"envelope_version"`
	AADVersion         uint32 `json:"aad_version"`
	Ciphertext         []byte `json:"ciphertext"`
	AEADAlgorithm      string `json:"aead_algorithm"`
	ValueEncoding      string `json:"value_encoding"`
	ValueVersion       uint64 `json:"value_version"`
	DEKID              string `json:"dek_id"`
	DEKGeneration      uint64 `json:"dek_generation"`
	WrappedDEK         []byte `json:"wrapped_dek"`
	WrapAlgorithm      string `json:"wrap_algorithm"`
	WrapRevision       uint64 `json:"wrap_revision"`
	WrappingKeyID      string `json:"wrapping_key_id"`
	WrappingKeyVersion uint64 `json:"wrapping_key_version"`
}

// SealSensitiveField generates a fresh random DEK, encrypts plaintext under
// that DEK, and independently wraps the DEK under avk.
func SealSensitiveField(avk *AgentVaultKey, plaintext []byte, options SensitiveFieldOptions) (SensitiveFieldEnvelope, error) {
	if !validAgentVaultKey(avk) {
		return SensitiveFieldEnvelope{}, ErrInvalidKeyEncoding
	}
	if len(plaintext) < 1 || len(plaintext) > MaxSensitiveValueBytes || options.ValueVersion == 0 ||
		options.DEKGeneration == 0 || options.WrapRevision == 0 ||
		!validValueEncoding(options.ValueEncoding) {
		return SensitiveFieldEnvelope{}, ErrInvalidBinding
	}
	if err := validateFieldScope(options.Scope); err != nil {
		return SensitiveFieldEnvelope{}, err
	}

	dekID, err := id.New("dek")
	if err != nil {
		return SensitiveFieldEnvelope{}, ErrRandomSource
	}
	var dek [DataEncryptionKeyBytes]byte
	if _, err := rand.Read(dek[:]); err != nil {
		return SensitiveFieldEnvelope{}, ErrRandomSource
	}
	defer clear(dek[:])

	valueBinding := ValueAADBinding{
		FieldScope:    options.Scope,
		DEKID:         dekID,
		ValueVersion:  options.ValueVersion,
		DEKGeneration: options.DEKGeneration,
		ValueEncoding: options.ValueEncoding,
		AEADAlgorithm: AES256GCMAlgorithm,
	}
	valueAAD, err := CanonicalValueAAD(valueBinding)
	if err != nil {
		return SensitiveFieldEnvelope{}, err
	}
	ciphertext, err := sealAES256GCM(dek[:], plaintext, valueAAD)
	clear(valueAAD)
	if err != nil {
		return SensitiveFieldEnvelope{}, err
	}

	wrapBinding := DEKWrapAADBinding{
		FieldScope:         options.Scope,
		DEKID:              dekID,
		DEKGeneration:      options.DEKGeneration,
		WrappingKeyID:      avk.ID(),
		WrappingKeyVersion: avk.Version(),
		WrapRevision:       options.WrapRevision,
		WrapAlgorithm:      AES256GCMAlgorithm,
	}
	wrapAAD, err := CanonicalDEKWrapAAD(wrapBinding)
	if err != nil {
		clear(ciphertext)
		return SensitiveFieldEnvelope{}, err
	}
	wrappedDEK, err := sealAES256GCM(avk.material[:], dek[:], wrapAAD)
	clear(wrapAAD)
	if err != nil {
		clear(ciphertext)
		return SensitiveFieldEnvelope{}, err
	}

	return SensitiveFieldEnvelope{
		EnvelopeVersion:    EnvelopeVersionV1,
		AADVersion:         AADVersionV1,
		Ciphertext:         ciphertext,
		AEADAlgorithm:      AES256GCMAlgorithm,
		ValueEncoding:      options.ValueEncoding,
		ValueVersion:       options.ValueVersion,
		DEKID:              dekID,
		DEKGeneration:      options.DEKGeneration,
		WrappedDEK:         wrappedDEK,
		WrapAlgorithm:      AES256GCMAlgorithm,
		WrapRevision:       options.WrapRevision,
		WrappingKeyID:      avk.ID(),
		WrappingKeyVersion: avk.Version(),
	}, nil
}

// OpenSensitiveField authenticates and decrypts one complete envelope. Every
// wrong-key, wrong-binding, malformed-metadata, and AEAD failure collapses to
// ErrIntegrity.
func OpenSensitiveField(avk *AgentVaultKey, scope FieldScope, envelope SensitiveFieldEnvelope) ([]byte, error) {
	if !validAgentVaultKey(avk) || !validEnvelopeForOpen(avk, scope, envelope) {
		return nil, ErrIntegrity
	}

	dek, err := unwrapEnvelopeDEK(avk, scope, envelope)
	if err != nil {
		return nil, ErrIntegrity
	}
	defer clear(dek)

	valueAAD, err := CanonicalValueAAD(valueBindingFromEnvelope(scope, envelope))
	if err != nil {
		return nil, ErrIntegrity
	}
	plaintext, err := openAES256GCM(dek, envelope.Ciphertext, valueAAD)
	clear(valueAAD)
	if err != nil || len(plaintext) < 1 || len(plaintext) > MaxSensitiveValueBytes {
		clear(plaintext)
		return nil, ErrIntegrity
	}
	return plaintext, nil
}

// RewrapSensitiveFieldDEK unwraps the existing field DEK with oldAVK and wraps
// the same DEK under newAVK and a strictly newer wrap revision. Field
// ciphertext is copied byte-for-byte and never decrypted.
func RewrapSensitiveFieldDEK(oldAVK, newAVK *AgentVaultKey, scope FieldScope, envelope SensitiveFieldEnvelope, newWrapRevision uint64) (SensitiveFieldEnvelope, error) {
	if !validAgentVaultKey(oldAVK) || !validAgentVaultKey(newAVK) ||
		!validEnvelopeForOpen(oldAVK, scope, envelope) {
		return SensitiveFieldEnvelope{}, ErrIntegrity
	}
	if newAVK.Version() <= oldAVK.Version() || newWrapRevision <= envelope.WrapRevision {
		return SensitiveFieldEnvelope{}, ErrInvalidBinding
	}

	dek, err := unwrapEnvelopeDEK(oldAVK, scope, envelope)
	if err != nil {
		return SensitiveFieldEnvelope{}, ErrIntegrity
	}
	defer clear(dek)

	result := cloneEnvelope(envelope)
	result.WrappingKeyID = newAVK.ID()
	result.WrappingKeyVersion = newAVK.Version()
	result.WrapRevision = newWrapRevision
	wrapAAD, err := CanonicalDEKWrapAAD(wrapBindingFromEnvelope(scope, result))
	if err != nil {
		return SensitiveFieldEnvelope{}, ErrInvalidBinding
	}
	result.WrappedDEK, err = sealAES256GCM(newAVK.material[:], dek, wrapAAD)
	clear(wrapAAD)
	if err != nil {
		return SensitiveFieldEnvelope{}, err
	}
	return result, nil
}

func unwrapEnvelopeDEK(avk *AgentVaultKey, scope FieldScope, envelope SensitiveFieldEnvelope) ([]byte, error) {
	wrapAAD, err := CanonicalDEKWrapAAD(wrapBindingFromEnvelope(scope, envelope))
	if err != nil {
		return nil, ErrIntegrity
	}
	dek, err := openAES256GCM(avk.material[:], envelope.WrappedDEK, wrapAAD)
	clear(wrapAAD)
	if err != nil || len(dek) != DataEncryptionKeyBytes {
		clear(dek)
		return nil, ErrIntegrity
	}
	return dek, nil
}

func validEnvelopeForOpen(avk *AgentVaultKey, scope FieldScope, envelope SensitiveFieldEnvelope) bool {
	if err := validateFieldScope(scope); err != nil ||
		envelope.EnvelopeVersion != EnvelopeVersionV1 ||
		envelope.AADVersion != AADVersionV1 ||
		envelope.AEADAlgorithm != AES256GCMAlgorithm ||
		envelope.WrapAlgorithm != AES256GCMAlgorithm ||
		!validValueEncoding(envelope.ValueEncoding) ||
		envelope.ValueVersion == 0 || envelope.DEKGeneration == 0 ||
		envelope.WrapRevision == 0 ||
		!validPrefixedID(envelope.DEKID, "dek") ||
		envelope.WrappingKeyID != avk.ID() ||
		envelope.WrappingKeyVersion != avk.Version() ||
		len(envelope.Ciphertext) < MinSensitiveCiphertextBytes ||
		len(envelope.Ciphertext) > MaxSensitiveCiphertextBytes ||
		len(envelope.WrappedDEK) != WrappedDEKBytes {
		return false
	}
	return true
}

func valueBindingFromEnvelope(scope FieldScope, envelope SensitiveFieldEnvelope) ValueAADBinding {
	return ValueAADBinding{
		FieldScope:    scope,
		DEKID:         envelope.DEKID,
		ValueVersion:  envelope.ValueVersion,
		DEKGeneration: envelope.DEKGeneration,
		ValueEncoding: envelope.ValueEncoding,
		AEADAlgorithm: envelope.AEADAlgorithm,
	}
}

func wrapBindingFromEnvelope(scope FieldScope, envelope SensitiveFieldEnvelope) DEKWrapAADBinding {
	return DEKWrapAADBinding{
		FieldScope:         scope,
		DEKID:              envelope.DEKID,
		DEKGeneration:      envelope.DEKGeneration,
		WrappingKeyID:      envelope.WrappingKeyID,
		WrappingKeyVersion: envelope.WrappingKeyVersion,
		WrapRevision:       envelope.WrapRevision,
		WrapAlgorithm:      envelope.WrapAlgorithm,
	}
}

func cloneEnvelope(envelope SensitiveFieldEnvelope) SensitiveFieldEnvelope {
	result := envelope
	result.Ciphertext = append([]byte(nil), envelope.Ciphertext...)
	result.WrappedDEK = append([]byte(nil), envelope.WrappedDEK...)
	return result
}

func sealAES256GCM(key, plaintext, aad []byte) ([]byte, error) {
	aead, err := randomNonceGCM(key)
	if err != nil {
		return nil, err
	}
	return aead.Seal(nil, nil, plaintext, aad), nil
}

func openAES256GCM(key, ciphertext, aad []byte) ([]byte, error) {
	aead, err := randomNonceGCM(key)
	if err != nil {
		return nil, ErrIntegrity
	}
	plaintext, err := aead.Open(nil, nil, ciphertext, aad)
	if err != nil {
		return nil, ErrIntegrity
	}
	return plaintext, nil
}

func randomNonceGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != AgentVaultKeyBytes {
		return nil, ErrIntegrity
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, ErrIntegrity
	}
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return nil, fmt.Errorf("initialize sealed AEAD: %w", err)
	}
	if aead.NonceSize() != 0 || aead.Overhead() != RandomNonceGCMOverhead {
		return nil, errors.New("sealed AEAD contract unavailable")
	}
	return aead, nil
}
