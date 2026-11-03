package store

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSyncCaptureActivityDiagnosticRedaction(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
	}{
		{"query_structure", " SELECT count(*)\nFROM sync_capture WHERE account_id=$1 ", "SELECT count(*) FROM sync_capture WHERE account_id=$1"},
		{"sql_literal", "SELECT 'synthetic private value'", "SELECT [redacted]"},
		{"escaped_literal", "SELECT 'synthetic ''private'' value'", "SELECT [redacted]"},
		{"backslash_literal", `SELECT E'synthetic \'private value'`, "SELECT E[redacted]"},
		{"ambiguous_backslash_boundary", `SELECT '\', 'synthetic private tail'`, "SELECT [redacted]"},
		{"escaped_literal_tail", `SELECT E'\\', 'synthetic private tail'`, "SELECT E[redacted]"},
		{"quoted_identifier", "SELECT \"synthetic private identifier\"", "SELECT [redacted]"},
		{"dollar_literal", "SELECT $body$synthetic private value$body$", "SELECT [redacted]"},
		{"dollar_nested_tag", "SELECT $outer$hidden $inner$ private tail $outer$", "SELECT [redacted]"},
		{"dollar_case_sensitive", "SELECT $Tag$hidden $tag$ private tail $Tag$", "SELECT [redacted]"},
		{"dollar_unicode_tag", "SELECT $界$synthetic private value$界$", "SELECT [redacted]"},
		{"dollar_unterminated", "SELECT $body$synthetic private value", "SELECT [redacted]"},
		{"literal_unterminated", "SELECT 'synthetic private value", "SELECT [redacted]"},
		{"literal_trailing_escape", "SELECT E'synthetic private value" + `\`, "SELECT E[redacted]"},
		{"identifier_unterminated", `SELECT "synthetic private identifier`, "SELECT [redacted]"},
		{"sql_comment", "SELECT 1 /* synthetic private value */", "SELECT 1 [redacted]"},
		{"nested_comment", "SELECT 1 /* outer /* inner */ private tail */", "SELECT 1 [redacted]"},
		{"comment_unterminated", "SELECT 1 /* synthetic private value", "SELECT 1 [redacted]"},
		{"nested_comment_unterminated", "SELECT 1 /* outer /* inner */ private tail", "SELECT 1 [redacted]"},
		{"comment_inside_literal", "SELECT 'private /* text */ -- tail'", "SELECT [redacted]"},
		{"literal_inside_comment", "SELECT 1 /* 'private' $tag$ tail */", "SELECT 1 [redacted]"},
		{"line_comment", "SELECT 1 -- synthetic private value", "SELECT 1 [redacted]"},
		{"connection", "postgres://synthetic-user:synthetic-password@local.invalid/synthetic", "[redacted]"},
		{"assignment", "password=synthetic-private-value", "[redacted]"},
		{"bearer", "Bearer synthetic-private-value", "[redacted]"},
		{"hexadecimal", strings.Repeat("a", 64), "[redacted]"},
		{"opaque", strings.Repeat("Q", 60), "[redacted]"},
		{"application", "lc_s6c1_fix3_cleanup", "lc_s6c1_fix3_cleanup"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := syncCaptureDiagnosticText(tc.input, 80); got != tc.want {
				t.Fatal("capture activity diagnostic redaction mismatch")
			}
		})
	}
}

func TestSyncCaptureActivityDiagnosticRedactsBeforeTruncation(t *testing.T) {
	// The secret-shaped literal crosses the prefix boundary. Truncating SQL
	// first would remove its closing delimiter and risk partial disclosure.
	input := strings.Repeat("SELECT 1; ", 7) + "'" + strings.Repeat("private ", 20) + "'"
	got := syncCaptureDiagnosticText(input, 80)
	if strings.Contains(got, "private") || !strings.HasSuffix(got, "[redacted]") || utf8.RuneCountInString(got) > 80 {
		t.Fatal("capture activity prefix disclosed a boundary-spanning literal")
	}
	got = syncCaptureDiagnosticText(strings.Repeat("界 ", 50), 80)
	if !utf8.ValidString(got) || utf8.RuneCountInString(got) != 80 {
		t.Fatal("capture activity prefix must bound characters without cutting UTF-8")
	}
}
