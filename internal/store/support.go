package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/witwave-ai/witself/internal/id"
)

// Ticket is the API view of a support_tickets row.
type Ticket struct {
	ID              string          `json:"id"`
	AccountID       string          `json:"account_id"`
	OpenedAt        time.Time       `json:"opened_at"`
	OpenedByKind    string          `json:"opened_by_kind"`
	OpenedByID      string          `json:"opened_by_id"`
	Subject         string          `json:"subject"`
	Category        string          `json:"category"`
	State           string          `json:"state"`
	Priority        string          `json:"priority"`
	FirstResponseAt *time.Time      `json:"first_response_at,omitempty"`
	ResolvedAt      *time.Time      `json:"resolved_at,omitempty"`
	ClosedAt        *time.Time      `json:"closed_at,omitempty"`
	LastActivityAt  time.Time       `json:"last_activity_at"`
	LastMessageID   string          `json:"last_message_id,omitempty"`
	Correlation     json.RawMessage `json:"correlation"`
	Metadata        json.RawMessage `json:"metadata"`
}

// TicketMessage is the API view of one support_ticket_messages row.
type TicketMessage struct {
	ID          string          `json:"id"`
	TicketID    string          `json:"ticket_id"`
	AccountID   string          `json:"account_id"`
	PostedAt    time.Time       `json:"posted_at"`
	AuthorKind  string          `json:"author_kind"`
	AuthorID    string          `json:"author_id,omitempty"`
	Body        string          `json:"body"`
	Attachments json.RawMessage `json:"attachments"`
	Metadata    json.RawMessage `json:"metadata"`
}

// Support-ticket state machine. The set is enforced in Go (not by a
// Postgres CHECK) so we can grow the transitions without a migration.
// Legal transitions:
//   - open → awaiting_admin (first admin sees it, or initial state promoted)
//   - awaiting_admin ↔ awaiting_customer (replies swap the ball)
//   - any of the above → resolved (admin says done)
//   - resolved → closed (customer confirms) OR resolved → awaiting_admin (customer reopens)
//
// The transition legality is enforced in changeTicketStateTx; callers can't
// jump straight from open to closed without going through resolved.
const (
	TicketStateOpen              = "open"
	TicketStateAwaitingAdmin     = "awaiting_admin"
	TicketStateAwaitingCustomer  = "awaiting_customer"
	TicketStateResolved          = "resolved"
	TicketStateClosed            = "closed"
)

// Support-ticket categories. Coarse taxonomy in slice 1; fine-grained tags
// are a later slice.
const (
	TicketCategoryTechnical = "technical"
	TicketCategoryBilling   = "billing"
	TicketCategorySecurity  = "security"
	TicketCategoryOther     = "other"
)

// Support-ticket priorities. The column is present from day one; the
// enterprise SLA slice will enforce first_response_at against these.
const (
	TicketPriorityLow    = "low"
	TicketPriorityNormal = "normal"
	TicketPriorityHigh   = "high"
	TicketPriorityUrgent = "urgent"
)

// Author kinds for support_ticket_messages.author_kind. fleet_admin is
// asserted by the control plane (the tenant's cell does not authenticate
// the admin directly — the provision token gates the admin-side endpoint).
const (
	MessageAuthorOwner      = "owner"
	MessageAuthorOperator   = "operator"
	MessageAuthorFleetAdmin = "fleet_admin"
	MessageAuthorSystem     = "system"
)

// Support-body size cap. 64 KiB is roomy for text, small enough that an
// archive with many tickets stays reasonable. Enforced at the store layer;
// the migration column is TEXT with no Postgres limit.
const maxSupportBodyBytes = 64 * 1024

// Support-subject length cap. Kept modest so list views render cleanly.
const maxSupportSubjectChars = 200

var (
	// ErrSupportDisabled is returned when the account's support_policy
	// forbids opening new tickets. Existing open tickets remain readable.
	ErrSupportDisabled = errors.New("support is not enabled for this account")

	// ErrTicketNotFound is returned when the ticket doesn't exist on
	// this account (or exists on a different tenant, which is
	// indistinguishable — same 404 either way).
	ErrTicketNotFound = errors.New("ticket not found")

	// ErrTicketStateInvalid signals a rejected transition — either the
	// requested target state isn't reachable from the current state or
	// the state itself is unknown.
	ErrTicketStateInvalid = errors.New("invalid ticket state transition")

	// ErrTicketInputInvalid signals a caller-input violation (empty
	// subject, oversized body, unknown category, etc.).
	ErrTicketInputInvalid = errors.New("invalid support ticket input")
)

var legalCategories = []string{
	TicketCategoryTechnical, TicketCategoryBilling,
	TicketCategorySecurity, TicketCategoryOther,
}
var legalPriorities = []string{
	TicketPriorityLow, TicketPriorityNormal,
	TicketPriorityHigh, TicketPriorityUrgent,
}

// legalTransitions maps a current state to the set of legal target
// states. Transitions not listed here return ErrTicketStateInvalid.
var legalTransitions = map[string][]string{
	TicketStateOpen:              {TicketStateAwaitingAdmin, TicketStateAwaitingCustomer, TicketStateResolved, TicketStateClosed},
	TicketStateAwaitingAdmin:     {TicketStateAwaitingCustomer, TicketStateResolved, TicketStateClosed},
	TicketStateAwaitingCustomer:  {TicketStateAwaitingAdmin, TicketStateResolved, TicketStateClosed},
	TicketStateResolved:          {TicketStateAwaitingAdmin, TicketStateClosed},
	TicketStateClosed:            {},
}

// OpenTicketInput is the payload OpenTicket takes. Kept as a struct so
// future optional fields (correlation, initial priority, category, etc.)
// don't cascade signature changes through the server and CLI layers.
type OpenTicketInput struct {
	AccountID  string
	OperatorID string
	Subject    string
	Category   string
	Priority   string
	Body       string
}

// OpenTicket creates a new support ticket with its first message. Refuses
// if the account has support_policy='disabled' or is not active. The
// ticket + first message + support.ticket.opened event all land in one
// transaction so an audit entry never records a ticket that failed to
// commit.
func (s *Store) OpenTicket(ctx context.Context, in OpenTicketInput) (Ticket, TicketMessage, error) {
	subject := strings.TrimSpace(in.Subject)
	body := strings.TrimSpace(in.Body)
	if subject == "" {
		return Ticket{}, TicketMessage{}, fmt.Errorf("%w: subject required", ErrTicketInputInvalid)
	}
	if len(subject) > maxSupportSubjectChars {
		return Ticket{}, TicketMessage{}, fmt.Errorf("%w: subject exceeds %d characters", ErrTicketInputInvalid, maxSupportSubjectChars)
	}
	if body == "" {
		return Ticket{}, TicketMessage{}, fmt.Errorf("%w: body required", ErrTicketInputInvalid)
	}
	if len(body) > maxSupportBodyBytes {
		return Ticket{}, TicketMessage{}, fmt.Errorf("%w: body exceeds %d bytes", ErrTicketInputInvalid, maxSupportBodyBytes)
	}
	category := in.Category
	if category == "" {
		category = TicketCategoryOther
	}
	if !slices.Contains(legalCategories, category) {
		return Ticket{}, TicketMessage{}, fmt.Errorf("%w: unknown category %q", ErrTicketInputInvalid, category)
	}
	priority := in.Priority
	if priority == "" {
		priority = TicketPriorityNormal
	}
	if !slices.Contains(legalPriorities, priority) {
		return Ticket{}, TicketMessage{}, fmt.Errorf("%w: unknown priority %q", ErrTicketInputInvalid, priority)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Ticket{}, TicketMessage{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Verify the operator belongs to the account AND the account status
	// AND the support policy in one query. Support is refused when the
	// account is not active OR the policy is disabled.
	var status, supportPolicy string
	var operatorRole string
	err = tx.QueryRow(ctx,
		`SELECT a.status, a.support_policy, o.role
		 FROM accounts a
		 JOIN operators o ON o.account_id = a.id
		 WHERE a.id = $1 AND o.id = $2 AND o.deleted_at IS NULL
		 FOR UPDATE OF a`,
		in.AccountID, in.OperatorID).Scan(&status, &supportPolicy, &operatorRole)
	if errors.Is(err, pgx.ErrNoRows) {
		return Ticket{}, TicketMessage{}, ErrNotAccountOwner
	}
	if err != nil {
		return Ticket{}, TicketMessage{}, fmt.Errorf("verify open-ticket authority: %w", err)
	}
	if status != "active" {
		return Ticket{}, TicketMessage{}, ErrAccountNotActive
	}
	if supportPolicy != "enabled" {
		return Ticket{}, TicketMessage{}, ErrSupportDisabled
	}

	// Decide the actor_kind for the audit event. Owner if root or
	// account_owner role; plain operator otherwise. The
	// support_tickets.opened_by_kind column stores the same value.
	openedByKind := MessageAuthorOperator
	if operatorRole == "account_owner" {
		openedByKind = MessageAuthorOwner
	}

	ticketID, err := id.New("tkt")
	if err != nil {
		return Ticket{}, TicketMessage{}, err
	}
	msgID, err := id.New("tkm")
	if err != nil {
		return Ticket{}, TicketMessage{}, err
	}

	var t Ticket
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
		ticketID, in.AccountID, openedByKind, in.OperatorID,
		subject, category, TicketStateAwaitingAdmin, priority, msgID,
	).Scan(&t.ID, &t.AccountID, &t.OpenedAt, &t.OpenedByKind, &t.OpenedByID,
		&t.Subject, &t.Category, &t.State, &t.Priority,
		&t.FirstResponseAt, &t.ResolvedAt, &t.ClosedAt,
		&t.LastActivityAt, &t.LastMessageID,
		&t.Correlation, &t.Metadata)
	if err != nil {
		return Ticket{}, TicketMessage{}, fmt.Errorf("insert support_tickets: %w", err)
	}

	// The opening description lands as the first message. author_kind
	// matches the ticket's opened_by_kind.
	var m TicketMessage
	err = tx.QueryRow(ctx,
		`INSERT INTO support_ticket_messages
		   (id, ticket_id, account_id, author_kind, author_id, body)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING id, ticket_id, account_id, posted_at, author_kind,
		           COALESCE(author_id, ''), body, attachments, metadata`,
		msgID, ticketID, in.AccountID, openedByKind, in.OperatorID, body,
	).Scan(&m.ID, &m.TicketID, &m.AccountID, &m.PostedAt, &m.AuthorKind,
		&m.AuthorID, &m.Body, &m.Attachments, &m.Metadata)
	if err != nil {
		return Ticket{}, TicketMessage{}, fmt.Errorf("insert support_ticket_messages: %w", err)
	}

	// Audit trail: the opener's row in account_events. Subject travels
	// on the event so an owner's audit view has meaningful text
	// without pulling the ticket body.
	if err := logEventTx(ctx, tx, EventInput{
		AccountID: in.AccountID,
		ActorKind: openedByKind, ActorID: in.OperatorID,
		Verb: VerbSupportTicketOpened,
		Metadata: map[string]any{
			"ticket_id": ticketID,
			"subject":   subject,
			"category":  category,
		},
	}); err != nil {
		return Ticket{}, TicketMessage{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return Ticket{}, TicketMessage{}, err
	}
	return t, m, nil
}

// ListTickets returns every support ticket on the account, newest activity
// first. Visible to ANY operator on the account (per the visibility rule
// locked with the product decision). Empty slice on no tickets — never nil.
func (s *Store) ListTickets(ctx context.Context, accountID, operatorID string) ([]Ticket, error) {
	// Verify the caller is a member of the account. Any operator role
	// suffices per the "all operators can see tickets" decision.
	var ok bool
	err := s.pool.QueryRow(ctx,
		`SELECT true FROM operators
		 WHERE account_id = $1 AND id = $2 AND deleted_at IS NULL`,
		accountID, operatorID).Scan(&ok)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotAccountOwner
	}
	if err != nil {
		return nil, fmt.Errorf("verify list-tickets authority: %w", err)
	}

	rows, err := s.pool.Query(ctx,
		`SELECT id, account_id, opened_at, opened_by_kind, opened_by_id,
		        subject, category, state, priority,
		        first_response_at, resolved_at, closed_at,
		        last_activity_at, COALESCE(last_message_id, ''),
		        correlation, metadata
		 FROM support_tickets
		 WHERE account_id = $1
		 ORDER BY last_activity_at DESC, id DESC`,
		accountID)
	if err != nil {
		return nil, fmt.Errorf("list support_tickets: %w", err)
	}
	defer rows.Close()

	out := make([]Ticket, 0)
	for rows.Next() {
		var t Ticket
		if err := rows.Scan(&t.ID, &t.AccountID, &t.OpenedAt,
			&t.OpenedByKind, &t.OpenedByID, &t.Subject, &t.Category,
			&t.State, &t.Priority, &t.FirstResponseAt, &t.ResolvedAt,
			&t.ClosedAt, &t.LastActivityAt, &t.LastMessageID,
			&t.Correlation, &t.Metadata); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetTicket returns one ticket plus its full message thread in
// chronological order (oldest first — a thread reads top-down). Any
// operator on the account can read; a non-member gets ErrNotAccountOwner.
func (s *Store) GetTicket(ctx context.Context, accountID, operatorID, ticketID string) (Ticket, []TicketMessage, error) {
	var ok bool
	err := s.pool.QueryRow(ctx,
		`SELECT true FROM operators
		 WHERE account_id = $1 AND id = $2 AND deleted_at IS NULL`,
		accountID, operatorID).Scan(&ok)
	if errors.Is(err, pgx.ErrNoRows) {
		return Ticket{}, nil, ErrNotAccountOwner
	}
	if err != nil {
		return Ticket{}, nil, fmt.Errorf("verify get-ticket authority: %w", err)
	}

	var t Ticket
	err = s.pool.QueryRow(ctx,
		`SELECT id, account_id, opened_at, opened_by_kind, opened_by_id,
		        subject, category, state, priority,
		        first_response_at, resolved_at, closed_at,
		        last_activity_at, COALESCE(last_message_id, ''),
		        correlation, metadata
		 FROM support_tickets
		 WHERE account_id = $1 AND id = $2`,
		accountID, ticketID).Scan(&t.ID, &t.AccountID, &t.OpenedAt,
		&t.OpenedByKind, &t.OpenedByID, &t.Subject, &t.Category,
		&t.State, &t.Priority, &t.FirstResponseAt, &t.ResolvedAt,
		&t.ClosedAt, &t.LastActivityAt, &t.LastMessageID,
		&t.Correlation, &t.Metadata)
	if errors.Is(err, pgx.ErrNoRows) {
		return Ticket{}, nil, ErrTicketNotFound
	}
	if err != nil {
		return Ticket{}, nil, fmt.Errorf("get support_ticket: %w", err)
	}

	rows, err := s.pool.Query(ctx,
		`SELECT id, ticket_id, account_id, posted_at,
		        author_kind, COALESCE(author_id, ''),
		        body, attachments, metadata
		 FROM support_ticket_messages
		 WHERE account_id = $1 AND ticket_id = $2
		 ORDER BY posted_at, id`,
		accountID, ticketID)
	if err != nil {
		return Ticket{}, nil, fmt.Errorf("list ticket messages: %w", err)
	}
	defer rows.Close()

	messages := make([]TicketMessage, 0)
	for rows.Next() {
		var m TicketMessage
		if err := rows.Scan(&m.ID, &m.TicketID, &m.AccountID, &m.PostedAt,
			&m.AuthorKind, &m.AuthorID, &m.Body, &m.Attachments,
			&m.Metadata); err != nil {
			return Ticket{}, nil, err
		}
		messages = append(messages, m)
	}
	if err := rows.Err(); err != nil {
		return Ticket{}, nil, err
	}
	return t, messages, nil
}

// ReplyToTicket appends a message from an operator on the account and
// transitions the ticket back to awaiting_admin (if it wasn't already
// closed). A closed ticket cannot be replied to — reopen first (via
// ChangeTicketState) or open a new one.
func (s *Store) ReplyToTicket(ctx context.Context, accountID, operatorID, ticketID, body string) (TicketMessage, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return TicketMessage{}, fmt.Errorf("%w: body required", ErrTicketInputInvalid)
	}
	if len(body) > maxSupportBodyBytes {
		return TicketMessage{}, fmt.Errorf("%w: body exceeds %d bytes", ErrTicketInputInvalid, maxSupportBodyBytes)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return TicketMessage{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Verify caller is on the account AND the account is active.
	// Support-policy is NOT checked here — an account whose plan was
	// downgraded should still be able to respond to existing threads.
	var status, operatorRole string
	err = tx.QueryRow(ctx,
		`SELECT a.status, o.role
		 FROM accounts a
		 JOIN operators o ON o.account_id = a.id
		 WHERE a.id = $1 AND o.id = $2 AND o.deleted_at IS NULL
		 FOR UPDATE OF a`,
		accountID, operatorID).Scan(&status, &operatorRole)
	if errors.Is(err, pgx.ErrNoRows) {
		return TicketMessage{}, ErrNotAccountOwner
	}
	if err != nil {
		return TicketMessage{}, fmt.Errorf("verify reply-ticket authority: %w", err)
	}
	if status != "active" {
		return TicketMessage{}, ErrAccountNotActive
	}

	authorKind := MessageAuthorOperator
	if operatorRole == "account_owner" {
		authorKind = MessageAuthorOwner
	}

	// Lock the ticket to serialize state transitions across concurrent
	// replies (same shape as CloseAccount's FOR UPDATE OF a).
	var currentState string
	err = tx.QueryRow(ctx,
		`SELECT state FROM support_tickets
		 WHERE account_id = $1 AND id = $2
		 FOR UPDATE`,
		accountID, ticketID).Scan(&currentState)
	if errors.Is(err, pgx.ErrNoRows) {
		return TicketMessage{}, ErrTicketNotFound
	}
	if err != nil {
		return TicketMessage{}, fmt.Errorf("lock ticket for reply: %w", err)
	}
	if currentState == TicketStateClosed {
		return TicketMessage{}, fmt.Errorf("%w: ticket is closed", ErrTicketStateInvalid)
	}

	msgID, err := id.New("tkm")
	if err != nil {
		return TicketMessage{}, err
	}

	var m TicketMessage
	err = tx.QueryRow(ctx,
		`INSERT INTO support_ticket_messages
		   (id, ticket_id, account_id, author_kind, author_id, body)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING id, ticket_id, account_id, posted_at, author_kind,
		           COALESCE(author_id, ''), body, attachments, metadata`,
		msgID, ticketID, accountID, authorKind, operatorID, body,
	).Scan(&m.ID, &m.TicketID, &m.AccountID, &m.PostedAt, &m.AuthorKind,
		&m.AuthorID, &m.Body, &m.Attachments, &m.Metadata)
	if err != nil {
		return TicketMessage{}, fmt.Errorf("insert reply message: %w", err)
	}

	// Ticket state transitions: any non-closed state becomes
	// awaiting_admin when the customer replies. If a resolved ticket
	// gets a customer reply, it's implicitly reopened — resolved_at
	// must clear so a later "true" resolution records the actual
	// time, not the earlier (now-stale) one. Idempotent on tickets
	// that were never resolved (column was already NULL).
	newState := TicketStateAwaitingAdmin
	if _, err := tx.Exec(ctx,
		`UPDATE support_tickets
		 SET state = $3, last_activity_at = now(), last_message_id = $4,
		     resolved_at = NULL
		 WHERE account_id = $1 AND id = $2`,
		accountID, ticketID, newState, msgID); err != nil {
		return TicketMessage{}, fmt.Errorf("advance ticket state: %w", err)
	}

	if err := logEventTx(ctx, tx, EventInput{
		AccountID: accountID,
		ActorKind: authorKind, ActorID: operatorID,
		Verb: VerbSupportTicketReplied,
		Metadata: map[string]any{
			"ticket_id": ticketID,
		},
	}); err != nil {
		return TicketMessage{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return TicketMessage{}, err
	}
	return m, nil
}

// ChangeTicketStateInput carries the metadata for a state transition
// initiated by an operator. Admins have their own path
// (AdminChangeTicketState) that carries the admin handle.
type ChangeTicketStateInput struct {
	AccountID  string
	OperatorID string
	TicketID   string
	NewState   string
}

// ChangeTicketState transitions a ticket into a new legal state (see
// legalTransitions). Any operator on the account may transition; the
// audit event records who did it.
func (s *Store) ChangeTicketState(ctx context.Context, in ChangeTicketStateInput) (Ticket, error) {
	if !isKnownTicketState(in.NewState) {
		return Ticket{}, fmt.Errorf("%w: unknown state %q", ErrTicketStateInvalid, in.NewState)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Ticket{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var status, operatorRole string
	err = tx.QueryRow(ctx,
		`SELECT a.status, o.role
		 FROM accounts a
		 JOIN operators o ON o.account_id = a.id
		 WHERE a.id = $1 AND o.id = $2 AND o.deleted_at IS NULL
		 FOR UPDATE OF a`,
		in.AccountID, in.OperatorID).Scan(&status, &operatorRole)
	if errors.Is(err, pgx.ErrNoRows) {
		return Ticket{}, ErrNotAccountOwner
	}
	if err != nil {
		return Ticket{}, fmt.Errorf("verify state-change authority: %w", err)
	}
	if status != "active" {
		return Ticket{}, ErrAccountNotActive
	}

	actorKind := MessageAuthorOperator
	if operatorRole == "account_owner" {
		actorKind = MessageAuthorOwner
	}

	var currentState string
	err = tx.QueryRow(ctx,
		`SELECT state FROM support_tickets
		 WHERE account_id = $1 AND id = $2
		 FOR UPDATE`,
		in.AccountID, in.TicketID).Scan(&currentState)
	if errors.Is(err, pgx.ErrNoRows) {
		return Ticket{}, ErrTicketNotFound
	}
	if err != nil {
		return Ticket{}, fmt.Errorf("lock ticket for state change: %w", err)
	}
	if currentState == in.NewState {
		// No-op transition — still return the current row without an
		// event write. This keeps a CLI "close it again" idempotent.
		t, err := readTicketRow(ctx, tx, in.AccountID, in.TicketID)
		if err != nil {
			return Ticket{}, err
		}
		return t, tx.Commit(ctx)
	}
	allowed, ok := legalTransitions[currentState]
	if !ok || !slices.Contains(allowed, in.NewState) {
		return Ticket{}, fmt.Errorf("%w: %s → %s", ErrTicketStateInvalid, currentState, in.NewState)
	}

	// Update the state + timestamps. resolved_at fills on entry to
	// 'resolved' AND clears on the reverse edge (resolved → not-resolved
	// & not-closed) so the SLA slice records the *true* later resolution
	// time. closed_at fills on entry to 'closed' and is otherwise left
	// alone (no legal reopen path from closed).
	setResolvedAt := "resolved_at"
	switch in.NewState {
	case TicketStateResolved:
		setResolvedAt = "COALESCE(resolved_at, now())"
	case TicketStateAwaitingAdmin, TicketStateAwaitingCustomer, TicketStateOpen:
		setResolvedAt = "NULL"
	}
	setClosedAt := "closed_at"
	if in.NewState == TicketStateClosed {
		setClosedAt = "COALESCE(closed_at, now())"
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(
		`UPDATE support_tickets
		 SET state = $3, last_activity_at = now(),
		     resolved_at = %s, closed_at = %s
		 WHERE account_id = $1 AND id = $2`,
		setResolvedAt, setClosedAt,
	), in.AccountID, in.TicketID, in.NewState); err != nil {
		return Ticket{}, fmt.Errorf("update ticket state: %w", err)
	}

	// Emit either the specific "closed" verb (final state) or the
	// general "state_changed" verb (intermediate transitions). This
	// matches the audit narrative — a closure is a distinctive
	// milestone; other transitions are just workflow.
	var verb string
	meta := map[string]any{"ticket_id": in.TicketID}
	if in.NewState == TicketStateClosed {
		verb = VerbSupportTicketClosed
	} else {
		verb = VerbSupportTicketStateChanged
		meta["state_from"] = currentState
		meta["state_to"] = in.NewState
	}
	if err := logEventTx(ctx, tx, EventInput{
		AccountID: in.AccountID,
		ActorKind: actorKind, ActorID: in.OperatorID,
		Verb: verb, Metadata: meta,
	}); err != nil {
		return Ticket{}, err
	}

	t, err := readTicketRow(ctx, tx, in.AccountID, in.TicketID)
	if err != nil {
		return Ticket{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Ticket{}, err
	}
	return t, nil
}

func isKnownTicketState(s string) bool {
	switch s {
	case TicketStateOpen, TicketStateAwaitingAdmin, TicketStateAwaitingCustomer,
		TicketStateResolved, TicketStateClosed:
		return true
	}
	return false
}

// readTicketRow re-reads a ticket after a mutation to return the current
// row (including the post-update timestamps and state). Called with an
// active tx so the read sees the mutation's own writes.
func readTicketRow(ctx context.Context, tx pgx.Tx, accountID, ticketID string) (Ticket, error) {
	var t Ticket
	err := tx.QueryRow(ctx,
		`SELECT id, account_id, opened_at, opened_by_kind, opened_by_id,
		        subject, category, state, priority,
		        first_response_at, resolved_at, closed_at,
		        last_activity_at, COALESCE(last_message_id, ''),
		        correlation, metadata
		 FROM support_tickets
		 WHERE account_id = $1 AND id = $2`,
		accountID, ticketID).Scan(&t.ID, &t.AccountID, &t.OpenedAt,
		&t.OpenedByKind, &t.OpenedByID, &t.Subject, &t.Category,
		&t.State, &t.Priority, &t.FirstResponseAt, &t.ResolvedAt,
		&t.ClosedAt, &t.LastActivityAt, &t.LastMessageID,
		&t.Correlation, &t.Metadata)
	if errors.Is(err, pgx.ErrNoRows) {
		return Ticket{}, ErrTicketNotFound
	}
	return t, err
}
