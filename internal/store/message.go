package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/witwave-ai/witself/internal/id"
)

// Message direction, recipient, delivery, and read-state values form the
// finite storage/API vocabulary for the first direct-messaging slice.
const (
	MessageDirectionInbox  = "inbox"
	MessageDirectionOutbox = "outbox"

	MessageRecipientAgent = "agent"

	MessageDeliveryQueued    = "queued"
	MessageDeliveryDelivered = "delivered"
	MessageDeliveryFailed    = "failed"

	MessageReadUnread = "unread"
	MessageReadRead   = "read"
	MessageReadAcked  = "acked"

	maxMessageSubjectBytes        = 256
	maxMessageKindBytes           = 64
	maxMessageBodyBytes           = 64 * 1024
	maxMessagePayloadBytes        = 16 * 1024
	maxMessageThreadIDBytes       = 128
	maxMessageIdempotencyKeyBytes = 512
	maxMessageRecipientBytes      = 256
	defaultMessagePageSize        = 50
	maxMessagePageSize            = 100
)

var (
	// ErrMessageInputInvalid reports caller-correctable message input.
	ErrMessageInputInvalid = errors.New("invalid message input")
	// ErrMessageNotFound reports a message outside the recipient mailbox.
	ErrMessageNotFound = errors.New("message not found")
	// ErrMessageRecipientMissing reports a recipient absent from the realm.
	ErrMessageRecipientMissing = errors.New("message recipient not found")
	// ErrMessageForbidden reports use by a principal that is not an agent.
	ErrMessageForbidden = errors.New("message access forbidden")
	// ErrMessageConflict reports incompatible reuse of an idempotency key.
	ErrMessageConflict = errors.New("message idempotency conflict")
	// ErrMessageCursorInvalid reports an invalid mailbox cursor.
	ErrMessageCursorInvalid = errors.New("malformed message cursor")
)

// MessageAgent identifies the token-derived sender.
type MessageAgent struct {
	ID   string `json:"agent_id"`
	Name string `json:"agent_name"`
}

// MessageRecipient identifies one resolved direct recipient.
type MessageRecipient struct {
	Kind string `json:"kind"`
	ID   string `json:"agent_id"`
	Name string `json:"agent_name"`
}

// MessageDelivery is the durable recipient delivery state.
type MessageDelivery struct {
	State       string     `json:"state"`
	DeliveredAt *time.Time `json:"delivered_at,omitempty"`
}

// MessageReadState is the recipient's unread/read/acked transition state.
type MessageReadState struct {
	State   string     `json:"state"`
	ReadAt  *time.Time `json:"read_at,omitempty"`
	AckedAt *time.Time `json:"acked_at,omitempty"`
}

// Message is one direct realm-local agent message plus the recipient's state.
// Body and Payload are empty on list results and populated only by send/read.
type Message struct {
	ID        string           `json:"id"`
	AccountID string           `json:"account_id"`
	RealmID   string           `json:"realm_id"`
	From      MessageAgent     `json:"from"`
	To        MessageRecipient `json:"to"`
	Subject   string           `json:"subject,omitempty"`
	Kind      string           `json:"kind"`
	Body      string           `json:"body,omitempty"`
	Payload   json.RawMessage  `json:"payload,omitempty"`
	ThreadID  string           `json:"thread_id"`
	CreatedAt time.Time        `json:"created_at"`
	Delivery  MessageDelivery  `json:"delivery"`
	ReadState MessageReadState `json:"read_state"`
}

// SendMessageInput is the caller-controlled, non-identity send payload.
type SendMessageInput struct {
	ToAgent        string
	Subject        string
	Kind           string
	Body           string
	Payload        json.RawMessage
	ThreadID       string
	IdempotencyKey string
}

// MessageFilter selects one bounded inbox or outbox page.
type MessageFilter struct {
	Direction string
	Unread    bool
	From      string
	ThreadID  string
	Kind      string
	Limit     int
	Cursor    string
}

// MessagePage is a metadata-only mailbox page and continuation cursor.
type MessagePage struct {
	Messages   []Message
	NextCursor string
}

// SendMessage resolves the direct recipient in the token-derived realm and
// atomically creates the immutable message, delivery row, and audit events.
func (s *Store) SendMessage(ctx context.Context, p Principal, in SendMessageInput) (Message, error) {
	if p.Kind != PrincipalAgent {
		return Message{}, ErrMessageForbidden
	}
	var err error
	in, err = normalizeSendMessageInput(in)
	if err != nil {
		return Message{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Message{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAccountForMint(ctx, tx, p.AccountID, false); err != nil {
		return Message{}, err
	}
	if err := verifyLiveAgentScope(ctx, tx, p.AccountID, p.RealmID, p.ID); err != nil {
		return Message{}, err
	}

	to, err := resolveMessageAgent(ctx, tx, p.AccountID, p.RealmID, in.ToAgent)
	if err != nil {
		return Message{}, err
	}
	if in.IdempotencyKey != "" {
		existing, findErr := messageByIdempotencyKey(ctx, tx, p.AccountID, p.ID, in.IdempotencyKey)
		switch {
		case findErr == nil:
			if !messageMatchesSend(existing, to.ID, in) {
				return Message{}, ErrMessageConflict
			}
			if err := tx.Commit(ctx); err != nil {
				return Message{}, err
			}
			return existing, nil
		case !errors.Is(findErr, pgx.ErrNoRows):
			return Message{}, fmt.Errorf("find message retry: %w", findErr)
		}
	}

	threadID := in.ThreadID
	if threadID == "" {
		threadID, err = id.New("thr")
		if err != nil {
			return Message{}, err
		}
	}
	messageID, err := id.New("msg")
	if err != nil {
		return Message{}, err
	}
	var payload any
	if len(in.Payload) > 0 {
		payload = string(in.Payload)
	}
	var createdAt time.Time
	err = tx.QueryRow(ctx, `
		INSERT INTO agent_messages
		  (id, account_id, realm_id, from_agent_id, to_agent_id,
		   subject, kind, body, payload, thread_id, idempotency_key)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8,
		        CASE WHEN $9::text IS NULL THEN NULL ELSE $9::jsonb END,
		        $10, NULLIF($11, ''))
		ON CONFLICT (account_id, from_agent_id, idempotency_key) DO NOTHING
		RETURNING created_at`,
		messageID, p.AccountID, p.RealmID, p.ID, to.ID,
		in.Subject, in.Kind, in.Body, payload, threadID, in.IdempotencyKey).Scan(&createdAt)
	if errors.Is(err, pgx.ErrNoRows) && in.IdempotencyKey != "" {
		existing, findErr := messageByIdempotencyKey(ctx, tx, p.AccountID, p.ID, in.IdempotencyKey)
		if findErr != nil {
			return Message{}, fmt.Errorf("find concurrent message retry: %w", findErr)
		}
		if !messageMatchesSend(existing, to.ID, in) {
			return Message{}, ErrMessageConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return Message{}, err
		}
		return existing, nil
	}
	if err != nil {
		return Message{}, fmt.Errorf("insert message: %w", err)
	}

	var delivery MessageDelivery
	err = tx.QueryRow(ctx, `
		INSERT INTO agent_message_deliveries
		  (message_id, account_id, realm_id, recipient_agent_id, state, delivered_at)
		VALUES ($1, $2, $3, $4, 'delivered', now())
		RETURNING state, delivered_at`, messageID, p.AccountID, p.RealmID, to.ID).
		Scan(&delivery.State, &delivery.DeliveredAt)
	if err != nil {
		return Message{}, fmt.Errorf("insert message delivery: %w", err)
	}

	msg := Message{
		ID: messageID, AccountID: p.AccountID, RealmID: p.RealmID,
		From:    MessageAgent{ID: p.ID, Name: p.AgentName},
		To:      MessageRecipient{Kind: MessageRecipientAgent, ID: to.ID, Name: to.Name},
		Subject: in.Subject, Kind: in.Kind, Body: in.Body, Payload: in.Payload,
		ThreadID: threadID, CreatedAt: createdAt, Delivery: delivery,
		ReadState: MessageReadState{State: MessageReadUnread},
	}
	if err := logMessageEvent(ctx, tx, VerbMessageSent, ActorAgent, p.ID, msg); err != nil {
		return Message{}, err
	}
	if err := logMessageEvent(ctx, tx, VerbMessageDelivered, ActorSystem, "", msg); err != nil {
		return Message{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Message{}, err
	}
	return msg, nil
}

// ListMessages returns metadata-only mailbox rows; listing does not mark read.
func (s *Store) ListMessages(ctx context.Context, p Principal, filter MessageFilter) (MessagePage, error) {
	if p.Kind != PrincipalAgent {
		return MessagePage{}, ErrMessageForbidden
	}
	filter, cursorTime, cursorID, err := normalizeMessageFilter(filter)
	if err != nil {
		return MessagePage{}, err
	}

	args := []any{p.AccountID, p.RealmID, p.ID}
	q := &strings.Builder{}
	q.WriteString(messageSelect(false))
	if filter.Direction == MessageDirectionInbox {
		q.WriteString(` WHERE d.account_id = $1 AND d.realm_id = $2 AND d.recipient_agent_id = $3`)
	} else {
		q.WriteString(` WHERE m.account_id = $1 AND m.realm_id = $2 AND m.from_agent_id = $3`)
	}
	if filter.Unread {
		q.WriteString(` AND d.read_at IS NULL`)
	}
	if filter.From != "" {
		args = append(args, filter.From)
		fmt.Fprintf(q, ` AND (m.from_agent_id = $%d OR sender.name = $%d)`, len(args), len(args))
	}
	if filter.ThreadID != "" {
		args = append(args, filter.ThreadID)
		fmt.Fprintf(q, ` AND m.thread_id = $%d`, len(args))
	}
	if filter.Kind != "" {
		args = append(args, filter.Kind)
		fmt.Fprintf(q, ` AND m.kind = $%d`, len(args))
	}
	if !cursorTime.IsZero() {
		args = append(args, cursorTime, cursorID)
		fmt.Fprintf(q, ` AND (m.created_at, m.id) < ($%d, $%d)`, len(args)-1, len(args))
	}
	args = append(args, filter.Limit+1)
	fmt.Fprintf(q, ` ORDER BY m.created_at DESC, m.id DESC LIMIT $%d`, len(args))

	rows, err := s.pool.Query(ctx, q.String(), args...)
	if err != nil {
		return MessagePage{}, fmt.Errorf("list messages: %w", err)
	}
	defer rows.Close()
	out := make([]Message, 0, filter.Limit)
	for rows.Next() {
		msg, err := scanMessage(rows)
		if err != nil {
			return MessagePage{}, err
		}
		out = append(out, msg)
	}
	if err := rows.Err(); err != nil {
		return MessagePage{}, err
	}
	var next string
	if len(out) > filter.Limit {
		last := out[filter.Limit-1]
		next = encodeMessageCursor(last.CreatedAt, last.ID)
		out = out[:filter.Limit]
	}
	return MessagePage{Messages: out, NextCursor: next}, nil
}

// ReadMessage returns content to the recipient and idempotently marks it read.
func (s *Store) ReadMessage(ctx context.Context, p Principal, messageID string) (Message, error) {
	return s.transitionMessage(ctx, p, strings.TrimSpace(messageID), false)
}

// AckMessage idempotently marks the recipient delivery read and acknowledged.
func (s *Store) AckMessage(ctx context.Context, p Principal, messageID string) (Message, error) {
	return s.transitionMessage(ctx, p, strings.TrimSpace(messageID), true)
}

func (s *Store) transitionMessage(ctx context.Context, p Principal, messageID string, ack bool) (Message, error) {
	if p.Kind != PrincipalAgent {
		return Message{}, ErrMessageForbidden
	}
	if messageID == "" {
		return Message{}, fmt.Errorf("%w: message id is required", ErrMessageInputInvalid)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Message{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAccountForMint(ctx, tx, p.AccountID, false); err != nil {
		return Message{}, err
	}
	if err := verifyLiveAgentScope(ctx, tx, p.AccountID, p.RealmID, p.ID); err != nil {
		return Message{}, err
	}

	row := tx.QueryRow(ctx, messageSelect(true)+`
		WHERE m.id = $1 AND d.account_id = $2 AND d.realm_id = $3
		  AND d.recipient_agent_id = $4
		FOR UPDATE OF d`, messageID, p.AccountID, p.RealmID, p.ID)
	msg, err := scanMessage(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Message{}, ErrMessageNotFound
	}
	if err != nil {
		return Message{}, fmt.Errorf("lock message delivery: %w", err)
	}
	wasUnread := msg.ReadState.ReadAt == nil
	wasUnacked := msg.ReadState.AckedAt == nil
	if ack {
		err = tx.QueryRow(ctx, `
			UPDATE agent_message_deliveries
			SET read_at = COALESCE(read_at, now()), acked_at = COALESCE(acked_at, now())
			WHERE message_id = $1 AND recipient_agent_id = $2
			RETURNING read_at, acked_at`, messageID, p.ID).
			Scan(&msg.ReadState.ReadAt, &msg.ReadState.AckedAt)
	} else {
		err = tx.QueryRow(ctx, `
			UPDATE agent_message_deliveries
			SET read_at = COALESCE(read_at, now())
			WHERE message_id = $1 AND recipient_agent_id = $2
			RETURNING read_at, acked_at`, messageID, p.ID).
			Scan(&msg.ReadState.ReadAt, &msg.ReadState.AckedAt)
	}
	if err != nil {
		return Message{}, fmt.Errorf("advance message state: %w", err)
	}
	msg.ReadState.State = readState(msg.ReadState.ReadAt, msg.ReadState.AckedAt)
	if wasUnread {
		if err := logMessageEvent(ctx, tx, VerbMessageRead, ActorAgent, p.ID, msg); err != nil {
			return Message{}, err
		}
	}
	if ack && wasUnacked {
		if err := logMessageEvent(ctx, tx, VerbMessageAcked, ActorAgent, p.ID, msg); err != nil {
			return Message{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Message{}, err
	}
	return msg, nil
}

func normalizeSendMessageInput(in SendMessageInput) (SendMessageInput, error) {
	in.ToAgent = strings.TrimSpace(in.ToAgent)
	in.Subject = strings.TrimSpace(in.Subject)
	in.Kind = strings.TrimSpace(in.Kind)
	in.ThreadID = strings.TrimSpace(in.ThreadID)
	in.IdempotencyKey = strings.TrimSpace(in.IdempotencyKey)
	if in.Kind == "" {
		in.Kind = "note"
	}
	switch {
	case in.ToAgent == "":
		return SendMessageInput{}, fmt.Errorf("%w: recipient is required", ErrMessageInputInvalid)
	case len(in.ToAgent) > maxMessageRecipientBytes:
		return SendMessageInput{}, fmt.Errorf("%w: recipient exceeds %d bytes", ErrMessageInputInvalid, maxMessageRecipientBytes)
	case len(in.Subject) > maxMessageSubjectBytes:
		return SendMessageInput{}, fmt.Errorf("%w: subject exceeds %d bytes", ErrMessageInputInvalid, maxMessageSubjectBytes)
	case len(in.Kind) > maxMessageKindBytes:
		return SendMessageInput{}, fmt.Errorf("%w: kind exceeds %d bytes", ErrMessageInputInvalid, maxMessageKindBytes)
	case strings.TrimSpace(in.Body) == "":
		return SendMessageInput{}, fmt.Errorf("%w: body is required", ErrMessageInputInvalid)
	case len(in.Body) > maxMessageBodyBytes:
		return SendMessageInput{}, fmt.Errorf("%w: body exceeds %d bytes", ErrMessageInputInvalid, maxMessageBodyBytes)
	case in.ThreadID != "" && (!strings.HasPrefix(in.ThreadID, "thr_") || len(in.ThreadID) > maxMessageThreadIDBytes):
		return SendMessageInput{}, fmt.Errorf("%w: thread_id must be a thr_ id no longer than %d bytes", ErrMessageInputInvalid, maxMessageThreadIDBytes)
	case len(in.IdempotencyKey) > maxMessageIdempotencyKeyBytes:
		return SendMessageInput{}, fmt.Errorf("%w: idempotency key exceeds %d bytes", ErrMessageInputInvalid, maxMessageIdempotencyKeyBytes)
	}
	if strings.TrimSpace(string(in.Payload)) == "null" {
		in.Payload = nil
	}
	payload, err := normalizeJSONObject(in.Payload, false)
	if err != nil {
		return SendMessageInput{}, fmt.Errorf("%w: payload %v", ErrMessageInputInvalid, err)
	}
	if len(payload) > maxMessagePayloadBytes {
		return SendMessageInput{}, fmt.Errorf("%w: payload exceeds %d bytes", ErrMessageInputInvalid, maxMessagePayloadBytes)
	}
	in.Payload = payload
	return in, nil
}

func normalizeMessageFilter(filter MessageFilter) (MessageFilter, time.Time, string, error) {
	filter.Direction = strings.TrimSpace(filter.Direction)
	filter.From = strings.TrimSpace(filter.From)
	filter.ThreadID = strings.TrimSpace(filter.ThreadID)
	filter.Kind = strings.TrimSpace(filter.Kind)
	if filter.Direction == "" {
		filter.Direction = MessageDirectionInbox
	}
	if filter.Direction != MessageDirectionInbox && filter.Direction != MessageDirectionOutbox {
		return MessageFilter{}, time.Time{}, "", fmt.Errorf("%w: direction must be inbox or outbox", ErrMessageInputInvalid)
	}
	if filter.Direction == MessageDirectionOutbox && (filter.Unread || filter.From != "") {
		return MessageFilter{}, time.Time{}, "", fmt.Errorf("%w: unread/from filters apply only to inbox", ErrMessageInputInvalid)
	}
	if filter.ThreadID != "" && (!strings.HasPrefix(filter.ThreadID, "thr_") || len(filter.ThreadID) > maxMessageThreadIDBytes) {
		return MessageFilter{}, time.Time{}, "", fmt.Errorf("%w: invalid thread_id", ErrMessageInputInvalid)
	}
	if filter.Limit == 0 {
		filter.Limit = defaultMessagePageSize
	}
	if filter.Limit < 1 || filter.Limit > maxMessagePageSize {
		return MessageFilter{}, time.Time{}, "", fmt.Errorf("%w: limit must be 1-%d", ErrMessageInputInvalid, maxMessagePageSize)
	}
	if filter.Cursor == "" {
		return filter, time.Time{}, "", nil
	}
	cursorTime, cursorID, err := decodeMessageCursor(filter.Cursor)
	if err != nil {
		return MessageFilter{}, time.Time{}, "", err
	}
	return filter, cursorTime, cursorID, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanMessage(row rowScanner) (Message, error) {
	var msg Message
	if err := row.Scan(
		&msg.ID, &msg.AccountID, &msg.RealmID,
		&msg.From.ID, &msg.From.Name, &msg.To.ID, &msg.To.Name,
		&msg.Subject, &msg.Kind, &msg.Body, &msg.Payload, &msg.ThreadID, &msg.CreatedAt,
		&msg.Delivery.State, &msg.Delivery.DeliveredAt,
		&msg.ReadState.ReadAt, &msg.ReadState.AckedAt,
	); err != nil {
		return Message{}, err
	}
	msg.To.Kind = MessageRecipientAgent
	msg.ReadState.State = readState(msg.ReadState.ReadAt, msg.ReadState.AckedAt)
	return msg, nil
}

func messageSelect(includeContent bool) string {
	body := `''::text`
	payload := `NULL::jsonb`
	if includeContent {
		body = `m.body`
		payload = `m.payload`
	}
	return fmt.Sprintf(`
		SELECT m.id, m.account_id, m.realm_id,
		       m.from_agent_id, sender.name, m.to_agent_id, recipient.name,
		       m.subject, m.kind, %s, %s, m.thread_id, m.created_at,
		       d.state, d.delivered_at, d.read_at, d.acked_at
		FROM agent_messages m
		JOIN agents sender ON sender.id = m.from_agent_id
		JOIN agents recipient ON recipient.id = m.to_agent_id
		JOIN agent_message_deliveries d
		  ON d.message_id = m.id AND d.recipient_agent_id = m.to_agent_id
	`, body, payload)
}

func resolveMessageAgent(ctx context.Context, tx pgx.Tx, accountID, realmID, selector string) (Agent, error) {
	var agent Agent
	err := tx.QueryRow(ctx, `
		SELECT a.id, a.name
		FROM agents a
		JOIN realms r ON r.id = a.realm_id
		WHERE r.account_id = $1 AND a.realm_id = $2
		  AND a.deleted_at IS NULL AND r.deleted_at IS NULL
		  AND (a.id = $3 OR a.name = $3)
		ORDER BY CASE WHEN a.id = $3 THEN 0 ELSE 1 END
		LIMIT 1`, accountID, realmID, selector).Scan(&agent.ID, &agent.Name)
	if errors.Is(err, pgx.ErrNoRows) {
		return Agent{}, ErrMessageRecipientMissing
	}
	if err != nil {
		return Agent{}, fmt.Errorf("resolve message recipient: %w", err)
	}
	return agent, nil
}

func messageByIdempotencyKey(ctx context.Context, tx pgx.Tx, accountID, senderID, key string) (Message, error) {
	return scanMessage(tx.QueryRow(ctx, messageSelect(true)+`
		WHERE m.account_id = $1 AND m.from_agent_id = $2 AND m.idempotency_key = $3`,
		accountID, senderID, key))
}

func messageMatchesSend(msg Message, toAgentID string, in SendMessageInput) bool {
	threadMatches := in.ThreadID == "" || msg.ThreadID == in.ThreadID
	return msg.To.ID == toAgentID && msg.Subject == in.Subject && msg.Kind == in.Kind &&
		msg.Body == in.Body && rawJSONEqual(msg.Payload, in.Payload) && threadMatches
}

func readState(readAt, ackedAt *time.Time) string {
	if ackedAt != nil {
		return MessageReadAcked
	}
	if readAt != nil {
		return MessageReadRead
	}
	return MessageReadUnread
}

func logMessageEvent(ctx context.Context, tx pgx.Tx, verb, actorKind, actorID string, msg Message) error {
	return logEventTx(ctx, tx, EventInput{
		AccountID: msg.AccountID, ActorKind: actorKind, ActorID: actorID,
		Verb: verb, Metadata: messageEventMetadata(msg),
	})
}

func messageEventMetadata(msg Message) map[string]any {
	return map[string]any{
		"message_id": msg.ID, "from_agent_id": msg.From.ID,
		"recipient_kind": MessageRecipientAgent, "recipient_agent_id": msg.To.ID,
		"kind": msg.Kind, "thread_id": msg.ThreadID,
		"subject_present": msg.Subject != "",
	}
}

func encodeMessageCursor(t time.Time, messageID string) string {
	return fmt.Sprintf("%d:%s", t.UnixNano(), messageID)
}

func decodeMessageCursor(cursor string) (time.Time, string, error) {
	i := strings.IndexByte(cursor, ':')
	if i <= 0 || i == len(cursor)-1 {
		return time.Time{}, "", ErrMessageCursorInvalid
	}
	tsPart := cursor[:i]
	for _, r := range tsPart {
		if r < '0' || r > '9' {
			return time.Time{}, "", ErrMessageCursorInvalid
		}
	}
	ns, err := strconv.ParseInt(tsPart, 10, 64)
	if err != nil || !strings.HasPrefix(cursor[i+1:], "msg_") {
		return time.Time{}, "", ErrMessageCursorInvalid
	}
	return time.Unix(0, ns).UTC(), cursor[i+1:], nil
}
