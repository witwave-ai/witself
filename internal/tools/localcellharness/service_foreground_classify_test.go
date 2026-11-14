package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/localcell"
)

type foregroundExitKind uint8

const (
	foregroundCommandBudgetExpired foregroundExitKind = iota
	foregroundParentBudgetExpired
	foregroundExitedWithError
	foregroundExitedCleanly
)

// Wait can complete after either context expires, including a nil Wait result.
// Classify the observed state in this order: command budget, parent budget,
// error exit, clean exit. exec.Cmd.Wait returns both process and copier/WaitDelay
// errors through exitErr. A clean exit is fatal before readiness, accepted after
// shutdown; the Windows call sites, not this pure decision, implement that policy.
func classifyForegroundExit(commandErr, parentErr, exitErr error) foregroundExitKind {
	switch {
	case commandErr != nil:
		return foregroundCommandBudgetExpired
	case parentErr != nil:
		return foregroundParentBudgetExpired
	case exitErr != nil:
		return foregroundExitedWithError
	default:
		return foregroundExitedCleanly
	}
}

// The install refusal accepts any non-nil run error with the expected cause.
// Empty means that passing case; all failures use closed, value-free words.
// Observe budgets before exit status, with the command's parent budget first.
func classifyServiceInstallFailure(parentErr, spawnErr, runErr error, hasCause bool) string {
	switch {
	case parentErr != nil:
		return "parent_budget"
	case spawnErr != nil:
		return "deadline"
	case runErr == nil:
		return "exit_zero"
	case hasCause:
		return ""
	default:
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			return "exit_without_cause"
		}
		return "start_failed"
	}
}

// Preserve only the first closed host failure site, never arbitrary stderr or
// a guessed OS cause. Existing guarded detail lines retain every joined site
// and lease-publication class. Every return is a literal so the portable raw-
// capture contract can prove this one bounded CI diagnostic is value-free.
func foregroundFailureClass(captured []byte) string {
	if len(captured) == 0 {
		return "no_output"
	}
	// The logger also reports accepted multiline shutdown output. Give that
	// case a success label, never an interrupted-tick failure classification.
	if foregroundShutdownFailure(captured) == "" {
		return "shutdown_accepted"
	}
	for _, line := range foregroundOutputLines(captured) {
		switch strings.TrimSuffix(line, "\r") {
		case "native service host site=host_config":
			return "host_config"
		case "native service host site=host_descriptor":
			return "host_descriptor"
		case "native service host site=host_prepare":
			return "host_prepare"
		case "native service host site=host_persisted_config":
			return "host_persisted_config"
		case "native service host site=initial_descriptor":
			return "initial_descriptor"
		case "native service host site=lease_directory":
			return "lease_directory"
		case "native service host site=lease_record":
			return "lease_record"
		case "native service host site=lease_write":
			return "lease_write"
		case "native service host site=control_secret":
			return "control_secret"
		case "native service host site=control_transport":
			return "control_transport"
		case "native service host site=control_cancelled":
			return "control_cancelled"
		case "native service host site=endpoint_descriptor":
			return "endpoint_descriptor"
		case "native service host site=endpoint_read":
			return "endpoint_read"
		case "native service host site=endpoint_write":
			return "endpoint_write"
		case "native service host site=endpoint_lease":
			return "endpoint_lease"
		case "native service host site=endpoint_remove":
			return "endpoint_remove"
		case "native service host site=supervisor_load":
			return "supervisor_load"
		case "native service host site=supervisor_traversal":
			return "supervisor_traversal"
		case "native service host site=supervisor_save":
			return "supervisor_save"
		case "native service host site=supervisor_start":
			return "supervisor_start"
		case "native service host site=supervisor_stop":
			return "supervisor_stop"
		case "native service host site=supervisor_repair":
			return "supervisor_repair"
		case "native service host site=supervisor_reconcile":
			return "supervisor_reconcile"
		case "native service host site=supervisor_health":
			return "supervisor_health"
		case "native service host site=supervisor_command":
			return "supervisor_command"
		case "native service host site=health_lock":
			return "health_lock"
		case "native service host site=health_tree":
			return "health_tree"
		case "native service host site=health_database":
			return "health_database"
		case "native service host site=health_credential":
			return "health_credential"
		case "native service host site=health_probe":
			return "health_probe"
		case "native service host site=health_ready":
			return "health_ready"
		case "native service host site=startup_reconcile":
			return "startup_reconcile"
		case "native service host site=startup_repair":
			return "startup_repair"
		case "native service host site=startup_stop":
			return "startup_stop"
		case "native service host site=shutdown_repair":
			return "shutdown_repair"
		case "native service host site=shutdown_stop":
			return "shutdown_stop"
		case "native service host site=boot_binding":
			return "boot_binding"
		case "native service host site=descriptor_write":
			return "descriptor_write"
		case "native service host site=unknown":
			return "unknown"
		}
	}
	// No known site line: name only the output's closed shape, never its bytes.
	// The first matching shape wins: a Go crash, a known localcell fixed cause, a
	// shutdown receipt outside the stopped set, then the count of lines
	// with visible text. Every class keeps the unclassified stem.
	lines := foregroundOutputLines(captured)
	for _, line := range lines {
		if strings.HasPrefix(line, "panic: ") {
			return "unclassified_go_panic"
		}
	}
	for _, line := range lines {
		if strings.HasPrefix(line, "fatal error: ") {
			return "unclassified_go_fatal_error"
		}
	}
	for _, line := range lines {
		for _, cause := range foregroundFixedCauses {
			if strings.Contains(line, cause.Error()) {
				return "unclassified_fixed_cause"
			}
		}
	}
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "service_shutdown phase=startup "):
			return "unclassified_receipt_startup"
		case strings.HasPrefix(line, "service_shutdown phase=tick "):
			return "unclassified_receipt_tick"
		case strings.HasPrefix(line, "service_shutdown phase=control "):
			return "unclassified_receipt_control"
		}
	}
	text := 0
	for _, line := range lines {
		if strings.TrimSpace(line) != "" {
			text++
		}
	}
	switch text {
	case 0:
		return "unclassified_blank"
	case 1:
		return "unclassified_one_line"
	default:
		return "unclassified_multi_line"
	}
}

// Exported, value-free localcell refusals the foreground host can print alone
// or inside a wrapped error line. Matching one selects only its class word.
var foregroundFixedCauses = []error{
	localcell.ErrLocked,
	localcell.ErrLocalMigrationPhaseRequired,
	localcell.ErrLocalRuntimeLoginOwnsTables,
	localcell.ErrLocalPrincipalCredentialsRefused,
	localcell.ErrServiceManagerUnavailable,
	localcell.ErrServiceOwnershipConflict,
	localcell.ErrServiceRegistrationFailed,
	localcell.ErrServiceRemovalFailed,
	localcell.ErrServiceDesiredState,
	localcell.ErrServiceShutdownForced,
	localcell.ErrServiceHostActive,
	localcell.ErrServiceControlRefused,
}

// Every closed host site the classifier recognizes, in its switch order.
var foregroundFailureSites = []string{
	"host_config", "host_descriptor", "host_prepare", "host_persisted_config",
	"initial_descriptor", "lease_directory", "lease_record", "lease_write",
	"control_secret", "control_transport", "control_cancelled",
	"endpoint_descriptor", "endpoint_read", "endpoint_write", "endpoint_lease", "endpoint_remove",
	"supervisor_load", "supervisor_traversal", "supervisor_save", "supervisor_start",
	"supervisor_stop", "supervisor_repair", "supervisor_reconcile", "supervisor_health", "supervisor_command",
	"health_lock", "health_tree", "health_database", "health_credential", "health_probe", "health_ready",
	"startup_reconcile", "startup_repair", "startup_stop", "shutdown_repair", "shutdown_stop",
	"boot_binding", "descriptor_write", "unknown",
}

// Accepted shutdown output has a success label. Other output without a known
// site keeps the unclassified stem and its shape; empty output keeps its class.
var foregroundFailureShapes = []string{
	"no_output", "shutdown_accepted", "unclassified_go_panic", "unclassified_go_fatal_error", "unclassified_fixed_cause",
	"unclassified_receipt_startup", "unclassified_receipt_tick", "unclassified_receipt_control",
	"unclassified_blank", "unclassified_one_line", "unclassified_multi_line",
}

func TestForegroundFailureClass(t *testing.T) {
	for _, site := range foregroundFailureSites {
		t.Run(site, func(t *testing.T) {
			if foregroundFailureClass([]byte("native service host site="+site+"\n")) != site {
				t.Fatal("foreground failure diagnostic lost a known host site")
			}
		})
	}
	const idle = "service_shutdown phase=idle operation=none journal_pending=false journal_read_ok=true\n"
	for _, test := range []struct{ name, captured, want string }{
		{"empty", "", "no_output"},
		{"private", "private /synthetic/home", "unclassified_one_line"},
		{"unknown_site", "native service host site=private", "unclassified_one_line"},
		{"private_suffix", "native service host site=lease_write /synthetic/home\n", "unclassified_one_line"},
		{"partial_site", "native service host site=lease_writ", "unclassified_one_line"},
		{"prefix", "private native service host site=lease_write\n", "unclassified_one_line"},
		{"crlf", "native service host site=lease_write\r\n", "lease_write"},
		{"lease_detail", "native service host lease_publish class=sharing_violation attempts=2 elapsed_ms=25\nnative service host site=lease_write\nlocalcell: service_control_refused\n", "lease_write"},
		{"repeated", "native service host site=lease_write\nnative service host site=lease_write\n", "lease_write"},
		{"joined", "native service host site=startup_repair\nnative service host site=shutdown_stop\n", "startup_repair"},
		{"peer_only", "service_control accept=rejected step=peer:pid\n", "unclassified_one_line"},
		{"site_over_panic", "panic: private /synthetic/home\nnative service host site=supervisor_reconcile\n", "supervisor_reconcile"},
		{"site_over_receipt", "native service host site=supervisor_command\n" + strings.Replace(idle, "phase=idle", "phase=control", 1), "supervisor_command"},
		{"go_panic", "panic: private /synthetic/home\n\ngoroutine 1 [running]:\nmain.main()\n", "unclassified_go_panic"},
		{"go_panic_crlf", "panic: private /synthetic/home\r\n", "unclassified_go_panic"},
		{"go_panic_over_fatal", "fatal error: panic during panic\npanic: private /synthetic/home [recovered]\n", "unclassified_go_panic"},
		{"go_panic_indented", " panic: private /synthetic/home\n", "unclassified_one_line"},
		{"go_fatal_error", "fatal error: all goroutines are asleep - deadlock!\n\ngoroutine 1 [chan receive]:\n", "unclassified_go_fatal_error"},
		{"go_fatal_over_cause", "localcell: service_control_refused\nfatal error: concurrent map writes\n", "unclassified_go_fatal_error"},
		{"fixed_cause", "localcell: service_control_refused\n", "unclassified_fixed_cause"},
		{"fixed_cause_wrapped", "local cell start: principal_verified failed: local_principal_credentials_refused\n", "unclassified_fixed_cause"},
		{"fixed_cause_detail", "service_manager_unavailable: use explicit localcellharness run foreground fallback: adapter:platform: unsupported\n", "unclassified_fixed_cause"},
		{"fixed_cause_over_receipt", "localcell: service_shutdown_forced\n" + strings.Replace(idle, "phase=idle", "phase=control", 1), "unclassified_fixed_cause"},
		{"receipt_startup", strings.Replace(idle, "phase=idle operation=none", "phase=startup operation=reconcile", 1), "unclassified_receipt_startup"},
		{"receipt_idle", idle, "shutdown_accepted"},
		{"receipt_tick_accepted", strings.Replace(idle, "phase=idle", "phase=tick", 1), "shutdown_accepted"},
		{"receipt_tick_outside_set", strings.Replace(idle, "phase=idle operation=none", "phase=tick operation=stop", 1), "unclassified_receipt_tick"},
		{"receipt_tick_pending", strings.ReplaceAll(strings.Replace(idle, "phase=idle", "phase=tick", 1), "journal_pending=false", "journal_pending=true"), "unclassified_receipt_tick"},
		{"receipt_control", strings.Replace(idle, "phase=idle", "phase=control", 1), "unclassified_receipt_control"},
		{"receipt_after_peer", "service_control accept=rejected step=peer:pid\n" + strings.Replace(idle, "phase=idle", "phase=tick", 1), "shutdown_accepted"},
		{"receipt_phase_token", strings.Replace(idle, "phase=idle", "phase=tickle", 1), "unclassified_one_line"},
		{"receipt_idle_fields", strings.Replace(idle, "journal_pending=false", "journal_pending=true", 1), "unclassified_one_line"},
		{"receipt_idle_after_peer", "service_control accept=rejected step=peer:pid\n" + idle, "shutdown_accepted"},
		{"blank", "\n", "unclassified_blank"},
		{"blank_whitespace", " \r\n\t\n", "unclassified_blank"},
		{"one_line_between_blanks", "\n\nprivate /synthetic/home\n\n", "unclassified_one_line"},
		{"multi_line", "private /synthetic/home\nprivate /synthetic/other\n", "unclassified_multi_line"},
		{"multi_line_partial", "private /synthetic/home\nprivate", "unclassified_multi_line"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if foregroundFailureClass([]byte(test.captured)) != test.want {
				t.Fatal("foreground failure diagnostic violated its closed value-free classes")
			}
		})
	}
}

// The classifier's literal returns are its whole CI vocabulary: every site and
// shape and no other word. Each must reach the real checker as an accepted
// class beside the unchanged readiness fatal; all rows share one launch.
func TestForegroundFailureClassEnrollment(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "service_foreground_classify_test.go", nil, 0)
	if err != nil {
		t.Fatal("cannot parse foreground failure classifier")
	}
	var classes []string
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != "foregroundFailureClass" {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			statement, ok := node.(*ast.ReturnStmt)
			if !ok {
				return true
			}
			value := ""
			if len(statement.Results) == 1 {
				if literal, ok := statement.Results[0].(*ast.BasicLit); ok && literal.Kind == token.STRING {
					value, _ = strconv.Unquote(literal.Value)
				}
			}
			classes = append(classes, value)
			return true
		})
	}
	want := append(slices.Clone(foregroundFailureSites), foregroundFailureShapes...)
	if !slices.Equal(slices.Sorted(slices.Values(classes)), slices.Sorted(slices.Values(want))) {
		t.Fatal("foreground failure classifier vocabulary changed")
	}
	const fatal = "foreground host exited with error before authenticated status after restart"
	var rows []foregroundClassEnrollment
	for _, class := range want {
		rows = append(rows, foregroundClassEnrollment{class, class, class, fatal, fatal})
	}
	assertForegroundClassEnrollments(t, rows)
}

func TestWindowsForegroundFailureDiagnosticContract(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "service_subprocess_windows_test.go", nil, 0)
	if err != nil {
		t.Fatal("cannot parse Windows foreground diagnostic source")
	}
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != "windowsLogForegroundExit" {
			continue
		}
		if len(function.Body.List) < 2 || foregroundSource(t, function.Body.List[1]) != `t.Logf("foreground_exit class=%s", foregroundFailureClass(captured))` {
			t.Fatal("Windows foreground exit lost its unconditional value-free failure class")
		}
		return
	}
	t.Fatal("Windows foreground exit diagnostic helper is missing")
}

func TestClassifyForegroundExit(t *testing.T) {
	// Explicit expectations cover every nil/non-nil combination, including each
	// adjacent precedence inversion and the R6 command-expired OR clean mutant.
	for _, test := range []struct {
		name                           string
		commandErr, parentErr, exitErr error
		want                           foregroundExitKind
	}{
		{"clean", nil, nil, nil, foregroundExitedCleanly},
		{"error", nil, nil, errors.New("synthetic exit"), foregroundExitedWithError},
		{"parent_clean", nil, context.DeadlineExceeded, nil, foregroundParentBudgetExpired},
		{"parent_error", nil, context.DeadlineExceeded, errors.New("synthetic exit"), foregroundParentBudgetExpired},
		{"command_clean", context.DeadlineExceeded, nil, nil, foregroundCommandBudgetExpired},
		{"command_error", context.DeadlineExceeded, nil, errors.New("synthetic exit"), foregroundCommandBudgetExpired},
		{"both_clean", context.DeadlineExceeded, context.DeadlineExceeded, nil, foregroundCommandBudgetExpired},
		{"both_error", context.DeadlineExceeded, context.DeadlineExceeded, errors.New("synthetic exit"), foregroundCommandBudgetExpired},
		{"command_cancelled", context.Canceled, context.DeadlineExceeded, exec.ErrWaitDelay, foregroundCommandBudgetExpired},
		{"parent_cancelled", nil, context.Canceled, exec.ErrWaitDelay, foregroundParentBudgetExpired},
		{"wait_delay", nil, nil, exec.ErrWaitDelay, foregroundExitedWithError},
	} {
		t.Run(test.name, func(t *testing.T) {
			if classifyForegroundExit(test.commandErr, test.parentErr, test.exitErr) != test.want {
				t.Fatal("foreground exit classification violated budget precedence")
			}
		})
	}
}

// Single source of truth for the kind x generation contract and the report's
// literal-to-cause table. Empty literals mean accepted clean shutdown. The four
// wait branches retain their existing direct literals and are checked too.
var foregroundDiagnosticLiterals = []struct {
	phase, branch string
	literals      [2]string
}{
	{"readiness", "foregroundCommandBudgetExpired", [2]string{
		"foreground command budget expired before authenticated status on first start",
		"foreground command budget expired before authenticated status after restart"}},
	{"readiness", "foregroundParentBudgetExpired", [2]string{
		"foreground synthetic budget expired before authenticated status on first start",
		"foreground synthetic budget expired before authenticated status after restart"}},
	{"readiness", "foregroundExitedWithError", [2]string{
		"foreground host exited with error before authenticated status on first start",
		"foreground host exited with error before authenticated status after restart"}},
	{"readiness", "foregroundExitedCleanly", [2]string{
		"foreground host exited cleanly before authenticated status on first start",
		"foreground host exited cleanly before authenticated status after restart"}},
	{"shutdown", "foregroundCommandBudgetExpired", [2]string{
		"foreground command budget expired during shutdown on first start",
		"foreground command budget expired during shutdown after restart"}},
	{"shutdown", "foregroundParentBudgetExpired", [2]string{
		"foreground synthetic budget expired during shutdown on first start",
		"foreground synthetic budget expired during shutdown after restart"}},
	{"shutdown", "foregroundExitedWithError", [2]string{
		"foreground host exited with error during shutdown on first start",
		"foreground host exited with error during shutdown after restart"}},
	{"shutdown", "foregroundExitedCleanly", [2]string{}},
	{"readiness", "parent_wait", [2]string{
		"foreground readiness exceeded synthetic budget on first start",
		"foreground readiness exceeded synthetic budget after restart"}},
	{"readiness", "allowance_wait", [2]string{
		"foreground host did not expose authenticated status within allowance on first start",
		"foreground host did not expose authenticated status within allowance after restart"}},
	{"shutdown", "parent_wait", [2]string{
		"foreground shutdown exceeded synthetic budget on first start",
		"foreground shutdown exceeded synthetic budget after restart"}},
	{"shutdown", "allowance_wait", [2]string{
		"foreground console shutdown exceeded allowance on first start",
		"foreground console shutdown exceeded allowance after restart"}},
}

func TestWindowsForegroundExitClassifierContract(t *testing.T) {
	// Parse Windows source on every platform. Pin both the call arguments and
	// each enum case's generation-specific direct t.Fatal; merely finding all
	// literals cannot detect a misplaced clean-exit branch (R6 M5).
	file, err := parser.ParseFile(token.NewFileSet(), "service_subprocess_windows_test.go", nil, 0)
	if err != nil {
		t.Fatal("cannot parse Windows foreground classifier source")
	}
	var selections []*ast.SelectStmt
	ast.Inspect(file, func(node ast.Node) bool {
		selection, ok := node.(*ast.SelectStmt)
		if !ok {
			return true
		}
		for _, statement := range selection.Body.List {
			clause := statement.(*ast.CommClause)
			if foregroundSource(t, clause.Comm) == "exitErr := <-done" {
				selections = append(selections, selection)
			}
		}
		return true
	})
	if len(selections) != 2 {
		t.Fatal("Windows foreground classifier lost readiness or shutdown exit selection")
	}
	for index, phase := range []string{"readiness", "shutdown"} {
		t.Run(phase, func(t *testing.T) {
			branches := make(map[string][]ast.Stmt)
			for _, statement := range selections[index].Body.List {
				clause := statement.(*ast.CommClause)
				switch foregroundSource(t, clause.Comm) {
				case "exitErr := <-done":
					// No extra conditional may gate the decision or bypass it.
					prefix := 1
					if phase == "readiness" {
						prefix++
					}
					if len(clause.Body) != prefix+1 || foregroundSource(t, clause.Body[0]) != "finished = true" {
						t.Fatal("Windows foreground exit classifier is conditionally bypassed")
					}
					if phase == "readiness" && foregroundSource(t, clause.Body[1]) != "windowsLogForegroundExit(t, command, captured.Bytes())" {
						t.Fatal("Windows foreground readiness exit lost guarded diagnostics")
					}
					decision, ok := clause.Body[prefix].(*ast.SwitchStmt)
					if !ok || decision.Init != nil || foregroundSource(t, decision.Tag) != "classifyForegroundExit(commandCtx.Err(), ctx.Err(), exitErr)" {
						t.Fatal("Windows foreground exit must use the pure classifier with observed errors")
					}
					if len(decision.Body.List) != 4 {
						t.Fatal("Windows foreground exit classifier lost an explicit kind")
					}
					for _, statement := range decision.Body.List {
						branch := statement.(*ast.CaseClause)
						if len(branch.List) != 1 {
							t.Fatal("Windows foreground exit classifier requires one explicit kind per case")
						}
						name := foregroundSource(t, branch.List[0])
						if _, duplicate := branches[name]; duplicate {
							t.Fatal("Windows foreground exit classifier repeats a kind")
						}
						branches[name] = branch.Body
					}
				case "<-ctx.Done()":
					branches["parent_wait"] = clause.Body
				case "<-deadline.C", "<-time.After(allowance)":
					branches["allowance_wait"] = clause.Body
				}
			}
			if len(branches) != 6 {
				t.Fatal("Windows foreground classifier branch set changed")
			}
			for _, contract := range foregroundDiagnosticLiterals {
				if contract.phase != phase {
					continue
				}
				t.Run(contract.branch, func(t *testing.T) {
					body, exists := branches[contract.branch]
					if !exists {
						t.Fatal("Windows foreground classifier lacks a contracted branch")
					}
					want := ""
					if contract.literals[0] != "" {
						switch contract.branch {
						case "parent_wait":
							want = fmt.Sprintf(`t.Logf("foreground_exit class=%%s", "%s_budget");`, phase)
						case "allowance_wait":
							want = fmt.Sprintf(`t.Logf("foreground_exit class=%%s", "%s_allowance");`, phase)
						}
						if phase == "shutdown" && contract.branch != "parent_wait" && contract.branch != "allowance_wait" {
							want = "windowsLogForegroundExit(t, command, captured.Bytes());"
						}
						want += fmt.Sprintf("if iteration == 0 { t.Fatal(%q) }; t.Fatal(%q)", contract.literals[0], contract.literals[1])
					}
					fixture, err := parser.ParseFile(token.NewFileSet(), "contract", "package main; func contract() {"+want+"}", 0)
					if err != nil {
						t.Fatal("invalid foreground literal contract")
					}
					expected := fixture.Decls[0].(*ast.FuncDecl).Body
					// Normalize positions as well as whitespace; source line breaks
					// must not affect the semantic statement/literal comparison.
					actual := &ast.BlockStmt{List: body}
					if foregroundSource(t, actual) != foregroundSource(t, expected) {
						t.Fatal("Windows foreground kind and generation selected the wrong literal or clean policy")
					}
				})
			}
		})
	}
}

func foregroundSource(t *testing.T, node ast.Node) string {
	t.Helper()
	if node == nil {
		return ""
	}
	var output bytes.Buffer
	if format.Node(&output, token.NewFileSet(), node) != nil {
		t.Fatal("cannot format foreground source contract")
	}
	return output.String()
}
