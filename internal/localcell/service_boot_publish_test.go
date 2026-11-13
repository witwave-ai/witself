package localcell

import (
	"bytes"
	"encoding/json"
	"errors"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Boot publication retries only the Windows private sharing or access refusal.
// Every other failure, like every failure off Windows, is one attempt, and a
// successful publication writes exactly once.
func TestServiceBootPublicationRetriesOnlyTransient(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "published_once"
		if fail {
			name = "refusal_single_attempt"
		}
		t.Run(name, func(t *testing.T) {
			c, d := descriptorTestCell(t)
			path := c.path("service", "boot.json")
			attempts := 0
			original := syncFile
			t.Cleanup(func() { syncFile = original })
			syncFile = func(f *os.File) error {
				if filepath.Dir(f.Name()) == c.path("service") && strings.HasPrefix(filepath.Base(f.Name()), ".pending-") {
					attempts++
					// Keep a mutant that retries every error finite.
					if fail || attempts > 16 {
						return errors.New("synthetic boot publication refusal")
					}
				}
				return original(f)
			}
			err := c.reconcileServiceBoot()
			if attempts != 1 {
				t.Fatalf("boot publication attempts=%d", attempts)
			}
			if fail {
				if err == nil || err.Error() != "cannot persist local cell record" {
					t.Fatal("boot publication changed a non-transient refusal")
				}
				if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatal("refused boot publication exposed a boot record")
				}
				return
			}
			if err != nil {
				t.Fatal("boot publication failed without a refusal")
			}
			boot, err := serviceBootID()
			if err != nil {
				t.Fatal("boot identity unavailable")
			}
			want, err := json.MarshalIndent(serviceBootRecord{d.InstallationID, boot}, "", "  ")
			if err != nil {
				t.Fatal("cannot encode expected boot record")
			}
			data, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(data, append(want, '\n')) {
				t.Fatal("boot publication changed the published boot record")
			}
		})
	}
}

const serviceBootPublicationStatements = `return writeAtomic(c.path("service", "boot.json"), serviceBootRecord{d.InstallationID, boot}, nil)`

// The Windows reproduction cannot run on Linux or macOS hosts. Pin the
// publication tail it exercises: one writeAtomic call, whose shared bounded
// retry (TestAtomicPublicationSharedRetrySourceContract) is the only retry. A
// call-site retry, such as the one this tail had before, would nest inside it.
func TestServiceBootPublicationSourceContract(t *testing.T) {
	source := leasePublishReadSource(t, "service_boot.go")
	if !serviceBootPublicationContract(source) {
		t.Fatal("boot publication left the shared bounded publication retry")
	}
	for _, mutation := range []struct{ name, before, after string }{
		{"nested_retry", `return writeAtomic(c.path("service", "boot.json"), serviceBootRecord{d.InstallationID, boot}, nil)`, "path, record := c.path(\"service\", \"boot.json\"), serviceBootRecord{d.InstallationID, boot}\n\t_, err = retryPublish(func() error { return writeAtomic(path, record, nil) }, func(err error) bool { return runtimeconfig.WindowsPrivateTransient(err) }, leasePublishHooks{})\n\treturn err"},
		{"single_attempt", `return writeAtomic(c.path("service", "boot.json")`, `return writeAtomicOnce(c.path("service", "boot.json")`},
		{"pending_target", `c.path("service", "boot.json"), serviceBootRecord`, `c.path("service", "boot.pending"), serviceBootRecord`},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			if strings.Count(source, mutation.before) != 1 {
				t.Fatal("boot publication mutation anchor is not unique")
			}
			if serviceBootPublicationContract(strings.Replace(source, mutation.before, mutation.after, 1)) {
				t.Fatal("boot publication contract accepted a mutation")
			}
		})
	}
}

func serviceBootPublicationContract(source string) bool {
	expected, err := parser.ParseFile(token.NewFileSet(), "expected.go", "package localcell\nfunc tail() {\n"+serviceBootPublicationStatements+"\n}\n", 0)
	if err != nil {
		return false
	}
	want := expected.Decls[0].(*ast.FuncDecl).Body.List
	file, err := parser.ParseFile(token.NewFileSet(), "service_boot.go", strings.ReplaceAll(source, "\r\n", "\n"), 0)
	if err != nil {
		return false
	}
	found := 0
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != "reconcileServiceBoot" || function.Body == nil {
			continue
		}
		found++
		body := function.Body.List
		if len(body) < len(want) || serviceBootPublicationText(&ast.BlockStmt{List: body[len(body)-len(want):]}) != serviceBootPublicationText(&ast.BlockStmt{List: want}) {
			return false
		}
		// Nothing else in the function may retry or bypass the shared retry.
		bypass := false
		ast.Inspect(function.Body, func(node ast.Node) bool {
			if identifier, ok := node.(*ast.Ident); ok {
				switch identifier.Name {
				case "retryPublish", "publishAtomic", "writeAtomicOnce", "writeAtomicBytesOnce":
					bypass = true
				}
			}
			return true
		})
		if bypass {
			return false
		}
	}
	return found == 1
}

func serviceBootPublicationText(node ast.Node) string {
	var output bytes.Buffer
	if format.Node(&output, token.NewFileSet(), node) != nil {
		return ""
	}
	return strings.Join(strings.Fields(output.String()), " ")
}

// The native reproduction must hold the validator's exact guard. Compare its
// CreateFile access, sharing, attributes, disposition, flags and template with
// the one windowsPrivateGuard uses for every component it validates.
func TestWindowsBootValidatorGuardMatchesPrivateGuard(t *testing.T) {
	private := leasePublishReadSource(t, "../runtimeconfig/windows_private.go")
	fixture := leasePublishReadSource(t, "service_boot_publish_windows_test.go")
	if !serviceBootGuardContract(private, fixture) {
		t.Fatal("boot publication reproduction no longer holds the private validator guard")
	}
	for _, mutation := range []struct{ name, file, before, after string }{
		{"fixture_delete_sharing", "fixture", "windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING", "windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING"},
		{"fixture_attribute_only", "fixture", "windows.FILE_READ_ATTRIBUTES|windows.FILE_TRAVERSE,", "windows.FILE_READ_ATTRIBUTES,"},
		{"guard_delete_sharing", "private", "windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING,\n\t\t\twindows.FILE_FLAG_BACKUP_SEMANTICS", "windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING,\n\t\t\twindows.FILE_FLAG_BACKUP_SEMANTICS"},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			sources := map[string]string{"private": private, "fixture": fixture}
			if strings.Count(sources[mutation.file], mutation.before) != 1 {
				t.Fatal("guard contract mutation anchor is not unique")
			}
			sources[mutation.file] = strings.Replace(sources[mutation.file], mutation.before, mutation.after, 1)
			if serviceBootGuardContract(sources["private"], sources["fixture"]) {
				t.Fatal("guard contract accepted a mutation")
			}
		})
	}
}

func serviceBootGuardContract(private, fixture string) bool {
	guard := serviceBootCreateFileArguments(private, "windowsPrivateGuard")
	held := serviceBootCreateFileArguments(fixture, "holdWindowsBootValidatorGuard")
	return guard != "" && guard == held
}

// Every argument after the path of the one windows.CreateFile call in name.
func serviceBootCreateFileArguments(source, name string) string {
	file, err := parser.ParseFile(token.NewFileSet(), "source.go", strings.ReplaceAll(source, "\r\n", "\n"), 0)
	if err != nil {
		return ""
	}
	var calls []*ast.CallExpr
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != name || function.Body == nil {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			if selector, ok := call.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "CreateFile" {
				if receiver, ok := selector.X.(*ast.Ident); ok && receiver.Name == "windows" {
					calls = append(calls, call)
				}
			}
			return true
		})
	}
	if len(calls) != 1 || len(calls[0].Args) != 7 {
		return ""
	}
	var arguments []string
	for _, argument := range calls[0].Args[1:] {
		arguments = append(arguments, serviceBootPublicationText(argument))
	}
	return strings.Join(arguments, ", ")
}
