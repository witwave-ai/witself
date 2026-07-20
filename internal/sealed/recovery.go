package sealed

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"

	"golang.org/x/crypto/argon2"
)

const (
	// AVKRecoveryFormatVersionV1 is the only supported offline recovery artifact
	// format. Every algorithm and Argon2 parameter is also embedded in the
	// artifact, but v1 accepts exactly these conservative values.
	AVKRecoveryFormatVersionV1 uint32 = 1
	// AVKRecoveryAADVersionV1 identifies the canonical binary recovery AAD.
	AVKRecoveryAADVersionV1 uint32 = 1

	// AVKRecoveryKDFAlgorithm identifies the fixed v1 Argon2id profile.
	AVKRecoveryKDFAlgorithm = "ARGON2ID_V1"
	// AVKRecoveryAEADAlgorithm identifies the fixed v1 recovery AEAD.
	AVKRecoveryAEADAlgorithm = AES256GCMAlgorithm

	// AVKRecoveryArgon2Time is the fixed v1 Argon2id pass count. There
	// is deliberately no caller-selectable weak mode. 32 MiB with three passes
	// keeps interactive export/import practical while imposing meaningful cost
	// on offline guessing.
	AVKRecoveryArgon2Time uint32 = 3
	// AVKRecoveryArgon2MemoryKiB is the fixed v1 Argon2id memory cost.
	AVKRecoveryArgon2MemoryKiB uint32 = 32 * 1024
	// AVKRecoveryArgon2Parallelism is the fixed v1 Argon2id parallelism.
	AVKRecoveryArgon2Parallelism uint8 = 1
	// AVKRecoverySaltBytes is the exact random salt size for v1 artifacts.
	AVKRecoverySaltBytes = 16

	// MinAVKRecoveryPassphraseBytes prevents empty or trivially short recovery
	// passwords.
	MinAVKRecoveryPassphraseBytes = 12
	// MaxAVKRecoveryPassphraseBytes bounds passphrase copies and Argon2 input.
	MaxAVKRecoveryPassphraseBytes = 1024

	// MaxAVKRecoveryPackageBytes is checked before outer base64 or JSON parsing,
	// and therefore before Argon2 allocation. V1 artifacts are below 2 KiB.
	MaxAVKRecoveryPackageBytes = 4 * 1024

	agentVaultKeyRecoveryPrefix    = "witself-avk-recovery-v1:"
	agentVaultKeyRecoveryAADDomain = "witself/avk-recovery/aad/v1\x00"

	agentVaultKeyRecoveryAVKRecordBytes = agentVaultKeyHeaderBytes + agentVaultKeyIDBytes +
		AgentVaultKeyBytes + agentVaultKeyChecksumSize
	agentVaultKeyRecoveryPlaintextBytes = len(agentVaultKeyRecordPrefix) +
		(agentVaultKeyRecoveryAVKRecordBytes*8+5)/6
	agentVaultKeyRecoveryCiphertextBytes = agentVaultKeyRecoveryPlaintextBytes + RandomNonceGCMOverhead
)

// AVKRecoveryScope is the authenticated canonical owner scope for an offline
// artifact. Local account aliases and cloud/cell coordinates are deliberately
// absent; the stable account, realm, and agent IDs survive cell migration.
type AVKRecoveryScope struct {
	AccountID    string `json:"account_id"`
	RealmID      string `json:"realm_id"`
	OwnerAgentID string `json:"owner_agent_id"`
}

// AVKRecoveryMetadata is the safe, self-describing public projection returned
// by InspectAgentVaultKeyRecovery. It contains no ciphertext, AVK bytes,
// passphrase, or derived key. Salt and AVK fingerprints are public metadata.
type AVKRecoveryMetadata struct {
	FormatVersion     uint32                `json:"format_version"`
	AADVersion        uint32                `json:"aad_version"`
	KDFAlgorithm      string                `json:"kdf_algorithm"`
	Argon2Time        uint32                `json:"argon2_time"`
	Argon2MemoryKiB   uint32                `json:"argon2_memory_kib"`
	Argon2Parallelism uint8                 `json:"argon2_parallelism"`
	Salt              string                `json:"salt"`
	AEADAlgorithm     string                `json:"aead_algorithm"`
	Scope             AVKRecoveryScope      `json:"scope"`
	AVK               AgentVaultKeyMetadata `json:"avk"`
	CiphertextBytes   int                   `json:"ciphertext_bytes"`
}

type agentVaultKeyRecoveryRecord struct {
	FormatVersion     uint32                `json:"format_version"`
	AADVersion        uint32                `json:"aad_version"`
	KDFAlgorithm      string                `json:"kdf_algorithm"`
	Argon2Time        uint32                `json:"argon2_time"`
	Argon2MemoryKiB   uint32                `json:"argon2_memory_kib"`
	Argon2Parallelism uint8                 `json:"argon2_parallelism"`
	Salt              string                `json:"salt"`
	AEADAlgorithm     string                `json:"aead_algorithm"`
	Scope             AVKRecoveryScope      `json:"scope"`
	AVK               AgentVaultKeyMetadata `json:"avk"`
	Ciphertext        string                `json:"ciphertext"`
}

// ExportAgentVaultKeyRecovery returns one canonical, self-describing offline
// artifact encrypted under a fresh Argon2id salt and AES-GCM nonce. passphrase
// is copied and the copy is cleared; ownership of the caller's slice remains
// with the caller.
func ExportAgentVaultKeyRecovery(avk *AgentVaultKey, passphrase []byte, scope AVKRecoveryScope) ([]byte, error) {
	if !validAgentVaultKey(avk) || validateAVKRecoveryScope(scope) != nil {
		return nil, ErrInvalidRecovery
	}
	if !validAVKRecoveryPassphrase(passphrase) {
		return nil, ErrInvalidRecoveryPassphrase
	}
	passphraseCopy := append([]byte(nil), passphrase...)
	defer clear(passphraseCopy)

	salt := make([]byte, AVKRecoverySaltBytes)
	if _, err := rand.Read(salt); err != nil {
		clear(salt)
		return nil, ErrRandomSource
	}
	defer clear(salt)
	record := agentVaultKeyRecoveryRecord{
		FormatVersion:     AVKRecoveryFormatVersionV1,
		AADVersion:        AVKRecoveryAADVersionV1,
		KDFAlgorithm:      AVKRecoveryKDFAlgorithm,
		Argon2Time:        AVKRecoveryArgon2Time,
		Argon2MemoryKiB:   AVKRecoveryArgon2MemoryKiB,
		Argon2Parallelism: AVKRecoveryArgon2Parallelism,
		Salt:              base64.RawURLEncoding.EncodeToString(salt),
		AEADAlgorithm:     AVKRecoveryAEADAlgorithm,
		Scope:             scope,
		AVK:               avk.Metadata(),
	}
	aad, err := canonicalAVKRecoveryAAD(record)
	if err != nil {
		return nil, err
	}
	defer clear(aad)
	key := deriveAVKRecoveryKey(passphraseCopy, salt)
	defer clear(key)
	plaintext, err := EncodeAgentVaultKey(avk)
	if err != nil || len(plaintext) != agentVaultKeyRecoveryPlaintextBytes {
		clear(plaintext)
		return nil, ErrInvalidRecovery
	}
	defer clear(plaintext)
	ciphertext, err := sealAES256GCM(key, plaintext, aad)
	if err != nil || len(ciphertext) != agentVaultKeyRecoveryCiphertextBytes {
		clear(ciphertext)
		return nil, ErrRecoveryIntegrity
	}
	defer clear(ciphertext)
	record.Ciphertext = base64.RawURLEncoding.EncodeToString(ciphertext)
	return encodeAgentVaultKeyRecoveryRecord(record)
}

// InspectAgentVaultKeyRecovery strictly parses an artifact without invoking
// Argon2 and returns only its safe public metadata.
func InspectAgentVaultKeyRecovery(artifact []byte) (AVKRecoveryMetadata, error) {
	record, salt, ciphertext, err := parseAgentVaultKeyRecoveryRecord(artifact)
	clear(salt)
	clear(ciphertext)
	if err != nil {
		return AVKRecoveryMetadata{}, err
	}
	return recoveryMetadata(record), nil
}

// OpenAgentVaultKeyRecovery strictly parses and bounds the artifact before
// invoking Argon2id, authenticates the expected stable owner scope, decrypts
// and parses the canonical AVK record, and verifies every public AVK field.
func OpenAgentVaultKeyRecovery(artifact, passphrase []byte, expectedScope AVKRecoveryScope) (*AgentVaultKey, error) {
	record, salt, ciphertext, err := parseAgentVaultKeyRecoveryRecord(artifact)
	if err != nil {
		return nil, err
	}
	defer clear(salt)
	defer clear(ciphertext)
	if validateAVKRecoveryScope(expectedScope) != nil {
		return nil, ErrInvalidRecovery
	}
	if !equalAVKRecoveryScope(record.Scope, expectedScope) {
		return nil, ErrRecoveryIntegrity
	}
	if !validAVKRecoveryPassphrase(passphrase) {
		return nil, ErrInvalidRecoveryPassphrase
	}
	passphraseCopy := append([]byte(nil), passphrase...)
	defer clear(passphraseCopy)
	aad, err := canonicalAVKRecoveryAAD(record)
	if err != nil {
		return nil, ErrInvalidRecovery
	}
	defer clear(aad)
	key := deriveAVKRecoveryKey(passphraseCopy, salt)
	defer clear(key)
	plaintext, err := openAES256GCM(key, ciphertext, aad)
	if err != nil || len(plaintext) != agentVaultKeyRecoveryPlaintextBytes {
		clear(plaintext)
		return nil, ErrRecoveryIntegrity
	}
	defer clear(plaintext)
	avk, err := ParseAgentVaultKey(plaintext)
	if err != nil || avk == nil || !equalAgentVaultKeyMetadata(avk.Metadata(), record.AVK) {
		if avk != nil {
			avk.Clear()
		}
		return nil, ErrRecoveryIntegrity
	}
	return avk, nil
}

func recoveryMetadata(record agentVaultKeyRecoveryRecord) AVKRecoveryMetadata {
	return AVKRecoveryMetadata{
		FormatVersion: record.FormatVersion, AADVersion: record.AADVersion,
		KDFAlgorithm: record.KDFAlgorithm, Argon2Time: record.Argon2Time,
		Argon2MemoryKiB: record.Argon2MemoryKiB, Argon2Parallelism: record.Argon2Parallelism,
		Salt: record.Salt, AEADAlgorithm: record.AEADAlgorithm, Scope: record.Scope,
		AVK: record.AVK, CiphertextBytes: agentVaultKeyRecoveryCiphertextBytes,
	}
}

func encodeAgentVaultKeyRecoveryRecord(record agentVaultKeyRecoveryRecord) ([]byte, error) {
	if err := validateAgentVaultKeyRecoveryRecord(record); err != nil {
		return nil, err
	}
	jsonRecord, err := json.Marshal(record)
	if err != nil {
		return nil, ErrInvalidRecovery
	}
	defer clear(jsonRecord)
	encodedLength := len(agentVaultKeyRecoveryPrefix) + base64.RawURLEncoding.EncodedLen(len(jsonRecord))
	if encodedLength <= len(agentVaultKeyRecoveryPrefix) || encodedLength > MaxAVKRecoveryPackageBytes {
		return nil, ErrInvalidRecovery
	}
	out := make([]byte, encodedLength)
	copy(out, agentVaultKeyRecoveryPrefix)
	base64.RawURLEncoding.Encode(out[len(agentVaultKeyRecoveryPrefix):], jsonRecord)
	return out, nil
}

func parseAgentVaultKeyRecoveryRecord(artifact []byte) (agentVaultKeyRecoveryRecord, []byte, []byte, error) {
	canonical := artifact
	if bytes.HasSuffix(canonical, []byte{'\n'}) {
		canonical = canonical[:len(canonical)-1]
	}
	if len(canonical) <= len(agentVaultKeyRecoveryPrefix) || len(canonical) > MaxAVKRecoveryPackageBytes ||
		bytes.ContainsAny(canonical, "\r\n\t ") || !bytes.HasPrefix(canonical, []byte(agentVaultKeyRecoveryPrefix)) {
		return agentVaultKeyRecoveryRecord{}, nil, nil, ErrInvalidRecovery
	}
	payload := canonical[len(agentVaultKeyRecoveryPrefix):]
	jsonRecord := make([]byte, base64.RawURLEncoding.DecodedLen(len(payload)))
	n, err := base64.RawURLEncoding.Decode(jsonRecord, payload)
	if err != nil {
		clear(jsonRecord)
		return agentVaultKeyRecoveryRecord{}, nil, nil, ErrInvalidRecovery
	}
	jsonRecord = jsonRecord[:n]
	defer clear(jsonRecord)
	if len(jsonRecord) == 0 || len(jsonRecord) > MaxAVKRecoveryPackageBytes || !json.Valid(jsonRecord) {
		return agentVaultKeyRecoveryRecord{}, nil, nil, ErrInvalidRecovery
	}
	var record agentVaultKeyRecoveryRecord
	if err := json.Unmarshal(jsonRecord, &record); err != nil || validateAgentVaultKeyRecoveryRecord(record) != nil {
		return agentVaultKeyRecoveryRecord{}, nil, nil, ErrInvalidRecovery
	}
	canonicalJSON, err := json.Marshal(record)
	if err != nil || !bytes.Equal(canonicalJSON, jsonRecord) {
		clear(canonicalJSON)
		return agentVaultKeyRecoveryRecord{}, nil, nil, ErrInvalidRecovery
	}
	clear(canonicalJSON)
	reencoded, err := encodeAgentVaultKeyRecoveryRecord(record)
	if err != nil || !bytes.Equal(reencoded, canonical) {
		clear(reencoded)
		return agentVaultKeyRecoveryRecord{}, nil, nil, ErrInvalidRecovery
	}
	clear(reencoded)
	salt, err := decodeCanonicalRecoveryValue(record.Salt, AVKRecoverySaltBytes)
	if err != nil {
		return agentVaultKeyRecoveryRecord{}, nil, nil, err
	}
	ciphertext, err := decodeCanonicalRecoveryValue(record.Ciphertext, agentVaultKeyRecoveryCiphertextBytes)
	if err != nil {
		clear(salt)
		return agentVaultKeyRecoveryRecord{}, nil, nil, err
	}
	return record, salt, ciphertext, nil
}

func validateAgentVaultKeyRecoveryRecord(record agentVaultKeyRecoveryRecord) error {
	if record.FormatVersion != AVKRecoveryFormatVersionV1 ||
		record.AADVersion != AVKRecoveryAADVersionV1 ||
		record.KDFAlgorithm != AVKRecoveryKDFAlgorithm ||
		record.Argon2Time != AVKRecoveryArgon2Time ||
		record.Argon2MemoryKiB != AVKRecoveryArgon2MemoryKiB ||
		record.Argon2Parallelism != AVKRecoveryArgon2Parallelism ||
		record.AEADAlgorithm != AVKRecoveryAEADAlgorithm ||
		validateAVKRecoveryScope(record.Scope) != nil ||
		!validAgentVaultKeyMetadata(record.AVK) ||
		!validCanonicalRecoveryValue(record.Salt, AVKRecoverySaltBytes) ||
		!validCanonicalRecoveryValue(record.Ciphertext, agentVaultKeyRecoveryCiphertextBytes) {
		return ErrInvalidRecovery
	}
	return nil
}

func canonicalAVKRecoveryAAD(record agentVaultKeyRecoveryRecord) ([]byte, error) {
	// Ciphertext is populated only after encryption, so validate every other
	// field independently rather than calling validateAgentVaultKeyRecoveryRecord.
	if record.FormatVersion != AVKRecoveryFormatVersionV1 ||
		record.AADVersion != AVKRecoveryAADVersionV1 ||
		record.KDFAlgorithm != AVKRecoveryKDFAlgorithm ||
		record.Argon2Time != AVKRecoveryArgon2Time ||
		record.Argon2MemoryKiB != AVKRecoveryArgon2MemoryKiB ||
		record.Argon2Parallelism != AVKRecoveryArgon2Parallelism ||
		record.AEADAlgorithm != AVKRecoveryAEADAlgorithm ||
		validateAVKRecoveryScope(record.Scope) != nil ||
		!validAgentVaultKeyMetadata(record.AVK) ||
		!validCanonicalRecoveryValue(record.Salt, AVKRecoverySaltBytes) {
		return nil, ErrInvalidRecovery
	}
	out := make([]byte, 0, 512)
	out = append(out, agentVaultKeyRecoveryAADDomain...)
	out = binary.BigEndian.AppendUint32(out, record.FormatVersion)
	out = binary.BigEndian.AppendUint32(out, record.AADVersion)
	out = appendString(out, record.KDFAlgorithm)
	out = binary.BigEndian.AppendUint32(out, record.Argon2Time)
	out = binary.BigEndian.AppendUint32(out, record.Argon2MemoryKiB)
	out = append(out, record.Argon2Parallelism)
	out = appendString(out, record.Salt)
	out = appendString(out, record.AEADAlgorithm)
	out = appendString(out, record.Scope.AccountID)
	out = appendString(out, record.Scope.RealmID)
	out = appendString(out, record.Scope.OwnerAgentID)
	out = appendString(out, record.AVK.ID)
	out = binary.BigEndian.AppendUint64(out, record.AVK.Version)
	out = appendString(out, record.AVK.Algorithm)
	out = appendString(out, record.AVK.Fingerprint)
	return out, nil
}

func deriveAVKRecoveryKey(passphrase, salt []byte) []byte {
	return argon2.IDKey(passphrase, salt, AVKRecoveryArgon2Time, AVKRecoveryArgon2MemoryKiB,
		AVKRecoveryArgon2Parallelism, AgentVaultKeyBytes)
}

func validateAVKRecoveryScope(scope AVKRecoveryScope) error {
	if !validPrefixedID(scope.AccountID, "acc") || !validPrefixedID(scope.RealmID, "realm") ||
		!validPrefixedID(scope.OwnerAgentID, "agent") {
		return ErrInvalidRecovery
	}
	return nil
}

func equalAVKRecoveryScope(a, b AVKRecoveryScope) bool {
	return constantTimeCanonicalStringEqual(a.AccountID, b.AccountID) &&
		constantTimeCanonicalStringEqual(a.RealmID, b.RealmID) &&
		constantTimeCanonicalStringEqual(a.OwnerAgentID, b.OwnerAgentID)
}

func validAVKRecoveryPassphrase(passphrase []byte) bool {
	return len(passphrase) >= MinAVKRecoveryPassphraseBytes && len(passphrase) <= MaxAVKRecoveryPassphraseBytes
}

func validCanonicalRecoveryValue(value string, decodedBytes int) bool {
	if len(value) != base64.RawURLEncoding.EncodedLen(decodedBytes) {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(raw) != decodedBytes || base64.RawURLEncoding.EncodeToString(raw) != value {
		clear(raw)
		return false
	}
	clear(raw)
	return true
}

func decodeCanonicalRecoveryValue(value string, decodedBytes int) ([]byte, error) {
	if !validCanonicalRecoveryValue(value, decodedBytes) {
		return nil, ErrInvalidRecovery
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(raw) != decodedBytes {
		clear(raw)
		return nil, ErrInvalidRecovery
	}
	return raw, nil
}
