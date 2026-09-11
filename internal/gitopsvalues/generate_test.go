package gitopsvalues

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestCommittedValuesMatchGenerator(t *testing.T) {
	root := repoRoot(t)
	generated, err := generateAll(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(generated) != 9 {
		t.Fatalf("generated %d cells, want 9", len(generated))
	}
	for _, cell := range sortedCells(generated) {
		path := filepath.Join(root, filepath.FromSlash(valuesRel(cell)))
		committed, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if !bytes.Equal(committed, generated[cell]) {
			t.Errorf("%s differs from generated output; run scripts/gitops-cell-values.sh --check", valuesRel(cell))
		}
	}
}

func TestCheckPassesOnCommittedTree(t *testing.T) {
	root := repoRoot(t)
	var buf bytes.Buffer
	if err := Check(root, &buf); err != nil {
		t.Fatalf("check: %v\n%s", err, buf.String())
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find go.mod walking upward")
		}
		dir = parent
	}
}
