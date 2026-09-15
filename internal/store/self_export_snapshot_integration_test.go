package store

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"sync"
	"testing"
	"time"

	archiveexport "github.com/witwave-ai/witself/internal/export"
	"github.com/witwave-ai/witself/internal/testenv"
)

// TestExportAccountSelfSnapshotPostgres proves both halves of active self
// export: an agent can commit while the stream is paused, and that commit is
// absent from the paused snapshot but present in a subsequent fresh export.
func TestExportAccountSelfSnapshotPostgres(t *testing.T) {
	st, _ := newMigrationTestStore(t, testenv.RequirePostgres(t))
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	// Migrations have their own fixture lifetime. Bound the actual scenario
	// separately so its cancellation also interrupts every database operation.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	account, err := st.ProvisionAccount(ctx,
		"self-export-snapshot@example.invalid", "self export snapshot", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if activated, err := st.ActivateAccount(ctx, account.AccountID); err != nil || !activated {
		t.Fatalf("activate snapshot account = %v, %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, account.AccountID, "default")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := st.CreateAgent(ctx, account.AccountID, realm.ID, "snapshot-agent")
	if err != nil {
		t.Fatal(err)
	}
	p := Principal{Kind: PrincipalAgent, ID: agent.ID, AccountID: account.AccountID,
		RealmID: realm.ID, AccountStatus: "active"}
	input := func(content, key string) CaptureMemoryInput {
		return CaptureMemoryInput{
			Content: content, Kind: "note", IdempotencyKey: key,
			Evidence: []MemoryEvidenceInput{{
				ResolutionState:    MemoryEvidenceUnavailable,
				TerminalReasonCode: "synthetic_fixture",
			}},
		}
	}
	baselineInput := input("Baseline note committed before the export snapshot.", "snapshot-baseline")
	baseline, err := st.CaptureMemory(ctx, p, baselineInput)
	if err != nil {
		t.Fatal(err)
	}
	assertSelfExportSnapshotCapture(t, baseline, baselineInput, 1)
	before := readSelfExportSnapshotRows(ctx, t, st, p.AccountID)
	assertSelfExportSnapshotState(t, before, p, []Memory{baseline.Memory})
	assertSelfExportSnapshotLiveCount(ctx, t, st, p, 1)

	gate := &selfExportSnapshotGate{
		ctx: ctx, entered: make(chan struct{}), release: make(chan struct{}),
	}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(gate.release) }) }
	done := make(chan struct{})
	var exportErr error
	// Registered after the migration fixture: cancel and drain the exporter
	// before Store.Close and schema cleanup can wait on its transaction.
	t.Cleanup(func() {
		cancel()
		release()
		timer := time.NewTimer(30 * time.Second)
		defer timer.Stop()
		select {
		case <-done:
		case <-timer.C:
			t.Error("snapshot exporter did not drain after cancellation")
		}
	})
	go func() {
		defer close(done)
		exportErr = st.ExportAccountSelf(ctx, p.AccountID,
			"snapshot-test-cell", "snapshot-test-version", gate)
	}()

	// Export reads account/schema/lifecycle state before archive.Write emits
	// its gzip header for the manifest. That first actual output precedes every
	// lazy querySource.Next, so pausing here pins the original relational
	// snapshot without relying on sleeps or a production test hook.
	barrierCtx, barrierCancel := context.WithTimeout(ctx, 30*time.Second)
	select {
	case <-gate.entered:
	case <-done:
		barrierCancel()
		t.Fatalf("snapshot export ended before first output: %v", exportErr)
	case <-barrierCtx.Done():
		barrierCancel()
		t.Fatal("snapshot export did not reach first output")
	}
	barrierCancel()

	concurrentInput := input("Concurrent note committed while the export is paused.", "snapshot-concurrent")
	mutationCtx, mutationCancel := context.WithTimeout(ctx, 30*time.Second)
	concurrent, err := st.CaptureMemory(mutationCtx, p, concurrentInput)
	mutationCancel()
	if err != nil {
		t.Fatalf("capture could not commit while export was paused: %v", err)
	}
	assertSelfExportSnapshotCapture(t, concurrent, concurrentInput, 3)
	if concurrent.Memory.ID == baseline.Memory.ID {
		t.Fatal("concurrent capture reused the baseline memory identity")
	}
	// These independent pool reads see committed rows, not the capture's
	// returned in-transaction object. The export still owns its connection and
	// cannot finish while the writer release remains closed.
	after := readSelfExportSnapshotRows(ctx, t, st, p.AccountID)
	assertSelfExportSnapshotState(t, after, p, []Memory{baseline.Memory, concurrent.Memory})
	assertSelfExportSnapshotLiveCount(ctx, t, st, p, 2)
	if before["memory_curation_requests"][0]["id"] != after["memory_curation_requests"][0]["id"] {
		t.Fatal("concurrent capture did not coalesce the baseline queued request")
	}
	select {
	case <-gate.release:
		t.Fatal("snapshot writer was released before the concurrent commit was verified")
	case <-done:
		t.Fatalf("snapshot export ended while its writer was paused: %v", exportErr)
	case <-ctx.Done():
		t.Fatal("snapshot scenario expired before concurrent commit verification")
	default:
	}
	t.Log("concurrent capture committed while export remained paused")
	release()
	select {
	case <-done:
		if exportErr != nil {
			t.Fatalf("complete paused snapshot export: %v", exportErr)
		}
	case <-ctx.Done():
		t.Fatal("snapshot export did not complete after writer release")
	}
	// Reading the buffer only after done also synchronizes with the writer.
	first := readSelfExportSnapshotArchive(ctx, t, gate.Bytes(), p.AccountID)
	assertSelfExportSnapshotMembership(t, "first export snapshot", first, []Memory{baseline.Memory})
	assertSelfExportSnapshotRowsEqual(t, "first export snapshot", first, before)

	var fresh bytes.Buffer
	if err := st.ExportAccountSelf(ctx, p.AccountID,
		"snapshot-test-cell", "snapshot-test-version", &fresh); err != nil {
		t.Fatalf("fresh snapshot export: %v", err)
	}
	second := readSelfExportSnapshotArchive(ctx, t, fresh.Bytes(), p.AccountID)
	assertSelfExportSnapshotMembership(t, "fresh export snapshot", second, []Memory{baseline.Memory, concurrent.Memory})
	assertSelfExportSnapshotRowsEqual(t, "fresh export snapshot", second, after)
	// Neither export may undo a write, reconcile the curation request, or
	// suspend the active source. The fresh archive is not an empty/stale oracle.
	assertSelfExportSnapshotRowsEqual(t, "source after exports",
		readSelfExportSnapshotRows(ctx, t, st, p.AccountID), after)
	assertSelfExportSnapshotLiveCount(ctx, t, st, p, 2)
	var status string
	if err := st.pool.QueryRow(ctx, `SELECT status FROM accounts WHERE id=$1`, p.AccountID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "active" {
		t.Fatalf("source account status after snapshot exports = %q, want active", status)
	}
}

type selfExportSnapshotGate struct {
	bytes.Buffer
	ctx     context.Context
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (w *selfExportSnapshotGate) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	w.once.Do(func() { close(w.entered) })
	select {
	case <-w.ctx.Done():
		return 0, w.ctx.Err()
	case <-w.release:
	}
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	return w.Buffer.Write(p)
}

// Compare full portable rows independently of export's jsonb_build_object
// queries. Only the two intentionally nonportable derived columns are removed
// from the database side; no archive fields are discarded. Sorting preserves
// duplicates, unlike indexing rows into a map by ID.
var selfExportSnapshotTables = []string{
	"memories", "memory_versions", "memory_evidence", "memory_change_clocks",
	"memory_curation_lanes", "memory_curation_requests", "memory_curation_mutations",
}

type selfExportSnapshotRows map[string][]map[string]any

func readSelfExportSnapshotRows(ctx context.Context, t *testing.T, st *Store, accountID string) selfExportSnapshotRows {
	t.Helper()
	result := selfExportSnapshotRows{}
	for _, table := range selfExportSnapshotTables {
		projection := "to_jsonb(r)"
		switch table {
		case "memory_versions":
			projection += " - 'search_document'"
		case "memory_change_clocks":
			projection += " - 'active_memory_count'"
		}
		// Table names above are fixed test constants, never input.
		rows, err := st.pool.Query(ctx, "SELECT "+projection+" FROM "+table+" r WHERE account_id=$1", accountID)
		if err != nil {
			t.Fatalf("read snapshot table %s: %v", table, err)
		}
		for rows.Next() {
			var raw []byte
			if err := rows.Scan(&raw); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			var row map[string]any
			if err := json.Unmarshal(raw, &row); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			result[table] = append(result[table], row)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
	}
	return result
}

func readSelfExportSnapshotArchive(ctx context.Context, t *testing.T, raw []byte, accountID string) selfExportSnapshotRows {
	t.Helper()
	result := selfExportSnapshotRows{}
	manifest, err := archiveexport.Read(ctx, bytes.NewReader(raw), archiveexport.ImportOptions{
		CurrentSchema: SchemaVersion(),
		Row: func(table string, raw []byte) error {
			if !slices.Contains(selfExportSnapshotTables, table) {
				return nil
			}
			var row map[string]any
			if err := json.Unmarshal(raw, &row); err != nil {
				return err
			}
			result[table] = append(result[table], row)
			return nil
		},
	})
	// Row callbacks only stage data. No row becomes evidence until the reader
	// verifies the complete structure and trailing chunk checksums.
	if err != nil {
		t.Fatalf("verify snapshot archive checksums: %v", err)
	}
	if manifest.Purpose != archiveexport.PurposeSelf || manifest.AccountID != accountID ||
		manifest.Status != "active" || manifest.FormatVersion != archiveexport.FormatVersion ||
		manifest.SchemaVersion != SchemaVersion() || manifest.BackupID != "" || manifest.EvacuationID != "" {
		t.Fatal("snapshot archive manifest identity or purpose differs")
	}
	return result
}

func assertSelfExportSnapshotCapture(t *testing.T, got MemoryMutationResult, in CaptureMemoryInput, seq int64) {
	t.Helper()
	m := got.Memory
	if got.Receipt.Replayed || m.ID == "" || m.Version != 1 || m.ChangeSeq != seq ||
		m.Content != in.Content || m.IdempotencyKey != in.IdempotencyKey ||
		m.State != MemoryStateActive || m.Kind != "note" || m.Sensitive || len(m.Evidence) != 1 {
		t.Fatal("snapshot capture did not create the expected nonempty note and version")
	}
	e := m.Evidence[0]
	if e.ID == "" || e.MemoryID != m.ID || e.TargetVersion != 1 || e.EvidenceChangeSeq != seq+1 ||
		e.ResolutionState != MemoryEvidenceUnavailable || e.ExternalLocator != "" ||
		e.TerminalReasonCode != "synthetic_fixture" {
		t.Fatal("snapshot capture did not create the expected unavailable evidence")
	}
}

func assertSelfExportSnapshotMembership(t *testing.T, phase string, rows selfExportSnapshotRows, memories []Memory) {
	t.Helper()
	var got, want []string
	for _, row := range rows["memories"] {
		id, ok := row["id"].(string)
		if !ok || id == "" {
			t.Fatalf("%s: invalid memory identity", phase)
		}
		got = append(got, id)
	}
	for _, memory := range memories {
		want = append(want, memory.ID)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("%s: memory membership differs (got %d rows, want %d)", phase, len(got), len(want))
	}
}

func assertSelfExportSnapshotState(t *testing.T, rows selfExportSnapshotRows, p Principal, memories []Memory) {
	t.Helper()
	n := len(memories)
	assertSelfExportSnapshotMembership(t, "committed database snapshot", rows, memories)
	for _, table := range selfExportSnapshotTables {
		want := n
		if table == "memory_change_clocks" || table == "memory_curation_lanes" || table == "memory_curation_requests" {
			want = 1
		}
		if len(rows[table]) != want || want == 0 {
			t.Fatalf("committed snapshot table %s has %d rows, want %d nonempty rows", table, len(rows[table]), want)
		}
		for _, row := range rows[table] {
			if row["account_id"] != p.AccountID || row["realm_id"] != p.RealmID ||
				row["owner_kind"] != "agent" || row["owner_id"] != p.ID {
				t.Fatalf("committed snapshot table %s has an unexpected owner", table)
			}
		}
	}
	for _, memory := range memories {
		var heads, versions, evidence int
		for _, row := range rows["memories"] {
			if row["id"] == memory.ID && row["current_version"] == float64(1) {
				heads++
			}
		}
		for _, row := range rows["memory_versions"] {
			if row["memory_id"] == memory.ID && row["version"] == float64(1) &&
				row["change_seq"] == float64(memory.ChangeSeq) && row["content"] == memory.Content &&
				row["state"] == MemoryStateActive && row["idempotency_key"] == memory.IdempotencyKey {
				versions++
			}
		}
		for _, row := range rows["memory_evidence"] {
			if row["id"] == memory.Evidence[0].ID && row["memory_id"] == memory.ID &&
				row["target_version"] == float64(1) && row["evidence_change_seq"] == float64(memory.ChangeSeq+1) &&
				row["resolution_state"] == MemoryEvidenceUnavailable && row["external_locator"] == nil {
				evidence++
			}
		}
		if heads != 1 || versions != 1 || evidence != 1 {
			t.Fatal("committed snapshot lacks an exact captured memory/version/evidence row")
		}
	}
	clock, lane, request := rows["memory_change_clocks"][0], rows["memory_curation_lanes"][0], rows["memory_curation_requests"][0]
	requestID, ok := request["id"].(string)
	if !ok || requestID == "" || clock["last_change_seq"] != float64(2*n) ||
		lane["request_generation"] != float64(n) || lane["fencing_generation"] != float64(0) || lane["active_run_id"] != nil ||
		request["request_generation"] != float64(n) || request["state"] != MemoryCurationRequestQueued ||
		request["trigger_reason"] != "memory_changed" || request["claimed_run_id"] != nil {
		t.Fatal("committed snapshot clock, lane, or queued request did not advance exactly")
	}
	var generations []float64
	for _, row := range rows["memory_curation_mutations"] {
		generation, ok := row["request_generation"].(float64)
		if !ok || row["request_id"] != requestID || row["operation"] != "request" ||
			row["result_state"] != MemoryCurationRequestQueued || row["run_id"] != nil {
			t.Fatal("committed snapshot has an unexpected curation mutation receipt")
		}
		generations = append(generations, generation)
	}
	slices.Sort(generations)
	for i, generation := range generations {
		if generation != float64(i+1) {
			t.Fatal("committed snapshot curation receipt generations are incomplete or duplicated")
		}
	}
}

func assertSelfExportSnapshotRowsEqual(t *testing.T, phase string, got, want selfExportSnapshotRows) {
	t.Helper()
	canonical := func(rows []map[string]any) []string {
		result := make([]string, 0, len(rows))
		for _, row := range rows {
			raw, err := json.Marshal(row)
			if err != nil {
				t.Fatal(err)
			}
			result = append(result, string(raw))
		}
		slices.Sort(result)
		return result
	}
	for _, table := range selfExportSnapshotTables {
		if !slices.Equal(canonical(got[table]), canonical(want[table])) {
			t.Fatalf("%s: portable rows differ in %s", phase, table)
		}
	}
}

func assertSelfExportSnapshotLiveCount(ctx context.Context, t *testing.T, st *Store, p Principal, want int64) {
	t.Helper()
	var count int64
	if err := st.pool.QueryRow(ctx, `SELECT active_memory_count FROM memory_change_clocks
		WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3`,
		p.AccountID, p.RealmID, p.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Fatalf("live active memory count = %d, want %d", count, want)
	}
}
