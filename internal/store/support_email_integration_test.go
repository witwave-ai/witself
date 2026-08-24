package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/plans"
)

func TestSupportEmailIntakePostgres(t *testing.T) {
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
	// Register pool closure first. Cleanups run LIFO, so the row cleanup
	// registered below executes before the pool is closed.
	t.Cleanup(st.Close)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}

	suffix := time.Now().UnixNano()
	email := fmt.Sprintf("support-email-%d@witwave.ai", suffix)
	provisioned, err := st.ProvisionAccount(
		ctx,
		email,
		fmt.Sprintf("support email %d", suffix),
		time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	accountID := provisioned.AccountID
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		for _, query := range []string{
			`DELETE FROM support_ticket_messages WHERE account_id = $1`,
			`DELETE FROM support_tickets WHERE account_id = $1`,
			`DELETE FROM account_events WHERE account_id = $1`,
			`DELETE FROM tokens WHERE operator_id IN
			   (SELECT id FROM operators WHERE account_id = $1)`,
			`DELETE FROM operators WHERE account_id = $1`,
			`DELETE FROM accounts WHERE id = $1`,
		} {
			if _, err := st.pool.Exec(cleanupCtx, query, accountID); err != nil {
				t.Errorf("cleanup %q: %v", query, err)
			}
		}
	})
	if activated, err := st.ActivateAccount(ctx, accountID); err != nil || !activated {
		t.Fatalf("activate = %v / %v", activated, err)
	}

	matches, err := st.FindAccountsByContactEmail(ctx, strings.ToUpper(email))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 || matches[0] != (SupportContactMatch{AccountID: accountID, Status: "active"}) {
		t.Fatalf("contact matches = %#v", matches)
	}

	applyPlan := func(revision int64, features []string) {
		t.Helper()
		hash, err := plans.SnapshotHash("support-email-test", map[string]int64{}, map[string]int64{}, features)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.SetAccountPlan(
			ctx, accountID, revision, hash, "support-email-test",
			map[string]int64{}, map[string]int64{}, features,
		); err != nil {
			t.Fatal(err)
		}
	}

	openInput := OpenTicketFromEmailInput{
		AccountID:      accountID,
		SenderEmail:    email,
		Subject:        "email intake",
		Body:           "opening email body",
		EmailMessageID: "<opening@example.test>",
	}
	applyPlan(1, []string{"memory"})
	if _, _, err := st.OpenTicketFromEmail(ctx, openInput); !errors.Is(err, ErrSupportNotIncluded) {
		t.Fatalf("open without entitlement = %v, want ErrSupportNotIncluded", err)
	}

	applyPlan(2, []string{"memory", plans.SupportFeature})
	mismatchInput := openInput
	mismatchInput.SenderEmail = "different@example.test"
	if _, _, err := st.OpenTicketFromEmail(ctx, mismatchInput); !errors.Is(err, ErrSupportSenderMismatch) {
		t.Fatalf("open sender mismatch = %v, want ErrSupportSenderMismatch", err)
	}

	ticket, opening, err := st.OpenTicketFromEmail(ctx, openInput)
	if err != nil {
		t.Fatal(err)
	}
	if ticket.OpenedByKind != MessageAuthorOwner || ticket.OpenedByID != provisioned.OperatorID ||
		opening.AuthorKind != MessageAuthorOwner || opening.AuthorID != provisioned.OperatorID {
		t.Fatalf("email opening attribution = ticket %s:%s message %s:%s, want owner:%s",
			ticket.OpenedByKind, ticket.OpenedByID,
			opening.AuthorKind, opening.AuthorID, provisioned.OperatorID)
	}
	if ticket.Category != TicketCategoryOther || ticket.Priority != TicketPriorityNormal {
		t.Fatalf("email opening category/priority = %s/%s", ticket.Category, ticket.Priority)
	}
	assertSupportEmailMetadata(t, opening.Metadata, "<opening@example.test>")

	replayedOpen := openInput
	replayedOpen.Subject = "a replay must keep the original subject"
	replayedOpen.Body = "a replay must keep the original body"
	replayedTicket, replayedOpening, err := st.OpenTicketFromEmail(ctx, replayedOpen)
	if err != nil {
		t.Fatal(err)
	}
	if replayedTicket.ID != ticket.ID || replayedOpening.ID != opening.ID ||
		replayedTicket.Subject != ticket.Subject || replayedOpening.Body != opening.Body {
		t.Fatalf("replayed email opening = ticket %s/%q message %s/%q, want %s/%q and %s/%q",
			replayedTicket.ID, replayedTicket.Subject, replayedOpening.ID, replayedOpening.Body,
			ticket.ID, ticket.Subject, opening.ID, opening.Body)
	}
	assertSupportEmailMessageIDCount(t, st, ctx, accountID, openInput.EmailMessageID, 1)
	var ticketCount int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM support_tickets WHERE account_id = $1`,
		accountID).Scan(&ticketCount); err != nil {
		t.Fatal(err)
	}
	if ticketCount != 1 {
		t.Fatalf("support ticket count after replay = %d, want 1", ticketCount)
	}
	replayedOpen.SenderEmail = "different@example.test"
	if _, _, err := st.OpenTicketFromEmail(ctx, replayedOpen); !errors.Is(err, ErrSupportSenderMismatch) {
		t.Fatalf("replayed open sender mismatch = %v, want ErrSupportSenderMismatch", err)
	}

	var auditChannel string
	if err := st.pool.QueryRow(ctx,
		`SELECT metadata->>'channel'
		 FROM account_events
		 WHERE account_id = $1 AND verb = $2 AND metadata->>'ticket_id' = $3`,
		accountID, VerbSupportTicketOpened, ticket.ID).Scan(&auditChannel); err != nil {
		t.Fatal(err)
	}
	if auditChannel != "email" {
		t.Fatalf("opening audit channel = %q, want email", auditChannel)
	}

	replyInput := ReplyToTicketFromEmailInput{
		AccountID:      accountID,
		SenderEmail:    email,
		TicketID:       ticket.ID,
		Body:           "reply email body",
		EmailMessageID: "<reply@example.test>",
	}
	replyMismatch := replyInput
	replyMismatch.SenderEmail = "different@example.test"
	if _, err := st.ReplyToTicketFromEmail(ctx, replyMismatch); !errors.Is(err, ErrSupportSenderMismatch) {
		t.Fatalf("reply sender mismatch = %v, want ErrSupportSenderMismatch", err)
	}
	reply, err := st.ReplyToTicketFromEmail(ctx, replyInput)
	if err != nil {
		t.Fatal(err)
	}
	if reply.AuthorKind != MessageAuthorOwner || reply.AuthorID != provisioned.OperatorID {
		t.Fatalf("email reply attribution = %s:%s, want owner:%s",
			reply.AuthorKind, reply.AuthorID, provisioned.OperatorID)
	}
	assertSupportEmailMetadata(t, reply.Metadata, "<reply@example.test>")
	replayedReplyInput := replyInput
	replayedReplyInput.Body = "a replay must keep the original reply body"
	replayedReply, err := st.ReplyToTicketFromEmail(ctx, replayedReplyInput)
	if err != nil {
		t.Fatal(err)
	}
	if replayedReply.ID != reply.ID || replayedReply.Body != reply.Body {
		t.Fatalf("replayed email reply = %s/%q, want %s/%q",
			replayedReply.ID, replayedReply.Body, reply.ID, reply.Body)
	}
	assertSupportEmailMessageIDCount(t, st, ctx, accountID, replyInput.EmailMessageID, 1)
	replayedReplyInput.SenderEmail = "different@example.test"
	if _, err := st.ReplyToTicketFromEmail(ctx, replayedReplyInput); !errors.Is(err, ErrSupportSenderMismatch) {
		t.Fatalf("replayed reply sender mismatch = %v, want ErrSupportSenderMismatch", err)
	}

	type openResult struct {
		ticket  Ticket
		message TicketMessage
		err     error
	}
	concurrentOpenInput := openInput
	concurrentOpenInput.Subject = "concurrent email intake"
	concurrentOpenInput.Body = "concurrent opening body"
	concurrentOpenInput.EmailMessageID = "<concurrent-opening@example.test>"
	openStart := make(chan struct{})
	openResults := make(chan openResult, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-openStart
			gotTicket, gotMessage, gotErr := st.OpenTicketFromEmail(ctx, concurrentOpenInput)
			openResults <- openResult{ticket: gotTicket, message: gotMessage, err: gotErr}
		}()
	}
	close(openStart)
	firstOpen := <-openResults
	secondOpen := <-openResults
	if firstOpen.err != nil || secondOpen.err != nil {
		t.Fatalf("concurrent email opens = %v / %v", firstOpen.err, secondOpen.err)
	}
	if firstOpen.ticket.ID != secondOpen.ticket.ID || firstOpen.message.ID != secondOpen.message.ID {
		t.Fatalf("concurrent email opens = ticket %s/%s message %s/%s, want identical ids",
			firstOpen.ticket.ID, secondOpen.ticket.ID, firstOpen.message.ID, secondOpen.message.ID)
	}
	assertSupportEmailMessageIDCount(t, st, ctx, accountID, concurrentOpenInput.EmailMessageID, 1)

	type replyResult struct {
		message TicketMessage
		err     error
	}
	concurrentReplyInput := replyInput
	concurrentReplyInput.TicketID = firstOpen.ticket.ID
	concurrentReplyInput.Body = "concurrent reply body"
	concurrentReplyInput.EmailMessageID = "<concurrent-reply@example.test>"
	replyStart := make(chan struct{})
	replyResults := make(chan replyResult, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-replyStart
			gotMessage, gotErr := st.ReplyToTicketFromEmail(ctx, concurrentReplyInput)
			replyResults <- replyResult{message: gotMessage, err: gotErr}
		}()
	}
	close(replyStart)
	firstReply := <-replyResults
	secondReply := <-replyResults
	if firstReply.err != nil || secondReply.err != nil {
		t.Fatalf("concurrent email replies = %v / %v", firstReply.err, secondReply.err)
	}
	if firstReply.message.ID != secondReply.message.ID {
		t.Fatalf("concurrent email replies = message %s/%s, want identical ids",
			firstReply.message.ID, secondReply.message.ID)
	}
	assertSupportEmailMessageIDCount(t, st, ctx, accountID, concurrentReplyInput.EmailMessageID, 1)

	for _, state := range []string{TicketStateResolved, TicketStateClosed} {
		if _, err := st.ChangeTicketState(ctx, ChangeTicketStateInput{
			AccountID: accountID, OperatorID: provisioned.OperatorID,
			TicketID: ticket.ID, NewState: state,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.ReplyToTicketFromEmail(ctx, replyInput); !errors.Is(err, ErrTicketStateInvalid) {
		t.Fatalf("reply to closed ticket = %v, want ErrTicketStateInvalid", err)
	}
}

func assertSupportEmailMessageIDCount(t *testing.T, st *Store, ctx context.Context, accountID, messageID string, want int) {
	t.Helper()
	var count int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*)
		 FROM support_ticket_messages
		 WHERE account_id = $1 AND metadata->>'email_message_id' = $2`,
		accountID, strings.TrimSpace(messageID)).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Fatalf("support email message count for %q = %d, want %d", messageID, count, want)
	}
}

func assertSupportEmailMetadata(t *testing.T, raw json.RawMessage, messageID string) {
	t.Helper()
	var metadata map[string]string
	if err := json.Unmarshal(raw, &metadata); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"channel":          "email",
		"email_message_id": messageID,
	}
	if len(metadata) != len(want) || metadata["channel"] != want["channel"] ||
		metadata["email_message_id"] != want["email_message_id"] {
		t.Fatalf("support email metadata = %#v, want %#v", metadata, want)
	}
}
