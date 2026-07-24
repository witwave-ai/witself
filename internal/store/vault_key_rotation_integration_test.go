package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/witwave-ai/witself/internal/sealed"
)

func TestVaultKeyRotationStagesAndFlipsAtomically(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, _ := newMigrationTestStore(t, baseDSN)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	provisioned, err := st.ProvisionAccount(ctx,
		"rotation@witwave.ai", "vault rotation", time.Hour)
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
	assertVaultLifecycleReceiptParentFKs(ctx, t, st, p)
	if open, err := st.GetOpenVaultKeyRotation(ctx, p); err != nil || open != nil {
		t.Fatalf("open rotation before start = %#v / %v", open, err)
	}

	oldKey, err := sealed.GenerateAgentVaultKey(1)
	if err != nil {
		t.Fatal(err)
	}
	defer oldKey.Clear()
	oldMetadata := oldKey.Metadata()
	current, _, err := st.RegisterVaultKey(ctx, p, RegisterVaultKeyInput{
		ID: oldMetadata.ID, KeyVersion: 1, Algorithm: oldMetadata.Algorithm,
		Fingerprint: oldMetadata.Fingerprint, IdempotencyKey: "rotation-register-v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertOrphanPendingVaultKeyFences(ctx, t, st, p, provisioned.OperatorID)
	assertSuspendedVaultEnrollmentCancellation(ctx, t, st, p)
	secretID := mustSecretTestID(t, "sec")
	fieldID := mustSecretTestID(t, "fld")
	scope := sealed.FieldScope{Domain: sealed.FieldValueDomain, AccountID: p.AccountID,
		RealmID: p.RealmID, OwnerAgentID: p.ID, SecretID: secretID, FieldID: fieldID}
	plaintext := []byte("rotation-canary")
	envelope, err := sealed.SealSensitiveField(oldKey, plaintext, sealed.SensitiveFieldOptions{
		Scope: scope, ValueVersion: 1, DEKGeneration: 1,
		ValueEncoding: sealed.ValueEncodingUTF8, WrapRevision: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	created, err := st.CreateSecret(ctx, p, CreateSecretInput{
		ID: secretID, Name: "rotation secret", Template: "login",
		IdempotencyKey: "rotation-create-v1",
		Fields: []CreateSecretFieldInput{{
			ID: fieldID, Name: "password", Kind: SecretFieldPassword, Sensitive: true,
			Encoding: SecretEncodingUTF8, ValueVersion: 1, Sealed: secretStoreEnvelope(envelope),
		}},
	})
	if err != nil || created.Secret.ID != secretID {
		t.Fatalf("create = %#v / %v", created, err)
	}

	newKey, err := sealed.GenerateAgentVaultKey(2)
	if err != nil {
		t.Fatal(err)
	}
	defer newKey.Clear()
	newMetadata := newKey.Metadata()
	rotationID := mustSecretTestID(t, "vkr")
	rotation, startReceipt, err := st.StartVaultKeyRotation(ctx, p, StartVaultKeyRotationInput{
		ID: rotationID, ExpectedSourceKeyID: current.ID,
		ExpectedSourceKeyVersion: current.KeyVersion, ExpectedSourceKeyRowVersion: current.RowVersion,
		TargetKeyID: newMetadata.ID, TargetKeyVersion: 2,
		TargetAlgorithm: newMetadata.Algorithm, TargetFingerprint: newMetadata.Fingerprint,
		IdempotencyKey: "rotation-start-v2",
	})
	if err != nil || rotation.LifecycleState != VaultKeyRotationOpen || rotation.ItemCount != 1 ||
		rotation.StagedCount != 0 || startReceipt.Replayed {
		t.Fatalf("start = %#v / %#v / %v", rotation, startReceipt, err)
	}
	if err := st.CloseAccount(ctx, p.AccountID, provisioned.OperatorID,
		"must not strand rotation"); !errors.Is(err, ErrVaultLifecycleInProgress) {
		t.Fatalf("close with open rotation error = %v", err)
	}
	if err := st.DeleteAgent(ctx, p.AccountID, p.RealmID, p.ID); !errors.Is(err, ErrVaultLifecycleInProgress) {
		t.Fatalf("delete agent with open rotation error = %v", err)
	}
	assertVaultRotationKeyMetadata(t, rotation, oldMetadata, newMetadata)
	if open, err := st.GetOpenVaultKeyRotation(ctx, p); err != nil || open == nil || open.ID != rotationID || open.RowVersion != rotation.RowVersion {
		t.Fatalf("open rotation after start = %#v / %v", open, err)
	} else {
		assertVaultRotationKeyMetadata(t, *open, oldMetadata, newMetadata)
	}
	startReplay, replayReceipt, err := st.StartVaultKeyRotation(ctx, p, StartVaultKeyRotationInput{
		ID: rotationID, ExpectedSourceKeyID: current.ID,
		ExpectedSourceKeyVersion: current.KeyVersion, ExpectedSourceKeyRowVersion: current.RowVersion,
		TargetKeyID: newMetadata.ID, TargetKeyVersion: 2,
		TargetAlgorithm: newMetadata.Algorithm, TargetFingerprint: newMetadata.Fingerprint,
		IdempotencyKey: "rotation-start-v2",
	})
	if err != nil || startReplay.ID != rotationID || !replayReceipt.Replayed {
		t.Fatalf("start replay = %#v / %#v / %v", startReplay, replayReceipt, err)
	}

	// The immutable snapshot must block a new sensitive wrapper while leaving
	// public-only writes and existing encrypted-material delivery available.
	blockedEnvelope, err := sealed.SealSensitiveField(oldKey, []byte("blocked"), sealed.SensitiveFieldOptions{
		Scope: sealed.FieldScope{Domain: sealed.FieldValueDomain, AccountID: p.AccountID,
			RealmID: p.RealmID, OwnerAgentID: p.ID, SecretID: "sec_aaaaaaaaaaaaaaaa",
			FieldID: "fld_bbbbbbbbbbbbbbbb"},
		ValueVersion: 1, DEKGeneration: 1, ValueEncoding: sealed.ValueEncodingUTF8, WrapRevision: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateSecret(ctx, p, CreateSecretInput{
		ID: "sec_aaaaaaaaaaaaaaaa", Name: "blocked sensitive", Template: "generic",
		IdempotencyKey: "rotation-blocked-sensitive",
		Fields: []CreateSecretFieldInput{{ID: "fld_bbbbbbbbbbbbbbbb", Name: "password",
			Kind: SecretFieldPassword, Sensitive: true, Encoding: SecretEncodingUTF8,
			ValueVersion: 1, Sealed: secretStoreEnvelope(blockedEnvelope)}},
	}); !errors.Is(err, ErrVaultKeyRotationInProgress) {
		t.Fatalf("sensitive create during rotation error = %v", err)
	}
	publicValue := "public"
	publicCreated, err := st.CreateSecret(ctx, p, CreateSecretInput{
		ID: "sec_cccccccccccccccc", Name: "public during rotation", Template: "generic",
		IdempotencyKey: "rotation-public-create",
		Fields: []CreateSecretFieldInput{{ID: "fld_dddddddddddddddd", Name: "username",
			Kind: SecretFieldUsername, Encoding: SecretEncodingUTF8,
			ValueVersion: 1, PublicValue: &publicValue}},
	})
	if err != nil {
		t.Fatalf("public create during rotation = %v", err)
	}
	deleteDuringRotation := SecretLifecycleInput{
		ExpectedRowVersion: publicCreated.Secret.RowVersion,
		IdempotencyKey:     "rotation-public-delete",
	}
	if _, err := st.DeleteSecret(ctx, p, publicCreated.Secret.ID, deleteDuringRotation); !errors.Is(err, ErrVaultKeyRotationInProgress) {
		t.Fatalf("delete while rotation open error = %v", err)
	}
	materialBefore, err := st.AccessSecretField(ctx, p, secretID, fieldID,
		AccessSecretFieldInput{IdempotencyKey: "rotation-access-before"})
	if err != nil {
		t.Fatal(err)
	}
	opened, err := sealed.OpenSensitiveField(oldKey, scope, secretEnvelopeFromMaterial(materialBefore))
	if err != nil || !bytes.Equal(opened, plaintext) {
		t.Fatalf("open during staging = %q / %v", opened, err)
	}
	clear(opened)

	page, err := st.ListVaultKeyRotationItems(ctx, p, rotationID,
		VaultKeyRotationItemListOptions{Limit: 1})
	if err != nil || len(page.Items) != 1 || page.Items[0].DEKID != envelope.DEKID {
		t.Fatalf("rotation item page = %#v / %v", page, err)
	}
	rewrapped, err := sealed.RewrapSensitiveFieldDEK(oldKey, newKey, scope, envelope, 2)
	if err != nil {
		t.Fatal(err)
	}
	stageInput := StageVaultKeyRotationInput{ExpectedRotationRowVersion: rotation.RowVersion,
		IdempotencyKey: "rotation-stage-v2", Items: []StageVaultKeyRotationItemInput{{
			DEKID: envelope.DEKID, ExpectedSourceDEKRowVersion: page.Items[0].SourceDEKRowVersion,
			ExpectedSourceWrapRevision: page.Items[0].SourceWrapRevision,
			TargetWrappedDEK:           rewrapped.WrappedDEK, TargetWrapRevision: 2,
		}}}
	rotation, stageReceipt, err := st.StageVaultKeyRotation(ctx, p, rotationID, stageInput)
	if err != nil || rotation.StagedCount != 1 || rotation.StagedPlanHash == "" || stageReceipt.Replayed {
		t.Fatalf("stage = %#v / %#v / %v", rotation, stageReceipt, err)
	}
	assertVaultRotationKeyMetadata(t, rotation, oldMetadata, newMetadata)
	stagedRevision := rotation.RowVersion
	stageReplay, stageReplayReceipt, err := st.StageVaultKeyRotation(ctx, p, rotationID, stageInput)
	if err != nil || stageReplay.RowVersion != stagedRevision || !stageReplayReceipt.Replayed {
		t.Fatalf("stage replay = %#v / %#v / %v", stageReplay, stageReplayReceipt, err)
	}
	if _, _, err := st.CommitVaultKeyRotation(ctx, p, rotationID,
		CommitVaultKeyRotationInput{ExpectedRotationRowVersion: rotation.RowVersion,
			ExpectedItemCount: rotation.ItemCount, ExpectedPlanHash: strings.Repeat("0", 64),
			RecoveryDisposition: VaultKeyRotationRecoveryDisposition{Mode: VaultKeyRotationRiskAccepted},
			IdempotencyKey:      "rotation-commit-wrong-plan"}); !errors.Is(err, ErrVaultKeyRotationConflict) {
		t.Fatalf("wrong-plan commit error = %v", err)
	}
	commitInput := CommitVaultKeyRotationInput{ExpectedRotationRowVersion: rotation.RowVersion,
		ExpectedItemCount: rotation.ItemCount, ExpectedPlanHash: rotation.StagedPlanHash,
		RecoveryDisposition: VaultKeyRotationRecoveryDisposition{
			Mode: VaultKeyRotationRecoveryArtifact, ArtifactSHA256: strings.Repeat("a", 64),
		},
		IdempotencyKey: "rotation-commit-v2"}
	rotation, commitReceipt, err := st.CommitVaultKeyRotation(ctx, p, rotationID, commitInput)
	if err != nil || rotation.LifecycleState != VaultKeyRotationCommitted || commitReceipt.Replayed {
		t.Fatalf("commit = %#v / %#v / %v", rotation, commitReceipt, err)
	}
	if rotation.RecoveryDispositionMode != VaultKeyRotationRecoveryArtifact ||
		rotation.RecoveryArtifactSHA256 != strings.Repeat("a", 64) {
		t.Fatalf("commit recovery disposition = %q / %q", rotation.RecoveryDispositionMode, rotation.RecoveryArtifactSHA256)
	}
	var auditRaw []byte
	if err := st.pool.QueryRow(ctx, `
		SELECT metadata
		  FROM account_events
		 WHERE account_id=$1 AND verb=$2 AND metadata->>'rotation_id'=$3`,
		p.AccountID, VerbVaultKeyRotationCommitted, rotationID).Scan(&auditRaw); err != nil {
		t.Fatalf("read rotation commit audit: %v", err)
	}
	var audit map[string]any
	if err := json.Unmarshal(auditRaw, &audit); err != nil ||
		audit["recovery_disposition_mode"] != VaultKeyRotationRecoveryArtifact ||
		audit["recovery_artifact_sha256"] != strings.Repeat("a", 64) {
		t.Fatalf("commit audit = %#v / %v", audit, err)
	}
	assertVaultRotationKeyMetadata(t, rotation, oldMetadata, newMetadata)
	if open, err := st.GetOpenVaultKeyRotation(ctx, p); err != nil || open != nil {
		t.Fatalf("open rotation after commit = %#v / %v", open, err)
	}
	commitReplay, commitReplayReceipt, err := st.CommitVaultKeyRotation(ctx, p, rotationID, commitInput)
	if err != nil || commitReplay.LifecycleState != VaultKeyRotationCommitted || !commitReplayReceipt.Replayed {
		t.Fatalf("commit replay = %#v / %#v / %v", commitReplay, commitReplayReceipt, err)
	}
	changedDisposition := commitInput
	changedDisposition.RecoveryDisposition = VaultKeyRotationRecoveryDisposition{Mode: VaultKeyRotationRiskAccepted}
	if _, _, err := st.CommitVaultKeyRotation(ctx, p, rotationID, changedDisposition); !errors.Is(err, ErrSecretIdempotencyConflict) {
		t.Fatalf("changed recovery disposition replay error = %v", err)
	}
	materialAfter, err := st.AccessSecretField(ctx, p, secretID, fieldID,
		AccessSecretFieldInput{IdempotencyKey: "rotation-access-after"})
	if err != nil {
		t.Fatal(err)
	}
	opened, err = sealed.OpenSensitiveField(newKey, scope, secretEnvelopeFromMaterial(materialAfter))
	if err != nil || !bytes.Equal(opened, plaintext) {
		t.Fatalf("open after commit = %q / %v", opened, err)
	}
	clear(opened)
	if _, err := sealed.OpenSensitiveField(oldKey, scope, secretEnvelopeFromMaterial(materialAfter)); !errors.Is(err, sealed.ErrIntegrity) {
		t.Fatalf("old key opened committed wrapper: %v", err)
	}
	currentAfter, err := st.GetCurrentVaultKey(ctx, p)
	if err != nil || currentAfter == nil || currentAfter.ID != newMetadata.ID || currentAfter.KeyVersion != 2 {
		t.Fatalf("current after commit = %#v / %v", currentAfter, err)
	}
	deletedAfterCommit, err := st.DeleteSecret(ctx, p, publicCreated.Secret.ID, deleteDuringRotation)
	if err != nil || deletedAfterCommit.Secret.Lifecycle != SecretLifecycleDeleted {
		t.Fatalf("delete after rotation commit = %#v / %v", deletedAfterCommit, err)
	}

	thirdKey, err := sealed.GenerateAgentVaultKey(3)
	if err != nil {
		t.Fatal(err)
	}
	defer thirdKey.Clear()
	thirdMetadata := thirdKey.Metadata()
	cancelRotation, _, err := st.StartVaultKeyRotation(ctx, p, StartVaultKeyRotationInput{
		ID: "vkr_eeeeeeeeeeeeeeee", ExpectedSourceKeyID: currentAfter.ID,
		ExpectedSourceKeyVersion:    currentAfter.KeyVersion,
		ExpectedSourceKeyRowVersion: currentAfter.RowVersion,
		TargetKeyID:                 thirdMetadata.ID, TargetKeyVersion: 3,
		TargetAlgorithm: thirdMetadata.Algorithm, TargetFingerprint: thirdMetadata.Fingerprint,
		IdempotencyKey: "rotation-start-v3",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertVaultRotationKeyMetadata(t, cancelRotation, newMetadata, thirdMetadata)
	if open, err := st.GetOpenVaultKeyRotation(ctx, p); err != nil || open == nil || open.ID != cancelRotation.ID {
		t.Fatalf("open rotation before cancel = %#v / %v", open, err)
	} else {
		assertVaultRotationKeyMetadata(t, *open, newMetadata, thirdMetadata)
	}
	cancelled, cancelReceipt, err := st.CancelVaultKeyRotation(ctx, p, cancelRotation.ID,
		CancelVaultKeyRotationInput{ExpectedRotationRowVersion: cancelRotation.RowVersion,
			IdempotencyKey: "rotation-cancel-v3"})
	if err != nil || cancelled.LifecycleState != VaultKeyRotationCancelled || cancelReceipt.Replayed {
		t.Fatalf("cancel = %#v / %#v / %v", cancelled, cancelReceipt, err)
	}
	assertVaultRotationKeyMetadata(t, cancelled, newMetadata, thirdMetadata)
	if open, err := st.GetOpenVaultKeyRotation(ctx, p); err != nil || open != nil {
		t.Fatalf("open rotation after cancel = %#v / %v", open, err)
	}
	currentAfterCancel, err := st.GetCurrentVaultKey(ctx, p)
	if err != nil || currentAfterCancel == nil || currentAfterCancel.ID != newMetadata.ID {
		t.Fatalf("current after cancel = %#v / %v", currentAfterCancel, err)
	}

	// A cancelled candidate never became an epoch. A fresh AVK id must be able
	// to retry the same logical source+1 version while both retired attempts
	// remain in immutable lifecycle history.
	retryKey, err := sealed.GenerateAgentVaultKey(3)
	if err != nil {
		t.Fatal(err)
	}
	defer retryKey.Clear()
	retryMetadata := retryKey.Metadata()
	retryRotation, _, err := st.StartVaultKeyRotation(ctx, p, StartVaultKeyRotationInput{
		ID: "vkr_ffffffffffffffff", ExpectedSourceKeyID: currentAfterCancel.ID,
		ExpectedSourceKeyVersion:    currentAfterCancel.KeyVersion,
		ExpectedSourceKeyRowVersion: currentAfterCancel.RowVersion,
		TargetKeyID:                 retryMetadata.ID, TargetKeyVersion: 3,
		TargetAlgorithm: retryMetadata.Algorithm, TargetFingerprint: retryMetadata.Fingerprint,
		IdempotencyKey: "rotation-retry-v3",
	})
	if err != nil || retryRotation.LifecycleState != VaultKeyRotationOpen {
		t.Fatalf("retry cancelled logical version = %#v / %v", retryRotation, err)
	}
	assertVaultRotationKeyMetadata(t, retryRotation, newMetadata, retryMetadata)
	if _, _, err := st.CancelVaultKeyRotation(ctx, p, retryRotation.ID, CancelVaultKeyRotationInput{
		ExpectedRotationRowVersion: retryRotation.RowVersion,
		IdempotencyKey:             "rotation-retry-v3-cancel",
	}); err != nil {
		t.Fatalf("cancel retry rotation = %v", err)
	}
	var versionThreeTotal, versionThreeLive int64
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*), count(*) FILTER (WHERE lifecycle_state IN ('pending','current'))
		  FROM agent_vault_keys
		 WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3 AND key_version=3`,
		p.AccountID, p.RealmID, p.ID).Scan(&versionThreeTotal, &versionThreeLive); err != nil {
		t.Fatal(err)
	}
	if versionThreeTotal != 2 || versionThreeLive != 0 {
		t.Fatalf("cancelled version 3 history/live = %d/%d, want 2/0", versionThreeTotal, versionThreeLive)
	}
}

func TestExportAccountRejectsNonPortableTerminalVaultLifecyclePostgres(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, _ := newMigrationTestStore(t, baseDSN)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	p := migration56TestPrincipal(ctx, t, st)
	sourceKey, source := migration56RegisterKey(ctx, t, st, p, 1,
		"non-portable-export-source")
	migration56CreateSecret(ctx, t, st, p, sourceKey, "non-portable-export")
	targetKey, err := sealed.GenerateAgentVaultKey(2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(targetKey.Clear)
	target := targetKey.Metadata()
	rotation, _, err := st.StartVaultKeyRotation(ctx, p, StartVaultKeyRotationInput{
		ID:                          mustSecretTestID(t, "vkr"),
		ExpectedSourceKeyID:         source.ID,
		ExpectedSourceKeyVersion:    source.KeyVersion,
		ExpectedSourceKeyRowVersion: source.RowVersion,
		TargetKeyID:                 target.ID,
		TargetKeyVersion:            int64(target.Version),
		TargetAlgorithm:             target.Algorithm,
		TargetFingerprint:           target.Fingerprint,
		IdempotencyKey:              "non-portable-export-start",
	})
	if err != nil || rotation.ItemCount != 1 {
		t.Fatalf("start export-fence rotation = %#v / %v", rotation, err)
	}

	// Simulate a terminal transition that was interrupted after the parent and
	// key states changed but before its cell-local staging workspace was purged.
	if _, err := st.pool.Exec(ctx, `
		UPDATE agent_vault_keys
		   SET lifecycle_state='retired', retired_at=created_at
		 WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3
		   AND id=$4 AND key_version=$5`, p.AccountID, p.RealmID, p.ID,
		target.ID, target.Version); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE agent_vault_key_rotations
		   SET lifecycle_state='cancelled', cancelled_at=now(), updated_at=now()
		 WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3 AND id=$4`,
		p.AccountID, p.RealmID, p.ID, rotation.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.SuspendAccountSystem(ctx, p.AccountID, "evacuation",
		"non-portable lifecycle export fence"); err != nil {
		t.Fatal(err)
	}
	assertRefusedExport := func(want error) {
		t.Helper()
		var archive bytes.Buffer
		err := st.ExportAccount(ctx, p.AccountID, "source-cell", "test", &archive)
		if !errors.Is(err, want) {
			t.Fatalf("non-portable lifecycle export error = %v, want %v", err, want)
		}
		if archive.Len() != 0 {
			t.Fatalf("refused lifecycle export wrote %d bytes", archive.Len())
		}
	}
	assertRefusedExport(ErrVaultLifecycleIntegrity)

	// With the workspace removed, an otherwise schema-valid cancelled row is
	// still not portable unless its target candidate remains retired.
	if _, err := st.pool.Exec(ctx, `
		DELETE FROM agent_vault_key_rotation_items
		 WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3 AND rotation_id=$4`,
		p.AccountID, p.RealmID, p.ID, rotation.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE agent_vault_keys
		   SET lifecycle_state='retired', retired_at=created_at
		 WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3
		   AND id=$4 AND key_version=$5`, p.AccountID, p.RealmID, p.ID,
		source.ID, source.KeyVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE agent_vault_keys
		   SET lifecycle_state='current', retired_at=NULL
		 WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3
		   AND id=$4 AND key_version=$5`, p.AccountID, p.RealmID, p.ID,
		target.ID, target.Version); err != nil {
		t.Fatal(err)
	}
	assertRefusedExport(ErrVaultLifecycleIntegrity)

	// A committed rotation must also report that every immutable snapshot item
	// was staged, even if a damaged row no longer carries the staging rows.
	if _, err := st.pool.Exec(ctx, `
		UPDATE agent_vault_key_rotations
		   SET lifecycle_state='committed',
		       recovery_disposition_mode='risk_accepted',
		       recovery_artifact_sha256=NULL,
		       staged_count=0,
		       committed_at=now(), cancelled_at=NULL, updated_at=now()
		 WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3 AND id=$4`,
		p.AccountID, p.RealmID, p.ID, rotation.ID); err != nil {
		t.Fatal(err)
	}
	assertRefusedExport(ErrVaultLifecycleIntegrity)
}

func TestSensitiveCreateReplayPrecedesVaultKeyMismatchAcrossRotation(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, _ := newMigrationTestStore(t, baseDSN)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	provisioned, err := st.ProvisionAccount(ctx,
		"rotation-create-replay@witwave.ai", "rotation create replay", time.Hour)
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

	sourceKey, err := sealed.GenerateAgentVaultKey(1)
	if err != nil {
		t.Fatal(err)
	}
	defer sourceKey.Clear()
	sourceMetadata := sourceKey.Metadata()
	current, _, err := st.RegisterVaultKey(ctx, p, RegisterVaultKeyInput{
		ID: sourceMetadata.ID, KeyVersion: int64(sourceMetadata.Version),
		Algorithm: sourceMetadata.Algorithm, Fingerprint: sourceMetadata.Fingerprint,
		IdempotencyKey: "rotation-create-replay-register",
	})
	if err != nil {
		t.Fatal(err)
	}

	acceptedSecretID := mustSecretTestID(t, "sec")
	acceptedFieldID := mustSecretTestID(t, "fld")
	acceptedScope := sealed.FieldScope{Domain: sealed.FieldValueDomain, AccountID: p.AccountID,
		RealmID: p.RealmID, OwnerAgentID: p.ID, SecretID: acceptedSecretID, FieldID: acceptedFieldID}
	acceptedPlaintext := []byte("accepted-before-rotation")
	acceptedEnvelope, err := sealed.SealSensitiveField(sourceKey, acceptedPlaintext, sealed.SensitiveFieldOptions{
		Scope: acceptedScope, ValueVersion: 1, DEKGeneration: 1,
		ValueEncoding: sealed.ValueEncodingUTF8, WrapRevision: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	acceptedInput := CreateSecretInput{
		ID: acceptedSecretID, Name: "accepted before rotation", Template: "login",
		IdempotencyKey: "rotation-create-accepted-response-lost",
		Fields: []CreateSecretFieldInput{{
			ID: acceptedFieldID, Name: "password", Kind: SecretFieldPassword, Sensitive: true,
			Encoding: SecretEncodingUTF8, ValueVersion: 1, Sealed: secretStoreEnvelope(acceptedEnvelope),
		}},
	}
	// Treat this successful return as the response that the client lost. The
	// exact encrypted request must remain recoverable after its key is retired.
	accepted, err := st.CreateSecret(ctx, p, acceptedInput)
	if err != nil || accepted.Receipt.Replayed {
		t.Fatalf("accepted create = %#v / %v", accepted, err)
	}

	targetKey, err := sealed.GenerateAgentVaultKey(2)
	if err != nil {
		t.Fatal(err)
	}
	defer targetKey.Clear()
	targetMetadata := targetKey.Metadata()
	rotationID := mustSecretTestID(t, "vkr")
	rotation, _, err := st.StartVaultKeyRotation(ctx, p, StartVaultKeyRotationInput{
		ID: rotationID, ExpectedSourceKeyID: current.ID,
		ExpectedSourceKeyVersion: current.KeyVersion, ExpectedSourceKeyRowVersion: current.RowVersion,
		TargetKeyID: targetMetadata.ID, TargetKeyVersion: int64(targetMetadata.Version),
		TargetAlgorithm: targetMetadata.Algorithm, TargetFingerprint: targetMetadata.Fingerprint,
		IdempotencyKey: "rotation-create-replay-start",
	})
	if err != nil || rotation.ItemCount != 1 || rotation.LifecycleState != VaultKeyRotationOpen {
		t.Fatalf("start rotation = %#v / %v", rotation, err)
	}

	blockedSecretID := mustSecretTestID(t, "sec")
	blockedFieldID := mustSecretTestID(t, "fld")
	blockedScope := sealed.FieldScope{Domain: sealed.FieldValueDomain, AccountID: p.AccountID,
		RealmID: p.RealmID, OwnerAgentID: p.ID, SecretID: blockedSecretID, FieldID: blockedFieldID}
	blockedPlaintext := []byte("generated-password-survives-rebase")
	blockedEnvelope, err := sealed.SealSensitiveField(sourceKey, blockedPlaintext, sealed.SensitiveFieldOptions{
		Scope: blockedScope, ValueVersion: 1, DEKGeneration: 1,
		ValueEncoding: sealed.ValueEncodingUTF8, WrapRevision: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	blockedInput := CreateSecretInput{
		ID: blockedSecretID, Name: "blocked during rotation", Template: "login",
		IdempotencyKey: "rotation-create-blocked-then-rebased",
		Fields: []CreateSecretFieldInput{{
			ID: blockedFieldID, Name: "password", Kind: SecretFieldPassword, Sensitive: true,
			Encoding: SecretEncodingUTF8, ValueVersion: 1, Sealed: secretStoreEnvelope(blockedEnvelope),
		}},
	}
	if _, err := st.CreateSecret(ctx, p, blockedInput); !errors.Is(err, ErrVaultKeyRotationInProgress) {
		t.Fatalf("create while rotation open error = %v", err)
	}
	assertSecretCreateReceiptCount(ctx, t, st, p, blockedInput.IdempotencyKey, 0)

	page, err := st.ListVaultKeyRotationItems(ctx, p, rotation.ID,
		VaultKeyRotationItemListOptions{Limit: 10})
	if err != nil || len(page.Items) != 1 || page.Items[0].DEKID != acceptedEnvelope.DEKID {
		t.Fatalf("rotation items = %#v / %v", page, err)
	}
	acceptedTargetEnvelope, err := sealed.RewrapSensitiveFieldDEK(
		sourceKey, targetKey, acceptedScope, acceptedEnvelope, 2,
	)
	if err != nil {
		t.Fatal(err)
	}
	rotation, _, err = st.StageVaultKeyRotation(ctx, p, rotation.ID, StageVaultKeyRotationInput{
		ExpectedRotationRowVersion: rotation.RowVersion,
		IdempotencyKey:             "rotation-create-replay-stage",
		Items: []StageVaultKeyRotationItemInput{{
			DEKID:                       acceptedEnvelope.DEKID,
			ExpectedSourceDEKRowVersion: page.Items[0].SourceDEKRowVersion,
			ExpectedSourceWrapRevision:  page.Items[0].SourceWrapRevision,
			TargetWrappedDEK:            acceptedTargetEnvelope.WrappedDEK,
			TargetWrapRevision:          int64(acceptedTargetEnvelope.WrapRevision),
		}},
	})
	if err != nil || rotation.StagedCount != rotation.ItemCount || rotation.StagedPlanHash == "" {
		t.Fatalf("stage rotation = %#v / %v", rotation, err)
	}
	rotation, _, err = st.CommitVaultKeyRotation(ctx, p, rotation.ID, CommitVaultKeyRotationInput{
		ExpectedRotationRowVersion: rotation.RowVersion,
		ExpectedItemCount:          rotation.ItemCount,
		ExpectedPlanHash:           rotation.StagedPlanHash,
		RecoveryDisposition: VaultKeyRotationRecoveryDisposition{
			Mode: VaultKeyRotationRiskAccepted,
		},
		IdempotencyKey: "rotation-create-replay-commit",
	})
	if err != nil || rotation.LifecycleState != VaultKeyRotationCommitted {
		t.Fatalf("commit rotation = %#v / %v", rotation, err)
	}

	// Receipt replay is deliberately checked before the retired wrapper is
	// compared with the current key. This is the response-loss recovery path.
	acceptedReplay, err := st.CreateSecret(ctx, p, acceptedInput)
	if err != nil || !acceptedReplay.Receipt.Replayed || acceptedReplay.Secret.ID != acceptedSecretID {
		t.Fatalf("accepted exact replay after commit = %#v / %v", acceptedReplay, err)
	}

	// The blocked request never acquired a receipt. Its exact old wrapper must
	// therefore reach the current-key check and receive the deterministic fence
	// that authorizes a client-side journal rebase.
	if _, err := st.CreateSecret(ctx, p, blockedInput); !errors.Is(err, ErrVaultKeyMismatch) {
		t.Fatalf("blocked old request after commit error = %v, want key mismatch", err)
	}
	assertSecretCreateReceiptCount(ctx, t, st, p, blockedInput.IdempotencyKey, 0)

	blockedTargetEnvelope, err := sealed.RewrapSensitiveFieldDEK(
		sourceKey, targetKey, blockedScope, blockedEnvelope, 2,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(blockedTargetEnvelope.Ciphertext, blockedEnvelope.Ciphertext) ||
		blockedTargetEnvelope.DEKID != blockedEnvelope.DEKID ||
		blockedTargetEnvelope.DEKGeneration != blockedEnvelope.DEKGeneration {
		t.Fatal("uncommitted create rewrap changed logical ciphertext or DEK identity")
	}
	rebasedInput := blockedInput
	rebasedInput.Fields = append([]CreateSecretFieldInput(nil), blockedInput.Fields...)
	rebasedInput.Fields[0].Sealed = secretStoreEnvelope(blockedTargetEnvelope)
	rebased, err := st.CreateSecret(ctx, p, rebasedInput)
	if err != nil || rebased.Receipt.Replayed || rebased.Secret.ID != blockedSecretID {
		t.Fatalf("rebased create = %#v / %v", rebased, err)
	}
	rebasedReplay, err := st.CreateSecret(ctx, p, rebasedInput)
	if err != nil || !rebasedReplay.Receipt.Replayed || rebasedReplay.Secret.ID != blockedSecretID {
		t.Fatalf("rebased exact replay = %#v / %v", rebasedReplay, err)
	}
	if _, err := st.CreateSecret(ctx, p, blockedInput); !errors.Is(err, ErrSecretIdempotencyConflict) {
		t.Fatalf("old request after rebased receipt error = %v, want idempotency conflict", err)
	}

	material, err := st.AccessSecretField(ctx, p, blockedSecretID, blockedFieldID,
		AccessSecretFieldInput{IdempotencyKey: "rotation-create-replay-access"})
	if err != nil {
		t.Fatal(err)
	}
	opened, err := sealed.OpenSensitiveField(targetKey, blockedScope, secretEnvelopeFromMaterial(material))
	if err != nil || !bytes.Equal(opened, blockedPlaintext) {
		t.Fatalf("rebased generated value = %q / %v, want original", opened, err)
	}
	clear(opened)
}

func assertSecretCreateReceiptCount(ctx context.Context, t *testing.T, st *Store, p Principal, idempotencyKey string, want int64) {
	t.Helper()
	var count int64
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM secret_mutation_receipts
		 WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3
		   AND actor_kind=$4 AND actor_id=$5 AND operation='secret_create'
		   AND idempotency_key_hash=$6`,
		p.AccountID, p.RealmID, p.ID, p.Kind, p.ID,
		secretIdempotencyKeyHash(idempotencyKey)).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Fatalf("secret create receipt count = %d, want %d", count, want)
	}
}

func assertVaultRotationKeyMetadata(t *testing.T, rotation VaultKeyRotation, source, target sealed.AgentVaultKeyMetadata) {
	t.Helper()
	if rotation.SourceKeyID != source.ID || rotation.SourceKeyVersion != int64(source.Version) ||
		rotation.SourceKeyAlgorithm != source.Algorithm || rotation.SourceKeyFingerprint != source.Fingerprint ||
		rotation.TargetKeyID != target.ID || rotation.TargetKeyVersion != int64(target.Version) ||
		rotation.TargetKeyAlgorithm != target.Algorithm || rotation.TargetKeyFingerprint != target.Fingerprint {
		t.Fatalf("rotation key metadata = %#v, want source=%#v target=%#v", rotation, source, target)
	}
}

func assertOrphanPendingVaultKeyFences(
	ctx context.Context,
	t *testing.T,
	st *Store,
	p Principal,
	operatorID string,
) {
	t.Helper()
	const pendingKeyID = "avk_zzzzzzzzzzzzzzzz"
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO agent_vault_keys
		       (id,account_id,realm_id,owner_agent_id,key_version,
		        algorithm,fingerprint,lifecycle_state,row_version)
		VALUES ($1,$2,$3,$4,2,$5,$6,'pending',7)`, pendingKeyID,
		p.AccountID, p.RealmID, p.ID, SecretAEADAlgorithm,
		strings.Repeat("9", 64)); err != nil {
		t.Fatal(err)
	}

	if err := st.CloseAccount(ctx, p.AccountID, operatorID,
		"orphan pending key fence"); !errors.Is(err, ErrVaultLifecycleInProgress) {
		t.Fatalf("close with orphan pending key error = %v", err)
	}
	if err := st.DeleteAgent(ctx, p.AccountID, p.RealmID, p.ID); !errors.Is(err, ErrVaultLifecycleInProgress) {
		t.Fatalf("delete agent with orphan pending key error = %v", err)
	}
	if err := st.SuspendAccountSystem(ctx, p.AccountID, "evacuation",
		"orphan pending key export fence"); err != nil {
		t.Fatal(err)
	}
	if err := st.ExportAccount(ctx, p.AccountID, "source-cell", "test", io.Discard); !errors.Is(err, ErrVaultLifecycleInProgress) {
		t.Fatalf("export with orphan pending key error = %v", err)
	}
	if err := st.ResumeAccountSystem(ctx, p.AccountID, "evacuation"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE agent_vault_keys
		   SET lifecycle_state='retired', retired_at=created_at
		 WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3 AND id=$4`,
		p.AccountID, p.RealmID, p.ID, pendingKeyID); err != nil {
		t.Fatal(err)
	}
}

func assertVaultLifecycleReceiptParentFKs(ctx context.Context, t *testing.T, st *Store, p Principal) {
	t.Helper()
	for _, test := range []struct {
		name, query, constraint string
	}{
		{
			name: "enrollment", constraint: "vault_key_enrollment_receipts_parent_fk",
			query: `INSERT INTO vault_key_enrollment_receipts
			       (account_id, realm_id, owner_agent_id, operation,
			        idempotency_key_hash, request_hash, enrollment_id, result_revision)
			       VALUES ($1,$2,$3,'enrollment_request',$4,$5,'enr_aaaaaaaaaaaaaaaa',1)`,
		},
		{
			name: "rotation", constraint: "vault_key_rotation_receipts_parent_fk",
			query: `INSERT INTO vault_key_rotation_receipts
			       (account_id, realm_id, owner_agent_id, operation,
			        idempotency_key_hash, request_hash, rotation_id, result_revision)
			       VALUES ($1,$2,$3,'rotation_start',$4,$5,'vkr_aaaaaaaaaaaaaaaa',1)`,
		},
	} {
		t.Run("receipt parent fk "+test.name, func(t *testing.T) {
			_, err := st.pool.Exec(ctx, test.query, p.AccountID, p.RealmID, p.ID,
				strings.Repeat("a", 64), strings.Repeat("b", 64))
			var postgresError *pgconn.PgError
			if !errors.As(err, &postgresError) || postgresError.Code != "23503" ||
				postgresError.ConstraintName != test.constraint {
				t.Fatalf("orphan %s receipt error = %#v / %v", test.name, postgresError, err)
			}
		})
	}
}

func assertSuspendedVaultEnrollmentCancellation(ctx context.Context, t *testing.T, st *Store, p Principal) {
	t.Helper()
	pendingRecipient, err := sealed.GenerateAVKEnrollmentRecipientKey()
	if err != nil {
		t.Fatal(err)
	}
	defer pendingRecipient.Clear()
	pendingPublicKey, err := pendingRecipient.PublicKey()
	if err != nil {
		t.Fatal(err)
	}
	approvedRecipient, err := sealed.GenerateAVKEnrollmentRecipientKey()
	if err != nil {
		t.Fatal(err)
	}
	defer approvedRecipient.Clear()
	approvedPublicKey, err := approvedRecipient.PublicKey()
	if err != nil {
		t.Fatal(err)
	}
	sourceEphemeral, err := sealed.GenerateAVKEnrollmentRecipientKey()
	if err != nil {
		t.Fatal(err)
	}
	defer sourceEphemeral.Clear()
	sourcePublicKey, err := sourceEphemeral.PublicKey()
	if err != nil {
		t.Fatal(err)
	}
	expiresAt := time.Now().UTC().Add(30 * time.Minute).Truncate(time.Second)
	pending, err := st.CreateVaultKeyEnrollment(ctx, p, CreateVaultKeyEnrollmentInput{
		ID: "enr_bbbbbbbbbbbbbbbb", TargetLocationID: "loc_bbbbbbbbbbbbbbbb",
		TargetLocationName: "pending target", TargetPublicKey: pendingPublicKey,
		TargetKeyAlgorithm: VaultEnrollmentTargetKeyAlgorithm,
		PairingCommitment:  strings.Repeat("c", 64), ExpiresAt: expiresAt,
		IdempotencyKey: "suspended-enrollment-pending-create",
	})
	if err != nil {
		t.Fatal(err)
	}
	approved, err := st.CreateVaultKeyEnrollment(ctx, p, CreateVaultKeyEnrollmentInput{
		ID: "enr_cccccccccccccccc", TargetLocationID: "loc_cccccccccccccccc",
		TargetLocationName: "approved target", TargetPublicKey: approvedPublicKey,
		TargetKeyAlgorithm: VaultEnrollmentTargetKeyAlgorithm,
		PairingCommitment:  strings.Repeat("d", 64), ExpiresAt: expiresAt,
		IdempotencyKey: "suspended-enrollment-approved-create",
	})
	if err != nil {
		t.Fatal(err)
	}
	approved, err = st.ApproveVaultKeyEnrollment(ctx, p, approved.ID, ApproveVaultKeyEnrollmentInput{
		ExpectedRowVersion: approved.RowVersion, SourceLocationID: "loc_dddddddddddddddd",
		SourceEphemeralPublicKey: sourcePublicKey, TransferCiphertext: bytes.Repeat([]byte{7}, 64),
		TransferAlgorithm: VaultEnrollmentTransferAlgorithm,
		ConsumeCommitment: strings.Repeat("e", 64),
		IdempotencyKey:    "suspended-enrollment-approve",
	})
	if err != nil || approved.LifecycleState != VaultEnrollmentStateApproved {
		t.Fatalf("approve enrollment = %#v / %v", approved, err)
	}
	if err := st.SuspendAccountSystem(ctx, p.AccountID, "evacuation", "test safety cancellation"); err != nil {
		t.Fatal(err)
	}
	pending, err = st.CancelVaultKeyEnrollment(ctx, p, pending.ID, CancelVaultKeyEnrollmentInput{
		ExpectedRowVersion: pending.RowVersion, IdempotencyKey: "suspended-enrollment-pending-cancel",
	})
	if err != nil || pending.LifecycleState != VaultEnrollmentStateCancelled {
		t.Fatalf("cancel pending enrollment while suspended = %#v / %v", pending, err)
	}
	approved, err = st.CancelVaultKeyEnrollment(ctx, p, approved.ID, CancelVaultKeyEnrollmentInput{
		ExpectedRowVersion: approved.RowVersion, IdempotencyKey: "suspended-enrollment-approved-cancel",
	})
	if err != nil || approved.LifecycleState != VaultEnrollmentStateCancelled {
		t.Fatalf("cancel approved enrollment while suspended = %#v / %v", approved, err)
	}
	var capsuleCleared bool
	if err := st.pool.QueryRow(ctx, `
		SELECT source_ephemeral_public_key IS NULL AND transfer_ciphertext IS NULL AND
		       transfer_algorithm IS NULL AND consume_commitment IS NULL
		  FROM agent_vault_key_enrollments
		 WHERE account_id=$1 AND realm_id=$2 AND owner_agent_id=$3 AND id=$4`,
		p.AccountID, p.RealmID, p.ID, approved.ID).Scan(&capsuleCleared); err != nil || !capsuleCleared {
		t.Fatalf("approved enrollment capsule cleared = %t / %v", capsuleCleared, err)
	}
	// Export performs account-scoped lazy expiry before checking for active
	// lifecycle work. Exercise that path with a due request so its UPDATE ...
	// RETURNING rows and per-request audit writes cannot regress into a busy pgx
	// connection.
	const dueEnrollmentID = "enr_eeeeeeeeeeeeeeee"
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO agent_vault_key_enrollments
		       (id, account_id, realm_id, owner_agent_id,
		        vault_key_id, vault_key_version, target_location_id,
		        target_location_name, target_public_key, target_key_algorithm,
		        pairing_commitment, created_at, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,'loc_eeeeeeeeeeeeeeee',
		        'due export target',$7,$8,$9,
		        clock_timestamp()-interval '2 minutes',
		        clock_timestamp()-interval '1 minute')`,
		dueEnrollmentID, p.AccountID, p.RealmID, p.ID,
		pending.VaultKeyID, pending.VaultKeyVersion, pendingPublicKey,
		VaultEnrollmentTargetKeyAlgorithm, strings.Repeat("f", 64)); err != nil {
		t.Fatal(err)
	}
	if err := st.ExportAccount(ctx, p.AccountID, "source-cell", "test", io.Discard); err != nil {
		t.Fatalf("export after suspended enrollment cancellation = %v", err)
	}
	var dueState string
	var dueExpiryEvents int64
	if err := st.pool.QueryRow(ctx, `
		SELECT e.lifecycle_state,
		       (SELECT count(*) FROM account_events a
		         WHERE a.account_id=e.account_id
		           AND a.verb=$2
		           AND a.metadata->>'enrollment_id'=e.id)
		  FROM agent_vault_key_enrollments e
		 WHERE e.account_id=$1 AND e.id=$3`,
		p.AccountID, VerbVaultEnrollmentExpired, dueEnrollmentID).Scan(
		&dueState, &dueExpiryEvents); err != nil || dueState != VaultEnrollmentStateExpired || dueExpiryEvents != 1 {
		t.Fatalf("export lazy expiry state/events = %q/%d / %v", dueState, dueExpiryEvents, err)
	}
	if err := st.ResumeAccountSystem(ctx, p.AccountID, "evacuation"); err != nil {
		t.Fatal(err)
	}
}
