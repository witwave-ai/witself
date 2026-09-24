package clientinventory

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

const providerLimit = 256 << 10

func configurationStatus(ctx context.Context, opts Options, cfg record, excluded tokenFileExclusions) (ConfigurationStatus, ConfigurationScope) {
	// Other topologies require separate plugin/workspace/YAML ownership proofs.
	// Never follow their references merely to make an inventory look complete.
	var dir, filename string
	switch cfg.Runtime {
	case RuntimeCodex:
		dir, filename = ".codex", "config.toml"
	case RuntimeClaudeCode:
		dir, filename = ".claude", ".claude.json"
	case RuntimeGrokBuild:
		dir, filename = ".grok", "config.toml"
	case RuntimeCursor:
		dir, filename = ".cursor", "mcp.json"
	default:
		return ConfigurationUnsupported, ScopeNone
	}
	if cfg.RuntimeConfigRoot == "" || cfg.RuntimeMCPConfigPath == "" || cfg.MCPCommand == "" || (cfg.Agent == "" && cfg.AgentName == "") || len(cfg.MCPEnvironment) == 0 {
		return ConfigurationIncomplete, ScopeNone
	}
	// Persisted selection wins; do not rediscover through ambient provider env.
	// Custom roots are intentionally unsupported: the record is not authority
	// to read an arbitrary directory (even one beneath the caller's home).
	if cfg.RuntimeConfigRoot != filepath.Join(opts.Home, dir) {
		return ConfigurationUnsupported, ScopeNone
	}
	rel := filepath.Join(dir, filename)
	if cfg.Runtime == RuntimeClaudeCode && cfg.RuntimeMCPConfigPath == filepath.Join(opts.Home, filename) {
		rel = filename
	}
	expected := filepath.Join(opts.Home, rel)
	if cfg.RuntimeMCPConfigPath != expected || excluded.excludes(expected) {
		return ConfigurationUnsupported, ScopeNone
	}
	if !cleanAbsolute(cfg.MCPCommand) || len(cfg.MCPEnvironment) != 1 || cfg.MCPEnvironment["WITSELF_HOME"] != opts.WitselfHome {
		return ConfigurationIncomplete, ScopeNone
	}
	root, err := openDirectory(opts.Home)
	if err != nil {
		return ConfigurationUnavailable, ScopeMCPRegistration
	}
	defer root.close()
	raw, err := root.read(ctx, filepath.ToSlash(rel), providerLimit)
	if errors.Is(err, errMissing) {
		return ConfigurationIncomplete, ScopeMCPRegistration
	}
	if err != nil {
		return ConfigurationUnavailable, ScopeMCPRegistration
	}
	var doc map[string]any
	container := "mcpServers"
	if cfg.Runtime == RuntimeCodex || cfg.Runtime == RuntimeGrokBuild {
		container = "mcp_servers"
		// Conservative bound on TOML structure before decoding; the parser also
		// bounds nesting. Counting punctuation in strings may safely reject a
		// complex config, never permit an unbounded one.
		count := 0
		for _, b := range raw {
			if strings.ContainsRune("\n.=[]{} ,", rune(b)) {
				count++
			}
		}
		if count > 8192 {
			return ConfigurationUnavailable, ScopeMCPRegistration
		}
		err = toml.Unmarshal(raw, &doc)
	} else {
		if !boundedJSON(raw) {
			return ConfigurationUnavailable, ScopeMCPRegistration
		}
		err = json.Unmarshal(raw, &doc)
	}
	if err != nil || doc == nil {
		return ConfigurationUnavailable, ScopeMCPRegistration
	}
	servers, exists := doc[container]
	if !exists {
		return ConfigurationIncomplete, ScopeMCPRegistration
	}
	serverMap, ok := servers.(map[string]any)
	if !ok {
		return ConfigurationUnavailable, ScopeMCPRegistration
	}
	for key := range serverMap {
		if key != "witself" && strings.EqualFold(key, "witself") {
			return ConfigurationChanged, ScopeMCPRegistration
		}
	}
	target, exists := serverMap["witself"]
	if !exists {
		return ConfigurationIncomplete, ScopeMCPRegistration
	}
	fields, ok := target.(map[string]any)
	if !ok {
		return ConfigurationChanged, ScopeMCPRegistration
	}
	for key := range fields {
		allowed := key == "command" || key == "args" || key == "env" || key == "enabled"
		if cfg.Runtime == RuntimeClaudeCode || cfg.Runtime == RuntimeCursor {
			allowed = allowed || key == "type"
		}
		if !allowed {
			return ConfigurationChanged, ScopeMCPRegistration
		}
	}
	if enabled, exists := fields["enabled"]; exists && enabled != true || !exists && cfg.Runtime == RuntimeGrokBuild {
		return ConfigurationChanged, ScopeMCPRegistration
	}
	if kind, exists := fields["type"]; exists && kind != "stdio" || !exists && cfg.Runtime == RuntimeClaudeCode {
		return ConfigurationChanged, ScopeMCPRegistration
	}
	account, realm, agent := cfg.Account, cfg.Realm, cfg.Agent
	if account == "" {
		account = "default"
	}
	if realm == "" {
		realm = "default"
	}
	if agent == "" {
		agent = cfg.AgentName
	}
	args := []any{"mcp", "serve", "--runtime", string(cfg.Runtime), "--account", account, "--realm", realm, "--agent", agent}
	if cfg.Location.Name != "" {
		args = append(args, "--location", cfg.Location.Name)
	}
	env := map[string]any{"WITSELF_HOME": opts.WitselfHome}
	if fields["command"] != cfg.MCPCommand || !reflect.DeepEqual(fields["args"], args) || !reflect.DeepEqual(fields["env"], env) {
		return ConfigurationChanged, ScopeMCPRegistration
	}
	return ConfigurationMatch, ScopeMCPRegistration
}
