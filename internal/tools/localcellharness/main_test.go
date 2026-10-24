package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
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
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal("cannot make harness test root private")
	}
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
