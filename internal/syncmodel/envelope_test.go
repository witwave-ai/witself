package syncmodel

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func fixtureEnvelope() OperationEnvelope {
	return OperationEnvelope{
		EnvelopeVersion: 1, SchemaVersion: 1, ProtocolVersion: 1, ID: OperationID{1},
		OriginalIncarnation: Incarnation{AccountID: "account-fixture", Lifecycle: 1, Restore: RestoreMarker{2}}, Capture: 1,
		Origin: Origin{InstallationID: "installation-fixture", ReplicaID: "replica-fixture", WriteLineageID: "lineage-fixture", OriginSequence: 1},
		Kind:   "fixture.write", Authorization: AuthorizationEvidence{PrincipalKind: PrincipalOperator, PrincipalID: "operator-fixture", RealmID: "realm-fixture", AccessProfileID: "profile-fixture", PolicyRevision: "policy-fixture", EnrollmentGeneration: "enrollment-fixture", GrantID: "grant-fixture"},
		Basis:   CausalBasis{Family: "facts_subjects_candidates_aliases", ObjectID: "object-fixture", Generation: GenerationID{3}, ExpectedAbsent: true, Expected: []RevisionID{}, Dependencies: []Dependency{}, DomainBasis: DomainValue(DomainTag("facts_subjects_candidates_aliases"), NullValue())},
		Payload: StringValue("synthetic-private-input"),
	}
}

type fixtureComparisonKeys struct{ missing bool }

func (k fixtureComparisonKeys) ComparisonKey(account string, id ComparisonKeyID) ([]byte, error) {
	if k.missing || id != "comparison-fixture" {
		return nil, errors.New("unavailable")
	}
	key := bytes.Repeat([]byte{7}, 32)
	if account != "account-fixture" {
		key[0] = 8
	}
	return key, nil
}

func TestEnvelopeAdmissionNegative(t *testing.T) {
	e := fixtureEnvelope()
	raw, err := e.CanonicalBytes()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeOperationEnvelope(raw); err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for name := range fields {
		t.Run("missing_"+name, func(t *testing.T) {
			copyFields := map[string]json.RawMessage{}
			for k, v := range fields {
				if k != name {
					copyFields[k] = v
				}
			}
			data, _ := json.Marshal(copyFields)
			if _, err := DecodeOperationEnvelope(data); err == nil {
				t.Fatal("missing field accepted")
			}
		})
	}
	tests := map[string]func(*OperationEnvelope){
		"version":             func(e *OperationEnvelope) { e.ProtocolVersion = 2 },
		"missing_origin":      func(e *OperationEnvelope) { e.Origin.ReplicaID = "" },
		"counter_bound":       func(e *OperationEnvelope) { e.Origin.OriginSequence = Ordinal(MaxCounter + 1) },
		"agent_impersonation": func(e *OperationEnvelope) { a := "agent-fixture"; e.Authorization.AgentID = &a },
		"missing_agent":       func(e *OperationEnvelope) { e.Authorization.PrincipalKind = PrincipalAgent },
		"unknown_principal":   func(e *OperationEnvelope) { e.Authorization.PrincipalKind = "unknown" },
		"causal_tag":          func(e *OperationEnvelope) { e.Basis.DomainBasis = DomainValue(DomainTag("realms"), NullValue()) },
		"free_causal_map": func(e *OperationEnvelope) {
			e.Basis.DomainBasis = DomainValue(DomainTag(e.Basis.Family), ObjectValue(Field{Name: "payload", Value: StringValue("secret")}))
		},
		"contradictory_cas":  func(e *OperationEnvelope) { e.Basis.Expected = []RevisionID{{4}} },
		"duplicate_revision": func(e *OperationEnvelope) { e.Basis.ExpectedAbsent = false; e.Basis.Expected = []RevisionID{{4}, {4}} },
		"cross_account_dependency": func(e *OperationEnvelope) {
			e.Basis.Dependencies = []Dependency{{Kind: "generation", AccountID: "other", RealmID: e.Authorization.RealmID, OwnerID: "owner", ObjectID: "object", Generation: GenerationID{5}}}
		},
		"omitted_payload": func(e *OperationEnvelope) { e.Payload = CanonicalTypedValue{} },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			bad := fixtureEnvelope()
			mutate(&bad)
			if _, err := bad.CanonicalBytes(); err == nil {
				t.Fatal("invalid envelope accepted")
			}
		})
	}
	for name, data := range map[string][]byte{
		"duplicate":               append([]byte(`{"operation_id":"`+e.ID.String()+`",`), raw[1:]...),
		"unknown":                 append([]byte(`{"unknown":true,`), raw[1:]...),
		"ambiguous_case":          append([]byte(`{"Operation_ID":"`+e.ID.String()+`",`), raw[1:]...),
		"null_boolean":            bytes.Replace(raw, []byte(`"expected_absent":true`), []byte(`"expected_absent":null`), 1),
		"missing_zero_coordinate": bytes.Replace(raw, []byte(`"origin_sequence":"1",`), nil, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeOperationEnvelope(data); err == nil {
				t.Fatal("malformed envelope accepted")
			}
		})
	}
}

func TestEnvelopeAuthorizationIsIndependent(t *testing.T) {
	e := fixtureEnvelope()
	scope := AdmissionScope{AccountID: e.OriginalIncarnation.AccountID, Origin: e.Origin, Authorization: e.Authorization, MayCompare: true, MayMutate: true}
	if err := AuthorizeAdmission(e, scope, false); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*AdmissionScope){func(s *AdmissionScope) { s.AccountID = "other" }, func(s *AdmissionScope) { s.Authorization.PrincipalID = "forged" }, func(s *AdmissionScope) { s.Origin.ReplicaID = "other" }, func(s *AdmissionScope) { s.MayCompare = false }, func(s *AdmissionScope) { s.MayMutate = false }} {
		bad := scope
		mutate(&bad)
		if err := AuthorizeAdmission(e, bad, false); err == nil {
			t.Fatal("forged scope authorized")
		}
	}
	scope.MayMutate = false
	if err := AuthorizeAdmission(e, scope, true); err != nil {
		t.Fatal("original authorized outcome lookup required mutation permission")
	}
}

func TestEnvelopeCommitmentCanonicalSetsAndSemanticBinding(t *testing.T) {
	e := fixtureEnvelope()
	e.Basis.ExpectedAbsent = false
	e.Basis.Expected = []RevisionID{{9}, {4}}
	e.Basis.Dependencies = []Dependency{{Kind: "generation", AccountID: e.OriginalIncarnation.AccountID, RealmID: e.Authorization.RealmID, OwnerID: "owner", ObjectID: "object-b", Generation: GenerationID{7}}, {Kind: "generation", AccountID: e.OriginalIncarnation.AccountID, RealmID: e.Authorization.RealmID, OwnerID: "owner", ObjectID: "object-a", Generation: GenerationID{8}}}
	p := fixtureComparisonKeys{}
	original, err := InputCommitment(p, e.OriginalIncarnation.AccountID, "comparison-fixture", e)
	if err != nil {
		t.Fatal(err)
	}
	reordered := e
	reordered.Basis.Expected = []RevisionID{{4}, {9}}
	reordered.Basis.Dependencies = []Dependency{e.Basis.Dependencies[1], e.Basis.Dependencies[0]}
	same, err := InputCommitment(p, e.OriginalIncarnation.AccountID, "comparison-fixture", reordered)
	if err != nil || same != original {
		t.Fatal("declared set order changed semantic commitment")
	}
	for _, mutate := range []func(*OperationEnvelope){func(e *OperationEnvelope) { e.Payload = StringValue("changed") }, func(e *OperationEnvelope) { e.Origin.OriginSequence++ }, func(e *OperationEnvelope) { e.Capture++ }, func(e *OperationEnvelope) { e.OriginalIncarnation.Lifecycle++ }, func(e *OperationEnvelope) { e.Authorization.PolicyRevision = "policy-other" }, func(e *OperationEnvelope) { e.Basis.ObjectID = "object-other" }, func(e *OperationEnvelope) { e.ID[0]++ }} {
		changed := e
		mutate(&changed)
		got, err := InputCommitment(p, e.OriginalIncarnation.AccountID, "comparison-fixture", changed)
		if err != nil || got == original {
			t.Fatal("semantic mutation did not change commitment")
		}
	}
	if _, err := InputCommitment(fixtureComparisonKeys{missing: true}, e.OriginalIncarnation.AccountID, "comparison-fixture", e); err == nil {
		t.Fatal("missing comparison key accepted")
	}
	if _, err := InputCommitment(p, "other-account", "comparison-fixture", e); err == nil {
		t.Fatal("cross-account comparison accepted")
	}
}

func TestBoundedMalformedAdmission(t *testing.T) {
	for _, n := range []int{MaxDependencies - 1, MaxDependencies, MaxDependencies + 1} {
		e := fixtureEnvelope()
		for i := 0; i < n; i++ {
			e.Basis.Expected = append(e.Basis.Expected, RevisionID{byte(i)})
		}
		e.Basis.ExpectedAbsent = false
		_, err := e.CanonicalBytes()
		if (err == nil) != (n <= MaxDependencies) {
			t.Fatal("dependency boundary mismatch")
		}
	}
	e := fixtureEnvelope()
	e.Payload = StringValue("")
	base, err := e.CanonicalBytes()
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{MaxEnvelopeBytes - 1, MaxEnvelopeBytes, MaxEnvelopeBytes + 1} {
		e.Payload = StringValue(strings.Repeat("x", n-len(base)))
		raw, err := e.CanonicalBytes()
		if n <= MaxEnvelopeBytes {
			if err != nil || len(raw) != n {
				t.Fatal("envelope boundary refused")
			}
			if _, err := DecodeOperationEnvelope(raw); err != nil {
				t.Fatal(err)
			}
		} else if err == nil {
			t.Fatal("oversized envelope accepted")
		}
	}
	if _, err := DecodeOperationEnvelope([]byte("not an identity")); err == nil {
		t.Fatal("garbage manufactured an operation")
	}
	if _, err := DecodeOperationEnvelope(bytes.Repeat([]byte{'x'}, MaxEnvelopeBytes+1)); err == nil {
		t.Fatal("oversized transport accepted")
	}
}

func TestDecodeOperationEnvelopeByteBoundary(t *testing.T) {
	e := fixtureEnvelope()
	e.Payload = StringValue("")
	base, err := e.CanonicalBytes()
	if err != nil {
		t.Fatal("cannot encode envelope boundary fixture")
	}
	const emptyPayload = `"payload":{"t":"string","v":1,"value":""}`
	if bytes.Count(base, []byte(emptyPayload)) != 1 {
		t.Fatal("envelope boundary fixture has no unique empty payload")
	}
	for _, tc := range []struct {
		name string
		size int
	}{
		{"below", MaxEnvelopeBytes - 1},
		{"at", MaxEnvelopeBytes},
		{"above", MaxEnvelopeBytes + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Pad the canonical wire directly so the encoder's limit cannot
			// hide the complete-envelope decoder's oversized-input boundary.
			payload := []byte(`"payload":{"t":"string","v":1,"value":"` + strings.Repeat("x", tc.size-len(base)) + `"}`)
			wire := bytes.Replace(base, []byte(emptyPayload), payload, 1)
			if len(wire) != tc.size {
				t.Fatal("envelope decoder boundary fixture has the wrong length")
			}
			decoded, err := DecodeOperationEnvelope(wire)
			if tc.size > MaxEnvelopeBytes {
				if err == nil {
					t.Fatal("public envelope decoder accepted oversized canonical wire")
				}
				return
			}
			if err != nil {
				t.Fatal("public envelope decoder refused in-bound canonical wire")
			}
			canonical, err := decoded.CanonicalBytes()
			if err != nil || !bytes.Equal(canonical, wire) {
				t.Fatal("envelope decoder boundary input did not preserve canonical wire")
			}
		})
	}
}

func TestCompoundManifestOrderedMembersAndFragmentSet(t *testing.T) {
	e := fixtureEnvelope()
	e.Compound = &CompoundManifest{GroupID: "group", RequestID: "request", AuthorityInstallationID: "installation", AuthorityEpoch: 1, ExpectedGraphRevision: 1, ResultGraphRevision: 2, OrderedMembers: []string{"one", "two"}, RequiredFragments: []string{"b", "a"}}
	first, err := e.CanonicalBytes()
	if err != nil {
		t.Fatal(err)
	}
	e.Compound.RequiredFragments = []string{"a", "b"}
	same, err := e.CanonicalBytes()
	if err != nil || !bytes.Equal(first, same) {
		t.Fatal("fragment set changed bytes")
	}
	e.Compound.OrderedMembers = []string{"two", "one"}
	changed, err := e.CanonicalBytes()
	if err != nil || bytes.Equal(first, changed) {
		t.Fatal("ordered members lost order")
	}
	e.Compound.RequiredFragments = []string{"a", "a"}
	if err := e.Compound.Validate(); err == nil {
		t.Fatal("duplicate fragment accepted")
	}
}
