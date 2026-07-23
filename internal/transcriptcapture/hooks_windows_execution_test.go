//go:build windows

package transcriptcapture

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"syscall"
	"testing"
)

func TestCodexHooksWindowsCommandExecutesLiteralArguments(t *testing.T) {
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate Windows hook execution test")
	}
	fixtureDirectory := filepath.Join(filepath.Dir(testFile), "testdata", "windows_hook_argv")
	root := filepath.Join(t.TempDir(), "Program Files O'Brien %PATH% & caret^")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(root, "fake witself O'Brien % & ^.exe")
	build := exec.Command("go", "build", "-o", executable, fixtureDirectory)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build Windows argv fixture: %v\n%s", err, output)
	}

	// A pipe is not legal in a Windows path, so the executable path exercises
	// every legal requested metacharacter and each binding also exercises |.
	account := "account space O'Brien %PATH% &value ^caret |pipe"
	realm := "realm 'quoted' %REALM% & ^ |"
	agent := "agent space 'apostrophe' %AGENT% & ^ |"
	location := "home location O'Brien %HOME% & ^ |"
	commandWindows, err := codexWindowsHookCommand(
		executable,
		RuntimeCodex,
		account,
		realm,
		agent,
		location,
	)
	if err != nil {
		t.Fatal(err)
	}
	outputPath := filepath.Join(root, "received argv O'Brien % & ^.json")
	shadowDirectory := filepath.Join(root, "untrusted checkout")
	if err := os.MkdirAll(shadowDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	// Codex hooks execute in the active project. A project-local PowerShell
	// decoy must never be selected ahead of the kernel-reported system binary.
	if err := os.WriteFile(filepath.Join(shadowDirectory, "powershell.exe"), []byte("untrusted decoy"), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("cmd.exe")
	// os/exec uses CommandLineToArgvW quoting for ordinary Args, while cmd.exe
	// has its own incompatible parser. Supply the exact command-processor line
	// and standard outer quotes so this exercises the stored hook command rather
	// than Go's application-argument escaping.
	command.SysProcAttr = &syscall.SysProcAttr{
		CmdLine: `/d /s /c "` + commandWindows + `"`,
	}
	command.Dir = shadowDirectory
	command.Env = append(
		os.Environ(),
		"PATH="+shadowDirectory+string(os.PathListSeparator)+os.Getenv("PATH"),
		"WITSELF_TEST_WINDOWS_HOOK_ARGV_OUTPUT="+outputPath,
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("execute commandWindows through cmd.exe: %v\n%s\ncommand: %s", err, output, commandWindows)
	}
	raw, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	var received []string
	if err := json.Unmarshal(raw, &received); err != nil {
		t.Fatalf("decode fixture argv: %v\n%s", err, raw)
	}
	want := []string{
		"transcript", "hook",
		"--runtime", RuntimeCodex,
		"--account", account,
		"--realm", realm,
		"--agent", agent,
		"--location", location,
	}
	if !reflect.DeepEqual(received, want) {
		t.Fatalf("literal Windows hook argv:\n got: %#v\nwant: %#v", received, want)
	}
}
