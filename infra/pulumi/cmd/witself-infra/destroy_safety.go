package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"

	"golang.org/x/term"

	"github.com/witwave-ai/witself/infra/pulumi/internal/fleet"
)

// placementStatusReader is the narrow, read-only fleet surface needed by the
// destroy preflight. Keeping the guard on this interface makes its fail-closed
// behavior testable without touching a real control plane.
type placementStatusReader interface {
	GetPlacementStatus(context.Context, int) (fleet.PlacementStatus, error)
}

type destroySafetyOptions struct {
	ConfigPath         string
	ControlPlane       string
	FleetTokenFile     string
	AllowUnknownCell   bool
	ForceWithAccounts  bool
	SkipAccountCheck   bool
	YesCell            string
	Input              io.Reader
	Output             io.Writer
	Interactive        bool
	PlacementStatusAPI placementStatusReader
}

// runDestroySafety applies the destructive-operation gates in their required
// order. It must run before provider login, backend setup, stack selection, or
// any fleet mutation.
func runDestroySafety(ctx context.Context, cellName string, opts destroySafetyOptions) error {
	out := opts.Output
	if out == nil {
		out = io.Discard
	}
	if err := checkDestroyInventory(cellName, opts.ConfigPath, opts.AllowUnknownCell, out); err != nil {
		return err
	}
	if err := checkDestroyAccounts(
		ctx,
		cellName,
		opts.ControlPlane,
		opts.FleetTokenFile,
		opts.ForceWithAccounts,
		opts.SkipAccountCheck,
		opts.PlacementStatusAPI,
		out,
	); err != nil {
		return err
	}
	return confirmDestroy(cellName, opts.YesCell, opts.Interactive, opts.Input, out)
}

// destroyPreflight gives the dashboard the same two read-only guards before it
// opens its typed-name dialog. The spawned CLI child repeats both checks before
// consuming --yes-cell, closing the gap between the UI check and execution.
func (liveDataSource) destroyPreflight(ctx context.Context, configPath, cellName string) error {
	if err := checkDestroyInventory(cellName, configPath, false, io.Discard); err != nil {
		return err
	}
	cfg, _, err := loadInfraConfig(configPath)
	if err != nil {
		return err
	}
	entry := cfg.Cells[cellName]
	controlPlane, fleetTokenFile := "", ""
	if cfg.Defaults != nil {
		if cfg.Defaults.ControlPlane != nil {
			controlPlane = *cfg.Defaults.ControlPlane
		}
		if cfg.Defaults.FleetTokenFile != nil {
			fleetTokenFile = *cfg.Defaults.FleetTokenFile
		}
	}
	if entry.ControlPlane != nil {
		controlPlane = *entry.ControlPlane
	}
	if entry.FleetTokenFile != nil {
		fleetTokenFile = *entry.FleetTokenFile
	}
	return checkDestroyAccounts(
		ctx, cellName, controlPlane, fleetTokenFile,
		false, false, nil, io.Discard,
	)
}

func checkDestroyInventory(cellName, configPath string, allowUnknown bool, out io.Writer) error {
	cfg, path, err := loadInfraConfig(configPath)
	if err != nil {
		if allowUnknown {
			_, _ = fmt.Fprintf(out, "warning: --allow-unknown-cell bypassed phantom-stack protection for cell %q because the current inventory could not be verified\n", cellName)
			return nil
		}
		return fmt.Errorf(
			"phantom-stack check unavailable: cannot verify target cell %q against current inventory %s; refusing destroy (pass --allow-unknown-cell to override): %w",
			cellName, path, err,
		)
	}
	if _, ok := cfg.Cells[cellName]; ok {
		return nil
	}
	if allowUnknown {
		_, _ = fmt.Fprintf(out, "warning: --allow-unknown-cell bypassed phantom-stack protection for cell %q\n", cellName)
		return nil
	}
	return fmt.Errorf(
		"phantom-stack mismatch: target cell %q is not present in current inventory %s; refusing destroy (pass --allow-unknown-cell to override)",
		cellName, path,
	)
}

func checkDestroyAccounts(
	ctx context.Context,
	cellName, controlPlane, fleetTokenFile string,
	forceWithAccounts, skipAccountCheck bool,
	reader placementStatusReader,
	out io.Writer,
) error {
	if skipAccountCheck {
		_, _ = fmt.Fprintf(out, "warning: --skip-account-check bypassed live/archived account placement verification for cell %q\n", cellName)
		return nil
	}
	if controlPlane == "" {
		return destroyAccountCheckUnavailable(cellName, "no control plane is configured")
	}
	if reader == nil {
		client, err := fleet.NewClient(controlPlane, fleetTokenFile)
		if err != nil {
			return destroyAccountCheckUnavailable(cellName, "fleet credentials are unavailable")
		}
		reader = client
	}
	status, err := reader.GetPlacementStatus(ctx, 1)
	if err != nil {
		return destroyAccountCheckUnavailable(cellName, "the control-plane placement-status read failed")
	}

	var target *fleet.PlacementStatusCell
	for i := range status.Cells {
		if status.Cells[i].Name != cellName {
			continue
		}
		if target != nil {
			return destroyAccountCheckUnavailable(cellName, "placement status contains duplicate target-cell rows")
		}
		target = &status.Cells[i]
	}
	if target == nil {
		return destroyAccountCheckUnavailable(cellName, "placement status has no target-cell row")
	}
	if !target.ReportsAccountCounts() {
		return destroyAccountCheckUnavailable(cellName, "placement status does not explicitly report both account counts")
	}
	if target.AccountCount < 0 || target.ArchivedCount < 0 {
		return destroyAccountCheckUnavailable(cellName, "placement status contains invalid account counts")
	}

	live, archived := target.AccountCount, target.ArchivedCount
	if live == 0 && archived == 0 {
		return nil
	}
	if !forceWithAccounts {
		return fmt.Errorf(
			"live-accounts protection: cell %q has %d live account(s) and %d archived account(s) still placed there; refusing destroy (pass --force-with-accounts to proceed)",
			cellName, live, archived,
		)
	}
	_, _ = fmt.Fprintf(
		out,
		"warning: --force-with-accounts permits destroy of cell %q with %d live account(s) and %d archived account(s) still placed there\n",
		cellName, live, archived,
	)
	return nil
}

func destroyAccountCheckUnavailable(cellName, reason string) error {
	return fmt.Errorf(
		"live-accounts protection: account placement status is unavailable for cell %q (%s); refusing destroy (pass --skip-account-check to bypass)",
		cellName, reason,
	)
}

func confirmDestroy(cellName, yesCell string, interactive bool, in io.Reader, out io.Writer) error {
	if !interactive {
		if yesCell == "" {
			return fmt.Errorf("non-interactive destroy requires --yes-cell=%s", cellName)
		}
		if yesCell != cellName {
			return fmt.Errorf("--yes-cell must exactly match target cell %q", cellName)
		}
		return nil
	}

	_, _ = fmt.Fprintf(out, "Destroy target %q. Type the exact cell name to confirm: ", cellName)
	if in == nil {
		return fmt.Errorf("destroy confirmation failed: no interactive input is available")
	}
	scanner := bufio.NewScanner(in)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return fmt.Errorf("read destroy confirmation: %w", err)
		}
		return fmt.Errorf("destroy confirmation mismatch: no cell name was entered for target %q", cellName)
	}
	if scanner.Text() != cellName {
		return fmt.Errorf("destroy confirmation mismatch: typed cell name does not exactly match target %q", cellName)
	}
	return nil
}

func stdinIsTerminal(in *os.File) bool {
	return in != nil && term.IsTerminal(int(in.Fd()))
}
