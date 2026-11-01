package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/localcell"
)

func TestRejectsInvalidArgumentsBeforeMutation(t *testing.T) {
	for _, args := range [][]string{
		{}, {"-operation", "unknown"}, {"-operation", "start", "-root", "relative"},
		{"-operation", "start", "-root", "/unused", "-purge"},
		{"-operation", "remove", "-root", "/unused", "-leave-running"},
		{"-operation", "install", "-root", "/unused", "-interrupt-after-postgres"},
		{"-operation", "inspect", "-root", "/unused", "-timeout", "0s"},
		{"-operation", "inspect", "-root", "/unused", "unexpected"},
	} {
		if err := run(context.Background(), args, io.Discard); err == nil {
			t.Fatal("invalid harness invocation succeeded")
		}
	}
}

func TestInspectDoesNotBuildComponents(t *testing.T) {
	root := privateHarnessRoot(t)
	var output bytes.Buffer
	if err := run(context.Background(), []string{"-operation", "inspect", "-root", root, "-source", "/not-a-source-tree"}, &output); err != nil {
		t.Fatalf("inspect failed: %v", err)
	}
	var inspection localcell.Inspection
	if err := json.Unmarshal(output.Bytes(), &inspection); err != nil || inspection.Manifest != nil || inspection.PGDataPresent {
		t.Fatal("inspect did not encode the empty cell state")
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatal("inspect mutated the empty cell root")
	}
}

func TestBuildBinariesPreservesExplicitPaths(t *testing.T) {
	binaries := localcell.Binaries{Server: "/explicit/server", Worker: "/explicit/worker"}
	if err := buildBinaries(context.Background(), "/not-a-source-tree", t.TempDir(), &binaries); err != nil {
		t.Fatal("explicit components unexpectedly triggered a build")
	}
	if binaries.Server != "/explicit/server" || binaries.Worker != "/explicit/worker" {
		t.Fatal("explicit component paths changed")
	}
}

func TestBuildFailureIsValueFree(t *testing.T) {
	var binaries localcell.Binaries
	err := buildBinaries(context.Background(), filepath.Join(t.TempDir(), "private-source-marker"), t.TempDir(), &binaries)
	if err == nil || strings.Contains(err.Error(), "private-source-marker") {
		t.Fatal("build failure was missing or exposed the source path")
	}
}

func TestBuildBinariesUseNativeExecutableNames(t *testing.T) {
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "go.mod"), []byte("module fixture\n\ngo 1.24\n"), 0o600); err != nil {
		t.Fatal("cannot prepare build fixture")
	}
	for _, name := range []string{"witself-server", "witself-worker"} {
		directory := filepath.Join(source, "cmd", name)
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal("cannot prepare component fixture")
		}
		if err := os.WriteFile(filepath.Join(directory, "main.go"), []byte("package main\nfunc main() {}\n"), 0o600); err != nil {
			t.Fatal("cannot write component fixture")
		}
	}
	directory := privateHarnessRoot(t)
	var binaries localcell.Binaries
	if err := buildBinaries(t.Context(), source, directory, &binaries); err != nil {
		t.Fatal(err)
	}
	for index, path := range []string{binaries.Server, binaries.Worker} {
		want := []string{"witself-server", "witself-worker"}[index]
		if runtime.GOOS == "windows" {
			want += ".exe"
		}
		if filepath.Base(path) != want {
			t.Fatal("component build omitted native executable suffix")
		}
		command := exec.CommandContext(t.Context(), path)
		if err := command.Run(); err != nil {
			t.Fatal("component build is not executable")
		}
	}
}

func TestAutobuildSurvivesCommandsUntilPurge(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	root := privateHarnessRoot(t)
	tools := t.TempDir()
	fixture := filepath.Join(tools, harnessExecutable("fixture"))
	build := exec.CommandContext(t.Context(), "go", "build", "-o", fixture, "../../localcell/testdata/lifecyclefixture")
	build.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "GOTOOLCHAIN=local")
	if err := build.Run(); err != nil {
		t.Fatal("cannot build lifecycle fixture")
	}
	for _, name := range []string{"postgres", "initdb", "pg_ctl", "createdb", "psql"} {
		if err := os.Link(fixture, filepath.Join(tools, harnessExecutable(name))); err != nil {
			t.Fatal("cannot prepare fixture tool")
		}
	}
	var built []string
	builder := func(_ context.Context, _, directory string, binaries *localcell.Binaries) error {
		for _, component := range []struct {
			name string
			path *string
		}{{"witself-server", &binaries.Server}, {"witself-worker", &binaries.Worker}} {
			path := filepath.Join(directory, harnessExecutable(component.name))
			if err := publishHarnessBinary(fixture, path); err != nil {
				return err
			}
			*component.path = path
			built = append(built, path)
		}
		return nil
	}
	invoke := func(operation string, extra ...string) error {
		args := []string{"-operation", operation, "-root", root, "-pgbin", tools, "-timeout", "30s"}
		return runWithBuilder(context.Background(), append(args, extra...), io.Discard, builder)
	}
	t.Cleanup(func() {
		if err := invoke("repair"); err != nil {
			t.Error("cannot repair harness fixture during cleanup")
			return
		}
		if err := invoke("remove", "-purge"); err != nil {
			t.Error("cannot purge harness fixture during cleanup")
		}
	})
	var witnesses []harnessProcessWitness
	for _, operation := range []string{"install", "start", "status", "stop", "start", "remove"} {
		if err := invoke(operation); err != nil {
			t.Fatalf("harness operation failed: %s: %v", operation, err)
		}
		if operation == "start" {
			// Keep both generations before Stop or Remove erases their records.
			// The final Remove starts with all three components still running.
			for _, component := range []string{"postgres", "server", "worker"} {
				witness := newHarnessProcessWitness(t, root, component)
				if !witness.alive(t) {
					t.Fatalf("harness cleanup witness did not observe the running component: %s", component)
				}
				witnesses = append(witnesses, witness)
			}
		}
		for _, path := range built {
			if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
				t.Fatal("autobuilt component disappeared after command return")
			}
		}
	}
	if _, err := os.Stat(filepath.Join(root, "run")); !os.IsNotExist(err) {
		t.Fatal("non-purge removal retained runtime records")
	}
	for _, witness := range witnesses {
		if witness.alive(t) {
			t.Errorf("harness process survived removal: %s", witness.record.Component)
		}
	}
	if err := invoke("remove", "-purge"); err != nil {
		t.Fatalf("harness purge failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".harness-build")); !os.IsNotExist(err) {
		t.Fatal("purge retained autobuilt components")
	}
}

type harnessProcessWitness struct {
	record localcell.ProcessRecord
	cell   *localcell.Cell
}

func newHarnessProcessWitness(t *testing.T, root, component string) harnessProcessWitness {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "run", component+".json"))
	if err != nil {
		t.Fatal("cannot retain harness process cleanup identity")
	}
	var record localcell.ProcessRecord
	if json.Unmarshal(data, &record) != nil || record.Component != component || record.PID <= 1 || record.StartTime == "" {
		t.Fatal("invalid harness process cleanup identity")
	}
	// localcell's test-only RecordAliveForTest export is unavailable in this
	// package. Inspect uses the same native PID/creation-identity witness. Keep
	// a private observation-only record outside the cell being removed; use the
	// server slot for every component to avoid PostgreSQL's postmaster.pid gate.
	probeRoot, err := filepath.EvalSymlinks(privateHarnessRoot(t))
	if err != nil || harnessPrivateDirectory(filepath.Join(probeRoot, "run")) != nil {
		t.Fatal("cannot prepare harness cleanup witness")
	}
	probe := record
	probe.Root, probe.Component = probeRoot, "server"
	file, err := harnessCreateFile(filepath.Join(probeRoot, "run", "server.json"))
	if err != nil {
		t.Fatal("cannot create harness cleanup witness")
	}
	encodeErr := json.NewEncoder(file).Encode(probe)
	closeErr := file.Close()
	if encodeErr != nil || closeErr != nil {
		t.Fatal("cannot retain harness cleanup witness")
	}
	cell, err := localcell.New(localcell.Config{Root: probeRoot})
	if err != nil {
		t.Fatal("cannot construct harness cleanup witness")
	}
	return harnessProcessWitness{record: record, cell: cell}
}

func (w harnessProcessWitness) alive(t *testing.T) bool {
	t.Helper()
	inspection, err := w.cell.Inspect(t.Context())
	if err != nil {
		t.Fatal("cannot verify harness cleanup identity")
	}
	state, ok := inspection.Processes["server"]
	if !ok || state.PID != w.record.PID {
		t.Fatal("harness cleanup witness lost its process identity")
	}
	return state.Alive && state.Matches
}
