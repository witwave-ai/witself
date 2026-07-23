package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

const (
	grokHookEventEnv       = "GROK_HOOK_EVENT"
	grokJSONOutputLimit    = 8 * 1024 * 1024
	grokCLIOutputWaitDelay = 2 * time.Second
)

type grokInspectSource struct {
	Type string `json:"type"`
	Path string `json:"path"`
}

type grokInspectHook struct {
	Target              string            `json:"target"`
	Source              grokInspectSource `json:"source"`
	Vendor              string            `json:"vendor"`
	CompatibilityStatus string            `json:"compatibilityStatus"`
}

type grokInspectMCPServer struct {
	Name                string            `json:"name"`
	Target              string            `json:"target"`
	Source              grokInspectSource `json:"source"`
	Vendor              string            `json:"vendor"`
	CompatibilityStatus string            `json:"compatibilityStatus"`
}

type grokInspectOutput struct {
	Version    string                 `json:"grokVersion"`
	Hooks      []grokInspectHook      `json:"hooks"`
	MCPServers []grokInspectMCPServer `json:"mcpServers"`
}

type grokCompatibilityFinding struct {
	Vendor string
	Name   string
	Target string
	Source string
}

type grokCompatibilityReport struct {
	Hooks       []grokCompatibilityFinding
	MCPServers  []grokCompatibilityFinding
	Inspected   bool
	InspectPath string
}

// inspectGrokCompatibility asks Grok for its effective configuration rather
// than reading another runtime's files directly. Grok's compatibility layer is
// intentionally broad, so an imported Witself hook or MCP server may otherwise
// execute with a Claude or Cursor binding inside a Grok session.
func inspectGrokCompatibility(runtimeCLI string) (grokCompatibilityReport, error) {
	output, err := runGrokJSONCLI(runtimeCLI, 10*time.Second, "inspect", "--json")
	if err != nil {
		return grokCompatibilityReport{}, fmt.Errorf("run grok inspect --json: %w", err)
	}
	if len(strings.TrimSpace(string(output))) == 0 {
		return grokCompatibilityReport{}, errors.New("grok inspect --json returned no JSON")
	}
	return parseGrokCompatibilityInspection(output)
}

func parseGrokCompatibilityInspection(raw []byte) (grokCompatibilityReport, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return grokCompatibilityReport{}, fmt.Errorf("parse grok inspect --json: %w", err)
	}
	if errorRaw, present := envelope["error"]; present && string(errorRaw) != "null" {
		return grokCompatibilityReport{}, errors.New("parse grok inspect --json: inspection returned an error object")
	}
	versionRaw, versionPresent := envelope["grokVersion"]
	hooksRaw, hooksPresent := envelope["hooks"]
	mcpServersRaw, mcpServersPresent := envelope["mcpServers"]
	var version string
	if !versionPresent || json.Unmarshal(versionRaw, &version) != nil || strings.TrimSpace(version) == "" {
		return grokCompatibilityReport{}, errors.New("parse grok inspect --json: expected a non-empty grokVersion")
	}
	if !hooksPresent || !mcpServersPresent || string(hooksRaw) == "null" || string(mcpServersRaw) == "null" {
		return grokCompatibilityReport{}, errors.New("parse grok inspect --json: expected non-null hooks and mcpServers arrays")
	}
	inspected := grokInspectOutput{Version: version}
	if err := json.Unmarshal(hooksRaw, &inspected.Hooks); err != nil || inspected.Hooks == nil {
		return grokCompatibilityReport{}, errors.New("parse grok inspect --json: hooks must be an array")
	}
	if err := json.Unmarshal(mcpServersRaw, &inspected.MCPServers); err != nil || inspected.MCPServers == nil {
		return grokCompatibilityReport{}, errors.New("parse grok inspect --json: mcpServers must be an array")
	}
	for i, hook := range inspected.Hooks {
		if strings.TrimSpace(hook.Target) == "" || strings.TrimSpace(hook.Source.Type) == "" || strings.TrimSpace(hook.Source.Path) == "" {
			return grokCompatibilityReport{}, fmt.Errorf("parse grok inspect --json: hook entry %d is missing target or source identity", i)
		}
	}
	for i, server := range inspected.MCPServers {
		if strings.TrimSpace(server.Name) == "" || strings.TrimSpace(server.Target) == "" ||
			strings.TrimSpace(server.Source.Type) == "" || strings.TrimSpace(server.Source.Path) == "" {
			return grokCompatibilityReport{}, fmt.Errorf("parse grok inspect --json: MCP server entry %d is missing name, target, or source identity", i)
		}
	}
	report := grokCompatibilityReport{Inspected: true}
	for _, hook := range inspected.Hooks {
		vendor := grokCompatibilityVendor(hook.Vendor, hook.Source.Path)
		if vendor == "" || compatibilityDisabled(hook.CompatibilityStatus) ||
			!grokTranscriptHookCandidate(hook.Target) {
			continue
		}
		report.Hooks = append(report.Hooks, grokCompatibilityFinding{
			Vendor: vendor, Target: hook.Target, Source: hook.Source.Path,
		})
	}
	for _, server := range inspected.MCPServers {
		vendor := grokCompatibilityVendor(server.Vendor, server.Source.Path)
		if vendor == "" || compatibilityDisabled(server.CompatibilityStatus) ||
			!looksLikeWitselfMCPServer(server.Name, server.Target) {
			continue
		}
		report.MCPServers = append(report.MCPServers, grokCompatibilityFinding{
			Vendor: vendor, Name: strings.TrimSpace(server.Name), Target: server.Target, Source: server.Source.Path,
		})
	}
	return report, nil
}

func grokCompatibilityVendor(vendor, sourcePath string) string {
	vendor = strings.ToLower(strings.TrimSpace(vendor))
	if vendor == "claude" || vendor == "cursor" {
		return vendor
	}
	path := strings.ToLower(filepath.ToSlash(sourcePath))
	switch {
	case strings.Contains(path, "/.claude/") || strings.HasSuffix(path, "/.claude"):
		return "claude"
	case strings.Contains(path, "/.cursor/") || strings.HasSuffix(path, "/.cursor"):
		return "cursor"
	default:
		return ""
	}
}

func compatibilityDisabled(status string) bool {
	return strings.EqualFold(strings.TrimSpace(status), "disabled")
}

func looksLikeWitselfMCPServer(name, target string) bool {
	if strings.Contains(strings.ToLower(strings.TrimSpace(name)), "witself") {
		return true
	}
	if strings.Contains(strings.ToLower(target), "witself") {
		return true
	}
	tokens, err := splitGrokCommand(target)
	if err != nil {
		tokens = strings.Fields(target)
	}
	for _, token := range tokens {
		for _, nested := range strings.Fields(token) {
			base := filepath.Base(strings.Trim(strings.TrimSpace(nested), "'\";|&()"))
			if strings.EqualFold(base, "witself") || strings.EqualFold(base, "ws") {
				return true
			}
		}
	}
	return false
}

// validateGrokCompatibilityReport rejects only configurations that cannot be
// made safe by the current executable. It never changes Grok's compatibility
// switches because those switches affect unrelated user hooks and MCP servers.
func validateGrokCompatibilityReport(report grokCompatibilityReport, executable string) error {
	var unsafe []string
	for _, hook := range report.Hooks {
		if !grokHookUsesExecutable(hook.Target, executable) {
			unsafe = append(unsafe, fmt.Sprintf("%s hooks from %s", hook.Vendor, displayGrokSource(hook.Source)))
		}
	}
	for _, server := range report.MCPServers {
		if server.Name != "witself" {
			unsafe = append(unsafe, fmt.Sprintf("%s MCP server %q from %s", server.Vendor, server.Name, displayGrokSource(server.Source)))
		}
	}
	if len(unsafe) == 0 {
		return nil
	}
	sort.Strings(unsafe)
	unsafe = slices.Compact(unsafe)
	return fmt.Errorf(
		"grok compatibility exposes a foreign Witself binding that cannot be isolated: %s; update that runtime's Witself integration to use %s, remove or rename the foreign MCP alias, or explicitly set hooks=false and/or mcps=false under [compat.claude] or [compat.cursor] in $GROK_HOME/config.toml; Witself did not change those broad compatibility settings",
		strings.Join(unsafe, ", "), executable,
	)
}

func grokHookUsesExecutable(target, executable string) bool {
	tokens, err := splitGrokCommand(target)
	if err != nil || len(tokens) < 4 || tokens[1] != "transcript" || tokens[2] != "hook" || tokens[0] != executable {
		return false
	}
	runtimeFlags := 0
	for index := 3; index < len(tokens); index++ {
		switch {
		case tokens[index] == "--runtime":
			if index+1 >= len(tokens) || strings.TrimSpace(tokens[index+1]) == "" {
				return false
			}
			runtimeFlags++
			index++
		case strings.HasPrefix(tokens[index], "--runtime="):
			if strings.TrimSpace(strings.TrimPrefix(tokens[index], "--runtime=")) == "" {
				return false
			}
			runtimeFlags++
		}
	}
	return runtimeFlags == 1
}

func grokTranscriptHookCandidate(target string) bool {
	tokens, err := splitGrokCommand(target)
	if err == nil {
		for index := 0; index+1 < len(tokens); index++ {
			if tokens[index] == "transcript" && tokens[index+1] == "hook" {
				return true
			}
		}
		for _, token := range tokens {
			normalized := strings.ToLower(strings.Join(strings.Fields(token), " "))
			if strings.Contains(normalized, "transcript hook") {
				return true
			}
		}
	}
	normalized := strings.ToLower(strings.Join(strings.Fields(target), " "))
	return strings.Contains(normalized, "transcript hook")
}

// splitGrokCommand parses the small shell-command surface emitted by Grok
// inspect without executing it. Adjacent quoted and unquoted fragments remain
// one token, which handles the standard single-quote escape used for paths.
func splitGrokCommand(value string) ([]string, error) {
	var tokens []string
	var current strings.Builder
	var quote rune
	escaped := false
	inToken := false
	flush := func() {
		if inToken {
			tokens = append(tokens, current.String())
			current.Reset()
			inToken = false
		}
	}
	for _, char := range value {
		if escaped {
			current.WriteRune(char)
			escaped = false
			inToken = true
			continue
		}
		if quote != 0 {
			if char == quote {
				quote = 0
				continue
			}
			if quote == '"' && char == '\\' {
				escaped = true
				continue
			}
			current.WriteRune(char)
			inToken = true
			continue
		}
		switch {
		case char == '\'' || char == '"':
			quote = char
			inToken = true
		case char == '\\':
			escaped = true
			inToken = true
		case unicode.IsSpace(char):
			flush()
		default:
			current.WriteRune(char)
			inToken = true
		}
	}
	if escaped || quote != 0 {
		return nil, errors.New("unterminated Grok inspect command quoting")
	}
	flush()
	return tokens, nil
}

func displayGrokSource(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return "an unknown compatibility source"
	}
	return path
}

func writeGrokCompatibilityWarnings(w io.Writer, report grokCompatibilityReport) {
	counts := map[string]int{}
	for _, hook := range report.Hooks {
		counts[hook.Vendor]++
	}
	vendors := make([]string, 0, len(counts))
	for vendor := range counts {
		vendors = append(vendors, vendor)
	}
	sort.Strings(vendors)
	for _, vendor := range vendors {
		_, _ = fmt.Fprintf(
			w,
			"witself: warning: Grok compatibility imports %d foreign Witself %s hook(s); Grok-originated events are fenced to the grok-build binding. To avoid launching all imported %s hooks, explicitly set hooks=false under [compat.%s] in $GROK_HOME/config.toml.\n",
			counts[vendor], vendor, vendor, vendor,
		)
	}
	for _, server := range report.MCPServers {
		if strings.EqualFold(server.Name, "witself") {
			_, _ = fmt.Fprintf(
				w,
				"witself: warning: Grok compatibility imports a %s Witself MCP named \"witself\"; the native Grok user registration will override it and its exact command will be verified. To disable all imported %s MCPs, explicitly set mcps=false under [compat.%s] in $GROK_HOME/config.toml.\n",
				server.Vendor, server.Vendor, server.Vendor,
			)
		}
	}
}

// foreignGrokCompatibilityHook reports an event launched by Grok's
// Claude/Cursor compatibility loader with a foreign runtime binding. Grok's
// documented hook contract sets GROK_HOOK_EVENT for every hook process.
func foreignGrokCompatibilityHook(runtimeName, grokHookEvent string) bool {
	if strings.TrimSpace(grokHookEvent) == "" {
		return false
	}
	runtimeName, err := transcriptcapture.NormalizeRuntime(runtimeName)
	return err == nil && runtimeName != transcriptcapture.RuntimeGrokBuild
}

type grokMCPListEntry struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
	Enabled *bool    `json:"enabled"`
	Name    string   `json:"name"`
	Scope   string   `json:"scope"`
}

func verifyGrokNativeMCPBinding(runtimeCLI string, serveArgs []string) (bool, error) {
	output, err := runGrokJSONCLI(runtimeCLI, 10*time.Second, "mcp", "list", "--json")
	if err != nil {
		return false, fmt.Errorf("run grok mcp list --json: %w", err)
	}
	if len(strings.TrimSpace(string(output))) == 0 {
		return false, errors.New("grok mcp list --json returned no JSON")
	}
	if err := validateGrokMCPList(output, serveArgs); err != nil {
		return true, err
	}
	return true, nil
}

func runGrokJSONCLI(runtimeCLI string, timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, runtimeCLI, args...)
	cleanup, err := isolateProviderCLIWorkingDirectory(cmd, "Grok")
	if err != nil {
		return nil, err
	}
	defer cleanup()
	cmd.WaitDelay = grokCLIOutputWaitDelay
	stdout := &antigravityValidationOutput{limit: grokJSONOutputLimit}
	stderr := &antigravityValidationOutput{limit: genericProviderCLIOutputLimit}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err = cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("the Grok CLI timed out after %s", timeout)
	}
	if stdout.truncated {
		return nil, fmt.Errorf("the Grok CLI JSON output exceeds %d bytes", grokJSONOutputLimit)
	}
	if err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return nil, errors.New(message)
	}
	return stdout.buffer.Bytes(), nil
}

func validateGrokMCPList(raw []byte, serveArgs []string) error {
	if len(serveArgs) < 2 {
		return errors.New("invalid expected Grok MCP command")
	}
	var entries []grokMCPListEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return fmt.Errorf("parse grok mcp list --json: %w", err)
	}
	var matches []grokMCPListEntry
	for _, entry := range entries {
		command := make([]string, 0, 1+len(entry.Args))
		command = append(command, entry.Command)
		command = append(command, entry.Args...)
		if looksLikeWitselfMCPServer(entry.Name, strings.Join(command, " ")) {
			matches = append(matches, entry)
		}
	}
	if len(matches) != 1 {
		return fmt.Errorf("effective Grok MCP list contains %d Witself registrations; want exactly one native user registration", len(matches))
	}
	entry := matches[0]
	if entry.Name != "witself" || entry.Scope != "user" || entry.Command != serveArgs[0] || !slices.Equal(entry.Args, serveArgs[1:]) ||
		entry.Enabled == nil || !*entry.Enabled {
		return errors.New("effective Grok MCP registration does not match the requested native user binding")
	}
	return nil
}
