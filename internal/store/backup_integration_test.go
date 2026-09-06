package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	avatardomain "github.com/witwave-ai/witself/internal/avatar"
	archiveexport "github.com/witwave-ai/witself/internal/export"
	"github.com/witwave-ai/witself/internal/testenv"
)

func TestAccountBackupValidationRollsBackPostgres(t *testing.T) {
	dsn := testenv.RequirePostgres(t)
	ctx := context.Background()
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}

	const (
		backupID       = "bkp_active_roundtrip"
		closedBackupID = "bkp_closed_roundtrip"
	)
	provisioned, err := st.ProvisionAccount(
		ctx,
		"active-backup-roundtrip@witwave.ai",
		"active backup roundtrip",
		time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	accountID := provisioned.AccountID
	defer func() {
		_ = deleteAccountForIntegrationTest(ctx, st, accountID)
	}()
	if activated, err := st.ActivateAccount(ctx, accountID); err != nil ||
		!activated {
		t.Fatalf("activate = %v / %v", activated, err)
	}

	var archive bytes.Buffer
	if err := st.ExportAccountBackup(
		ctx, accountID, backupID, "backup-source", "test", &archive,
	); err != nil {
		t.Fatal(err)
	}
	manifest, err := archiveexport.Read(
		ctx,
		bytes.NewReader(archive.Bytes()),
		archiveexport.ImportOptions{CurrentSchema: SchemaVersion()},
	)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Purpose != archiveexport.PurposeBackup ||
		manifest.BackupID != backupID ||
		manifest.AccountID != accountID ||
		manifest.Status != "active" ||
		manifest.EvacuationID != "" {
		t.Fatalf("backup manifest = %+v", manifest)
	}

	var status string
	var evacuationID *string
	if err := st.pool.QueryRow(ctx, `
		SELECT status, evacuation_id
		  FROM accounts
		 WHERE id=$1`, accountID).Scan(&status, &evacuationID); err != nil {
		t.Fatal(err)
	}
	if status != "active" || evacuationID != nil {
		t.Fatalf("source after backup status=%q evacuation_id=%v",
			status, evacuationID)
	}

	if _, err := st.ImportAccount(
		ctx, accountID, bytes.NewReader(archive.Bytes()),
	); !errors.Is(err, ErrArchiveContent) {
		t.Fatalf("generic import of backup = %v, want ErrArchiveContent", err)
	}

	if err := st.CloseAccount(
		ctx, accountID, provisioned.OperatorID, "closed backup validation",
	); err != nil {
		t.Fatal(err)
	}
	var closedArchive bytes.Buffer
	if err := st.ExportAccountBackup(
		ctx, accountID, closedBackupID,
		"backup-source", "test", &closedArchive,
	); err != nil {
		t.Fatal(err)
	}
	closedManifest, err := archiveexport.Read(
		ctx,
		bytes.NewReader(closedArchive.Bytes()),
		archiveexport.ImportOptions{CurrentSchema: SchemaVersion()},
	)
	if err != nil {
		t.Fatal(err)
	}
	if closedManifest.Purpose != archiveexport.PurposeBackup ||
		closedManifest.BackupID != closedBackupID ||
		closedManifest.AccountID != accountID ||
		closedManifest.Status != "closed" ||
		closedManifest.EvacuationID != "" {
		t.Fatalf("closed backup manifest = %+v", closedManifest)
	}
	if err := st.pool.QueryRow(ctx, `
		SELECT status, evacuation_id
		  FROM accounts
		 WHERE id=$1`, accountID).Scan(&status, &evacuationID); err != nil {
		t.Fatal(err)
	}
	if status != "closed" || evacuationID != nil {
		t.Fatalf("source after closed backup status=%q evacuation_id=%v",
			status, evacuationID)
	}

	if err := deleteAccountForIntegrationTest(ctx, st, accountID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ValidateAccountBackup(
		ctx, accountID, "bkp_wrong", bytes.NewReader(archive.Bytes()),
	); !errors.Is(err, ErrArchiveContent) {
		t.Fatalf("mismatched backup validation = %v, want ErrArchiveContent", err)
	}
	var accountRows int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM accounts WHERE id=$1`,
		accountID,
	).Scan(&accountRows); err != nil {
		t.Fatal(err)
	}
	if accountRows != 0 {
		t.Fatalf("mismatched backup validation landed %d account rows",
			accountRows)
	}

	for _, backup := range []struct {
		id     string
		status string
		body   []byte
	}{
		{id: backupID, status: "active", body: archive.Bytes()},
		{id: closedBackupID, status: "closed", body: closedArchive.Bytes()},
	} {
		for attempt := 1; attempt <= 2; attempt++ {
			validated, err := st.ValidateAccountBackup(
				ctx, accountID, backup.id, bytes.NewReader(backup.body),
			)
			if err != nil {
				t.Fatalf("%s validation attempt %d: %v",
					backup.status, attempt, err)
			}
			if validated.BackupID != backup.id ||
				validated.Purpose != archiveexport.PurposeBackup ||
				validated.Status != backup.status {
				t.Fatalf("validated %s manifest attempt %d = %+v",
					backup.status, attempt, validated)
			}
			if err := st.pool.QueryRow(ctx,
				`SELECT count(*) FROM accounts WHERE id=$1`,
				accountID,
			).Scan(&accountRows); err != nil {
				t.Fatal(err)
			}
			if accountRows != 0 {
				t.Fatalf(
					"%s validation attempt %d committed %d account rows",
					backup.status, attempt, accountRows,
				)
			}
		}
	}
}

func TestAccountBackupValidationCarriesCompactedAvatarLineagePostgres(t *testing.T) {
	dsn := testenv.RequirePostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	st, err := Open(ctx, dsn, WithAvatarPayloadCompactionEnabled(true))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	provisioned, err := st.ProvisionAccount(ctx,
		fmt.Sprintf("avatar-backup-%d@witwave.ai", time.Now().UnixNano()),
		"avatar backup validation", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := deleteAvatarAccountForIntegrationTest(context.Background(), st, provisioned.AccountID); err != nil {
			t.Errorf("clean up avatar backup account: %v", err)
		}
	}()
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate account = %t / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "avatar-backup")
	if err != nil {
		t.Fatal(err)
	}
	agent := createAvatarResetTestAgent(ctx, t, st, provisioned.AccountID,
		realm.ID, "backup-portrait")
	operator := Principal{Kind: PrincipalOperator, ID: provisioned.OperatorID,
		AccountID: provisioned.AccountID, AccountStatus: "active"}
	style, err := st.GetRealmAvatarStyle(ctx, agent, "")
	if err != nil {
		t.Fatal(err)
	}
	// A real safe payload must exceed the fixed-size continuity fingerprint:
	// quota compaction deliberately refuses to grow storage. Non-rendering
	// descriptions in an unlocked layer preserve the reference portrait while
	// exercising production compaction without falsifying stored byte counts.
	svg := strings.Replace(style.StylePack.References[0].SVG,
		`<g id="experience" data-layer="experience"></g>`,
		`<g id="experience" data-layer="experience">`+
			strings.Repeat("<desc>"+strings.Repeat("x", 512)+"</desc>", 80)+"</g>", 1)
	propose := func(revision, parent, version int64) AvatarMutationResult {
		t.Helper()
		result, err := st.ProposeAvatar(ctx, agent, ProposeAvatarInput{
			ExpectedProfileRevision: revision, ParentVersion: parent,
			StylePackID: style.StylePack.ID, StylePackVersion: style.StylePack.Version,
			SubjectForm: avatardomain.SubjectHuman, SVG: svg,
			Description:    "A portable portrait for rollback-only backup validation.",
			VisualSpec:     json.RawMessage(`{"identity":{"expression":"calm"}}`),
			IdempotencyKey: fmt.Sprintf("avatar-backup-propose-%d", version),
		})
		if err != nil {
			t.Fatalf("propose version %d: %v", version, err)
		}
		if result.Avatar.Profile.ProposedVersion != version {
			t.Fatalf("proposal version = %d, want %d", result.Avatar.Profile.ProposedVersion, version)
		}
		return result
	}
	activate := func(proposal AvatarMutationResult) AvatarMutationResult {
		t.Helper()
		version := proposal.Avatar.Profile.ProposedVersion
		result, err := st.ActivateAvatar(ctx, agent, ActivateAvatarInput{
			Version: version, ExpectedProfileRevision: proposal.Avatar.Profile.ProfileRevision,
			IdempotencyKey: fmt.Sprintf("avatar-backup-activate-%d", version),
		})
		if err != nil {
			t.Fatalf("activate version %d: %v", version, err)
		}
		return result
	}
	active := activate(propose(1, 0, 1))
	active = activate(propose(active.Avatar.Profile.ProfileRevision, 1, 2))
	rolledBack, err := st.RollbackAvatar(ctx, agent, RollbackAvatarInput{
		Version: 1, ExpectedProfileRevision: active.Avatar.Profile.ProfileRevision,
		IdempotencyKey: "avatar-backup-rollback-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	pending := propose(rolledBack.Avatar.Profile.ProfileRevision, 1, 3)
	rejected, err := st.RejectAgentAvatar(ctx, operator, agent.ID, RejectAvatarInput{
		Version: 3, ExpectedProfileRevision: pending.Avatar.Profile.ProfileRevision,
		ReasonCode: "operator_declined", IdempotencyKey: "avatar-backup-reject-3",
	})
	if err != nil {
		t.Fatal(err)
	}
	active = activate(propose(rejected.Avatar.Profile.ProfileRevision, 1, 4))
	reset, err := st.ResetAvatar(ctx, agent, ResetAvatarInput{
		ExpectedProfileRevision: active.Avatar.Profile.ProfileRevision,
		ReasonCode:              "new_direction", IdempotencyKey: "avatar-backup-reset",
	})
	if err != nil {
		t.Fatal(err)
	}
	active = activate(propose(reset.Avatar.Profile.ProfileRevision, 0, 5))
	quota, err := st.SetAvatarQuota(ctx, operator, agent.ID, UpdateAvatarQuotaInput{
		RetainedPayloadCountLimit: AvatarMinRetainedPayloadCountLimit,
		RetainedPayloadByteLimit:  AvatarMaxRetainedPayloadByteLimit,
		ExpectedProfileRevision:   active.Avatar.Profile.ProfileRevision,
		IdempotencyKey:            "avatar-backup-quota",
	})
	if err != nil {
		t.Fatal(err)
	}
	if quota.Avatar.Profile.ActiveVersion != 5 || quota.Avatar.Profile.LatestVersion != 5 ||
		quota.Avatar.Profile.LineageGeneration != 2 || quota.Avatar.Profile.RetainedPayloadCount != 4 {
		t.Fatalf("backup avatar profile = %#v", quota.Avatar.Profile)
	}

	const backupID = "bkp_compacted_avatar_lineage"
	var archive bytes.Buffer
	if err := st.ExportAccountBackup(ctx, provisioned.AccountID, backupID,
		"backup-source", "test", &archive); err != nil {
		t.Fatal(err)
	}
	manifest, rows := readAvatarArchiveRows(t, archive.Bytes(), SchemaVersion())
	if manifest.Purpose != archiveexport.PurposeBackup || manifest.Status != "active" ||
		manifest.BackupID != backupID || manifest.SchemaVersion != SchemaVersion() {
		t.Fatalf("avatar backup manifest = %+v", manifest)
	}
	for table, want := range map[string]int{
		"agent_avatar_profiles": 1, "agent_avatar_versions": 5,
		"agent_avatar_activations": 5, "agent_avatar_rejections": 1,
		"agent_avatar_resets": 1, "avatar_mutation_receipts": 13,
	} {
		if got := len(rows[table]); got != want {
			t.Fatalf("backup %s rows = %d, want %d", table, got, want)
		}
	}
	var compactedIndex = -1
	var fingerprint []byte
	for i, row := range rows["agent_avatar_versions"] {
		var version map[string]any
		if err := json.Unmarshal(row, &version); err != nil {
			t.Fatal(err)
		}
		if version["version"] != float64(1) {
			continue
		}
		compactedIndex = i
		fingerprint, err = importedAvatarContinuityFingerprint(version)
		if err != nil {
			t.Fatal(err)
		}
		if version["payload_state"] != "compacted" || version["payload_compaction_reason"] != "quota" ||
			version["lineage_generation"] != float64(1) || version["renderer_profile"] != "perceptual-v1" ||
			version["svg"] != nil || version["description"] != nil || version["visual_spec"] != nil ||
			len(fingerprint) != avatardomain.PerceptualContinuityFingerprintBytes {
			t.Fatal("backup omitted the quota-compacted retired parent or its continuity fingerprint")
		}
	}
	if compactedIndex < 0 {
		t.Fatal("backup omitted version 1")
	}
	if err := avatardomain.ValidatePerceptualContinuityFingerprintForStyle(fingerprint, style.StylePack); err != nil {
		t.Fatalf("backup continuity fingerprint: %v", err)
	}
	if err := deleteAvatarAccountForIntegrationTest(ctx, st, provisioned.AccountID); err != nil {
		t.Fatal(err)
	}
	assertNoImportedRows := func() {
		t.Helper()
		for _, table := range []string{
			"avatar_style_packs", "avatar_style_pack_versions", "realm_avatar_styles",
			"avatar_style_rollout_jobs", "agent_avatar_profiles", "agent_avatar_versions",
			"agent_avatar_activations", "agent_avatar_rejections", "agent_avatar_resets",
			"avatar_mutation_receipts",
		} {
			var count int
			if err := st.pool.QueryRow(ctx, "SELECT count(*) FROM "+table+" WHERE account_id=$1",
				provisioned.AccountID).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 0 {
				t.Fatalf("rollback-only validation left %d rows in %s", count, table)
			}
		}
		var accountRows int
		if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM accounts WHERE id=$1`,
			provisioned.AccountID).Scan(&accountRows); err != nil {
			t.Fatal(err)
		}
		if accountRows != 0 {
			t.Fatalf("rollback-only validation left %d account rows", accountRows)
		}
	}
	assertNoImportedRows()
	for attempt := 1; attempt <= 2; attempt++ {
		validated, err := st.ValidateAccountBackup(ctx, provisioned.AccountID, backupID,
			bytes.NewReader(archive.Bytes()))
		if err != nil {
			t.Fatalf("avatar backup validation attempt %d: %v", attempt, err)
		}
		if validated.Purpose != archiveexport.PurposeBackup || validated.Status != "active" ||
			validated.AccountID != provisioned.AccountID || validated.BackupID != backupID {
			t.Fatalf("validated avatar backup attempt %d = %+v", attempt, validated)
		}
		assertNoImportedRows()
	}
	// A checksummed archive with a broken compacted-parent boundary must reach
	// semantic validation. A successful no-op drill cannot satisfy this case.
	var brokenParent map[string]any
	if err := json.Unmarshal(rows["agent_avatar_versions"][compactedIndex], &brokenParent); err != nil {
		t.Fatal(err)
	}
	brokenParent["continuity_fingerprint"] = nil
	rows["agent_avatar_versions"][compactedIndex], err = json.Marshal(brokenParent)
	if err != nil {
		t.Fatal(err)
	}
	brokenArchive := writeAvatarArchiveRows(t, manifest, canonicalArchiveTableNamesForSchema(SchemaVersion()), rows)
	if _, err := st.ValidateAccountBackup(ctx, provisioned.AccountID, backupID,
		bytes.NewReader(brokenArchive)); !errors.Is(err, ErrArchiveContent) {
		t.Fatalf("backup missing continuity fingerprint = %v, want ErrArchiveContent", err)
	}
	assertNoImportedRows()
}
