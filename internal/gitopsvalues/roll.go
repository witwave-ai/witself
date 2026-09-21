package gitopsvalues

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
)

var releaseVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

// RollCell generates exactly one existing cell with a complete release pin set.
// It refuses drift and validates every input before atomically replacing values.
func RollCell(root, cell, version, digest string, out io.Writer) error {
	return RollCellWithBackupImage(root, cell, version, digest, nil, out)
}

// RollCellWithBackupImage optionally sets the PostgreSQL backup image in the same
// atomic write as the server pins. A nil backup preserves the existing image.
// The caller must resolve both digests before calling this function.
func RollCellWithBackupImage(root, cell, version, digest string, backup *BackupImagePins, out io.Writer) error {
	return RollCellWithImages(root, cell, version, digest, backup, nil, out)
}

// RollCellWithImages optionally selects the published PostgreSQL mirror together
// with the backup and server images in one atomic write. Omitted image selections
// preserve existing pins. The mirror must preserve the cell's existing content.
func RollCellWithImages(root, cell, version, digest string, backup *BackupImagePins, postgres *PostgresImagePins, out io.Writer) error {
	if !releaseVersionPattern.MatchString(version) {
		return fmt.Errorf("release version must look like MAJOR.MINOR.PATCH")
	}
	if !imageDigestPattern.MatchString(digest) {
		return fmt.Errorf("release image digest must be sha256 followed by 64 lowercase hexadecimal characters")
	}
	if backup != nil {
		if err := backup.validate(); err != nil {
			return err
		}
		if backup.Repository == "" || backup.Tag != version || !imageDigestPattern.MatchString(backup.Digest) {
			return fmt.Errorf("backup release pin requires a repository, release-matching tag, and sha256 digest")
		}
	}
	if postgres != nil {
		if err := postgres.validate(); err != nil {
			return err
		}
		if postgres.Registry != "ghcr.io" || postgres.Repository != "witwave-ai/images/postgresql" || postgres.Tag != version+"-"+cell || !imageDigestPattern.MatchString(postgres.Digest) {
			return fmt.Errorf("PostgreSQL mirror pin requires the GHCR mirror, release-and-cell-matching tag, and sha256 digest")
		}
		selected := *postgres
		selected.allowInsecureImages, selected.allowInsecureImagesSet = true, true
		postgres = &selected
	}
	cfg, err := loadCatalog(root)
	if err != nil {
		return err
	}
	if _, ok := cfg.Cells[cell]; !ok {
		return fmt.Errorf("roll cell is not in the cell catalog")
	}
	charts, err := loadChartPins(root)
	if err != nil {
		return err
	}
	path := filepath.Join(root, filepath.FromSlash(valuesRel(cell)))
	before, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	expected, err := generateCell(root, cell, cfg, charts)
	if err != nil {
		return err
	}
	if !bytes.Equal(before, expected) {
		return fmt.Errorf("%s differs from generated output; resolve drift before rolling", valuesRel(cell))
	}
	if postgres != nil {
		existing, err := parsePostgresImagePins(before)
		if err != nil {
			return err
		}
		if existing == nil || existing.Digest == "" || existing.Digest != postgres.Digest {
			return fmt.Errorf("PostgreSQL mirror digest must equal the cell's existing pinned image digest")
		}
	}
	pins := serverPins{ChartVersion: version, ImageTag: version, ImageDigest: digest}
	rolled, err := generateCellWithImagePins(root, cell, cfg, charts, &pins, backup, postgres)
	if err != nil {
		return err
	}
	// Refuse an edit observed during generation rather than replacing that edit.
	current, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(before, current) {
		return fmt.Errorf("%s changed during generation; retry after resolving the edit", valuesRel(cell))
	}
	if !bytes.Equal(before, rolled) {
		if err := writeFileAtomic(path, rolled); err != nil {
			return fmt.Errorf("write %s: %w", valuesRel(cell), err)
		}
	}
	_, _ = fmt.Fprintf(out, "gitops cell values: rolled %s chartVersion, imageTag, and imageDigest\n", cell)
	if backup != nil {
		_, _ = fmt.Fprintln(out, "gitops cell values: pinned PostgreSQL backup image repository, tag, and digest")
	}
	if postgres != nil {
		_, _ = fmt.Fprintln(out, "gitops cell values: pinned PostgreSQL mirror registry, repository, tag, and digest")
	}
	return nil
}
