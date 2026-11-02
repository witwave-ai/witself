package localcell

import (
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
// exercise its actual pattern and byte bound portably; an unconditional t.Log
// or a broadened vocabulary fails this contract.
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
		{"rejected_first", "private diagnostic\nlocalcell: service_lease_unavailable\n", false},
		{"accepted_first", "localcell: service_lease_unavailable\nprivate diagnostic\n", true},
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
		// The helper is deliberately small: Helper, first-line extraction,
		// CRLF handling, guarded Log, then numeric exit status. Pin extraction
		// too so a later safe line cannot conceal a rejected first line.
		statements := function.Body.List
		if len(statements) != 5 {
			return "", 0, false
		}
		cut, ok := statements[1].(*ast.AssignStmt)
		if !ok || len(cut.Lhs) != 3 || len(cut.Rhs) != 1 || !foregroundDiagnosticIdent(cut.Lhs[0], "firstLine") {
			return "", 0, false
		}
		call, ok := cut.Rhs[0].(*ast.CallExpr)
		if !ok || !foregroundDiagnosticSelector(call.Fun, "strings", "Cut") || len(call.Args) != 2 || !foregroundDiagnosticString(call.Args[1], "\n") {
			return "", 0, false
		}
		conversion, ok := call.Args[0].(*ast.CallExpr)
		if !ok || !foregroundDiagnosticIdent(conversion.Fun, "string") || len(conversion.Args) != 1 || !foregroundDiagnosticIdent(conversion.Args[0], "captured") {
			return "", 0, false
		}
		trim, ok := statements[2].(*ast.AssignStmt)
		if !ok || len(trim.Lhs) != 1 || len(trim.Rhs) != 1 || !foregroundDiagnosticIdent(trim.Lhs[0], "firstLine") {
			return "", 0, false
		}
		call, ok = trim.Rhs[0].(*ast.CallExpr)
		if !ok || !foregroundDiagnosticSelector(call.Fun, "strings", "TrimSuffix") || len(call.Args) != 2 || !foregroundDiagnosticIdent(call.Args[0], "firstLine") || !foregroundDiagnosticString(call.Args[1], "\r") {
			return "", 0, false
		}
		guard, ok := statements[3].(*ast.IfStmt)
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
