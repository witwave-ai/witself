package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
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
	oldBase, oldLook := releaseDownloadBase, cosignLook
	releaseDownloadBase = srv.URL
	cosignLook = func() (string, error) { return "", errors.New("not installed") }
	t.Cleanup(func() {
		releaseDownloadBase = oldBase
		cosignLook = oldLook
		srv.Close()
	})
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
