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

	sources := []export.RowSource{
		&querySource{s: s, table: "accounts", q: `
			SELECT jsonb_build_object(
			  'id', id, 'is_default', is_default, 'display_name', display_name,
			  'email', email, 'status', status, 'created_at', created_at,
			  'closed_at', closed_at, 'closed_reason', closed_reason,
			  'suspended_at', suspended_at, 'suspended_for', suspended_for,
			  'suspended_reason', suspended_reason)
			FROM accounts WHERE id = $1`, arg: accountID},
		&querySource{s: s, table: "operators", q: `
			SELECT jsonb_build_object(
			  'id', id, 'account_id', account_id, 'role', role, 'is_root', is_root,
			  'display_name', display_name, 'created_at', created_at,
			  'updated_at', updated_at, 'deleted_at', deleted_at)
			FROM operators WHERE account_id = $1 ORDER BY id`, arg: accountID},
		&querySource{s: s, table: "tokens", q: `
			SELECT jsonb_build_object(
			  'id', id, 'account_id', account_id, 'operator_id', operator_id,
			  'agent_id', agent_id, 'kind', kind, 'token_hash', token_hash,
			  'display_name', display_name, 'created_at', created_at,
			  'expires_at', expires_at, 'consumed_at', consumed_at)
			FROM tokens WHERE account_id = $1 ORDER BY id`, arg: accountID},
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
