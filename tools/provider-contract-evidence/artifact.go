package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

const maxBinaryBytes int64 = 512 << 20

var reportedVersion = regexp.MustCompile(`^witself ([0-9A-Za-z.+-]+) \(commit ([0-9a-f]{7,40}), built ([0-9TZ:.-]+)\)\n$`)

// Integrity and read errors are checked while consuming these read-only streams.
// Closing releases resources and cannot change the already-read evidence.
func closeReadOnly(c io.Closer) { _ = c.Close() }

func readBounded(path string, limit int64) ([]byte, error) {
	info, e := os.Lstat(path)
	if e != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errInvalidEvidence
	}
	f, e := os.Open(path)
	if e != nil {
		return nil, errInvalidEvidence
	}
	defer closeReadOnly(f)
	raw, e := io.ReadAll(io.LimitReader(f, limit+1))
	if e != nil || int64(len(raw)) > limit {
		return nil, errInvalidEvidence
	}
	return raw, nil
}
func hashReader(r io.Reader) (string, error) {
	h := sha256.New()
	n, e := io.Copy(h, io.LimitReader(r, maxBinaryBytes+1))
	if e != nil || n > maxBinaryBytes || n == 0 {
		return "", errInvalidEvidence
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func hashFile(path string) (string, error) {
	info, e := os.Lstat(path)
	if e != nil || !info.Mode().IsRegular() || info.Size() > maxBinaryBytes {
		return "", errInvalidEvidence
	}
	f, e := os.Open(path)
	if e != nil {
		return "", errInvalidEvidence
	}
	defer closeReadOnly(f)
	return hashReader(f)
}
func hashBytes(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }
func looseJSON(raw []byte, v any) error {
	if len(raw) == 0 || len(raw) > maxReportBytes {
		return errInvalidEvidence
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if scanJSON(d, 0) != nil || !json.Valid(raw) || json.Unmarshal(raw, v) != nil {
		return errInvalidEvidence
	}
	return nil
}
func archiveBinaryHash(path, goos string) (string, error) {
	want := "witself"
	if goos == "windows" {
		want += ".exe"
	}
	if goos == "windows" {
		r, e := zip.OpenReader(path)
		if e != nil {
			return "", errInvalidEvidence
		}
		defer closeReadOnly(r)
		if len(r.File) != 1 || r.File[0].Name != want || !r.File[0].Mode().IsRegular() || r.File[0].UncompressedSize64 > uint64(maxBinaryBytes) {
			return "", errInvalidEvidence
		}
		f, e := r.File[0].Open()
		if e != nil {
			return "", errInvalidEvidence
		}
		defer closeReadOnly(f)
		return hashReader(f)
	}
	f, e := os.Open(path)
	if e != nil {
		return "", errInvalidEvidence
	}
	defer closeReadOnly(f)
	g, e := gzip.NewReader(f)
	if e != nil {
		return "", errInvalidEvidence
	}
	defer closeReadOnly(g)
	r := tar.NewReader(io.LimitReader(g, maxBinaryBytes+(1<<20)))
	h, e := r.Next()
	if e != nil || h.Name != want || h.Typeflag != tar.TypeReg || h.Mode&0o111 == 0 || h.Size > maxBinaryBytes {
		return "", errInvalidEvidence
	}
	digest, e := hashReader(r)
	if e != nil {
		return "", e
	}
	if _, e = r.Next(); e != io.EOF {
		return "", errInvalidEvidence
	}
	// Read through the gzip trailer so a truncated stream or bad checksum fails.
	if n, e := io.Copy(io.Discard, io.LimitReader(g, (1<<20)+1)); e != nil || n > 1<<20 {
		return "", errInvalidEvidence
	}
	return digest, nil
}

// These are a projection of GoReleaser's metadata, not public report fields.
type goreleaserArtifact struct {
	Name   string `json:"name"`
	Type   string `json:"type"`
	GOOS   string `json:"goos"`
	GOARCH string `json:"goarch"`
	Extra  struct {
		ID       string            `json:"ID"`
		Format   string            `json:"Format"`
		Binaries []string          `json:"Binaries"`
		Files    []json.RawMessage `json:"Files"`
		Checksum string            `json:"Checksum"`
	} `json:"extra"`
}

func inspectArtifact(dist, binary string, c cell) (*artifact, error) {
	metadata, e := readBounded(filepath.Join(dist, "metadata.json"), maxReportBytes)
	if e != nil {
		return nil, e
	}
	var m struct {
		Version string `json:"version"`
		Commit  string `json:"commit"`
	}
	if looseJSON(metadata, &m) != nil || m.Commit != c.Identity.SourceCommit || len(m.Version) > 100 || !versionPattern.MatchString(m.Version) || !strings.Contains(m.Version, "SNAPSHOT") {
		return nil, errInvalidEvidence
	}
	raw, e := readBounded(filepath.Join(dist, "artifacts.json"), maxReportBytes)
	if e != nil {
		return nil, e
	}
	var artifacts []goreleaserArtifact
	if looseJSON(raw, &artifacts) != nil {
		return nil, errInvalidEvidence
	}
	name := archiveName(m.Version, c.GOOS, c.GOARCH)
	format, member := "tar.gz", "witself"
	if c.GOOS == "windows" {
		format, member = "zip", "witself.exe"
	}
	count, checksums := 0, 0
	manifestHash := ""
	for _, a := range artifacts {
		if a.Type == "Checksum" && a.Name == "checksums.txt" {
			checksums++
		}
		if a.Type == "Archive" && a.GOOS == c.GOOS && a.GOARCH == c.GOARCH && a.Extra.ID == "witself" {
			count++
			if a.Name != name || a.Extra.Format != format || len(a.Extra.Files) != 0 || len(a.Extra.Binaries) != 1 || a.Extra.Binaries[0] != member {
				return nil, errInvalidEvidence
			}
			manifestHash = a.Extra.Checksum
		}
	}
	if count != 1 || checksums != 1 {
		return nil, errInvalidEvidence
	}
	checksumRaw, e := readBounded(filepath.Join(dist, "checksums.txt"), maxReportBytes)
	if e != nil {
		return nil, e
	}
	checksumHash := ""
	matches := 0
	for _, line := range strings.Split(string(checksumRaw), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 || !shaPattern.MatchString(fields[0]) {
			return nil, errInvalidEvidence
		}
		if strings.TrimPrefix(fields[1], "*") == name {
			matches++
			checksumHash = fields[0]
		}
	}
	archivePath := filepath.Join(dist, name)
	archiveHash, e := hashFile(archivePath)
	if e != nil || matches != 1 || archiveHash != checksumHash || manifestHash != "sha256:"+archiveHash {
		return nil, errInvalidEvidence
	}
	binaryHash, e := hashFile(binary)
	if e != nil {
		return nil, e
	}
	memberHash, e := archiveBinaryHash(archivePath, c.GOOS)
	if e != nil || memberHash != binaryHash {
		return nil, errInvalidEvidence
	}
	return &artifact{Kind: "goreleaser_snapshot", SnapshotVersion: m.Version, ArchiveName: name, ArchiveSHA256: archiveHash, BinarySHA256: binaryHash, ChecksumsSHA256: hashBytes(checksumRaw)}, nil
}

type cappedBuffer struct {
	bytes.Buffer
	limit    int
	overflow bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := b.limit - b.Len()
	if n > remaining {
		b.overflow = true
		p = p[:remaining]
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}

// version starts the real CLI. Its legacy migration must see only disposable
// directories and an already-complete private marker, never the runner's home.
func isolatedVersionEnvironment(base []string) ([]string, func(), error) {
	dir, e := os.MkdirTemp("", "provider-contract-version-")
	if e != nil {
		return nil, nil, errInvalidEvidence
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	home, wh, config, temp := filepath.Join(dir, "home"), filepath.Join(dir, "witself"), filepath.Join(dir, "config"), filepath.Join(dir, "tmp")
	for _, p := range []string{home, wh, config, temp, filepath.Join(wh, "migrations")} {
		if os.MkdirAll(p, 0o700) != nil {
			cleanup()
			return nil, nil, errInvalidEvidence
		}
	}
	if os.WriteFile(filepath.Join(wh, "migrations", "foreground-messaging-v1"), []byte("completed\n"), 0o600) != nil {
		cleanup()
		return nil, nil, errInvalidEvidence
	}
	env := []string{}
	for _, pair := range base {
		key, _, _ := strings.Cut(pair, "=")
		switch strings.ToUpper(key) {
		case "PATH", "SYSTEMROOT", "WINDIR", "COMSPEC", "PATHEXT":
			env = append(env, pair)
		}
	}
	for key, value := range map[string]string{"HOME": home, "USERPROFILE": home, "WITSELF_HOME": wh, "XDG_CONFIG_HOME": config, "TMPDIR": temp, "TMP": temp, "TEMP": temp} {
		env = append(env, key+"="+value)
	}
	return env, cleanup, nil
}
func probeVersion(ctx context.Context, binary string, a *artifact, c cell, makeCommand commandFactory) error {
	env, cleanup, e := isolatedVersionEnvironment(os.Environ())
	if e != nil {
		return e
	}
	defer cleanup()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := makeCommand(ctx, binary, "version")
	cmd.Env = env
	cmd.Dir = envValue(env, "HOME")
	cmd.WaitDelay = 2 * time.Second
	out := &cappedBuffer{limit: 4096}
	cmd.Stdout = out
	cmd.Stderr = io.Discard
	if cmd.Run() != nil || out.overflow {
		return errInvalidEvidence
	}
	match := reportedVersion.FindStringSubmatch(out.String())
	if len(match) != 4 || match[1] != a.SnapshotVersion || !strings.HasPrefix(c.Identity.SourceCommit, match[2]) {
		return errInvalidEvidence
	}
	if _, e := time.Parse(time.RFC3339, match[3]); e != nil {
		return errInvalidEvidence
	}
	a.BinaryReportedCommit = match[2]
	info, e := buildinfo.ReadFile(binary)
	if e != nil {
		return errInvalidEvidence
	}
	settings := map[string]string{}
	for _, s := range info.Settings {
		settings[s.Key] = s.Value
	}
	if settings["GOOS"] != c.GOOS || settings["GOARCH"] != c.GOARCH || settings["vcs.modified"] == "true" {
		return errInvalidEvidence
	}
	a.BinaryVCSRevision = settings["vcs.revision"]
	if a.BinaryVCSRevision != "" && a.BinaryVCSRevision != c.Identity.SourceCommit {
		return errInvalidEvidence
	}
	if !validArtifact(a, c) {
		return errInvalidEvidence
	}
	return nil
}
func envValue(env []string, key string) string {
	for _, v := range env {
		k, value, ok := strings.Cut(v, "=")
		if ok && strings.EqualFold(k, key) {
			return value
		}
	}
	return ""
}

type commandFactory func(context.Context, string, ...string) *exec.Cmd

func nativeTarget(target string) bool {
	return nativeTargets[target] == [2]string{runtime.GOOS, runtime.GOARCH}
}
