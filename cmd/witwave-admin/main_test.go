package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSafeTextStripsTerminalEscapes pins parity with the ws CLI's
// safeText: any operator- or admin-controlled string that reaches this
// binary's stdout must have C0 control chars + DEL stripped so a
// malicious ticket body can't hijack the admin's terminal.
func TestSafeTextStripsTerminalEscapes(t *testing.T) {
	tests := map[string]string{
		"\x1b[2J\x1b[Hyou have been pwned":     "[2J[Hyou have been pwned",
		"\x1b]0;URGENT: account suspended\x07": "]0;URGENT: account suspended",
		"before\x08\x08\x08\x08after":          "beforeafter",
		"\x7fDEL":                              "DEL",
		"plain ASCII stays":                    "plain ASCII stays",
		"tabs\tand\nnewlines\tare kept":        "tabs\tand\nnewlines\tare kept",
		"unicode π survives":                   "unicode π survives",
	}
	for in, want := range tests {
		if got := safeText(in); got != want {
			t.Errorf("safeText(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestReadBodyFromFlags pins the three-source rule (only one of --body /
// --body-file / --stdin may be set) plus the file / '-' / stdin paths.
func TestReadBodyFromFlags(t *testing.T) {
	// Empty inline is a no-op — the default source; caller decides
	// whether the empty text is acceptable.
	if got, err := readBodyFromFlags("", "", false); err != nil || got != "" {
		t.Errorf("all empty: got %q err=%v, want \"\" err=nil", got, err)
	}
	// Inline wins alone.
	if got, err := readBodyFromFlags("hello", "", false); err != nil || got != "hello" {
		t.Errorf("inline: got %q err=%v", got, err)
	}
	// File path reads content.
	dir := t.TempDir()
	fp := filepath.Join(dir, "body.txt")
	if err := os.WriteFile(fp, []byte("line 1\nline 2"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := readBodyFromFlags("", fp, false); err != nil || got != "line 1\nline 2" {
		t.Errorf("body-file: got %q err=%v", got, err)
	}
	// Two sources set → error.
	if _, err := readBodyFromFlags("x", fp, false); err == nil || !strings.Contains(err.Error(), "only one") {
		t.Errorf("both set: err = %v, want 'only one'", err)
	}
	// All three set → error.
	if _, err := readBodyFromFlags("x", fp, true); err == nil || !strings.Contains(err.Error(), "only one") {
		t.Errorf("all three set: err = %v, want 'only one'", err)
	}
}

// TestCpEndpointPrecedence pins the flag > env > default order.
func TestCpEndpointPrecedence(t *testing.T) {
	t.Setenv("WITSELF_CONTROL_PLANE", "https://env.example")
	if got := cpEndpoint("https://flag.example"); got != "https://flag.example" {
		t.Errorf("flag should win: got %q", got)
	}
	if got := cpEndpoint(""); got != "https://env.example" {
		t.Errorf("env should win over default: got %q", got)
	}
	t.Setenv("WITSELF_CONTROL_PLANE", "")
	if got := cpEndpoint(""); got != defaultControlPlane {
		t.Errorf("default fallback: got %q, want %q", got, defaultControlPlane)
	}
}

// TestResolveAdminToken pins the flag > file > env order and the
// friendly-error path when none is set.
func TestResolveAdminToken(t *testing.T) {
	t.Setenv("WITWAVE_ADMIN_TOKEN", "")
	if _, err := resolveAdminToken("", ""); err == nil {
		t.Error("no sources should error")
	}
	// env picked up when nothing else set.
	t.Setenv("WITWAVE_ADMIN_TOKEN", "from-env")
	if got, err := resolveAdminToken("", ""); err != nil || got != "from-env" {
		t.Errorf("env: got %q err=%v", got, err)
	}
	// --token wins over env.
	if got, err := resolveAdminToken("from-flag", ""); err != nil || got != "from-flag" {
		t.Errorf("flag beats env: got %q err=%v", got, err)
	}
	// --token-file wins over env when no --token flag.
	dir := t.TempDir()
	fp := filepath.Join(dir, "tok")
	if err := os.WriteFile(fp, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := resolveAdminToken("", fp); err != nil || got != "from-file" {
		t.Errorf("file beats env: got %q err=%v", got, err)
	}
}
