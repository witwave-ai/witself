package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/witwave-ai/witself/internal/export"
)

// ErrSyncArchiveRequired refuses unsupported, absent, or incomplete sync
// evidence. The durable namespace, never a process flag, selects this boundary.
var ErrSyncArchiveRequired = errors.New("sync archive extension is required and must be supported")

// ErrSyncDestructionUnsupported protects portable evidence until movement and
// recovery supply successor publication, retained floors, and terminal denial.
var ErrSyncDestructionUnsupported = errors.New("sync evidence prevents destructive finalization or purge")

const syncArchiveExtensionVersion = 1

func exportSyncExtension(ctx context.Context, tx pgx.Tx) (*export.SyncExtension, error) {
	version, err := appliedSyncSchemaVersion(ctx, tx)
	if err != nil {
		return nil, err
	}
	if version == 0 {
		return nil, nil
	}
	if version != SyncSchemaVersion() {
		return nil, ErrSyncArchiveRequired
	}
	return &export.SyncExtension{
		Version: syncArchiveExtensionVersion, SchemaVersion: version,
		RequiredStreams: syncArchiveTableNamesForSchema(version),
	}, nil
}

func validateSyncImportManifest(ctx context.Context, tx pgx.Tx, m export.Manifest) error {
	installed, err := appliedSyncSchemaVersion(ctx, tx)
	if err != nil {
		return err
	}
	if m.SyncExtension == nil {
		if installed != 0 {
			return ErrSyncArchiveRequired
		}
		return validateArchiveManifestTables(m.SchemaVersion, m.Tables)
	}
	ext := m.SyncExtension
	if ext.Version != syncArchiveExtensionVersion || ext.SchemaVersion != SyncSchemaVersion() || installed != ext.SchemaVersion {
		return ErrSyncArchiveRequired
	}
	if err := validateSyncArchiveManifestTables(ext.SchemaVersion, ext.RequiredStreams); err != nil {
		return err
	}
	// Sync streams follow the unchanged core streams in dependency order. This
	// also makes old core-only importers refuse the new manifest's extra tables.
	count := len(ext.RequiredStreams)
	if len(m.Tables) < count || !slices.Equal(m.Tables[len(m.Tables)-count:], ext.RequiredStreams) {
		return ErrSyncArchiveRequired
	}
	return validateArchiveManifestTables(m.SchemaVersion, m.Tables[:len(m.Tables)-count])
}

func refuseSyncDestruction(ctx context.Context, tx pgx.Tx) error {
	version, err := appliedSyncSchemaVersion(ctx, tx)
	if err != nil {
		return err
	}
	if version != 0 {
		return ErrSyncDestructionUnsupported
	}
	return nil
}

// importSyncRow uses the same closed registry as the exporter. PostgreSQL
// applies the actual column types, account-scoped FKs, closed pairs and floors;
// a malformed row aborts the enclosing archive transaction before any commit.
// Import does not install capture controls, writer capabilities or live leases.
func importSyncRow(ctx context.Context, tx pgx.Tx, spec syncArchiveTable, accountID string, row []byte, alreadyImported bool, retry *syncImportStreamState) error {
	obj, err := decodeImportRow(row)
	if err != nil {
		return fmt.Errorf("%w: invalid sync row", ErrArchiveContent)
	}
	if err := validateSyncArchiveRow(spec, obj, accountID); err != nil {
		return err
	}
	columns := make([]string, len(spec.columns))
	for i, column := range spec.columns {
		columns[i] = pgx.Identifier{column}.Sanitize()
	}
	// Marshal the number-preserving object only after duplicate-name rejection.
	encoded, err := json.Marshal(obj)
	if err != nil {
		return fmt.Errorf("%w: invalid sync row", ErrArchiveContent)
	}
	table := pgx.Identifier{spec.name}.Sanitize()
	if alreadyImported {
		return compareSyncRetryRow(ctx, tx, spec, obj, encoded, retry)
	}
	projection := strings.Join(columns, ", ")
	_, err = tx.Exec(ctx, "INSERT INTO "+table+" ("+projection+") SELECT "+projection+" FROM jsonb_populate_record(NULL::"+table+", $1::jsonb)", string(encoded))
	if err != nil {
		return fmt.Errorf("%w: sync row violates schema", ErrArchiveContent)
	}
	return nil
}

func validateSyncArchiveRow(spec syncArchiveTable, obj map[string]any, accountID string) error {
	if len(obj) != len(spec.columns) || obj["account_id"] != accountID {
		return fmt.Errorf("%w: invalid sync row", ErrArchiveContent)
	}
	for _, column := range spec.columns {
		if _, present := obj[column]; !present {
			return fmt.Errorf("%w: invalid sync row columns", ErrArchiveContent)
		}
	}
	return nil
}

// A retry retains one primary key and one count per stream, not the archive.
// Typed PostgreSQL ordering detects duplicate or reordered representations of
// the same bytea/integer key; counts also cover declared-but-empty streams.
type syncImportStreamState struct {
	lastKey []byte
	rows    int64
}

func compareSyncRetryRow(ctx context.Context, tx pgx.Tx, spec syncArchiveTable, obj map[string]any, encoded []byte, state *syncImportStreamState) error {
	table := pgx.Identifier{spec.name}.Sanitize()
	predicates := make([]string, len(spec.columns))
	for i, name := range spec.columns {
		column := pgx.Identifier{name}.Sanitize()
		predicates[i] = "existing." + column + " IS NOT DISTINCT FROM incoming." + column
	}
	query := "SELECT EXISTS (SELECT 1 FROM " + table + " existing, jsonb_populate_record(NULL::" + table + ", $1::jsonb) incoming"
	args := []any{string(encoded)}
	if state.lastKey != nil {
		query += ", jsonb_populate_record(NULL::" + table + ", $2::jsonb) previous"
		args = append(args, string(state.lastKey))
		left, right := make([]string, len(spec.orderBy)), make([]string, len(spec.orderBy))
		for i, name := range spec.orderBy {
			column := pgx.Identifier{name}.Sanitize()
			left[i], right[i] = "incoming."+column, "previous."+column
		}
		predicates = append(predicates, "ROW("+strings.Join(left, ",")+") > ROW("+strings.Join(right, ",")+")")
	}
	query += " WHERE " + strings.Join(predicates, " AND ") + ")"
	var matches bool
	if err := tx.QueryRow(ctx, query, args...).Scan(&matches); err != nil || !matches {
		return fmt.Errorf("%w: sync retry evidence differs", ErrArchiveContent)
	}
	key := make(map[string]any, len(spec.orderBy))
	for _, name := range spec.orderBy {
		key[name] = obj[name]
	}
	lastKey, err := json.Marshal(key)
	if err != nil {
		return fmt.Errorf("%w: invalid sync retry key", ErrArchiveContent)
	}
	state.lastKey = lastKey
	state.rows++
	return nil
}

func validateSyncRetryCompleteness(ctx context.Context, tx pgx.Tx, accountID string, streams map[string]*syncImportStreamState) error {
	for _, spec := range syncArchiveTables {
		var expected int64
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM "+pgx.Identifier{spec.name}.Sanitize()+" WHERE account_id=$1", accountID).Scan(&expected); err != nil {
			return fmt.Errorf("%w: sync retry evidence unavailable", ErrArchiveContent)
		}
		var received int64
		if stream := streams[spec.name]; stream != nil {
			received = stream.rows
		}
		if expected != received {
			return fmt.Errorf("%w: incomplete sync retry evidence", ErrArchiveContent)
		}
	}
	return nil
}
