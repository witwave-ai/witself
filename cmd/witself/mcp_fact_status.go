package main

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/witwave-ai/witself/internal/client"
)

type witselfMCPFactStatusBackend interface {
	FactLimitStatus(context.Context) (client.FactLimitStatus, error)
}

func (b configuredMCPBackend) FactLimitStatus(ctx context.Context) (client.FactLimitStatus, error) {
	conn, err := b.connect(ctx)
	if err != nil {
		return client.FactLimitStatus{}, err
	}
	status, err := client.GetFactLimitStatus(ctx, conn.Endpoint, conn.Token)
	if err != nil {
		return client.FactLimitStatus{}, err
	}
	return *status, nil
}

type mcpFactStatusOutput struct {
	FactCapacity client.FactLimitStatus `json:"fact_capacity"`
}

func registerFactStatusMCPTool(server *mcp.Server, runtimeName string, backend witselfMCPBackend) {
	mcp.AddTool(server, &mcp.Tool{
		Name: mcpToolName(runtimeName, "witself.fact.status"),
		Description: "Read this agent's authenticated, value-free current-fact capacity. " +
			"At or above the cap, reads, history, deletion, and updates to an existing current fact remain available; " +
			"only writes that would create or recreate another current fact are refused.",
		Annotations: mcpReadOnlyClosedWorldAnnotations(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ mcpNoInput) (*mcp.CallToolResult, mcpFactStatusOutput, error) {
		statusBackend, ok := backend.(witselfMCPFactStatusBackend)
		if !ok {
			return nil, mcpFactStatusOutput{}, fmt.Errorf("fact capacity status is unavailable in this backend")
		}
		status, err := statusBackend.FactLimitStatus(ctx)
		return nil, mcpFactStatusOutput{FactCapacity: status}, err
	})
}
