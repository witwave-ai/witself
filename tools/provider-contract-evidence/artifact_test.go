package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func tarFixture(t *testing.T, name string, body []byte, tail int, badTrailer bool) []byte {
	t.Helper()
	var raw bytes.Buffer
	gz := gzip.NewWriter(&raw)
	tw := tar.NewWriter(gz)
	if tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}) != nil {
		t.Fatal("header")
	}
	if _, e := tw.Write(body); e != nil {
		t.Fatal(e)
	}
	if tw.Close() != nil {
		t.Fatal("tar close")
	}
	if tail > 0 {
		if _, e := gz.Write(bytes.Repeat([]byte{0}, tail)); e != nil {
			t.Fatal(e)
		}
	}
	if gz.Close() != nil {
		t.Fatal("gzip close")
	}
	b := raw.Bytes()
	if badTrailer {
		b[len(b)-8] ^= 1
	}
	return b
}
func TestArchiveTailRequiresObservedEOF(t *testing.T) {
	for _, bad := range []bool{false, true} {
		name := "oversized tail"
		if bad {
			name = "oversized tail masks bad checksum"
		}
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "archive.tar.gz")
			raw := tarFixture(t, "witself", []byte("synthetic binary"), (1<<20)+4096, bad)
			if os.WriteFile(p, raw, 0o600) != nil {
				t.Fatal("write")
			}
			if _, e := archiveBinaryHash(p, "linux"); e == nil {
				t.Fatal("accepted archive without observing bounded gzip EOF")
			}
		})
	}
}
func TestArchiveMemberContracts(t *testing.T) {
	for _, name := range []string{"witself", "../witself", "nested/witself", "witself.exe"} {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "a.tar.gz")
			body := []byte("SYNTHETIC executable fixture")
			_ = os.WriteFile(p, tarFixture(t, name, body, 0, false), 0o600)
			got, e := archiveBinaryHash(p, "linux")
			if name == "witself" {
				if e != nil || got != hashBytes(body) {
					t.Fatal("valid member hash refused")
				}
			} else if e == nil {
				t.Fatal("unsafe archive member accepted")
			}
		})
	}
	t.Run("gzip checksum", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "a.tar.gz")
		_ = os.WriteFile(p, tarFixture(t, "witself", []byte("fixture"), 0, true), 0o600)
		if _, e := archiveBinaryHash(p, "linux"); e == nil {
			t.Fatal("corrupt gzip accepted")
		}
	})
	for _, name := range []string{"witself.exe", "../witself.exe", "witself"} {
		t.Run("zip "+name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "a.zip")
			var b bytes.Buffer
			w := zip.NewWriter(&b)
			f, e := w.Create(name)
			if e != nil {
				t.Fatal(e)
			}
			_, _ = f.Write([]byte("fixture"))
			if w.Close() != nil {
				t.Fatal("close")
			}
			_ = os.WriteFile(p, b.Bytes(), 0o600)
			got, e := archiveBinaryHash(p, "windows")
			if name == "witself.exe" {
				if e != nil || got != hashBytes([]byte("fixture")) {
					t.Fatal("valid zip refused")
				}
			} else if e == nil {
				t.Fatal("unsafe zip member accepted")
			}
		})
	}
}
func artifactFixture(t *testing.T) (string, string, cell) {
	t.Helper()
	dist := t.TempDir()
	c := fixture(t).Cells[0]
	body := []byte("SYNTHETIC non-executable fixture bytes")
	archive := tarFixture(t, "witself", body, 0, false)
	name := archiveName(c.Artifact.SnapshotVersion, c.GOOS, c.GOARCH)
	put := func(name string, b []byte) {
		t.Helper()
		if os.WriteFile(filepath.Join(dist, name), b, 0o600) != nil {
			t.Fatal("write fixture")
		}
	}
	put(name, archive)
	put("installed", body)
	put("metadata.json", marshal(t, map[string]any{"version": c.Artifact.SnapshotVersion, "commit": c.Identity.SourceCommit}))
	meta := []any{map[string]any{"name": "checksums.txt", "type": "Checksum"}, map[string]any{"name": name, "type": "Archive", "goos": "linux", "goarch": "amd64", "extra": map[string]any{"ID": "witself", "Format": "tar.gz", "Binaries": []string{"witself"}, "Files": []string{}, "Checksum": "sha256:" + hashBytes(archive)}}}
	put("artifacts.json", marshal(t, meta))
	put("checksums.txt", []byte(hashBytes(archive)+"  "+name+"\n"))
	return dist, filepath.Join(dist, "installed"), c
}
func TestArtifactProvenance(t *testing.T) {
	t.Run("actual hashes", func(t *testing.T) {
		dist, binary, c := artifactFixture(t)
		a, e := inspectArtifact(dist, binary, c)
		if e != nil {
			t.Fatal("valid fixture refused")
		}
		body, _ := os.ReadFile(binary)
		if a.BinarySHA256 != hashBytes(body) || a.Kind != "goreleaser_snapshot" || a.BinaryReportedCommit != "" {
			t.Fatal("wrong binary identity")
		}
	})
	for _, kind := range []string{"installed mismatch", "archive mismatch", "checksum duplicate", "checksum missing", "metadata commit", "metadata duplicate", "archive metadata hash", "extra member metadata", "binary symlink"} {
		t.Run(kind, func(t *testing.T) {
			dist, binary, c := artifactFixture(t)
			name := archiveName(c.Artifact.SnapshotVersion, c.GOOS, c.GOARCH)
			switch kind {
			case "installed mismatch":
				_ = os.WriteFile(binary, []byte("wrong"), 0o600)
			case "archive mismatch":
				_ = os.WriteFile(filepath.Join(dist, name), tarFixture(t, "witself", []byte("other"), 0, false), 0o600)
			case "checksum duplicate":
				p := filepath.Join(dist, "checksums.txt")
				b, _ := os.ReadFile(p)
				_ = os.WriteFile(p, append(b, b...), 0o600)
			case "checksum missing":
				_ = os.WriteFile(filepath.Join(dist, "checksums.txt"), nil, 0o600)
			case "metadata commit":
				_ = os.WriteFile(filepath.Join(dist, "metadata.json"), marshal(t, map[string]any{"version": c.Artifact.SnapshotVersion, "commit": strings.Repeat("b", 40)}), 0o600)
			case "metadata duplicate":
				p := filepath.Join(dist, "metadata.json")
				b, _ := os.ReadFile(p)
				_ = os.WriteFile(p, append([]byte(`{"commit":"`+c.Identity.SourceCommit+`",`), b[1:]...), 0o600)
			case "archive metadata hash", "extra member metadata":
				p := filepath.Join(dist, "artifacts.json")
				b, _ := os.ReadFile(p)
				var entries []map[string]any
				_ = json.Unmarshal(b, &entries)
				extra := entries[1]["extra"].(map[string]any)
				if kind == "archive metadata hash" {
					extra["Checksum"] = "sha256:" + strings.Repeat("b", 64)
				} else {
					extra["Binaries"] = []string{"witself", "unexpected"}
				}
				_ = os.WriteFile(p, marshal(t, entries), 0o600)
			case "binary symlink":
				b, _ := os.ReadFile(binary)
				p := filepath.Join(t.TempDir(), "private")
				_ = os.WriteFile(p, b, 0o600)
				_ = os.Remove(binary)
				if e := os.Symlink(p, binary); e != nil {
					t.Skip("symlink unavailable")
				}
			}
			if _, e := inspectArtifact(dist, binary, c); e == nil {
				t.Fatal("invalid artifact provenance accepted")
			}
		})
	}
}
func TestVersionIsolation(t *testing.T) {
	env, cleanup, e := isolatedVersionEnvironment([]string{"HOME=/private/REAL_HOME", "USERPROFILE=/private/REAL_HOME", "WITSELF_HOME=/private/REAL_RUNTIME", "XDG_CONFIG_HOME=/private/REAL_CONFIG", "API_TOKEN=PRIVATE_SENTINEL", "PATH=/fixture/bin", "SystemRoot=C:\\Windows"})
	if e != nil {
		t.Fatal(e)
	}
	home, wh := envValue(env, "HOME"), envValue(env, "WITSELF_HOME")
	defer cleanup()
	for _, entry := range env {
		if strings.Contains(entry, "PRIVATE_SENTINEL") || strings.Contains(entry, "REAL_") {
			t.Fatal("real runtime/secret inherited")
		}
	}
	if home == "" || wh == "" || home == wh || !filepath.IsAbs(home) || envValue(env, "USERPROFILE") != home {
		t.Fatal("missing disposable isolation")
	}
	p := filepath.Join(wh, "migrations", "foreground-messaging-v1")
	b, e := os.ReadFile(p)
	if e != nil || string(b) != "completed\n" {
		t.Fatal("migration completion marker missing")
	}
	info, e := os.Stat(p)
	if e != nil || (!info.Mode().IsRegular()) || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) {
		t.Fatal("migration marker not private regular file")
	}
	cleanup()
	if _, e := os.Stat(home); !os.IsNotExist(e) {
		t.Fatal("disposable home retained")
	}
}
