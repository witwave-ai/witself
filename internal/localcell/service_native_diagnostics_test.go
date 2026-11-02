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
var nativeServiceDiagnosticToken = regexp.MustCompile(`[A-Za-z0-9_+/-]{43,}={0,2}`)
var nativeServiceDiagnosticEnvironment = regexp.MustCompile(`(?m)\b(?:Environment(?:File)?|[A-Z_][A-Z0-9_]*)[ \t]*=[^\r\n]*`)
var nativeServiceLaunchdAssignment = regexp.MustCompile(`^(.+?)([ \t]*(?:=>|=)[ \t]*)(.*)$`)
var nativeServiceLaunchdKey = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9 _()-]*$`)

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
	// Cover both standard and URL-safe base64, after replacing root/home so
	// those placeholders survive. Long plain path segments may also redact as
	// tokens: privacy takes precedence over that diagnostic detail. Hex is
	// replaced first so its already value-free placeholder remains intact.
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
	if name == "launchctl" {
		const binary = "/bin/launchctl"
		info, err := os.Stat(binary)
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			command := exec.CommandContext(ctx, binary, args...)
			command.Env = nativeServiceLaunchdCommandEnvironment(nativeServiceDiagnosticHome())
			return command.Output()
		}
	}
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

func nativeServiceLaunchdCommandEnvironment(home string) []string {
	return []string{"HOME=" + home}
}

// Keep parser-shape evidence in successful native CI rounds as well as failed
// rounds. This is a read-only observation after the adapter verified ownership.
func nativeServiceLaunchdRegistrationDiagnostics(uid int, logf func(string, ...any), root, home, label string, run nativeServiceDiagnosticCommand) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output, err := run(ctx, "launchctl", "print", "gui/"+strconv.Itoa(uid)+"/"+label)
	payload := sanitizeNativeServiceDiagnostic(fmt.Sprintf("native service launchctl registration observation error: %v\nnative service launchctl print shape:", err), root, home)
	// Shape values are already removed. Do not apply the environment-assignment
	// sanitizer to it: that would erase structural fields such as "ID = <value>".
	payload += "\n" + nativeServiceLaunchdPrintShape(output)
	for _, line := range strings.Split(payload, "\n") {
		// Preserve launchctl's indentation inside each source-prefixed CI line;
		// the shape formatter has already replaced every selected field value.
		logf("%s", line)
	}
}

// launchctl(1), print: this output is not an API. Preserve its full structure
// for CI parser evidence, including unknown dictionaries, without field values.
// reports/LC-S7C1-owner-premise-evidence.md, read-only probes, "Real print shape"
// describes the observed outer frame, raw argument lines and nested dictionaries.
// Environment entry names are private data too; retain their delimiters but use
// <key>, while retaining structural dictionary names outside environment blocks.
func nativeServiceLaunchdPrintShape(output []byte) string {
	type block struct {
		indent      string
		arguments   bool
		environment bool
		nextArg     int
	}
	var blocks []block
	lines := strings.Split(string(output), "\n")
	for index, line := range lines {
		trimmed := strings.TrimSpace(line)
		indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
		if len(blocks) > 0 && blocks[len(blocks)-1].arguments {
			current := &blocks[len(blocks)-1]
			if trimmed == "}" && indent == current.indent {
				blocks = blocks[:len(blocks)-1]
			} else {
				// The observed format adds one tab to the block indentation.
				// Whitespace after that prefix belongs to the argument value:
				// an argument containing spaces is not an empty argument.
				argument := line
				if prefix := current.indent + "\t"; strings.HasPrefix(line, prefix) {
					indent, argument = prefix, strings.TrimPrefix(line, prefix)
				}
				empty := ""
				if argument == "" {
					empty = ":empty"
				}
				lines[index] = fmt.Sprintf("%s<arg:%d%s>", indent, current.nextArg, empty)
				current.nextArg++
			}
			continue
		}
		if trimmed == "" {
			continue // Blank lines, including the final newline, are evidence too.
		}
		if trimmed == "}" {
			if len(blocks) > 0 {
				blocks = blocks[:len(blocks)-1]
			}
			continue
		}
		parts := nativeServiceLaunchdAssignment.FindStringSubmatch(strings.TrimPrefix(line, indent))
		if parts == nil {
			lines[index] = indent + "<value>"
			if trimmed == "{" {
				lines[index] = line
				blocks = append(blocks, block{indent: indent, environment: len(blocks) > 0 && blocks[len(blocks)-1].environment})
			}
			continue
		}
		key, delimiter, value := parts[1], parts[2], strings.TrimSpace(parts[3])
		environment := len(blocks) > 0 && blocks[len(blocks)-1].environment
		shownKey := key
		if len(blocks) == 0 && strings.HasPrefix(key, "gui/") {
			shownKey = "gui/<uid>"
			if strings.Count(key, "/") > 1 {
				shownKey += "/<label>"
			}
		} else if environment || !nativeServiceLaunchdKey.MatchString(key) {
			shownKey = "<key>"
		}
		shownKey = nativeServiceDiagnosticHex.ReplaceAllString(shownKey, "<hex>")
		shownKey = nativeServiceDiagnosticToken.ReplaceAllString(shownKey, "<token>")
		shownValue := "<value>"
		if value == "{" {
			shownValue = "{"
			blocks = append(blocks, block{
				indent:      indent,
				arguments:   key == "arguments" && !environment,
				environment: environment || key == "environment" || strings.HasSuffix(key, " environment"),
			})
		} else if key == "path" || key == "program" {
			shownValue = "<path>"
		}
		lines[index] = indent + shownKey + delimiter + shownValue
	}
	if len(output) == 0 {
		return "launchctl print: no recognized diagnostic fields"
	}
	return strings.Join(lines, "\n")
}

func nativeServiceLaunchdDirectoryDiagnostics(log func(string, ...any), home string) {
	if home == "" {
		log("native service directory metadata: home unavailable")
		return
	}
	for _, suffix := range []string{"", "Library", "Library/LaunchAgents"} {
		path := filepath.Join(home, filepath.FromSlash(suffix))
		info, err := os.Lstat(path)
		if err != nil {
			log("native service directory %s metadata unavailable", path)
			return
		}
		log("native service directory %s mode=%04o symlink=%t", path, info.Mode().Perm(), info.Mode()&os.ModeSymlink != 0)
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return
		}
	}
	directory := filepath.Join(home, "Library", "LaunchAgents")
	entries, err := os.ReadDir(directory)
	if err != nil {
		log("native service LaunchAgents entries unavailable")
		return
	}
	for index, entry := range entries {
		if index == 128 {
			log("native service LaunchAgents entries truncated")
			return
		}
		info, err := os.Lstat(filepath.Join(directory, entry.Name()))
		if err != nil {
			log("native service LaunchAgents entry metadata unavailable")
			continue
		}
		log("native service LaunchAgents entry %s mode=%04o", entry.Name(), info.Mode().Perm())
	}
}

func nativeServiceLaunchdStderr(root string) ([]byte, error) {
	directory, err := os.Lstat(filepath.Join(root, "service"))
	if err != nil || !directory.IsDir() || directory.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("native_service_host_stderr_unavailable")
	}
	path := filepath.Join(root, "service", "host-stderr.log")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("native_service_host_stderr_unavailable")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("native_service_host_stderr_unavailable")
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.New("native_service_host_stderr_replaced")
	}
	const limit = 64 << 10
	offset := max(int64(0), opened.Size()-limit)
	data, err := io.ReadAll(io.NewSectionReader(file, offset, limit))
	if err != nil {
		return nil, errors.New("native_service_host_stderr_unreadable")
	}
	if offset > 0 {
		// Drop the potentially partial first line before sanitization so a
		// boundary cannot turn a long credential into a short visible suffix.
		if newline := bytes.IndexByte(data, '\n'); newline >= 0 {
			data = data[newline+1:]
		} else {
			data = nil
		}
	}
	return data, nil
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
	nativeServiceFailureDiagnosticsForGOOS(runtime.GOOS, os.Getuid(), logf, root, home, label, processErr, stderr, run)
}

func nativeServiceFailureDiagnosticsForGOOS(goos string, uid int, logf func(string, ...any), root, home, label string, processErr error, stderr []byte, run nativeServiceDiagnosticCommand) {
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
	commands := []struct {
		name string
		args []string
	}{
		{"systemctl", []string{"--user", "show", label, "--property=Id,LoadState,FragmentPath,User,DropInPaths,ActiveState,SubState,Result,ExecMainStatus,UnitFileState", "--no-pager"}},
		{"systemctl", []string{"--user", "show", "--property=Version,UnitPath,NFailedUnits", "--no-pager"}},
		{"systemctl", []string{"--user", "list-unit-files", label, "--no-pager"}},
		{"systemctl", []string{"--user", "show-environment"}},
		{"journalctl", []string{"--user", "-u", label, "--no-pager", "-n", "40"}},
		{"child-capability", nil},
	}
	if goos == "darwin" {
		nativeServiceLaunchdDirectoryDiagnostics(log, home)
		hostStderr, err := nativeServiceLaunchdStderr(root)
		log("native service host stderr error: %v", err)
		log("native service host stderr (last 30 lines):\n%s", nativeServiceStderrTail([]byte(sanitizeNativeServiceDiagnostic(string(hostStderr), root, home))))
		domain := "gui/" + strconv.Itoa(uid)
		commands = []struct {
			name string
			args []string
		}{
			{"launchctl", []string{"print", domain + "/" + label}},
			{"launchctl", []string{"print", domain}},
			{"child-capability", nil},
		}
	} else {
		nativeServiceDirectoryDiagnostics(log, home)
	}
	for _, command := range commands {
		// Cleanup may run after t.Context is canceled. Each diagnostic gets an
		// independent short deadline and cannot prevent the other from running.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		output, err := run(ctx, command.name, command.args...)
		cancel()
		if command.name == "systemctl" && len(command.args) == 2 && command.args[1] == "show-environment" {
			output = []byte(nativeServiceManagerEnvironment(output, root, home))
		}
		if command.name == "launchctl" {
			output = []byte(nativeServiceLaunchdPrintShape(output))
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
				log("native service launchctl stderr:\n%s", exitErr.Stderr)
			}
		}
		log("native service %s diagnostic command: %s", command.name, strings.Join(command.args, " "))
		if command.name == "launchctl" {
			log("native service %s diagnostic error: %v", command.name, err)
			// The formatter removed all values. Keep structural uppercase keys
			// such as ID instead of treating them as environment assignments.
			for _, line := range strings.Split(string(output), "\n") {
				logf("%s", line)
			}
		} else {
			log("native service %s diagnostic error: %v\n%s", command.name, err, output)
		}
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
	if runtime.GOOS == "darwin" {
		domain := "gui/" + strconv.Itoa(os.Getuid())
		wantCommands = [][]string{{"launchctl", "print", domain + "/" + label}, {"launchctl", "print", domain}, {"child-capability"}}
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
	directoryCount := 5
	if runtime.GOOS == "darwin" {
		directoryCount = 2 // The private fixture has no Library directory.
	}
	if strings.Count(output, "native service directory ") != directoryCount {
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
	if runtime.GOOS == "darwin" {
		domain := "gui/" + strconv.Itoa(os.Getuid())
		wantCommands = [][]string{{"launchctl", "print", domain + "/" + label}, {"launchctl", "print", domain}, {"child-capability"}}
	}
	if !reflect.DeepEqual(commands, wantCommands) {
		t.Error("caller-environment regression did not attempt every best-effort diagnostic command")
	}
	if strings.Count(output, " diagnostic command: ") != len(wantCommands) || strings.Count(output, "diagnostic error: diagnostic unavailable") != len(wantCommands) {
		t.Error("caller-environment regression omitted best-effort command failure diagnostics")
	}
}

func nativeServiceStandardBase64TokenFixture(t *testing.T) string {
	t.Helper()
	data := bytes.Repeat([]byte{0xfb, 0xff, 0xff}, 13)[:37]
	token := base64.RawStdEncoding.EncodeToString(data)
	decoded, err := base64.RawStdEncoding.DecodeString(token)
	if len(token) != 50 || !strings.Contains(token, "+") || !strings.Contains(token, "/") || err != nil || !bytes.Equal(data, decoded) {
		t.Fatal("standard-base64 fixture is not a canonical 50-character token containing both alphabet symbols")
	}
	return token
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
	standardToken := nativeServiceStandardBase64TokenFixture(t)
	for _, token := range []string{standardToken, standardToken + "=="} {
		if got := sanitizeNativeServiceDiagnostic("control="+token, root, home); got != "control=<token>" {
			t.Fatal("diagnostic sanitizer did not redact a planted standard-base64 token")
		}
	}
	longHome := "/home/" + strings.Repeat("fixture", 8)
	longRoot := longHome + "/private-cell"
	if got := sanitizeNativeServiceDiagnostic(longRoot+"/run "+longHome+"/.config "+standardToken, longRoot, longHome); got != "<root>/run <home>/.config <token>" {
		t.Fatal("token redaction consumed a root or home before its placeholder replacement")
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
	nativeServiceFailureDiagnosticsForGOOS("linux", 1000, func(format string, args ...any) {
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
	if runtime.GOOS == "darwin" {
		// A forbidden root owner is deterministic even on a runner with a GUI
		// domain. The fresh child must refuse before any launchctl invocation.
		descriptor.Adapter, descriptor.UID = "launchd-agent", "0"
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	output, err := nativeServiceCapabilityDiagnostic(ctx, descriptor)
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatal("capability probe did not capture the fresh child's refusal")
	}
	want := "service_manager_unavailable"
	if runtime.GOOS == "darwin" || (runtime.GOOS == "linux" && os.Getuid() == 0) {
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

func TestNativeServiceLaunchdCommandEnvironment(t *testing.T) {
	home := t.TempDir()
	if got := nativeServiceLaunchdCommandEnvironment(home); !reflect.DeepEqual(got, []string{"HOME=" + home}) {
		t.Fatal("launchd diagnostic environment exceeds the adapter's HOME-only contract")
	}
}

func TestNativeServiceLaunchdPrintShapeObserved(t *testing.T) {
	// reports/LC-S7C1-owner-premise-evidence.md, read-only probes, "Real print
	// shape": this is the owner capture, including tabs and blank lines.
	fixture, err := os.ReadFile(filepath.Join("testdata", "launchd-print-observed.txt"))
	if err != nil {
		t.Fatal("cannot read observed launchctl print fixture")
	}
	input := strings.NewReplacer("<home>", "/fixture/home", "<uid>", "1001", "<asid>", "42", "<n>", "7").Replace(string(fixture))
	const want = `gui/<uid>/<label> = {
	active count = <value>
	path = <path>
	type = <value>
	state = <value>

	program = <path>
	arguments = {
		<arg:0>
		<arg:1>
		<arg:2>
		<arg:3>
		<arg:4>
		<arg:5>
		<arg:6>
		<arg:7>
	}

	working directory = <value>

	stdin path = <value>
	stdout path = <value>
	stderr path = <value>
	inherited environment = {
		<key> => <value>
	}

	default environment = {
		<key> => <value>
	}

	environment = {
		<key> => <value>
		<key> => <value>
	}

	domain = <value>
	umask = <value>
	asid = <value>
	minimum runtime = <value>
	exit timeout = <value>
	runs = <value>
	pid = <value>
	immediate reason = <value>
	forks = <value>
	execs = <value>
	initialized = <value>
	trampolined = <value>
	started suspended = <value>
	proxy started suspended = <value>
	checked allocations = <value>
	checked allocations reason = <value>
	checked allocations flags = <value>
	last exit code = <value>

	resource coalition = {
		ID = <value>
		type = <value>
		state = <value>
		active count = <value>
		name = <value>
	}

	jetsam coalition = {
		ID = <value>
		type = <value>
		state = <value>
		active count = <value>
		name = <value>
	}

	spawn type = <value>
	jetsam priority = <value>
	jetsam memory limit (active) = <value>
	jetsam memory limit (inactive) = <value>
	jetsamproperties category = <value>
	jetsam thread limit = <value>
	cpumon = <value>

	properties = <value>
}
`
	if got := nativeServiceLaunchdPrintShape([]byte(input)); got != want {
		t.Fatal("observed launchctl diagnostic shape lost framing, indentation, delimiters, blank lines, fields, or arguments")
	}
}

func TestNativeServiceLaunchdPrintShapeNestedAndEmptyArguments(t *testing.T) {
	// The observed fixture has no empty argument. Keep an adversarial structural
	// fixture so an empty line or an argument that resembles a brace stays visible.
	input := "gui/1001/fixture = {\n\targuments = {\n\t\t/bin/sh\n\t\t\n\t\t \t \n\n\t\t}\n\t\tkey = {\n\t}\n\n\tunknown structure = {\n\t\tnested\t=>\t{\n\t\t\tID = private value\n\t\t}\n\t}\n\tenvironment = {\n\t\tPRIVATE_NAME => private value\n\t}\n}\n"
	want := "gui/<uid>/<label> = {\n\targuments = {\n\t\t<arg:0>\n\t\t<arg:1:empty>\n\t\t<arg:2>\n<arg:3:empty>\n\t\t<arg:4>\n\t\t<arg:5>\n\t}\n\n\tunknown structure = {\n\t\tnested\t=>\t{\n\t\t\tID = <value>\n\t\t}\n\t}\n\tenvironment = {\n\t\t<key> => <value>\n\t}\n}\n"
	if got := nativeServiceLaunchdPrintShape([]byte(input)); got != want {
		t.Fatal("launchctl diagnostic shape lost unknown nested structure, delimiters, indentation, or empty argument positions")
	}
}

func TestNativeServiceLaunchdPrintShapePrivateKeys(t *testing.T) {
	hex, token := strings.Repeat("a1", 32), strings.Repeat("x", 48)
	standardToken := nativeServiceStandardBase64TokenFixture(t)
	input := "gui/1001/fixture = {\n\t/private/key = value\n\t" + hex + " = value\n\t" + token + " = value\n\t" + standardToken + " = value\n\tbad\rkey = value\n}\n"
	want := "gui/<uid>/<label> = {\n\t<key> = <value>\n\t<hex> = <value>\n\t<token> = <value>\n\t<key> = <value>\n\t<key> = <value>\n}\n"
	if got := nativeServiceLaunchdPrintShape([]byte(input)); got != want {
		t.Fatal("launchctl diagnostic shape exposed a path, token, hex value, or control character in a key")
	}
}

func TestNativeServiceLaunchdPrintShapeZeroSpaceDelimiters(t *testing.T) {
	// Even a malformed or future spacing variant should show the delimiters
	// the parser received, while every scalar value remains redacted.
	input := "gui/1001/fixture={\n\tunknown={\n\t\tchild=>{\n\t\t\titem=private scalar\n\t\t}\n\t}\n}\n"
	want := "gui/<uid>/<label>={\n\tunknown={\n\t\tchild=>{\n\t\t\titem=<value>\n\t\t}\n\t}\n}\n"
	if got := nativeServiceLaunchdPrintShape([]byte(input)); got != want {
		t.Fatal("launchctl diagnostic shape lost zero-space delimiters or nested structure")
	}
}

func TestNativeServiceLaunchdRegistrationDiagnostics(t *testing.T) {
	const label = "ai.witwave.witself.cell.fixture"
	home, root := t.TempDir(), t.TempDir()
	var logged strings.Builder
	var commands [][]string
	run := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 5*time.Second || ctx.Err() != nil {
			t.Error("successful registration observation lacks an independent bounded context")
		}
		commands = append(commands, append([]string{name}, args...))
		return []byte("gui/501/fixture = {\n\tpath = " + home + "/Library/LaunchAgents/fixture.plist\n\tprogram = /private/unlisted-host\n\targuments = {\n\t\t/private/unlisted-host\n\t\t" + root + "\n\t}\n\tenvironment = {\n\t\tUNRELATED_SECRET => private environment value\n\t}\n\tresource coalition = {\n\t\tID = 7\n\t}\n}"), nil
	}
	nativeServiceLaunchdRegistrationDiagnostics(501, func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		if strings.Contains(line, "\n") {
			t.Error("registration observation would lose CI source prefixes")
		}
		fmt.Fprintln(&logged, line)
	}, root, home, label, run)
	if !reflect.DeepEqual(commands, [][]string{{"launchctl", "print", "gui/501/" + label}}) {
		t.Fatal("successful registration observation queried more than the exact service")
	}
	output := logged.String()
	for _, private := range []string{root, home, "/private/unlisted-host", "UNRELATED_SECRET", "private environment value"} {
		if strings.Contains(output, private) {
			t.Fatal("successful registration observation exposed a planted private value")
		}
	}
	if !strings.Contains(output, "gui/<uid>/<label> = {\n\tpath = <path>\n\tprogram = <path>\n\targuments = {\n\t\t<arg:0>\n\t\t<arg:1>\n\t}\n") || !strings.HasSuffix(output, "\tresource coalition = {\n\t\tID = <value>\n\t}\n}\n") {
		t.Fatal("successful registration observation lost field or argument indentation evidence")
	}
}

func TestNativeServiceLaunchdStderrBounds(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "service"), 0o700); err != nil {
		t.Fatal("cannot create bounded stderr fixture")
	}
	path := filepath.Join(root, "service", "host-stderr.log")
	for _, tc := range []struct {
		name, data, want string
	}{
		{"partial_line", strings.Repeat("x", 70<<10) + "\ncomplete-line\n", "complete-line\n"},
		{"no_complete_line", strings.Repeat("x", 70<<10), ""},
		{"small_file", "first\nsecond\n", "first\nsecond\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(tc.data), 0o600); err != nil {
				t.Fatal("cannot write bounded stderr fixture")
			}
			got, err := nativeServiceLaunchdStderr(root)
			if err != nil || string(got) != tc.want {
				t.Fatal("bounded stderr read exposed an incomplete leading line or lost complete data")
			}
		})
	}
}
