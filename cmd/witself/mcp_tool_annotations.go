package main

import "github.com/modelcontextprotocol/go-sdk/mcp"

// MCP read-only tools use observational transport paths that do not write
// access telemetry, metering, audit records, or resource state. Witself is a
// realm-private closed system, so openWorldHint is false.
func mcpReadOnlyClosedWorldAnnotations() *mcp.ToolAnnotations {
	destructive := false
	openWorld := false
	return &mcp.ToolAnnotations{
		ReadOnlyHint:    true,
		DestructiveHint: &destructive,
		IdempotentHint:  true,
		OpenWorldHint:   &openWorld,
	}
}

// MCP destructive=false is reserved for operations that perform only additive
// updates. Any tool that may update, overwrite, revoke, delete, or otherwise
// mutate existing state uses destructive=true, as do irreversible outbound
// messages and transactions, even when a compensating or versioned workflow
// exists.
func mcpWriteClosedWorldAnnotations(destructive, idempotent bool) *mcp.ToolAnnotations {
	openWorld := false
	return &mcp.ToolAnnotations{
		DestructiveHint: &destructive,
		IdempotentHint:  idempotent,
		OpenWorldHint:   &openWorld,
	}
}
