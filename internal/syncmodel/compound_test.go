package syncmodel

import (
	"testing"
)

func compoundFixture(t *testing.T) (*CompoundStage, GraphAuthority, []Dependency, []CompoundFragment) {
	t.Helper()
	roles := []string{"graph", "candidate", "sibling_reservation", "claim", "notification"}
	m := CompoundManifest{GroupID: "group", RequestID: "request", AuthorityInstallationID: "installation", AuthorityEpoch: 2, ExpectedGraphRevision: 3, ResultGraphRevision: 4, OrderedMembers: []string{"operation"}, RequiredFragments: roles}
	d := []Dependency{{Kind: "generation", AccountID: "account", RealmID: "realm", OwnerID: "owner", ObjectID: "object", Generation: GenerationID{1}}}
	var fragments []CompoundFragment
	for _, role := range roles {
		fragments = append(fragments, CompoundFragment{ID: role, Kind: role, Payload: []byte("synthetic-" + role)})
	}
	var err error
	m.ManifestCommitment, err = SyntheticCompoundCommitment(m, fragments)
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewCompoundStage("account", m, d)
	if err != nil {
		t.Fatal(err)
	}
	return s, GraphAuthority{AccountID: "account", RequestID: "request", InstallationID: "installation", Epoch: 2, Revision: 3, PublicationPin: true}, d, fragments
}

func TestCompoundManifestBindsFirstSeenFragmentBytes(t *testing.T) {
	for _, corruption := range []string{"payload", "commitment", "role"} {
		t.Run(corruption, func(t *testing.T) {
			s, a, deps, fragments := compoundFixture(t)
			switch corruption {
			case "payload":
				fragments[0].Payload = []byte("altered-first-seen")
			case "commitment":
				s.manifest.ManifestCommitment[0] ^= 1
			case "role":
				fragments[0].Kind, fragments[1].Kind = fragments[1].Kind, fragments[0].Kind
			}
			for _, f := range fragments {
				if err := s.StageFragment(f); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.Publish(a, deps); err == nil || s.Inspect().Visible {
				t.Fatal("altered first-seen content published")
			}
		})
	}
	s, _, _, fragments := compoundFixture(t)
	original, err := SyntheticCompoundCommitment(s.manifest, fragments)
	if err != nil {
		t.Fatal(err)
	}
	fragments[0], fragments[4] = fragments[4], fragments[0]
	reordered, err := SyntheticCompoundCommitment(s.manifest, fragments)
	if err != nil || original != reordered {
		t.Fatal("delivery ordering changed declared fragment set")
	}
}

func TestCompoundNotificationBeforeLink(t *testing.T) {
	for _, adapter := range []string{"snapshot", "incremental"} {
		t.Run(adapter, func(t *testing.T) {
			s, authority, deps, fragments := compoundFixture(t)
			// Notification first, followed by separate batches. Inspection and
			// ordinary claim observations must be inert at each partial boundary.
			order := []int{4, 2, 3, 1, 0}
			for i, index := range order {
				if err := s.StageFragment(fragments[index]); err != nil {
					t.Fatal(err)
				}
				before := s.Inspect()
				for range 3 {
					if got := s.Inspect(); got != before || got.Visible || got.LinkVisible || got.NotificationVisible || got.OrdinarilyClaimable {
						t.Fatal("staging or inspection exposed partial graph")
					}
				}
				if i < len(order)-1 {
					if _, err := s.Publish(authority, deps); err == nil {
						t.Fatal("partial group published")
					}
				}
			}
			authority.PublicationPin = false
			if _, err := s.Publish(authority, deps); err == nil || s.Inspect().Visible {
				t.Fatal("complete but unpinned group published")
			}
			authority.PublicationPin = true
			got, err := s.Publish(authority, deps)
			if err != nil || !got.Visible || !got.LinkVisible || !got.NotificationVisible || got.OrdinarilyClaimable || got.Revision != 4 {
				t.Fatal("atomic compound projection failed")
			}
			if again, err := s.Publish(authority, deps); err != nil || again != got {
				t.Fatal("duplicate publication changed projection")
			}
		})
	}
}

func TestCompoundAuthorityDependenciesAndAlteredFragmentsRefuse(t *testing.T) {
	s, authority, deps, fragments := compoundFixture(t)
	for _, f := range fragments {
		if err := s.StageFragment(f); err != nil {
			t.Fatal(err)
		}
		if err := s.StageFragment(f); err != nil {
			t.Fatal("exact duplicate refused")
		}
	}
	changed := fragments[0]
	changed.Payload = []byte("changed")
	if err := s.StageFragment(changed); err == nil {
		t.Fatal("altered duplicate accepted")
	}
	for _, mutate := range []func(*GraphAuthority){
		func(a *GraphAuthority) { a.AccountID = "other" },
		func(a *GraphAuthority) { a.RequestID = "other" },
		func(a *GraphAuthority) { a.InstallationID = "other" },
		func(a *GraphAuthority) { a.Epoch++ },
		func(a *GraphAuthority) { a.Revision++ },
	} {
		bad := authority
		mutate(&bad)
		if _, err := s.Publish(bad, deps); err == nil || s.Inspect().Visible {
			t.Fatal("mismatched graph authority published")
		}
	}
	if _, err := s.Publish(authority, nil); err == nil {
		t.Fatal("missing dependency published")
	}
	badDeps := append([]Dependency{}, deps...)
	badDeps[0].Generation = GenerationID{2}
	if _, err := s.Publish(authority, badDeps); err == nil {
		t.Fatal("different dependency generation published")
	}
	if _, err := s.Publish(authority, deps); err != nil {
		t.Fatal("refusals destroyed staged work")
	}
}

func TestCompoundCopiesInputAndPreservesStagingAtCapacity(t *testing.T) {
	s, _, _, fragments := compoundFixture(t)
	f := fragments[0]
	f.Payload = make([]byte, MaxCompoundBytes)
	if err := s.StageFragment(f); err != nil {
		t.Fatal(err)
	}
	f.Payload[0] = 1
	if err := s.StageFragment(f); err == nil {
		t.Fatal("caller mutated persisted fragment")
	}
	if err := s.StageFragment(fragments[1]); err != ErrResourceCapacity {
		t.Fatal("compound payload bound not enforced")
	}
	f.Payload[0] = 0
	if err := s.StageFragment(f); err != nil {
		t.Fatal("capacity refusal destroyed queued fragment")
	}
}
