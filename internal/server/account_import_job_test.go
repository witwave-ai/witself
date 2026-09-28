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
	"sync/atomic"
	"testing"
	"time"
)

type fakeImportLease struct {
	receipt  bool
	importFn func(context.Context, io.Reader) (ImportSummary, error)
	closeFn  func()
}

func importTestSummary() ImportSummary {
	return ImportSummary{AccountID: "acc_job", EvacuationID: "evac_job", Status: "suspended", EvacuationRole: "target", SchemaVersion: 98}
}
func (l *fakeImportLease) Receipt() (ImportSummary, bool) {
	s := importTestSummary()
	s.AlreadyImported = true
	return s, l.receipt
}
func (l *fakeImportLease) Import(ctx context.Context, r io.Reader) (ImportSummary, error) {
	return l.importFn(ctx, r)
}
func (l *fakeImportLease) Close() {
	if l.closeFn != nil {
		l.closeFn()
	}
}

type importJobFixture struct {
	responseMode                           string
	cfg                                    Config
	client                                 *http.Client
	cp                                     *httptest.Server
	gets, imports, begins, closes, reports atomic.Int32
	imported                               atomic.Bool
	lease                                  *fakeImportLease
	dir                                    string
}

func newImportJobFixture(t *testing.T) *importJobFixture {
	t.Helper()
	f := &importJobFixture{dir: t.TempDir()}
	t.Setenv("TMPDIR", f.dir)
	f.cp = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.gets.Add(1)
		if r.ProtoMajor != 1 || r.Header.Get("Authorization") != "Bearer cap_"+strings.Repeat("a", 64) {
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
	f.lease = &fakeImportLease{importFn: func(_ context.Context, r io.Reader) (ImportSummary, error) {
		f.imports.Add(1)
		data, err := io.ReadAll(r)
		if err != nil || string(data) != "archive" {
			t.Error("bad spool")
		}
		f.imported.Store(true)
		return importTestSummary(), nil
	}, closeFn: func() { f.closes.Add(1) }}
	f.cfg = Config{ProvisionToken: "job-test-provision", BackupValidationArchiveOrigin: f.cp.URL,
		accountImportJobs: newAccountImportJobs(context.Background(), 1),
		ImportAccountArchive: func(context.Context, string, string, io.Reader) (ImportSummary, error) {
			t.Error("sync path used")
			return ImportSummary{}, nil
		},
		BeginAccountImport: func(context.Context, string, string) (AccountImportLease, error) {
			f.begins.Add(1)
			return f.lease, nil
		},
		AccountImportStatus: func(context.Context, string, string) (ImportSummary, AccountImportState, error) {
			s := importTestSummary()
			s.AlreadyImported = true
			if f.imported.Load() {
				return s, AccountImportImported, nil
			}
			return s, AccountImportAbsent, nil
		},
		ReportAccountImportFailure: func(_ context.Context, _ string, err error) {
			f.reports.Add(1)
			if strings.Contains(err.Error(), f.cp.URL) || strings.Contains(err.Error(), "cap_") {
				t.Error("secret in diagnostic")
			}
		},
	}
	t.Cleanup(func() {
		f.cfg.accountImportJobs.cancel()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		f.cfg.accountImportJobs.wait(ctx)
	})
	return f
}
func (f *importJobFixture) request(action, wait string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/accounts/acc_job:"+action, nil)
	r.Header.Set("Authorization", "Bearer job-test-provision")
	r.Header.Set(AccountEvacuationIDHeader, "evac_job")
	if action == "start-import-evacuation" {
		r.Header.Set("X-Witself-Archive-URL", f.cp.URL+"/v1/archives:pull")
		r.Header.Set("X-Witself-Archive-Token", "cap_"+strings.Repeat("a", 64))
		r.Header.Set("X-Witself-Archive-Size", "7")
		if wait != "" {
			r.Header.Set("X-Witself-Import-Wait", wait)
		}
	}
	return r
}
func importResult(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var b map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(w.Body.String(), "cap_") {
		t.Fatal("capability leaked")
	}
	return b
}
func (f *importJobFixture) serve(r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	accountLifecycleHandlerWithArchiveClient(f.cfg, f.client)(w, r)
	return w
}
func (f *importJobFixture) drained(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	f.cfg.accountImportJobs.wait(ctx)
	if ctx.Err() != nil {
		t.Fatal("job failed to drain")
	}
	files, err := filepath.Glob(filepath.Join(f.dir, "witself-account-import-*.tar.gz"))
	if err != nil || len(files) != 0 {
		t.Fatal("spool leaked")
	}
}

func TestAccountImportJobInlineAndReceipt(t *testing.T) {
	f := newImportJobFixture(t)
	w := f.serve(f.request("start-import-evacuation", "300"))
	b := importResult(t, w)
	if w.Code != 200 || len(b) != 8 || b["archive_schema_version"] != float64(98) {
		t.Fatal("missing exact archive acknowledgement")
	}
	f.drained(t)
	for _, action := range []string{"start-import-evacuation", "import-evacuation-status"} {
		b = importResult(t, f.serve(f.request(action, "0")))
		if len(b) != 7 || b["already_imported"] != true || b["archive_schema_version"] != nil {
			t.Fatal("receipt fabricated manifest")
		}
	}
	if f.gets.Load() != 1 || f.imports.Load() != 1 {
		t.Fatal("receipt redownloaded")
	}
}
func TestAccountImportJobLeaseReceipt(t *testing.T) {
	f := newImportJobFixture(t)
	f.lease.receipt = true
	b := importResult(t, f.serve(f.request("start-import-evacuation", "1")))
	f.drained(t)
	if len(b) != 7 || f.gets.Load() != 0 {
		t.Fatal("lease receipt downloaded")
	}
}
func TestAccountImportJobDuplicateCapacityAndDetachedRequest(t *testing.T) {
	f := newImportJobFixture(t)
	release := make(chan struct{})
	original := f.lease.importFn
	f.lease.importFn = func(ctx context.Context, r io.Reader) (ImportSummary, error) {
		select {
		case <-release:
			return original(ctx, r)
		case <-ctx.Done():
			return ImportSummary{}, ctx.Err()
		}
	}
	handler := accountLifecycleHandlerWithArchiveClient(f.cfg, f.client)
	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		r := f.request("start-import-evacuation", "0").WithContext(ctx)
		w := httptest.NewRecorder()
		handler(w, r)
		cancel()
		b := importResult(t, w)
		if b["import_job"].(map[string]any)["started"] != true {
			t.Fatal("not started")
		}
	}
	r := f.request("start-import-evacuation", "0")
	r.URL.Path = "/v1/accounts/acc_other:start-import-evacuation"
	w := httptest.NewRecorder()
	handler(w, r)
	if w.Code != 503 || w.Header().Get("Retry-After") != "60" || f.begins.Load() != 1 {
		t.Fatal("capacity or duplicate ownership broken")
	}
	close(release)
	f.drained(t)
	if f.gets.Load() != 1 || f.imports.Load() != 1 {
		t.Fatal("duplicate job")
	}
}
func TestAccountImportJobAcquireOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		code int
	}{{"held", ErrAccountImportLeaseHeld, 200}, {"unavailable", ErrAccountImportLeaseUnavailable, 503}, {"conflict", ErrConflict, 409}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newImportJobFixture(t)
			f.cfg.BeginAccountImport = func(context.Context, string, string) (AccountImportLease, error) { return nil, tc.err }
			w := f.serve(f.request("start-import-evacuation", "1"))
			if w.Code != tc.code || f.gets.Load() != 0 {
				t.Fatal("acquire consumed capability")
			}
			if tc.code == 200 && importResult(t, w)["import_job"].(map[string]any)["started"] != false {
				t.Fatal("foreign job reported started")
			}
			if tc.code == 503 && w.Header().Get("Retry-After") != "60" {
				t.Fatal("missing retry")
			}
		})
	}
}
func TestAccountImportJobFailuresAndCancellation(t *testing.T) {
	for _, mode := range []string{"download", "import", "shutdown", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			f := newImportJobFixture(t)
			entered := make(chan struct{})
			release := make(chan struct{})
			report := make(chan string, 1)
			f.cfg.ReportAccountImportFailure = func(_ context.Context, _ string, err error) { f.reports.Add(1); report <- err.Error() }
			if mode == "download" {
				f.cp.Close()
			} else {
				f.lease.importFn = func(ctx context.Context, _ io.Reader) (ImportSummary, error) {
					close(entered)
					if mode == "import" {
						<-release
					}
					if mode == "shutdown" || mode == "deadline" {
						<-ctx.Done()
					}
					return ImportSummary{}, errors.New("private upstream detail")
				}
			}
			wait := "1"
			if mode == "shutdown" || mode == "import" {
				wait = "0"
			}
			if mode == "deadline" {
				f.cfg.AccountImportJobTimeout = 20 * time.Millisecond
			}
			w := f.serve(f.request("start-import-evacuation", wait))
			if mode == "import" {
				close(release)
			}
			if mode == "shutdown" {
				<-entered
				f.cfg.accountImportJobs.cancel()
			}
			f.drained(t)
			if f.closes.Load() != 1 || f.reports.Load() != 1 {
				t.Fatal("missing cleanup/report")
			}
			msg := <-report
			if strings.Contains(msg, "private") || strings.Contains(msg, "cap_") || strings.Contains(msg, f.cp.URL) {
				t.Fatal("diagnostic disclosed source")
			}
			if mode == "shutdown" && msg != "import job cancelled by shutdown" {
				t.Fatal("wrong shutdown classification")
			}
			if mode == "deadline" && msg != "import job deadline exceeded" {
				t.Fatal("wrong deadline classification")
			}
			if wait == "1" && importResult(t, w)["error"] == nil {
				t.Fatal("missing inline error")
			}
			b := importResult(t, f.serve(f.request("import-evacuation-status", "")))
			if b["import_job"].(map[string]any)["state"] != "absent" {
				t.Fatal("failed job emitted durable failure")
			}
		})
	}
}
func TestAccountImportJobRemovalPrecedesLeaseClose(t *testing.T) {
	f := newImportJobFixture(t)
	f.cfg.accountImportJobs = newAccountImportJobs(context.Background(), 2)
	closing := make(chan struct{})
	release := make(chan struct{})
	first := true
	f.cfg.BeginAccountImport = func(context.Context, string, string) (AccountImportLease, error) {
		f.begins.Add(1)
		if first {
			first = false
			return &fakeImportLease{receipt: true, closeFn: func() { close(closing); <-release }}, nil
		}
		return &fakeImportLease{receipt: true}, nil
	}
	f.serve(f.request("start-import-evacuation", "0"))
	<-closing
	f.serve(f.request("start-import-evacuation", "1"))
	close(release)
	f.drained(t)
	if f.begins.Load() != 2 {
		t.Fatal("attached to terminal job")
	}
}
func TestAccountImportJobValidationBeforeNetwork(t *testing.T) {
	for _, mode := range []string{"wrong-origin", "wrong-port", "http", "missing-config", "invalid-config", "bad-token", "bad-size", "too-large", "zero", "negative", "empty-url", "userinfo", "fragment", "wait-301", "wait--1", "wait-abc", "wait-01", "wait-empty", "evacuation", "auth", "conflict"} {
		t.Run(mode, func(t *testing.T) {
			f := newImportJobFixture(t)
			r := f.request("start-import-evacuation", "0")
			want := 400
			switch mode {
			case "wrong-origin":
				f.cfg.BackupValidationArchiveOrigin = "https://wrong.invalid"
			case "wrong-port":
				f.cfg.BackupValidationArchiveOrigin = "https://127.0.0.1:1"
			case "http":
				r.Header.Set("X-Witself-Archive-URL", "http://127.0.0.1/archive")
			case "missing-config":
				f.cfg.BackupValidationArchiveOrigin = ""
			case "invalid-config":
				f.cfg.BackupValidationArchiveOrigin = f.cp.URL + "/path"
			case "bad-token":
				r.Header.Set("X-Witself-Archive-Token", "bad")
			case "bad-size":
				r.Header.Set("X-Witself-Archive-Size", "1.5")
			case "too-large":
				r.Header.Set("X-Witself-Archive-Size", "8589934593")
			case "zero":
				r.Header.Set("X-Witself-Archive-Size", "0")
			case "negative":
				r.Header.Set("X-Witself-Archive-Size", "-1")
			case "empty-url":
				r.Header.Set("X-Witself-Archive-URL", "")
			case "userinfo":
				r.Header.Set("X-Witself-Archive-URL", strings.Replace(f.cp.URL, "https://", "https://user@", 1))
			case "fragment":
				r.Header.Set("X-Witself-Archive-URL", f.cp.URL+"/#fragment")
			case "evacuation":
				r.Header.Del(AccountEvacuationIDHeader)
			case "auth":
				r.Header.Del("Authorization")
				want = 401
			case "conflict":
				f.cfg.AccountImportStatus = func(context.Context, string, string) (ImportSummary, AccountImportState, error) {
					return ImportSummary{}, AccountImportAbsent, ErrConflict
				}
				want = 409
			default:
				raw := strings.TrimPrefix(mode, "wait-")
				if raw == "empty" {
					raw = ""
				}
				r.Header.Set("X-Witself-Import-Wait", raw)
			}
			w := f.serve(r)
			if w.Code != want || f.gets.Load() != 0 || f.begins.Load() != 0 {
				t.Fatal("invalid request reached capability or lease")
			}
		})
	}
}
func TestAccountImportStatusValidation(t *testing.T) {
	f := newImportJobFixture(t)
	r := f.request("import-evacuation-status", "")
	r.Header.Del(AccountEvacuationIDHeader)
	if f.serve(r).Code != 400 {
		t.Fatal("missing epoch accepted")
	}
	f.cfg.AccountImportStatus = func(context.Context, string, string) (ImportSummary, AccountImportState, error) {
		return ImportSummary{}, AccountImportAbsent, ErrConflict
	}
	if f.serve(f.request("import-evacuation-status", "")).Code != 409 {
		t.Fatal("conflict accepted")
	}
}
func TestAccountImportClientTimeouts(t *testing.T) {
	if newBackupArchiveClient().Timeout != 15*time.Minute || newBackupArchiveClientWithTimeout(accountImportTimeout(Config{})).Timeout != 3*time.Hour {
		t.Fatal("incorrect download budgets")
	}
}

func TestAccountImportJobDownloadFailures(t *testing.T) {
	for _, mode := range []string{"redirect", "non-200", "size-mismatch", "short-body"} {
		t.Run(mode, func(t *testing.T) {
			f := newImportJobFixture(t)
			f.responseMode = mode
			b := importResult(t, f.serve(f.request("start-import-evacuation", "1")))
			f.drained(t)
			if b["error"] != "archive download failed" || f.gets.Load() != 1 || f.imports.Load() != 0 || f.reports.Load() != 1 {
				t.Fatal("download failure escaped classification or retried")
			}
		})
	}
}
func TestAccountImportJobInlineCallerGone(t *testing.T) {
	f := newImportJobFixture(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	original := f.lease.importFn
	f.lease.importFn = func(ctx context.Context, r io.Reader) (ImportSummary, error) {
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
	r := f.request("start-import-evacuation", "300").WithContext(ctx)
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { accountLifecycleHandlerWithArchiveClient(f.cfg, f.client)(w, r); close(done) }()
	<-entered
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("disconnected inline wait blocked")
	}
	if strings.TrimSpace(w.Body.String()) != "" {
		t.Fatal("disconnected caller got terminal object")
	}
	close(release)
	f.drained(t)
	if !f.imported.Load() {
		t.Fatal("caller cancelled detached import")
	}
}

func TestAccountImportStatusUsesDatabaseOnly(t *testing.T) {
	f := newImportJobFixture(t)
	reads := 0
	f.cfg.AccountImportStatus = func(context.Context, string, string) (ImportSummary, AccountImportState, error) {
		reads++
		return ImportSummary{}, AccountImportRunning, nil
	}
	b := importResult(t, f.serve(f.request("import-evacuation-status", "")))
	state := b["import_job"].(map[string]any)
	if reads != 1 || len(state) != 1 || state["state"] != "running" || f.begins.Load() != 0 || f.gets.Load() != 0 {
		t.Fatal("status used local registry or claimed ownership")
	}
}
