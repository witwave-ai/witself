// Package backupevidence verifies retained Civo pre-migration backup
// artifacts against the witself.civo-pre-migration-backup.v1 contract
// produced by scripts/civo-pre-migration-backup.sh.
//
// It is the scripted equivalent of the manual manifest gate in the
// operations runbook: both reviewed Civo cells must present a verified,
// integrity-checked, release-matched artifact triple before a
// schema-advancing rollout may change GitOps. The verifier is strictly
// offline — it reads only the artifact directories it is given, makes no
// network, database, or cluster calls, and never decrypts an artifact.
//
// Output is deliberately count-only. Findings name an input by ordinal and
// a manifest field by name; no filesystem path, backup identifier, cell
// value, checksum, or other manifest content is ever copied into a finding
// or report, so the summary is safe to retain as production evidence.
package backupevidence

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// ManifestSchema is the exact schema marker the backup script writes.
	ManifestSchema = "witself.civo-pre-migration-backup.v1"
	// ReportSchema marks the verifier's own count-only evidence summary.
	ReportSchema = "witself.civo-pre-migration-backup.verify.v1"

	maxManifestBytes = 64 * 1024
	maxSidecarBytes  = 4 * 1024
	maxJSONDepth     = 8
	maxCountValue    = int64(1_000_000_000_000)
	maxArtifactBytes = int64(1) << 50
	// The producer accepts any positive goose version id, including a
	// possible future timestamped id such as 20260820120000, so this is a
	// pure overflow sanity bound, not a policy fence.
	maxSchemaVersion = int64(1_000_000_000_000_000)
	clockSkew        = 5 * time.Minute

	timeLayout  = "2006-01-02T15:04:05Z"
	stampLayout = "20060102T150405Z"
)

// ReviewedCells is the closed set of source cells the backup script
// accepts. The verifier refuses evidence claiming any other cell.
var ReviewedCells = []string{"civo-sandbox-use1-backup", "civo-sandbox-usw2-dev"}

var (
	releasePattern     = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	sha256HexPattern   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	stampPattern       = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z$`)
	randomHexPattern   = regexp.MustCompile(`^[0-9a-f]{8}$`)
	contextPattern     = regexp.MustCompile(`^[A-Za-z0-9._:@/-]+$`)
	imageRefPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/@:-]{0,255}$`)
	imageIDPattern     = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	sidecarLinePattern = regexp.MustCompile(`^([0-9a-f]{64})  (\S+)\n$`)
)

// Reason is a bounded failure classification. The set is closed so
// retained evidence summaries stay deterministic and value-free.
type Reason string

// The bounded reason codes an evidence check can fail with.
const (
	ReasonInputPathInvalid     Reason = "input_path_invalid"
	ReasonDirLayoutInvalid     Reason = "dir_layout_invalid"
	ReasonPermissionsInsecure  Reason = "permissions_insecure"
	ReasonManifestUnreadable   Reason = "manifest_unreadable"
	ReasonManifestSyntax       Reason = "manifest_syntax_invalid"
	ReasonManifestFieldInvalid Reason = "manifest_field_invalid"
	ReasonManifestPending      Reason = "manifest_pending"
	ReasonGateFailed           Reason = "manifest_gate_failed"
	ReasonSidecarInvalid       Reason = "checksum_sidecar_invalid"
	ReasonArtifactMismatch     Reason = "artifact_integrity_mismatch"
	ReasonTimestampInvalid     Reason = "timestamp_invalid"
	ReasonTimestampStale       Reason = "timestamp_stale"
	ReasonReleaseMismatch      Reason = "release_mismatch"
	ReasonCellUnsupported      Reason = "cell_unsupported"
	ReasonCellDuplicate        Reason = "cell_duplicate"
	ReasonCellMissing          Reason = "cell_missing"
)

// Finding names one rejected property of one input. Detail carries only a
// static field or check name — never manifest content, paths, or values.
type Finding struct {
	// Input is the 1-based ordinal of the artifact directory argument, or
	// 0 for summary-level findings such as a missing required cell.
	Input  int    `json:"input"`
	Reason Reason `json:"reason"`
	Detail string `json:"detail"`
}

func (f Finding) String() string {
	if f.Input == 0 {
		return fmt.Sprintf("summary: %s (%s)", f.Reason, f.Detail)
	}
	return fmt.Sprintf("input %d: %s (%s)", f.Input, f.Reason, f.Detail)
}

// Options configures one verification pass.
type Options struct {
	// Release is the intended rollout version, MAJOR.MINOR.PATCH without a
	// v prefix. Every manifest must target exactly this release.
	Release string
	// RequiredCells is the set of source cells that must each be covered
	// by exactly one verified input. Empty means both reviewed cells, the
	// documented hard-gate default.
	RequiredCells []string
	// MaxAge, when positive, rejects evidence whose created_at is older.
	// Zero disables the staleness check; the canonical gate expresses
	// currency through the exact target release instead of a number.
	MaxAge time.Duration
	// Now supplies the clock for future-dating and staleness checks.
	// Nil means time.Now.
	Now func() time.Time
}

// Report is the deterministic, count-only verification summary. It carries
// no paths, identifiers, checksums, or manifest values.
type Report struct {
	Schema            string         `json:"schema"`
	Release           string         `json:"release"`
	InputsChecked     int            `json:"inputs_checked"`
	ManifestsVerified int            `json:"manifests_verified"`
	CellsRequired     int            `json:"cells_required"`
	CellsSatisfied    int            `json:"cells_satisfied"`
	FailureCounts     map[string]int `json:"failure_counts"`
	Result            string         `json:"result"`
}

// Verify checks every artifact directory against the recorded contract and
// the documented rollout gate, fail-closed. It performs no network or
// database access and never decrypts artifact ciphertext.
func Verify(dirs []string, opts Options) (Report, []Finding) {
	report := Report{
		Schema:        ReportSchema,
		Release:       opts.Release,
		FailureCounts: map[string]int{},
		Result:        "fail",
	}
	var findings []Finding
	fail := func(f Finding) {
		findings = append(findings, f)
		report.FailureCounts[string(f.Reason)]++
	}

	if !releasePattern.MatchString(opts.Release) {
		fail(Finding{Reason: ReasonReleaseMismatch, Detail: "release option"})
		return report, findings
	}
	required := opts.RequiredCells
	if len(required) == 0 {
		required = ReviewedCells
	}
	requiredSet := map[string]bool{}
	for _, cell := range required {
		if !isReviewedCell(cell) {
			fail(Finding{Reason: ReasonCellUnsupported, Detail: "required cell option"})
			return report, findings
		}
		if requiredSet[cell] {
			fail(Finding{Reason: ReasonCellDuplicate, Detail: "required cell option"})
			return report, findings
		}
		requiredSet[cell] = true
	}
	report.CellsRequired = len(requiredSet)
	if len(dirs) == 0 {
		fail(Finding{Reason: ReasonInputPathInvalid, Detail: "no artifact directories"})
		return report, findings
	}

	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}

	coveredCells := map[string]int{}
	seenBackupIDs := map[string]bool{}
	report.InputsChecked = len(dirs)
	for i, dir := range dirs {
		ordinal := i + 1
		cell, backupID, inputFindings := verifyOne(dir, opts.Release, opts.MaxAge, now())
		if len(inputFindings) > 0 {
			for _, f := range inputFindings {
				f.Input = ordinal
				fail(f)
			}
			continue
		}
		if seenBackupIDs[backupID] {
			fail(Finding{Input: ordinal, Reason: ReasonCellDuplicate, Detail: "backup_id already verified"})
			continue
		}
		seenBackupIDs[backupID] = true
		if prior, dup := coveredCells[cell]; dup {
			_ = prior
			fail(Finding{Input: ordinal, Reason: ReasonCellDuplicate, Detail: "source.cell already covered"})
			continue
		}
		if !requiredSet[cell] {
			fail(Finding{Input: ordinal, Reason: ReasonCellUnsupported, Detail: "source.cell not required"})
			continue
		}
		coveredCells[cell] = ordinal
		report.ManifestsVerified++
	}
	report.CellsSatisfied = len(coveredCells)
	for _, cell := range required {
		if _, ok := coveredCells[cell]; !ok {
			fail(Finding{Reason: ReasonCellMissing, Detail: "required cell without verified evidence"})
		}
	}
	if len(findings) == 0 {
		report.Result = "pass"
	}
	sort.Slice(findings, func(a, b int) bool {
		if findings[a].Input != findings[b].Input {
			return findings[a].Input < findings[b].Input
		}
		if findings[a].Reason != findings[b].Reason {
			return findings[a].Reason < findings[b].Reason
		}
		return findings[a].Detail < findings[b].Detail
	})
	return report, findings
}

func isReviewedCell(cell string) bool {
	for _, reviewed := range ReviewedCells {
		if cell == reviewed {
			return true
		}
	}
	return false
}

// verifyOne validates a single artifact directory. On success it returns
// the manifest's source cell and backup id (for cross-input duplicate
// detection only; callers must not copy them into output).
func verifyOne(dir, release string, maxAge time.Duration, now time.Time) (string, string, []Finding) {
	dirInfo, err := os.Lstat(dir)
	if err != nil {
		return "", "", []Finding{{Reason: ReasonInputPathInvalid, Detail: "artifact directory"}}
	}
	if dirInfo.Mode()&os.ModeSymlink != 0 {
		return "", "", []Finding{{Reason: ReasonInputPathInvalid, Detail: "artifact directory is a symlink"}}
	}
	if !dirInfo.IsDir() {
		return "", "", []Finding{{Reason: ReasonInputPathInvalid, Detail: "artifact directory is not a directory"}}
	}
	if dirInfo.Mode().Perm()&0o077 != 0 {
		return "", "", []Finding{{Reason: ReasonPermissionsInsecure, Detail: "artifact directory mode"}}
	}

	base := filepath.Base(filepath.Clean(dir))
	manifestName := base + ".json"
	sidecarName := base + ".sha256"
	artifactName := base + ".dump.age"

	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", "", []Finding{{Reason: ReasonInputPathInvalid, Detail: "artifact directory unreadable"}}
	}
	expected := map[string]bool{manifestName: false, sidecarName: false, artifactName: false}
	for _, entry := range entries {
		seen, ok := expected[entry.Name()]
		if !ok {
			return "", "", []Finding{{Reason: ReasonDirLayoutInvalid, Detail: "unexpected directory entry"}}
		}
		if seen {
			return "", "", []Finding{{Reason: ReasonDirLayoutInvalid, Detail: "duplicate directory entry"}}
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
			return "", "", []Finding{{Reason: ReasonDirLayoutInvalid, Detail: "irregular directory entry"}}
		}
		expected[entry.Name()] = true
	}
	for name, seen := range expected {
		if !seen {
			_ = name
			return "", "", []Finding{{Reason: ReasonDirLayoutInvalid, Detail: "missing artifact triple member"}}
		}
	}

	manifest, findings := readManifest(filepath.Join(dir, manifestName))
	if len(findings) > 0 {
		return "", "", findings
	}
	findings = validateManifest(manifest, base, release, maxAge, now)
	if len(findings) > 0 {
		return "", "", findings
	}
	if f := verifyArtifactIntegrity(dir, artifactName, sidecarName, manifest); f != nil {
		return "", "", f
	}
	return *manifest.Source.Cell, *manifest.BackupID, nil
}

type manifestDoc struct {
	Schema              *string            `json:"schema"`
	BackupID            *string            `json:"backup_id"`
	Source              *manifestSource    `json:"source"`
	TargetRelease       *string            `json:"target_release"`
	CreatedAt           *string            `json:"created_at"`
	Artifact            *manifestArtifact  `json:"artifact"`
	Procedure           *manifestProcedure `json:"procedure"`
	RestoreVerification *manifestRestore   `json:"restore_verification"`
}

type manifestSource struct {
	Cell                       *string      `json:"cell"`
	KubernetesContext          *string      `json:"kubernetes_context"`
	PostgresqlVersionNum       *json.Number `json:"postgresql_version_num"`
	SchemaVersion              *json.Number `json:"schema_version"`
	PgvectorExtensionInstalled *bool        `json:"pgvector_extension_installed"`
}

type manifestArtifact struct {
	File              *string      `json:"file"`
	Bytes             *json.Number `json:"bytes"`
	Encryption        *string      `json:"encryption"`
	ChecksumAlgorithm *string      `json:"checksum_algorithm"`
	CiphertextSHA256  *string      `json:"ciphertext_sha256"`
	ChecksumFile      *string      `json:"checksum_file"`
}

type manifestProcedure struct {
	ScriptSHA256 *string `json:"script_sha256"`
}

type manifestRestore struct {
	Status                         *string      `json:"status"`
	VerifiedAt                     *string      `json:"verified_at"`
	Network                        *string      `json:"network"`
	PlaintextStorage               *string      `json:"plaintext_storage"`
	ImageRef                       *string      `json:"image_ref"`
	ImageID                        *string      `json:"image_id"`
	SchemaVersion                  *json.Number `json:"schema_version"`
	PublicTableCount               *json.Number `json:"public_table_count"`
	AccountCount                   *json.Number `json:"account_count"`
	InvalidIndexCount              *json.Number `json:"invalid_index_count"`
	UnvalidatedConstraintCount     *json.Number `json:"unvalidated_constraint_count"`
	PgvectorExtensionInstalled     *bool        `json:"pgvector_extension_installed"`
	PgvectorExtensionMatchesSource *bool        `json:"pgvector_extension_matches_source"`
	DisposableTargetCleaned        *bool        `json:"disposable_target_cleaned"`
}

func readManifest(path string) (*manifestDoc, []Finding) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, []Finding{{Reason: ReasonManifestUnreadable, Detail: "manifest missing"}}
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, []Finding{{Reason: ReasonManifestUnreadable, Detail: "manifest not a regular file"}}
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, []Finding{{Reason: ReasonPermissionsInsecure, Detail: "manifest mode"}}
	}
	if info.Size() <= 0 || info.Size() > maxManifestBytes {
		return nil, []Finding{{Reason: ReasonManifestUnreadable, Detail: "manifest size"}}
	}
	raw, err := readBounded(path, maxManifestBytes)
	if err != nil || int64(len(raw)) != info.Size() {
		return nil, []Finding{{Reason: ReasonManifestUnreadable, Detail: "manifest read"}}
	}
	if err := checkStrictJSON(raw); err != nil {
		return nil, []Finding{{Reason: ReasonManifestSyntax, Detail: err.Error()}}
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	dec.DisallowUnknownFields()
	var doc manifestDoc
	if err := dec.Decode(&doc); err != nil {
		return nil, []Finding{{Reason: ReasonManifestSyntax, Detail: "manifest shape"}}
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, []Finding{{Reason: ReasonManifestSyntax, Detail: "trailing data"}}
	}
	return &doc, nil
}

// checkStrictJSON walks the raw token stream to reject duplicate object
// keys, excessive nesting, a non-object document, and trailing content —
// properties encoding/json accepts silently.
func checkStrictJSON(raw []byte) error {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return errors.New("manifest syntax")
	}
	delim, ok := tok.(json.Delim)
	if !ok || delim != '{' {
		return errors.New("document is not an object")
	}
	if err := walkObject(dec, 1); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing data")
	}
	return nil
}

func walkObject(dec *json.Decoder, depth int) error {
	if depth > maxJSONDepth {
		return errors.New("document too deep")
	}
	seen := map[string]bool{}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return errors.New("manifest syntax")
		}
		key, ok := keyTok.(string)
		if !ok {
			return errors.New("manifest syntax")
		}
		if seen[key] {
			return errors.New("duplicate object key")
		}
		seen[key] = true
		if err := walkValue(dec, depth); err != nil {
			return err
		}
	}
	if _, err := dec.Token(); err != nil {
		return errors.New("manifest syntax")
	}
	return nil
}

func walkValue(dec *json.Decoder, depth int) error {
	tok, err := dec.Token()
	if err != nil {
		return errors.New("manifest syntax")
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		return walkObject(dec, depth+1)
	case '[':
		if depth+1 > maxJSONDepth {
			return errors.New("document too deep")
		}
		for dec.More() {
			if err := walkValue(dec, depth+1); err != nil {
				return err
			}
		}
		if _, err := dec.Token(); err != nil {
			return errors.New("manifest syntax")
		}
		return nil
	default:
		return errors.New("manifest syntax")
	}
}

func validateManifest(doc *manifestDoc, dirBase, release string, maxAge time.Duration, now time.Time) []Finding {
	var findings []Finding
	field := func(reason Reason, name string) {
		findings = append(findings, Finding{Reason: reason, Detail: name})
	}
	// Every required string in the producer's manifest is non-empty by
	// construction, so a present-but-empty value is exactly as invalid as a
	// missing one. Returning "" for both keeps every later `!= ""` guard a
	// pure skip-already-failed-field guard rather than a bypass.
	requireString := func(value *string, name string) string {
		if value == nil || *value == "" {
			field(ReasonManifestFieldInvalid, name)
			return ""
		}
		return *value
	}
	requireBool := func(value *bool, name string) bool {
		if value == nil {
			field(ReasonManifestFieldInvalid, name)
			return false
		}
		return *value
	}
	requireInt := func(value *json.Number, name string, lowest, highest int64) int64 {
		if value == nil {
			field(ReasonManifestFieldInvalid, name)
			return -1
		}
		parsed, err := strconv.ParseInt(value.String(), 10, 64)
		if err != nil || parsed < lowest || parsed > highest {
			field(ReasonManifestFieldInvalid, name)
			return -1
		}
		return parsed
	}

	if doc.Source == nil || doc.Artifact == nil || doc.Procedure == nil || doc.RestoreVerification == nil {
		return []Finding{{Reason: ReasonManifestFieldInvalid, Detail: "required object missing"}}
	}

	if requireString(doc.Schema, "schema") != ManifestSchema {
		field(ReasonManifestFieldInvalid, "schema")
	}
	cell := requireString(doc.Source.Cell, "source.cell")
	if cell != "" && !isReviewedCell(cell) {
		field(ReasonCellUnsupported, "source.cell")
	}
	if kctx := requireString(doc.Source.KubernetesContext, "source.kubernetes_context"); kctx != "" && !contextPattern.MatchString(kctx) {
		field(ReasonManifestFieldInvalid, "source.kubernetes_context")
	}
	requireInt(doc.Source.PostgresqlVersionNum, "source.postgresql_version_num", 10000, 999999)
	sourceSchema := requireInt(doc.Source.SchemaVersion, "source.schema_version", 1, maxSchemaVersion)
	sourcePgvector := requireBool(doc.Source.PgvectorExtensionInstalled, "source.pgvector_extension_installed")

	targetRelease := requireString(doc.TargetRelease, "target_release")
	if targetRelease != "" && !releasePattern.MatchString(targetRelease) {
		field(ReasonManifestFieldInvalid, "target_release")
	}
	if targetRelease != "" && releasePattern.MatchString(targetRelease) && targetRelease != release {
		field(ReasonReleaseMismatch, "target_release")
	}

	backupID := requireString(doc.BackupID, "backup_id")
	var idStamp time.Time
	if backupID != "" && cell != "" && targetRelease != "" {
		prefix := cell + "-pre-v" + targetRelease + "-"
		rest, hasPrefix := strings.CutPrefix(backupID, prefix)
		parts := strings.SplitN(rest, "-", 2)
		if !hasPrefix || len(parts) != 2 ||
			!stampPattern.MatchString(parts[0]) || !randomHexPattern.MatchString(parts[1]) {
			field(ReasonManifestFieldInvalid, "backup_id")
		} else {
			stamp, err := time.Parse(stampLayout, parts[0])
			if err != nil {
				field(ReasonManifestFieldInvalid, "backup_id")
			} else {
				idStamp = stamp
			}
		}
		if backupID != dirBase {
			field(ReasonDirLayoutInvalid, "backup_id directory name")
		}
	}

	createdAtRaw := requireString(doc.CreatedAt, "created_at")
	var createdAt time.Time
	if createdAtRaw != "" {
		parsed, err := time.Parse(timeLayout, createdAtRaw)
		if err != nil || parsed.Format(timeLayout) != createdAtRaw {
			field(ReasonTimestampInvalid, "created_at")
		} else {
			createdAt = parsed
			if createdAt.After(now.Add(clockSkew)) {
				field(ReasonTimestampInvalid, "created_at future")
			}
			if maxAge > 0 && now.Sub(createdAt) > maxAge {
				field(ReasonTimestampStale, "created_at")
			}
			if !idStamp.IsZero() {
				delta := createdAt.Sub(idStamp)
				if delta < -time.Hour || delta > time.Hour {
					field(ReasonTimestampInvalid, "created_at vs backup_id stamp")
				}
			}
		}
	}

	if file := requireString(doc.Artifact.File, "artifact.file"); file != "" && backupID != "" && file != backupID+".dump.age" {
		field(ReasonManifestFieldInvalid, "artifact.file")
	}
	requireInt(doc.Artifact.Bytes, "artifact.bytes", 1, maxArtifactBytes)
	if requireString(doc.Artifact.Encryption, "artifact.encryption") != "age" {
		field(ReasonManifestFieldInvalid, "artifact.encryption")
	}
	if requireString(doc.Artifact.ChecksumAlgorithm, "artifact.checksum_algorithm") != "sha256" {
		field(ReasonManifestFieldInvalid, "artifact.checksum_algorithm")
	}
	if digest := requireString(doc.Artifact.CiphertextSHA256, "artifact.ciphertext_sha256"); digest != "" && !sha256HexPattern.MatchString(digest) {
		field(ReasonManifestFieldInvalid, "artifact.ciphertext_sha256")
	}
	if checksumFile := requireString(doc.Artifact.ChecksumFile, "artifact.checksum_file"); checksumFile != "" && backupID != "" && checksumFile != backupID+".sha256" {
		field(ReasonManifestFieldInvalid, "artifact.checksum_file")
	}
	if script := requireString(doc.Procedure.ScriptSHA256, "procedure.script_sha256"); script != "" && !sha256HexPattern.MatchString(script) {
		field(ReasonManifestFieldInvalid, "procedure.script_sha256")
	}

	rv := doc.RestoreVerification
	status := requireString(rv.Status, "restore_verification.status")
	if requireString(rv.Network, "restore_verification.network") != "none" {
		field(ReasonManifestFieldInvalid, "restore_verification.network")
	}
	if requireString(rv.PlaintextStorage, "restore_verification.plaintext_storage") != "container tmpfs" {
		field(ReasonManifestFieldInvalid, "restore_verification.plaintext_storage")
	}
	if ref := requireString(rv.ImageRef, "restore_verification.image_ref"); ref != "" && !imageRefPattern.MatchString(ref) {
		field(ReasonManifestFieldInvalid, "restore_verification.image_ref")
	}
	if id := requireString(rv.ImageID, "restore_verification.image_id"); id != "" && !imageIDPattern.MatchString(id) {
		field(ReasonManifestFieldInvalid, "restore_verification.image_id")
	}
	restoredSchema := requireInt(rv.SchemaVersion, "restore_verification.schema_version", 0, maxSchemaVersion)
	publicTables := requireInt(rv.PublicTableCount, "restore_verification.public_table_count", 0, maxCountValue)
	requireInt(rv.AccountCount, "restore_verification.account_count", 0, maxCountValue)
	invalidIndexes := requireInt(rv.InvalidIndexCount, "restore_verification.invalid_index_count", 0, maxCountValue)
	unvalidated := requireInt(rv.UnvalidatedConstraintCount, "restore_verification.unvalidated_constraint_count", 0, maxCountValue)
	restoredPgvector := requireBool(rv.PgvectorExtensionInstalled, "restore_verification.pgvector_extension_installed")
	matchesSource := requireBool(rv.PgvectorExtensionMatchesSource, "restore_verification.pgvector_extension_matches_source")
	cleaned := requireBool(rv.DisposableTargetCleaned, "restore_verification.disposable_target_cleaned")

	switch status {
	case "verified":
		if rv.VerifiedAt == nil {
			field(ReasonManifestFieldInvalid, "restore_verification.verified_at")
		} else {
			verifiedAt, err := time.Parse(timeLayout, *rv.VerifiedAt)
			if err != nil || verifiedAt.Format(timeLayout) != *rv.VerifiedAt {
				field(ReasonTimestampInvalid, "restore_verification.verified_at")
			} else {
				if verifiedAt.After(now.Add(clockSkew)) {
					field(ReasonTimestampInvalid, "restore_verification.verified_at future")
				}
				if !createdAt.IsZero() && verifiedAt.Before(createdAt) {
					field(ReasonTimestampInvalid, "restore_verification.verified_at before created_at")
				}
			}
		}
		// The -1 missing/invalid sentinel already carries its own
		// manifest_field_invalid finding; gate findings fire only on values
		// that genuinely parsed, so the retained count-only summary never
		// misreports a field absence as a failed restore drill.
		if sourceSchema > 0 && restoredSchema >= 0 && restoredSchema != sourceSchema {
			field(ReasonGateFailed, "restore_verification.schema_version")
		}
		if publicTables == 0 {
			field(ReasonGateFailed, "restore_verification.public_table_count")
		}
		if invalidIndexes > 0 {
			field(ReasonGateFailed, "restore_verification.invalid_index_count")
		}
		if unvalidated > 0 {
			field(ReasonGateFailed, "restore_verification.unvalidated_constraint_count")
		}
		if rv.PgvectorExtensionInstalled != nil && doc.Source.PgvectorExtensionInstalled != nil &&
			restoredPgvector != sourcePgvector {
			field(ReasonGateFailed, "restore_verification.pgvector_extension_installed")
		}
		if rv.PgvectorExtensionMatchesSource != nil && !matchesSource {
			field(ReasonGateFailed, "restore_verification.pgvector_extension_matches_source")
		}
		if rv.DisposableTargetCleaned != nil && !cleaned {
			field(ReasonGateFailed, "restore_verification.disposable_target_cleaned")
		}
	case "pending":
		// A pending manifest is a deliberate producer state, but it must be
		// internally consistent: pending evidence claiming verified data is
		// contradictory, and pending never passes the rollout gate.
		if rv.VerifiedAt != nil {
			field(ReasonManifestFieldInvalid, "restore_verification.verified_at on pending")
		}
		if restoredSchema > 0 || publicTables > 0 || invalidIndexes > 0 || unvalidated > 0 {
			field(ReasonManifestFieldInvalid, "restore_verification counts on pending")
		}
		if (rv.PgvectorExtensionMatchesSource != nil && matchesSource) ||
			(rv.DisposableTargetCleaned != nil && cleaned) {
			field(ReasonManifestFieldInvalid, "restore_verification flags on pending")
		}
		field(ReasonManifestPending, "restore_verification.status")
	default:
		if status != "" {
			field(ReasonManifestFieldInvalid, "restore_verification.status")
		}
	}
	return findings
}

func verifyArtifactIntegrity(dir, artifactName, sidecarName string, doc *manifestDoc) []Finding {
	artifactPath := filepath.Join(dir, artifactName)
	info, err := os.Lstat(artifactPath)
	if err != nil {
		return []Finding{{Reason: ReasonArtifactMismatch, Detail: "artifact missing"}}
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return []Finding{{Reason: ReasonArtifactMismatch, Detail: "artifact not a regular file"}}
	}
	if info.Mode().Perm()&0o077 != 0 {
		return []Finding{{Reason: ReasonPermissionsInsecure, Detail: "artifact mode"}}
	}
	declaredBytes, err := strconv.ParseInt(doc.Artifact.Bytes.String(), 10, 64)
	if err != nil || info.Size() != declaredBytes {
		return []Finding{{Reason: ReasonArtifactMismatch, Detail: "artifact.bytes"}}
	}

	file, err := os.Open(artifactPath)
	if err != nil {
		return []Finding{{Reason: ReasonArtifactMismatch, Detail: "artifact unreadable"}}
	}
	defer func() { _ = file.Close() }()
	hasher := sha256.New()
	hashed, err := io.Copy(hasher, io.LimitReader(file, declaredBytes+1))
	if err != nil || hashed != declaredBytes {
		return []Finding{{Reason: ReasonArtifactMismatch, Detail: "artifact read"}}
	}
	computed := hex.EncodeToString(hasher.Sum(nil))
	if computed != *doc.Artifact.CiphertextSHA256 {
		return []Finding{{Reason: ReasonArtifactMismatch, Detail: "artifact.ciphertext_sha256"}}
	}

	sidecarPath := filepath.Join(dir, sidecarName)
	sidecarInfo, err := os.Lstat(sidecarPath)
	if err != nil {
		return []Finding{{Reason: ReasonSidecarInvalid, Detail: "sidecar missing"}}
	}
	if sidecarInfo.Mode()&os.ModeSymlink != 0 || !sidecarInfo.Mode().IsRegular() {
		return []Finding{{Reason: ReasonSidecarInvalid, Detail: "sidecar not a regular file"}}
	}
	if sidecarInfo.Mode().Perm()&0o077 != 0 {
		return []Finding{{Reason: ReasonPermissionsInsecure, Detail: "sidecar mode"}}
	}
	if sidecarInfo.Size() <= 0 || sidecarInfo.Size() > maxSidecarBytes {
		return []Finding{{Reason: ReasonSidecarInvalid, Detail: "sidecar size"}}
	}
	raw, err := readBounded(sidecarPath, maxSidecarBytes)
	if err != nil {
		return []Finding{{Reason: ReasonSidecarInvalid, Detail: "sidecar read"}}
	}
	match := sidecarLinePattern.FindSubmatch(raw)
	if match == nil {
		return []Finding{{Reason: ReasonSidecarInvalid, Detail: "sidecar format"}}
	}
	if string(match[2]) != artifactName {
		return []Finding{{Reason: ReasonSidecarInvalid, Detail: "sidecar filename"}}
	}
	if string(match[1]) != computed {
		return []Finding{{Reason: ReasonSidecarInvalid, Detail: "sidecar checksum"}}
	}
	return nil
}

// readBounded reads a file through a hard byte limit so a post-Lstat file
// swap can never trigger an unbounded read; the artifact hash path applies
// the same discipline through its own LimitReader.
func readBounded(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	raw, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, errors.New("file exceeds size bound")
	}
	return raw, nil
}

// WriteEvidence persists the count-only report as a create-only, owner-only
// JSON file. An existing file is never overwritten.
func WriteEvidence(report Report, path string) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("evidence path is empty")
	}
	payload, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("encode evidence: %w", err)
	}
	payload = append(payload, '\n')
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create evidence file: %w", err)
	}
	if _, err := file.Write(payload); err != nil {
		_ = file.Close()
		return fmt.Errorf("write evidence file: %w", err)
	}
	return file.Close()
}
