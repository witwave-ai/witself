package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

const (
	agentEmailArtifactDirectory       = "/private"
	agentEmailArtifactOverrideInput   = "/overrides/overrides.json"
	agentEmailArtifactOverrideName    = "overrides.json"
	agentEmailArtifactExceptionName   = "backfill-exception.json"
	agentEmailArtifactCanaryName      = "primary-canary.json"
	agentEmailArtifactCompletionName  = ".export-complete"
	agentEmailArtifactMaximumBytes    = 1024 * 1024
	agentEmailArtifactHoldMaximumTime = 3 * time.Hour
)

var errAgentEmailArtifactMissing = errors.New("private artifact is absent")

// runAgentEmailArtifactHelper is a deliberately narrow bridge for a
// distroless one-shot Kubernetes Job. It never prints artifact metadata. Only
// the export action writes private bytes, and the supported operator script
// redirects those bytes straight into a private local staging file rather
// than a terminal or a container log.
func runAgentEmailArtifactHelper(args []string) int {
	if len(args) == 1 {
		switch args[0] {
		case "ready":
			// A value-free exec probe for the operator script. Reaching this
			// process proves the distroless holder can accept subsequent execs.
			return 0
		case "hold":
			ctx, stop := signal.NotifyContext(
				context.Background(), syscall.SIGINT, syscall.SIGTERM,
			)
			defer stop()
			if err := holdAgentEmailArtifactExport(
				ctx, agentEmailArtifactDirectory, agentEmailArtifactHoldMaximumTime,
			); err != nil && !errors.Is(err, context.Canceled) {
				fmt.Fprintln(os.Stderr, "witself-server: private artifact holder failed")
				return 1
			}
			return 0
		case "stage-overrides":
			if err := stageAgentEmailArtifactOverrides(
				agentEmailArtifactOverrideInput, agentEmailArtifactDirectory,
			); err != nil {
				fmt.Fprintln(os.Stderr, "witself-server: private override staging failed")
				return 1
			}
			return 0
		case "complete":
			if err := completeAgentEmailArtifactExport(agentEmailArtifactDirectory); err != nil {
				fmt.Fprintln(os.Stderr, "witself-server: private artifact completion failed")
				return 1
			}
			return 0
		}
	}
	if len(args) == 3 && args[1] == "--name" {
		name, ok := agentEmailPrivateArtifactName(args[2])
		if !ok {
			fmt.Fprintln(os.Stderr, "witself-server: invalid private artifact name")
			return 2
		}
		switch args[0] {
		case "exists":
			_, closeArtifact, err := openAgentEmailPrivateArtifact(
				agentEmailArtifactDirectory, name,
			)
			if errors.Is(err, errAgentEmailArtifactMissing) {
				return 3
			}
			if err != nil {
				fmt.Fprintln(os.Stderr, "witself-server: private artifact inspection failed")
				return 1
			}
			if err := closeArtifact(); err != nil {
				fmt.Fprintln(os.Stderr, "witself-server: private artifact inspection failed")
				return 1
			}
			return 0
		case "export":
			artifact, closeArtifact, err := openAgentEmailPrivateArtifact(
				agentEmailArtifactDirectory, name,
			)
			if err != nil {
				fmt.Fprintln(os.Stderr, "witself-server: private artifact export failed")
				return 1
			}
			if _, err := io.Copy(os.Stdout, artifact); err != nil {
				_ = closeArtifact()
				fmt.Fprintln(os.Stderr, "witself-server: private artifact export failed")
				return 1
			}
			if err := closeArtifact(); err != nil {
				fmt.Fprintln(os.Stderr, "witself-server: private artifact export failed")
				return 1
			}
			return 0
		}
	}
	fmt.Fprintln(os.Stderr, "witself-server: invalid private artifact helper command")
	return 2
}

func agentEmailPrivateArtifactName(value string) (string, bool) {
	switch value {
	case "backfill-exception":
		return agentEmailArtifactExceptionName, true
	case "primary-canary":
		return agentEmailArtifactCanaryName, true
	default:
		return "", false
	}
}

func stageAgentEmailArtifactOverrides(inputPath, directory string) error {
	inputInfo, err := os.Stat(inputPath)
	if err != nil || !inputInfo.Mode().IsRegular() || inputInfo.Size() < 2 ||
		inputInfo.Size() > 64*1024 {
		return errors.New("invalid override input")
	}
	input, err := os.Open(inputPath)
	if err != nil {
		return errors.New("open override input")
	}
	defer func() { _ = input.Close() }()
	openedInfo, err := input.Stat()
	if err != nil || !os.SameFile(inputInfo, openedInfo) {
		return errors.New("override input changed while opening")
	}

	outputPath := filepath.Join(directory, agentEmailArtifactOverrideName)
	output, err := os.OpenFile(outputPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errors.New("create staged override")
	}
	fail := func(cause error) error {
		_ = output.Close()
		_ = os.Remove(outputPath)
		return cause
	}
	if err := output.Chmod(0o600); err != nil {
		return fail(errors.New("protect staged override"))
	}
	written, err := io.Copy(output, io.LimitReader(input, 64*1024+1))
	if err != nil || written != inputInfo.Size() || written > 64*1024 {
		return fail(errors.New("copy staged override"))
	}
	if err := output.Sync(); err != nil {
		return fail(errors.New("sync staged override"))
	}
	if err := output.Close(); err != nil {
		_ = os.Remove(outputPath)
		return errors.New("close staged override")
	}
	return nil
}

func openAgentEmailPrivateArtifact(
	directory, name string,
) (*os.File, func() error, error) {
	path := filepath.Join(directory, name)
	pathInfo, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, errAgentEmailArtifactMissing
	}
	if err != nil || !pathInfo.Mode().IsRegular() || pathInfo.Mode().Perm() != 0o600 ||
		pathInfo.Size() < 2 || pathInfo.Size() > agentEmailArtifactMaximumBytes {
		return nil, nil, errors.New("invalid private artifact")
	}
	artifact, err := os.Open(path)
	if err != nil {
		return nil, nil, errors.New("open private artifact")
	}
	openedInfo, err := artifact.Stat()
	if err != nil || !os.SameFile(pathInfo, openedInfo) {
		_ = artifact.Close()
		return nil, nil, errors.New("private artifact changed while opening")
	}
	return artifact, artifact.Close, nil
}

func completeAgentEmailArtifactExport(directory string) error {
	path := filepath.Join(directory, agentEmailArtifactCompletionName)
	marker, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		info, statErr := os.Lstat(path)
		if statErr == nil && info.Mode().IsRegular() && info.Mode().Perm() == 0o600 &&
			info.Size() == 0 {
			return nil
		}
		return errors.New("invalid completion marker")
	}
	if err != nil {
		return errors.New("create completion marker")
	}
	if err := marker.Chmod(0o600); err != nil {
		_ = marker.Close()
		_ = os.Remove(path)
		return errors.New("protect completion marker")
	}
	return marker.Close()
}

func holdAgentEmailArtifactExport(
	ctx context.Context, directory string, maximum time.Duration,
) error {
	if maximum <= 0 {
		return errors.New("invalid hold timeout")
	}
	deadline := time.NewTimer(maximum)
	defer deadline.Stop()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	markerPath := filepath.Join(directory, agentEmailArtifactCompletionName)
	for {
		info, err := os.Lstat(markerPath)
		switch {
		case err == nil:
			if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() != 0 {
				return errors.New("invalid completion marker")
			}
			return nil
		case !errors.Is(err, os.ErrNotExist):
			return errors.New("inspect completion marker")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("private artifact hold timed out")
		case <-ticker.C:
		}
	}
}
