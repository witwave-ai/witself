package localcell_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

var nativeServiceDiagnosticHex = regexp.MustCompile(`(?i)[0-9a-f]{64}`)
var nativeServiceDiagnosticToken = regexp.MustCompile(`[A-Za-z0-9_-]{43,}`)
var nativeServiceDiagnosticEnvironment = regexp.MustCompile(`(?m)\b(?:Environment(?:File)?|[A-Z_][A-Z0-9_]*)[ \t]*=[^\r\n]*`)

// A failed interrupt can leave a foreground child writing while cleanup records
// its stderr. Snapshot under the same lock as writes to keep diagnostics safe.
type nativeServiceDiagnosticBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *nativeServiceDiagnosticBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(data)
}

func (b *nativeServiceDiagnosticBuffer) snapshot() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.buffer.Bytes())
}

// Only the explicit runtime selector and user-session prerequisites reach the
// harness. Cell subprocess credentials/configuration remain lifecycle-owned.
func nativeServiceChildEnvironment(lookup func(string) (string, bool)) []string {
	result := []string{"WITSELF_RUNTIME=local"}
	for _, key := range []string{"HOME", "PATH", "XDG_RUNTIME_DIR", "XDG_CONFIG_HOME", "DBUS_SESSION_BUS_ADDRESS", "LANG", "LC_ALL", "TMPDIR"} {
		if value, present := lookup(key); present {
			result = append(result, key+"="+value)
		}
	}
	return result
}

func nativeServiceDiagnosticHome() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return home
	}
	if current, err := user.Current(); err == nil {
		return current.HomeDir
	}
	return ""
}

func sanitizeNativeServiceDiagnostic(text, root, home string) string {
	// Root must be replaced first because it may be nested inside the home.
	if root != "" {
		text = strings.ReplaceAll(text, root, "<root>")
	}
	if home != "" {
		text = strings.ReplaceAll(text, home, "<home>")
	}
	// The property query never requests Environment. A journal line can still
	// contain Environment= or a shell-style uppercase variable assignment;
	// discard the assignment and its remaining line, including quoted spaces.
	text = nativeServiceDiagnosticEnvironment.ReplaceAllString(text, "<environment>")
	text = nativeServiceDiagnosticHex.ReplaceAllString(text, "<hex>")
	// Control tokens encode 32 random bytes as 43 raw-base64url characters.
	// Conservatively redact longer token-shaped runs too, without reading any
	// credential file or depending on a particular token's current value.
	return nativeServiceDiagnosticToken.ReplaceAllString(text, "<token>")
}

func nativeServiceStderrTail(stderr []byte) string {
	lines := strings.Split(strings.TrimSuffix(string(stderr), "\n"), "\n")
	if len(lines) > 30 {
		lines = lines[len(lines)-30:]
	}
	return strings.Join(lines, "\n")
}

type nativeServiceDiagnosticCommand func(context.Context, string, ...string) ([]byte, error)

// Share the actual process/capture path with the hermetic regression, even when
// native lifecycle prerequisites are unavailable on the current host.
func nativeServiceInvoke(ctx context.Context, host, root, home, label string, args []string, logf func(string, ...any), run nativeServiceDiagnosticCommand) error {
	cmd := exec.CommandContext(ctx, host, args...)
	cmd.Env = nativeServiceChildEnvironment(os.LookupEnv)
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = io.Discard, &stderr
	err := cmd.Run()
	if err != nil {
		nativeServiceFailureDiagnostics(logf, root, home, label, err, stderr.Bytes(), run)
	}
	return err
}

func nativeServiceDiagnosticOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	if name == "systemctl" || name == "journalctl" {
		// Match the adapter's trusted tool directories; diagnostics must not run
		// a same-named executable supplied through the caller's PATH.
		for _, dir := range []string{"/usr/bin", "/bin"} {
			binary := filepath.Join(dir, name)
			info, err := os.Stat(binary)
			if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
				continue
			}
			command := exec.CommandContext(ctx, binary, args...)
			command.Env = nativeServiceChildEnvironment(os.LookupEnv)
			return command.CombinedOutput()
		}
	}
	return nil, errors.New("native_service_diagnostic_tool_unavailable")
}

func nativeServiceFailureDiagnostics(logf func(string, ...any), root, home, label string, processErr error, stderr []byte, run nativeServiceDiagnosticCommand) {
	log := func(format string, values ...any) {
		// Sanitize the complete payload first, then give every line its own
		// testing.T source prefix so the CI JSON checker retains diagnostic bodies.
		payload := sanitizeNativeServiceDiagnostic(fmt.Sprintf(format, values...), root, home)
		for _, line := range strings.Split(payload, "\n") {
			logf("%s", line)
		}
	}
	log("native service process error: %v", processErr)
	log("native service stderr (last 30 lines):\n%s", nativeServiceStderrTail(stderr))
	for _, command := range []struct {
		name string
		args []string
	}{
		{"systemctl", []string{"--user", "show", label, "--property=Id,LoadState,FragmentPath,User,DropInPaths,ActiveState,SubState,Result,ExecMainStatus,UnitFileState"}},
		{"journalctl", []string{"--user", "-u", label, "--no-pager", "-n", "40"}},
	} {
		// Cleanup may run after t.Context is canceled. Each diagnostic gets an
		// independent short deadline and cannot prevent the other from running.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		output, err := run(ctx, command.name, command.args...)
		cancel()
		log("native service %s diagnostic error: %v\n%s", command.name, err, output)
	}
}

func TestNativeServiceInvocationFailure(t *testing.T) {
	const childArgument = "native-service-failure-fixture"
	const reason = "service_registration_failed"
	if os.Args[len(os.Args)-1] == childArgument {
		fmt.Fprintln(os.Stderr, reason)
		os.Exit(23)
	}
	home, root := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	host, err := os.Executable()
	if err != nil {
		t.Fatal("cannot locate test executable for failing child")
	}
	const label = "witself-cell-fixture.service"
	var logged strings.Builder
	var commands [][]string
	run := func(_ context.Context, name string, args ...string) ([]byte, error) {
		commands = append(commands, append([]string{name}, args...))
		return nil, errors.New("diagnostic unavailable")
	}
	err = nativeServiceInvoke(t.Context(), host, root, home, label,
		[]string{"-test.run=^TestNativeServiceInvocationFailure$", "--", childArgument},
		func(format string, args ...any) { fmt.Fprintf(&logged, format+"\n", args...) }, run)
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 23 {
		t.Fatal("invocation did not return the failing child's process error")
	}
	if !strings.Contains(logged.String(), "native service process error: exit status 23\n") {
		t.Error("invocation did not forward the process error to the diagnostic callback")
	}
	if !strings.Contains(logged.String(), "native service stderr (last 30 lines):\n"+reason+"\n") {
		t.Error("invocation did not forward captured stderr to the diagnostic callback")
	}
	wantCommands := [][]string{
		{"systemctl", "--user", "show", label, "--property=Id,LoadState,FragmentPath,User,DropInPaths,ActiveState,SubState,Result,ExecMainStatus,UnitFileState"},
		{"journalctl", "--user", "-u", label, "--no-pager", "-n", "40"},
	}
	if !reflect.DeepEqual(commands, wantCommands) {
		t.Error("invocation did not attempt both best-effort diagnostic commands")
	}
}

func TestNativeServiceDiagnosticSanitizer(t *testing.T) {
	root, home := "/home/fixture/private cell '雪'", "/home/fixture"
	for _, token := range []string{strings.Repeat("a1", 32), strings.Repeat("B2", 32)} {
		got := sanitizeNativeServiceDiagnostic(root+"/run "+home+"/.config token="+token, root, home)
		if got != "<root>/run <home>/.config token=<hex>" {
			t.Fatal("diagnostic sanitizer did not redact the planted root, home, and hex value")
		}
	}
	controlToken := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0xa7}, 32))
	if got := sanitizeNativeServiceDiagnostic("control="+controlToken, root, home); got != "control=<token>" {
		t.Fatal("diagnostic sanitizer did not redact a planted raw-base64url control token")
	}
	for _, assignment := range []string{"Environment=\"PRIVATE_VALUE=private value with spaces\"", "EnvironmentFile=/private/config", "LANG=private value with spaces"} {
		if got := sanitizeNativeServiceDiagnostic("journal: "+assignment+"\nResult=exit-code", root, home); got != "journal: <environment>\nResult=exit-code" {
			t.Fatal("diagnostic sanitizer exposed an environment assignment or changed a requested property")
		}
	}
	if got := sanitizeNativeServiceDiagnostic("service_registration_failed", "", ""); got != "service_registration_failed" {
		t.Fatal("diagnostic sanitizer changed a fixed reason")
	}
}

func TestNativeServiceChildEnvironment(t *testing.T) {
	values := map[string]string{
		"HOME": "/home/fixture", "PATH": "/usr/bin:/bin", "XDG_RUNTIME_DIR": "/run/user/fixture", "XDG_CONFIG_HOME": "/home/fixture/.config",
		"DBUS_SESSION_BUS_ADDRESS": "unix:path=/run/user/fixture/bus", "LANG": "C.UTF-8", "LC_ALL": "", "TMPDIR": "/tmp/fixture",
		"WITSELF_RUNTIME": "hosted", "UNRELATED_SECRET": "must-not-inherit",
	}
	got := nativeServiceChildEnvironment(func(key string) (string, bool) { value, ok := values[key]; return value, ok })
	want := []string{"WITSELF_RUNTIME=local", "HOME=/home/fixture", "PATH=/usr/bin:/bin", "XDG_RUNTIME_DIR=/run/user/fixture", "XDG_CONFIG_HOME=/home/fixture/.config", "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/fixture/bus", "LANG=C.UTF-8", "LC_ALL=", "TMPDIR=/tmp/fixture"}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("child environment did not preserve exactly the requested whitelist")
	}
	got = nativeServiceChildEnvironment(func(string) (string, bool) { return "", false })
	if !reflect.DeepEqual(got, []string{"WITSELF_RUNTIME=local"}) {
		t.Fatal("child environment added unset session variables")
	}
}

func TestNativeServiceFailureDiagnostics(t *testing.T) {
	root, home, label := "/home/fixture/cell", "/home/fixture", "witself-cell-fixture.service"
	secret := strings.Repeat("d3", 32)
	controlToken := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0xa7}, 32))
	environmentValue := "private environment value with spaces"
	var stderr strings.Builder
	for line := 0; line < 35; line++ {
		fmt.Fprintf(&stderr, "line-%02d %s %s %s LANG=%s\n", line, root, secret, controlToken, environmentValue)
	}
	var logged strings.Builder
	var commands [][]string
	run := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 5*time.Second || ctx.Err() != nil {
			t.Error("diagnostic command lacks an independent bounded context")
		}
		commands = append(commands, append([]string{name}, args...))
		return []byte(home + "/.config " + root + " " + secret + " " + controlToken + "\nEnvironment=PRIVATE_VALUE=" + environmentValue), errors.New("diagnostic failed at " + root + " " + secret + " " + controlToken + " LANG=" + environmentValue)
	}
	nativeServiceFailureDiagnostics(func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		if strings.Contains(line, "\n") {
			t.Error("diagnostic callback contains multiple lines and would lose its CI source prefixes")
		}
		fmt.Fprintln(&logged, line)
	}, root, home, label, errors.New("exit status 1 "+root+" "+secret+" "+controlToken+" LANG="+environmentValue), []byte(stderr.String()), run)
	wantCommands := [][]string{
		{"systemctl", "--user", "show", label, "--property=Id,LoadState,FragmentPath,User,DropInPaths,ActiveState,SubState,Result,ExecMainStatus,UnitFileState"},
		{"journalctl", "--user", "-u", label, "--no-pager", "-n", "40"},
	}
	if !reflect.DeepEqual(commands, wantCommands) {
		t.Error("diagnostics did not attempt both requested read-only manager commands")
	}
	output := logged.String()
	if strings.Contains(output, root) || strings.Contains(output, home) || strings.Contains(output, secret) || strings.Contains(output, controlToken) || strings.Contains(output, environmentValue) {
		t.Fatal("diagnostics exposed a planted path, credential, or environment value")
	}
	for _, expected := range []string{"native service process error: exit status 1 <root> <hex> <token> <environment>", "line-05", "line-34", "<home>/.config", "systemctl diagnostic error:", "journalctl diagnostic error:"} {
		if !strings.Contains(output, expected) {
			t.Error("diagnostics omitted required sanitized evidence")
		}
	}
	for line := 0; line < 5; line++ {
		if strings.Contains(output, fmt.Sprintf("line-%02d", line)) {
			t.Error("stderr diagnostic exceeded its last-30-lines boundary")
		}
	}
}
