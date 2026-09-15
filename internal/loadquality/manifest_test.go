package loadquality

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidateManifestAcceptsFivePassingSlices(t *testing.T) {
	dir, m := manifestTestFixture(t)
	if err := ValidateManifest(m, dir); err != nil {
		t.Fatal(err)
	}
	if m.Outcome != "pass" || len(m.Slices) != 5 {
		t.Fatalf("incomplete passing manifest: %#v", m)
	}
}

func TestValidateManifestRejectsUnsafeOrInconsistentEvidence(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*Manifest, string)
	}{
		{"foreign run", func(m *Manifest, _ string) { m.RunURL = "https://example.com/actions/runs/1" }},
		{"run whitespace", func(m *Manifest, _ string) { m.RunURL += " " }},
		{"run too long", func(m *Manifest, _ string) { m.RunURL += strings.Repeat("1", 257) }},
		{"release", func(m *Manifest, _ string) { m.Release = "claude/unsafe" }},
		{"commit", func(m *Manifest, _ string) { m.Commit = "not-hex" }},
		{"runner name", func(m *Manifest, _ string) { m.Runner.Name = "runner.example" }},
		{"runner os", func(m *Manifest, _ string) { m.Runner.OS = "Linux unknown" }},
		{"runner arch", func(m *Manifest, _ string) { m.Runner.Arch = "x64.example" }},
		{"runner environment", func(m *Manifest, _ string) { m.Runner.Environment = "github.hosted" }},
		{"postgres image", func(m *Manifest, _ string) { m.Postgres.ImageLabel = "pg16.example" }},
		{"unknown schema", func(m *Manifest, _ string) { m.Slices[0].Schema = "unknown" }},
		{"hash mismatch", func(m *Manifest, _ string) { m.Slices[0].SHA256 = strings.Repeat("0", 64) }},
		{"failed slice", func(m *Manifest, _ string) { m.Slices[0].Outcome = "fail" }},
		{"duplicate slice", func(m *Manifest, _ string) { m.Slices[1] = m.Slices[0] }},
		{"path traversal", func(m *Manifest, _ string) { m.Slices[0].Path = "../memory-lexical.json" }},
		{"missing slices", func(m *Manifest, _ string) { m.Slices = nil }},
		{"redaction marker", func(_ *Manifest, dir string) {
			if err := os.WriteFile(filepath.Join(dir, "test-lexical.log.redacted"), []byte(evidenceRedaction), 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{"provider", func(_ *Manifest, dir string) {
			manifestMutateJSON(t, dir, "lexical", func(v map[string]any) { v["environment"].(map[string]any)["provider"] = "github.hosted" })
		}},
		{"release mismatch", func(_ *Manifest, dir string) {
			manifestMutateJSON(t, dir, "curation", func(v map[string]any) { v["environment"].(map[string]any)["release"] = "v9.9.9" })
		}},
		{"unknown JSON property", func(_ *Manifest, dir string) {
			manifestMutateJSON(t, dir, "lexical", func(v map[string]any) { v["private_context"] = "untrusted" })
		}},
		{"failed quality", func(_ *Manifest, dir string) {
			manifestMutateJSON(t, dir, "lexical", func(v map[string]any) { v["quality"].(map[string]any)["cross_tenant_isolated"] = false })
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir, m := manifestTestFixture(t)
			test.mutate(&m, dir)
			switch test.name {
			case "provider", "release mismatch", "unknown JSON property", "failed quality":
				for i := range m.Slices {
					raw, err := os.ReadFile(filepath.Join(dir, m.Slices[i].Path))
					if err != nil {
						t.Fatal(err)
					}
					digest := sha256.Sum256(raw)
					m.Slices[i].SHA256 = hex.EncodeToString(digest[:])
				}
			}
			if err := ValidateManifest(m, dir); err == nil {
				t.Fatal("unsafe evidence accepted")
			}
		})
	}
}

func TestBuildManifestPreservesFailureAndMissingSlices(t *testing.T) {
	for _, status := range []string{"failure", "cancelled", "skipped", "", "success"} {
		t.Run("status_"+status, func(t *testing.T) {
			dir, m := manifestTestFixture(t)
			m.Slices = []ManifestSlice{{Name: "lexical"}, {Name: "curation"}}
			m, err := BuildManifest(dir, m, map[string]string{"lexical": status, "curation": "success"})
			if err != nil {
				t.Fatal(err)
			}
			want := "fail"
			if status == "success" {
				want = "pass"
			}
			if m.Outcome != want {
				t.Fatalf("outcome=%s want %s", m.Outcome, want)
			}
			if err := ValidateManifest(m, dir); err != nil {
				t.Fatal(err)
			}
		})
	}
	t.Run("missing", func(t *testing.T) {
		dir, m := manifestTestFixture(t)
		if err := os.Remove(filepath.Join(dir, "memory-archive.json")); err != nil {
			t.Fatal(err)
		}
		m, err := BuildManifest(dir, m, manifestSuccesses())
		if err != nil {
			t.Fatal(err)
		}
		if m.Outcome != "fail" || m.Slices[3].Outcome != "fail" || m.Slices[3].SHA256 != "" {
			t.Fatalf("missing evidence: %#v", m)
		}
		if err := ValidateManifest(m, dir); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("startup failure", func(t *testing.T) {
		dir, m := manifestTestFixture(t)
		m.Postgres.ServerVersionNum = 0
		m, err := BuildManifest(dir, m, map[string]string{})
		if err != nil {
			t.Fatal(err)
		}
		if m.Outcome != "fail" {
			t.Fatal("startup failure upgraded to passing")
		}
		if err := ValidateManifest(m, dir); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("scan failure", func(t *testing.T) {
		dir, m := manifestTestFixture(t)
		m.Outcome = "fail"
		m, err := BuildManifest(dir, m, manifestSuccesses())
		if err != nil {
			t.Fatal(err)
		}
		if m.Outcome != "fail" {
			t.Fatal("scan failure upgraded to passing")
		}
	})
	t.Run("typed invalid result", func(t *testing.T) {
		dir, m := manifestTestFixture(t)
		manifestMutateJSON(t, dir, "recall", func(v map[string]any) { v["outcome"] = "fail" })
		m, err := BuildManifest(dir, m, manifestSuccesses())
		if err != nil {
			t.Fatal(err)
		}
		if m.Outcome != "fail" || m.Slices[2].Outcome != "fail" {
			t.Fatal("invalid result upgraded to passing")
		}
	})
}

func TestWriteManifestIsPrivateAndAtomicallyReplaced(t *testing.T) {
	dir, m := manifestTestFixture(t)
	path := filepath.Join(dir, "workflow-manifest.json")
	if err := os.WriteFile(path, []byte("previous"), 0644); err != nil {
		t.Fatal(err)
	}
	raw, err := WriteManifest(path, m, dir)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode=%o", info.Mode().Perm())
	}
	m.RunAttempt = 2
	next, err := WriteManifest(path, m, dir)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) == string(next) {
		t.Fatal("manifest was not replaced")
	}
	m.Commit = "invalid"
	if _, err := WriteManifest(path, m, dir); err == nil {
		t.Fatal("invalid manifest accepted")
	}
	kept, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(kept) != string(next) {
		t.Fatal("failed write replaced previous document")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tmp") {
			t.Fatal("temporary artifact left behind")
		}
	}
}

func TestScanEvidenceRedactsEveryDSNComponentWithoutLeakingErrors(t *testing.T) {
	const dsn = "postgres://report_user:report_password@private.example:5432/report_database?sslmode=disable"
	for _, forbidden := range []string{dsn, "postgres://private.example:5432", "report_user", "report_password", "report_database", "postgres://", "private.example:5432"} {
		t.Run(strings.ReplaceAll(forbidden, "/", "_"), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "test-lexical.log")
			if err := os.WriteFile(path, []byte("database error "+forbidden), 0600); err != nil {
				t.Fatal(err)
			}
			err := ScanEvidence(dir, dsn)
			if !errors.Is(err, ErrEvidenceRedacted) {
				t.Fatalf("error=%v", err)
			}
			if strings.Contains(err.Error(), "report_password") || strings.Contains(err.Error(), dsn) {
				t.Fatal("scanner error leaked credentials")
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("raw sensitive log retained")
			}
			raw, err := os.ReadFile(path + ".redacted")
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), forbidden) {
				t.Fatal("redacted file retained sensitive bytes")
			}
			if err := ScanEvidence(dir, dsn); err != nil {
				t.Fatalf("safe second scan: %v", err)
			}
		})
	}
}

func TestScanEvidenceAcceptsHostedResultsAndCleanLogs(t *testing.T) {
	for _, extra := range []string{"", " credential=witself"} {
		t.Run("extra_"+extra, func(t *testing.T) {
			original, m := manifestTestFixture(t)
			dir := filepath.Join(t.TempDir(), "witself", "witself", "evidence")
			if err := os.MkdirAll(filepath.Dir(dir), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(original, dir); err != nil {
				t.Fatal(err)
			}
			if _, err := WriteManifest(filepath.Join(dir, "workflow-manifest.json"), m, dir); err != nil {
				t.Fatal(err)
			}
			var log strings.Builder
			log.WriteString("=== RUN   TestNarrativeMemoryLoadQualityPostgres\n")
			for _, slice := range m.Slices {
				path := filepath.Join(dir, slice.Path)
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				log.WriteString("sanitized result written to " + path + extra + "\n")
				log.Write(raw)
				log.WriteString(extra + "\n")
			}
			log.WriteString("--- PASS: TestNarrativeMemoryLoadQualityPostgres (0.01s)\nPASS\nok  github.com/witwave-ai/witself/internal/store 0.01s\n")
			if err := os.WriteFile(filepath.Join(dir, "test-lexical.log"), []byte(log.String()), 0600); err != nil {
				t.Fatal(err)
			}
			err := ScanEvidence(dir, "postgres://witself:witself@localhost:5432/witself?sslmode=disable")
			if extra == "" && err != nil {
				t.Fatal(err)
			}
			if extra != "" && !errors.Is(err, ErrEvidenceRedacted) {
				t.Fatalf("adjacent credential was exempted: %v", err)
			}
		})
	}
}

func TestScanEvidenceRemovesUnsafeFilesAndRejectsInvalidDSN(t *testing.T) {
	const dsn = "postgres://report_user:report_password@private.example:5432/report_database?sslmode=disable"
	for _, kind := range []string{"unknown JSON", "known JSON extra field", "known JSON invalid quality", "symlink", "nested directory", "filename secret", "escaped JSON secret", "duplicate JSON key", "nested duplicate JSON key", "forged redacted"} {
		t.Run(kind, func(t *testing.T) {
			dir, m := manifestTestFixture(t)
			_ = m
			switch kind {
			case "unknown JSON":
				if err := os.WriteFile(filepath.Join(dir, "secret.json"), []byte(`{"unknown":"private"}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "known JSON extra field":
				manifestMutateJSON(t, dir, "archive", func(v map[string]any) { v["unexpected"] = "private" })
			case "known JSON invalid quality":
				manifestMutateJSON(t, dir, "lexical", func(v map[string]any) { v["quality"].(map[string]any)["cross_tenant_isolated"] = false })
			case "symlink":
				outside := filepath.Join(t.TempDir(), "outside.log")
				if err := os.WriteFile(outside, []byte("report_password"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(dir, "test-lexical.log")); err != nil {
					t.Fatal(err)
				}
			case "nested directory":
				if err := os.MkdirAll(filepath.Join(dir, "nested"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "nested", "secret.log"), []byte("private"), 0600); err != nil {
					t.Fatal(err)
				}
			case "filename secret":
				if err := os.WriteFile(filepath.Join(dir, "report_password.log"), []byte("private"), 0600); err != nil {
					t.Fatal(err)
				}
			case "escaped JSON secret":
				manifestMutateJSON(t, dir, "lexical", func(v map[string]any) { v["postgresql_version"] = "report_password" })
				p := filepath.Join(dir, "memory-lexical.json")
				raw, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				raw = []byte(strings.ReplaceAll(string(raw), "report_password", `report_\u0070assword`))
				if err := os.WriteFile(p, raw, 0600); err != nil {
					t.Fatal(err)
				}
			case "duplicate JSON key", "nested duplicate JSON key":
				p := filepath.Join(dir, "memory-lexical.json")
				raw, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				if kind == "duplicate JSON key" {
					raw = []byte(strings.Replace(string(raw), `"postgresql_version":`, `"postgresql_version":"report_password","postgresql_version":`, 1))
				} else {
					raw = []byte(strings.Replace(string(raw), `"release":`, `"release":"report_password","release":`, 1))
				}
				if err := os.WriteFile(p, raw, 0600); err != nil {
					t.Fatal(err)
				}
			case "forged redacted":
				if err := os.WriteFile(filepath.Join(dir, "test-lexical.log.redacted"), []byte("report_password"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := ScanEvidence(dir, dsn); !errors.Is(err, ErrEvidenceRedacted) {
				t.Fatalf("unsafe input not redacted: %v", err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.Contains(entry.Name(), "report_password") || entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
					t.Fatal("unsafe path retained")
				}
				raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(raw), "report_password") {
					t.Fatal("secret bytes retained")
				}
			}
			if err := ScanEvidence(dir, dsn); err != nil {
				t.Fatalf("safe second scan: %v", err)
			}
		})
	}
	dir := t.TempDir()
	if err := ScanEvidence(dir, "postgres://%report_password"); err == nil || strings.Contains(err.Error(), "report_password") {
		t.Fatalf("unsafe invalid-DSN error=%v", err)
	}
}

func TestScanEvidencePreservesHostedGoFailureStacks(t *testing.T) {
	const dsn = "postgres://witself:witself@localhost:5432/witself?sslmode=disable"
	// Real Go 1.26.6 runtime stacks captured with a temporary test calling
	// ParseOptions through a callback that panics or sleeps past the test
	// timeout. Only checkout/toolchain paths are normalized to hosted paths;
	// the package-result footer is outside these retained stack excerpts.
	for _, name := range []string{"timeout", "panic"} {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", "hosted-go-"+name+".log"))
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			path := filepath.Join(dir, "test-lexical.log")
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if err := ScanEvidence(dir, dsn); err != nil {
				t.Fatal("public failure stack was redacted", err)
			}
			if kept, err := os.ReadFile(path); err != nil || string(kept) != string(raw) {
				t.Fatalf("failure stack changed: %v", err)
			}
		})
	}
}

func TestScanEvidencePreservesHostedGoReceiverAndCreatedFrames(t *testing.T) {
	const source = "\t/home/runner/work/witself/witself/internal/store/memory.go:347"
	for name, log := range map[string]string{
		"receiver": "github.com/witwave-ai/witself/internal/store.(*Store).CaptureMemory(0xc000123400, {0xc000567800, 0x1})\n" + source + " +0x123\n",
		"inlined":  "github.com/witwave-ai/witself/internal/store.(*Store).CaptureMemory(...)\n" + source + "\n",
		"created":  "created by github.com/witwave-ai/witself/internal/store.runMemoryLoadQualityRecalls.func1 in goroutine 18\n\t/home/runner/work/witself/witself/internal/store/memory_load_quality_integration_test.go:290 +0x123\n",
		"crlf":     "github.com/witwave-ai/witself/internal/store.(*Store).CaptureMemory(...)\r\n" + source + " +0x123\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "test-lexical.log")
			if err := os.WriteFile(path, []byte(log), 0600); err != nil {
				t.Fatal(err)
			}
			if err := ScanEvidence(dir, "postgres://witself:witself@localhost:5432/witself?sslmode=disable"); err != nil {
				t.Fatal("public stack frame was redacted", err)
			}
			if kept, err := os.ReadFile(path); err != nil || string(kept) != log {
				t.Fatalf("public stack frame changed: %v", err)
			}
		})
	}
}

func TestScanEvidenceHostedGoFramesStillRejectCredentials(t *testing.T) {
	const frame = "github.com/witwave-ai/witself/internal/loadquality.WriteResult(...)\n\t/home/runner/work/witself/witself/internal/loadquality/loadquality.go:423 +0x123\n"
	for name, log := range map[string]string{
		"source adjacent credential": strings.Replace(frame, "+0x123", "+0x123 password=witself", 1),
		"frame argument credential":  strings.Replace(frame, "(...)", "(witself)", 1),
		"frame adjacent credential":  strings.Replace(frame, "(...)", "(...) password=witself", 1),
		"following credential":       frame + "password=witself\n",
		"source credential filename": strings.Replace(frame, "loadquality.go", "witself.go", 1),
		"source credential suffix":   strings.Replace(frame, "+0x123", "+0x123witself", 1),
		"frame credential suffix":    strings.Replace(frame, ".WriteResult", ".WriteResultwitself", 1),
		"lookalike checkout":         strings.Replace(frame, "/work/witself/witself/", "/work/witself/witself-other/", 1),
		"unvalidated checkout":       strings.Replace(frame, "/home/runner/work/", "/tmp/work/", 1),
		"path traversal":             strings.Replace(frame, "/internal/loadquality/loadquality.go", "/internal/store/../loadquality/loadquality.go", 1),
		"package mismatch":           strings.Replace(frame, "witself/internal/loadquality.WriteResult", "witself/internal/loadquality-other.WriteResult", 1),
		"unpaired source":            "\t/home/runner/work/witself/witself/internal/loadquality/loadquality.go:423 +0x123\n",
		"unpaired function":          "github.com/witwave-ai/witself/internal/loadquality.WriteResult(...)\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "test-lexical.log")
			if err := os.WriteFile(path, []byte(log), 0600); err != nil {
				t.Fatal(err)
			}
			if err := ScanEvidence(dir, "postgres://witself:witself@localhost:5432/witself?sslmode=disable"); !errors.Is(err, ErrEvidenceRedacted) {
				t.Fatalf("unsafe frame accepted: %v", err)
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("unsafe log retained")
			}
			if notice, err := os.ReadFile(path + ".redacted"); err != nil || string(notice) != evidenceRedaction {
				t.Fatalf("unsafe redaction notice: %v", err)
			}
		})
	}
}

func manifestTestFixture(t *testing.T) (string, Manifest) {
	t.Helper()
	dir := t.TempDir()
	started := time.Date(2026, 9, 5, 1, 0, 0, 0, time.UTC)
	environment := SafeMetadata{Release: "v1.2.3", Commit: "abcdef1", Provider: "github-hosted", HardwareTier: "ubuntu-latest-pg16", GoVersion: "go1.26.6", GOOS: "linux", GOARCH: "amd64", LogicalCPUs: 4}
	lexical := validTestResult(started)
	lexical.Environment = environment
	lexical.PostgreSQLVersion = "16.14 (Debian 16.14-1.pgdg13+1)"
	curation := validCurationTestResult(started)
	curation.Environment = environment
	curation.PostgreSQLVersion = lexical.PostgreSQLVersion
	recall := validRecallTestResult(started)
	recall.Environment = environment
	recall.PostgreSQLVersion = lexical.PostgreSQLVersion
	archive := validArchiveTestResult(started)
	archive.Environment = environment
	archive.PostgreSQLVersion = lexical.PostgreSQLVersion
	concurrency := validConcurrencyTestResult(started)
	concurrency.Environment = environment
	concurrency.PostgreSQLVersion = lexical.PostgreSQLVersion
	for name, value := range map[string]any{"lexical": lexical, "curation": curation, "recall": recall, "archive": archive, "concurrency": concurrency} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "memory-"+name+".json"), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	m := Manifest{Schema: ManifestSchemaV1, GeneratedAt: started, RunURL: "https://github.com/witwave-ai/witself/actions/runs/1234", RunAttempt: 1, Release: environment.Release, Commit: environment.Commit, Runner: ManifestRunner{Name: "github-actions-12", OS: "linux", Arch: "x64", Environment: "github-hosted"}, Postgres: ManifestPostgres{ImageLabel: "pg16", ServerVersionNum: 160014}, Outcome: "pass"}
	for _, name := range []string{"lexical", "curation", "recall", "archive", "concurrency"} {
		m.Slices = append(m.Slices, ManifestSlice{Name: name})
	}
	m, err := BuildManifest(dir, m, manifestSuccesses())
	if err != nil {
		t.Fatal(err)
	}
	if m.Outcome != "pass" {
		t.Fatalf("fixture did not pass: %#v", m)
	}
	return dir, m
}

func manifestSuccesses() map[string]string {
	return map[string]string{"lexical": "success", "curation": "success", "recall": "success", "archive": "success", "concurrency": "success"}
}
func manifestMutateJSON(t *testing.T, dir, name string, mutate func(map[string]any)) {
	t.Helper()
	path := filepath.Join(dir, "memory-"+name+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	mutate(value)
	raw, err = json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}
