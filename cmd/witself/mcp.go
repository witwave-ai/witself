package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/transcriptcapture"
	"github.com/witwave-ai/witself/internal/version"
)

const witselfMCPInstructions = "You have a persistent Witself identity and transcript ledger. Call `witself.self.show` at the start of a non-trivial task. Use `witself.transcript.list` to find prior sessions, `witself.transcript.tail` for recent context, and `witself.transcript.get` to page through a full transcript. Transcript tools are read-only and contain runtime-visible interaction data, never hidden model reasoning."

type witselfMCPBackend interface {
	Self(context.Context) (client.SelfDigest, error)
	ListTranscripts(context.Context) ([]client.Transcript, error)
	GetTranscriptPage(context.Context, string, client.TranscriptPageOptions) (client.TranscriptDetail, error)
}

type configuredMCPBackend struct {
	cfg transcriptcapture.Config
}

func (b configuredMCPBackend) connect(ctx context.Context) (agentConnection, error) {
	return connectAgent(ctx, b.cfg.Account, b.cfg.Realm, b.cfg.Agent, b.cfg.Endpoint, b.cfg.TokenFile)
}

func (b configuredMCPBackend) Self(ctx context.Context) (client.SelfDigest, error) {
	conn, err := b.connect(ctx)
	if err != nil {
		return client.SelfDigest{}, err
	}
	return client.GetSelf(ctx, conn.Endpoint, conn.Token, client.SelfOptions{})
}

func (b configuredMCPBackend) ListTranscripts(ctx context.Context) ([]client.Transcript, error) {
	conn, err := b.connect(ctx)
	if err != nil {
		return nil, err
	}
	return client.ListTranscripts(ctx, conn.Endpoint, conn.Token)
}

func (b configuredMCPBackend) GetTranscriptPage(ctx context.Context, transcriptID string, opts client.TranscriptPageOptions) (client.TranscriptDetail, error) {
	conn, err := b.connect(ctx)
	if err != nil {
		return client.TranscriptDetail{}, err
	}
	return client.GetTranscriptPage(ctx, conn.Endpoint, conn.Token, transcriptID, opts)
}

type mcpNoInput struct{}

type mcpTranscriptListInput struct {
	Limit int `json:"limit,omitempty" jsonschema:"maximum number of newest transcripts to return, from 1 to 100"`
}

type mcpTranscriptListOutput struct {
	Transcripts []mcpTranscript `json:"transcripts"`
}

type mcpTranscript struct {
	ID           string    `json:"id"`
	AccountID    string    `json:"account_id"`
	RealmID      string    `json:"realm_id"`
	OwnerAgentID string    `json:"owner_agent_id"`
	ExternalID   string    `json:"external_id,omitempty"`
	Title        string    `json:"title,omitempty"`
	Metadata     any       `json:"metadata"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type mcpTranscriptEntry struct {
	ID                string    `json:"id"`
	AccountID         string    `json:"account_id"`
	TranscriptID      string    `json:"transcript_id"`
	RealmID           string    `json:"realm_id"`
	RecordedByAgentID string    `json:"recorded_by_agent_id"`
	Sequence          int64     `json:"sequence"`
	ExternalID        string    `json:"external_id,omitempty"`
	Role              string    `json:"role"`
	Body              string    `json:"body,omitempty"`
	Payload           any       `json:"payload,omitempty"`
	Model             string    `json:"model,omitempty"`
	ReplyToEntryID    string    `json:"reply_to_entry_id,omitempty"`
	Artifacts         any       `json:"artifacts"`
	CreatedAt         time.Time `json:"created_at"`
}

type mcpTranscriptReadInput struct {
	TranscriptID  string `json:"transcript_id" jsonschema:"Witself transcript id beginning with trn_"`
	AfterSequence int64  `json:"after_sequence,omitempty" jsonschema:"return entries after this sequence number"`
	Limit         int    `json:"limit,omitempty" jsonschema:"entries to return, from 1 to 500"`
}

type mcpTranscriptReadOutput struct {
	Transcript        mcpTranscript        `json:"transcript"`
	Entries           []mcpTranscriptEntry `json:"entries"`
	NextAfterSequence int64                `json:"next_after_sequence,omitempty"`
}

type mcpTranscriptTailInput struct {
	TranscriptID string `json:"transcript_id" jsonschema:"Witself transcript id beginning with trn_"`
	Limit        int    `json:"limit,omitempty" jsonschema:"newest entries to return, from 1 to 500"`
}

func mcpCmd(args []string) int {
	if len(args) == 0 || args[0] != "serve" {
		fmt.Fprintln(os.Stderr, "usage: witself mcp serve --runtime codex|claude-code")
		return 2
	}
	fs := flag.NewFlagSet("mcp serve", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	runtime := fs.String("runtime", "", "installed integration: codex|claude-code")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	cfg, err := transcriptcapture.LoadConfig(*runtime)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself mcp: %v\n", err)
		return 1
	}
	server := newWitselfMCPServer(configuredMCPBackend{cfg: cfg})
	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		fmt.Fprintf(os.Stderr, "witself mcp: %v\n", err)
		return 1
	}
	return 0
}

func newWitselfMCPServer(backend witselfMCPBackend) *mcp.Server {
	server := mcp.NewServer(
		&mcp.Implementation{Name: "witself", Version: version.Version},
		&mcp.ServerOptions{Instructions: witselfMCPInstructions},
	)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "witself.self.show",
		Description: "Return the authenticated Witself agent identity and bounded self digest.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ mcpNoInput) (*mcp.CallToolResult, client.SelfDigest, error) {
		out, err := backend.Self(ctx)
		return nil, out, err
	})
	mcp.AddTool(server, &mcp.Tool{
		Name:        "witself.transcript.list",
		Description: "List this agent's newest captured transcripts.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in mcpTranscriptListInput) (*mcp.CallToolResult, mcpTranscriptListOutput, error) {
		if in.Limit == 0 {
			in.Limit = 20
		}
		if in.Limit < 1 || in.Limit > 100 {
			return nil, mcpTranscriptListOutput{}, fmt.Errorf("limit must be between 1 and 100")
		}
		rows, err := backend.ListTranscripts(ctx)
		if err != nil {
			return nil, mcpTranscriptListOutput{}, err
		}
		if len(rows) > in.Limit {
			rows = rows[:in.Limit]
		}
		if rows == nil {
			rows = []client.Transcript{}
		}
		return nil, mcpTranscriptListOutput{Transcripts: toMCPTranscripts(rows)}, nil
	})
	mcp.AddTool(server, &mcp.Tool{
		Name:        "witself.transcript.get",
		Description: "Read one bounded forward page from a captured transcript.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in mcpTranscriptReadInput) (*mcp.CallToolResult, mcpTranscriptReadOutput, error) {
		if in.TranscriptID == "" {
			return nil, mcpTranscriptReadOutput{}, fmt.Errorf("transcript_id is required")
		}
		if in.Limit == 0 {
			in.Limit = 100
		}
		if in.Limit < 1 || in.Limit > 500 || in.AfterSequence < 0 {
			return nil, mcpTranscriptReadOutput{}, fmt.Errorf("limit must be 1-500 and after_sequence cannot be negative")
		}
		page, err := backend.GetTranscriptPage(ctx, in.TranscriptID, client.TranscriptPageOptions{AfterSequence: in.AfterSequence, Limit: in.Limit})
		if err != nil {
			return nil, mcpTranscriptReadOutput{}, err
		}
		return nil, mcpTranscriptReadOutput{
			Transcript:        toMCPTranscript(page.Transcript),
			Entries:           toMCPTranscriptEntries(page.Entries),
			NextAfterSequence: page.NextAfterSequence,
		}, nil
	})
	mcp.AddTool(server, &mcp.Tool{
		Name:        "witself.transcript.tail",
		Description: "Read the newest entries from a captured transcript, ordered oldest-first.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in mcpTranscriptTailInput) (*mcp.CallToolResult, mcpTranscriptReadOutput, error) {
		if in.TranscriptID == "" {
			return nil, mcpTranscriptReadOutput{}, fmt.Errorf("transcript_id is required")
		}
		if in.Limit == 0 {
			in.Limit = 20
		}
		if in.Limit < 1 || in.Limit > 500 {
			return nil, mcpTranscriptReadOutput{}, fmt.Errorf("limit must be between 1 and 500")
		}
		page, err := backend.GetTranscriptPage(ctx, in.TranscriptID, client.TranscriptPageOptions{Limit: in.Limit, Tail: true})
		if err != nil {
			return nil, mcpTranscriptReadOutput{}, err
		}
		return nil, mcpTranscriptReadOutput{
			Transcript: toMCPTranscript(page.Transcript),
			Entries:    toMCPTranscriptEntries(page.Entries),
		}, nil
	})
	return server
}

func toMCPTranscripts(rows []client.Transcript) []mcpTranscript {
	out := make([]mcpTranscript, len(rows))
	for i, row := range rows {
		out[i] = toMCPTranscript(row)
	}
	return out
}

func toMCPTranscript(row client.Transcript) mcpTranscript {
	return mcpTranscript{
		ID: row.ID, AccountID: row.AccountID, RealmID: row.RealmID,
		OwnerAgentID: row.OwnerAgentID, ExternalID: row.ExternalID, Title: row.Title,
		Metadata: decodeMCPJSON(row.Metadata), CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
}

func toMCPTranscriptEntries(rows []client.TranscriptEntry) []mcpTranscriptEntry {
	out := make([]mcpTranscriptEntry, len(rows))
	for i, row := range rows {
		out[i] = mcpTranscriptEntry{
			ID: row.ID, AccountID: row.AccountID, TranscriptID: row.TranscriptID,
			RealmID: row.RealmID, RecordedByAgentID: row.RecordedByAgentID,
			Sequence: row.Sequence, ExternalID: row.ExternalID, Role: row.Role, Body: row.Body,
			Payload: decodeMCPJSON(row.Payload), Model: row.Model, ReplyToEntryID: row.ReplyToEntryID,
			Artifacts: decodeMCPJSON(row.Artifacts), CreatedAt: row.CreatedAt,
		}
	}
	return out
}

func decodeMCPJSON(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil
	}
	return value
}
