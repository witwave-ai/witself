package main

import (
	"strings"
	"testing"
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
				"pause the current task once",
				"realm style pack",
				"agent name as the strongest creative seed",
				"safe SVG",
				"exact profile revision",
				"fresh idempotency key",
				"backend never generates an image",
				"voluntary evolution",
				"meaningful identity or experience milestone",
				"Do not interrupt routine work",
				"Resume",
				"never wake or launch",
			} {
				if !strings.Contains(contract, want) {
					t.Errorf("avatar routing contract does not contain %q", want)
				}
			}
			showTool, failureTool := "witself.avatar.show", "witself.avatar.generation.fail"
			resetTool := "witself.avatar.reset"
			if name == "grok" {
				showTool, failureTool = "witself_avatar_show", "witself_avatar_generation_fail"
				resetTool = "witself_avatar_reset"
			}
			for _, want := range []string{showTool, resetTool, failureTool} {
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
		"agent_self_managed",
		"agent_proposes or operator_only",
		"operator must execute the reset",
		"Vague dissatisfaction",
		"not reset intent",
		"without purging immutable history",
		"never describe or treat it as deletion",
		"continue the normal generation flow",
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

func TestAvatarLifecyclePolicyBranchesProposalFromActivation(t *testing.T) {
	for _, want := range []string{
		"For activation_due",
		"never replace an activation-pending proposal",
		"Do not generate or propose another avatar in this branch",
		"For initial_avatar, avatar_reset, style_changed, proposal_rejected, or retry_due",
		"when policy is agent_self_managed, immediately call witself.avatar.activate",
		"a second fresh idempotency key",
		"If activation fails, leave the immutable proposal pending",
		"report it through witself.avatar.generation.fail only when no proposal is pending",
	} {
		if !strings.Contains(avatarRoutingInstructions, want) {
			t.Errorf("avatar routing contract does not contain %q", want)
		}
	}
}
