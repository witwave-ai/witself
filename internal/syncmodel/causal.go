package syncmodel

import (
	"bytes"
	"errors"
	"sort"
)

// MaxDependencies bounds each declared fixture dependency or expected-revision set.
const MaxDependencies = 64

// MaxFragments bounds compound members and required fragment identifiers.
const MaxFragments = 64

// Dependency names an immutable prerequisite in its complete ownership scope.
// These kinds are fixture protocol vocabulary, not installed domain adapters.
type Dependency struct {
	Kind       string       `json:"kind"`
	AccountID  string       `json:"account_id"`
	RealmID    string       `json:"realm_id"`
	OwnerID    string       `json:"owner_id"`
	ObjectID   string       `json:"object_id"`
	Generation GenerationID `json:"generation"`
	Revision   *RevisionID  `json:"revision"`
	Operation  *OperationID `json:"operation"`
}

// Validate checks the closed dependency kind and its complete ownership scope.
func (d Dependency) Validate() error {
	if d.Kind != "revision" && d.Kind != "operation" && d.Kind != "generation" {
		return errors.New("invalid dependency kind")
	}
	for _, s := range []string{d.AccountID, d.RealmID, d.OwnerID, d.ObjectID} {
		if !validLabel(s) {
			return errors.New("invalid dependency scope")
		}
	}
	if (d.Kind == "revision") != (d.Revision != nil) || (d.Kind == "operation") != (d.Operation != nil) {
		return errors.New("invalid dependency identity")
	}
	return nil
}

// CausalBasis binds a fixture operation to immutable generation and revision evidence.
type CausalBasis struct {
	Family             string              `json:"family"`
	ObjectID           string              `json:"object_id"`
	Generation         GenerationID        `json:"generation"`
	ExpectedAbsent     bool                `json:"expected_absent"`
	Expected           []RevisionID        `json:"expected"`
	Dependencies       []Dependency        `json:"dependencies"`
	DeletionGeneration *GenerationID       `json:"deletion_generation"`
	RecreationIntentID *string             `json:"recreation_intent_id"`
	DomainBasis        CanonicalTypedValue `json:"domain_basis"`
}

// Validate rejects conflicting expectations, duplicate sets and untyped family evidence.
func (b CausalBasis) Validate() error {
	if !validFamily(b.Family) || !validLabel(b.ObjectID) {
		return errors.New("invalid causal scope")
	}
	if b.Expected == nil || b.Dependencies == nil || len(b.Expected) > MaxDependencies || len(b.Dependencies) > MaxDependencies {
		return errors.New("invalid causal bounds")
	}
	if b.ExpectedAbsent && len(b.Expected) > 0 {
		return errors.New("contradictory causal basis")
	}
	if (b.DeletionGeneration == nil) != (b.RecreationIntentID == nil) {
		return errors.New("incomplete recreation basis")
	}
	if b.RecreationIntentID != nil && !validLabel(*b.RecreationIntentID) {
		return errors.New("invalid recreation identity")
	}
	seen := make(map[RevisionID]bool, len(b.Expected))
	for _, id := range b.Expected {
		if seen[id] {
			return errors.New("duplicate expected revision")
		}
		seen[id] = true
	}
	deps := make(map[string]bool, len(b.Dependencies))
	for _, d := range b.Dependencies {
		if err := d.Validate(); err != nil {
			return err
		}
		raw, err := MarshalCanonical(d)
		if err != nil {
			return err
		}
		if deps[string(raw)] {
			return errors.New("duplicate dependency")
		}
		deps[string(raw)] = true
	}
	if err := b.DomainBasis.Validate(); err != nil {
		return err
	}
	tag, ok := b.DomainBasis.DomainTag()
	if !ok || string(tag) != b.Family {
		return errors.New("invalid family causal tag")
	}
	inner, ok := b.DomainBasis.DomainContent()
	if !ok {
		return errors.New("invalid family causal basis")
	}
	return validateIdentityBasis(inner)
}

func (b CausalBasis) canonical() CausalBasis {
	b.Expected = append([]RevisionID{}, b.Expected...)
	sort.Slice(b.Expected, func(i, j int) bool { return bytes.Compare(b.Expected[i][:], b.Expected[j][:]) < 0 })
	b.Dependencies = append([]Dependency{}, b.Dependencies...)
	sort.Slice(b.Dependencies, func(i, j int) bool {
		a, _ := MarshalCanonical(b.Dependencies[i])
		c, _ := MarshalCanonical(b.Dependencies[j])
		return bytes.Compare(a, c) < 0
	})
	return b
}

// CompoundManifest preserves ordered operation members and canonicalizes only
// RequiredFragments, which the profile declares a set.
type CompoundManifest struct {
	GroupID                 string        `json:"group_id"`
	RequestID               string        `json:"request_id"`
	AuthorityInstallationID string        `json:"authority_installation_id"`
	AuthorityEpoch          GraphEpoch    `json:"authority_epoch"`
	ExpectedGraphRevision   GraphRevision `json:"expected_graph_revision"`
	ResultGraphRevision     GraphRevision `json:"result_graph_revision"`
	OrderedMembers          []string      `json:"ordered_members"`
	RequiredFragments       []string      `json:"required_fragments"`
	ManifestCommitment      Commitment    `json:"manifest_commitment"`
}

// Validate checks compound bounds, unique members and an advancing graph revision.
func (m CompoundManifest) Validate() error {
	for _, s := range []string{m.GroupID, m.RequestID, m.AuthorityInstallationID} {
		if !validLabel(s) {
			return errors.New("invalid compound scope")
		}
	}
	if err := m.AuthorityEpoch.Validate(); err != nil {
		return err
	}
	if err := m.ExpectedGraphRevision.Validate(); err != nil {
		return err
	}
	if err := m.ResultGraphRevision.Validate(); err != nil {
		return err
	}
	if m.ResultGraphRevision <= m.ExpectedGraphRevision {
		return errors.New("invalid compound revision")
	}
	for _, items := range [][]string{m.OrderedMembers, m.RequiredFragments} {
		if len(items) == 0 || len(items) > MaxFragments {
			return errors.New("invalid compound bounds")
		}
		seen := make(map[string]bool, len(items))
		for _, s := range items {
			if !validLabel(s) || seen[s] {
				return errors.New("invalid compound membership")
			}
			seen[s] = true
		}
	}
	return nil
}

func (m CompoundManifest) canonical() CompoundManifest {
	m.OrderedMembers = append([]string{}, m.OrderedMembers...)
	m.RequiredFragments = append([]string{}, m.RequiredFragments...)
	sort.Strings(m.RequiredFragments)
	return m
}

func validFamily(s string) bool {
	switch s {
	case "accounts_operators_agents", "realms", "facts_subjects_candidates_aliases", "memories_capture_curation_vectors_deletion", "transcripts", "messages_complete_requests", "inbound_outbound_email", "secrets_vault", "avatars", "preferences_consent", "usage_billing", "backups_exports_imports":
		return true
	}
	return false
}
