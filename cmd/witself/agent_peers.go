package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/witwave-ai/witself/internal/client"
)

func agentPeers(args []string) int {
	fs := flag.NewFlagSet("agent peers", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account := accountFlag(fs)
	realm := fs.String("realm", "", `local realm name (default: WITSELF_REALM or "default")`)
	agent := fs.String("agent", "", "local agent name (default: WITSELF_AGENT)")
	endpoint := fs.String("endpoint", "", "witself-server endpoint URL")
	tokenFile := fs.String("token-file", "", "file containing an agent token")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: witself agent peers [--account NAME] [--realm NAME] (--agent NAME | --endpoint URL --token-file FILE) [--json]")
		return 2
	}

	ctx := context.Background()
	conn, err := connectAgent(ctx, *account, *realm, *agent, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	if err := verifyAgentConnection(ctx, conn); err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	peers, err := client.GetSelfPeers(ctx, conn.Endpoint, conn.Token)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	if *jsonOut {
		return printJSON(peers)
	}

	w, flush := tableWriter("agent\tlast activity (UTC)\tage\truntime\tlocation\tevent")
	now := time.Now().UTC()
	for _, peer := range peers.Peers {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			tabSafe(peer.Name), peerActivityTimestamp(peer.LastActivityAt),
			peerActivityAge(peer.LastActivityAt, now),
			peerActivityField(peer.LastRuntime), peerActivityField(peer.LastLocation),
			peerActivityField(peer.LastEvent))
	}
	flush()
	return 0
}

func verifyAgentConnection(ctx context.Context, conn agentConnection) error {
	digest, err := client.GetSelf(ctx, conn.Endpoint, conn.Token, client.SelfOptions{})
	if err != nil {
		return fmt.Errorf("verify agent identity: %w", err)
	}
	if conn.AccountID != "" && digest.Identity.AccountID != conn.AccountID {
		return fmt.Errorf("agent token belongs to account %s, not local account %s (%s)",
			digest.Identity.AccountID, conn.AccountName, conn.AccountID)
	}
	if conn.RealmName != "" && digest.Identity.RealmName != conn.RealmName {
		return fmt.Errorf("agent token belongs to realm %q, not %q", digest.Identity.RealmName, conn.RealmName)
	}
	if conn.AgentName != "" && digest.Identity.AgentName != conn.AgentName {
		return fmt.Errorf("agent token belongs to agent %q, not %q", digest.Identity.AgentName, conn.AgentName)
	}
	return nil
}

func peerActivityTimestamp(value *time.Time) string {
	if value == nil || value.IsZero() {
		return "-"
	}
	return value.UTC().Format(time.RFC3339)
}

func peerActivityAge(value *time.Time, now time.Time) string {
	if value == nil || value.IsZero() {
		return "never"
	}
	age := now.UTC().Sub(value.UTC())
	if age < time.Minute {
		return "<1m ago"
	}
	if age < time.Hour {
		return fmt.Sprintf("%dm ago", int(age/time.Minute))
	}
	if age < 24*time.Hour {
		return fmt.Sprintf("%dh ago", int(age/time.Hour))
	}
	return fmt.Sprintf("%dd ago", int(age/(24*time.Hour)))
}

func peerActivityField(value string) string {
	if value == "" {
		return "-"
	}
	return tabSafe(value)
}
