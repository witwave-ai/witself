package localcell

import (
	"cmp"
	"errors"
	"go/ast"
	"go/build"
	"go/parser"
	"go/types"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
)

// readers7Waiting can wait on or follow what stands at a name, by declaration;
// openPrivateWriter's handle is one no check has seen.
var readers7Waiting = strings.Fields(`os.Open os.OpenFile os.Create os.ReadFile os.WriteFile os.ReadDir os.DirFS os.OpenRoot os.OpenInRoot
	(*os.Root).Open (*os.Root).OpenFile (*os.Root).Create (*os.Root).ReadFile (*os.Root).WriteFile (*os.Root).OpenRoot (*os.Root).FS
	io/ioutil.ReadFile io/ioutil.WriteFile io/ioutil.ReadDir net/http.Dir (net/http.Dir).Open net/http.ServeFile syscall.Open syscall.Openat
	syscall.Creat golang.org/x/sys/unix.Open golang.org/x/sys/unix.Openat golang.org/x/sys/unix.Openat2 golang.org/x/sys/unix.Creat
	localcell.openPrivateFile localcell.readPrivateFile localcell.readPostgresFile localcell.openPlainReader localcell.openPrivateWriter`)

// readers7Pinned are logFile's checked open's arguments, a local as defined.
var readers7Pinned = []string{"context.Background()", `c.path("logs", component + ".log")`, "os.O_WRONLY | os.O_CREATE | os.O_APPEND", "0o600",
	`errors.New("local cell log must be a regular file")`, `errors.New("cannot open local cell log")`}

// readers7Contract type-checks the package sources as built here (sharing
// readers4's parse state) and returns commands.go's failed rules, sorted:
// types; each waiting opener referenced, directly, as a method or through a
// package variable declared as one; and logFile's checked open, the file's
// one reference to it, returned as is with its pinned arguments.
func readers7Contract(sources map[string]string) []string {
	failed, admit, files := map[string]bool{}, map[string]bool{}, []*ast.File{}
	fail := func(key string, bad bool) { failed[key] = failed[key] || bad }
	var commands *ast.File
	for _, name := range slices.Sorted(maps.Keys(sources)) {
		file, err := parser.ParseFile(readers4Fset, name, sources[name], 0)
		if fail("types", err != nil); name == "commands.go" {
			commands = file
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
		fail("types", file != nil && file.Name() == "commands.go")
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
		if function, ok := object.(*types.Func); ok && function.Signature().Recv() != nil {
			return function.FullName()
		} else if object == nil || object.Pkg() == nil || object.Parent() != object.Pkg().Scope() {
			return ""
		}
		return object.Pkg().Path() + "." + object.Name()
	}
	logFile, calls, references := (*ast.FuncDecl)(nil), []*ast.CallExpr(nil), 0
	ast.Inspect(commands, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.FuncDecl:
			if node.Recv != nil && node.Name.Name == "logFile" && node.Body != nil {
				logFile = node
			}
		case *ast.CallExpr:
			if declared(named(node.Fun)) == "localcell.openRegularWriter" {
				calls = append(calls, node)
			}
		case *ast.Ident:
			if name := declared(info.Uses[node]); name == "localcell.openRegularWriter" {
				references++
			} else if slices.Contains(readers7Waiting, name) {
				failed[name] = true
			}
		}
		return true
	})
	fail("log_open_count", logFile == nil || references != 1 || len(calls) != 1 || calls[0].Pos() < logFile.Body.Pos() || calls[0].End() > logFile.Body.End())
	if !failed["log_open_count"] {
		last, _ := logFile.Body.List[len(logFile.Body.List)-1].(*ast.ReturnStmt)
		fail("log_open_returned", last == nil || len(last.Results) != 1 || last.Results[0] != calls[0])
		defined, arguments := map[types.Object]string{}, []string{}
		for _, statement := range logFile.Body.List {
			if assign, ok := statement.(*ast.AssignStmt); ok && len(assign.Lhs) == 1 && len(assign.Rhs) == 1 {
				if identifier, ok := assign.Lhs[0].(*ast.Ident); ok && info.Defs[identifier] != nil {
					defined[info.Defs[identifier]] = types.ExprString(assign.Rhs[0])
				}
			}
		}
		for _, argument := range calls[0].Args {
			identifier, _ := argument.(*ast.Ident)
			arguments = append(arguments, cmp.Or(defined[info.Uses[identifier]], types.ExprString(argument)))
		}
		fail("log_open_args", !slices.Equal(arguments, readers7Pinned))
	}
	maps.DeleteFunc(failed, func(_ string, bad bool) bool { return !bad })
	return slices.Sorted(maps.Keys(failed))
}

// commands.go references no opener that can wait on or follow what stands at
// a name, and logFile's checked open is its one log open: each row reopens the
// log another way or adds an open, and fails exactly its rules, LF and CRLF.
func TestLogOpenSourceContract(t *testing.T) {
	pkg, err := build.Default.ImportDir(".", 0)
	sources := map[string]string{}
	for _, name := range pkg.GoFiles {
		data, readErr := os.ReadFile(name)
		sources[name], err = strings.ReplaceAll(string(data), "\r\n", "\n"), errors.Join(err, readErr)
	}
	check := func(input map[string]string) (keys []string) {
		for _, ending := range []string{"\n", "\r\n"} {
			ended := map[string]string{}
			for name, source := range input {
				ended[name] = strings.ReplaceAll(source, "\n", ending)
			}
			keys = append(keys, strings.Join(readers7Contract(ended), " "))
		}
		return keys
	}
	if err != nil || !slices.Equal(check(sources), []string{"", ""}) {
		t.Fatal("log open source contract refused the tree")
	}
	const open, second = `return openRegularWriter(context.Background(), path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600, irregular, errors.New("cannot open local cell log"))`, `log, err := c.logFile("postgres")`
	reopen := func(before, call string) func(string) string {
		return func(source string) string {
			return strings.Replace(source, open, before+"f, err := "+open[len("return "):]+"\n\tif err != nil {\n\t\treturn nil, err\n\t}\n\t_ = f.Close()\n\treturn "+call, 1)
		}
	}
	replace := func(from, to string) func(string) string {
		return func(source string) string { return strings.Replace(source, from, to, 1) }
	}
	for _, row := range []struct {
		name, want string
		change     func(string) string
	}{
		{"old_helper", "localcell.openPrivateFile log_open_count", replace(open, "return openPrivateFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)")},
		{"same_file_wrapper", "log_open_returned os.OpenFile", func(source string) string {
			return reopen("", "readers7Reopen(path)")(source) + "\nfunc readers7Reopen(path string) (*os.File, error) {\n\treturn os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)\n}\n"
		}},
		{"package_alias", "localcell.openPrivateFile log_open_returned", reopen("", "openUpdateJournalFile(path, os.O_WRONLY|os.O_APPEND, 0)")},
		{"function_value", "localcell.openPrivateFile log_open_returned", reopen("open := openPrivateFile\n\t", "open(path, os.O_WRONLY|os.O_APPEND, 0)")},
		{"renamed_import", "log_open_returned os.OpenFile", func(source string) string {
			return replace("import (\n", "import (\n\tstdos \"os\"\n")(reopen("", "stdos.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)")(source))
		}},
		{"root_method", "(*os.Root).OpenFile log_open_returned os.OpenRoot", reopen("root, _ := os.OpenRoot(c.path(\"logs\"))\n\t", "root.OpenFile(component+\".log\", os.O_WRONLY|os.O_APPEND, 0)")},
		{"second_checked_open", "log_open_count", replace(second, `log, err := openRegularWriter(context.Background(), c.path("logs", "postgres.log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600, nil, nil)`)},
		{"second_waiting_open", "localcell.openPrivateFile", replace(second, `log, err := openPrivateFile(c.path("logs", "postgres.log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)`)},
		{"mode_changed", "log_open_args", replace("APPEND, 0o600, irregular", "APPEND, 0o644, irregular")},
		{"unresolved_reference", "types", replace(open, "_ = readers7Unresolved\n\t"+open)},
	} {
		t.Run(row.name, func(t *testing.T) {
			mutant := maps.Clone(sources)
			if mutant["commands.go"] = row.change(sources["commands.go"]); mutant["commands.go"] == sources["commands.go"] || !slices.Equal(check(mutant), []string{row.want, row.want}) {
				t.Fatal("log open source contract missed its mutation")
			}
		})
	}
}

// The log still opens on every platform: created when absent, appended to when
// present, through a regular handle. A purged logs/ and a regular file at logs
// (Windows reports not-exist for the name) are refused with the open text,
// never taken for an absent log, the planted bytes kept.
func TestLogFileStillOpens(t *testing.T) {
	for _, row := range []struct{ name, planted, want string }{{"absent", "", ""}, {"existing", "earlier line\n", ""},
		{"logs_absent", "", "cannot open local cell log"}, {"logs_regular_file", "planted", "cannot open local cell log"}} {
		t.Run(row.name, func(t *testing.T) {
			c, _ := unitCell(t)
			name, err := c.path("logs", "server.log"), error(nil)
			if row.want != "" {
				name, err = c.path("logs"), os.RemoveAll(c.path("logs"))
			}
			if row.planted != "" {
				err = errors.Join(err, testWriteFile(name, []byte(row.planted), 0o600))
			}
			f, openErr := c.logFile("server")
			if row.want != "" {
				data, _ := os.ReadFile(name)
				if err != nil || openErr == nil || openErr.Error() != row.want || string(data) != row.planted {
					t.Fatal("a missing or regular-file logs was not refused with the log open text, its bytes kept")
				}
				return
			}
			info, statErr := f.Stat()
			_, writeErr := f.WriteString("readers7 line\n")
			closeErr := f.Close()
			data, readErr := os.ReadFile(name)
			if err != nil || openErr != nil || statErr != nil || !info.Mode().IsRegular() || errors.Join(writeErr, closeErr, readErr) != nil || string(data) != row.planted+"readers7 line\n" {
				t.Fatal("the log did not open as a regular file appended to after its content")
			}
		})
	}
}
