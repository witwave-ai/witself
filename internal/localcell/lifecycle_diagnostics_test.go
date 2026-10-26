package localcell_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/localcell"
)

// Keep the same mask order as ci-go-test-json-check.mjs: mask the whole line
// before truncating, including a secret that straddles the display boundary.
var lifecycleDiagnosticMasks = []struct {
	pattern *regexp.Regexp
	replace string
}{
	{regexp.MustCompile(`(?i)[a-f0-9]{64}`), "[MASKED]"},
	{regexp.MustCompile(`(?i)\b(?:pwfile|PGPASSWORD)\b[^\r\n]*`), "[MASKED]"},
	{regexp.MustCompile(`(?i)\bpassword\s*=[^\r\n]*`), "password=[MASKED]"},
	{regexp.MustCompile(`[^\s@/]+:[^\s@/]+@`), "[MASKED]@"},
	{regexp.MustCompile(`[\x00-\x1f\x7f]`), " "},
}

func maskLifecycleDiagnostic(line string) string {
	for _, mask := range lifecycleDiagnosticMasks {
		line = mask.pattern.ReplaceAllString(line, mask.replace)
	}
	runes := []rune(line)
	if len(runes) > 200 {
		line = string(runes[:200])
	}
	return line
}

func lifecycleLogTail(root, component string) []string {
	// Only fixed, test-owned log names are readable through this helper.
	switch component {
	case "server", "worker", "postgres":
	default:
		return []string{"log unavailable"}
	}
	data, err := os.ReadFile(filepath.Join(root, "logs", component+".log"))
	if err != nil {
		return []string{component + " log unavailable"}
	}
	if len(data) == 0 {
		return []string{component + " log empty"}
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) > 40 {
		lines = lines[len(lines)-40:]
	}
	for i, line := range lines {
		lines[i] = maskLifecycleDiagnostic(component + " log: " + line)
	}
	return lines
}

func lifecycleFailureDiagnostics(cell *localcell.Cell, root string, err error) []string {
	lines := []string{maskLifecycleDiagnostic("start cause: " + err.Error())}
	components := []string{"server", "worker"}
	component, cause, state, observationFailed, ok := localcell.ReadinessFailureForTest(err)
	if ok {
		components = []string{component}
		lines = append(lines, "inner cause: "+cause,
			fmt.Sprintf("%s identity before rollback: recorded=%t alive=%t matches=%t exit_observed=%t observation_failed=%t",
				component, state.PID > 1, state.Alive, state.Matches,
				state.PID > 1 && !state.Alive && !observationFailed, observationFailed))
	} else {
		// Non-readiness failures have no pre-rollback observation. Label the
		// current state explicitly so rollback is never mistaken for an exit.
		inspection, inspectErr := cell.Inspect(context.Background())
		for _, name := range components {
			state := inspection.Processes[name]
			lines = append(lines, fmt.Sprintf("%s identity after Start: recorded=%t alive=%t matches=%t exit_observed=false observation_failed=%t",
				name, state.PID > 1, state.Alive, state.Matches, inspectErr != nil))
		}
	}
	for _, name := range append(components, "postgres") {
		lines = append(lines, lifecycleLogTail(root, name)...)
	}
	return lines
}

func startLifecycle(t *testing.T, cell *localcell.Cell, root string) error {
	t.Helper()
	err := cell.Start(t.Context())
	if err != nil {
		for _, line := range lifecycleFailureDiagnostics(cell, root, err) {
			t.Logf("%s", line)
		}
	}
	return err
}

func TestLifecycleDiagnosticMasking(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"digest " + strings.Repeat("a", 64), "digest [MASKED]"},
		{"password=private-value more", "password=[MASKED]"},
		{"PASSWORD =\"private value\" more", "password=[MASKED]"},
		{"--pwfile=\"private path\" more", "--[MASKED]"},
		{"PGPASSWORD=private-value", "[MASKED]"},
		{"postgres://private-user:priv'ate\"sec\\ret@127.0.0.1/db", "postgres://[MASKED]@127.0.0.1/db"},
		{"private-user:private-value@host", "[MASKED]@host"},
		{strings.Repeat("x", 190) + strings.Repeat("B", 64), strings.Repeat("x", 190) + "[MASKED]"},
		{strings.Repeat("x", 250), strings.Repeat("x", 200)},
		{"line\r\t\x00\x1b\x7f", "line     "},
	} {
		if maskLifecycleDiagnostic(tc.input) != tc.want {
			t.Error("lifecycle diagnostic mask or width changed")
		}
	}
}

func TestLifecycleDiagnosticLogTails(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "logs"), 0o700); err != nil {
		t.Fatal("cannot create diagnostic fixture")
	}
	for _, component := range []string{"server", "worker", "postgres"} {
		var data strings.Builder
		for i := range 45 {
			_, _ = fmt.Fprintf(&data, "line %02d password=private-value\r\n", i)
		}
		path := filepath.Join(root, "logs", component+".log")
		if err := os.WriteFile(path, []byte(data.String()), 0o600); err != nil {
			t.Fatal("cannot write diagnostic fixture")
		}
		lines := lifecycleLogTail(root, component)
		if len(lines) != 40 || lines[0] != component+" log: line 05 password=[MASKED] " ||
			lines[39] != component+" log: line 44 password=[MASKED] " {
			t.Fatal("diagnostics did not preserve exactly the last forty masked log lines")
		}
		for _, line := range lines {
			if strings.Contains(line, "private-value") || len([]rune(line)) > 200 {
				t.Fatal("diagnostics disclosed or exceeded the line bound")
			}
		}
	}
	if got := lifecycleLogTail(root, "../run/postgres.password"); len(got) != 1 || got[0] != "log unavailable" {
		t.Fatal("diagnostic reader accepted a non-log path")
	}
}
