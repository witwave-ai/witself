//go:build !windows

package trust

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

var handoffHostBuild struct {
	once sync.Once
	body []byte
	err  error
}

func handoffFixtureHost(t *testing.T) []byte {
	t.Helper()
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("NOT_RUN go_unavailable_for_process_fixture")
	}
	handoffHostBuild.once.Do(func() {
		dir := t.TempDir()
		source := filepath.Join(dir, "fixture.go")
		binary := filepath.Join(dir, "fixture-host")
		program := "package main\nimport (\"fmt\"; \"time\")\nfunc main() { fmt.Println(\"ready\"); time.Sleep(time.Minute) }\n"
		if err := os.WriteFile(source, []byte(program), 0600); err != nil {
			handoffHostBuild.err = errors.New("fixture_source_write_failed")
			return
		}
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, goTool, "build", "-o", binary, source)
		if err := cmd.Run(); err != nil {
			handoffHostBuild.err = errors.New("fixture_host_build_failed_or_timed_out")
			return
		}
		handoffHostBuild.body, err = os.ReadFile(binary)
		if err != nil {
			handoffHostBuild.err = errors.New("fixture_host_read_failed")
		}
	})
	if handoffHostBuild.err != nil {
		t.Fatal(handoffHostBuild.err)
	}
	return handoffHostBuild.body
}

func TestRetireRunningFixtureHostIdentity(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("NOT_RUN process_executable_identity_unavailable_on_host")
	}
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	body := handoffFixtureHost(t)
	f := payloadInput(t)
	manifest, err := ParseComponentSet(f.set.Document)
	if err != nil {
		t.Fatal("fixture manifest parse failed")
	}
	manifest.ReleaseSequence = 2
	manifest.Platforms[0].Arch = runtime.GOARCH
	f.v.options.Arch = runtime.GOARCH
	foundHost := false
	for i := range manifest.Platforms[0].Components {
		component := &manifest.Platforms[0].Components[i]
		if component.Role != "service_host" {
			continue
		}
		foundHost = true
		entry := &component.Inventory[0]
		entry.Size, entry.SHA256 = uint64(len(body)), digestBytes(body)
		archive := payloadArchive(t, entry.Path, body)
		component.Archive.Size, component.Archive.SHA256 = uint64(len(archive)), digestBytes(archive)
		if os.WriteFile(f.archives[component.Role], archive, 0600) != nil {
			t.Fatal("fixture host archive write failed")
		}
	}
	if !foundHost {
		t.Fatal("fixture lacks host component")
	}
	release, _ := verifierFixtureKey(t, "release-1")
	f.set.Document, f.set.Envelope = verifierSign(t, manifest, "component_set", release)
	manifest, err = ParseComponentSet(f.set.Document)
	if err != nil {
		t.Fatal("signed fixture manifest parse failed")
	}
	f.components = manifest.Platforms[0].Components
	if out := f.v.AcceptSet(f.set.Document, f.set.Envelope, f.components); !out.Accepted {
		t.Fatalf("fixture host acceptance: %s", out.Reason)
	}
	first, err := StageGeneration(t.Context(), f.v, f.state, f.set, f.components, f.archives, f.root)
	if err != nil {
		t.Fatalf("fixture host stage: %s", reasonOf(err))
	}
	payload, receipt, err := Activate(t.Context(), f.root, first.Name, f.clock)
	if err != nil || VerifyPayloadIdentity(payload, receipt) != nil {
		t.Fatal("fixture host activation failed")
	}
	child := exec.CommandContext(t.Context(), payload.Host, payload.Args...)
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal("fixture host pipe failed")
	}
	if err := child.Start(); err != nil {
		t.Fatal("fixture host start failed")
	}
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_ = child.Wait()
	})
	ready := make(chan bool, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		ready <- scanner.Scan() && scanner.Text() == "ready"
	}()
	select {
	case ok := <-ready:
		if !ok {
			t.Fatal("fixture host did not report ready")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fixture host readiness timeout")
	}
	f = payloadNext(t, f, 3)
	second, err := StageGeneration(t.Context(), f.v, f.state, f.set, f.components, f.archives, f.root)
	if err != nil {
		t.Fatalf("next fixture host stage: %s", reasonOf(err))
	}
	if _, _, err := Activate(t.Context(), f.root, second.Name, f.clock); err != nil {
		t.Fatalf("next fixture activation: %s", reasonOf(err))
	}
	journal, err := loadHandoff(f.root)
	if err != nil || journal.Generations[first.Name].State != "superseded" {
		t.Fatal("running generation not retained as superseded")
	}
	if err := Retire(t.Context(), f.root, first.Name); !errors.Is(err, ReasonGenerationReferenced) {
		t.Fatalf("running executable identity not protected: %s", reasonOf(err))
	}
	if _, err := LoadGeneration(f.root, first.Name); err != nil {
		t.Fatal("retirement refusal damaged running generation")
	}
}
