package localcell

import (
	"context"
	"errors"
	"go/ast"
	"go/build"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"maps"
	"os"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// readers4Diagnostic is a line the readiness log tail recognizes.
const readers4Diagnostic = "witself-server: listener bind failed: other"

// readers4LogRows plant the server log and give the diagnostic its tail yields:
// a regular log its own, an empty one none; linux and darwin add FIFO and link.
var readers4LogRows = map[string]func(t *testing.T, log string) (string, bool){
	"regular": func(_ *testing.T, log string) (string, bool) {
		return readers4Diagnostic, writeAtomicBytes(log, []byte("untrusted-output\n"+readers4Diagnostic+"\n"), nil) == nil
	},
	"empty": func(_ *testing.T, log string) (string, bool) { return "", writeAtomicBytes(log, nil, nil) == nil },
}

// Each row calls the reader directly within the reader watchdog; no writer is
// opened at the log's name unless the read has already blocked.
func TestReaders4ReadinessLog(t *testing.T) {
	for name, plant := range readers4LogRows {
		t.Run(name, func(t *testing.T) {
			c, _ := unitCell(t)
			log, got := c.path("logs", "server.log"), ""
			want, prepared := plant(t, log)
			if !prepared {
				t.Fatal("cannot prepare the readiness log")
			}
			_ = readerWatchdog(t, func() error { got = c.lastValueFreeDiagnostic(context.Background(), "server", 0); return nil }, func() {
				if writer, err := os.OpenFile(log, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
					_ = writer.Close()
				}
			})
			if got != want {
				t.Fatal("readiness log tail changed its outcome")
			}
		})
	}
}

// readers4Recover recovers a postmaster.pid of exactly size bytes for PID 4242
// through fixed observers; nil only when its exact fence was written.
func readers4Recover(t *testing.T, size int) error {
	t.Helper()
	c, _ := unitCell(t)
	record := "4242\n" + c.path("pg") + "\n1700000000\n"
	record += strings.Repeat("x", size-len(record)-1) + "\n"
	if testMkdir(c.path("pg"), 0o700) != nil || testWriteFile(c.path("pg", "postmaster.pid"), []byte(record), 0o600) != nil {
		t.Fatal("cannot prepare the recovery record")
	}
	c.recoveryProcessIdentity = func(int) (string, bool, error) { return "identity-A", true, nil }
	c.recoveryProcessStartedAt = func(int) (time.Time, error) { return time.Unix(1700000000-5, 0), nil }
	var fence ProcessRecord
	err := c.recoverPostgresRecord(t.Context())
	if err == nil && (readJSON(t.Context(), c.path("run", "postgres.json"), &fence) != nil || fence != (ProcessRecord{PID: 4242, StartTime: "identity-A", Root: c.cfg.Root, Component: "postgres"})) {
		err = errors.New("recovery fence lost")
	}
	return err
}

// Both reads of postmaster.pid accept a regular record of exactly the shared
// bound; one byte more is refused by the first read with its existing text.
func TestReaders4RecoveryRecordBound(t *testing.T) {
	if readers4Recover(t, 8192) != nil {
		t.Fatal("recovery refused a regular record within the bound")
	}
	if err := readers4Recover(t, 8193); err == nil || err.Error() != "invalid PostgreSQL process record" {
		t.Fatal("recovery accepted a record past the bound")
	}
}

// readers4Std type-checks standard packages from source, in process.
var readers4Fset = token.NewFileSet()
var readers4Std = importer.ForCompiler(readers4Fset, "source", nil)

type readers4Importer func(string) (*types.Package, error)

func (f readers4Importer) Import(path string) (*types.Package, error) { return f(path) }

// readers4Forbidden can wait on or follow what stands at a name, by declaration.
var readers4Forbidden = []string{"os.ReadFile", "os.Open", "os.OpenFile", "localcell.readPostgresFile", "localcell.readPrivateFile", "localcell.openPrivateFile"}

// readers4Contract type-checks the package sources as built here and returns
// lifecycle.go's failed rules, sorted: types; each forbidden read referenced,
// directly or through a package variable declared as one; and the re-read's
// call, opener, bound (postmaster's own), lease cut and ordinary failure cause.
func readers4Contract(sources map[string]string) []string {
	failed, admit, files := map[string]bool{}, map[string]bool{}, []*ast.File{}
	fail := func(key string, bad bool) { failed[key] = failed[key] || bad }
	var lifecycle *ast.File
	for _, name := range slices.Sorted(maps.Keys(sources)) {
		file, err := parser.ParseFile(readers4Fset, name, sources[name], 0)
		if fail("types", err != nil); name == "lifecycle.go" {
			lifecycle = file
			for _, spec := range file.Imports {
				admit[strings.Trim(spec.Path.Value, "\"`")] = true
			}
		}
		files = append(files, file)
	}
	info := &types.Info{Uses: map[*ast.Ident]types.Object{}, Defs: map[*ast.Ident]types.Object{}}
	config := types.Config{Importer: readers4Importer(func(path string) (*types.Package, error) {
		if !admit[path] {
			return nil, errors.New("import not admitted")
		}
		return readers4Std.Import(path)
	}), Error: func(err error) {
		file := readers4Fset.File(err.(types.Error).Pos)
		fail("types", file != nil && file.Name() == "lifecycle.go")
	}}
	_, _ = config.Check("localcell", readers4Fset, files, info)
	named := func(expression ast.Expr) types.Object {
		if selector, ok := ast.Unparen(expression).(*ast.SelectorExpr); ok {
			expression = selector.Sel
		}
		identifier, _ := ast.Unparen(expression).(*ast.Ident)
		return info.Uses[identifier]
	}
	aliases := map[types.Object]types.Object{}
	for _, initializer := range info.InitOrder {
		aliases[initializer.Lhs[0]] = named(initializer.Rhs)
	}
	declared := func(object types.Object) string {
		for depth := 0; depth < 8 && aliases[object] != nil; depth++ {
			object = aliases[object]
		}
		if object == nil || object.Pkg() == nil || object.Parent() != object.Pkg().Scope() {
			return ""
		}
		return object.Pkg().Path() + "." + object.Name()
	}
	current, calls, bodies := "", map[string][]*ast.CallExpr{}, map[string][]ast.Stmt{}
	ast.Inspect(lifecycle, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.FuncDecl:
			if current = node.Name.Name; node.Body != nil {
				bodies[current] = node.Body.List
			}
		case *ast.CallExpr:
			if declared(named(node.Fun)) == "localcell.readRegular" {
				calls[current] = append(calls[current], node)
			}
		case *ast.Ident:
			if name := declared(info.Uses[node]); slices.Contains(readers4Forbidden, name) {
				failed[name] = true
			}
		}
		return true
	})
	reread, first := calls["recoverPostgresRecord"], calls["postmaster"]
	fail("reread_call", len(reread) != 1 || len(first) != 1 || len(reread[0].Args) != 7 || len(first[0].Args) != 7 ||
		types.ExprString(reread[0].Args[0]) != "ctx" || types.ExprString(first[0].Args[0]) != "ctx")
	if !failed["reread_call"] {
		args, bound, body, valid := reread[0].Args[1:], named(reread[0].Args[2]), bodies["recoverPostgresRecord"], false
		fail("reread_opener", declared(named(args[0])) != "localcell.openPostgresReader")
		fail("reread_bound", declared(bound) == "" || bound != named(first[0].Args[2]))
		// The read uses one locally bound cause. Only the lease cut precedes its return.
		for index, cause := 1, named(args[3]); index+2 < len(body); index++ {
			define, _ := body[index-1].(*ast.AssignStmt)
			read, _ := body[index].(*ast.AssignStmt)
			check, _ := body[index+2].(*ast.IfStmt)
			if define == nil || read == nil || check == nil || read.Rhs[0] != reread[0] || len(read.Lhs) != 2 || len(check.Body.List) != 1 {
				continue
			}
			identifier, _ := define.Lhs[0].(*ast.Ident)
			result, _ := check.Body.List[0].(*ast.ReturnStmt)
			readErr := types.ExprString(read.Lhs[1])
			valid = admissionNodeText(body[index+1]) == "if cut := (*leaseWaitCut)(nil); errors.As("+readErr+", &cut) {\n\treturn cut\n}" &&
				cause != nil && info.Defs[identifier] == cause && named(args[4]) == cause && named(args[5]) == cause &&
				types.ExprString(define.Rhs[0]) == `fixedCause("cannot read PostgreSQL recovery identity")` &&
				declared(named(define.Rhs[0].(*ast.CallExpr).Fun)) == "localcell.fixedCause" && check.Init == nil && check.Else == nil &&
				types.ExprString(check.Cond) == readErr+" != nil" && result != nil && len(result.Results) == 1 && named(result.Results[0]) == cause
		}
		fail("reread_failure", !valid)
	}
	maps.DeleteFunc(failed, func(_ string, bad bool) bool { return !bad })
	return slices.Sorted(maps.Keys(failed))
}

// lifecycle.go references no read that can wait on or follow what stands at a
// name, and Repair's re-read keeps its form: each row reintroduces such a read
// by any spelling, or breaks one part of the form, and fails exactly its rules.
func TestReaders4LifecycleSourceContract(t *testing.T) {
	pkg, err := build.Default.ImportDir(".", 0)
	sources := map[string]string{}
	for _, name := range pkg.GoFiles {
		data, readErr := os.ReadFile(name)
		sources[name], err = strings.ReplaceAll(string(data), "\r\n", "\n"), errors.Join(err, readErr)
	}
	if err != nil || len(readers4Contract(sources)) != 0 {
		t.Fatal("lifecycle source contract refused the tree")
	}
	const anchor, path = "\tunreadable := fixedCause(", `c.path("pg", "postmaster.pid")`
	const call = "readRegular(ctx, openPostgresReader, postmasterBounded, " + path + ", unreadable"
	insert := func(code string) func(string) string {
		return func(source string) string { return strings.Replace(source, anchor, "\t"+code+"\n"+anchor, 1) }
	}
	replace := func(from, to string) func(string) string {
		return func(source string) string { return strings.Replace(source, from, to, 1) }
	}
	// Every mutant also holds a package variable declared as readPostgresFile;
	// only package_alias calls it, and shadowing_local declares its own name.
	for _, row := range []struct {
		name, want string
		change     func(string) string
	}{
		{"readPostgresFile", "localcell.readPostgresFile", insert("_, _ = readPostgresFile(" + path + ")")},
		{"readPrivateFile", "localcell.readPrivateFile", insert("_, _ = readPrivateFile(" + path + ")")},
		{"os.ReadFile", "os.ReadFile", insert("_, _ = os.ReadFile(" + path + ")")},
		{"os.Open", "os.Open", insert("_, _ = os.Open(" + path + ")")},
		{"os.OpenFile", "os.OpenFile", insert("_, _ = os.OpenFile(" + path + ", os.O_RDONLY, 0)")},
		{"openPrivateFile", "localcell.openPrivateFile", insert("_, _ = openPrivateFile(" + path + ", os.O_RDONLY, 0)")},
		{"renamed_import", "os.ReadFile", func(source string) string {
			return replace("import (\n", "import (\n\tstdos \"os\"\n")(insert("_, _ = stdos.ReadFile(" + path + ")")(source))
		}},
		{"function_value", "localcell.readPostgresFile", insert("read := readPostgresFile\n\t_, _ = read(" + path + ")")},
		{"package_alias", "localcell.readPostgresFile", insert("_, _ = readers4Read(" + path + ")")},
		{"shadowing_local", "", insert("readPostgresFile := func(string) ([]byte, error) { return nil, nil }\n\t_, _ = readPostgresFile(" + path + ")")},
		{"reread_plain", "localcell.readPostgresFile reread_call", replace(call+", unreadable, unreadable)", "readPostgresFile("+path+")")},
		{"reread_background", "reread_call", replace(call, strings.Replace(call, "(ctx,", "(context.Background(),", 1))},
		{"reread_cut_refused", "reread_failure", replace("return cut\n\t}\n\tif err != nil {\n\t\treturn unreadable", "return unreadable\n\t}\n\tif err != nil {\n\t\treturn unreadable")},
		{"reread_opener", "reread_opener", replace(call, strings.Replace(call, "openPostgresReader", "openPrivateReader", 1))},
		{"reread_bound_dropped", "reread_bound", replace(call, strings.Replace(call, "postmasterBounded", "nil", 1))},
		{"reread_text_changed", "reread_failure", replace("recovery identity\")", "recovery record\")")},
		{"reread_failure_returned_raw", "reread_failure", replace("\t\treturn unreadable\n", "\t\treturn err\n")},
		{"unresolved_reference", "types", insert("_ = readers4Unresolved")},
	} {
		t.Run(row.name, func(t *testing.T) {
			mutant := maps.Clone(sources)
			mutant["lifecycle.go"], mutant["readers4_alias.go"] = row.change(sources["lifecycle.go"]), "package localcell\n\nvar readers4Read = readPostgresFile\n"
			if mutant["lifecycle.go"] == sources["lifecycle.go"] || strings.Join(readers4Contract(mutant), " ") != row.want {
				t.Fatal("lifecycle source contract missed its mutation")
			}
		})
	}
}
