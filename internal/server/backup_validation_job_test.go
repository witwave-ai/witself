package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeValidationLease struct {
	f       *validationJobFixture
	callsMu sync.Mutex
	calls   []string
	closed  atomic.Bool
}

func (l *fakeValidationLease) record(call string) {
	l.callsMu.Lock()
	defer l.callsMu.Unlock()
	l.calls = append(l.calls, call)
}
func (l *fakeValidationLease) Validate(ctx context.Context, r io.Reader) (ImportSummary, error) {
	l.record("Validate")
	l.f.validates.Add(1)
	return l.f.validate(ctx, r)
}
func (l *fakeValidationLease) Held() bool {
	l.record("Held")
	return !l.closed.Load() && l.f.held.Load()
}
func (l *fakeValidationLease) Close() {
	l.record("Close")
	if l.closed.Swap(true) {
		l.f.t.Error("lease closed twice")
		return
	}
	l.f.open.Add(-1)
	l.f.closes.Add(1)
	if l.f.onClose != nil {
		l.f.onClose()
	}
}

type validationJobFixture struct {
	t                                                     *testing.T
	cfg                                                   Config
	client                                                *http.Client
	cp                                                    *httptest.Server
	dir                                                   string
	responseMode                                          string
	gets, validates, begins, closes, open, maxOpen, reads atomic.Int32
	held                                                  atomic.Bool
	validate                                              func(context.Context, io.Reader) (ImportSummary, error)
	onClose                                               func()
	reports, outcomes                                     chan string
	leasesMu                                              sync.Mutex
	leases                                                []*fakeValidationLease
}

func newValidationJobFixture(t *testing.T, concurrency int) *validationJobFixture {
	t.Helper()
	f := &validationJobFixture{t: t, dir: t.TempDir(), reports: make(chan string, 30), outcomes: make(chan string, 30)}
	t.Setenv("TMPDIR", f.dir)
	f.held.Store(true)
	f.cp = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.gets.Add(1)
		if r.ProtoMajor != 1 || r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer cap_"+strings.Repeat("a", 64) {
			t.Error("incorrect archive transport")
		}
		switch f.responseMode {
		case "redirect":
			w.Header().Set("Location", "/again")
			w.WriteHeader(302)
			return
		case "non-200":
			w.WriteHeader(404)
			return
		case "size-mismatch":
			w.Header().Set("Content-Length", "8")
			_, _ = io.WriteString(w, "archive!")
			return
		case "short-body":
			w.Header().Set("Content-Length", "7")
			_, _ = io.WriteString(w, "short")
			return
		}
		w.Header().Set("Content-Length", "7")
		_, _ = io.WriteString(w, "archive")
	}))
	f.cp.EnableHTTP2 = true
	f.cp.StartTLS()
	t.Cleanup(f.cp.Close)
	f.client = newBackupArchiveClient()
	f.client.Transport.(*http.Transport).TLSClientConfig.RootCAs = f.cp.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	t.Cleanup(f.client.CloseIdleConnections)
	f.validate = func(_ context.Context, r io.Reader) (ImportSummary, error) {
		b, e := io.ReadAll(r)
		if e != nil || string(b) != "archive" {
			t.Error("bad archive spool")
		}
		return ImportSummary{AccountID: "acc_job", Status: "suspended", SchemaVersion: 98, BackupID: "backup_20261003T000000Z"}, nil
	}
	f.cfg = Config{BackupToken: "test-backup", ProvisionToken: "test-provision", BackupValidationEnabled: true, BackupValidationArchiveOrigin: f.cp.URL, BackupValidationJobTimeout: time.Minute,
		backupValidationJobs: newBackupValidationJobs(context.Background(), concurrency, time.Minute),
		ValidateAccountBackup: func(context.Context, string, string, io.Reader) (ImportSummary, error) {
			t.Error("sync path used")
			return ImportSummary{}, nil
		},
		BeginBackupValidation: func(context.Context, string, string) (BackupValidationLease, error) {
			f.begins.Add(1)
			n := f.open.Add(1)
			for old := f.maxOpen.Load(); n > old; old = f.maxOpen.Load() {
				if f.maxOpen.CompareAndSwap(old, n) {
					break
				}
			}
			l := &fakeValidationLease{f: f}
			f.leasesMu.Lock()
			f.leases = append(f.leases, l)
			f.leasesMu.Unlock()
			return l, nil
		},
		BackupValidationInProgress: func(context.Context, string) (bool, error) { f.reads.Add(1); return f.open.Load() > 0, nil },
		ReportAccountBackupValidationFailure: func(ctx context.Context, _ string, e error) {
			if ctx.Err() != nil {
				t.Error("failure report context cancelled")
			}
			if strings.Contains(e.Error(), f.cp.URL) || strings.Contains(e.Error(), "cap_") || strings.Contains(e.Error(), "private") {
				t.Error("private failure diagnostic")
			}
			f.reports <- e.Error()
		},
		ReportBackupValidationJob: func(_, _, outcome string, download, total time.Duration) {
			if outcome != "" {
				if download < 0 || total < download {
					t.Error("invalid job timing")
				}
				f.outcomes <- outcome
			}
		},
	}
	t.Cleanup(func() { f.cfg.backupValidationJobs.cancel(); f.drained(t) })
	return f
}
func (f *validationJobFixture) request(account, validation, action, wait string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/accounts/"+account+":"+action, nil)
	r.Header.Set("Authorization", "Bearer test-backup")
	r.Header.Set(AccountBackupIDHeader, "backup_20261003T000000Z")
	r.Header.Set(BackupValidationIDHeader, validation)
	if action == "start-validate-backup" {
		r.Header.Set("X-Witself-Backup-Archive-URL", f.cp.URL+"/v1/archives:pull")
		r.Header.Set("X-Witself-Backup-Archive-Token", "cap_"+strings.Repeat("a", 64))
		r.Header.Set("X-Witself-Backup-Archive-Size", "7")
		if wait != "" {
			r.Header.Set("X-Witself-Validation-Wait", wait)
		}
	}
	return r
}
func (f *validationJobFixture) start(account, validation, wait string) *http.Request {
	return f.request(account, validation, "start-validate-backup", wait)
}
func (f *validationJobFixture) serve(r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	accountBackupHandlerWithArchiveClient(f.cfg, f.client)(w, r)
	return w
}
func (f *validationJobFixture) status(account, validation string) *httptest.ResponseRecorder {
	return f.serve(f.request(account, validation, "validate-backup-status", ""))
}
func (f *validationJobFixture) job(account string) *backupValidationJob {
	j := f.cfg.backupValidationJobs
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.jobs[account]
}
func (f *validationJobFixture) noSpool(t *testing.T) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(f.dir, "witself-backup-validate-*.tar.gz"))
	if err != nil || len(files) != 0 {
		t.Error("validation spool leaked")
	}
}
func (f *validationJobFixture) drained(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	f.cfg.backupValidationJobs.wait(ctx)
	if ctx.Err() != nil {
		t.Error("validation jobs did not drain")
		return
	}
	f.noSpool(t)
}
func validationBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var b map[string]any
	if strings.Contains(w.Body.String(), "cap_") || strings.Contains(w.Body.String(), "https://") {
		t.Fatal("response disclosed capability")
	}
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if b["schema_version"] != "witself.v0" {
		t.Fatal("wrong response schema")
	}
	return b
}
func requireValidationAck(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	b := validationBody(t, w)
	if w.Code != 200 || len(b) != 7 || b["validated"] != true || b["purpose"] != "backup" || b["archive_schema_version"] != float64(98) || (b["status"] != "suspended" && b["status"] != "active") || b["account_id"] == nil || b["backup_id"] != "backup_20261003T000000Z" {
		t.Fatalf("wrong acknowledgement: %v", b)
	}
}
func requireValidationState(t *testing.T, w *httptest.ResponseRecorder, state string, started *bool) {
	t.Helper()
	b := validationBody(t, w)
	j, ok := b["validation_job"].(map[string]any)
	if w.Code != 200 || len(b) != 5 || !ok || j["state"] != state || b["account_id"] == nil || b["backup_id"] == nil || b["validation_id"] == nil {
		t.Fatalf("wrong job state: %v", b)
	}
	if started == nil {
		if len(j) != 1 {
			t.Fatal("status contains started")
		}
	} else if len(j) != 2 || j["started"] != *started {
		t.Fatal("wrong started disposition")
	}
}
func requireValidationError(t *testing.T, w *httptest.ResponseRecorder, code int, msg string) {
	t.Helper()
	b := validationBody(t, w)
	if w.Code != code || b["error"] != msg {
		t.Fatalf("want %d %q, got %d %v", code, msg, w.Code, b)
	}
	if code == 200 && len(b) != 2 {
		t.Fatal("inline error contains extra fields")
	}
	if code == 503 && w.Header().Get("Retry-After") != "60" {
		t.Fatal("missing Retry-After")
	}
}
func waitValidation(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("validation condition timed out")
		}
		time.Sleep(time.Millisecond)
	}
}
func validationBool(v bool) *bool { return &v }
func waitValidationSignal(t *testing.T, c <-chan struct{}) {
	t.Helper()
	select {
	case <-c:
	case <-time.After(time.Second):
		t.Fatal("validation signal timed out")
	}
}

func TestBackupValidationJobInlineAndRetainedReplay(t *testing.T) {
	f := newValidationJobFixture(t, 1)
	requireValidationAck(t, f.serve(f.start("acc_job", "V1", "300")))
	if !f.job("acc_job").delivered.Load() {
		t.Fatal("inline START acknowledgement did not mark result delivered")
	}
	f.noSpool(t)
	requireValidationAck(t, f.status("acc_job", "V1"))
	requireValidationAck(t, f.serve(f.start("acc_job", "V1", "0")))
	if f.gets.Load() != 1 || f.validates.Load() != 1 || f.begins.Load() != 1 || f.closes.Load() != 0 {
		t.Fatal("retained replay repeated or closed validation")
	}
	f.leasesMu.Lock()
	l := f.leases[0]
	f.leasesMu.Unlock()
	l.callsMu.Lock()
	defer l.callsMu.Unlock()
	if strings.Join(l.calls, ",") != "Validate,Held" {
		t.Fatal("incorrect lease call order")
	}
}

func TestBackupValidationJobHeartbeat(t *testing.T) {
	previous := backupValidationHeartbeat
	backupValidationHeartbeat = 20 * time.Millisecond
	defer func() { backupValidationHeartbeat = previous }()
	f := newValidationJobFixture(t, 1)
	entered := make(chan struct{})
	release := make(chan struct{})
	original := f.validate
	f.validate = func(ctx context.Context, r io.Reader) (ImportSummary, error) {
		close(entered)
		select {
		case <-release:
			return original(ctx, r)
		case <-ctx.Done():
			return ImportSummary{}, ctx.Err()
		}
	}
	answer := make(chan *httptest.ResponseRecorder, 1)
	go func() { answer <- f.serve(f.start("acc_job", "heartbeat", "300")) }()
	waitValidationSignal(t, entered)
	timer := time.NewTimer(5 * backupValidationHeartbeat)
	defer timer.Stop()
	<-timer.C
	close(release)
	var w *httptest.ResponseRecorder
	select {
	case w = <-answer:
	case <-time.After(time.Second):
		t.Fatal("heartbeat waiter did not finish")
	}
	requireValidationAck(t, w)
	body := w.Body.String()
	terminal := strings.TrimLeft(body, "\n")
	if len(terminal) == len(body) {
		t.Fatal("asynchronous waiter emitted no newline heartbeat")
	}
	decoder := json.NewDecoder(strings.NewReader(terminal))
	var object json.RawMessage
	if err := decoder.Decode(&object); err != nil {
		t.Fatalf("invalid heartbeat terminal object: %v", err)
	}
	// The encoder ends its one JSON object with a newline; no later heartbeat
	// or other bytes may follow that framing newline.
	if terminal[decoder.InputOffset():] != "\n" {
		t.Fatal("heartbeat stream contains data after the terminal object")
	}
}

func TestBackupValidationJobRunningAttachAndBinding(t *testing.T) {
	f := newValidationJobFixture(t, 1)
	release := make(chan struct{})
	entered := make(chan struct{})
	original := f.validate
	f.validate = func(ctx context.Context, r io.Reader) (ImportSummary, error) {
		close(entered)
		select {
		case <-release:
			return original(ctx, r)
		case <-ctx.Done():
			return ImportSummary{}, ctx.Err()
		}
	}
	requireValidationState(t, f.serve(f.start("acc_job", "V1", "0")), "running", validationBool(true))
	waitValidationSignal(t, entered)
	requireValidationState(t, f.status("acc_job", "V1"), "running", nil)
	requireValidationState(t, f.serve(f.start("acc_job", "V1", "0")), "running", validationBool(true))
	r := f.start("acc_job", "V1", "0")
	r.Header.Set(AccountBackupIDHeader, "backup_20261003T010000Z")
	requireValidationError(t, f.serve(r), 409, "validation id is bound to a different backup")
	close(release)
	waitValidationSignal(t, f.job("acc_job").done)
	requireValidationAck(t, f.status("acc_job", "V1"))
	if f.gets.Load() != 1 || f.begins.Load() != 1 || f.validates.Load() != 1 {
		t.Fatal("duplicate attached job")
	}
}

func TestBackupValidationJobLeaseOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		code int
	}{{"held", ErrBackupValidationLeaseHeld, 200}, {"unavailable", ErrBackupValidationLeaseUnavailable, 503}, {"other", errors.New("private lease failure"), 500}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newValidationJobFixture(t, 1)
			begin := f.cfg.BeginBackupValidation
			f.cfg.BeginBackupValidation = func(ctx context.Context, a, b string) (BackupValidationLease, error) {
				if a == "acc_job" {
					return nil, tc.err
				}
				return begin(ctx, a, b)
			}
			w := f.serve(f.start("acc_job", "V1", "0"))
			if tc.code == 200 {
				requireValidationState(t, w, "running", validationBool(false))
			} else {
				msg := "could not validate backup"
				if tc.code == 503 {
					msg = "validation capacity exhausted"
				}
				requireValidationError(t, w, tc.code, msg)
			}
			if f.gets.Load() != 0 || len(f.cfg.backupValidationJobs.sem) != 0 {
				t.Fatal("lease refusal consumed capability or slot")
			}
			requireValidationAck(t, f.serve(f.start("acc_other", "V2", "300")))
		})
	}
}

func TestBackupValidationJobRunningCapacity(t *testing.T) {
	f := newValidationJobFixture(t, 1)
	entered := make(chan struct{})
	f.validate = func(ctx context.Context, _ io.Reader) (ImportSummary, error) {
		close(entered)
		<-ctx.Done()
		return ImportSummary{}, ctx.Err()
	}
	requireValidationState(t, f.serve(f.start("acc_job", "V1", "0")), "running", validationBool(true))
	waitValidationSignal(t, entered)
	requireValidationError(t, f.serve(f.start("acc_other", "V2", "0")), 503, "validation capacity exhausted")
	if f.gets.Load() != 1 || f.begins.Load() != 1 || f.maxOpen.Load() != 1 {
		t.Fatal("capacity opened second validation")
	}
}

func TestBackupValidationJobRetainedBound(t *testing.T) {
	f := newValidationJobFixture(t, 1)
	release := make(chan struct{})
	original := f.validate
	f.validate = func(ctx context.Context, r io.Reader) (ImportSummary, error) {
		select {
		case <-release:
			return original(ctx, r)
		case <-ctx.Done():
			return ImportSummary{}, ctx.Err()
		}
	}
	f.onClose = func() {
		if f.open.Load() != 0 {
			t.Error("Begin preceded eviction Close")
		}
	}
	requireValidationState(t, f.serve(f.start("acc_job", "V1", "0")), "running", validationBool(true))
	job := f.job("acc_job")
	close(release)
	waitValidationSignal(t, job.done)
	if job.delivered.Load() {
		t.Fatal("unread completion marked delivered")
	}
	requireValidationError(t, f.serve(f.start("acc_B", "V2", "0")), 503, "validation capacity exhausted")
	if f.gets.Load() != 1 || f.begins.Load() != 1 || f.closes.Load() != 0 {
		t.Fatal("undelivered result evicted")
	}
	requireValidationAck(t, f.serve(f.start("acc_job", "V1", "0")))
	if !job.delivered.Load() {
		t.Fatal("retained START replay did not mark result delivered")
	}
	for _, a := range []string{"acc_B", "acc_C", "acc_D"} {
		requireValidationAck(t, f.serve(f.start(a, "V2", "300")))
	}
	if f.maxOpen.Load() != 1 || f.begins.Load() != 4 || f.closes.Load() != 3 {
		t.Fatal("retained session bound exceeded")
	}
}

func TestBackupValidationJobSupersedeAndEvict(t *testing.T) {
	t.Run("undelivered_retained", func(t *testing.T) {
		f := newValidationJobFixture(t, 1)
		release := make(chan struct{})
		original := f.validate
		f.validate = func(ctx context.Context, r io.Reader) (ImportSummary, error) {
			select {
			case <-release:
				return original(ctx, r)
			case <-ctx.Done():
				return ImportSummary{}, ctx.Err()
			}
		}
		f.serve(f.start("acc_job", "V1", "0"))
		old := f.job("acc_job")
		close(release)
		waitValidationSignal(t, old.done)
		if old.delivered.Load() {
			t.Fatal("premature delivery")
		}
		requireValidationAck(t, f.serve(f.start("acc_job", "V2", "300")))
		if f.maxOpen.Load() != 1 || f.closes.Load() != 1 || f.begins.Load() != 2 {
			t.Fatal("eviction did not close before Begin")
		}
	})
	t.Run("running_waiter", func(t *testing.T) {
		f := newValidationJobFixture(t, 1)
		entered := make(chan struct{})
		original := f.validate
		var call atomic.Int32
		f.validate = func(ctx context.Context, r io.Reader) (ImportSummary, error) {
			if call.Add(1) == 1 {
				close(entered)
				<-ctx.Done()
				return ImportSummary{}, ctx.Err()
			}
			return original(ctx, r)
		}
		answer := make(chan *httptest.ResponseRecorder, 1)
		go func() { answer <- f.serve(f.start("acc_job", "V1", "300")) }()
		waitValidationSignal(t, entered)
		requireValidationState(t, f.serve(f.start("acc_job", "V2", "0")), "running", validationBool(false))
		requireValidationState(t, <-answer, "running", validationBool(true))
		waitValidation(t, func() bool { return f.job("acc_job") == nil && f.closes.Load() == 1 })
		f.drained(t)
		if <-f.outcomes != "superseded" || <-f.reports != "backup validation job superseded" || f.closes.Load() != 1 {
			t.Fatal("supersede did not retire")
		}
		requireValidationAck(t, f.serve(f.start("acc_job", "V2", "300")))
	})
}

func TestBackupValidationJobStatusSemantics(t *testing.T) {
	f := newValidationJobFixture(t, 1)
	requireValidationAck(t, f.serve(f.start("acc_job", "V1", "300")))
	requireValidationState(t, f.status("acc_job", "V2"), "absent", nil)
	r := f.request("acc_job", "V1", "validate-backup-status", "")
	r.Header.Set(AccountBackupIDHeader, "backup_20261003T010000Z")
	requireValidationState(t, f.serve(r), "absent", nil)
	if f.reads.Load() != 0 {
		t.Fatal("local differing identity read lock")
	}
	for _, tc := range []struct {
		held  bool
		err   error
		state string
	}{{true, nil, "running"}, {false, nil, "absent"}, {false, errors.New("private lock failure"), ""}} {
		f.cfg.BackupValidationInProgress = func(context.Context, string) (bool, error) { return tc.held, tc.err }
		w := f.status("acc_other", "V1")
		if tc.err != nil {
			requireValidationError(t, w, 500, "could not validate backup")
		} else {
			requireValidationState(t, w, tc.state, nil)
		}
	}
	if f.begins.Load() != 1 || f.gets.Load() != 1 || f.closes.Load() != 0 {
		t.Fatal("status mutated registry or fetched archive")
	}
}

func TestBackupValidationJobRetentionEnds(t *testing.T) {
	for _, manual := range []bool{true, false} {
		name := "default"
		if manual {
			name = "injected"
		}
		t.Run(name, func(t *testing.T) {
			f := newValidationJobFixture(t, 1)
			j := f.cfg.backupValidationJobs
			j.retention = 50 * time.Millisecond
			fire := make(chan time.Time, 1)
			var calls, stops atomic.Int32
			called := make(chan struct{})
			if manual {
				j.retentionTimer = func(d time.Duration) (<-chan time.Time, func() bool) {
					if d != j.retention {
						t.Error("wrong retention duration")
					}
					if calls.Add(1) == 1 {
						close(called)
					}
					return fire, func() bool { stops.Add(1); return true }
				}
			}
			requireValidationAck(t, f.serve(f.start("acc_job", "V1", "300")))
			if manual {
				waitValidationSignal(t, called)
				requireValidationAck(t, f.status("acc_job", "V1"))
				if calls.Load() != 1 || f.job("acc_job") == nil || f.closes.Load() != 0 {
					t.Fatal("result not retained before timer")
				}
				fire <- time.Now()
			}
			waitValidation(t, func() bool { return f.closes.Load() == 1 && f.job("acc_job") == nil })
			f.drained(t)
			requireValidationState(t, f.status("acc_job", "V1"), "absent", nil)
			if manual && stops.Load() != 1 {
				t.Fatal("retention timer not stopped exactly once")
			}
		})
	}
}

func TestBackupValidationJobFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{{"conflict", ErrConflict, "backup validation target already contains the account"}, {"new", ErrArchiveTooNew, "backup schema is newer than this cell — upgrade the cell first"}, {"bad", ErrBadArchive, "invalid or mismatched backup archive"}, {"other", errors.New("private store failure"), "could not validate backup"}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newValidationJobFixture(t, 1)
			f.validate = func(context.Context, io.Reader) (ImportSummary, error) { return ImportSummary{}, tc.err }
			requireValidationError(t, f.serve(f.start("acc_job", "V1", "300")), 200, tc.want)
			if !f.job("acc_job").delivered.Load() {
				t.Fatal("inline START error did not mark result delivered")
			}
			requireValidationError(t, f.status("acc_job", "V1"), 200, tc.want)
			if <-f.reports != tc.want {
				t.Fatal("unclassified report")
			}
		})
	}
	for _, mode := range []string{"non-200", "size-mismatch", "short-body", "redirect"} {
		t.Run(mode, func(t *testing.T) {
			f := newValidationJobFixture(t, 1)
			f.responseMode = mode
			requireValidationError(t, f.serve(f.start("acc_job", "V1", "300")), 200, "backup archive download failed")
			if f.gets.Load() != 1 || f.validates.Load() != 0 {
				t.Fatal("invalid download validated or redirected")
			}
			f.noSpool(t)
		})
	}
}

func TestBackupValidationJobValidationBeforeNetwork(t *testing.T) {
	for _, mode := range []string{"id-missing", "id-bad", "id-long", "token", "size", "origin", "wait-301", "wait--1", "wait-abc", "wait-01", "wait-empty", "wait-two"} {
		t.Run(mode, func(t *testing.T) {
			f := newValidationJobFixture(t, 1)
			r := f.start("acc_job", "V1", "0")
			want := "a valid validation wait is required"
			switch mode {
			case "id-missing":
				r.Header.Del(BackupValidationIDHeader)
			case "id-bad":
				r.Header.Set(BackupValidationIDHeader, "V!")
			case "id-long":
				r.Header.Set(BackupValidationIDHeader, strings.Repeat("v", 129))
			case "token":
				r.Header.Del("X-Witself-Backup-Archive-Token")
			case "size":
				r.Header.Set("X-Witself-Backup-Archive-Size", "bad")
			case "origin":
				r.Header.Set("X-Witself-Backup-Archive-URL", "https://foreign.invalid/archive")
			case "wait-empty":
				r.Header.Set("X-Witself-Validation-Wait", "")
			case "wait-two":
				r.Header.Add("X-Witself-Validation-Wait", "0")
			default:
				r.Header.Set("X-Witself-Validation-Wait", strings.TrimPrefix(mode, "wait-"))
			}
			if strings.HasPrefix(mode, "id-") {
				want = "a valid validation id is required"
			} else if mode == "token" || mode == "size" || mode == "origin" {
				want = "a valid backup archive source is required"
			}
			requireValidationError(t, f.serve(r), 400, want)
			if f.gets.Load() != 0 || f.begins.Load() != 0 {
				t.Fatal("invalid request began work")
			}
		})
	}
	for _, wait := range []string{"", "0", "300"} {
		t.Run("accepted_"+wait, func(t *testing.T) {
			f := newValidationJobFixture(t, 1)
			f.cfg.BeginBackupValidation = func(context.Context, string, string) (BackupValidationLease, error) {
				return nil, ErrBackupValidationLeaseHeld
			}
			requireValidationState(t, f.serve(f.start("acc_job", "V1", wait)), "running", validationBool(false))
			if f.gets.Load() != 0 {
				t.Fatal("wait validation fetched")
			}
		})
	}
}

func TestBackupValidationJobAuthorityAndRouting(t *testing.T) {
	for _, action := range []string{"start-validate-backup", "validate-backup-status"} {
		t.Run(action, func(t *testing.T) {
			f := newValidationJobFixture(t, 1)
			f.cfg.ProvisionAccountExact = func(context.Context, string, string, string, string, string) (ProvisionedAccount, error) {
				return ProvisionedAccount{}, nil
			}
			f.cfg.SuspendAccountSystem = func(context.Context, string, string, string) error { return nil }
			f.cfg.BeginBackupValidation = func(context.Context, string, string) (BackupValidationLease, error) {
				return nil, ErrBackupValidationLeaseHeld
			}
			serve := func(r *http.Request) *httptest.ResponseRecorder {
				w := httptest.NewRecorder()
				apiMux(f.cfg).ServeHTTP(w, r)
				return w
			}
			r := f.request("acc_job", "V1", action, "0")
			r.Header.Set("Authorization", "Bearer test-provision")
			requireValidationError(t, serve(r), 401, "invalid backup token")
			r.Header.Set("Authorization", "Bearer test-backup")
			w := serve(r)
			if action == "start-validate-backup" {
				requireValidationState(t, w, "running", validationBool(false))
			} else {
				requireValidationState(t, w, "absent", nil)
			}
			f.cfg.BackupValidationEnabled = false
			r.Header.Del("Authorization")
			requireValidationError(t, serve(r), 404, "unknown backup action")
			f.cfg.BackupValidationEnabled = true
			r.URL.Path += "x"
			r.Header.Set("Authorization", "Bearer test-backup")
			w = serve(r)
			if w.Code != 401 {
				t.Fatal("similar action bypassed lifecycle authority")
			}
			if f.gets.Load() != 0 {
				t.Fatal("routing test fetched archive")
			}
		})
	}
}

func TestBackupValidationJobCallerGone(t *testing.T) {
	f := newValidationJobFixture(t, 1)
	entered := make(chan struct{})
	release := make(chan struct{})
	original := f.validate
	f.validate = func(ctx context.Context, r io.Reader) (ImportSummary, error) {
		close(entered)
		select {
		case <-release:
			return original(ctx, r)
		case <-ctx.Done():
			return ImportSummary{}, ctx.Err()
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	answer := make(chan *httptest.ResponseRecorder, 1)
	go func() { answer <- f.serve(f.start("acc_job", "V1", "300").WithContext(ctx)) }()
	waitValidationSignal(t, entered)
	cancel()
	w := <-answer
	if strings.TrimSpace(w.Body.String()) != "" {
		t.Fatal("disconnected waiter received terminal object")
	}
	job := f.job("acc_job")
	close(release)
	waitValidationSignal(t, job.done)
	requireValidationAck(t, f.status("acc_job", "V1"))
	if f.closes.Load() != 0 {
		t.Fatal("caller cancellation closed retained session")
	}
}

func TestBackupValidationJobRemovalPrecedesLeaseClose(t *testing.T) {
	f := newValidationJobFixture(t, 1)
	f.onClose = func() {
		if f.job("acc_job") != nil {
			t.Error("lease closed before registry removal")
		}
	}
	requireValidationAck(t, f.serve(f.start("acc_job", "V1", "300")))
	f.cfg.backupValidationJobs.cancel()
	f.drained(t)
	if f.closes.Load() != 1 {
		t.Fatal("lease not closed exactly once")
	}
}
func TestBackupValidationJobShutdown(t *testing.T) {
	f := newValidationJobFixture(t, 2)
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	var calls atomic.Int32
	f.validate = func(ctx context.Context, _ io.Reader) (ImportSummary, error) {
		if calls.Add(1) == 1 {
			return ImportSummary{AccountID: "acc_other", Status: "active", SchemaVersion: 98, BackupID: "backup_20261003T000000Z"}, nil
		}
		close(entered)
		<-ctx.Done()
		<-release
		return ImportSummary{}, ctx.Err()
	}
	requireValidationAck(t, f.serve(f.start("acc_other", "retained", "300")))
	waiter := make(chan *httptest.ResponseRecorder, 1)
	go func() { waiter <- f.serve(f.start("acc_job", "shutdown", "300")) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("validation did not begin")
	}
	f.cfg.backupValidationJobs.cancel()
	var answer *httptest.ResponseRecorder
	select {
	case answer = <-waiter:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not detach the inline waiter")
	}
	started := true
	requireValidationState(t, answer, "running", &started)
	requireValidationState(t, f.serve(f.start("acc_job", "shutdown", "0")), "running", &started)
	requireValidationState(t, f.status("acc_job", "shutdown"), "running", nil)
	waitValidation(t, func() bool { return f.closes.Load() == 1 })
	if f.open.Load() != 1 {
		t.Fatal("shutdown did not retire the other account's retained lease")
	}
	if len(f.cfg.backupValidationJobs.sem) != 1 {
		t.Fatal("shutdown refusal test needs one free running slot")
	}
	beginCalls := f.begins.Load()
	requireValidationError(t, f.serve(f.start("acc_new", "after-shutdown", "0")), http.StatusServiceUnavailable, "validation capacity exhausted")
	if f.begins.Load() != beginCalls {
		t.Fatal("START acquired a lease after shutdown began")
	}
	releaseOnce.Do(func() { close(release) })
	f.drained(t)
	if f.closes.Load() != 2 {
		t.Fatalf("lease close calls = %d, want 2", f.closes.Load())
	}
	requireValidationOutcome(t, f, "cancelled")
	requireValidationReport(t, f, "backup validation job cancelled by shutdown")
}

func TestBackupValidationJobDeadline(t *testing.T) {
	t.Run("failure", func(t *testing.T) {
		f := newValidationJobFixture(t, 1)
		f.cfg.BackupValidationJobTimeout = 50 * time.Millisecond
		var entered atomic.Bool
		var retentionCalls atomic.Int32
		f.cfg.backupValidationJobs.retentionTimer = func(time.Duration) (<-chan time.Time, func() bool) {
			retentionCalls.Add(1)
			return make(chan time.Time), func() bool { return true }
		}
		f.validate = func(ctx context.Context, _ io.Reader) (ImportSummary, error) {
			entered.Store(true)
			<-ctx.Done()
			return ImportSummary{}, ctx.Err()
		}
		answer := f.serve(f.start("acc_job", "deadline", "300"))
		message := "backup archive download failed"
		if entered.Load() {
			message = "could not validate backup"
		}
		requireValidationError(t, answer, http.StatusOK, message)
		requireValidationOutcome(t, f, "deadline")
		requireValidationReport(t, f, "backup validation job deadline exceeded")
		waitValidation(t, func() bool { return f.closes.Load() == 1 && f.job("acc_job") == nil })
		if retentionCalls.Load() != 0 {
			t.Fatal("deadline result entered retention")
		}
		requireValidationState(t, f.status("acc_job", "deadline"), "absent", nil)
		f.drained(t)
	})
	for _, tc := range []struct {
		name string
		held bool
	}{{"success_held", true}, {"success_session_ended", false}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newValidationJobFixture(t, 1)
			f.cfg.BackupValidationJobTimeout = time.Second
			f.held.Store(tc.held)
			var retentionCalls atomic.Int32
			f.cfg.backupValidationJobs.retentionTimer = func(time.Duration) (<-chan time.Time, func() bool) {
				retentionCalls.Add(1)
				return make(chan time.Time), func() bool { return true }
			}
			original := f.validate
			f.validate = func(ctx context.Context, r io.Reader) (ImportSummary, error) {
				sum, err := original(ctx, r)
				// Synchronize on the actual deadline, with no scheduling sleep:
				// validation succeeded, but disposition sees an expired context.
				<-ctx.Done()
				if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
					t.Error("success did not race the job deadline")
				}
				return sum, err
			}
			requireValidationAck(t, f.serve(f.start("acc_job", "deadline-success", "300")))
			requireValidationOutcome(t, f, "validated")
			select {
			case report := <-f.reports:
				t.Fatalf("successful validation emitted failure report: %q", report)
			default:
			}
			if tc.held {
				waitValidation(t, func() bool { return retentionCalls.Load() == 1 })
				job := f.job("acc_job")
				if job == nil || !job.retained || f.closes.Load() != 0 {
					t.Fatal("successful validation with held lease was not retained")
				}
				requireValidationAck(t, f.status("acc_job", "deadline-success"))
			} else {
				f.drained(t)
				if retentionCalls.Load() != 0 || f.closes.Load() != 1 || f.job("acc_job") != nil {
					t.Fatal("successful validation without held lease entered retention")
				}
				requireValidationState(t, f.status("acc_job", "deadline-success"), "absent", nil)
			}
		})
	}
}

func TestBackupValidationJobSessionEnded(t *testing.T) {
	for _, failed := range []bool{false, true} {
		name := "success"
		if failed {
			name = "failure"
		}
		t.Run(name, func(t *testing.T) {
			f := newValidationJobFixture(t, 1)
			f.held.Store(false)
			var retentionCalls atomic.Int32
			f.cfg.backupValidationJobs.retentionTimer = func(time.Duration) (<-chan time.Time, func() bool) {
				retentionCalls.Add(1)
				return make(chan time.Time), func() bool { return true }
			}
			f.validate = func(context.Context, io.Reader) (ImportSummary, error) {
				if failed {
					return ImportSummary{}, errors.New("session ended")
				}
				return ImportSummary{AccountID: "acc_job", Status: "active", SchemaVersion: 98, BackupID: "backup_20261003T000000Z"}, nil
			}
			answer := f.serve(f.start("acc_job", "session-ended", "300"))
			if failed {
				started := true
				requireValidationState(t, answer, "running", &started)
				requireValidationOutcome(t, f, "failed")
				requireValidationReport(t, f, "could not validate backup")
			} else {
				requireValidationAck(t, answer)
				requireValidationOutcome(t, f, "validated")
			}
			waitValidation(t, func() bool { return f.closes.Load() == 1 && f.job("acc_job") == nil })
			if retentionCalls.Load() != 0 {
				t.Fatal("session-ended job entered retention")
			}
			requireValidationState(t, f.status("acc_job", "session-ended"), "absent", nil)
			f.drained(t)
		})
	}
}

func TestBackupValidationJobRetiring(t *testing.T) {
	for _, outcome := range []string{"cancelled", "superseded"} {
		t.Run(outcome, func(t *testing.T) {
			f := newValidationJobFixture(t, 1)
			entered := make(chan struct{})
			published := make(chan *backupValidationJob, 1)
			release := make(chan struct{})
			var releaseOnce sync.Once
			t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
			var lockHeld atomic.Bool
			lockHeld.Store(true)
			f.cfg.BackupValidationInProgress = func(context.Context, string) (bool, error) {
				f.reads.Add(1)
				return lockHeld.Load(), nil
			}
			f.cfg.backupValidationJobs.afterPublish = func(job *backupValidationJob) {
				published <- job
				<-release
			}
			f.validate = func(ctx context.Context, _ io.Reader) (ImportSummary, error) {
				close(entered)
				<-ctx.Done()
				return ImportSummary{}, ctx.Err()
			}
			waiter := make(chan *httptest.ResponseRecorder, 1)
			go func() { waiter <- f.serve(f.start("acc_job", "old", "300")) }()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("validation did not begin")
			}
			if outcome == "cancelled" {
				f.cfg.backupValidationJobs.cancel()
			} else {
				started := false
				requireValidationState(t, f.serve(f.start("acc_job", "new", "0")), "running", &started)
			}
			var old *backupValidationJob
			select {
			case old = <-published:
			case <-time.After(time.Second):
				t.Fatal("non-retained job did not reach afterPublish")
			}
			if old.result != nil || old.retained {
				t.Fatalf("%s published a result or entered retention", outcome)
			}
			var answer *httptest.ResponseRecorder
			select {
			case answer = <-waiter:
			case <-time.After(time.Second):
				t.Fatal("inline waiter did not finish")
			}
			started := true
			requireValidationState(t, answer, "running", &started)
			for _, account := range []string{"acc_job", "acc_other"} {
				answer := f.serve(f.start(account, "old", "0"))
				requireValidationError(t, answer, http.StatusServiceUnavailable, "validation capacity exhausted")
				if answer.Header().Get("Retry-After") != "60" {
					t.Fatal("capacity refusal omitted Retry-After: 60")
				}
			}
			if f.begins.Load() != 1 || f.gets.Load() != 1 || f.open.Load() != 1 || f.maxOpen.Load() != 1 || f.closes.Load() != 0 {
				t.Fatal("retiring job released its capacity before its lease was closed")
			}
			requireValidationState(t, f.status("acc_job", "old"), "running", nil)
			lockHeld.Store(false)
			requireValidationState(t, f.status("acc_job", "old"), "absent", nil)
			if f.reads.Load() != 2 {
				t.Fatal("status read the retiring entry instead of consulting the lock")
			}
			releaseOnce.Do(func() { close(release) })
			waitValidation(t, func() bool { return f.job("acc_job") == nil && f.closes.Load() == 1 })
			requireValidationOutcome(t, f, outcome)
			f.drained(t)
		})
	}
}

func TestBackupValidationJobReplacement(t *testing.T) {
	f := newValidationJobFixture(t, 2)
	f.cfg.BackupValidationJobTimeout = 500 * time.Millisecond
	published := make(chan *backupValidationJob, 1)
	releaseOld := make(chan struct{})
	releaseNew := make(chan struct{})
	enteredNew := make(chan struct{})
	var oldOnce, newOnce sync.Once
	t.Cleanup(func() {
		oldOnce.Do(func() { close(releaseOld) })
		newOnce.Do(func() { close(releaseNew) })
	})
	var publishCalls atomic.Int32
	var oldPublished atomic.Bool
	f.cfg.backupValidationJobs.afterPublish = func(job *backupValidationJob) {
		if publishCalls.Add(1) == 1 {
			oldPublished.Store(true)
			published <- job
			<-releaseOld
		}
	}
	f.validate = func(ctx context.Context, _ io.Reader) (ImportSummary, error) {
		if !oldPublished.Load() {
			<-ctx.Done()
			return ImportSummary{}, ctx.Err()
		}
		close(enteredNew)
		select {
		case <-releaseNew:
			return ImportSummary{AccountID: "acc_job", Status: "active", SchemaVersion: 98, BackupID: "backup_20261003T000000Z"}, nil
		case <-ctx.Done():
			return ImportSummary{}, ctx.Err()
		}
	}
	started := true
	requireValidationState(t, f.serve(f.start("acc_job", "same-id", "0")), "running", &started)
	var old *backupValidationJob
	select {
	case old = <-published:
	case <-time.After(time.Second):
		t.Fatal("deadline entry did not reach afterPublish")
	}
	if old.retained || old.result == nil {
		t.Fatal("deadline entry must be retiring with an attached-waiter result")
	}
	requireValidationOutcome(t, f, "deadline")
	f.cfg.BackupValidationJobTimeout = time.Minute
	requireValidationState(t, f.serve(f.start("acc_job", "same-id", "0")), "running", &started)
	newJob := f.job("acc_job")
	if newJob == nil || newJob == old {
		t.Fatal("START attached to a retiring entry instead of replacing it")
	}
	select {
	case <-enteredNew:
	case <-time.After(time.Second):
		t.Fatal("replacement validation did not begin")
	}
	if f.begins.Load() != 2 || f.open.Load() != 2 {
		t.Fatal("replacement did not acquire its independent session")
	}
	oldOnce.Do(func() { close(releaseOld) })
	waitValidation(t, func() bool { return f.closes.Load() == 1 })
	if f.job("acc_job") != newJob || f.open.Load() != 1 {
		t.Fatal("retiring old entry removed or closed its replacement")
	}
	requireValidationState(t, f.status("acc_job", "same-id"), "running", nil)
	newOnce.Do(func() { close(releaseNew) })
	waitValidation(t, newJob.finished)
	requireValidationAck(t, f.status("acc_job", "same-id"))
	f.cfg.backupValidationJobs.cancel()
	f.drained(t)
}

func requireValidationOutcome(t *testing.T, f *validationJobFixture, want string) {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		select {
		case outcome := <-f.outcomes:
			if outcome == want {
				return
			}
		case <-timer.C:
			t.Fatalf("validation outcome %q was not reported", want)
		}
	}
}

func requireValidationReport(t *testing.T, f *validationJobFixture, want string) {
	t.Helper()
	select {
	case report := <-f.reports:
		if report != want {
			t.Fatalf("failure report = %q, want %q", report, want)
		}
	case <-time.After(time.Second):
		t.Fatalf("failure report %q was not emitted", want)
	}
}

type heldCallbackBackupValidationLease struct {
	BackupValidationLease
	held func() bool
}

func (l *heldCallbackBackupValidationLease) Held() bool { return l.held() }

func TestBackupValidationJobShutdownDuringHeld(t *testing.T) {
	f := newValidationJobFixture(t, 1)
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	begin := f.cfg.BeginBackupValidation
	f.cfg.BeginBackupValidation = func(ctx context.Context, accountID, backupID string) (BackupValidationLease, error) {
		lease, err := begin(ctx, accountID, backupID)
		if err != nil {
			return nil, err
		}
		return &heldCallbackBackupValidationLease{BackupValidationLease: lease, held: func() bool {
			close(entered)
			<-release
			return lease.Held()
		}}, nil
	}
	var retentionCalls atomic.Int32
	f.cfg.backupValidationJobs.retentionTimer = func(time.Duration) (<-chan time.Time, func() bool) {
		retentionCalls.Add(1)
		return make(chan time.Time), func() bool { return true }
	}
	published := make(chan *backupValidationJob, 1)
	f.cfg.backupValidationJobs.afterPublish = func(job *backupValidationJob) { published <- job }
	waiter := make(chan *httptest.ResponseRecorder, 1)
	go func() { waiter <- f.serve(f.start("acc_job", "cancel-during-held", "300")) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("job did not reach Held")
	}
	f.cfg.backupValidationJobs.cancel()
	started := true
	select {
	case answer := <-waiter:
		requireValidationState(t, answer, "running", &started)
	case <-time.After(time.Second):
		t.Fatal("shutdown did not detach waiter while Held was blocked")
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case job := <-published:
		if job.result != nil || job.retained {
			t.Fatal("shutdown during Held published or retained a stale result")
		}
	case <-time.After(time.Second):
		t.Fatal("job did not publish after Held returned")
	}
	f.drained(t)
	requireValidationOutcome(t, f, "cancelled")
	requireValidationReport(t, f, "backup validation job cancelled by shutdown")
	if retentionCalls.Load() != 0 || f.closes.Load() != 1 {
		t.Fatal("cancelled job retained its result or failed to close its lease once")
	}
	requireValidationState(t, f.status("acc_job", "cancel-during-held"), "absent", nil)
}

func TestBackupValidationJobSupersedeAfterDeadline(t *testing.T) {
	f := newValidationJobFixture(t, 1)
	// Leave ample time for TLS under -race; hold Validate after the deadline so
	// supersede ordering depends on channels rather than scheduler timing.
	f.cfg.BackupValidationJobTimeout = 2 * time.Second
	entered := make(chan struct{})
	expired := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	f.validate = func(ctx context.Context, _ io.Reader) (ImportSummary, error) {
		close(entered)
		<-ctx.Done()
		close(expired)
		<-release
		return ImportSummary{}, ctx.Err()
	}
	var retentionCalls atomic.Int32
	f.cfg.backupValidationJobs.retentionTimer = func(time.Duration) (<-chan time.Time, func() bool) {
		retentionCalls.Add(1)
		return make(chan time.Time), func() bool { return true }
	}
	waiter := make(chan *httptest.ResponseRecorder, 1)
	go func() { waiter <- f.serve(f.start("acc_job", "expired", "300")) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("validation did not begin")
	}
	select {
	case <-expired:
	case <-time.After(3 * time.Second):
		t.Fatal("validation deadline did not expire")
	}
	declined := false
	requireValidationState(t, f.serve(f.start("acc_job", "replacement", "0")), "running", &declined)
	releaseOnce.Do(func() { close(release) })
	started := true
	select {
	case answer := <-waiter:
		requireValidationState(t, answer, "running", &started)
	case <-time.After(time.Second):
		t.Fatal("superseded waiter did not finish")
	}
	f.drained(t)
	requireValidationOutcome(t, f, "superseded")
	requireValidationReport(t, f, "backup validation job superseded")
	if retentionCalls.Load() != 0 || f.closes.Load() != 1 {
		t.Fatal("superseded expired job retained its result or failed to close once")
	}
	requireValidationState(t, f.status("acc_job", "expired"), "absent", nil)
}

func TestBackupValidationJobClosingRetainedSessionCountsCapacity(t *testing.T) {
	for _, mode := range []string{"retention expiry", "same-account eviction"} {
		t.Run(mode, func(t *testing.T) {
			f := newValidationJobFixture(t, 1)
			timer := make(chan time.Time, 1)
			timerStarted := make(chan struct{}, 2)
			f.cfg.backupValidationJobs.retentionTimer = func(time.Duration) (<-chan time.Time, func() bool) {
				timerStarted <- struct{}{}
				return timer, func() bool { return true }
			}
			closing := make(chan struct{})
			release := make(chan struct{})
			var releaseOnce sync.Once
			t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
			var closeCalls atomic.Int32
			f.onClose = func() {
				if closeCalls.Add(1) == 1 {
					close(closing)
					<-release
				}
			}
			requireValidationAck(t, f.serve(f.start("acc_job", "old", "300")))
			select {
			case <-timerStarted:
			case <-time.After(time.Second):
				t.Fatal("retained job did not arm retention")
			}
			var replacement chan *httptest.ResponseRecorder
			if mode == "retention expiry" {
				timer <- time.Now()
			} else {
				replacement = make(chan *httptest.ResponseRecorder, 1)
				go func() { replacement <- f.serve(f.start("acc_job", "replacement", "300")) }()
			}
			select {
			case <-closing:
			case <-time.After(time.Second):
				t.Fatal("retained lease did not begin closing")
			}
			if f.job("acc_job") != nil {
				t.Fatal("retained entry remained visible during Close")
			}
			requireValidationError(t, f.serve(f.start("acc_other", "other", "0")), http.StatusServiceUnavailable, "validation capacity exhausted")
			if f.begins.Load() != 1 || f.gets.Load() != 1 {
				t.Fatal("another session started before retained Close returned")
			}
			releaseOnce.Do(func() { close(release) })
			if replacement != nil {
				select {
				case answer := <-replacement:
					requireValidationAck(t, answer)
				case <-time.After(time.Second):
					t.Fatal("replacement did not start after retained Close returned")
				}
			} else {
				f.drained(t)
				requireValidationAck(t, f.serve(f.start("acc_other", "other", "300")))
			}
			if f.begins.Load() != 2 {
				t.Fatal("capacity was not restored after retained Close returned")
			}
		})
	}
}
