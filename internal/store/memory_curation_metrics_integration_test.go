package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/testenv"
)

func TestMemoryCurationMetricsLifecyclePostgres(t *testing.T) {
	ctx := context.Background()
	st, _ := newMigrationTestStore(t, testenv.RequirePostgres(t))
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	p := provisionMemoryCurationApplyPrincipal(ctx, t, st)
	want := MemoryCurationCounters{Transitions: map[MemoryCurationTransition]uint64{}, LeaseEvents: map[string]uint64{}}
	for _, planned := range []bool{false, true} {
		for _, operation := range []string{"cancel", "abandon"} {
			name := operation + "_open"
			if planned {
				name = operation + "_planned"
			}
			t.Run(name, func(t *testing.T) {
				agent, err := st.CreateAgent(ctx, p.AccountID, p.RealmID, name)
				if err != nil {
					t.Fatal(err)
				}
				owner := p
				owner.ID = agent.ID
				request, err := st.RequestCuration(ctx, owner, RequestMemoryCurationInput{
					TriggerReason: "manual_refine", IdempotencyKey: "mcrq_sensitive_request",
				})
				if err != nil {
					t.Fatal(err)
				}
				start := StartMemoryCurationInput{RequestID: request.Request.ID, IdempotencyKey: "mrun_sensitive_start",
					Client: MemoryClientProvenance{Runtime: "agent_sensitive_runtime", Model: "realm_sensitive_model"}}
				started, err := st.StartCuration(ctx, owner, start)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := st.StartCuration(ctx, owner, start); err != nil {
					t.Fatal(err)
				}
				want.Transitions[MemoryCurationTransition{"none", "open"}]++
				want.LeaseEvents["start"]++
				renew := RenewMemoryCurationInput{FencingGeneration: started.Run.FencingGeneration,
					Extension: time.Minute, IdempotencyKey: "mrun_sensitive_renew"}
				for range 2 {
					if _, err := st.RenewCuration(ctx, owner, started.Run.ID, renew); err != nil {
						t.Fatal(err)
					}
				}
				want.LeaseEvents["renew"]++
				if _, err := st.PlanCuration(ctx, owner, started.Run.ID, PlanMemoryCurationInput{
					FencingGeneration: started.Run.FencingGeneration, IdempotencyKey: "mrun_sensitive_invalid",
					Draft: json.RawMessage(`{"schema":"mrun_sensitive_invalid"}`),
				}); !errors.Is(err, ErrMemoryCurationInputInvalid) {
					t.Fatalf("malformed plan: %v", err)
				}
				state := "open"
				if planned {
					for range 2 {
						planHardeningCuration(ctx, t, st, owner, started, nil, "mrun_sensitive_plan")
					}
					want.Transitions[MemoryCurationTransition{"open", "planned"}]++
					state = "planned"
				}
				finish := st.CancelCuration
				if operation == "abandon" {
					finish = st.AbandonCuration
				}
				for range 2 {
					if _, err := finish(ctx, owner, started.Run.ID, FinishMemoryCurationInput{
						FencingGeneration: started.Run.FencingGeneration, Reason: "mrun_sensitive_reason",
						IdempotencyKey: "mrun_sensitive_finish",
					}); err != nil {
						t.Fatal(err)
					}
				}
				want.Transitions[MemoryCurationTransition{state, "abandoned"}]++
				assertMemoryCurationCounters(t, st, want)
			})
		}
	}
}

func TestMemoryCurationMetricsExpiryCallSitesPostgres(t *testing.T) {
	ctx := context.Background()
	st, _ := newMigrationTestStore(t, testenv.RequirePostgres(t))
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	p := provisionMemoryCurationApplyPrincipal(ctx, t, st)
	want := MemoryCurationCounters{Transitions: map[MemoryCurationTransition]uint64{}, LeaseEvents: map[string]uint64{}}
	for _, state := range []string{"open", "planned"} {
		for _, operation := range []string{"start", "renew", "plan", "apply", "cancel", "abandon"} {
			t.Run(state+"_"+operation, func(t *testing.T) {
				agent, err := st.CreateAgent(ctx, p.AccountID, p.RealmID, state+"_"+operation)
				if err != nil {
					t.Fatal(err)
				}
				owner := p
				owner.ID = agent.ID
				request, err := st.RequestCuration(ctx, owner, RequestMemoryCurationInput{
					TriggerReason: "manual_refine", IdempotencyKey: "mcrq_private_request",
				})
				if err != nil {
					t.Fatal(err)
				}
				start := StartMemoryCurationInput{RequestID: request.Request.ID, IdempotencyKey: "mrun_private_start"}
				started, err := st.StartCuration(ctx, owner, start)
				if err != nil {
					t.Fatal(err)
				}
				want.Transitions[MemoryCurationTransition{"none", "open"}]++
				want.LeaseEvents["start"]++
				if state == "planned" {
					planHardeningCuration(ctx, t, st, owner, started, nil, "mrun_private_plan")
					want.Transitions[MemoryCurationTransition{"open", "planned"}]++
				}
				if _, err := st.pool.Exec(ctx, `UPDATE memory_curation_runs SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, started.Run.ID); err != nil {
					t.Fatal(err)
				}
				if _, err := st.GetCurationRunInputs(ctx, owner, started.Run.ID, started.Run.FencingGeneration, "", 10); !errors.Is(err, ErrMemoryCurationLeaseExpired) {
					t.Fatalf("expired read: %v", err)
				}
				assertMemoryCurationCounters(t, st, want) // A read never reconciles.
				finish := FinishMemoryCurationInput{FencingGeneration: started.Run.FencingGeneration, IdempotencyKey: "mrun_private_finish"}
				switch operation {
				case "start":
					_, err = st.StartCuration(ctx, owner, start)
				case "renew":
					in := RenewMemoryCurationInput{FencingGeneration: started.Run.FencingGeneration, Extension: time.Minute, IdempotencyKey: "mrun_private_renew"}
					for range 2 { // The expired renewal receipt must not double count.
						_, err = st.RenewCuration(ctx, owner, started.Run.ID, in)
						if !errors.Is(err, ErrMemoryCurationLeaseExpired) {
							t.Fatalf("expired renew: %v", err)
						}
					}
				case "plan":
					raw, marshalErr := json.Marshal(MemoryCurationPlanDraft{Schema: MemoryCurationPlanSchemaV1, DraftRevision: 1, Actions: []MemoryCurationPlanAction{}})
					if marshalErr != nil {
						t.Fatal(marshalErr)
					}
					_, err = st.PlanCuration(ctx, owner, started.Run.ID, PlanMemoryCurationInput{FencingGeneration: started.Run.FencingGeneration, Draft: raw, IdempotencyKey: "mrun_private_new_plan"})
				case "apply":
					_, err = st.ApplyCuration(ctx, owner, started.Run.ID, ApplyMemoryCurationInput{FencingGeneration: started.Run.FencingGeneration, PlanRevision: 1, PlanHash: strings.Repeat("a", 64), IdempotencyKey: "mrun_private_apply"})
				case "cancel":
					_, err = st.CancelCuration(ctx, owner, started.Run.ID, finish)
				case "abandon":
					_, err = st.AbandonCuration(ctx, owner, started.Run.ID, finish)
				}
				if operation == "start" {
					if err != nil {
						t.Fatal(err)
					}
				} else if !errors.Is(err, ErrMemoryCurationLeaseExpired) {
					t.Fatalf("expiry reconciliation: %v", err)
				}
				want.Transitions[MemoryCurationTransition{state, "interrupted"}]++
				want.LeaseEvents["expire"]++
				want.LeaseEvents["reconcile"]++
				assertMemoryCurationCounters(t, st, want)
			})
		}
	}
}

func TestMemoryCurationMetricsQueuePostgres(t *testing.T) {
	ctx := context.Background()
	st, _ := newMigrationTestStore(t, testenv.RequirePostgres(t))
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	p := provisionMemoryCurationApplyPrincipal(ctx, t, st)
	assertQueue := func(pending int64, minAge, maxAge float64) {
		t.Helper()
		before := st.MemoryCurationCounters()
		got, err := st.ReadMemoryCurationQueueMetrics(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got.RequestsPending != pending || got.QueueAgeSeconds < minAge || got.QueueAgeSeconds > maxAge {
			t.Fatalf("queue metrics = %#v, want pending=%d age=[%v,%v]", got, pending, minAge, maxAge)
		}
		assertMemoryCurationCounters(t, st, before)
	}
	assertQueue(0, 0, 0)
	request := func(name string) MemoryCurationRequest {
		t.Helper()
		got, err := st.RequestCuration(ctx, p, RequestMemoryCurationInput{
			TriggerReason: "manual_refine", CoalescingKey: name, IdempotencyKey: "mcrq_private_" + name,
		})
		if err != nil {
			t.Fatal(err)
		}
		return got.Request
	}
	oldest := request("oldest")
	if _, err := st.pool.Exec(ctx, `UPDATE memory_curation_requests SET due_at=clock_timestamp()-interval '2 hours' WHERE id=$1`, oldest.ID); err != nil {
		t.Fatal(err)
	}
	future := request("future")
	if _, err := st.pool.Exec(ctx, `UPDATE memory_curation_requests SET state='retry_wait',due_at=clock_timestamp()+interval '1 hour' WHERE id=$1`, future.ID); err != nil {
		t.Fatal(err)
	}
	assertQueue(2, 7200, 7260) // Future retries count, but do not create age.
	claimed := request("claimed")
	started, err := st.StartCuration(ctx, p, StartMemoryCurationInput{
		RequestID: claimed.ID, IdempotencyKey: "mrun_private_claimed",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertQueue(2, 7200, 7260)
	if _, err := st.CancelCuration(ctx, p, started.Run.ID, FinishMemoryCurationInput{
		FencingGeneration: started.Run.FencingGeneration, IdempotencyKey: "mrun_private_cancelled",
	}); err != nil {
		t.Fatal(err)
	}
	assertQueue(2, 7200, 7260) // Cancelled request remains excluded.
	if _, err := st.pool.Exec(ctx, `UPDATE memory_curation_requests SET state='dead_letter',dead_lettered_at=clock_timestamp() WHERE id=$1`, oldest.ID); err != nil {
		t.Fatal(err)
	}
	assertQueue(1, 0, 0) // Pending future work, without any due unclaimed row.
	for _, item := range []struct{ hide, restore string }{
		{`UPDATE agents SET deleted_at=clock_timestamp() WHERE id=$1`, `UPDATE agents SET deleted_at=NULL WHERE id=$1`},
		{`UPDATE realms SET deleted_at=clock_timestamp(),email_route_state='retired',
			email_route_generation=email_route_generation+1,email_route_operation_id='memory-metrics-retire' WHERE id=$1`,
			`UPDATE realms SET deleted_at=NULL,email_route_state='live',email_route_operation_id=NULL WHERE id=$1`},
		{`UPDATE accounts SET status='suspended' WHERE id=$1`, `UPDATE accounts SET status='active' WHERE id=$1`},
	} {
		ownerID := p.ID
		if strings.Contains(item.hide, "realms") {
			ownerID = p.RealmID
		} else if strings.Contains(item.hide, "accounts") {
			ownerID = p.AccountID
		}
		if _, err := st.pool.Exec(ctx, item.hide, ownerID); err != nil {
			t.Fatal(err)
		}
		assertQueue(0, 0, 0)
		if _, err := st.pool.Exec(ctx, item.restore, ownerID); err != nil {
			t.Fatal(err)
		}
		assertQueue(1, 0, 0)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := st.ReadMemoryCurationQueueMetrics(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled observation = %v", err)
	}
}
