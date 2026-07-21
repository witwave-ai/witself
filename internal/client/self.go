package client

import (
	"context"
	"net/http"
	neturl "net/url"
	"strconv"
	"strings"
	"time"
)

// SelfIdentity is the token-derived account, realm, and agent identity.
type SelfIdentity struct {
	AccountID string `json:"account_id"`
	AgentID   string `json:"agent_id"`
	AgentName string `json:"agent_name"`
	RealmID   string `json:"realm_id"`
	RealmName string `json:"realm_name"`
}

// SelfFact is the bounded fact shape carried by a self digest. Facts are not
// implemented in the first identity-only slice, but the stable response shape
// allows them to land without changing the CLI contract.
type SelfFact struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Value     any    `json:"value,omitempty"`
	Primary   bool   `json:"primary"`
	Sensitive bool   `json:"sensitive,omitempty"`
	Redacted  bool   `json:"redacted,omitempty"`
	Source    string `json:"source,omitempty"`
}

// SelfMemory is the bounded salient-memory shape carried by a self digest.
type SelfMemory struct {
	ID              string   `json:"id"`
	Snippet         string   `json:"snippet"`
	ContentEncoding string   `json:"content_encoding,omitempty"`
	Kind            string   `json:"kind"`
	Tags            []string `json:"tags,omitempty"`
	Salience        float64  `json:"salience"`
	Sensitive       bool     `json:"sensitive,omitempty"`
	Redacted        bool     `json:"redacted,omitempty"`
	Source          string   `json:"source,omitempty"`
}

// SelfIndex summarizes discoverable open-plane identity state.
type SelfIndex struct {
	Kinds  []string       `json:"kinds"`
	Tags   []string       `json:"tags"`
	Counts map[string]int `json:"counts"`
}

// SelfMemoryCheckpoint is value-free lifecycle metadata for pending
// client-side narrative-memory curation. Its presence never includes memory,
// transcript, or fact content; the active client must use the fenced curation
// tools to inspect any authorized inputs.
type SelfMemoryCheckpoint struct {
	Pending           bool       `json:"pending"`
	Unavailable       bool       `json:"unavailable,omitempty"`
	RequestID         string     `json:"request_id"`
	RequestGeneration int64      `json:"request_generation"`
	DueAt             *time.Time `json:"due_at,omitempty"`
	RunID             string     `json:"run_id,omitempty"`
	RunState          string     `json:"run_state,omitempty"`
	FencingGeneration int64      `json:"fencing_generation,omitempty"`
	LeaseExpiresAt    *time.Time `json:"lease_expires_at,omitempty"`
}

// SelfMessageCheckpoint is content-free, authenticated discovery state for
// durable messaging work. It is not a claim fence, availability signal, or
// authorization grant.
type SelfMessageCheckpoint struct {
	Pending                     bool `json:"pending"`
	Unavailable                 bool `json:"unavailable,omitempty"`
	MailboxPending              bool `json:"mailbox_pending,omitempty"`
	CandidateOfferPending       bool `json:"candidate_offer_pending,omitempty"`
	CoordinatorSelectionPending bool `json:"coordinator_selection_pending,omitempty"`
	CandidateAssignmentPending  bool `json:"candidate_assignment_pending,omitempty"`
}

// SelfEmailCheckpoint is value-free, authenticated discovery state for the
// receive-only agent-email mailbox. It carries no sender, subject, body,
// address, message identifier, or processing fence.
type SelfEmailCheckpoint struct {
	Pending        bool `json:"pending"`
	Unavailable    bool `json:"unavailable,omitempty"`
	MailboxPending bool `json:"mailbox_pending,omitempty"`
}

// SelfAvatarCheckpoint is value-free, authenticated lifecycle state for an
// agent avatar that needs attention from an active AI client. It never carries
// SVG, prompt, description, or visual-spec content.
type SelfAvatarCheckpoint struct {
	Pending           bool       `json:"pending"`
	Unavailable       bool       `json:"unavailable,omitempty"`
	Status            string     `json:"status,omitempty"`
	Reason            string     `json:"reason,omitempty"`
	ProfileRevision   int64      `json:"profile_revision,omitempty"`
	LineageGeneration int64      `json:"lineage_generation,omitempty"`
	StylePackID       string     `json:"style_pack_id,omitempty"`
	StylePackVersion  int64      `json:"style_pack_version,omitempty"`
	ActiveVersion     int64      `json:"active_version,omitempty"`
	ProposedVersion   int64      `json:"proposed_version,omitempty"`
	AttemptCount      int        `json:"attempt_count,omitempty"`
	RetryAfter        *time.Time `json:"retry_after,omitempty"`
}

// SelfDigest is the bounded response from GET /v1/self.
type SelfDigest struct {
	SchemaVersion     string                 `json:"schema_version"`
	Identity          SelfIdentity           `json:"identity"`
	PrimaryFacts      []SelfFact             `json:"primary_facts"`
	SalientMemories   []SelfMemory           `json:"salient_memories"`
	MemoryCheckpoint  *SelfMemoryCheckpoint  `json:"memory_checkpoint,omitempty"`
	MessageCheckpoint *SelfMessageCheckpoint `json:"message_checkpoint,omitempty"`
	EmailCheckpoint   *SelfEmailCheckpoint   `json:"email_checkpoint,omitempty"`
	AvatarCheckpoint  *SelfAvatarCheckpoint  `json:"avatar_checkpoint,omitempty"`
	Index             SelfIndex              `json:"index"`
	Elided            bool                   `json:"elided"`
}

// SelfOptions controls bounded digest sections. The identity block is always
// returned and always comes from the authenticated agent token.
type SelfOptions struct {
	IncludeFacts   bool
	IncludeSalient bool
	// IncludeCounts requests inventory totals in the self index. Identity and
	// checkpoint-only callers should leave this false to avoid count queries.
	IncludeCounts bool
	// IncludeCheckpoint requests value-free narrative-curation lifecycle state.
	// Identity-only callers should leave this false to avoid queue queries.
	IncludeCheckpoint bool
	// IncludeMessageCheckpoint requests a content-free projection of durable
	// mailbox and open-request work. Identity-only callers should leave this
	// false to avoid messaging queries.
	IncludeMessageCheckpoint bool
	// IncludeEmailCheckpoint requests a content-free projection of pending
	// receive-only email. Identity-only callers should leave this false.
	IncludeEmailCheckpoint bool
	// IncludeAvatarCheckpoint requests content-free avatar lifecycle state.
	// Identity-only callers should leave this false to avoid avatar queries.
	IncludeAvatarCheckpoint bool
	// IncludeSensitive intentionally includes authorized private fact and memory
	// values. Sealed secrets are a separate service and are never in this digest.
	IncludeSensitive bool
	// Observational requests fact hydration without retrieval-usage writes.
	// It is used by strictly read-only MCP tools; ordinary clients leave it false.
	Observational   bool
	SalientLimit    int
	MaximumByteSize int
}

// PeerAgent is one other principal in the authenticated agent's realm.
// Activity fields are observations only; they do not imply that the peer is
// online, available, or accepting work.
type PeerAgent struct {
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	LastActivityAt *time.Time `json:"last_activity_at,omitempty"`
	LastRuntime    string     `json:"last_runtime,omitempty"`
	LastLocation   string     `json:"last_location,omitempty"`
	LastEvent      string     `json:"last_event,omitempty"`
}

// SelfPeers is the realm-safe peer inventory returned to an authenticated
// agent. The server derives both the realm and the excluded self agent from the
// token; callers cannot provide either as targeting input.
type SelfPeers struct {
	SchemaVersion string      `json:"schema_version"`
	Peers         []PeerAgent `json:"peers"`
}

// GetSelf fetches the token-bound agent's self digest.
func GetSelf(ctx context.Context, endpoint, token string, opts SelfOptions) (SelfDigest, error) {
	params := neturl.Values{}
	params.Set("include_facts", strconv.FormatBool(opts.IncludeFacts))
	params.Set("include_salient", strconv.FormatBool(opts.IncludeSalient))
	params.Set("include_counts", strconv.FormatBool(opts.IncludeCounts))
	params.Set("include_checkpoint", strconv.FormatBool(opts.IncludeCheckpoint))
	params.Set("include_message_checkpoint", strconv.FormatBool(opts.IncludeMessageCheckpoint))
	params.Set("include_email_checkpoint", strconv.FormatBool(opts.IncludeEmailCheckpoint))
	params.Set("include_avatar_checkpoint", strconv.FormatBool(opts.IncludeAvatarCheckpoint))
	params.Set("include_sensitive", strconv.FormatBool(opts.IncludeSensitive))
	if opts.Observational {
		params.Set("observational", "true")
	}
	if opts.SalientLimit > 0 {
		params.Set("salient_limit", strconv.Itoa(opts.SalientLimit))
	}
	if opts.MaximumByteSize > 0 {
		params.Set("max_bytes", strconv.Itoa(opts.MaximumByteSize))
	}
	url := strings.TrimRight(endpoint, "/") + "/v1/self?" + params.Encode()
	var out SelfDigest
	if err := doJSON(ctx, http.MethodGet, url, token, nil, &out); err != nil {
		return SelfDigest{}, err
	}
	return out, nil
}

// GetSelfPeers lists every other agent in the token-derived realm together
// with its most recently observed activity, when any has been recorded.
func GetSelfPeers(ctx context.Context, endpoint, token string) (SelfPeers, error) {
	url := strings.TrimRight(endpoint, "/") + "/v1/self/peers"
	var out SelfPeers
	if err := doJSON(ctx, http.MethodGet, url, token, nil, &out); err != nil {
		return SelfPeers{}, err
	}
	if out.Peers == nil {
		out.Peers = []PeerAgent{}
	}
	return out, nil
}
