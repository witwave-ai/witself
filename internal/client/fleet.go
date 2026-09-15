package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// ErrFleetCellNotRegistered means the control plane has no registry entry for
// the requested cell.
var ErrFleetCellNotRegistered = errors.New("cell is not registered with the control plane")

var fleetCellNamePattern = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)

// FleetCell is the registration contract shared by witself-admin and
// witself-infra. The control plane keeps credentials on upserts that omit them.
type FleetCell struct {
	Name       string  `json:"name"`
	Endpoint   string  `json:"endpoint"`
	Cloud      string  `json:"cloud,omitempty"`
	Region     string  `json:"region,omitempty"`
	RegionCode string  `json:"region_code,omitempty"`
	Channel    string  `json:"channel,omitempty"`
	Weight     float64 `json:"weight,omitempty"`
	Accepting  *bool   `json:"accepting,omitempty"`
	// BackupValidationTarget marks a cell that must remain placement-ineligible.
	BackupValidationTarget bool `json:"backup_validation_target"`
	// These response-only flags report credential presence without the values.
	HasProvisionToken bool `json:"has_provision_token,omitempty"`
	HasBackupToken    bool `json:"has_backup_token,omitempty"`
	// Credentials are write-only and are removed from every client read result.
	ProvisionToken string `json:"provision_token,omitempty"`
	BackupToken    string `json:"backup_token,omitempty"`
}

// FleetCellRegistrationAck preserves explicit field presence for the
// provisioner's existing credential and isolation acknowledgement guards.
type FleetCellRegistrationAck struct {
	SchemaVersion string `json:"schema_version"`
	Cell          struct {
		Name                   string `json:"name"`
		Accepting              *bool  `json:"accepting"`
		BackupValidationTarget *bool  `json:"backup_validation_target"`
		HasBackupToken         bool   `json:"has_backup_token"`
	} `json:"cell"`
}

// FleetCellResult is the public registration acknowledgement.
type FleetCellResult struct {
	SchemaVersion string    `json:"schema_version"`
	Cell          FleetCell `json:"cell"`
}

// ListFleetCells reads the fleet-token-authorized registry, with credentials
// removed even if an unexpected server response includes them.
func ListFleetCells(ctx context.Context, endpoint, fleetToken string) ([]FleetCell, error) {
	requestURL, err := fleetRequestURL(endpoint, "/v1/cells")
	if err != nil {
		return nil, err
	}
	var out struct {
		SchemaVersion string `json:"schema_version"`
		Cells         []struct {
			FleetCell
			BackupValidationTarget *bool `json:"backup_validation_target"`
		} `json:"cells"`
	}
	if err := doJSON(ctx, http.MethodGet, requestURL, fleetToken, nil, &out); err != nil {
		return nil, err
	}
	if out.SchemaVersion != "witself.v0" || out.Cells == nil {
		return nil, fmt.Errorf("control plane returned an invalid cell registry response")
	}
	cells := make([]FleetCell, len(out.Cells))
	seen := make(map[string]bool, len(out.Cells))
	for i, entry := range out.Cells {
		if !fleetCellNamePattern.MatchString(entry.Name) || entry.Accepting == nil || entry.BackupValidationTarget == nil {
			return nil, fmt.Errorf("control plane returned missing or invalid cell registry metadata")
		}
		if seen[entry.Name] {
			return nil, fmt.Errorf("control plane returned duplicate cell registry entries")
		}
		seen[entry.Name] = true
		cells[i] = entry.FleetCell
		cells[i].BackupValidationTarget = *entry.BackupValidationTarget
		redactFleetCell(&cells[i])
	}
	return cells, nil
}

// GetFleetCell finds a cell through GET /v1/cells; the control plane has no
// individual-cell GET route.
func GetFleetCell(ctx context.Context, endpoint, fleetToken, name string) (*FleetCell, error) {
	if err := validateFleetCellName(name); err != nil {
		return nil, err
	}
	cells, err := ListFleetCells(ctx, endpoint, fleetToken)
	if err != nil {
		return nil, err
	}
	for i := range cells {
		if cells[i].Name == name {
			return &cells[i], nil
		}
	}
	return nil, ErrFleetCellNotRegistered
}

// RegisterFleetCell upserts the cell using POST /v1/cells and checks that the
// acknowledgement identifies the requested cell and its isolation settings.
func RegisterFleetCell(ctx context.Context, endpoint, fleetToken string, cell FleetCell) (*FleetCellResult, error) {
	requestURL, err := fleetRequestURL(endpoint, "/v1/cells")
	if err != nil {
		return nil, err
	}
	if err := validateFleetCellName(cell.Name); err != nil {
		return nil, err
	}
	if cell.BackupValidationTarget && (cell.Accepting == nil || *cell.Accepting) {
		return nil, fmt.Errorf("backup validation target must register with accepting=false")
	}
	if cell.ProvisionToken != "" && cell.BackupToken == cell.ProvisionToken {
		return nil, fmt.Errorf("backup_token must be distinct from provision_token")
	}
	// Include a zero weight when repairing an existing entry. FleetCell keeps
	// its historical omitempty tag for the provisioner's unchanged transport.
	body, err := json.Marshal(struct {
		FleetCell
		Weight float64 `json:"weight"`
	}{FleetCell: cell, Weight: cell.Weight})
	if err != nil {
		return nil, err
	}
	var raw json.RawMessage
	if err := doJSON(ctx, http.MethodPost, requestURL, fleetToken, body, &raw); err != nil {
		return nil, err
	}
	var ack FleetCellRegistrationAck
	if err := json.Unmarshal(raw, &ack); err != nil {
		return nil, fmt.Errorf("decode cell registration acknowledgement: %w", err)
	}
	if ack.SchemaVersion != "witself.v0" || ack.Cell.Name != cell.Name {
		return nil, fmt.Errorf("control plane returned an invalid cell registration acknowledgement")
	}
	if cell.Accepting != nil && (ack.Cell.Accepting == nil || *ack.Cell.Accepting != *cell.Accepting) {
		return nil, fmt.Errorf("control plane did not acknowledge accepting=%t", *cell.Accepting)
	}
	if (cell.BackupToken != "" || cell.HasBackupToken) && !ack.Cell.HasBackupToken {
		return nil, fmt.Errorf("control plane did not acknowledge backup_token")
	}
	if ack.Cell.BackupValidationTarget == nil || *ack.Cell.BackupValidationTarget != cell.BackupValidationTarget {
		return nil, fmt.Errorf("control plane did not acknowledge backup_validation_target=%t", cell.BackupValidationTarget)
	}
	var out FleetCellResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode cell registration result: %w", err)
	}
	if (cell.ProvisionToken != "" || cell.HasProvisionToken) && !out.Cell.HasProvisionToken {
		return nil, fmt.Errorf("control plane did not acknowledge provision_token")
	}
	redactFleetCell(&out.Cell)
	return &out, nil
}

// SetFleetCellAccepting changes only the authoritative accepting state. Never
// replay the eventually consistent registry projection into registration: a
// stale snapshot could clear backup isolation or overwrite other metadata.
func SetFleetCellAccepting(ctx context.Context, endpoint, fleetToken, name string, accepting bool) (*FleetCellResult, error) {
	if err := validateFleetCellName(name); err != nil {
		return nil, err
	}
	requestURL, err := fleetRequestURL(endpoint, "/v1/cells/"+name)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(struct {
		Accepting bool `json:"accepting"`
	}{Accepting: accepting})
	if err != nil {
		return nil, err
	}
	var raw json.RawMessage
	// Older control planes reject PATCH. Do not fall back to an unsafe upsert.
	if err := doJSON(ctx, http.MethodPatch, requestURL, fleetToken, body, &raw); err != nil {
		return nil, err
	}
	var ack FleetCellRegistrationAck
	if err := json.Unmarshal(raw, &ack); err != nil {
		return nil, fmt.Errorf("decode cell accepting acknowledgement: %w", err)
	}
	if ack.SchemaVersion != "witself.v0" || ack.Cell.Name != name ||
		ack.Cell.Accepting == nil || *ack.Cell.Accepting != accepting ||
		ack.Cell.BackupValidationTarget == nil || (accepting && *ack.Cell.BackupValidationTarget) {
		return nil, fmt.Errorf("control plane returned an invalid cell accepting acknowledgement")
	}
	var out FleetCellResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode cell accepting result: %w", err)
	}
	redactFleetCell(&out.Cell)
	return &out, nil
}

// DeleteFleetCell uses the safe DELETE route. The control plane independently
// refuses cells that are accepting placements or still have account pointers.
func DeleteFleetCell(ctx context.Context, endpoint, fleetToken, name string) error {
	if err := validateFleetCellName(name); err != nil {
		return err
	}
	requestURL, err := fleetRequestURL(endpoint, "/v1/cells/"+name)
	if err != nil {
		return err
	}
	return doJSON(ctx, http.MethodDelete, requestURL, fleetToken, nil, nil)
}

func validateFleetCellName(name string) error {
	if !fleetCellNamePattern.MatchString(name) {
		return fmt.Errorf("cell name must contain 1-64 lowercase letters, digits, or hyphens")
	}
	return nil
}

func redactFleetCell(cell *FleetCell) {
	cell.ProvisionToken = ""
	cell.BackupToken = ""
}

// fleetRequestURL accepts an origin, never a request target. In particular an
// empty '#' fragment must be rejected before appending anything: string
// concatenation would turn the intended route into a fragment and send the
// bearer token and mutation to an attacker-selected path such as :purge.
func fleetRequestURL(endpoint, route string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") ||
		u.Hostname() == "" || u.User != nil || u.Opaque != "" ||
		(u.Path != "" && u.Path != "/") || u.RawPath != "" ||
		u.RawQuery != "" || u.ForceQuery || strings.ContainsAny(endpoint, "?#") {
		return "", fmt.Errorf("control-plane endpoint must be an HTTP or HTTPS origin without a path, credentials, query, or fragment")
	}
	u.Path = route
	if u.EscapedPath() != route || u.RequestURI() != route {
		return "", fmt.Errorf("invalid cell request route")
	}
	return u.String(), nil
}
