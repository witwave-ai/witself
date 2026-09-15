package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/loadquality"
)

func TestDeriveReleaseAllowsOnlyMainAndExactSemver(t *testing.T) {
	for _, tc := range []struct{ ref, commit, want string }{
		{"refs/tags/v1.2.3", "abcdef1234567", "v1.2.3"},
		{"refs/tags/v1.2.3-rc.1+build.2", "abcdef1", "v1.2.3-rc.1+build.2"},
		{"refs/heads/main", "abcdef1234567", "main-abcdef1"},
		{"refs/heads/claude/foo", "abcdef1", ""},
		{"refs/tags/v01.2.3", "abcdef1", ""},
		{"refs/tags/v1.2.3-01", "abcdef1", ""},
		{"refs/tags/v1.2.3\n", "abcdef1", ""},
		{"refs/tags/v1.2.3", "nothex1", ""},
		{"refs/heads/main", "abc", ""},
		{"refs/heads/main", strings.Repeat("a", 65), ""},
	} {
		t.Run(tc.ref+"/"+tc.commit, func(t *testing.T) {
			got, err := deriveRelease(tc.ref, tc.commit)
			if (err != nil) != (tc.want == "") || got != tc.want {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestSanitizeRunnerLabelPinsDotlessMapping(t *testing.T) {
	for input, want := range map[string]string{
		"GitHub Actions 12": "github-actions-12", "runner.example.test": "runner-example-test",
		"  Linux / X64  ": "linux-x64", "github-hosted": "github-hosted", "": "",
		strings.Repeat("a", 129): strings.Repeat("a", 128),
	} {
		if got := sanitizeRunnerLabel(input); got != want {
			t.Errorf("mapping %q: got %q, want %q", input, got, want)
		}
	}
}

func TestParseOptionsUsesInjectedEnvironmentAndFlags(t *testing.T) {
	dir, env := commandFixture(t)
	opts, err := parseOptions([]string{"--evidence", dir, "--out", filepath.Join(dir, "workflow-manifest.json")}, envGet(env))
	if err != nil {
		t.Fatal(err)
	}
	if opts.evidence != dir || opts.manifest.Release != "main-abcdef1" || opts.manifest.RunAttempt != 2 ||
		opts.manifest.Runner.Name != "github-actions-12" || opts.manifest.Runner.OS != "linux" ||
		opts.manifest.Runner.Arch != "x64" || opts.manifest.Postgres.ServerVersionNum != 160014 || len(opts.manifest.Slices) != 5 {
		t.Fatalf("unexpected parsed options: %+v", opts.manifest)
	}
	for key, value := range map[string]string{"RELEASE": "wrong", "SLICES": "unknown", "RUN_ATTEMPT": "0", "POSTGRES_SERVER_VERSION_NUM": "broken", "RUNNER_NAME": "", "LEXICAL_OUTCOME": "pass"} {
		t.Run(key, func(t *testing.T) {
			copyEnv := make(map[string]string)
			for k, v := range env {
				copyEnv[k] = v
			}
			copyEnv[key] = value
			if _, err := parseOptions([]string{"--evidence", dir, "--out", filepath.Join(dir, "workflow-manifest.json")}, envGet(copyEnv)); err == nil {
				t.Fatal("accepted invalid environment")
			}
		})
	}
	for _, args := range [][]string{{"--unknown", "private-marker"}, {"--evidence", dir, "--out", filepath.Join(t.TempDir(), "outside.json")}, {"--evidence", dir, "extra"}} {
		if _, err := parseOptions(args, envGet(env)); err == nil || strings.Contains(err.Error(), "private-marker") {
			t.Fatalf("invalid flags leaked or passed: %v", err)
		}
	}
}

func TestRunWritesFailedManifestForMissingSelectedSlice(t *testing.T) {
	dir, env := commandFixture(t)
	env["SLICES"] = "lexical"
	env["LEXICAL_OUTCOME"] = "failure"
	env["POSTGRES_SERVER_VERSION_NUM"] = ""
	var output bytes.Buffer
	code := run([]string{"--evidence", dir, "--out", filepath.Join(dir, "workflow-manifest.json")}, envGet(env), &output)
	if code != 1 {
		t.Fatalf("got status %d: %s", code, output.String())
	}
	raw, err := os.ReadFile(filepath.Join(dir, "workflow-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest loadquality.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Outcome != "fail" || len(manifest.Slices) != 1 || manifest.Slices[0].Name != "lexical" || manifest.Slices[0].Outcome != "fail" {
		t.Fatalf("missing failed slice became passing evidence: %s", raw)
	}
	if safe, err := os.ReadFile(env["GITHUB_OUTPUT"]); err != nil || string(safe) != "safe_to_upload=true\n" {
		t.Fatalf("safe failed artifact was not published: %q, %v", safe, err)
	}
}

func TestRunRedactsLeaksAndKeepsFailureArtifact(t *testing.T) {
	dir, env := commandFixture(t)
	if err := os.WriteFile(filepath.Join(dir, "test-lexical.log"), []byte("database failure: secret-canary"), 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	code := run([]string{"--evidence", dir, "--out", filepath.Join(dir, "workflow-manifest.json")}, envGet(env), &output)
	if code != 1 || strings.Contains(output.String(), "secret-canary") {
		t.Fatalf("bad redaction exit/output: %d, %s", code, output.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "test-lexical.log")); !os.IsNotExist(err) {
		t.Fatal("raw log retained")
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "test-lexical.log.redacted")); err != nil || strings.Contains(string(raw), "secret-canary") {
		t.Fatalf("unsafe placeholder: %q, %v", raw, err)
	}
	if _, err := os.Stat(env["GITHUB_OUTPUT"]); err != nil {
		t.Fatal("safe failure artifact unavailable", err)
	}
}

func TestRunWithholdsUploadOnInvalidMetadata(t *testing.T) {
	dir, env := commandFixture(t)
	env["RUN_URL"] = "https://evil.example/actions/runs/1"
	var output bytes.Buffer
	if code := run([]string{"--evidence", dir, "--out", filepath.Join(dir, "workflow-manifest.json")}, envGet(env), &output); code != 2 {
		t.Fatalf("unsafe run returned %d: %s", code, output.String())
	}
	if _, err := os.Stat(env["GITHUB_OUTPUT"]); !os.IsNotExist(err) {
		t.Fatal("invalid report authorized upload")
	}
}

func TestRunBindsPassingResultToSuccessfulStepAndCleanScan(t *testing.T) {
	for _, tc := range []struct {
		name, status, log string
		wantCode          int
	}{
		{"pass", "success", "PASS\nok\tgithub.com/witwave-ai/witself/internal/store\t1.0s\n", 0},
		{"failed-process", "failure", "FAIL\n", 1},
		{"credential-leak", "success", "password=witself\n", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, env := commandFixture(t)
			env["SLICES"], env["LEXICAL_OUTCOME"] = "lexical", tc.status
			env["WITSELF_TEST_DATABASE_URL"] = "postgres://witself:witself@localhost:5432/witself?sslmode=disable"
			writeCommandResult(t, dir)
			result, err := os.ReadFile(filepath.Join(dir, "memory-lexical.json"))
			if err != nil {
				t.Fatal(err)
			}
			// The real harness logs both its JSON and its absolute output path.
			log := string(result) + "\n" + filepath.Join(dir, "memory-lexical.json") + "\n" + tc.log
			if err := os.WriteFile(filepath.Join(dir, "test-lexical.log"), []byte(log), 0o600); err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			if code := run([]string{"--evidence", dir, "--out", filepath.Join(dir, "workflow-manifest.json")}, envGet(env), &output); code != tc.wantCode {
				t.Fatalf("got %d, want %d: %s", code, tc.wantCode, output.String())
			}
			if safe, err := os.ReadFile(env["GITHUB_OUTPUT"]); err != nil || string(safe) != "safe_to_upload=true\n" {
				t.Fatalf("safe artifact marker: %q, %v", safe, err)
			}
		})
	}
}

func TestRunPreservesValidatedReleaseEvidence(t *testing.T) {
	for _, status := range []string{"success", "failure"} {
		t.Run(status, func(t *testing.T) {
			dir, env := commandFixture(t)
			env["GITHUB_REF"], env["RELEASE"] = "refs/tags/v1.2.3-witself", "v1.2.3-witself"
			env["SLICES"], env["LEXICAL_OUTCOME"] = "lexical", status
			env["WITSELF_TEST_DATABASE_URL"] = "postgres://witself:witself@localhost:5432/witself?sslmode=disable"
			writeCommandReleaseResult(t, dir, env["RELEASE"])
			resultPath := filepath.Join(dir, "memory-lexical.json")
			result, err := os.ReadFile(resultPath)
			if err != nil {
				t.Fatal(err)
			}
			var compact bytes.Buffer
			if err := json.Compact(&compact, result); err != nil {
				t.Fatal(err)
			}
			// Cover the indented artifact and compact JSON emitted by harness logs.
			logPath := filepath.Join(dir, "test-lexical.log")
			log := string(result) + "\n" + compact.String() + "\n" + resultPath + "\n" + status + "\n"
			if err := os.WriteFile(logPath, []byte(log), 0o600); err != nil {
				t.Fatal(err)
			}
			wantCode, wantOutcome := 0, "pass"
			if status == "failure" {
				wantCode, wantOutcome = 1, "fail"
			}
			var output bytes.Buffer
			manifestPath := filepath.Join(dir, "workflow-manifest.json")
			if code := run([]string{"--evidence", dir, "--out", manifestPath}, envGet(env), &output); code != wantCode {
				t.Fatalf("got %d, want %d: %s", code, wantCode, output.String())
			}
			if safe, err := os.ReadFile(env["GITHUB_OUTPUT"]); err != nil || string(safe) != "safe_to_upload=true\n" {
				t.Fatalf("safe artifact marker: %q, %v", safe, err)
			}
			for path, want := range map[string]string{resultPath: string(result), logPath: log} {
				if kept, err := os.ReadFile(path); err != nil || string(kept) != want {
					t.Fatalf("release evidence was removed or changed: %s: %v", filepath.Base(path), err)
				}
			}
			raw, err := os.ReadFile(manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			var manifest loadquality.Manifest
			if err := json.Unmarshal(raw, &manifest); err != nil {
				t.Fatal(err)
			}
			if manifest.Release != env["RELEASE"] || manifest.Outcome != wantOutcome || len(manifest.Slices) != 1 || manifest.Slices[0].Outcome != wantOutcome || manifest.Slices[0].SHA256 == "" {
				t.Fatalf("incorrect retained manifest: %s", raw)
			}
			if err := loadquality.ValidateManifest(manifest, dir); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRunRetainsHostedGoFailureArtifacts(t *testing.T) {
	for _, name := range []string{"timeout", "panic"} {
		for _, resultExists := range []bool{false, true} {
			t.Run(name+map[bool]string{false: "-before-result", true: "-after-result"}[resultExists], func(t *testing.T) {
				dir, env := commandFixture(t)
				env["SLICES"], env["LEXICAL_OUTCOME"] = "lexical", "failure"
				env["WITSELF_TEST_DATABASE_URL"] = "postgres://witself:witself@localhost:5432/witself?sslmode=disable"
				if resultExists {
					writeCommandResult(t, dir)
				}
				raw, err := os.ReadFile(filepath.Join("..", "..", "loadquality", "testdata", "hosted-go-"+name+".log"))
				if err != nil {
					t.Fatal(err)
				}
				logPath := filepath.Join(dir, "test-lexical.log")
				if err := os.WriteFile(logPath, raw, 0600); err != nil {
					t.Fatal(err)
				}
				var output bytes.Buffer
				manifestPath := filepath.Join(dir, "workflow-manifest.json")
				if code := run([]string{"--evidence", dir, "--out", manifestPath}, envGet(env), &output); code != 1 {
					t.Fatalf("got %d, want sanitized failure: %s", code, output.String())
				}
				if kept, err := os.ReadFile(logPath); err != nil || string(kept) != string(raw) {
					t.Fatalf("failure diagnostics were removed or changed: %v", err)
				}
				if marker, err := os.ReadFile(env["GITHUB_OUTPUT"]); err != nil || string(marker) != "safe_to_upload=true\n" {
					t.Fatalf("safe failure artifact was withheld: %q, %v", marker, err)
				}
				manifestRaw, err := os.ReadFile(manifestPath)
				if err != nil {
					t.Fatal(err)
				}
				var manifest loadquality.Manifest
				if err := json.Unmarshal(manifestRaw, &manifest); err != nil {
					t.Fatal(err)
				}
				if manifest.Outcome != "fail" || len(manifest.Slices) != 1 || manifest.Slices[0].Outcome != "fail" || (manifest.Slices[0].SHA256 != "") != resultExists {
					t.Fatalf("failure artifact lost its step outcome or result digest: %s", manifestRaw)
				}
				if err := loadquality.ValidateManifest(manifest, dir); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestRunRedactsCredentialsBesideHostedGoFailureFrames(t *testing.T) {
	for _, name := range []string{"timeout", "panic"} {
		t.Run(name, func(t *testing.T) {
			dir, env := commandFixture(t)
			env["SLICES"], env["LEXICAL_OUTCOME"] = "lexical", "failure"
			env["WITSELF_TEST_DATABASE_URL"] = "postgres://witself:witself@localhost:5432/witself?sslmode=disable"
			writeCommandResult(t, dir)
			raw, err := os.ReadFile(filepath.Join("..", "..", "loadquality", "testdata", "hosted-go-"+name+".log"))
			if err != nil {
				t.Fatal(err)
			}
			logPath := filepath.Join(dir, "test-lexical.log")
			lines := strings.Split(string(raw), "\n")
			mutated := false
			for i, line := range lines {
				if strings.HasPrefix(line, "\t/home/runner/work/witself/witself/") {
					lines[i] += " password=witself"
					mutated = true
					break
				}
			}
			if !mutated {
				t.Fatal("fixture is missing a hosted source frame")
			}
			log := strings.Join(lines, "\n")
			if err := os.WriteFile(logPath, []byte(log), 0600); err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			manifestPath := filepath.Join(dir, "workflow-manifest.json")
			if code := run([]string{"--evidence", dir, "--out", manifestPath}, envGet(env), &output); code != 1 {
				t.Fatalf("got %d, want sanitized failure: %s", code, output.String())
			}
			if _, err := os.Stat(logPath); !os.IsNotExist(err) {
				t.Fatal("credential-bearing stack retained")
			}
			if notice, err := os.ReadFile(logPath + ".redacted"); err != nil || strings.Contains(string(notice), "witself") {
				t.Fatalf("unsafe redaction notice: %q, %v", notice, err)
			}
			if marker, err := os.ReadFile(env["GITHUB_OUTPUT"]); err != nil || string(marker) != "safe_to_upload=true\n" {
				t.Fatalf("safe failure artifact was withheld: %q, %v", marker, err)
			}
			manifestRaw, err := os.ReadFile(manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			var manifest loadquality.Manifest
			if err := json.Unmarshal(manifestRaw, &manifest); err != nil {
				t.Fatal(err)
			}
			if manifest.Outcome != "fail" || len(manifest.Slices) != 1 || manifest.Slices[0].Outcome != "fail" || manifest.Slices[0].SHA256 == "" {
				t.Fatalf("invalid redacted failure artifact: %s", manifestRaw)
			}
			if err := loadquality.ValidateManifest(manifest, dir); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRunReleaseExemptionKeepsCredentialScanning(t *testing.T) {
	for _, tc := range []struct {
		name, jsonField, value, logExtra string
	}{
		{name: "adjacent log credential", logExtra: ` password=witself`},
		{name: "release under another log key", logExtra: ` "credential":"v1.2.3-witself"`},
		{name: "mismatched log release", logExtra: ` "release":"v1.2.3-witself-other"`},
		{name: "credential in another JSON field", jsonField: "postgresql_version", value: "16.14-witself"},
		{name: "mismatched JSON release", jsonField: "release", value: "v1.2.3-witself-other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, env := commandFixture(t)
			env["GITHUB_REF"], env["RELEASE"] = "refs/tags/v1.2.3-witself", "v1.2.3-witself"
			env["SLICES"] = "lexical"
			env["WITSELF_TEST_DATABASE_URL"] = "postgres://witself:witself@localhost:5432/witself?sslmode=disable"
			writeCommandReleaseResult(t, dir, env["RELEASE"])
			resultPath := filepath.Join(dir, "memory-lexical.json")
			if tc.jsonField != "" {
				raw, err := os.ReadFile(resultPath)
				if err != nil {
					t.Fatal(err)
				}
				var result loadquality.Result
				if err := json.Unmarshal(raw, &result); err != nil {
					t.Fatal(err)
				}
				if tc.jsonField == "release" {
					result.Environment.Release = tc.value
				} else {
					result.PostgreSQLVersion = tc.value
				}
				if _, err := loadquality.WriteResult(resultPath, result); err != nil {
					t.Fatal(err)
				}
			}
			logPath := filepath.Join(dir, "test-lexical.log")
			log := `"release":"v1.2.3-witself"` + tc.logExtra + "\n"
			if err := os.WriteFile(logPath, []byte(log), 0o600); err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			manifestPath := filepath.Join(dir, "workflow-manifest.json")
			if code := run([]string{"--evidence", dir, "--out", manifestPath}, envGet(env), &output); code != 1 {
				t.Fatalf("got %d, want sanitized failure: %s", code, output.String())
			}
			if safe, err := os.ReadFile(env["GITHUB_OUTPUT"]); err != nil || string(safe) != "safe_to_upload=true\n" {
				t.Fatalf("safe failure artifact marker: %q, %v", safe, err)
			}
			unsafePath, safePath := logPath, resultPath
			if tc.jsonField != "" {
				unsafePath, safePath = resultPath, logPath
			}
			if _, err := os.Stat(unsafePath); !os.IsNotExist(err) {
				t.Fatal("credential-bearing evidence retained")
			}
			if notice, err := os.ReadFile(unsafePath + ".redacted"); err != nil || strings.Contains(string(notice), "witself") {
				t.Fatalf("unsafe redaction notice: %q, %v", notice, err)
			}
			if _, err := os.Stat(safePath); err != nil {
				t.Fatal("public release evidence removed", err)
			}
			raw, err := os.ReadFile(manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			var manifest loadquality.Manifest
			if err := json.Unmarshal(raw, &manifest); err != nil {
				t.Fatal(err)
			}
			if manifest.Release != env["RELEASE"] || manifest.Outcome != "fail" {
				t.Fatalf("incorrect failure manifest: %s", raw)
			}
			if err := loadquality.ValidateManifest(manifest, dir); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func writeCommandResult(t *testing.T, dir string) {
	t.Helper()
	writeCommandReleaseResult(t, dir, "main-abcdef1")
}

func writeCommandReleaseResult(t *testing.T, dir, release string) {
	t.Helper()
	started := time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)
	result := loadquality.Result{
		Schema: loadquality.ResultSchemaV1, HarnessVersion: loadquality.HarnessVersion,
		StartedAt: started, CompletedAt: started.Add(time.Second), Outcome: "pass", PostgreSQLVersion: "16.14",
		Environment: loadquality.SafeMetadata{
			Release: release, Commit: "abcdef1234567", Provider: "github-hosted", HardwareTier: "ubuntu-latest-pg16",
			GoVersion: "go1.26.6", GOOS: "linux", GOARCH: "amd64", LogicalCPUs: 4,
		},
		Workload: loadquality.Workload{
			Seed: loadquality.DefaultSeed, CorpusSHA256: strings.Repeat("a", 64), SyntheticAccounts: 2, SyntheticAgents: 3,
			CorpusMemories: 3, NoiseMemories: 0, QueryIterations: 1, Concurrency: 1,
		},
		Measurements: loadquality.Measurements{
			Capture: loadquality.OperationStats{Count: 3, WallDurationMS: 10, ThroughputPerSecond: 300},
			Recall:  loadquality.OperationStats{Count: 1, WallDurationMS: 10, ThroughputPerSecond: 100},
		},
		Quality: loadquality.Quality{
			RelevanceCases:    []loadquality.RelevanceCaseResult{{Name: "recall", Passed: true, ObservedRank: 1, MaximumRank: 1}},
			RelevancePassRate: 1, SensitiveBroadRedacted: true, SensitiveExactOwnerVisible: true, CrossAgentIsolated: true, CrossTenantIsolated: true,
		},
	}
	if _, err := loadquality.WriteResult(filepath.Join(dir, "memory-lexical.json"), result); err != nil {
		t.Fatal(err)
	}
}

func commandFixture(t *testing.T) (string, map[string]string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "witself", "witself", "evidence")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir, map[string]string{
		"GITHUB_REF": "refs/heads/main", "RELEASE": "main-abcdef1", "COMMIT": "abcdef1234567",
		"RUN_URL": "https://github.com/witwave-ai/witself/actions/runs/123", "RUN_ATTEMPT": "2",
		"RUNNER_NAME": "GitHub Actions 12", "RUNNER_OS": "Linux", "RUNNER_ARCH": "X64", "RUNNER_ENVIRONMENT": "github-hosted",
		"POSTGRES_IMAGE_LABEL": "pg16", "POSTGRES_SERVER_VERSION_NUM": "160014", "SLICES": "all",
		"LEXICAL_OUTCOME": "success", "CURATION_OUTCOME": "success", "RECALL_OUTCOME": "success", "ARCHIVE_OUTCOME": "success", "CONCURRENCY_OUTCOME": "success",
		"WITSELF_TEST_DATABASE_URL": "postgres://fixture-user:secret-canary@db.example:5599/fixture-db?sslmode=disable",
		"GITHUB_OUTPUT":             filepath.Join(t.TempDir(), "github-output"),
	}
}

func envGet(env map[string]string) func(string) string {
	return func(key string) string { return env[key] }
}
