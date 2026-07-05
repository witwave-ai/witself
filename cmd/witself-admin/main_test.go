package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/client"
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
	t.Setenv("WITSELF_ADMIN_TOKEN", "")
	if _, err := resolveAdminToken("", ""); err == nil {
		t.Error("no sources should error")
	}
	// env picked up when nothing else set.
	t.Setenv("WITSELF_ADMIN_TOKEN", "from-env")
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

// TestJSONEnvelopes pins the top-level wrapped-envelope shape for
// every --json output witself-admin emits, per #27. Drift here is a
// UX-contract break for TUI (#29) and AI-agent consumers — they
// route on the envelope key, so a renamed key silently drops every
// downstream parser. This test asserts:
//   - the top-level JSON is an object (never a bare struct/array/string)
//   - the expected envelope key is present
//   - no unexpected top-level keys appear
func TestJSONEnvelopes(t *testing.T) {
	tests := []struct {
		name     string
		value    map[string]any
		wantKeys []string
	}{
		{
			name:     "whoami",
			value:    whoamiJSONMap(&client.AdminWhoami{AdminID: "adm_abcd1234", Handle: "sarah"}),
			wantKeys: []string{"admin"},
		},
		{
			name: "support-policy read",
			value: supportPolicyReadJSONMap(&client.SupportPolicyRead{
				AccountID: "acc_1", SupportPolicy: "enabled",
			}),
			wantKeys: []string{"support_policy"},
		},
		{
			name: "support-policy set",
			value: supportPolicyChangeJSONMap(&client.SupportPolicyChange{
				AccountID: "acc_1", PolicyFrom: "enabled", PolicyTo: "disabled",
			}),
			wantKeys: []string{"support_policy_change"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			buf, err := json.Marshal(tc.value)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var got map[string]any
			if err := json.Unmarshal(buf, &got); err != nil {
				t.Fatalf("unmarshal: %v — expected top-level object, got %s", err, string(buf))
			}
			if len(got) != len(tc.wantKeys) {
				t.Errorf("top-level key count = %d, want %d (got keys: %v)",
					len(got), len(tc.wantKeys), keys(got))
			}
			for _, k := range tc.wantKeys {
				if _, ok := got[k]; !ok {
					t.Errorf("missing envelope key %q; got keys: %v", k, keys(got))
				}
			}
			// The domain fields must sit UNDER the envelope, not
			// alongside it. If a caller collapsed the envelope, the
			// domain fields would appear at top level.
			for _, forbidden := range []string{"admin_id", "handle", "account_id", "support_policy", "policy_from", "policy_to"} {
				if _, ok := got[forbidden]; ok && !contains(tc.wantKeys, forbidden) {
					t.Errorf("domain field %q leaked to top level — envelope collapsed", forbidden)
				}
			}
		})
	}
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// TestNewestActivity pins the pure high-water-mark advancement rule
// that `ticket watch` relies on to know what's new between polls.
// Off-by-one here would either drop tickets (bad, silent) or re-emit
// them forever (loud, annoying) — both break the TUI + agent contract.
func TestNewestActivity(t *testing.T) {
	mk := func(rfc string) time.Time {
		v, err := time.Parse(time.RFC3339, rfc)
		if err != nil {
			t.Fatalf("bad time %q: %v", rfc, err)
		}
		return v
	}
	fallback := mk("2026-07-04T12:00:00Z")

	t.Run("empty tickets returns fallback", func(t *testing.T) {
		got := newestActivity(nil, fallback)
		if !got.Equal(fallback) {
			t.Errorf("got %v, want %v", got, fallback)
		}
	})
	t.Run("all older than fallback returns fallback", func(t *testing.T) {
		ts := []client.AdminTicket{
			{SupportTicket: client.SupportTicket{LastActivityAt: mk("2026-07-04T11:00:00Z")}},
			{SupportTicket: client.SupportTicket{LastActivityAt: mk("2026-07-04T11:30:00Z")}},
		}
		got := newestActivity(ts, fallback)
		if !got.Equal(fallback) {
			t.Errorf("got %v, want %v", got, fallback)
		}
	})
	t.Run("newer ticket advances the mark", func(t *testing.T) {
		newer := mk("2026-07-04T13:00:00Z")
		ts := []client.AdminTicket{
			{SupportTicket: client.SupportTicket{LastActivityAt: mk("2026-07-04T11:00:00Z")}},
			{SupportTicket: client.SupportTicket{LastActivityAt: newer}},
			{SupportTicket: client.SupportTicket{LastActivityAt: mk("2026-07-04T12:30:00Z")}},
		}
		got := newestActivity(ts, fallback)
		if !got.Equal(newer) {
			t.Errorf("got %v, want %v", got, newer)
		}
	})
	t.Run("simulated two-tick advancement", func(t *testing.T) {
		// Tick 1: fallback is an early baseline; tickets are newer,
		// so hwm should advance to the newest ticket.
		earlyBaseline := mk("2026-07-04T09:00:00Z")
		t1 := []client.AdminTicket{
			{SupportTicket: client.SupportTicket{LastActivityAt: mk("2026-07-04T10:00:00Z")}},
			{SupportTicket: client.SupportTicket{LastActivityAt: mk("2026-07-04T10:05:00Z")}},
		}
		hwm := newestActivity(t1, earlyBaseline)
		if !hwm.Equal(mk("2026-07-04T10:05:00Z")) {
			t.Fatalf("first-tick hwm = %v, want 10:05:00Z", hwm)
		}
		// Tick 2: server returns a fresh update. hwm advances to it.
		t2 := []client.AdminTicket{
			{SupportTicket: client.SupportTicket{LastActivityAt: mk("2026-07-04T10:10:00Z")}},
		}
		hwm = newestActivity(t2, hwm)
		if !hwm.Equal(mk("2026-07-04T10:10:00Z")) {
			t.Errorf("second-tick hwm = %v, want 10:10:00Z", hwm)
		}
		// Tick 3: empty. hwm stays put.
		hwm = newestActivity(nil, hwm)
		if !hwm.Equal(mk("2026-07-04T10:10:00Z")) {
			t.Errorf("third-tick hwm should not regress: got %v", hwm)
		}
	})

	t.Run("baseline case: fallback wins when all tickets are older", func(t *testing.T) {
		// The initial baseline pattern in ticket watch: fallback is
		// "now - small buffer" from the client. Any pre-existing
		// ticket last_activity_at older than that must NOT advance
		// hwm past client-now — otherwise tick 2 could miss a new
		// ticket that landed between tick 1's fetch and now.
		ts := []client.AdminTicket{
			{SupportTicket: client.SupportTicket{LastActivityAt: mk("2026-07-04T11:00:00Z")}},
			{SupportTicket: client.SupportTicket{LastActivityAt: mk("2026-07-04T11:30:00Z")}},
		}
		got := newestActivity(ts, fallback) // fallback = 12:00
		if !got.Equal(fallback) {
			t.Errorf("baseline: got %v, want fallback %v", got, fallback)
		}
	})
}
