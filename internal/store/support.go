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
	"github.com/witwave-ai/witself/internal/supportstates"
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
// State names alias the canonical values in the supportstates package
// so the store's enforcement, the CLIs' rendering, and any future TUI
// share exactly one source of truth. Changing a state name is a
// single-package operation.
const (
	TicketStateOpen              = supportstates.StateOpen
	TicketStateAwaitingAdmin     = supportstates.StateAwaitingAdmin
	TicketStateAwaitingCustomer  = supportstates.StateAwaitingCustomer
	TicketStateResolved          = supportstates.StateResolved
	TicketStateClosed            = supportstates.StateClosed
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

// legalTransitions is sourced from the supportstates package so the
// CLI's `witwave-admin ticket states` render and the store's
// enforcement can never drift. Graph well-formedness is locked by
// supportstates.TestGraphWellFormed.
var legalTransitions = supportstates.LegalTransitions()

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

// adminHandleRE mirrors the Cloudflare Worker's ADMIN_HANDLE regex. The
// store trusts the CP to have authenticated the caller; the pattern
// check here is just a shape guard so a malformed handle never lands
// in author_id or account_events.metadata.
var adminHandleRE = mustSimpleRE(`^[a-z][a-z0-9_-]{1,31}$`)

// mustSimpleRE compiles a small handle-shape regex; centralised so the
// admin call sites don't each pull "regexp" as a top-level import.
func mustSimpleRE(pat string) *simpleHandleMatcher {
	return &simpleHandleMatcher{pat: pat}
}

// simpleHandleMatcher accepts only the ADMIN_HANDLE shape without a
// regexp dependency: first byte is a-z; length 2..32; every byte is
// a-z, 0-9, '_' or '-'. Keeping the check inline avoids importing
// "regexp" just for one call site.
type simpleHandleMatcher struct{ pat string }

func (m *simpleHandleMatcher) MatchString(s string) bool {
	if len(s) < 2 || len(s) > 32 {
		return false
	}
	first := s[0]
	if first < 'a' || first > 'z' {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		ok := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '-'
		if !ok {
			return false
		}
	}
	return true
}

// ListTicketsAdmin returns every support ticket on the account, newest
// activity first. Unlike the tenant ListTickets, there is no operator
// membership check: the caller has already been authenticated by the
// control plane (provision-token at the server boundary; admin token at
// the CP). Empty slice for an unknown account — the CP distinguishes
// "no such account" via the existing :contact route.
func (s *Store) ListTicketsAdmin(ctx context.Context, accountID string) ([]Ticket, error) {
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
		return nil, fmt.Errorf("list support_tickets (admin): %w", err)
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

// GetTicketAdmin returns one ticket + its full message thread. Same
// contract as the tenant GetTicket but without an operator-membership
// check. ErrTicketNotFound when no such ticket on the account.
func (s *Store) GetTicketAdmin(ctx context.Context, accountID, ticketID string) (Ticket, []TicketMessage, error) {
	var t Ticket
	err := s.pool.QueryRow(ctx,
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
		return Ticket{}, nil, fmt.Errorf("get support_ticket (admin): %w", err)
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
		return Ticket{}, nil, fmt.Errorf("list ticket messages (admin): %w", err)
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

// ReplyAdminInput carries the payload for ReplyAdminTicket. AdminHandle
// must match ADMIN_HANDLE — the store does not re-authenticate but does
// reject malformed handles as ErrTicketInputInvalid (defense in depth
// against a compromised CP-side check).
type ReplyAdminInput struct {
	AccountID   string
	AdminHandle string
	TicketID    string
	Body        string
}

// ReplyAdminTicket appends a message from a fleet admin. Ticket state
// swings to awaiting_customer (the mirror of the tenant reply's
// awaiting_admin); resolved_at clears if the admin is implicitly
// reopening a resolved ticket. Sets first_response_at on the first
// admin reply (the SLA slice will consume this).
func (s *Store) ReplyAdminTicket(ctx context.Context, in ReplyAdminInput) (TicketMessage, error) {
	if !adminHandleRE.MatchString(in.AdminHandle) {
		return TicketMessage{}, fmt.Errorf("%w: invalid admin_handle", ErrTicketInputInvalid)
	}
	body := strings.TrimSpace(in.Body)
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

	// No status gate: an admin can reply on suspended/pending accounts —
	// support continuity beats tenant-write status here. Closed accounts
	// still get replies; the account row itself just needs to exist.
	var accountExists bool
	if err := tx.QueryRow(ctx,
		`SELECT true FROM accounts WHERE id = $1`, in.AccountID,
	).Scan(&accountExists); errors.Is(err, pgx.ErrNoRows) {
		return TicketMessage{}, ErrAccountNotFound
	} else if err != nil {
		return TicketMessage{}, fmt.Errorf("verify admin-reply account: %w", err)
	}

	var currentState string
	err = tx.QueryRow(ctx,
		`SELECT state FROM support_tickets
		 WHERE account_id = $1 AND id = $2
		 FOR UPDATE`,
		in.AccountID, in.TicketID).Scan(&currentState)
	if errors.Is(err, pgx.ErrNoRows) {
		return TicketMessage{}, ErrTicketNotFound
	}
	if err != nil {
		return TicketMessage{}, fmt.Errorf("lock ticket for admin reply: %w", err)
	}
	if currentState == TicketStateClosed {
		return TicketMessage{}, fmt.Errorf("%w: ticket is closed", ErrTicketStateInvalid)
	}

	msgID, err := id.New("tkm")
	if err != nil {
		return TicketMessage{}, err
	}

	var m TicketMessage
	// author_id carries the handle directly per the migration comment
	// ("When author_kind is 'fleet_admin', author_id is the admin's
	// chosen HANDLE"). The store also asserts it in metadata for
	// symmetry with the audit event.
	metaJSON := fmt.Sprintf(`{"admin_handle":%q}`, in.AdminHandle)
	err = tx.QueryRow(ctx,
		`INSERT INTO support_ticket_messages
		   (id, ticket_id, account_id, author_kind, author_id, body, metadata)
		 VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb)
		 RETURNING id, ticket_id, account_id, posted_at, author_kind,
		           COALESCE(author_id, ''), body, attachments, metadata`,
		msgID, in.TicketID, in.AccountID, MessageAuthorFleetAdmin, in.AdminHandle, body, metaJSON,
	).Scan(&m.ID, &m.TicketID, &m.AccountID, &m.PostedAt, &m.AuthorKind,
		&m.AuthorID, &m.Body, &m.Attachments, &m.Metadata)
	if err != nil {
		return TicketMessage{}, fmt.Errorf("insert admin reply message: %w", err)
	}

	// State transition: any non-closed → awaiting_customer. Clear
	// resolved_at (implicit reopen from resolved). Fill
	// first_response_at on the FIRST admin reply — SLA foundation.
	if _, err := tx.Exec(ctx,
		`UPDATE support_tickets
		 SET state = $3, last_activity_at = now(), last_message_id = $4,
		     resolved_at = NULL,
		     first_response_at = COALESCE(first_response_at, now())
		 WHERE account_id = $1 AND id = $2`,
		in.AccountID, in.TicketID, TicketStateAwaitingCustomer, msgID); err != nil {
		return TicketMessage{}, fmt.Errorf("advance ticket state (admin reply): %w", err)
	}

	if err := logEventTx(ctx, tx, EventInput{
		AccountID: in.AccountID,
		ActorKind: ActorControlPlane,
		Verb:      VerbSupportTicketReplied,
		Metadata: map[string]any{
			"ticket_id":    in.TicketID,
			"admin_handle": in.AdminHandle,
		},
	}); err != nil {
		return TicketMessage{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return TicketMessage{}, err
	}
	return m, nil
}

// ChangeAdminStateInput carries the payload for ChangeAdminTicketState.
type ChangeAdminStateInput struct {
	AccountID   string
	AdminHandle string
	TicketID    string
	NewState    string
}

// ChangeAdminTicketState is the admin mirror of ChangeTicketState. Same
// legality map, same resolved_at / closed_at rules; the actor on the
// audit event is control_plane with admin_handle in metadata, and
// there is no operator-membership check.
func (s *Store) ChangeAdminTicketState(ctx context.Context, in ChangeAdminStateInput) (Ticket, error) {
	if !adminHandleRE.MatchString(in.AdminHandle) {
		return Ticket{}, fmt.Errorf("%w: invalid admin_handle", ErrTicketInputInvalid)
	}
	if !isKnownTicketState(in.NewState) {
		return Ticket{}, fmt.Errorf("%w: unknown state %q", ErrTicketStateInvalid, in.NewState)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Ticket{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var accountExists bool
	if err := tx.QueryRow(ctx,
		`SELECT true FROM accounts WHERE id = $1`, in.AccountID,
	).Scan(&accountExists); errors.Is(err, pgx.ErrNoRows) {
		return Ticket{}, ErrAccountNotFound
	} else if err != nil {
		return Ticket{}, fmt.Errorf("verify admin-state account: %w", err)
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
		return Ticket{}, fmt.Errorf("lock ticket for admin state change: %w", err)
	}
	if currentState == in.NewState {
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
		return Ticket{}, fmt.Errorf("update ticket state (admin): %w", err)
	}

	var verb string
	meta := map[string]any{
		"ticket_id":    in.TicketID,
		"admin_handle": in.AdminHandle,
	}
	if in.NewState == TicketStateClosed {
		verb = VerbSupportTicketClosed
	} else {
		verb = VerbSupportTicketStateChanged
		meta["state_from"] = currentState
		meta["state_to"] = in.NewState
	}
	if err := logEventTx(ctx, tx, EventInput{
		AccountID: in.AccountID,
		ActorKind: ActorControlPlane,
		Verb:      verb,
		Metadata:  meta,
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

// ListAdminAllInput filters ListTicketsAdminAll (the cell-wide list
// endpoint the CP calls once per cell during fan-out).
type ListAdminAllInput struct {
	States    []string
	Since     *time.Time
	Limit     int
	PageToken string
}

// ListAdminAllResult carries a page of tickets + the opaque continuation
// token. Empty NextPageToken means the page fully drained the cell.
type ListAdminAllResult struct {
	Tickets       []Ticket
	NextPageToken string
}

// ListTicketsAdminAll returns tickets across every account on this cell,
// newest activity first, cursor-paginated on (last_activity_at, id).
// Filters are optional; unset states means "any state".
func (s *Store) ListTicketsAdminAll(ctx context.Context, in ListAdminAllInput) (ListAdminAllResult, error) {
	limit := in.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	for _, st := range in.States {
		if !isKnownTicketState(st) {
			return ListAdminAllResult{}, fmt.Errorf("%w: unknown state %q", ErrTicketStateInvalid, st)
		}
	}
	cursorTS, cursorID, err := decodeEventCursor(in.PageToken)
	if in.PageToken != "" && err != nil {
		return ListAdminAllResult{}, fmt.Errorf("%w: %v", ErrBadEventCursor, err)
	}

	q := `SELECT id, account_id, opened_at, opened_by_kind, opened_by_id,
	             subject, category, state, priority,
	             first_response_at, resolved_at, closed_at,
	             last_activity_at, COALESCE(last_message_id, ''),
	             correlation, metadata
	      FROM support_tickets
	      WHERE ($1::text[] IS NULL OR state = ANY($1::text[]))
	        AND ($2::timestamptz IS NULL OR last_activity_at >= $2)
	        AND ($3::timestamptz IS NULL
	             OR (last_activity_at, id) < ($3, $4))
	      ORDER BY last_activity_at DESC, id DESC
	      LIMIT $5`

	var cursorArg any
	var cursorIDArg any
	if in.PageToken != "" {
		cursorArg = cursorTS
		cursorIDArg = cursorID
	}
	var statesArg any
	if len(in.States) > 0 {
		statesArg = in.States
	}
	rows, err := s.pool.Query(ctx, q, statesArg, in.Since, cursorArg, cursorIDArg, limit+1)
	if err != nil {
		return ListAdminAllResult{}, fmt.Errorf("list support_tickets (admin all): %w", err)
	}
	defer rows.Close()

	out := make([]Ticket, 0, limit)
	for rows.Next() {
		var t Ticket
		if err := rows.Scan(&t.ID, &t.AccountID, &t.OpenedAt,
			&t.OpenedByKind, &t.OpenedByID, &t.Subject, &t.Category,
			&t.State, &t.Priority, &t.FirstResponseAt, &t.ResolvedAt,
			&t.ClosedAt, &t.LastActivityAt, &t.LastMessageID,
			&t.Correlation, &t.Metadata); err != nil {
			return ListAdminAllResult{}, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return ListAdminAllResult{}, err
	}
	// Cursor pagination reuses the account_events cursor codec (same
	// (timestamp, id) shape); the client sees an opaque token either way.
	var next string
	if len(out) > limit {
		last := out[limit-1]
		next = encodeEventCursor(last.LastActivityAt, last.ID)
		out = out[:limit]
	}
	return ListAdminAllResult{Tickets: out, NextPageToken: next}, nil
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

// Support-policy values. Currently binary; when tiered support arrives
// (basic/priority/enterprise-24h) we add named tiers and keep enabled
// as an alias for the lowest tier that permits opening tickets.
const (
	SupportPolicyEnabled  = "enabled"
	SupportPolicyDisabled = "disabled"
)

var legalSupportPolicies = []string{SupportPolicyEnabled, SupportPolicyDisabled}

// SetSupportPolicyAdminInput carries the payload for
// SetSupportPolicyAdmin. AdminHandle attributes the change; the
// store validates its shape but trusts the control plane's identity
// check.
type SetSupportPolicyAdminInput struct {
	AccountID   string
	AdminHandle string
	NewPolicy   string
}

// SetSupportPolicyAdminResult reports the transition — the previous
// policy value is what the caller needs to render "flipped to
// disabled" style messages without a second round-trip.
type SetSupportPolicyAdminResult struct {
	AccountID  string
	PolicyFrom string
	PolicyTo   string
}

// SetSupportPolicyAdmin flips an account's support_policy on behalf
// of a fleet admin. Idempotent — setting the same value twice is a
// no-op that commits without an audit event (no state change). A
// real transition emits VerbAccountSupportPolicyChanged so the
// tenant's audit ledger records the fleet-side decision.
func (s *Store) SetSupportPolicyAdmin(ctx context.Context, in SetSupportPolicyAdminInput) (SetSupportPolicyAdminResult, error) {
	if !adminHandleRE.MatchString(in.AdminHandle) {
		return SetSupportPolicyAdminResult{}, fmt.Errorf("%w: invalid admin_handle", ErrTicketInputInvalid)
	}
	if !slices.Contains(legalSupportPolicies, in.NewPolicy) {
		return SetSupportPolicyAdminResult{}, fmt.Errorf("%w: unknown support_policy %q (want enabled|disabled)", ErrTicketInputInvalid, in.NewPolicy)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return SetSupportPolicyAdminResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// FOR UPDATE serialises the transition against a concurrent
	// admin flip on the same account. Two admins racing get a clean
	// serial order.
	var currentPolicy string
	err = tx.QueryRow(ctx,
		`SELECT support_policy FROM accounts WHERE id = $1 FOR UPDATE`,
		in.AccountID).Scan(&currentPolicy)
	if errors.Is(err, pgx.ErrNoRows) {
		return SetSupportPolicyAdminResult{}, ErrAccountNotFound
	}
	if err != nil {
		return SetSupportPolicyAdminResult{}, fmt.Errorf("lock account for support-policy change: %w", err)
	}

	if currentPolicy == in.NewPolicy {
		// Idempotent no-op. Commit the (empty) tx so callers can rely
		// on Begin/Commit balance, and skip the audit event so the
		// ledger doesn't accrue no-op noise.
		if err := tx.Commit(ctx); err != nil {
			return SetSupportPolicyAdminResult{}, err
		}
		return SetSupportPolicyAdminResult{
			AccountID:  in.AccountID,
			PolicyFrom: currentPolicy,
			PolicyTo:   in.NewPolicy,
		}, nil
	}

	if _, err := tx.Exec(ctx,
		`UPDATE accounts SET support_policy = $2 WHERE id = $1`,
		in.AccountID, in.NewPolicy); err != nil {
		return SetSupportPolicyAdminResult{}, fmt.Errorf("update support_policy: %w", err)
	}

	if err := logEventTx(ctx, tx, EventInput{
		AccountID: in.AccountID,
		ActorKind: ActorControlPlane,
		Verb:      VerbAccountSupportPolicyChanged,
		Metadata: map[string]any{
			"policy_from":  currentPolicy,
			"policy_to":    in.NewPolicy,
			"admin_handle": in.AdminHandle,
		},
	}); err != nil {
		return SetSupportPolicyAdminResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return SetSupportPolicyAdminResult{}, err
	}
	return SetSupportPolicyAdminResult{
		AccountID:  in.AccountID,
		PolicyFrom: currentPolicy,
		PolicyTo:   in.NewPolicy,
	}, nil
}

// GetSupportPolicyAdmin reads the current support_policy for an
// account. Admin path — no operator-membership check. 404 for an
// unknown account so the CP can render a distinguishable message.
func (s *Store) GetSupportPolicyAdmin(ctx context.Context, accountID string) (string, error) {
	var policy string
	err := s.pool.QueryRow(ctx,
		`SELECT support_policy FROM accounts WHERE id = $1`, accountID).Scan(&policy)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrAccountNotFound
	}
	if err != nil {
		return "", fmt.Errorf("get support_policy (admin): %w", err)
	}
	return policy, nil
}
