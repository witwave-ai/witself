package localcell

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// syncDirectoryOpen is the one open syncDirectory makes: the name, a separator
// and a dot, which resolve only a directory (LC-READERS-5 R6).
const syncDirectoryOpen = `os.Open(path + string(filepath.Separator) + ".")`

// syncDirectoryOpeners are the calls that open a name.
var syncDirectoryOpeners = []string{"os.Open", "os.OpenFile", "os.OpenRoot", "openPrivateFile", "openPrivateReader", "syscall.Open"}

// syncDirectoryContract returns every failed check of syncDirectory in
// files.go: exactly one call that opens anything, spelled syncDirectoryOpen.
// Any other opener call, or an opener named without being called, is
// sync_directory_open:<text>; zero or more than one open is
// sync_directory_open_count. Windows never runs the open (it returns first),
// so on Windows this pins only the source text.
func syncDirectoryContract(source string) []string {
	file, err := parser.ParseFile(token.NewFileSet(), "files.go", strings.ReplaceAll(source, "\r\n", "\n"), 0)
	if err != nil {
		return []string{"parse:files.go"}
	}
	failures, opens := map[string]bool{}, 0
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Recv != nil || function.Name.Name != "syncDirectory" || function.Body == nil {
			continue
		}
		called := map[ast.Node]bool{}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			call, isCall := node.(*ast.CallExpr)
			if isCall && slices.Contains(syncDirectoryOpeners, types.ExprString(call.Fun)) {
				called[call.Fun], opens = true, opens+1
				failures["sync_directory_open:"+types.ExprString(call)] = types.ExprString(call) != syncDirectoryOpen
			} else if expression, ok := node.(ast.Expr); ok && !isCall && !called[node] && slices.Contains(syncDirectoryOpeners, types.ExprString(expression)) {
				failures["sync_directory_open:"+types.ExprString(expression)] = true
			}
			return true
		})
	}
	failures["sync_directory_open_count"] = opens != 1
	var result []string
	for key, failed := range failures {
		if failed {
			result = append(result, key)
		}
	}
	slices.Sort(result)
	return result
}

// The every-platform contract of syncDirectory's one open, with LF and CRLF
// endings; each row's mutation reports its key with both.
func TestSyncDirectorySourceContract(t *testing.T) {
	source := strings.ReplaceAll(admissionReadSources(t)["files.go"], "\r\n", "\n")
	for _, ending := range []string{"\n", "\r\n"} {
		if failures := syncDirectoryContract(strings.ReplaceAll(source, "\n", ending)); len(failures) != 0 {
			t.Fatalf("sync directory source contract failed with keys %v", failures)
		}
	}
	const syncLine = "\tif err := d.Sync(); err != nil {"
	for _, row := range []struct{ name, before, after, want string }{
		{"sync_directory_plain", syncDirectoryOpen, "os.Open(path)", "sync_directory_open:os.Open(path)"},
		{"sync_directory_join", syncDirectoryOpen, `os.Open(filepath.Join(path, "."))`, `sync_directory_open:os.Open(filepath.Join(path, "."))`},
		{"sync_directory_openfile", syncDirectoryOpen, "os.OpenFile(path, os.O_RDONLY, 0)", "sync_directory_open:os.OpenFile(path, os.O_RDONLY, 0)"},
		{"sync_directory_second_open", syncLine, "\tif second, err := " + syncDirectoryOpen + "; err == nil { _ = second.Close() }; if err := d.Sync(); err != nil {", "sync_directory_open_count"},
		{"sync_directory_open_value", syncLine, "\treopen := os.Open; _ = reopen; if err := d.Sync(); err != nil {", "sync_directory_open:os.Open"},
		{"sync_directory_no_open", syncDirectoryOpen, "os.NewFile(0, path)", "sync_directory_open_count"},
	} {
		t.Run(row.name, func(t *testing.T) {
			if strings.Count(source, row.before) != 1 {
				t.Fatal("sync directory contract mutation anchor is not unique")
			}
			for _, ending := range []string{"\n", "\r\n"} {
				mutated := strings.ReplaceAll(strings.Replace(source, row.before, row.after, 1), "\n", ending)
				if failures := syncDirectoryContract(mutated); !slices.Contains(failures, row.want) {
					t.Fatalf("sync directory source contract reported keys %v without the row key", failures)
				}
			}
		})
	}
}

// A private directory and one inside it still sync on every platform; on
// Windows syncDirectory returns before any open, by design.
func TestSyncDirectoryStillSyncs(t *testing.T) {
	dir := testPrivateRoot(t)
	child := filepath.Join(dir, "child")
	if testMkdir(child, 0o700) != nil || testWriteFile(filepath.Join(child, "record"), []byte("record\n"), 0o600) != nil {
		t.Fatal("cannot prepare the private directory to sync")
	}
	for _, name := range []string{dir, child} {
		if err := syncDirectory(name); err != nil {
			t.Fatalf("a private directory did not sync, with the fixed text %q", err.Error())
		}
	}
}
