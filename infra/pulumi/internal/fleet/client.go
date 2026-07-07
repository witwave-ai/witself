// Package fleet is witself-infra's client for the control-plane fleet registry
// (/v1/cells on e.g. https://self.witwave.ai). The provisioner — not the cell —
// registers: `up -control-plane URL` registers as a post-step, and `destroy`
// drains + removes (or purges) as a pre-step. Authorization is the fleet token,
// read from -fleet-token-file, WITSELF_FLEET_TOKEN, or
// ~/.witself/tokens/fleet.token (all Witself credentials live under
// ~/.witself/tokens; WITSELF_HOME overrides the root).
package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var (
	// ErrNotRegistered means the cell has no registry entry.
	ErrNotRegistered = errors.New("cell is not registered with the control plane")
	// ErrAccountsLive means directory entries still point at the cell.
	ErrAccountsLive = errors.New("accounts still live on this cell")
	// ErrNotDrained means the cell is still accepting placements.
	ErrNotDrained = errors.New("cell is not drained")
)

// Cell is a fleet registry entry as sent/received over the API.
type Cell struct {
	Name       string  `json:"name"`
	Endpoint   string  `json:"endpoint"`
	Cloud      string  `json:"cloud,omitempty"`
	Region     string  `json:"region,omitempty"`
	RegionCode string  `json:"region_code,omitempty"`
	Channel    string  `json:"channel,omitempty"`
	Weight     float64 `json:"weight,omitempty"`
	Accepting  *bool   `json:"accepting,omitempty"`
	// ProvisionToken is the cell's account-provisioning credential, sent once at
	// registration; the control plane stores it and never returns it on reads.
	ProvisionToken string `json:"provision_token,omitempty"`
}

// Client talks to one control plane with the fleet token.
type Client struct {
	base  string
	token string
	hc    *http.Client
}

// NewClient resolves the fleet token and returns a client for the control
// plane. Resolution order: the explicit tokenFile (an error if unreadable),
// then WITSELF_FLEET_TOKEN, then ~/.witself/tokens/fleet.token (with legacy
// fallbacks to ~/.witself/fleet.token and ~/.witself-infra/fleet.token).
func NewClient(controlPlane, tokenFile string) (*Client, error) {
	tok, err := fleetToken(tokenFile)
	if err != nil {
		return nil, err
	}
	return &Client{
		base:  strings.TrimRight(controlPlane, "/"),
		token: tok,
		hc:    &http.Client{Timeout: 30 * time.Second},
	}, nil
}

func fleetToken(tokenFile string) (string, error) {
	if tokenFile != "" {
		return readTokenFile(tokenFile)
	}
	if t := strings.TrimSpace(os.Getenv("WITSELF_FLEET_TOKEN")); t != "" {
		return t, nil
	}
	root := os.Getenv("WITSELF_HOME")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home for fleet token: %w", err)
		}
		root = filepath.Join(home, ".witself")
	}
	path := filepath.Join(root, "tokens", "fleet.token")
	candidates := []string{path, filepath.Join(root, "fleet.token")} // + legacy root
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".witself-infra", "fleet.token"))
	}
	for _, c := range candidates {
		if t, err := readTokenFile(c); err == nil {
			return t, nil
		}
	}
	return "", fmt.Errorf("no fleet token: pass -fleet-token-file, set WITSELF_FLEET_TOKEN, or create %s", path)
}

func readTokenFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read fleet token file %s: %w", path, err)
	}
	t := strings.TrimSpace(string(b))
	if t == "" {
		return "", fmt.Errorf("fleet token file %s is empty", path)
	}
	return t, nil
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) (int, string, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, "", err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("control plane %s: %w", c.base, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if out != nil && resp.StatusCode < 300 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, string(raw), fmt.Errorf("decode control-plane response: %w", err)
		}
	}
	return resp.StatusCode, string(raw), nil
}

// Register upserts the cell's registry entry (accepting defaults to true).
func (c *Client) Register(ctx context.Context, cell Cell) error {
	code, body, err := c.do(ctx, http.MethodPost, "/v1/cells", cell, nil)
	if err != nil {
		return err
	}
	if code != http.StatusOK && code != http.StatusCreated {
		return fmt.Errorf("register cell: HTTP %d: %s", code, strings.TrimSpace(body))
	}
	return nil
}

// lookup fetches the cell's current registry entry.
func (c *Client) lookup(ctx context.Context, name string) (*Cell, error) {
	var out struct {
		Cells []Cell `json:"cells"`
	}
	code, body, err := c.do(ctx, http.MethodGet, "/v1/cells", nil, &out)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("list cells: HTTP %d: %s", code, strings.TrimSpace(body))
	}
	for i := range out.Cells {
		if out.Cells[i].Name == name {
			return &out.Cells[i], nil
		}
	}
	return nil, ErrNotRegistered
}

// Drain re-registers the cell with accepting=false so placement stops.
func (c *Client) Drain(ctx context.Context, name string) error {
	cell, err := c.lookup(ctx, name)
	if err != nil {
		return err
	}
	f := false
	cell.Accepting = &f
	return c.Register(ctx, *cell)
}

// Delete removes a drained, account-free cell from the registry.
func (c *Client) Delete(ctx context.Context, name string) error {
	code, body, err := c.do(ctx, http.MethodDelete, "/v1/cells/"+name, nil, nil)
	if err != nil {
		return err
	}
	switch code {
	case http.StatusNoContent:
		return nil
	case http.StatusNotFound:
		return ErrNotRegistered
	case http.StatusConflict:
		if strings.Contains(body, "drained") {
			return ErrNotDrained
		}
		return ErrAccountsLive
	default:
		return fmt.Errorf("delete cell: HTTP %d: %s", code, strings.TrimSpace(body))
	}
}

// EvacuationResult reports one :evacuate call's outcome. Evacuated is the
// per-account report for THIS batch (ok/error each); Remaining is what the
// control plane still sees pointing at this cell — the caller loops until
// it hits zero.
type EvacuationResult struct {
	Evacuated []EvacuatedAccount `json:"evacuated"`
	Remaining int                `json:"remaining"`
}

// EvacuatedAccount is one line in an evacuation batch's report. Reaped is
// set for pending accounts the Worker dropped inline instead of archiving —
// signups too incomplete to preserve, treated the same way the pending
// expiry sweep treats them.
type EvacuatedAccount struct {
	AccountID string `json:"account_id"`
	OK        bool   `json:"ok"`
	Error     string `json:"error,omitempty"`
	Reaped    bool   `json:"reaped,omitempty"`
}

// Evacuate asks the control plane to move a batch of the cell's accounts into
// R2 archives (per-account file), retiring their acct: routing pointers. The
// call is bounded in wall-clock by batch size; the caller loops until
// Remaining is zero. Uses a longer HTTP timeout than the routine registry
// endpoints because each account's archive can stream for a while.
func (c *Client) Evacuate(ctx context.Context, name string, batch int) (EvacuationResult, error) {
	body := map[string]int{"batch": batch}
	rdr, err := marshalBody(body)
	if err != nil {
		return EvacuationResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/cells/"+name+":evacuate", rdr)
	if err != nil {
		return EvacuationResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	// Evacuation streams whole account archives to R2; the per-call ceiling
	// needs to be well above the routine registry HTTP client's 30s.
	hc := &http.Client{Timeout: 10 * time.Minute}
	resp, err := hc.Do(req)
	if err != nil {
		return EvacuationResult{}, fmt.Errorf("control plane %s: %w", c.base, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch resp.StatusCode {
	case http.StatusOK:
		var out EvacuationResult
		if err := json.Unmarshal(raw, &out); err != nil {
			return EvacuationResult{}, fmt.Errorf("decode evacuate response: %w", err)
		}
		return out, nil
	case http.StatusNotFound:
		// 404 could mean "unknown cell" (JSON body from the fleet handler)
		// OR "route missing" (control plane predates the :evacuate slice).
		// The bodies differ; distinguishing keeps the operator error honest
		// when they see it. Unknown-cell → ErrNotRegistered; anything else
		// → the raw HTTP error so they know to upgrade the control plane.
		if strings.Contains(string(raw), "unknown cell") {
			return EvacuationResult{}, ErrNotRegistered
		}
		return EvacuationResult{}, fmt.Errorf("evacuate cell: HTTP 404: %s (control plane may be too old — upgrade, or re-run with -destroy-accounts)", strings.TrimSpace(string(raw)))
	case http.StatusConflict:
		return EvacuationResult{}, ErrNotDrained
	default:
		return EvacuationResult{}, fmt.Errorf("evacuate cell: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
}

// ErrCellDrained means the cell was registered with accepting=false, so the
// control plane refused to route new accounts (whether from signups or
// restores) into it. Re-register the cell as accepting=true and retry.
var ErrCellDrained = errors.New("cell is drained — cannot restore accounts into it")

// ProbeResult reports what a POST /v1/cells/{name}:probe call saw when it
// tried to reach the cell's API from inside the Cloudflare Worker. OK=true
// means the Worker's GET on <cell>/v1/version returned a witself-server-
// shaped JSON body. Otherwise Reason describes what actually happened
// (DNS error, TLS error, HTTP status code, non-JSON response).
type ProbeResult struct {
	OK          bool   `json:"ok"`
	Reason      string `json:"reason,omitempty"`
	CellStatus  int    `json:"cell_status,omitempty"`
	CellVersion string `json:"cell_version,omitempty"`
}

// Probe asks the control plane whether it can reach the named cell. The
// Worker is the client that will do the actual restore, so its view of
// cell reachability is what the driver's wait step should trust — not the
// operator's local resolver, which can hold stale NXDOMAIN across
// destroy+up cycles for hours (issue #22).
//
// Returns ErrNotRegistered for a 404 unknown cell (mirroring Evacuate /
// Restore's disambiguated 404 handling). Any other HTTP failure comes
// through as a generic error. A 200 always decodes into ProbeResult —
// including probe-said-cell-is-not-ready cases, where OK is false and
// Reason names the problem.
func (c *Client) Probe(ctx context.Context, name string) (ProbeResult, error) {
	code, body, err := c.do(ctx, http.MethodPost, "/v1/cells/"+name+":probe", struct{}{}, nil)
	if err != nil {
		return ProbeResult{}, err
	}
	if code == http.StatusNotFound {
		// Same disambiguation as Evacuate/Restore — an "unknown cell" body
		// means the fleet doesn't know about it; anything else is either
		// no-route (control plane too old) or transport-level 404.
		if strings.Contains(body, "unknown cell") {
			return ProbeResult{}, ErrNotRegistered
		}
		return ProbeResult{}, fmt.Errorf("probe cell: HTTP 404: %s (control plane may be too old — upgrade)", strings.TrimSpace(body))
	}
	if code != http.StatusOK {
		return ProbeResult{}, fmt.Errorf("probe cell: HTTP %d: %s", code, strings.TrimSpace(body))
	}
	var out ProbeResult
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		return ProbeResult{}, fmt.Errorf("decode probe response: %w", err)
	}
	return out, nil
}

// RestoreResult reports one :restore call's outcome. Restored is the
// per-account report for THIS batch; Remaining is the number of archived:
// pointers still awaiting placement in the requested restore scope. The caller
// loops until Remaining is zero.
type RestoreResult struct {
	Restored  []RestoredAccount `json:"restored"`
	Blocked   []BlockedAccount  `json:"blocked,omitempty"`
	Remaining int               `json:"remaining"`
	Unplaced  int               `json:"unplaced,omitempty"`
	Region    string            `json:"region,omitempty"`
}

// RestoredAccount is one line in a restore batch's report. Same shape as
// EvacuatedAccount, distinct type so a caller cannot accidentally mix them.
type RestoredAccount struct {
	AccountID string `json:"account_id"`
	OK        bool   `json:"ok"`
	Cell      string `json:"cell,omitempty"`
	Error     string `json:"error,omitempty"`
}

type BlockedAccount struct {
	AccountID string `json:"account_id"`
	Reason    string `json:"reason"`
}

type RebalanceResult struct {
	Rebalanced []RebalancedAccount `json:"rebalanced"`
	Skipped    []SkippedAccount    `json:"skipped,omitempty"`
	Remaining  int                 `json:"remaining"`
	DryRun     bool                `json:"dry_run,omitempty"`
}

type RebalancedAccount struct {
	AccountID string `json:"account_id"`
	OK        bool   `json:"ok"`
	FromCell  string `json:"from_cell"`
	ToCell    string `json:"to_cell"`
	Reason    string `json:"reason,omitempty"`
	DryRun    bool   `json:"dry_run,omitempty"`
	Error     string `json:"error,omitempty"`
}

type SkippedAccount struct {
	AccountID string `json:"account_id"`
	Cell      string `json:"cell,omitempty"`
	Reason    string `json:"reason"`
}

type PlacementRunnerConfig struct {
	Enabled          bool `json:"enabled"`
	RestoreArchives  bool `json:"restore_archives"`
	RestoreBatch     int  `json:"restore_batch"`
	RestoreAnyRegion bool `json:"restore_any_region"`
	Rebalance        bool `json:"rebalance"`
	RebalanceBatch   int  `json:"rebalance_batch"`
}

type PlacementRunnerResult struct {
	PlacementRunner PlacementRunnerConfig     `json:"placement_runner"`
	Restore         *RestoreResult            `json:"restore,omitempty"`
	Rebalance       *RebalanceResult          `json:"rebalance,omitempty"`
	RestoreError    *PlacementRunnerStepError `json:"restore_error,omitempty"`
	RebalanceError  *PlacementRunnerStepError `json:"rebalance_error,omitempty"`
}

type PlacementRunnerStepError struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

// Restore asks the control plane to pull a batch of archived accounts from R2
// and land them on the named cell. By default the Worker only selects archives
// whose stored region matches the target cell; allRegions is an operator
// override for intentionally landing every archived account on one cell.
// The call is bounded in wall-clock by batch size; the caller loops until
// Remaining is zero. Same longer HTTP timeout as Evacuate — each archive
// streams from R2 through the Worker into the cell's :import.
func (c *Client) Restore(ctx context.Context, name string, batch int, allRegions bool) (RestoreResult, error) {
	body := struct {
		Batch      int  `json:"batch"`
		AllRegions bool `json:"all_regions,omitempty"`
	}{
		Batch:      batch,
		AllRegions: allRegions,
	}
	rdr, err := marshalBody(body)
	if err != nil {
		return RestoreResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/cells/"+name+":restore", rdr)
	if err != nil {
		return RestoreResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	hc := &http.Client{Timeout: 10 * time.Minute}
	resp, err := hc.Do(req)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("control plane %s: %w", c.base, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch resp.StatusCode {
	case http.StatusOK:
		var out RestoreResult
		if err := json.Unmarshal(raw, &out); err != nil {
			return RestoreResult{}, fmt.Errorf("decode restore response: %w", err)
		}
		return out, nil
	case http.StatusNotFound:
		// Same disambiguation as Evacuate: unknown cell vs missing route.
		if strings.Contains(string(raw), "unknown cell") {
			return RestoreResult{}, ErrNotRegistered
		}
		return RestoreResult{}, fmt.Errorf("restore cell: HTTP 404: %s (control plane may be too old — upgrade)", strings.TrimSpace(string(raw)))
	case http.StatusConflict:
		return RestoreResult{}, ErrCellDrained
	default:
		return RestoreResult{}, fmt.Errorf("restore cell: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
}

func (c *Client) RestorePlacement(ctx context.Context, batch int, allRegions bool) (RestoreResult, error) {
	body := struct {
		Batch      int  `json:"batch"`
		AllRegions bool `json:"all_regions,omitempty"`
	}{
		Batch:      batch,
		AllRegions: allRegions,
	}
	rdr, err := marshalBody(body)
	if err != nil {
		return RestoreResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/placement:restore", rdr)
	if err != nil {
		return RestoreResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	hc := &http.Client{Timeout: 10 * time.Minute}
	resp, err := hc.Do(req)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("control plane %s: %w", c.base, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return RestoreResult{}, fmt.Errorf("placement restore: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out RestoreResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return RestoreResult{}, fmt.Errorf("decode placement restore response: %w", err)
	}
	return out, nil
}

func (c *Client) Rebalance(ctx context.Context, batch int, dryRun bool) (RebalanceResult, error) {
	body := struct {
		Batch  int  `json:"batch"`
		DryRun bool `json:"dry_run,omitempty"`
	}{
		Batch:  batch,
		DryRun: dryRun,
	}
	rdr, err := marshalBody(body)
	if err != nil {
		return RebalanceResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/placement:rebalance", rdr)
	if err != nil {
		return RebalanceResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	hc := &http.Client{Timeout: 10 * time.Minute}
	resp, err := hc.Do(req)
	if err != nil {
		return RebalanceResult{}, fmt.Errorf("control plane %s: %w", c.base, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return RebalanceResult{}, fmt.Errorf("rebalance: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out RebalanceResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return RebalanceResult{}, fmt.Errorf("decode rebalance response: %w", err)
	}
	return out, nil
}

func (c *Client) GetPlacementRunner(ctx context.Context) (PlacementRunnerConfig, error) {
	var out struct {
		PlacementRunner PlacementRunnerConfig `json:"placement_runner"`
	}
	code, body, err := c.do(ctx, http.MethodGet, "/v1/placement-runner", nil, &out)
	if err != nil {
		return PlacementRunnerConfig{}, err
	}
	if code != http.StatusOK {
		return PlacementRunnerConfig{}, fmt.Errorf("placement runner: HTTP %d: %s", code, strings.TrimSpace(body))
	}
	return out.PlacementRunner, nil
}

func (c *Client) SetPlacementRunner(ctx context.Context, cfg PlacementRunnerConfig) (PlacementRunnerConfig, error) {
	var out struct {
		PlacementRunner PlacementRunnerConfig `json:"placement_runner"`
	}
	code, body, err := c.do(ctx, http.MethodPost, "/v1/placement-runner", cfg, &out)
	if err != nil {
		return PlacementRunnerConfig{}, err
	}
	if code != http.StatusOK {
		return PlacementRunnerConfig{}, fmt.Errorf("set placement runner: HTTP %d: %s", code, strings.TrimSpace(body))
	}
	return out.PlacementRunner, nil
}

func (c *Client) RunPlacementRunner(ctx context.Context, cfg PlacementRunnerConfig) (PlacementRunnerResult, error) {
	rdr, err := marshalBody(cfg)
	if err != nil {
		return PlacementRunnerResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/placement:run", rdr)
	if err != nil {
		return PlacementRunnerResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	hc := &http.Client{Timeout: 10 * time.Minute}
	resp, err := hc.Do(req)
	if err != nil {
		return PlacementRunnerResult{}, fmt.Errorf("control plane %s: %w", c.base, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return PlacementRunnerResult{}, fmt.Errorf("placement runner: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out PlacementRunnerResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return PlacementRunnerResult{}, fmt.Errorf("decode placement runner response: %w", err)
	}
	return out, nil
}

// marshalBody encodes body as JSON if non-nil.
func marshalBody(body any) (io.Reader, error) {
	if body == nil {
		return nil, nil
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return bytes.NewReader(b), nil
}

// Purge force-removes the cell AND every directory entry pointing at it — for
// teardowns where the cell's data is genuinely dying. Returns the number of
// account entries removed.
func (c *Client) Purge(ctx context.Context, name string) (int, error) {
	var out struct {
		PurgedAccounts int  `json:"purged_accounts"`
		CellDeleted    bool `json:"cell_deleted"`
	}
	code, body, err := c.do(ctx, http.MethodPost, "/v1/cells/"+name+":purge", struct{}{}, &out)
	if err != nil {
		return 0, err
	}
	if code != http.StatusOK {
		return 0, fmt.Errorf("purge cell: HTTP %d: %s", code, strings.TrimSpace(body))
	}
	return out.PurgedAccounts, nil
}
