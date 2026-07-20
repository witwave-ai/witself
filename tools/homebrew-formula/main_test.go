package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	homebrewtemplate "github.com/witwave-ai/witself/packaging/homebrew"
)

func TestRenderFormulasHashesAndContent(t *testing.T) {
	t.Parallel()

	const version = "1.2.3-rc.1"
	dist := t.TempDir()
	output := filepath.Join(t.TempDir(), "rendered")
	formulaDir := filepath.Join(output, "Formula")
	expectedHashes := writeFixtureArchives(t, dist, version, "")

	if err := run([]string{"--version", version, "--dist", dist, "--output", output}); err != nil {
		t.Fatalf("run() error = %v", err)
	}

	expectedFormulae := map[string]struct {
		binary      string
		description string
		testArg     string
	}{
		"witself.rb": {
			binary:      "witself",
			description: "Identity, memory, messaging, and secrets CLI for autonomous AI agents",
			testArg:     "version",
		},
		"witself-infra.rb": {
			binary:      "witself-infra",
			description: "Cell infrastructure provisioner using the Pulumi Automation API",
			testArg:     "help",
		},
		"witself-admin.rb": {
			binary:      "witself-admin",
			description: "Fleet administration CLI for the Witself control plane",
			testArg:     "version",
		},
	}

	firstRender := make(map[string][]byte, len(expectedFormulae))
	for filename, expected := range expectedFormulae {
		path := filepath.Join(formulaDir, filename)
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", filename, err)
		}
		firstRender[filename] = content
		text := string(content)

		assertContains(t, text, fmt.Sprintf("desc %q", expected.description))
		assertContains(t, text, `version "1.2.3-rc.1"`)
		assertContains(t, text, fmt.Sprintf(`system bin/%q, %q`, expected.binary, expected.testArg))
		if strings.Contains(text, `#{bin}`) {
			t.Errorf("%s contains deprecated interpolated bin path", filename)
		}

		for _, target := range []string{"darwin_amd64", "darwin_arm64", "linux_amd64", "linux_arm64"} {
			archiveName := fmt.Sprintf("%s_%s_%s.tar.gz", expected.binary, version, target)
			assertContains(t, text, "/v"+version+"/"+archiveName)
			assertContains(t, text, expectedHashes[archiveName])
		}

		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", filename, err)
		}
		if got := info.Mode().Perm(); got != formulaFileMode {
			t.Errorf("%s mode = %o, want %o", filename, got, formulaFileMode)
		}
	}

	witself := string(firstRender["witself.rb"])
	assertContains(t, witself, `bin.install_symlink "witself" => "ws"`)
	if strings.Contains(witself, `depends_on "pulumi"`) {
		t.Error("witself formula unexpectedly depends on pulumi")
	}

	infra := string(firstRender["witself-infra.rb"])
	assertContains(t, infra, `depends_on "pulumi"`)
	if strings.Contains(infra, "install_symlink") {
		t.Error("witself-infra formula unexpectedly installs an alias")
	}

	admin := string(firstRender["witself-admin.rb"])
	if strings.Contains(admin, `depends_on "pulumi"`) || strings.Contains(admin, "install_symlink") {
		t.Error("witself-admin formula contains another product's install behavior")
	}

	if err := renderFormulas(version, dist, output, homebrewtemplate.FormulaTemplate); err != nil {
		t.Fatalf("second renderFormulas() error = %v", err)
	}
	for filename, first := range firstRender {
		second, err := os.ReadFile(filepath.Join(formulaDir, filename))
		if err != nil {
			t.Fatalf("read second %s: %v", filename, err)
		}
		if string(second) != string(first) {
			t.Errorf("%s changed across identical renders", filename)
		}
	}
}

func TestRenderFormulasMissingArchiveWritesNothing(t *testing.T) {
	t.Parallel()

	const version = "2.0.0"
	dist := t.TempDir()
	output := filepath.Join(t.TempDir(), "Formula")
	missing := "witself-infra_2.0.0_linux_arm64.tar.gz"
	writeFixtureArchives(t, dist, version, missing)

	err := renderFormulas(version, dist, output, homebrewtemplate.FormulaTemplate)
	if err == nil {
		t.Fatal("renderFormulas() error = nil, want missing archive error")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Fatalf("renderFormulas() error = %q, want missing archive %q", err, missing)
	}
	if _, statErr := os.Stat(output); !os.IsNotExist(statErr) {
		t.Fatalf("output directory stat error = %v, want not exist", statErr)
	}
}

func TestValidateVersion(t *testing.T) {
	t.Parallel()

	for _, version := range []string{"0.0.1", "1.2.3", "1.2.3-rc.1", "1.2.3+build.7", "1.2.3-rc.1+build.7"} {
		version := version
		t.Run("valid_"+version, func(t *testing.T) {
			t.Parallel()
			if err := validateVersion(version); err != nil {
				t.Errorf("validateVersion(%q) error = %v", version, err)
			}
		})
	}

	for _, version := range []string{"", "v1.2.3", "1.2", "01.2.3", "1.02.3", "1.2.03", "1.2.3/other", "1.2.3 rc1"} {
		version := version
		t.Run("invalid_"+version, func(t *testing.T) {
			t.Parallel()
			if err := validateVersion(version); err == nil {
				t.Errorf("validateVersion(%q) error = nil, want error", version)
			}
		})
	}
}

func TestValidateProduct(t *testing.T) {
	t.Parallel()

	for _, product := range products {
		if err := validateProduct(product); err != nil {
			t.Errorf("validateProduct(%q) error = %v", product.FormulaName, err)
		}
	}

	base := products[0]
	tests := []struct {
		name    string
		mutate  func(*product)
		wantErr string
	}{
		{name: "formula name", mutate: func(product *product) { product.FormulaName = "../witself" }, wantErr: "invalid formula name"},
		{name: "class", mutate: func(product *product) { product.ClassName = "witself" }, wantErr: "invalid formula class"},
		{name: "binary", mutate: func(product *product) { product.Binary = "witself/admin" }, wantErr: "invalid formula binary"},
		{name: "description empty", mutate: func(product *product) { product.Description = " " }, wantErr: "invalid description"},
		{name: "description quote", mutate: func(product *product) { product.Description = `Unsafe "description"` }, wantErr: "invalid description"},
		{name: "description begins name", mutate: func(product *product) { product.Description = "Witself command line" }, wantErr: "must not begin"},
		{name: "alias", mutate: func(product *product) { product.Alias = "../ws" }, wantErr: "invalid alias"},
		{name: "dependency", mutate: func(product *product) { product.Dependency = "pulumi/latest" }, wantErr: "invalid dependency"},
		{name: "test", mutate: func(product *product) { product.TestArgument = "--version" }, wantErr: "invalid test"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			candidate := base
			test.mutate(&candidate)
			err := validateProduct(candidate)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("validateProduct() error = %v, want containing %q", err, test.wantErr)
			}
		})
	}
}

func TestRunValidatesRequiredPaths(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "version", args: nil, wantErr: "--version is required"},
		{name: "dist", args: []string{"--version", "1.2.3"}, wantErr: "--dist is required"},
		{name: "output", args: []string{"--version", "1.2.3", "--dist", t.TempDir()}, wantErr: "--output is required"},
		{name: "positional", args: []string{"--version", "1.2.3", "--dist", t.TempDir(), "--output", t.TempDir(), "extra"}, wantErr: "unexpected positional arguments"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := run(test.args)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("run() error = %v, want containing %q", err, test.wantErr)
			}
		})
	}
}

func TestRenderFormulasRejectsInvalidDirectoriesAndTemplate(t *testing.T) {
	t.Parallel()

	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		dist     string
		output   string
		template string
		wantErr  string
	}{
		{name: "dist missing", dist: filepath.Join(t.TempDir(), "missing"), output: t.TempDir(), template: homebrewtemplate.FormulaTemplate, wantErr: "dist directory"},
		{name: "dist file", dist: file, output: t.TempDir(), template: homebrewtemplate.FormulaTemplate, wantErr: "dist path"},
		{name: "output file", dist: t.TempDir(), output: file, template: homebrewtemplate.FormulaTemplate, wantErr: "output path"},
		{name: "template", dist: t.TempDir(), output: t.TempDir(), template: "{{", wantErr: "parse formula template"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := renderFormulas("1.2.3", test.dist, test.output, test.template)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("renderFormulas() error = %v, want containing %q", err, test.wantErr)
			}
		})
	}
}

func writeFixtureArchives(t *testing.T, dist, version, skip string) map[string]string {
	t.Helper()

	hashes := make(map[string]string)
	for _, product := range products {
		for _, target := range []struct {
			goos   string
			goarch string
		}{
			{goos: "darwin", goarch: "amd64"},
			{goos: "darwin", goarch: "arm64"},
			{goos: "linux", goarch: "amd64"},
			{goos: "linux", goarch: "arm64"},
		} {
			name := fmt.Sprintf("%s_%s_%s_%s.tar.gz", product.Binary, version, target.goos, target.goarch)
			if name == skip {
				continue
			}
			content := []byte("fixture:" + name + "\n")
			if err := os.WriteFile(filepath.Join(dist, name), content, 0o600); err != nil {
				t.Fatalf("write %s: %v", name, err)
			}
			digest := sha256.Sum256(content)
			hashes[name] = fmt.Sprintf("%x", digest)
		}
	}
	return hashes
}

func assertContains(t *testing.T, text, want string) {
	t.Helper()
	if !strings.Contains(text, want) {
		t.Errorf("rendered formula missing %q", want)
	}
}
