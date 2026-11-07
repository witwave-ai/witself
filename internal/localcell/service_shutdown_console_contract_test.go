package localcell_test

import (
	"os"
	"strings"
	"testing"
)

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
		{"wrong-error", `!errors.Is(callErr, windows.ERROR_INVALID_HANDLE)`, `!errors.Is(callErr, windows.ERROR_ACCESS_DENIED)`},
		{"ignore-child-failure", `code = exit.ExitCode()`, `code = 0`},
		{"unbounded-probe", `context.WithTimeout(ctx, probeBudget)`, `context.WithCancel(ctx)`},
		{"fixed-probe-budget", `args = append(args, c.cfg.Timeout.String())`, `args = append(args, "20s")`},
		{"outer-expires-first", `waitShutdownMarker(t, probeResult, f.cell.cfg.Timeout+10*f.startup)`, `waitShutdownMarker(t, probeResult, 10*f.startup)`},
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
		strings.Contains(testSource, `result, err := readPrivateFile(probeResult)`) &&
		strings.Contains(testSource, `waitShutdownMarker(t, probeResult, f.cell.cfg.Timeout+10*f.startup)`) &&
		strings.Contains(testSource, "case \"74\":\n\t\tt.Fatal(\"detached AttachConsole probe did not prove ERROR_INVALID_HANDLE\")") &&
		strings.Contains(testSource, "default:\n\t\tt.Fatal(\"no-console probe exited unexpectedly\")")
}
