package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/plans"
	"github.com/witwave-ai/witself/internal/server"
	"github.com/witwave-ai/witself/internal/store"
)

func TestConfigureAgentEmailOutboundIsIndependentFromReceive(t *testing.T) {
	t.Setenv(agentEmailProviderEventTokenEnv, "provider-event-token-0123456789-abcd")
	var cfg server.Config
	if err := configureAgentEmail(context.Background(), &cfg, &store.Store{}, server.AgentEmailReceiveConfig{}); err != nil {
		t.Fatal(err)
	}
	if cfg.RequireAgentEmailSendEntitlement == nil || cfg.QueueAgentEmail == nil ||
		cfg.ReplyAgentEmail == nil || cfg.ListAgentEmailOutbox == nil ||
		cfg.GetAgentEmailOutbound == nil || cfg.GetAgentEmailSendControl == nil ||
		cfg.SetAgentEmailSendControl == nil || cfg.GetRealmEmailSendControl == nil ||
		cfg.SetRealmEmailSendControl == nil || cfg.ApplyAgentEmailOutboundProviderEvent == nil {
		t.Fatalf("outbound callbacks not fully configured: %#v", cfg)
	}
	if cfg.IngestAgentEmailPilot != nil || cfg.GetAgentEmailAddress != nil {
		t.Fatal("disabled receive unexpectedly installed receive callbacks")
	}
}

func TestConfigureAgentEmailProviderEventTokenFailsClosed(t *testing.T) {
	for _, token := range []string{"", "too-short", " " + "provider-event-token-0123456789-abcd"} {
		t.Run(token, func(t *testing.T) {
			t.Setenv(agentEmailProviderEventTokenEnv, token)
			var cfg server.Config
			if err := configureAgentEmail(context.Background(), &cfg, &store.Store{}, server.AgentEmailReceiveConfig{}); err == nil {
				t.Fatalf("token %q was accepted", token)
			}
			if cfg.ApplyAgentEmailOutboundProviderEvent != nil {
				t.Fatal("invalid token installed provider callback")
			}
		})
	}
}

func TestAgentEmailOutboundErrorMappingPreservesPolicyRateAndConflictMeaning(t *testing.T) {
	feature := mapAgentEmailOutboundError(&store.FeatureNotEnabledError{Feature: plans.AgentEmailSendFeature})
	var featureDetail *server.FeatureNotEnabledError
	if !errors.As(feature, &featureDetail) || featureDetail.Feature != plans.AgentEmailSendFeature {
		t.Fatalf("feature mapping = %#v", feature)
	}
	if disabled := mapAgentEmailOutboundError(store.ErrAgentEmailSendDisabled); !errors.As(disabled, &featureDetail) || featureDetail.Feature != plans.AgentEmailSendFeature {
		t.Fatalf("disabled mapping = %#v", disabled)
	}
	if got := mapAgentEmailOutboundError(store.ErrAgentEmailOutboundConflict); !errors.Is(got, server.ErrIdempotencyConflict) {
		t.Fatalf("send conflict = %v", got)
	}
	if got := mapAgentEmailOutboundControlError(store.ErrAgentEmailOutboundConflict); !errors.Is(got, server.ErrConflict) || errors.Is(got, server.ErrIdempotencyConflict) {
		t.Fatalf("control conflict = %v", got)
	}
	rate := mapAgentEmailOutboundError(&store.AgentEmailOutboundRateLimitError{
		Scope: "agent", Limit: 4, Used: 4, WindowSeconds: 60,
		RetryAfter: 2 * time.Second, Source: "plan", Retryable: true,
	})
	var rateDetail *server.AgentEmailOutboundRateLimitError
	if !errors.As(rate, &rateDetail) || rateDetail.Scope != "agent" || rateDetail.Limit != 4 || rateDetail.Used != 4 || rateDetail.RetryAfter != 2*time.Second {
		t.Fatalf("rate mapping = %#v", rate)
	}
}

func TestAgentEmailOutboundProjectionNeverExposesProviderCorrelationID(t *testing.T) {
	when := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	got := toServerAgentEmailOutboundMessage(store.AgentEmailOutboundMessage{
		ID: "esnd_aaaaaaaaaaaaaaaa", AccountID: "acc_aaaaaaaaaaaaaaaa",
		RealmID: "realm_aaaaaaaaaaaaaaaa", OwnerAgentID: "agent_aaaaaaaaaaaaaaaa",
		FromAddress: "owner.realm@send.witmail.net", ReplyToAddress: "owner.realm@witmail.net",
		ToAddress: "person@example.com", Subject: "Hello", State: "accepted",
		Provider: "cloudflare_email_sending", ProviderState: "accepted",
		ProviderMessageID: "private-provider-correlation", QueuedAt: when, CreatedAt: when, UpdatedAt: when,
	})
	if got.ID == "" || got.From == "" || got.Provider != "cloudflare_email_sending" {
		t.Fatalf("projection = %#v", got)
	}
}
