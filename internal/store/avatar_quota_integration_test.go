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
	"sync"
	"testing"
	"time"

	avatardomain "github.com/witwave-ai/witself/internal/avatar"
	"github.com/witwave-ai/witself/internal/id"
)

func TestAvatarPayloadCompactionExpandActivateGatePostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	phaseA, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer phaseA.Close()
	if phaseA.avatarPayloadCompactionEnabled {
		t.Fatal("avatar payload compaction defaulted on")
	}
	if err := phaseA.Migrate(); err != nil {
		t.Fatal(err)
	}
	provisioned, err := phaseA.ProvisionAccount(ctx,
		fmt.Sprintf("avatar-compaction-gate-%d@witwave.ai", time.Now().UnixNano()),
		"avatar compaction gate", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = deleteAccountForIntegrationTest(context.Background(), phaseA,
			provisioned.AccountID)
	}()
	if activated, err := phaseA.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate account = %t / %v", activated, err)
	}
	realm, err := phaseA.CreateRealm(ctx, provisioned.AccountID, "compaction-gate")
	if err != nil {
		t.Fatal(err)
	}
	agent := createAvatarResetTestAgent(ctx, t, phaseA,
		provisioned.AccountID, realm.ID, "compaction-gate-agent")
	operator := Principal{Kind: PrincipalOperator, ID: provisioned.OperatorID,
		AccountID: provisioned.AccountID, AccountStatus: "active"}
	style, err := phaseA.GetRealmAvatarStyle(ctx, operator, realm.ID)
	if err != nil {
		t.Fatal(err)
	}
	proposalInput := func(revision, parent, version int64) ProposeAvatarInput {
		return ProposeAvatarInput{
			ExpectedProfileRevision: revision,
			ParentVersion:           parent,
			StylePackID:             style.StylePack.ID,
			StylePackVersion:        style.StylePack.Version,
			SubjectForm:             avatardomain.SubjectHuman,
			Description:             "An operator-reviewed portrait during a safe rollout.",
			VisualSpec:              json.RawMessage(`{"identity":{"expression":"calm"}}`),
			SVG:                     style.StylePack.References[0].SVG,
			Provenance: AvatarClientProvenance{Runtime: "codex", Model: "gpt-5.6",
				Recipe: "compaction-gate-test", RecipeVersion: "1"},
			IdempotencyKey: fmt.Sprintf("compaction-gate-propose-%d", version),
		}
	}
	revision, parent := int64(1), int64(0)
	for version := int64(1); version <= 5; version++ {
		proposed, err := phaseA.ProposeAgentAvatar(ctx, operator, agent.ID,
			proposalInput(revision, parent, version))
		if err != nil {
			t.Fatal(err)
		}
		active, err := phaseA.ActivateAgentAvatar(ctx, operator, agent.ID,
			ActivateAvatarInput{Version: version,
				ExpectedProfileRevision: proposed.Avatar.Profile.ProfileRevision,
				IdempotencyKey:          fmt.Sprintf("compaction-gate-activate-%d", version)})
		if err != nil {
			t.Fatal(err)
		}
		revision, parent = active.Avatar.Profile.ProfileRevision, version
	}
	// Model a schema-50 writer that committed during Phase A after this pod's
	// startup backfill. Disabled quota accounting must tolerate the nullable
	// application-derived digest without attempting cleanup.
	if _, err := phaseA.pool.Exec(ctx, `
		UPDATE agent_avatar_versions SET locked_layers_sha256=NULL
		 WHERE agent_id=$1 AND version=1`, agent.ID); err != nil {
		t.Fatal(err)
	}
	// A no-cleanup lower succeeds while the gate remains off.
	quotaSix, err := phaseA.SetAvatarQuota(ctx, operator, agent.ID,
		UpdateAvatarQuotaInput{RetainedPayloadCountLimit: 6,
			RetainedPayloadByteLimit: AvatarDefaultRetainedPayloadByteLimit,
			ExpectedProfileRevision:  revision,
			IdempotencyKey:           "compaction-gate-quota-six"})
	if err != nil || quotaSix.Avatar.Profile.RetainedPayloadCount != 5 {
		t.Fatalf("phase-A no-cleanup quota = %#v / %v", quotaSix, err)
	}
	revision = quotaSix.Avatar.Profile.ProfileRevision
	proposedSix, err := phaseA.ProposeAgentAvatar(ctx, operator, agent.ID,
		proposalInput(revision, parent, 6))
	if err != nil {
		t.Fatalf("phase-A fitting proposal = %v", err)
	}
	activeSix, err := phaseA.ActivateAgentAvatar(ctx, operator, agent.ID,
		ActivateAvatarInput{Version: 6,
			ExpectedProfileRevision: proposedSix.Avatar.Profile.ProfileRevision,
			IdempotencyKey:          "compaction-gate-activate-6"})
	if err != nil {
		t.Fatal(err)
	}
	revision, parent = activeSix.Avatar.Profile.ProfileRevision, 6

	if _, err := phaseA.ProposeAgentAvatar(ctx, operator, agent.ID,
		proposalInput(revision, parent, 7)); !errors.Is(err, ErrAvatarPayloadCompactionDisabled) {
		t.Fatalf("phase-A cleanup proposal error = %v, want activation conflict", err)
	}
	quotaFourInput := UpdateAvatarQuotaInput{
		RetainedPayloadCountLimit: AvatarMinRetainedPayloadCountLimit,
		RetainedPayloadByteLimit:  AvatarDefaultRetainedPayloadByteLimit,
		ExpectedProfileRevision:   revision,
		IdempotencyKey:            "compaction-gate-quota-four",
	}
	if _, err := phaseA.SetAvatarQuota(ctx, operator, agent.ID,
		quotaFourInput); !errors.Is(err, ErrAvatarPayloadCompactionDisabled) {
		t.Fatalf("phase-A cleanup quota error = %v, want activation conflict", err)
	}
	phaseAView, err := phaseA.GetAvatar(ctx, agent)
	if err != nil {
		t.Fatal(err)
	}
	if phaseAView.Profile.ProfileRevision != revision ||
		phaseAView.Profile.LatestVersion != 6 ||
		phaseAView.Profile.RetainedPayloadCount != 6 ||
		phaseAView.Profile.RetainedPayloadCountLimit != 6 {
		t.Fatalf("phase-A conflict mutated avatar = %#v", phaseAView.Profile)
	}
	var compactedBefore int
	if err := phaseA.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM agent_avatar_versions
		 WHERE agent_id=$1 AND payload_state='compacted'`, agent.ID).
		Scan(&compactedBefore); err != nil || compactedBefore != 0 {
		t.Fatalf("phase-A compacted rows = %d / %v", compactedBefore, err)
	}

	// Phase B is a config-only restart. Migrate reruns the nullable digest
	// repair before this enabled store can serve a cleanup mutation.
	phaseB, err := Open(ctx, dsn, WithAvatarPayloadCompactionEnabled(true))
	if err != nil {
		t.Fatal(err)
	}
	defer phaseB.Close()
	if err := phaseB.Migrate(); err != nil {
		t.Fatal(err)
	}
	var repairedDigest *string
	if err := phaseB.pool.QueryRow(ctx, `
		SELECT locked_layers_sha256 FROM agent_avatar_versions
		 WHERE agent_id=$1 AND version=1`, agent.ID).Scan(&repairedDigest); err != nil ||
		repairedDigest == nil {
		t.Fatalf("Phase-B restart digest repair = %v / %v", repairedDigest, err)
	}
	// The enabled mutation boundary also repairs a last nullable legacy row in
	// case one committed between startup repair and old-writer convergence.
	if _, err := phaseB.pool.Exec(ctx, `
		UPDATE agent_avatar_versions SET locked_layers_sha256=NULL
		 WHERE agent_id=$1 AND version=6`, agent.ID); err != nil {
		t.Fatal(err)
	}
	lowered, err := phaseB.SetAvatarQuota(ctx, operator, agent.ID, quotaFourInput)
	if err != nil {
		t.Fatal(err)
	}
	if lowered.Avatar.Profile.RetainedPayloadCount != 4 ||
		lowered.Avatar.Profile.RetainedPayloadCountLimit != 4 ||
		lowered.Avatar.Profile.ActiveVersion != 6 {
		t.Fatalf("phase-B activated cleanup = %#v", lowered.Avatar.Profile)
	}
	var compactedAfter int
	if err := phaseB.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM agent_avatar_versions
		 WHERE agent_id=$1 AND payload_state='compacted'`, agent.ID).
		Scan(&compactedAfter); err != nil || compactedAfter != 2 {
		t.Fatalf("phase-B compacted rows = %d / %v, want 2", compactedAfter, err)
	}
	if err := phaseB.pool.QueryRow(ctx, `
		SELECT locked_layers_sha256 FROM agent_avatar_versions
		 WHERE agent_id=$1 AND version=6`, agent.ID).Scan(&repairedDigest); err != nil ||
		repairedDigest == nil {
		t.Fatalf("enabled quota digest repair = %v / %v", repairedDigest, err)
	}
	var compactedVersion int64
	if err := phaseB.pool.QueryRow(ctx, `
		UPDATE agent_avatar_versions SET locked_layers_sha256=NULL
		 WHERE agent_id=$1 AND version=(
		   SELECT MIN(version) FROM agent_avatar_versions
		    WHERE agent_id=$1 AND payload_state='compacted'
		 ) RETURNING version`, agent.ID).Scan(&compactedVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := phaseB.GetAvatarVersion(ctx, agent, compactedVersion); err == nil ||
		!strings.Contains(err.Error(), "lacks a recoverable locked-layer digest") {
		t.Fatalf("unrecoverable compacted-row read error = %v", err)
	}
}

func TestAvatarPayloadQuotaCompactionLifecyclePostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
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
		fmt.Sprintf("avatar-quota-%d@witwave.ai", time.Now().UnixNano()),
		"avatar quota", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = deleteAccountForIntegrationTest(context.Background(), st, provisioned.AccountID) }()
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate = %t / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "quota")
	if err != nil {
		t.Fatal(err)
	}
	agent := createAvatarResetTestAgent(ctx, t, st, provisioned.AccountID, realm.ID, "quota-agent")
	operator := Principal{Kind: PrincipalOperator, ID: provisioned.OperatorID,
		AccountID: provisioned.AccountID, AccountStatus: "active"}
	style, err := st.GetRealmAvatarStyle(ctx, operator, realm.ID)
	if err != nil {
		t.Fatal(err)
	}

	revision, parent := int64(1), int64(0)
	for version := int64(1); version <= 4; version++ {
		proposed := proposeAvatarResetVersion(ctx, t, st, agent, style.StylePack,
			revision, parent, fmt.Sprintf("quota-propose-%d", version))
		active, err := st.ActivateAvatar(ctx, agent, ActivateAvatarInput{
			Version:                 proposed.Avatar.Profile.ProposedVersion,
			ExpectedProfileRevision: proposed.Avatar.Profile.ProfileRevision,
			IdempotencyKey:          fmt.Sprintf("quota-activate-%d", version),
		})
		if err != nil {
			t.Fatal(err)
		}
		revision = active.Avatar.Profile.ProfileRevision
		parent = version
	}
	rejectedProposal := proposeAvatarResetVersion(ctx, t, st, agent, style.StylePack,
		revision, parent, "quota-propose-rejected-5")
	rejected, err := st.RejectAgentAvatar(ctx, operator, agent.ID, RejectAvatarInput{
		Version: 5, ExpectedProfileRevision: rejectedProposal.Avatar.Profile.ProfileRevision,
		ReasonCode: "quota_fixture", IdempotencyKey: "quota-reject-5",
	})
	if err != nil {
		t.Fatal(err)
	}
	pending := proposeAvatarResetVersion(ctx, t, st, agent, style.StylePack,
		rejected.Avatar.Profile.ProfileRevision, parent, "quota-propose-pending-6")
	// Simulate a larger historical rejected payload so compacting version 5
	// offsets the exact WAPF retained when the small version 1 boundary is
	// compacted. The planner must never let boundary creation grow total retained
	// content, even when the mutation is count-quota driven.
	if _, err := st.pool.Exec(ctx, `
		UPDATE agent_avatar_versions SET payload_bytes=50000
		 WHERE agent_id=$1 AND version=5`, agent.ID); err != nil {
		t.Fatal(err)
	}
	retainedContentBytes := func() int64 {
		t.Helper()
		var bytes int64
		if err := st.pool.QueryRow(ctx, `
			SELECT COALESCE(SUM(CASE WHEN payload_state='full' THEN payload_bytes ELSE 0 END),0) +
			       COALESCE(SUM(octet_length(continuity_fingerprint)),0)
			  FROM agent_avatar_versions WHERE agent_id=$1`, agent.ID).Scan(&bytes); err != nil {
			t.Fatal(err)
		}
		return bytes
	}
	beforeLoweringBytes := retainedContentBytes()

	quotaInput := UpdateAvatarQuotaInput{
		RetainedPayloadCountLimit: AvatarMinRetainedPayloadCountLimit,
		RetainedPayloadByteLimit:  AvatarMinRetainedPayloadByteLimit,
		ExpectedProfileRevision:   pending.Avatar.Profile.ProfileRevision,
		IdempotencyKey:            "quota-lower-immediate",
	}
	lowered, err := st.SetAvatarQuota(ctx, operator, agent.ID, quotaInput)
	if err != nil {
		t.Fatal(err)
	}
	if lowered.Avatar.Profile.RetainedPayloadCountLimit != 4 ||
		lowered.Avatar.Profile.RetainedPayloadCount != 4 ||
		lowered.Avatar.Profile.ActiveVersion != 4 ||
		lowered.Avatar.Profile.ProposedVersion != 6 {
		t.Fatalf("lowered quota view = %#v", lowered.Avatar.Profile)
	}
	afterLoweringBytes := retainedContentBytes()
	if afterLoweringBytes > beforeLoweringBytes {
		t.Fatalf("quota cleanup grew retained content from %d to %d bytes",
			beforeLoweringBytes, afterLoweringBytes)
	}
	if afterLoweringBytes > quotaInput.RetainedPayloadByteLimit ||
		lowered.Avatar.Profile.RetainedPayloadBytes != afterLoweringBytes {
		t.Fatalf("inclusive retained bytes = db:%d view:%d limit:%d",
			afterLoweringBytes, lowered.Avatar.Profile.RetainedPayloadBytes,
			quotaInput.RetainedPayloadByteLimit)
	}
	history, err := st.GetAvatarHistory(ctx, agent, 10)
	if err != nil {
		t.Fatal(err)
	}
	byVersion := make(map[int64]AvatarVersionSummary, len(history.Versions))
	for _, version := range history.Versions {
		byVersion[version.Version] = version
	}
	for _, version := range []int64{1, 5} {
		if byVersion[version].PayloadState != avatardomain.PayloadCompacted ||
			byVersion[version].PayloadCompactedAt == nil ||
			byVersion[version].PayloadCompactionReason != "quota" ||
			byVersion[version].LockedLayersSHA256 == "" ||
			byVersion[version].RollbackEligible {
			t.Fatalf("compacted history version %d = %#v", version, byVersion[version])
		}
	}
	for _, version := range []int64{2, 3, 4, 6} {
		if byVersion[version].PayloadState != avatardomain.PayloadFull {
			t.Fatalf("protected history version %d = %#v", version, byVersion[version])
		}
	}
	if !byVersion[4].IsActive || !byVersion[6].IsProposed ||
		!byVersion[2].RollbackEligible || !byVersion[3].RollbackEligible {
		t.Fatalf("protected lifecycle projection = %#v", byVersion)
	}
	var boundaryFingerprint, obsoleteFingerprint []byte
	if err := st.pool.QueryRow(ctx, `
		SELECT continuity_fingerprint FROM agent_avatar_versions
		 WHERE agent_id=$1 AND version=1`, agent.ID).Scan(&boundaryFingerprint); err != nil {
		t.Fatal(err)
	}
	if len(boundaryFingerprint) != avatardomain.PerceptualContinuityFingerprintBytes {
		t.Fatalf("boundary fingerprint length = %d, want %d", len(boundaryFingerprint),
			avatardomain.PerceptualContinuityFingerprintBytes)
	}
	if err := avatardomain.ValidatePerceptualContinuityFingerprintForStyle(
		boundaryFingerprint, style.StylePack); err != nil {
		t.Fatalf("boundary fingerprint = %v", err)
	}
	if err := st.pool.QueryRow(ctx, `
		SELECT continuity_fingerprint FROM agent_avatar_versions
		 WHERE agent_id=$1 AND version=5`, agent.ID).Scan(&obsoleteFingerprint); err != nil {
		t.Fatal(err)
	}
	if len(obsoleteFingerprint) != 0 {
		t.Fatalf("obsolete rejected version retained %d fingerprint bytes", len(obsoleteFingerprint))
	}
	compacted, err := st.GetAvatarVersion(ctx, agent, 5)
	if err != nil {
		t.Fatal(err)
	}
	if compacted.PayloadState != avatardomain.PayloadCompacted || compacted.SVG != "" ||
		compacted.Description != "" || compacted.VisualSpec != nil ||
		compacted.SVGSHA256 == "" || compacted.LockedLayersSHA256 == "" ||
		compacted.Provenance.Runtime == "" || compacted.PayloadCompactedAt == nil {
		t.Fatalf("compacted exact version = %#v", compacted)
	}

	assertCompactionEvents := func(want int, firstVersions string) {
		t.Helper()
		events, err := st.ListAccountEvents(ctx, provisioned.AccountID,
			provisioned.OperatorID, EventFilter{Verb: VerbAvatarPayloadCompacted, Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		if len(events.Events) != want {
			t.Fatalf("compaction events = %d, want %d", len(events.Events), want)
		}
		if firstVersions != "" {
			var metadata map[string]string
			if err := json.Unmarshal(events.Events[len(events.Events)-1].Metadata, &metadata); err != nil {
				t.Fatal(err)
			}
			if metadata["compacted_versions"] != firstVersions {
				t.Fatalf("first compaction metadata = %#v", metadata)
			}
			if metadata["net_reclaimed_bytes"] == "" || metadata["compacted_bytes"] != "" {
				t.Fatalf("compaction byte metadata = %#v", metadata)
			}
		}
	}
	assertCompactionEvents(1, "5,1")
	firstCompactedAt := compacted.PayloadCompactedAt.UTC()
	replayed, err := st.SetAvatarQuota(ctx, operator, agent.ID, quotaInput)
	if err != nil || !replayed.Receipt.Replayed {
		t.Fatalf("quota replay = %#v / %v", replayed.Receipt, err)
	}
	compactedAfterReplay, err := st.GetAvatarVersion(ctx, agent, 5)
	if err != nil || compactedAfterReplay.PayloadCompactedAt == nil ||
		!compactedAfterReplay.PayloadCompactedAt.Equal(firstCompactedAt) {
		t.Fatalf("quota replay recompacted payload = %#v / %v", compactedAfterReplay, err)
	}
	assertCompactionEvents(1, "5,1")

	// The internal transaction path still fails closed if a future operator
	// surface or migrated configuration asks for less than the protected set.
	beforeProtected := avatarPayloadStateSnapshot(ctx, t, st, agent.ID)
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	target := avatarTarget{accountID: agent.AccountID, realmID: agent.RealmID,
		agentID: agent.ID, agentName: agent.AgentName}
	if err := lockAccountForMint(ctx, tx, agent.AccountID, false); err != nil {
		t.Fatal(err)
	}
	profile, err := lockAvatarProfileTx(ctx, tx, target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compactAvatarPayloadsTx(ctx, tx, target, profile,
		0, 0, 3, AvatarMaxRetainedPayloadByteLimit); !errors.Is(err, ErrAvatarPayloadQuotaExceeded) {
		t.Fatalf("protected-only quota error = %v", err)
	}
	_ = tx.Rollback(ctx)
	afterProtected := avatarPayloadStateSnapshot(ctx, t, st, agent.ID)
	if fmt.Sprint(afterProtected) != fmt.Sprint(beforeProtected) {
		t.Fatalf("failed protected-only quota changed payloads: before=%v after=%v",
			beforeProtected, afterProtected)
	}

	rejectedPending, err := st.RejectAgentAvatar(ctx, operator, agent.ID, RejectAvatarInput{
		Version: 6, ExpectedProfileRevision: lowered.Avatar.Profile.ProfileRevision,
		ReasonCode: "quota_fixture", IdempotencyKey: "quota-reject-6",
	})
	if err != nil {
		t.Fatal(err)
	}
	automatic := proposeAvatarResetVersion(ctx, t, st, agent, style.StylePack,
		rejectedPending.Avatar.Profile.ProfileRevision, 4, "quota-proposal-compacts-7")
	if automatic.Avatar.Profile.RetainedPayloadCount != 4 ||
		automatic.Avatar.Proposed == nil || automatic.Avatar.Proposed.Version != 7 {
		t.Fatalf("proposal-triggered quota view = %#v", automatic.Avatar)
	}
	versionSix, err := st.GetAvatarVersion(ctx, agent, 6)
	if err != nil || versionSix.PayloadState != avatardomain.PayloadCompacted {
		t.Fatalf("proposal did not compact rejected version 6: %#v / %v", versionSix, err)
	}
	assertCompactionEvents(2, "")

	// Two proposals at the same revision serialize on the profile/account lock:
	// one compacts the rejected candidate and commits, the stale peer changes
	// nothing and returns the optimistic-concurrency sentinel.
	rejectedSeven, err := st.RejectAgentAvatar(ctx, operator, agent.ID, RejectAvatarInput{
		Version: 7, ExpectedProfileRevision: automatic.Avatar.Profile.ProfileRevision,
		ReasonCode: "quota_fixture", IdempotencyKey: "quota-reject-7",
	})
	if err != nil {
		t.Fatal(err)
	}
	baseProposal := ProposeAvatarInput{
		ExpectedProfileRevision: rejectedSeven.Avatar.Profile.ProfileRevision,
		ParentVersion:           4, StylePackID: style.StylePack.ID,
		StylePackVersion: style.StylePack.Version,
		SubjectForm:      avatardomain.SubjectHuman,
		Description:      "A concurrent quota evolution in the shared flat portrait style.",
		VisualSpec:       json.RawMessage(`{"identity":{"expression":"calm"}}`),
		SVG:              style.StylePack.References[0].SVG,
		Provenance: AvatarClientProvenance{Runtime: "codex", Model: "gpt-5.6",
			Recipe: "avatar-quota-concurrency", RecipeVersion: "1"},
	}
	errorsOut := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			input := baseProposal
			input.IdempotencyKey = fmt.Sprintf("quota-concurrent-proposal-%d", index)
			_, err := st.ProposeAvatar(ctx, agent, input)
			errorsOut <- err
		}(i)
	}
	wg.Wait()
	close(errorsOut)
	var successes, conflicts int
	for err := range errorsOut {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrAvatarConflict):
			conflicts++
		default:
			t.Fatalf("concurrent proposal error = %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent proposals = successes:%d conflicts:%d", successes, conflicts)
	}
	final, err := st.GetAvatar(ctx, agent)
	if err != nil {
		t.Fatal(err)
	}
	if final.Profile.RetainedPayloadCount != 4 || final.Profile.LatestVersion != 8 ||
		final.Profile.ProposedVersion != 8 {
		t.Fatalf("concurrent quota result = %#v", final.Profile)
	}
	versionSeven, err := st.GetAvatarVersion(ctx, agent, 7)
	if err != nil || versionSeven.PayloadState != avatardomain.PayloadCompacted {
		t.Fatalf("concurrent proposal compaction = %#v / %v", versionSeven, err)
	}
	var fingerprintCount int
	if err := st.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM agent_avatar_versions
		 WHERE agent_id=$1 AND continuity_fingerprint IS NOT NULL`, agent.ID).
		Scan(&fingerprintCount); err != nil {
		t.Fatal(err)
	}
	if fingerprintCount != 1 {
		t.Fatalf("retained continuity fingerprints = %d, want only version 1", fingerprintCount)
	}
	assertCompactionEvents(3, "")

	activatedEight, err := st.ActivateAvatar(ctx, agent, ActivateAvatarInput{
		Version: 8, ExpectedProfileRevision: final.Profile.ProfileRevision,
		IdempotencyKey: "quota-activate-8-for-fingerprint-prune",
	})
	if err != nil {
		t.Fatal(err)
	}
	pruningProposal := proposeAvatarResetVersion(ctx, t, st, agent, style.StylePack,
		activatedEight.Avatar.Profile.ProfileRevision, 8, "quota-proposal-prunes-boundary")
	if pruningProposal.Avatar.Proposed == nil || pruningProposal.Avatar.Proposed.Version != 9 {
		t.Fatalf("fingerprint-pruning proposal = %#v", pruningProposal.Avatar)
	}
	versionTwo, err := st.GetAvatarVersion(ctx, agent, 2)
	if err != nil || versionTwo.PayloadState != avatardomain.PayloadCompacted {
		t.Fatalf("last qualifying child was not compacted: %#v / %v", versionTwo, err)
	}
	var prunedFingerprint []byte
	if err := st.pool.QueryRow(ctx, `
		SELECT continuity_fingerprint FROM agent_avatar_versions
		 WHERE agent_id=$1 AND version=1`, agent.ID).Scan(&prunedFingerprint); err != nil {
		t.Fatal(err)
	}
	if len(prunedFingerprint) != 0 {
		t.Fatalf("last-child compaction retained %d obsolete fingerprint bytes", len(prunedFingerprint))
	}
	var transferredBoundary []byte
	if err := st.pool.QueryRow(ctx, `
		SELECT continuity_fingerprint FROM agent_avatar_versions
		 WHERE agent_id=$1 AND version=2`, agent.ID).Scan(&transferredBoundary); err != nil {
		t.Fatal(err)
	}
	if len(transferredBoundary) != avatardomain.PerceptualContinuityFingerprintBytes {
		t.Fatalf("new compacted parent boundary length = %d, want %d",
			len(transferredBoundary), avatardomain.PerceptualContinuityFingerprintBytes)
	}
	assertCompactionEvents(4, "")
}

func TestAvatarContinuityFingerprintCompactionBoundariesPostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}

	type scenario struct {
		accountID string
		realmID   string
		agent     Principal
		operator  Principal
		pack      avatardomain.StylePack
	}
	newScenario := func(t *testing.T, name string) scenario {
		t.Helper()
		provisioned, err := st.ProvisionAccount(ctx,
			fmt.Sprintf("avatar-boundary-%s-%d@witwave.ai", name, time.Now().UnixNano()),
			"avatar boundary", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = deleteAccountForIntegrationTest(context.Background(), st, provisioned.AccountID)
		})
		if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
			t.Fatalf("activate account = %t / %v", activated, err)
		}
		realm, err := st.CreateRealm(ctx, provisioned.AccountID, "boundary")
		if err != nil {
			t.Fatal(err)
		}
		agent := createAvatarResetTestAgent(ctx, t, st, provisioned.AccountID,
			realm.ID, "boundary-agent")
		operator := Principal{Kind: PrincipalOperator, ID: provisioned.OperatorID,
			AccountID: provisioned.AccountID, AccountStatus: "active"}
		style, err := st.GetRealmAvatarStyle(ctx, operator, realm.ID)
		if err != nil {
			t.Fatal(err)
		}
		return scenario{accountID: provisioned.AccountID, realmID: realm.ID,
			agent: agent, operator: operator, pack: style.StylePack}
	}
	insertVersion := func(t *testing.T, s scenario, version, parent int64,
		pack avatardomain.StylePack, actorKind, actorID, svg string, payloadBytes int64) {
		t.Helper()
		versionID, err := id.New("avver")
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256([]byte(svg))
		lockedDigest, err := avatardomain.LockedLayersSHA256([]byte(svg), pack)
		if err != nil {
			t.Fatal(err)
		}
		var parentValue any
		if parent > 0 {
			parentValue = parent
		}
		if payloadBytes == 0 {
			payloadBytes, err = avatarCreativePayloadBytes(svg,
				"A direct boundary validation fixture.", []byte(`{"identity":{"expression":"calm"}}`))
			if err != nil {
				t.Fatal(err)
			}
		}
		if _, err := st.pool.Exec(ctx, `
			INSERT INTO agent_avatar_versions
			       (account_id, realm_id, agent_id, id, version, parent_version,
			        lineage_generation, style_pack_id, style_pack_version,
			        subject_form, svg, description, visual_spec, svg_sha256,
			        locked_layers_sha256, provenance, proposed_by_kind,
			        proposed_by_id, payload_bytes)
			VALUES ($1,$2,$3,$4,$5,$6,1,$7,$8,'human',$9,
			        'A direct boundary validation fixture.',
			        '{"identity":{"expression":"calm"}}'::jsonb,$10,$11,
			        '{"runtime":"boundary-test"}'::jsonb,$12,$13,$14)`,
			s.accountID, s.realmID, s.agent.ID, versionID,
			version, parentValue, pack.ID, pack.Version, svg,
			hex.EncodeToString(digest[:]), lockedDigest, actorKind, actorID,
			payloadBytes); err != nil {
			t.Fatal(err)
		}
	}
	compact := func(t *testing.T, s scenario, countLimit int) (avatarPayloadCompactionPlan, error) {
		t.Helper()
		tx, err := st.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if err := lockAccountForMint(ctx, tx, s.accountID, false); err != nil {
			t.Fatal(err)
		}
		profile, err := lockAvatarProfileTx(ctx, tx, avatarTarget{
			accountID: s.accountID, realmID: s.realmID,
			agentID: s.agent.ID, agentName: s.agent.AgentName,
		})
		if err != nil {
			t.Fatal(err)
		}
		plan, err := compactAvatarPayloadsTx(ctx, tx, avatarTarget{
			accountID: s.accountID, realmID: s.realmID,
			agentID: s.agent.ID, agentName: s.agent.AgentName,
		}, profile, 0, 0, countLimit, AvatarMaxRetainedPayloadByteLimit)
		if err != nil {
			return avatarPayloadCompactionPlan{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		return plan, nil
	}
	assertNoFingerprints := func(t *testing.T, s scenario) {
		t.Helper()
		var count int
		if err := st.pool.QueryRow(ctx, `
			SELECT COUNT(*) FROM agent_avatar_versions
			 WHERE agent_id=$1 AND continuity_fingerprint IS NOT NULL`, s.agent.ID).
			Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("retained continuity fingerprints = %d, want 0", count)
		}
	}

	t.Run("operator-authored child", func(t *testing.T) {
		s := newScenario(t, "operator")
		insertVersion(t, s, 1, 0, s.pack, PrincipalAgent, s.agent.ID,
			s.pack.References[0].SVG, 0)
		insertVersion(t, s, 2, 1, s.pack, PrincipalOperator, s.operator.ID,
			s.pack.References[0].SVG, 0)
		plan, err := compact(t, s, 1)
		if err != nil || len(plan.versions) != 1 || plan.versions[0] != 1 {
			t.Fatalf("operator-child compaction = %#v / %v", plan, err)
		}
		assertNoFingerprints(t, s)
	})

	t.Run("different-style child", func(t *testing.T) {
		s := newScenario(t, "style")
		packV2 := s.pack
		packV2.Version = 2
		packV2.Description = "A second immutable boundary-test style version."
		if _, err := st.SetRealmAvatarStyle(ctx, s.operator, s.realmID,
			CreateAvatarStyleVersionInput{ExpectedStyleRevision: 1, StylePack: packV2,
				IdempotencyKey: "boundary-style-v2"}); err != nil {
			t.Fatal(err)
		}
		insertVersion(t, s, 1, 0, s.pack, PrincipalAgent, s.agent.ID,
			s.pack.References[0].SVG, 0)
		insertVersion(t, s, 2, 1, packV2, PrincipalAgent, s.agent.ID,
			packV2.References[0].SVG, 0)
		plan, err := compact(t, s, 1)
		if err != nil || len(plan.versions) != 1 || plan.versions[0] != 1 {
			t.Fatalf("different-style compaction = %#v / %v", plan, err)
		}
		assertNoFingerprints(t, s)
	})

	t.Run("child in same plan", func(t *testing.T) {
		s := newScenario(t, "same-plan")
		insertVersion(t, s, 1, 0, s.pack, PrincipalAgent, s.agent.ID,
			s.pack.References[0].SVG, 0)
		insertVersion(t, s, 2, 1, s.pack, PrincipalAgent, s.agent.ID,
			s.pack.References[0].SVG, 0)
		plan, err := compact(t, s, 0)
		if err != nil || len(plan.versions) != 2 {
			t.Fatalf("same-plan child compaction = %#v / %v", plan, err)
		}
		assertNoFingerprints(t, s)
	})

	t.Run("different-subject child", func(t *testing.T) {
		s := newScenario(t, "subject")
		insertVersion(t, s, 1, 0, s.pack, PrincipalAgent, s.agent.ID,
			s.pack.References[0].SVG, 0)
		insertVersion(t, s, 2, 1, s.pack, PrincipalAgent, s.agent.ID,
			s.pack.References[0].SVG, 0)
		if _, err := st.pool.Exec(ctx, `
			UPDATE agent_avatar_versions SET subject_form='animal'
			 WHERE agent_id=$1 AND version=2`, s.agent.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := compact(t, s, 1); !errors.Is(err, ErrAvatarConflict) {
			t.Fatalf("different-subject compaction error = %v, want conflict", err)
		}
		var full, payloads, fingerprints int
		if err := st.pool.QueryRow(ctx, `
			SELECT COUNT(*) FILTER (WHERE payload_state='full'),
			       COUNT(svg), COUNT(continuity_fingerprint)
			  FROM agent_avatar_versions WHERE agent_id=$1`, s.agent.ID).
			Scan(&full, &payloads, &fingerprints); err != nil {
			t.Fatal(err)
		}
		if full != 2 || payloads != 2 || fingerprints != 0 {
			t.Fatalf("different-subject rollback = full:%d payloads:%d fingerprints:%d",
				full, payloads, fingerprints)
		}
	})

	t.Run("different-subject final child remains full", func(t *testing.T) {
		s := newScenario(t, "subject-final-child")
		insertVersion(t, s, 1, 0, s.pack, PrincipalAgent, s.agent.ID,
			s.pack.References[0].SVG, 50_000)
		insertVersion(t, s, 2, 1, s.pack, PrincipalAgent, s.agent.ID,
			s.pack.References[0].SVG, 0)
		if plan, err := compact(t, s, 1); err != nil ||
			len(plan.versions) != 1 || plan.versions[0] != 1 {
			t.Fatalf("prepare subject boundary = %#v / %v", plan, err)
		}
		if _, err := st.pool.Exec(ctx, `
			UPDATE agent_avatar_versions SET subject_form='animal'
			 WHERE agent_id=$1 AND version=2`, s.agent.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := compact(t, s, 0); !errors.Is(err, ErrAvatarConflict) {
			t.Fatalf("different-subject final-child error = %v, want conflict", err)
		}
		var parentState, childState string
		var fingerprint []byte
		var childSVG *string
		if err := st.pool.QueryRow(ctx, `
			SELECT parent.payload_state, parent.continuity_fingerprint,
			       child.payload_state, child.svg
			  FROM agent_avatar_versions parent
			  JOIN agent_avatar_versions child
			    ON child.agent_id=parent.agent_id AND child.version=2
			 WHERE parent.agent_id=$1 AND parent.version=1`, s.agent.ID).
			Scan(&parentState, &fingerprint, &childState, &childSVG); err != nil {
			t.Fatal(err)
		}
		if parentState != string(avatardomain.PayloadCompacted) ||
			len(fingerprint) != avatardomain.PerceptualContinuityFingerprintBytes ||
			childState != string(avatardomain.PayloadFull) || childSVG == nil {
			t.Fatalf("different-subject final-child rollback = parent:%s fingerprint:%d child:%s svg:%t",
				parentState, len(fingerprint), childState, childSVG != nil)
		}
	})

	t.Run("large selected history compacts transactionally", func(t *testing.T) {
		s := newScenario(t, "bounded-sources")
		const selectedHistory = 257
		for version := int64(1); version <= selectedHistory; version++ {
			insertVersion(t, s, version, 0, s.pack, PrincipalAgent, s.agent.ID,
				s.pack.References[0].SVG, 50_000)
		}
		insertVersion(t, s, selectedHistory+1, 1, s.pack, PrincipalAgent,
			s.agent.ID, s.pack.References[0].SVG, 0)
		plan, err := compact(t, s, 1)
		if err != nil {
			t.Fatalf("large-history compaction: %v", err)
		}
		if plan.count != selectedHistory || len(plan.versions) != selectedHistory ||
			plan.versions[0] != 1 || plan.versions[len(plan.versions)-1] != selectedHistory ||
			plan.retainedCount != 1 {
			t.Fatalf("large-history plan = %#v", plan)
		}
		var full, compacted int
		var retainedBytes int64
		var fingerprint []byte
		var childState string
		if err := st.pool.QueryRow(ctx, `
			SELECT COUNT(*) FILTER (WHERE payload_state='full'),
			       COUNT(*) FILTER (WHERE payload_state='compacted'),
			       COALESCE(SUM(CASE WHEN payload_state='full' THEN payload_bytes ELSE 0 END),0) +
			       COALESCE(SUM(octet_length(continuity_fingerprint)),0)
			  FROM agent_avatar_versions WHERE agent_id=$1`, s.agent.ID).
			Scan(&full, &compacted, &retainedBytes); err != nil {
			t.Fatal(err)
		}
		if err := st.pool.QueryRow(ctx, `
			SELECT parent.continuity_fingerprint, child.payload_state
			  FROM agent_avatar_versions parent
			  JOIN agent_avatar_versions child
			    ON child.agent_id=parent.agent_id AND child.version=$2
			 WHERE parent.agent_id=$1 AND parent.version=1`, s.agent.ID,
			selectedHistory+1).Scan(&fingerprint, &childState); err != nil {
			t.Fatal(err)
		}
		if full != 1 || compacted != selectedHistory ||
			retainedBytes != plan.retainedBytes ||
			len(fingerprint) != avatardomain.PerceptualContinuityFingerprintBytes ||
			childState != string(avatardomain.PayloadFull) {
			t.Fatalf("large-history committed state = full:%d compacted:%d bytes:%d/%d fingerprint:%d child:%s",
				full, compacted, retainedBytes, plan.retainedBytes, len(fingerprint), childState)
		}
		if err := avatardomain.ValidatePerceptualContinuityFingerprintForStyle(
			fingerprint, s.pack); err != nil {
			t.Fatalf("large-history boundary fingerprint: %v", err)
		}
	})

	t.Run("legacy occluding child refuses irreversible parent compaction", func(t *testing.T) {
		s := newScenario(t, "occlusion")
		parentSVG := s.pack.References[0].SVG
		childSVG := strings.Replace(parentSVG,
			`<g id="experience" data-layer="experience"></g>`,
			`<g id="experience" data-layer="experience"><circle cx="256" cy="230" r="136" fill="#F7FAFC"></circle></g>`, 1)
		insertVersion(t, s, 1, 0, s.pack, PrincipalAgent, s.agent.ID,
			parentSVG, 50_000)
		insertVersion(t, s, 2, 1, s.pack, PrincipalAgent, s.agent.ID,
			childSVG, 0)
		if _, err := compact(t, s, 1); !errors.Is(err, ErrAvatarConflict) {
			t.Fatalf("occluding legacy child compaction error = %v, want conflict", err)
		}
		var state string
		var svg *string
		var fingerprint []byte
		if err := st.pool.QueryRow(ctx, `
			SELECT payload_state, svg, continuity_fingerprint
			  FROM agent_avatar_versions WHERE agent_id=$1 AND version=1`, s.agent.ID).
			Scan(&state, &svg, &fingerprint); err != nil {
			t.Fatal(err)
		}
		if state != string(avatardomain.PayloadFull) || svg == nil || *svg != parentSVG ||
			len(fingerprint) != 0 {
			t.Fatalf("failed-closed parent = state:%s svg:%v fingerprint:%d",
				state, svg != nil, len(fingerprint))
		}
	})

	t.Run("corrupt final child refuses compacted boundary pruning", func(t *testing.T) {
		s := newScenario(t, "corrupt-final-child")
		parentSVG := s.pack.References[0].SVG
		insertVersion(t, s, 1, 0, s.pack, PrincipalAgent, s.agent.ID,
			parentSVG, 50_000)
		insertVersion(t, s, 2, 1, s.pack, PrincipalAgent, s.agent.ID,
			parentSVG, 0)
		if plan, err := compact(t, s, 1); err != nil ||
			len(plan.versions) != 1 || plan.versions[0] != 1 {
			t.Fatalf("prepare compacted boundary = %#v / %v", plan, err)
		}
		childSVG := strings.Replace(parentSVG,
			`<g id="experience" data-layer="experience"></g>`,
			`<g id="experience" data-layer="experience"><circle cx="256" cy="230" r="136" fill="#F7FAFC"></circle></g>`, 1)
		digest := sha256.Sum256([]byte(childSVG))
		if _, err := st.pool.Exec(ctx, `
			UPDATE agent_avatar_versions
			   SET svg=$2, svg_sha256=$3
			 WHERE agent_id=$1 AND version=2`, s.agent.ID, childSVG,
			hex.EncodeToString(digest[:])); err != nil {
			t.Fatal(err)
		}
		if _, err := compact(t, s, 0); !errors.Is(err, ErrAvatarConflict) {
			t.Fatalf("corrupt final-child compaction error = %v, want conflict", err)
		}
		var state string
		var retainedSVG *string
		var fingerprintBytes int
		if err := st.pool.QueryRow(ctx, `
			SELECT child.payload_state, child.svg,
			       octet_length(parent.continuity_fingerprint)
			  FROM agent_avatar_versions child
			  JOIN agent_avatar_versions parent
			    ON parent.agent_id=child.agent_id AND parent.version=1
			 WHERE child.agent_id=$1 AND child.version=2`, s.agent.ID).
			Scan(&state, &retainedSVG, &fingerprintBytes); err != nil {
			t.Fatal(err)
		}
		if state != string(avatardomain.PayloadFull) || retainedSVG == nil ||
			*retainedSVG != childSVG ||
			fingerprintBytes != avatardomain.PerceptualContinuityFingerprintBytes {
			t.Fatalf("failed-closed child = state:%s svg:%v fingerprint:%d",
				state, retainedSVG != nil, fingerprintBytes)
		}
	})

	t.Run("corrupt fingerprint refuses final child compaction", func(t *testing.T) {
		s := newScenario(t, "corrupt-fingerprint")
		parentSVG := s.pack.References[0].SVG
		insertVersion(t, s, 1, 0, s.pack, PrincipalAgent, s.agent.ID,
			parentSVG, 50_000)
		insertVersion(t, s, 2, 1, s.pack, PrincipalAgent, s.agent.ID,
			parentSVG, 0)
		if plan, err := compact(t, s, 1); err != nil ||
			len(plan.versions) != 1 || plan.versions[0] != 1 {
			t.Fatalf("prepare compacted boundary = %#v / %v", plan, err)
		}
		var corrupt []byte
		if err := st.pool.QueryRow(ctx, `
			SELECT continuity_fingerprint FROM agent_avatar_versions
			 WHERE agent_id=$1 AND version=1`, s.agent.ID).Scan(&corrupt); err != nil {
			t.Fatal(err)
		}
		corrupt[len(corrupt)-1] ^= 0xff
		if _, err := st.pool.Exec(ctx, `
			UPDATE agent_avatar_versions SET continuity_fingerprint=$2
			 WHERE agent_id=$1 AND version=1`, s.agent.ID, corrupt); err != nil {
			t.Fatal(err)
		}
		if _, err := compact(t, s, 0); !errors.Is(err, ErrAvatarConflict) {
			t.Fatalf("corrupt-fingerprint compaction error = %v, want conflict", err)
		}
		var state string
		var retainedSVG *string
		var retainedFingerprint []byte
		if err := st.pool.QueryRow(ctx, `
			SELECT child.payload_state, child.svg, parent.continuity_fingerprint
			  FROM agent_avatar_versions child
			  JOIN agent_avatar_versions parent
			    ON parent.agent_id=child.agent_id AND parent.version=1
			 WHERE child.agent_id=$1 AND child.version=2`, s.agent.ID).
			Scan(&state, &retainedSVG, &retainedFingerprint); err != nil {
			t.Fatal(err)
		}
		if state != string(avatardomain.PayloadFull) || retainedSVG == nil ||
			*retainedSVG != parentSVG || !bytes.Equal(retainedFingerprint, corrupt) {
			t.Fatalf("failed-closed corrupt fingerprint = state:%s svg:%v retained:%t",
				state, retainedSVG != nil, bytes.Equal(retainedFingerprint, corrupt))
		}
	})

	t.Run("missing fingerprint refuses final child compaction", func(t *testing.T) {
		s := newScenario(t, "missing-fingerprint")
		parentSVG := s.pack.References[0].SVG
		insertVersion(t, s, 1, 0, s.pack, PrincipalAgent, s.agent.ID,
			parentSVG, 50_000)
		insertVersion(t, s, 2, 1, s.pack, PrincipalAgent, s.agent.ID,
			parentSVG, 0)
		if plan, err := compact(t, s, 1); err != nil ||
			len(plan.versions) != 1 || plan.versions[0] != 1 {
			t.Fatalf("prepare compacted boundary = %#v / %v", plan, err)
		}
		if _, err := st.pool.Exec(ctx, `
			UPDATE agent_avatar_versions SET continuity_fingerprint=NULL
			 WHERE agent_id=$1 AND version=1`, s.agent.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := compact(t, s, 0); !errors.Is(err, ErrAvatarConflict) {
			t.Fatalf("missing-fingerprint compaction error = %v, want conflict", err)
		}
		var parentState, childState string
		var fingerprint []byte
		var childSVG *string
		if err := st.pool.QueryRow(ctx, `
			SELECT parent.payload_state, parent.continuity_fingerprint,
			       child.payload_state, child.svg
			  FROM agent_avatar_versions parent
			  JOIN agent_avatar_versions child
			    ON child.agent_id=parent.agent_id AND child.version=2
			 WHERE parent.agent_id=$1 AND parent.version=1`, s.agent.ID).
			Scan(&parentState, &fingerprint, &childState, &childSVG); err != nil {
			t.Fatal(err)
		}
		if parentState != string(avatardomain.PayloadCompacted) || len(fingerprint) != 0 ||
			childState != string(avatardomain.PayloadFull) || childSVG == nil ||
			*childSVG != parentSVG {
			t.Fatalf("missing-fingerprint rollback = parent:%s fingerprint:%d child:%s svg:%t",
				parentState, len(fingerprint), childState, childSVG != nil)
		}
	})
}

func avatarPayloadStateSnapshot(ctx context.Context, t *testing.T, st *Store, agentID string) []string {
	t.Helper()
	rows, err := st.pool.Query(ctx, `
		SELECT version, payload_state, COALESCE(payload_compacted_at::text,'')
		  FROM agent_avatar_versions WHERE agent_id=$1 ORDER BY version`, agentID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	result := make([]string, 0)
	for rows.Next() {
		var version int64
		var state, compactedAt string
		if err := rows.Scan(&version, &state, &compactedAt); err != nil {
			t.Fatal(err)
		}
		result = append(result, fmt.Sprintf("%d:%s:%s", version, state, compactedAt))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}
