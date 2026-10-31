package trust

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func verifierFixtureKey(t *testing.T, label string) (*rsa.PrivateKey, PublicKey) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "keys", "TEST-ONLY-"+label+".private.pem"))
	if err != nil {
		t.Fatal("fixture key read failed")
	}
	block, rest := pem.Decode(raw)
	if block == nil || len(rest) != 0 || !strings.Contains(block.Type, "TEST-ONLY") {
		t.Fatal("invalid fixture PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		t.Fatal("invalid fixture private key")
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		t.Fatal("non-RSA fixture key")
	}
	public, err := PublicKeyFromRSA(&key.PublicKey)
	if err != nil {
		t.Fatal("invalid fixture public key")
	}
	return key, public
}

func verifierFixtureSet() ComponentSet {
	components := []Component{}
	for _, role := range []string{"app", "service_host", "server", "worker", "postgresql"} {
		version := "1.0.0"
		if role == "postgresql" {
			version = "16.4"
		}
		path := "bin/" + role
		c := Component{Role: role, Version: version, Archive: Archive{Name: role + ".tar.gz", Size: 100, SHA256: digestBytes([]byte(role)), Format: "tar.gz"}, Inventory: []InventoryEntry{{Path: path, Size: 1, SHA256: digestBytes([]byte("x")), Mode: "0755", Executable: true}}, DependsOn: []string{}, RequiredExtensions: []Extension{}, Schema: Schema{Target: 1, AcceptedMin: 1, AcceptedMax: 1}, Ownership: Ownership{PackageOwnedPaths: []string{}, PrivatePayloadPaths: []string{path}}}
		if role == "postgresql" {
			c.PostgreSQL = &PostgreSQL{Major: 16, Version: version, Distribution: "fixture", Build: "fixture", RuntimeLibraries: []string{}, DataLayoutVersion: 1}
		}
		components = append(components, c)
	}
	return ComponentSet{FormatVersion: 1, Critical: []string{}, Kind: "component_set", RootSequence: 1, DelegationSequence: 1, ReleaseSequence: 1, ReleaseTag: "v1.0.0", Channel: "stable", IssuedAt: "2026-01-02T00:00:00Z", ExpiresAt: "2026-12-01T00:00:00Z", Source: Source{Repository: "fixture", Commit: strings.Repeat("a", 40)}, Provenance: Provenance{ChecksumsSHA256: digestBytes([]byte("checksums")), SigstoreBundle: "checksums.sigstore.json"}, License: "LICENSE", SBOM: "sbom.json", Rollback: Rollback{AllowedTargets: []uint64{}}, Platforms: []Platform{{OS: "linux", Arch: "amd64", MinOS: "fixture", GlibcFloor: "2.31", Components: components}}}
}

func verifierSign(t *testing.T, document any, kind string, keys ...*rsa.PrivateKey) ([]byte, []byte) {
	t.Helper()
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal("marshal fixture failed")
	}
	envelope, err := SignDocument(raw, kind, keys...)
	if err != nil {
		t.Fatalf("sign fixture failed: %s", reasonOf(err))
	}
	return raw, envelope
}

func verifierOptions(root PublicKey, state TrustState) Options {
	return Options{Roots: []PublicKey{root}, State: state, Clock: Clock{Now: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), Trusted: true}}
}

func verifierDelegate(t *testing.T, v *Verifier, root *rsa.PrivateKey, release PublicKey, mutate func(*Delegation)) {
	t.Helper()
	d := Delegation{FormatVersion: 1, Critical: []string{}, Kind: "delegation", RootSequence: 1, DelegationSequence: 1, IssuedAt: "2026-01-01T00:00:00Z", ExpiresAt: "2027-01-01T00:00:00Z", Keys: []PublicKey{release}, RevokedKeyIDs: []string{}}
	if mutate != nil {
		mutate(&d)
	}
	raw, envelope := verifierSign(t, d, "delegation", root)
	if out := v.VerifyDelegation(raw, envelope); !out.Accepted {
		t.Fatalf("delegation rejected: %s", out.Reason)
	}
}

func verifierAssertReason(t *testing.T, out Outcome, expected Reason) {
	t.Helper()
	if out.Reason != expected || out.Accepted != (expected == ReasonAccepted) {
		t.Fatalf("outcome accepted=%t reason=%s; expected %s", out.Accepted, out.Reason, expected)
	}
}

func TestProductionRootsEmptyAndFixturesUnauthorized(t *testing.T) {
	if len(ProductionRoots()) != 0 {
		t.Fatal("production roots must remain empty")
	}
	for _, label := range []string{"root-a", "root-b", "release-1", "release-2", "revoked-1"} {
		_, fixture := verifierFixtureKey(t, label)
		for _, production := range ProductionRoots() {
			if fixture.KeyID == production.KeyID {
				t.Fatal("fixture became production authority")
			}
		}
	}
	v := NewVerifier(Options{})
	for _, verify := range []func([]byte, []byte) Outcome{v.VerifyRotation, v.VerifyDelegation, v.VerifyComponentSet, v.AcceptForAcquisition, v.AcceptForOfflineStart} {
		verifierAssertReason(t, verify([]byte("untrusted"), nil), ReasonNoTrustRoot)
	}
	verifierAssertReason(t, v.AcceptSet(nil, nil, nil), ReasonNoTrustRoot)
}

func TestVerifierAcquisitionDoesNotCommitOfflineInstallation(t *testing.T) {
	root, public := verifierFixtureKey(t, "root-a")
	release, releasePublic := verifierFixtureKey(t, "release-1")
	state := NewMemoryState()
	v := NewVerifier(verifierOptions(public, state))
	verifierDelegate(t, v, root, releasePublic, nil)
	raw, envelope := verifierSign(t, verifierFixtureSet(), "component_set", release)
	verifierAssertReason(t, v.VerifyComponentSet(raw, envelope), ReasonAccepted)
	s, err := state.Load()
	if err != nil || len(s.Releases) != 0 {
		t.Fatal("pure verification mutated release state")
	}
	verifierAssertReason(t, v.AcceptForAcquisition(raw, envelope), ReasonAccepted)
	verifierAssertReason(t, v.AcceptForOfflineStart(raw, envelope), ReasonNotInstalled)
	parsed, err := ParseComponentSet(raw)
	if err != nil {
		t.Fatal("valid fixture failed to parse")
	}
	verifierAssertReason(t, v.AcceptSet(raw, envelope, parsed.Platforms[0].Components), ReasonAccepted)
	verifierAssertReason(t, v.AcceptForOfflineStart(raw, envelope), ReasonAccepted)
	opts := verifierOptions(public, state)
	opts.ExpectedTag = "v9.9.9"
	verifierAssertReason(t, NewVerifier(opts).AcceptForOfflineStart(raw, envelope), ReasonWrongTag)
	opts.ExpectedTag = ""
	opts.OS = "windows"
	opts.Arch = "amd64"
	verifierAssertReason(t, NewVerifier(opts).AcceptForOfflineStart(raw, envelope), ReasonWrongPlatform)
}

func TestVerifierOfflineStartSurvivesExpiryRevocationAndRestart(t *testing.T) {
	root, public := verifierFixtureKey(t, "root-a")
	release, releasePublic := verifierFixtureKey(t, "release-1")
	state := NewMemoryState()
	v := NewVerifier(verifierOptions(public, state))
	verifierDelegate(t, v, root, releasePublic, nil)
	raw, envelope := verifierSign(t, verifierFixtureSet(), "component_set", release)
	parsed, err := ParseComponentSet(raw)
	if err != nil {
		t.Fatal("valid fixture failed to parse")
	}
	verifierAssertReason(t, v.AcceptSet(raw, envelope, parsed.Platforms[0].Components), ReasonAccepted)
	verifierDelegate(t, v, root, releasePublic, func(d *Delegation) { d.DelegationSequence = 2; d.RevokedKeyIDs = []string{releasePublic.KeyID} })
	verifierAssertReason(t, v.AcceptForAcquisition(raw, envelope), ReasonRevokedKey)
	opts := verifierOptions(public, state)
	opts.Clock.Now = time.Date(2028, 1, 1, 0, 0, 0, 0, time.UTC)
	restarted := NewVerifier(opts)
	out := restarted.AcceptForOfflineStart(raw, envelope)
	verifierAssertReason(t, out, ReasonAccepted)
	if out.Freshness != Expired {
		t.Fatal("offline outcome hid expiry")
	}
	opts.Clock.Trusted = false
	out = NewVerifier(opts).AcceptForOfflineStart(raw, envelope)
	verifierAssertReason(t, out, ReasonAccepted)
	if out.Freshness != UnknownClock {
		t.Fatal("offline outcome hid clock uncertainty")
	}
	verifierAssertReason(t, restarted.AcceptForOfflineStart(append(append([]byte{}, raw...), ' '), envelope), ReasonNotInstalled)
	verifierAssertReason(t, restarted.AcceptForOfflineStart(raw, append(append([]byte{}, envelope...), ' ')), ReasonNotInstalled)
}

func TestVerifierReplayEquivocationAndExplicitRollbackHighWater(t *testing.T) {
	root, public := verifierFixtureKey(t, "root-a")
	release, releasePublic := verifierFixtureKey(t, "release-1")
	state := NewMemoryState()
	v := NewVerifier(verifierOptions(public, state))
	verifierDelegate(t, v, root, releasePublic, nil)
	set := verifierFixtureSet()
	set.ReleaseSequence = 3
	set.ReleaseTag = "v3.0.0"
	set.Rollback.AllowedTargets = []uint64{1}
	raw, envelope := verifierSign(t, set, "component_set", release)
	verifierAssertReason(t, v.AcceptForAcquisition(raw, envelope), ReasonAccepted)
	verifierAssertReason(t, v.AcceptForAcquisition(raw, envelope), ReasonAccepted)
	set.ReleaseTag = "v3.0.1"
	changed, changedEnvelope := verifierSign(t, set, "component_set", release)
	verifierAssertReason(t, v.AcceptForAcquisition(changed, changedEnvelope), ReasonReplay)
	lower, lowerEnvelope := verifierSign(t, verifierFixtureSet(), "component_set", release)
	verifierAssertReason(t, v.AcceptForAcquisition(lower, lowerEnvelope), ReasonReplay)
	opts := verifierOptions(public, state)
	opts.AllowRollback = true
	rollback := NewVerifier(opts)
	verifierAssertReason(t, rollback.AcceptForAcquisition(lower, lowerEnvelope), ReasonAccepted)
	set = verifierFixtureSet()
	set.ReleaseSequence = 2
	set.ReleaseTag = "v2.0.0"
	denied, deniedEnvelope := verifierSign(t, set, "component_set", release)
	verifierAssertReason(t, rollback.AcceptForAcquisition(denied, deniedEnvelope), ReasonRollbackDenied)
	s, err := state.Load()
	if err != nil || s.Releases["stable"].Sequence != 3 || s.Releases["stable"].Tag != "v3.0.0" {
		t.Fatal("rollback lowered or changed high-water record")
	}
}

func TestVerifierDelegationEquivocationAndReplay(t *testing.T) {
	root, public := verifierFixtureKey(t, "root-a")
	_, releasePublic := verifierFixtureKey(t, "release-1")
	_, release2Public := verifierFixtureKey(t, "release-2")
	state := NewMemoryState()
	v := NewVerifier(verifierOptions(public, state))
	verifierDelegate(t, v, root, releasePublic, nil)
	verifierDelegate(t, v, root, releasePublic, nil)
	d := Delegation{FormatVersion: 1, Critical: []string{}, Kind: "delegation", RootSequence: 1, DelegationSequence: 1, IssuedAt: "2026-01-01T00:00:00Z", ExpiresAt: "2027-01-01T00:00:00Z", Keys: []PublicKey{release2Public}, RevokedKeyIDs: []string{}}
	raw, envelope := verifierSign(t, d, "delegation", root)
	verifierAssertReason(t, v.VerifyDelegation(raw, envelope), ReasonReplay)
	verifierDelegate(t, v, root, releasePublic, func(d *Delegation) { d.DelegationSequence = 2 })
	verifierAssertReason(t, v.VerifyDelegation(raw, envelope), ReasonReplay)
	s, err := state.Load()
	if err != nil || s.DelegationSequence != 2 || s.Delegation.Keys[0].KeyID != releasePublic.KeyID {
		t.Fatal("refused delegation mutated state")
	}
}

func TestVerifierRotationPossessionAndInterruptedState(t *testing.T) {
	rootA, publicA := verifierFixtureKey(t, "root-a")
	rootB, publicB := verifierFixtureKey(t, "root-b")
	_, releasePublic := verifierFixtureKey(t, "release-1")
	state := NewMemoryState()
	v := NewVerifier(verifierOptions(publicA, state))
	verifierDelegate(t, v, rootA, releasePublic, nil)
	rotation := RootRotation{FormatVersion: 1, Critical: []string{}, Kind: "root_rotation", RootSequence: 1, NewSequence: 2, IssuedAt: "2026-01-01T00:00:00Z", ExpiresAt: "2027-01-01T00:00:00Z", NewRoot: publicB}
	raw, envelope := verifierSign(t, rotation, "root_rotation", rootA, rootB)
	var parsed Envelope
	if err := json.Unmarshal(envelope, &parsed); err != nil {
		t.Fatal("fixture envelope parse failed")
	}
	parsed.Signatures[1].Signature = parsed.Signatures[0].Signature
	broken, err := json.Marshal(parsed)
	if err != nil {
		t.Fatal("fixture envelope marshal failed")
	}
	verifierAssertReason(t, v.VerifyRotation(raw, broken), ReasonSignatureInvalid)
	s, err := state.Load()
	if err != nil || s.RootSequence != 1 || s.Root.KeyID != publicA.KeyID || s.Delegation == nil {
		t.Fatal("failed rotation changed authority")
	}
	verifierAssertReason(t, v.VerifyRotation(raw, envelope), ReasonAccepted)
	s, err = state.Load()
	if err != nil || s.RootSequence != 2 || s.Root.KeyID != publicB.KeyID || s.Delegation != nil || s.DelegationSequence != 1 {
		t.Fatal("rotation lost sequence or retained old delegation")
	}
	restarted := NewVerifier(verifierOptions(publicA, state))
	verifierDelegate(t, restarted, rootB, releasePublic, func(d *Delegation) { d.RootSequence = 2; d.DelegationSequence = 2 })
	verifierAssertReason(t, restarted.VerifyRotation(raw, envelope), ReasonSequence)
}

func TestVerifierDelegationFreshnessLimitsAcquisition(t *testing.T) {
	root, public := verifierFixtureKey(t, "root-a")
	release, releasePublic := verifierFixtureKey(t, "release-1")
	state := NewMemoryState()
	v := NewVerifier(verifierOptions(public, state))
	verifierDelegate(t, v, root, releasePublic, func(d *Delegation) { d.ExpiresAt = "2026-07-01T00:00:00Z" })
	raw, envelope := verifierSign(t, verifierFixtureSet(), "component_set", release)
	verifierAssertReason(t, v.AcceptForAcquisition(raw, envelope), ReasonInvalidDocument)
	opts := verifierOptions(public, state)
	opts.Clock.Now = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	out := NewVerifier(opts).AcceptForAcquisition(raw, envelope)
	verifierAssertReason(t, out, ReasonExpired)
	if out.Freshness != Expired {
		t.Fatal("expired authority reported fresh")
	}
}

func TestVerifierAuthenticatesBeforeInterpretingDocument(t *testing.T) {
	root, public := verifierFixtureKey(t, "root-a")
	release, releasePublic := verifierFixtureKey(t, "release-1")
	v := NewVerifier(verifierOptions(public, nil))
	verifierDelegate(t, v, root, releasePublic, nil)
	raw, envelope := verifierSign(t, verifierFixtureSet(), "component_set", release)
	raw[0] = '['
	verifierAssertReason(t, v.VerifyComponentSet(raw, envelope), ReasonSignatureInvalid)
}

func TestAcceptSetRejectsMixingAndPartialPlatform(t *testing.T) {
	root, public := verifierFixtureKey(t, "root-a")
	release, releasePublic := verifierFixtureKey(t, "release-1")
	v := NewVerifier(verifierOptions(public, nil))
	verifierDelegate(t, v, root, releasePublic, nil)
	first := verifierFixtureSet()
	firstRaw, firstEnvelope := verifierSign(t, first, "component_set", release)
	second := verifierFixtureSet()
	second.ReleaseSequence = 2
	second.ReleaseTag = "v2.0.0"
	second.Platforms[0].Components[0].Version = "2.0.0"
	secondRaw, secondEnvelope := verifierSign(t, second, "component_set", release)
	verifierAssertReason(t, v.VerifyComponentSet(firstRaw, firstEnvelope), ReasonAccepted)
	verifierAssertReason(t, v.VerifyComponentSet(secondRaw, secondEnvelope), ReasonAccepted)
	parsed, err := ParseComponentSet(firstRaw)
	if err != nil {
		t.Fatal("fixture parse failed")
	}
	other, err := ParseComponentSet(secondRaw)
	if err != nil {
		t.Fatal("fixture parse failed")
	}
	mixed := append([]Component{}, parsed.Platforms[0].Components...)
	mixed[0] = other.Platforms[0].Components[0]
	verifierAssertReason(t, v.AcceptSet(firstRaw, firstEnvelope, mixed), ReasonMixedSet)
	// Identical metadata still has another raw-manifest provenance, and cannot
	// silently cross the binding merely because that component did not change.
	mixed = append([]Component{}, parsed.Platforms[0].Components...)
	mixed[1] = other.Platforms[0].Components[1]
	verifierAssertReason(t, v.AcceptSet(firstRaw, firstEnvelope, mixed), ReasonMixedSet)
	verifierAssertReason(t, v.AcceptSet(firstRaw, firstEnvelope, parsed.Platforms[0].Components[:1]), ReasonMixedSet)
	verifierAssertReason(t, v.AcceptForOfflineStart(firstRaw, firstEnvelope), ReasonNotInstalled)
}

func TestVerifierBootstrapRotationOrderIndependent(t *testing.T) {
	rootA, publicA := verifierFixtureKey(t, "root-a")
	rootB, publicB := verifierFixtureKey(t, "root-b")
	rotation := RootRotation{FormatVersion: 1, Critical: []string{}, Kind: "root_rotation", RootSequence: 1, NewSequence: 2, IssuedAt: "2026-01-01T00:00:00Z", ExpiresAt: "2027-01-01T00:00:00Z", NewRoot: publicB}
	for _, roots := range [][]PublicKey{{publicA, publicB}, {publicB, publicA}} {
		for _, keys := range [][]*rsa.PrivateKey{{rootA, rootB}, {rootB, rootA}} {
			state := NewMemoryState()
			opts := verifierOptions(publicA, state)
			opts.Roots = roots
			raw, envelope := verifierSign(t, rotation, "root_rotation", keys...)
			verifierAssertReason(t, NewVerifier(opts).VerifyRotation(raw, envelope), ReasonAccepted)
			snapshot, err := state.Load()
			if err != nil || snapshot.InitialRootID != publicA.KeyID || snapshot.Root.KeyID != publicB.KeyID || snapshot.RootSequence != 2 {
				t.Fatal("rotation authority depended on root or signature order")
			}
		}
	}
}
