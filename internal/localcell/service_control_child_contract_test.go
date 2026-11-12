package localcell_test

import (
	"bytes"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The child and host paths share this closed, value-free log boundary. The
// classifier line is logged on its own so CI's why retains the exact cause.
var serviceControlChildDiagnosticLine = regexp.MustCompile(`^(localcell: [a-z_]{1,48}|localcell: service_control_unavailable: transport:(dial|deadline|eof|response)|localcell: service_control_refused: (pipe|peer|endpoint):[a-z_]{1,24}|service_control accept=rejected step=(peer:pid|peer:identity|peer:sid|duplicate|conn)|service_control action=stop response=unflushed|service_control stop=handlers_unjoined)$`)

func TestServiceControlChildDiagnosticGuard(t *testing.T) {
	for _, line := range []string{
		"localcell: service_control_unavailable: transport:dial", "localcell: service_control_unavailable: transport:deadline",
		"localcell: service_control_unavailable: transport:eof", "localcell: service_control_unavailable: transport:response",
		"localcell: service_control_refused: pipe:io", "service_control accept=rejected step=peer:pid",
		"service_control accept=rejected step=peer:identity", "service_control accept=rejected step=peer:sid",
		"service_control accept=rejected step=duplicate", "service_control accept=rejected step=conn",
		"service_control action=stop response=unflushed",
		"service_control stop=handlers_unjoined",
		"localcell: " + strings.Repeat("a", 48),
		"localcell: service_control_refused: pipe:" + strings.Repeat("a", 24),
	} {
		if !serviceControlChildDiagnosticLine.MatchString(line) {
			t.Fatal("child diagnostic guard lost a closed control diagnostic")
		}
	}
	for _, line := range []string{
		"localcell: " + strings.Repeat("a", 49),
		"localcell: service_control_refused: pipe:" + strings.Repeat("a", 25),
		"localcell: service_control_unavailable: transport:unknown", "localcell: service_control_unavailable: transport:eof /private/path",
		"service_control accept=rejected step=peer:sid value=private", "service_control accept=rejected step=" + strings.Repeat("a", 64),
		"service_control action=stop response=written", "service_control action=stop response=unflushed password=private", "service_control accept=rejected step=private",
		"service_control stop=unknown", "service_control stop=handlers_unjoined password=private",
	} {
		if serviceControlChildDiagnosticLine.MatchString(line) {
			t.Fatal("child diagnostic guard accepted a private or unknown value")
		}
	}
}

func TestServiceControlChildProcessSourceContract(t *testing.T) {
	source, err := os.ReadFile("service_control_child_windows_test.go")
	if err != nil {
		t.Fatal("child source contract could not read the Windows client")
	}
	// Windows checkouts carry CRLF line endings; the multi-line mutation literals below assume LF.
	source = bytes.ReplaceAll(source, []byte("\r\n"), []byte("\n"))
	if err := serviceControlChildSourceContract(source); err != nil {
		switch err.Error() {
		case "child helper bypassed its guarded diagnostic output":
			t.Fatal("child helper bypassed its guarded diagnostic output")
		case "child client lost post-stop observation before host shutdown or receipt counting":
			t.Fatal("child client lost post-stop observation before host shutdown or receipt counting")
		default:
			t.Fatal("child client lost the minimal and full environment matrix or exact native constructor")
		}
	}
	const row = `{"minimal", func() []string { return nativeServiceChildEnvironment(os.LookupEnv) }},`
	mutant := strings.Replace(string(source), row, "", 1)
	if mutant == string(source) {
		t.Fatal("minimal environment row mutation did not apply")
	}
	if serviceControlChildSourceContract([]byte(mutant)) == nil {
		t.Fatal("child source contract survived removal of the minimal environment row")
	}
	mutant = strings.Replace(string(source), "Deadline: deadline.Add(-waitDelay)", "Deadline: deadline", 1)
	if mutant == string(source) || serviceControlChildSourceContract([]byte(mutant)) == nil {
		t.Fatal("child source contract survived removal of the deadline diagnostic interval")
	}
	mutant = strings.Replace(string(source), "serviceControlChildDiagnostic(err)", "err.Error()", 1)
	if mutant == string(source) || serviceControlChildSourceContract([]byte(mutant)) == nil {
		t.Fatal("child source contract survived unguarded helper error output")
	}
	const observe = `status, err := cell.Control(observeContext, "observe")`
	mutant = strings.Replace(string(source), observe, "", 1)
	if mutant == string(source) || serviceControlChildSourceContract([]byte(mutant)) == nil {
		t.Fatal("child source contract survived removal of post-stop observation")
	}
	const stop = "\t\t\tstopHost()\n\t\t\tselect {"
	mutant = strings.Replace(string(source), stop, "\t\t\tselect {", 1)
	if mutant == string(source) {
		t.Fatal("child source contract could not move foreground host shutdown")
	}
	mutant = strings.Replace(mutant, observe, "stopHost()\n\t\t\t"+observe, 1)
	if serviceControlChildSourceContract([]byte(mutant)) == nil {
		t.Fatal("child source contract survived host shutdown before post-stop observation")
	}
}

func serviceControlChildSourceContract(source []byte) error {
	invalid := errors.New("child client environment matrix is incomplete")
	file, err := parser.ParseFile(token.NewFileSet(), "child.go", source, 0)
	if err != nil {
		return invalid
	}
	var body, helper *ast.BlockStmt
	for _, declaration := range file.Decls {
		if function, ok := declaration.(*ast.FuncDecl); ok {
			switch function.Name.Name {
			case "TestServiceControlChildProcessClient":
				body = function.Body
			case "TestServiceControlChildProcessHelper":
				helper = function.Body
			}
		}
	}
	if body == nil {
		return invalid
	}
	selector := func(expression ast.Expr, object, field string) bool {
		value, ok := expression.(*ast.SelectorExpr)
		if !ok || value.Sel.Name != field {
			return false
		}
		name, ok := value.X.(*ast.Ident)
		return ok && name.Name == object
	}
	rowsValid, rowLoop, exactEnvironment, actionsValid, diagnosticInterval := false, false, false, false, false
	ast.Inspect(body, func(node ast.Node) bool {
		if field, ok := node.(*ast.KeyValueExpr); ok {
			if key, ok := field.Key.(*ast.Ident); ok && key.Name == "Deadline" {
				if call, ok := field.Value.(*ast.CallExpr); ok && selector(call.Fun, "deadline", "Add") && len(call.Args) == 1 {
					if margin, ok := call.Args[0].(*ast.UnaryExpr); ok && margin.Op == token.SUB {
						if name, ok := margin.X.(*ast.Ident); ok && name.Name == "waitDelay" {
							diagnosticInterval = true
						}
					}
				}
			}
		}
		if loop, ok := node.(*ast.RangeStmt); ok {
			if name, ok := loop.X.(*ast.Ident); ok && name.Name == "rows" {
				rowLoop = true
			}
			if values, ok := loop.X.(*ast.CompositeLit); ok && len(values.Elts) == 4 {
				var actions []string
				for _, value := range values.Elts {
					if literal, ok := value.(*ast.BasicLit); ok {
						action, _ := strconv.Unquote(literal.Value)
						actions = append(actions, action)
					}
				}
				actionsValid = actionsValid || strings.Join(actions, ",") == "health,status,observe,stop"
			}
		}
		assignment, ok := node.(*ast.AssignStmt)
		if !ok || len(assignment.Lhs) != 1 || len(assignment.Rhs) != 1 {
			return true
		}
		if selector(assignment.Lhs[0], "command", "Env") {
			if appendCall, ok := assignment.Rhs[0].(*ast.CallExpr); ok && len(appendCall.Args) == 2 {
				if name, ok := appendCall.Fun.(*ast.Ident); ok && name.Name == "append" {
					if environment, ok := appendCall.Args[0].(*ast.CallExpr); ok && selector(environment.Fun, "row", "environment") {
						exactEnvironment = true
					}
				}
			}
		}
		name, ok := assignment.Lhs[0].(*ast.Ident)
		if !ok || name.Name != "rows" {
			return true
		}
		rows, ok := assignment.Rhs[0].(*ast.CompositeLit)
		if !ok || len(rows.Elts) != 2 {
			return true
		}
		for index, expected := range []string{"minimal", "full"} {
			row, ok := rows.Elts[index].(*ast.CompositeLit)
			if !ok || len(row.Elts) != 2 {
				return true
			}
			literal, ok := row.Elts[0].(*ast.BasicLit)
			if !ok || literal.Value != strconv.Quote(expected) {
				return true
			}
			if expected == "full" {
				if !selector(row.Elts[1], "os", "Environ") {
					return true
				}
				continue
			}
			factory, ok := row.Elts[1].(*ast.FuncLit)
			if !ok || len(factory.Body.List) != 1 {
				return true
			}
			result, ok := factory.Body.List[0].(*ast.ReturnStmt)
			if !ok || len(result.Results) != 1 {
				return true
			}
			call, ok := result.Results[0].(*ast.CallExpr)
			if !ok || len(call.Args) != 1 || !selector(call.Args[0], "os", "LookupEnv") {
				return true
			}
			function, ok := call.Fun.(*ast.Ident)
			if !ok || function.Name != "nativeServiceChildEnvironment" {
				return true
			}
		}
		rowsValid = true
		return true
	})
	if !rowsValid || !rowLoop || !exactEnvironment || !actionsValid || !diagnosticInterval {
		return invalid
	}
	if !serviceControlChildHelperOutputContract(helper) {
		return errors.New("child helper bypassed its guarded diagnostic output")
	}
	if !serviceControlChildStopObservationContract(body) {
		return errors.New("child client lost post-stop observation before host shutdown or receipt counting")
	}
	return nil
}

func serviceControlChildSelector(expression ast.Expr, object, field string) bool {
	selector, ok := expression.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != field {
		return false
	}
	name, ok := selector.X.(*ast.Ident)
	return ok && name.Name == object
}

// Account for every stderr reference, so an alternate writer or an alias cannot
// bypass the three permitted output sites. Error selectors are forbidden even
// when the result would be assigned to an intermediate variable.
func serviceControlChildHelperOutputContract(body *ast.BlockStmt) bool {
	if body == nil {
		return false
	}
	valid := true
	stderrReferences := 0
	outputs := map[string]int{}
	ast.Inspect(body, func(node ast.Node) bool {
		if selector, ok := node.(*ast.SelectorExpr); ok {
			if selector.Sel.Name == "Error" {
				valid = false
			}
			if serviceControlChildSelector(selector, "os", "Stderr") {
				stderrReferences++
			}
		}
		call, ok := node.(*ast.CallExpr)
		if !ok || !serviceControlChildSelector(call.Fun, "fmt", "Fprintln") || len(call.Args) == 0 ||
			!serviceControlChildSelector(call.Args[0], "os", "Stderr") {
			return true
		}
		if len(call.Args) != 2 || call.Ellipsis.IsValid() {
			valid = false
			return true
		}
		switch argument := call.Args[1].(type) {
		case *ast.BasicLit:
			line, err := strconv.Unquote(argument.Value)
			if err != nil || (line != "localcell: service_control_child_request_invalid" && line != "localcell: service_control_child_status_invalid") {
				valid = false
			} else {
				outputs[line]++
			}
		case *ast.CallExpr:
			name, ok := argument.Fun.(*ast.Ident)
			if !ok || name.Name != "serviceControlChildDiagnostic" || len(argument.Args) != 1 || argument.Ellipsis.IsValid() {
				valid = false
				return true
			}
			input, ok := argument.Args[0].(*ast.Ident)
			if !ok || input.Name != "err" {
				valid = false
			} else {
				outputs["guarded"]++
			}
		default:
			valid = false
		}
		return true
	})
	return valid && stderrReferences == 3 && outputs["guarded"] == 1 &&
		outputs["localcell: service_control_child_request_invalid"] == 1 &&
		outputs["localcell: service_control_child_status_invalid"] == 1
}

// The ordered operations must be direct statements in the same row body. A
// cleanup callback or the earlier readiness probe cannot satisfy this contract.
func serviceControlChildStopObservationContract(body *ast.BlockStmt) bool {
	valid := false
	ast.Inspect(body, func(node ast.Node) bool {
		block, ok := node.(*ast.BlockStmt)
		if !ok {
			return true
		}
		actions, observe, stop, count := -1, -1, -1, -1
		for index, statement := range block.List {
			if loop, ok := statement.(*ast.RangeStmt); ok {
				if values, ok := loop.X.(*ast.CompositeLit); ok && len(values.Elts) == 4 {
					var names []string
					for _, value := range values.Elts {
						if literal, ok := value.(*ast.BasicLit); ok {
							name, _ := strconv.Unquote(literal.Value)
							names = append(names, name)
						}
					}
					if strings.Join(names, ",") == "health,status,observe,stop" {
						actions = index
					}
				}
			}
			var expressions []ast.Expr
			switch statement := statement.(type) {
			case *ast.AssignStmt:
				expressions = statement.Rhs
			case *ast.ExprStmt:
				expressions = []ast.Expr{statement.X}
			}
			for _, expression := range expressions {
				call, ok := expression.(*ast.CallExpr)
				if !ok {
					continue
				}
				if serviceControlChildSelector(call.Fun, "cell", "Control") && len(call.Args) == 2 {
					if literal, ok := call.Args[1].(*ast.BasicLit); ok && literal.Value == `"observe"` && observe < 0 {
						observe = index
					}
				}
				if name, ok := call.Fun.(*ast.Ident); ok {
					switch name.Name {
					case "stopHost":
						if stop < 0 {
							stop = index
						}
					case "nativeServiceShutdownReceiptCount":
						count = index
					}
				}
			}
		}
		valid = valid || (actions >= 0 && observe > actions && stop > observe && count > stop)
		return true
	})
	return valid
}
