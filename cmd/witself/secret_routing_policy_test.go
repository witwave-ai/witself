package main

import (
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

func TestSecretRoutingIsInstalledForEverySupportedRuntime(t *testing.T) {
	blocks := map[string]string{
		transcriptcapture.RuntimeCodex:      string(codexMemoryRoutingBlock),
		transcriptcapture.RuntimeClaudeCode: string(claudeMemoryRoutingBlock),
		transcriptcapture.RuntimeCursor:     string(cursorMemoryRoutingBlock),
		transcriptcapture.RuntimeGrokBuild:  string(grokMemoryRoutingBlock),
	}
	for runtimeName, block := range blocks {
		t.Run(runtimeName, func(t *testing.T) {
			for _, phrase := range []string{
				"## Witself agent secrets",
				"automatically search the agent's Witself secret inventory",
				"Reveal or calculate only the one exact field needed",
				"A missing or mismatched local agent vault key is a fail-closed condition",
				"MCP and Witself never wake an offline client",
			} {
				if !strings.Contains(block, phrase) {
					t.Errorf("installed routing omitted %q", phrase)
				}
			}
		})
	}
}

func TestSecretMCPProfilesCoverEverySupportedRuntime(t *testing.T) {
	runtimes := []string{
		transcriptcapture.RuntimeCodex,
		transcriptcapture.RuntimeClaudeCode,
		transcriptcapture.RuntimeCursor,
		transcriptcapture.RuntimeGrokBuild,
	}
	readTools := []string{"witself.secret.search", "witself.secret.show"}
	valueOrWriteTools := []string{
		"witself.secret.create", "witself.secret.reveal",
		"witself.password.generate", "witself.totp.code",
	}
	for _, runtimeName := range runtimes {
		t.Run(runtimeName, func(t *testing.T) {
			portable := func(name string) string {
				if runtimeName == transcriptcapture.RuntimeGrokBuild {
					return strings.ReplaceAll(name, ".", "_")
				}
				return name
			}
			backend := newFakeSecretMCPBackend()
			full := listSecretMCPTools(t, newWitselfMCPServerForRuntime(backend, runtimeName))
			for _, dotted := range append(append([]string(nil), readTools...), valueOrWriteTools...) {
				if full[portable(dotted)] == nil {
					t.Errorf("full profile omitted %s", portable(dotted))
				}
			}

			readOnly := listSecretMCPTools(t, newWitselfMCPServerForRuntimeOptions(
				backend, runtimeName, mcpServerOptions{Profile: mcpProfileReadOnly},
			))
			for _, dotted := range readTools {
				if readOnly[portable(dotted)] == nil {
					t.Errorf("read-only profile omitted %s", portable(dotted))
				}
			}
			for _, dotted := range valueOrWriteTools {
				if readOnly[portable(dotted)] != nil {
					t.Errorf("read-only profile retained %s", portable(dotted))
				}
			}
		})
	}
}
