package transcriptcapture

import (
	"os"
	"path/filepath"
	"testing"
)

func statReplacementTestFileByHandle(t *testing.T, path string) os.FileInfo {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := file.Stat()
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func TestReplaceFileAtomicCreatesAndReplaces(t *testing.T) {
	directory := t.TempDir()
	destination := filepath.Join(directory, "config.json")

	for index, contents := range []string{"first\n", "second\n"} {
		source := filepath.Join(directory, "staged-"+contents[:len(contents)-1])
		if err := os.WriteFile(source, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		stagedInfo := statReplacementTestFileByHandle(t, source)
		var previousInfo os.FileInfo
		if index > 0 {
			previousInfo = statReplacementTestFileByHandle(t, destination)
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
		committedInfo, err := os.Stat(destination)
		if err != nil {
			t.Fatal(err)
		}
		if !replacementCommitIdentityMatches(stagedInfo, previousInfo, committedInfo) {
			t.Fatalf("replacement %d has unexpected committed identity", index)
		}
	}
}
