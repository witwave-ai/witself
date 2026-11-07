package capturemix

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

type fakeRow struct {
	steps    []string
	fail     string
	stopFail bool
}

func (f *fakeRow) step(name string) error {
	f.steps = append(f.steps, name)
	if f.fail == name {
		return errors.New("capturemix injected stage failure")
	}
	return nil
}
func (f *fakeRow) start(context.Context) error              { return f.step("start") }
func (f *fakeRow) holdLegacy(context.Context) error         { return f.step("hold") }
func (f *fakeRow) beginInstall(context.Context) error       { return f.step("install") }
func (f *fakeRow) observeInstallWait(context.Context) error { return f.step("observe") }
func (f *fakeRow) completeLegacy(_ context.Context, completion string) error {
	return f.step(completion)
}
func (f *fakeRow) finishInstall(context.Context) error   { return f.step("finish") }
func (f *fakeRow) activate(context.Context) error        { return f.step("activate") }
func (f *fakeRow) assertWriters(context.Context) error   { return f.step("writers") }
func (f *fakeRow) assertDowngrade(context.Context) error { return f.step("downgrade") }
func (f *fakeRow) stopProcesses() error {
	f.steps = append(f.steps, "stop")
	if f.stopFail {
		return errors.New("capturemix injected stop failure")
	}
	return nil
}

func TestOrchestrationObservedDrainAndCleanup(t *testing.T) {
	for _, completion := range []string{"commit", "rollback"} {
		t.Run(completion, func(t *testing.T) {
			backend := &fakeRow{}
			checks, err := executeRow(context.Background(), backend, completion)
			want := []string{"start", "hold", "install", "observe", completion, "finish", "activate", "writers", "downgrade", "stop"}
			if err != nil || !reflect.DeepEqual(backend.steps, want) || len(checks) != 9 {
				t.Fatal("capturemix ordering lost observed drain or cleanup")
			}
		})
	}
}
func TestOrchestrationStopsOnEveryStageFailure(t *testing.T) {
	stages := []string{"start", "hold", "install", "observe", "commit", "finish", "activate", "writers", "downgrade"}
	for i, stage := range stages {
		t.Run(stage, func(t *testing.T) {
			backend := &fakeRow{fail: stage}
			checks, err := executeRow(context.Background(), backend, "commit")
			want := append(append([]string(nil), stages[:i+1]...), "stop")
			if err == nil || !reflect.DeepEqual(backend.steps, want) || len(checks) != i {
				t.Fatal("capturemix failed stage advanced or omitted cleanup")
			}
		})
	}
}
func TestOrchestrationCleanupFailureIsFailure(t *testing.T) {
	backend := &fakeRow{stopFail: true}
	if _, err := executeRow(context.Background(), backend, "commit"); err == nil || !strings.Contains(err.Error(), "capturemix process cleanup failed") {
		t.Fatal("capturemix cleanup error was discarded")
	}
}

func TestOrchestrationPreservesStageAndCleanupFailures(t *testing.T) {
	backend := &fakeRow{fail: "start", stopFail: true}
	_, err := executeRow(context.Background(), backend, "commit")
	if err == nil || !strings.Contains(err.Error(), "capturemix stage failed: processes_ready") ||
		!strings.Contains(err.Error(), "capturemix injected stage failure") ||
		!strings.Contains(err.Error(), "capturemix injected stop failure") {
		t.Fatal("capturemix teardown concealed the primary stage failure")
	}
}

func TestStartupClassifierEmitsOnlyFixedCauses(t *testing.T) {
	for _, test := range []struct{ line, want string }{
		{"witself-server: WITSELF_LOCAL_SPOOL_DIR arbitrary-private-value", "spool_refused"},
		{"witself-worker: migrate: arbitrary-private-value", "migration_failed"},
		{"witself-server: local bootstrap account must exist and be active", "bootstrap_refused"},
		{"witself-server: arbitrary-private-value", ""},
	} {
		if classifyStartupCause(test.line) != test.want {
			t.Fatal("capturemix startup classifier exposed an unreviewed cause")
		}
	}
}

func TestInstallerEarlyCompletionCannotCountAsObservedDrain(t *testing.T) {
	for _, failure := range []error{nil, errors.New("apply sync migration failed"), errors.New("arbitrary-private-value")} {
		f := &fixture{install: make(chan error, 1)}
		if f.installerBeforeWait() != nil || f.installConsumed {
			t.Fatal("capturemix pending installer was marked completed")
		}
		f.install <- failure
		err := f.installerBeforeWait()
		if err == nil || !f.installConsumed || strings.Contains(err.Error(), "arbitrary-private-value") {
			t.Fatal("capturemix completed installer bypassed drain observation or exposed its error")
		}
	}
}

func TestHTTPObservationRetainsLateMetricsAndRefusesOverflow(t *testing.T) {
	const wanted = `witself_worker_job_failures_total{job="avatar_style_rollout"} `
	for _, oversized := range []bool{false, true} {
		body := strings.Repeat("fixture_metric 0\n", 1024) + wanted + "2\n"
		if oversized {
			body = strings.Repeat("x", maximumResponseBytes+1)
		}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(body))
		}))
		status, observed, err := httpRequest(context.Background(), "GET", server.URL, "", "")
		server.Close()
		if oversized {
			if err == nil || err.Error() != "capturemix response exceeded limit" || observed != "" {
				t.Fatal("capturemix oversized response was silently truncated")
			}
		} else if err != nil || status != http.StatusOK || metricCount(observed, wanted) != 2 {
			t.Fatal("capturemix late worker metric was truncated")
		}
	}
}

func TestWorkerSnapshotReportsEveryMissingOutcome(t *testing.T) {
	p := &childProcess{
		logs:            &observationWriter{refused: map[string]bool{"avatar_style_rollout": true}, receipts: map[string]int{"avatar_style_rollout": 3, "support_ticket_age_out": 1}},
		metricBaseline:  map[string]float64{"avatar_style_rollout": 1},
		receiptBaseline: map[string]int{"avatar_style_rollout": 1},
	}
	metrics := "witself_worker_job_running{job=\"avatar_style_rollout\"} 1\nwitself_worker_job_running{job=\"support_ticket_age_out\"} 1\nwitself_worker_job_failures_total{job=\"avatar_style_rollout\"} 3\n"
	missing := p.missingWorkerOutcomes(metrics, []string{"avatar_style_rollout", "support_ticket_age_out"})
	if !reflect.DeepEqual(missing, []string{"support_ticket_age_out:failure_counter", "support_ticket_age_out:new_receipts"}) {
		t.Fatal("capturemix worker snapshot omitted or misattributed missing outcomes")
	}
}

func TestPurgeRearmRequiresNewDestructionOutcome(t *testing.T) {
	p := &childProcess{logs: &observationWriter{destructionRefused: true, receipts: map[string]int{"account_purge": 1}}, metricBaseline: map[string]float64{"account_purge": 1}, receiptBaseline: map[string]int{"account_purge": 1}}
	metrics := "witself_worker_job_failures_total{job=\"account_purge\"} 2\n"
	if p.purgeRearmReady(metrics) {
		t.Fatal("capturemix purge rearm accepted a preactivation receipt")
	}
	p.logs.receipts["account_purge"]++
	p.logs.destructionRefused = false
	if p.purgeRearmReady(metrics) {
		t.Fatal("capturemix purge rearm accepted an unrelated failure")
	}
	p.logs.destructionRefused = true
	if !p.purgeRearmReady(metrics) {
		t.Fatal("capturemix purge rearm rejected a new destruction outcome")
	}
}

func TestPurgeCandidateIsDueBeforeWorkerResume(t *testing.T) {
	raw, err := os.ReadFile("postgres.go")
	if err != nil {
		t.Fatal("capturemix activation source unavailable")
	}
	source := strings.ReplaceAll(string(raw), "\r\n", "\n")
	file, err := parser.ParseFile(token.NewFileSet(), "postgres.go", source, 0)
	if err != nil {
		t.Fatal("capturemix activation source invalid")
	}
	var calls []string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "activate" {
			continue
		}
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fun := call.Fun.(type) {
			case *ast.SelectorExpr:
				switch fun.Sel.Name {
				case "assertWorkerCandidateRows", "Activate", "scheduleWorkerCandidateLanes", "schedulePurgeCandidate":
					calls = append(calls, fun.Sel.Name)
				}
			case *ast.Ident:
				if fun.Name == "resumeChild" {
					calls = append(calls, fun.Name)
				}
			}
			return true
		})
	}
	want := []string{"assertWorkerCandidateRows", "Activate", "scheduleWorkerCandidateLanes", "schedulePurgeCandidate", "resumeChild"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatal("capturemix purge candidate is not scheduled after activation and before worker resume")
	}
}

func TestPreferenceRefusalRejectsMetadataAndEvidenceChanges(t *testing.T) {
	before := preferenceSnapshot{row: `{"prefs":{"theme":"amber"},"updated_at":"before"}`, bundles: 1, operations: 1, touches: 1, finalized: 1}
	after := before
	after.row = `{"prefs":{"theme":"amber"},"updated_at":"after"}`
	if before == after || !preferenceCaptureUnchanged(before, after) {
		t.Fatal("capturemix preference snapshot lost metadata-only mutation")
	}
	for _, mutate := range []func(*preferenceSnapshot){
		func(s *preferenceSnapshot) { s.bundles++ }, func(s *preferenceSnapshot) { s.operations++ },
		func(s *preferenceSnapshot) { s.touches++ }, func(s *preferenceSnapshot) { s.finalized++ },
	} {
		after = before
		mutate(&after)
		if preferenceCaptureUnchanged(before, after) {
			t.Fatal("capturemix preference snapshot lost capture evidence mutation")
		}
	}
}

func TestRequestBundleMembershipMatchesProductionTouchOrder(t *testing.T) {
	// Production sealing copies touch_id directly into member_order. The
	// executable proof must join that identity instead of assuming zero-based
	// membership, because trigger event IDs begin at one.
	for _, contract := range []struct{ path, text string }{
		{"process.go", "m.member_order=t.touch_id"},
		{"../../store/sync_migrations/0003_sync_principals.sql", "SELECT account_id,target_bundle_id,touch_id,operation_id,atomic_group FROM sync_tx_touches"},
	} {
		raw, err := os.ReadFile(contract.path)
		if err != nil || !strings.Contains(string(raw), contract.text) {
			t.Fatal("capturemix bundle membership diverged from production touch order")
		}
	}
}

func TestProofAccountLockingJobIntervalsAreDistinctAndBounded(t *testing.T) {
	seen := map[time.Duration]bool{}
	for _, job := range []string{"TRANSCRIPT_RETENTION", "AVATAR_STYLE_ROLLOUT", "MESSAGE_RETENTION", "AGENT_EMAIL_RETENTION", "ACCOUNT_PURGE", "SUPPORT_TICKET_AGE_OUT"} {
		interval := proofWorkerInterval(job)
		if interval < time.Minute || interval*2 > proofWorkerOutcomeBudget() || seen[interval] {
			t.Fatal("capturemix job intervals collide or cannot yield two bounded production attempts")
		}
		seen[interval] = true
	}
}
func TestPredecessorPinRefusal(t *testing.T) {
	for _, test := range []struct {
		name, argument, contents string
		want                     bool
	}{
		{"reviewed", syncPredecessorCommit, syncPredecessorCommit + "\n", true},
		{"argument", strings.Repeat("a", 40), syncPredecessorCommit, false},
		{"archive", syncPredecessorCommit, strings.Repeat("b", 40), false},
		{"abbreviated", syncPredecessorCommit, "2f1c1943", false},
		{"missing", syncPredecessorCommit, "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "PREDECESSOR_COMMIT")
			if test.contents != "" && os.WriteFile(file, []byte(test.contents), 0600) != nil {
				t.Fatal("capturemix pin fixture write failed")
			}
			err := verifyPredecessor(context.Background(), Options{PredecessorCommit: test.argument, PredecessorCommitFile: file})
			if (err == nil) != test.want || (err != nil && err.Error() != "predecessor_commit_mismatch") {
				t.Fatal("capturemix predecessor refusal contract changed")
			}
		})
	}
}
func TestProcessObservationSeparatesRegistrationFromRefusal(t *testing.T) {
	w := &observationWriter{registered: map[string]bool{}, refused: map[string]bool{}}
	for _, part := range []string{"witself-worker: message rate bucket cleanup enabled (batch 1)\n", "witself-worker: message rate bucket cleanup: rate_bucket=sync writer ", "not admitted\n"} {
		if _, err := w.Write([]byte(part)); err != nil {
			t.Fatal("capturemix observation write failed")
		}
	}
	if !w.registered["message_rate_bucket_cleanup"] || !w.hasRefusal("message_rate_bucket_cleanup") || w.hasRefusal("agent_email_outbound") {
		t.Fatal("capturemix observation attribution changed")
	}
	w2 := &observationWriter{registered: map[string]bool{}, refused: map[string]bool{}}
	_, _ = w2.Write([]byte("witself-worker: account purge enabled\n"))
	if w2.hasRefusal("account_purge") {
		t.Fatal("capturemix registration was treated as refusal")
	}
}
func TestDriverRefusesBeforeDatabaseWhenPinMismatches(t *testing.T) {
	_, err := Run(context.Background(), Options{PredecessorCommit: "unreviewed"})
	if err == nil || err.Error() != "predecessor_commit_mismatch" {
		t.Fatal("capturemix pin refusal did not precede allocation")
	}
}

type fakeProcessHandle struct {
	steps           *[]string
	done            chan struct{}
	signalFail      bool
	signalCompletes bool
}

func (p *fakeProcessHandle) Signal(os.Signal) error {
	*p.steps = append(*p.steps, "signal")
	if p.signalFail {
		return errors.New("capturemix fake signal failure")
	}
	if p.signalCompletes {
		close(p.done)
	}
	return nil
}
func (p *fakeProcessHandle) Kill() error {
	*p.steps = append(*p.steps, "kill")
	close(p.done)
	return nil
}

func TestProcessCleanupFailuresKillAndJoin(t *testing.T) {
	for _, test := range []struct {
		name                           string
		paused, resumeFail, signalFail bool
		want                           []string
	}{
		{"resume", true, true, false, []string{"resume", "kill"}},
		{"signal", false, false, true, []string{"signal", "kill"}},
		{"timeout", false, false, false, []string{"signal", "kill"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			steps := []string{}
			done := make(chan struct{})
			p := &childProcess{handle: &fakeProcessHandle{steps: &steps, done: done, signalFail: test.signalFail}, done: done, paused: test.paused, shutdownWait: time.Millisecond}
			p.resume = func() error {
				steps = append(steps, "resume")
				if test.resumeFail {
					return errors.New("capturemix fake resume failure")
				}
				return nil
			}
			if err := p.stop(); err == nil || !reflect.DeepEqual(steps, test.want) || p.alive() {
				t.Fatal("capturemix failed process cleanup omitted kill or join")
			}
		})
	}
}
func TestProcessCleanShutdownJoinsWithoutKill(t *testing.T) {
	steps := []string{}
	done := make(chan struct{})
	p := &childProcess{handle: &fakeProcessHandle{steps: &steps, done: done, signalCompletes: true}, done: done, shutdownWait: time.Millisecond}
	if p.stop() != nil || p.alive() || !reflect.DeepEqual(steps, []string{"signal"}) {
		t.Fatal("capturemix graceful process cleanup ordering changed")
	}
}

// These tables independently pin every shared-instance port and lane.
var rehearsalPorts = []string{"5599", "5600", "5601"}
var rehearsalDatabases = []string{"/codex_a", "/codex_b", "/codex_c", "/codex_d"}

func TestFixtureDSNHasNoPublicFallback(t *testing.T) {
	for _, database := range rehearsalDatabases {
		for _, initial := range []string{"", "public", "existing,public"} {
			target := &url.URL{Scheme: "postgres", Host: "127.0.0.1:5599", Path: database}
			query := target.Query()
			query.Set("search_path", initial)
			query.Set("application_name", "fixture")
			target.RawQuery = query.Encode()
			isolateSchemaURL(target, "capturemix_fixture")
			if target.Query().Get("search_path") != "capturemix_fixture" || target.Query().Get("application_name") != "fixture" || target.Path != database {
				t.Fatal("capturemix schema isolation retained public fallback or changed the lane")
			}
		}
	}
}

func TestRehearsalAcceptsEverySharedLane(t *testing.T) {
	for _, port := range rehearsalPorts {
		for _, database := range rehearsalDatabases {
			t.Run(port+database, func(t *testing.T) {
				if !allowedClusterTarget(&url.URL{Host: "127.0.0.1:" + port, Path: database}, false) {
					t.Fatal("capturemix allowed rehearsal lane refused")
				}
			})
		}
	}
}

func TestRehearsalTargetSetsPinned(t *testing.T) {
	source, err := parser.ParseFile(token.NewFileSet(), "postgres.go", nil, 0)
	if err != nil {
		t.Fatal("capturemix target selector source unavailable")
	}
	var selector *ast.FuncDecl
	for _, declaration := range source.Decls {
		if function, ok := declaration.(*ast.FuncDecl); ok && function.Name.Name == "allowedClusterTarget" {
			selector = function
		}
	}
	if selector == nil {
		t.Fatal("capturemix target selector missing")
	}
	sets := map[string][]string{}
	switches := map[string]int{}
	ast.Inspect(selector, func(node ast.Node) bool {
		statement, ok := node.(*ast.SwitchStmt)
		if !ok {
			return true
		}
		tag := statement.Tag
		if call, ok := tag.(*ast.CallExpr); ok && len(call.Args) == 0 {
			tag = call.Fun
		}
		field, ok := tag.(*ast.SelectorExpr)
		if !ok {
			t.Fatal("capturemix target set selector changed")
		}
		receiver, ok := field.X.(*ast.Ident)
		if !ok || receiver.Name != "target" || (field.Sel.Name != "Port" && field.Sel.Name != "Path") {
			t.Fatal("capturemix target set selector changed")
		}
		switches[field.Sel.Name]++
		defaults := 0
		for _, clause := range statement.Body.List {
			branch := clause.(*ast.CaseClause)
			if branch.List == nil {
				defaults++
				if len(branch.Body) != 1 {
					t.Fatal("capturemix target set default no longer refuses")
				}
				result, ok := branch.Body[0].(*ast.ReturnStmt)
				if !ok || len(result.Results) != 1 {
					t.Fatal("capturemix target set default no longer refuses")
				}
				value, ok := result.Results[0].(*ast.Ident)
				if !ok || value.Name != "false" {
					t.Fatal("capturemix target set default no longer refuses")
				}
			}
			for _, expression := range branch.List {
				literal, ok := expression.(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					t.Fatal("capturemix target set is not a closed literal list")
				}
				value, err := strconv.Unquote(literal.Value)
				if err != nil {
					t.Fatal("capturemix target set literal invalid")
				}
				sets[field.Sel.Name] = append(sets[field.Sel.Name], value)
			}
		}
		if defaults != 1 {
			t.Fatal("capturemix target set default refusal missing")
		}
		return true
	})
	if switches["Port"] != 1 || switches["Path"] != 1 {
		t.Fatal("capturemix target set switches changed")
	}
	if !reflect.DeepEqual(sets["Port"], rehearsalPorts) {
		t.Fatal("capturemix rehearsal port set changed")
	}
	if !reflect.DeepEqual(sets["Path"], rehearsalDatabases) {
		t.Fatal("capturemix rehearsal database set changed")
	}
}

func TestRehearsalRefusesEveryOtherTargetBeforeConnection(t *testing.T) {
	t.Setenv("WITSELF_SYNC_HARNESS_DB", "")
	for _, test := range []struct{ name, host, database string }{
		{"port", "127.0.0.1:5432", "/codex_b"},
		{"default_port", "127.0.0.1", "/codex_b"},
		{"hostname", "localhost:5599", "/codex_b"},
		{"ipv6", "[::1]:5599", "/codex_b"},
		{"database", "127.0.0.1:5599", "/other"},
		{"postgres", "127.0.0.1:5599", "/postgres"},
		{"witself", "127.0.0.1:5599", "/witself"},
		{"unlisted_port", "127.0.0.1:5602", "/codex_b"},
		{"unlisted_lane", "127.0.0.1:5599", "/codex_e"},
	} {
		t.Run(test.name, func(t *testing.T) {
			target := &url.URL{Scheme: "postgres", Host: test.host, Path: test.database, RawQuery: "sslmode=disable"}
			if allowedClusterTarget(target, false) {
				t.Fatal("capturemix rehearsal accepted refused target")
			}
			cluster, err := openCluster(context.Background(), Options{OwnerDSN: target.String()})
			if cluster != nil || err == nil || err.Error() != "capturemix database target refused" {
				t.Fatal("capturemix rehearsal connected outside its assigned target")
			}
		})
	}
	if !allowedClusterTarget(&url.URL{Host: "127.0.0.1:5432", Path: "/witself"}, true) {
		t.Fatal("capturemix disposable selector changed")
	}
}

func TestDriverRefusesUnownedStubBeforeAllocation(t *testing.T) {
	file := filepath.Join(t.TempDir(), "PREDECESSOR_COMMIT")
	if os.WriteFile(file, []byte(syncPredecessorCommit), 0600) != nil {
		t.Fatal("capturemix stub fixture identity failed")
	}
	_, err := Run(context.Background(), Options{PredecessorCommit: syncPredecessorCommit, PredecessorCommitFile: file, StubEmailURL: "http://127.0.0.1:1/v1/dispatch"})
	if err == nil || err.Error() != "capturemix external stub refused" {
		t.Fatal("capturemix unowned stub was silently accepted")
	}
}

func TestObservationBudgetBoundsPredicateAndHonorsParentCancellation(t *testing.T) {
	for _, parentCanceled := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		if parentCanceled {
			cancel()
		}
		calls := 0
		err := pollWithBudget(ctx, time.Millisecond, func(observation context.Context) (bool, error) {
			calls++
			<-observation.Done()
			return false, nil
		})
		cancel()
		if err == nil || calls != 1 {
			t.Fatal("capturemix observation ignored its deadline or parent cancellation")
		}
	}
}

func TestRetentionRearmRequiresBothNewGuardOutcomes(t *testing.T) {
	p := &childProcess{
		logs: &observationWriter{
			refused:  map[string]bool{"message_retention": true, "agent_email_retention": true},
			receipts: map[string]int{"message_retention": 1, "agent_email_retention": 1},
		},
		metricBaseline:  map[string]float64{"message_retention": 1, "agent_email_retention": 1},
		receiptBaseline: map[string]int{"message_retention": 1, "agent_email_retention": 1},
	}
	metrics := "witself_worker_job_failures_total{job=\"message_retention\"} 2\nwitself_worker_job_failures_total{job=\"agent_email_retention\"} 2\n"
	if p.retentionRearmReady(metrics) {
		t.Fatal("capturemix retention rearm accepted preactivation receipts")
	}
	p.logs.receipts["message_retention"]++
	if p.retentionRearmReady(metrics) {
		t.Fatal("capturemix retention rearm accepted one job outcome")
	}
	p.logs.receipts["agent_email_retention"]++
	p.logs.refused["agent_email_retention"] = false
	if p.retentionRearmReady(metrics) {
		t.Fatal("capturemix retention rearm accepted a failure without guard cause")
	}
	p.logs.refused["agent_email_retention"] = true
	if !p.retentionRearmReady(metrics) {
		t.Fatal("capturemix retention rearm rejected both new guard outcomes")
	}
}
