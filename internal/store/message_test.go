package store

import (
	"encoding/json"
	"errors"
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
	if in.ToAgent != "peer" || in.Subject != "handoff" || in.Kind != "note" || in.IdempotencyKey != "retry-1" {
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
}

func TestMessageRetryComparisonUsesJSONSemantics(t *testing.T) {
	msg := Message{
		To: MessageRecipient{ID: "agent_2"}, Subject: "handoff", Kind: "note",
		Body: "hello", Payload: json.RawMessage(`{"a":1,"b":2}`), ThreadID: "thr_1",
	}
	in := SendMessageInput{
		Subject: "handoff", Kind: "note", Body: "hello",
		Payload: json.RawMessage(`{"b":2,"a":1}`), ThreadID: "thr_1",
	}
	if !messageMatchesSend(msg, "agent_2", in) {
		t.Fatal("semantically identical retry did not match")
	}
	in.Body = "different"
	if messageMatchesSend(msg, "agent_2", in) {
		t.Fatal("different retry content matched")
	}
}

func TestMessageAuditMetadataContainsNoContent(t *testing.T) {
	msg := Message{
		ID: "msg_1", AccountID: "acc_1", RealmID: "realm_1",
		From: MessageAgent{ID: "agent_1"}, To: MessageRecipient{ID: "agent_2"},
		Subject: "secret subject", Kind: "request", Body: "private body",
		Payload: json.RawMessage(`{"secret":true}`), ThreadID: "thr_1",
	}
	meta := messageEventMetadata(msg)
	for _, forbidden := range []string{"body", "payload", "subject"} {
		if _, ok := meta[forbidden]; ok {
			t.Fatalf("audit metadata contains %q: %#v", forbidden, meta)
		}
	}
	if meta["message_id"] != "msg_1" || meta["subject_present"] != true {
		t.Fatalf("audit metadata = %#v", meta)
	}
	for _, verb := range []string{VerbMessageSent, VerbMessageDelivered, VerbMessageRead, VerbMessageAcked} {
		actor := ActorAgent
		actorID := "agent_1"
		if verb == VerbMessageDelivered {
			actor, actorID = ActorSystem, ""
		}
		if err := checkEventShape(EventInput{AccountID: "acc_1", ActorKind: actor, ActorID: actorID, Verb: verb, Metadata: meta}); err != nil {
			t.Fatalf("%s metadata: %v", verb, err)
		}
	}
}
