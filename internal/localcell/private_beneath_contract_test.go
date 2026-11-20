package localcell

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// private_beneath_unix.go's imports (none renamed), unix and seam calls by exact text and count, the unix constants it
// may name (anything else named from unix breaks a key), and the pathname calls it never names, beside any filepath function.
var (
	privateBeneathImports = []string{"errors", "golang.org/x/sys/unix", "io/fs", "math/rand/v2", "os", "strconv", "strings", "time"}
	privateBeneathCalls   = map[string]int{"unix.Fstat(dir, &device)": 1, "unix.Fstat(next, &stat)": 1, "unix.Close(next)": 1,
		"unix.Openat(dir, part, unix.O_NOFOLLOW | unix.O_DIRECTORY | unix.O_CLOEXEC, 0)": 1, "unix.Fstat(dir, &parent)": 1, "unix.Fstat(fd, &stat)": 1,
		"unix.Openat(dir, last, unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC, 0)": 1, "unix.Close(fd)": 2, "unix.Mkdirat(dir, last, 0o700)": 1,
		"unix.Geteuid()": 1, "unix.Openat(parent, name, unix.O_RDWR | unix.O_CREAT | unix.O_EXCL | unix.O_NOFOLLOW | unix.O_CLOEXEC, 0o600)": 1,
		"unix.Fstatat(parent, to, &stat, unix.AT_SYMLINK_NOFOLLOW)": 1, "unix.Renameat(parent, from, parent, to)": 2, "unix.Fstatat(dir, last, &info.stat, unix.AT_SYMLINK_NOFOLLOW)": 1,
		"unix.Openat(dir, last, unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_NOCTTY | unix.O_CLOEXEC, 0)": 1, "unix.FcntlInt(uintptr(fd), unix.F_DUPFD_CLOEXEC, 0)": 1,
		"unix.Fstatat(fd, names[index], &stat, unix.AT_SYMLINK_NOFOLLOW)": 1, "unix.Unlinkat(dir, last, 0)": 1, "unix.Unlinkat(dir, last, unix.AT_REMOVEDIR)": 1,
		"unix.Fstat(fd, &self)": 1, "unix.Fstatat(fd, name, &stat, unix.AT_SYMLINK_NOFOLLOW)": 1, "unix.Unlinkat(fd, name, 0)": 1, "unix.Unlinkat(fd, name, unix.AT_REMOVEDIR)": 1,
		"privateBeneathSameDevice(uint64(self.Dev), uint64(stat.Dev))": 1, "dir.Sync()": 1}
	privateBeneathConstants = strings.Fields("AT_REMOVEDIR AT_SYMLINK_NOFOLLOW EBADF EEXIST EINVAL ENOENT ENOTDIR EXDEV F_DUPFD_CLOEXEC O_CLOEXEC O_CREAT O_DIRECTORY O_EXCL O_NOCTTY O_NOFOLLOW O_NONBLOCK O_RDONLY O_RDWR S_IFDIR S_IFIFO S_IFLNK S_IFMT S_IFREG Stat_t")
	privateBeneathPathname  = strings.Fields("os.Open os.OpenFile os.Lstat os.Stat os.Mkdir os.MkdirAll os.Rename os.Remove os.RemoveAll os.ReadDir os.Create os.CreateTemp os.Chmod os.OpenRoot unix.Fsync")
)

// privateBeneathContract returns the keys sources break, sorted: the walk file's build line, imports, calls, constants,
// pathname calls and os uses (root.Open("."), a called os.NewFile, no other root method or os name), and any other
// non-test file but the copy's tree naming privateBeneath... or errPrivateBeneath... (R7).
func privateBeneathContract(sources map[string]string) []string {
	const walk = "private_beneath_unix.go"
	keys, calls, texts, roots, called, imports := map[string]bool{}, map[string]int{}, map[string]int{}, map[string]bool{}, map[ast.Expr]string{}, []string{}
	file, err := parser.ParseFile(token.NewFileSet(), walk, sources[walk], parser.SkipObjectResolution)
	if err != nil {
		return []string{"private_beneath_parse:" + walk}
	}
	keys["private_beneath_build"] = !strings.HasPrefix(strings.ReplaceAll(sources[walk], "\r\n", "\n"), "//go:build linux || darwin\n") // R9
	for _, spec := range file.Imports {
		path, _ := strconv.Unquote(spec.Path.Value)
		imports = append(imports, strings.TrimSpace(map[bool]string{true: spec.Name.String()}[spec.Name != nil]+" "+path))
	}
	for _, path := range slices.Concat(imports, privateBeneathImports) {
		keys["private_beneath_import:"+path] = !slices.Contains(imports, path) || !slices.Contains(privateBeneathImports, path)
	}
	for node := range ast.Preorder(file) {
		switch node := node.(type) {
		case *ast.Field:
			for _, name := range node.Names {
				roots[name.Name] = roots[name.Name] || types.ExprString(node.Type) == "*os.Root"
			}
		case *ast.CallExpr:
			called[node.Fun] = types.ExprString(node)
			texts[called[node.Fun]]++
		case *ast.SelectorExpr:
			x, text, call := types.ExprString(node.X), types.ExprString(node), called[node]
			switch {
			case x == "filepath" || slices.Contains(privateBeneathPathname, text):
				keys["private_beneath_pathname:"+text] = true
			case x == "unix" && call != "":
				calls[call]++
			case x == "unix" && !slices.Contains(privateBeneathConstants, node.Sel.Name):
				keys["private_beneath_call:"+text] = true // a function as a value, or another constant
			case x == "os" && text != "os.Root" && text != "os.File" && (text != "os.NewFile" || call == ""), strings.HasPrefix(x, "(*os."), roots[strings.Trim(x, "(*)")] && call != x+`.Open(".")`:
				keys["private_beneath_os:"+text] = true
			}
		}
	}
	for _, text := range slices.Concat(slices.Collect(maps.Keys(calls)), slices.Collect(maps.Keys(privateBeneathCalls))) {
		keys["private_beneath_call:"+text] = keys["private_beneath_call:"+text] || texts[text] != privateBeneathCalls[text]
	}
	for name, source := range sources {
		if name == walk || name == "checkpoint_tree_unix.go" || !strings.Contains(source, "rivateBeneath") {
			continue // the copy's tree, and a file that cannot name the walk's declarations, are not parsed
		}
		other, err := parser.ParseFile(token.NewFileSet(), name, source, parser.SkipObjectResolution)
		for node := range ast.Preorder(other) {
			if identifier, ok := node.(*ast.Ident); ok && (strings.HasPrefix(identifier.Name, "privateBeneath") || strings.HasPrefix(identifier.Name, "errPrivateBeneath")) {
				keys["private_beneath_reference:"+name+":"+identifier.Name] = true
			}
		}
		keys["private_beneath_parse:"+name] = err != nil
	}
	maps.DeleteFunc(keys, func(_ string, failed bool) bool { return !failed })
	return slices.Sorted(maps.Keys(keys))
}

// The clean package gives no key, and each row (each edit matching once) exactly its keys, with LF and with CRLF.
func TestPrivateBeneathSourceContract(t *testing.T) {
	const walk, open = "private_beneath_unix.go", "private_beneath_call:unix.Openat(dir, part, unix.O_"
	sources := referencesEnding(admissionReadSources(t), "\n")
	for _, row := range []struct {
		name, file string
		edits      [][2]string
		keys       []string
	}{
		{"clean", walk, nil, nil},
		{"walk_follows", walk, [][2]string{{"unix.O_NOFOLLOW|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)", "unix.O_DIRECTORY|unix.O_CLOEXEC, 0)"}},
			[]string{open + "DIRECTORY | unix.O_CLOEXEC, 0)", open + "NOFOLLOW | unix.O_DIRECTORY | unix.O_CLOEXEC, 0)"}},
		{"walk_pathname", walk, [][2]string{{"\tparts := strings.Split(name, \"/\")\n", "\tparts := strings.Split(name, \"/\")\n\t_, _ = os.Lstat(name)\n"}}, []string{"private_beneath_pathname:os.Lstat"}},
		{"sync_raw", walk, [][2]string{{"return privateBeneathDup(fd, func(dir *os.File) error { return dir.Sync() })", "return unix.Fsync(fd)"}},
			[]string{"private_beneath_call:dir.Sync()", "private_beneath_pathname:unix.Fsync"}},
		{"reference_elsewhere", "checkpoint.go", [][2]string{{"\nfunc ClassifyCheckpointFailure(", "\nfunc zz() { _ = privateBeneathAt }\n\nfunc ClassifyCheckpointFailure("}},
			[]string{"private_beneath_reference:checkpoint.go:privateBeneathAt"}},
		{"reference_windows_tree", "checkpoint_tree_windows.go", [][2]string{{"\nfunc (t *checkpointTree) close() {}\n", "\nfunc (t *checkpointTree) close() { _ = privateBeneathAt }\n"}},
			[]string{"private_beneath_reference:checkpoint_tree_windows.go:privateBeneathAt"}},
		{"import_added", walk, [][2]string{{"\t\"errors\"\n", "\t\"errors\"\n\t\"path/filepath\"\n"}, {"\nfunc privateBeneathPart(", "\nvar _ = filepath.Base\n\nfunc privateBeneathPart("}},
			[]string{"private_beneath_import:path/filepath", "private_beneath_pathname:filepath.Base"}},
		{"unix_constant", walk, [][2]string{{"\tdir := int(held.Fd())\n", "\tdir := unix.AT_FDCWD\n"}}, []string{"private_beneath_call:unix.AT_FDCWD"}},
		{"root_method", walk, [][2]string{{"\tparts := strings.Split(name, \"/\")\n", "\tparts := strings.Split(name, \"/\")\n\t_ = root.Mkdir\n"}}, []string{"private_beneath_os:root.Mkdir"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			edited := maps.Clone(sources)
			for _, edit := range row.edits {
				if strings.Count(edited[row.file], edit[0]) != 1 {
					t.Fatal("a private beneath contract row does not match its file exactly once")
				}
				edited[row.file] = strings.Replace(edited[row.file], edit[0], edit[1], 1)
			}
			for _, ending := range []string{"\n", "\r\n"} {
				if keys := privateBeneathContract(referencesEnding(edited, ending)); !slices.Equal(keys, row.keys) {
					t.Fatalf("private beneath contract keys %q", keys)
				}
			}
		})
	}
}
