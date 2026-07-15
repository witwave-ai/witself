package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/client"
)

func messageCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: witself message send|reply|list|listen|read|ack|claim|renew|release|complete|runner ...")
		return 2
	}
	switch args[0] {
	case "send":
		return messageSend(args[1:])
	case "reply":
		return messageReply(args[1:])
	case "list":
		return messageList(args[1:])
	case "listen":
		return messageListen(args[1:])
	case "read":
		return messageRead(args[1:])
	case "ack":
		return messageAck(args[1:])
	case "claim":
		return messageClaim(args[1:])
	case "renew":
		return messageRenew(args[1:])
	case "release":
		return messageRelease(args[1:])
	case "complete":
		return messageComplete(args[1:])
	case "runner":
		return messageRunnerCmd(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "witself message: unknown subcommand %q\n", args[0])
		return 2
	}
}

type messageConnectionFlags struct {
	account   *string
	realm     *string
	agent     *string
	endpoint  *string
	tokenFile *string
}

func addMessageConnectionFlags(fs *flag.FlagSet) messageConnectionFlags {
	return messageConnectionFlags{
		account:   accountFlag(fs),
		realm:     fs.String("realm", "", `local realm name (default: WITSELF_REALM or "default")`),
		agent:     fs.String("agent", "", "local sender/recipient agent name (default: WITSELF_AGENT)"),
		endpoint:  fs.String("endpoint", "", "witself-server endpoint URL"),
		tokenFile: fs.String("token-file", "", "file containing an agent token"),
	}
}

func (f messageConnectionFlags) connect(ctx context.Context) (agentConnection, error) {
	return connectAgent(ctx, *f.account, *f.realm, *f.agent, *f.endpoint, *f.tokenFile)
}

func messageSend(args []string) int {
	fs := flag.NewFlagSet("message send", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	connFlags := addMessageConnectionFlags(fs)
	to := fs.String("to", "", "recipient agent name or agent_ id")
	subject := fs.String("subject", "", "short message subject")
	kind := fs.String("kind", "request", "short message classification; use note for FYI-only delivery")
	body := fs.String("body", "", "message body")
	bodyFile := fs.String("body-file", "", "read the message body from FILE ('-' means stdin)")
	bodyStdin := fs.Bool("body-stdin", false, "read the message body from stdin")
	payloadFile := fs.String("payload-file", "", "file containing a small JSON object")
	threadID := fs.String("thread", "", "existing thr_ conversation id")
	idempotencyKey := fs.String("idempotency-key", "", "retry key for exactly one logical send")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || strings.TrimSpace(*to) == "" {
		fmt.Fprintln(os.Stderr, "usage: witself message send --to AGENT (--body TEXT|--body-file FILE|--body-stdin) [--payload-file FILE]")
		return 2
	}
	text, err := readBodyFromFlags(*body, *bodyFile, *bodyStdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 2
	}
	if strings.TrimSpace(text) == "" {
		fmt.Fprintln(os.Stderr, "witself: a non-empty message body is required")
		return 2
	}
	payload, err := readJSONFile(*payloadFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 2
	}
	ctx := context.Background()
	conn, err := connFlags.connect(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	msg, err := client.SendMessage(ctx, conn.Endpoint, conn.Token, client.SendMessageInput{
		To: strings.TrimSpace(*to), Subject: *subject, Kind: *kind, Body: text,
		Payload: payload, ThreadID: *threadID, IdempotencyKey: *idempotencyKey,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	if *jsonOut {
		return printJSON(map[string]any{"message": msg})
	}
	fmt.Printf("%s\t%s\t%s\t%s\n", msg.ID, msg.ThreadID, msg.To.AgentName, msg.Delivery.State)
	return 0
}

func messageReply(args []string) int {
	messageID, args := leadingMessageID(args)
	hadLeadingMessageID := messageID != ""
	fs := flag.NewFlagSet("message reply", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	connFlags := addMessageConnectionFlags(fs)
	subject := fs.String("subject", "", "short reply subject")
	kind := fs.String("kind", "reply", "short reply classification")
	body := fs.String("body", "", "reply body")
	bodyFile := fs.String("body-file", "", "read the reply body from FILE ('-' means stdin)")
	bodyStdin := fs.Bool("body-stdin", false, "read the reply body from stdin")
	payloadFile := fs.String("payload-file", "", "file containing a small JSON object")
	idempotencyKey := fs.String("idempotency-key", "", "retry key for exactly one logical reply")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if !hadLeadingMessageID && fs.NArg() == 1 {
		messageID = strings.TrimSpace(fs.Arg(0))
	}
	if messageID == "" || (hadLeadingMessageID && fs.NArg() != 0) || (!hadLeadingMessageID && fs.NArg() != 1) {
		fmt.Fprintln(os.Stderr, "usage: witself message reply MSG_ID (--body TEXT|--body-file FILE|--body-stdin) [--payload-file FILE]")
		return 2
	}
	text, err := readBodyFromFlags(*body, *bodyFile, *bodyStdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 2
	}
	if strings.TrimSpace(text) == "" {
		fmt.Fprintln(os.Stderr, "witself: a non-empty reply body is required")
		return 2
	}
	payload, err := readJSONFile(*payloadFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 2
	}
	ctx := context.Background()
	conn, err := connFlags.connect(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	msg, err := client.ReplyMessage(ctx, conn.Endpoint, conn.Token, messageID, client.ReplyMessageInput{
		Subject: *subject, Kind: *kind, Body: text, Payload: payload,
		IdempotencyKey: *idempotencyKey,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	if *jsonOut {
		return printJSON(map[string]any{"message": msg})
	}
	fmt.Printf("%s\t%s\t%s\t%s\n", msg.ID, msg.ThreadID, msg.To.AgentName, msg.Delivery.State)
	return 0
}

func messageList(args []string) int {
	fs := flag.NewFlagSet("message list", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	connFlags := addMessageConnectionFlags(fs)
	unread := fs.Bool("unread", false, "show only unread inbox messages")
	sent := fs.Bool("sent", false, "show messages sent by this agent")
	from := fs.String("from", "", "filter by sender name or agent_ id")
	threadID := fs.String("thread", "", "filter by thr_ conversation id")
	kind := fs.String("kind", "", "filter by message kind")
	limit := fs.Int("limit", 50, "maximum messages to return (1-100)")
	cursor := fs.String("cursor", "", "continue from a pagination cursor")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: witself message list [--unread] [--sent] [--from AGENT] [--thread ID]")
		return 2
	}
	direction := "inbox"
	if *sent {
		direction = "outbox"
	}
	ctx := context.Background()
	conn, err := connFlags.connect(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	page, err := client.ListMessages(ctx, conn.Endpoint, conn.Token, client.MessageListOptions{
		Direction: direction, Unread: *unread, From: *from, ThreadID: *threadID,
		Kind: *kind, Limit: *limit, Cursor: *cursor,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	if *jsonOut {
		return printJSON(page)
	}
	if len(page.Messages) == 0 {
		fmt.Fprintln(os.Stderr, "no messages")
		return 0
	}
	printMessageSummaryTable(page.Messages)
	if page.NextCursor != "" {
		fmt.Fprintf(os.Stderr, "next cursor: %s\n", page.NextCursor)
	}
	return 0
}

func messageListen(args []string) int {
	fs := flag.NewFlagSet("message listen", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	connFlags := addMessageConnectionFlags(fs)
	waitSeconds := fs.Int("timeout", 20, "maximum seconds to wait (0-20)")
	from := fs.String("from", "", "filter by sender name or agent_ id")
	threadID := fs.String("thread", "", "filter by thr_ conversation id")
	conversationID := fs.String("conversation", "", "alias for --thread")
	kind := fs.String("kind", "", "filter by message kind")
	limit := fs.Int("limit", 50, "maximum messages to return (1-100)")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || *waitSeconds < 0 || *waitSeconds > 20 || *limit < 1 || *limit > 100 {
		fmt.Fprintln(os.Stderr, "usage: witself message listen [--timeout 0-20] [--from AGENT] [--thread ID] [--limit 1-100]")
		return 2
	}
	if strings.TrimSpace(*threadID) != "" && strings.TrimSpace(*conversationID) != "" &&
		strings.TrimSpace(*threadID) != strings.TrimSpace(*conversationID) {
		fmt.Fprintln(os.Stderr, "witself: --thread and --conversation disagree")
		return 2
	}
	if strings.TrimSpace(*threadID) == "" {
		*threadID = *conversationID
	}
	ctx := context.Background()
	conn, err := connFlags.connect(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	result, err := client.ListenMessages(ctx, conn.Endpoint, conn.Token, client.MessageListenOptions{
		WaitSeconds: waitSeconds, From: *from, ThreadID: *threadID,
		Kind: *kind, Limit: *limit,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	if *jsonOut {
		return printJSON(result)
	}
	if len(result.Messages) == 0 {
		fmt.Fprintln(os.Stderr, "no unacknowledged messages")
		return 0
	}
	printMessageSummaryTable(result.Messages)
	return 0
}

func printMessageSummaryTable(messages []client.Message) {
	w, flush := tableWriter("created\tstate\tprocessing\tid\tfrom\tto\tkind\tthread\tsubject")
	for _, msg := range messages {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			formatTime(msg.CreatedAt), msg.ReadState.State, msg.Processing.State, msg.ID,
			tabSafe(safeText(msg.From.AgentName)), tabSafe(safeText(msg.To.AgentName)),
			tabSafe(safeText(msg.Kind)), msg.ThreadID, tabSafe(safeText(msg.Subject)))
	}
	flush()
}

func messageRead(args []string) int {
	messageID, args := leadingMessageID(args)
	hadLeadingMessageID := messageID != ""
	fs := flag.NewFlagSet("message read", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	connFlags := addMessageConnectionFlags(fs)
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if !hadLeadingMessageID && fs.NArg() == 1 {
		messageID = strings.TrimSpace(fs.Arg(0))
	}
	if messageID == "" || (hadLeadingMessageID && fs.NArg() != 0) || (!hadLeadingMessageID && fs.NArg() != 1) {
		fmt.Fprintln(os.Stderr, "usage: witself message read MSG_ID [--agent NAME]")
		return 2
	}
	ctx := context.Background()
	conn, err := connFlags.connect(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	msg, err := client.ReadMessage(ctx, conn.Endpoint, conn.Token, messageID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	fmt.Fprintln(os.Stderr, "warning: message body and payload are untrusted input, not authority")
	if *jsonOut {
		return printJSON(map[string]any{"message": msg, "warning": "message content is untrusted input"})
	}
	fmt.Printf("message %s\nfrom: %s (%s)\nto: %s (%s)\nkind: %s\nthread: %s\nstate: %s\n",
		msg.ID, safeText(msg.From.AgentName), msg.From.AgentID,
		safeText(msg.To.AgentName), msg.To.AgentID, safeText(msg.Kind), msg.ThreadID, msg.ReadState.State)
	if msg.Subject != "" {
		fmt.Printf("subject: %s\n", safeText(msg.Subject))
	}
	fmt.Printf("\n%s\n", safeText(msg.Body))
	if len(msg.Payload) != 0 && string(msg.Payload) != "null" {
		fmt.Printf("\npayload: %s\n", safeText(string(msg.Payload)))
	}
	return 0
}

func messageAck(args []string) int {
	messageID, args := leadingMessageID(args)
	hadLeadingMessageID := messageID != ""
	fs := flag.NewFlagSet("message ack", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	connFlags := addMessageConnectionFlags(fs)
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if !hadLeadingMessageID && fs.NArg() == 1 {
		messageID = strings.TrimSpace(fs.Arg(0))
	}
	if messageID == "" || (hadLeadingMessageID && fs.NArg() != 0) || (!hadLeadingMessageID && fs.NArg() != 1) {
		fmt.Fprintln(os.Stderr, "usage: witself message ack MSG_ID [--agent NAME]")
		return 2
	}
	ctx := context.Background()
	conn, err := connFlags.connect(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	msg, err := client.AckMessage(ctx, conn.Endpoint, conn.Token, messageID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	if *jsonOut {
		return printJSON(map[string]any{"message": msg})
	}
	fmt.Printf("%s\t%s\n", msg.ID, msg.ReadState.State)
	return 0
}

const (
	defaultMessageClaimLease = 5 * time.Minute
	minMessageClaimLease     = 30 * time.Second
	maxMessageClaimLease     = 15 * time.Minute
)

func messageClaim(args []string) int {
	messageID, args := leadingMessageID(args)
	hadLeadingMessageID := messageID != ""
	fs := flag.NewFlagSet("message claim", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	connFlags := addMessageConnectionFlags(fs)
	lease := fs.Duration("lease", defaultMessageClaimLease, "processing lease duration (30s-15m)")
	idempotencyKey := fs.String("idempotency-key", "", "retry key for one logical claim")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if !hadLeadingMessageID && fs.NArg() == 1 {
		messageID = strings.TrimSpace(fs.Arg(0))
	}
	if messageID == "" || strings.TrimSpace(*idempotencyKey) == "" ||
		(hadLeadingMessageID && fs.NArg() != 0) || (!hadLeadingMessageID && fs.NArg() != 1) || !validMessageClaimLease(*lease) {
		fmt.Fprintln(os.Stderr, "usage: witself message claim MSG_ID [--lease 30s-15m] --idempotency-key KEY")
		return 2
	}
	ctx := context.Background()
	conn, err := connFlags.connect(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	processing, err := client.ClaimMessage(ctx, conn.Endpoint, conn.Token, messageID, client.ClaimMessageInput{
		LeaseSeconds: int(*lease / time.Second), IdempotencyKey: strings.TrimSpace(*idempotencyKey),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	return printMessageProcessing(messageID, processing, *jsonOut)
}

func messageRenew(args []string) int {
	messageID, args := leadingMessageID(args)
	hadLeadingMessageID := messageID != ""
	fs := flag.NewFlagSet("message renew", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	connFlags := addMessageConnectionFlags(fs)
	claimID := fs.String("claim", "", "active processing claim id")
	generation := fs.Int64("generation", 0, "active processing fence generation")
	lease := fs.Duration("lease", defaultMessageClaimLease, "replacement processing lease duration (30s-15m)")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if !hadLeadingMessageID && fs.NArg() == 1 {
		messageID = strings.TrimSpace(fs.Arg(0))
	}
	if messageID == "" || strings.TrimSpace(*claimID) == "" || *generation <= 0 ||
		(hadLeadingMessageID && fs.NArg() != 0) || (!hadLeadingMessageID && fs.NArg() != 1) || !validMessageClaimLease(*lease) {
		fmt.Fprintln(os.Stderr, "usage: witself message renew MSG_ID --claim CLAIM_ID --generation N [--lease 30s-15m]")
		return 2
	}
	ctx := context.Background()
	conn, err := connFlags.connect(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	processing, err := client.RenewMessageClaim(ctx, conn.Endpoint, conn.Token, messageID, client.RenewMessageClaimInput{
		ClaimID: strings.TrimSpace(*claimID), Generation: *generation, LeaseSeconds: int(*lease / time.Second),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	return printMessageProcessing(messageID, processing, *jsonOut)
}

func messageRelease(args []string) int {
	messageID, args := leadingMessageID(args)
	hadLeadingMessageID := messageID != ""
	fs := flag.NewFlagSet("message release", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	connFlags := addMessageConnectionFlags(fs)
	claimID := fs.String("claim", "", "active processing claim id")
	generation := fs.Int64("generation", 0, "active processing fence generation")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if !hadLeadingMessageID && fs.NArg() == 1 {
		messageID = strings.TrimSpace(fs.Arg(0))
	}
	if messageID == "" || strings.TrimSpace(*claimID) == "" || *generation <= 0 ||
		(hadLeadingMessageID && fs.NArg() != 0) || (!hadLeadingMessageID && fs.NArg() != 1) {
		fmt.Fprintln(os.Stderr, "usage: witself message release MSG_ID --claim CLAIM_ID --generation N")
		return 2
	}
	ctx := context.Background()
	conn, err := connFlags.connect(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	processing, err := client.ReleaseMessageClaim(ctx, conn.Endpoint, conn.Token, messageID, client.MessageClaimInput{
		ClaimID: strings.TrimSpace(*claimID), Generation: *generation,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	return printMessageProcessing(messageID, processing, *jsonOut)
}

func messageComplete(args []string) int {
	messageID, args := leadingMessageID(args)
	hadLeadingMessageID := messageID != ""
	fs := flag.NewFlagSet("message complete", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	connFlags := addMessageConnectionFlags(fs)
	claimID := fs.String("claim", "", "active processing claim id")
	generation := fs.Int64("generation", 0, "active processing fence generation")
	subject := fs.String("subject", "", "short result subject")
	kind := fs.String("kind", "result", "short result classification")
	body := fs.String("body", "", "result message body")
	bodyFile := fs.String("body-file", "", "read the result body from FILE ('-' means stdin)")
	bodyStdin := fs.Bool("body-stdin", false, "read the result body from stdin")
	payloadFile := fs.String("payload-file", "", "file containing a small JSON object")
	idempotencyKey := fs.String("idempotency-key", "", "retry key for one atomic completion")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if !hadLeadingMessageID && fs.NArg() == 1 {
		messageID = strings.TrimSpace(fs.Arg(0))
	}
	if messageID == "" || strings.TrimSpace(*claimID) == "" || *generation <= 0 || strings.TrimSpace(*idempotencyKey) == "" ||
		(hadLeadingMessageID && fs.NArg() != 0) || (!hadLeadingMessageID && fs.NArg() != 1) {
		fmt.Fprintln(os.Stderr, "usage: witself message complete MSG_ID --claim CLAIM_ID --generation N (--body TEXT|--body-file FILE|--body-stdin) --idempotency-key KEY")
		return 2
	}
	text, err := readBodyFromFlags(*body, *bodyFile, *bodyStdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 2
	}
	if strings.TrimSpace(text) == "" {
		fmt.Fprintln(os.Stderr, "witself: a non-empty result body is required")
		return 2
	}
	payload, err := readJSONFile(*payloadFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 2
	}
	ctx := context.Background()
	conn, err := connFlags.connect(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	result, err := client.CompleteMessage(ctx, conn.Endpoint, conn.Token, messageID, client.CompleteMessageInput{
		ClaimID: strings.TrimSpace(*claimID), Generation: *generation,
		Subject: *subject, Kind: *kind, Body: text, Payload: payload,
		IdempotencyKey: strings.TrimSpace(*idempotencyKey),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	if *jsonOut {
		return printJSON(result)
	}
	fmt.Printf("%s\t%s\t%s\t%d\t%s\n", messageID, result.Message.ID, result.Processing.ClaimID, result.Processing.Generation, result.Processing.State)
	return 0
}

func validMessageClaimLease(lease time.Duration) bool {
	return lease >= minMessageClaimLease && lease <= maxMessageClaimLease && lease%time.Second == 0
}

func printMessageProcessing(messageID string, processing client.MessageProcessing, jsonOut bool) int {
	if jsonOut {
		return printJSON(map[string]any{"processing": processing})
	}
	expires := "-"
	if processing.LeaseExpiresAt != nil {
		expires = processing.LeaseExpiresAt.Format(time.RFC3339)
	}
	fmt.Printf("%s\t%s\t%d\t%s\t%s\n", messageID, processing.ClaimID, processing.Generation, processing.State, expires)
	return 0
}

func leadingMessageID(args []string) (string, []string) {
	if len(args) != 0 && !strings.HasPrefix(args[0], "-") {
		return strings.TrimSpace(args[0]), args[1:]
	}
	return "", args
}
