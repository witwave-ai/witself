package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/backupevidence"
)

// backupEvidenceCmd verifies retained Civo pre-migration backup artifact
// triples against the witself.civo-pre-migration-backup.v1 contract. Unlike
// every other witself-admin command it is deliberately offline: it reads
// only the local artifact directories it is given and never contacts the
// control plane, a cell, or any network service.
func backupEvidenceCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, backupEvidenceUsage)
		return 2
	}
	switch args[0] {
	case "verify":
		return backupEvidenceVerifyCmd(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "witself-admin backup-evidence: unknown subcommand %q\n", args[0])
		fmt.Fprintln(os.Stderr, backupEvidenceUsage)
		return 2
	}
}

const backupEvidenceUsage = `usage: witself-admin backup-evidence verify --release MAJOR.MINOR.PATCH \
  [--cell CELL]... [--max-age DURATION] [--json] [--evidence-out FILE] DIR [DIR...]

Verifies the encrypted pre-migration backup artifact triples produced by
scripts/civo-pre-migration-backup.sh (the <backup-id>/.dump.age, .sha256,
and .json layout) against the documented rollout gate: verified restore
drill, matching release and schema fences, intact ciphertext checksums, and
owner-only storage. By default evidence for both reviewed Civo cells is
required. Offline; output is count-only and safe to retain.`

type cellListFlag []string

func (c *cellListFlag) String() string { return strings.Join(*c, ",") }

func (c *cellListFlag) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("cell must not be empty")
	}
	*c = append(*c, value)
	return nil
}

func backupEvidenceVerifyCmd(args []string) int {
	fs := flag.NewFlagSet("backup-evidence verify", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	release := fs.String("release", "", "intended rollout version, MAJOR.MINOR.PATCH without a v prefix (required)")
	var cells cellListFlag
	fs.Var(&cells, "cell", "required source cell (repeatable; default: both reviewed Civo cells)")
	maxAge := fs.Duration("max-age", 0, "reject evidence whose created_at is older than this duration (0 disables)")
	jsonOut := fs.Bool("json", false, "emit the count-only report as JSON")
	evidenceOut := fs.String("evidence-out", "", "additionally write the count-only report to this new file (create-only, mode 0600)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	dirs := fs.Args()
	if strings.TrimSpace(*release) == "" || len(dirs) == 0 {
		fmt.Fprintln(os.Stderr, backupEvidenceUsage)
		return 2
	}
	if *maxAge < 0 {
		fmt.Fprintln(os.Stderr, "witself-admin: --max-age must not be negative")
		return 2
	}

	report, findings := backupevidence.Verify(dirs, backupevidence.Options{
		Release:       strings.TrimSpace(*release),
		RequiredCells: cells,
		MaxAge:        *maxAge,
		Now:           time.Now,
	})
	for _, finding := range findings {
		fmt.Fprintf(os.Stderr, "witself-admin: %s\n", finding)
	}
	if *evidenceOut != "" {
		if err := backupevidence.WriteEvidence(report, *evidenceOut); err != nil {
			fmt.Fprintf(os.Stderr, "witself-admin: %v\n", err)
			return 1
		}
	}
	if *jsonOut {
		if code := printJSON(backupEvidenceJSONMap(report)); code != 0 {
			return code
		}
	} else {
		printBackupEvidenceReport(report)
	}
	if report.Result != "pass" {
		return 1
	}
	return 0
}

func backupEvidenceJSONMap(report backupevidence.Report) map[string]any {
	counts := map[string]any{}
	for reason, count := range report.FailureCounts {
		counts[reason] = count
	}
	return map[string]any{
		"schema":             report.Schema,
		"release":            report.Release,
		"inputs_checked":     report.InputsChecked,
		"manifests_verified": report.ManifestsVerified,
		"cells_required":     report.CellsRequired,
		"cells_satisfied":    report.CellsSatisfied,
		"failure_counts":     counts,
		"result":             report.Result,
	}
}

func printBackupEvidenceReport(report backupevidence.Report) {
	fmt.Printf("schema: %s\n", report.Schema)
	fmt.Printf("release: %s\n", report.Release)
	fmt.Printf("inputs_checked: %d\n", report.InputsChecked)
	fmt.Printf("manifests_verified: %d\n", report.ManifestsVerified)
	fmt.Printf("cells_required: %d\n", report.CellsRequired)
	fmt.Printf("cells_satisfied: %d\n", report.CellsSatisfied)
	if len(report.FailureCounts) == 0 {
		fmt.Println("failure_counts: none")
	} else {
		reasons := make([]string, 0, len(report.FailureCounts))
		for reason := range report.FailureCounts {
			reasons = append(reasons, reason)
		}
		sort.Strings(reasons)
		parts := make([]string, 0, len(reasons))
		for _, reason := range reasons {
			parts = append(parts, fmt.Sprintf("%s=%d", reason, report.FailureCounts[reason]))
		}
		fmt.Printf("failure_counts: %s\n", strings.Join(parts, " "))
	}
	fmt.Printf("result: %s\n", report.Result)
}
