package localcell_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/localcell"
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
	if home, err := os.UserHomeDir(); err == nil && filepath.IsAbs(home) {
		return filepath.Clean(home)
	}
	if current, err := user.Current(); err == nil && filepath.IsAbs(current.HomeDir) {
		return filepath.Clean(current.HomeDir)
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

// Match the adapter's command environment, including its forced session bus.
// A caller-provided bus address must not send diagnostics to another manager.
func nativeServiceDiagnosticCommandEnvironment(home string, lookup func(string) (string, bool)) []string {
	runtimeDir, _ := lookup("XDG_RUNTIME_DIR")
	configHome, _ := lookup("XDG_CONFIG_HOME")
	if configHome == "" {
		configHome = filepath.Join(home, ".config")
	}
	if filepath.IsAbs(configHome) {
		configHome = filepath.Clean(configHome)
	}
	return []string{"XDG_RUNTIME_DIR=" + runtimeDir, "DBUS_SESSION_BUS_ADDRESS=unix:path=" + runtimeDir + "/bus", "HOME=" + home, "XDG_CONFIG_HOME=" + configHome}
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
			command.Env = nativeServiceDiagnosticCommandEnvironment(nativeServiceDiagnosticHome(), os.LookupEnv)
			return command.CombinedOutput()
		}
	}
	return nil, errors.New("native_service_diagnostic_tool_unavailable")
}

// Only manager-reported values from this explicit allowlist may be displayed.
// Format the key separately so the general assignment sanitizer still discards
// any nested environment assignment in the value.
func nativeServiceManagerEnvironment(output []byte, root, home string) string {
	allowed := map[string]bool{"XDG_RUNTIME_DIR": true, "DBUS_SESSION_BUS_ADDRESS": true, "HOME": true, "XDG_CONFIG_HOME": true}
	var lines []string
	for _, line := range strings.Split(string(output), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok && allowed[key] {
			lines = append(lines, key+": "+sanitizeNativeServiceDiagnostic(value, root, home))
		}
	}
	return strings.Join(lines, "\n")
}

func nativeServiceDirectoryDiagnostics(log func(string, ...any), home string) {
	if home == "" {
		log("native service directory metadata: home unavailable")
		return
	}
	for _, suffix := range []string{"", ".config", ".config/systemd", ".config/systemd/user", ".config/systemd/user/default.target.wants"} {
		path := filepath.Join(home, filepath.FromSlash(suffix))
		info, err := os.Lstat(path)
		if err != nil {
			log("native service directory %s metadata error: %v", path, err)
			continue
		}
		// FileInfo.Sys is platform-specific. Read only the numeric Unix Uid
		// field when present, keeping this diagnostic test file cross-platform.
		uid := "unavailable"
		stat := reflect.ValueOf(info.Sys())
		if stat.Kind() == reflect.Pointer && !stat.IsNil() {
			stat = stat.Elem()
			if stat.Kind() == reflect.Struct {
				field := stat.FieldByName("Uid")
				if field.IsValid() && field.CanUint() {
					uid = strconv.FormatUint(field.Uint(), 10)
				}
			}
		}
		log("native service directory %s mode=%04o uid=%s symlink=%t", path, info.Mode().Perm(), uid, info.Mode()&os.ModeSymlink != 0)
	}
}

const nativeServiceCapabilityArgument = "native-service-capability-probe"

// Recheck Plan in a fresh child with exactly the harness's allowed environment.
// The descriptor travels over stdin and this probe performs no lifecycle writes.
func nativeServiceCapabilityDiagnostic(ctx context.Context, descriptor localcell.ServiceDescriptor) ([]byte, error) {
	host, err := os.Executable()
	if err != nil {
		return nil, errors.New("native_service_capability_executable_unavailable")
	}
	data, err := json.Marshal(descriptor)
	if err != nil {
		return nil, errors.New("native_service_capability_descriptor_invalid")
	}
	command := exec.CommandContext(ctx, host, "-test.run=^TestNativeServiceCapabilityDiagnostic$", "--", nativeServiceCapabilityArgument)
	command.Env = nativeServiceChildEnvironment(os.LookupEnv)
	command.Stdin = bytes.NewReader(data)
	return command.CombinedOutput()
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
	nativeServiceDirectoryDiagnostics(log, home)
	for _, command := range []struct {
		name string
		args []string
	}{
		{"systemctl", []string{"--user", "show", label, "--property=Id,LoadState,FragmentPath,User,DropInPaths,ActiveState,SubState,Result,ExecMainStatus,UnitFileState", "--no-pager"}},
		{"systemctl", []string{"--user", "show", "--property=Version,UnitPath,NFailedUnits", "--no-pager"}},
		{"systemctl", []string{"--user", "list-unit-files", label, "--no-pager"}},
		{"systemctl", []string{"--user", "show-environment"}},
		{"journalctl", []string{"--user", "-u", label, "--no-pager", "-n", "40"}},
		{"child-capability", nil},
	} {
		// Cleanup may run after t.Context is canceled. Each diagnostic gets an
		// independent short deadline and cannot prevent the other from running.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		output, err := run(ctx, command.name, command.args...)
		cancel()
		if command.name == "systemctl" && len(command.args) == 2 && command.args[1] == "show-environment" {
			output = []byte(nativeServiceManagerEnvironment(output, root, home))
		}
		log("native service %s diagnostic command: %s", command.name, strings.Join(command.args, " "))
		log("native service %s diagnostic error: %v\n%s", command.name, err, output)
	}
}

func TestNativeServiceInvocationFailure(t *testing.T) {
	const childArgument = "native-service-failure-fixture"
	const reason = "service_registration_failed: enable_now: fixture refusal"
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
		{"systemctl", "--user", "show", label, "--property=Id,LoadState,FragmentPath,User,DropInPaths,ActiveState,SubState,Result,ExecMainStatus,UnitFileState", "--no-pager"},
		{"systemctl", "--user", "show", "--property=Version,UnitPath,NFailedUnits", "--no-pager"},
		{"systemctl", "--user", "list-unit-files", label, "--no-pager"},
		{"systemctl", "--user", "show-environment"},
		{"journalctl", "--user", "-u", label, "--no-pager", "-n", "40"},
		{"child-capability"},
	}
	if !reflect.DeepEqual(commands, wantCommands) {
		t.Error("invocation did not attempt every best-effort diagnostic command")
	}
}

func TestNativeServiceFailureDiagnosticsRejectCallerEnvironment(t *testing.T) {
	const probeArgument = "native-service-caller-environment-probe"
	const childArgument = "native-service-caller-environment-failure"
	const canaryKey = "WITSELF_DIAGNOSTIC_CANARY"
	const reason = "service_registration_failed: enable_now: canary fixture refusal"
	argument := os.Args[len(os.Args)-1]
	if argument == childArgument {
		fmt.Fprintln(os.Stderr, reason)
		os.Exit(23)
	}
	host, err := os.Executable()
	if err != nil {
		t.Fatal("cannot locate test executable for caller-environment diagnostic fixture")
	}
	if argument != probeArgument {
		// Keep even a deliberately broken diagnostic implementation away from
		// the real caller environment. Capture every byte without echoing it.
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			t.Fatal("cannot generate diagnostic environment canary")
		}
		canary := fmt.Sprintf("%x", random)
		t.Setenv(canaryKey, canary)
		command := exec.CommandContext(t.Context(), host, "-test.run=^TestNativeServiceFailureDiagnosticsRejectCallerEnvironment$", "--", probeArgument)
		command.Env = []string{
			"HOME=" + t.TempDir(), "WITSELF_HOME=" + t.TempDir(), "DSH_HOME=" + t.TempDir(),
			"TMPDIR=" + t.TempDir(), canaryKey + "=" + canary,
		}
		var captured bytes.Buffer
		command.Stdout, command.Stderr = &captured, &captured
		err := command.Run()
		if strings.Contains(captured.String(), canary) || strings.Contains(captured.String(), canaryKey) {
			t.Fatal("caller environment reached diagnostic process stdout or stderr")
		}
		if strings.Contains(captured.String(), "test-process environment reached failure diagnostics") {
			t.Fatal("test-process environment reached failure diagnostics")
		}
		if err != nil {
			t.Fatal("hermetic caller-environment diagnostic probe failed")
		}
		return
	}
	canary, ok := os.LookupEnv(canaryKey)
	if !ok || len(canary) != 32 {
		t.Fatal("diagnostic environment canary is missing")
	}
	home, root := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	const label = "witself-cell-canary.service"
	var logged strings.Builder
	var commands [][]string
	run := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 5*time.Second || ctx.Err() != nil {
			t.Error("diagnostic command lacks an independent bounded context")
		}
		commands = append(commands, append([]string{name}, args...))
		return []byte("fixture diagnostic output"), errors.New("diagnostic unavailable")
	}
	err = nativeServiceInvoke(t.Context(), host, root, home, label,
		[]string{"-test.run=^TestNativeServiceFailureDiagnosticsRejectCallerEnvironment$", "--", childArgument},
		func(format string, args ...any) { fmt.Fprintf(&logged, format+"\n", args...) }, run)
	output := logged.String()
	if strings.Contains(output, canary) || strings.Contains(output, canaryKey) {
		t.Fatal("test-process environment reached failure diagnostics")
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 23 {
		t.Fatal("diagnostic fixture did not execute the failing child")
	}
	for _, expected := range []string{"native service process error: exit status 23\n", "native service stderr (last 30 lines):\n" + reason + "\n"} {
		if !strings.Contains(output, expected) {
			t.Error("caller-environment regression omitted required child failure diagnostics")
		}
	}
	if strings.Count(output, "native service directory ") != 5 {
		t.Error("caller-environment regression omitted required directory diagnostics")
	}
	wantCommands := [][]string{
		{"systemctl", "--user", "show", label, "--property=Id,LoadState,FragmentPath,User,DropInPaths,ActiveState,SubState,Result,ExecMainStatus,UnitFileState", "--no-pager"},
		{"systemctl", "--user", "show", "--property=Version,UnitPath,NFailedUnits", "--no-pager"},
		{"systemctl", "--user", "list-unit-files", label, "--no-pager"},
		{"systemctl", "--user", "show-environment"},
		{"journalctl", "--user", "-u", label, "--no-pager", "-n", "40"},
		{"child-capability"},
	}
	if !reflect.DeepEqual(commands, wantCommands) {
		t.Error("caller-environment regression did not attempt every best-effort diagnostic command")
	}
	if strings.Count(output, " diagnostic command: ") != len(wantCommands) || strings.Count(output, "diagnostic error: diagnostic unavailable") != len(wantCommands) {
		t.Error("caller-environment regression omitted best-effort command failure diagnostics")
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
		if name == "systemctl" && len(args) == 2 && args[1] == "show-environment" {
			return []byte("HOME=" + home + "\nUNRELATED_SECRET=" + environmentValue + "\n" + environmentValue), nil
		}
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
		{"systemctl", "--user", "show", label, "--property=Id,LoadState,FragmentPath,User,DropInPaths,ActiveState,SubState,Result,ExecMainStatus,UnitFileState", "--no-pager"},
		{"systemctl", "--user", "show", "--property=Version,UnitPath,NFailedUnits", "--no-pager"},
		{"systemctl", "--user", "list-unit-files", label, "--no-pager"},
		{"systemctl", "--user", "show-environment"},
		{"journalctl", "--user", "-u", label, "--no-pager", "-n", "40"},
		{"child-capability"},
	}
	if !reflect.DeepEqual(commands, wantCommands) {
		t.Error("diagnostics did not attempt every requested read-only diagnostic command")
	}
	output := logged.String()
	if strings.Contains(output, root) || strings.Contains(output, home) || strings.Contains(output, secret) || strings.Contains(output, controlToken) || strings.Contains(output, environmentValue) {
		t.Fatal("diagnostics exposed a planted path, credential, or environment value")
	}
	for _, expected := range []string{"native service process error: exit status 1 <root> <hex> <token> <environment>", "line-05", "line-34", "<home>/.config", "HOME: <home>", "systemctl diagnostic error:", "journalctl diagnostic error:", "child-capability diagnostic error:"} {
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

func TestNativeServiceCapabilityDiagnostic(t *testing.T) {
	if os.Args[len(os.Args)-1] == nativeServiceCapabilityArgument {
		var descriptor localcell.ServiceDescriptor
		if json.NewDecoder(io.LimitReader(os.Stdin, 1<<20)).Decode(&descriptor) != nil {
			fmt.Fprintln(os.Stderr, "native_service_capability_descriptor_invalid")
			os.Exit(2)
		}
		if _, err := localcell.NewServiceAdapter().Plan(t.Context(), descriptor); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if _, err := fmt.Fprintln(os.Stdout, "service_manager_available"); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", "")
	descriptor := localcell.ServiceDescriptor{Version: 1, InstallationID: "fixture", UID: strconv.Itoa(os.Getuid()), Root: t.TempDir(), Adapter: "systemd-user", Scope: "user", Runtime: "local", Payload: localcell.ServicePayload{Host: filepath.Join(t.TempDir(), "host"), Generation: "fixture"}}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	output, err := nativeServiceCapabilityDiagnostic(ctx, descriptor)
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatal("capability probe did not capture the fresh child's refusal")
	}
	want := "service_manager_unavailable"
	if runtime.GOOS == "linux" && os.Getuid() == 0 {
		want = "service_ownership_conflict"
	}
	if !strings.HasPrefix(string(output), want) {
		t.Fatal("capability probe lost the fresh child's wrapped refusal")
	}
	if runtime.GOOS == "linux" && os.Getuid() != 0 && !strings.HasPrefix(string(output), localcell.ErrServiceManagerUnavailable.Error()+": ") {
		t.Fatal("capability probe lost the child's exact capability sub-reason")
	}
}

func TestNativeServiceManagerEnvironmentDiagnostics(t *testing.T) {
	root, home := "/home/fixture/cell", "/home/fixture"
	hex := strings.Repeat("a1", 32)
	token := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0xa7}, 32))
	output := []byte("UNRELATED_SECRET=must-not-print\nHOME=" + home + "\nXDG_CONFIG_HOME=" + home + "/.config\nXDG_RUNTIME_DIR=" + root + "/run\nDBUS_SESSION_BUS_ADDRESS=unix:path=" + root + "/run/bus;" + hex + ";" + token + "\n HOME=invalid-key\nHOME_SUFFIX=must-not-print\nMALFORMED\nHOME=PRIVATE_VALUE=must-not-print\n")
	got := nativeServiceManagerEnvironment(output, root, home)
	want := "HOME: <home>\nXDG_CONFIG_HOME: <home>/.config\nXDG_RUNTIME_DIR: <root>/run\nDBUS_SESSION_BUS_ADDRESS: unix:path=<root>/run/bus;<hex>;<token>\nHOME: <environment>"
	if got != want {
		t.Fatal("manager environment diagnostics did not preserve only sanitized allowlisted values")
	}
}

func TestNativeServiceDirectoryDiagnostics(t *testing.T) {
	home := t.TempDir()
	unitDir := filepath.Join(home, ".config", "systemd", "user")
	wants := filepath.Join(unitDir, "default.target.wants")
	if err := os.MkdirAll(wants, 0o755); err != nil {
		t.Fatal("cannot create directory diagnostic fixture")
	}
	if err := os.Chmod(wants, 0o755); err != nil {
		t.Fatal("cannot set directory diagnostic fixture mode")
	}
	log := func() string {
		var output strings.Builder
		nativeServiceDirectoryDiagnostics(func(format string, values ...any) { fmt.Fprintf(&output, format+"\n", values...) }, home)
		return sanitizeNativeServiceDiagnostic(output.String(), "", home)
	}
	output := log()
	for _, suffix := range []string{"", "/.config", "/.config/systemd", "/.config/systemd/user", "/.config/systemd/user/default.target.wants"} {
		if !strings.Contains(output, "directory <home>"+suffix+" mode=") {
			t.Fatal("directory diagnostics omitted a required path")
		}
	}
	uid := "unavailable"
	if runtime.GOOS != "windows" {
		uid = strconv.Itoa(os.Getuid())
	}
	mode := "0755"
	if runtime.GOOS == "windows" {
		info, err := os.Lstat(wants)
		if err != nil {
			t.Fatal("cannot inspect directory diagnostic fixture")
		}
		mode = fmt.Sprintf("%04o", info.Mode().Perm())
	}
	if !strings.Contains(output, "default.target.wants mode="+mode+" uid="+uid+" symlink=false") {
		t.Fatal("directory diagnostics omitted mode, owner UID, or symlink evidence")
	}
	if runtime.GOOS == "windows" {
		return // Windows may not grant unprivileged symlink creation.
	}
	if err := os.Remove(wants); err != nil {
		t.Fatal("cannot replace directory diagnostic fixture")
	}
	if err := os.Symlink(filepath.Join(home, "missing-target"), wants); err != nil {
		t.Fatal("cannot create symlink diagnostic fixture")
	}
	if output = log(); !strings.Contains(output, "symlink=true") || strings.Contains(output, "default.target.wants metadata error") {
		t.Fatal("directory diagnostics followed the symlink instead of reporting its own metadata")
	}
}

func TestNativeServiceDiagnosticCommandEnvironment(t *testing.T) {
	home, configHome := t.TempDir(), filepath.Join(t.TempDir(), "config")
	values := map[string]string{"XDG_RUNTIME_DIR": "/run/user/fixture", "DBUS_SESSION_BUS_ADDRESS": "unix:path=/wrong/bus", "HOME": "/wrong/home", "PATH": "/wrong/path", "UNRELATED_SECRET": "must-not-inherit"}
	lookup := func(key string) (string, bool) { value, ok := values[key]; return value, ok }
	want := []string{"XDG_RUNTIME_DIR=/run/user/fixture", "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/fixture/bus", "HOME=" + home, "XDG_CONFIG_HOME=" + filepath.Join(home, ".config")}
	if got := nativeServiceDiagnosticCommandEnvironment(home, lookup); !reflect.DeepEqual(got, want) {
		t.Fatal("diagnostic commands did not use the adapter's forced manager environment")
	}
	values["XDG_CONFIG_HOME"] = configHome + string(filepath.Separator) + ".." + string(filepath.Separator) + "config"
	want[3] = "XDG_CONFIG_HOME=" + configHome
	if got := nativeServiceDiagnosticCommandEnvironment(home, lookup); !reflect.DeepEqual(got, want) {
		t.Fatal("diagnostic commands lost the adapter's cleaned explicit configuration path")
	}
}
