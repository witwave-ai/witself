package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/rsa"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/localcell/trust"
)

func testFixture(parts ...string) string {
	return filepath.Join(append([]string{"..", "..", "internal", "localcell", "trust", "testdata"}, parts...)...)
}
func testRead(t *testing.T, path string) []byte {
	t.Helper()
	b, e := os.ReadFile(path)
	if e != nil {
		t.Fatal("fixture read failed")
	}
	return b
}
func testWrite(t *testing.T, path string, b []byte) {
	t.Helper()
	if os.WriteFile(path, b, 0600) != nil {
		t.Fatal("fixture write failed")
	}
}
func testJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, e := json.Marshal(v)
	if e != nil {
		t.Fatal("fixture encoding failed")
	}
	return b
}

func TestKeygenFixtureAndProductionRefusal(t *testing.T) {
	// Exercise the command with a committed key, keeping normal tests free of RSA
	// key generation and its unbounded random-prime search.
	fixture, err := privateKey(testFixture("keys", "TEST-ONLY-root-a.private.pem"))
	if err != nil {
		t.Fatal("fixture unavailable")
	}
	previous := generateKey
	generateKey = func() (*rsa.PrivateKey, error) { return fixture, nil }
	t.Cleanup(func() { generateKey = previous })
	dir := filepath.Join(t.TempDir(), "keys")
	if err := run([]string{"keygen", "--out", dir, "--label", "TEST-ONLY-roundtrip"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	got, err := privateKey(filepath.Join(dir, "TEST-ONLY-roundtrip.private.pem"))
	if err != nil || got.N.Cmp(fixture.N) != 0 {
		t.Fatal("generated fixture did not round-trip")
	}
	pub, err := publicKey(filepath.Join(dir, "TEST-ONLY-roundtrip.public.pem"))
	want, e := trust.PublicKeyFromRSA(&fixture.PublicKey)
	if err != nil || e != nil || pub != want {
		t.Fatal("public fixture mismatch")
	}
	for _, command := range []string{"keygen", "build", "sign", "verify"} {
		err := run([]string{command, "--production"}, &bytes.Buffer{})
		if err == nil || err.Error() != "production key generation is not authorized before S7-F2" {
			t.Fatal("production generation was not refused")
		}
	}
	for _, label := range []string{"production", "TEST-ONLY-", "TEST-ONLY-../escape", "TEST-ONLY-x\\y", "TEST-ONLY-x:y"} {
		if run([]string{"keygen", "--out", filepath.Join(t.TempDir(), "keys"), "--label", label}, &bytes.Buffer{}) == nil {
			t.Fatal("unsafe key label accepted")
		}
	}
	if run([]string{"keygen", "--out", dir, "--label", "TEST-ONLY-again"}, &bytes.Buffer{}) == nil {
		t.Fatal("existing key directory overwritten")
	}
}

func TestSignExactFixtureBytes(t *testing.T) {
	for _, c := range []struct {
		name, kind string
		keys       []string
	}{
		{"positive-base", "component_set", []string{"release-1"}},
		{"positive-delegation", "delegation", []string{"root-a"}},
		{"positive-rotation", "root_rotation", []string{"root-a", "root-b"}},
	} {
		t.Run(c.kind, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "signed.json")
			args := []string{"sign", "--kind", c.kind, "--in", testFixture("vectors", c.name+".json"), "--out", out}
			for _, key := range c.keys {
				args = append(args, "--key", testFixture("keys", "TEST-ONLY-"+key+".private.pem"))
			}
			if err := run(args, &bytes.Buffer{}); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(testRead(t, out), testRead(t, testFixture("vectors", c.name+".json.sig.json"))) {
				t.Fatal("exact-byte signature differs from frozen fixture")
			}
		})
	}
}

func TestVerifyFixtureChainAndPersistedState(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	base := []string{"verify", "--root", testFixture("keys", "TEST-ONLY-root-a.public.pem"), "--state", state, "--manifest", testFixture("vectors", "positive-base.json"), "--now", "2026-10-27T12:00:00Z"}
	args := append(append([]string{}, base...), "--delegation", testFixture("vectors", "setup-delegation.json"))
	var output bytes.Buffer
	if err := run(args, &output); err != nil {
		t.Fatal(err)
	}
	var result trust.Outcome
	if json.Unmarshal(output.Bytes(), &result) != nil || !result.Accepted || result.Reason != trust.ReasonAccepted {
		t.Fatal("chain verification failed")
	}
	output.Reset()
	if err := run(base, &output); err != nil {
		t.Fatal("persisted delegation was not usable:", err)
	}
	if bytes.Contains(output.Bytes(), []byte("PRIVATE KEY")) || bytes.Contains(output.Bytes(), []byte("spki")) || bytes.Contains(output.Bytes(), []byte("platforms")) {
		t.Fatal("outcome included key material or document bytes")
	}
	for _, c := range []struct {
		name   string
		args   []string
		reason string
	}{
		{"missing-root", []string{"verify", "--manifest", testFixture("vectors", "positive-base.json")}, "no_trust_root"},
		{"untrusted-clock", []string{"verify", "--root", testFixture("keys", "TEST-ONLY-root-a.public.pem"), "--state", state, "--manifest", testFixture("vectors", "positive-base.json"), "--untrusted-clock"}, "unknown_clock"},
		{"tamper", []string{"verify", "--root", testFixture("keys", "TEST-ONLY-root-a.public.pem"), "--state", state, "--manifest", testFixture("vectors", "tampered-document.json"), "--now", "2026-10-27T12:00:00Z"}, "signature_invalid"},
	} {
		t.Run(c.name, func(t *testing.T) {
			output.Reset()
			err := run(c.args, &output)
			if err == nil || err.Error() != c.reason {
				t.Fatalf("wanted %s refusal", c.reason)
			}
			if json.Unmarshal(output.Bytes(), &result) != nil || result.Accepted || string(result.Reason) != c.reason {
				t.Fatal("refusal outcome missing")
			}
		})
	}
}

func TestVerifyRotationChain(t *testing.T) {
	dir := t.TempDir()
	var delegation trust.Delegation
	if json.Unmarshal(testRead(t, testFixture("vectors", "setup-delegation.json")), &delegation) != nil {
		t.Fatal("delegation fixture invalid")
	}
	delegation.RootSequence = 2
	delegation.DelegationSequence = 2
	var manifest trust.ComponentSet
	if json.Unmarshal(testRead(t, testFixture("vectors", "positive-base.json")), &manifest) != nil {
		t.Fatal("manifest fixture invalid")
	}
	manifest.RootSequence = 2
	manifest.DelegationSequence = 2
	docPath, manifestPath := filepath.Join(dir, "delegation.json"), filepath.Join(dir, "manifest.json")
	testWrite(t, docPath, testJSON(t, delegation))
	testWrite(t, manifestPath, testJSON(t, manifest))
	for _, c := range []struct{ path, kind, key string }{{docPath, "delegation", "root-b"}, {manifestPath, "component_set", "release-1"}} {
		if err := run([]string{"sign", "--key", testFixture("keys", "TEST-ONLY-"+c.key+".private.pem"), "--kind", c.kind, "--in", c.path, "--out", c.path + ".sig.json"}, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
	}
	var output bytes.Buffer
	if err := run([]string{"verify", "--root", testFixture("keys", "TEST-ONLY-root-a.public.pem"), "--rotation", testFixture("vectors", "positive-rotation.json"), "--delegation", docPath, "--manifest", manifestPath, "--now", "2026-10-27T12:00:00Z"}, &output); err != nil {
		t.Fatal(err)
	}
	var result trust.Outcome
	if json.Unmarshal(output.Bytes(), &result) != nil || !result.Accepted || result.RootSequence != 2 || result.DelegationSequence != 2 {
		t.Fatal("rotation chain failed")
	}
}

func makeToolArchive(t *testing.T, path, name string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal("archive create failed")
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	body := []byte("fixture\n")
	if tw.WriteHeader(&tar.Header{Name: name, Mode: 0755, Size: int64(len(body)), Typeflag: tar.TypeReg, Format: tar.FormatUSTAR}) != nil {
		t.Fatal("archive header failed")
	}
	if _, err = tw.Write(body); err != nil {
		t.Fatal("archive body failed")
	}
	if tw.Close() != nil || gz.Close() != nil || f.Close() != nil {
		t.Fatal("archive close failed")
	}
}

func TestBuildComputesBoundedArchiveInventory(t *testing.T) {
	dir := t.TempDir()
	metadata := testRead(t, testFixture("vectors", "positive-base.json"))
	var m trust.ComponentSet
	if json.Unmarshal(metadata, &m) != nil {
		t.Fatal("manifest fixture invalid")
	}
	p := m.Platforms[0]
	for _, c := range p.Components {
		makeToolArchive(t, filepath.Join(dir, c.Archive.Name), c.Inventory[0].Path)
	}
	testWrite(t, filepath.Join(dir, "component-metadata.json"), metadata)
	platform := filepath.Join(dir, "platform.json")
	testWrite(t, platform, testJSON(t, p))
	out := filepath.Join(dir, "built.json")
	args := []string{"build", "--dist", dir, "--tag", "v1.2.4", "--sequence", "2", "--channel", "stable", "--platform", "linux/amd64=" + platform, "--out", out}
	if err := run(args, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	built, err := trust.ParseComponentSet(testRead(t, out))
	if err != nil || built.ReleaseTag != "v1.2.4" || built.ReleaseSequence != 2 {
		t.Fatal("build did not produce valid selected release")
	}
	for _, c := range built.Platforms[0].Components {
		if c.Archive.Size == 1024 || c.Archive.SHA256 == strings.Repeat("a", 64) || len(c.Inventory) != 1 || c.Inventory[0].Size != 8 {
			t.Fatal("archive metadata was not recomputed")
		}
	}
	// Same reader as extraction must reject archive traversal during build.
	makeToolArchive(t, filepath.Join(dir, p.Components[0].Archive.Name), "../outside")
	if run(args, &bytes.Buffer{}) == nil {
		t.Fatal("unsafe build archive accepted")
	}
}

func TestBuildMatchesCommittedVector(t *testing.T) {
	committed := testRead(t, testFixture("vectors", "positive-built-dist.json"))
	for i := 0; i < 2; i++ {
		dir := t.TempDir()
		dist, out := filepath.Join(dir, "dist"), filepath.Join(dir, "component-set.json")
		recipe := exec.Command("go", "run", "./"+testFixture("build-vector"), "--metadata", testFixture("vectors", "positive-base.json"), "--out", dist)
		if err := recipe.Run(); err != nil {
			t.Fatal("deterministic dist recipe failed")
		}
		args := []string{"build", "--dist", dist, "--tag", "v1.2.3", "--sequence", "1", "--channel", "stable", "--platform", "linux/amd64=" + filepath.Join(dist, "platform.json"), "--out", out}
		if err := run(args, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(testRead(t, out), committed) {
			t.Fatal("build output differs from committed dist vector")
		}
		if err := run([]string{"sign", "--key", testFixture("keys", "TEST-ONLY-release-1.private.pem"), "--kind", "component_set", "--in", out, "--out", out + ".sig.json"}, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(testRead(t, out+".sig.json"), testRead(t, testFixture("vectors", "positive-built-dist.json.sig.json"))) {
			t.Fatal("built document signature differs from committed envelope")
		}
		var output bytes.Buffer
		if err := run([]string{"verify", "--root", testFixture("keys", "TEST-ONLY-root-a.public.pem"), "--delegation", testFixture("vectors", "setup-delegation.json"), "--manifest", out, "--now", "2026-10-27T12:00:00Z"}, &output); err != nil {
			t.Fatal(err)
		}
		var result trust.Outcome
		if json.Unmarshal(output.Bytes(), &result) != nil || !result.Accepted || result.Reason != trust.ReasonAccepted {
			t.Fatal("built vector chain verification failed")
		}
	}
}

func TestToolArgumentsAndInputBounds(t *testing.T) {
	for _, args := range [][]string{nil, {"unknown"}, {"keygen"}, {"sign"}, {"build"}, {"verify"}, {"verify", "--manifest", "missing", "--now", "bad"}, {"verify", "--manifest", "missing", "--now", "2026-10-27T12:00:00Z", "--untrusted-clock"}} {
		if run(args, &bytes.Buffer{}) == nil {
			t.Fatal("invalid command accepted")
		}
	}
	path := filepath.Join(t.TempDir(), "oversized")
	testWrite(t, path, bytes.Repeat([]byte("x"), 17<<10))
	if _, err := publicKey(path); err == nil {
		t.Fatal("oversized public key accepted")
	}
	if _, err := privateKey(path); err == nil {
		t.Fatal("oversized private key accepted")
	}
}
