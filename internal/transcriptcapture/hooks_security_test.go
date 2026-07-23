package transcriptcapture

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
)

func TestHookMutationsRejectSymlinkAndNonRegularTargets(t *testing.T) {
	for _, runtimeName := range []string{RuntimeClaudeCode, RuntimeGrokBuild} {
		t.Run(runtimeName+"/symlink", func(t *testing.T) {
			path := configureHookSecurityTestRuntime(t, runtimeName)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			outside := filepath.Join(t.TempDir(), "outside.json")
			want := []byte(`{"foreign":"preserve"}`)
			if err := os.WriteFile(outside, want, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, path); err != nil {
				if goruntime.GOOS == "windows" {
					t.Skipf("symlinks unavailable on this Windows runner: %v", err)
				}
				t.Fatal(err)
			}

			if _, err := InstallHooks(runtimeName, ModeRaw, "/usr/local/bin/witself", "default", "default", "scott", "home"); err == nil ||
				!strings.Contains(err.Error(), "real regular file") {
				t.Fatalf("install error = %v", err)
			}
			if _, err := RemoveHooks(runtimeName); err == nil || !strings.Contains(err.Error(), "real regular file") {
				t.Fatalf("remove error = %v", err)
			}
			if _, err := HooksInstalled(runtimeName); err == nil || !strings.Contains(err.Error(), "real regular file") {
				t.Fatalf("inspection error = %v", err)
			}
			got, err := os.ReadFile(outside)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("symlink target changed: got %q want %q", got, want)
			}
		})

		t.Run(runtimeName+"/directory", func(t *testing.T) {
			path := configureHookSecurityTestRuntime(t, runtimeName)
			if err := os.MkdirAll(path, 0o700); err != nil {
				t.Fatal(err)
			}
			if _, err := InstallHooks(runtimeName, ModeRaw, "/usr/local/bin/witself", "default", "default", "scott", "home"); err == nil ||
				!strings.Contains(err.Error(), "real regular file") {
				t.Fatalf("install error = %v", err)
			}
			if _, err := RemoveHooks(runtimeName); err == nil || !strings.Contains(err.Error(), "real regular file") {
				t.Fatalf("remove error = %v", err)
			}
		})
	}
}

func TestHookMutationRejectsOversizedConfig(t *testing.T) {
	path := configureHookSecurityTestRuntime(t, RuntimeClaudeCode)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	original := bytes.Repeat([]byte{' '}, hookConfigReadLimit+1)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallHooks(RuntimeClaudeCode, ModeRaw, "/usr/local/bin/witself", "default", "default", "scott", "home"); err == nil ||
		!strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized hook config error = %v", err)
	}
	assertHookSecurityFileBytes(t, path, original)
}

func TestSharedHookInstallPreservesAmbiguousForeignDocuments(t *testing.T) {
	for name, original := range map[string]string{
		"duplicate key":    `{"foreign":1,"foreign":2}`,
		"null root":        `null`,
		"non-object hooks": `{"foreign":"keep","hooks":"custom"}`,
		"non-array event":  `{"foreign":"keep","hooks":{"SessionStart":{"command":"custom"}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := configureHookSecurityTestRuntime(t, RuntimeClaudeCode)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := InstallHooks(RuntimeClaudeCode, ModeRaw, "/usr/local/bin/witself", "default", "default", "scott", "home"); err == nil {
				t.Fatal("install accepted an ambiguous foreign hook document")
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != original {
				t.Fatalf("foreign hook document changed: got %q want %q", got, original)
			}
			_, _ = RemoveHooks(RuntimeClaudeCode)
			got, err = os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != original {
				t.Fatalf("foreign hook document changed during remove: got %q want %q", got, original)
			}
		})
	}
}

func TestRemoveHooksWithoutOwnedHandlersPreservesExactForeignBytes(t *testing.T) {
	path := configureHookSecurityTestRuntime(t, RuntimeClaudeCode)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	original := []byte("{\n  \"foreign\" : true,\n  \"hooks\" : {\"Stop\":[{\"hooks\":[{\"command\":\"custom\"}]}]}\n}\n")
	if err := os.WriteFile(path, original, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := RemoveHooks(RuntimeClaudeCode); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("foreign-only config was reformatted: got %q want %q", got, original)
	}
}

func TestHookCASRejectsStaleContentAndIdentity(t *testing.T) {
	t.Run("content", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "hooks.json")
		if err := os.WriteFile(path, []byte(`{"foreign":"before"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		snapshot, err := readHookFileSnapshot(path)
		if err != nil {
			t.Fatal(err)
		}
		later := []byte(`{"foreign":"concurrent"}`)
		if err := os.WriteFile(path, later, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := writeHookJSONAtomicCAS(path, map[string]any{"hooks": map[string]any{}}, snapshot); err == nil ||
			!strings.Contains(err.Error(), "changed concurrently") {
			t.Fatalf("CAS error = %v", err)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, later) {
			t.Fatalf("concurrent content was overwritten: got %q want %q", got, later)
		}
	})

	t.Run("identity", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "hooks.json")
		original := []byte(`{"foreign":"same bytes"}`)
		if err := os.WriteFile(path, original, 0o600); err != nil {
			t.Fatal(err)
		}
		snapshot, err := readHookFileSnapshot(path)
		if err != nil {
			t.Fatal(err)
		}
		replacement := filepath.Join(dir, "replacement.json")
		if err := os.WriteFile(replacement, original, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := replaceFileAtomic(replacement, path); err != nil {
			t.Fatal(err)
		}
		if err := removeHookFileCAS(path, snapshot); err == nil || !strings.Contains(err.Error(), "changed concurrently") {
			t.Fatalf("CAS error = %v", err)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, original) {
			t.Fatalf("replacement identity changed: got %q want %q", got, original)
		}
	})
}

func TestGrokDedicatedHookFileRefusesForeignContentAndDrift(t *testing.T) {
	t.Run("foreign preexisting file", func(t *testing.T) {
		path := configureHookSecurityTestRuntime(t, RuntimeGrokBuild)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		original := []byte(`{"hooks":{},"foreign":"keep"}`)
		if err := os.WriteFile(path, original, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := InstallHooks(RuntimeGrokBuild, ModeRaw, "/usr/local/bin/witself", "default", "default", "scott", "home"); err == nil ||
			!strings.Contains(err.Error(), "non-Witself settings") {
			t.Fatalf("install error = %v", err)
		}
		assertHookSecurityFileBytes(t, path, original)
		if _, err := RemoveHooks(RuntimeGrokBuild); err == nil || !strings.Contains(err.Error(), "non-Witself settings") {
			t.Fatalf("remove error = %v", err)
		}
		assertHookSecurityFileBytes(t, path, original)
	})

	t.Run("owned file drift", func(t *testing.T) {
		path := configureHookSecurityTestRuntime(t, RuntimeGrokBuild)
		if _, err := InstallHooks(RuntimeGrokBuild, ModeRaw, "/usr/local/bin/witself", "default", "default", "scott", "home"); err != nil {
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
		delete(hooks, "SessionStart")
		drifted, err := json.Marshal(root)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, drifted, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := InstallHooks(RuntimeGrokBuild, ModeRaw, "/usr/local/bin/witself", "default", "default", "new-agent", "home"); err == nil ||
			!strings.Contains(err.Error(), "has drifted") {
			t.Fatalf("reinstall error = %v", err)
		}
		assertHookSecurityFileBytes(t, path, drifted)
		if _, err := RemoveHooks(RuntimeGrokBuild); err == nil || !strings.Contains(err.Error(), "has drifted") {
			t.Fatalf("remove error = %v", err)
		}
		assertHookSecurityFileBytes(t, path, drifted)
	})
}

func configureHookSecurityTestRuntime(t *testing.T, runtimeName string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	switch runtimeName {
	case RuntimeClaudeCode:
		root := filepath.Join(home, ".claude")
		t.Setenv("CLAUDE_CONFIG_DIR", root)
		return filepath.Join(root, "settings.json")
	case RuntimeGrokBuild:
		root := filepath.Join(home, ".grok")
		t.Setenv("GROK_HOME", root)
		return filepath.Join(root, "hooks", "witself.json")
	default:
		t.Fatalf("unsupported hook security test runtime %q", runtimeName)
		return ""
	}
}

func assertHookSecurityFileBytes(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("hook file changed: got %q want %q", got, want)
	}
}
