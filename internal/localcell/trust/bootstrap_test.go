package trust_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

// The scripts exercise the very functions used by the installers, without
// installing a candidate or contacting a release service. Keep each interpreter
// visible: missing PowerShell must never masquerade as cross-platform evidence.
func TestBootstrapVerifierParity(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	for _, suite := range []struct {
		name     string
		deps     []string
		commands [][]string
	}{
		{"bash", []string{"bash", "openssl"}, [][]string{{"scripts/test-bootstrap-verifiers.sh"}}},
		{"pwsh", []string{"pwsh"}, [][]string{
			{"-NoLogo", "-NoProfile", "-NonInteractive", "-File", "scripts/test-bootstrap-verifiers.ps1"},
			{"-NoLogo", "-NoProfile", "-NonInteractive", "-File", "scripts/test-install-windows.ps1", "-BootstrapOnly"},
		}},
	} {
		t.Run(suite.name, func(t *testing.T) {
			for _, dep := range suite.deps {
				if _, err := exec.LookPath(dep); err != nil {
					if os.Getenv("WITSELF_TEST_REQUIRE_BOOTSTRAP_PARITY") == "1" {
						t.Fatalf("NOT_RUN %s_unavailable (bootstrap parity required)", dep)
					}
					t.Skipf("NOT_RUN %s_unavailable", dep)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			for _, args := range suite.commands {
				cmd := exec.CommandContext(ctx, suite.name, args...)
				cmd.Dir = root
				cmd.Env = append(os.Environ(), "WITSELF_HOME="+t.TempDir(), "DSH_HOME="+t.TempDir())
				out, err := cmd.CombinedOutput()
				// Even a broken external verifier must not put key identifiers or
				// payload digests in captured Go test diagnostics.
				output := regexp.MustCompile(`(?i)[0-9a-f]{64}`).ReplaceAllString(string(out), "[redacted]")
				if err != nil {
					t.Fatalf("bootstrap parity failed: %v\n%s", err, output)
				}
				t.Log(strings.TrimSpace(output))
			}
		})
	}
}

// These expectations deliberately do not reuse the shell predicate. A green
// corpus cannot expose an adjudicator that silently ignores refused positives.
func TestBootstrapParityAdjudication(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("NOT_RUN bash_unavailable")
	}
	for _, tc := range []struct {
		name, positive, expected, result, status string
		wantOK                                   bool
	}{
		{"PositiveAcceptedZero", "true", "accepted", "accepted", "0", true},
		{"PositiveRefusedNonzero", "true", "accepted", "signature_invalid", "1", false},
		{"PositiveAcceptedNonzero", "true", "accepted", "accepted", "1", false},
		{"NegativeAcceptedZero", "false", "signature_invalid", "accepted", "0", false},
		{"NegativeClosedReasonRefusedNonzero", "false", "signature_invalid", "signature_invalid", "1", true},
		{"UnknownReason", "false", "not_a_closed_reason", "not_a_closed_reason", "1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "bash", "-c",
				`source "$1"; shift; ws_bootstrap_adjudicate "$@"`, "adjudication",
				filepath.ToSlash(filepath.Join("..", "..", "..", "scripts", "test-bootstrap-verifiers.sh")),
				tc.name, tc.positive, tc.expected, tc.result, tc.status)
			out, err := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatal("adjudicator timed out")
			}
			if (err == nil) != tc.wantOK {
				output := regexp.MustCompile(`(?i)[0-9a-f]{64}`).ReplaceAllString(string(out), "[redacted]")
				t.Fatalf("adjudicator success=%t, want %t: %s", err == nil, tc.wantOK, output)
			}
		})
	}
}

// Exercise both the real script and the actual Go driver with one controlled
// verifier refusal. All other verifier calls, including delegation setup, use
// the real installer, and the full corpus index is copied without filtering.
func TestBootstrapVerifierParityRejectsRefusedPositive(t *testing.T) {
	for _, dep := range []string{"bash", "openssl"} {
		if _, err := exec.LookPath(dep); err != nil {
			if os.Getenv("WITSELF_TEST_REQUIRE_BOOTSTRAP_PARITY") == "1" {
				t.Fatalf("NOT_RUN %s_unavailable (bootstrap parity required)", dep)
			}
			t.Skipf("NOT_RUN %s_unavailable", dep)
		}
	}
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	fixture := t.TempDir()
	packageDir := filepath.Join(fixture, "internal", "localcell", "trust")
	for _, dir := range []string{filepath.Join(fixture, "scripts"), packageDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	script, err := os.ReadFile(filepath.Join(root, "scripts", "test-bootstrap-verifiers.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture, "scripts", "test-bootstrap-verifiers.sh"), script, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.CopyFS(filepath.Join(packageDir, "testdata"), os.DirFS(filepath.Join(root, "internal", "localcell", "trust", "testdata"))); err != nil {
		t.Fatal(err)
	}
	// Quote the locally resolved path for Bash; no environment or production
	// installer hook is needed to substitute this test-only verifier.
	installer := "'" + strings.ReplaceAll(filepath.ToSlash(filepath.Join(root, "install.sh")), "'", "'\"'\"'") + "'"
	wrapper := "WS_BOOTSTRAP_LIBRARY=1 source " + installer + `
	eval "$(declare -f ws_cs_apply | sed '1s/ws_cs_apply/ws_cs_apply_uncontrolled/')"
	ws_cs_apply() {
	  case "$3" in
	    */positive-base.json) printf 'signature_invalid\n'; return 1 ;;
	  esac
	  ws_cs_apply_uncontrolled "$@"
	}
`
	if err := os.WriteFile(filepath.Join(fixture, "install.sh"), []byte(wrapper), 0o600); err != nil {
		t.Fatal(err)
	}
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, command, dir string
		args               []string
	}{
		{"Script", "bash", fixture, []string{"scripts/test-bootstrap-verifiers.sh"}},
		{"GoDriver", testBinary, packageDir, []string{"-test.run=^TestBootstrapVerifierParity$/^bash$", "-test.count=1", "-test.v"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, tc.command, tc.args...)
			cmd.Dir = tc.dir
			cmd.Env = append(os.Environ(), "WITSELF_HOME="+t.TempDir(), "DSH_HOME="+t.TempDir())
			out, err := cmd.CombinedOutput()
			output := regexp.MustCompile(`(?i)[0-9a-f]{64}`).ReplaceAllString(string(out), "[redacted]")
			if ctx.Err() != nil {
				t.Fatal("controlled verifier challenge timed out")
			}
			if err == nil {
				t.Fatal("parity accepted the controlled positive refusal")
			}
			if !strings.Contains(output, "shell parity: positive-base got signature_invalid status=1 want accepted") {
				t.Fatalf("challenge failed outside positive adjudication: %s", output)
			}
			if tc.name == "GoDriver" && !strings.Contains(output, "--- FAIL: TestBootstrapVerifierParity/bash") {
				t.Fatalf("actual Go parity driver did not report its failure: %s", output)
			}
		})
	}
}

type bootstrapReleaseConfig struct {
	Builds []struct {
		ID     string         `yaml:"id"`
		Main   string         `yaml:"main"`
		Binary string         `yaml:"binary"`
		Dir    string         `yaml:"dir"`
		Hooks  map[string]any `yaml:"hooks"`
	} `yaml:"builds"`
	Archives []map[string]any `yaml:"archives"`
}

func clientArchiveBoundary(config bootstrapReleaseConfig) bool {
	for _, client := range []string{"witself", "witself-admin", "witself-infra"} {
		buildCount, archiveCount := 0, 0
		dir := ""
		if client == "witself-infra" {
			dir = "infra/pulumi"
		}
		for _, build := range config.Builds {
			if build.ID != client {
				continue
			}
			buildCount++
			if build.Binary != client || build.Main != "./cmd/"+client || build.Dir != dir || len(build.Hooks) != 0 {
				return false
			}
		}
		for _, archive := range config.Archives {
			if archive["id"] != client {
				continue
			}
			archiveCount++
			// These are the currently declared client assets: one matching
			// binary and no supplemental files. New packaging capabilities
			// need deliberate review rather than an implicit permissive default.
			for field := range archive {
				switch field {
				case "id", "ids", "formats", "format_overrides", "name_template", "files":
				default:
					return false
				}
			}
			ids, idsOK := archive["ids"].([]any)
			files, filesOK := archive["files"].([]any)
			if !idsOK || len(ids) != 1 || ids[0] != client || !filesOK || len(files) != 1 || files[0] != "none*" {
				return false
			}
			if archive["name_template"] != client+"_{{ .Version }}_{{ .Os }}_{{ .Arch }}" {
				return false
			}
		}
		if buildCount != 1 || archiveCount != 1 {
			return false
		}
	}
	return true
}

func readBootstrapReleaseConfig(t *testing.T) bootstrapReleaseConfig {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", ".goreleaser.yaml"))
	if err != nil {
		t.Fatal("release configuration unavailable")
	}
	var config bootstrapReleaseConfig
	if yaml.Unmarshal(data, &config) != nil {
		t.Fatal("invalid release configuration")
	}
	return config
}

func TestRemoteOnlyClientArchiveBoundary(t *testing.T) {
	if !clientArchiveBoundary(readBootstrapReleaseConfig(t)) {
		t.Fatal("remote-only archives must contain only their matching client binary and declared client assets")
	}
	// GoReleaser's explicit sentinel glob must continue matching no files.
	matches, err := filepath.Glob(filepath.Join("..", "..", "..", "none*"))
	if err != nil || len(matches) != 0 {
		t.Fatal("client archive sentinel unexpectedly includes files")
	}
}

func TestRemoteOnlyClientArchiveBoundaryRejectsCellPayloads(t *testing.T) {
	for _, payload := range []string{"witself-server", "witself-worker", "postgresql", "extension", "service-host"} {
		t.Run(payload, func(t *testing.T) {
			for _, field := range []string{"ids", "files"} {
				config := readBootstrapReleaseConfig(t)
				for _, archive := range config.Archives {
					if archive["id"] == "witself" {
						archive[field] = append(archive[field].([]any), payload)
					}
				}
				if clientArchiveBoundary(config) {
					t.Fatalf("cell payload accepted through %s", field)
				}
			}
		})
	}
}
