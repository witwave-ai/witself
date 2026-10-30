package trust_test

import (
	"bytes"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/localcell/trust"
)

var updateVectors = flag.Bool("update-trust-vectors", false, "explicitly regenerate the public frozen fixture corpus")

type vectorRef struct {
	Kind     string `json:"kind"`
	Document string `json:"document"`
	Envelope string `json:"envelope"`
}
type vectorCase struct {
	Name string `json:"name"`
	vectorRef
	Expected         trust.Reason `json:"expected_reason"`
	Positive         bool         `json:"positive"`
	SignatureInvalid bool         `json:"negative_signature,omitempty"`
	Signers          []string     `json:"signers"`
	Roots            []string     `json:"roots"`
	Setup            []vectorRef  `json:"setup"`
	Now              string       `json:"now,omitempty"`
	Untrusted        bool         `json:"untrusted_clock,omitempty"`
	AllowRollback    bool         `json:"allow_rollback,omitempty"`
	ExpectedTag      string       `json:"expected_tag,omitempty"`
	OS               string       `json:"os,omitempty"`
	Arch             string       `json:"arch,omitempty"`
}
type vectorIndex struct {
	FormatVersion int          `json:"format_version"`
	Cases         []vectorCase `json:"cases"`
}

func fixturePrivate(t *testing.T, name string) *rsa.PrivateKey {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "keys", "TEST-ONLY-"+name+".private.pem"))
	if err != nil {
		t.Fatal("fixture unavailable")
	}
	p, rest := pem.Decode(b)
	if p == nil || p.Type != "TEST-ONLY PRIVATE KEY" || len(rest) != 0 {
		t.Fatal("fixture PEM invalid")
	}
	k, err := x509.ParsePKCS8PrivateKey(p.Bytes)
	if err != nil {
		t.Fatal("fixture DER invalid")
	}
	r, ok := k.(*rsa.PrivateKey)
	if !ok {
		t.Fatal("fixture not RSA")
	}
	return r
}
func fixturePublic(t *testing.T, name string) trust.PublicKey {
	t.Helper()
	p, err := trust.PublicKeyFromRSA(&fixturePrivate(t, name).PublicKey)
	if err != nil {
		t.Fatal("fixture public key invalid")
	}
	return p
}
func vectorBytes(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "vectors", name))
	if err != nil {
		t.Fatal("vector unavailable:", name)
	}
	return b
}
func frozenIndex(t *testing.T) vectorIndex {
	t.Helper()
	var idx vectorIndex
	if json.Unmarshal(vectorBytes(t, "index.json"), &idx) != nil || idx.FormatVersion != 1 {
		t.Fatal("invalid vector index")
	}
	return idx
}
func vectorClock(c vectorCase) trust.Clock {
	now := "2026-10-27T12:00:00Z"
	if c.Now != "" {
		now = c.Now
	}
	parsed, _ := time.Parse(time.RFC3339, now)
	return trust.Clock{Now: parsed, Trusted: !c.Untrusted}
}
func applyVector(v *trust.Verifier, kind string, doc, sig []byte) trust.Outcome {
	switch kind {
	case "delegation":
		return v.VerifyDelegation(doc, sig)
	case "root_rotation":
		return v.VerifyRotation(doc, sig)
	default:
		return v.AcceptForAcquisition(doc, sig)
	}
}
func TestFrozenVectors(t *testing.T) {
	if *updateVectors {
		generateFrozenVectors(t)
	}
	idx := frozenIndex(t)
	positives, negatives := 0, 0
	for _, c := range idx.Cases {
		if c.Positive {
			positives++
		} else {
			negatives++
		}
		t.Run(c.Name, func(t *testing.T) {
			opts := trust.Options{Clock: vectorClock(c), State: trust.NewMemoryState(), AllowRollback: c.AllowRollback, ExpectedTag: c.ExpectedTag, OS: c.OS, Arch: c.Arch}
			for _, name := range c.Roots {
				opts.Roots = append(opts.Roots, fixturePublic(t, name))
			}
			setupOpts := opts
			setupOpts.Clock.Trusted = true
			v := trust.NewVerifier(setupOpts)
			for _, s := range c.Setup {
				o := applyVector(v, s.Kind, vectorBytes(t, s.Document), vectorBytes(t, s.Envelope))
				if !o.Accepted {
					t.Fatalf("setup refused: %s", o.Reason)
				}
			}
			v = trust.NewVerifier(opts)
			o := applyVector(v, c.Kind, vectorBytes(t, c.Document), vectorBytes(t, c.Envelope))
			if o.Reason != c.Expected || o.Accepted != c.Positive {
				t.Fatalf("got accepted=%t reason=%s, want accepted=%t reason=%s", o.Accepted, o.Reason, c.Positive, c.Expected)
			}
		})
	}
	if positives < 12 || negatives < 40 {
		t.Fatalf("insufficient corpus: positive=%d negative=%d", positives, negatives)
	}
	t.Logf("frozen corpus: positive=%d negative=%d", positives, negatives)
}

func TestPositiveVectorSignatureRegeneration(t *testing.T) {
	for _, c := range frozenIndex(t).Cases {
		if !c.Positive {
			continue
		}
		t.Run(c.Name, func(t *testing.T) {
			document := regeneratePositiveDocument(t, c.Name)
			if !bytes.Equal(document, vectorBytes(t, c.Document)) {
				t.Fatal("deterministic fixture document differs")
			}
			var keys []*rsa.PrivateKey
			for _, name := range c.Signers {
				keys = append(keys, fixturePrivate(t, name))
			}
			got, err := trust.SignDocument(document, c.Kind, keys...)
			if err != nil || !bytes.Equal(got, vectorBytes(t, c.Envelope)) {
				t.Fatal("deterministic fixture envelope differs")
			}
		})
	}
}

func regeneratePositiveDocument(t *testing.T, name string) []byte {
	t.Helper()
	m := baseVectorSet()
	var authority any
	switch name {
	case "positive-built-dist":
		return buildPositiveDistVector(t)
	case "positive-base", "positive-release-key-two", "positive-allowed-rollback", "positive-noncritical-field", "positive-uppercase-aliases-ignored":
	case "positive-darwin":
		m.Platforms[0].OS, m.Platforms[0].GlibcFloor, m.Platforms[0].MinOS = "darwin", "", "14"
	case "positive-windows":
		m.Platforms[0].OS, m.Platforms[0].GlibcFloor, m.Platforms[0].MinOS = "windows", "", "11"
	case "positive-arm64":
		m.Platforms[0].Arch = "arm64"
	case "positive-nfc-path":
		m.Platforms[0].Components[0].Inventory[0].Path = "bin/caf\u00e9"
		m.Platforms[0].Components[0].Ownership.PrivatePayloadPaths = []string{"bin/caf\u00e9"}
	case "positive-release-two":
		m.ReleaseSequence, m.ReleaseTag = 2, "v1.2.4"
	case "positive-second-worker":
		c := m.Platforms[0].Components[3]
		c.Archive.Name = "worker-two.tar.gz"
		c.Inventory = []trust.InventoryEntry{{Path: "bin/worker-two", Size: 4, SHA256: strings.Repeat("a", 64), Mode: "0755", Executable: true}}
		c.Ownership.PrivatePayloadPaths = []string{"bin/worker-two"}
		m.Platforms[0].Components = append(m.Platforms[0].Components, c)
	case "positive-delegation", "positive-delegation-two":
		d := trust.Delegation{FormatVersion: 1, Critical: []string{"kind", "keys"}, Kind: "delegation", RootSequence: 1, DelegationSequence: 1, IssuedAt: "2026-01-01T00:00:00Z", ExpiresAt: "2030-01-01T00:00:00Z", Keys: []trust.PublicKey{fixturePublic(t, "release-1"), fixturePublic(t, "release-2"), fixturePublic(t, "revoked-1")}, RevokedKeyIDs: []string{}}
		if name == "positive-delegation-two" {
			d.DelegationSequence = 2
		}
		authority = d
	case "positive-rotation", "positive-rotation-reversed":
		authority = trust.RootRotation{FormatVersion: 1, Critical: []string{"kind", "new_root"}, Kind: "root_rotation", RootSequence: 1, NewSequence: 2, IssuedAt: "2026-01-01T00:00:00Z", ExpiresAt: "2030-01-01T00:00:00Z", NewRoot: fixturePublic(t, "root-b")}
	default:
		t.Fatal("positive fixture has no regeneration recipe")
	}
	var doc any = m
	if authority != nil {
		doc = authority
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal("positive fixture regeneration failed")
	}
	if name == "positive-noncritical-field" {
		b = bytes.Replace(b, []byte("{\n"), []byte("{\n  \"future_extension\": true,\n"), 1)
	}
	if name == "positive-uppercase-aliases-ignored" {
		b = vectorCaseAliases(b)
	}
	return b
}

func buildPositiveDistVector(t *testing.T) []byte {
	t.Helper()
	dir := t.TempDir()
	dist, out := filepath.Join(dir, "dist"), filepath.Join(dir, "component-set.json")
	commands := [][]string{
		{"run", "./testdata/build-vector", "--metadata", "testdata/vectors/positive-base.json", "--out", dist},
		{"run", "../../../tools/component-manifest", "build", "--dist", dist, "--tag", "v1.2.3", "--sequence", "1", "--channel", "stable", "--platform", "linux/amd64=" + filepath.Join(dist, "platform.json"), "--out", out},
	}
	for _, args := range commands {
		if err := exec.Command("go", args...).Run(); err != nil {
			t.Fatal("deterministic dist recipe/build command failed")
		}
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal("built fixture document unavailable")
	}
	return raw
}

func vectorCaseAliases(raw []byte) []byte {
	raw = addIgnoredField(raw, "RELEASE_SEQUENCE", []byte("99"))
	raw = addIgnoredField(raw, "CRITICAL", []byte(`["unknown_critical_field"]`))
	raw = addIgnoredField(raw, "PLATFORMS", []byte(`[{"unknown":true,"components":[{"required_extensions":null,"schema":{"target":1.5}}]}]`))
	return addIgnoredField(raw, "\u212aind", []byte(`"unicode-casefold-kind"`))
}

func TestFixtureKeysHaveNoProductionAuthority(t *testing.T) {
	roots := trust.ProductionRoots()
	if len(roots) != 0 {
		t.Fatal("production roots must remain empty")
	}
	for _, name := range []string{"root-a", "root-b", "release-1", "release-2", "revoked-1"} {
		key := fixturePublic(t, name)
		for _, root := range roots {
			if root.KeyID == key.KeyID {
				t.Fatal("fixture has production authority")
			}
		}
		b, err := os.ReadFile(filepath.Join("testdata", "keys", "TEST-ONLY-"+name+".public.pem"))
		if err != nil {
			t.Fatal("public fixture missing")
		}
		p, rest := pem.Decode(b)
		der, decodeErr := base64.StdEncoding.DecodeString(key.SPKI)
		if p == nil || p.Type != "TEST-ONLY PUBLIC KEY" || len(rest) != 0 || decodeErr != nil || !bytes.Equal(p.Bytes, der) {
			t.Fatal("public fixture differs from private fixture")
		}
	}
}

func TestOpenSSLParity(t *testing.T) {
	bin, err := exec.LookPath("openssl")
	if err != nil {
		if os.Getenv("WITSELF_TEST_REQUIRE_OPENSSL") == "1" {
			t.Fatal("openssl_required_unavailable")
		}
		t.Log("NOT_RUN openssl_unavailable")
		return
	}
	passed, failed := 0, 0
	for _, c := range frozenIndex(t).Cases {
		if !c.Positive && !c.SignatureInvalid {
			continue
		}
		t.Run(c.Name, func(t *testing.T) {
			var envelope trust.Envelope
			if json.Unmarshal(vectorBytes(t, c.Envelope), &envelope) != nil {
				t.Fatal("oracle envelope malformed")
			}
			for i, signature := range envelope.Signatures {
				key := fixturePrivate(t, c.Signers[i])
				der, e := x509.MarshalPKIXPublicKey(&key.PublicKey)
				if e != nil {
					t.Fatal("oracle public key failed")
				}
				raw, e := base64.StdEncoding.DecodeString(signature.Signature)
				if e != nil {
					t.Fatal("oracle signature decode failed")
				}
				dir := t.TempDir()
				pub, sig := filepath.Join(dir, "public.pem"), filepath.Join(dir, "raw.sig")
				if os.WriteFile(pub, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0600) != nil || os.WriteFile(sig, raw, 0600) != nil {
					t.Fatal("oracle file write failed")
				}
				cmd := exec.Command(bin, "dgst", "-sha256", "-verify", pub, "-signature", sig, filepath.Join("testdata", "vectors", c.Document))
				err := cmd.Run()
				if c.Positive && err != nil {
					t.Fatal("OpenSSL rejected positive signature")
				}
				if c.SignatureInvalid && err == nil {
					t.Fatal("OpenSSL accepted negative signature")
				}
				if c.Positive {
					passed++
				} else {
					failed++
				}
			}
		})
	}
	t.Logf("OpenSSL parity: positive signatures=%d negative signatures=%d", passed, failed)
}

func baseVectorSet() trust.ComponentSet {
	digest := strings.Repeat("a", 64)
	components := []trust.Component{}
	for _, role := range []string{"app", "service_host", "server", "worker", "postgresql"} {
		path := "bin/" + role
		c := trust.Component{Role: role, Version: "1.2.3", Archive: trust.Archive{Name: role + ".tar.gz", Size: 1024, SHA256: digest, Format: "tar.gz"}, Inventory: []trust.InventoryEntry{{Path: path, Size: 4, SHA256: digest, Mode: "0755", Executable: true}}, DependsOn: []string{}, RequiredExtensions: []trust.Extension{}, Schema: trust.Schema{Target: 1, AcceptedMin: 1, AcceptedMax: 1}, Ownership: trust.Ownership{PackageOwnedPaths: []string{}, PrivatePayloadPaths: []string{path}}}
		if role == "postgresql" {
			c.Version = "16.4"
			c.PostgreSQL = &trust.PostgreSQL{Major: 16, Version: "16.4", Distribution: "test-only", Build: "fixture", RuntimeLibraries: []string{}, DataLayoutVersion: 1}
		}
		components = append(components, c)
	}
	return trust.ComponentSet{FormatVersion: 1, Critical: []string{"kind", "root_sequence", "delegation_sequence", "release_sequence", "platforms"}, Kind: "component_set", RootSequence: 1, DelegationSequence: 1, ReleaseSequence: 1, ReleaseTag: "v1.2.3", Channel: "stable", IssuedAt: "2026-01-01T00:00:00Z", ExpiresAt: "2030-01-01T00:00:00Z", Source: trust.Source{Repository: "fixture/repository", Commit: strings.Repeat("b", 40)}, Provenance: trust.Provenance{ChecksumsSHA256: digest, SigstoreBundle: "checksums.sigstore.json"}, License: "LICENSE", SBOM: "sbom.json", Rollback: trust.Rollback{AllowedTargets: []uint64{}}, Platforms: []trust.Platform{{OS: "linux", Arch: "amd64", MinOS: "fixture", GlibcFloor: "2.31", Components: components}}}
}

func generateFrozenVectors(t *testing.T) {
	t.Helper()
	dir := filepath.Join("testdata", "vectors")
	if os.MkdirAll(dir, 0755) != nil {
		t.Fatal("vector directory unavailable")
	}
	marshal := func(v any) []byte {
		b, e := json.MarshalIndent(v, "", "  ")
		if e != nil {
			t.Fatal("vector encoding failed")
		}
		return b
	}
	write := func(name string, b []byte) {
		if os.WriteFile(filepath.Join(dir, name), b, 0644) != nil {
			t.Fatal("vector write failed")
		}
	}
	sign := func(raw []byte, kind string, names []string) []byte {
		keys := []*rsa.PrivateKey{}
		for _, n := range names {
			keys = append(keys, fixturePrivate(t, n))
		}
		b, e := trust.SignDocument(raw, kind, keys...)
		if e != nil {
			t.Fatal("vector signing failed")
		}
		return b
	}
	delegation := trust.Delegation{FormatVersion: 1, Critical: []string{"kind", "keys"}, Kind: "delegation", RootSequence: 1, DelegationSequence: 1, IssuedAt: "2026-01-01T00:00:00Z", ExpiresAt: "2030-01-01T00:00:00Z", Keys: []trust.PublicKey{fixturePublic(t, "release-1"), fixturePublic(t, "release-2"), fixturePublic(t, "revoked-1")}, RevokedKeyIDs: []string{}}
	setup := func(name, kind string, v any, names ...string) vectorRef {
		b := marshal(v)
		r := vectorRef{Kind: kind, Document: name + ".json", Envelope: name + ".json.sig.json"}
		write(r.Document, b)
		write(r.Envelope, sign(b, kind, names))
		return r
	}
	delegationRef := setup("setup-delegation", "delegation", delegation, "root-a")
	idx := vectorIndex{FormatVersion: 1, Cases: []vectorCase{}}
	add := func(name, kind string, raw []byte, reason trust.Reason, names ...string) *vectorCase {
		c := vectorCase{Name: name, vectorRef: vectorRef{Kind: kind, Document: name + ".json", Envelope: name + ".json.sig.json"}, Expected: reason, Positive: reason == trust.ReasonAccepted, Signers: names, Roots: []string{"root-a"}, Setup: []vectorRef{}}
		if kind == "component_set" {
			c.Setup = []vectorRef{delegationRef}
		}
		write(c.Document, raw)
		write(c.Envelope, sign(raw, kind, names))
		idx.Cases = append(idx.Cases, c)
		return &idx.Cases[len(idx.Cases)-1]
	}
	component := func(name string, reason trust.Reason, mutate func(*trust.ComponentSet)) *vectorCase {
		m := baseVectorSet()
		if mutate != nil {
			mutate(&m)
		}
		return add(name, "component_set", marshal(m), reason, "release-1")
	}
	base := marshal(baseVectorSet())
	component("positive-base", trust.ReasonAccepted, nil)
	component("positive-darwin", trust.ReasonAccepted, func(m *trust.ComponentSet) {
		m.Platforms[0].OS = "darwin"
		m.Platforms[0].GlibcFloor = ""
		m.Platforms[0].MinOS = "14"
	})
	component("positive-windows", trust.ReasonAccepted, func(m *trust.ComponentSet) {
		m.Platforms[0].OS = "windows"
		m.Platforms[0].GlibcFloor = ""
		m.Platforms[0].MinOS = "11"
	})
	component("positive-arm64", trust.ReasonAccepted, func(m *trust.ComponentSet) { m.Platforms[0].Arch = "arm64" })
	component("positive-nfc-path", trust.ReasonAccepted, func(m *trust.ComponentSet) {
		m.Platforms[0].Components[0].Inventory[0].Path = "bin/caf\u00e9"
		m.Platforms[0].Components[0].Ownership.PrivatePayloadPaths = []string{"bin/caf\u00e9"}
	})
	component("positive-release-two", trust.ReasonAccepted, func(m *trust.ComponentSet) { m.ReleaseSequence = 2; m.ReleaseTag = "v1.2.4" })
	component("positive-second-worker", trust.ReasonAccepted, func(m *trust.ComponentSet) {
		c := m.Platforms[0].Components[3]
		c.Archive.Name = "worker-two.tar.gz"
		c.Inventory = []trust.InventoryEntry{{Path: "bin/worker-two", Size: 4, SHA256: strings.Repeat("a", 64), Mode: "0755", Executable: true}}
		c.Ownership.PrivatePayloadPaths = []string{"bin/worker-two"}
		m.Platforms[0].Components = append(m.Platforms[0].Components, c)
	})
	add("positive-release-key-two", "component_set", base, trust.ReasonAccepted, "release-2")
	add("positive-noncritical-field", "component_set", bytes.Replace(base, []byte("{\n"), []byte("{\n  \"future_extension\": true,\n"), 1), trust.ReasonAccepted, "release-1")
	add("positive-uppercase-aliases-ignored", "component_set", vectorCaseAliases(base), trust.ReasonAccepted, "release-1")
	add("positive-delegation", "delegation", marshal(delegation), trust.ReasonAccepted, "root-a")
	d2 := delegation
	d2.DelegationSequence = 2
	add("positive-delegation-two", "delegation", marshal(d2), trust.ReasonAccepted, "root-a").Setup = []vectorRef{delegationRef}
	rotation := trust.RootRotation{FormatVersion: 1, Critical: []string{"kind", "new_root"}, Kind: "root_rotation", RootSequence: 1, NewSequence: 2, IssuedAt: delegation.IssuedAt, ExpiresAt: delegation.ExpiresAt, NewRoot: fixturePublic(t, "root-b")}
	add("positive-rotation", "root_rotation", marshal(rotation), trust.ReasonAccepted, "root-a", "root-b")
	add("positive-rotation-reversed", "root_rotation", marshal(rotation), trust.ReasonAccepted, "root-b", "root-a")
	// Cryptographic failures remain valid envelope JSON so OpenSSL can independently reject them.
	c := add("tampered-document", "component_set", base, trust.ReasonSignatureInvalid, "release-1")
	c.SignatureInvalid = true
	write(c.Document, append(append([]byte{}, base...), '\n'))
	c = add("tampered-signature", "component_set", base, trust.ReasonSignatureInvalid, "release-1")
	c.SignatureInvalid = true
	var env trust.Envelope
	if json.Unmarshal(vectorBytes(t, c.Envelope), &env) != nil {
		t.Fatal("generated envelope invalid")
	}
	raw, _ := base64.StdEncoding.DecodeString(env.Signatures[0].Signature)
	raw[0] ^= 1
	env.Signatures[0].Signature = base64.StdEncoding.EncodeToString(raw)
	write(c.Envelope, marshal(env))
	envelopeMutation := func(name string, reason trust.Reason, f func(*trust.Envelope)) *vectorCase {
		c := add(name, "component_set", base, reason, "release-1")
		var e trust.Envelope
		if json.Unmarshal(vectorBytes(t, c.Envelope), &e) != nil {
			t.Fatal("envelope invalid")
		}
		f(&e)
		write(c.Envelope, marshal(e))
		return c
	}
	envelopeMutation("unknown-key", trust.ReasonUnknownKey, func(e *trust.Envelope) { e.Signatures[0].KeyID = strings.Repeat("0", 64) })
	envelopeMutation("wrong-algorithm", trust.ReasonWrongAlgorithm, func(e *trust.Envelope) { e.Algorithm = "rsa-pss-sha256" })
	envelopeMutation("wrong-signature-length", trust.ReasonSignatureLength, func(e *trust.Envelope) {
		e.Signatures[0].Signature = base64.StdEncoding.EncodeToString(make([]byte, 383))
	}).SignatureInvalid = true
	envelopeMutation("invalid-signature-encoding", trust.ReasonInvalidEncoding, func(e *trust.Envelope) { e.Signatures[0].Signature = "not-base64" })
	envelopeMutation("extra-signature", trust.ReasonSignatureCount, func(e *trust.Envelope) { e.Signatures = append(e.Signatures, e.Signatures[0]) })
	envelopeMutation("wrong-envelope-kind", trust.ReasonWrongKind, func(e *trust.Envelope) { e.Kind = "delegation" })
	c = add("tampered-envelope", "component_set", base, trust.ReasonInvalidJSON, "release-1")
	write(c.Envelope, []byte("{"))
	c = add("oversized-envelope", "component_set", base, trust.ReasonEnvelopeTooLarge, "release-1")
	write(c.Envelope, bytes.Repeat([]byte(" "), trust.MaxEnvelopeBytes+1))
	add("root-signs-component", "component_set", base, trust.ReasonWrongSigner, "root-a")
	add("release-signs-delegation", "delegation", marshal(delegation), trust.ReasonWrongSigner, "release-1").Setup = []vectorRef{delegationRef}
	c = add("rotation-one-signature", "root_rotation", marshal(rotation), trust.ReasonSignatureCount, "root-a", "root-b")
	if json.Unmarshal(vectorBytes(t, c.Envelope), &env) != nil {
		t.Fatal("envelope invalid")
	}
	env.Signatures = env.Signatures[:1]
	write(c.Envelope, marshal(env))
	badRotation := rotation
	badRotation.NewSequence = 3
	add("rotation-sequence-skip", "root_rotation", marshal(badRotation), trust.ReasonSequence, "root-a", "root-b")
	badDelegation := delegation
	badDelegation.RevokedKeyIDs = []string{fixturePublic(t, "revoked-1").KeyID}
	revRef := setup("setup-revocation", "delegation", badDelegation, "root-a")
	add("revoked-key", "component_set", base, trust.ReasonRevokedKey, "revoked-1").Setup = []vectorRef{revRef}
	for _, variant := range []string{"rsa2048", "exponent-three"} {
		k := fixturePrivate(t, "release-1").PublicKey
		if variant == "rsa2048" {
			k.N = new(big.Int).Rsh(new(big.Int).Set(k.N), 1024)
		} else {
			k.E = 3
		}
		der, e := x509.MarshalPKIXPublicKey(&k)
		if e != nil {
			t.Fatal("invalid-key fixture failed")
		}
		sum := sha256.Sum256(der)
		d := delegation
		d.Keys = []trust.PublicKey{{KeyID: hex.EncodeToString(sum[:]), SPKI: base64.StdEncoding.EncodeToString(der)}}
		add(variant, "delegation", marshal(d), trust.ReasonInvalidKey, "root-a")
	}
	component("expired", trust.ReasonExpired, func(m *trust.ComponentSet) { m.ExpiresAt = "2026-10-26T00:00:00Z" })
	component("not-yet-valid", trust.ReasonNotYetValid, func(m *trust.ComponentSet) { m.IssuedAt = "2026-10-28T00:00:00Z" })
	c = component("untrusted-clock", trust.ReasonUnknownClock, nil)
	c.Untrusted = true
	c.Setup = nil
	// Delegate under a trusted clock first; the replay test changes only the acquisition clock.
	c.Setup = []vectorRef{delegationRef}
	component("wrong-tag", trust.ReasonWrongTag, func(m *trust.ComponentSet) { m.ReleaseTag = "1.2.3" })
	c = component("unexpected-tag", trust.ReasonWrongTag, nil)
	c.ExpectedTag = "v2.0.0"
	component("wrong-platform", trust.ReasonWrongPlatform, func(m *trust.ComponentSet) { m.Platforms[0].OS = "plan9" })
	c = component("unexpected-platform", trust.ReasonWrongPlatform, nil)
	c.OS = "darwin"
	c.Arch = "amd64"
	component("schema-excludes-target", trust.ReasonSchema, func(m *trust.ComponentSet) { m.Platforms[0].Components[0].Schema.AcceptedMax = 0 })
	component("postgresql-major", trust.ReasonPostgreSQL, func(m *trust.ComponentSet) { m.Platforms[0].Components[4].PostgreSQL.Major = 17 })
	component("required-extension", trust.ReasonExtensions, func(m *trust.ComponentSet) {
		m.Platforms[0].Components[0].RequiredExtensions = []trust.Extension{{Name: "test", Version: "1", ABI: "test"}}
	})
	component("missing-role", trust.ReasonRoles, func(m *trust.ComponentSet) { m.Platforms[0].Components = m.Platforms[0].Components[1:] })
	component("duplicate-role", trust.ReasonRoles, func(m *trust.ComponentSet) { m.Platforms[0].Components[3].Role = "app" })
	component("unknown-role", trust.ReasonRoles, func(m *trust.ComponentSet) { m.Platforms[0].Components[0].Role = "unknown" })
	for _, path := range []struct{ name, value string }{{"parent", "bin/../escape"}, {"absolute", "/escape"}, {"backslash", `bin\escape`}, {"nul", "bin/\x00escape"}, {"nfd", "bin/cafe\u0301"}, {"drive", "C:/escape"}, {"dot", "bin/./escape"}, {"long", strings.Repeat("x", 1025)}, {"empty-segment", "bin//escape"}} {
		component("path-"+path.name, trust.ReasonPath, func(m *trust.ComponentSet) { m.Platforms[0].Components[0].Inventory[0].Path = path.value })
	}
	component("duplicate-inventory", trust.ReasonInventory, func(m *trust.ComponentSet) {
		c := &m.Platforms[0].Components[0]
		c.Inventory = append(c.Inventory, c.Inventory[0])
	})
	component("mode-0600", trust.ReasonMode, func(m *trust.ComponentSet) { m.Platforms[0].Components[0].Inventory[0].Mode = "0600" })
	component("inventory-exceeds-archive", trust.ReasonInventory, func(m *trust.ComponentSet) { m.Platforms[0].Components[0].Inventory[0].Size = 1025 })
	component("archive-over-two-gib", trust.ReasonInventory, func(m *trust.ComponentSet) { m.Platforms[0].Components[0].Archive.Size = trust.MaxArchiveBytes + 1 })
	component("executable-mode-mismatch", trust.ReasonMode, func(m *trust.ComponentSet) { m.Platforms[0].Components[0].Inventory[0].Executable = false })
	add("duplicate-top-level", "component_set", bytes.Replace(base, []byte("{\n"), []byte("{\n  \"format_version\": 1,\n"), 1), trust.ReasonDuplicateKey, "release-1")
	add("duplicate-escaped-key", "component_set", bytes.Replace(base, []byte("{\n"), []byte("{\n  \"format_versi\\u006fn\": 1,\n"), 1), trust.ReasonDuplicateKey, "release-1")
	add("duplicate-nested", "component_set", bytes.Replace(base, []byte(`"repository":`), []byte(`"repository":"duplicate", "repository":`), 1), trust.ReasonDuplicateKey, "release-1")
	add("float-integer", "component_set", bytes.Replace(base, []byte(`"release_sequence": 1`), []byte(`"release_sequence": 1.0`), 1), trust.ReasonProfileViolation, "release-1")
	add("exponent-integer", "component_set", bytes.Replace(base, []byte(`"release_sequence": 1`), []byte(`"release_sequence": 1e0`), 1), trust.ReasonProfileViolation, "release-1")
	component("unknown-critical", trust.ReasonUnknownCritical, func(m *trust.ComponentSet) { m.Critical = append(m.Critical, "future") })
	add("unknown-nested", "component_set", bytes.Replace(base, []byte(`"repository":`), []byte(`"future":true, "repository":`), 1), trust.ReasonUnknownField, "release-1")
	add("oversized-document", "component_set", append(append([]byte{}, base...), bytes.Repeat([]byte(" "), trust.MaxComponentSetBytes)...), trust.ReasonDocumentTooLarge, "release-1")
	add("overlong-string", "component_set", bytes.Replace(base, []byte("fixture/repository"), []byte(strings.Repeat("x", 4097)), 1), trust.ReasonProfileViolation, "release-1")
	add("excessive-nesting", "component_set", bytes.Replace(base, []byte("{\n"), []byte("{\n\"future\":[[[[[[[[[]]]]]]]]],\n"), 1), trust.ReasonProfileViolation, "release-1")
	add("lone-surrogate", "component_set", bytes.Replace(base, []byte("fixture/repository"), []byte(`\ud800`), 1), trust.ReasonProfileViolation, "release-1")
	add("invalid-utf8", "component_set", bytes.Replace(base, []byte("fixture/repository"), []byte{0xff}, 1), trust.ReasonProfileViolation, "release-1")
	add("trailing-document", "component_set", append(append([]byte{}, base...), []byte(" {}")...), trust.ReasonInvalidJSON, "release-1")
	high := baseVectorSet()
	high.ReleaseSequence = 2
	high.ReleaseTag = "v1.2.4"
	highRef := setup("setup-high-release", "component_set", high, "release-1")
	c = component("release-replay", trust.ReasonReplay, nil)
	c.Setup = []vectorRef{delegationRef, highRef}
	c = component("rollback-denied", trust.ReasonRollbackDenied, nil)
	c.Setup = []vectorRef{delegationRef, highRef}
	c.AllowRollback = true
	high.Rollback.AllowedTargets = []uint64{1}
	allowedRef := setup("setup-rollback-policy", "component_set", high, "release-1")
	c = component("positive-allowed-rollback", trust.ReasonAccepted, nil)
	c.Setup = []vectorRef{delegationRef, allowedRef}
	c.AllowRollback = true
	add("positive-built-dist", "component_set", buildPositiveDistVector(t), trust.ReasonAccepted, "release-1")
	write("index.json", marshal(idx))
	write("README.md", []byte(fmt.Sprintf("# Frozen component trust v1 vectors\n\nThis corpus contains %d cases. index.json is format_version 1. Each case names exact document/envelope bytes, expected_reason, positive, fixture signers, explicit roots, ordered setup records, optional clock/target/rollback options, and negative_signature for the independent OpenSSL failure subset. Setup records are authenticated before the case. The untrusted-clock case sets its clock only after trusted delegation setup.\n\nRegeneration is explicit: go test ./internal/localcell/trust -run '^TestFrozenVectors$' -update-trust-vectors. It uses committed public TEST-ONLY private fixtures; ordinary tests do not create keys or rewrite vectors.\n\nThe positive-built-dist document and envelope use the [deterministic dist recipe](../build-vector/README.md). Its document is regenerated through the real component-manifest build command. Earlier synthetic vectors remain schema fixtures with their original frozen bytes.\n", len(idx.Cases))))
}
