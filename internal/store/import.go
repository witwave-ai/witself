package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/witwave-ai/witself/internal/export"
	"github.com/witwave-ai/witself/internal/placement"
)

// ErrAccountExists is returned when an import targets an account id already
// present on this cell — restore never overwrites.
var ErrAccountExists = errors.New("account already exists on this cell")

// ErrArchiveAccountMismatch is returned when the archive's manifest names a
// different account than the caller asked to import.
var ErrArchiveAccountMismatch = errors.New("archive is for a different account")

// ErrArchiveContent is returned for archives that are structurally well-formed
// (gzip/tar/checksums all check out) but whose row payload violates the
// import contract: a row scoped to a different account, an off-list column,
// an agent pointing at a realm that never arrived, extra accounts rows.
// The condition is permanent for a given archive object, so it maps to a
// 400 (server.ErrBadArchive) — the caller should quarantine the archive,
// not retry it.
var ErrArchiveContent = errors.New("archive content is not importable")

// importColumns is the strict per-table allowlist of column names an archive
// may carry. It doubles as the SQL-identifier boundary — the row's JSON keys
// are looked up here and only allowlisted names are interpolated into the
// INSERT — and as the additive-migration safety net: unlisted (new) columns
// are refused rather than smuggled in with attacker-chosen values.
var importColumns = map[string]map[string]bool{
	"accounts": {
		"id": true, "is_default": true, "display_name": true, "email": true,
		"status": true, "created_at": true, "closed_at": true, "closed_reason": true,
		"suspended_at": true, "suspended_for": true, "suspended_reason": true,
		"support_policy": true,
		"plan":           true, "plan_limits": true, "plan_features": true,
		"placement_policy": true,
	},
	"operators": {
		"id": true, "account_id": true, "role": true, "is_root": true,
		"display_name": true, "created_at": true, "updated_at": true, "deleted_at": true,
	},
	"realms": {
		"id": true, "account_id": true, "name": true,
		"created_at": true, "updated_at": true, "deleted_at": true,
	},
	"agents": {
		"id": true, "realm_id": true, "name": true,
		"created_at": true, "updated_at": true, "deleted_at": true,
	},
	"tokens": {
		"id": true, "account_id": true, "operator_id": true, "agent_id": true,
		"kind": true, "token_hash": true, "display_name": true,
		"created_at": true, "expires_at": true, "consumed_at": true,
	},
	"account_events": {
		"id": true, "account_id": true, "occurred_at": true,
		"actor_kind": true, "actor_id": true,
		"verb": true, "metadata": true, "retain_until": true,
	},
	"support_tickets": {
		"id": true, "account_id": true, "opened_at": true,
		"opened_by_kind": true, "opened_by_id": true,
		"subject": true, "category": true, "state": true,
		"priority": true, "first_response_at": true, "resolved_at": true,
		"closed_at": true, "last_activity_at": true, "last_message_id": true,
		"correlation": true, "metadata": true, "retain_until": true,
	},
	"support_ticket_messages": {
		"id": true, "ticket_id": true, "account_id": true, "posted_at": true,
		"author_kind": true, "author_id": true,
		"body": true, "attachments": true, "metadata": true,
	},
}

// importCtx accumulates per-import state: how many accounts rows we have seen
// (must be exactly one), and the set of ids inserted for each table. The FK
// targets an incoming row references (agents.realm_id, tokens.operator_id,
// tokens.agent_id) must have already been inserted by THIS import — the FK
// constraint alone would accept a target belonging to any tenant on the cell.
type importCtx struct {
	accountID string
	accounts  int
	operators map[string]bool
	realms    map[string]bool
	agents    map[string]bool
	tickets   map[string]bool
}

func newImportCtx(accountID string) *importCtx {
	return &importCtx{
		accountID: accountID,
		operators: map[string]bool{},
		realms:    map[string]bool{},
		agents:    map[string]bool{},
		tickets:   map[string]bool{},
	}
}

// validateAndRecord is the row-content boundary: it enforces the account
// scoping the FKs alone cannot, and records the id (if any) for later FK
// targets. Called BEFORE the INSERT so a bad row aborts the transaction
// without touching the database.
func (ic *importCtx) validateAndRecord(table string, obj map[string]any) error {
	badf := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrArchiveContent, fmt.Sprintf(format, args...))
	}
	// Tables that carry account_id must have it equal the manifest's.
	// Agents scope transitively: their realm_id lands in ic.realms only
	// for realms this archive itself just wrote, so the realm_id check
	// below is the FK-safety boundary for that table.
	switch table {
	case "operators", "realms", "tokens", "account_events",
		"support_tickets", "support_ticket_messages":
		id, err := requireStringField(obj, "account_id")
		if err != nil {
			return badf("%s row missing account_id", table)
		}
		if id != ic.accountID {
			return badf("%s row account_id %q does not match manifest %q", table, id, ic.accountID)
		}
	}
	switch table {
	case "accounts":
		id, err := requireStringField(obj, "id")
		if err != nil || id != ic.accountID {
			return badf("accounts row id %q does not match manifest %q", id, ic.accountID)
		}
		if v, ok := obj["is_default"]; ok {
			if b, _ := v.(bool); b {
				return badf("accounts row claims is_default=true")
			}
		}
		// Plan-snapshot shape checks: these jsonb columns are decoded into
		// typed Go values on every read (map[string]int64 / []string), so a
		// malformed value would import fine and then poison the account —
		// every GetAccount and every gated create fails until the control
		// plane re-applies a snapshot. Content-hostile streams must land
		// nothing, so refuse the shapes here (absent keys are fine: archives
		// from before migration 0017 fall back to the column defaults).
		if v, present := obj["plan"]; present {
			if _, ok := v.(string); !ok {
				return badf("accounts row plan must be a string")
			}
		}
		if v, present := obj["plan_limits"]; present {
			m, ok := v.(map[string]any)
			if !ok {
				return badf("accounts row plan_limits must be an object of integer limits")
			}
			for key, raw := range m {
				f, ok := raw.(float64)
				if !ok || f != math.Trunc(f) {
					return badf("accounts row plan_limits[%q] must be an integer", key)
				}
			}
		}
		if v, present := obj["plan_features"]; present {
			fs, ok := v.([]any)
			if !ok {
				return badf("accounts row plan_features must be an array of strings")
			}
			for _, raw := range fs {
				if _, ok := raw.(string); !ok {
					return badf("accounts row plan_features entries must be strings")
				}
			}
		}
		if v, present := obj["placement_policy"]; present {
			if _, err := placement.FromAny(v); err != nil {
				return badf("accounts row %v", err)
			}
		}
		ic.accounts++
		if ic.accounts > 1 {
			return badf("archive contains more than one accounts row")
		}
	case "operators":
		if id, ok := stringField(obj, "id"); ok {
			ic.operators[id] = true
		}
	case "realms":
		if id, ok := stringField(obj, "id"); ok {
			ic.realms[id] = true
		}
	case "agents":
		realmID, err := requireStringField(obj, "realm_id")
		if err != nil {
			return badf("agents row missing realm_id")
		}
		if !ic.realms[realmID] {
			return badf("agents row references realm %q not present in this archive", realmID)
		}
		if id, ok := stringField(obj, "id"); ok {
			ic.agents[id] = true
		}
	case "tokens":
		if opID, present := optionalStringField(obj, "operator_id"); present && !ic.operators[opID] {
			return badf("tokens row references operator %q not present in this archive", opID)
		}
		if agID, present := optionalStringField(obj, "agent_id"); present && !ic.agents[agID] {
			return badf("tokens row references agent %q not present in this archive", agID)
		}
	case "account_events":
		// The account_id scoping check already ran in the first switch;
		// no downstream table references account_events, so nothing to
		// record here. Metadata is opaque JSONB — the write-time verb
		// contract was enforced when the event was created and doesn't
		// need to be re-enforced at import time (an old cell may have
		// written events under a schema this cell no longer knows).
	case "support_tickets":
		// Record the ticket id so incoming support_ticket_messages
		// rows can be FK-validated against this-archive tickets only.
		if id, ok := stringField(obj, "id"); ok {
			ic.tickets[id] = true
		}
	case "support_ticket_messages":
		// FK-scope check: the ticket_id must belong to a ticket this
		// same archive already inserted. Cross-tenant grafting is
		// blocked the same way agents.realm_id is checked against
		// ic.realms.
		ticketID, err := requireStringField(obj, "ticket_id")
		if err != nil {
			return badf("support_ticket_messages row missing ticket_id")
		}
		if !ic.tickets[ticketID] {
			return badf("support_ticket_messages row references ticket %q not present in this archive", ticketID)
		}
	default:
		return badf("table %q is not importable", table)
	}
	return nil
}

// requireStringField reads a JSON string field; treats JSON null / missing / wrong-type as absent.
func requireStringField(obj map[string]any, key string) (string, error) {
	s, ok := stringField(obj, key)
	if !ok {
		return "", fmt.Errorf("required %s absent", key)
	}
	return s, nil
}

func stringField(obj map[string]any, key string) (string, bool) {
	v, present := obj[key]
	if !present || v == nil {
		return "", false
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return "", false
	}
	return s, true
}

// optionalStringField distinguishes "the field is a non-empty string" from
// "the field is absent or JSON null" (both legal — FKs are nullable). A
// present-but-non-string value is treated as absent, since it can't be a
// valid FK target anyway; the subsequent INSERT will fail its type coercion.
func optionalStringField(obj map[string]any, key string) (string, bool) {
	return stringField(obj, key)
}

// ImportAccount restores one account's logical archive from r into this cell.
// The entire restore is a single transaction committed only after the
// archive's trailing checksums verify AND every row's account/FK scoping
// checks pass, so a truncated, tampered, or content-hostile stream lands
// nothing. The account arrives in its exported state — suspended (or a
// closed tombstone); resuming is the caller's separate, explicit step.
//
// expectedAccountID pins the archive to the account the caller believes it
// is restoring; a manifest naming anyone else refuses before rows stream.
func (s *Store) ImportAccount(ctx context.Context, expectedAccountID string, r io.Reader) (export.Manifest, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return export.Manifest{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	ic := newImportCtx(expectedAccountID)

	m, err := export.Read(ctx, r, export.ImportOptions{
		CurrentSchema: SchemaVersion(),
		OnManifest: func(m export.Manifest) error {
			if m.AccountID == "" || m.AccountID != expectedAccountID {
				return fmt.Errorf("%w: archive is for %q", ErrArchiveAccountMismatch, m.AccountID)
			}
			if m.Status != "suspended" && m.Status != "closed" {
				return fmt.Errorf("%w: manifest status %q — exports are only taken frozen", ErrArchiveContent, m.Status)
			}
			var exists bool
			if err := tx.QueryRow(ctx,
				`SELECT EXISTS(SELECT 1 FROM accounts WHERE id = $1)`,
				m.AccountID).Scan(&exists); err != nil {
				return fmt.Errorf("check import target: %w", err)
			}
			if exists {
				return ErrAccountExists
			}
			return nil
		},
		Row: func(table string, row []byte) error {
			if _, ok := importColumns[table]; !ok {
				return fmt.Errorf("%w: table %q not importable", ErrArchiveContent, table)
			}
			var obj map[string]any
			if err := json.Unmarshal(row, &obj); err != nil {
				return fmt.Errorf("%w: %s row is not JSON: %v", ErrArchiveContent, table, err)
			}
			if err := ic.validateAndRecord(table, obj); err != nil {
				return err
			}
			return insertProjected(ctx, tx, table, obj, row)
		},
	})
	if err != nil {
		return export.Manifest{}, err
	}

	// The archive's own account row must have actually landed, and must not
	// claim the deployment's default seat. These are all permanent
	// archive-content defects (missing accounts row, is_default lie,
	// status mismatch), so they wrap ErrArchiveContent and surface as
	// 400 — retrying against the same object cannot recover.
	var isDefault bool
	var status string
	err = tx.QueryRow(ctx,
		`SELECT is_default, status FROM accounts WHERE id = $1`,
		m.AccountID).Scan(&isDefault, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return export.Manifest{}, fmt.Errorf("%w: no accounts row landed for %s", ErrArchiveContent, m.AccountID)
	}
	if err != nil {
		return export.Manifest{}, fmt.Errorf("verify landed account: %w", err)
	}
	if isDefault {
		return export.Manifest{}, fmt.Errorf("%w: landed row claims the default seat", ErrArchiveContent)
	}
	if status != m.Status {
		return export.Manifest{}, fmt.Errorf("%w: landed account row status %q disagrees with manifest %q", ErrArchiveContent, status, m.Status)
	}

	if err := tx.Commit(ctx); err != nil {
		return export.Manifest{}, err
	}
	return m, nil
}

// insertProjected inserts one row using ONLY the columns the archive
// actually carries, so columns the archive omits take their destination
// DEFAULT — the additive-migration contract in export/upgrade.go. The set
// of legal column names per table is fixed at compile time (importColumns);
// any JSON key outside it is refused, so no attacker-chosen identifier
// reaches the SQL text.
//
// Concurrent same-account imports collide on the accounts primary-key
// insert: the loser's INSERT blocks on the winner's uncommitted tuple, and
// on the winner's commit fails with unique_violation (23505). That maps to
// ErrAccountExists here — a clean 409 for the retry — instead of falling to
// the generic 500 arm.
func insertProjected(ctx context.Context, tx pgxExec, table string, obj map[string]any, raw []byte) error {
	allowed := importColumns[table]
	keys := make([]string, 0, len(obj))
	for k := range obj {
		if !allowed[k] {
			return fmt.Errorf("%w: %s row has unknown column %q", ErrArchiveContent, table, k)
		}
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic SQL text, useful for logs
	if len(keys) == 0 {
		return fmt.Errorf("%w: empty %s row", ErrArchiveContent, table)
	}
	colList := strings.Join(keys, ", ")
	// INSERT INTO t (c1,c2) SELECT c1,c2 FROM jsonb_populate_record(NULL::t, $1)
	// projects only the columns present in the JSON. Unlisted columns take
	// their DEFAULT — new NOT NULL DEFAULT columns land correctly without
	// needing an upgrader; new nullable-with-default columns land at their
	// default instead of the silent NULL a full-record insert would leave.
	stmt := fmt.Sprintf(
		`INSERT INTO %s (%s) SELECT %s FROM jsonb_populate_record(NULL::%s, $1::jsonb)`,
		table, colList, colList, table)
	if _, err := tx.Exec(ctx, stmt, raw); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && table == "accounts" {
			// A concurrent import (or a retry racing the winner) landed
			// this account first. Surface it as the same 409 an early
			// EXISTS-check would give.
			return ErrAccountExists
		}
		return fmt.Errorf("import %s row: %w", table, err)
	}
	return nil
}

// pgxExec is the minimal Exec surface insertProjected needs from a pgx.Tx —
// declared here so the helper can be unit-tested with an in-memory fake
// without pulling in a live database.
type pgxExec interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}
