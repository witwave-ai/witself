package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	avatardomain "github.com/witwave-ai/witself/internal/avatar"
)

func drainAvatarStyleRolloutsForTest(t *testing.T, ctx context.Context, st *Store, batchSize int) {
	t.Helper()
	for attempts := 0; attempts < 1000; attempts++ {
		result, err := st.ProcessAvatarStyleRolloutBatch(ctx, batchSize)
		if err != nil {
			t.Fatal(err)
		}
		if !result.Found {
			return
		}
	}
	t.Fatal("avatar style rollouts did not drain within 1000 bounded batches")
}

func TestAvatarStyleRolloutBoundedFencedAndLifecycleSafePostgres(t *testing.T) {
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
		fmt.Sprintf("avatar-rollout-%d@witwave.ai", time.Now().UnixNano()),
		"avatar rollout", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = deleteAccountForIntegrationTest(context.Background(), st, provisioned.AccountID) }()
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate = %t / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "rollout")
	if err != nil {
		t.Fatal(err)
	}
	agents := make([]Agent, 5)
	for i := range agents {
		agents[i], err = st.CreateAgent(ctx, provisioned.AccountID, realm.ID, fmt.Sprintf("rollout-%d", i))
		if err != nil {
			t.Fatal(err)
		}
	}
	operator := Principal{Kind: PrincipalOperator, ID: provisioned.OperatorID,
		AccountID: provisioned.AccountID, AccountStatus: "active"}
	principal := func(agent Agent) Principal {
		return Principal{Kind: PrincipalAgent, ID: agent.ID, AccountID: provisioned.AccountID,
			RealmID: realm.ID, AgentName: agent.Name, AccountStatus: "active"}
	}
	style, err := st.GetRealmAvatarStyle(ctx, principal(agents[0]), "")
	if err != nil {
		t.Fatal(err)
	}
	humanSVG := style.StylePack.References[0].SVG
	for _, reference := range style.StylePack.References {
		if reference.SubjectForm == avatardomain.SubjectHuman {
			humanSVG = reference.SVG
			break
		}
	}
	propose := func(agent Agent, revision int64, key string) AvatarMutationResult {
		t.Helper()
		result, err := st.ProposeAvatar(ctx, principal(agent), ProposeAvatarInput{
			ExpectedProfileRevision: revision,
			StylePackID:             style.StylePack.ID, StylePackVersion: style.StylePack.Version,
			SubjectForm: avatardomain.SubjectHuman, Description: "A rollout safety portrait.",
			VisualSpec: []byte(`{"test":"rollout"}`), SVG: humanSVG, IdempotencyKey: key,
		})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	pending := propose(agents[0], 1, "rollout-pending")
	activeProposal := propose(agents[1], 1, "rollout-active-proposal")
	active, err := st.ActivateAvatar(ctx, principal(agents[1]), ActivateAvatarInput{
		Version:                 activeProposal.Avatar.Profile.ProposedVersion,
		ExpectedProfileRevision: activeProposal.Avatar.Profile.ProfileRevision,
		IdempotencyKey:          "rollout-active-activation",
	})
	if err != nil {
		t.Fatal(err)
	}

	beforeRevision := map[string]int64{}
	for _, agent := range agents {
		view, err := st.GetAvatar(ctx, principal(agent))
		if err != nil {
			t.Fatal(err)
		}
		beforeRevision[agent.ID] = view.Profile.ProfileRevision
	}
	styleV2 := publishAvatarStyleForTest(t, ctx, st, operator, realm.ID, 1, 2, "rollout-style-v2")
	if styleV2.Style.Rollout == nil || styleV2.Style.Rollout.Status != "pending" ||
		styleV2.Style.Rollout.TargetProfileCount != nil ||
		styleV2.Style.Rollout.ProcessedProfileCount != 0 ||
		!styleV2.Style.Rollout.CreatedAt.Equal(styleV2.Style.Rollout.UpdatedAt) {
		t.Fatalf("new rollout = %#v", styleV2.Style.Rollout)
	}
	agentStyle, err := st.GetRealmAvatarStyle(ctx, principal(agents[0]), "")
	if err != nil || agentStyle.Rollout != nil {
		t.Fatalf("agent style exposed operator rollout telemetry = %#v / %v", agentStyle.Rollout, err)
	}
	unchanged, err := st.GetAvatar(ctx, principal(agents[0]))
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Profile.Style.Version != 1 || unchanged.Profile.ProposedVersion != pending.Avatar.Profile.ProposedVersion {
		t.Fatalf("publish rewrote a profile synchronously: %#v", unchanged.Profile)
	}

	firstBatch, err := st.ProcessAvatarStyleRolloutBatch(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !firstBatch.Found || firstBatch.ProcessedProfiles != 2 || firstBatch.Completed {
		t.Fatalf("first bounded batch = %#v", firstBatch)
	}
	progress, err := st.GetRealmAvatarStyle(ctx, operator, realm.ID)
	if err != nil {
		t.Fatal(err)
	}
	if progress.Rollout == nil || progress.Rollout.Status != "running" ||
		progress.Rollout.ProcessedProfileCount != 2 || progress.Rollout.BatchCount != 1 ||
		progress.Rollout.LastBatchSize != 2 || progress.Rollout.StartedAt == nil ||
		!progress.Rollout.StartedAt.Equal(progress.Rollout.UpdatedAt) {
		t.Fatalf("running progress = %#v", progress.Rollout)
	}

	// Simulate several server replicas ticking at once. The job row fence lets
	// at most one transaction own each batch; the final exact revisions prove
	// no profile was projected twice for the same selected style.
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := st.ProcessAvatarStyleRolloutBatch(ctx, 2)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	drainAvatarStyleRolloutsForTest(t, ctx, st, 2)
	completed, err := st.GetRealmAvatarStyle(ctx, operator, realm.ID)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Rollout == nil || completed.Rollout.Status != "completed" ||
		completed.Rollout.ProcessedProfileCount != int64(len(agents)) ||
		completed.Rollout.TargetProfileCount == nil ||
		*completed.Rollout.TargetProfileCount != int64(len(agents)) ||
		completed.Rollout.LastBatchSize > 2 || completed.Rollout.CompletedAt == nil ||
		!completed.Rollout.CompletedAt.Equal(completed.Rollout.UpdatedAt) {
		t.Fatalf("completed rollout = %#v", completed.Rollout)
	}
	for _, agent := range agents {
		view, err := st.GetAvatar(ctx, principal(agent))
		if err != nil {
			t.Fatal(err)
		}
		if view.Profile.Style.Version != 2 || view.Profile.ProfileRevision != beforeRevision[agent.ID]+1 {
			t.Fatalf("agent %s projected more or less than once: %#v", agent.ID, view.Profile)
		}
	}
	pendingAfter, _ := st.GetAvatar(ctx, principal(agents[0]))
	if pendingAfter.Profile.ProposedVersion != 0 || pendingAfter.Proposed != nil ||
		pendingAfter.Profile.LatestVersion != pending.Avatar.Profile.LatestVersion ||
		pendingAfter.Profile.Status != avatardomain.StatusGenerationDue {
		t.Fatalf("stale pending safety = %#v", pendingAfter)
	}
	activeAfter, _ := st.GetAvatar(ctx, principal(agents[1]))
	if activeAfter.Profile.ActiveVersion != active.Avatar.Profile.ActiveVersion ||
		activeAfter.Active == nil || activeAfter.Active.Style.Version != 1 ||
		activeAfter.Profile.Status != avatardomain.StatusEvolutionDue {
		t.Fatalf("active immutable safety = %#v", activeAfter)
	}
	var completionEvents int
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM account_events
		 WHERE account_id=$1 AND verb=$2 AND metadata->>'realm_id'=$3`,
		provisioned.AccountID, VerbAvatarStyleRolloutCompleted, realm.ID).Scan(&completionEvents); err != nil || completionEvents != 1 {
		t.Fatalf("completion events = %d / %v", completionEvents, err)
	}

	// A newer publish fences a partially processed job. A concurrently created
	// agent inherits the selected style directly, while a deleted target is
	// excluded from the final mismatch check and explains target-count slack.
	publishAvatarStyleForTest(t, ctx, st, operator, realm.ID, 2, 3, "rollout-style-v3")
	partialV3, err := st.ProcessAvatarStyleRolloutBatch(ctx, 2)
	if err != nil || partialV3.ProcessedProfiles != 2 {
		t.Fatalf("partial v3 = %#v / %v", partialV3, err)
	}
	styleV4 := publishAvatarStyleForTest(t, ctx, st, operator, realm.ID, 3, 4, "rollout-style-v4")
	if styleV4.Style.Rollout == nil || styleV4.Style.Rollout.TargetProfileCount != nil {
		t.Fatalf("v4 rollout = %#v", styleV4.Style.Rollout)
	}
	newAgent, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "created-during-rollout")
	if err != nil {
		t.Fatal(err)
	}
	newView, err := st.GetAvatar(ctx, principal(newAgent))
	if err != nil || newView.Profile.Style.Version != 4 || newView.Profile.ProfileRevision != 1 {
		t.Fatalf("new agent inherited current style = %#v / %v", newView.Profile, err)
	}
	if err := st.DeleteAgent(ctx, provisioned.AccountID, realm.ID, agents[4].ID); err != nil {
		t.Fatal(err)
	}
	drainAvatarStyleRolloutsForTest(t, ctx, st, 2)
	var v3Status string
	var v3SupersededStampAligned bool
	if err := st.pool.QueryRow(ctx, `
		SELECT status, updated_at=superseded_at FROM avatar_style_rollout_jobs
		 WHERE account_id=$1 AND realm_id=$2 AND style_revision=3`,
		provisioned.AccountID, realm.ID).Scan(&v3Status, &v3SupersededStampAligned); err != nil ||
		v3Status != "superseded" || !v3SupersededStampAligned {
		t.Fatalf("v3 status/stamp = %q/%t / %v", v3Status, v3SupersededStampAligned, err)
	}
	finalV4, err := st.GetRealmAvatarStyle(ctx, operator, realm.ID)
	if err != nil || finalV4.Rollout == nil || finalV4.Rollout.Status != "completed" ||
		finalV4.Rollout.TargetProfileCount == nil || *finalV4.Rollout.TargetProfileCount != 4 ||
		finalV4.Rollout.ProcessedProfileCount != 4 {
		t.Fatalf("v4 deleted-target completion = %#v / %v", finalV4.Rollout, err)
	}
	var supersedeEvents int
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM account_events
		 WHERE account_id=$1 AND verb=$2 AND metadata->>'realm_id'=$3
		   AND metadata->>'style_revision'='3'`, provisioned.AccountID,
		VerbAvatarStyleRolloutSuperseded, realm.ID).Scan(&supersedeEvents); err != nil || supersedeEvents != 1 {
		t.Fatalf("supersede events = %d / %v", supersedeEvents, err)
	}

	// Suspension is an exact write pause: discovery ignores the job until the
	// authority that suspended the account resumes it.
	publishAvatarStyleForTest(t, ctx, st, operator, realm.ID, 4, 5, "rollout-style-v5")
	if err := st.SuspendAccountSystem(ctx, provisioned.AccountID, "evacuation", "rollout pause"); err != nil {
		t.Fatal(err)
	}
	paused, err := st.ProcessAvatarStyleRolloutBatch(ctx, 2)
	if err != nil || paused.Found {
		t.Fatalf("suspended rollout attempt = %#v / %v", paused, err)
	}
	if err := st.ResumeAccountSystem(ctx, provisioned.AccountID, "evacuation"); err != nil {
		t.Fatal(err)
	}
	drainAvatarStyleRolloutsForTest(t, ctx, st, 2)

	// An empty realm can be deleted while its zero-target job is pending. The
	// worker still discovers that durable row and supersedes it value-free.
	emptyRealm, err := st.CreateRealm(ctx, provisioned.AccountID, "deleted-rollout")
	if err != nil {
		t.Fatal(err)
	}
	publishAvatarStyleForTest(t, ctx, st, operator, emptyRealm.ID, 1, 2, "deleted-realm-style-v2")
	if err := st.DeleteRealm(ctx, provisioned.AccountID, emptyRealm.ID); err != nil {
		t.Fatal(err)
	}
	deletedResult, err := st.ProcessAvatarStyleRolloutBatch(ctx, 2)
	if err != nil || !deletedResult.Found || !deletedResult.Superseded {
		t.Fatalf("deleted realm rollout = %#v / %v", deletedResult, err)
	}
	var deletedStatus string
	var deletedSupersededStampAligned bool
	if err := st.pool.QueryRow(ctx, `
		SELECT status, updated_at=superseded_at FROM avatar_style_rollout_jobs
		 WHERE account_id=$1 AND realm_id=$2 AND style_revision=2`,
		provisioned.AccountID, emptyRealm.ID).Scan(&deletedStatus, &deletedSupersededStampAligned); err != nil ||
		deletedStatus != "superseded" || !deletedSupersededStampAligned {
		t.Fatalf("deleted realm status/stamp = %q/%t / %v", deletedStatus, deletedSupersededStampAligned, err)
	}
}

func TestAvatarStyleRolloutPartialArchiveResumesAfterActivationPostgres(t *testing.T) {
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
		fmt.Sprintf("avatar-rollout-archive-%d@witwave.ai", time.Now().UnixNano()),
		"avatar rollout archive", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = deleteAccountForIntegrationTest(context.Background(), st, provisioned.AccountID) }()
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate = %t / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "archive")
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if _, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, fmt.Sprintf("archive-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	operator := Principal{Kind: PrincipalOperator, ID: provisioned.OperatorID,
		AccountID: provisioned.AccountID, AccountStatus: "active"}
	publishAvatarStyleForTest(t, ctx, st, operator, realm.ID, 1, 2, "archive-rollout-v2")
	partial, err := st.ProcessAvatarStyleRolloutBatch(ctx, 1)
	if err != nil || partial.ProcessedProfiles != 1 || partial.Completed {
		t.Fatalf("partial archive batch = %#v / %v", partial, err)
	}
	if err := st.SuspendAccountSystem(ctx, provisioned.AccountID, "evacuation", "partial rollout archive"); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := st.ExportAccount(ctx, provisioned.AccountID, "source-cell", "test", &archive); err != nil {
		t.Fatal(err)
	}
	_, rows := readAvatarArchiveRows(t, archive.Bytes(), SchemaVersion())
	if len(rows["avatar_style_rollout_jobs"]) != 1 {
		t.Fatalf("archive rollout rows = %d, want 1", len(rows["avatar_style_rollout_jobs"]))
	}
	if err := deleteAccountForIntegrationTest(ctx, st, provisioned.AccountID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ImportAccount(ctx, provisioned.AccountID, bytes.NewReader(archive.Bytes())); err != nil {
		t.Fatal(err)
	}
	restored, err := st.GetRealmAvatarStyle(ctx, operator, realm.ID)
	if err != nil || restored.Rollout == nil || restored.Rollout.Status != "running" ||
		restored.Rollout.ProcessedProfileCount != 1 || restored.Rollout.BatchCount != 1 {
		t.Fatalf("restored rollout = %#v / %v", restored.Rollout, err)
	}
	paused, err := st.ProcessAvatarStyleRolloutBatch(ctx, 1)
	if err != nil || paused.Found {
		t.Fatalf("imported suspended job ran = %#v / %v", paused, err)
	}
	if err := st.ResumeAccountSystem(ctx, provisioned.AccountID, "evacuation"); err != nil {
		t.Fatal(err)
	}
	drainAvatarStyleRolloutsForTest(t, ctx, st, 1)
	resumed, err := st.GetRealmAvatarStyle(ctx, operator, realm.ID)
	if err != nil || resumed.Rollout == nil || resumed.Rollout.Status != "completed" ||
		resumed.Rollout.ProcessedProfileCount != 3 || resumed.Rollout.TargetProfileCount == nil ||
		*resumed.Rollout.TargetProfileCount != 3 || resumed.Rollout.BatchCount != 4 {
		t.Fatalf("resumed rollout = %#v / %v", resumed.Rollout, err)
	}
}

func TestAvatarStylePublishAndConcurrentCreateCannotLoseTargetPostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
		fmt.Sprintf("avatar-rollout-create-%d@witwave.ai", time.Now().UnixNano()),
		"avatar rollout create", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = deleteAccountForIntegrationTest(context.Background(), st, provisioned.AccountID) }()
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate = %t / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "create-race")
	if err != nil {
		t.Fatal(err)
	}
	operator := Principal{Kind: PrincipalOperator, ID: provisioned.OperatorID,
		AccountID: provisioned.AccountID, AccountStatus: "active"}
	start := make(chan struct{})
	var created Agent
	var createErr, publishErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		created, createErr = st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "concurrent-create")
	}()
	go func() {
		defer wg.Done()
		<-start
		_, publishErr = st.SetRealmAvatarStyle(ctx, operator, realm.ID, CreateAvatarStyleVersionInput{
			ExpectedStyleRevision: 1, StylePack: avatarStylePackVersionForTest(2),
			IdempotencyKey: "concurrent-create-style-v2",
		})
	}()
	close(start)
	wg.Wait()
	if createErr != nil || publishErr != nil {
		t.Fatalf("create/publish = %v / %v", createErr, publishErr)
	}
	drainAvatarStyleRolloutsForTest(t, ctx, st, 1)
	view, err := st.GetAvatar(ctx, Principal{Kind: PrincipalAgent, ID: created.ID,
		AccountID: provisioned.AccountID, RealmID: realm.ID, AgentName: created.Name,
		AccountStatus: "active"})
	if err != nil || view.Profile.Style.Version != 2 {
		t.Fatalf("concurrent create final style = %#v / %v", view.Profile, err)
	}
}

func TestAvatarStyleRolloutSchedulerIsFairAndSkipsLockedOldestPostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	provisioned, operator := provisionActiveRolloutAccountForTest(t, ctx, st, "scheduler")
	defer func() { _ = deleteAccountForIntegrationTest(context.Background(), st, provisioned.AccountID) }()
	realmA := createRolloutRealmWithAgentsForTest(t, ctx, st, provisioned.AccountID, "fair-a", 3)
	realmB := createRolloutRealmWithAgentsForTest(t, ctx, st, provisioned.AccountID, "fair-b", 3)
	publishAvatarStyleForTest(t, ctx, st, operator, realmA.ID, 1, 2, "fair-a-v2")
	publishAvatarStyleForTest(t, ctx, st, operator, realmB.ID, 1, 2, "fair-b-v2")
	if _, err := st.pool.Exec(ctx, `
		UPDATE avatar_style_rollout_jobs
		   SET created_at=CASE realm_id WHEN $2 THEN statement_timestamp()-interval '2 minutes'
		                                ELSE statement_timestamp()-interval '1 minute' END,
		       updated_at=CASE realm_id WHEN $2 THEN statement_timestamp()-interval '2 minutes'
		                                ELSE statement_timestamp()-interval '1 minute' END
		 WHERE account_id=$1 AND realm_id IN ($2,$3)`, provisioned.AccountID,
		realmA.ID, realmB.ID); err != nil {
		t.Fatal(err)
	}

	// A direct or buggy writer cannot create a second open job for a realm.
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO avatar_style_rollout_jobs
		       (account_id,realm_id,style_revision,style_pack_id,style_pack_version)
		SELECT account_id,realm_id,style_revision+100,style_pack_id,style_pack_version
		  FROM avatar_style_rollout_jobs
		 WHERE account_id=$1 AND realm_id=$2 AND status='pending'`,
		provisioned.AccountID, realmA.ID); err == nil {
		t.Fatal("database accepted two open avatar style jobs for one realm")
	}

	want := []string{realmA.ID, realmB.ID, realmA.ID, realmB.ID}
	for i, wantRealm := range want {
		result, err := st.ProcessAvatarStyleRolloutBatch(ctx, 1)
		if err != nil || !result.Found || result.RealmID != wantRealm || result.ProcessedProfiles != 1 {
			t.Fatalf("fair batch %d = %#v / %v, want realm %s", i, result, err, wantRealm)
		}
	}

	// Lock the now-oldest A job. Candidate fallback must advance B instead of
	// letting one replica's job lock cap the whole cluster at one realm.
	lockTx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := lockTx.QueryRow(ctx, `
		SELECT style_revision FROM avatar_style_rollout_jobs
		 WHERE account_id=$1 AND realm_id=$2 AND status='running'
		 FOR UPDATE`, provisioned.AccountID, realmA.ID).Scan(new(int64)); err != nil {
		_ = lockTx.Rollback(ctx)
		t.Fatal(err)
	}
	lockedFallback, err := st.ProcessAvatarStyleRolloutBatch(ctx, 1)
	if err != nil || !lockedFallback.Found || lockedFallback.RealmID != realmB.ID {
		_ = lockTx.Rollback(ctx)
		t.Fatalf("locked-oldest fallback = %#v / %v, want realm %s", lockedFallback, err, realmB.ID)
	}
	if err := lockTx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	drainAvatarStyleRolloutsForTest(t, ctx, st, 2)
}

func TestAvatarStyleRolloutTimeoutBackoffDoesNotStarveAnotherRealmPostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	provisioned, operator := provisionActiveRolloutAccountForTest(t, ctx, st, "timeout")
	defer func() { _ = deleteAccountForIntegrationTest(context.Background(), st, provisioned.AccountID) }()
	realmA := createRolloutRealmWithAgentsForTest(t, ctx, st, provisioned.AccountID, "timeout-a", 1)
	realmB := createRolloutRealmWithAgentsForTest(t, ctx, st, provisioned.AccountID, "timeout-b", 1)
	publishAvatarStyleForTest(t, ctx, st, operator, realmA.ID, 1, 2, "timeout-a-v2")
	publishAvatarStyleForTest(t, ctx, st, operator, realmB.ID, 1, 2, "timeout-b-v2")
	if _, err := st.pool.Exec(ctx, `
		UPDATE avatar_style_rollout_jobs
		   SET created_at=CASE realm_id WHEN $2 THEN statement_timestamp()-interval '2 minutes'
		                                ELSE statement_timestamp()-interval '1 minute' END,
		       updated_at=CASE realm_id WHEN $2 THEN statement_timestamp()-interval '2 minutes'
		                                ELSE statement_timestamp()-interval '1 minute' END
		 WHERE account_id=$1 AND realm_id IN ($2,$3)`, provisioned.AccountID,
		realmA.ID, realmB.ID); err != nil {
		t.Fatal(err)
	}
	profileLock, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := profileLock.QueryRow(ctx, `
		SELECT agent_id FROM agent_avatar_profiles
		 WHERE account_id=$1 AND realm_id=$2 LIMIT 1 FOR UPDATE`,
		provisioned.AccountID, realmA.ID).Scan(new(string)); err != nil {
		_ = profileLock.Rollback(ctx)
		t.Fatal(err)
	}
	started := time.Now()
	result, err := st.processAvatarStyleRolloutBatch(ctx, 1, 250*time.Millisecond)
	if err != nil || !result.Found || result.RealmID != realmB.ID ||
		result.ProcessedProfiles != 1 || time.Since(started) > 2*time.Second {
		_ = profileLock.Rollback(ctx)
		t.Fatalf("timeout isolation = %#v / %v after %s", result, err, time.Since(started))
	}
	var failures int
	var retryAfter *time.Time
	var failureCode string
	if err := st.pool.QueryRow(ctx, `
		SELECT failure_count,retry_after,last_failure_code
		  FROM avatar_style_rollout_jobs
		 WHERE account_id=$1 AND realm_id=$2 AND status='pending'`,
		provisioned.AccountID, realmA.ID).Scan(&failures, &retryAfter, &failureCode); err != nil {
		_ = profileLock.Rollback(ctx)
		t.Fatal(err)
	}
	if failures != 1 || retryAfter == nil ||
		(failureCode != "lock_timeout" && failureCode != "statement_timeout") {
		_ = profileLock.Rollback(ctx)
		t.Fatalf("durable timeout backoff = failures:%d retry:%v code:%q", failures, retryAfter, failureCode)
	}
	if err := profileLock.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE avatar_style_rollout_jobs SET retry_after=statement_timestamp()-interval '1 second'
		 WHERE account_id=$1 AND realm_id=$2`, provisioned.AccountID, realmA.ID); err != nil {
		t.Fatal(err)
	}
	drainAvatarStyleRolloutsForTest(t, ctx, st, 1)
	var status string
	if err := st.pool.QueryRow(ctx, `
		SELECT status,failure_count,last_failure_code
		  FROM avatar_style_rollout_jobs WHERE account_id=$1 AND realm_id=$2`,
		provisioned.AccountID, realmA.ID).Scan(&status, &failures, &failureCode); err != nil {
		t.Fatal(err)
	}
	if status != "completed" || failures != 0 || failureCode != "" {
		t.Fatalf("recovered timeout job = status:%q failures:%d code:%q", status, failures, failureCode)
	}
}

func TestAvatarStyleRolloutCallerCancellationDoesNotRecordFailurePostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	provisioned, operator := provisionActiveRolloutAccountForTest(t, ctx, st, "caller-cancel")
	defer func() { _ = deleteAccountForIntegrationTest(context.Background(), st, provisioned.AccountID) }()
	realm := createRolloutRealmWithAgentsForTest(t, ctx, st, provisioned.AccountID, "caller-cancel", 1)
	publishAvatarStyleForTest(t, ctx, st, operator, realm.ID, 1, 2, "caller-cancel-v2")

	profileLock, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := profileLock.QueryRow(ctx, `
		SELECT agent_id FROM agent_avatar_profiles
		 WHERE account_id=$1 AND realm_id=$2 LIMIT 1 FOR UPDATE`,
		provisioned.AccountID, realm.ID).Scan(new(string)); err != nil {
		_ = profileLock.Rollback(ctx)
		t.Fatal(err)
	}
	callCtx, cancelCall := context.WithTimeout(ctx, 150*time.Millisecond)
	result, err := st.processAvatarStyleRolloutBatch(callCtx, 1, time.Second)
	cancelCall()
	if !errors.Is(err, context.DeadlineExceeded) || !result.Found {
		_ = profileLock.Rollback(ctx)
		t.Fatalf("cancelled batch = %#v / %v", result, err)
	}
	if err := profileLock.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var status, failureCode string
	var failures int
	var retryAfter *time.Time
	var processed, batches int64
	if err := st.pool.QueryRow(ctx, `
		SELECT status,failure_count,retry_after,last_failure_code,
		       processed_profile_count,batch_count
		  FROM avatar_style_rollout_jobs
		 WHERE account_id=$1 AND realm_id=$2`, provisioned.AccountID, realm.ID).Scan(
		&status, &failures, &retryAfter, &failureCode, &processed, &batches); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || failures != 0 || retryAfter != nil || failureCode != "" ||
		processed != 0 || batches != 0 {
		t.Fatalf("caller cancellation mutated job = status:%q failures:%d retry:%v code:%q processed:%d batches:%d",
			status, failures, retryAfter, failureCode, processed, batches)
	}
	drainAvatarStyleRolloutsForTest(t, ctx, st, 1)
	style, err := st.GetRealmAvatarStyle(ctx, operator, realm.ID)
	if err != nil || style.Rollout == nil || style.Rollout.Status != "completed" {
		t.Fatalf("rollout did not resume after cancellation = %#v / %v", style.Rollout, err)
	}
}

func TestAvatarStyleRolloutAccountCloseSupersedesOpenJobsPostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	provisioned, operator := provisionActiveRolloutAccountForTest(t, ctx, st, "account-close")
	defer func() { _ = deleteAccountForIntegrationTest(context.Background(), st, provisioned.AccountID) }()
	realmA := createRolloutRealmWithAgentsForTest(t, ctx, st, provisioned.AccountID, "close-a", 2)
	realmB := createRolloutRealmWithAgentsForTest(t, ctx, st, provisioned.AccountID, "close-b", 1)
	publishAvatarStyleForTest(t, ctx, st, operator, realmA.ID, 1, 2, "close-a-v2")
	publishAvatarStyleForTest(t, ctx, st, operator, realmB.ID, 1, 2, "close-b-v2")
	partial, err := st.ProcessAvatarStyleRolloutBatch(ctx, 1)
	if err != nil || !partial.Found {
		t.Fatalf("partial pre-close batch = %#v / %v", partial, err)
	}
	if err := st.CloseAccount(ctx, provisioned.AccountID, provisioned.OperatorID, "rollout close test"); err != nil {
		t.Fatal(err)
	}
	var openJobs, supersededJobs, alignedTerminalTargets int
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE status IN ('pending','running')),
		       count(*) FILTER (WHERE status='superseded'),
		       count(*) FILTER (WHERE status='superseded' AND
		         target_profile_count=processed_profile_count AND
		         superseded_at=updated_at AND failure_count=0 AND
		         retry_after IS NULL AND last_failure_code='')
		  FROM avatar_style_rollout_jobs WHERE account_id=$1`,
		provisioned.AccountID).Scan(&openJobs, &supersededJobs, &alignedTerminalTargets); err != nil {
		t.Fatal(err)
	}
	if openJobs != 0 || supersededJobs != 2 || alignedTerminalTargets != 2 {
		t.Fatalf("closed rollout jobs = open:%d superseded:%d aligned:%d",
			openJobs, supersededJobs, alignedTerminalTargets)
	}
	result, err := st.ProcessAvatarStyleRolloutBatch(ctx, 1)
	if err != nil || result.Found {
		t.Fatalf("closed account remained discoverable = %#v / %v", result, err)
	}
	var eventsBefore, eventsAfter int
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM account_events
		 WHERE account_id=$1 AND verb=$2 AND metadata->>'reason'='account_closed'`,
		provisioned.AccountID, VerbAvatarStyleRolloutSuperseded).Scan(&eventsBefore); err != nil {
		t.Fatal(err)
	}
	if err := st.CloseAccount(ctx, provisioned.AccountID, provisioned.OperatorID, "repeat"); err != nil {
		t.Fatal(err)
	}
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM account_events
		 WHERE account_id=$1 AND verb=$2 AND metadata->>'reason'='account_closed'`,
		provisioned.AccountID, VerbAvatarStyleRolloutSuperseded).Scan(&eventsAfter); err != nil {
		t.Fatal(err)
	}
	if eventsBefore != 2 || eventsAfter != eventsBefore {
		t.Fatalf("close supersede events before/after replay = %d/%d", eventsBefore, eventsAfter)
	}
}

func TestAvatarStyleRolloutWorkerReconcilesLegacyClosedAccountJobPostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	provisioned, operator := provisionActiveRolloutAccountForTest(t, ctx, st, "legacy-close")
	defer func() { _ = deleteAccountForIntegrationTest(context.Background(), st, provisioned.AccountID) }()
	realm := createRolloutRealmWithAgentsForTest(t, ctx, st, provisioned.AccountID, "legacy-close", 1)
	publishAvatarStyleForTest(t, ctx, st, operator, realm.ID, 1, 2, "legacy-close-v2")
	// Simulate an older binary that knows the account lifecycle but predates
	// avatar rollout terminalization.
	if _, err := st.pool.Exec(ctx, `
		UPDATE accounts
		   SET status='closed',closed_at=statement_timestamp(),
		       closed_reason='legacy writer simulation'
		 WHERE id=$1`, provisioned.AccountID); err != nil {
		t.Fatal(err)
	}
	result, err := st.ProcessAvatarStyleRolloutBatch(ctx, 1)
	if err != nil || !result.Found || !result.Superseded || result.RealmID != realm.ID {
		t.Fatalf("legacy closed-account reconciliation = %#v / %v", result, err)
	}
	var status string
	var target *int64
	var processed int64
	if err := st.pool.QueryRow(ctx, `
		SELECT status,target_profile_count,processed_profile_count
		  FROM avatar_style_rollout_jobs
		 WHERE account_id=$1 AND realm_id=$2`, provisioned.AccountID, realm.ID).Scan(
		&status, &target, &processed); err != nil {
		t.Fatal(err)
	}
	if status != "superseded" || target == nil || *target != processed {
		t.Fatalf("legacy closed job = status:%q target:%v processed:%v", status, target, processed)
	}
}

func TestAvatarStyleRolloutLargeRealmUsesRevisionIndexPostgres(t *testing.T) {
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
	provisioned, operator := provisionActiveRolloutAccountForTest(t, ctx, st, "plan")
	defer func() { _ = deleteAccountForIntegrationTest(context.Background(), st, provisioned.AccountID) }()
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "large-plan")
	if err != nil {
		t.Fatal(err)
	}
	const profiles = 5000
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO agents (id,realm_id,name)
		SELECT $1 || '_agent_' || lpad(g::text,5,'0'), $1,
		       'plan-' || lpad(g::text,5,'0')
		  FROM generate_series(1,$2) g`, realm.ID, profiles); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO agent_avatar_profiles
		       (account_id,realm_id,agent_id,style_pack_id,style_pack_version,
		        style_revision,fallback_seed)
		SELECT $1,$2,a.id,ras.style_pack_id,ras.style_pack_version,ras.revision,a.id
		  FROM agents a
		  JOIN realm_avatar_styles ras ON ras.account_id=$1 AND ras.realm_id=$2
		 WHERE a.realm_id=$2`, provisioned.AccountID, realm.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `ANALYZE agent_avatar_profiles`); err != nil {
		t.Fatal(err)
	}
	published := publishAvatarStyleForTest(t, ctx, st, operator, realm.ID, 1, 2, "large-plan-v2")
	if published.Style.Rollout == nil || published.Style.Rollout.TargetProfileCount != nil {
		t.Fatalf("large publish synchronously finalized target count: %#v", published.Style.Rollout)
	}
	assertAvatarStyleRevisionIndexPlan(t, ctx, st, provisioned.AccountID, realm.ID, 2)
	for batch := 0; batch < 3; batch++ {
		result, err := st.ProcessAvatarStyleRolloutBatch(ctx, 100)
		if err != nil || result.RealmID != realm.ID || result.ProcessedProfiles != 100 || result.Completed {
			t.Fatalf("large batch %d = %#v / %v", batch, result, err)
		}
	}
	var projected int
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM agent_avatar_profiles
		 WHERE account_id=$1 AND realm_id=$2 AND style_revision=2`,
		provisioned.AccountID, realm.ID).Scan(&projected); err != nil {
		t.Fatal(err)
	}
	if projected != 300 {
		t.Fatalf("projected profiles after three batches = %d, want 300", projected)
	}
	assertAvatarStyleRevisionIndexPlan(t, ctx, st, provisioned.AccountID, realm.ID, 2)
}

func provisionActiveRolloutAccountForTest(
	t *testing.T,
	ctx context.Context,
	st *Store,
	label string,
) (ProvisionedAccount, Principal) {
	t.Helper()
	provisioned, err := st.ProvisionAccount(ctx,
		fmt.Sprintf("avatar-rollout-%s-%d@witwave.ai", label, time.Now().UnixNano()),
		"avatar rollout "+label, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate = %t / %v", activated, err)
	}
	return provisioned, Principal{Kind: PrincipalOperator, ID: provisioned.OperatorID,
		AccountID: provisioned.AccountID, AccountStatus: "active"}
}

func createRolloutRealmWithAgentsForTest(
	t *testing.T,
	ctx context.Context,
	st *Store,
	accountID, name string,
	count int,
) Realm {
	t.Helper()
	realm, err := st.CreateRealm(ctx, accountID, name)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < count; i++ {
		if _, err := st.CreateAgent(ctx, accountID, realm.ID, fmt.Sprintf("%s-%d", name, i)); err != nil {
			t.Fatal(err)
		}
	}
	return realm
}

func assertAvatarStyleRevisionIndexPlan(
	t *testing.T,
	ctx context.Context,
	st *Store,
	accountID, realmID string,
	desiredRevision int64,
) {
	t.Helper()
	var plan string
	if err := st.pool.QueryRow(ctx, `
		EXPLAIN (FORMAT JSON)
		SELECT p.account_id,p.realm_id,p.agent_id
		  FROM agent_avatar_profiles p
		 WHERE p.account_id=$1 AND p.realm_id=$2
		   AND COALESCE(p.style_revision,0) < $3
		 ORDER BY COALESCE(p.style_revision,0),p.agent_id
		 LIMIT 100`, accountID, realmID, desiredRevision).Scan(&plan); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, "agent_avatar_profiles_by_style_revision") ||
		strings.Contains(plan, `"Node Type": "Sort"`) {
		t.Fatalf("rollout target plan does not use the ordered style-revision index: %s", plan)
	}
}

func publishAvatarStyleForTest(
	t *testing.T,
	ctx context.Context,
	st *Store,
	operator Principal,
	realmID string,
	expectedRevision int64,
	version int,
	key string,
) AvatarStyleMutationResult {
	t.Helper()
	result, err := st.SetRealmAvatarStyle(ctx, operator, realmID, CreateAvatarStyleVersionInput{
		ExpectedStyleRevision: expectedRevision,
		StylePack:             avatarStylePackVersionForTest(version),
		IdempotencyKey:        key,
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func avatarStylePackVersionForTest(version int) avatardomain.StylePack {
	pack := avatardomain.BuiltInFlatVectorStylePack()
	pack.Version = version
	pack.Description = fmt.Sprintf("Avatar rollout integration style version %d.", version)
	return pack
}
