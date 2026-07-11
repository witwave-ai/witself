package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/witwave-ai/witself/internal/id"
)

// Transcript entry roles. They describe recorded content, not authentication.
const (
	TranscriptRoleUser      = "user"
	TranscriptRoleAssistant = "assistant"
	TranscriptRoleSystem    = "system"
	TranscriptRoleTool      = "tool"

	maxTranscriptTitleBytes      = 256
	maxTranscriptExternalIDBytes = 512
	maxTranscriptBodyBytes       = 64 * 1024
	maxTranscriptJSONBytes       = 16 * 1024
	maxTranscriptModelBytes      = 256
	maxTranscriptAppendBatch     = 100
	defaultTranscriptPageSize    = 100
	maxTranscriptPageSize        = 500
)

var (
	// ErrTranscriptExists reports a duplicate transcript or entry external id.
	ErrTranscriptExists = errors.New("transcript already exists")
	// ErrTranscriptNotFound reports an unknown account-scoped transcript.
	ErrTranscriptNotFound = errors.New("transcript not found")
	// ErrTranscriptForbidden reports an agent crossing transcript ownership.
	ErrTranscriptForbidden = errors.New("transcript access forbidden")
	// ErrTranscriptInputInvalid reports caller-correctable transcript content.
	ErrTranscriptInputInvalid = errors.New("invalid transcript input")
)

// Transcript is one visible interaction thread owned by the agent integration
// that records it. Metadata is always a JSON object.
type Transcript struct {
	ID           string          `json:"id"`
	AccountID    string          `json:"account_id"`
	RealmID      string          `json:"realm_id"`
	OwnerAgentID string          `json:"owner_agent_id"`
	ExternalID   string          `json:"external_id,omitempty"`
	Title        string          `json:"title,omitempty"`
	Metadata     json.RawMessage `json:"metadata"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
}

// TranscriptEntry is one immutable visible turn or explicit system/tool trace.
// RecordedByAgentID is always derived from the authenticated token.
type TranscriptEntry struct {
	ID                string          `json:"id"`
	AccountID         string          `json:"account_id"`
	TranscriptID      string          `json:"transcript_id"`
	RealmID           string          `json:"realm_id"`
	RecordedByAgentID string          `json:"recorded_by_agent_id"`
	Sequence          int64           `json:"sequence"`
	ExternalID        string          `json:"external_id,omitempty"`
	Role              string          `json:"role"`
	Body              string          `json:"body,omitempty"`
	Payload           json.RawMessage `json:"payload,omitempty"`
	Model             string          `json:"model,omitempty"`
	ReplyToEntryID    string          `json:"reply_to_entry_id,omitempty"`
	Artifacts         json.RawMessage `json:"artifacts"`
	CreatedAt         time.Time       `json:"created_at"`
}

// CreateTranscriptInput carries optional metadata for a new transcript.
type CreateTranscriptInput struct {
	ExternalID string
	Title      string
	Metadata   json.RawMessage
}

// AppendTranscriptEntryInput carries one visible turn to append.
type AppendTranscriptEntryInput struct {
	ExternalID        string
	Role              string
	Body              string
	Payload           json.RawMessage
	Model             string
	ReplyToEntryID    string
	ReplyToExternalID string
	Artifacts         json.RawMessage
}

// TranscriptPageOptions bounds a transcript read. Tail returns the newest
// entries (still ordered oldest-first); otherwise AfterSequence pages forward.
type TranscriptPageOptions struct {
	AfterSequence int64
	Limit         int
	Tail          bool
}

// TranscriptPage is one bounded transcript read.
type TranscriptPage struct {
	Transcript        Transcript
	Entries           []TranscriptEntry
	NextAfterSequence int64
}

// CreateTranscript creates an empty transcript for the token-derived agent.
func (s *Store) CreateTranscript(ctx context.Context, accountID, realmID, agentID string, in CreateTranscriptInput) (Transcript, error) {
	in, err := normalizeCreateTranscriptInput(in)
	if err != nil {
		return Transcript{}, err
	}
	transcriptID, err := id.New("trn")
	if err != nil {
		return Transcript{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Transcript{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := lockAccountForMint(ctx, tx, accountID, false); err != nil {
		return Transcript{}, err
	}
	if err := verifyLiveAgentScope(ctx, tx, accountID, realmID, agentID); err != nil {
		return Transcript{}, err
	}

	var out Transcript
	err = tx.QueryRow(ctx, `
		INSERT INTO transcript_conversations
		  (id, account_id, realm_id, owner_agent_id, external_id, title, metadata)
		VALUES ($1, $2, $3, $4, NULLIF($5, ''), NULLIF($6, ''), $7::jsonb)
		ON CONFLICT (account_id, owner_agent_id, external_id)
		DO UPDATE SET external_id = EXCLUDED.external_id
		RETURNING id, account_id, realm_id, owner_agent_id,
		          COALESCE(external_id, ''), COALESCE(title, ''), metadata,
		          created_at, updated_at`,
		transcriptID, accountID, realmID, agentID, in.ExternalID, in.Title, string(in.Metadata)).
		Scan(&out.ID, &out.AccountID, &out.RealmID, &out.OwnerAgentID,
			&out.ExternalID, &out.Title, &out.Metadata, &out.CreatedAt, &out.UpdatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return Transcript{}, ErrTranscriptExists
		}
		return Transcript{}, fmt.Errorf("create transcript: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Transcript{}, err
	}
	return out, nil
}

// AppendTranscriptEntry appends one immutable entry.
func (s *Store) AppendTranscriptEntry(ctx context.Context, accountID, realmID, agentID, transcriptID string, in AppendTranscriptEntryInput) (TranscriptEntry, error) {
	entries, err := s.AppendTranscriptEntries(ctx, accountID, realmID, agentID, transcriptID, []AppendTranscriptEntryInput{in})
	if err != nil {
		return TranscriptEntry{}, err
	}
	return entries[0], nil
}

// AppendTranscriptEntries appends a bounded batch under one transcript lock.
// Existing matching external ids are returned, making outbox retries safe.
func (s *Store) AppendTranscriptEntries(ctx context.Context, accountID, realmID, agentID, transcriptID string, inputs []AppendTranscriptEntryInput) ([]TranscriptEntry, error) {
	if len(inputs) == 0 || len(inputs) > maxTranscriptAppendBatch {
		return nil, fmt.Errorf("%w: entries batch must contain 1-%d items", ErrTranscriptInputInvalid, maxTranscriptAppendBatch)
	}
	for i := range inputs {
		in, err := normalizeAppendTranscriptEntryInput(inputs[i])
		if err != nil {
			return nil, fmt.Errorf("entry %d: %w", i, err)
		}
		inputs[i] = in
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := lockAccountForMint(ctx, tx, accountID, false); err != nil {
		return nil, err
	}
	if err := verifyLiveAgentScope(ctx, tx, accountID, realmID, agentID); err != nil {
		return nil, err
	}

	var storedRealmID, ownerAgentID string
	var sequence int64
	err = tx.QueryRow(ctx, `
		SELECT realm_id, owner_agent_id, next_sequence
		FROM transcript_conversations
		WHERE id = $1 AND account_id = $2
		FOR UPDATE`, transcriptID, accountID).
		Scan(&storedRealmID, &ownerAgentID, &sequence)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrTranscriptNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lock transcript: %w", err)
	}
	if storedRealmID != realmID || ownerAgentID != agentID {
		return nil, ErrTranscriptForbidden
	}

	out := make([]TranscriptEntry, 0, len(inputs))
	startSequence := sequence
	var latestInsertedAt time.Time
	for _, in := range inputs {
		if in.ReplyToExternalID != "" {
			if in.ReplyToEntryID != "" {
				return nil, fmt.Errorf("%w: set only one of reply_to_entry_id or reply_to_external_id", ErrTranscriptInputInvalid)
			}
			err := tx.QueryRow(ctx, `
				SELECT id FROM transcript_entries
				WHERE transcript_id = $1 AND account_id = $2 AND external_id = $3`,
				transcriptID, accountID, in.ReplyToExternalID).Scan(&in.ReplyToEntryID)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, fmt.Errorf("%w: reply external id is not in this transcript", ErrTranscriptInputInvalid)
			}
			if err != nil {
				return nil, fmt.Errorf("resolve transcript reply target: %w", err)
			}
		}
		if in.ReplyToEntryID != "" {
			var exists bool
			err := tx.QueryRow(ctx, `
			SELECT EXISTS (
			  SELECT 1 FROM transcript_entries
			  WHERE id = $1 AND transcript_id = $2 AND account_id = $3
			)`, in.ReplyToEntryID, transcriptID, accountID).Scan(&exists)
			if err != nil {
				return nil, fmt.Errorf("check transcript reply target: %w", err)
			}
			if !exists {
				return nil, fmt.Errorf("%w: reply target is not in this transcript", ErrTranscriptInputInvalid)
			}
		}

		if in.ExternalID != "" {
			existing, err := transcriptEntryByExternalID(ctx, tx, accountID, transcriptID, in.ExternalID)
			switch {
			case err == nil:
				if !transcriptEntryMatches(existing, in) {
					return nil, ErrTranscriptExists
				}
				out = append(out, existing)
				continue
			case !errors.Is(err, pgx.ErrNoRows):
				return nil, fmt.Errorf("find transcript entry retry: %w", err)
			}
		}

		entryID, err := id.New("ent")
		if err != nil {
			return nil, err
		}
		var payload any
		if len(in.Payload) > 0 {
			payload = string(in.Payload)
		}
		var entry TranscriptEntry
		err = tx.QueryRow(ctx, `
		INSERT INTO transcript_entries
		  (id, account_id, transcript_id, realm_id, recorded_by_agent_id,
		   sequence, external_id, role, body, payload, model, reply_to_entry_id, artifacts)
		VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''), $8,
		        $9, CASE WHEN $10::text IS NULL THEN NULL ELSE $10::jsonb END,
		        NULLIF($11, ''), NULLIF($12, ''), $13::jsonb)
		RETURNING id, account_id, transcript_id, realm_id, recorded_by_agent_id,
		          sequence, COALESCE(external_id, ''), role, body, payload, COALESCE(model, ''),
		          COALESCE(reply_to_entry_id, ''), artifacts, created_at`,
			entryID, accountID, transcriptID, realmID, agentID,
			sequence, in.ExternalID, in.Role, in.Body, payload, in.Model, in.ReplyToEntryID, string(in.Artifacts)).
			Scan(&entry.ID, &entry.AccountID, &entry.TranscriptID, &entry.RealmID,
				&entry.RecordedByAgentID, &entry.Sequence, &entry.ExternalID, &entry.Role, &entry.Body,
				&entry.Payload, &entry.Model, &entry.ReplyToEntryID, &entry.Artifacts, &entry.CreatedAt)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return nil, ErrTranscriptExists
			}
			return nil, fmt.Errorf("append transcript entry: %w", err)
		}
		out = append(out, entry)
		latestInsertedAt = entry.CreatedAt
		sequence++
	}
	if sequence != startSequence {
		if _, err := tx.Exec(ctx, `
		UPDATE transcript_conversations
		SET next_sequence = $2, updated_at = $3
		WHERE id = $1`, transcriptID, sequence, latestInsertedAt); err != nil {
			return nil, fmt.Errorf("advance transcript sequence: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

func transcriptEntryByExternalID(ctx context.Context, tx pgx.Tx, accountID, transcriptID, externalID string) (TranscriptEntry, error) {
	var entry TranscriptEntry
	err := tx.QueryRow(ctx, `
		SELECT id, account_id, transcript_id, realm_id, recorded_by_agent_id,
		       sequence, COALESCE(external_id, ''), role, body, payload, COALESCE(model, ''),
		       COALESCE(reply_to_entry_id, ''), artifacts, created_at
		FROM transcript_entries
		WHERE transcript_id = $1 AND account_id = $2 AND external_id = $3`,
		transcriptID, accountID, externalID).
		Scan(&entry.ID, &entry.AccountID, &entry.TranscriptID, &entry.RealmID,
			&entry.RecordedByAgentID, &entry.Sequence, &entry.ExternalID, &entry.Role,
			&entry.Body, &entry.Payload, &entry.Model, &entry.ReplyToEntryID,
			&entry.Artifacts, &entry.CreatedAt)
	return entry, err
}

func transcriptEntryMatches(entry TranscriptEntry, in AppendTranscriptEntryInput) bool {
	return entry.Role == in.Role && entry.Body == in.Body && entry.Model == in.Model &&
		entry.ReplyToEntryID == in.ReplyToEntryID && rawJSONEqual(entry.Payload, in.Payload) &&
		rawJSONEqual(entry.Artifacts, in.Artifacts)
}

func rawJSONEqual(a, b json.RawMessage) bool {
	if len(a) == 0 || string(a) == "null" {
		a = nil
	}
	if len(b) == 0 || string(b) == "null" {
		b = nil
	}
	if len(a) == 0 || len(b) == 0 {
		return len(a) == len(b)
	}
	var av, bv any
	return json.Unmarshal(a, &av) == nil && json.Unmarshal(b, &bv) == nil && reflect.DeepEqual(av, bv)
}

// ListTranscripts returns newest-active first. Agents see only their own;
// operators see every transcript in the account.
func (s *Store) ListTranscripts(ctx context.Context, p Principal) ([]Transcript, error) {
	if p.Kind != PrincipalAgent && p.Kind != PrincipalOperator {
		return nil, ErrTranscriptForbidden
	}
	query := `
		SELECT id, account_id, realm_id, owner_agent_id,
		       COALESCE(external_id, ''), COALESCE(title, ''), metadata,
		       created_at, updated_at
		FROM transcript_conversations
		WHERE account_id = $1`
	args := []any{p.AccountID}
	if p.Kind == PrincipalAgent {
		query += ` AND owner_agent_id = $2`
		args = append(args, p.ID)
	}
	query += ` ORDER BY updated_at DESC, id DESC LIMIT 100`
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list transcripts: %w", err)
	}
	defer rows.Close()
	var out []Transcript
	for rows.Next() {
		var tr Transcript
		if err := rows.Scan(&tr.ID, &tr.AccountID, &tr.RealmID, &tr.OwnerAgentID,
			&tr.ExternalID, &tr.Title, &tr.Metadata, &tr.CreatedAt, &tr.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, tr)
	}
	return out, rows.Err()
}

// GetTranscript returns one transcript and all of its entries oldest-first.
// Agents may read only their own transcript; operators may inspect the account.
func (s *Store) GetTranscript(ctx context.Context, p Principal, transcriptID string) (Transcript, []TranscriptEntry, error) {
	if p.Kind != PrincipalAgent && p.Kind != PrincipalOperator {
		return Transcript{}, nil, ErrTranscriptForbidden
	}
	var tr Transcript
	err := s.pool.QueryRow(ctx, `
		SELECT id, account_id, realm_id, owner_agent_id,
		       COALESCE(external_id, ''), COALESCE(title, ''), metadata,
		       created_at, updated_at
		FROM transcript_conversations
		WHERE id = $1 AND account_id = $2`, transcriptID, p.AccountID).
		Scan(&tr.ID, &tr.AccountID, &tr.RealmID, &tr.OwnerAgentID,
			&tr.ExternalID, &tr.Title, &tr.Metadata, &tr.CreatedAt, &tr.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Transcript{}, nil, ErrTranscriptNotFound
	}
	if err != nil {
		return Transcript{}, nil, fmt.Errorf("get transcript: %w", err)
	}
	if p.Kind == PrincipalAgent && tr.OwnerAgentID != p.ID {
		return Transcript{}, nil, ErrTranscriptForbidden
	}

	rows, err := s.pool.Query(ctx, `
		SELECT id, account_id, transcript_id, realm_id, recorded_by_agent_id,
		       sequence, COALESCE(external_id, ''), role, body, payload, COALESCE(model, ''),
		       COALESCE(reply_to_entry_id, ''), artifacts, created_at
		FROM transcript_entries
		WHERE transcript_id = $1 AND account_id = $2
		ORDER BY sequence, id`, transcriptID, p.AccountID)
	if err != nil {
		return Transcript{}, nil, fmt.Errorf("list transcript entries: %w", err)
	}
	defer rows.Close()
	var entries []TranscriptEntry
	for rows.Next() {
		var entry TranscriptEntry
		if err := rows.Scan(&entry.ID, &entry.AccountID, &entry.TranscriptID,
			&entry.RealmID, &entry.RecordedByAgentID, &entry.Sequence, &entry.ExternalID, &entry.Role,
			&entry.Body, &entry.Payload, &entry.Model, &entry.ReplyToEntryID,
			&entry.Artifacts, &entry.CreatedAt); err != nil {
			return Transcript{}, nil, err
		}
		entries = append(entries, entry)
	}
	return tr, entries, rows.Err()
}

// GetTranscriptPage returns a bounded page without loading the full transcript.
func (s *Store) GetTranscriptPage(ctx context.Context, p Principal, transcriptID string, opts TranscriptPageOptions) (TranscriptPage, error) {
	if p.Kind != PrincipalAgent && p.Kind != PrincipalOperator {
		return TranscriptPage{}, ErrTranscriptForbidden
	}
	opts, err := normalizeTranscriptPageOptions(opts)
	if err != nil {
		return TranscriptPage{}, err
	}
	var tr Transcript
	err = s.pool.QueryRow(ctx, `
		SELECT id, account_id, realm_id, owner_agent_id,
		       COALESCE(external_id, ''), COALESCE(title, ''), metadata,
		       created_at, updated_at
		FROM transcript_conversations
		WHERE id = $1 AND account_id = $2`, transcriptID, p.AccountID).
		Scan(&tr.ID, &tr.AccountID, &tr.RealmID, &tr.OwnerAgentID,
			&tr.ExternalID, &tr.Title, &tr.Metadata, &tr.CreatedAt, &tr.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return TranscriptPage{}, ErrTranscriptNotFound
	}
	if err != nil {
		return TranscriptPage{}, fmt.Errorf("get transcript: %w", err)
	}
	if p.Kind == PrincipalAgent && tr.OwnerAgentID != p.ID {
		return TranscriptPage{}, ErrTranscriptForbidden
	}

	order := "ASC"
	where := "sequence > $3"
	after := opts.AfterSequence
	if opts.Tail {
		order = "DESC"
		where = "$3::bigint >= 0"
		after = 0
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, account_id, transcript_id, realm_id, recorded_by_agent_id,
		       sequence, COALESCE(external_id, ''), role, body, payload, COALESCE(model, ''),
		       COALESCE(reply_to_entry_id, ''), artifacts, created_at
		FROM transcript_entries
		WHERE transcript_id = $1 AND account_id = $2 AND `+where+`
		ORDER BY sequence `+order+`, id `+order+`
		LIMIT $4`, transcriptID, p.AccountID, after, opts.Limit+1)
	if err != nil {
		return TranscriptPage{}, fmt.Errorf("page transcript entries: %w", err)
	}
	defer rows.Close()
	entries := make([]TranscriptEntry, 0, opts.Limit+1)
	for rows.Next() {
		var entry TranscriptEntry
		if err := rows.Scan(&entry.ID, &entry.AccountID, &entry.TranscriptID,
			&entry.RealmID, &entry.RecordedByAgentID, &entry.Sequence, &entry.ExternalID,
			&entry.Role, &entry.Body, &entry.Payload, &entry.Model, &entry.ReplyToEntryID,
			&entry.Artifacts, &entry.CreatedAt); err != nil {
			return TranscriptPage{}, err
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return TranscriptPage{}, err
	}
	if opts.Tail {
		if len(entries) > opts.Limit {
			entries = entries[:opts.Limit]
		}
		for left, right := 0, len(entries)-1; left < right; left, right = left+1, right-1 {
			entries[left], entries[right] = entries[right], entries[left]
		}
		return TranscriptPage{Transcript: tr, Entries: entries}, nil
	}

	var next int64
	if len(entries) > opts.Limit {
		entries = entries[:opts.Limit]
		next = entries[len(entries)-1].Sequence
	}
	return TranscriptPage{Transcript: tr, Entries: entries, NextAfterSequence: next}, nil
}

func normalizeTranscriptPageOptions(opts TranscriptPageOptions) (TranscriptPageOptions, error) {
	if opts.AfterSequence < 0 {
		return TranscriptPageOptions{}, fmt.Errorf("%w: after_sequence cannot be negative", ErrTranscriptInputInvalid)
	}
	if opts.Tail && opts.AfterSequence != 0 {
		return TranscriptPageOptions{}, fmt.Errorf("%w: tail and after_sequence are mutually exclusive", ErrTranscriptInputInvalid)
	}
	if opts.Limit == 0 {
		opts.Limit = defaultTranscriptPageSize
	}
	if opts.Limit < 1 || opts.Limit > maxTranscriptPageSize {
		return TranscriptPageOptions{}, fmt.Errorf("%w: limit must be 1-%d", ErrTranscriptInputInvalid, maxTranscriptPageSize)
	}
	return opts, nil
}

func verifyLiveAgentScope(ctx context.Context, tx pgx.Tx, accountID, realmID, agentID string) error {
	var exists bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS (
		  SELECT 1 FROM agents a
		  JOIN realms r ON r.id = a.realm_id
		  WHERE a.id = $1 AND a.realm_id = $2 AND r.account_id = $3
		    AND a.deleted_at IS NULL AND r.deleted_at IS NULL
		)`, agentID, realmID, accountID).Scan(&exists)
	if err != nil {
		return fmt.Errorf("verify transcript agent: %w", err)
	}
	if !exists {
		return ErrAgentNotFound
	}
	return nil
}

func normalizeCreateTranscriptInput(in CreateTranscriptInput) (CreateTranscriptInput, error) {
	in.ExternalID = strings.TrimSpace(in.ExternalID)
	in.Title = strings.TrimSpace(in.Title)
	if len(in.ExternalID) > maxTranscriptExternalIDBytes {
		return CreateTranscriptInput{}, fmt.Errorf("%w: external_id exceeds %d bytes", ErrTranscriptInputInvalid, maxTranscriptExternalIDBytes)
	}
	if len(in.Title) > maxTranscriptTitleBytes {
		return CreateTranscriptInput{}, fmt.Errorf("%w: title exceeds %d bytes", ErrTranscriptInputInvalid, maxTranscriptTitleBytes)
	}
	metadata, err := normalizeJSONObject(in.Metadata, true)
	if err != nil {
		return CreateTranscriptInput{}, fmt.Errorf("%w: metadata %v", ErrTranscriptInputInvalid, err)
	}
	in.Metadata = metadata
	return in, nil
}

func normalizeAppendTranscriptEntryInput(in AppendTranscriptEntryInput) (AppendTranscriptEntryInput, error) {
	in.ExternalID = strings.TrimSpace(in.ExternalID)
	in.Role = strings.TrimSpace(in.Role)
	in.Model = strings.TrimSpace(in.Model)
	in.ReplyToEntryID = strings.TrimSpace(in.ReplyToEntryID)
	in.ReplyToExternalID = strings.TrimSpace(in.ReplyToExternalID)
	switch in.Role {
	case TranscriptRoleUser, TranscriptRoleAssistant, TranscriptRoleSystem, TranscriptRoleTool:
	default:
		return AppendTranscriptEntryInput{}, fmt.Errorf("%w: role must be user, assistant, system, or tool", ErrTranscriptInputInvalid)
	}
	if len(in.Body) > maxTranscriptBodyBytes {
		return AppendTranscriptEntryInput{}, fmt.Errorf("%w: body exceeds %d bytes", ErrTranscriptInputInvalid, maxTranscriptBodyBytes)
	}
	if len(in.Model) > maxTranscriptModelBytes {
		return AppendTranscriptEntryInput{}, fmt.Errorf("%w: model exceeds %d bytes", ErrTranscriptInputInvalid, maxTranscriptModelBytes)
	}
	if len(in.ExternalID) > maxTranscriptExternalIDBytes {
		return AppendTranscriptEntryInput{}, fmt.Errorf("%w: external_id exceeds %d bytes", ErrTranscriptInputInvalid, maxTranscriptExternalIDBytes)
	}
	if len(in.ReplyToExternalID) > maxTranscriptExternalIDBytes {
		return AppendTranscriptEntryInput{}, fmt.Errorf("%w: reply_to_external_id exceeds %d bytes", ErrTranscriptInputInvalid, maxTranscriptExternalIDBytes)
	}
	if in.ReplyToEntryID != "" && in.ReplyToExternalID != "" {
		return AppendTranscriptEntryInput{}, fmt.Errorf("%w: set only one reply target", ErrTranscriptInputInvalid)
	}
	payload, err := normalizeJSONObject(in.Payload, false)
	if err != nil {
		return AppendTranscriptEntryInput{}, fmt.Errorf("%w: payload %v", ErrTranscriptInputInvalid, err)
	}
	in.Payload = payload
	if in.Body == "" && len(in.Payload) == 0 {
		return AppendTranscriptEntryInput{}, fmt.Errorf("%w: body or payload is required", ErrTranscriptInputInvalid)
	}
	artifacts := in.Artifacts
	if len(artifacts) == 0 {
		artifacts = json.RawMessage(`[]`)
	}
	var refs []json.RawMessage
	if len(artifacts) > maxTranscriptJSONBytes || json.Unmarshal(artifacts, &refs) != nil || refs == nil {
		return AppendTranscriptEntryInput{}, fmt.Errorf("%w: artifacts must be a JSON array no larger than %d bytes", ErrTranscriptInputInvalid, maxTranscriptJSONBytes)
	}
	if len(refs) != 0 {
		return AppendTranscriptEntryInput{}, fmt.Errorf("%w: file artifacts require object storage and are not enabled yet", ErrTranscriptInputInvalid)
	}
	in.Artifacts = json.RawMessage(`[]`)
	return in, nil
}

func normalizeJSONObject(raw json.RawMessage, emptyObject bool) (json.RawMessage, error) {
	if len(raw) == 0 {
		if emptyObject {
			return json.RawMessage(`{}`), nil
		}
		return nil, nil
	}
	if len(raw) > maxTranscriptJSONBytes {
		return nil, fmt.Errorf("exceeds %d bytes", maxTranscriptJSONBytes)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, errors.New("must be a JSON object")
	}
	canonical, err := json.Marshal(object)
	if err != nil {
		return nil, err
	}
	return canonical, nil
}
