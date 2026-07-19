package sealed

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	// TOTPPayloadVersionV1 identifies the canonical encrypted-plaintext payload
	// format produced by EncodeTOTPPayload.
	TOTPPayloadVersionV1 uint32 = 1

	// DefaultTOTPDigits is the code width used when enrollment omits digits.
	DefaultTOTPDigits = 6
	// MinTOTPDigits is the smallest accepted TOTP code width.
	MinTOTPDigits = 6
	// MaxTOTPDigits is the largest accepted TOTP code width.
	MaxTOTPDigits = 8
	// DefaultTOTPPeriodSeconds is the time step used when enrollment omits a
	// period.
	DefaultTOTPPeriodSeconds = 30
	// MinTOTPPeriodSeconds is the smallest accepted TOTP time step.
	MinTOTPPeriodSeconds = 1
	// MaxTOTPPeriodSeconds is the largest accepted TOTP time step.
	MaxTOTPPeriodSeconds = 300

	// MinTOTPSeedBytes is the minimum decoded enrollment seed length.
	MinTOTPSeedBytes = 10
	// MaxTOTPSeedBytes is the maximum decoded enrollment seed length.
	MaxTOTPSeedBytes = 128

	// MaxOTPAuthURIBytes bounds an accepted otpauth enrollment URI.
	MaxOTPAuthURIBytes = 8 << 10
	// MaxTOTPPayloadBytes bounds a canonical cleartext enrollment payload before
	// it is sealed.
	MaxTOTPPayloadBytes = 4 << 10
	// MaxTOTPIssuerBytes bounds issuer metadata encoded in an enrollment.
	MaxTOTPIssuerBytes = 256
	// MaxTOTPAccountBytes bounds account metadata encoded in an enrollment.
	MaxTOTPAccountBytes = 512
)

// TOTPAlgorithm is an allowlisted RFC 6238 HMAC algorithm.
type TOTPAlgorithm string

const (
	// TOTPAlgorithmSHA1 selects HMAC-SHA-1.
	TOTPAlgorithmSHA1 TOTPAlgorithm = "SHA1"
	// TOTPAlgorithmSHA256 selects HMAC-SHA-256.
	TOTPAlgorithmSHA256 TOTPAlgorithm = "SHA256"
	// TOTPAlgorithmSHA512 selects HMAC-SHA-512.
	TOTPAlgorithmSHA512 TOTPAlgorithm = "SHA512"
)

// TOTPOptions supplies public enrollment metadata. Empty algorithm, zero
// digits, and zero period select the RFC-compatible Witself defaults.
type TOTPOptions struct {
	Algorithm     TOTPAlgorithm
	Digits        int
	PeriodSeconds uint64
	Issuer        string
	Account       string
}

// TOTPPayload is an opaque local TOTP enrollment. The decoded seed and all
// fields are deliberately unexported so callers cannot accidentally marshal
// the seed. Use Metadata for the public projection and EncodeTOTPPayload only
// when immediately sealing the returned bytes.
type TOTPPayload struct {
	version       uint32
	seed          []byte
	algorithm     TOTPAlgorithm
	digits        int
	periodSeconds uint64
	issuer        string
	account       string
}

// TOTPPayloadMetadata is safe for ordinary rendering and JSON serialization.
// It confirms that setup material exists but never contains that material.
type TOTPPayloadMetadata struct {
	Version       uint32        `json:"version"`
	Algorithm     TOTPAlgorithm `json:"algorithm"`
	Digits        int           `json:"digits"`
	PeriodSeconds uint64        `json:"period_seconds"`
	Issuer        string        `json:"issuer,omitempty"`
	Account       string        `json:"account,omitempty"`
	SeedPresent   bool          `json:"seed_present"`
}

// TOTPCode is the explicit value-returning result of local code generation.
// It never contains the enrollment seed.
type TOTPCode struct {
	Code             string    `json:"code"`
	Digits           int       `json:"digits"`
	PeriodSeconds    uint64    `json:"period_seconds"`
	RemainingSeconds uint64    `json:"remaining_seconds"`
	ExpiresAt        time.Time `json:"expires_at"`
}

type totpPayloadWireV1 struct {
	Version       uint32        `json:"version"`
	SeedBase32    string        `json:"seed_base32"`
	Algorithm     TOTPAlgorithm `json:"algorithm"`
	Digits        int           `json:"digits"`
	PeriodSeconds uint64        `json:"period_seconds"`
	Issuer        string        `json:"issuer"`
	Account       string        `json:"account"`
}

// NewTOTPPayload validates and normalizes one unpadded Base32 seed and its
// public metadata. Base32 input is ASCII case-insensitive but its decoded
// length and alphabet are strictly bounded; whitespace and padding are not
// accepted.
func NewTOTPPayload(seedBase32 string, options TOTPOptions) (*TOTPPayload, error) {
	seed, err := decodeTOTPSeed(seedBase32)
	if err != nil {
		return nil, ErrInvalidTOTPPayload
	}
	algorithm, digits, period, err := normalizeTOTPOptions(options)
	if err != nil {
		clear(seed)
		return nil, ErrInvalidTOTPPayload
	}
	return &TOTPPayload{
		version:       TOTPPayloadVersionV1,
		seed:          seed,
		algorithm:     algorithm,
		digits:        digits,
		periodSeconds: period,
		issuer:        options.Issuer,
		account:       options.Account,
	}, nil
}

// ParseOTPAuthTOTP parses the Key URI Format's TOTP form. It requires an exact
// otpauth://totp origin, one decoded label, one secret parameter, and only the
// standard issuer, algorithm, digits, and period parameters. Duplicate and
// unknown parameters are rejected. When the label carries an issuer prefix and
// the query carries issuer, they must match byte-for-byte after URL decoding.
func ParseOTPAuthTOTP(raw string) (*TOTPPayload, error) {
	if len(raw) < 1 || len(raw) > MaxOTPAuthURIBytes || !utf8.ValidString(raw) ||
		!strings.HasPrefix(raw, "otpauth://totp/") || strings.Contains(raw, "#") ||
		strings.Contains(raw, "?&") || strings.Contains(raw, "&&") || strings.HasSuffix(raw, "&") {
		return nil, ErrInvalidTOTPPayload
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "otpauth" || parsed.Host != "totp" ||
		parsed.User != nil || parsed.Opaque != "" || parsed.Fragment != "" ||
		parsed.RawFragment != "" || parsed.RawQuery == "" ||
		!strings.HasPrefix(parsed.Path, "/") {
		return nil, ErrInvalidTOTPPayload
	}
	label := strings.TrimPrefix(parsed.Path, "/")
	if label == "" || strings.Contains(label, "/") || !validTOTPText(label, MaxTOTPAccountBytes+MaxTOTPIssuerBytes+1, false) {
		return nil, ErrInvalidTOTPPayload
	}

	query, err := url.ParseQuery(parsed.RawQuery)
	if err != nil {
		return nil, ErrInvalidTOTPPayload
	}
	allowed := map[string]bool{
		"secret": true, "issuer": true, "algorithm": true, "digits": true, "period": true,
	}
	for key, values := range query {
		if !allowed[key] || len(values) != 1 {
			return nil, ErrInvalidTOTPPayload
		}
	}
	seed, ok := singleQueryValue(query, "secret")
	if !ok || seed == "" {
		return nil, ErrInvalidTOTPPayload
	}

	issuer := ""
	account := label
	if separator := strings.IndexByte(label, ':'); separator >= 0 {
		issuer, account = label[:separator], label[separator+1:]
		if issuer == "" || account == "" {
			return nil, ErrInvalidTOTPPayload
		}
	}
	if !validTOTPText(issuer, MaxTOTPIssuerBytes, true) ||
		!validTOTPText(account, MaxTOTPAccountBytes, false) {
		return nil, ErrInvalidTOTPPayload
	}
	if queryIssuer, present := singleQueryValue(query, "issuer"); present {
		if !validTOTPText(queryIssuer, MaxTOTPIssuerBytes, false) ||
			(issuer != "" && queryIssuer != issuer) {
			return nil, ErrInvalidTOTPPayload
		}
		issuer = queryIssuer
	}

	options := TOTPOptions{Issuer: issuer, Account: account}
	if algorithm, present := singleQueryValue(query, "algorithm"); present {
		if algorithm == "" {
			return nil, ErrInvalidTOTPPayload
		}
		options.Algorithm = TOTPAlgorithm(algorithm)
	}
	if digits, present := singleQueryValue(query, "digits"); present {
		value, ok := parseCanonicalUint(digits)
		if !ok || value < MinTOTPDigits || value > MaxTOTPDigits {
			return nil, ErrInvalidTOTPPayload
		}
		options.Digits = int(value)
	}
	if period, present := singleQueryValue(query, "period"); present {
		value, ok := parseCanonicalUint(period)
		if !ok || value < MinTOTPPeriodSeconds || value > MaxTOTPPeriodSeconds {
			return nil, ErrInvalidTOTPPayload
		}
		options.PeriodSeconds = value
	}
	return NewTOTPPayload(seed, options)
}

// EncodeTOTPPayload returns the canonical v1 encrypted-plaintext JSON. This is
// the one deliberate seed-export path in this type: callers must immediately
// seal the returned bytes under TOTPPayloadDomain and clear their copy.
func EncodeTOTPPayload(payload *TOTPPayload) ([]byte, error) {
	if !validTOTPPayload(payload) {
		return nil, ErrInvalidTOTPPayload
	}
	wire := totpPayloadWireV1{
		Version:       payload.version,
		SeedBase32:    encodeTOTPSeed(payload.seed),
		Algorithm:     payload.algorithm,
		Digits:        payload.digits,
		PeriodSeconds: payload.periodSeconds,
		Issuer:        payload.issuer,
		Account:       payload.account,
	}
	encoded, err := json.Marshal(wire)
	if err != nil || len(encoded) > MaxTOTPPayloadBytes {
		clear(encoded)
		return nil, ErrInvalidTOTPPayload
	}
	return encoded, nil
}

// ParseTOTPPayload accepts only the exact canonical v1 representation emitted
// by EncodeTOTPPayload. This rejects unknown/duplicate fields, alternate JSON
// spellings, trailing data, and unsupported versions.
func ParseTOTPPayload(encoded []byte) (*TOTPPayload, error) {
	if len(encoded) < 1 || len(encoded) > MaxTOTPPayloadBytes || !utf8.Valid(encoded) {
		return nil, ErrInvalidTOTPPayload
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var wire totpPayloadWireV1
	if err := decoder.Decode(&wire); err != nil {
		return nil, ErrInvalidTOTPPayload
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, ErrInvalidTOTPPayload
	}
	if wire.Version != TOTPPayloadVersionV1 {
		return nil, ErrInvalidTOTPPayload
	}
	payload, err := NewTOTPPayload(wire.SeedBase32, TOTPOptions{
		Algorithm:     wire.Algorithm,
		Digits:        wire.Digits,
		PeriodSeconds: wire.PeriodSeconds,
		Issuer:        wire.Issuer,
		Account:       wire.Account,
	})
	if err != nil {
		return nil, ErrInvalidTOTPPayload
	}
	canonical, err := EncodeTOTPPayload(payload)
	if err != nil || !bytes.Equal(canonical, encoded) {
		clear(canonical)
		clear(payload.seed)
		return nil, ErrInvalidTOTPPayload
	}
	clear(canonical)
	return payload, nil
}

// Metadata returns the public, seed-free projection of payload.
func (p TOTPPayload) Metadata() TOTPPayloadMetadata {
	return TOTPPayloadMetadata{
		Version:       p.version,
		Algorithm:     p.algorithm,
		Digits:        p.digits,
		PeriodSeconds: p.periodSeconds,
		Issuer:        p.issuer,
		Account:       p.account,
		SeedPresent:   len(p.seed) != 0,
	}
}

// Clear releases the caller-owned decoded seed as soon as local TOTP work is
// complete. It is safe to call more than once. The payload becomes invalid and
// cannot generate another code or be encoded after this call.
func (p *TOTPPayload) Clear() {
	if p == nil {
		return
	}
	clear(p.seed)
	p.seed = nil
}

// String is deliberately redacted so common formatting cannot expose setup
// material. User-provided labels are omitted as well.
func (p TOTPPayload) String() string {
	return fmt.Sprintf("<totp-payload version=%d algorithm=%s digits=%d period=%ds seed=redacted>",
		p.version, p.algorithm, p.digits, p.periodSeconds)
}

// GoString keeps %#v formatting redacted.
func (p TOTPPayload) GoString() string { return p.String() }

// MarshalJSON rejects accidental serialization. Metadata is the safe public
// JSON projection; EncodeTOTPPayload is the explicit private encoding.
func (p TOTPPayload) MarshalJSON() ([]byte, error) { return nil, ErrTOTPDisclosure }

// UnmarshalJSON rejects generic private setup ingestion.
func (p *TOTPPayload) UnmarshalJSON([]byte) error { return ErrTOTPDisclosure }

// GenerateTOTPCode computes an RFC 6238 code at an explicit instant. The
// expiration is the next UTC period boundary and RemainingSeconds is rounded
// up for sub-second instants, so it is in [1, period].
func GenerateTOTPCode(payload *TOTPPayload, at time.Time) (TOTPCode, error) {
	if !validTOTPPayload(payload) {
		return TOTPCode{}, ErrInvalidTOTPPayload
	}
	unix := at.Unix()
	if unix < 0 {
		return TOTPCode{}, ErrInvalidTOTPTime
	}
	period := int64(payload.periodSeconds)
	remainder := unix % period
	untilBoundary := period - remainder
	if unix > math.MaxInt64-untilBoundary {
		return TOTPCode{}, ErrInvalidTOTPTime
	}
	expiresUnix := unix + untilBoundary

	code, err := generateHOTP(payload.seed, uint64(unix)/payload.periodSeconds, payload.algorithm, payload.digits)
	if err != nil {
		return TOTPCode{}, ErrInvalidTOTPPayload
	}
	return TOTPCode{
		Code:             code,
		Digits:           payload.digits,
		PeriodSeconds:    payload.periodSeconds,
		RemainingSeconds: uint64(untilBoundary),
		ExpiresAt:        time.Unix(expiresUnix, 0).UTC(),
	}, nil
}

func generateHOTP(seed []byte, counter uint64, algorithm TOTPAlgorithm, digits int) (string, error) {
	var newHash func() hash.Hash
	switch algorithm {
	case TOTPAlgorithmSHA1:
		newHash = sha1.New
	case TOTPAlgorithmSHA256:
		newHash = sha256.New
	case TOTPAlgorithmSHA512:
		newHash = sha512.New
	default:
		return "", ErrInvalidTOTPPayload
	}
	if digits < MinTOTPDigits || digits > MaxTOTPDigits {
		return "", ErrInvalidTOTPPayload
	}
	var movingFactor [8]byte
	binary.BigEndian.PutUint64(movingFactor[:], counter)
	mac := hmac.New(newHash, seed)
	_, _ = mac.Write(movingFactor[:])
	digest := mac.Sum(nil)
	offset := int(digest[len(digest)-1] & 0x0f)
	if offset+4 > len(digest) {
		clear(digest)
		return "", ErrInvalidTOTPPayload
	}
	value := binary.BigEndian.Uint32(digest[offset:offset+4]) & 0x7fffffff
	clear(digest)
	modulus := uint32(1)
	for range digits {
		modulus *= 10
	}
	code := strconv.FormatUint(uint64(value%modulus), 10)
	return strings.Repeat("0", digits-len(code)) + code, nil
}

func normalizeTOTPOptions(options TOTPOptions) (TOTPAlgorithm, int, uint64, error) {
	algorithm := TOTPAlgorithm(strings.ToUpper(string(options.Algorithm)))
	if algorithm == "" {
		algorithm = TOTPAlgorithmSHA1
	}
	digits := options.Digits
	if digits == 0 {
		digits = DefaultTOTPDigits
	}
	period := options.PeriodSeconds
	if period == 0 {
		period = DefaultTOTPPeriodSeconds
	}
	if (algorithm != TOTPAlgorithmSHA1 && algorithm != TOTPAlgorithmSHA256 && algorithm != TOTPAlgorithmSHA512) ||
		digits < MinTOTPDigits || digits > MaxTOTPDigits ||
		period < MinTOTPPeriodSeconds || period > MaxTOTPPeriodSeconds ||
		!validTOTPText(options.Issuer, MaxTOTPIssuerBytes, true) ||
		!validTOTPText(options.Account, MaxTOTPAccountBytes, true) {
		return "", 0, 0, ErrInvalidTOTPPayload
	}
	return algorithm, digits, period, nil
}

func validTOTPPayload(payload *TOTPPayload) bool {
	if payload == nil || payload.version != TOTPPayloadVersionV1 ||
		len(payload.seed) < MinTOTPSeedBytes || len(payload.seed) > MaxTOTPSeedBytes {
		return false
	}
	algorithm, digits, period, err := normalizeTOTPOptions(TOTPOptions{
		Algorithm:     payload.algorithm,
		Digits:        payload.digits,
		PeriodSeconds: payload.periodSeconds,
		Issuer:        payload.issuer,
		Account:       payload.account,
	})
	return err == nil && algorithm == payload.algorithm && digits == payload.digits && period == payload.periodSeconds
}

func decodeTOTPSeed(value string) ([]byte, error) {
	encoding := base32.StdEncoding.WithPadding(base32.NoPadding)
	if value == "" || len(value) > encoding.EncodedLen(MaxTOTPSeedBytes) {
		return nil, ErrInvalidTOTPPayload
	}
	normalized := make([]byte, len(value))
	for i := range len(value) {
		character := value[i]
		switch {
		case character >= 'A' && character <= 'Z':
			normalized[i] = character
		case character >= 'a' && character <= 'z':
			normalized[i] = character - ('a' - 'A')
		case character >= '2' && character <= '7':
			normalized[i] = character
		default:
			clear(normalized)
			return nil, ErrInvalidTOTPPayload
		}
	}
	seed := make([]byte, encoding.DecodedLen(len(normalized)))
	written, err := encoding.Decode(seed, normalized)
	seed = seed[:written]
	if err != nil || len(seed) < MinTOTPSeedBytes || len(seed) > MaxTOTPSeedBytes {
		clear(normalized)
		clear(seed)
		return nil, ErrInvalidTOTPPayload
	}
	canonical := make([]byte, encoding.EncodedLen(len(seed)))
	encoding.Encode(canonical, seed)
	canonicalInput := bytes.Equal(canonical, normalized)
	clear(canonical)
	clear(normalized)
	if !canonicalInput {
		clear(seed)
		return nil, ErrInvalidTOTPPayload
	}
	return seed, nil
}

func encodeTOTPSeed(seed []byte) string {
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(seed)
}

func validTOTPText(value string, maximumBytes int, emptyAllowed bool) bool {
	if (!emptyAllowed && value == "") || len(value) > maximumBytes || !utf8.ValidString(value) ||
		strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if character == 0 || !unicode.IsPrint(character) {
			return false
		}
	}
	return true
}

func singleQueryValue(query url.Values, key string) (string, bool) {
	values, ok := query[key]
	if !ok || len(values) != 1 {
		return "", false
	}
	return values[0], true
}

func parseCanonicalUint(value string) (uint64, bool) {
	if value == "" || (len(value) > 1 && value[0] == '0') {
		return 0, false
	}
	for i := range len(value) {
		if value[i] < '0' || value[i] > '9' {
			return 0, false
		}
	}
	parsed, err := strconv.ParseUint(value, 10, 64)
	return parsed, err == nil
}
