package localcell

import (
	"bytes"
	"errors"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// Windows does not expose a process-group query. Pin the creation expression
// on every host as well as exercising actual Ctrl-Break isolation on Windows.
func TestWindowsLaunchRequiresSuspensionGroupAndAtomicJob(t *testing.T) {
	if err := windowsLaunchContract(nil); err != nil {
		t.Fatal(err)
	}
	if err := windowsPostgresCommandContract(nil); err != nil {
		t.Fatal(err)
	}
}

func windowsLaunchContract(source []byte) error {
	var input any
	if source != nil {
		input = source
	}
	file, err := parser.ParseFile(token.NewFileSet(), "launch_windows.go", input, 0)
	if err != nil {
		return errors.New("cannot inspect Windows launch contract")
	}
	flags := map[string]bool{}
	atomicJob := false
	creationUsesFlags := map[string]bool{}
	ast.Inspect(file, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.AssignStmt:
			if len(node.Lhs) != 1 || len(node.Rhs) != 1 {
				break
			}
			name, ok := node.Lhs[0].(*ast.Ident)
			if !ok || name.Name != "flags" {
				break
			}
			ast.Inspect(node.Rhs[0], func(child ast.Node) bool {
				if selector, ok := child.(*ast.SelectorExpr); ok {
					flags[selector.Sel.Name] = true
				}
				return true
			})
		case *ast.CallExpr:
			selector, ok := node.Fun.(*ast.SelectorExpr)
			if !ok {
				break
			}
			if selector.Sel.Name == "Update" && len(node.Args) > 0 {
				name, ok := node.Args[0].(*ast.Ident)
				atomicJob = atomicJob || (ok && name.Name == "procThreadAttributeJobList")
			}
			index, found := map[string]int{"CreateProcess": 5, "CreateProcessAsUser": 6}[selector.Sel.Name]
			if found && len(node.Args) > index {
				name, ok := node.Args[index].(*ast.Ident)
				creationUsesFlags[selector.Sel.Name] = ok && name.Name == "flags"
			}
		}
		return true
	})
	if !atomicJob || !creationUsesFlags["CreateProcess"] || !creationUsesFlags["CreateProcessAsUser"] {
		return errors.New("Windows creation lost atomic job assignment or explicit flags")
	}
	for _, required := range []string{"CREATE_SUSPENDED", "CREATE_NEW_PROCESS_GROUP", "CREATE_UNICODE_ENVIRONMENT", "EXTENDED_STARTUPINFO_PRESENT"} {
		if !flags[required] {
			return errors.New("Windows launch flags omit " + required)
		}
	}
	return nil
}

func TestWindowsLaunchContractMutations(t *testing.T) {
	source, err := os.ReadFile("launch_windows.go")
	if err != nil {
		t.Fatal("cannot read launch fixture")
	}
	for _, mutation := range []struct{ name, from, to string }{
		{"ordinary-launch-flags", "windows.CreateProcess(application, command, nil, nil, true, flags,", "windows.CreateProcess(application, command, nil, nil, true, 0,"},
		{"postgres-launch-flags", "windows.CreateProcessAsUser(token, application, command, nil, nil, true, flags,", "windows.CreateProcessAsUser(token, application, command, nil, nil, true, 0,"},
		{"suspension", "windows.CREATE_SUSPENDED | ", ""},
		{"atomic-job", "attributes.Update(procThreadAttributeJobList,", "attributes.Update(0,"},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			changed := strings.Replace(string(source), mutation.from, mutation.to, 1)
			if changed == string(source) {
				t.Fatal("mutation did not match")
			}
			if err := windowsLaunchContract([]byte(changed)); err == nil {
				t.Fatal("unsafe launch mutation survived")
			}
		})
	}
}

// Native tests inspect the actual child token. This host contract also keeps
// every launch path attached to that policy when Windows cannot be executed.
func windowsPostgresCommandContract(sources map[string][]byte) error {
	functions := map[string]*ast.FuncDecl{}
	for _, name := range []string{"commands.go", "postgres_token_windows.go", "launch_windows.go", "lifecycle_proof_test.go", "postgres_export_test.go"} {
		var source any
		if data, ok := sources[name]; ok {
			source = data
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, source, 0)
		if err != nil {
			return errors.New("cannot inspect PostgreSQL command contract")
		}
		for _, declaration := range file.Decls {
			if function, ok := declaration.(*ast.FuncDecl); ok {
				functions[function.Name.Name] = function
			}
		}
	}
	for _, required := range []struct{ name, configure, execute string }{
		{"pgCommand", "configurePostgresCommand(cmd)", "cmd.Run()"},
		{"databaseCommand", "configurePostgresCommand(cmd)", "cmd.Run()"},
		{"reportedVersion", "configurePostgresVersionCommand(cmd, c.cfg.Binaries.PostgresDir)", "cmd.Run()"},
		{"requirePostgres16", "localcell.ConfigurePostgresProofCommand(cmd)", "cmd.Output()"},
	} {
		function := functions[required.name]
		if function == nil || !windowsPostgresConfiguredCommand(function.Body.List, required.configure, required.execute) {
			return errors.New("PostgreSQL execution lost restricted command configuration: " + required.name)
		}
	}
	function := functions["configurePostgresCommand"]
	if function == nil || !windowsPostgresDirectStatement(function.Body.List, "token, err := windowsPostgresToken()") ||
		!windowsPostgresDirectStatement(function.Body.List, "cmd.SysProcAttr.Token = syscall.Token(token)") {
		return errors.New("PostgreSQL commands lost restricted token assignment")
	}
	function = functions["ConfigurePostgresProofCommand"]
	if function == nil || !windowsPostgresDirectStatement(function.Body.List, "return configurePostgresCommand(cmd)") {
		return errors.New("PostgreSQL proof lost the product token policy")
	}
	function = functions["configurePostgresVersionCommand"]
	if function == nil {
		return errors.New("PostgreSQL version restriction is missing")
	}
	var distribution *ast.IfStmt
	for _, statement := range function.Body.List {
		if branch, ok := statement.(*ast.IfStmt); ok && windowsLaunchExpression(branch.Cond) == "filepath.IsAbs(directory)" {
			distribution = branch
		}
	}
	if distribution == nil || !windowsPostgresDirectStatement(distribution.Body.List, "relative, err := filepath.Rel(directory, cmd.Path)") {
		return errors.New("PostgreSQL version restriction lost its distribution boundary")
	}
	var inside *ast.IfStmt
	for _, statement := range distribution.Body.List {
		if branch, ok := statement.(*ast.IfStmt); ok && windowsLaunchExpression(branch.Cond) == `err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))` {
			inside = branch
		}
	}
	if inside == nil || !windowsPostgresDirectStatement(inside.Body.List, "return configurePostgresCommand(cmd)") {
		return errors.New("PostgreSQL version probes lost the restricted token")
	}
	function = functions["launchWindows"]
	var postgres *ast.IfStmt
	if function != nil {
		for _, statement := range function.Body.List {
			if branch, ok := statement.(*ast.IfStmt); ok && windowsLaunchExpression(branch.Cond) == `component == "postgres"` {
				postgres = branch
			}
		}
	}
	if postgres == nil || !windowsPostgresDirectStatement(postgres.Body.List, "token, tokenErr := windowsPostgresToken()") ||
		!windowsPostgresDirectStatement(postgres.Body.List, "err = windows.CreateProcessAsUser(token, application, command, nil, nil, true, flags, &block[0], directory, &startup.StartupInfo, &process)") {
		return errors.New("direct PostgreSQL launch lost the restricted token")
	}
	return nil
}

func windowsPostgresConfiguredCommand(statements []ast.Stmt, configure, execute string) bool {
	configured, released := false, false
	for index, statement := range statements {
		if windowsLaunchExpression(statement) == "release, err := "+configure {
			// Configuration must propagate failure before deferring the token
			// close and running the executable. A dead nested call cannot count.
			if index+1 >= len(statements) {
				return false
			}
			branch, ok := statements[index+1].(*ast.IfStmt)
			if !ok || windowsLaunchExpression(branch.Cond) != "err != nil" || len(branch.Body.List) != 1 {
				return false
			}
			_, returns := branch.Body.List[0].(*ast.ReturnStmt)
			if !returns && !strings.HasPrefix(windowsLaunchExpression(branch.Body.List[0]), "t.Fatal(") {
				return false
			}
			configured = true
		}
		if windowsLaunchExpression(statement) == "defer release()" {
			released = configured
		}
		executes := false
		ast.Inspect(statement, func(node ast.Node) bool {
			if call, ok := node.(*ast.CallExpr); ok && windowsLaunchExpression(call) == execute {
				executes = true
			}
			return true
		})
		if executes {
			return configured && released
		}
	}
	return false
}

func windowsPostgresDirectStatement(statements []ast.Stmt, want string) bool {
	for _, statement := range statements {
		if windowsLaunchExpression(statement) == want {
			return true
		}
	}
	return false
}

func windowsLaunchExpression(node ast.Node) string {
	var output bytes.Buffer
	if err := format.Node(&output, token.NewFileSet(), node); err != nil {
		return ""
	}
	return output.String()
}

func TestWindowsPostgresCommandContractMutations(t *testing.T) {
	for _, mutation := range []struct{ name, file, function, from, to string }{
		{"initdb-and-pg-ctl-token", "commands.go", "pgCommand", "configurePostgresCommand(cmd)", "unrestrictedCommand(cmd)"},
		{"createdb-and-psql-token", "commands.go", "databaseCommand", "configurePostgresCommand(cmd)", "unrestrictedCommand(cmd)"},
		{"product-version-token", "commands.go", "reportedVersion", "configurePostgresVersionCommand(cmd, c.cfg.Binaries.PostgresDir)", "unrestrictedCommand(cmd)"},
		{"proof-version-token", "lifecycle_proof_test.go", "requirePostgres16", "localcell.ConfigurePostgresProofCommand(cmd)", "unrestrictedCommand(cmd)"},
		{"token-assignment", "postgres_token_windows.go", "configurePostgresCommand", "cmd.SysProcAttr.Token = syscall.Token(token)", "cmd.SysProcAttr.Token = 0"},
		{"version-token-route", "postgres_token_windows.go", "configurePostgresVersionCommand", "return configurePostgresCommand(cmd)", "return func() {}, nil"},
		{"proof-token-route", "postgres_export_test.go", "ConfigurePostgresProofCommand", "return configurePostgresCommand(cmd)", "return func() {}, nil"},
		{"direct-postgres-token", "launch_windows.go", "launchWindows", "windows.CreateProcessAsUser(token,", "windows.CreateProcessAsUser(0,"},
		{"token-closed-before-run", "commands.go", "pgCommand", "defer release()", "release()"},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			source, err := os.ReadFile(mutation.file)
			if err != nil {
				t.Fatal("cannot read PostgreSQL command mutation fixture")
			}
			file, err := parser.ParseFile(token.NewFileSet(), mutation.file, source, 0)
			if err != nil {
				t.Fatal("cannot parse PostgreSQL command mutation fixture")
			}
			start, end := -1, -1
			for _, declaration := range file.Decls {
				if function, ok := declaration.(*ast.FuncDecl); ok && function.Name.Name == mutation.function {
					start, end = int(function.Pos())-1, int(function.End())-1
				}
			}
			if start < 0 {
				t.Fatal("mutation function is missing")
			}
			body := string(source[start:end])
			changed := strings.Replace(body, mutation.from, mutation.to, 1)
			if changed == body {
				t.Fatal("mutation did not match")
			}
			modified := string(source[:start]) + changed + string(source[end:])
			if err := windowsPostgresCommandContract(map[string][]byte{mutation.file: []byte(modified)}); err == nil {
				t.Fatal("unrestricted PostgreSQL command mutation survived")
			}
		})
	}
}
