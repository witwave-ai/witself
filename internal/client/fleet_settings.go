package client

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"time"
)

// PlacementRunnerConfig is the scheduled restore/rebalance configuration.
type PlacementRunnerConfig struct {
	Enabled          bool `json:"enabled"`
	RestoreArchives  bool `json:"restore_archives"`
	RestoreBatch     int  `json:"restore_batch"`
	RestoreAnyRegion bool `json:"restore_any_region"`
	Rebalance        bool `json:"rebalance"`
	RebalanceBatch   int  `json:"rebalance_batch"`
}

// PlacementRunnerPatch changes only fields whose pointers are non-nil, preserving
// the stored values of omitted settings when the control plane merges the patch.
type PlacementRunnerPatch struct {
	Enabled          *bool `json:"enabled,omitempty"`
	RestoreArchives  *bool `json:"restore_archives,omitempty"`
	RestoreBatch     *int  `json:"restore_batch,omitempty"`
	RestoreAnyRegion *bool `json:"restore_any_region,omitempty"`
	Rebalance        *bool `json:"rebalance,omitempty"`
	RebalanceBatch   *int  `json:"rebalance_batch,omitempty"`
}

// ReaperConfig controls closure of pending accounts that never activated.
// TTLMinutes is a finite number of minutes, including fractions, required when
// enabled and omitted when disabled.
type ReaperConfig struct {
	Enabled    bool    `json:"enabled"`
	TTLMinutes float64 `json:"ttl_minutes,omitempty"`
}

// PlacementConfig selects weighted placement or a soft pin to PinnedCell.
type PlacementConfig struct {
	Strategy   string `json:"strategy"`
	PinnedCell string `json:"pinned_cell,omitempty"`
}

// PlacementRunnerResult preserves authoritative configuration and raw step
// results, including partial successes when a runner step fails.
type PlacementRunnerResult struct {
	PlacementRunner PlacementRunnerConfig     `json:"placement_runner"`
	Restore         json.RawMessage           `json:"restore,omitempty"`
	Rebalance       json.RawMessage           `json:"rebalance,omitempty"`
	RestoreError    *PlacementRunnerStepError `json:"restore_error,omitempty"`
	RebalanceError  *PlacementRunnerStepError `json:"rebalance_error,omitempty"`
}

// PlacementRunnerStepError retains a failed step's HTTP status and response body.
type PlacementRunnerStepError struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

// GetPlacementRunner reads the scheduled restore/rebalance configuration.
func GetPlacementRunner(ctx context.Context, endpoint, fleetToken string) (PlacementRunnerConfig, error) {
	out, err := placementRunnerRequest(ctx, endpoint, fleetToken, nil, false)
	return out.PlacementRunner, err
}

// SetPlacementRunner changes only explicitly supplied fields. The control plane
// merges the patch into its stored configuration before normalizing it.
func SetPlacementRunner(ctx context.Context, endpoint, fleetToken string, patch PlacementRunnerPatch) (PlacementRunnerConfig, error) {
	if patch == (PlacementRunnerPatch{}) {
		return PlacementRunnerConfig{}, fmt.Errorf("placement runner requires at least one setting")
	}
	out, err := placementRunnerRequest(ctx, endpoint, fleetToken, &patch, false)
	return out.PlacementRunner, err
}

// RunPlacementRunner synchronously runs the stored configuration with optional
// overrides. The control plane forces enabled=true for this pass without
// changing the stored configuration. Step failures remain in the result.
func RunPlacementRunner(ctx context.Context, endpoint, fleetToken string, patch PlacementRunnerPatch) (PlacementRunnerResult, error) {
	return placementRunnerRequest(ctx, endpoint, fleetToken, &patch, true)
}

func placementRunnerRequest(ctx context.Context, endpoint, fleetToken string, patch *PlacementRunnerPatch, run bool) (PlacementRunnerResult, error) {
	if patch != nil {
		if patch.RestoreBatch != nil && (*patch.RestoreBatch < 1 || *patch.RestoreBatch > 10) {
			return PlacementRunnerResult{}, fmt.Errorf("restore_batch must be between 1 and 10")
		}
		if patch.RebalanceBatch != nil && (*patch.RebalanceBatch < 1 || *patch.RebalanceBatch > 5) {
			return PlacementRunnerResult{}, fmt.Errorf("rebalance_batch must be between 1 and 5")
		}
	}
	route := "/v1/placement-runner"
	var timeout time.Duration
	if run {
		route = "/v1/placement:run"
		timeout = 10 * time.Minute
	}
	var body any
	if patch != nil {
		body = patch
	}
	var out struct {
		SchemaVersion   string                    `json:"schema_version"`
		PlacementRunner *PlacementRunnerPatch     `json:"placement_runner"`
		Restore         json.RawMessage           `json:"restore,omitempty"`
		Rebalance       json.RawMessage           `json:"rebalance,omitempty"`
		RestoreError    *PlacementRunnerStepError `json:"restore_error,omitempty"`
		RebalanceError  *PlacementRunnerStepError `json:"rebalance_error,omitempty"`
	}
	if err := fleetSettingsJSON(ctx, endpoint, fleetToken, route, body, &out, timeout); err != nil {
		return PlacementRunnerResult{}, err
	}
	if out.SchemaVersion != "witself.v0" || out.PlacementRunner == nil {
		return PlacementRunnerResult{}, fmt.Errorf("control plane returned an invalid placement runner response")
	}
	ack := out.PlacementRunner
	if patch != nil {
		expected := *patch
		if run {
			enabled := true
			expected.Enabled = &enabled
		}
		for _, err := range []error{
			fleetSettingAcknowledged("enabled", expected.Enabled, ack.Enabled),
			fleetSettingAcknowledged("restore_archives", expected.RestoreArchives, ack.RestoreArchives),
			fleetSettingAcknowledged("restore_batch", expected.RestoreBatch, ack.RestoreBatch),
			fleetSettingAcknowledged("restore_any_region", expected.RestoreAnyRegion, ack.RestoreAnyRegion),
			fleetSettingAcknowledged("rebalance", expected.Rebalance, ack.Rebalance),
			fleetSettingAcknowledged("rebalance_batch", expected.RebalanceBatch, ack.RebalanceBatch),
		} {
			if err != nil {
				return PlacementRunnerResult{}, err
			}
		}
	}
	if ack.Enabled == nil || ack.RestoreArchives == nil || ack.RestoreBatch == nil ||
		ack.RestoreAnyRegion == nil || ack.Rebalance == nil || ack.RebalanceBatch == nil ||
		*ack.RestoreBatch < 1 || *ack.RestoreBatch > 10 || *ack.RebalanceBatch < 1 || *ack.RebalanceBatch > 5 {
		return PlacementRunnerResult{}, fmt.Errorf("control plane returned missing or invalid placement runner settings")
	}
	return PlacementRunnerResult{
		PlacementRunner: PlacementRunnerConfig{
			Enabled: *ack.Enabled, RestoreArchives: *ack.RestoreArchives, RestoreBatch: *ack.RestoreBatch,
			RestoreAnyRegion: *ack.RestoreAnyRegion, Rebalance: *ack.Rebalance, RebalanceBatch: *ack.RebalanceBatch,
		},
		Restore: out.Restore, Rebalance: out.Rebalance,
		RestoreError: out.RestoreError, RebalanceError: out.RebalanceError,
	}, nil
}

// GetReaper reads the pending-account reaper configuration.
func GetReaper(ctx context.Context, endpoint, fleetToken string) (ReaperConfig, error) {
	return reaperRequest(ctx, endpoint, fleetToken, nil)
}

// SetReaper changes the pending-account reaper configuration. Disabled reapers
// omit the activation window because the control plane drops it when disabled.
func SetReaper(ctx context.Context, endpoint, fleetToken string, cfg ReaperConfig) (ReaperConfig, error) {
	if cfg.Enabled && !validReaperTTL(cfg.TTLMinutes) {
		return ReaperConfig{}, fmt.Errorf("ttl_minutes must be at least 1 and finite when the reaper is enabled")
	}
	if !cfg.Enabled {
		cfg.TTLMinutes = 0
	}
	return reaperRequest(ctx, endpoint, fleetToken, &cfg)
}

func reaperRequest(ctx context.Context, endpoint, fleetToken string, cfg *ReaperConfig) (ReaperConfig, error) {
	var body any
	if cfg != nil {
		body = cfg
	}
	var out struct {
		SchemaVersion string `json:"schema_version"`
		Reaper        *struct {
			Enabled    *bool    `json:"enabled"`
			TTLMinutes *float64 `json:"ttl_minutes"`
		} `json:"reaper"`
	}
	if err := fleetSettingsJSON(ctx, endpoint, fleetToken, "/v1/reaper", body, &out, 0); err != nil {
		return ReaperConfig{}, err
	}
	if out.SchemaVersion != "witself.v0" || out.Reaper == nil {
		return ReaperConfig{}, fmt.Errorf("control plane returned an invalid reaper response")
	}
	ack := out.Reaper
	if cfg != nil {
		if err := fleetSettingAcknowledged("enabled", &cfg.Enabled, ack.Enabled); err != nil {
			return ReaperConfig{}, err
		}
		if cfg.Enabled {
			if err := fleetSettingAcknowledged("ttl_minutes", &cfg.TTLMinutes, ack.TTLMinutes); err != nil {
				return ReaperConfig{}, err
			}
		}
	}
	if ack.Enabled == nil || (*ack.Enabled && (ack.TTLMinutes == nil || !validReaperTTL(*ack.TTLMinutes))) {
		return ReaperConfig{}, fmt.Errorf("control plane returned missing or invalid reaper settings")
	}
	result := ReaperConfig{Enabled: *ack.Enabled}
	if ack.TTLMinutes != nil {
		result.TTLMinutes = *ack.TTLMinutes
	}
	return result, nil
}

func validReaperTTL(minutes float64) bool {
	return minutes >= 1 && !math.IsNaN(minutes) && !math.IsInf(minutes, 0)
}

// GetPlacement reads the fleet account-placement strategy.
func GetPlacement(ctx context.Context, endpoint, fleetToken string) (PlacementConfig, error) {
	return placementRequest(ctx, endpoint, fleetToken, nil)
}

// SetPlacement selects weighted placement or a soft pin to one named cell.
func SetPlacement(ctx context.Context, endpoint, fleetToken string, cfg PlacementConfig) (PlacementConfig, error) {
	if err := validatePlacementConfig(cfg); err != nil {
		return PlacementConfig{}, err
	}
	if cfg.Strategy == "weighted" {
		cfg.PinnedCell = ""
	}
	return placementRequest(ctx, endpoint, fleetToken, &cfg)
}

func placementRequest(ctx context.Context, endpoint, fleetToken string, cfg *PlacementConfig) (PlacementConfig, error) {
	var body any
	if cfg != nil {
		body = cfg
	}
	var out struct {
		SchemaVersion string           `json:"schema_version"`
		Placement     *PlacementConfig `json:"placement"`
	}
	if err := fleetSettingsJSON(ctx, endpoint, fleetToken, "/v1/placement", body, &out, 0); err != nil {
		return PlacementConfig{}, err
	}
	if out.SchemaVersion != "witself.v0" || out.Placement == nil {
		return PlacementConfig{}, fmt.Errorf("control plane returned an invalid placement response")
	}
	if cfg != nil && (out.Placement.Strategy != cfg.Strategy ||
		(cfg.Strategy == "pinned" && out.Placement.PinnedCell != cfg.PinnedCell)) {
		return PlacementConfig{}, fmt.Errorf("control plane did not acknowledge placement strategy or pinned_cell")
	}
	if err := validatePlacementConfig(*out.Placement); err != nil {
		return PlacementConfig{}, fmt.Errorf("control plane returned invalid placement settings: %w", err)
	}
	return *out.Placement, nil
}

func validatePlacementConfig(cfg PlacementConfig) error {
	switch cfg.Strategy {
	case "weighted":
		return nil
	case "pinned":
		return validateFleetCellName(cfg.PinnedCell)
	default:
		return fmt.Errorf("strategy must be weighted or pinned")
	}
}

// Preserve field presence: an omitted false acknowledgement is not an echo.
func fleetSettingAcknowledged[T comparable](name string, expected, actual *T) error {
	if expected != nil && (actual == nil || *actual != *expected) {
		return fmt.Errorf("control plane did not acknowledge %s", name)
	}
	return nil
}

func fleetSettingsJSON(ctx context.Context, endpoint, fleetToken, route string, input, out any, timeout time.Duration) error {
	requestURL, err := fleetRequestURL(endpoint, route)
	if err != nil {
		return err
	}
	method := http.MethodGet
	var body []byte
	if input != nil {
		method = http.MethodPost
		body, err = json.Marshal(input)
		if err != nil {
			return err
		}
	}
	if timeout != 0 {
		return doJSONWithHeadersTimeout(ctx, method, requestURL, fleetToken, nil, body, out, timeout)
	}
	return doJSON(ctx, method, requestURL, fleetToken, body, out)
}
