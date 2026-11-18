package localcell

import (
	"bytes"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
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
	if declarations := functions[name]; len(declarations) == 1 && len(declarations[0].Body.List) == 1 {
		if ret, ok := declarations[0].Body.List[0].(*ast.ReturnStmt); ok && len(ret.Results) == 1 {
			return "return " + types.ExprString(ret.Results[0])
		}
	}
	return ""
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
	failures["writer_unix_open"] = writerReturn(other, "openPrivateWriter") != "return os.OpenFile(path, flags | syscall.O_NONBLOCK | syscall.O_NOFOLLOW, mode)"
	failures["writer_blocking_unix"] = len(other["blockingWriter"]) != 1 || !slices.Equal(syscalls, []string{"syscall.SetNonblock(int(fd), false)"})
	failures["writer_windows_open"] = writerReturn(windows, "openPrivateWriter") != "return openPrivateFile(path, flags, mode)" || writerReturn(windows, "blockingWriter") != "return nil"
	failures["writer_check_order"] = !writerCheckOrder(parsed["files.go"]["openRegularWriter"])
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
		{"unix_no_nonblock", "files_other.go", "flags|syscall.O_NONBLOCK|syscall.O_NOFOLLOW", "flags|syscall.O_NOFOLLOW", "writer_unix_open"},
		{"unix_no_nofollow", "files_other.go", "flags|syscall.O_NONBLOCK|syscall.O_NOFOLLOW", "flags|syscall.O_NONBLOCK", "writer_unix_open"},
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
