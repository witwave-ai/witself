package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// TestValidateAndRecordEnforcesAccountScoping pins the row-content boundary:
// the FK constraints alone would happily accept operators, realms, tokens,
// or agents pointing at ANOTHER tenant on the cell, so this validator is
// the sole gate against a tampered R2 archive grafting rows onto a victim
// account. Each case is a concrete tamper pattern.
func TestValidateAndRecordEnforcesAccountScoping(t *testing.T) {
	const acc = "acc_target"
	tests := []struct {
		name   string
		table  string
		row    map[string]any
		setup  func(*importCtx)
		wantOK bool
		want   string // error substring; ignored when wantOK
	}{
		{
			name:   "accounts row must equal manifest id",
			table:  "accounts",
			row:    map[string]any{"id": "acc_someone_else", "status": "suspended"},
			wantOK: false, want: "does not match manifest",
		},
		{
			name:   "accounts row cannot claim is_default",
			table:  "accounts",
			row:    map[string]any{"id": acc, "is_default": true, "status": "suspended"},
			wantOK: false, want: "is_default=true",
		},
		{
			name:   "second accounts row is refused",
			table:  "accounts",
			row:    map[string]any{"id": acc, "status": "suspended"},
			setup:  func(ic *importCtx) { ic.accounts = 1 },
			wantOK: false, want: "more than one accounts row",
		},
		{
			name:   "operators row missing account_id is refused",
			table:  "operators",
			row:    map[string]any{"id": "op_1", "role": "account_owner"},
			wantOK: false, want: "missing account_id",
		},
		{
			name:   "operators row for the wrong account is refused",
			table:  "operators",
			row:    map[string]any{"id": "op_1", "account_id": "acc_victim"},
			wantOK: false, want: "does not match manifest",
		},
		{
			name:   "tokens row grafting onto a victim account is refused",
			table:  "tokens",
			row:    map[string]any{"id": "tok_1", "account_id": "acc_victim", "operator_id": "op_v"},
			wantOK: false, want: "does not match manifest",
		},
		{
			name:  "tokens row referencing an operator not in this archive is refused",
			table: "tokens",
			row: map[string]any{
				"id": "tok_1", "account_id": acc, "operator_id": "op_stranger",
			},
			wantOK: false, want: "not present in this archive",
		},
		{
			name:  "tokens row with only agent_id set, and that agent not in the archive, is refused",
			table: "tokens",
			row: map[string]any{
				"id": "tok_1", "account_id": acc, "agent_id": "agt_stranger",
			},
			wantOK: false, want: "not present in this archive",
		},
		{
			name:  "agents row with a foreign realm_id is refused",
			table: "agents",
			row: map[string]any{
				"id": "agt_1", "realm_id": "rlm_victim", "name": "archivist",
			},
			wantOK: false, want: "not present in this archive",
		},
		{
			name:  "agents row with a realm that arrived earlier in the archive is accepted",
			table: "agents",
			row: map[string]any{
				"id": "agt_1", "realm_id": "rlm_ok", "name": "archivist",
			},
			setup:  func(ic *importCtx) { ic.realms["rlm_ok"] = true },
			wantOK: true,
		},
		{
			name:  "tokens row referencing operator + agent that arrived earlier is accepted",
			table: "tokens",
			row: map[string]any{
				"id": "tok_1", "account_id": acc,
				"operator_id": "op_ok", "agent_id": "agt_ok",
				"kind": "agent",
			},
			setup: func(ic *importCtx) {
				ic.operators["op_ok"] = true
				ic.agents["agt_ok"] = true
			},
			wantOK: true,
		},
		{
			name:  "tokens row with JSON null for the unused FK slot is accepted",
			table: "tokens",
			row: map[string]any{
				"id": "tok_bootstrap", "account_id": acc,
				"operator_id": nil, "agent_id": nil,
				"kind": "bootstrap",
			},
			wantOK: true,
		},
		{
			name:   "unknown table is refused",
			table:  "audit_log",
			row:    map[string]any{"id": "audit_1", "account_id": acc},
			wantOK: false, want: "not importable",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ic := newImportCtx(acc)
			if tc.setup != nil {
				tc.setup(ic)
			}
			err := ic.validateAndRecord(tc.table, tc.row)
			if tc.wantOK {
				if err != nil {
					t.Fatalf("wantOK, got err = %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("wanted error containing %q, got nil", tc.want)
			}
			if !errors.Is(err, ErrArchiveContent) {
				t.Errorf("error not wrapped in ErrArchiveContent: %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

// TestValidateAndRecordAccumulatesOverAStream ties the guarantees together:
// the ORDER the exporter writes tables (accounts, operators, realms,
// agents, tokens) matches the order rows must be recorded so later rows can
// reference earlier ids. The test walks a legal archive stream by feeding
// its rows to the validator in that order.
func TestValidateAndRecordAccumulatesOverAStream(t *testing.T) {
	const acc = "acc_stream"
	ic := newImportCtx(acc)

	feed := func(table string, row map[string]any) {
		t.Helper()
		if err := ic.validateAndRecord(table, row); err != nil {
			t.Fatalf("%s row failed: %v", table, err)
		}
	}
	feed("accounts", map[string]any{"id": acc, "status": "suspended", "is_default": false})
	feed("operators", map[string]any{"id": "op_root", "account_id": acc, "role": "account_owner"})
	feed("realms", map[string]any{"id": "rlm_default", "account_id": acc, "name": "default"})
	feed("agents", map[string]any{"id": "agt_1", "realm_id": "rlm_default", "name": "archivist"})
	feed("tokens", map[string]any{
		"id": "tok_op", "account_id": acc, "operator_id": "op_root", "kind": "operator",
	})
	feed("tokens", map[string]any{
		"id": "tok_agent", "account_id": acc, "agent_id": "agt_1", "kind": "agent",
	})

	if ic.accounts != 1 {
		t.Errorf("accounts count = %d, want 1", ic.accounts)
	}
	if !ic.operators["op_root"] || !ic.realms["rlm_default"] || !ic.agents["agt_1"] {
		t.Error("ids not recorded across a legal stream")
	}
}

// TestInsertProjectedRejectsUnlistedColumn — the column allowlist doubles as
// the SQL-identifier boundary. A JSON key outside the per-table set must
// refuse before any SQL runs.
func TestInsertProjectedRejectsUnlistedColumn(t *testing.T) {
	fake := &recordingExec{}
	err := insertProjected(context.Background(), fake, "accounts",
		map[string]any{"id": "acc_x", "status": "suspended", "; DROP TABLE": "gotcha"},
		[]byte(`{"id":"acc_x"}`))
	if !errors.Is(err, ErrArchiveContent) {
		t.Errorf("error not ErrArchiveContent: %v", err)
	}
	if fake.calls != 0 {
		t.Errorf("SQL executed despite unlisted column: %d calls", fake.calls)
	}
}

// TestInsertProjectedProjectsOnlyRowKeys — the whole point of the projected
// insert is that unlisted columns take their DEFAULT instead of an explicit
// NULL from jsonb_populate_record. Confirm by inspecting the generated SQL:
// only the JSON's keys appear.
func TestInsertProjectedProjectsOnlyRowKeys(t *testing.T) {
	fake := &recordingExec{}
	err := insertProjected(context.Background(), fake, "accounts",
		map[string]any{"id": "acc_x", "status": "suspended"},
		[]byte(`{"id":"acc_x","status":"suspended"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.calls != 1 {
		t.Fatalf("calls = %d, want 1", fake.calls)
	}
	got := fake.lastSQL
	// Column list appears in the two-column projected form. Every listed
	// column MUST be in the row's JSON — no other column may appear.
	for _, col := range []string{"display_name", "email", "created_at", "is_default"} {
		if strings.Contains(got, col) {
			t.Errorf("SQL mentions column %q the row did not carry — DEFAULT will be bypassed: %s", col, got)
		}
	}
	if !strings.Contains(got, "id, status") || !strings.Contains(got, "SELECT id, status") {
		t.Errorf("SQL does not project only (id, status): %s", got)
	}
}

// TestInsertProjectedMapsUniqueViolationOnAccounts — the concurrent-import
// race path: a 23505 on the accounts insert must surface as ErrAccountExists,
// which the server maps to 409, not the generic 500.
func TestInsertProjectedMapsUniqueViolationOnAccounts(t *testing.T) {
	fake := &recordingExec{err: &pgconn.PgError{Code: "23505", ConstraintName: "accounts_pkey"}}
	err := insertProjected(context.Background(), fake, "accounts",
		map[string]any{"id": "acc_x", "status": "suspended"},
		[]byte(`{"id":"acc_x"}`))
	if !errors.Is(err, ErrAccountExists) {
		t.Errorf("23505 on accounts insert = %v, want ErrAccountExists", err)
	}
}

// TestInsertProjectedDoesNotMapUniqueViolationOnOtherTables — 23505 on
// tokens (a token_hash collision) is a legitimate archive-vs-cell conflict
// that should NOT masquerade as "account already exists"; it should bubble
// up as a generic error the caller can log verbatim.
func TestInsertProjectedDoesNotMapUniqueViolationOnOtherTables(t *testing.T) {
	fake := &recordingExec{err: &pgconn.PgError{Code: "23505", ConstraintName: "tokens_token_hash_key"}}
	err := insertProjected(context.Background(), fake, "tokens",
		map[string]any{"id": "tok_x", "account_id": "acc_x", "token_hash": "abcd"},
		[]byte(`{"id":"tok_x"}`))
	if errors.Is(err, ErrAccountExists) {
		t.Error("tokens 23505 masqueraded as ErrAccountExists — mapping is too broad")
	}
	if err == nil {
		t.Error("expected the pg error to bubble up")
	}
}

// recordingExec is a pgxExec that records what SQL was sent (and optionally
// fails with a caller-supplied error), letting the tests inspect the
// generated INSERT text without a live database.
type recordingExec struct {
	calls   int
	lastSQL string
	err     error
}

func (r *recordingExec) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	r.calls++
	r.lastSQL = sql
	return pgconn.CommandTag{}, r.err
}
