package memorycurator

// This file packages the client-owned automatic curator as a per-user
// operating-system service. Service definitions intentionally contain only a
// persistent Witself executable path, one supported runtime name, and the
// value-free WITSELF_HOME root. Credentials, agent identity, provider/model
// choices, and source content remain in private local files loaded by the
// trusted parent process at run time.

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
	// CuratorServiceStatusSchemaV1 identifies the versioned service-status document.
	CuratorServiceStatusSchemaV1 = "witself.curator-service-status.v1"

	// CuratorServicePlatformDarwin selects launchd user services.
	CuratorServicePlatformDarwin = "darwin"
	// CuratorServicePlatformLinux selects systemd user services.
	CuratorServicePlatformLinux = "linux"

	curatorServiceManagedMarker = "witself-managed-memory-curator-v1"
	curatorServiceInterval      = 300
)

var errCuratorServiceNotLoaded = errors.New("automatic curator service is not loaded")

// CuratorServiceStatus is a value-free projection of the local service
// manager. Paths identify only user-owned service definition files.
type CuratorServiceStatus struct {
	Schema    string   `json:"schema"`
	Platform  string   `json:"platform"`
	Runtime   string   `json:"runtime"`
	Installed bool     `json:"installed"`
	Enabled   bool     `json:"enabled"`
	Active    bool     `json:"active"`
	Paths     []string `json:"paths"`
}

// CuratorServiceCommand runs one host service-manager command. It is
// injectable so lifecycle tests never touch the real launchd or systemd state.
type CuratorServiceCommand func(context.Context, string, ...string) error

// CuratorServiceManager owns only Witself's exact per-runtime service files.
// Existing files without the managed marker are never changed or removed.
type CuratorServiceManager struct {
	Platform    string
	UserHome    string
	ConfigHome  string
	WitselfHome string
	Executable  string
	UID         int
	RunCommand  CuratorServiceCommand
}

// DefaultCuratorServiceManager resolves the current per-user service roots.
func DefaultCuratorServiceManager(executable string) (CuratorServiceManager, error) {
	userHome, err := os.UserHomeDir()
	if err != nil {
		return CuratorServiceManager{}, err
	}
	witselfHome := strings.TrimSpace(os.Getenv("WITSELF_HOME"))
	if witselfHome == "" {
		witselfHome = filepath.Join(userHome, ".witself")
	} else {
		witselfHome, err = filepath.Abs(witselfHome)
		if err != nil {
			return CuratorServiceManager{}, err
		}
	}
	configHome := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME"))
	if configHome == "" {
		configHome = filepath.Join(userHome, ".config")
	} else if !filepath.IsAbs(configHome) {
		return CuratorServiceManager{}, errors.New("XDG_CONFIG_HOME must be an absolute path")
	}
	return CuratorServiceManager{
		Platform: runtime.GOOS, UserHome: filepath.Clean(userHome), ConfigHome: filepath.Clean(configHome),
		WitselfHome: filepath.Clean(witselfHome), Executable: strings.TrimSpace(executable),
		UID: os.Getuid(), RunCommand: runCuratorServiceCommand,
	}, nil
}

// Install atomically writes and activates one per-user service definition.
// Repeating Install is safe and refreshes only files bearing our marker.
func (m CuratorServiceManager) Install(ctx context.Context, runtimeName string) (CuratorServiceStatus, error) {
	if err := m.validate(runtimeName, true); err != nil {
		return CuratorServiceStatus{}, err
	}
	definitions, err := m.definitions(runtimeName)
	if err != nil {
		return CuratorServiceStatus{}, err
	}
	if err := preflightCuratorServiceFiles(definitions); err != nil {
		return CuratorServiceStatus{}, err
	}
	for _, definition := range definitions {
		if err := writeCuratorServiceFile(definition.Path, definition.Body); err != nil {
			return CuratorServiceStatus{}, err
		}
	}

	switch m.Platform {
	case CuratorServicePlatformDarwin:
		domain, target := m.launchdDomain(), m.launchdTarget(runtimeName)
		// bootout is intentionally best-effort: a first install has nothing to
		// unload, while a repeated install must refresh the loaded definition.
		_ = m.run(ctx, "launchctl", "bootout", target)
		if err := m.run(ctx, "launchctl", "enable", target); err != nil {
			return CuratorServiceStatus{}, err
		}
		if err := m.run(ctx, "launchctl", "bootstrap", domain, definitions[0].Path); err != nil {
			return CuratorServiceStatus{}, err
		}
	case CuratorServicePlatformLinux:
		if err := m.run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
			return CuratorServiceStatus{}, err
		}
		if err := m.run(ctx, "systemctl", "--user", "enable", "--now", m.systemdTimerName(runtimeName)); err != nil {
			return CuratorServiceStatus{}, err
		}
		if err := m.run(ctx, "systemctl", "--user", "start", "--no-block", m.systemdServiceName(runtimeName)); err != nil {
			return CuratorServiceStatus{}, err
		}
	}
	return m.Status(ctx, runtimeName)
}

// Start requests one immediate bounded run without changing configuration.
func (m CuratorServiceManager) Start(ctx context.Context, runtimeName string) (CuratorServiceStatus, error) {
	if err := m.validate(runtimeName, false); err != nil {
		return CuratorServiceStatus{}, err
	}
	installed, _, err := m.installed(runtimeName)
	if err != nil {
		return CuratorServiceStatus{}, err
	}
	if !installed {
		return CuratorServiceStatus{}, errors.New("automatic curator service is not installed")
	}
	switch m.Platform {
	case CuratorServicePlatformDarwin:
		if err := m.run(ctx, "launchctl", "kickstart", "-k", m.launchdTarget(runtimeName)); err != nil {
			return CuratorServiceStatus{}, err
		}
	case CuratorServicePlatformLinux:
		if err := m.run(ctx, "systemctl", "--user", "start", "--no-block", m.systemdServiceName(runtimeName)); err != nil {
			return CuratorServiceStatus{}, err
		}
	}
	return m.Status(ctx, runtimeName)
}

// Status reports whether Witself's owned definitions exist and whether their
// host manager currently has them enabled/active. An inactive service is a
// normal status result, not a command failure.
func (m CuratorServiceManager) Status(ctx context.Context, runtimeName string) (CuratorServiceStatus, error) {
	if err := m.validate(runtimeName, false); err != nil {
		return CuratorServiceStatus{}, err
	}
	installed, paths, err := m.installed(runtimeName)
	if err != nil {
		return CuratorServiceStatus{}, err
	}
	status := CuratorServiceStatus{
		Schema: CuratorServiceStatusSchemaV1, Platform: m.Platform, Runtime: runtimeName,
		Installed: installed, Paths: paths,
	}
	if !installed {
		return status, nil
	}
	switch m.Platform {
	case CuratorServicePlatformDarwin:
		if err := m.run(ctx, "launchctl", "print", m.launchdTarget(runtimeName)); err == nil {
			status.Enabled, status.Active = true, true
		}
	case CuratorServicePlatformLinux:
		if err := m.run(ctx, "systemctl", "--user", "is-enabled", "--quiet", m.systemdTimerName(runtimeName)); err == nil {
			status.Enabled = true
		}
		if err := m.run(ctx, "systemctl", "--user", "is-active", "--quiet", m.systemdTimerName(runtimeName)); err == nil {
			status.Active = true
		}
	}
	return status, nil
}

// Uninstall deactivates and removes only owned definitions. It deliberately
// leaves the private automation policy, credentials, and pending wakes intact.
func (m CuratorServiceManager) Uninstall(ctx context.Context, runtimeName string) (CuratorServiceStatus, error) {
	if err := m.validate(runtimeName, false); err != nil {
		return CuratorServiceStatus{}, err
	}
	definitions, err := m.definitions(runtimeName)
	if err != nil {
		return CuratorServiceStatus{}, err
	}
	if err := preflightCuratorServiceFiles(definitions); err != nil {
		return CuratorServiceStatus{}, err
	}
	definitionPresent := false
	for _, definition := range definitions {
		exists, _, err := inspectCuratorServiceFile(definition.Path)
		if err != nil {
			return CuratorServiceStatus{}, err
		}
		definitionPresent = definitionPresent || exists
	}
	// A repeated uninstall is a no-op. If any owned definition remains, however,
	// deactivation must succeed before the files are removed; otherwise a loaded
	// job could survive while a successful result makes it impossible to manage.
	if !definitionPresent {
		return CuratorServiceStatus{
			Schema: CuratorServiceStatusSchemaV1, Platform: m.Platform, Runtime: runtimeName,
			Paths: curatorServiceDefinitionPaths(definitions),
		}, nil
	}
	var deactivateErrors []error
	switch m.Platform {
	case CuratorServicePlatformDarwin:
		target := m.launchdTarget(runtimeName)
		loaded := true
		if err := m.run(ctx, "launchctl", "print", target); err != nil {
			if errors.Is(err, errCuratorServiceNotLoaded) {
				loaded = false
			} else {
				return CuratorServiceStatus{}, fmt.Errorf("inspect automatic curator service before uninstall: %w", err)
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
	case CuratorServicePlatformLinux:
		if err := m.run(ctx, "systemctl", "--user", "disable", "--now", m.systemdTimerName(runtimeName)); err != nil {
			deactivateErrors = append(deactivateErrors, err)
		}
		if err := m.run(ctx, "systemctl", "--user", "stop", m.systemdServiceName(runtimeName)); err != nil {
			deactivateErrors = append(deactivateErrors, err)
		}
	}
	if err := errors.Join(deactivateErrors...); err != nil {
		return CuratorServiceStatus{}, fmt.Errorf("deactivate automatic curator service: %w", err)
	}
	for _, definition := range definitions {
		if err := removeOwnedCuratorServiceFile(definition.Path); err != nil {
			return CuratorServiceStatus{}, err
		}
	}
	if m.Platform == CuratorServicePlatformLinux {
		if err := m.run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
			return CuratorServiceStatus{}, err
		}
	}
	return CuratorServiceStatus{
		Schema: CuratorServiceStatusSchemaV1, Platform: m.Platform, Runtime: runtimeName,
		Paths: curatorServiceDefinitionPaths(definitions),
	}, nil
}

type curatorServiceDefinition struct {
	Path string
	Body []byte
}

func (m CuratorServiceManager) definitions(runtimeName string) ([]curatorServiceDefinition, error) {
	switch m.Platform {
	case CuratorServicePlatformDarwin:
		body, err := m.launchdDefinition(runtimeName)
		if err != nil {
			return nil, err
		}
		return []curatorServiceDefinition{{Path: m.launchdPath(runtimeName), Body: body}}, nil
	case CuratorServicePlatformLinux:
		service, err := m.systemdServiceDefinition(runtimeName)
		if err != nil {
			return nil, err
		}
		timer := m.systemdTimerDefinition(runtimeName)
		return []curatorServiceDefinition{
			{Path: m.systemdServicePath(runtimeName), Body: service},
			{Path: m.systemdTimerPath(runtimeName), Body: timer},
		}, nil
	default:
		return nil, fmt.Errorf("persistent automatic curator services are unsupported on %s", m.Platform)
	}
}

func (m CuratorServiceManager) launchdDefinition(runtimeName string) ([]byte, error) {
	executable, err := xmlServiceText(m.Executable)
	if err != nil {
		return nil, err
	}
	home, err := xmlServiceText(m.WitselfHome)
	if err != nil {
		return nil, err
	}
	label, _ := xmlServiceText(m.launchdLabel(runtimeName))
	runtimeXML, _ := xmlServiceText(runtimeName)
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
    <string>memory</string>
    <string>curate</string>
    <string>auto</string>
    <string>run</string>
    <string>--runtime</string>
    <string>%s</string>
    <string>--force</string>
    <string>--supervise</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>WITSELF_HOME</key>
    <string>%s</string>
  </dict>
  <key>RunAtLoad</key>
  <true/>
  <key>StartInterval</key>
  <integer>%d</integer>
  <key>KeepAlive</key>
  <dict>
    <key>SuccessfulExit</key>
    <false/>
  </dict>
  <key>ThrottleInterval</key>
  <integer>60</integer>
  <key>Umask</key>
  <integer>63</integer>
  <key>ProcessType</key>
  <string>Background</string>
</dict>
</plist>
`, curatorServiceManagedMarker, label, executable, runtimeXML, home, curatorServiceInterval)
	return []byte(definition), nil
}

func (m CuratorServiceManager) systemdServiceDefinition(runtimeName string) ([]byte, error) {
	executable, err := systemdServiceQuote(m.Executable)
	if err != nil {
		return nil, err
	}
	home, err := systemdEnvironmentQuote("WITSELF_HOME=" + m.WitselfHome)
	if err != nil {
		return nil, err
	}
	runtimeArg, err := systemdServiceQuote(runtimeName)
	if err != nil {
		return nil, err
	}
	definition := fmt.Sprintf(`# %s
[Unit]
Description=Witself automatic memory curator (%s)
StartLimitIntervalSec=15min
StartLimitBurst=3

[Service]
Type=oneshot
UMask=0077
Environment=%s
ExecStart=%s "memory" "curate" "auto" "run" "--runtime" %s "--force" "--supervise"
Restart=on-failure
RestartSec=60s
TimeoutStartSec=50min
`, curatorServiceManagedMarker, runtimeName, home, executable, runtimeArg)
	return []byte(definition), nil
}

func (m CuratorServiceManager) systemdTimerDefinition(runtimeName string) []byte {
	return []byte(fmt.Sprintf(`# %s
[Unit]
Description=Schedule Witself automatic memory curator (%s)

[Timer]
OnBootSec=2min
OnUnitInactiveSec=%ds
AccuracySec=30s
Persistent=true
Unit=%s

[Install]
WantedBy=timers.target
`, curatorServiceManagedMarker, runtimeName, curatorServiceInterval, m.systemdServiceName(runtimeName)))
}

func (m CuratorServiceManager) installed(runtimeName string) (bool, []string, error) {
	definitions, err := m.definitions(runtimeName)
	if err != nil {
		return false, nil, err
	}
	paths := curatorServiceDefinitionPaths(definitions)
	installed := true
	for _, definition := range definitions {
		exists, owned, err := inspectCuratorServiceFile(definition.Path)
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

func (m CuratorServiceManager) validate(runtimeName string, requireExecutable bool) error {
	if runtimeName != string(ProviderCodex) && runtimeName != string(ProviderClaudeCode) &&
		runtimeName != string(ProviderGrokBuild) && runtimeName != string(ProviderCursor) {
		return fmt.Errorf("unsupported automatic curator runtime %q", runtimeName)
	}
	if m.Platform != CuratorServicePlatformDarwin && m.Platform != CuratorServicePlatformLinux {
		return fmt.Errorf("persistent automatic curator services are unsupported on %s", m.Platform)
	}
	for label, path := range map[string]string{
		"user home": m.UserHome, "Witself home": m.WitselfHome,
	} {
		if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsAny(path, "\x00\r\n") {
			return fmt.Errorf("automatic curator service %s must be a clean absolute path", label)
		}
	}
	if m.Platform == CuratorServicePlatformLinux &&
		(m.ConfigHome == "" || !filepath.IsAbs(m.ConfigHome) || filepath.Clean(m.ConfigHome) != m.ConfigHome || strings.ContainsAny(m.ConfigHome, "\x00\r\n")) {
		return errors.New("automatic curator service config home must be a clean absolute path")
	}
	if requireExecutable && (m.Executable == "" || !filepath.IsAbs(m.Executable) ||
		filepath.Clean(m.Executable) != m.Executable || strings.ContainsAny(m.Executable, "\x00\r\n")) {
		return errors.New("automatic curator service executable must be a clean absolute path")
	}
	if m.Platform == CuratorServicePlatformDarwin && m.UID < 0 {
		return errors.New("automatic curator service uid is invalid")
	}
	if m.RunCommand == nil {
		return errors.New("automatic curator service command runner is required")
	}
	return nil
}

func (m CuratorServiceManager) run(ctx context.Context, name string, args ...string) error {
	if ctx == nil {
		return errors.New("automatic curator service context is required")
	}
	if err := m.RunCommand(ctx, name, args...); err != nil {
		return fmt.Errorf("run %s: %w", name, err)
	}
	return nil
}

func (m CuratorServiceManager) launchdLabel(runtimeName string) string {
	return "ai.witwave.witself.memory-curator." + runtimeName
}

func (m CuratorServiceManager) launchdDomain() string {
	return "gui/" + strconv.Itoa(m.UID)
}

func (m CuratorServiceManager) launchdTarget(runtimeName string) string {
	return m.launchdDomain() + "/" + m.launchdLabel(runtimeName)
}

func (m CuratorServiceManager) launchdPath(runtimeName string) string {
	return filepath.Join(m.UserHome, "Library", "LaunchAgents", m.launchdLabel(runtimeName)+".plist")
}

func (m CuratorServiceManager) systemdServiceName(runtimeName string) string {
	return "witself-memory-curator-" + runtimeName + ".service"
}

func (m CuratorServiceManager) systemdTimerName(runtimeName string) string {
	return "witself-memory-curator-" + runtimeName + ".timer"
}

func (m CuratorServiceManager) systemdServicePath(runtimeName string) string {
	return filepath.Join(m.ConfigHome, "systemd", "user", m.systemdServiceName(runtimeName))
}

func (m CuratorServiceManager) systemdTimerPath(runtimeName string) string {
	return filepath.Join(m.ConfigHome, "systemd", "user", m.systemdTimerName(runtimeName))
}

func runCuratorServiceCommand(ctx context.Context, name string, args ...string) error {
	command := exec.CommandContext(ctx, name, args...)
	command.Stdin = nil
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	err := command.Run()
	if err != nil && name == "launchctl" && len(args) > 0 && args[0] == "print" {
		message := strings.ToLower(output.String())
		if strings.Contains(message, "could not find service") || strings.Contains(message, "service not found") {
			return fmt.Errorf("%w: %v", errCuratorServiceNotLoaded, err)
		}
	}
	return err
}

func preflightCuratorServiceFiles(definitions []curatorServiceDefinition) error {
	for _, definition := range definitions {
		exists, owned, err := inspectCuratorServiceFile(definition.Path)
		if err != nil {
			return err
		}
		if exists && !owned {
			return fmt.Errorf("refusing to overwrite unowned service file %s", definition.Path)
		}
	}
	return nil
}

func inspectCuratorServiceFile(path string) (exists, owned bool, err error) {
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
	return true, ownedCuratorServiceFile(path, raw), nil
}

func ownedCuratorServiceFile(path string, raw []byte) bool {
	base := filepath.Base(path)
	switch {
	case strings.HasSuffix(base, ".plist"):
		label := strings.TrimSuffix(base, ".plist")
		prefix := "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n" +
			"<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n" +
			"<!-- " + curatorServiceManagedMarker + " -->\n<plist version=\"1.0\">\n"
		return bytes.HasPrefix(raw, []byte(prefix)) &&
			bytes.Contains(raw, []byte("<key>Label</key>\n  <string>"+label+"</string>")) &&
			bytes.Contains(raw, []byte("<string>memory</string>\n    <string>curate</string>\n    <string>auto</string>\n    <string>run</string>"))
	case strings.HasPrefix(base, "witself-memory-curator-") && strings.HasSuffix(base, ".service"):
		runtimeName := strings.TrimSuffix(strings.TrimPrefix(base, "witself-memory-curator-"), ".service")
		prefix := "# " + curatorServiceManagedMarker + "\n[Unit]\n"
		return bytes.HasPrefix(raw, []byte(prefix)) &&
			bytes.Contains(raw, []byte("\n[Service]\n")) &&
			bytes.Contains(raw, []byte(`"memory" "curate" "auto" "run" "--runtime" "`+runtimeName+`"`))
	case strings.HasPrefix(base, "witself-memory-curator-") && strings.HasSuffix(base, ".timer"):
		runtimeName := strings.TrimSuffix(strings.TrimPrefix(base, "witself-memory-curator-"), ".timer")
		prefix := "# " + curatorServiceManagedMarker + "\n[Unit]\n"
		return bytes.HasPrefix(raw, []byte(prefix)) &&
			bytes.Contains(raw, []byte("\n[Timer]\n")) &&
			bytes.Contains(raw, []byte("\nUnit=witself-memory-curator-"+runtimeName+".service\n"))
	default:
		return false
	}
}

func writeCuratorServiceFile(path string, body []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, body) {
		return os.Chmod(path, 0o600)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".witself-memory-curator-*")
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

func removeOwnedCuratorServiceFile(path string) error {
	exists, owned, err := inspectCuratorServiceFile(path)
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

func curatorServiceDefinitionPaths(definitions []curatorServiceDefinition) []string {
	paths := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		paths = append(paths, definition.Path)
	}
	return paths
}

func xmlServiceText(value string) (string, error) {
	if strings.ContainsAny(value, "\x00\r\n") {
		return "", errors.New("automatic curator service value contains an unsupported control character")
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

func systemdServiceQuote(value string) (string, error) {
	if strings.ContainsAny(value, "\x00\r\n") {
		return "", errors.New("automatic curator service value contains an unsupported control character")
	}
	replacer := strings.NewReplacer(
		"\\", "\\\\",
		"\"", "\\\"",
		"%", "%%",
		"$", "$$",
	)
	return "\"" + replacer.Replace(value) + "\"", nil
}

func systemdEnvironmentQuote(value string) (string, error) {
	if strings.ContainsAny(value, "\x00\r\n") {
		return "", errors.New("automatic curator service value contains an unsupported control character")
	}
	// Environment= performs no dollar expansion; unlike ExecStart=, doubling a
	// dollar here would change the actual state-root path. Percent specifiers do
	// expand in both directives and therefore remain doubled.
	replacer := strings.NewReplacer(
		"\\", "\\\\",
		"\"", "\\\"",
		"%", "%%",
	)
	return "\"" + replacer.Replace(value) + "\"", nil
}
