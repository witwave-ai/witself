package main

import (
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

// Transcript redaction is an owner-operator CLI operation, never an MCP tool.
func TestMCPDoesNotExposeTranscriptRedaction(t *testing.T) {
	for _, row := range mcpToolMatrixRows() {
		for _, route := range row.Routes {
			if route == "POST /v1/transcripts/{transcript}/entries:redact" {
				t.Errorf("MCP tool %q routes to owner-only transcript redaction", row.Tool)
			}
		}
	}
	for _, tc := range []struct{ name, runtime string }{
		{name: "dotted"},
		{name: "portable", runtime: transcriptcapture.RuntimeGrokBuild},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			server := newWitselfMCPServerForRuntime(&fakeMCPBackend{}, tc.runtime)
			clientTransport, serverTransport := mcp.NewInMemoryTransports()
			serverSession, err := server.Connect(ctx, serverTransport, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = serverSession.Close() }()
			clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = clientSession.Close() }()

			for name := range mcpToolMatrixToolNames(t, clientSession) {
				if strings.Contains(name, "transcript.redact") || strings.Contains(name, "transcript_redact") {
					t.Errorf("MCP advertises owner-only transcript redaction tool %q", name)
				}
			}
		})
	}
}
