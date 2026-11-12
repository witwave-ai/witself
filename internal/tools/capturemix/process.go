package capturemix

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Process output never leaves this fixed-name classifier. This deliberately
// discards arbitrary errors, values, request bodies, and startup environment.
type observationWriter struct {
	mu                 sync.Mutex
	pending            string
	refused            map[string]bool
	hidden             map[string]bool
	registered         map[string]bool
	destructionRefused bool
	receipts           map[string]int
	startupCause       string
}

func (w *observationWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.receipts == nil {
		w.receipts = map[string]int{}
	}
	if w.hidden == nil {
		w.hidden = map[string]bool{}
	}
	w.pending += string(p)
	for {
		line, rest, ok := strings.Cut(w.pending, "\n")
		if !ok {
			break
		}
		w.pending = rest
		if cause := classifyStartupCause(line); cause != "" {
			w.startupCause = cause
		}
		for job, prefix := range jobLogPrefixes {
			if strings.HasPrefix(line, "witself-worker: "+prefix) {
				if !strings.Contains(line, "enabled") {
					w.receipts[job]++
				}
				if job == "account_purge" && strings.Contains(line, "sync evidence prevents destructive finalization or purge") {
					w.destructionRefused = true
				}
				if strings.Contains(line, "sync writer not admitted") {
					w.refused[job] = true
				}
				if strings.HasSuffix(line, " batch failed") || job == "transcript_storage_reconcile" && positiveFailureCount(line, "error=") {
					w.hidden[job] = true
				}
				if strings.Contains(line, "enabled") {
					w.registered[job] = true
				}
			}
		}
	}
	if len(w.pending) > 8192 {
		w.pending = ""
	}
	return len(p), nil
}

func classifyStartupCause(line string) string {
	if !strings.HasPrefix(line, "witself-server: ") && !strings.HasPrefix(line, "witself-worker: ") {
		return ""
	}
	for _, cause := range []struct{ text, name string }{
		{"WITSELF_LOCAL_SPOOL_DIR", "spool_refused"},
		{"capture_proof_database_unidentified", "marker_refused"},
		{"capture_proof_requires_local_sync", "profile_refused"},
		{"local bootstrap account", "bootstrap_refused"},
		{"WITSELF_LOCAL_ACCOUNT_ID", "account_binding_refused"},
		{"migration wait exceeded", "migration_timeout"},
		{"database schema is newer", "core_schema_ahead"},
		{"sync_principals_missing", "principals_missing"},
		{"migrate:", "migration_failed"},
		{"apply migrations:", "migration_failed"},
		{"database:", "database_unavailable"},
		{"database readiness:", "database_unavailable"},
		{"configuration:", "configuration_refused"},
		{"listen tcp", "listener_failed"},
		{"bind: address already in use", "listener_failed"},
	} {
		if strings.Contains(line, cause.text) {
			return cause.name
		}
	}
	return ""
}

func (w *observationWriter) startupFailure() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.startupCause == "" {
		return "unclassified"
	}
	return w.startupCause
}
func (w *observationWriter) hasRefusal(name string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.refused[name]
}

var jobLogPrefixes = map[string]string{
	"avatar_style_rollout": "avatar style rollout", "message_rate_bucket_cleanup": "message rate bucket cleanup",
	"agent_email_rate_bucket_cleanup": "agent-email rate bucket cleanup", "agent_email_outbound": "outbound agent email",
	"transcript_retention": "transcript retention", "message_retention": "message retention",
	"agent_email_retention": "agent-email retention", "account_purge": "account purge",
	"support_ticket_age_out": "support ticket age-out", "transcript_storage_reconcile": "transcript storage reconcile",
}

type processHandle interface {
	Signal(os.Signal) error
	Kill() error
}

type childProcess struct {
	handle               processHandle
	resume               func() error
	shutdownWait         time.Duration
	cmd                  *exec.Cmd
	done                 chan struct{}
	waitErr              error
	logs                 *observationWriter
	api, health, metrics string
	directory            string
	kind, version        string
	paused               bool
	application          string
	metricBaseline       map[string]float64
	receiptBaseline      map[string]int
}

func (p *childProcess) alive() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}
func (p *childProcess) stop() error {
	if !p.alive() {
		return errors.New("capturemix process cleanup: reason=unexpected_exit")
	}
	wait := p.shutdownWait
	if wait <= 0 {
		wait = 10 * time.Second
	}
	failureReason := ""
	if p.paused {
		if p.resume == nil || p.resume() != nil {
			failureReason = "resume_failed"
		} else {
			p.paused = false
		}
	}
	if failureReason == "" && p.handle.Signal(os.Interrupt) != nil {
		failureReason = "signal_failed"
	}
	if failureReason == "" {
		timer := time.NewTimer(wait)
		select {
		case <-p.done:
			timer.Stop()
			if p.waitErr != nil {
				return errors.New("capturemix process cleanup: reason=nonzero_exit")
			}
			return nil
		case <-timer.C:
			failureReason = "graceful_timeout"
		}
	}
	// A failed graceful shutdown never permits database teardown with a live
	// child: kill and join even when resume or the first signal failed.
	killErr := p.handle.Kill()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-p.done:
		if killErr != nil {
			return errors.New("capturemix process cleanup: reason=" + failureReason + " outcome=kill_failed")
		}
		return errors.New("capturemix process cleanup: reason=" + failureReason + " outcome=killed")
	case <-timer.C:
		return errors.New("capturemix process cleanup: reason=" + failureReason + " outcome=join_timed_out")
	}
}

func (f *fixture) start(ctx context.Context) error {
	var err error
	f.stub, err = newStubServer()
	if err != nil {
		return err
	}
	if f.cluster.disposable || f.serverVersion == "old" {
		if err := f.startServer(ctx); err != nil {
			return err
		}
	}
	if f.serverVersion == "old" {
		f.oldServer = f.server
	} else {
		f.oldServer, err = f.startProcess(ctx, "server", "old")
		if err != nil {
			return err
		}
	}
	if f.cluster.disposable || f.workerVersion == "old" {
		if err := f.startWorker(ctx); err != nil {
			return err
		}
	}
	return nil
}
func (f *fixture) startServer(ctx context.Context) error {
	p, err := f.startProcess(ctx, "server", f.serverVersion)
	if err == nil {
		f.server = p
	}
	return err
}
func (f *fixture) startWorker(ctx context.Context) error {
	p, err := f.startProcess(ctx, "worker", f.workerVersion)
	if err == nil {
		f.worker = p
	}
	return err
}
func (f *fixture) startProcess(ctx context.Context, kind, version string) (*childProcess, error) {
	for attempt := 1; ; attempt++ {
		p, err := f.startProcessAttempt(ctx, kind, version)
		if err == nil {
			return p, nil
		}
		// Only readiness failures return a child. A closed done channel means
		// cmd.Wait has joined, including its output-copy goroutines.
		if p == nil || attempt == 3 || p.logs.startupFailure() != "listener_failed" || p.alive() || ctx.Err() != nil {
			return nil, err
		}
		if os.RemoveAll(p.directory) != nil {
			return nil, err
		}
		f.processes = f.processes[:len(f.processes)-1]
		f.options.startRetries = append(f.options.startRetries, "capturemix process start retried: kind="+kind+" version="+version+" attempt="+strconv.Itoa(attempt)+" cause=listener_failed")
	}
}

// processCommand substitutes only the child executable in hermetic tests.
// A nil hook preserves the production command and private environment.
var processCommand func(context.Context, *childProcess) *exec.Cmd

func (f *fixture) startProcessAttempt(ctx context.Context, kind, version string) (*childProcess, error) {
	directory, err := newProcessDirectory()
	if err != nil {
		return nil, errors.New("capturemix process directory failed")
	}
	p := &childProcess{directory: directory, kind: kind, version: version, done: make(chan struct{}), logs: &observationWriter{refused: map[string]bool{}, registered: map[string]bool{}}}
	for _, address := range []*string{&p.api, &p.health, &p.metrics} {
		*address, err = loopbackAddress()
		if err != nil {
			_ = os.RemoveAll(directory)
			return nil, err
		}
	}
	binDir := f.options.CurrentBinDir
	if version == "old" {
		binDir = f.options.PredecessorBinDir
	}
	role := "witself_sync_" + kind
	if version == "old" {
		role = "witself_sync_legacy"
	}
	processURL, _ := url.Parse(f.roleDSN(role))
	p.application = "capturemix_" + kind + "_" + f.name
	query := processURL.Query()
	query.Set("application_name", p.application)
	processURL.RawQuery = query.Encode()
	privateEnv := []string{
		"HOME=" + directory, "WITSELF_HOME=" + directory, "DSH_HOME=" + directory, "TMPDIR=" + filepath.Join(directory, "tmp"),
		"WITSELF_DATABASE_URL=" + processURL.String(), "WITSELF_API_ADDR=" + p.api,
		"WITSELF_HEALTH_ADDR=" + p.health, "WITSELF_METRICS_ADDR=" + p.metrics,
	}
	local := kind == "server" || version == "new"
	if local {
		privateEnv = append(privateEnv, "WITSELF_RUNTIME=local", "WITSELF_LOCAL_SYNC_SCHEMA=1", "WITSELF_LOCAL_SPOOL_DIR="+filepath.Join(directory, "spool"), "WITSELF_LOCAL_ACCOUNT_ID=mix_active")
		if version == "new" && f.cluster.disposable {
			privateEnv = append(privateEnv, "WITSELF_LOCAL_CAPTURE_PROOF=1", "WITSELF_LOCAL_CAPTURE_PROOF_TOKEN="+f.marker)
			if kind == "worker" {
				privateEnv = append(privateEnv, "WITSELF_LOCAL_CAPTURE_PROOF_STUB_EMAIL="+f.stub.plain.URL+"/v1/dispatch")
			}
		} else if kind == "worker" {
			privateEnv = append(privateEnv, "WITSELF_LOCAL_JOBS=avatar_style_rollout,message_rate_bucket_cleanup,agent_email_rate_bucket_cleanup,transcript_storage_reconcile")
		}
	}
	if kind == "worker" {
		for _, prefix := range proofWorkerPrefixes {
			privateEnv = append(privateEnv, "WITSELF_"+prefix+"_ENABLED=true", "WITSELF_"+prefix+"_INTERVAL="+proofWorkerInterval(prefix).String())
		}
		for _, prefix := range []string{"TRANSCRIPT_RETENTION", "MESSAGE_RETENTION", "AGENT_EMAIL_RETENTION", "ACCOUNT_PURGE"} {
			privateEnv = append(privateEnv, "WITSELF_"+prefix+"_MODE=enforce")
		}
		if version == "old" {
			_, key, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				_ = os.RemoveAll(directory)
				return nil, errors.New("capturemix stub signing key failed")
			}
			certPath := filepath.Join(directory, "stub-ca.pem")
			if os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.stub.tls.Certificate().Raw}), 0600) != nil {
				_ = os.RemoveAll(directory)
				return nil, errors.New("capturemix stub certificate failed")
			}
			privateEnv = append(privateEnv, "SSL_CERT_FILE="+certPath, "SSL_CERT_DIR="+directory,
				"WITSELF_AGENT_EMAIL_OUTBOUND_DISPATCH_ENDPOINT="+f.stub.tls.URL+"/v1/dispatch",
				"WITSELF_AGENT_EMAIL_OUTBOUND_DISPATCH_AUDIENCE=capturemix",
				"WITSELF_AGENT_EMAIL_OUTBOUND_DISPATCH_KEY_ID=capturemix",
				"WITSELF_AGENT_EMAIL_OUTBOUND_DISPATCH_PRIVATE_KEY="+base64.StdEncoding.EncodeToString(key))
		}
	}
	p.cmd = exec.CommandContext(ctx, filepath.Join(binDir, "witself-"+kind), "serve")
	if processCommand != nil {
		p.cmd = processCommand(ctx, p)
	}
	p.cmd.Env = privateEnv
	p.cmd.Dir = directory
	p.cmd.Stdout = p.logs
	p.cmd.Stderr = p.logs
	if p.cmd.Start() != nil {
		_ = os.RemoveAll(directory)
		return nil, errors.New("capturemix process start failed")
	}
	p.handle = p.cmd.Process
	p.resume = func() error { return resumeChild(p.cmd.Process) }
	f.processes = append(f.processes, p)
	go func() { p.waitErr = p.cmd.Wait(); close(p.done) }()
	err = poll(ctx, time.Since(f.started), func(ctx context.Context) (bool, error) {
		if !p.alive() {
			return false, errors.New("capturemix process exited before readiness")
		}
		status, _, err := httpRequest(ctx, "GET", "http://"+p.health+"/readyz", "", "")
		return err == nil && status == http.StatusOK, nil
	})
	if err != nil {
		return p, errors.New("capturemix process readiness failed: kind=" + kind + " version=" + version + " cause=" + p.logs.startupFailure())
	}
	if kind == "worker" {
		_, metrics, metricsErr := httpRequest(ctx, "GET", "http://"+p.metrics+"/metrics", "", "")
		if metricsErr != nil {
			return nil, errors.New("capturemix initial metrics unavailable")
		}
		p.metricBaseline = map[string]float64{}
		for job := range jobLogPrefixes {
			p.metricBaseline[job] = metricCount(metrics, `witself_worker_job_failures_total{job="`+job+`"} `)
		}
		if err := poll(ctx, time.Since(f.started), func(ctx context.Context) (bool, error) {
			if pauseChild(p.cmd.Process) != nil {
				return false, errors.New("capturemix worker pause failed")
			}
			p.paused = true
			var drained bool
			err := f.db.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND application_name=$1 AND (state='active' OR xact_start IS NOT NULL))`, p.application).Scan(&drained)
			if err == nil && drained {
				return true, nil
			}
			if resumeChild(p.cmd.Process) != nil {
				return false, errors.New("capturemix worker resume failed")
			}
			p.paused = false
			return false, err
		}); err != nil {
			return nil, errors.New("capturemix worker preseed drain failed")
		}
		p.logs.mu.Lock()
		p.receiptBaseline = map[string]int{}
		for job, count := range p.logs.receipts {
			p.receiptBaseline[job] = count
		}
		// Only post-baseline observations may qualify the seeded attempts.
		p.logs.refused = map[string]bool{}
		p.logs.hidden = map[string]bool{}
		p.logs.destructionRefused = false
		p.logs.mu.Unlock()
	}

	return p, nil
}

var proofWorkerPrefixes = []string{
	"AVATAR_STYLE_ROLLOUT", "MESSAGE_RATE_BUCKET_CLEANUP", "AGENT_EMAIL_RATE_BUCKET_CLEANUP",
	"AGENT_EMAIL_OUTBOUND", "TRANSCRIPT_RETENTION", "MESSAGE_RETENTION", "AGENT_EMAIL_RETENTION",
	"ACCOUNT_PURGE", "SUPPORT_TICKET_AGE_OUT", "TRANSCRIPT_STORAGE_RECONCILE",
}

// Stagger account-locking jobs to reduce repeated SKIP LOCKED collisions.
// Initial callbacks establish their own ticker phases, so distinct intervals
// alone do not guarantee a qualifying attempt on every tick.
func proofWorkerInterval(prefix string) time.Duration {
	switch prefix {
	case "AVATAR_STYLE_ROLLOUT":
		return 61 * time.Second
	case "MESSAGE_RETENTION":
		return 62 * time.Second
	case "AGENT_EMAIL_RETENTION":
		return 63 * time.Second
	case "ACCOUNT_PURGE":
		return 65 * time.Second
	case "SUPPORT_TICKET_AGE_OUT":
		return 67 * time.Second
	default:
		return time.Minute
	}
}

const workerOutcomeBudgetCap = 5 * time.Minute

func proofWorkerOutcomeBudget() time.Duration {
	var longest time.Duration
	for _, prefix := range proofWorkerPrefixes {
		longest = max(longest, proofWorkerInterval(prefix))
	}
	const requiredAttempts = 2
	const registrationAndSchedulingIntervals = 2
	return min((requiredAttempts+registrationAndSchedulingIntervals)*longest, workerOutcomeBudgetCap)
}

func newProcessDirectory() (directory string, resultErr error) {
	root, err := os.MkdirTemp("", "capturemix-process-")
	if err != nil {
		return "", errors.New("capturemix process directory failed")
	}
	defer func() {
		if resultErr != nil && os.RemoveAll(root) != nil {
			resultErr = errors.Join(resultErr, errors.New("capturemix process directory cleanup failed"))
		}
	}()
	directory, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", errors.New("capturemix process directory canonicalization failed")
	}
	for _, name := range []string{"tmp", "spool"} {
		if os.Mkdir(filepath.Join(directory, name), 0700) != nil {
			return "", errors.New("capturemix process directory setup failed")
		}
	}
	return directory, nil
}

func (f *fixture) stopProcesses() error {
	var failure error
	for i := len(f.processes) - 1; i >= 0; i-- {
		p := f.processes[i]
		if err := p.stop(); err != nil {
			failure = errors.Join(failure, errors.New("capturemix process cleanup failed: kind="+p.kind+" version="+p.version), err)
		}
		if os.RemoveAll(p.directory) != nil {
			failure = errors.Join(failure, errors.New("capturemix process directory cleanup failed"))
		}
	}
	f.processes = nil
	return failure
}

const requestedPreferences = `{"schema":"witself.dashboard-prefs.v1","theme":"console"}`

type preferenceSnapshot struct {
	row                                     string
	bundles, operations, touches, finalized int64
}

func (f *fixture) preferenceSnapshot(ctx context.Context, account string) (preferenceSnapshot, error) {
	var snapshot preferenceSnapshot
	err := f.db.QueryRow(ctx, `SELECT row_to_json(p)::text,
 (SELECT count(*) FROM sync_bundles WHERE account_id=$1),
 (SELECT count(*) FROM sync_operations WHERE account_id=$1),
 (SELECT count(*) FROM sync_tx_touches WHERE account_id=$1),
 (SELECT coalesce(max(finalized),0) FROM sync_bundles WHERE account_id=$1)
 FROM agent_dashboard_preferences p WHERE account_id=$1 AND realm_id=$2 AND agent_id=$3`,
		account, account+"_realm", account+"_agent").Scan(&snapshot.row, &snapshot.bundles, &snapshot.operations, &snapshot.touches, &snapshot.finalized)
	if err != nil {
		return snapshot, errors.New("capturemix preference snapshot failed")
	}
	return snapshot, nil
}

func preferenceCaptureUnchanged(before, after preferenceSnapshot) bool {
	return before.bundles == after.bundles && before.operations == after.operations && before.touches == after.touches && before.finalized == after.finalized
}

func (f *fixture) preferenceDocumentMatches(ctx context.Context, account string) bool {
	var matches bool
	return f.db.QueryRow(ctx, `SELECT prefs=$4::jsonb FROM agent_dashboard_preferences WHERE account_id=$1 AND realm_id=$2 AND agent_id=$3`, account, account+"_realm", account+"_agent", requestedPreferences).Scan(&matches) == nil && matches
}

func (f *fixture) assertServer(ctx context.Context) error {
	before, err := f.preferenceSnapshot(ctx, "mix_active")
	if err != nil {
		return err
	}
	inactiveBefore, err := f.preferenceSnapshot(ctx, "mix_inactive")
	if err != nil {
		return err
	}
	request := `{"prefs":` + requestedPreferences + `}`
	status, body, err := httpRequest(ctx, "PUT", "http://"+f.server.api+"/v1/self/dashboard-preferences", f.activeToken, request)
	if err != nil {
		return errors.New("capturemix server request failed")
	}
	after, err := f.preferenceSnapshot(ctx, "mix_active")
	if err != nil {
		return err
	}
	if f.serverVersion == "old" || !f.cluster.disposable {
		if status != http.StatusInternalServerError || body != "{\"error\":\"could not write dashboard preferences\",\"schema_version\":\"witself.v0\"}\n" {
			return errors.New("capturemix legacy server refusal mismatch")
		}
		if before != after {
			return errors.New("capturemix legacy server changed domain or capture evidence")
		}
	} else {
		if status != http.StatusOK || !f.preferenceDocumentMatches(ctx, "mix_active") {
			return errors.New("capturemix current server admission failed")
		}
		if after.bundles != before.bundles+1 || after.operations != before.operations+1 || after.touches != before.touches+1 || after.finalized != before.finalized+1 {
			return errors.New("capturemix current server capture delta mismatch")
		}
		var matching int64
		if f.db.QueryRow(ctx, `SELECT count(*) FROM sync_bundles b
 JOIN sync_tx_touches t ON t.account_id=b.account_id AND t.tx_id=b.tx_id
 JOIN sync_bundle_operations m ON m.account_id=b.account_id AND m.bundle_id=b.bundle_id AND m.operation_id=t.operation_id
 JOIN sync_operations o ON o.account_id=m.account_id AND o.operation_id=m.operation_id
 WHERE b.account_id='mix_active' AND b.finalized>$1 AND b.sealed AND b.origin_class='original'
 AND b.semantic_effects=jsonb_build_array($2::jsonb) AND t.semantic_effect=$2::jsonb
 AND t.dependencies->>'agent_id'='mix_active_agent' AND t.dependencies->>'realm_id'='mix_active_realm'
 AND t.table_name='agent_dashboard_preferences' AND t.atomic_group='preferences'
 AND m.atomic_group='preferences' AND m.member_order=t.touch_id AND o.operation_kind='preferences.put'`, before.finalized, requestedPreferences).Scan(&matching) != nil || matching != 1 {
			return errors.New("capturemix current server request-bound bundle missing")
		}
	}
	inactiveUntouched, err := f.preferenceSnapshot(ctx, "mix_inactive")
	if err != nil || inactiveUntouched != inactiveBefore {
		return errors.New("capturemix active request changed inactive scope")
	}
	status, _, err = httpRequest(ctx, "PUT", "http://"+f.server.api+"/v1/self/dashboard-preferences", f.inactiveToken, request)
	if err != nil || status != http.StatusOK || !f.preferenceDocumentMatches(ctx, "mix_inactive") {
		return errors.New("capturemix inactive account writer changed")
	}
	inactiveAfter, err := f.preferenceSnapshot(ctx, "mix_inactive")
	if err != nil || !preferenceCaptureUnchanged(inactiveBefore, inactiveAfter) {
		return errors.New("capturemix inactive request changed capture evidence")
	}
	activeUntouched, err := f.preferenceSnapshot(ctx, "mix_active")
	if err != nil || activeUntouched != after {
		return errors.New("capturemix inactive request changed active scope")
	}
	return nil
}
func (f *fixture) assertWorker(ctx context.Context) error {
	expected := []string{"avatar_style_rollout", "message_rate_bucket_cleanup", "agent_email_rate_bucket_cleanup", "transcript_storage_reconcile"}
	if f.cluster.disposable || f.workerVersion == "old" {
		expected = append(expected, "agent_email_outbound", "transcript_retention", "message_retention", "agent_email_retention", "account_purge", "support_ticket_age_out")
	}
	retentionRescheduled, purgeRescheduled := false, false
	missing := []string{"worker:metrics_unavailable"}
	// Two largest configured intervals for the required attempts, plus two
	// largest intervals for registration, ticker phase and missed scheduling
	// opportunities; the helper caps this below the enclosing context.
	// Lock observations retain their separately proportional, bounded polls.
	if err := pollWithBudget(ctx, proofWorkerOutcomeBudget(), func(ctx context.Context) (bool, error) {
		if !f.worker.alive() {
			missing = append(missing, "worker:process_exited")
			return false, errors.New("capturemix worker exited during refusal proof")
		}
		status, body, err := httpRequest(ctx, "GET", "http://"+f.worker.metrics+"/metrics", "", "")
		if err != nil || status != http.StatusOK {
			// A deadline-canceled final request must not replace the last full
			// snapshot's useful, value-free missing-outcome diagnostics.
			return false, nil
		}
		missing = f.worker.missingWorkerOutcomes(body, expected)
		if !retentionRescheduled && f.worker.retentionRearmReady(body) {
			// These two jobs durably lease a lane before covered domain DML.
			// Re-arm excluded coordination rows after their first real refusals
			// so the next ordinary production tick attempts the same account.
			if err := f.scheduleWorkerCandidateLanes(ctx); err != nil {
				missing = append(missing, "retention_scheduler:rearm_failed")
				return false, err
			}
			retentionRescheduled = true
		}
		if !purgeRescheduled && f.worker.purgeRearmReady(body) {
			if err := f.schedulePurgeCandidate(ctx); err != nil {
				missing = append(missing, "purge_scheduler:rearm_failed")
				return false, err
			}
			purgeRescheduled = true
		}
		completed, failureClass := f.inactiveWorkerCompanionCompleted(ctx)
		if failureClass != "" {
			missing = append(missing, "inactive_companion:"+failureClass)
			return false, errors.New("capturemix inactive worker observation failed")
		}
		if !completed {
			missing = append(missing, "inactive_companion:domain_change_missing")
		}
		return len(missing) == 0, nil
	}); err != nil {
		// Every label comes from reviewed fixed names, never process values,
		// metric counts, database errors, or request bodies.
		return errors.New("capturemix worker outcome incomplete: missing=" + strings.Join(missing, ","))
	}
	if err := f.assertWorkerCandidateRows(ctx); err != nil {
		return err
	}
	if !f.worker.alive() {
		return errors.New("capturemix worker exited after refusal")
	}
	evidence, err := f.worker.verifiedJobEvidence(expected, true)
	if err != nil {
		return err
	}
	f.jobEvidence = evidence
	return nil
}

func (p *childProcess) missingWorkerOutcomes(metrics string, expected []string) []string {
	p.logs.mu.Lock()
	defer p.logs.mu.Unlock()
	var missing []string
	for _, job := range expected {
		if !strings.Contains(metrics, `witself_worker_job_running{job="`+job+`"} 1`) {
			missing = append(missing, job+":registration_missing")
		}
		if metricCount(metrics, `witself_worker_job_failures_total{job="`+job+`"} `) < p.metricBaseline[job]+2 {
			missing = append(missing, job+":failure_counter")
		}
		if p.logs.receipts[job]-p.receiptBaseline[job] < 2 {
			missing = append(missing, job+":new_receipts")
		}
		if p.logs.receipts[job]-p.receiptBaseline[job] >= 2 && metricCount(metrics, `witself_worker_job_failures_total{job="`+job+`"} `) >= p.metricBaseline[job]+2 {
			if _, err := observedJobEvidence(job, p.logs.refused[job], p.logs.hidden[job], job == "account_purge" && p.logs.destructionRefused, true); err != nil {
				missing = append(missing, job+":evidence_class")
			}
		}
	}
	registered := 0
	for job := range jobLogPrefixes {
		if strings.Contains(metrics, `witself_worker_job_running{job="`+job+`"} 1`) {
			registered++
		}
	}
	if registered != len(expected) {
		missing = append(missing, "worker:registration_set_mismatch")
	}
	return missing
}

func (p *childProcess) retentionRearmReady(metrics string) bool {
	p.logs.mu.Lock()
	defer p.logs.mu.Unlock()
	for _, job := range []string{"message_retention", "agent_email_retention"} {
		if !p.logs.refused[job] || p.logs.receipts[job] <= p.receiptBaseline[job] ||
			metricCount(metrics, `witself_worker_job_failures_total{job="`+job+`"} `) <= p.metricBaseline[job] {
			return false
		}
	}
	return true
}

// Purge commits its excluded scheduler lease before its destruction refusal.
// Re-arm only after a fresh callback from the already-running production loop.
func (p *childProcess) purgeRearmReady(metrics string) bool {
	p.logs.mu.Lock()
	defer p.logs.mu.Unlock()
	const job = "account_purge"
	return p.logs.destructionRefused && p.logs.receipts[job] > p.receiptBaseline[job] &&
		metricCount(metrics, `witself_worker_job_failures_total{job="account_purge"} `) > p.metricBaseline[job]
}

func metricCount(body, prefix string) float64 {
	for _, line := range strings.Split(body, "\n") {
		if value, ok := strings.CutPrefix(line, prefix); ok {
			count, err := strconv.ParseFloat(value, 64)
			if err == nil {
				return count
			}
		}
	}
	return 0
}

func positiveFailureCount(line, prefix string) bool {
	for _, field := range strings.Fields(line) {
		if value, ok := strings.CutPrefix(field, prefix); ok {
			count, err := strconv.ParseUint(value, 10, 64)
			return err == nil && count > 0
		}
	}
	return false
}

func loopbackAddress() (string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", errors.New("capturemix loopback allocation failed")
	}
	address := listener.Addr().String()
	if listener.Close() != nil {
		return "", errors.New("capturemix loopback allocation failed")
	}
	return address, nil
}

const maximumResponseBytes = 1 << 20

func httpRequest(ctx context.Context, method, url, token, body string) (int, string, error) {
	request, err := http.NewRequestWithContext(ctx, method, url, strings.NewReader(body))
	if err != nil {
		return 0, "", errors.New("capturemix request construction failed")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	request.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	response, err := client.Do(request)
	if err != nil {
		return 0, "", errors.New("capturemix loopback request failed")
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maximumResponseBytes+1))
	if err != nil {
		return 0, "", errors.New("capturemix response read failed")
	}
	if len(raw) > maximumResponseBytes {
		return 0, "", errors.New("capturemix response exceeded limit")
	}
	return response.StatusCode, string(raw), nil
}

type stubServer struct {
	plain, tls *httptest.Server
	mu         sync.Mutex
	requests   []string
}

func newStubServer() (*stubServer, error) {
	s := &stubServer{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.requests = append(s.requests, "dispatch_request")
		s.mu.Unlock()
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 1<<20))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":"capturemix_stub_refusal"}`)
	})
	s.plain = httptest.NewServer(handler)
	s.tls = httptest.NewTLSServer(handler)
	return s, nil
}
func (s *stubServer) close() { s.plain.Close(); s.tls.Close() }
