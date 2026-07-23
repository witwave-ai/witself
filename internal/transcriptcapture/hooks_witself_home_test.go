package transcriptcapture

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallHooksPinsWitselfHomeInPOSIXAndWindowsCommands(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	executable := `C:\Program Files\Witself\witself.exe`
	witselfHome := `C:\Users\Scott\Witself State`
	path, err := installHooksForPlatformWithWitselfHome(
		"windows", RuntimeCodex, ModeRaw, executable,
		"default", "default", "scott", "home", witselfHome,
	)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}
	hooks := root["hooks"].(map[string]any)
	foundPOSIX := false
	foundWindows := false
	for _, groupsRaw := range hooks {
		groups, _ := groupsRaw.([]any)
		for _, groupRaw := range groups {
			group, _ := groupRaw.(map[string]any)
			handlers, _ := group["hooks"].([]any)
			for _, handlerRaw := range handlers {
				handler, _ := handlerRaw.(map[string]any)
				command, _ := handler["command"].(string)
				if !strings.Contains(command, hookCommandMarker) {
					continue
				}
				if !strings.Contains(command, "--witself-home '"+witselfHome+"'") {
					t.Fatalf("POSIX hook command does not pin WITSELF_HOME: %s", command)
				}
				foundPOSIX = true
				windowsCommand, _ := handler["commandWindows"].(string)
				script := decodePowerShellEncodedCommand(t, windowsCommand)
				if !strings.Contains(script, "'--witself-home' '"+witselfHome+"'") {
					t.Fatalf("Windows hook script does not pin WITSELF_HOME: %s", script)
				}
				foundWindows = true
				break
			}
		}
	}
	if !foundPOSIX || !foundWindows {
		t.Fatalf("Witself hook commands found: posix=%t windows=%t", foundPOSIX, foundWindows)
	}
}

func TestManagedHooksPinWitselfHome(t *testing.T) {
	for _, runtimeName := range []string{RuntimeCodex, RuntimeClaudeCode} {
		t.Run(runtimeName, func(t *testing.T) {
			opts := managedHooksTestOptions(t, runtimeName, ModeRaw)
			opts.WitselfHome = filepath.Join(filepath.Dir(opts.PolicyPath()), "witself-state")
			path, err := InstallManagedHooks(opts)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(raw), "--witself-home") || !strings.Contains(string(raw), opts.WitselfHome) {
				t.Fatalf("managed hook does not pin WITSELF_HOME:\n%s", raw)
			}
		})
	}
}
