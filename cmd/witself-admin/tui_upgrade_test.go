package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/client"
)

// mkTarGz builds an in-memory .tar.gz with one regular-file member.
func mkTarGz(t *testing.T, memberName string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{
		Name: memberName, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// stubRelease serves a fake GitHub release (checksums.txt + tarball)
// and points releaseDownloadBase at it. Restores globals on cleanup.
// cosign lookup is stubbed to "absent" so tests run hermetically —
// checksum-only mode, no network to Sigstore.
func stubRelease(t *testing.T, tag, artifactName string, tarball []byte, sumOverride string) {
	t.Helper()
	sum := sha256.Sum256(tarball)
	sumHex := hex.EncodeToString(sum[:])
	if sumOverride != "" {
		sumHex = sumOverride
	}
	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/%s/checksums.txt", tag), func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, "%s  %s\n", sumHex, artifactName)
	})
	mux.HandleFunc(fmt.Sprintf("/%s/%s", tag, artifactName), func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(tarball)
	})
	srv := httptest.NewServer(mux)
	oldBase, oldLook, oldRun := releaseDownloadBase, cosignLook, cosignRun
	releaseDownloadBase = srv.URL
	cosignLook = func() (string, error) { return "", errors.New("not installed") }
	t.Cleanup(func() {
		releaseDownloadBase = oldBase
		cosignLook = oldLook
		cosignRun = oldRun
		srv.Close()
	})
}

func stubSignedRelease(
	t *testing.T,
	tag, artifactName string,
	tarball []byte,
	signingAssets map[string][]byte,
	run func(context.Context, string, ...string) ([]byte, error),
) {
	t.Helper()
	sum := sha256.Sum256(tarball)
	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/%s/checksums.txt", tag), func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(sum[:]), artifactName)
	})
	mux.HandleFunc(fmt.Sprintf("/%s/%s", tag, artifactName), func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(tarball)
	})
	for name, content := range signingAssets {
		name, content := name, content
		mux.HandleFunc(fmt.Sprintf("/%s/%s", tag, name), func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(content)
		})
	}
	srv := httptest.NewServer(mux)
	oldBase, oldLook, oldRun := releaseDownloadBase, cosignLook, cosignRun
	releaseDownloadBase = srv.URL
	cosignLook = func() (string, error) { return "/fake/cosign", nil }
	cosignRun = run
	t.Cleanup(func() {
		releaseDownloadBase = oldBase
		cosignLook = oldLook
		cosignRun = oldRun
		srv.Close()
	})
}

func flagValue(t *testing.T, args []string, flag string) string {
	t.Helper()
	for i := range args {
		if args[i] == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	t.Fatalf("missing %s in cosign args: %q", flag, args)
	return ""
}

func artifactNameFor(tag string) string {
	return fmt.Sprintf("witself-admin_%s_%s_%s.tar.gz",
		strings.TrimPrefix(tag, "v"), runtime.GOOS, runtime.GOARCH)
}

// TestDownloadAndReplaceHappyPath pins the whole download → verify →
// extract → atomic-swap chain against a local release server.
func TestDownloadAndReplaceHappyPath(t *testing.T) {
	newBinary := []byte("#!/bin/true — the new version\n")
	tag := "v9.9.9"
	stubRelease(t, tag, artifactNameFor(tag), mkTarGz(t, "witself-admin", newBinary), "")

	binPath := filepath.Join(t.TempDir(), "witself-admin")
	if err := os.WriteFile(binPath, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := downloadAndReplace(t.Context(), tag, binPath); err != nil {
		t.Fatalf("downloadAndReplace: %v", err)
	}
	got, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, newBinary) {
		t.Fatalf("binary not replaced: %q", got)
	}
}

// TestDownloadAndReplaceVerifiesSigstoreBundle pins the cosign v3 path:
// the signed checksums are verified through the release's Sigstore bundle
// before the tarball is trusted and the binary is swapped.
func TestDownloadAndReplaceVerifiesSigstoreBundle(t *testing.T) {
	newBinary := []byte("signed replacement")
	tag := "v9.9.9"
	artifact := artifactNameFor(tag)
	tarball := mkTarGz(t, "witself-admin", newBinary)
	bundle := []byte(`{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json"}`)
	called := false
	stubSignedRelease(t, tag, artifact, tarball, map[string][]byte{
		"checksums.txt.sigstore.json": bundle,
	}, func(_ context.Context, bin string, args ...string) ([]byte, error) {
		called = true
		if bin != "/fake/cosign" || len(args) == 0 || args[0] != "verify-blob" {
			t.Fatalf("cosign invocation = %q %q", bin, args)
		}
		gotBundle, err := os.ReadFile(flagValue(t, args, "--bundle"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(gotBundle, bundle) {
			t.Fatalf("bundle = %q", gotBundle)
		}
		if got := flagValue(t, args, "--certificate-identity"); got != "https://github.com/witwave-ai/witself/.github/workflows/release.yml@refs/tags/"+tag {
			t.Fatalf("certificate identity = %q", got)
		}
		if got := flagValue(t, args, "--certificate-oidc-issuer"); got != "https://token.actions.githubusercontent.com" {
			t.Fatalf("OIDC issuer = %q", got)
		}
		sums, err := os.ReadFile(args[len(args)-1])
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(sums), artifact) {
			t.Fatalf("checksums = %q", sums)
		}
		return nil, nil
	})

	binPath := filepath.Join(t.TempDir(), "witself-admin")
	if err := os.WriteFile(binPath, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := downloadAndReplace(t.Context(), tag, binPath); err != nil {
		t.Fatalf("downloadAndReplace: %v", err)
	}
	if !called {
		t.Fatal("cosign bundle verification was not called")
	}
	got, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, newBinary) {
		t.Fatalf("binary not replaced: %q", got)
	}
}

// TestDownloadAndReplaceBadBundleKeepsBinary pins both fail-closed
// behavior and binary safety. Even when legacy companions exist, an
// invalid present bundle cannot downgrade to the legacy path, and the
// installed executable remains byte-for-byte unchanged.
func TestDownloadAndReplaceBadBundleKeepsBinary(t *testing.T) {
	tag := "v9.9.9"
	artifact := artifactNameFor(tag)
	tarball := mkTarGz(t, "witself-admin", []byte("untrusted replacement"))
	calls := 0
	stubSignedRelease(t, tag, artifact, tarball, map[string][]byte{
		"checksums.txt.sigstore.json": []byte("invalid bundle"),
		"checksums.txt.sig":           []byte("legacy signature"),
		"checksums.txt.pem":           []byte("legacy certificate"),
	}, func(_ context.Context, _ string, args ...string) ([]byte, error) {
		calls++
		if flagValue(t, args, "--bundle") == "" {
			t.Fatal("present bundle was not selected")
		}
		return []byte("bundle signature invalid"), errors.New("exit status 1")
	})

	binPath := filepath.Join(t.TempDir(), "witself-admin")
	oldBinary := []byte("known-good old binary")
	if err := os.WriteFile(binPath, oldBinary, 0o755); err != nil {
		t.Fatal(err)
	}
	err := downloadAndReplace(t.Context(), tag, binPath)
	if err == nil || !strings.Contains(err.Error(), "signature verification FAILED") {
		t.Fatalf("err = %v, want signature refusal", err)
	}
	if calls != 1 {
		t.Fatalf("cosign calls = %d, want one bundle verification and no legacy fallback", calls)
	}
	got, readErr := os.ReadFile(binPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(got, oldBinary) {
		t.Fatalf("installed binary changed after signature failure: %q", got)
	}
}

// TestVerifyChecksumsSignatureLegacyFallback keeps pre-bundle releases
// verifiable: only a missing bundle (HTTP 404) selects detached .sig/.pem
// verification with the same pinned GitHub Actions identity.
func TestVerifyChecksumsSignatureLegacyFallback(t *testing.T) {
	tag := "v9.9.8"
	artifact := artifactNameFor(tag)
	tarball := mkTarGz(t, "witself-admin", []byte("legacy release"))
	sig := []byte("legacy signature")
	cert := []byte("legacy certificate")
	called := false
	stubSignedRelease(t, tag, artifact, tarball, map[string][]byte{
		"checksums.txt.sig": sig,
		"checksums.txt.pem": cert,
	}, func(_ context.Context, _ string, args ...string) ([]byte, error) {
		called = true
		for flag, want := range map[string][]byte{"--signature": sig, "--certificate": cert} {
			got, err := os.ReadFile(flagValue(t, args, flag))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("%s contents = %q", flag, got)
			}
		}
		for _, arg := range args {
			if arg == "--bundle" {
				t.Fatalf("legacy invocation unexpectedly used a bundle: %q", args)
			}
		}
		if got := flagValue(t, args, "--certificate-identity"); got != "https://github.com/witwave-ai/witself/.github/workflows/release.yml@refs/tags/"+tag {
			t.Fatalf("certificate identity = %q", got)
		}
		return nil, nil
	})

	if err := verifyChecksumsSignature(t.Context(), tag, []byte("legacy sums")); err != nil {
		t.Fatalf("verifyChecksumsSignature: %v", err)
	}
	if !called {
		t.Fatal("legacy cosign verification was not called")
	}
}

// TestVerifyChecksumsSignatureBundleFetchFailureDoesNotDowngrade pins
// the fallback boundary: legacy verification is compatibility for a
// release that genuinely predates bundles, not a way around a transient
// or adversarial failure while fetching a published bundle.
func TestVerifyChecksumsSignatureBundleFetchFailureDoesNotDowngrade(t *testing.T) {
	tag := "v9.9.9"
	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/%s/checksums.txt.sigstore.json", tag), func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "temporary failure", http.StatusServiceUnavailable)
	})
	mux.HandleFunc(fmt.Sprintf("/%s/checksums.txt.sig", tag), func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("legacy signature that must not be used"))
	})
	mux.HandleFunc(fmt.Sprintf("/%s/checksums.txt.pem", tag), func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("legacy certificate that must not be used"))
	})
	srv := httptest.NewServer(mux)
	oldBase, oldLook, oldRun := releaseDownloadBase, cosignLook, cosignRun
	releaseDownloadBase = srv.URL
	cosignLook = func() (string, error) { return "/fake/cosign", nil }
	cosignCalled := false
	cosignRun = func(context.Context, string, ...string) ([]byte, error) {
		cosignCalled = true
		return nil, nil
	}
	t.Cleanup(func() {
		releaseDownloadBase = oldBase
		cosignLook = oldLook
		cosignRun = oldRun
		srv.Close()
	})

	err := verifyChecksumsSignature(t.Context(), tag, []byte("sums"))
	if err == nil || !strings.Contains(err.Error(), "fetch checksums signature bundle") || !strings.Contains(err.Error(), "HTTP 503") {
		t.Fatalf("err = %v, want bundle HTTP 503 refusal", err)
	}
	if cosignCalled {
		t.Fatal("non-404 bundle fetch failure downgraded to legacy verification")
	}
}

func TestReleaseWorkflowIdentityRejectsNonCanonicalTags(t *testing.T) {
	want := "https://github.com/witwave-ai/witself/.github/workflows/release.yml@refs/tags/v1.2.3"
	got, err := releaseWorkflowIdentity("v1.2.3")
	if err != nil || got != want {
		t.Fatalf("releaseWorkflowIdentity(valid) = %q, %v", got, err)
	}
	for _, tag := range []string{
		"1.2.3",
		"v1.2",
		"v1.2.3-rc.1",
		"v1.2.3/../../main",
		"v01.2.3",
		"v1.2.3 ",
	} {
		if got, err := releaseWorkflowIdentity(tag); err == nil {
			t.Errorf("releaseWorkflowIdentity(%q) = %q, want error", tag, got)
		}
	}
}

// TestDownloadAndReplaceIgnoresPredictableStagingSymlink regresses the
// old deterministic staging pathname. An attacker-controlled symlink at
// that former path must neither receive the new bytes nor alter its target;
// exclusive random staging still completes the requested atomic swap.
func TestDownloadAndReplaceIgnoresPredictableStagingSymlink(t *testing.T) {
	newBinary := []byte("trusted new binary")
	tag := "v9.9.9"
	stubRelease(t, tag, artifactNameFor(tag), mkTarGz(t, "witself-admin", newBinary), "")

	dir := t.TempDir()
	binPath := filepath.Join(dir, "witself-admin")
	victimPath := filepath.Join(dir, "victim")
	victim := []byte("do not overwrite")
	if err := os.WriteFile(binPath, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(victimPath, victim, 0o600); err != nil {
		t.Fatal(err)
	}
	oldPredictablePath := binPath + ".upgrade-" + strings.TrimPrefix(tag, "v")
	if err := os.Symlink(victimPath, oldPredictablePath); err != nil {
		t.Fatal(err)
	}

	if err := downloadAndReplace(t.Context(), tag, binPath); err != nil {
		t.Fatalf("downloadAndReplace: %v", err)
	}
	gotVictim, err := os.ReadFile(victimPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotVictim, victim) {
		t.Fatalf("predictable staging symlink target changed: %q", gotVictim)
	}
	gotBinary, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotBinary, newBinary) {
		t.Fatalf("binary not replaced: %q", gotBinary)
	}
	if info, err := os.Lstat(oldPredictablePath); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("old predictable staging symlink was unexpectedly used: info=%v err=%v", info, err)
	}
}

// TestStageAndReplaceBinaryCleansFailedStage proves that a failed final
// rename does not leave executable staging files behind in the target
// directory.
func TestStageAndReplaceBinaryCleansFailedStage(t *testing.T) {
	dir := t.TempDir()
	binPath := filepath.Join(dir, "witself-admin")
	if err := os.Mkdir(binPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binPath, "keep"), []byte("non-empty"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := stageAndReplaceBinary(binPath, "9.9.9", []byte("replacement"))
	if err == nil || !strings.Contains(err.Error(), "swap binary") {
		t.Fatalf("err = %v, want failed final rename", err)
	}
	leftovers, globErr := filepath.Glob(filepath.Join(dir, ".witself-admin.upgrade-9.9.9-*"))
	if globErr != nil {
		t.Fatal(globErr)
	}
	if len(leftovers) != 0 {
		t.Fatalf("failed upgrade left staging files: %q", leftovers)
	}
}

// TestDownloadAndReplaceChecksumMismatch pins the security refusal: a
// tarball whose hash doesn't match checksums.txt must not touch the
// installed binary.
func TestDownloadAndReplaceChecksumMismatch(t *testing.T) {
	tag := "v9.9.9"
	stubRelease(t, tag, artifactNameFor(tag),
		mkTarGz(t, "witself-admin", []byte("evil")),
		strings.Repeat("ab", 32)) // wrong sum

	binPath := filepath.Join(t.TempDir(), "witself-admin")
	if err := os.WriteFile(binPath, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := downloadAndReplace(t.Context(), tag, binPath)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("err = %v, want checksum mismatch refusal", err)
	}
	got, _ := os.ReadFile(binPath)
	if string(got) != "old" {
		t.Fatalf("installed binary was touched despite mismatch: %q", got)
	}
}

// TestDownloadAndReplaceMissingArtifact pins the error when the release
// has no tarball for this OS/arch (e.g. a partial release).
func TestDownloadAndReplaceMissingArtifact(t *testing.T) {
	tag := "v9.9.9"
	// Serve checksums that name a DIFFERENT artifact.
	stubRelease(t, tag, "witself-admin_9.9.9_plan9_mips.tar.gz",
		mkTarGz(t, "witself-admin", []byte("x")), "")

	binPath := filepath.Join(t.TempDir(), "witself-admin")
	err := downloadAndReplace(t.Context(), tag, binPath)
	if err == nil || !strings.Contains(err.Error(), "no artifact") {
		t.Fatalf("err = %v, want no-artifact error", err)
	}
}

// TestExtractFromTarGz pins member selection: finds the named regular
// file anywhere in the archive, errors when absent.
func TestExtractFromTarGz(t *testing.T) {
	content := []byte("binary bytes")
	tb := mkTarGz(t, "dist/witself-admin", content) // nested path — Base match
	got, err := extractFromTarGz(tb, "witself-admin")
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("extract: %v / %q", err, got)
	}
	if _, err := extractFromTarGz(mkTarGz(t, "README.md", []byte("hi")), "witself-admin"); err == nil {
		t.Fatal("archive without the binary must error")
	}
	if _, err := extractFromTarGz([]byte("not a tarball"), "witself-admin"); err == nil {
		t.Fatal("garbage input must error")
	}
}

// TestResumeComposeRestore pins the full back half of the upgrade
// round-trip: a resume snapshot in compose mode must re-enter the
// composer with the draft intact and show the upgraded banner once the
// thread loads.
func TestResumeComposeRestore(t *testing.T) {
	m := newModel(t.Context(), &adminCLI{bin: "/nonexistent"}, nil)
	m = m.withResume(&resumeState{
		Mode:          "compose",
		ThreadAccount: "acc_1",
		ThreadTicket:  "tkt_1",
		Draft:         "half-typed reply",
		UpgradedTo:    "v9.9.9",
	})
	if m.threadAccount != "acc_1" || m.threadTicket != "tkt_1" {
		t.Fatalf("withResume did not seed thread coords: %q/%q", m.threadAccount, m.threadTicket)
	}

	next, _ := m.Update(threadLoadedMsg{res: client.GetSupportTicketResult{}})
	m2 := next.(model)
	if m2.mode != modeCompose {
		t.Fatalf("resume did not re-enter compose: mode = %v", m2.mode)
	}
	if m2.composer.Value() != "half-typed reply" {
		t.Fatalf("draft lost across upgrade: %q", m2.composer.Value())
	}
	if !strings.Contains(m2.status, "upgraded to v9.9.9") {
		t.Fatalf("status = %q, want upgraded banner", m2.status)
	}
	if m2.resume != nil {
		t.Fatal("resume must be consumed exactly once")
	}
}

// TestUpgradeNoopDoesNotRestart pins the brew-lag loop-breaker: a
// channel no-op must surface a status line and NOT quit/re-exec.
func TestUpgradeNoopDoesNotRestart(t *testing.T) {
	m := newModel(t.Context(), &adminCLI{bin: "/nonexistent"}, nil)
	m = m.withSelfUpgrade("/usr/local/bin/witself-admin", "0.0.94")

	next, cmd := m.Update(upgradeAppliedMsg{tag: "v0.0.95", noop: true})
	m2 := next.(model)
	if cmd != nil || m2.relaunch != nil {
		t.Fatal("noop upgrade must not restart")
	}
	if !strings.Contains(m2.status, "not yet available") {
		t.Fatalf("status = %q", m2.status)
	}
}

// TestUpgradeDeferredWhileLoading pins that an installed upgrade never
// restarts while an action subprocess is in flight — the teardown
// would kill it and lose the operator's action.
func TestUpgradeDeferredWhileLoading(t *testing.T) {
	m := newModel(t.Context(), &adminCLI{bin: "/nonexistent"}, nil)
	m = m.withSelfUpgrade("/usr/local/bin/witself-admin", "0.0.94")
	m.mode = modeDetail
	m.threadAccount, m.threadTicket = "acc_1", "tkt_1"
	m.loading = true // reply send in flight

	next, cmd := m.Update(upgradeAppliedMsg{tag: "v0.0.95"})
	m2 := next.(model)
	if cmd != nil || m2.relaunch != nil {
		t.Fatal("upgrade mid-action must defer, not quit")
	}
	if m2.upgradeReadyTag != "v0.0.95" {
		t.Fatalf("deferred tag = %q", m2.upgradeReadyTag)
	}

	// The action completes → actionDoneMsg → relaunch fires.
	next, cmd = m2.Update(actionDoneMsg{label: "reply"})
	m3 := next.(model)
	if m3.relaunch == nil || cmd == nil {
		t.Fatal("relaunch must fire once the in-flight action lands")
	}
}
