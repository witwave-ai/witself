package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAgentEmailArtifactHelperStagesPrivateOverride(t *testing.T) {
	directory := t.TempDir()
	inputTarget := filepath.Join(directory, "secret-data")
	payload := []byte(`{"schema_version":1,"overrides":[{"agent_id":"agent_aaaaaaaaaaaaaaaa","agent_segment":"support"}]}`)
	if err := os.WriteFile(inputTarget, payload, 0o640); err != nil {
		t.Fatal(err)
	}
	inputLink := filepath.Join(directory, "projected-secret-link")
	if err := os.Symlink(inputTarget, inputLink); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	privateDirectory := filepath.Join(directory, "private")
	if err := os.Mkdir(privateDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := stageAgentEmailArtifactOverrides(inputLink, privateDirectory); err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(privateDirectory, agentEmailArtifactOverrideName)
	info, err := os.Lstat(staged)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("staged override mode = %v", info.Mode())
	}
	got, err := os.ReadFile(staged)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatal("staged override bytes changed")
	}
	if err := stageAgentEmailArtifactOverrides(inputLink, privateDirectory); err == nil {
		t.Fatal("staged override was overwritten")
	}
}

func TestAgentEmailArtifactHelperExportsOnlyPrivateRegularArtifacts(t *testing.T) {
	directory := t.TempDir()
	name := agentEmailArtifactCanaryName
	path := filepath.Join(directory, name)
	payload := []byte("{\"schema_version\":2}\n")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	artifact, closeArtifact, err := openAgentEmailPrivateArtifact(directory, name)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(payload))
	if _, err := artifact.Read(got); err != nil {
		t.Fatal(err)
	}
	if err := closeArtifact(); err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatal("exported artifact bytes changed")
	}

	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, _, err := openAgentEmailPrivateArtifact(directory, name); err == nil {
		t.Fatal("group-readable artifact unexpectedly opened")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(directory, "target")
	if err := os.WriteFile(target, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, _, err := openAgentEmailPrivateArtifact(directory, name); err == nil {
		t.Fatal("symlink artifact unexpectedly opened")
	}
	if _, _, err := openAgentEmailPrivateArtifact(directory, agentEmailArtifactExceptionName); !errors.Is(err, errAgentEmailArtifactMissing) {
		t.Fatalf("missing artifact error = %v", err)
	}
}

func TestAgentEmailArtifactHelperCompletionReleasesHolder(t *testing.T) {
	directory := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- holdAgentEmailArtifactExport(ctx, directory, time.Second)
	}()
	if err := completeAgentEmailArtifactExport(directory); err != nil {
		t.Fatal(err)
	}
	if err := completeAgentEmailArtifactExport(directory); err != nil {
		t.Fatalf("idempotent complete = %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("artifact holder did not observe completion")
	}
	markerInfo, err := os.Lstat(filepath.Join(directory, agentEmailArtifactCompletionName))
	if err != nil {
		t.Fatal(err)
	}
	if !markerInfo.Mode().IsRegular() || markerInfo.Mode().Perm() != 0o600 || markerInfo.Size() != 0 {
		t.Fatalf("completion marker = mode %v size %d", markerInfo.Mode(), markerInfo.Size())
	}
}

func TestAgentEmailArtifactHelperNamesAreClosedVocabulary(t *testing.T) {
	if code := runAgentEmailArtifactHelper([]string{"ready"}); code != 0 {
		t.Fatalf("artifact helper ready exit = %d", code)
	}
	for input, want := range map[string]string{
		"backfill-exception": agentEmailArtifactExceptionName,
		"primary-canary":     agentEmailArtifactCanaryName,
	} {
		got, ok := agentEmailPrivateArtifactName(input)
		if !ok || got != want {
			t.Fatalf("artifact name %q = %q / %t", input, got, ok)
		}
	}
	if _, ok := agentEmailPrivateArtifactName("../private"); ok {
		t.Fatal("path-like artifact name unexpectedly accepted")
	}
}
