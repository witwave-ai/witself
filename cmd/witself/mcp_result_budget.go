package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/witwave-ai/witself/internal/client"
)

// Byte budgets keep every MCP tool result inside what real AI-runtime MCP
// clients actually deliver: an oversized frame does not fail gracefully, it
// drops the whole connection and strands the caller mid-protocol. The
// transcript tools elide oversized entry content in band (stored entries are
// unchanged and remain fully readable through the witself CLI), and a
// last-resort middleware converts any other oversized tool result into an
// in-band tool error. The typed-tool path serializes structured output twice
// per frame (structured content plus the spec-suggested text fallback), so a
// frame carries roughly double the structured bytes plus JSON escaping.
const (
	maxMCPTranscriptPageBytes = 512 * 1024
	maxMCPToolResultBytes     = 8 << 20
)

// boundMCPTranscriptEntries elides oversized entry content from one
// transcript page in place. Membership stays exact: every returned sequence
// keeps its identity fields, and an in-band note documents each elision so
// the caller can re-read that entry alone with a tighter window or fall back
// to the CLI for full fidelity.
func boundMCPTranscriptEntries(entries []client.TranscriptEntry) {
	remaining := maxMCPTranscriptPageBytes
	for i := range entries {
		remaining -= boundMCPTranscriptEntry(&entries[i], remaining)
	}
}

// boundMCPTranscriptEntry rewrites oversized fields in place and returns the
// bytes the bounded entry still carries. Once the shared page budget is
// spent, later entries keep only elision notes.
func boundMCPTranscriptEntry(entry *client.TranscriptEntry, remaining int) int {
	if remaining < 0 {
		remaining = 0
	}
	if len(entry.Body) > remaining {
		prefix := truncateMCPUTF8(entry.Body, remaining)
		entry.Body = prefix + mcpTranscriptElisionNote(len(entry.Body)-len(prefix))
	}
	retained := len(entry.Body)
	if len(entry.Payload) > 0 && len(entry.Payload) > remaining-retained {
		entry.Payload = json.RawMessage(fmt.Sprintf(
			`{"witself_elided":true,"omitted_bytes":%d}`, len(entry.Payload)))
	}
	retained += len(entry.Payload)
	if isElidableMCPJSONArray(entry.Artifacts) &&
		len(entry.Artifacts) > remaining-retained {
		entry.Artifacts = json.RawMessage(fmt.Sprintf(
			`[{"witself_elided":true,"omitted_bytes":%d}]`, len(entry.Artifacts)))
	}
	retained += len(entry.Artifacts)
	return retained
}

func mcpTranscriptElisionNote(omitted int) string {
	return fmt.Sprintf(
		"\n[witself:elided omitted_bytes=%d; re-read this entry alone with after_sequence and a smaller limit, or use the witself CLI]",
		omitted)
}

// isElidableMCPJSONArray reports whether replacing artifacts with an elision
// stub would actually shrink it; the default empty array never qualifies.
func isElidableMCPJSONArray(raw json.RawMessage) bool {
	return len(raw) > 64
}

// mcpResultSizeGuard converts any tools/call result whose serialized size
// exceeds maxMCPToolResultBytes into an in-band tool error. This is the
// backstop for tools without their own byte budget: a structured error keeps
// the session alive and tells the caller how to narrow the read, where an
// oversized frame would read as a dead connection.
func mcpResultSizeGuard() mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			res, err := next(ctx, method, req)
			if err != nil || method != "tools/call" {
				return res, err
			}
			result, ok := res.(*mcp.CallToolResult)
			if !ok || result == nil {
				return res, err
			}
			encoded, marshalErr := json.Marshal(result)
			if marshalErr != nil || len(encoded) <= maxMCPToolResultBytes {
				return res, err
			}
			oversized := &mcp.CallToolResult{IsError: true, Content: []mcp.Content{
				&mcp.TextContent{Text: fmt.Sprintf(
					"witself: tool result was %d bytes, over the %d-byte transport budget; retry with a smaller limit or narrower filters, or use the witself CLI for bulk reads",
					len(encoded), maxMCPToolResultBytes)},
			}}
			return oversized, nil
		}
	}
}
