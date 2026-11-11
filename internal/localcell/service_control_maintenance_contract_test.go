package localcell

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strings"
	"testing"
)

const maintenanceControlQueryCondition = "held && s.queryStep"
const maintenanceControlServerCondition = `(action == "start" || action == "repair") && serviceMaintenanceOnly(err)`

func maintenanceControlFunction(file *ast.File, name string) *ast.FuncDecl {
	for _, declaration := range file.Decls {
		if fn, ok := declaration.(*ast.FuncDecl); ok && fn.Name.Name == name {
			return fn
		}
	}
	return nil
}

func maintenanceControlIfs(node ast.Node, condition string) []*ast.IfStmt {
	var matches []*ast.IfStmt
	ast.Inspect(node, func(node ast.Node) bool {
		if conditional, ok := node.(*ast.IfStmt); ok && admissionNodeText(conditional.Cond) == condition {
			matches = append(matches, conditional)
		}
		return true
	})
	return matches
}

func maintenanceControlCalls(node ast.Node, name string) int {
	count := 0
	ast.Inspect(node, func(node ast.Node) bool {
		if call, ok := node.(*ast.CallExpr); ok && admissionNodeText(call.Fun) == name {
			count++
		}
		return true
	})
	return count
}

// This contract checks independent AST properties instead of reproducing the
// production function body. Every property has a rejecting control below.
func maintenanceControlSourceContract(sources map[string]string) []string {
	var failures []string
	fail := func(name string) { failures = append(failures, name) }
	files := make(map[string]*ast.File)
	assignments, reads := 0, 0
	for filename, source := range sources {
		if !strings.HasSuffix(filename, ".go") || strings.HasSuffix(filename, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), filename, strings.ReplaceAll(source, "\r\n", "\n"), 0)
		if err != nil {
			fail("parse")
			continue
		}
		files[filename] = file
		writes := make(map[*ast.SelectorExpr]bool)
		ast.Inspect(file, func(node ast.Node) bool {
			if assignment, ok := node.(*ast.AssignStmt); ok {
				for _, lhs := range assignment.Lhs {
					if selector, ok := lhs.(*ast.SelectorExpr); ok && selector.Sel.Name == "queryStep" {
						assignments++
						writes[selector] = true
					}
				}
			}
			return true
		})
		ast.Inspect(file, func(node ast.Node) bool {
			if selector, ok := node.(*ast.SelectorExpr); ok && selector.Sel.Name == "queryStep" && !writes[selector] {
				reads++
			}
			return true
		})
	}
	if assignments != 2 {
		fail("query_assignments")
	}
	if reads != 1 {
		fail("query_reads")
	}
	supervisorFile, controlFile := files["service_supervisor.go"], files["service_control.go"]
	if supervisorFile == nil || controlFile == nil {
		fail("owners")
		return failures
	}
	step := maintenanceControlFunction(supervisorFile, "step")
	command := maintenanceControlFunction(supervisorFile, "command")
	control := maintenanceControlFunction(controlFile, "Control")
	server := maintenanceControlFunction(controlFile, "serveServiceControlForPlatform")
	helper := maintenanceControlFunction(controlFile, "serviceControlMaintenanceRefusal")
	if step == nil || command == nil || control == nil || server == nil || helper == nil {
		fail("owners")
		return failures
	}
	queries := maintenanceControlIfs(step.Body, maintenanceControlQueryCondition)
	if len(queries) != 1 {
		fail("step_query_count")
	} else {
		query := queries[0]
		index, heldIndex, blockedIndex := -1, -1, -1
		for i, statement := range step.Body.List {
			if statement == query {
				index = i
			}
			if conditional, ok := statement.(*ast.IfStmt); ok {
				switch admissionNodeText(conditional.Cond) {
				case "held":
					if heldIndex == -1 {
						heldIndex = i
					}
				case "s.blockedServiceShutdown(ctx)":
					blockedIndex = i
				}
			}
		}
		if index != heldIndex+1 || blockedIndex != index+1 || heldIndex < 0 {
			fail("step_query_position")
		}
		ast.Inspect(query.Body, func(node ast.Node) bool {
			if call, ok := node.(*ast.CallExpr); ok && admissionNodeText(call.Fun) != "len" {
				fail("step_query_calls")
			}
			return true
		})
		if len(query.Body.List) == 0 || admissionNodeText(query.Body.List[len(query.Body.List)-1]) != "return nil" {
			fail("step_query_return")
		}
	}
	queryCases := 0
	ast.Inspect(command.Body, func(node ast.Node) bool {
		clause, ok := node.(*ast.CaseClause)
		if !ok || len(clause.List) != 2 || admissionNodeText(clause.List[0]) != `"status"` || admissionNodeText(clause.List[1]) != `"health"` {
			return true
		}
		queryCases++
		if len(clause.Body) != 3 || admissionNodeText(clause.Body[0]) != "s.queryStep = true" || admissionNodeText(clause.Body[1]) != "err = s.step(ctx)" || admissionNodeText(clause.Body[2]) != "s.queryStep = false" {
			fail("query_command_bracket")
		}
		return true
	})
	if queryCases != 1 {
		fail("query_command_bracket")
	}
	var refuse *ast.FuncLit
	ast.Inspect(server.Body, func(node ast.Node) bool {
		assignment, ok := node.(*ast.AssignStmt)
		if ok && len(assignment.Lhs) == 1 && admissionNodeText(assignment.Lhs[0]) == "refuse" && len(assignment.Rhs) == 1 {
			refuse, _ = assignment.Rhs[0].(*ast.FuncLit)
		}
		return true
	})
	if refuse == nil || len(refuse.Body.List) != 1 {
		fail("server_refuse_order")
	} else {
		conditions := []string{}
		var branches []*ast.IfStmt
		var terminal *ast.BlockStmt
		for node := refuse.Body.List[0]; node != nil; {
			conditional, ok := node.(*ast.IfStmt)
			if !ok {
				terminal, _ = node.(*ast.BlockStmt)
				break
			}
			conditions = append(conditions, admissionNodeText(conditional.Cond))
			branches = append(branches, conditional)
			node = conditional.Else
		}
		want := []string{
			"errors.Is(err, errServiceControlDeferred) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)",
			`action == "health" && errors.Is(err, errServiceUnhealthy)`,
			maintenanceControlServerCondition,
		}
		if !slices.Equal(conditions, want) || terminal == nil || len(terminal.List) != 1 || admissionNodeText(terminal.List[0]) != `http.Error(w, "service_control_action_failed", http.StatusConflict)` {
			fail("server_refuse_order")
		}
		if len(branches) != 3 || admissionNodeText(branches[2].Cond) != maintenanceControlServerCondition {
			fail("server_maintenance_condition")
		} else if len(branches[2].Body.List) != 1 || admissionNodeText(branches[2].Body.List[0]) != "http.Error(w, errMaintenanceFenceActive.Error(), http.StatusConflict)" {
			fail("server_maintenance_body")
		}
	}
	if maintenanceControlCalls(controlFile, "errors.Is") > 0 {
		ast.Inspect(controlFile, func(node ast.Node) bool {
			if call, ok := node.(*ast.CallExpr); ok && admissionNodeText(call) == "errors.Is(err, errMaintenanceFenceActive)" {
				fail("server_no_errors_is")
			}
			return true
		})
	}
	if maintenanceControlCalls(control.Body, "serviceControlMaintenanceRefusal") != 1 {
		fail("client_call_count")
	}
	acceptedIndex, maintenanceIndex, genericIndex := -1, -1, -1
	for index, statement := range control.Body.List {
		conditional, ok := statement.(*ast.IfStmt)
		if !ok {
			continue
		}
		switch admissionNodeText(conditional.Cond) {
		case "response.StatusCode == http.StatusAccepted":
			acceptedIndex = index
		case "response.StatusCode != http.StatusOK":
			genericIndex = index
		case "serviceControlMaintenanceRefusal(action, response.StatusCode, response.Body)":
			maintenanceIndex = index
			if len(conditional.Body.List) != 1 || admissionNodeText(conditional.Body.List[0]) != "return ServiceStatus{}, errMaintenanceFenceActive" {
				fail("client_sentinel_return")
			}
		}
	}
	if acceptedIndex < 0 || maintenanceIndex <= acceptedIndex || genericIndex <= maintenanceIndex {
		fail("client_branch_order")
	}
	ast.Inspect(helper.Body, func(node ast.Node) bool {
		if selector, ok := node.(*ast.SelectorExpr); ok && admissionNodeText(selector.X) == "fmt" {
			fail("helper_closed_errors")
		}
		if call, ok := node.(*ast.CallExpr); ok && (admissionNodeText(call.Fun) == "errors.New" || admissionNodeText(call.Fun) == "error") {
			fail("helper_closed_errors")
		}
		return true
	})
	return failures
}

func TestServiceControlMaintenanceSourceContract(t *testing.T) {
	sources := admissionReadSources(t)
	for _, ending := range []string{"LF", "CRLF"} {
		t.Run(ending, func(t *testing.T) {
			input := serviceAdmissionClone(sources)
			for filename, source := range input {
				source = strings.ReplaceAll(source, "\r\n", "\n")
				if ending == "CRLF" {
					source = strings.ReplaceAll(source, "\n", "\r\n")
				}
				input[filename] = source
			}
			if failures := maintenanceControlSourceContract(input); len(failures) != 0 {
				t.Fatalf("Maintenance source contract failed %v", failures)
			}
			maintenanceControlContractControls(t, input)
		})
	}
}

func maintenanceControlContractControls(t *testing.T, sources map[string]string) {
	t.Helper()
	type mutation struct {
		name, invariant, file, owner string
		change                       func(string) string
	}
	replace := func(before, after string) func(string) string {
		return func(source string) string { return strings.Replace(source, before, after, 1) }
	}
	insert := func(statement string) func(string) string {
		return func(source string) string { return serviceAdmissionInsert(source, 0, statement) }
	}
	mutations := []mutation{
		{"QueryCount", "step_query_count", "service_supervisor.go", "step", replace("held && s.queryStep", "held")},
		{"QueryPosition", "step_query_position", "service_supervisor.go", "step", func(source string) string {
			return admissionEditStatements(source, func(statements []ast.Stmt) []ast.Stmt {
				for index, statement := range statements {
					if conditional, ok := statement.(*ast.IfStmt); ok && admissionNodeText(conditional.Cond) == maintenanceControlQueryCondition {
						statements[index], statements[index+1] = statements[index+1], statements[index]
						break
					}
				}
				return statements
			})
		}},
		{"QueryCalls", "step_query_calls", "service_supervisor.go", "step", replace("if held && s.queryStep {", "if held && s.queryStep { _ = s.save();")},
		{"QueryReturn", "step_query_return", "service_supervisor.go", "step", func(source string) string {
			fn := serviceAdmissionFunction(t, "service_supervisor.go", "package localcell\n"+source, "step")
			query := maintenanceControlIfs(fn.Body, maintenanceControlQueryCondition)[0]
			query.Body.List[len(query.Body.List)-1] = &ast.ReturnStmt{Results: []ast.Expr{ast.NewIdent("errServiceUnhealthy")}}
			return admissionNodeText(fn)
		}},
		{"QueryAssignments", "query_assignments", "service_supervisor.go", "command", insert("s.queryStep = true")},
		{"QueryRead", "query_reads", "service_supervisor.go", "command", insert("_ = s.queryStep")},
		{"QueryBracket", "query_command_bracket", "service_supervisor.go", "command", replace("s.queryStep = false", "s.queryStep = true")},
		{"RefuseOrder", "server_refuse_order", "service_control.go", "serveServiceControlForPlatform", replace("errors.Is(err, errServiceControlDeferred)", "false")},
		{"RefuseCondition", "server_maintenance_condition", "service_control.go", "serveServiceControlForPlatform", replace(maintenanceControlServerCondition, "serviceMaintenanceOnly(err)")},
		{"RefuseBody", "server_maintenance_body", "service_control.go", "serveServiceControlForPlatform", replace("http.Error(w, errMaintenanceFenceActive.Error(), http.StatusConflict)", `http.Error(w, "localcell: " + errMaintenanceFenceActive.Error(), http.StatusConflict)`)},
		{"RefuseIdentity", "server_no_errors_is", "service_control.go", "serveServiceControlForPlatform", replace("serviceMaintenanceOnly(err)", "errors.Is(err, errMaintenanceFenceActive)")},
		{"ClientCallCount", "client_call_count", "service_control.go", "Control", insert("_ = serviceControlMaintenanceRefusal(action, 0, nil)")},
		{"ClientBranchOrder", "client_branch_order", "service_control.go", "Control", func(source string) string {
			return admissionEditStatements(source, func(statements []ast.Stmt) []ast.Stmt {
				accepted, maintenance := -1, -1
				for index, statement := range statements {
					if conditional, ok := statement.(*ast.IfStmt); ok {
						switch admissionNodeText(conditional.Cond) {
						case "response.StatusCode == http.StatusAccepted":
							accepted = index
						case "serviceControlMaintenanceRefusal(action, response.StatusCode, response.Body)":
							maintenance = index
						}
					}
				}
				statements[accepted], statements[maintenance] = statements[maintenance], statements[accepted]
				return statements
			})
		}},
		{"ClientSentinel", "client_sentinel_return", "service_control.go", "Control", replace("return ServiceStatus{}, errMaintenanceFenceActive", "return ServiceStatus{}, errors.New(errMaintenanceFenceActive.Error())")},
		{"HelperNewError", "helper_closed_errors", "service_control.go", "serviceControlMaintenanceRefusal", insert(`_ = errors.New(string([]byte("private")))`)},
		{"HelperFormattedError", "helper_closed_errors", "service_control.go", "serviceControlMaintenanceRefusal", insert(`_ = fmt.Errorf("private")`)},
		{"HelperConvertedError", "helper_closed_errors", "service_control.go", "serviceControlMaintenanceRefusal", insert(`_ = error(nil)`)},
	}
	t.Run("Controls", func(t *testing.T) {
		for _, item := range mutations {
			t.Run(item.name, func(t *testing.T) {
				input := serviceAdmissionClone(sources)
				original := admissionNodeText(serviceAdmissionFunction(t, item.file, input[item.file], item.owner))
				changed := item.change(original)
				if changed == original {
					t.Fatal("Maintenance control mutation was unchanged")
				}
				if strings.Contains(input[item.file], "\r\n") {
					changed = strings.ReplaceAll(changed, "\n", "\r\n")
				}
				input[item.file] = admissionMutateFunction(t, input[item.file], item.owner, func(string) string { return changed })
				failures := maintenanceControlSourceContract(input)
				if slices.Contains(failures, "parse") {
					t.Fatal("Maintenance control mutation did not parse")
				}
				if !slices.Contains(failures, item.invariant) {
					t.Fatalf("Maintenance control invariant survived %s with failures %v", item.invariant, failures)
				}
			})
		}
	})
}
