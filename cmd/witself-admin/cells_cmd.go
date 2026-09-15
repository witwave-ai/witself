package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/charmbracelet/x/term"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/cliout"
	"github.com/witwave-ai/witself/internal/tokenfile"
)

var cellRegistryName = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)

func cellsUsage(w io.Writer) {
	cliout.Line(w, "usage: witself-admin cells list|show|register|deregister|drain|undrain ...")
	cliout.Line(w, "  list                         Admin-token fleet view with account counts")
	cliout.Line(w, "  show NAME                    Inspect one registry entry (fleet token)")
	cliout.Line(w, "  register NAME --cell-endpoint HTTPS_URL [flags]  Upsert a registry entry")
	cliout.Line(w, "  drain NAME                   Stop new placements; existing accounts remain")
	cliout.Line(w, "  undrain NAME                 Resume placements (except backup validation targets)")
	cliout.Line(w, "  deregister NAME --yes [--yes-cell NAME]  Remove a drained, account-free entry")
	cliout.Line(w, "Repair flags: --endpoint URL, --fleet-token TOKEN (or --token / --token-file), --json")
	cliout.Line(w, "Deregister requires exact-name confirmation; scripts need --yes --yes-cell NAME.")
	cliout.Line(w, "Deregister uses safe DELETE; the control plane refuses cells with accounts or new placements enabled.")
}

type cellRegistryFlags struct {
	fs         *flag.FlagSet
	endpoint   *string
	fleetToken *string
	token      *string
	tokenFile  *string
	json       *bool
}

func newCellRegistryFlags(verb string) cellRegistryFlags {
	fs := flag.NewFlagSet("cells "+verb, flag.ContinueOnError)
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

// parse accepts NAME first or last, just like invite show/delete.
func (c cellRegistryFlags) parse(args []string) (string, error) {
	name := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") && cellRegistryName.MatchString(args[0]) {
		name, args = args[0], args[1:]
	}
	if err := c.fs.Parse(args); err != nil {
		return "", err
	}
	if name == "" && c.fs.NArg() == 1 {
		name = c.fs.Arg(0)
	} else if c.fs.NArg() != 0 {
		return "", fmt.Errorf("expected one cell name, with flags before or after it")
	}
	if !cellRegistryName.MatchString(name) {
		return "", fmt.Errorf("expected a cell name of 1-64 lowercase letters, digits, or hyphens")
	}
	return name, nil
}

func (c cellRegistryFlags) credentials() (string, string, error) {
	if strings.TrimSpace(*c.fleetToken) != "" && strings.TrimSpace(*c.token) != "" {
		return "", "", fmt.Errorf("use only one of --fleet-token and --token")
	}
	explicit := *c.fleetToken
	if strings.TrimSpace(explicit) == "" {
		explicit = *c.token
	}
	var tok string
	var err error
	if strings.TrimSpace(explicit) == "" && strings.TrimSpace(*c.tokenFile) != "" {
		tok, err = tokenfile.Read(*c.tokenFile, tokenfile.Options{Description: "fleet token file"})
	} else {
		tok, err = resolveFleetToken(explicit)
	}
	return cpEndpoint(*c.endpoint), tok, err
}

func cellRegistryError(err error, code int) int {
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	fmt.Fprintf(os.Stderr, "witself-admin cells: %v\n", err)
	return code
}

func cellsShow(args []string) int {
	c := newCellRegistryFlags("show")
	name, err := c.parse(args)
	if err != nil {
		return cellRegistryError(err, 2)
	}
	ep, tok, err := c.credentials()
	if err != nil {
		return cellRegistryError(err, 2)
	}
	cell, err := client.GetFleetCell(context.Background(), ep, tok, name)
	if err != nil {
		return cellRegistryError(err, 1)
	}
	return printFleetCell(&client.FleetCellResult{SchemaVersion: "witself.v0", Cell: *cell}, *c.json)
}

func cellsRegister(args []string) int {
	c := newCellRegistryFlags("register")
	cellEndpoint := c.fs.String("cell-endpoint", "", "cell HTTPS URL (required; --endpoint selects the control plane)")
	cloud := c.fs.String("cloud", "", "cloud provider")
	region := c.fs.String("region", "", "provider region")
	regionCode := c.fs.String("region-code", "", "canonical placement region code")
	channel := c.fs.String("channel", "", "placement channel: stable, edge, or experimental")
	weight := c.fs.Float64("weight", 1, "placement weight")
	accepting := c.fs.Bool("accepting", false, "accept new placements (default: register drained; use --accepting=true to enable)")
	backupValidation := c.fs.Bool("backup-validation-target", false, "placement-ineligible backup validation target; requires --accepting=false")
	provisionFile := c.fs.String("provision-token-file", "", "cell provisioning credential file; omitted preserves existing credential")
	backupFile := c.fs.String("backup-token-file", "", "cell backup credential file; omitted preserves existing credential")
	name, err := c.parse(args)
	if err != nil {
		return cellRegistryError(err, 2)
	}
	u, err := url.Parse(*cellEndpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || strings.ContainsAny(*cellEndpoint, "?#") {
		return cellRegistryError(fmt.Errorf("--cell-endpoint must be an HTTPS URL without credentials, query, or fragment"), 2)
	}
	if math.IsNaN(*weight) || math.IsInf(*weight, 0) || *weight <= 0 {
		return cellRegistryError(fmt.Errorf("--weight must be finite and positive"), 2)
	}
	if *channel != "" && *channel != "stable" && *channel != "edge" && *channel != "experimental" {
		return cellRegistryError(fmt.Errorf("--channel must be stable, edge, or experimental"), 2)
	}
	if *backupValidation && *accepting {
		return cellRegistryError(fmt.Errorf("--backup-validation-target requires --accepting=false"), 2)
	}
	cell := client.FleetCell{
		Name: name, Endpoint: strings.TrimRight(*cellEndpoint, "/"), Cloud: *cloud,
		Region: *region, RegionCode: *regionCode, Channel: *channel, Weight: *weight,
		Accepting: accepting, BackupValidationTarget: *backupValidation,
	}
	for _, credential := range []struct {
		path  string
		value *string
	}{
		{*provisionFile, &cell.ProvisionToken},
		{*backupFile, &cell.BackupToken},
	} {
		if credential.path != "" {
			*credential.value, err = tokenfile.Read(credential.path, tokenfile.Options{Description: "cell credential file"})
			if err != nil {
				return cellRegistryError(err, 2)
			}
		}
	}
	ep, tok, err := c.credentials()
	if err != nil {
		return cellRegistryError(err, 2)
	}
	res, err := client.RegisterFleetCell(context.Background(), ep, tok, cell)
	if err != nil {
		return cellRegistryError(err, 1)
	}
	return printFleetCell(res, *c.json)
}

func cellsSetAccepting(args []string, accepting bool) int {
	verb := "drain"
	if accepting {
		verb = "undrain"
	}
	c := newCellRegistryFlags(verb)
	name, err := c.parse(args)
	if err != nil {
		return cellRegistryError(err, 2)
	}
	ep, tok, err := c.credentials()
	if err != nil {
		return cellRegistryError(err, 2)
	}
	res, err := client.SetFleetCellAccepting(context.Background(), ep, tok, name, accepting)
	if err != nil {
		return cellRegistryError(err, 1)
	}
	return printFleetCell(res, *c.json)
}

func cellsDeregister(args []string) int {
	c := newCellRegistryFlags("deregister")
	yes := c.fs.Bool("yes", false, "confirm destructive registry removal (also requires exact cell-name confirmation)")
	yesCell := c.fs.String("yes-cell", "", "exact target cell name for noninteractive confirmation")
	name, err := c.parse(args)
	if err != nil {
		return cellRegistryError(err, 2)
	}
	if !*yes {
		return cellRegistryError(fmt.Errorf("deregister requires --yes and exact cell-name confirmation"), 2)
	}
	ep, tok, err := c.credentials()
	if err != nil {
		return cellRegistryError(err, 2)
	}
	if err := confirmCellDeregister(name, *yesCell, term.IsTerminal(os.Stdin.Fd()), os.Stdin, os.Stderr); err != nil {
		return cellRegistryError(err, 2)
	}
	// The server's coordinated DELETE is the authoritative account/drain guard;
	// preserve its refusal rather than relying on a potentially stale preflight.
	if err := client.DeleteFleetCell(context.Background(), ep, tok, name); err != nil {
		return cellRegistryError(err, 1)
	}
	if *c.json {
		return printJSON(map[string]any{"schema_version": "witself.v0", "name": name, "deleted": true})
	}
	fmt.Printf("deregistered\t%s\n", name)
	return 0
}

func confirmCellDeregister(name, yesCell string, interactive bool, in io.Reader, out io.Writer) error {
	if yesCell != "" {
		if yesCell != name {
			return fmt.Errorf("--yes-cell must exactly match target cell %q", name)
		}
		return nil
	}
	if !interactive {
		return fmt.Errorf("noninteractive deregister requires --yes --yes-cell=%s", name)
	}
	_, _ = fmt.Fprintf(out, "Deregister target %q. Type the exact cell name to confirm: ", name)
	if in == nil {
		return fmt.Errorf("deregister confirmation failed: no input is available")
	}
	scanner := bufio.NewScanner(in)
	if !scanner.Scan() {
		return fmt.Errorf("deregister confirmation failed: no cell name was read")
	}
	if scanner.Text() != name {
		return fmt.Errorf("deregister confirmation mismatch: typed name must exactly match %q", name)
	}
	return nil
}

func printFleetCell(res *client.FleetCellResult, jsonOut bool) int {
	// Registry responses should already redact these. Never render a credential
	// even if a stale or misconfigured control plane echoes registration input.
	res.Cell.ProvisionToken, res.Cell.BackupToken = "", ""
	if jsonOut {
		return printJSON(res)
	}
	c := res.Cell
	accepting := "unknown"
	if c.Accepting != nil {
		accepting = fmt.Sprint(*c.Accepting)
	}
	w, flush := tableWriter("cell\tendpoint\tcloud\tregion\tregion-code\tchannel\tweight\taccepting\tbackup-validation-target\thas-provision-token\thas-backup-token")
	_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%g\t%s\t%t\t%t\t%t\n",
		tabSafe(c.Name), tabSafe(c.Endpoint), tabSafe(c.Cloud), tabSafe(c.Region), tabSafe(c.RegionCode),
		tabSafe(c.Channel), c.Weight, accepting, c.BackupValidationTarget, c.HasProvisionToken, c.HasBackupToken)
	flush()
	return 0
}
