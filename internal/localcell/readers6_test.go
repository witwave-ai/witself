package localcell

import (
	"bytes"
	"encoding/json"
	"errors"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// writerOpenSites: each journal write open's file, function and checked open (R5).
var writerOpenSites = [][3]string{
	{"files.go", "journalCause", `openRegularWriter(path, os.O_WRONLY | os.O_CREATE | os.O_APPEND, 0o600, irregular, errors.New("cannot open local cell journal"))`},
	{"files.go", "discardJournalTail", `openRegularWriter(c.path("journal", "operations.jsonl"), os.O_WRONLY, 0, torn, torn)`},
	{"removal_retention.go", "finishRetainedRemoval", `openRegularWriter(c.path("journal", "operations.jsonl"), os.O_WRONLY, 0, unavailable, unavailable)`},
}

// writerOpeners are the calls that open, read or write a name.
var writerOpeners = strings.Fields("os.Open os.OpenFile os.Create os.CreateTemp os.OpenRoot os.OpenInRoot os.ReadFile os.WriteFile openPrivateFile " +
	"openPrivateReader openPrivateWriter openRegular openRegularBy readRegular readPrivateFile createPrivateTemp syscall.Open unix.Open")

// writerCalls lists the calls below node in source order.
func writerCalls(node ast.Node) (calls []*ast.CallExpr) {
	ast.Inspect(node, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			calls = append(calls, call)
		}
		return true
	})
	return calls
}

// writerReturn prints name's one declaration's body when it is one return.
func writerReturn(functions map[string][]*ast.FuncDecl, name string) string {
	if declarations := functions[name]; len(declarations) == 1 {
		return writerOneReturn(declarations[0].Body)
	}
	return ""
}

// writerOneReturn prints body when it is one return of one result, a function
// literal there as its own one return (types.ExprString prints a literal's type).
func writerOneReturn(body *ast.BlockStmt) string {
	if len(body.List) != 1 {
		return ""
	}
	ret, ok := body.List[0].(*ast.ReturnStmt)
	if !ok || len(ret.Results) != 1 {
		return ""
	}
	text := types.ExprString(ret.Results[0])
	ast.Inspect(ret.Results[0], func(n ast.Node) bool {
		literal, ok := n.(*ast.FuncLit)
		if ok {
			text = strings.Replace(text, types.ExprString(literal), types.ExprString(literal.Type)+" { "+writerOneReturn(literal.Body)+" }", 1)
		}
		return !ok
	})
	return "return " + text
}

// writerCheckOrder: openRegularWriter's first statement opens, the next is its
// failure branch holding the Lstat; then Stat and blockingWriter, nothing else
// (Close, errors.Is, Mode, IsRegular aside) and never a write, truncate or sync.
func writerCheckOrder(declarations []*ast.FuncDecl) bool {
	if len(declarations) != 1 {
		return false
	}
	body, order, lstat := declarations[0].Body, []string(nil), ast.Node(nil)
	for _, call := range writerCalls(body) {
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if ok && slices.Contains([]string{"Write", "WriteString", "WriteAt", "ReadFrom", "Truncate", "Sync"}, selector.Sel.Name) {
			return false
		} else if ok && slices.Contains([]string{"Close", "Mode", "IsRegular"}, selector.Sel.Name) || types.ExprString(call.Fun) == "errors.Is" {
			continue
		}
		if order = append(order, types.ExprString(call)); types.ExprString(call) == "os.Lstat(path)" {
			lstat = call
		}
	}
	if !slices.Equal(order, []string{"openPrivateWriter(path, flags, mode)", "os.Lstat(path)", "f.Stat()", "blockingWriter(f)"}) || len(body.List) < 2 {
		return false
	}
	open, isAssign := body.List[0].(*ast.AssignStmt)
	branch, isIf := body.List[1].(*ast.IfStmt)
	return isAssign && len(open.Rhs) == 1 && types.ExprString(open.Rhs[0]) == "openPrivateWriter(path, flags, mode)" && isIf && branch.Init == nil &&
		types.ExprString(branch.Cond) == "err != nil" && lstat.Pos() > branch.Body.Pos() && lstat.End() < branch.Body.End()
}

// writerForbidden: files.go and removal_retention.go reference none of these
// by any spelling but syncDirectory's pinned directory open (G1).
var writerForbidden = []string{"os.OpenFile", "os.Open", "os.Create", "os.ReadFile", "os.WriteFile",
	"localcell.openPrivateFile", "localcell.readPrivateFile", "localcell.readPostgresFile"}

const writerPinnedOpen = `os.Open(path + string(filepath.Separator) + ".")`

// writerReferences type-checks the sources this platform builds, as readers4Contract
// does: writer_reference:<file>:<name> per reference in the two files to a forbidden
// name (a call, a value, a renamed import, a package variable declared as one in any
// file); writer_reference_types per parse error, or type error there but at a non-standard import.
func writerReferences(sources map[string]string, failures map[string]bool) {
	platform, files, own, admit, unresolved := build.Default, []*ast.File(nil), map[string]*ast.File{}, map[string]bool{}, map[token.Pos]bool{}
	lf := func(name string) string { return strings.ReplaceAll(sources[filepath.Base(name)], "\r\n", "\n") }
	platform.OpenFile = func(name string) (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(lf(name))), nil }
	for _, name := range slices.Sorted(maps.Keys(sources)) {
		match, err := platform.MatchFile(".", name)
		file, parseErr := parser.ParseFile(readers4Fset, name, lf(name), 0)
		if failures["writer_reference_types"] = failures["writer_reference_types"] || err != nil || match && parseErr != nil; !match || parseErr != nil {
			continue
		}
		if files = append(files, file); name == "files.go" || name == "removal_retention.go" {
			own[name] = file
			for _, spec := range file.Imports {
				path := strings.Trim(spec.Path.Value, "\"`")
				admit[path] = !strings.Contains(strings.Split(path, "/")[0], ".")
				unresolved[spec.Path.Pos()] = !admit[path]
			}
		}
	}
	info := &types.Info{Uses: map[*ast.Ident]types.Object{}}
	config := types.Config{Importer: readers4Importer(func(path string) (*types.Package, error) {
		if !admit[path] {
			return nil, errors.New("import not admitted")
		}
		return readers4Std.Import(path)
	}), Error: func(err error) {
		file := readers4Fset.File(err.(types.Error).Pos)
		failures["writer_reference_types"] = failures["writer_reference_types"] || file != nil && own[file.Name()] != nil && !unresolved[err.(types.Error).Pos]
	}}
	_, _ = config.Check("localcell", readers4Fset, files, info)
	aliases := map[types.Object]types.Object{}
	for _, initializer := range info.InitOrder {
		if selector, ok := ast.Unparen(initializer.Rhs).(*ast.SelectorExpr); ok {
			aliases[initializer.Lhs[0]] = info.Uses[selector.Sel]
		} else if identifier, ok := ast.Unparen(initializer.Rhs).(*ast.Ident); ok {
			aliases[initializer.Lhs[0]] = info.Uses[identifier]
		}
	}
	for name, file := range own {
		var pinned *ast.Ident
		for _, declaration := range file.Decls {
			function, _ := declaration.(*ast.FuncDecl)
			ast.Inspect(declaration, func(node ast.Node) bool {
				switch node := node.(type) {
				case *ast.CallExpr:
					if selector, ok := node.Fun.(*ast.SelectorExpr); ok && pinned == nil && name == "files.go" && function != nil && function.Recv == nil && function.Name.Name == "syncDirectory" && types.ExprString(node) == writerPinnedOpen {
						pinned = selector.Sel
					}
				case *ast.Ident:
					object := info.Uses[node]
					for depth := 0; depth < 8 && aliases[object] != nil; depth++ {
						object = aliases[object]
					}
					if object != nil && object.Pkg() != nil && object.Parent() == object.Pkg().Scope() && node != pinned && slices.Contains(writerForbidden, object.Pkg().Path()+"."+object.Name()) {
						failures["writer_reference:"+name+":"+object.Pkg().Path()+"."+object.Name()] = true
					}
				}
				return true
			})
		}
	}
}

// writerOpenContract returns every failed key of R7 over the non-test sources,
// read without build constraints, so every platform checks both platform files.
func writerOpenContract(sources map[string]string) (result []string) {
	failures, parsed := map[string]bool{}, map[string]map[string][]*ast.FuncDecl{}
	for name, source := range sources {
		if !slices.Contains([]string{"files.go", "files_other.go", "files_windows.go", "removal_retention.go"}, name) && !strings.Contains(source, "openPrivateWriter") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, strings.ReplaceAll(source, "\r\n", "\n"), 0)
		if err != nil {
			failures["parse:"+name] = true
			continue
		}
		parsed[name] = map[string][]*ast.FuncDecl{}
		for _, declaration := range file.Decls {
			if function, ok := declaration.(*ast.FuncDecl); ok && function.Body != nil {
				parsed[name][function.Name.Name] = append(parsed[name][function.Name.Name], function)
				ast.Inspect(function.Body, func(n ast.Node) bool {
					if ident, ok := n.(*ast.Ident); ok && ident.Name == "openPrivateWriter" && (name != "files.go" || function.Name.Name != "openRegularWriter") {
						failures["writer_open_caller:"+name+":"+function.Name.Name] = true
					}
					return true
				})
			}
		}
	}
	for _, site := range writerOpenSites {
		count := 0
		for _, function := range parsed[site[0]][site[1]] {
			for _, call := range writerCalls(function.Body) {
				if text, fun := types.ExprString(call), types.ExprString(call.Fun); text == site[2] {
					count++
				} else if fun == "openRegularWriter" || slices.Contains(writerOpeners, fun) {
					failures["writer_open:"+site[1]+":"+text] = true
				}
			}
		}
		failures["writer_open_count:"+site[1]] = count != 1
	}
	other, windows, syscalls := parsed["files_other.go"], parsed["files_windows.go"], []string(nil)
	for _, function := range other["blockingWriter"] {
		for _, call := range writerCalls(function.Body) {
			if selector, ok := call.Fun.(*ast.SelectorExpr); ok && types.ExprString(selector.X) == "syscall" {
				syscalls = append(syscalls, types.ExprString(call))
			}
		}
	}
	failures["writer_unix_open"] = writerReturn(other, "openPrivateWriter") != "return waitOutLease(path, os.Lstat, func() (*os.File, error) { return os.OpenFile(path, flags | syscall.O_NONBLOCK | syscall.O_NOFOLLOW, mode) })"
	failures["writer_blocking_unix"] = len(other["blockingWriter"]) != 1 || !slices.Equal(syscalls, []string{"syscall.SetNonblock(int(fd), false)"})
	failures["writer_windows_open"] = writerReturn(windows, "openPrivateWriter") != "return openPrivateFile(path, flags, mode)" || writerReturn(windows, "blockingWriter") != "return nil"
	failures["writer_check_order"] = !writerCheckOrder(parsed["files.go"]["openRegularWriter"])
	writerReferences(sources, failures)
	for key, failed := range failures {
		if failed {
			result = append(result, key)
		}
	}
	slices.Sort(result)
	return result
}

// The every-platform contract of the journal write opens, with LF and CRLF
// endings; each row's mutation reports its key with both.
func TestWriterOpenSourceContract(t *testing.T) {
	sources := admissionReadSources(t)
	for name, source := range sources {
		sources[name] = strings.ReplaceAll(source, "\r\n", "\n")
	}
	ended := func(input map[string]string, ending string) map[string]string {
		output := map[string]string{}
		for name, source := range input {
			output[name] = strings.ReplaceAll(source, "\n", ending)
		}
		return output
	}
	for _, ending := range []string{"\n", "\r\n"} {
		if failures := writerOpenContract(ended(sources, ending)); len(failures) != 0 {
			t.Fatalf("writer open source contract failed with keys %v", failures)
		}
	}
	const journalOpen = `openRegularWriter(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600, irregular, errors.New("cannot open local cell journal"))`
	for _, row := range []struct{ name, file, before, after, want string }{
		{"journal_open_private_file", "files.go", journalOpen, "openPrivateFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)", "writer_open:journalCause:openPrivateFile(path, os.O_WRONLY | os.O_CREATE | os.O_APPEND, 0o600)"},
		{"journal_open_flags", "files.go", "os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600, irregular", "os.O_WRONLY|os.O_CREATE, 0o600, irregular", `writer_open:journalCause:openRegularWriter(path, os.O_WRONLY | os.O_CREATE, 0o600, irregular, errors.New("cannot open local cell journal"))`},
		{"discard_open_private_writer", "files.go", `openRegularWriter(c.path("journal", "operations.jsonl"), os.O_WRONLY, 0, torn, torn)`, `openPrivateWriter(c.path("journal", "operations.jsonl"), os.O_WRONLY, 0)`, "writer_open_caller:files.go:discardJournalTail"},
		{"retained_open_private_file", "removal_retention.go", `openRegularWriter(c.path("journal", "operations.jsonl"), os.O_WRONLY, 0, unavailable, unavailable)`, `openPrivateFile(c.path("journal", "operations.jsonl"), os.O_WRONLY, 0)`, "writer_open_count:finishRetainedRemoval"},
		{"unix_no_nonblock", "files_other.go", "return os.OpenFile(path, flags|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, mode)", "return os.OpenFile(path, flags|syscall.O_NOFOLLOW, mode)", "writer_unix_open"},
		{"unix_no_nofollow", "files_other.go", "return os.OpenFile(path, flags|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, mode)", "return os.OpenFile(path, flags|syscall.O_NONBLOCK, mode)", "writer_unix_open"},
		{"unix_blocking_kept", "files_other.go", "syscall.SetNonblock(int(fd), false)", "syscall.SetNonblock(int(fd), true)", "writer_blocking_unix"},
		{"windows_plain_open", "files_windows.go", "return openPrivateFile(path, flags, mode)", "return os.OpenFile(path, flags, mode)", "writer_windows_open"},
		{"check_by_name", "files.go", "if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {", "if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() {", "writer_check_order"},
		{"blocking_not_restored", "files.go", "if err := blockingWriter(f); err != nil {", "if err := error(nil); err != nil {", "writer_check_order"},
	} {
		t.Run(row.name, func(t *testing.T) {
			if strings.Count(sources[row.file], row.before) != 1 {
				t.Fatal("writer open contract mutation anchor is not unique")
			}
			mutated := maps.Clone(sources)
			mutated[row.file] = strings.Replace(sources[row.file], row.before, row.after, 1)
			for _, ending := range []string{"\n", "\r\n"} {
				if failures := writerOpenContract(ended(mutated, ending)); !slices.Contains(failures, row.want) {
					t.Fatalf("writer open source contract reported keys %v without the row key", failures)
				}
			}
		})
	}
	// G1: the review's six reopens (rows 1 to 6), aliases declared in another file, a renamed
	// import, the other names, the pin, a type error, a non-standard import left unresolved.
	appendBlock := "\tf, err := " + journalOpen + "\n\tif err != nil {\n\t\treturn err\n\t}\n"
	discardBlock := "\tf, err := openRegularWriter(c.path(\"journal\", \"operations.jsonl\"), os.O_WRONLY, 0, torn, torn)\n\tif err != nil {\n\t\treturn err\n\t}\n"
	retainedBlock := "\t\tf, err := openRegularWriter(c.path(\"journal\", \"operations.jsonl\"), os.O_WRONLY, 0, unavailable, unavailable)\n\t\tif err != nil {\n\t\t\treturn err\n\t\t}\n"
	const progress, boundary, journalName = "// journalProgress contains only", "func (c *Cell) removalBoundary(step string) error {", `c.path("journal", "operations.jsonl")`
	reopen := func(open, failure string) string {
		return "\t_ = f.Close()\n\tif f, err = " + open + "; err != nil {\n\t\treturn " + failure + "\n\t}\n"
	}
	wrapper := func(name, flags string) string {
		return "func " + name + "(path string) (*os.File, error) {\n\treturn os.OpenFile(path, " + flags + ", 0)\n}\n\n"
	}
	for _, row := range []struct {
		name, want string
		edits      []string // file, anchor, replacement, ...
	}{
		{"reopen_wrapper_append", "writer_reference:files.go:os.OpenFile", []string{"files.go", appendBlock, appendBlock + reopen("reopenJournal(path)", `errors.New("cannot open local cell journal")`), "files.go", progress, wrapper("reopenJournal", "os.O_WRONLY|os.O_APPEND") + progress}},
		{"reopen_wrapper_truncate", "writer_reference:files.go:os.OpenFile", []string{"files.go", discardBlock, discardBlock + reopen("reopenJournal("+journalName+")", `errors.New("cannot open torn local cell journal")`), "files.go", progress, wrapper("reopenJournal", "os.O_WRONLY") + progress}},
		{"reopen_wrapper_retained", "writer_reference:removal_retention.go:os.OpenFile", []string{"removal_retention.go", retainedBlock, retainedBlock + reopen("reopenRetainedJournal("+journalName+")", "unavailable"), "removal_retention.go", boundary, wrapper("reopenRetainedJournal", "os.O_WRONLY") + boundary}},
		{"reopen_package_alias", "writer_reference:files.go:localcell.openPrivateFile", []string{"files.go", progress, "var journalOpener = openPrivateFile\n\n" + progress, "files.go", appendBlock, appendBlock + reopen("journalOpener(path, os.O_WRONLY|os.O_APPEND, 0)", `errors.New("cannot open local cell journal")`)}},
		{"reopen_value_append", "writer_reference:files.go:localcell.openPrivateFile", []string{"files.go", appendBlock, "\topen := openPrivateFile\n" + appendBlock + reopen("open(path, os.O_WRONLY|os.O_APPEND, 0)", `errors.New("cannot open local cell journal")`)}},
		{"reopen_value_truncate", "writer_reference:files.go:os.OpenFile", []string{"files.go", discardBlock, "\topen := os.OpenFile\n" + discardBlock + reopen("open("+journalName+", os.O_WRONLY, 0)", "torn")}},
		{"other_file_alias", "writer_reference:files.go:localcell.openPrivateFile", []string{"files.go", appendBlock, appendBlock + reopen("openUpdateJournalFile(path, os.O_WRONLY|os.O_APPEND, 0)", "err")}},
		{"other_file_alias_selector", "writer_reference:files.go:os.OpenFile", []string{"update_record.go", "= openPrivateFile", "= os.OpenFile", "files.go", appendBlock, appendBlock + reopen("openUpdateJournalFile(path, os.O_WRONLY|os.O_APPEND, 0)", "err")}},
		{"renamed_import", "writer_reference:files.go:os.ReadFile", []string{"files.go", "import (\n", "import (\n\tstdos \"os\"\n", "files.go", progress, "var journalRead = stdos.ReadFile\n\n" + progress}},
		{"other_names", "writer_reference:files.go:localcell.readPostgresFile writer_reference:files.go:localcell.readPrivateFile writer_reference:files.go:os.Create writer_reference:files.go:os.WriteFile",
			[]string{"files.go", progress, "var journalNames = []any{os.Create, os.WriteFile, readPrivateFile, readPostgresFile}\n\n" + progress}},
		{"pinned_open_changed", "writer_reference:files.go:os.Open", []string{"files.go", writerPinnedOpen, "os.Open(path)"}},
		{"pinned_open_twice", "writer_reference:files.go:os.Open", []string{"files.go", "\td, err := " + writerPinnedOpen, "\t_, _ = " + writerPinnedOpen + "\n\td, err := " + writerPinnedOpen}},
		{"unresolved_reference", "writer_reference_types", []string{"removal_retention.go", boundary, "var journalUnresolved = readers6Unresolved\n\n" + boundary}},
		{"non_standard_import", "", []string{"files.go", "import (\n", "import (\n\t\"example.com/readers6\"\n", "files.go", progress, "var journalExample = readers6.Value\n\n" + progress}},
	} {
		t.Run(row.name, func(t *testing.T) {
			mutated := maps.Clone(sources)
			for index := 0; index+2 < len(row.edits); index += 3 {
				name, anchor := row.edits[index], row.edits[index+1]
				if strings.Count(mutated[name], anchor) != 1 {
					t.Fatal("writer reference contract mutation anchor is not unique")
				}
				mutated[name] = strings.Replace(mutated[name], anchor, row.edits[index+2], 1)
			}
			for _, ending := range []string{"\n", "\r\n"} {
				if failures := writerOpenContract(ended(mutated, ending)); strings.Join(failures, " ") != row.want {
					t.Fatalf("writer reference contract reported keys %v for the row", failures)
				}
			}
		})
	}
}

// writerRetainedCleanup: a retained removal cut after operation/done and before
// its cleanup checkpoint clear, so Repair reaches the retained-cleanup sync.
func writerRetainedCleanup(t *testing.T) *Cell {
	t.Helper()
	c := retainedRemovalFixture(t, "legacy", false).cell
	c.cfg.Failpoint = func(op, step string) error {
		if op == "remove" && step == "before_cleanup_clear" {
			return errors.New("synthetic crash")
		}
		return nil
	}
	if err := c.Remove(t.Context(), RemoveOptions{}); !errors.Is(err, errInterrupted) {
		t.Fatal("cannot interrupt the retained removal before its cleanup clear")
	}
	c.cfg.Failpoint = nil
	return c
}

func writerTornCell(t *testing.T) (*Cell, journalProgress) {
	t.Helper()
	c, _ := unitCell(t)
	err := testWriteFile(readerJournalPath(c), []byte(readerJournalLine+`{"operation":"install","st`), 0o600)
	progress, readErr := c.readJournal()
	if err != nil || readErr != nil || !progress.tornTail {
		t.Fatal("cannot prepare the torn journal")
	}
	return c, progress
}

// writerRecords counts the records when every line is one whole record, else -1.
func writerRecords(c *Cell) int {
	data, err := os.ReadFile(readerJournalPath(c))
	lines := bytes.Split(bytes.TrimSuffix(data, []byte("\n")), []byte("\n"))
	for _, line := range lines {
		if entry := (journalEntry{}); json.Unmarshal(line, &entry) != nil || entry.Operation == "" || entry.At.IsZero() {
			return -1
		}
	}
	if err != nil || !bytes.HasSuffix(data, []byte("\n")) {
		return -1
	}
	return len(lines)
}

// The journal writers still write on every platform, and a regular file at
// journal/ is refused by both opens, never taken for an absent journal, though
// Windows reports not-exist for the name (R9). Every row passes at HEAD.
func TestJournalWritersStillWrite(t *testing.T) {
	syncs := func(t *testing.T) *int {
		saved, calls := syncJournalDirectory, new(int)
		t.Cleanup(func() { syncJournalDirectory = saved })
		syncJournalDirectory = func(name string) error { *calls++; return saved(name) }
		return calls
	}
	t.Run("absent", func(t *testing.T) {
		c, _ := unitCell(t)
		if calls := syncs(t); c.journal("stop", "operation", "started") != nil || writerRecords(c) != 1 || *calls != 1 {
			t.Fatal("an absent journal was not created with one whole record and one directory sync")
		}
	})
	t.Run("complete", func(t *testing.T) {
		c, _ := unitCell(t)
		if c.journal("stop", "operation", "started") != nil || c.journal("stop", "operation", "done") != nil {
			t.Fatal("cannot prepare the complete journal")
		}
		if calls := syncs(t); c.journal("stop", "operation", "started") != nil || writerRecords(c) != 3 || *calls != 0 {
			t.Fatal("a complete journal did not gain one whole record without a directory sync")
		}
	})
	t.Run("torn", func(t *testing.T) {
		c, progress := writerTornCell(t)
		if c.discardJournalTail(progress) != nil {
			t.Fatal("the torn journal tail was not discarded")
		}
		if info, err := os.Lstat(readerJournalPath(c)); err != nil || info.Size() != progress.durableBytes || writerRecords(c) != 1 {
			t.Fatal("the discarded journal is not its durable prefix")
		}
	})
	t.Run("retained_sync", func(t *testing.T) {
		c, saved, journalSyncs := writerRetainedCleanup(t), syncFile, 0
		t.Cleanup(func() { syncFile = saved })
		syncFile = func(f *os.File) error {
			if filepath.Base(f.Name()) == "operations.jsonl" {
				journalSyncs++
			}
			return saved(f)
		}
		err := c.Repair(t.Context())
		if m, manifestErr := readManifest(c.cfg.Root); err != nil || journalSyncs != 1 || manifestErr != nil || m.Cleanup != nil {
			t.Fatal("repair did not sync the retained journal once and clear the cleanup checkpoint")
		}
	})
	// P3 on every platform: a regular file put at the journal's name after the
	// read is refused by both journal writes with their open texts, nothing
	// truncated or synced; the moved journal and the cleanup marker are kept.
	for _, site := range []string{"torn", "retained"} {
		t.Run(site+"_replaced", func(t *testing.T) {
			var c *Cell
			var progress journalProgress
			if site == "torn" {
				c, progress = writerTornCell(t)
			} else {
				c = writerRetainedCleanup(t)
			}
			saved, syncs, bystander := syncFile, 0, []byte("bystander")
			t.Cleanup(func() { syncFile = saved })
			syncFile = func(f *os.File) error {
				syncs += map[bool]int{true: 1}[filepath.Base(f.Name()) == "operations.jsonl"]
				return saved(f)
			}
			name := readerJournalPath(c)
			before, err := os.ReadFile(name)
			replace := func() { err = errors.Join(err, os.Rename(name, name+".moved"), testWriteFile(name, bystander, 0o600)) }
			got, want := error(nil), "cannot open torn local cell journal"
			if site == "torn" {
				replace()
				got = c.discardJournalTail(progress)
			} else {
				want, c.cfg.Failpoint = "cannot open local cell journal", func(op, step string) error {
					if op == "remove" && step == "cleanup_journal" {
						replace()
					}
					return nil
				}
				got = c.Repair(t.Context())
			}
			now, nowErr := os.ReadFile(name)
			moved, movedErr := os.ReadFile(name + ".moved")
			m, manifestErr := readManifest(c.cfg.Root)
			if err != nil || got == nil || got.Error() != want || nowErr != nil || !bytes.Equal(now, bystander) || movedErr != nil || !bytes.Equal(moved, before) ||
				syncs != 0 || manifestErr != nil || site == "retained" && m.Cleanup == nil {
				t.Fatal("a regular file put at the journal name after the read was not refused with nothing truncated or synced")
			}
		})
	}
	t.Run("journal_directory_file", func(t *testing.T) {
		c, _ := unitCell(t)
		if os.RemoveAll(c.path("journal")) != nil || testWriteFile(c.path("journal"), []byte("planted"), 0o600) != nil {
			t.Fatal("cannot put a regular file at the journal directory")
		}
		appendErr, discardErr := c.journal("stop", "operation", "started"), c.discardJournalTail(journalProgress{tornTail: true})
		data, err := os.ReadFile(c.path("journal"))
		if appendErr == nil || appendErr.Error() != "cannot open local cell journal" || discardErr == nil ||
			discardErr.Error() != "cannot open torn local cell journal" || err != nil || string(data) != "planted" {
			t.Fatal("a regular file at the journal directory was not refused by both journal opens")
		}
	})
}
