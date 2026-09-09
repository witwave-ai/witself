package store

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/plans"
	"github.com/witwave-ai/witself/internal/testenv"
)

func TestCollaborationMutationGatePreservesReadsPostgres(t *testing.T) {
	st, _ := newMigrationTestStore(t, testenv.RequirePostgres(t))
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	owner := newMemoryLimitPrincipal(ctx, t, st, "collaboration-gate")
	agent, err := st.CreateAgent(ctx, owner.AccountID, owner.RealmID, "worker")
	if err != nil {
		t.Fatal(err)
	}
	worker := owner
	worker.ID = agent.ID
	worker.AgentName = agent.Name
	policies := map[string]int64{plans.MessagingEntitlementVersionPolicy: 1}
	apply := func(revision int64, features []string) {
		t.Helper()
		hash, err := plans.SnapshotHash("custom", nil, policies, features)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.SetAccountPlan(ctx, owner.AccountID, revision, hash, "custom", nil, policies, features); err != nil {
			t.Fatal(err)
		}
	}
	apply(1, []string{plans.MessagingFeature})
	opened, err := st.OpenMessageRequest(ctx, owner, OpenMessageRequestInput{Body: "bounded collaboration fixture", IdempotencyKey: "legacy-open", OfferWindow: time.Minute, ExpiresIn: time.Hour})
	if err != nil {
		t.Fatalf("legacy collaboration open refused: %v", err)
	}
	requestID := opened.Request.ID
	if _, err := st.OfferMessageRequest(ctx, worker, requestID, OfferMessageRequestInput{Body: "fixture offer", IdempotencyKey: "legacy-offer"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SelectMessageRequest(ctx, owner, requestID, SelectMessageRequestInput{SelectedAgentIDs: []string{worker.ID}, IdempotencyKey: "legacy-select", Reservation: 5 * time.Minute}); err != nil {
		t.Fatal(err)
	}
	claim, err := st.ClaimMessageRequest(ctx, worker, requestID, ClaimMessageRequestInput{IdempotencyKey: "legacy-claim", LeaseDuration: 5 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	t.Log("collaboration gate: legacy messaging-only snapshot completed real request admission")
	policies[plans.CollaborationEntitlementVersionPolicy] = 1
	apply(2, []string{plans.MessagingFeature})
	// Capture the complete durable request/message rows, not just row counts.
	snapshot := func() map[string]string {
		t.Helper()
		rows := map[string]string{}
		for _, table := range []string{"agent_message_requests", "agent_message_request_candidates", "agent_message_request_selections", "agent_message_request_claims", "agent_messages", "agent_message_deliveries", "account_events", "usage_events"} {
			var raw string
			if err := st.pool.QueryRow(ctx, "SELECT COALESCE(jsonb_agg(to_jsonb(r) ORDER BY to_jsonb(r)::text),'[]'::jsonb)::text FROM "+table+" r WHERE account_id=$1", owner.AccountID).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			rows[table] = raw
		}
		return rows
	}
	before := snapshot()
	mutations := []struct {
		name string
		call func() error
	}{
		{"open", func() error {
			_, err := st.OpenMessageRequest(ctx, owner, OpenMessageRequestInput{Body: "denied open", IdempotencyKey: "denied-open"})
			return err
		}},
		{"offer", func() error {
			_, err := st.OfferMessageRequest(ctx, worker, requestID, OfferMessageRequestInput{Body: "denied offer", IdempotencyKey: "denied-offer"})
			return err
		}},
		{"decline", func() error {
			_, err := st.DeclineMessageRequest(ctx, worker, requestID, DeclineMessageRequestInput{})
			return err
		}},
		{"select", func() error {
			_, err := st.SelectMessageRequest(ctx, owner, requestID, SelectMessageRequestInput{SelectedAgentIDs: []string{worker.ID}, IdempotencyKey: "denied-select"})
			return err
		}},
		{"cancel", func() error { _, err := st.CancelMessageRequest(ctx, owner, requestID); return err }},
		{"claim", func() error {
			_, err := st.ClaimMessageRequest(ctx, worker, requestID, ClaimMessageRequestInput{IdempotencyKey: "denied-claim"})
			return err
		}},
		{"renew", func() error {
			_, err := st.RenewMessageRequest(ctx, worker, requestID, RenewMessageRequestInput{ClaimID: claim.ClaimID, Generation: claim.Generation, LeaseDuration: 5 * time.Minute})
			return err
		}},
		{"release", func() error {
			_, err := st.ReleaseMessageRequest(ctx, worker, requestID, ReleaseMessageRequestInput{ClaimID: claim.ClaimID, Generation: claim.Generation})
			return err
		}},
		{"complete", func() error {
			_, err := st.CompleteMessageRequest(ctx, worker, requestID, CompleteMessageRequestInput{ClaimID: claim.ClaimID, Generation: claim.Generation, Body: "denied result", IdempotencyKey: "denied-complete"})
			return err
		}},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			err := mutation.call()
			var feature *FeatureNotEnabledError
			if !errors.Is(err, ErrFeatureNotEnabled) || !errors.As(err, &feature) || feature.Feature != plans.CollaborationFeature {
				t.Fatalf("collaboration mutation %s: error=%v, want typed collaboration denial", mutation.name, err)
			}
		})
	}
	if after := snapshot(); !reflect.DeepEqual(before, after) {
		t.Fatal("collaboration denial mutated durable request/message graph")
	}
	detail, err := st.GetMessageRequest(ctx, owner, requestID)
	if err != nil || detail.Request.ID != requestID || len(detail.Claims) != 1 {
		t.Fatalf("disabled collaboration detail=%+v error=%v", detail, err)
	}
	page, err := st.ListMessageRequests(ctx, worker, MessageRequestFilter{})
	if err != nil || len(page.Requests) != 1 || page.Requests[0].ID != requestID {
		t.Fatalf("disabled collaboration list=%+v error=%v", page, err)
	}
	checkpoint, err := st.GetSelfMessageCheckpoint(ctx, worker)
	if err != nil || !checkpoint.Enabled {
		t.Fatalf("collaboration denial changed messaging checkpoint: %+v / %v", checkpoint, err)
	}
	t.Log("collaboration gate: all nine mutations refused without changing graph; reads remained available")
	apply(3, []string{plans.MessagingFeature, plans.CollaborationFeature})
	if _, err := st.RenewMessageRequest(ctx, worker, requestID, RenewMessageRequestInput{ClaimID: claim.ClaimID, Generation: claim.Generation, LeaseDuration: 5 * time.Minute}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CompleteMessageRequest(ctx, worker, requestID, CompleteMessageRequestInput{ClaimID: claim.ClaimID, Generation: claim.Generation, Body: "enabled result", IdempotencyKey: "enabled-complete"}); err != nil {
		t.Fatal(err)
	}
	apply(4, []string{plans.CollaborationFeature})
	_, err = st.OpenMessageRequest(ctx, owner, OpenMessageRequestInput{Body: "messaging denied", IdempotencyKey: "messaging-denied"})
	var feature *FeatureNotEnabledError
	if !errors.As(err, &feature) || feature.Feature != plans.MessagingFeature {
		t.Fatalf("messaging denial lost precedence: %v", err)
	}
	t.Log("collaboration gate: governed re-enable and messaging denial precedence verified")
}

func TestCollaborationAuthoritySurvivesApplyAndImportPostgres(t *testing.T) {
	st, _ := newMigrationTestStore(t, testenv.RequirePostgres(t))
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	owner := newMemoryLimitPrincipal(ctx, t, st, "collaboration-authority")
	makeTarget := func(revision int64, governed, enabled bool) AccountPlanFitApplyTarget {
		t.Helper()
		policies := map[string]int64{plans.MessagingEntitlementVersionPolicy: 1}
		features := []string{plans.MessagingFeature}
		if governed {
			policies[plans.CollaborationEntitlementVersionPolicy] = 1
		}
		if enabled {
			features = append(features, plans.CollaborationFeature)
		}
		hash, err := plans.SnapshotHash("custom", nil, policies, features)
		if err != nil {
			t.Fatal(err)
		}
		return AccountPlanFitApplyTarget{Revision: revision, Plan: "custom", SnapshotHash: hash, Limits: map[string]int64{}, Policies: policies, Features: features}
	}
	apply := func(target AccountPlanFitApplyTarget) (AccountPlanSnapshot, error) {
		return st.SetAccountPlan(ctx, owner.AccountID, target.Revision, target.SnapshotHash, target.Plan, target.Limits, target.Policies, target.Features)
	}
	if _, err := apply(makeTarget(1, false, false)); err != nil {
		t.Fatal(err)
	}
	denied := makeTarget(2, true, false)
	before, err := apply(denied)
	if err != nil {
		t.Fatal(err)
	}
	legacy := makeTarget(3, false, false)
	t.Log("collaboration authority: governed denial persisted before newer legacy attempts")
	if _, err := apply(legacy); !errors.Is(err, ErrPlanSnapshotStale) {
		t.Fatalf("collaboration ordinary apply removed authority: %v", err)
	}
	if _, err := st.ApplyAccountPlanIfFits(ctx, owner.AccountID, legacy); !errors.Is(err, ErrPlanSnapshotStale) {
		t.Fatalf("collaboration fit apply removed authority: %v", err)
	}
	after, err := st.GetAccountPlan(ctx, owner.AccountID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("refused writers changed governed snapshot: %v", err)
	}
	if replay, err := apply(denied); err != nil || replay.Hash != before.Hash || replay.Revision != before.Revision {
		t.Fatalf("ordinary replay=%+v error=%v", replay, err)
	}
	enabled := makeTarget(3, true, true)
	result, err := st.ApplyAccountPlanIfFits(ctx, owner.AccountID, enabled)
	if err != nil || result.State != PlanFitApplyStateApplied || result.AppliedSnapshot == nil {
		t.Fatalf("governed fit enable=%+v error=%v", result, err)
	}
	replay, err := st.ApplyAccountPlanIfFits(ctx, owner.AccountID, enabled)
	if err != nil || !reflect.DeepEqual(result.AppliedSnapshot, replay.AppliedSnapshot) {
		t.Fatalf("fit replay=%+v error=%v", replay, err)
	}
	if _, err := apply(denied); !errors.Is(err, ErrPlanSnapshotStale) {
		t.Fatalf("older governed writer error=%v", err)
	}
	final, err := apply(makeTarget(4, true, false))
	if err != nil {
		t.Fatal(err)
	}
	t.Log("collaboration authority: both stale-writer guards and governed replay/re-enable verified")
	// Existing account export/import fields must preserve the authority marker,
	// exact fence and denial without a new migration or importer normalization.
	if err := st.SuspendAccountSystem(ctx, owner.AccountID, "evacuation", "collaboration fixture"); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := st.ExportAccount(ctx, owner.AccountID, "collaboration-fixture", "test", &archive); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ImportAccount(ctx, owner.AccountID, bytes.NewReader(archive.Bytes())); !errors.Is(err, ErrAccountExists) {
		t.Fatalf("occupied import error=%v", err)
	}
	if err := deleteAccountForIntegrationTest(ctx, st, owner.AccountID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ImportAccount(ctx, owner.AccountID, bytes.NewReader(archive.Bytes())); err != nil {
		t.Fatal(err)
	}
	restored, err := st.GetAccountPlan(ctx, owner.AccountID)
	if err != nil || !reflect.DeepEqual(final, restored) || CollaborationEnabledForPlanSnapshot(restored.AppliedAt, restored.Policies, restored.Features) {
		t.Fatalf("import changed governed denial: %v", err)
	}
	if _, err := apply(makeTarget(5, false, false)); !errors.Is(err, ErrPlanSnapshotStale) {
		t.Fatalf("restored authority removed: %v", err)
	}
	t.Log("collaboration authority: archive roundtrip preserved exact denial and monotonic fence")
}
