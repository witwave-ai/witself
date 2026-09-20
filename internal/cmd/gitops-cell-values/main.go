// Command gitops-cell-values generates per-cell GitOps values overlays and
// either writes them or checks the committed files for drift.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/witwave-ai/witself/internal/gitopsvalues"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("gitops-cell-values", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	check := fs.Bool("check", false, "exit non-zero with a unified diff when generated files differ")
	write := fs.Bool("write", false, "rewrite generated files that differ")
	rollCell := fs.String("roll-cell", "", "roll exactly one existing catalog cell; sanctioned only via scripts/roll-cell.sh, which guarantees the digest matches the tag")
	version := fs.String("version", "", "release chart version and image tag for --roll-cell")
	imageDigest := fs.String("image-digest", "", "required sha256 image digest for --roll-cell")
	backupRepository := fs.String("backup-image-repository", "", "optional backup image repository for --roll-cell; requires tag and digest")
	backupTag := fs.String("backup-image-tag", "", "backup image release tag for --roll-cell")
	backupDigest := fs.String("backup-image-digest", "", "backup image sha256 digest for --roll-cell")
	root := fs.String("root", ".", "repository root containing .gitops/cells")
	fs.Usage = func() {
		_, _ = fmt.Fprintf(fs.Output(), "usage: gitops-cell-values --check|--write [--root PATH]\n       gitops-cell-values --roll-cell CELL --version VERSION --image-digest DIGEST\n         [--backup-image-repository REPOSITORY --backup-image-tag TAG --backup-image-digest DIGEST] [--root PATH]\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}
	modeCount := 0
	for _, selected := range []bool{*check, *write, *rollCell != ""} {
		if selected {
			modeCount++
		}
	}
	if modeCount != 1 {
		fs.Usage()
		_, _ = fmt.Fprintln(os.Stderr, "error: exactly one of --check, --write, or --roll-cell is required")
		return 2
	}
	if *rollCell == "" && (*version != "" || *imageDigest != "") {
		_, _ = fmt.Fprintln(os.Stderr, "error: --version and --image-digest require --roll-cell")
		return 2
	}
	if *rollCell != "" && (*version == "" || *imageDigest == "") {
		_, _ = fmt.Fprintln(os.Stderr, "error: --roll-cell requires --version and --image-digest")
		return 2
	}
	var backup *gitopsvalues.BackupImagePins
	if *backupRepository != "" || *backupTag != "" || *backupDigest != "" {
		if *rollCell == "" || *backupRepository == "" || *backupTag == "" || *backupDigest == "" {
			_, _ = fmt.Fprintln(os.Stderr, "error: backup image repository, tag, and digest require --roll-cell and must all be supplied")
			return 2
		}
		backup = &gitopsvalues.BackupImagePins{Repository: *backupRepository, Tag: *backupTag, Digest: *backupDigest}
	}
	var err error
	if *check {
		err = gitopsvalues.Check(*root, os.Stdout)
	} else if *rollCell != "" {
		err = gitopsvalues.RollCellWithBackupImage(*root, *rollCell, *version, *imageDigest, backup, os.Stdout)
	} else {
		err = gitopsvalues.Write(*root, os.Stdout)
	}
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		if *check {
			return 1
		}
		return 2
	}
	return 0
}
