package sealed

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestAgentVaultKeyRecoveryRoundTripInspectionAndRandomization(t *testing.T) {
	avk := mustAgentVaultKey(t, 4)
	passphrase := []byte("correct horse battery staple")
	passphraseBefore := append([]byte(nil), passphrase...)
	scope := testAVKRecoveryScope()

	first, err := ExportAgentVaultKeyRecovery(avk, passphrase, scope)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(passphrase, passphraseBefore) {
		t.Fatal("ExportAgentVaultKeyRecovery modified caller-owned passphrase")
	}
	if len(first) > MaxAVKRecoveryPackageBytes || !bytes.HasPrefix(first, []byte(agentVaultKeyRecoveryPrefix)) ||
		bytes.ContainsAny(first, "\r\n\t ") {
		t.Fatalf("artifact is not a bounded canonical one-line record")
	}
	metadata, err := InspectAgentVaultKeyRecovery(append(append([]byte(nil), first...), '\n'))
	if err != nil {
		t.Fatal(err)
	}
	if metadata.FormatVersion != AVKRecoveryFormatVersionV1 || metadata.AADVersion != AVKRecoveryAADVersionV1 ||
		metadata.KDFAlgorithm != AVKRecoveryKDFAlgorithm || metadata.Argon2Time != AVKRecoveryArgon2Time ||
		metadata.Argon2MemoryKiB != AVKRecoveryArgon2MemoryKiB ||
		metadata.Argon2Parallelism != AVKRecoveryArgon2Parallelism ||
		metadata.AEADAlgorithm != AVKRecoveryAEADAlgorithm || metadata.Scope != scope ||
		metadata.AVK != avk.Metadata() || metadata.CiphertextBytes != agentVaultKeyRecoveryCiphertextBytes ||
		!validCanonicalRecoveryValue(metadata.Salt, AVKRecoverySaltBytes) {
		t.Fatalf("inspection metadata = %#v", metadata)
	}
	metadataJSON, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	encodedAVK, err := EncodeAgentVaultKey(avk)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(encodedAVK)
	if bytes.Contains(metadataJSON, passphrase) || bytes.Contains(metadataJSON, encodedAVK) ||
		bytes.Contains(metadataJSON, avk.material[:]) || bytes.Contains(first, passphrase) ||
		bytes.Contains(first, encodedAVK) || bytes.Contains(first, avk.material[:]) {
		t.Fatal("artifact or public metadata disclosed AVK/passphrase material")
	}

	opened, err := OpenAgentVaultKeyRecovery(first, passphrase, scope)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Clear()
	if opened.Metadata() != avk.Metadata() || !bytes.Equal(opened.material[:], avk.material[:]) {
		t.Fatal("recovered AVK differs from exported AVK")
	}
	if !bytes.Equal(passphrase, passphraseBefore) {
		t.Fatal("OpenAgentVaultKeyRecovery modified caller-owned passphrase")
	}

	second, err := ExportAgentVaultKeyRecovery(avk, passphrase, scope)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first, second) {
		t.Fatal("independent recovery exports reused salt/nonce")
	}
	firstRecord := mustParseAVKRecoveryRecord(t, first)
	secondRecord := mustParseAVKRecoveryRecord(t, second)
	if firstRecord.Salt == secondRecord.Salt || firstRecord.Ciphertext == secondRecord.Ciphertext {
		t.Fatal("independent recovery exports did not randomize salt and ciphertext")
	}
}

func TestAgentVaultKeyRecoveryRejectsWrongPassphraseAndScope(t *testing.T) {
	avk := mustAgentVaultKey(t, 1)
	passphrase := []byte("a sufficiently long recovery passphrase")
	scope := testAVKRecoveryScope()
	artifact, err := ExportAgentVaultKeyRecovery(avk, passphrase, scope)
	if err != nil {
		t.Fatal(err)
	}
	for name, test := range map[string]struct {
		passphrase []byte
		scope      AVKRecoveryScope
	}{
		"wrong passphrase": {[]byte("a different long recovery passphrase"), scope},
		"wrong account":    {passphrase, AVKRecoveryScope{AccountID: "acc_bbbbbbbbbbbbbbbb", RealmID: scope.RealmID, OwnerAgentID: scope.OwnerAgentID}},
		"wrong realm":      {passphrase, AVKRecoveryScope{AccountID: scope.AccountID, RealmID: "realm_bbbbbbbbbbbbbbbb", OwnerAgentID: scope.OwnerAgentID}},
		"wrong agent":      {passphrase, AVKRecoveryScope{AccountID: scope.AccountID, RealmID: scope.RealmID, OwnerAgentID: "agent_bbbbbbbbbbbbbbbb"}},
	} {
		t.Run(name, func(t *testing.T) {
			opened, err := OpenAgentVaultKeyRecovery(artifact, test.passphrase, test.scope)
			if opened != nil {
				opened.Clear()
			}
			if !errors.Is(err, ErrRecoveryIntegrity) {
				t.Fatalf("error = %v, want ErrRecoveryIntegrity", err)
			}
			if strings.Contains(fmt.Sprint(err), string(test.passphrase)) {
				t.Fatal("error disclosed passphrase")
			}
		})
	}
}

func TestAgentVaultKeyRecoveryAADBindsScopeAndAVKMetadata(t *testing.T) {
	avk := mustAgentVaultKey(t, 2)
	passphrase := []byte("recovery passphrase for aad tests")
	scope := testAVKRecoveryScope()
	artifact, err := ExportAgentVaultKeyRecovery(avk, passphrase, scope)
	if err != nil {
		t.Fatal(err)
	}
	record := mustParseAVKRecoveryRecord(t, artifact)
	baseAAD, err := canonicalAVKRecoveryAAD(record)
	if err != nil {
		t.Fatal(err)
	}

	mutations := []struct {
		name   string
		mutate func(*agentVaultKeyRecoveryRecord)
	}{
		{"account", func(r *agentVaultKeyRecoveryRecord) { r.Scope.AccountID = "acc_bbbbbbbbbbbbbbbb" }},
		{"realm", func(r *agentVaultKeyRecoveryRecord) { r.Scope.RealmID = "realm_bbbbbbbbbbbbbbbb" }},
		{"agent", func(r *agentVaultKeyRecoveryRecord) { r.Scope.OwnerAgentID = "agent_bbbbbbbbbbbbbbbb" }},
		{"avk id", func(r *agentVaultKeyRecoveryRecord) { r.AVK.ID = "avk_bbbbbbbbbbbbbbbb" }},
		{"avk version", func(r *agentVaultKeyRecoveryRecord) { r.AVK.Version++ }},
		{"avk fingerprint", func(r *agentVaultKeyRecoveryRecord) { r.AVK.Fingerprint = strings.Repeat("0", 64) }},
	}
	for _, test := range mutations {
		t.Run(test.name, func(t *testing.T) {
			candidate := record
			test.mutate(&candidate)
			aad, err := canonicalAVKRecoveryAAD(candidate)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Equal(aad, baseAAD) {
				t.Fatalf("AAD did not bind %s", test.name)
			}
		})
	}

	// One validly shaped metadata mutation exercises the actual AEAD boundary,
	// while the deterministic checks above cover every individual coordinate
	// without repeatedly invoking the expensive KDF.
	tampered := record
	tampered.AVK.Fingerprint = strings.Repeat("0", 64)
	tamperedArtifact, err := encodeAgentVaultKeyRecoveryRecord(tampered)
	if err != nil {
		t.Fatal(err)
	}
	if opened, err := OpenAgentVaultKeyRecovery(tamperedArtifact, passphrase, scope); opened != nil || !errors.Is(err, ErrRecoveryIntegrity) {
		if opened != nil {
			opened.Clear()
		}
		t.Fatalf("metadata tamper = %#v, %v", opened, err)
	}
}

func TestAgentVaultKeyRecoveryRejectsCiphertextSaltAndRecoveredMetadataTamper(t *testing.T) {
	avk := mustAgentVaultKey(t, 5)
	passphrase := []byte("recovery passphrase for tamper tests")
	scope := testAVKRecoveryScope()
	artifact, err := ExportAgentVaultKeyRecovery(avk, passphrase, scope)
	if err != nil {
		t.Fatal(err)
	}
	record, salt, ciphertext, err := parseAgentVaultKeyRecoveryRecord(artifact)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("ciphertext", func(t *testing.T) {
		candidate := record
		changed := append([]byte(nil), ciphertext...)
		changed[len(changed)-1] ^= 1
		candidate.Ciphertext = base64.RawURLEncoding.EncodeToString(changed)
		candidateArtifact, err := encodeAgentVaultKeyRecoveryRecord(candidate)
		if err != nil {
			t.Fatal(err)
		}
		if opened, err := OpenAgentVaultKeyRecovery(candidateArtifact, passphrase, scope); opened != nil || !errors.Is(err, ErrRecoveryIntegrity) {
			if opened != nil {
				opened.Clear()
			}
			t.Fatalf("ciphertext tamper = %#v, %v", opened, err)
		}
	})

	t.Run("salt", func(t *testing.T) {
		candidate := record
		changed := append([]byte(nil), salt...)
		changed[0] ^= 1
		candidate.Salt = base64.RawURLEncoding.EncodeToString(changed)
		candidateArtifact, err := encodeAgentVaultKeyRecoveryRecord(candidate)
		if err != nil {
			t.Fatal(err)
		}
		if opened, err := OpenAgentVaultKeyRecovery(candidateArtifact, passphrase, scope); opened != nil || !errors.Is(err, ErrRecoveryIntegrity) {
			if opened != nil {
				opened.Clear()
			}
			t.Fatalf("salt tamper = %#v, %v", opened, err)
		}
	})

	t.Run("decrypted avk metadata", func(t *testing.T) {
		other := mustAgentVaultKey(t, avk.Version())
		otherPlaintext, err := EncodeAgentVaultKey(other)
		if err != nil {
			t.Fatal(err)
		}
		defer clear(otherPlaintext)
		aad, err := canonicalAVKRecoveryAAD(record)
		if err != nil {
			t.Fatal(err)
		}
		defer clear(aad)
		key := deriveAVKRecoveryKey(passphrase, salt)
		defer clear(key)
		otherCiphertext, err := sealAES256GCM(key, otherPlaintext, aad)
		if err != nil {
			t.Fatal(err)
		}
		candidate := record
		candidate.Ciphertext = base64.RawURLEncoding.EncodeToString(otherCiphertext)
		candidateArtifact, err := encodeAgentVaultKeyRecoveryRecord(candidate)
		if err != nil {
			t.Fatal(err)
		}
		if opened, err := OpenAgentVaultKeyRecovery(candidateArtifact, passphrase, scope); opened != nil || !errors.Is(err, ErrRecoveryIntegrity) {
			if opened != nil {
				opened.Clear()
			}
			t.Fatalf("recovered metadata mismatch = %#v, %v", opened, err)
		}
	})
}

func TestAgentVaultKeyRecoveryParserRejectsMalformedBeforeOpen(t *testing.T) {
	avk := mustAgentVaultKey(t, 1)
	artifact, err := ExportAgentVaultKeyRecovery(avk, []byte("parser test recovery passphrase"), testAVKRecoveryScope())
	if err != nil {
		t.Fatal(err)
	}
	record := mustParseAVKRecoveryRecord(t, artifact)
	tweak := func(mutate func(*agentVaultKeyRecoveryRecord)) []byte {
		candidate := record
		mutate(&candidate)
		return encodeUncheckedRecoveryRecord(t, candidate)
	}
	unknownJSON := decodeRecoveryJSONForTest(t, artifact)
	unknownJSON = bytes.TrimSuffix(unknownJSON, []byte{'}'})
	unknownJSON = append(unknownJSON, []byte(`,"unknown":true}`)...)

	tests := map[string][]byte{
		"empty":            nil,
		"prefix only":      []byte(agentVaultKeyRecoveryPrefix),
		"wrong prefix":     append([]byte("wrong:"), artifact[len(agentVaultKeyRecoveryPrefix):]...),
		"padding":          append(append([]byte(nil), artifact...), '='),
		"space":            append(append([]byte(nil), artifact...), ' '),
		"two newlines":     append(append([]byte(nil), artifact...), '\n', '\n'),
		"truncated":        append([]byte(nil), artifact[:len(artifact)-1]...),
		"oversized":        bytes.Repeat([]byte{'A'}, MaxAVKRecoveryPackageBytes+1),
		"unknown json":     encodeRecoveryJSONForTest(unknownJSON),
		"format version":   tweak(func(r *agentVaultKeyRecoveryRecord) { r.FormatVersion++ }),
		"aad version":      tweak(func(r *agentVaultKeyRecoveryRecord) { r.AADVersion++ }),
		"kdf algorithm":    tweak(func(r *agentVaultKeyRecoveryRecord) { r.KDFAlgorithm = "other" }),
		"weak time":        tweak(func(r *agentVaultKeyRecoveryRecord) { r.Argon2Time = 1 }),
		"weak memory":      tweak(func(r *agentVaultKeyRecoveryRecord) { r.Argon2MemoryKiB = 8 }),
		"zero parallelism": tweak(func(r *agentVaultKeyRecoveryRecord) { r.Argon2Parallelism = 0 }),
		"aead algorithm":   tweak(func(r *agentVaultKeyRecoveryRecord) { r.AEADAlgorithm = "other" }),
		"short salt":       tweak(func(r *agentVaultKeyRecoveryRecord) { r.Salt = base64.RawURLEncoding.EncodeToString(make([]byte, 15)) }),
		"short ciphertext": tweak(func(r *agentVaultKeyRecoveryRecord) {
			r.Ciphertext = base64.RawURLEncoding.EncodeToString(make([]byte, agentVaultKeyRecoveryCiphertextBytes-1))
		}),
		"invalid scope": tweak(func(r *agentVaultKeyRecoveryRecord) { r.Scope.AccountID = "bad" }),
		"invalid avk":   tweak(func(r *agentVaultKeyRecoveryRecord) { r.AVK.Fingerprint = strings.Repeat("A", 64) }),
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			if metadata, err := InspectAgentVaultKeyRecovery(input); metadata != (AVKRecoveryMetadata{}) || !errors.Is(err, ErrInvalidRecovery) {
				t.Fatalf("inspect = %#v, %v", metadata, err)
			}
		})
	}
}

func TestAgentVaultKeyRecoveryPassphraseAndScopePolicy(t *testing.T) {
	avk := mustAgentVaultKey(t, 1)
	scope := testAVKRecoveryScope()
	for name, passphrase := range map[string][]byte{
		"empty": nil,
		"short": bytes.Repeat([]byte{'a'}, MinAVKRecoveryPassphraseBytes-1),
		"long":  bytes.Repeat([]byte{'a'}, MaxAVKRecoveryPassphraseBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			if artifact, err := ExportAgentVaultKeyRecovery(avk, passphrase, scope); artifact != nil || !errors.Is(err, ErrInvalidRecoveryPassphrase) {
				t.Fatalf("export = %q, %v", artifact, err)
			}
		})
	}
	validPassphrase := bytes.Repeat([]byte{'a'}, MinAVKRecoveryPassphraseBytes)
	for name, invalidScope := range map[string]AVKRecoveryScope{
		"account": {AccountID: "bad", RealmID: scope.RealmID, OwnerAgentID: scope.OwnerAgentID},
		"realm":   {AccountID: scope.AccountID, RealmID: "bad", OwnerAgentID: scope.OwnerAgentID},
		"agent":   {AccountID: scope.AccountID, RealmID: scope.RealmID, OwnerAgentID: "bad"},
	} {
		t.Run(name, func(t *testing.T) {
			if artifact, err := ExportAgentVaultKeyRecovery(avk, validPassphrase, invalidScope); artifact != nil || !errors.Is(err, ErrInvalidRecovery) {
				t.Fatalf("export = %q, %v", artifact, err)
			}
		})
	}
}

func testAVKRecoveryScope() AVKRecoveryScope {
	return AVKRecoveryScope{
		AccountID: "acc_aaaaaaaaaaaaaaaa", RealmID: "realm_aaaaaaaaaaaaaaaa",
		OwnerAgentID: "agent_aaaaaaaaaaaaaaaa",
	}
}

func mustParseAVKRecoveryRecord(t testing.TB, artifact []byte) agentVaultKeyRecoveryRecord {
	t.Helper()
	record, salt, ciphertext, err := parseAgentVaultKeyRecoveryRecord(artifact)
	clear(salt)
	clear(ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func encodeUncheckedRecoveryRecord(t testing.TB, record agentVaultKeyRecoveryRecord) []byte {
	t.Helper()
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	return encodeRecoveryJSONForTest(raw)
}

func decodeRecoveryJSONForTest(t testing.TB, artifact []byte) []byte {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(string(artifact[len(agentVaultKeyRecoveryPrefix):]))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func encodeRecoveryJSONForTest(raw []byte) []byte {
	out := make([]byte, len(agentVaultKeyRecoveryPrefix)+base64.RawURLEncoding.EncodedLen(len(raw)))
	copy(out, agentVaultKeyRecoveryPrefix)
	base64.RawURLEncoding.Encode(out[len(agentVaultKeyRecoveryPrefix):], raw)
	return out
}
