package main

import (
	"bytes"
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
		return openClawAgent{}, fmt.Errorf("inspect OpenClaw agents: %w", err)
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

func openClawCLIJSONWithEnvironment(runtimeCLI string, environment map[string]string, args ...string) ([]byte, error) {
	return openClawCLIJSONWithTimeout(runtimeCLI, environment, openClawCLIReadTimeout, args...)
}

func openClawCLIJSONWithTimeout(runtimeCLI string, environment map[string]string, timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := openClawCommandContext(ctx, runtimeCLI, environment, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	raw, err := cmd.Output()
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
	if len(raw) > openClawCLIJSONLimit {
		return nil, fmt.Errorf("openclaw JSON output exceeds %d bytes", openClawCLIJSONLimit)
	}
	return raw, nil
}

func openClawCommandContext(ctx context.Context, runtimeCLI string, environment map[string]string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, runtimeCLI, args...)
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
	environment := map[string]string{"WITSELF_HOME": witselfHome}
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
	if profile := strings.TrimSpace(os.Getenv("OPENCLAW_PROFILE")); profile != "" {
		if err := validateOpenClawProfile(profile); err != nil {
			return nil, err
		}
		if environment["OPENCLAW_STATE_DIR"] == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, fmt.Errorf("derive OPENCLAW_STATE_DIR for profile %s: %w", profile, err)
			}
			directoryName := ".openclaw"
			if !strings.EqualFold(profile, "default") {
				directoryName += "-" + profile
			}
			environment["OPENCLAW_STATE_DIR"] = filepath.Join(home, directoryName)
		}
		if environment["OPENCLAW_CONFIG_PATH"] == "" {
			environment["OPENCLAW_CONFIG_PATH"] = filepath.Join(environment["OPENCLAW_STATE_DIR"], "openclaw.json")
		}
		environment["OPENCLAW_PROFILE"] = profile
	}
	return environment, nil
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
	raw, err := openClawCLIJSONWithEnvironment(runtimeCLI, environment, "mcp", "list", "--json")
	if err != nil {
		return openClawMCPBinding{}, false, fmt.Errorf("list OpenClaw MCP servers: %w", err)
	}
	var servers map[string]json.RawMessage
	if err := json.Unmarshal(raw, &servers); err != nil {
		return openClawMCPBinding{}, false, fmt.Errorf("parse OpenClaw MCP server list: %w", err)
	}
	definition, exists := servers["witself"]
	if !exists {
		return openClawMCPBinding{}, false, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(definition, &fields); err != nil {
		return openClawMCPBinding{}, false, fmt.Errorf("parse OpenClaw-managed mcp.servers.witself registration: %w", err)
	}
	for field := range fields {
		switch field {
		case "command", "args", "env", "connectTimeout":
		default:
			return openClawMCPBinding{}, false, errors.New("openclaw-managed mcp.servers.witself has a non-standard registration; refusing to modify it")
		}
	}
	if fields["command"] == nil || fields["args"] == nil {
		return openClawMCPBinding{}, false, errors.New("openclaw-managed mcp.servers.witself has a non-standard registration; refusing to modify it")
	}
	var binding openClawMCPBinding
	if err := json.Unmarshal(definition, &binding); err != nil {
		return openClawMCPBinding{}, false, fmt.Errorf("parse OpenClaw-managed mcp.servers.witself registration: %w", err)
	}
	if strings.TrimSpace(binding.Command) == "" || binding.Args == nil || !filepath.IsAbs(binding.Command) || filepath.Clean(binding.Command) != binding.Command {
		return openClawMCPBinding{}, false, errors.New("openclaw-managed mcp.servers.witself has an incomplete registration; refusing to modify it")
	}
	if err := validateOpenClawBindingEnvironment(binding.Env, false); err != nil {
		return openClawMCPBinding{}, false, fmt.Errorf("openclaw-managed mcp.servers.witself has a non-standard registration; refusing to modify it: %w", err)
	}
	return binding, true, nil
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

// prepareOpenClawMCPInstall permits replacement only when the live definition
// exactly matches the prior durable integration. With no prior record, any
// existing `witself` name is foreign even if its command happens to look right.
func prepareOpenClawMCPInstall(runtimeCLI, executable string, desired transcriptcapture.Config, previous *transcriptcapture.Config) (bool, error) {
	current, exists, err := inspectOpenClawMCPWithEnvironment(runtimeCLI, desired.MCPEnvironment)
	if err != nil || !exists {
		return false, err
	}
	if previous == nil {
		return false, errors.New("openclaw-managed mcp.servers.witself exists without a Witself integration record; refusing to claim or replace it")
	}
	desiredBinding, err := openClawMCPBindingFromConfig(executable, desired)
	if err != nil {
		return false, err
	}
	if equalOpenClawMCPBinding(current, desiredBinding) {
		return false, nil
	}
	previousBinding, err := openClawMCPBindingFromConfig(executable, *previous)
	if err != nil {
		return false, err
	}
	if !equalOpenClawMCPBinding(current, previousBinding) {
		return false, errors.New("openclaw-managed mcp.servers.witself differs from both the prior and requested bindings; refusing to replace it")
	}
	if err := unregisterOpenClawMCP(runtimeCLI, &previousBinding); err != nil {
		return true, err
	}
	return true, nil
}

func registerOpenClawMCP(runtimeCLI string, serveArgs []string) error {
	desired, err := openClawMCPBindingFromServeArgs(serveArgs)
	if err != nil {
		return err
	}
	return registerOpenClawMCPBinding(runtimeCLI, desired)
}

func registerOpenClawMCPBinding(runtimeCLI string, desired openClawMCPBinding) error {
	if err := validateOpenClawBindingEnvironment(desired.Env, true); err != nil {
		return err
	}
	if desired.ConnectTimeout <= 0 {
		return errors.New("openclaw MCP connect timeout must be positive")
	}
	current, exists, err := inspectOpenClawMCPWithEnvironment(runtimeCLI, desired.Env)
	if err != nil {
		return err
	}
	if exists {
		if equalOpenClawMCPBinding(current, desired) {
			return nil
		}
		return errors.New("openclaw-managed mcp.servers.witself has a foreign registration; refusing to replace it")
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
	output, err := openClawCommandContext(ctx, runtimeCLI, desired.Env, args...).CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("add OpenClaw MCP server timed out after %s: %w", openClawCLIMutationTimeout, ctx.Err())
		}
		return fmt.Errorf("add OpenClaw MCP server: %w: %s", err, strings.TrimSpace(string(output)))
	}
	current, exists, err = inspectOpenClawMCPWithEnvironment(runtimeCLI, desired.Env)
	if err != nil {
		return fmt.Errorf("verify OpenClaw MCP registration: %w", err)
	}
	if !exists || !equalOpenClawMCPBinding(current, desired) {
		return errors.New("openclaw did not persist the exact Witself stdio MCP registration")
	}
	return nil
}

func unregisterOpenClawMCP(runtimeCLI string, expected *openClawMCPBinding) error {
	var environment map[string]string
	if expected != nil {
		environment = expected.Env
	}
	current, exists, err := inspectOpenClawMCPWithEnvironment(runtimeCLI, environment)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if expected == nil || !equalOpenClawMCPBinding(current, *expected) {
		return errors.New("openclaw-managed mcp.servers.witself does not match the installed binding; refusing to remove it")
	}
	ctx, cancel := context.WithTimeout(context.Background(), openClawCLIMutationTimeout)
	defer cancel()
	output, err := openClawCommandContext(ctx, runtimeCLI, environment, "mcp", "unset", "witself").CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("remove OpenClaw MCP server timed out after %s: %w", openClawCLIMutationTimeout, ctx.Err())
		}
		if mcpRegistrationAlreadyMissing(output) {
			return nil
		}
		return fmt.Errorf("remove OpenClaw MCP server: %w: %s", err, strings.TrimSpace(string(output)))
	}
	if _, exists, err := inspectOpenClawMCPWithEnvironment(runtimeCLI, environment); err != nil {
		return fmt.Errorf("verify OpenClaw MCP removal: %w", err)
	} else if exists {
		return errors.New("openclaw retained the Witself MCP registration after removal")
	}
	return nil
}
