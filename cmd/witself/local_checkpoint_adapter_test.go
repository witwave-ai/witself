package main

import (
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
	"path"
	"path/filepath"
	"reflect"
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

// lcaCell is every Cell its run's open returns. Inspect replays reads in
// order (the last repeats) and fails read number failRead; calls are recorded.
type lcaCell struct {
	t                 *testing.T
	reads             []localcell.Inspection
	failRead          int
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
		if mode != localCheckpointColdV1 {
			t.Fatal("adapter opened a cell without the selector")
		}
		opened++
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
// golden equals stdout, stderr is empty, no sentinel, one LF-terminated line,
// no key repeated at any depth, the closed projection and its disclosures.
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

// lcaTree records each name, type and file content below dir; never printed.
func lcaTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	tree := map[string]string{}
	if filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err == nil {
			var data []byte
			if entry.Type().IsRegular() {
				data, err = os.ReadFile(path)
			}
			tree[path] = entry.Type().String() + string(data)
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
var lcAdapterImports = []string{"context", "errors", "io", "os", "strings", "github.com/witwave-ai/witself/internal/localcell"}
var lcAdapterNames = []string{"Binaries", "Bytes", "Checkpoint", "CheckpointID", "CheckpointListing", "CheckpointOptions", "CheckpointUnknown", "Checkpoints", "Class", "ClassifyCheckpointFailure", "CleanupIncomplete", "Complete", "Config", "ContainsRune", "Context", "CreatedAt", "DeleteCheckpoint", "DeleteCheckpointOptions", "ID", "Inspect", "Inspection", "InstallationID", "Manifest", "New", "PostgresDir", "Reason", "Root", "Timeout", "Writer", "checkpointID", "entries", "installationID", "installationPresent", "pgbin", "root", "status", "timeout", "unknown"}
var lcAdapterHandler = []string{"localCheckpointCommand", "localCheckpointParse", "localCheckpointCallbacks", "localCheckpointRequest", "localCheckpointFailure", "localCheckpointObservation", "localCheckpointSummary"}

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
	// (c) Outside the two files no reserved identifier occurs and no other
	// command source imports the local cell package.
	for name, file := range files {
		if name != handler && name != adapter {
			ast.Inspect(file, func(node ast.Node) bool {
				ident, ok := node.(*ast.Ident)
				fail("outside", ok && strings.HasPrefix(ident.Name, "localCheckpoint"))
				return true
			})
			fail("outside", path.Dir(name) == "cmd/witself" && slices.ContainsFunc(file.Imports, func(spec *ast.ImportSpec) bool { return spec.Path.Value == strconv.Quote(cell) }))
		}
	}
	// (e) The adapter's imports, selectors and composite keys are closed.
	for _, spec := range files[adapter].Imports {
		value, err := strconv.Unquote(spec.Path.Value)
		fail("allowlist", err != nil || spec.Name != nil || !slices.Contains(lcAdapterImports, value))
	}
	ast.Inspect(files[adapter], func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.SelectorExpr:
			fail("allowlist", !slices.Contains(lcAdapterNames, node.Sel.Name))
		case *ast.KeyValueExpr:
			key, ok := node.Key.(*ast.Ident)
			fail("allowlist", !ok || !slices.Contains(lcAdapterNames, key.Name))
		}
		return true
	})
	// (f) Neither file declares init. (g) run and usage, both present, hold
	// no folded string constant equal to local or containing checkpoint.
	for _, decl := range append(slices.Clone(files[handler].Decls), files[adapter].Decls...) {
		function, ok := decl.(*ast.FuncDecl)
		fail("init", ok && function.Name.Name == "init")
	}
	routes := 0
	for _, decl := range files["cmd/witself/main.go"].Decls {
		if function, ok := decl.(*ast.FuncDecl); ok && function.Recv == nil && function.Body != nil && (function.Name.Name == "run" || function.Name.Name == "usage") {
			routes++
			ast.Inspect(function.Body, func(node ast.Node) bool {
				value, constant := lcStringConstant(node)
				fail("route", constant && (value == "local" || strings.Contains(value, "checkpoint")))
				return true
			})
		}
	}
	fail("route", routes != 2)
	return failed
}

func TestLocalCheckpointAdapter(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	t.Run("disabled-direct", func(t *testing.T) {
		parent, root := lcaRoot(t)
		before, pgbin := lcaTree(t, parent), filepath.Join(parent, "bin")
		for _, r := range []localCheckpointRequest{{root: root, pgbin: pgbin}, {root: root + "\x00", pgbin: pgbin}, {root: root, pgbin: pgbin + "\x00"}} {
			for _, mode := range []localCheckpointMode{"", "cold-v", "cold-v1 ", "COLD-V1", localCheckpointColdV1} {
				cell, err := localCheckpointOpen(mode, r)
				if (err == nil) != (mode == localCheckpointColdV1 && r.root == root && r.pgbin == pgbin) || (cell == nil) == (err == nil) {
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
		for _, mode := range []localCheckpointMode{localCheckpointColdV1, ""} {
			if callbacks := localCheckpointCallbacksFor(mode, localCheckpointOpen); callbacks.create == nil || callbacks.read == nil || callbacks.delete == nil {
				t.Fatal("adapter callback missing")
			}
		}
	})
	t.Run("inspect", func(t *testing.T) {
		parent, root := lcaRoot(t)
		before, args := lcaTree(t, parent), []string{"inspect", "--checkpoint-mode=cold-v1", "--root=" + root}
		lcaRun(t, args, localCheckpointOpen, 0, `{"operation":"inspect","outcome":"succeeded","after":{"status":"listed","count":0,"unknown_count":0},"verification":"not_performed","notice":"NOTICE"}`)
		if !maps.Equal(before, lcaTree(t, parent)) {
			t.Fatal("inspect changed the real root")
		}
		if lcaPrivateFile(filepath.Join(root, ".owner.lock"), nil) != nil || lcaPrivateDir(filepath.Join(root, "recovery")) != nil || os.WriteFile(filepath.Join(root, "recovery", "stray"), nil, 0o644) != nil {
			t.Fatal("cannot plant a refused recovery entry")
		}
		unavailable := `{"operation":"inspect","outcome":"refused","failure":FAILURE,"after":{"status":"unavailable","failure":FAILURE},"verification":"not_performed","notice":"NOTICE"}`
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
		after := lcaTree(t, parent)
		delete(after, filepath.Join(root, "run"))
		delete(after, filepath.Join(root, "run", "operation.lock"))
		if !maps.Equal(before, after) {
			t.Fatal("fenced delete changed checkpoint, journal or manifest bytes")
		}
	})
}
