// Command memory-load-quality-manifest validates and sanitizes hosted harness
// evidence before authorizing its upload. It never opens a database connection.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/loadquality"
)

type options struct {
	evidence, out, dsn, githubOutput string
	manifest                         loadquality.Manifest
	outcomes                         map[string]string
}

var (
	commitPattern = regexp.MustCompile(`^[0-9a-f]{7,64}$`)
	semverPattern = regexp.MustCompile(`^refs/tags/v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-(0|[1-9][0-9]*|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9][0-9]*|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*))*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`)
	runnerPattern = regexp.MustCompile(`[^a-z0-9+_-]+`)
)

func main() { os.Exit(run(os.Args[1:], os.Getenv, os.Stderr)) }

// Exit 1 means a sanitized failure artifact is available; exit 2 means no
// upload is authorized. Only the final output marker authorizes upload.
func run(args []string, getenv func(string) string, output io.Writer) int {
	opts, err := parseOptions(args, getenv)
	if err != nil {
		_, _ = fmt.Fprintln(output, "memory load-quality manifest: invalid options")
		return 2
	}
	if err := os.MkdirAll(opts.evidence, 0o700); err != nil {
		_, _ = fmt.Fprintln(output, "memory load-quality manifest: evidence directory unavailable")
		return 2
	}
	if err := loadquality.ScanEvidence(opts.evidence, opts.dsn, opts.manifest.Release); err != nil {
		if !errors.Is(err, loadquality.ErrEvidenceRedacted) {
			_, _ = fmt.Fprintln(output, "memory load-quality manifest: evidence scan failed")
			return 2
		}
		opts.manifest.Outcome = "fail"
	}
	manifest, err := loadquality.BuildManifest(opts.evidence, opts.manifest, opts.outcomes)
	if err != nil {
		_, _ = fmt.Fprintln(output, "memory load-quality manifest: invalid evidence metadata")
		return 2
	}
	if _, err := loadquality.WriteManifest(opts.out, manifest, opts.evidence); err != nil {
		_, _ = fmt.Fprintln(output, "memory load-quality manifest: publication failed")
		return 2
	}
	// Include the newly written document in the final scan. Any change here
	// withholds upload; an earlier clean scan alone cannot approve new bytes.
	if err := loadquality.ScanEvidence(opts.evidence, opts.dsn, opts.manifest.Release); err != nil {
		_, _ = fmt.Fprintln(output, "memory load-quality manifest: final evidence scan failed")
		return 2
	}
	if opts.githubOutput != "" {
		if err := writeUploadMarker(opts.githubOutput); err != nil {
			_, _ = fmt.Fprintln(output, "memory load-quality manifest: upload marker unavailable")
			return 2
		}
	}
	if manifest.Outcome != "pass" {
		_, _ = fmt.Fprintln(output, "memory load-quality manifest: sanitized failure evidence written")
		return 1
	}
	_, _ = fmt.Fprintln(output, "memory load-quality manifest: sanitized passing evidence written")
	return 0
}

func parseOptions(args []string, getenv func(string) string) (options, error) {
	invalid := errors.New("invalid memory load-quality manifest options")
	opts := options{outcomes: make(map[string]string)}
	flags := flag.NewFlagSet("memory-load-quality-manifest", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&opts.evidence, "evidence", "evidence", "directory of sanitized slice evidence")
	flags.StringVar(&opts.out, "out", "evidence/workflow-manifest.json", "manifest in the evidence directory")
	if flags.Parse(args) != nil || flags.NArg() != 0 || opts.evidence == "" || opts.out == "" {
		return options{}, invalid
	}
	var err error
	if opts.evidence, err = filepath.Abs(opts.evidence); err != nil {
		return options{}, invalid
	}
	if opts.out, err = filepath.Abs(opts.out); err != nil || opts.out != filepath.Join(opts.evidence, "workflow-manifest.json") {
		return options{}, invalid
	}
	commit := getenv("COMMIT")
	release, err := deriveRelease(getenv("GITHUB_REF"), commit)
	if err != nil || (getenv("RELEASE") != "" && getenv("RELEASE") != release) {
		return options{}, invalid
	}
	attempt, err := strconv.Atoi(getenv("RUN_ATTEMPT"))
	if err != nil || attempt < 1 {
		return options{}, invalid
	}
	version := 0
	if value := getenv("POSTGRES_SERVER_VERSION_NUM"); value != "" {
		version, err = strconv.Atoi(value)
		if err != nil || version < 0 {
			return options{}, invalid
		}
	}
	opts.manifest = loadquality.Manifest{
		Schema: loadquality.ManifestSchemaV1, GeneratedAt: time.Now().UTC(),
		RunURL: getenv("RUN_URL"), RunAttempt: attempt, Release: release, Commit: commit,
		Runner: loadquality.ManifestRunner{
			Name: sanitizeRunnerLabel(getenv("RUNNER_NAME")), OS: sanitizeRunnerLabel(getenv("RUNNER_OS")),
			Arch: sanitizeRunnerLabel(getenv("RUNNER_ARCH")), Environment: sanitizeRunnerLabel(getenv("RUNNER_ENVIRONMENT")),
		},
		Postgres: loadquality.ManifestPostgres{ImageLabel: getenv("POSTGRES_IMAGE_LABEL"), ServerVersionNum: version},
		Outcome:  "pass",
	}
	if opts.manifest.Runner.Name == "" || opts.manifest.Runner.OS == "" || opts.manifest.Runner.Arch == "" || opts.manifest.Runner.Environment == "" {
		return options{}, invalid
	}
	selection := getenv("SLICES")
	if selection == "" {
		selection = "all"
	}
	for _, name := range []string{"lexical", "curation", "recall", "archive", "concurrency"} {
		if selection != "all" && selection != name {
			continue
		}
		opts.manifest.Slices = append(opts.manifest.Slices, loadquality.ManifestSlice{Name: name})
		outcome := getenv(strings.ToUpper(name) + "_OUTCOME")
		switch outcome {
		case "success", "failure", "cancelled", "skipped", "":
			opts.outcomes[name] = outcome
		default:
			return options{}, invalid
		}
	}
	if len(opts.manifest.Slices) == 0 {
		return options{}, invalid
	}
	opts.dsn, opts.githubOutput = getenv("WITSELF_TEST_DATABASE_URL"), getenv("GITHUB_OUTPUT")
	return opts, nil
}

func deriveRelease(ref, commit string) (string, error) {
	if commitPattern.MatchString(commit) {
		if ref == "refs/heads/main" {
			return "main-" + commit[:7], nil
		}
		if semverPattern.MatchString(ref) && len(strings.TrimPrefix(ref, "refs/tags/")) <= 128 {
			return strings.TrimPrefix(ref, "refs/tags/"), nil
		}
	}
	return "", errors.New("release must identify main or an exact semantic-version tag and a hexadecimal commit")
}

func sanitizeRunnerLabel(value string) string {
	value = strings.Trim(runnerPattern.ReplaceAllString(strings.ToLower(value), "-"), "-+_")
	if len(value) > 128 {
		value = strings.TrimRight(value[:128], "-+_")
	}
	return value
}

func writeUploadMarker(path string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := io.WriteString(file, "safe_to_upload=true\n")
	return errors.Join(writeErr, file.Close())
}
