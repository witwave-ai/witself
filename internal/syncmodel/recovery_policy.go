package syncmodel

import (
	"errors"
	"time"
)

// DestinationMode is the closed SC-8 policy inventory.
type DestinationMode string

const (
	// LocalStandalone has explicit local policy without hosted entitlements.
	LocalStandalone DestinationMode = "local_standalone"
	// SelfManaged applies its own authoritative destination policy.
	SelfManaged DestinationMode = "self_managed"
	// SaaSDestination relies on hosted policy and billing authority.
	SaaSDestination DestinationMode = "saas"
)

// DestinationPolicyRow makes unsupported capability and custody explicit.
type DestinationPolicyRow struct {
	Mode                                                                  DestinationMode
	Messaging, HostedEmail, ProviderCustody, VaultCapacity, PaidAuthority string
}

// DestinationPolicyRows is the complete SC-8.2 fixture inventory. It is a model
// catalog and installs no production policy or default configuration.
func DestinationPolicyRows() []DestinationPolicyRow {
	return []DestinationPolicyRow{
		{LocalStandalone, "explicit_local_policy", "unsupported", "unsupported", "explicit_local_policy", "unsupported"},
		{SelfManaged, "destination_policy", "explicitly_operated_service", "destination_service", "destination_policy", "destination_policy_only"},
		{SaaSDestination, "destination_policy", "hosted_entitlement_routing_capacity", "hosted_service", "saas_policy", "saas_control_plane"},
	}
}

// DestinationAdmission describes independently authenticated fixture policy.
// ClaimedPaidPlan and CopiedProviderState are adversarial input, never authority.
type DestinationAdmission struct {
	Mode                                                                   DestinationMode
	ExplicitPolicy, AuthoritativePolicy, OwnerAuthorized, ReplicaEnrolled  bool
	ProtocolCompatible, SchemaCompatible, CapacityAvailable, CompleteScope bool
	AgentCredential, ClaimedPaidPlan, CopiedProviderState                  bool
	EmailOperated, EmailEntitled, EmailRouted, EmailCapacity               bool
}

// DestinationDecision preserves local work even when connected admission fails.
type DestinationDecision struct {
	WholeAccountAllowed, LocalMessaging, PreservePending, ProviderDispatch bool
	Reason                                                                 string
}

// EvaluateDestinationAdmission refuses partial scope, copied entitlement and
// agent download privilege. Local/self-managed use requires no SaaS plan.
func EvaluateDestinationAdmission(a DestinationAdmission) (DestinationDecision, error) {
	decision := DestinationDecision{LocalMessaging: true, PreservePending: true}
	switch a.Mode {
	case LocalStandalone:
		decision.LocalMessaging = a.ExplicitPolicy
		decision.Reason = "standalone_no_connected_authority"
		return decision, nil
	case SelfManaged, SaaSDestination:
	default:
		return DestinationDecision{}, errors.New("unknown destination mode")
	}
	switch {
	case !a.AuthoritativePolicy:
		decision.Reason = "authoritative_policy_required"
	case !a.OwnerAuthorized || a.AgentCredential:
		decision.Reason = "owner_authorization_required"
	case !a.ReplicaEnrolled:
		decision.Reason = "replica_enrollment_required"
	case !a.ProtocolCompatible || !a.SchemaCompatible:
		decision.Reason = "capability_required"
	case !a.CapacityAvailable:
		decision.Reason = "capacity_refused"
	case !a.CompleteScope:
		decision.Reason = "whole_account_scope_required"
	default:
		decision.WholeAccountAllowed = true
		decision.Reason = "admitted"
	}
	// Enrollment never transports provider credentials. Only independently
	// operated destination-service authority may dispatch; copied state has no
	// positive effect, including when hosted entitlement is absent.
	decision.ProviderDispatch = decision.WholeAccountAllowed && a.EmailOperated && a.EmailRouted && a.EmailCapacity && (a.Mode == SelfManaged || a.EmailEntitled)
	return decision, nil
}

// ReplicaGrant models a personal replica grant independently of agent profiles.
type ReplicaGrant struct {
	AccountID, ReplicaID, Enrollment string
	IssuedAt, ExpiresAt              time.Time
	Retired                          bool
}

// GrantContinuation supplies permission and deletion proof separately; renewed
// permission alone cannot extend a stalled cursor or erase pending operations.
type GrantContinuation struct {
	Now, LastObserved                                      time.Time
	ClockCertain, AuthoritativePermission, DeletionCatchup bool
	Renew                                                  bool
	RequestedLifetime, DeletionRetention                   time.Duration
	Pending                                                []OperationID
}

// GrantDecision carries a copied unresolved-work list and never fresh operation
// identities. Independent local work remains available under its own policy.
type GrantDecision struct {
	Reason                              RecoveryReason
	CursorAllowed, IndependentLocalWork bool
	ExpiresAt                           time.Time
	Pending                             []OperationID
}

// EvaluateGrantContinuation implements SC-9's expiry/catch-up/retirement order.
func EvaluateGrantContinuation(g ReplicaGrant, input GrantContinuation) (GrantDecision, error) {
	if !validLabel(g.AccountID) || !validLabel(g.ReplicaID) || !validLabel(g.Enrollment) || g.IssuedAt.IsZero() || !g.ExpiresAt.After(g.IssuedAt) || input.Now.IsZero() {
		return GrantDecision{}, errors.New("invalid replica grant")
	}
	out := GrantDecision{IndependentLocalWork: true, ExpiresAt: g.ExpiresAt, Pending: append([]OperationID(nil), input.Pending...)}
	switch {
	case g.Retired:
		out.Reason = ReplicaReenrollmentRequired
	case !input.ClockCertain || input.Now.Before(input.LastObserved) || input.Now.Before(g.IssuedAt) || !input.AuthoritativePermission:
		out.Reason = AuthorizationCoverageUnproven
	case !input.DeletionCatchup:
		out.Reason = RebootstrapRequired
	case input.Renew:
		if input.RequestedLifetime <= 0 || input.DeletionRetention < DeletionSafetyMargin || input.RequestedLifetime > input.DeletionRetention-DeletionSafetyMargin {
			return GrantDecision{}, errors.New("grant exceeds deletion retention")
		}
		out.ExpiresAt = input.Now.Add(input.RequestedLifetime)
		out.Reason, out.CursorAllowed = ContinuationVerified, true
	case !input.Now.Before(g.ExpiresAt):
		out.Reason = AuthorizationCoverageUnproven
	default:
		out.Reason, out.CursorAllowed = ContinuationVerified, true
	}
	return out, nil
}

// ReenrollRetiredReplica requires new identity and enrollment. Old grants and
// cursors do not become valid through a restored database or name reuse.
func ReenrollRetiredReplica(old ReplicaGrant, replicaID, enrollment string, now time.Time) (ReplicaGrant, error) {
	if !old.Retired || !validLabel(old.AccountID) || !validLabel(replicaID) || !validLabel(enrollment) || replicaID == old.ReplicaID || enrollment == old.Enrollment || now.IsZero() {
		return ReplicaGrant{}, errors.New("new replica enrollment required")
	}
	return ReplicaGrant{AccountID: old.AccountID, ReplicaID: replicaID, Enrollment: enrollment, IssuedAt: now, ExpiresAt: now.Add(DefaultOfflineGrant)}, nil
}
