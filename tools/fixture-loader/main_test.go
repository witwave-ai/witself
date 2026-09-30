package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if os.Getenv("FIXTURE_LOADER_FAKE_KUBECTL") == "1" {
		os.Exit(fakeKubectlMain(os.Args[1:]))
	}
	os.Exit(m.Run())
}

type validationTransport struct {
	t *testing.T
}

func (v validationTransport) RoundTrip(*http.Request) (*http.Response, error) {
	v.t.Error("invalid flags invoked HTTP")
	return nil, context.Canceled
}

func TestUsageAndFlagValidation(t *testing.T) {
	mark := []string{"mark", "--account", fakeAccount, "--agent-token-file", "<agent file>", "--operator-token-file", "<operator file>", "--expect-cell", "fake-cell"}
	load := append([]string{"load"}, mark[1:]...)
	load = append(load, "--kube-context", "witself-fake-cell")
	cases := []struct {
		name    string
		args    []string
		message string
	}{
		{"no-verb", nil, usage},
		{"unknown-verb", []string{"unknown"}, usage},
		{"unknown-flag", []string{"check", "--unknown"}, usage},
		{"unparsed-flag", []string{"check", "--max-node-fs-percent", "invalid"}, usage},
		{"invalid-boolean", append(append([]string(nil), mark...), "--yes=invalid"), usage},
		{"positional", []string{"check", "unexpected"}, usage},
		{"bad-account", []string{"close", "--account", "invalid", "--operator-token-file", "<operator file>"}, "--account must be an acc_ id"},
		{"neither-mode", load, "give exactly one of --founder-archive-bytes or --entries"},
		{"both-modes", append(append([]string(nil), load...), "--founder-archive-bytes", "1", "--entries", "1"), "give exactly one of --founder-archive-bytes or --entries"},
	}
	for _, verb := range []string{"check", "mark", "load", "measure", "close"} {
		var required [][2]string
		switch verb {
		case "check":
			required = [][2]string{{"kube-context", "witself-fake-cell"}}
		case "mark", "load":
			required = [][2]string{{"account", fakeAccount}, {"agent-token-file", "<agent file>"}, {"operator-token-file", "<operator file>"}, {"expect-cell", "fake-cell"}}
			if verb == "load" {
				required = append(required, [2]string{"kube-context", "witself-fake-cell"})
			}
		case "measure":
			required = [][2]string{{"account", fakeAccount}, {"operator-token-file", "<operator file>"}, {"expect-cell", "fake-cell"}, {"kube-context", "witself-fake-cell"}}
		case "close":
			required = [][2]string{{"account", fakeAccount}, {"operator-token-file", "<operator file>"}}
		}
		args := []string{verb}
		for _, field := range required {
			cases = append(cases, struct {
				name    string
				args    []string
				message string
			}{verb + "-missing-" + field[0], append([]string(nil), args...), "missing required flag --" + field[0]})
			args = append(args, "--"+field[0], field[1])
		}
	}
	for _, rangeCase := range []struct {
		flag    string
		values  []string
		message string
	}{
		{"founder-archive-bytes", []string{"", "0", "-1", "1.2", "invalid", "9223372036854775808"}, "--founder-archive-bytes must be a positive integer"},
		{"entries", []string{"0", "450001", "invalid"}, "--entries must be between 1 and 450000"},
		{"max-write-bytes-per-second", []string{"99999", "2000001"}, "--max-write-bytes-per-second must be between 100000 and 2000000"},
		{"max-measures", []string{"0", "3"}, "--max-measures must be between 1 and 2"},
	} {
		for i, value := range rangeCase.values {
			args := append([]string(nil), load...)
			if rangeCase.flag != "founder-archive-bytes" && rangeCase.flag != "entries" {
				args = append(args, "--entries", "1")
			}
			args = append(args, "--"+rangeCase.flag, value)
			cases = append(cases, struct {
				name    string
				args    []string
				message string
			}{fmt.Sprintf("%s-%d", rangeCase.flag, i), args, rangeCase.message})
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			client := newHTTPClient()
			client.Transport = validationTransport{t: t}
			d := deps{client: client, stdout: &stdout, stderr: &stderr, run: func(context.Context, string, []string) ([]byte, int, error) {
				t.Error("invalid flags invoked subprocess")
				return nil, 1, nil
			}}
			code := cliWithOptions(context.Background(), tc.args, d, productionOptions())
			if code != 2 || stdout.Len() != 0 || stderr.String() != tc.message+"\n" {
				t.Errorf("usage refusal mismatch: code=%d, message matches=%t", code, stderr.String() == tc.message+"\n")
			}
		})
	}
}

func mainTokenFile(t *testing.T, value string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent_x.token")
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal("cannot create fixture credential")
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal("cannot set fixture credential mode")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != mode {
		t.Fatal("fixture credential mode differs")
	}
	return path
}

func TestTokenFileModeRefused(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		mode  os.FileMode
		code  int
	}{
		{"permissive", fakeAgentToken, 0o644, 3},
		{"private", fakeAgentToken, 0o600, 0},
		{"empty", "", 0o600, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeCell(t)
			agentFile := mainTokenFile(t, tc.value, tc.mode)
			operatorFile := mainTokenFile(t, fakeOperatorToken, 0o600)
			var stdout, stderr bytes.Buffer
			d := deps{now: func() time.Time { return f.createdAt.Add(time.Hour) }, client: newHTTPClient(), stdout: &stdout, stderr: &stderr}
			args := []string{"mark", "--account", fakeAccount, "--agent-token-file", agentFile, "--operator-token-file", operatorFile, "--expect-cell", "fake-cell", "--control-plane", f.server.URL, "--yes"}
			code := cliWithOptions(context.Background(), args, d, productionOptions())
			if code != tc.code {
				t.Fatalf("exit %d, want %d", code, tc.code)
			}
			if tc.code == 0 {
				if f.count("create transcript") != 1 || stderr.Len() != 0 {
					t.Error("private credential failed to mark")
				}
				return
			}
			message := "refused: token file " + agentFile + " must not be readable by group or others\n"
			if tc.value == "" {
				message = "refused: cannot read token file " + agentFile + "\n"
			}
			if stderr.String() != message || stdout.Len() != 0 || len(f.requests) != 0 {
				t.Error("mode refusal did not precede network access")
			}
		})
	}
}

func TestSafeTextIdentifierShapes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		want  string
	}{
		{"token-path", "/tmp/agent_x.token", "/tmp/agent_x.token"},
		{"kubeconfig-path", "/tmp/client_x.yaml", "/tmp/client_x.yaml"},
		{"transcript-id", "/tmp/trn_abcdefghijklmnop.yaml", "redacted"},
		{"entry-id", "/tmp/ent_abcdefghijklmnop.yaml", "redacted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := safeText(tc.value); got != tc.want {
				t.Error("identifier shape redaction mismatch")
			}
		})
	}
}

func TestResponseWordIdentifierSubstringsStayRedacted(t *testing.T) {
	for _, value := range []string{"agent_x", "client_x", "trn_x"} {
		if got := responseWord(value); got != "redacted" {
			t.Error("server-supplied identifier substring was not redacted")
		}
	}
}
