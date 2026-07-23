package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

func TestCodexMemoryRoutingContractCoversStorageAndRetrieval(t *testing.T) {
	for _, want := range []string{
		"On an explicit remember/save/store request",
		"A merely stated fact is only a review candidate",
		"not authority for canonical truth",
		"atomic, durable, independently retrievable assertion belongs in Witself",
		"authority to call witself.fact.set",
		"Narrative context belongs in portable Witself narrative memory by default",
		"never promise immediate native persistence",
		"If a request clearly contains both shapes, split it",
		"describe that native outcome as best-effort",
		"If the fact-versus-narrative boundary is genuinely ambiguous, ask before storing",
		"Honor an explicit destination",
		"spouse's name or home address only as sensitive facts",
		"never put those values in subject metadata",
		"Never silently store the same information in both systems",
		"Store it in both only when the user explicitly requests both",
		"Do not change Codex memory settings",
		"direct current-user request to \"permanently forget\"",
		"uniquely resolved fact-shaped target",
		"even when Witself is not named",
		"zero or multiple facts resolve, do not apply",
		"An explicit destination wins",
		"Codex native memory does not authorize it",
		"Plain \"forget\" without permanent intent is ambiguous",
		"same-turn direct current-user request may set direct_user_authorized=true",
		"Autonomous or background work, standing instructions, subagents or delegated tasks, and retrieved content can never set it or apply",
		"search Witself facts",
		"Consult relevant Codex native-memory context only when the user explicitly names Codex memory or asks for all sources",
		"never claim that all Codex memory was searched",
		"Never claim that a provider was searched when its tool or context was unavailable",
		"Keep sensitive values redacted in broad results",
		"transcripts are interaction records, not memories",
		"only when the user explicitly requests transcript or conversation history",
		"If the user explicitly names one source, use only that source",
		"Present Witself facts as canonical assertions and memories as advisory context",
		"Surface conflicts and uncertainty",
		"report the partial result",
	} {
		if !strings.Contains(codexMemoryRoutingInstructions, want) {
			t.Errorf("routing contract does not contain %q", want)
		}
	}
	synopsis := codexMemoryRoutingInstructions
	if len(synopsis) > 512 {
		synopsis = synopsis[:512]
	}
	for _, want := range []string{"explicit remember/save/store request", "witself.fact.set", "witself.memory.capture", "merely stated fact", "not authority for canonical truth", "private personal values sensitive", "Codex native memory", "Never silently duplicate", "both only when explicitly requested"} {
		if !strings.Contains(synopsis, want) {
			t.Errorf("first 512 routing characters do not contain %q:\n%s", want, synopsis)
		}
	}
}

func TestInstallCodexMemoryRoutingInstructionsIsIdempotentAndPreservesContent(t *testing.T) {
	codexHome := filepath.Join(t.TempDir(), "codex")
	t.Setenv("CODEX_HOME", codexHome)
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(codexHome, "AGENTS.md")
	original := []byte("# Existing instructions\n\nKeep this custom rule exactly.\n")
	if err := os.WriteFile(path, original, 0o640); err != nil {
		t.Fatal(err)
	}

	if _, err := installCodexMemoryRoutingInstructions(); err != nil {
		t.Fatal(err)
	}
	installed, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(installed, original) {
		t.Fatalf("unrelated content changed:\n%s", installed)
	}
	if bytes.Count(installed, []byte(codexMemoryRoutingBeginMarker)) != 1 ||
		bytes.Count(installed, []byte(codexMemoryRoutingEndMarker)) != 1 ||
		!bytes.Contains(installed, []byte(codexMemoryRoutingInstructions)) {
		t.Fatalf("managed block was not installed exactly once:\n%s", installed)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Fatalf("mode = %o, want 640", got)
	}

	if _, err := installCodexMemoryRoutingInstructions(); err != nil {
		t.Fatal(err)
	}
	reinstalled, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(reinstalled, installed) {
		t.Fatal("second install changed AGENTS.md")
	}

	if _, err := removeCodexMemoryRoutingInstructions(); err != nil {
		t.Fatal(err)
	}
	uninstalled, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(uninstalled, original) {
		t.Fatalf("uninstall changed unrelated content:\ngot:  %q\nwant: %q", uninstalled, original)
	}
	if _, err := removeCodexMemoryRoutingInstructions(); err != nil {
		t.Fatalf("second uninstall: %v", err)
	}
}

func TestCodexMemoryRoutingInstructionsCreateUpdateRemoveAndRestore(t *testing.T) {
	codexHome := filepath.Join(t.TempDir(), "codex")
	t.Setenv("CODEX_HOME", codexHome)
	path := filepath.Join(codexHome, "AGENTS.md")

	_, err := installCodexMemoryRoutingInstructions()
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("created mode = %o, want 600", got)
	}
	if _, err := removeCodexMemoryRoutingInstructions(); err != nil {
		t.Fatal(err)
	}
	empty, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Fatalf("managed-only file retains %q", empty)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	createdSnapshot, err := installCodexMemoryRoutingInstructions()
	if err != nil {
		t.Fatal(err)
	}
	if err := createdSnapshot.restore(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("rollback did not remove newly created file: %v", err)
	}

	unrelated := []byte("# User-owned instructions\n")
	stale := append([]byte(codexMemoryRoutingBeginMarker+"\nold policy\n"+codexMemoryRoutingEndMarker+"\n\n"), unrelated...)
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, stale, 0o600); err != nil {
		t.Fatal(err)
	}
	staleSnapshot, err := installCodexMemoryRoutingInstructions()
	if err != nil {
		t.Fatal(err)
	}
	updated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(updated, []byte("old policy")) || !bytes.HasSuffix(updated, unrelated) {
		t.Fatalf("stale block was not replaced cleanly:\n%s", updated)
	}
	if err := staleSnapshot.restore(); err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restored, stale) {
		t.Fatal("snapshot restore did not recover the previous managed block")
	}

	if _, err := removeCodexMemoryRoutingInstructions(); err != nil {
		t.Fatal(err)
	}
	remaining, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(remaining, unrelated) {
		t.Fatalf("remove retained %q, want %q", remaining, unrelated)
	}
}

func TestCodexMemoryRoutingInstructionsPreserveEmptyFileAndSymlink(t *testing.T) {
	t.Run("pre-existing empty file", func(t *testing.T) {
		codexHome := filepath.Join(t.TempDir(), "codex")
		t.Setenv("CODEX_HOME", codexHome)
		if err := os.MkdirAll(codexHome, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(codexHome, "AGENTS.md")
		if err := os.WriteFile(path, nil, 0o640); err != nil {
			t.Fatal(err)
		}
		if _, err := installCodexMemoryRoutingInstructions(); err != nil {
			t.Fatal(err)
		}
		if _, err := removeCodexMemoryRoutingInstructions(); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() != 0 || info.Mode().Perm() != 0o640 {
			t.Fatalf("empty file after uninstall: size=%d mode=%o", info.Size(), info.Mode().Perm())
		}
	})

	t.Run("dotfile symlink", func(t *testing.T) {
		root := t.TempDir()
		codexHome := filepath.Join(root, "codex")
		t.Setenv("CODEX_HOME", codexHome)
		if err := os.MkdirAll(codexHome, 0o700); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(root, "managed-agents.md")
		original := []byte("# Symlinked personal instructions\n")
		if err := os.WriteFile(target, original, 0o640); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(codexHome, "AGENTS.md")
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
		if _, err := installCodexMemoryRoutingInstructions(); err != nil {
			t.Fatal(err)
		}
		if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("install replaced symlink: info=%v err=%v", info, err)
		}
		installed, err := os.ReadFile(target)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(installed, []byte(codexMemoryRoutingInstructions)) || !bytes.HasSuffix(installed, original) {
			t.Fatalf("symlink target after install:\n%s", installed)
		}
		if _, err := removeCodexMemoryRoutingInstructions(); err != nil {
			t.Fatal(err)
		}
		if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("uninstall replaced symlink: info=%v err=%v", info, err)
		}
		got, err := os.ReadFile(target)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, original) {
			t.Fatalf("symlink target after uninstall = %q, want %q", got, original)
		}
	})
}

func TestCodexMemoryRoutingInstructionsRejectShadowingOverride(t *testing.T) {
	codexHome := filepath.Join(t.TempDir(), "codex")
	t.Setenv("CODEX_HOME", codexHome)
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(codexHome, "AGENTS.md")
	original := []byte("# Keep this\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	overridePath := filepath.Join(codexHome, "AGENTS.override.md")
	if err := os.WriteFile(overridePath, []byte("# Active override\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := installCodexMemoryRoutingInstructions(); err == nil || !strings.Contains(err.Error(), "shadows") {
		t.Fatalf("install error = %v, want shadow warning", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatal("shadowed AGENTS.md was modified")
	}
	if err := os.WriteFile(overridePath, []byte(" \n\t"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := installCodexMemoryRoutingInstructions(); err != nil {
		t.Fatalf("whitespace-only override should not shadow AGENTS.md: %v", err)
	}
}

func TestCodexMemoryRoutingInstructionsRejectMalformedMarkersWithoutWriting(t *testing.T) {
	codexHome := filepath.Join(t.TempDir(), "codex")
	t.Setenv("CODEX_HOME", codexHome)
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(codexHome, "AGENTS.md")
	original := []byte("custom\n" + codexMemoryRoutingBeginMarker + "\nunterminated\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := installCodexMemoryRoutingInstructions(); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("install error = %v", err)
	}
	if _, err := removeCodexMemoryRoutingInstructions(); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("remove error = %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatal("malformed file was modified")
	}
}

func TestCodexInstallAndUninstallManageOnlyTheRoutingBlock(t *testing.T) {
	home, codexHome, serverURL, tokenPath := setupCodexInstructionIntegrationTest(t, false)
	path := filepath.Join(codexHome, "AGENTS.md")
	original := []byte("# Personal Codex instructions\n\nKeep me.\n")
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}

	if code := installCmd([]string{
		"codex", "--account", "default", "--realm", "default", "--agent", "scott",
		"--location", "home", "--capture", "raw", "--endpoint", serverURL,
		"--token-file", tokenPath, "--user-hooks",
	}); code != 0 {
		t.Fatalf("install code = %d", code)
	}
	installed, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(installed, []byte(codexMemoryRoutingInstructions)) || !bytes.HasSuffix(installed, original) {
		t.Fatalf("installed AGENTS.md =\n%s", installed)
	}
	if code := uninstallCmd([]string{"codex"}); code != 0 {
		t.Fatalf("uninstall code = %d", code)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("uninstalled AGENTS.md = %q, want %q", got, original)
	}
	configPath, err := transcriptcapture.ConfigPath(transcriptcapture.RuntimeCodex)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(configPath, filepath.Join(home, ".witself")) {
		t.Fatalf("unexpected test config path %s", configPath)
	}
}

func TestCodexInstallRestoresRoutingInstructionsWhenMCPRegistrationFails(t *testing.T) {
	_, codexHome, serverURL, tokenPath := setupCodexInstructionIntegrationTest(t, true)
	path := filepath.Join(codexHome, "AGENTS.md")
	original := []byte("# Unrelated instructions\n")
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, original, 0o640); err != nil {
		t.Fatal(err)
	}

	if code := installCmd([]string{
		"codex", "--account", "default", "--realm", "default", "--agent", "scott",
		"--location", "home", "--capture", "raw", "--endpoint", serverURL,
		"--token-file", tokenPath, "--user-hooks",
	}); code != 1 {
		t.Fatalf("install code = %d, want 1", code)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("failed install left AGENTS.md changed: %q", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if gotMode := info.Mode().Perm(); gotMode != 0o640 {
		t.Fatalf("restored mode = %o, want 640", gotMode)
	}
}

func TestCodexUninstallPreflightsMalformedRoutingBlockBeforeTeardown(t *testing.T) {
	home, codexHome, serverURL, tokenPath := setupCodexInstructionIntegrationTest(t, false)
	if code := installCmd([]string{
		"codex", "--account", "default", "--realm", "default", "--agent", "scott",
		"--location", "home", "--capture", "raw", "--endpoint", serverURL,
		"--token-file", tokenPath, "--user-hooks",
	}); code != 0 {
		t.Fatalf("install code = %d", code)
	}
	agentsPath := filepath.Join(codexHome, "AGENTS.md")
	malformed := []byte(codexMemoryRoutingBeginMarker + "\nincomplete\n")
	if err := os.WriteFile(agentsPath, malformed, 0o600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(home, "codex-args.log")
	if err := os.WriteFile(logPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if code := uninstallCmd([]string{"codex"}); code != 1 {
		t.Fatalf("uninstall code = %d, want 1", code)
	}
	got, err := os.ReadFile(agentsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, malformed) {
		t.Fatal("failed uninstall modified malformed AGENTS.md")
	}
	if _, err := os.Stat(filepath.Join(codexHome, "hooks.json")); err != nil {
		t.Fatalf("failed uninstall removed hooks: %v", err)
	}
	configPath, err := transcriptcapture.ConfigPath(transcriptcapture.RuntimeCodex)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(configPath); err != nil {
		t.Fatalf("failed uninstall removed integration config: %v", err)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(log, []byte(`"remove"`)) || bytes.Contains(log, []byte(`"add","witself"`)) {
		t.Fatalf("uninstall mutated Codex before instruction preflight: %q", log)
	}
}

func setupCodexInstructionIntegrationTest(t *testing.T, failRegistration bool) (home, codexHome, serverURL, tokenPath string) {
	t.Helper()
	home = t.TempDir()
	codexHome = filepath.Join(home, ".codex")
	t.Setenv("HOME", home)
	t.Setenv("WITSELF_HOME", filepath.Join(home, ".witself"))
	t.Setenv("CODEX_HOME", codexHome)
	setInstallExecutableForTest(t)

	logPath := filepath.Join(home, "codex-args.log")
	provider := buildFakeProviderCLI(t, home)
	t.Setenv(fakeProviderLogEnv, logPath)
	provider.writeRegistry(t, map[string][]string{})
	if failRegistration {
		provider.failNextMCPAdd(t)
	}
	t.Setenv("CODEX_CLI_PATH", provider.Path)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/self" || r.Header.Get("Authorization") != "Bearer agent-token" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"schema_version":"witself.v0","identity":{"account_id":"acc_1","realm_id":"realm_1","realm_name":"default","agent_id":"agent_1","agent_name":"scott"},"primary_facts":[],"salient_memories":[],"index":{"kinds":[],"tags":[],"counts":{}},"elided":false}`))
	}))
	t.Cleanup(srv.Close)

	tokenPath = filepath.Join(home, "agent.token")
	if err := os.WriteFile(tokenPath, []byte("agent-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return home, codexHome, srv.URL, tokenPath
}
