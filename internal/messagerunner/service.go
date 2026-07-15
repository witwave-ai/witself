package messagerunner

// This file packages the client-owned autonomous message runner as a per-user
// operating-system service. Managed definitions intentionally contain only a
// persistent Witself executable path, one supported runtime slug, and the
// value-free WITSELF_HOME root. Credentials, token paths, agent identity,
// provider configuration, and message content remain in private local state
// loaded by the trusted runner process at run time.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

const (
	// ServiceStatusSchemaV1 identifies the versioned service-status document.
	ServiceStatusSchemaV1 = "witself.message-runner-service-status.v1"

	// ServicePlatformDarwin selects launchd user services.
	ServicePlatformDarwin = "darwin"
	// ServicePlatformLinux selects systemd user services.
	ServicePlatformLinux = "linux"

	messageRunnerServiceManagedMarker = "witself-managed-message-runner-v1"
)

var (
	errMessageRunnerServiceNotLoaded = errors.New("message runner service is not loaded")
	errMessageRunnerServiceDisabled  = errors.New("message runner service is disabled")
	errMessageRunnerServiceInactive  = errors.New("message runner service is inactive")
)

// ServiceStatus is a value-free projection of the local service
// manager. Paths identify only user-owned service definition files.
type ServiceStatus struct {
	Schema    string   `json:"schema"`
	Platform  string   `json:"platform"`
	Runtime   string   `json:"runtime"`
	Installed bool     `json:"installed"`
	Enabled   bool     `json:"enabled"`
	Active    bool     `json:"active"`
	Paths     []string `json:"paths"`
}

// ServiceCommand runs one host service-manager command. It is
// injectable so lifecycle tests never touch real launchd or systemd state.
type ServiceCommand func(context.Context, string, ...string) error

// ServiceInspectCommand runs one host command while returning bounded status
// output needed to distinguish a loaded service from a running service.
type ServiceInspectCommand func(context.Context, string, ...string) (string, error)

// ServiceManager owns only Witself's exact per-runtime service
// files. Existing files without the managed marker and command contract are
// never changed or removed.
type ServiceManager struct {
	Platform       string
	UserHome       string
	ConfigHome     string
	WitselfHome    string
	Executable     string
	UID            int
	RunCommand     ServiceCommand
	InspectCommand ServiceInspectCommand
}

// DefaultServiceManager resolves the current per-user service roots.
func DefaultServiceManager(executable string) (ServiceManager, error) {
	userHome, err := os.UserHomeDir()
	if err != nil {
		return ServiceManager{}, err
	}
	witselfHome := strings.TrimSpace(os.Getenv("WITSELF_HOME"))
	if witselfHome == "" {
		witselfHome = filepath.Join(userHome, ".witself")
	} else {
		witselfHome, err = filepath.Abs(witselfHome)
		if err != nil {
			return ServiceManager{}, err
		}
	}
	configHome := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME"))
	if configHome == "" {
		configHome = filepath.Join(userHome, ".config")
	} else if !filepath.IsAbs(configHome) {
		return ServiceManager{}, errors.New("XDG_CONFIG_HOME must be an absolute path")
	}
	return ServiceManager{
		Platform: runtime.GOOS, UserHome: filepath.Clean(userHome), ConfigHome: filepath.Clean(configHome),
		WitselfHome: filepath.Clean(witselfHome), Executable: strings.TrimSpace(executable),
		UID: os.Getuid(), RunCommand: runMessageRunnerServiceCommand,
		InspectCommand: inspectMessageRunnerServiceCommand,
	}, nil
}

// Install atomically writes and activates one per-user service definition.
// Repeating Install safely refreshes only a file bearing the exact ownership
// marker and command contract.
func (m ServiceManager) Install(ctx context.Context, runtimeName string) (ServiceStatus, error) {
	if err := m.validate(runtimeName, true); err != nil {
		return ServiceStatus{}, err
	}
	definitions, err := m.definitions(runtimeName)
	if err != nil {
		return ServiceStatus{}, err
	}
	if err := preflightMessageRunnerServiceFiles(definitions); err != nil {
		return ServiceStatus{}, err
	}
	for _, definition := range definitions {
		if err := writeMessageRunnerServiceFile(definition.Path, definition.Body); err != nil {
			return ServiceStatus{}, err
		}
	}

	switch m.Platform {
	case ServicePlatformDarwin:
		domain, target := m.launchdDomain(), m.launchdTarget(runtimeName)
		// A first install has nothing to unload. Repeated installs unload the
		// previous definition so bootstrap observes the atomically replaced file.
		_ = m.run(ctx, "launchctl", "bootout", target)
		if err := m.run(ctx, "launchctl", "enable", target); err != nil {
			return ServiceStatus{}, err
		}
		if err := m.run(ctx, "launchctl", "bootstrap", domain, definitions[0].Path); err != nil {
			return ServiceStatus{}, err
		}
	case ServicePlatformLinux:
		if err := m.run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
			return ServiceStatus{}, err
		}
		if err := m.run(ctx, "systemctl", "--user", "enable", "--now", m.systemdServiceName(runtimeName)); err != nil {
			return ServiceStatus{}, err
		}
	}
	return m.Status(ctx, runtimeName)
}

// Start requests that an installed long-running message runner be started
// without changing its configuration.
func (m ServiceManager) Start(ctx context.Context, runtimeName string) (ServiceStatus, error) {
	if err := m.validate(runtimeName, false); err != nil {
		return ServiceStatus{}, err
	}
	installed, _, err := m.installed(runtimeName)
	if err != nil {
		return ServiceStatus{}, err
	}
	if !installed {
		return ServiceStatus{}, errors.New("message runner service is not installed")
	}
	switch m.Platform {
	case ServicePlatformDarwin:
		if err := m.run(ctx, "launchctl", "kickstart", "-k", m.launchdTarget(runtimeName)); err != nil {
			return ServiceStatus{}, err
		}
	case ServicePlatformLinux:
		if err := m.run(ctx, "systemctl", "--user", "start", "--no-block", m.systemdServiceName(runtimeName)); err != nil {
			return ServiceStatus{}, err
		}
	}
	return m.Status(ctx, runtimeName)
}

// Status reports whether Witself's owned definition exists and whether its
// host manager currently has it enabled and active. Disabled or inactive is a
// normal status result rather than a command failure.
func (m ServiceManager) Status(ctx context.Context, runtimeName string) (ServiceStatus, error) {
	if err := m.validate(runtimeName, false); err != nil {
		return ServiceStatus{}, err
	}
	installed, paths, err := m.installed(runtimeName)
	if err != nil {
		return ServiceStatus{}, err
	}
	status := ServiceStatus{
		Schema: ServiceStatusSchemaV1, Platform: m.Platform, Runtime: runtimeName,
		Installed: installed, Paths: paths,
	}
	if !installed {
		return status, nil
	}
	switch m.Platform {
	case ServicePlatformDarwin:
		if output, err := m.inspect(ctx, "launchctl", "print", m.launchdTarget(runtimeName)); err == nil {
			status.Enabled = true
			status.Active = launchdServiceRunning(output)
		} else if !errors.Is(err, errMessageRunnerServiceNotLoaded) {
			return ServiceStatus{}, fmt.Errorf("inspect launchd message runner service: %w", err)
		}
	case ServicePlatformLinux:
		if err := m.run(ctx, "systemctl", "--user", "is-enabled", "--quiet", m.systemdServiceName(runtimeName)); err == nil {
			status.Enabled = true
		} else if !errors.Is(err, errMessageRunnerServiceDisabled) {
			return ServiceStatus{}, fmt.Errorf("inspect systemd message runner enablement: %w", err)
		}
		if err := m.run(ctx, "systemctl", "--user", "is-active", "--quiet", m.systemdServiceName(runtimeName)); err == nil {
			status.Active = true
		} else if !errors.Is(err, errMessageRunnerServiceInactive) {
			return ServiceStatus{}, fmt.Errorf("inspect systemd message runner activity: %w", err)
		}
	}
	return status, nil
}

// Uninstall deactivates and removes only the exact owned definition. It leaves
// private runner configuration, credentials, and pending messages intact.
func (m ServiceManager) Uninstall(ctx context.Context, runtimeName string) (ServiceStatus, error) {
	if err := m.validate(runtimeName, false); err != nil {
		return ServiceStatus{}, err
	}
	definitions, err := m.definitions(runtimeName)
	if err != nil {
		return ServiceStatus{}, err
	}
	if err := preflightMessageRunnerServiceFiles(definitions); err != nil {
		return ServiceStatus{}, err
	}
	definitionPresent := false
	for _, definition := range definitions {
		exists, _, err := inspectMessageRunnerServiceFile(definition.Path)
		if err != nil {
			return ServiceStatus{}, err
		}
		definitionPresent = definitionPresent || exists
	}
	if !definitionPresent {
		return ServiceStatus{
			Schema: ServiceStatusSchemaV1, Platform: m.Platform, Runtime: runtimeName,
			Paths: messageRunnerServiceDefinitionPaths(definitions),
		}, nil
	}

	var deactivateErrors []error
	switch m.Platform {
	case ServicePlatformDarwin:
		target := m.launchdTarget(runtimeName)
		loaded := true
		if err := m.run(ctx, "launchctl", "print", target); err != nil {
			if errors.Is(err, errMessageRunnerServiceNotLoaded) {
				loaded = false
			} else {
				return ServiceStatus{}, fmt.Errorf("inspect message runner service before uninstall: %w", err)
			}
		}
		if loaded {
			if err := m.run(ctx, "launchctl", "bootout", target); err != nil {
				deactivateErrors = append(deactivateErrors, err)
			}
		}
		if err := m.run(ctx, "launchctl", "disable", target); err != nil {
			deactivateErrors = append(deactivateErrors, err)
		}
	case ServicePlatformLinux:
		if err := m.run(ctx, "systemctl", "--user", "disable", "--now", m.systemdServiceName(runtimeName)); err != nil {
			deactivateErrors = append(deactivateErrors, err)
		}
	}
	if err := errors.Join(deactivateErrors...); err != nil {
		return ServiceStatus{}, fmt.Errorf("deactivate message runner service: %w", err)
	}
	for _, definition := range definitions {
		if err := removeOwnedMessageRunnerServiceFile(definition.Path); err != nil {
			return ServiceStatus{}, err
		}
	}
	if m.Platform == ServicePlatformLinux {
		if err := m.run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
			return ServiceStatus{}, err
		}
	}
	return ServiceStatus{
		Schema: ServiceStatusSchemaV1, Platform: m.Platform, Runtime: runtimeName,
		Paths: messageRunnerServiceDefinitionPaths(definitions),
	}, nil
}

type messageRunnerServiceDefinition struct {
	Path string
	Body []byte
}

func (m ServiceManager) definitions(runtimeName string) ([]messageRunnerServiceDefinition, error) {
	switch m.Platform {
	case ServicePlatformDarwin:
		body, err := m.launchdDefinition(runtimeName)
		if err != nil {
			return nil, err
		}
		return []messageRunnerServiceDefinition{{Path: m.launchdPath(runtimeName), Body: body}}, nil
	case ServicePlatformLinux:
		body, err := m.systemdServiceDefinition(runtimeName)
		if err != nil {
			return nil, err
		}
		return []messageRunnerServiceDefinition{{Path: m.systemdServicePath(runtimeName), Body: body}}, nil
	default:
		return nil, fmt.Errorf("persistent message runner services are unsupported on %s", m.Platform)
	}
}

func (m ServiceManager) launchdDefinition(runtimeName string) ([]byte, error) {
	executable, err := xmlMessageRunnerServiceText(m.Executable)
	if err != nil {
		return nil, err
	}
	home, err := xmlMessageRunnerServiceText(m.WitselfHome)
	if err != nil {
		return nil, err
	}
	label, _ := xmlMessageRunnerServiceText(m.launchdLabel(runtimeName))
	runtimeXML, _ := xmlMessageRunnerServiceText(runtimeName)
	definition := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!-- %s -->
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>message</string>
    <string>runner</string>
    <string>serve</string>
    <string>--runtime</string>
    <string>%s</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>WITSELF_HOME</key>
    <string>%s</string>
  </dict>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>ThrottleInterval</key>
  <integer>10</integer>
  <key>Umask</key>
  <integer>63</integer>
  <key>ProcessType</key>
  <string>Background</string>
</dict>
</plist>
`, messageRunnerServiceManagedMarker, label, executable, runtimeXML, home)
	return []byte(definition), nil
}

func (m ServiceManager) systemdServiceDefinition(runtimeName string) ([]byte, error) {
	executable, err := systemdMessageRunnerServiceQuote(m.Executable)
	if err != nil {
		return nil, err
	}
	home, err := systemdMessageRunnerEnvironmentQuote("WITSELF_HOME=" + m.WitselfHome)
	if err != nil {
		return nil, err
	}
	runtimeArg, err := systemdMessageRunnerServiceQuote(runtimeName)
	if err != nil {
		return nil, err
	}
	definition := fmt.Sprintf(`# %s
[Unit]
Description=Witself autonomous message runner (%s)
StartLimitIntervalSec=15min
StartLimitBurst=10

[Service]
Type=simple
UMask=0077
Environment=%s
ExecStart=%s "message" "runner" "serve" "--runtime" %s
Restart=always
RestartSec=10s

[Install]
WantedBy=default.target
`, messageRunnerServiceManagedMarker, runtimeName, home, executable, runtimeArg)
	return []byte(definition), nil
}

func (m ServiceManager) installed(runtimeName string) (bool, []string, error) {
	definitions, err := m.definitions(runtimeName)
	if err != nil {
		return false, nil, err
	}
	paths := messageRunnerServiceDefinitionPaths(definitions)
	installed := true
	for _, definition := range definitions {
		exists, owned, err := inspectMessageRunnerServiceFile(definition.Path)
		if err != nil {
			return false, paths, err
		}
		if exists && !owned {
			return false, paths, fmt.Errorf("refusing to manage unowned service file %s", definition.Path)
		}
		installed = installed && exists
	}
	return installed, paths, nil
}

func (m ServiceManager) validate(runtimeName string, requireExecutable bool) error {
	if !supportedMessageRunnerServiceRuntime(runtimeName) {
		return fmt.Errorf("unsupported message runner runtime %q", runtimeName)
	}
	if m.Platform != ServicePlatformDarwin && m.Platform != ServicePlatformLinux {
		return fmt.Errorf("persistent message runner services are unsupported on %s", m.Platform)
	}
	for label, path := range map[string]string{
		"user home": m.UserHome, "Witself home": m.WitselfHome,
	} {
		if !cleanAbsoluteMessageRunnerServicePath(path) {
			return fmt.Errorf("message runner service %s must be a clean absolute path", label)
		}
	}
	if m.Platform == ServicePlatformLinux && !cleanAbsoluteMessageRunnerServicePath(m.ConfigHome) {
		return errors.New("message runner service config home must be a clean absolute path")
	}
	if requireExecutable && !cleanAbsoluteMessageRunnerServicePath(m.Executable) {
		return errors.New("message runner service executable must be a clean absolute path")
	}
	if m.Platform == ServicePlatformDarwin && m.UID < 0 {
		return errors.New("message runner service uid is invalid")
	}
	if m.RunCommand == nil {
		return errors.New("message runner service command runner is required")
	}
	if m.Platform == ServicePlatformDarwin && m.InspectCommand == nil {
		return errors.New("message runner service inspection runner is required")
	}
	return nil
}

func supportedMessageRunnerServiceRuntime(runtimeName string) bool {
	switch runtimeName {
	case "codex", "claude-code", "grok-build", "cursor":
		return true
	default:
		return false
	}
}

func cleanAbsoluteMessageRunnerServicePath(path string) bool {
	return path != "" && filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.ContainsAny(path, "\x00\r\n")
}

func (m ServiceManager) run(ctx context.Context, name string, args ...string) error {
	if ctx == nil {
		return errors.New("message runner service context is required")
	}
	if err := m.RunCommand(ctx, name, args...); err != nil {
		return fmt.Errorf("run %s: %w", name, err)
	}
	return nil
}

func (m ServiceManager) inspect(ctx context.Context, name string, args ...string) (string, error) {
	if ctx == nil {
		return "", errors.New("message runner service context is required")
	}
	output, err := m.InspectCommand(ctx, name, args...)
	if err != nil {
		return "", fmt.Errorf("run %s: %w", name, err)
	}
	return output, nil
}

func launchdServiceRunning(output string) bool {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.SplitN(strings.TrimSpace(line), "=", 2)
		if len(fields) == 2 && strings.EqualFold(strings.TrimSpace(fields[0]), "state") {
			return strings.EqualFold(strings.TrimSpace(fields[1]), "running")
		}
	}
	return false
}

func (m ServiceManager) launchdLabel(runtimeName string) string {
	return "ai.witwave.witself.message-runner." + runtimeName
}

func (m ServiceManager) launchdDomain() string {
	return "gui/" + strconv.Itoa(m.UID)
}

func (m ServiceManager) launchdTarget(runtimeName string) string {
	return m.launchdDomain() + "/" + m.launchdLabel(runtimeName)
}

func (m ServiceManager) launchdPath(runtimeName string) string {
	return filepath.Join(m.UserHome, "Library", "LaunchAgents", m.launchdLabel(runtimeName)+".plist")
}

func (m ServiceManager) systemdServiceName(runtimeName string) string {
	return "witself-message-runner-" + runtimeName + ".service"
}

func (m ServiceManager) systemdServicePath(runtimeName string) string {
	return filepath.Join(m.ConfigHome, "systemd", "user", m.systemdServiceName(runtimeName))
}

func runMessageRunnerServiceCommand(ctx context.Context, name string, args ...string) error {
	_, err := executeMessageRunnerServiceCommand(ctx, name, args...)
	return err
}

func inspectMessageRunnerServiceCommand(ctx context.Context, name string, args ...string) (string, error) {
	return executeMessageRunnerServiceCommand(ctx, name, args...)
}

func executeMessageRunnerServiceCommand(ctx context.Context, name string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, name, args...)
	command.Stdin = nil
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	err := command.Run()
	if err != nil && name == "systemctl" {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) && len(args) >= 3 {
			switch {
			case args[0] == "--user" && args[1] == "is-enabled" && exitError.ExitCode() == 1:
				return "", fmt.Errorf("%w: %v", errMessageRunnerServiceDisabled, err)
			case args[0] == "--user" && args[1] == "is-active" && exitError.ExitCode() == 3:
				return "", fmt.Errorf("%w: %v", errMessageRunnerServiceInactive, err)
			}
		}
	}
	if err != nil && name == "launchctl" && len(args) > 0 && args[0] == "print" {
		message := strings.ToLower(output.String())
		if strings.Contains(message, "could not find service") || strings.Contains(message, "service not found") {
			return "", fmt.Errorf("%w: %v", errMessageRunnerServiceNotLoaded, err)
		}
	}
	if err != nil {
		return "", err
	}
	return output.String(), nil
}

func preflightMessageRunnerServiceFiles(definitions []messageRunnerServiceDefinition) error {
	for _, definition := range definitions {
		exists, owned, err := inspectMessageRunnerServiceFile(definition.Path)
		if err != nil {
			return err
		}
		if exists && !owned {
			return fmt.Errorf("refusing to overwrite unowned service file %s", definition.Path)
		}
	}
	return nil
}

func inspectMessageRunnerServiceFile(path string) (exists, owned bool, err error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	if !info.Mode().IsRegular() {
		return true, false, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return true, false, err
	}
	return true, ownedMessageRunnerServiceFile(path, raw), nil
}

func ownedMessageRunnerServiceFile(path string, raw []byte) bool {
	base := filepath.Base(path)
	switch {
	case strings.HasSuffix(base, ".plist"):
		label := strings.TrimSuffix(base, ".plist")
		prefix := "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n" +
			"<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n" +
			"<!-- " + messageRunnerServiceManagedMarker + " -->\n<plist version=\"1.0\">\n"
		return bytes.HasPrefix(raw, []byte(prefix)) &&
			bytes.Contains(raw, []byte("<key>Label</key>\n  <string>"+label+"</string>")) &&
			bytes.Contains(raw, []byte("<string>message</string>\n    <string>runner</string>\n    <string>serve</string>"))
	case strings.HasPrefix(base, "witself-message-runner-") && strings.HasSuffix(base, ".service"):
		runtimeName := strings.TrimSuffix(strings.TrimPrefix(base, "witself-message-runner-"), ".service")
		prefix := "# " + messageRunnerServiceManagedMarker + "\n[Unit]\n"
		return bytes.HasPrefix(raw, []byte(prefix)) &&
			bytes.Contains(raw, []byte("\n[Service]\n")) &&
			bytes.Contains(raw, []byte(`"message" "runner" "serve" "--runtime" "`+runtimeName+`"`))
	default:
		return false
	}
}

func writeMessageRunnerServiceFile(path string, body []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, body) {
		return os.Chmod(path, 0o600)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".witself-message-runner-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

func removeOwnedMessageRunnerServiceFile(path string) error {
	exists, owned, err := inspectMessageRunnerServiceFile(path)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if !owned {
		return fmt.Errorf("refusing to remove unowned service file %s", path)
	}
	return os.Remove(path)
}

func messageRunnerServiceDefinitionPaths(definitions []messageRunnerServiceDefinition) []string {
	paths := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		paths = append(paths, definition.Path)
	}
	return paths
}

func xmlMessageRunnerServiceText(value string) (string, error) {
	if strings.ContainsAny(value, "\x00\r\n") {
		return "", errors.New("message runner service value contains an unsupported control character")
	}
	var escaped bytes.Buffer
	for _, char := range value {
		switch char {
		case '&':
			escaped.WriteString("&amp;")
		case '<':
			escaped.WriteString("&lt;")
		case '>':
			escaped.WriteString("&gt;")
		case '"':
			escaped.WriteString("&quot;")
		case '\'':
			escaped.WriteString("&apos;")
		default:
			escaped.WriteRune(char)
		}
	}
	return escaped.String(), nil
}

func systemdMessageRunnerServiceQuote(value string) (string, error) {
	if strings.ContainsAny(value, "\x00\r\n") {
		return "", errors.New("message runner service value contains an unsupported control character")
	}
	replacer := strings.NewReplacer(
		"\\", "\\\\",
		"\"", "\\\"",
		"%", "%%",
		"$", "$$",
	)
	return "\"" + replacer.Replace(value) + "\"", nil
}

func systemdMessageRunnerEnvironmentQuote(value string) (string, error) {
	if strings.ContainsAny(value, "\x00\r\n") {
		return "", errors.New("message runner service value contains an unsupported control character")
	}
	// Environment= performs no dollar expansion; unlike ExecStart=, doubling a
	// dollar here would change the state-root path. Percent specifiers expand in
	// both directives and therefore remain doubled.
	replacer := strings.NewReplacer(
		"\\", "\\\\",
		"\"", "\\\"",
		"%", "%%",
	)
	return "\"" + replacer.Replace(value) + "\"", nil
}
