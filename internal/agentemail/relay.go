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

	// PilotMaximumRawBytes is intentionally lower than Cloudflare Email
	// Routing's provider limit. It is part of the authorized pilot boundary.
	PilotMaximumRawBytes = 5 * 1024 * 1024

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

// RelayMetadata is the complete capability-limited pilot signature envelope.
// Provider message IDs and authentication/spam results are intentionally
// absent because Cloudflare's EmailMessage event does not expose authoritative
// values for them.
type RelayMetadata struct {
	Timestamp         int64
	KeyID             string
	Audience          string
	EnvelopeSender    string
	EnvelopeRecipient string
	RawSize           int64
	RawSHA256         string
}

// Normalize validates and canonicalizes the signed envelope. A sender may be
// empty for the SMTP null reverse-path. Lowercasing the sender is acceptable
// for this pilot because it is used only as unverified display metadata and a
// non-destructive suspected-duplicate grouping component.
func (m RelayMetadata) Normalize() (RelayMetadata, error) {
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
	if m.RawSize < 1 || m.RawSize > PilotMaximumRawBytes {
		return RelayMetadata{}, fmt.Errorf("%w: raw size must be 1-%d bytes", ErrRelayMetadataInvalid, PilotMaximumRawBytes)
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
		RelaySignatureVersion,
		strconv.FormatInt(m.Timestamp, 10),
		m.KeyID,
		base64.RawURLEncoding.EncodeToString([]byte(m.EnvelopeSender)),
		base64.RawURLEncoding.EncodeToString([]byte(m.EnvelopeRecipient)),
		base64.RawURLEncoding.EncodeToString([]byte(m.Audience)),
		strconv.FormatInt(m.RawSize, 10),
		"sha256:" + m.RawSHA256,
	}
	return []byte(strings.Join(fields, "\n") + "\n"), nil
}

// VerifyRelay verifies the timestamp, raw-body binding, and detached Ed25519
// signature. The caller selects the public key by an independently validated
// key id. A zero replayWindow is invalid rather than silently unbounded.
func VerifyRelay(
	now time.Time,
	replayWindow time.Duration,
	publicKey ed25519.PublicKey,
	metadata RelayMetadata,
	raw []byte,
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
	if int64(len(raw)) != m.RawSize {
		return RelayMetadata{}, ErrRelayBodyMismatch
	}
	digest := sha256.Sum256(raw)
	if hex.EncodeToString(digest[:]) != m.RawSHA256 {
		return RelayMetadata{}, ErrRelayBodyMismatch
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
