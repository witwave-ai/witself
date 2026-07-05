package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/version"
)

// Self-upgrade: the dashboard is meant to stay open all day, so it
// checks for a newer release occasionally (startup + every 6h),
// upgrades itself through whatever channel installed it, and re-execs
// into the same view (resume state travels via --resume). "dev" builds
// never auto-upgrade — a source build must not clobber itself with a
// release binary.

const upgradeCheckInterval = 6 * time.Hour

// vars (not consts) so tests can point them at an httptest server and
// stub the cosign lookup for hermetic runs.
var (
	releaseLatestURL    = "https://api.github.com/repos/witwave-ai/witself/releases/latest"
	releaseDownloadBase = "https://github.com/witwave-ai/witself/releases/download"
	cosignLook          = func() (string, error) { return exec.LookPath("cosign") }
)

// latestReleaseTag asks GitHub for the newest release tag ("v0.0.95").
// Unauthenticated — the occasional-cadence check stays far under the
// per-IP rate limit.
func latestReleaseTag(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releaseLatestURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "witself-admin/"+version.Version)
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("release check: HTTP %d", resp.StatusCode)
	}
	var body struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return "", err
	}
	if body.TagName == "" {
		return "", fmt.Errorf("release check: empty tag")
	}
	return body.TagName, nil
}

// newerVersion reports whether latest ("v0.0.95" or "0.0.95") is
// strictly newer than current ("0.0.94"). "dev" or unparseable
// versions never upgrade — fail safe toward doing nothing.
func newerVersion(current, latest string) bool {
	cur, ok1 := parseSemver(current)
	lat, ok2 := parseSemver(latest)
	if !ok1 || !ok2 {
		return false
	}
	for i := 0; i < 3; i++ {
		if lat[i] != cur[i] {
			return lat[i] > cur[i]
		}
	}
	return false
}

func parseSemver(s string) ([3]int, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	parts := strings.SplitN(s, "-", 2) // strip prerelease suffix
	nums := strings.Split(parts[0], ".")
	if len(nums) != 3 {
		return [3]int{}, false
	}
	var out [3]int
	for i, n := range nums {
		v, err := strconv.Atoi(n)
		if err != nil || v < 0 {
			return [3]int{}, false
		}
		out[i] = v
	}
	return out, true
}

// installMethod classifies how this binary got onto the machine so the
// upgrade goes back through the same door.
//   - "brew":   the resolved path lives in a Homebrew Cellar
//   - "binary": everything else — upgrade by direct release download
//     with checksum verification (mirrors install.sh)
func installMethod(binPath string) string {
	resolved, err := filepath.EvalSymlinks(binPath)
	if err != nil {
		resolved = binPath
	}
	if strings.Contains(resolved, "/Cellar/") || strings.Contains(resolved, "/homebrew/") {
		return "brew"
	}
	return "binary"
}

// doUpgrade brings the on-disk binary to the target release. It does
// NOT restart the process — the caller verifies the installed version
// and re-execs after this returns. Success here means only "the
// channel reported success", NOT "the target version is installed":
// brew exits 0 as a no-op when the tap formula lags the GitHub tag.
func doUpgrade(ctx context.Context, method, tag, binPath string) error {
	switch method {
	case "brew":
		// brew upgrade is idempotent and pulls the formula the tap
		// published for this tag. Output is discarded — failures
		// surface via exit code + stderr.
		cmd := exec.CommandContext(ctx, "brew", "upgrade", "witself-admin")
		out, err := cmd.CombinedOutput()
		if err != nil {
			msg := strings.TrimSpace(string(out))
			if len(msg) > 200 {
				msg = msg[:200]
			}
			return fmt.Errorf("brew upgrade: %v: %s", err, msg)
		}
		return nil
	default:
		return downloadAndReplace(ctx, tag, binPath)
	}
}

// verifyInstalledVersion runs the freshly-installed binary's `version`
// subcommand and reports whether it now carries a version >= tag. This
// is the loop-breaker for channel lag: a no-op brew upgrade (tap
// formula not yet published for the tag) exits 0 without changing the
// binary, and blindly re-execing the same version would restart-loop
// every check interval until the tap caught up.
func verifyInstalledVersion(ctx context.Context, binPath, tag string) bool {
	cmd := exec.CommandContext(ctx, binPath, "version")
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	// Output shape: "witself-admin 0.0.95 (commit abc, built ...)".
	fields := strings.Fields(string(out))
	if len(fields) < 2 {
		return false
	}
	installed := fields[1]
	// Installed >= tag ⇔ NOT (installed < tag).
	return !newerVersion(installed, tag)
}

// downloadAndReplace fetches the release tarball for this OS/arch,
// verifies its SHA-256 against checksums.txt, extracts the binary, and
// atomically swaps it over binPath.
//
// Trust model, stated honestly: checksums.txt is fetched from the SAME
// origin as the tarball over HTTPS, so on its own the hash only
// protects against corruption/truncation — an attacker who can replace
// release assets can replace both files. The release pipeline
// cosign-signs checksums.txt (keyless, GitHub-Actions OIDC); when a
// cosign binary is available on PATH we fetch the .sig/.pem companions
// and verify the signature against the pinned repository identity
// before trusting the sums, which closes the asset-swap window.
// Without cosign we proceed checksum-only — parity with install.sh —
// because vendoring the sigstore verification tree into this binary
// is a dependency-budget decision deferred to its own issue.
func downloadAndReplace(ctx context.Context, tag, binPath string) error {
	ver := strings.TrimPrefix(tag, "v")
	name := fmt.Sprintf("witself-admin_%s_%s_%s.tar.gz", ver, runtime.GOOS, runtime.GOARCH)

	sums, err := fetchBytes(ctx, fmt.Sprintf("%s/%s/checksums.txt", releaseDownloadBase, tag), 1<<20)
	if err != nil {
		return fmt.Errorf("fetch checksums: %w", err)
	}
	if err := verifyChecksumsSignature(ctx, tag, sums); err != nil {
		return err
	}
	wantSum := ""
	for _, line := range strings.Split(string(sums), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == name {
			wantSum = fields[0]
			break
		}
	}
	if wantSum == "" {
		return fmt.Errorf("release %s has no artifact %s", tag, name)
	}

	tarball, err := fetchBytes(ctx, fmt.Sprintf("%s/%s/%s", releaseDownloadBase, tag, name), 128<<20)
	if err != nil {
		return fmt.Errorf("fetch tarball: %w", err)
	}
	sum := sha256.Sum256(tarball)
	if hex.EncodeToString(sum[:]) != wantSum {
		return fmt.Errorf("checksum mismatch for %s — refusing to install", name)
	}

	bin, err := extractFromTarGz(tarball, "witself-admin")
	if err != nil {
		return err
	}

	// Atomic swap: write next to the target (same filesystem), then
	// rename over it. A crash mid-write leaves the old binary intact.
	tmp := binPath + ".upgrade-" + ver
	if err := os.WriteFile(tmp, bin, 0o755); err != nil {
		return fmt.Errorf("stage new binary: %w", err)
	}
	if err := os.Rename(tmp, binPath); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("swap binary: %w", err)
	}
	return nil
}

// verifyChecksumsSignature verifies the cosign keyless signature over
// checksums.txt when a cosign binary is available. Verification FAILURE
// (cosign present, signature bad) hard-fails the upgrade; cosign being
// absent skips with no error (checksum-only parity with install.sh).
// The certificate identity is pinned to this repository's GitHub
// Actions workflows so a signature from any other Sigstore identity is
// rejected.
func verifyChecksumsSignature(ctx context.Context, tag string, sums []byte) error {
	cosignBin, err := cosignLook()
	if err != nil {
		return nil // no cosign — best-effort mode
	}
	sig, err := fetchBytes(ctx, fmt.Sprintf("%s/%s/checksums.txt.sig", releaseDownloadBase, tag), 1<<20)
	if err != nil {
		return fmt.Errorf("fetch checksums signature: %w", err)
	}
	cert, err := fetchBytes(ctx, fmt.Sprintf("%s/%s/checksums.txt.pem", releaseDownloadBase, tag), 1<<20)
	if err != nil {
		return fmt.Errorf("fetch checksums certificate: %w", err)
	}
	dir, err := os.MkdirTemp("", "witself-upgrade-verify-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	sumsPath := filepath.Join(dir, "checksums.txt")
	sigPath := filepath.Join(dir, "checksums.txt.sig")
	certPath := filepath.Join(dir, "checksums.txt.pem")
	for p, b := range map[string][]byte{sumsPath: sums, sigPath: sig, certPath: cert} {
		if err := os.WriteFile(p, b, 0o600); err != nil {
			return err
		}
	}
	cmd := exec.CommandContext(ctx, cosignBin, "verify-blob",
		"--certificate", certPath,
		"--signature", sigPath,
		"--certificate-identity-regexp", `^https://github\.com/witwave-ai/witself/`,
		"--certificate-oidc-issuer", "https://token.actions.githubusercontent.com",
		sumsPath,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(out))
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return fmt.Errorf("checksums signature verification FAILED — refusing to install: %s", msg)
	}
	return nil
}

func fetchBytes(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "witself-admin/"+version.Version)
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

func extractFromTarGz(tarball []byte, binaryName string) ([]byte, error) {
	gz, err := gzip.NewReader(strings.NewReader(string(tarball)))
	if err != nil {
		return nil, fmt.Errorf("gunzip: %w", err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("untar: %w", err)
		}
		if filepath.Base(hdr.Name) == binaryName && hdr.Typeflag == tar.TypeReg {
			return io.ReadAll(io.LimitReader(tr, 256<<20))
		}
	}
	return nil, fmt.Errorf("tarball has no %q", binaryName)
}

// resumeState is the view snapshot that survives a self-upgrade
// re-exec: whatever the operator was looking at — including a
// half-typed reply draft — comes back exactly as it was.
type resumeState struct {
	Mode          string `json:"mode"` // "list" | "detail" | "compose"
	Cursor        int    `json:"cursor"`
	ThreadAccount string `json:"thread_account,omitempty"`
	ThreadTicket  string `json:"thread_ticket,omitempty"`
	Draft         string `json:"draft,omitempty"`
	UpgradedTo    string `json:"upgraded_to,omitempty"`
}

func (r resumeState) encode() string {
	buf, err := json.Marshal(r)
	if err != nil {
		return ""
	}
	return hex.EncodeToString(buf)
}

func decodeResumeState(s string) (resumeState, error) {
	var r resumeState
	buf, err := hex.DecodeString(s)
	if err != nil {
		return r, err
	}
	err = json.Unmarshal(buf, &r)
	return r, err
}
