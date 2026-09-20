package gitopscheck

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/gitopsvalues"
)

// Digest pins are optional for existing tag-only cells, but a malformed pin
// must not become an unusable image reference in a later deployment.
func TestCellServerImageDigestsValid(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join(repoRoot(t), ".gitops", "cells", "*", "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("no cell values files found")
	}
	for _, path := range paths {
		t.Run(filepath.Base(filepath.Dir(path)), func(t *testing.T) {
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := gitopsvalues.ValidateServerImageDigest(body); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestServerImageDigestValidation(t *testing.T) {
	valid := "sha256:" + strings.Repeat("a", 64)
	cases := []struct {
		name    string
		field   string
		wantErr bool
	}{
		{name: "legacy tag without digest"},
		{name: "explicit empty digest", field: "imageDigest: \"\""},
		{name: "valid digest", field: "imageDigest: " + valid},
		{name: "quoted valid digest", field: "imageDigest: \"" + valid + "\""},
		{name: "wrong algorithm", field: "imageDigest: sha512:" + strings.Repeat("a", 64), wantErr: true},
		{name: "short digest", field: "imageDigest: sha256:" + strings.Repeat("a", 63), wantErr: true},
		{name: "long digest", field: "imageDigest: sha256:" + strings.Repeat("a", 65), wantErr: true},
		{name: "nonhex digest", field: "imageDigest: sha256:" + strings.Repeat("g", 64), wantErr: true},
		{name: "uppercase digest", field: "imageDigest: sha256:" + strings.Repeat("A", 64), wantErr: true},
		{name: "trailing whitespace", field: "imageDigest: \"" + valid + " \"", wantErr: true},
		{name: "null digest", field: "imageDigest: null", wantErr: true},
		{name: "boolean digest", field: "imageDigest: true", wantErr: true},
		{name: "numeric digest", field: "imageDigest: 12", wantErr: true},
		{name: "list digest", field: "imageDigest: []", wantErr: true},
		{name: "mapping digest", field: "imageDigest: {}", wantErr: true},
		{name: "duplicate digest", field: "imageDigest: \"\"\n    imageDigest: \"\"", wantErr: true},
		{name: "malformed YAML", field: "imageDigest: [", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte("apps:\n  witselfServer:\n    imageTag: 0.0.289\n    " + tc.field + "\n")
			err := gitopsvalues.ValidateServerImageDigest(body)
			if (err != nil) != tc.wantErr {
				t.Fatalf("digest validation error = %v, want error %t", err, tc.wantErr)
			}
		})
	}
}
