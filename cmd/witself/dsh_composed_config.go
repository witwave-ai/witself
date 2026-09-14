package main

import (
	"bytes"
	"errors"
	"io"
	"net/url"
	"path"
	"path/filepath"
	"strings"

	"github.com/witwave-ai/witself/internal/transcriptcapture"
	"go.yaml.in/yaml/v3"
)

// Read nodes only: Decode into application objects could resolve aliases or
// merge keys, and a provider loader would evaluate !!js. Foreign data is never
// executed or serialized. Diagnostics deliberately contain no submitted values.
func dshDumpNode(raw []byte) (*yaml.Node, error) {
	if len(raw) > dshCLIOutputLimit {
		return nil, errors.New("DeepSeek Harness composed config exceeds the read bound")
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	var document, extra yaml.Node
	if decoder.Decode(&document) != nil || len(document.Content) != 1 {
		return nil, errors.New("invalid DeepSeek Harness composed YAML")
	}
	if decoder.Decode(&extra) != io.EOF {
		return nil, errors.New("ambiguous DeepSeek Harness composed YAML documents")
	}
	count := 0
	var bounded func(*yaml.Node, int) bool
	bounded = func(n *yaml.Node, depth int) bool {
		count++
		if depth > 64 || count > 50000 {
			return false
		}
		for _, child := range n.Content {
			if !bounded(child, depth+1) {
				return false
			}
		}
		return true
	}
	if !bounded(&document, 0) {
		return nil, errors.New("DeepSeek Harness composed YAML exceeds traversal bounds")
	}
	return document.Content[0], nil
}

func dshNodeMap(n *yaml.Node) (map[string]*yaml.Node, bool) {
	if n == nil || n.Kind != yaml.MappingNode || n.Tag != "!!map" {
		return nil, false
	}
	fields := map[string]*yaml.Node{}
	for i := 0; i < len(n.Content); i += 2 {
		key := n.Content[i]
		if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || fields[key.Value] != nil {
			return nil, false
		}
		fields[key.Value] = n.Content[i+1]
	}
	return fields, true
}

func dshNodeLiteral(n *yaml.Node, tag, value string) bool {
	return n != nil && n.Kind == yaml.ScalarNode && n.Tag == tag && n.Value == value
}

func dshExactLiteralMap(n *yaml.Node, want map[string]string) bool {
	if n == nil {
		return len(want) == 0
	}
	fields, ok := dshNodeMap(n)
	if !ok || len(fields) != len(want) {
		return false
	}
	for key, value := range want {
		tag := "!!str"
		if value == "true" {
			tag = "!!bool"
		}
		if !dshNodeLiteral(fields[key], tag, value) {
			return false
		}
	}
	return true
}

// Only loader entries count. The fixture wrapper is supported at the document
// root; arbitrary plugin config maps cannot donate managed rows or fields.
func dshDumpEntries(root *yaml.Node) (*yaml.Node, bool) {
	if root.Kind == yaml.SequenceNode && root.Tag == "!!seq" {
		return root, true
	}
	fields, ok := dshNodeMap(root)
	if !ok {
		return nil, false
	}
	entries := fields["plugins"]
	if entries == nil || entries.Kind != yaml.SequenceNode || entries.Tag != "!!seq" {
		return nil, false
	}
	return entries, true
}

// The installed Include mounts file-backed children and applies its own patches;
// --dump-config leaves that effective subtree unseen. Recognize the builtin,
// package name, and direct module paths without loading files, resolving symlinks,
// or importing arbitrary plugins. This is not a universal plugin classifier.
func dshExecutableInclude(name string) bool {
	const packageName = "@deepseek-ai/cordis-plugin-include"
	if name == "cordis:include" || name == packageName {
		return true
	}
	modulePath := filepath.ToSlash(name)
	if filepath.IsAbs(name) || strings.HasPrefix(name, "/") {
		// Named groups can also leave absolute names unanchored. Node import
		// resolves POSIX absolute specifiers as URLs, including search/escapes.
		if module, err := url.Parse(modulePath); err == nil && module.Scheme == "" && module.Opaque == "" {
			modulePath = module.Path
		}
	} else {
		module, err := url.Parse(name)
		if err != nil || module.Opaque != "" {
			return false
		}
		switch module.Scheme {
		case "file":
			if !path.IsAbs(module.Path) {
				return false
			}
		case "":
			// Named groups need no group flag, so their nested names can miss
			// overlay anchoring. Loader resolves dot-prefixed names as URLs.
			if !strings.HasPrefix(name, ".") {
				return false
			}
		default:
			return false
		}
		// Decode once: an anchored file URL's encoded literal percent/query
		// characters remain pathname data rather than another URL to resolve.
		modulePath = module.Path
	}
	// Normalize dot segments without consulting the filesystem.
	return strings.HasSuffix(path.Clean(modulePath), "/"+packageName+"/lib/index.js")
}

func validateDSHPrivateComposedConfig(raw []byte, cfg transcriptcapture.Config) error {
	fail := func() error {
		return errors.New("DeepSeek Harness composed config has a missing, ambiguous, or overridden private capture boundary")
	}
	root, err := dshDumpNode(raw)
	if err != nil {
		return err
	}
	entries, ok := dshDumpEntries(root)
	if !ok {
		return fail()
	}
	type expectedRow struct {
		name            string
		isolate, config map[string]string
	}
	expected := map[string]expectedRow{
		dshPatchRowID:       {name: dshMCPClientPluginName},
		dshHooksPolicyRowID: {dshHooksPolicyPlugin, map[string]string{"sandboxPolicy": dshCapturePolicy, "systemPrompt": "true"}, map[string]string{"mode": "workspace-write", "workspaceRoot": cfg.MCPEnvironment["WITSELF_HOME"]}},
		dshHooksShellRowID:  {dshHooksShellPlugin, map[string]string{"shell": dshCaptureShell, "sandboxPolicy": dshCapturePolicy, "settings": "true"}, nil},
		dshHooksPatchRowID:  {dshHooksBridgePluginName, map[string]string{"shell": dshCaptureShell}, map[string]string{"configPath": cfg.HookConfigPath}},
	}
	if cfg.DSHLegacyHookBridge {
		expected[dshLegacyHooksRowID] = expectedRow{dshHooksBridgePluginName, nil, map[string]string{"configPath": filepath.Join(cfg.RuntimeConfigRoot, dshHookConfigFileName)}}
	}
	managedID := func(id string) bool {
		return id == dshPatchRowID || id == dshHooksPolicyRowID || id == dshHooksShellRowID || id == dshHooksPatchRowID || id == dshLegacyHooksRowID
	}
	seen := map[string]bool{}
	var visit func(*yaml.Node, bool) bool
	visit = func(rows *yaml.Node, top bool) bool {
		if rows == nil || rows.Kind != yaml.SequenceNode || rows.Tag != "!!seq" {
			return false
		}
		for _, row := range rows.Content {
			fields, ok := dshNodeMap(row)
			if !ok {
				return false
			}
			// Loader boundary keys must be literal, including on foreign/group rows:
			// unknown expressions or aliases could select the reserved labels or IDs.
			for _, key := range []string{"id", "name"} {
				if n := fields[key]; n != nil && (n.Kind != yaml.ScalarNode || n.Tag != "!!str") {
					return false
				}
			}
			id, name := "", ""
			if n := fields["id"]; n != nil {
				id = n.Value
			}
			if n := fields["name"]; n != nil {
				name = n.Value
			}
			if dshExecutableInclude(name) {
				// Refuse even disabled carriers; no unseen subtree is certified inert.
				return false
			}
			if fields["<<"] != nil {
				return false
			}
			if n := fields["isolate"]; n != nil {
				isolation, ok := dshNodeMap(n)
				if !ok {
					return false
				}
				for _, v := range isolation {
					if v.Kind != yaml.ScalarNode || (v.Tag != "!!str" && v.Tag != "!!bool") {
						return false
					}
					if (v.Value == dshCaptureShell || v.Value == dshCapturePolicy) && !managedID(id) {
						return false
					}
				}
			}
			want, required := expected[id]
			if managedID(id) {
				if !required || !top || seen[id] || !dshNodeLiteral(fields["name"], "!!str", want.name) {
					return false
				}
				seen[id] = true
				for key := range fields {
					switch key {
					case "id", "name", "config", "isolate", "disabled":
					default:
						return false
					}
				}
				if n := fields["disabled"]; n != nil && !dshNodeLiteral(n, "!!bool", "false") {
					return false
				}
				if !dshExactLiteralMap(fields["isolate"], want.isolate) {
					return false
				}
				// Preserve the MCP row's existing ID/name contract. Capture providers and
				// bridges require exact literal config, with no hidden override fields.
				if id != dshPatchRowID && !dshExactLiteralMap(fields["config"], want.config) {
					return false
				}
			}
			isGroup := name == "cordis:group" || name == "@deepseek-ai/cordis-plugin-group"
			if n := fields["group"]; n != nil {
				if !dshNodeLiteral(n, "!!bool", "true") && !dshNodeLiteral(n, "!!bool", "false") {
					return false
				}
				isGroup = isGroup || n.Value == "true"
			}
			if isGroup {
				if n := fields["disabled"]; n != nil && n.Kind != yaml.ScalarNode {
					return false
				}
				if !visit(fields["config"], false) {
					return false
				}
			}
		}
		return true
	}
	if !visit(entries, true) || len(seen) != len(expected) {
		return fail()
	}
	return nil
}
