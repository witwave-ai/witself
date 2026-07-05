package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/witwave-ai/witself/internal/client"
)

// cellsCmd handles `witself-admin cells ...` — the fleet registry as
// the operator (and the dashboard's cells pane) sees it.
func cellsCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: witself-admin cells list ...")
		return 2
	}
	switch args[0] {
	case "list":
		return cellsList(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "witself-admin cells: unknown subcommand %q\n", args[0])
		return 2
	}
}

func cellsList(args []string) int {
	fs := flag.NewFlagSet("cells list", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	endpoint := fs.String("endpoint", "", "control-plane URL")
	token := fs.String("token", "", "admin token")
	tokenFile := fs.String("token-file", "", "file containing the admin token")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	tok, err := resolveAdminToken(*token, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself-admin: %v\n", err)
		return 2
	}
	cells, err := client.ListAdminCells(context.Background(), cpEndpoint(*endpoint), tok)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself-admin: %v\n", err)
		return 1
	}
	if *jsonOut {
		return printJSON(map[string]any{"cells": cells})
	}
	if len(cells) == 0 {
		fmt.Fprintln(os.Stderr, "no cells registered")
		return 0
	}
	w, flush := tableWriter("cell\tcloud\tregion\taccepting\taccounts")
	for _, c := range cells {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%v\t%d\n",
			tabSafe(safeText(c.Name)), c.Cloud, c.Region, c.Accepting, c.AccountCount)
	}
	flush()
	return 0
}

// eventsCmd handles `witself-admin events ...` — the fleet-wide audit
// tail (every cell, every account, newest first).
func eventsCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: witself-admin events list|watch ...")
		return 2
	}
	switch args[0] {
	case "list":
		return eventsList(args[1:])
	case "watch":
		return eventsWatch(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "witself-admin events: unknown subcommand %q\n", args[0])
		return 2
	}
}

func eventsList(args []string) int {
	fs := flag.NewFlagSet("events list", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	endpoint := fs.String("endpoint", "", "control-plane URL")
	token := fs.String("token", "", "admin token")
	tokenFile := fs.String("token-file", "", "file containing the admin token")
	verb := fs.String("verb", "", "filter by exact verb (e.g. recovery.requested)")
	sinceStr := fs.String("since", "", "only events at or after this RFC3339 timestamp")
	limit := fs.Int("limit", 50, "per-cell page size (1-500)")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	tok, err := resolveAdminToken(*token, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself-admin: %v\n", err)
		return 2
	}
	filter := client.AdminEventFilter{Verb: *verb, Limit: *limit}
	if s := strings.TrimSpace(*sinceStr); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			fmt.Fprintf(os.Stderr, "witself-admin: --since must be RFC3339: %v\n", err)
			return 2
		}
		filter.Since = &t
	}
	res, err := client.ListAdminEvents(context.Background(), cpEndpoint(*endpoint), tok, filter)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself-admin: %v\n", err)
		return 1
	}
	if *jsonOut {
		return printJSON(res)
	}
	if len(res.Events) == 0 {
		fmt.Fprintln(os.Stderr, "no events across the fleet matching your filter")
	} else {
		w, flush := tableWriter("occurred_at\tcell\tverb\tactor\taccount\tmetadata")
		for _, e := range res.Events {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
				e.OccurredAt.UTC().Format(time.RFC3339),
				e.Cell, e.Verb, eventActor(e), e.AccountID,
				tabSafe(safeText(string(e.Metadata))))
		}
		flush()
	}
	reportDegradedCells(res.Cells)
	if res.AggregateCapped {
		fmt.Fprintln(os.Stderr, "\nresult was capped at 500 events — use --since or --verb to narrow the query.")
	}
	return 0
}

// eventsWatch tails the fleet-wide audit ledger: poll + high-water
// mark on occurred_at, same contract as ticket watch. --json emits
// ndjson for the dashboard's events pane and agent pipelines.
func eventsWatch(args []string) int {
	fs := flag.NewFlagSet("events watch", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	endpoint := fs.String("endpoint", "", "control-plane URL")
	token := fs.String("token", "", "admin token")
	tokenFile := fs.String("token-file", "", "file containing the admin token")
	interval := fs.Duration("interval", 30*time.Second, "poll cadence (minimum 5s)")
	verb := fs.String("verb", "", "filter by exact verb")
	limit := fs.Int("limit", 100, "per-cell page size (1-500)")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *interval < 5*time.Second {
		*interval = 5 * time.Second
	}
	tok, err := resolveAdminToken(*token, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself-admin: %v\n", err)
		return 2
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if !*jsonOut {
		fmt.Fprintf(os.Stderr, "watching every %s. ctrl-c to stop.\n", *interval)
	}

	ep := cpEndpoint(*endpoint)
	var hwm time.Time
	first := true

	tick := func() {
		f := client.AdminEventFilter{Verb: *verb, Limit: *limit}
		if !first {
			since := hwm
			f.Since = &since
		}
		res, err := client.ListAdminEvents(ctx, ep, tok, f)
		if err != nil {
			fmt.Fprintf(os.Stderr, "witself-admin: watch tick failed: %v\n", err)
			return
		}
		if first {
			hwm = newestEventTime(res.Events, time.Now().UTC().Add(-1*time.Second))
			first = false
			return
		}
		for _, e := range res.Events {
			if !e.OccurredAt.After(hwm) {
				continue
			}
			emitWatchedEvent(e, *jsonOut)
		}
		// Audit events are immutable — unlike tickets they never
		// self-heal into a later window via activity bumps. Advancing
		// the high-water mark past a tick where a cell was down would
		// permanently drop that cell's events from the tail, so on a
		// degraded tick we HOLD the mark and retry the same window
		// (duplicates are suppressed by the After() filter above).
		if n := degradedCellCount(res.Cells); n > 0 {
			fmt.Fprintf(os.Stderr, "witself-admin: %d cell(s) unreachable this tick — holding position so their events aren't skipped\n", n)
			return
		}
		if res.AggregateCapped {
			// The cap keeps the NEWEST events, so advancing may skip
			// ones in the dropped middle. No silent gaps: warn loudly.
			fmt.Fprintln(os.Stderr, "witself-admin: event volume exceeded the 500-event cap this tick — a gap is possible; narrow with --verb or shorten --interval")
		}
		hwm = newestEventTime(res.Events, hwm)
	}

	tick()
	if ctx.Err() != nil {
		return 0
	}
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return 0
		case <-ticker.C:
			tick()
		}
	}
}

// newestEventTime mirrors newestActivity for the events tail.
func newestEventTime(events []client.AdminEvent, fallback time.Time) time.Time {
	newest := fallback
	for _, e := range events {
		if e.OccurredAt.After(newest) {
			newest = e.OccurredAt
		}
	}
	return newest
}

func emitWatchedEvent(e client.AdminEvent, jsonMode bool) {
	if jsonMode {
		buf, err := json.Marshal(e)
		if err != nil {
			return
		}
		_, _ = os.Stdout.Write(append(buf, '\n'))
		return
	}
	fmt.Printf("%s\t%s\t%s\t%s\t%s\t%s\n",
		e.OccurredAt.UTC().Format(time.RFC3339),
		e.Cell, e.Verb, eventActor(e), e.AccountID,
		tabSafe(safeText(string(e.Metadata))))
}

// eventActor renders "actor_kind:actor_id" (or bare kind for
// non-principal actors like system/control_plane).
func eventActor(e client.AdminEvent) string {
	if e.ActorID != "" {
		return e.ActorKind + ":" + safeText(e.ActorID)
	}
	return e.ActorKind
}

// degradedCellCount counts cells that failed to answer a fan-out.
func degradedCellCount(cells []client.AdminCellStatus) int {
	n := 0
	for _, c := range cells {
		if c.Status != "ok" {
			n++
		}
	}
	return n
}

// reportDegradedCells prints the shared partial-failure footer.
func reportDegradedCells(cells []client.AdminCellStatus) {
	degraded := degradedCellCount(cells)
	if degraded == 0 {
		return
	}
	fmt.Fprintf(os.Stderr, "\n%d of %d cells reported an error or timeout:\n", degraded, len(cells))
	for _, c := range cells {
		if c.Status != "ok" {
			fmt.Fprintf(os.Stderr, "  %s: %s (%s)\n", c.Name, c.Status, c.Error)
		}
	}
}
