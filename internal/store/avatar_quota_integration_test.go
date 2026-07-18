package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	avatardomain "github.com/witwave-ai/witself/internal/avatar"
)

func TestAvatarPayloadQuotaCompactionLifecyclePostgres(t *testing.T) {
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

	quotaInput := UpdateAvatarQuotaInput{
		RetainedPayloadCountLimit: AvatarMinRetainedPayloadCountLimit,
		RetainedPayloadByteLimit:  AvatarMaxRetainedPayloadByteLimit,
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
	assertCompactionEvents(3, "")
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
