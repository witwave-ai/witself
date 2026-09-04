package legal

import (
	"bytes"
	"os"
	"testing"
)

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
