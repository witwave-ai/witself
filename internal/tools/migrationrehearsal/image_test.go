package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/gitopsvalues"
)

// The caller can point this same contract at a temporary tree after applying
// actual roll-cell states. Neither mirror selection nor unrelated server and
// backup pins are frozen into this test.
func TestRehearsalCommittedImagePins(t *testing.T) {
	root := os.Getenv("WITSELF_REHEARSAL_VALUES_ROOT")
	if root == "" {
		root = filepath.Join("..", "..", "..")
	}
	values := filepath.Join(root, ".gitops", "cells", "civo-sandbox-use1-serving", "values.yaml")
	pins, err := gitopsvalues.ReadPostgresImagePins(values)
	if err != nil {
		t.Fatal("read committed PostgreSQL image selection")
	}
	var output bytes.Buffer
	err = run(context.Background(), []string{"--mode", "image", "--values", values}, &output)
	if pins == nil || pins.Digest == "" {
		if err == nil || output.Len() != 0 {
			t.Fatal("missing image pin must fail without returning an image")
		}
		return
	}
	if err != nil {
		t.Fatal("helper rejected committed PostgreSQL image selection")
	}
	var selected struct {
		Image string `json:"image"`
	}
	if err := json.Unmarshal(output.Bytes(), &selected); err != nil {
		t.Fatal("image protocol must contain valid JSON")
	}
	want := pins.Registry + "/" + pins.Repository + "@" + pins.Digest
	if selected.Image != want || !strings.Contains(selected.Image, "@sha256:") {
		t.Fatal("helper did not retain the selected registry, repository, and immutable pin")
	}
}
