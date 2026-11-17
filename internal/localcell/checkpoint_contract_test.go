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
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// checkpointFiles are the files that implement checkpoints.
var checkpointFiles = []string{"checkpoint.go", "checkpoint_unix.go", "checkpoint_windows.go", "checkpoint_other.go", "checkpoint_verify.go", "drill_root.go"}

// checkpointForbiddenOS are calls the checkpoint files may not make (C7(e));
// os.RemoveAll is counted by (a); os.OpenRoot is the Unix opener's and the drill root's.
var checkpointForbiddenOS = []string{"os.Create", "os.CreateTemp", "os.WriteFile", "os.Mkdir", "os.MkdirAll", "os.OpenFile",
	"os.Rename", "os.Remove", "os.Truncate", "os.Chmod", "os.Chown", "os.Chtimes", "os.Link", "os.Symlink",
	"exec.Command", "exec.CommandContext"}

// checkpointForbiddenHelpers mint credentials, run PostgreSQL, stop, remove or
// change service intent (C7(c)).
var checkpointForbiddenHelpers = []string{"makePassword", "makePrincipalPasswords", "pgCommand", "databaseCommand", "startPostgres",
	"startServicePostgres", "stopAll", "retainRemoval", "finishRemoval", "setServiceDesiredStateLocked", "Repair", "ownExisting",
	"discardJournalTail", "stopSelected", "signalTerminate", "stopPostgres", "terminateWindowsPending"}

// checkpointReaders may receive a path below pg/ or run/, or the manifest;
// filepath.Rel is pure path arithmetic.
var checkpointReaders = []string{"os.Lstat", "os.OpenRoot", "filepath.WalkDir", "filepath.Rel", "readPostgresFile", "openPrivateFile",
	"runtimeconfig.WindowsPrivatePostgresOpen", "c.command"}

// drillRootCalls are the only calls drill_root.go makes on a root receiver, by
// method, argument text and exact count.
var drillRootCalls = map[string][]string{"Stat": {`"."`}, "Lstat": {"drillID"}, "Open": {`"."`},
	"OpenFile": {"drillRootRecordName, os.O_RDONLY | syscall.O_NONBLOCK, 0"}, "OpenRoot": {"drillID + drillRootHere"},
	"RemoveAll": {"name"}, "Remove": {"drillRootRecordName", "drillID"}, "Close": {""}}
var drillRootCounts = map[string]int{"Stat": 2, "Lstat": 1, "Open": 2, "OpenFile": 1, "OpenRoot": 1, "RemoveAll": 1, "Remove": 2, "Close": 2}

// drillRootReceivers are drill_root.go's identifiers assigned once from
// os.OpenRoot or a root receiver's OpenRoot, and its *os.Root parameters;
// deferred holds every node inside a defer statement.
func drillRootReceivers(file *ast.File, values map[any][]ast.Expr) (receivers map[any]bool, deferred map[ast.Node]bool) {
	receivers, deferred = map[any]bool{}, map[ast.Node]bool{}
	ast.Inspect(file, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.FuncType:
			for _, field := range node.Params.List {
				for _, identifier := range field.Names {
					if identifier.Obj != nil && types.ExprString(field.Type) == "*os.Root" {
						receivers[identifier.Obj] = true
					}
				}
			}
		case *ast.DeferStmt:
			ast.Inspect(node, func(inner ast.Node) bool { deferred[inner] = true; return true })
		}
		return true
	})
	for changed := true; changed; {
		changed = false
		for object, assigned := range values {
			call, ok := assigned[0].(*ast.CallExpr)
			if !ok || len(assigned) != 1 || receivers[object] {
				continue
			}
			selector, _ := call.Fun.(*ast.SelectorExpr)
			if selector != nil && selector.Sel.Name == "OpenRoot" && (types.ExprString(selector.X) == "os" || drillRootReceiver(receivers, selector.X)) {
				receivers[object], changed = true, true
			}
		}
	}
	return receivers, deferred
}

func drillRootReceiver(receivers map[any]bool, expression ast.Expr) bool {
	identifier, ok := expression.(*ast.Ident)
	return ok && identifier.Obj != nil && receivers[identifier.Obj]
}

// drillRootIdentity is the one call assigned to an identity operand, with a
// root receiver spelled "root".
func drillRootIdentity(values map[any][]ast.Expr, receivers map[any]bool, expression ast.Expr) string {
	identifier, ok := expression.(*ast.Ident)
	if !ok || identifier.Obj == nil || len(values[identifier.Obj]) != 1 {
		return ""
	}
	call, ok := values[identifier.Obj][0].(*ast.CallExpr)
	if !ok {
		return ""
	}
	if selector, ok := call.Fun.(*ast.SelectorExpr); ok && drillRootReceiver(receivers, selector.X) {
		return "root." + strings.TrimPrefix(types.ExprString(call), types.ExprString(selector.X)+".")
	}
	return types.ExprString(call)
}

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

// checkpointDocumentReference follows local aliases of a document name.
func checkpointDocumentReference(values map[any][]ast.Expr, expression ast.Expr, document string, depth int) bool {
	if depth >= 8 {
		return false
	}
	found := false
	ast.Inspect(expression, func(node ast.Node) bool {
		identifier, ok := node.(*ast.Ident)
		if !ok || found {
			return !found
		}
		if identifier.Name == document {
			found = true
		} else if identifier.Obj != nil {
			for _, value := range values[identifier.Obj] {
				if value != nil && checkpointDocumentReference(values, value, document, depth+1) {
					found = true
				}
			}
		}
		return !found
	})
	return found
}

// checkpointSourceContract returns every failed C7 check over production
// sources, independent of GOOS and build tags.
func checkpointSourceContract(sources map[string]string) []string {
	failures := map[string]bool{}
	declared, parsed := map[string]string{}, map[string]*ast.File{}
	packageFunctions := map[string]bool{}
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
				if node.Recv == nil && !strings.HasSuffix(name, "_test.go") {
					packageFunctions[node.Name.Name] = true
				}
				if slices.Contains(checkpointFiles, name) && (node.Recv == nil || updateReceiverName(node) == "Cell") {
					declared[node.Name.Name] = name
				}
			case *ast.TypeSpec:
				if slices.Contains(checkpointFiles, name) {
					declared[node.Name.Name] = name
				}
			case *ast.ValueSpec:
				for _, identifier := range node.Names {
					if slices.Contains(checkpointFiles, name) {
						declared[identifier.Name] = name
					}
				}
			}
			_, isFunction := node.(*ast.FuncDecl)
			return !isFunction
		})
	}
	removals := map[string]int{"checkpoint.go": 0, "lifecycle.go": 0, "drill_root.go": 0}
	documentReads := map[string]int{}
	wantedReads := map[string]int{"witness_read:checkpoint.go:checkpointComplete": 1,
		"witness_read:checkpoint_verify.go:verifyCheckpointDocuments": 1, "inventory_read:checkpoint_verify.go:verifyCheckpointDocuments": 1}
	documentReferences, verifierNames := map[string]int{}, map[string]int{}
	wantedReferences := map[string]int{
		"checkpointWitnessName:checkpoint.go:witnessCheckpoint": 1, "checkpointWitnessName:checkpoint.go:discardOwnCheckpoint": 1,
		"checkpointWitnessName:checkpoint.go:deleteCheckpointLocked": 2, "checkpointWitnessName:checkpoint.go:checkpointComplete": 1,
		"checkpointInventoryName:checkpoint.go:inventoryCheckpoint":            1,
		"checkpointWitnessName:checkpoint_verify.go:verifyCheckpointDocuments": 1, "checkpointInventoryName:checkpoint_verify.go:verifyCheckpointDocuments": 1,
		"checkpointWitnessName:checkpoint_verify.go:checkpointVerifyPath": 1, "checkpointInventoryName:checkpoint_verify.go:checkpointVerifyPath": 1,
		"checkpointWitnessName:checkpoint_verify.go:checkpointVerifyTree": 1, "checkpointInventoryName:checkpoint_verify.go:checkpointVerifyTree": 1,
	}
	witnessSpellings, inventorySpellings, journalCreates, inspectSummaries, drillHooks, drillOpenRoots := 0, 0, 0, 0, 0, 0
	drillRootCalled := map[string]int{}
	for name, file := range parsed {
		checkpointFile := slices.Contains(checkpointFiles, name)
		values, read := checkpointAssignments(file), map[ast.Node]bool{}
		receivers, deferred := map[any]bool{}, map[ast.Node]bool{}
		if name == "drill_root.go" {
			receivers, deferred = drillRootReceivers(file, values)
		}
		selectorNames, readOnlyOpens := map[*ast.Ident]bool{}, map[ast.Node]bool{}
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
			if selector, ok := node.(*ast.SelectorExpr); ok {
				selectorNames[selector.Sel] = true
			}
			if call, ok := node.(*ast.CallExpr); ok && len(call.Args) == 3 && types.ExprString(call.Args[1]) == "os.O_RDONLY" {
				readOnlyOpens[call], readOnlyOpens[call.Fun] = true, true
			}
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
			constantNames := map[*ast.Ident]bool{}
			declarationNames := map[ast.Node]string{}
			if group, ok := declaration.(*ast.GenDecl); ok {
				for _, spec := range group.Specs {
					switch spec := spec.(type) {
					case *ast.ValueSpec:
						declarationNames[spec] = spec.Names[0].Name
						if group.Tok == token.CONST {
							for _, identifier := range spec.Names {
								constantNames[identifier] = true
							}
						}
					case *ast.TypeSpec:
						declarationNames[spec] = spec.Name.Name
					}
				}
			}
			referenceOwner := owner
			ast.Inspect(declaration, func(node ast.Node) bool {
				if declaredName, ok := declarationNames[node]; ok {
					referenceOwner = declaredName
				}
				if identifier, ok := node.(*ast.Ident); ok {
					if !constantNames[identifier] && (identifier.Name == "checkpointWitnessName" || identifier.Name == "checkpointInventoryName") {
						documentReferences[identifier.Name+":"+name+":"+referenceOwner]++
					}
					if name == "checkpoint_verify.go" && identifier.Name == "verifyCheckpoint" {
						verifierNames[referenceOwner]++
					}
				}
				if identifier, ok := node.(*ast.Ident); ok && name != "checkpoint_verify.go" && identifier.Name == "verifyCheckpoint" {
					failures["verifier_called:"+name+":"+owner] = true
				}
				if identifier, ok := node.(*ast.Ident); ok && declared[identifier.Name] != "" {
					origin := declared[identifier.Name]
					if checkpointFile && name != "drill_root.go" && origin == "drill_root.go" {
						failures["drill_reference:"+name+":"+identifier.Name] = true
					}
					if name == "drill_root.go" && origin != "drill_root.go" && !slices.Contains([]string{"recoveryDir", "validateRecovery", "checkpointIDPattern"}, identifier.Name) {
						failures["drill_checkpoint_reference:"+name+":"+identifier.Name] = true
					}
				}
				if identifier, ok := node.(*ast.Ident); ok && !checkpointFile && declared[identifier.Name] != "" {
					// (f) checkpoint declarations are reached only from the hook field
					// and summary type in types.go, and from Inspect.
					switch {
					case name == "types.go" && (identifier.Name == "checkpointOperations" || identifier.Name == "CheckpointSummary"):
					case name == "types.go" && identifier.Name == "drillRootOperations":
						drillHooks++
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
				if checkpointLiteral(node) == checkpointInventoryName {
					inventorySpellings++
				}
				if name == "checkpoint_verify.go" {
					member := ""
					if call, ok := node.(*ast.CallExpr); ok {
						function := types.ExprString(call.Fun)
						member = function[strings.LastIndex(function, ".")+1:]
					} else if selector, ok := node.(*ast.SelectorExpr); ok {
						member = selector.Sel.Name
					} else if identifier, ok := node.(*ast.Ident); ok && !selectorNames[identifier] && packageFunctions[identifier.Name] && (identifier.Obj == nil || identifier.Obj.Kind == ast.Fun) {
						member = identifier.Name
					}
					switch {
					case member == "checkpointComplete":
						failures["verifier_shortcut"] = true
					case member == "begin" || member == "step" || member == "closeCheckpointFailure" || member == "closeCheckpointLeftover" || strings.Contains(strings.ToLower(member), "journal"):
						failures["verifier_journal"] = true
					case slices.Contains([]string{"Remove", "RemoveAll", "removePrivateFile", "discardOwnCheckpoint", "deleteCheckpointLocked", "DeleteCheckpoint"}, member):
						failures["verifier_remove"] = true
					case slices.Contains([]string{"Write", "WriteAt", "WriteString", "WriteFile", "Create", "CreateTemp", "Mkdir", "MkdirAll", "Rename", "Chmod", "Chown", "Chtimes", "Truncate", "Sync",
						"writeAtomic", "writeAtomicBytes", "createPrivateTemp", "publishPrivateFile", "publish", "syncDirectory", "syncCheckpointDirectory", "syncFile", "privateDir", "failCheckpoint", "Checkpoint", "copyCheckpoint", "inventoryCheckpoint", "witnessCheckpoint"}, member):
						failures["verifier_write"] = true
					}
					if member == "openPrivateFile" && !readOnlyOpens[node] {
						failures["verifier_write"] = true
					}
				}
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				function := types.ExprString(call.Fun)
				if selector, ok := call.Fun.(*ast.SelectorExpr); ok && name == "drill_root.go" && drillRootReceiver(receivers, selector.X) {
					// (b) a root call is typed by method, argument text and count.
					var arguments []string
					for _, argument := range call.Args {
						arguments = append(arguments, types.ExprString(argument))
					}
					if allowed, known := drillRootCalls[selector.Sel.Name]; !known {
						failures["drill_root_method:"+selector.Sel.Name] = true
					} else if !slices.Contains(allowed, strings.Join(arguments, ", ")) {
						failures["drill_root_call:"+selector.Sel.Name] = true
					}
					drillRootCalled[selector.Sel.Name]++
					failures["drill_root_close"] = failures["drill_root_close"] || selector.Sel.Name == "Close" && !deferred[call]
				} else if ok && name == "drill_root.go" && (selector.Sel.Name == "RemoveAll" || selector.Sel.Name == "Remove") && types.ExprString(selector.X) != "os" {
					failures["forbidden_call:"+name+":"+function] = true
				}
				if name == "drill_root.go" && function == "os.OpenRoot" {
					// (c) the one pathname root holds recovery/ as a directory only.
					drillOpenRoots++
					if len(call.Args) != 1 || types.ExprString(call.Args[0]) != "c.recoveryDir() + drillRootHere" {
						failures["drill_call_target:os.OpenRoot"] = true
					}
				}
				// (d) a held identity comes from a root handle, a current one from
				// recovery/ or the drill's name without following its last element.
				if name == "drill_root.go" && (function == "os.Stat" || function == "os.Lstat" && (len(call.Args) != 1 || types.ExprString(call.Args[0]) != "c.recoveryDir()") ||
					function == "os.SameFile" && (len(call.Args) != 2 || drillRootIdentity(values, receivers, call.Args[0]) != `root.Stat(".")` ||
						!slices.Contains([]string{"os.Lstat(c.recoveryDir())", "root.Lstat(drillID)"}, drillRootIdentity(values, receivers, call.Args[1])))) {
					failures["drill_identity:"+owner] = true
				}
				if function == "os.RemoveAll" {
					// (a) each checkpoint deletion targets a local checkpointDir value.
					removals[name]++
					root, bare := checkpointRoot(values, call.Args[0], 0)
					if _, local := call.Args[0].(*ast.Ident); name == "checkpoint.go" && (!local || root != "checkpoint" || !bare) {
						failures["removeall_target:"+name] = true
					}
				}
				// (g) exact document reader inventories, including repeated reads
				// within an otherwise permitted owner and local path aliases.
				if slices.Contains([]string{"readJSON", "openPrivateFile", "readPrivateFile", "readPostgresFile", "os.ReadFile", "os.Open", "ops.open", "checkpointVerifyRead"}, function) {
					argument := 0
					if function == "checkpointVerifyRead" {
						argument = 3
					}
					for document, kind := range map[string]string{"checkpointWitnessName": "witness_read:", "checkpointInventoryName": "inventory_read:"} {
						if len(call.Args) > argument && checkpointDocumentReference(values, call.Args[argument], document, 0) {
							documentReads[kind+name+":"+owner]++
						}
					}
				}
				if !checkpointFile {
					return true
				}
				if slices.Contains(checkpointForbiddenOS, function) || (function == "os.OpenRoot" && name != "checkpoint_unix.go" && name != "drill_root.go") {
					failures["forbidden_call:"+name+":"+function] = true
				}
				// (e) a root open is read-only; the drill record's alone is also non-blocking.
				if flags := ""; strings.HasSuffix(function, ".OpenFile") && function != "os.OpenFile" {
					if len(call.Args) == 3 {
						flags = types.ExprString(call.Args[1])
					}
					if flags != "os.O_RDONLY" && (name != "drill_root.go" || flags != "os.O_RDONLY | syscall.O_NONBLOCK") {
						failures["root_open_not_read_only:"+name] = true
					}
				}
				// (b) each writer's path is rooted in a checkpointDir value, or is
				// exactly recovery/ (create or sync) or journal/ (one create).
				paths := map[string]int{"writeAtomic": 1, "writeAtomicBytes": 1, "createPrivateTemp": 1, "removePrivateFile": 1, "syncDirectory": 1,
					"syncCheckpointDirectory": 1, "os.RemoveAll": 1, "publishPrivateFile": 2, "ops.publish": 2, "privateDir": 1}[function]
				if function == "privateDir" && types.ExprString(call.Args[1]) == "false" {
					paths = 0
				}
				for _, argument := range call.Args[:min(paths, len(call.Args))] {
					switch root, bare := checkpointRoot(values, argument, 0); {
					case root == "checkpoint" && name != "drill_root.go":
					case root == "recovery" && bare && name != "drill_root.go" && (function == "privateDir" || function == "syncDirectory" || function == "syncCheckpointDirectory"):
					case root == "journal" && bare && function == "privateDir" && owner == "deleteCheckpointLocked":
						journalCreates++
					default:
						failures["writer_root:"+name+":"+function] = true
					}
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
	for key := range wantedReferences {
		failures["document_reference:"+key] = documentReferences[key] != wantedReferences[key]
	}
	for key := range documentReferences {
		failures["document_reference:"+key] = documentReferences[key] != wantedReferences[key]
	}
	if !maps.Equal(verifierNames, map[string]int{"verifyCheckpoint": 1}) {
		for owner := range verifierNames {
			failures["verifier_called:checkpoint_verify.go:"+owner] = true
		}
		if len(verifierNames) == 0 {
			failures["verifier_called:checkpoint_verify.go:"] = true
		}
	}
	failures["inventory_name_spelling"] = inventorySpellings != 1
	for reader := range wantedReads {
		failures[reader] = documentReads[reader] != wantedReads[reader]
	}
	for reader := range documentReads {
		failures[reader] = documentReads[reader] != wantedReads[reader]
	}
	for name, count := range removals {
		failures["removeall_count:"+name] = count != map[string]int{"checkpoint.go": 2, "lifecycle.go": 3, "drill_root.go": 0}[name]
	}
	for method, count := range drillRootCounts {
		failures["drill_root_count:"+method] = drillRootCalled[method] != count
	}
	failures["drill_call_count:os.OpenRoot"], failures["drill_hook_count"] = drillOpenRoots != 1, drillHooks != 1
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
	const verifyTail = "func (c *Cell) verifyCheckpoint("
	const drillTail = "const drillRootRecordName"
	mutations := []mutation{
		{"drill_openroot", "drill_root.go", drillTail, "func drillRootProbe(c *Cell) { dir, _ := c.drillDir(\"\"); _, _ = os.OpenRoot(dir) }\n\n" + drillTail, "drill_call_target:os.OpenRoot"},
		{"drill_remove", "drill_root.go", drillTail, "func drillRootProbe(c *Cell) { dir, _ := c.drillDir(\"\"); _ = os.Remove(dir) }\n\n" + drillTail, "forbidden_call:drill_root.go:os.Remove"},
		{"drill_root_removeall", "drill_root.go", drillTail, "func drillRootProbe(root *os.Root) { _ = root.RemoveAll(\"pg\") }\n\n" + drillTail, "drill_root_call:RemoveAll"},
		{"drill_root_remove", "drill_root.go", drillTail, "func drillRootProbe(root *os.Root) { _ = root.Remove(drillRootRecordName) }\n\n" + drillTail, "drill_root_count:Remove"},
		{"drill_recovery_sync", "drill_root.go", drillTail, "func drillRootProbe(c *Cell) { _ = syncDirectory(c.recoveryDir()) }\n\n" + drillTail, "writer_root:drill_root.go:syncDirectory"},
		{"drill_checkpoint_sync", "drill_root.go", drillTail, "func drillRootProbe() { _ = syncCheckpointDirectory }\n\n" + drillTail, "drill_checkpoint_reference:drill_root.go:syncCheckpointDirectory"},
		{"drill_second_openroot", "drill_root.go", drillTail, "func drillRootProbe(c *Cell) { _, _ = os.OpenRoot(c.recoveryDir() + drillRootHere) }\n\n" + drillTail, "drill_call_count:os.OpenRoot"},
		{"drill_openroot_plain", "drill_root.go", "os.OpenRoot(c.recoveryDir() + drillRootHere)", "os.OpenRoot(c.recoveryDir())", "drill_call_target:os.OpenRoot"},
		{"drill_child_openroot_plain", "drill_root.go", "recovery.OpenRoot(drillID + drillRootHere)", "recovery.OpenRoot(drillID)", "drill_root_call:OpenRoot"},
		{"drill_identity_from_lstat", "drill_root.go", "drillHeld, err := drill.Stat(\".\")", "drillHeld, err := recovery.Lstat(drillID)", "drill_identity:drillRootBound"},
		{"drill_identity_follows", "drill_root.go", "drillNow, nowErr := recovery.Lstat(drillID)", "drillNow, nowErr := recovery.Stat(drillID)", "drill_identity:drillRootBound"},
		{"drill_recovery_identity_path", "drill_root.go", "recoveryHeld, err := recovery.Stat(\".\")", "recoveryHeld, err := os.Lstat(c.recoveryDir())", "drill_identity:drillRootBound"},
		{"drill_final_removeall", "drill_root.go", "recovery.Remove(drillID)", "recovery.RemoveAll(drillID)", "drill_root_count:RemoveAll"},
		{"drill_remove_other", "drill_root.go", "drill.Remove(drillRootRecordName)", "drill.Remove(\"pg\")", "drill_root_call:Remove"},
		{"drill_root_write", "drill_root.go", "defer func() { _ = drill.Close() }()", "defer func() { _ = drill.Close() }(); _ = drill.WriteFile(drillRootRecordName, nil, 0o600)", "drill_root_method:WriteFile"},
		{"drill_root_rename", "drill_root.go", "defer func() { _ = recovery.Close() }()", "defer func() { _ = recovery.Close() }(); _ = recovery.Rename(drillID, \"aside\")", "drill_root_method:Rename"},
		{"drill_root_mkdir", "drill_root.go", drillTail, "func drillRootProbe(r *os.Root) { _ = r.Mkdir(\"late\", 0o700) }\n\n" + drillTail, "drill_root_method:Mkdir"},
		{"drill_record_open_write", "drill_root.go", "drill.OpenFile(drillRootRecordName, os.O_RDONLY|syscall.O_NONBLOCK, 0)", "drill.OpenFile(drillRootRecordName, os.O_RDWR|syscall.O_NONBLOCK, 0)", "root_open_not_read_only:drill_root.go"},
		{"drill_unclosed_root", "drill_root.go", "defer func() { _ = drill.Close() }()", "", "drill_root_count:Close"},
		{"drill_close_not_deferred", "drill_root.go", "defer func() { _ = drill.Close() }()", "_ = drill.Close()", "drill_root_close"},
		{"drill_alias_remove", "drill_root.go", drillTail, "func drillRootProbe(r *os.Root) { alias := r; _ = alias.RemoveAll(\"pg\") }\n\n" + drillTail, "forbidden_call:drill_root.go:alias.RemoveAll"},
		{"drill_second_hook", "types.go", "type Cell struct {", "var drillRootHookProbe *drillRootOperations\n\n" + "type Cell struct {", "drill_hook_count"},
		{"drill_checkpoint_sync_reference", "drill_root.go", "if drillRootSync(recovery) != nil {", "if syncCheckpointDirectory(c.recoveryDir()) != nil {", "drill_checkpoint_reference:drill_root.go:syncCheckpointDirectory"},
		{"checkpoint_root_open_nonblock", "checkpoint_unix.go", "root.OpenFile(filepath.FromSlash(rel), os.O_RDONLY, 0)", "root.OpenFile(filepath.FromSlash(rel), os.O_RDONLY|syscall.O_NONBLOCK, 0)", "root_open_not_read_only:checkpoint_unix.go"},
		{"checkpoint_openroot", "checkpoint.go", tail, "func checkpointProbe(c *Cell) { _, _ = os.OpenRoot(c.recoveryDir()) }\n\n" + tail, "forbidden_call:checkpoint.go:os.OpenRoot"},
		{"drill_checkpoint_writer", "drill_root.go", drillTail, "func drillRootProbe(c *Cell) { dir, _ := c.checkpointDir(\"\"); _ = syncCheckpointDirectory(dir) }\n\n" + drillTail, "writer_root:drill_root.go:syncCheckpointDirectory"},
		{"drill_third_removeall", "drill_root.go", drillTail, "func drillRootProbe(c *Cell) { dir, _ := c.drillDir(\"\"); _ = os.RemoveAll(dir) }\n\n" + drillTail, "removeall_count:drill_root.go"},
		{"checkpoint_drill_writer", "checkpoint.go", tail, "func checkpointProbe(c *Cell) { dir, _ := c.drillDir(\"\"); _ = syncCheckpointDirectory(dir) }\n\n" + tail, "writer_root:checkpoint.go:syncCheckpointDirectory"},
		{"drill_outside_reference", "lifecycle.go", "func (c *Cell) begin(", "func drillRootOutsideProbe() { _ = drillRootRecordName }\n\n" + "func (c *Cell) begin(", "reference:lifecycle.go:drillRootRecordName"},
		{"drill_checkpoint_reference", "checkpoint.go", tail, "func checkpointProbe() { _ = drillRootRecordName }\n\n" + tail, "drill_reference:checkpoint.go:drillRootRecordName"},
		{"drill_calls_checkpoint_delete", "drill_root.go", drillTail, "func drillRootProbe(c *Cell) { _ = c.deleteCheckpointLocked(\"\") }\n\n" + drillTail, "drill_checkpoint_reference:drill_root.go:deleteCheckpointLocked"},
		{"removeall_on_recovery_path", "checkpoint.go", "if err := os.RemoveAll(dir); err != nil {\n\t\treturn errCheckpointCopy", "if err := os.RemoveAll(c.path(\"recovery\", checkpointID)); err != nil {\n\t\treturn errCheckpointCopy", "removeall_target:checkpoint.go"},
		{"third_removeall", "checkpoint.go", tail, "func checkpointSweep(c *Cell) error { dir, _ := c.checkpointDir(\"x\"); return os.RemoveAll(dir) }\n\n" + tail, "removeall_count:checkpoint.go"},
		{"lifecycle_removeall_dropped", "lifecycle.go", "if err := os.RemoveAll(c.path(name)); err != nil {", "if err := removePrivateFile(c.path(name)); err != nil {", "removeall_count:lifecycle.go"},
		{"publish_into_pgdata", "checkpoint.go", "target, attempted := filepath.Join(dir, filepath.FromSlash(entry.path)), false", "target, attempted := filepath.Join(c.path(\"pg\"), filepath.FromSlash(entry.path)), false", "writer_root:checkpoint.go:ops.publish"},
		{"escape_by_literal", "checkpoint.go", "writeAtomic(filepath.Join(dir, checkpointWitnessName), witness, nil)", "writeAtomic(filepath.Join(dir, \"..\", \"..\", \"manifest.json\"), witness, nil)", "writer_root:checkpoint.go:writeAtomic"},
		{"temporary_in_run", "checkpoint.go", "createPrivateTemp(filepath.Join(dir, filepath.FromSlash(path.Dir(entry.path))))", "createPrivateTemp(c.path(\"run\"))", "live_path_not_read:checkpoint.go:run"},
		{"recovery_joined_sync", "checkpoint.go", "return syncCheckpointDirectory(c.recoveryDir())\n}\n\n// failCheckpoint", "return syncCheckpointDirectory(filepath.Join(c.recoveryDir(), checkpointID))\n}\n\n// failCheckpoint", "writer_root:checkpoint.go:syncCheckpointDirectory"},
		{"second_journal_create", "checkpoint.go", "\tif err := c.begin(\"checkpoint\"); err != nil {", "\t_ = privateDir(c.path(\"journal\"), true)\n\tif err := c.begin(\"checkpoint\"); err != nil {", "writer_root:checkpoint.go:privateDir"},
		{"manifest_opened_for_write", "checkpoint.go", "openPrivateFile(c.path(\"manifest.json\"), os.O_RDONLY, 0)", "openPrivateFile(c.path(\"manifest.json\"), os.O_RDWR, 0)", "live_path_not_read:checkpoint.go:manifest.json"},
		{"root_open_for_write", "checkpoint_unix.go", "root.OpenFile(filepath.FromSlash(rel), os.O_RDONLY, 0)", "root.OpenFile(filepath.FromSlash(rel), os.O_RDWR, 0)", "root_open_not_read_only:checkpoint_unix.go"},
		{"witness_read_elsewhere", "checkpoint.go", tail, "func checkpointPeek(dir string) error { var w checkpointWitness; return readJSON(filepath.Join(dir, checkpointWitnessName), &w) }\n\n" + tail, "witness_read:checkpoint.go:checkpointPeek"},
		{"witness_second_spelling", "checkpoint.go", tail, "var checkpointSeal = \"witness.json\"\n\n" + tail, "witness_name_spelling"},
		{"recovery_dir_elsewhere", "lifecycle.go", "\tif err := c.checkpointSummaries(lock, &result); err != nil {", "\t_ = c.recoveryDir()\n\tif err := c.checkpointSummaries(lock, &result); err != nil {", "reference:lifecycle.go:recoveryDir"},
		{"second_inspect_reference", "lifecycle.go", "\tif err := c.checkpointSummaries(lock, &result); err != nil {", "\t_ = c.checkpointSummaries(lock, &result)\n\tif err := c.checkpointSummaries(lock, &result); err != nil {", "inspect_summaries"},
		{"inspect_closes_leftover", "lifecycle.go", "\tif err := c.checkpointSummaries(lock, &result); err != nil {", "\t_ = c.closeCheckpointLeftover()\n\tif err := c.checkpointSummaries(lock, &result); err != nil {", "reference:lifecycle.go:closeCheckpointLeftover"},
		{"summaries_outside_inspect", "lifecycle.go", "func (c *Cell) finishRemoval(", "func (c *Cell) listRecovery() { _ = c.checkpointSummaries(nil, &Inspection{}) }\n\nfunc (c *Cell) finishRemoval(", "reference:lifecycle.go:checkpointSummaries"},
		{"checkpoint_called_elsewhere", "reinstall.go", "func (c *Cell) reinstallBoundary(", "func (c *Cell) reinstallCheckpoint() { _ = c.Checkpoint(nil, CheckpointOptions{}) }\n\nfunc (c *Cell) reinstallBoundary(", "reference:reinstall.go:Checkpoint"},
		{"call_os.OpenRoot_off_unix", "checkpoint_other.go", "func validateRecoveryObject(", "func checkpointCall() { os.OpenRoot(\"\") }\n\nfunc validateRecoveryObject(", "forbidden_call:checkpoint_other.go:os.OpenRoot"},
		{"third_witness_reader", "checkpoint_verify.go", verifyTail, "func checkpointVerifyPeek(dir string) { _, _ = os.Open(filepath.Join(dir, checkpointWitnessName)) }\n\n" + verifyTail, "witness_read:checkpoint_verify.go:checkpointVerifyPeek"},
		{"second_inventory_reader", "checkpoint_verify.go", verifyTail, "func checkpointVerifyInventoryPeek(dir string) { p := filepath.Join(dir, checkpointInventoryName); _, _ = os.Open(p) }\n\n" + verifyTail, "inventory_read:checkpoint_verify.go:checkpointVerifyInventoryPeek"},
		{"repeated_witness_owner", "checkpoint.go", tail, "func checkpointComplete(dir string) { _, _ = os.Open(filepath.Join(dir, checkpointWitnessName)) }\n\n" + tail, "witness_read:checkpoint.go:checkpointComplete"},
		{"repeated_inventory_owner", "checkpoint_verify.go", verifyTail, "func verifyCheckpointDocuments(dir string) { _, _ = os.Open(filepath.Join(dir, checkpointInventoryName)) }\n\n" + verifyTail, "inventory_read:checkpoint_verify.go:verifyCheckpointDocuments"},
		{"verifier_writes", "checkpoint_verify.go", verifyTail, "func checkpointVerifyWrite(f *os.File) { _, _ = f.Write(nil) }\n\n" + verifyTail, "verifier_write"},
		{"verifier_syncs", "checkpoint_verify.go", verifyTail, "func checkpointVerifySync(f *os.File) { _ = syncFile(f) }\n\n" + verifyTail, "verifier_write"},
		{"verifier_removes", "checkpoint_verify.go", verifyTail, "func checkpointVerifyRemove() { _ = os.Remove(\"\") }\n\n" + verifyTail, "verifier_remove"},
		{"verifier_journals", "checkpoint_verify.go", verifyTail, "func checkpointVerifyJournal(c *Cell) { _ = c.begin(\"checkpoint\") }\n\n" + verifyTail, "verifier_journal"},
		{"verifier_closes_leftover", "checkpoint_verify.go", verifyTail, "func checkpointVerifyClose(c *Cell) { _ = c.closeCheckpointLeftover() }\n\n" + verifyTail, "verifier_journal"},
		{"verifier_shortcut", "checkpoint_verify.go", verifyTail, "func checkpointVerifyShortcut() { _, _ = checkpointComplete(\"\", \"\") }\n\n" + verifyTail, "verifier_shortcut"},
		{"verifier_called_elsewhere", "checkpoint.go", tail, "func checkpointVerifyCaller(c *Cell) { _, _ = c.verifyCheckpoint(nil, nil, \"\") }\n\n" + tail, "verifier_called:checkpoint.go:checkpointVerifyCaller"},
		{"helper_return", "checkpoint_verify.go", verifyTail, "func reviewPath(dir string) string { return filepath.Join(dir, checkpointWitnessName) }\nfunc reviewRead(dir string) { _, _ = os.Open(reviewPath(dir)) }\n\n" + verifyTail, "document_reference:checkpointWitnessName:checkpoint_verify.go:reviewPath"},
		{"struct_field", "checkpoint_verify.go", verifyTail, "func reviewRead(dir string) { holder := struct{ path string }{}; holder.path = filepath.Join(dir, checkpointWitnessName); _, _ = os.Open(holder.path) }\n\n" + verifyTail, "document_reference:checkpointWitnessName:checkpoint_verify.go:reviewRead"},
		{"method_value", "checkpoint_verify.go", verifyTail, "func reviewRead(dir string, ops checkpointOperations) { reader := ops.open; _, _ = reader(filepath.Join(dir, checkpointWitnessName)) }\n\n" + verifyTail, "document_reference:checkpointWitnessName:checkpoint_verify.go:reviewRead"},
		{"unrecognized_reader", "checkpoint_verify.go", verifyTail, "func reviewRead(dir string) { _, _ = os.DirFS(dir).Open(checkpointWitnessName) }\n\n" + verifyTail, "document_reference:checkpointWitnessName:checkpoint_verify.go:reviewRead"},
		{"method_value_write", "checkpoint_verify.go", verifyTail, "func reviewWrite(f *os.File) { writer := f.Write; _, _ = writer([]byte{0x01}) }\n\n" + verifyTail, "verifier_write"},
		{"function_value_write", "checkpoint_verify.go", verifyTail, "func reviewWrite() { publish := writeAtomicBytes; _ = publish }\n\n" + verifyTail, "verifier_write"},
		{"same_file_caller", "checkpoint_verify.go", verifyTail, "func reviewCaller(c *Cell) { _, _ = c.verifyCheckpoint(nil, nil, \"\") }\n\n" + verifyTail, "verifier_called:checkpoint_verify.go:reviewCaller"},
		{"inventory_second_spelling", "checkpoint.go", tail, "var checkpointInventorySeal = \"inventory.json\"\n\n" + tail, "inventory_name_spelling"},
		{"inventory_reference_elsewhere", "checkpoint.go", tail, "func reviewInventory() string { return checkpointInventoryName }\n\n" + tail, "document_reference:checkpointInventoryName:checkpoint.go:reviewInventory"},
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

// checkpointAPIWired names every source that reaches the checkpoint API: a
// selector of one of its names in a package directory where any file imports
// the local cell package, or, in any file, a two-argument call of a selector
// named Checkpoint or DeleteCheckpoint.
func checkpointAPIWired(sources map[string]string) []string {
	var wired []string
	files, importing := map[string]*ast.File{}, map[string]bool{}
	for _, name := range slices.Sorted(maps.Keys(sources)) {
		file, err := parser.ParseFile(token.NewFileSet(), name, sources[name], 0)
		if err != nil {
			wired = append(wired, "parse:"+name)
			continue
		}
		files[name] = file
		importing[path.Dir(name)] = importing[path.Dir(name)] || slices.ContainsFunc(file.Imports, func(spec *ast.ImportSpec) bool {
			return checkpointLiteral(spec.Path) == "github.com/witwave-ai/witself/internal/localcell"
		})
	}
	for _, name := range slices.Sorted(maps.Keys(files)) {
		ast.Inspect(files[name], func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.SelectorExpr:
				if importing[path.Dir(name)] && slices.Contains([]string{"Checkpoint", "DeleteCheckpoint", "CheckpointOptions", "DeleteCheckpointOptions"}, node.Sel.Name) {
					wired = append(wired, name)
				}
			case *ast.CallExpr:
				if selector, ok := node.Fun.(*ast.SelectorExpr); ok && len(node.Args) == 2 && (selector.Sel.Name == "Checkpoint" || selector.Sel.Name == "DeleteCheckpoint") {
					wired = append(wired, name)
				}
			}
			return true
		})
	}
	return slices.Compact(wired)
}

// checkpointWiringSources reads, by slash path below root, every non-test Go
// file outside internal/localcell that names a checkpoint API or the package;
// dot, node_modules, vendor and testdata directories are not walked.
func checkpointWiringSources(t *testing.T, root string) map[string]string {
	t.Helper()
	sources := map[string]string{}
	err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, name)
		if rel = filepath.ToSlash(rel); err != nil || rel == "." {
			return err
		}
		if entry.IsDir() && (strings.HasPrefix(entry.Name(), ".") || slices.Contains([]string{"node_modules", "vendor", "testdata"}, entry.Name()) || rel == "internal/localcell") {
			return filepath.SkipDir
		}
		if entry.IsDir() || !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(name)
		if bytes.Contains(data, []byte("Checkpoint")) || bytes.Contains(data, []byte("github.com/witwave-ai/witself/internal/localcell")) {
			sources[rel] = string(data)
		}
		return err
	})
	if err != nil {
		t.Fatal("cannot read the module's sources for the checkpoint wiring contract")
	}
	return sources
}

// Dark until E2: no production code of the module outside this package reaches
// the checkpoint API. Only a later, explicitly specified slice may change this.
func TestCheckpointAPIUnwired(t *testing.T) {
	sources := checkpointWiringSources(t, "../..")
	if _, read := sources["cmd/witself-server/main.go"]; !read {
		t.Fatal("the checkpoint wiring contract did not read a known importer of the package")
	}
	if wired := checkpointAPIWired(sources); len(wired) != 0 {
		t.Fatalf("the checkpoint API is reachable from production code: %v", wired)
	}
	const importing = "package main\n\nimport (\n\t\"context\"\n\n\t\"github.com/witwave-ai/witself/internal/localcell\"\n)\n\nvar _ context.Context\n\n"
	const call = importing + "func run(ctx context.Context, cell *localcell.Cell) error { return cell.Checkpoint(ctx, localcell.CheckpointOptions{}) }\n"
	const fixture = "cmd/witself-fixture/main.go"
	for _, row := range []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{"checkpoint-call", map[string]string{fixture: call}, []string{fixture}},
		{"delete-call", map[string]string{fixture: importing + "func run(ctx context.Context, cell *localcell.Cell) error { return cell.DeleteCheckpoint(ctx, localcell.DeleteCheckpointOptions{}) }\n"}, []string{fixture}},
		{"method-value", map[string]string{fixture: importing + "var _ = (*localcell.Cell).Checkpoint\n"}, []string{fixture}},
		{"unrelated-zero-argument-call", map[string]string{"internal/model/model.go": "package model\n\ntype model struct{}\n\nfunc (model) Checkpoint() {}\n\nfunc run() { model{}.Checkpoint() }\n"}, nil},
		{"same-package-non-importing", map[string]string{fixture: importing + "var cell *localcell.Cell\n", "cmd/witself-fixture/run.go": "package main\n\nvar run = cell.Checkpoint\n"}, []string{"cmd/witself-fixture/run.go"}},
		{"two-argument-call-without-import", map[string]string{"tools/other/other.go": "package other\n\nfunc run(cell interface{ Checkpoint(any, any) error }) error { return cell.Checkpoint(nil, nil) }\n"}, []string{"tools/other/other.go"}},
		{"root-level-package", map[string]string{"wiring.go": call}, []string{"wiring.go"}},
		{"outside-the-walk", map[string]string{".tools/main.go": call, "node_modules/x/main.go": call, "vendor/x/main.go": call, "cmd/witself-fixture/testdata/main.go": call,
			"internal/localcell/wiring.go": call, "cmd/witself-fixture/main_test.go": call}, nil},
	} {
		t.Run(row.name, func(t *testing.T) {
			root := t.TempDir()
			for name, source := range row.files {
				if os.MkdirAll(filepath.Join(root, path.Dir(name)), 0o700) != nil || os.WriteFile(filepath.Join(root, name), []byte(source), 0o600) != nil {
					t.Fatal("cannot write a checkpoint wiring fixture")
				}
			}
			if got := checkpointAPIWired(checkpointWiringSources(t, root)); !slices.Equal(got, row.want) {
				t.Fatalf("checkpoint wiring fixture %s reported %v", row.name, got)
			}
		})
	}
}
