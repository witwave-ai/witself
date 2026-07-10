package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/witwave-ai/witself/internal/client"
)

func placementCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: witself-admin placement rescue --account-id ID [--axes cloud,region,channel]")
		return 2
	}
	switch args[0] {
	case "rescue":
		return placementRescueCmd(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "witself-admin placement: unknown subcommand %q\n", args[0])
		return 2
	}
}

func placementRescueCmd(args []string) int {
	fs := flag.NewFlagSet("placement rescue", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	endpoint := fs.String("endpoint", "", "control-plane URL")
	fleetToken := fs.String("fleet-token", "", "fleet token")
	accountID := fs.String("account-id", "", "archived account ID")
	axesCSV := fs.String("axes", "cloud,region,channel", "hard-pin axes to clear")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || strings.TrimSpace(*accountID) == "" {
		fmt.Fprintln(os.Stderr, "usage: witself-admin placement rescue --account-id ID [--axes cloud,region,channel]")
		return 2
	}
	axes := commaList(*axesCSV)
	if len(axes) == 0 {
		fmt.Fprintln(os.Stderr, "witself-admin: --axes must include cloud, region, or channel")
		return 2
	}
	for _, axis := range axes {
		if axis != "cloud" && axis != "region" && axis != "channel" {
			fmt.Fprintf(os.Stderr, "witself-admin: unknown placement axis %q\n", axis)
			return 2
		}
	}
	token, err := resolveFleetToken(*fleetToken)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself-admin: %v\n", err)
		return 2
	}
	res, err := client.RescueArchivedPlacement(
		context.Background(),
		cpEndpoint(*endpoint),
		token,
		strings.TrimSpace(*accountID),
		axes,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself-admin: %v\n", err)
		return 1
	}
	if *jsonOut {
		return printJSON(placementRescueJSONMap(res))
	}
	state := "already clear"
	if res.Changed {
		state = "cleared"
	}
	fmt.Printf("%s\t%s\t%s\n", safeText(res.AccountID), state, strings.Join(res.ClearedAxes, ","))
	return 0
}

func placementRescueJSONMap(res *client.ArchivedPlacementRescue) map[string]any {
	return map[string]any{"placement_rescue": res}
}

func commaList(raw string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, item := range strings.Split(raw, ",") {
		item = strings.ToLower(strings.TrimSpace(item))
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	return out
}
