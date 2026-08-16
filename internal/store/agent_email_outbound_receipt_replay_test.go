package store

import (
	"errors"
	"testing"
	"time"
)

func TestValidateAgentEmailOutboundReceiptReplayRequiresExactFreshAcceptance(t *testing.T) {
	now := time.Date(2026, 8, 15, 18, 0, 0, 0, time.UTC)
	acceptedAt := now.Add(-time.Hour)
	validMessage := AgentEmailOutboundMessage{
		ID: "esnd_aaaaaaaaaaaaaaaa", AccountID: "acc_aaaaaaaaaaaaaaaa",
		State: AgentEmailOutboundAccepted, ProviderState: AgentEmailOutboundAccepted,
		Provider:          AgentEmailOutboundCloudflareProvider,
		ProviderMessageID: "private-provider-id", AttemptCount: 1,
		AcceptedAt: &acceptedAt,
	}
	validInput := AgentEmailOutboundReceiptReplayInput{
		AccountID: validMessage.AccountID, SendID: validMessage.ID,
		ExpectedAcceptedAt: acceptedAt, ExpectedAttemptCount: 1,
	}
	if err := validateAgentEmailOutboundReceiptReplay(validMessage, validInput, now); err != nil {
		t.Fatalf("valid receipt rejected: %v", err)
	}

	tests := map[string]func(*AgentEmailOutboundMessage, *AgentEmailOutboundReceiptReplayInput, *time.Time){
		"account": func(_ *AgentEmailOutboundMessage, in *AgentEmailOutboundReceiptReplayInput, _ *time.Time) {
			in.AccountID = "acc_bbbbbbbbbbbbbbbb"
		},
		"send": func(_ *AgentEmailOutboundMessage, in *AgentEmailOutboundReceiptReplayInput, _ *time.Time) {
			in.SendID = "esnd_bbbbbbbbbbbbbbbb"
		},
		"state": func(msg *AgentEmailOutboundMessage, _ *AgentEmailOutboundReceiptReplayInput, _ *time.Time) {
			msg.State = AgentEmailOutboundDelivered
		},
		"provider state": func(msg *AgentEmailOutboundMessage, _ *AgentEmailOutboundReceiptReplayInput, _ *time.Time) {
			msg.ProviderState = AgentEmailOutboundDelivered
		},
		"provider": func(msg *AgentEmailOutboundMessage, _ *AgentEmailOutboundReceiptReplayInput, _ *time.Time) {
			msg.Provider = "other_provider"
		},
		"provider id": func(msg *AgentEmailOutboundMessage, _ *AgentEmailOutboundReceiptReplayInput, _ *time.Time) {
			msg.ProviderMessageID = ""
		},
		"accepted missing": func(msg *AgentEmailOutboundMessage, _ *AgentEmailOutboundReceiptReplayInput, _ *time.Time) {
			msg.AcceptedAt = nil
		},
		"accepted mismatch": func(_ *AgentEmailOutboundMessage, in *AgentEmailOutboundReceiptReplayInput, _ *time.Time) {
			in.ExpectedAcceptedAt = acceptedAt.Add(time.Microsecond)
		},
		"attempt mismatch": func(_ *AgentEmailOutboundMessage, in *AgentEmailOutboundReceiptReplayInput, _ *time.Time) {
			in.ExpectedAttemptCount = 2
		},
		"expired": func(msg *AgentEmailOutboundMessage, in *AgentEmailOutboundReceiptReplayInput, _ *time.Time) {
			value := now.Add(-AgentEmailOutboundReceiptReplayTTL - time.Nanosecond)
			msg.AcceptedAt = &value
			in.ExpectedAcceptedAt = value
		},
		"future": func(msg *AgentEmailOutboundMessage, in *AgentEmailOutboundReceiptReplayInput, _ *time.Time) {
			value := now.Add(time.Nanosecond)
			msg.AcceptedAt = &value
			in.ExpectedAcceptedAt = value
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			msg := validMessage
			in := validInput
			clock := now
			mutate(&msg, &in, &clock)
			if err := validateAgentEmailOutboundReceiptReplay(msg, in, clock); !errors.Is(err, ErrAgentEmailOutboundReceiptReplayRefused) {
				t.Fatalf("error = %v, want replay refusal", err)
			}
		})
	}

	boundaryMessage := validMessage
	boundaryAcceptedAt := now.Add(-AgentEmailOutboundReceiptReplayTTL)
	boundaryMessage.AcceptedAt = &boundaryAcceptedAt
	boundaryInput := validInput
	boundaryInput.ExpectedAcceptedAt = boundaryAcceptedAt
	if err := validateAgentEmailOutboundReceiptReplay(
		boundaryMessage, boundaryInput, now,
	); err != nil {
		t.Fatalf("exact receipt TTL boundary rejected: %v", err)
	}
}
