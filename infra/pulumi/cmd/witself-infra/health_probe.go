package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/witwave-ai/witself/infra/pulumi/internal/fleet"
)

const (
	defaultHealthProbeTimeout = 5 * time.Second
	maxConcurrentHealthProbes = 8
)

// automationHealthState is the stable state vocabulary emitted by
// `witself-infra health --json`. Keep this smaller than the dashboard's
// presentation-oriented scale: automation needs a clear alert/no-alert answer.
type automationHealthState string

const (
	automationHealthOK       automationHealthState = "ok"
	automationHealthDegraded automationHealthState = "degraded"
	automationHealthTimeout  automationHealthState = "timeout"
	automationHealthDown     automationHealthState = "down"
)

// automationHealthResult is one NDJSON record from the health command.
// Detail is deliberately omitted: the contract stays value-free and stable for
// cron/monitoring consumers while the interactive dashboard carries diagnostics.
type automationHealthResult struct {
	Name      string                `json:"name"`
	State     automationHealthState `json:"state"`
	LatencyMS int64                 `json:"latency_ms"`
	CheckedAt time.Time             `json:"checked_at"`
}

var errHealthNotOK = errors.New("one or more health targets are not ok")

// fleetHealthClient is the read-only subset used by health probes. The seam
// keeps classification and concurrency tests independent of cloud services.
type fleetHealthClient interface {
	ListCells(ctx context.Context) ([]fleet.Cell, error)
	Probe(ctx context.Context, name string) (fleet.ProbeResult, error)
}

type fleetHealthClientFactory func(controlPlane, tokenFile string) (fleetHealthClient, error)

type automationHealthTarget struct {
	name  string
	probe func(context.Context) automationHealthState
}

func newFleetHealthClient(controlPlane, tokenFile string) (fleetHealthClient, error) {
	return fleet.NewClient(controlPlane, tokenFile)
}

func classifyHealthError(err error) automationHealthState {
	switch {
	case err == nil:
		return automationHealthOK
	case errors.Is(err, context.DeadlineExceeded):
		return automationHealthTimeout
	default:
		return automationHealthDown
	}
}

func classifyCellProbe(result fleet.ProbeResult, err error) automationHealthState {
	if err != nil {
		return classifyHealthError(err)
	}
	if result.OK {
		return automationHealthOK
	}
	if probeResultTimedOut(result.CellStatus, result.Reason) {
		return automationHealthTimeout
	}
	return automationHealthDegraded
}

// A control plane may finish its own bounded upstream fetch before the
// caller's deadline and return the timeout as an OK=false probe result. Keep
// that distinct from a reachable-but-unhealthy cell. Reason is the only
// timeout discriminator exposed by older control planes, so accept the common
// platform spellings as well as the two HTTP timeout statuses.
func probeResultTimedOut(status int, reason string) bool {
	if status == http.StatusRequestTimeout || status == http.StatusGatewayTimeout {
		return true
	}
	reason = strings.ToLower(reason)
	return strings.Contains(reason, "timeout") ||
		strings.Contains(reason, "timed out") ||
		strings.Contains(reason, "deadline exceeded")
}

func controlPlaneHealthTargetName(controlPlane string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(controlPlane), "/")
	if parsed, err := url.Parse(trimmed); err == nil && parsed.Host != "" {
		name := parsed.Host
		if parsed.EscapedPath() != "" && parsed.EscapedPath() != "/" {
			name += parsed.EscapedPath()
		}
		return "control-plane:" + name
	}
	return "control-plane:" + trimmed
}

func effectiveHealthConnection(entry cellEntry, defaults *cellEntry) (controlPlane, tokenFile string) {
	if defaults != nil {
		if defaults.ControlPlane != nil {
			controlPlane = *defaults.ControlPlane
		}
		if defaults.FleetTokenFile != nil {
			tokenFile = *defaults.FleetTokenFile
		}
	}
	if entry.ControlPlane != nil {
		controlPlane = *entry.ControlPlane
	}
	if entry.FleetTokenFile != nil {
		tokenFile = *entry.FleetTokenFile
	}
	return strings.TrimRight(strings.TrimSpace(controlPlane), "/"), tokenFile
}

// configuredHealthTargets maps the local inventory to the same control-plane
// probe used by the dashboard. Control planes are de-duplicated; configured
// cells without a control plane remain visible as down instead of disappearing
// from an automation report.
func configuredHealthTargets(configPath, selectedCell string, factory fleetHealthClientFactory) ([]automationHealthTarget, error) {
	cfg, _, err := loadInfraConfig(configPath)
	if err != nil {
		return nil, err
	}
	if factory == nil {
		factory = newFleetHealthClient
	}

	names := make([]string, 0, len(cfg.Cells))
	for name := range cfg.Cells {
		if selectedCell == "" || name == selectedCell {
			names = append(names, name)
		}
	}
	if selectedCell != "" {
		if _, ok := cfg.Cells[selectedCell]; !ok {
			return nil, fmt.Errorf("cell %q is not in the infra inventory", selectedCell)
		}
	}
	sort.Strings(names)

	type cpConnection struct {
		url       string
		tokenFile string
	}
	cpByURL := map[string]cpConnection{}
	if selectedCell == "" && cfg.Defaults != nil && cfg.Defaults.ControlPlane != nil {
		cp, tokenFile := effectiveHealthConnection(cellEntry{}, cfg.Defaults)
		if cp != "" {
			cpByURL[cp] = cpConnection{url: cp, tokenFile: tokenFile}
		}
	}

	targets := make([]automationHealthTarget, 0, len(names)+len(cpByURL))
	for _, name := range names {
		entry := cfg.Cells[name]
		cp, tokenFile := effectiveHealthConnection(entry, cfg.Defaults)
		if cp != "" {
			connection, exists := cpByURL[cp]
			if !exists || (connection.tokenFile == "" && tokenFile != "") {
				cpByURL[cp] = cpConnection{url: cp, tokenFile: tokenFile}
			}
		}

		cellName, controlPlane, tokenPath := name, cp, tokenFile
		targets = append(targets, automationHealthTarget{
			name: cellName,
			probe: func(ctx context.Context) automationHealthState {
				if controlPlane == "" {
					return automationHealthDown
				}
				client, err := factory(controlPlane, tokenPath)
				if err != nil {
					return classifyHealthError(err)
				}
				result, err := client.Probe(ctx, cellName)
				return classifyCellProbe(result, err)
			},
		})
	}

	cpURLs := make([]string, 0, len(cpByURL))
	for cp := range cpByURL {
		cpURLs = append(cpURLs, cp)
	}
	sort.Strings(cpURLs)
	for _, cp := range cpURLs {
		connection := cpByURL[cp]
		targets = append(targets, automationHealthTarget{
			name: controlPlaneHealthTargetName(connection.url),
			probe: func(ctx context.Context) automationHealthState {
				client, err := factory(connection.url, connection.tokenFile)
				if err != nil {
					return classifyHealthError(err)
				}
				_, err = client.ListCells(ctx)
				return classifyHealthError(err)
			},
		})
	}

	sort.Slice(targets, func(i, j int) bool { return targets[i].name < targets[j].name })
	return targets, nil
}

// probeAutomationHealth runs independent per-target contexts concurrently.
// Results retain target order, so NDJSON is deterministic even when a later
// target answers before an earlier one.
func probeAutomationHealth(ctx context.Context, targets []automationHealthTarget, timeout time.Duration, now func() time.Time) []automationHealthResult {
	if now == nil {
		now = time.Now
	}
	results := make([]automationHealthResult, len(targets))
	if len(targets) == 0 {
		return results
	}
	limit := min(maxConcurrentHealthProbes, len(targets))
	semaphore := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i, target := range targets {
		i, target := i, target
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				results[i] = automationHealthResult{
					Name:      target.name,
					State:     classifyHealthError(ctx.Err()),
					CheckedAt: now().UTC(),
				}
				return
			}

			started := now()
			probeCtx, cancel := context.WithTimeout(ctx, timeout)
			state := target.probe(probeCtx)
			cancel()
			checkedAt := now()
			latency := checkedAt.Sub(started).Milliseconds()
			if latency < 0 {
				latency = 0
			}
			results[i] = automationHealthResult{
				Name:      target.name,
				State:     state,
				LatencyMS: latency,
				CheckedAt: checkedAt.UTC(),
			}
		}()
	}
	wg.Wait()
	return results
}

func emitAutomationHealth(results []automationHealthResult, jsonOutput bool, out io.Writer) error {
	if jsonOutput {
		encoder := json.NewEncoder(out)
		for _, result := range results {
			if err := encoder.Encode(result); err != nil {
				return fmt.Errorf("write health JSON: %w", err)
			}
		}
		return nil
	}
	for _, result := range results {
		if _, err := fmt.Fprintf(out, "%-44s %-10s %dms %s\n",
			result.Name, result.State, result.LatencyMS, result.CheckedAt.Format(time.RFC3339)); err != nil {
			return fmt.Errorf("write health output: %w", err)
		}
	}
	return nil
}

func runHealthCommand(ctx context.Context, configPath, selectedCell string, timeout time.Duration, jsonOutput bool, out io.Writer) error {
	if timeout <= 0 {
		return fmt.Errorf("-timeout must be greater than zero")
	}
	targets, err := configuredHealthTargets(configPath, selectedCell, nil)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		return fmt.Errorf("infra inventory has no health targets")
	}
	results := probeAutomationHealth(ctx, targets, timeout, time.Now)
	if err := emitAutomationHealth(results, jsonOutput, out); err != nil {
		return err
	}
	notOK := 0
	for _, result := range results {
		if result.State != automationHealthOK {
			notOK++
		}
	}
	if notOK > 0 {
		return fmt.Errorf("%w: %d of %d targets", errHealthNotOK, notOK, len(results))
	}
	return nil
}
