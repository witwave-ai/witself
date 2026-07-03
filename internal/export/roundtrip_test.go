package export

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

// TestWriteReadComposedRoundTrip pins the seam between the writer and
// reader as ONE contract. A future edit that breaks either side in a way
// the isolated writer/reader tests miss — a chunk-size shift that changes
// row boundaries, a header field the writer emits but the reader ignores,
// a manifest change that decodes as null — must fail here.
//
// The fixture uses an ExportAccount-shaped table set (accounts, operators,
// realms, agents, tokens) in FK dependency order with JSON payloads that
// mimic the Postgres jsonb_build_object output. Every row that goes in
// must come out byte-identical, in the same table order, with the manifest
// intact.
func TestWriteReadComposedRoundTrip(t *testing.T) {
	// Rows that would legitimately be emitted by ExportAccount: strings,
	// booleans, ISO-8601 timestamps, explicit nulls for nullable columns
	// like closed_at / suspended_for / agent_id. These are the value
	// shapes the reader must round-trip untouched.
	fixture := []tableFixture{
		{
			table: "accounts",
			rows: []map[string]any{
				{
					"id":         "acc_rt_target",
					"is_default": false,
					"status":     "suspended",
					"created_at": "2026-06-14T22:30:00Z",
					// nullable fields explicitly null
					"closed_at":        nil,
					"closed_reason":    nil,
					"suspended_at":     "2026-07-03T08:00:00Z",
					"suspended_for":    "evacuation",
					"suspended_reason": "cell decommission",
					"display_name":     "Round-trip target",
					"email":            "scott@example.com",
				},
			},
		},
		{
			table: "operators",
			rows: []map[string]any{
				{
					"id":           "op_root",
					"account_id":   "acc_rt_target",
					"role":         "account_owner",
					"is_root":      true,
					"display_name": "owner",
					"created_at":   "2026-06-14T22:30:00Z",
					"updated_at":   "2026-06-14T22:30:00Z",
					"deleted_at":   nil,
				},
			},
		},
		{
			table: "realms",
			rows: []map[string]any{
				{
					"id":         "rlm_default",
					"account_id": "acc_rt_target",
					"name":       "default",
					"created_at": "2026-06-14T22:30:00Z",
					"updated_at": "2026-06-14T22:30:00Z",
					"deleted_at": nil,
				},
			},
		},
		{
			table: "agents",
			rows: []map[string]any{
				{
					"id":         "agt_archivist",
					"realm_id":   "rlm_default",
					"name":       "archivist",
					"created_at": "2026-06-14T22:30:00Z",
					"updated_at": "2026-06-14T22:30:00Z",
					"deleted_at": nil,
				},
			},
		},
		{
			table: "tokens",
			rows: []map[string]any{
				// One operator token; one agent token; one bootstrap token
				// with both operator_id and agent_id null — the exact shape
				// the FK-scoping validator on the import side tolerates.
				{
					"id":           "tok_op",
					"account_id":   "acc_rt_target",
					"operator_id":  "op_root",
					"agent_id":     nil,
					"kind":         "operator",
					"token_hash":   "abcdef0123456789",
					"display_name": "owner-token",
					"created_at":   "2026-06-14T22:30:00Z",
					"expires_at":   nil,
					"consumed_at":  nil,
				},
				{
					"id":           "tok_agt",
					"account_id":   "acc_rt_target",
					"operator_id":  nil,
					"agent_id":     "agt_archivist",
					"kind":         "agent",
					"token_hash":   "0011223344556677",
					"display_name": "archivist-token",
					"created_at":   "2026-06-14T22:30:00Z",
					"expires_at":   nil,
					"consumed_at":  nil,
				},
				{
					"id":           "tok_bootstrap",
					"account_id":   "acc_rt_target",
					"operator_id":  nil,
					"agent_id":     nil,
					"kind":         "bootstrap",
					"token_hash":   "cafebabecafebabe",
					"display_name": "bootstrap",
					"created_at":   "2026-06-14T22:30:00Z",
					"expires_at":   "2026-06-14T23:30:00Z",
					"consumed_at":  nil,
				},
			},
		},
	}

	var buf bytes.Buffer
	m := Manifest{
		SchemaVersion: 13,
		ServerVersion: "0.0.83",
		AccountID:     "acc_rt_target",
		Cell:          "aws-sandbox-usw2-dev",
		Status:        "suspended",
	}
	sources := make([]RowSource, len(fixture))
	for i, tf := range fixture {
		sources[i] = &fixtureSource{table: tf.table, rows: tf.rows}
	}
	if err := Write(context.Background(), &buf, m, sources); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// The manifest that comes back must carry the same coordinates AND
	// the same Tables list, in the same order — that ordering is what
	// tells the importer the FK dependency order it must insert in.
	var readTables []string
	roundTripRows := map[string][]map[string]any{}
	gotManifest, err := Read(context.Background(), &buf, ImportOptions{
		CurrentSchema: 13,
		OnManifest: func(m Manifest) error {
			if m.SchemaVersion != 13 || m.AccountID != "acc_rt_target" || m.Status != "suspended" {
				return fmt.Errorf("manifest coords wrong: %+v", m)
			}
			return nil
		},
		Row: func(table string, row []byte) error {
			if len(readTables) == 0 || readTables[len(readTables)-1] != table {
				readTables = append(readTables, table)
			}
			var obj map[string]any
			if err := json.Unmarshal(row, &obj); err != nil {
				return fmt.Errorf("row not JSON: %w", err)
			}
			roundTripRows[table] = append(roundTripRows[table], obj)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	// Table order MUST match — importer relies on it for FK order.
	wantOrder := []string{"accounts", "operators", "realms", "agents", "tokens"}
	if len(readTables) != len(wantOrder) {
		t.Fatalf("read table order %v, want %v", readTables, wantOrder)
	}
	for i, table := range wantOrder {
		if readTables[i] != table {
			t.Errorf("table %d = %q, want %q", i, readTables[i], table)
		}
	}

	if gotManifest.SchemaVersion != m.SchemaVersion {
		t.Errorf("manifest schema round-trip: got %d, want %d", gotManifest.SchemaVersion, m.SchemaVersion)
	}
	if gotManifest.ServerVersion != m.ServerVersion {
		t.Errorf("manifest server_version round-trip: got %q, want %q", gotManifest.ServerVersion, m.ServerVersion)
	}
	if gotManifest.AccountID != m.AccountID {
		t.Errorf("manifest account_id round-trip: got %q, want %q", gotManifest.AccountID, m.AccountID)
	}
	if gotManifest.Cell != m.Cell {
		t.Errorf("manifest cell round-trip: got %q, want %q", gotManifest.Cell, m.Cell)
	}
	if gotManifest.Status != m.Status {
		t.Errorf("manifest status round-trip: got %q, want %q", gotManifest.Status, m.Status)
	}

	// Every row is preserved exactly: every field the writer emitted must
	// come back with the same value and the same JSON type. This is what
	// would catch a future edit that (say) drops nullable fields or
	// stringifies booleans on the way through.
	for _, tf := range fixture {
		gotRows := roundTripRows[tf.table]
		if len(gotRows) != len(tf.rows) {
			t.Errorf("%s: got %d rows, want %d", tf.table, len(gotRows), len(tf.rows))
			continue
		}
		for i, want := range tf.rows {
			got := gotRows[i]
			if len(got) != len(want) {
				t.Errorf("%s row %d: got %d fields, want %d (got=%v want=%v)", tf.table, i, len(got), len(want), got, want)
			}
			for k, wantV := range want {
				gotV, present := got[k]
				if !present {
					t.Errorf("%s row %d: field %q missing after round-trip", tf.table, i, k)
					continue
				}
				// JSON booleans / strings / numbers / nulls all decode to
				// their Go zero-form equivalents; nil == nil.
				if !equalJSON(wantV, gotV) {
					t.Errorf("%s row %d: field %q = %v (%T), want %v (%T)", tf.table, i, k, gotV, gotV, wantV, wantV)
				}
			}
		}
	}
}

type tableFixture struct {
	table string
	rows  []map[string]any
}

// fixtureSource emits caller-supplied JSON rows as if they came from a
// Postgres jsonb_build_object query — one row at a time, in the given
// order.
type fixtureSource struct {
	table string
	rows  []map[string]any
	i     int
}

func (fs *fixtureSource) Table() string { return fs.table }

func (fs *fixtureSource) Next(_ context.Context) ([]byte, error) {
	if fs.i >= len(fs.rows) {
		return nil, nil
	}
	raw, err := json.Marshal(fs.rows[fs.i])
	fs.i++
	return raw, err
}

// equalJSON compares two values through their JSON representation so that
// (for example) a Go string "false" isn't accidentally treated as equal
// to a JSON boolean false — this test's whole purpose is to catch type
// slippage.
func equalJSON(a, b any) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	ab, ae := json.Marshal(a)
	bb, be := json.Marshal(b)
	if ae != nil || be != nil {
		return false
	}
	return bytes.Equal(ab, bb)
}
