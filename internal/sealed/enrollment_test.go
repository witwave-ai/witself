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

func TestAVKEnrollmentRoundTripAndPublicShape(t *testing.T) {
	avk := mustAgentVaultKey(t, 7)
	recipient, pairing, request := testAVKEnrollmentRequest(t)

	if err := VerifyAVKEnrollmentPairingSecret(pairing, request); err != nil {
		t.Fatalf("VerifyAVKEnrollmentPairingSecret: %v", err)
	}
	sas, err := AVKEnrollmentSAS(pairing, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(sas) != AVKEnrollmentSASDigits {
		t.Fatalf("SAS length = %d, want %d", len(sas), AVKEnrollmentSASDigits)
	}
	for _, c := range sas {
		if c < '0' || c > '9' {
			t.Fatalf("SAS is not decimal: %q", sas)
		}
	}

	packageValue, err := SealAVKEnrollmentPackage(avk, pairing, request, testEnrollmentSourceInstallationID)
	if err != nil {
		t.Fatal(err)
	}
	if packageValue.PackageVersion != AVKEnrollmentPackageVersionV1 ||
		packageValue.AADVersion != AVKEnrollmentAADVersionV1 ||
		packageValue.KDFAlgorithm != AVKEnrollmentKDFAlgorithm ||
		packageValue.AEADAlgorithm != AES256GCMAlgorithm ||
		packageValue.ConsumeCommitmentAlgorithm != AVKEnrollmentConsumeCommitmentAlgorithm ||
		packageValue.SourceInstallationID != testEnrollmentSourceInstallationID ||
		packageValue.AVK != avk.Metadata() || len(packageValue.Ciphertext) != AVKEnrollmentCiphertextBytes {
		t.Fatalf("unexpected public package shape: %#v", packageValue)
	}
	if packageValue.SourceEphemeralPublicKey == request.TargetPublicKey ||
		!validCommitment(packageValue.ConsumeCommitment) {
		t.Fatal("ephemeral source key or consume commitment is invalid")
	}

	opened, proof, err := OpenAVKEnrollmentPackage(recipient, pairing, request, packageValue)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Clear()
	defer proof.Clear()
	if !equalAgentVaultKeyMetadata(opened.Metadata(), avk.Metadata()) || !bytes.Equal(opened.material[:], avk.material[:]) {
		t.Fatal("opened AVK differs from source AVK")
	}
	proofBytes, err := proof.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	defer clear(proofBytes)
	if len(proofBytes) != AVKEnrollmentConsumeProofBytes ||
		!VerifyAVKEnrollmentConsumeProof(request.EnrollmentRequestID, packageValue.ConsumeCommitment, proofBytes) {
		t.Fatal("opened consume proof does not satisfy public commitment")
	}
	commitment, err := proof.Commitment(request.EnrollmentRequestID)
	if err != nil || commitment != packageValue.ConsumeCommitment {
		t.Fatalf("proof commitment = %q, %v", commitment, err)
	}

	packageJSON, err := json.Marshal(packageValue)
	if err != nil {
		t.Fatal(err)
	}
	encodedAVK, err := EncodeAgentVaultKey(avk)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(encodedAVK)
	pairingEncoded, err := EncodeAVKEnrollmentPairingSecret(pairing)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(packageJSON, encodedAVK) || bytes.Contains(packageJSON, avk.material[:]) ||
		bytes.Contains(packageJSON, pairing.material[:]) || strings.Contains(string(packageJSON), pairingEncoded) ||
		bytes.Contains(packageJSON, proofBytes) {
		t.Fatal("public package serialization disclosed private enrollment material")
	}
}

func TestAVKEnrollmentSealsAreRandomizedAndIndependentlyConsumable(t *testing.T) {
	avk := mustAgentVaultKey(t, 1)
	recipient, pairing, request := testAVKEnrollmentRequest(t)
	first, err := SealAVKEnrollmentPackage(avk, pairing, request, testEnrollmentSourceInstallationID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := SealAVKEnrollmentPackage(avk, pairing, request, testEnrollmentSourceInstallationID)
	if err != nil {
		t.Fatal(err)
	}
	if first.SourceEphemeralPublicKey == second.SourceEphemeralPublicKey ||
		first.ConsumeCommitment == second.ConsumeCommitment || bytes.Equal(first.Ciphertext, second.Ciphertext) {
		t.Fatal("independent seals reused ephemeral or consume material")
	}
	for i, packageValue := range []AVKEnrollmentPackage{first, second} {
		opened, proof, err := OpenAVKEnrollmentPackage(recipient, pairing, request, packageValue)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		opened.Clear()
		proof.Clear()
	}
}

func TestAVKEnrollmentRejectsTamperWrongRecipientAndReplayScope(t *testing.T) {
	avk := mustAgentVaultKey(t, 3)
	recipient, pairing, request := testAVKEnrollmentRequest(t)
	packageValue, err := SealAVKEnrollmentPackage(avk, pairing, request, testEnrollmentSourceInstallationID)
	if err != nil {
		t.Fatal(err)
	}
	otherRecipient, otherPairing, otherRequest := testAVKEnrollmentRequestWithIDs(t,
		"enr_bbbbbbbbbbbbbbbb", "loc_dddddddddddddddd")

	assertEnrollmentIntegrity := func(t *testing.T, gotKey *AgentVaultKey, gotProof *AVKEnrollmentConsumeProof, err error) {
		t.Helper()
		if gotKey != nil {
			gotKey.Clear()
		}
		if gotProof != nil {
			gotProof.Clear()
		}
		if !errors.Is(err, ErrEnrollmentIntegrity) {
			t.Fatalf("error = %v, want ErrEnrollmentIntegrity", err)
		}
	}

	t.Run("wrong recipient", func(t *testing.T) {
		key, proof, err := OpenAVKEnrollmentPackage(otherRecipient, pairing, request, packageValue)
		assertEnrollmentIntegrity(t, key, proof, err)
	})
	t.Run("wrong pairing secret", func(t *testing.T) {
		key, proof, err := OpenAVKEnrollmentPackage(recipient, otherPairing, request, packageValue)
		assertEnrollmentIntegrity(t, key, proof, err)
	})
	t.Run("replayed under another request", func(t *testing.T) {
		key, proof, err := OpenAVKEnrollmentPackage(recipient, pairing, otherRequest, packageValue)
		assertEnrollmentIntegrity(t, key, proof, err)
	})

	tamperCases := []struct {
		name   string
		mutate func(*AVKEnrollmentPackage)
	}{
		{"package version", func(p *AVKEnrollmentPackage) { p.PackageVersion++ }},
		{"aad version", func(p *AVKEnrollmentPackage) { p.AADVersion++ }},
		{"kdf algorithm", func(p *AVKEnrollmentPackage) { p.KDFAlgorithm = "other" }},
		{"aead algorithm", func(p *AVKEnrollmentPackage) { p.AEADAlgorithm = "other" }},
		{"consume algorithm", func(p *AVKEnrollmentPackage) { p.ConsumeCommitmentAlgorithm = "other" }},
		{"account", func(p *AVKEnrollmentPackage) { p.Request.AccountID = "acc_bbbbbbbbbbbbbbbb" }},
		{"realm", func(p *AVKEnrollmentPackage) { p.Request.RealmID = "realm_bbbbbbbbbbbbbbbb" }},
		{"owner", func(p *AVKEnrollmentPackage) { p.Request.OwnerAgentID = "agent_bbbbbbbbbbbbbbbb" }},
		{"request id", func(p *AVKEnrollmentPackage) { p.Request.EnrollmentRequestID = "enr_bbbbbbbbbbbbbbbb" }},
		{"source installation", func(p *AVKEnrollmentPackage) { p.SourceInstallationID = "loc_bbbbbbbbbbbbbbbb" }},
		{"target installation", func(p *AVKEnrollmentPackage) { p.Request.TargetInstallationID = "loc_bbbbbbbbbbbbbbbb" }},
		{"target public key", func(p *AVKEnrollmentPackage) {
			p.Request.TargetPublicKey, _ = otherRecipient.PublicKey()
		}},
		{"expires at", func(p *AVKEnrollmentPackage) { p.Request.ExpiresAt++ }},
		{"request version", func(p *AVKEnrollmentPackage) { p.Request.RequestVersion++ }},
		{"key agreement", func(p *AVKEnrollmentPackage) { p.Request.KeyAgreementAlgorithm = "other" }},
		{"pairing algorithm", func(p *AVKEnrollmentPackage) { p.Request.PairingCommitmentAlgorithm = "other" }},
		{"sas algorithm", func(p *AVKEnrollmentPackage) { p.Request.SASAlgorithm = "other" }},
		{"pairing commitment", func(p *AVKEnrollmentPackage) { p.Request.PairingCommitment = strings.Repeat("0", 64) }},
		{"source public key", func(p *AVKEnrollmentPackage) {
			p.SourceEphemeralPublicKey, _ = otherRecipient.PublicKey()
		}},
		{"avk id", func(p *AVKEnrollmentPackage) { p.AVK.ID = "avk_bbbbbbbbbbbbbbbb" }},
		{"avk version", func(p *AVKEnrollmentPackage) { p.AVK.Version++ }},
		{"avk algorithm", func(p *AVKEnrollmentPackage) { p.AVK.Algorithm = "other" }},
		{"avk fingerprint", func(p *AVKEnrollmentPackage) { p.AVK.Fingerprint = strings.Repeat("0", 64) }},
		{"consume commitment", func(p *AVKEnrollmentPackage) { p.ConsumeCommitment = strings.Repeat("0", 64) }},
		{"ciphertext", func(p *AVKEnrollmentPackage) { p.Ciphertext[0] ^= 1 }},
		{"short ciphertext", func(p *AVKEnrollmentPackage) { p.Ciphertext = p.Ciphertext[:len(p.Ciphertext)-1] }},
	}
	for _, test := range tamperCases {
		t.Run(test.name, func(t *testing.T) {
			candidate := cloneAVKEnrollmentPackage(packageValue)
			test.mutate(&candidate)
			key, proof, err := OpenAVKEnrollmentPackage(recipient, pairing, request, candidate)
			assertEnrollmentIntegrity(t, key, proof, err)
		})
	}
}

func TestAVKEnrollmentPairingCommitmentAndSASBindRequest(t *testing.T) {
	recipient := &AVKEnrollmentRecipientKey{}
	for i := range recipient.private {
		recipient.private[i] = byte(i + 1)
	}
	pairing := &AVKEnrollmentPairingSecret{}
	for i := range pairing.material {
		pairing.material[i] = byte(0xa0 + i)
	}
	request, err := NewAVKEnrollmentRequest(recipient, pairing, AVKEnrollmentRequestOptions{
		AccountID:            testEnrollmentAccountID,
		RealmID:              testEnrollmentRealmID,
		OwnerAgentID:         testEnrollmentOwnerAgentID,
		EnrollmentRequestID:  testEnrollmentRequestID,
		TargetInstallationID: testEnrollmentTargetInstallationID,
		ExpiresAt:            testEnrollmentExpiresAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	sas, err := AVKEnrollmentSAS(pairing, request)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := AVKEnrollmentSAS(pairing, request); err != nil || again != sas {
		t.Fatalf("SAS is not deterministic: %q, %v", again, err)
	}

	otherRecipient, err := GenerateAVKEnrollmentRecipientKey()
	if err != nil {
		t.Fatal(err)
	}
	otherPublic, err := otherRecipient.PublicKey()
	if err != nil {
		t.Fatal(err)
	}
	mutations := []struct {
		name   string
		mutate func(*AVKEnrollmentRequest)
	}{
		{"account", func(r *AVKEnrollmentRequest) { r.AccountID = "acc_bbbbbbbbbbbbbbbb" }},
		{"realm", func(r *AVKEnrollmentRequest) { r.RealmID = "realm_bbbbbbbbbbbbbbbb" }},
		{"owner", func(r *AVKEnrollmentRequest) { r.OwnerAgentID = "agent_bbbbbbbbbbbbbbbb" }},
		{"request", func(r *AVKEnrollmentRequest) { r.EnrollmentRequestID = "enr_bbbbbbbbbbbbbbbb" }},
		{"target installation", func(r *AVKEnrollmentRequest) { r.TargetInstallationID = "loc_bbbbbbbbbbbbbbbb" }},
		{"target public", func(r *AVKEnrollmentRequest) { r.TargetPublicKey = otherPublic }},
		{"expiry", func(r *AVKEnrollmentRequest) { r.ExpiresAt++ }},
	}
	for _, test := range mutations {
		t.Run(test.name, func(t *testing.T) {
			candidate := request
			test.mutate(&candidate)
			if err := VerifyAVKEnrollmentPairingSecret(pairing, candidate); !errors.Is(err, ErrEnrollmentIntegrity) {
				t.Fatalf("verification error = %v", err)
			}
			candidate.PairingCommitment, err = pairingCommitment(pairing, candidate)
			if err != nil {
				t.Fatal(err)
			}
			changedSAS, err := AVKEnrollmentSAS(pairing, candidate)
			if err != nil {
				t.Fatal(err)
			}
			if changedSAS == sas {
				t.Fatalf("SAS did not bind %s", test.name)
			}
		})
	}
}

func TestAVKEnrollmentRecipientAndPairingSecretStrictEncoding(t *testing.T) {
	recipient, err := GenerateAVKEnrollmentRecipientKey()
	if err != nil {
		t.Fatal(err)
	}
	public, err := recipient.PublicKey()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := EncodeAVKEnrollmentRecipientKey(recipient)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseAVKEnrollmentRecipientKey(append(append([]byte(nil), encoded...), '\n'))
	if err != nil {
		t.Fatal(err)
	}
	parsedPublic, err := parsed.PublicKey()
	if err != nil || parsedPublic != public {
		t.Fatalf("recipient public key changed: %q, %v", parsedPublic, err)
	}
	for name, input := range map[string][]byte{
		"empty":        nil,
		"wrong prefix": append([]byte("wrong:"), encoded[len(avkEnrollmentRecipientRecordPrefix):]...),
		"padding":      append(append([]byte(nil), encoded...), '='),
		"space":        append(append([]byte(nil), encoded...), ' '),
		"two newlines": append(append([]byte(nil), encoded...), '\n', '\n'),
		"truncated":    append([]byte(nil), encoded[:len(encoded)-1]...),
		"mutation":     mutateByte(encoded, len(encoded)-1),
	} {
		t.Run("recipient "+name, func(t *testing.T) {
			if got, err := ParseAVKEnrollmentRecipientKey(input); got != nil || !errors.Is(err, ErrInvalidEnrollment) {
				t.Fatalf("parse = %#v, %v", got, err)
			}
		})
	}
	if raw, err := json.Marshal(recipient); len(raw) != 0 || !errors.Is(err, ErrEnrollmentDisclosure) {
		t.Fatalf("recipient JSON = %q, %v", raw, err)
	}
	privateB64 := base64.RawURLEncoding.EncodeToString(recipient.private[:])
	for _, rendered := range []string{fmt.Sprint(recipient), fmt.Sprintf("%#v", recipient), fmt.Sprint(*recipient)} {
		if strings.Contains(rendered, privateB64) || !strings.Contains(rendered, "redacted") {
			t.Fatalf("recipient formatting disclosed material: %q", rendered)
		}
	}
	recipient.Clear()
	if _, err := recipient.PublicKey(); !errors.Is(err, ErrInvalidEnrollment) {
		t.Fatalf("cleared recipient error = %v", err)
	}

	pairing, err := GenerateAVKEnrollmentPairingSecret()
	if err != nil {
		t.Fatal(err)
	}
	pairingEncoded, err := EncodeAVKEnrollmentPairingSecret(pairing)
	if err != nil {
		t.Fatal(err)
	}
	pairingParsed, err := ParseAVKEnrollmentPairingSecret(pairingEncoded)
	if err != nil || !bytes.Equal(pairingParsed.material[:], pairing.material[:]) {
		t.Fatalf("pairing parse changed material: %v", err)
	}
	for name, input := range map[string]string{
		"empty":        "",
		"wrong prefix": "wrong:" + pairingEncoded[len(avkEnrollmentPairingSecretPrefix):],
		"padding":      pairingEncoded + "=",
		"space":        pairingEncoded + " ",
		"mutation":     pairingEncoded[:len(pairingEncoded)-1] + "!",
	} {
		t.Run("pairing "+name, func(t *testing.T) {
			if got, err := ParseAVKEnrollmentPairingSecret(input); got != nil || !errors.Is(err, ErrInvalidEnrollment) {
				t.Fatalf("parse = %#v, %v", got, err)
			}
		})
	}
	if raw, err := json.Marshal(pairing); len(raw) != 0 || !errors.Is(err, ErrEnrollmentDisclosure) {
		t.Fatalf("pairing JSON = %q, %v", raw, err)
	}
	if rendered := fmt.Sprintf("%#v", pairing); strings.Contains(rendered, pairingEncoded) || !strings.Contains(rendered, "redacted") {
		t.Fatalf("pairing formatting disclosed material: %q", rendered)
	}
	pairing.Clear()
	if _, err := EncodeAVKEnrollmentPairingSecret(pairing); !errors.Is(err, ErrInvalidEnrollment) {
		t.Fatalf("cleared pairing error = %v", err)
	}
}

func TestAVKEnrollmentConsumeProofIsOpaqueAndRequestBound(t *testing.T) {
	avk := mustAgentVaultKey(t, 1)
	recipient, pairing, request := testAVKEnrollmentRequest(t)
	packageValue, err := SealAVKEnrollmentPackage(avk, pairing, request, testEnrollmentSourceInstallationID)
	if err != nil {
		t.Fatal(err)
	}
	opened, proof, err := OpenAVKEnrollmentPackage(recipient, pairing, request, packageValue)
	if err != nil {
		t.Fatal(err)
	}
	opened.Clear()
	proofBytes, err := proof.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	copyBytes, err := proof.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	proofBytes[0] ^= 1
	if bytes.Equal(proofBytes, copyBytes) {
		t.Fatal("Bytes did not return independent copies")
	}
	proofBytes[0] ^= 1
	commitment, err := AVKEnrollmentConsumeCommitment(request.EnrollmentRequestID, proofBytes)
	if err != nil || commitment != packageValue.ConsumeCommitment {
		t.Fatalf("commitment = %q, %v", commitment, err)
	}
	if VerifyAVKEnrollmentConsumeProof("enr_bbbbbbbbbbbbbbbb", commitment, proofBytes) {
		t.Fatal("consume proof was not request-bound")
	}
	if _, err := AVKEnrollmentConsumeCommitment("bad", proofBytes); !errors.Is(err, ErrInvalidEnrollment) {
		t.Fatalf("invalid request commitment error = %v", err)
	}
	if _, err := AVKEnrollmentConsumeCommitment(request.EnrollmentRequestID, proofBytes[:31]); !errors.Is(err, ErrInvalidEnrollment) {
		t.Fatalf("short proof commitment error = %v", err)
	}
	if raw, err := json.Marshal(proof); len(raw) != 0 || !errors.Is(err, ErrEnrollmentDisclosure) {
		t.Fatalf("proof JSON = %q, %v", raw, err)
	}
	proofB64 := base64.RawURLEncoding.EncodeToString(copyBytes)
	if rendered := fmt.Sprintf("%#v", proof); strings.Contains(rendered, proofB64) || !strings.Contains(rendered, "redacted") {
		t.Fatalf("proof formatting disclosed material: %q", rendered)
	}
	clear(proofBytes)
	clear(copyBytes)
	proof.Clear()
	if _, err := proof.Bytes(); !errors.Is(err, ErrInvalidEnrollment) {
		t.Fatalf("cleared proof error = %v", err)
	}
}

func TestAVKEnrollmentConsumeCommitmentV1Vector(t *testing.T) {
	proof := make([]byte, AVKEnrollmentConsumeProofBytes)
	for i := range proof {
		proof[i] = byte(i + 1)
	}
	commitment, err := AVKEnrollmentConsumeCommitment(testEnrollmentRequestID, proof)
	if err != nil {
		t.Fatal(err)
	}
	const want = "3be1868544acdc5b0c23cb2fd484b62e7b9fbafdfc2cedc612cdba3359c56c78"
	if commitment != want {
		t.Fatalf("v1 commitment = %q, want %q", commitment, want)
	}
	if !VerifyAVKEnrollmentConsumeProof(testEnrollmentRequestID, want, proof) {
		t.Fatal("v1 commitment vector did not verify")
	}
}

func TestAVKEnrollmentStrictValidationAndLowOrderTarget(t *testing.T) {
	recipient, pairing, request := testAVKEnrollmentRequest(t)
	avk := mustAgentVaultKey(t, 1)
	_ = recipient

	invalidRequests := []AVKEnrollmentRequestOptions{
		{},
		{AccountID: "bad", RealmID: testEnrollmentRealmID, OwnerAgentID: testEnrollmentOwnerAgentID, EnrollmentRequestID: testEnrollmentRequestID, TargetInstallationID: testEnrollmentTargetInstallationID, ExpiresAt: testEnrollmentExpiresAt},
		{AccountID: testEnrollmentAccountID, RealmID: testEnrollmentRealmID, OwnerAgentID: testEnrollmentOwnerAgentID, EnrollmentRequestID: testEnrollmentRequestID, TargetInstallationID: testEnrollmentTargetInstallationID, ExpiresAt: 0},
		{AccountID: testEnrollmentAccountID, RealmID: testEnrollmentRealmID, OwnerAgentID: testEnrollmentOwnerAgentID, EnrollmentRequestID: testEnrollmentRequestID, TargetInstallationID: testEnrollmentTargetInstallationID, ExpiresAt: MaxAVKEnrollmentExpiresAtUnix + 1},
	}
	for i, options := range invalidRequests {
		if got, err := NewAVKEnrollmentRequest(recipient, pairing, options); got != (AVKEnrollmentRequest{}) || !errors.Is(err, ErrInvalidEnrollment) {
			t.Fatalf("invalid request %d = %#v, %v", i, got, err)
		}
	}
	if _, err := SealAVKEnrollmentPackage(nil, pairing, request, testEnrollmentSourceInstallationID); !errors.Is(err, ErrInvalidEnrollment) {
		t.Fatalf("nil AVK seal error = %v", err)
	}
	if _, err := SealAVKEnrollmentPackage(avk, pairing, request, "bad"); !errors.Is(err, ErrInvalidEnrollment) {
		t.Fatalf("invalid source seal error = %v", err)
	}

	lowOrder := request
	lowOrder.TargetPublicKey = base64.RawURLEncoding.EncodeToString(make([]byte, AVKEnrollmentPublicKeyBytes))
	lowOrder.PairingCommitment, _ = pairingCommitment(pairing, lowOrder)
	if _, err := SealAVKEnrollmentPackage(avk, pairing, lowOrder, testEnrollmentSourceInstallationID); !errors.Is(err, ErrEnrollmentIntegrity) {
		t.Fatalf("low-order target error = %v, want ErrEnrollmentIntegrity", err)
	}
}

func testAVKEnrollmentRequest(t testing.TB) (*AVKEnrollmentRecipientKey, *AVKEnrollmentPairingSecret, AVKEnrollmentRequest) {
	t.Helper()
	return testAVKEnrollmentRequestWithIDs(t, testEnrollmentRequestID, testEnrollmentTargetInstallationID)
}

func testAVKEnrollmentRequestWithIDs(t testing.TB, requestID, targetInstallationID string) (*AVKEnrollmentRecipientKey, *AVKEnrollmentPairingSecret, AVKEnrollmentRequest) {
	t.Helper()
	recipient, err := GenerateAVKEnrollmentRecipientKey()
	if err != nil {
		t.Fatal(err)
	}
	pairing, err := GenerateAVKEnrollmentPairingSecret()
	if err != nil {
		t.Fatal(err)
	}
	request, err := NewAVKEnrollmentRequest(recipient, pairing, AVKEnrollmentRequestOptions{
		AccountID:            testEnrollmentAccountID,
		RealmID:              testEnrollmentRealmID,
		OwnerAgentID:         testEnrollmentOwnerAgentID,
		EnrollmentRequestID:  requestID,
		TargetInstallationID: targetInstallationID,
		ExpiresAt:            testEnrollmentExpiresAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	return recipient, pairing, request
}

func cloneAVKEnrollmentPackage(packageValue AVKEnrollmentPackage) AVKEnrollmentPackage {
	clone := packageValue
	clone.Ciphertext = append([]byte(nil), packageValue.Ciphertext...)
	return clone
}

const (
	testEnrollmentAccountID                  = "acc_aaaaaaaaaaaaaaaa"
	testEnrollmentRealmID                    = "realm_aaaaaaaaaaaaaaaa"
	testEnrollmentOwnerAgentID               = "agent_aaaaaaaaaaaaaaaa"
	testEnrollmentRequestID                  = "enr_aaaaaaaaaaaaaaaa"
	testEnrollmentTargetInstallationID       = "loc_aaaaaaaaaaaaaaaa"
	testEnrollmentSourceInstallationID       = "loc_cccccccccccccccc"
	testEnrollmentExpiresAt            int64 = 1_800_000_000
)
