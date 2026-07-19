package sealed

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestAgentVaultKeyEncodeParseAndPublicMetadata(t *testing.T) {
	key := mustAgentVaultKey(t, InitialAgentVaultKeyVersion)
	encoded, err := EncodeAgentVaultKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(encoded, []byte(agentVaultKeyRecordPrefix)) || bytes.ContainsAny(encoded, "\r\n\t ") {
		t.Fatalf("encoded key is not the canonical one-line record")
	}

	parsed, err := ParseAgentVaultKey(append(append([]byte(nil), encoded...), '\n'))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.ID() != key.ID() || parsed.Version() != key.Version() ||
		parsed.Algorithm() != AES256GCMAlgorithm || parsed.Fingerprint() != key.Fingerprint() ||
		!bytes.Equal(parsed.material[:], key.material[:]) {
		t.Fatalf("parsed key metadata or material changed")
	}
	if len(parsed.Fingerprint()) != 2*32 {
		t.Fatalf("fingerprint length = %d, want 64 hex characters", len(parsed.Fingerprint()))
	}
	if _, err := hex.DecodeString(parsed.Fingerprint()); err != nil {
		t.Fatalf("fingerprint is not lowercase hex: %v", err)
	}

	metadataJSON, err := json.Marshal(parsed.Metadata())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(metadataJSON, []byte(parsed.ID())) || bytes.Contains(metadataJSON, parsed.material[:]) {
		t.Fatalf("public metadata projection is incomplete or contains key bytes")
	}
}

func TestAgentVaultKeyRejectsGenericSerializationAndFormattingIsRedacted(t *testing.T) {
	key := mustAgentVaultKey(t, 7)
	for _, value := range []any{key, *key} {
		if raw, err := json.Marshal(value); !errors.Is(err, ErrKeyDisclosure) || len(raw) != 0 {
			t.Fatalf("json.Marshal(%T) = %q, %v; want ErrKeyDisclosure", value, raw, err)
		}
	}
	var decoded AgentVaultKey
	if err := json.Unmarshal([]byte(`{"id":"avk_aaaaaaaaaaaaaaaa"}`), &decoded); !errors.Is(err, ErrKeyDisclosure) {
		t.Fatalf("json.Unmarshal error = %v, want ErrKeyDisclosure", err)
	}

	rawHex := hex.EncodeToString(key.material[:])
	for _, rendered := range []string{fmt.Sprint(key), fmt.Sprintf("%v", *key), fmt.Sprintf("%#v", *key)} {
		if strings.Contains(rendered, rawHex) || !strings.Contains(rendered, key.Fingerprint()) {
			t.Fatalf("unsafe or incomplete redacted formatting: %q", rendered)
		}
	}
}

func TestAgentVaultKeyClearIsIdempotentAndKeepsFormattingRedacted(t *testing.T) {
	key := mustAgentVaultKey(t, 1)
	key.Clear()
	key.Clear()
	if _, err := EncodeAgentVaultKey(key); !errors.Is(err, ErrInvalidKeyEncoding) {
		t.Fatalf("EncodeAgentVaultKey(cleared) error = %v, want ErrInvalidKeyEncoding", err)
	}
	for _, formatted := range []string{fmt.Sprint(key), fmt.Sprintf("%#v", key), fmt.Sprint(*key), fmt.Sprintf("%#v", *key)} {
		if !strings.Contains(formatted, "<agent-vault-key") || strings.Contains(formatted, "material") {
			t.Fatalf("cleared key formatting was not redacted: %q", formatted)
		}
	}
}

func TestAgentVaultKeyStrictParseRejectsMalformedRecords(t *testing.T) {
	key := mustAgentVaultKey(t, 1)
	valid, err := EncodeAgentVaultKey(key)
	if err != nil {
		t.Fatal(err)
	}

	tests := map[string][]byte{
		"empty":             nil,
		"wrong prefix":      append([]byte("wrong:"), valid[len(agentVaultKeyRecordPrefix):]...),
		"padding":           append(append([]byte(nil), valid...), '='),
		"space":             append(append([]byte(nil), valid...), ' '),
		"two newlines":      append(append([]byte(nil), valid...), '\n', '\n'),
		"carriage return":   append(append([]byte(nil), valid...), '\r', '\n'),
		"truncated":         append([]byte(nil), valid[:len(valid)-1]...),
		"checksum mutation": mutateByte(valid, len(valid)-1),
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			if parsed, err := ParseAgentVaultKey(input); parsed != nil || !errors.Is(err, ErrInvalidKeyEncoding) {
				t.Fatalf("ParseAgentVaultKey = %#v, %v; want ErrInvalidKeyEncoding", parsed, err)
			}
		})
	}
}

func TestAgentVaultKeyGenerationHasIndependentIdentityAndMaterial(t *testing.T) {
	first := mustAgentVaultKey(t, 1)
	second := mustAgentVaultKey(t, 1)
	if first.ID() == second.ID() || first.Fingerprint() == second.Fingerprint() || bytes.Equal(first.material[:], second.material[:]) {
		t.Fatal("independent AVK generations collided")
	}
	if _, err := GenerateAgentVaultKey(0); !errors.Is(err, ErrInvalidKeyEncoding) {
		t.Fatalf("version-zero generation error = %v", err)
	}
	if _, err := EncodeAgentVaultKey(nil); !errors.Is(err, ErrInvalidKeyEncoding) {
		t.Fatalf("nil key encoding error = %v", err)
	}
}

func mustAgentVaultKey(t testing.TB, version uint64) *AgentVaultKey {
	t.Helper()
	key, err := GenerateAgentVaultKey(version)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func mutateByte(input []byte, offset int) []byte {
	mutated := append([]byte(nil), input...)
	if mutated[offset] == 'A' {
		mutated[offset] = 'B'
	} else {
		mutated[offset] = 'A'
	}
	return mutated
}
