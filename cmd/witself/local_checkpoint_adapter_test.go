package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/localcell"
)

// lcaError panics if its text is consulted. Unwrap exposes cause; As claims
// the local cell's private cleanup failure, without a cause, when cleanup is set.
type lcaError struct {
	cause   error
	cleanup bool
}

func (lcaError) Error() string   { panic("raw error was consulted") }
func (e lcaError) Unwrap() error { return e.cause }
func (e lcaError) As(target any) bool {
	return e.cleanup && reflect.TypeOf(target).Elem().Name() == "checkpointCleanupError"
}

// lcaCell is every Cell its run's open returns, except open number failOpen,
// which fails. Inspect replays reads in order (the last repeats) and fails
// read number failRead; calls are recorded.
type lcaCell struct {
	t                 *testing.T
	reads             []localcell.Inspection
	failRead          int
	failOpen          int
	readErr, mutation error
	golden            string
	calls             []string
}

func (c *lcaCell) record(ctx context.Context, call string) int {
	if _, ok := ctx.Deadline(); !ok {
		c.t.Fatal("adapter dropped the context budget")
	}
	c.calls = append(c.calls, call)
	return strings.Count(strings.Join(c.calls, ","), "read")
}

func (c *lcaCell) Inspect(ctx context.Context) (localcell.Inspection, error) {
	n, inspection := c.record(ctx, "read"), lcaListed()
	if len(c.reads) > 0 {
		inspection = c.reads[min(n, len(c.reads))-1]
	}
	if n == c.failRead {
		return inspection, c.readErr
	}
	return inspection, nil
}

func (c *lcaCell) Checkpoint(ctx context.Context, options localcell.CheckpointOptions) error {
	c.record(ctx, "create "+options.InstallationID)
	return c.mutation
}

func (c *lcaCell) DeleteCheckpoint(ctx context.Context, options localcell.DeleteCheckpointOptions) error {
	c.record(ctx, "delete "+options.CheckpointID)
	return c.mutation
}

// run drives localCheckpointRun once with this cell behind the injected open.
func (c *lcaCell) run(t *testing.T, args []string, want int, calls string) lcWireReceipt {
	t.Helper()
	c.t, c.calls = t, nil
	opened := 0
	receipt := lcaRun(t, args, func(mode localCheckpointMode, _ localCheckpointRequest) (localCheckpointCell, error) {
		if mode != "cold-v1" {
			t.Fatal("adapter opened a cell without the selector")
		}
		if opened++; opened == c.failOpen {
			c.calls = append(c.calls, "unconstructed")
			return nil, lcaError{}
		}
		return c, nil
	}, want, c.golden)
	if strings.Join(c.calls, ",") != calls {
		t.Fatal("adapter call sequence differs")
	}
	if opened != len(c.calls) {
		t.Fatal("adapter opened cells and called the API differently")
	}
	return receipt
}

// lcaRun drives localCheckpointRun once and checks the raw receipt: a set
// golden equals stdout, stderr is empty, and lcaReceipt holds.
func lcaRun(t *testing.T, args []string, open func(localCheckpointMode, localCheckpointRequest) (localCheckpointCell, error), want int, golden string) (receipt lcWireReceipt) {
	t.Helper()
	var out, errOut bytes.Buffer
	if localCheckpointRun(context.Background(), args, open, &out, &errOut) != want {
		t.Fatal("adapter exit code differs")
	}
	raw := out.String()
	if golden != "" && raw != strings.ReplaceAll(golden, "NOTICE", lcNotice)+"\n" {
		t.Fatal("adapter golden receipt differs")
	}
	if errOut.Len() != 0 {
		t.Fatal("adapter wrote to stderr")
	}
	return lcaReceipt(t, raw)
}

// lcaReceipt checks a raw receipt: no sentinel, one LF-terminated line, no key
// repeated at any depth, the closed projection and its disclosures.
func lcaReceipt(t *testing.T, raw string) (receipt lcWireReceipt) {
	t.Helper()
	if strings.Contains(raw, lcSentinel) {
		t.Fatal("adapter receipt carries a sentinel")
	}
	if strings.Count(raw, "\n") != 1 || !strings.HasSuffix(raw, "\n") || strings.Contains(raw, "\r") {
		t.Fatal("adapter receipt framing differs")
	}
	if !lcaUniqueKeys(json.NewDecoder(strings.NewReader(raw))) {
		t.Fatal("adapter receipt repeats a key")
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&receipt) != nil || decoder.Decode(new(any)) != io.EOF {
		t.Fatal("adapter receipt projection differs")
	}
	if receipt.Verification != "not_performed" {
		t.Fatal("adapter receipt claims verification")
	}
	if receipt.Notice != lcNotice {
		t.Fatal("adapter receipt notice differs")
	}
	return receipt
}

// lcaUniqueKeys reads one JSON value and refuses a key repeated in any object.
func lcaUniqueKeys(decoder *json.Decoder) bool {
	token, err := decoder.Token()
	if err != nil || (token != json.Delim('{') && token != json.Delim('[')) {
		return err == nil
	}
	seen := map[string]bool{}
	for decoder.More() {
		if token == json.Delim('{') {
			key, err := decoder.Token()
			name, ok := key.(string)
			if err != nil || !ok || seen[name] {
				return false
			}
			seen[name] = true
		}
		if !lcaUniqueKeys(decoder) {
			return false
		}
	}
	_, err = decoder.Token()
	return err == nil
}

func lcaListed() localcell.Inspection {
	return localcell.Inspection{CheckpointListing: "listed", Manifest: &localcell.Manifest{InstallationID: lcIdentity}}
}

func lcaSummary(id string, complete bool) localcell.CheckpointSummary {
	if !complete {
		return localcell.CheckpointSummary{ID: id}
	}
	return localcell.CheckpointSummary{ID: id, InstallationID: lcIdentity, CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Bytes: 5, Complete: true}
}

// lcaRoot returns a fresh parent directory and its private, empty cell root.
func lcaRoot(t *testing.T) (parent, root string) {
	t.Helper()
	parent = t.TempDir()
	if root = filepath.Join(parent, "root"); lcaPrivateDir(root) != nil {
		t.Fatal("cannot create a private cell root")
	}
	return parent, root
}

// lcaTree records each name, type, permission bits and file content below
// dir; never printed.
func lcaTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	tree := map[string]string{}
	if filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		var info fs.FileInfo
		if err == nil {
			info, err = entry.Info()
		}
		if err == nil {
			var data []byte
			if entry.Type().IsRegular() {
				data, err = os.ReadFile(path)
			}
			tree[path] = info.Mode().String() + string(data)
		}
		return err
	}) != nil {
		t.Fatal("cannot snapshot the cell tree")
	}
	return tree
}

// The fence for TestLocalCheckpointNeverStopsOrRepairs/source-contract lives
// here because the command test file is at its density ceiling. lcFset and
// lcStdlib type-check standard packages from source, in process.
var lcFset = token.NewFileSet()
var lcStdlib = importer.ForCompiler(lcFset, "source", nil)

type lcImporter func(string) (*types.Package, error)

func (f lcImporter) Import(path string) (*types.Package, error) { return f(path) }

// Closed allowlists: the handler's package-level standard objects; the
// adapter's imports, selector and composite-key names, and handler names.
var lcHandlerUses = []string{"context.Context", "context.WithTimeout", "encoding/json.NewEncoder", "errors.New", "flag.ContinueOnError", "flag.NewFlagSet", "io.Discard", "io.WriteString", "io.Writer", "path/filepath.IsAbs", "regexp.MustCompile", "strings.Trim", "time.Duration", "time.ParseDuration", "time.Second", "time.Time"}
var lcAdapterImports = []string{"context", "errors", "io", "os", "os/signal", "strings", "syscall", "github.com/witwave-ai/witself/internal/localcell"}
var lcAdapterNames = []string{"Binaries", "Bytes", "Checkpoint", "CheckpointID", "CheckpointListing", "CheckpointOptions", "CheckpointUnknown", "Checkpoints", "Class", "ClassifyCheckpointFailure", "CleanupIncomplete", "Complete", "Config", "ContainsRune", "Context", "CreatedAt", "DeleteCheckpoint", "DeleteCheckpointOptions", "ID", "Inspect", "Inspection", "InstallationID", "Manifest", "New", "PostgresDir", "Reason", "Root", "Timeout", "Writer", "checkpointID", "entries", "installationID", "installationPresent", "pgbin", "root", "status", "timeout", "unknown"}
var lcAdapterHandler = []string{"localCheckpointCommand", "localCheckpointParse", "localCheckpointCallbacks", "localCheckpointRequest", "localCheckpointFailure", "localCheckpointObservation", "localCheckpointSummary"}

// The command sources that may import os/signal, closed (LC-S7D2F4B2R G-3).
var lcSignalFiles = []string{"dashboard.go", "local_checkpoint_adapter.go", "memory_curator_auto.go", "tui.go", "tui_console_child.go"}

// Signal names and objects, only inside localCheckpointSignals (pinned text).
var lcSignalNames = []string{"AfterFunc", "Background", "CancelFunc", "Interrupt", "NotifyContext", "SIGHUP", "SIGTERM"}
var lcSignalObjects = []string{"context.AfterFunc", "context.Background", "context.CancelFunc", "os.Interrupt", "os/signal.NotifyContext", "syscall.SIGHUP", "syscall.SIGTERM"}

const lcSignalsEntry = `func localCheckpointSignals() (context.Context, context.CancelFunc) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	context.AfterFunc(ctx, stop)
	return ctx, stop
}`

// The released route: run's one case body, usage's one help line, the namespace.
const lcRoute, lcHelp = "return localCheckpointNamespace(args[1:])", "  witself local checkpoint create|inspect|delete  Opt-in cold local-cell checkpoints (--checkpoint-mode=cold-v1); not restore-verified"
const lcNamespaceEntry = `func localCheckpointNamespace(args []string) int {
	if len(args) == 0 || args[0] != "checkpoint" {
		_, _ = io.WriteString(os.Stderr, "usage: witself local checkpoint create|inspect|delete --checkpoint-mode=cold-v1 --root=ABSOLUTE_PATH\n")
		return 2
	}
	ctx, stop := localCheckpointSignals()
	defer stop()
	return localCheckpointRun(ctx, args[1:], localCheckpointOpen, os.Stdout, os.Stderr)
}`

// Entry functions pinned by text, with the names and objects only they may use.
var lcEntries = []struct {
	name, text string
	admits     []string
}{{"localCheckpointSignals", lcSignalsEntry, slices.Concat(lcSignalNames, lcSignalObjects)},
	{"localCheckpointNamespace", lcNamespaceEntry, []string{"Stderr", "Stdout", "WriteString", "io.WriteString", "os.Stderr", "os.Stdout"}}}

// lcCheck type-checks files as package main, importing only admitted paths.
func lcCheck(files []*ast.File, admit func(string) bool) (*types.Scope, map[*ast.Ident]types.Object, []types.Error) {
	var errs []types.Error
	uses := map[*ast.Ident]types.Object{}
	conf := types.Config{Error: func(err error) { errs = append(errs, err.(types.Error)) }, Importer: lcImporter(func(path string) (*types.Package, error) {
		if !admit(path) {
			return nil, errors.New("import refused")
		}
		return lcStdlib.Import(path)
	})}
	pkg, _ := conf.Check("main", lcFset, files, &types.Info{Uses: uses})
	return pkg.Scope(), uses, errs
}

// lcFence returns the fence checks that non-test module sources, keyed by
// slash path, fail: resolved objects for the handler and adapter, explicit
// route and import assertions elsewhere, and the module linkname refusal.
func lcFence(module map[string]string) map[string]bool {
	const handler, adapter, cell = "cmd/witself/local_checkpoint_command.go", "cmd/witself/local_checkpoint_adapter.go", "github.com/witwave-ai/witself/internal/localcell"
	failed, files := map[string]bool{}, map[string]*ast.File{}
	fail := func(label string, bad bool) {
		if bad {
			failed[label] = true
		}
	}
	fail("linkname", !lcModuleDisconnected(module))
	for name, source := range module {
		if name == "cmd/witself/main.go" || strings.Contains(source, "localCheckpoint") || strings.Contains(source, cell) {
			file, err := parser.ParseFile(lcFset, name, source, parser.SkipObjectResolution)
			fail("isolation", err != nil)
			files[name] = file
		}
	}
	if failed["isolation"] || files[handler] == nil || files[adapter] == nil || files["cmd/witself/main.go"] == nil {
		return map[string]bool{"isolation": true}
	}
	// (a) The handler alone type-checks against closed standard objects.
	handlerAdmits := func(path string) bool {
		return slices.ContainsFunc(lcHandlerUses, func(use string) bool { return strings.HasPrefix(use, path+".") })
	}
	_, uses, errs := lcCheck([]*ast.File{files[handler]}, handlerAdmits)
	fail("isolation", len(errs) != 0)
	for _, object := range uses {
		fail("isolation", object.Pkg() != nil && object.Pkg().Path() != "main" && object.Parent() == object.Pkg().Scope() && !slices.Contains(lcHandlerUses, object.Pkg().Path()+"."+object.Name()))
	}
	// (b), (d) With the adapter, only the refused local cell import is left
	// unresolved; package-level names are reserved; handler names are closed.
	scope, uses, errs := lcCheck([]*ast.File{files[handler], files[adapter]}, func(path string) bool {
		return path != cell && (handlerAdmits(path) || slices.Contains(lcAdapterImports, path))
	})
	for _, err := range errs {
		fail("references", !slices.ContainsFunc(files[adapter].Imports, func(spec *ast.ImportSpec) bool {
			return spec.Path.Pos() == err.Pos && spec.Path.Value == strconv.Quote(cell)
		}))
	}
	for ident, object := range uses {
		fail("references", lcFset.File(ident.Pos()).Name() == adapter && object.Parent() == scope && lcFset.File(object.Pos()).Name() == handler && !slices.Contains(lcAdapterHandler, object.Name()))
	}
	for _, name := range scope.Names() {
		fail("prefix", !strings.HasPrefix(name, "localCheckpoint"))
	}
	// (c) Outside the two files only (g)'s route names a reserved identifier,
	// and no other command source imports the local cell package.
	reserved := 0
	for name, file := range files {
		if name != handler && name != adapter {
			ast.Inspect(file, func(node ast.Node) bool {
				if ident, ok := node.(*ast.Ident); ok && strings.HasPrefix(ident.Name, "localCheckpoint") {
					reserved++
					fail("outside", name != "cmd/witself/main.go" || ident.Name != "localCheckpointNamespace")
				}
				return true
			})
			fail("outside", path.Dir(name) == "cmd/witself" && slices.ContainsFunc(file.Imports, func(spec *ast.ImportSpec) bool {
				value, err := strconv.Unquote(spec.Path.Value)
				return err != nil || value == cell
			}))
		}
	}
	fail("outside", reserved != 1)
	// run's one caller is main in main.go: every other bare run in a command
	// source is a declaration, a field or key, or a value of a local that its
	// declaration defines. Only lcSignalFiles may import os/signal.
	calls := 0
	for name, source := range module {
		file, err := files[name], error(nil)
		if path.Dir(name) != "cmd/witself" {
			continue
		} else if file == nil {
			file, err = parser.ParseFile(lcFset, name, source, parser.SkipObjectResolution)
		}
		if fail("outside", err != nil); err != nil {
			continue
		}
		fail("route", !slices.Contains(lcSignalFiles, path.Base(name)) && slices.ContainsFunc(file.Imports, func(spec *ast.ImportSpec) bool {
			value, err := strconv.Unquote(spec.Path.Value)
			return err != nil || value == "os/signal"
		}))
		for _, decl := range file.Decls {
			function, _ := decl.(*ast.FuncDecl)
			named, called, local := map[*ast.Ident]bool{}, map[*ast.Ident]bool{}, false
			ast.Inspect(decl, func(node ast.Node) bool {
				var defines []*ast.Ident
				switch node := node.(type) {
				case *ast.SelectorExpr:
					named[node.Sel] = true
				case *ast.KeyValueExpr:
					key, _ := node.Key.(*ast.Ident)
					named[key] = true
				case *ast.CallExpr:
					fun, _ := ast.Unparen(node.Fun).(*ast.Ident)
					called[fun] = true
				case *ast.Field:
					defines = node.Names
				case *ast.ValueSpec:
					defines = node.Names
				case *ast.AssignStmt:
					for _, lhs := range node.Lhs {
						if ident, ok := lhs.(*ast.Ident); ok && node.Tok == token.DEFINE {
							defines = append(defines, ident)
						}
					}
				case *ast.RangeStmt:
					for _, expr := range []ast.Expr{node.Key, node.Value} {
						if ident, ok := expr.(*ast.Ident); ok && node.Tok == token.DEFINE {
							defines = append(defines, ident)
						}
					}
				}
				for _, ident := range defines {
					named[ident], local = true, local || ident.Name == "run"
				}
				return true
			})
			ast.Inspect(decl, func(node ast.Node) bool {
				switch ident, ok := node.(*ast.Ident); {
				case !ok || ident.Name != "run" || named[ident] || function != nil && ident == function.Name:
				case called[ident]:
					calls++
					fail("outside", function == nil || function.Recv != nil || function.Name.Name != "main" || name != "cmd/witself/main.go")
				default:
					fail("outside", !local)
				}
				return true
			})
		}
	}
	fail("outside", calls != 1)
	// (e) Imports, selectors and composite keys are closed; each entry's names
	// and objects lie in it alone; the signal context's one use is the namespace.
	decls := make([]ast.Decl, len(lcEntries))
	for index, entry := range lcEntries {
		object := scope.Lookup(entry.name)
		at := slices.IndexFunc(files[adapter].Decls, func(decl ast.Decl) bool {
			function, ok := decl.(*ast.FuncDecl)
			return ok && object != nil && function.Name.Pos() == object.Pos()
		})
		if fail("entry", at < 0); at >= 0 {
			decls[index] = files[adapter].Decls[at]
			start, end := lcFset.Position(decls[index].Pos()).Offset, lcFset.Position(decls[index].End()).Offset
			fail("entry", strings.ReplaceAll(module[adapter][start:end], "\r\n", "\n") != entry.text)
		}
	}
	inside := func(pos token.Pos, index int) bool {
		return decls[index] != nil && decls[index].Pos() <= pos && pos < decls[index].End()
	}
	admitted := func(pos token.Pos, value string) bool { // names never hold a dot, objects always do
		return inside(pos, 0) && slices.Contains(lcEntries[0].admits, value) || inside(pos, 1) && slices.Contains(lcEntries[1].admits, value)
	}
	signals := 0
	for ident, object := range uses {
		if object != nil && object == scope.Lookup(lcEntries[0].name) {
			signals++
			fail("references", !inside(ident.Pos(), 1))
		}
		if lcFset.File(ident.Pos()).Name() == adapter && object.Pkg() != nil {
			name := object.Pkg().Path() + "." + object.Name()
			confined := object.Pkg().Path() == "os/signal" || object.Pkg().Path() == "syscall" || slices.Contains(lcEntries[0].admits, name) || slices.Contains(lcEntries[1].admits, name)
			fail("allowlist", confined && !admitted(ident.Pos(), name))
		}
	}
	fail("references", signals != 1)
	for _, spec := range files[adapter].Imports {
		value, err := strconv.Unquote(spec.Path.Value)
		fail("allowlist", err != nil || spec.Name != nil || !slices.Contains(lcAdapterImports, value))
	}
	ast.Inspect(files[adapter], func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.SelectorExpr:
			fail("allowlist", !slices.Contains(lcAdapterNames, node.Sel.Name) && !admitted(node.Pos(), node.Sel.Name))
		case *ast.KeyValueExpr:
			key, ok := node.Key.(*ast.Ident)
			fail("allowlist", !ok || !slices.Contains(lcAdapterNames, key.Name))
		}
		return true
	})
	// (f) Neither file declares init, and the adapter declares no package
	// variable, whose initializer would run before main. (g) main.go imports
	// no os/signal or syscall; run and usage each hold one folded constant equal
	// to local or containing checkpoint: the sole literal of one case whose body
	// is the pinned route, and the pinned help line. main and main.go's
	// package-level declarations hold none (LC-S7D2F4B2R G-1).
	for _, decl := range append(slices.Clone(files[handler].Decls), files[adapter].Decls...) {
		function, ok := decl.(*ast.FuncDecl)
		fail("init", ok && function.Name.Name == "init")
		general, ok := decl.(*ast.GenDecl)
		fail("variable", ok && general.Tok == token.VAR && lcFset.File(decl.Pos()).Name() == adapter)
	}
	for _, spec := range files["cmd/witself/main.go"].Imports {
		fail("route", spec.Path.Value == `"os/signal"` || spec.Path.Value == `"syscall"`)
	}
	routes, clauses, found := 0, 0, map[string][]string{}
	for _, decl := range files["cmd/witself/main.go"].Decls {
		if function, ok := decl.(*ast.FuncDecl); ok && function.Recv == nil && function.Body != nil && slices.Contains([]string{"run", "usage", "main"}, function.Name.Name) {
			routes++
			ast.Inspect(function.Body, func(node ast.Node) bool {
				if value, constant := lcStringConstant(node); constant && (value == "local" || strings.Contains(value, "checkpoint")) {
					found[function.Name.Name] = append(found[function.Name.Name], value)
				}
				if clause, ok := node.(*ast.CaseClause); ok && slices.ContainsFunc(clause.List, func(value ast.Expr) bool {
					local, constant := lcStringConstant(value)
					return constant && local == "local"
				}) {
					literal, ok := clause.List[0].(*ast.BasicLit)
					clauses++
					fail("route", !ok || literal.Value != `"local"` || len(clause.List) != 1 || len(clause.Body) != 1 ||
						strings.ReplaceAll(module["cmd/witself/main.go"][lcFset.Position(clause.Body[0].Pos()).Offset:lcFset.Position(clause.Body[0].End()).Offset], "\r\n", "\n") != lcRoute)
				}
				return true
			})
		} else if general, ok := decl.(*ast.GenDecl); ok && general.Tok != token.IMPORT {
			ast.Inspect(general, func(node ast.Node) bool {
				value, constant := lcStringConstant(node)
				fail("route", constant && (value == "local" || strings.Contains(value, "checkpoint")))
				return true
			})
		}
	}
	fail("route", routes != 3 || clauses != 1 || found["main"] != nil || !slices.Equal(found["run"], []string{"local"}) || !slices.Equal(found["usage"], []string{lcHelp}))
	return failed
}

func TestLocalCheckpointAdapter(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	t.Run("disabled-direct", func(t *testing.T) {
		parent, root := lcaRoot(t)
		before, pgbin := lcaTree(t, parent), filepath.Join(parent, "bin")
		// The literal, never the constant it guards, is the expectation.
		for _, r := range []localCheckpointRequest{{root: root, pgbin: pgbin}, {root: root + "\x00", pgbin: pgbin}, {root: root, pgbin: pgbin + "\x00"}} {
			for _, mode := range []localCheckpointMode{"", "cold-v", "cold-v1 ", "COLD-V1", "cold-v1"} {
				cell, err := localCheckpointOpen(mode, r)
				if (err == nil) != (mode == "cold-v1" && r.root == root && r.pgbin == pgbin) || (cell == nil) == (err == nil) {
					t.Fatal("adapter construction differs from the selector")
				}
			}
		}
		if !maps.Equal(before, lcaTree(t, parent)) {
			t.Fatal("refused construction changed the root")
		}
	})
	t.Run("create", func(t *testing.T) {
		after := lcaListed()
		after.Checkpoints = []localcell.CheckpointSummary{lcaSummary(lcID, true), lcaSummary(lcOtherID, true)}
		cell := &lcaCell{reads: []localcell.Inspection{lcaListed(), after}, golden: `{"operation":"create","outcome":"succeeded","before":{"status":"listed","installation_id":"installation_A","count":0,"unknown_count":0},"after":{"status":"listed","installation_id":"installation_A","count":2,"unknown_count":0,"entries":[{"id":"ckpt_abcdefghijklmnop","installation_id":"installation_A","complete":true,"verification":"not_performed"},{"id":"ckpt_bcdefghijklmnopq","installation_id":"installation_A","complete":true,"verification":"not_performed"}]},"verification":"not_performed","notice":"NOTICE"}`}
		cell.run(t, lcArgs("create"), 0, "read,create "+lcIdentity+",read")
		(&lcaCell{failOpen: 2, golden: `{"operation":"create","outcome":"failed","failure":{"class":"local_checkpoint_operation_failed","cleanup_incomplete":false},"before":{"status":"listed","installation_id":"installation_A","count":0,"unknown_count":0},"after":{"status":"listed","installation_id":"installation_A","count":0,"unknown_count":0},"verification":"not_performed","notice":"NOTICE"}`}).run(t, lcArgs("create"), 1, "read,unconstructed,read")
		for _, mode := range []localCheckpointMode{localCheckpointColdV1, "", "cold-v"} {
			var opened []localCheckpointMode
			callbacks := localCheckpointCallbacksFor(mode, func(selected localCheckpointMode, _ localCheckpointRequest) (localCheckpointCell, error) {
				opened = append(opened, selected)
				return nil, lcaError{}
			})
			if callbacks.create == nil || callbacks.read == nil || callbacks.delete == nil {
				t.Fatal("adapter callback missing")
			}
			_, failure := callbacks.read(t.Context(), localCheckpointRequest{})
			if failure.err == nil || callbacks.create(t.Context(), localCheckpointRequest{}).err == nil || callbacks.delete(t.Context(), localCheckpointRequest{}).err == nil {
				t.Fatal("adapter callback hid a failed construction")
			}
			if !slices.Equal(opened, []localCheckpointMode{mode, mode, mode}) {
				t.Fatal("adapter opened a cell with another mode")
			}
		}
	})
	t.Run("inspect", func(t *testing.T) {
		parent, root := lcaRoot(t)
		before, args := lcaTree(t, parent), []string{"inspect", "--checkpoint-mode=cold-v1", "--root=" + root}
		lcaRun(t, args, localCheckpointOpen, 0, `{"operation":"inspect","outcome":"succeeded","after":{"status":"listed","count":0,"unknown_count":0},"verification":"not_performed","notice":"NOTICE"}`)
		unavailable := `{"operation":"inspect","outcome":"refused","failure":FAILURE,"after":{"status":"unavailable","failure":FAILURE},"verification":"not_performed","notice":"NOTICE"}`
		lcaRun(t, lcReplace(args, "root", filepath.Join(parent, "missing", "root")), localCheckpointOpen, 1, strings.ReplaceAll(unavailable, "FAILURE", `{"class":"local_checkpoint_operation_failed","cleanup_incomplete":false}`))
		if !maps.Equal(before, lcaTree(t, parent)) {
			t.Fatal("inspect changed the real root")
		}
		stray := filepath.Join(root, "recovery", "stray")
		if lcaPrivateFile(filepath.Join(root, ".owner.lock"), nil) != nil || lcaPrivateDir(filepath.Join(root, "recovery")) != nil || os.WriteFile(stray, nil, 0o644) != nil || os.Chmod(stray, 0o644) != nil {
			t.Fatal("cannot plant a refused recovery entry")
		}
		lcaRun(t, args, localCheckpointOpen, 1, strings.ReplaceAll(unavailable, "FAILURE", `{"class":"local_checkpoint_state_unsupported","reason":"descendant_refused","cleanup_incomplete":false}`))
		listed := lcaListed()
		listed.Manifest.Database, listed.Manifest.Role, listed.Manifest.AccountID, listed.Manifest.ServerVersion = lcSentinel, lcSentinel, lcSentinel, lcSentinel
		listed.Processes, listed.Interrupted = map[string]localcell.ProcessState{lcSentinel: {PID: 4242, Alive: true, Matches: true}}, &localcell.Interruption{Operation: lcSentinel, Step: lcSentinel}
		listed.InstallState, listed.PrincipalTopology, listed.CheckpointUnknown = lcSentinel, lcSentinel, 3
		listed.Checkpoints = []localcell.CheckpointSummary{lcaSummary(lcID, true), lcaSummary(lcOtherID, false)}
		(&lcaCell{reads: []localcell.Inspection{listed}, golden: `{"operation":"inspect","outcome":"succeeded","after":{"status":"listed","installation_id":"installation_A","count":2,"unknown_count":3,"entries":[{"id":"ckpt_abcdefghijklmnop","installation_id":"installation_A","complete":true,"verification":"not_performed"},{"id":"ckpt_bcdefghijklmnopq","complete":false,"verification":"not_performed"}]},"verification":"not_performed","notice":"NOTICE"}`}).run(t, lcArgs("inspect"), 0, "read")
		repeated, unsafe := lcaListed(), lcaListed()
		repeated.Checkpoints, unsafe.Manifest.InstallationID = []localcell.CheckpointSummary{lcaSummary(lcID, true), lcaSummary(lcID, false)}, lcSentinel+"!"
		for failure, cell := range map[string]*lcaCell{
			`{"class":"local_checkpoint_operation_failed","cleanup_incomplete":false}`:     {reads: []localcell.Inspection{repeated}},
			`{"class":"local_checkpoint_metadata_unsupported","cleanup_incomplete":false}`: {reads: []localcell.Inspection{unsafe}},
			`{"class":"local_cell_locked","cleanup_incomplete":false}`:                     {failRead: 1, readErr: lcaError{cause: localcell.ErrLocked}},
		} {
			cell.golden = strings.ReplaceAll(unavailable, "FAILURE", failure)
			cell.run(t, lcArgs("inspect"), 1, "read")
		}
		if lcaUniqueKeys(json.NewDecoder(strings.NewReader(`{"after":{"count":0,"count":1}}`))) {
			t.Fatal("repeated receipt key admitted")
		}
		// Three entries, a fake's and then real ones: inspect lists each, opens
		// once, calls no mutation, takes no lock and leaves every one in place.
		// The third is planted after the fake's run: a re-entered read sees two.
		parent, root = lcaRoot(t)
		args, many := lcReplace(args, "root", root), localcell.Inspection{CheckpointListing: "listed"}
		plant := func(id string) {
			if lcaPrivateDir(filepath.Join(root, "recovery", id)) != nil || lcaPrivateFile(filepath.Join(root, "recovery", id, "inventory.json"), []byte("{}")) != nil {
				t.Fatal("cannot plant three checkpoints")
			}
		}
		if lcaPrivateFile(filepath.Join(root, ".owner.lock"), nil) != nil || lcaPrivateDir(filepath.Join(root, "recovery")) != nil {
			t.Fatal("cannot plant three checkpoints")
		}
		for _, id := range []string{lcID, lcOtherID, "ckpt_cdefghijklmnopqr"} {
			many.Checkpoints = append(many.Checkpoints, lcaSummary(id, false))
		}
		plant(lcID)
		plant(lcOtherID)
		before, golden := lcaTree(t, parent), `{"operation":"inspect","outcome":"succeeded","after":{"status":"listed","count":3,"unknown_count":0,"entries":[{"id":"ckpt_abcdefghijklmnop","complete":false,"verification":"not_performed"},{"id":"ckpt_bcdefghijklmnopq","complete":false,"verification":"not_performed"},{"id":"ckpt_cdefghijklmnopqr","complete":false,"verification":"not_performed"}]},"verification":"not_performed","notice":"NOTICE"}`
		(&lcaCell{reads: []localcell.Inspection{many}, golden: golden}).run(t, args, 0, "read")
		if !maps.Equal(before, lcaTree(t, parent)) {
			t.Fatal("inspect changed the real root")
		}
		plant("ckpt_cdefghijklmnopqr")
		before = lcaTree(t, parent)
		lcaRun(t, args, localCheckpointOpen, 0, golden)
		if !maps.Equal(before, lcaTree(t, parent)) {
			t.Fatal("inspect changed the real root")
		}
	})
	t.Run("exact-delete", func(t *testing.T) {
		present, consent := lcaListed(), lcArgs("delete")
		present.Checkpoints = []localcell.CheckpointSummary{lcaSummary(lcID, true)}
		for _, row := range []struct {
			after    localcell.Inspection
			mutation error
			want     int
			presence string
		}{{lcaListed(), nil, 0, "absent"}, {present, nil, 0, "present"}, {lcaListed(), localcell.ErrLocked, 1, "absent"}, {present, localcell.ErrLocked, 1, "present"}} {
			r := (&lcaCell{reads: []localcell.Inspection{present, row.after}, mutation: row.mutation}).run(t, consent, row.want, "read,delete "+lcID+",read")
			if r.Target != lcID {
				t.Fatal("delete target differs from the consent")
			}
			if r.Presence != row.presence {
				t.Fatal("delete presence differs from the after-read")
			}
		}
		listed := `{"status":"listed","installation_id":"installation_A","count":1,"unknown_count":0,"entries":[{"id":"ckpt_abcdefghijklmnop","installation_id":"installation_A","complete":true,"verification":"not_performed"}]}`
		(&lcaCell{reads: []localcell.Inspection{present}, failOpen: 2, golden: `{"operation":"delete","outcome":"failed","failure":{"class":"local_checkpoint_operation_failed","cleanup_incomplete":false},"before":` + listed + `,"after":` + listed + `,"target":"ckpt_abcdefghijklmnop","presence":"present","verification":"not_performed","notice":"NOTICE"}`}).run(t, consent, 1, "read,unconstructed,read")
		for _, args := range [][]string{consent[:3], append(slices.Clone(consent), "--checkpoint-id="+lcID), append(slices.Clone(consent), "--checkpoint-id="+lcOtherID)} {
			(&lcaCell{}).run(t, args, 2, "")
		}
		for _, id := range []string{"", "ckpt_abc", "ckpt_abcdefghijklmno", lcID + "*", "ckpt_*", "latest", "oldest", "all", "ckpt_Abcdefghijklmnop"} {
			(&lcaCell{}).run(t, lcReplace(consent, "checkpoint-id", id), 2, "")
		}
	})
	t.Run("error-after-witness", func(t *testing.T) {
		after := lcaListed()
		after.Checkpoints = []localcell.CheckpointSummary{lcaSummary(lcID, true)}
		golden := `{"operation":"create","outcome":"failed","failure":FAILURE,"before":{"status":"listed","installation_id":"installation_A","count":0,"unknown_count":0},"after":{"status":"listed","installation_id":"installation_A","count":1,"unknown_count":0,"entries":[{"id":"ckpt_abcdefghijklmnop","installation_id":"installation_A","complete":true,"verification":"not_performed"}]},"verification":"not_performed","notice":"NOTICE"}`
		for failure, err := range map[string]error{
			`{"class":"local_cell_locked","cleanup_incomplete":false}`:                lcaError{cause: localcell.ErrLocked},
			`{"class":"local_checkpoint_operation_failed","cleanup_incomplete":true}`: lcaError{cleanup: true},
		} {
			cell := &lcaCell{reads: []localcell.Inspection{lcaListed(), after}, mutation: err, golden: strings.ReplaceAll(golden, "FAILURE", failure)}
			cell.run(t, lcArgs("create"), 1, "read,create "+lcIdentity+",read")
		}
	})
	t.Run("busy", func(t *testing.T) {
		for verb, call := range map[string]string{"create": "create " + lcIdentity, "delete": "delete " + lcID} {
			for _, status := range []string{"busy", "unowned"} {
				observed := localcell.Inspection{CheckpointListing: status, Locked: status == "busy"}
				r := (&lcaCell{reads: []localcell.Inspection{observed}}).run(t, lcArgs(verb), 1, "read")
				if r.Outcome != "refused" || r.Failure != nil {
					t.Fatal("unobserved cell reported a mutation result")
				}
				if r.Before == nil || r.Before.Status != status {
					t.Fatal("before-read status differs")
				}
				r = (&lcaCell{reads: []localcell.Inspection{lcaListed(), observed}}).run(t, lcArgs(verb), 1, "read,"+call+",read")
				if r.Outcome != "succeeded" || r.Failure != nil {
					t.Fatal("busy after-read overwrote the success")
				}
				if r.After == nil || r.After.Status != status {
					t.Fatal("after-read status differs")
				}
			}
			r := (&lcaCell{mutation: localcell.ErrLocked}).run(t, lcArgs(verb), 1, "read,"+call+",read")
			if r.Outcome != "failed" || r.Failure == nil || r.Failure.Class != "local_cell_locked" {
				t.Fatal("locked mutation class differs")
			}
		}
	})
	t.Run("maintenance", func(t *testing.T) {
		parent, root := lcaRoot(t)
		for _, dir := range []string{"journal", "recovery", filepath.Join("recovery", lcID)} {
			if lcaPrivateDir(filepath.Join(root, dir)) != nil {
				t.Fatal("cannot plant the fenced cell")
			}
		}
		for name, data := range map[string]string{".owner.lock": "", filepath.Join("journal", "maintenance.json"): "{}", filepath.Join("recovery", lcID, "inventory.json"): "{}"} {
			if lcaPrivateFile(filepath.Join(root, name), []byte(data)) != nil {
				t.Fatal("cannot plant the fenced cell")
			}
		}
		before := lcaTree(t, parent)
		listed := `{"status":"listed","count":1,"unknown_count":0,"entries":[{"id":"ckpt_abcdefghijklmnop","complete":false,"verification":"not_performed"}]}`
		lcaRun(t, []string{"delete", "--checkpoint-mode=cold-v1", "--root=" + root, "--checkpoint-id=" + lcID}, localCheckpointOpen, 1, `{"operation":"delete","outcome":"failed","failure":{"class":"local_maintenance_fence_active","cleanup_incomplete":false},"before":`+listed+`,"after":`+listed+`,"target":"ckpt_abcdefghijklmnop","presence":"present","verification":"not_performed","notice":"NOTICE"}`)
		// A fence beside a missing lock name defers to own(false): exactly run/
		// and run/operation.lock are added (OQ1(a)), nothing else changes. They
		// are recorded as the private directory and empty lock file planted here.
		want := maps.Clone(before)
		want[filepath.Join(root, "run")], want[filepath.Join(root, "run", "operation.lock")] = before[filepath.Join(root, "journal")], before[filepath.Join(root, ".owner.lock")]
		if !maps.Equal(want, lcaTree(t, parent)) {
			t.Fatal("fenced delete changed the root beyond its two run names")
		}
	})
	t.Run("lockless", func(t *testing.T) {
		parent, root := lcaRoot(t)
		before := lcaTree(t, parent)
		lcaRun(t, []string{"delete", "--checkpoint-mode=cold-v1", "--root=" + root, "--checkpoint-id=" + lcID}, localCheckpointOpen, 1, `{"operation":"delete","outcome":"failed","failure":{"class":"local_checkpoint_identity_mismatch","cleanup_incomplete":false},"before":{"status":"listed","count":0,"unknown_count":0},"after":{"status":"listed","count":0,"unknown_count":0},"target":"ckpt_abcdefghijklmnop","presence":"absent","verification":"not_performed","notice":"NOTICE"}`)
		if !maps.Equal(before, lcaTree(t, parent)) {
			t.Fatal("refused delete created a lock name in a fresh private root")
		}
	})
	// A never-cell root holding a 0755 tmp: refused for its layout, nothing
	// created. On Windows the mode is no DACL: the inherited one is refused by
	// the private-tree validator with a cause outside the class table.
	t.Run("never-cell-layout", func(t *testing.T) {
		parent, root := lcaRoot(t)
		if os.Mkdir(filepath.Join(root, "tmp"), 0o755) != nil || os.Chmod(filepath.Join(root, "tmp"), 0o755) != nil {
			t.Fatal("cannot plant a shared directory in the root")
		}
		failure := `{"class":"` + map[bool]string{false: "local_cell_layout_refused", true: "local_checkpoint_operation_failed"}[runtime.GOOS == "windows"] + `","cleanup_incomplete":false}`
		before := lcaTree(t, parent)
		lcaRun(t, []string{"delete", "--checkpoint-mode=cold-v1", "--root=" + root, "--checkpoint-id=" + lcID}, localCheckpointOpen, 1, `{"operation":"delete","outcome":"refused","failure":`+failure+`,"before":{"status":"unavailable","failure":`+failure+`},"target":"ckpt_abcdefghijklmnop","presence":"unknown","verification":"not_performed","notice":"NOTICE"}`)
		if !maps.Equal(before, lcaTree(t, parent)) {
			t.Fatal("refused delete changed a never-cell root")
		}
	})
	// Incomplete only on proof: an empty witness and a directory at its name
	// list "complete": false, a valid one complete. One that cannot be read
	// (Windows: held without read sharing; macOS: a deny-read ACL entry) is
	// never listed: inspect and a delete of it or of another are refused,
	// nothing changed; released, it lists complete again.
	t.Run("witness", func(t *testing.T) {
		parent, root := lcaRoot(t)
		witness, args := filepath.Join(root, "recovery", lcID, "witness.json"), []string{"inspect", "--checkpoint-mode=cold-v1", "--root=" + root}
		if lcaPrivateFile(filepath.Join(root, ".owner.lock"), nil) != nil || lcaPrivateDir(filepath.Join(root, "recovery")) != nil ||
			lcaPrivateDir(filepath.Dir(witness)) != nil || lcaPrivateFile(witness, nil) != nil {
			t.Fatal("cannot plant the checkpoint witness")
		}
		incomplete := `{"operation":"inspect","outcome":"succeeded","after":{"status":"listed","count":1,"unknown_count":0,"entries":[{"id":"ckpt_abcdefghijklmnop","complete":false,"verification":"not_performed"}]},"verification":"not_performed","notice":"NOTICE"}`
		lcaRun(t, args, localCheckpointOpen, 0, incomplete)
		if os.Remove(witness) != nil || lcaPrivateDir(witness) != nil {
			t.Fatal("cannot plant a directory at the witness name")
		}
		lcaRun(t, args, localCheckpointOpen, 0, incomplete)
		if os.Remove(witness) != nil || lcaPrivateFile(witness, []byte(`{"format":1,"checkpoint_id":"ckpt_abcdefghijklmnop","installation_id":"installation_A","created_at":"2026-01-01T00:00:00Z","bytes":5}`)) != nil {
			t.Fatal("cannot plant a valid witness")
		}
		complete := strings.Replace(incomplete, `"complete":false`, `"installation_id":"installation_A","complete":true`, 1)
		lcaRun(t, args, localCheckpointOpen, 0, complete)
		before := lcaTree(t, parent)
		release := lcaUnreadable(t, witness)
		if release == nil {
			return
		}
		failure := `{"class":"local_checkpoint_operation_failed","cleanup_incomplete":false}`
		lcaRun(t, args, localCheckpointOpen, 1, `{"operation":"inspect","outcome":"refused","failure":`+failure+`,"after":{"status":"unavailable","failure":`+failure+`},"verification":"not_performed","notice":"NOTICE"}`)
		for _, id := range []string{lcID, lcOtherID} {
			lcaRun(t, []string{"delete", "--checkpoint-mode=cold-v1", "--root=" + root, "--checkpoint-id=" + id}, localCheckpointOpen, 1, `{"operation":"delete","outcome":"refused","failure":`+failure+`,"before":{"status":"unavailable","failure":`+failure+`},"target":"`+id+`","presence":"unknown","verification":"not_performed","notice":"NOTICE"}`)
		}
		release()
		if !maps.Equal(before, lcaTree(t, parent)) {
			t.Fatal("a refusal beside an unreadable witness changed the root")
		}
		lcaRun(t, args, localCheckpointOpen, 0, complete)
	})
}

// lcsCell: the verb's own call writes blocked and waits for the cancellation
// (else unsignalled after 20 s); then create fails, and inspect and delete
// succeed, as the API does. second-exits writes cancelled and holds.
type lcsCell struct{ verb, call string }

func (c *lcsCell) wait(ctx context.Context, call string) {
	if call != c.call {
		return
	}
	_, _ = io.WriteString(os.Stderr, "blocked\n")
	select {
	case <-ctx.Done():
	case <-time.After(20 * time.Second):
		_, _ = io.WriteString(os.Stderr, "unsignalled\n")
		return
	}
	if c.verb == "second-exits" {
		_, _ = io.WriteString(os.Stderr, "cancelled\n")
		time.Sleep(20 * time.Second)
	}
}

func (c *lcsCell) Inspect(ctx context.Context) (localcell.Inspection, error) {
	c.wait(ctx, "inspect")
	listed := lcaListed()
	if c.verb == "delete" && c.call != "" {
		listed.Checkpoints = []localcell.CheckpointSummary{lcaSummary(lcID, true)}
	}
	return listed, nil
}

func (c *lcsCell) Checkpoint(ctx context.Context, _ localcell.CheckpointOptions) error {
	c.wait(ctx, "create")
	return errors.New("checkpoint cancelled")
}

func (c *lcsCell) DeleteCheckpoint(ctx context.Context, _ localcell.DeleteCheckpointOptions) error {
	c.wait(ctx, "delete")
	c.call = ""
	return nil
}

// TestLocalCheckpointSignalHelperProcess is TestLocalCheckpointSignal's child
// when its last two arguments are local-checkpoint-signal-helper and a verb.
func TestLocalCheckpointSignalHelperProcess(_ *testing.T) {
	if len(os.Args) < 3 || os.Args[len(os.Args)-2] != "local-checkpoint-signal-helper" {
		return
	}
	cell := &lcsCell{verb: os.Args[len(os.Args)-1], call: strings.Replace(os.Args[len(os.Args)-1], "second-exits", "create", 1)}
	ctx, stop := localCheckpointSignals()
	code := localCheckpointRun(ctx, lcArgs(cell.call), func(localCheckpointMode, localCheckpointRequest) (localCheckpointCell, error) { return cell, nil }, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// TestLocalCheckpointSignal signals the command child once its call blocks: it
// ends on its own with one clean receipt; for second-exits a signal repeated
// every 50 ms (at most 10 s) ends it by that signal, with no receipt.
func TestLocalCheckpointSignal(t *testing.T) {
	for _, verb := range []string{"create", "delete", "inspect", "second-exits"} {
		t.Run(verb, func(t *testing.T) {
			for _, name := range lcsNames {
				t.Run(name, func(t *testing.T) {
					ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
					defer cancel()
					child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLocalCheckpointSignalHelperProcess$", "--", "local-checkpoint-signal-helper", verb)
					// A coverage build writes its data on exit, here into the row's own directory.
					child.Env = []string{"WITSELF_HOME=" + t.TempDir(), "DSH_HOME=" + t.TempDir(), "GOCOVERDIR=" + t.TempDir()}
					var stdout bytes.Buffer
					child.Stdout = &stdout
					stderr, err := child.StderrPipe()
					if err != nil || lcsStart(child, name) != nil {
						t.Fatal("cannot start the signal helper")
					}
					t.Cleanup(func() {
						_ = child.Process.Kill()
						_ = child.Wait()
					})
					reader := bufio.NewReader(stderr)
					if line, err := reader.ReadString('\n'); err != nil || line != "blocked\n" {
						t.Fatal("the signal helper did not block in its call")
					}
					if lcsSend(child.Process, name) != nil {
						t.Fatal("cannot signal the signal helper")
					}
					if verb == "second-exits" {
						if line, err := reader.ReadString('\n'); err != nil || line != "cancelled\n" {
							t.Fatal("the first signal did not cancel the call")
						}
					}
					// The rest of stderr must be empty. The child is not reaped while
					// signals are sent, so its identifier cannot name another process.
					ended := make(chan bool, 1)
					go func() {
						rest, err := io.ReadAll(reader)
						ended <- err == nil && len(rest) == 0
					}()
					var clean bool
					for deadline, done := time.Now().Add(10*time.Second), false; !done; {
						select {
						case clean = <-ended:
							done = true
						case <-time.After(50 * time.Millisecond):
							if time.Now().After(deadline) {
								t.Fatal("the signalled command did not end within its bound")
							}
							if verb == "second-exits" {
								_ = lcsSend(child.Process, name)
							}
						}
					}
					_ = child.Wait()
					if verb == "second-exits" {
						if !clean || stdout.Len() != 0 || !lcsEndedBy(child.ProcessState, name) {
							t.Fatal("a further signal did not end the command by that signal")
						}
						return
					}
					if !clean || child.ProcessState.ExitCode() != map[string]int{"create": 1}[verb] {
						t.Fatal("the signalled command did not end on its own")
					}
					r := lcaReceipt(t, stdout.String())
					switch {
					case verb == "create" && (r.Outcome != "failed" || r.Failure == nil || r.Failure.Class != "local_checkpoint_operation_failed" || r.After == nil || r.After.Status != "listed"):
						t.Fatal("a signalled create did not fail with its after-read")
					case verb == "delete" && (r.Outcome != "succeeded" || r.Failure != nil || r.Target != lcID || r.Before == nil || r.Before.Count == nil || *r.Before.Count != 1 || r.Presence != "absent"):
						t.Fatal("a signalled delete did not finish with its after-read")
					case verb == "inspect" && (r.Outcome != "succeeded" || r.After == nil || r.After.Status != "listed"):
						t.Fatal("a signalled inspect did not list")
					}
				})
			}
		})
	}
}

// TestLocalCheckpointDispatch drives run: without the exact selector (also with
// WITSELF_CHECKPOINT_MODE set) each spelling exits 2 with a fixed class or the
// usage line, echoes nothing and leaves a real root unchanged; with it an empty
// root lists, repeated consent is refused, and notice and help are documented.
func TestLocalCheckpointDispatch(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	refused := func(t *testing.T, root string, args []string, class string) {
		t.Helper()
		before := lcaTree(t, filepath.Dir(root))
		stdout, stderr, code := capturePlanCLI(t, func() int { return run(append([]string{"local"}, args...)) })
		if code != 2 || strings.Contains(stdout+stderr, root) || strings.Contains(stdout+stderr, lcSentinel) || !maps.Equal(before, lcaTree(t, filepath.Dir(root))) {
			t.Fatal("a released spelling was not refused unchanged or echoed an argument")
		}
		if class == "" && (stdout != "" || stderr != "usage: witself local checkpoint create|inspect|delete --checkpoint-mode=cold-v1 --root=ABSOLUTE_PATH\n") {
			t.Fatal("the namespace usage line differs")
		}
		if class != "" {
			if r := lcaReceipt(t, stdout); stderr != "" || r.Outcome != "refused" || r.Failure == nil || r.Failure.Class != class || r.Before != nil || r.After != nil {
				t.Fatal("a released refusal differs from its fixed class")
			}
		}
	}
	t.Run("disabled", func(t *testing.T) {
		t.Setenv("WITSELF_CHECKPOINT_MODE", "cold-v1")
		_, root := lcaRoot(t)
		refused(t, root, nil, "")
		refused(t, root, []string{lcSentinel, "checkpoint"}, "")
		refused(t, root, []string{"checkpoint"}, "local_checkpoint_command_disabled")
		refused(t, root, []string{"checkpoint", lcSentinel, "--root=" + root}, "local_checkpoint_command_disabled")
		for _, verb := range []string{"create", "inspect", "delete"} {
			args := lcReplace(lcArgs(verb), "root", root)
			refused(t, root, append([]string{"checkpoint"}, slices.DeleteFunc(slices.Clone(args), func(a string) bool { return strings.HasPrefix(a, "--checkpoint-mode=") })...), "local_checkpoint_command_disabled")
			for _, mode := range []string{"cold-v1x", "COLD-V1", "cold-v1 "} {
				refused(t, root, append([]string{"checkpoint"}, lcReplace(args, "checkpoint-mode", mode)...), "local_checkpoint_command_disabled")
			}
		}
	})
	t.Run("enabled", func(t *testing.T) {
		parent, root := lcaRoot(t)
		before := lcaTree(t, parent)
		stdout, stderr, code := capturePlanCLI(t, func() int {
			return run([]string{"local", "checkpoint", "inspect", "--checkpoint-mode=cold-v1", "--root=" + root})
		})
		if r := lcaReceipt(t, stdout); code != 0 || stderr != "" || r.Outcome != "succeeded" || r.After == nil || r.After.Status != "listed" || r.After.Count == nil || *r.After.Count != 0 || !maps.Equal(before, lcaTree(t, parent)) {
			t.Fatal("a selected inspect did not list an empty private root unchanged")
		}
		for verb, consent := range map[string][]string{"create": {"--installation-id=" + lcIdentity}, "delete": {"--checkpoint-id=" + lcID, "--checkpoint-mode=cold-v1"}} {
			for _, repeated := range consent {
				refused(t, root, append(append([]string{"checkpoint"}, lcReplace(lcArgs(verb), "root", root)...), repeated), "local_checkpoint_arguments_invalid")
			}
		}
		doc, err := os.ReadFile(commandSurfaceDoc)
		_, section, found := strings.Cut(strings.ReplaceAll(string(doc), "\r\n", "\n"), "\n## `witself local`\n")
		section, _, _ = strings.Cut(section, "\n## ")
		var help bytes.Buffer
		usage(&help)
		if err != nil || !found || !slices.ContainsFunc(strings.Split(section, "\n"), func(line string) bool { return strings.Contains(line, lcNotice) }) || strings.Count("\n"+help.String(), "\n"+lcHelp+"\n") != 1 {
			t.Fatal("the receipt notice or help line differs from the documented one")
		}
	})
}
