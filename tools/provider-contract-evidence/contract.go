// Command provider-contract-evidence retains only bounded, allowlisted outcomes
// from the existing offline provider fixtures. It is not a Witself client verb.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	cellSchema     = "witself.provider-contract.cell.v1"
	matrixSchema   = "witself.provider-contract.matrix.v1"
	packageName    = "github.com/witwave-ai/witself/cmd/witself"
	installedEnv   = "WITSELF_INSTALLED_COMMAND_ACCEPTANCE_BINARY"
	maxReportBytes = 2 << 20
)

var errInvalidEvidence = errors.New("invalid_evidence")
var targets = []string{"linux-x64", "linux-arm64", "macos-intel", "macos-arm64", "windows-x64"}
var providers = []string{"codex", "claude-code", "grok-build", "cursor", "openclaw", "antigravity", "copilot"}
var nativeTargets = map[string][2]string{
	"linux-x64": {"linux", "amd64"}, "linux-arm64": {"linux", "arm64"},
	"macos-intel": {"darwin", "amd64"}, "macos-arm64": {"darwin", "arm64"}, "windows-x64": {"windows", "amd64"},
}
var shaPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
var shortCommitPattern = regexp.MustCompile(`^[0-9a-f]{7,40}$`)
var versionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:-[A-Za-z0-9.-]+)?(?:\+[A-Za-z0-9.-]+)?$`)
var repoPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,99}/[A-Za-z0-9][A-Za-z0-9_.-]{0,99}$`)
var runPattern = regexp.MustCompile(`^[1-9][0-9]{0,19}$`)
var refPattern = regexp.MustCompile(`^refs/(?:heads/[A-Za-z0-9][A-Za-z0-9._/-]*|pull/[1-9][0-9]*/merge|tags/v[0-9][A-Za-z0-9.+-]*)$`)

type identity struct {
	Repository   string `json:"repository"`
	Workflow     string `json:"workflow"`
	RunID        string `json:"run_id"`
	RunAttempt   int    `json:"run_attempt"`
	SourceRef    string `json:"source_ref"`
	SourceCommit string `json:"source_commit"`
	ReleaseTag   string `json:"release_tag"`
}

type artifact struct {
	Kind                 string `json:"kind"`
	SnapshotVersion      string `json:"snapshot_version"`
	BinaryReportedCommit string `json:"binary_reported_commit"`
	BinaryVCSRevision    string `json:"binary_vcs_revision"`
	ArchiveName          string `json:"archive_name"`
	ArchiveSHA256        string `json:"archive_sha256"`
	BinarySHA256         string `json:"binary_sha256"`
	ChecksumsSHA256      string `json:"checksums_sha256"`
}

type testResult struct {
	Name            string `json:"name"`
	Provider        string `json:"provider"`
	Status          string `json:"status"`
	FailureCategory string `json:"failure_category"`
}
type phase struct {
	Name            string       `json:"name"`
	StartedAt       string       `json:"started_at"`
	CompletedAt     string       `json:"completed_at"`
	ProcessExit     *int         `json:"process_exit"`
	Status          string       `json:"status"`
	FailureCategory string       `json:"failure_category"`
	Tests           []testResult `json:"tests"`
}
type cell struct {
	SchemaVersion        string    `json:"schema_version"`
	Identity             identity  `json:"identity"`
	Target               string    `json:"target"`
	GOOS                 string    `json:"goos"`
	GOARCH               string    `json:"goarch"`
	StartedAt            string    `json:"started_at"`
	CompletedAt          string    `json:"completed_at"`
	RuntimeKind          string    `json:"runtime_kind"`
	VendorVersion        string    `json:"vendor_version"`
	ClientResult         string    `json:"client_result"`
	ModelResult          string    `json:"model_result"`
	PublishedBytesTested bool      `json:"published_bytes_tested"`
	Artifact             *artifact `json:"artifact"`
	Phases               []phase   `json:"phases"`
	Status               string    `json:"status"`
	FailureCategory      string    `json:"failure_category"`
}
type matrixRow struct {
	Provider                string `json:"provider"`
	Target                  string `json:"target"`
	SourceResult            string `json:"source_result"`
	InstalledSnapshotResult string `json:"installed_snapshot_result"`
	ContractResult          string `json:"contract_result"`
	ClientResult            string `json:"client_result"`
	ModelResult             string `json:"model_result"`
}
type aggregate struct {
	SchemaVersion             string      `json:"schema_version"`
	Identity                  identity    `json:"identity"`
	GeneratedAt               string      `json:"generated_at"`
	Status                    string      `json:"status"`
	ExpectedTestOutcomes      int         `json:"expected_test_outcomes"`
	PassedTestOutcomes        int         `json:"passed_test_outcomes"`
	NotApplicableTestOutcomes int         `json:"not_applicable_test_outcomes"`
	Cells                     []cell      `json:"cells"`
	Matrix                    []matrixRow `json:"matrix"`
}

type phaseSpec struct {
	name, selection string
	tests           []testResult
}

func specs() []phaseSpec {
	portable := []testResult{}
	for i, suffix := range []string{"Claude", "Grok", "Cursor", "OpenClaw", "Antigravity", "Copilot"} {
		portable = append(portable, testResult{Name: "TestProviderIntegrationContract" + suffix, Provider: providers[i+1]})
	}
	return []phaseSpec{
		{"source-codex", "^(TestProviderIntegrationContractCodex|TestProviderIntegrationContractCodexRollbackRestoresPriorInstall)$", []testResult{
			{Name: "TestProviderIntegrationContractCodex", Provider: "codex"},
			{Name: "TestProviderIntegrationContractCodexRollbackRestoresPriorInstall", Provider: "codex"},
		}},
		{"source-portable", "^TestProviderIntegrationContract(Claude|Grok|Cursor|OpenClaw|Antigravity|Copilot)$", portable},
		{"installed-snapshot", "^(TestProviderIntegrationContractCodexInstalledCommandMCPStdio|TestProviderIntegrationContract(Claude|Grok|Cursor|OpenClaw|Antigravity|Copilot))$", append([]testResult{{Name: "TestProviderIntegrationContractCodexInstalledCommandMCPStdio", Provider: "codex"}}, portable...)},
	}
}
func validIdentity(i identity) bool {
	if !repoPattern.MatchString(i.Repository) || !slices.Contains([]string{"ci", "release"}, i.Workflow) ||
		!runPattern.MatchString(i.RunID) || i.RunAttempt < 1 || i.RunAttempt > 10000 ||
		!commitPattern.MatchString(i.SourceCommit) || len(i.SourceRef) > 200 || !refPattern.MatchString(i.SourceRef) ||
		strings.Contains(i.SourceRef, "..") || strings.Contains(i.SourceRef, "//") || strings.HasSuffix(i.SourceRef, "/") {
		return false
	}
	if i.ReleaseTag != "" {
		return i.Workflow == "release" && strings.HasPrefix(i.ReleaseTag, "v") && len(i.ReleaseTag) <= 101 &&
			versionPattern.MatchString(i.ReleaseTag[1:]) && i.SourceRef == "refs/tags/"+i.ReleaseTag
	}
	return i.Workflow == "release" || i.SourceRef == "refs/heads/main" || strings.HasPrefix(i.SourceRef, "refs/pull/")
}
func validTime(s string) bool {
	t, e := time.Parse(time.RFC3339Nano, s)
	return e == nil && t.UTC().Format(time.RFC3339Nano) == s
}
func interval(start, end string) bool {
	return validTime(start) && validTime(end) && startTime(start).Compare(startTime(end)) <= 0
}
func startTime(s string) time.Time { t, _ := time.Parse(time.RFC3339Nano, s); return t }
func stamp() string                { return time.Now().UTC().Format(time.RFC3339Nano) }
func archiveName(version, goos, arch string) string {
	ext := ".tar.gz"
	if goos == "windows" {
		ext = ".zip"
	}
	return "witself_" + version + "_" + goos + "_" + arch + ext
}
func validArtifact(a *artifact, c cell) bool {
	return a != nil && a.Kind == "goreleaser_snapshot" && len(a.SnapshotVersion) <= 100 && versionPattern.MatchString(a.SnapshotVersion) &&
		strings.Contains(a.SnapshotVersion, "SNAPSHOT") && shortCommitPattern.MatchString(a.BinaryReportedCommit) && strings.HasPrefix(c.Identity.SourceCommit, a.BinaryReportedCommit) &&
		(a.BinaryVCSRevision == "" || a.BinaryVCSRevision == c.Identity.SourceCommit) &&
		a.ArchiveName == archiveName(a.SnapshotVersion, c.GOOS, c.GOARCH) && shaPattern.MatchString(a.ArchiveSHA256) && shaPattern.MatchString(a.BinarySHA256) && shaPattern.MatchString(a.ChecksumsSHA256)
}
func validateCell(c cell, expected identity) error {
	if !validIdentity(c.Identity) || c.Identity != expected || c.SchemaVersion != cellSchema ||
		!slices.Contains(targets, c.Target) || nativeTargets[c.Target] != [2]string{c.GOOS, c.GOARCH} ||
		!interval(c.StartedAt, c.CompletedAt) || c.RuntimeKind != "fixture" || c.VendorVersion != "unobserved" ||
		c.ClientResult != "not_run" || c.ModelResult != "not_run" || c.PublishedBytesTested ||
		c.Status != "passed" || c.FailureCategory != "none" || !validArtifact(c.Artifact, c) || len(c.Phases) != 3 {
		return errInvalidEvidence
	}
	previous := c.StartedAt
	for index, spec := range specs() {
		p := c.Phases[index]
		if p.Name != spec.name || p.Status != "passed" || p.FailureCategory != "none" || p.ProcessExit == nil || *p.ProcessExit != 0 ||
			!interval(p.StartedAt, p.CompletedAt) || startTime(p.StartedAt).Before(startTime(previous)) || startTime(p.CompletedAt).After(startTime(c.CompletedAt)) || len(p.Tests) != len(spec.tests) {
			return errInvalidEvidence
		}
		previous = p.CompletedAt
		for j, want := range spec.tests {
			got := p.Tests[j]
			status, reason := "passed", "none"
			if want.Provider == "cursor" && c.Target == "windows-x64" {
				status, reason = "not_applicable", "cursor_native_windows_unsupported"
			}
			if got.Name != want.Name || got.Provider != want.Provider || got.Status != status || got.FailureCategory != reason {
				return errInvalidEvidence
			}
		}
	}
	return nil
}
func makeAggregate(cells []cell, i identity, generated string) (aggregate, error) {
	a := aggregate{SchemaVersion: matrixSchema, Identity: i, GeneratedAt: generated, Status: "passed", ExpectedTestOutcomes: 75, PassedTestOutcomes: 73, NotApplicableTestOutcomes: 2}
	if !validIdentity(i) || !validTime(generated) || len(cells) != len(targets) {
		return a, errInvalidEvidence
	}
	for _, target := range targets {
		found := 0
		for _, c := range cells {
			if c.Target == target {
				found++
				if validateCell(c, i) != nil || startTime(generated).Before(startTime(c.CompletedAt)) {
					return a, errInvalidEvidence
				}
				a.Cells = append(a.Cells, c)
			}
		}
		if found != 1 {
			return a, errInvalidEvidence
		}
		for _, provider := range providers {
			status := "passed"
			if target == "windows-x64" && provider == "cursor" {
				status = "not_applicable"
			}
			a.Matrix = append(a.Matrix, matrixRow{provider, target, status, status, status, "not_run", "not_run"})
		}
	}
	return a, nil
}
func validateAggregate(a aggregate) error {
	want, e := makeAggregate(a.Cells, a.Identity, a.GeneratedAt)
	if e != nil || !reflect.DeepEqual(a, want) {
		return errInvalidEvidence
	}
	return nil
}

// Reject unknown fields, duplicate keys, trailing data and unbounded nesting.
// Go's ordinary struct decoder alone accepts duplicate JSON keys.
func decodeStrict(raw []byte, v any) error {
	if len(raw) == 0 || len(raw) > maxReportBytes {
		return errInvalidEvidence
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if scanJSON(d, 0) != nil {
		return errInvalidEvidence
	}
	if _, e := d.Token(); e != io.EOF {
		return errInvalidEvidence
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(v) != nil {
		return errInvalidEvidence
	}
	var shape any
	if json.Unmarshal(raw, &shape) != nil || requiredFields(shape, reflect.TypeOf(v)) != nil {
		return errInvalidEvidence
	}
	return nil
}
func requiredFields(value any, t reflect.Type) error {
	for t.Kind() == reflect.Pointer {
		if value == nil {
			return nil
		}
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct:
		object, ok := value.(map[string]any)
		if !ok || len(object) != t.NumField() {
			return errInvalidEvidence
		}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			v, ok := object[name]
			if !ok || requiredFields(v, f.Type) != nil {
				return errInvalidEvidence
			}
		}
	case reflect.Slice:
		items, ok := value.([]any)
		if !ok {
			return errInvalidEvidence
		}
		for _, v := range items {
			if requiredFields(v, t.Elem()) != nil {
				return errInvalidEvidence
			}
		}
	default:
		if value == nil {
			return errInvalidEvidence
		}
	}
	return nil
}
func scanJSON(d *json.Decoder, depth int) error {
	if depth > 20 {
		return errInvalidEvidence
	}
	token, e := d.Token()
	if e != nil {
		return errInvalidEvidence
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			key, e := d.Token()
			name, ok := key.(string)
			if e != nil || !ok || seen[name] {
				return errInvalidEvidence
			}
			seen[name] = true
			if scanJSON(d, depth+1) != nil {
				return errInvalidEvidence
			}
		}
		end, e := d.Token()
		if e != nil || end != json.Delim('}') {
			return errInvalidEvidence
		}
	case '[':
		for d.More() {
			if scanJSON(d, depth+1) != nil {
				return errInvalidEvidence
			}
		}
		end, e := d.Token()
		if e != nil || end != json.Delim(']') {
			return errInvalidEvidence
		}
	default:
		return errInvalidEvidence
	}
	return nil
}
