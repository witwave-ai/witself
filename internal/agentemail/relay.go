// Package agentemail contains provider-neutral primitives for the receive-only
// agent-email surface. It deliberately does not parse or interpret message
// content; callers provide the raw RFC 5322 bytes and signed SMTP-envelope
// metadata.
package agentemail

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// RelaySignatureVersion is the first canonical signed-envelope format.
	RelaySignatureVersion = "witself-email-relay-pilot-v1"

	// RelaySignatureVersionV2 extends the signed envelope with the edge's
	// attested SPF/DKIM/DMARC verdicts as three appended plain-token lines.
	// Cells dual-accept both versions; the edge switches only after every
	// receiving cell accepts v2 (cell-side inert first).
	RelaySignatureVersionV2 = "witself-email-relay-v2"

	// RelayMaximumRawBytes is the transport-level technical ceiling shared by
	// the production edge relay and the cell signature envelope.
	RelayMaximumRawBytes = 25 * 1024 * 1024

	maxEnvelopeAddressBytes = 320
	maxAudienceBytes        = 128
)

var (
	// ErrRelayMetadataInvalid reports malformed or non-canonical signed fields.
	ErrRelayMetadataInvalid = errors.New("invalid agent-email relay metadata")
	// ErrRelayBodyMismatch reports a signed size or digest that does not match
	// the request body.
	ErrRelayBodyMismatch = errors.New("agent-email relay body does not match signed metadata")
	// ErrRelaySignatureInvalid reports an invalid detached Ed25519 signature.
	ErrRelaySignatureInvalid = errors.New("invalid agent-email relay signature")
	// ErrRelayTimestampInvalid reports a signed timestamp outside the accepted
	// replay window.
	ErrRelayTimestampInvalid = errors.New("agent-email relay timestamp is outside the replay window")
)

// RelayMetadata is the complete signed edge-to-cell relay envelope.
// Provider message IDs and spam verdicts are intentionally absent because
// Cloudflare's EmailMessage event does not expose authoritative values for
// them. A v2 envelope carries the edge's attested SPF/DKIM/DMARC verdicts:
// domain-granularity advisory transport of the trusted attester's own
// Authentication-Results header, signed for transport integrity — never
// authentication of a sender principal.
type RelayMetadata struct {
	// Version selects the canonical signed-envelope format. The zero value
	// means RelaySignatureVersion so existing callers keep their exact
	// prior byte contract.
	Version           string
	Timestamp         int64
	KeyID             string
	Audience          string
	EnvelopeSender    string
	EnvelopeRecipient string
	RawSize           int64
	RawSHA256         string
	// SPFResult, DKIMResult, and DMARCResult are the edge-attested verdicts
	// on a v2 envelope, bounded to the schema-0059 column vocabularies.
	// "unknown" means not evaluated by a trusted attester. They must be
	// empty on a v1 envelope.
	SPFResult   string
	DKIMResult  string
	DMARCResult string
}

// Signed verdict vocabularies, exactly the schema-0059 column CHECK lists.
var (
	relaySPFVocabulary = map[string]bool{
		"unknown": true, "none": true, "neutral": true, "pass": true,
		"fail": true, "softfail": true, "temperror": true, "permerror": true,
	}
	relayDKIMVocabulary = map[string]bool{
		"unknown": true, "none": true, "neutral": true, "pass": true,
		"fail": true, "policy": true, "temperror": true, "permerror": true,
	}
	relayDMARCVocabulary = map[string]bool{
		"unknown": true, "none": true, "pass": true, "fail": true,
		"temperror": true, "permerror": true,
	}
)

// Normalize validates and canonicalizes the signed envelope. A sender may be
// empty for the SMTP null reverse-path. Lowercasing the sender is acceptable
// for this provider profile because it is used only as unverified display
// metadata and a non-destructive suspected-duplicate grouping component.
func (m RelayMetadata) Normalize() (RelayMetadata, error) {
	m.Version = strings.TrimSpace(m.Version)
	if m.Version == "" {
		m.Version = RelaySignatureVersion
	}
	m.SPFResult = strings.ToLower(strings.TrimSpace(m.SPFResult))
	m.DKIMResult = strings.ToLower(strings.TrimSpace(m.DKIMResult))
	m.DMARCResult = strings.ToLower(strings.TrimSpace(m.DMARCResult))
	switch m.Version {
	case RelaySignatureVersion:
		if m.SPFResult != "" || m.DKIMResult != "" || m.DMARCResult != "" {
			return RelayMetadata{}, fmt.Errorf("%w: a v1 envelope cannot carry authentication verdicts", ErrRelayMetadataInvalid)
		}
	case RelaySignatureVersionV2:
		if !relaySPFVocabulary[m.SPFResult] || !relayDKIMVocabulary[m.DKIMResult] || !relayDMARCVocabulary[m.DMARCResult] {
			return RelayMetadata{}, fmt.Errorf("%w: authentication verdicts are outside the signed vocabulary", ErrRelayMetadataInvalid)
		}
	default:
		return RelayMetadata{}, fmt.Errorf("%w: unsupported envelope version", ErrRelayMetadataInvalid)
	}
	m.Audience = strings.ToLower(strings.TrimSpace(m.Audience))
	m.KeyID = strings.ToLower(strings.TrimSpace(m.KeyID))
	m.EnvelopeSender = strings.ToLower(strings.TrimSpace(m.EnvelopeSender))
	m.EnvelopeRecipient = strings.ToLower(strings.TrimSpace(m.EnvelopeRecipient))
	m.RawSHA256 = strings.ToLower(strings.TrimSpace(m.RawSHA256))

	if strings.HasPrefix(m.EnvelopeSender, "<") && strings.HasSuffix(m.EnvelopeSender, ">") {
		m.EnvelopeSender = strings.TrimSpace(m.EnvelopeSender[1 : len(m.EnvelopeSender)-1])
	}
	if m.EnvelopeSender == "<>" {
		m.EnvelopeSender = ""
	}
	if strings.HasPrefix(m.EnvelopeRecipient, "<") && strings.HasSuffix(m.EnvelopeRecipient, ">") {
		m.EnvelopeRecipient = strings.TrimSpace(m.EnvelopeRecipient[1 : len(m.EnvelopeRecipient)-1])
	}

	if m.Timestamp <= 0 {
		return RelayMetadata{}, fmt.Errorf("%w: timestamp must be positive", ErrRelayMetadataInvalid)
	}
	if !validKeyID(m.KeyID) {
		return RelayMetadata{}, fmt.Errorf("%w: key id is invalid", ErrRelayMetadataInvalid)
	}
	if !validAudience(m.Audience) {
		return RelayMetadata{}, fmt.Errorf("%w: audience is invalid", ErrRelayMetadataInvalid)
	}
	if !validEnvelopeAddress(m.EnvelopeSender, true) {
		return RelayMetadata{}, fmt.Errorf("%w: envelope sender is invalid", ErrRelayMetadataInvalid)
	}
	if !validEnvelopeAddress(m.EnvelopeRecipient, false) {
		return RelayMetadata{}, fmt.Errorf("%w: envelope recipient is invalid", ErrRelayMetadataInvalid)
	}
	if m.RawSize < 1 || m.RawSize > RelayMaximumRawBytes {
		return RelayMetadata{}, fmt.Errorf("%w: raw size must be 1-%d bytes", ErrRelayMetadataInvalid, RelayMaximumRawBytes)
	}
	if !isLowerSHA256(m.RawSHA256) {
		return RelayMetadata{}, fmt.Errorf("%w: raw digest must be lowercase SHA-256", ErrRelayMetadataInvalid)
	}
	return m, nil
}

// CanonicalSignatureInput returns the byte-exact detached-signature input.
// Variable-width fields are base64url encoded, making the newline-delimited
// format unambiguous and straightforward to reproduce with a Worker
// Uint8Array. The trailing newline is part of the signature contract.
func CanonicalSignatureInput(metadata RelayMetadata) ([]byte, error) {
	m, err := metadata.Normalize()
	if err != nil {
		return nil, err
	}
	fields := []string{
		m.Version,
		strconv.FormatInt(m.Timestamp, 10),
		m.KeyID,
		base64.RawURLEncoding.EncodeToString([]byte(m.EnvelopeSender)),
		base64.RawURLEncoding.EncodeToString([]byte(m.EnvelopeRecipient)),
		base64.RawURLEncoding.EncodeToString([]byte(m.Audience)),
		strconv.FormatInt(m.RawSize, 10),
		"sha256:" + m.RawSHA256,
	}
	if m.Version == RelaySignatureVersionV2 {
		// Plain lowercase tokens by construction (Normalize bounds them to
		// the signed vocabulary), so no encoding is required.
		fields = append(fields, m.SPFResult, m.DKIMResult, m.DMARCResult)
	}
	return []byte(strings.Join(fields, "\n") + "\n"), nil
}

// VerifyRelayEnvelope authenticates the signed metadata, including the claimed
// raw-body size and SHA-256 digest, before a caller allocates or reads that
// body. The caller selects the public key by an independently validated key id.
// A zero replayWindow is invalid rather than silently unbounded.
func VerifyRelayEnvelope(
	now time.Time,
	replayWindow time.Duration,
	publicKey ed25519.PublicKey,
	metadata RelayMetadata,
	signature []byte,
) (RelayMetadata, error) {
	m, err := metadata.Normalize()
	if err != nil {
		return RelayMetadata{}, err
	}
	if replayWindow <= 0 {
		return RelayMetadata{}, fmt.Errorf("%w: replay window must be positive", ErrRelayMetadataInvalid)
	}
	if len(publicKey) != ed25519.PublicKeySize || len(signature) != ed25519.SignatureSize {
		return RelayMetadata{}, ErrRelaySignatureInvalid
	}
	signedAt := time.Unix(m.Timestamp, 0)
	delta := now.Sub(signedAt)
	if delta < -replayWindow || delta > replayWindow {
		return RelayMetadata{}, ErrRelayTimestampInvalid
	}
	input, err := CanonicalSignatureInput(m)
	if err != nil {
		return RelayMetadata{}, err
	}
	if !ed25519.Verify(publicKey, input, signature) {
		return RelayMetadata{}, ErrRelaySignatureInvalid
	}
	return m, nil
}

// VerifyRelayBody binds one fully read body to an already authenticated relay
// envelope. Callers should use VerifyRelayEnvelope before reading a large body.
func VerifyRelayBody(metadata RelayMetadata, raw []byte) error {
	m, err := metadata.Normalize()
	if err != nil {
		return err
	}
	if int64(len(raw)) != m.RawSize {
		return ErrRelayBodyMismatch
	}
	digest := sha256.Sum256(raw)
	if hex.EncodeToString(digest[:]) != m.RawSHA256 {
		return ErrRelayBodyMismatch
	}
	return nil
}

// VerifyRelay verifies the timestamp, detached Ed25519 signature, and raw-body
// binding. New network handlers should call the two stages separately so an
// unauthenticated request cannot force a large body allocation.
func VerifyRelay(
	now time.Time,
	replayWindow time.Duration,
	publicKey ed25519.PublicKey,
	metadata RelayMetadata,
	raw []byte,
	signature []byte,
) (RelayMetadata, error) {
	m, err := VerifyRelayEnvelope(
		now,
		replayWindow,
		publicKey,
		metadata,
		signature,
	)
	if err != nil {
		return RelayMetadata{}, err
	}
	if err := VerifyRelayBody(m, raw); err != nil {
		return RelayMetadata{}, err
	}
	return m, nil
}

// ParsePublicKey decodes one base64-encoded raw Ed25519 public key.
func ParsePublicKey(encoded string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("%w: public key must be base64-encoded raw Ed25519", ErrRelayMetadataInvalid)
	}
	return ed25519.PublicKey(raw), nil
}

// ParseSignature decodes one base64-encoded detached Ed25519 signature.
func ParseSignature(encoded string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil || len(raw) != ed25519.SignatureSize {
		return nil, ErrRelaySignatureInvalid
	}
	return raw, nil
}

func validAudience(value string) bool {
	if len(value) < 1 || len(value) > maxAudienceBytes || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for _, c := range []byte(value) {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return value[len(value)-1] != '-'
}

func validKeyID(value string) bool {
	if len(value) < 1 || len(value) > 64 || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for _, c := range []byte(value) {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '_' {
			return false
		}
	}
	return true
}

func validEnvelopeAddress(value string, allowEmpty bool) bool {
	if value == "" {
		return allowEmpty
	}
	if len(value) > maxEnvelopeAddressBytes || !utf8.ValidString(value) || strings.Count(value, "@") != 1 {
		return false
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f || r == '\r' || r == '\n' {
			return false
		}
	}
	local, domain, ok := strings.Cut(value, "@")
	return ok && local != "" && domain != "" && !strings.ContainsAny(domain, " <>\t")
}

func isLowerSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	for _, c := range []byte(value) {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
