// Package main renders deterministic Homebrew formulae from release archives.
package main

import (
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"

	homebrewtemplate "github.com/witwave-ai/witself/packaging/homebrew"
)

const (
	formulaFileMode = 0o644
	formulaDirMode  = 0o755
)

var (
	semanticVersionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)
	formulaNamePattern     = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	rubyClassPattern       = regexp.MustCompile(`^[A-Z][A-Za-z0-9]*$`)
)

type product struct {
	FormulaName  string
	ClassName    string
	Binary       string
	Description  string
	Dependency   string
	Alias        string
	TestArgument string
}

var products = []product{
	{
		FormulaName:  "witself",
		ClassName:    "Witself",
		Binary:       "witself",
		Description:  "Identity, memory, messaging, and secrets CLI for autonomous AI agents",
		Alias:        "ws",
		TestArgument: "version",
	},
	{
		FormulaName:  "witself-infra",
		ClassName:    "WitselfInfra",
		Binary:       "witself-infra",
		Description:  "Cell infrastructure provisioner using the Pulumi Automation API",
		Dependency:   "pulumi",
		TestArgument: "help",
	},
	{
		FormulaName:  "witself-admin",
		ClassName:    "WitselfAdmin",
		Binary:       "witself-admin",
		Description:  "Fleet administration CLI for the Witself control plane",
		TestArgument: "version",
	},
}

type archive struct {
	URL    string
	SHA256 string
}

type formulaData struct {
	product
	Version     string
	DarwinAMD64 archive
	DarwinARM64 archive
	LinuxAMD64  archive
	LinuxARM64  archive
}

type renderedFormula struct {
	Name    string
	Content []byte
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "homebrew-formula: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("homebrew-formula", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	version := flags.String("version", "", "release version without the v prefix")
	dist := flags.String("dist", "", "directory containing GoReleaser archives")
	output := flags.String("output", "", "directory to receive rendered formulae")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %s", strings.Join(flags.Args(), " "))
	}

	if err := validateVersion(*version); err != nil {
		return err
	}
	if strings.TrimSpace(*dist) == "" {
		return errors.New("--dist is required")
	}
	if strings.TrimSpace(*output) == "" {
		return errors.New("--output is required")
	}

	return renderFormulas(*version, *dist, *output, homebrewtemplate.FormulaTemplate)
}

func validateVersion(version string) error {
	if version == "" {
		return errors.New("--version is required")
	}
	if !semanticVersionPattern.MatchString(version) {
		return fmt.Errorf("invalid --version %q: expected semantic version without a v prefix", version)
	}
	return nil
}

func renderFormulas(version, distDir, outputDir, formulaTemplate string) error {
	if err := validateVersion(version); err != nil {
		return err
	}
	if err := requireDirectory("dist", distDir); err != nil {
		return err
	}
	if err := validateOutputDirectory(outputDir); err != nil {
		return err
	}

	tmpl, err := template.New("formula.rb").Option("missingkey=error").Parse(formulaTemplate)
	if err != nil {
		return fmt.Errorf("parse formula template: %w", err)
	}

	formulae := make([]renderedFormula, 0, len(products))
	for _, product := range products {
		if err := validateProduct(product); err != nil {
			return err
		}
		data, err := loadFormulaData(version, distDir, product)
		if err != nil {
			return err
		}

		var content strings.Builder
		if err := tmpl.Execute(&content, data); err != nil {
			return fmt.Errorf("render %s formula: %w", product.FormulaName, err)
		}
		formulae = append(formulae, renderedFormula{
			Name:    product.FormulaName + ".rb",
			Content: []byte(content.String()),
		})
	}

	formulaDir := filepath.Join(outputDir, "Formula")
	if err := os.MkdirAll(formulaDir, formulaDirMode); err != nil {
		return fmt.Errorf("create formula directory %q: %w", formulaDir, err)
	}
	for _, formula := range formulae {
		if err := atomicWriteFile(filepath.Join(formulaDir, formula.Name), formula.Content, formulaFileMode); err != nil {
			return fmt.Errorf("write %s: %w", formula.Name, err)
		}
	}
	return nil
}

func validateProduct(product product) error {
	if !formulaNamePattern.MatchString(product.FormulaName) {
		return fmt.Errorf("invalid formula name %q", product.FormulaName)
	}
	if !rubyClassPattern.MatchString(product.ClassName) {
		return fmt.Errorf("invalid formula class %q", product.ClassName)
	}
	if !formulaNamePattern.MatchString(product.Binary) {
		return fmt.Errorf("invalid formula binary %q", product.Binary)
	}
	description := strings.TrimSpace(product.Description)
	if description == "" || strings.ContainsAny(description, "\"\r\n") {
		return fmt.Errorf("invalid description for %s", product.FormulaName)
	}
	projectName := strings.SplitN(product.FormulaName, "-", 2)[0]
	if strings.HasPrefix(strings.ToLower(description), projectName) {
		return fmt.Errorf("description for %s must not begin with the formula name", product.FormulaName)
	}
	for label, value := range map[string]string{
		"alias":      product.Alias,
		"dependency": product.Dependency,
		"test":       product.TestArgument,
	} {
		if value != "" && !formulaNamePattern.MatchString(value) {
			return fmt.Errorf("invalid %s %q for %s", label, value, product.FormulaName)
		}
	}
	return nil
}

func loadFormulaData(version, distDir string, product product) (formulaData, error) {
	load := func(goos, goarch string) (archive, error) {
		name := fmt.Sprintf("%s_%s_%s_%s.tar.gz", product.Binary, version, goos, goarch)
		digest, err := hashArchive(filepath.Join(distDir, name))
		if err != nil {
			return archive{}, fmt.Errorf("archive %s: %w", name, err)
		}
		return archive{
			URL:    fmt.Sprintf("https://github.com/witwave-ai/witself/releases/download/v%s/%s", version, name),
			SHA256: digest,
		}, nil
	}

	darwinAMD64, err := load("darwin", "amd64")
	if err != nil {
		return formulaData{}, err
	}
	darwinARM64, err := load("darwin", "arm64")
	if err != nil {
		return formulaData{}, err
	}
	linuxAMD64, err := load("linux", "amd64")
	if err != nil {
		return formulaData{}, err
	}
	linuxARM64, err := load("linux", "arm64")
	if err != nil {
		return formulaData{}, err
	}

	return formulaData{
		product:     product,
		Version:     version,
		DarwinAMD64: darwinAMD64,
		DarwinARM64: darwinARM64,
		LinuxAMD64:  linuxAMD64,
		LinuxARM64:  linuxARM64,
	}, nil
}

func hashArchive(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("expected regular file, got %s", info.Mode().Type())
	}

	file, err := os.Open(path)
	if err != nil {
		return "", err
	}

	digest := sha256.New()
	_, copyErr := io.Copy(digest, file)
	closeErr := file.Close()
	if copyErr != nil {
		return "", copyErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	return fmt.Sprintf("%x", digest.Sum(nil)), nil
}

func requireDirectory(label, path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%s directory %q: %w", label, path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s path %q is not a directory", label, path)
	}
	return nil
}

func validateOutputDirectory(path string) error {
	info, err := os.Stat(path)
	if err == nil {
		if !info.IsDir() {
			return fmt.Errorf("output path %q is not a directory", path)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect output directory %q: %w", path, err)
	}
	return nil
}

func atomicWriteFile(path string, content []byte, mode os.FileMode) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}()

	if err := temporary.Chmod(mode); err != nil {
		return err
	}
	if _, err := temporary.Write(content); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return nil
}
