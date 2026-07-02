// Package fleet is witself-infra's client for the control-plane fleet registry
// (/v1/cells on e.g. https://self.witwave.ai). The provisioner — not the cell —
// registers: `up -control-plane URL` registers as a post-step, and `destroy`
// drains + removes (or purges) as a pre-step. Authorization is the fleet token,
// read from WITSELF_FLEET_TOKEN or ~/.witself-infra/fleet.token.
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
	Name      string  `json:"name"`
	Endpoint  string  `json:"endpoint"`
	Cloud     string  `json:"cloud,omitempty"`
	Region    string  `json:"region,omitempty"`
	Weight    float64 `json:"weight,omitempty"`
	Accepting *bool   `json:"accepting,omitempty"`
}

// Client talks to one control plane with the fleet token.
type Client struct {
	base  string
	token string
	hc    *http.Client
}

// NewClient resolves the fleet token and returns a client for the control
// plane. Resolution order: the explicit tokenFile (an error if unreadable),
// then WITSELF_FLEET_TOKEN, then ~/.witself-infra/fleet.token.
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
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home for fleet token: %w", err)
	}
	path := filepath.Join(home, ".witself-infra", "fleet.token")
	t, err := readTokenFile(path)
	if err != nil {
		return "", fmt.Errorf("no fleet token: pass -fleet-token-file, set WITSELF_FLEET_TOKEN, or create %s: %w", path, err)
	}
	return t, nil
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
