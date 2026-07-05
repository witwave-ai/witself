package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/witwave-ai/witself/internal/id"
)

// Event is the API view of an account_events row. Verb is namespaced
// ("email.changed", "recovery.completed") so downstream consumers can
// filter by category. Metadata carries per-verb context (masked emails,
// operator ids, etc.) — see the verb registry below for what each verb
// promises to carry.
type Event struct {
	ID         string          `json:"id"`
	AccountID  string          `json:"account_id"`
	OccurredAt time.Time       `json:"occurred_at"`
	ActorKind  string          `json:"actor_kind"`
	ActorID    string          `json:"actor_id,omitempty"`
	Verb       string          `json:"verb"`
	Metadata   json.RawMessage `json:"metadata"`
}

// ActorKind names the source of an event. `owner` and `operator` are
// principal-authenticated cell-side actions; `agent` is a token-carrying
// agent action; `system` covers cell-internal state transitions like
// evacuation/restore; `control_plane` covers Worker-forwarded events like
// signup activation and recovery.
const (
	ActorOwner        = "owner"
	ActorOperator     = "operator"
	ActorAgent        = "agent"
	ActorSystem       = "system"
	ActorControlPlane = "control_plane"
)

// Verbs — the finite namespace of things worth remembering. Adding a new
// verb here is a policy decision (do owners need to see it?) and must
// come with a matching entry in verbMetadataSchema below so unfamiliar
// metadata shapes don't slip in silently.
const (
	// Owner-initiated on the cell.
	VerbOperatorCreated      = "operator.created"
	VerbOperatorDeleted      = "operator.deleted"
	VerbOperatorTokenMinted  = "operator.token.minted"
	VerbAgentTokenMinted     = "agent.token.minted"
	VerbTokenRevoked         = "token.revoked"
	VerbAccountRenamed       = "account.renamed"
	VerbAccountEmailChanged  = "account.email.changed"
	VerbAccountEmailUndone   = "account.email.undone"
	VerbAccountSuspendedByMe = "account.suspended.owner"
	VerbAccountResumedByMe   = "account.resumed.owner"

	// Control-plane forwarded (Worker calls the cell's :events endpoint
	// with these).
	VerbAccountProvisioned        = "account.provisioned"
	VerbAccountActivated          = "account.activated"
	VerbRecoveryRequested         = "recovery.requested"
	VerbRecoveryCompleted         = "recovery.completed"
	VerbAccountEmailChangeStarted = "account.email.change.initiated"
	VerbAccountEmailVerifySent    = "account.email.verify.sent"
	VerbAccountEmailRecoverySent  = "account.email.recovery.sent"
	VerbAccountEmailChangeSent    = "account.email.change.sent"
	VerbAccountEmailUndoSent      = "account.email.undo.sent"

	// System state transitions.
	VerbAccountSuspendedBySystem = "account.suspended.system"
	VerbAccountResumedBySystem   = "account.resumed.system"
	VerbAccountEvacuated         = "account.evacuated"
	VerbAccountRestored          = "account.restored"
	VerbAccountReaped            = "account.reaped"
	VerbAccountClosed            = "account.closed"

	// Support-ticket lifecycle. Every ticket mutation lands both a
	// support_tickets state change AND an account_events row so the
	// owner's audit ledger surfaces "you filed a ticket / support
	// replied / ticket closed" without the owner having to know about
	// two separate views. The metadata carries the ticket id + a short
	// human summary; the full body lives on support_ticket_messages.
	VerbSupportTicketOpened       = "support.ticket.opened"
	VerbSupportTicketReplied      = "support.ticket.replied"
	VerbSupportTicketStateChanged = "support.ticket.state_changed"
	VerbSupportTicketClosed       = "support.ticket.closed"

	// Fleet-admin flipped the account's support_policy on or off.
	// policy_from / policy_to carry the transition; admin_handle
	// attributes the actor to a specific admin (audit-trail
	// requirement). Only control_plane may emit this — a compromised
	// operator token cannot forge a plan-tier change.
	VerbAccountSupportPolicyChanged = "account.support_policy_changed"
)

// ErrUnknownVerb is returned when a caller tries to log a verb not in the
// registry. Every event must go through a registered verb so the metadata
// contract is enforced at write time — no drift-then-diagnose surprises.
var ErrUnknownVerb = errors.New("unknown audit-log verb")

// ErrBadEventMetadata is returned when the metadata for a verb doesn't
// match its declared shape (missing required keys, wrong type, extra
// keys). The caller has to fix the caller, not swallow the error.
var ErrBadEventMetadata = errors.New("event metadata does not match verb schema")

// ErrBadEventCursor is returned by ListAccountEvents when the caller
// passes a malformed pagination cursor. Distinct from "server fault"
// so the server layer can map it to 400 instead of 500.
var ErrBadEventCursor = errors.New("malformed pagination cursor")

// verbSpec pins one verb's expected metadata shape. RequiredKeys must be
// present and hold non-null strings; AllowedKeys is the full set of
// permitted keys (superset of required). Extra keys are refused so a
// future consumer can trust the shape.
type verbSpec struct {
	requiredKeys []string
	allowedKeys  []string
	// allowedActors is the set of ActorKind values legitimate for this
	// verb. `recovery.requested` can only come from control_plane;
	// `operator.created` can only come from owner/operator. Constraining
	// this at write time catches bugs where an event fires from the
	// wrong path.
	allowedActors []string
}

// verbMetadataSchema is the write-time contract for every known verb. Add
// a verb constant above, add its entry here, or the write refuses.
var verbMetadataSchema = map[string]verbSpec{
	// Owner-initiated.
	VerbOperatorCreated: {
		requiredKeys:  []string{"operator_id", "role"},
		allowedKeys:   []string{"operator_id", "role", "display_name"},
		allowedActors: []string{ActorOwner, ActorOperator},
	},
	VerbOperatorDeleted: {
		requiredKeys:  []string{"operator_id"},
		allowedKeys:   []string{"operator_id", "role"},
		allowedActors: []string{ActorOwner, ActorOperator},
	},
	VerbOperatorTokenMinted: {
		requiredKeys:  []string{"token_id", "operator_id"},
		allowedKeys:   []string{"token_id", "operator_id", "display_name", "expires_at"},
		allowedActors: []string{ActorOwner, ActorOperator},
	},
	VerbAgentTokenMinted: {
		requiredKeys:  []string{"token_id", "agent_id"},
		allowedKeys:   []string{"token_id", "agent_id", "agent_name"},
		allowedActors: []string{ActorOwner, ActorOperator},
	},
	VerbTokenRevoked: {
		requiredKeys:  []string{"token_id"},
		allowedKeys:   []string{"token_id", "operator_id", "agent_id", "kind"},
		allowedActors: []string{ActorOwner, ActorOperator},
	},
	VerbAccountRenamed: {
		requiredKeys:  []string{"display_name"},
		allowedKeys:   []string{"display_name"},
		allowedActors: []string{ActorOwner, ActorOperator},
	},
	VerbAccountEmailChanged: {
		requiredKeys:  []string{"old_masked", "new_masked"},
		allowedKeys:   []string{"old_masked", "new_masked"},
		allowedActors: []string{ActorOwner, ActorOperator, ActorControlPlane},
	},
	VerbAccountEmailUndone: {
		requiredKeys:  []string{"restored_masked"},
		allowedKeys:   []string{"restored_masked"},
		allowedActors: []string{ActorControlPlane},
	},
	VerbAccountSuspendedByMe: {
		allowedKeys:   []string{"reason"},
		allowedActors: []string{ActorOwner},
	},
	VerbAccountResumedByMe: {
		allowedKeys:   []string{},
		allowedActors: []string{ActorOwner},
	},

	// Control-plane forwarded.
	VerbAccountProvisioned: {
		requiredKeys:  []string{"email_masked"},
		allowedKeys:   []string{"email_masked", "operator_id"},
		allowedActors: []string{ActorControlPlane},
	},
	VerbAccountActivated: {
		allowedKeys:   []string{},
		allowedActors: []string{ActorControlPlane},
	},
	VerbRecoveryRequested: {
		requiredKeys:  []string{"email_masked"},
		allowedKeys:   []string{"email_masked"},
		allowedActors: []string{ActorControlPlane},
	},
	VerbRecoveryCompleted: {
		requiredKeys:  []string{"new_operator_id"},
		allowedKeys:   []string{"new_operator_id"},
		allowedActors: []string{ActorControlPlane},
	},
	VerbAccountEmailChangeStarted: {
		requiredKeys:  []string{"new_masked"},
		allowedKeys:   []string{"new_masked"},
		allowedActors: []string{ActorOwner, ActorOperator, ActorControlPlane},
	},
	VerbAccountEmailVerifySent: {
		requiredKeys:  []string{"to_masked"},
		allowedKeys:   []string{"to_masked"},
		allowedActors: []string{ActorControlPlane},
	},
	VerbAccountEmailRecoverySent: {
		requiredKeys:  []string{"to_masked"},
		allowedKeys:   []string{"to_masked"},
		allowedActors: []string{ActorControlPlane},
	},
	VerbAccountEmailChangeSent: {
		requiredKeys:  []string{"to_masked"},
		allowedKeys:   []string{"to_masked"},
		allowedActors: []string{ActorControlPlane},
	},
	VerbAccountEmailUndoSent: {
		requiredKeys:  []string{"to_masked"},
		allowedKeys:   []string{"to_masked"},
		allowedActors: []string{ActorControlPlane},
	},

	// System.
	VerbAccountSuspendedBySystem: {
		requiredKeys:  []string{"category"},
		allowedKeys:   []string{"category", "reason"},
		allowedActors: []string{ActorSystem, ActorControlPlane},
	},
	VerbAccountResumedBySystem: {
		requiredKeys:  []string{"category"},
		allowedKeys:   []string{"category"},
		allowedActors: []string{ActorSystem, ActorControlPlane},
	},
	VerbAccountEvacuated: {
		allowedKeys:   []string{"cell"},
		allowedActors: []string{ActorSystem},
	},
	VerbAccountRestored: {
		allowedKeys:   []string{"from_cell"},
		allowedActors: []string{ActorSystem},
	},
	VerbAccountReaped: {
		allowedKeys:   []string{"reason"},
		allowedActors: []string{ActorSystem, ActorControlPlane},
	},
	VerbAccountClosed: {
		allowedKeys:   []string{"reason"},
		allowedActors: []string{ActorOwner, ActorOperator, ActorSystem},
	},

	// Support-ticket verbs. ticket_id is required on all four so the
	// owner can correlate an audit entry back to the ticket. subject
	// is a short human string carried on opened/closed so the audit
	// view has meaningful text without pulling the ticket. state_from
	// / state_to on state_changed give a compact transition record.
	// admin_handle appears on replies/closes originated by fleet admins
	// (matches the tenant-visible handle shape).
	VerbSupportTicketOpened: {
		requiredKeys:  []string{"ticket_id", "subject"},
		allowedKeys:   []string{"ticket_id", "subject", "category"},
		allowedActors: []string{ActorOwner, ActorOperator},
	},
	VerbSupportTicketReplied: {
		requiredKeys:  []string{"ticket_id"},
		allowedKeys:   []string{"ticket_id", "admin_handle"},
		allowedActors: []string{ActorOwner, ActorOperator, ActorControlPlane},
	},
	VerbSupportTicketStateChanged: {
		requiredKeys:  []string{"ticket_id", "state_from", "state_to"},
		allowedKeys:   []string{"ticket_id", "state_from", "state_to", "admin_handle"},
		allowedActors: []string{ActorOwner, ActorOperator, ActorControlPlane},
	},
	VerbSupportTicketClosed: {
		requiredKeys:  []string{"ticket_id"},
		allowedKeys:   []string{"ticket_id", "subject", "admin_handle"},
		allowedActors: []string{ActorOwner, ActorOperator, ActorControlPlane},
	},
	VerbAccountSupportPolicyChanged: {
		requiredKeys:  []string{"policy_from", "policy_to", "admin_handle"},
		allowedKeys:   []string{"policy_from", "policy_to", "admin_handle"},
		allowedActors: []string{ActorControlPlane},
	},
}

// EventInput is one row to write. The store fills in the id and
// occurred_at; the caller provides everything else. Zero values for
// Metadata are OK (an empty {}).
type EventInput struct {
	AccountID string
	ActorKind string
	ActorID   string // may be empty for system/control_plane
	Verb      string
	Metadata  map[string]any
}

// checkEventShape enforces the verb → metadata contract before any SQL
// runs. Extracted from logEventTx so it can be unit-tested without a
// live database. Every rule here is the same rule the write helper
// applies; keeping them in one place means a passing test guarantees
// the write path behaves the same way.
func checkEventShape(in EventInput) error {
	spec, known := verbMetadataSchema[in.Verb]
	if !known {
		return fmt.Errorf("%w: %s", ErrUnknownVerb, in.Verb)
	}
	if in.AccountID == "" {
		return fmt.Errorf("%w: account_id is required", ErrBadEventMetadata)
	}
	if !slices.Contains(spec.allowedActors, in.ActorKind) {
		return fmt.Errorf("%w: actor_kind %q not allowed for verb %s (want one of %v)",
			ErrBadEventMetadata, in.ActorKind, in.Verb, spec.allowedActors)
	}
	// A principal actor MUST carry its id; a system/control_plane actor
	// must NOT (there's no principal id to record — the presence of the
	// verb and the actor_kind is the record).
	principalActor := in.ActorKind == ActorOwner || in.ActorKind == ActorOperator || in.ActorKind == ActorAgent
	if principalActor && in.ActorID == "" {
		return fmt.Errorf("%w: verb %s with actor_kind %q requires actor_id",
			ErrBadEventMetadata, in.Verb, in.ActorKind)
	}
	if !principalActor && in.ActorID != "" {
		return fmt.Errorf("%w: verb %s with actor_kind %q must not carry actor_id",
			ErrBadEventMetadata, in.Verb, in.ActorKind)
	}
	meta := in.Metadata
	// Every allowed key that is required must be present and non-empty.
	for _, k := range spec.requiredKeys {
		v, ok := meta[k]
		if !ok {
			return fmt.Errorf("%w: verb %s requires metadata key %q", ErrBadEventMetadata, in.Verb, k)
		}
		s, isStr := v.(string)
		if !isStr || strings.TrimSpace(s) == "" {
			return fmt.Errorf("%w: verb %s metadata key %q must be a non-empty string", ErrBadEventMetadata, in.Verb, k)
		}
	}
	// No key in metadata may be outside the allowlist — a stray key would
	// otherwise silently accumulate into the archive without any
	// documented meaning.
	for k := range meta {
		if !slices.Contains(spec.allowedKeys, k) {
			return fmt.Errorf("%w: verb %s metadata carries unknown key %q", ErrBadEventMetadata, in.Verb, k)
		}
	}
	return nil
}

// LogEvent writes one event row in its own transaction. Used from the
// server layer for events whose cause is not a Store mutation — Worker-
// forwarded control-plane events, out-of-band system events. The
// mutation-inline path (logEventTx) is preferred when the caller
// already holds a transaction.
func (s *Store) LogEvent(ctx context.Context, in EventInput) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := logEventTx(ctx, tx, in); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// logEventTx writes one event row inside the caller's transaction. Used
// by every mutation that wants an audit entry alongside its state change
// so an event never lands without its cause. Callers wrap their own
// transaction; if the mutation aborts, the event aborts with it.
func logEventTx(ctx context.Context, tx pgx.Tx, in EventInput) error {
	if err := checkEventShape(in); err != nil {
		return err
	}
	meta := in.Metadata
	if meta == nil {
		meta = map[string]any{}
	}
	eventID, err := id.New("evt")
	if err != nil {
		return fmt.Errorf("mint event id: %w", err)
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marshal event metadata: %w", err)
	}

	var actorID any
	if in.ActorID != "" {
		actorID = in.ActorID
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO account_events (id, account_id, actor_kind, actor_id, verb, metadata)
		 VALUES ($1, $2, $3, $4, $5, $6::jsonb)`,
		eventID, in.AccountID, in.ActorKind, actorID, in.Verb, metaJSON); err != nil {
		// FK violation on account_id means the account doesn't exist.
		// Distinguish from a generic DB error so the server layer can
		// return 404 instead of 500 — critical for out-of-tx callers
		// (Store.LogEvent from the :events endpoint) where "unknown
		// account" is a permanent client error the Worker mustn't
		// retry against.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" && strings.Contains(pgErr.ConstraintName, "account_events_account_id") {
			return ErrAccountNotFound
		}
		return fmt.Errorf("insert account_events: %w", err)
	}
	return nil
}

// MaskEmail turns "scott@witwave.ai" into "s***@w***.ai" — the shape used
// throughout the event registry. Empty input returns empty.
func MaskEmail(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return ""
	}
	at := strings.LastIndexByte(addr, '@')
	if at <= 0 || at == len(addr)-1 {
		// Not obviously an email; return a fully-masked placeholder so a
		// malformed value never leaks unmasked into the ledger.
		return "***"
	}
	local, domain := addr[:at], addr[at+1:]
	dot := strings.LastIndexByte(domain, '.')
	var domainMasked string
	if dot > 0 {
		domainMasked = string(domain[0]) + "***" + domain[dot:]
	} else {
		domainMasked = string(domain[0]) + "***"
	}
	localMasked := string(local[0]) + "***"
	return localMasked + "@" + domainMasked
}

// EventPage is the return type of ListAccountEvents: a page of events plus
// an opaque cursor for the next page. NextCursor is empty on the last page.
type EventPage struct {
	Events     []Event
	NextCursor string
}

// EventFilter constrains ListAccountEvents. All fields are optional. Cursor
// is opaque — pass the previous page's NextCursor to continue. Since/Until
// let the caller bound to a time window.
type EventFilter struct {
	Since  *time.Time
	Until  *time.Time
	Verb   string // exact match; empty = any
	Limit  int    // default 50, max 500
	Cursor string
}

// ListAccountEvents returns a page of events for one account, newest
// first, with an opaque cursor for pagination. Owner-only — the audit
// trail is a trust-boundary artifact, not a general operator view. A
// non-owner operator gets ErrNotAccountOwner even if they can otherwise
// read the account.
func (s *Store) ListAccountEvents(ctx context.Context, accountID, operatorID string, filter EventFilter) (EventPage, error) {
	// Owner check: same shape as suspend/rename — the operator must
	// belong to the account AND be root-or-owner.
	var isOwner bool
	err := s.pool.QueryRow(ctx,
		`SELECT (o.is_root OR o.role = 'account_owner')
		 FROM operators o
		 WHERE o.id = $1 AND o.account_id = $2 AND o.deleted_at IS NULL`,
		operatorID, accountID).Scan(&isOwner)
	if errors.Is(err, pgx.ErrNoRows) {
		return EventPage{}, ErrNotAccountOwner
	}
	if err != nil {
		return EventPage{}, fmt.Errorf("verify event-list authority: %w", err)
	}
	if !isOwner {
		return EventPage{}, ErrNotAccountOwner
	}

	if filter.Limit <= 0 {
		filter.Limit = 50
	}
	if filter.Limit > 500 {
		filter.Limit = 500
	}

	// Cursor decodes to a strict upper bound on (occurred_at, id) so the
	// next page picks up right where the previous left off, even for
	// same-timestamp events (id break-ties DESC alongside timestamp).
	var (
		afterTime *time.Time
		afterID   string
	)
	if filter.Cursor != "" {
		t, i, err := decodeEventCursor(filter.Cursor)
		if err != nil {
			return EventPage{}, fmt.Errorf("%w: %v", ErrBadEventCursor, err)
		}
		afterTime = &t
		afterID = i
	}

	args := []any{accountID}
	q := &strings.Builder{}
	q.WriteString(`SELECT id, account_id, occurred_at, actor_kind, actor_id, verb, metadata
		FROM account_events
		WHERE account_id = $1`)

	if filter.Since != nil {
		args = append(args, *filter.Since)
		fmt.Fprintf(q, " AND occurred_at >= $%d", len(args))
	}
	if filter.Until != nil {
		args = append(args, *filter.Until)
		fmt.Fprintf(q, " AND occurred_at <= $%d", len(args))
	}
	if filter.Verb != "" {
		args = append(args, filter.Verb)
		fmt.Fprintf(q, " AND verb = $%d", len(args))
	}
	if afterTime != nil {
		args = append(args, *afterTime, afterID)
		fmt.Fprintf(q, " AND (occurred_at, id) < ($%d, $%d)", len(args)-1, len(args))
	}
	// Fetch one more than the limit so we know whether to emit a cursor.
	args = append(args, filter.Limit+1)
	fmt.Fprintf(q, " ORDER BY occurred_at DESC, id DESC LIMIT $%d", len(args))

	rows, err := s.pool.Query(ctx, q.String(), args...)
	if err != nil {
		return EventPage{}, fmt.Errorf("query account_events: %w", err)
	}
	defer rows.Close()

	events := make([]Event, 0, filter.Limit)
	for rows.Next() {
		var e Event
		var actorID *string
		var meta []byte
		if err := rows.Scan(&e.ID, &e.AccountID, &e.OccurredAt, &e.ActorKind, &actorID, &e.Verb, &meta); err != nil {
			return EventPage{}, fmt.Errorf("scan account_events: %w", err)
		}
		if actorID != nil {
			e.ActorID = *actorID
		}
		e.Metadata = json.RawMessage(meta)
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return EventPage{}, err
	}

	// If we fetched more than the caller asked for, the last row is the
	// cursor and we drop it from the page.
	var next string
	if len(events) > filter.Limit {
		cursorRow := events[filter.Limit-1]
		next = encodeEventCursor(cursorRow.OccurredAt, cursorRow.ID)
		events = events[:filter.Limit]
	}

	return EventPage{Events: events, NextCursor: next}, nil
}

// ListEventsAdminAll returns a page of events across EVERY account on
// this cell, newest first — the fleet-admin tail behind `witself-admin
// events list|watch` and the dashboard's events pane. Provision-token
// authorized at the server boundary (the CP authenticated the admin);
// no per-account owner gate applies, which is exactly the point:
// this is the operator's fleet-wide view. Uses the account_events_by_
// time index from migration 0016. Same filter semantics + opaque
// cursor codec as the owner view.
func (s *Store) ListEventsAdminAll(ctx context.Context, filter EventFilter) (EventPage, error) {
	if filter.Limit <= 0 {
		filter.Limit = 50
	}
	if filter.Limit > 500 {
		filter.Limit = 500
	}
	var (
		afterTime *time.Time
		afterID   string
	)
	if filter.Cursor != "" {
		t, i, err := decodeEventCursor(filter.Cursor)
		if err != nil {
			return EventPage{}, fmt.Errorf("%w: %v", ErrBadEventCursor, err)
		}
		afterTime = &t
		afterID = i
	}

	args := []any{}
	q := &strings.Builder{}
	q.WriteString(`SELECT id, account_id, occurred_at, actor_kind, actor_id, verb, metadata
		FROM account_events
		WHERE true`)
	if filter.Since != nil {
		args = append(args, *filter.Since)
		fmt.Fprintf(q, " AND occurred_at >= $%d", len(args))
	}
	if filter.Until != nil {
		args = append(args, *filter.Until)
		fmt.Fprintf(q, " AND occurred_at <= $%d", len(args))
	}
	if filter.Verb != "" {
		args = append(args, filter.Verb)
		fmt.Fprintf(q, " AND verb = $%d", len(args))
	}
	if afterTime != nil {
		args = append(args, *afterTime, afterID)
		fmt.Fprintf(q, " AND (occurred_at, id) < ($%d, $%d)", len(args)-1, len(args))
	}
	args = append(args, filter.Limit+1)
	fmt.Fprintf(q, " ORDER BY occurred_at DESC, id DESC LIMIT $%d", len(args))

	rows, err := s.pool.Query(ctx, q.String(), args...)
	if err != nil {
		return EventPage{}, fmt.Errorf("query account_events (admin all): %w", err)
	}
	defer rows.Close()

	events := make([]Event, 0, filter.Limit)
	for rows.Next() {
		var e Event
		var actorID *string
		var meta []byte
		if err := rows.Scan(&e.ID, &e.AccountID, &e.OccurredAt, &e.ActorKind, &actorID, &e.Verb, &meta); err != nil {
			return EventPage{}, fmt.Errorf("scan account_events (admin all): %w", err)
		}
		if actorID != nil {
			e.ActorID = *actorID
		}
		e.Metadata = json.RawMessage(meta)
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return EventPage{}, err
	}
	var next string
	if len(events) > filter.Limit {
		cursorRow := events[filter.Limit-1]
		next = encodeEventCursor(cursorRow.OccurredAt, cursorRow.ID)
		events = events[:filter.Limit]
	}
	return EventPage{Events: events, NextCursor: next}, nil
}

func encodeEventCursor(t time.Time, id string) string {
	return fmt.Sprintf("%d:%s", t.UnixNano(), id)
}

func decodeEventCursor(cursor string) (time.Time, string, error) {
	i := strings.IndexByte(cursor, ':')
	if i <= 0 || i == len(cursor)-1 {
		return time.Time{}, "", fmt.Errorf("malformed cursor")
	}
	// Reject anything that isn't a pure positive integer for the
	// timestamp part. strconv.ParseInt would accept a leading '+' or
	// '-' — cursors are opaque and our timestamps are always positive
	// Unix nanoseconds — and fmt.Sscanf used to silently swallow
	// trailing garbage. Enforce the strictest legal shape.
	tsPart := cursor[:i]
	for _, r := range tsPart {
		if r < '0' || r > '9' {
			return time.Time{}, "", fmt.Errorf("malformed cursor timestamp")
		}
	}
	ns, err := strconv.ParseInt(tsPart, 10, 64)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("malformed cursor timestamp: %w", err)
	}
	return time.Unix(0, ns).UTC(), cursor[i+1:], nil
}
