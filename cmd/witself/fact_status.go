package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/witwave-ai/witself/internal/client"
)

func factStatus(args []string) int {
	fs := flag.NewFlagSet("fact status", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configureCommandUsage(fs, "usage: witself fact status [agent connection flags]")
	account, realm, agent, endpoint, tokenFile := factConnectionFlags(fs)
	jsonOut := jsonFlag(fs)
	if parsed, exitCode := parseCommandFlags(fs, args); !parsed {
		return exitCode
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "witself: fact status accepts no positional arguments")
		return 2
	}
	conn, err := connectAgent(context.Background(), *account, *realm, *agent, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: connect fact service: %v\n", err)
		return 1
	}
	status, err := client.GetFactLimitStatus(context.Background(), conn.Endpoint, conn.Token)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: read fact limit status: %v\n", err)
		return 1
	}
	if *jsonOut {
		return printJSON(status)
	}
	if status.Used < 0 {
		fmt.Fprintln(os.Stderr, "witself: read fact limit status: server returned an invalid fact limit status")
		return 1
	}
	if status.Unlimited {
		if status.Max != nil || status.Remaining != nil || status.NearLimit || status.AtLimit || status.OverLimit {
			fmt.Fprintln(os.Stderr, "witself: read fact limit status: server returned an invalid unlimited fact limit status")
			return 1
		}
		fmt.Printf("used:\t%d\nmax:\tunlimited\nremaining:\tunlimited\nnear limit:\tfalse\nat limit:\tfalse\nover limit:\tfalse\n", status.Used)
		return 0
	}
	if status.Max == nil || status.Remaining == nil || *status.Max < 0 {
		fmt.Fprintln(os.Stderr, "witself: read fact limit status: server returned an invalid capped fact limit status")
		return 1
	}
	expectedRemaining := *status.Max - status.Used
	if expectedRemaining < 0 {
		expectedRemaining = 0
	}
	expectedNear := status.Used >= (*status.Max*9+9)/10
	if *status.Remaining != expectedRemaining ||
		status.NearLimit != expectedNear ||
		status.AtLimit != (status.Used == *status.Max) ||
		status.OverLimit != (status.Used > *status.Max) {
		fmt.Fprintln(os.Stderr, "witself: read fact limit status: server returned an inconsistent capped fact limit status")
		return 1
	}
	fmt.Printf("used:\t%d\nmax:\t%d\nremaining:\t%d\nnear limit:\t%t\nat limit:\t%t\nover limit:\t%t\n",
		status.Used, *status.Max, *status.Remaining,
		status.NearLimit, status.AtLimit, status.OverLimit)
	return 0
}
