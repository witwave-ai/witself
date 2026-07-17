package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

const (
	memoryCloudEvidenceSchema  = "witself.memory-cloud-conformance-evidence.v1"
	memoryCloudEvidencePath    = "WITSELF_MEMORY_CLOUD_EVIDENCE_PATH"
	memoryCloudEvidenceRelease = "WITSELF_MEMORY_CLOUD_RELEASE"
	memoryCloudEvidenceCommit  = "WITSELF_MEMORY_CLOUD_COMMIT"
	memoryCloudEvidenceRunURL  = "WITSELF_MEMORY_CLOUD_RUN_URL"
)

var (
	memoryCloudReleasePattern     = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-(?:0|[1-9][0-9]*|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9][0-9]*|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*))*)?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)
	memoryCloudCommitPattern      = regexp.MustCompile(`^[0-9a-f]{7,64}$`)
	memoryCloudFingerprintPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)
	memoryCloudVersionPattern     = regexp.MustCompile(`^[0-9]{5,6}$`)
)

type managedMemoryCloudDirectedCaseEvidence struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Outcome     string `json:"outcome"`
}

type managedMemoryCloudCertificationEvidence struct {
	Schema        string                                   `json:"schema"`
	GeneratedAt   time.Time                                `json:"generated_at"`
	Release       string                                   `json:"release"`
	Commit        string                                   `json:"commit"`
	RunURL        string                                   `json:"run_url"`
	Outcome       string                                   `json:"outcome"`
	Endpoints     []managedMemoryCloudEndpointEvidence     `json:"endpoints"`
	DirectedCases []managedMemoryCloudDirectedCaseEvidence `json:"directed_cases"`
}

func newManagedMemoryCloudCertificationEvidence() *managedMemoryCloudCertificationEvidence {
	now := time.Now().UTC()
	return &managedMemoryCloudCertificationEvidence{
		Schema: memoryCloudEvidenceSchema, GeneratedAt: now, Outcome: "fail",
		Endpoints:     []managedMemoryCloudEndpointEvidence{},
		DirectedCases: []managedMemoryCloudDirectedCaseEvidence{},
	}
}

func (e *managedMemoryCloudCertificationEvidence) recordCase(source, destination string, passed bool) {
	outcome := "fail"
	if passed {
		outcome = "pass"
	}
	e.DirectedCases = append(e.DirectedCases, managedMemoryCloudDirectedCaseEvidence{
		Source: source, Destination: destination, Outcome: outcome,
	})
}

func (e *managedMemoryCloudCertificationEvidence) write(t *testing.T) {
	t.Helper()
	path := strings.TrimSpace(os.Getenv(memoryCloudEvidencePath))
	if path == "" {
		return
	}
	e.GeneratedAt = time.Now().UTC()
	e.Release = strings.TrimSpace(os.Getenv(memoryCloudEvidenceRelease))
	e.Commit = strings.ToLower(strings.TrimSpace(os.Getenv(memoryCloudEvidenceCommit)))
	e.RunURL = strings.TrimSpace(os.Getenv(memoryCloudEvidenceRunURL))
	e.Outcome = "pass"
	if len(e.Endpoints) != 3 || len(e.DirectedCases) != 9 {
		e.Outcome = "fail"
	}
	for _, directed := range e.DirectedCases {
		if directed.Outcome != "pass" {
			e.Outcome = "fail"
		}
	}
	if err := writeManagedMemoryCloudCertificationEvidence(path, *e); err != nil {
		t.Errorf("write sanitized managed-cloud certification evidence: %v", err)
	}
}

func writeManagedMemoryCloudCertificationEvidence(path string, evidence managedMemoryCloudCertificationEvidence) error {
	if err := validateManagedMemoryCloudCertificationEvidence(evidence); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal managed-cloud certification evidence: %w", err)
	}
	raw = append(raw, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create managed-cloud certification evidence directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".memory-cloud-evidence-*")
	if err != nil {
		return fmt.Errorf("create managed-cloud certification evidence file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("protect managed-cloud certification evidence file: %w", err)
	}
	if _, err := temporary.Write(raw); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write managed-cloud certification evidence file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync managed-cloud certification evidence file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close managed-cloud certification evidence file: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("publish managed-cloud certification evidence file: %w", err)
	}
	return nil
}

func validateManagedMemoryCloudCertificationEvidence(evidence managedMemoryCloudCertificationEvidence) error {
	if evidence.Schema != memoryCloudEvidenceSchema || evidence.GeneratedAt.IsZero() {
		return errors.New("managed-cloud certification evidence envelope is invalid")
	}
	if !memoryCloudReleasePattern.MatchString(evidence.Release) {
		return errors.New("managed-cloud certification release must be an exact semantic-version tag")
	}
	if !memoryCloudCommitPattern.MatchString(evidence.Commit) {
		return errors.New("managed-cloud certification commit must be a hexadecimal revision")
	}
	if !strings.HasPrefix(evidence.RunURL, "https://github.com/witwave-ai/witself/actions/runs/") ||
		strings.ContainsAny(evidence.RunURL, "\r\n\t ") || len(evidence.RunURL) > 256 {
		return errors.New("managed-cloud certification run URL is invalid")
	}
	if evidence.Outcome != "pass" && evidence.Outcome != "fail" {
		return errors.New("managed-cloud certification outcome is invalid")
	}
	if len(evidence.Endpoints) > 3 || len(evidence.DirectedCases) > 9 {
		return errors.New("managed-cloud certification evidence exceeds the bounded matrix")
	}
	if evidence.Outcome == "pass" && (len(evidence.Endpoints) != 3 || len(evidence.DirectedCases) != 9) {
		return errors.New("passing managed-cloud certification evidence must contain three endpoints and nine directed cases")
	}
	canonicalProviders := map[string]bool{"aws": true, "gcp": true, "azure": true}
	seenProviders := make(map[string]bool, 3)
	for _, endpoint := range evidence.Endpoints {
		if safeManagedMemoryCloudProvider(endpoint.Provider) != endpoint.Provider || endpoint.Provider == "unknown" ||
			!memoryCloudFingerprintPattern.MatchString(endpoint.Fingerprint) ||
			!memoryCloudVersionPattern.MatchString(endpoint.PostgreSQLVersion) || seenProviders[endpoint.Provider] {
			return errors.New("managed-cloud certification endpoint evidence is invalid")
		}
		seenProviders[endpoint.Provider] = true
	}
	if evidence.Outcome == "pass" && len(seenProviders) != 3 {
		return errors.New("managed-cloud certification endpoint providers are incomplete")
	}
	wantCases := make(map[string]bool, 9)
	for provider := range canonicalProviders {
		for destination := range canonicalProviders {
			wantCases[provider+"_to_"+destination] = true
		}
	}
	seenCases := make(map[string]bool, 9)
	for _, directed := range evidence.DirectedCases {
		key := directed.Source + "_to_" + directed.Destination
		if !wantCases[key] || seenCases[key] || (directed.Outcome != "pass" && directed.Outcome != "fail") {
			return errors.New("managed-cloud certification directed-case evidence is invalid")
		}
		seenCases[key] = true
	}
	if evidence.Outcome == "pass" && len(seenCases) != len(wantCases) {
		return errors.New("managed-cloud certification directed-case matrix is incomplete")
	}
	return nil
}

func TestWriteManagedMemoryCloudCertificationEvidenceIsSanitizedAndPrivate(t *testing.T) {
	evidence := newManagedMemoryCloudCertificationEvidence()
	evidence.Release = "v0.0.172"
	evidence.Commit = "67ec81d3f5485f1865f87e265ae9f33fa15c6988"
	evidence.RunURL = "https://github.com/witwave-ai/witself/actions/runs/12345"
	evidence.Outcome = "pass"
	evidence.Endpoints = []managedMemoryCloudEndpointEvidence{
		{Provider: "aws", Fingerprint: "1111111111111111", PostgreSQLVersion: "160004"},
		{Provider: "gcp", Fingerprint: "2222222222222222", PostgreSQLVersion: "180000"},
		{Provider: "azure", Fingerprint: "3333333333333333", PostgreSQLVersion: "170002"},
	}
	for _, source := range []string{"aws", "gcp", "azure"} {
		for _, destination := range []string{"aws", "gcp", "azure"} {
			evidence.recordCase(source, destination, true)
		}
	}
	path := filepath.Join(t.TempDir(), "evidence", "result.json")
	if err := writeManagedMemoryCloudCertificationEvidence(path, *evidence); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("evidence mode = %o, want 600", info.Mode().Perm())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"postgres://", "password", "resource_id", "database_url", "host"} {
		if strings.Contains(strings.ToLower(string(raw)), forbidden) {
			t.Fatalf("evidence contains forbidden field %q:\n%s", forbidden, raw)
		}
	}
}

func TestManagedMemoryCloudCertificationEvidenceRejectsUnsafeMetadata(t *testing.T) {
	evidence := newManagedMemoryCloudCertificationEvidence()
	evidence.Release = "postgres://user:password@host/database"
	evidence.Commit = "67ec81d"
	evidence.RunURL = "https://github.com/witwave-ai/witself/actions/runs/12345"
	if err := validateManagedMemoryCloudCertificationEvidence(*evidence); err == nil || !strings.Contains(err.Error(), "release") {
		t.Fatalf("unsafe release error = %v", err)
	}
}

func TestManagedMemoryCloudCertificationReleaseUsesExactSemVer(t *testing.T) {
	for _, release := range []string{
		"v0.0.172",
		"v1.2.3-rc.1+build.7",
		"v1.2.3+build.7",
		"v1.2.3-0",
	} {
		if !memoryCloudReleasePattern.MatchString(release) {
			t.Errorf("valid release %q rejected", release)
		}
	}
	for _, release := range []string{
		"1.2.3",
		"v01.2.3",
		"v1.02.3",
		"v1.2.03",
		"v1.2.3-.",
		"v1.2.3-..",
		"v1.2.3-01",
		"v1.2",
	} {
		if memoryCloudReleasePattern.MatchString(release) {
			t.Errorf("invalid release %q accepted", release)
		}
	}
}
