package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

const dshForeignHookCanary = "operator-command-canary"
const dshForeignPatchFixture = "# operator bytes\n- insert:\n    - id: operator\n      name: '@operator/plugin'\n      config:\n        value: !!js/function >\n          function () { return 'untouched' }\n"

// Handwritten composed tree, independent of the patch renderer. Like the actual
// provider dump it is a top-level list, with provenance comments and inert tags.
func dshPrivateComposedFixture(t *testing.T, cfg transcriptcapture.Config) string {
	t.Helper()
	quote := func(s string) string {
		q, err := dshQuoteYAMLScalar("fixture", s)
		if err != nil {
			t.Fatal(err)
		}
		return q
	}
	raw := "# == provider\n- id: operator\n  name: '@operator/plugin'\n  config:\n    value: !!js/function 'function () { throw 1 }'\n" +
		"# == home patch\n- id: witself-mcp\n  name: '@deepseek-ai/dsh-mcp-client'\n" +
		"- id: witself-hooks-policy\n  name: '@deepseek-ai/dsh-sandbox-policy'\n  isolate:\n    sandboxPolicy: witself-capture-policy\n    systemPrompt: true\n  config:\n    mode: workspace-write\n    workspaceRoot: " + quote(cfg.MCPEnvironment["WITSELF_HOME"]) + "\n" +
		"- id: witself-hooks-shell\n  name: '@deepseek-ai/dsh-bash-sandbox'\n  isolate:\n    shell: witself-capture-shell\n    sandboxPolicy: witself-capture-policy\n    settings: true\n" +
		"- id: witself-hooks\n  name: '@deepseek-ai/dsh-hooks-claude-code'\n  isolate:\n    shell: witself-capture-shell\n  config:\n    configPath: " + quote(cfg.HookConfigPath) + "\n"
	if cfg.DSHLegacyHookBridge {
		raw += "- id: witself-hooks-legacy\n  name: '@deepseek-ai/dsh-hooks-claude-code'\n  config:\n    configPath: " + quote(filepath.Join(cfg.RuntimeConfigRoot, dshHookConfigFileName)) + "\n"
	}
	return raw
}

func dshAddForeignHook(t *testing.T, path string) {
	t.Helper()
	doc := map[string]any{}
	if raw, err := os.ReadFile(path); err == nil {
		if json.Unmarshal(raw, &doc) != nil {
			t.Fatal("invalid fixture document")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	hooks, _ := doc["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
		doc["hooks"] = hooks
	}
	rows, _ := hooks["Stop"].([]any)
	hooks["Stop"] = append(rows, map[string]any{"matcher": "*", "hooks": []any{map[string]any{"type": "command", "command": dshForeignHookCanary}}})
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func dshInstalledHookFixture(t *testing.T, legacy, foreign bool) transcriptcapture.Config {
	t.Helper()
	cfg := installedDSHTestBinding(t, dshForeignPatchFixture)
	t.Setenv("DSH_HOME", cfg.RuntimeConfigRoot)
	cfg.HookMode = transcriptcapture.HookModeUser
	if legacy {
		cfg.HookConfigPath = filepath.Join(cfg.RuntimeConfigRoot, dshHookConfigFileName)
	} else if err := planRuntimeHooksOwned(&cfg, nil); err != nil {
		t.Fatal(err)
	}
	if foreign {
		dshAddForeignHook(t, filepath.Join(cfg.RuntimeConfigRoot, dshHookConfigFileName))
	}
	if _, _, err := installRuntimeHooksOwned(&cfg, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := installDSHPatchBlock(cfg); err != nil {
		t.Fatal(err)
	}
	if err := transcriptcapture.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestDSHPrivateComposedBoundary(t *testing.T) {
	cfg := configuredDSHTestConfig(t)
	cfg.HookMode = transcriptcapture.HookModeUser
	// Canonical literal quoting, including spaces and apostrophes.
	cfg.RuntimeConfigRoot = filepath.Join(cfg.RuntimeConfigRoot, "root's space")
	cfg.MCPEnvironment["WITSELF_HOME"] = filepath.Join(cfg.MCPEnvironment["WITSELF_HOME"], "store's space")
	if err := planRuntimeHooksOwned(&cfg, nil); err != nil {
		t.Fatal(err)
	}
	raw := dshPrivateComposedFixture(t, cfg)
	if err := validateDSHPrivateComposedConfig([]byte(raw), cfg); err != nil {
		t.Fatal(err)
	}
	fixtureWrapper := "profile: headless\nplugins:\n" + "  " + strings.ReplaceAll(strings.TrimSuffix(raw, "\n"), "\n", "\n  ") + "\n"
	if err := validateDSHPrivateComposedConfig([]byte(fixtureWrapper), cfg); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"dropped-shell-isolation": strings.ReplaceAll(raw, "    shell: witself-capture-shell\n", ""),
		"swapped-labels":          strings.ReplaceAll(raw, "shell: witself-capture-shell", "shell: witself-capture-policy"),
		"missing-system-prompt":   strings.Replace(raw, "    systemPrompt: true\n", "", 1),
		"missing-settings":        strings.Replace(raw, "    settings: true\n", "", 1),
		"mode-override":           strings.Replace(raw, "mode: workspace-write", "mode: danger-full-access", 1),
		"root-override":           strings.Replace(raw, "workspaceRoot: ", "workspaceRoot: canary # ", 1),
		"path-override":           strings.Replace(raw, "configPath: ", "configPath: canary # ", 1),
		"disabled":                strings.Replace(raw, "- id: witself-hooks\n", "- id: witself-hooks\n  disabled: true\n", 1),
		"dynamic-disabled":        strings.Replace(raw, "- id: witself-hooks\n", "- id: witself-hooks\n  disabled: !!js canary\n", 1),
		"quoted-disabled":         strings.Replace(raw, "- id: witself-hooks\n", "- id: witself-hooks\n  disabled: 'false'\n", 1),
		"duplicate-id":            raw + "- id: witself-hooks\n  name: foreign\n",
		"duplicate-key":           strings.Replace(raw, "    mode: workspace-write\n", "    mode: workspace-write\n    mode: canary\n", 1),
		"neighbor-donation":       strings.Replace(raw, "- id: witself-hooks\n  name:", "- id: witself-hooks\n- id: neighbor\n  name:", 1),
		"nested-donation":         strings.Replace(raw, "- id: witself-hooks\n  name:", "- id: witself-hooks\n  config:\n    name:", 1),
		"foreign-config-donation": "- id: foreign\n  name: '@foreign/plugin'\n  config:\n    plugins:\n      " + strings.ReplaceAll(strings.TrimSuffix(raw, "\n"), "\n", "\n      ") + "\n",
		"label-consumer":          raw + "- id: foreign\n  name: foreign\n  isolate: {shell: witself-capture-shell}\n",
		"policy-consumer":         raw + "- id: foreign\n  name: foreign\n  isolate: {sandboxPolicy: witself-capture-policy}\n",
		"ancestor-consumer":       raw + "- id: group\n  name: cordis:group\n  isolate: {shell: witself-capture-shell}\n  config: []\n",
		"nested-consumer":         raw + "- id: group\n  name: cordis:group\n  config:\n  - id: consumer\n    name: foreign\n    isolate: {shell: witself-capture-shell}\n",
		"group-managed-rows":      "- id: group\n  name: cordis:group\n  disabled: true\n  config:\n    " + strings.ReplaceAll(strings.TrimSuffix(raw, "\n"), "\n", "\n    ") + "\n",
		"dynamic-isolation":       strings.Replace(raw, "    shell: witself-capture-shell", "    shell: !!js canary", 1),
		"alias":                   "- id: foreign-anchor\n  name: foreign\n  config: &policy witself-capture-policy\n" + strings.Replace(raw, "sandboxPolicy: witself-capture-policy", "sandboxPolicy: *policy", 1),
		"alias-map":               "- id: foreign-anchor\n  name: foreign\n  config: &policy {sandboxPolicy: witself-capture-policy, systemPrompt: true}\n" + strings.Replace(raw, "  isolate:\n    sandboxPolicy: witself-capture-policy\n    systemPrompt: true", "  isolate: *policy", 1),
		"merge":                   strings.Replace(raw, "- id: witself-hooks\n", "- id: witself-hooks\n  <<: {disabled: true}\n", 1),
		"intercept":               strings.Replace(raw, "- id: witself-hooks\n", "- id: witself-hooks\n  intercept: {shell: canary}\n", 1),
		"extra-document":          raw + "---\n[]\n",
		"malformed-canary":        raw + "[CANARY_SECRET_VALUE\n",
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			err := validateDSHPrivateComposedConfig([]byte(input), cfg)
			if err == nil {
				t.Fatal("unsafe boundary accepted")
			}
			if strings.Contains(err.Error(), "canary") || strings.Contains(err.Error(), "CANARY_SECRET_VALUE") {
				t.Fatal("diagnostic reflected input")
			}
		})
	}
	// Ordinary nested data mentioning labels is not a loader consumer.
	if err := validateDSHPrivateComposedConfig([]byte(raw+"- id: foreign-data\n  name: foreign\n  config: {isolate: {shell: witself-capture-shell}}\n"), cfg); err != nil {
		t.Fatal(err)
	}
	cfg.DSHLegacyHookBridge = true
	compat := dshPrivateComposedFixture(t, cfg)
	if err := validateDSHPrivateComposedConfig([]byte(compat), cfg); err != nil {
		t.Fatal(err)
	}
	if err := validateDSHPrivateComposedConfig([]byte(raw), cfg); err == nil {
		t.Fatal("missing compatibility bridge accepted")
	}
	if err := validateDSHPrivateComposedConfig([]byte(strings.Replace(compat, "- id: witself-hooks-legacy\n", "- id: witself-hooks-legacy\n  isolate: {shell: witself-capture-shell}\n", 1)), cfg); err == nil {
		t.Fatal("private compatibility bridge accepted")
	}
}

func TestDSHPrivateComposedRefusesExecutableIncludes(t *testing.T) {
	cfg := configuredDSHTestConfig(t)
	cfg.HookMode = transcriptcapture.HookModeUser
	if err := planRuntimeHooksOwned(&cfg, nil); err != nil {
		t.Fatal(err)
	}
	raw := dshPrivateComposedFixture(t, cfg)
	// These module paths are synthetic and need not exist: classification must
	// neither load backing files nor import plugins to discover child entries.
	module := filepath.ToSlash(filepath.Join(t.TempDir(), "provider space", "node_modules", "@deepseek-ai", "cordis-plugin-include", "lib", "index.js"))
	fileURL := (&url.URL{Scheme: "file", Path: module}).String()
	names := map[string]string{
		"builtin":                     "cordis:include",
		"package":                     "@deepseek-ai/cordis-plugin-include",
		"absolute":                    module,
		"absolute normalized":         strings.Replace(module, "/lib/index.js", "/lib/../lib/index.js", 1),
		"file URL":                    fileURL,
		"file URL escaped package":    strings.Replace(fileURL, "cordis-plugin-include", "cordis-plugin-%69nclude", 1),
		"file URL normalized":         strings.Replace(fileURL, "/lib/index.js", "/lib/../lib/index.js", 1),
		"file URL query fragment":     fileURL + "?CANARY_SECRET_VALUE#module",
		"relative":                    "./node_modules/@deepseek-ai/cordis-plugin-include/lib/index.js",
		"relative parent":             "../node_modules/@deepseek-ai/cordis-plugin-include/lib/index.js",
		"relative dot directory":      ".modules/@deepseek-ai/cordis-plugin-include/lib/index.js",
		"relative URL normalized":     "../node_modules/@deepseek-ai/cordis-plugin-%69nclude/lib/../lib/index.js?probe#module",
		"unanchored absolute URL":     "/synthetic/node_modules/@deepseek-ai/cordis-plugin-include/lib/index.js?probe#module",
		"unanchored absolute escaped": "/synthetic/node_modules/@deepseek-ai/cordis-plugin-%69nclude/lib/index.js",
	}
	for name, moduleName := range names {
		for _, shape := range []string{"insert", "external", "group", "named-group", "disabled"} {
			t.Run(name+"/"+shape, func(t *testing.T) {
				carrier := fmt.Sprintf("- id: foreign-include\n  name: %q\n", moduleName)
				if shape == "disabled" {
					carrier += "  disabled: true\n"
				}
				carrier += "  config:\n    path: CANARY_SECRET_VALUE.yml\n"
				if shape == "insert" {
					carrier += "    patches:\n    - insert:\n      - id: foreign-consumer\n        name: '@deepseek-ai/dsh-hooks-claude-code'\n        isolate: {shell: witself-capture-shell}\n        config: {configPath: CANARY_SECRET_VALUE.json}\n"
				}
				if shape == "group" {
					carrier = "- id: real-group\n  name: cordis:group\n  group: true\n  config:\n    " + strings.ReplaceAll(strings.TrimSuffix(carrier, "\n"), "\n", "\n    ") + "\n"
				}
				if shape == "named-group" {
					carrier = "- id: real-group\n  name: cordis:group\n  config:\n    " + strings.ReplaceAll(strings.TrimSuffix(carrier, "\n"), "\n", "\n    ") + "\n"
				}
				err := validateDSHPrivateComposedConfig([]byte(raw+carrier), cfg)
				if err == nil {
					t.Fatal("executable Include with unseen children was accepted")
				}
				if strings.Contains(err.Error(), "CANARY_SECRET_VALUE") || strings.Contains(err.Error(), moduleName) {
					t.Fatal("Include refusal reflected input")
				}
			})
		}
	}
	for _, name := range []string{"@operator/plugin", "@deepseek-ai/cordis-plugin-include-data", module + ".data", "./node_modules/@deepseek-ai/cordis-plugin-include-data/lib/index.js", "../node_modules/@deepseek-ai/cordis-plugin-include/lib/index.js.data", "file:///synthetic/@deepseek-ai/cordis-plugin-include/lib/index.js%3Fprobe"} {
		// Identical nested fields, including an Include name, are ordinary plugin
		// data here. Only actual loader carriers participate in this inspection.
		data := fmt.Sprintf("- id: inert-data\n  name: %q\n  config:\n    name: cordis:include\n    patches:\n    - insert:\n      - name: cordis:include\n        isolate: {shell: witself-capture-shell}\n        config: {path: CANARY_SECRET_VALUE.yml}\n", name)
		for _, nested := range []bool{false, true} {
			input := data
			if nested {
				input = "- id: real-group\n  group: true\n  config:\n    " + strings.ReplaceAll(strings.TrimSuffix(data, "\n"), "\n", "\n    ") + "\n"
			}
			if err := validateDSHPrivateComposedConfig([]byte(raw+input), cfg); err != nil {
				t.Fatal("ordinary nested data was treated as an executable Include")
			}
		}
	}
}

func TestDSHMigrationRecoveryEveryCut(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		for _, cut := range []string{"journal", "desired", "legacy", "patch", "config"} {
			t.Run(fmt.Sprintf("mixed=%t/%s", mixed, cut), func(t *testing.T) {
				previous := dshInstalledHookFixture(t, true, mixed)
				desired := previous
				desired.Agent, desired.AgentName = "rebound-bot", "rebound-bot"
				if err := planRuntimeHooksOwned(&desired, &previous); err != nil {
					t.Fatal(err)
				}
				if desired.DSHLegacyHookBridge != mixed {
					t.Fatal("incorrect saved compatibility selection")
				}
				journal, err := beginDSHTransaction(dshTransactionInstall, &previous, &desired)
				if err != nil {
					t.Fatal(err)
				}
				rawJournal, err := os.ReadFile(dshTransactionPath(desired.RuntimeConfigRoot))
				if err != nil {
					t.Fatal(err)
				}
				if bytes.Contains(rawJournal, []byte(dshForeignHookCanary)) {
					t.Fatal("journal copied foreign commands")
				}
				if cut != "journal" {
					desiredOpts, err := userHooksOptionsFromConfig(desired, desired.MCPCommand)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := transcriptcapture.InstallOwnedHooks(desiredOpts, nil); err != nil {
						t.Fatal(err)
					}
				}
				if cut == "legacy" || cut == "patch" || cut == "config" {
					if _, err := removeRuntimeHooksOwned(previous); err != nil {
						t.Fatal(err)
					}
				}
				if cut == "patch" || cut == "config" {
					if _, err := installDSHPatchBlock(desired); err != nil {
						t.Fatal(err)
					}
				}
				if cut == "config" {
					if err := transcriptcapture.SaveConfig(desired); err != nil {
						t.Fatal(err)
					}
				}
				stubDSHDumpConfig(t, dshPrivateComposedFixture(t, desired), nil)
				if err := recoverDSHTransaction(desired.RuntimeConfigRoot); err != nil {
					t.Fatal(err)
				}
				if err := verifyRuntimeHooksOwned(desired); err != nil {
					t.Fatal(err)
				}
				legacy, err := transcriptcapture.InspectDSHLegacyHooks(previous.HookConfigPath, nil)
				if err != nil || legacy != mixed {
					t.Fatalf("legacy cleanup: %v", err)
				}
				if raw, err := os.ReadFile(previous.HookConfigPath); err == nil && bytes.Contains(raw, []byte(" transcript hook --runtime dsh ")) {
					t.Fatal("legacy owned set survived recovery")
				}
				if err := validateDSHServeTopology(desired); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(dshTransactionPath(desired.RuntimeConfigRoot)); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("recovery journal not cleared")
				}
				snapshot, err := readDSHPatchSnapshot(desired.RuntimeMCPConfigPath)
				if err != nil {
					t.Fatal(err)
				}
				if err := validateDSHTransactionNonTarget(journal, snapshot); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestDSHHookMutationFailureAndRollback(t *testing.T) {
	for _, layout := range []string{"first", "legacy", "rebind"} {
		for _, cut := range []string{"desired", "legacy", "patch"} {
			if layout != "legacy" && cut == "legacy" {
				continue
			}
			t.Run(layout+"/"+cut, func(t *testing.T) {
				var previous *transcriptcapture.Config
				desired := configuredDSHTestConfig(t)
				if layout != "first" {
					cfg := dshInstalledHookFixture(t, layout == "legacy", true)
					previous = &cfg
					desired = cfg
				}
				desired.HookMode = transcriptcapture.HookModeUser
				desired.Agent, desired.AgentName = "new-agent", "new-agent"
				if err := planRuntimeHooksOwned(&desired, previous); err != nil {
					t.Fatal(err)
				}
				journal, err := beginDSHTransaction(dshTransactionInstall, previous, &desired)
				if err != nil {
					t.Fatal(err)
				}
				dshAfterHookMutationForTest = func(which string) error {
					if which == cut {
						return errors.New("synthetic hook cut")
					}
					return nil
				}
				t.Cleanup(func() { dshAfterHookMutationForTest = nil })
				_, touched, err := installRuntimeHooksOwned(&desired, previous)
				if !touched {
					t.Fatal("lost touched across hook mutation")
				}
				if cut != "patch" && err == nil {
					t.Fatal("failure cut was not reached")
				}
				if cut == "patch" {
					if err != nil {
						t.Fatal(err)
					}
					if _, err := installDSHPatchBlock(desired); err != nil {
						t.Fatal(err)
					}
				}
				dshAfterHookMutationForTest = nil
				if previous == nil {
					if _, err := removeDSHPatchBlock(&desired); err != nil {
						t.Fatal(err)
					}
				}
				if err := restoreRuntimeHooksOwned(&desired, previous); err != nil {
					t.Fatal(err)
				}
				if previous != nil {
					if _, err := installDSHPatchBlock(*previous); err != nil {
						t.Fatal(err)
					}
					if err := verifyRuntimeHooksOwned(*previous); err != nil {
						t.Fatal(err)
					}
				} else if _, err := os.Lstat(desired.HookConfigPath); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("first-install hooks survived rollback")
				}
				if err := clearDSHTransaction(desired.RuntimeConfigRoot, journal); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestDSHRefusesUnownedDedicatedBeforePublication(t *testing.T) {
	for _, exact := range []bool{false, true} {
		t.Run(fmt.Sprint(exact), func(t *testing.T) {
			cfg := configuredDSHTestConfig(t)
			cfg.HookMode = transcriptcapture.HookModeUser
			if err := planRuntimeHooksOwned(&cfg, nil); err != nil {
				t.Fatal(err)
			}
			if exact {
				opts, err := userHooksOptionsFromConfig(cfg, cfg.MCPCommand)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := transcriptcapture.InstallOwnedHooks(opts, nil); err != nil {
					t.Fatal(err)
				}
			} else {
				dshAddForeignHook(t, cfg.HookConfigPath)
			}
			before, err := os.ReadFile(cfg.HookConfigPath)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := beginDSHTransaction(dshTransactionInstall, nil, &cfg); err == nil {
				t.Fatal("unowned file gained journal authority")
			}
			if _, touched, err := installRuntimeHooksOwned(&cfg, nil); err == nil || touched {
				t.Fatal("unowned file accepted")
			}
			if _, err := os.Lstat(cfg.RuntimeMCPConfigPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("private patch published")
			}
			after, err := os.ReadFile(cfg.HookConfigPath)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("foreign file changed")
			}
		})
	}
}

func TestDSHConcurrentLegacySelectionAndHookDrift(t *testing.T) {
	for _, cut := range []string{"before", "after-desired", "before-patch"} {
		t.Run(cut, func(t *testing.T) {
			previous := dshInstalledHookFixture(t, true, false)
			desired := previous
			if err := planRuntimeHooksOwned(&desired, &previous); err != nil {
				t.Fatal(err)
			}
			if _, err := beginDSHTransaction(dshTransactionInstall, &previous, &desired); err != nil {
				t.Fatal(err)
			}
			oldPatch := readDSHPatchFile(t, previous.RuntimeMCPConfigPath)
			if cut == "before" {
				dshAddForeignHook(t, previous.HookConfigPath)
			}
			if cut == "after-desired" {
				dshAfterHookMutationForTest = func(which string) error {
					if which == "desired" {
						dshAddForeignHook(t, previous.HookConfigPath)
					}
					return nil
				}
				t.Cleanup(func() { dshAfterHookMutationForTest = nil })
			}
			_, touched, err := installRuntimeHooksOwned(&desired, &previous)
			dshAfterHookMutationForTest = nil
			if cut != "before-patch" {
				if err == nil {
					t.Fatal("changed selection accepted")
				}
				if touched != (cut == "after-desired") {
					t.Fatal("incorrect touched after refusal")
				}
				stubDSHDumpConfig(t, dshPrivateComposedFixture(t, desired), nil)
				if err := recoverDSHTransaction(desired.RuntimeConfigRoot); err == nil {
					t.Fatal("recovery discarded changed compatibility choice")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				dshPatchBeforeMutationForTest = func() { dshAddForeignHook(t, desired.HookConfigPath) }
				t.Cleanup(func() { dshPatchBeforeMutationForTest = nil })
				if changed, err := installDSHPatchBlock(desired); err == nil || changed {
					t.Fatal("contaminated hooks activated at publication cut")
				}
				dshPatchBeforeMutationForTest = nil
				if err := restoreRuntimeHooksOwned(&desired, &previous); err == nil {
					t.Fatal("rollback discarded ownership drift")
				}
			}
			if readDSHPatchFile(t, previous.RuntimeMCPConfigPath) != oldPatch {
				t.Fatal("private patch published before refusal")
			}
			if _, err := os.Stat(dshTransactionPath(desired.RuntimeConfigRoot)); err != nil {
				t.Fatal("drift journal lost")
			}
		})
	}
}

func TestDSHMigrationDurabilityClosesBothDocuments(t *testing.T) {
	for _, failLegacy := range []bool{false, true} {
		t.Run(fmt.Sprint(failLegacy), func(t *testing.T) {
			previous := dshInstalledHookFixture(t, true, true)
			desired := previous
			if err := planRuntimeHooksOwned(&desired, &previous); err != nil {
				t.Fatal(err)
			}
			journal, err := beginDSHTransaction(dshTransactionInstall, &previous, &desired)
			if err != nil {
				t.Fatal(err)
			}
			stubDSHDumpConfig(t, dshPrivateComposedFixture(t, desired), nil)
			if err := recoverDSHInstallTransaction(journal); err != nil {
				t.Fatal(err)
			}
			seen := map[string]bool{}
			dshSyncCommittedFileForTest = func(path string) error {
				seen[path] = true
				if failLegacy && path == previous.HookConfigPath {
					return errors.New("synthetic sync failure")
				}
				return nil
			}
			t.Cleanup(func() { dshSyncCommittedFileForTest = nil })
			err = clearDSHTransaction(desired.RuntimeConfigRoot, journal)
			if failLegacy {
				if err == nil {
					t.Fatal("failed sync cleared journal")
				}
				if _, err := os.Stat(dshTransactionPath(desired.RuntimeConfigRoot)); err != nil {
					t.Fatal("sync failure lost journal")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if !seen[previous.HookConfigPath] || !seen[desired.HookConfigPath] {
				t.Fatal("both hook file states must be synchronized")
			}
			dshSyncCommittedFileForTest = nil
		})
	}
}

func TestDSHOldUserHookJournalsKeepLegacyRendering(t *testing.T) {
	for _, empty := range []bool{false, true} {
		for _, operation := range []string{dshTransactionInstall, dshTransactionUninstall} {
			t.Run(fmt.Sprintf("empty=%t/%s", empty, operation), func(t *testing.T) {
				previous := dshInstalledHookFixture(t, true, true)
				desired := previous
				desired.Agent, desired.AgentName = "old-rebind", "old-rebind"
				if empty {
					previous.HookConfigPath = ""
					desired.HookConfigPath = ""
					if err := transcriptcapture.SaveConfig(previous); err != nil {
						t.Fatal(err)
					}
				}
				if err := validateDSHServeTopology(previous); err != nil {
					t.Fatal(err)
				}
				var journal dshTransactionJournal
				var err error
				if operation == dshTransactionInstall {
					journal, err = beginDSHTransaction(operation, &previous, &desired)
				} else {
					journal, err = beginDSHTransaction(operation, &previous, nil)
				}
				if err != nil {
					t.Fatal(err)
				}
				// Serialize the historical empty path exactly, bypassing current hydration.
				if empty {
					journal.Previous.HookConfigPath = ""
					if journal.Desired != nil {
						journal.Desired.HookConfigPath = ""
					}
					raw, err := json.Marshal(journal)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(dshTransactionPath(previous.RuntimeConfigRoot), raw, 0600); err != nil {
						t.Fatal(err)
					}
				}
				block, err := dshManagedPatchBlock(desired)
				if err != nil {
					t.Fatal(err)
				}
				if bytes.Contains(block, []byte(dshHooksPolicyRowID)) || bytes.Contains(block, []byte(transcriptcapture.DSHDedicatedHooksFilename)) {
					t.Fatal("old journal adopted the new layout")
				}
				stubDSHDumpConfig(t, dshComposedFixture()+"  - id: witself-hooks\n    name: '@deepseek-ai/dsh-hooks-claude-code'\n", nil)
				if err := recoverDSHTransaction(previous.RuntimeConfigRoot); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Lstat(filepath.Join(previous.RuntimeConfigRoot, transcriptcapture.DSHDedicatedHooksFilename)); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("old recovery created dedicated hooks")
				}
				if operation == dshTransactionInstall {
					if err := verifyRuntimeHooksOwned(desired); err != nil {
						t.Fatal(err)
					}
					if err := validateDSHServeTopology(desired); err != nil {
						t.Fatal(err)
					}
				} else {
					remain, err := transcriptcapture.InspectDSHLegacyHooks(filepath.Join(previous.RuntimeConfigRoot, dshHookConfigFileName), nil)
					if err != nil || !remain {
						t.Fatal("old uninstall lost foreign hooks")
					}
				}
			})
		}
	}
}

func TestDSHDedicatedRecoveryRebindNoneAndUninstall(t *testing.T) {
	for _, operation := range []string{"rebind", "none", "uninstall"} {
		for _, cut := range []string{"journal", "hooks", "patch"} {
			t.Run(operation+"/"+cut, func(t *testing.T) {
				// Acquire the flag through a real legacy migration, then rebind with live
				// foreign content changed. The saved flag, not that content, controls rows.
				previous := dshInstalledHookFixture(t, true, true)
				migrated := previous
				if err := planRuntimeHooksOwned(&migrated, &previous); err != nil {
					t.Fatal(err)
				}
				if _, _, err := installRuntimeHooksOwned(&migrated, &previous); err != nil {
					t.Fatal(err)
				}
				if _, err := installDSHPatchBlock(migrated); err != nil {
					t.Fatal(err)
				}
				if err := transcriptcapture.SaveConfig(migrated); err != nil {
					t.Fatal(err)
				}
				previous = migrated
				desired := previous
				desired.Agent, desired.AgentName = "second-identity", "second-identity"
				if operation == "none" {
					desired.HookMode = transcriptcapture.HookModeNone
				}
				if err := planRuntimeHooksOwned(&desired, &previous); err != nil {
					t.Fatal(err)
				}
				if (operation != "none") != desired.DSHLegacyHookBridge {
					t.Fatal("flag was not copied or cleared with ownership")
				}
				journalOperation := dshTransactionInstall
				target := &desired
				if operation == "uninstall" {
					journalOperation = dshTransactionUninstall
					target = nil
				}
				if _, err := beginDSHTransaction(journalOperation, &previous, target); err != nil {
					t.Fatal(err)
				}
				if cut != "journal" {
					if operation == "uninstall" {
						if _, err := removeRuntimeHooksOwned(previous); err != nil {
							t.Fatal(err)
						}
					} else if _, _, err := installRuntimeHooksOwned(&desired, &previous); err != nil {
						t.Fatal(err)
					}
				}
				if cut == "patch" {
					if operation == "uninstall" {
						if _, err := removeDSHPatchBlock(&previous); err != nil {
							t.Fatal(err)
						}
					} else if _, err := installDSHPatchBlock(desired); err != nil {
						t.Fatal(err)
					}
				}
				if operation == "rebind" {
					stubDSHDumpConfig(t, dshPrivateComposedFixture(t, desired), nil)
				} else {
					stubDSHDumpConfig(t, dshComposedFixture(), nil)
				}
				if err := recoverDSHTransaction(previous.RuntimeConfigRoot); err != nil {
					t.Fatal(err)
				}
				if operation == "rebind" {
					if err := verifyRuntimeHooksOwned(desired); err != nil {
						t.Fatal(err)
					}
					// Repeat with the operator document now absent. No live re-derivation.
					if err := os.Remove(filepath.Join(previous.RuntimeConfigRoot, dshHookConfigFileName)); err != nil {
						t.Fatal(err)
					}
					repeated := desired
					if err := planRuntimeHooksOwned(&repeated, &desired); err != nil {
						t.Fatal(err)
					}
					if !repeated.DSHLegacyHookBridge {
						t.Fatal("rebind forgot the saved bridge selection")
					}
					if _, touched, err := installRuntimeHooksOwned(&repeated, &desired); err != nil || touched {
						t.Fatalf("repeat install: %v", err)
					}
				} else {
					if _, err := os.Lstat(previous.HookConfigPath); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("removed hooks survived recovery")
					}
					if strings.Contains(readDSHPatchFile(t, previous.RuntimeMCPConfigPath), "id: witself-hooks") {
						t.Fatal("managed hook rows survived removal")
					}
					if remain, err := transcriptcapture.InspectDSHLegacyHooks(filepath.Join(previous.RuntimeConfigRoot, dshHookConfigFileName), nil); err != nil || !remain {
						t.Fatal("ordinary hook content removed")
					}
				}
			})
		}
	}
}

func TestDSHFirstDedicatedRecoveryAndPendingServe(t *testing.T) {
	for _, cut := range []string{"journal", "hooks", "patch", "config"} {
		t.Run(cut, func(t *testing.T) {
			cfg := configuredDSHTestConfig(t)
			cfg.HookMode = transcriptcapture.HookModeUser
			if err := planRuntimeHooksOwned(&cfg, nil); err != nil {
				t.Fatal(err)
			}
			if _, err := beginDSHTransaction(dshTransactionInstall, nil, &cfg); err != nil {
				t.Fatal(err)
			}
			if err := validateDSHServeTopology(cfg); err == nil {
				t.Fatal("serve accepted unpublished installation")
			}
			if cut != "journal" {
				if _, _, err := installRuntimeHooksOwned(&cfg, nil); err != nil {
					t.Fatal(err)
				}
			}
			if cut == "patch" || cut == "config" {
				if _, err := installDSHPatchBlock(cfg); err != nil {
					t.Fatal(err)
				}
			}
			if cut == "config" {
				if err := transcriptcapture.SaveConfig(cfg); err != nil {
					t.Fatal(err)
				}
			}
			stubDSHDumpConfig(t, dshPrivateComposedFixture(t, cfg), nil)
			if err := recoverDSHTransaction(cfg.RuntimeConfigRoot); err != nil {
				t.Fatal(err)
			}
			if err := validateDSHServeTopology(cfg); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDSHLegacyBareHooksMigrationAndMarkerRefusal(t *testing.T) {
	for _, marker := range []bool{false, true} {
		t.Run(fmt.Sprint(marker), func(t *testing.T) {
			previous := dshInstalledHookFixture(t, true, false)
			raw, err := os.ReadFile(previous.HookConfigPath)
			if err != nil {
				t.Fatal(err)
			}
			doc := map[string]json.RawMessage{}
			if json.Unmarshal(raw, &doc) != nil {
				t.Fatal("invalid fixture")
			}
			bare := doc["hooks"]
			if !marker {
				bare = []byte(`{"Stop":[{"hooks":[{"type":"command","command":"operator-command-canary"}]}]}`)
			}
			if err := os.WriteFile(previous.HookConfigPath, bare, 0600); err != nil {
				t.Fatal(err)
			}
			desired := previous
			err = planRuntimeHooksOwned(&desired, &previous)
			if marker {
				if err == nil {
					t.Fatal("marker-bearing bare map was migrated")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !desired.DSHLegacyHookBridge {
				t.Fatal("bare operator handlers lost their bridge")
			}
			if _, _, err := installRuntimeHooksOwned(&desired, &previous); err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(previous.HookConfigPath)
			if err != nil || !bytes.Equal(after, bare) {
				t.Fatal("bare foreign document changed")
			}
		})
	}
}

func TestDSHCommandRollbackAfterHookMutations(t *testing.T) {
	for _, layout := range []string{"first", "legacy"} {
		for _, cut := range []string{"desired", "legacy", "finalize"} {
			if layout == "first" && cut == "legacy" {
				continue
			}
			t.Run(layout+"/"+cut, func(t *testing.T) {
				t.Setenv(installedCommandAcceptanceBinaryEnv, "")
				fixture := setupPortableProviderContract(t, "dsh")
				t.Setenv("DSH_CLI_PATH", fixture.provider.Path)
				t.Setenv("DSH_HOME", filepath.Join(fixture.home, "dsh"))
				args := fixture.installArgs(transcriptcapture.RuntimeDSH)
				var previous *transcriptcapture.Config
				if layout == "legacy" {
					if code := installCmd(args); code != 0 {
						t.Fatal("fixture install failed")
					}
					cfg, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeDSH)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := removeRuntimeHooksOwned(cfg); err != nil {
						t.Fatal(err)
					}
					cfg.HookConfigPath = filepath.Join(cfg.RuntimeConfigRoot, dshHookConfigFileName)
					dshAddForeignHook(t, cfg.HookConfigPath)
					if _, _, err := installRuntimeHooksOwned(&cfg, nil); err != nil {
						t.Fatal(err)
					}
					if _, err := installDSHPatchBlock(cfg); err != nil {
						t.Fatal(err)
					}
					if err := transcriptcapture.SaveConfig(cfg); err != nil {
						t.Fatal(err)
					}
					previous = &cfg
				}
				dshAfterHookMutationForTest = func(which string) error {
					if which == cut {
						return errors.New("synthetic command cut")
					}
					return nil
				}
				t.Cleanup(func() { dshAfterHookMutationForTest = nil })
				originalFinalize := finalizeRuntimeIntegrationConfig
				if cut == "finalize" {
					finalizeRuntimeIntegrationConfig = func(transcriptcapture.Config) error { return errors.New("synthetic finalize cut") }
				}
				t.Cleanup(func() { finalizeRuntimeIntegrationConfig = originalFinalize })
				// Every actual command-level publication must see exclusive ownership.
				dshPatchBeforeMutationForTest = func() {
					if cut != "finalize" {
						return
					}
					cfg, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeDSH)
					if previous != nil {
						cfg = *previous
					}
					cfg.HookMode = transcriptcapture.HookModeUser
					cfg.HookConfigPath = filepath.Join(os.Getenv("DSH_HOME"), transcriptcapture.DSHDedicatedHooksFilename)
					if err == nil && previous == nil {
						if err := verifyRuntimeHooksOwned(cfg); err != nil {
							t.Error("private publication preceded owned hooks")
						}
					}
				}
				t.Cleanup(func() { dshPatchBeforeMutationForTest = nil })
				if code := installCmd(args); code == 0 {
					t.Fatal("injected failure did not fail install")
				}
				dshAfterHookMutationForTest = nil
				dshPatchBeforeMutationForTest = nil
				finalizeRuntimeIntegrationConfig = originalFinalize
				root := os.Getenv("DSH_HOME")
				if _, err := os.Stat(dshTransactionPath(root)); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("completed rollback left journal")
				}
				if previous != nil {
					if err := verifyRuntimeHooksOwned(*previous); err != nil {
						t.Fatal(err)
					}
					if err := validateDSHServeTopology(*previous); err != nil {
						t.Fatal(err)
					}
					remain, err := transcriptcapture.InspectDSHLegacyHooks(previous.HookConfigPath, nil)
					// Still contains exact prior markers: inspection without ownership must refuse.
					if err == nil || remain {
						t.Fatal("rollback did not restore legacy capture handlers")
					}
				} else if _, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeDSH); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("first rollback retained config")
				}
				if _, err := os.Stat(filepath.Join(root, transcriptcapture.DSHDedicatedHooksFilename)); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("attempted dedicated document survived rollback")
				}
			})
		}
	}
}

func TestDSHUninstallRollbackRequiresHooksBeforeActivation(t *testing.T) {
	cfg := dshInstalledHookFixture(t, false, false)
	journal, err := beginDSHTransaction(dshTransactionUninstall, &cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := removeDSHPatchBlock(&cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := removeRuntimeHooksOwned(cfg); err != nil {
		t.Fatal(err)
	}
	if changed, err := installDSHPatchBlock(cfg); err == nil || changed {
		t.Fatal("rollback reactivated bridge before hooks")
	}
	if err := restoreRuntimeHooksOwned(nil, &cfg); err != nil {
		t.Fatal(err)
	}
	dshPatchBeforeMutationForTest = func() { dshAddForeignHook(t, cfg.HookConfigPath) }
	t.Cleanup(func() { dshPatchBeforeMutationForTest = nil })
	if changed, err := installDSHPatchBlock(cfg); err == nil || changed {
		t.Fatal("rollback published contaminated private bridge")
	}
	dshPatchBeforeMutationForTest = nil
	snapshot, err := readDSHPatchSnapshot(cfg.RuntimeMCPConfigPath)
	if err != nil || snapshot.blockPresent() {
		t.Fatal("unsafe rollback activated a bridge")
	}
	if err := recoverDSHUninstallTransaction(journal); err == nil {
		t.Fatal("recovery discarded contaminated hook document")
	}
	if _, err := os.Stat(dshTransactionPath(cfg.RuntimeConfigRoot)); err != nil {
		t.Fatal("rollback lost journal")
	}
}

func TestDSHBareOperatorRollbackPreservesExecutionAndJournal(t *testing.T) {
	previous := dshInstalledHookFixture(t, true, false)
	bare := []byte(`{"Stop":[{"hooks":[{"type":"command","command":"operator-command-canary"}]}]}`)
	if err := os.WriteFile(previous.HookConfigPath, bare, 0600); err != nil {
		t.Fatal(err)
	}
	desired := previous
	if err := planRuntimeHooksOwned(&desired, &previous); err != nil {
		t.Fatal(err)
	}
	if _, err := beginDSHTransaction(dshTransactionInstall, &previous, &desired); err != nil {
		t.Fatal(err)
	}
	if _, _, err := installRuntimeHooksOwned(&desired, &previous); err != nil {
		t.Fatal(err)
	}
	if _, err := installDSHPatchBlock(desired); err != nil {
		t.Fatal(err)
	}
	if err := restoreRuntimeHooksOwned(&desired, &previous); err == nil {
		t.Fatal("unsafe rollback wrapped and masked bare operator hooks")
	}
	raw, err := os.ReadFile(previous.HookConfigPath)
	if err != nil || !bytes.Equal(raw, bare) {
		t.Fatal("rollback changed ordinary hook execution")
	}
	if err := verifyRuntimeHooksOwned(desired); err != nil {
		t.Fatal("refused rollback removed active capture")
	}
	if _, err := os.Stat(dshTransactionPath(desired.RuntimeConfigRoot)); err != nil {
		t.Fatal("refused rollback discarded journal")
	}
	stubDSHDumpConfig(t, dshPrivateComposedFixture(t, desired), nil)
	if err := recoverDSHTransaction(desired.RuntimeConfigRoot); err != nil {
		t.Fatal(err)
	}
}

func TestDSHPrivateDiagnosticsAndTraversalLimits(t *testing.T) {
	cfg := configuredDSHTestConfig(t)
	cfg.HookMode = transcriptcapture.HookModeUser
	if err := planRuntimeHooksOwned(&cfg, nil); err != nil {
		t.Fatal(err)
	}
	stubDSHDumpConfig(t, "", errors.New("CANARY_SUBMITTED_VALUE"))
	err := validateDSHComposedConfig(cfg)
	if err == nil || dshTopologyClass(err) != integrationVerificationUnavailable || strings.Contains(err.Error(), "CANARY_SUBMITTED_VALUE") {
		t.Fatal("provider failure reflected content or reported health")
	}
	for _, raw := range []string{strings.Repeat(" ", dshCLIOutputLimit+1), strings.Repeat("[", 100) + "0" + strings.Repeat("]", 100)} {
		if _, err := dshDumpNode([]byte(raw)); err == nil {
			t.Fatal("unbounded composed document accepted")
		}
	}
	if err := os.MkdirAll(cfg.RuntimeConfigRoot, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.RuntimeMCPConfigPath, []byte(dshPatchBlockBeginPrefix+" CANARY_SUBMITTED_VALUE\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = readDSHPatchSnapshot(cfg.RuntimeMCPConfigPath)
	if err == nil || strings.Contains(err.Error(), "CANARY_SUBMITTED_VALUE") {
		t.Fatal("patch refusal reflected content")
	}
}

func TestDSHBridgeCapabilityTransitionValidation(t *testing.T) {
	cfg := configuredDSHTestConfig(t)
	cfg.HookMode = transcriptcapture.HookModeUser
	if err := planRuntimeHooksOwned(&cfg, nil); err != nil {
		t.Fatal(err)
	}
	cfg.DSHLegacyHookBridge = true
	if _, err := beginDSHTransaction(dshTransactionInstall, nil, &cfg); err == nil {
		t.Fatal("fresh install acquired compatibility capability")
	}
	previous := cfg
	previous.DSHLegacyHookBridge = false
	if err := validateDSHBridgeTransition(&previous, &cfg); err == nil {
		t.Fatal("rebind acquired compatibility capability")
	}
}

func TestDSHLegacySelectionRecheckedAtPrivatePublication(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		t.Run(fmt.Sprint(mixed), func(t *testing.T) {
			previous := dshInstalledHookFixture(t, true, mixed)
			desired := previous
			if err := planRuntimeHooksOwned(&desired, &previous); err != nil {
				t.Fatal(err)
			}
			if _, err := beginDSHTransaction(dshTransactionInstall, &previous, &desired); err != nil {
				t.Fatal(err)
			}
			plan, _, err := prepareDSHPatchInstallPlan(desired.RuntimeCLICommand, desired, &previous)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := installRuntimeHooksOwned(&desired, &previous); err != nil {
				t.Fatal(err)
			}
			oldPatch := readDSHPatchFile(t, previous.RuntimeMCPConfigPath)
			dshPatchBeforeMutationForTest = func() {
				if mixed {
					if err := os.Remove(previous.HookConfigPath); err != nil {
						t.Fatal(err)
					}
				} else {
					dshAddForeignHook(t, previous.HookConfigPath)
				}
			}
			t.Cleanup(func() { dshPatchBeforeMutationForTest = nil })
			if changed, err := installDSHPatchBlockWithPlan(plan); err == nil || changed {
				t.Fatal("changed compatibility selection was published")
			}
			dshPatchBeforeMutationForTest = nil
			if readDSHPatchFile(t, previous.RuntimeMCPConfigPath) != oldPatch {
				t.Fatal("refusal changed active patch")
			}
			if _, err := os.Stat(dshTransactionPath(desired.RuntimeConfigRoot)); err != nil {
				t.Fatal("publication cut lost journal")
			}
		})
	}
}

func TestDSHLegacySamePathRollbackRetainsExactReplacement(t *testing.T) {
	previous := dshInstalledHookFixture(t, true, true)
	attempted := previous
	attempted.Agent, attempted.AgentName = "legacy-rebind", "legacy-rebind"
	if _, _, err := installRuntimeHooksOwned(&attempted, &previous); err != nil {
		t.Fatal(err)
	}
	if err := restoreRuntimeHooksOwned(&attempted, &previous); err != nil {
		t.Fatal(err)
	}
	if err := verifyRuntimeHooksOwned(previous); err != nil {
		t.Fatal(err)
	}
}

// Exercise the actual command rollback, with the operator edit scheduled after
// the early guard and old-binding verification but before InstallOwnedHooks.
func TestDSHCommandRollbackRefusesConcurrentBareHooks(t *testing.T) {
	t.Setenv(installedCommandAcceptanceBinaryEnv, "")
	fixture := setupPortableProviderContract(t, "dsh")
	t.Setenv("DSH_CLI_PATH", fixture.provider.Path)
	t.Setenv("DSH_HOME", filepath.Join(fixture.home, "dsh"))
	args := fixture.installArgs(transcriptcapture.RuntimeDSH)
	if code := installCmd(args); code != 0 {
		t.Fatal("fixture install failed")
	}
	previous, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeDSH)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := removeRuntimeHooksOwned(previous); err != nil {
		t.Fatal(err)
	}
	previous.HookConfigPath = filepath.Join(previous.RuntimeConfigRoot, dshHookConfigFileName)
	dshAddForeignHook(t, previous.HookConfigPath)
	if _, _, err := installRuntimeHooksOwned(&previous, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := installDSHPatchBlock(previous); err != nil {
		t.Fatal(err)
	}
	if err := transcriptcapture.SaveConfig(previous); err != nil {
		t.Fatal(err)
	}
	bare := []byte("{\n  \"Stop\": [{\"hooks\": [{\"type\": \"command\", \"command\": \"operator-command-canary\"}]}]\n}\n")
	var operatorInfo os.FileInfo
	calls := 0
	dshBeforeHookRestoreForTest = func() {
		calls++
		if err := os.WriteFile(previous.HookConfigPath, bare, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(previous.HookConfigPath, 0640); err != nil {
			t.Fatal(err)
		}
		operatorInfo, err = os.Stat(previous.HookConfigPath)
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { dshBeforeHookRestoreForTest = nil })
	originalFinalize := finalizeRuntimeIntegrationConfig
	var journalBefore []byte
	finalizeRuntimeIntegrationConfig = func(transcriptcapture.Config) error {
		journalBefore, err = os.ReadFile(dshTransactionPath(previous.RuntimeConfigRoot))
		if err != nil {
			t.Fatal(err)
		}
		return errors.New("synthetic finalize cut")
	}
	t.Cleanup(func() { finalizeRuntimeIntegrationConfig = originalFinalize })
	if code := installCmd(args); code == 0 {
		t.Fatal("injected failure did not fail install")
	}
	dshBeforeHookRestoreForTest = nil
	finalizeRuntimeIntegrationConfig = originalFinalize
	if calls != 1 {
		t.Fatal("restore mutation cut was not reached exactly once")
	}
	after, err := os.ReadFile(previous.HookConfigPath)
	if err != nil || !bytes.Equal(after, bare) {
		t.Fatal("rollback wrapped or changed the new bare operator document")
	}
	info, err := os.Stat(previous.HookConfigPath)
	if err != nil || info.Mode() != operatorInfo.Mode() || !os.SameFile(info, operatorInfo) {
		t.Fatal("rollback changed operator file mode or identity")
	}
	journalAfter, err := os.ReadFile(dshTransactionPath(previous.RuntimeConfigRoot))
	if err != nil || !bytes.Equal(journalBefore, journalAfter) {
		t.Fatal("late rollback refusal lost recovery state")
	}
	if _, err := os.Stat(filepath.Join(previous.RuntimeConfigRoot, transcriptcapture.DSHDedicatedHooksFilename)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("regression did not reach the post-removal restore cut")
	}
	if err := recoverDSHTransaction(previous.RuntimeConfigRoot); err != nil {
		t.Fatal("retained journal could not recover forward")
	}
	after, err = os.ReadFile(previous.HookConfigPath)
	if err != nil || !bytes.Equal(after, bare) {
		t.Fatal("forward recovery changed bare operator bytes")
	}
}
