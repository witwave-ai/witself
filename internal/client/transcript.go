package client

import (
	"context"
	"encoding/json"
	"net/http"
	neturl "net/url"
	"strconv"
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
	ExternalID        string          `json:"external_id,omitempty"`
	Role              string          `json:"role"`
	Body              string          `json:"body,omitempty"`
	Payload           json.RawMessage `json:"payload,omitempty"`
	Model             string          `json:"model,omitempty"`
	ReplyToEntryID    string          `json:"reply_to_entry_id,omitempty"`
	ReplyToExternalID string          `json:"reply_to_external_id,omitempty"`
}

// TranscriptDetail is a transcript plus its ordered entries.
type TranscriptDetail struct {
	Transcript        Transcript        `json:"transcript"`
	Entries           []TranscriptEntry `json:"entries"`
	NextAfterSequence int64             `json:"next_after_sequence,omitempty"`
}

// TranscriptPageOptions selects one bounded transcript page.
type TranscriptPageOptions struct {
	AfterSequence int64
	Limit         int
	Tail          bool
	Observational bool
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

// AppendTranscriptEntries records a bounded batch in caller order.
func AppendTranscriptEntries(ctx context.Context, endpoint, token, transcriptID string, inputs []AppendTranscriptEntryInput) ([]TranscriptEntry, error) {
	body, err := json.Marshal(map[string]any{"entries": inputs})
	if err != nil {
		return nil, err
	}
	var out struct {
		Entries []TranscriptEntry `json:"entries"`
	}
	url := transcriptsURL(endpoint) + "/" + neturl.PathEscape(transcriptID) + "/entries:batch"
	if err := doJSON(ctx, http.MethodPost, url, token, body, &out); err != nil {
		return nil, err
	}
	return out.Entries, nil
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

// GetTranscript returns one transcript and all entries by following pages.
func GetTranscript(ctx context.Context, endpoint, token, transcriptID string) (TranscriptDetail, error) {
	var out TranscriptDetail
	var after int64
	for {
		page, err := GetTranscriptPage(ctx, endpoint, token, transcriptID, TranscriptPageOptions{AfterSequence: after, Limit: 500})
		if err != nil {
			return TranscriptDetail{}, err
		}
		if out.Transcript.ID == "" {
			out.Transcript = page.Transcript
		}
		out.Entries = append(out.Entries, page.Entries...)
		if page.NextAfterSequence == 0 {
			break
		}
		after = page.NextAfterSequence
	}
	return out, nil
}

// GetTranscriptPage returns a bounded forward page or newest tail.
func GetTranscriptPage(ctx context.Context, endpoint, token, transcriptID string, opts TranscriptPageOptions) (TranscriptDetail, error) {
	params := neturl.Values{}
	if opts.AfterSequence > 0 {
		params.Set("after_sequence", strconv.FormatInt(opts.AfterSequence, 10))
	}
	if opts.Limit > 0 {
		params.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.Tail {
		params.Set("tail", "true")
	}
	if opts.Observational {
		params.Set("observational", "true")
	}
	url := transcriptsURL(endpoint) + "/" + neturl.PathEscape(transcriptID)
	if encoded := params.Encode(); encoded != "" {
		url += "?" + encoded
	}
	var out TranscriptDetail
	if err := doJSON(ctx, http.MethodGet, url, token, nil, &out); err != nil {
		return TranscriptDetail{}, err
	}
	return out, nil
}

func transcriptsURL(endpoint string) string {
	return strings.TrimRight(endpoint, "/") + "/v1/transcripts"
}
