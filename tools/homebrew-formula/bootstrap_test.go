package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"text/template"
	"time"

	homebrewtemplate "github.com/witwave-ai/witself/packaging/homebrew"
)

// The fixture is the exact pre-B2 template. Comparing all three products through
// the same renderer proves the opt-in cannot alter currently published formulae.
func TestRemoteOnlyFormulaBytesUnchanged(t *testing.T) {
	t.Parallel()
	baseline, err := os.ReadFile("testdata/remote-only.rb.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	dist := t.TempDir()
	writeFixtureArchives(t, dist, "1.2.3", "")
	before, after := t.TempDir(), t.TempDir()
	if err := renderFormulas("1.2.3", dist, before, string(baseline)); err != nil {
		t.Fatal(err)
	}
	if err := renderFormulas("1.2.3", dist, after, homebrewtemplate.FormulaTemplate); err != nil {
		t.Fatal(err)
	}
	for _, product := range products {
		if product.LocalCell {
			t.Fatal("a current product opts into local-cell behavior")
		}
		oldContent, err := os.ReadFile(filepath.Join(before, "Formula", product.FormulaName+".rb"))
		if err != nil {
			t.Fatal(err)
		}
		newContent, err := os.ReadFile(filepath.Join(after, "Formula", product.FormulaName+".rb"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(oldContent, newContent) {
			t.Errorf("%s default rendered bytes changed", product.FormulaName)
		}
	}
}

func TestLocalCellFormulaRubyVectorParity(t *testing.T) {
	ruby, err := exec.LookPath("ruby")
	if err != nil {
		if os.Getenv("WITSELF_TEST_REQUIRE_BOOTSTRAP_PARITY") == "1" {
			t.Fatal("NOT_RUN ruby_unavailable")
		}
		t.Skip("NOT_RUN ruby_unavailable")
	}
	dist := t.TempDir()
	writeFixtureArchives(t, dist, "1.2.3", "")
	candidate := products[0]
	candidate.LocalCell = true
	data, err := loadFormulaData("1.2.3", dist, candidate)
	if err != nil {
		t.Fatal(err)
	}
	tmpl, err := template.New("formula").Option("missingkey=error").Parse(homebrewtemplate.FormulaTemplate)
	if err != nil {
		t.Fatal(err)
	}
	var rendered bytes.Buffer
	if err := tmpl.Execute(&rendered, data); err != nil {
		t.Fatal(err)
	}
	if strings.Count(rendered.String(), `depends_on "openssl@3"`) != 1 ||
		!strings.Contains(rendered.String(), "def verify_component_set!(dir, root_pem)") {
		t.Fatal("opted-in formula missing trusted verifier dependency or private helper")
	}
	formulaPath := filepath.Join(t.TempDir(), "witself.rb")
	if err := os.WriteFile(formulaPath, rendered.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	vectors, err := filepath.Abs("../../internal/localcell/trust/testdata")
	if err != nil {
		t.Fatal(err)
	}
	archivePath := writeBootstrapAppArchive(t)
	cmd := exec.Command(ruby, "testdata/bootstrap_parity.rb", formulaPath, vectors, archivePath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Ruby parity failed: %s", output)
	}
	t.Log(strings.TrimSpace(string(output)))
}

// Reproduce the public build-vector recipe without using expected digest bytes.
func writeBootstrapAppArchive(t *testing.T) string {
	t.Helper()
	var raw bytes.Buffer
	gz, err := gzip.NewWriterLevel(&raw, gzip.NoCompression)
	if err != nil {
		t.Fatal(err)
	}
	gz.Header = gzip.Header{ModTime: time.Unix(0, 0).UTC(), OS: 255}
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "bin/app", Mode: 0o755, Size: 4, Typeflag: tar.TypeReg,
		Format: tar.FormatUSTAR, ModTime: time.Unix(0, 0).UTC()}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("data")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "app.tar.gz")
	if err := os.WriteFile(path, raw.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
