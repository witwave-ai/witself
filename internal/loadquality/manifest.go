package loadquality

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/witwave-ai/witself/internal/jsonstrict"
)

// ManifestSchemaV1 identifies the separate, run-level hosted evidence document.
const ManifestSchemaV1 = "witself.memory-load-quality-manifest.v1"

// ErrEvidenceRedacted means unsafe evidence was removed successfully. A caller
// may publish the remaining sanitized artifact, but must mark the run failed.
var ErrEvidenceRedacted = errors.New("unsafe evidence removed; measurement outcome must fail")

// Manifest binds validated slice documents to one hosted workflow invocation.
// Slice paths are flat, fixed basenames relative to the evidence directory.
type Manifest struct {
	Schema      string           `json:"schema"`
	GeneratedAt time.Time        `json:"generated_at"`
	RunURL      string           `json:"run_url"`
	RunAttempt  int              `json:"run_attempt"`
	Release     string           `json:"release"`
	Commit      string           `json:"commit"`
	Runner      ManifestRunner   `json:"runner"`
	Postgres    ManifestPostgres `json:"postgres"`
	Slices      []ManifestSlice  `json:"slices"`
	Outcome     string           `json:"outcome"`
}

// ManifestRunner contains only bounded, dotless GitHub runner labels.
type ManifestRunner struct {
	Name        string `json:"name"`
	OS          string `json:"os"`
	Arch        string `json:"arch"`
	Environment string `json:"environment"`
}

// ManifestPostgres identifies software, never database topology or credentials.
// ServerVersionNum may be zero only on a failed run that never reached SQL.
type ManifestPostgres struct {
	ImageLabel       string `json:"image_label"`
	ServerVersionNum int    `json:"server_version_num"`
}

// ManifestSlice records the expected result even when its step failed or never
// ran. SHA256 is empty when no valid result was produced.
type ManifestSlice struct {
	Name    string `json:"name"`
	Schema  string `json:"schema"`
	Path    string `json:"path"`
	SHA256  string `json:"sha256"`
	Outcome string `json:"outcome"`
}

const (
	manifestRunURLPrefix = "https://github.com/witwave-ai/witself/actions/runs/"
	evidenceRedaction    = "Evidence removed by the credential and schema safety scan.\n"
	maximumEvidenceBytes = 16 << 20
)

var manifestCommitPattern = regexp.MustCompile(`^[0-9a-f]{7,64}$`)
var manifestDigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var manifestRunIDPattern = regexp.MustCompile(`^[0-9]+$`)

const hostedGoModulePrefix = "github.com/witwave-ai/witself/"
const hostedGoSourcePrefix = "/home/runner/work/witself/witself/"
const goStackSymbol = `(?:\(\*?[A-Za-z_][A-Za-z0-9_]*(?:\[[^()\r\n]*\])?\)\.)?[A-Za-z_][A-Za-z0-9_.]*(?:\[[^()\r\n]*\])?`

var hostedGoSourceFrame = regexp.MustCompile(`^[ \t]+/home/runner/work/witself/witself/((?:[A-Za-z0-9_-]+/)*[A-Za-z0-9_-]+\.go):[1-9][0-9]*(?:[ \t]+\+0x[0-9a-f]+)?[ \t]*\r?$`)
var goCallFrame = regexp.MustCompile(`^` + goStackSymbol + `\([^()\r\n]*\)$`)
var goCreatedFrame = regexp.MustCompile(`^` + goStackSymbol + `(?: in goroutine [1-9][0-9]*)?$`)

// scrubHostedGoStackPrefixes exempts only the two fixed public repository
// prefixes in a paired Go function/source frame. The relative source path must
// be canonical and agree with the function's package; standalone paths, other
// checkouts, and lookalikes are not exemptions. Function names, arguments,
// relative paths and every surrounding byte remain subject to credential
// scanning. This changes only the scan input, never the retained diagnostics.
func scrubHostedGoStackPrefixes(log string) string {
	lines := strings.Split(log, "\n")
	for i := 1; i < len(lines); i++ {
		source := hostedGoSourceFrame.FindStringSubmatch(lines[i])
		if source == nil {
			continue
		}
		frame := strings.TrimSuffix(lines[i-1], "\r")
		function, created := strings.CutPrefix(frame, "created by ")
		packagePrefix := hostedGoModulePrefix + filepath.Dir(source[1]) + "."
		symbol, matchesPackage := strings.CutPrefix(function, packagePrefix)
		if !matchesPackage || (!created && !goCallFrame.MatchString(symbol)) || (created && !goCreatedFrame.MatchString(symbol)) {
			continue
		}
		lines[i-1] = strings.Replace(lines[i-1], hostedGoModulePrefix, "", 1)
		lines[i] = strings.Replace(lines[i], hostedGoSourcePrefix, "", 1)
	}
	return strings.Join(lines, "\n")
}

func manifestSliceSchema(name string) string {
	switch name {
	case "lexical":
		return ResultSchemaV1
	case "curation":
		return CurationResultSchemaV1
	case "recall":
		return RecallResultSchemaV1
	case "archive":
		return ArchiveResultSchemaV1
	case "concurrency":
		return ConcurrencyResultSchemaV1
	default:
		return ""
	}
}

func validateManifestEnvelope(m Manifest) error {
	if m.Schema != ManifestSchemaV1 || m.GeneratedAt.IsZero() || m.RunAttempt < 1 ||
		len(m.RunURL) > 256 || !strings.HasPrefix(m.RunURL, manifestRunURLPrefix) ||
		!manifestRunIDPattern.MatchString(strings.TrimPrefix(m.RunURL, manifestRunURLPrefix)) ||
		!safeMetadata(m.Release) || !manifestCommitPattern.MatchString(m.Commit) ||
		!curationLabelMetadata(m.Runner.Name) || !curationLabelMetadata(m.Runner.OS) ||
		!curationLabelMetadata(m.Runner.Arch) || !curationLabelMetadata(m.Runner.Environment) ||
		!curationLabelMetadata(m.Postgres.ImageLabel) ||
		(m.Outcome != "pass" && m.Outcome != "fail") || len(m.Slices) < 1 || len(m.Slices) > 5 ||
		m.Postgres.ServerVersionNum < 0 || m.Postgres.ServerVersionNum > 999999 ||
		(m.Outcome == "pass" && m.Postgres.ServerVersionNum < 100000) {
		return errors.New("invalid memory load-quality manifest metadata")
	}
	return nil
}

// ValidateManifest validates the envelope, known slice schemas and semantic
// checks, metadata consistency, and hashes of the exact retained result bytes.
func ValidateManifest(m Manifest, dir string) error {
	if err := validateManifestEnvelope(m); err != nil {
		return err
	}
	if m.Outcome == "pass" {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return errors.New("read evidence directory")
		}
		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), ".redacted") {
				return errors.New("redacted evidence cannot have a passing manifest")
			}
		}
	}
	seen := make(map[string]bool, len(m.Slices))
	for _, slice := range m.Slices {
		schema := manifestSliceSchema(slice.Name)
		if schema == "" || slice.Schema != schema || seen[slice.Name] || slice.Path != "memory-"+slice.Name+".json" ||
			(slice.Outcome != "pass" && slice.Outcome != "fail") || (m.Outcome == "pass" && slice.Outcome != "pass") {
			return errors.New("invalid memory load-quality manifest slice")
		}
		seen[slice.Name] = true
		if slice.SHA256 == "" && slice.Outcome == "fail" {
			continue
		}
		if !manifestDigestPattern.MatchString(slice.SHA256) {
			return errors.New("invalid memory load-quality manifest digest")
		}
		raw, err := readEvidenceFile(filepath.Join(dir, slice.Path))
		if err != nil {
			return err
		}
		digest := sha256.Sum256(raw)
		if hex.EncodeToString(digest[:]) != slice.SHA256 {
			return errors.New("memory load-quality result digest mismatch")
		}
		metadata, err := validateManifestResult(slice.Name, raw)
		if err != nil {
			return err
		}
		if metadata.Release != m.Release || metadata.Commit != m.Commit || metadata.Provider != "github-hosted" ||
			metadata.HardwareTier != "ubuntu-latest-"+m.Postgres.ImageLabel {
			return errors.New("memory load-quality result metadata mismatch")
		}
	}
	return nil
}

// BuildManifest fills the named expected slices using their fixed result paths.
// Workflow outcomes are independent evidence: a valid result cannot turn a
// failed, cancelled, skipped, or unknown step into a pass. An input failure
// (including a credential-scan failure) is never upgraded to a pass.
func BuildManifest(dir string, m Manifest, outcomes map[string]string) (Manifest, error) {
	// A failed startup may have no SQL version or result files yet. Validate
	// the metadata shape now and the final pass requirements after roll-up.
	envelope := m
	envelope.Outcome = "fail"
	if m.Outcome != "pass" && m.Outcome != "fail" {
		return Manifest{}, errors.New("invalid memory load-quality manifest outcome")
	}
	if err := validateManifestEnvelope(envelope); err != nil {
		return Manifest{}, err
	}
	if m.Postgres.ServerVersionNum < 100000 {
		m.Outcome = "fail"
	}
	slices := make([]ManifestSlice, 0, len(m.Slices))
	seen := map[string]bool{}
	for _, expected := range m.Slices {
		schema := manifestSliceSchema(expected.Name)
		if schema == "" || seen[expected.Name] {
			return Manifest{}, errors.New("invalid expected memory load-quality slice")
		}
		seen[expected.Name] = true
		slice := ManifestSlice{Name: expected.Name, Schema: schema, Path: "memory-" + expected.Name + ".json", Outcome: "fail"}
		raw, err := readEvidenceFile(filepath.Join(dir, slice.Path))
		if err == nil {
			metadata, validationErr := validateManifestResult(slice.Name, raw)
			if validationErr == nil && metadata.Release == m.Release && metadata.Commit == m.Commit && metadata.Provider == "github-hosted" && metadata.HardwareTier == "ubuntu-latest-"+m.Postgres.ImageLabel {
				digest := sha256.Sum256(raw)
				slice.SHA256 = hex.EncodeToString(digest[:])
				if outcomes[slice.Name] == "success" || outcomes[slice.Name] == "pass" {
					slice.Outcome = "pass"
				}
			}
		}
		if slice.Outcome != "pass" {
			m.Outcome = "fail"
		}
		slices = append(slices, slice)
	}
	m.Slices = slices
	if err := ValidateManifest(m, dir); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// WriteManifest atomically publishes a validated, private manifest. Errors
// deliberately omit filesystem paths and underlying input values.
func WriteManifest(path string, m Manifest, dir string) ([]byte, error) {
	if err := ValidateManifest(m, dir); err != nil {
		return nil, err
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, errors.New("encode memory load-quality manifest")
	}
	raw = append(raw, '\n')
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("memory load-quality manifest path is required")
	}
	if err := writePrivateEvidence(path, raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func writePrivateEvidence(path string, raw []byte) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return errors.New("create private evidence directory")
	}
	temporary, err := os.CreateTemp(directory, ".memory-manifest-*.tmp")
	if err != nil {
		return errors.New("create private evidence file")
	}
	name := temporary.Name()
	defer func() { _ = os.Remove(name) }()
	if err := temporary.Chmod(0600); err != nil {
		_ = temporary.Close()
		return errors.New("protect evidence file")
	}
	if _, err := temporary.Write(raw); err != nil {
		_ = temporary.Close()
		return errors.New("write evidence file")
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return errors.New("sync evidence file")
	}
	if err := temporary.Close(); err != nil {
		return errors.New("close evidence file")
	}
	if err := os.Rename(name, path); err != nil {
		return errors.New("publish evidence file")
	}
	return nil
}

func readEvidenceFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maximumEvidenceBytes {
		return nil, errors.New("unavailable or unsafe evidence file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("read evidence file")
	}
	return raw, nil
}

func decodeEvidenceJSON(raw []byte, target any) error {
	structural := json.NewDecoder(bytes.NewReader(raw))
	if err := jsonstrict.ConsumeUniqueValue(structural); err != nil {
		return errors.New("invalid or duplicate evidence JSON fields")
	}
	if err := jsonstrict.RequireEOF(structural); err != nil {
		return errors.New("invalid trailing evidence JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return errors.New("invalid evidence JSON")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("invalid trailing evidence JSON")
	}
	return nil
}

func validateManifestResult(name string, raw []byte) (SafeMetadata, error) {
	var metadata SafeMetadata
	var err error
	switch name {
	case "lexical":
		var result Result
		err = decodeEvidenceJSON(raw, &result)
		if err == nil {
			err = ValidateResult(result)
		}
		if err == nil {
			var schema jsonschema.Schema
			err = json.Unmarshal(ResultJSONSchema(), &schema)
			if err == nil {
				var resolved *jsonschema.Resolved
				resolved, err = schema.Resolve(nil)
				if err == nil {
					var instance any
					err = json.Unmarshal(raw, &instance)
					if err == nil {
						err = resolved.Validate(instance)
					}
				}
			}
		}
		metadata = result.Environment
	case "curation":
		var result CurationResult
		err = decodeEvidenceJSON(raw, &result)
		if err == nil {
			err = ValidateCurationResult(result)
		}
		if err == nil {
			err = ValidateCurationResultJSON(raw)
		}
		metadata = result.Environment
	case "recall":
		var result RecallResult
		err = decodeEvidenceJSON(raw, &result)
		if err == nil {
			err = ValidateRecallResult(result)
		}
		if err == nil {
			err = ValidateRecallResultJSON(raw)
		}
		metadata = result.Environment
	case "archive":
		var result ArchiveResult
		err = decodeEvidenceJSON(raw, &result)
		if err == nil {
			err = ValidateArchiveResult(result)
		}
		if err == nil {
			err = ValidateArchiveResultJSON(raw)
		}
		metadata = result.Environment
	case "concurrency":
		var result ConcurrencyResult
		err = decodeEvidenceJSON(raw, &result)
		if err == nil {
			err = ValidateConcurrencyResult(result)
		}
		if err == nil {
			err = ValidateConcurrencyResultJSON(raw)
		}
		metadata = result.Environment
	default:
		err = errors.New("unknown result schema")
	}
	if err != nil || !curationLabelMetadata(metadata.Provider) || !curationLabelMetadata(metadata.HardwareTier) {
		return SafeMetadata{}, errors.New("invalid or unsafe memory load-quality result")
	}
	return metadata, nil
}

// ScanEvidence checks the complete flat upload directory, validates each known
// JSON result with its existing schema and semantic validator, and scans all
// values for the supplied DSN's credentials and topology. Unsafe inputs are
// removed, never renamed with their original bytes into the uploaded artifact.
// Only a fixed, value-free .redacted notice remains. Unknown files, nested
// directories, symlinks, and forged redaction notices are removed as well.
// An optional public release must be independently validated by the caller
// against the dispatch ref, never learned from the evidence being scanned.
// Only matching JSON release fields are exempt from credential matching.
func ScanEvidence(dir, dsn string, publicRelease ...string) error {
	if len(publicRelease) > 1 || (len(publicRelease) == 1 && !safeMetadata(publicRelease[0])) {
		return errors.New("invalid evidence public release metadata")
	}
	release := ""
	var loggedRelease *regexp.Regexp
	if len(publicRelease) == 1 {
		release = publicRelease[0]
		quotedRelease, _ := json.Marshal(release)
		loggedRelease = regexp.MustCompile(`("release"[ \t\r\n]*:[ \t\r\n]*)` + regexp.QuoteMeta(string(quotedRelease)))
	}
	forbidden, err := evidenceForbidden(dsn)
	if err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("unsafe or unavailable evidence directory")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return errors.New("read evidence directory")
	}
	redacted := false
	for _, entry := range entries {
		name := entry.Name()
		path := filepath.Join(dir, name)
		known := knownEvidenceName(name)
		raw, readErr := readEvidenceFile(path)
		safe := known && readErr == nil && !containsEvidenceForbidden(name, forbidden)
		if safe {
			switch {
			case strings.HasSuffix(name, ".redacted"):
				safe = string(raw) == evidenceRedaction
			case strings.HasSuffix(name, ".log"):
				// The canonical go-test package name contains the hosted database's
				// throwaway username. Exempt that exact public identifier, not its line.
				scrubbed := scrubHostedGoStackPrefixes(string(raw))
				scrubbed = strings.ReplaceAll(scrubbed, "github.com/witwave-ai/witself/internal/store", "")
				for _, slice := range []string{"lexical", "curation", "recall", "archive", "concurrency"} {
					// Harnesses log their exact JSON document and configured
					// result path. These public schema identifiers and known
					// local output paths also contain the throwaway username.
					// Retain scanning of every other byte on the same line.
					quotedSchema, _ := json.Marshal(manifestSliceSchema(slice))
					scrubbed = strings.ReplaceAll(scrubbed, string(quotedSchema), "")
					resultPath := filepath.Join(dir, "memory-"+slice+".json")
					scrubbed = strings.ReplaceAll(scrubbed, resultPath, "")
				}
				if loggedRelease != nil {
					// Exempt only the exact public release field, retaining all
					// surrounding bytes for the credential scan.
					scrubbed = loggedRelease.ReplaceAllString(scrubbed, `${1}""`)
				}
				safe = !containsEvidenceForbidden(scrubbed, forbidden)
			case name == "workflow-manifest.json":
				var manifest Manifest
				safe = decodeEvidenceJSON(raw, &manifest) == nil && ValidateManifest(manifest, dir) == nil && safeEvidenceJSON(raw, forbidden, release)
			default:
				sliceName := strings.TrimSuffix(strings.TrimPrefix(name, "memory-"), ".json")
				_, validationErr := validateManifestResult(sliceName, raw)
				safe = validationErr == nil && safeEvidenceJSON(raw, forbidden, release)
			}
		}
		if safe {
			continue
		}
		redacted = true
		if err := os.RemoveAll(path); err != nil {
			return errors.New("remove unsafe evidence")
		}
		notice := "removed-evidence.redacted"
		if known && !containsEvidenceForbidden(name, forbidden) {
			notice = strings.TrimSuffix(name, ".redacted") + ".redacted"
		}
		if err := writePrivateEvidence(filepath.Join(dir, notice), []byte(evidenceRedaction)); err != nil {
			return err
		}
	}
	if redacted {
		return ErrEvidenceRedacted
	}
	return nil
}

func knownEvidenceName(name string) bool {
	if name == "removed-evidence.redacted" {
		return true
	}
	name = strings.TrimSuffix(name, ".redacted")
	if name == "workflow-manifest.json" {
		return true
	}
	for _, slice := range []string{"lexical", "curation", "recall", "archive", "concurrency"} {
		if name == "memory-"+slice+".json" || name == "test-"+slice+".log" {
			return true
		}
	}
	return false
}

func evidenceForbidden(dsn string) ([]string, error) {
	parsed, err := url.Parse(dsn)
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") || parsed.Host == "" || parsed.User == nil || parsed.User.Username() == "" || strings.TrimPrefix(parsed.Path, "/") == "" {
		return nil, errors.New("invalid evidence database connection configuration")
	}
	password, _ := parsed.User.Password()
	candidates := []string{dsn, "postgres://", "postgresql://", parsed.Scheme + "://" + parsed.Host, parsed.Host, parsed.Hostname(), parsed.User.Username(), password, strings.TrimPrefix(parsed.Path, "/")}
	forbidden := make([]string, 0, len(candidates))
	for _, value := range candidates {
		if value != "" {
			forbidden = append(forbidden, value)
		}
	}
	return forbidden, nil
}

func containsEvidenceForbidden(value string, forbidden []string) bool {
	for _, part := range forbidden {
		if strings.Contains(value, part) {
			return true
		}
	}
	return false
}

func safeEvidenceJSON(raw []byte, forbidden []string, publicRelease string) bool {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return false
	}
	var scan func(string, any) bool
	scan = func(key string, value any) bool {
		switch typed := value.(type) {
		case string:
			if key == "release" && publicRelease != "" && typed == publicRelease {
				return true
			}
			// Exact versioned schema identities and the validated repository run URL
			// are public constants containing "witself", also the hosted credential.
			if key == "schema" && (typed == ManifestSchemaV1 || typed == ResultSchemaV1 || typed == CurationResultSchemaV1 || typed == RecallResultSchemaV1 || typed == ArchiveResultSchemaV1 || typed == ConcurrencyResultSchemaV1) {
				return true
			}
			if key == "run_url" && strings.HasPrefix(typed, manifestRunURLPrefix) && manifestRunIDPattern.MatchString(strings.TrimPrefix(typed, manifestRunURLPrefix)) {
				typed = strings.TrimPrefix(typed, manifestRunURLPrefix)
			}
			return !containsEvidenceForbidden(typed, forbidden)
		case map[string]any:
			for nestedKey, nestedValue := range typed {
				if containsEvidenceForbidden(nestedKey, forbidden) || !scan(nestedKey, nestedValue) {
					return false
				}
			}
		case []any:
			for _, nestedValue := range typed {
				if !scan("", nestedValue) {
					return false
				}
			}
		}
		return true
	}
	return scan("", value)
}
