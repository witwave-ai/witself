package agentemail

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"testing"
	"time"
)

func TestVerifyRelayBindsEveryPilotFieldAndRawBody(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte("From: sender@example.com\r\nTo: agent@example.com\r\n\r\ncode 123456\r\n")
	digest := sha256.Sum256(raw)
	now := time.Unix(1_800_000_000, 0)
	metadata := RelayMetadata{
		Timestamp: now.Unix(), KeyID: "pilot-2026-07", Audience: "gcp-prod-us-central1-core",
		EnvelopeSender: "Sender@Example.COM", EnvelopeRecipient: "Scott.ABCDEFGHIJKL2345@Agent-Mail.Witwave.AI",
		RawSize: int64(len(raw)), RawSHA256: hex.EncodeToString(digest[:]),
	}
	input, err := CanonicalSignatureInput(metadata)
	if err != nil {
		t.Fatal(err)
	}
	signature := ed25519.Sign(privateKey, input)
	verified, err := VerifyRelay(now.Add(time.Minute), 5*time.Minute, publicKey, metadata, raw, signature)
	if err != nil {
		t.Fatalf("VerifyRelay: %v", err)
	}
	if verified.EnvelopeSender != "sender@example.com" ||
		verified.EnvelopeRecipient != "scott.abcdefghijkl2345@agent-mail.witwave.ai" {
		t.Fatalf("verified metadata was not canonicalized: %+v", verified)
	}

	mutations := []struct {
		name string
		edit func(*RelayMetadata)
	}{
		{"timestamp", func(m *RelayMetadata) { m.Timestamp++ }},
		{"key id", func(m *RelayMetadata) { m.KeyID = "pilot-rotated" }},
		{"audience", func(m *RelayMetadata) { m.Audience = "gcp-other-core" }},
		{"sender", func(m *RelayMetadata) { m.EnvelopeSender = "other@example.com" }},
		{"recipient", func(m *RelayMetadata) { m.EnvelopeRecipient = "other.abcdefghijkl2345@agent-mail.witwave.ai" }},
		{"size", func(m *RelayMetadata) { m.RawSize++ }},
		{"digest", func(m *RelayMetadata) {
			m.RawSHA256 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		}},
	}
	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			changed := metadata
			tc.edit(&changed)
			_, err := VerifyRelay(now, 5*time.Minute, publicKey, changed, raw, signature)
			if err == nil {
				t.Fatal("mutated signed field verified")
			}
		})
	}
}

func TestVerifyRelayRejectsReplayBodyAndEncodingFailures(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte("Subject: test\r\n\r\nbody")
	digest := sha256.Sum256(raw)
	now := time.Unix(1_800_000_000, 0)
	metadata := RelayMetadata{
		Timestamp: now.Unix(), KeyID: "pilot-1", Audience: "cell-one", EnvelopeSender: "",
		EnvelopeRecipient: "agent.abcdefghijkl2345@example.com",
		RawSize:           int64(len(raw)), RawSHA256: hex.EncodeToString(digest[:]),
	}
	input, err := CanonicalSignatureInput(metadata)
	if err != nil {
		t.Fatal(err)
	}
	signature := ed25519.Sign(privateKey, input)

	if _, err := VerifyRelay(now.Add(6*time.Minute), 5*time.Minute, publicKey, metadata, raw, signature); !errors.Is(err, ErrRelayTimestampInvalid) {
		t.Fatalf("stale timestamp error = %v", err)
	}
	if _, err := VerifyRelay(now, 5*time.Minute, publicKey, metadata, append(raw, '!'), signature); !errors.Is(err, ErrRelayBodyMismatch) {
		t.Fatalf("body mismatch error = %v", err)
	}
	if _, err := ParsePublicKey("not-base64"); !errors.Is(err, ErrRelayMetadataInvalid) {
		t.Fatalf("public key error = %v", err)
	}
	if _, err := ParseSignature("not-base64"); !errors.Is(err, ErrRelaySignatureInvalid) {
		t.Fatalf("signature error = %v", err)
	}
	encoded := base64.StdEncoding.EncodeToString(publicKey)
	parsed, err := ParsePublicKey(encoded)
	if err != nil || !parsed.Equal(publicKey) {
		t.Fatalf("ParsePublicKey = %x, %v", parsed, err)
	}
}

func TestCanonicalSignatureInputRejectsNonCanonicalMetadata(t *testing.T) {
	cases := []RelayMetadata{
		{},
		{Timestamp: 1, KeyID: "pilot", Audience: "Cell_One", EnvelopeRecipient: "a@b", RawSize: 1, RawSHA256: string(make([]byte, 64))},
		{Timestamp: 1, KeyID: "pilot", Audience: "cell", EnvelopeRecipient: "missing-at", RawSize: 1, RawSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		{Timestamp: 1, KeyID: "pilot", Audience: "cell", EnvelopeRecipient: "a@b", RawSize: PilotMaximumRawBytes + 1, RawSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	}
	for i, metadata := range cases {
		if _, err := CanonicalSignatureInput(metadata); !errors.Is(err, ErrRelayMetadataInvalid) {
			t.Errorf("case %d error = %v", i, err)
		}
	}
}
