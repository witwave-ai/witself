package client

import (
	"context"
	"encoding/json"
	"net/http"
	neturl "net/url"
	"strings"
	"time"
)

// Transcript is one append-only visible interaction thread.
type Transcript struct {
	ID           string          `json:"id"`
	AccountID    string          `json:"account_id"`
	RealmID      string          `json:"realm_id"`
	OwnerAgentID string          `json:"owner_agent_id"`
	ExternalID   string          `json:"external_id,omitempty"`
	Title        string          `json:"title,omitempty"`
	Metadata     json.RawMessage `json:"metadata"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
}

// TranscriptEntry is one immutable visible turn or explicit system/tool trace.
type TranscriptEntry struct {
	ID                string          `json:"id"`
	AccountID         string          `json:"account_id"`
	TranscriptID      string          `json:"transcript_id"`
	RealmID           string          `json:"realm_id"`
	RecordedByAgentID string          `json:"recorded_by_agent_id"`
	Sequence          int64           `json:"sequence"`
	ExternalID        string          `json:"external_id,omitempty"`
	Role              string          `json:"role"`
	Body              string          `json:"body,omitempty"`
	Payload           json.RawMessage `json:"payload,omitempty"`
	Model             string          `json:"model,omitempty"`
	ReplyToEntryID    string          `json:"reply_to_entry_id,omitempty"`
	Artifacts         json.RawMessage `json:"artifacts"`
	CreatedAt         time.Time       `json:"created_at"`
}

// CreateTranscriptInput carries optional caller metadata for a new transcript.
type CreateTranscriptInput struct {
	ExternalID string          `json:"external_id,omitempty"`
	Title      string          `json:"title,omitempty"`
	Metadata   json.RawMessage `json:"metadata,omitempty"`
}

// AppendTranscriptEntryInput carries one visible turn to append.
type AppendTranscriptEntryInput struct {
	ExternalID     string          `json:"external_id,omitempty"`
	Role           string          `json:"role"`
	Body           string          `json:"body,omitempty"`
	Payload        json.RawMessage `json:"payload,omitempty"`
	Model          string          `json:"model,omitempty"`
	ReplyToEntryID string          `json:"reply_to_entry_id,omitempty"`
}

// TranscriptDetail is a transcript plus its ordered entries.
type TranscriptDetail struct {
	Transcript Transcript        `json:"transcript"`
	Entries    []TranscriptEntry `json:"entries"`
}

// CreateTranscript creates a transcript using an agent bearer token.
func CreateTranscript(ctx context.Context, endpoint, token string, in CreateTranscriptInput) (Transcript, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return Transcript{}, err
	}
	var out struct {
		Transcript Transcript `json:"transcript"`
	}
	if err := doJSON(ctx, http.MethodPost, transcriptsURL(endpoint), token, body, &out); err != nil {
		return Transcript{}, err
	}
	return out.Transcript, nil
}

// AppendTranscriptEntry records one immutable visible turn.
func AppendTranscriptEntry(ctx context.Context, endpoint, token, transcriptID string, in AppendTranscriptEntryInput) (TranscriptEntry, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return TranscriptEntry{}, err
	}
	var out struct {
		Entry TranscriptEntry `json:"entry"`
	}
	url := transcriptsURL(endpoint) + "/" + neturl.PathEscape(transcriptID) + "/entries"
	if err := doJSON(ctx, http.MethodPost, url, token, body, &out); err != nil {
		return TranscriptEntry{}, err
	}
	return out.Entry, nil
}

// ListTranscripts lists transcripts visible to an agent or operator token.
func ListTranscripts(ctx context.Context, endpoint, token string) ([]Transcript, error) {
	var out struct {
		Transcripts []Transcript `json:"transcripts"`
	}
	if err := doJSON(ctx, http.MethodGet, transcriptsURL(endpoint), token, nil, &out); err != nil {
		return nil, err
	}
	return out.Transcripts, nil
}

// GetTranscript returns one transcript and its entries.
func GetTranscript(ctx context.Context, endpoint, token, transcriptID string) (TranscriptDetail, error) {
	var out TranscriptDetail
	url := transcriptsURL(endpoint) + "/" + neturl.PathEscape(transcriptID)
	if err := doJSON(ctx, http.MethodGet, url, token, nil, &out); err != nil {
		return TranscriptDetail{}, err
	}
	return out, nil
}

func transcriptsURL(endpoint string) string {
	return strings.TrimRight(endpoint, "/") + "/v1/transcripts"
}
