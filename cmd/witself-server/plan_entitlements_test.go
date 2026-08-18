package main

import (
	"reflect"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/plans"
	"github.com/witwave-ai/witself/internal/server"
	"github.com/witwave-ai/witself/internal/store"
)

func TestProjectSelfAgentEntitlementsUsesOnlyClosedAgentDomainVocabulary(t *testing.T) {
	appliedAt := time.Now().UTC()
	got := projectSelfAgentEntitlements(store.AccountPlanSnapshot{
		AccountID: "acc_token", Plan: "standard", AppliedAt: &appliedAt,
		Features: []string{
			plans.MemoryFeature, plans.FactsFeature, plans.MessagingFeature,
			plans.AgentEmailReceiveFeature,
			// These valid or injected snapshot features are deliberately outside
			// the agent-domain browser projection.
			plans.SupportFeature, plans.AgentEmailCustomDomainFeature, "billing_admin",
		},
		Limits: map[string]int64{
			plans.RealmLimit: 1, plans.StoredMemoryLimit: 10_000,
		},
		Policies: map[string]int64{
			plans.TranscriptRetentionDaysPolicy:     90,
			plans.MessageRetentionDaysPolicy:        30,
			plans.MessagingEntitlementVersionPolicy: 1,
			"provider_retry_days":                   7,
		},
	})
	if got.SchemaVersion != server.SelfAgentEntitlementsSchema ||
		got.State != server.SelfAgentEntitlementsApplied ||
		got.Source != server.SelfAgentEntitlementsSource || got.EnforcedPlanID != "standard" {
		t.Fatalf("envelope = %#v", got)
	}
	wantFeatures := &server.SelfAgentEntitlementFeatures{
		Memory: true, Facts: true, Messaging: true, AgentEmailReceive: true,
	}
	if !reflect.DeepEqual(got.Features, wantFeatures) {
		t.Fatalf("features = %#v, want %#v", got.Features, wantFeatures)
	}
	if got.RetentionDays == nil || got.RetentionDays.TranscriptRetentionDays == nil ||
		*got.RetentionDays.TranscriptRetentionDays != 90 ||
		got.RetentionDays.MessageRetentionDays == nil || *got.RetentionDays.MessageRetentionDays != 30 ||
		got.RetentionDays.AgentEmailRetentionDays != nil {
		t.Fatalf("retention = %#v", got.RetentionDays)
	}
}

func TestProjectSelfAgentEntitlementsMarksNeverAppliedCellUnmanaged(t *testing.T) {
	got := projectSelfAgentEntitlements(store.AccountPlanSnapshot{
		Plan: "free", Features: []string{plans.MemoryFeature},
	})
	want := &server.SelfAgentEntitlements{
		SchemaVersion: server.SelfAgentEntitlementsSchema,
		State:         server.SelfAgentEntitlementsUnmanaged,
		Source:        server.SelfAgentEntitlementsSource,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unmanaged = %#v, want %#v", got, want)
	}
}

func TestProjectSelfAgentEntitlementsMatchesProductionRolloutMarkerSemantics(t *testing.T) {
	appliedAt := time.Now().UTC()

	t.Run("legacy applied snapshot keeps messaging and inbound email allowed", func(t *testing.T) {
		got := projectSelfAgentEntitlements(store.AccountPlanSnapshot{
			Plan: "legacy", AppliedAt: &appliedAt,
			Policies: map[string]int64{}, Features: []string{},
		})
		if got.Features == nil || !got.Features.Messaging || !got.Features.AgentEmailReceive ||
			got.Features.AgentEmailSend {
			t.Fatalf("legacy enforced features = %#v", got.Features)
		}
	})

	t.Run("modern marker makes explicit feature membership authoritative", func(t *testing.T) {
		policies := map[string]int64{
			plans.MessagingEntitlementVersionPolicy:  plans.MessagingEntitlementVersion,
			plans.AgentEmailEntitlementVersionPolicy: plans.AgentEmailEntitlementVersion,
		}
		disabled := projectSelfAgentEntitlements(store.AccountPlanSnapshot{
			Plan: "free", AppliedAt: &appliedAt, Policies: policies, Features: []string{},
		})
		if disabled.Features == nil || disabled.Features.Messaging || disabled.Features.AgentEmailReceive {
			t.Fatalf("modern disabled features = %#v", disabled.Features)
		}

		enabled := projectSelfAgentEntitlements(store.AccountPlanSnapshot{
			Plan: "standard", AppliedAt: &appliedAt, Policies: policies,
			Features: []string{plans.MessagingFeature, plans.AgentEmailReceiveFeature},
		})
		if enabled.Features == nil || !enabled.Features.Messaging || !enabled.Features.AgentEmailReceive {
			t.Fatalf("modern enabled features = %#v", enabled.Features)
		}
	})
}
