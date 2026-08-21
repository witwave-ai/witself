package backupevidence

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

var testNow = time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)

type fixture struct {
	dir       string
	backupID  string
	cell      string
	release   string
	createdAt time.Time
	artifact  []byte
}

func sha256hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func makeFixture(t *testing.T, root, cell, release, suffix string) *fixture {
	t.Helper()
	createdAt := testNow.Add(-30 * time.Minute)
	f := &fixture{
		cell:      cell,
		release:   release,
		createdAt: createdAt,
		artifact:  []byte("witself-test-ciphertext-" + cell + "-" + suffix),
	}
	f.backupID = cell + "-pre-v" + release + "-" + createdAt.Format(stampLayout) + "-" + suffix
	f.dir = filepath.Join(root, f.backupID)
	if err := os.Mkdir(f.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	writePrivate(t, filepath.Join(f.dir, f.backupID+".dump.age"), f.artifact)
	writePrivate(t, filepath.Join(f.dir, f.backupID+".sha256"),
		[]byte(fmt.Sprintf("%s  %s\n", sha256hex(f.artifact), f.backupID+".dump.age")))
	f.writeManifest(t, f.manifestMap())
	return f
}

func writePrivate(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) manifestMap() map[string]any {
	return map[string]any{
		"schema":    ManifestSchema,
		"backup_id": f.backupID,
		"source": map[string]any{
			"cell":                         f.cell,
			"kubernetes_context":           "civo-admin@witself",
			"postgresql_version_num":       180003,
			"schema_version":               91,
			"pgvector_extension_installed": true,
		},
		"target_release": f.release,
		"created_at":     f.createdAt.Format(timeLayout),
		"artifact": map[string]any{
			"file":               f.backupID + ".dump.age",
			"bytes":              len(f.artifact),
			"encryption":         "age",
			"checksum_algorithm": "sha256",
			"ciphertext_sha256":  sha256hex(f.artifact),
			"checksum_file":      f.backupID + ".sha256",
		},
		"procedure": map[string]any{"script_sha256": strings.Repeat("ab", 32)},
		"restore_verification": map[string]any{
			"status":                            "verified",
			"verified_at":                       f.createdAt.Add(5 * time.Minute).Format(timeLayout),
			"network":                           "none",
			"plaintext_storage":                 "container tmpfs",
			"image_ref":                         "pgvector/pgvector:pg18",
			"image_id":                          "sha256:" + strings.Repeat("cd", 32),
			"schema_version":                    91,
			"public_table_count":                120,
			"account_count":                     3,
			"invalid_index_count":               0,
			"unvalidated_constraint_count":      0,
			"pgvector_extension_installed":      true,
			"pgvector_extension_matches_source": true,
			"disposable_target_cleaned":         true,
		},
	}
}

func (f *fixture) writeManifest(t *testing.T, doc map[string]any) {
	t.Helper()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	writePrivate(t, filepath.Join(f.dir, f.backupID+".json"), raw)
}

func (f *fixture) writeManifestRaw(t *testing.T, raw string) {
	t.Helper()
	writePrivate(t, filepath.Join(f.dir, f.backupID+".json"), []byte(raw))
}

func (f *fixture) mutate(t *testing.T, edit func(doc map[string]any)) {
	t.Helper()
	doc := f.manifestMap()
	edit(doc)
	f.writeManifest(t, doc)
}

func (f *fixture) makePending(t *testing.T) {
	t.Helper()
	f.mutate(t, func(doc map[string]any) {
		rv := doc["restore_verification"].(map[string]any)
		rv["status"] = "pending"
		rv["verified_at"] = nil
		rv["schema_version"] = 0
		rv["public_table_count"] = 0
		rv["account_count"] = 0
		rv["invalid_index_count"] = 0
		rv["unvalidated_constraint_count"] = 0
		rv["pgvector_extension_installed"] = false
		rv["pgvector_extension_matches_source"] = false
		rv["disposable_target_cleaned"] = false
	})
}

func verifyDirs(dirs []string, edit func(*Options)) (Report, []Finding) {
	opts := Options{
		Release: "0.0.258",
		Now:     func() time.Time { return testNow },
	}
	if edit != nil {
		edit(&opts)
	}
	return Verify(dirs, opts)
}

func makePair(t *testing.T) (*fixture, *fixture) {
	t.Helper()
	root := t.TempDir()
	a := makeFixture(t, root, "civo-sandbox-use1-backup", "0.0.258", "0a1b2c3d")
	b := makeFixture(t, root, "civo-sandbox-usw2-dev", "0.0.258", "4e5f6071")
	return a, b
}

func requireReason(t *testing.T, findings []Finding, reason Reason) {
	t.Helper()
	if len(findings) == 0 {
		t.Fatalf("expected findings with reason %s, got none", reason)
	}
	for _, f := range findings {
		if f.Reason == reason {
			return
		}
	}
	t.Fatalf("expected reason %s, got %v", reason, findings)
}

func TestVerifyPassBothCells(t *testing.T) {
	a, b := makePair(t)
	report, findings := verifyDirs([]string{a.dir, b.dir}, nil)
	if len(findings) != 0 {
		t.Fatalf("unexpected findings: %v", findings)
	}
	want := Report{
		Schema:            ReportSchema,
		Release:           "0.0.258",
		InputsChecked:     2,
		ManifestsVerified: 2,
		CellsRequired:     2,
		CellsSatisfied:    2,
		FailureCounts:     map[string]int{},
		Result:            "pass",
	}
	got, _ := json.Marshal(report)
	expected, _ := json.Marshal(want)
	if string(got) != string(expected) {
		t.Fatalf("report mismatch:\n got %s\nwant %s", got, expected)
	}
}

func TestVerifySingleRequiredCell(t *testing.T) {
	root := t.TempDir()
	f := makeFixture(t, root, "civo-sandbox-usw2-dev", "0.0.258", "0a1b2c3d")
	report, findings := verifyDirs([]string{f.dir}, func(o *Options) {
		o.RequiredCells = []string{"civo-sandbox-usw2-dev"}
	})
	if len(findings) != 0 || report.Result != "pass" || report.CellsRequired != 1 {
		t.Fatalf("expected single-cell pass, got %v %+v", findings, report)
	}
}

func TestMissingSecondCellFailsClosed(t *testing.T) {
	root := t.TempDir()
	f := makeFixture(t, root, "civo-sandbox-usw2-dev", "0.0.258", "0a1b2c3d")
	report, findings := verifyDirs([]string{f.dir}, nil)
	requireReason(t, findings, ReasonCellMissing)
	if report.Result != "fail" || report.ManifestsVerified != 1 || report.CellsSatisfied != 1 {
		t.Fatalf("unexpected report %+v", report)
	}
}

func TestPendingManifestBlocks(t *testing.T) {
	a, b := makePair(t)
	b.makePending(t)
	report, findings := verifyDirs([]string{a.dir, b.dir}, nil)
	requireReason(t, findings, ReasonManifestPending)
	requireReason(t, findings, ReasonCellMissing)
	if report.Result != "fail" || report.ManifestsVerified != 1 {
		t.Fatalf("unexpected report %+v", report)
	}
}

// TestMissingCountIsFieldErrorNotGateFailure pins the failure
// classification: an absent drill count is a manifest defect, and must not
// additionally be recorded as a failed restore drill in retained evidence.
func TestMissingCountIsFieldErrorNotGateFailure(t *testing.T) {
	a, b := makePair(t)
	b.mutate(t, func(doc map[string]any) {
		delete(doc["restore_verification"].(map[string]any), "invalid_index_count")
	})
	report, _ := verifyDirs([]string{a.dir, b.dir}, nil)
	want := map[string]int{
		string(ReasonManifestFieldInvalid): 1,
		string(ReasonCellMissing):          1,
	}
	got, _ := json.Marshal(report.FailureCounts)
	expected, _ := json.Marshal(want)
	if string(got) != string(expected) {
		t.Fatalf("failure counts %s, want %s", got, expected)
	}
}

func TestPendingWithVerifiedDataIsContradictory(t *testing.T) {
	a, b := makePair(t)
	b.makePending(t)
	b.mutate(t, func(doc map[string]any) {
		rv := doc["restore_verification"].(map[string]any)
		rv["status"] = "pending"
		rv["verified_at"] = nil
		rv["schema_version"] = 91
		rv["disposable_target_cleaned"] = true
	})
	_, findings := verifyDirs([]string{a.dir, b.dir}, nil)
	requireReason(t, findings, ReasonManifestFieldInvalid)
}

func TestManifestMutations(t *testing.T) {
	cases := []struct {
		name   string
		edit   func(doc map[string]any)
		reason Reason
	}{
		{"schema marker", func(d map[string]any) { d["schema"] = "witself.other.v1" }, ReasonManifestFieldInvalid},
		{"empty status bypass", func(d map[string]any) {
			d["restore_verification"].(map[string]any)["status"] = ""
		}, ReasonManifestFieldInvalid},
		{"empty target_release bypass", func(d map[string]any) { d["target_release"] = "" }, ReasonManifestFieldInvalid},
		{"empty created_at bypass", func(d map[string]any) { d["created_at"] = "" }, ReasonManifestFieldInvalid},
		{"empty backup_id bypass", func(d map[string]any) { d["backup_id"] = "" }, ReasonManifestFieldInvalid},
		{"empty script digest", func(d map[string]any) {
			d["procedure"].(map[string]any)["script_sha256"] = ""
		}, ReasonManifestFieldInvalid},
		{"empty image ref", func(d map[string]any) {
			d["restore_verification"].(map[string]any)["image_ref"] = ""
		}, ReasonManifestFieldInvalid},
		{"empty kubernetes context", func(d map[string]any) {
			d["source"].(map[string]any)["kubernetes_context"] = ""
		}, ReasonManifestFieldInvalid},
		{"release mismatch", func(d map[string]any) { d["target_release"] = "0.0.259" }, ReasonReleaseMismatch},
		{"release malformed", func(d map[string]any) { d["target_release"] = "v0.0.258" }, ReasonManifestFieldInvalid},
		{"cell unsupported", func(d map[string]any) {
			d["source"].(map[string]any)["cell"] = "civo-sandbox-use1-dev"
		}, ReasonCellUnsupported},
		{"backup id cell mismatch", func(d map[string]any) {
			d["backup_id"] = "civo-sandbox-use1-backup-pre-v0.0.258-20260820T113000Z-0a1b2c3d"
		}, ReasonManifestFieldInvalid},
		{"artifact file traversal", func(d map[string]any) {
			d["artifact"].(map[string]any)["file"] = "../evil.dump.age"
		}, ReasonManifestFieldInvalid},
		{"checksum file mismatch", func(d map[string]any) {
			d["artifact"].(map[string]any)["checksum_file"] = "other.sha256"
		}, ReasonManifestFieldInvalid},
		{"encryption", func(d map[string]any) {
			d["artifact"].(map[string]any)["encryption"] = "gpg"
		}, ReasonManifestFieldInvalid},
		{"checksum algorithm", func(d map[string]any) {
			d["artifact"].(map[string]any)["checksum_algorithm"] = "sha1"
		}, ReasonManifestFieldInvalid},
		{"uppercase digest", func(d map[string]any) {
			d["artifact"].(map[string]any)["ciphertext_sha256"] = strings.ToUpper(strings.Repeat("ab", 32))
		}, ReasonManifestFieldInvalid},
		{"script digest short", func(d map[string]any) {
			d["procedure"].(map[string]any)["script_sha256"] = "abcd"
		}, ReasonManifestFieldInvalid},
		{"context characters", func(d map[string]any) {
			d["source"].(map[string]any)["kubernetes_context"] = "bad context\n"
		}, ReasonManifestFieldInvalid},
		{"postgres version range", func(d map[string]any) {
			d["source"].(map[string]any)["postgresql_version_num"] = 999
		}, ReasonManifestFieldInvalid},
		{"schema version zero", func(d map[string]any) {
			d["source"].(map[string]any)["schema_version"] = 0
		}, ReasonManifestFieldInvalid},
		{"count negative", func(d map[string]any) {
			d["restore_verification"].(map[string]any)["account_count"] = -1
		}, ReasonManifestFieldInvalid},
		{"count float", func(d map[string]any) {
			d["restore_verification"].(map[string]any)["account_count"] = 1.5
		}, ReasonManifestFieldInvalid},
		{"count overflow", func(d map[string]any) {
			d["restore_verification"].(map[string]any)["account_count"] = json.Number("99999999999999999999")
		}, ReasonManifestFieldInvalid},
		{"count above cap", func(d map[string]any) {
			d["restore_verification"].(map[string]any)["account_count"] = json.Number("1000000000001")
		}, ReasonManifestFieldInvalid},
		{"artifact bytes zero", func(d map[string]any) {
			d["artifact"].(map[string]any)["bytes"] = 0
		}, ReasonManifestFieldInvalid},
		{"missing created_at", func(d map[string]any) { delete(d, "created_at") }, ReasonManifestFieldInvalid},
		{"missing restore object", func(d map[string]any) { delete(d, "restore_verification") }, ReasonManifestFieldInvalid},
		{"status unknown", func(d map[string]any) {
			d["restore_verification"].(map[string]any)["status"] = "maybe"
		}, ReasonManifestFieldInvalid},
		{"network host", func(d map[string]any) {
			d["restore_verification"].(map[string]any)["network"] = "host"
		}, ReasonManifestFieldInvalid},
		{"plaintext storage", func(d map[string]any) {
			d["restore_verification"].(map[string]any)["plaintext_storage"] = "host disk"
		}, ReasonManifestFieldInvalid},
		{"image id prefix", func(d map[string]any) {
			d["restore_verification"].(map[string]any)["image_id"] = strings.Repeat("cd", 32)
		}, ReasonManifestFieldInvalid},
		{"image ref characters", func(d map[string]any) {
			d["restore_verification"].(map[string]any)["image_ref"] = "bad image ref"
		}, ReasonManifestFieldInvalid},
		{"gate schema version", func(d map[string]any) {
			d["restore_verification"].(map[string]any)["schema_version"] = 90
		}, ReasonGateFailed},
		{"gate tables zero", func(d map[string]any) {
			d["restore_verification"].(map[string]any)["public_table_count"] = 0
		}, ReasonGateFailed},
		{"gate invalid indexes", func(d map[string]any) {
			d["restore_verification"].(map[string]any)["invalid_index_count"] = 1
		}, ReasonGateFailed},
		{"gate unvalidated constraints", func(d map[string]any) {
			d["restore_verification"].(map[string]any)["unvalidated_constraint_count"] = 2
		}, ReasonGateFailed},
		{"gate pgvector mismatch", func(d map[string]any) {
			d["restore_verification"].(map[string]any)["pgvector_extension_installed"] = false
		}, ReasonGateFailed},
		{"gate matches source false", func(d map[string]any) {
			d["restore_verification"].(map[string]any)["pgvector_extension_matches_source"] = false
		}, ReasonGateFailed},
		{"gate cleanup false", func(d map[string]any) {
			d["restore_verification"].(map[string]any)["disposable_target_cleaned"] = false
		}, ReasonGateFailed},
		{"verified_at missing", func(d map[string]any) {
			d["restore_verification"].(map[string]any)["verified_at"] = nil
		}, ReasonManifestFieldInvalid},
		{"verified_at before created", func(d map[string]any) {
			d["restore_verification"].(map[string]any)["verified_at"] = testNow.Add(-2 * time.Hour).Format(timeLayout)
		}, ReasonTimestampInvalid},
		{"verified_at future", func(d map[string]any) {
			d["restore_verification"].(map[string]any)["verified_at"] = testNow.Add(2 * time.Hour).Format(timeLayout)
		}, ReasonTimestampInvalid},
		{"created_at future", func(d map[string]any) {
			d["created_at"] = testNow.Add(2 * time.Hour).Format(timeLayout)
		}, ReasonTimestampInvalid},
		{"created_at offset layout", func(d map[string]any) {
			d["created_at"] = "2026-08-20T11:30:00+00:00"
		}, ReasonTimestampInvalid},
		{"created_at far from id stamp", func(d map[string]any) {
			d["created_at"] = testNow.Add(-30 * time.Minute).Add(-3 * time.Hour).Format(timeLayout)
		}, ReasonTimestampInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, b := makePair(t)
			b.mutate(t, tc.edit)
			_, findings := verifyDirs([]string{a.dir, b.dir}, nil)
			requireReason(t, findings, tc.reason)
		})
	}
}

func TestManifestRawSyntax(t *testing.T) {
	cases := []struct {
		name      string
		transform func(valid string) string
	}{
		{"unknown field", func(v string) string {
			return strings.Replace(v, `{"artifact"`, `{"extra_field":1,"artifact"`, 1)
		}},
		{"duplicate key", func(v string) string {
			return strings.Replace(v, `{"artifact"`, `{"schema":"x","artifact"`, 1)
		}},
		{"nested duplicate key", func(v string) string {
			return strings.Replace(v, `"source":{"cell"`, `"source":{"cell":"x","cell"`, 1)
		}},
		{"document too deep", func(string) string {
			return strings.Repeat(`{"a":`, 12) + `1` + strings.Repeat(`}`, 12)
		}},
		{"trailing data", func(v string) string { return v + "{}" }},
		{"array document", func(v string) string { return "[" + v + "]" }},
		{"empty file", func(string) string { return "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, b := makePair(t)
			raw, err := json.Marshal(b.manifestMap())
			if err != nil {
				t.Fatal(err)
			}
			mutated := tc.transform(string(raw))
			if mutated == string(raw) {
				t.Fatal("transform did not change the manifest")
			}
			b.writeManifestRaw(t, mutated)
			_, findings := verifyDirs([]string{a.dir, b.dir}, nil)
			if tc.name == "empty file" {
				requireReason(t, findings, ReasonManifestUnreadable)
				return
			}
			requireReason(t, findings, ReasonManifestSyntax)
		})
	}
}

func TestManifestTooLarge(t *testing.T) {
	a, b := makePair(t)
	doc := b.manifestMap()
	doc["schema"] = ManifestSchema
	raw, _ := json.Marshal(doc)
	padded := string(raw[:len(raw)-1]) + `,"pad":"` + strings.Repeat("x", maxManifestBytes) + `"}`
	b.writeManifestRaw(t, padded)
	_, findings := verifyDirs([]string{a.dir, b.dir}, nil)
	requireReason(t, findings, ReasonManifestUnreadable)
}

func TestStaleEvidence(t *testing.T) {
	a, b := makePair(t)
	_, findings := verifyDirs([]string{a.dir, b.dir}, func(o *Options) {
		o.MaxAge = 10 * time.Minute
	})
	requireReason(t, findings, ReasonTimestampStale)
	report, findings := verifyDirs([]string{a.dir, b.dir}, func(o *Options) {
		o.MaxAge = 2 * time.Hour
	})
	if len(findings) != 0 || report.Result != "pass" {
		t.Fatalf("expected pass within max age, got %v", findings)
	}
}

func TestArtifactTampering(t *testing.T) {
	t.Run("bit flip", func(t *testing.T) {
		a, b := makePair(t)
		tampered := append([]byte{}, b.artifact...)
		tampered[0] ^= 0xff
		writePrivate(t, filepath.Join(b.dir, b.backupID+".dump.age"), tampered)
		_, findings := verifyDirs([]string{a.dir, b.dir}, nil)
		requireReason(t, findings, ReasonArtifactMismatch)
	})
	t.Run("size change", func(t *testing.T) {
		a, b := makePair(t)
		writePrivate(t, filepath.Join(b.dir, b.backupID+".dump.age"), append(b.artifact, 'x'))
		_, findings := verifyDirs([]string{a.dir, b.dir}, nil)
		requireReason(t, findings, ReasonArtifactMismatch)
	})
	t.Run("empty artifact", func(t *testing.T) {
		a, b := makePair(t)
		writePrivate(t, filepath.Join(b.dir, b.backupID+".dump.age"), nil)
		_, findings := verifyDirs([]string{a.dir, b.dir}, nil)
		requireReason(t, findings, ReasonArtifactMismatch)
	})
}

func TestSidecarRejections(t *testing.T) {
	cases := []struct {
		name    string
		content func(f *fixture) string
	}{
		{"wrong hash", func(f *fixture) string {
			return strings.Repeat("00", 32) + "  " + f.backupID + ".dump.age\n"
		}},
		{"wrong filename", func(f *fixture) string {
			return sha256hex(f.artifact) + "  other.dump.age\n"
		}},
		{"single space", func(f *fixture) string {
			return sha256hex(f.artifact) + " " + f.backupID + ".dump.age\n"
		}},
		{"missing newline", func(f *fixture) string {
			return sha256hex(f.artifact) + "  " + f.backupID + ".dump.age"
		}},
		{"two lines", func(f *fixture) string {
			line := sha256hex(f.artifact) + "  " + f.backupID + ".dump.age\n"
			return line + line
		}},
		{"oversized", func(f *fixture) string {
			return sha256hex(f.artifact) + "  " + f.backupID + ".dump.age" +
				strings.Repeat(" ", maxSidecarBytes) + "\n"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, b := makePair(t)
			writePrivate(t, filepath.Join(b.dir, b.backupID+".sha256"), []byte(tc.content(b)))
			_, findings := verifyDirs([]string{a.dir, b.dir}, nil)
			requireReason(t, findings, ReasonSidecarInvalid)
		})
	}
}

func TestDirectoryLayout(t *testing.T) {
	t.Run("extra file", func(t *testing.T) {
		a, b := makePair(t)
		writePrivate(t, filepath.Join(b.dir, "notes.txt"), []byte("x"))
		_, findings := verifyDirs([]string{a.dir, b.dir}, nil)
		requireReason(t, findings, ReasonDirLayoutInvalid)
	})
	t.Run("missing sidecar", func(t *testing.T) {
		a, b := makePair(t)
		if err := os.Remove(filepath.Join(b.dir, b.backupID+".sha256")); err != nil {
			t.Fatal(err)
		}
		_, findings := verifyDirs([]string{a.dir, b.dir}, nil)
		requireReason(t, findings, ReasonDirLayoutInvalid)
	})
	t.Run("symlinked manifest", func(t *testing.T) {
		a, b := makePair(t)
		manifest := filepath.Join(b.dir, b.backupID+".json")
		target := filepath.Join(t.TempDir(), "target.json")
		raw, _ := json.Marshal(b.manifestMap())
		writePrivate(t, target, raw)
		if err := os.Remove(manifest); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, manifest); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		_, findings := verifyDirs([]string{a.dir, b.dir}, nil)
		requireReason(t, findings, ReasonDirLayoutInvalid)
	})
	t.Run("renamed directory", func(t *testing.T) {
		a, b := makePair(t)
		renamed := filepath.Join(filepath.Dir(b.dir), "renamed-evidence")
		if err := os.Rename(b.dir, renamed); err != nil {
			t.Fatal(err)
		}
		_, findings := verifyDirs([]string{a.dir, renamed}, nil)
		requireReason(t, findings, ReasonDirLayoutInvalid)
	})
	t.Run("symlinked input directory", func(t *testing.T) {
		a, b := makePair(t)
		link := filepath.Join(t.TempDir(), filepath.Base(b.dir))
		if err := os.Symlink(b.dir, link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		_, findings := verifyDirs([]string{a.dir, link}, nil)
		requireReason(t, findings, ReasonInputPathInvalid)
	})
	t.Run("input is a file", func(t *testing.T) {
		a, _ := makePair(t)
		file := filepath.Join(t.TempDir(), "plain")
		writePrivate(t, file, []byte("x"))
		_, findings := verifyDirs([]string{a.dir, file}, nil)
		requireReason(t, findings, ReasonInputPathInvalid)
	})
	t.Run("missing input", func(t *testing.T) {
		a, _ := makePair(t)
		_, findings := verifyDirs([]string{a.dir, filepath.Join(t.TempDir(), "absent")}, nil)
		requireReason(t, findings, ReasonInputPathInvalid)
	})
}

func TestInsecurePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not enforced on windows")
	}
	t.Run("manifest group readable", func(t *testing.T) {
		a, b := makePair(t)
		if err := os.Chmod(filepath.Join(b.dir, b.backupID+".json"), 0o640); err != nil {
			t.Fatal(err)
		}
		_, findings := verifyDirs([]string{a.dir, b.dir}, nil)
		requireReason(t, findings, ReasonPermissionsInsecure)
	})
	t.Run("artifact world readable", func(t *testing.T) {
		a, b := makePair(t)
		if err := os.Chmod(filepath.Join(b.dir, b.backupID+".dump.age"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, findings := verifyDirs([]string{a.dir, b.dir}, nil)
		requireReason(t, findings, ReasonPermissionsInsecure)
	})
	t.Run("directory group accessible", func(t *testing.T) {
		a, b := makePair(t)
		if err := os.Chmod(b.dir, 0o750); err != nil {
			t.Fatal(err)
		}
		_, findings := verifyDirs([]string{a.dir, b.dir}, nil)
		requireReason(t, findings, ReasonPermissionsInsecure)
	})
}

func TestDuplicateEvidence(t *testing.T) {
	t.Run("same directory twice", func(t *testing.T) {
		root := t.TempDir()
		f := makeFixture(t, root, "civo-sandbox-usw2-dev", "0.0.258", "0a1b2c3d")
		_, findings := verifyDirs([]string{f.dir, f.dir}, func(o *Options) {
			o.RequiredCells = []string{"civo-sandbox-usw2-dev"}
		})
		requireReason(t, findings, ReasonCellDuplicate)
	})
	t.Run("same cell two artifacts", func(t *testing.T) {
		root := t.TempDir()
		f1 := makeFixture(t, root, "civo-sandbox-usw2-dev", "0.0.258", "0a1b2c3d")
		f2 := makeFixture(t, root, "civo-sandbox-usw2-dev", "0.0.258", "4e5f6071")
		_, findings := verifyDirs([]string{f1.dir, f2.dir}, func(o *Options) {
			o.RequiredCells = []string{"civo-sandbox-usw2-dev"}
		})
		requireReason(t, findings, ReasonCellDuplicate)
	})
}

func TestOptionValidation(t *testing.T) {
	t.Run("bad release", func(t *testing.T) {
		report, findings := verifyDirs([]string{t.TempDir()}, func(o *Options) {
			o.Release = "v0.0.258"
		})
		requireReason(t, findings, ReasonReleaseMismatch)
		if report.Result != "fail" {
			t.Fatal("expected fail result")
		}
	})
	t.Run("unsupported required cell", func(t *testing.T) {
		_, findings := verifyDirs([]string{t.TempDir()}, func(o *Options) {
			o.RequiredCells = []string{"aws-sandbox-use1-dev"}
		})
		requireReason(t, findings, ReasonCellUnsupported)
	})
	t.Run("duplicate required cell", func(t *testing.T) {
		_, findings := verifyDirs([]string{t.TempDir()}, func(o *Options) {
			o.RequiredCells = []string{"civo-sandbox-usw2-dev", "civo-sandbox-usw2-dev"}
		})
		requireReason(t, findings, ReasonCellDuplicate)
	})
	t.Run("no inputs", func(t *testing.T) {
		_, findings := verifyDirs(nil, nil)
		requireReason(t, findings, ReasonInputPathInvalid)
	})
}

func TestFindingsNeverLeakValues(t *testing.T) {
	a, b := makePair(t)
	b.mutate(t, func(doc map[string]any) {
		doc["target_release"] = "0.0.999"
		doc["source"].(map[string]any)["kubernetes_context"] = "leaky context value\n"
		rv := doc["restore_verification"].(map[string]any)
		rv["disposable_target_cleaned"] = false
	})
	report, findings := verifyDirs([]string{a.dir, b.dir}, nil)
	forbidden := []string{a.dir, b.dir, a.backupID, b.backupID, "leaky", "0.0.999", os.TempDir()}
	for _, f := range findings {
		text := f.String()
		for _, needle := range forbidden {
			if needle != "" && strings.Contains(text, needle) {
				t.Fatalf("finding leaked %q: %s", needle, text)
			}
		}
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range forbidden {
		if needle != "" && strings.Contains(string(raw), needle) {
			t.Fatalf("report leaked %q: %s", needle, raw)
		}
	}
}

func TestVerifyDeterministic(t *testing.T) {
	a, b := makePair(t)
	b.mutate(t, func(doc map[string]any) {
		doc["target_release"] = "0.0.999"
		doc["restore_verification"].(map[string]any)["invalid_index_count"] = 3
	})
	report1, findings1 := verifyDirs([]string{a.dir, b.dir}, nil)
	report2, findings2 := verifyDirs([]string{a.dir, b.dir}, nil)
	raw1, _ := json.Marshal(struct {
		R Report
		F []Finding
	}{report1, findings1})
	raw2, _ := json.Marshal(struct {
		R Report
		F []Finding
	}{report2, findings2})
	if string(raw1) != string(raw2) {
		t.Fatalf("verification output is not deterministic:\n%s\n%s", raw1, raw2)
	}
}

func TestWriteEvidence(t *testing.T) {
	a, b := makePair(t)
	report, findings := verifyDirs([]string{a.dir, b.dir}, nil)
	if len(findings) != 0 {
		t.Fatalf("unexpected findings: %v", findings)
	}
	path := filepath.Join(t.TempDir(), "evidence.json")
	if err := WriteEvidence(report, path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("evidence file mode %v, want 0600", info.Mode().Perm())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Report
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("evidence is not valid JSON: %v", err)
	}
	if decoded.Schema != ReportSchema || decoded.Result != "pass" {
		t.Fatalf("unexpected evidence content %+v", decoded)
	}
	err = WriteEvidence(report, path)
	if err == nil || !errors.Is(err, os.ErrExist) {
		t.Fatalf("expected create-only failure, got %v", err)
	}
	if err := WriteEvidence(report, "  "); err == nil {
		t.Fatal("expected empty evidence path to be rejected")
	}
}
