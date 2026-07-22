//go:build darwin || linux

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

func TestAntigravityBundleOwnershipShapesAreExplicit(t *testing.T) {
	fixture := setupAntigravityIntegrationFixture(t)
	current := installAntigravityFixtureConfig(t, fixture)
	sharedBundle, err := verifiedAntigravitySourceBundle(current)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := sharedBundle.files["mcp_config.json"]; ok {
		t.Fatal("shared-MCP binding retained a plugin-level MCP declaration")
	}
	legacy := current
	legacy.RuntimeMCPConfigPath = ""
	legacyBundle, err := antigravityBundleFromConfig(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := legacyBundle.files["mcp_config.json"]; !ok {
		t.Fatal("legacy binding lacks its plugin-level MCP declaration")
	}
	if err := validateRecordedAntigravityBundle(current, sharedBundle); err != nil {
		t.Fatalf("shared-MCP bundle did not validate: %v", err)
	}
	if err := validateRecordedAntigravityBundle(legacy, legacyBundle); err != nil {
		t.Fatalf("legacy bundle did not validate: %v", err)
	}
	if err := validateRecordedAntigravityBundle(current, legacyBundle); err == nil {
		t.Fatal("legacy bundle was accepted for shared-MCP ownership")
	}
	if err := validateRecordedAntigravityBundle(legacy, sharedBundle); err == nil {
		t.Fatal("rules-only bundle was accepted for legacy ownership")
	}
}

func TestAntigravityFirstInstallRefusesMatchingUnownedSharedEntry(t *testing.T) {
	fixture := setupAntigravityIntegrationFixture(t)
	cfg := transcriptcapture.Config{
		Runtime:              transcriptcapture.RuntimeAntigravity,
		MCPCommand:           fixture.witself,
		MCPEnvironment:       map[string]string{"WITSELF_HOME": filepath.Join(fixture.home, ".witself")},
		RuntimeConfigRoot:    fixture.configRoot(),
		RuntimeMCPConfigPath: filepath.Join(fixture.configRoot(), "mcp_config.json"),
		Account:              "default", AccountID: "acc_1", Realm: "default", RealmID: "realm_1",
		Agent: "scott", AgentID: "agent_1", AgentName: "scott",
		Location: transcriptcapture.Location{Name: "home"},
	}
	name, err := antigravityMCPServerName(cfg)
	if err != nil {
		t.Fatal(err)
	}
	server, err := antigravityExpectedMCPServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(antigravityMCPConfig{Servers: map[string]antigravityMCPServer{name: server}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(fixture.configRoot(), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(fixture.configRoot(), "mcp_config.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if code := installCmd(fixture.installArgs()); code != 1 {
		t.Fatalf("matching unowned entry install code = %d", code)
	}
	if after, err := os.ReadFile(path); err != nil || string(after) != string(raw) {
		t.Fatalf("matching unowned entry changed: %q, %v", after, err)
	}
	if _, err := os.Lstat(fixture.pluginPath(t)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plugin written despite matching unowned entry: %v", err)
	}
}

func TestAntigravitySharedMCPDriftFailsClosed(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, cfg transcriptcapture.Config)
	}{
		{"malformed JSON", func(t *testing.T, cfg transcriptcapture.Config) {
			t.Helper()
			if err := os.WriteFile(cfg.RuntimeMCPConfigPath, []byte("{\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"extra target field", func(t *testing.T, cfg transcriptcapture.Config) {
			t.Helper()
			mutateAntigravitySharedMCPForTest(t, cfg, func(name string, _ map[string]json.RawMessage, servers map[string]json.RawMessage) {
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(servers[name], &fields); err != nil {
					t.Fatal(err)
				}
				fields["disabled"] = json.RawMessage("false")
				servers[name] = mustMarshalRawMessage(t, fields)
			})
		}},
		{"case alias", func(t *testing.T, cfg transcriptcapture.Config) {
			t.Helper()
			mutateAntigravitySharedMCPForTest(t, cfg, func(name string, _ map[string]json.RawMessage, servers map[string]json.RawMessage) {
				servers[strings.ToUpper(name)] = servers[name]
				delete(servers, name)
			})
		}},
		{"duplicate target key", func(t *testing.T, cfg transcriptcapture.Config) {
			t.Helper()
			name, err := antigravityMCPServerName(cfg)
			if err != nil {
				t.Fatal(err)
			}
			document, err := readAntigravitySharedMCPDocument(cfg.RuntimeMCPConfigPath)
			if err != nil {
				t.Fatal(err)
			}
			server := document.servers[name]
			raw := []byte(fmt.Sprintf(`{"mcpServers":{%q:%s,%q:%s}}`, name, server, name, server))
			if err := os.WriteFile(cfg.RuntimeMCPConfigPath, raw, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"insecure mode", func(t *testing.T, cfg transcriptcapture.Config) {
			t.Helper()
			if err := os.Chmod(cfg.RuntimeMCPConfigPath, 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"symlink", func(t *testing.T, cfg transcriptcapture.Config) {
			t.Helper()
			raw, err := os.ReadFile(cfg.RuntimeMCPConfigPath)
			if err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(filepath.Dir(cfg.RuntimeMCPConfigPath), "foreign-mcp.json")
			if err := os.WriteFile(target, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(cfg.RuntimeMCPConfigPath); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, cfg.RuntimeMCPConfigPath); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := setupAntigravityIntegrationFixture(t)
			cfg := installAntigravityFixtureConfig(t, fixture)
			bundle, err := verifiedAntigravitySourceBundle(cfg)
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(t, cfg)
			if err := validateAntigravityInstalledTopology(cfg); err == nil {
				t.Fatal("drifted shared MCP state passed topology validation")
			}
			if code := uninstallCmd([]string{"antigravity"}); code != 1 {
				t.Fatalf("drifted shared MCP uninstall code = %d", code)
			}
			if _, err := transcriptcapture.LoadConfig(transcriptcapture.RuntimeAntigravity); err != nil {
				t.Fatalf("integration config was removed despite shared drift: %v", err)
			}
			if err := verifyAntigravityBundleDirectory(cfg.RuntimePluginPath, bundle); err != nil {
				t.Fatalf("plugin changed despite shared drift: %v", err)
			}
		})
	}
}

func TestAntigravitySharedMCPFingerprintRecoveryAcrossLegacyMigration(t *testing.T) {
	for _, test := range []struct {
		name          string
		afterExchange bool
	}{
		{name: "before exchange"},
		{name: "after exchange", afterExchange: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := setupAntigravityIntegrationFixture(t)
			legacy := installLegacyAntigravityPluginMCPConfig(t, fixture)
			canonicalPath := filepath.Join(fixture.configRoot(), "mcp_config.json")
			if err := os.WriteFile(canonicalPath, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(canonicalPath, 0o644); err != nil {
				t.Fatal(err)
			}
			desired := prepareLegacyAntigravitySharedMCPMigration(t, legacy)
			journal, err := beginAntigravityTransaction(antigravityTransactionInstall, &legacy, &desired)
			if err != nil {
				t.Fatal(err)
			}
			stageAntigravitySharedMCPMutationForTest(t, journal, desired)
			if test.afterExchange {
				if err := exchangeManagedInstructionFiles(
					canonicalPath,
					antigravitySharedMCPScratchPathForJournal(fixture.configRoot(), journal),
				); err != nil {
					t.Fatal(err)
				}
			}
			if err := recoverAntigravityTransaction(fixture.configRoot()); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(canonicalPath)
			if err != nil || len(raw) != 0 {
				t.Fatalf("legacy canonical MCP config changed: %q, %v", raw, err)
			}
			info, err := os.Lstat(canonicalPath)
			if err != nil {
				t.Fatalf("inspect recovered legacy canonical MCP config: %v", err)
			}
			if info.Mode().Perm() != 0o644 {
				t.Fatalf("legacy canonical MCP mode = %v", info.Mode())
			}
			assertAntigravityTransactionAbsent(t, fixture.configRoot(), journal)
		})
	}
}

func TestAntigravitySharedMCPFingerprintRecoveryRetainsCapturedExternalEdit(t *testing.T) {
	fixture := setupAntigravityIntegrationFixture(t)
	legacy := installLegacyAntigravityPluginMCPConfig(t, fixture)
	canonicalPath := filepath.Join(fixture.configRoot(), "mcp_config.json")
	if err := os.WriteFile(canonicalPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(canonicalPath, 0o644); err != nil {
		t.Fatal(err)
	}
	desired := prepareLegacyAntigravitySharedMCPMigration(t, legacy)
	journal, err := beginAntigravityTransaction(antigravityTransactionInstall, &legacy, &desired)
	if err != nil {
		t.Fatal(err)
	}
	stageAntigravitySharedMCPMutationForTest(t, journal, desired)
	external := []byte("{\n  \"concurrentProviderEdit\": {\"preserved\": true}\n}\n")
	if err := os.WriteFile(canonicalPath, external, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := exchangeManagedInstructionFiles(
		canonicalPath,
		antigravitySharedMCPScratchPathForJournal(fixture.configRoot(), journal),
	); err != nil {
		t.Fatal(err)
	}
	if err := recoverAntigravityTransaction(fixture.configRoot()); err == nil {
		t.Fatal("recovery accepted a captured external shared MCP edit")
	}
	scratchPath := antigravitySharedMCPScratchPathForJournal(fixture.configRoot(), journal)
	if raw, err := os.ReadFile(scratchPath); err != nil || !bytes.Equal(raw, external) {
		t.Fatalf("captured external shared MCP edit was not retained: %q, %v", raw, err)
	}
	if _, err := os.Lstat(antigravityTransactionPath(fixture.configRoot())); err != nil {
		t.Fatalf("transaction journal was cleared after ambiguous recovery: %v", err)
	}
	if _, err := os.Lstat(antigravitySharedMCPMutationPath(fixture.configRoot(), journal)); err != nil {
		t.Fatalf("fingerprint fence was cleared after ambiguous recovery: %v", err)
	}
}

func TestAntigravitySharedMCPFingerprintRecoveryDiscardsCandidateBesideExternalEdit(t *testing.T) {
	fixture := setupAntigravityIntegrationFixture(t)
	legacy := installLegacyAntigravityPluginMCPConfig(t, fixture)
	canonicalPath := filepath.Join(fixture.configRoot(), "mcp_config.json")
	if err := os.WriteFile(canonicalPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(canonicalPath, 0o644); err != nil {
		t.Fatal(err)
	}
	desired := prepareLegacyAntigravitySharedMCPMigration(t, legacy)
	journal, err := beginAntigravityTransaction(antigravityTransactionInstall, &legacy, &desired)
	if err != nil {
		t.Fatal(err)
	}
	stageAntigravitySharedMCPMutationForTest(t, journal, desired)
	external := []byte("{\n  \"concurrentProviderEdit\": {\"preserved\": true}\n}\n")
	if err := os.WriteFile(canonicalPath, external, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := recoverAntigravityTransaction(fixture.configRoot()); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(canonicalPath); err != nil || !bytes.Equal(raw, external) {
		t.Fatalf("external canonical shared MCP edit was not preserved: %q, %v", raw, err)
	}
	assertAntigravityTransactionAbsent(t, fixture.configRoot(), journal)
}

func TestAntigravitySharedMCPFingerprintRecoveryRemovesPartialStagingWrite(t *testing.T) {
	fixture := setupAntigravityIntegrationFixture(t)
	legacy := installLegacyAntigravityPluginMCPConfig(t, fixture)
	canonicalPath := filepath.Join(fixture.configRoot(), "mcp_config.json")
	if err := os.WriteFile(canonicalPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(canonicalPath, 0o644); err != nil {
		t.Fatal(err)
	}
	desired := prepareLegacyAntigravitySharedMCPMigration(t, legacy)
	journal, err := beginAntigravityTransaction(antigravityTransactionInstall, &legacy, &desired)
	if err != nil {
		t.Fatal(err)
	}
	preimage, err := readAntigravitySharedMCPDocument(canonicalPath)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := antigravitySharedMCPDocumentWithTarget(preimage, &desired, &desired)
	if err != nil {
		t.Fatal(err)
	}
	exists, raw, err := antigravitySharedMCPDocumentOutput(candidate)
	if err != nil || !exists {
		t.Fatalf("prepare shared MCP candidate: exists=%t err=%v", exists, err)
	}
	mutation := antigravitySharedMCPMutation{
		SchemaVersion: antigravitySharedMCPMutationSchema,
		JournalID:     journal.ID,
		CanonicalPath: canonicalPath,
		Preimage:      antigravitySharedMCPDocumentFingerprint(preimage),
		Candidate:     antigravitySharedMCPCandidateFingerprint(exists, raw),
	}
	if err := writeAntigravitySharedMCPMutation(fixture.configRoot(), mutation); err != nil {
		t.Fatal(err)
	}
	stagePath := antigravitySharedMCPStagePath(
		antigravitySharedMCPScratchPathForJournal(fixture.configRoot(), journal),
	)
	if err := os.WriteFile(stagePath, raw[:len(raw)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	if err := recoverAntigravityTransaction(fixture.configRoot()); err != nil {
		t.Fatal(err)
	}
	if canonicalRaw, err := os.ReadFile(canonicalPath); err != nil || len(canonicalRaw) != 0 {
		t.Fatalf("partial stage recovery changed canonical config: %q, %v", canonicalRaw, err)
	}
	assertAntigravityTransactionAbsent(t, fixture.configRoot(), journal)
}

func prepareLegacyAntigravitySharedMCPMigration(
	t *testing.T,
	legacy transcriptcapture.Config,
) transcriptcapture.Config {
	t.Helper()
	desired := legacy
	desired.RuntimeMCPConfigPath = filepath.Join(desired.RuntimeConfigRoot, "mcp_config.json")
	bundle, err := antigravityBundleFromConfig(desired)
	if err != nil {
		t.Fatal(err)
	}
	desired.RuntimePluginDigest = bundle.digest()
	desired.RuntimePluginSource = filepath.Join(
		desired.MCPEnvironment["WITSELF_HOME"], "integrations", "antigravity", "bundles", desired.RuntimePluginDigest,
	)
	if err := stageAntigravitySourceBundle(desired); err != nil {
		t.Fatal(err)
	}
	return desired
}

func stageAntigravitySharedMCPMutationForTest(
	t *testing.T,
	journal antigravityTransactionJournal,
	desired transcriptcapture.Config,
) {
	t.Helper()
	preimage, err := readAntigravitySharedMCPDocument(desired.RuntimeMCPConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := antigravitySharedMCPDocumentWithTarget(preimage, &desired, &desired)
	if err != nil {
		t.Fatal(err)
	}
	exists, raw, err := antigravitySharedMCPDocumentOutput(candidate)
	if err != nil || !exists {
		t.Fatalf("prepare shared MCP candidate: exists=%t err=%v", exists, err)
	}
	mutation := antigravitySharedMCPMutation{
		SchemaVersion: antigravitySharedMCPMutationSchema,
		JournalID:     journal.ID,
		CanonicalPath: desired.RuntimeMCPConfigPath,
		Preimage:      antigravitySharedMCPDocumentFingerprint(preimage),
		Candidate:     antigravitySharedMCPCandidateFingerprint(exists, raw),
	}
	if err := writeAntigravitySharedMCPMutation(desired.RuntimeConfigRoot, mutation); err != nil {
		t.Fatal(err)
	}
	if err := writeAntigravitySharedMCPScratch(
		antigravitySharedMCPScratchPathForJournal(desired.RuntimeConfigRoot, journal), raw,
	); err != nil {
		t.Fatal(err)
	}
}

func mutateAntigravitySharedMCPForTest(
	t *testing.T,
	cfg transcriptcapture.Config,
	mutate func(name string, root, servers map[string]json.RawMessage),
) {
	t.Helper()
	document, err := readAntigravitySharedMCPDocument(cfg.RuntimeMCPConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	name, err := antigravityMCPServerName(cfg)
	if err != nil {
		t.Fatal(err)
	}
	mutate(name, document.root, document.servers)
	document.root["mcpServers"] = mustMarshalRawMessage(t, document.servers)
	raw, err := json.Marshal(document.root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.RuntimeMCPConfigPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustMarshalRawMessage(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
