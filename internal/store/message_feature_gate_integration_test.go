package store

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/plans"
	"github.com/witwave-ai/witself/internal/testenv"
)

func TestMessagingFeatureGateCoversMailboxAndRequestOperationsPostgres(t *testing.T) {
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

	provisioned, err := st.ProvisionAccount(ctx,
		"message-feature-gate@witwave.ai", "message feature gate", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = deleteAccountForIntegrationTest(context.Background(), st, provisioned.AccountID) }()
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate = %v / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "default")
	if err != nil {
		t.Fatal(err)
	}
	sender, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "sender")
	if err != nil {
		t.Fatal(err)
	}
	recipient, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "recipient")
	if err != nil {
		t.Fatal(err)
	}
	principal := Principal{
		Kind: PrincipalAgent, ID: sender.ID, AccountID: provisioned.AccountID,
		RealmID: realm.ID, AgentName: sender.Name, AccountStatus: "active",
	}

	applyPlan := func(revision int64, policies map[string]int64, features []string) {
		t.Helper()
		hash, err := plans.SnapshotHash("test", map[string]int64{}, policies, features)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.SetAccountPlan(ctx, provisioned.AccountID, revision, hash, "test",
			map[string]int64{}, policies, features); err != nil {
			t.Fatal(err)
		}
	}
	legacyPolicies := map[string]int64{TranscriptRetentionDaysPolicy: 30}
	applyPlan(1, legacyPolicies, []string{"memory", "facts"})
	if _, err := st.ListMessages(ctx, principal, MessageFilter{}); err != nil {
		t.Fatalf("legacy applied snapshot without messaging marker was gated: %v", err)
	}
	legacyCheckpoint, err := st.GetSelfMessageCheckpoint(ctx, principal)
	if err != nil || !legacyCheckpoint.Enabled {
		t.Fatalf("legacy applied checkpoint = %+v / %v", legacyCheckpoint, err)
	}
	messagePolicies := map[string]int64{
		TranscriptRetentionDaysPolicy:           30,
		plans.MessageRetentionDaysPolicy:        30,
		plans.MessagingEntitlementVersionPolicy: plans.MessagingEntitlementVersion,
	}
	applyPlan(2, messagePolicies, []string{"memory", "facts"})

	messageID := "msg_abcdefghijklmnop"
	messageClaimID := "mcl_abcdefghijklmnop"
	requestID := "mrq_abcdefghijklmnop"
	requestClaimID := "mrc_abcdefghijklmnop"
	disabledOperations := []struct {
		name string
		call func() error
	}{
		{"send", func() error {
			_, err := st.SendMessage(ctx, principal, SendMessageInput{
				ToAgent: recipient.ID, Body: "must never persist", Payload: json.RawMessage(`{"private":"payload"}`),
				IdempotencyKey: "disabled-send",
			})
			return err
		}},
		{"reply", func() error {
			_, err := st.ReplyMessage(ctx, principal, messageID, ReplyMessageInput{
				Body: "reply", IdempotencyKey: "disabled-reply",
			})
			return err
		}},
		{"claim", func() error {
			_, err := st.ClaimMessage(ctx, principal, messageID, ClaimMessageInput{
				IdempotencyKey: "disabled-claim",
			})
			return err
		}},
		{"renew", func() error {
			_, err := st.RenewMessageClaim(ctx, principal, messageID, RenewMessageClaimInput{
				ClaimID: messageClaimID, ProcessingGeneration: 1,
			})
			return err
		}},
		{"release", func() error {
			_, err := st.ReleaseMessageClaim(ctx, principal, messageID, ReleaseMessageClaimInput{
				ClaimID: messageClaimID, ProcessingGeneration: 1,
			})
			return err
		}},
		{"complete", func() error {
			_, err := st.CompleteMessage(ctx, principal, messageID, CompleteMessageInput{
				ClaimID: messageClaimID, ProcessingGeneration: 1,
				Body: "complete", IdempotencyKey: "disabled-complete",
			})
			return err
		}},
		{"list", func() error {
			_, err := st.ListMessages(ctx, principal, MessageFilter{})
			return err
		}},
		{"read", func() error {
			_, err := st.ReadMessage(ctx, principal, messageID)
			return err
		}},
		{"peek", func() error {
			_, err := st.PeekMessage(ctx, principal, messageID)
			return err
		}},
		{"ack", func() error {
			_, err := st.AckMessage(ctx, principal, messageID)
			return err
		}},
		{"request open", func() error {
			_, err := st.OpenMessageRequest(ctx, principal, OpenMessageRequestInput{
				Body: "open", IdempotencyKey: "disabled-open",
			})
			return err
		}},
		{"request list", func() error {
			_, err := st.ListMessageRequests(ctx, principal, MessageRequestFilter{})
			return err
		}},
		{"request get", func() error {
			_, err := st.GetMessageRequest(ctx, principal, requestID)
			return err
		}},
		{"request offer", func() error {
			_, err := st.OfferMessageRequest(ctx, principal, requestID, OfferMessageRequestInput{
				Body: "offer", IdempotencyKey: "disabled-offer",
			})
			return err
		}},
		{"request decline", func() error {
			_, err := st.DeclineMessageRequest(ctx, principal, requestID, DeclineMessageRequestInput{})
			return err
		}},
		{"request select", func() error {
			_, err := st.SelectMessageRequest(ctx, principal, requestID, SelectMessageRequestInput{
				SelectedAgentIDs: []string{recipient.ID}, IdempotencyKey: "disabled-select",
			})
			return err
		}},
		{"request cancel", func() error {
			_, err := st.CancelMessageRequest(ctx, principal, requestID)
			return err
		}},
		{"request claim", func() error {
			_, err := st.ClaimMessageRequest(ctx, principal, requestID, ClaimMessageRequestInput{
				IdempotencyKey: "disabled-request-claim",
			})
			return err
		}},
		{"request renew", func() error {
			_, err := st.RenewMessageRequest(ctx, principal, requestID, RenewMessageRequestInput{
				ClaimID: requestClaimID, Generation: 1,
			})
			return err
		}},
		{"request release", func() error {
			_, err := st.ReleaseMessageRequest(ctx, principal, requestID, ReleaseMessageRequestInput{
				ClaimID: requestClaimID, Generation: 1,
			})
			return err
		}},
		{"request complete", func() error {
			_, err := st.CompleteMessageRequest(ctx, principal, requestID, CompleteMessageRequestInput{
				ClaimID: requestClaimID, Generation: 1, Body: "done",
				IdempotencyKey: "disabled-request-complete",
			})
			return err
		}},
	}
	for _, operation := range disabledOperations {
		t.Run(operation.name, func(t *testing.T) {
			err := operation.call()
			var featureErr *FeatureNotEnabledError
			if !errors.Is(err, ErrFeatureNotEnabled) || !errors.As(err, &featureErr) ||
				featureErr.Feature != plans.MessagingFeature {
				t.Fatalf("error = %v, want messaging FeatureNotEnabledError", err)
			}
		})
	}

	var messages, requests int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM agent_messages WHERE account_id=$1`, provisioned.AccountID).Scan(&messages); err != nil {
		t.Fatal(err)
	}
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM agent_message_requests WHERE account_id=$1`, provisioned.AccountID).Scan(&requests); err != nil {
		t.Fatal(err)
	}
	if messages != 0 || requests != 0 {
		t.Fatalf("disabled operations persisted messages/requests = %d/%d", messages, requests)
	}
	checkpoint, err := st.GetSelfMessageCheckpoint(ctx, principal)
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.Enabled || checkpoint.Pending {
		t.Fatalf("disabled checkpoint = %+v", checkpoint)
	}

	indefiniteMessagePolicies := map[string]int64{
		TranscriptRetentionDaysPolicy:           30,
		plans.MessagingEntitlementVersionPolicy: plans.MessagingEntitlementVersion,
	}
	applyPlan(3, indefiniteMessagePolicies, []string{"memory", "facts", plans.MessagingFeature})
	msg, err := st.SendMessage(ctx, principal, SendMessageInput{
		ToAgent: recipient.ID, Body: "enabled", IdempotencyKey: "enabled-send",
	})
	if err != nil || msg.ID == "" {
		t.Fatalf("enabled send = %+v / %v", msg, err)
	}
	checkpoint, err = st.GetSelfMessageCheckpoint(ctx, principal)
	if err != nil || !checkpoint.Enabled {
		t.Fatalf("enabled checkpoint = %+v / %v", checkpoint, err)
	}
	pending, err := st.CaptureMemory(ctx, principal, CaptureMemoryInput{
		Content: "pending message evidence",
		Kind:    "decision",
		Evidence: []MemoryEvidenceInput{{
			ResolutionState: MemoryEvidencePending,
			ExternalLocator: "witself://message/pending",
		}},
		IdempotencyKey: "enabled-pending-message-evidence",
	})
	if err != nil {
		t.Fatal(err)
	}
	pendingEvidenceID := pending.Memory.Evidence[0].ID

	applyPlan(4, messagePolicies, []string{"memory", "facts"})
	if _, err := st.ListMessages(ctx, principal, MessageFilter{}); !errors.Is(err, ErrFeatureNotEnabled) {
		t.Fatalf("post-transition list error = %v", err)
	}
	if _, err := st.CaptureMemory(ctx, principal, CaptureMemoryInput{
		Content: "must roll back with disabled message evidence",
		Kind:    "decision",
		Evidence: []MemoryEvidenceInput{{
			ResolutionState: MemoryEvidenceResolved,
			ResolvedKind:    "message",
			SourceMessageID: msg.ID,
		}},
		IdempotencyKey: "disabled-message-evidence-capture",
	}); !errors.Is(err, ErrFeatureNotEnabled) {
		t.Fatalf("disabled message-evidence capture error = %v", err)
	}
	if _, err := st.ResolveMemoryEvidence(
		ctx,
		principal,
		pendingEvidenceID,
		ResolveMemoryEvidenceInput{
			ResolvedKind:    "message",
			SourceMessageID: msg.ID,
			IdempotencyKey:  "disabled-message-evidence-resolve",
		},
	); !errors.Is(err, ErrFeatureNotEnabled) {
		t.Fatalf("disabled message-evidence resolution error = %v", err)
	}
	var memoryCount, terminalResolutionCount int
	if err := st.pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM memories WHERE account_id=$1),
		  (SELECT count(*) FROM memory_evidence
		    WHERE pending_evidence_id=$2
		      AND resolution_state IN ('resolved','unresolvable'))`,
		provisioned.AccountID,
		pendingEvidenceID,
	).Scan(&memoryCount, &terminalResolutionCount); err != nil {
		t.Fatal(err)
	}
	if memoryCount != 1 || terminalResolutionCount != 0 {
		t.Fatalf(
			"disabled message evidence persisted memory/terminal rows = %d/%d, want 1/0",
			memoryCount,
			terminalResolutionCount,
		)
	}
}

func TestCatalogCollaborationFeatureGateRequestMutationsPostgres(t *testing.T) {
	testenv.RequirePostgres(t)
	catalog, err := plans.Load()
	if err != nil {
		t.Fatal(err)
	}
	messagingPlans := 0
	for _, plan := range catalog.Plans {
		if !plan.HasFeature(plans.MessagingFeature) {
			continue
		}
		messagingPlans++
		t.Run(plan.ID, func(t *testing.T) {
			for _, legacy := range []bool{false, true} {
				name := "adopted"
				if legacy {
					name = "legacy_without_marker"
				}
				t.Run(name, func(t *testing.T) {
					f := newMessageRequestHardeningFixture(t, "coordinator", "worker", "decliner")
					coordinator := f.principals["coordinator"]
					worker := f.principals["worker"]
					policies := maps.Clone(plan.Policies)
					if legacy {
						delete(policies, plans.CollaborationEntitlementVersionPolicy)
					}
					// Adopted authority must come from the real catalog, never from a
					// marker injected by this fixture. Removing catalog adoption makes
					// the denied open below fail, even though the store gate still exists.
					messagingOnly := slices.DeleteFunc(slices.Clone(plan.Features), func(feature string) bool {
						return feature == plans.CollaborationFeature
					})
					var revision int64
					apply := func(t *testing.T, features []string) {
						t.Helper()
						revision++
						hash, err := plans.SnapshotHash(plan.ID, plan.Limits, policies, features)
						if err != nil {
							t.Fatal(err)
						}
						if _, err := f.st.SetAccountPlan(f.ctx, f.accountID, revision, hash,
							plan.ID, plan.Limits, policies, features); err != nil {
							t.Fatal(err)
						}
					}
					// Include audit and usage rows so a denial cannot hide side effects
					// behind unchanged request state or table counts.
					snapshot := func(t *testing.T) map[string]string {
						t.Helper()
						rows := map[string]string{}
						for _, table := range []string{
							"agent_message_requests", "agent_message_request_candidates",
							"agent_message_request_selections", "agent_message_request_claims",
							"agent_messages", "agent_message_deliveries", "account_events", "usage_events",
						} {
							var raw string
							if err := f.st.pool.QueryRow(f.ctx,
								"SELECT COALESCE(jsonb_agg(to_jsonb(r) ORDER BY to_jsonb(r)::text),'[]'::jsonb)::text FROM "+table+" r WHERE account_id=$1",
								f.accountID).Scan(&raw); err != nil {
								t.Fatal(err)
							}
							rows[table] = raw
						}
						return rows
					}
					apply(t, messagingOnly)
					checkMutation := func(name string, call func() error) {
						t.Helper()
						if !t.Run(name, func(t *testing.T) {
							if !legacy {
								apply(t, messagingOnly)
								before := snapshot(t)
								err := call()
								var feature *FeatureNotEnabledError
								if !errors.Is(err, ErrFeatureNotEnabled) || !errors.As(err, &feature) ||
									feature.Feature != plans.CollaborationFeature {
									t.Fatalf("catalog collaboration denial = %v, want typed collaboration FeatureNotEnabledError", err)
								}
								if after := snapshot(t); !reflect.DeepEqual(before, after) {
									t.Fatal("catalog collaboration denial changed durable request/message, audit, or usage rows")
								}
								checkpoint, err := f.st.GetSelfMessageCheckpoint(f.ctx, worker)
								if err != nil || !checkpoint.Enabled {
									t.Fatalf("messaging must remain enabled: checkpoint=%+v / %v", checkpoint, err)
								}
								apply(t, plan.Features)
							}
							if err := call(); err != nil {
								t.Fatalf("permitted catalog request mutation: %v", err)
							}
						}) {
							t.FailNow()
						}
					}

					var opened OpenMessageRequestResult
					checkMutation("open", func() error {
						result, err := f.st.OpenMessageRequest(f.ctx, coordinator, OpenMessageRequestInput{
							Body: "catalog collaboration lifecycle", MaxAssignees: 1,
							OfferWindow: time.Minute, ExpiresIn: time.Hour, IdempotencyKey: "catalog-open",
						})
						if err == nil {
							opened = result
						}
						return err
					})
					if opened.Request.ID == "" || opened.Request.PendingCount != 2 {
						t.Fatalf("opened request = %+v", opened.Request)
					}
					requestID := opened.Request.ID
					var offered OfferMessageRequestResult
					checkMutation("offer", func() error {
						result, err := f.st.OfferMessageRequest(f.ctx, worker, requestID, OfferMessageRequestInput{
							Body: "catalog worker offer", IdempotencyKey: "catalog-offer",
						})
						if err == nil {
							offered = result
						}
						return err
					})
					if offered.Request.OfferCount != 1 || offered.Offer.Message.ID == "" {
						t.Fatalf("offered request = %+v", offered)
					}
					var declined MessageRequest
					checkMutation("decline", func() error {
						result, err := f.st.DeclineMessageRequest(f.ctx, f.principals["decliner"], requestID, DeclineMessageRequestInput{})
						if err == nil {
							declined = result
						}
						return err
					})
					if declined.DeclineCount != 1 || declined.PendingCount != 0 {
						t.Fatalf("declined request = %+v", declined)
					}
					var selected SelectMessageRequestResult
					checkMutation("select", func() error {
						result, err := f.st.SelectMessageRequest(f.ctx, coordinator, requestID, SelectMessageRequestInput{
							SelectedAgentIDs: []string{worker.ID}, Reservation: 2 * time.Minute, IdempotencyKey: "catalog-select",
						})
						if err == nil {
							selected = result
						}
						return err
					})
					if len(selected.Claims) != 1 || selected.Claims[0].State != MessageRequestClaimReserved {
						t.Fatalf("selected request = %+v", selected)
					}
					var claim MessageRequestClaim
					checkMutation("claim", func() error {
						result, err := f.st.ClaimMessageRequest(f.ctx, worker, requestID, ClaimMessageRequestInput{
							LeaseDuration: 2 * time.Minute, IdempotencyKey: "catalog-claim",
						})
						if err == nil {
							claim = result
						}
						return err
					})
					if claim.State != MessageRequestClaimClaimed || claim.LeaseExpiresAt == nil {
						t.Fatalf("claimed request = %+v", claim)
					}
					var renewed MessageRequestClaim
					checkMutation("renew", func() error {
						result, err := f.st.RenewMessageRequest(f.ctx, worker, requestID, RenewMessageRequestInput{
							ClaimID: claim.ClaimID, Generation: claim.Generation, LeaseDuration: 3 * time.Minute,
						})
						if err == nil {
							renewed = result
						}
						return err
					})
					if renewed.LeaseExpiresAt == nil || !renewed.LeaseExpiresAt.After(*claim.LeaseExpiresAt) {
						t.Fatalf("renewed request = %+v, original = %+v", renewed, claim)
					}
					var released MessageRequestClaim
					checkMutation("release", func() error {
						result, err := f.st.ReleaseMessageRequest(f.ctx, worker, requestID, ReleaseMessageRequestInput{
							ClaimID: claim.ClaimID, Generation: claim.Generation,
						})
						if err == nil {
							released = result
						}
						return err
					})
					if released.State != MessageRequestClaimReleased {
						t.Fatalf("released request = %+v", released)
					}
					// Completion must use a fresh, valid claim after the release above.
					f.selectAgents(t, opened, "coordinator", "catalog-reselect", "worker")
					claim = f.claim(t, opened, "worker", "catalog-reclaim")
					var completed CompleteMessageRequestResult
					checkMutation("complete", func() error {
						result, err := f.st.CompleteMessageRequest(f.ctx, worker, requestID, CompleteMessageRequestInput{
							ClaimID: claim.ClaimID, Generation: claim.Generation, Body: "catalog result", IdempotencyKey: "catalog-complete",
						})
						if err == nil {
							completed = result
						}
						return err
					})
					if completed.Request.State != MessageRequestStateCompleted ||
						completed.Claim.State != MessageRequestClaimCompleted || completed.Message.ID == "" {
						t.Fatalf("completed request = %+v", completed)
					}
					cancellable := f.open(t, "coordinator", "catalog-cancellable", 1)
					var cancelled MessageRequest
					checkMutation("cancel", func() error {
						result, err := f.st.CancelMessageRequest(f.ctx, coordinator, cancellable.Request.ID)
						if err == nil {
							cancelled = result
						}
						return err
					})
					if cancelled.State != MessageRequestStateCancelled {
						t.Fatalf("cancelled request = %+v", cancelled)
					}
				})
			}
		})
	}
	if messagingPlans == 0 {
		t.Fatal("catalog contains no messaging plans to exercise")
	}
}

// The catalog Free plan now carries the collaboration marker so a paid
// account can downgrade to Free (Stripe cancellation, subscription end, or
// operator downgrade) and reach Applied=Free on the cell through the normal
// apply and fit-apply paths.
func TestCatalogCollaborationDowngradeToFreeAppliesPostgres(t *testing.T) {
	testenv.RequirePostgres(t)
	catalog, err := plans.Load()
	if err != nil {
		t.Fatal(err)
	}
	free, ok := catalog.Get("free")
	if !ok {
		t.Fatal("free catalog plan missing")
	}
	if _, marker := free.Policies[plans.CollaborationEntitlementVersionPolicy]; !marker {
		t.Fatal("free catalog plan must carry the collaboration marker")
	}
	freeHash, err := plans.SnapshotHash(free.ID, free.Limits, free.Policies, free.Features)
	if err != nil {
		t.Fatal(err)
	}
	messagingPlans := 0
	for _, plan := range catalog.Plans {
		if !plan.HasFeature(plans.MessagingFeature) {
			continue
		}
		messagingPlans++
		for _, mode := range []string{"ordinary_apply", "fit_apply"} {
			t.Run(plan.ID+"/"+mode, func(t *testing.T) {
				f := newMessageRequestHardeningFixture(t, "coordinator")
				hash, err := plans.SnapshotHash(plan.ID, plan.Limits, plan.Policies, plan.Features)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.st.SetAccountPlan(f.ctx, f.accountID, 1, hash,
					plan.ID, plan.Limits, plan.Policies, plan.Features); err != nil {
					t.Fatalf("apply %s: %v", plan.ID, err)
				}
				var applied AccountPlanSnapshot
				switch mode {
				case "ordinary_apply":
					applied, err = f.st.SetAccountPlan(f.ctx, f.accountID, 2, freeHash,
						free.ID, free.Limits, free.Policies, free.Features)
				case "fit_apply":
					var fit AccountPlanFitApplyResult
					fit, err = f.st.ApplyAccountPlanIfFits(f.ctx, f.accountID, AccountPlanFitApplyTarget{
						Revision: 2, Plan: free.ID, SnapshotHash: freeHash,
						Limits: free.Limits, Policies: free.Policies, Features: free.Features,
					})
					if err == nil {
						if fit.State != PlanFitApplyStateApplied || fit.AppliedSnapshot == nil {
							t.Fatalf("fit apply did not land applied: %+v", fit)
						}
						applied = *fit.AppliedSnapshot
					}
				}
				if err != nil {
					t.Fatalf("downgrade to free (%s) = %v", mode, err)
				}
				if applied.Plan != free.ID || applied.Hash != freeHash || applied.Revision != 2 {
					t.Fatalf("downgrade landed = %+v; want plan=%s hash=%s revision=2", applied, free.ID, freeHash)
				}
				stored, err := f.st.GetAccountPlan(f.ctx, f.accountID)
				if err != nil {
					t.Fatal(err)
				}
				if stored.Plan != free.ID || stored.Hash != freeHash {
					t.Fatalf("post-downgrade account plan = %+v; want free", stored)
				}
				if stored.Policies[plans.CollaborationEntitlementVersionPolicy] != plans.CollaborationEntitlementVersion {
					t.Fatalf("downgraded free snapshot lost the collaboration marker: %+v", stored.Policies)
				}
				if slices.Contains(stored.Features, plans.CollaborationFeature) || slices.Contains(stored.Features, plans.MessagingFeature) {
					t.Fatalf("free downgrade granted messaging or collaboration: %v", stored.Features)
				}
			})
		}
	}
	if messagingPlans == 0 {
		t.Fatal("catalog contains no messaging plans to exercise")
	}
}

// The fence that prevents an unmarked snapshot from silently stripping
// collaboration authority stays in force. A synthetic Free-shaped snapshot
// without the marker is still refused after a governed plan, no matter how it
// reaches the store.
func TestCatalogCollaborationUnmarkedSnapshotStillFencedPostgres(t *testing.T) {
	testenv.RequirePostgres(t)
	catalog, err := plans.Load()
	if err != nil {
		t.Fatal(err)
	}
	free, ok := catalog.Get("free")
	if !ok {
		t.Fatal("free catalog plan missing")
	}
	unmarkedPolicies := maps.Clone(free.Policies)
	delete(unmarkedPolicies, plans.CollaborationEntitlementVersionPolicy)
	unmarkedHash, err := plans.SnapshotHash(free.ID, free.Limits, unmarkedPolicies, free.Features)
	if err != nil {
		t.Fatal(err)
	}
	for _, plan := range catalog.Plans {
		if !plan.HasFeature(plans.MessagingFeature) {
			continue
		}
		t.Run(plan.ID, func(t *testing.T) {
			f := newMessageRequestHardeningFixture(t, "coordinator")
			hash, err := plans.SnapshotHash(plan.ID, plan.Limits, plan.Policies, plan.Features)
			if err != nil {
				t.Fatal(err)
			}
			before, err := f.st.SetAccountPlan(f.ctx, f.accountID, 1, hash,
				plan.ID, plan.Limits, plan.Policies, plan.Features)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.st.SetAccountPlan(f.ctx, f.accountID, 2, unmarkedHash,
				free.ID, free.Limits, unmarkedPolicies, free.Features); !errors.Is(err, ErrPlanSnapshotStale) {
				t.Fatalf("ordinary apply of unmarked snapshot = %v, want ErrPlanSnapshotStale", err)
			}
			if _, err := f.st.ApplyAccountPlanIfFits(f.ctx, f.accountID, AccountPlanFitApplyTarget{
				Revision: 2, Plan: free.ID, SnapshotHash: unmarkedHash,
				Limits: free.Limits, Policies: unmarkedPolicies, Features: free.Features,
			}); !errors.Is(err, ErrPlanSnapshotStale) {
				t.Fatalf("fit apply of unmarked snapshot = %v, want ErrPlanSnapshotStale", err)
			}
			after, err := f.st.GetAccountPlan(f.ctx, f.accountID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("rejected unmarked snapshot mutated stored plan: %v", err)
			}
		})
	}
}
