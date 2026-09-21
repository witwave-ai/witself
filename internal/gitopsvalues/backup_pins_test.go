package gitopsvalues

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const backupReleaseRepository = "ghcr.io/witwave-ai/images/witself-postgres-backup"

func TestReadBackupImagePins(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	for _, tc := range []struct {
		name  string
		image string
		bad   bool
		set   bool
	}{
		{name: "absent"},
		{name: "empty map", image: "{}"},
		{name: "empty fields", image: `{repository: "", tag: "", digest: ""}`},
		{name: "legacy", image: "postgres:18-alpine3.23", set: true},
		{name: "tag only", image: `{repository: postgres, tag: "18"}`, set: true},
		{name: "registry port", image: `{repository: localhost:5000/a/b, tag: release-1}`, set: true},
		{name: "repository separators", image: `{repository: ghcr.io/a__b/c--d, tag: release-1}`, set: true},
		{name: "canonical", image: `{repository: ` + backupReleaseRepository + `, tag: 0.0.999, digest: ` + digest + `}`, set: true},
		{name: "empty scalar", image: `""`, bad: true},
		{name: "null image", image: "null", bad: true},
		{name: "sequence image", image: "[]", bad: true},
		{name: "missing repository", image: `{tag: 0.0.999}`, bad: true},
		{name: "missing tag", image: `{repository: postgres}`, bad: true},
		{name: "repository has tag", image: `{repository: postgres:18, tag: 0.0.999}`, bad: true},
		{name: "nested port", image: `{repository: ghcr.io/a:5000/b, tag: 0.0.999}`, bad: true},
		{name: "tag whitespace", image: `{repository: postgres, tag: "0.0.999 "}`, bad: true},
		{name: "tag numeric", image: `{repository: postgres, tag: 18}`, bad: true},
		{name: "digest short", image: `{repository: postgres, tag: "18", digest: sha256:short}`, bad: true},
		{name: "digest uppercase", image: `{repository: postgres, tag: "18", digest: sha256:` + strings.Repeat("A", 64) + `}`, bad: true},
		{name: "digest boolean", image: `{repository: postgres, tag: "18", digest: false}`, bad: true},
		{name: "digest null", image: `{repository: postgres, tag: "18", digest: null}`, bad: true},
		{name: "digest map", image: `{repository: postgres, tag: "18", digest: {}}`, bad: true},
		{name: "duplicate", image: `{repository: postgres, tag: "18", tag: "19"}`, bad: true},
		{name: "unknown", image: `{repository: postgres, tag: "18", ignored: true}`, bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := "apps:\n  civoPostgres:\n    backup:\n      enabled: true\n"
			if tc.image != "" {
				body += "      image: " + tc.image + "\n"
			}
			got, err := parseBackupImagePins([]byte(body))
			if tc.bad {
				if err == nil {
					t.Fatal("invalid backup image pins accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if (got != nil) != tc.set {
				t.Fatal("backup image presence changed")
			}
		})
	}
}

func TestRollBackupImagePreservesOtherBytesAndRegenerates(t *testing.T) {
	for _, cell := range []string{"civo-sandbox-use1-backup", "civo-sandbox-use1-serving"} {
		t.Run(cell, func(t *testing.T) {
			root := copyGenerationFixture(t)
			serverDigest := "sha256:" + strings.Repeat("a", 64)
			backup := &BackupImagePins{Repository: backupReleaseRepository, Tag: "0.0.999", Digest: "sha256:" + strings.Repeat("b", 64)}
			// Establish the independent server-only output, then replace only the
			// expected backup block to compare every other byte and every cell.
			if err := RollCell(root, cell, backup.Tag, serverDigest, io.Discard); err != nil {
				t.Fatal(err)
			}
			before := readCellFixtureBytes(t, root)
			var out bytes.Buffer
			if err := RollCellWithBackupImage(root, cell, backup.Tag, serverDigest, backup, &out); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(out.String(), serverDigest) || strings.Contains(out.String(), backup.Digest) {
				t.Fatal("pin writer output contains digest bytes")
			}
			after := readCellFixtureBytes(t, root)
			block := "      image:\n        repository: \"" + backup.Repository + "\"\n        tag: \"" + backup.Tag + "\"\n        digest: " + backup.Digest + "\n"
			for name, body := range before {
				expected := body
				if name == cell {
					expected = setFixtureYAMLField(t, body, "apps.civoPostgres.backup.image", block, "enabled")
				}
				if !bytes.Equal(after[name], expected) {
					t.Fatalf("backup pin changed unrelated bytes in %s", name)
				}
			}
			if err := Check(root, io.Discard); err != nil {
				t.Fatal(err)
			}
			if err := Write(root, io.Discard); err != nil {
				t.Fatal(err)
			}
			for name, body := range readCellFixtureBytes(t, root) {
				if !bytes.Equal(body, after[name]) {
					t.Fatalf("ordinary generation changed pinned cell %s", name)
				}
			}
			// A future server-only roll preserves the exact prior backup pin.
			if err := RollCell(root, cell, "0.1.0", "sha256:"+strings.Repeat("c", 64), io.Discard); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, filepath.FromSlash(valuesRel(cell)))
			preserved, err := readBackupImagePins(path)
			if err != nil || preserved == nil || *preserved != *backup {
				t.Fatal("server-only roll changed prior backup image pins")
			}
			backup.Tag = "0.1.1"
			backup.Digest = "sha256:" + strings.Repeat("d", 64)
			if err := RollCellWithBackupImage(root, cell, backup.Tag, serverDigest, backup, io.Discard); err != nil {
				t.Fatal(err)
			}
			updated, err := readBackupImagePins(path)
			if err != nil || updated == nil || *updated != *backup {
				t.Fatal("explicit backup roll failed to replace prior pins")
			}
			if err := Check(root, io.Discard); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestBackupImagePinsRoundTrip(t *testing.T) {
	for _, image := range []string{`"postgres:18-alpine3.23"`, `{repository: "123", tag: "18"}`, `{repository: "true", tag: "18"}`, `{repository: "null", tag: "18"}`} {
		t.Run(image, func(t *testing.T) {
			root := copyGenerationFixture(t)
			cell := "civo-sandbox-use1-serving"
			path := filepath.Join(root, filepath.FromSlash(valuesRel(cell)))
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			body = setFixtureYAMLField(t, body, "apps.civoPostgres.backup.image", "      image: "+image+"\n", "enabled")
			if err := os.WriteFile(path, body, 0o600); err != nil {
				t.Fatal(err)
			}
			want, err := readBackupImagePins(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := Write(root, io.Discard); err != nil {
				t.Fatal(err)
			}
			if err := Check(root, io.Discard); err != nil {
				t.Fatal(err)
			}
			if err := RollCell(root, cell, "0.0.999", "sha256:"+strings.Repeat("a", 64), io.Discard); err != nil {
				t.Fatal(err)
			}
			got, err := readBackupImagePins(path)
			if err != nil || got == nil || want == nil || *got != *want {
				t.Fatal("generation or server roll changed backup image selection")
			}
			if err := Check(root, io.Discard); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestGenerationRejectsMalformedBackupImage(t *testing.T) {
	for _, image := range []string{"null", `{repository: postgres, tag: "18", tag: "19"}`} {
		t.Run(image, func(t *testing.T) {
			root := copyGenerationFixture(t)
			cell := "civo-sandbox-use1-serving"
			path := filepath.Join(root, filepath.FromSlash(valuesRel(cell)))
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			body = setFixtureYAMLField(t, body, "apps.civoPostgres.backup.image", "      image: "+image+"\n", "enabled")
			if err := os.WriteFile(path, body, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := Write(root, io.Discard); err == nil {
				t.Fatal("generation accepted malformed backup image")
			}
			if err := Check(root, io.Discard); err == nil {
				t.Fatal("generation check accepted malformed backup image")
			}
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(body, got) {
				t.Fatal("generation modified malformed backup image")
			}
		})
	}
}

func TestRollBackupImageFailureLeavesAllCellsUntouched(t *testing.T) {
	for _, name := range []string{"missing digest", "bad digest", "bad repository", "wrong tag", "missing backup mapping", "malformed existing pin", "drift"} {
		t.Run(name, func(t *testing.T) {
			root := copyGenerationFixture(t)
			cell := "civo-sandbox-use1-serving"
			backup := &BackupImagePins{Repository: backupReleaseRepository, Tag: "0.0.999", Digest: "sha256:" + strings.Repeat("b", 64)}
			switch name {
			case "missing digest":
				backup.Digest = ""
			case "bad digest":
				backup.Digest = "sha256:invalid"
			case "bad repository":
				backup.Repository += "\ninjected: true"
			case "wrong tag":
				backup.Tag = "0.0.998"
			case "missing backup mapping":
				cfg, err := loadCatalog(root)
				if err != nil {
					t.Fatal(err)
				}
				for candidate := range cfg.Cells {
					if strings.HasPrefix(candidate, "aws-") {
						cell = candidate
						break
					}
				}
			case "malformed existing pin", "drift":
				path := filepath.Join(root, filepath.FromSlash(valuesRel(cell)))
				body, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if name == "drift" {
					body = append(body, []byte("# operator edit\n")...)
				} else {
					body = setFixtureYAMLField(t, body, "apps.civoPostgres.backup.image", "      image: {repository: postgres, tag: \"18\", digest: false}\n", "enabled")
				}
				if err := os.WriteFile(path, body, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before := readCellFixtureBytes(t, root)
			if err := RollCellWithBackupImage(root, cell, "0.0.999", "sha256:"+strings.Repeat("a", 64), backup, io.Discard); err == nil {
				t.Fatal("invalid backup roll accepted")
			}
			for candidate, body := range readCellFixtureBytes(t, root) {
				if !bytes.Equal(body, before[candidate]) {
					t.Fatalf("failed backup roll modified %s", candidate)
				}
			}
		})
	}
}
