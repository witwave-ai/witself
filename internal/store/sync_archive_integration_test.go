package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/witwave-ai/witself/internal/export"
)

type syncArchiveFixtureSource struct {
	table string
	rows  [][]byte
}

func (s *syncArchiveFixtureSource) Table() string { return s.table }
func (s *syncArchiveFixtureSource) Next(context.Context) ([]byte, error) {
	if len(s.rows) == 0 {
		return nil, nil
	}
	row := s.rows[0]
	s.rows = s.rows[1:]
	return row, nil
}

func readSyncArchiveFixture(t *testing.T, raw []byte) (export.Manifest, map[string][][]byte) {
	t.Helper()
	rows := make(map[string][][]byte)
	m, err := export.Read(context.Background(), bytes.NewReader(raw), export.ImportOptions{
		CurrentSchema: SchemaVersion(), Row: func(table string, row []byte) error {
			rows[table] = append(rows[table], bytes.Clone(row))
			return nil
		},
	})
	if err != nil {
		t.Fatal("read fixture archive failed")
	}
	return m, rows
}

func writeSyncArchiveFixture(t *testing.T, m export.Manifest, rows map[string][][]byte) []byte {
	t.Helper()
	sources := make([]export.RowSource, 0, len(m.Tables))
	for _, table := range m.Tables {
		sources = append(sources, &syncArchiveFixtureSource{table: table, rows: slices.Clone(rows[table])})
	}
	var out bytes.Buffer
	if err := export.Write(context.Background(), &out, m, sources); err != nil {
		t.Fatal("write fixture archive failed")
	}
	return out.Bytes()
}

func syncArchiveFixture(t *testing.T, populated bool) (*Store, string, []byte) {
	t.Helper()
	// The lexical inventory is only a fast pre-check. PostgreSQL's installed
	// schema must independently match the registry before round-trip evidence.
	if err := validateSyncArchiveRegistryContract(syncMigrationsFS); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	st, dsn := newSyncSchemaTestStore(t)
	before := requireSyncSchemaCatalog(t, st.pool)
	if err := st.MigrateSync(ctx); err != nil {
		t.Fatal(err)
	}
	if err := validateSyncInstalledSchema(before, requireSyncSchemaCatalog(t, st.pool), requireDeclaredSyncSchemaProjection(t)); err != nil {
		t.Fatal(err)
	}
	const accountID = "acc_sync_archive"
	if populated {
		seedSyncSchemaRows(t, st, accountID)
	} else if _, err := st.pool.Exec(ctx, `INSERT INTO accounts(id,display_name,is_default) VALUES($1,'Sync archive',false)`, accountID); err != nil {
		t.Fatal("seed archive account failed")
	}
	if _, err := st.pool.Exec(ctx, `UPDATE accounts SET status='suspended', suspended_at=clock_timestamp(), suspended_for='operator', suspended_reason='archive test' WHERE id=$1`, accountID); err != nil {
		t.Fatal("freeze archive account failed")
	}
	var out bytes.Buffer
	if err := st.ExportAccount(ctx, accountID, "test", "test", &out); err != nil {
		t.Fatalf("export sync fixture: %v", err)
	}
	return st, dsn, out.Bytes()
}

func TestSyncArchiveEveryTableRoundTripPostgres(t *testing.T) {
	for _, populated := range []bool{false, true} {
		t.Run(fmt.Sprintf("populated_%t", populated), func(t *testing.T) {
			ctx := context.Background()
			_, dsn, raw := syncArchiveFixture(t, populated)
			m, rows := readSyncArchiveFixture(t, raw)
			if m.SyncExtension == nil || m.SyncExtension.SchemaVersion != SyncSchemaVersion() || !slices.Equal(m.SyncExtension.RequiredStreams, syncArchiveTableNamesForSchema(SyncSchemaVersion())) {
				t.Fatal("export omitted durable extension requirement")
			}
			// A new Store has no process sync configuration. Durable custody still
			// selects the extension, even when every sync stream is empty.
			reopened, err := Open(ctx, dsn)
			if err != nil {
				t.Fatal("reopen source failed")
			}
			defer reopened.Close()
			var again bytes.Buffer
			if err := reopened.ExportAccount(ctx, m.AccountID, "test", "test", &again); err != nil {
				t.Fatal("selection removal blocked safe extension export")
			}
			am, ar := readSyncArchiveFixture(t, again.Bytes())
			if !reflect.DeepEqual(am.SyncExtension, m.SyncExtension) || !reflect.DeepEqual(ar, rows) {
				t.Fatal("selection removal changed sync archive evidence")
			}
			dst, _ := newSyncSchemaTestStore(t)
			if err := dst.MigrateSync(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := dst.ImportAccount(ctx, m.AccountID, bytes.NewReader(raw)); err != nil {
				t.Fatalf("import sync fixture: %v", err)
			}
			for _, spec := range syncArchiveTables {
				t.Run(spec.name, func(t *testing.T) {
					var count int
					if err := dst.pool.QueryRow(ctx, "SELECT count(*) FROM "+pgx.Identifier{spec.name}.Sanitize()).Scan(&count); err != nil {
						t.Fatal("count imported stream failed")
					}
					want := 0
					if populated {
						want = 1
					}
					if count != want || len(rows[spec.name]) != want {
						t.Fatal("empty/populated stream did not round trip")
					}
				})
			}
			for name := range syncLocalArchiveExclusions {
				t.Run(name, func(t *testing.T) {
					var count int
					if err := dst.pool.QueryRow(ctx, "SELECT count(*) FROM "+pgx.Identifier{name}.Sanitize()).Scan(&count); err != nil || count != 0 || slices.Contains(m.Tables, name) {
						t.Fatal("archive copied live local control or scratch")
					}
				})
			}
			var exported bytes.Buffer
			if err := dst.ExportAccount(ctx, m.AccountID, "test", "test", &exported); err != nil {
				t.Fatal("re-export imported evidence failed")
			}
			_, importedRows := readSyncArchiveFixture(t, exported.Bytes())
			for _, spec := range syncArchiveTables {
				if !reflect.DeepEqual(rows[spec.name], importedRows[spec.name]) {
					t.Errorf("%s changed evidence bytes on round trip", spec.name)
				}
			}
		})
	}
}

func TestSyncArchiveNegotiationAndMalformedRowsPostgres(t *testing.T) {
	ctx := context.Background()
	_, _, raw := syncArchiveFixture(t, true)
	m, rows := readSyncArchiveFixture(t, raw)
	dst, _ := newSyncSchemaTestStore(t)
	if _, err := dst.ImportAccount(ctx, m.AccountID, bytes.NewReader(raw)); !errors.Is(err, ErrSyncArchiveRequired) {
		t.Fatal("unselected destination accepted sync archive")
	}
	if err := dst.MigrateSync(ctx); err != nil {
		t.Fatal(err)
	}
	refuse := func(t *testing.T, manifest export.Manifest, content map[string][][]byte) {
		t.Helper()
		_, err := dst.ImportAccount(ctx, m.AccountID, bytes.NewReader(writeSyncArchiveFixture(t, manifest, content)))
		if err == nil {
			t.Fatal("invalid sync archive imported")
		}
		var exists bool
		if err := dst.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM accounts WHERE id=$1)`, m.AccountID).Scan(&exists); err != nil || exists {
			t.Fatal("refused import left account state")
		}
	}
	t.Run("core_only_into_required_destination", func(t *testing.T) {
		core := m
		core.SyncExtension = nil
		core.Tables = canonicalArchiveTableNamesForSchema(SchemaVersion())
		refuse(t, core, rows)
	})
	t.Run("stripped_extension", func(t *testing.T) {
		bad := m
		bad.SyncExtension = nil
		refuse(t, bad, rows)
	})
	for _, field := range []string{"version", "schema"} {
		t.Run("unsupported_"+field, func(t *testing.T) {
			bad := m
			ext := *m.SyncExtension
			bad.SyncExtension = &ext
			if field == "version" {
				ext.Version++
			} else {
				ext.SchemaVersion++
			}
			refuse(t, bad, rows)
		})
	}
	for _, spec := range syncArchiveTables {
		t.Run(spec.name, func(t *testing.T) {
			t.Run("omitted_stream", func(t *testing.T) {
				bad := m
				bad.Tables = withoutArchiveTable(m.Tables, spec.name)
				ext := *m.SyncExtension
				ext.RequiredStreams = withoutArchiveTable(ext.RequiredStreams, spec.name)
				bad.SyncExtension = &ext
				refuse(t, bad, rows)
			})
			for _, mutation := range []string{"cross_account", "unknown_column", "missing_column", "null_account"} {
				t.Run(mutation, func(t *testing.T) {
					obj, err := decodeImportRow(rows[spec.name][0])
					if err != nil {
						t.Fatal("decode row failed")
					}
					switch mutation {
					case "cross_account":
						obj["account_id"] = "acc_other"
					case "unknown_column":
						obj["unreviewed"] = true
					case "missing_column":
						delete(obj, spec.columns[len(spec.columns)-1])
					case "null_account":
						obj["account_id"] = nil
					}
					encoded, err := json.Marshal(obj)
					if err != nil {
						t.Fatal(err)
					}
					badRows := make(map[string][][]byte, len(rows))
					for name, rs := range rows {
						badRows[name] = rs
					}
					badRows[spec.name] = [][]byte{encoded}
					refuse(t, m, badRows)
				})
			}
		})
	}
	t.Run("unknown_stream", func(t *testing.T) {
		bad := m
		bad.Tables = append(slices.Clone(m.Tables), "sync_unknown")
		ext := *m.SyncExtension
		ext.RequiredStreams = append(slices.Clone(ext.RequiredStreams), "sync_unknown")
		bad.SyncExtension = &ext
		refuse(t, bad, rows)
	})
	t.Run("legacy_importer_boundary", func(t *testing.T) {
		if !errors.Is(validateArchiveManifestTables(m.SchemaVersion, m.Tables), ErrArchiveContent) {
			t.Fatal("old core importer contract accepted sync archive")
		}
	})
	if _, err := dst.ImportAccount(ctx, m.AccountID, bytes.NewReader(raw)); err != nil {
		t.Fatalf("valid archive refused after negative matrix: %v", err)
	}
}

// Also executed with HEAD's unmodified import.go through a source overlay. That
// binary exercises the previous importer against the new declared streams; it
// must refuse at its existing canonical table-membership boundary.
func TestSyncArchiveLegacyImporterRefusesPostgres(t *testing.T) {
	_, _, raw := syncArchiveFixture(t, false)
	dst, _ := newSyncSchemaTestStore(t)
	_, err := dst.ImportAccount(context.Background(), "acc_sync_archive", bytes.NewReader(raw))
	if !errors.Is(err, ErrSyncArchiveRequired) && !errors.Is(err, ErrArchiveContent) {
		t.Fatal("core-only importer did not refuse sync archive")
	}
	var count int
	if err := dst.pool.QueryRow(context.Background(), `SELECT count(*) FROM accounts`).Scan(&count); err != nil || count != 0 {
		t.Fatal("core-only importer left account state")
	}
}

func TestSyncArchiveCompositeByteaPaginationPostgres(t *testing.T) {
	ctx := context.Background()
	src, _, _ := syncArchiveFixture(t, true)
	const accountID = "acc_sync_archive"
	if _, err := src.pool.Exec(ctx, `INSERT INTO sync_incarnations
		(account_id,lifecycle_epoch,restore_marker,allocation_id,transition_id,authority_installation,state)
		SELECT $1,2,decode(lpad(to_hex(n),32,'0'),'hex'),decode(lpad(to_hex(n),32,'0'),'hex'),
		decode(lpad(to_hex(n),32,'0'),'hex'),'installation','allocated'
		FROM generate_series(1,270) n`, accountID); err != nil {
		t.Fatal("seed paginated evidence failed")
	}
	var archive bytes.Buffer
	if err := src.ExportAccount(ctx, accountID, "test", "test", &archive); err != nil {
		t.Fatal("paginated export failed")
	}
	_, rows := readSyncArchiveFixture(t, archive.Bytes())
	if len(rows["sync_incarnations"]) != 271 {
		t.Fatal("composite bytea pagination omitted or repeated rows")
	}
	dst, _ := newSyncSchemaTestStore(t)
	if err := dst.MigrateSync(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := dst.ImportAccount(ctx, accountID, bytes.NewReader(archive.Bytes())); err != nil {
		t.Fatalf("paginated import failed: %v", err)
	}
	var again bytes.Buffer
	if err := dst.ExportAccount(ctx, accountID, "test", "test", &again); err != nil {
		t.Fatal("paginated re-export failed")
	}
	_, imported := readSyncArchiveFixture(t, again.Bytes())
	if !reflect.DeepEqual(rows["sync_incarnations"], imported["sync_incarnations"]) {
		t.Fatal("paginated evidence order or bytes changed")
	}
}

func TestSyncArchiveBackupEvacuationAndRetryPostgres(t *testing.T) {
	ctx := context.Background()
	src, _, _ := syncArchiveFixture(t, true)
	if _, err := src.pool.Exec(ctx, `INSERT INTO sync_retained_evidence VALUES('acc_sync_archive','identity','extra','2026-03-01'::timestamptz)`); err != nil {
		t.Fatal("seed second retry evidence row failed")
	}
	var original bytes.Buffer
	if err := src.ExportAccount(ctx, "acc_sync_archive", "test", "test", &original); err != nil {
		t.Fatal("export retry fixture failed")
	}
	m, originalRows := readSyncArchiveFixture(t, original.Bytes())
	dst, _ := newSyncSchemaTestStore(t)
	if err := dst.MigrateSync(ctx); err != nil {
		t.Fatal(err)
	}
	var backup bytes.Buffer
	if err := src.ExportAccountBackup(ctx, m.AccountID, "backup_sync_test", "test", "test", &backup); err != nil {
		t.Fatal("sync backup export failed")
	}
	if _, err := dst.ValidateAccountBackup(ctx, m.AccountID, "backup_sync_test", bytes.NewReader(backup.Bytes())); err != nil {
		t.Fatalf("sync backup validation failed: %v", err)
	}
	var exists bool
	if err := dst.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM accounts WHERE id=$1)`, m.AccountID).Scan(&exists); err != nil || exists {
		t.Fatal("backup validation committed account state")
	}
	var self bytes.Buffer
	if err := src.ExportAccountSelf(ctx, m.AccountID, "test", "test", &self); err != nil {
		t.Fatal("sync self export failed")
	}
	selfManifest, selfRows := readSyncArchiveFixture(t, self.Bytes())
	if selfManifest.SyncExtension == nil || !reflect.DeepEqual(selfRows, originalRows) {
		t.Fatal("self snapshot omitted evidence")
	}
	const evacuationID = "evac_sync_archive"
	if _, err := src.BeginAccountEvacuation(ctx, m.AccountID, evacuationID, "test"); err != nil {
		t.Fatal("begin sync evidence evacuation failed")
	}
	var moved bytes.Buffer
	if err := src.ExportAccountEvacuation(ctx, m.AccountID, evacuationID, "test", "test", &moved); err != nil {
		t.Fatal("sync evacuation export failed")
	}
	for attempt := 0; attempt < 2; attempt++ {
		_, disposition, err := dst.ImportAccountEvacuation(ctx, m.AccountID, evacuationID, bytes.NewReader(moved.Bytes()))
		if err != nil || disposition.AlreadyImported != (attempt == 1) {
			t.Fatalf("sync evacuation import/retry failed: %v", err)
		}
	}
	moveManifest, moveRows := readSyncArchiveFixture(t, moved.Bytes())
	for _, spec := range syncArchiveTables {
		for _, mutation := range []string{"omitted_rows", "duplicate_row"} {
			t.Run(spec.name+"/"+mutation, func(t *testing.T) {
				badRows := make(map[string][][]byte, len(moveRows))
				for table, rows := range moveRows {
					badRows[table] = rows
				}
				badRows[spec.name] = nil
				if mutation == "duplicate_row" {
					// retained_evidence has two rows: this replacement keeps the
					// count unchanged, so only strict PK ordering catches it.
					badRows[spec.name] = [][]byte{moveRows[spec.name][0], moveRows[spec.name][0]}
				}
				bad := writeSyncArchiveFixture(t, moveManifest, badRows)
				for _, target := range []*Store{dst, src} {
					if _, _, err := target.ImportAccountEvacuation(ctx, m.AccountID, evacuationID, bytes.NewReader(bad)); !errors.Is(err, ErrArchiveContent) {
						t.Fatal("retry or source promotion accepted incomplete/duplicated sync evidence")
					}
				}
				var role string
				if err := src.pool.QueryRow(ctx, `SELECT evacuation_role FROM accounts WHERE id=$1`, m.AccountID).Scan(&role); err != nil || role != "source" {
					t.Fatal("invalid retry promoted source authority")
				}
			})
		}
	}
	obj, err := decodeImportRow(moveRows["sync_receipts"][0])
	if err != nil {
		t.Fatal("decode retry receipt failed")
	}
	obj["canonical_body"] = `\x02`
	changed, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	moveRows["sync_receipts"] = [][]byte{changed}
	if _, _, err := dst.ImportAccountEvacuation(ctx, m.AccountID, evacuationID, bytes.NewReader(writeSyncArchiveFixture(t, moveManifest, moveRows))); !errors.Is(err, ErrArchiveContent) {
		t.Fatal("retry accepted changed immutable evidence")
	}
	if _, err := src.FinalizeAccountEvacuationSource(ctx, m.AccountID, evacuationID); !errors.Is(err, ErrSyncDestructionUnsupported) {
		t.Fatal("inert imported evidence authorized source finalization")
	}
}

func TestSyncFinalizationAndPurgeRefusePostgres(t *testing.T) {
	ctx := context.Background()
	for _, populated := range []bool{false, true} {
		t.Run(fmt.Sprintf("populated_%t", populated), func(t *testing.T) {
			st, dsn, _ := syncArchiveFixture(t, populated)
			// A newly opened store is deliberately unaware of runtime selection.
			reopened, err := Open(ctx, dsn)
			if err != nil {
				t.Fatal("reopen failed")
			}
			defer reopened.Close()
			const accountID = "acc_sync_archive"
			if _, err := st.pool.Exec(ctx, `UPDATE accounts SET status='closed', closed_at=clock_timestamp()-interval '100 days', closed_reason='test' WHERE id=$1`, accountID); err != nil {
				t.Fatal("close fixture failed")
			}
			for _, enforce := range []bool{false, true} {
				if _, err := reopened.processAccountPurgeCandidate(ctx, accountID, 30*24*time.Hour, enforce); !errors.Is(err, ErrSyncDestructionUnsupported) {
					t.Fatal("sync purge/preview did not refuse")
				}
			}
			if _, err := reopened.BeginAccountEvacuation(ctx, accountID, "evac_sync_refusal", "test"); err != nil {
				t.Fatal("begin evacuation failed")
			}
			if _, err := reopened.FinalizeAccountEvacuationSource(ctx, accountID, "evac_sync_refusal"); !errors.Is(err, ErrSyncDestructionUnsupported) {
				t.Fatal("sync source finalization did not refuse")
			}
			var exists bool
			if err := st.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM accounts WHERE id=$1 AND purged_at IS NULL)`, accountID).Scan(&exists); err != nil || !exists {
				t.Fatal("destructive refusal lost account")
			}
			for _, spec := range syncArchiveTables {
				var count int
				if err := st.pool.QueryRow(ctx, "SELECT count(*) FROM "+pgx.Identifier{spec.name}.Sanitize()).Scan(&count); err != nil {
					t.Fatal("read evidence after refusal failed")
				}
				want := 0
				if populated {
					want = 1
				}
				if count != want {
					t.Errorf("refusal changed %s", spec.name)
				}
			}
		})
	}
}
