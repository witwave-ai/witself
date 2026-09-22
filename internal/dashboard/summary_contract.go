package dashboard

import "time"

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
	Status string `json:"status"` // available, unavailable, disabled
	Count  *int64 `json:"count"`
	Label  string `json:"label"`
	Exact  bool   `json:"exact"`
}

// SummaryActivity contains recorded quantities in the category's fixed unit.
type SummaryActivity struct {
	Status    string  `json:"status"` // available, unavailable, disabled
	Bins      []int64 `json:"bins"`   // exactly 24 UTC hourly quantities when available
	Total     *int64  `json:"total"`
	Unit      string  `json:"unit"`
	Dimension string  `json:"dimension"`
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
