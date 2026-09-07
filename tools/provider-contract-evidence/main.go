package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

func main() { os.Exit(cli(os.Args[1:], os.Stderr)) }
func identityFlags(f *flag.FlagSet, i *identity, commitName string) {
	f.StringVar(&i.Repository, "repository", "", "expected repository")
	f.StringVar(&i.Workflow, "workflow", "", "ci or release")
	f.StringVar(&i.RunID, "run-id", "", "expected workflow run ID")
	f.IntVar(&i.RunAttempt, "run-attempt", 0, "expected workflow run attempt")
	f.StringVar(&i.SourceRef, "source-ref", "", "expected checked-out ref")
	f.StringVar(&i.SourceCommit, commitName, "", "expected full checkout commit")
	f.StringVar(&i.ReleaseTag, "release-tag", "", "publishing tag, empty for CI/manual runs")
}
func cli(args []string, stderr io.Writer) int {
	return cliWithDependencies(args, stderr, defaultDependencies())
}
func cliWithDependencies(args []string, stderr io.Writer, deps runDependencies) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, "provider-contract-evidence: invalid_arguments")
		return 2
	}
	f := flag.NewFlagSet("provider-contract-evidence", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	i := identity{}
	input, output, markdown, version := "", "", "", ""
	o := runOptions{}
	switch args[0] {
	case "run":
		identityFlags(f, &i, "expected-commit")
		f.StringVar(&o.output, "output", "", "cell output path")
		f.StringVar(&o.target, "target", "", "native target")
		f.StringVar(&o.binary, "binary", "", "installer output executable")
		f.StringVar(&o.dist, "dist", "", "GoReleaser snapshot directory")
	case "aggregate":
		identityFlags(f, &i, "expected-commit")
		f.StringVar(&input, "input-dir", "", "downloaded cell directory")
		f.StringVar(&output, "output", "", "aggregate output path")
		f.StringVar(&markdown, "markdown", "", "optional bounded summary path")
	case "validate-release":
		f.StringVar(&input, "input", "", "published aggregate path")
		f.StringVar(&version, "version", "", "unprefixed publishing version")
		f.StringVar(&i.SourceCommit, "commit", "", "expected full commit")
		f.StringVar(&i.Repository, "repository", "", "expected repository")
		f.StringVar(&i.RunID, "run-id", "", "expected publishing workflow run ID")
		f.IntVar(&i.RunAttempt, "run-attempt", 0, "expected workflow run attempt")
	default:
		_, _ = fmt.Fprintln(stderr, "provider-contract-evidence: invalid_arguments")
		return 2
	}
	if f.Parse(args[1:]) != nil || f.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "provider-contract-evidence: invalid_arguments")
		return 2
	}
	if args[0] == "validate-release" {
		if len(version) > 100 || !versionPattern.MatchString(version) {
			_, _ = fmt.Fprintln(stderr, "provider-contract-evidence: invalid_arguments")
			return 2
		}
		i.Workflow = "release"
		i.ReleaseTag = "v" + version
		i.SourceRef = "refs/tags/" + i.ReleaseTag
	}
	if !validIdentity(i) {
		_, _ = fmt.Fprintln(stderr, "provider-contract-evidence: invalid_arguments")
		return 2
	}
	code := 0
	var err error
	switch args[0] {
	case "run":
		if o.output == "" || o.binary == "" || o.dist == "" || nativeTargets[o.target] == [2]string{} {
			code = 2
			break
		}
		o.identity = i
		var c cell
		c, code = executeRun(context.Background(), o, deps)
		err = writeJSON(o.output, c)
	case "aggregate":
		if input == "" || output == "" {
			code = 2
			break
		}
		var cells []cell
		cells, err = readCells(input)
		if err == nil {
			var a aggregate
			a, err = makeAggregate(cells, i, stamp())
			if err == nil {
				err = writeJSON(output, a)
				if err == nil && markdown != "" {
					err = writeAtomic(markdown, []byte(renderMarkdown(a)))
				}
			}
		}
	case "validate-release":
		if input == "" {
			code = 2
			break
		}
		var raw []byte
		raw, err = readBounded(input, maxReportBytes)
		if err == nil {
			var a aggregate
			err = decodeStrict(raw, &a)
			if err == nil {
				err = validateAggregate(a)
				if a.Identity != i {
					err = errInvalidEvidence
				}
			}
		}
	}
	if err != nil && code == 0 {
		code = 1
	}
	if code != 0 {
		category := "invalid_evidence"
		if code == 2 {
			category = "invalid_arguments"
		}
		_, _ = fmt.Fprintln(stderr, "provider-contract-evidence: "+category)
	}
	return code
}
func writeJSON(path string, value any) error {
	raw, e := json.MarshalIndent(value, "", "  ")
	if e != nil {
		return errInvalidEvidence
	}
	return writeAtomic(path, append(raw, '\n'))
}
func writeAtomic(path string, raw []byte) error {
	if len(raw) > maxReportBytes {
		return errInvalidEvidence
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".provider-contract-*")
	if e != nil {
		return errInvalidEvidence
	}
	name := f.Name()
	// After rename the old temporary path is absent; failed writes still get
	// best-effort cleanup without replacing the authoritative failure result.
	defer func() { _ = os.Remove(name) }()
	if _, e = f.Write(raw); e != nil {
		_ = f.Close()
		return errInvalidEvidence
	}
	if f.Close() != nil {
		return errInvalidEvidence
	}
	if os.Rename(name, path) != nil {
		return errInvalidEvidence
	}
	return nil
}

// download-artifact retains one directory per native cell. Do not flatten:
// repeated targets must be observed and rejected rather than overwritten.
func readCells(root string) ([]cell, error) {
	info, e := os.Lstat(root)
	if e != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errInvalidEvidence
	}
	cells := []cell{}
	entries := 0
	e = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkError error) error {
		if walkError != nil {
			return errInvalidEvidence
		}
		entries++
		if entries > 16 || d.Type()&os.ModeSymlink != 0 {
			return errInvalidEvidence
		}
		rel, e := filepath.Rel(root, path)
		if e != nil || len(strings.Split(rel, string(os.PathSeparator))) > 2 {
			return errInvalidEvidence
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() || len(cells) >= 5 {
			return errInvalidEvidence
		}
		raw, e := readBounded(path, maxReportBytes)
		if e != nil {
			return e
		}
		var c cell
		if decodeStrict(raw, &c) != nil || d.Name() != "provider-contract-"+c.Target+".json" {
			return errInvalidEvidence
		}
		cells = append(cells, c)
		return nil
	})
	if e != nil || len(cells) != 5 {
		return nil, errInvalidEvidence
	}
	return cells, nil
}
func renderMarkdown(a aggregate) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Provider fixture contracts: %d passed, %d native Windows Cursor outcomes not applicable (%d expected test outcomes).\n\n", a.PassedTestOutcomes, a.NotApplicableTestOutcomes, a.ExpectedTestOutcomes)
	fmt.Fprintf(&b, "Source: `%s` at `%s`. Workflow `%s`, run `%s`, attempt `%d`.\n\n", a.Identity.Repository, a.Identity.SourceCommit, a.Identity.Workflow, a.Identity.RunID, a.Identity.RunAttempt)
	b.WriteString("These are offline fixture results from source and installed GoReleaser snapshots. Vendor versions were not observed; real client/model acceptance was not run. The separately built published archive bytes were not tested by these phases.\n\n| Provider | Target | Source | Installed snapshot | Client | Model |\n| --- | --- | --- | --- | --- | --- |\n")
	for _, r := range a.Matrix {
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s |\n", r.Provider, r.Target, r.SourceResult, r.InstalledSnapshotResult, r.ClientResult, r.ModelResult)
	}
	return b.String()
}
