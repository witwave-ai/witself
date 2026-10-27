package syncmodel

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
)

// CompoundFragment contains synthetic staging data, never a visible message.
type CompoundFragment struct {
	ID      string
	Kind    string
	Payload []byte
}

// SyntheticCompoundCommitment binds the manifest and length-framed fragment
// identities, roles and bytes. It hashes public synthetic fixtures only. It is
// not a protected commitment for private production payloads, nor does it prove
// who supplied the expected manifest; adapters authenticate that separately.
func SyntheticCompoundCommitment(manifest CompoundManifest, fragments []CompoundFragment) (Commitment, error) {
	if err := manifest.Validate(); err != nil {
		return Commitment{}, err
	}
	manifest = manifest.canonical()
	manifest.ManifestCommitment = Commitment{}
	if len(fragments) != len(manifest.RequiredFragments) {
		return Commitment{}, errors.New("compound commitment incomplete")
	}
	byID := make(map[string]CompoundFragment, len(fragments))
	size := 0
	for _, f := range fragments {
		if !validLabel(f.ID) || !validLabel(f.Kind) {
			return Commitment{}, errors.New("invalid compound fragment identity")
		}
		if _, ok := byID[f.ID]; ok {
			return Commitment{}, errors.New("duplicate compound fragment identity")
		}
		if len(f.Payload) > MaxCompoundBytes-size {
			return Commitment{}, ErrResourceCapacity
		}
		size += len(f.Payload)
		byID[f.ID] = f
	}
	body, err := MarshalCanonical(manifest)
	if err != nil {
		return Commitment{}, err
	}
	h := sha256.New()
	_, _ = h.Write([]byte("witself.sync.synthetic-compound.v1\x00"))
	frame := func(raw []byte) {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(raw)))
		_, _ = h.Write(length[:])
		_, _ = h.Write(raw)
	}
	frame(body)
	for _, id := range manifest.RequiredFragments {
		f, ok := byID[id]
		if !ok {
			return Commitment{}, errors.New("compound commitment incomplete")
		}
		frame([]byte(f.ID))
		frame([]byte(f.Kind))
		frame(f.Payload)
	}
	var commitment Commitment
	copy(commitment[:], h.Sum(nil))
	return commitment, nil
}

// GraphAuthority is independently verified authority supplied to publication.
type GraphAuthority struct {
	AccountID      string
	RequestID      string
	InstallationID string
	Epoch          GraphEpoch
	Revision       GraphRevision
	PublicationPin bool
}

// CompoundProjection is the atomic miniature settled-visibility boundary. SQL
// may expose inert pending projections earlier, but this conservative model
// exposes only pinned results. A protocol notification is never ordinarily claimable.
type CompoundProjection struct {
	Visible             bool
	LinkVisible         bool
	NotificationVisible bool
	OrdinarilyClaimable bool
	Revision            GraphRevision
}

// CompoundStage keeps complete graph operations invisible until a verified
// publication boundary. It represents one group, not a cross-store driver.
type CompoundStage struct {
	account      string
	manifest     CompoundManifest
	dependencies map[string]bool
	fragments    map[string]CompoundFragment
	bytes        int
	projection   CompoundProjection
}

// NewCompoundStage freezes the declared manifest and dependency set.
func NewCompoundStage(account string, manifest CompoundManifest, dependencies []Dependency) (*CompoundStage, error) {
	if !validLabel(account) || dependencies == nil || len(dependencies) > MaxDependencies {
		return nil, errors.New("invalid compound staging scope")
	}
	if err := manifest.Validate(); err != nil {
		return nil, err
	}
	deps, err := compoundDependencies(account, dependencies)
	if err != nil {
		return nil, err
	}
	return &CompoundStage{account: account, manifest: manifest.canonical(), dependencies: deps, fragments: make(map[string]CompoundFragment)}, nil
}

func compoundDependencies(account string, dependencies []Dependency) (map[string]bool, error) {
	set := make(map[string]bool, len(dependencies))
	for _, d := range dependencies {
		if err := d.Validate(); err != nil || d.AccountID != account {
			return nil, errors.New("invalid compound dependency")
		}
		b, err := MarshalCanonical(d)
		if err != nil || set[string(b)] {
			return nil, errors.New("invalid compound dependency")
		}
		set[string(b)] = true
	}
	return set, nil
}

// StageFragment persists a copy in the model. Exact duplicates are harmless;
// altered duplicates and unknown fragments refuse without replacing staged work.
func (s *CompoundStage) StageFragment(f CompoundFragment) error {
	if s == nil || s.projection.Visible {
		return errors.New("compound staging closed")
	}
	known := false
	for _, id := range s.manifest.RequiredFragments {
		known = known || id == f.ID
	}
	switch f.Kind {
	case "graph", "candidate", "sibling_reservation", "claim", "notification":
	default:
		return errors.New("invalid compound fragment kind")
	}
	if !known {
		return errors.New("unknown compound fragment")
	}
	if old, ok := s.fragments[f.ID]; ok {
		if old.Kind != f.Kind || !bytes.Equal(old.Payload, f.Payload) {
			return errors.New("compound fragment conflict")
		}
		return nil
	}
	if len(s.fragments) >= MaxFragments || len(f.Payload) > MaxCompoundBytes-s.bytes {
		return ErrResourceCapacity
	}
	f.Payload = bytes.Clone(f.Payload)
	s.fragments[f.ID] = f
	s.bytes += len(f.Payload)
	return nil
}

// Publish validates every fragment, all dependencies and exact graph authority.
// A missing witness pin leaves the complete staged group invisible. Snapshot
// and incremental adapters deliberately call this same boundary.
func (s *CompoundStage) Publish(a GraphAuthority, dependencies []Dependency) (CompoundProjection, error) {
	if s == nil {
		return CompoundProjection{}, errors.New("compound unavailable")
	}
	if a.AccountID != s.account || a.RequestID != s.manifest.RequestID || a.InstallationID != s.manifest.AuthorityInstallationID || a.Epoch != s.manifest.AuthorityEpoch || a.Revision != s.manifest.ExpectedGraphRevision {
		return CompoundProjection{}, errors.New("compound authority mismatch")
	}
	if s.projection.Visible {
		return s.projection, nil
	}
	if len(s.fragments) != len(s.manifest.RequiredFragments) {
		return CompoundProjection{}, errors.New("compound incomplete")
	}
	kinds := make(map[string]bool)
	for _, f := range s.fragments {
		kinds[f.Kind] = true
	}
	for _, kind := range []string{"graph", "candidate", "sibling_reservation", "claim", "notification"} {
		if !kinds[kind] {
			return CompoundProjection{}, errors.New("compound membership incomplete")
		}
	}
	fragments := make([]CompoundFragment, 0, len(s.fragments))
	for _, f := range s.fragments {
		fragments = append(fragments, f)
	}
	commitment, err := SyntheticCompoundCommitment(s.manifest, fragments)
	if err != nil || commitment != s.manifest.ManifestCommitment {
		return CompoundProjection{}, errors.New("compound manifest commitment mismatch")
	}
	deps, err := compoundDependencies(s.account, dependencies)
	if err != nil || len(deps) != len(s.dependencies) {
		return CompoundProjection{}, errors.New("compound dependencies incomplete")
	}
	for dep := range s.dependencies {
		if !deps[dep] {
			return CompoundProjection{}, errors.New("compound dependencies incomplete")
		}
	}
	if !a.PublicationPin {
		return CompoundProjection{}, errors.New("durability_publication_pending")
	}
	s.projection = CompoundProjection{Visible: true, LinkVisible: true, NotificationVisible: true, Revision: s.manifest.ResultGraphRevision}
	return s.projection, nil
}

// Inspect returns projection data only and performs no reconciliation.
func (s *CompoundStage) Inspect() CompoundProjection { return s.projection }
