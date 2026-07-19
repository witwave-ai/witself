package sealed

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"

	"github.com/witwave-ai/witself/internal/id"
)

const (
	// AgentVaultKeyBytes is the only AVK material size supported by v1.
	AgentVaultKeyBytes = 32

	// InitialAgentVaultKeyVersion is the first logical agent key epoch.
	InitialAgentVaultKeyVersion uint64 = 1

	// AES256GCMAlgorithm is the frozen v1 algorithm label used in persisted
	// envelope metadata and authenticated data.
	AES256GCMAlgorithm = "AES_256_GCM_RANDOM_NONCE_V1"

	agentVaultKeyRecordPrefix = "witself-avk-v1:"
	agentVaultKeyRecordMagic  = "WAVK"
	agentVaultKeyRecordFormat = byte(1)
	agentVaultKeyAlgorithmID  = byte(1)
	agentVaultKeyIDBytes      = len("avk_") + 16
	agentVaultKeyHeaderBytes  = 4 + 1 + 1 + 2 + 8 + 2
	agentVaultKeyChecksumSize = sha256.Size

	agentVaultKeyFingerprintDomain = "witself/avk-fingerprint/v1\x00"
)

// AgentVaultKey is an opaque 256-bit client-held vault key. Its material is
// intentionally unexported. Generic JSON serialization is rejected; the only
// supported private encoding is EncodeAgentVaultKey for the dedicated local
// key file.
type AgentVaultKey struct {
	id       string
	version  uint64
	material [AgentVaultKeyBytes]byte
}

// AgentVaultKeyMetadata is the complete public projection that may be sent to
// the backend or rendered in ordinary status output.
type AgentVaultKeyMetadata struct {
	ID          string `json:"id"`
	Version     uint64 `json:"version"`
	Algorithm   string `json:"algorithm"`
	Fingerprint string `json:"fingerprint"`
}

// GenerateAgentVaultKey creates one new random AVK for the requested logical
// version. Generation does not perform bootstrap or storage; callers must first
// reconcile local and backend key-binding state.
func GenerateAgentVaultKey(version uint64) (*AgentVaultKey, error) {
	if version == 0 {
		return nil, ErrInvalidKeyEncoding
	}
	keyID, err := id.New("avk")
	if err != nil {
		return nil, fmt.Errorf("%w", ErrRandomSource)
	}
	key := &AgentVaultKey{id: keyID, version: version}
	if _, err := rand.Read(key.material[:]); err != nil {
		clear(key.material[:])
		return nil, fmt.Errorf("%w", ErrRandomSource)
	}
	return key, nil
}

// ID returns the public random key identifier.
func (k AgentVaultKey) ID() string { return k.id }

// Version returns the logical AVK epoch.
func (k AgentVaultKey) Version() uint64 { return k.version }

// Algorithm returns the frozen v1 wrapping algorithm.
func (k AgentVaultKey) Algorithm() string { return AES256GCMAlgorithm }

// Fingerprint returns a domain-separated SHA-256 fingerprint of the key
// material. The fingerprint is public metadata; it is not a key derivation.
func (k AgentVaultKey) Fingerprint() string {
	h := sha256.New()
	_, _ = h.Write([]byte(agentVaultKeyFingerprintDomain))
	_, _ = h.Write(k.material[:])
	return hex.EncodeToString(h.Sum(nil))
}

// Metadata returns the JSON-safe public projection of k.
func (k AgentVaultKey) Metadata() AgentVaultKeyMetadata {
	return AgentVaultKeyMetadata{
		ID:          k.ID(),
		Version:     k.Version(),
		Algorithm:   k.Algorithm(),
		Fingerprint: k.Fingerprint(),
	}
}

// String is deliberately redacted so common formatting cannot expose key
// material.
func (k AgentVaultKey) String() string {
	return fmt.Sprintf("<agent-vault-key id=%s version=%d fingerprint=%s>",
		k.ID(), k.Version(), k.Fingerprint())
}

// GoString keeps %#v formatting redacted as well.
func (k AgentVaultKey) GoString() string { return k.String() }

// Clear overwrites this in-memory AVK instance. It is idempotent and leaves the
// value invalid for every cryptographic operation. Go cannot guarantee removal
// of compiler- or runtime-created copies, so callers should also keep AVK
// lifetimes bounded and defer Clear immediately after acquiring an owned key.
// String and GoString remain value receivers so both values and pointers retain
// their redacted formatting behavior.
func (k *AgentVaultKey) Clear() {
	if k != nil {
		clear(k.material[:])
	}
}

// MarshalJSON rejects accidental generic serialization of the private key.
// Call Metadata when a public JSON projection is intended.
func (k AgentVaultKey) MarshalJSON() ([]byte, error) {
	return nil, ErrKeyDisclosure
}

// UnmarshalJSON rejects generic private-key ingestion. Local key files must use
// ParseAgentVaultKey so the strict versioned format and checksum are enforced.
func (k *AgentVaultKey) UnmarshalJSON([]byte) error {
	return ErrKeyDisclosure
}

// EncodeAgentVaultKey returns the canonical, versioned local-key record. This
// function intentionally exports private material and must only be used by the
// dedicated owner-only key-file writer.
func EncodeAgentVaultKey(key *AgentVaultKey) ([]byte, error) {
	if !validAgentVaultKey(key) {
		return nil, ErrInvalidKeyEncoding
	}

	idBytes := []byte(key.id)
	record := make([]byte, 0, agentVaultKeyHeaderBytes+len(idBytes)+AgentVaultKeyBytes+agentVaultKeyChecksumSize)
	record = append(record, agentVaultKeyRecordMagic...)
	record = append(record, agentVaultKeyRecordFormat, agentVaultKeyAlgorithmID, 0, 0)
	record = binary.BigEndian.AppendUint64(record, key.version)
	record = binary.BigEndian.AppendUint16(record, uint16(len(idBytes)))
	record = append(record, idBytes...)
	record = append(record, key.material[:]...)
	checksum := sha256.Sum256(record)
	record = append(record, checksum[:]...)

	encoded := make([]byte, len(agentVaultKeyRecordPrefix)+base64.RawURLEncoding.EncodedLen(len(record)))
	copy(encoded, agentVaultKeyRecordPrefix)
	base64.RawURLEncoding.Encode(encoded[len(agentVaultKeyRecordPrefix):], record)
	clear(record)
	return encoded, nil
}

// ParseAgentVaultKey decodes one canonical local-key record. It accepts at most
// one final newline, matching the dedicated file writer, and rejects every
// other non-canonical representation.
func ParseAgentVaultKey(input []byte) (*AgentVaultKey, error) {
	canonicalInput := input
	if bytes.HasSuffix(canonicalInput, []byte{'\n'}) {
		canonicalInput = canonicalInput[:len(canonicalInput)-1]
	}
	if len(canonicalInput) == 0 || bytes.ContainsAny(canonicalInput, "\r\n\t ") ||
		!bytes.HasPrefix(canonicalInput, []byte(agentVaultKeyRecordPrefix)) {
		return nil, ErrInvalidKeyEncoding
	}

	payload := canonicalInput[len(agentVaultKeyRecordPrefix):]
	record := make([]byte, base64.RawURLEncoding.DecodedLen(len(payload)))
	n, err := base64.RawURLEncoding.Decode(record, payload)
	if err != nil {
		clear(record)
		return nil, ErrInvalidKeyEncoding
	}
	record = record[:n]
	defer clear(record)

	minimum := agentVaultKeyHeaderBytes + agentVaultKeyIDBytes + AgentVaultKeyBytes + agentVaultKeyChecksumSize
	if len(record) != minimum || string(record[:4]) != agentVaultKeyRecordMagic ||
		record[4] != agentVaultKeyRecordFormat || record[5] != agentVaultKeyAlgorithmID ||
		record[6] != 0 || record[7] != 0 {
		return nil, ErrInvalidKeyEncoding
	}

	version := binary.BigEndian.Uint64(record[8:16])
	idLength := int(binary.BigEndian.Uint16(record[16:18]))
	if version == 0 || idLength != agentVaultKeyIDBytes {
		return nil, ErrInvalidKeyEncoding
	}
	idStart := agentVaultKeyHeaderBytes
	idEnd := idStart + idLength
	materialEnd := idEnd + AgentVaultKeyBytes
	checksumStart := materialEnd
	if checksumStart+agentVaultKeyChecksumSize != len(record) {
		return nil, ErrInvalidKeyEncoding
	}
	wantChecksum := sha256.Sum256(record[:checksumStart])
	if subtle.ConstantTimeCompare(wantChecksum[:], record[checksumStart:]) != 1 {
		return nil, ErrInvalidKeyEncoding
	}

	key := &AgentVaultKey{id: string(record[idStart:idEnd]), version: version}
	copy(key.material[:], record[idEnd:materialEnd])
	if !validAgentVaultKey(key) {
		clear(key.material[:])
		return nil, ErrInvalidKeyEncoding
	}

	reencoded, err := EncodeAgentVaultKey(key)
	if err != nil || !bytes.Equal(reencoded, canonicalInput) {
		clear(key.material[:])
		clear(reencoded)
		return nil, ErrInvalidKeyEncoding
	}
	clear(reencoded)
	return key, nil
}

func validAgentVaultKey(key *AgentVaultKey) bool {
	if key == nil || key.version == 0 || !validPrefixedID(key.id, "avk") {
		return false
	}
	var nonzero byte
	for _, b := range key.material {
		nonzero |= b
	}
	return nonzero != 0
}
