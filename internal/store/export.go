package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/witwave-ai/witself/internal/export"
)

// ErrAccountNotExportable is returned when an export is attempted against an
// account that is neither suspended nor closed. Exports require the write
// freeze for consistency; closed tombstones are exportable because they must
// survive their cell's decommissioning (accounts live forever).
var ErrAccountNotExportable = errors.New("account must be suspended (or closed) to export")

// SchemaVersion is the highest embedded migration number — the schema
// coordinate written into every archive manifest.
func SchemaVersion() int {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return 0
	}
	highest := 0
	for _, e := range entries {
		name := e.Name()
		if i := strings.IndexByte(name, '_'); i > 0 {
			if n, err := strconv.Atoi(name[:i]); err == nil && n > highest {
				highest = n
			}
		}
	}
	return highest
}

// ExportAccount streams the account's complete logical archive to w. The
// account must be suspended or closed (the write freeze is what makes the
// snapshot consistent). Row order inside tables is stable (primary key) so
// repeated exports of a frozen account are byte-identical.
func (s *Store) ExportAccount(ctx context.Context, accountID, cellName, serverVersion string, w io.Writer) error {
	var status string
	err := s.pool.QueryRow(ctx,
		`SELECT status FROM accounts WHERE id = $1`, accountID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrAccountNotFound
	}
	if err != nil {
		return fmt.Errorf("verify export target: %w", err)
	}
	if status != "suspended" && status != "closed" {
		return ErrAccountNotExportable
	}

	// Tables stream in FOREIGN-KEY DEPENDENCY ORDER (tokens reference
	// operators and agents; agents reference realms) so a streaming importer
	// can insert every row the moment it arrives, no buffering, no deferred
	// constraints.
	sources := []export.RowSource{
		&querySource{s: s, table: "accounts", q: `
			SELECT jsonb_build_object(
			  'id', id, 'is_default', is_default, 'display_name', display_name,
			  'email', email, 'status', status, 'created_at', created_at,
			  'closed_at', closed_at, 'closed_reason', closed_reason,
			  'suspended_at', suspended_at, 'suspended_for', suspended_for,
			  'suspended_reason', suspended_reason,
			  'support_policy', support_policy,
			  'plan', plan, 'plan_limits', plan_limits,
			  'plan_features', plan_features,
			  'placement_policy', placement_policy)
			FROM accounts WHERE id = $1`, arg: accountID},
		&querySource{s: s, table: "operators", q: `
			SELECT jsonb_build_object(
			  'id', id, 'account_id', account_id, 'role', role, 'is_root', is_root,
			  'display_name', display_name, 'created_at', created_at,
			  'updated_at', updated_at, 'deleted_at', deleted_at)
			FROM operators WHERE account_id = $1 ORDER BY id`, arg: accountID},
		&querySource{s: s, table: "realms", q: `
			SELECT jsonb_build_object(
			  'id', id, 'account_id', account_id, 'name', name,
			  'created_at', created_at, 'updated_at', updated_at,
			  'deleted_at', deleted_at)
			FROM realms WHERE account_id = $1 ORDER BY id`, arg: accountID},
		&querySource{s: s, table: "agents", q: `
			SELECT jsonb_build_object(
			  'id', a.id, 'realm_id', a.realm_id, 'name', a.name,
			  'created_at', a.created_at, 'updated_at', a.updated_at,
			  'deleted_at', a.deleted_at)
			FROM agents a JOIN realms r ON r.id = a.realm_id
			WHERE r.account_id = $1 ORDER BY a.id`, arg: accountID},
		&querySource{s: s, table: "fact_subjects", q: `
			SELECT jsonb_build_object(
			  'id', id, 'account_id', account_id, 'realm_id', realm_id,
			  'owner_agent_id', owner_agent_id, 'canonical_key', canonical_key,
			  'display_name', display_name, 'aliases', aliases,
			  'created_at', created_at, 'updated_at', updated_at)
			FROM fact_subjects WHERE account_id = $1 ORDER BY id`, arg: accountID},
		&querySource{s: s, table: "facts", q: `
			SELECT jsonb_build_object(
			  'id', id, 'account_id', account_id, 'realm_id', realm_id,
			  'owner_agent_id', owner_agent_id, 'subject_id', subject_id,
			  'predicate', predicate, 'cardinality', cardinality,
			  'sensitive', sensitive, 'resolved_assertion_id', resolved_assertion_id,
			  'created_at', created_at, 'updated_at', updated_at)
			FROM facts WHERE account_id = $1 ORDER BY id`, arg: accountID},
		&querySource{s: s, table: "fact_assertions", q: `
			WITH RECURSIVE assertion_order AS (
			  SELECT a.*, 0 AS chain_depth
			  FROM fact_assertions a
			  WHERE a.account_id = $1 AND a.supersedes_id IS NULL
			  UNION ALL
			  SELECT child.*, parent.chain_depth + 1
			  FROM fact_assertions child
			  JOIN assertion_order parent ON child.supersedes_id = parent.id
			  WHERE child.account_id = $1
			)
			SELECT jsonb_build_object(
			  'id', id, 'fact_id', fact_id, 'account_id', account_id,
			  'realm_id', realm_id, 'asserted_by_agent_id', asserted_by_agent_id,
			  'value_type', value_type, 'value', value,
			  'recurrence', recurrence,
			  'source_kind', source_kind, 'source_ref', source_ref,
			  'confidence', confidence, 'observed_at', observed_at,
			  'confirmed_at', confirmed_at, 'valid_from', valid_from,
			  'valid_until', valid_until, 'supersedes_id', supersedes_id,
			  'created_at', created_at)
			FROM assertion_order
			ORDER BY fact_id, chain_depth, created_at, id`, arg: accountID},
		&querySource{s: s, table: "fact_candidates", q: `
			SELECT jsonb_build_object(
			  'id', id, 'account_id', account_id, 'realm_id', realm_id,
			  'owner_agent_id', owner_agent_id, 'subject_key', subject_key,
			  'predicate', predicate, 'value_type', value_type, 'value', value,
			  'recurrence', recurrence,
			  'cardinality', cardinality, 'sensitive', sensitive,
			  'source_ref', source_ref, 'confidence', confidence,
			  'observed_at', observed_at, 'valid_from', valid_from,
			  'valid_until', valid_until,
			  'reason', reason, 'status', status,
			  'conflict_fact_id', conflict_fact_id,
			  'observed_assertion_id', observed_assertion_id,
			  'resolved_fact_id', resolved_fact_id,
			  'proposed_at', proposed_at, 'decided_at', decided_at)
			FROM fact_candidates WHERE account_id = $1
			ORDER BY proposed_at, id`, arg: accountID},
		&querySource{s: s, table: "tokens", q: `
			SELECT jsonb_build_object(
			  'id', id, 'account_id', account_id, 'operator_id', operator_id,
			  'agent_id', agent_id, 'kind', kind, 'token_hash', token_hash,
			  'display_name', display_name, 'created_at', created_at,
			  'expires_at', expires_at, 'consumed_at', consumed_at)
			FROM tokens WHERE account_id = $1 ORDER BY id`, arg: accountID},
		// Transcript conversations depend on realms + agents; entries depend
		// on their conversation and may reply only to an earlier entry. Stable
		// sequence order therefore makes this stream directly insertable.
		&querySource{s: s, table: "transcript_conversations", q: `
			SELECT jsonb_build_object(
			  'id', id, 'account_id', account_id, 'realm_id', realm_id,
			  'owner_agent_id', owner_agent_id, 'external_id', external_id,
			  'title', title, 'metadata', metadata,
			  'next_sequence', next_sequence,
			  'created_at', created_at, 'updated_at', updated_at)
			FROM transcript_conversations WHERE account_id = $1
			ORDER BY created_at, id`, arg: accountID},
		&querySource{s: s, table: "transcript_entries", q: `
			SELECT jsonb_build_object(
			  'id', id, 'account_id', account_id,
			  'transcript_id', transcript_id, 'realm_id', realm_id,
			  'recorded_by_agent_id', recorded_by_agent_id,
			  'sequence', sequence, 'external_id', external_id,
			  'role', role, 'body', body,
			  'payload', payload, 'model', model,
			  'reply_to_entry_id', reply_to_entry_id,
			  'artifacts', artifacts, 'created_at', created_at)
			FROM transcript_entries WHERE account_id = $1
			ORDER BY transcript_id, sequence, id`, arg: accountID},
		// Usage facts and their fast projections are account-owned data. Both
		// are preserved so a moved account keeps exact history and can serve
		// usage immediately without rebuilding rollups during import.
		&querySource{s: s, table: "usage_events", q: `
			SELECT jsonb_build_object(
			  'id', id, 'account_id', account_id, 'realm_id', realm_id,
			  'agent_id', agent_id, 'dimension', dimension,
			  'quantity', quantity, 'unit', unit,
			  'subject_type', subject_type, 'subject_id', subject_id,
			  'idempotency_key', idempotency_key, 'metadata', metadata,
			  'occurred_at', occurred_at, 'created_at', created_at)
			FROM usage_events WHERE account_id = $1
			ORDER BY occurred_at, id`, arg: accountID},
		&querySource{s: s, table: "usage_rollups", q: `
			SELECT jsonb_build_object(
			  'account_id', account_id, 'realm_id', realm_id,
			  'agent_id', agent_id, 'dimension', dimension, 'unit', unit,
			  'bucket', bucket, 'bucket_start', bucket_start,
			  'quantity', quantity, 'event_count', event_count,
			  'updated_at', updated_at)
			FROM usage_rollups WHERE account_id = $1
			ORDER BY bucket, bucket_start, agent_id, dimension, unit`, arg: accountID},
		// Messages depend on realms + agents; recipient delivery state depends
		// on its message. Preserve bodies here because the account archive is
		// the durable, encrypted migration unit for all account-owned data.
		&querySource{s: s, table: "agent_messages", q: `
			SELECT jsonb_build_object(
			  'id', id, 'account_id', account_id, 'realm_id', realm_id,
			  'from_agent_id', from_agent_id, 'to_agent_id', to_agent_id,
			  'subject', subject, 'kind', kind, 'body', body,
			  'payload', payload, 'thread_id', thread_id,
			  'idempotency_key', idempotency_key, 'created_at', created_at)
			FROM agent_messages WHERE account_id = $1
			ORDER BY created_at, id`, arg: accountID},
		&querySource{s: s, table: "agent_message_deliveries", q: `
			SELECT jsonb_build_object(
			  'message_id', message_id, 'account_id', account_id,
			  'realm_id', realm_id, 'recipient_agent_id', recipient_agent_id,
			  'state', state, 'delivered_at', delivered_at,
			  'read_at', read_at, 'acked_at', acked_at,
			  'created_at', created_at)
			FROM agent_message_deliveries WHERE account_id = $1
			ORDER BY created_at, message_id, recipient_agent_id`, arg: accountID},
		// account_events streams last because it has no outbound FKs
		// beyond account_id, and it is the append-only ledger — its rows
		// point AT the state changes recorded above, not the other way
		// around, so ordering it here keeps the restore side inserting
		// in the natural read order.
		&querySource{s: s, table: "account_events", q: `
			SELECT jsonb_build_object(
			  'id', id, 'account_id', account_id, 'occurred_at', occurred_at,
			  'actor_kind', actor_kind, 'actor_id', actor_id,
			  'verb', verb, 'metadata', metadata, 'retain_until', retain_until)
			FROM account_events WHERE account_id = $1
			ORDER BY occurred_at, id`, arg: accountID},
		// support_tickets + messages stream after account_events because
		// messages FK-depend on tickets AND on accounts; the importCtx
		// FK-validation reads ic.tickets which the tickets query
		// populates. Both queries emit every column of the base
		// migration so the round-trip preserves the shape exactly.
		&querySource{s: s, table: "support_tickets", q: `
			SELECT jsonb_build_object(
			  'id', id, 'account_id', account_id, 'opened_at', opened_at,
			  'opened_by_kind', opened_by_kind, 'opened_by_id', opened_by_id,
			  'subject', subject, 'category', category, 'state', state,
			  'priority', priority, 'first_response_at', first_response_at,
			  'resolved_at', resolved_at, 'closed_at', closed_at,
			  'last_activity_at', last_activity_at, 'last_message_id', last_message_id,
			  'correlation', correlation, 'metadata', metadata,
			  'retain_until', retain_until)
			FROM support_tickets WHERE account_id = $1
			ORDER BY opened_at, id`, arg: accountID},
		&querySource{s: s, table: "support_ticket_messages", q: `
			SELECT jsonb_build_object(
			  'id', id, 'ticket_id', ticket_id, 'account_id', account_id,
			  'posted_at', posted_at,
			  'author_kind', author_kind, 'author_id', author_id,
			  'body', body, 'attachments', attachments, 'metadata', metadata)
			FROM support_ticket_messages WHERE account_id = $1
			ORDER BY posted_at, id`, arg: accountID},
	}

	m := export.Manifest{
		SchemaVersion: SchemaVersion(),
		ServerVersion: serverVersion,
		AccountID:     accountID,
		Cell:          cellName,
		Status:        status,
		ExportedAt:    time.Now().UTC(),
	}
	return export.Write(ctx, w, m, sources)
}

// querySource streams one table's rows as JSON objects built by Postgres
// itself (jsonb_build_object), so field names are explicit and stable — the
// logical-format contract — and rows never pass through Go structs that
// could silently drop columns.
type querySource struct {
	s     *Store
	table string
	q     string
	arg   string

	rows pgx.Rows
	done bool
}

func (qs *querySource) Table() string { return qs.table }

func (qs *querySource) Next(ctx context.Context) ([]byte, error) {
	if qs.done {
		return nil, nil
	}
	if qs.rows == nil {
		rows, err := qs.s.pool.Query(ctx, qs.q, qs.arg)
		if err != nil {
			return nil, err
		}
		qs.rows = rows
	}
	if !qs.rows.Next() {
		qs.done = true
		err := qs.rows.Err()
		qs.rows.Close()
		return nil, err
	}
	var raw json.RawMessage
	if err := qs.rows.Scan(&raw); err != nil {
		qs.rows.Close()
		qs.done = true
		return nil, err
	}
	// jsonb text output is already a single line — NDJSON-safe as-is.
	return raw, nil
}
