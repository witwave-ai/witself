package sealed

import (
	"bytes"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
)

const (
	// AVKEnrollmentRequestVersionV1 is the only accepted online-enrollment
	// request format.
	AVKEnrollmentRequestVersionV1 uint32 = 1
	// AVKEnrollmentPackageVersionV1 is the only accepted encrypted transfer
	// package format.
	AVKEnrollmentPackageVersionV1 uint32 = 1
	// AVKEnrollmentAADVersionV1 identifies the canonical enrollment AAD below.
	AVKEnrollmentAADVersionV1 uint32 = 1

	// AVKEnrollmentKeyAgreementAlgorithm identifies X25519 using raw 32-byte
	// public keys encoded as unpadded base64url strings.
	AVKEnrollmentKeyAgreementAlgorithm = "X25519_RAW_32_BASE64URL_V1"
	// AVKEnrollmentKDFAlgorithm identifies RFC 5869 HKDF with SHA-256.
	AVKEnrollmentKDFAlgorithm = "HKDF_SHA256_V1"
	// AVKEnrollmentPairingCommitmentAlgorithm identifies the public commitment
	// to the separately transferred 128-bit pairing secret.
	AVKEnrollmentPairingCommitmentAlgorithm = "SHA256_PAIRING_COMMITMENT_V1"
	// AVKEnrollmentConsumeCommitmentAlgorithm identifies the public commitment
	// to the encrypted one-time consume proof.
	AVKEnrollmentConsumeCommitmentAlgorithm = "SHA256_CONSUME_COMMITMENT_V1"
	// AVKEnrollmentSASAlgorithm identifies the short, display-only decimal SAS.
	// A SAS is never input to X25519, HKDF, or the enrollment AEAD.
	AVKEnrollmentSASAlgorithm = "SHA256_DECIMAL_6_V1"

	// AVKEnrollmentRecipientPrivateKeyBytes is the exact X25519 scalar size.
	AVKEnrollmentRecipientPrivateKeyBytes = 32
	// AVKEnrollmentPublicKeyBytes is the exact raw X25519 public-key size.
	AVKEnrollmentPublicKeyBytes = 32
	// AVKEnrollmentPairingSecretBytes is the exact out-of-band pairing-secret size.
	AVKEnrollmentPairingSecretBytes = 16
	// AVKEnrollmentConsumeProofBytes is the exact one-time consume-proof size.
	AVKEnrollmentConsumeProofBytes = 32
	// AVKEnrollmentCommitmentBytes is the exact SHA-256 commitment size.
	AVKEnrollmentCommitmentBytes = sha256.Size
	// AVKEnrollmentSASDigits is the display width of the short authentication string.
	AVKEnrollmentSASDigits = 6

	// MaxAVKEnrollmentExpiresAtUnix bounds expires_at to a canonical year-9999
	// UTC Unix second, matching common RFC 3339 and database timestamp support.
	MaxAVKEnrollmentExpiresAtUnix int64 = 253402300799

	avkEnrollmentRecipientRecordPrefix = "witself-avk-enrollment-recipient-v1:"
	avkEnrollmentPairingSecretPrefix   = "witself-avk-enrollment-pairing-v1:"
	avkEnrollmentRecipientRecordMagic  = "WAER"
	avkEnrollmentRecipientRecordFormat = byte(1)
	avkEnrollmentX25519AlgorithmID     = byte(1)

	avkEnrollmentRecipientHeaderBytes = 4 + 1 + 1 + 2
	avkEnrollmentRecipientRecordBytes = avkEnrollmentRecipientHeaderBytes +
		AVKEnrollmentRecipientPrivateKeyBytes + sha256.Size

	avkEnrollmentPayloadMagic       = "WAEP"
	avkEnrollmentPayloadFormat      = byte(1)
	avkEnrollmentPayloadHeaderBytes = 4 + 1 + 1 + 2
	avkEnrollmentAVKRecordBytes     = agentVaultKeyHeaderBytes + agentVaultKeyIDBytes +
		AgentVaultKeyBytes + agentVaultKeyChecksumSize
	avkEnrollmentAVKEncodedBytes = len(agentVaultKeyRecordPrefix) +
		(avkEnrollmentAVKRecordBytes*8+5)/6
	avkEnrollmentPayloadBytes = avkEnrollmentPayloadHeaderBytes +
		avkEnrollmentAVKEncodedBytes + AVKEnrollmentConsumeProofBytes

	// AVKEnrollmentCiphertextBytes is the exact v1 encrypted-payload length.
	AVKEnrollmentCiphertextBytes = avkEnrollmentPayloadBytes + RandomNonceGCMOverhead

	avkEnrollmentAADDomain               = "witself/avk-enrollment/aad/v1\x00"
	avkEnrollmentRequestBindingDomain    = "witself/avk-enrollment/request-binding/v1\x00"
	avkEnrollmentPairingCommitmentDomain = "witself/avk-enrollment/pairing-commitment/v1\x00"
	avkEnrollmentConsumeCommitmentDomain = "witself/avk-enrollment/consume-commitment/v1\x00"
	avkEnrollmentSASDomain               = "witself/avk-enrollment/sas/v1\x00"
	avkEnrollmentKDFSaltDomain           = "witself/avk-enrollment/kdf-salt/v1\x00"
	avkEnrollmentKDFInfo                 = "witself/avk-enrollment/content-key/v1"
)

// AVKEnrollmentRecipientKey is a short-lived X25519 private key owned by the
// target installation. Its material is deliberately opaque. Generic JSON
// serialization is rejected; EncodeAVKEnrollmentRecipientKey is the dedicated
// owner-only pending-request persistence path.
type AVKEnrollmentRecipientKey struct {
	private [AVKEnrollmentRecipientPrivateKeyBytes]byte
}

// AVKEnrollmentPairingSecret is a random out-of-band authenticator generated
// by the target installation. Only its domain-separated commitment belongs on
// the backend. EncodeAVKEnrollmentPairingSecret is the deliberate CLI transfer
// path to an already enrolled installation.
type AVKEnrollmentPairingSecret struct {
	material [AVKEnrollmentPairingSecretBytes]byte
}

// AVKEnrollmentConsumeProof is the random one-time proof recovered only by the
// target after decrypting a package. Bytes returns the deliberate opaque value
// that a client may submit to consume the backend package.
type AVKEnrollmentConsumeProof struct {
	material [AVKEnrollmentConsumeProofBytes]byte
}

// AVKEnrollmentRequest is the complete public, immutable target request. The
// pairing secret is absent; PairingCommitment is safe to persist and display.
type AVKEnrollmentRequest struct {
	RequestVersion             uint32 `json:"request_version"`
	KeyAgreementAlgorithm      string `json:"key_agreement_algorithm"`
	PairingCommitmentAlgorithm string `json:"pairing_commitment_algorithm"`
	SASAlgorithm               string `json:"sas_algorithm"`
	AccountID                  string `json:"account_id"`
	RealmID                    string `json:"realm_id"`
	OwnerAgentID               string `json:"owner_agent_id"`
	EnrollmentRequestID        string `json:"enrollment_request_id"`
	TargetInstallationID       string `json:"target_installation_id"`
	TargetPublicKey            string `json:"target_public_key"`
	ExpiresAt                  int64  `json:"expires_at"`
	PairingCommitment          string `json:"pairing_commitment"`
}

// AVKEnrollmentRequestOptions supplies caller-owned immutable coordinates for
// a new request. ExpiresAt is a positive Unix second no later than year 9999.
type AVKEnrollmentRequestOptions struct {
	AccountID            string
	RealmID              string
	OwnerAgentID         string
	EnrollmentRequestID  string
	TargetInstallationID string
	ExpiresAt            int64
}

// AVKEnrollmentPackage is the public encrypted AVK transfer package. It
// contains no AVK material, pairing secret, consume proof, or derived key.
type AVKEnrollmentPackage struct {
	PackageVersion             uint32                `json:"package_version"`
	AADVersion                 uint32                `json:"aad_version"`
	KDFAlgorithm               string                `json:"kdf_algorithm"`
	AEADAlgorithm              string                `json:"aead_algorithm"`
	ConsumeCommitmentAlgorithm string                `json:"consume_commitment_algorithm"`
	Request                    AVKEnrollmentRequest  `json:"request"`
	SourceInstallationID       string                `json:"source_installation_id"`
	SourceEphemeralPublicKey   string                `json:"source_ephemeral_public_key"`
	AVK                        AgentVaultKeyMetadata `json:"avk"`
	ConsumeCommitment          string                `json:"consume_commitment"`
	Ciphertext                 []byte                `json:"ciphertext"`
}

// GenerateAVKEnrollmentRecipientKey creates a fresh short-lived X25519 key.
func GenerateAVKEnrollmentRecipientKey() (*AVKEnrollmentRecipientKey, error) {
	private, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, ErrRandomSource
	}
	raw := private.Bytes()
	defer clear(raw)
	if len(raw) != AVKEnrollmentRecipientPrivateKeyBytes {
		return nil, ErrRandomSource
	}
	result := &AVKEnrollmentRecipientKey{}
	copy(result.private[:], raw)
	return result, nil
}

// PublicKey returns the canonical unpadded base64url X25519 public key.
func (k *AVKEnrollmentRecipientKey) PublicKey() (string, error) {
	private, err := enrollmentECDHPrivateKey(k)
	if err != nil {
		return "", err
	}
	return encodeEnrollmentPublicKey(private.PublicKey().Bytes()), nil
}

// Clear overwrites the locally held X25519 scalar. It is idempotent.
func (k *AVKEnrollmentRecipientKey) Clear() {
	if k != nil {
		clear(k.private[:])
	}
}

func (k AVKEnrollmentRecipientKey) String() string {
	return "<avk-enrollment-recipient-key redacted>"
}

// GoString returns the same redacted diagnostic representation as String.
func (k AVKEnrollmentRecipientKey) GoString() string { return k.String() }

// MarshalJSON rejects generic serialization of the recipient private key.
func (k AVKEnrollmentRecipientKey) MarshalJSON() ([]byte, error) {
	return nil, ErrEnrollmentDisclosure
}

// UnmarshalJSON rejects generic deserialization of the recipient private key.
func (k *AVKEnrollmentRecipientKey) UnmarshalJSON([]byte) error {
	return ErrEnrollmentDisclosure
}

// EncodeAVKEnrollmentRecipientKey returns the canonical owner-only pending-key
// record. It deliberately exports private material and must not be logged.
func EncodeAVKEnrollmentRecipientKey(key *AVKEnrollmentRecipientKey) ([]byte, error) {
	if _, err := enrollmentECDHPrivateKey(key); err != nil {
		return nil, ErrInvalidEnrollment
	}
	record := make([]byte, 0, avkEnrollmentRecipientRecordBytes)
	record = append(record, avkEnrollmentRecipientRecordMagic...)
	record = append(record, avkEnrollmentRecipientRecordFormat, avkEnrollmentX25519AlgorithmID, 0, 0)
	record = append(record, key.private[:]...)
	checksum := sha256.Sum256(record)
	record = append(record, checksum[:]...)

	encoded := make([]byte, len(avkEnrollmentRecipientRecordPrefix)+base64.RawURLEncoding.EncodedLen(len(record)))
	copy(encoded, avkEnrollmentRecipientRecordPrefix)
	base64.RawURLEncoding.Encode(encoded[len(avkEnrollmentRecipientRecordPrefix):], record)
	clear(record)
	return encoded, nil
}

// ParseAVKEnrollmentRecipientKey accepts exactly one canonical private-key
// record and, like ParseAgentVaultKey, at most one trailing newline.
func ParseAVKEnrollmentRecipientKey(input []byte) (*AVKEnrollmentRecipientKey, error) {
	canonical := trimOneFinalNewline(input)
	if len(canonical) == 0 || bytes.ContainsAny(canonical, "\r\n\t ") ||
		!bytes.HasPrefix(canonical, []byte(avkEnrollmentRecipientRecordPrefix)) {
		return nil, ErrInvalidEnrollment
	}
	payload := canonical[len(avkEnrollmentRecipientRecordPrefix):]
	record := make([]byte, base64.RawURLEncoding.DecodedLen(len(payload)))
	n, err := base64.RawURLEncoding.Decode(record, payload)
	if err != nil {
		clear(record)
		return nil, ErrInvalidEnrollment
	}
	record = record[:n]
	defer clear(record)
	if len(record) != avkEnrollmentRecipientRecordBytes ||
		string(record[:4]) != avkEnrollmentRecipientRecordMagic ||
		record[4] != avkEnrollmentRecipientRecordFormat ||
		record[5] != avkEnrollmentX25519AlgorithmID || record[6] != 0 || record[7] != 0 {
		return nil, ErrInvalidEnrollment
	}
	checksumStart := len(record) - sha256.Size
	wantChecksum := sha256.Sum256(record[:checksumStart])
	if subtle.ConstantTimeCompare(wantChecksum[:], record[checksumStart:]) != 1 {
		return nil, ErrInvalidEnrollment
	}
	key := &AVKEnrollmentRecipientKey{}
	copy(key.private[:], record[avkEnrollmentRecipientHeaderBytes:checksumStart])
	if _, err := enrollmentECDHPrivateKey(key); err != nil {
		key.Clear()
		return nil, ErrInvalidEnrollment
	}
	reencoded, err := EncodeAVKEnrollmentRecipientKey(key)
	if err != nil || !bytes.Equal(reencoded, canonical) {
		key.Clear()
		clear(reencoded)
		return nil, ErrInvalidEnrollment
	}
	clear(reencoded)
	return key, nil
}

// GenerateAVKEnrollmentPairingSecret creates a separate 128-bit out-of-band
// authenticator. It is never used as an encryption or key-derivation key.
func GenerateAVKEnrollmentPairingSecret() (*AVKEnrollmentPairingSecret, error) {
	secret := &AVKEnrollmentPairingSecret{}
	if _, err := rand.Read(secret.material[:]); err != nil {
		secret.Clear()
		return nil, ErrRandomSource
	}
	if !nonzeroBytes(secret.material[:]) {
		secret.Clear()
		return nil, ErrRandomSource
	}
	return secret, nil
}

// EncodeAVKEnrollmentPairingSecret returns the canonical deliberate CLI
// transfer representation of the out-of-band secret.
func EncodeAVKEnrollmentPairingSecret(secret *AVKEnrollmentPairingSecret) (string, error) {
	if !validPairingSecret(secret) {
		return "", ErrInvalidEnrollment
	}
	return avkEnrollmentPairingSecretPrefix + base64.RawURLEncoding.EncodeToString(secret.material[:]), nil
}

// ParseAVKEnrollmentPairingSecret parses only the canonical CLI form.
func ParseAVKEnrollmentPairingSecret(input string) (*AVKEnrollmentPairingSecret, error) {
	if len(input) != len(avkEnrollmentPairingSecretPrefix)+base64.RawURLEncoding.EncodedLen(AVKEnrollmentPairingSecretBytes) ||
		!bytes.HasPrefix([]byte(input), []byte(avkEnrollmentPairingSecretPrefix)) {
		return nil, ErrInvalidEnrollment
	}
	raw, err := base64.RawURLEncoding.DecodeString(input[len(avkEnrollmentPairingSecretPrefix):])
	if err != nil || len(raw) != AVKEnrollmentPairingSecretBytes {
		clear(raw)
		return nil, ErrInvalidEnrollment
	}
	defer clear(raw)
	secret := &AVKEnrollmentPairingSecret{}
	copy(secret.material[:], raw)
	if !validPairingSecret(secret) {
		secret.Clear()
		return nil, ErrInvalidEnrollment
	}
	encoded, err := EncodeAVKEnrollmentPairingSecret(secret)
	if err != nil || encoded != input {
		secret.Clear()
		return nil, ErrInvalidEnrollment
	}
	return secret, nil
}

// Clear overwrites the pairing secret. It is idempotent.
func (s *AVKEnrollmentPairingSecret) Clear() {
	if s != nil {
		clear(s.material[:])
	}
}

func (s AVKEnrollmentPairingSecret) String() string {
	return "<avk-enrollment-pairing-secret redacted>"
}

// GoString returns the same redacted diagnostic representation as String.
func (s AVKEnrollmentPairingSecret) GoString() string { return s.String() }

// MarshalJSON rejects generic serialization of the pairing secret.
func (s AVKEnrollmentPairingSecret) MarshalJSON() ([]byte, error) {
	return nil, ErrEnrollmentDisclosure
}

// UnmarshalJSON rejects generic deserialization of the pairing secret.
func (s *AVKEnrollmentPairingSecret) UnmarshalJSON([]byte) error {
	return ErrEnrollmentDisclosure
}

// NewAVKEnrollmentRequest constructs a canonical public request and commits to
// the separate pairing secret without placing that secret in the request.
func NewAVKEnrollmentRequest(recipient *AVKEnrollmentRecipientKey, pairingSecret *AVKEnrollmentPairingSecret, options AVKEnrollmentRequestOptions) (AVKEnrollmentRequest, error) {
	publicKey, err := recipient.PublicKey()
	if err != nil || !validPairingSecret(pairingSecret) {
		return AVKEnrollmentRequest{}, ErrInvalidEnrollment
	}
	request := AVKEnrollmentRequest{
		RequestVersion:             AVKEnrollmentRequestVersionV1,
		KeyAgreementAlgorithm:      AVKEnrollmentKeyAgreementAlgorithm,
		PairingCommitmentAlgorithm: AVKEnrollmentPairingCommitmentAlgorithm,
		SASAlgorithm:               AVKEnrollmentSASAlgorithm,
		AccountID:                  options.AccountID,
		RealmID:                    options.RealmID,
		OwnerAgentID:               options.OwnerAgentID,
		EnrollmentRequestID:        options.EnrollmentRequestID,
		TargetInstallationID:       options.TargetInstallationID,
		TargetPublicKey:            publicKey,
		ExpiresAt:                  options.ExpiresAt,
	}
	if err := validateAVKEnrollmentRequestBinding(request); err != nil {
		return AVKEnrollmentRequest{}, err
	}
	request.PairingCommitment, err = pairingCommitment(pairingSecret, request)
	if err != nil {
		return AVKEnrollmentRequest{}, err
	}
	return request, nil
}

// VerifyAVKEnrollmentPairingSecret authenticates the out-of-band pairing
// secret against the backend-held public request. Clients should call this
// before loading an AVK; SealAVKEnrollmentPackage repeats the check.
func VerifyAVKEnrollmentPairingSecret(secret *AVKEnrollmentPairingSecret, request AVKEnrollmentRequest) error {
	if !validPairingSecret(secret) || validateAVKEnrollmentRequest(request) != nil {
		return ErrEnrollmentIntegrity
	}
	commitment, err := pairingCommitment(secret, request)
	if err != nil || !constantTimeCanonicalStringEqual(commitment, request.PairingCommitment) {
		return ErrEnrollmentIntegrity
	}
	return nil
}

// AVKEnrollmentSAS derives a six-digit display-only short authentication
// string from the separate pairing secret and immutable request binding. The
// returned value is for human comparison only and is never a cryptographic key.
func AVKEnrollmentSAS(secret *AVKEnrollmentPairingSecret, request AVKEnrollmentRequest) (string, error) {
	if !validPairingSecret(secret) || validateAVKEnrollmentRequest(request) != nil {
		return "", ErrInvalidEnrollment
	}
	binding, err := canonicalAVKEnrollmentRequestBinding(request)
	if err != nil {
		return "", err
	}
	defer clear(binding)
	h := sha256.New()
	_, _ = h.Write([]byte(avkEnrollmentSASDomain))
	_, _ = h.Write(secret.material[:])
	_, _ = h.Write(binding)
	digest := h.Sum(nil)
	value := binary.BigEndian.Uint32(digest[:4]) % 1_000_000
	clear(digest)
	return fmt.Sprintf("%0*d", AVKEnrollmentSASDigits, value), nil
}

// SealAVKEnrollmentPackage authenticates the pairing secret, creates an
// ephemeral X25519 source key and random consume proof, and encrypts the AVK.
// HKDF input is exclusively the X25519 shared secret plus AAD-derived salt;
// neither pairingSecret nor the human SAS is a KDF input.
func SealAVKEnrollmentPackage(avk *AgentVaultKey, pairingSecret *AVKEnrollmentPairingSecret, request AVKEnrollmentRequest, sourceInstallationID string) (AVKEnrollmentPackage, error) {
	if !validAgentVaultKey(avk) || !validPrefixedID(sourceInstallationID, "loc") {
		return AVKEnrollmentPackage{}, ErrInvalidEnrollment
	}
	if err := VerifyAVKEnrollmentPairingSecret(pairingSecret, request); err != nil {
		return AVKEnrollmentPackage{}, err
	}

	targetPublic, err := decodeEnrollmentPublicKey(request.TargetPublicKey)
	if err != nil {
		return AVKEnrollmentPackage{}, ErrEnrollmentIntegrity
	}
	target, err := ecdh.X25519().NewPublicKey(targetPublic)
	clear(targetPublic)
	if err != nil {
		return AVKEnrollmentPackage{}, ErrEnrollmentIntegrity
	}
	source, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return AVKEnrollmentPackage{}, ErrRandomSource
	}

	var consumeProof [AVKEnrollmentConsumeProofBytes]byte
	if _, err := rand.Read(consumeProof[:]); err != nil {
		return AVKEnrollmentPackage{}, ErrRandomSource
	}
	defer clear(consumeProof[:])
	if !nonzeroBytes(consumeProof[:]) {
		return AVKEnrollmentPackage{}, ErrRandomSource
	}

	avkRecord, err := EncodeAgentVaultKey(avk)
	if err != nil || len(avkRecord) != avkEnrollmentAVKEncodedBytes {
		clear(avkRecord)
		return AVKEnrollmentPackage{}, ErrInvalidEnrollment
	}
	defer clear(avkRecord)
	payload := encodeAVKEnrollmentPayload(avkRecord, consumeProof[:])
	defer clear(payload)
	consumeCommitmentValue, err := AVKEnrollmentConsumeCommitment(request.EnrollmentRequestID, consumeProof[:])
	if err != nil {
		return AVKEnrollmentPackage{}, ErrInvalidEnrollment
	}

	packageValue := AVKEnrollmentPackage{
		PackageVersion:             AVKEnrollmentPackageVersionV1,
		AADVersion:                 AVKEnrollmentAADVersionV1,
		KDFAlgorithm:               AVKEnrollmentKDFAlgorithm,
		AEADAlgorithm:              AES256GCMAlgorithm,
		ConsumeCommitmentAlgorithm: AVKEnrollmentConsumeCommitmentAlgorithm,
		Request:                    request,
		SourceInstallationID:       sourceInstallationID,
		SourceEphemeralPublicKey:   encodeEnrollmentPublicKey(source.PublicKey().Bytes()),
		AVK:                        avk.Metadata(),
		ConsumeCommitment:          consumeCommitmentValue,
	}
	aad, err := canonicalAVKEnrollmentAAD(packageValue)
	if err != nil {
		return AVKEnrollmentPackage{}, err
	}
	defer clear(aad)
	contentKey, err := deriveAVKEnrollmentContentKey(source, target, aad)
	if err != nil {
		return AVKEnrollmentPackage{}, err
	}
	defer clear(contentKey)
	packageValue.Ciphertext, err = sealAES256GCM(contentKey, payload, aad)
	if err != nil || len(packageValue.Ciphertext) != AVKEnrollmentCiphertextBytes {
		clear(packageValue.Ciphertext)
		return AVKEnrollmentPackage{}, ErrEnrollmentIntegrity
	}
	return packageValue, nil
}

// OpenAVKEnrollmentPackage verifies the expected request and recipient,
// decrypts the package, parses the canonical AVK record, verifies all public
// AVK metadata and the consume commitment, and returns owned secret values.
func OpenAVKEnrollmentPackage(recipient *AVKEnrollmentRecipientKey, pairingSecret *AVKEnrollmentPairingSecret, expectedRequest AVKEnrollmentRequest, packageValue AVKEnrollmentPackage) (*AgentVaultKey, *AVKEnrollmentConsumeProof, error) {
	if validateAVKEnrollmentPackage(packageValue) != nil ||
		validateAVKEnrollmentRequest(expectedRequest) != nil ||
		!equalAVKEnrollmentRequest(packageValue.Request, expectedRequest) ||
		VerifyAVKEnrollmentPairingSecret(pairingSecret, expectedRequest) != nil {
		return nil, nil, ErrEnrollmentIntegrity
	}
	private, err := enrollmentECDHPrivateKey(recipient)
	if err != nil {
		return nil, nil, ErrEnrollmentIntegrity
	}
	publicKey := encodeEnrollmentPublicKey(private.PublicKey().Bytes())
	if !constantTimeCanonicalStringEqual(publicKey, expectedRequest.TargetPublicKey) {
		return nil, nil, ErrEnrollmentIntegrity
	}

	sourceRaw, err := decodeEnrollmentPublicKey(packageValue.SourceEphemeralPublicKey)
	if err != nil {
		return nil, nil, ErrEnrollmentIntegrity
	}
	source, err := ecdh.X25519().NewPublicKey(sourceRaw)
	clear(sourceRaw)
	if err != nil {
		return nil, nil, ErrEnrollmentIntegrity
	}
	aad, err := canonicalAVKEnrollmentAAD(packageValue)
	if err != nil {
		return nil, nil, ErrEnrollmentIntegrity
	}
	defer clear(aad)
	contentKey, err := deriveAVKEnrollmentContentKey(private, source, aad)
	if err != nil {
		return nil, nil, ErrEnrollmentIntegrity
	}
	defer clear(contentKey)
	payload, err := openAES256GCM(contentKey, packageValue.Ciphertext, aad)
	if err != nil || len(payload) != avkEnrollmentPayloadBytes {
		clear(payload)
		return nil, nil, ErrEnrollmentIntegrity
	}
	defer clear(payload)

	avkRecord, proofBytes, err := parseAVKEnrollmentPayload(payload)
	if err != nil {
		return nil, nil, ErrEnrollmentIntegrity
	}
	defer clear(avkRecord)
	defer clear(proofBytes)
	avk, err := ParseAgentVaultKey(avkRecord)
	if err != nil || !equalAgentVaultKeyMetadata(avk.Metadata(), packageValue.AVK) {
		if avk != nil {
			avk.Clear()
		}
		return nil, nil, ErrEnrollmentIntegrity
	}
	if !VerifyAVKEnrollmentConsumeProof(packageValue.Request.EnrollmentRequestID, packageValue.ConsumeCommitment, proofBytes) {
		avk.Clear()
		return nil, nil, ErrEnrollmentIntegrity
	}
	proof := &AVKEnrollmentConsumeProof{}
	copy(proof.material[:], proofBytes)
	return avk, proof, nil
}

// Bytes returns a fresh copy of the opaque one-time consume proof for an
// explicit package-consume request. The caller owns and should clear the copy.
func (p *AVKEnrollmentConsumeProof) Bytes() ([]byte, error) {
	if p == nil || !nonzeroBytes(p.material[:]) {
		return nil, ErrInvalidEnrollment
	}
	return append([]byte(nil), p.material[:]...), nil
}

// Commitment returns the public domain-separated SHA-256 commitment.
func (p *AVKEnrollmentConsumeProof) Commitment(enrollmentRequestID string) (string, error) {
	if p == nil || !nonzeroBytes(p.material[:]) {
		return "", ErrInvalidEnrollment
	}
	return AVKEnrollmentConsumeCommitment(enrollmentRequestID, p.material[:])
}

// AVKEnrollmentConsumeCommitment computes the public domain-separated
// commitment that the backend compares before consuming and nulling a package.
// proof is the exact 32-byte value obtained through AVKEnrollmentConsumeProof.Bytes.
func AVKEnrollmentConsumeCommitment(enrollmentRequestID string, proof []byte) (string, error) {
	if !validPrefixedID(enrollmentRequestID, "enr") || len(proof) != AVKEnrollmentConsumeProofBytes ||
		!nonzeroBytes(proof) {
		return "", ErrInvalidEnrollment
	}
	h := sha256.New()
	_, _ = h.Write([]byte(avkEnrollmentConsumeCommitmentDomain))
	_, _ = h.Write([]byte(enrollmentRequestID))
	_, _ = h.Write(proof)
	return hex.EncodeToString(h.Sum(nil)), nil
}

// VerifyAVKEnrollmentConsumeProof compares a submitted opaque proof with a
// canonical public commitment. It returns false for every malformed input.
func VerifyAVKEnrollmentConsumeProof(enrollmentRequestID, commitment string, proof []byte) bool {
	if !validCommitment(commitment) {
		return false
	}
	want, err := AVKEnrollmentConsumeCommitment(enrollmentRequestID, proof)
	if err != nil {
		return false
	}
	return constantTimeCanonicalStringEqual(want, commitment)
}

// Clear overwrites the one-time consume proof. It is idempotent.
func (p *AVKEnrollmentConsumeProof) Clear() {
	if p != nil {
		clear(p.material[:])
	}
}

func (p AVKEnrollmentConsumeProof) String() string {
	return "<avk-enrollment-consume-proof redacted>"
}

// GoString returns the same redacted diagnostic representation as String.
func (p AVKEnrollmentConsumeProof) GoString() string { return p.String() }

// MarshalJSON rejects generic serialization of the consume proof.
func (p AVKEnrollmentConsumeProof) MarshalJSON() ([]byte, error) {
	return nil, ErrEnrollmentDisclosure
}

// UnmarshalJSON rejects generic deserialization of the consume proof.
func (p *AVKEnrollmentConsumeProof) UnmarshalJSON([]byte) error {
	return ErrEnrollmentDisclosure
}

func canonicalAVKEnrollmentRequestBinding(request AVKEnrollmentRequest) ([]byte, error) {
	if err := validateAVKEnrollmentRequestBinding(request); err != nil {
		return nil, err
	}
	out := make([]byte, 0, 384)
	out = append(out, avkEnrollmentRequestBindingDomain...)
	out = binary.BigEndian.AppendUint32(out, request.RequestVersion)
	out = appendString(out, request.KeyAgreementAlgorithm)
	out = appendString(out, request.PairingCommitmentAlgorithm)
	out = appendString(out, request.SASAlgorithm)
	out = appendString(out, request.AccountID)
	out = appendString(out, request.RealmID)
	out = appendString(out, request.OwnerAgentID)
	out = appendString(out, request.EnrollmentRequestID)
	out = appendString(out, request.TargetInstallationID)
	out = appendString(out, request.TargetPublicKey)
	out = binary.BigEndian.AppendUint64(out, uint64(request.ExpiresAt))
	return out, nil
}

// canonicalAVKEnrollmentAAD binds every immutable scope field, both X25519
// public keys, expiry, AVK public metadata, commitments, versions, and
// algorithms. Ciphertext is intentionally excluded.
func canonicalAVKEnrollmentAAD(packageValue AVKEnrollmentPackage) ([]byte, error) {
	if err := validateAVKEnrollmentPackageWithoutCiphertext(packageValue); err != nil {
		return nil, err
	}
	r := packageValue.Request
	out := make([]byte, 0, 768)
	out = append(out, avkEnrollmentAADDomain...)
	out = binary.BigEndian.AppendUint32(out, packageValue.PackageVersion)
	out = binary.BigEndian.AppendUint32(out, packageValue.AADVersion)
	out = appendString(out, r.KeyAgreementAlgorithm)
	out = appendString(out, packageValue.KDFAlgorithm)
	out = appendString(out, packageValue.AEADAlgorithm)
	out = appendString(out, r.PairingCommitmentAlgorithm)
	out = appendString(out, packageValue.ConsumeCommitmentAlgorithm)
	out = appendString(out, r.SASAlgorithm)
	out = appendString(out, r.AccountID)
	out = appendString(out, r.RealmID)
	out = appendString(out, r.OwnerAgentID)
	out = appendString(out, r.EnrollmentRequestID)
	out = appendString(out, packageValue.SourceInstallationID)
	out = appendString(out, r.TargetInstallationID)
	out = appendString(out, r.TargetPublicKey)
	out = binary.BigEndian.AppendUint64(out, uint64(r.ExpiresAt))
	out = appendString(out, packageValue.SourceEphemeralPublicKey)
	out = appendString(out, r.PairingCommitment)
	out = appendString(out, packageValue.ConsumeCommitment)
	out = appendString(out, packageValue.AVK.ID)
	out = binary.BigEndian.AppendUint64(out, packageValue.AVK.Version)
	out = appendString(out, packageValue.AVK.Algorithm)
	out = appendString(out, packageValue.AVK.Fingerprint)
	return out, nil
}

func pairingCommitment(secret *AVKEnrollmentPairingSecret, request AVKEnrollmentRequest) (string, error) {
	if !validPairingSecret(secret) {
		return "", ErrInvalidEnrollment
	}
	binding, err := canonicalAVKEnrollmentRequestBinding(request)
	if err != nil {
		return "", err
	}
	defer clear(binding)
	h := sha256.New()
	_, _ = h.Write([]byte(avkEnrollmentPairingCommitmentDomain))
	_, _ = h.Write(secret.material[:])
	_, _ = h.Write(binding)
	return hex.EncodeToString(h.Sum(nil)), nil
}

func deriveAVKEnrollmentContentKey(private *ecdh.PrivateKey, public *ecdh.PublicKey, aad []byte) ([]byte, error) {
	shared, err := private.ECDH(public)
	if err != nil || len(shared) == 0 {
		clear(shared)
		return nil, ErrEnrollmentIntegrity
	}
	defer clear(shared)
	h := sha256.New()
	_, _ = h.Write([]byte(avkEnrollmentKDFSaltDomain))
	_, _ = h.Write(aad)
	salt := h.Sum(nil)
	defer clear(salt)
	key, err := hkdf.Key(sha256.New, shared, salt, avkEnrollmentKDFInfo, AgentVaultKeyBytes)
	if err != nil || len(key) != AgentVaultKeyBytes {
		clear(key)
		return nil, ErrEnrollmentIntegrity
	}
	return key, nil
}

func encodeAVKEnrollmentPayload(avkRecord, proof []byte) []byte {
	out := make([]byte, 0, avkEnrollmentPayloadBytes)
	out = append(out, avkEnrollmentPayloadMagic...)
	out = append(out, avkEnrollmentPayloadFormat, 0)
	out = binary.BigEndian.AppendUint16(out, uint16(len(avkRecord)))
	out = append(out, avkRecord...)
	out = append(out, proof...)
	return out
}

func parseAVKEnrollmentPayload(payload []byte) ([]byte, []byte, error) {
	if len(payload) != avkEnrollmentPayloadBytes || string(payload[:4]) != avkEnrollmentPayloadMagic ||
		payload[4] != avkEnrollmentPayloadFormat || payload[5] != 0 {
		return nil, nil, ErrEnrollmentIntegrity
	}
	avkLength := int(binary.BigEndian.Uint16(payload[6:8]))
	if avkLength != avkEnrollmentAVKEncodedBytes {
		return nil, nil, ErrEnrollmentIntegrity
	}
	avkEnd := avkEnrollmentPayloadHeaderBytes + avkLength
	if avkEnd+AVKEnrollmentConsumeProofBytes != len(payload) {
		return nil, nil, ErrEnrollmentIntegrity
	}
	return append([]byte(nil), payload[avkEnrollmentPayloadHeaderBytes:avkEnd]...),
		append([]byte(nil), payload[avkEnd:]...), nil
}

func validateAVKEnrollmentRequestBinding(request AVKEnrollmentRequest) error {
	if request.RequestVersion != AVKEnrollmentRequestVersionV1 ||
		request.KeyAgreementAlgorithm != AVKEnrollmentKeyAgreementAlgorithm ||
		request.PairingCommitmentAlgorithm != AVKEnrollmentPairingCommitmentAlgorithm ||
		request.SASAlgorithm != AVKEnrollmentSASAlgorithm ||
		!validPrefixedID(request.AccountID, "acc") ||
		!validPrefixedID(request.RealmID, "realm") ||
		!validPrefixedID(request.OwnerAgentID, "agent") ||
		!validPrefixedID(request.EnrollmentRequestID, "enr") ||
		!validPrefixedID(request.TargetInstallationID, "loc") ||
		request.ExpiresAt <= 0 || request.ExpiresAt > MaxAVKEnrollmentExpiresAtUnix {
		return ErrInvalidEnrollment
	}
	if _, err := decodeEnrollmentPublicKey(request.TargetPublicKey); err != nil {
		return ErrInvalidEnrollment
	}
	return nil
}

func validateAVKEnrollmentRequest(request AVKEnrollmentRequest) error {
	if err := validateAVKEnrollmentRequestBinding(request); err != nil || !validCommitment(request.PairingCommitment) {
		return ErrInvalidEnrollment
	}
	return nil
}

func validateAVKEnrollmentPackageWithoutCiphertext(packageValue AVKEnrollmentPackage) error {
	if packageValue.PackageVersion != AVKEnrollmentPackageVersionV1 ||
		packageValue.AADVersion != AVKEnrollmentAADVersionV1 ||
		packageValue.KDFAlgorithm != AVKEnrollmentKDFAlgorithm ||
		packageValue.AEADAlgorithm != AES256GCMAlgorithm ||
		packageValue.ConsumeCommitmentAlgorithm != AVKEnrollmentConsumeCommitmentAlgorithm ||
		validateAVKEnrollmentRequest(packageValue.Request) != nil ||
		!validPrefixedID(packageValue.SourceInstallationID, "loc") ||
		!validCommitment(packageValue.ConsumeCommitment) ||
		!validAgentVaultKeyMetadata(packageValue.AVK) {
		return ErrInvalidEnrollment
	}
	if _, err := decodeEnrollmentPublicKey(packageValue.SourceEphemeralPublicKey); err != nil {
		return ErrInvalidEnrollment
	}
	return nil
}

func validateAVKEnrollmentPackage(packageValue AVKEnrollmentPackage) error {
	if err := validateAVKEnrollmentPackageWithoutCiphertext(packageValue); err != nil ||
		len(packageValue.Ciphertext) != AVKEnrollmentCiphertextBytes {
		return ErrInvalidEnrollment
	}
	return nil
}

func validAgentVaultKeyMetadata(metadata AgentVaultKeyMetadata) bool {
	return validPrefixedID(metadata.ID, "avk") && metadata.Version > 0 &&
		metadata.Algorithm == AES256GCMAlgorithm && validCommitment(metadata.Fingerprint)
}

func equalAgentVaultKeyMetadata(a, b AgentVaultKeyMetadata) bool {
	return a.Version == b.Version && a.Algorithm == b.Algorithm &&
		constantTimeCanonicalStringEqual(a.ID, b.ID) &&
		constantTimeCanonicalStringEqual(a.Fingerprint, b.Fingerprint)
}

func equalAVKEnrollmentRequest(a, b AVKEnrollmentRequest) bool {
	return a.RequestVersion == b.RequestVersion && a.ExpiresAt == b.ExpiresAt &&
		constantTimeCanonicalStringEqual(a.KeyAgreementAlgorithm, b.KeyAgreementAlgorithm) &&
		constantTimeCanonicalStringEqual(a.PairingCommitmentAlgorithm, b.PairingCommitmentAlgorithm) &&
		constantTimeCanonicalStringEqual(a.SASAlgorithm, b.SASAlgorithm) &&
		constantTimeCanonicalStringEqual(a.AccountID, b.AccountID) &&
		constantTimeCanonicalStringEqual(a.RealmID, b.RealmID) &&
		constantTimeCanonicalStringEqual(a.OwnerAgentID, b.OwnerAgentID) &&
		constantTimeCanonicalStringEqual(a.EnrollmentRequestID, b.EnrollmentRequestID) &&
		constantTimeCanonicalStringEqual(a.TargetInstallationID, b.TargetInstallationID) &&
		constantTimeCanonicalStringEqual(a.TargetPublicKey, b.TargetPublicKey) &&
		constantTimeCanonicalStringEqual(a.PairingCommitment, b.PairingCommitment)
}

func enrollmentECDHPrivateKey(key *AVKEnrollmentRecipientKey) (*ecdh.PrivateKey, error) {
	if key == nil || !nonzeroBytes(key.private[:]) {
		return nil, ErrInvalidEnrollment
	}
	private, err := ecdh.X25519().NewPrivateKey(key.private[:])
	if err != nil {
		return nil, ErrInvalidEnrollment
	}
	return private, nil
}

func encodeEnrollmentPublicKey(raw []byte) string {
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeEnrollmentPublicKey(encoded string) ([]byte, error) {
	if len(encoded) != base64.RawURLEncoding.EncodedLen(AVKEnrollmentPublicKeyBytes) {
		return nil, ErrInvalidEnrollment
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(raw) != AVKEnrollmentPublicKeyBytes || encodeEnrollmentPublicKey(raw) != encoded {
		clear(raw)
		return nil, ErrInvalidEnrollment
	}
	if _, err := ecdh.X25519().NewPublicKey(raw); err != nil {
		clear(raw)
		return nil, ErrInvalidEnrollment
	}
	return raw, nil
}

func validPairingSecret(secret *AVKEnrollmentPairingSecret) bool {
	return secret != nil && nonzeroBytes(secret.material[:])
}

func validCommitment(value string) bool {
	if len(value) != 2*AVKEnrollmentCommitmentBytes {
		return false
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != AVKEnrollmentCommitmentBytes || hex.EncodeToString(decoded) != value {
		clear(decoded)
		return false
	}
	clear(decoded)
	return true
}

func nonzeroBytes(value []byte) bool {
	var nonzero byte
	for _, b := range value {
		nonzero |= b
	}
	return nonzero != 0
}

func constantTimeCanonicalStringEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func trimOneFinalNewline(input []byte) []byte {
	if bytes.HasSuffix(input, []byte{'\n'}) {
		return input[:len(input)-1]
	}
	return input
}
