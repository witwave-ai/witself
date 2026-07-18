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

func TestAvatarResetLineageLifecycleAndGuardsPostgres(t *testing.T) {
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
		fmt.Sprintf("avatar-reset-%d@witwave.ai", time.Now().UnixNano()),
		"avatar reset", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = deleteAccountForIntegrationTest(context.Background(), st, provisioned.AccountID) }()
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate account = %t / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "reset")
	if err != nil {
		t.Fatal(err)
	}
	operator := Principal{Kind: PrincipalOperator, ID: provisioned.OperatorID,
		AccountID: provisioned.AccountID, AccountStatus: "active"}
	style, err := st.GetRealmAvatarStyle(ctx, operator, realm.ID)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("active and pending lineage reset", func(t *testing.T) {
		agent := createAvatarResetTestAgent(ctx, t, st, provisioned.AccountID, realm.ID, "reset-main")
		initial, err := st.GetAvatar(ctx, agent)
		if err != nil {
			t.Fatal(err)
		}
		if initial.Profile.LineageGeneration != 1 || initial.Active == nil ||
			initial.Active.LineageGeneration != 1 {
			t.Fatalf("initial lineage = %#v", initial)
		}
		if _, err := st.ResetAvatar(ctx, agent, ResetAvatarInput{
			ExpectedProfileRevision: 1, IdempotencyKey: "reset-empty",
		}); !errors.Is(err, ErrAvatarConflict) {
			t.Fatalf("empty-lineage reset = %v, want conflict", err)
		}

		proposed1 := proposeAvatarResetVersion(ctx, t, st, agent, style.StylePack,
			1, 0, "reset-main-propose-1")
		active1, err := st.ActivateAvatar(ctx, agent, ActivateAvatarInput{
			Version:                 proposed1.Avatar.Profile.ProposedVersion,
			ExpectedProfileRevision: proposed1.Avatar.Profile.ProfileRevision,
			IdempotencyKey:          "reset-main-activate-1",
		})
		if err != nil {
			t.Fatal(err)
		}
		proposed2 := proposeAvatarResetVersion(ctx, t, st, agent, style.StylePack,
			active1.Avatar.Profile.ProfileRevision, active1.Avatar.Profile.ActiveVersion,
			"reset-main-propose-2")
		if proposed2.Avatar.Profile.LatestVersion != 2 ||
			proposed2.Avatar.Proposed == nil || proposed2.Avatar.Proposed.LineageGeneration != 1 {
			t.Fatalf("pending evolution = %#v", proposed2.Avatar)
		}

		resetInput := ResetAvatarInput{
			ExpectedProfileRevision: proposed2.Avatar.Profile.ProfileRevision,
			ReasonCode:              "identity_restart",
			IdempotencyKey:          "reset-main-lineage-1",
		}
		reset, err := st.ResetAvatar(ctx, agent, resetInput)
		if err != nil {
			t.Fatal(err)
		}
		if reset.Receipt.Operation != "reset" || reset.Receipt.Replayed ||
			reset.Receipt.ResultVersion != 0 || reset.Receipt.ResultLineageGeneration != 2 {
			t.Fatalf("reset receipt = %#v", reset.Receipt)
		}
		profile := reset.Avatar.Profile
		if profile.LineageGeneration != 2 || profile.ProfileRevision != 5 ||
			profile.LatestVersion != 2 || profile.ActiveVersion != 0 ||
			profile.ProposedVersion != 0 || profile.Status != avatardomain.StatusGenerationDue ||
			profile.SubjectForm != avatardomain.SubjectHuman || profile.AttemptCount != 0 ||
			profile.RetryAfter != nil || profile.FailureCode != "" || reset.Avatar.Proposed != nil {
			t.Fatalf("reset profile = %#v", reset.Avatar)
		}
		if reset.Avatar.Active == nil || reset.Avatar.Active.Version != 0 ||
			reset.Avatar.Active.LineageGeneration != 2 ||
			reset.Avatar.Active.ID == initial.Active.ID ||
			reset.Avatar.Active.SVG != initial.Active.SVG ||
			!reset.Avatar.Active.ProposedAt.After(initial.Active.ProposedAt) {
			t.Fatalf("reset placeholder = initial:%#v reset:%#v", initial.Active, reset.Avatar.Active)
		}

		var sequence, retiredLineage, newLineage int64
		var retiredActive, retiredProposed *int64
		var resetByKind, resetByID, reasonCode string
		if err := st.pool.QueryRow(ctx, `
			SELECT sequence, retired_lineage_generation, new_lineage_generation,
			       retired_active_version, retired_proposed_version,
			       reset_by_kind, reset_by_id, reason_code
			  FROM agent_avatar_resets
			 WHERE account_id=$1 AND realm_id=$2 AND agent_id=$3`,
			provisioned.AccountID, realm.ID, agent.ID).Scan(&sequence,
			&retiredLineage, &newLineage, &retiredActive, &retiredProposed,
			&resetByKind, &resetByID, &reasonCode); err != nil {
			t.Fatal(err)
		}
		if sequence != 1 || retiredLineage != 1 || newLineage != 2 ||
			retiredActive == nil || *retiredActive != 1 ||
			retiredProposed == nil || *retiredProposed != 2 ||
			resetByKind != PrincipalAgent || resetByID != agent.ID ||
			reasonCode != "identity_restart" {
			t.Fatalf("reset ledger = seq:%d line:%d->%d active:%v proposed:%v actor:%s/%s reason:%q",
				sequence, retiredLineage, newLineage, retiredActive, retiredProposed,
				resetByKind, resetByID, reasonCode)
		}
		var resetEvents int
		if err := st.pool.QueryRow(ctx, `SELECT COUNT(*) FROM account_events
			WHERE account_id=$1 AND verb=$2`, provisioned.AccountID,
			VerbAvatarReset).Scan(&resetEvents); err != nil {
			t.Fatal(err)
		}
		if resetEvents != 1 {
			t.Fatalf("reset event count = %d", resetEvents)
		}
		checkpoint, err := st.GetSelfAvatarCheckpoint(ctx, agent)
		if err != nil || !checkpoint.Pending || checkpoint.Reason != "avatar_reset" ||
			checkpoint.LineageGeneration != 2 {
			t.Fatalf("reset checkpoint = %#v / %v", checkpoint, err)
		}

		history, err := st.GetAvatarHistory(ctx, agent, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(history.Versions) != 2 || history.Versions[0].LineageGeneration != 1 ||
			history.Versions[1].LineageGeneration != 1 ||
			history.Versions[1].RollbackEligible {
			t.Fatalf("retired history = %#v", history.Versions)
		}
		oldVersion, err := st.GetAvatarVersion(ctx, agent, 1)
		if err != nil || oldVersion.RollbackEligible || oldVersion.LineageGeneration != 1 {
			t.Fatalf("retired exact version = %#v / %v", oldVersion, err)
		}
		if _, err := st.ResetAvatar(ctx, agent, ResetAvatarInput{
			ExpectedProfileRevision: profile.ProfileRevision,
			IdempotencyKey:          "reset-empty-lineage-2",
		}); !errors.Is(err, ErrAvatarConflict) {
			t.Fatalf("repeated empty reset = %v, want conflict", err)
		}

		proposed3 := proposeAvatarResetVersion(ctx, t, st, agent, style.StylePack,
			profile.ProfileRevision, 0, "reset-main-propose-3")
		if proposed3.Avatar.Proposed == nil || proposed3.Avatar.Proposed.Version != 3 ||
			proposed3.Avatar.Proposed.ParentVersion != nil ||
			proposed3.Avatar.Proposed.LineageGeneration != 2 {
			t.Fatalf("fresh-lineage proposal = %#v", proposed3.Avatar.Proposed)
		}
		active3, err := st.ActivateAvatar(ctx, agent, ActivateAvatarInput{
			Version: 3, ExpectedProfileRevision: proposed3.Avatar.Profile.ProfileRevision,
			IdempotencyKey: "reset-main-activate-3",
		})
		if err != nil {
			t.Fatal(err)
		}
		var activationSequence, activationLineage int64
		var priorActive *int64
		if err := st.pool.QueryRow(ctx, `
			SELECT sequence, lineage_generation, prior_active_version
			  FROM agent_avatar_activations
			 WHERE account_id=$1 AND realm_id=$2 AND agent_id=$3 AND avatar_version=3`,
			provisioned.AccountID, realm.ID, agent.ID).Scan(&activationSequence,
			&activationLineage, &priorActive); err != nil {
			t.Fatal(err)
		}
		if activationSequence != 2 || activationLineage != 2 || priorActive != nil {
			t.Fatalf("fresh-lineage activation = seq:%d lineage:%d prior:%v",
				activationSequence, activationLineage, priorActive)
		}
		if _, err := st.RollbackAvatar(ctx, agent, RollbackAvatarInput{
			Version: 1, ExpectedProfileRevision: active3.Avatar.Profile.ProfileRevision,
			IdempotencyKey: "reset-main-old-rollback",
		}); !errors.Is(err, ErrAvatarConflict) {
			t.Fatalf("cross-lineage rollback = %v, want conflict", err)
		}

		replayed, err := st.ResetAvatar(ctx, agent, resetInput)
		if err != nil || !replayed.Receipt.Replayed ||
			replayed.Receipt.ResultLineageGeneration != 2 ||
			replayed.Avatar.Profile.ActiveVersion != 3 ||
			replayed.Avatar.Profile.LineageGeneration != 2 {
			t.Fatalf("reset replay after later state = %#v / %v", replayed, err)
		}
		conflicting := resetInput
		conflicting.ReasonCode = "different_reason"
		if _, err := st.ResetAvatar(ctx, agent, conflicting); !errors.Is(err, ErrAvatarIdempotencyConflict) {
			t.Fatalf("reset idempotency conflict = %v", err)
		}
	})

	for _, test := range []struct {
		name   string
		policy avatardomain.AutonomyPolicy
	}{
		{"agent proposes requires operator", avatardomain.AutonomyAgentProposes},
		{"operator only requires operator", avatardomain.AutonomyOperatorOnly},
	} {
		t.Run(test.name, func(t *testing.T) {
			agent := createAvatarResetTestAgent(ctx, t, st, provisioned.AccountID, realm.ID, test.name)
			active := activateInitialAvatarResetVersion(ctx, t, st, agent, style.StylePack, test.name)
			updated, err := st.SetAvatarPolicy(ctx, operator, agent.ID, UpdateAvatarPolicyInput{
				Policy: test.policy, ExpectedProfileRevision: active.Avatar.Profile.ProfileRevision,
				IdempotencyKey: test.name + "-policy",
			})
			if err != nil {
				t.Fatal(err)
			}
			input := ResetAvatarInput{ExpectedProfileRevision: updated.Avatar.Profile.ProfileRevision,
				IdempotencyKey: test.name + "-self-reset"}
			if _, err := st.ResetAvatar(ctx, agent, input); !errors.Is(err, ErrAvatarForbidden) {
				t.Fatalf("self reset under %q = %v, want forbidden", test.policy, err)
			}
			input.IdempotencyKey = test.name + "-operator-reset"
			reset, err := st.ResetAgentAvatar(ctx, operator, agent.ID, input)
			if err != nil || reset.Avatar.Profile.LineageGeneration != 2 {
				t.Fatalf("operator reset under %q = %#v / %v", test.policy, reset, err)
			}
		})
	}

	t.Run("pending-only reset is allowed", func(t *testing.T) {
		agent := createAvatarResetTestAgent(ctx, t, st, provisioned.AccountID, realm.ID, "reset-pending-only")
		proposed := proposeAvatarResetVersion(ctx, t, st, agent, style.StylePack,
			1, 0, "pending-only-proposal")
		reset, err := st.ResetAvatar(ctx, agent, ResetAvatarInput{
			ExpectedProfileRevision: proposed.Avatar.Profile.ProfileRevision,
			IdempotencyKey:          "pending-only-reset",
		})
		if err != nil || reset.Avatar.Profile.LineageGeneration != 2 ||
			reset.Avatar.Profile.ActiveVersion != 0 || reset.Avatar.Profile.ProposedVersion != 0 {
			t.Fatalf("pending-only reset = %#v / %v", reset, err)
		}
		var retiredActive, retiredProposed *int64
		if err := st.pool.QueryRow(ctx, `SELECT retired_active_version,
			retired_proposed_version FROM agent_avatar_resets WHERE agent_id=$1`,
			agent.ID).Scan(&retiredActive, &retiredProposed); err != nil {
			t.Fatal(err)
		}
		if retiredActive != nil || retiredProposed == nil || *retiredProposed != 1 {
			t.Fatalf("pending-only retired pointers = active:%v proposed:%v", retiredActive, retiredProposed)
		}
	})

	t.Run("failure backoff cannot be bypassed without durable avatar", func(t *testing.T) {
		agent := createAvatarResetTestAgent(ctx, t, st, provisioned.AccountID, realm.ID, "reset-bare-failure")
		if _, err := st.pool.Exec(ctx, `UPDATE agent_avatar_profiles
			SET status='generation_failed', attempt_count=1,
			    retry_after=clock_timestamp()+interval '30 minutes',
			    failure_code='renderer_unavailable', revision=revision+1
			WHERE agent_id=$1`, agent.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := st.ResetAvatar(ctx, agent, ResetAvatarInput{
			ExpectedProfileRevision: 2, IdempotencyKey: "bare-failure-reset",
		}); !errors.Is(err, ErrAvatarConflict) {
			t.Fatalf("bare failure reset = %v, want conflict", err)
		}
	})

	t.Run("active failure state is cleared", func(t *testing.T) {
		agent := createAvatarResetTestAgent(ctx, t, st, provisioned.AccountID, realm.ID, "reset-active-failure")
		active := activateInitialAvatarResetVersion(ctx, t, st, agent, style.StylePack, "active-failure")
		if _, err := st.pool.Exec(ctx, `UPDATE agent_avatar_profiles
			SET status='generation_failed', attempt_count=3,
			    retry_after=clock_timestamp()+interval '30 minutes',
			    failure_code='renderer_unavailable', revision=revision+1
			WHERE agent_id=$1`, agent.ID); err != nil {
			t.Fatal(err)
		}
		reset, err := st.ResetAvatar(ctx, agent, ResetAvatarInput{
			ExpectedProfileRevision: active.Avatar.Profile.ProfileRevision + 1,
			IdempotencyKey:          "active-failure-reset",
		})
		if err != nil || reset.Avatar.Profile.AttemptCount != 0 ||
			reset.Avatar.Profile.RetryAfter != nil || reset.Avatar.Profile.FailureCode != "" {
			t.Fatalf("active failure reset = %#v / %v", reset, err)
		}
	})

	t.Run("reset transaction rolls back when audit event insertion fails", func(t *testing.T) {
		agent := createAvatarResetTestAgent(ctx, t, st, provisioned.AccountID, realm.ID, "reset-atomicity")
		before := activateInitialAvatarResetVersion(ctx, t, st, agent, style.StylePack, "reset-atomicity")
		triggerName := fmt.Sprintf("avatar_reset_fault_%d", time.Now().UnixNano())
		var quotedAccountID string
		if err := st.pool.QueryRow(ctx, `SELECT quote_literal($1)`,
			provisioned.AccountID).Scan(&quotedAccountID); err != nil {
			t.Fatal(err)
		}
		if _, err := st.pool.Exec(ctx, fmt.Sprintf(`
			CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN
			  IF NEW.account_id = %s AND NEW.verb = 'avatar.reset' THEN
			    RAISE EXCEPTION 'avatar reset event fault injection';
			  END IF;
			  RETURN NEW;
			END
			$$;
			CREATE TRIGGER %s BEFORE INSERT ON account_events
			FOR EACH ROW EXECUTE FUNCTION %s()`, triggerName, quotedAccountID,
			triggerName, triggerName)); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = st.pool.Exec(context.Background(), fmt.Sprintf(
				`DROP TRIGGER IF EXISTS %s ON account_events; DROP FUNCTION IF EXISTS %s()`,
				triggerName, triggerName))
		})

		if _, err := st.ResetAvatar(ctx, agent, ResetAvatarInput{
			ExpectedProfileRevision: before.Avatar.Profile.ProfileRevision,
			IdempotencyKey:          "reset-atomicity-fault",
		}); err == nil {
			t.Fatal("reset unexpectedly committed after audit fault")
		}
		after, err := st.GetAvatar(ctx, agent)
		if err != nil {
			t.Fatal(err)
		}
		if after.Profile.ProfileRevision != before.Avatar.Profile.ProfileRevision ||
			after.Profile.LineageGeneration != before.Avatar.Profile.LineageGeneration ||
			after.Profile.LatestVersion != before.Avatar.Profile.LatestVersion ||
			after.Profile.ActiveVersion != before.Avatar.Profile.ActiveVersion ||
			after.Profile.ProposedVersion != before.Avatar.Profile.ProposedVersion ||
			after.Profile.Status != before.Avatar.Profile.Status {
			t.Fatalf("profile changed across failed reset: before=%#v after=%#v",
				before.Avatar.Profile, after.Profile)
		}
		var resets, receipts, events int
		if err := st.pool.QueryRow(ctx, `SELECT
			(SELECT COUNT(*) FROM agent_avatar_resets WHERE agent_id=$1),
			(SELECT COUNT(*) FROM avatar_mutation_receipts
			  WHERE target_kind='avatar' AND target_id=$1 AND operation='reset'),
			(SELECT COUNT(*) FROM account_events
			  WHERE account_id=$2 AND verb=$3 AND metadata->>'agent_id'=$1)`,
			agent.ID, provisioned.AccountID, VerbAvatarReset).Scan(&resets,
			&receipts, &events); err != nil {
			t.Fatal(err)
		}
		if resets != 0 || receipts != 0 || events != 0 {
			t.Fatalf("failed reset residue = resets:%d receipts:%d events:%d",
				resets, receipts, events)
		}
	})

	t.Run("concurrent stale revision permits one reset", func(t *testing.T) {
		agent := createAvatarResetTestAgent(ctx, t, st, provisioned.AccountID, realm.ID, "reset-race")
		active := activateInitialAvatarResetVersion(ctx, t, st, agent, style.StylePack, "reset-race")
		revision := active.Avatar.Profile.ProfileRevision
		start := make(chan struct{})
		errs := make(chan error, 2)
		var wg sync.WaitGroup
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				_, err := st.ResetAvatar(ctx, agent, ResetAvatarInput{
					ExpectedProfileRevision: revision,
					IdempotencyKey:          "reset-race-" + string(rune('a'+i)),
				})
				errs <- err
			}(i)
		}
		close(start)
		wg.Wait()
		close(errs)
		successes, conflicts := 0, 0
		for err := range errs {
			switch {
			case err == nil:
				successes++
			case errors.Is(err, ErrAvatarConflict):
				conflicts++
			default:
				t.Fatalf("concurrent reset error = %v", err)
			}
		}
		if successes != 1 || conflicts != 1 {
			t.Fatalf("concurrent resets = successes:%d conflicts:%d", successes, conflicts)
		}
	})

	t.Run("reset and proposal serialize on one profile revision", func(t *testing.T) {
		agent := createAvatarResetTestAgent(ctx, t, st, provisioned.AccountID, realm.ID, "reset-proposal-race")
		active := activateInitialAvatarResetVersion(ctx, t, st, agent, style.StylePack, "reset-proposal-race")
		revision := active.Avatar.Profile.ProfileRevision
		proposal := ProposeAvatarInput{
			ExpectedProfileRevision: revision,
			ParentVersion:           active.Avatar.Profile.ActiveVersion,
			StylePackID:             style.StylePack.ID,
			StylePackVersion:        style.StylePack.Version,
			SubjectForm:             avatardomain.SubjectHuman,
			Description:             "A calm human teammate in the shared flat portrait style.",
			VisualSpec:              json.RawMessage(`{"identity":{"expression":"calm"}}`),
			SVG:                     style.StylePack.References[0].SVG,
			Provenance: AvatarClientProvenance{Runtime: "codex", Model: "gpt-5.6",
				Recipe: "avatar-reset-race", RecipeVersion: "1"},
			IdempotencyKey: "reset-proposal-race-evolution",
		}
		start := make(chan struct{})
		type outcome struct {
			operation string
			err       error
		}
		outcomes := make(chan outcome, 2)
		go func() {
			<-start
			_, err := st.ResetAvatar(ctx, agent, ResetAvatarInput{
				ExpectedProfileRevision: revision,
				IdempotencyKey:          "reset-proposal-race-reset",
			})
			outcomes <- outcome{operation: "reset", err: err}
		}()
		go func() {
			<-start
			_, err := st.ProposeAvatar(ctx, agent, proposal)
			outcomes <- outcome{operation: "propose", err: err}
		}()
		close(start)
		first, second := <-outcomes, <-outcomes
		successes, conflicts := 0, 0
		for _, result := range []outcome{first, second} {
			switch {
			case result.err == nil:
				successes++
			case errors.Is(result.err, ErrAvatarConflict):
				conflicts++
			default:
				t.Fatalf("%s race error = %v", result.operation, result.err)
			}
		}
		if successes != 1 || conflicts != 1 {
			t.Fatalf("reset/proposal race = %#v / %#v", first, second)
		}
		view, err := st.GetAvatar(ctx, agent)
		if err != nil {
			t.Fatal(err)
		}
		switch view.Profile.LineageGeneration {
		case 1:
			if view.Profile.Status != avatardomain.StatusProposed ||
				view.Profile.LatestVersion != 2 || view.Profile.ProposedVersion != 2 {
				t.Fatalf("proposal-won projection = %#v", view.Profile)
			}
		case 2:
			if view.Profile.Status != avatardomain.StatusGenerationDue ||
				view.Profile.LatestVersion != 1 || view.Profile.ActiveVersion != 0 {
				t.Fatalf("reset-won projection = %#v", view.Profile)
			}
		default:
			t.Fatalf("race lineage = %d", view.Profile.LineageGeneration)
		}
	})

	t.Run("archived and deleted targets fail closed", func(t *testing.T) {
		archived := createAvatarResetTestAgent(ctx, t, st, provisioned.AccountID, realm.ID, "reset-archived")
		active := activateInitialAvatarResetVersion(ctx, t, st, archived, style.StylePack, "reset-archived")
		if _, err := st.pool.Exec(ctx, `UPDATE agent_avatar_profiles SET status='archived'
			WHERE agent_id=$1`, archived.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := st.ResetAgentAvatar(ctx, operator, archived.ID, ResetAvatarInput{
			ExpectedProfileRevision: active.Avatar.Profile.ProfileRevision,
			IdempotencyKey:          "archived-reset",
		}); !errors.Is(err, ErrAvatarConflict) {
			t.Fatalf("archived reset = %v, want conflict", err)
		}

		deleted := createAvatarResetTestAgent(ctx, t, st, provisioned.AccountID, realm.ID, "reset-deleted")
		_ = activateInitialAvatarResetVersion(ctx, t, st, deleted, style.StylePack, "reset-deleted")
		if err := st.DeleteAgent(ctx, provisioned.AccountID, realm.ID, deleted.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := st.ResetAgentAvatar(ctx, operator, deleted.ID, ResetAvatarInput{
			ExpectedProfileRevision: 3, IdempotencyKey: "deleted-reset",
		}); !errors.Is(err, ErrAvatarNotFound) {
			t.Fatalf("deleted reset = %v, want not found", err)
		}
	})
}

func createAvatarResetTestAgent(ctx context.Context, t *testing.T, st *Store, accountID, realmID, name string) Principal {
	t.Helper()
	agent, err := st.CreateAgent(ctx, accountID, realmID, name)
	if err != nil {
		t.Fatal(err)
	}
	return Principal{Kind: PrincipalAgent, ID: agent.ID, AccountID: accountID,
		RealmID: realmID, AgentName: agent.Name, AccountStatus: "active"}
}

func proposeAvatarResetVersion(ctx context.Context, t *testing.T, st *Store, agent Principal, pack avatardomain.StylePack, expectedRevision, parentVersion int64, key string) AvatarMutationResult {
	t.Helper()
	result, err := st.ProposeAvatar(ctx, agent, ProposeAvatarInput{
		ExpectedProfileRevision: expectedRevision,
		ParentVersion:           parentVersion,
		StylePackID:             pack.ID,
		StylePackVersion:        pack.Version,
		SubjectForm:             avatardomain.SubjectHuman,
		Description:             "A calm human teammate in the shared flat portrait style.",
		VisualSpec:              json.RawMessage(`{"identity":{"expression":"calm"}}`),
		SVG:                     pack.References[0].SVG,
		Provenance: AvatarClientProvenance{Runtime: "codex", Model: "gpt-5.6",
			Recipe: "avatar-reset-test", RecipeVersion: "1"},
		IdempotencyKey: key,
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func activateInitialAvatarResetVersion(ctx context.Context, t *testing.T, st *Store, agent Principal, pack avatardomain.StylePack, key string) AvatarMutationResult {
	t.Helper()
	proposed := proposeAvatarResetVersion(ctx, t, st, agent, pack, 1, 0, key+"-proposal")
	active, err := st.ActivateAvatar(ctx, agent, ActivateAvatarInput{
		Version:                 proposed.Avatar.Profile.ProposedVersion,
		ExpectedProfileRevision: proposed.Avatar.Profile.ProfileRevision,
		IdempotencyKey:          key + "-activation",
	})
	if err != nil {
		t.Fatal(err)
	}
	return active
}
