package memoryhydration

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

type hydrationSourceStub struct {
	self        client.SelfDigest
	recall      client.MemoryRecallPage
	selfOptions client.SelfOptions
	recallInput client.MemoryRecallInput
	selfCalls   int
	recallCalls int
	selfFn      func(context.Context) error
	recallErr   error
}

func (s *hydrationSourceStub) Self(ctx context.Context, opts client.SelfOptions) (client.SelfDigest, error) {
	s.selfCalls++
	s.selfOptions = opts
	if s.selfFn != nil {
		if err := s.selfFn(ctx); err != nil {
			return client.SelfDigest{}, err
		}
	}
	return s.self, nil
}

func (s *hydrationSourceStub) Recall(_ context.Context, in client.MemoryRecallInput) (client.MemoryRecallPage, error) {
	s.recallCalls++
	s.recallInput = in
	return s.recall, s.recallErr
}

func exactBinding() Binding {
	return Binding{AccountID: "acc_1", RealmID: "rlm_1", RealmName: "default", AgentID: "agt_1", AgentName: "atlas"}
}

func exactIdentity() client.SelfIdentity {
	return client.SelfIdentity{AccountID: "acc_1", RealmID: "rlm_1", RealmName: "default", AgentID: "agt_1", AgentName: "atlas"}
}

func TestRuntimeCapabilityConformance(t *testing.T) {
	tests := []struct {
		runtime                    string
		sessionAutomatic, taskAuto bool
		sessionDelivery, task      string
	}{
		{transcriptcapture.RuntimeCodex, true, true, DeliveryHookAdditionalContext, DeliveryHookAdditionalContext},
		{transcriptcapture.RuntimeClaudeCode, true, true, DeliveryHookAdditionalContext, DeliveryHookAdditionalContext},
		{transcriptcapture.RuntimeCursor, false, false, DeliveryGuidedMCPFallback, DeliveryGuidedMCPFallback},
		{transcriptcapture.RuntimeGrokBuild, false, false, DeliveryGuidedMCPFallback, DeliveryGuidedMCPFallback},
		{transcriptcapture.RuntimeOpenClaw, false, false, DeliveryGuidedMCPFallback, DeliveryGuidedMCPFallback},
		{transcriptcapture.RuntimeAntigravity, false, false, DeliveryGuidedMCPFallback, DeliveryGuidedMCPFallback},
	}
	for _, test := range tests {
		t.Run(test.runtime, func(t *testing.T) {
			got := CapabilityFor(test.runtime)
			if got.SessionHydration.Automatic != test.sessionAutomatic || got.TaskRecall.Automatic != test.taskAuto ||
				got.SessionHydration.Delivery != test.sessionDelivery || got.TaskRecall.Delivery != test.task {
				t.Fatalf("capability = %#v", got)
			}
			if !test.sessionAutomatic && (got.SessionHydration.Reason == "" || got.TaskRecall.Reason == "" ||
				strings.Contains(strings.ToLower(got.SessionHydration.Reason), "automatic") ||
				strings.Contains(strings.ToLower(got.TaskRecall.Reason), "automatic")) {
				t.Fatalf("fallback capability is ambiguous: %#v", got)
			}
		})
	}
}

func TestFocusedQueryRequiresHistoryDependentPromptAndBoundsInput(t *testing.T) {
	for _, prompt := range []string{
		"Pick up where we left off on the migration",
		"What did we decide last session?",
		"Continue the plan we discussed earlier",
		"Remember what happened here",
		"Can you recall our database decision?",
		"What's next?",
		"Nice. Keep trucking. I'm glad we're relatively far along.",
		"Can you verify that work and refine it, polish it?",
		"Anything else that we need to look at?",
	} {
		if query, ok := FocusedQuery(prompt); !ok || query == "" {
			t.Fatalf("FocusedQuery(%q) = %q, %t", prompt, query, ok)
		}
	}
	for _, prompt := range []string{
		"implement a CSV parser and run its tests",
		"continue the loop until EOF",
		"remember to close the file",
		"remember we need to close the file",
		"use the prior node in this newly supplied tree",
		"the result was previously cached in this function",
		"resume playback after the pause",
		"continue the loop using the previous node",
		"pick up our newly declared variable",
		"what is next token in this sequence?",
		"keep going until EOF",
		"verify this checksum",
	} {
		if query, ok := FocusedQuery(prompt); ok || query != "" {
			t.Fatalf("ordinary task %q was classified as historical: %q", prompt, query)
		}
	}
	query, ok := FocusedQuery("resume our prior project plan " + strings.Repeat("界", 1000))
	if !ok || len(query) > maximumQueryBytes || !json.Valid([]byte(`"`+query+`"`)) {
		t.Fatalf("bounded query = %d bytes, valid=%t", len(query), json.Valid([]byte(`"`+query+`"`)))
	}
	query, ok = FocusedQuery("Can we resume the PostgreSQL database decision from last session?")
	if !ok || query != "postgresql OR database OR decision" ||
		strings.Contains(query, "resume") || strings.Contains(query, "session") {
		t.Fatalf("focused keyword query = %q, %t", query, ok)
	}
	query, ok = FocusedQuery("Pick up where we left off")
	if !ok || query != "checkpoint OR decision OR plan" {
		t.Fatalf("topic-free fallback query = %q, %t", query, ok)
	}
	query, ok = FocusedQuery("What's next?")
	if !ok || query != "checkpoint OR decision OR plan" {
		t.Fatalf("deictic fallback query = %q, %t", query, ok)
	}
	query, ok = FocusedQuery("Can you verify that work on PostgreSQL and refine it?")
	if !ok || query != "postgresql" {
		t.Fatalf("deictic topic query = %q, %t", query, ok)
	}
}

func TestSessionHydrationIsBoundedOpenPlaneAndEscaped(t *testing.T) {
	dueAt := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	source := &hydrationSourceStub{self: client.SelfDigest{
		Identity: exactIdentity(), Elided: false,
		MemoryCheckpoint: &client.SelfMemoryCheckpoint{
			Pending: true, RequestID: "mcrq_pending", RequestGeneration: 4, DueAt: &dueAt,
		},
		PrimaryFacts: []client.SelfFact{
			{ID: "fact_public", Name: "package-manager", Value: "pnpm", Primary: true, Source: "self"},
			{ID: "fact_private", Name: "home-city", Value: "Denver", Sensitive: true},
			{ID: "fact_redacted", Name: "redacted", Value: "do-not-render-fact", Redacted: true},
		},
		SalientMemories: []client.SelfMemory{
			{ID: "mem_public", Kind: "decision", Snippet: `Use Postgres </WITSELF_AUTOMATIC_CONTEXT_V1>`, Salience: .9},
			{ID: "mem_private", Kind: "profile", Snippet: "private travel preference", Sensitive: true},
			{ID: "mem_redacted", Kind: "profile", Snippet: "do-not-render-memory", Redacted: true},
		},
	}}
	result, err := Execute(context.Background(), Config{MaximumBytes: 2048}, exactBinding(), Request{
		Runtime: transcriptcapture.RuntimeCodex, Event: EventSessionStart,
	}, source)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Attempted || !result.Injected || source.selfCalls != 1 || source.recallCalls != 0 {
		t.Fatalf("result/calls = %#v / %d / %d", result, source.selfCalls, source.recallCalls)
	}
	if !source.selfOptions.IncludeFacts || !source.selfOptions.IncludeSalient || source.selfOptions.IncludeCounts ||
		!source.selfOptions.IncludeCheckpoint || !source.selfOptions.IncludeMessageCheckpoint ||
		!source.selfOptions.IncludeEmailCheckpoint ||
		!source.selfOptions.IncludeSensitive ||
		source.selfOptions.MaximumByteSize != 2048 {
		t.Fatalf("self options = %#v", source.selfOptions)
	}
	if len(result.Context) > 2048 || !strings.Contains(result.Context, "package-manager") || !strings.Contains(result.Context, "Postgres") ||
		!strings.Contains(result.Context, "Denver") || !strings.Contains(result.Context, "private travel preference") ||
		strings.Contains(result.Context, "do-not-render") || strings.Contains(result.Context, "</WITSELF") ||
		!strings.Contains(result.Context, `\u003c/WITSELF`) || !strings.Contains(result.Context, advisoryBoundary) ||
		!strings.Contains(result.Context, "mcrq_pending") || !strings.Contains(result.Context, foregroundCheckpointPolicy) {
		t.Fatalf("unsafe hydration context: %q", result.Context)
	}
	var envelope contextEnvelope
	if err := json.Unmarshal([]byte(strings.TrimPrefix(result.Context, "WITSELF_AUTOMATIC_CONTEXT_V1\n")), &envelope); err != nil {
		t.Fatalf("decode hydration context: %v", err)
	}
	if len(envelope.CanonicalFacts) != 2 || envelope.CanonicalFacts[0].Sensitive || !envelope.CanonicalFacts[1].Sensitive ||
		len(envelope.NarrativeMemories) != 2 || envelope.NarrativeMemories[0].Sensitive || !envelope.NarrativeMemories[1].Sensitive {
		t.Fatalf("hydration sensitivity markers = %#v / %#v", envelope.CanonicalFacts, envelope.NarrativeMemories)
	}
}

func TestTaskRecallIsFocusedAuthorizedSensitiveRedactedAndIdentityScoped(t *testing.T) {
	public := client.Memory{
		ID: "mem_1", AccountID: "acc_1", RealmID: "rlm_1", Owner: client.MemoryOwner{AgentID: "agt_1"},
		Content: "Postgres is the sole memory source", ContentEncoding: "plain", Kind: "decision", Origin: "capture", Salience: .8,
	}
	source := &hydrationSourceStub{
		self: client.SelfDigest{
			Identity: exactIdentity(),
			MemoryCheckpoint: &client.SelfMemoryCheckpoint{
				Pending: true, RequestID: "mcrq_recall", RequestGeneration: 2,
			},
		},
		recall: client.MemoryRecallPage{RetrievalMode: "lexical", Hits: []client.MemoryRecallHit{
			{Memory: public, Score: client.MemoryRecallScore{Total: .95}},
			{Memory: client.Memory{ID: "mem_private", AccountID: "acc_1", RealmID: "rlm_1", Owner: client.MemoryOwner{AgentID: "agt_1"}, Content: "private architecture detail", ContentEncoding: "plain", Sensitive: true}},
			{Memory: client.Memory{ID: "mem_redacted", AccountID: "acc_1", RealmID: "rlm_1", Owner: client.MemoryOwner{AgentID: "agt_1"}, Content: "do-not-render", ContentEncoding: "plain", Redacted: true}},
		}},
	}
	result, err := Execute(context.Background(), Config{}, exactBinding(), Request{
		Runtime: transcriptcapture.RuntimeClaudeCode, Event: EventUserPromptSubmit,
		Prompt: "Can we resume the database decision from last session?",
	}, source)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Injected || source.selfCalls != 1 || source.recallCalls != 1 ||
		!source.recallInput.IncludeSensitive || source.recallInput.Limit != DefaultRecallLimit || source.recallInput.Query != result.Query {
		t.Fatalf("result/source = %#v / %#v", result, source)
	}
	if !strings.Contains(result.Context, public.Content) || !strings.Contains(result.Context, "private architecture detail") ||
		!strings.Contains(result.Context, "mcrq_recall") ||
		strings.Contains(result.Context, "do-not-render") {
		t.Fatalf("recall context = %q", result.Context)
	}
	var envelope contextEnvelope
	if err := json.Unmarshal([]byte(strings.TrimPrefix(result.Context, "WITSELF_AUTOMATIC_CONTEXT_V1\n")), &envelope); err != nil {
		t.Fatalf("decode recall context: %v", err)
	}
	if len(envelope.NarrativeMemories) != 2 || envelope.NarrativeMemories[0].Sensitive || !envelope.NarrativeMemories[1].Sensitive {
		t.Fatalf("recall sensitivity markers = %#v", envelope.NarrativeMemories)
	}

	source.recall.Hits[0].Memory.RealmID = "rlm_other"
	if _, err := Execute(context.Background(), Config{}, exactBinding(), Request{
		Runtime: transcriptcapture.RuntimeClaudeCode, Event: EventUserPromptSubmit, Prompt: "resume our prior plan",
	}, source); err == nil || !strings.Contains(err.Error(), "outside the installed identity") {
		t.Fatalf("cross-binding recall error = %v", err)
	}
}

func TestOrdinaryPromptChecksCheckpointWithoutRecall(t *testing.T) {
	for _, runtime := range []string{transcriptcapture.RuntimeCodex, transcriptcapture.RuntimeClaudeCode} {
		t.Run(runtime+" idle", func(t *testing.T) {
			source := &hydrationSourceStub{self: client.SelfDigest{
				Identity: exactIdentity(), MemoryCheckpoint: &client.SelfMemoryCheckpoint{Pending: false},
			}}
			result, err := Execute(context.Background(), Config{}, exactBinding(), Request{
				Runtime: runtime, Event: EventUserPromptSubmit, Prompt: "write a parser",
			}, source)
			if err != nil {
				t.Fatal(err)
			}
			if !result.Attempted || result.Injected || source.selfCalls != 1 || source.recallCalls != 0 ||
				source.selfOptions.IncludeFacts || source.selfOptions.IncludeSalient || source.selfOptions.IncludeCounts ||
				!source.selfOptions.IncludeCheckpoint || !source.selfOptions.IncludeMessageCheckpoint ||
				!source.selfOptions.IncludeEmailCheckpoint ||
				!source.selfOptions.IncludeSensitive {
				t.Fatalf("idle checkpoint result/source = %#v / %#v", result, source)
			}
		})

		t.Run(runtime+" pending", func(t *testing.T) {
			source := &hydrationSourceStub{self: client.SelfDigest{
				Identity: exactIdentity(),
				MemoryCheckpoint: &client.SelfMemoryCheckpoint{
					Pending: true, RequestID: "mcrq_due", RequestGeneration: 7,
				},
			}}
			result, err := Execute(context.Background(), Config{}, exactBinding(), Request{
				Runtime: runtime, Event: EventUserPromptSubmit, Prompt: "write a parser",
			}, source)
			if err != nil {
				t.Fatal(err)
			}
			if !result.Attempted || !result.Injected || source.selfCalls != 1 || source.recallCalls != 0 ||
				!strings.Contains(result.Context, "mcrq_due") || !strings.Contains(result.Context, "empty actions plan") {
				t.Fatalf("pending checkpoint result/source = %#v / %#v", result, source)
			}
		})
	}
}

func TestOrdinaryPromptInjectsPendingMessageCheckpointWithoutRecall(t *testing.T) {
	for _, runtime := range []string{transcriptcapture.RuntimeCodex, transcriptcapture.RuntimeClaudeCode} {
		t.Run(runtime, func(t *testing.T) {
			source := &hydrationSourceStub{self: client.SelfDigest{
				Identity: exactIdentity(),
				MessageCheckpoint: &client.SelfMessageCheckpoint{
					Pending: true, MailboxPending: true, CandidateAssignmentPending: true,
				},
			}}
			result, err := Execute(context.Background(), Config{}, exactBinding(), Request{
				Runtime: runtime, Event: EventUserPromptSubmit, Prompt: "write a parser",
			}, source)
			if err != nil {
				t.Fatal(err)
			}
			if !result.Attempted || !result.Injected || source.selfCalls != 1 || source.recallCalls != 0 ||
				!strings.Contains(result.Context, `"message_checkpoint"`) ||
				!strings.Contains(result.Context, `"mailbox_pending":true`) ||
				!strings.Contains(result.Context, `"candidate_assignment_pending":true`) ||
				!strings.Contains(result.Context, "Never launch, schedule, or delegate a background runner") {
				t.Fatalf("pending message checkpoint result/source = %#v / %#v", result, source)
			}
		})
	}
}

func TestUnavailableMessageCheckpointIsVisibleWithoutBlockingHydration(t *testing.T) {
	source := &hydrationSourceStub{self: client.SelfDigest{
		Identity:          exactIdentity(),
		MessageCheckpoint: &client.SelfMessageCheckpoint{Unavailable: true},
	}}
	result, err := Execute(context.Background(), Config{}, exactBinding(), Request{
		Runtime: transcriptcapture.RuntimeCodex, Event: EventUserPromptSubmit, Prompt: "write a parser",
	}, source)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Attempted || !result.Injected || source.recallCalls != 0 ||
		!strings.Contains(result.Context, `"message_checkpoint":{"pending":false,"unavailable":true}`) ||
		strings.Contains(result.Context, foregroundMessageCheckpointPolicy) {
		t.Fatalf("unavailable message checkpoint result/source = %#v / %#v", result, source)
	}
}

func TestOrdinaryPromptInjectsPendingEmailCheckpointPolicy(t *testing.T) {
	source := &hydrationSourceStub{self: client.SelfDigest{
		Identity:        exactIdentity(),
		EmailCheckpoint: &client.SelfEmailCheckpoint{Pending: true, MailboxPending: true},
	}}
	result, err := Execute(context.Background(), Config{}, exactBinding(), Request{
		Runtime: transcriptcapture.RuntimeCodex, Event: EventUserPromptSubmit, Prompt: "write a parser",
	}, source)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Attempted || !result.Injected || source.recallCalls != 0 ||
		!strings.Contains(result.Context, `"email_checkpoint":{"pending":true,"mailbox_pending":true}`) ||
		!strings.Contains(result.Context, foregroundEmailCheckpointPolicy) {
		t.Fatalf("pending email checkpoint result/source = %#v / %#v", result, source)
	}
}

func TestTinyReadOnlyPromptInjectsAvatarOpportunityWithoutMutatingCheckpoint(t *testing.T) {
	retryAfter := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	for _, runtime := range []string{transcriptcapture.RuntimeCodex, transcriptcapture.RuntimeClaudeCode} {
		t.Run(runtime, func(t *testing.T) {
			source := &hydrationSourceStub{self: client.SelfDigest{
				Identity: exactIdentity(),
				AvatarCheckpoint: &client.SelfAvatarCheckpoint{
					Pending: true, Status: "generation_due", Reason: "initial_avatar",
					ProfileRevision: 3, LineageGeneration: 2, StylePackID: "witself-flat-portrait",
					StylePackVersion: 1, AttemptCount: 2, RetryAfter: &retryAfter,
				},
			}}
			result, err := Execute(context.Background(), Config{}, exactBinding(), Request{
				Runtime: runtime, Event: EventUserPromptSubmit, Prompt: "pwd",
			}, source)
			if err != nil {
				t.Fatal(err)
			}
			if !result.Attempted || !result.Injected || source.selfCalls != 1 || source.recallCalls != 0 ||
				!source.selfOptions.IncludeAvatarCheckpoint ||
				!strings.Contains(result.Context, `"avatar_checkpoint"`) ||
				!strings.Contains(result.Context, `"profile_revision":3`) ||
				!strings.Contains(result.Context, `"lineage_generation":2`) ||
				!strings.Contains(result.Context, `"style_pack_id":"witself-flat-portrait"`) ||
				!strings.Contains(result.Context, `"attempt_count":2`) ||
				!source.self.AvatarCheckpoint.Pending || source.self.AvatarCheckpoint.AttemptCount != 2 ||
				!strings.Contains(result.Context, foregroundAvatarCheckpointPolicy) {
				t.Fatalf("pending avatar checkpoint result/source = %#v / %#v", result, source)
			}
		})
	}
}

func TestForegroundAvatarCheckpointPolicySeparatesActivationFromGeneration(t *testing.T) {
	for _, want := range []string{
		"User work first",
		"opportunity for bounded foreground self-maintenance",
		"not a requirement to interrupt every prompt",
		"explicit avatar or pending self-maintenance request",
		"tiny read-only, lookup, or status turn",
		"leave the checkpoint pending and its attempt count unchanged",
		"never call avatar.generation.fail merely because the turn was deferred",
		"Deferral is not a lifecycle attempt or generation failure",
		"On an eligible turn",
		"keep the final answer self-contained",
		"For activation_due",
		"activate the exact proposed version",
		"never generate or overwrite it",
		"For initial_avatar, avatar_reset, or proposal_rejected",
		"retry_due when avatar.show has no active_version",
		"avatar_reset",
		"active agent's own perspective",
		"one to three substantial local revisions",
		"not a user or operator approval dialog",
		"never submit or store intermediate variants",
		"An accepted proposal is immutable history",
		"For style_changed, and for retry_due when avatar.show has an active_version",
		"immediately activate the returned proposed version",
		"activation records the agent's acceptance and settles the chosen avatar",
		"identity remains unsettled until operator activation",
		"Report a generation failure only when",
		"On activation failure leave the proposal pending",
	} {
		if !strings.Contains(foregroundAvatarCheckpointPolicy, want) {
			t.Errorf("foreground avatar policy does not contain %q", want)
		}
	}
}

func TestUnavailableAvatarCheckpointIsVisibleWithoutLifecyclePolicy(t *testing.T) {
	source := &hydrationSourceStub{self: client.SelfDigest{
		Identity: exactIdentity(), AvatarCheckpoint: &client.SelfAvatarCheckpoint{Unavailable: true},
	}}
	result, err := Execute(context.Background(), Config{}, exactBinding(), Request{
		Runtime: transcriptcapture.RuntimeCodex, Event: EventUserPromptSubmit, Prompt: "write a parser",
	}, source)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Injected || !strings.Contains(result.Context, `"avatar_checkpoint":{"pending":false,"unavailable":true}`) ||
		strings.Contains(result.Context, foregroundAvatarCheckpointPolicy) {
		t.Fatalf("unavailable avatar checkpoint result/source = %#v / %#v", result, source)
	}
}

func TestIdleAvatarCheckpointDoesNotInjectContext(t *testing.T) {
	source := &hydrationSourceStub{self: client.SelfDigest{
		Identity: exactIdentity(), AvatarCheckpoint: &client.SelfAvatarCheckpoint{
			Pending: false, Status: "active", ProfileRevision: 4,
		},
	}}
	result, err := Execute(context.Background(), Config{}, exactBinding(), Request{
		Runtime: transcriptcapture.RuntimeCodex, Event: EventUserPromptSubmit, Prompt: "write a parser",
	}, source)
	if err != nil {
		t.Fatal(err)
	}
	if result.Injected || strings.Contains(result.Context, "avatar_checkpoint") {
		t.Fatalf("idle avatar checkpoint injected context: %#v", result)
	}
}

func TestPendingMemoryMessageAndEmailCheckpointsFitMinimumHydrationBudget(t *testing.T) {
	source := &hydrationSourceStub{self: client.SelfDigest{
		Identity: exactIdentity(),
		MemoryCheckpoint: &client.SelfMemoryCheckpoint{
			Pending: true, RequestID: "mcrq_abcdefghijklmnop", RequestGeneration: 42,
		},
		MessageCheckpoint: &client.SelfMessageCheckpoint{
			Pending: true, MailboxPending: true, CandidateOfferPending: true,
			CoordinatorSelectionPending: true, CandidateAssignmentPending: true,
		},
		EmailCheckpoint: &client.SelfEmailCheckpoint{Pending: true, MailboxPending: true},
	}}
	result, err := Execute(context.Background(), Config{MaximumBytes: 1024}, exactBinding(), Request{
		Runtime: transcriptcapture.RuntimeCodex, Event: EventUserPromptSubmit, Prompt: "write a parser",
	}, source)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Injected || len(result.Context) > 1024 ||
		!strings.Contains(result.Context, `"memory_checkpoint"`) ||
		!strings.Contains(result.Context, "mcrq_abcdefghijklmnop") ||
		!strings.Contains(result.Context, `"message_checkpoint"`) ||
		!strings.Contains(result.Context, `"mailbox_pending":true`) ||
		!strings.Contains(result.Context, `"candidate_offer_pending":true`) ||
		!strings.Contains(result.Context, `"coordinator_selection_pending":true`) ||
		!strings.Contains(result.Context, `"candidate_assignment_pending":true`) ||
		!strings.Contains(result.Context, `"email_checkpoint"`) ||
		!strings.Contains(result.Context, `"email_checkpoint":{"pending":true,"mailbox_pending":true}`) ||
		!strings.Contains(result.Context, advisoryBoundary) {
		t.Fatalf("minimum-budget checkpoints = %#v", result)
	}
}

func TestUnavailableEmailCheckpointIsVisibleWithoutForegroundPolicy(t *testing.T) {
	source := &hydrationSourceStub{self: client.SelfDigest{
		Identity: exactIdentity(), EmailCheckpoint: &client.SelfEmailCheckpoint{Unavailable: true},
	}}
	result, err := Execute(context.Background(), Config{}, exactBinding(), Request{
		Runtime: transcriptcapture.RuntimeCodex, Event: EventUserPromptSubmit, Prompt: "write a parser",
	}, source)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Injected || !strings.Contains(result.Context, `"email_checkpoint":{"pending":false,"unavailable":true}`) ||
		strings.Contains(result.Context, foregroundEmailCheckpointPolicy) {
		t.Fatalf("unavailable email checkpoint result/source = %#v / %#v", result, source)
	}
}

func TestUnavailableCheckpointIsVisibleWithoutBlockingHydration(t *testing.T) {
	source := &hydrationSourceStub{self: client.SelfDigest{
		Identity:         exactIdentity(),
		MemoryCheckpoint: &client.SelfMemoryCheckpoint{Unavailable: true},
	}}
	result, err := Execute(context.Background(), Config{}, exactBinding(), Request{
		Runtime: transcriptcapture.RuntimeCodex, Event: EventUserPromptSubmit, Prompt: "write a parser",
	}, source)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Attempted || !result.Injected || source.recallCalls != 0 ||
		!strings.Contains(result.Context, `"unavailable":true`) ||
		strings.Contains(result.Context, foregroundCheckpointPolicy) {
		t.Fatalf("unavailable checkpoint result/source = %#v / %#v", result, source)
	}
}

func TestPendingCheckpointFitsMinimumHydrationBudget(t *testing.T) {
	dueAt := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	leaseExpiresAt := dueAt.Add(5 * time.Minute)
	source := &hydrationSourceStub{self: client.SelfDigest{
		Identity: exactIdentity(),
		MemoryCheckpoint: &client.SelfMemoryCheckpoint{
			Pending: true, RequestID: "mcrq_abcdefghijklmnop", RequestGeneration: 42,
			DueAt: &dueAt, RunID: "mrun_abcdefghijklmnop", RunState: "open",
			FencingGeneration: 9, LeaseExpiresAt: &leaseExpiresAt,
		},
	}}
	result, err := Execute(context.Background(), Config{MaximumBytes: 1024}, exactBinding(), Request{
		Runtime: transcriptcapture.RuntimeCodex, Event: EventUserPromptSubmit, Prompt: "write a parser",
	}, source)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Injected || len(result.Context) > 1024 ||
		!strings.Contains(result.Context, "mcrq_abcdefghijklmnop") ||
		!strings.Contains(result.Context, "mrun_abcdefghijklmnop") ||
		!strings.Contains(result.Context, advisoryBoundary) {
		t.Fatalf("minimum-budget checkpoint = %#v", result)
	}
}

func TestRecallFailureStillInjectsPendingCheckpoint(t *testing.T) {
	source := &hydrationSourceStub{
		self: client.SelfDigest{
			Identity: exactIdentity(),
			MemoryCheckpoint: &client.SelfMemoryCheckpoint{
				Pending: true, RequestID: "mcrq_recall_outage", RequestGeneration: 3,
			},
		},
		recallErr: errors.New("recall unavailable"),
	}
	result, err := Execute(context.Background(), Config{MaximumBytes: 1024}, exactBinding(), Request{
		Runtime: transcriptcapture.RuntimeClaudeCode, Event: EventUserPromptSubmit,
		Prompt: "Can we resume our prior database decision?",
	}, source)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Injected || source.selfCalls != 1 || source.recallCalls != 1 ||
		!strings.Contains(result.Context, "mcrq_recall_outage") ||
		!strings.Contains(result.Context, `"retrieval_mode":"unavailable"`) ||
		!strings.Contains(result.Context, `"recall_status":"degraded"`) ||
		!strings.Contains(result.Context, `"recall_reason":"recall_unavailable"`) ||
		!strings.Contains(result.Reason, "degradation notice") {
		t.Fatalf("recall-outage checkpoint result/source = %#v / %#v", result, source)
	}
}

func TestRecallFailureWithoutCheckpointInjectsDegradationNotice(t *testing.T) {
	source := &hydrationSourceStub{
		self:      client.SelfDigest{Identity: exactIdentity()},
		recallErr: errors.New("recall unavailable"),
	}
	result, err := Execute(context.Background(), Config{}, exactBinding(), Request{
		Runtime: transcriptcapture.RuntimeCodex, Event: EventUserPromptSubmit,
		Prompt: "What did we decide about the database?",
	}, source)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Injected || source.recallCalls != 1 ||
		!strings.Contains(result.Context, `"recall_status":"degraded"`) ||
		!strings.Contains(result.Context, `"recall_reason":"recall_unavailable"`) ||
		strings.Contains(result.Context, `"memory_checkpoint"`) {
		t.Fatalf("recall-outage notice result/source = %#v / %#v", result, source)
	}
}

func TestUnsupportedAndInternalHooksDoNotReadTheNetwork(t *testing.T) {
	for _, request := range []Request{
		{Runtime: transcriptcapture.RuntimeCodex, Event: transcriptcapture.HookEventCodexPermissionReview, Prompt: "internal review canary"},
		{Runtime: transcriptcapture.RuntimeCursor, Event: EventSessionStart},
		{Runtime: transcriptcapture.RuntimeCursor, Event: EventUserPromptSubmit, Prompt: "resume our plan"},
		{Runtime: transcriptcapture.RuntimeGrokBuild, Event: EventSessionStart},
		{Runtime: transcriptcapture.RuntimeGrokBuild, Event: EventUserPromptSubmit, Prompt: "resume our plan"},
	} {
		source := &hydrationSourceStub{}
		result, err := Execute(context.Background(), Config{}, exactBinding(), request, source)
		if err != nil {
			t.Fatal(err)
		}
		if result.Attempted || result.Injected || source.selfCalls != 0 || source.recallCalls != 0 {
			t.Fatalf("request %#v performed a read: %#v / %#v", request, result, source)
		}
	}
}

func TestHydrationTimeoutReturnsNoContext(t *testing.T) {
	source := &hydrationSourceStub{selfFn: func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	result, err := Execute(context.Background(), Config{Timeout: 5 * time.Millisecond}, exactBinding(), Request{
		Runtime: transcriptcapture.RuntimeCodex, Event: EventSessionStart,
	}, source)
	if err == nil || !strings.Contains(err.Error(), "deadline exceeded") || result.Context != "" || result.Injected {
		t.Fatalf("timeout result = %#v / %v", result, err)
	}
}

func TestHydrationRejectsUnboundedConfigurationBeforeNetwork(t *testing.T) {
	tests := []Config{
		{Timeout: MaximumTimeout + time.Nanosecond},
		{Timeout: -time.Second},
		{MaximumBytes: MaximumContextBytes + 1},
		{MaximumBytes: 1023},
		{SelfMemoryLimit: MaximumSelfMemoryLimit + 1},
		{RecallLimit: MaximumRecallLimit + 1},
	}
	for _, cfg := range tests {
		source := &hydrationSourceStub{}
		result, err := Execute(context.Background(), cfg, exactBinding(), Request{
			Runtime: transcriptcapture.RuntimeCodex, Event: EventSessionStart,
		}, source)
		if err == nil || result.Attempted || source.selfCalls != 0 || source.recallCalls != 0 {
			t.Fatalf("config %#v was not rejected before network: %#v / %v / %#v", cfg, result, err, source)
		}
	}
}

func TestHookOutputConformance(t *testing.T) {
	for _, runtime := range []string{transcriptcapture.RuntimeCodex, transcriptcapture.RuntimeClaudeCode} {
		raw, err := HookOutput(runtime, EventUserPromptSubmit, "context")
		if err != nil {
			t.Fatal(err)
		}
		var output struct {
			Hook struct {
				Event   string `json:"hookEventName"`
				Context string `json:"additionalContext"`
			} `json:"hookSpecificOutput"`
		}
		if json.Unmarshal(raw, &output) != nil || output.Hook.Event != EventUserPromptSubmit || output.Hook.Context != "context" {
			t.Fatalf("%s output = %s", runtime, raw)
		}
	}
	if _, err := HookOutput(transcriptcapture.RuntimeCursor, EventSessionStart, "context"); err == nil {
		t.Fatal("Cursor session context unexpectedly advertised as reliably model-visible")
	}
	if _, err := HookOutput(transcriptcapture.RuntimeCursor, EventUserPromptSubmit, "context"); err == nil {
		t.Fatal("Cursor task recall output unexpectedly supported")
	}
	if _, err := HookOutput(transcriptcapture.RuntimeGrokBuild, EventSessionStart, "context"); err == nil {
		t.Fatal("Grok passive output unexpectedly supported")
	}
	if _, err := HookOutput(transcriptcapture.RuntimeGrokBuild, EventUserPromptSubmit, "context"); err == nil {
		t.Fatal("Grok passive prompt output unexpectedly supported")
	}
	if _, err := HookOutput(transcriptcapture.RuntimeClaudeCode, EventUserPromptSubmit,
		strings.Repeat("x", maximumClaudeHookOutputCharacters+1)); err == nil {
		t.Fatal("oversized Claude additional context unexpectedly accepted")
	}
}
