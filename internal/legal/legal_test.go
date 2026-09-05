package legal

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// A version identifies the complete published text, not just its header.
// Register a new fingerprint under a new version for a material change and
// retain the previous document verbatim under docs/legal/versions/<version>/.
func TestPublishedLegalVersionsIdentifyExactText(t *testing.T) {
	var documents map[string]struct {
		File     string            `json:"file"`
		Versions map[string]string `json:"versions"`
	}
	registry, err := os.ReadFile("testdata/published-versions.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(registry, &documents); err != nil {
		t.Fatal(err)
	}
	versionPattern := regexp.MustCompile(`(?m)^\*\*Version (\d{4}-\d{2}-\d{2}) · Effective \d{4}-\d{2}-\d{2}\*\*$`)
	for slug, document := range documents {
		t.Run(slug, func(t *testing.T) {
			current, err := os.ReadFile(filepath.Join("../../docs/legal", document.File))
			if err != nil {
				t.Fatal(err)
			}
			label := versionPattern.FindSubmatch(current)
			if label == nil {
				t.Fatal("published document lacks a canonical version header")
			}
			currentVersion := string(label[1])
			if _, ok := document.Versions[currentVersion]; !ok {
				t.Fatalf("register the exact published text for version %s", currentVersion)
			}
			checkText := func(version string, text []byte) {
				t.Helper()
				label := versionPattern.FindSubmatch(text)
				if label == nil || string(label[1]) != version {
					t.Errorf("document for %s has a different version header", version)
				}
				hash := sha256.Sum256(text)
				if hex.EncodeToString(hash[:]) != document.Versions[version] {
					t.Errorf("published version %s changed text; use a new version and preserve the prior text", version)
				}
			}
			checkText(currentVersion, current)
			for version := range document.Versions {
				archived, err := os.ReadFile(filepath.Join("../../docs/legal/versions", version, document.File))
				if os.IsNotExist(err) && version == currentVersion {
					continue // The current version need not yet have an archive copy.
				}
				if err != nil {
					t.Errorf("preserve published version %s: %v", version, err)
					continue
				}
				checkText(version, archived)
			}
		})
	}
}

func TestConsentVersionsMatchPublishedDocuments(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		version string
	}{
		{name: "terms", path: "../../docs/legal/terms-of-service.md", version: TermsVersion},
		{name: "privacy", path: "../../docs/legal/privacy-policy.md", version: PrivacyVersion},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			published, err := os.ReadFile(test.path)
			if err != nil {
				t.Fatalf("read published document: %v", err)
			}

			labelMarker := []byte("**Version ")
			if count := bytes.Count(published, labelMarker); count != 1 {
				t.Fatalf("published document contains %d Version labels; want exactly 1", count)
			}
			marker := append([]byte("\n\n"), labelMarker...)
			if !bytes.Contains(published, marker) {
				t.Fatal("published Version label is not in the canonical document header")
			}
			label := published[bytes.Index(published, marker)+len(marker):]
			end := bytes.Index(label, []byte(" · "))
			if end < 0 {
				t.Fatal("published Version label is missing the exact \" · \" delimiter")
			}
			if got := label[:end]; !bytes.Equal(got, []byte(test.version)) {
				t.Fatalf("published Version label = %q, compiled-in version = %q; update them together", got, test.version)
			}
		})
	}
}
