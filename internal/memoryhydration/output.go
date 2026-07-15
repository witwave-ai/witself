package memoryhydration

import (
	"encoding/json"
	"errors"

	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

// HookOutput encodes model-visible context in the exact output shape supported
// by the runtime/event pair. Unsupported pairs return an error rather than
// emitting fields that a runtime would silently ignore.
func HookOutput(runtime, event, contextText string) ([]byte, error) {
	if contextText == "" {
		return nil, nil
	}
	switch runtime {
	case transcriptcapture.RuntimeCodex, transcriptcapture.RuntimeClaudeCode:
		if event != EventSessionStart && event != EventUserPromptSubmit {
			return nil, errors.New("runtime event does not support additional context")
		}
		return json.Marshal(map[string]any{
			"hookSpecificOutput": map[string]any{
				"hookEventName":     event,
				"additionalContext": contextText,
			},
		})
	case transcriptcapture.RuntimeCursor:
		return nil, errors.New("cursor additional_context is not a reliable model-visible hydration channel")
	case transcriptcapture.RuntimeGrokBuild:
		return nil, errors.New("grok passive hook output is not model-visible")
	default:
		return nil, errors.New("unknown runtime hydration output contract")
	}
}
