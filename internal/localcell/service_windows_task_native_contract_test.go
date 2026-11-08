package localcell

import (
	"bytes"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// These source contracts make Windows-only safety premises reviewable on every
// build host. They do not substitute for execution on the native Windows row.
func windowsTaskContractFunction(source, name string, method bool) *ast.FuncDecl {
	source = strings.ReplaceAll(source, "\r\n", "\n")
	file, err := parser.ParseFile(token.NewFileSet(), "contract.go", source, 0)
	if err != nil {
		return nil
	}
	var found *ast.FuncDecl
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if ok && function.Name.Name == name && (function.Recv != nil) == method {
			if found != nil || function.Body == nil {
				return nil
			}
			found = function
		}
	}
	return found
}

func windowsTaskContractText(node ast.Node) string {
	var text bytes.Buffer
	if node == nil || format.Node(&text, token.NewFileSet(), node) != nil {
		return ""
	}
	return text.String()
}

func windowsTaskContractOrdered(function *ast.FuncDecl, required ...string) bool {
	if function == nil {
		return false
	}
	next := 0
	for _, statement := range function.Body.List {
		if next < len(required) && windowsTaskContractText(statement) == required[next] {
			next++
		}
	}
	return next == len(required)
}

func windowsTaskNativeCommandContract(source string) bool {
	system := windowsTaskContractFunction(source, "windowsTaskSystemCommand", false)
	if !windowsTaskContractOrdered(system,
		`directory, err := windows.GetSystemDirectory()`,
		`root, rootErr := windows.GetWindowsDirectory()`,
		`path := filepath.Join(directory, "schtasks.exe")`,
		`info, err := os.Lstat(path)`, `return path, root, nil`) {
		return false
	}
	runner := windowsTaskContractFunction(source, "runWindowsTaskCommand", false)
	if !windowsTaskContractOrdered(runner,
		`var result windowsTaskCommandResult`,
		`path, root, err := windowsTaskSystemCommand()`,
		`bounded, cancel := context.WithTimeout(ctx, 15*time.Second)`,
		`command := exec.CommandContext(bounded, path, args...)`,
		`command.WaitDelay = time.Second`,
		`command.Env = []string{"SystemRoot=" + root, "WINDIR=" + root}`,
		`command.Dir = filepath.Dir(path)`, `command.Stdin = nil`,
		`capture := &windowsTaskCapture{}`,
		`command.Stdout, command.Stderr = capture.stdout(), capture.stderr()`,
		`err = command.Run()`,
		"if capture.exceeded() {\n\treturn result, windowsTaskError(ErrServiceManagerUnavailable, \"query:result\", 0, \"output_limit\")\n}",
		`result.Output = capture.output()`, `return result, nil`) {
		return false
	}
	// No later reassignment or alias can relax the environment, stdin or pipe
	// bound. The only result typedness allowed in this unproven native transport
	// is the zero value; even an explicit Typed:false assignment is disallowed.
	protected := map[string]int{"Env": 0, "Stdin": 0, "WaitDelay": 0, "Stdout": 0, "Stderr": 0}
	valid := true
	ast.Inspect(runner.Body, func(node ast.Node) bool {
		if selector, ok := node.(*ast.SelectorExpr); ok {
			object, identifier := selector.X.(*ast.Ident)
			if selector.Sel.Name == "Typed" {
				valid = false
			}
			if identifier && object.Name == "command" {
				if _, tracked := protected[selector.Sel.Name]; tracked {
					protected[selector.Sel.Name]++
				}
			}
			if identifier && (object.Name == "os" && (selector.Sel.Name == "Environ" || selector.Sel.Name == "Getenv" || selector.Sel.Name == "LookupEnv") || object.Name == "exec" && selector.Sel.Name == "LookPath") {
				valid = false
			}
		}
		if unary, ok := node.(*ast.UnaryExpr); ok && unary.Op == token.AND && windowsTaskContractText(unary.X) == "command" {
			valid = false
		}
		if key, ok := node.(*ast.KeyValueExpr); ok && windowsTaskContractText(key.Key) == "Typed" {
			valid = false
		}
		return true
	})
	for _, count := range protected {
		valid = valid && count == 1
	}
	capability := windowsTaskContractFunction(source, "WindowsLogonTaskCapability", false)
	return valid && windowsTaskCapabilityCallsContract(capability) && windowsTaskContractOrdered(capability,
		"if err := validateTaskHostOwner(uid); err != nil {\n\treturn err\n}",
		"if err := windowsTaskInteractiveSession(); err != nil {\n\treturn err\n}",
		`return windowsTaskError(ErrServiceManagerUnavailable, "query:result", 0, "query_result_untyped")`)
}

func windowsTaskCapabilityCallsContract(function *ast.FuncDecl) bool {
	if function == nil {
		return false
	}
	// Capability may only read local identity/session and resolve the trusted
	// executable. In particular, no additional manager command may hide before,
	// after, or inside one of the existing ordered refusal checks.
	want := map[string]int{
		"ctx.Err": 1, "serviceOwner": 1, "validateTaskHostOwner": 1,
		"windowsTaskInteractiveSession": 1, "windowsTaskSystemCommand": 1,
		"windowsTaskError": 2,
	}
	valid := true
	ast.Inspect(function.Body, func(node ast.Node) bool {
		if call, ok := node.(*ast.CallExpr); ok {
			name := windowsTaskContractText(call.Fun)
			if want[name] <= 0 {
				valid = false
			} else {
				want[name]--
			}
		}
		return true
	})
	for _, count := range want {
		valid = valid && count == 0
	}
	return valid
}

func windowsTaskRetainedIdentityContract(sources map[string]string) bool {
	descriptor := windowsTaskContractFunction(sources["service_descriptor.go"], "validateServiceHost", false)
	if !windowsTaskContractOrdered(descriptor, `return validateServiceHostIdentity(expected)`) {
		return false
	}
	native := windowsTaskContractFunction(sources["service_windows_task_native_windows.go"], "validateServiceHostIdentity", false)
	if !windowsTaskContractOrdered(native,
		`handle, err := windowsOpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(os.Getpid()))`,
		"if windowsValidateProcessExecutable(handle, expected) != nil {\n\treturn ErrServiceOwnershipConflict\n}") {
		return false
	}
	exit := windowsTaskContractFunction(sources["service_control_exit_windows.go"], "validateHost", true)
	if !windowsTaskContractOrdered(exit, `sid, err := windowsProcessTokenUser(o.handle)`,
		"if err != nil || sid == nil || sid.String() != d.UID || windowsValidateProcessExecutable(o.handle, d.Payload.Host) != nil {\n\treturn hostExitRefused(\"envelope\")\n}") {
		return false
	}
	retain := windowsTaskContractFunction(sources["service_control_exit_windows.go"], "retainServiceExitHost", false)
	if !windowsTaskContractOrdered(retain, `handle, alive, err := openVerifiedWindowsProcess(ProcessRecord{PID: lease.PID, StartTime: lease.StartIdentity}, 0)`) {
		return false
	}
	wrapper := windowsTaskContractFunction(sources["service_control_exit.go"], "retainServiceExitHost", true)
	if wrapper == nil {
		return false
	}
	verified := false
	ast.Inspect(wrapper.Body, func(node ast.Node) bool {
		branch, ok := node.(*ast.IfStmt)
		if ok && windowsTaskContractText(branch.Cond) == `err != nil || verifier.validateHost(d) != nil` && len(branch.Body.List) == 2 &&
			windowsTaskContractText(branch.Body.List[0]) == `observer.close()` && windowsTaskContractText(branch.Body.List[1]) == `return nil, hostExitRefused("envelope")` {
			verified = true
		}
		return true
	})
	if !verified {
		return false
	}
	for _, name := range []string{"controlExitHost", "ExitServiceHost", "exitServiceHostForRemoval"} {
		entry := windowsTaskContractFunction(sources["service_control_exit.go"], name, true)
		if !windowsTaskContractOrdered(entry, `observer, err := c.retainServiceExitHost(lease)`) {
			return false
		}
	}
	image := windowsTaskContractFunction(sources["process_windows.go"], "windowsValidateProcessExecutable", false)
	if image == nil {
		return false
	}
	text := windowsTaskContractText(image.Body)
	for _, required := range []string{
		`windows.QueryFullProcessImageName(handle, 0, &buffer[0], &size)`,
		`windows.CreateFile(name, windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)`,
		`windows.GetFileInformationByHandle(file, &info)`,
		`info.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0`,
		`gotInfo.VolumeSerialNumber != wantInfo.VolumeSerialNumber || gotInfo.FileIndexHigh != wantInfo.FileIndexHigh || gotInfo.FileIndexLow != wantInfo.FileIndexLow`,
	} {
		if !strings.Contains(text, required) {
			return false
		}
	}
	return true
}

func windowsTaskNativeContractSources(t *testing.T) map[string]string {
	t.Helper()
	sources := map[string]string{}
	for _, name := range []string{"service_windows_task_native_windows.go", "service_descriptor.go", "service_control_exit_windows.go", "service_control_exit.go", "process_windows.go"} {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal("cannot read Windows task native source contract")
		}
		sources[name] = strings.ReplaceAll(string(data), "\r\n", "\n")
	}
	return sources
}

func TestWindowsTaskNativeCommandSourceContract(t *testing.T) {
	sources := windowsTaskNativeContractSources(t)
	if !windowsTaskNativeCommandContract(sources["service_windows_task_native_windows.go"]) {
		t.Fatal("Windows task command lost trusted path bounded IO or untyped refusal")
	}
}

func TestWindowsTaskRetainedIdentitySourceContract(t *testing.T) {
	if !windowsTaskRetainedIdentityContract(windowsTaskNativeContractSources(t)) {
		t.Fatal("Windows task host lost retained owner creation or executable identity")
	}
}

func TestWindowsTaskNativeSourceContractsRejectMutations(t *testing.T) {
	for _, row := range []struct{ name, file, from, to string }{
		{"trusted-path", "service_windows_task_native_windows.go", `directory, err := windows.GetSystemDirectory()`, `directory, err := os.UserHomeDir()`},
		{"inherited-env", "service_windows_task_native_windows.go", `command.Env = []string{"SystemRoot=" + root, "WINDIR=" + root}`, `command.Env = os.Environ()`},
		{"stdin", "service_windows_task_native_windows.go", `command.Stdin = nil`, `command.Stdin = os.Stdin`},
		{"pipe-wait", "service_windows_task_native_windows.go", `command.WaitDelay = time.Second`, `command.WaitDelay = 0`},
		{"typed-without-proof", "service_windows_task_native_windows.go", `result.Output = capture.output()`, "result.Output = capture.output()\nresult.Typed = true"},
		{"capability", "service_windows_task_native_windows.go", `return windowsTaskError(ErrServiceManagerUnavailable, "query:result", 0, "query_result_untyped")`, `return nil`},
		{"capability-mutation", "service_windows_task_native_windows.go", `func WindowsLogonTaskCapability(ctx context.Context) error {`, "func WindowsLogonTaskCapability(ctx context.Context) error {\n_, _ = runWindowsTaskCommand(ctx, \"/Create\")"},
		{"owner", "service_control_exit_windows.go", `sid.String() != d.UID || `, ``},
		{"creation", "service_control_exit_windows.go", `StartTime: lease.StartIdentity`, `StartTime: "unfenced"`},
		{"executable", "service_control_exit_windows.go", `windowsValidateProcessExecutable(o.handle, d.Payload.Host) != nil`, `false`},
		{"exit-wrapper", "service_control_exit.go", `verifier.validateHost(d) != nil`, `false`},
		{"file-identity", "process_windows.go", `gotInfo.FileIndexLow != wantInfo.FileIndexLow`, `false`},
		{"host-entry", "service_descriptor.go", `return validateServiceHostIdentity(expected)`, `return nil`},
	} {
		t.Run(row.name, func(t *testing.T) {
			sources := windowsTaskNativeContractSources(t)
			if strings.Count(sources[row.file], row.from) != 1 {
				t.Fatal("Windows task native mutation raising site changed")
			}
			sources[row.file] = strings.Replace(sources[row.file], row.from, row.to, 1)
			if windowsTaskNativeCommandContract(sources["service_windows_task_native_windows.go"]) && windowsTaskRetainedIdentityContract(sources) {
				t.Fatal("Windows task native contract accepted a safety mutation")
			}
		})
	}
	sources := windowsTaskNativeContractSources(t)
	for name, source := range sources {
		sources[name] = strings.ReplaceAll(source, "\n", "\r\n")
	}
	if !windowsTaskNativeCommandContract(sources["service_windows_task_native_windows.go"]) || !windowsTaskRetainedIdentityContract(sources) {
		t.Fatal("Windows task native contracts rejected CRLF sources")
	}
}
