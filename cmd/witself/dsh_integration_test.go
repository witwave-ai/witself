package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

// dshTopologyClass reports the verification class an error carries, or an
// empty string for plain drift.
func dshTopologyClass(err error) string {
	var classified *integrationTopologyClassError
	if errors.As(err, &classified) {
		return classified.class
	}
	return ""
}

func dshTestConfig() transcriptcapture.Config {
	return transcriptcapture.Config{
		Runtime:        transcriptcapture.RuntimeDSH,
		RuntimeVersion: "0.1.5-rc.1",
		Account:        "default",
		AccountID:      "acc_test",
		Realm:          "default",
		RealmID:        "rlm_test",
		Agent:          "dsh-test-bot",
		AgentID:        "agt_test",
		AgentName:      "dsh-test-bot",
		Location:       transcriptcapture.Location{ID: "loc_test", Name: "home"},
		CaptureMode:    transcriptcapture.ModeRaw,
		HookMode:       transcriptcapture.HookModeNone,
	}
}

// configuredDSHTestConfig pins DSH_HOME and WITSELF_HOME to fresh temp trees so
// no test can read, write, or even resolve a developer's real ~/.dsh.
func configuredDSHTestConfig(t *testing.T) transcriptcapture.Config {
	t.Helper()
	base := t.TempDir()
	dshHome := filepath.Join(base, "dsh")
	witselfHome := filepath.Join(base, "witself")
	if err := os.MkdirAll(witselfHome, 0o700); err != nil {
		t.Fatal(err)
	}
	runtimeCLI := copilotTestFile(t, base, "bin", "dsh")
	executable := copilotTestFile(t, base, "bin", "witself")
	t.Setenv("DSH_HOME", dshHome)
	t.Setenv("WITSELF_HOME", witselfHome)
	cfg := dshTestConfig()
	if err := configureDSHBinding(&cfg, runtimeCLI, executable); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// stubDSHDumpConfig replaces the composed-tree probe. Unit tests exercise the
// patch-file manager, not dsh's Node boot path.
func stubDSHDumpConfig(t *testing.T, output string, err error) *[][]string {
	t.Helper()
	calls := &[][]string{}
	previous := runDSHDumpConfig
	runDSHDumpConfig = func(_, _ string, _ time.Duration, args ...string) ([]byte, error) {
		*calls = append(*calls, append([]string(nil), args...))
		return []byte(output), err
	}
	t.Cleanup(func() { runDSHDumpConfig = previous })
	return calls
}

func dshComposedFixture() string {
	return "profile: headless\nplugins:\n" +
		"  - id: foreign-plugin\n    name: '@deepseek-ai/other'\n" +
		"  - id: " + dshPatchRowID + "\n    name: '" + dshMCPClientPluginName + "'\n"
}

func readDSHPatchFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestConfigureDSHBindingPinsRootsAndEnvironment(t *testing.T) {
	base := t.TempDir()
	dshHome := filepath.Join(base, "dsh")
	witselfHome := filepath.Join(base, "witself")
	runtimeCLI := copilotTestFile(t, base, "bin", "dsh")
	executable := copilotTestFile(t, base, "bin", "witself")
	t.Setenv("DSH_HOME", dshHome)
	t.Setenv("WITSELF_HOME", witselfHome)

	cfg := dshTestConfig()
	if err := configureDSHBinding(&cfg, runtimeCLI, executable); err != nil {
		t.Fatal(err)
	}
	canonicalCLI, _ := cleanCopilotInvocationPath("test CLI", runtimeCLI)
	canonicalExecutable, _ := cleanCopilotInvocationPath("test executable", executable)
	canonicalDSHHome, _ := cleanCopilotAbsolutePath("test DSH_HOME", dshHome)
	canonicalWitselfHome, _ := cleanCopilotAbsolutePath("test WITSELF_HOME", witselfHome)
	if cfg.RuntimeCLICommand != canonicalCLI || cfg.MCPCommand != canonicalExecutable ||
		cfg.RuntimeConfigRoot != canonicalDSHHome ||
		cfg.RuntimeMCPConfigPath != filepath.Join(canonicalDSHHome, dshPatchFileName) ||
		!reflect.DeepEqual(cfg.MCPEnvironment, map[string]string{
			"DSH_HOME": canonicalDSHHome, "WITSELF_HOME": canonicalWitselfHome,
		}) {
		t.Fatalf("configured dsh binding = %#v", cfg)
	}
	if err := transcriptcapture.SaveConfig(cfg); err != nil {
		t.Fatalf("configured binding failed durable validation: %v", err)
	}
}

func TestDSHManagedPatchBlockRendersExactMCPRow(t *testing.T) {
	cfg := configuredDSHTestConfig(t)
	block, err := dshManagedPatchBlock(cfg)
	if err != nil {
		t.Fatal(err)
	}
	text := string(block)
	if !strings.HasPrefix(text, dshPatchBlockBeginMarker+"\n") || !strings.HasSuffix(text, "\n"+dshPatchBlockEndMarker) {
		t.Fatalf("block is not fenced by its markers:\n%s", text)
	}
	for _, want := range []string{
		"- insert:\n",
		"    - id: " + dshPatchRowID + "\n",
		"      name: '" + dshMCPClientPluginName + "'\n",
		"        transport: stdio\n",
		"        serverName: " + dshMCPServerName + "\n",
		"        command: '" + cfg.MCPCommand + "'\n",
		"'mcp', 'serve', '--runtime', 'dsh', '--account', 'default', '--realm', 'default', '--agent', 'dsh-test-bot', '--location', 'home'",
		// dsh scrubs ambient DSH_* names from MCP children, so the block must
		// carry DSH_HOME itself for `mcp serve` to find the installed root.
		"env: {'DSH_HOME': '" + cfg.RuntimeConfigRoot + "', 'WITSELF_HOME': '" + cfg.MCPEnvironment["WITSELF_HOME"] + "'}",
		"        failOnStartupError: false\n",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("block is missing %q:\n%s", want, text)
		}
	}
	id, name, err := parseDSHManagedPatchBlock(block)
	if err != nil {
		t.Fatalf("rendered block is not valid YAML: %v", err)
	}
	if id != dshPatchRowID || name != dshMCPClientPluginName {
		t.Fatalf("parsed row = %q/%q", id, name)
	}
	if !strings.Contains(text, "'--runtime', 'dsh'") {
		t.Fatalf("block does not pass --runtime dsh:\n%s", text)
	}
}

func TestDSHPatchInstallAcceptsOwnedShapesAndPreservesForeignBytes(t *testing.T) {
	// Foreign rows include a `!!js` tagged expression and a bare `- id:`
	// override row. A generic YAML round-trip would destroy both.
	foreign := "# operator notes\n" +
		"- id: existing-row\n" +
		"  config:\n" +
		"    retries: 3\n" +
		"- insert:\n" +
		"    - id: operator-plugin\n" +
		"      name: '@operator/plugin'\n" +
		"      config:\n" +
		"        factory: !!js/function >\n" +
		"          function () { return 1 }\n"

	for _, test := range []struct {
		name    string
		initial string
		// mustRetain is checked verbatim after install and again after removal.
		mustRetain string
		wantAfter  string
	}{
		{name: "missing file", initial: "\x00"},
		{name: "empty array", initial: "[]\n"},
		{
			name:       "empty array with comments",
			initial:    "# generated by dsh\n# edit freely\n[]\n",
			mustRetain: "# generated by dsh\n# edit freely\n",
			wantAfter:  "# generated by dsh\n# edit freely\n[]\n",
		},
		{name: "empty array without trailing newline", initial: "[]"},
		{name: "comments only", initial: "# nothing here yet\n", mustRetain: "# nothing here yet\n"},
		{
			name:       "block sequence with foreign rows",
			initial:    foreign,
			mustRetain: foreign,
			wantAfter:  foreign,
		},
		{
			name:       "block sequence without trailing newline",
			initial:    strings.TrimSuffix(foreign, "\n"),
			mustRetain: strings.TrimSuffix(foreign, "\n"),
			// Appending the block adds the missing terminator; removal keeps it.
			wantAfter: foreign,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := configuredDSHTestConfig(t)
			stubDSHDumpConfig(t, dshComposedFixture(), nil)
			if _, err := installRuntimeMemoryRoutingInstructions(transcriptcapture.RuntimeDSH); err != nil {
				t.Fatal(err)
			}
			if test.initial != "\x00" {
				if err := os.MkdirAll(cfg.RuntimeConfigRoot, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(cfg.RuntimeMCPConfigPath, []byte(test.initial), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			block, err := dshManagedPatchBlock(cfg)
			if err != nil {
				t.Fatal(err)
			}
			touched, err := installDSHPatchBlock(cfg)
			if err != nil || !touched {
				t.Fatalf("install touched=%t err=%v", touched, err)
			}
			installed := readDSHPatchFile(t, cfg.RuntimeMCPConfigPath)
			if !strings.Contains(installed, string(block)+"\n") {
				t.Fatalf("installed file does not contain the exact block:\n%s", installed)
			}
			if test.mustRetain != "" && !strings.Contains(installed, test.mustRetain) {
				t.Fatalf("install did not preserve foreign bytes:\n%s", installed)
			}
			if strings.Contains(installed, "\n[]\n") || strings.HasPrefix(installed, "[]\n") {
				t.Fatalf("install left the placeholder empty array behind:\n%s", installed)
			}
			if err := validateDSHPersistedTopology(cfg); err != nil {
				t.Fatalf("verify after install: %v", err)
			}

			// A reinstall over an identical block must be a no-op write.
			touched, err = installDSHPatchBlock(cfg)
			if err != nil || touched {
				t.Fatalf("idempotent reinstall touched=%t err=%v", touched, err)
			}

			touched, err = removeDSHPatchBlock(&cfg)
			if err != nil || !touched {
				t.Fatalf("remove touched=%t err=%v", touched, err)
			}
			removed := readDSHPatchFile(t, cfg.RuntimeMCPConfigPath)
			if strings.Contains(removed, dshPatchBlockBeginMarker) || strings.Contains(removed, dshPatchBlockEndMarker) {
				t.Fatalf("removal left a marker behind:\n%s", removed)
			}
			want := test.wantAfter
			if want == "" {
				want = test.mustRetain + "[]\n"
			}
			if removed != want {
				t.Fatalf("file after removal = %q, want %q", removed, want)
			}
			if entries, err := os.ReadDir(cfg.RuntimeConfigRoot); err != nil {
				t.Fatal(err)
			} else {
				for _, entry := range entries {
					if strings.HasPrefix(entry.Name(), "."+dshPatchFileName) {
						t.Fatalf("atomic write left a temporary file behind: %s", entry.Name())
					}
				}
			}
			touched, err = removeDSHPatchBlock(&cfg)
			if err != nil || touched {
				t.Fatalf("second removal touched=%t err=%v", touched, err)
			}
		})
	}
}

func TestDSHPatchInstallReplacesOnlyItsOwnBlockOnRebind(t *testing.T) {
	cfg := configuredDSHTestConfig(t)
	stubDSHDumpConfig(t, dshComposedFixture(), nil)
	if _, err := installRuntimeMemoryRoutingInstructions(transcriptcapture.RuntimeDSH); err != nil {
		t.Fatal(err)
	}
	foreign := "- id: existing-row\n  config:\n    retries: 3\n"
	if err := os.MkdirAll(cfg.RuntimeConfigRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.RuntimeMCPConfigPath, []byte(foreign), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := installDSHPatchBlock(cfg); err != nil {
		t.Fatal(err)
	}

	rebound := cfg
	rebound.Agent = "dsh-other-bot"
	rebound.AgentName = "dsh-other-bot"
	reboundBlock, err := dshManagedPatchBlock(rebound)
	if err != nil {
		t.Fatal(err)
	}
	plan, _, err := prepareDSHPatchInstallPlan(rebound.RuntimeCLICommand, rebound, &cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := installDSHPatchBlockWithPlan(plan); err != nil {
		t.Fatal(err)
	}
	updated := readDSHPatchFile(t, cfg.RuntimeMCPConfigPath)
	if !strings.HasPrefix(updated, foreign) {
		t.Fatalf("rebind disturbed foreign rows:\n%s", updated)
	}
	if strings.Count(updated, dshPatchBlockBeginMarker) != 1 ||
		!strings.Contains(updated, string(reboundBlock)) ||
		strings.Contains(updated, "'dsh-test-bot'") {
		t.Fatalf("rebind did not replace exactly one managed block:\n%s", updated)
	}
	if err := validateDSHPersistedTopology(rebound); err != nil {
		t.Fatalf("verify after rebind: %v", err)
	}
}

func TestDSHPatchInstallRefusesForeignAndTamperedFiles(t *testing.T) {
	cfg := configuredDSHTestConfig(t)
	block, err := dshManagedPatchBlock(cfg)
	if err != nil {
		t.Fatal(err)
	}
	fenced := string(block)
	for _, test := range []struct {
		name    string
		content string
		want    string
	}{
		{"flow sequence", "[ {id: x} ]\n", "not a block-style YAML list"},
		{"mapping", "plugins:\n  - id: x\n", "not a block-style YAML list"},
		{"array with extra rows", "[]\n- id: x\n", "not a block-style YAML list"},
		{
			"other managed version",
			strings.ReplaceAll(fenced, dshPatchBlockVersion, "v2") + "\n",
			"another version",
		},
		{
			"truncated fence",
			strings.ReplaceAll(fenced, dshPatchBlockEndMarker, "") + "\n",
			"truncated Witself managed block fence",
		},
		{"duplicate blocks", fenced + "\n" + fenced + "\n", "multiple Witself managed blocks"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := os.MkdirAll(cfg.RuntimeConfigRoot, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(cfg.RuntimeMCPConfigPath, []byte(test.content), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := installDSHPatchBlock(cfg)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("install error = %v, want %q", err, test.want)
			}
			if err != nil && !strings.Contains(err.Error(), cfg.RuntimeMCPConfigPath) {
				t.Fatalf("refusal does not name the patch file: %v", err)
			}
			if got := readDSHPatchFile(t, cfg.RuntimeMCPConfigPath); got != test.content {
				t.Fatalf("refused install modified the file:\n%s", got)
			}
		})
	}
}

// The exact fence is Witself's region. A stale or drifted block (a wiped
// ~/.witself, an interrupted first install, an editor that re-quoted the rows)
// must never lock the operator out: install replaces it and uninstall removes
// it, while foreign rows outside the fence stay untouched.
func TestDSHPatchInstallReclaimsAndRemovesADriftedOrUnrecordedBlock(t *testing.T) {
	cfg := configuredDSHTestConfig(t)
	stubDSHDumpConfig(t, dshComposedFixture(), nil)
	block, err := dshManagedPatchBlock(cfg)
	if err != nil {
		t.Fatal(err)
	}
	foreign := "- id: existing-row\n  config:\n    retries: 3\n"
	drifted := strings.Replace(string(block), "transport: stdio", "transport: \"stdio\"", 1)
	if drifted == string(block) {
		t.Fatal("fixture did not drift the block")
	}
	if err := os.MkdirAll(cfg.RuntimeConfigRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.RuntimeMCPConfigPath, []byte(foreign+drifted+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// No integration record at all: a first install still claims the fence.
	plan, _, err := prepareDSHPatchInstallPlan(cfg.RuntimeCLICommand, cfg, nil)
	if err != nil {
		t.Fatalf("first install over an unrecorded block = %v", err)
	}
	if touched, err := installDSHPatchBlockWithPlan(plan); err != nil || !touched {
		t.Fatalf("reclaim touched=%t err=%v", touched, err)
	}
	installed := readDSHPatchFile(t, cfg.RuntimeMCPConfigPath)
	if installed != foreign+string(block)+"\n" {
		t.Fatalf("reclaimed file = %q", installed)
	}
	// Drift it again and prove uninstall removes exactly the fence.
	if err := os.WriteFile(cfg.RuntimeMCPConfigPath, []byte(foreign+drifted+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if touched, err := removeDSHPatchBlock(&cfg); err != nil || !touched {
		t.Fatalf("remove drifted block touched=%t err=%v", touched, err)
	}
	if got := readDSHPatchFile(t, cfg.RuntimeMCPConfigPath); got != foreign {
		t.Fatalf("removal of a drifted block did not restore the foreign rows exactly: %q", got)
	}
}

func TestCurrentDSHConfigRootExpandsATildeLikeDSH(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, test := range []struct{ value, want string }{
		{"~", home},
		{"~/harness", filepath.Join(home, "harness")},
		{"", filepath.Join(home, ".dsh")},
	} {
		t.Setenv("DSH_HOME", test.value)
		got, err := currentDSHConfigRoot()
		if err != nil {
			t.Fatalf("DSH_HOME=%q: %v", test.value, err)
		}
		want, _ := cleanCopilotAbsolutePath("want", test.want)
		if got != want {
			t.Fatalf("DSH_HOME=%q resolved to %q, want %q", test.value, got, want)
		}
	}
}

// A shared AGENTS.md is commonly a symlink into dotfiles. The routing block is
// written through it, so the transaction commit must sync its target instead
// of leaving the journal pending forever.
func TestDSHTransactionCommitsThroughASymlinkedAGENTSFile(t *testing.T) {
	cfg := configuredDSHTestConfig(t)
	stubDSHDumpConfig(t, dshComposedFixture(), nil)
	if err := os.MkdirAll(cfg.RuntimeConfigRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "dotfiles-AGENTS.md")
	if err := os.WriteFile(target, []byte("# dotfiles guidance\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(cfg.RuntimeConfigRoot, dshMemoryRoutingFile)); err != nil {
		t.Fatal(err)
	}
	if err := transcriptcapture.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	persisted, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeDSH)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := beginDSHTransaction(dshTransactionInstall, nil, &persisted)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := installDSHPatchBlock(persisted); err != nil {
		t.Fatal(err)
	}
	if _, err := installRuntimeMemoryRoutingInstructions(transcriptcapture.RuntimeDSH); err != nil {
		t.Fatal(err)
	}
	if err := clearDSHTransaction(persisted.RuntimeConfigRoot, journal); err != nil {
		t.Fatalf("commit through a symlinked AGENTS.md: %v", err)
	}
	linked, err := os.ReadFile(target)
	if err != nil || !strings.Contains(string(linked), dshMemoryRoutingBeginMarker) ||
		!strings.Contains(string(linked), "# dotfiles guidance") {
		t.Fatalf("routing block did not land in the symlink target: %q, %v", linked, err)
	}
}

func TestDSHVerificationDistinguishesAbsentDriftedAndComposedState(t *testing.T) {
	cfg := configuredDSHTestConfig(t)
	stubDSHDumpConfig(t, dshComposedFixture(), nil)

	err := validateDSHPersistedTopology(cfg)
	if err == nil || dshTopologyClass(err) != integrationVerificationIncomplete {
		t.Fatalf("missing patch file verification = %v, want incomplete", err)
	}
	if _, err := installDSHPatchBlock(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := installRuntimeMemoryRoutingInstructions(transcriptcapture.RuntimeDSH); err != nil {
		t.Fatal(err)
	}
	if err := validateDSHPersistedTopology(cfg); err != nil {
		t.Fatalf("healthy verification = %v", err)
	}

	// Hand-edited block content must read as drift, not as absence.
	installed := readDSHPatchFile(t, cfg.RuntimeMCPConfigPath)
	tampered := strings.Replace(installed, "transport: stdio", "transport: streamable-http", 1)
	if tampered == installed {
		t.Fatal("test fixture did not tamper with the block")
	}
	if err := os.WriteFile(cfg.RuntimeMCPConfigPath, []byte(tampered), 0o600); err != nil {
		t.Fatal(err)
	}
	err = validateDSHPersistedTopology(cfg)
	if err == nil || dshTopologyClass(err) == integrationVerificationIncomplete ||
		!strings.Contains(err.Error(), "no longer matches") {
		t.Fatalf("tampered block verification = %v, want plain drift", err)
	}
	if err := os.WriteFile(cfg.RuntimeMCPConfigPath, []byte(installed), 0o600); err != nil {
		t.Fatal(err)
	}

	// A composed tree that never mounts the row is drift too: the block is
	// present but the home-level patch is not in effect.
	stubDSHDumpConfig(t, "profile: headless\nplugins:\n  - id: other\n    name: '@x/y'\n", nil)
	err = validateDSHPersistedTopology(cfg)
	if err == nil || !strings.Contains(err.Error(), "home-level patch is not in effect") {
		t.Fatalf("unmounted composed row verification = %v", err)
	}

	stubDSHDumpConfig(t, "", os.ErrPermission)
	err = validateDSHPersistedTopology(cfg)
	if err == nil || dshTopologyClass(err) != integrationVerificationUnavailable {
		t.Fatalf("failed dump-config verification = %v, want unavailable", err)
	}
}

func TestDSHVerificationSkipsComposedProbeWithoutAnExecutableCLI(t *testing.T) {
	cfg := configuredDSHTestConfig(t)
	calls := stubDSHDumpConfig(t, "", os.ErrPermission)
	if _, err := installDSHPatchBlock(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := installRuntimeMemoryRoutingInstructions(transcriptcapture.RuntimeDSH); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(cfg.RuntimeCLICommand); err != nil {
		t.Fatal(err)
	}
	if err := validateDSHPersistedTopology(cfg); err != nil {
		t.Fatalf("verification without a CLI = %v, want healthy", err)
	}
	if len(*calls) != 0 {
		t.Fatalf("verification probed the missing CLI: %v", *calls)
	}
}

func TestDSHMemoryRoutingSharesAGENTSFileAndSurvivesRemoval(t *testing.T) {
	cfg := configuredDSHTestConfig(t)
	spec, displayName, managed, err := runtimeMemoryRoutingSpecAt(transcriptcapture.RuntimeDSH, "")
	if err != nil || !managed {
		t.Fatalf("routing spec managed=%t err=%v", managed, err)
	}
	if displayName != "DeepSeek Harness" {
		t.Fatalf("display name = %q", displayName)
	}
	if spec.path != filepath.Join(cfg.RuntimeConfigRoot, "AGENTS.md") {
		t.Fatalf("routing path = %q", spec.path)
	}
	if spec.exclusive || spec.removeEmpty {
		t.Fatalf("shared AGENTS.md spec must not be exclusive or removed when empty: %#v", spec)
	}

	foreign := "# Operator instructions\n\nKeep the build green.\n"
	if err := os.MkdirAll(cfg.RuntimeConfigRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(spec.path, []byte(foreign), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := installRuntimeMemoryRoutingInstructions(transcriptcapture.RuntimeDSH); err != nil {
		t.Fatal(err)
	}
	installed := readDSHPatchFile(t, spec.path)
	if !strings.Contains(installed, dshMemoryRoutingBeginMarker) ||
		!strings.Contains(installed, dshMemoryRoutingEndMarker) ||
		!strings.Contains(installed, foreign) {
		t.Fatalf("routing install did not add a fenced block beside foreign text:\n%s", installed)
	}
	current, err := runtimeMemoryRoutingCurrentAt(transcriptcapture.RuntimeDSH, "")
	if err != nil || !current {
		t.Fatalf("routing current=%t err=%v", current, err)
	}
	if _, err := removeRuntimeMemoryRoutingInstructionsAt(transcriptcapture.RuntimeDSH, ""); err != nil {
		t.Fatal(err)
	}
	after := readDSHPatchFile(t, spec.path)
	if strings.Contains(after, dshMemoryRoutingBeginMarker) || after != foreign {
		t.Fatalf("routing removal did not restore the shared file exactly:\n%q", after)
	}
}

func TestDSHComposedConfigRowMatchingIgnoresForeignNeighbours(t *testing.T) {
	for _, test := range []struct {
		name     string
		document string
		want     bool
	}{
		{"exact row", dshComposedFixture(), true},
		{
			"quoted id",
			"plugins:\n  - id: '" + dshPatchRowID + "'\n    name: \"" + dshMCPClientPluginName + "\"\n",
			true,
		},
		{
			"name from another row",
			"plugins:\n  - id: " + dshPatchRowID + "\n  - id: other\n    name: '" + dshMCPClientPluginName + "'\n",
			false,
		},
		{
			"nested row borrowing an ancestor name",
			"plugins:\n  - name: '" + dshMCPClientPluginName + "'\n    config:\n      nested:\n        - id: " + dshPatchRowID + "\n",
			false,
		},
		{"absent", "plugins: []\n", false},
		{
			// The shape `dsh --profile headless --dump-config` actually prints:
			// top-level rows per source layer, introduced by comment headers.
			"real dump layout",
			"# == @deepseek-ai/dsh-base\n- id: timer\n  name: '@deepseek-ai/cordis-plugin-timer'\n" +
				"- id: session-persistence-jsonl\n  name: '@deepseek-ai/dsh-session-persistence-jsonl'\n  config:\n    root: !!js dshHomePath('sessions')\n" +
				"# == /Users/test/.dsh/cordis.patch.yml\n- id: " + dshPatchRowID + "\n  name: '" + dshMCPClientPluginName + "'\n  config:\n    transport: stdio\n",
			true,
		},
		{
			"real dump layout with the row replaced by an override",
			"# == @deepseek-ai/dsh-base\n- id: timer\n  name: '@deepseek-ai/cordis-plugin-timer'\n" +
				"# == /Users/test/.dsh/cordis.patch.yml\n- id: " + dshPatchRowID + "\n  config:\n    transport: stdio\n",
			false,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := dshComposedConfigHasManagedRow([]byte(test.document), dshPatchRowID, dshMCPClientPluginName)
			if got != test.want {
				t.Fatalf("composed row present = %t, want %t", got, test.want)
			}
		})
	}
}

func TestDSHCommandEnvironmentPinsExactConfigRoot(t *testing.T) {
	environment := dshCommandEnvironment(
		[]string{"PATH=/usr/bin", "DSH_HOME=/stale", "dsh_home=/also-stale", "HOME=/home/test"},
		"/exact/dsh",
	)
	found := 0
	for _, entry := range environment {
		key, value, _ := strings.Cut(entry, "=")
		if strings.EqualFold(key, "DSH_HOME") {
			found++
			if key != "DSH_HOME" || value != "/exact/dsh" {
				t.Fatalf("DSH_HOME entry = %q", entry)
			}
		}
	}
	if found != 1 {
		t.Fatalf("environment carries %d DSH_HOME entries: %v", found, environment)
	}
}

func TestDSHOperationLockSerializesOnTheInstalledRoot(t *testing.T) {
	cfg := configuredDSHTestConfig(t)
	if err := os.MkdirAll(cfg.RuntimeConfigRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := dshOperationLockRoot()
	if err != nil {
		t.Fatal(err)
	}
	if root != cfg.RuntimeConfigRoot {
		t.Fatalf("lock root = %q, want %q", root, cfg.RuntimeConfigRoot)
	}
	release, err := acquireDSHOperationLock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireDSHOperationLock(); err == nil {
		t.Fatal("second DeepSeek Harness operation lock was granted")
	}
	release()
	release2, err := acquireDSHOperationLock()
	if err != nil {
		t.Fatalf("lock was not released: %v", err)
	}
	release2()

	// Once a binding is installed, the lock root is the installed config root
	// even when the ambient DSH_HOME now points somewhere else, so two shells
	// with different selectors still serialize on the same integration.
	if err := transcriptcapture.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DSH_HOME", filepath.Join(t.TempDir(), "elsewhere"))
	installedRoot, err := dshOperationLockRoot()
	if err != nil {
		t.Fatal(err)
	}
	if installedRoot != cfg.RuntimeConfigRoot {
		t.Fatalf("installed lock root = %q, want the installed %q", installedRoot, cfg.RuntimeConfigRoot)
	}
}

// The patch file names the command dsh executes, so a group- or world-writable
// file lets any local account rewrite that command. Install must tighten it and
// verification must not call a wide file healthy.
func TestDSHPatchInstallHoldsAnOwnerOnlyPermissionFloor(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not enforced on Windows")
	}
	cfg := configuredDSHTestConfig(t)
	stubDSHDumpConfig(t, dshComposedFixture(), nil)
	if err := os.MkdirAll(cfg.RuntimeConfigRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.RuntimeMCPConfigPath, []byte("[]\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cfg.RuntimeMCPConfigPath, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := installDSHPatchBlock(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := installRuntimeMemoryRoutingInstructions(transcriptcapture.RuntimeDSH); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(cfg.RuntimeMCPConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("patch file permissions after install = %04o, want 0600", info.Mode().Perm())
	}
	if err := validateDSHPersistedTopology(cfg); err != nil {
		t.Fatalf("healthy verification after tightening = %v", err)
	}

	// Widened after install, the same exact block is drift rather than health.
	if err := os.Chmod(cfg.RuntimeMCPConfigPath, 0o666); err != nil {
		t.Fatal(err)
	}
	err = validateDSHPersistedTopology(cfg)
	if err == nil || dshTopologyClass(err) != "" || !strings.Contains(err.Error(), "permissions are 0666") {
		t.Fatalf("widened patch verification = %v, want plain drift naming the mode", err)
	}
	if err := validateDSHInstalledTopology(cfg); err == nil {
		t.Fatal("mcp serve accepted a world-writable patch file")
	}

	// Reinstalling repairs the mode even though the block bytes already match.
	plan, _, err := prepareDSHPatchInstallPlan(cfg.RuntimeCLICommand, cfg, &cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.writeRequired {
		t.Fatal("reinstall over a world-writable patch file planned no write")
	}
	if _, err := installDSHPatchBlockWithPlan(plan); err != nil {
		t.Fatal(err)
	}
	if err := validateDSHPersistedTopology(cfg); err != nil {
		t.Fatalf("verification after mode repair = %v", err)
	}
	if strings.Count(readDSHPatchFile(t, cfg.RuntimeMCPConfigPath), dshPatchBlockBeginMarker) != 1 {
		t.Fatal("mode repair duplicated the managed block")
	}
}

// The operation lock serializes Witself against Witself only; dsh itself and
// the operator's editor are real second writers. A replacement must prove it
// displaced the exact preimage instead of discarding a concurrent edit.
func TestDSHPatchInstallRefusesToDiscardAConcurrentEdit(t *testing.T) {
	cfg := configuredDSHTestConfig(t)
	stubDSHDumpConfig(t, dshComposedFixture(), nil)
	foreign := "- id: existing-row\n  config:\n    retries: 3\n"
	if err := os.MkdirAll(cfg.RuntimeConfigRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.RuntimeMCPConfigPath, []byte(foreign), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := installDSHPatchBlock(cfg); err != nil {
		t.Fatal(err)
	}

	rebound := cfg
	rebound.Agent = "dsh-other-bot"
	rebound.AgentName = "dsh-other-bot"
	plan, _, err := prepareDSHPatchInstallPlan(rebound.RuntimeCLICommand, rebound, &cfg)
	if err != nil {
		t.Fatal(err)
	}
	competing := foreign + "- id: operator-row\n  config:\n    retries: 9\n"
	interposed := false
	previous := dshPatchBeforeMutationForTest
	dshPatchBeforeMutationForTest = func() {
		if interposed {
			return
		}
		interposed = true
		if err := os.WriteFile(cfg.RuntimeMCPConfigPath, []byte(competing), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { dshPatchBeforeMutationForTest = previous })

	touched, err := installDSHPatchBlockWithPlan(plan)
	if err == nil {
		t.Fatal("install overwrote a concurrent edit and reported success")
	}
	if touched {
		t.Fatal("a restored preimage must not be reported as a provider mutation")
	}
	var changed *providerPreflightChangedError
	if !errors.As(err, &changed) {
		t.Fatalf("install error = %v, want a preflight-changed refusal", err)
	}
	if live := readDSHPatchFile(t, cfg.RuntimeMCPConfigPath); live != competing {
		t.Fatalf("the concurrent edit was not preserved:\n%s", live)
	}
	entries, err := os.ReadDir(cfg.RuntimeConfigRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "."+dshPatchFileName+".witself-") {
			t.Fatalf("refusal left a staged temporary behind: %s", entry.Name())
		}
	}
}

// A failing composed probe is the operator's only signal that dsh rejected the
// patch, so the unavailable verdict must carry dsh's own message.
func TestDSHComposedProbeFailureCarriesTheProviderDiagnostic(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fixture is a POSIX shell script")
	}
	cfg := configuredDSHTestConfig(t)
	script := "#!/bin/sh\n" +
		"echo 'patch: cannot parse cordis.patch.yml at line 3' 1>&2\n" +
		"exit 1\n"
	if err := os.WriteFile(cfg.RuntimeCLICommand, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := installDSHPatchBlock(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := installRuntimeMemoryRoutingInstructions(transcriptcapture.RuntimeDSH); err != nil {
		t.Fatal(err)
	}
	err := validateDSHPersistedTopology(cfg)
	if err == nil || dshTopologyClass(err) != integrationVerificationUnavailable {
		t.Fatalf("failing probe verification = %v, want unavailable", err)
	}
	if !strings.Contains(err.Error(), "cannot parse cordis.patch.yml at line 3") {
		t.Fatalf("unavailable verdict dropped the provider diagnostic: %v", err)
	}
}

// The top-level shape refusal must not be bypassed by an already-present
// fence, and verification must re-check it rather than trusting the block.
func TestDSHPatchRefusesAnUnsupportedShapeAroundAnInstalledBlock(t *testing.T) {
	cfg := configuredDSHTestConfig(t)
	stubDSHDumpConfig(t, dshComposedFixture(), nil)
	block, err := dshManagedPatchBlock(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// A mapping followed by top-level sequence entries is not valid YAML at
	// all, even though the fence Witself owns is byte-exact.
	content := "plugins:\n  - id: x\n" + string(block) + "\n"
	if err := os.MkdirAll(cfg.RuntimeConfigRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.RuntimeMCPConfigPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := prepareDSHPatchInstallPlan(cfg.RuntimeCLICommand, cfg, &cfg); err == nil ||
		!strings.Contains(err.Error(), "not a block-style YAML list") {
		t.Fatalf("install plan over an unsupported shape = %v", err)
	}
	if live := readDSHPatchFile(t, cfg.RuntimeMCPConfigPath); live != content {
		t.Fatalf("the refusal rewrote foreign bytes:\n%s", live)
	}
	err = validateDSHPersistedTopology(cfg)
	if err == nil || dshTopologyClass(err) != "" ||
		!strings.Contains(err.Error(), "not a block-style YAML list") {
		t.Fatalf("unsupported-shape verification = %v, want plain drift", err)
	}

	// Uninstall still removes exactly the Witself fence so the operator is
	// never locked into a file Witself refuses to install over.
	if _, err := removeDSHPatchBlock(&cfg); err != nil {
		t.Fatalf("remove the managed block from an unsupported shape: %v", err)
	}
	if live := readDSHPatchFile(t, cfg.RuntimeMCPConfigPath); live != "plugins:\n  - id: x\n" {
		t.Fatalf("removal did not restore the exact foreign bytes:\n%s", live)
	}
}

// Serving must never boot dsh: the session-start latency and any transient
// probe failure would fall on every credential-bound MCP connection.
func TestDSHServeTopologyNeverRunsTheComposedProbe(t *testing.T) {
	cfg := configuredDSHTestConfig(t)
	calls := stubDSHDumpConfig(t, "", os.ErrPermission)
	if _, err := installDSHPatchBlock(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := installRuntimeMemoryRoutingInstructions(transcriptcapture.RuntimeDSH); err != nil {
		t.Fatal(err)
	}
	if err := validateDSHServeTopology(cfg); err != nil {
		t.Fatalf("serve topology = %v, want healthy without a probe", err)
	}
	if len(*calls) != 0 {
		t.Fatalf("serve topology probed dsh: %v", *calls)
	}
	// The owned files still gate serving.
	installed := readDSHPatchFile(t, cfg.RuntimeMCPConfigPath)
	if err := os.WriteFile(cfg.RuntimeMCPConfigPath, []byte(strings.Replace(installed, "transport: stdio", "transport: streamable-http", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateDSHServeTopology(cfg); err == nil || !strings.Contains(err.Error(), "no longer matches") {
		t.Fatalf("serve topology over a tampered block = %v", err)
	}
}

// Install finalization writes and verifies the block itself; a probe that
// cannot run is a warning, while a composed tree missing the row is drift.
func TestDSHCommitTopologyWarnsOnUnavailableProbeButFailsOnDrift(t *testing.T) {
	cfg := configuredDSHTestConfig(t)
	stubDSHDumpConfig(t, "", os.ErrPermission)
	if _, err := installDSHPatchBlock(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := installRuntimeMemoryRoutingInstructions(transcriptcapture.RuntimeDSH); err != nil {
		t.Fatal(err)
	}
	warning, err := validateDSHCommitTopology(cfg)
	if err != nil || !strings.Contains(warning, "could not run") {
		t.Fatalf("commit topology with an unavailable probe = %q, %v; want a warning and no error", warning, err)
	}
	stubDSHDumpConfig(t, "profile: headless\nplugins:\n  - id: other\n    name: '@x/y'\n", nil)
	warning, err = validateDSHCommitTopology(cfg)
	if err == nil || warning != "" || !strings.Contains(err.Error(), "home-level patch is not in effect") {
		t.Fatalf("commit topology over an unmounted row = %q, %v; want drift", warning, err)
	}
	stubDSHDumpConfig(t, dshComposedFixture(), nil)
	if warning, err = validateDSHCommitTopology(cfg); err != nil || warning != "" {
		t.Fatalf("healthy commit topology = %q, %v", warning, err)
	}
}
