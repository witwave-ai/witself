package localcell_test

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/localcell"
)

// The same test-only calculation is exercised through the internal hook/transport
// probe and the native proof. Four host bounds cover a queued restart tick (2H),
// a control health decision (H), and queue/response delivery margin (H).
func nativeServiceReadinessBudget(bound, firstReady time.Duration) time.Duration {
	return localcell.NativeServiceReadinessBudgetForTest(bound, firstReady)
}

type nativeServiceReadinessStatistics struct {
	attempts      int
	classes       map[string]int
	accepted      bool
	firstAccepted time.Duration
	longest       time.Duration
}

func (s *nativeServiceReadinessStatistics) observe(err error, elapsed, duration time.Duration) {
	class := nativeServiceControlDiagnostic(err)
	if s.classes == nil {
		s.classes = make(map[string]int)
	}
	s.classes[class]++
	s.attempts++
	s.longest = max(s.longest, duration)
	// A decoded response or an action failure proves that the authenticated
	// handler accepted the request. Local/transport/authentication refusals do
	// not. Unhealthy and deferred responses both prove authenticated HTTP
	// acceptance; their separate classes distinguish a verdict from no verdict.
	if !s.accepted && (class == "none" || class == "localcell: service_control_action_failed" || class == "localcell: service_control_deferred" || class == "localcell: service_unhealthy") {
		s.accepted, s.firstAccepted = true, elapsed
	}
}

func (s *nativeServiceReadinessStatistics) log(logf func(string, ...any), elapsed, budget time.Duration) {
	logf("native service readiness attempts=%d accepted=%t first_accepted_ms=%d longest_attempt_ms=%d elapsed_ms=%d budget_ms=%d", s.attempts, s.accepted, s.firstAccepted.Milliseconds(), s.longest.Milliseconds(), elapsed.Milliseconds(), budget.Milliseconds())
	classes := make([]string, 0, len(s.classes))
	for class := range s.classes {
		classes = append(classes, class)
	}
	sort.Strings(classes)
	for _, class := range classes {
		logf("native service readiness class=%s attempts=%d", class, s.classes[class])
	}
}

func TestNativeServiceReadinessBudget(t *testing.T) {
	const maximum = time.Duration(1<<63 - 1)
	for _, test := range []struct {
		name               string
		bound, first, want time.Duration
	}{
		{"first_host", nativeServiceHostBound, 0, 4 * time.Minute},
		{"invalid_negative", nativeServiceHostBound, -time.Second, 4 * time.Minute},
		{"floor", nativeServiceHostBound, 10 * time.Second, 4 * time.Minute},
		{"floor_boundary", nativeServiceHostBound, 80 * time.Second, 4 * time.Minute},
		{"scale", nativeServiceHostBound, 90 * time.Second, 4 * time.Minute},
		{"first_host_slow", nativeServiceHostBound, 89 * time.Second, 4 * time.Minute},
		{"cap_boundary", nativeServiceHostBound, 100 * time.Second, 4 * time.Minute},
		{"cap", nativeServiceHostBound, 200 * time.Second, 4 * time.Minute},
		{"overflow", nativeServiceHostBound, maximum, 4 * time.Minute},
		{"scaled_floor", time.Second, 0, 4 * time.Second},
		{"negative_bound", -time.Second, maximum, 0},
		{"cap_equals_floor", 2 * time.Minute, maximum, 8 * time.Minute},
		{"bound_overflow", maximum, maximum, maximum},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := nativeServiceReadinessBudget(test.bound, test.first); got != test.want {
				t.Fatalf("readiness budget = %s, want %s", got, test.want)
			}
		})
	}
	t.Run("cap_equals_path_floor", func(t *testing.T) {
		for _, bound := range []time.Duration{time.Second, nativeServiceHostBound, 2 * time.Minute, maximum / 4, maximum} {
			floor := nativeServiceReadinessBudget(bound, 0)
			for _, firstReady := range []time.Duration{-time.Second, 0, 80 * time.Second, 90 * time.Second, 100 * time.Second, maximum} {
				if ceiling := nativeServiceReadinessBudget(bound, firstReady); ceiling != floor {
					t.Fatal("readiness cap differs from its host-derived floor")
				}
			}
		}
	})
	t.Run("bound_drift_contract", func(t *testing.T) {
		source, err := os.ReadFile("service_native_test.go")
		if err != nil {
			t.Fatal("cannot read native readiness bound contract")
		}
		source = bytes.ReplaceAll(source, []byte("\r\n"), []byte("\n"))
		if !nativeServiceReadinessBoundContract(source) {
			t.Fatal("native host payload and control client lost their shared declared bound")
		}
		for _, change := range [][2]string{
			{"config.Timeout = nativeServiceHostBound", "config.Timeout = 60 * time.Second"},
			{"nativeServiceHostBound.String()", `"60s"`},
			{"const nativeServiceHostBound = 60 * time.Second", "const nativeServiceHostBound = 45 * time.Second"},
		} {
			mutant := bytes.Replace(source, []byte(change[0]), []byte(change[1]), 1)
			if bytes.Equal(source, mutant) || nativeServiceReadinessBoundContract(mutant) {
				t.Fatal("native readiness bound contract survived host/client drift")
			}
		}
	})
}

func nativeServiceReadinessBoundContract(source []byte) bool {
	file, err := parser.ParseFile(token.NewFileSet(), "native.go", source, 0)
	if err != nil {
		return false
	}
	declarations, clients, payloads := 0, 0, 0
	for _, declaration := range file.Decls {
		if value, ok := declaration.(*ast.GenDecl); ok && value.Tok == token.CONST {
			for _, specification := range value.Specs {
				constant, ok := specification.(*ast.ValueSpec)
				if ok && len(constant.Names) == 1 && constant.Names[0].Name == "nativeServiceHostBound" && len(constant.Values) == 1 && nativeServiceProgressNode(constant.Values[0]) == "60 * time.Second" {
					declarations++
				}
			}
		}
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != "TestNativeServiceLifecycle" {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			if assignment, ok := node.(*ast.AssignStmt); ok && nativeServiceProgressNode(assignment) == "cell, root := disposableCellWithCleanup(t, binaries, nil, func(ctx context.Context) error { if stopForCleanup != nil { return stopForCleanup(ctx) } return nil }, func(config *localcell.Config) { config.Timeout = nativeServiceHostBound })" {
				clients++
			}
			if value, ok := node.(*ast.KeyValueExpr); ok && nativeServiceProgressNode(value.Key) == "Args" {
				arguments, ok := value.Value.(*ast.CompositeLit)
				if ok {
					for index, argument := range arguments.Elts {
						if nativeServiceProgressNode(argument) == `"--timeout"` && index+1 < len(arguments.Elts) && nativeServiceProgressNode(arguments.Elts[index+1]) == "nativeServiceHostBound.String()" {
							payloads++
						}
					}
				}
			}
			return true
		})
	}
	return declarations == 1 && clients == 1 && payloads == 1
}

func TestNativeServiceReadinessStatisticsGuard(t *testing.T) {
	var stats nativeServiceReadinessStatistics
	stats.observe(errors.New("localcell: service_control_unavailable: transport:deadline"), 30*time.Second, 30*time.Second)
	stats.observe(errors.New("localcell: service_control_unavailable: transport:deadline"), 60*time.Second, 29*time.Second)
	stats.observe(errors.New("private-readiness-diagnostic"), 61*time.Second, time.Millisecond)
	stats.observe(errors.New("localcell: service_control_deferred"), 61500*time.Millisecond, 500*time.Millisecond)
	stats.observe(errors.New("localcell: service_unhealthy"), 62*time.Second, 750*time.Millisecond)
	stats.observe(nil, 63*time.Second, 250*time.Millisecond)
	var lines []string
	stats.log(func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }, 65*time.Second, 120*time.Second)
	want := []string{
		"native service readiness attempts=6 accepted=true first_accepted_ms=61500 longest_attempt_ms=30000 elapsed_ms=65000 budget_ms=120000",
		"native service readiness class=localcell: service_control_child_error_unclassified attempts=1",
		"native service readiness class=localcell: service_control_deferred attempts=1",
		"native service readiness class=localcell: service_control_unavailable: transport:deadline attempts=2",
		"native service readiness class=localcell: service_unhealthy attempts=1",
		"native service readiness class=none attempts=1",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatal("readiness statistics lost measured attempts, classified counts, acceptance timing, or budget")
	}
	summary := regexp.MustCompile(`^native service readiness attempts=[0-9]+ accepted=(true|false) first_accepted_ms=[0-9]+ longest_attempt_ms=[0-9]+ elapsed_ms=[0-9]+ budget_ms=[0-9]+$`)
	if !summary.MatchString(lines[0]) {
		t.Fatal("readiness statistics summary escaped its closed vocabulary")
	}
	for _, line := range lines[1:] {
		prefix, suffix := "native service readiness class=", " attempts="
		fields := strings.Split(strings.TrimPrefix(line, prefix), suffix)
		if len(fields) != 2 || !nativeServiceStepDiagnosticLine.MatchString("native service step=ready host_exit=running control="+fields[0]) || !regexp.MustCompile(`^[0-9]+$`).MatchString(fields[1]) {
			t.Fatal("readiness statistics class escaped its closed control vocabulary")
		}
	}
	for _, reason := range []string{"", "localcell: service_control_action_failed", "localcell: service_control_deferred", "localcell: service_unhealthy"} {
		var accepted nativeServiceReadinessStatistics
		var reply error
		if reason != "" {
			reply = errors.New(reason)
		}
		accepted.observe(reply, 2*time.Second, time.Second)
		accepted.observe(reply, 4*time.Second, time.Second)
		if !accepted.accepted || accepted.firstAccepted != 2*time.Second {
			t.Fatal("readiness statistics omitted or replaced the first accepted response")
		}
	}
	var empty nativeServiceReadinessStatistics
	lines = nil
	empty.log(func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }, time.Microsecond, 90*time.Second)
	if len(lines) != 1 || lines[0] != "native service readiness attempts=0 accepted=false first_accepted_ms=0 longest_attempt_ms=0 elapsed_ms=0 budget_ms=90000" {
		t.Fatal("readiness statistics lost absent-acceptance or millisecond semantics")
	}
}

func TestNativeServiceReadinessSourceContract(t *testing.T) {
	source, err := os.ReadFile("service_native_test.go")
	if err != nil {
		t.Fatal("cannot read native readiness source contract")
	}
	source = bytes.ReplaceAll(source, []byte("\r\n"), []byte("\n"))
	if !nativeServiceReadinessSourceContract(source) {
		t.Fatal("native readiness lost its measured budget or failure statistics binding")
	}
	for _, change := range [][2]string{
		{"firstStarted := time.Now()", "firstStarted := time.Time{}"},
		{"restartStarted := time.Now()", "restartStarted := time.Time{}"},
		{"firstReady := waitServiceReadyAfter(proofCtx, t, cell, firstStarted, 0, &diagnostics)", "firstReady := waitServiceReadyAfter(proofCtx, t, cell, firstStarted, time.Second, &diagnostics)"},
		{"waitServiceReadyAfter(proofCtx, t, cell, restartStarted, firstReady, &diagnostics)", "waitServiceReadyAfter(proofCtx, t, cell, restartStarted, 0, &diagnostics)"},
		{"budget := nativeServiceReadinessBudget(nativeServiceHostBound, firstReady)", "budget := 90 * time.Second"},
		{"ctx, cancel := proof.phase(parent, budget)", "ctx, cancel := context.WithTimeout(t.Context(), budget)"},
		{"stats.observe(err, time.Since(waitingStarted), time.Since(attemptStarted))", "stats.observe(nil, time.Since(waitingStarted), time.Since(attemptStarted))"},
		{"stats.log(t.Logf, time.Since(waitingStarted), budget)", "stats.log(t.Logf, 0, budget)"},
		{"if err == nil && status.Healthy {", "if err == nil || nativeServiceControlDiagnostic(err) == \"localcell: service_control_deferred\" {"},
	} {
		mutant := bytes.Replace(source, []byte(change[0]), []byte(change[1]), 1)
		if bytes.Equal(source, mutant) || nativeServiceReadinessSourceContract(mutant) {
			t.Fatal("native readiness source contract survived a measurement mutation")
		}
	}
	needle := []byte("stats.log(t.Logf, time.Since(waitingStarted), budget)")
	for start := 0; ; {
		offset := bytes.Index(source[start:], needle)
		if offset < 0 {
			break
		}
		offset += start
		mutant := append(append([]byte(nil), source[:offset]...), source[offset+len(needle):]...)
		if nativeServiceReadinessSourceContract(mutant) {
			t.Fatal("native readiness source contract survived removal of failure statistics")
		}
		start = offset + len(needle)
	}
}

func nativeServiceReadinessSourceContract(source []byte) bool {
	file, err := parser.ParseFile(token.NewFileSet(), "native.go", source, 0)
	if err != nil {
		return false
	}
	want := map[string]map[string]int{
		"TestNativeServiceLifecycle": {
			"firstStarted := time.Now()":   1,
			"restartStarted := time.Now()": 1,
			"firstReady := waitServiceReadyAfter(proofCtx, t, cell, firstStarted, 0, &diagnostics)": 1,
			"waitServiceReadyAfter(proofCtx, t, cell, restartStarted, firstReady, &diagnostics)":    1,
		},
		"waitServiceReadyAfter": {
			"waitingStarted := time.Now()": 1,
			"budget := nativeServiceReadinessBudget(nativeServiceHostBound, firstReady)": 1,
			"proof = diagnostics[0].proof":                                               1,
			"ctx, cancel := proof.phase(parent, budget)":                                 1,
			"stats.observe(err, time.Since(waitingStarted), time.Since(attemptStarted))": 1,
			"stats.log(t.Logf, time.Since(waitingStarted), budget)":                      2,
			"if err == nil && status.Healthy { return time.Since(started) }":             1,
		},
	}
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || want[function.Name.Name] == nil {
			continue
		}
		statements := want[function.Name.Name]
		ast.Inspect(function.Body, func(node ast.Node) bool {
			if _, ok := node.(ast.Stmt); ok {
				if key := nativeServiceProgressNode(node); statements[key] > 0 {
					statements[key]--
				}
			}
			return true
		})
	}
	for _, statements := range want {
		for _, remaining := range statements {
			if remaining != 0 {
				return false
			}
		}
	}
	return true
}
