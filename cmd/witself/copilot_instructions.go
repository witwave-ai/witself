package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	copilotMemoryRoutingRuleFile = "witself-memory-routing.instructions.md"

	// Copilot instruction files require frontmatter at byte zero. Keeping the
	// opening delimiter in the begin marker lets the generic exact-owned file
	// lifecycle reject any prefix, suffix, or foreign content.
	copilotMemoryRoutingBeginMarker = "---\n# BEGIN WITSELF MANAGED MEMORY ROUTING"
	copilotMemoryRoutingEndMarker   = "<!-- END WITSELF MANAGED MEMORY ROUTING -->"

	// Copilot's native memory surface is runtime-dependent, so its filesystem
	// rule uses the portable runtime-neutral contract. Exact tool names are also
	// supplied through the MCP server instructions.
	copilotMemoryRoutingInstructions = runtimeNeutralMemoryRoutingInstructions
)

var copilotMemoryRoutingBlock = []byte(
	copilotMemoryRoutingBeginMarker + "\n" +
		"applyTo: \"**\"\n" +
		"---\n" +
		copilotMemoryRoutingInstructions + "\n\n" +
		foregroundMessagingRoutingInstructions + "\n\n" +
		avatarRoutingInstructions + "\n\n" +
		secretRoutingInstructions + "\n" +
		copilotMemoryRoutingEndMarker,
)

// currentCopilotConfigRoot resolves the exact COPILOT_HOME passed to every
// Copilot CLI operation. Persisting this value prevents a later shell from
// silently targeting a different user-level MCP registry.
func currentCopilotConfigRoot() (string, error) {
	root := os.Getenv("COPILOT_HOME")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve GitHub Copilot home: %w", err)
		}
		root = filepath.Join(home, ".copilot")
	}
	return cleanCopilotAbsolutePath("COPILOT_HOME", root)
}

func copilotMemoryRoutingPath() (string, error) {
	root, err := currentCopilotConfigRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "instructions", copilotMemoryRoutingRuleFile), nil
}

func copilotManagedInstructionsSpec() (managedInstructionsSpec, error) {
	path, err := copilotMemoryRoutingPath()
	if err != nil {
		return managedInstructionsSpec{}, err
	}
	return managedInstructionsSpec{
		path:        path,
		fileName:    copilotMemoryRoutingRuleFile,
		tempPattern: ".witself-memory-routing.instructions.md.witself-*",
		beginMarker: copilotMemoryRoutingBeginMarker,
		endMarker:   copilotMemoryRoutingEndMarker,
		block:       copilotMemoryRoutingBlock,
		removeEmpty: true,
		exclusive:   true,
	}, nil
}

func cleanCopilotAbsolutePath(label, value string) (string, error) {
	if value == "" || strings.TrimSpace(value) != value ||
		strings.ContainsAny(value, "\x00\r\n") || len(value) > 4096 {
		return "", fmt.Errorf("%s must be a non-empty path without surrounding whitespace", label)
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", label, err)
	}
	absolute = filepath.Clean(absolute)
	if !filepath.IsAbs(absolute) {
		return "", fmt.Errorf("%s must resolve to an absolute path", label)
	}

	// Canonicalize the longest existing prefix. The instructions directory and
	// MCP config may not exist on first install, while an existing symlinked
	// home still needs one stable persisted identity.
	missing := make([]string, 0, 4)
	probe := absolute
	for {
		if _, err := os.Lstat(probe); err == nil {
			resolved, err := filepath.EvalSymlinks(probe)
			if err != nil {
				return "", fmt.Errorf("resolve %s symlinks: %w", label, err)
			}
			for index := len(missing) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, missing[index])
			}
			return filepath.Clean(resolved), nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("inspect %s: %w", label, err)
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return "", fmt.Errorf("resolve an existing ancestor for %s", label)
		}
		missing = append(missing, filepath.Base(probe))
		probe = parent
	}
}
