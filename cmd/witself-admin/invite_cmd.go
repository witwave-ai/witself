package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/witwave-ai/witself/internal/client"
)

var inviteAdminCode = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{2,63}$`)

func inviteAdminCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: witself-admin invite list|show|create|disable|enable|delete ...")
		return 2
	}
	switch args[0] {
	case "list":
		return inviteAdminList(args[1:])
	case "show":
		return inviteAdminShow(args[1:])
	case "create":
		return inviteAdminCreate(args[1:])
	case "disable":
		return inviteAdminSetEnabled(args[1:], false)
	case "enable":
		return inviteAdminSetEnabled(args[1:], true)
	case "delete":
		return inviteAdminDelete(args[1:])
	default:
		// Do not echo an unrecognized value: operators can accidentally place a
		// semi-sensitive invite code in the subcommand position.
		fmt.Fprintln(os.Stderr, "witself-admin invite: unknown subcommand")
		return 2
	}
}

type inviteAdminCommon struct {
	flagSet    *flag.FlagSet
	endpoint   *string
	fleetToken *string
	json       *bool
}

func newInviteAdminCommon(name string) inviteAdminCommon {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return inviteAdminCommon{
		flagSet:    fs,
		endpoint:   fs.String("endpoint", "", "control-plane URL"),
		fleetToken: fs.String("fleet-token", "", "fleet shared secret"),
		json:       jsonFlag(fs),
	}
}

func (c inviteAdminCommon) credentials() (string, string, error) {
	token, err := resolveFleetToken(*c.fleetToken)
	if err != nil {
		return "", "", err
	}
	return cpEndpoint(*c.endpoint), token, nil
}

func inviteAdminList(args []string) int {
	common := newInviteAdminCommon("invite list")
	if err := common.flagSet.Parse(args); err != nil || common.flagSet.NArg() != 0 {
		return 2
	}
	endpoint, token, err := common.credentials()
	if err != nil {
		return printInviteAdminError(err, 2)
	}
	result, err := client.ListAdminInvites(context.Background(), endpoint, token)
	if err != nil {
		return printInviteAdminError(err, 1)
	}
	if result.Invites == nil {
		result.Invites = []client.AdminInvite{}
	}
	if *common.json {
		return printJSON(result)
	}
	w, flush := tableWriter("code\tenabled\tuses/max\twindow\tcell/region\tnote")
	for _, invite := range result.Invites {
		printInviteAdminListRow(w, invite)
	}
	flush()
	return 0
}

func inviteAdminShow(args []string) int {
	code, common, ok := parseInviteAdminCodeArgs("show", args)
	if !ok {
		return 2
	}
	endpoint, token, err := common.credentials()
	if err != nil {
		return printInviteAdminError(err, 2)
	}
	result, err := client.GetAdminInvite(context.Background(), endpoint, token, code)
	if err != nil {
		return printInviteAdminError(err, 1)
	}
	if *common.json {
		return printJSON(result)
	}
	printInviteAdminDetail(result.Invite)
	return 0
}

func inviteAdminCreate(args []string) int {
	common := newInviteAdminCommon("invite create")
	code := common.flagSet.String("code", "", "invite code (generated when omitted)")
	maxUses := common.flagSet.Int64("max-uses", 0, "maximum successful uses")
	notBefore := common.flagSet.String("not-before", "", "ISO-8601 validity start")
	expiresAt := common.flagSet.String("expires", "", "ISO-8601 expiry")
	cell := common.flagSet.String("cell", "", "hard-pinned cell")
	region := common.flagSet.String("region", "", "required placement region")
	note := common.flagSet.String("note", "", "operator note")
	if err := common.flagSet.Parse(args); err != nil || common.flagSet.NArg() != 0 {
		return 2
	}
	set := inviteAdminSetFlags(common.flagSet)
	if set["code"] && !inviteAdminCode.MatchString(*code) {
		return printInviteAdminError(fmt.Errorf("invalid invite code"), 2)
	}
	if set["max-uses"] && *maxUses < 1 {
		return printInviteAdminError(fmt.Errorf("--max-uses must be a positive integer"), 2)
	}
	input := client.AdminInviteInput{}
	if set["code"] {
		input.Code = code
	}
	if set["max-uses"] {
		input.MaxUses = maxUses
	}
	if set["not-before"] {
		input.NotBefore = notBefore
	}
	if set["expires"] {
		input.ExpiresAt = expiresAt
	}
	if set["cell"] {
		input.Cell = cell
	}
	if set["region"] {
		input.Region = region
	}
	if set["note"] {
		input.Note = note
	}
	endpoint, token, err := common.credentials()
	if err != nil {
		return printInviteAdminError(err, 2)
	}
	result, err := client.CreateAdminInvite(context.Background(), endpoint, token, input)
	if err != nil {
		return printInviteAdminError(err, 1)
	}
	if *common.json {
		return printJSON(result)
	}
	printInviteAdminDetail(result.Invite)
	return 0
}

func inviteAdminSetEnabled(args []string, enabled bool) int {
	action := "disable"
	if enabled {
		action = "enable"
	}
	code, common, ok := parseInviteAdminCodeArgs(action, args)
	if !ok {
		return 2
	}
	endpoint, token, err := common.credentials()
	if err != nil {
		return printInviteAdminError(err, 2)
	}
	result, err := client.SetAdminInviteEnabled(
		context.Background(), endpoint, token, code, enabled,
	)
	if err != nil {
		return printInviteAdminError(err, 1)
	}
	if *common.json {
		return printJSON(result)
	}
	printInviteAdminDetail(result.Invite)
	return 0
}

func inviteAdminDelete(args []string) int {
	code, common, ok := parseInviteAdminCodeArgs("delete", args)
	if !ok {
		return 2
	}
	endpoint, token, err := common.credentials()
	if err != nil {
		return printInviteAdminError(err, 2)
	}
	result, err := client.DeleteAdminInvite(context.Background(), endpoint, token, code)
	if err != nil {
		return printInviteAdminError(err, 1)
	}
	if *common.json {
		return printJSON(result)
	}
	fmt.Printf("deleted\t%t\n", result.Deleted)
	return 0
}

func parseInviteAdminCodeArgs(
	action string,
	args []string,
) (string, inviteAdminCommon, bool) {
	common := newInviteAdminCommon("invite " + action)
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "usage: witself-admin invite %s CODE\n", action)
		return "", common, false
	}
	// Accept both the documented CODE-first form and conventional flags-first
	// form. The standard flag package does not intersperse flags and positional
	// arguments, so each form is parsed from the side that contains its flags.
	if inviteAdminCode.MatchString(args[0]) {
		if err := common.flagSet.Parse(args[1:]); err != nil || common.flagSet.NArg() != 0 {
			return "", common, false
		}
		return args[0], common, true
	}
	if err := common.flagSet.Parse(args); err != nil || common.flagSet.NArg() != 1 {
		return "", common, false
	}
	code := common.flagSet.Arg(0)
	if !inviteAdminCode.MatchString(code) {
		fmt.Fprintf(os.Stderr, "usage: witself-admin invite %s CODE\n", action)
		return "", common, false
	}
	return code, common, true
}

func inviteAdminSetFlags(fs *flag.FlagSet) map[string]bool {
	set := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) {
		set[f.Name] = true
	})
	return set
}

func printInviteAdminListRow(w interface{ Write([]byte) (int, error) }, invite client.AdminInvite) {
	_, _ = fmt.Fprintf(w, "%s\t%t\t%s\t%s\t%s\t%s\n",
		inviteAdminColumn(invite.Code),
		invite.Enabled,
		inviteAdminUses(invite),
		inviteAdminWindow(invite),
		inviteAdminPlacement(invite),
		inviteAdminColumn(invite.Note))
}

func printInviteAdminDetail(invite client.AdminInvite) {
	w, flush := tableWriter(
		"code\tenabled\tuses/max\twindow\tcell/region\tvalid\texhausted\texpired\tnot-yet-valid\treason\tcreated-at\tnote",
	)
	_, _ = fmt.Fprintf(w, "%s\t%t\t%s\t%s\t%s\t%t\t%t\t%t\t%t\t%s\t%s\t%s\n",
		inviteAdminColumn(invite.Code),
		invite.Enabled,
		inviteAdminUses(invite),
		inviteAdminWindow(invite),
		inviteAdminPlacement(invite),
		invite.Valid,
		invite.Exhausted,
		invite.Expired,
		invite.NotYetValid,
		inviteAdminColumn(invite.Reason),
		inviteAdminColumn(invite.CreatedAt),
		inviteAdminColumn(invite.Note))
	flush()
}

func inviteAdminUses(invite client.AdminInvite) string {
	maximum := "unlimited"
	if invite.MaxUses != nil {
		maximum = strconv.FormatInt(*invite.MaxUses, 10)
	}
	return fmt.Sprintf("%d/%s", invite.Uses, maximum)
}

func inviteAdminWindow(invite client.AdminInvite) string {
	if invite.NotBefore == nil && invite.ExpiresAt == nil {
		return "always"
	}
	start, end := "-", "-"
	if invite.NotBefore != nil {
		start = *invite.NotBefore
	}
	if invite.ExpiresAt != nil {
		end = *invite.ExpiresAt
	}
	return inviteAdminColumn(start + ".." + end)
}

func inviteAdminPlacement(invite client.AdminInvite) string {
	if invite.Cell == nil && invite.Region == nil {
		return "-"
	}
	cell, region := "-", "-"
	if invite.Cell != nil {
		cell = *invite.Cell
	}
	if invite.Region != nil {
		region = *invite.Region
	}
	return inviteAdminColumn(cell + "/" + region)
}

func inviteAdminColumn(value string) string {
	return tabSafe(safeText(strings.TrimSpace(value)))
}

func printInviteAdminError(err error, code int) int {
	fmt.Fprintf(os.Stderr, "witself-admin: %s\n", inviteAdminColumn(err.Error()))
	return code
}
