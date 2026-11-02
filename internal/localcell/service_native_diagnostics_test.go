package localcell_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
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

var nativeServiceDiagnosticDSN = regexp.MustCompile(`(?i)\bpostgres(?:ql)?://[^\r\n]*`)
var nativeServiceDiagnosticPassword = regexp.MustCompile(`(?i)\b(?:password|pgpassword)[ \t]*=[^\r\n]*`)
var nativeServiceDiagnosticPID = regexp.MustCompile(`(?i)\b(?:[a-z_]*_)?pid(?:\s*[=:]\s*|\s+)[0-9]+`)
var nativeServiceDiagnosticHex = regexp.MustCompile(`(?i)[0-9a-f]{64}`)
var nativeServiceDiagnosticToken = regexp.MustCompile(`[A-Za-z0-9_+/-]{43,}={0,2}`)
var nativeServiceDiagnosticSID = regexp.MustCompile(`S-[0-9]+(?:-[0-9]+)+`)
var nativeServiceDiagnosticPipe = regexp.MustCompile(`(?i)\\\\\.\\pipe\\[^\s"'<>]+`)
var nativeServiceDiagnosticWindowsPath = regexp.MustCompile(`(?i)(?:[a-z]:[\\/]|\\\\)[^\r\n"'<>]*`)
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
	for _, key := range []string{"HOME", "PATH", "XDG_RUNTIME_DIR", "XDG_CONFIG_HOME", "DBUS_SESSION_BUS_ADDRESS", "LANG", "LC_ALL", "TMPDIR", "WITSELF_HOME", "DSH_HOME", "GOCOVERDIR", "SystemRoot", "TEMP", "TMP"} {
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
	// url.Parse permits apostrophes in passwords and raw spaces/quotes in
	// paths. Their end cannot safely be inferred from an error line. Discard
	// the entire line tail for a DSN or password assignment, including quoted,
	// escaped and percent-encoded forms, rather than retaining a private tail.
	text = nativeServiceDiagnosticDSN.ReplaceAllString(text, "<dsn>")
	text = nativeServiceDiagnosticPassword.ReplaceAllString(text, "<password>")
	text = nativeServiceDiagnosticPID.ReplaceAllString(text, "<pid>")
	text = nativeServiceDiagnosticPipe.ReplaceAllString(text, "<pipe>")
	text = nativeServiceDiagnosticSID.ReplaceAllString(text, "<sid>")
	text = nativeServiceDiagnosticWindowsPath.ReplaceAllString(text, "<path>")
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
func nativeServiceInvoke(ctx context.Context, host, root, home, label string, args []string, logf func(string, ...any), run nativeServiceDiagnosticCommand, hostCaptures ...*nativeServiceDiagnosticBuffer) error {
	cmd := exec.CommandContext(ctx, host, args...)
	cmd.WaitDelay = time.Second
	cmd.Env = nativeServiceChildEnvironment(os.LookupEnv)
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = io.Discard, &stderr
	err := cmd.Run()
	if err != nil {
		var hostCapture *nativeServiceDiagnosticBuffer
		if len(hostCaptures) != 0 {
			hostCapture = hostCaptures[0]
		}
		nativeServiceFailureDiagnostics(logf, root, home, label, err, stderr.Bytes(), hostCapture, run)
		return &nativeServiceInvocationError{cause: err, control: nativeServiceInvocationControl(stderr.Bytes())}
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
			command.WaitDelay = time.Second
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
			command.WaitDelay = time.Second
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

func nativeServiceCapabilityArguments() []string {
	return []string{"-test.run=^TestNativeServiceCapabilityDiagnostic$", "--", nativeServiceCapabilityArgument}
}

// Record only the probe verb and flag names from the actual argv constructor.
// Neither the executable nor flag values, paths or descriptor are loggable.
func nativeServiceCapabilityCommandShape() string {
	var shape []string
	for _, argument := range nativeServiceCapabilityArguments() {
		if strings.HasPrefix(argument, "-") {
			flag, _, _ := strings.Cut(argument, "=")
			shape = append(shape, flag)
		} else if argument == nativeServiceCapabilityArgument {
			shape = append(shape, argument)
		}
	}
	return strings.Join(shape, " ")
}

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
	command := exec.CommandContext(ctx, host, nativeServiceCapabilityArguments()...)
	command.WaitDelay = time.Second
	command.Env = nativeServiceChildEnvironment(os.LookupEnv)
	command.Stdin = bytes.NewReader(data)
	return command.CombinedOutput()
}

// The native proof and portable failed-host fixture share this exact invocation
// path. Keep the capture on the receiver so each restarted foreground host
// replaces the capture consumed by the same failure callback.
type nativeServiceFailureDiagnostic struct {
	proof                   *nativeServiceProofBudget
	goos, root, home, label string
	uid                     int
	logf                    func(string, ...any)
	run                     nativeServiceDiagnosticCommand
	foregroundStderr        *nativeServiceDiagnosticBuffer
	foregroundExit          *nativeServiceDiagnosticExit
	step                    string
	controlErr              error
}

func (d *nativeServiceFailureDiagnostic) logFailure(processErr error, stderr []byte) {
	d.logProofBudget()
	d.logStep()
	var sites strings.Builder
	localcell.WriteServiceHostFailure(&sites, d.controlErr, "control_transport")
	for _, line := range strings.Split(strings.TrimSpace(sites.String()), "\n") {
		if line != "" {
			d.logf("%s", line)
		}
	}
	if d.foregroundExit != nil {
		payload := sanitizeNativeServiceDiagnostic(fmt.Sprintf("native service host exit error: %v", d.foregroundExit.exitError()), d.root, d.home)
		for _, line := range strings.Split(payload, "\n") {
			d.logf("%s", line)
		}
	}
	nativeServiceFailureDiagnosticsForGOOS(d.goos, d.uid, d.logf, d.root, d.home, d.label, processErr, stderr, d.foregroundStderr, d.run)
}

// Success receipts admit complete internal labels only; sanitizing a partially
// private line into something that resembles a receipt would erase evidence.
var nativeServiceShutdownLine = regexp.MustCompile(`^service_shutdown phase=(startup|tick|control|idle) operation=((reconcile|repair|stop|start|none)|health site=(health_lock|health_tree|health_database|health_credential|health_probe|health_ready|unknown)) journal_pending=(true|false) journal_read_ok=(true|false)$`)

func nativeServiceShutdownReceipt(before, after []byte) (string, error) {
	fresh := after
	if len(before) > 0 {
		// The bounded manager tail can slide as lines append. Only accept new
		// bytes after an unchanged complete suffix of the pre-stop capture.
		found := false
		for offset := 0; offset < len(before); {
			if bytes.HasPrefix(after, before[offset:]) {
				fresh = after[len(before)-offset:]
				found = true
				break
			}
			newline := bytes.IndexByte(before[offset:], '\n')
			if newline < 0 {
				break
			}
			offset += newline + 1
		}
		if !found {
			return "", errors.New("native_service_shutdown_capture_changed")
		}
		if before[len(before)-1] != '\n' {
			// A new suffix is not a new line when the prior capture ended in
			// private partial text. Drop that completed partial line entirely.
			newline := bytes.IndexByte(fresh, '\n')
			if newline < 0 {
				fresh = nil
			} else {
				fresh = fresh[newline+1:]
			}
		}
	}
	lines := strings.Split(string(fresh), "\n")
	for index := len(lines) - 2; index >= 0; index-- {
		line := lines[index]
		if nativeServiceShutdownLine.MatchString(line) {
			return line, nil
		}
	}
	return "", errors.New("native_service_shutdown_receipt_unavailable")
}

// Count complete fresh receipts without changing the inherited last-receipt
// projection. Capture suffix and partial-line handling deliberately match it.
func nativeServiceShutdownReceiptCount(before, after []byte) int {
	fresh := after
	if len(before) > 0 {
		found := false
		for offset := 0; offset < len(before); {
			if bytes.HasPrefix(after, before[offset:]) {
				fresh = after[len(before)-offset:]
				found = true
				break
			}
			newline := bytes.IndexByte(before[offset:], '\n')
			if newline < 0 {
				break
			}
			offset += newline + 1
		}
		if !found {
			return 0
		}
		if before[len(before)-1] != '\n' {
			newline := bytes.IndexByte(fresh, '\n')
			if newline < 0 {
				fresh = nil
			} else {
				fresh = fresh[newline+1:]
			}
		}
	}
	count := 0
	lines := strings.Split(string(fresh), "\n")
	for index := 0; index < len(lines)-1; index++ {
		if nativeServiceShutdownLine.MatchString(lines[index]) {
			count++
		}
	}
	return count
}

func nativeServiceShutdownStderr(goos, root, label string, foreground *nativeServiceDiagnosticBuffer, run nativeServiceDiagnosticCommand) ([]byte, error) {
	if foreground != nil {
		return foreground.snapshot(), nil
	}
	if goos == "darwin" {
		return nativeServiceLaunchdStderr(root)
	}
	if goos == "linux" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return run(ctx, "journalctl", "--user", "-u", label, "--no-pager", "-n", "40", "--output=cat")
	}
	return nil, errors.New("native_service_shutdown_capture_unavailable")
}

func nativeServiceStopWithShutdownReceipt(goos, root, label string, foreground *nativeServiceDiagnosticBuffer, logf func(string, ...any), run nativeServiceDiagnosticCommand, stopHost func() error) error {
	before, beforeErr := nativeServiceShutdownStderr(goos, root, label, foreground, run)
	if err := stopHost(); err != nil {
		return err
	}
	after, afterErr := nativeServiceShutdownStderr(goos, root, label, foreground, run)
	if beforeErr != nil || afterErr != nil {
		return errors.New("native_service_shutdown_capture_unavailable")
	}
	line, err := nativeServiceShutdownReceipt(before, after)
	if err != nil {
		return err
	}
	logf("%s", line)
	return nil
}

func nativeServiceFailureDiagnostics(logf func(string, ...any), root, home, label string, processErr error, stderr []byte, foregroundStderr *nativeServiceDiagnosticBuffer, run nativeServiceDiagnosticCommand) {
	nativeServiceFailureDiagnosticsForGOOS(runtime.GOOS, os.Getuid(), logf, root, home, label, processErr, stderr, foregroundStderr, run)
}

func nativeServiceFailureDiagnosticsForGOOS(goos string, uid int, logf func(string, ...any), root, home, label string, processErr error, stderr []byte, foregroundStderr *nativeServiceDiagnosticBuffer, run nativeServiceDiagnosticCommand) {
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
	if foregroundStderr != nil || goos == "darwin" || goos == "windows" {
		var hostStderr []byte
		var err error
		if foregroundStderr != nil {
			// Foreground hosts write to the synchronized process buffer, not
			// the launchd file. An empty capture is still available evidence.
			hostStderr = foregroundStderr.snapshot()
		} else {
			hostStderr, err = nativeServiceLaunchdStderr(root)
		}
		log("native service host stderr error: %v", err)
		log("native service host stderr (last 30 lines):\n%s", nativeServiceStderrTail([]byte(sanitizeNativeServiceDiagnostic(string(hostStderr), root, home))))
	}
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
	switch goos {
	case "darwin":
		nativeServiceLaunchdDirectoryDiagnostics(log, home)
		domain := "gui/" + strconv.Itoa(uid)
		commands = []struct {
			name string
			args []string
		}{
			{"launchctl", []string{"print", domain + "/" + label}},
			{"launchctl", []string{"print", domain}},
			{"child-capability", nil},
		}
	case "windows":
		nativeServiceWindowsEndpointDiagnostics(log, root)
		nativeServiceWindowsDirectoryDiagnostics(log, root)
		commands = []struct {
			name string
			args []string
		}{{"child-capability", nil}}
	default:
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
		shape := strings.Join(command.args, " ")
		if command.name == "child-capability" {
			shape = nativeServiceCapabilityCommandShape()
		}
		log("native service %s diagnostic command: %s", command.name, shape)
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
	hostCapture := &nativeServiceDiagnosticBuffer{}
	_, _ = hostCapture.Write([]byte("service_control accept=rejected step=peer:sid\n"))
	err = nativeServiceInvoke(t.Context(), host, root, home, label,
		[]string{"-test.run=^TestNativeServiceInvocationFailure$", "--", childArgument},
		func(format string, args ...any) { fmt.Fprintf(&logged, format+"\n", args...) }, run, hostCapture)
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
	if !strings.Contains(logged.String(), "native service host stderr (last 30 lines):\nservice_control accept=rejected step=peer:sid\n") {
		t.Error("invocation did not forward the active foreground host capture")
	}
	wantCommands := [][]string{
		{"systemctl", "--user", "show", label, "--property=Id,LoadState,FragmentPath,User,DropInPaths,ActiveState,SubState,Result,ExecMainStatus,UnitFileState", "--no-pager"},
		{"systemctl", "--user", "show", "--property=Version,UnitPath,NFailedUnits", "--no-pager"},
		{"systemctl", "--user", "list-unit-files", label, "--no-pager"},
		{"systemctl", "--user", "show-environment"},
		{"journalctl", "--user", "-u", label, "--no-pager", "-n", "40"},
		{"child-capability"},
	}
	switch runtime.GOOS {
	case "darwin":
		domain := "gui/" + strconv.Itoa(os.Getuid())
		wantCommands = [][]string{{"launchctl", "print", domain + "/" + label}, {"launchctl", "print", domain}, {"child-capability"}}
	case "windows":
		wantCommands = [][]string{{"child-capability"}}
	}
	if !reflect.DeepEqual(commands, wantCommands) {
		t.Error("invocation did not attempt every best-effort diagnostic command")
	}
}

// Keep the native-only proof bound to the same behavioral path exercised below.
// This contract supplements that test; it does not substitute for capture checks.
func TestNativeServiceForegroundDiagnosticBinding(t *testing.T) {
	source, err := os.ReadFile("service_native_test.go")
	if err != nil || !nativeServiceForegroundDiagnosticBinding(source) {
		t.Fatal("native proof bypassed the shared failed-host capture invocation")
	}
	mutant := strings.Replace(string(source), "t.Logf, diagnosticRun, diagnostics.foregroundStderr)", "t.Logf, diagnosticRun)", 1)
	if mutant == string(source) || nativeServiceForegroundDiagnosticBinding([]byte(mutant)) {
		t.Fatal("native diagnostic binding survived removal of the invocation host capture")
	}
}

func nativeServiceForegroundDiagnosticBinding(source []byte) bool {
	file, err := parser.ParseFile(token.NewFileSet(), "service_native_test.go", source, 0)
	if err != nil {
		return false
	}
	var proof *ast.FuncDecl
	for _, declaration := range file.Decls {
		if function, ok := declaration.(*ast.FuncDecl); ok && function.Name.Name == "TestNativeServiceLifecycle" {
			proof = function
		}
	}
	if proof == nil {
		return false
	}
	ident := func(expression ast.Expr, name string) bool {
		value, ok := expression.(*ast.Ident)
		return ok && value.Name == name
	}
	selector := func(expression ast.Expr, receiver, name string) bool {
		value, ok := expression.(*ast.SelectorExpr)
		return ok && ident(value.X, receiver) && value.Sel.Name == name
	}
	bindings, starts, bypasses, invocationCaptures := 0, 0, 0, 0
	ast.Inspect(proof.Body, func(node ast.Node) bool {
		if call, ok := node.(*ast.CallExpr); ok {
			if selector(call.Fun, "diagnostics", "startForeground") {
				starts++
			}
			if name, ok := call.Fun.(*ast.Ident); ok && name.Name == "nativeServiceInvoke" && len(call.Args) > 0 && selector(call.Args[len(call.Args)-1], "diagnostics", "foregroundStderr") {
				invocationCaptures++
			}
			if name, ok := call.Fun.(*ast.Ident); ok && strings.HasPrefix(name.Name, "nativeServiceFailureDiagnostics") {
				bypasses++
			}
		}
		assignment, ok := node.(*ast.AssignStmt)
		if !ok || len(assignment.Lhs) != len(assignment.Rhs) {
			return true
		}
		for index, left := range assignment.Lhs {
			right := assignment.Rhs[index]
			if ident(left, "logFailure") && selector(right, "diagnostics", "logFailure") {
				bindings++
			}
		}
		return true
	})
	return bindings == 1 && starts == 1 && bypasses == 0 && invocationCaptures == 1
}

func TestNativeServiceShutdownReceipt(t *testing.T) {
	const first = "service_shutdown phase=idle operation=none journal_pending=false journal_read_ok=true"
	const last = "service_shutdown phase=tick operation=stop journal_pending=true journal_read_ok=false"
	const health = "service_shutdown phase=control operation=health site=health_database journal_pending=false journal_read_ok=true"
	for _, test := range []struct {
		name, before, after, want string
	}{
		{"complete", "", first + "\n", first},
		{"health_substep", "", health + "\n", health},
		{"health_unknown", "", strings.Replace(health, "site=health_database", "site=unknown", 1) + "\n", strings.Replace(health, "site=health_database", "site=unknown", 1)},
		{"health_missing_substep", "", strings.Replace(health, " site=health_database", "", 1) + "\n", ""},
		{"health_nonhealth_substep", "", strings.Replace(health, "site=health_database", "site=supervisor_health", 1) + "\n", ""},
		{"health_private_substep", "", strings.Replace(health, "site=health_database", "site=private", 1) + "\n", ""},
		{"nonhealth_substep", "", strings.Replace(health, "operation=health", "operation=stop", 1) + "\n", ""},
		{"last_complete", "", first + "\n" + last + "\n", last},
		{"private_other_lines", "", "private text\n" + first + "\nprivate tail\n", first},
		{"new_receipt_only", first + "\n", first + "\n" + last + "\n", last},
		{"bounded_tail_slides", "old prefix\n" + first + "\n", first + "\n" + last + "\n", last},
		{"stale_receipt", first + "\n", first + "\nprivate tail\n", ""},
		{"replaced_capture", first + "\n", last + "\n", ""},
		{"partial_private_prefix", "private prefix ", "private prefix " + first + "\n", ""},
		{"partial_then_complete", "private prefix ", "private prefix " + first + "\n" + last + "\n", last},
		{"private_suffix", "", first + " private text\n", ""},
		{"private_prefix", "", "private text " + first + "\n", ""},
		{"incomplete_fields", "", "service_shutdown phase=idle operation=none journal_pending=false\n", ""},
		{"unterminated", "", first, ""},
		{"private_phase", "", strings.Replace(first, "phase=idle", "phase=private", 1) + "\n", ""},
		{"private_operation", "", strings.Replace(first, "operation=none", "operation=private", 1) + "\n", ""},
		{"non_boolean", "", strings.Replace(first, "journal_pending=false", "journal_pending=0", 1) + "\n", ""},
		{"extra_field", "", first + " extra=false\n", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			line, err := nativeServiceShutdownReceipt([]byte(test.before), []byte(test.after))
			if line != test.want || (err == nil) != (test.want != "") {
				t.Fatal("shutdown projection accepted private, incomplete, or stale evidence or lost a complete receipt")
			}
		})
	}
}

func TestNativeServiceShutdownReceiptCount(t *testing.T) {
	const receipt = "service_shutdown phase=idle operation=none journal_pending=false journal_read_ok=true"
	const partial = "service_shutdown phase=idle "
	for _, test := range []struct {
		name, before, after string
		want                int
	}{
		{"zero", "", "", 0},
		{"one", "", receipt + "\n", 1},
		{"two_identical", "", receipt + "\n" + receipt + "\n", 2},
		{"valid_and_malformed", "", receipt + "\n" + receipt + " extra=false\n", 1},
		{"split_before_boundary", partial, receipt + "\n", 0},
		{"partial_then_complete", partial, receipt + "\n" + receipt + "\n", 1},
		{"fresh_only", receipt + "\n", receipt + "\n" + receipt + "\n", 1},
		{"bounded_tail_slides", "old prefix\n" + receipt + "\n", receipt + "\n" + receipt + "\n", 1},
		{"stale", receipt + "\n", receipt + "\n", 0},
		{"replaced_capture", "old prefix\n", receipt + "\n", 0},
		{"unterminated", "", receipt, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			if nativeServiceShutdownReceiptCount([]byte(test.before), []byte(test.after)) != test.want {
				t.Fatal("shutdown receipt count accepted incomplete or stale evidence or lost a complete receipt")
			}
		})
	}
}

func TestNativeServiceSuccessfulStopDiagnostics(t *testing.T) {
	const receipt = "service_shutdown phase=idle operation=none journal_pending=false journal_read_ok=true"
	for _, goos := range []string{"windows", "darwin", "linux"} {
		for _, outcome := range []string{"success", "stop_error", "missing_receipt"} {
			t.Run(goos+"/"+outcome, func(t *testing.T) {
				root := t.TempDir()
				before := []byte(receipt + "\nprivate previous host\n")
				after := append(bytes.Clone(before), []byte("private current host\n"+receipt+"\nprivate trailing text\n")...)
				if outcome == "missing_receipt" {
					after = append(bytes.Clone(before), []byte("private current host\n")...)
				}
				var foreground *nativeServiceDiagnosticBuffer
				current := before
				if goos == "windows" {
					foreground = &nativeServiceDiagnosticBuffer{}
					_, _ = foreground.Write(before)
				}
				if goos == "darwin" {
					if err := os.Mkdir(filepath.Join(root, "service"), 0o700); err != nil {
						t.Fatal("cannot prepare manager shutdown fixture")
					}
					if err := os.WriteFile(filepath.Join(root, "service", "host-stderr.log"), before, 0o600); err != nil {
						t.Fatal("cannot prepare manager shutdown log")
					}
				}
				reads, stops := 0, 0
				run := func(ctx context.Context, name string, args ...string) ([]byte, error) {
					reads++
					deadline, bounded := ctx.Deadline()
					if name != "journalctl" || !reflect.DeepEqual(args, []string{"--user", "-u", "fixture", "--no-pager", "-n", "40", "--output=cat"}) || !bounded || time.Until(deadline) > 5*time.Second {
						t.Fatal("shutdown manager log read was not bounded and value-only")
					}
					return bytes.Clone(current), nil
				}
				var logged []string
				err := nativeServiceStopWithShutdownReceipt(goos, root, "fixture", foreground, func(format string, args ...any) {
					logged = append(logged, fmt.Sprintf(format, args...))
				}, run, func() error {
					stops++
					if outcome == "stop_error" {
						return errors.New("fixture stop failed")
					}
					current = after
					if foreground != nil {
						_, _ = foreground.Write(after[len(before):])
					}
					if goos == "darwin" {
						if err := os.WriteFile(filepath.Join(root, "service", "host-stderr.log"), after, 0o600); err != nil {
							t.Fatal("cannot finish manager shutdown log")
						}
					}
					return nil
				})
				if stops != 1 || (err == nil) != (outcome == "success") {
					t.Fatal("shutdown projection changed the stop result or accepted missing evidence")
				}
				if outcome == "success" {
					if len(logged) != 1 || logged[0] != receipt {
						t.Fatal("successful shutdown did not emit exactly one complete value-free receipt")
					}
				} else if len(logged) != 0 {
					t.Fatal("unsuccessful shutdown emitted a success receipt")
				}
				wantReads := 0
				if goos == "linux" {
					wantReads = 2
					if outcome == "stop_error" {
						wantReads = 1
					}
				}
				if reads != wantReads {
					t.Fatal("shutdown receipt queried an unrelated manager")
				}
			})
		}
	}
}

func TestNativeServiceForegroundFailureDiagnostics(t *testing.T) {
	for _, goos := range []string{"windows", "darwin", "linux"} {
		t.Run(goos, func(t *testing.T) {
			root, home := t.TempDir(), t.TempDir()
			if err := os.Mkdir(filepath.Join(root, "service"), 0o700); err != nil {
				t.Fatal("cannot create foreground diagnostic fixture")
			}
			const staleLog = "stale manager host stderr"
			if err := os.WriteFile(filepath.Join(root, "service", "host-stderr.log"), []byte(staleLog), 0o600); err != nil {
				t.Fatal("cannot create stale manager log fixture")
			}
			hex, token := strings.Repeat("d3", 32), nativeServiceStandardBase64TokenFixture(t)
			const privateEnvironment = "private foreground environment"
			foreground := &nativeServiceDiagnosticBuffer{}
			for line := 0; line < 35; line++ {
				if _, err := fmt.Fprintf(foreground, "foreground-line-%02d %s %s %s LANG=%s\n", line, root, hex, token, privateEnvironment); err != nil {
					t.Fatal("cannot populate foreground diagnostic buffer")
				}
			}
			log := func(buffer *nativeServiceDiagnosticBuffer) string {
				var logged strings.Builder
				diagnostics := nativeServiceFailureDiagnostic{
					goos: goos, uid: 501, root: root, home: home, label: "fixture", foregroundStderr: buffer,
					logf: func(format string, args ...any) {
						line := fmt.Sprintf(format, args...)
						if strings.Contains(line, "\n") {
							t.Error("foreground diagnostic lost source-line prefixes")
						}
						fmt.Fprintln(&logged, line)
					},
					run: func(context.Context, string, ...string) ([]byte, error) { return nil, nil },
				}
				logFailure := diagnostics.logFailure
				logFailure(errors.New("fixture failure"), []byte("invocation stderr"))
				return logged.String()
			}
			output := log(foreground)
			for _, private := range []string{root, home, hex, token, privateEnvironment, staleLog} {
				if strings.Contains(output, private) {
					t.Fatal("foreground diagnostics exposed planted private data or used the stale manager log")
				}
			}
			for _, expected := range []string{
				"native service stderr (last 30 lines):\ninvocation stderr\n",
				"native service host stderr error: <nil>\n",
				"native service host stderr (last 30 lines):\nforeground-line-05 <root> <hex> <token> <environment>\n",
				"foreground-line-34",
			} {
				if !strings.Contains(output, expected) {
					t.Error("foreground diagnostics omitted the available sanitized host capture")
				}
			}
			var hostBlock strings.Builder
			hostBlock.WriteString("native service host stderr (last 30 lines):\n")
			for line := 5; line < 35; line++ {
				fmt.Fprintf(&hostBlock, "foreground-line-%02d <root> <hex> <token> <environment>\n", line)
			}
			if strings.Count(output, "foreground-line-") != 30 || !strings.Contains(output, hostBlock.String()) {
				t.Error("foreground diagnostics did not retain exactly the last thirty host stderr lines")
			}
			output = log(&nativeServiceDiagnosticBuffer{})
			if strings.Contains(output, "native_service_host_stderr_unavailable") || strings.Contains(output, staleLog) ||
				!strings.Contains(output, "native service host stderr error: <nil>\nnative service host stderr (last 30 lines):\n\n") {
				t.Fatal("an empty foreground capture was confused with an unavailable host log")
			}
		})
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
	switch runtime.GOOS {
	case "darwin":
		directoryCount = 2 // The private fixture has no Library directory.
	case "windows":
		directoryCount = 1 // The private fixture has no service directory.
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
	switch runtime.GOOS {
	case "darwin":
		domain := "gui/" + strconv.Itoa(os.Getuid())
		wantCommands = [][]string{{"launchctl", "print", domain + "/" + label}, {"launchctl", "print", domain}, {"child-capability"}}
	case "windows":
		wantCommands = [][]string{{"child-capability"}}
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
	}, root, home, label, errors.New("exit status 1 "+root+" "+secret+" "+controlToken+" LANG="+environmentValue), []byte(stderr.String()), nil, run)
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
			if runtime.GOOS == "windows" {
				fmt.Fprintln(os.Stderr, "native service capability: service_adapter_unsupported")
			}
			if diagnostic := nativeServicePlatformCapabilityDiagnostic(); len(diagnostic) != 0 {
				fmt.Fprintln(os.Stderr, string(diagnostic))
			}
			os.Exit(1)
		}
		if diagnostic := nativeServicePlatformCapabilityDiagnostic(); len(diagnostic) != 0 {
			if _, err := fmt.Fprintln(os.Stdout, string(diagnostic)); err != nil {
				os.Exit(1)
			}
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

// This diagnostic record deliberately keeps data local: only field-presence and
// equality booleans reach the log, including when parsing fails.
type nativeServiceEndpointDiagnosticRecord struct {
	Version            int    `json:"version"`
	InstallationID     string `json:"installation_id"`
	Generation         string `json:"generation"`
	DescriptorRevision uint64 `json:"descriptor_revision"`
	BootID             string `json:"boot_id"`
	HostPID            int    `json:"host_pid"`
	HostStartIdentity  string `json:"host_start_identity"`
	PipeName           string `json:"pipe_name"`
}

func nativeServiceDiagnosticRecord(root, name string, target any) (present, valid bool) {
	directory, err := os.Lstat(filepath.Join(root, "service"))
	if err != nil || !directory.IsDir() || directory.Mode()&os.ModeSymlink != 0 {
		return false, false
	}
	path := filepath.Join(root, "service", name)
	info, err := os.Lstat(path)
	if err != nil {
		return false, false
	}
	if !info.Mode().IsRegular() {
		return true, false
	}
	file, err := os.Open(path)
	if err != nil {
		return true, false
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || opened.Size() > 64<<10 {
		return true, false
	}
	decoder := json.NewDecoder(io.LimitReader(file, 64<<10))
	return true, decoder.Decode(target) == nil && decoder.Decode(new(any)) == io.EOF
}

func nativeServiceWindowsEndpointDiagnostics(log func(string, ...any), root string) {
	var endpoint nativeServiceEndpointDiagnosticRecord
	var descriptor localcell.ServiceDescriptor
	var lease struct {
		Version        int    `json:"version"`
		BootID         string `json:"boot_id"`
		PID            int    `json:"pid"`
		StartIdentity  string `json:"start_identity"`
		InstallationID string `json:"installation_id"`
		Owner          string `json:"owner"`
	}
	endpointPresent, endpointValid := nativeServiceDiagnosticRecord(root, "control-endpoint.json", &endpoint)
	descriptorPresent, descriptorValid := nativeServiceDiagnosticRecord(root, "descriptor.json", &descriptor)
	leasePresent, leaseValid := nativeServiceDiagnosticRecord(root, "lease.json", &lease)
	descriptorPair, leasePair := endpointValid && descriptorValid, endpointValid && leaseValid
	log("native service endpoint record_present=%t record_decoded=%t descriptor_present=%t descriptor_decoded=%t lease_present=%t lease_decoded=%t", endpointPresent, endpointValid, descriptorPresent, descriptorValid, leasePresent, leaseValid)
	log("native service endpoint version_valid=%t installation_equal=%t generation_equal=%t revision_equal=%t host_pid_equal=%t host_start_equal=%t boot_equal=%t owner_equal=%t pipe_present=%t",
		endpointValid && endpoint.Version == 1, descriptorPair && endpoint.InstallationID != "" && endpoint.InstallationID == descriptor.InstallationID,
		descriptorPair && endpoint.Generation != "" && endpoint.Generation == descriptor.Payload.Generation,
		descriptorPair && endpoint.DescriptorRevision > 0 && endpoint.DescriptorRevision == descriptor.Revision,
		leasePair && endpoint.HostPID > 1 && endpoint.HostPID == lease.PID,
		leasePair && endpoint.HostStartIdentity != "" && endpoint.HostStartIdentity == lease.StartIdentity,
		leasePair && endpoint.BootID != "" && endpoint.BootID == lease.BootID,
		descriptorValid && leaseValid && lease.Owner != "" && lease.Owner == descriptor.UID,
		endpointValid && endpoint.PipeName != "")
	// The platform seam permits portable Windows-diagnostic fixtures; only a
	// Windows process may query the actual Windows boot identity here.
	if runtime.GOOS == "windows" {
		log("native service endpoint current_boot_equal=%t", endpointValid && localcell.NativeServiceEndpointBootMatches(endpoint.BootID))
	}
}

func nativeServiceWindowsDirectoryDiagnostics(log func(string, ...any), root string) {
	directory := filepath.Join(root, "service")
	info, err := os.Lstat(directory)
	if err != nil {
		log("native service directory service metadata unavailable")
		return
	}
	attributes := func(info os.FileInfo) uint64 {
		stat := reflect.ValueOf(info.Sys())
		if stat.Kind() == reflect.Pointer && !stat.IsNil() {
			stat = stat.Elem()
			if stat.Kind() == reflect.Struct {
				field := stat.FieldByName("FileAttributes")
				if field.IsValid() && field.CanUint() {
					return field.Uint()
				}
			}
		}
		return 0
	}
	log("native service directory service attributes=0x%x directory=%t symlink=%t", attributes(info), info.IsDir(), info.Mode()&os.ModeSymlink != 0)
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		log("native service entries unavailable")
		return
	}
	known := map[string]bool{"descriptor.json": true, "control-endpoint.json": true, "lease.json": true, "boot.json": true, "supervisor.json": true, "control.secret": true, "host-stderr.log": true}
	for index, entry := range entries {
		if index == 128 {
			log("native service entries truncated")
			return
		}
		name := entry.Name()
		if !known[name] {
			name = "<other>"
		}
		info, err := entry.Info()
		if err != nil {
			log("native service entry %s metadata unavailable", name)
			continue
		}
		log("native service entry %s attributes=0x%x regular=%t symlink=%t", name, attributes(info), info.Mode().IsRegular(), info.Mode()&os.ModeSymlink != 0)
	}
}

func TestNativeServiceWindowsFailureDiagnostics(t *testing.T) {
	t.Setenv("WITSELF_RUNTIME", "local")
	root, home := t.TempDir(), t.TempDir()
	if os.Mkdir(filepath.Join(root, "service"), 0o700) != nil {
		t.Fatal("cannot prepare diagnostic fixture")
	}
	const sid = "S-1-5-21-300-400-500-1001"
	const pipe = `\\.\pipe\witself-cell-private-abcdefghijklmnopqrstuvwxyz`
	const privatePath = `C:\private user\private cell\host.exe`
	write := func(name string, value any) {
		t.Helper()
		data, err := json.Marshal(value)
		if err != nil || os.WriteFile(filepath.Join(root, "service", name), data, 0o600) != nil {
			t.Fatal("cannot write diagnostic fixture")
		}
	}
	write("control-endpoint.json", nativeServiceEndpointDiagnosticRecord{Version: 1, InstallationID: "fixture", Generation: "generation", DescriptorRevision: 1, BootID: "windows:boot", HostPID: 50, HostStartIdentity: "windows:1:2", PipeName: pipe})
	write("descriptor.json", localcell.ServiceDescriptor{InstallationID: "fixture", Revision: 1, UID: sid, Payload: localcell.ServicePayload{Generation: "generation"}})
	write("lease.json", map[string]any{"version": 1, "boot_id": "windows:boot", "pid": 50, "start_identity": "windows:1:2", "installation_id": "fixture", "owner": sid})
	var stderr strings.Builder
	for i := 0; i < 35; i++ {
		fmt.Fprintf(&stderr, "host-line-%02d %s %s %s\n", i, sid, pipe, privatePath)
	}
	if os.WriteFile(filepath.Join(root, "service", "host-stderr.log"), []byte(stderr.String()), 0o600) != nil {
		t.Fatal("cannot write host diagnostic fixture")
	}
	var logged strings.Builder
	var commands []string
	run := func(_ context.Context, name string, _ ...string) ([]byte, error) {
		commands = append(commands, name)
		return []byte("service_manager_unavailable: service_adapter_unsupported\nservice_boot_identity ntstatus=0x00000000 length=32 guid_nonzero=true"), errors.New("fixture unavailable")
	}
	nativeServiceFailureDiagnosticsForGOOS("windows", -1, func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		if strings.Contains(line, "\n") {
			t.Error("Windows diagnostic lost source-line prefixes")
		}
		fmt.Fprintln(&logged, line)
	}, root, home, "unused", errors.New("fixture "+privatePath), []byte(sid+" "+pipe), nil, run)
	output := logged.String()
	if !reflect.DeepEqual(commands, []string{"child-capability"}) {
		t.Fatal("Windows diagnostics invoked a Unix manager")
	}
	for _, private := range []string{root, home, sid, pipe, privatePath, "windows:boot", "windows:1:2"} {
		if strings.Contains(output, private) {
			t.Fatal("Windows diagnostics exposed an identity or path")
		}
	}
	for _, expected := range []string{"record_present=true", "lease_present=true", "installation_equal=true", "generation_equal=true", "revision_equal=true", "host_pid_equal=true", "host_start_equal=true", "boot_equal=true", "owner_equal=true", "pipe_present=true", "service_boot_identity ntstatus=0x00000000 length=32 guid_nonzero=true", "entry control-endpoint.json attributes=", "host-line-05", "host-line-34"} {
		if !strings.Contains(output, expected) {
			t.Error("Windows diagnostics omitted required value-free evidence")
		}
	}
	for i := 0; i < 5; i++ {
		if strings.Contains(output, fmt.Sprintf("host-line-%02d", i)) {
			t.Error("Windows host log tail exceeded thirty lines")
		}
	}
	write("control-endpoint.json", nativeServiceEndpointDiagnosticRecord{Version: 1, InstallationID: "other", Generation: "other", DescriptorRevision: 2, BootID: "other", HostPID: 51, HostStartIdentity: "other"})
	logged.Reset()
	nativeServiceWindowsEndpointDiagnostics(func(format string, args ...any) { fmt.Fprintf(&logged, format+"\n", args...) }, root)
	for _, key := range []string{"installation_equal", "generation_equal", "revision_equal", "host_pid_equal", "host_start_equal", "boot_equal", "pipe_present"} {
		if !strings.Contains(logged.String(), key+"=false") {
			t.Error("Windows diagnostics hid an endpoint mismatch")
		}
	}
}

// Portable receipt for the native child's command recorder, which previously
// logged an empty argv even though it dispatched the capability probe.
func TestNativeServiceCapabilityCommandRecorder(t *testing.T) {
	if !reflect.DeepEqual(nativeServiceCapabilityArguments(), []string{"-test.run=^TestNativeServiceCapabilityDiagnostic$", "--", "native-service-capability-probe"}) {
		t.Fatal("capability probe argv changed without updating the command-shape contract")
	}
	var logged strings.Builder
	run := func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("fixture refusal") }
	nativeServiceFailureDiagnosticsForGOOS("windows", 0, func(format string, args ...any) {
		fmt.Fprintf(&logged, format+"\n", args...)
	}, t.TempDir(), t.TempDir(), "fixture", errors.New("fixture failure"), nil, &nativeServiceDiagnosticBuffer{}, run)
	const want = "native service child-capability diagnostic command: -test.run -- native-service-capability-probe\n"
	if strings.Count(logged.String(), want) != 1 {
		t.Fatal("capability diagnostic omitted its value-free verb and flag names")
	}
	if strings.Contains(logged.String(), "TestNativeServiceCapabilityDiagnostic") {
		t.Fatal("capability diagnostic exposed its flag value")
	}
}
