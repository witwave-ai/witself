package gitopsvalues

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
)

// Check regenerates every cell values file and reports a unified diff when
// the committed file differs. It never writes the live GitOps inputs.
func Check(root string, out io.Writer) error {
	generated, err := generateAll(root)
	if err != nil {
		return err
	}
	var drifted []string
	for _, cell := range sortedCells(generated) {
		rel := valuesRel(cell)
		path := filepath.Join(root, filepath.FromSlash(rel))
		committed, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				_, _ = fmt.Fprintf(out, "gitops cell values: missing %s\n", rel)
				drifted = append(drifted, cell)
				continue
			}
			return err
		}
		if bytes.Equal(committed, generated[cell]) {
			continue
		}
		drifted = append(drifted, cell)
		if err := writeUnifiedDiff(out, rel, committed, generated[cell]); err != nil {
			return err
		}
	}
	if len(drifted) > 0 {
		return fmt.Errorf("gitops cell values: %d file(s) differ from generated output", len(drifted))
	}
	_, _ = fmt.Fprintf(out, "gitops cell values: %d files match\n", len(generated))
	return nil
}

// Write regenerates every cell values file and replaces committed files that
// differ. Files that already match are left untouched.
func Write(root string, out io.Writer) error {
	generated, err := generateAll(root)
	if err != nil {
		return err
	}
	wrote, skipped := 0, 0
	for _, cell := range sortedCells(generated) {
		rel := valuesRel(cell)
		path := filepath.Join(root, filepath.FromSlash(rel))
		committed, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil && bytes.Equal(committed, generated[cell]) {
			skipped++
			continue
		}
		if err := writeFileAtomic(path, generated[cell]); err != nil {
			return fmt.Errorf("write %s: %w", rel, err)
		}
		wrote++
		_, _ = fmt.Fprintf(out, "gitops cell values: wrote %s\n", rel)
	}
	_, _ = fmt.Fprintf(out, "gitops cell values: wrote %d files, skipped %d unchanged\n", wrote, skipped)
	return nil
}

func sortedCells(generated map[string][]byte) []string {
	names := make([]string, 0, len(generated))
	for name := range generated {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func writeUnifiedDiff(out io.Writer, rel string, committed, generated []byte) error {
	dir, err := os.MkdirTemp("", "gitops-cell-values-diff-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	a := filepath.Join(dir, "committed")
	b := filepath.Join(dir, "generated")
	if err := os.WriteFile(a, committed, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(b, generated, 0o600); err != nil {
		return err
	}
	cmd := exec.Command("diff", "-u", "-L", rel+" (committed)", "-L", rel+" (generated)", a, b)
	cmd.Stdout = out
	cmd.Stderr = out
	err = cmd.Run()
	if err == nil {
		return nil
	}
	if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
		return nil
	}
	return fmt.Errorf("diff %s: %w", rel, err)
}

func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".values.yaml.*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
