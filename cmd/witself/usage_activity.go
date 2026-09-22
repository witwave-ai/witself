package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/witwave-ai/witself/internal/client"
)

func printAgentActivity(ctx context.Context, conn agentConnection, since, until time.Time, jsonOut bool) int {
	self, err := client.GetSelf(ctx, conn.Endpoint, conn.Token, client.SelfOptions{Observational: true})
	if err != nil || verifyInstallAgentIdentity(conn, self.Identity) != nil {
		fmt.Fprintln(os.Stderr, "witself: could not verify the selected agent for activity")
		return 1
	}
	report, err := client.GetActivity(ctx, conn.Endpoint, conn.Token, client.ActivityQuery{Since: since, Until: until, Bucket: "hour"})
	if errors.Is(err, client.ErrNotFound) {
		fmt.Fprintln(os.Stderr, "witself: server update needed for activity metrics")
		return 1
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "witself: activity report unavailable")
		return 1
	}
	if report.AccountID != self.Identity.AccountID || report.RealmID != self.Identity.RealmID || report.AgentID != self.Identity.AgentID {
		fmt.Fprintln(os.Stderr, "witself: activity report identity does not match the selected agent")
		return 1
	}
	if jsonOut {
		return printJSON(report)
	}
	if report.TrackingSince == nil {
		fmt.Fprintln(os.Stderr, "Not tracked yet: no supported activity has been recorded for this agent.")
		return 0
	}
	fmt.Fprintf(os.Stderr, "Recorded activity since %s; earlier history is not tracked.\n", report.TrackingSince.UTC().Format(time.RFC3339))
	if len(report.Points) == 0 {
		fmt.Fprintln(os.Stderr, "No recorded activity in the requested window.")
		return 0
	}
	w, flush := tableWriter("start\tdimension\tquantity\tunit\tevents")
	for _, point := range report.Points {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%d\n", point.BucketStart.UTC().Format(time.RFC3339), point.Dimension, point.Quantity, point.Unit, point.EventCount)
	}
	flush()
	return 0
}
