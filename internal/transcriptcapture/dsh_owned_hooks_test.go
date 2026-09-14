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

func dshOwnedHooksTestOptions(t *testing.T, filename string) UserHooksOptions {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DSH_HOME", filepath.Join(home, "dsh"))
	t.Setenv("WITSELF_HOME", filepath.Join(home, "witself"))
	opts, err := DefaultUserHooksOptions(RuntimeDSH, ModeRaw, filepath.Join(home, "bin", "witself"),
		"account", "realm", "agent", "home", os.Getenv("WITSELF_HOME"))
	if err != nil {
		t.Fatal(err)
	}
	if opts.ConfigPath != filepath.Join(os.Getenv("DSH_HOME"), "hooks.json") {
		t.Fatal("default DSH hook path changed")
	}
	opts.ConfigPath = filepath.Join(os.Getenv("DSH_HOME"), filename)
	return opts
}

func readDSHHookTestDocument(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}
	return root
}

func writeDSHHookTestDocument(t *testing.T, path string, root map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestOwnedDSHDedicatedHooksLifecycle(t *testing.T) {
	opts := dshOwnedHooksTestOptions(t, DSHDedicatedHooksFilename)
	if mutation, err := InstallOwnedHooks(opts, nil); err != nil || !mutation.Touched || mutation.Path != opts.ConfigPath {
		t.Fatalf("first install = %+v, %v", mutation, err)
	}
	if err := VerifyOwnedHooks(opts); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(opts.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if goruntime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatal("dedicated hook file is not owner-only")
	}
	before, err := os.ReadFile(opts.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := InstallOwnedHooks(opts, nil); err == nil || !strings.Contains(err.Error(), "without a durable Witself ownership record") {
		t.Fatalf("unclaimed exact file install error = %v", err)
	}
	assertHookSecurityFileBytes(t, opts.ConfigPath, before)
	if mutation, err := InstallOwnedHooks(opts, &opts); err != nil || mutation.Touched {
		t.Fatalf("idempotent reinstall = %+v, %v", mutation, err)
	}
	assertHookSecurityFileBytes(t, opts.ConfigPath, before)

	desired := opts
	desired.Account, desired.Realm, desired.Agent, desired.Location = "next-account", "next-realm", "next-agent", "office"
	desired.Executable = filepath.Join(filepath.Dir(opts.Executable), "witself-next")
	desired.WitselfHome = filepath.Join(t.TempDir(), "next-witself")
	desired.Mode = ModeMessages
	if mutation, err := InstallOwnedHooks(desired, &opts); err != nil || !mutation.Touched {
		t.Fatalf("durable rebind = %+v, %v", mutation, err)
	}
	if err := VerifyOwnedHooks(desired); err != nil {
		t.Fatal(err)
	}
	if err := VerifyOwnedHooks(opts); err == nil {
		t.Fatal("old binding verified after rebind")
	}
	if mutation, err := InstallOwnedHooks(desired, &desired); err != nil || mutation.Touched {
		t.Fatalf("rebound reinstall = %+v, %v", mutation, err)
	}
	if mutation, err := RemoveOwnedHooks(desired); err != nil || !mutation.Touched {
		t.Fatalf("remove = %+v, %v", mutation, err)
	}
	if _, err := os.Lstat(desired.ConfigPath); !os.IsNotExist(err) {
		t.Fatalf("dedicated file remains: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(desired.ConfigPath)); err != nil {
		t.Fatal("removal removed DSH_HOME")
	}
	if mutation, err := RemoveOwnedHooks(desired); err != nil || mutation.Touched {
		t.Fatalf("absent removal = %+v, %v", mutation, err)
	}
	if err := VerifyOwnedHooks(desired); err == nil {
		t.Fatal("absent file verified")
	}
	if mutation, err := InstallOwnedHooks(desired, &opts); err != nil || !mutation.Touched {
		t.Fatalf("absent prior document recovery = %+v, %v", mutation, err)
	}
}

func TestOwnedDSHDedicatedHooksRejectMismatchedBinding(t *testing.T) {
	for name, mutate := range map[string]func(*UserHooksOptions){
		"account":    func(o *UserHooksOptions) { o.Account = "different" },
		"realm":      func(o *UserHooksOptions) { o.Realm = "different" },
		"agent":      func(o *UserHooksOptions) { o.Agent = "different" },
		"location":   func(o *UserHooksOptions) { o.Location = "different" },
		"home":       func(o *UserHooksOptions) { o.WitselfHome = filepath.Join(t.TempDir(), "different") },
		"executable": func(o *UserHooksOptions) { o.Executable = filepath.Join(t.TempDir(), "different") },
	} {
		t.Run(name, func(t *testing.T) {
			opts := dshOwnedHooksTestOptions(t, DSHDedicatedHooksFilename)
			if _, err := InstallOwnedHooks(opts, nil); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(opts.ConfigPath)
			if err != nil {
				t.Fatal(err)
			}
			other := opts
			mutate(&other)
			if _, err := InstallOwnedHooks(opts, &other); err == nil {
				t.Fatal("mismatched same-path prior binding accepted")
			}
			if err := VerifyOwnedHooks(other); err == nil {
				t.Fatal("mismatched binding verified")
			}
			if _, err := RemoveOwnedHooks(other); err == nil {
				t.Fatal("mismatched binding removed")
			}
			assertHookSecurityFileBytes(t, opts.ConfigPath, before)
		})
	}
}

const dshHookErrorCanary = "PRIVATE-HOOK-DOCUMENT-CANARY"

func dshOperatorHookTestEntry() map[string]any {
	return map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "operator-command " + dshHookErrorCanary}}}
}

func TestOwnedDSHDedicatedHooksRefuseDocumentDrift(t *testing.T) {
	for name, mutate := range map[string]func(map[string]any){
		"root field": func(root map[string]any) { root[dshHookErrorCanary] = "private" },
		"operator command": func(root map[string]any) {
			hooks := root["hooks"].(map[string]any)
			hooks["Stop"] = append(hooks["Stop"].([]any), dshOperatorHookTestEntry())
		},
		"extra event": func(root map[string]any) { root["hooks"].(map[string]any)[dshHookErrorCanary] = []any{} },
		"partial set": func(root map[string]any) { delete(root["hooks"].(map[string]any), "Stop") },
		"absent set":  func(root map[string]any) { root["hooks"] = map[string]any{} },
		"duplicates": func(root map[string]any) {
			hooks := root["hooks"].(map[string]any)
			hooks["Stop"] = append(hooks["Stop"].([]any), hooks["Stop"].([]any)[0])
		},
		"matcher": func(root map[string]any) {
			root["hooks"].(map[string]any)["PreToolUse"].([]any)[0].(map[string]any)["matcher"] = dshHookErrorCanary
		},
		"timeout": func(root map[string]any) {
			root["hooks"].(map[string]any)["Stop"].([]any)[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)["timeout"] = 99
		},
		"changed command": func(root map[string]any) {
			root["hooks"].(map[string]any)["Stop"].([]any)[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)["command"] = dshHookErrorCanary
		},
		"mixed command group": func(root map[string]any) {
			group := root["hooks"].(map[string]any)["Stop"].([]any)[0].(map[string]any)
			group["hooks"] = append(group["hooks"].([]any), map[string]any{"command": dshHookErrorCanary})
		},
	} {
		t.Run(name, func(t *testing.T) {
			opts := dshOwnedHooksTestOptions(t, DSHDedicatedHooksFilename)
			if _, err := InstallOwnedHooks(opts, nil); err != nil {
				t.Fatal(err)
			}
			root := readDSHHookTestDocument(t, opts.ConfigPath)
			mutate(root)
			raw := writeDSHHookTestDocument(t, opts.ConfigPath, root)
			assertDSHDedicatedHooksRefused(t, opts, raw)
		})
	}
}

func assertDSHDedicatedHooksRefused(t *testing.T, opts UserHooksOptions, raw []byte) {
	t.Helper()
	for _, operation := range []func() error{
		func() error { _, err := InstallOwnedHooks(opts, nil); return err },
		func() error { _, err := InstallOwnedHooks(opts, &opts); return err },
		func() error { return VerifyOwnedHooks(opts) },
		func() error { _, err := RemoveOwnedHooks(opts); return err },
	} {
		if err := operation(); err == nil || strings.Contains(err.Error(), dshHookErrorCanary) || strings.Contains(err.Error(), "Grok") {
			t.Fatalf("expected value-free DSH refusal, got %v", err)
		}
		assertHookSecurityFileBytes(t, opts.ConfigPath, raw)
	}
}

func TestOwnedDSHHooksKeepSharedDocuments(t *testing.T) {
	for _, test := range []struct{ runtime, filename string }{
		{RuntimeDSH, "hooks.json"},
		{RuntimeDSH, "prefix-witself-hooks.json"},
		{RuntimeDSH, "witself-hooks.json.bak"},
		{RuntimeDSH, "Witself-hooks.json"},
		{RuntimeClaudeCode, DSHDedicatedHooksFilename},
	} {
		t.Run(test.runtime+"/"+test.filename, func(t *testing.T) {
			opts := dshOwnedHooksTestOptions(t, test.filename)
			opts.Runtime = test.runtime
			original := map[string]any{"metadata": "preserve", "hooks": map[string]any{"Stop": []any{dshOperatorHookTestEntry()}}}
			writeDSHHookTestDocument(t, opts.ConfigPath, original)
			if _, err := InstallOwnedHooks(opts, nil); err != nil {
				t.Fatal(err)
			}
			if err := VerifyOwnedHooks(opts); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(opts.ConfigPath)
			if err != nil {
				t.Fatal(err)
			}
			if test.runtime == RuntimeDSH {
				remaining, err := InspectDSHLegacyHooks(opts.ConfigPath, &opts)
				if err != nil || !remaining {
					t.Fatalf("mixed document inspection = %v, %v", remaining, err)
				}
				assertHookSecurityFileBytes(t, opts.ConfigPath, before)
			}
			if _, err := RemoveOwnedHooks(opts); err != nil {
				t.Fatal(err)
			}
			if !hookJSONEquivalent(readDSHHookTestDocument(t, opts.ConfigPath), original) {
				t.Fatal("shared operator document was not preserved")
			}
			if test.runtime == RuntimeDSH {
				for _, prior := range []*UserHooksOptions{nil, &opts} {
					remaining, err := InspectDSHLegacyHooks(opts.ConfigPath, prior)
					if err != nil || !remaining {
						t.Fatalf("operator-only inspection = %v, %v", remaining, err)
					}
				}
			}
		})
	}
}

func TestInspectDSHLegacyHooksEmptyAndOwnedStates(t *testing.T) {
	opts := dshOwnedHooksTestOptions(t, "hooks.json")
	for _, prior := range []*UserHooksOptions{nil, &opts} {
		if remaining, err := InspectDSHLegacyHooks(opts.ConfigPath, prior); err != nil || remaining {
			t.Fatalf("missing document inspection = %v, %v", remaining, err)
		}
	}
	for _, root := range []map[string]any{
		{}, {"hooks": map[string]any{}}, {"metadata": dshHookErrorCanary},
		{"hooks": map[string]any{"Stop": []any{}}},
	} {
		raw := writeDSHHookTestDocument(t, opts.ConfigPath, root)
		for _, prior := range []*UserHooksOptions{nil, &opts} {
			if remaining, err := InspectDSHLegacyHooks(opts.ConfigPath, prior); err != nil || remaining {
				t.Fatalf("empty document inspection = %v, %v", remaining, err)
			}
			assertHookSecurityFileBytes(t, opts.ConfigPath, raw)
		}
	}
	if _, err := InstallOwnedHooks(opts, nil); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(opts.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	// Normalize the exact claim without mutating the caller's options.
	prior := opts
	prior.Runtime, prior.Mode, prior.Account = " deepseek ", " RAW ", " account "
	prior.ConfigPath = " " + opts.ConfigPath + " "
	if remaining, err := InspectDSHLegacyHooks(" "+opts.ConfigPath+" ", &prior); err != nil || remaining {
		t.Fatalf("owned-only inspection = %v, %v", remaining, err)
	}
	if prior.Runtime != " deepseek " || prior.Account != " account " {
		t.Fatal("inspection mutated the caller's claim")
	}
	assertHookSecurityFileBytes(t, opts.ConfigPath, raw)
	if _, err := RemoveOwnedHooks(opts); err != nil {
		t.Fatal(err)
	}
	if remaining, err := InspectDSHLegacyHooks(opts.ConfigPath, &opts); err != nil || remaining {
		t.Fatalf("removed owned-only inspection = %v, %v", remaining, err)
	}
}

func assertDSHLegacyHookTestBytes(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatal("legacy hook document bytes were not preserved")
	}
}

func TestInspectDSHLegacyHooksBareForeignEvents(t *testing.T) {
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "Stop", "SubagentStart", "SubagentStop"} {
		t.Run(event, func(t *testing.T) {
			opts := dshOwnedHooksTestOptions(t, "hooks.json")
			raw := writeDSHHookTestDocument(t, opts.ConfigPath, map[string]any{
				event: []any{dshOperatorHookTestEntry()}, "metadata": dshHookErrorCanary,
			})
			for _, prior := range []*UserHooksOptions{nil, &opts} {
				remaining, err := InspectDSHLegacyHooks(opts.ConfigPath, prior)
				if err != nil || !remaining {
					t.Fatal("bare operator event must remain without adopting a claim")
				}
				assertDSHLegacyHookTestBytes(t, opts.ConfigPath, raw)
			}
		})
	}
}

func TestInspectDSHLegacyHooksBareMetadataAndEmptyEvents(t *testing.T) {
	for name, root := range map[string]map[string]any{
		"metadata scalar": {"metadata": dshHookErrorCanary},
		"metadata array":  {"metadata": []any{dshOperatorHookTestEntry()}},
		"unsupported event": {"SessionEnd": []any{map[string]any{
			"command": "operator" + hookCommandMarker + "dsh " + dshHookErrorCanary,
		}}},
		"empty events": {
			"SessionStart": []any{}, "UserPromptSubmit": []any{}, "PreToolUse": []any{},
			"PostToolUse": []any{}, "Stop": []any{}, "SubagentStart": []any{}, "SubagentStop": []any{},
		},
	} {
		t.Run(name, func(t *testing.T) {
			opts := dshOwnedHooksTestOptions(t, "hooks.json")
			raw := writeDSHHookTestDocument(t, opts.ConfigPath, root)
			for _, prior := range []*UserHooksOptions{nil, &opts} {
				if remaining, err := InspectDSHLegacyHooks(opts.ConfigPath, prior); err != nil || remaining {
					t.Fatal("bare metadata or empty events must not require a compatibility bridge")
				}
				assertDSHLegacyHookTestBytes(t, opts.ConfigPath, raw)
			}
		})
	}
}

func TestInspectDSHLegacyHooksWrappedPrecedence(t *testing.T) {
	for _, name := range []string{"empty", "operator", "exact prior"} {
		t.Run(name, func(t *testing.T) {
			opts := dshOwnedHooksTestOptions(t, "hooks.json")
			hooks := map[string]any{}
			if name == "operator" {
				hooks["Stop"] = []any{dshOperatorHookTestEntry()}
			} else if name == "exact prior" {
				if err := addExactOwnedHookSet(hooks, opts); err != nil {
					t.Fatal("could not construct exact prior hook set")
				}
			}
			raw := writeDSHHookTestDocument(t, opts.ConfigPath, map[string]any{
				"hooks": hooks,
				"Stop":  []any{dshOperatorHookTestEntry()},
				"SubagentStop": []any{map[string]any{
					"command": "operator" + hookCommandMarker + "dsh " + dshHookErrorCanary,
				}},
			})
			for _, prior := range []*UserHooksOptions{nil, &opts} {
				remaining, err := InspectDSHLegacyHooks(opts.ConfigPath, prior)
				if name == "exact prior" && prior == nil {
					if err == nil || remaining || strings.Contains(err.Error(), dshHookErrorCanary) {
						t.Fatal("unclaimed wrapped handlers must receive a value-free refusal")
					}
				} else if err != nil || remaining != (name == "operator") {
					t.Fatal("wrapped hooks must take precedence over bare root events")
				}
				assertDSHLegacyHookTestBytes(t, opts.ConfigPath, raw)
			}
		})
	}
}

func TestInspectDSHLegacyHooksBareMarkerRefusals(t *testing.T) {
	for _, name := range []string{"exact prior", "mixed prior", "unclaimed", "late subagent marker", "windows marker"} {
		t.Run(name, func(t *testing.T) {
			opts := dshOwnedHooksTestOptions(t, "hooks.json")
			opts.Agent = dshHookErrorCanary
			root := map[string]any{}
			if name == "late subagent marker" || name == "windows marker" {
				root["SessionStart"] = []any{dshOperatorHookTestEntry()}
				field := "command"
				if name == "windows marker" {
					field = "commandWindows"
				}
				root["SubagentStop"] = []any{map[string]any{"hooks": []any{map[string]any{
					field: "operator" + hookCommandMarker + "dsh " + dshHookErrorCanary,
				}}}}
			} else {
				if err := addExactOwnedHookSet(root, opts); err != nil {
					t.Fatal("could not construct exact prior hook set")
				}
				if name == "mixed prior" {
					root["Stop"] = append(root["Stop"].([]any), dshOperatorHookTestEntry())
				}
			}
			raw := writeDSHHookTestDocument(t, opts.ConfigPath, root)
			claim := &opts
			if name == "unclaimed" {
				claim = nil
			}
			remaining, err := InspectDSHLegacyHooks(opts.ConfigPath, claim)
			if err == nil || remaining || strings.Contains(err.Error(), dshHookErrorCanary) {
				t.Fatal("bare marker handlers must receive a value-free refusal even with an exact claim")
			}
			assertDSHLegacyHookTestBytes(t, opts.ConfigPath, raw)
		})
	}
}

func TestInspectDSHLegacyHooksBareMalformedEventsRefusal(t *testing.T) {
	for name, value := range map[string]any{"null": nil, "string": dshHookErrorCanary, "object": dshOperatorHookTestEntry()} {
		t.Run(name, func(t *testing.T) {
			opts := dshOwnedHooksTestOptions(t, "hooks.json")
			raw := writeDSHHookTestDocument(t, opts.ConfigPath, map[string]any{
				"SessionStart": []any{dshOperatorHookTestEntry()}, "SubagentStop": value,
			})
			for _, prior := range []*UserHooksOptions{nil, &opts} {
				remaining, err := InspectDSHLegacyHooks(opts.ConfigPath, prior)
				if err == nil || remaining || strings.Contains(err.Error(), dshHookErrorCanary) {
					t.Fatal("malformed bare events must receive a value-free refusal")
				}
				assertDSHLegacyHookTestBytes(t, opts.ConfigPath, raw)
			}
		})
	}
}

func TestInspectDSHLegacyHooksPresentMalformedHooksRefusal(t *testing.T) {
	for name, hooks := range map[string]any{
		"null": nil, "string": dshHookErrorCanary, "array": []any{dshOperatorHookTestEntry()},
		"number": 1, "boolean": false,
	} {
		t.Run(name, func(t *testing.T) {
			opts := dshOwnedHooksTestOptions(t, "hooks.json")
			raw := writeDSHHookTestDocument(t, opts.ConfigPath, map[string]any{
				"hooks": hooks, "Stop": []any{dshOperatorHookTestEntry()},
			})
			for _, prior := range []*UserHooksOptions{nil, &opts} {
				remaining, err := InspectDSHLegacyHooks(opts.ConfigPath, prior)
				if err == nil || remaining || strings.Contains(err.Error(), dshHookErrorCanary) {
					t.Fatal("present malformed hooks must receive a value-free refusal without bare fallback")
				}
				assertDSHLegacyHookTestBytes(t, opts.ConfigPath, raw)
			}
		})
	}
}

func TestInspectDSHLegacyHooksRejectInvalidClaimsAndOwnedSets(t *testing.T) {
	for _, name := range []string{"unclaimed", "wrong runtime", "wrong path", "invalid claim", "wrong identity", "partial", "duplicate", "mixed", "foreign marker"} {
		t.Run(name, func(t *testing.T) {
			opts := dshOwnedHooksTestOptions(t, "hooks.json")
			if _, err := InstallOwnedHooks(opts, nil); err != nil {
				t.Fatal(err)
			}
			prior := opts
			claim := &prior
			root := readDSHHookTestDocument(t, opts.ConfigPath)
			hooks := root["hooks"].(map[string]any)
			switch name {
			case "unclaimed":
				claim = nil
			case "wrong runtime":
				prior.Runtime = RuntimeClaudeCode
			case "wrong path":
				prior.ConfigPath = filepath.Join(filepath.Dir(opts.ConfigPath), DSHDedicatedHooksFilename)
			case "invalid claim":
				prior.Location = dshHookErrorCanary + "/invalid"
			case "wrong identity":
				prior.Agent = dshHookErrorCanary
			case "partial":
				delete(hooks, "Stop")
			case "duplicate":
				hooks["Stop"] = append(hooks["Stop"].([]any), hooks["Stop"].([]any)[0])
			case "mixed":
				group := hooks["Stop"].([]any)[0].(map[string]any)
				group["hooks"] = append(group["hooks"].([]any), map[string]any{"command": dshHookErrorCanary})
			case "foreign marker":
				hooks[dshHookErrorCanary] = []any{map[string]any{"command": "foreign" + hookCommandMarker + "dsh " + dshHookErrorCanary}}
			}
			raw := writeDSHHookTestDocument(t, opts.ConfigPath, root)
			if remaining, err := InspectDSHLegacyHooks(opts.ConfigPath, claim); err == nil || remaining || strings.Contains(err.Error(), dshHookErrorCanary) {
				t.Fatalf("expected value-free inspection refusal, got %v, %v", remaining, err)
			}
			assertHookSecurityFileBytes(t, opts.ConfigPath, raw)
		})
	}
	// Validate claims even when the document is absent, before any recovery no-op.
	opts := dshOwnedHooksTestOptions(t, "hooks.json")
	wrong := opts
	wrong.Runtime = RuntimeClaudeCode
	if _, err := InspectDSHLegacyHooks(opts.ConfigPath, &wrong); err == nil {
		t.Fatal("missing document accepted an invalid claim")
	}
	if _, err := InstallOwnedHooks(opts, &wrong); err == nil {
		t.Fatal("install accepted prior ownership of a different runtime")
	}
	wrong = opts
	wrong.ConfigPath = filepath.Join(filepath.Dir(opts.ConfigPath), DSHDedicatedHooksFilename)
	if _, err := InstallOwnedHooks(opts, &wrong); err == nil {
		t.Fatal("install accepted prior ownership of a different path")
	}
	for _, path := range []string{"relative/hooks.json", opts.ConfigPath + "/../hooks.json", opts.ConfigPath + "\x00"} {
		if _, err := InspectDSHLegacyHooks(path, nil); err == nil {
			t.Fatal("inspection accepted an invalid path")
		}
	}
}

func TestOwnedDSHHooksRejectUnsafeDocuments(t *testing.T) {
	for name, raw := range map[string][]byte{
		"duplicate key":  []byte(`{"PRIVATE-HOOK-DOCUMENT-CANARY":1,"PRIVATE-HOOK-DOCUMENT-CANARY":2}`),
		"malformed":      []byte(`{"PRIVATE-HOOK-DOCUMENT-CANARY":`),
		"array root":     []byte(`["PRIVATE-HOOK-DOCUMENT-CANARY"]`),
		"null root":      []byte(`null`),
		"null hooks":     []byte(`{"hooks":null}`),
		"nonarray event": []byte(`{"hooks":{"PRIVATE-HOOK-DOCUMENT-CANARY":{}}}`),
		"oversized":      []byte(strings.Repeat(" ", hookConfigReadLimit+1)),
	} {
		t.Run(name, func(t *testing.T) {
			opts := dshOwnedHooksTestOptions(t, DSHDedicatedHooksFilename)
			if err := os.MkdirAll(filepath.Dir(opts.ConfigPath), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(opts.ConfigPath, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			assertDSHDedicatedHooksRefused(t, opts, raw)
			legacyPath := filepath.Join(filepath.Dir(opts.ConfigPath), "hooks.json")
			if err := os.WriteFile(legacyPath, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			opts.ConfigPath = legacyPath
			for _, prior := range []*UserHooksOptions{nil, &opts} {
				if remaining, err := InspectDSHLegacyHooks(legacyPath, prior); err == nil || remaining || strings.Contains(err.Error(), dshHookErrorCanary) {
					t.Fatalf("expected value-free unsafe document refusal, got %v, %v", remaining, err)
				}
				assertHookSecurityFileBytes(t, legacyPath, raw)
			}
		})
	}
	for _, shape := range []string{"symlink", "directory"} {
		t.Run(shape, func(t *testing.T) {
			opts := dshOwnedHooksTestOptions(t, DSHDedicatedHooksFilename)
			outside := filepath.Join(t.TempDir(), "outside.json")
			raw := writeDSHHookTestDocument(t, outside, map[string]any{"private": dshHookErrorCanary})
			if err := os.MkdirAll(filepath.Dir(opts.ConfigPath), 0o700); err != nil {
				t.Fatal(err)
			}
			if shape == "symlink" {
				if err := os.Symlink(outside, opts.ConfigPath); err != nil {
					if goruntime.GOOS == "windows" {
						t.Skipf("symlinks unavailable on this Windows runner: %v", err)
					}
					t.Fatal(err)
				}
			} else if err := os.Mkdir(opts.ConfigPath, 0o700); err != nil {
				t.Fatal(err)
			}
			if _, err := InstallOwnedHooks(opts, nil); err == nil {
				t.Fatal("unsafe target installed")
			}
			if err := VerifyOwnedHooks(opts); err == nil {
				t.Fatal("unsafe target verified")
			}
			if _, err := RemoveOwnedHooks(opts); err == nil {
				t.Fatal("unsafe target removed")
			}
			if _, err := InspectDSHLegacyHooks(opts.ConfigPath, &opts); err == nil {
				t.Fatal("unsafe inspection target accepted")
			}
			info, err := os.Lstat(opts.ConfigPath)
			if err != nil || (shape == "symlink" && info.Mode()&os.ModeSymlink == 0) || (shape == "directory" && !info.IsDir()) {
				t.Fatal("unsafe target changed")
			}
			assertHookSecurityFileBytes(t, outside, raw)
		})
	}
}

func TestOwnedDSHDedicatedHookCAS(t *testing.T) {
	for _, operation := range []string{"first install", "rebind", "remove"} {
		t.Run(operation, func(t *testing.T) {
			opts := dshOwnedHooksTestOptions(t, DSHDedicatedHooksFilename)
			if operation != "first install" {
				if _, err := InstallOwnedHooks(opts, nil); err != nil {
					t.Fatal(err)
				}
			}
			later := []byte(`{"operator":"concurrent"}`)
			ownedHookBeforeMutationForTest = func(path string) {
				ownedHookBeforeMutationForTest = nil
				if err := os.WriteFile(path, later, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(func() { ownedHookBeforeMutationForTest = nil })
			var err error
			switch operation {
			case "first install":
				_, err = InstallOwnedHooks(opts, nil)
			case "rebind":
				desired := opts
				desired.Agent = "next-agent"
				_, err = InstallOwnedHooks(desired, &opts)
			case "remove":
				_, err = RemoveOwnedHooks(opts)
			}
			if err == nil || !strings.Contains(err.Error(), "changed concurrently") {
				t.Fatalf("CAS refusal = %v", err)
			}
			assertHookSecurityFileBytes(t, opts.ConfigPath, later)
		})
	}
}
