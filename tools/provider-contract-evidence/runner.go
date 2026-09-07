package main

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"time"
)

type runOptions struct {
	identity                     identity
	output, target, binary, dist string
}
type runDependencies struct {
	command  commandFactory
	checkout func(context.Context, string) error
	artifact func(string, string, cell) (*artifact, error)
	version  func(context.Context, string, *artifact, cell, commandFactory) error
}

func defaultDependencies() runDependencies {
	return runDependencies{exec.CommandContext, checkCheckout, inspectArtifact, probeVersion}
}

func checkCheckout(ctx context.Context, expected string) error {
	for _, entry := range []struct {
		args []string
		want string
	}{
		{[]string{"rev-parse", "HEAD"}, expected + "\n"},
		{[]string{"status", "--porcelain=v1", "--untracked-files=normal"}, ""},
	} {
		child, cancel := context.WithTimeout(ctx, 15*time.Second)
		cmd := exec.CommandContext(child, "git", entry.args...)
		out := &cappedBuffer{limit: 65536}
		cmd.Stdout = out
		cmd.Stderr = io.Discard
		cmd.WaitDelay = 2 * time.Second
		err := cmd.Run()
		cancel()
		if err != nil || out.overflow || out.String() != entry.want {
			return errInvalidEvidence
		}
	}
	return nil
}
func phaseEnvironment(base []string, binary string) []string {
	env := []string{}
	for _, v := range base {
		key, _, _ := strings.Cut(v, "=")
		switch strings.ToUpper(key) {
		case installedEnv, "GOOS", "GOARCH", "GOFLAGS", "GOWORK", "GOENV", "GOEXPERIMENT":
			continue
		}
		env = append(env, v)
	}
	// A clean checkout alone cannot detect source supplied through Go overlays,
	// workspaces, alternate modfiles, tags, or persistent go-env configuration.
	env = append(env, "GOOS="+runtime.GOOS, "GOARCH="+runtime.GOARCH, "GOENV=off", "GOWORK=off", "GOFLAGS=-p=2 -mod=readonly", "GOEXPERIMENT=")
	if binary != "" {
		env = append(env, installedEnv+"="+binary)
	}
	return env
}
func incompletePhase(spec phaseSpec, reason string) phase {
	now := stamp()
	p := phase{Name: spec.name, StartedAt: now, CompletedAt: now, Status: "incomplete", FailureCategory: reason, Tests: append([]testResult(nil), spec.tests...)}
	for index := range p.Tests {
		p.Tests[index].Status = "not_run"
		p.Tests[index].FailureCategory = reason
	}
	return p
}
func runPhase(ctx context.Context, spec phaseSpec, target, binary string, makeCommand commandFactory) phase {
	p := phase{Name: spec.name, StartedAt: stamp()}
	child, cancel := context.WithTimeout(ctx, 12*time.Minute)
	defer cancel()
	stream := newEventStream(spec, target, cancel)
	cmd := makeCommand(child, "go", "test", "-json", "./cmd/witself", "-run", spec.selection, "-count=1")
	cmd.Env = phaseEnvironment(os.Environ(), binary)
	cmd.Stdout = stream
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 2 * time.Second
	err := cmd.Run()
	exit := 0
	if err != nil {
		exit = -1
		if cmd.ProcessState != nil {
			exit = cmd.ProcessState.ExitCode()
		}
	}
	p.ProcessExit = &exit
	p.Tests, p.FailureCategory = stream.finish(exit)
	if child.Err() == context.DeadlineExceeded {
		p.FailureCategory = "process_timeout"
	}
	p.Status = "passed"
	if p.FailureCategory != "none" {
		p.Status = "failed"
	}
	p.CompletedAt = stamp()
	return p
}
func executeRun(ctx context.Context, o runOptions, deps runDependencies) (cell, int) {
	c := cell{SchemaVersion: cellSchema, Identity: o.identity, Target: o.target, GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, StartedAt: stamp(), RuntimeKind: "fixture", VendorVersion: "unobserved", ClientResult: "not_run", ModelResult: "not_run", Phases: []phase{}, Status: "failed", FailureCategory: "source_identity"}
	finish := func(code int) (cell, int) { c.CompletedAt = stamp(); return c, code }
	if !nativeTarget(o.target) {
		c.FailureCategory = "target_mismatch"
		return finish(1)
	}
	if deps.checkout(ctx, o.identity.SourceCommit) != nil {
		return finish(1)
	}
	binary, e := filepath.Abs(o.binary)
	if e != nil {
		c.FailureCategory = "artifact_invalid"
		return finish(1)
	}
	a, artifactError := deps.artifact(o.dist, binary, c)
	if artifactError == nil {
		artifactError = deps.version(ctx, binary, a, c, deps.command)
	}
	if artifactError == nil {
		c.Artifact = a
	}
	firstExit, firstChildExit := 0, 0
	c.Status = "passed"
	c.FailureCategory = "none"
	for _, spec := range specs() {
		installed := ""
		var p phase
		if spec.name == "installed-snapshot" && artifactError != nil {
			p = incompletePhase(spec, "artifact_invalid")
		} else {
			if spec.name == "installed-snapshot" {
				installed = binary
			}
			p = runPhase(ctx, spec, o.target, installed, deps.command)
		}
		c.Phases = append(c.Phases, p)
		if firstChildExit == 0 && p.ProcessExit != nil && *p.ProcessExit > 0 && *p.ProcessExit < 256 {
			firstChildExit = *p.ProcessExit
		}
		if p.Status != "passed" && firstExit == 0 {
			firstExit = 1
			if p.ProcessExit != nil && *p.ProcessExit > 0 && *p.ProcessExit < 256 {
				firstExit = *p.ProcessExit
			}
			c.Status = "failed"
			c.FailureCategory = p.FailureCategory
		}
	}
	if deps.checkout(ctx, o.identity.SourceCommit) != nil && firstExit == 0 {
		firstExit = 1
		c.Status = "failed"
		c.FailureCategory = "source_changed"
	}
	if artifactError == nil {
		after, e := deps.artifact(o.dist, binary, c)
		if e == nil {
			after.BinaryReportedCommit = a.BinaryReportedCommit
			after.BinaryVCSRevision = a.BinaryVCSRevision
		}
		if (e != nil || !reflect.DeepEqual(after, a)) && firstExit == 0 {
			firstExit = 1
			c.Status = "failed"
			c.FailureCategory = "artifact_changed"
		}
	}
	if firstChildExit != 0 {
		return finish(firstChildExit)
	}
	return finish(firstExit)
}
