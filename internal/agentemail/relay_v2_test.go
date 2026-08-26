package agentemail

import (
	"crypto/ed25519"
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func testV2Metadata() RelayMetadata {
	return RelayMetadata{
		Version:           RelaySignatureVersionV2,
		Timestamp:         1800000000,
		KeyID:             "pilot-2026-07",
		Audience:          "gcp-prod-us-central1-core",
		EnvelopeSender:    "sender@example.com",
		EnvelopeRecipient: "alpha.abcdefghijkl2345@agent-mail.witwave.ai",
		RawSize:           132,
		RawSHA256:         "72b8c6918edc264c0973978f7911e21651cc36eab283421d7f2435eaa7ae90c8",
		SPFResult:         "pass",
		DKIMResult:        "none",
		DMARCResult:       "fail",
	}
}

func TestCanonicalSignatureInputV2AppendsVerdictLines(t *testing.T) {
	m := testV2Metadata()
	input, err := CanonicalSignatureInput(m)
	if err != nil {
		t.Fatal(err)
	}
	b64 := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	want := strings.Join([]string{
		RelaySignatureVersionV2,
		"1800000000",
		"pilot-2026-07",
		b64(m.EnvelopeSender),
		b64(m.EnvelopeRecipient),
		b64(m.Audience),
		"132",
		"sha256:" + m.RawSHA256,
		"pass",
		"none",
		"fail",
	}, "\n") + "\n"
	if string(input) != want {
		t.Fatalf("v2 canonical mismatch:\n got %q\nwant %q", input, want)
	}
}

func TestV2SignatureBindsEveryVerdictField(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	base := testV2Metadata()
	canonical, err := CanonicalSignatureInput(base)
	if err != nil {
		t.Fatal(err)
	}
	signature := ed25519.Sign(privateKey, canonical)
	now := time.Unix(base.Timestamp, 0)
	if _, err := VerifyRelayEnvelope(now, time.Minute, publicKey, base, signature); err != nil {
		t.Fatalf("baseline v2 envelope rejected: %v", err)
	}
	for name, mutate := range map[string]func(*RelayMetadata){
		"spf":   func(m *RelayMetadata) { m.SPFResult = "fail" },
		"dkim":  func(m *RelayMetadata) { m.DKIMResult = "pass" },
		"dmarc": func(m *RelayMetadata) { m.DMARCResult = "none" },
	} {
		mutated := base
		mutate(&mutated)
		if _, err := VerifyRelayEnvelope(now, time.Minute, publicKey, mutated, signature); err == nil {
			t.Fatalf("mutated %s verdict still verified", name)
		}
	}
}

func TestCrossVersionRelabelFailsSignature(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	v2 := testV2Metadata()
	now := time.Unix(v2.Timestamp, 0)

	// v1-signed bytes presented as a v2 envelope must fail.
	v1 := v2
	v1.Version = RelaySignatureVersion
	v1.SPFResult, v1.DKIMResult, v1.DMARCResult = "", "", ""
	v1Canonical, err := CanonicalSignatureInput(v1)
	if err != nil {
		t.Fatal(err)
	}
	v1Signature := ed25519.Sign(privateKey, v1Canonical)
	if _, err := VerifyRelayEnvelope(now, time.Minute, publicKey, v2, v1Signature); err == nil {
		t.Fatal("v1 signature accepted for a v2 envelope")
	}

	// v2-signed bytes presented as a v1 envelope must fail.
	v2Canonical, err := CanonicalSignatureInput(v2)
	if err != nil {
		t.Fatal(err)
	}
	v2Signature := ed25519.Sign(privateKey, v2Canonical)
	if _, err := VerifyRelayEnvelope(now, time.Minute, publicKey, v1, v2Signature); err == nil {
		t.Fatal("v2 signature accepted for a v1 envelope")
	}
}

func TestNormalizeRejectsVersionAndVocabularyViolations(t *testing.T) {
	v1WithVerdicts := testV2Metadata()
	v1WithVerdicts.Version = RelaySignatureVersion
	if _, err := v1WithVerdicts.Normalize(); err == nil {
		t.Fatal("v1 envelope with verdicts normalized")
	}

	unknownVersion := testV2Metadata()
	unknownVersion.Version = "witself-email-relay-v3"
	if _, err := unknownVersion.Normalize(); err == nil {
		t.Fatal("unsupported version normalized")
	}

	badVocab := testV2Metadata()
	badVocab.DMARCResult = "softfail" // valid for SPF, not DMARC
	if _, err := badVocab.Normalize(); err == nil {
		t.Fatal("cross-method verdict vocabulary accepted")
	}

	empty := testV2Metadata()
	empty.SPFResult = ""
	if _, err := empty.Normalize(); err == nil {
		t.Fatal("empty verdict accepted on v2")
	}

	// Zero-value Version stays the exact v1 byte contract.
	legacy := testV2Metadata()
	legacy.Version = ""
	legacy.SPFResult, legacy.DKIMResult, legacy.DMARCResult = "", "", ""
	normalized, err := legacy.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if normalized.Version != RelaySignatureVersion {
		t.Fatalf("zero-value version = %q, want v1", normalized.Version)
	}
}
