package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// AccountImportState is the database-observed import disposition.
type AccountImportState string

// Database-observed import states never depend on the local job registry.
const (
	AccountImportImported AccountImportState = "imported"
	AccountImportRunning  AccountImportState = "running"
	AccountImportAbsent   AccountImportState = "absent"
)

var (
	// ErrAccountImportLeaseHeld means another session owns the import.
	ErrAccountImportLeaseHeld = errors.New("account import lease held")
	// ErrAccountImportLeaseUnavailable means no pool connection was available.
	ErrAccountImportLeaseUnavailable = errors.New("account import lease unavailable")
)

// AccountImportLease owns one database session through download and import.
type AccountImportLease interface {
	Receipt() (ImportSummary, bool)
	Import(context.Context, io.Reader) (ImportSummary, error)
	Close()
}

type accountImportJob struct {
	done   chan struct{}
	result map[string]any
	token  string
}

type accountImportJobs struct {
	mu     sync.Mutex
	jobs   map[[2]string]*accountImportJob
	sem    chan struct{}
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func newAccountImportJobs(ctx context.Context, concurrency int) *accountImportJobs {
	if concurrency < 1 {
		concurrency = 1
	}
	ctx, cancel := context.WithCancel(ctx)
	return &accountImportJobs{jobs: make(map[[2]string]*accountImportJob), sem: make(chan struct{}, concurrency), ctx: ctx, cancel: cancel}
}

func (j *accountImportJobs) wait(ctx context.Context) {
	done := make(chan struct{})
	go func() { j.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

func accountImportTimeout(cfg Config) time.Duration {
	if cfg.AccountImportJobTimeout > 0 {
		return cfg.AccountImportJobTimeout
	}
	return 3 * time.Hour
}

func importReceiptAck(s ImportSummary, archive bool) map[string]any {
	result := map[string]any{"schema_version": "witself.v0", "account_id": s.AccountID, "status": s.Status,
		"evacuation_id": s.EvacuationID, "evacuation_role": s.EvacuationRole,
		"already_imported": s.AlreadyImported, "evacuation_completed": s.EvacuationCompleted}
	if archive {
		result["archive_schema_version"] = s.SchemaVersion
	}
	return result
}

func importJobBody(accountID, evacuationID string, state AccountImportState, started *bool) map[string]any {
	job := map[string]any{"state": state}
	if started != nil {
		job["started"] = *started
	}
	return map[string]any{"schema_version": "witself.v0", "account_id": accountID, "evacuation_id": evacuationID, "import_job": job}
}

func writeImportJSON(w http.ResponseWriter, body map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

var importWaitPattern = regexp.MustCompile(`^(0|[1-9][0-9]{0,2})$`)

func accountImportAction(w http.ResponseWriter, r *http.Request, cfg Config, client *http.Client, accountID string, start bool) {
	evacuationID := strings.TrimSpace(r.Header.Get(AccountEvacuationIDHeader))
	if !validEvacuationID(evacuationID) {
		writeJSONError(w, 400, "a valid evacuation id is required")
		return
	}
	var u *neturl.URL
	var size int64
	wait := 0
	if start {
		source := r.Clone(r.Context())
		for _, name := range []string{"URL", "Token", "Size"} {
			source.Header.Set("X-Witself-Backup-Archive-"+name, r.Header.Get("X-Witself-Archive-"+name))
		}
		var valid bool
		u, size, valid = backupArchiveSource(source, cfg.BackupValidationArchiveOrigin)
		if !valid {
			writeJSONError(w, 400, "a valid archive source is required")
			return
		}
		if raw, present := r.Header[http.CanonicalHeaderKey("X-Witself-Import-Wait")]; present {
			if len(raw) != 1 || !importWaitPattern.MatchString(raw[0]) {
				writeJSONError(w, 400, "a valid import wait is required")
				return
			}
			wait, _ = strconv.Atoi(raw[0])
			if wait > 300 {
				writeJSONError(w, 400, "a valid import wait is required")
				return
			}
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	sum, state, err := cfg.AccountImportStatus(ctx, accountID, evacuationID)
	cancel()
	if errors.Is(err, ErrConflict) {
		writeJSONError(w, 409, "account exists under a different evacuation")
		return
	}
	if err != nil {
		writeJSONError(w, 500, "could not import account")
		return
	}
	if state == AccountImportImported {
		writeImportJSON(w, importReceiptAck(sum, false))
		return
	}
	if !start {
		writeImportJSON(w, importJobBody(accountID, evacuationID, state, nil))
		return
	}
	jobs := cfg.accountImportJobs
	key := [2]string{accountID, evacuationID}
	jobs.mu.Lock()
	job := jobs.jobs[key]
	if job == nil {
		if jobs.ctx.Err() != nil {
			jobs.mu.Unlock()
			writeJSONError(w, 503, "could not import account")
			return
		}
		select {
		case jobs.sem <- struct{}{}:
		default:
			jobs.mu.Unlock()
			importCapacityError(w)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		lease, err := cfg.BeginAccountImport(ctx, accountID, evacuationID)
		cancel()
		if err != nil {
			<-jobs.sem
			jobs.mu.Unlock()
			switch {
			case errors.Is(err, ErrAccountImportLeaseHeld):
				started := false
				writeImportJSON(w, importJobBody(accountID, evacuationID, AccountImportRunning, &started))
			case errors.Is(err, ErrAccountImportLeaseUnavailable):
				importCapacityError(w)
			case errors.Is(err, ErrConflict):
				writeJSONError(w, 409, "account exists under a different evacuation")
			default:
				writeJSONError(w, 500, "could not import account")
			}
			return
		}
		job = &accountImportJob{done: make(chan struct{}), token: r.Header.Get("X-Witself-Archive-Token")}
		jobs.jobs[key] = job
		jobs.wg.Add(1)
		go jobs.run(cfg, client, key, job, lease, u, size)
	}
	jobs.mu.Unlock()
	started := true
	running := importJobBody(accountID, evacuationID, AccountImportRunning, &started)
	if wait == 0 {
		select {
		case <-job.done:
			writeImportJSON(w, job.result)
		default:
			writeImportJSON(w, running)
		}
		return
	}
	stream := startValidationStream(w, backupValidationHeartbeat)
	timer := time.NewTimer(time.Duration(wait) * time.Second)
	defer timer.Stop()
	select {
	case <-job.done:
		stream.finish(job.result)
	case <-timer.C:
		stream.finish(running)
	case <-jobs.ctx.Done():
		stream.finish(running)
	case <-r.Context().Done():
		close(stream.stop)
		<-stream.done
	}
}

func importCapacityError(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "60")
	writeJSONError(w, http.StatusServiceUnavailable, "import capacity exhausted")
}

func (j *accountImportJobs) run(cfg Config, client *http.Client, key [2]string, job *accountImportJob, lease AccountImportLease, u *neturl.URL, size int64) {
	ctx, cancel := context.WithTimeout(j.ctx, accountImportTimeout(cfg))
	defer cancel()
	started := time.Now()
	outcome := "failed"
	if cfg.ReportAccountImportJob != nil {
		cfg.ReportAccountImportJob(key[0], key[1], "", 0)
	}
	var spool *os.File
	defer func() {
		job.token = ""
		j.mu.Lock()
		delete(j.jobs, key)
		j.mu.Unlock()
		lease.Close()
		if spool != nil {
			_ = spool.Close()
			_ = os.Remove(spool.Name())
		}
		<-j.sem
		if cfg.ReportAccountImportJob != nil {
			cfg.ReportAccountImportJob(key[0], key[1], outcome, time.Since(started))
		}
		close(job.done)
		j.wg.Done()
	}()
	if sum, present := lease.Receipt(); present {
		job.result = importReceiptAck(sum, false)
		outcome = "replayed"
		return
	}
	var err error
	spool, err = downloadImportArchiveContext(ctx, client, u, job.takeToken(), size)
	message := "archive download failed"
	if err == nil {
		var sum ImportSummary
		sum, err = lease.Import(ctx, spool)
		if err == nil {
			job.result = importReceiptAck(sum, true)
			outcome = "imported"
			return
		}
		switch {
		case errors.Is(err, ErrConflict):
			message = "account exists under a different evacuation"
		case errors.Is(err, ErrArchiveTooNew):
			message = "archive schema is newer than this cell — upgrade the cell first"
		case errors.Is(err, ErrBadArchive):
			message = "invalid or corrupt archive"
		default:
			message = "could not import account"
		}
	}
	report := message
	if spool == nil && err != nil {
		// The download helper exposes only fixed, value-free failure strings.
		report = err.Error()
	}
	if j.ctx.Err() != nil {
		outcome = "cancelled"
		report = "import job cancelled by shutdown"
	} else if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		outcome = "deadline"
		report = "import job deadline exceeded"
	}
	if cfg.ReportAccountImportFailure != nil {
		cfg.ReportAccountImportFailure(ctx, key[0], errors.New(report))
	}
	job.result = map[string]any{"schema_version": "witself.v0", "error": message}
}

func (j *accountImportJob) takeToken() string {
	token := j.token
	j.token = ""
	return token
}
