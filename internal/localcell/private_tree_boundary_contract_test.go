package localcell

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// recoveryBoundaryLiteralSites is the exact inventory of non-test string
// literals under internal/localcell/** that spell "recovery" as a path element
// in any letter case. files.go holds the validateRoot member (R1) and
// private_tree_boundary.go the walk boundary (R2). The trust literal is a stage
// label its generation seal passes to its own check, never a path.
// checkpoint.go holds recoveryDir, the one checkpoint spelling (LC-S7D2E1 RD).
var recoveryBoundaryLiteralSites = map[string]int{
	"checkpoint.go":                    1,
	"files.go":                         1,
	"private_tree_boundary.go":         1,
	"trust/private_generation_seal.go": 1,
}

// recoveryBoundaryDeclarationSites are the only declarations of
// validatePrivateTree: one per platform file, with an unchanged signature.
var recoveryBoundaryDeclarationSites = map[string]int{"files_other.go": 1, "files_windows.go": 1}

// recoveryBoundarySources reads every non-test Go source below this package
// directory, trust/ and testdata/ included, keyed by slash path, with LF line
// endings because the Windows checkout may use CRLF.
func recoveryBoundarySources(t *testing.T) map[string]string {
	t.Helper()
	sources := make(map[string]string)
	err := filepath.WalkDir(".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		sources[filepath.ToSlash(name)] = strings.ReplaceAll(string(data), "\r\n", "\n")
		return nil
	})
	if err != nil {
		t.Fatal("cannot read local cell sources for the recovery boundary contract")
	}
	return sources
}

// recoveryPathElement reports whether a quoted Go string literal names
// "recovery" as a path element under either separator, in any letter case and
// with any trailing dots or spaces Windows would ignore. An unreadable literal
// fails closed.
func recoveryPathElement(quoted string) bool {
	value, err := strconv.Unquote(quoted)
	if err != nil {
		return true
	}
	for _, element := range strings.FieldsFunc(value, func(r rune) bool { return r == '/' || r == '\\' }) {
		if strings.EqualFold(strings.TrimRight(element, ". "), "recovery") {
			return true
		}
	}
	return false
}

func recoveryBoundarySignature(function *ast.FuncType) bool {
	if function.TypeParams != nil || function.Params == nil || len(function.Params.List) != 1 ||
		function.Results == nil || len(function.Results.List) != 1 {
		return false
	}
	parameter, result := function.Params.List[0], function.Results.List[0]
	return len(parameter.Names) <= 1 && types.ExprString(parameter.Type) == "string" &&
		len(result.Names) == 0 && types.ExprString(result.Type) == "error"
}

// recoveryBoundaryReturned unpacks a function literal whose body is one return
// of one expression: its printed signature and that expression.
func recoveryBoundaryReturned(expression ast.Expr) (string, ast.Expr, bool) {
	literal, ok := expression.(*ast.FuncLit)
	if !ok || len(literal.Body.List) != 1 {
		return "", nil, false
	}
	returned, ok := literal.Body.List[0].(*ast.ReturnStmt)
	if !ok || len(returned.Results) != 1 {
		return "", nil, false
	}
	return types.ExprString(literal.Type), returned.Results[0], true
}

// recoveryBoundaryWired reports whether the Windows walk only binds the real
// validators to each reported entry and hands them, with the cell root, to the
// OS-neutral boundary decision.
func recoveryBoundaryWired(function *ast.FuncDecl) bool {
	if function.Body == nil {
		return false
	}
	for _, statement := range function.Body.List {
		returned, ok := statement.(*ast.ReturnStmt)
		if !ok || len(returned.Results) != 1 {
			continue
		}
		snapshot, ok := returned.Results[0].(*ast.CallExpr)
		if !ok || types.ExprString(snapshot.Fun) != "validatePrivateTreeSnapshot" {
			continue
		}
		if len(snapshot.Args) != 4 || types.ExprString(snapshot.Args[0]) != "root" ||
			types.ExprString(snapshot.Args[1]) != "filepath.WalkDir" || types.ExprString(snapshot.Args[3]) != "os.Lstat" {
			return false
		}
		signature, result, ok := recoveryBoundaryReturned(snapshot.Args[2])
		entry, isCall := result.(*ast.CallExpr)
		if !ok || !isCall || signature != "func(path string, directory bool) error" || len(entry.Args) != 5 ||
			types.ExprString(entry) != "privateTreeEntry(root, path, directory, (func() error literal), (func(pg string) error literal))" {
			return false
		}
		strictSignature, strict, strictOK := recoveryBoundaryReturned(entry.Args[3])
		postgresSignature, postgres, postgresOK := recoveryBoundaryReturned(entry.Args[4])
		return strictOK && postgresOK && strictSignature == "func() error" && postgresSignature == "func(pg string) error" &&
			types.ExprString(strict) == "validatePrivate(path, directory)" &&
			types.ExprString(postgres) == "runtimeconfig.WindowsPrivatePostgresValidate(pg, path, directory)"
	}
	return false
}

// recoveryBoundaryContract returns every failed check over sources, sorted.
// Every source joins the literal inventory; the walk-skip, signature and
// wiring checks apply to this package's own files.
func recoveryBoundaryContract(sources map[string]string) []string {
	var failures []string
	literals, declarations := map[string]int{}, map[string]int{}
	skips, wired := 0, false
	for _, name := range slices.Sorted(maps.Keys(sources)) {
		file, err := parser.ParseFile(token.NewFileSet(), name, sources[name], parser.SkipObjectResolution)
		if err != nil {
			failures = append(failures, "parse:"+name)
			continue
		}
		ast.Inspect(file, func(node ast.Node) bool {
			if literal, ok := node.(*ast.BasicLit); ok && literal.Kind == token.STRING && recoveryPathElement(literal.Value) {
				literals[name]++
			}
			return true
		})
		if strings.Contains(name, "/") {
			continue
		}
		walkPackages := map[string]bool{}
		for _, spec := range file.Imports {
			imported, _ := strconv.Unquote(spec.Path.Value)
			if imported != "io/fs" && imported != "path/filepath" {
				continue
			}
			local := path.Base(imported)
			if spec.Name != nil {
				local = spec.Name.Name
			}
			if local == "." {
				failures = append(failures, "dot_import:"+name)
			}
			walkPackages[local] = true
		}
		for _, declaration := range file.Decls {
			owner := ""
			if function, ok := declaration.(*ast.FuncDecl); ok {
				owner = function.Name.Name
				if function.Recv == nil && owner == "validatePrivateTree" {
					declarations[name]++
					if !recoveryBoundarySignature(function.Type) {
						failures = append(failures, "signature:"+name)
					}
					if name == "files_windows.go" {
						wired = recoveryBoundaryWired(function)
					}
				}
			}
			ast.Inspect(declaration, func(node ast.Node) bool {
				selector, ok := node.(*ast.SelectorExpr)
				if !ok || (selector.Sel.Name != "SkipDir" && selector.Sel.Name != "SkipAll") {
					return true
				}
				if base, ok := selector.X.(*ast.Ident); !ok || !walkPackages[base.Name] {
					return true
				} else if name == "private_tree_boundary.go" && owner == "privateTreeEntry" && base.Name == "fs" && selector.Sel.Name == "SkipDir" {
					skips++
				} else {
					failures = append(failures, "skip_outside_boundary:"+name)
				}
				return true
			})
		}
	}
	inventory := func(kind string, got, want map[string]int) {
		names := maps.Clone(got)
		maps.Copy(names, want)
		for name := range names {
			if got[name] != want[name] {
				failures = append(failures, kind+":"+name)
			}
		}
	}
	inventory("literal", literals, recoveryBoundaryLiteralSites)
	inventory("declaration", declarations, recoveryBoundaryDeclarationSites)
	if skips != 1 {
		failures = append(failures, "boundary_skip_count")
	}
	if !wired {
		failures = append(failures, "windows_wiring")
	}
	slices.Sort(failures)
	return slices.Compact(failures)
}

func TestRecoveryBoundarySourceContract(t *testing.T) {
	sources := recoveryBoundarySources(t)
	if failures := recoveryBoundaryContract(sources); len(failures) != 0 {
		t.Fatalf("recovery boundary source contract failed: %v", failures)
	}
	const wiredClosure = `		return privateTreeEntry(root, path, directory, func() error {
			return validatePrivate(path, directory)
		}, func(pg string) error {
			return runtimeconfig.WindowsPrivatePostgresValidate(pg, path, directory)
		})`
	const revertedClosure = `		pg := filepath.Join(root, "pg")
		if path == pg || strings.HasPrefix(path, pg+string(filepath.Separator)) {
			return runtimeconfig.WindowsPrivatePostgresValidate(pg, path, directory)
		}
		return validatePrivate(path, directory)`
	for _, mutation := range []struct {
		name, file, before, after, want string // an empty before adds file
	}{
		// Literal inventory: every new spelling of the path element, anywhere.
		{"new_read_site", "checkpoint_scan.go", "", "package localcell\n\nimport (\n\t\"os\"\n\t\"path/filepath\"\n)\n\nfunc scanCheckpoints(root string) ([]os.DirEntry, error) {\n\treturn os.ReadDir(filepath.Join(root, \"recovery\"))\n}\n", "literal:checkpoint_scan.go"},
		{"checkpoint_second_literal", "checkpoint.go", "func (c *Cell) recoveryDir() string { return c.path(\"recovery\") }", "func (c *Cell) recoveryDir() string { return c.path(\"recovery\") }\n\nfunc (c *Cell) recoveryStaging() string { return c.path(\"recovery\", \"staging\") }", "literal:checkpoint.go"},
		{"layout_creates_boundary", "files.go", `"logs", "tmp", "home"} {
		if err := privateDir(c.path(name), true)`, `"logs", "tmp", "home", "recovery"} {
		if err := privateDir(c.path(name), true)`, "literal:files.go"},
		{"validate_root_member_dropped", "files.go", `"tmp", "home", "recovery"}`, `"tmp", "home"}`, "literal:files.go"},
		{"nested_path_literal", "checkpoint_scan.go", "", "package localcell\n\nimport \"path/filepath\"\n\nfunc checkpointStaging(root string) string { return filepath.Join(root, \"recovery/staging\") }\n", "literal:checkpoint_scan.go"},
		{"windows_separator_literal", "checkpoint_scan.go", "", "package localcell\n\nfunc checkpointStaging(root string) string { return root + \"\\\\recovery\\\\staging\" }\n", "literal:checkpoint_scan.go"},
		{"case_variant_literal", "checkpoint_scan.go", "", "package localcell\n\nimport \"path/filepath\"\n\nfunc checkpointRoot(root string) string { return filepath.Join(root, \"Recovery\") }\n", "literal:checkpoint_scan.go"},
		{"raw_literal", "checkpoint_scan.go", "", "package localcell\n\nimport \"path/filepath\"\n\nfunc checkpointRoot(root string) string { return filepath.Join(root, `recovery`) }\n", "literal:checkpoint_scan.go"},
		{"trailing_dot_literal", "checkpoint_scan.go", "", "package localcell\n\nimport \"path/filepath\"\n\nfunc checkpointRoot(root string) string { return filepath.Join(root, \"recovery.\") }\n", "literal:checkpoint_scan.go"},
		{"trust_read_site", "trust/payload.go", `entries, err := os.ReadDir(payloadDir)`, `entries, err := os.ReadDir(filepath.Join(root, "recovery"))`, "literal:trust/payload.go"},
		{"trust_second_stage_label", "trust/private_generation_seal.go", `recovery.handle, "rollback", false)`, `recovery.handle, "recovery", false)`, "literal:trust/private_generation_seal.go"},
		{"testdata_read_site", "testdata/lifecyclefixture/scan.go", "", "package main\n\nimport \"os\"\n\nfunc scan() { _, _ = os.ReadDir(\"cell/recovery\") }\n", "literal:testdata/lifecyclefixture/scan.go"},
		// Only the boundary helper may end a walk early.
		{"skipdir_in_other_function", "checkpoint_scan.go", "", "package localcell\n\nimport \"io/fs\"\n\nfunc skipCheckpoint() error { return fs.SkipDir }\n", "skip_outside_boundary:checkpoint_scan.go"},
		{"filepath_skipdir", "checkpoint_scan.go", "", "package localcell\n\nimport \"path/filepath\"\n\nfunc skipCheckpoint() error { return filepath.SkipDir }\n", "skip_outside_boundary:checkpoint_scan.go"},
		{"aliased_skipall", "checkpoint_scan.go", "", "package localcell\n\nimport iofs \"io/fs\"\n\nvar stopWalk = iofs.SkipAll\n", "skip_outside_boundary:checkpoint_scan.go"},
		{"dot_import", "checkpoint_scan.go", "", "package localcell\n\nimport . \"io/fs\"\n\nvar stopWalk = SkipDir\n", "dot_import:checkpoint_scan.go"},
		{"second_skip_in_boundary", "private_tree_boundary.go", "\t\treturn fs.SkipDir\n", "\t\tif root == path {\n\t\t\treturn fs.SkipDir\n\t\t}\n\t\treturn fs.SkipDir\n", "boundary_skip_count"},
		{"boundary_without_skip", "private_tree_boundary.go", "\t\treturn fs.SkipDir\n", "\t\treturn nil\n", "boundary_skip_count"},
		{"skip_in_boundary_file_other_function", "private_tree_boundary.go", "func privateTreeEntry(", "func privateTreeSkip() error { return fs.SkipDir }\n\nfunc privateTreeEntry(", "skip_outside_boundary:private_tree_boundary.go"},
		// The validatePrivateTree signature and its two platform declarations.
		{"signature_walk_parameter", "files_windows.go", "func validatePrivateTree(root string) error {", "func validatePrivateTree(root string, walk func(string, fs.WalkDirFunc) error) error {", "signature:files_windows.go"},
		{"signature_result", "files_other.go", "func validatePrivateTree(string) error { return nil }", "func validatePrivateTree(string) bool   { return true }", "signature:files_other.go"},
		{"signature_third_declaration", "files_plan9.go", "", "package localcell\n\nfunc validatePrivateTree(string) error { return nil }\n", "declaration:files_plan9.go"},
		{"signature_renamed", "files_other.go", "func validatePrivateTree(string) error { return nil }", "func validatePrivateTreeOther(string) error { return nil }", "declaration:files_other.go"},
		// The Windows walk wires only the real validators into the boundary.
		{"wiring_reverted", "files_windows.go", wiredClosure, revertedClosure, "windows_wiring"},
		{"wiring_other_validator", "files_windows.go", "\t\t\treturn validatePrivate(path, directory)\n", "\t\t\treturn runtimeconfig.WindowsPrivateValidate(path, directory)\n", "windows_wiring"},
		{"wiring_other_root", "files_windows.go", "return privateTreeEntry(root, path, directory,", "return privateTreeEntry(filepath.Dir(root), path, directory,", "windows_wiring"},
		{"wiring_postgres_root", "files_windows.go", "WindowsPrivatePostgresValidate(pg, path, directory)", "WindowsPrivatePostgresValidate(root, path, directory)", "windows_wiring"},
		{"unparsable", "files.go", "package localcell\n", "package localcell {\n", "parse:files.go"},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			mutated := maps.Clone(sources)
			if mutation.before == "" {
				if _, exists := mutated[mutation.file]; exists {
					t.Fatal("synthetic source would replace a real file")
				}
				mutated[mutation.file] = mutation.after
			} else {
				original := mutated[mutation.file]
				if strings.Count(original, mutation.before) != 1 {
					t.Fatal("source contract mutation anchor is not unique")
				}
				mutated[mutation.file] = strings.Replace(original, mutation.before, mutation.after, 1)
			}
			if failures := recoveryBoundaryContract(mutated); !slices.Contains(failures, mutation.want) {
				t.Fatalf("source contract reported %v, want %s", failures, mutation.want)
			}
		})
	}
	t.Run("non_path_mentions_pass", func(t *testing.T) {
		mutated := maps.Clone(sources)
		mutated["checkpoint_notes.go"] = "package localcell\n\n// Checkpoints will live under <root>/recovery/.\nvar checkpointNotes = []string{\"recovery_verified\", \"manual recovery required\", \"recovery.json\", \"recoveryPoint\"}\n"
		if failures := recoveryBoundaryContract(mutated); len(failures) != 0 {
			t.Fatalf("source contract refused mentions that are not path elements: %v", failures)
		}
	})
}
