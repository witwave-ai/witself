package store_test

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/store"
	"github.com/witwave-ai/witself/internal/testenv"
	"github.com/witwave-ai/witself/internal/tools/capturemix"
)

// TestCaptureMixMatrix is the single entry point for both role-isolated CI and
// the explicitly weaker shared-database rehearsal. Build output is private: an
// unavailable cached predecessor is reported by its fixed NOT RUN reason only.
func TestCaptureMixMatrix(t *testing.T) {
	// The executable matrix's production target policy deliberately requires
	// the exact shared lane database or the exclusively owned witself cluster.
	// Keep its explicit base target; ordinary schema fixtures use package DBs.
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		dsn = testenv.RequirePostgres(t) // Preserve the shared missing-DSN policy.
	}
	dsn = testenv.GuardPostgresTransactions(t, dsn)
	ctx, cancel := context.WithTimeout(context.Background(), 24*time.Minute)
	defer cancel()
	current := os.Getenv("WITSELF_CAPTUREMIX_CURRENT_BIN_DIR")
	predecessor := os.Getenv("WITSELF_CAPTUREMIX_PREDECESSOR_BIN_DIR")
	commitFile := os.Getenv("WITSELF_CAPTUREMIX_PREDECESSOR_COMMIT_FILE")
	disposable := os.Getenv("WITSELF_TEST_DISPOSABLE_CLUSTER") == "1"
	if current == "" && predecessor == "" && commitFile == "" {
		if disposable {
			t.Fatal("capturemix CI build directories missing")
		}
		_, sourceFile, _, ok := runtime.Caller(0)
		if !ok {
			t.Fatal("capturemix source unavailable")
		}
		source := filepath.Dir(filepath.Dir(filepath.Dir(sourceFile)))
		staging := t.TempDir()
		tree := filepath.Join(staging, "predecessor")
		if err := archivePredecessor(ctx, source, tree); err != nil {
			t.Log("NOT RUN mixed_binaries_roles_unavailable")
			t.Log("NOT RUN proof_profile_database_unavailable")
			t.Skip("NOT RUN predecessor_build_unavailable")
		}
		predecessor = filepath.Join(tree, "bin")
		current = filepath.Join(staging, "current")
		commitFile = filepath.Join(tree, "PREDECESSOR_COMMIT")
		if os.WriteFile(commitFile, []byte(capturemix.PredecessorCommit()+"\n"), 0600) != nil {
			t.Fatal("capturemix predecessor identity write failed")
		}
		if !buildCaptureMixBinaries(ctx, tree, predecessor) {
			t.Log("NOT RUN mixed_binaries_roles_unavailable")
			t.Log("NOT RUN proof_profile_database_unavailable")
			t.Skip("NOT RUN predecessor_build_unavailable")
		}
		if !buildCaptureMixBinaries(ctx, source, current) {
			t.Fatal("capturemix current build unavailable")
		}
	} else if current == "" || predecessor == "" || commitFile == "" {
		t.Fatal("capturemix partial binary configuration refused")
	}
	activate := capturemix.Activate
	if !disposable {
		activate = func(ctx context.Context, st *store.Store, account string) error {
			store.SetSyncPrincipalSingleLoginForTest(st)
			return capturemix.Activate(ctx, st, account)
		}
	}
	artifacts := os.Getenv("WITSELF_CAPTUREMIX_ARTIFACT_DIR")
	if artifacts == "" {
		artifacts = t.TempDir()
	}
	result, err := capturemix.Run(ctx, capturemix.Options{
		CurrentBinDir: current, PredecessorBinDir: predecessor, PredecessorCommit: capturemix.PredecessorCommit(), PredecessorCommitFile: commitFile,
		OwnerDSN: dsn, SuperuserDSN: dsn, DisposableCluster: disposable, Activate: activate, ArtifactDir: artifacts,
	})
	for _, notRun := range result.NotRun {
		t.Log(notRun)
	}
	for _, line := range result.StartRetries {
		t.Log(line)
	}
	for _, row := range result.Rows {
		t.Logf("capturemix row server=%s worker=%s completion=%s checks=%d", row.Server, row.Worker, row.Completion, len(row.Checks))
		for _, evidence := range row.JobEvidence {
			t.Log(evidence.Check())
		}
	}
	if err != nil && !disposable && err.Error() == "capturemix database target refused" {
		// The shared-database rehearsal is limited to the closed local test
		// lanes (127.0.0.1:5599-5601, codex_a..d). A local gate database or
		// any other target is a qualified NOT RUN, never a failure and never
		// a PASS receipt; the role-isolated CI run (disposable) still fails.
		t.Log("NOT RUN mixed_binaries_roles_unavailable")
		t.Log("NOT RUN proof_profile_database_unavailable")
		t.Skip("NOT RUN capturemix_target_outside_closed_lanes")
	}
	if err != nil {
		fatalLines(t, err)
	}
	if len(result.Rows) != 4 {
		t.Fatal("capturemix matrix coverage incomplete")
	}
	t.Log("predecessor build identity recorded")
	if disposable {
		if err := result.VerifyMixedExecution(); err != nil {
			fatalLines(t, err)
		}
	}
}

func buildCaptureMixBinaries(ctx context.Context, source, output string) bool {
	if os.MkdirAll(output, 0700) != nil {
		return false
	}
	for _, name := range []string{"witself-server", "witself-worker"} {
		command := exec.CommandContext(ctx, "go", "build", "-o", filepath.Join(output, name), "./cmd/"+name)
		command.Dir = source
		command.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "GOTOOLCHAIN=local", "GOFLAGS=-mod=mod")
		command.Stdout, command.Stderr = io.Discard, io.Discard
		if command.Run() != nil {
			return false
		}
	}
	return true
}

func archivePredecessor(ctx context.Context, source, destination string) error {
	failure := errors.New("capturemix predecessor archive unavailable")
	command := exec.CommandContext(ctx, "git", "ls-tree", "-r", "--name-only", capturemix.PredecessorCommit())
	command.Dir = source
	command.Stderr = io.Discard
	raw, err := command.Output()
	if err != nil {
		return failure
	}
	args := []string{"archive", capturemix.PredecessorCommit(), "--"}
	for _, name := range strings.Split(string(raw), "\n") {
		// Exclude before archive creation: neither secret bytes nor encrypted
		// files ever enter the archive stream or the temporary checkout.
		if name == "" || name == "secrets" || strings.HasPrefix(name, "secrets/") || strings.HasSuffix(name, ".sops") || strings.HasSuffix(name, ".token") {
			continue
		}
		args = append(args, name)
	}
	if len(args) == 3 || os.MkdirAll(destination, 0700) != nil {
		return failure
	}
	command = exec.CommandContext(ctx, "git", args...)
	command.Dir = source
	command.Stderr = io.Discard
	output, err := command.StdoutPipe()
	if err != nil {
		return failure
	}
	if command.Start() != nil {
		return failure
	}
	reader := tar.NewReader(output)
	for {
		header, readErr := reader.Next()
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
			return failure
		}
		name := filepath.Clean(header.Name)
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			_ = command.Process.Kill()
			_ = command.Wait()
			return failure
		}
		path := filepath.Join(destination, name)
		switch header.Typeflag {
		case tar.TypeXGlobalHeader:
			continue
		case tar.TypeDir:
			err = os.MkdirAll(path, 0700)
		case tar.TypeReg:
			if err = os.MkdirAll(filepath.Dir(path), 0700); err == nil {
				var file *os.File
				file, err = os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(header.Mode)&0700)
				if err == nil {
					_, err = io.Copy(file, reader)
					closeErr := file.Close()
					if err == nil {
						err = closeErr
					}
				}
			}
		default:
			err = failure
		}
		if err != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
			return failure
		}
	}
	if command.Wait() != nil {
		return failure
	}
	return nil
}
