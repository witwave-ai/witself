package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/testenv"
)

func TestMessagePeekObservationalPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	st, actors, msg := newMessagePeekFixture(ctx, t)
	recipient := actors["recipient"]
	check := func(p Principal, id, readState, processing string, generation int64) {
		t.Helper()
		before := messagePeekSnapshot(ctx, t, st)
		got, err := st.PeekMessage(ctx, p, id)
		if after := messagePeekSnapshot(ctx, t, st); before != after {
			t.Fatal("observational peek mutated persisted message, delivery, event, or usage state")
		}
		var payload map[string]int
		payloadErr := json.Unmarshal(got.Payload, &payload)
		if err != nil || got.ID != id || got.Body != "synthetic observational body" ||
			payloadErr != nil || len(payload) != 1 || payload["task"] != 42 || got.ReadState.State != readState ||
			got.Processing.State != processing || got.Processing.Generation != generation {
			t.Fatalf("peek did not return exact recipient content/state: %v", err)
		}
		if got.Processing.ClaimID != "" || got.Processing.LeaseExpiresAt != nil {
			t.Fatal("store peek exposed a processing capability")
		}
	}
	check(recipient, msg.ID, MessageReadUnread, MessageProcessingAvailable, 0)
	check(recipient, msg.ID, MessageReadUnread, MessageProcessingAvailable, 0)
	claim, err := st.ClaimMessage(ctx, recipient, msg.ID, ClaimMessageInput{IdempotencyKey: "peek-claim"})
	if err != nil || claim.Processing.ClaimID == "" || claim.Processing.LeaseExpiresAt == nil {
		t.Fatalf("fixture did not acquire an actual processing capability: %v", err)
	}
	check(recipient, msg.ID, MessageReadUnread, MessageProcessingClaimed, claim.Processing.Generation)
	if _, err := st.ReleaseMessageClaim(ctx, recipient, msg.ID, ReleaseMessageClaimInput{
		ClaimID: claim.Processing.ClaimID, ProcessingGeneration: claim.Processing.Generation,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReadMessage(ctx, recipient, msg.ID); err != nil {
		t.Fatal(err)
	}
	check(recipient, msg.ID, MessageReadRead, MessageProcessingAvailable, claim.Processing.Generation)
	if _, err := st.AckMessage(ctx, recipient, msg.ID); err != nil {
		t.Fatal(err)
	}
	check(recipient, msg.ID, MessageReadAcked, MessageProcessingAvailable, claim.Processing.Generation)

	// Fan-out shares content but never another recipient's read state.
	fanout, err := st.SendMessage(ctx, actors["sender"], SendMessageInput{
		AudienceKind: MessageRecipientAgents, ToAgents: []string{recipient.ID, actors["bystander"].ID},
		Body: msg.Body, Payload: msg.Payload, IdempotencyKey: "peek-fanout",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReadMessage(ctx, recipient, fanout.ID); err != nil {
		t.Fatal(err)
	}
	check(recipient, fanout.ID, MessageReadRead, MessageProcessingAvailable, 0)
	check(actors["bystander"], fanout.ID, MessageReadUnread, MessageProcessingAvailable, 0)
	for _, direction := range []string{MessageDirectionInbox, MessageDirectionOutbox} {
		p := recipient
		if direction == MessageDirectionOutbox {
			p = actors["sender"]
		}
		page, err := st.ListMessages(ctx, p, MessageFilter{Direction: direction})
		if err != nil || len(page.Messages) == 0 {
			t.Fatalf("passive mailbox fixture: %v", err)
		}
		for _, listed := range page.Messages {
			if listed.Body != "" || len(listed.Payload) != 0 {
				t.Fatal("peek widened passive list content")
			}
		}
	}
}

func TestMessagePeekAuthorizationPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	st, actors, msg := newMessagePeekFixture(ctx, t)
	refused := func(t *testing.T, p Principal, id string, want error) {
		t.Helper()
		before := messagePeekSnapshot(ctx, t, st)
		got, err := st.PeekMessage(ctx, p, id)
		if !errors.Is(err, want) || got.ID != "" || got.Body != "" || len(got.Payload) != 0 {
			t.Fatalf("peek refusal = %v, want %v and no message", err, want)
		}
		if after := messagePeekSnapshot(ctx, t, st); before != after {
			t.Fatal("refused peek mutated message, delivery, event, or usage state")
		}
	}
	for _, name := range []string{"sender", "bystander", "other realm", "other account"} {
		t.Run(name, func(t *testing.T) { refused(t, actors[name], msg.ID, ErrMessageNotFound) })
	}
	operator := actors["recipient"]
	operator.Kind = PrincipalOperator
	refused(t, operator, msg.ID, ErrMessageForbidden)
	refused(t, actors["recipient"], " \t", ErrMessageInputInvalid)
	refused(t, actors["recipient"], "msg_missing", ErrMessageNotFound)
	forgedRealm := actors["recipient"]
	forgedRealm.RealmID = actors["other realm"].RealmID
	refused(t, forgedRealm, msg.ID, ErrAgentNotFound)
	forgedAccount := actors["recipient"]
	forgedAccount.AccountID = actors["other account"].AccountID
	refused(t, forgedAccount, msg.ID, ErrAgentNotFound)

	// Use real lifecycle writes, then reuse the formerly authenticated principal
	// to prove that the store rechecks current state rather than trusting it.
	stillLive, err := st.SendMessage(ctx, actors["sender"], SendMessageInput{
		ToAgent: actors["bystander"].ID, Body: msg.Body, IdempotencyKey: "peek-suspension-fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteAgent(ctx, actors["recipient"].AccountID, actors["recipient"].RealmID, actors["recipient"].ID); err != nil {
		t.Fatal(err)
	}
	refused(t, actors["recipient"], msg.ID, ErrAgentNotFound)
	if err := st.SuspendAccountSystem(ctx, actors["sender"].AccountID, "evacuation", "synthetic peek refusal"); err != nil {
		t.Fatal(err)
	}
	refused(t, actors["bystander"], stillLive.ID, ErrAccountNotActive)
}

func newMessagePeekFixture(ctx context.Context, t *testing.T) (*Store, map[string]Principal, Message) {
	t.Helper()
	st, _ := newMigrationTestStore(t, testenv.RequirePostgres(t))
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	account := func(name string) string {
		t.Helper()
		created, err := st.ProvisionAccount(ctx, name+"@witwave.ai", name, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if ok, err := st.ActivateAccount(ctx, created.AccountID); err != nil || !ok {
			t.Fatalf("activate fixture: %v", err)
		}
		return created.AccountID
	}
	first, second := account("message-peek-first"), account("message-peek-second")
	primary, err := st.CreateRealm(ctx, first, "primary")
	if err != nil {
		t.Fatal(err)
	}
	otherRealm, err := st.CreateRealm(ctx, first, "other")
	if err != nil {
		t.Fatal(err)
	}
	otherAccountRealm, err := st.CreateRealm(ctx, second, "primary")
	if err != nil {
		t.Fatal(err)
	}
	actors := make(map[string]Principal)
	for _, actor := range []struct{ label, name, account, realm string }{
		{"sender", "sender", first, primary.ID},
		{"recipient", "recipient", first, primary.ID},
		{"bystander", "bystander", first, primary.ID},
		{"other realm", "other-realm", first, otherRealm.ID},
		{"other account", "other-account", second, otherAccountRealm.ID},
	} {
		agent, err := st.CreateAgent(ctx, actor.account, actor.realm, actor.name)
		if err != nil {
			t.Fatal(err)
		}
		actors[actor.label] = Principal{Kind: PrincipalAgent, ID: agent.ID, AccountID: actor.account,
			RealmID: actor.realm, AgentName: agent.Name, AccountStatus: "active"}
	}
	msg, err := st.SendMessage(ctx, actors["sender"], SendMessageInput{
		ToAgent: actors["recipient"].ID, Body: "synthetic observational body",
		Payload: json.RawMessage(`{"task":42}`), IdempotencyKey: "peek-fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	var events, usage int
	if err := st.pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM account_events WHERE account_id=$1),
		(SELECT count(*) FROM usage_events WHERE account_id=$1)`, first).Scan(&events, &usage); err != nil {
		t.Fatal(err)
	}
	if events == 0 || usage == 0 {
		t.Fatal("fixture must contain durable audit and usage rows")
	}
	return st, actors, msg
}

// Every fixture has its own schema, so comparing all rows also catches writes
// attributed to a wrong caller account. PostgreSQL locks themselves are not
// application state and deliberately do not appear in this snapshot.
func messagePeekSnapshot(ctx context.Context, t *testing.T, st *Store) string {
	t.Helper()
	var snapshot string
	if err := st.pool.QueryRow(ctx, `SELECT jsonb_build_object(
		'messages',(SELECT jsonb_agg(to_jsonb(m) ORDER BY id) FROM agent_messages m),
		'deliveries',(SELECT jsonb_agg(to_jsonb(d) ORDER BY message_id,recipient_agent_id) FROM agent_message_deliveries d),
		'events',(SELECT jsonb_agg(to_jsonb(e) ORDER BY id) FROM account_events e),
		'usage',(SELECT jsonb_agg(to_jsonb(u) ORDER BY id) FROM usage_events u),
		'rollups',(SELECT jsonb_agg(to_jsonb(u) ORDER BY agent_id,dimension,unit,bucket,bucket_start) FROM usage_rollups u)
	)::text`).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot
}
