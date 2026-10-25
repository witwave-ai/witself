package localcell_test

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

func TestRealLifecycleUnicodeFixture(t *testing.T) {
	// Evaluate the actual round-trip constant, rather than matching source text
	// or requiring a database. ASCII alone cannot exercise the encoding proof.
	for _, script := range []struct {
		name  string
		runes *unicode.RangeTable
	}{{"Latin", unicode.Latin}, {"Greek", unicode.Greek}, {"Han", unicode.Han}} {
		t.Run(script.name, func(t *testing.T) {
			for _, codepoint := range lifecycleUnicodeName {
				if codepoint > unicode.MaxASCII && unicode.Is(script.runes, codepoint) {
					return
				}
			}
			t.Fatal("Unicode round-trip constant lacks a non-ASCII script witness")
		})
	}
	t.Run("non-BMP", func(t *testing.T) {
		for _, codepoint := range lifecycleUnicodeName {
			if codepoint > 0xffff {
				return
			}
		}
		t.Fatal("Unicode round-trip constant lacks a non-BMP code point")
	})
}

func TestRealLifecycleDatabaseSettings(t *testing.T) {
	for _, fixture := range []struct {
		name, encoding, collate, ctype string
		want                           bool
	}{
		{"UTF8-C-C", "UTF8", "C", "C", true},
		{"wrong-encoding", "SQL_ASCII", "C", "C", false},
		{"empty-encoding", "", "C", "C", false},
		{"encoding-case", "utf8", "C", "C", false},
		{"wrong-collation", "UTF8", "en_US.UTF-8", "C", false},
		{"empty-collation", "UTF8", "", "C", false},
		{"collation-suffix", "UTF8", "C.UTF-8", "C", false},
		{"wrong-ctype", "UTF8", "C", "en_US.UTF-8", false},
		{"empty-ctype", "UTF8", "C", "", false},
		{"ctype-suffix", "UTF8", "C", "C.UTF-8", false},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			if got := postgresEncodingAndLocaleMatch(fixture.encoding, fixture.collate, fixture.ctype); got != fixture.want {
				t.Fatalf("database settings match = %t, want %t", got, fixture.want)
			}
		})
	}
}

// The database proof is deliberately not run without an explicit native PG16
// distribution. Inspect its wiring on every host so that a skipped native run
// cannot hide a detached assertion. This contracts the current success path,
// not arbitrary Go control flow: required calls must be direct statements.
func TestRealLifecycleProofContract(t *testing.T) {
	if err := realLifecycleProofContract(nil); err != nil {
		t.Fatal(err)
	}
}

func realLifecycleProofContract(sources map[string][]byte) error {
	functions := make(map[string]*ast.FuncDecl)
	for _, name := range []string{"lifecycle_integration_test.go", "lifecycle_proof_test.go"} {
		var source any
		if data, ok := sources[name]; ok {
			source = data
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, source, 0)
		if err != nil {
			return errors.New("cannot parse real lifecycle proof contract")
		}
		for _, declaration := range file.Decls {
			if function, ok := declaration.(*ast.FuncDecl); ok && function.Recv == nil {
				functions[function.Name.Name] = function
			}
		}
	}

	parent := lifecycleProofBody(functions["TestRealLifecycle"])
	buildIndex := -1
	for index, statement := range parent {
		assignment, ok := statement.(*ast.AssignStmt)
		if ok && len(assignment.Lhs) == 1 && len(assignment.Rhs) == 1 &&
			lifecycleProofExpression(assignment.Lhs[0]) == "binaries" &&
			lifecycleProofExpression(assignment.Rhs[0]) == "buildRealComponents(t, pgbin)" {
			buildIndex = index
		}
	}
	if buildIndex < 0 || !lifecycleProofDirectCall(lifecycleProofBody(functions["buildRealComponents"]), "requirePostgres16(t, pgbin)") {
		return errors.New("real lifecycle scenarios lost the shared PostgreSQL 16 preflight")
	}
	if lifecycleProofFailureBranch(lifecycleProofBody(functions["requirePostgres16"]), "!postgres16Version.Match(bytes.TrimSpace(output))") < 0 {
		return errors.New("PostgreSQL executable-major preflight lost its terminating guard")
	}
	if !lifecycleProofStoppedWitness(lifecycleProofBody(functions["disposableCell"])) {
		return errors.New("real lifecycle cleanup lost its deferred stopped-process witness")
	}

	scenarios := []struct {
		name, helper string
		callback     bool
		cells        int
		proofCalls   []string
	}{
		{"InstallStartRestartRemove", "testInstallStartRestartRemove", false, 1, []string{"assertPostgresEncodingAndUnicode(t, root, manifest)"}},
		{"TwoCellsAreIndependent", "testTwoCellsAreIndependent", true, 2, []string{
			"databaseProof(t, firstRoot, assertReady(t, first))",
			"databaseProof(t, secondRoot, assertReady(t, second))",
		}},
		{"InterruptedStartRepair", "testInterruptedStartRepair", true, 1, []string{"databaseProof(t, root, manifest)"}},
		{"WaitsForReadiness", "testWaitsForReadiness", true, 1, []string{"databaseProof(t, root, assertReady(t, cell))"}},
	}
	for _, scenario := range scenarios {
		dispatch := scenario.helper + "(t, binaries)"
		proofStatements := lifecycleProofBody(functions[scenario.helper])
		if lifecycleProofDisposableCells(proofStatements) != scenario.cells {
			return fmt.Errorf("real lifecycle scenario %s lost its witnessed cell cleanup", scenario.name)
		}
		if scenario.callback {
			dispatch = scenario.helper + "(t, binaries, assertPostgresEncodingAndUnicode)"
			proofStatements = lifecycleProofCallbackBody(proofStatements)
		}
		matched := 0
		for index, statement := range parent {
			call := lifecycleProofStatementCall(statement)
			if call == nil || lifecycleProofExpression(call.Fun) != "t.Run" || len(call.Args) != 2 {
				continue
			}
			name, ok := call.Args[0].(*ast.BasicLit)
			if !ok || name.Kind != token.STRING {
				continue
			}
			value, err := strconv.Unquote(name.Value)
			if err != nil || value != scenario.name {
				continue
			}
			callback, ok := call.Args[1].(*ast.FuncLit)
			if index <= buildIndex || !ok || !lifecycleProofDirectCall(lifecycleProofLiveStatements(callback.Body), dispatch) {
				return fmt.Errorf("real lifecycle scenario %s lost its post-preflight dispatch", scenario.name)
			}
			matched++
		}
		if matched != 1 {
			return fmt.Errorf("real lifecycle scenario %s is absent or duplicated", scenario.name)
		}
		for _, proof := range scenario.proofCalls {
			if !lifecycleProofDirectCall(proofStatements, proof) {
				return fmt.Errorf("real lifecycle scenario %s lost its encoding and Unicode proof", scenario.name)
			}
		}
	}

	proof := lifecycleProofBody(functions["assertPostgresEncodingAndUnicode"])
	manifestIndex := lifecycleProofFailureBranch(proof, "!postgres16Version.MatchString(manifest.PostgreSQLVersion)")
	sqlIndex := lifecycleProofFailureBranch(proof, "version/10000 != 16")
	if manifestIndex < 0 || sqlIndex < 0 || manifestIndex >= sqlIndex {
		return errors.New("real lifecycle proof lost manifest or SQL PostgreSQL 16 terminating guard")
	}
	if lifecycleProofFailureBranch(proof, "!postgresEncodingAndLocaleMatch(encoding, collate, ctype)") != sqlIndex {
		return errors.New("real lifecycle proof lost its database encoding and locale terminating guard")
	}
	unicodeRequest := false
	for _, statement := range proof {
		assignment, ok := statement.(*ast.AssignStmt)
		if ok && len(assignment.Rhs) == 1 && lifecycleProofExpression(assignment.Rhs[0]) == `json.Marshal(map[string]string{"display_name": lifecycleUnicodeName, "ttl": "1h"})` {
			unicodeRequest = true
		}
	}
	if !unicodeRequest {
		return errors.New("real lifecycle proof lost its shared Unicode request constant")
	}
	for _, mismatch := range []string{"created.Operator.Name != lifecycleUnicodeName", "!found", "stored != lifecycleUnicodeName"} {
		if lifecycleProofFailureBranch(proof, mismatch) < 0 {
			return errors.New("real lifecycle Unicode mismatch lost its terminating guard")
		}
	}
	// Pin the SQL source of version as well as the predicate, so a free-standing
	// expression or an unused version query cannot stand in for the database check.
	queryIndex := -1
	for index, statement := range proof {
		assignment, ok := statement.(*ast.AssignStmt)
		if !ok || len(assignment.Rhs) != 1 {
			continue
		}
		scan, ok := assignment.Rhs[0].(*ast.CallExpr)
		if !ok || len(scan.Args) != 4 || lifecycleProofExpression(scan.Args[0]) != "&encoding" ||
			lifecycleProofExpression(scan.Args[1]) != "&collate" || lifecycleProofExpression(scan.Args[2]) != "&ctype" ||
			lifecycleProofExpression(scan.Args[3]) != "&version" {
			continue
		}
		selector, ok := scan.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "Scan" {
			continue
		}
		query, ok := selector.X.(*ast.CallExpr)
		if !ok || lifecycleProofExpression(query.Fun) != "db.QueryRow" || len(query.Args) != 2 {
			continue
		}
		literal, ok := query.Args[1].(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			continue
		}
		sql, err := strconv.Unquote(literal.Value)
		if err == nil && strings.Contains(sql, "pg_encoding_to_char(encoding), datcollate, datctype, current_setting('server_version_num')::int") {
			queryIndex = index
		}
	}
	if queryIndex <= manifestIndex || queryIndex >= sqlIndex {
		return errors.New("real lifecycle SQL major, encoding or locale guard is disconnected from its database query")
	}
	return nil
}

func lifecycleProofDisposableCells(statements []ast.Stmt) int {
	count := 0
	for _, statement := range statements {
		assignment, ok := statement.(*ast.AssignStmt)
		if !ok || len(assignment.Rhs) != 1 {
			continue
		}
		call, ok := assignment.Rhs[0].(*ast.CallExpr)
		if ok && lifecycleProofExpression(call.Fun) == "disposableCell" && len(call.Args) == 3 &&
			lifecycleProofExpression(call.Args[0]) == "t" && lifecycleProofExpression(call.Args[1]) == "binaries" {
			count++
		}
	}
	return count
}

func lifecycleProofStoppedWitness(statements []ast.Stmt) bool {
	for _, statement := range statements {
		call := lifecycleProofStatementCall(statement)
		if call == nil || lifecycleProofExpression(call.Fun) != "t.Cleanup" || len(call.Args) != 1 {
			continue
		}
		callback, ok := call.Args[0].(*ast.FuncLit)
		if !ok {
			continue
		}
		for _, cleanup := range lifecycleProofLiveStatements(callback.Body) {
			deferred, ok := cleanup.(*ast.DeferStmt)
			if ok && lifecycleProofExpression(deferred.Call) == "witness.assertStopped(t)" {
				return true
			}
		}
	}
	return false
}

func lifecycleProofCallbackBody(statements []ast.Stmt) []ast.Stmt {
	for _, statement := range statements {
		branch, ok := statement.(*ast.IfStmt)
		if ok && lifecycleProofExpression(branch.Cond) == "databaseProof != nil" {
			return lifecycleProofLiveStatements(branch.Body)
		}
	}
	return nil
}

func lifecycleProofBody(function *ast.FuncDecl) []ast.Stmt {
	if function == nil {
		return nil
	}
	return lifecycleProofLiveStatements(function.Body)
}

func lifecycleProofLiveStatements(body *ast.BlockStmt) []ast.Stmt {
	if body == nil {
		return nil
	}
	for index, statement := range body.List {
		if _, ok := statement.(*ast.ReturnStmt); ok {
			return body.List[:index]
		}
		if lifecycleProofTerminatingCall(statement) {
			return body.List[:index+1]
		}
	}
	return body.List
}

func lifecycleProofStatementCall(statement ast.Stmt) *ast.CallExpr {
	expression, ok := statement.(*ast.ExprStmt)
	if !ok {
		return nil
	}
	call, _ := expression.X.(*ast.CallExpr)
	return call
}

func lifecycleProofDirectCall(statements []ast.Stmt, expression string) bool {
	for _, statement := range statements {
		if call := lifecycleProofStatementCall(statement); call != nil && lifecycleProofExpression(call) == expression {
			return true
		}
	}
	return false
}

func lifecycleProofTerminatingCall(statement ast.Stmt) bool {
	if lifecycleProofTestTerminatingCall(statement) {
		return true
	}
	call := lifecycleProofStatementCall(statement)
	return call != nil && lifecycleProofExpression(call.Fun) == "panic"
}

func lifecycleProofTestTerminatingCall(statement ast.Stmt) bool {
	call := lifecycleProofStatementCall(statement)
	if call == nil {
		return false
	}
	switch lifecycleProofExpression(call.Fun) {
	case "t.Fatal", "t.Fatalf", "t.FailNow":
		return true
	}
	return false
}

func lifecycleProofFailureBranch(statements []ast.Stmt, predicate string) int {
	for index, statement := range statements {
		branch, ok := statement.(*ast.IfStmt)
		if !ok || !lifecycleProofFailureDisjunct(branch.Cond, predicate) {
			continue
		}
		for _, child := range lifecycleProofLiveStatements(branch.Body) {
			if lifecycleProofTestTerminatingCall(child) {
				return index
			}
		}
	}
	return -1
}

func lifecycleProofFailureDisjunct(expression ast.Expr, predicate string) bool {
	if lifecycleProofExpression(expression) == predicate {
		return true
	}
	switch expression := expression.(type) {
	case *ast.ParenExpr:
		return lifecycleProofFailureDisjunct(expression.X, predicate)
	case *ast.BinaryExpr:
		return expression.Op == token.LOR && (lifecycleProofFailureDisjunct(expression.X, predicate) || lifecycleProofFailureDisjunct(expression.Y, predicate))
	}
	return false
}

func lifecycleProofExpression(node ast.Node) string {
	var buffer bytes.Buffer
	if err := format.Node(&buffer, token.NewFileSet(), node); err != nil {
		return ""
	}
	return buffer.String()
}

func TestRealLifecycleProofContractMutations(t *testing.T) {
	sources := make(map[string][]byte)
	for _, name := range []string{"lifecycle_integration_test.go", "lifecycle_proof_test.go"} {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal("cannot read real lifecycle proof fixture")
		}
		// Windows checkouts may carry CRLF line endings (core.autocrlf); the
		// contract compares and mutates LF source text on every platform.
		data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
		sources[name] = data
	}
	for _, mutation := range []struct{ name, file, from, to string }{
		{"M6-executable-major-call", "lifecycle_integration_test.go", "requirePostgres16(t, pgbin)", "// requirePostgres16(t, pgbin)"},
		{"M6-manifest-major-branch", "lifecycle_proof_test.go", "!postgres16Version.MatchString(manifest.PostgreSQLVersion)", "false"},
		{"M6-sql-major-predicate", "lifecycle_proof_test.go", " || version/10000 != 16", ""},
		{"M7-install-unicode-call", "lifecycle_integration_test.go", "assertPostgresEncodingAndUnicode(t, root, manifest)", "// assertPostgresEncodingAndUnicode(t, root, manifest)"},
		{"M17-unicode-request-detached", "lifecycle_proof_test.go", `"display_name": lifecycleUnicodeName`, `"display_name": "ASCII only"`},
		{"M19-settings-predicate-deleted", "lifecycle_proof_test.go", " || !postgresEncodingAndLocaleMatch(encoding, collate, ctype)", ""},
		{"M19-settings-input-detached", "lifecycle_proof_test.go", "!postgresEncodingAndLocaleMatch(encoding, collate, ctype)", `!postgresEncodingAndLocaleMatch("UTF8", "C", "C")`},
		{"M19-settings-unscanned", "lifecycle_proof_test.go", "Scan(&encoding, &collate, &ctype, &version)", "Scan(&unrelated, &collate, &ctype, &version)"},
		{"M19-settings-unqueried", "lifecycle_proof_test.go", "pg_encoding_to_char(encoding), datcollate, datctype,", "'UTF8', 'C', 'C',"},
		{"M21-unicode-create-nonterminating", "lifecycle_proof_test.go", `t.Fatal("Unicode create response did not round trip")`, `t.Log("Unicode create response did not round trip")`},
		{"M21-unicode-read-nonterminating", "lifecycle_proof_test.go", `t.Fatal("Unicode row did not round trip through the local server")`, `t.Log("Unicode row did not round trip through the local server")`},
		{"M21-unicode-store-nonterminating", "lifecycle_proof_test.go", `t.Fatal("Unicode server write was not persisted byte-for-byte")`, `t.Log("Unicode server write was not persisted byte-for-byte")`},
		{"R-cleanup-deferred-witness-deleted", "lifecycle_integration_test.go", "defer witness.assertStopped(t)", ""},
		{"R-cleanup-witness-not-deferred", "lifecycle_integration_test.go", "defer witness.assertStopped(t)", "witness.assertStopped(t)"},
		{"R-cleanup-witness-unreachable", "lifecycle_integration_test.go", "defer witness.assertStopped(t)", "return\n\t\tdefer witness.assertStopped(t)"},
		{"R-cleanup-second-cell-unwitnessed", "lifecycle_integration_test.go", "second, secondRoot := disposableCell(t, binaries, nil)", "second, secondRoot := unwitnessedCell(t, binaries, nil)"},
		{"two-cells-first-proof", "lifecycle_integration_test.go", "databaseProof(t, firstRoot, assertReady(t, first))", ""},
		{"two-cells-second-proof", "lifecycle_integration_test.go", "databaseProof(t, secondRoot, assertReady(t, second))", ""},
		{"interruption-proof", "lifecycle_integration_test.go", "databaseProof(t, root, manifest)", ""},
		{"readiness-proof", "lifecycle_integration_test.go", "databaseProof(t, root, assertReady(t, cell))", ""},
		{"callback-guard", "lifecycle_integration_test.go", "if databaseProof != nil {\n\t\tdatabaseProof(t, firstRoot", "if databaseProof == nil {\n\t\tdatabaseProof(t, firstRoot"},
		{"unreached-proof", "lifecycle_integration_test.go", "assertPostgresEncodingAndUnicode(t, root, manifest)", "return\n\tassertPostgresEncodingAndUnicode(t, root, manifest)"},
		{"unused-closure-proof", "lifecycle_integration_test.go", "assertPostgresEncodingAndUnicode(t, root, manifest)", "_ = func() { assertPostgresEncodingAndUnicode(t, root, manifest) }"},
		{"manifest-major-nonterminating", "lifecycle_proof_test.go", "t.Fatal(\"manifest did not record PostgreSQL major 16\")", "t.Log(\"manifest did not record PostgreSQL major 16\")"},
		{"sql-major-nonterminating", "lifecycle_proof_test.go", "t.Fatal(\"created database must use PostgreSQL 16, UTF8 encoding and C collation/ctype\")", "t.Log(\"created database must use PostgreSQL 16, UTF8 encoding and C collation/ctype\")"},
		{"sql-major-unscanned", "lifecycle_proof_test.go", "&collate, &ctype, &version", "&collate, &ctype, &unrelated"},
		{"sql-major-unqueried", "lifecycle_proof_test.go", "current_setting('server_version_num')::int", "160000"},
		{"missing-install-dispatch", "lifecycle_integration_test.go", "testInstallStartRestartRemove(t, binaries)", ""},
		{"missing-two-cells-dispatch", "lifecycle_integration_test.go", "testTwoCellsAreIndependent(t, binaries, assertPostgresEncodingAndUnicode)", ""},
		{"missing-interruption-dispatch", "lifecycle_integration_test.go", "testInterruptedStartRepair(t, binaries, assertPostgresEncodingAndUnicode)", ""},
		{"missing-readiness-dispatch", "lifecycle_integration_test.go", "testWaitsForReadiness(t, binaries, assertPostgresEncodingAndUnicode)", ""},
		{"nil-real-callback", "lifecycle_integration_test.go", "testWaitsForReadiness(t, binaries, assertPostgresEncodingAndUnicode)", "testWaitsForReadiness(t, binaries, nil)"},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			original := string(sources[mutation.file])
			if strings.Count(original, mutation.from) != 1 {
				t.Fatal("proof mutation target is not unique")
			}
			changed := map[string][]byte{
				"lifecycle_integration_test.go": sources["lifecycle_integration_test.go"],
				"lifecycle_proof_test.go":       sources["lifecycle_proof_test.go"],
			}
			changed[mutation.file] = []byte(strings.Replace(original, mutation.from, mutation.to, 1))
			if err := realLifecycleProofContract(changed); err == nil {
				t.Fatal("disconnected real lifecycle proof mutation survived")
			}
		})
	}
}
