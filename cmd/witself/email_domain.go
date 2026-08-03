package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/id"
)

func emailDomainCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: witself email-domain (request|list) ...")
		return 2
	}
	switch args[0] {
	case "request":
		return emailDomainRequest(args[1:])
	case "list":
		return emailDomainList(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "witself email-domain: unknown action %q\n", args[0])
		return 2
	}
}

func emailDomainFlags(
	name string,
) (*flag.FlagSet, *string, *string, *string, *string, *bool) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account := accountFlag(fs)
	accountID := fs.String("account-id", "", "account id (only with --token-file)")
	endpoint := fs.String("endpoint", defaultControlPlane, "control-plane URL")
	tokenFile := fs.String("token-file", "", "file containing an operator token")
	jsonOut := jsonFlag(fs)
	return fs, account, accountID, endpoint, tokenFile, jsonOut
}

func emailDomainRequest(args []string) int {
	fs, account, accountID, endpoint, tokenFile, jsonOut :=
		emailDomainFlags("email-domain request")
	domain := fs.String("domain", "", "organization-owned inbound email domain (required)")
	idempotencyKey := fs.String("idempotency-key", "", "retry key (generated when omitted)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || strings.TrimSpace(*domain) == "" {
		fmt.Fprintln(os.Stderr,
			"usage: witself email-domain request --domain DOMAIN [--account NAME]")
		return 2
	}
	resolvedAccountID, token, err := resolveControlPlaneAccountOperator(
		*account, *accountID, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	key := strings.TrimSpace(*idempotencyKey)
	if key == "" {
		key, err = id.New("email_domain_request")
		if err != nil {
			fmt.Fprintf(os.Stderr, "witself: generate idempotency key: %v\n", err)
			return 1
		}
	}
	request, err := client.RequestAgentEmailDomain(
		context.Background(), strings.TrimSpace(*endpoint), token,
		resolvedAccountID, strings.TrimSpace(*domain), key)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	if *jsonOut {
		return printJSON(map[string]any{"request": request})
	}
	var recordName, recordType, recordValue string
	if request.OwnershipChallenge != nil {
		recordName = request.OwnershipChallenge.RecordName
		recordType = request.OwnershipChallenge.RecordType
		recordValue = request.OwnershipChallenge.RecordValue
	}
	w, flush := tableWriter("id\tdomain\tstate\trecord_name\trecord_type\trecord_value")
	_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
		request.RequestID, request.Domain, request.State,
		recordName, recordType, recordValue)
	flush()
	return 0
}

func emailDomainList(args []string) int {
	fs, account, accountID, endpoint, tokenFile, jsonOut :=
		emailDomainFlags("email-domain list")
	cursor := fs.String("cursor", "", "continue from an opaque next-page cursor")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr,
			"usage: witself email-domain list [--cursor CURSOR] [--account NAME]")
		return 2
	}
	resolvedAccountID, token, err := resolveControlPlaneAccountOperator(
		*account, *accountID, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	page, err := client.ListAgentEmailDomainRequestsPage(
		context.Background(), strings.TrimSpace(*endpoint), token,
		resolvedAccountID, strings.TrimSpace(*cursor))
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	if page.Requests == nil {
		page.Requests = []client.AgentEmailDomainRequest{}
	}
	if *jsonOut {
		return printJSON(page)
	}
	w, flush := tableWriter("id\tdomain\tstate\tupdated")
	for i := range page.Requests {
		request := &page.Requests[i]
		updated := "-"
		if request.UpdatedAt != nil {
			updated = formatTime(*request.UpdatedAt)
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
			request.RequestID, request.Domain, request.State, updated)
	}
	flush()
	if page.NextCursor != "" {
		fmt.Fprintf(os.Stderr, "next cursor: %s\n", page.NextCursor)
	}
	return 0
}
