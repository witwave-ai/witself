// Package capturemix runs reviewed executables against disposable capture
// fixtures. It never builds, downloads, or substitutes an executable.
package capturemix

import (
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/store"
	"github.com/witwave-ai/witself/internal/syncharness"
)

const syncPredecessorCommit = "2f1c194398c5df943c305b8e11c9ab32db0e7f3e"

// PredecessorCommit returns the immutable, reviewed source identity.
func PredecessorCommit() string { return syncPredecessorCommit }

// Options contains explicit private fixture inputs. DSNs, tokens and identities
// must never be formatted in diagnostics. Activate is supplied by the external
// store test: its local-only test helper is not available to production callers.
type Options struct {
	CurrentBinDir, PredecessorBinDir                          string
	PredecessorCommit, PredecessorCommitFile, PredecessorTree string
	OwnerDSN, SuperuserDSN, ArtifactDir, StubEmailURL         string
	DisposableCluster                                         bool
	Activate                                                  func(context.Context, *store.Store, string) error
	// Each fixture owns its copy; retain retry diagnostics through cleanup.
	startRetries []string
}

// RowResult contains only fixed, value-free assertion names.
type RowResult struct {
	Server      string                         `json:"server"`
	Worker      string                         `json:"worker"`
	Completion  string                         `json:"completion"`
	Checks      []string                       `json:"checks"`
	NotRun      []string                       `json:"not_run,omitempty"`
	JobEvidence []syncharness.MixedJobEvidence `json:"job_evidence,omitempty"`
}

// Result keeps private build identities separate from value-free row summaries.
type Result struct {
	Rows             []RowResult                       `json:"rows"`
	NotRun           []string                          `json:"not_run,omitempty"`
	StartRetries     []string                          `json:"start_retries,omitempty"`
	Identity         syncharness.MixedBuildIdentity    `json:"build_identity"`
	RuntimeResults   []syncharness.RuntimeResultRecord `json:"runtime_results"`
	RuntimeArtifacts []syncharness.RuntimeArtifact     `json:"runtime_artifacts,omitempty"`
}

// Run executes the four server/worker combinations with both commit and abort
// installation drains. All fixture cleanup finishes before it returns.
func Run(ctx context.Context, options Options) (result Result, resultErr error) {
	if err := verifyPredecessor(ctx, options); err != nil {
		return result, err
	}
	// This driver owns and records its stub requests. A caller-supplied HTTP
	// server cannot establish that no request was forwarded or omitted from the
	// private request artifact, even when its address is loopback.
	if options.StubEmailURL != "" {
		return result, errors.New("capturemix external stub refused")
	}
	if options.Activate == nil || options.CurrentBinDir == "" || options.PredecessorBinDir == "" {
		return result, errors.New("capturemix options incomplete")
	}
	identity, err := buildIdentity(options)
	if err != nil {
		return result, err
	}
	result.Identity = identity
	if options.ArtifactDir != "" {
		if err := writePrivateJSON(filepath.Join(options.ArtifactDir, "build-identities.json"), identity); err != nil {
			return result, err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Minute)
	defer cancel()
	cluster, err := openCluster(ctx, options)
	if err != nil {
		return result, err
	}
	defer func() {
		if err := cluster.close(); err != nil {
			resultErr = errors.Join(resultErr, errors.New("capturemix cluster cleanup failed"), err)
		}
		if resultErr == nil {
			resultErr = finalizeRuntimeResults(&result, options.DisposableCluster)
		}
		if options.ArtifactDir != "" && writePrivateJSON(filepath.Join(options.ArtifactDir, "runtime-result.json"), result) != nil {
			resultErr = errors.Join(resultErr, errors.New("capturemix result artifact failed"))
		}
	}()
	if !options.DisposableCluster {
		result.NotRun = append(result.NotRun, "NOT RUN mixed_binaries_roles_unavailable", "NOT RUN proof_profile_database_unavailable", "NOT RUN current_server_profile_database_unavailable", "NOT RUN current_process_preinstall_unavailable")
	}
	for _, server := range []string{"old", "new"} {
		for _, worker := range []string{"old", "new"} {
			row := RowResult{Server: server, Worker: worker}
			for _, completion := range []string{"commit", "rollback"} {
				row.Completion = completion
				fixture, err := cluster.fixture(ctx, options, server, worker)
				if err != nil {
					return result, err
				}
				checks, runErr := executeRowWithCleanup(ctx, fixture, completion, fixture.close)
				result.StartRetries = append(result.StartRetries, fixture.options.startRetries...)
				row.Checks = append(row.Checks, checks...)
				if runErr != nil {
					return result, runErr
				}
				if completion == "commit" {
					row.JobEvidence = append([]syncharness.MixedJobEvidence(nil), fixture.jobEvidence...)
				} else if !reflect.DeepEqual(row.JobEvidence, fixture.jobEvidence) {
					return result, errors.New("capturemix drain job evidence differs")
				}
				for _, evidence := range fixture.jobEvidence {
					row.Checks = append(row.Checks, evidence.Check())
				}
			}
			row.Completion = "commit_and_rollback"
			row.NotRun = append([]string(nil), result.NotRun...)
			result.Rows = append(result.Rows, row)
		}
	}
	return result, nil
}

func verifyPredecessor(ctx context.Context, options Options) error {
	refused := errors.New("predecessor_commit_mismatch")
	if options.PredecessorCommit != syncPredecessorCommit {
		return refused
	}
	var raw []byte
	var err error
	if options.PredecessorTree != "" {
		command := exec.CommandContext(ctx, "git", "-C", options.PredecessorTree, "rev-parse", "HEAD")
		command.Stderr = io.Discard
		raw, err = command.Output()
	} else if options.PredecessorCommitFile != "" {
		raw, err = os.ReadFile(options.PredecessorCommitFile)
	} else {
		return refused
	}
	if err != nil || strings.TrimSpace(string(raw)) != syncPredecessorCommit {
		return refused
	}
	return nil
}

func buildIdentity(options Options) (syncharness.MixedBuildIdentity, error) {
	identity := syncharness.MixedBuildIdentity{PredecessorCommit: syncPredecessorCommit}
	for _, item := range []struct {
		directory, name string
		output          *syncharness.BinaryBuildIdentity
	}{
		{options.CurrentBinDir, "witself-server", &identity.CurrentServer},
		{options.CurrentBinDir, "witself-worker", &identity.CurrentWorker},
		{options.PredecessorBinDir, "witself-server", &identity.PredecessorServer},
		{options.PredecessorBinDir, "witself-worker", &identity.PredecessorWorker},
	} {
		path := filepath.Join(item.directory, item.name)
		info, err := buildinfo.ReadFile(path)
		if err != nil {
			return identity, errors.New("capturemix executable identity unavailable")
		}
		file, err := os.Open(path)
		if err != nil {
			return identity, errors.New("capturemix executable identity unavailable")
		}
		digest := sha256.New()
		_, copyErr := io.Copy(digest, file)
		closeErr := file.Close()
		if copyErr != nil || closeErr != nil {
			return identity, errors.New("capturemix executable identity unavailable")
		}
		*item.output = syncharness.BinaryBuildIdentity{Digest: hex.EncodeToString(digest.Sum(nil)), GoVersion: info.GoVersion}
		for _, setting := range info.Settings {
			switch setting.Key {
			case "GOOS":
				item.output.GOOS = setting.Value
			case "GOARCH":
				item.output.GOARCH = setting.Value
			}
		}
	}
	if identity.CurrentServer.Digest == identity.PredecessorServer.Digest || identity.CurrentWorker.Digest == identity.PredecessorWorker.Digest {
		return identity, errors.New("capturemix executable versions identical")
	}
	return identity, nil
}

func writePrivateJSON(path string, value any) error {
	if os.MkdirAll(filepath.Dir(path), 0700) != nil {
		return errors.New("capturemix artifact directory failed")
	}
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil || os.WriteFile(path, raw, 0600) != nil {
		return errors.New("capturemix artifact write failed")
	}
	return nil
}

// rowBackend makes ordering and failure cleanup independently testable. Each
// observation must finish before the next mutating stage is admitted.
type rowBackend interface {
	start(context.Context) error
	holdLegacy(context.Context) error
	beginInstall(context.Context) error
	observeInstallWait(context.Context) error
	completeLegacy(context.Context, string) error
	finishInstall(context.Context) error
	activate(context.Context) error
	assertWriters(context.Context) error
	assertDowngrade(context.Context) error
	stopProcesses() error
}

// Every Run row owns its fixture cleanup, including failed writer assertions.
// The cleanup callback also lets regression tests inject a row failure without
// starting executables while retaining the real database cleanup path.
func executeRowWithCleanup(ctx context.Context, backend rowBackend, completion string, cleanup func() error) (checks []string, resultErr error) {
	defer func() {
		if err := cleanup(); err != nil {
			resultErr = errors.Join(resultErr, errors.New("capturemix fixture cleanup failed"), err)
		}
	}()
	return executeRow(ctx, backend, completion)
}

func executeRow(ctx context.Context, backend rowBackend, completion string) (checks []string, resultErr error) {
	defer func() {
		if err := backend.stopProcesses(); err != nil {
			resultErr = errors.Join(resultErr, errors.New("capturemix process cleanup failed"), err)
		}
	}()
	steps := []struct {
		name string
		run  func(context.Context) error
	}{
		{"processes_ready", backend.start}, {"legacy_transactions_held", backend.holdLegacy},
		{"installation_started", backend.beginInstall}, {"installation_lock_observed", backend.observeInstallWait},
		{"legacy_" + completion, func(ctx context.Context) error { return backend.completeLegacy(ctx, completion) }},
		{"guards_installed", backend.finishInstall}, {"capture_activated", backend.activate},
		{"writer_assertions", backend.assertWriters}, {"downgrade_refused", backend.assertDowngrade},
	}
	for _, step := range steps {
		if err := step.run(ctx); err != nil {
			return checks, errors.Join(errors.New("capturemix stage failed: "+step.name), err)
		}
		checks = append(checks, step.name)
	}
	return checks, nil
}
