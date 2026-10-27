package syncmodel

import (
	"reflect"
	"testing"
	"time"
)

func fixtureDestinationAdmission(mode DestinationMode) DestinationAdmission {
	return DestinationAdmission{Mode: mode, ExplicitPolicy: true, AuthoritativePolicy: true, OwnerAuthorized: true, ReplicaEnrolled: true, ProtocolCompatible: true, SchemaCompatible: true, CapacityAvailable: true, CompleteScope: true}
}

func TestDestinationPolicyModes(t *testing.T) {
	rows := DestinationPolicyRows()
	if len(rows) != 3 || rows[0].Mode != LocalStandalone || rows[1].Mode != SelfManaged || rows[2].Mode != SaaSDestination || rows[0].HostedEmail != "unsupported" || rows[0].ProviderCustody != "unsupported" || rows[1].PaidAuthority != "destination_policy_only" || rows[2].PaidAuthority != "saas_control_plane" {
		t.Fatal("explicit three-mode policy inventory changed")
	}
	for _, mode := range []DestinationMode{LocalStandalone, SelfManaged, SaaSDestination} {
		t.Run(string(mode), func(t *testing.T) {
			a := fixtureDestinationAdmission(mode)
			d, err := EvaluateDestinationAdmission(a)
			if err != nil || !d.LocalMessaging || !d.PreservePending || d.ProviderDispatch || d.WholeAccountAllowed != (mode != LocalStandalone) {
				t.Fatal("subscription-free local/self-managed policy or explicit provider boundary failed")
			}
			a.CopiedProviderState, a.ClaimedPaidPlan = true, true
			d, err = EvaluateDestinationAdmission(a)
			if err != nil || d.ProviderDispatch {
				t.Fatal("copied provider or paid-plan state conferred dispatch")
			}
		})
	}
	for _, test := range []struct {
		name   string
		mutate func(*DestinationAdmission)
		want   string
	}{
		{"forged_paid_plan", func(a *DestinationAdmission) { a.AuthoritativePolicy = false; a.ClaimedPaidPlan = true }, "authoritative_policy_required"},
		{"agent_download", func(a *DestinationAdmission) { a.AgentCredential = true }, "owner_authorization_required"},
		{"not_owner", func(a *DestinationAdmission) { a.OwnerAuthorized = false }, "owner_authorization_required"},
		{"not_enrolled", func(a *DestinationAdmission) { a.ReplicaEnrolled = false }, "replica_enrollment_required"},
		{"old_protocol", func(a *DestinationAdmission) { a.ProtocolCompatible = false }, "capability_required"},
		{"old_schema", func(a *DestinationAdmission) { a.SchemaCompatible = false }, "capability_required"},
		{"capacity", func(a *DestinationAdmission) { a.CapacityAvailable = false }, "capacity_refused"},
		{"trimmed_scope", func(a *DestinationAdmission) { a.CompleteScope = false }, "whole_account_scope_required"},
	} {
		t.Run(test.name, func(t *testing.T) {
			a := fixtureDestinationAdmission(SaaSDestination)
			test.mutate(&a)
			d, err := EvaluateDestinationAdmission(a)
			if err != nil || d.WholeAccountAllowed || !d.LocalMessaging || !d.PreservePending || d.Reason != test.want {
				t.Fatal("admission refusal lost local/pending state or silently trimmed scope")
			}
		})
	}
	for _, mode := range []DestinationMode{SelfManaged, SaaSDestination} {
		a := fixtureDestinationAdmission(mode)
		a.EmailOperated, a.EmailRouted, a.EmailCapacity = true, true, true
		d, err := EvaluateDestinationAdmission(a)
		if err != nil || d.ProviderDispatch != (mode == SelfManaged) {
			t.Fatal("self-managed operation and SaaS entitlement collapsed")
		}
		a.EmailEntitled = true
		d, err = EvaluateDestinationAdmission(a)
		if err != nil || !d.ProviderDispatch {
			t.Fatal("authoritative service policy refused")
		}
	}
	if _, err := EvaluateDestinationAdmission(DestinationAdmission{Mode: "unknown"}); err == nil {
		t.Fatal("unknown destination mode admitted")
	}
}

func fixtureReplicaGrant() (ReplicaGrant, GrantContinuation) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return ReplicaGrant{AccountID: "account", ReplicaID: "replica", Enrollment: "enrollment", IssuedAt: now, ExpiresAt: now.Add(DefaultOfflineGrant)}, GrantContinuation{Now: now.Add(time.Hour), LastObserved: now, ClockCertain: true, AuthoritativePermission: true, DeletionCatchup: true, RequestedLifetime: DefaultOfflineGrant, DeletionRetention: DefaultOfflineGrant + DeletionSafetyMargin, Pending: []OperationID{{1}, {2}}}
}

func TestGrantDeletionClockHorizon(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*ReplicaGrant, *GrantContinuation)
		want   RecoveryReason
	}{
		{"valid", func(_ *ReplicaGrant, _ *GrantContinuation) {}, ContinuationVerified},
		{"renewal_without_catchup", func(_ *ReplicaGrant, i *GrantContinuation) { i.Renew = true; i.DeletionCatchup = false }, RebootstrapRequired},
		{"clock_rollback", func(_ *ReplicaGrant, i *GrantContinuation) { i.Renew = true; i.LastObserved = i.Now.Add(time.Second) }, AuthorizationCoverageUnproven},
		{"uncertain_expiry", func(_ *ReplicaGrant, i *GrantContinuation) { i.Renew = true; i.ClockCertain = false }, AuthorizationCoverageUnproven},
		{"expired", func(g *ReplicaGrant, i *GrantContinuation) { i.Now = g.ExpiresAt }, AuthorizationCoverageUnproven},
		{"permission_revoked", func(_ *ReplicaGrant, i *GrantContinuation) { i.Renew = true; i.AuthoritativePermission = false }, AuthorizationCoverageUnproven},
		{"retired", func(g *ReplicaGrant, i *GrantContinuation) { g.Retired = true; i.Renew = true }, ReplicaReenrollmentRequired},
	} {
		t.Run(test.name, func(t *testing.T) {
			g, input := fixtureReplicaGrant()
			test.mutate(&g, &input)
			d, err := EvaluateGrantContinuation(g, input)
			if err != nil || d.Reason != test.want || d.CursorAllowed != (test.want == ContinuationVerified) || !d.IndependentLocalWork || !reflect.DeepEqual(d.Pending, input.Pending) || d.ExpiresAt != g.ExpiresAt {
				t.Fatal("grant continuation changed identity, extended uncertain grant or lost independent work")
			}
		})
	}
	g, input := fixtureReplicaGrant()
	input.Renew, input.Now = true, g.ExpiresAt.Add(time.Hour)
	d, err := EvaluateGrantContinuation(g, input)
	if err != nil || !d.CursorAllowed || d.ExpiresAt != input.Now.Add(DefaultOfflineGrant) {
		t.Fatal("verified permission and catch-up could not renew grant")
	}
	input.RequestedLifetime += time.Hour
	if _, err := EvaluateGrantContinuation(g, input); err == nil {
		t.Fatal("longer grant issued before longer deletion retention")
	}
	input.DeletionRetention += time.Hour
	if _, err := EvaluateGrantContinuation(g, input); err != nil {
		t.Fatal("extended retained deletion coverage did not permit reviewed lifetime")
	}
	g.Retired = true
	if _, err := ReenrollRetiredReplica(g, g.ReplicaID, "fresh-enrollment", input.Now); err == nil {
		t.Fatal("retired replica reused identity")
	}
	fresh, err := ReenrollRetiredReplica(g, "fresh-replica", "fresh-enrollment", input.Now)
	if err != nil || fresh.ReplicaID == g.ReplicaID || fresh.Enrollment == g.Enrollment || fresh.Retired {
		t.Fatal("reenrollment did not allocate independent identity")
	}
}

func TestUnresolvedBeyondHorizon(t *testing.T) {
	g, input := fixtureReplicaGrant()
	input.Now = g.IssuedAt.Add(60 * 24 * time.Hour)
	input.DeletionCatchup = false
	d, err := EvaluateGrantContinuation(g, input)
	if err != nil || d.CursorAllowed || !reflect.DeepEqual(d.Pending, input.Pending) {
		t.Fatal("expired grant settled pending operations")
	}
	var pins EvidencePins
	for _, kind := range []string{"identity", "evidence", "comparison_key", "signing_key", "deletion"} {
		if err := pins.Pin(g.AccountID, kind, "retained", "unresolved-operation"); err != nil {
			t.Fatal(err)
		}
		if pins.CanCollect(g.AccountID, kind, "retained", input.Now, g.IssuedAt.Add(37*24*time.Hour)) {
			t.Fatal("horizon collected unresolved dependency")
		}
	}
	// Rebuilding a projection changes neither acceptance nor the original IDs.
	e := fixtureRecoveryEvidence()
	e.Replica, e.HorizonExpired, e.EffectsRecovered = true, true, true
	recovery, err := EvaluateRecovery(fixtureRecoveryCase(), e)
	if err != nil || recovery.Case.Reason != AcceptanceAmbiguous || recovery.Case.Convergence || recovery.RetrySameOperationAllowed {
		t.Fatal("expired projection rebuild manufactured nonacceptance")
	}
	e.DeletionCoverage = false
	recovery, err = EvaluateRecovery(fixtureRecoveryCase(), e)
	if err != nil || recovery.Case.Reason != DeletionCoverageUnproven || recovery.RecoverMaterialization {
		t.Fatal("expired horizon bypassed missing deletion coverage")
	}
}
