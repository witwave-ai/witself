package client

import (
	"context"
	"net/http"
	neturl "net/url"
	"strings"
	"time"
)

// UsageQuery selects the authenticated agent's usage rollups.
type UsageQuery struct {
	Since      time.Time
	Until      time.Time
	Bucket     string
	Dimensions []string
}

// UsagePoint is one dimension total in a UTC time bucket.
type UsagePoint struct {
	Dimension   string    `json:"dimension"`
	Unit        string    `json:"unit"`
	BucketStart time.Time `json:"bucket_start"`
	Quantity    int64     `json:"quantity"`
	EventCount  int64     `json:"event_count"`
}

// UsageTotal is a dimension total across the report window.
type UsageTotal struct {
	Dimension  string `json:"dimension"`
	Unit       string `json:"unit"`
	Quantity   int64  `json:"quantity"`
	EventCount int64  `json:"event_count"`
}

// UsageReport is the token-derived per-agent usage view.
type UsageReport struct {
	AccountID string       `json:"account_id"`
	RealmID   string       `json:"realm_id"`
	RealmName string       `json:"realm_name,omitempty"`
	AgentID   string       `json:"agent_id"`
	AgentName string       `json:"agent_name,omitempty"`
	Since     time.Time    `json:"since"`
	Until     time.Time    `json:"until"`
	Bucket    string       `json:"bucket"`
	Points    []UsagePoint `json:"points"`
	Totals    []UsageTotal `json:"totals"`
}

// GetUsage returns hourly or daily rollups for the bearer-token agent.
func GetUsage(ctx context.Context, endpoint, token string, query UsageQuery) (UsageReport, error) {
	params := neturl.Values{}
	if !query.Since.IsZero() {
		params.Set("since", query.Since.UTC().Format(time.RFC3339))
	}
	if !query.Until.IsZero() {
		params.Set("until", query.Until.UTC().Format(time.RFC3339))
	}
	if query.Bucket != "" {
		params.Set("group_by", query.Bucket)
	}
	for _, dimension := range query.Dimensions {
		params.Add("dimension", dimension)
	}
	url := strings.TrimRight(endpoint, "/") + "/v1/usage"
	if encoded := params.Encode(); encoded != "" {
		url += "?" + encoded
	}
	var out struct {
		Usage UsageReport `json:"usage"`
	}
	if err := doJSON(ctx, http.MethodGet, url, token, nil, &out); err != nil {
		return UsageReport{}, err
	}
	return out.Usage, nil
}
