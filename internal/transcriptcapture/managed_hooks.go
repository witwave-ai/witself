package transcriptcapture

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

const (
	codexManagedBlockBegin = "# BEGIN WITSELF MANAGED HOOKS"
	codexManagedBlockEnd   = "# END WITSELF MANAGED HOOKS"
	managedRunnerName      = "transcript-hook"
)

// ManagedHooksOptions identifies the administrator-owned policy and runner
// paths for one runtime.
type ManagedHooksOptions struct {
	Runtime     string
	Mode        string
	Executable  string
	Account     string
	Realm       string
	Agent       string
	Location    string
	WitselfHome string

	CodexRequirementsPath string
	CodexManagedDir       string
	ClaudeSettingsPath    string
	ClaudeManagedDir      string
}

// DefaultManagedHooksOptions returns the official file-managed policy paths
// for Codex and Claude Code on macOS and Linux.
func DefaultManagedHooksOptions(runtimeName, mode, executable, account, realm, agent, location string) (ManagedHooksOptions, error) {
	runtimeName, err := NormalizeRuntime(runtimeName)
	if err != nil {
		return ManagedHooksOptions{}, err
	}
	mode, err = NormalizeMode(mode)
	if err != nil {
		return ManagedHooksOptions{}, err
	}
	opts := ManagedHooksOptions{
		Runtime:               runtimeName,
		Mode:                  mode,
		Executable:            executable,
		Account:               strings.TrimSpace(account),
		Realm:                 strings.TrimSpace(realm),
		Agent:                 strings.TrimSpace(agent),
		Location:              strings.TrimSpace(location),
		CodexRequirementsPath: "/etc/codex/requirements.toml",
		CodexManagedDir:       "/etc/codex/witself-hooks",
	}
	switch goruntime.GOOS {
	case "darwin":
		opts.ClaudeSettingsPath = "/Library/Application Support/ClaudeCode/managed-settings.d/50-witself.json"
		opts.ClaudeManagedDir = "/Library/Application Support/ClaudeCode/witself-hooks"
	case "linux":
		opts.ClaudeSettingsPath = "/etc/claude-code/managed-settings.d/50-witself.json"
		opts.ClaudeManagedDir = "/etc/claude-code/witself-hooks"
	default:
		return ManagedHooksOptions{}, fmt.Errorf("managed hooks are not supported on %s", goruntime.GOOS)
	}
	return opts, nil
}

// PolicyPath returns the system policy file managed for this runtime.
func (o ManagedHooksOptions) PolicyPath() string {
	if o.Runtime == RuntimeCodex {
		return o.CodexRequirementsPath
	}
	return o.ClaudeSettingsPath
}

// InstallManagedHooks installs administrator-managed runtime hooks without
// modifying unrelated managed settings.
func InstallManagedHooks(opts ManagedHooksOptions) (string, error) {
	var err error
	opts.Runtime, err = NormalizeRuntime(opts.Runtime)
	if err != nil {
		return "", err
	}
	opts.Mode, err = NormalizeMode(opts.Mode)
	if err != nil {
		return "", err
	}
	opts.Executable = strings.TrimSpace(opts.Executable)
	opts.Account = strings.TrimSpace(opts.Account)
	opts.Realm = strings.TrimSpace(opts.Realm)
	opts.Agent = strings.TrimSpace(opts.Agent)
	opts.Location = strings.TrimSpace(opts.Location)
	opts.WitselfHome = strings.TrimSpace(opts.WitselfHome)
	if !filepath.IsAbs(opts.Executable) {
		return "", errors.New("managed hook executable must be an absolute path")
	}
	if opts.Account == "" || opts.Realm == "" || opts.Agent == "" {
		return "", errors.New("managed hook account, realm, and agent are required")
	}
	if opts.Location != "" && !locationNamePattern.MatchString(opts.Location) {
		return "", fmt.Errorf("invalid managed hook location %q", opts.Location)
	}
	if opts.WitselfHome != "" && (!filepath.IsAbs(opts.WitselfHome) || filepath.Clean(opts.WitselfHome) != opts.WitselfHome || strings.ContainsAny(opts.WitselfHome, "\x00\r\n")) {
		return "", errors.New("managed hook WITSELF_HOME must be a clean absolute path")
	}
	info, err := os.Stat(opts.Executable)
	if err != nil {
		return "", fmt.Errorf("stat managed hook executable: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("managed hook executable must be a regular file")
	}

	switch opts.Runtime {
	case RuntimeCodex:
		return installCodexManagedHooks(opts)
	case RuntimeClaudeCode:
		return installClaudeManagedHooks(opts)
	default:
		return "", fmt.Errorf("unsupported runtime %q", opts.Runtime)
	}
}

// RemoveManagedHooks removes only the managed policy fragment and runner that
// Witself installed.
func RemoveManagedHooks(opts ManagedHooksOptions) (string, error) {
	var err error
	opts.Runtime, err = NormalizeRuntime(opts.Runtime)
	if err != nil {
		return "", err
	}
	switch opts.Runtime {
	case RuntimeCodex:
		return removeCodexManagedHooks(opts)
	case RuntimeClaudeCode:
		return removeClaudeManagedHooks(opts)
	default:
		return "", fmt.Errorf("unsupported runtime %q", opts.Runtime)
	}
}

// ManagedHooksInstalled reports whether Witself's administrator-managed hook
// policy is present. Codex shares requirements.toml with unrelated policy, so
// only its marker-delimited Witself fragment counts; Claude's policy path is a
// dedicated Witself-owned settings fragment.
func ManagedHooksInstalled(opts ManagedHooksOptions) (bool, error) {
	var err error
	opts.Runtime, err = NormalizeRuntime(opts.Runtime)
	if err != nil {
		return false, err
	}
	switch opts.Runtime {
	case RuntimeCodex:
		raw, err := readOptionalFile(opts.CodexRequirementsPath)
		if err != nil || len(raw) == 0 {
			return false, err
		}
		_, found, err := stripCodexManagedBlock(raw)
		return found, err
	case RuntimeClaudeCode:
		_, err := os.Lstat(opts.ClaudeSettingsPath)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return err == nil, err
	default:
		return false, fmt.Errorf("unsupported runtime %q", opts.Runtime)
	}
}

func installCodexManagedHooks(opts ManagedHooksOptions) (string, error) {
	raw, err := readOptionalFile(opts.CodexRequirementsPath)
	if err != nil {
		return "", err
	}
	base, _, err := stripCodexManagedBlock(raw)
	if err != nil {
		return "", err
	}
	root, err := parseRequirements(base, opts.CodexRequirementsPath)
	if err != nil {
		return "", err
	}

	managedDir := opts.CodexManagedDir
	includeHooksTable := true
	if rawHooks, ok := root["hooks"]; ok {
		hooks, ok := rawHooks.(map[string]any)
		if !ok {
			return "", errors.New("existing Codex hooks policy is not a table")
		}
		existingDir, _ := hooks["managed_dir"].(string)
		if !filepath.IsAbs(existingDir) {
			return "", errors.New("existing Codex hooks policy must define an absolute managed_dir")
		}
		managedDir = existingDir
		includeHooksTable = false
	}
	includeFeaturesTable := true
	if rawFeatures, ok := root["features"]; ok {
		includeFeaturesTable = false
		features, ok := rawFeatures.(map[string]any)
		if !ok {
			return "", errors.New("existing Codex features policy is not a table")
		}
		if enabled, ok := features["hooks"].(bool); ok && !enabled {
			return "", errors.New("existing Codex policy disables hooks")
		}
	}

	runnerPath := filepath.Join(managedDir, managedRunnerName)
	fragment := codexManagedFragment(opts.Runtime, opts.Mode, opts.Account, opts.Realm, opts.Agent, opts.Location, opts.WitselfHome, managedDir, runnerPath, includeFeaturesTable, includeHooksTable)
	combined := appendManagedFragment(base, fragment)
	if _, err := parseRequirements(combined, opts.CodexRequirementsPath); err != nil {
		return "", fmt.Errorf("validate merged Codex requirements: %w", err)
	}
	if err := writeManagedRunner(runnerPath, opts.Executable); err != nil {
		return "", err
	}
	if err := writeManagedFileAtomic(opts.CodexRequirementsPath, combined, 0o644); err != nil {
		return "", err
	}
	return opts.CodexRequirementsPath, nil
}

func removeCodexManagedHooks(opts ManagedHooksOptions) (string, error) {
	raw, err := readOptionalFile(opts.CodexRequirementsPath)
	if err != nil || len(raw) == 0 {
		return opts.CodexRequirementsPath, err
	}
	full, err := parseRequirements(raw, opts.CodexRequirementsPath)
	if err != nil {
		return "", err
	}
	base, found, err := stripCodexManagedBlock(raw)
	if err != nil || !found {
		return opts.CodexRequirementsPath, err
	}
	managedDir := opts.CodexManagedDir
	if hooks, ok := full["hooks"].(map[string]any); ok {
		if existingDir, ok := hooks["managed_dir"].(string); ok && filepath.IsAbs(existingDir) {
			managedDir = existingDir
		}
	}
	if len(bytes.TrimSpace(base)) == 0 {
		if err := os.Remove(opts.CodexRequirementsPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	} else {
		if _, err := parseRequirements(base, opts.CodexRequirementsPath); err != nil {
			return "", err
		}
		if err := writeManagedFileAtomic(opts.CodexRequirementsPath, base, 0o644); err != nil {
			return "", err
		}
	}
	removeManagedRunner(managedDir)
	return opts.CodexRequirementsPath, nil
}

func installClaudeManagedHooks(opts ManagedHooksOptions) (string, error) {
	runnerPath := filepath.Join(opts.ClaudeManagedDir, managedRunnerName)
	hooks := map[string]any{}
	command := shellQuote(runnerPath) + " " + hookBindingArgsWithWitselfHome(opts.Runtime, opts.Account, opts.Realm, opts.Agent, opts.Location, opts.WitselfHome)
	addWitselfHandlers(hooks, opts.Runtime, opts.Mode, command)
	root := map[string]any{
		"$schema": "https://json.schemastore.org/claude-code-settings.json",
		"hooks":   hooks,
	}
	raw, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return "", err
	}
	if err := writeManagedRunner(runnerPath, opts.Executable); err != nil {
		return "", err
	}
	if err := writeManagedFileAtomic(opts.ClaudeSettingsPath, append(raw, '\n'), 0o644); err != nil {
		return "", err
	}
	return opts.ClaudeSettingsPath, nil
}

func removeClaudeManagedHooks(opts ManagedHooksOptions) (string, error) {
	if err := os.Remove(opts.ClaudeSettingsPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	removeManagedRunner(opts.ClaudeManagedDir)
	_ = os.Remove(filepath.Dir(opts.ClaudeSettingsPath))
	return opts.ClaudeSettingsPath, nil
}

func codexManagedFragment(runtimeName, mode, account, realm, agent, location, witselfHome, managedDir, runnerPath string, includeFeaturesTable, includeHooksTable bool) []byte {
	var out strings.Builder
	out.WriteString(codexManagedBlockBegin)
	out.WriteByte('\n')
	if includeFeaturesTable {
		out.WriteString("[features]\n")
		out.WriteString("hooks = true\n\n")
	}
	if includeHooksTable {
		out.WriteString("[hooks]\n")
		fmt.Fprintf(&out, "managed_dir = %s\n\n", strconv.Quote(managedDir))
	}
	for _, event := range hookEvents(runtimeName, mode) {
		fmt.Fprintf(&out, "[[hooks.%s]]\n", event)
		if eventNeedsToolMatcher(event) {
			out.WriteString("matcher = \"*\"\n")
		}
		fmt.Fprintf(&out, "[[hooks.%s.hooks]]\n", event)
		out.WriteString("type = \"command\"\n")
		command := shellQuote(runnerPath) + " " + hookBindingArgsWithWitselfHome(runtimeName, account, realm, agent, location, witselfHome)
		fmt.Fprintf(&out, "command = %s\n", strconv.Quote(command))
		out.WriteString("timeout = 10\n\n")
	}
	out.WriteString(codexManagedBlockEnd)
	out.WriteByte('\n')
	return []byte(out.String())
}

func appendManagedFragment(base, fragment []byte) []byte {
	base = bytes.TrimRight(base, "\r\n")
	out := append([]byte{}, base...)
	if len(out) > 0 {
		out = append(out, '\n', '\n')
	}
	return append(out, fragment...)
}

func stripCodexManagedBlock(raw []byte) ([]byte, bool, error) {
	start := bytes.Index(raw, []byte(codexManagedBlockBegin))
	end := bytes.Index(raw, []byte(codexManagedBlockEnd))
	if start < 0 && end < 0 {
		return raw, false, nil
	}
	if start < 0 || end < start {
		return nil, false, errors.New("codex requirements contain an incomplete Witself managed hook block")
	}
	end += len(codexManagedBlockEnd)
	if len(bytes.TrimSpace(raw[end:])) != 0 {
		return nil, false, errors.New("witself managed hook block must remain at the end of Codex requirements")
	}
	if bytes.Contains(raw[end:], []byte(codexManagedBlockEnd)) || bytes.Contains(raw[start+len(codexManagedBlockBegin):end], []byte(codexManagedBlockBegin)) {
		return nil, false, errors.New("codex requirements contain duplicate Witself managed hook blocks")
	}
	base := bytes.TrimRight(raw[:start], " \t\r\n")
	if len(base) > 0 {
		base = append(append([]byte{}, base...), '\n')
	}
	return base, true, nil
}

func parseRequirements(raw []byte, path string) (map[string]any, error) {
	root := map[string]any{}
	if len(bytes.TrimSpace(raw)) == 0 {
		return root, nil
	}
	if err := toml.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return root, nil
}

func writeManagedRunner(path, executable string) error {
	raw := []byte("#!/bin/sh\nexec " + shellQuote(executable) + " transcript hook \"$@\"\n")
	return writeManagedFileAtomic(path, raw, 0o755)
}

func removeManagedRunner(dir string) {
	_ = os.Remove(filepath.Join(dir, managedRunnerName))
	_ = os.Remove(dir)
}

func readOptionalFile(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return raw, nil
}

func writeManagedFileAtomic(path string, raw []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".witself-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
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
	return os.Rename(tmpPath, path)
}
