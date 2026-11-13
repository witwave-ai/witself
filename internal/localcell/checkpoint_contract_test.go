package localcell

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// checkpointFiles are the four files that implement checkpoints.
var checkpointFiles = []string{"checkpoint.go", "checkpoint_unix.go", "checkpoint_windows.go", "checkpoint_other.go"}

// checkpointForbiddenOS are calls the checkpoint files may not make (C7(e));
// os.RemoveAll is counted by (a) and os.OpenRoot is the Unix opener's alone.
var checkpointForbiddenOS = []string{"os.Create", "os.CreateTemp", "os.WriteFile", "os.Mkdir", "os.MkdirAll", "os.OpenFile",
	"os.Rename", "os.Remove", "os.Truncate", "os.Chmod", "os.Chown", "os.Chtimes", "os.Link", "os.Symlink",
	"exec.Command", "exec.CommandContext"}

// checkpointForbiddenHelpers mint credentials, run PostgreSQL, stop, remove or
// change service intent (C7(c)).
var checkpointForbiddenHelpers = []string{"makePassword", "makePrincipalPasswords", "pgCommand", "databaseCommand", "startPostgres",
	"startServicePostgres", "stopAll", "retainRemoval", "finishRemoval", "setServiceDesiredStateLocked"}

// checkpointReaders may receive a path below pg/ or run/, or the manifest;
// filepath.Rel is pure path arithmetic.
var checkpointReaders = []string{"os.Lstat", "os.OpenRoot", "filepath.WalkDir", "filepath.Rel", "readPostgresFile", "openPrivateFile",
	"runtimeconfig.WindowsPrivatePostgresOpen", "c.command"}

func checkpointLiteral(node ast.Node) string {
	if literal, ok := node.(*ast.BasicLit); ok && literal.Kind == token.STRING {
		value, _ := strconv.Unquote(literal.Value)
		return value
	}
	return ""
}

// checkpointAssignments maps each object to every value assigned to it; the
// first name of a multi-value assignment receives the call, later names nil.
func checkpointAssignments(file *ast.File) map[any][]ast.Expr {
	values := map[any][]ast.Expr{}
	ast.Inspect(file, func(node ast.Node) bool {
		assign, ok := node.(*ast.AssignStmt)
		for index := 0; ok && index < len(assign.Lhs); index++ {
			if identifier, isIdent := assign.Lhs[index].(*ast.Ident); isIdent && identifier.Obj != nil {
				var value ast.Expr
				if len(assign.Rhs) == len(assign.Lhs) || index == 0 {
					value = assign.Rhs[min(index, len(assign.Rhs)-1)]
				}
				values[identifier.Obj] = append(values[identifier.Obj], value)
			}
		}
		return true
	})
	return values
}

// checkpointRoot names where a path is rooted: "checkpoint" for a value of
// checkpointDir, "recovery" for c.recoveryDir() and "journal" for
// c.path("journal"). bare is false once joined; a joined ".." roots nothing.
func checkpointRoot(values map[any][]ast.Expr, expression ast.Expr, depth int) (root string, bare bool) {
	if identifier, ok := expression.(*ast.Ident); ok && identifier.Obj != nil && depth < 8 {
		for index, value := range values[identifier.Obj] {
			next, nextBare := checkpointRoot(values, value, depth+1)
			if next == "" || (index > 0 && next != root) {
				return "", false
			}
			root, bare = next, (index == 0 || bare) && nextBare
		}
		return root, bare
	}
	call, ok := expression.(*ast.CallExpr)
	if !ok || depth >= 8 {
		return "", false
	}
	switch function := types.ExprString(call.Fun); {
	case function == "c.checkpointDir" && len(call.Args) == 1:
		return "checkpoint", true
	case function == "c.recoveryDir":
		return "recovery", true
	case function == "c.path" && len(call.Args) == 1 && checkpointLiteral(call.Args[0]) == "journal":
		return "journal", true
	case function == "filepath.Join" && !slices.ContainsFunc(call.Args[1:], func(argument ast.Expr) bool {
		return strings.Contains(checkpointLiteral(argument), "..")
	}):
		root, _ = checkpointRoot(values, call.Args[0], depth+1)
		return root, false
	case strings.HasSuffix(function, ".Name") && len(call.Args) == 0:
		// A temporary's name is rooted where createPrivateTemp created it.
		if base, ok := call.Fun.(*ast.SelectorExpr).X.(*ast.Ident); ok && base.Obj != nil && len(values[base.Obj]) == 1 {
			if created, ok := values[base.Obj][0].(*ast.CallExpr); ok && types.ExprString(created.Fun) == "createPrivateTemp" {
				root, _ = checkpointRoot(values, created.Args[0], depth+1)
				return root, false
			}
		}
	}
	return "", false
}

// checkpointSourceContract returns every failed C7 check over production
// sources, independent of GOOS and build tags.
func checkpointSourceContract(sources map[string]string) []string {
	failures := map[string]bool{}
	declared, parsed := map[string]bool{}, map[string]*ast.File{}
	for name, source := range sources {
		file, err := parser.ParseFile(token.NewFileSet(), name, strings.ReplaceAll(source, "\r\n", "\n"), 0)
		if err != nil {
			failures["parse:"+name] = true
			continue
		}
		parsed[name] = file
		// Cell methods and package-level names of the checkpoint files; methods
		// of checkpoint types are reachable only through those types.
		ast.Inspect(file, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.FuncDecl:
				declared[node.Name.Name] = declared[node.Name.Name] || slices.Contains(checkpointFiles, name) && (node.Recv == nil || updateReceiverName(node) == "Cell")
			case *ast.TypeSpec:
				declared[node.Name.Name] = declared[node.Name.Name] || slices.Contains(checkpointFiles, name)
			case *ast.ValueSpec:
				for _, identifier := range node.Names {
					declared[identifier.Name] = declared[identifier.Name] || slices.Contains(checkpointFiles, name)
				}
			}
			_, isFunction := node.(*ast.FuncDecl)
			return !isFunction
		})
	}
	removals := map[string]int{"checkpoint.go": 0, "lifecycle.go": 0}
	witnessSpellings, journalCreates, inspectSummaries := 0, 0, 0
	for name, file := range parsed {
		checkpointFile := slices.Contains(checkpointFiles, name)
		values, read := checkpointAssignments(file), map[ast.Node]bool{}
		var markRead func([]ast.Expr)
		markRead = func(arguments []ast.Expr) {
			for _, argument := range arguments {
				if join, ok := argument.(*ast.CallExpr); ok && types.ExprString(join.Fun) == "filepath.Join" {
					markRead(join.Args)
				}
				read[argument] = true
			}
		}
		ast.Inspect(file, func(node ast.Node) bool {
			if call, ok := node.(*ast.CallExpr); ok && slices.Contains(checkpointReaders, types.ExprString(call.Fun)) &&
				(types.ExprString(call.Fun) != "openPrivateFile" || types.ExprString(call.Args[1]) == "os.O_RDONLY") {
				markRead(call.Args)
			}
			return true
		})
		for _, declaration := range file.Decls {
			owner := ""
			if fn, ok := declaration.(*ast.FuncDecl); ok {
				owner = fn.Name.Name
			}
			ast.Inspect(declaration, func(node ast.Node) bool {
				if identifier, ok := node.(*ast.Ident); ok && !checkpointFile && declared[identifier.Name] {
					// (f) checkpoint declarations are reached only from the hook field
					// and summary type in types.go, and from Inspect.
					switch {
					case name == "types.go" && (identifier.Name == "checkpointOperations" || identifier.Name == "CheckpointSummary"):
					case name == "lifecycle.go" && identifier.Name == "checkpointSummaries" && owner == "Inspect":
						inspectSummaries++
					default:
						failures["reference:"+name+":"+identifier.Name] = true
					}
				} else if ok && checkpointFile && slices.Contains(checkpointForbiddenHelpers, identifier.Name) {
					failures["forbidden_helper:"+name+":"+identifier.Name] = true
				}
				if checkpointLiteral(node) == checkpointWitnessName {
					witnessSpellings++
				}
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				function := types.ExprString(call.Fun)
				if function == "os.RemoveAll" {
					// (a) each checkpoint deletion targets a local checkpointDir value.
					removals[name]++
					root, bare := checkpointRoot(values, call.Args[0], 0)
					if _, local := call.Args[0].(*ast.Ident); name == "checkpoint.go" && (!local || root != "checkpoint" || !bare) {
						failures["removeall_target:"+name] = true
					}
				}
				if !checkpointFile {
					return true
				}
				if slices.Contains(checkpointForbiddenOS, function) || (function == "os.OpenRoot" && name != "checkpoint_unix.go") {
					failures["forbidden_call:"+name+":"+function] = true
				}
				if strings.HasSuffix(function, ".OpenFile") && function != "os.OpenFile" && (len(call.Args) != 3 || types.ExprString(call.Args[1]) != "os.O_RDONLY") {
					failures["root_open_not_read_only:"+name] = true
				}
				// (b) each writer's path is rooted in a checkpointDir value, or is
				// exactly recovery/ (create or sync) or journal/ (one create).
				paths := map[string]int{"writeAtomic": 1, "writeAtomicBytes": 1, "createPrivateTemp": 1, "removePrivateFile": 1, "syncDirectory": 1,
					"os.RemoveAll": 1, "publishPrivateFile": 2, "privateDir": 1}[function]
				if function == "privateDir" && types.ExprString(call.Args[1]) == "false" {
					paths = 0
				}
				for _, argument := range call.Args[:min(paths, len(call.Args))] {
					switch root, bare := checkpointRoot(values, argument, 0); {
					case root == "checkpoint":
					case root == "recovery" && bare && (function == "privateDir" || function == "syncDirectory"):
					case root == "journal" && bare && function == "privateDir" && owner == "deleteCheckpointLocked":
						journalCreates++
					default:
						failures["writer_root:"+name+":"+function] = true
					}
				}
				// (g) the witness is read only by checkpointComplete.
				if slices.Contains([]string{"readJSON", "openPrivateFile", "readPrivateFile", "readPostgresFile", "os.ReadFile", "os.Open"}, function) &&
					strings.Contains(types.ExprString(call), "checkpointWitnessName") && owner != "checkpointComplete" {
					failures["witness_read:"+name+":"+owner] = true
				}
				// (b) a path below pg/ or run/, or the manifest, reaches only a reader.
				if first := ""; function == "c.path" && len(call.Args) > 0 {
					if first = checkpointLiteral(call.Args[0]); (first == "pg" || first == "run" || first == "manifest.json") && !read[call] {
						failures["live_path_not_read:"+name+":"+first] = true
					}
				}
				return true
			})
		}
	}
	for name, count := range removals {
		failures["removeall_count:"+name] = count != map[string]int{"checkpoint.go": 2, "lifecycle.go": 3}[name]
	}
	failures["journal_create_count"], failures["witness_name_spelling"], failures["inspect_summaries"] = journalCreates != 1, witnessSpellings != 1, inspectSummaries != 1
	var result []string
	for failure, failed := range failures {
		if failed {
			result = append(result, failure)
		}
	}
	slices.Sort(result)
	return result
}

func TestCheckpointSourceContract(t *testing.T) {
	sources := admissionReadSources(t)
	for _, ending := range []string{"\n", "\r\n"} {
		input := maps.Clone(sources)
		for name, source := range input {
			input[name] = strings.ReplaceAll(strings.ReplaceAll(source, "\r\n", "\n"), "\n", ending)
		}
		if failures := checkpointSourceContract(input); len(failures) != 0 {
			t.Fatalf("checkpoint source contract (%q endings): %v", ending, failures)
		}
	}
	type mutation struct{ name, file, before, after, want string }
	const tail = "func checkpointDigits("
	mutations := []mutation{
		{"removeall_on_recovery_path", "checkpoint.go", "if err := os.RemoveAll(dir); err != nil {\n\t\treturn errCheckpointCopy", "if err := os.RemoveAll(c.path(\"recovery\", checkpointID)); err != nil {\n\t\treturn errCheckpointCopy", "removeall_target:checkpoint.go"},
		{"third_removeall", "checkpoint.go", tail, "func checkpointSweep(c *Cell) error { dir, _ := c.checkpointDir(\"x\"); return os.RemoveAll(dir) }\n\n" + tail, "removeall_count:checkpoint.go"},
		{"lifecycle_removeall_dropped", "lifecycle.go", "if err := os.RemoveAll(c.path(name)); err != nil {", "if err := removePrivateFile(c.path(name)); err != nil {", "removeall_count:lifecycle.go"},
		{"publish_into_pgdata", "checkpoint.go", "publishPrivateFile(temporary.Name(), filepath.Join(dir, ", "publishPrivateFile(temporary.Name(), filepath.Join(c.path(\"pg\"), ", "writer_root:checkpoint.go:publishPrivateFile"},
		{"escape_by_literal", "checkpoint.go", "writeAtomic(filepath.Join(dir, checkpointWitnessName), witness, nil)", "writeAtomic(filepath.Join(dir, \"..\", \"..\", \"manifest.json\"), witness, nil)", "writer_root:checkpoint.go:writeAtomic"},
		{"temporary_in_run", "checkpoint.go", "createPrivateTemp(filepath.Join(dir, filepath.FromSlash(path.Dir(entry.path))))", "createPrivateTemp(c.path(\"run\"))", "live_path_not_read:checkpoint.go:run"},
		{"recovery_joined_sync", "checkpoint.go", "return syncDirectory(c.recoveryDir())\n}\n\n// failCheckpoint", "return syncDirectory(filepath.Join(c.recoveryDir(), checkpointID))\n}\n\n// failCheckpoint", "writer_root:checkpoint.go:syncDirectory"},
		{"second_journal_create", "checkpoint.go", "\tif err := c.begin(\"checkpoint\"); err != nil {", "\t_ = privateDir(c.path(\"journal\"), true)\n\tif err := c.begin(\"checkpoint\"); err != nil {", "writer_root:checkpoint.go:privateDir"},
		{"manifest_opened_for_write", "checkpoint.go", "openPrivateFile(c.path(\"manifest.json\"), os.O_RDONLY, 0)", "openPrivateFile(c.path(\"manifest.json\"), os.O_RDWR, 0)", "live_path_not_read:checkpoint.go:manifest.json"},
		{"root_open_for_write", "checkpoint_unix.go", "root.OpenFile(filepath.FromSlash(rel), os.O_RDONLY, 0)", "root.OpenFile(filepath.FromSlash(rel), os.O_RDWR, 0)", "root_open_not_read_only:checkpoint_unix.go"},
		{"witness_read_elsewhere", "checkpoint.go", tail, "func checkpointPeek(dir string) error { var w checkpointWitness; return readJSON(filepath.Join(dir, checkpointWitnessName), &w) }\n\n" + tail, "witness_read:checkpoint.go:checkpointPeek"},
		{"witness_second_spelling", "checkpoint.go", tail, "var checkpointSeal = \"witness.json\"\n\n" + tail, "witness_name_spelling"},
		{"recovery_dir_elsewhere", "lifecycle.go", "\tif result.Checkpoints, err = c.checkpointSummaries(); err != nil {", "\t_ = c.recoveryDir()\n\tif result.Checkpoints, err = c.checkpointSummaries(); err != nil {", "reference:lifecycle.go:recoveryDir"},
		{"second_inspect_reference", "lifecycle.go", "\tif result.Checkpoints, err = c.checkpointSummaries(); err != nil {", "\t_, _ = c.checkpointSummaries()\n\tif result.Checkpoints, err = c.checkpointSummaries(); err != nil {", "inspect_summaries"},
		{"summaries_outside_inspect", "lifecycle.go", "func (c *Cell) finishRemoval(", "func (c *Cell) listRecovery() { _, _ = c.checkpointSummaries() }\n\nfunc (c *Cell) finishRemoval(", "reference:lifecycle.go:checkpointSummaries"},
		{"checkpoint_called_elsewhere", "reinstall.go", "func (c *Cell) reinstallBoundary(", "func (c *Cell) reinstallCheckpoint() { _ = c.Checkpoint(nil, CheckpointOptions{}) }\n\nfunc (c *Cell) reinstallBoundary(", "reference:reinstall.go:Checkpoint"},
		{"call_os.OpenRoot_off_unix", "checkpoint_other.go", "func validateRecoveryObject(", "func checkpointCall() { os.OpenRoot(\"\") }\n\nfunc validateRecoveryObject(", "forbidden_call:checkpoint_other.go:os.OpenRoot"},
	}
	for _, helper := range checkpointForbiddenHelpers {
		mutations = append(mutations, mutation{"helper_" + helper, "checkpoint.go", tail, "func checkpointCalls(c *Cell) { _ = " + helper + " }\n\n" + tail, "forbidden_helper:checkpoint.go:" + helper})
	}
	for _, call := range checkpointForbiddenOS {
		mutations = append(mutations, mutation{"call_" + call, "checkpoint_windows.go", "func validateRecoveryObject(", "func checkpointCall() { " + call + "() }\n\nfunc validateRecoveryObject(", "forbidden_call:checkpoint_windows.go:" + call})
	}
	for _, row := range mutations {
		t.Run(row.name, func(t *testing.T) {
			input, source := maps.Clone(sources), strings.ReplaceAll(sources[row.file], "\r\n", "\n")
			if strings.Count(source, row.before) != 1 {
				t.Fatal("checkpoint contract mutation anchor is not unique")
			}
			for _, ending := range []string{"\n", "\r\n"} {
				input[row.file] = strings.ReplaceAll(strings.Replace(source, row.before, row.after, 1), "\n", ending)
				if failures := checkpointSourceContract(input); !slices.Contains(failures, row.want) {
					t.Fatalf("checkpoint contract reported %v, want %s", failures, row.want)
				}
			}
		})
	}
	t.Run("journal_vocabulary", checkpointJournalVocabulary)
}

// checkpointPredecessorJournalStep is copied literally from the pre-slice validJournalStep.
func checkpointPredecessorJournalStep(operation, step string) bool {
	steps := map[string]map[string]bool{
		"install": {"manifest": true, "password": true, "initdb": true, "configure": true, "postgres": true, "database": true, "stop": true, "principal_credentials": true, "principals": true, "principal_migrations": true, "principal_grants": true, "principal_verified": true},
		"start":   {"ports": true, "postgres": true, "server": true, "server_ready": true, "account": true, "worker": true, "worker_ready": true, "principal_verified": true},
		"stop":    {"stop": true},
		"remove":  {"stop": true, "spool": true, "tmp": true, "logs": true, "purge": true, "runtime_cleanup": true},

		"reinstall": {"postgres": true, "verified": true, "stop": true, "readmitted": true},
	}
	allowed, ok := steps[operation]
	return ok && (step == "operation" || step == "revert_stop" || allowed[step])
}

// checkpointJournalVocabulary (C7(d)): the predecessor refuses every line a
// checkpoint and a deletion write; this reader accepts them.
func checkpointJournalVocabulary(t *testing.T) {
	f, _ := checkpointFixture(t, "legacy", false)
	c := f.cell
	if c.Checkpoint(t.Context(), checkpointOptions(f)) != nil || c.DeleteCheckpoint(t.Context(), DeleteCheckpointOptions{CheckpointID: checkpointIDs(t, c)[0]}) != nil {
		t.Fatal("cannot write checkpoint journal lines")
	}
	lines := bytes.Split(bytes.TrimSuffix(retainedRead(t, c, "journal/operations.jsonl")[len(f.prefix):], []byte("\n")), []byte("\n"))
	for _, line := range lines {
		var entry journalEntry
		if json.Unmarshal(line, &entry) != nil || !validJournalStep(entry.Operation, entry.Step) || checkpointPredecessorJournalStep(entry.Operation, entry.Step) {
			t.Fatal("a checkpoint journal line is accepted by the predecessor or refused by this reader")
		}
	}
	if _, err := c.readJournal(); err != nil || len(lines) != len(checkpointRecords)+len(checkpointDeleteRecords) {
		t.Fatal("the current reader refused the checkpoint journal lines")
	}
}

// checkpointAPIWired names every source that imports the local cell package
// and selects a checkpoint API name.
func checkpointAPIWired(sources map[string]string) []string {
	var wired []string
	for _, name := range slices.Sorted(maps.Keys(sources)) {
		file, err := parser.ParseFile(token.NewFileSet(), name, sources[name], 0)
		if err != nil {
			wired = append(wired, "parse:"+name)
			continue
		}
		imports := slices.ContainsFunc(file.Imports, func(spec *ast.ImportSpec) bool {
			return spec.Path.Value == strconv.Quote("github.com/witwave-ai/witself/internal/localcell")
		})
		ast.Inspect(file, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if ok && imports && slices.Contains([]string{"Checkpoint", "DeleteCheckpoint", "CheckpointOptions", "DeleteCheckpointOptions"}, selector.Sel.Name) {
				wired = append(wired, name)
			}
			return true
		})
	}
	return slices.Compact(wired)
}

// Dark until E2: no production code outside this package reaches the
// checkpoint API. Only a later, explicitly specified slice may change this.
func TestCheckpointAPIUnwired(t *testing.T) {
	sources := map[string]string{}
	for _, root := range []string{"../../cmd", "../../internal"} {
		err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() && filepath.ToSlash(name) == "../../internal/localcell" {
				return filepath.SkipDir
			}
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				return nil
			}
			data, err := os.ReadFile(name)
			if bytes.Contains(data, []byte(`"github.com/witwave-ai/witself/internal/localcell"`)) {
				sources[filepath.ToSlash(name)] = string(data)
			}
			return err
		})
		if err != nil || len(sources) == 0 {
			t.Fatal("cannot read production sources for the checkpoint wiring contract")
		}
	}
	if wired := checkpointAPIWired(sources); len(wired) != 0 {
		t.Fatalf("the checkpoint API is reachable from production code: %v", wired)
	}
	const importing = "package main\n\nimport (\n\t\"context\"\n\n\t\"github.com/witwave-ai/witself/internal/localcell\"\n)\n\nvar _ context.Context\n\n"
	for _, row := range []struct {
		name, source string
		wired        bool
	}{
		{"checkpoint-call", importing + "func run(ctx context.Context, cell *localcell.Cell) error { return cell.Checkpoint(ctx, localcell.CheckpointOptions{}) }\n", true},
		{"delete-call", importing + "func run(ctx context.Context, cell *localcell.Cell) error { return cell.DeleteCheckpoint(ctx, localcell.DeleteCheckpointOptions{}) }\n", true},
		{"method-value", importing + "var _ = (*localcell.Cell).Checkpoint\n", true},
		{"unrelated-checkpoint", "package main\n\ntype model struct{}\n\nfunc (model) Checkpoint() {}\n\nfunc run() { model{}.Checkpoint() }\n", false},
	} {
		got := checkpointAPIWired(map[string]string{"cmd/witself-fixture/main.go": row.source})
		if (len(got) != 0) != row.wired || slices.ContainsFunc(got, func(name string) bool { return strings.HasPrefix(name, "parse:") }) {
			t.Fatalf("checkpoint wiring fixture %s reported %v", row.name, got)
		}
	}
}
