package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/sealed"
)

func TestVaultKeyLifecycleMutualExclusion(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}

	t.Run("sequential fences and terminal escape paths", func(t *testing.T) {
		ctx := context.Background()
		st, p, current := newVaultLifecycleTestAgent(ctx, t, baseDSN, "lifecycle-sequential@witwave.ai")

		target, err := sealed.GenerateAgentVaultKey(2)
		if err != nil {
			t.Fatal(err)
		}
		defer target.Clear()
		rotationInput := vaultLifecycleRotationInput(t, current, target,
			"rotation-blocked-by-enrollment")

		pendingInput := vaultLifecycleEnrollmentInput(t, "pending-before-rotation")
		pending, err := st.CreateVaultKeyEnrollment(ctx, p, pendingInput)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := st.StartVaultKeyRotation(ctx, p, rotationInput); !errors.Is(err, ErrVaultLifecycleInProgress) {
			t.Fatalf("start with pending enrollment error = %v", err)
		}

		sourceRecipient, err := sealed.GenerateAVKEnrollmentRecipientKey()
		if err != nil {
			t.Fatal(err)
		}
		defer sourceRecipient.Clear()
		sourcePublicKey, err := sourceRecipient.PublicKey()
		if err != nil {
			t.Fatal(err)
		}
		approved, err := st.ApproveVaultKeyEnrollment(ctx, p, pending.ID, ApproveVaultKeyEnrollmentInput{
			ExpectedRowVersion:       pending.RowVersion,
			SourceLocationID:         "loc_bbbbbbbbbbbbbbbb",
			SourceEphemeralPublicKey: sourcePublicKey,
			TransferCiphertext:       bytes.Repeat([]byte{7}, 64),
			TransferAlgorithm:        VaultEnrollmentTransferAlgorithm,
			ConsumeCommitment:        strings.Repeat("b", 64),
			IdempotencyKey:           "approve-before-rotation",
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := st.StartVaultKeyRotation(ctx, p, rotationInput); !errors.Is(err, ErrVaultLifecycleInProgress) {
			t.Fatalf("start with approved enrollment error = %v", err)
		}
		if _, err := st.CancelVaultKeyEnrollment(ctx, p, approved.ID, CancelVaultKeyEnrollmentInput{
			ExpectedRowVersion: approved.RowVersion,
			IdempotencyKey:     "cancel-before-rotation",
		}); err != nil {
			t.Fatal(err)
		}

		rotation, _, err := st.StartVaultKeyRotation(ctx, p, rotationInput)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.CreateVaultKeyEnrollment(ctx, p,
			vaultLifecycleEnrollmentInput(t, "create-during-rotation")); !errors.Is(err, ErrVaultKeyRotationInProgress) {
			t.Fatalf("create during rotation error = %v", err)
		}

		// Seed legacy/corrupt overlap directly so Approve and Consume are proven to
		// fail closed too. Normal store operations cannot create this state after
		// the mutual-exclusion fences above.
		overlap := vaultLifecycleEnrollmentInput(t, "overlap-during-rotation")
		if _, err := st.pool.Exec(ctx, `
			INSERT INTO agent_vault_key_enrollments
			       (id, account_id, realm_id, owner_agent_id,
			        vault_key_id, vault_key_version, target_location_id,
			        target_location_name, target_public_key, target_key_algorithm,
			        pairing_commitment, expires_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
			overlap.ID, p.AccountID, p.RealmID, p.ID, current.ID,
			current.KeyVersion, overlap.TargetLocationID, overlap.TargetLocationName,
			overlap.TargetPublicKey, overlap.TargetKeyAlgorithm,
			overlap.PairingCommitment, overlap.ExpiresAt); err != nil {
			t.Fatal(err)
		}
		if _, err := st.ApproveVaultKeyEnrollment(ctx, p, overlap.ID, ApproveVaultKeyEnrollmentInput{
			ExpectedRowVersion:       1,
			SourceLocationID:         "loc_cccccccccccccccc",
			SourceEphemeralPublicKey: sourcePublicKey,
			TransferCiphertext:       bytes.Repeat([]byte{8}, 64),
			TransferAlgorithm:        VaultEnrollmentTransferAlgorithm,
			ConsumeCommitment:        strings.Repeat("c", 64),
			IdempotencyKey:           "approve-during-rotation",
		}); !errors.Is(err, ErrVaultKeyRotationInProgress) {
			t.Fatalf("approve during rotation error = %v", err)
		}
		if _, err := st.pool.Exec(ctx, `
			UPDATE agent_vault_key_enrollments
			   SET lifecycle_state='approved', source_location_id=$5,
			       source_ephemeral_public_key=$6, transfer_ciphertext=$7,
			       transfer_algorithm=$8, consume_commitment=$9,
			       approved_at=clock_timestamp(), row_version=2
			 WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3 AND id=$4`,
			p.AccountID, p.RealmID, p.ID, overlap.ID,
			"loc_cccccccccccccccc", sourcePublicKey, bytes.Repeat([]byte{8}, 64),
			VaultEnrollmentTransferAlgorithm, strings.Repeat("c", 64)); err != nil {
			t.Fatal(err)
		}
		if _, err := st.ConsumeVaultKeyEnrollment(ctx, p, overlap.ID, ConsumeVaultKeyEnrollmentInput{
			ExpectedRowVersion: 2,
			TargetLocationID:   overlap.TargetLocationID,
			ConsumeProof:       bytes.Repeat([]byte{9}, 32),
			IdempotencyKey:     "consume-during-rotation",
		}); !errors.Is(err, ErrVaultKeyRotationInProgress) {
			t.Fatalf("consume during rotation error = %v", err)
		}
		if got, err := st.GetVaultKeyEnrollment(ctx, p, overlap.ID); err != nil || got.LifecycleState != VaultEnrollmentStateApproved {
			t.Fatalf("read during rotation = %#v / %v", got, err)
		}
		cancelledEnrollment, err := st.CancelVaultKeyEnrollment(ctx, p, overlap.ID,
			CancelVaultKeyEnrollmentInput{ExpectedRowVersion: 2, IdempotencyKey: "cancel-during-rotation"})
		if err != nil || cancelledEnrollment.LifecycleState != VaultEnrollmentStateCancelled {
			t.Fatalf("cancel enrollment during rotation = %#v / %v", cancelledEnrollment, err)
		}
		if _, _, err := st.CancelVaultKeyRotation(ctx, p, rotation.ID, CancelVaultKeyRotationInput{
			ExpectedRotationRowVersion: rotation.RowVersion,
			IdempotencyKey:             "cancel-rotation-after-enrollment-fences",
		}); err != nil {
			t.Fatal(err)
		}

		// A due request is not active authority. Start materializes its terminal
		// expiry in the same transaction instead of leaving a stale fence.
		due := vaultLifecycleEnrollmentInput(t, "expired-before-rotation")
		if _, err := st.pool.Exec(ctx, `
			INSERT INTO agent_vault_key_enrollments
			       (id, account_id, realm_id, owner_agent_id,
			        vault_key_id, vault_key_version, target_location_id,
			        target_location_name, target_public_key, target_key_algorithm,
			        pairing_commitment, created_at, expires_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,
			        clock_timestamp()-interval '2 minutes',
			        clock_timestamp()-interval '1 minute')`,
			due.ID, p.AccountID, p.RealmID, p.ID, current.ID, current.KeyVersion,
			due.TargetLocationID, due.TargetLocationName, due.TargetPublicKey,
			due.TargetKeyAlgorithm, due.PairingCommitment); err != nil {
			t.Fatal(err)
		}
		retryTarget, err := sealed.GenerateAgentVaultKey(2)
		if err != nil {
			t.Fatal(err)
		}
		defer retryTarget.Clear()
		retryRotation, _, err := st.StartVaultKeyRotation(ctx, p,
			vaultLifecycleRotationInput(t, current, retryTarget, "rotation-after-lazy-expiry"))
		if err != nil {
			t.Fatalf("start after due enrollment = %v", err)
		}
		var dueState string
		if err := st.pool.QueryRow(ctx, `SELECT lifecycle_state
			FROM agent_vault_key_enrollments WHERE id=$1`, due.ID).Scan(&dueState); err != nil || dueState != VaultEnrollmentStateExpired {
			t.Fatalf("due enrollment state = %q / %v", dueState, err)
		}
		if _, _, err := st.CancelVaultKeyRotation(ctx, p, retryRotation.ID, CancelVaultKeyRotationInput{
			ExpectedRotationRowVersion: retryRotation.RowVersion,
			IdempotencyKey:             "cancel-rotation-after-lazy-expiry",
		}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("concurrent begin chooses exactly one lifecycle", func(t *testing.T) {
		ctx := context.Background()
		st, p, current := newVaultLifecycleTestAgent(ctx, t, baseDSN, "lifecycle-concurrent@witwave.ai")
		target, err := sealed.GenerateAgentVaultKey(2)
		if err != nil {
			t.Fatal(err)
		}
		defer target.Clear()
		enrollmentInput := vaultLifecycleEnrollmentInput(t, "concurrent-enrollment")
		rotationInput := vaultLifecycleRotationInput(t, current, target, "concurrent-rotation")

		type enrollmentResult struct {
			value VaultKeyEnrollment
			err   error
		}
		type rotationResult struct {
			value VaultKeyRotation
			err   error
		}
		ready := make(chan struct{})
		enrollmentDone := make(chan enrollmentResult, 1)
		rotationDone := make(chan rotationResult, 1)
		go func() {
			<-ready
			value, err := st.CreateVaultKeyEnrollment(ctx, p, enrollmentInput)
			enrollmentDone <- enrollmentResult{value: value, err: err}
		}()
		go func() {
			<-ready
			value, _, err := st.StartVaultKeyRotation(ctx, p, rotationInput)
			rotationDone <- rotationResult{value: value, err: err}
		}()
		close(ready)
		enrollmentOutcome := <-enrollmentDone
		rotationOutcome := <-rotationDone

		if enrollmentOutcome.err == nil {
			if !errors.Is(rotationOutcome.err, ErrVaultLifecycleInProgress) {
				t.Fatalf("enrollment won but rotation error = %v", rotationOutcome.err)
			}
		} else if rotationOutcome.err == nil {
			if !errors.Is(enrollmentOutcome.err, ErrVaultKeyRotationInProgress) {
				t.Fatalf("rotation won but enrollment error = %v", enrollmentOutcome.err)
			}
		} else {
			t.Fatalf("neither lifecycle won: enrollment=%v rotation=%v",
				enrollmentOutcome.err, rotationOutcome.err)
		}
		var activeEnrollments, openRotations int64
		if err := st.pool.QueryRow(ctx, `
			SELECT
			  (SELECT count(*) FROM agent_vault_key_enrollments
			    WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3
			      AND lifecycle_state IN ('pending','approved')),
			  (SELECT count(*) FROM agent_vault_key_rotations
			    WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3
			      AND lifecycle_state='open')`, p.AccountID, p.RealmID, p.ID).Scan(
			&activeEnrollments, &openRotations); err != nil {
			t.Fatal(err)
		}
		if activeEnrollments+openRotations != 1 || activeEnrollments*openRotations != 0 {
			t.Fatalf("active enrollment/open rotation = %d/%d, want exactly one",
				activeEnrollments, openRotations)
		}
		if enrollmentOutcome.err == nil {
			if _, err := st.CancelVaultKeyEnrollment(ctx, p, enrollmentOutcome.value.ID,
				CancelVaultKeyEnrollmentInput{ExpectedRowVersion: enrollmentOutcome.value.RowVersion,
					IdempotencyKey: "cancel-concurrent-enrollment"}); err != nil {
				t.Fatal(err)
			}
		} else if _, _, err := st.CancelVaultKeyRotation(ctx, p, rotationOutcome.value.ID,
			CancelVaultKeyRotationInput{ExpectedRotationRowVersion: rotationOutcome.value.RowVersion,
				IdempotencyKey: "cancel-concurrent-rotation"}); err != nil {
			t.Fatal(err)
		}
	})
}

func newVaultLifecycleTestAgent(ctx context.Context, t *testing.T, baseDSN, email string) (*Store, Principal, VaultKeyBinding) {
	t.Helper()
	st, _ := newMigrationTestStore(t, baseDSN)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	provisioned, err := st.ProvisionAccount(ctx, email, "vault lifecycle", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate = %t / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "default")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "scott")
	if err != nil {
		t.Fatal(err)
	}
	p := Principal{Kind: PrincipalAgent, ID: agent.ID, AccountID: provisioned.AccountID,
		RealmID: realm.ID, AgentName: agent.Name, AccountStatus: "active", AccessProfile: AccessProfileFull}
	key, err := sealed.GenerateAgentVaultKey(1)
	if err != nil {
		t.Fatal(err)
	}
	defer key.Clear()
	metadata := key.Metadata()
	current, _, err := st.RegisterVaultKey(ctx, p, RegisterVaultKeyInput{
		ID: metadata.ID, KeyVersion: int64(metadata.Version), Algorithm: metadata.Algorithm,
		Fingerprint: metadata.Fingerprint, IdempotencyKey: "register-lifecycle-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	return st, p, current
}

func vaultLifecycleEnrollmentInput(t *testing.T, suffix string) CreateVaultKeyEnrollmentInput {
	t.Helper()
	recipient, err := sealed.GenerateAVKEnrollmentRecipientKey()
	if err != nil {
		t.Fatal(err)
	}
	defer recipient.Clear()
	publicKey, err := recipient.PublicKey()
	if err != nil {
		t.Fatal(err)
	}
	return CreateVaultKeyEnrollmentInput{
		ID: mustSecretTestID(t, "enr"), TargetLocationID: mustSecretTestID(t, "loc"),
		TargetLocationName: "test target", TargetPublicKey: publicKey,
		TargetKeyAlgorithm: VaultEnrollmentTargetKeyAlgorithm,
		PairingCommitment:  strings.Repeat("a", 64),
		ExpiresAt:          time.Now().UTC().Add(30 * time.Minute).Truncate(time.Second),
		IdempotencyKey:     "enrollment-" + suffix,
	}
}

func vaultLifecycleRotationInput(t *testing.T, current VaultKeyBinding, target *sealed.AgentVaultKey, idempotencyKey string) StartVaultKeyRotationInput {
	t.Helper()
	metadata := target.Metadata()
	return StartVaultKeyRotationInput{
		ID: mustSecretTestID(t, "vkr"), ExpectedSourceKeyID: current.ID,
		ExpectedSourceKeyVersion: current.KeyVersion, ExpectedSourceKeyRowVersion: current.RowVersion,
		TargetKeyID: metadata.ID, TargetKeyVersion: int64(metadata.Version),
		TargetAlgorithm: metadata.Algorithm, TargetFingerprint: metadata.Fingerprint,
		IdempotencyKey: idempotencyKey,
	}
}
