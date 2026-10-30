package trust

import (
	"strings"
	"testing"
)

func TestValidInventoryPathProfile(t *testing.T) {
	for _, name := range []string{"bin/tool", "share/café.txt", "한글/file", "a/Ḍ\u0307", "a/\u035C\u0345", strings.Repeat("a", 1024)} {
		if !ValidPath(name) {
			t.Fatal("valid NFC path refused")
		}
	}
	for _, name := range []string{"", ".", "..", "a/./b", "a/../b", "a//b", "/a", "a/", "a\\b", "a\x00b", "C:/file", "file:stream", "cafe\u0301", "\u1100\u1161", "\u212B", "a/\u1E0A\u0323", "a/\u0345\u035C", strings.Repeat("a", 1025), string([]byte{0xff})} {
		if ValidPath(name) {
			t.Fatal("invalid path accepted")
		}
	}
}
func TestPathUnicodeVersionFrozen(t *testing.T) {
	if PathUnicodeVersion != "16.0.0" {
		t.Fatal("normalization version changed without format revision")
	}
	if len(canonicalDecomposition) != 2081 || len(canonicalComposition) != 961 || len(canonicalCombiningClass) != 934 {
		t.Fatalf("normalization table count drift: %d/%d/%d", len(canonicalDecomposition), len(canonicalComposition), len(canonicalCombiningClass))
	}
}
