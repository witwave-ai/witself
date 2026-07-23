package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pressly/goose/v3"

	avatardomain "github.com/witwave-ai/witself/internal/avatar"
	"github.com/witwave-ai/witself/internal/id"
)

func createSchema50AvatarAgentForMigrationTest(ctx context.Context, t *testing.T,
	st *Store, accountID, realmID, name string) Agent {
	t.Helper()
	agentID, err := id.New("agent")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		`INSERT INTO agents (id,realm_id,name) VALUES ($1,$2,$3)`,
		agentID, realmID, name); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO agent_avatar_profiles
		       (account_id,realm_id,agent_id,style_pack_id,
		        style_pack_version,fallback_seed)
		SELECT $1,$2,$3,style_pack_id,style_pack_version,$3
		  FROM realm_avatar_styles
		 WHERE account_id=$1 AND realm_id=$2`, accountID, realmID,
		agentID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return Agent{ID: agentID, Name: name}
}

func TestMigration51BackfillsLockedDigestsAndRefusesCompactedDowngradePostgres(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	st, dsn := newMigrationTestStore(t, baseDSN)
	migrationTestUpTo(t, dsn, 50)
	ctx := context.Background()
	provisioned, err := st.ProvisionAccount(ctx,
		"avatar-quota-migration@witwave.ai", "avatar quota migration", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate = %t / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "migration")
	if err != nil {
		t.Fatal(err)
	}
	agent := createSchema50AvatarAgentForMigrationTest(ctx, t, st,
		provisioned.AccountID, realm.ID, "migration-avatar")
	pack := avatardomain.BuiltInFlatVectorStylePack()
	reference := pack.References[0]
	digest := sha256.Sum256([]byte(reference.SVG))
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO agent_avatar_versions
		       (account_id, realm_id, agent_id, id, version, lineage_generation,
		        style_pack_id, style_pack_version, subject_form, svg, description,
		        visual_spec, svg_sha256, provenance, proposed_by_kind,
		        proposed_by_id)
		VALUES ($1,$2,$3,'avver_aaaaaaaaaaaaaaaa',1,1,$4,1,'human',$5,$6,
		        '{"identity":{"expression":"calm"}}'::jsonb,$7,
		        '{"runtime":"migration-test"}'::jsonb,'agent',$3)`,
		provisioned.AccountID, realm.ID, agent.ID, pack.ID, reference.SVG,
		"A migration backfill portrait.", hex.EncodeToString(digest[:])); err != nil {
		t.Fatal(err)
	}
	migrationTestUpTo(t, dsn, 51)
	if _, err := st.finalizeAvatarLockedLayerDigestMigration(ctx); err != nil {
		t.Fatal(err)
	}
	assertMigrationTestVersion(t, dsn, 51)
	wantLockedDigest, err := avatardomain.LockedLayersSHA256([]byte(reference.SVG), pack)
	if err != nil {
		t.Fatal(err)
	}
	var state, lockedDigest string
	var payloadBytes int64
	if err := st.pool.QueryRow(ctx, `
		SELECT payload_state, payload_bytes, locked_layers_sha256
		  FROM agent_avatar_versions WHERE agent_id=$1 AND version=1`, agent.ID).
		Scan(&state, &payloadBytes, &lockedDigest); err != nil {
		t.Fatal(err)
	}
	if state != "full" || payloadBytes < 1 || lockedDigest != wantLockedDigest {
		t.Fatalf("schema-51 backfill = state:%q bytes:%d locked:%q", state, payloadBytes, lockedDigest)
	}
	fingerprint, err := avatardomain.BuildPerceptualContinuityFingerprint(
		[]byte(reference.SVG), pack)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE agent_avatar_versions SET continuity_fingerprint=$2
		 WHERE agent_id=$1 AND version=1`, agent.ID, fingerprint); err == nil {
		t.Fatal("schema-51 accepted a continuity fingerprint on a full payload")
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE agent_avatar_versions
		   SET payload_state='compacted', svg=NULL, description=NULL,
		       visual_spec=NULL, payload_compacted_at=clock_timestamp(),
		       payload_compaction_reason='quota', continuity_fingerprint=$2
		 WHERE agent_id=$1 AND version=1`, agent.ID, fingerprint[:len(fingerprint)-1]); err == nil {
		t.Fatal("schema-51 accepted a compacted continuity fingerprint with the wrong length")
	}
	if err := migrationTestDown(t, dsn, false); err != nil {
		t.Fatal(err)
	}
	assertMigrationTestVersion(t, dsn, 50)
	assertMigrationTestColumn(t, st, "agent_avatar_profiles",
		"payload_quota_reconciliation_required", false)
	migrationTestUpTo(t, dsn, 51)
	if _, err := st.finalizeAvatarLockedLayerDigestMigration(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE agent_avatar_versions
		   SET payload_state='compacted', svg=NULL, description=NULL,
		       visual_spec=NULL, payload_compacted_at=clock_timestamp(),
		       payload_compaction_reason='quota', continuity_fingerprint=$2
		 WHERE agent_id=$1 AND version=1`, agent.ID, fingerprint); err != nil {
		t.Fatal(err)
	}
	var retainedFingerprintBytes int
	if err := st.pool.QueryRow(ctx, `
		SELECT octet_length(continuity_fingerprint) FROM agent_avatar_versions
		 WHERE agent_id=$1 AND version=1`, agent.ID).Scan(&retainedFingerprintBytes); err != nil {
		t.Fatal(err)
	}
	if retainedFingerprintBytes != avatardomain.PerceptualContinuityFingerprintBytes {
		t.Fatalf("schema-51 continuity fingerprint length = %d, want %d",
			retainedFingerprintBytes, avatardomain.PerceptualContinuityFingerprintBytes)
	}
	if err := migrationTestDown(t, dsn, true); err == nil {
		t.Fatal("schema-51 downgrade accepted an irreversible compacted payload")
	}
	assertMigrationTestVersion(t, dsn, 51)
}

func TestMigration51BackfillBatchesLargeHistoryPostgres(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	st, dsn := newMigrationTestStore(t, baseDSN)
	migrationTestUpTo(t, dsn, 50)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	provisioned, err := st.ProvisionAccount(ctx,
		"avatar-quota-batched-migration@witwave.ai", "avatar quota batched migration", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate = %t / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "batched-migration")
	if err != nil {
		t.Fatal(err)
	}
	agent := createSchema50AvatarAgentForMigrationTest(ctx, t, st,
		provisioned.AccountID, realm.ID, "batched-migration-avatar")
	pack := avatardomain.BuiltInFlatVectorStylePack()
	reference := pack.References[0]
	digest := sha256.Sum256([]byte(reference.SVG))
	const historyRows = avatarLockedLayerDigestBackfillBatchSize*2 + 17
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for version := int64(1); version <= historyRows; version++ {
		versionID, err := id.New("avver")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO agent_avatar_versions
			       (account_id, realm_id, agent_id, id, version, lineage_generation,
			        style_pack_id, style_pack_version, subject_form, svg, description,
			        visual_spec, svg_sha256, provenance, proposed_by_kind,
			        proposed_by_id)
			VALUES ($1,$2,$3,$4,$5,1,$6,1,'human',$7,$8,
			        '{"identity":{"expression":"calm"}}'::jsonb,$9,
			        '{"runtime":"migration-batch-test"}'::jsonb,'agent',$3)`,
			provisioned.AccountID, realm.ID, agent.ID, versionID, version,
			pack.ID, reference.SVG, "A batched migration backfill portrait.",
			hex.EncodeToString(digest[:])); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// Apply the SQL half first so the test can inspect the application-level
	// finalizer's real transactional batch envelope.
	migrationTestUpTo(t, dsn, 51)
	legacyVersionID, err := id.New("avver")
	if err != nil {
		t.Fatal(err)
	}
	insertedDuringBackfill := false
	stats, err := st.backfillAvatarLockedLayerDigests(ctx,
		avatarLockedLayerDigestBackfillFilter{},
		func(progress avatarLockedLayerDigestBackfillStats) error {
			if progress.batches != 1 || insertedDuringBackfill {
				return nil
			}
			insertedDuringBackfill = true
			_, err := st.pool.Exec(ctx, `
				INSERT INTO agent_avatar_versions
				       (account_id, realm_id, agent_id, id, version,
				        lineage_generation, style_pack_id, style_pack_version,
				        subject_form, svg, description, visual_spec, svg_sha256,
				        provenance, proposed_by_kind, proposed_by_id)
				VALUES ($1,$2,$3,$4,$5,1,$6,1,'human',$7,$8,
				        '{"identity":{"expression":"calm"}}'::jsonb,$9,
				        '{"runtime":"schema-50-concurrent-writer"}'::jsonb,
				        'agent',$3)`, provisioned.AccountID, realm.ID, agent.ID,
				legacyVersionID, int64(historyRows+1), pack.ID, reference.SVG,
				"A concurrent schema-50 writer portrait.",
				hex.EncodeToString(digest[:]))
			return err
		})
	if err != nil {
		t.Fatal(err)
	}
	if !insertedDuringBackfill || stats.rows != historyRows+1 || stats.batches != 3 ||
		stats.maxBatchSize != avatarLockedLayerDigestBackfillBatchSize {
		t.Fatalf("backfill stats = %#v inserted:%t, want rows:%d batches:3 max:%d", stats,
			insertedDuringBackfill, historyRows+1,
			avatarLockedLayerDigestBackfillBatchSize)
	}
	var total, digests int
	var notNull, temporaryProofExists bool
	if err := st.pool.QueryRow(ctx, `
		SELECT COUNT(*), COUNT(locked_layers_sha256)
		  FROM agent_avatar_versions WHERE agent_id=$1`, agent.ID).
		Scan(&total, &digests); err != nil {
		t.Fatal(err)
	}
	if err := st.pool.QueryRow(ctx, `
		SELECT attnotnull
		  FROM pg_attribute
		 WHERE attrelid='agent_avatar_versions'::regclass
		   AND attname='locked_layers_sha256' AND NOT attisdropped`).
		Scan(&notNull); err != nil {
		t.Fatal(err)
	}
	if err := st.pool.QueryRow(ctx, `
		SELECT EXISTS (
		  SELECT 1 FROM pg_constraint
		   WHERE conname='agent_avatar_versions_locked_layers_sha256_not_null'
		     AND conrelid='agent_avatar_versions'::regclass
		)`).Scan(&temporaryProofExists); err != nil {
		t.Fatal(err)
	}
	if total != historyRows+1 || digests != historyRows+1 || notNull || temporaryProofExists {
		t.Fatalf("finalized backfill = total:%d digests:%d not-null:%t proof:%t",
			total, digests, notNull, temporaryProofExists)
	}
}

func TestMigration51Schema50WriterRemainsReadableExportableAndRestartBackfilledPostgres(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	st, dsn := newMigrationTestStore(t, baseDSN)
	migrationTestUpTo(t, dsn, 50)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	provisioned, err := st.ProvisionAccount(ctx,
		"avatar-mixed-writer@witwave.ai", "avatar mixed writer", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate = %t / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "mixed-writer")
	if err != nil {
		t.Fatal(err)
	}
	agent := createSchema50AvatarAgentForMigrationTest(ctx, t, st,
		provisioned.AccountID, realm.ID, "mixed-writer-avatar")
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	assertMigrationTestVersion(t, dsn, int64(SchemaVersion()))

	pack := avatardomain.BuiltInFlatVectorStylePack()
	reference := pack.References[0]
	svgDigest := sha256.Sum256([]byte(reference.SVG))
	wantLockedDigest, err := avatardomain.LockedLayersSHA256(
		[]byte(reference.SVG), pack)
	if err != nil {
		t.Fatal(err)
	}
	insertSchema50Version := func(version int64, runtime string) {
		t.Helper()
		versionID, err := id.New("avver")
		if err != nil {
			t.Fatal(err)
		}
		var parent any
		if version > 1 {
			parent = version - 1
		}
		// This is the schema-50 writer's exact column surface: it names neither
		// payload_bytes nor locked_layers_sha256.
		if _, err := st.pool.Exec(ctx, `
			INSERT INTO agent_avatar_versions
			       (account_id, realm_id, agent_id, id, version, parent_version,
			        lineage_generation, style_pack_id, style_pack_version,
			        subject_form, svg, description, visual_spec, svg_sha256,
			        provenance, proposed_by_kind, proposed_by_id, proposed_at)
			VALUES ($1,$2,$3,$4,$5,$6,1,$7,1,'human',$8,$9,
			        '{"identity":{"expression":"calm"}}'::jsonb,$10,
			        jsonb_build_object('runtime',$11::text),'agent',$3,
			        clock_timestamp())`,
			provisioned.AccountID, realm.ID, agent.ID, versionID, version,
			parent, pack.ID, reference.SVG, "A schema-50 mixed-writer portrait.",
			hex.EncodeToString(svgDigest[:]), runtime); err != nil {
			t.Fatal(err)
		}
	}
	insertSchema50Version(1, "schema-50-after-schema-54")

	var payloadBytes, derivedPayloadBytes int64
	var storedLockedDigest *string
	if err := st.pool.QueryRow(ctx, `
		SELECT payload_bytes,
		       octet_length(svg)+octet_length(description)+octet_length(visual_spec::text),
		       locked_layers_sha256
		  FROM agent_avatar_versions
		 WHERE agent_id=$1 AND version=1`, agent.ID).Scan(
		&payloadBytes, &derivedPayloadBytes, &storedLockedDigest); err != nil {
		t.Fatal(err)
	}
	if payloadBytes != derivedPayloadBytes || payloadBytes < 1 || storedLockedDigest != nil {
		t.Fatalf("legacy insert = bytes:%d derived:%d locked:%v",
			payloadBytes, derivedPayloadBytes, storedLockedDigest)
	}
	p := Principal{Kind: PrincipalAgent, ID: agent.ID,
		AccountID: provisioned.AccountID, RealmID: realm.ID,
		AgentName: agent.Name, AccountStatus: "active"}
	detail, err := st.GetAvatarVersion(ctx, p, 1)
	if err != nil || detail.LockedLayersSHA256 != wantLockedDigest {
		t.Fatalf("legacy exact read = %#v / %v", detail, err)
	}
	history, err := st.GetAvatarHistory(ctx, p, 10)
	if err != nil || len(history.Versions) != 1 ||
		history.Versions[0].LockedLayersSHA256 != wantLockedDigest {
		t.Fatalf("legacy history read = %#v / %v", history, err)
	}
	if err := st.pool.QueryRow(ctx, `
		SELECT locked_layers_sha256 FROM agent_avatar_versions
		 WHERE agent_id=$1 AND version=1`, agent.ID).Scan(&storedLockedDigest); err != nil {
		t.Fatal(err)
	}
	if storedLockedDigest != nil {
		t.Fatal("read path unexpectedly mutated the legacy digest")
	}

	// A Phase-B config restart calls Migrate again even at schema 54 and
	// repairs the nullable digest before the process serves.
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := st.pool.QueryRow(ctx, `
		SELECT locked_layers_sha256 FROM agent_avatar_versions
		 WHERE agent_id=$1 AND version=1`, agent.ID).Scan(&storedLockedDigest); err != nil {
		t.Fatal(err)
	}
	if storedLockedDigest == nil || *storedLockedDigest != wantLockedDigest {
		t.Fatalf("restart backfill locked digest = %v", storedLockedDigest)
	}

	// Export owns a frozen account row and repairs any final legacy write that
	// landed after startup before emitting a schema-54 archive.
	insertSchema50Version(2, "schema-50-before-export")
	if err := st.SuspendAccountSystem(ctx, provisioned.AccountID,
		"evacuation", "mixed-writer export"); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := st.ExportAccount(ctx, provisioned.AccountID,
		"source-cell", "test", &archive); err != nil {
		t.Fatal(err)
	}
	_, archived := readAvatarArchiveRows(t, archive.Bytes(), SchemaVersion())
	if len(archived["agent_avatar_versions"]) != 2 {
		t.Fatalf("archived avatar versions = %d, want 2",
			len(archived["agent_avatar_versions"]))
	}
	for _, raw := range archived["agent_avatar_versions"] {
		var row map[string]any
		if err := json.Unmarshal(raw, &row); err != nil {
			t.Fatal(err)
		}
		digest, ok := row["locked_layers_sha256"].(string)
		if !ok || digest != wantLockedDigest {
			t.Fatalf("archived legacy digest = %#v", row["locked_layers_sha256"])
		}
	}
}

func insertAvatarQuotaReconciliationHistory(ctx context.Context, t *testing.T,
	st *Store, accountID, realmID, agentID, operatorID string,
	firstVersion, lastVersion int64, schema50Writer, rejectHead bool) {
	t.Helper()
	pack := avatardomain.BuiltInFlatVectorStylePack()
	reference := pack.References[0]
	svgDigest := sha256.Sum256([]byte(reference.SVG))
	lockedDigest, err := avatardomain.LockedLayersSHA256([]byte(reference.SVG), pack)
	if err != nil {
		t.Fatal(err)
	}
	description := "A schema-50 portable portrait retained for quota reconciliation."
	visualSpec := json.RawMessage(`{"identity":{"expression":"calm"}}`)
	payloadBytes, err := avatarCreativePayloadBytes(reference.SVG, description, visualSpec)
	if err != nil {
		t.Fatal(err)
	}
	for version := firstVersion; version <= lastVersion; version++ {
		versionID, err := id.New("avver")
		if err != nil {
			t.Fatal(err)
		}
		if schema50Writer {
			_, err = st.pool.Exec(ctx, `
				INSERT INTO agent_avatar_versions
				       (account_id, realm_id, agent_id, id, version,
				        lineage_generation, style_pack_id, style_pack_version,
				        subject_form, svg, description, visual_spec, svg_sha256,
				        provenance, proposed_by_kind, proposed_by_id, proposed_at)
				VALUES ($1,$2,$3,$4,$5,1,$6,1,'human',$7,$8,$9,$10,
				        '{"runtime":"schema-50-round-trip"}'::jsonb,
				        'operator',$11,clock_timestamp())`, accountID, realmID,
				agentID, versionID, version, pack.ID, reference.SVG, description,
				visualSpec, hex.EncodeToString(svgDigest[:]), operatorID)
		} else {
			_, err = st.pool.Exec(ctx, `
				INSERT INTO agent_avatar_versions
				       (account_id, realm_id, agent_id, id, version,
				        lineage_generation, style_pack_id, style_pack_version,
				        subject_form, svg, description, visual_spec, svg_sha256,
				        locked_layers_sha256, provenance, proposed_by_kind,
				        proposed_by_id, proposed_at, payload_bytes)
				VALUES ($1,$2,$3,$4,$5,1,$6,1,'human',$7,$8,$9,$10,$11,
				        '{"runtime":"current-round-trip"}'::jsonb,
				        'operator',$12,clock_timestamp(),$13)`, accountID, realmID,
				agentID, versionID, version, pack.ID, reference.SVG, description,
				visualSpec, hex.EncodeToString(svgDigest[:]), lockedDigest,
				operatorID, payloadBytes)
		}
		if err != nil {
			t.Fatalf("insert avatar version %d: %v", version, err)
		}
		if version < lastVersion || rejectHead {
			rejectionID, err := id.New("avrej")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.pool.Exec(ctx, `
				INSERT INTO agent_avatar_rejections
				       (id,account_id,realm_id,agent_id,avatar_version,
				        reason_code,rejected_by_kind,rejected_by_id,rejected_at)
				VALUES ($1,$2,$3,$4,$5,'superseded','operator',$6,
				        clock_timestamp())`, rejectionID, accountID, realmID,
				agentID, version, operatorID); err != nil {
				t.Fatalf("reject avatar version %d: %v", version, err)
			}
		}
	}
	status := string(avatardomain.StatusProposed)
	var proposed any = lastVersion
	if rejectHead {
		status = string(avatardomain.StatusRejected)
		proposed = nil
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE agent_avatar_profiles
		   SET status=$4, latest_avatar_version=$5,
		       proposed_avatar_version=$6, active_avatar_version=NULL,
		       subject_form='human', revision=revision+1,
		       updated_at=clock_timestamp()
		 WHERE account_id=$1 AND realm_id=$2 AND agent_id=$3`, accountID,
		realmID, agentID, status, lastVersion, proposed); err != nil {
		t.Fatal(err)
	}
}

func TestAvatarQuotaReconciliationLegacyHistoriesRoundTripPostgres(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	tests := []struct {
		name               string
		existingAtSchema50 bool
	}{
		{name: "existing schema-50 overage", existingAtSchema50: true},
		{name: "late schema-50 writer", existingAtSchema50: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			st, dsn := newMigrationTestStore(t, baseDSN)
			if test.existingAtSchema50 {
				migrationTestUpTo(t, dsn, 50)
			} else if err := st.Migrate(); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			provisioned, err := st.ProvisionAccount(ctx,
				fmt.Sprintf("avatar-quota-reconcile-%d@witwave.ai", time.Now().UnixNano()),
				"avatar quota reconciliation", time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
				t.Fatalf("activate = %t / %v", activated, err)
			}
			realm, err := st.CreateRealm(ctx, provisioned.AccountID, "quota-reconcile")
			if err != nil {
				t.Fatal(err)
			}
			agent := createSchema50AvatarAgentForMigrationTest(ctx, t, st,
				provisioned.AccountID, realm.ID, "quota-reconcile-agent")
			if test.existingAtSchema50 {
				insertAvatarQuotaReconciliationHistory(ctx, t, st,
					provisioned.AccountID, realm.ID, agent.ID, provisioned.OperatorID,
					1, 21, true, false)
				if err := st.Migrate(); err != nil {
					t.Fatal(err)
				}
			} else {
				insertAvatarQuotaReconciliationHistory(ctx, t, st,
					provisioned.AccountID, realm.ID, agent.ID, provisioned.OperatorID,
					1, 20, false, true)
				var marked bool
				if err := st.pool.QueryRow(ctx, `
					SELECT payload_quota_reconciliation_required
					  FROM agent_avatar_profiles WHERE agent_id=$1`, agent.ID).
					Scan(&marked); err != nil || marked {
					t.Fatalf("pre-legacy marker = %t / %v, want false", marked, err)
				}
				insertAvatarQuotaReconciliationHistory(ctx, t, st,
					provisioned.AccountID, realm.ID, agent.ID, provisioned.OperatorID,
					21, 21, true, false)
			}
			var marked bool
			var fullCount int
			if err := st.pool.QueryRow(ctx, `
				SELECT p.payload_quota_reconciliation_required,
				       COUNT(*) FILTER (WHERE v.payload_state='full')
				  FROM agent_avatar_profiles p
				  JOIN agent_avatar_versions v ON v.agent_id=p.agent_id
				 WHERE p.agent_id=$1
				 GROUP BY p.payload_quota_reconciliation_required`, agent.ID).
				Scan(&marked, &fullCount); err != nil || !marked || fullCount != 21 {
				t.Fatalf("legacy overage = marker:%t full:%d / %v", marked, fullCount, err)
			}
			if err := st.SuspendAccountSystem(ctx, provisioned.AccountID,
				"evacuation", "avatar quota reconciliation round trip"); err != nil {
				t.Fatal(err)
			}
			var archive bytes.Buffer
			if err := st.ExportAccount(ctx, provisioned.AccountID,
				"source-cell", "test", &archive); err != nil {
				t.Fatal(err)
			}
			if err := deleteAccountForIntegrationTest(ctx, st, provisioned.AccountID); err != nil {
				t.Fatal(err)
			}
			if _, err := st.ImportAccount(ctx, provisioned.AccountID,
				bytes.NewReader(archive.Bytes())); err != nil {
				t.Fatalf("import current-schema legacy overage: %v", err)
			}
			if err := st.pool.QueryRow(ctx, `
				SELECT p.payload_quota_reconciliation_required,
				       COUNT(*) FILTER (WHERE v.payload_state='full')
				  FROM agent_avatar_profiles p
				  JOIN agent_avatar_versions v ON v.agent_id=p.agent_id
				 WHERE p.agent_id=$1
				 GROUP BY p.payload_quota_reconciliation_required`, agent.ID).
				Scan(&marked, &fullCount); err != nil || !marked || fullCount != 21 {
				t.Fatalf("restored legacy overage = marker:%t full:%d / %v",
					marked, fullCount, err)
			}

			phaseB, err := Open(ctx, dsn, WithAvatarPayloadCompactionEnabled(true))
			if err != nil {
				t.Fatal(err)
			}
			defer phaseB.Close()
			if err := phaseB.Migrate(); err != nil {
				t.Fatal(err)
			}
			if err := phaseB.ResumeAccountSystem(ctx, provisioned.AccountID, "evacuation"); err != nil {
				t.Fatal(err)
			}
			var revision int64
			if err := phaseB.pool.QueryRow(ctx, `
				SELECT revision FROM agent_avatar_profiles WHERE agent_id=$1`, agent.ID).
				Scan(&revision); err != nil {
				t.Fatal(err)
			}
			operator := Principal{Kind: PrincipalOperator, ID: provisioned.OperatorID,
				AccountID: provisioned.AccountID, AccountStatus: "active"}
			if _, err := phaseB.SetAvatarQuota(ctx, operator, agent.ID,
				UpdateAvatarQuotaInput{
					RetainedPayloadCountLimit: AvatarDefaultRetainedPayloadCountLimit,
					RetainedPayloadByteLimit:  AvatarDefaultRetainedPayloadByteLimit,
					ExpectedProfileRevision:   revision,
					IdempotencyKey:            "reconcile-restored-legacy-overage",
				}); err != nil {
				t.Fatalf("phase-B reconcile restored legacy overage: %v", err)
			}
			if err := phaseB.pool.QueryRow(ctx, `
				SELECT p.payload_quota_reconciliation_required,
				       COUNT(*) FILTER (WHERE v.payload_state='full')
				  FROM agent_avatar_profiles p
				  JOIN agent_avatar_versions v ON v.agent_id=p.agent_id
				 WHERE p.agent_id=$1
				 GROUP BY p.payload_quota_reconciliation_required`, agent.ID).
				Scan(&marked, &fullCount); err != nil || marked || fullCount != 20 {
				t.Fatalf("reconciled restored overage = marker:%t full:%d / %v",
					marked, fullCount, err)
			}
		})
	}
}

func TestMigration54QuarantinesLegacyWritersAndRefusesV1DowngradePostgres(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	st, dsn := newMigrationTestStore(t, baseDSN)
	migrationTestUpTo(t, dsn, 53)
	ctx := context.Background()
	provisioned, err := st.ProvisionAccount(ctx,
		"avatar-renderer-migration@witwave.ai", "avatar renderer migration", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate = %t / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "renderer-migration")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "renderer-migration-avatar")
	if err != nil {
		t.Fatal(err)
	}
	pack := avatardomain.BuiltInFlatVectorStylePack()
	reference := pack.References[0]
	digest := sha256.Sum256([]byte(reference.SVG))
	lockedDigest, err := avatardomain.LockedLayersSHA256([]byte(reference.SVG), pack)
	if err != nil {
		t.Fatal(err)
	}
	description := "A renderer migration portrait."
	visualSpec := []byte(`{"identity":{"expression":"calm"}}`)
	payloadBytes, err := avatarCreativePayloadBytes(reference.SVG, description, visualSpec)
	if err != nil {
		t.Fatal(err)
	}
	insertOldWriterVersion := func(version int64, versionID string) {
		t.Helper()
		// This is the pre-schema-54 writer shape: it intentionally omits
		// renderer_profile while preserving every schema-51 payload field.
		if _, err := st.pool.Exec(ctx, `
			INSERT INTO agent_avatar_versions
			       (account_id, realm_id, agent_id, id, version, lineage_generation,
			        style_pack_id, style_pack_version, subject_form, svg, description,
			        visual_spec, svg_sha256, locked_layers_sha256, provenance,
			        proposed_by_kind, proposed_by_id, payload_bytes)
			VALUES ($1,$2,$3,$4,$5,1,$6,1,'human',$7,$8,$9::jsonb,$10,$11,
			        '{"runtime":"v185-test"}'::jsonb,'agent',$3,$12)`,
			provisioned.AccountID, realm.ID, agent.ID, versionID, version,
			pack.ID, reference.SVG, description, string(visualSpec),
			hex.EncodeToString(digest[:]), lockedDigest, payloadBytes); err != nil {
			t.Fatal(err)
		}
	}

	insertOldWriterVersion(1, "avver_aaaaaaaaaaaaaaaa")
	legacyFingerprint, err := avatardomain.BuildPerceptualContinuityFingerprint(
		[]byte(reference.SVG), pack)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE agent_avatar_versions
		   SET payload_state='compacted',svg=NULL,description=NULL,visual_spec=NULL,
		       payload_compacted_at=clock_timestamp(),payload_compaction_reason='quota',
		       continuity_fingerprint=$2
		 WHERE agent_id=$1 AND version=1`, agent.ID, legacyFingerprint); err != nil {
		t.Fatal(err)
	}
	migrationTestUpTo(t, dsn, 54)
	assertMigrationTestColumn(t, st, "agent_avatar_versions", "renderer_profile", true)
	assertMigrationTestTableConstraint(t, st, "agent_avatar_versions",
		"agent_avatar_versions_renderer_profile_check", true)
	var rendererProfile string
	var retainedLegacyFingerprint []byte
	if err := st.pool.QueryRow(ctx, `
		SELECT renderer_profile,continuity_fingerprint FROM agent_avatar_versions
		 WHERE agent_id=$1 AND version=1`, agent.ID).
		Scan(&rendererProfile, &retainedLegacyFingerprint); err != nil {
		t.Fatal(err)
	}
	if rendererProfile != string(avatardomain.RendererProfileLegacy) {
		t.Fatalf("schema-53 row renderer profile = %q, want legacy", rendererProfile)
	}
	if len(retainedLegacyFingerprint) != 0 {
		t.Fatalf("schema-53 fingerprint survived legacy quarantine: %d bytes",
			len(retainedLegacyFingerprint))
	}
	downErr := migrationTestDown(t, dsn, true)
	if downErr == nil || !strings.Contains(downErr.Error(), "compacted avatar versions exist") {
		t.Fatalf("schema-54 compacted-history downgrade error = %v", downErr)
	}
	assertMigrationTestVersion(t, dsn, 54)
	if _, err := st.pool.Exec(ctx, `
		DELETE FROM agent_avatar_versions
		 WHERE agent_id=$1 AND version=1`, agent.ID); err != nil {
		t.Fatal(err)
	}

	// A still-running v185 writer executes the same INSERT after schema 54.
	// The database default quarantines the row as legacy, and v186 can read it.
	insertOldWriterVersion(2, "avver_bbbbbbbbbbbbbbbb")
	principal := Principal{Kind: PrincipalAgent, ID: agent.ID,
		AccountID: provisioned.AccountID, RealmID: realm.ID, AgentName: agent.Name,
		AccountStatus: "active"}
	exact, err := st.GetAvatarVersion(ctx, principal, 2)
	if err != nil || exact.RendererProfile != avatardomain.RendererProfileLegacy ||
		exact.SVG != reference.SVG {
		t.Fatalf("old-writer exact avatar = %#v / %v", exact, err)
	}

	// A database containing only quarantined legacy rows can safely remove the
	// provenance column.
	if err := migrationTestDown(t, dsn, false); err != nil {
		t.Fatal(err)
	}
	assertMigrationTestVersion(t, dsn, 53)
	assertMigrationTestColumn(t, st, "agent_avatar_versions", "renderer_profile", false)
	migrationTestUpTo(t, dsn, 54)

	// Legacy bytes can never become a WAPF seed, even through a direct writer.
	fingerprint, err := avatardomain.BuildPerceptualContinuityFingerprint(
		[]byte(reference.SVG), pack)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE agent_avatar_versions
		   SET payload_state='compacted',svg=NULL,description=NULL,visual_spec=NULL,
		       payload_compacted_at=clock_timestamp(),payload_compaction_reason='quota',
		       continuity_fingerprint=$2
		 WHERE agent_id=$1 AND version=2`, agent.ID, fingerprint); err == nil {
		t.Fatal("schema 54 accepted a continuity fingerprint on a legacy avatar")
	}

	if _, err := st.pool.Exec(ctx, `
		UPDATE agent_avatar_versions SET renderer_profile='perceptual-v1'
		 WHERE agent_id=$1 AND version=2`, agent.ID); err != nil {
		t.Fatal(err)
	}
	downErr = migrationTestDown(t, dsn, true)
	if downErr == nil || !strings.Contains(downErr.Error(), "perceptual-v1 avatar versions exist") {
		t.Fatalf("schema-54 v1 downgrade error = %v", downErr)
	}
	assertMigrationTestVersion(t, dsn, 54)
	assertMigrationTestColumn(t, st, "agent_avatar_versions", "renderer_profile", true)
	if err := st.pool.QueryRow(ctx, `
		SELECT renderer_profile FROM agent_avatar_versions
		 WHERE agent_id=$1 AND version=2`, agent.ID).Scan(&rendererProfile); err != nil ||
		rendererProfile != string(avatardomain.RendererProfilePerceptualV1) {
		t.Fatalf("renderer profile after refused down = %q / %v", rendererProfile, err)
	}
}

func TestMigration54DownLocksBeforeRendererSafetyCheckPostgres(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, dsn := newMigrationTestStore(t, baseDSN)
	migrationTestUpTo(t, dsn, 41)
	insertMigrationTestMemoryPrincipals(t, st)
	migrationTestUpTo(t, dsn, 54)

	pack := avatardomain.BuiltInFlatVectorStylePack()
	reference := pack.References[0]
	digest := sha256.Sum256([]byte(reference.SVG))
	lockedDigest, err := avatardomain.LockedLayersSHA256([]byte(reference.SVG), pack)
	if err != nil {
		t.Fatal(err)
	}
	description := "A renderer downgrade race portrait."
	visualSpec := `{"identity":{"expression":"calm"}}`
	payloadBytes, err := avatarCreativePayloadBytes(reference.SVG, description, []byte(visualSpec))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO agent_avatar_versions
		       (account_id,realm_id,agent_id,id,version,lineage_generation,
		        style_pack_id,style_pack_version,subject_form,svg,description,
		        visual_spec,svg_sha256,locked_layers_sha256,provenance,
		        proposed_by_kind,proposed_by_id,payload_bytes)
		VALUES ('acc_memory_trigger','realm_memory_trigger','agent_memory_owner',
		        'avver_aaaaaaaaaaaaaaaa',1,1,$1,1,'human',$2,$3,$4::jsonb,$5,$6,
		        '{"runtime":"v185-race"}'::jsonb,'agent','agent_memory_owner',$7)`,
		pack.ID, reference.SVG, description, visualSpec, hex.EncodeToString(digest[:]),
		lockedDigest, payloadBytes); err != nil {
		t.Fatal(err)
	}

	writer, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Exec(ctx, `
		UPDATE agent_avatar_versions SET renderer_profile='perceptual-v1'
		 WHERE agent_id='agent_memory_owner' AND version=1`); err != nil {
		_ = writer.Rollback(ctx)
		t.Fatal(err)
	}
	db := migrationTestSQLDB(t, dsn)
	defer func() { _ = db.Close() }()
	downDone := make(chan error, 1)
	go func() { downDone <- goose.Down(db, "migrations") }()
	waiting := false
	for attempts := 0; attempts < 100; attempts++ {
		if err := st.pool.QueryRow(ctx, `
			SELECT EXISTS (
			  SELECT 1 FROM pg_locks
			   WHERE relation='agent_avatar_versions'::regclass
			     AND mode='AccessExclusiveLock' AND NOT granted
			)`).Scan(&waiting); err != nil {
			_ = writer.Rollback(ctx)
			t.Fatal(err)
		}
		if waiting {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !waiting {
		_ = writer.Rollback(ctx)
		t.Fatal("schema-54 down never waited for the renderer writer")
	}
	if err := writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-downDone:
		if err == nil || !strings.Contains(err.Error(), "perceptual-v1 avatar versions exist") {
			t.Fatalf("down after racing v1 writer = %v", err)
		}
	case <-ctx.Done():
		t.Fatal("schema-54 down did not finish after renderer writer committed")
	}
	assertMigrationTestVersion(t, dsn, 54)
	assertMigrationTestColumn(t, st, "agent_avatar_versions", "renderer_profile", true)
}

func TestAvatarMigrationsBackfillStateAndAddStyleRolloutsPostgres(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	st, dsn := newMigrationTestStore(t, baseDSN)
	migrationTestUpTo(t, dsn, 41)
	insertMigrationTestMemoryPrincipals(t, st)
	migrationTestUpTo(t, dsn, 50)
	assertMigrationTestVersion(t, dsn, 50)
	ctx := context.Background()
	var styles, selections, profiles int
	if err := st.pool.QueryRow(ctx, `SELECT COUNT(*) FROM avatar_style_pack_versions`).Scan(&styles); err != nil {
		t.Fatal(err)
	}
	if err := st.pool.QueryRow(ctx, `SELECT COUNT(*) FROM realm_avatar_styles`).Scan(&selections); err != nil {
		t.Fatal(err)
	}
	if err := st.pool.QueryRow(ctx, `SELECT COUNT(*) FROM agent_avatar_profiles`).Scan(&profiles); err != nil {
		t.Fatal(err)
	}
	if styles != 1 || selections != 1 || profiles != 3 {
		t.Fatalf("avatar backfill counts = styles:%d selections:%d profiles:%d", styles, selections, profiles)
	}
	if err := migrationTestDown(t, dsn, false); err != nil {
		t.Fatal(err)
	}
	assertMigrationTestVersion(t, dsn, 41)
	assertMigrationTestTable(t, st, "agent_avatar_profiles", false)
	assertMigrationTestTable(t, st, "avatar_style_packs", false)
	migrationTestUpTo(t, dsn, 50)
	assertMigrationTestVersion(t, dsn, 50)
	if err := st.pool.QueryRow(ctx, `SELECT COUNT(*) FROM agent_avatar_profiles`).Scan(&profiles); err != nil {
		t.Fatal(err)
	}
	if profiles != 3 {
		t.Fatalf("avatar profiles after re-upgrade = %d, want 3", profiles)
	}
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	assertMigrationTestVersion(t, dsn, int64(SchemaVersion()))
	assertMigrationTestTable(t, st, "avatar_style_rollout_jobs", true)
	assertMigrationTestColumn(t, st, "agent_avatar_profiles", "style_revision", true)
	assertMigrationTestIndex(t, st, "agent_avatar_profiles", "agent_avatar_profiles_by_style_revision", true)
	assertMigrationTestColumn(t, st, "accounts", "plan_policies", true)
	assertMigrationTestTable(t, st, "transcript_retention_worker_lanes", true)
	assertMigrationTestIndex(t, st, "accounts",
		"accounts_transcript_retention_worker_lane_idx", true)
	view, err := st.GetAvatar(ctx, Principal{Kind: PrincipalAgent,
		ID: "agent_memory_owner", AccountID: "acc_memory_trigger",
		RealmID: "realm_memory_trigger", AgentName: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if view.Profile.Status != avatardomain.StatusGenerationDue ||
		view.Profile.AutonomyPolicy != avatardomain.AutonomyAgentSelfManaged ||
		view.Profile.RetainedPayloadCountLimit != AvatarDefaultRetainedPayloadCountLimit ||
		view.Profile.RetainedPayloadByteLimit != AvatarDefaultRetainedPayloadByteLimit ||
		view.Active == nil || view.Active.Version != 0 ||
		view.Active.LockedLayersSHA256 == "" {
		t.Fatalf("backfilled avatar = %#v", view)
	}
	if err := migrationTestDown(t, dsn, false); err != nil {
		t.Fatal(err)
	}
	assertMigrationTestVersion(t, dsn, 65)
	assertMigrationTestTable(t, st, "transcript_retention_worker_lanes", false)
	assertMigrationTestIndex(t, st, "accounts",
		"accounts_transcript_retention_worker_lane_idx", false)
	assertMigrationTestIndex(t, st, "memory_curation_run_inputs",
		"memory_curation_run_inputs_by_transcript_cursor", true)
	if err := migrationTestDown(t, dsn, false); err != nil {
		t.Fatal(err)
	}
	assertMigrationTestVersion(t, dsn, 64)
	assertMigrationTestColumn(t, st, "accounts", "plan_policies", true)
	assertMigrationTestColumn(t, st, "memory_curation_run_inputs", "transcript_pruned_at", true)
	assertMigrationTestIndex(t, st, "memory_curation_run_inputs",
		"memory_curation_run_inputs_by_transcript_cursor", false)
	if err := migrationTestDown(t, dsn, false); err != nil {
		t.Fatal(err)
	}
	assertMigrationTestVersion(t, dsn, 63)
	assertMigrationTestColumn(t, st, "accounts", "plan_policies", true)
	if err := migrationTestDown(t, dsn, false); err != nil {
		t.Fatal(err)
	}
	assertMigrationTestVersion(t, dsn, 62)
	assertMigrationTestColumn(t, st, "accounts", "plan_policies", true)
	if err := migrationTestDown(t, dsn, false); err != nil {
		t.Fatal(err)
	}
	assertMigrationTestVersion(t, dsn, 61)
	assertMigrationTestColumn(t, st, "accounts", "plan_policies", false)
	assertMigrationTestColumn(t, st, "memory_curation_run_inputs", "transcript_pruned_at", false)
	assertMigrationTestTable(t, st, "transcript_retention_sweep_state", false)
	assertMigrationTestTable(t, st, "transcript_retention_account_scan_state", false)
	assertMigrationTestTable(t, st, "agent_email_retry_canary_arms", true)
	if err := migrationTestDown(t, dsn, false); err != nil {
		t.Fatal(err)
	}
	assertMigrationTestVersion(t, dsn, 60)
	assertMigrationTestTable(t, st, "agent_email_retry_canary_arms", false)
	assertMigrationTestTable(t, st, "agent_email_realm_receive_controls", true)
	assertMigrationTestTable(t, st, "agent_email_addresses", true)
	if err := migrationTestDown(t, dsn, false); err != nil {
		t.Fatal(err)
	}
	assertMigrationTestVersion(t, dsn, 59)
	assertMigrationTestTable(t, st, "agent_email_realm_receive_controls", false)
	assertMigrationTestTable(t, st, "agent_email_addresses", true)
	if err := migrationTestDown(t, dsn, false); err != nil {
		t.Fatal(err)
	}
	assertMigrationTestVersion(t, dsn, 58)
	assertMigrationTestTable(t, st, "agent_email_addresses", false)
	assertMigrationTestColumn(t, st, "memory_curation_run_inputs", "coverage_counts", true)
	assertMigrationTestTable(t, st, "agent_dashboard_preferences", true)
	if err := migrationTestDown(t, dsn, false); err != nil {
		t.Fatal(err)
	}
	assertMigrationTestVersion(t, dsn, 57)
	assertMigrationTestColumn(t, st, "memory_curation_run_inputs", "coverage_counts", false)
	assertMigrationTestTable(t, st, "agent_dashboard_preferences", true)
	if err := migrationTestDown(t, dsn, false); err != nil {
		t.Fatal(err)
	}
	assertMigrationTestVersion(t, dsn, 56)
	assertMigrationTestTable(t, st, "agent_dashboard_preferences", false)
	assertMigrationTestTable(t, st, "secrets", true)
	if err := migrationTestDown(t, dsn, false); err != nil {
		t.Fatal(err)
	}
	assertMigrationTestVersion(t, dsn, 55)
	assertMigrationTestColumn(t, st, "agent_avatar_versions", "renderer_profile", true)
	assertMigrationTestIndex(t, st, "agent_avatar_profiles", "agent_avatar_profiles_by_style_revision", true)
	assertMigrationTestTable(t, st, "avatar_style_rollout_jobs", true)
	if err := migrationTestDown(t, dsn, false); err != nil {
		t.Fatal(err)
	}
	assertMigrationTestVersion(t, dsn, 54)
	assertMigrationTestColumn(t, st, "agent_avatar_versions", "renderer_profile", true)
	assertMigrationTestIndex(t, st, "agent_avatar_profiles", "agent_avatar_profiles_by_style_revision", true)
	assertMigrationTestTable(t, st, "avatar_style_rollout_jobs", true)
	if err := migrationTestDown(t, dsn, false); err != nil {
		t.Fatal(err)
	}
	assertMigrationTestVersion(t, dsn, 53)
	assertMigrationTestColumn(t, st, "agent_avatar_versions", "renderer_profile", false)
	assertMigrationTestIndex(t, st, "agent_avatar_profiles", "agent_avatar_profiles_by_style_revision", true)
	assertMigrationTestTable(t, st, "avatar_style_rollout_jobs", true)
	if err := migrationTestDown(t, dsn, false); err != nil {
		t.Fatal(err)
	}
	assertMigrationTestVersion(t, dsn, int64(avatarStyleRolloutArchiveSchema))
	assertMigrationTestIndex(t, st, "agent_avatar_profiles", "agent_avatar_profiles_by_style_revision", false)
	assertMigrationTestTable(t, st, "avatar_style_rollout_jobs", true)
	if err := migrationTestDown(t, dsn, false); err != nil {
		t.Fatal(err)
	}
	assertMigrationTestVersion(t, dsn, int64(avatarStyleRolloutArchiveSchema-1))
	assertMigrationTestTable(t, st, "avatar_style_rollout_jobs", false)
	assertMigrationTestColumn(t, st, "agent_avatar_profiles", "style_revision", false)
	assertMigrationTestTable(t, st, "agent_avatar_profiles", true)
	if err := migrationTestDown(t, dsn, false); err != nil {
		t.Fatal(err)
	}
	assertMigrationTestVersion(t, dsn, 50)
	assertMigrationTestColumn(t, st, "agent_avatar_profiles", "retained_payload_count_limit", false)
	assertMigrationTestColumn(t, st, "agent_avatar_versions", "payload_state", false)
	if err := migrationTestDown(t, dsn, false); err != nil {
		t.Fatal(err)
	}
	assertMigrationTestVersion(t, dsn, 41)
	assertMigrationTestTable(t, st, "agent_avatar_profiles", false)
	assertMigrationTestTable(t, st, "avatar_style_packs", false)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	assertMigrationTestVersion(t, dsn, int64(SchemaVersion()))
	if err := st.pool.QueryRow(ctx, `SELECT COUNT(*) FROM agent_avatar_profiles`).Scan(&profiles); err != nil {
		t.Fatal(err)
	}
	if profiles != 3 {
		t.Fatalf("avatar profiles after re-upgrade = %d, want 3", profiles)
	}
}

func TestAvatarStyleRevisionConstraintUsesWriterCompatibleValidationPostgres(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	st, dsn := newMigrationTestStore(t, baseDSN)
	migrationTestUpTo(t, dsn, 41)
	insertMigrationTestMemoryPrincipals(t, st)
	migrationTestUpTo(t, dsn, int64(avatarStyleRolloutArchiveSchema-1))
	const extraProfiles = 20000
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO agents (id,realm_id,name)
		SELECT 'agent_style_constraint_' || lpad(g::text,5,'0'),
		       'realm_memory_trigger','constraint-' || lpad(g::text,5,'0')
		  FROM generate_series(1,$1) g`, extraProfiles); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO agent_avatar_profiles
		       (account_id,realm_id,agent_id,style_pack_id,style_pack_version,fallback_seed)
		SELECT 'acc_memory_trigger','realm_memory_trigger',a.id,
		       ras.style_pack_id,ras.style_pack_version,a.id
		  FROM agents a
		  JOIN realm_avatar_styles ras
		    ON ras.account_id='acc_memory_trigger' AND ras.realm_id='realm_memory_trigger'
		 WHERE a.id LIKE 'agent_style_constraint_%'`); err != nil {
		t.Fatal(err)
	}
	migrationTestUpTo(t, dsn, int64(avatarStyleRolloutArchiveSchema))
	var validated bool
	if err := st.pool.QueryRow(ctx, `
		SELECT convalidated FROM pg_constraint
		 WHERE conrelid='agent_avatar_profiles'::regclass
		   AND conname='agent_avatar_profiles_style_revision_positive'`).Scan(&validated); err != nil {
		t.Fatal(err)
	}
	if validated {
		t.Fatal("base rollout migration scanned and validated the large profile constraint under its metadata lock")
	}

	writer, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Exec(ctx, `LOCK TABLE agent_avatar_profiles IN ROW EXCLUSIVE MODE`); err != nil {
		_ = writer.Rollback(ctx)
		t.Fatal(err)
	}
	validationDone := make(chan error, 1)
	go func() {
		_, err := st.pool.Exec(ctx, `
			ALTER TABLE agent_avatar_profiles
			VALIDATE CONSTRAINT agent_avatar_profiles_style_revision_positive`)
		validationDone <- err
	}()
	select {
	case err := <-validationDone:
		if err != nil {
			_ = writer.Rollback(ctx)
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		_ = writer.Rollback(ctx)
		t.Fatal("constraint validation blocked behind a writer-compatible ROW EXCLUSIVE table lock")
	}
	if err := writer.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.pool.QueryRow(ctx, `
		SELECT convalidated FROM pg_constraint
		 WHERE conrelid='agent_avatar_profiles'::regclass
		   AND conname='agent_avatar_profiles_style_revision_positive'`).Scan(&validated); err != nil || !validated {
		t.Fatalf("validated constraint = %t / %v", validated, err)
	}
	migrationTestUpTo(t, dsn, int64(SchemaVersion()))
	assertMigrationTestIndex(t, st, "agent_avatar_profiles", "agent_avatar_profiles_by_style_revision", true)
}

func TestAvatarStyleRolloutConcurrentIndexMigrationIsRetrySafePostgres(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()

	t.Run("up after index build before version record", func(t *testing.T) {
		st, dsn := newMigrationTestStore(t, baseDSN)
		migrationTestUpTo(t, dsn, 54)
		if err := migrationTestDown(t, dsn, false); err != nil {
			t.Fatal(err)
		}
		if err := migrationTestDown(t, dsn, false); err != nil {
			t.Fatal(err)
		}
		if _, err := st.pool.Exec(ctx, `
			CREATE INDEX agent_avatar_profiles_by_style_revision
			    ON agent_avatar_profiles
			       (account_id,realm_id,(COALESCE(style_revision,0)),agent_id)`); err != nil {
			t.Fatal(err)
		}
		if err := st.Migrate(); err != nil {
			t.Fatalf("retry migration with pre-existing named index: %v", err)
		}
		var readyAndValid bool
		if err := st.pool.QueryRow(ctx, `
			SELECT i.indisready AND i.indisvalid
			  FROM pg_index i
			  JOIN pg_class c ON c.oid=i.indexrelid
			 WHERE c.relname='agent_avatar_profiles_by_style_revision'`).Scan(&readyAndValid); err != nil || !readyAndValid {
			t.Fatalf("retried index ready/valid = %t / %v", readyAndValid, err)
		}
		assertMigrationTestVersion(t, dsn, int64(SchemaVersion()))
	})

	t.Run("down after index already absent", func(t *testing.T) {
		st, dsn := newMigrationTestStore(t, baseDSN)
		migrationTestUpTo(t, dsn, 54)
		if _, err := st.pool.Exec(ctx, `DROP INDEX CONCURRENTLY agent_avatar_profiles_by_style_revision`); err != nil {
			t.Fatal(err)
		}
		if err := migrationTestDown(t, dsn, false); err != nil {
			t.Fatalf("retry down with already-absent index: %v", err)
		}
		if err := migrationTestDown(t, dsn, false); err != nil {
			t.Fatalf("retry down with already-absent index: %v", err)
		}
		assertMigrationTestVersion(t, dsn, int64(avatarStyleRolloutArchiveSchema))
	})
}

func TestAvatarStyleRolloutDownMigrationFailsClosedPostgres(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()

	t.Run("open and mismatched terminal jobs refuse; aligned completed permits", func(t *testing.T) {
		st, dsn := newMigrationTestStore(t, baseDSN)
		migrationTestUpTo(t, dsn, 54)
		provisioned, operator := provisionActiveRolloutAccountForTest(ctx, t, st, "down-open")
		realm := createRolloutRealmWithAgentsForTest(ctx, t, st, provisioned.AccountID, "down-open", 1)
		publishAvatarStyleForTest(ctx, t, st, operator, realm.ID, 1, 2, "down-open-v2")
		if err := migrationTestDown(t, dsn, false); err != nil { // remove renderer provenance
			t.Fatal(err)
		}
		if err := migrationTestDown(t, dsn, false); err != nil { // remove the concurrent lookup index
			t.Fatal(err)
		}
		err := migrationTestDown(t, dsn, true)
		if err == nil || !strings.Contains(err.Error(), "pending or running") {
			t.Fatalf("open rollout down error = %v", err)
		}
		assertMigrationTestVersion(t, dsn, int64(avatarStyleRolloutArchiveSchema))
		if _, err := st.pool.Exec(ctx, `
			WITH stamp AS MATERIALIZED (SELECT statement_timestamp() AS at)
			UPDATE avatar_style_rollout_jobs
			   SET status='completed',target_profile_count=0,batch_count=1,
			       started_at=stamp.at,completed_at=stamp.at,updated_at=stamp.at
			  FROM stamp WHERE account_id=$1 AND realm_id=$2`,
			provisioned.AccountID, realm.ID); err != nil {
			t.Fatal(err)
		}
		err = migrationTestDown(t, dsn, true)
		if err == nil || !strings.Contains(err.Error(), "live profile differs") {
			t.Fatalf("forged terminal mismatch down error = %v", err)
		}
		if _, err := st.pool.Exec(ctx, `
			UPDATE agent_avatar_profiles p
			   SET style_pack_id=ras.style_pack_id,style_pack_version=ras.style_pack_version
			  FROM realm_avatar_styles ras
			 WHERE p.account_id=ras.account_id AND p.realm_id=ras.realm_id
			   AND p.account_id=$1 AND p.realm_id=$2`, provisioned.AccountID, realm.ID); err != nil {
			t.Fatal(err)
		}
		if err := migrationTestDown(t, dsn, false); err != nil {
			t.Fatal(err)
		}
		assertMigrationTestVersion(t, dsn, int64(avatarStyleRolloutArchiveSchema-1))
		assertMigrationTestTable(t, st, "avatar_style_rollout_jobs", false)
	})

	t.Run("aligned superseded history permits", func(t *testing.T) {
		st, dsn := newMigrationTestStore(t, baseDSN)
		migrationTestUpTo(t, dsn, 54)
		provisioned, operator := provisionActiveRolloutAccountForTest(ctx, t, st, "down-superseded")
		realm := createRolloutRealmWithAgentsForTest(ctx, t, st, provisioned.AccountID, "down-superseded", 0)
		publishAvatarStyleForTest(ctx, t, st, operator, realm.ID, 1, 2, "down-superseded-v2")
		if _, err := st.pool.Exec(ctx, `
			WITH stamp AS MATERIALIZED (SELECT statement_timestamp() AS at)
			UPDATE avatar_style_rollout_jobs
			   SET status='superseded',target_profile_count=0,
			       superseded_at=stamp.at,updated_at=stamp.at
			  FROM stamp WHERE account_id=$1 AND realm_id=$2`,
			provisioned.AccountID, realm.ID); err != nil {
			t.Fatal(err)
		}
		if err := migrationTestDown(t, dsn, false); err != nil { // remove renderer provenance
			t.Fatal(err)
		}
		if err := migrationTestDown(t, dsn, false); err != nil { // remove the concurrent lookup index
			t.Fatal(err)
		}
		if err := migrationTestDown(t, dsn, false); err != nil { // remove the rollout job and profile fence
			t.Fatal(err)
		}
		assertMigrationTestVersion(t, dsn, int64(avatarStyleRolloutArchiveSchema-1))
	})
}

func TestAvatarStyleRolloutDownMigrationLocksBeforeSafetyCheckPostgres(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, dsn := newMigrationTestStore(t, baseDSN)
	migrationTestUpTo(t, dsn, 54)
	provisioned, _ := provisionActiveRolloutAccountForTest(ctx, t, st, "down-race")
	realm := createRolloutRealmWithAgentsForTest(ctx, t, st, provisioned.AccountID, "down-race", 0)
	if err := migrationTestDown(t, dsn, false); err != nil { // remove renderer provenance
		t.Fatal(err)
	}
	if err := migrationTestDown(t, dsn, false); err != nil { // remove the concurrent lookup index
		t.Fatal(err)
	}
	writer, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Exec(ctx, `
		INSERT INTO avatar_style_rollout_jobs
		       (account_id,realm_id,style_revision,style_pack_id,style_pack_version)
		SELECT account_id,realm_id,revision,style_pack_id,style_pack_version
		  FROM realm_avatar_styles WHERE account_id=$1 AND realm_id=$2`,
		provisioned.AccountID, realm.ID); err != nil {
		_ = writer.Rollback(ctx)
		t.Fatal(err)
	}
	db := migrationTestSQLDB(t, dsn)
	defer func() { _ = db.Close() }()
	downDone := make(chan error, 1)
	go func() { downDone <- goose.Down(db, "migrations") }()
	waiting := false
	for attempts := 0; attempts < 100; attempts++ {
		if err := st.pool.QueryRow(ctx, `
			SELECT EXISTS (
			  SELECT 1 FROM pg_locks
			   WHERE relation='avatar_style_rollout_jobs'::regclass
			     AND mode='AccessExclusiveLock' AND NOT granted
			)`).Scan(&waiting); err != nil {
			_ = writer.Rollback(ctx)
			t.Fatal(err)
		}
		if waiting {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !waiting {
		_ = writer.Rollback(ctx)
		t.Fatal("down migration never waited for the transaction-held rollout table lock")
	}
	if err := writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-downDone:
		if err == nil || !strings.Contains(err.Error(), "pending or running") {
			t.Fatalf("down after racing insert = %v, want open-job refusal", err)
		}
	case <-ctx.Done():
		t.Fatal("down migration did not finish after writer committed")
	}
	assertMigrationTestVersion(t, dsn, int64(avatarStyleRolloutArchiveSchema))
}

func TestAvatarLifecycleIsolationIdempotencyAndStylePropagationPostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}

	provisioned, err := st.ProvisionAccount(ctx, "avatar-store@witwave.ai", "avatar store", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = deleteAccountForIntegrationTest(context.Background(), st, provisioned.AccountID) }()
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate = %t / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "default")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "portrait-test")
	if err != nil {
		t.Fatal(err)
	}
	agentPrincipal := Principal{Kind: PrincipalAgent, ID: agent.ID,
		AccountID: provisioned.AccountID, RealmID: realm.ID, AgentName: agent.Name,
		AccountStatus: "active"}
	operator := Principal{Kind: PrincipalOperator, ID: provisioned.OperatorID,
		AccountID: provisioned.AccountID, AccountStatus: "active"}

	initial, err := st.GetAvatar(ctx, agentPrincipal)
	if err != nil {
		t.Fatal(err)
	}
	if initial.Profile.Status != avatardomain.StatusGenerationDue ||
		initial.Profile.AutonomyPolicy != avatardomain.AutonomyAgentSelfManaged ||
		initial.Profile.ProfileRevision != 1 || initial.Profile.FallbackSeed != agent.ID ||
		initial.Active == nil || initial.Active.ProposedBy.Kind != ActorSystem ||
		initial.Active.Version != 0 || !initial.Active.IsActive ||
		initial.Profile.ActiveVersion != 0 {
		t.Fatalf("initial avatar = %#v", initial)
	}
	again, err := st.GetAvatar(ctx, agentPrincipal)
	if err != nil {
		t.Fatal(err)
	}
	if again.Active == nil || again.Active.SVG != initial.Active.SVG ||
		again.Active.SVGSHA256 != initial.Active.SVGSHA256 || again.Active.ID != initial.Active.ID {
		t.Fatal("deterministic placeholder changed between reads")
	}
	checkpoint, err := st.GetSelfAvatarCheckpoint(ctx, agentPrincipal)
	if err != nil {
		t.Fatal(err)
	}
	if !checkpoint.Pending || checkpoint.Reason != "initial_avatar" ||
		checkpoint.ProfileRevision != 1 || checkpoint.StylePackVersion != 1 {
		t.Fatalf("initial avatar checkpoint = %#v", checkpoint)
	}
	style, err := st.GetRealmAvatarStyle(ctx, agentPrincipal, "")
	if err != nil {
		t.Fatal(err)
	}
	if style.StyleRevision != 1 || style.StylePack.ID != avatardomain.DefaultStylePackID ||
		style.StylePack.Version != 1 || len(style.StylePack.References) < 3 {
		t.Fatalf("initial style = %#v", style)
	}

	restricted := agentPrincipal
	restricted.AccessProfile = AccessProfileCuratorPreview
	if _, err := st.GetAvatar(ctx, restricted); !errors.Is(err, ErrAvatarForbidden) {
		t.Fatalf("restricted avatar read = %v", err)
	}
	if _, err := st.ProposeAvatar(ctx, restricted, ProposeAvatarInput{}); !errors.Is(err, ErrAvatarForbidden) {
		t.Fatalf("restricted avatar proposal = %v", err)
	}

	proposal := ProposeAvatarInput{
		ExpectedProfileRevision: 1,
		StylePackID:             style.StylePack.ID,
		StylePackVersion:        style.StylePack.Version,
		SubjectForm:             avatardomain.SubjectHuman,
		Description:             "A calm human teammate in the shared flat portrait style.",
		VisualSpec:              []byte(`{"identity":{"expression":"calm"}}`),
		SVG:                     style.StylePack.References[0].SVG,
		Provenance: AvatarClientProvenance{Runtime: "cursor", Model: "GPT-5.6 Sol",
			Recipe: "avatar-initial", RecipeVersion: "1"},
		IdempotencyKey: "avatar-proposal-1",
	}
	proposed, err := st.ProposeAvatar(ctx, agentPrincipal, proposal)
	if err != nil {
		t.Fatal(err)
	}
	if proposed.Avatar.Profile.ProfileRevision != 2 ||
		proposed.Avatar.Profile.Status != avatardomain.StatusProposed ||
		proposed.Avatar.Proposed == nil || proposed.Avatar.Proposed.Version != 1 ||
		proposed.Avatar.Proposed.Provenance.Model != "GPT-5.6 Sol" ||
		!proposed.Avatar.Proposed.IsProposed || proposed.Avatar.Proposed.WasActivated ||
		proposed.Avatar.Active == nil || proposed.Avatar.Active.Version != 0 {
		t.Fatalf("proposed avatar = %#v", proposed.Avatar)
	}
	replayedProposal, err := st.ProposeAvatar(ctx, agentPrincipal, proposal)
	if err != nil || !replayedProposal.Receipt.Replayed || replayedProposal.Receipt.ResultVersion != 1 {
		t.Fatalf("proposal replay = %#v / %v", replayedProposal.Receipt, err)
	}
	conflictingProposal := proposal
	conflictingProposal.Description = "Different semantics under the same retry key."
	if _, err := st.ProposeAvatar(ctx, agentPrincipal, conflictingProposal); !errors.Is(err, ErrAvatarIdempotencyConflict) {
		t.Fatalf("proposal idempotency conflict = %v", err)
	}
	staleProposal := proposal
	staleProposal.IdempotencyKey = "avatar-proposal-stale"
	if _, err := st.ProposeAvatar(ctx, agentPrincipal, staleProposal); !errors.Is(err, ErrAvatarConflict) {
		t.Fatalf("stale proposal = %v", err)
	}

	activation := ActivateAvatarInput{Version: 1, ExpectedProfileRevision: 2,
		IdempotencyKey: "avatar-activate-1"}
	active, err := st.ActivateAvatar(ctx, agentPrincipal, activation)
	if err != nil {
		t.Fatal(err)
	}
	if active.Avatar.Profile.ProfileRevision != 3 ||
		active.Avatar.Profile.ActiveVersion != 1 || active.Avatar.Proposed != nil ||
		active.Avatar.Active == nil || active.Avatar.Active.Version != 1 ||
		!active.Avatar.Active.IsActive || !active.Avatar.Active.WasActivated ||
		active.Avatar.Active.RollbackEligible || active.Avatar.Active.LastActivatedAt == nil {
		t.Fatalf("active avatar = %#v", active.Avatar)
	}
	checkpoint, err = st.GetSelfAvatarCheckpoint(ctx, agentPrincipal)
	if err != nil || checkpoint.Pending {
		t.Fatalf("active avatar checkpoint = %#v / %v", checkpoint, err)
	}
	replayedActivation, err := st.ActivateAvatar(ctx, agentPrincipal, activation)
	if err != nil || !replayedActivation.Receipt.Replayed {
		t.Fatalf("activation replay = %#v / %v", replayedActivation.Receipt, err)
	}
	// Exact retry keys preserve their original value-free receipt while reads
	// return the resource's current projection. A replay never rolls mutable
	// profile state back to the historical post-mutation view.
	replayedProposal, err = st.ProposeAvatar(ctx, agentPrincipal, proposal)
	if err != nil || !replayedProposal.Receipt.Replayed ||
		replayedProposal.Receipt.ResultRevision != 2 ||
		replayedProposal.Avatar.Profile.ProfileRevision != 3 ||
		replayedProposal.Avatar.Profile.Status != avatardomain.StatusActive {
		t.Fatalf("proposal replay after activation = %#v / %#v / %v",
			replayedProposal.Receipt, replayedProposal.Avatar.Profile, err)
	}

	policyResult, err := st.SetAvatarPolicy(ctx, operator, agent.ID, UpdateAvatarPolicyInput{
		Policy: avatardomain.AutonomyAgentProposes, ExpectedProfileRevision: 3,
		IdempotencyKey: "avatar-policy-proposes",
	})
	if err != nil {
		t.Fatal(err)
	}
	if policyResult.Avatar.Profile.ProfileRevision != 4 ||
		policyResult.Avatar.Profile.AutonomyPolicy != avatardomain.AutonomyAgentProposes {
		t.Fatalf("policy result = %#v", policyResult.Avatar.Profile)
	}

	evolution := proposal
	evolution.ExpectedProfileRevision = 4
	evolution.ParentVersion = 1
	evolution.SubjectForm = avatardomain.SubjectAnimal
	evolution.Description = "A fox teammate evolved from the active portrait while retaining the team grammar."
	evolution.SVG = style.StylePack.References[1].SVG
	evolution.IdempotencyKey = "avatar-evolution-2"
	// Operators may deliberately override same-style locked-layer and subject
	// continuity; the restriction applies only to self-authored evolution.
	evolvedProposal, err := st.ProposeAgentAvatar(ctx, operator, agent.ID, evolution)
	if err != nil {
		t.Fatal(err)
	}
	if evolvedProposal.Avatar.Proposed == nil || evolvedProposal.Avatar.Proposed.Version != 2 ||
		evolvedProposal.Avatar.Proposed.ParentVersion == nil || *evolvedProposal.Avatar.Proposed.ParentVersion != 1 {
		t.Fatalf("evolution proposal = %#v", evolvedProposal.Avatar.Proposed)
	}
	if _, err := st.ActivateAvatar(ctx, agentPrincipal, ActivateAvatarInput{Version: 2,
		ExpectedProfileRevision: 5, IdempotencyKey: "self-activation-denied"}); !errors.Is(err, ErrAvatarForbidden) {
		t.Fatalf("agent_proposes self activation = %v", err)
	}
	operatorActive, err := st.ActivateAgentAvatar(ctx, operator, agent.ID, ActivateAvatarInput{
		Version: 2, ExpectedProfileRevision: 5, IdempotencyKey: "operator-activate-2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if operatorActive.Avatar.Profile.ActiveVersion != 2 ||
		operatorActive.Avatar.Profile.SubjectForm != avatardomain.SubjectAnimal {
		t.Fatalf("operator activation = %#v", operatorActive.Avatar.Profile)
	}
	rolledBack, err := st.RollbackAgentAvatar(ctx, operator, agent.ID, RollbackAvatarInput{
		Version: 1, ExpectedProfileRevision: 6, IdempotencyKey: "operator-rollback-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rolledBack.Avatar.Profile.ActiveVersion != 1 ||
		rolledBack.Avatar.Profile.ProfileRevision != 7 {
		t.Fatalf("rollback = %#v", rolledBack.Avatar.Profile)
	}
	rolledForward, err := st.RollbackAgentAvatar(ctx, operator, agent.ID, RollbackAvatarInput{
		Version: 2, ExpectedProfileRevision: 7, IdempotencyKey: "operator-rollback-forward-2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rolledForward.Avatar.Profile.ActiveVersion != 2 ||
		rolledForward.Avatar.Profile.ProfileRevision != 8 {
		t.Fatalf("rollback to later historical activation = %#v", rolledForward.Avatar.Profile)
	}
	rolledBack, err = st.RollbackAgentAvatar(ctx, operator, agent.ID, RollbackAvatarInput{
		Version: 1, ExpectedProfileRevision: 8, IdempotencyKey: "operator-rollback-1-again",
	})
	if err != nil {
		t.Fatal(err)
	}
	history, err := st.GetAvatarHistory(ctx, agentPrincipal, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(history.Versions) != 2 {
		t.Fatalf("activation history versions = %d, want 2", len(history.Versions))
	}
	byVersion := map[int64]AvatarVersionSummary{}
	for _, version := range history.Versions {
		byVersion[version.Version] = version
	}
	versionOne, versionTwo := byVersion[1], byVersion[2]
	if !versionOne.IsActive || !versionOne.WasActivated || versionOne.RollbackEligible ||
		versionOne.LastActivatedAt == nil || versionOne.Rejected ||
		versionTwo.IsActive || !versionTwo.WasActivated || !versionTwo.RollbackEligible ||
		versionTwo.LastActivatedAt == nil || versionTwo.Rejected {
		t.Fatalf("projected lifecycle history = v1:%#v v2:%#v", versionOne, versionTwo)
	}
	firstHistoryPage, err := st.GetAvatarHistoryPage(ctx, agentPrincipal, AvatarHistoryOptions{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	secondHistoryPage, err := st.GetAvatarHistoryPage(ctx, agentPrincipal, AvatarHistoryOptions{
		Limit: 1, BeforeVersion: firstHistoryPage.NextBeforeVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(firstHistoryPage.Versions) != 1 || firstHistoryPage.Versions[0].Version != 2 || firstHistoryPage.NextBeforeVersion != 2 ||
		len(secondHistoryPage.Versions) != 1 || secondHistoryPage.Versions[0].Version != 1 || secondHistoryPage.NextBeforeVersion != 0 ||
		firstHistoryPage.Versions[0].Version == secondHistoryPage.Versions[0].Version {
		t.Fatalf("history pagination continuity = first:%#v second:%#v", firstHistoryPage, secondHistoryPage)
	}
	exactVersion, err := st.GetAvatarVersion(ctx, agentPrincipal, 2)
	if err != nil || exactVersion.Version != 2 || exactVersion.SVG == "" || len(exactVersion.VisualSpec) == 0 ||
		exactVersion.SVGSHA256 == "" || !exactVersion.WasActivated || !exactVersion.RollbackEligible {
		t.Fatalf("exact avatar version = %#v / %v", exactVersion, err)
	}
	if _, err := st.GetAvatarVersion(ctx, agentPrincipal, 0); !errors.Is(err, ErrAvatarInputInvalid) {
		t.Fatalf("version zero error = %v, want ErrAvatarInputInvalid", err)
	}
	if _, err := st.GetAvatarVersion(ctx, agentPrincipal, 999); !errors.Is(err, ErrAvatarVersionNotFound) {
		t.Fatalf("missing version error = %v, want ErrAvatarVersionNotFound", err)
	}

	selfManaged, err := st.SetAvatarPolicy(ctx, operator, agent.ID, UpdateAvatarPolicyInput{
		Policy: avatardomain.AutonomyAgentSelfManaged, ExpectedProfileRevision: 9,
		IdempotencyKey: "avatar-policy-self-managed",
	})
	if err != nil {
		t.Fatal(err)
	}

	staleStyleProposal := proposal
	staleStyleProposal.ExpectedProfileRevision = selfManaged.Avatar.Profile.ProfileRevision
	staleStyleProposal.ParentVersion = 1
	staleStyleProposal.SubjectForm = avatardomain.SubjectAnimal
	staleStyleProposal.Description = "An animal evolution awaiting review under the first team style version."
	staleStyleProposal.SVG = style.StylePack.References[1].SVG
	staleStyleProposal.IdempotencyKey = "avatar-stale-style-proposal"
	staleStyleResult, err := st.ProposeAgentAvatar(ctx, operator, agent.ID, staleStyleProposal)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReportAvatarGenerationFailure(ctx, agentPrincipal,
		AvatarGenerationFailureInput{ExpectedProfileRevision: staleStyleResult.Avatar.Profile.ProfileRevision,
			ReasonCode: "renderer_unavailable", IdempotencyKey: "failure-with-proposal"}); !errors.Is(err, ErrAvatarConflict) {
		t.Fatalf("generation failure with pending proposal = %v", err)
	}

	styleV2 := avatardomain.BuiltInFlatVectorStylePack()
	styleV2.Version = 2
	styleV2.Description = "The second immutable version of the shared flat-vector team portrait grammar."
	styleUpdate, err := st.SetRealmAvatarStyle(ctx, operator, realm.ID,
		CreateAvatarStyleVersionInput{ExpectedStyleRevision: 1, StylePack: styleV2,
			IdempotencyKey: "realm-avatar-style-2"})
	if err != nil {
		t.Fatal(err)
	}
	if styleUpdate.Style.StyleRevision != 2 || styleUpdate.Style.StylePack.Version != 2 {
		t.Fatalf("style update = %#v", styleUpdate.Style)
	}
	drainAvatarStyleRolloutsForTest(ctx, t, st, 10)
	afterStyle, err := st.GetAvatar(ctx, agentPrincipal)
	if err != nil {
		t.Fatal(err)
	}
	if afterStyle.Profile.Style.Version != 2 ||
		afterStyle.Profile.Status != avatardomain.StatusEvolutionDue ||
		afterStyle.Profile.RetryAfter != nil || afterStyle.Profile.AttemptCount != 0 ||
		afterStyle.Profile.ProposedVersion != 0 || afterStyle.Proposed != nil ||
		afterStyle.Profile.SubjectForm != avatardomain.SubjectHuman ||
		afterStyle.Profile.ActiveVersion != 1 {
		t.Fatalf("profile after style update = %#v", afterStyle.Profile)
	}
	failure, err := st.ReportAvatarGenerationFailure(ctx, agentPrincipal,
		AvatarGenerationFailureInput{ExpectedProfileRevision: afterStyle.Profile.ProfileRevision,
			ReasonCode: "renderer_unavailable", IdempotencyKey: "avatar-failure-1"})
	if err != nil {
		t.Fatal(err)
	}
	if failure.Avatar.Profile.Status != avatardomain.StatusGenerationFailed ||
		failure.Avatar.Profile.AttemptCount != 1 || failure.Avatar.Profile.RetryAfter == nil ||
		!failure.Avatar.Profile.RetryAfter.After(time.Now().Add(30*time.Second)) {
		t.Fatalf("failure checkpoint = %#v", failure.Avatar.Profile)
	}
	checkpoint, err = st.GetSelfAvatarCheckpoint(ctx, agentPrincipal)
	if err != nil || checkpoint.Pending || checkpoint.RetryAfter == nil {
		t.Fatalf("future retry checkpoint = %#v / %v", checkpoint, err)
	}
	blockedRetryProposal := proposal
	blockedRetryProposal.ExpectedProfileRevision = failure.Avatar.Profile.ProfileRevision
	blockedRetryProposal.ParentVersion = failure.Avatar.Profile.ActiveVersion
	blockedRetryProposal.StylePackVersion = styleV2.Version
	blockedRetryProposal.Description = "A retry proposal that must wait for the server-stamped backoff."
	blockedRetryProposal.SVG = styleV2.References[0].SVG
	blockedRetryProposal.IdempotencyKey = "avatar-proposal-before-retry-due"
	if _, err := st.ProposeAvatar(ctx, agentPrincipal, blockedRetryProposal); !errors.Is(err, ErrAvatarConflict) {
		t.Fatalf("proposal before retry due = %v", err)
	}
	if _, err := st.ReportAvatarGenerationFailure(ctx, agentPrincipal,
		AvatarGenerationFailureInput{ExpectedProfileRevision: failure.Avatar.Profile.ProfileRevision,
			ReasonCode: "renderer_unavailable", IdempotencyKey: "failure-before-retry-due"}); !errors.Is(err, ErrAvatarConflict) {
		t.Fatalf("repeated failure before retry due = %v", err)
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE agent_avatar_profiles SET retry_after=clock_timestamp()-interval '1 second'
		 WHERE agent_id=$1`, agent.ID); err != nil {
		t.Fatal(err)
	}
	checkpoint, err = st.GetSelfAvatarCheckpoint(ctx, agentPrincipal)
	if err != nil || !checkpoint.Pending || checkpoint.Reason != "retry_due" {
		t.Fatalf("due retry checkpoint = %#v / %v", checkpoint, err)
	}
	retryEvolution := proposal
	retryEvolution.ExpectedProfileRevision = failure.Avatar.Profile.ProfileRevision
	retryEvolution.ParentVersion = failure.Avatar.Profile.ActiveVersion
	retryEvolution.StylePackVersion = styleV2.Version
	retryEvolution.Description = "A style-v2 evolution used to preserve the checkpoint after rejection."
	retryEvolution.SVG = styleV2.References[0].SVG
	retryEvolution.IdempotencyKey = "avatar-style-v2-retry-proposal"
	retryProposal, err := st.ProposeAvatar(ctx, agentPrincipal, retryEvolution)
	if err != nil {
		t.Fatal(err)
	}
	rejectedEvolution, err := st.RejectAgentAvatar(ctx, operator, agent.ID,
		RejectAvatarInput{Version: retryProposal.Avatar.Profile.ProposedVersion,
			ExpectedProfileRevision: retryProposal.Avatar.Profile.ProfileRevision,
			ReasonCode:              "operator_declined", IdempotencyKey: "reject-style-v2-evolution"})
	if err != nil {
		t.Fatal(err)
	}
	if rejectedEvolution.Avatar.Profile.Status != avatardomain.StatusEvolutionDue ||
		rejectedEvolution.Avatar.Profile.ActiveVersion != failure.Avatar.Profile.ActiveVersion ||
		rejectedEvolution.Avatar.Profile.Style.Version != styleV2.Version {
		t.Fatalf("rejected style evolution = %#v", rejectedEvolution.Avatar.Profile)
	}
	checkpoint, err = st.GetSelfAvatarCheckpoint(ctx, agentPrincipal)
	if err != nil || !checkpoint.Pending || checkpoint.Reason != "style_changed" {
		t.Fatalf("rejected style evolution checkpoint = %#v / %v", checkpoint, err)
	}
	acceptedEvolution := retryEvolution
	acceptedEvolution.ExpectedProfileRevision = rejectedEvolution.Avatar.Profile.ProfileRevision
	acceptedEvolution.IdempotencyKey = "avatar-style-v2-accepted-proposal"
	acceptedProposal, err := st.ProposeAvatar(ctx, agentPrincipal, acceptedEvolution)
	if err != nil {
		t.Fatal(err)
	}
	acceptedActive, err := st.ActivateAvatar(ctx, agentPrincipal, ActivateAvatarInput{
		Version:                 acceptedProposal.Avatar.Profile.ProposedVersion,
		ExpectedProfileRevision: acceptedProposal.Avatar.Profile.ProfileRevision,
		IdempotencyKey:          "avatar-style-v2-accepted-activation",
	})
	if err != nil {
		t.Fatal(err)
	}
	offStyleRollback, err := st.RollbackAgentAvatar(ctx, operator, agent.ID, RollbackAvatarInput{
		Version:                 1,
		ExpectedProfileRevision: acceptedActive.Avatar.Profile.ProfileRevision,
		IdempotencyKey:          "avatar-off-style-rollback",
	})
	if err != nil {
		t.Fatal(err)
	}
	if offStyleRollback.Avatar.Profile.Status != avatardomain.StatusEvolutionDue {
		t.Fatalf("off-style rollback profile = %#v", offStyleRollback.Avatar.Profile)
	}
	events, err := st.ListAccountEvents(ctx, provisioned.AccountID,
		provisioned.OperatorID, EventFilter{Verb: VerbAvatarRolledBack, Limit: 1})
	if err != nil || len(events.Events) != 1 {
		t.Fatalf("off-style rollback event = %#v / %v", events, err)
	}
	var rollbackMetadata map[string]string
	if err := json.Unmarshal(events.Events[0].Metadata, &rollbackMetadata); err != nil {
		t.Fatal(err)
	}
	if rollbackMetadata["status"] != string(avatardomain.StatusEvolutionDue) {
		t.Fatalf("off-style rollback event metadata = %#v", rollbackMetadata)
	}

	newAgent, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "style-v2-agent")
	if err != nil {
		t.Fatal(err)
	}
	newAgentView, err := st.GetAvatar(ctx, Principal{Kind: PrincipalAgent,
		ID: newAgent.ID, AccountID: provisioned.AccountID, RealmID: realm.ID,
		AgentName: newAgent.Name})
	if err != nil {
		t.Fatal(err)
	}
	if newAgentView.Profile.Style.Version != 2 {
		t.Fatalf("new agent inherited style version %d, want 2", newAgentView.Profile.Style.Version)
	}
	newAgentPrincipal := Principal{Kind: PrincipalAgent, ID: newAgent.ID,
		AccountID: provisioned.AccountID, RealmID: realm.ID, AgentName: newAgent.Name,
		AccountStatus: "active"}
	initialAnimal := proposal
	initialAnimal.ExpectedProfileRevision = 1
	initialAnimal.ParentVersion = 0
	initialAnimal.StylePackVersion = 2
	initialAnimal.SubjectForm = avatardomain.SubjectAnimal
	initialAnimal.Description = "An animal proposal used to verify rejection projection behavior."
	initialAnimal.SVG = styleV2.References[1].SVG
	initialAnimal.IdempotencyKey = "new-agent-animal-1"
	if _, err := st.ProposeAvatar(ctx, newAgentPrincipal, initialAnimal); err != nil {
		t.Fatal(err)
	}
	rejectedWithoutActive, err := st.RejectAgentAvatar(ctx, operator, newAgent.ID,
		RejectAvatarInput{Version: 1, ExpectedProfileRevision: 2,
			ReasonCode: "operator_declined", IdempotencyKey: "reject-new-agent-1"})
	if err != nil {
		t.Fatal(err)
	}
	if rejectedWithoutActive.Avatar.Profile.Status != avatardomain.StatusRejected ||
		rejectedWithoutActive.Avatar.Profile.SubjectForm != avatardomain.SubjectHuman ||
		rejectedWithoutActive.Avatar.Profile.ProposedVersion != 0 ||
		rejectedWithoutActive.Avatar.Proposed != nil {
		t.Fatalf("rejection without active avatar = %#v", rejectedWithoutActive.Avatar.Profile)
	}
	initialHuman := initialAnimal
	initialHuman.ExpectedProfileRevision = 3
	initialHuman.SubjectForm = avatardomain.SubjectHuman
	initialHuman.Description = "A human proposal that becomes the active projection."
	initialHuman.SVG = styleV2.References[0].SVG
	initialHuman.IdempotencyKey = "new-agent-human-2"
	initialHumanResult, err := st.ProposeAvatar(ctx, newAgentPrincipal, initialHuman)
	if err != nil {
		t.Fatal(err)
	}
	if initialHumanResult.Avatar.Proposed == nil || initialHumanResult.Avatar.Proposed.Version != 2 {
		t.Fatalf("second initial proposal = %#v", initialHumanResult.Avatar.Proposed)
	}
	if _, err := st.ActivateAvatar(ctx, newAgentPrincipal, ActivateAvatarInput{
		Version: 2, ExpectedProfileRevision: 4, IdempotencyKey: "activate-new-agent-2",
	}); err != nil {
		t.Fatal(err)
	}
	pendingAnimal := initialAnimal
	pendingAnimal.ExpectedProfileRevision = 5
	pendingAnimal.ParentVersion = 2
	pendingAnimal.IdempotencyKey = "new-agent-animal-3"
	if _, err := st.ProposeAgentAvatar(ctx, operator, newAgent.ID, pendingAnimal); err != nil {
		t.Fatal(err)
	}
	rejectedWithActive, err := st.RejectAgentAvatar(ctx, operator, newAgent.ID,
		RejectAvatarInput{Version: 3, ExpectedProfileRevision: 6,
			ReasonCode: "operator_declined", IdempotencyKey: "reject-new-agent-3"})
	if err != nil {
		t.Fatal(err)
	}
	if rejectedWithActive.Avatar.Profile.Status != avatardomain.StatusActive ||
		rejectedWithActive.Avatar.Profile.SubjectForm != avatardomain.SubjectHuman ||
		rejectedWithActive.Avatar.Profile.ActiveVersion != 2 ||
		rejectedWithActive.Avatar.Profile.ProposedVersion != 0 ||
		rejectedWithActive.Avatar.Proposed != nil {
		t.Fatalf("rejection with active avatar = %#v", rejectedWithActive.Avatar.Profile)
	}
	newAgentHistory, err := st.GetAvatarHistory(ctx, newAgentPrincipal, 10)
	if err != nil {
		t.Fatal(err)
	}
	newAgentByVersion := map[int64]AvatarVersionSummary{}
	for _, version := range newAgentHistory.Versions {
		newAgentByVersion[version.Version] = version
	}
	if rejected := newAgentByVersion[3]; !rejected.Rejected || rejected.RejectedAt == nil ||
		rejected.WasActivated || rejected.RollbackEligible || rejected.IsProposed {
		t.Fatalf("rejected lifecycle projection = %#v", rejected)
	}

	retryAgent, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "rejected-retry-agent")
	if err != nil {
		t.Fatal(err)
	}
	retryPrincipal := Principal{Kind: PrincipalAgent, ID: retryAgent.ID,
		AccountID: provisioned.AccountID, RealmID: realm.ID, AgentName: retryAgent.Name,
		AccountStatus: "active"}
	retryInitial := initialAnimal
	retryInitial.IdempotencyKey = "rejected-retry-proposal"
	retryInitialResult, err := st.ProposeAvatar(ctx, retryPrincipal, retryInitial)
	if err != nil {
		t.Fatal(err)
	}
	retryRejected, err := st.RejectAgentAvatar(ctx, operator, retryAgent.ID,
		RejectAvatarInput{Version: retryInitialResult.Avatar.Profile.ProposedVersion,
			ExpectedProfileRevision: retryInitialResult.Avatar.Profile.ProfileRevision,
			ReasonCode:              "operator_declined", IdempotencyKey: "rejected-retry-rejection"})
	if err != nil {
		t.Fatal(err)
	}
	if retryRejected.Avatar.Profile.Status != avatardomain.StatusRejected {
		t.Fatalf("retry fixture rejection = %#v", retryRejected.Avatar.Profile)
	}
	retryFailure, err := st.ReportAvatarGenerationFailure(ctx, retryPrincipal,
		AvatarGenerationFailureInput{ExpectedProfileRevision: retryRejected.Avatar.Profile.ProfileRevision,
			ReasonCode: "renderer_unavailable", IdempotencyKey: "rejected-retry-failure"})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err = st.GetSelfAvatarCheckpoint(ctx, retryPrincipal)
	if err != nil || checkpoint.Pending || retryFailure.Avatar.Profile.RetryAfter == nil {
		t.Fatalf("rejected failure backoff = %#v / %#v / %v", retryFailure.Avatar.Profile, checkpoint, err)
	}

	other, err := st.ProvisionAccount(ctx, "avatar-other@witwave.ai", "avatar other", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = deleteAccountForIntegrationTest(context.Background(), st, other.AccountID) }()
	if activated, err := st.ActivateAccount(ctx, other.AccountID); err != nil || !activated {
		t.Fatalf("activate other = %t / %v", activated, err)
	}
	otherOperator := Principal{Kind: PrincipalOperator, ID: other.OperatorID,
		AccountID: other.AccountID, AccountStatus: "active"}
	if _, err := st.GetAgentAvatar(ctx, otherOperator, agent.ID); !errors.Is(err, ErrAvatarNotFound) {
		t.Fatalf("cross-account operator avatar lookup = %v", err)
	}

	if err := st.DeleteAgent(ctx, provisioned.AccountID, realm.ID, agent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetAvatar(ctx, agentPrincipal); !errors.Is(err, ErrAvatarNotFound) {
		t.Fatalf("deleted avatar lookup = %v", err)
	}
}
