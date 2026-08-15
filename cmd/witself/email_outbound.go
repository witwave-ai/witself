package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/client"
)

func emailSend(args []string) int {
	fs := flag.NewFlagSet("email send", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	connFlags := addMessageConnectionFlags(fs)
	to := fs.String("to", "", "single recipient email address")
	subject := fs.String("subject", "", "email subject")
	text := fs.String("text", "", "plain-text body")
	textFile := fs.String("text-file", "", "read the plain-text body from FILE ('-' means stdin)")
	textStdin := fs.Bool("text-stdin", false, "read the plain-text body from stdin")
	idempotencyKey := fs.String("idempotency-key", "", "required retry key for this logical send")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || strings.TrimSpace(*to) == "" || strings.TrimSpace(*subject) == "" {
		fmt.Fprintln(os.Stderr, "usage: witself email send --to ADDRESS --subject SUBJECT (--text TEXT|--text-file FILE|--text-stdin) [agent connection flags]")
		return 2
	}
	key := strings.TrimSpace(*idempotencyKey)
	if key == "" {
		fmt.Fprintln(os.Stderr, "witself: --idempotency-key is required for an irreversible external send")
		return 2
	}
	body, err := readBodyFromFlags(*text, *textFile, *textStdin)
	if err != nil || body == "" {
		if err != nil {
			fmt.Fprintf(os.Stderr, "witself: %s\n", emailCLIColumn(err.Error()))
		} else {
			fmt.Fprintln(os.Stderr, "witself: a non-empty plain-text body is required")
		}
		return 2
	}
	ctx := context.Background()
	conn, err := connFlags.connect(ctx)
	if err != nil {
		return printAgentEmailOutboundCLIError(err, *jsonOut)
	}
	message, err := client.SendAgentEmail(ctx, conn.Endpoint, conn.Token, client.SendAgentEmailInput{
		To: strings.TrimSpace(*to), Subject: strings.TrimSpace(*subject), Text: body, IdempotencyKey: key,
	})
	if err != nil {
		return printAgentEmailOutboundCLIError(err, *jsonOut)
	}
	return printAgentEmailOutbound(message, *jsonOut)
}

func emailReply(args []string) int {
	inboundID, args := leadingMessageID(args)
	hadLeadingID := inboundID != ""
	fs := flag.NewFlagSet("email reply", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	connFlags := addMessageConnectionFlags(fs)
	text := fs.String("text", "", "plain-text reply body")
	textFile := fs.String("text-file", "", "read the plain-text reply from FILE ('-' means stdin)")
	textStdin := fs.Bool("text-stdin", false, "read the plain-text reply from stdin")
	idempotencyKey := fs.String("idempotency-key", "", "required retry key for this logical reply")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if !hadLeadingID && fs.NArg() == 1 {
		inboundID = strings.TrimSpace(fs.Arg(0))
	}
	if inboundID == "" || (hadLeadingID && fs.NArg() != 0) || (!hadLeadingID && fs.NArg() != 1) {
		fmt.Fprintln(os.Stderr, "usage: witself email reply INBOUND_EMAIL_ID (--text TEXT|--text-file FILE|--text-stdin) [agent connection flags]")
		return 2
	}
	key := strings.TrimSpace(*idempotencyKey)
	if key == "" {
		fmt.Fprintln(os.Stderr, "witself: --idempotency-key is required for an irreversible external reply")
		return 2
	}
	body, err := readBodyFromFlags(*text, *textFile, *textStdin)
	if err != nil || body == "" {
		if err != nil {
			fmt.Fprintf(os.Stderr, "witself: %s\n", emailCLIColumn(err.Error()))
		} else {
			fmt.Fprintln(os.Stderr, "witself: a non-empty plain-text reply is required")
		}
		return 2
	}
	ctx := context.Background()
	conn, err := connFlags.connect(ctx)
	if err != nil {
		return printAgentEmailOutboundCLIError(err, *jsonOut)
	}
	message, err := client.ReplyAgentEmail(ctx, conn.Endpoint, conn.Token, inboundID, client.ReplyAgentEmailInput{
		Text: body, IdempotencyKey: key,
	})
	if err != nil {
		return printAgentEmailOutboundCLIError(err, *jsonOut)
	}
	return printAgentEmailOutbound(message, *jsonOut)
}

func emailSent(args []string) int {
	fs := flag.NewFlagSet("email sent", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	connFlags := addMessageConnectionFlags(fs)
	state := fs.String("state", "", "filter by outbound state")
	limit := fs.Int("limit", 50, "maximum messages to return (1-100)")
	cursor := fs.String("cursor", "", "continue from a pagination cursor")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || *limit < 1 || *limit > 100 {
		fmt.Fprintln(os.Stderr, "usage: witself email sent [--state STATE] [--limit 1-100] [--cursor CURSOR] [agent connection flags]")
		return 2
	}
	ctx := context.Background()
	conn, err := connFlags.connect(ctx)
	if err != nil {
		return printAgentEmailOutboundCLIError(err, *jsonOut)
	}
	page, err := client.ListSentAgentEmails(ctx, conn.Endpoint, conn.Token, client.AgentEmailOutboundListOptions{
		State: strings.TrimSpace(*state), Limit: *limit, Cursor: *cursor,
	})
	if err != nil {
		return printAgentEmailOutboundCLIError(err, *jsonOut)
	}
	if *jsonOut {
		return printJSON(map[string]any{"messages": page.Messages, "next_cursor": page.NextCursor})
	}
	w, flush := tableWriter("id\tfrom\tto\tsubject\tstate\tprovider_state\tcreated_at")
	defer flush()
	for _, message := range page.Messages {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			emailCLIColumn(message.ID), emailCLIColumn(message.From), emailCLIColumn(message.To),
			emailCLIColumn(message.Subject), emailCLIColumn(message.State), emailCLIColumn(message.ProviderState),
			message.CreatedAt.Format("2006-01-02T15:04:05Z07:00"))
	}
	if page.NextCursor != "" {
		fmt.Fprintf(os.Stderr, "next cursor: %s\n", emailCLIColumn(page.NextCursor))
	}
	return 0
}

func emailSentShow(args []string) int {
	messageID, args := leadingMessageID(args)
	hadLeadingID := messageID != ""
	fs := flag.NewFlagSet("email sent-show", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	connFlags := addMessageConnectionFlags(fs)
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if !hadLeadingID && fs.NArg() == 1 {
		messageID = strings.TrimSpace(fs.Arg(0))
	}
	if messageID == "" || (hadLeadingID && fs.NArg() != 0) || (!hadLeadingID && fs.NArg() != 1) {
		fmt.Fprintln(os.Stderr, "usage: witself email sent-show OUTBOUND_EMAIL_ID [agent connection flags]")
		return 2
	}
	ctx := context.Background()
	conn, err := connFlags.connect(ctx)
	if err != nil {
		return printAgentEmailOutboundCLIError(err, *jsonOut)
	}
	message, err := client.GetSentAgentEmail(ctx, conn.Endpoint, conn.Token, messageID)
	if err != nil {
		return printAgentEmailOutboundCLIError(err, *jsonOut)
	}
	return printAgentEmailOutbound(message, *jsonOut)
}

func printAgentEmailOutbound(message client.AgentEmailOutboundMessage, jsonOut bool) int {
	if jsonOut {
		return printJSON(map[string]any{"message": message})
	}
	fmt.Printf("id\t%s\nfrom\t%s\nto\t%s\nsubject\t%s\nstate\t%s\nprovider state\t%s\n",
		emailCLIColumn(message.ID), emailCLIColumn(message.From), emailCLIColumn(message.To),
		emailCLIColumn(message.Subject), emailCLIColumn(message.State), emailCLIColumn(message.ProviderState))
	return 0
}

func emailOperatorSendCmd(args []string) int {
	if len(args) == 0 || (args[0] != "show" && args[0] != "enable" && args[0] != "disable") {
		fmt.Fprintln(os.Stderr, "usage: witself email operator send show|enable|disable (--agent-id AGENT | --realm-id REALM) [operator connection flags]")
		return 2
	}
	operation := args[0]
	fs := flag.NewFlagSet("email operator send "+operation, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account := accountFlag(fs)
	endpoint := fs.String("endpoint", "", "witself-server endpoint URL")
	tokenFile := fs.String("token-file", "", "file containing an operator token")
	agentID := fs.String("agent-id", "", "target account agent id")
	realmID := fs.String("realm-id", "", "target realm id")
	expectedRowVersion := fs.Int64("expected-row-version", 0, "optional optimistic concurrency fence")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	agent := strings.TrimSpace(*agentID)
	realm := strings.TrimSpace(*realmID)
	if fs.NArg() != 0 || (agent == "") == (realm == "") || *expectedRowVersion < 0 || operation == "show" && *expectedRowVersion != 0 {
		fmt.Fprintln(os.Stderr, "usage: witself email operator send "+operation+" (--agent-id AGENT | --realm-id REALM) [operator connection flags]")
		return 2
	}
	ctx := context.Background()
	ep, token, err := connect(ctx, *account, *endpoint, *tokenFile)
	if err != nil {
		return printAgentEmailOutboundCLIError(err, *jsonOut)
	}
	desired := ""
	switch operation {
	case "enable":
		desired = "enabled"
	case "disable":
		desired = "disabled"
	}
	if agent != "" {
		var control client.AgentEmailSendControl
		if operation == "show" {
			control, err = client.GetAgentEmailSendControl(ctx, ep, token, agent)
		} else {
			control, err = client.SetAgentEmailSendControlExact(ctx, ep, token, agent, desired, *expectedRowVersion)
		}
		if err != nil {
			return printAgentEmailOutboundCLIError(err, *jsonOut)
		}
		if *jsonOut {
			return printJSON(map[string]any{"control": control})
		}
		fmt.Printf("agent\t%s\neffective send\t%s\nagent send\t%s\nrealm send\t%s\nrow version\t%d\n",
			emailCLIColumn(control.AgentID), emailCLIColumn(control.SendState),
			emailCLIColumn(control.AgentSendState), emailCLIColumn(control.RealmSendState), control.RowVersion)
		return 0
	}
	var control client.AgentEmailRealmSendControl
	if operation == "show" {
		control, err = client.GetRealmAgentEmailSendControl(ctx, ep, token, realm)
	} else {
		control, err = client.SetRealmAgentEmailSendControlExact(ctx, ep, token, realm, desired, *expectedRowVersion)
	}
	if err != nil {
		return printAgentEmailOutboundCLIError(err, *jsonOut)
	}
	if *jsonOut {
		return printJSON(map[string]any{"control": control})
	}
	fmt.Printf("realm\t%s\nsend\t%s\nagents\t%d\nrow version\t%d\n",
		emailCLIColumn(control.RealmID), emailCLIColumn(control.SendState), control.AgentCount, control.RowVersion)
	return 0
}

func printAgentEmailOutboundCLIError(err error, jsonOut bool) int {
	message := emailCLIColumn(err.Error())
	code, exitCode, retryable := "backend_unavailable", 7, true
	payload := map[string]any{
		"code": code, "message": message, "retryable": retryable,
	}

	var featureErr *client.FeatureNotEnabledError
	var rateErr *client.MessageRateLimitError
	var apiErr *client.APIError
	switch {
	case errors.As(err, &featureErr):
		code, exitCode, retryable = "feature_not_enabled", 3, false
		payload["feature"] = featureErr.Feature
	case errors.As(err, &rateErr):
		exitCode, retryable = 7, rateErr.Retryable
		if retryable {
			code = "rate_limited"
		} else {
			code = "limit_exceeded"
		}
		details := map[string]any{
			"limit_dimension": rateErr.LimitDimension,
			"limit_key":       rateErr.LimitKey,
			"scope":           rateErr.Scope,
			"limit":           rateErr.Limit,
			"used":            rateErr.Used,
			"attempted":       rateErr.Attempted,
			"window_seconds":  rateErr.WindowSeconds,
			"source":          rateErr.Source,
		}
		if retryable {
			retryAfter := int64((rateErr.RetryAfter + time.Second - 1) / time.Second)
			if retryAfter < 1 {
				retryAfter = 1
			}
			payload["retry_after"] = retryAfter
			details["retry_after"] = retryAfter
		}
		if !rateErr.ResetAt.IsZero() {
			details["reset_at"] = rateErr.ResetAt.UTC().Format(time.RFC3339Nano)
		}
		payload["details"] = details
	case errors.Is(err, client.ErrUnauthorized):
		code, exitCode, retryable = "auth_failed", 4, false
	case errors.As(err, &apiErr):
		code, retryable = apiErr.Code, apiErr.Retryable
		switch apiErr.Code {
		case "forbidden", "feature_not_enabled":
			exitCode = 3
		case "auth_failed":
			exitCode = 4
		case "not_found":
			exitCode = 5
		case "agent_email_idempotency_conflict", "agent_email_state_conflict", "agent_email_processing_busy":
			exitCode = 6
		case "invalid_request":
			exitCode = 2
		default:
			exitCode = 7
		}
	case errors.Is(err, client.ErrForbidden):
		code, exitCode, retryable = "forbidden", 3, false
	case errors.Is(err, client.ErrNotFound):
		code, exitCode, retryable = "not_found", 5, false
	case errors.Is(err, client.ErrConflict):
		code, exitCode, retryable = "conflict", 6, false
	case errors.Is(err, client.ErrBadRequest):
		code, exitCode, retryable = "invalid_request", 2, false
	}
	payload["code"], payload["retryable"] = code, retryable
	if jsonOut {
		_ = printJSON(map[string]any{
			"schema_version": "witself.v0",
			"ok":             false,
			"error":          payload,
		})
	} else {
		fmt.Fprintf(os.Stderr, "witself: %s\n", message)
	}
	return exitCode
}
