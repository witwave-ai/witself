package localcell

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func assertWindowsTaskFailure(t *testing.T, err error, step, reason string) {
	t.Helper()
	var failure *windowsTaskFailure
	if !errors.As(err, &failure) || failure.step != step || failure.reason != reason || !errors.Is(err, ErrServiceOwnershipConflict) {
		t.Fatal("task refusal did not raise at the intended fixed site")
	}
}

func TestWindowsTaskXMLRejectsPercentReferences(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*ServiceDescriptor)
	}{
		{"root", func(d *ServiceDescriptor) { d.Root = `C:\%PRIVATE%\cell` }},
		{"executable", func(d *ServiceDescriptor) { d.Payload.Host = `C:\%PRIVATE%\cell.exe` }},
		{"generation", func(d *ServiceDescriptor) { d.Payload.Generation = "%PRIVATE%" }},
		{"arguments", func(d *ServiceDescriptor) { d.Payload.Args = []string{"--fixture=%PRIVATE%"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := windowsTaskTestDescriptor()
			tc.change(&d)
			raw, err := RenderWindowsLogonTaskXML(d)
			assertWindowsTaskFailure(t, err, "inspect:action", "task_value_percent_reference")
			if len(raw) != 0 {
				t.Fatal("percent reference reached task XML")
			}
		})
	}
	// WorkingDirectory is currently derived from Root; Author is absent and
	// Description fixed. Test their rendering boundary independently so later
	// additions cannot create an unchecked task-visible expansion input.
	for _, tc := range []struct {
		name   string
		change func(*windowsTaskRenderValues)
	}{
		{"working_directory", func(v *windowsTaskRenderValues) { v.workingDirectory = `C:\%PRIVATE%\cell` }},
		{"author", func(v *windowsTaskRenderValues) { v.author = "%PRIVATE%" }},
		{"description", func(v *windowsTaskRenderValues) { v.description = "%PRIVATE%" }},
		{"rendered_arguments", func(v *windowsTaskRenderValues) { v.arguments = "%PRIVATE%" }},
		{"rendered_root", func(v *windowsTaskRenderValues) { v.root = "%PRIVATE%" }},
		{"rendered_executable", func(v *windowsTaskRenderValues) { v.executable = "%PRIVATE%" }},
		{"rendered_generation", func(v *windowsTaskRenderValues) { v.generation = "%PRIVATE%" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var values windowsTaskRenderValues
			tc.change(&values)
			assertWindowsTaskFailure(t, values.validate(), "inspect:action", "task_value_percent_reference")
		})
	}
}

func TestWindowsTaskXMLFieldEqualityRaisingSites(t *testing.T) {
	d := windowsTaskTestDescriptor()
	raw, err := RenderWindowsLogonTaskXML(d)
	if err != nil {
		t.Fatal("field fixture render failed")
	}
	decoded, err := decodeWindowsTaskXML(raw)
	if err != nil {
		t.Fatal("field fixture decode failed")
	}
	xml := "<?xml version=\"1.0\" encoding=\"UTF-8\"?>" + string(decoded)
	for _, tc := range []struct{ name, before, after, site string }{
		{"UserId", "<UserId>" + d.UID + "</UserId>", "<UserId>" + d.UID + "9</UserId>", "inspect:principal"},
		{"WorkingDirectory", "<WorkingDirectory>", "<WorkingDirectory>foreign", "inspect:action"},
		{"ExecutionTimeLimit", "<ExecutionTimeLimit>PT0S", "<ExecutionTimeLimit>PT1H", "inspect:settings"},
		{"StopIfGoingOnBatteries", "<StopIfGoingOnBatteries>false", "<StopIfGoingOnBatteries>true", "inspect:settings"},
		{"DisallowStartIfOnBatteries", "<DisallowStartIfOnBatteries>false", "<DisallowStartIfOnBatteries>true", "inspect:settings"},
		{"RunOnlyIfNetworkAvailable", "<RunOnlyIfNetworkAvailable>false", "<RunOnlyIfNetworkAvailable>true", "inspect:settings"},
		{"RunOnlyIfIdle", "<RunOnlyIfIdle>false", "<RunOnlyIfIdle>true", "inspect:settings"},
		{"AllowStartOnDemand", "<AllowStartOnDemand>true", "<AllowStartOnDemand>false", "inspect:settings"},
		{"MultipleInstancesPolicy", "<MultipleInstancesPolicy>IgnoreNew", "<MultipleInstancesPolicy>Parallel", "inspect:settings"},
		{"TriggerUserId", "<UserId>" + d.UID + "</UserId>", "<UserId>" + d.UID + "9</UserId>", "inspect:trigger"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// UserId normally occurs in both principal and trigger; change both
			// for the principal case, and only the first for the trigger case.
			mutated := strings.ReplaceAll(xml, tc.before, tc.after)
			if tc.name == "TriggerUserId" {
				mutated = strings.Replace(xml, tc.before, tc.after, 1)
			}
			if mutated == xml {
				t.Fatal("field mutation did not change fixture")
			}
			assertWindowsTaskFailure(t, ValidateWindowsLogonTaskXML([]byte(mutated), d), tc.site, "mismatch")
		})
	}
}

var windowsTaskExpectedSites = []string{
	"capability:session", "query:result", "inspect:principal", "inspect:trigger", "inspect:action", "inspect:settings", "inspect:host_owner", "create", "post_create", "remove:host_exit", "remove:ownership", "delete", "remove:absence",
}

func TestWindowsTaskDiagnosticVocabularyClosed(t *testing.T) {
	for _, step := range windowsTaskExpectedSites {
		if !validWindowsTaskDiagnostic(ErrServiceOwnershipConflict, step, "mismatch") {
			t.Fatal("required R8 site missing")
		}
	}
	for _, step := range []string{"inspect:xml", "inspect:query", "apply:create", "remove:drain", "remove:query", "remove:delete", "private-site"} {
		if validWindowsTaskDiagnostic(ErrServiceOwnershipConflict, step, "mismatch") {
			t.Fatal("diagnostic vocabulary accepted unapproved site")
		}
	}
	private := windowsTaskTestDescriptor()
	for _, value := range []string{private.UID, private.Root, "query_result_untyped:" + private.UID, strings.Repeat("Q", 48), "caller_env=PRIVATE"} {
		for _, err := range []error{
			windowsTaskError(ErrServiceOwnershipConflict, value, 7, "mismatch"),
			windowsTaskError(ErrServiceOwnershipConflict, "query:result", 7, value),
			windowsTaskError(errors.New(value), "query:result", 7, "mismatch"),
			&windowsTaskFailure{kind: errors.New(value), step: value, code: 7, reason: value},
		} {
			if err.Error() != "service_ownership_conflict: query:result: code=7 reason=diagnostic_argument_refused" {
				t.Fatal("diagnostic accepted private or open arguments")
			}
		}
	}
}

func TestWindowsTaskQueryDiagnosticsNeverContainManagerOutput(t *testing.T) {
	d := windowsTaskTestDescriptor()
	name, _ := WindowsLogonTaskName(d)
	for _, output := range []string{d.UID, d.Root, name, `\\.\pipe\private-fixture`, strings.Repeat("Q", 48), strings.Repeat("a", 64), "caller_env=PRIVATE"} {
		for _, tc := range []windowsTaskCommandResult{
			{Output: []byte(output), Code: 1},
			{Output: []byte(output), Code: 1, Typed: true},
			{Output: []byte(output), Code: 0x80070002, Typed: true},
		} {
			_, err := classifyWindowsTaskQuery(tc, nil)
			var failure *windowsTaskFailure
			if !errors.As(err, &failure) || failure.step != "query:result" || failure.reason != "query_result_untyped" || strings.Contains(err.Error(), output) {
				t.Fatal("query diagnostic admitted raw manager output")
			}
		}
	}
}

func TestWindowsTaskVerboseDiagnosticsArePrivate(t *testing.T) {
	if os.Getenv("WITSELF_TASK_PRIVACY_CHILD") == "1" {
		return
	}
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	executable, err := os.Executable()
	if err != nil {
		t.Fatal("privacy fixture executable unavailable")
	}
	command := exec.CommandContext(t.Context(), executable, "-test.run=^TestWindowsTask", "-test.v")
	command.Env = append(os.Environ(), "WITSELF_TASK_PRIVACY_CHILD=1")
	output, err := command.CombinedOutput()
	// Never print captured output, including on a child failure. Inspect privacy
	// before status so another failed assertion cannot mask disclosure.
	for _, pattern := range []string{`S-1-[0-9-]+`, `[A-Za-z]:\\`, `\\Witself-Cell-`, `\\\\\.\\pipe\\`} {
		if regexp.MustCompile(pattern).Match(output) {
			t.Fatal("verbose task suite disclosed a private fixture class")
		}
	}
	if err != nil {
		t.Fatal("verbose task privacy child failed")
	}
}

func TestWindowsTaskDiagnosticSourceContract(t *testing.T) {
	paths, err := filepath.Glob("service_windows_task*.go")
	if err != nil || len(paths) == 0 {
		t.Fatal("diagnostic source unavailable")
	}
	seen := map[string]bool{}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal("diagnostic source unavailable")
		}
		// Source contracts must see the same tree on Windows CRLF checkouts.
		source := strings.ReplaceAll(string(raw), "\r\n", "\n")
		file, err := parser.ParseFile(token.NewFileSet(), path, source, 0)
		if err != nil {
			t.Fatal("diagnostic source parse failed")
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			name, ok := call.Fun.(*ast.Ident)
			if !ok || name.Name != "windowsTaskError" {
				return true
			}
			if len(call.Args) != 4 {
				t.Fatal("diagnostic call changed arity")
			}
			kind, kindOK := call.Args[0].(*ast.Ident)
			step, stepOK := call.Args[1].(*ast.BasicLit)
			reason, reasonOK := call.Args[3].(*ast.BasicLit)
			if !kindOK || !stepOK || !reasonOK || step.Kind != token.STRING || reason.Kind != token.STRING {
				t.Fatal("diagnostic raising site uses nonliteral private arguments")
			}
			stepValue, stepErr := strconv.Unquote(step.Value)
			reasonValue, reasonErr := strconv.Unquote(reason.Value)
			if stepErr != nil || reasonErr != nil || !slices.Contains([]string{"ErrServiceOwnershipConflict", "ErrServiceManagerUnavailable", "ErrServiceRegistrationFailed", "ErrServiceDesiredState", "ErrServiceRemovalFailed"}, kind.Name) || !slices.Contains(windowsTaskExpectedSites, stepValue) || !validWindowsTaskDiagnostic(ErrServiceOwnershipConflict, stepValue, reasonValue) {
				t.Fatal("diagnostic raising site escaped the closed vocabulary")
			}
			seen[stepValue] = true
			return true
		})
	}
	if len(seen) != len(windowsTaskExpectedSites) {
		t.Fatal("production is missing a required R8 raising site")
	}
	// Pin the switch vocabulary independently of the representative runtime
	// probes, so an unused newly accepted site cannot quietly expand the API.
	raw, err := os.ReadFile("service_windows_task.go")
	if err != nil {
		t.Fatal("diagnostic vocabulary source unavailable")
	}
	file, err := parser.ParseFile(token.NewFileSet(), "", strings.ReplaceAll(string(raw), "\r\n", "\n"), 0)
	if err != nil {
		t.Fatal("diagnostic vocabulary parse failed")
	}
	var declared []string
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != "validWindowsTaskDiagnostic" {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			statement, ok := node.(*ast.SwitchStmt)
			if !ok {
				return true
			}
			tag, ok := statement.Tag.(*ast.Ident)
			if !ok || tag.Name != "step" {
				return true
			}
			for _, child := range statement.Body.List {
				for _, expression := range child.(*ast.CaseClause).List {
					literal, ok := expression.(*ast.BasicLit)
					if !ok {
						t.Fatal("diagnostic site vocabulary is not literal")
					}
					value, err := strconv.Unquote(literal.Value)
					if err != nil {
						t.Fatal("diagnostic site literal invalid")
					}
					declared = append(declared, value)
				}
			}
			return false
		})
	}
	if !reflect.DeepEqual(declared, windowsTaskExpectedSites) {
		t.Fatal("R8 site vocabulary changed")
	}
}
