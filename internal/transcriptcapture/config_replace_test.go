package transcriptcapture

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReplaceFileAtomicCreatesAndReplaces(t *testing.T) {
	directory := t.TempDir()
	destination := filepath.Join(directory, "config.json")

	for index, contents := range []string{"first\n", "second\n"} {
		source := filepath.Join(directory, "staged-"+contents[:len(contents)-1])
		if err := os.WriteFile(source, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := replaceFileAtomic(source, destination); err != nil {
			t.Fatalf("replace %d: %v", index, err)
		}
		if _, err := os.Stat(source); !os.IsNotExist(err) {
			t.Fatalf("staged file %d remains: %v", index, err)
		}
		raw, err := os.ReadFile(destination)
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != contents {
			t.Fatalf("replacement %d = %q, want %q", index, raw, contents)
		}
	}
}
