package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	avatardomain "github.com/witwave-ai/witself/internal/avatar"
	archiveexport "github.com/witwave-ai/witself/internal/export"
)

func TestAvatarArchiveCurrentSchemaRoundTripPostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}

	provisioned, err := st.ProvisionAccount(ctx,
		fmt.Sprintf("avatar-archive-%d@witwave.ai", time.Now().UnixNano()),
		"avatar archive round trip", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = deleteAccountForIntegrationTest(context.Background(), st, provisioned.AccountID) }()
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate account = %t / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "default")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "archive-portrait")
	if err != nil {
		t.Fatal(err)
	}
	p := Principal{Kind: PrincipalAgent, ID: agent.ID, AccountID: provisioned.AccountID,
		RealmID: realm.ID, AgentName: agent.Name, AccountStatus: "active"}
	style, err := st.GetRealmAvatarStyle(ctx, p, "")
	if err != nil {
		t.Fatal(err)
	}

	propose := func(revision, parent int64, form avatardomain.SubjectForm, key string) AvatarMutationResult {
		t.Helper()
		reference := style.StylePack.References[0]
		for _, candidate := range style.StylePack.References {
			if candidate.SubjectForm == form {
				reference = candidate
				break
			}
		}
		result, err := st.ProposeAvatar(ctx, p, ProposeAvatarInput{
			ExpectedProfileRevision: revision, ParentVersion: parent,
			StylePackID: style.StylePack.ID, StylePackVersion: style.StylePack.Version,
			SubjectForm: form, SVG: reference.SVG,
			Description: "A portable portrait in the shared flat vector style.",
			VisualSpec:  []byte(`{"identity":{"expression":"calm"}}`),
			Provenance: AvatarClientProvenance{Runtime: "codex", Model: "gpt-5.6",
				Recipe: "archive-round-trip", RecipeVersion: "1"},
			IdempotencyKey: key,
		})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	first := propose(1, 0, avatardomain.SubjectHuman, "avatar-archive-propose-1")
	active, err := st.ActivateAvatar(ctx, p, ActivateAvatarInput{
		Version: 1, ExpectedProfileRevision: first.Avatar.Profile.ProfileRevision,
		IdempotencyKey: "avatar-archive-activate-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	pending := propose(active.Avatar.Profile.ProfileRevision, 1,
		avatardomain.SubjectHuman, "avatar-archive-propose-2")
	if pending.Avatar.Profile.SubjectForm != avatardomain.SubjectHuman ||
		pending.Avatar.Active == nil || pending.Avatar.Active.SubjectForm != avatardomain.SubjectHuman ||
		pending.Avatar.Proposed == nil || pending.Avatar.Proposed.SubjectForm != avatardomain.SubjectHuman {
		t.Fatalf("pending evolution before archive = %#v", pending.Avatar)
	}
	operator := Principal{Kind: PrincipalOperator, ID: provisioned.OperatorID,
		AccountID: provisioned.AccountID, AccountStatus: "active"}
	styleV2 := avatardomain.BuiltInFlatVectorStylePack()
	styleV2.Version = 2
	styleV2.Description = "A second immutable style used across an avatar reset archive boundary."
	styleUpdate, err := st.SetRealmAvatarStyle(ctx, operator, realm.ID,
		CreateAvatarStyleVersionInput{ExpectedStyleRevision: 1, StylePack: styleV2,
			IdempotencyKey: "avatar-archive-style-2"})
	if err != nil {
		t.Fatal(err)
	}
	style = styleUpdate.Style
	afterStyle, err := st.GetAvatar(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if afterStyle.Profile.LatestVersion != 2 || afterStyle.Profile.ActiveVersion != 1 ||
		afterStyle.Profile.ProposedVersion != 0 || afterStyle.Proposed != nil ||
		afterStyle.Profile.Style.Version != 2 {
		t.Fatalf("style-cleared pending proposal = %#v", afterStyle)
	}
	pending = propose(afterStyle.Profile.ProfileRevision, 1,
		avatardomain.SubjectHuman, "avatar-archive-propose-3")
	if pending.Avatar.Proposed == nil || pending.Avatar.Proposed.Version != 3 ||
		pending.Avatar.Proposed.Style.Version != 2 {
		t.Fatalf("new-style proposal before reset = %#v", pending.Avatar)
	}
	reset, err := st.ResetAvatar(ctx, p, ResetAvatarInput{
		ExpectedProfileRevision: pending.Avatar.Profile.ProfileRevision,
		ReasonCode:              "new_direction", IdempotencyKey: "avatar-archive-reset-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if reset.Receipt.ResultLineageGeneration != 2 ||
		reset.Avatar.Profile.LineageGeneration != 2 ||
		reset.Avatar.Profile.LatestVersion != 3 || reset.Avatar.Profile.ActiveVersion != 0 ||
		reset.Avatar.Profile.ProposedVersion != 0 || reset.Avatar.Active == nil ||
		reset.Avatar.Active.Version != 0 || reset.Avatar.Active.LineageGeneration != 2 {
		t.Fatalf("reset projection before archive = %#v", reset)
	}
	fresh := propose(reset.Avatar.Profile.ProfileRevision, 0,
		avatardomain.SubjectAnimal, "avatar-archive-propose-4")
	freshActive, err := st.ActivateAvatar(ctx, p, ActivateAvatarInput{
		Version: 4, ExpectedProfileRevision: fresh.Avatar.Profile.ProfileRevision,
		IdempotencyKey: "avatar-archive-activate-4",
	})
	if err != nil {
		t.Fatal(err)
	}
	pending = propose(freshActive.Avatar.Profile.ProfileRevision, 4,
		avatardomain.SubjectAnimal, "avatar-archive-propose-5")
	quota, err := st.SetAvatarQuota(ctx, operator, agent.ID, UpdateAvatarQuotaInput{
		RetainedPayloadCountLimit: AvatarMinRetainedPayloadCountLimit,
		RetainedPayloadByteLimit:  AvatarMaxRetainedPayloadByteLimit,
		ExpectedProfileRevision:   pending.Avatar.Profile.ProfileRevision,
		IdempotencyKey:            "avatar-archive-quota-4",
	})
	if err != nil {
		t.Fatal(err)
	}
	if quota.Avatar.Profile.RetainedPayloadCount != 4 {
		t.Fatalf("archive quota compaction = %#v", quota.Avatar.Profile)
	}

	if err := st.SuspendAccountSystem(ctx, provisioned.AccountID, "evacuation", "avatar archive round trip"); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := st.ExportAccount(ctx, provisioned.AccountID, "source-cell", "test", &archive); err != nil {
		t.Fatal(err)
	}
	manifest, rows := readAvatarArchiveRows(t, archive.Bytes(), SchemaVersion())
	for _, table := range []string{
		"avatar_style_packs", "avatar_style_pack_versions", "realm_avatar_styles",
		"agent_avatar_profiles", "agent_avatar_versions", "agent_avatar_activations",
		"agent_avatar_resets", "avatar_mutation_receipts",
	} {
		if len(rows[table]) == 0 {
			t.Fatalf("current archive omitted non-empty %s stream", table)
		}
	}
	if manifest.SchemaVersion != SchemaVersion() {
		t.Fatalf("manifest schema = %d, want %d", manifest.SchemaVersion, SchemaVersion())
	}
	var archivedCompacted bool
	var archivedFingerprint []byte
	for _, row := range rows["agent_avatar_versions"] {
		var version map[string]any
		if err := json.Unmarshal(row, &version); err != nil {
			t.Fatal(err)
		}
		if number, ok := version["version"].(float64); ok && number == 1 {
			archivedFingerprint, err = importedAvatarContinuityFingerprint(version)
			if err != nil {
				t.Fatalf("archived continuity fingerprint = %v", err)
			}
			archivedCompacted = version["payload_state"] == "compacted" &&
				version["svg"] == nil && version["description"] == nil &&
				version["visual_spec"] == nil && version["locked_layers_sha256"] != nil &&
				len(archivedFingerprint) == avatardomain.PerceptualContinuityFingerprintBytes
		}
	}
	if !archivedCompacted {
		t.Fatal("archive did not explicitly retain the compacted version envelope")
	}
	if err := avatardomain.ValidatePerceptualContinuityFingerprintForStyle(
		archivedFingerprint, avatardomain.BuiltInFlatVectorStylePack()); err != nil {
		t.Fatalf("archived continuity fingerprint style = %v", err)
	}

	if err := deleteAccountForIntegrationTest(ctx, st, provisioned.AccountID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ImportAccount(ctx, provisioned.AccountID, bytes.NewReader(archive.Bytes())); err != nil {
		t.Fatal(err)
	}
	var restoredFingerprint []byte
	if err := st.pool.QueryRow(ctx, `
		SELECT continuity_fingerprint FROM agent_avatar_versions
		 WHERE account_id=$1 AND realm_id=$2 AND agent_id=$3 AND version=1`,
		provisioned.AccountID, realm.ID, agent.ID).Scan(&restoredFingerprint); err != nil {
		t.Fatal(err)
	}
	if len(restoredFingerprint) != avatardomain.PerceptualContinuityFingerprintBytes {
		t.Fatalf("restored continuity fingerprint length = %d, want %d",
			len(restoredFingerprint), avatardomain.PerceptualContinuityFingerprintBytes)
	}
	if !bytes.Equal(restoredFingerprint, archivedFingerprint) {
		t.Fatal("restored continuity fingerprint differs from the archived boundary")
	}
	if err := avatardomain.ValidatePerceptualContinuityFingerprintForStyle(
		restoredFingerprint, avatardomain.BuiltInFlatVectorStylePack()); err != nil {
		t.Fatalf("restored continuity fingerprint style = %v", err)
	}
	restored, err := st.GetAvatar(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Profile.Status != avatardomain.StatusProposed ||
		restored.Profile.SubjectForm != avatardomain.SubjectAnimal ||
		restored.Profile.LineageGeneration != 2 || restored.Profile.LatestVersion != 5 ||
		restored.Profile.ActiveVersion != 4 || restored.Profile.ProposedVersion != 5 ||
		restored.Active == nil || restored.Active.LineageGeneration != 2 ||
		restored.Active.SubjectForm != avatardomain.SubjectAnimal || restored.Proposed == nil ||
		restored.Proposed.LineageGeneration != 2 ||
		restored.Proposed.SubjectForm != avatardomain.SubjectAnimal ||
		restored.Profile.RetainedPayloadCountLimit != 4 ||
		restored.Profile.RetainedPayloadCount != 4 {
		t.Fatalf("restored pending evolution = %#v", restored)
	}
	history, err := st.GetAvatarHistory(ctx, p, 10)
	if err != nil || len(history.Versions) != 5 {
		t.Fatalf("restored history = %#v / %v", history, err)
	}
	if history.Versions[4].Version != 1 || history.Versions[4].LineageGeneration != 1 ||
		!history.Versions[4].WasActivated || history.Versions[4].RollbackEligible ||
		history.Versions[4].PayloadState != avatardomain.PayloadCompacted ||
		history.Versions[4].LockedLayersSHA256 == "" ||
		history.Versions[3].Version != 2 || history.Versions[3].WasActivated ||
		history.Versions[3].RollbackEligible ||
		history.Versions[1].Version != 4 || history.Versions[1].LineageGeneration != 2 ||
		!history.Versions[1].IsActive {
		t.Fatalf("restored lineage history = %#v", history.Versions)
	}
	compactedVersion, err := st.GetAvatarVersion(ctx, p, 1)
	if err != nil || compactedVersion.PayloadState != avatardomain.PayloadCompacted ||
		compactedVersion.SVG != "" || compactedVersion.Description != "" ||
		compactedVersion.VisualSpec != nil || compactedVersion.SVGSHA256 == "" ||
		compactedVersion.LockedLayersSHA256 == "" {
		t.Fatalf("restored compacted exact version = %#v / %v", compactedVersion, err)
	}
	var receipts int
	if err := st.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM avatar_mutation_receipts WHERE account_id=$1`,
		provisioned.AccountID).Scan(&receipts); err != nil || receipts != 10 {
		t.Fatalf("restored receipts = %d / %v, want 10", receipts, err)
	}
}

func TestAvatarArchiveSchema49SynthesizesDefaultsPostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}

	provisioned, err := st.ProvisionAccount(ctx,
		fmt.Sprintf("avatar-legacy-%d@witwave.ai", time.Now().UnixNano()),
		"avatar legacy synthesis", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = deleteAccountForIntegrationTest(context.Background(), st, provisioned.AccountID) }()
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate account = %t / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "default")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "legacy-portrait")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SuspendAccountSystem(ctx, provisioned.AccountID, "evacuation", "schema 49 avatar synthesis"); err != nil {
		t.Fatal(err)
	}
	var current bytes.Buffer
	if err := st.ExportAccount(ctx, provisioned.AccountID, "source-cell", "test", &current); err != nil {
		t.Fatal(err)
	}
	manifest, rows := readAvatarArchiveRows(t, current.Bytes(), SchemaVersion())
	legacy := writeAvatarArchiveRows(t, archiveexport.Manifest{
		SchemaVersion: 49, ServerVersion: manifest.ServerVersion,
		AccountID: provisioned.AccountID, Cell: manifest.Cell,
		Status: manifest.Status, ExportedAt: manifest.ExportedAt,
	}, canonicalArchiveTableNamesForSchema(49), rows)

	if err := deleteAccountForIntegrationTest(ctx, st, provisioned.AccountID); err != nil {
		t.Fatal(err)
	}
	imported, err := st.ImportAccount(ctx, provisioned.AccountID, bytes.NewReader(legacy))
	if err != nil {
		t.Fatal(err)
	}
	if imported.SchemaVersion != 49 {
		t.Fatalf("imported manifest schema = %d, want 49", imported.SchemaVersion)
	}
	p := Principal{Kind: PrincipalAgent, ID: agent.ID, AccountID: provisioned.AccountID,
		RealmID: realm.ID, AgentName: agent.Name}
	restored, err := st.GetAvatar(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Profile.Status != avatardomain.StatusGenerationDue ||
		restored.Profile.ProfileRevision != 1 || restored.Profile.LatestVersion != 0 ||
		restored.Profile.ActiveVersion != 0 || restored.Profile.FallbackSeed != agent.ID ||
		restored.Active == nil || restored.Active.Version != 0 {
		t.Fatalf("synthesized legacy avatar = %#v", restored)
	}
	style, err := st.GetRealmAvatarStyle(ctx, p, "")
	if err != nil || style.StylePack.ID != avatardomain.DefaultStylePackID ||
		style.StylePack.Version != avatardomain.BuiltInStylePackVersion {
		t.Fatalf("synthesized legacy style = %#v / %v", style, err)
	}
	var profiles, versions, receipts int
	if err := st.pool.QueryRow(ctx, `
		SELECT (SELECT COUNT(*) FROM agent_avatar_profiles WHERE account_id=$1),
		       (SELECT COUNT(*) FROM agent_avatar_versions WHERE account_id=$1),
		       (SELECT COUNT(*) FROM avatar_mutation_receipts WHERE account_id=$1)`,
		provisioned.AccountID).Scan(&profiles, &versions, &receipts); err != nil {
		t.Fatal(err)
	}
	if profiles != 1 || versions != 0 || receipts != 0 {
		t.Fatalf("legacy synthesis counts = profiles:%d versions:%d receipts:%d", profiles, versions, receipts)
	}
}

type avatarArchiveRowsSource struct {
	table string
	rows  [][]byte
	next  int
}

func (s *avatarArchiveRowsSource) Table() string { return s.table }

func (s *avatarArchiveRowsSource) Next(context.Context) ([]byte, error) {
	if s.next >= len(s.rows) {
		return nil, nil
	}
	row := s.rows[s.next]
	s.next++
	return row, nil
}

func readAvatarArchiveRows(t *testing.T, raw []byte, currentSchema int) (archiveexport.Manifest, map[string][][]byte) {
	t.Helper()
	rows := map[string][][]byte{}
	manifest, err := archiveexport.Read(context.Background(), bytes.NewReader(raw), archiveexport.ImportOptions{
		CurrentSchema: currentSchema,
		Row: func(table string, row []byte) error {
			rows[table] = append(rows[table], bytes.Clone(row))
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return manifest, rows
}

func writeAvatarArchiveRows(t *testing.T, manifest archiveexport.Manifest, tables []string, rows map[string][][]byte) []byte {
	t.Helper()
	sources := make([]archiveexport.RowSource, 0, len(tables))
	for _, table := range tables {
		sources = append(sources, &avatarArchiveRowsSource{table: table, rows: rows[table]})
	}
	var archive bytes.Buffer
	if err := archiveexport.Write(context.Background(), &archive, manifest, sources); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}
