package main

import (
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

func TestManagedRuntimeContractsCarryAvatarLifecyclePolicy(t *testing.T) {
	contracts := map[string]string{
		"codex":  string(codexMemoryRoutingBlock),
		"claude": string(claudeMemoryRoutingBlock),
		"cursor": string(cursorMemoryRoutingBlock),
		"grok":   string(grokMemoryRoutingBlock),
	}
	for name, contract := range contracts {
		t.Run(name, func(t *testing.T) {
			for _, want := range []string{
				"avatar_checkpoint",
				"User work comes first",
				"opportunity for bounded foreground self-maintenance",
				"explicit avatar or pending self-maintenance request",
				"tiny read-only, lookup, or status turn",
				"attempt count unchanged",
				"merely because the turn was deferred",
				"Deferral is not a lifecycle attempt or a generation failure",
				"realm style pack",
				"agent name as the strongest creative seed",
				"all returned canonical references",
				"as style exemplars",
				"choose one subject form",
				"active agent's own perspective",
				"one to three substantial local revisions",
				"not a user or operator approval dialog",
				"ephemeral and non-durable",
				"one final candidate",
				"exact profile revision",
				"fresh idempotency key",
				"activation records the agent's acceptance and settles",
				"identity is not settled until operator activation",
				"backend never generates an image",
				"voluntary evolution",
				"meaningful identity or experience milestone",
				"Do not interrupt routine work",
				"preserve that work in the final response",
				"never wake or launch",
			} {
				if !strings.Contains(contract, want) {
					t.Errorf("avatar routing contract does not contain %q", want)
				}
			}
			showTool, proposeTool, failureTool := "witself.avatar.show", "witself.avatar.propose", "witself.avatar.generation.fail"
			resetTool := "witself.avatar.reset"
			if name == "grok" {
				showTool, proposeTool, failureTool = "witself_avatar_show", "witself_avatar_propose", "witself_avatar_generation_fail"
				resetTool = "witself_avatar_reset"
			}
			for _, want := range []string{showTool, proposeTool, resetTool, failureTool} {
				if !strings.Contains(contract, want) {
					t.Errorf("avatar routing contract does not contain %q", want)
				}
			}
		})
	}
}

func TestAvatarLifecyclePolicyRequiresExplicitBoundedResetIntent(t *testing.T) {
	for _, want := range []string{
		`"start my avatar over"`,
		`"start my avatar from scratch"`,
		"First call witself.avatar.show",
		"exact autonomy policy and profile revision",
		"If there is no durable active or proposed version, do not call reset",
		"already at a fresh start",
		"continue the bounded generation-due flow",
		"make exactly one bounded witself.avatar.reset call",
		"continue the initial fitting flow",
		"Reset reopens broad agent-owned fitting",
		"new parentless lineage may substantially change subject form, palette, and defining details",
		"one chosen final candidate",
		"agent_self_managed",
		"agent_proposes or operator_only",
		"operator must execute the reset",
		"Vague dissatisfaction",
		"not reset intent",
		"without purging immutable history",
		"never describe or treat it as deletion",
	} {
		if !strings.Contains(avatarRoutingInstructions, want) {
			t.Errorf("avatar reset routing contract does not contain %q", want)
		}
	}
}

func TestAvatarLifecyclePolicyIsRuntimeNeutral(t *testing.T) {
	for _, forbidden := range []string{"Codex", "Claude", "Cursor", "Grok"} {
		if strings.Contains(avatarRoutingInstructions, forbidden) {
			t.Errorf("shared avatar policy contains provider name %q", forbidden)
		}
	}
}

func TestAvatarLifecyclePolicyDefersTinyTurnsWithoutMutationOrFailure(t *testing.T) {
	for _, want := range []string{
		"not a requirement to interrupt every prompt",
		"after an explicit avatar or pending self-maintenance request",
		"near the end of a non-trivial foreground turn",
		"For a tiny read-only, lookup, or status turn",
		"leave the checkpoint pending and its attempt count unchanged",
		"never call witself.avatar.generation.fail merely because the turn was deferred",
		"Deferral is not a lifecycle attempt or a generation failure",
		"On an eligible turn, handle at most one bounded lifecycle attempt",
	} {
		if !strings.Contains(avatarRoutingInstructions, want) {
			t.Errorf("avatar opportunity contract does not contain %q", want)
		}
	}
}

func TestAvatarLifecyclePolicyBranchesProposalFromActivation(t *testing.T) {
	for _, want := range []string{
		"For activation_due",
		"never replace an activation-pending proposal",
		"Do not generate or propose another avatar in this branch",
		"For initial_avatar, avatar_reset, or proposal_rejected",
		"retry_due when witself.avatar.show reports no active_version",
		"never call witself.avatar.propose for an intermediate or discarded draft",
		"call witself.avatar.propose once",
		"For style_changed, and for retry_due when witself.avatar.show reports an active_version",
		"when policy is agent_self_managed, immediately call witself.avatar.activate",
		"activation records the agent's acceptance and settles that chosen avatar",
		"agent's creative selection is complete, but identity is not settled until operator activation",
		"a second fresh idempotency key",
		"If activation fails, leave the immutable proposal pending",
		"report it through witself.avatar.generation.fail only when no proposal is pending",
	} {
		if !strings.Contains(avatarRoutingInstructions, want) {
			t.Errorf("avatar routing contract does not contain %q", want)
		}
	}
}

func TestAvatarInitialFittingIsAgentOwnedBeforeOneImmutableProposal(t *testing.T) {
	for _, want := range []string{
		"From the active agent's own perspective",
		"inspect whether it represents you",
		"one to three substantial local revisions",
		"subject form, facial hair, eyewear, eye color, palette, accessories, or expression",
		"not a user or operator approval dialog",
		"Do not put draft artifacts in the repository or project files",
		"clean up any temporary files",
		"An accidentally accepted proposal is immutable history",
		"After choosing exactly one final candidate",
		"activation records the agent's acceptance and settles that chosen avatar",
	} {
		if !strings.Contains(avatarRoutingInstructions, want) {
			t.Errorf("agent-owned initial-fitting contract does not contain %q", want)
		}
	}
}

func TestProviderMCPInstructionsCarryInitialFittingContract(t *testing.T) {
	fullContracts := map[string]string{
		"codex": mcpInstructions(
			transcriptcapture.RuntimeCodex, "witself.self.show", "witself.message.list"),
		"cursor": mcpInstructions(
			transcriptcapture.RuntimeCursor, "witself.self.show", "witself.message.list"),
		"grok": mcpInstructions(
			transcriptcapture.RuntimeGrokBuild, "witself_self_show", "witself_message_list"),
	}
	for name, contract := range fullContracts {
		t.Run(name, func(t *testing.T) {
			for _, want := range []string{
				"User work comes first",
				"active agent's own perspective",
				"one to three substantial local revisions",
				"ephemeral and non-durable",
				"identity is not settled until operator activation",
			} {
				if !strings.Contains(contract, want) {
					t.Errorf("provider MCP instructions omit %q", want)
				}
			}
		})
	}

	claude := mcpInstructions(
		transcriptcapture.RuntimeClaudeCode, "witself.self.show", "witself.message.list")
	for _, want := range []string{
		"Avatar opportunity:user-first",
		"Tiny/status=>defer",
		"pending/attempt same",
		"not failure",
		"Eligible=>review/final",
	} {
		if !strings.Contains(claude, want) {
			t.Errorf("Claude MCP synopsis omits %q", want)
		}
	}
}
