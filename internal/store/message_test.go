package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"testing"
	"time"
)

func TestNormalizeSendMessageInput(t *testing.T) {
	in, err := normalizeSendMessageInput(SendMessageInput{
		ToAgent: "  peer  ", Subject: "  handoff  ", Body: "hello",
		Payload: json.RawMessage(`{"b":2,"a":1}`), IdempotencyKey: "  retry-1  ",
	})
	if err != nil {
		t.Fatal(err)
	}
	if in.ToAgent != "peer" || in.Subject != "handoff" || in.Kind != "request" || in.IdempotencyKey != "retry-1" {
		t.Fatalf("normalized input = %#v", in)
	}
	if string(in.Payload) != `{"a":1,"b":2}` {
		t.Fatalf("canonical payload = %s", in.Payload)
	}
	nullPayload, err := normalizeSendMessageInput(SendMessageInput{
		ToAgent: "peer", Body: "hello", Payload: json.RawMessage(`null`),
	})
	if err != nil || len(nullPayload.Payload) != 0 {
		t.Fatalf("null payload = %s / %v, want absent", nullPayload.Payload, err)
	}

	tests := []struct {
		name string
		in   SendMessageInput
		want string
	}{
		{name: "missing recipient", in: SendMessageInput{Body: "x"}, want: "recipient"},
		{name: "missing body", in: SendMessageInput{ToAgent: "peer"}, want: "body"},
		{name: "payload array", in: SendMessageInput{ToAgent: "peer", Body: "x", Payload: json.RawMessage(`[]`)}, want: "payload"},
		{name: "bad thread", in: SendMessageInput{ToAgent: "peer", Body: "x", ThreadID: "thread_1"}, want: "thread_id"},
		{name: "body too large", in: SendMessageInput{ToAgent: "peer", Body: strings.Repeat("x", maxMessageBodyBytes+1)}, want: "body exceeds"},
		{name: "payload too large", in: SendMessageInput{ToAgent: "peer", Body: "x", Payload: json.RawMessage(`{"x":"` + strings.Repeat("y", maxMessagePayloadBytes) + `"}`)}, want: "payload exceeds"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := normalizeSendMessageInput(tc.in)
			if !errors.Is(err, ErrMessageInputInvalid) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want ErrMessageInputInvalid containing %q", err, tc.want)
			}
		})
	}
}

func TestNormalizeMessageAudiences(t *testing.T) {
	explicit, err := normalizeSendMessageInput(SendMessageInput{
		AudienceKind: MessageRecipientAgents,
		ToAgents:     []string{" Bob ", "agent_aaaaaaaaaaaaaaaa", "Bob"},
		Body:         "hello",
	})
	if err != nil {
		t.Fatal(err)
	}
	if explicit.AudienceKind != MessageRecipientAgents || len(explicit.ToAgents) != 2 ||
		explicit.ToAgents[0] != "Bob" || explicit.ToAgents[1] != "agent_aaaaaaaaaaaaaaaa" ||
		len(explicit.audienceFingerprint()) != 64 {
		t.Fatalf("normalized explicit audience = %#v", explicit)
	}
	realm, err := normalizeSendMessageInput(SendMessageInput{
		AudienceKind: MessageRecipientRealm, Body: "hello",
	})
	if err != nil || realm.audienceFingerprint() == "" {
		t.Fatalf("normalized realm audience = %#v / %v", realm, err)
	}
	realmAgain, err := normalizeSendMessageInput(SendMessageInput{
		AudienceKind: MessageRecipientRealm, Body: "hello",
	})
	if err != nil || realmAgain.audienceFingerprint() != realm.audienceFingerprint() {
		t.Fatalf("realm fingerprint = %q / %v, want %q", realmAgain.audienceFingerprint(), err, realm.audienceFingerprint())
	}

	tests := []SendMessageInput{
		{AudienceKind: MessageRecipientAgents, Body: "x"},
		{AudienceKind: MessageRecipientAgents, ToAgent: "Bob", ToAgents: []string{"Alice"}, Body: "x"},
		{AudienceKind: MessageRecipientRealm, ToAgent: "Bob", Body: "x"},
		{AudienceKind: MessageRecipientAgent, ToAgent: "Bob", ToAgents: []string{"Alice"}, Body: "x"},
		{AudienceKind: "room", Body: "x"},
		{AudienceKind: MessageRecipientAgents, ToAgents: make([]string, maxMessageAudienceRecipients+1), Body: "x"},
	}
	for i := range tests[len(tests)-1].ToAgents {
		tests[len(tests)-1].ToAgents[i] = fmt.Sprintf("agent_%016d", i)
	}
	for i, in := range tests {
		if _, err := normalizeSendMessageInput(in); !errors.Is(err, ErrMessageInputInvalid) {
			t.Fatalf("invalid audience %d error = %v, want ErrMessageInputInvalid", i, err)
		}
	}
}

func TestSendMessageRejectsCallerSuppliedReplyParent(t *testing.T) {
	_, err := (&Store{}).SendMessage(context.Background(), Principal{Kind: PrincipalAgent}, SendMessageInput{
		ToAgent: "peer", Body: "forged reply", ReplyToMessageID: "msg_parent",
	})
	if !errors.Is(err, ErrMessageInputInvalid) || !strings.Contains(err.Error(), "direct send cannot set") {
		t.Fatalf("error = %v, want caller-supplied reply parent refusal", err)
	}
}

func TestMessageAgentIDSelectorNamespace(t *testing.T) {
	for _, tc := range []struct {
		selector string
		wantID   bool
	}{
		{selector: "agent_aaaaaaaaaaaaaaaa", wantID: true},
		{selector: "agent_", wantID: true},
		{selector: "Bob", wantID: false},
		{selector: "Agent_aaaaaaaaaaaaaaaa", wantID: false},
	} {
		if got := messageAgentSelectorIsID(tc.selector); got != tc.wantID {
			t.Fatalf("selector %q id namespace = %t, want %t", tc.selector, got, tc.wantID)
		}
	}
}

func TestAppendMessageFromFilterUsesExactIDNamespace(t *testing.T) {
	for _, tc := range []struct {
		name     string
		selector string
		wantSQL  string
	}{
		{
			name:     "generated id is exact",
			selector: "agent_aaaaaaaaaaaaaaaa",
			wantSQL:  ` AND m.from_agent_id = $4`,
		},
		{
			name:     "ordinary name retains id precedence",
			selector: "Bob",
			wantSQL:  ` AND (m.from_agent_id = $4 OR sender.name = $4)`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := &strings.Builder{}
			args := []any{"acc_1", "realm_1", "agent_self"}
			appendMessageFromFilter(q, &args, tc.selector)
			if q.String() != tc.wantSQL {
				t.Fatalf("sender filter SQL = %q, want %q", q.String(), tc.wantSQL)
			}
			if len(args) != 4 || args[3] != tc.selector {
				t.Fatalf("sender filter args = %#v", args)
			}
		})
	}
}

func TestNormalizeMessageFilterAndCursor(t *testing.T) {
	filter, cursorTime, cursorID, err := normalizeMessageFilter(MessageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if filter.Direction != MessageDirectionInbox || filter.Limit != defaultMessagePageSize || !cursorTime.IsZero() || cursorID != "" {
		t.Fatalf("defaults = %#v / %v / %q", filter, cursorTime, cursorID)
	}

	wantTime := time.Unix(0, 1730000000123456789).UTC()
	cursor := encodeMessageCursor(wantTime, "msg_123")
	decodedTime, decodedID, err := decodeMessageCursor(cursor)
	if err != nil {
		t.Fatal(err)
	}
	if !decodedTime.Equal(wantTime) || decodedID != "msg_123" {
		t.Fatalf("decoded = %v / %q", decodedTime, decodedID)
	}
	for _, bad := range []string{"", "wat", "abc:msg_1", "1:not_message"} {
		if _, _, err := decodeMessageCursor(bad); !errors.Is(err, ErrMessageCursorInvalid) {
			t.Fatalf("cursor %q error = %v", bad, err)
		}
	}
	if _, _, _, err := normalizeMessageFilter(MessageFilter{Direction: MessageDirectionOutbox, Unread: true}); !errors.Is(err, ErrMessageInputInvalid) {
		t.Fatalf("outbox unread error = %v", err)
	}
	if _, _, _, err := normalizeMessageFilter(MessageFilter{Direction: MessageDirectionOutbox, Unacked: true}); !errors.Is(err, ErrMessageInputInvalid) {
		t.Fatalf("outbox unacked error = %v", err)
	}
	if _, _, _, err := normalizeMessageFilter(MessageFilter{OldestFirst: true, Cursor: cursor}); !errors.Is(err, ErrMessageInputInvalid) {
		t.Fatalf("oldest-first cursor error = %v", err)
	}
	oldest, oldestCursorTime, oldestCursorID, err := normalizeMessageFilter(MessageFilter{Unacked: true, OldestFirst: true, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !oldest.Unacked || !oldest.OldestFirst || oldest.Limit != 1 || !oldestCursorTime.IsZero() || oldestCursorID != "" {
		t.Fatalf("oldest unacknowledged filter = %#v / %v / %q", oldest, oldestCursorTime, oldestCursorID)
	}
}

func TestMessageRetryComparisonUsesJSONSemantics(t *testing.T) {
	msg := Message{
		To: MessageRecipient{ID: "agent_2"}, Subject: "handoff", Kind: "note",
		Body: "hello", Payload: json.RawMessage(`{"a":1,"b":2}`), ThreadID: "thr_1",
		ReplyToMessageID: "msg_parent",
	}
	in := SendMessageInput{
		Subject: "handoff", Kind: "note", Body: "hello",
		Payload: json.RawMessage(`{"b":2,"a":1}`), ThreadID: "thr_1",
		ReplyToMessageID: "msg_parent",
	}
	if !messageMatchesSend(msg, "agent_2", in) {
		t.Fatal("semantically identical retry did not match")
	}
	in.Body = "different"
	if messageMatchesSend(msg, "agent_2", in) {
		t.Fatal("different retry content matched")
	}
	in.Body = "hello"
	in.ReplyToMessageID = "msg_other_parent"
	if messageMatchesSend(msg, "agent_2", in) {
		t.Fatal("retry with a different causal parent matched")
	}
}

func TestMessageAuditMetadataContainsNoContent(t *testing.T) {
	msg := Message{
		ID: "msg_1", AccountID: "acc_1", RealmID: "realm_1",
		From: MessageAgent{ID: "agent_1"}, To: MessageRecipient{ID: "agent_2"},
		Subject: "secret subject", Kind: "request", Body: "private body",
		Payload: json.RawMessage(`{"secret":true}`), ThreadID: "thr_1",
		ReplyToMessageID: "msg_parent", CausalDepth: 2,
	}
	meta := messageEventMetadata(msg)
	for _, forbidden := range []string{"body", "payload", "subject"} {
		if _, ok := meta[forbidden]; ok {
			t.Fatalf("audit metadata contains %q: %#v", forbidden, meta)
		}
	}
	if meta["message_id"] != "msg_1" || meta["subject_present"] != true || meta["reply_to_message_id"] != "msg_parent" {
		t.Fatalf("audit metadata = %#v", meta)
	}
	for _, verb := range []string{
		VerbMessageSent, VerbMessageDelivered, VerbMessageDeliveryFailed,
		VerbMessageRead, VerbMessageAcked,
	} {
		actor := ActorAgent
		actorID := "agent_1"
		if verb == VerbMessageDelivered || verb == VerbMessageDeliveryFailed {
			actor, actorID = ActorSystem, ""
		}
		if err := checkEventShape(EventInput{AccountID: "acc_1", ActorKind: actor, ActorID: actorID, Verb: verb, Metadata: meta}); err != nil {
			t.Fatalf("%s metadata: %v", verb, err)
		}
	}
}

func TestNormalizeMessageProcessingInputs(t *testing.T) {
	if got, err := normalizeMessageLeaseDuration(0); err != nil || got != defaultMessageLeaseDuration {
		t.Fatalf("default lease = %v / %v", got, err)
	}
	for _, invalid := range []time.Duration{time.Second, 29 * time.Second, 16 * time.Minute} {
		if _, err := normalizeMessageLeaseDuration(invalid); !errors.Is(err, ErrMessageInputInvalid) {
			t.Fatalf("lease %v error = %v", invalid, err)
		}
	}
	if got, err := normalizeMessageLeaseDuration(30 * time.Second); err != nil || got != 30*time.Second {
		t.Fatalf("minimum lease = %v / %v", got, err)
	}
	if _, err := normalizeMessageProcessingKey("", "claim"); !errors.Is(err, ErrMessageInputInvalid) {
		t.Fatalf("empty processing key error = %v", err)
	}
	first, err := normalizeMessageProcessingKey(" retry-key ", "claim")
	if err != nil || !isSHA256Hex(first) || strings.Contains(first, "retry-key") {
		t.Fatalf("processing key hash = %q / %v", first, err)
	}
	second, err := normalizeMessageProcessingKey("retry-key", "completion")
	if err != nil || second != first {
		t.Fatalf("stable processing key hash = %q / %v, want %q", second, err, first)
	}
	fence, err := normalizeMessageClaimFence(MessageClaimFence{
		ClaimID: " mcl_aaaaaaaaaaaaaaaa ", ProcessingGeneration: 4,
	})
	if err != nil || fence.ClaimID != "mcl_aaaaaaaaaaaaaaaa" || fence.ProcessingGeneration != 4 {
		t.Fatalf("normalized fence = %#v / %v", fence, err)
	}
	for _, bad := range []MessageClaimFence{
		{}, {ClaimID: "claim_1", ProcessingGeneration: 1},
		{ClaimID: "mcl_aaaaaaaaaaaaaaaa", ProcessingGeneration: 0},
	} {
		if _, err := normalizeMessageClaimFence(bad); !errors.Is(err, ErrMessageInputInvalid) {
			t.Fatalf("fence %#v error = %v", bad, err)
		}
	}
}

func TestMessageProcessingAuditMetadataIsValueFree(t *testing.T) {
	msg := Message{
		ID: "msg_1", AccountID: "acc_1", RealmID: "rlm_1",
		From: MessageAgent{ID: "agt_sender"}, To: MessageRecipient{ID: "agt_worker"},
		Subject: "secret", Kind: "request", Body: "private body",
		Payload: json.RawMessage(`{"private":true}`), ThreadID: "thr_1",
		CausalDepth: 1,
		Processing:  MessageProcessing{State: MessageProcessingClaimed, Generation: 7},
	}
	metadata := messageEventMetadata(msg)
	metadata["processing_generation"] = "7"
	metadata["failure_count"] = "0"
	for _, verb := range []string{
		VerbMessageProcessingClaimed, VerbMessageProcessingRenewed, VerbMessageProcessingReleased,
	} {
		if err := checkEventShape(EventInput{
			AccountID: "acc_1", ActorKind: ActorAgent, ActorID: "agt_worker",
			Verb: verb, Metadata: metadata,
		}); err != nil {
			t.Fatalf("%s metadata: %v", verb, err)
		}
	}
	completed := maps.Clone(metadata)
	completed["result_message_id"] = "msg_result"
	if err := checkEventShape(EventInput{
		AccountID: "acc_1", ActorKind: ActorAgent, ActorID: "agt_worker",
		Verb: VerbMessageProcessingCompleted, Metadata: completed,
	}); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		"body", "payload", "subject", "claim_id", "claim_key_hash", "complete_key_hash", "idempotency_key",
	} {
		if _, present := completed[forbidden]; present {
			t.Fatalf("processing audit contains %q: %#v", forbidden, completed)
		}
	}
}

func TestMessageCompletionRetryAndGeneralFenceRedaction(t *testing.T) {
	expires := time.Now().UTC().Add(time.Minute)
	msg := Message{
		Subject: "answer", Kind: "result", Body: "done",
		Payload: json.RawMessage(`{"a":1,"b":2}`),
		Processing: MessageProcessing{
			State: MessageProcessingClaimed, Generation: 4,
			ClaimID: "mcl_aaaaaaaaaaaaaaaa", LeaseExpiresAt: &expires,
		},
	}
	draft := SendMessageInput{
		Subject: "answer", Kind: "result", Body: "done",
		Payload: json.RawMessage(`{"b":2,"a":1}`),
	}
	if !messageMatchesCompletion(msg, draft) {
		t.Fatal("semantically equal normalized completion retry did not match")
	}
	for _, mutate := range []func(*SendMessageInput){
		func(in *SendMessageInput) { in.Subject = "changed" },
		func(in *SendMessageInput) { in.Kind = "changed" },
		func(in *SendMessageInput) { in.Body = "changed" },
		func(in *SendMessageInput) { in.Payload = json.RawMessage(`{"a":2,"b":2}`) },
	} {
		changed := draft
		mutate(&changed)
		if messageMatchesCompletion(msg, changed) {
			t.Fatalf("changed completion retry matched: %#v", changed)
		}
	}
	redacted := redactMessageProcessingFence(msg)
	if redacted.Processing.ClaimID != "" || redacted.Processing.LeaseExpiresAt != nil ||
		redacted.Processing.State != MessageProcessingClaimed || redacted.Processing.Generation != 4 {
		t.Fatalf("redacted projection = %#v", redacted.Processing)
	}
	if msg.Processing.ClaimID == "" || msg.Processing.LeaseExpiresAt == nil {
		t.Fatal("redaction mutated the source message")
	}
}
