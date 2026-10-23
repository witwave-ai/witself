package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestTranscriptRedactionDocumentation(t *testing.T) {
	for _, tc := range []struct {
		file, heading string
		fragments     []string
	}{
		{
			file: "../../docs/cli-command-surface.md", heading: "## `witself transcript`",
			fragments: []string{
				"an append-only enterprise ledger (the account owner can redact entries), not an addressed A2A mailbox",
				"whose transcript inputs or transcript cursor overlap the selected range",
			},
		},
		{
			file: "../../docs/cli-command-surface.md", heading: "## `witself capabilities`",
			fragments: []string{"The server reports `transcript_redaction` when the redaction route is wired."},
		},
		{
			file: "../../docs/decisions/0005-transcript-entry-redaction.md", heading: "### Owner authority and curation",
			fragments: []string{"whose transcript inputs or transcript cursor overlap the selected range"},
		},
		{
			file: "../../docs/mcp-tools.md", heading: "### `witself.fact.propose_from_transcript`",
			fragments: []string{
				"No MCP tool redacts transcripts; redaction requires the account owner's operator token",
				"(for example [`witself transcript redact`](cli-command-surface.md#witself-transcript-redact))",
			},
		},
	} {
		t.Run(tc.file+"/"+tc.heading, func(t *testing.T) {
			raw, err := os.ReadFile(tc.file)
			if err != nil {
				t.Fatal(err)
			}
			section := dsrRunbookSection(t, tc.file, string(raw), tc.heading)
			normalized := strings.Join(strings.Fields(section), " ")
			for _, fragment := range tc.fragments {
				if !strings.Contains(normalized, fragment) {
					t.Errorf("%s: redaction contract is missing %q", tc.file, fragment)
				}
			}
		})
	}
}

func TestTranscriptRedactInOverviewHelp(t *testing.T) {
	var stdout bytes.Buffer
	usage(&stdout)
	if !strings.Contains(stdout.String(), "witself transcript create|append|list|show|tail|redact") {
		t.Error("top-level help omits transcript redact")
	}
	_, stderr, code := captureIntegrationsCLI(t, func() int { return transcriptCmd(nil) })
	if code != 2 || !strings.Contains(stderr, "witself transcript create|append|list|show|tail|redact") {
		t.Error("transcript overview help omits redact")
	}
}
