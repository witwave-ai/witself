package sealed

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

func FuzzParseAgentVaultKey(f *testing.F) {
	key := mustAgentVaultKey(f, 1)
	encoded, err := EncodeAgentVaultKey(key)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(encoded)
	f.Add(append(append([]byte(nil), encoded...), '\n'))
	f.Add([]byte(agentVaultKeyRecordPrefix))
	f.Add([]byte("not-a-key"))

	f.Fuzz(func(t *testing.T, input []byte) {
		if len(input) > 4096 {
			return
		}
		parsed, err := ParseAgentVaultKey(input)
		if err != nil {
			if !errors.Is(err, ErrInvalidKeyEncoding) {
				t.Fatalf("unexpected parse error: %v", err)
			}
			return
		}
		reencoded, err := EncodeAgentVaultKey(parsed)
		if err != nil {
			t.Fatalf("successful parse did not re-encode: %v", err)
		}
		canonicalInput := bytes.TrimSuffix(input, []byte{'\n'})
		if !bytes.Equal(reencoded, canonicalInput) {
			t.Fatal("successful parse was not canonical")
		}
	})
}

func FuzzParseAVKEnrollmentRecipientKey(f *testing.F) {
	key, err := GenerateAVKEnrollmentRecipientKey()
	if err != nil {
		f.Fatal(err)
	}
	encoded, err := EncodeAVKEnrollmentRecipientKey(key)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(encoded)
	f.Add(append(append([]byte(nil), encoded...), '\n'))
	f.Add([]byte(avkEnrollmentRecipientRecordPrefix))
	f.Add([]byte("not-an-enrollment-key"))

	f.Fuzz(func(t *testing.T, input []byte) {
		if len(input) > 4096 {
			return
		}
		parsed, err := ParseAVKEnrollmentRecipientKey(input)
		if err != nil {
			if !errors.Is(err, ErrInvalidEnrollment) {
				t.Fatalf("unexpected recipient parse error: %v", err)
			}
			return
		}
		reencoded, err := EncodeAVKEnrollmentRecipientKey(parsed)
		if err != nil {
			t.Fatalf("successful recipient parse did not re-encode: %v", err)
		}
		canonicalInput := bytes.TrimSuffix(input, []byte{'\n'})
		if !bytes.Equal(reencoded, canonicalInput) {
			t.Fatal("successful recipient parse was not canonical")
		}
	})
}

func FuzzParseAVKEnrollmentPairingSecret(f *testing.F) {
	secret, err := GenerateAVKEnrollmentPairingSecret()
	if err != nil {
		f.Fatal(err)
	}
	encoded, err := EncodeAVKEnrollmentPairingSecret(secret)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(encoded)
	f.Add(avkEnrollmentPairingSecretPrefix)
	f.Add("not-a-pairing-secret")

	f.Fuzz(func(t *testing.T, input string) {
		if len(input) > 4096 {
			return
		}
		parsed, err := ParseAVKEnrollmentPairingSecret(input)
		if err != nil {
			if !errors.Is(err, ErrInvalidEnrollment) {
				t.Fatalf("unexpected pairing parse error: %v", err)
			}
			return
		}
		reencoded, err := EncodeAVKEnrollmentPairingSecret(parsed)
		if err != nil || reencoded != input {
			t.Fatalf("accepted pairing secret was not canonical: %v", err)
		}
	})
}

func FuzzInspectAgentVaultKeyRecoveryNeverPanics(f *testing.F) {
	key := mustAgentVaultKey(f, 1)
	artifact, err := ExportAgentVaultKeyRecovery(key, []byte("fuzz recovery passphrase"), testAVKRecoveryScope())
	if err != nil {
		f.Fatal(err)
	}
	f.Add(artifact)
	f.Add(append(append([]byte(nil), artifact...), '\n'))
	f.Add([]byte(agentVaultKeyRecoveryPrefix))
	f.Add([]byte("not-a-recovery-package"))

	f.Fuzz(func(t *testing.T, input []byte) {
		if len(input) > MaxAVKRecoveryPackageBytes+1024 {
			return
		}
		metadata, err := InspectAgentVaultKeyRecovery(input)
		if err != nil {
			if !errors.Is(err, ErrInvalidRecovery) {
				t.Fatalf("unexpected recovery parse error: %v", err)
			}
			return
		}
		if metadata.FormatVersion != AVKRecoveryFormatVersionV1 || metadata.Scope != testAVKRecoveryScope() && bytes.Equal(input, artifact) {
			t.Fatalf("accepted recovery metadata is inconsistent: %#v", metadata)
		}
	})
}

func FuzzOpenAVKEnrollmentPackageNeverPanics(f *testing.F) {
	avk := mustAgentVaultKey(f, 1)
	recipient, pairing, request := testAVKEnrollmentRequest(f)
	packageValue, err := SealAVKEnrollmentPackage(avk, pairing, request, testEnrollmentSourceInstallationID)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(packageValue.Ciphertext, packageValue.SourceEphemeralPublicKey, packageValue.ConsumeCommitment, request.ExpiresAt, byte(0))
	f.Add([]byte{}, "", "", int64(0), byte(1))

	f.Fuzz(func(t *testing.T, ciphertext []byte, sourcePublic, consumeCommitment string, expiresAt int64, selector byte) {
		if len(ciphertext) > AVKEnrollmentCiphertextBytes+1024 || len(sourcePublic) > 4096 || len(consumeCommitment) > 4096 {
			return
		}
		candidate := cloneAVKEnrollmentPackage(packageValue)
		candidate.Ciphertext = append([]byte(nil), ciphertext...)
		candidate.SourceEphemeralPublicKey = sourcePublic
		candidate.ConsumeCommitment = consumeCommitment
		candidate.Request.ExpiresAt = expiresAt
		expected := request
		if selector&1 != 0 {
			expected.EnrollmentRequestID = "enr_bbbbbbbbbbbbbbbb"
		}
		key, proof, err := OpenAVKEnrollmentPackage(recipient, pairing, expected, candidate)
		if err != nil {
			if !errors.Is(err, ErrEnrollmentIntegrity) {
				t.Fatalf("unexpected enrollment open error: %v", err)
			}
			return
		}
		key.Clear()
		proof.Clear()
	})
}

func FuzzCanonicalAADNeverPanics(f *testing.F) {
	base := testValueBinding()
	f.Add(base.AccountID, base.RealmID, base.OwnerAgentID, base.SecretID, base.FieldID, base.DEKID, base.ValueVersion, base.DEKGeneration, base.ValueEncoding)
	f.Add("", "", "", "", "", "", uint64(0), uint64(0), "")
	f.Fuzz(func(t *testing.T, account, realm, agent, secret, field, dek string, valueVersion, dekGeneration uint64, encoding string) {
		binding := testValueBinding()
		binding.AccountID, binding.RealmID, binding.OwnerAgentID = account, realm, agent
		binding.SecretID, binding.FieldID, binding.DEKID = secret, field, dek
		binding.ValueVersion, binding.DEKGeneration, binding.ValueEncoding = valueVersion, dekGeneration, encoding
		first, err := CanonicalValueAAD(binding)
		if err != nil {
			if !errors.Is(err, ErrInvalidBinding) {
				t.Fatalf("unexpected AAD error: %v", err)
			}
			return
		}
		second, err := CanonicalValueAAD(binding)
		if err != nil || !bytes.Equal(first, second) {
			t.Fatalf("accepted AAD binding is not deterministic: %v", err)
		}
	})
}

func FuzzOpenSensitiveFieldNeverPanics(f *testing.F) {
	key := mustAgentVaultKey(f, 1)
	envelope, err := SealSensitiveField(key, []byte("fuzz seed"), testSealOptions())
	if err != nil {
		f.Fatal(err)
	}
	f.Add(envelope.Ciphertext, envelope.WrappedDEK, envelope.ValueVersion, byte(0))
	f.Add([]byte{}, []byte{}, uint64(0), byte(1))

	f.Fuzz(func(t *testing.T, ciphertext, wrapped []byte, valueVersion uint64, selector byte) {
		if len(ciphertext) > MaxSensitiveCiphertextBytes+1024 || len(wrapped) > 4096 {
			return
		}
		candidate := cloneEnvelope(envelope)
		candidate.Ciphertext = append([]byte(nil), ciphertext...)
		candidate.WrappedDEK = append([]byte(nil), wrapped...)
		candidate.ValueVersion = valueVersion
		scope := testFieldScope()
		if selector&1 != 0 {
			scope.FieldID = "fld_ffffffffffffffff"
		}
		_, err := OpenSensitiveField(key, scope, candidate)
		if err != nil && !errors.Is(err, ErrIntegrity) {
			t.Fatalf("unexpected open error: %v", err)
		}
	})
}

func FuzzGeneratePasswordNeverPanics(f *testing.F) {
	f.Add(32, true, true, true, true, false)
	f.Add(0, false, false, false, false, false)
	f.Fuzz(func(t *testing.T, length int, lower, upper, digits, symbols, exclude bool) {
		password, err := GeneratePassword(PasswordPolicy{
			Length: length, Lowercase: lower, Uppercase: upper,
			Digits: digits, Symbols: symbols, ExcludeAmbiguous: exclude,
		})
		if err != nil {
			if !errors.Is(err, ErrInvalidPasswordPolicy) {
				t.Fatalf("unexpected password error: %v", err)
			}
			return
		}
		if len(password) != length {
			t.Fatalf("password length = %d, want %d", len(password), length)
		}
	})
}

func FuzzParseOTPAuthTOTPNeverPanics(f *testing.F) {
	f.Add("otpauth://totp/Example:alice?secret=" + testTOTPSeed + "&issuer=Example&algorithm=SHA256&digits=8&period=30")
	f.Add("otpauth://totp/alice?secret=" + testTOTPSeed)
	f.Add("not-a-uri")
	f.Fuzz(func(t *testing.T, input string) {
		if len(input) > MaxOTPAuthURIBytes+1024 {
			return
		}
		payload, err := ParseOTPAuthTOTP(input)
		if err != nil {
			if !errors.Is(err, ErrInvalidTOTPPayload) {
				t.Fatalf("unexpected URI error: %v", err)
			}
			return
		}
		encoded, err := EncodeTOTPPayload(payload)
		if err != nil {
			t.Fatalf("accepted URI did not encode: %v", err)
		}
		parsed, err := ParseTOTPPayload(encoded)
		if err != nil {
			t.Fatalf("encoded accepted URI did not parse: %v", err)
		}
		if _, err := GenerateTOTPCode(parsed, time.Unix(1_234_567_890, 0)); err != nil {
			t.Fatalf("accepted URI did not generate: %v", err)
		}
	})
}

func FuzzParseTOTPPayloadNeverPanics(f *testing.F) {
	payload, err := NewTOTPPayload(testTOTPSeed, TOTPOptions{Issuer: "Example", Account: "alice"})
	if err != nil {
		f.Fatal(err)
	}
	encoded, err := EncodeTOTPPayload(payload)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(encoded)
	f.Add([]byte(`{"version":1}`))
	f.Add([]byte(testTOTPSeed))
	f.Fuzz(func(t *testing.T, input []byte) {
		if len(input) > MaxTOTPPayloadBytes+1024 {
			return
		}
		parsed, err := ParseTOTPPayload(input)
		if err != nil {
			if !errors.Is(err, ErrInvalidTOTPPayload) {
				t.Fatalf("unexpected payload error: %v", err)
			}
			return
		}
		reencoded, err := EncodeTOTPPayload(parsed)
		if err != nil || !bytes.Equal(reencoded, input) {
			t.Fatalf("accepted payload was not canonical: %v", err)
		}
	})
}
