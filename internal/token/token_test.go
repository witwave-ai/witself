package token

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestNewBootstrapFormat(t *testing.T) {
	tok, err := New(KindBootstrap)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok, "witself_boot_") {
		t.Errorf("token %q missing witself_boot_ prefix", tok)
	}
	kind, body, err := Parse(tok)
	if err != nil {
		t.Fatalf("Parse(%q): %v", tok, err)
	}
	if kind != KindBootstrap {
		t.Errorf("kind = %q, want %q", kind, KindBootstrap)
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		t.Fatalf("body not base64url: %v", err)
	}
	if len(raw) != entropyBytes {
		t.Errorf("entropy = %d bytes, want %d", len(raw), entropyBytes)
	}
}

func TestNewUnique(t *testing.T) {
	seen := make(map[string]bool, 100)
	for range 100 {
		tok, err := New(KindBootstrap)
		if err != nil {
			t.Fatal(err)
		}
		if seen[tok] {
			t.Fatalf("duplicate token: %s", tok)
		}
		seen[tok] = true
	}
}

func TestParseRejectsJunk(t *testing.T) {
	for _, bad := range []string{"", "nope", "witself_boot", "witself__body", "x_boot_body", "witself_boot_"} {
		if _, _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) = nil error, want error", bad)
		}
	}
}
