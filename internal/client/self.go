package client

import (
	"context"
	"net/http"
	neturl "net/url"
	"strconv"
	"strings"
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
	ID       string  `json:"id"`
	Snippet  string  `json:"snippet"`
	Kind     string  `json:"kind"`
	Salience float64 `json:"salience"`
	Source   string  `json:"source,omitempty"`
}

// SelfIndex summarizes discoverable open-plane identity state.
type SelfIndex struct {
	Kinds  []string       `json:"kinds"`
	Tags   []string       `json:"tags"`
	Counts map[string]int `json:"counts"`
}

// SelfDigest is the bounded response from GET /v1/self.
type SelfDigest struct {
	SchemaVersion   string       `json:"schema_version"`
	Identity        SelfIdentity `json:"identity"`
	PrimaryFacts    []SelfFact   `json:"primary_facts"`
	SalientMemories []SelfMemory `json:"salient_memories"`
	Index           SelfIndex    `json:"index"`
	Elided          bool         `json:"elided"`
}

// SelfOptions controls bounded digest sections. The identity block is always
// returned and always comes from the authenticated agent token.
type SelfOptions struct {
	IncludeFacts    bool
	IncludeSalient  bool
	SalientLimit    int
	MaximumByteSize int
}

// GetSelf fetches the token-bound agent's self digest.
func GetSelf(ctx context.Context, endpoint, token string, opts SelfOptions) (SelfDigest, error) {
	params := neturl.Values{}
	params.Set("include_facts", strconv.FormatBool(opts.IncludeFacts))
	params.Set("include_salient", strconv.FormatBool(opts.IncludeSalient))
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
