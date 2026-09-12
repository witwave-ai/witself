package transcriptcapture

import (
	"encoding/json"
	"strings"
)

// normalizeDSHToolName interprets the dsh MCP bridge's identity suffix only at
// the dsh boundary. The bridge rewrites unsupported name characters and caps
// long names, appending an underscore and twelve lowercase hex characters.
// Keep the original name on capture events; this spelling is for privacy
// classification of direct, wrapper, and shell tools only.
func normalizeDSHToolName(name string) string {
	name = strings.TrimSpace(name)
	const digestLength = 12
	if len(name) < digestLength+2 || name[len(name)-digestLength-1] != '_' {
		return name
	}
	for _, character := range name[len(name)-digestLength:] {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return name
		}
	}
	return name[:len(name)-digestLength-1]
}

func protectDSHSensitiveToolPayload(input *hookInput, state *sessionState) {
	if input != nil && state != nil && isToolHookEvent(input.HookEventName) &&
		dshSensitiveToolPayload(input.ToolName, input.ToolInput, input.ToolResponse) {
		// The shared machinery owns correlation and pending-turn redaction;
		// only recognition of the provider's published names is dsh-specific.
		state.SensitiveTurn = true
	}
}

// dshSensitiveToolPayload is shared by dsh hooks and native-log recovery so
// every tool class sees exactly the same bridge-name normalization.
func dshSensitiveToolPayload(name string, payloads ...json.RawMessage) bool {
	name = normalizeDSHToolName(name)
	if sensitiveSealedToolName(name) || dshDelegationToolName(name) {
		return true
	}
	wrapper, shell := mcpWrapperToolName(name), shellToolName(name)
	for _, raw := range payloads {
		if wrapper && dshRawContainsSensitiveToolName(raw) {
			return true
		}
		if shell && rawContainsSensitiveCLICommand(raw) {
			return true
		}
	}
	return false
}

func dshRawContainsSensitiveToolName(raw json.RawMessage) bool {
	var value any
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil {
		return false
	}
	return dshValueContainsSensitiveToolName(value)
}

// Wrapper payloads can name another published bridge tool. Keep that
// normalization here too: another runtime's ordinary digest-shaped names
// retain the shared scanner's original semantics.
func dshValueContainsSensitiveToolName(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			switch compactName(key) {
			case "tool", "toolname", "mcptool", "mcptoolname", "function", "functionname", "method", "name":
				if name, ok := item.(string); ok && sensitiveWrappedToolName(normalizeDSHToolName(name)) {
					return true
				}
			}
			if dshValueContainsSensitiveToolName(item) {
				return true
			}
		}
	case []any:
		for _, item := range typed {
			if dshValueContainsSensitiveToolName(item) {
				return true
			}
		}
	}
	return false
}
