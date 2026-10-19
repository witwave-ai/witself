// Command migrationrehearsal exercises embedded store migrations against a
// disposable, explicitly addressed loopback database. Its output is a small,
// value-free protocol consumed by scripts/rehearse-store-migrations.sh.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net"
	"net/url"
	"os"
	"os/signal"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/witwave-ai/witself/internal/gitopsvalues"
	"github.com/witwave-ai/witself/internal/store"
)

const (
	migratorApplication = "witself_rehearsal_migrator"
	appenderApplication = "witself_rehearsal_appender"
)

var (
	errLocalDSN     = errors.New("DSN must explicitly address one literal loopback host and a non-reserved port")
	errPasswordFile = errors.New("password_file")
)

type migrationFile struct {
	Version       int64  `json:"version"`
	Name          string `json:"name"`
	ExplicitLocks bool   `json:"explicit_locks"`
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		// Driver and Goose errors may contain SQL, data, paths or connection values.
		// Deliberately expose only this closed diagnostic vocabulary.
		_, _ = fmt.Fprintln(os.Stderr, safeDiagnostic(err))
		os.Exit(1)
	}
}

// SQLSTATE is a five-character protocol code, never a server message or value.
func safeDiagnostic(err error) string {
	const prefix = "migration rehearsal helper failed: helper_error"
	if errors.Is(err, errPasswordFile) {
		return prefix + " password_file"
	}
	if match := regexp.MustCompile(`^synthetic generation failed: ([a-z ]{1,64})(?: \(SQLSTATE ([A-Z0-9]{5})\))?$`).FindStringSubmatch(err.Error()); match != nil {
		diagnostic := prefix + " seed_phase=" + strings.ReplaceAll(match[1], " ", "_")
		if match[2] != "" {
			diagnostic += " sqlstate=" + match[2]
		}
		return diagnostic
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && regexp.MustCompile(`^[A-Z0-9]{5}$`).MatchString(pgErr.Code) {
		return prefix + " sqlstate=" + pgErr.Code
	}
	return prefix
}

func run(ctx context.Context, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("migrationrehearsal", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	mode := flags.String("mode", "", "operation")
	dsn := flags.String("dsn", "", "explicit loopback database URL")
	passwordFile := flags.String("password-file", "", "private regular file containing the connection password")
	to := flags.Int64("to", 0, "target version; zero selects latest")
	appendRate := flags.Float64("append-rate", 1, "100-entry batches per second")
	longTX := flags.Float64("long-tx", 10, "repeatable read duration in seconds")
	verifySQL := flags.String("verify-sql", "", "local boolean verification query")
	values := flags.String("values", "", "generated cell values file")
	var seed seedOptions
	addSeedFlags(flags, &seed)
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return errors.New("invalid arguments")
	}
	if *mode == "image" {
		pins, err := gitopsvalues.ReadPostgresImagePins(*values)
		if err != nil || pins == nil || pins.Digest == "" {
			return errors.New("pinned PostgreSQL image unavailable")
		}
		return json.NewEncoder(out).Encode(struct {
			Image string `json:"image"`
		}{pins.Registry + "/" + pins.Repository + "@" + pins.Digest})
	}
	files, err := migrationFiles(store.MigrationFiles())
	if err != nil {
		return err
	}
	if *mode == "list" {
		return json.NewEncoder(out).Encode(files)
	}
	config, err := passwordConfig(*dsn, *passwordFile)
	if err != nil {
		return err
	}
	switch *mode {
	case "migrate":
		config.RuntimeParams["application_name"] = migratorApplication
	case "append":
		config.RuntimeParams["application_name"] = appenderApplication
	}
	db := stdlib.OpenDB(*config)
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(2)
	switch *mode {
	case "migrate":
		return migrate(ctx, db, files, *to, out)
	case "observe":
		return observe(ctx, db, out)
	case "append":
		return appendEntries(ctx, db, *appendRate, out)
	case "long-tx":
		return longTransaction(ctx, db, *longTX, out)
	case "verify":
		return verify(ctx, db, *verifySQL)
	case "inspect":
		return inspect(ctx, db, out)
	case "seed":
		metrics, seedErr := seedSynthetic(ctx, db, seed)
		if err := json.NewEncoder(out).Encode(metrics); err != nil {
			return err
		}
		return seedErr
	case "measure":
		metrics, measureErr := measureSynthetic(ctx, db, seed)
		if err := json.NewEncoder(out).Encode(metrics); err != nil {
			return err
		}
		return measureErr
	default:
		return errors.New("unknown mode")
	}
}

// passwordConfig keeps the password out of the command line and environment.
// O_NOFOLLOW closes the symlink race after Lstat, and O_NONBLOCK prevents a
// swapped special file from blocking before its descriptor can be checked.
func passwordConfig(dsn, filename string) (*pgx.ConnConfig, error) {
	info, err := os.Lstat(filename)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 256 {
		return nil, errPasswordFile
	}
	file, err := os.OpenFile(filename, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errPasswordFile
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() || opened.Mode().Perm()&0077 != 0 || opened.Size() > 256 {
		return nil, errPasswordFile
	}
	password, err := io.ReadAll(io.LimitReader(file, 256))
	if err != nil || int64(len(password)) != opened.Size() {
		return nil, errPasswordFile
	}
	after, err := file.Stat()
	if err != nil || after.Size() != opened.Size() || after.Mode().Perm()&0077 != 0 {
		return nil, errPasswordFile
	}
	u, err := url.Parse(dsn)
	if err != nil || u.User == nil {
		return nil, errLocalDSN
	}
	u.User = url.UserPassword(u.User.Username(), strings.TrimSuffix(string(password), "\n"))
	return localConfig(u.String())
}

// localConfig rejects alternate routing, DNS, sockets and reserved test-server
// endpoints before pgx can inspect any connection-related files. Only sslmode
// disable is accepted as a URL query. Canonical parser inputs override every
// file-bearing libpq default; ambient PGOPTIONS are discarded afterward.
func localConfig(dsn string) (*pgx.ConnConfig, error) {
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Opaque != "" || u.Fragment != "" || u.User == nil || u.User.Username() == "" {
		return nil, errLocalDSN
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	port, portErr := strconv.ParseUint(u.Port(), 10, 16)
	if ip == nil || !ip.IsLoopback() || portErr != nil || port == 0 || port == 5599 || strings.Contains(u.Host, ",") || u.Path == "" || u.Path == "/" {
		return nil, errLocalDSN
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return nil, errLocalDSN
	}
	for key, vals := range query {
		if key != "sslmode" || len(vals) != 1 || vals[0] != "disable" {
			return nil, errLocalDSN
		}
	}
	query = url.Values{
		"sslmode": {"disable"}, "servicefile": {"/dev/null"},
		"passfile": {"/dev/null"}, "sslkey": {""}, "sslcert": {""}, "sslrootcert": {""},
		"sslpassword": {""}, "connect_timeout": {"5"}, "target_session_attrs": {"any"},
		"channel_binding": {"disable"}, "require_auth": {""}, "min_protocol_version": {"3.0"},
		"max_protocol_version": {"latest"}, "sslnegotiation": {"postgres"},
	}
	password, _ := u.User.Password()
	query.Set("password", password)
	u.User = url.UserPassword(u.User.Username(), password)
	u.RawQuery = query.Encode()
	allowed := []string{"host", "port", "user", "database"}
	for key := range query {
		allowed = append(allowed, key)
	}
	// pgx treats even an empty service parameter as a request to read a
	// service file. This standalone tool must never use ambient discovery.
	if err := os.Unsetenv("PGSERVICE"); err != nil {
		return nil, errLocalDSN
	}
	config, err := pgx.ParseConfigWithOptions(u.String(), pgx.ParseConfigOptions{ParseConfigOptions: pgconn.ParseConfigOptions{ConnStringAllowedKeys: allowed}})
	if err != nil {
		return nil, errLocalDSN
	}
	if config.Host != host || config.Port != uint16(port) || len(config.Fallbacks) != 0 || config.TLSConfig != nil {
		return nil, errLocalDSN
	}
	config.RuntimeParams = map[string]string{"application_name": "witself_rehearsal_helper"}
	return config, nil
}

func migrationFiles(source fs.FS) ([]migrationFile, error) {
	entries, err := fs.ReadDir(source, "migrations")
	if err != nil {
		return nil, err
	}
	files := make([]migrationFile, 0, len(entries))
	explicitLock := regexp.MustCompile(`(?im)^\s*LOCK\s+(?:TABLE\s+)?[a-z_"]`)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		prefix, _, ok := strings.Cut(entry.Name(), "_")
		version, err := strconv.ParseInt(prefix, 10, 64)
		if !ok || err != nil || version <= 0 {
			return nil, errors.New("invalid embedded migration name")
		}
		body, err := fs.ReadFile(source, path.Join("migrations", entry.Name()))
		if err != nil {
			return nil, err
		}
		up, _, _ := strings.Cut(string(body), "-- +goose Down")
		files = append(files, migrationFile{version, entry.Name(), explicitLock.MatchString(up)})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Version < files[j].Version })
	for i := 1; i < len(files); i++ {
		if files[i].Version == files[i-1].Version {
			return nil, errors.New("duplicate embedded migration version")
		}
	}
	return files, nil
}

func versionRange(files []migrationFile, current, target int64) ([]migrationFile, error) {
	if len(files) == 0 || current < 0 || target < 0 {
		return nil, errors.New("invalid version range")
	}
	latest := files[len(files)-1].Version
	if target == 0 {
		target = latest
	}
	if current > target || target > latest {
		return nil, errors.New("invalid version range")
	}
	foundCurrent, foundTarget := current == 0, false
	var selected []migrationFile
	for _, file := range files {
		foundCurrent = foundCurrent || file.Version == current
		foundTarget = foundTarget || file.Version == target
		if file.Version > current && file.Version <= target {
			selected = append(selected, file)
		}
	}
	if !foundCurrent || !foundTarget {
		return nil, errors.New("unknown version")
	}
	return selected, nil
}

func writeTiming(out io.Writer, file migrationFile, elapsed time.Duration) error {
	_, err := fmt.Fprintf(out, "FILE %s %d\n", file.Name, elapsed.Milliseconds())
	return err
}

func configureGoose() error {
	goose.SetLogger(goose.NopLogger())
	goose.SetBaseFS(store.MigrationFiles())
	return goose.SetDialect("postgres")
}

func migrate(ctx context.Context, db *sql.DB, files []migrationFile, target int64, out io.Writer) error {
	if err := configureGoose(); err != nil {
		return err
	}
	// Mirror the store startup session lock without retaining a transaction that
	// would prevent concurrent-index migrations from completing.
	lock, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	for {
		var acquired bool
		err := lock.QueryRowContext(ctx, `SELECT pg_try_advisory_lock(hashtextextended(current_database() || ':witself:store:migrate:v1', 0))`).Scan(&acquired)
		if err != nil {
			return err
		}
		if acquired {
			break
		}
		if err := waitContext(ctx, 100*time.Millisecond); err != nil {
			return err
		}
	}
	defer func() {
		// Closing sql.Conn alone returns it to the pool, so release explicitly.
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = lock.ExecContext(releaseCtx, `SELECT pg_advisory_unlock(hashtextextended(current_database() || ':witself:store:migrate:v1', 0))`)
	}()
	current, err := goose.GetDBVersionContext(ctx, db)
	if err != nil {
		return err
	}
	pending, err := versionRange(files, current, target)
	if err != nil {
		return err
	}
	for _, file := range pending {
		if _, err := fmt.Fprintf(out, "START %s\n", file.Name); err != nil {
			return err
		}
		start := time.Now()
		migrationErr := goose.UpToContext(ctx, db, "migrations", file.Version)
		if err := writeTiming(out, file, time.Since(start)); err != nil {
			return err
		}
		if migrationErr != nil {
			return migrationErr
		}
		version, err := goose.GetDBVersionContext(ctx, db)
		if err != nil || version != file.Version {
			return errors.New("migration version did not converge")
		}
	}
	return nil
}

type observation struct {
	Ready           bool `json:"ready"`
	LockHeld        bool `json:"lock_held"`
	LockWaiting     bool `json:"lock_waiting"`
	BackendCount    int  `json:"backend_count"`
	AppenderWaiting bool `json:"appender_waiting"`
}

func observe(ctx context.Context, db *sql.DB, out io.Writer) error {
	encoder := json.NewEncoder(out)
	for {
		var sample observation
		err := db.QueryRowContext(ctx, `SELECT
   EXISTS (SELECT 1 FROM pg_locks l JOIN pg_stat_activity a ON a.pid=l.pid
    WHERE a.datname=current_database() AND a.application_name=$1 AND l.granted
    AND l.locktype='relation' AND l.mode IN ('ShareLock','ShareRowExclusiveLock','ExclusiveLock','AccessExclusiveLock')),
   EXISTS (SELECT 1 FROM pg_locks l JOIN pg_stat_activity a ON a.pid=l.pid
    WHERE a.datname=current_database() AND a.application_name=$1 AND NOT l.granted
    AND l.locktype='relation' AND l.mode IN ('ShareLock','ShareRowExclusiveLock','ExclusiveLock','AccessExclusiveLock')),
   (SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND application_name=$1),
   EXISTS (SELECT 1 FROM pg_stat_activity WHERE datname=current_database()
    AND application_name=$2 AND wait_event_type='Lock')`, migratorApplication, appenderApplication).Scan(&sample.LockHeld, &sample.LockWaiting, &sample.BackendCount, &sample.AppenderWaiting)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		sample.Ready = true
		if err := encoder.Encode(sample); err != nil {
			return err
		}
		if err := waitContext(ctx, 200*time.Millisecond); err != nil {
			return nil
		}
	}
}

func appendEntries(ctx context.Context, db *sql.DB, rate float64, out io.Writer) error {
	if math.IsNaN(rate) || math.IsInf(rate, 0) || rate <= 0 || rate > 10000 {
		return errors.New("invalid append rate")
	}
	interval := time.Duration(float64(time.Second) / rate)
	encoder := json.NewEncoder(out)
	for batch := int64(0); ; batch++ {
		start := time.Now()
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		// Allocate sequences by locking the parent as the application's appender
		// does. Existing parent rows also keep all four foreign-key checks active.
		result, err := tx.ExecContext(ctx, `WITH parent AS (
    UPDATE transcript_conversations SET next_sequence=GREATEST(next_sequence,
      COALESCE((SELECT max(sequence)+1 FROM transcript_entries WHERE transcript_id=transcript_conversations.id),1))+100,
      updated_at=now()
    WHERE id=(SELECT id FROM transcript_conversations ORDER BY id LIMIT 1)
    RETURNING id,account_id,realm_id,owner_agent_id,next_sequence-100 AS first_sequence
   ) INSERT INTO transcript_entries
    (id,account_id,transcript_id,realm_id,recorded_by_agent_id,sequence,role,body,artifacts)
   SELECT 'rehearsal_append_' || pg_backend_pid() || '_' || $1::text || '_' || s::text,
    p.account_id,p.id,p.realm_id,p.owner_agent_id,p.first_sequence+s,'user',repeat('x',1024),'[]'::jsonb
   FROM parent p CROSS JOIN generate_series(0,99) s`, strconv.FormatInt(batch, 10))
		if err == nil {
			var inserted int64
			inserted, err = result.RowsAffected()
			if err == nil && inserted != 100 {
				err = errors.New("appender requires a transcript parent")
			}
		}
		if err == nil {
			err = tx.Commit()
		} else {
			_ = tx.Rollback()
		}
		elapsed := time.Since(start)
		if outputErr := encoder.Encode(struct {
			Ready  bool    `json:"ready"`
			WaitMS float64 `json:"wait_ms"`
		}{err == nil, float64(elapsed) / float64(time.Millisecond)}); outputErr != nil {
			return outputErr
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if remaining := interval - elapsed; remaining > 0 {
			if err := waitContext(ctx, remaining); err != nil {
				return nil
			}
		}
	}
}

func longTransaction(ctx context.Context, db *sql.DB, seconds float64, out io.Writer) error {
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds <= 0 || seconds > 86400 {
		return errors.New("invalid transaction duration")
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var present bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM transcript_entries LIMIT 1)`).Scan(&present); err != nil {
		return err
	}
	if err := json.NewEncoder(out).Encode(struct {
		Ready bool `json:"ready"`
	}{true}); err != nil {
		return err
	}
	if err := waitContext(ctx, time.Duration(seconds*float64(time.Second))); err != nil {
		return nil
	}
	return tx.Commit()
}

func verify(ctx context.Context, db *sql.DB, filename string) error {
	query, err := os.ReadFile(filename)
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, string(query))
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	columns, err := rows.Columns()
	if err != nil || len(columns) != 1 || !rows.Next() {
		return errors.New("verification requires one boolean result")
	}
	var result any
	if err := rows.Scan(&result); err != nil {
		return err
	}
	valid, ok := result.(bool)
	if !ok || !valid || rows.Next() {
		return errors.New("verification requires one true result")
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if rows.NextResultSet() {
		return errors.New("verification requires one result set")
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return rows.Close()
}

func inspect(ctx context.Context, db *sql.DB, out io.Writer) error {
	if err := configureGoose(); err != nil {
		return err
	}
	version, err := goose.GetDBVersionContext(ctx, db)
	if err != nil {
		return err
	}
	var invalid int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_index WHERE NOT indisvalid`).Scan(&invalid); err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(struct {
		Version        int64 `json:"version"`
		InvalidIndexes int   `json:"invalid_indexes"`
	}{version, invalid})
}

func waitContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
