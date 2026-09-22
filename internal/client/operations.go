package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/activity"
)

// ActivityQuery selects a bounded UTC hourly reporting window.
type ActivityQuery struct {
	Since, Until time.Time
	Bucket       string
}

// ActivityReport contains recorded quantities and an immutable tracking boundary.
type ActivityReport struct {
	Schema        string       `json:"schema"`
	Catalog       string       `json:"catalog"`
	AccountID     string       `json:"account_id"`
	RealmID       string       `json:"realm_id"`
	AgentID       string       `json:"agent_id"`
	Since         time.Time    `json:"since"`
	Until         time.Time    `json:"until"`
	Bucket        string       `json:"bucket"`
	TrackingSince *time.Time   `json:"tracking_since"`
	Points        []UsagePoint `json:"points"`
	Totals        []UsageTotal `json:"totals"`
	Truncated     bool         `json:"truncated"`
}
type activityResponse struct {
	Activity json.RawMessage `json:"activity"`
}

func (*activityResponse) maxResponseBytes() int64 { return 2 << 20 }

// GetActivity is a passive, bounded, agent-scoped read. ErrNotFound remains
// distinguishable for older servers; malformed data never becomes zero usage.
func GetActivity(ctx context.Context, endpoint, token string, q ActivityQuery) (ActivityReport, error) {
	invalid := errors.New("invalid activity report")
	if q.Bucket != "" && q.Bucket != "hour" {
		return ActivityReport{}, errors.New("invalid activity query")
	}
	params := url.Values{"group_by": {"hour"}}
	if !q.Since.IsZero() {
		params.Set("since", q.Since.UTC().Format(time.RFC3339))
	}
	if !q.Until.IsZero() {
		params.Set("until", q.Until.UTC().Format(time.RFC3339))
	}
	var out activityResponse
	if err := doJSON(activity.WithObservation(ctx), http.MethodGet, strings.TrimRight(endpoint, "/")+"/v1/activity?"+params.Encode(), token, nil, &out); err != nil {
		return ActivityReport{}, err
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(out.Activity, &fields) != nil {
		return ActivityReport{}, invalid
	}
	for _, key := range []string{"schema", "catalog", "account_id", "realm_id", "agent_id", "since", "until", "bucket", "tracking_since", "points", "totals", "truncated"} {
		value, ok := fields[key]
		if !ok || key != "tracking_since" && bytes.Equal(value, []byte("null")) {
			return ActivityReport{}, invalid
		}
	}
	var r ActivityReport
	decoder := json.NewDecoder(bytes.NewReader(out.Activity))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&r) != nil || !validActivityReport(r) {
		return ActivityReport{}, invalid
	}
	if !q.Since.IsZero() && !r.Since.Equal(q.Since.UTC().Truncate(time.Hour)) || !q.Until.IsZero() && !r.Until.Equal(q.Until.UTC().Truncate(time.Second)) {
		return ActivityReport{}, invalid
	}
	return r, nil
}
func validActivityReport(r ActivityReport) bool {
	if r.Schema != activity.Schema || r.Catalog != activity.CatalogVersion || r.Bucket != "hour" || r.Truncated || r.Points == nil || r.Totals == nil || len(r.Points) > 10000 || len(r.Totals) > 9 {
		return false
	}
	for _, id := range []string{r.AccountID, r.RealmID, r.AgentID} {
		if len(id) == 0 || len(id) > 256 {
			return false
		}
	}
	if r.Since.IsZero() || !r.Since.Equal(r.Since.UTC().Truncate(time.Hour)) || !r.Since.Before(r.Until) || r.Until.Sub(r.Since) > 31*24*time.Hour {
		return false
	}
	if r.TrackingSince == nil {
		return len(r.Points) == 0 && len(r.Totals) == 0
	}
	if r.TrackingSince.IsZero() {
		return false
	}
	sums := map[string]UsageTotal{}
	seen := map[string]bool{}
	for _, p := range r.Points {
		if !validActivityQuantity(p.Dimension, p.Unit, p.Quantity, p.EventCount) || !p.BucketStart.Equal(p.BucketStart.UTC().Truncate(time.Hour)) || p.BucketStart.Before(r.Since) || !p.BucketStart.Before(r.Until) || p.BucketStart.Before(r.TrackingSince.UTC().Truncate(time.Hour)) {
			return false
		}
		key := p.Dimension + ":" + p.BucketStart.UTC().Format(time.RFC3339)
		if seen[key] {
			return false
		}
		seen[key] = true
		sum := sums[p.Dimension]
		if sum.Quantity > math.MaxInt64-p.Quantity || sum.EventCount > math.MaxInt64-p.EventCount {
			return false
		}
		sum.Dimension = p.Dimension
		sum.Unit = p.Unit
		sum.Quantity += p.Quantity
		sum.EventCount += p.EventCount
		sums[p.Dimension] = sum
	}
	if len(sums) != len(r.Totals) {
		return false
	}
	for _, t := range r.Totals {
		if !validActivityQuantity(t.Dimension, t.Unit, t.Quantity, t.EventCount) || sums[t.Dimension] != t {
			return false
		}
		delete(sums, t.Dimension)
	}
	return true
}
func validActivityQuantity(dimension, unit string, quantity, events int64) bool {
	want := activity.Unit(dimension)
	if want == "" || want == "activation" || unit != want || quantity <= 0 || events <= 0 {
		return false
	}
	if unit == "operation" || unit == "change" {
		return quantity == events
	}
	return quantity >= events
}
