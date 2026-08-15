package store

import (
	"errors"
	"testing"
	"time"
)

func TestAgentEmailProviderEventCanaryPreflightAllowsPristineOrExactCompletedSend(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 30, 0, 0, time.UTC)
	acceptedAt := now.Add(-time.Minute)
	pristine := agentEmailProviderEventCanarySnapshot{
		state: AgentEmailOutboundAccepted, providerState: AgentEmailOutboundAccepted,
		provider:          AgentEmailOutboundCloudflareProvider,
		providerMessageID: "private-provider-id", acceptedAt: &acceptedAt,
		databaseNow: now, emailSentUsageCount: 1, canonicalEmailUsageCount: 1,
	}
	if err := validateAgentEmailProviderEventCanaryPreflight(pristine, acceptedAt); err != nil {
		t.Fatalf("valid pristine preflight = %v", err)
	}
	deliveredAt := acceptedAt
	completed := pristine
	completed.state = AgentEmailOutboundDelivered
	completed.providerState = AgentEmailOutboundDelivered
	completed.deliveredAt = &deliveredAt
	completed.providerEventCount = 1
	completed.eventIdentityCount = 1
	completed.matchingCanaryReceiptCount = 1
	if err := validateAgentEmailProviderEventCanaryPreflight(completed, acceptedAt); err != nil {
		t.Fatalf("valid completed preflight = %v", err)
	}

	mutations := map[string]func(*agentEmailProviderEventCanarySnapshot){
		"provider":    func(v *agentEmailProviderEventCanarySnapshot) { v.provider = "other" },
		"provider id": func(v *agentEmailProviderEventCanarySnapshot) { v.providerMessageID = "" },
		"accepted missing": func(v *agentEmailProviderEventCanarySnapshot) {
			v.acceptedAt = nil
		},
		"accepted stale": func(v *agentEmailProviderEventCanarySnapshot) {
			stale := now.Add(-15*time.Minute - time.Nanosecond)
			v.acceptedAt = &stale
		},
		"accepted future": func(v *agentEmailProviderEventCanarySnapshot) {
			future := now.Add(time.Nanosecond)
			v.acceptedAt = &future
		},
		"usage total": func(v *agentEmailProviderEventCanarySnapshot) {
			v.emailSentUsageCount = 2
		},
		"usage canonical": func(v *agentEmailProviderEventCanarySnapshot) {
			v.canonicalEmailUsageCount = 0
		},
	}
	for shape, base := range map[string]agentEmailProviderEventCanarySnapshot{
		"pristine": pristine, "completed": completed,
	} {
		for name, mutate := range mutations {
			t.Run(shape+"/"+name, func(t *testing.T) {
				candidate := base
				mutate(&candidate)
				expected := acceptedAt
				if name == "accepted stale" || name == "accepted future" {
					expected = *candidate.acceptedAt
				}
				if err := validateAgentEmailProviderEventCanaryPreflight(candidate, expected); !errors.Is(err, ErrAgentEmailProviderEventCanaryFence) {
					t.Fatalf("preflight error = %v", err)
				}
			})
		}
	}
	if err := validateAgentEmailProviderEventCanaryPreflight(
		pristine, acceptedAt.Add(time.Microsecond),
	); !errors.Is(err, ErrAgentEmailProviderEventCanaryFence) {
		t.Fatalf("accepted-at mismatch = %v", err)
	}

	invalidPristine := map[string]func(*agentEmailProviderEventCanarySnapshot){
		"message state": func(v *agentEmailProviderEventCanarySnapshot) {
			v.state = AgentEmailOutboundDelivered
		},
		"provider state": func(v *agentEmailProviderEventCanarySnapshot) {
			v.providerState = AgentEmailOutboundDeferred
		},
		"delivery timestamp": func(v *agentEmailProviderEventCanarySnapshot) {
			v.deliveredAt = &deliveredAt
		},
		"existing receipt": func(v *agentEmailProviderEventCanarySnapshot) {
			v.providerEventCount = 1
		},
		"identity used by another send": func(v *agentEmailProviderEventCanarySnapshot) {
			v.eventIdentityCount = 1
		},
		"impossible matching receipt": func(v *agentEmailProviderEventCanarySnapshot) {
			v.matchingCanaryReceiptCount = 1
		},
	}
	for name, mutate := range invalidPristine {
		t.Run("pristine/"+name, func(t *testing.T) {
			candidate := pristine
			mutate(&candidate)
			if err := validateAgentEmailProviderEventCanaryPreflight(candidate, acceptedAt); !errors.Is(err, ErrAgentEmailProviderEventCanaryFence) {
				t.Fatalf("preflight error = %v", err)
			}
		})
	}
}

func TestAgentEmailProviderEventCanaryPreflightRejectsLookalikeAndUnrelatedReceipt(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 30, 0, 0, time.UTC)
	acceptedAt := now.Add(-time.Minute)
	deliveredAt := acceptedAt
	completed := agentEmailProviderEventCanarySnapshot{
		state: AgentEmailOutboundDelivered, providerState: AgentEmailOutboundDelivered,
		provider:          AgentEmailOutboundCloudflareProvider,
		providerMessageID: "private-provider-id", acceptedAt: &acceptedAt,
		deliveredAt: &deliveredAt, databaseNow: now,
		providerEventCount: 1, eventIdentityCount: 1,
		matchingCanaryReceiptCount: 1,
		emailSentUsageCount:        1, canonicalEmailUsageCount: 1,
	}
	cases := map[string]func(*agentEmailProviderEventCanarySnapshot){
		"lookalike wrong request hash class or timestamp": func(v *agentEmailProviderEventCanarySnapshot) {
			v.matchingCanaryReceiptCount = 0
		},
		"unrelated real receipt": func(v *agentEmailProviderEventCanarySnapshot) {
			v.eventIdentityCount = 0
			v.matchingCanaryReceiptCount = 0
		},
		"identity reused by another send": func(v *agentEmailProviderEventCanarySnapshot) {
			v.providerEventCount = 0
			v.matchingCanaryReceiptCount = 0
		},
		"extra real receipt": func(v *agentEmailProviderEventCanarySnapshot) {
			v.providerEventCount = 2
		},
		"different delivery timestamp": func(v *agentEmailProviderEventCanarySnapshot) {
			other := acceptedAt.Add(time.Nanosecond)
			v.deliveredAt = &other
		},
		"different message state": func(v *agentEmailProviderEventCanarySnapshot) {
			v.state = AgentEmailOutboundBounced
		},
		"different provider state": func(v *agentEmailProviderEventCanarySnapshot) {
			v.providerState = AgentEmailOutboundDeferred
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			candidate := completed
			mutate(&candidate)
			if err := validateAgentEmailProviderEventCanaryPreflight(candidate, acceptedAt); !errors.Is(err, ErrAgentEmailProviderEventCanaryFence) {
				t.Fatalf("preflight error = %v", err)
			}
		})
	}
}

func TestAgentEmailProviderEventCanaryPostflightRequiresExactOneOneDeliveredFold(t *testing.T) {
	acceptedAt := time.Date(2026, 8, 15, 12, 30, 0, 0, time.UTC)
	deliveredAt := acceptedAt
	target := AgentEmailProviderEventCanaryTarget{
		AccountID: "acc_aaaaaaaaaaaaaaaa", SendID: "esnd_bbbbbbbbbbbbbbbb",
		ProviderMessageID: "private-provider-id", AcceptedAt: acceptedAt,
	}
	base := agentEmailProviderEventCanarySnapshot{
		state: AgentEmailOutboundDelivered, providerState: AgentEmailOutboundDelivered,
		provider:          AgentEmailOutboundCloudflareProvider,
		providerMessageID: target.ProviderMessageID, acceptedAt: &acceptedAt,
		deliveredAt: &deliveredAt, providerEventCount: 1, eventIdentityCount: 1,
		matchingCanaryReceiptCount: 1, emailSentUsageCount: 1,
		canonicalEmailUsageCount: 1,
	}
	if err := validateAgentEmailProviderEventCanaryPostflight(base, target); err != nil {
		t.Fatalf("valid postflight = %v", err)
	}
	mutations := map[string]func(*agentEmailProviderEventCanarySnapshot){
		"message state":     func(v *agentEmailProviderEventCanarySnapshot) { v.state = AgentEmailOutboundAccepted },
		"provider state":    func(v *agentEmailProviderEventCanarySnapshot) { v.providerState = AgentEmailOutboundAccepted },
		"provider":          func(v *agentEmailProviderEventCanarySnapshot) { v.provider = "other" },
		"provider id":       func(v *agentEmailProviderEventCanarySnapshot) { v.providerMessageID = "other" },
		"accepted missing":  func(v *agentEmailProviderEventCanarySnapshot) { v.acceptedAt = nil },
		"delivered missing": func(v *agentEmailProviderEventCanarySnapshot) { v.deliveredAt = nil },
		"delivered timestamp": func(v *agentEmailProviderEventCanarySnapshot) {
			other := acceptedAt.Add(time.Nanosecond)
			v.deliveredAt = &other
		},
		"receipt total": func(v *agentEmailProviderEventCanarySnapshot) { v.providerEventCount = 2 },
		"receipt identity total": func(v *agentEmailProviderEventCanarySnapshot) {
			v.eventIdentityCount = 0
		},
		"receipt identity": func(v *agentEmailProviderEventCanarySnapshot) { v.matchingCanaryReceiptCount = 0 },
		"usage total":      func(v *agentEmailProviderEventCanarySnapshot) { v.emailSentUsageCount = 0 },
		"usage canonical":  func(v *agentEmailProviderEventCanarySnapshot) { v.canonicalEmailUsageCount = 0 },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			candidate := base
			mutate(&candidate)
			if err := validateAgentEmailProviderEventCanaryPostflight(candidate, target); !errors.Is(err, ErrAgentEmailProviderEventCanaryFence) {
				t.Fatalf("postflight error = %v", err)
			}
		})
	}
}
