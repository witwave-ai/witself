package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/id"
)

func emailAliasAdminCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr,
			"usage: witself-admin email-alias (requests|assignments|reserved|audit) ...")
		return 2
	}
	switch args[0] {
	case "requests":
		return emailAliasAdminRequests(args[1:])
	case "assignments":
		return emailAliasAdminAssignments(args[1:])
	case "reserved":
		return emailAliasAdminReserved(args[1:])
	case "audit":
		return emailAliasAdminAudit(args[1:])
	default:
		fmt.Fprintf(os.Stderr,
			"witself-admin email-alias: unknown subcommand %q\n", args[0])
		return 2
	}
}

type emailAliasAdminCommon struct {
	endpoint      *string
	token         *string
	tokenFile     *string
	idempotency   *string
	reason        *string
	json          *bool
	flagSet       *flag.FlagSet
	mutationLabel string
}

func newEmailAliasAdminCommon(name string, mutation bool) emailAliasAdminCommon {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	common := emailAliasAdminCommon{
		flagSet:       fs,
		endpoint:      fs.String("endpoint", "", "control-plane URL"),
		token:         fs.String("token", "", "admin token"),
		tokenFile:     fs.String("token-file", "", "file containing the admin token"),
		json:          jsonFlag(fs),
		mutationLabel: name,
	}
	if mutation {
		common.idempotency = fs.String("idempotency-key", "", "retry key (generated when omitted)")
		common.reason = fs.String("reason", "", "required audit reason")
	}
	return common
}

func (c emailAliasAdminCommon) credentials() (string, string, error) {
	token, err := resolveAdminToken(*c.token, *c.tokenFile)
	if err != nil {
		return "", "", err
	}
	return cpEndpoint(*c.endpoint), token, nil
}

func (c emailAliasAdminCommon) mutation() (string, string, error) {
	reason := strings.TrimSpace(*c.reason)
	if reason == "" {
		return "", "", fmt.Errorf("--reason is required")
	}
	key := strings.TrimSpace(*c.idempotency)
	if key == "" {
		var err error
		key, err = id.New("admin_email_alias")
		if err != nil {
			return "", "", fmt.Errorf("generate idempotency key: %w", err)
		}
	}
	return key, reason, nil
}

func emailAliasAdminRequests(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr,
			"usage: witself-admin email-alias requests (list|approve|reject) ...")
		return 2
	}
	action := args[0]
	switch action {
	case "list":
		common := newEmailAliasAdminCommon("email-alias requests list", false)
		status := common.flagSet.String("status", "", "filter by request status")
		accountID := common.flagSet.String("account", "", "filter by account id")
		realmID := common.flagSet.String("realm", "", "filter by realm id")
		cursor := common.flagSet.String("cursor", "", "continue from an opaque next-page cursor")
		if err := common.flagSet.Parse(args[1:]); err != nil || common.flagSet.NArg() != 0 {
			return 2
		}
		endpoint, token, err := common.credentials()
		if err != nil {
			return printEmailAliasAdminError(err, 2)
		}
		page, err := client.ListAdminRealmEmailAliasRequestsPage(context.Background(), endpoint, token,
			client.AdminRealmEmailAliasRequestFilter{
				Status: *status, AccountID: *accountID, RealmID: *realmID,
				Cursor: *cursor,
			})
		if err != nil {
			return printEmailAliasAdminError(err, 1)
		}
		if page.Requests == nil {
			page.Requests = []client.RealmEmailAliasRequest{}
		}
		if *common.json {
			return printJSON(page)
		}
		w, flush := tableWriter("id\talias\taccount\trealm\tstatus\tupdated")
		for _, request := range page.Requests {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
				emailAliasAdminColumn(request.ID),
				emailAliasAdminColumn(request.Alias),
				emailAliasAdminColumn(request.AccountID),
				emailAliasAdminColumn(request.RealmID),
				emailAliasAdminColumn(request.Status),
				emailAliasAdminTime(request.UpdatedAt))
		}
		flush()
		printEmailAliasAdminNextCursor(page.NextCursor)
		return 0
	case "approve", "reject":
		common := newEmailAliasAdminCommon("email-alias requests "+action, true)
		requestID := common.flagSet.String("request", "", "request id (required)")
		if err := common.flagSet.Parse(args[1:]); err != nil ||
			common.flagSet.NArg() != 0 || strings.TrimSpace(*requestID) == "" {
			fmt.Fprintf(os.Stderr,
				"usage: witself-admin email-alias requests %s --request REQUEST_ID --reason REASON\n", action)
			return 2
		}
		key, reason, err := common.mutation()
		if err != nil {
			return printEmailAliasAdminError(err, 2)
		}
		endpoint, token, err := common.credentials()
		if err != nil {
			return printEmailAliasAdminError(err, 2)
		}
		var request *client.RealmEmailAliasRequest
		var assignment *client.RealmEmailAliasAssignment
		if action == "approve" {
			result, approveErr := client.ApproveAdminRealmEmailAliasRequest(context.Background(), endpoint, token, *requestID, key, reason)
			err = approveErr
			if result != nil {
				request = &result.Request
				assignment = result.Assignment
			}
		} else {
			request, err = client.RejectAdminRealmEmailAliasRequest(context.Background(), endpoint, token, *requestID, key, reason)
		}
		if err != nil {
			return printEmailAliasAdminError(err, 1)
		}
		if *common.json {
			result := map[string]any{"request": request}
			if assignment != nil {
				result["assignment"] = assignment
			}
			return printJSON(result)
		}
		if assignment != nil {
			fmt.Printf("%s\t%s\t%s\t%s\t%s\n",
				emailAliasAdminColumn(request.ID),
				emailAliasAdminColumn(request.Alias),
				emailAliasAdminColumn(request.Status),
				emailAliasAdminColumn(assignment.ClaimID),
				emailAliasAdminColumn(assignment.Status))
		} else {
			fmt.Printf("%s\t%s\t%s\n", emailAliasAdminColumn(request.ID),
				emailAliasAdminColumn(request.Alias),
				emailAliasAdminColumn(request.Status))
		}
		return 0
	default:
		fmt.Fprintf(os.Stderr,
			"witself-admin email-alias requests: unknown action %q\n", action)
		return 2
	}
}

func emailAliasAdminAssignments(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr,
			"usage: witself-admin email-alias assignments (list|suspend|reactivate|retire|abort-provisioning|assign-internal) ...")
		return 2
	}
	action := args[0]
	if action == "list" {
		common := newEmailAliasAdminCommon("email-alias assignments list", false)
		status := common.flagSet.String("status", "", "filter by assignment status")
		accountID := common.flagSet.String("account", "", "filter by account id")
		realmID := common.flagSet.String("realm", "", "filter by realm id")
		cursor := common.flagSet.String("cursor", "", "continue from an opaque next-page cursor")
		if err := common.flagSet.Parse(args[1:]); err != nil || common.flagSet.NArg() != 0 {
			return 2
		}
		endpoint, token, err := common.credentials()
		if err != nil {
			return printEmailAliasAdminError(err, 2)
		}
		page, err := client.ListAdminRealmEmailAliasesPage(context.Background(), endpoint, token,
			client.AdminRealmEmailAliasFilter{
				Status: *status, AccountID: *accountID, RealmID: *realmID,
				Cursor: *cursor,
			})
		if err != nil {
			return printEmailAliasAdminError(err, 1)
		}
		if page.Aliases == nil {
			page.Aliases = []client.RealmEmailAliasAssignment{}
		}
		if *common.json {
			return printJSON(page)
		}
		w, flush := tableWriter("alias\taccount\trealm\tstatus\tinternal\tupdated")
		for _, alias := range page.Aliases {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%t\t%s\n",
				emailAliasAdminColumn(alias.Alias),
				emailAliasAdminColumn(alias.AccountID),
				emailAliasAdminColumn(alias.RealmID),
				emailAliasAdminColumn(alias.Status),
				alias.AssignmentKind == "internal",
				emailAliasAdminTime(alias.UpdatedAt))
		}
		flush()
		printEmailAliasAdminNextCursor(page.NextCursor)
		return 0
	}

	if action != "suspend" && action != "reactivate" && action != "retire" &&
		action != "abort-provisioning" && action != "assign-internal" {
		fmt.Fprintf(os.Stderr,
			"witself-admin email-alias assignments: unknown action %q\n", action)
		return 2
	}
	common := newEmailAliasAdminCommon("email-alias assignments "+action, true)
	alias := common.flagSet.String("alias", "", "alias label (required)")
	accountID := common.flagSet.String("account", "", "account id (assign-internal only)")
	realmID := common.flagSet.String("realm", "", "realm id (assign-internal only)")
	if err := common.flagSet.Parse(args[1:]); err != nil || common.flagSet.NArg() != 0 ||
		strings.TrimSpace(*alias) == "" {
		return 2
	}
	if action == "assign-internal" &&
		(strings.TrimSpace(*accountID) == "" || strings.TrimSpace(*realmID) == "") {
		fmt.Fprintln(os.Stderr,
			"witself-admin: assign-internal requires --account and --realm")
		return 2
	}
	if action != "assign-internal" &&
		(strings.TrimSpace(*accountID) != "" || strings.TrimSpace(*realmID) != "") {
		fmt.Fprintln(os.Stderr,
			"witself-admin: --account and --realm are only valid for assign-internal")
		return 2
	}
	key, reason, err := common.mutation()
	if err != nil {
		return printEmailAliasAdminError(err, 2)
	}
	endpoint, token, err := common.credentials()
	if err != nil {
		return printEmailAliasAdminError(err, 2)
	}
	var assignment *client.RealmEmailAliasAssignment
	switch action {
	case "suspend":
		assignment, err = client.SuspendAdminRealmEmailAlias(context.Background(), endpoint, token, *alias, key, reason)
	case "reactivate":
		assignment, err = client.ReactivateAdminRealmEmailAlias(context.Background(), endpoint, token, *alias, key, reason)
	case "retire":
		assignment, err = client.RetireAdminRealmEmailAlias(context.Background(), endpoint, token, *alias, key, reason)
	case "abort-provisioning":
		assignment, err = client.AbortAdminRealmEmailAliasProvisioning(
			context.Background(), endpoint, token, *alias, key, reason)
	case "assign-internal":
		assignment, err = client.AssignInternalAdminRealmEmailAlias(context.Background(), endpoint, token,
			*accountID, *realmID, *alias, key, reason)
	}
	if err != nil {
		return printEmailAliasAdminError(err, 1)
	}
	if *common.json {
		return printJSON(map[string]any{"alias": assignment})
	}
	fmt.Printf("%s\t%s\t%s\t%s\n", emailAliasAdminColumn(assignment.Alias),
		emailAliasAdminColumn(assignment.AccountID),
		emailAliasAdminColumn(assignment.RealmID),
		emailAliasAdminColumn(assignment.Status))
	return 0
}

func emailAliasAdminReserved(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr,
			"usage: witself-admin email-alias reserved (list|get|add|update|retire) ...")
		return 2
	}
	action := args[0]
	if action == "list" || action == "get" {
		common := newEmailAliasAdminCommon("email-alias reserved "+action, false)
		name := common.flagSet.String("name", "", "reserved name (required for get)")
		category := common.flagSet.String("category", "", "filter list by exact policy category")
		enabled := common.flagSet.String("enabled", "", "filter list by true or false")
		cursor := common.flagSet.String("cursor", "", "continue from an opaque next-page cursor")
		if err := common.flagSet.Parse(args[1:]); err != nil || common.flagSet.NArg() != 0 {
			return 2
		}
		if action == "get" && strings.TrimSpace(*name) == "" {
			fmt.Fprintln(os.Stderr, "witself-admin: get requires --name")
			return 2
		}
		if action == "list" && strings.TrimSpace(*name) != "" {
			fmt.Fprintln(os.Stderr, "witself-admin: list does not accept --name")
			return 2
		}
		if action == "get" && (strings.TrimSpace(*category) != "" ||
			strings.TrimSpace(*enabled) != "" || strings.TrimSpace(*cursor) != "") {
			fmt.Fprintln(os.Stderr,
				"witself-admin: --category, --enabled, and --cursor are only valid for list")
			return 2
		}
		enabledValue, err := emailAliasOptionalBool("--enabled", *enabled)
		if err != nil {
			return printEmailAliasAdminError(err, 2)
		}
		endpoint, token, err := common.credentials()
		if err != nil {
			return printEmailAliasAdminError(err, 2)
		}
		if action == "get" {
			reserved, err := client.GetAdminRealmEmailReservedName(context.Background(), endpoint, token, *name)
			if err != nil {
				return printEmailAliasAdminError(err, 1)
			}
			if *common.json {
				return printJSON(map[string]any{"reserved_name": reserved})
			}
			fmt.Printf("%s\t%s\t%t\t%s\n",
				emailAliasAdminColumn(reserved.Name),
				emailAliasAdminColumn(reserved.Category), reserved.Enabled,
				emailAliasAdminColumn(reserved.Reason))
			return 0
		}
		page, err := client.ListAdminRealmEmailReservedNamesPage(
			context.Background(), endpoint, token,
			client.AdminRealmEmailReservedNameFilter{
				Category: *category, Enabled: enabledValue, Cursor: *cursor,
			})
		if err != nil {
			return printEmailAliasAdminError(err, 1)
		}
		if page.ReservedNames == nil {
			page.ReservedNames = []client.RealmEmailReservedName{}
		}
		if *common.json {
			return printJSON(page)
		}
		w, flush := tableWriter("name\tskeleton\tcategory\tenabled\tinternal\tversion\treason")
		for _, name := range page.ReservedNames {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%t\t%t\t%d\t%s\n",
				emailAliasAdminColumn(name.Name),
				emailAliasAdminColumn(name.ConfusableSkeleton),
				emailAliasAdminColumn(name.Category), name.Enabled,
				name.InternalAssignable, name.Version,
				emailAliasAdminColumn(name.Reason))
		}
		flush()
		printEmailAliasAdminNextCursor(page.NextCursor)
		return 0
	}

	if action != "add" && action != "update" && action != "retire" {
		fmt.Fprintf(os.Stderr,
			"witself-admin email-alias reserved: unknown action %q\n", action)
		return 2
	}
	common := newEmailAliasAdminCommon("email-alias reserved "+action, true)
	name := common.flagSet.String("name", "", "reserved name (required)")
	category := common.flagSet.String("category", "", "policy category, such as platform_brand, operational_role, or infrastructure")
	enabled := common.flagSet.String("enabled", "", "true or false (update only)")
	internalAssignable := common.flagSet.String("internal-assignable", "", "true or false (default false for add; update only when supplied)")
	if err := common.flagSet.Parse(args[1:]); err != nil || common.flagSet.NArg() != 0 ||
		strings.TrimSpace(*name) == "" {
		return 2
	}
	if action == "add" && strings.TrimSpace(*category) == "" {
		fmt.Fprintln(os.Stderr, "witself-admin: add requires --category")
		return 2
	}
	if action == "retire" && strings.TrimSpace(*category) != "" {
		fmt.Fprintln(os.Stderr,
			"witself-admin: --category is only valid for add or update")
		return 2
	}
	if action != "update" && strings.TrimSpace(*enabled) != "" {
		fmt.Fprintln(os.Stderr, "witself-admin: --enabled is only valid for update")
		return 2
	}
	if action == "retire" && strings.TrimSpace(*internalAssignable) != "" {
		fmt.Fprintln(os.Stderr,
			"witself-admin: --internal-assignable is only valid for add or update")
		return 2
	}
	enabledValue, err := emailAliasOptionalBool("--enabled", *enabled)
	if err != nil {
		return printEmailAliasAdminError(err, 2)
	}
	internalAssignableValue, err := emailAliasOptionalBool(
		"--internal-assignable", *internalAssignable)
	if err != nil {
		return printEmailAliasAdminError(err, 2)
	}
	key, reason, err := common.mutation()
	if err != nil {
		return printEmailAliasAdminError(err, 2)
	}
	endpoint, token, err := common.credentials()
	if err != nil {
		return printEmailAliasAdminError(err, 2)
	}
	var reserved *client.RealmEmailReservedName
	switch action {
	case "add":
		assignable := false
		if internalAssignableValue != nil {
			assignable = *internalAssignableValue
		}
		reserved, err = client.CreateAdminRealmEmailReservedName(context.Background(), endpoint, token,
			*name, *category, reason, key, assignable)
	case "update":
		reserved, err = client.UpdateAdminRealmEmailReservedName(context.Background(), endpoint, token,
			*name, *category, reason, key, enabledValue, internalAssignableValue)
	case "retire":
		reserved, err = client.RetireAdminRealmEmailReservedName(context.Background(), endpoint, token,
			*name, reason, key)
	}
	if err != nil {
		return printEmailAliasAdminError(err, 1)
	}
	if *common.json {
		return printJSON(map[string]any{"reserved_name": reserved})
	}
	fmt.Printf("%s\t%s\t%t\t%s\n", emailAliasAdminColumn(reserved.Name),
		emailAliasAdminColumn(reserved.Category), reserved.Enabled,
		emailAliasAdminColumn(reserved.Reason))
	return 0
}

func emailAliasAdminAudit(args []string) int {
	common := newEmailAliasAdminCommon("email-alias audit", false)
	action := common.flagSet.String("action", "", "filter by exact audit action")
	limit := common.flagSet.Int("limit", 100, "newest underlying events to scan on this page (1-500)")
	cursor := common.flagSet.String("cursor", "", "continue from an opaque next-page cursor")
	if err := common.flagSet.Parse(args); err != nil || common.flagSet.NArg() != 0 {
		return 2
	}
	if *limit < 1 || *limit > 500 {
		fmt.Fprintln(os.Stderr, "witself-admin: --limit must be between 1 and 500")
		return 2
	}
	endpoint, token, err := common.credentials()
	if err != nil {
		return printEmailAliasAdminError(err, 2)
	}
	page, err := client.ListAdminRealmEmailAliasAuditPage(
		context.Background(), endpoint, token,
		client.AdminRealmEmailAliasAuditFilter{
			Action: *action, Limit: *limit, Cursor: *cursor,
		})
	if err != nil {
		return printEmailAliasAdminError(err, 1)
	}
	if page.Events == nil {
		page.Events = []client.RealmEmailAliasAuditEvent{}
	}
	if *common.json {
		return printJSON(page)
	}
	w, flush := tableWriter("sequence\ttime\taction\ttarget\tactor")
	for _, event := range page.Events {
		_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s:%s\n",
			event.Sequence, emailAliasAdminTime(event.OccurredAt),
			emailAliasAdminColumn(event.Action),
			emailAliasAdminColumn(event.Target),
			emailAliasAdminColumn(event.ActorKind),
			emailAliasAdminColumn(event.ActorID))
	}
	flush()
	printEmailAliasAdminNextCursor(page.NextCursor)
	return 0
}

func printEmailAliasAdminNextCursor(cursor string) {
	if strings.TrimSpace(cursor) != "" {
		fmt.Fprintf(os.Stderr, "next cursor: %s\n",
			emailAliasAdminColumn(cursor))
	}
}

func emailAliasAdminColumn(value string) string {
	return tabSafe(safeText(value))
}

func emailAliasOptionalBool(name, raw string) (*bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	value := strings.EqualFold(raw, "true")
	if !value && !strings.EqualFold(raw, "false") {
		return nil, fmt.Errorf("%s must be true or false", name)
	}
	return &value, nil
}

func printEmailAliasAdminError(err error, code int) int {
	fmt.Fprintf(os.Stderr, "witself-admin: %s\n",
		emailAliasAdminColumn(err.Error()))
	return code
}

func emailAliasAdminTime(value time.Time) string {
	if value.IsZero() {
		return "-"
	}
	return value.UTC().Format(time.RFC3339)
}
