package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/id"
)

func emailDomainAdminCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr,
			"usage: witself-admin email-domain (requests|audit) ...")
		return 2
	}
	switch args[0] {
	case "requests":
		return emailDomainAdminRequests(args[1:])
	case "audit":
		return emailDomainAdminAudit(args[1:])
	default:
		fmt.Fprintf(os.Stderr,
			"witself-admin email-domain: unknown subcommand %q\n", args[0])
		return 2
	}
}

type emailDomainAdminCommon struct {
	endpoint    *string
	token       *string
	tokenFile   *string
	idempotency *string
	reason      *string
	json        *bool
	flagSet     *flag.FlagSet
}

func newEmailDomainAdminCommon(name string, mutation bool) emailDomainAdminCommon {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	common := emailDomainAdminCommon{
		flagSet:   fs,
		endpoint:  fs.String("endpoint", "", "control-plane URL"),
		token:     fs.String("token", "", "admin token"),
		tokenFile: fs.String("token-file", "", "file containing the admin token"),
		json:      jsonFlag(fs),
	}
	if mutation {
		common.idempotency = fs.String("idempotency-key", "",
			"retry key (generated when omitted)")
		common.reason = fs.String("reason", "", "required audit reason")
	}
	return common
}

func (c emailDomainAdminCommon) credentials() (string, string, error) {
	token, err := resolveAdminToken(*c.token, *c.tokenFile)
	if err != nil {
		return "", "", err
	}
	return cpEndpoint(*c.endpoint), token, nil
}

func (c emailDomainAdminCommon) mutation() (string, string, error) {
	reason := strings.TrimSpace(*c.reason)
	if reason == "" {
		return "", "", fmt.Errorf("--reason is required")
	}
	key := strings.TrimSpace(*c.idempotency)
	if key == "" {
		var err error
		key, err = id.New("admin_email_domain")
		if err != nil {
			return "", "", fmt.Errorf("generate idempotency key: %w", err)
		}
	}
	return key, reason, nil
}

func emailDomainAdminRequests(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr,
			"usage: witself-admin email-domain requests (list|show|reject|retire) ...")
		return 2
	}
	action := args[0]
	switch action {
	case "list":
		common := newEmailDomainAdminCommon("email-domain requests list", false)
		state := common.flagSet.String("state", "", "filter by request state")
		accountID := common.flagSet.String("account", "", "filter by account id")
		domain := common.flagSet.String("domain", "", "filter by exact domain")
		cursor := common.flagSet.String("cursor", "", "continue from an opaque next-page cursor")
		if err := common.flagSet.Parse(args[1:]); err != nil || common.flagSet.NArg() != 0 {
			return 2
		}
		endpoint, token, err := common.credentials()
		if err != nil {
			return printEmailDomainAdminError(err, 2)
		}
		page, err := client.ListAdminAgentEmailDomainRequestsPage(
			context.Background(), endpoint, token,
			client.AdminAgentEmailDomainRequestFilter{
				State: *state, AccountID: *accountID, Domain: *domain, Cursor: *cursor,
			})
		if err != nil {
			return printEmailDomainAdminError(err, 1)
		}
		if page.Requests == nil {
			page.Requests = []client.AgentEmailDomainRequest{}
		}
		if *common.json {
			return printJSON(page)
		}
		w, flush := tableWriter("id\taccount\tdomain\tstate\tupdated")
		for i := range page.Requests {
			request := &page.Requests[i]
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
				emailDomainAdminColumn(request.RequestID),
				emailDomainAdminColumn(request.AccountID),
				emailDomainAdminColumn(request.Domain),
				emailDomainAdminColumn(request.State),
				emailDomainAdminOptionalTime(request.UpdatedAt))
		}
		flush()
		printEmailDomainAdminNextCursor(page.NextCursor)
		return 0

	case "show":
		common := newEmailDomainAdminCommon("email-domain requests show", false)
		requestID := common.flagSet.String("request", "", "request id (required)")
		if err := common.flagSet.Parse(args[1:]); err != nil ||
			common.flagSet.NArg() != 0 || strings.TrimSpace(*requestID) == "" {
			fmt.Fprintln(os.Stderr,
				"usage: witself-admin email-domain requests show --request REQUEST_ID")
			return 2
		}
		endpoint, token, err := common.credentials()
		if err != nil {
			return printEmailDomainAdminError(err, 2)
		}
		request, err := client.GetAdminAgentEmailDomainRequest(
			context.Background(), endpoint, token, *requestID)
		if err != nil {
			return printEmailDomainAdminError(err, 1)
		}
		return printEmailDomainAdminRequest(request, *common.json, true)

	case "reject", "retire":
		common := newEmailDomainAdminCommon("email-domain requests "+action, true)
		requestID := common.flagSet.String("request", "", "request id (required)")
		if err := common.flagSet.Parse(args[1:]); err != nil ||
			common.flagSet.NArg() != 0 || strings.TrimSpace(*requestID) == "" {
			fmt.Fprintf(os.Stderr,
				"usage: witself-admin email-domain requests %s --request REQUEST_ID --reason REASON\n",
				action)
			return 2
		}
		key, reason, err := common.mutation()
		if err != nil {
			return printEmailDomainAdminError(err, 2)
		}
		endpoint, token, err := common.credentials()
		if err != nil {
			return printEmailDomainAdminError(err, 2)
		}
		var request *client.AgentEmailDomainRequest
		if action == "reject" {
			request, err = client.RejectAdminAgentEmailDomainRequest(
				context.Background(), endpoint, token, *requestID, key, reason)
		} else {
			request, err = client.RetireAdminAgentEmailDomainRequest(
				context.Background(), endpoint, token, *requestID, key, reason)
		}
		if err != nil {
			return printEmailDomainAdminError(err, 1)
		}
		return printEmailDomainAdminRequest(request, *common.json, false)

	default:
		fmt.Fprintf(os.Stderr,
			"witself-admin email-domain requests: unknown action %q\n", action)
		return 2
	}
}

func emailDomainAdminAudit(args []string) int {
	common := newEmailDomainAdminCommon("email-domain audit", false)
	action := common.flagSet.String("action", "", "filter by audit action")
	accountID := common.flagSet.String("account", "", "filter by account id")
	domain := common.flagSet.String("domain", "", "filter by exact domain")
	cursor := common.flagSet.String("cursor", "", "continue from an opaque next-page cursor")
	limit := common.flagSet.Int("limit", 100, "underlying audit rows to scan (1-100)")
	if err := common.flagSet.Parse(args); err != nil || common.flagSet.NArg() != 0 {
		return 2
	}
	if *limit < 1 || *limit > 100 {
		fmt.Fprintln(os.Stderr, "witself-admin: --limit must be between 1 and 100")
		return 2
	}
	endpoint, token, err := common.credentials()
	if err != nil {
		return printEmailDomainAdminError(err, 2)
	}
	page, err := client.ListAdminAgentEmailDomainAuditPage(
		context.Background(), endpoint, token,
		client.AdminAgentEmailDomainAuditFilter{
			Action: *action, AccountID: *accountID, Domain: *domain,
			Limit: *limit, Cursor: *cursor,
		})
	if err != nil {
		return printEmailDomainAdminError(err, 1)
	}
	if page.Events == nil {
		page.Events = []client.AgentEmailDomainAuditEvent{}
	}
	if *common.json {
		return printJSON(page)
	}
	w, flush := tableWriter("sequence\ttime\taction\ttarget\taccount\tdomain\tactor")
	for i := range page.Events {
		event := &page.Events[i]
		accountID, domain := emailDomainAdminAuditScope(event)
		_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s:%s\n",
			event.Sequence, emailDomainAdminTime(event.OccurredAt),
			emailDomainAdminColumn(event.Action),
			emailDomainAdminColumn(event.Target),
			emailDomainAdminColumn(accountID),
			emailDomainAdminColumn(domain),
			emailDomainAdminColumn(event.ActorKind),
			emailDomainAdminColumn(event.ActorID))
	}
	flush()
	printEmailDomainAdminNextCursor(page.NextCursor)
	return 0
}

func emailDomainAdminAuditScope(event *client.AgentEmailDomainAuditEvent) (string, string) {
	var accountID, domain string
	if len(event.Metadata) != 0 {
		var metadata struct {
			AccountID string `json:"account_id"`
			Domain    string `json:"domain"`
		}
		if json.Unmarshal(event.Metadata, &metadata) == nil {
			if accountID == "" {
				accountID = metadata.AccountID
			}
			if domain == "" {
				domain = metadata.Domain
			}
		}
	}
	if domain == "" && strings.Contains(event.Target, ".") {
		domain = event.Target
	}
	return accountID, domain
}

func printEmailDomainAdminRequest(
	request *client.AgentEmailDomainRequest, jsonOut, includeChallenge bool,
) int {
	if jsonOut {
		return printJSON(map[string]any{"request": request})
	}
	if includeChallenge {
		var recordName, recordType, recordValue string
		if request.OwnershipChallenge != nil {
			recordName = request.OwnershipChallenge.RecordName
			recordType = request.OwnershipChallenge.RecordType
			recordValue = request.OwnershipChallenge.RecordValue
		}
		w, flush := tableWriter(
			"id\taccount\tdomain\tstate\trecord_name\trecord_type\trecord_value\tupdated")
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			emailDomainAdminColumn(request.RequestID),
			emailDomainAdminColumn(request.AccountID),
			emailDomainAdminColumn(request.Domain),
			emailDomainAdminColumn(request.State),
			emailDomainAdminColumn(recordName),
			emailDomainAdminColumn(recordType),
			emailDomainAdminColumn(recordValue),
			emailDomainAdminOptionalTime(request.UpdatedAt))
		flush()
		return 0
	}
	fmt.Printf("%s\t%s\t%s\t%s\n",
		emailDomainAdminColumn(request.RequestID),
		emailDomainAdminColumn(request.AccountID),
		emailDomainAdminColumn(request.Domain),
		emailDomainAdminColumn(request.State))
	return 0
}

func emailDomainAdminColumn(value string) string {
	return tabSafe(safeText(value))
}

func printEmailDomainAdminNextCursor(cursor string) {
	if cursor = strings.TrimSpace(cursor); cursor != "" {
		fmt.Fprintf(os.Stderr, "next cursor: %s\n", emailDomainAdminColumn(cursor))
	}
}

func printEmailDomainAdminError(err error, code int) int {
	fmt.Fprintf(os.Stderr, "witself-admin: %v\n", err)
	return code
}

func emailDomainAdminTime(value time.Time) string {
	if value.IsZero() {
		return "-"
	}
	return value.UTC().Format(time.RFC3339)
}

func emailDomainAdminOptionalTime(value *time.Time) string {
	if value == nil {
		return "-"
	}
	return emailDomainAdminTime(*value)
}
