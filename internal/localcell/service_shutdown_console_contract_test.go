package localcell_test

import (
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

func TestWindowsShutdownDetachedLaunchSourceContract(t *testing.T) {
	source, err := os.ReadFile("launch_windows.go")
	if err != nil {
		t.Fatal("shutdown launch source unavailable")
	}
	if !windowsShutdownDetachedLaunchSourceContract(string(source)) {
		t.Fatal("explicit shutdown launch lost console detachment")
	}
}

func TestWindowsShutdownDetachedLaunchSourceContractMutations(t *testing.T) {
	source, err := os.ReadFile("launch_windows.go")
	if err != nil {
		t.Fatal("shutdown launch source unavailable")
	}
	original := strings.ReplaceAll(string(source), "\r\n", "\n")
	const guard = "if shutdownPath != \"\" {\n\t\tflags |= windows.DETACHED_PROCESS\n\t}"
	const packageConst = "const procThreadAttributeJobList = 0x0002000d"
	// Two-site regressions retain the valid guard and introduce their source at
	// package scope, outside the function scanned by the previous contract.
	for _, row := range []struct{ name, declaration, write string }{
		{"detached-package-const", "const reviewDetachedFlag = windows.DETACHED_PROCESS", "flags |= reviewDetachedFlag"},
		{"detached-helper", "func reviewDetachedFlags(flags uint32) uint32 { return flags | windows.DETACHED_PROCESS }", "flags = reviewDetachedFlags(flags)"},
		{"detached-package-var", "var reviewDetachedFlag = uint32(windows.DETACHED_PROCESS)", "flags |= reviewDetachedFlag"},
		{"detached-package-numeric", "const reviewDetachedFlag = 0x00000008", "flags |= reviewDetachedFlag"},
		{"detached-alias-chain", "const reviewDetachedFlag = windows.DETACHED_PROCESS\nconst reviewDetachedAlias = reviewDetachedFlag", "flags |= reviewDetachedAlias"},
		{"hidden-package-var", "var reviewHiddenFlag = uint32(windows.CREATE_NO_WINDOW)", "flags |= reviewHiddenFlag"},
		{"helper-computed-write", "func reviewFlags(flags uint32) uint32 { return flags | (1 << 3) }", "flags = reviewFlags(flags)"},
		{"helper-pointer-write", "func reviewFlags(flags *uint32) { *flags |= 1 << 3 }", "reviewFlags(&flags)"},
	} {
		t.Run(row.name, func(t *testing.T) {
			if strings.Count(original, packageConst) != 1 || strings.Count(original, guard) != 1 {
				t.Fatal("shutdown launch mutation site changed")
			}
			mutant := strings.Replace(original, packageConst, packageConst+"\n\n"+row.declaration, 1)
			mutant = strings.Replace(mutant, guard, guard+"\n\t"+row.write, 1)
			if windowsShutdownDetachedLaunchSourceContract(mutant) {
				t.Fatal("shutdown launch contract accepted console regression")
			}
		})
	}
	for _, row := range []struct{ name, from, to string }{
		{"hidden-console", `flags |= windows.DETACHED_PROCESS`, `flags |= windows.CREATE_NO_WINDOW`},
		{"inherited-console", `flags |= windows.DETACHED_PROCESS`, `flags |= 0`},
		{"combined-flags", `flags |= windows.DETACHED_PROCESS`, `flags |= windows.DETACHED_PROCESS | windows.CREATE_NO_WINDOW`},
		{"detached-legacy", guard, `flags |= windows.DETACHED_PROCESS`},
		{"detached-after-guard", guard, guard + "\n\tflags = flags | windows.DETACHED_PROCESS"},
		{"detached-base-flags", `flags := uint32(windows.CREATE_SUSPENDED |`, `flags := uint32(windows.DETACHED_PROCESS | windows.CREATE_SUSPENDED |`},
		{"detached-postgres", "if component == \"postgres\" {", "if component == \"postgres\" {\n\t\tflags |= windows.DETACHED_PROCESS"},
		{"detached-intermediate", guard, guard + "\n\tdetached := uint32(windows.DETACHED_PROCESS)\n\tflags |= detached"},
		{"detached-indirect-guard", guard, "detached := uint32(windows.DETACHED_PROCESS)\n\tif shutdownPath != \"\" {\n\t\tflags |= detached\n\t}"},
		{"detached-numeric", guard, guard + "\n\tflags |= 0x00000008"},
		{"computed-write", guard, guard + "\n\tflags |= 1 << 3"},
		{"plain-write", guard, guard + "\n\tflags = 0"},
		{"compound-write", guard, guard + "\n\tflags += 1"},
		{"increment-write", guard, guard + "\n\tflags++"},
		{"pointer-write", guard, guard + "\n\tp := &flags\n\t*p |= 1 << 3"},
		{"closure-write", guard, guard + "\n\tfunc() { flags |= 1 << 3 }()"},
		{"postgres-shadow-write", "if component == \"postgres\" {", "if component == \"postgres\" {\n\t\tflags := flags | (1 << 3)"},
		{"other-function-selector", packageConst, packageConst + "\n\nfunc reviewUnused() uint32 { return windows.DETACHED_PROCESS }"},
		{"other-function-literal", packageConst, packageConst + "\n\nfunc reviewUnused() uint32 { return 0x08000000 }"},
		{"package-decimal-literal", packageConst, packageConst + "\n\nconst reviewUnused = 8"},
		{"package-octal-literal", packageConst, packageConst + "\n\nconst reviewUnused = 0o10"},
		{"package-hex-literal", packageConst, packageConst + "\n\nconst reviewUnused = 0x0800_0000"},
		{"package-float-literal", packageConst, packageConst + "\n\nconst reviewUnused = 8.0"},
	} {
		t.Run(row.name, func(t *testing.T) {
			if strings.Count(original, row.from) != 1 {
				t.Fatal("shutdown launch mutation site changed")
			}
			if windowsShutdownDetachedLaunchSourceContract(strings.Replace(original, row.from, row.to, 1)) {
				t.Fatal("shutdown launch contract accepted console regression")
			}
		})
	}
	if !windowsShutdownDetachedLaunchSourceContract(strings.ReplaceAll(original, "\n", "\r\n")) {
		t.Fatal("shutdown launch contract rejected CRLF source")
	}
}

func windowsShutdownDetachedLaunchSourceContract(source string) bool {
	source = strings.ReplaceAll(source, "\r\n", "\n")
	file, err := parser.ParseFile(token.NewFileSet(), "launch_windows.go", source, 0)
	if err != nil {
		return false
	}
	var launch *ast.FuncDecl
	for _, declaration := range file.Decls {
		if function, ok := declaration.(*ast.FuncDecl); ok && function.Name.Name == "launchWindows" {
			if launch != nil || function.Body == nil {
				return false
			}
			launch = function
		}
	}
	if launch == nil {
		return false
	}
	// Locate the sole permitted use by AST identity, directly under the shutdown
	// guard and immediately after the unchanged base flags. An alias or an
	// intermediate value must not supply detachment from outside this assignment.
	var guardedDetachment *ast.SelectorExpr
	var baseFlags *ast.Ident
	permittedFlags := make(map[*ast.Ident]bool)
	for index, statement := range launch.Body.List {
		branch, ok := statement.(*ast.IfStmt)
		if !ok || index == 0 || branch.Init != nil || branch.Else != nil || len(branch.Body.List) != 1 ||
			lifecycleProofExpression(branch.Cond) != `shutdownPath != ""` ||
			lifecycleProofExpression(launch.Body.List[index-1]) != `flags := uint32(windows.CREATE_SUSPENDED | windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_UNICODE_ENVIRONMENT | windows.EXTENDED_STARTUPINFO_PRESENT)` {
			continue
		}
		assignment, ok := branch.Body.List[0].(*ast.AssignStmt)
		if !ok || assignment.Tok != token.OR_ASSIGN || len(assignment.Lhs) != 1 || len(assignment.Rhs) != 1 ||
			lifecycleProofExpression(assignment.Lhs[0]) != "flags" ||
			!serviceControlChildSelector(assignment.Rhs[0], "windows", "DETACHED_PROCESS") {
			continue
		}
		if guardedDetachment != nil {
			return false
		}
		guardedDetachment = assignment.Rhs[0].(*ast.SelectorExpr)
		baseFlags = launch.Body.List[index-1].(*ast.AssignStmt).Lhs[0].(*ast.Ident)
		permittedFlags[baseFlags] = true
		permittedFlags[assignment.Lhs[0].(*ast.Ident)] = true
	}
	if guardedDetachment == nil {
		return false
	}
	// Inspect the SAME whole-file tree, including package declarations, helper
	// functions and literals. In-file aliases (including alias chains) cannot
	// hide either constant: their source declaration is itself inspected here.
	// Comments and string contents are not selectors or numeric values.
	detachmentUses := 0
	invalidConsole := false
	ast.Inspect(file, func(node ast.Node) bool {
		if selector, ok := node.(*ast.SelectorExpr); ok {
			switch selector.Sel.Name {
			case "CREATE_NO_WINDOW":
				invalidConsole = true
			case "DETACHED_PROCESS":
				detachmentUses++
				if selector != guardedDetachment {
					invalidConsole = true
				}
			}
		}
		if literal, ok := node.(*ast.BasicLit); ok {
			value := constant.MakeFromLiteral(literal.Value, literal.Kind, 0)
			if (value.Kind() == constant.Int || value.Kind() == constant.Float) &&
				(constant.Compare(value, token.EQL, constant.MakeInt64(0x00000008)) ||
					constant.Compare(value, token.EQL, constant.MakeInt64(0x08000000))) {
				invalidConsole = true
			}
		}
		return true
	})
	if invalidConsole || detachmentUses != 1 {
		return false
	}
	// The current launcher has exactly two writes and two reads of flags. Pin
	// the creation arguments, then reject every other identifier occurrence in
	// the entire function, even after creation or inside a closure. This covers
	// assignment/compound assignment, increments, shadowing, and &flags escapes
	// without trying to infer the effects of helper calls or pointer aliases.
	creationCalls := make(map[string]int)
	ast.Inspect(launch, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		for name, index := range map[string]int{"CreateProcess": 5, "CreateProcessAsUser": 6} {
			if !serviceControlChildSelector(call.Fun, "windows", name) {
				continue
			}
			creationCalls[name]++
			if len(call.Args) > index {
				if flags, ok := call.Args[index].(*ast.Ident); ok && flags.Name == "flags" && flags.Pos() > guardedDetachment.End() {
					permittedFlags[flags] = true
				}
			}
		}
		return true
	})
	validFlags := creationCalls["CreateProcess"] == 1 && creationCalls["CreateProcessAsUser"] == 1 && len(permittedFlags) == 4 && baseFlags.Obj != nil
	ast.Inspect(launch, func(node ast.Node) bool {
		if identifier, ok := node.(*ast.Ident); ok && identifier.Name == "flags" {
			if !permittedFlags[identifier] || identifier.Obj != baseFlags.Obj {
				validFlags = false
			}
		}
		return true
	})
	return validFlags
}

func TestWindowsNoConsoleFixtureSourceContract(t *testing.T) {
	source, err := os.ReadFile("service_shutdown_windows_test.go")
	if err != nil {
		t.Fatal("no-console fixture source unavailable")
	}
	if !windowsNoConsoleFixtureSourceContract(string(source)) {
		t.Fatal("no-console fixture lost exact console absence proof")
	}
}

func TestWindowsNoConsoleFixtureSourceContractMutations(t *testing.T) {
	source, err := os.ReadFile("service_shutdown_windows_test.go")
	if err != nil {
		t.Fatal("no-console fixture source unavailable")
	}
	original := strings.ReplaceAll(string(source), "\r\n", "\n")
	for _, row := range []struct{ name, from, to string }{
		{"window-only", windowsNoConsoleFixtureProof, "getConsole := windows.NewLazySystemDLL(\"kernel32.dll\").NewProc(\"GetConsoleWindow\")\n\twindow, _, _ := getConsole.Call()\n\tif window != 0 {\n\t\tos.Exit(77)\n\t}"},
		{"ignore-attached-count", `count != 0 || !errors.Is(consoleErr, windows.ERROR_INVALID_HANDLE)`, `!errors.Is(consoleErr, windows.ERROR_INVALID_HANDLE)`},
		{"ignore-error", `count != 0 || !errors.Is(consoleErr, windows.ERROR_INVALID_HANDLE)`, `count != 0`},
		{"wrong-error", `!errors.Is(consoleErr, windows.ERROR_INVALID_HANDLE)`, `!errors.Is(consoleErr, windows.ERROR_ACCESS_DENIED)`},
	} {
		t.Run(row.name, func(t *testing.T) {
			if strings.Count(original, row.from) != 1 {
				t.Fatal("no-console fixture mutation site changed")
			}
			if windowsNoConsoleFixtureSourceContract(strings.Replace(original, row.from, row.to, 1)) {
				t.Fatal("no-console fixture contract accepted a broken oracle")
			}
		})
	}
	if !windowsNoConsoleFixtureSourceContract(strings.ReplaceAll(original, "\n", "\r\n")) {
		t.Fatal("no-console fixture contract rejected CRLF source")
	}
}

const windowsNoConsoleFixtureProof = `var consoleProcess uint32
	getConsoleProcesses := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleProcessList")
	count, _, consoleErr := getConsoleProcesses.Call(uintptr(unsafe.Pointer(&consoleProcess)), 1)
	if count != 0 || !errors.Is(consoleErr, windows.ERROR_INVALID_HANDLE) {
		os.Exit(77)
	}`

func windowsNoConsoleFixtureSourceContract(source string) bool {
	source = strings.ReplaceAll(source, "\r\n", "\n")
	fset, helper, err := nativeServiceProofFunction([]byte(source), "service_shutdown_windows_test.go", "TestWindowsShutdownChildHelper")
	if err != nil {
		return false
	}
	helperSource := source[fset.Position(helper.Pos()).Offset:fset.Position(helper.End()).Offset]
	// Require the proof immediately before publication so a window check alone
	// cannot authorize the marker. Neither query depends on redirected stdio.
	return strings.Contains(helperSource, windowsNoConsoleFixtureProof+"\n\tif writeAtomicBytes(filepath.Join(root, \"tmp\", \"console-absent\"), []byte(\"absent\"), nil) != nil {")
}

// Linux cannot execute AttachConsole. This contract guards the native oracle's
// process topology and exact result checks; the Windows row proves API behavior.
func TestWindowsNoConsoleProbeSourceContract(t *testing.T) {
	source, err := os.ReadFile("service_shutdown_windows_test.go")
	if err != nil {
		t.Fatal("no-console probe source unavailable")
	}
	if !windowsNoConsoleProbeSourceContract(string(source)) {
		t.Fatal("no-console probe lost detached parent or exact error proof")
	}
}

func TestWindowsNoConsoleProbeSourceContractMutations(t *testing.T) {
	source, err := os.ReadFile("service_shutdown_windows_test.go")
	if err != nil {
		t.Fatal("no-console probe source unavailable")
	}
	original := strings.ReplaceAll(string(source), "\r\n", "\n")
	for _, row := range []struct{ name, from, to string }{
		{"runner-parent", `f := launchShutdownFixture(t, "normal-probe", false)`, `f := launchShutdownFixture(t, "normal", false)`},
		{"explicit-process", `attachConsoleProc.Call(uintptr(attachParentProcess))`, `attachConsoleProc.Call(uintptr(os.Getpid()))`},
		{"inherited-console", `probe.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.DETACHED_PROCESS}`, `probe.SysProcAttr = &syscall.SysProcAttr{}`},
		{"attached-probe", "if window, _, _ := getConsole.Call(); window != 0 {\n\t\t\tos.Exit(71)\n\t\t}", ""},
		{"successful-attach", "if result != 0 {\n\t\t\t_, _, _ = freeConsoleProc.Call()\n\t\t\tos.Exit(73)\n\t\t}", ""},
		{"accept-success-category", "case \"73\":\n\t\tt.Fatal(\"detached probe unexpectedly attached to parent console\")", "case \"73\":"},
		{"accept-wrong-error-category", "case \"74\":\n\t\tt.Fatal(\"detached AttachConsole probe did not prove ERROR_INVALID_HANDLE\")", "case \"74\":"},
		{"wrong-error", `!errors.Is(callErr, windows.ERROR_INVALID_HANDLE)`, `!errors.Is(callErr, windows.ERROR_ACCESS_DENIED)`},
		{"ignore-child-failure", `code = exit.ExitCode()`, `code = 0`},
		{"unbounded-probe", `context.WithTimeout(ctx, probeBudget)`, `context.WithCancel(ctx)`},
		{"fixed-probe-budget", `args = append(args, c.cfg.Timeout.String())`, `args = append(args, "20s")`},
		{"second-unprotected-read", `switch string(result) {`, "result, err = readPrivateFile(probeResult)\n\tswitch string(result) {"},
		{"one-shot-result", `result, observation, err := readShutdownProbeResult(probeCtx,`, `result, observation, err := oneShotProbeResult(probeCtx,`},
		{"unvalidated-result", `func() ([]byte, error) { return readPrivateFile(probeResult) }`, `func() ([]byte, error) { return os.ReadFile(probeResult) }`},
		{"unclassified-retry", `windowsShutdownProbeReadClass, func(ctx context.Context) error {`, `retryEveryError, func(ctx context.Context) error {`},
		{"outer-expires-first", `context.WithTimeout(t.Context(), f.cell.cfg.Timeout+10*f.startup)`, `context.WithTimeout(t.Context(), 10*f.startup)`},
	} {
		t.Run(row.name, func(t *testing.T) {
			if strings.Count(original, row.from) != 1 {
				t.Fatal("no-console probe mutation site changed")
			}
			mutant := strings.Replace(original, row.from, row.to, 1)
			if windowsNoConsoleProbeSourceContract(mutant) {
				t.Fatal("no-console probe contract accepted a broken oracle")
			}
		})
	}
	if !windowsNoConsoleProbeSourceContract(strings.ReplaceAll(original, "\n", "\r\n")) {
		t.Fatal("no-console probe contract rejected CRLF source")
	}
}

func windowsNoConsoleProbeSourceContract(source string) bool {
	source = strings.ReplaceAll(source, "\r\n", "\n")
	fset, helper, err := nativeServiceProofFunction([]byte(source), "service_shutdown_windows_test.go", "TestWindowsShutdownChildHelper")
	if err != nil {
		return false
	}
	helperSource := source[fset.Position(helper.Pos()).Offset:fset.Position(helper.End()).Offset]
	for _, required := range []string{
		`if args[0] == "probe" {`,
		`const attachParentProcess = ^uint32(0)`,
		`result, _, callErr := attachConsoleProc.Call(uintptr(attachParentProcess))`,
		"if window, _, _ := getConsole.Call(); window != 0 {\n\t\t\tos.Exit(71)\n\t\t}",
		"if result != 0 {\n\t\t\t_, _, _ = freeConsoleProc.Call()\n\t\t\tos.Exit(73)\n\t\t}",
		"if !errors.Is(callErr, windows.ERROR_INVALID_HANDLE) {\n\t\t\tos.Exit(74)\n\t\t}",
		`if mode == "normal-probe" {`,
		`probeBudget, err := time.ParseDuration(args[1])`,
		`probeCtx, probeCancel := context.WithTimeout(ctx, probeBudget)`,
		`exec.CommandContext(probeCtx, os.Args[0], "-test.run=^TestWindowsShutdownChildHelper$", "--", "probe")`,
		`probe.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.DETACHED_PROCESS}`,
		`code = exit.ExitCode()`,
		`writeAtomicBytes(filepath.Join(root, "tmp", "console-probe-result"), []byte(strconv.Itoa(code)), nil)`,
	} {
		if !strings.Contains(helperSource, required) {
			return false
		}
	}
	fset, launch, err := nativeServiceProofFunction([]byte(source), "service_shutdown_windows_test.go", "launchShutdownFixture")
	if err != nil {
		return false
	}
	launchSource := source[fset.Position(launch.Pos()).Offset:fset.Position(launch.End()).Offset]
	if !strings.Contains(launchSource, `args = append(args, c.cfg.Timeout.String())`) ||
		!strings.Contains(launchSource, `err = c.launchWindows(os.Args[0], args, "server", environment, log, nil)`) {
		return false
	}
	fset, test, err := nativeServiceProofFunction([]byte(source), "service_shutdown_windows_test.go", "TestWindowsNoConsoleShutdown")
	if err != nil {
		return false
	}
	testSource := source[fset.Position(test.Pos()).Offset:fset.Position(test.End()).Offset]
	return strings.Contains(testSource, `f := launchShutdownFixture(t, "normal-probe", false)`) &&
		strings.Contains(testSource, `result, observation, err := readShutdownProbeResult(probeCtx,`) &&
		strings.Contains(testSource, `func() ([]byte, error) { return readPrivateFile(probeResult) }`) &&
		strings.Count(testSource, `readPrivateFile(probeResult)`) == 1 &&
		strings.Contains(testSource, `windowsShutdownProbeReadClass, func(ctx context.Context) error {`) &&
		strings.Contains(testSource, `probeCtx, probeCancel := context.WithTimeout(t.Context(), f.cell.cfg.Timeout+10*f.startup)`) &&
		!strings.Contains(testSource, `waitShutdownMarker(t, probeResult,`) &&
		strings.Contains(testSource, "case \"73\":\n\t\tt.Fatal(\"detached probe unexpectedly attached to parent console\")") &&
		strings.Contains(testSource, "case \"74\":\n\t\tt.Fatal(\"detached AttachConsole probe did not prove ERROR_INVALID_HANDLE\")") &&
		strings.Contains(testSource, "default:\n\t\tt.Fatal(\"no-console probe exited unexpectedly\")")
}
