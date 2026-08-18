package main

import (
	"context"
	"fmt"

	"github.com/witwave-ai/witself/internal/plans"
	"github.com/witwave-ai/witself/internal/server"
	"github.com/witwave-ai/witself/internal/store"
)

// configureSelfPlanEntitlements wires the agent-only self projection directly
// to the token account's durable cell snapshot. This adapter performs no
// catalog, control-plane, billing-provider, or other external-service read;
// its sole I/O is the cell store query below.
func configureSelfPlanEntitlements(cfg *server.Config, st *store.Store) {
	cfg.GetSelfPlanEntitlements = func(ctx context.Context, p server.DomainPrincipal) (*server.SelfAgentEntitlements, error) {
		if p.Kind != server.PrincipalKindAgent || p.AccountID == "" || p.ID == "" || p.RealmID == "" {
			return nil, fmt.Errorf("self plan entitlements require a complete agent principal")
		}
		snapshot, err := st.GetAccountPlan(ctx, p.AccountID)
		if err != nil {
			return nil, err
		}
		if snapshot.AccountID != p.AccountID {
			return nil, fmt.Errorf("self plan entitlement snapshot account mismatch")
		}
		return projectSelfAgentEntitlements(snapshot), nil
	}
}

func projectSelfAgentEntitlements(snapshot store.AccountPlanSnapshot) *server.SelfAgentEntitlements {
	if snapshot.AppliedAt == nil {
		return &server.SelfAgentEntitlements{
			SchemaVersion: server.SelfAgentEntitlementsSchema,
			State:         server.SelfAgentEntitlementsUnmanaged,
			Source:        server.SelfAgentEntitlementsSource,
		}
	}
	features := map[string]bool{}
	for _, feature := range snapshot.Features {
		features[feature] = true
	}
	return &server.SelfAgentEntitlements{
		SchemaVersion:  server.SelfAgentEntitlementsSchema,
		State:          server.SelfAgentEntitlementsApplied,
		Source:         server.SelfAgentEntitlementsSource,
		EnforcedPlanID: snapshot.Plan,
		Features: &server.SelfAgentEntitlementFeatures{
			Memory:            features[plans.MemoryFeature],
			Facts:             features[plans.FactsFeature],
			Secrets:           features[plans.SecretsFeature],
			Messaging:         store.MessagingEnabledForPlanSnapshot(snapshot.AppliedAt, snapshot.Policies, snapshot.Features),
			Collaboration:     features[plans.CollaborationFeature],
			AgentEmailReceive: store.AgentEmailReceiveEnabledForPlanSnapshot(snapshot.AppliedAt, snapshot.Policies, snapshot.Features),
			AgentEmailSend:    features[plans.AgentEmailSendFeature],
		},
		RetentionDays: &server.SelfAgentRetentionDays{
			TranscriptRetentionDays: retentionDays(snapshot.Policies, plans.TranscriptRetentionDaysPolicy),
			MessageRetentionDays:    retentionDays(snapshot.Policies, plans.MessageRetentionDaysPolicy),
			AgentEmailRetentionDays: retentionDays(snapshot.Policies, plans.AgentEmailRetentionDaysPolicy),
		},
	}
}

func retentionDays(policies map[string]int64, key string) *int64 {
	value, ok := policies[key]
	if !ok {
		return nil
	}
	return &value
}
