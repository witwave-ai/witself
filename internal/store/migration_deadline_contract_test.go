package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// These deadlines exercise cancellation itself or intentionally reserve part of
// the enclosing test deadline. They must not be replaced with migration budgets.
var migrationDeadlineExemptions = map[string]string{
	"TestMigrateContextAdvisoryLockCanceledPostgres":          "the deadline cancels the advisory-lock wait under test",
	"TestMigrateContextPreflightCanceledReleasesLockPostgres": "the deadline cancels the blocked preflight under test",
	"runFounderArchiveLoad":                                   "the enclosing t.Deadline supplies the whole workload deadline and cleanup reserve",
}

func TestStoreIntegrationMigrationDeadlineContract(t *testing.T) {
	paths, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, path := range paths {
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		// Normalize before parsing so source positions and synthetic mutation
		// checks have the same meaning on Windows checkouts.
		file, err := parser.ParseFile(fset, path, strings.ReplaceAll(string(source), "\r\n", "\n"), parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	for _, issue := range migrationDeadlineIssues(files, migrationDeadlineExemptions) {
		at := fset.Position(issue.deadline)
		t.Errorf("%s:%d: %s creates %s before migration at line %d without testenv.MigrationBudget", at.Filename, at.Line, issue.function, issue.kind, fset.Position(issue.migration).Line)
	}
}

type migrationDeadlineIssue struct {
	function  string
	kind      string
	deadline  token.Pos
	migration token.Pos
}

type migrationDeadlineCall struct {
	call   *ast.CallExpr
	blocks []*ast.BlockStmt
}

type migrationDeadlineBinding struct {
	name   string
	value  ast.Expr
	after  token.Pos
	blocks []*ast.BlockStmt
}

type migrationDeadlineFunction struct {
	calls    []migrationDeadlineCall
	bindings []migrationDeadlineBinding
}

// Block ancestry distinguishes ordinary setup from a migration in an earlier
// optional/returning branch. A subtest inherits completed outer setup, while a
// migration in a sibling subtest cannot protect its deadline.
func migrationDeadlineCalls(body *ast.BlockStmt) migrationDeadlineFunction {
	var result migrationDeadlineFunction
	var blocks []*ast.BlockStmt
	var visit func(ast.Node)
	visit = func(root ast.Node) {
		ast.Inspect(root, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.DeferStmt:
				return false
			case *ast.BlockStmt:
				blocks = append(blocks, node)
				for _, stmt := range node.List {
					visit(stmt)
				}
				blocks = blocks[:len(blocks)-1]
				return false
			case *ast.CallExpr:
				if selector, ok := node.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "Cleanup" {
					return false
				}
				result.calls = append(result.calls, migrationDeadlineCall{node, append([]*ast.BlockStmt(nil), blocks...)})
			case *ast.AssignStmt:
				if len(node.Lhs) == len(node.Rhs) {
					for i, lhs := range node.Lhs {
						if ident, ok := lhs.(*ast.Ident); ok {
							result.bindings = append(result.bindings, migrationDeadlineBinding{ident.Name, node.Rhs[i], node.End(), append([]*ast.BlockStmt(nil), blocks...)})
						}
					}
				}
			case *ast.ValueSpec:
				if len(node.Names) == len(node.Values) {
					for i, ident := range node.Names {
						result.bindings = append(result.bindings, migrationDeadlineBinding{ident.Name, node.Values[i], node.End(), append([]*ast.BlockStmt(nil), blocks...)})
					}
				}
			}
			return true
		})
	}
	visit(body)
	return result
}

func migrationDeadlineIssues(files []*ast.File, exemptions map[string]string) []migrationDeadlineIssue {
	functions := make(map[string]migrationDeadlineFunction)
	for _, file := range files {
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil && fn.Recv == nil {
				functions[fn.Name.Name] = migrationDeadlineCalls(fn.Body)
			}
		}
	}
	// Resolve fixture constructors transitively rather than maintaining a list
	// of names that silently misses newly added migration wrappers.
	migrating := make(map[string]bool)
	for changed := true; changed; {
		changed = false
		for name, function := range functions {
			if migrating[name] {
				continue
			}
			for _, call := range function.calls {
				if migrationDeadlineIsMigration(call.call, migrating) {
					migrating[name] = true
					changed = true
					break
				}
			}
		}
	}
	var issues []migrationDeadlineIssue
	for name, function := range functions {
		if exemptions[name] != "" {
			continue
		}
		for _, deadline := range function.calls {
			selector, ok := deadline.call.Fun.(*ast.SelectorExpr)
			if !ok || (selector.Sel.Name != "WithTimeout" && selector.Sel.Name != "WithDeadline") || len(deadline.call.Args) != 2 {
				continue
			}
			if migrationDeadlineHasBudget(deadline.call.Args[1], deadline.call.Pos(), deadline.blocks, function.bindings) {
				continue
			}
			var firstLater token.Pos
			setupComplete := false
			for _, migration := range function.calls {
				if !migrationDeadlineIsMigration(migration.call, migrating) {
					continue
				}
				if migration.call.Pos() < deadline.call.Pos() && migrationDeadlineScopeContains(migration.blocks, deadline.blocks) {
					setupComplete = true
					break
				}
				if migration.call.Pos() > deadline.call.Pos() && migrationDeadlineScopeContains(deadline.blocks, migration.blocks) && (firstLater == token.NoPos || migration.call.Pos() < firstLater) {
					firstLater = migration.call.Pos()
				}
			}
			if !setupComplete && firstLater != token.NoPos {
				issues = append(issues, migrationDeadlineIssue{name, selector.Sel.Name, deadline.call.Pos(), firstLater})
			}
		}
	}
	sort.Slice(issues, func(i, j int) bool { return issues[i].deadline < issues[j].deadline })
	return issues
}

func migrationDeadlineScopeContains(outer, inner []*ast.BlockStmt) bool {
	if len(outer) > len(inner) {
		return false
	}
	for i := range outer {
		if outer[i] != inner[i] {
			return false
		}
	}
	return true
}

func migrationDeadlineIsMigration(call *ast.CallExpr, migrating map[string]bool) bool {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return migrating[fn.Name] || fn.Name == "migrateSchemaWithAdvisoryLock" || fn.Name == "applyMigrationsContext"
	case *ast.SelectorExpr:
		if fn.Sel.Name == "Migrate" || fn.Sel.Name == "MigrateContext" || fn.Sel.Name == "MigrateSync" {
			return true
		}
		if pkg, ok := fn.X.(*ast.Ident); ok && pkg.Name == "goose" {
			switch fn.Sel.Name {
			case "Up", "UpContext", "UpTo", "UpToContext", "UpByOne", "Down", "DownTo", "DownToContext":
				return true
			}
		}
	}
	return false
}

func migrationDeadlineHasBudget(expr ast.Expr, before token.Pos, blocks []*ast.BlockStmt, bindings []migrationDeadlineBinding) bool {
	found := false
	ast.Inspect(expr, func(node ast.Node) bool {
		if found {
			return false
		}
		switch node := node.(type) {
		case *ast.CallExpr:
			if fn, ok := node.Fun.(*ast.SelectorExpr); ok && fn.Sel.Name == "MigrationBudget" {
				if pkg, ok := fn.X.(*ast.Ident); ok && pkg.Name == "testenv" {
					found = true
				}
			}
		case *ast.Ident:
			// Resolve only earlier assignments visible in this lexical block;
			// pair each identifier with its own RHS in a multiple assignment.
			var nearest *migrationDeadlineBinding
			for i := range bindings {
				binding := &bindings[i]
				if binding.name != node.Name || binding.after >= before || !migrationDeadlineScopeContains(binding.blocks, blocks) {
					continue
				}
				if nearest == nil || len(binding.blocks) > len(nearest.blocks) || (len(binding.blocks) == len(nearest.blocks) && binding.after > nearest.after) {
					nearest = binding
				}
			}
			if nearest != nil {
				found = migrationDeadlineHasBudget(nearest.value, nearest.after, nearest.blocks, bindings)
			}
		}
		return !found
	})
	return found
}

func TestStoreMigrationDeadlineAnalyzer(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		helper string
		want   int
	}{
		{"plain timeout", `ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second); defer cancel(); st.Migrate()`, "", 1},
		{"plain deadline", `ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(time.Minute)); defer cancel(); st.MigrateContext(ctx)`, "", 1},
		{"after migration", `if err := st.Migrate(); err != nil { t.Fatal(err) }; ctx, cancel := context.WithTimeout(context.Background(), time.Minute); defer cancel(); st.Migrate()`, "", 0},
		{"shared budget", `ctx, cancel := context.WithTimeout(context.Background(), time.Minute+testenv.MigrationBudget(0)); defer cancel(); st.Migrate()`, "", 0},
		{"resolved budget", `budget := testenv.MigrationBudget(0); allowance := time.Minute+budget; ctx, cancel := context.WithTimeout(context.Background(), allowance); defer cancel(); st.Migrate()`, "", 0},
		{"unbudgeted parallel binding", `budget, plain := testenv.MigrationBudget(0), time.Minute; ctx, cancel := context.WithTimeout(context.Background(), plain); defer cancel(); st.Migrate()`, "", 1},
		{"shadowed budget", `budget := testenv.MigrationBudget(0); t.Run("child", func(t *testing.T) { budget := time.Minute; ctx, cancel := context.WithTimeout(context.Background(), budget); defer cancel(); st.Migrate() })`, "", 1},
		{"reassigned budget", `budget := testenv.MigrationBudget(0); budget = time.Minute; ctx, cancel := context.WithTimeout(context.Background(), budget); defer cancel(); st.Migrate()`, "", 1},
		{"deadline budget", `ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(testenv.MigrationBudget(0))); defer cancel(); st.Migrate()`, "", 0},
		{"indirect fixture", `ctx, cancel := context.WithTimeout(context.Background(), time.Minute); defer cancel(); fixture(ctx)`, `func fixture(ctx context.Context) { inner() }; func inner() { st.Migrate() }`, 1},
		{"returning branch", `if founder { fixture(); return }; ctx, cancel := context.WithTimeout(context.Background(), time.Minute); defer cancel(); st.Migrate()`, `func fixture() { st.Migrate() }`, 1},
		{"ancestor setup", `st.Migrate(); t.Run("child", func(t *testing.T) { ctx, cancel := context.WithTimeout(context.Background(), time.Minute); defer cancel(); st.Migrate() })`, "", 0},
		{"sibling setup", `t.Run("first", func(t *testing.T) { st.Migrate() }); t.Run("second", func(t *testing.T) { ctx, cancel := context.WithTimeout(context.Background(), time.Minute); defer cancel(); st.Migrate() })`, "", 1},
		{"deferred migration", `ctx, cancel := context.WithTimeout(context.Background(), time.Minute); defer cancel(); defer func() { st.Migrate() }()`, "", 0},
		{"cleanup migration", `ctx, cancel := context.WithTimeout(context.Background(), time.Minute); defer cancel(); t.Cleanup(func() { st.Migrate() })`, "", 0},
		{"cleanup does not mask", `t.Cleanup(func() { st.Migrate() }); ctx, cancel := context.WithTimeout(context.Background(), time.Minute); defer cancel(); st.Migrate()`, "", 1},
		{"goose fixture", `ctx, cancel := context.WithTimeout(context.Background(), time.Minute); defer cancel(); goose.UpTo(db, "migrations", 1)`, "", 1},
		{"schema helper", `ctx, cancel := context.WithTimeout(context.Background(), time.Minute); defer cancel(); migrateSchemaWithAdvisoryLock(ctx, db, 1)`, "", 1},
	}
	for _, tc := range cases {
		for _, lineEnding := range []string{"\n", "\r\n"} {
			name := "LF"
			if lineEnding == "\r\n" {
				name = "CRLF"
			}
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				source := "package store\nfunc TestFixture(t *testing.T) {\n" + strings.ReplaceAll(tc.body, "; ", ";\n") + "\n}\n" + tc.helper
				source = strings.ReplaceAll(source, "\n", lineEnding)
				fset := token.NewFileSet()
				file, err := parser.ParseFile(fset, "fixture_test.go", strings.ReplaceAll(source, "\r\n", "\n"), parser.SkipObjectResolution)
				if err != nil {
					t.Fatal(err)
				}
				if got := len(migrationDeadlineIssues([]*ast.File{file}, nil)); got != tc.want {
					t.Fatalf("migration deadline violations=%d, want %d", got, tc.want)
				}
			})
		}
	}
}
