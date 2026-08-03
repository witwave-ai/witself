package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/id"
	"github.com/witwave-ai/witself/internal/local"
)

func realmEmailAliasCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr,
			"usage: witself realm email-alias (request|list) ...")
		return 2
	}
	switch args[0] {
	case "request":
		return realmEmailAliasRequest(args[1:])
	case "list":
		return realmEmailAliasList(args[1:])
	default:
		fmt.Fprintf(os.Stderr,
			"witself realm email-alias: unknown action %q\n", args[0])
		return 2
	}
}

func resolveControlPlaneAccountOperator(
	accountName, accountID, tokenFile string,
) (string, string, error) {
	if strings.TrimSpace(tokenFile) != "" {
		if strings.TrimSpace(accountID) == "" {
			return "", "", fmt.Errorf("--token-file requires --account-id")
		}
		token, err := readToken(tokenFile)
		if err != nil {
			return "", "", err
		}
		return strings.TrimSpace(accountID), token, nil
	}
	if strings.TrimSpace(accountID) != "" {
		return "", "", fmt.Errorf("--account-id is only used with --token-file")
	}
	_, account, token, err := local.Resolve(accountName)
	if err != nil {
		return "", "", err
	}
	return account.ID, token, nil
}

func realmEmailAliasFlags(name string) (*flag.FlagSet, *string, *string, *string, *string, *string, *bool) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account := accountFlag(fs)
	accountID := fs.String("account-id", "", "account id (only with --token-file)")
	realmID := fs.String("realm", "", "realm id (required)")
	endpoint := fs.String("endpoint", defaultControlPlane, "control-plane URL")
	tokenFile := fs.String("token-file", "", "file containing an operator token")
	jsonOut := jsonFlag(fs)
	return fs, account, accountID, realmID, endpoint, tokenFile, jsonOut
}

func realmEmailAliasRequest(args []string) int {
	fs, account, accountID, realmID, endpoint, tokenFile, jsonOut :=
		realmEmailAliasFlags("realm email-alias request")
	alias := fs.String("alias", "", "memorable realm email alias (required)")
	idempotencyKey := fs.String("idempotency-key", "", "retry key (generated when omitted)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || strings.TrimSpace(*realmID) == "" ||
		strings.TrimSpace(*alias) == "" {
		fmt.Fprintln(os.Stderr,
			"usage: witself realm email-alias request --realm REALM_ID --alias ALIAS [--account NAME]")
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
		key, err = id.New("email_alias_request")
		if err != nil {
			fmt.Fprintf(os.Stderr, "witself: generate idempotency key: %v\n", err)
			return 1
		}
	}
	request, err := client.RequestRealmEmailAlias(context.Background(),
		strings.TrimSpace(*endpoint), token, resolvedAccountID,
		strings.TrimSpace(*realmID), strings.TrimSpace(*alias), key)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	if *jsonOut {
		return printJSON(map[string]any{"request": request})
	}
	fmt.Printf("%s\t%s\t%s\n", request.ID, request.Alias, request.Status)
	return 0
}

func realmEmailAliasList(args []string) int {
	fs, account, accountID, realmID, endpoint, tokenFile, jsonOut :=
		realmEmailAliasFlags("realm email-alias list")
	cursor := fs.String("cursor", "", "continue from an opaque next-page cursor")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || strings.TrimSpace(*realmID) == "" {
		fmt.Fprintln(os.Stderr,
			"usage: witself realm email-alias list --realm REALM_ID [--cursor CURSOR] [--account NAME]")
		return 2
	}
	resolvedAccountID, token, err := resolveControlPlaneAccountOperator(
		*account, *accountID, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	page, err := client.ListRealmEmailAliasRequestsPage(context.Background(),
		strings.TrimSpace(*endpoint), token, resolvedAccountID,
		strings.TrimSpace(*realmID), strings.TrimSpace(*cursor))
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	if page.Requests == nil {
		page.Requests = []client.RealmEmailAliasRequest{}
	}
	if *jsonOut {
		return printJSON(page)
	}
	w, flush := tableWriter("id\talias\tstatus\tupdated")
	for _, request := range page.Requests {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
			request.ID, request.Alias, request.Status, formatTime(request.UpdatedAt))
	}
	flush()
	if page.NextCursor != "" {
		fmt.Fprintf(os.Stderr, "next cursor: %s\n", page.NextCursor)
	}
	return 0
}
