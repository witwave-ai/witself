package dashboard

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Summary is a content-free, passive projection shared by both agent consoles.
// Counts and hourly quantities have different meanings and must not be summed.
type Summary struct {
	Schema              string              `json:"schema"`
	GeneratedAt         time.Time           `json:"generated_at"`
	RefreshAfterSeconds int                 `json:"refresh_after_seconds"`
	Window              SummaryWindow       `json:"window"`
	Categories          []SummaryCategory   `json:"categories"`
	Recent              []SummaryUpdate     `json:"recent"`
	Checkpoints         []SummaryCheckpoint `json:"checkpoints"`
}

// SummaryWindow identifies the 24 UTC buckets, including the partial current hour.
type SummaryWindow struct {
	Since                time.Time `json:"since"`
	Until                time.Time `json:"until"`
	Bucket               string    `json:"bucket"`
	Timezone             string    `json:"timezone"`
	PartialCurrentBucket bool      `json:"partial_current_bucket"`
}

// SummaryCategory keeps inventory separate from one named activity measure.
type SummaryCategory struct {
	Key       string           `json:"key"`
	Label     string           `json:"label"`
	Code      string           `json:"code"`
	Inventory SummaryInventory `json:"inventory"`
	Activity  SummaryActivity  `json:"activity"`
}

// SummaryInventory distinguishes an exact total from a bounded record page.
type SummaryInventory struct {
	Status string `json:"status"` // available, unavailable, disabled; Operations: not_applicable
	Count  *int64 `json:"count"`
	Label  string `json:"label"`
	Exact  bool   `json:"exact"`
}

// SummaryActivity contains recorded quantities in the category's fixed unit.
type SummaryActivity struct {
	Status    string           `json:"status"` // available, unavailable, disabled, not_tracked, server_update_needed
	Bins      []*int64         `json:"bins"`   // 24 UTC quantities; null before coverage
	Total     *int64           `json:"total"`
	Unit      string           `json:"unit"`
	Dimension string           `json:"dimension"`
	Coverage  *SummaryCoverage `json:"coverage,omitempty"`
	Breakdown []SummaryMeasure `json:"breakdown,omitempty"`
}

// SummaryUpdate is a timestamp observation, without a record identifier or content.
type SummaryUpdate struct {
	Key    string    `json:"key"`
	At     time.Time `json:"at"`
	Action string    `json:"action"` // fixed server-owned verb, never source content
}

// SummaryCheckpoint reports pending work without claiming or processing it.
type SummaryCheckpoint struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Status string `json:"status"` // pending, clear, disabled, unavailable
}

// SummaryCoverage marks recorded coverage, not completeness of client instrumentation.
type SummaryCoverage struct {
	TrackingSince      *time.Time `json:"tracking_since"`
	PartialFirstBucket bool       `json:"partial_first_bucket"`
}

// SummaryMeasure is a fixed dimension quantity; records are never added to operations.
type SummaryMeasure struct {
	Dimension string `json:"dimension"`
	Unit      string `json:"unit"`
	Total     *int64 `json:"total"`
}

// UnmarshalJSON rejects missing coverage fields instead of treating them as false/null.
func (c *SummaryCoverage) UnmarshalJSON(raw []byte) error {
	f := summaryObject(raw)
	partial, ok := summaryBool(f["partial_first_bucket"])
	if !ok || f["tracking_since"] == nil {
		return fmt.Errorf("invalid summary coverage")
	}
	var at *time.Time
	if err := json.Unmarshal(f["tracking_since"], &at); err != nil {
		return fmt.Errorf("invalid summary coverage")
	}
	if at != nil {
		var value string
		if json.Unmarshal(f["tracking_since"], &value) != nil || !strings.HasSuffix(value, "Z") {
			return fmt.Errorf("invalid summary coverage")
		}
	}
	c.TrackingSince, c.PartialFirstBucket = at, partial
	return nil
}
