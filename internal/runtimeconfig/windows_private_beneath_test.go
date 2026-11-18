package runtimeconfig

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"slices"
	"strings"
	"testing"
)

func TestWindowsPrivateBeneathDecision(t *testing.T) {
	t.Run("parts", func(t *testing.T) {
		for index, name := range []string{"a", "a.b", "clone/pg/base/1/2", ".pending-drill.json"} {
			if parts, ok := windowsPrivateBeneathParts(name); !ok || strings.Join(parts, "/") != name {
				t.Fatalf("a well-formed name was refused at row %d", index)
			}
		}
		for index, name := range []string{"", "/", "/a", "a/", "a//b", ".", "..", "./a", "a/./b", "a/..", "../a",
			`a\b`, `a/b\c`, "a:b", "a/b:x", `\\?\C:\x`} {
			if parts, ok := windowsPrivateBeneathParts(name); ok || parts != nil {
				t.Fatalf("a malformed name was accepted at row %d", index)
			}
		}
	})
	t.Run("directory", func(t *testing.T) {
		for index, row := range []struct {
			attributes, volume, rootVolume uint32
			want                           bool
		}{{0x10, 7, 7, true}, {0x10 | 0x20, 7, 7, true}, {0x80, 7, 7, false}, {0x10 | 0x400, 7, 7, false}, {0x400, 7, 7, false}, {0x10, 7, 8, false}} {
			if windowsPrivateBeneathDirectory(row.attributes, row.volume, row.rootVolume) != row.want {
				t.Fatalf("the component decision differs at row %d", index)
			}
		}
	})
}

// windowsPrivateBeneathCalls are the calls of windows_private_beneath_windows.go
// by exact text and count: every call of a windows. function or conversion, of
// a windowsPrivate function other than windowsPrivateFailure (the wrapper's
// nil seam included), of the seam, of a name-resolving os function, through
// the held root, and every windows. composite literal with elements.
var windowsPrivateBeneathCalls = map[string]int{
	"windowsPrivateBeneath(root, name, create, directory, nil)": 1, "held(index)": 1, "held(len(parts) - 1)": 1,
	"windows.Handle(directoryHandle.Fd())": 1, "windows.GetFileInformationByHandle(parent, &rootInfo)": 1, "windows.GetFileInformationByHandle(next, &info)": 1,
	"windows.CloseHandle(next)": 1, "windows.CloseHandle(handle)": 1, "windows.NewNTUnicodeString(name)": 1, "windowsPrivateBeneathParts(name)": 1,
	"windowsPrivateCleanPath(`C:\\` + part)": 1, "windowsPrivateValidateHandle(handle, directory)": 1, `root.Open(".")`: 1,
	"windowsPrivateRollback(handle, err)": 1, "windowsPrivateBeneathDirectory(info.FileAttributes, info.VolumeSerialNumber, rootInfo.VolumeSerialNumber)": 1,
	"windowsPrivateDescriptor()": 1, "windows.OBJECT_ATTRIBUTES{RootDirectory: parent, ObjectName: object, Attributes: windows.OBJ_CASE_INSENSITIVE, SecurityDescriptor: descriptor}": 1,
	"windowsPrivateAt(parent, part, windows.FILE_TRAVERSE, windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE, windows.FILE_OPEN, windows.FILE_DIRECTORY_FILE, nil)":                                   1,
	"windowsPrivateAt(parent, parts[len(parts) - 1], access | windows.READ_CONTROL, windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE, disposition, options, descriptor)": 1,
	windowsPrivateBeneathNT: 1,
}

// windowsPrivateBeneathNT is the walk's one NtCreateFile call, printed.
const windowsPrivateBeneathNT = "windows.NtCreateFile(&handle, access | windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE, &attributes, &windows.IO_STATUS_BLOCK{}, nil, windows.FILE_ATTRIBUTE_NORMAL, share, disposition, options | windows.FILE_OPEN_REPARSE_POINT | windows.FILE_SYNCHRONOUS_IO_NONALERT, 0, 0)"

// windowsPrivateBeneathOrder is, in source order, every if (init and
// condition), switch case, range head, return (results) and watched call of
// windowsPrivateBeneath and then windowsPrivateAt; a line per statement.
var windowsPrivateBeneathOrder = []string{
	"call:windowsPrivateBeneathParts(name)", "range:_, part, parts", "call:windowsPrivateCleanPath(`C:\\` + part)",
	"if:;!ok || root == nil", "return:nil, windowsPrivateFailure(nil)",
	`call:root.Open(".")`, "if:;err != nil", "return:nil, windowsPrivateFailure(errno)", "call:windows.Handle(directoryHandle.Fd())",
	"if:err := windows.GetFileInformationByHandle(parent, &rootInfo);err != nil", "call:windows.GetFileInformationByHandle(parent, &rootInfo)", "return:nil, windowsPrivateFailure(err)",
	"range:index, part, parts[:len(parts) - 1]",
	"call:windowsPrivateAt(parent, part, windows.FILE_TRAVERSE, windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE, windows.FILE_OPEN, windows.FILE_DIRECTORY_FILE, nil)", "if:;err != nil", "return:nil, err",
	"call:windows.CloseHandle(next)", "call:windows.GetFileInformationByHandle(next, &info)",
	"if:;err != nil || !windowsPrivateBeneathDirectory(info.FileAttributes, info.VolumeSerialNumber, rootInfo.VolumeSerialNumber)",
	"call:windowsPrivateBeneathDirectory(info.FileAttributes, info.VolumeSerialNumber, rootInfo.VolumeSerialNumber)", "return:nil, windowsPrivateFailure(err)",
	"if:;held != nil && held(index) != nil", "call:held(index)", "return:nil, windowsPrivateFailure(nil)",
	"if:;directory", "if:;create", "if:descriptor, err = windowsPrivateDescriptor();err != nil", "call:windowsPrivateDescriptor()", "return:nil, err",
	"if:access, disposition = access | windows.DELETE, windows.FILE_CREATE;!directory",
	"call:windowsPrivateAt(parent, parts[len(parts) - 1], access | windows.READ_CONTROL, windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE, disposition, options, descriptor)", "if:;err != nil", "return:nil, err",
	"if:;held != nil && held(len(parts) - 1) != nil", "call:held(len(parts) - 1)",
	"if:err = windowsPrivateValidateHandle(handle, directory);err == nil", "call:windowsPrivateValidateHandle(handle, directory)", `return:os.NewFile(uintptr(handle), "private object"), nil`,
	"if:;create", "call:windowsPrivateRollback(handle, err)", "call:windows.CloseHandle(handle)", "return:nil, err",
	"call:windows.NewNTUnicodeString(name)", "if:;err != nil", "return:windows.InvalidHandle, windowsPrivateFailure(err)", "call:" + windowsPrivateBeneathNT,
	"case:err == nil", "return:handle, nil", "case:!errors.As(err, &status)", "return:windows.InvalidHandle, windowsPrivateFailure(err)",
	"case:status == windows.STATUS_OBJECT_NAME_COLLISION, status == windows.STATUS_FILE_IS_A_DIRECTORY && disposition == windows.FILE_CREATE",
	"return:windows.InvalidHandle, windowsPrivateFailure(windows.ERROR_FILE_EXISTS)", "return:windows.InvalidHandle, windowsPrivateFailure(status.Errno())",
}

// windowsPrivateBeneathSelectors count every windows. name the file selects
// (functions, conversions, types and constants): an unlisted name or a changed
// count is refused, so is a function value; the imports are exact.
var windowsPrivateBeneathSelectors = map[string]int{
	"ByHandleFileInformation": 1, "CloseHandle": 2, "DELETE": 2, "ERROR_FILE_EXISTS": 1, "ERROR_INVALID_HANDLE": 1, "FILE_ATTRIBUTE_NORMAL": 1,
	"FILE_CREATE": 2, "FILE_DIRECTORY_FILE": 2, "FILE_NON_DIRECTORY_FILE": 1, "FILE_OPEN": 2, "FILE_OPEN_REPARSE_POINT": 1, "FILE_READ_ATTRIBUTES": 1,
	"FILE_SHARE_DELETE": 1, "FILE_SHARE_READ": 2, "FILE_SHARE_WRITE": 2, "FILE_SYNCHRONOUS_IO_NONALERT": 1, "FILE_TRAVERSE": 2, "GENERIC_READ": 1,
	"GENERIC_WRITE": 1, "GetFileInformationByHandle": 2, "Handle": 3, "IO_STATUS_BLOCK": 1, "InvalidHandle": 5, "NTStatus": 1, "NewNTUnicodeString": 1,
	"NtCreateFile": 1, "OBJECT_ATTRIBUTES": 1, "OBJ_CASE_INSENSITIVE": 1, "READ_CONTROL": 1, "SECURITY_DESCRIPTOR": 2, "STATUS_FILE_IS_A_DIRECTORY": 1,
	"STATUS_OBJECT_NAME_COLLISION": 1, "SYNCHRONIZE": 1,
}

func TestWindowsPrivateBeneathSourceContract(t *testing.T) {
	data, err := os.ReadFile("windows_private_beneath_windows.go")
	if err != nil {
		t.Fatal("cannot read the beneath walk source")
	}
	// The Windows checkout may contain CRLF, including mutation sites.
	source := strings.ReplaceAll(string(data), "\r\n", "\n")
	if keys := windowsPrivateBeneathContract(source); len(keys) != 0 {
		t.Fatalf("the beneath walk source contract failed: %q", keys)
	}
	for _, row := range []struct{ name, old, new, key string }{
		{"follow_reparse", "options|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT", "options|windows.FILE_SYNCHRONOUS_IO_NONALERT",
			"beneath_call:" + strings.Replace(windowsPrivateBeneathNT, "windows.FILE_OPEN_REPARSE_POINT | ", "", 1)},
		{"share_delete", "windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, windows.FILE_OPEN", "windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_OPEN",
			"beneath_call:windowsPrivateAt(parent, part, windows.FILE_TRAVERSE, windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE, windows.FILE_OPEN, windows.FILE_DIRECTORY_FILE, nil)"},
		{"absolute", "RootDirectory: parent,", "RootDirectory: 0,",
			"beneath_call:windows.OBJECT_ATTRIBUTES{RootDirectory: 0, ObjectName: object, Attributes: windows.OBJ_CASE_INSENSITIVE, SecurityDescriptor: descriptor}"},
		{"pathname", "\tobject, err := windows.NewNTUnicodeString(name)", "\t_, _ = windowsPrivateOpenMetadata(name, 0)\n\tobject, err := windows.NewNTUnicodeString(name)",
			"beneath_call:windowsPrivateOpenMetadata(name, 0)"},
		{"second_create", "\truntime.KeepAlive(descriptor)", "\terr = " + windowsPrivateBeneathNT + "\n\truntime.KeepAlive(descriptor)", "beneath_count:" + windowsPrivateBeneathNT},
		{"root_follow", `root.Open(".")`, "root.Open(name)", "beneath_call:root.Open(name)"},
		{"create_if", "windows.DELETE, windows.FILE_CREATE; !directory", "windows.DELETE, windows.FILE_OPEN_IF; !directory", "beneath_selector:FILE_OPEN_IF"},
		{"function_value", "\tvar status windows.NTStatus", "\tvar status windows.NTStatus\n\t_ = windows.DeleteFile", "beneath_selector:DeleteFile"},
		{"import", "\t\"unsafe\"\n", "\t\"syscall\"\n\t\"unsafe\"\n", "beneath_imports"},
		{"seam_gate", "if err != nil || !windowsPrivateBeneathDirectory(", "if err != nil || held != nil && !windowsPrivateBeneathDirectory(",
			"beneath_statement:if:;err != nil || held != nil && !windowsPrivateBeneathDirectory(info.FileAttributes, info.VolumeSerialNumber, rootInfo.VolumeSerialNumber)"},
		{"refusal_return", "\t\t\treturn nil, windowsPrivateFailure(err)\n\t\t}\n\t\tif held", "\t\t\t_ = windowsPrivateFailure(err)\n\t\t}\n\t\tif held", "beneath_statement_count:return:nil, windowsPrivateFailure(err)"},
		{"rollback_guard", "\tif create {\n\t\terr = windowsPrivateRollback", "\tif !create {\n\t\terr = windowsPrivateRollback", "beneath_statement:if:;!create"},
		{"close_order", "\tif create {\n\t\terr = windowsPrivateRollback(handle, err)\n\t}\n\t_ = windows.CloseHandle(handle)\n",
			"\t_ = windows.CloseHandle(handle)\n\tif create {\n\t\terr = windowsPrivateRollback(handle, err)\n\t}\n", "beneath_order:call:windows.CloseHandle(handle)"},
		{"validate_created", "directory); err == nil {", "directory); err == nil || create {", "beneath_statement:if:err = windowsPrivateValidateHandle(handle, directory);err == nil || create"},
		{"validate_directory", "directory); err == nil {", "directory); err == nil || directory {", "beneath_statement:if:err = windowsPrivateValidateHandle(handle, directory);err == nil || directory"},
		{"seam_second", "\tif held != nil && held(len(parts)-1) != nil {\n\t\terr = windowsPrivateFailure(nil)\n\t} else if err", "\tif err", "beneath_count:held(len(parts) - 1)"},
		{"wrapper_seam", "directory, nil)", "directory, func(int) error { return nil })", "beneath_call:windowsPrivateBeneath(root, name, create, directory, (func(int) error literal))"},
	} {
		t.Run(row.name, func(t *testing.T) {
			if strings.Count(source, row.old) != 1 {
				t.Fatal("a mutation must replace exactly one source site")
			}
			if keys := windowsPrivateBeneathContract(strings.Replace(source, row.old, row.new, 1)); !slices.Contains(keys, row.key) {
				t.Fatalf("the beneath walk source contract accepted the mutant: %q", keys)
			}
		})
	}
}

// windowsPrivateBeneathContract returns, sorted, every way source differs from
// the pinned imports, windows. selectors, calls and ordered statements (empty
// when it matches).
func windowsPrivateBeneathContract(source string) []string {
	file, err := parser.ParseFile(token.NewFileSet(), "windows_private_beneath_windows.go", source, 0)
	if err != nil {
		return []string{"beneath_parse"}
	}
	var keys, imports []string
	for _, spec := range file.Imports {
		imports = append(imports, types.ExprString(spec.Path))
		if spec.Name != nil {
			imports = append(imports, spec.Name.Name)
		}
	}
	if strings.Join(imports, " ") != `"errors" "os" "runtime" "unsafe" "golang.org/x/sys/windows"` {
		keys = append(keys, "beneath_imports")
	}
	calls, selectors := map[string]int{}, map[string]int{}
	ast.Inspect(file, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.SelectorExpr:
			if types.ExprString(node.X) == "windows" {
				selectors[node.Sel.Name]++
			}
		case *ast.CallExpr:
			if windowsPrivateBeneathWatched(node) {
				calls[types.ExprString(node)]++
			}
		case *ast.CompositeLit:
			if strings.HasPrefix(types.ExprString(node.Type), "windows.") && len(node.Elts) != 0 {
				calls[windowsPrivateBeneathLiteral(node)]++
			}
		}
		return true
	})
	// The two functions' guards, results and watched calls, in source order.
	var order []string
	for _, declaration := range file.Decls {
		if function, ok := declaration.(*ast.FuncDecl); ok && function.Body != nil && slices.Contains([]string{"windowsPrivateBeneath", "windowsPrivateAt"}, function.Name.Name) {
			ast.Inspect(function.Body, func(node ast.Node) bool {
				switch node := node.(type) {
				case *ast.IfStmt:
					order = append(order, "if:"+windowsPrivateBeneathInit(node.Init)+";"+types.ExprString(node.Cond))
				case *ast.CaseClause:
					order = append(order, "case:"+windowsPrivateBeneathList(node.List))
				case *ast.RangeStmt:
					order = append(order, "range:"+windowsPrivateBeneathList([]ast.Expr{node.Key, node.Value, node.X}))
				case *ast.ReturnStmt:
					order = append(order, "return:"+windowsPrivateBeneathList(node.Results))
				case *ast.CallExpr:
					if windowsPrivateBeneathWatched(node) {
						order = append(order, "call:"+types.ExprString(node))
					}
				}
				return true
			})
		}
	}
	statements, pinnedStatements := map[string]int{}, map[string]int{}
	for _, statement := range order {
		statements[statement]++
	}
	for _, statement := range windowsPrivateBeneathOrder {
		pinnedStatements[statement]++
	}
	if !slices.Equal(order, windowsPrivateBeneathOrder) {
		// The first statement out of its pinned place names what moved.
		index, moved := 0, "end"
		for index < min(len(order), len(windowsPrivateBeneathOrder)) && order[index] == windowsPrivateBeneathOrder[index] {
			index++
		}
		if index < len(order) {
			moved = order[index]
		}
		keys = append(keys, "beneath_order:"+moved)
	}
	for _, pinned := range []struct {
		listed, count  string
		found, allowed map[string]int
	}{{"beneath_call:", "beneath_count:", calls, windowsPrivateBeneathCalls}, {"beneath_selector:", "beneath_selector_count:", selectors, windowsPrivateBeneathSelectors},
		{"beneath_statement:", "beneath_statement_count:", statements, pinnedStatements}} {
		for text, count := range pinned.found {
			if want, listed := pinned.allowed[text]; !listed {
				keys = append(keys, pinned.listed+text)
			} else if count != want {
				keys = append(keys, pinned.count+text)
			}
		}
		for text := range pinned.allowed {
			if pinned.found[text] == 0 {
				keys = append(keys, pinned.count+text)
			}
		}
	}
	slices.Sort(keys)
	return keys
}

// windowsPrivateBeneathLiteral spells a composite literal with its elements;
// types.ExprString shortens them.
func windowsPrivateBeneathLiteral(literal *ast.CompositeLit) string {
	elements := make([]string, len(literal.Elts))
	for index, element := range literal.Elts {
		if pair, ok := element.(*ast.KeyValueExpr); ok {
			elements[index] = types.ExprString(pair.Key) + ": " + types.ExprString(pair.Value)
		} else {
			elements[index] = types.ExprString(element)
		}
	}
	return types.ExprString(literal.Type) + "{" + strings.Join(elements, ", ") + "}"
}

// windowsPrivateBeneathWatched reports whether a call is pinned by text.
func windowsPrivateBeneathWatched(call *ast.CallExpr) bool {
	function := types.ExprString(call.Fun)
	return strings.HasPrefix(function, "windows.") || strings.HasPrefix(function, "root.") || function == "held" ||
		slices.Contains([]string{"os.Open", "os.OpenFile", "os.OpenRoot", "os.Mkdir", "os.Lstat", "os.Stat"}, function) ||
		(strings.HasPrefix(function, "windowsPrivate") && function != "windowsPrivateFailure")
}

// windowsPrivateBeneathInit prints an if statement's init, an assignment in
// the walk ("" when there is none).
func windowsPrivateBeneathInit(init ast.Stmt) string {
	assign, ok := init.(*ast.AssignStmt)
	switch {
	case init == nil:
		return ""
	case !ok:
		return "unprinted"
	}
	return windowsPrivateBeneathList(assign.Lhs) + " " + assign.Tok.String() + " " + windowsPrivateBeneathList(assign.Rhs)
}

func windowsPrivateBeneathList(list []ast.Expr) string {
	texts := make([]string, len(list))
	for index, expression := range list {
		texts[index] = types.ExprString(expression)
	}
	return strings.Join(texts, ", ")
}
