package store

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestNormalizeVaultKeyRotationInputs(t *testing.T) {
	start := StartVaultKeyRotationInput{
		ID:                          "vkr_aaaaaaaaaaaaaaaa",
		ExpectedSourceKeyID:         "avk_bbbbbbbbbbbbbbbb",
		ExpectedSourceKeyVersion:    1,
		ExpectedSourceKeyRowVersion: 1,
		TargetKeyID:                 "avk_cccccccccccccccc",
		TargetKeyVersion:            2,
		TargetAlgorithm:             SecretAEADAlgorithm,
		TargetFingerprint:           strings.Repeat("A", 64),
		IdempotencyKey:              " start-1 ",
	}
	normalized, err := normalizeStartVaultKeyRotationInput(start)
	if err != nil {
		t.Fatal(err)
	}
	if normalized.TargetFingerprint != strings.Repeat("a", 64) || normalized.IdempotencyKey != "start-1" {
		t.Fatalf("normalized start = %#v", normalized)
	}
	invalid := start
	invalid.TargetKeyVersion = invalid.ExpectedSourceKeyVersion
	if _, err := normalizeStartVaultKeyRotationInput(invalid); !errors.Is(err, ErrSecretInputInvalid) {
		t.Fatalf("non-increasing target version error = %v", err)
	}
	invalid.TargetKeyVersion = invalid.ExpectedSourceKeyVersion + 2
	if _, err := normalizeStartVaultKeyRotationInput(invalid); !errors.Is(err, ErrSecretInputInvalid) {
		t.Fatalf("non-contiguous target version error = %v", err)
	}

	wrapper := bytes.Repeat([]byte{0x5a}, vaultKeyRotationWrappedDEKBytes)
	stage := StageVaultKeyRotationInput{
		ExpectedRotationRowVersion: 1,
		IdempotencyKey:             " stage-1 ",
		Items: []StageVaultKeyRotationItemInput{
			{DEKID: "dek_dddddddddddddddd", ExpectedSourceDEKRowVersion: 2,
				ExpectedSourceWrapRevision: 4, TargetWrappedDEK: wrapper, TargetWrapRevision: 5},
		},
	}
	normalizedStage, err := normalizeStageVaultKeyRotationInput(stage)
	if err != nil {
		t.Fatal(err)
	}
	if normalizedStage.IdempotencyKey != "stage-1" ||
		&normalizedStage.Items[0].TargetWrappedDEK[0] == &wrapper[0] {
		t.Fatalf("normalized stage did not trim and clone: %#v", normalizedStage)
	}
	invalidStage := stage
	invalidStage.Items = append([]StageVaultKeyRotationItemInput(nil), stage.Items...)
	invalidStage.Items[0].TargetWrapRevision = 6
	if _, err := normalizeStageVaultKeyRotationInput(invalidStage); !errors.Is(err, ErrSecretInputInvalid) {
		t.Fatalf("non-contiguous wrap revision error = %v", err)
	}
}

func TestNormalizeCommitVaultKeyRotationRecoveryDisposition(t *testing.T) {
	base := CommitVaultKeyRotationInput{
		ExpectedRotationRowVersion: 2,
		ExpectedItemCount:          1,
		ExpectedPlanHash:           strings.Repeat("a", 64),
		IdempotencyKey:             " commit-1 ",
	}
	tests := []struct {
		name        string
		disposition VaultKeyRotationRecoveryDisposition
		wantMode    string
		wantDigest  string
		wantErr     bool
	}{
		{name: "artifact", disposition: VaultKeyRotationRecoveryDisposition{
			Mode: " recovery_artifact ", ArtifactSHA256: " " + strings.Repeat("b", 64) + " ",
		}, wantMode: VaultKeyRotationRecoveryArtifact, wantDigest: strings.Repeat("b", 64)},
		{name: "risk accepted", disposition: VaultKeyRotationRecoveryDisposition{
			Mode: " risk_accepted ",
		}, wantMode: VaultKeyRotationRiskAccepted},
		{name: "missing", wantErr: true},
		{name: "unknown", disposition: VaultKeyRotationRecoveryDisposition{Mode: "backup_exists"}, wantErr: true},
		{name: "artifact missing digest", disposition: VaultKeyRotationRecoveryDisposition{Mode: VaultKeyRotationRecoveryArtifact}, wantErr: true},
		{name: "artifact malformed digest", disposition: VaultKeyRotationRecoveryDisposition{
			Mode: VaultKeyRotationRecoveryArtifact, ArtifactSHA256: strings.Repeat("g", 64),
		}, wantErr: true},
		{name: "artifact uppercase digest", disposition: VaultKeyRotationRecoveryDisposition{
			Mode: VaultKeyRotationRecoveryArtifact, ArtifactSHA256: strings.Repeat("B", 64),
		}, wantErr: true},
		{name: "risk with digest", disposition: VaultKeyRotationRecoveryDisposition{
			Mode: VaultKeyRotationRiskAccepted, ArtifactSHA256: strings.Repeat("b", 64),
		}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			in := base
			in.RecoveryDisposition = test.disposition
			got, err := normalizeCommitVaultKeyRotationInput(in)
			if (err != nil) != test.wantErr {
				t.Fatalf("normalize error = %v", err)
			}
			if test.wantErr {
				if !errors.Is(err, ErrSecretInputInvalid) {
					t.Fatalf("error = %v, want ErrSecretInputInvalid", err)
				}
				return
			}
			if got.RecoveryDisposition.Mode != test.wantMode ||
				got.RecoveryDisposition.ArtifactSHA256 != test.wantDigest || got.IdempotencyKey != "commit-1" {
				t.Fatalf("normalized commit = %#v", got)
			}
		})
	}
}

func TestCommitVaultKeyRotationRequestHashBindsRecoveryDispositionAndExcludesRetryKey(t *testing.T) {
	base := CommitVaultKeyRotationInput{
		ExpectedRotationRowVersion: 2, ExpectedItemCount: 1,
		ExpectedPlanHash: strings.Repeat("a", 64), IdempotencyKey: "first-key",
		RecoveryDisposition: VaultKeyRotationRecoveryDisposition{
			Mode: VaultKeyRotationRecoveryArtifact, ArtifactSHA256: strings.Repeat("b", 64),
		},
	}
	first, err := vaultKeyRotationCommitRequestHash("vkr_aaaaaaaaaaaaaaaa", base)
	if err != nil {
		t.Fatal(err)
	}
	retry := base
	retry.IdempotencyKey = "another-key"
	retryHash, err := vaultKeyRotationCommitRequestHash("vkr_aaaaaaaaaaaaaaaa", retry)
	if err != nil || retryHash != first {
		t.Fatalf("retry hash = %q / %v, want %q", retryHash, err, first)
	}
	risk := base
	risk.RecoveryDisposition = VaultKeyRotationRecoveryDisposition{Mode: VaultKeyRotationRiskAccepted}
	riskHash, err := vaultKeyRotationCommitRequestHash("vkr_aaaaaaaaaaaaaaaa", risk)
	if err != nil || riskHash == first {
		t.Fatalf("risk hash = %q / %v, artifact hash %q", riskHash, err, first)
	}
	otherArtifact := base
	otherArtifact.RecoveryDisposition.ArtifactSHA256 = strings.Repeat("c", 64)
	otherHash, err := vaultKeyRotationCommitRequestHash("vkr_aaaaaaaaaaaaaaaa", otherArtifact)
	if err != nil || otherHash == first {
		t.Fatalf("other artifact hash = %q / %v, first %q", otherHash, err, first)
	}
}

func TestVaultKeyRotationCommitAuditMetadataIsValueFreeAndDispositionAware(t *testing.T) {
	p := Principal{ID: "agent_aaaaaaaaaaaaaaaa"}
	rotation := VaultKeyRotation{
		ID: "vkr_aaaaaaaaaaaaaaaa", SourceKeyID: "avk_bbbbbbbbbbbbbbbb", SourceKeyVersion: 1,
		TargetKeyID: "avk_cccccccccccccccc", TargetKeyVersion: 2, ItemCount: 3,
	}
	artifact := vaultKeyRotationCommitEventMetadata(p, rotation, VaultKeyRotationRecoveryDisposition{
		Mode: VaultKeyRotationRecoveryArtifact, ArtifactSHA256: strings.Repeat("d", 64),
	})
	if artifact["recovery_disposition_mode"] != VaultKeyRotationRecoveryArtifact ||
		artifact["recovery_artifact_sha256"] != strings.Repeat("d", 64) {
		t.Fatalf("artifact metadata = %#v", artifact)
	}
	if err := checkEventShape(EventInput{
		AccountID: "acc_1", ActorKind: ActorAgent, ActorID: p.ID,
		Verb: VerbVaultKeyRotationCommitted, Metadata: artifact,
	}); err != nil {
		t.Fatalf("artifact audit shape: %v", err)
	}
	risk := vaultKeyRotationCommitEventMetadata(p, rotation,
		VaultKeyRotationRecoveryDisposition{Mode: VaultKeyRotationRiskAccepted})
	if risk["recovery_disposition_mode"] != VaultKeyRotationRiskAccepted {
		t.Fatalf("risk metadata = %#v", risk)
	}
	if _, exists := risk["recovery_artifact_sha256"]; exists {
		t.Fatalf("risk audit metadata carried artifact digest: %#v", risk)
	}
	if err := checkEventShape(EventInput{
		AccountID: "acc_1", ActorKind: ActorAgent, ActorID: p.ID,
		Verb: VerbVaultKeyRotationCommitted, Metadata: risk,
	}); err != nil {
		t.Fatalf("risk audit shape: %v", err)
	}
	for _, forbidden := range []string{"artifact", "path", "passphrase", "key", "key_material"} {
		if _, exists := artifact[forbidden]; exists {
			t.Fatalf("audit metadata leaked %q: %#v", forbidden, artifact)
		}
	}
}

func TestVaultKeyRotationPlanHashIsDeterministicAndBinding(t *testing.T) {
	rotation := VaultKeyRotation{
		ID: "vkr_aaaaaaaaaaaaaaaa", SourceKeyID: "avk_bbbbbbbbbbbbbbbb",
		SourceKeyVersion: 1, SourceKeyAlgorithm: SecretAEADAlgorithm,
		SourceKeyFingerprint: strings.Repeat("a", 64), TargetKeyID: "avk_cccccccccccccccc",
		TargetKeyVersion: 2, TargetKeyAlgorithm: SecretAEADAlgorithm,
		TargetKeyFingerprint: strings.Repeat("b", 64), ItemCount: 1,
	}
	item := VaultKeyRotationItem{
		RotationID: "vkr_aaaaaaaaaaaaaaaa", SecretID: "sec_dddddddddddddddd",
		FieldID: "fld_eeeeeeeeeeeeeeee", FieldKind: SecretFieldPassword,
		DEKID: "dek_ffffffffffffffff", DEKGeneration: 1,
		SourceDEKRowVersion: 1, SourceWrapRevision: 1,
		SourceWrappedDEK:    bytes.Repeat([]byte{1}, vaultKeyRotationWrappedDEKBytes),
		SourceWrapAlgorithm: SecretAEADAlgorithm, SourceAADVersion: SecretAADVersion,
		SourceWrappingKeyID: "avk_bbbbbbbbbbbbbbbb", SourceWrappingKeyVersion: 1,
		TargetWrapRevision:  2,
		TargetWrapperSHA256: vaultKeyRotationWrapperHash(bytes.Repeat([]byte{2}, vaultKeyRotationWrappedDEKBytes)),
	}
	first := newVaultKeyRotationPlanHasher(rotation)
	first.Add(item)
	second := newVaultKeyRotationPlanHasher(rotation)
	second.Add(item)
	if first.Sum() != second.Sum() || !validFactSHA256(first.Sum()) {
		t.Fatalf("plan hashes = %q / %q", first.Sum(), second.Sum())
	}
	item.TargetWrapperSHA256 = vaultKeyRotationWrapperHash(bytes.Repeat([]byte{3}, vaultKeyRotationWrappedDEKBytes))
	changed := newVaultKeyRotationPlanHasher(rotation)
	changed.Add(item)
	if changed.Sum() == first.Sum() {
		t.Fatal("target wrapper change did not change plan hash")
	}
}

func TestVaultKeyRotationItemCursorRoundTrip(t *testing.T) {
	const dekID = "dek_abcdefghijklmnop"
	cursor, err := encodeVaultKeyRotationItemCursor(dekID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeVaultKeyRotationItemCursor(cursor)
	if err != nil || got != dekID {
		t.Fatalf("cursor round trip = %q / %v", got, err)
	}
	if _, err := decodeVaultKeyRotationItemCursor("not-a-cursor"); !errors.Is(err, ErrSecretInputInvalid) {
		t.Fatalf("bad cursor error = %v", err)
	}
}
