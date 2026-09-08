package store

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	archiveexport "github.com/witwave-ai/witself/internal/export"
	"github.com/witwave-ai/witself/internal/testenv"
)

// These histories exercise the real accounts unique-index dependency. The
// AFTER INSERT barrier holds A's uncommitted tuple while B passes the initial
// absence check and waits for A; launching two goroutines alone proves neither.
func TestImportAccountContentionPostgres(t *testing.T) {
	st, _ := newMigrationTestStore(t, testenv.RequirePostgres(t))
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	for _, corrupt := range []bool{false, true} {
		name := "valid_winner"
		if corrupt {
			name = "corrupt_trailer_rolls_back"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			t.Cleanup(cancel)
			fixture := prepareImportContentionFixture(ctx, t, st, name)
			bystander := prepareImportContentionFixture(ctx, t, st, name+"-bystander")
			bystanderBefore := importContentionDatabaseRows(ctx, t, st, bystander.accountID)
			counts := make(map[string]int, len(fixture.rows))
			for table, rows := range fixture.rows {
				counts[table] = len(rows)
			}
			removed, err := removeMemoryArchiveLoadAccount(ctx, st, fixture.accountID, counts)
			if err != nil || !removed {
				t.Fatalf("import contention fixture removal failed: %v", err)
			}
			for table, rows := range importContentionDatabaseRows(ctx, t, st, fixture.accountID) {
				if len(rows) != 0 {
					t.Fatalf("import contention fixture still has rows in %s", table)
				}
			}
			assertImportContentionRows(t, "fixture removal changed bystander", bystanderBefore,
				importContentionDatabaseRows(ctx, t, st, bystander.accountID))

			firstArchive := fixture.archive
			if corrupt {
				firstArchive = corruptImportContentionTrailer(t, fixture.archive)
				_, staged, readErr := readImportContentionArchive(ctx, firstArchive)
				if !isImportContentionChecksumError(readErr) {
					t.Fatal("import contention variant did not fail at the checksum trailer")
				}
				// A trailer error comes after every Row callback. Staged rows are
				// only compared here to prove the deliberately broken fixture's
				// exact boundary, never accepted as a valid archive.
				assertImportContentionRows(t, "checksum variant changed streamed rows", fixture.rows, staged)
				t.Log("import contention: checksum variant preserved every streamed row")
			}

			run := prepareImportContentionRun(ctx, t, st, fixture.accountID)
			run.start(ctx, 0, fixture.accountID, firstArchive)
			waitForImportContention(ctx, t, st, run, false)
			run.start(ctx, 1, fixture.accountID, fixture.archive)
			waitForImportContention(ctx, t, st, run, true)
			t.Log("import contention: A is paused after account insert; B is directly blocked by A")
			if err := run.release(ctx); err != nil {
				t.Fatalf("import contention barrier release failed: %v", err)
			}
			first := run.receive(ctx, t, 0)
			second := run.receive(ctx, t, 1)
			if corrupt {
				if !isImportContentionChecksumError(first.err) {
					t.Fatal("import contention first importer did not reach the checksum trailer")
				}
				t.Log("import contention: first archive rejected at checksum trailer")
				if second.err != nil {
					var exists bool
					if err := st.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM accounts WHERE id=$1)`, fixture.accountID).Scan(&exists); err != nil {
						t.Fatalf("import contention failed-waiter observation failed: %v", err)
					}
					if exists && errors.Is(second.err, ErrAccountExists) {
						// The deferred-Commit control must actually commit the
						// invalid restore. A failed deferred constraint cannot
						// produce this marker or satisfy the external control.
						t.Log("import contention: corrupt import left a durable account and refused the valid waiter")
					}
					t.Fatal("import contention rollback: waiting valid importer did not succeed")
				}
				assertImportContentionManifest(t, second.manifest, fixture.accountID)
			} else {
				if first.err != nil {
					t.Fatalf("import contention valid first importer failed: %v", first.err)
				}
				assertImportContentionManifest(t, first.manifest, fixture.accountID)
				t.Log("import contention: valid winner committed")
				if !errors.Is(second.err, ErrAccountExists) {
					var pgErr *pgconn.PgError
					if errors.As(second.err, &pgErr) && pgErr.Code == "23505" {
						t.Log("import contention: losing importer exposed accounts unique violation")
					}
					t.Fatal("import contention winner: losing importer did not return ErrAccountExists")
				}
				var pgErr *pgconn.PgError
				if errors.As(second.err, &pgErr) {
					t.Fatal("import contention losing importer leaked a PostgreSQL error")
				}
			}

			var restored bytes.Buffer
			if err := st.ExportAccount(ctx, fixture.accountID, "import-contention", "test", &restored); err != nil {
				t.Fatalf("import contention restored export failed: %v", err)
			}
			manifest, restoredRows, err := readImportContentionArchive(ctx, restored.Bytes())
			if err != nil {
				t.Fatalf("import contention restored archive verification failed: %v", err)
			}
			assertImportContentionManifest(t, manifest, fixture.accountID)
			assertImportContentionRows(t, "restored portable graph differs", fixture.rows, restoredRows)
			assertImportContentionBookkeeping(ctx, t, st, fixture)
			beforeRetry := importContentionDatabaseRows(ctx, t, st, fixture.accountID)
			if _, err := st.ImportAccount(ctx, fixture.accountID, bytes.NewReader(fixture.archive)); !errors.Is(err, ErrAccountExists) {
				t.Fatal("import contention subsequent generic retry did not return ErrAccountExists")
			}
			assertImportContentionRows(t, "generic retry changed durable rows", beforeRetry,
				importContentionDatabaseRows(ctx, t, st, fixture.accountID))
			assertImportContentionRows(t, "imports changed bystander", bystanderBefore,
				importContentionDatabaseRows(ctx, t, st, bystander.accountID))
			t.Log("import contention: exact restored graph, retry and bystander preserved")
		})
	}
}

type importContentionRows map[string][]string

type importContentionFixture struct {
	accountID, realmID, agentID, memoryID string
	archive                               []byte
	rows                                  importContentionRows
}

func prepareImportContentionFixture(ctx context.Context, t *testing.T, st *Store, name string) importContentionFixture {
	t.Helper()
	account, err := st.ProvisionAccount(ctx, name+"@example.invalid", "import contention", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if activated, err := st.ActivateAccount(ctx, account.AccountID); err != nil || !activated {
		t.Fatalf("activate import fixture failed: %v", err)
	}
	realm, err := st.CreateRealm(ctx, account.AccountID, "default")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := st.CreateAgent(ctx, account.AccountID, realm.ID, "import-agent")
	if err != nil {
		t.Fatal(err)
	}
	p := Principal{Kind: PrincipalAgent, ID: agent.ID, AccountID: account.AccountID,
		RealmID: realm.ID, AccountStatus: "active"}
	created, err := st.CaptureMemory(ctx, p, CaptureMemoryInput{
		Content: "Synthetic note retained across competing account imports.", Kind: "note",
		IdempotencyKey: "import-contention-memory",
		Evidence: []MemoryEvidenceInput{{ResolutionState: MemoryEvidenceUnavailable,
			TerminalReasonCode: "synthetic_fixture"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Receipt.Replayed || created.Memory.ID == "" || created.Memory.Version != 1 ||
		created.Memory.ChangeSeq != 1 || len(created.Memory.Evidence) != 1 ||
		created.Memory.Evidence[0].EvidenceChangeSeq != 2 {
		t.Fatal("import contention fixture did not create nonempty memory history")
	}
	if err := st.SuspendAccountSystem(ctx, p.AccountID, "evacuation", "synthetic import contention fixture"); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := st.ExportAccount(ctx, p.AccountID, "import-contention", "test", &archive); err != nil {
		t.Fatal(err)
	}
	manifest, rows, err := readImportContentionArchive(ctx, archive.Bytes())
	if err != nil {
		t.Fatalf("import contention source archive verification failed: %v", err)
	}
	assertImportContentionManifest(t, manifest, p.AccountID)
	for _, table := range []string{"accounts", "operators", "realms", "agents", "memories", "memory_versions",
		"memory_evidence", "memory_change_clocks", "memory_curation_lanes", "memory_curation_requests", "memory_curation_mutations"} {
		if len(rows[table]) != 1 {
			t.Fatalf("import contention fixture %s must have exactly one positive row", table)
		}
	}
	f := importContentionFixture{accountID: p.AccountID, realmID: p.RealmID, agentID: p.ID,
		memoryID: created.Memory.ID, archive: archive.Bytes(), rows: rows}
	assertImportContentionBookkeeping(ctx, t, st, f)
	return f
}

func assertImportContentionBookkeeping(ctx context.Context, t *testing.T, st *Store, f importContentionFixture) {
	t.Helper()
	var clock, active, generation, queued, runs int64
	var noActiveRun bool
	if err := st.pool.QueryRow(ctx, `SELECT
		(SELECT last_change_seq FROM memory_change_clocks WHERE account_id=$1 AND owner_id=$2),
		(SELECT active_memory_count FROM memory_change_clocks WHERE account_id=$1 AND owner_id=$2),
		(SELECT request_generation FROM memory_curation_lanes WHERE account_id=$1 AND owner_id=$2),
		(SELECT active_run_id IS NULL FROM memory_curation_lanes WHERE account_id=$1 AND owner_id=$2),
		(SELECT count(*) FROM memory_curation_requests WHERE account_id=$1 AND owner_id=$2 AND state='queued'),
		(SELECT count(*) FROM memory_curation_runs WHERE account_id=$1)`,
		f.accountID, f.agentID).Scan(&clock, &active, &generation, &noActiveRun, &queued, &runs); err != nil {
		t.Fatalf("import contention memory bookkeeping read failed: %v", err)
	}
	if clock != 2 || active != 1 || generation != 1 || !noActiveRun || queued != 1 || runs != 0 {
		t.Fatal("import contention memory clocks, derived count or queued curation differ")
	}
	var memoryCount int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM memories
		WHERE id=$1 AND account_id=$2 AND realm_id=$3 AND owner_id=$4 AND current_version=1`,
		f.memoryID, f.accountID, f.realmID, f.agentID).Scan(&memoryCount); err != nil || memoryCount != 1 {
		t.Fatalf("import contention restored memory identity differs: %v", err)
	}
}

func assertImportContentionManifest(t *testing.T, m archiveexport.Manifest, accountID string) {
	t.Helper()
	if m.AccountID != accountID || m.Status != "suspended" || m.Purpose != "" || m.BackupID != "" ||
		m.EvacuationID != "" || m.FormatVersion != archiveexport.FormatVersion || m.SchemaVersion != SchemaVersion() {
		t.Fatal("import contention archive manifest identity differs")
	}
	if err := validateArchiveManifestTables(m.SchemaVersion, m.Tables); err != nil {
		t.Fatal(err)
	}
}

// UseNumber preserves int64 cursors and generations exactly. Sorting encoded
// objects compares multisets without losing duplicate rows or numeric bits.
func canonicalImportContentionRow(raw []byte) (string, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var row map[string]any
	if err := decoder.Decode(&row); err != nil {
		return "", err
	}
	if row == nil {
		return "", errors.New("import contention row is not an object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return "", errors.New("import contention row has trailing JSON")
	}
	normalized, err := json.Marshal(row)
	return string(normalized), err
}

func readImportContentionArchive(ctx context.Context, raw []byte) (archiveexport.Manifest, importContentionRows, error) {
	rows := importContentionRows{}
	m, err := archiveexport.Read(ctx, bytes.NewReader(raw), archiveexport.ImportOptions{
		CurrentSchema: SchemaVersion(),
		OnManifest: func(m archiveexport.Manifest) error {
			for _, table := range m.Tables {
				rows[table] = []string{}
			}
			return nil
		},
		Row: func(table string, raw []byte) error {
			row, err := canonicalImportContentionRow(raw)
			if err == nil {
				rows[table] = append(rows[table], row)
			}
			return err
		},
	})
	for _, tableRows := range rows {
		slices.Sort(tableRows)
	}
	return m, rows, err
}

// The durable retry/bystander snapshot keeps all SQL columns, including local
// derived projections. It is distinct from comparing two verified portable
// archives, which omit those projections by design.
func importContentionDatabaseRows(ctx context.Context, t *testing.T, st *Store, accountID string) importContentionRows {
	t.Helper()
	result := importContentionRows{}
	for _, table := range canonicalArchiveTableNamesForSchema(SchemaVersion()) {
		where := "r.account_id=$1"
		switch table {
		case "accounts":
			where = "r.id=$1"
		case "agents":
			where = "r.realm_id IN (SELECT id FROM realms WHERE account_id=$1)"
		case "agent_activity":
			where = "r.agent_id IN (SELECT a.id FROM agents a JOIN realms realm ON realm.id=a.realm_id WHERE realm.account_id=$1)"
		}
		rows, err := st.pool.Query(ctx, "SELECT to_jsonb(r) FROM "+pgx.Identifier{table}.Sanitize()+" r WHERE "+where, accountID)
		if err != nil {
			t.Fatalf("import contention snapshot %s failed: %v", table, err)
		}
		result[table] = []string{}
		for rows.Next() {
			var raw []byte
			if err := rows.Scan(&raw); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			row, err := canonicalImportContentionRow(raw)
			if err != nil {
				rows.Close()
				t.Fatal(err)
			}
			result[table] = append(result[table], row)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		slices.Sort(result[table])
	}
	return result
}

func assertImportContentionRows(t *testing.T, label string, want, got importContentionRows) {
	t.Helper()
	if !reflect.DeepEqual(want, got) {
		// Deliberately do not print complete portable account rows on failure.
		t.Fatalf("import contention: %s", label)
	}
}

func isImportContentionChecksumError(err error) bool {
	return errors.Is(err, archiveexport.ErrCorrupt) && strings.Contains(err.Error(), "does not match its checksum")
}

func corruptImportContentionTrailer(t *testing.T, original []byte) []byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(original))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	var output bytes.Buffer
	zw := gzip.NewWriter(&output)
	tw := tar.NewWriter(zw)
	trailers := 0
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		if header.Name == "checksums.json" {
			trailers++
			var sums archiveexport.Checksums
			if err := json.Unmarshal(body, &sums); err != nil {
				t.Fatal(err)
			}
			if len(sums.Chunks) == 0 || len(sums.Chunks[0].SHA256) != 64 {
				t.Fatal("import contention fixture has no checksum to corrupt")
			}
			prefix := "0"
			if sums.Chunks[0].SHA256[:1] == prefix {
				prefix = "1"
			}
			sums.Chunks[0].SHA256 = prefix + sums.Chunks[0].SHA256[1:]
			body, err = json.Marshal(sums)
			if err != nil {
				t.Fatal(err)
			}
		}
		copyHeader := *header
		copyHeader.Size = int64(len(body))
		if err := tw.WriteHeader(&copyHeader); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if trailers != 1 {
		t.Fatal("import contention fixture must have exactly one checksum trailer")
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

type importContentionOutcome struct {
	manifest archiveexport.Manifest
	err      error
}

type importContentionRun struct {
	barrier    pgx.Tx
	barrierPID int
	workers    [2]*Store
	pids       [2]int
	names      [2]string
	started    [2]bool
	cancel     [2]context.CancelFunc
	done       [2]chan struct{}
	results    [2]chan importContentionOutcome
}

func prepareImportContentionRun(ctx context.Context, t *testing.T, st *Store, accountID string) *importContentionRun {
	t.Helper()
	r := &importContentionRun{}
	triggerCreated, functionCreated := false, false
	// Registered after the parent migration fixture, and before acquiring
	// resources or starting either importer. Even t.Fatal in a semantic control
	// releases the barrier before draining callers and closing their pools.
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		clean := true
		if err := r.release(cleanupCtx); err != nil {
			t.Errorf("import contention cleanup barrier release failed: %v", err)
			clean = false
		}
		for _, cancelWorker := range r.cancel {
			if cancelWorker != nil {
				cancelWorker()
			}
		}
		for i, worker := range r.workers {
			if worker == nil {
				continue
			}
			if r.started[i] {
				select {
				case <-r.done[i]:
				case <-cleanupCtx.Done():
					t.Error("import contention cleanup importer did not drain")
					clean = false
					continue // Never wait in pool.Close on an unresolved importer.
				}
				select {
				case <-r.results[i]:
				default:
				}
			}
			worker.Close()
		}
		// A separate deadline leaves resource cleanup usable after a drain
		// deadline; any unresolved caller still makes the test fail.
		dropCtx, cancelDrop := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancelDrop()
		if triggerCreated {
			if _, err := st.pool.Exec(dropCtx, `DROP TRIGGER import_contention_pause ON accounts`); err != nil {
				t.Errorf("import contention cleanup trigger removal failed: %v", err)
				clean = false
			}
		}
		if functionCreated {
			if _, err := st.pool.Exec(dropCtx, `DROP FUNCTION import_contention_pause()`); err != nil {
				t.Errorf("import contention cleanup function removal failed: %v", err)
				clean = false
			}
		}
		ownedNames := make([]string, 0, len(r.names))
		for _, name := range r.names {
			if name != "" {
				ownedNames = append(ownedNames, name)
			}
		}
		for {
			var remaining int
			if err := st.pool.QueryRow(dropCtx, `SELECT count(*) FROM pg_stat_activity
				WHERE datname=current_database() AND application_name=ANY($1::text[])`, ownedNames).Scan(&remaining); err != nil {
				t.Errorf("import contention cleanup worker connections remain: %v", err)
				clean = false
				break
			}
			if remaining == 0 {
				break
			}
		}
		if clean && r.started[0] && r.started[1] {
			t.Log("import contention: all started importers drained")
		}
	})
	for i := range r.workers {
		cfg, err := pgxpool.ParseConfig(st.dsn)
		if err != nil {
			t.Fatal(err)
		}
		r.names[i] = fmt.Sprintf("import_contention_%s_%d", accountID, i)
		cfg.ConnConfig.RuntimeParams["application_name"] = r.names[i]
		cfg.MaxConns = 1
		cfg.MinConns = 0
		pool, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		r.workers[i] = &Store{pool: pool}
		r.done[i] = make(chan struct{})
		r.results[i] = make(chan importContentionOutcome, 1)
		if err := pool.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&r.pids[i]); err != nil {
			t.Fatal(err)
		}
	}
	var err error
	r.barrier, err = st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(accountID))
	key1, key2 := int32(binary.BigEndian.Uint32(digest[:4])), int32(binary.BigEndian.Uint32(digest[4:8]))
	if _, err := r.barrier.Exec(ctx, `SELECT pg_advisory_xact_lock($1::int, $2::int)`, key1, key2); err != nil {
		t.Fatal(err)
	}
	if err := r.barrier.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&r.barrierPID); err != nil {
		t.Fatal(err)
	}
	if r.pids[0] == r.pids[1] || r.pids[0] == r.barrierPID || r.pids[1] == r.barrierPID {
		t.Fatal("import contention requires three distinct backend identities")
	}
	if _, err := st.pool.Exec(ctx, `CREATE FUNCTION import_contention_pause() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.id = TG_ARGV[0] THEN
				PERFORM pg_advisory_xact_lock(TG_ARGV[1]::int, TG_ARGV[2]::int);
			END IF;
			RETURN NEW;
		END $$`); err != nil {
		t.Fatal(err)
	}
	functionCreated = true
	// The only literal is our generated synthetic account ID, quoted even
	// though generated IDs contain no quotes. Trigger arguments select exactly
	// that account and the separately held advisory key.
	trigger := fmt.Sprintf(`CREATE TRIGGER import_contention_pause AFTER INSERT ON accounts
		FOR EACH ROW EXECUTE FUNCTION import_contention_pause('%s', '%d', '%d')`,
		strings.ReplaceAll(accountID, "'", "''"), key1, key2)
	if _, err := st.pool.Exec(ctx, trigger); err != nil {
		t.Fatal(err)
	}
	triggerCreated = true
	return r
}

func (r *importContentionRun) start(ctx context.Context, i int, accountID string, archive []byte) {
	workerCtx, cancel := context.WithCancel(ctx)
	r.cancel[i] = cancel
	r.started[i] = true
	go func() {
		defer close(r.done[i])
		m, err := r.workers[i].ImportAccount(workerCtx, accountID, bytes.NewReader(archive))
		r.results[i] <- importContentionOutcome{manifest: m, err: err}
	}()
}

func (r *importContentionRun) release(ctx context.Context) error {
	if r.barrier == nil {
		return nil
	}
	err := r.barrier.Rollback(ctx)
	if err == nil || errors.Is(err, pgx.ErrTxClosed) {
		r.barrier = nil
		return nil
	}
	return err
}

func (r *importContentionRun) receive(ctx context.Context, t *testing.T, i int) importContentionOutcome {
	t.Helper()
	select {
	case <-r.done[i]:
		return <-r.results[i]
	case <-ctx.Done():
		t.Fatal("import contention importer did not complete before deadline")
		return importContentionOutcome{}
	}
}

func waitForImportContention(ctx context.Context, t *testing.T, st *Store, r *importContentionRun, both bool) {
	t.Helper()
	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		for i, started := range r.started {
			if started {
				select {
				case <-r.done[i]:
					t.Fatal("import contention importer completed before the observed barrier")
				default:
				}
			}
		}
		var aBlocked, bBlocked bool
		if err := st.pool.QueryRow(waitCtx, `SELECT
			EXISTS(SELECT 1 FROM pg_stat_activity a WHERE a.pid=$1 AND a.application_name=$4
				AND a.datname=current_database() AND a.wait_event_type='Lock' AND a.wait_event='advisory'
				AND $3::int=ANY(pg_blocking_pids(a.pid)) AND a.query LIKE 'INSERT INTO accounts%'),
			EXISTS(SELECT 1 FROM pg_stat_activity b WHERE b.pid=$2 AND b.application_name=$5
				AND b.datname=current_database() AND b.wait_event_type='Lock' AND b.wait_event='transactionid'
				AND $1::int=ANY(pg_blocking_pids(b.pid)) AND b.query LIKE 'INSERT INTO accounts%')`,
			r.pids[0], r.pids[1], r.barrierPID, r.names[0], r.names[1]).Scan(&aBlocked, &bBlocked); err != nil {
			t.Fatalf("import contention direct blocker observation failed: %v", err)
		}
		if aBlocked && (!both || bBlocked) {
			return
		}
		if waitCtx.Err() != nil {
			t.Fatal("import contention direct blocker observation expired")
		}
	}
}
