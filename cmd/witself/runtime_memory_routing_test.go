package main

import (
	"bytes"
	"os"
	"testing"

	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

func TestRuntimeMemoryRoutingCurrentDetectsMissingAndStaleManagedBlock(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())

	current, err := runtimeMemoryRoutingCurrentAt(transcriptcapture.RuntimeCodex, "")
	if err != nil {
		t.Fatalf("inspect missing routing: %v", err)
	}
	if current {
		t.Fatal("missing routing reported current")
	}

	installed, err := installRuntimeMemoryRoutingInstructions(transcriptcapture.RuntimeCodex)
	if err != nil {
		t.Fatalf("install routing: %v", err)
	}
	current, err = runtimeMemoryRoutingCurrentAt(transcriptcapture.RuntimeCodex, "")
	if err != nil {
		t.Fatalf("inspect installed routing: %v", err)
	}
	if !current {
		t.Fatal("installed routing reported stale")
	}

	raw, err := os.ReadFile(installed.path)
	if err != nil {
		t.Fatalf("read installed routing: %v", err)
	}
	stale := bytes.Replace(
		raw,
		[]byte("Witself facts and Codex memory"),
		[]byte("Witself facts and Codex memories"),
		1,
	)
	if bytes.Equal(stale, raw) {
		t.Fatal("installed routing did not contain expected managed content")
	}
	if err := os.WriteFile(installed.path, stale, 0o600); err != nil {
		t.Fatalf("corrupt installed routing: %v", err)
	}
	current, err = runtimeMemoryRoutingCurrentAt(transcriptcapture.RuntimeCodex, "")
	if err != nil {
		t.Fatalf("inspect stale routing: %v", err)
	}
	if current {
		t.Fatal("stale routing reported current")
	}
}
