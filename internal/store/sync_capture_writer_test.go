package store

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/witwave-ai/witself/internal/runtimeconfig"
)

func syncPreferenceDocument(theme string) json.RawMessage {
	return json.RawMessage(`{"schema":"witself.dashboard-prefs.v1","theme":"` + theme + `"}`)
}

func TestSyncCaptureSelectionRefusesHosted(t *testing.T) {
	for _, cfg := range []runtimeconfig.Config{{}, {Local: true}, {SyncSchema: true}} {
		st := &Store{}
		if err := st.SelectSyncCapture(cfg); !errors.Is(err, ErrSyncWriterNotAdmitted) || st.syncCaptureSelected.Load() {
			t.Fatal("sync selection accepted an unselected runtime")
		}
		if _, err := st.AdmitSyncTx(context.Background(), "account", "principal", "capability"); !errors.Is(err, ErrSyncWriterNotAdmitted) {
			t.Fatal("sync selection attempted unselected admission")
		}
	}
}

func TestSyncCapturePreferencesDefaultSQL(t *testing.T) {
	read := func(path, function string) string {
		t.Helper()
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal("sync preference source parsing failed")
		}
		var sql string
		for _, declaration := range file.Decls {
			fn, ok := declaration.(*ast.FuncDecl)
			if !ok || fn.Name.Name != function {
				continue
			}
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok || len(call.Args) < 2 {
					return true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || selector.Sel.Name != "QueryRow" {
					return true
				}
				literal, ok := call.Args[1].(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING || sql != "" {
					t.Fatal("sync preference SQL literal changed shape")
				}
				sql, err = strconv.Unquote(literal.Value)
				if err != nil {
					t.Fatal("sync preference SQL literal decoding failed")
				}
				return true
			})
		}
		return sql
	}
	baseline := read("dashboard_preferences.go", "PutDashboardPreferences")
	if baseline == "" || baseline != read("sync_capture_preferences.go", "putSyncDashboardPreferencesSQL") {
		t.Fatal("sync preference selected upsert changed original SQL bytes")
	}
}

func TestSyncCaptureForgottenJournal(t *testing.T) {
	if err := validateSyncWriterInventory(migrationsFS, syncMigrationsFS, syncWriterTables); err != nil {
		t.Fatal("sync omission source inventory completeness failed")
	}
	st, p := newSyncCaptureFixture(t, true)
	ctx := context.Background()
	if err := validateInstalledSyncWriterInventory(ctx, st); err != nil {
		t.Fatal("sync omission installed inventory completeness failed")
	}
	second := p
	if err := st.pool.QueryRow(ctx, `SELECT id FROM agents WHERE realm_id=$1 AND name='second'`, p.RealmID).Scan(&second.ID); err != nil {
		t.Fatal("sync omission second agent fixture unavailable")
	}
	cases := []struct {
		name  string
		fault syncPreferencesFault
	}{
		{"omit_touch", syncPreferencesFault{omitTouch: true}},
		{"omit_seal", syncPreferencesFault{omitSeal: true}},
		{"seal_empty_bundle", syncPreferencesFault{emptySeal: true}},
		{"touch_one_of_two_writes", syncPreferencesFault{second: &second}},
		{"same_row_twice_one_touch", syncPreferencesFault{repeatWrite: true}},
	}
	completed := 0
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := st.putDashboardPreferencesSync(ctx, p, syncPreferenceDocument("amber"), tc.fault); !errors.Is(err, ErrSyncWriterNotAdmitted) {
				t.Fatalf("sync omission did not refuse atomically: %v", err)
			}
			for _, table := range []string{"agent_dashboard_preferences", "sync_tx_admissions", "sync_tx_touches", "sync_operations", "sync_operation_payloads", "sync_bundles", "sync_bundle_operations"} {
				var count int
				if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE account_id=$1`, p.AccountID).Scan(&count); err != nil || count != 0 {
					t.Fatalf("sync omission left durable rows in reviewed table: %s", table)
				}
			}
			var finalized int64
			if err := st.pool.QueryRow(ctx, `SELECT last_finalized FROM sync_finalized_counter WHERE account_id=$1`, p.AccountID).Scan(&finalized); err != nil || finalized != 0 {
				t.Fatal("sync omission advanced the committed counter")
			}
			completed++
		})
	}
	captureEvidence(t, completed, len(cases))
}

func TestSyncCapturePreferencesDefaultBytes(t *testing.T) {
	st, p := newSyncCaptureFixture(t, false)
	ctx := context.Background()
	var eventsBefore int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM account_events WHERE account_id=$1`, p.AccountID).Scan(&eventsBefore); err != nil {
		t.Fatal("sync default baseline event count failed")
	}
	// Freeze only the nondeterministic clock field. Document bytes, agent identity,
	// field order and the full domain projection remain exact comparisons.
	fixed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var responseBaseline, domainBaseline []byte
	for _, selected := range []bool{false, true} {
		st.syncCaptureSelected.Store(selected)
		out, err := st.PutDashboardPreferences(ctx, p, syncPreferenceDocument("amber"))
		if err != nil {
			t.Fatal("sync default fixed preference write failed")
		}
		out.UpdatedAt = fixed
		response, err := json.Marshal(out)
		if err != nil {
			t.Fatal("sync default response serialization failed")
		}
		var domain []byte
		if err := st.pool.QueryRow(ctx, `SELECT jsonb_build_object('agent_id',agent_id,'account_id',account_id,
			'realm_id',realm_id,'prefs',prefs,'updated_at',$2::timestamptz)::text
			FROM agent_dashboard_preferences WHERE agent_id=$1`, p.ID, fixed).Scan(&domain); err != nil {
			t.Fatal("sync default domain projection failed")
		}
		if !selected {
			responseBaseline, domainBaseline = response, domain
		} else if !bytes.Equal(response, responseBaseline) || !bytes.Equal(domain, domainBaseline) {
			t.Fatal("sync default selected-inactive bytes changed")
		}
	}
	var eventsAfter int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM account_events WHERE account_id=$1`, p.AccountID).Scan(&eventsAfter); err != nil || eventsAfter != eventsBefore {
		t.Fatal("sync default preferences added an audit verb")
	}
	for _, table := range []string{"sync_capture", "sync_tx_admissions", "sync_tx_touches", "sync_operations", "sync_bundles"} {
		var count int
		if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE account_id=$1`, p.AccountID).Scan(&count); err != nil || count != 0 {
			t.Fatalf("sync default created rows in reviewed sync table: %s", table)
		}
	}
}

func TestSyncCapturePreferencesSemanticBundle(t *testing.T) {
	st, p := newSyncCaptureFixture(t, true)
	ctx := context.Background()
	var priorRevision string
	for i, theme := range []string{"amber", "console"} {
		canonical := syncPreferenceDocument(theme)
		if _, err := st.PutDashboardPreferences(ctx, p, canonical); err != nil {
			t.Fatalf("sync preference admitted write failed: %v", err)
		}
		var kind, dependency, effect string
		var finalized, members int
		if err := st.pool.QueryRow(ctx, `SELECT o.operation_kind,t.dependencies->>'prior_revision',
			t.semantic_effect->>'theme',b.finalized,
			(SELECT count(*) FROM sync_bundle_operations m WHERE m.account_id=b.account_id AND m.bundle_id=b.bundle_id)
			FROM sync_bundles b JOIN sync_tx_touches t ON t.account_id=b.account_id AND t.tx_id=b.tx_id
			JOIN sync_operations o ON o.account_id=t.account_id AND o.operation_id=t.operation_id
			WHERE b.account_id=$1 ORDER BY b.finalized DESC LIMIT 1`, p.AccountID).Scan(&kind, &dependency, &effect, &finalized, &members); err != nil {
			t.Fatal("sync preference semantic bundle read failed")
		}
		if kind != SyncPreferencesCapability || effect != theme || finalized != i+1 || members != 1 ||
			(i == 0 && dependency != "absent") || (i == 1 && dependency != priorRevision) {
			t.Fatal("sync preference semantic bundle differs from actual write")
		}
		var envelope []byte
		if err := st.pool.QueryRow(ctx, `SELECT p.canonical_semantic_envelope FROM sync_operation_payloads p
			JOIN sync_bundle_operations m USING(account_id,operation_id) JOIN sync_bundles b USING(account_id,bundle_id)
			WHERE b.account_id=$1 AND b.finalized=$2`, p.AccountID, i+1).Scan(&envelope); err != nil {
			t.Fatal("sync preference canonical payload read failed")
		}
		var decoded struct {
			Effect json.RawMessage `json:"effect"`
		}
		if json.Unmarshal(envelope, &decoded) != nil || !bytes.Equal(decoded.Effect, canonical) {
			t.Fatal("sync preference canonical document bytes changed")
		}
		var operationID []byte
		if err := st.pool.QueryRow(ctx, `SELECT m.operation_id FROM sync_bundle_operations m JOIN sync_bundles b USING(account_id,bundle_id)
			WHERE b.account_id=$1 AND b.finalized=$2`, p.AccountID, i+1).Scan(&operationID); err != nil {
			t.Fatal("sync preference prior revision read failed")
		}
		priorRevision = "operation:" + hex.EncodeToString(operationID)
	}
}

func TestSyncCaptureSavepointAndPostSeal(t *testing.T) {
	st, p := newSyncCaptureFixture(t, true)
	ctx := context.Background()
	tx, err := st.AdmitSyncTx(ctx, p.AccountID, SyncPreferencesPrincipal, SyncPreferencesCapability)
	if err != nil {
		t.Fatal("sync savepoint initial admission failed")
	}
	defer func() { _ = tx.Rollback(ctx) }()
	intentional := errors.New("sync savepoint intentional rollback")
	err = tx.Savepoint(ctx, func(child *SyncTx) error {
		if _, err := putSyncDashboardPreferencesSQL(ctx, child.Tx(), p, syncPreferenceDocument("amber")); err != nil {
			return err
		}
		if err := child.Touch(ctx, SyncPreferencesCapability, "preferences", syncPreferenceDocument("amber"), json.RawMessage(`[]`)); err != nil {
			return err
		}
		if err := child.Seal(ctx); err != nil {
			return err
		}
		return intentional
	})
	if !errors.Is(err, intentional) || tx.sealed {
		t.Fatal("sync savepoint did not restore unsealed handle")
	}
	var actual, semantic, operations, bundles int
	if err := tx.Tx().QueryRow(ctx, `SELECT jsonb_array_length(actual_touches),
		(SELECT count(*) FROM sync_tx_touches WHERE account_id=$1),
		(SELECT count(*) FROM sync_operations WHERE account_id=$1),
		(SELECT count(*) FROM sync_bundles WHERE account_id=$1)
		FROM sync_tx_admissions WHERE account_id=$1 AND tx_id=$2`, p.AccountID, tx.id).Scan(&actual, &semantic, &operations, &bundles); err != nil || actual != 0 || semantic != 0 || operations != 0 || bundles != 0 {
		t.Fatal("sync savepoint retained rolled-back semantic fragments")
	}
	if _, err := putSyncDashboardPreferencesSQL(ctx, tx.Tx(), p, syncPreferenceDocument("console")); err != nil {
		t.Fatal("sync savepoint replacement domain write failed")
	}
	if err := tx.Touch(ctx, SyncPreferencesCapability, "preferences", syncPreferenceDocument("console"), json.RawMessage(`[]`)); err != nil {
		t.Fatal("sync savepoint replacement touch failed")
	}
	if err := tx.Seal(ctx); err != nil {
		t.Fatal("sync savepoint replacement seal failed")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal("sync savepoint replacement commit failed")
	}
	tx, err = st.AdmitSyncTx(ctx, p.AccountID, SyncPreferencesPrincipal, SyncPreferencesCapability)
	if err != nil {
		t.Fatal("sync postseal initial admission failed")
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := putSyncDashboardPreferencesSQL(ctx, tx.Tx(), p, syncPreferenceDocument("amber")); err != nil {
		t.Fatal("sync postseal initial write failed")
	}
	if err := tx.Touch(ctx, SyncPreferencesCapability, "preferences", syncPreferenceDocument("amber"), json.RawMessage(`[]`)); err != nil {
		t.Fatal("sync postseal initial touch failed")
	}
	if err := tx.Seal(ctx); err != nil {
		t.Fatal("sync postseal initial seal failed")
	}
	_, err = tx.Tx().Exec(ctx, `UPDATE agent_dashboard_preferences SET prefs=$2::jsonb WHERE agent_id=$1`, p.ID, syncPreferenceDocument("console"))
	var pgerr *pgconn.PgError
	if !errors.As(err, &pgerr) || pgerr.Code != "P0001" || pgerr.Message != ErrSyncWriterNotAdmitted.Error() {
		t.Fatal("sync postseal mutation did not receive fixed refusal")
	}
	if err := tx.Commit(ctx); err == nil {
		t.Fatal("sync postseal failed transaction unexpectedly committed")
	}
}
