package sealed

import (
	"bytes"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

const testTOTPSeed = "JBSWY3DPEHPK3PXP"

func TestTOTPPayloadCanonicalRoundTrip(t *testing.T) {
	payload, err := NewTOTPPayload(strings.ToLower(testTOTPSeed), TOTPOptions{
		Issuer:  "Example",
		Account: "alice@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	metadata := payload.Metadata()
	if metadata != (TOTPPayloadMetadata{
		Version:       TOTPPayloadVersionV1,
		Algorithm:     TOTPAlgorithmSHA1,
		Digits:        DefaultTOTPDigits,
		PeriodSeconds: DefaultTOTPPeriodSeconds,
		Issuer:        "Example",
		Account:       "alice@example.com",
		SeedPresent:   true,
	}) {
		t.Fatalf("metadata = %+v", metadata)
	}

	encoded, err := EncodeTOTPPayload(payload)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"version":1,"seed_base32":"JBSWY3DPEHPK3PXP","algorithm":"SHA1","digits":6,"period_seconds":30,"issuer":"Example","account":"alice@example.com"}`
	if string(encoded) != want {
		t.Fatalf("encoded payload = %s, want %s", encoded, want)
	}
	parsed, err := ParseTOTPPayload(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Metadata() != metadata {
		t.Fatalf("round-trip metadata = %+v, want %+v", parsed.Metadata(), metadata)
	}
	first, err := GenerateTOTPCode(payload, time.Unix(1_234_567_890, 0))
	if err != nil {
		t.Fatal(err)
	}
	second, err := GenerateTOTPCode(parsed, time.Unix(1_234_567_890, 0))
	if err != nil || second != first {
		t.Fatalf("round-trip code = %+v, %v, want %+v", second, err, first)
	}
}

func TestParseOTPAuthTOTP(t *testing.T) {
	tests := []struct {
		name string
		uri  string
		want TOTPPayloadMetadata
	}{
		{
			name: "label issuer and defaults",
			uri:  "otpauth://totp/GitHub:scott%40example.com?secret=" + testTOTPSeed + "&issuer=GitHub",
			want: TOTPPayloadMetadata{Version: 1, Algorithm: TOTPAlgorithmSHA1, Digits: 6, PeriodSeconds: 30, Issuer: "GitHub", Account: "scott@example.com", SeedPresent: true},
		},
		{
			name: "escaped issuer separator",
			uri:  "otpauth://totp/ACME%3Aalice?secret=" + strings.ToLower(testTOTPSeed) + "&issuer=ACME",
			want: TOTPPayloadMetadata{Version: 1, Algorithm: TOTPAlgorithmSHA1, Digits: 6, PeriodSeconds: 30, Issuer: "ACME", Account: "alice", SeedPresent: true},
		},
		{
			name: "query issuer",
			uri:  "otpauth://totp/alice?issuer=Example&secret=" + testTOTPSeed,
			want: TOTPPayloadMetadata{Version: 1, Algorithm: TOTPAlgorithmSHA1, Digits: 6, PeriodSeconds: 30, Issuer: "Example", Account: "alice", SeedPresent: true},
		},
		{
			name: "all algorithms and timing options",
			uri:  "otpauth://totp/alice?secret=" + testTOTPSeed + "&algorithm=sha512&digits=7&period=45",
			want: TOTPPayloadMetadata{Version: 1, Algorithm: TOTPAlgorithmSHA512, Digits: 7, PeriodSeconds: 45, Account: "alice", SeedPresent: true},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payload, err := ParseOTPAuthTOTP(test.uri)
			if err != nil {
				t.Fatal(err)
			}
			if got := payload.Metadata(); got != test.want {
				t.Fatalf("metadata = %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestParseOTPAuthTOTPRejectsMalformedOrAmbiguousInput(t *testing.T) {
	validSecret := testTOTPSeed
	tests := map[string]string{
		"empty":                 "",
		"wrong scheme":          "https://totp/alice?secret=" + validSecret,
		"uppercase scheme":      "OTPAUTH://totp/alice?secret=" + validSecret,
		"wrong type":            "otpauth://hotp/alice?secret=" + validSecret,
		"uppercase type":        "otpauth://TOTP/alice?secret=" + validSecret,
		"userinfo":              "otpauth://user@totp/alice?secret=" + validSecret,
		"port":                  "otpauth://totp:443/alice?secret=" + validSecret,
		"empty label":           "otpauth://totp/?secret=" + validSecret,
		"nested label":          "otpauth://totp/team/alice?secret=" + validSecret,
		"empty issuer prefix":   "otpauth://totp/:alice?secret=" + validSecret,
		"empty account":         "otpauth://totp/Example:?secret=" + validSecret,
		"leading label space":   "otpauth://totp/%20alice?secret=" + validSecret,
		"label control":         "otpauth://totp/alice%0Aadmin?secret=" + validSecret,
		"missing query":         "otpauth://totp/alice",
		"missing secret":        "otpauth://totp/alice?issuer=Example",
		"empty secret":          "otpauth://totp/alice?secret=",
		"duplicate secret":      "otpauth://totp/alice?secret=" + validSecret + "&secret=" + validSecret,
		"empty first parameter": "otpauth://totp/alice?&secret=" + validSecret,
		"empty mid parameter":   "otpauth://totp/alice?secret=" + validSecret + "&&issuer=Example",
		"empty last parameter":  "otpauth://totp/alice?secret=" + validSecret + "&",
		"unknown parameter":     "otpauth://totp/alice?secret=" + validSecret + "&image=x",
		"semicolon query":       "otpauth://totp/alice?secret=" + validSecret + ";issuer=x",
		"fragment":              "otpauth://totp/alice?secret=" + validSecret + "#private",
		"empty fragment":        "otpauth://totp/alice?secret=" + validSecret + "#",
		"issuer mismatch":       "otpauth://totp/Example:alice?secret=" + validSecret + "&issuer=Other",
		"empty query issuer":    "otpauth://totp/alice?secret=" + validSecret + "&issuer=",
		"padded seed":           "otpauth://totp/alice?secret=" + validSecret + "=",
		"seed whitespace":       "otpauth://totp/alice?secret=" + validSecret + "%20",
		"seed outside alphabet": "otpauth://totp/alice?secret=" + validSecret + "1",
		"seed too short":        "otpauth://totp/alice?secret=MY",
		"unknown algorithm":     "otpauth://totp/alice?secret=" + validSecret + "&algorithm=MD5",
		"digits too small":      "otpauth://totp/alice?secret=" + validSecret + "&digits=5",
		"digits too large":      "otpauth://totp/alice?secret=" + validSecret + "&digits=9",
		"digits signed":         "otpauth://totp/alice?secret=" + validSecret + "&digits=%2B6",
		"digits leading zero":   "otpauth://totp/alice?secret=" + validSecret + "&digits=06",
		"zero period":           "otpauth://totp/alice?secret=" + validSecret + "&period=0",
		"period too large":      "otpauth://totp/alice?secret=" + validSecret + "&period=301",
		"period leading zero":   "otpauth://totp/alice?secret=" + validSecret + "&period=030",
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := ParseOTPAuthTOTP(input)
			if !errors.Is(err, ErrInvalidTOTPPayload) {
				t.Fatalf("error = %v, want ErrInvalidTOTPPayload", err)
			}
			if err != nil && strings.Contains(err.Error(), validSecret) {
				t.Fatalf("error leaked seed: %v", err)
			}
		})
	}
	oversized := "otpauth://totp/" + strings.Repeat("a", MaxOTPAuthURIBytes) + "?secret=" + validSecret
	if _, err := ParseOTPAuthTOTP(oversized); !errors.Is(err, ErrInvalidTOTPPayload) {
		t.Fatalf("oversized URI error = %v", err)
	}
}

func TestNewTOTPPayloadValidationAndDefaults(t *testing.T) {
	valid, err := NewTOTPPayload(testTOTPSeed, TOTPOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := valid.Metadata(); got.Algorithm != TOTPAlgorithmSHA1 || got.Digits != 6 || got.PeriodSeconds != 30 {
		t.Fatalf("defaults = %+v", got)
	}

	tests := []struct {
		name    string
		seed    string
		options TOTPOptions
	}{
		{name: "empty seed"},
		{name: "padded seed", seed: testTOTPSeed + "="},
		{name: "short seed", seed: "MY"},
		{name: "algorithm", seed: testTOTPSeed, options: TOTPOptions{Algorithm: "MD5"}},
		{name: "digits low", seed: testTOTPSeed, options: TOTPOptions{Digits: 5}},
		{name: "digits high", seed: testTOTPSeed, options: TOTPOptions{Digits: 9}},
		{name: "period high", seed: testTOTPSeed, options: TOTPOptions{PeriodSeconds: MaxTOTPPeriodSeconds + 1}},
		{name: "issuer whitespace", seed: testTOTPSeed, options: TOTPOptions{Issuer: " Example"}},
		{name: "issuer control", seed: testTOTPSeed, options: TOTPOptions{Issuer: "Example\nAdmin"}},
		{name: "account too long", seed: testTOTPSeed, options: TOTPOptions{Account: strings.Repeat("a", MaxTOTPAccountBytes+1)}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewTOTPPayload(test.seed, test.options)
			if !errors.Is(err, ErrInvalidTOTPPayload) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	canonicalOddSeed := base32Seed("12345678901")
	nonCanonicalOddSeed := canonicalOddSeed[:len(canonicalOddSeed)-1] + "R"
	if _, err := NewTOTPPayload(nonCanonicalOddSeed, TOTPOptions{}); !errors.Is(err, ErrInvalidTOTPPayload) {
		t.Fatalf("non-canonical trailing Base32 bits error = %v", err)
	}
}

func TestTOTPPayloadClearInvalidatesDecodedSeed(t *testing.T) {
	payload, err := NewTOTPPayload(testTOTPSeed, TOTPOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !payload.Metadata().SeedPresent {
		t.Fatal("fresh payload does not report enrollment material")
	}
	payload.Clear()
	payload.Clear()
	if payload.Metadata().SeedPresent {
		t.Fatal("cleared payload still reports enrollment material")
	}
	if _, err := GenerateTOTPCode(payload, time.Unix(59, 0)); !errors.Is(err, ErrInvalidTOTPPayload) {
		t.Fatalf("GenerateTOTPCode after Clear error = %v", err)
	}
}

func TestGenerateTOTPCodeRFC6238Vectors(t *testing.T) {
	seeds := map[TOTPAlgorithm]string{
		TOTPAlgorithmSHA1:   base32Seed("12345678901234567890"),
		TOTPAlgorithmSHA256: base32Seed("12345678901234567890123456789012"),
		TOTPAlgorithmSHA512: base32Seed("1234567890123456789012345678901234567890123456789012345678901234"),
	}
	times := []int64{59, 1_111_111_109, 1_111_111_111, 1_234_567_890, 2_000_000_000, 20_000_000_000}
	wants := map[TOTPAlgorithm][]string{
		TOTPAlgorithmSHA1:   {"94287082", "07081804", "14050471", "89005924", "69279037", "65353130"},
		TOTPAlgorithmSHA256: {"46119246", "68084774", "67062674", "91819424", "90698825", "77737706"},
		TOTPAlgorithmSHA512: {"90693936", "25091201", "99943326", "93441116", "38618901", "47863826"},
	}
	for algorithm, seed := range seeds {
		payload, err := NewTOTPPayload(seed, TOTPOptions{Algorithm: algorithm, Digits: 8, PeriodSeconds: 30})
		if err != nil {
			t.Fatal(err)
		}
		for index, unix := range times {
			result, err := GenerateTOTPCode(payload, time.Unix(unix, 0))
			if err != nil {
				t.Fatalf("%s at %d: %v", algorithm, unix, err)
			}
			if result.Code != wants[algorithm][index] {
				t.Fatalf("%s at %d = %s, want %s", algorithm, unix, result.Code, wants[algorithm][index])
			}
		}
	}
}

func TestGenerateTOTPCodeTimingMetadata(t *testing.T) {
	payload, err := NewTOTPPayload(testTOTPSeed, TOTPOptions{Digits: 7, PeriodSeconds: 30})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		at        time.Time
		remaining uint64
		expires   time.Time
	}{
		{at: time.Unix(59, 0), remaining: 1, expires: time.Unix(60, 0).UTC()},
		{at: time.Unix(59, 999_999_999), remaining: 1, expires: time.Unix(60, 0).UTC()},
		{at: time.Unix(60, 0), remaining: 30, expires: time.Unix(90, 0).UTC()},
	}
	for _, test := range tests {
		result, err := GenerateTOTPCode(payload, test.at)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Code) != 7 || result.Digits != 7 || result.PeriodSeconds != 30 ||
			result.RemainingSeconds != test.remaining || !result.ExpiresAt.Equal(test.expires) ||
			result.ExpiresAt.Location() != time.UTC {
			t.Fatalf("result = %+v", result)
		}
	}
	if _, err := GenerateTOTPCode(payload, time.Unix(-1, 0)); !errors.Is(err, ErrInvalidTOTPTime) {
		t.Fatalf("pre-epoch error = %v", err)
	}
	if _, err := GenerateTOTPCode(payload, time.Unix(math.MaxInt64, 0)); !errors.Is(err, ErrInvalidTOTPTime) {
		t.Fatalf("overflow error = %v", err)
	}
	if _, err := GenerateTOTPCode(nil, time.Unix(0, 0)); !errors.Is(err, ErrInvalidTOTPPayload) {
		t.Fatalf("nil payload error = %v", err)
	}
	corrupt := *payload
	corrupt.algorithm = "sha1"
	if _, err := GenerateTOTPCode(&corrupt, time.Unix(0, 0)); !errors.Is(err, ErrInvalidTOTPPayload) {
		t.Fatalf("non-normalized payload error = %v", err)
	}
	corrupt = *payload
	corrupt.periodSeconds = 0
	if _, err := EncodeTOTPPayload(&corrupt); !errors.Is(err, ErrInvalidTOTPPayload) {
		t.Fatalf("zero-period payload error = %v", err)
	}
}

func TestTOTPPayloadRedactsFormattingAndRejectsGenericJSON(t *testing.T) {
	seed := testTOTPSeed
	payload, err := NewTOTPPayload(seed, TOTPOptions{Issuer: "issuer-canary", Account: "account-canary"})
	if err != nil {
		t.Fatal(err)
	}
	for _, rendered := range []string{
		fmt.Sprint(*payload),
		fmt.Sprintf("%v", payload),
		fmt.Sprintf("%+v", payload),
		fmt.Sprintf("%#v", payload),
	} {
		if strings.Contains(rendered, seed) || strings.Contains(rendered, "issuer-canary") || strings.Contains(rendered, "account-canary") ||
			!strings.Contains(rendered, "seed=redacted") {
			t.Fatalf("unsafe rendering: %q", rendered)
		}
	}
	encoded, err := json.Marshal(payload)
	if !errors.Is(err, ErrTOTPDisclosure) || bytes.Contains(encoded, []byte(seed)) {
		t.Fatalf("generic JSON = %q, %v", encoded, err)
	}
	var decoded TOTPPayload
	err = json.Unmarshal([]byte(`{"seed":"`+seed+`"}`), &decoded)
	if !errors.Is(err, ErrTOTPDisclosure) || strings.Contains(err.Error(), seed) {
		t.Fatalf("generic unmarshal error = %v", err)
	}
	metadataJSON, err := json.Marshal(payload.Metadata())
	if err != nil || bytes.Contains(metadataJSON, []byte(seed)) {
		t.Fatalf("metadata JSON = %q, %v", metadataJSON, err)
	}
}

func TestParseTOTPPayloadRejectsNonCanonicalAndMalformedRepresentations(t *testing.T) {
	payload, err := NewTOTPPayload(testTOTPSeed, TOTPOptions{})
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := EncodeTOTPPayload(payload)
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string][]byte{
		"whitespace":       append([]byte(" "), canonical...),
		"trailing":         append(append([]byte(nil), canonical...), []byte("\n{}")...),
		"version":          bytes.Replace(canonical, []byte(`"version":1`), []byte(`"version":2`), 1),
		"unknown field":    bytes.Replace(canonical, []byte(`{"version":1`), []byte(`{"extra":1,"version":1`), 1),
		"duplicate field":  bytes.Replace(canonical, []byte(`{"version":1`), []byte(`{"version":1,"version":1`), 1),
		"lowercase seed":   bytes.Replace(canonical, []byte(testTOTPSeed), []byte(strings.ToLower(testTOTPSeed)), 1),
		"padded seed":      bytes.Replace(canonical, []byte(testTOTPSeed), []byte(testTOTPSeed+"="), 1),
		"unsupported hash": bytes.Replace(canonical, []byte(`"SHA1"`), []byte(`"MD5"`), 1),
		"digits":           bytes.Replace(canonical, []byte(`"digits":6`), []byte(`"digits":9`), 1),
		"period":           bytes.Replace(canonical, []byte(`"period_seconds":30`), []byte(`"period_seconds":301`), 1),
		"reordered":        []byte(`{"seed_base32":"` + testTOTPSeed + `","version":1,"algorithm":"SHA1","digits":6,"period_seconds":30,"issuer":"","account":""}`),
		"not json":         []byte(testTOTPSeed),
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := ParseTOTPPayload(input)
			if !errors.Is(err, ErrInvalidTOTPPayload) {
				t.Fatalf("error = %v", err)
			}
			if err != nil && strings.Contains(err.Error(), testTOTPSeed) {
				t.Fatalf("error leaked seed: %v", err)
			}
		})
	}
}

func TestTOTPPayloadEnvelopeRoundTripAndTamper(t *testing.T) {
	payload, err := ParseOTPAuthTOTP("otpauth://totp/Example:alice?secret=" + testTOTPSeed + "&issuer=Example&algorithm=SHA256&digits=8&period=30")
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := EncodeTOTPPayload(payload)
	if err != nil {
		t.Fatal(err)
	}
	key := mustAgentVaultKey(t, 1)
	options := testSealOptions()
	options.Scope.Domain = TOTPPayloadDomain
	options.ValueEncoding = ValueEncodingJSON
	envelope, err := SealSensitiveField(key, plaintext, options)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(envelope.Ciphertext, plaintext) || bytes.Contains(envelope.Ciphertext, []byte(testTOTPSeed)) {
		t.Fatal("TOTP envelope exposed plaintext")
	}
	scope := testFieldScope()
	scope.Domain = TOTPPayloadDomain
	opened, err := OpenSensitiveField(key, scope, envelope)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseTOTPPayload(opened)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := GenerateTOTPCode(parsed, time.Unix(59, 0)); err != nil {
		t.Fatal(err)
	}

	tampered := cloneEnvelope(envelope)
	tampered.Ciphertext[len(tampered.Ciphertext)/2] ^= 0x80
	if _, err := OpenSensitiveField(key, scope, tampered); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("tampered envelope error = %v", err)
	}
	if _, err := OpenSensitiveField(key, testFieldScope(), envelope); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("wrong-domain envelope error = %v", err)
	}
}

func base32Seed(value string) string {
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte(value))
}
