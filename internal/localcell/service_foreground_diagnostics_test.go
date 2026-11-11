package localcell

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The Windows-only logger cannot execute on Unix. Inspect its actual guard and
// exercise its actual per-line pattern and byte bound portably; an unconditional
// t.Log, a missing line count, or a first-line-only scan fails this contract.
func TestWindowsForegroundExitDiagnosticGuard(t *testing.T) {
	source, err := os.ReadFile(windowsSyntheticHarnessSource)
	if err != nil {
		t.Fatal("cannot read Windows foreground diagnostic source")
	}
	pattern, maxLength, ok := windowsForegroundDiagnosticGuard(source)
	if !ok {
		t.Fatal("Windows foreground exit diagnostic lost its vocabulary guard")
	}
	guard, err := regexp.Compile(pattern)
	if err != nil {
		t.Fatal("Windows foreground exit diagnostic vocabulary is invalid")
	}
	for _, test := range []struct {
		name, output string
		allowed      bool
	}{
		{"lifecycle", "localcell: service_lease_unavailable\n", true},
		{"crlf", "localcell: service_host_already_active\r\n", true},
		{"receipt", "service_shutdown phase=idle operation=none journal_pending=false journal_read_ok=true\n", true},
		{"health_receipt", "service_shutdown phase=control operation=health site=health_database journal_pending=false journal_read_ok=true\n", true},
		{"private_health_site", "service_shutdown phase=control operation=health site=/private/path\n", false},
		{"service_error", "service_manager_unavailable\n", true},
		{"peer_pid", "service_control accept=rejected step=peer:pid\n", true},
		{"peer_identity", "service_control accept=rejected step=peer:identity\n", true},
		{"peer_sid", "service_control accept=rejected step=peer:sid\n", true},
		{"duplicate", "service_control accept=rejected step=duplicate\n", true},
		{"conn", "service_control accept=rejected step=conn\n", true},
		{"unflushed_stop", "service_control action=stop response=unflushed\n", true},
		{"native_command", "native service host site=supervisor_command\n", true},
		{"native_cancelled", "native service host site=control_cancelled\n", true},
		{"lease_publish_sharing", "native service host lease_publish class=sharing_violation attempts=12 elapsed_ms=2175\n", true},
		{"lease_publish_access", "native service host lease_publish class=access_denied attempts=2 elapsed_ms=25\n", true},
		{"lease_publish_validation", "native service host lease_publish class=validation attempts=1 elapsed_ms=0\n", true},
		{"lease_publish_other", "native service host lease_publish class=other attempts=1 elapsed_ms=0\n", true},
		{"lease_publish_unknown_class", "native service host lease_publish class=none attempts=1 elapsed_ms=0\n", false},
		{"private_native_site", "native service host site=/synthetic/home\n", false},
		{"unknown_native_site", "native service host site=not_a_site\n", false},
		{"private_accept_step", "service_control accept=rejected step=/synthetic/home\n", false},
		{"unknown_accept_step", "service_control accept=rejected step=peer:other\n", false},
		{"localcell_token_at_bound", "localcell: " + strings.Repeat("a", 48), true},
		{"receipt_value_at_bound", "service_shutdown phase=" + strings.Repeat("a", 24), true},
		{"line_at_bound", "service_" + strings.Repeat("a", 152), true},
		{"password_key", "service_shutdown password=private", false},
		{"hex_value", "service_shutdown phase=" + strings.Repeat("a", 64), false},
		{"token_key", "service_shutdown token=private", false},
		{"overlong_value", "service_shutdown phase=" + strings.Repeat("a", 25), false},
		{"overlong_localcell_token", "localcell: " + strings.Repeat("a", 49), false},
		{"overlong_line", "service_" + strings.Repeat("a", 153), false},
		{"empty", "", false},
		{"private_line", "private diagnostic /synthetic/home value=not-for-output\n", false},
		{"path_suffix", "localcell: service_lease_unavailable /synthetic/home\n", false},
		{"stage_suffix", "localcell: service_control_unavailable: pipe:first_instance\n", false},
		{"leading_space", " localcell: service_lease_unavailable\n", false},
		{"embedded_cr", "localcell: service_lease\runavailable\n", false},
		{"uppercase", "service_shutdown phase=PRIVATE\n", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			firstLine, _, _ := strings.Cut(test.output, "\n")
			firstLine = strings.TrimSuffix(firstLine, "\r")
			if (len(firstLine) <= maxLength && guard.MatchString(firstLine)) != test.allowed {
				t.Fatal("Windows foreground diagnostic accepted private output or lost a lifecycle line")
			}
		})
	}
}

// The diagnostic allowlist is independent of the guard grammar. Compare it with
// the production receipt emitter so adding or removing an emitted key requires
// a deliberate diagnostic vocabulary update.
func TestWindowsForegroundDiagnosticReceiptKeys(t *testing.T) {
	harness, err := parser.ParseFile(token.NewFileSet(), windowsSyntheticHarnessSource, nil, 0)
	if err != nil {
		t.Fatal("cannot parse Windows foreground diagnostic source")
	}
	allowed, ok := foregroundDiagnosticConstant(harness, "windowsForegroundReceiptKeys")
	if !ok {
		t.Fatal("Windows foreground receipt key allowlist is not a string constant")
	}
	emitter, err := parser.ParseFile(token.NewFileSet(), "service_supervisor.go", nil, 0)
	if err != nil {
		t.Fatal("cannot parse production supervisor receipt emitter")
	}
	var emitted []string
	receipts, valid := 0, true
	formats := map[string]int{
		"service_shutdown phase=%s operation=%s journal_pending=%t journal_read_ok=%t\n":             0,
		"service_shutdown phase=%s operation=health site=%s journal_pending=%t journal_read_ok=%t\n": 0,
	}
	ast.Inspect(emitter, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || !foregroundDiagnosticSelector(call.Fun, "fmt", "Fprintf") || len(call.Args) < 2 || !foregroundDiagnosticSelector(call.Args[0], "os", "Stderr") {
			return true
		}
		format, ok := foregroundDiagnosticPattern(emitter, call.Args[1])
		if !ok || !strings.HasPrefix(format, "service_") {
			return true
		}
		receipts++
		if _, reviewed := formats[format]; !reviewed || len(call.Args) != 6 {
			valid = false
		}
		formats[format]++
		fields := strings.Fields(format)
		for _, field := range fields[1:] {
			key, value, found := strings.Cut(field, "=")
			if !found || key == "" || (value != "%s" && value != "%t" && (key != "operation" || value != "health")) {
				valid = false
			}
			emitted = append(emitted, key)
		}
		return true
	})
	keys := strings.Split(allowed, "|")
	slices.Sort(keys)
	slices.Sort(emitted)
	for _, count := range formats {
		if count != 1 {
			valid = false
		}
	}
	if !valid || receipts != 2 || len(slices.Compact(slices.Clone(keys))) != len(keys) || !slices.Equal(keys, slices.Compact(emitted)) {
		t.Fatal("Windows foreground receipt key allowlist differs from production emitter keys")
	}
}

func foregroundDiagnosticConstant(file *ast.File, name string) (string, bool) {
	for _, declaration := range file.Decls {
		group, ok := declaration.(*ast.GenDecl)
		if !ok || group.Tok != token.CONST {
			continue
		}
		for _, spec := range group.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok || len(value.Names) != 1 || value.Names[0].Name != name || len(value.Values) != 1 {
				continue
			}
			literal, ok := value.Values[0].(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return "", false
			}
			text, err := strconv.Unquote(literal.Value)
			return text, err == nil
		}
	}
	return "", false
}

func foregroundDiagnosticPattern(file *ast.File, expression ast.Expr) (string, bool) {
	switch value := expression.(type) {
	case *ast.BasicLit:
		if value.Kind != token.STRING {
			return "", false
		}
		text, err := strconv.Unquote(value.Value)
		return text, err == nil
	case *ast.Ident:
		return foregroundDiagnosticConstant(file, value.Name)
	case *ast.BinaryExpr:
		if value.Op != token.ADD {
			return "", false
		}
		left, leftOK := foregroundDiagnosticPattern(file, value.X)
		right, rightOK := foregroundDiagnosticPattern(file, value.Y)
		return left + right, leftOK && rightOK
	default:
		return "", false
	}
}

func windowsForegroundDiagnosticGuard(source []byte) (string, int, bool) {
	file, err := parser.ParseFile(token.NewFileSet(), "windows-harness", source, 0)
	if err != nil {
		return "", 0, false
	}
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != "windowsLogForegroundExit" || function.Body == nil {
			continue
		}
		// Pin the whole-output range and numeric count before inspecting the
		// per-line guard. Unknown lines must not stop later safe diagnostics.
		statements := function.Body.List
		if len(statements) != 5 {
			return "", 0, false
		}
		classification, ok := statements[1].(*ast.ExprStmt)
		if !ok {
			return "", 0, false
		}
		classLog, ok := classification.X.(*ast.CallExpr)
		if !ok || !foregroundDiagnosticSelector(classLog.Fun, "t", "Logf") || len(classLog.Args) != 2 || !foregroundDiagnosticString(classLog.Args[0], "foreground_exit class=%s") {
			return "", 0, false
		}
		classCall, ok := classLog.Args[1].(*ast.CallExpr)
		if !ok || !foregroundDiagnosticIdent(classCall.Fun, "foregroundFailureClass") || len(classCall.Args) != 1 || !foregroundDiagnosticIdent(classCall.Args[0], "captured") {
			return "", 0, false
		}
		count, ok := statements[2].(*ast.ExprStmt)
		if !ok {
			return "", 0, false
		}
		countCall, ok := count.X.(*ast.CallExpr)
		if !ok || !foregroundDiagnosticSelector(countCall.Fun, "t", "Logf") || len(countCall.Args) != 2 || !foregroundDiagnosticString(countCall.Args[0], "foreground_output_lines=%d") {
			return "", 0, false
		}
		countLength, ok := countCall.Args[1].(*ast.CallExpr)
		if !ok || !foregroundDiagnosticIdent(countLength.Fun, "len") || len(countLength.Args) != 1 || !foregroundDiagnosticLines(countLength.Args[0]) {
			return "", 0, false
		}
		loop, ok := statements[3].(*ast.RangeStmt)
		if !ok || !foregroundDiagnosticIdent(loop.Key, "_") || !foregroundDiagnosticIdent(loop.Value, "firstLine") || !foregroundDiagnosticLines(loop.X) || len(loop.Body.List) != 2 {
			return "", 0, false
		}
		trim, ok := loop.Body.List[0].(*ast.AssignStmt)
		if !ok || len(trim.Lhs) != 1 || len(trim.Rhs) != 1 || !foregroundDiagnosticIdent(trim.Lhs[0], "firstLine") {
			return "", 0, false
		}
		call, ok := trim.Rhs[0].(*ast.CallExpr)
		if !ok || !foregroundDiagnosticSelector(call.Fun, "strings", "TrimSuffix") || len(call.Args) != 2 || !foregroundDiagnosticIdent(call.Args[0], "firstLine") || !foregroundDiagnosticString(call.Args[1], "\r") {
			return "", 0, false
		}
		guard, ok := loop.Body.List[1].(*ast.IfStmt)
		if !ok || guard.Init != nil || guard.Else != nil || len(guard.Body.List) != 1 {
			return "", 0, false
		}
		condition, ok := guard.Cond.(*ast.BinaryExpr)
		if !ok || condition.Op != token.LAND {
			return "", 0, false
		}
		bound, ok := condition.X.(*ast.BinaryExpr)
		if !ok || bound.Op != token.LEQ {
			return "", 0, false
		}
		length, ok := bound.X.(*ast.CallExpr)
		if !ok || !foregroundDiagnosticIdent(length.Fun, "len") || len(length.Args) != 1 || !foregroundDiagnosticIdent(length.Args[0], "firstLine") {
			return "", 0, false
		}
		limit, ok := bound.Y.(*ast.BasicLit)
		if !ok || limit.Kind != token.INT {
			return "", 0, false
		}
		maxLength, err := strconv.Atoi(limit.Value)
		if err != nil || maxLength <= 0 {
			return "", 0, false
		}
		match, ok := condition.Y.(*ast.CallExpr)
		if !ok || len(match.Args) != 1 || !foregroundDiagnosticIdent(match.Args[0], "firstLine") {
			return "", 0, false
		}
		selector, ok := match.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "MatchString" {
			return "", 0, false
		}
		compile, ok := selector.X.(*ast.CallExpr)
		if !ok || !foregroundDiagnosticSelector(compile.Fun, "regexp", "MustCompile") || len(compile.Args) != 1 {
			return "", 0, false
		}
		pattern, ok := foregroundDiagnosticPattern(file, compile.Args[0])
		if !ok {
			return "", 0, false
		}
		logs, guarded := 0, false
		ast.Inspect(function.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if ok && foregroundDiagnosticSelector(call.Fun, "t", "Log") {
				logs++
				expression, ok := guard.Body.List[0].(*ast.ExprStmt)
				guarded = ok && expression.X == call && len(call.Args) == 1 && foregroundDiagnosticIdent(call.Args[0], "firstLine")
			}
			return true
		})
		return pattern, maxLength, logs == 1 && guarded
	}
	return "", 0, false
}

func foregroundDiagnosticLines(expression ast.Expr) bool {
	call, ok := expression.(*ast.CallExpr)
	return ok && foregroundDiagnosticIdent(call.Fun, "foregroundOutputLines") && len(call.Args) == 1 && foregroundDiagnosticIdent(call.Args[0], "captured")
}

func foregroundDiagnosticIdent(expression ast.Expr, name string) bool {
	identifier, ok := expression.(*ast.Ident)
	return ok && identifier.Name == name
}

func foregroundDiagnosticSelector(expression ast.Expr, receiver, method string) bool {
	selector, ok := expression.(*ast.SelectorExpr)
	return ok && foregroundDiagnosticIdent(selector.X, receiver) && selector.Sel.Name == method
}

func foregroundDiagnosticString(expression ast.Expr, want string) bool {
	literal, ok := expression.(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return false
	}
	value, err := strconv.Unquote(literal.Value)
	return err == nil && value == want
}

// Follow captured bytes through local aliases, rather than pinning a particular
// logging statement or buffer name. The only raw-byte sink allowed is the exact
// per-line call already checked by the vocabulary/length contract above.
func TestWindowsForegroundCapturedOutputContract(t *testing.T) {
	source, err := os.ReadFile(windowsSyntheticHarnessSource)
	if err != nil {
		t.Fatal("cannot read Windows foreground capture source")
	}
	file, err := parser.ParseFile(token.NewFileSet(), windowsSyntheticHarnessSource, source, 0)
	if err != nil {
		t.Fatal("cannot parse Windows foreground capture source")
	}
	var guarded *ast.CallExpr
	if _, _, ok := windowsForegroundDiagnosticGuard(source); ok {
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Name.Name != "windowsLogForegroundExit" {
				continue
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				if call, ok := node.(*ast.CallExpr); ok && foregroundDiagnosticSelector(call.Fun, "t", "Log") {
					guarded = call
				}
				return true
			})
		}
	}
	// The validator returns only fixed rejection messages. Pin that property
	// before allowing t.Fatal(failure) to consume its result without raw taint.
	validator, err := parser.ParseFile(token.NewFileSet(), "../tools/localcellharness/service_foreground_output_test.go", nil, 0)
	if err != nil {
		t.Fatal("cannot parse foreground output validator")
	}
	fixedMessages := false
	for _, declaration := range validator.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != "foregroundShutdownFailure" {
			continue
		}
		fixedMessages = true
		returns := 0
		ast.Inspect(function.Body, func(node ast.Node) bool {
			if statement, ok := node.(*ast.ReturnStmt); ok {
				returns++
				if len(statement.Results) != 1 {
					fixedMessages = false
				} else if literal, ok := statement.Results[0].(*ast.BasicLit); !ok || literal.Kind != token.STRING {
					fixedMessages = false
				}
			}
			return true
		})
		fixedMessages = fixedMessages && returns > 0
	}
	if !fixedMessages {
		t.Fatal("foreground shutdown validator no longer returns only fixed messages")
	}
	classifier, err := parser.ParseFile(token.NewFileSet(), "../tools/localcellharness/service_foreground_classify_test.go", nil, 0)
	if err != nil {
		t.Fatal("cannot parse foreground failure classifier")
	}
	// Each classifier returns only fixed value-free classes. The install
	// classifier's one empty literal is its refusal-preserved result.
	for name, pattern := range map[string]string{"foregroundFailureClass": `^[a-z_]{1,48}$`, "classifyServiceInstallFailure": `^[a-z_]{0,48}$`} {
		fixedClasses := false
		for _, declaration := range classifier.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Name.Name != name {
				continue
			}
			fixedClasses = true
			returns := 0
			ast.Inspect(function.Body, func(node ast.Node) bool {
				if statement, ok := node.(*ast.ReturnStmt); ok {
					returns++
					if len(statement.Results) != 1 {
						fixedClasses = false
					} else if literal, ok := statement.Results[0].(*ast.BasicLit); !ok || literal.Kind != token.STRING {
						fixedClasses = false
					} else if value, err := strconv.Unquote(literal.Value); err != nil || !regexp.MustCompile(pattern).MatchString(value) {
						fixedClasses = false
					}
				}
				return true
			})
			fixedClasses = fixedClasses && returns > 0
		}
		if !fixedClasses {
			t.Fatal("foreground failure classifier no longer returns only fixed value-free classes")
		}
	}
	for _, violation := range windowsForegroundCapturedOutputViolations(file, guarded) {
		t.Error(violation)
	}
}

func windowsForegroundCapturedOutputViolations(file *ast.File, guarded *ast.CallExpr) []string {
	var violations []string
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Body == nil {
			continue
		}
		tainted := map[string]bool{}
		mark := func(expression ast.Expr) bool {
			for {
				index, ok := expression.(*ast.IndexExpr)
				if !ok {
					break
				}
				expression = index.X
			}
			identifier, ok := expression.(*ast.Ident)
			if !ok || identifier.Name == "_" || tainted[identifier.Name] {
				return false
			}
			tainted[identifier.Name] = true
			return true
		}
		for _, field := range function.Type.Params.List {
			if array, ok := field.Type.(*ast.ArrayType); ok && foregroundDiagnosticIdent(array.Elt, "byte") {
				for _, name := range field.Names {
					mark(name)
				}
			}
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			if declaration, ok := node.(*ast.ValueSpec); ok && foregroundDiagnosticSelector(declaration.Type, "bytes", "Buffer") {
				for _, name := range declaration.Names {
					mark(name)
				}
			}
			// Also find captures whose declaration uses new or a composite
			// literal: the stdout/stderr binding identifies the actual buffer.
			if assignment, ok := node.(*ast.AssignStmt); ok && len(assignment.Lhs) == len(assignment.Rhs) {
				for index, left := range assignment.Lhs {
					if selector, ok := left.(*ast.SelectorExpr); ok && (selector.Sel.Name == "Stdout" || selector.Sel.Name == "Stderr") {
						right := assignment.Rhs[index]
						if address, ok := right.(*ast.UnaryExpr); ok && address.Op == token.AND {
							right = address.X
						}
						mark(right)
					}
				}
			}
			return true
		})
		// Monotone, function-local flow also covers aliases assigned inside
		// closures and loops. Keeping earlier taint is intentionally conservative.
		for changed := true; changed; {
			changed = false
			assign := func(left []ast.Expr, right []ast.Expr) {
				for index, expression := range left {
					values := right
					if len(left) == len(right) {
						values = right[index : index+1]
					}
					for _, value := range values {
						if foregroundCapturedExpression(value, tainted) && mark(expression) {
							changed = true
						}
					}
				}
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				switch value := node.(type) {
				case *ast.AssignStmt:
					assign(value.Lhs, value.Rhs)
				case *ast.ValueSpec:
					var left []ast.Expr
					for _, name := range value.Names {
						left = append(left, name)
					}
					assign(left, value.Values)
				case *ast.RangeStmt:
					if foregroundCapturedExpression(value.X, tainted) && mark(value.Value) {
						changed = true
					}
				}
				return true
			})
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || call == guarded {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			testingSink := foregroundDiagnosticIdent(selector.X, "t") && slices.Contains([]string{"Log", "Logf", "Error", "Errorf", "Fatal", "Fatalf"}, selector.Sel.Name)
			formatSink := foregroundDiagnosticIdent(selector.X, "fmt") && strings.Contains(strings.ToLower(selector.Sel.Name), "print")
			if !testingSink && !formatSink {
				return true
			}
			for _, argument := range call.Args {
				if foregroundCapturedExpression(argument, tainted) {
					receiver := selector.X.(*ast.Ident).Name
					violations = append(violations, fmt.Sprintf("%s: unguarded captured-output call %s.%s", function.Name.Name, receiver, selector.Sel.Name))
					break
				}
			}
			return true
		})
	}
	return violations
}

func foregroundCapturedExpression(expression ast.Expr, tainted map[string]bool) bool {
	found := false
	ast.Inspect(expression, func(node ast.Node) bool {
		if call, ok := node.(*ast.CallExpr); ok {
			if identifier, ok := call.Fun.(*ast.Ident); ok && identifier.Obj == nil && slices.Contains([]string{"len", "cap", "foregroundShutdownFailure", "foregroundFailureClass", "classifyServiceInstallFailure"}, identifier.Name) {
				return false // Numeric counts or independently pinned fixed messages.
			}
			if selector, ok := call.Fun.(*ast.SelectorExpr); ok && (selector.Sel.Name == "CombinedOutput" || selector.Sel.Name == "Output") {
				found = true
			}
		}
		if identifier, ok := node.(*ast.Ident); ok && tainted[identifier.Name] {
			found = true
		}
		return !found
	})
	return found
}

func TestWindowsForegroundCapturedOutputFlow(t *testing.T) {
	for _, test := range []struct {
		name, body string
		rejected   bool
	}{
		{"buffer", `var captured bytes.Buffer; command.Stdout = &captured; t.Logf("%s", captured)`, true},
		{"renamed_alias", `var output bytes.Buffer; command.Stderr = &output; first := output.Bytes(); var second = first; t.Error(string(second))`, true},
		{"assigned_index", `lines := make([]string, 1); lines[0] = string(captured); t.Log(lines[0])`, true},
		{"split_index", `lines := strings.Split(string(captured), "\n"); t.Logf("%s", string(lines[0]))`, true},
		{"range_line", `for _, line := range strings.Split(string(captured), "\n") { t.Fatalf("%s", line) }`, true},
		{"closure", `alias := captured; f := func() { t.Fatal(alias) }`, true},
		{"multi_assignment", `alias, other := captured, 0; t.Errorf("%s", alias)`, true},
		{"combined_output", `output, err := command.CombinedOutput(); t.Log(output)`, true},
		{"fmt_fprintf", `fmt.Fprintf(writer, "%s", captured)`, true},
		{"fmt_sprintf", `message := fmt.Sprintf("%s", captured)`, true},
		{"shadowed_count", `len := func(raw []byte) []byte { return raw }; t.Log(len(captured))`, true},
		{"shadowed_validator", `foregroundShutdownFailure := func(raw []byte) string { return string(raw) }; t.Log(foregroundShutdownFailure(captured))`, true},
		{"shadowed_classifier", `foregroundFailureClass := func(raw []byte) string { return string(raw) }; t.Log(foregroundFailureClass(captured))`, true},
		{"shadowed_install_classifier", `classifyServiceInstallFailure := func(raw []byte) string { return string(raw) }; t.Log(classifyServiceInstallFailure(captured))`, true},
		{"unclassified_install", `output, err := command.CombinedOutput(); class := string(output); t.Logf("service_install class=%s", class)`, true},
		{"count", `t.Logf("%d", len(captured))`, false},
		{"count_alias", `count := len(strings.Split(string(captured), "\n")); t.Log(count)`, false},
		{"range_index", `for index, line := range foregroundOutputLines(captured) { t.Log(index) }`, false},
		{"fixed_failure", `failure := foregroundShutdownFailure(captured); t.Fatal(failure)`, false},
		{"fixed_class", `t.Logf("foreground_exit class=%s", foregroundFailureClass(captured))`, false},
		{"fixed_install_class", `output, err := command.CombinedOutput(); class := classifyServiceInstallFailure(nil, nil, err, strings.Contains(string(output), "cause")); t.Logf("service_install class=%s", class)`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			file, err := parser.ParseFile(token.NewFileSet(), "flow", "package main; func capture(captured []byte) {"+test.body+"}", 0)
			if err != nil {
				t.Fatal("invalid captured-output flow fixture")
			}
			violations := windowsForegroundCapturedOutputViolations(file, nil)
			if (len(violations) > 0) != test.rejected {
				t.Fatal("captured-output contract lost identifier flow or numeric-count allowance")
			}
		})
	}
}
