package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

var (
	// ErrBackupValidationLeaseHeld means another session owns the account's validation lock.
	ErrBackupValidationLeaseHeld = errors.New("backup validation lease held")
	// ErrBackupValidationLeaseUnavailable means a database session is temporarily unavailable.
	ErrBackupValidationLeaseUnavailable = errors.New("backup validation lease unavailable")
	errBackupValidationSuperseded       = errors.New("backup validation job superseded")
)

// BackupValidationLease pins the account's session through validation and result retention.
type BackupValidationLease interface {
	Validate(context.Context, io.Reader) (ImportSummary, error)
	Held() bool
	Close()
}

type backupValidationJob struct {
	validationID, backupID string
	done                   chan struct{}
	result                 map[string]any
	finishedAt             time.Time
	retained               bool
	delivered              atomic.Bool
	token                  string
	cancel                 context.CancelFunc
	release                chan struct{}
	releaseOnce            sync.Once
	lease                  BackupValidationLease
	closeOnce              sync.Once
}

type backupValidationJobs struct {
	mu             sync.Mutex
	jobs           map[string]*backupValidationJob
	sem            chan struct{}
	sessions       int // Includes removed entries until their lease close completes.
	bound          int
	ctx            context.Context
	cancel         context.CancelFunc
	wg             sync.WaitGroup
	retention      time.Duration
	retentionTimer func(time.Duration) (<-chan time.Time, func() bool)
	afterPublish   func(*backupValidationJob)
}

func newBackupValidationJobs(ctx context.Context, concurrency int, retention time.Duration) *backupValidationJobs {
	if concurrency < 1 {
		concurrency = 1
	}
	if retention <= 0 {
		retention = 30 * time.Minute
	}
	ctx, cancel := context.WithCancel(ctx)
	return &backupValidationJobs{jobs: make(map[string]*backupValidationJob), sem: make(chan struct{}, concurrency), bound: concurrency, ctx: ctx, cancel: cancel, retention: retention,
		retentionTimer: func(d time.Duration) (<-chan time.Time, func() bool) { t := time.NewTimer(d); return t.C, t.Stop },
	}
}

func (j *backupValidationJobs) wait(ctx context.Context) {
	done := make(chan struct{})
	go func() { j.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

func backupValidationJobTimeout(cfg Config) time.Duration {
	if cfg.BackupValidationJobTimeout > 0 {
		return cfg.BackupValidationJobTimeout
	}
	return time.Hour
}

// retire removes only this incarnation before closing its session, outside mu.
func (j *backupValidationJobs) retire(accountID string, job *backupValidationJob) {
	j.mu.Lock()
	if j.jobs[accountID] == job {
		delete(j.jobs, accountID)
	}
	j.mu.Unlock()
	j.closeLease(job)
}

func (j *backupValidationJobs) closeLease(job *backupValidationJob) {
	job.closeOnce.Do(func() {
		job.lease.Close()
		j.mu.Lock()
		j.sessions--
		j.mu.Unlock()
	})
}

// evict is called with mu held and returns with it held. No session close runs
// under mu: a retiring goroutine may already be closing this same session.
func (j *backupValidationJobs) evict(accountID string, job *backupValidationJob) {
	job.releaseOnce.Do(func() { close(job.release) })
	if j.jobs[accountID] == job {
		delete(j.jobs, accountID)
	}
	j.mu.Unlock()
	j.closeLease(job)
	j.mu.Lock()
}

func (job *backupValidationJob) finished() bool {
	select {
	case <-job.done:
		return true
	default:
		return false
	}
}

func backupValidationJobBody(accountID, backupID, validationID, state string, started *bool) map[string]any {
	job := map[string]any{"state": state}
	if started != nil {
		job["started"] = *started
	}
	return map[string]any{"schema_version": "witself.v0", "account_id": accountID, "backup_id": backupID, "validation_id": validationID, "validation_job": job}
}

func backupValidationCapacityError(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "60")
	writeJSONError(w, http.StatusServiceUnavailable, "validation capacity exhausted")
}

func backupValidationAction(w http.ResponseWriter, r *http.Request, cfg Config, client *http.Client, accountID, backupID string, start bool) {
	validationID := r.Header.Get(BackupValidationIDHeader)
	if !validOperationID(validationID) {
		writeJSONError(w, 400, "a valid validation id is required")
		return
	}
	if !start {
		backupValidationStatus(w, r, cfg, accountID, backupID, validationID)
		return
	}
	u, size, valid := backupArchiveSource(r, cfg.BackupValidationArchiveOrigin)
	if !valid {
		writeJSONError(w, 400, "a valid backup archive source is required")
		return
	}
	wait := 0
	if raw, present := r.Header[http.CanonicalHeaderKey("X-Witself-Validation-Wait")]; present {
		if len(raw) != 1 || !importWaitPattern.MatchString(raw[0]) {
			writeJSONError(w, 400, "a valid validation wait is required")
			return
		}
		wait, _ = strconv.Atoi(raw[0])
		if wait > 300 {
			writeJSONError(w, 400, "a valid validation wait is required")
			return
		}
	}
	jobs := cfg.backupValidationJobs
	jobs.mu.Lock()
	for {
		job := jobs.jobs[accountID]
		if job != nil && (!job.finished() || job.retained) {
			if job.validationID == validationID {
				if job.backupID != backupID {
					jobs.mu.Unlock()
					writeJSONError(w, 409, "validation id is bound to a different backup")
					return
				}
				jobs.mu.Unlock()
				backupValidationAnswer(w, r, jobs, job, accountID, wait)
				return
			}
			if job.retained {
				jobs.evict(accountID, job)
				continue
			}
			job.cancel()
			job.releaseOnce.Do(func() { close(job.release) })
			jobs.mu.Unlock()
			started := false
			writeImportJSON(w, backupValidationJobBody(accountID, backupID, validationID, "running", &started))
			return
		}
		if jobs.ctx.Err() != nil {
			jobs.mu.Unlock()
			backupValidationCapacityError(w)
			return
		}
		select {
		case jobs.sem <- struct{}{}:
		default:
			jobs.mu.Unlock()
			backupValidationCapacityError(w)
			return
		}
		restart := false
		for {
			count := 0
			var oldest *backupValidationJob
			var oldestAccount string
			for account, candidate := range jobs.jobs {
				if account == accountID || !candidate.retained {
					continue
				}
				count++
				if candidate.delivered.Load() && (oldest == nil || candidate.finishedAt.Before(oldest.finishedAt)) {
					oldest, oldestAccount = candidate, account
				}
			}
			if count < jobs.bound {
				break
			}
			if oldest == nil {
				<-jobs.sem
				jobs.mu.Unlock()
				backupValidationCapacityError(w)
				return
			}
			jobs.evict(oldestAccount, oldest)
			if jobs.jobs[accountID] != nil {
				<-jobs.sem
				restart = true
				break
			}
		}
		if restart {
			continue
		}
		// Shutdown may have arrived while an eviction released mu.
		if jobs.ctx.Err() != nil {
			<-jobs.sem
			jobs.mu.Unlock()
			backupValidationCapacityError(w)
			return
		}
		// Removed retained entries still pin a session until Close returns.
		if jobs.sessions > len(jobs.jobs) && jobs.sessions >= 2*jobs.bound-1 {
			<-jobs.sem
			jobs.mu.Unlock()
			backupValidationCapacityError(w)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		lease, err := cfg.BeginBackupValidation(ctx, accountID, backupID)
		cancel()
		if err != nil {
			<-jobs.sem
			jobs.mu.Unlock()
			switch {
			case errors.Is(err, ErrBackupValidationLeaseHeld):
				started := false
				writeImportJSON(w, backupValidationJobBody(accountID, backupID, validationID, "running", &started))
			case errors.Is(err, ErrBackupValidationLeaseUnavailable):
				backupValidationCapacityError(w)
			default:
				writeJSONError(w, 500, "could not validate backup")
			}
			return
		}
		jobs.sessions++
		jobCtx, timeoutCancel := context.WithTimeout(jobs.ctx, backupValidationJobTimeout(cfg))
		jobCtx, supersede := context.WithCancelCause(jobCtx)
		job = &backupValidationJob{validationID: validationID, backupID: backupID, done: make(chan struct{}), release: make(chan struct{}), lease: lease, token: r.Header.Get("X-Witself-Backup-Archive-Token"), cancel: func() { supersede(errBackupValidationSuperseded) }}
		jobs.jobs[accountID] = job
		jobs.wg.Add(1)
		go func() {
			defer timeoutCancel()
			defer supersede(nil)
			jobs.run(jobCtx, cfg, client, accountID, job, u, size)
		}()
		jobs.mu.Unlock()
		backupValidationAnswer(w, r, jobs, job, accountID, wait)
		return
	}
}

func backupValidationStatus(w http.ResponseWriter, r *http.Request, cfg Config, accountID, backupID, validationID string) {
	jobs := cfg.backupValidationJobs
	jobs.mu.Lock()
	job := jobs.jobs[accountID]
	if job != nil && (!job.finished() || job.retained) {
		state := "absent"
		if job.validationID == validationID && job.backupID == backupID {
			if job.retained {
				job.delivered.Store(true)
				result := job.result
				jobs.mu.Unlock()
				writeImportJSON(w, result)
				return
			}
			state = "running"
		}
		jobs.mu.Unlock()
		writeImportJSON(w, backupValidationJobBody(accountID, backupID, validationID, state, nil))
		return
	}
	jobs.mu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	held, err := cfg.BackupValidationInProgress(ctx, accountID)
	cancel()
	if err != nil {
		writeJSONError(w, 500, "could not validate backup")
		return
	}
	state := "absent"
	if held {
		state = "running"
	}
	writeImportJSON(w, backupValidationJobBody(accountID, backupID, validationID, state, nil))
}

func backupValidationAnswer(w http.ResponseWriter, r *http.Request, jobs *backupValidationJobs, job *backupValidationJob, accountID string, wait int) {
	started := true
	running := backupValidationJobBody(accountID, job.backupID, job.validationID, "running", &started)
	answer := func() map[string]any {
		if job.finished() && job.result != nil {
			job.delivered.Store(true)
			return job.result
		}
		return running
	}
	if r.Context().Err() != nil {
		return
	}
	if wait == 0 || job.finished() {
		writeImportJSON(w, answer())
		return
	}
	stream := startValidationStream(w, backupValidationHeartbeat)
	timer := time.NewTimer(time.Duration(wait) * time.Second)
	defer timer.Stop()
	var body map[string]any
	select {
	case <-job.done:
		body = answer()
	case <-timer.C:
		body = running
	case <-jobs.ctx.Done():
		body = running
	case <-r.Context().Done():
	}
	if r.Context().Err() != nil {
		close(stream.stop)
		<-stream.done
		return
	}
	stream.finish(body)
}

func backupValidationFailure(err error) string {
	switch {
	case errors.Is(err, ErrConflict):
		return "backup validation target already contains the account"
	case errors.Is(err, ErrArchiveTooNew):
		return "backup schema is newer than this cell — upgrade the cell first"
	case errors.Is(err, ErrBadArchive):
		return "invalid or mismatched backup archive"
	default:
		return "could not validate backup"
	}
}

func (j *backupValidationJobs) run(ctx context.Context, cfg Config, client *http.Client, accountID string, job *backupValidationJob, u *neturl.URL, size int64) {
	defer j.wg.Done()
	started := time.Now()
	if cfg.ReportBackupValidationJob != nil {
		cfg.ReportBackupValidationJob(accountID, job.validationID, "", 0, 0)
	}
	spool, err := downloadArchiveSpool(ctx, client, u, job.takeToken(), size, "witself-backup-validate-*.tar.gz")
	downloadDuration := time.Since(started)
	var message, report string
	var result map[string]any
	outcome := "failed"
	if err != nil {
		message = "backup archive download failed"
		report = err.Error()
	} else {
		var sum ImportSummary
		sum, err = job.lease.Validate(ctx, spool)
		_ = spool.Close()
		_ = os.Remove(spool.Name())
		if err == nil {
			outcome = "validated"
			result = map[string]any{"schema_version": "witself.v0", "account_id": sum.AccountID, "status": sum.Status, "archive_schema_version": sum.SchemaVersion, "purpose": "backup", "backup_id": sum.BackupID, "validated": true}
		} else {
			message = backupValidationFailure(err)
			report = message
		}
	}
	// Serialize the final disposition with START supersession before publishing.
	j.mu.Lock()
	held := job.lease.Held()
	superseded := false
	select {
	case <-job.release:
		superseded = true
	default:
	}
	switch {
	case superseded:
		outcome, report = "superseded", "backup validation job superseded"
	case j.ctx.Err() != nil:
		outcome, report = "cancelled", "backup validation job cancelled by shutdown"
	case err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded):
		outcome, report = "deadline", "backup validation job deadline exceeded"
	}
	if outcome != "validated" && cfg.ReportAccountBackupValidationFailure != nil {
		cfg.ReportAccountBackupValidationFailure(context.WithoutCancel(ctx), accountID, errors.New(report))
	}
	if cfg.ReportBackupValidationJob != nil {
		cfg.ReportBackupValidationJob(accountID, job.validationID, outcome, downloadDuration, time.Since(started))
	}
	retain := (outcome == "validated" || outcome == "failed") && held
	if outcome == "superseded" || outcome == "cancelled" || (outcome == "failed" && !retain) {
		result = nil
	} else if outcome != "validated" {
		result = map[string]any{"schema_version": "witself.v0", "error": message}
	}
	job.result, job.finishedAt, job.retained = result, time.Now(), retain
	close(job.done)
	if retain {
		<-j.sem
	}
	j.mu.Unlock()
	if j.afterPublish != nil {
		j.afterPublish(job)
	}
	if retain {
		timer, stop := j.retentionTimer(j.retention)
		select {
		case <-timer:
		case <-job.release:
		case <-j.ctx.Done():
		}
		stop()
	}
	j.retire(accountID, job)
	if !retain {
		<-j.sem
	}
}

func (job *backupValidationJob) takeToken() string {
	token := job.token
	job.token = ""
	return token
}
