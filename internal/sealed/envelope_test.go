package sealed

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestSensitiveFieldRoundTripAndEnvelopeShape(t *testing.T) {
	key := mustAgentVaultKey(t, 1)
	plaintext := []byte{'p', 'a', 's', 's', 0, 0xff, '\n'}
	envelope, err := SealSensitiveField(key, plaintext, testSealOptions())
	if err != nil {
		t.Fatal(err)
	}
	if envelope.EnvelopeVersion != EnvelopeVersionV1 || envelope.AADVersion != AADVersionV1 ||
		envelope.AEADAlgorithm != AES256GCMAlgorithm || envelope.WrapAlgorithm != AES256GCMAlgorithm ||
		envelope.WrappingKeyID != key.ID() || envelope.WrappingKeyVersion != key.Version() ||
		!validPrefixedID(envelope.DEKID, "dek") {
		t.Fatalf("unexpected envelope metadata: %+v", envelope)
	}
	if len(envelope.Ciphertext) != len(plaintext)+RandomNonceGCMOverhead {
		t.Fatalf("ciphertext length = %d, want %d", len(envelope.Ciphertext), len(plaintext)+RandomNonceGCMOverhead)
	}
	if len(envelope.WrappedDEK) != WrappedDEKBytes {
		t.Fatalf("wrapped DEK length = %d", len(envelope.WrappedDEK))
	}
	if bytes.Contains(envelope.Ciphertext, plaintext) || bytes.Contains(envelope.WrappedDEK, plaintext) {
		t.Fatal("envelope contains plaintext bytes")
	}
	got, err := OpenSensitiveField(key, testFieldScope(), envelope)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("opened plaintext = %x, want %x", got, plaintext)
	}
}

func TestSensitiveFieldRoundTripsEveryValueEncoding(t *testing.T) {
	key := mustAgentVaultKey(t, 1)
	for _, encoding := range []string{ValueEncodingUTF8, ValueEncodingJSON, ValueEncodingBinary} {
		t.Run(encoding, func(t *testing.T) {
			options := testSealOptions()
			options.ValueEncoding = encoding
			plaintext := []byte{0, 1, 2, 0xff}
			envelope, err := SealSensitiveField(key, plaintext, options)
			if err != nil {
				t.Fatal(err)
			}
			if envelope.ValueEncoding != encoding {
				t.Fatalf("value encoding = %q, want %q", envelope.ValueEncoding, encoding)
			}
			got, err := OpenSensitiveField(key, testFieldScope(), envelope)
			if err != nil || !bytes.Equal(got, plaintext) {
				t.Fatalf("round trip = %x, %v", got, err)
			}
		})
	}
}

func TestSensitiveFieldRejectsEmptyAndEnforcesSizeLimit(t *testing.T) {
	key := mustAgentVaultKey(t, 1)
	if _, err := SealSensitiveField(key, nil, testSealOptions()); !errors.Is(err, ErrInvalidBinding) {
		t.Fatalf("empty plaintext error = %v", err)
	}
	maximum := bytes.Repeat([]byte{'x'}, MaxSensitiveValueBytes)
	envelope, err := SealSensitiveField(key, maximum, testSealOptions())
	if err != nil {
		t.Fatalf("maximum value rejected: %v", err)
	}
	if len(envelope.Ciphertext) != MaxSensitiveCiphertextBytes {
		t.Fatalf("maximum ciphertext length = %d", len(envelope.Ciphertext))
	}
	if _, err := SealSensitiveField(key, append(maximum, 'x'), testSealOptions()); !errors.Is(err, ErrInvalidBinding) {
		t.Fatalf("oversized plaintext error = %v", err)
	}
}

func TestSensitiveFieldUsesFreshDEKAndNonces(t *testing.T) {
	key := mustAgentVaultKey(t, 1)
	first, err := SealSensitiveField(key, []byte("same value"), testSealOptions())
	if err != nil {
		t.Fatal(err)
	}
	second, err := SealSensitiveField(key, []byte("same value"), testSealOptions())
	if err != nil {
		t.Fatal(err)
	}
	if first.DEKID == second.DEKID || bytes.Equal(first.Ciphertext, second.Ciphertext) ||
		bytes.Equal(first.WrappedDEK, second.WrappedDEK) {
		t.Fatal("separate field generations reused a DEK id, DEK, or nonce")
	}
}

func TestSensitiveFieldRejectsEveryBindingSwapWithGenericIntegrityError(t *testing.T) {
	key := mustAgentVaultKey(t, 1)
	secret := "do-not-leak-this-secret"
	envelope, err := SealSensitiveField(key, []byte(secret), testSealOptions())
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		scope  FieldScope
		mutate func(*SensitiveFieldEnvelope)
	}{
		{name: "account", scope: mutateScope(func(s *FieldScope) { s.AccountID = "acc_bbbbbbbbbbbbbbbb" })},
		{name: "realm", scope: mutateScope(func(s *FieldScope) { s.RealmID = "realm_cccccccccccccccc" })},
		{name: "owner agent", scope: mutateScope(func(s *FieldScope) { s.OwnerAgentID = "agent_dddddddddddddddd" })},
		{name: "secret", scope: mutateScope(func(s *FieldScope) { s.SecretID = "sec_eeeeeeeeeeeeeeee" })},
		{name: "field", scope: mutateScope(func(s *FieldScope) { s.FieldID = "fld_ffffffffffffffff" })},
		{name: "domain", scope: mutateScope(func(s *FieldScope) { s.Domain = TOTPPayloadDomain })},
		{name: "envelope version", scope: testFieldScope(), mutate: func(e *SensitiveFieldEnvelope) { e.EnvelopeVersion++ }},
		{name: "aad version", scope: testFieldScope(), mutate: func(e *SensitiveFieldEnvelope) { e.AADVersion++ }},
		{name: "value algorithm", scope: testFieldScope(), mutate: func(e *SensitiveFieldEnvelope) { e.AEADAlgorithm = "other" }},
		{name: "value encoding", scope: testFieldScope(), mutate: func(e *SensitiveFieldEnvelope) { e.ValueEncoding = ValueEncodingBinary }},
		{name: "value version", scope: testFieldScope(), mutate: func(e *SensitiveFieldEnvelope) { e.ValueVersion++ }},
		{name: "dek id", scope: testFieldScope(), mutate: func(e *SensitiveFieldEnvelope) { e.DEKID = "dek_gggggggggggggggg" }},
		{name: "dek generation", scope: testFieldScope(), mutate: func(e *SensitiveFieldEnvelope) { e.DEKGeneration++ }},
		{name: "wrap algorithm", scope: testFieldScope(), mutate: func(e *SensitiveFieldEnvelope) { e.WrapAlgorithm = "other" }},
		{name: "wrap revision", scope: testFieldScope(), mutate: func(e *SensitiveFieldEnvelope) { e.WrapRevision++ }},
		{name: "wrapping key id", scope: testFieldScope(), mutate: func(e *SensitiveFieldEnvelope) { e.WrappingKeyID = "avk_hhhhhhhhhhhhhhhh" }},
		{name: "wrapping key version", scope: testFieldScope(), mutate: func(e *SensitiveFieldEnvelope) { e.WrappingKeyVersion++ }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := cloneEnvelope(envelope)
			if test.mutate != nil {
				test.mutate(&candidate)
			}
			assertGenericIntegrityError(t, secret, func() error {
				_, err := OpenSensitiveField(key, test.scope, candidate)
				return err
			})
		})
	}
}

func TestSensitiveFieldRejectsCiphertextTamperAndWrongKey(t *testing.T) {
	key := mustAgentVaultKey(t, 1)
	wrong := mustAgentVaultKey(t, 1)
	secret := "integrity-canary"
	envelope, err := SealSensitiveField(key, []byte(secret), testSealOptions())
	if err != nil {
		t.Fatal(err)
	}

	for _, target := range []string{"value-first", "value-middle", "value-last", "wrap-first", "wrap-middle", "wrap-last"} {
		t.Run(target, func(t *testing.T) {
			candidate := cloneEnvelope(envelope)
			var bytesToMutate []byte
			switch {
			case strings.HasPrefix(target, "value"):
				bytesToMutate = candidate.Ciphertext
			default:
				bytesToMutate = candidate.WrappedDEK
			}
			switch {
			case strings.HasSuffix(target, "first"):
				bytesToMutate[0] ^= 0x80
			case strings.HasSuffix(target, "middle"):
				bytesToMutate[len(bytesToMutate)/2] ^= 0x80
			default:
				bytesToMutate[len(bytesToMutate)-1] ^= 0x80
			}
			assertGenericIntegrityError(t, secret, func() error {
				_, err := OpenSensitiveField(key, testFieldScope(), candidate)
				return err
			})
		})
	}
	assertGenericIntegrityError(t, secret, func() error {
		_, err := OpenSensitiveField(wrong, testFieldScope(), envelope)
		return err
	})
}

func TestSensitiveFieldDEKRewrapLeavesValueCiphertextUntouched(t *testing.T) {
	oldKey := mustAgentVaultKey(t, 1)
	newKey := mustAgentVaultKey(t, 2)
	plaintext := []byte("rotation canary")
	original, err := SealSensitiveField(oldKey, plaintext, testSealOptions())
	if err != nil {
		t.Fatal(err)
	}
	rewrapped, err := RewrapSensitiveFieldDEK(oldKey, newKey, testFieldScope(), original, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rewrapped.Ciphertext, original.Ciphertext) ||
		rewrapped.DEKID != original.DEKID || rewrapped.DEKGeneration != original.DEKGeneration ||
		rewrapped.ValueVersion != original.ValueVersion {
		t.Fatal("DEK rewrap changed the value envelope")
	}
	if bytes.Equal(rewrapped.WrappedDEK, original.WrappedDEK) ||
		rewrapped.WrappingKeyID != newKey.ID() || rewrapped.WrappingKeyVersion != newKey.Version() ||
		rewrapped.WrapRevision != 2 {
		t.Fatal("DEK rewrap did not replace exactly the wrapping envelope")
	}
	got, err := OpenSensitiveField(newKey, testFieldScope(), rewrapped)
	if err != nil || !bytes.Equal(got, plaintext) {
		t.Fatalf("new AVK open = %q, %v", got, err)
	}
	if _, err := OpenSensitiveField(oldKey, testFieldScope(), rewrapped); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("old AVK opened rewrapped envelope: %v", err)
	}
	if _, err := RewrapSensitiveFieldDEK(oldKey, newKey, testFieldScope(), original, original.WrapRevision); !errors.Is(err, ErrInvalidBinding) {
		t.Fatalf("non-increasing wrap revision error = %v", err)
	}
	sameVersionKey := mustAgentVaultKey(t, oldKey.Version())
	if _, err := RewrapSensitiveFieldDEK(oldKey, sameVersionKey, testFieldScope(), original, 2); !errors.Is(err, ErrInvalidBinding) {
		t.Fatalf("non-increasing AVK version error = %v", err)
	}

	rewrapped.Ciphertext[0] ^= 1
	if bytes.Equal(rewrapped.Ciphertext, original.Ciphertext) {
		t.Fatal("rewrapped and original envelopes alias ciphertext storage")
	}
}

func TestTOTPPayloadUsesIndependentDomainAndRoundTrips(t *testing.T) {
	key := mustAgentVaultKey(t, 1)
	options := testSealOptions()
	options.Scope.Domain = TOTPPayloadDomain
	options.ValueEncoding = ValueEncodingJSON
	payload := []byte(`{"seed":"JBSWY3DPEHPK3PXP","digits":6,"period":30}`)
	envelope, err := SealSensitiveField(key, payload, options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenSensitiveField(key, testFieldScope(), envelope); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("field domain opened TOTP envelope: %v", err)
	}
	scope := testFieldScope()
	scope.Domain = TOTPPayloadDomain
	got, err := OpenSensitiveField(key, scope, envelope)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("TOTP payload round trip = %q, %v", got, err)
	}
}

func testSealOptions() SensitiveFieldOptions {
	return SensitiveFieldOptions{
		Scope:         testFieldScope(),
		ValueVersion:  1,
		DEKGeneration: 1,
		ValueEncoding: ValueEncodingUTF8,
		WrapRevision:  1,
	}
}

func mutateScope(mutate func(*FieldScope)) FieldScope {
	scope := testFieldScope()
	mutate(&scope)
	return scope
}

func assertGenericIntegrityError(t testing.TB, secret string, operation func() error) {
	t.Helper()
	err := operation()
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("error = %v, want ErrIntegrity", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("integrity error leaked secret: %v", err)
	}
}
