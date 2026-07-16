package memoryhydration

import (
	"encoding/json"
	"errors"
	"unicode/utf8"

	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

const maximumClaudeHookOutputCharacters = 10_000

// HookOutput encodes model-visible context in the exact output shape supported
// by the runtime/event pair. Unsupported pairs return an error rather than
// emitting fields that a runtime would silently ignore.
func HookOutput(runtime, event, contextText string) ([]byte, error) {
	if contextText == "" {
		return nil, nil
	}
	switch runtime {
	case transcriptcapture.RuntimeCodex:
		if event != EventSessionStart && event != EventUserPromptSubmit {
			return nil, errors.New("runtime event does not support additional context")
		}
		return json.Marshal(map[string]any{
			"hookSpecificOutput": map[string]any{
				"hookEventName":     event,
				"additionalContext": contextText,
			},
		})
	case transcriptcapture.RuntimeClaudeCode:
		if event != EventSessionStart && event != EventUserPromptSubmit {
			return nil, errors.New("runtime event does not support additional context")
		}
		// Claude Code replaces hook strings above 10,000 characters with a
		// preview and file pointer. Automatic memory must remain directly visible
		// to the model, so fail open instead of silently changing delivery mode.
		if utf8.RuneCountInString(contextText) > maximumClaudeHookOutputCharacters {
			return nil, errors.New("claude hook additional context exceeds 10000 characters")
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
