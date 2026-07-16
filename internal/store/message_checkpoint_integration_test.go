package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func TestSelfMessageCheckpointTracksDurableForegroundWorkPostgres(t *testing.T) {
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

	provisioned, err := st.ProvisionAccount(ctx, "message-checkpoint@witwave.ai", "message checkpoint", time.Hour)
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
	createAgent := func(name string) Agent {
		t.Helper()
		agent, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		return agent
	}
	principal := func(agent Agent) Principal {
		return Principal{Kind: PrincipalAgent, ID: agent.ID, AccountID: provisioned.AccountID,
			RealmID: realm.ID, AgentName: agent.Name, AccountStatus: "active"}
	}
	scott := createAgent("scott")
	bob := createAgent("bob")
	alice := createAgent("alice")
	scottPrincipal, bobPrincipal, alicePrincipal := principal(scott), principal(bob), principal(alice)

	assertCheckpoint := func(p Principal, want SelfMessageCheckpoint) {
		t.Helper()
		got, err := st.GetSelfMessageCheckpoint(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("checkpoint for %s = %+v, want %+v", p.AgentName, got, want)
		}
	}
	assertCheckpoint(scottPrincipal, SelfMessageCheckpoint{})

	direct, err := st.SendMessage(ctx, scottPrincipal, SendMessageInput{
		ToAgent: bob.ID, Body: "Please inspect this request.", IdempotencyKey: "message-checkpoint-direct",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertCheckpoint(bobPrincipal, SelfMessageCheckpoint{Pending: true, MailboxPending: true})
	if _, err := st.ReadMessage(ctx, bobPrincipal, direct.ID); err != nil {
		t.Fatal(err)
	}
	assertCheckpoint(bobPrincipal, SelfMessageCheckpoint{Pending: true, MailboxPending: true})
	if _, err := st.AckMessage(ctx, bobPrincipal, direct.ID); err != nil {
		t.Fatal(err)
	}
	assertCheckpoint(bobPrincipal, SelfMessageCheckpoint{})

	opened, err := st.OpenMessageRequest(ctx, scottPrincipal, OpenMessageRequestInput{
		Body: "Offer a bounded approach.", OfferWindow: 10 * time.Minute, ExpiresIn: time.Hour,
		IdempotencyKey: "message-checkpoint-open",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertCheckpoint(bobPrincipal, SelfMessageCheckpoint{
		Pending: true, MailboxPending: true, CandidateOfferPending: true,
	})
	assertCheckpoint(alicePrincipal, SelfMessageCheckpoint{
		Pending: true, MailboxPending: true, CandidateOfferPending: true,
	})
	if _, err := st.AckMessage(ctx, bobPrincipal, opened.OpeningMessage.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AckMessage(ctx, alicePrincipal, opened.OpeningMessage.ID); err != nil {
		t.Fatal(err)
	}
	assertCheckpoint(bobPrincipal, SelfMessageCheckpoint{Pending: true, CandidateOfferPending: true})

	offered, err := st.OfferMessageRequest(ctx, bobPrincipal, opened.Request.ID, OfferMessageRequestInput{
		Body: "I can handle it.", IdempotencyKey: "message-checkpoint-offer",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DeclineMessageRequest(ctx, alicePrincipal, opened.Request.ID, DeclineMessageRequestInput{
		IdempotencyKey: "message-checkpoint-decline",
	}); err != nil {
		t.Fatal(err)
	}
	assertCheckpoint(bobPrincipal, SelfMessageCheckpoint{})
	assertCheckpoint(scottPrincipal, SelfMessageCheckpoint{
		Pending: true, MailboxPending: true, CoordinatorSelectionPending: true,
	})
	if _, err := st.AckMessage(ctx, scottPrincipal, offered.Offer.Message.ID); err != nil {
		t.Fatal(err)
	}
	assertCheckpoint(scottPrincipal, SelfMessageCheckpoint{Pending: true, CoordinatorSelectionPending: true})

	if _, err := st.SelectMessageRequest(ctx, scottPrincipal, opened.Request.ID, SelectMessageRequestInput{
		SelectedAgentIDs: []string{bob.ID}, Reservation: 5 * time.Minute,
		IdempotencyKey: "message-checkpoint-select",
	}); err != nil {
		t.Fatal(err)
	}
	assertCheckpoint(scottPrincipal, SelfMessageCheckpoint{})
	assertCheckpoint(bobPrincipal, SelfMessageCheckpoint{Pending: true, CandidateAssignmentPending: true})
	assertCheckpoint(alicePrincipal, SelfMessageCheckpoint{})

	if _, err := st.CancelMessageRequest(ctx, scottPrincipal, opened.Request.ID); err != nil {
		t.Fatal(err)
	}
	assertCheckpoint(bobPrincipal, SelfMessageCheckpoint{})

	if _, err := st.GetSelfMessageCheckpoint(ctx, Principal{Kind: PrincipalOperator}); !errors.Is(err, ErrMessageForbidden) {
		t.Fatalf("operator checkpoint error = %v, want ErrMessageForbidden", err)
	}
	if err := st.DeleteAgent(ctx, provisioned.AccountID, realm.ID, bob.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetSelfMessageCheckpoint(ctx, bobPrincipal); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("deleted-agent checkpoint error = %v, want ErrAgentNotFound", err)
	}
}
