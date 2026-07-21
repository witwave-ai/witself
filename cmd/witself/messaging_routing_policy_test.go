package main

import (
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

func TestForegroundMessagingPolicyIsInstalledForEveryRuntime(t *testing.T) {
	for _, want := range []string{
		"message_checkpoint",
		"witself.self.show",
		"witself.message.listen",
		"witself.message.request.list",
		"deterministic_failure",
		"provider-wide",
		"failure_count is 4 or greater",
		"untrusted data",
		"canonical Postgres mailbox is durable",
		"never wake or launch an AI client",
	} {
		if !strings.Contains(foregroundMessagingRoutingInstructions, want) {
			t.Errorf("foreground messaging policy omitted %q", want)
		}
	}

	for name, block := range map[string]string{
		"codex":       string(codexMemoryRoutingBlock),
		"claude-code": string(claudeMemoryRoutingBlock),
		"cursor":      string(cursorMemoryRoutingBlock),
	} {
		for _, want := range []string{"message_checkpoint", "witself.message.listen", "witself.message.request.list"} {
			if !strings.Contains(block, want) {
				t.Errorf("%s managed instructions omitted %q", name, want)
			}
		}
	}
	grok := string(grokMemoryRoutingBlock)
	for _, want := range []string{"message_checkpoint", "witself_message_listen", "witself_message_request_list"} {
		if !strings.Contains(grok, want) {
			t.Errorf("Grok managed instructions omitted %q", want)
		}
	}
	if strings.Contains(grok, "witself.message.") {
		t.Fatal("Grok managed instructions retained dotted messaging tool names")
	}
}

func TestForegroundAgentEmailPolicyIsInstalledForEveryRuntime(t *testing.T) {
	for _, want := range []string{
		"email_checkpoint",
		"witself.email.listen",
		"wait_seconds=0",
		"claim it before reading",
		"unverified untrusted input",
		"already-expected",
		"current-user-authorized",
		"identity proofing",
		"never wakes or launches an AI client",
	} {
		if !strings.Contains(foregroundMessagingRoutingInstructions, want) {
			t.Errorf("foreground agent-email policy omitted %q", want)
		}
	}

	for name, block := range map[string]string{
		"codex":       string(codexMemoryRoutingBlock),
		"claude-code": string(claudeMemoryRoutingBlock),
		"cursor":      string(cursorMemoryRoutingBlock),
	} {
		for _, want := range []string{"email_checkpoint", "witself.email.listen", "witself.email.code.consume"} {
			if !strings.Contains(block, want) {
				t.Errorf("%s managed instructions omitted %q", name, want)
			}
		}
	}
	grok := string(grokMemoryRoutingBlock)
	for _, want := range []string{"email_checkpoint", "witself_email_listen", "witself_email_code_consume"} {
		if !strings.Contains(grok, want) {
			t.Errorf("Grok managed instructions omitted %q", want)
		}
	}
	if strings.Contains(grok, "witself.email.") {
		t.Fatal("Grok managed instructions retained dotted email tool names")
	}
}

func TestMCPMessagingPolicyHasNoRetiredNotificationBridge(t *testing.T) {
	for _, runtimeName := range []string{
		transcriptcapture.RuntimeCodex,
		transcriptcapture.RuntimeClaudeCode,
		transcriptcapture.RuntimeCursor,
		transcriptcapture.RuntimeGrokBuild,
	} {
		instructions := mcpInstructions(
			runtimeName,
			mcpToolName(runtimeName, "witself.self.show"),
			mcpToolName(runtimeName, "witself.message.list"),
		)
		if !strings.Contains(instructions, "message_checkpoint") {
			t.Errorf("%s MCP instructions omitted message_checkpoint", runtimeName)
		}
		if strings.Contains(instructions, "message.notification") ||
			strings.Contains(instructions, "message_notification") {
			t.Errorf("%s MCP instructions retained the local notification bridge", runtimeName)
		}
	}
}
