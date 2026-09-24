// Package clientinventory projects recorded local installs without running clients.
package clientinventory

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/text/unicode/norm"
)

const SchemaVersion = "witself.console.clients.v1"

type Runtime string

const (
	RuntimeCodex       Runtime = "codex"
	RuntimeClaudeCode  Runtime = "claude-code"
	RuntimeGrokBuild   Runtime = "grok-build"
	RuntimeCursor      Runtime = "cursor"
	RuntimeOpenClaw    Runtime = "openclaw"
	RuntimeAntigravity Runtime = "antigravity"
	RuntimeCopilot     Runtime = "copilot"
	RuntimeDSH         Runtime = "dsh"
)

var runtimes = [...]Runtime{RuntimeCodex, RuntimeClaudeCode, RuntimeGrokBuild, RuntimeCursor, RuntimeOpenClaw, RuntimeAntigravity, RuntimeCopilot, RuntimeDSH}

type ExecutableStatus string

const (
	ExecutablePresent   ExecutableStatus = "present"
	ExecutableMissing   ExecutableStatus = "missing"
	ExecutableUnchecked ExecutableStatus = "unchecked"
)

type ConfigurationStatus string

const (
	ConfigurationMatch       ConfigurationStatus = "match"
	ConfigurationIncomplete  ConfigurationStatus = "incomplete"
	ConfigurationChanged     ConfigurationStatus = "changed"
	ConfigurationUnavailable ConfigurationStatus = "unavailable"
	ConfigurationUnsupported ConfigurationStatus = "unsupported"
)

type EffectiveVerification string

const EffectiveNotRun EffectiveVerification = "not_run"

// ConfigurationScope makes the deliberately narrow meaning of match explicit.
type ConfigurationScope string

const (
	ScopeNone            ConfigurationScope = "none"
	ScopeMCPRegistration ConfigurationScope = "mcp_registration"
)

type ScanStatus string

const (
	ScanComplete    ScanStatus = "complete"
	ScanPartial     ScanStatus = "partial"
	ScanUnavailable ScanStatus = "unavailable"
)

// Options contains private, caller-owned roots. No ambient environment is used.
// Home must be absolute; empty WitselfHome/DSHHome mean Home/.witself and Home/.dsh.
type Options struct{ Home, WitselfHome, DSHHome, AccountID string }

type Report struct {
	SchemaVersion string     `json:"schema_version"`
	DeviceLabel   string     `json:"device_label"`
	CheckedAt     time.Time  `json:"checked_at"`
	Entries       []Entry    `json:"entries"`
	ScanStatus    ScanStatus `json:"scan_status"`
}

type Entry struct {
	Runtime               Runtime               `json:"runtime"`
	RecordedVersion       string                `json:"recorded_version"`
	ExecutableStatus      ExecutableStatus      `json:"executable_status"`
	ConfigurationStatus   ConfigurationStatus   `json:"configuration_status"`
	ConfigurationScope    ConfigurationScope    `json:"configuration_scope"`
	EffectiveVerification EffectiveVerification `json:"effective_verification"`
	InstalledAt           *time.Time            `json:"installed_at,omitempty"`
}

var (
	ErrAccountRequired     = errors.New("clientinventory: canonical account ID required")
	ErrInvalidOptions      = errors.New("clientinventory: invalid roots")
	ErrUnsupportedPlatform = errors.New("clientinventory: platform unsupported")
	versionPattern         = regexp.MustCompile(`^v?(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})(-(alpha|beta|rc)(\.(0|[1-9][0-9]{0,8}))?)?$`)
	cursorVersionPattern   = regexp.MustCompile(`^[1-9][0-9]{3}\.[0-9]{2}\.[0-9]{2}-[0-9a-f]{7,16}$`)
)

const recordLimit = 64 << 10

// record intentionally does not decode arbitrary environments, endpoints, or
// credential values. TokenFile is a deny reference only, never an input to I/O.
type record struct {
	SchemaVersion        string            `json:"schema_version"`
	Runtime              Runtime           `json:"runtime"`
	AccountID            string            `json:"account_id"`
	RuntimeVersion       string            `json:"runtime_version"`
	RuntimeCLICommand    string            `json:"runtime_cli_command"`
	RuntimeConfigRoot    string            `json:"runtime_config_root"`
	RuntimeMCPConfigPath string            `json:"runtime_mcp_config_path"`
	MCPCommand           string            `json:"mcp_command"`
	MCPEnvironment       map[string]string `json:"mcp_environment"`
	Account              string            `json:"account"`
	Realm                string            `json:"realm"`
	Agent                string            `json:"agent"`
	AgentName            string            `json:"agent_name"`
	Location             struct {
		Name string `json:"name"`
	} `json:"location"`
	InstalledAt string `json:"installed_at"`
	TokenFile   string `json:"token_file"`
}

// Scan examines exactly eight fixed installation records. It never invokes a
// provider, uses credentials, follows record-selected roots, or repairs files.
// Local read failures are closed scan/status values; cancellation discards rows.
func Scan(ctx context.Context, opts Options) (Report, error) {
	if !validID(opts.AccountID) {
		return Report{}, ErrAccountRequired
	}
	if !cleanAbsolute(opts.Home) {
		return Report{}, ErrInvalidOptions
	}
	if opts.WitselfHome == "" {
		opts.WitselfHome = filepath.Join(opts.Home, ".witself")
	}
	if opts.DSHHome == "" {
		opts.DSHHome = filepath.Join(opts.Home, ".dsh")
	}
	if !cleanAbsolute(opts.WitselfHome) || !cleanAbsolute(opts.DSHHome) {
		return Report{}, ErrInvalidOptions
	}
	if err := ctx.Err(); err != nil {
		return Report{}, err
	}
	r := Report{SchemaVersion: SchemaVersion, DeviceLabel: "This device", CheckedAt: time.Now().UTC(), Entries: []Entry{}, ScanStatus: ScanComplete}
	root, err := openDirectory(opts.WitselfHome)
	if errors.Is(err, ErrUnsupportedPlatform) {
		return Report{}, ErrUnsupportedPlatform
	}
	if err != nil {
		if !errors.Is(err, errMissing) {
			r.ScanStatus = ScanUnavailable
		}
		if err := ctx.Err(); err != nil {
			return Report{}, err
		}
		return r, nil
	}
	defer root.close()
	var records []record
	var excluded tokenFileExclusions
	for _, runtime := range runtimes {
		if err := ctx.Err(); err != nil {
			return Report{}, err
		}
		raw, err := root.read(ctx, "integrations/"+string(runtime)+"/config.json", recordLimit)
		if err != nil {
			if !errors.Is(err, errMissing) {
				r.ScanStatus = ScanPartial
			}
			continue
		}
		if !boundedJSON(raw) {
			r.ScanStatus = ScanPartial
			continue
		}
		// Account fencing precedes decoding or inspecting any path reference.
		var fence struct {
			AccountID string `json:"account_id"`
		}
		if json.Unmarshal(raw, &fence) != nil {
			r.ScanStatus = ScanPartial
			continue
		}
		if fence.AccountID == "" || fence.AccountID != opts.AccountID {
			continue
		}
		var cfg record
		if json.Unmarshal(raw, &cfg) != nil || cfg.SchemaVersion != "witself.capture.v1" || cfg.Runtime != runtime {
			r.ScanStatus = ScanPartial
			continue
		}
		records = append(records, cfg)
		excluded.add(cfg.TokenFile)
	}
	// Collect every eligible record's credential exclusions before checking any
	// runtime: a later record can identify an earlier runtime's path as secret.
	for _, cfg := range records {
		if err := ctx.Err(); err != nil {
			return Report{}, err
		}
		e := Entry{Runtime: cfg.Runtime, ExecutableStatus: ExecutableUnchecked, ConfigurationStatus: ConfigurationUnsupported, ConfigurationScope: ScopeNone, EffectiveVerification: EffectiveNotRun}
		if validRecordedVersion(cfg.Runtime, cfg.RuntimeVersion) {
			e.RecordedVersion = cfg.RuntimeVersion
		}
		if installed, err := time.Parse(time.RFC3339, cfg.InstalledAt); err == nil && installed.Year() >= 2000 && !installed.After(r.CheckedAt) {
			installed = installed.UTC()
			e.InstalledAt = &installed
		}
		e.ExecutableStatus = executableStatus(ctx, opts, cfg, excluded)
		e.ConfigurationStatus, e.ConfigurationScope = configurationStatus(ctx, opts, cfg, excluded)
		r.Entries = append(r.Entries, e)
	}
	if err := ctx.Err(); err != nil {
		return Report{}, err
	}
	return r, nil
}

func validRecordedVersion(runtime Runtime, version string) bool {
	if len(version) > 48 {
		return false
	}
	if versionPattern.MatchString(version) {
		return true
	}
	// Cursor Agent records a calendar date plus a short lowercase hex build.
	// The shape guard bounds slicing; parsing rejects impossible calendar dates.
	if runtime != RuntimeCursor || !cursorVersionPattern.MatchString(version) {
		return false
	}
	_, err := time.Parse("2006.01.02", version[:10])
	return err == nil
}

func validID(s string) bool {
	if len(s) == 0 || len(s) > 256 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func cleanAbsolute(s string) bool {
	return s != "" && len(s) <= 4096 && filepath.IsAbs(s) && filepath.Clean(s) == s && !strings.ContainsAny(s, "\x00\r\n")
}

// TokenFile remains a deny reference, never a path to stat or resolve. Reject
// case and canonical Unicode aliases before any I/O, even on filesystems that
// distinguish them: the record cannot establish whether they are distinct files.
type tokenFileExclusions []string

func (paths *tokenFileExclusions) add(path string) {
	if path != "" {
		*paths = append(*paths, norm.NFC.String(filepath.Clean(path)))
	}
}

func (paths tokenFileExclusions) excludes(allowedPath string) bool {
	allowedPath = norm.NFC.String(filepath.Clean(allowedPath))
	for _, path := range paths {
		if strings.EqualFold(path, allowedPath) {
			return true
		}
	}
	return false
}

func executableStatus(ctx context.Context, opts Options, cfg record, excluded tokenFileExclusions) ExecutableStatus {
	// Fixed runtime-specific basenames prevent secret-file existence probes.
	name := map[Runtime]string{RuntimeCodex: "codex", RuntimeClaudeCode: "claude", RuntimeGrokBuild: "grok", RuntimeCursor: "cursor-agent", RuntimeOpenClaw: "openclaw", RuntimeAntigravity: "agy", RuntimeCopilot: "copilot", RuntimeDSH: "dsh"}[cfg.Runtime]
	for _, dir := range []string{".local/bin", ".bun/bin", "bin"} {
		rel := dir + "/" + name
		if cfg.RuntimeCLICommand != filepath.Join(opts.Home, filepath.FromSlash(rel)) || excluded.excludes(cfg.RuntimeCLICommand) {
			continue
		}
		root, err := openDirectory(opts.Home)
		if err != nil {
			return ExecutableUnchecked
		}
		defer root.close()
		err = root.regular(ctx, rel)
		if errors.Is(err, errMissing) {
			return ExecutableMissing
		}
		if err == nil {
			return ExecutablePresent
		}
		return ExecutableUnchecked
	}
	return ExecutableUnchecked
}
