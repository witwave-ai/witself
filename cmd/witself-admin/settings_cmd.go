package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"sort"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/cliout"
)

func settingsUsage(w io.Writer) {
	cliout.Line(w, "usage: witself-admin settings show|placement-runner|reaper|placement ...")
	cliout.Line(w, "  show                                         Read all three control-plane settings")
	cliout.Line(w, "  placement-runner show|enable|disable          Inspect or toggle scheduled placement")
	cliout.Line(w, "  placement-runner set|run [flags]              Update selected settings or run once (10-minute timeout)")
	cliout.Line(w, "    --restore-archives=BOOL --restore-batch=1..10 --restore-any-region=BOOL")
	cliout.Line(w, "    --rebalance=BOOL --rebalance-batch=1..5      Only explicitly supplied flags override stored settings")
	cliout.Line(w, "  reaper show|enable --ttl-minutes N|disable     Configure never-activated account cleanup")
	cliout.Line(w, "    N is finite and at least 1; fractional minutes are accepted")
	cliout.Line(w, "  placement show|set --strategy weighted|pinned [--pinned-cell NAME]")
	cliout.Line(w, "Every write requires --yes. Reaper enable requires cells to serve :reap first.")
	cliout.Line(w, "Flags: --endpoint URL, --fleet-token TOKEN (or --token / --token-file), --json")
}

func settingsCmd(args []string) int {
	if len(args) == 0 {
		settingsUsage(os.Stderr)
		return 2
	}
	switch args[0] {
	case "help", "--help", "-h":
		settingsUsage(os.Stdout)
		return 0
	case "show":
		return settingsShow(args[1:])
	case "placement-runner":
		return settingsPlacementRunner(args[1:])
	case "reaper":
		return settingsReaper(args[1:])
	case "placement":
		return settingsPlacement(args[1:])
	default:
		return settingsError(fmt.Errorf("unknown subcommand %q", args[0]), 2)
	}
}

// Reuse the cell-registry fleet-token resolution, with settings-specific help.
func newSettingsFlags(verb string) cellRegistryFlags {
	fs := flag.NewFlagSet("settings "+verb, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return cellRegistryFlags{
		fs:         fs,
		endpoint:   fs.String("endpoint", "", "control-plane URL"),
		fleetToken: fs.String("fleet-token", "", "fleet shared secret"),
		token:      fs.String("token", "", "fleet shared secret (alias for --fleet-token; not an admin token)"),
		tokenFile:  fs.String("token-file", "", "file containing the fleet shared secret"),
		json:       jsonFlag(fs),
	}
}

func settingsError(err error, code int) int {
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	fmt.Fprintf(os.Stderr, "witself-admin settings: %v\n", err)
	return code
}

func parseSettingsFlags(c cellRegistryFlags, args []string, yes *bool) error {
	if err := c.fs.Parse(args); err != nil {
		return err
	}
	if c.fs.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	if yes != nil && !*yes {
		return fmt.Errorf("%s requires --yes", c.fs.Name())
	}
	return nil
}

func settingsSectionVerb(section string, args []string) (string, int) {
	if len(args) == 0 {
		settingsUsage(os.Stderr)
		return "", 2
	}
	if args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		settingsUsage(os.Stdout)
		return "", 0
	}
	verb := args[0]
	valid := verb == "show" || (section == "placement" && verb == "set") ||
		(section != "placement" && (verb == "enable" || verb == "disable")) ||
		(section == "placement-runner" && (verb == "set" || verb == "run"))
	if !valid {
		return "", settingsError(fmt.Errorf("unknown %s subcommand %q", section, verb), 2)
	}
	return verb, 0
}

func settingsShow(args []string) int {
	c := newSettingsFlags("show")
	if err := parseSettingsFlags(c, args, nil); err != nil {
		return settingsError(err, 2)
	}
	ep, tok, err := c.credentials()
	if err != nil {
		return settingsError(err, 2)
	}
	ctx := context.Background()
	runner, err := client.GetPlacementRunner(ctx, ep, tok)
	if err != nil {
		return settingsError(err, 1)
	}
	reaper, err := client.GetReaper(ctx, ep, tok)
	if err != nil {
		return settingsError(err, 1)
	}
	placement, err := client.GetPlacement(ctx, ep, tok)
	if err != nil {
		return settingsError(err, 1)
	}
	return printSettingsResponse(settingsJSONMap(runner, reaper, placement), *c.json)
}

func settingsPlacementRunner(args []string) int {
	verb, code := settingsSectionVerb("placement-runner", args)
	if verb == "" {
		return code
	}
	c := newSettingsFlags("placement-runner " + verb)
	var yes *bool
	if verb != "show" {
		yes = c.fs.Bool("yes", false, "confirm fleet-wide placement settings or account movement")
	}
	var patch client.PlacementRunnerPatch
	if verb == "set" || verb == "run" {
		restoreArchives := c.fs.Bool("restore-archives", false, "restore archived accounts (use =true or =false)")
		restoreBatch := c.fs.Int("restore-batch", 0, "archive restore batch size (1-10)")
		restoreAnyRegion := c.fs.Bool("restore-any-region", false, "allow restores in any region (use =true or =false)")
		rebalance := c.fs.Bool("rebalance", false, "rebalance live accounts (use =true or =false)")
		rebalanceBatch := c.fs.Int("rebalance-batch", 0, "live rebalance batch size (1-5)")
		if err := parseSettingsFlags(c, args[1:], yes); err != nil {
			return settingsError(err, 2)
		}
		// Unvisited flags must stay nil: the control plane merges the patch with
		// stored settings, and would clamp unintended zero batch sizes to one.
		c.fs.Visit(func(f *flag.Flag) {
			switch f.Name {
			case "restore-archives":
				patch.RestoreArchives = restoreArchives
			case "restore-batch":
				patch.RestoreBatch = restoreBatch
			case "restore-any-region":
				patch.RestoreAnyRegion = restoreAnyRegion
			case "rebalance":
				patch.Rebalance = rebalance
			case "rebalance-batch":
				patch.RebalanceBatch = rebalanceBatch
			}
		})
		if verb == "set" && patch == (client.PlacementRunnerPatch{}) {
			return settingsError(fmt.Errorf("set requires at least one placement-runner setting flag"), 2)
		}
		if patch.RestoreBatch != nil && (*patch.RestoreBatch < 1 || *patch.RestoreBatch > 10) {
			return settingsError(fmt.Errorf("--restore-batch must be between 1 and 10"), 2)
		}
		if patch.RebalanceBatch != nil && (*patch.RebalanceBatch < 1 || *patch.RebalanceBatch > 5) {
			return settingsError(fmt.Errorf("--rebalance-batch must be between 1 and 5"), 2)
		}
	} else {
		if err := parseSettingsFlags(c, args[1:], yes); err != nil {
			return settingsError(err, 2)
		}
		if verb != "show" {
			enabled := verb == "enable"
			patch.Enabled = &enabled
		}
	}
	ep, tok, err := c.credentials()
	if err != nil {
		return settingsError(err, 2)
	}
	ctx := context.Background()
	if verb == "run" {
		result, err := client.RunPlacementRunner(ctx, ep, tok, patch)
		if err != nil {
			return settingsError(err, 1)
		}
		if code := printSettingsResponse(placementRunnerResultJSONMap(result), *c.json); code != 0 {
			return code
		}
		if result.RestoreError != nil || result.RebalanceError != nil ||
			placementRunnerBatchFailed(result.Restore, "restored") || placementRunnerBatchFailed(result.Rebalance, "rebalanced") {
			return 1
		}
		return 0
	}
	var config client.PlacementRunnerConfig
	if verb == "show" {
		config, err = client.GetPlacementRunner(ctx, ep, tok)
	} else {
		config, err = client.SetPlacementRunner(ctx, ep, tok, patch)
	}
	if err != nil {
		return settingsError(err, 1)
	}
	return printSettingsResponse(placementRunnerJSONMap(config), *c.json)
}

// The Worker can return HTTP 200 for a batch containing failed account
// operations. Inspect explicit failures while preserving the raw result output.
func placementRunnerBatchFailed(body json.RawMessage, key string) bool {
	var result map[string]json.RawMessage
	if err := json.Unmarshal(body, &result); err != nil {
		return false
	}
	var accounts []struct {
		OK *bool `json:"ok"`
	}
	if err := json.Unmarshal(result[key], &accounts); err != nil {
		return false
	}
	for _, account := range accounts {
		if account.OK != nil && !*account.OK {
			return true
		}
	}
	return false
}

func settingsReaper(args []string) int {
	verb, code := settingsSectionVerb("reaper", args)
	if verb == "" {
		return code
	}
	c := newSettingsFlags("reaper " + verb)
	var yes *bool
	if verb != "show" {
		yes = c.fs.Bool("yes", false, "confirm fleet-wide never-activated account cleanup settings")
	}
	var ttl *float64
	if verb == "enable" {
		ttl = c.fs.Float64("ttl-minutes", 0, "minimum account age in minutes (required, finite and at least 1; fractions allowed; cells must serve :reap first)")
	}
	if err := parseSettingsFlags(c, args[1:], yes); err != nil {
		return settingsError(err, 2)
	}
	config := client.ReaperConfig{Enabled: verb == "enable"}
	if ttl != nil {
		if *ttl < 1 || math.IsNaN(*ttl) || math.IsInf(*ttl, 0) {
			return settingsError(fmt.Errorf("--ttl-minutes must be at least 1 and finite"), 2)
		}
		config.TTLMinutes = *ttl
	}
	ep, tok, err := c.credentials()
	if err != nil {
		return settingsError(err, 2)
	}
	if verb == "show" {
		config, err = client.GetReaper(context.Background(), ep, tok)
	} else {
		if verb == "enable" {
			fmt.Fprintln(os.Stderr, "Note: cells must serve :reap before enabling fleet-wide never-activated account cleanup.")
		}
		config, err = client.SetReaper(context.Background(), ep, tok, config)
	}
	if err != nil {
		return settingsError(err, 1)
	}
	return printSettingsResponse(reaperJSONMap(config), *c.json)
}

func settingsPlacement(args []string) int {
	verb, code := settingsSectionVerb("placement", args)
	if verb == "" {
		return code
	}
	c := newSettingsFlags("placement " + verb)
	var yes *bool
	var strategy, pinnedCell *string
	if verb == "set" {
		yes = c.fs.Bool("yes", false, "confirm fleet-wide placement strategy")
		strategy = c.fs.String("strategy", "", "placement strategy: weighted or pinned (required)")
		pinnedCell = c.fs.String("pinned-cell", "", "target cell name (required for pinned strategy)")
	}
	if err := parseSettingsFlags(c, args[1:], yes); err != nil {
		return settingsError(err, 2)
	}
	var config client.PlacementConfig
	if verb == "set" {
		if *strategy != "weighted" && *strategy != "pinned" {
			return settingsError(fmt.Errorf("--strategy must be weighted or pinned"), 2)
		}
		if *strategy == "pinned" && !cellRegistryName.MatchString(*pinnedCell) {
			return settingsError(fmt.Errorf("--pinned-cell must be 1-64 lowercase letters, digits, or hyphens for pinned strategy"), 2)
		}
		if *strategy == "weighted" && *pinnedCell != "" {
			return settingsError(fmt.Errorf("--pinned-cell requires --strategy=pinned"), 2)
		}
		config = client.PlacementConfig{Strategy: *strategy, PinnedCell: *pinnedCell}
	}
	ep, tok, err := c.credentials()
	if err != nil {
		return settingsError(err, 2)
	}
	if verb == "show" {
		config, err = client.GetPlacement(context.Background(), ep, tok)
	} else {
		config, err = client.SetPlacement(context.Background(), ep, tok, config)
	}
	if err != nil {
		return settingsError(err, 1)
	}
	return printSettingsResponse(placementJSONMap(config), *c.json)
}

func settingsJSONMap(runner client.PlacementRunnerConfig, reaper client.ReaperConfig, placement client.PlacementConfig) map[string]any {
	return map[string]any{"schema_version": "witself.v0", "placement_runner": runner, "reaper": reaper, "placement": placement}
}

func placementRunnerJSONMap(config client.PlacementRunnerConfig) map[string]any {
	return map[string]any{"schema_version": "witself.v0", "placement_runner": config}
}

func reaperJSONMap(config client.ReaperConfig) map[string]any {
	return map[string]any{"schema_version": "witself.v0", "reaper": config}
}

func placementJSONMap(config client.PlacementConfig) map[string]any {
	return map[string]any{"schema_version": "witself.v0", "placement": config}
}

func placementRunnerResultJSONMap(result client.PlacementRunnerResult) map[string]any {
	out := placementRunnerJSONMap(result.PlacementRunner)
	if len(result.Restore) != 0 {
		out["restore"] = result.Restore
	}
	if len(result.Rebalance) != 0 {
		out["rebalance"] = result.Rebalance
	}
	if result.RestoreError != nil {
		out["restore_error"] = result.RestoreError
	}
	if result.RebalanceError != nil {
		out["rebalance_error"] = result.RebalanceError
	}
	return out
}

func printSettingsResponse(response map[string]any, jsonOutput bool) int {
	if jsonOutput {
		return printJSON(response)
	}
	w, flush := tableWriter("section\tsetting\tvalue")
	defer flush()
	for _, section := range []string{"placement_runner", "reaper", "placement", "restore", "rebalance", "restore_error", "rebalance_error"} {
		value, ok := response[section]
		if !ok {
			continue
		}
		body, err := json.Marshal(value)
		if err != nil {
			return settingsError(err, 1)
		}
		if section == "placement_runner" || section == "reaper" || section == "placement" {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(body, &fields); err != nil {
				return settingsError(err, 1)
			}
			keys := make([]string, 0, len(fields))
			for key := range fields {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", section, key, safeText(string(fields[key])))
			}
		} else {
			_, _ = fmt.Fprintf(w, "%s\tresult\t%s\n", section, safeText(string(body)))
		}
	}
	return 0
}
