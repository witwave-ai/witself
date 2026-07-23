package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/local"
	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

const (
	openClawCLIJSONLimit                = 1024 * 1024
	openClawCLIReadTimeout              = 45 * time.Second
	openClawCLIMutationTimeout          = 45 * time.Second
	openClawCLIWaitDelay                = 250 * time.Millisecond
	openClawMCPConnectTimeoutSeconds    = 60
	openClawMCPEnvironmentValueMaxBytes = 4096
)

var openClawUnsupportedSelectorEnvironment = []string{
	"OPENCLAW_HOME",
	"OPENCLAW_WORKSPACE_DIR",
	"OPENCLAW_AGENT_DIR",
	"OPENCLAW_INCLUDE_ROOTS",
}

type openClawAgent struct {
	ID        string `json:"id"`
	Workspace string `json:"workspace"`
	IsDefault bool   `json:"isDefault"`
}

type openClawMCPBinding struct {
	Command        string            `json:"command"`
	Args           []string          `json:"args"`
	Env            map[string]string `json:"env,omitempty"`
	ConnectTimeout int               `json:"connectTimeout,omitempty"`
}

type openClawMCPConfigSnapshot struct {
	targetExists bool
	target       string
	nonTarget    map[string]string
}

func validateOpenClawDefaultAgent(runtimeCLI string) (string, error) {
	agent, err := inspectOpenClawDefaultAgent(runtimeCLI)
	if err != nil {
		return "", err
	}
	return agent.Workspace, nil
}

func inspectOpenClawDefaultAgent(runtimeCLI string) (openClawAgent, error) {
	return inspectOpenClawDefaultAgentWithEnvironment(runtimeCLI, nil)
}

func inspectOpenClawDefaultAgentWithEnvironment(runtimeCLI string, environment map[string]string) (openClawAgent, error) {
	raw, err := openClawCLIJSONWithEnvironment(runtimeCLI, environment, "agents", "list", "--json")
	if err != nil {
		return openClawAgent{}, unavailableIntegrationTopology(fmt.Errorf("inspect OpenClaw agents: %w", err))
	}
	var agents []openClawAgent
	if err := json.Unmarshal(raw, &agents); err != nil {
		return openClawAgent{}, fmt.Errorf("parse OpenClaw agent list: %w", err)
	}
	if len(agents) != 1 {
		return openClawAgent{}, fmt.Errorf("openclaw has %d configured agents; Witself phase-1 integration requires exactly one default agent", len(agents))
	}
	agent := agents[0]
	workspace := strings.TrimSpace(agent.Workspace)
	if strings.TrimSpace(agent.ID) == "" || !agent.IsDefault || workspace == "" {
		return openClawAgent{}, errors.New("openclaw's sole agent is not an unambiguous default agent with a workspace")
	}
	if !filepath.IsAbs(workspace) || filepath.Clean(workspace) != workspace {
		return openClawAgent{}, fmt.Errorf("openclaw default workspace must be a clean absolute path, got %q", workspace)
	}
	agent.ID = strings.TrimSpace(agent.ID)
	agent.Workspace = workspace
	return agent, nil
}

func validateOpenClawInstalledTopology(cfg transcriptcapture.Config) error {
	agent, err := inspectOpenClawDefaultAgentWithEnvironment(cfg.RuntimeCLICommand, cfg.MCPEnvironment)
	if err != nil {
		return fmt.Errorf("openclaw topology no longer matches the installed single-agent binding: %w", err)
	}
	if agent.ID != cfg.RuntimeAgentID {
		return fmt.Errorf("openclaw default agent changed from installed %s to %s; refusing to expose the credential-bound MCP server", cfg.RuntimeAgentID, agent.ID)
	}
	if agent.Workspace != cfg.RuntimeWorkspace {
		return fmt.Errorf("openclaw default workspace changed from installed %s to %s; refusing to expose the credential-bound MCP server", cfg.RuntimeWorkspace, agent.Workspace)
	}
	return nil
}

func validateOpenClawInstalledIntegration(cfg transcriptcapture.Config) error {
	if err := validateOpenClawInstalledTopology(cfg); err != nil {
		return err
	}
	expected, err := openClawMCPBindingFromConfig(cfg.MCPCommand, cfg)
	if err != nil {
		return fmt.Errorf("reconstruct installed OpenClaw MCP binding: %w", err)
	}
	current, exists, err := inspectOpenClawMCPWithEnvironment(cfg.RuntimeCLICommand, cfg.MCPEnvironment)
	if err != nil {
		return fmt.Errorf("inspect installed OpenClaw MCP binding: %w", err)
	}
	if !exists || !equalOpenClawMCPBinding(current, expected) {
		return errors.New("installed OpenClaw MCP server witself does not match the persisted exact binding")
	}
	return nil
}

func openClawCLIJSONWithEnvironment(runtimeCLI string, environment map[string]string, args ...string) ([]byte, error) {
	return openClawCLIJSONWithTimeout(runtimeCLI, environment, openClawCLIReadTimeout, args...)
}

func openClawCLIJSONWithTimeout(runtimeCLI string, environment map[string]string, timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := openClawCommandContext(ctx, runtimeCLI, environment, args...)
	cleanup, err := isolateProviderCLIWorkingDirectory(cmd, "OpenClaw")
	if err != nil {
		return nil, err
	}
	defer cleanup()
	stdout := &antigravityValidationOutput{limit: openClawCLIJSONLimit}
	stderr := &antigravityValidationOutput{limit: genericProviderCLIOutputLimit}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err = cmd.Run()
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("openclaw CLI timed out after %s: %w", timeout, ctx.Err())
		}
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return nil, errors.New(message)
	}
	if stdout.truncated {
		return nil, fmt.Errorf("openclaw JSON output exceeds %d bytes", openClawCLIJSONLimit)
	}
	return append([]byte(nil), stdout.buffer.Bytes()...), nil
}

func openClawCommandContext(ctx context.Context, runtimeCLI string, environment map[string]string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, runtimeCLI, args...)
	cmd.WaitDelay = openClawCLIWaitDelay
	if environment == nil {
		return cmd
	}
	cmd.Env = openClawCommandEnvironment(os.Environ(), environment)
	return cmd
}

func openClawCommandEnvironment(inherited []string, environment map[string]string) []string {
	values := make(map[string]string, len(inherited))
	for _, entry := range inherited {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			values[key] = value
		}
	}
	pinnedKeys := append([]string{
		"WITSELF_HOME",
		"OPENCLAW_CONFIG_PATH",
		"OPENCLAW_STATE_DIR",
		"OPENCLAW_PROFILE",
	}, openClawUnsupportedSelectorEnvironment...)
	for inheritedKey := range values {
		for _, pinnedKey := range pinnedKeys {
			if strings.EqualFold(inheritedKey, pinnedKey) {
				delete(values, inheritedKey)
				break
			}
		}
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys)+len(environment))
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	overlayKeys := make([]string, 0, len(environment))
	for key := range environment {
		overlayKeys = append(overlayKeys, key)
	}
	sort.Strings(overlayKeys)
	for _, key := range overlayKeys {
		result = append(result, key+"="+environment[key])
	}
	return result
}

func captureOpenClawMCPEnvironment() (map[string]string, error) {
	for _, key := range openClawUnsupportedSelectorEnvironment {
		if strings.TrimSpace(os.Getenv(key)) != "" {
			return nil, fmt.Errorf("%s is not supported by the phase-1 OpenClaw integration; unset it before installing Witself", key)
		}
	}
	witselfHome, err := local.Home()
	if err != nil {
		return nil, fmt.Errorf("resolve WITSELF_HOME: %w", err)
	}
	if strings.TrimSpace(witselfHome) != witselfHome {
		return nil, errors.New("`WITSELF_HOME` must not have leading or trailing whitespace for OpenClaw integration")
	}
	witselfHome, err = cleanAbsoluteOpenClawEnvironmentPath("WITSELF_HOME", witselfHome)
	if err != nil {
		return nil, err
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve default OpenClaw namespace: %w", err)
	}
	userHome, err = cleanAbsoluteOpenClawEnvironmentPath("HOME", userHome)
	if err != nil {
		return nil, err
	}
	// Persist the default namespace as explicit paths too. Otherwise a later
	// HOME change could make reinstall or uninstall operate on a different
	// OpenClaw profile even though no OPENCLAW_* selector was set originally.
	defaultStateDirectory := filepath.Join(userHome, ".openclaw")
	environment := map[string]string{
		"WITSELF_HOME":         witselfHome,
		"OPENCLAW_STATE_DIR":   defaultStateDirectory,
		"OPENCLAW_CONFIG_PATH": filepath.Join(defaultStateDirectory, "openclaw.json"),
	}
	for _, key := range []string{"OPENCLAW_CONFIG_PATH", "OPENCLAW_STATE_DIR"} {
		value := strings.TrimSpace(os.Getenv(key))
		if value == "" {
			continue
		}
		value, err = cleanAbsoluteOpenClawEnvironmentPath(key, value)
		if err != nil {
			return nil, err
		}
		environment[key] = value
	}
	if strings.TrimSpace(os.Getenv("OPENCLAW_CONFIG_PATH")) == "" {
		environment["OPENCLAW_CONFIG_PATH"] = filepath.Join(environment["OPENCLAW_STATE_DIR"], "openclaw.json")
	}
	if profile := strings.TrimSpace(os.Getenv("OPENCLAW_PROFILE")); profile != "" {
		if err := validateOpenClawProfile(profile); err != nil {
			return nil, err
		}
		if strings.TrimSpace(os.Getenv("OPENCLAW_STATE_DIR")) == "" {
			directoryName := ".openclaw"
			if !strings.EqualFold(profile, "default") {
				directoryName += "-" + profile
			}
			environment["OPENCLAW_STATE_DIR"] = filepath.Join(userHome, directoryName)
		}
		if strings.TrimSpace(os.Getenv("OPENCLAW_CONFIG_PATH")) == "" {
			environment["OPENCLAW_CONFIG_PATH"] = filepath.Join(environment["OPENCLAW_STATE_DIR"], "openclaw.json")
		}
		environment["OPENCLAW_PROFILE"] = profile
	}
	return environment, nil
}

func legacyOpenClawDefaultEnvironmentCanMigrate(previous, desired map[string]string) bool {
	if len(previous) != 1 || previous["WITSELF_HOME"] == "" || len(desired) != 3 {
		return false
	}
	return previous["WITSELF_HOME"] == desired["WITSELF_HOME"] &&
		desired["OPENCLAW_STATE_DIR"] != "" &&
		desired["OPENCLAW_CONFIG_PATH"] == filepath.Join(desired["OPENCLAW_STATE_DIR"], "openclaw.json") &&
		desired["OPENCLAW_PROFILE"] == ""
}

func cleanAbsoluteOpenClawEnvironmentPath(key, value string) (string, error) {
	if value == "~" || strings.HasPrefix(value, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve %s: %w", key, err)
		}
		if value == "~" {
			value = home
		} else {
			value = filepath.Join(home, strings.TrimPrefix(value, "~/"))
		}
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", key, err)
	}
	absolute = filepath.Clean(absolute)
	if len(absolute) > openClawMCPEnvironmentValueMaxBytes || strings.ContainsAny(absolute, "\x00\r\n") {
		return "", fmt.Errorf("%s resolves to an invalid path", key)
	}
	return absolute, nil
}

func validateOpenClawProfile(profile string) error {
	if len(profile) == 0 || len(profile) > 64 || !isOpenClawProfileLetterOrDigit(profile[0]) {
		return errors.New("`OPENCLAW_PROFILE` must start with an ASCII letter or digit and contain at most 64 letters, digits, hyphens, or underscores")
	}
	for _, value := range []byte(profile) {
		if !isOpenClawProfileLetterOrDigit(value) && value != '-' && value != '_' {
			return errors.New("`OPENCLAW_PROFILE` must start with an ASCII letter or digit and contain at most 64 letters, digits, hyphens, or underscores")
		}
	}
	return nil
}

func isOpenClawProfileLetterOrDigit(value byte) bool {
	return (value >= 'a' && value <= 'z') || (value >= 'A' && value <= 'Z') || (value >= '0' && value <= '9')
}

func openClawMCPBindingFromServeArgs(serveArgs []string) (openClawMCPBinding, error) {
	environment, err := captureOpenClawMCPEnvironment()
	if err != nil {
		return openClawMCPBinding{}, err
	}
	return openClawMCPBindingFromServeArgsAndEnvironment(serveArgs, environment)
}

func openClawMCPBindingFromServeArgsAndEnvironment(serveArgs []string, environment map[string]string) (openClawMCPBinding, error) {
	return openClawMCPBindingFromServeArgsEnvironmentAndTimeout(serveArgs, environment, openClawMCPConnectTimeoutSeconds)
}

func openClawMCPBindingFromServeArgsEnvironmentAndTimeout(serveArgs []string, environment map[string]string, connectTimeout int) (openClawMCPBinding, error) {
	if len(serveArgs) == 0 || !filepath.IsAbs(serveArgs[0]) || filepath.Clean(serveArgs[0]) != serveArgs[0] {
		return openClawMCPBinding{}, errors.New("openclaw MCP command must use a clean absolute executable path")
	}
	if err := validateOpenClawBindingEnvironment(environment, true); err != nil {
		return openClawMCPBinding{}, err
	}
	if connectTimeout <= 0 || connectTimeout > 3600 {
		return openClawMCPBinding{}, errors.New("openclaw MCP connect timeout must be between 1 and 3600 seconds")
	}
	return openClawMCPBinding{
		Command: serveArgs[0], Args: append([]string(nil), serveArgs[1:]...),
		Env: cloneOpenClawEnvironment(environment), ConnectTimeout: connectTimeout,
	}, nil
}

func inspectOpenClawMCPWithEnvironment(runtimeCLI string, environment map[string]string) (openClawMCPBinding, bool, error) {
	binding, exists, _, err := inspectOpenClawMCPState(runtimeCLI, environment)
	return binding, exists, err
}

func inspectOpenClawMCPState(runtimeCLI string, environment map[string]string) (openClawMCPBinding, bool, openClawMCPConfigSnapshot, error) {
	raw, err := openClawCLIJSONWithEnvironment(runtimeCLI, environment, "mcp", "list", "--json")
	if err != nil {
		return openClawMCPBinding{}, false, openClawMCPConfigSnapshot{}, fmt.Errorf("list OpenClaw MCP servers: %w", err)
	}
	if err := rejectDuplicateJSONKeys(raw); err != nil {
		return openClawMCPBinding{}, false, openClawMCPConfigSnapshot{}, fmt.Errorf("parse OpenClaw MCP server list: %w", err)
	}
	var servers map[string]json.RawMessage
	if err := json.Unmarshal(raw, &servers); err != nil || servers == nil {
		if err == nil {
			err = errors.New("root must be a JSON object")
		}
		return openClawMCPBinding{}, false, openClawMCPConfigSnapshot{}, fmt.Errorf("parse OpenClaw MCP server list: %w", err)
	}
	snapshot := openClawMCPConfigSnapshot{nonTarget: map[string]string{}}
	for name, rawDefinition := range servers {
		if name != "witself" && strings.EqualFold(name, "witself") {
			return openClawMCPBinding{}, false, openClawMCPConfigSnapshot{}, fmt.Errorf("OpenClaw MCP server %q collides case-insensitively with witself", name)
		}
		canonical, canonicalErr := canonicalCopilotJSON(rawDefinition)
		if canonicalErr != nil {
			return openClawMCPBinding{}, false, openClawMCPConfigSnapshot{}, fmt.Errorf("parse OpenClaw MCP server %s: %w", name, canonicalErr)
		}
		if name == "witself" {
			snapshot.targetExists = true
			snapshot.target = canonical
		} else {
			snapshot.nonTarget[name] = canonical
		}
	}
	definition, exists := servers["witself"]
	if !exists {
		return openClawMCPBinding{}, false, snapshot, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(definition, &fields); err != nil {
		return openClawMCPBinding{}, false, openClawMCPConfigSnapshot{}, fmt.Errorf("parse OpenClaw-managed mcp.servers.witself registration: %w", err)
	}
	for field := range fields {
		switch field {
		case "command", "args", "env", "connectTimeout":
		default:
			return openClawMCPBinding{}, false, openClawMCPConfigSnapshot{}, errors.New("openclaw-managed mcp.servers.witself has a non-standard registration; refusing to modify it")
		}
	}
	if fields["command"] == nil || fields["args"] == nil {
		return openClawMCPBinding{}, false, openClawMCPConfigSnapshot{}, errors.New("openclaw-managed mcp.servers.witself has a non-standard registration; refusing to modify it")
	}
	var binding openClawMCPBinding
	if err := json.Unmarshal(definition, &binding); err != nil {
		return openClawMCPBinding{}, false, openClawMCPConfigSnapshot{}, fmt.Errorf("parse OpenClaw-managed mcp.servers.witself registration: %w", err)
	}
	if strings.TrimSpace(binding.Command) == "" || binding.Args == nil || !filepath.IsAbs(binding.Command) || filepath.Clean(binding.Command) != binding.Command {
		return openClawMCPBinding{}, false, openClawMCPConfigSnapshot{}, errors.New("openclaw-managed mcp.servers.witself has an incomplete registration; refusing to modify it")
	}
	if err := validateOpenClawBindingEnvironment(binding.Env, false); err != nil {
		return openClawMCPBinding{}, false, openClawMCPConfigSnapshot{}, fmt.Errorf("openclaw-managed mcp.servers.witself has a non-standard registration; refusing to modify it: %w", err)
	}
	return binding, true, snapshot, nil
}

func equalOpenClawMCPConfigSnapshot(left, right openClawMCPConfigSnapshot) bool {
	return left.targetExists == right.targetExists && left.target == right.target &&
		equalCopilotSemanticFields(left.nonTarget, right.nonTarget)
}

func verifyOpenClawNonTargetPreservation(before, after openClawMCPConfigSnapshot) error {
	if !equalCopilotSemanticFields(before.nonTarget, after.nonTarget) {
		return errors.New("OpenClaw changed sibling MCP servers during the Witself mutation")
	}
	return nil
}

func equalOpenClawMCPBinding(left, right openClawMCPBinding) bool {
	if left.Command != right.Command || left.ConnectTimeout != right.ConnectTimeout || len(left.Args) != len(right.Args) || !equalOpenClawMCPEnvironment(left.Env, right.Env) {
		return false
	}
	for index := range left.Args {
		if left.Args[index] != right.Args[index] {
			return false
		}
	}
	return true
}

func equalOpenClawMCPEnvironment(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

func cloneOpenClawEnvironment(environment map[string]string) map[string]string {
	cloned := make(map[string]string, len(environment))
	for key, value := range environment {
		cloned[key] = value
	}
	return cloned
}

func validateOpenClawBindingEnvironment(environment map[string]string, requireWitselfHome bool) error {
	if requireWitselfHome && strings.TrimSpace(environment["WITSELF_HOME"]) == "" {
		return errors.New("openclaw MCP environment requires WITSELF_HOME")
	}
	for key, value := range environment {
		switch key {
		case "WITSELF_HOME", "OPENCLAW_CONFIG_PATH", "OPENCLAW_STATE_DIR":
			if value == "" || len(value) > openClawMCPEnvironmentValueMaxBytes || strings.ContainsAny(value, "\x00\r\n") || !filepath.IsAbs(value) || filepath.Clean(value) != value {
				return fmt.Errorf("openclaw MCP environment %s must be a clean absolute path", key)
			}
		case "OPENCLAW_PROFILE":
			if err := validateOpenClawProfile(value); err != nil {
				return err
			}
		default:
			return fmt.Errorf("openclaw MCP environment key %q is not allowed", key)
		}
	}
	return nil
}

func openClawMCPBindingFromConfig(executable string, cfg transcriptcapture.Config) (openClawMCPBinding, error) {
	if strings.TrimSpace(cfg.MCPCommand) != "" {
		executable = strings.TrimSpace(cfg.MCPCommand)
	}
	account := strings.TrimSpace(cfg.Account)
	if account == "" {
		account = "default"
	}
	realm := strings.TrimSpace(cfg.Realm)
	if realm == "" {
		realm = "default"
	}
	agent := strings.TrimSpace(cfg.Agent)
	if agent == "" {
		agent = strings.TrimSpace(cfg.AgentName)
	}
	if agent == "" {
		return openClawMCPBinding{}, errors.New("installed OpenClaw integration has no agent name")
	}
	return openClawMCPBindingFromServeArgsEnvironmentAndTimeout(runtimeMCPServeArgs(
		transcriptcapture.RuntimeOpenClaw,
		executable,
		account,
		realm,
		agent,
		cfg.Location.Name,
	), cfg.MCPEnvironment, cfg.MCPConnectTimeoutSeconds)
}

type openClawMCPInstallPlan struct {
	desired          openClawMCPBinding
	expected         openClawMCPConfigSnapshot
	registerRequired bool
}

// prepareOpenClawMCPInstallPlan permits replacement only when the live
// definition exactly matches the prior durable integration. The returned plan
// fences register against the exact expected post-prepare state: either the
// desired binding is already owned by the same durable config, or the name must
// remain absent until this process invokes the provider CLI.
func prepareOpenClawMCPInstallPlan(runtimeCLI, executable string, desired transcriptcapture.Config, previous *transcriptcapture.Config) (openClawMCPInstallPlan, bool, error) {
	desiredBinding, err := openClawMCPBindingFromConfig(executable, desired)
	if err != nil {
		return openClawMCPInstallPlan{}, false, err
	}
	current, exists, currentSnapshot, err := inspectOpenClawMCPState(runtimeCLI, desired.MCPEnvironment)
	if err != nil {
		return openClawMCPInstallPlan{}, false, err
	}
	plan := openClawMCPInstallPlan{
		desired: desiredBinding, expected: currentSnapshot, registerRequired: true,
	}
	if !exists {
		return plan, false, nil
	}
	if previous == nil {
		return openClawMCPInstallPlan{}, false, errors.New("openclaw-managed mcp.servers.witself exists without a Witself integration record; refusing to claim or replace it")
	}
	previousBinding, err := openClawMCPBindingFromConfig(executable, *previous)
	if err != nil {
		return openClawMCPInstallPlan{}, false, err
	}
	if equalOpenClawMCPBinding(current, desiredBinding) {
		if !equalOpenClawMCPBinding(previousBinding, desiredBinding) {
			return openClawMCPInstallPlan{}, false, errors.New("openclaw-managed mcp.servers.witself matches the requested binding but not the prior durable integration; refusing to claim an unjournaled interrupted rebind")
		}
		plan.registerRequired = false
		return plan, false, nil
	}
	if !equalOpenClawMCPBinding(current, previousBinding) {
		return openClawMCPInstallPlan{}, false, errors.New("openclaw-managed mcp.servers.witself differs from both the prior and requested bindings; refusing to replace it")
	}
	touched, err := unregisterOpenClawMCPWithSnapshot(runtimeCLI, &previousBinding, &currentSnapshot)
	if err != nil {
		return openClawMCPInstallPlan{}, touched, err
	}
	_, exists, expected, err := inspectOpenClawMCPState(runtimeCLI, desired.MCPEnvironment)
	if err != nil {
		return openClawMCPInstallPlan{}, touched, err
	}
	if exists {
		return openClawMCPInstallPlan{}, touched, &providerMutationUncertainError{err: errors.New("OpenClaw MCP binding reappeared after removal; refusing unsafe recovery")}
	}
	plan.expected = expected
	return plan, touched, nil
}

func registerOpenClawMCP(runtimeCLI string, serveArgs []string) error {
	desired, err := openClawMCPBindingFromServeArgs(serveArgs)
	if err != nil {
		return err
	}
	return registerOpenClawMCPBinding(runtimeCLI, desired)
}

func registerOpenClawMCPBinding(runtimeCLI string, desired openClawMCPBinding) error {
	current, exists, snapshot, err := inspectOpenClawMCPState(runtimeCLI, desired.Env)
	if err != nil {
		return err
	}
	if exists {
		if equalOpenClawMCPBinding(current, desired) {
			return nil
		}
		return errors.New("openclaw-managed mcp.servers.witself has a foreign registration; refusing to replace it")
	}
	_, err = registerOpenClawMCPBindingWithPlan(runtimeCLI, openClawMCPInstallPlan{
		desired: desired, expected: snapshot, registerRequired: true,
	})
	return err
}

func registerOpenClawMCPBindingWithPlan(runtimeCLI string, plan openClawMCPInstallPlan) (bool, error) {
	desired := plan.desired
	if err := validateOpenClawBindingEnvironment(desired.Env, true); err != nil {
		return false, err
	}
	if desired.ConnectTimeout <= 0 {
		return false, errors.New("openclaw MCP connect timeout must be positive")
	}
	current, exists, before, err := inspectOpenClawMCPState(runtimeCLI, desired.Env)
	if err != nil {
		return false, err
	}
	if !equalOpenClawMCPConfigSnapshot(plan.expected, before) {
		return false, &providerPreflightChangedError{err: errors.New("OpenClaw MCP registry changed after preflight; refusing to claim, replace, or remove it")}
	}
	if !plan.registerRequired {
		if !exists || !equalOpenClawMCPBinding(current, desired) {
			return false, &providerPreflightChangedError{err: errors.New("OpenClaw MCP binding changed after preflight; refusing to claim, replace, or remove it")}
		}
		return false, nil
	}
	if exists {
		return false, &providerPreflightChangedError{err: errors.New("OpenClaw MCP binding appeared after preflight; refusing to claim, replace, or remove it")}
	}
	args := []string{
		"mcp", "add", "witself", "--command", desired.Command, "--no-probe",
		"--connect-timeout", strconv.Itoa(desired.ConnectTimeout),
	}
	keys := make([]string, 0, len(desired.Env))
	for key := range desired.Env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		args = append(args, "--env", key+"="+desired.Env[key])
	}
	for _, value := range desired.Args {
		args = append(args, "--arg", value)
	}
	ctx, cancel := context.WithTimeout(context.Background(), openClawCLIMutationTimeout)
	defer cancel()
	output, err := runOpenClawMutationCommand(ctx, runtimeCLI, desired.Env, args...)
	if err != nil {
		if ctx.Err() != nil {
			return false, &providerMutationUncertainError{err: fmt.Errorf("add OpenClaw MCP server timed out after %s: %w", openClawCLIMutationTimeout, ctx.Err())}
		}
		if _, _, after, inspectErr := inspectOpenClawMCPState(runtimeCLI, desired.Env); inspectErr == nil && equalOpenClawMCPConfigSnapshot(before, after) {
			return false, fmt.Errorf("add OpenClaw MCP server: %w: %s", err, strings.TrimSpace(string(output)))
		}
		return false, &providerMutationUncertainError{err: fmt.Errorf("add OpenClaw MCP server failed and its post-state could not be proven: %w: %s", err, strings.TrimSpace(string(output)))}
	}
	current, exists, after, err := inspectOpenClawMCPState(runtimeCLI, desired.Env)
	if err != nil {
		return true, &providerMutationUncertainError{err: fmt.Errorf("verify OpenClaw MCP registration: %w", err)}
	}
	if !exists || !equalOpenClawMCPBinding(current, desired) {
		return true, &providerMutationUncertainError{err: errors.New("openclaw did not retain the exact Witself stdio MCP registration after a successful add")}
	}
	if err := verifyOpenClawNonTargetPreservation(before, after); err != nil {
		return true, &providerMutationUncertainError{err: err}
	}
	return true, nil
}

func unregisterOpenClawMCP(runtimeCLI string, expected *openClawMCPBinding) error {
	_, err := unregisterOpenClawMCPWithMutation(runtimeCLI, expected)
	return err
}

func unregisterOpenClawMCPWithMutation(runtimeCLI string, expected *openClawMCPBinding) (bool, error) {
	return unregisterOpenClawMCPWithSnapshot(runtimeCLI, expected, nil)
}

func unregisterOpenClawMCPWithSnapshot(runtimeCLI string, expected *openClawMCPBinding, expectedSnapshot *openClawMCPConfigSnapshot) (bool, error) {
	var environment map[string]string
	if expected != nil {
		environment = expected.Env
	}
	current, exists, before, err := inspectOpenClawMCPState(runtimeCLI, environment)
	if err != nil {
		return false, err
	}
	if expectedSnapshot != nil && !equalOpenClawMCPConfigSnapshot(*expectedSnapshot, before) {
		return false, &providerPreflightChangedError{err: errors.New("OpenClaw MCP registry changed after preflight; refusing to remove it")}
	}
	if !exists {
		return false, nil
	}
	if expected == nil || !equalOpenClawMCPBinding(current, *expected) {
		return false, errors.New("openclaw-managed mcp.servers.witself does not match the installed binding; refusing to remove it")
	}
	ctx, cancel := context.WithTimeout(context.Background(), openClawCLIMutationTimeout)
	defer cancel()
	output, err := runOpenClawMutationCommand(ctx, runtimeCLI, environment, "mcp", "unset", "witself")
	if err != nil {
		if ctx.Err() != nil {
			return false, &providerMutationUncertainError{err: fmt.Errorf("remove OpenClaw MCP server timed out after %s: %w", openClawCLIMutationTimeout, ctx.Err())}
		}
		afterBinding, afterExists, after, inspectErr := inspectOpenClawMCPState(runtimeCLI, environment)
		if inspectErr == nil && afterExists && equalOpenClawMCPBinding(afterBinding, *expected) &&
			equalOpenClawMCPConfigSnapshot(before, after) {
			return false, fmt.Errorf("remove OpenClaw MCP server: %w: %s", err, strings.TrimSpace(string(output)))
		}
		return false, &providerMutationUncertainError{err: fmt.Errorf("remove OpenClaw MCP server failed and its post-state could not be proven: %w: %s", err, strings.TrimSpace(string(output)))}
	}
	if _, exists, after, err := inspectOpenClawMCPState(runtimeCLI, environment); err != nil {
		return true, &providerMutationUncertainError{err: fmt.Errorf("verify OpenClaw MCP removal: %w", err)}
	} else if exists {
		return true, &providerMutationUncertainError{err: errors.New("openclaw retained or replaced the Witself MCP registration after a successful removal")}
	} else if err := verifyOpenClawNonTargetPreservation(before, after); err != nil {
		return true, &providerMutationUncertainError{err: err}
	}
	return true, nil
}

func runOpenClawMutationCommand(
	ctx context.Context,
	runtimeCLI string,
	environment map[string]string,
	args ...string,
) ([]byte, error) {
	cmd := openClawCommandContext(ctx, runtimeCLI, environment, args...)
	cleanup, err := isolateProviderCLIWorkingDirectory(cmd, "OpenClaw")
	if err != nil {
		return nil, err
	}
	defer cleanup()
	output := &antigravityValidationOutput{limit: genericProviderCLIOutputLimit}
	cmd.Stdout = output
	cmd.Stderr = output
	err = cmd.Run()
	return []byte(output.String()), err
}
