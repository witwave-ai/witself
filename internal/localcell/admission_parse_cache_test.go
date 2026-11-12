package localcell

import (
	"embed"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"strings"
	"sync"
	"testing"
)

type admissionParseEntry struct {
	source      string
	file        *ast.File
	err         error
	fingerprint uint64
}

type admissionSourceCache struct {
	mu          sync.Mutex
	files       map[string]admissionParseEntry
	fingerprint admissionTreeFingerprint
	inspect     func(ast.Node) bool
}

func newAdmissionSourceCache() *admissionSourceCache {
	cache := &admissionSourceCache{files: make(map[string]admissionParseEntry)}
	// Bind the visitor once, outside the allocation-free fingerprint walk.
	cache.inspect = cache.fingerprint.visit
	return cache
}

var admissionParseCache = newAdmissionSourceCache()

// Validators only: callers must never mutate the returned tree. Keep exactly
// one source version per filename; recompute every validator's verdict.
func admissionReadOnlyParse(name, source string) (*ast.File, error) {
	return admissionParseCache.parse(name, source)
}

func (c *admissionSourceCache) parse(name, source string) (*ast.File, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cached, ok := c.files[name]; ok {
		if cached.source == source {
			return cached.file, cached.err
		}
		if c.treeFingerprint(cached.file) != cached.fingerprint {
			panic("admission parse cache entry mutated")
		}
	}
	file, err := parser.ParseFile(token.NewFileSet(), name, source, 0)
	c.files[name] = admissionParseEntry{source: source, file: file, err: err, fingerprint: c.treeFingerprint(file)}
	return file, err
}

func (c *admissionSourceCache) verify() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, entry := range c.files {
		if c.treeFingerprint(entry.file) != entry.fingerprint {
			return false
		}
	}
	return true
}

// Called only while the owning cache is locked (or by a private test cache).
// The retained visitor avoids allocating a closure for each ast.Inspect walk.
func (c *admissionSourceCache) treeFingerprint(file *ast.File) uint64 {
	c.fingerprint.value = 14695981039346656037
	if file != nil {
		ast.Inspect(file, c.inspect)
	}
	return c.fingerprint.value
}

type admissionTreeFingerprint struct{ value uint64 }

func (h *admissionTreeFingerprint) number(value uint64) {
	for range 8 {
		h.value = (h.value ^ (value & 255)) * 1099511628211
		value >>= 8
	}
}

func (h *admissionTreeFingerprint) text(value string) {
	h.number(uint64(len(value)))
	for i := range len(value) {
		h.value = (h.value ^ uint64(value[i])) * 1099511628211
	}
}

func (h *admissionTreeFingerprint) visit(node ast.Node) bool {
	if node == nil {
		h.number(0) // Preserve child boundaries as well as preorder node values.
		return true
	}
	h.text(reflect.TypeOf(node).String())
	h.number(uint64(node.Pos()))
	h.number(uint64(node.End()))
	switch node := node.(type) {
	case *ast.Ident:
		h.text(node.Name)
	case *ast.BasicLit:
		h.number(uint64(node.Kind))
		h.text(node.Value)
	case *ast.UnaryExpr:
		h.number(uint64(node.Op))
	case *ast.BinaryExpr:
		h.number(uint64(node.Op))
	case *ast.AssignStmt:
		h.number(uint64(node.Tok))
	case *ast.IncDecStmt:
		h.number(uint64(node.Tok))
	case *ast.BranchStmt:
		h.number(uint64(node.Tok))
	case *ast.RangeStmt:
		h.number(uint64(node.Tok))
	case *ast.GenDecl:
		h.number(uint64(node.Tok))
	}
	return true
}

func TestAdmissionParseCacheKeys(t *testing.T) {
	cache := newAdmissionSourceCache()
	const source = "package localcell; func f() { x := 1; _ = x }"
	first, err := cache.parse("first.go", source)
	if err != nil {
		t.Fatal("cache fixture did not parse")
	}
	again, err := cache.parse("first.go", source)
	if err != nil || again != first {
		t.Fatal("cache did not reuse identical name and source")
	}
	other, err := cache.parse("second.go", source)
	if err != nil || other == first || len(cache.files) != 2 {
		t.Fatal("cache conflated distinct filenames")
	}
	changed, err := cache.parse("first.go", source+"\nvar changed int")
	if err != nil || changed == first || len(cache.files) != 2 {
		t.Fatal("cache did not replace changed source")
	}
	returned, err := cache.parse("first.go", source)
	if err != nil || returned == first || returned == changed {
		t.Fatal("cache retained an evicted source version")
	}
	if !cache.verify() {
		t.Fatal("cache rejected unchanged trees")
	}
}

func TestAdmissionParseCacheErrors(t *testing.T) {
	cache := newAdmissionSourceCache()
	const invalid = "package localcell; func broken("
	first, firstErr := cache.parse("fixture.go", invalid)
	second, secondErr := cache.parse("fixture.go", invalid)
	if firstErr == nil || secondErr == nil || first != second || firstErr.Error() != secondErr.Error() {
		t.Fatal("cache did not retain the parse error with its source")
	}
	corrected, err := cache.parse("fixture.go", "package localcell; func fixed() {}")
	if err != nil || corrected == nil || corrected == first {
		t.Fatal("cache returned a stale parse error for corrected source")
	}
	if !cache.verify() {
		t.Fatal("cache rejected a corrected source tree")
	}
}

func TestAdmissionParseCacheFingerprint(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*ast.FuncDecl)
	}{
		{"DeleteStatement", func(fn *ast.FuncDecl) { fn.Body.List = fn.Body.List[1:] }},
		{"Identifier", func(fn *ast.FuncDecl) { fn.Name.Name = "g" }},
		{"Operator", func(fn *ast.FuncDecl) { fn.Body.List[0].(*ast.AssignStmt).Tok = token.ASSIGN }},
		{"StatementPosition", func(fn *ast.FuncDecl) { fn.Body.List[1].(*ast.ReturnStmt).Return++ }},
		{"StatementEnd", func(fn *ast.FuncDecl) { fn.Body.Rbrace++ }},
		{"LiteralValue", func(fn *ast.FuncDecl) { fn.Body.List[0].(*ast.AssignStmt).Rhs[0].(*ast.BasicLit).Value = "2" }},
		{"LiteralKind", func(fn *ast.FuncDecl) { fn.Body.List[0].(*ast.AssignStmt).Rhs[0].(*ast.BasicLit).Kind = token.FLOAT }},
	} {
		t.Run(test.name, func(t *testing.T) {
			cache := newAdmissionSourceCache()
			file, err := cache.parse("fixture.go", "package localcell; func f() int { x := 1; return x }")
			if err != nil || !cache.verify() {
				t.Fatal("fingerprint fixture did not parse unchanged")
			}
			test.edit(file.Decls[0].(*ast.FuncDecl))
			if cache.verify() {
				t.Fatal("admission parse cache mutation was not detected")
			}
		})
	}
}

// Hoisting the last statement out of a nested block keeps the preorder node
// sequence and every position; only the child boundaries move.
func TestAdmissionParseCacheFingerprintChildBoundary(t *testing.T) {
	cache := newAdmissionSourceCache()
	file, err := cache.parse("fixture.go", "package localcell; func f() { { g(); h() } }")
	if err != nil || !cache.verify() {
		t.Fatal("fingerprint fixture did not parse unchanged")
	}
	outer := file.Decls[0].(*ast.FuncDecl).Body
	inner := outer.List[0].(*ast.BlockStmt)
	outer.List = append(outer.List, inner.List[1])
	inner.List = inner.List[:1]
	if cache.verify() {
		t.Fatal("admission parse cache mutation was not detected")
	}
}

func TestAdmissionParseCacheReplacementGuard(t *testing.T) {
	cache := newAdmissionSourceCache()
	file, err := cache.parse("fixture.go", "package localcell; func f() {}")
	if err != nil {
		t.Fatal("replacement fixture did not parse")
	}
	file.Name.Name = "othercell"
	defer func() {
		if recover() != "admission parse cache entry mutated" {
			t.Fatal("cache replacement did not reject the mutated entry")
		}
	}()
	_, _ = cache.parse("fixture.go", "package localcell; func g() {}")
}

func TestAdmissionParseCacheFingerprintAllocations(t *testing.T) {
	cache := newAdmissionSourceCache()
	file, err := cache.parse("fixture.go", "package localcell; func f(x int) int { return x + 1 }")
	if err != nil {
		t.Fatal("allocation fixture did not parse")
	}
	if testing.AllocsPerRun(100, func() { cache.treeFingerprint(file) }) != 0 {
		t.Fatal("fingerprint walk allocated")
	}
}

// Embed source contracts so go test -overlay mutations are visible to the
// validator, including on platforms where runtime source reads use disk bytes.
//
//go:embed process_test.go update_admission_contract_test.go update_service_admission_contract_test.go service_maintenance_contract_test.go update_service_adapter_admission_contract_test.go windows_launch_contract_test.go service_native_progress_test.go
var admissionContractSources embed.FS

func admissionSelectorCall(expression ast.Expr, receiver, method string) (*ast.CallExpr, bool) {
	call, ok := expression.(*ast.CallExpr)
	if !ok {
		return nil, false
	}
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return nil, false
	}
	name, ok := selector.X.(*ast.Ident)
	return call, ok && name.Name == receiver && selector.Sel.Name == method
}

func TestAdmissionParseCachePostRunVerification(t *testing.T) {
	source, err := admissionContractSources.ReadFile("process_test.go")
	if err != nil {
		t.Fatal("cannot read post-run cache source contract")
	}
	lf := strings.ReplaceAll(string(source), "\r\n", "\n")
	for _, variant := range []struct{ name, source string }{{"LF", lf}, {"CRLF", strings.ReplaceAll(lf, "\n", "\r\n")}} {
		t.Run(variant.name, func(t *testing.T) {
			file, err := parser.ParseFile(token.NewFileSet(), "process_test.go", variant.source, 0)
			if err != nil {
				t.Fatal("post-run cache source contract did not parse")
			}
			var main *ast.FuncDecl
			for _, declaration := range file.Decls {
				if fn, ok := declaration.(*ast.FuncDecl); ok && fn.Name.Name == "TestMain" {
					main = fn
				}
			}
			if main == nil || main.Body == nil || len(main.Body.List) != 3 {
				t.Fatal("post-run cache verification missing")
			}
			run, ok := main.Body.List[0].(*ast.AssignStmt)
			if !ok || len(run.Lhs) != 1 || len(run.Rhs) != 1 || run.Tok != token.DEFINE {
				t.Fatal("post-run cache result was not retained")
			}
			result, ok := run.Lhs[0].(*ast.Ident)
			if !ok {
				t.Fatal("post-run cache result was not retained")
			}
			if call, ok := admissionSelectorCall(run.Rhs[0], "m", "Run"); !ok || len(call.Args) != 0 {
				t.Fatal("post-run cache verification must follow the tests")
			}
			guard, ok := main.Body.List[1].(*ast.IfStmt)
			if !ok || guard.Init != nil || guard.Else != nil {
				t.Fatal("post-run cache verification missing")
			}
			condition, ok := guard.Cond.(*ast.UnaryExpr)
			if !ok || condition.Op != token.NOT {
				t.Fatal("post-run cache verification missing")
			}
			if call, ok := admissionSelectorCall(condition.X, "admissionParseCache", "verify"); !ok || len(call.Args) != 0 {
				t.Fatal("post-run cache verification missing")
			}
			if len(guard.Body.List) != 2 {
				t.Fatal("post-run cache failure must report and fail")
			}
			diagnostic, ok := guard.Body.List[0].(*ast.ExprStmt)
			if !ok {
				t.Fatal("post-run cache fatal diagnostic missing")
			}
			call, ok := admissionSelectorCall(diagnostic.X, "fmt", "Fprintln")
			if !ok || len(call.Args) != 2 {
				t.Fatal("post-run cache fatal diagnostic missing")
			}
			stderr, ok := call.Args[0].(*ast.SelectorExpr)
			if !ok {
				t.Fatal("post-run cache fatal diagnostic missing")
			}
			output, ok := stderr.X.(*ast.Ident)
			if !ok || output.Name != "os" || stderr.Sel.Name != "Stderr" {
				t.Fatal("post-run cache fatal diagnostic missing")
			}
			message, ok := call.Args[1].(*ast.BasicLit)
			if !ok || message.Kind != token.STRING || message.Value != `"admission parse cache entry mutated"` {
				t.Fatal("post-run cache fatal diagnostic changed")
			}
			failed, ok := guard.Body.List[1].(*ast.AssignStmt)
			if !ok || failed.Tok != token.ASSIGN || len(failed.Lhs) != 1 || len(failed.Rhs) != 1 {
				t.Fatal("post-run cache failure did not change exit status")
			}
			failedResult, ok := failed.Lhs[0].(*ast.Ident)
			if !ok || failedResult.Name != result.Name {
				t.Fatal("post-run cache failure did not change exit status")
			}
			status, ok := failed.Rhs[0].(*ast.BasicLit)
			if !ok || status.Kind != token.INT || status.Value != "1" {
				t.Fatal("post-run cache failure did not change exit status")
			}
			exit, ok := main.Body.List[2].(*ast.ExprStmt)
			if !ok {
				t.Fatal("post-run cache result was not returned")
			}
			exitCall, ok := admissionSelectorCall(exit.X, "os", "Exit")
			if !ok || len(exitCall.Args) != 1 {
				t.Fatal("post-run cache result was not returned")
			}
			exitResult, ok := exitCall.Args[0].(*ast.Ident)
			if !ok || exitResult.Name != result.Name {
				t.Fatal("post-run cache result was not returned")
			}
		})
	}
}

func TestAdmissionMutableParsersStayFresh(t *testing.T) {
	for _, fixture := range []struct {
		file  string
		names []string
	}{
		{"update_admission_contract_test.go", []string{"admissionStatements", "admissionMutateFunction", "admissionEditStatements", "admissionApprovedOwners", "TestMaintenanceFenceAdmissionSourceContractMutations", "TestMaintenanceFenceAdmissionAppendixC"}},
		{"update_service_admission_contract_test.go", []string{"serviceAdmissionFunction", "serviceAdmissionInsert", "serviceAdmissionEditAt"}},
		{"service_maintenance_contract_test.go", []string{"maintenanceHostMutateDecision", "maintenanceHostSchedulerMutationFixtures"}},
		{"update_service_adapter_admission_contract_test.go", []string{"TestWindowsTaskMaintenanceAdmissionSourceContract"}},
		{"windows_launch_contract_test.go", []string{"TestWindowsPostgresCommandContractMutations"}},
		{"service_native_progress_test.go", []string{"TestNativeServiceProgressSourceContract"}},
	} {
		source, err := admissionContractSources.ReadFile(fixture.file)
		if err != nil {
			t.Fatal("cannot read fresh-parser source contract")
		}
		lf := strings.ReplaceAll(string(source), "\r\n", "\n")
		for _, variant := range []struct{ name, source string }{{"LF", lf}, {"CRLF", strings.ReplaceAll(lf, "\n", "\r\n")}} {
			t.Run(fixture.file+"/"+variant.name, func(t *testing.T) {
				file, err := parser.ParseFile(token.NewFileSet(), fixture.file, variant.source, 0)
				if err != nil {
					t.Fatal("fresh-parser source contract did not parse")
				}
				for _, name := range fixture.names {
					found := false
					for _, declaration := range file.Decls {
						fn, ok := declaration.(*ast.FuncDecl)
						if !ok || fn.Name.Name != name {
							continue
						}
						found = true
						ast.Inspect(fn, func(node ast.Node) bool {
							if ident, ok := node.(*ast.Ident); ok && ident.Name == "admissionReadOnlyParse" {
								t.Error("mutation builder used the read-only parse cache")
							}
							return true
						})
					}
					if !found {
						t.Error("fresh-parser source contract lost a mutation builder")
					}
				}
			})
		}
	}
}
