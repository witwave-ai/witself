package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/witwave-ai/witself/internal/id"
)

// SupportContactMatch is an active account whose contact email matches a
// support-email sender. FindAccountsByContactEmail bounds the returned set so
// a malformed or unexpectedly non-unique directory cannot create an
// unbounded control-plane response.
type SupportContactMatch struct {
	AccountID string
	Status    string
}

// OpenTicketFromEmailInput carries one authenticated support-email intake
// request into the account transaction.
type OpenTicketFromEmailInput struct {
	AccountID      string
	SenderEmail    string
	Subject        string
	Body           string
	EmailMessageID string
}

// ReplyToTicketFromEmailInput carries one authenticated support-email reply
// into the account transaction.
type ReplyToTicketFromEmailInput struct {
	AccountID      string
	SenderEmail    string
	TicketID       string
	Body           string
	EmailMessageID string
}

// ErrSupportSenderMismatch is returned when an email intake sender no longer
// matches the account's contact email inside the ticket transaction.
var ErrSupportSenderMismatch = errors.New("support sender email does not match account contact email")

// FindAccountsByContactEmail returns at most ten active accounts whose contact
// email matches email case-insensitively. An empty slice is returned when
// there are no matches.
func (s *Store) FindAccountsByContactEmail(ctx context.Context, email string) ([]SupportContactMatch, error) {
	email = strings.TrimSpace(email)
	if email == "" {
		return nil, fmt.Errorf("%w: sender email required", ErrTicketInputInvalid)
	}

	rows, err := s.pool.Query(ctx,
		`SELECT id, status
		 FROM accounts
		 WHERE lower(email) = lower($1)
		   AND status = 'active'
		 ORDER BY id
		 LIMIT 10`,
		email)
	if err != nil {
		return nil, fmt.Errorf("find support contact accounts: %w", err)
	}
	defer rows.Close()

	matches := make([]SupportContactMatch, 0)
	for rows.Next() {
		var match SupportContactMatch
		if err := rows.Scan(&match.AccountID, &match.Status); err != nil {
			return nil, fmt.Errorf("scan support contact account: %w", err)
		}
		matches = append(matches, match)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("find support contact accounts: %w", err)
	}
	return matches, nil
}

// OpenTicketFromEmail creates an owner-attributed support ticket from an
// authenticated email. The account status, support policy, plan entitlement,
// sender-email match, and owner attribution are all decided in the same
// transaction as the ticket, first message, and audit event.
func (s *Store) OpenTicketFromEmail(ctx context.Context, in OpenTicketFromEmailInput) (Ticket, TicketMessage, error) {
	in.AccountID = strings.TrimSpace(in.AccountID)
	in.SenderEmail = strings.TrimSpace(in.SenderEmail)
	subject := strings.TrimSpace(in.Subject)
	body := strings.TrimSpace(in.Body)
	if in.AccountID == "" {
		return Ticket{}, TicketMessage{}, fmt.Errorf("%w: account id required", ErrTicketInputInvalid)
	}
	if in.SenderEmail == "" {
		return Ticket{}, TicketMessage{}, fmt.Errorf("%w: sender email required", ErrTicketInputInvalid)
	}
	if subject == "" {
		return Ticket{}, TicketMessage{}, fmt.Errorf("%w: subject required", ErrTicketInputInvalid)
	}
	if supportSubjectTooLong(subject) {
		return Ticket{}, TicketMessage{}, fmt.Errorf("%w: subject exceeds %d characters", ErrTicketInputInvalid, maxSupportSubjectChars)
	}
	if body == "" {
		return Ticket{}, TicketMessage{}, fmt.Errorf("%w: body required", ErrTicketInputInvalid)
	}
	if len(body) > maxSupportBodyBytes {
		return Ticket{}, TicketMessage{}, fmt.Errorf("%w: body exceeds %d bytes", ErrTicketInputInvalid, maxSupportBodyBytes)
	}
	emailMessageID := strings.TrimSpace(in.EmailMessageID)
	messageMetadata, err := supportEmailMessageMetadata(emailMessageID)
	if err != nil {
		return Ticket{}, TicketMessage{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Ticket{}, TicketMessage{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	authority, err := supportEmailAuthorityTx(ctx, tx, in.AccountID, in.SenderEmail)
	if err != nil {
		return Ticket{}, TicketMessage{}, err
	}
	if authority.status != "active" {
		return Ticket{}, TicketMessage{}, ErrAccountNotActive
	}
	if authority.supportPolicy != "enabled" {
		return Ticket{}, TicketMessage{}, ErrSupportDisabled
	}
	if authority.planRevision > 0 && !authority.planHasSupport {
		return Ticket{}, TicketMessage{}, ErrSupportNotIncluded
	}
	if !authority.senderMatches {
		return Ticket{}, TicketMessage{}, ErrSupportSenderMismatch
	}
	if !authority.ownerID.Valid {
		return Ticket{}, TicketMessage{}, errors.New("support email account has no root or account owner")
	}
	if existing, ok, err := existingSupportEmailMessageTx(ctx, tx, in.AccountID, emailMessageID); err != nil {
		return Ticket{}, TicketMessage{}, err
	} else if ok {
		ticket, err := readTicketRow(ctx, tx, in.AccountID, existing.TicketID)
		if err != nil {
			return Ticket{}, TicketMessage{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return Ticket{}, TicketMessage{}, err
		}
		return ticket, existing, nil
	}

	ticketID, err := id.New("tkt")
	if err != nil {
		return Ticket{}, TicketMessage{}, err
	}
	messageID, err := id.New("tkm")
	if err != nil {
		return Ticket{}, TicketMessage{}, err
	}

	var ticket Ticket
	err = tx.QueryRow(ctx,
		`INSERT INTO support_tickets
		   (id, account_id, opened_by_kind, opened_by_id,
		    subject, category, state, priority, last_message_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		 RETURNING id, account_id, opened_at, opened_by_kind, opened_by_id,
		           subject, category, state, priority,
		           first_response_at, resolved_at, closed_at,
		           last_activity_at, COALESCE(last_message_id, ''),
		           correlation, metadata`,
		ticketID, in.AccountID, MessageAuthorOwner, authority.ownerID.String,
		subject, TicketCategoryOther, TicketStateAwaitingAdmin,
		TicketPriorityNormal, messageID,
	).Scan(&ticket.ID, &ticket.AccountID, &ticket.OpenedAt,
		&ticket.OpenedByKind, &ticket.OpenedByID, &ticket.Subject,
		&ticket.Category, &ticket.State, &ticket.Priority,
		&ticket.FirstResponseAt, &ticket.ResolvedAt, &ticket.ClosedAt,
		&ticket.LastActivityAt, &ticket.LastMessageID,
		&ticket.Correlation, &ticket.Metadata)
	if err != nil {
		return Ticket{}, TicketMessage{}, fmt.Errorf("insert email support ticket: %w", err)
	}

	var message TicketMessage
	err = tx.QueryRow(ctx,
		`INSERT INTO support_ticket_messages
		   (id, ticket_id, account_id, author_kind, author_id, body, metadata)
		 VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb)
		 RETURNING id, ticket_id, account_id, posted_at, author_kind,
		           COALESCE(author_id, ''), body, attachments, metadata`,
		messageID, ticketID, in.AccountID, MessageAuthorOwner,
		authority.ownerID.String, body, messageMetadata,
	).Scan(&message.ID, &message.TicketID, &message.AccountID,
		&message.PostedAt, &message.AuthorKind, &message.AuthorID,
		&message.Body, &message.Attachments, &message.Metadata)
	if err != nil {
		return Ticket{}, TicketMessage{}, fmt.Errorf("insert email support message: %w", err)
	}

	if err := s.logEventTx(ctx, tx, EventInput{
		AccountID: in.AccountID,
		ActorKind: MessageAuthorOwner,
		ActorID:   authority.ownerID.String,
		Verb:      VerbSupportTicketOpened,
		Metadata: map[string]any{
			"ticket_id": ticketID,
			"subject":   subject,
			"category":  TicketCategoryOther,
			"channel":   "email",
		},
	}); err != nil {
		return Ticket{}, TicketMessage{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return Ticket{}, TicketMessage{}, err
	}
	return ticket, message, nil
}

// ReplyToTicketFromEmail appends an owner-attributed email message to an
// existing ticket. It mirrors the tenant reply state transition, but replaces
// operator membership with an in-transaction contact-email match and does not
// re-check the support entitlement.
func (s *Store) ReplyToTicketFromEmail(ctx context.Context, in ReplyToTicketFromEmailInput) (TicketMessage, error) {
	in.AccountID = strings.TrimSpace(in.AccountID)
	in.SenderEmail = strings.TrimSpace(in.SenderEmail)
	in.TicketID = strings.TrimSpace(in.TicketID)
	body := strings.TrimSpace(in.Body)
	if in.AccountID == "" {
		return TicketMessage{}, fmt.Errorf("%w: account id required", ErrTicketInputInvalid)
	}
	if in.SenderEmail == "" {
		return TicketMessage{}, fmt.Errorf("%w: sender email required", ErrTicketInputInvalid)
	}
	if in.TicketID == "" {
		return TicketMessage{}, fmt.Errorf("%w: ticket id required", ErrTicketInputInvalid)
	}
	if body == "" {
		return TicketMessage{}, fmt.Errorf("%w: body required", ErrTicketInputInvalid)
	}
	if len(body) > maxSupportBodyBytes {
		return TicketMessage{}, fmt.Errorf("%w: body exceeds %d bytes", ErrTicketInputInvalid, maxSupportBodyBytes)
	}
	emailMessageID := strings.TrimSpace(in.EmailMessageID)
	messageMetadata, err := supportEmailMessageMetadata(emailMessageID)
	if err != nil {
		return TicketMessage{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return TicketMessage{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	authority, err := supportEmailAuthorityTx(ctx, tx, in.AccountID, in.SenderEmail)
	if err != nil {
		return TicketMessage{}, err
	}
	if authority.status != "active" {
		return TicketMessage{}, ErrAccountNotActive
	}
	if !authority.senderMatches {
		return TicketMessage{}, ErrSupportSenderMismatch
	}
	if !authority.ownerID.Valid {
		return TicketMessage{}, errors.New("support email account has no root or account owner")
	}
	var currentState string
	err = tx.QueryRow(ctx,
		`SELECT state
		 FROM support_tickets
		 WHERE account_id = $1 AND id = $2
		 FOR UPDATE`,
		in.AccountID, in.TicketID).Scan(&currentState)
	if errors.Is(err, pgx.ErrNoRows) {
		return TicketMessage{}, ErrTicketNotFound
	}
	if err != nil {
		return TicketMessage{}, fmt.Errorf("lock ticket for email reply: %w", err)
	}
	if currentState == TicketStateClosed {
		return TicketMessage{}, fmt.Errorf("%w: ticket is closed", ErrTicketStateInvalid)
	}
	if existing, ok, err := existingSupportEmailMessageTx(ctx, tx, in.AccountID, emailMessageID); err != nil {
		return TicketMessage{}, err
	} else if ok {
		if err := tx.Commit(ctx); err != nil {
			return TicketMessage{}, err
		}
		return existing, nil
	}

	messageID, err := id.New("tkm")
	if err != nil {
		return TicketMessage{}, err
	}
	var message TicketMessage
	err = tx.QueryRow(ctx,
		`INSERT INTO support_ticket_messages
		   (id, ticket_id, account_id, author_kind, author_id, body, metadata)
		 VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb)
		 RETURNING id, ticket_id, account_id, posted_at, author_kind,
		           COALESCE(author_id, ''), body, attachments, metadata`,
		messageID, in.TicketID, in.AccountID, MessageAuthorOwner,
		authority.ownerID.String, body, messageMetadata,
	).Scan(&message.ID, &message.TicketID, &message.AccountID,
		&message.PostedAt, &message.AuthorKind, &message.AuthorID,
		&message.Body, &message.Attachments, &message.Metadata)
	if err != nil {
		return TicketMessage{}, fmt.Errorf("insert email support reply: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`UPDATE support_tickets
		 SET state = $3, last_activity_at = now(), last_message_id = $4,
		     resolved_at = NULL
		 WHERE account_id = $1 AND id = $2`,
		in.AccountID, in.TicketID, TicketStateAwaitingAdmin, messageID); err != nil {
		return TicketMessage{}, fmt.Errorf("advance ticket state after email reply: %w", err)
	}

	if err := s.logEventTx(ctx, tx, EventInput{
		AccountID: in.AccountID,
		ActorKind: MessageAuthorOwner,
		ActorID:   authority.ownerID.String,
		Verb:      VerbSupportTicketReplied,
		Metadata: map[string]any{
			"ticket_id": in.TicketID,
		},
	}); err != nil {
		return TicketMessage{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return TicketMessage{}, err
	}
	return message, nil
}

type supportEmailAuthority struct {
	status         string
	supportPolicy  string
	planRevision   int64
	planHasSupport bool
	senderMatches  bool
	ownerID        sql.NullString
}

func supportEmailAuthorityTx(ctx context.Context, tx pgx.Tx, accountID, senderEmail string) (supportEmailAuthority, error) {
	var authority supportEmailAuthority
	err := tx.QueryRow(ctx,
		`SELECT a.status, a.support_policy, a.plan_snapshot_revision,
		        a.plan_features ? 'support',
		        a.email IS NOT NULL AND lower(a.email) = lower($2),
		        (SELECT o.id
		         FROM operators o
		         WHERE o.account_id = a.id
		           AND o.deleted_at IS NULL
		           AND (o.is_root OR o.role = 'account_owner')
		         ORDER BY o.is_root DESC, o.created_at, o.id
		         LIMIT 1)
		 FROM accounts a
		 WHERE a.id = $1
		 FOR UPDATE OF a`,
		accountID, senderEmail).Scan(
		&authority.status, &authority.supportPolicy, &authority.planRevision,
		&authority.planHasSupport, &authority.senderMatches, &authority.ownerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return supportEmailAuthority{}, ErrAccountNotFound
	}
	if err != nil {
		return supportEmailAuthority{}, fmt.Errorf("verify support email authority: %w", err)
	}
	return authority, nil
}

func supportEmailMessageMetadata(messageID string) (json.RawMessage, error) {
	metadata := map[string]string{"channel": "email"}
	if messageID = strings.TrimSpace(messageID); messageID != "" {
		metadata["email_message_id"] = messageID
	}
	raw, err := json.Marshal(metadata)
	if err != nil {
		return nil, fmt.Errorf("marshal support email metadata: %w", err)
	}
	return raw, nil
}

// existingSupportEmailMessageTx finds the first message previously accepted
// with messageID for accountID. Callers hold the account row FOR UPDATE before
// this lookup, which serializes concurrent deliveries for the same account.
func existingSupportEmailMessageTx(ctx context.Context, tx pgx.Tx, accountID, messageID string) (TicketMessage, bool, error) {
	if messageID == "" {
		return TicketMessage{}, false, nil
	}
	var message TicketMessage
	err := tx.QueryRow(ctx,
		`SELECT id, ticket_id, account_id, posted_at, author_kind,
		        COALESCE(author_id, ''), body, attachments, metadata
		 FROM support_ticket_messages
		 WHERE account_id = $1
		   AND metadata->>'email_message_id' = $2
		 ORDER BY posted_at, id
		 LIMIT 1`,
		accountID, messageID).Scan(
		&message.ID, &message.TicketID, &message.AccountID,
		&message.PostedAt, &message.AuthorKind, &message.AuthorID,
		&message.Body, &message.Attachments, &message.Metadata)
	if errors.Is(err, pgx.ErrNoRows) {
		return TicketMessage{}, false, nil
	}
	if err != nil {
		return TicketMessage{}, false, fmt.Errorf("find existing support email message: %w", err)
	}
	return message, true, nil
}
