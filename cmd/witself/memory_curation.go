package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/client"
)

const memoryCurateUsage = "usage: witself memory curate request|requests|run|auto|start|renew|show|plan|plan-get|apply|cancel|abandon|rollback|status ..."

func memoryCurate(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, memoryCurateUsage)
		return 2
	}
	switch args[0] {
	case "request":
		return memoryCurateRequest(args[1:])
	case "requests", "list":
		return memoryCurateRequests(args[1:])
	case "run":
		return memoryCurateRun(args[1:])
	case "auto":
		return memoryCurateAuto(args[1:])
	case "start":
		return memoryCurateStart(args[1:])
	case "renew":
		return memoryCurateRenew(args[1:])
	case "show", "get":
		return memoryCurateShow(args[1:])
	case "plan":
		return memoryCuratePlan(args[1:])
	case "plan-get", "accepted-plan":
		return memoryCuratePlanGet(args[1:])
	case "apply":
		return memoryCurateApply(args[1:])
	case "cancel", "abandon":
		return memoryCurateFinish(args[0], args[1:])
	case "rollback":
		return memoryCurateRollback(args[1:])
	case "status":
		return memoryCurateStatus(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "witself memory curate: unknown subcommand %q\n", args[0])
		return 2
	}
}

func memoryCurateRequest(args []string) int {
	fs := flag.NewFlagSet("memory curate request", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account, realm, agent, endpoint, tokenFile := factConnectionFlags(fs)
	var sources, states csvListFlag
	fs.Var(&sources, "source", "source kind: memory, evidence, or transcript (repeatable or comma-separated)")
	fs.Var(&states, "memory-state", "included memory state (repeatable or comma-separated)")
	includeSensitive := fs.Bool("include-sensitive", false, "allow sensitive inputs in this run")
	maxMemories := fs.Int("max-memories", 0, "maximum memory inputs")
	maxEvidence := fs.Int("max-evidence", 0, "maximum evidence inputs")
	maxTranscript := fs.Int("max-transcript-entries", 0, "maximum transcript entries")
	coalescingKey := fs.String("coalescing-key", "manual", "stable key used to coalesce equivalent open work")
	triggerReason := fs.String("trigger-reason", "manual_refine", "bounded machine-readable trigger reason")
	triggerGeneration := fs.Int64("trigger-generation", 0, "optional external lower bound for the owner generation")
	priority := fs.Int("priority", 0, "queue priority")
	dueAtRaw := fs.String("due-at", "", "optional RFC3339 earliest claim time")
	maxAttempts := fs.Int("max-attempts", 0, "maximum claim attempts")
	idempotencyKey := fs.String("idempotency-key", "", "fresh retry key for this logical request")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || strings.TrimSpace(*coalescingKey) == "" ||
		strings.TrimSpace(*triggerReason) == "" || strings.TrimSpace(*idempotencyKey) == "" {
		fmt.Fprintln(os.Stderr, "usage: witself memory curate request --idempotency-key KEY [flags]")
		return 2
	}
	var dueAt *time.Time
	if raw := strings.TrimSpace(*dueAtRaw); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			fmt.Fprintln(os.Stderr, "witself: --due-at must be RFC3339")
			return 2
		}
		dueAt = &parsed
	}
	ctx := context.Background()
	conn, err := connectAgent(ctx, *account, *realm, *agent, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	result, err := client.RequestMemoryCuration(ctx, conn.Endpoint, conn.Token, client.RequestMemoryCurationInput{
		Scope: client.MemoryCurationScope{
			Sources: []string(sources), MemoryStates: []string(states),
			IncludeSensitive: *includeSensitive, MaxMemories: *maxMemories,
			MaxEvidence: *maxEvidence, MaxTranscriptEntries: *maxTranscript,
		},
		CoalescingKey: strings.TrimSpace(*coalescingKey),
		TriggerReason: strings.TrimSpace(*triggerReason), TriggerGeneration: *triggerGeneration,
		Priority: *priority, DueAt: dueAt, MaxAttempts: *maxAttempts,
		IdempotencyKey: strings.TrimSpace(*idempotencyKey),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: request memory curation: %v\n", err)
		return 1
	}
	if *jsonOut {
		return printJSON(result)
	}
	fmt.Printf("requested\trequest=%s\tgeneration=%d\tstate=%s\tdue=%s\treplayed=%t\n",
		result.Request.ID, result.Request.RequestGeneration, result.Request.State,
		result.Request.DueAt.Format(time.RFC3339), result.Receipt.Replayed)
	return 0
}

func memoryCurateRequests(args []string) int {
	fs := flag.NewFlagSet("memory curate requests", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account, realm, agent, endpoint, tokenFile := factConnectionFlags(fs)
	state := fs.String("state", "", "request lifecycle state; empty lists due work")
	limit := fs.Int("limit", 50, "maximum requests in this page")
	cursor := fs.String("cursor", "", "opaque continuation cursor")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || *limit < 1 || *limit > 200 {
		fmt.Fprintln(os.Stderr, "usage: witself memory curate requests [--state STATE] [--limit N] [--cursor CURSOR]")
		return 2
	}
	ctx := context.Background()
	conn, err := connectAgent(ctx, *account, *realm, *agent, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	page, err := client.ListMemoryCurationRequests(ctx, conn.Endpoint, conn.Token,
		client.MemoryCurationRequestListOptions{State: strings.TrimSpace(*state), Limit: *limit, Cursor: strings.TrimSpace(*cursor)})
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: list memory curation requests: %v\n", err)
		return 1
	}
	if *jsonOut {
		return printJSON(page)
	}
	for _, request := range page.Requests {
		fmt.Printf("%s\t%s\tgeneration=%d\tdue=%s\tattempt=%d/%d\n", request.ID,
			request.State, request.RequestGeneration, request.DueAt.Format(time.RFC3339),
			request.AttemptCount, request.MaxAttempts)
	}
	if page.NextCursor != "" {
		fmt.Printf("next-cursor\t%s\n", page.NextCursor)
	}
	return 0
}

func memoryCurateStart(args []string) int {
	requestID, args := memoryLeadingID(args)
	fs := flag.NewFlagSet("memory curate start", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account, realm, agent, endpoint, tokenFile := factConnectionFlags(fs)
	requestFlag := fs.String("request", "", "curation request id")
	maxMemories := fs.Int("max-memories", 0, "per-run memory cap")
	maxEvidence := fs.Int("max-evidence", 0, "per-run evidence cap")
	maxTranscript := fs.Int("max-transcript-entries", 0, "per-run transcript cap")
	leaseSeconds := fs.Int64("lease-seconds", 300, "initial lease duration in seconds")
	budgetsFile := fs.String("budgets-file", "", "optional JSON object with client-side budgets ('-' means stdin)")
	runtimeName, model, recipe, recipeVersion := memoryClientFlags(fs)
	idempotencyKey := fs.String("idempotency-key", "", "fresh retry key for this start")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if requestID == "" {
		requestID = strings.TrimSpace(*requestFlag)
	} else if strings.TrimSpace(*requestFlag) != "" && requestID != strings.TrimSpace(*requestFlag) {
		fmt.Fprintln(os.Stderr, "witself: positional request id and --request disagree")
		return 2
	}
	if fs.NArg() != 0 || requestID == "" || *leaseSeconds < 1 || strings.TrimSpace(*idempotencyKey) == "" {
		fmt.Fprintln(os.Stderr, "usage: witself memory curate start --request REQ_ID --idempotency-key KEY [flags]")
		return 2
	}
	budgets, err := readOptionalJSONObject(*budgetsFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: budgets: %v\n", err)
		return 2
	}
	ctx := context.Background()
	conn, err := connectAgent(ctx, *account, *realm, *agent, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	result, err := client.StartMemoryCuration(ctx, conn.Endpoint, conn.Token, client.StartMemoryCurationInput{
		RequestID:     requestID,
		Caps:          client.MemoryCurationInputCaps{MaxMemories: *maxMemories, MaxEvidence: *maxEvidence, MaxTranscriptEntries: *maxTranscript},
		LeaseDuration: time.Duration(*leaseSeconds) * time.Second,
		Client:        memoryClientProvenance(*runtimeName, *model, *recipe, *recipeVersion),
		Budgets:       budgets, IdempotencyKey: strings.TrimSpace(*idempotencyKey),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: start memory curation: %v\n", err)
		return 1
	}
	if *jsonOut {
		return printJSON(result)
	}
	fmt.Printf("started\trun=%s\trequest=%s\tfence=%d\tlease=%s\tinputs=%d\tfirst-cursor=%s\treplayed=%t\n",
		result.Run.ID, result.Request.ID, result.Run.FencingGeneration,
		formatOptionalTime(result.Run.LeaseExpiresAt), result.Run.InputCount,
		result.FirstInputCursor, result.Receipt.Replayed)
	return 0
}

func memoryCurateRenew(args []string) int {
	runID, args := memoryLeadingID(args)
	fs := flag.NewFlagSet("memory curate renew", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account, realm, agent, endpoint, tokenFile := factConnectionFlags(fs)
	fence := fs.Int64("fence", 0, "current fencing generation")
	extensionSeconds := fs.Int64("extension-seconds", 300, "requested lease extension in seconds")
	idempotencyKey := fs.String("idempotency-key", "", "fresh retry key for this renewal")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if runID == "" && fs.NArg() == 1 {
		runID = strings.TrimSpace(fs.Arg(0))
	} else if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: witself memory curate renew RUN_ID --fence N --idempotency-key KEY")
		return 2
	}
	if runID == "" || *fence < 1 || *extensionSeconds < 1 || strings.TrimSpace(*idempotencyKey) == "" {
		fmt.Fprintln(os.Stderr, "usage: witself memory curate renew RUN_ID --fence N --idempotency-key KEY")
		return 2
	}
	ctx := context.Background()
	conn, err := connectAgent(ctx, *account, *realm, *agent, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	result, err := client.RenewMemoryCuration(ctx, conn.Endpoint, conn.Token, client.RenewMemoryCurationInput{
		RunID: runID, FencingGeneration: *fence,
		Extension:      time.Duration(*extensionSeconds) * time.Second,
		IdempotencyKey: strings.TrimSpace(*idempotencyKey),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: renew memory curation: %v\n", err)
		return 1
	}
	if *jsonOut {
		return printJSON(result)
	}
	fmt.Printf("renewed\trun=%s\tfence=%d\tlease=%s\treplayed=%t\n", result.Run.ID,
		result.Run.FencingGeneration, formatOptionalTime(result.Run.LeaseExpiresAt), result.Receipt.Replayed)
	return 0
}

func memoryCurateShow(args []string) int {
	runID, args := memoryLeadingID(args)
	fs := flag.NewFlagSet("memory curate show", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account, realm, agent, endpoint, tokenFile := factConnectionFlags(fs)
	fence := fs.Int64("fence", 0, "current fencing generation")
	cursor := fs.String("cursor", "", "opaque input cursor returned by start or the prior page")
	limit := fs.Int("limit", 50, "maximum materialized inputs in this page")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if runID == "" && fs.NArg() == 1 {
		runID = strings.TrimSpace(fs.Arg(0))
	} else if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: witself memory curate show RUN_ID --fence N [--cursor CURSOR]")
		return 2
	}
	if runID == "" || *fence < 1 || *limit < 1 || *limit > 200 {
		fmt.Fprintln(os.Stderr, "usage: witself memory curate show RUN_ID --fence N [--cursor CURSOR]")
		return 2
	}
	ctx := context.Background()
	conn, err := connectAgent(ctx, *account, *realm, *agent, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	page, err := client.GetMemoryCurationRunInputs(ctx, conn.Endpoint, conn.Token, runID,
		*fence, strings.TrimSpace(*cursor), *limit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: get memory curation inputs: %v\n", err)
		return 1
	}
	if *jsonOut {
		return printJSON(page)
	}
	for _, input := range page.Inputs {
		coverage := ""
		if input.CoverageCounts != nil {
			coverage = fmt.Sprintf("\tcoverage=calls:%d,results:%d,signal:%d",
				input.CoverageCounts.ToolCalls, input.CoverageCounts.ToolResults,
				input.CoverageCounts.Signal)
		}
		fmt.Printf("%d\t%s\tmemory=%s@%d\tevidence=%s\ttranscript=%s:%d-%d%s\n",
			input.Ordinal, input.Kind, input.MemoryID, input.MemoryVersion, input.EvidenceID,
			input.TranscriptID, input.SequenceFrom, input.SequenceUntil, coverage)
	}
	if page.NextCursor != "" {
		fmt.Printf("next-cursor\t%s\n", page.NextCursor)
	}
	return 0
}

func memoryCuratePlan(args []string) int {
	runID, args := memoryLeadingID(args)
	fs := flag.NewFlagSet("memory curate plan", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account, realm, agent, endpoint, tokenFile := factConnectionFlags(fs)
	fence := fs.Int64("fence", 0, "current fencing generation")
	planFile := fs.String("file", "", "strict witself.memory-plan.v1 JSON draft ('-' means stdin)")
	idempotencyKey := fs.String("idempotency-key", "", "fresh retry key for this plan")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if runID == "" && fs.NArg() == 1 {
		runID = strings.TrimSpace(fs.Arg(0))
	} else if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: witself memory curate plan RUN_ID --fence N --file PLAN.json --idempotency-key KEY")
		return 2
	}
	if runID == "" || *fence < 1 || strings.TrimSpace(*planFile) == "" || strings.TrimSpace(*idempotencyKey) == "" {
		fmt.Fprintln(os.Stderr, "usage: witself memory curate plan RUN_ID --fence N --file PLAN.json --idempotency-key KEY")
		return 2
	}
	raw, err := readCurationJSONFile(*planFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: read plan: %v\n", err)
		return 2
	}
	ctx := context.Background()
	conn, err := connectAgent(ctx, *account, *realm, *agent, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	result, err := client.PlanMemoryCuration(ctx, conn.Endpoint, conn.Token, client.PlanMemoryCurationInput{
		RunID: runID, FencingGeneration: *fence, Draft: raw,
		IdempotencyKey: strings.TrimSpace(*idempotencyKey),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: plan memory curation: %v\n", err)
		return 1
	}
	if *jsonOut {
		return printJSON(result)
	}
	fmt.Printf("planned\trun=%s\trevision=%d\thash=%s\tactions=%d\tcreates=%d\twrites=%d\treplayed=%t\n",
		result.Run.ID, result.Receipt.PlanRevision, result.Receipt.PlanHash,
		result.Preview.ActionCount, result.Preview.NewMemories,
		result.Preview.MemoryVersionWrites, result.Receipt.Replayed)
	for _, allocated := range result.PreallocatedMemoryIDs {
		fmt.Printf("allocated\tlocal-ref=%s\tmemory=%s\n", allocated.LocalRef, allocated.MemoryID)
	}
	return 0
}

func memoryCuratePlanGet(args []string) int {
	runID, args := memoryLeadingID(args)
	fs := flag.NewFlagSet("memory curate plan-get", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account, realm, agent, endpoint, tokenFile := factConnectionFlags(fs)
	fence := fs.Int64("fence", 0, "current fencing generation")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if runID == "" && fs.NArg() == 1 {
		runID = strings.TrimSpace(fs.Arg(0))
	} else if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: witself memory curate plan-get RUN_ID --fence N")
		return 2
	}
	if runID == "" || *fence < 1 {
		fmt.Fprintln(os.Stderr, "usage: witself memory curate plan-get RUN_ID --fence N")
		return 2
	}
	ctx := context.Background()
	conn, err := connectAgent(ctx, *account, *realm, *agent, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	result, err := client.GetMemoryCurationPlan(ctx, conn.Endpoint, conn.Token, runID, *fence)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: get accepted memory curation plan: %v\n", err)
		return 1
	}
	if *jsonOut {
		return printJSON(result)
	}
	fmt.Printf("accepted-plan\trun=%s\tfence=%d\trevision=%d\thash=%s\tactions=%d\tcreates=%d\twrites=%d\n",
		result.Run.ID, result.Run.FencingGeneration, result.Run.PlanRevision,
		result.Run.PlanHash, result.Preview.ActionCount, result.Preview.NewMemories,
		result.Preview.MemoryVersionWrites)
	fmt.Println(string(result.Plan))
	for _, allocated := range result.PreallocatedMemoryIDs {
		fmt.Printf("allocated\tlocal-ref=%s\tmemory=%s\n", allocated.LocalRef, allocated.MemoryID)
	}
	return 0
}

func memoryCurateApply(args []string) int {
	runID, args := memoryLeadingID(args)
	fs := flag.NewFlagSet("memory curate apply", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account, realm, agent, endpoint, tokenFile := factConnectionFlags(fs)
	fence := fs.Int64("fence", 0, "current fencing generation")
	planRevision := fs.Int64("plan-revision", 0, "accepted plan revision")
	planHash := fs.String("plan-hash", "", "accepted lowercase SHA-256 plan hash")
	idempotencyKey := fs.String("idempotency-key", "", "fresh retry key for this apply")
	yes := fs.Bool("yes", false, "confirm applying the exact accepted plan")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if runID == "" && fs.NArg() == 1 {
		runID = strings.TrimSpace(fs.Arg(0))
	} else if fs.NArg() != 0 {
		memoryCurateApplyUsage()
		return 2
	}
	if runID == "" || *fence < 1 || *planRevision < 1 ||
		!factCandidateRevisionPattern.MatchString(strings.TrimSpace(*planHash)) ||
		strings.TrimSpace(*idempotencyKey) == "" || !*yes {
		memoryCurateApplyUsage()
		return 2
	}
	ctx := context.Background()
	conn, err := connectAgent(ctx, *account, *realm, *agent, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	result, err := client.ApplyMemoryCuration(ctx, conn.Endpoint, conn.Token, client.ApplyMemoryCurationInput{
		RunID: runID, FencingGeneration: *fence, PlanRevision: *planRevision,
		PlanHash: strings.TrimSpace(*planHash), IdempotencyKey: strings.TrimSpace(*idempotencyKey),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: apply memory curation: %v\n", err)
		return 1
	}
	if *jsonOut {
		return printJSON(result)
	}
	fmt.Printf("applied\trun=%s\treceipt=%s\tactions=%d\tcursors=%d\tfollow-up=%s\treplayed=%t\n",
		result.Run.ID, result.Receipt.ID, len(result.Receipt.ActionResults),
		len(result.Receipt.CursorIntervals), result.Receipt.FollowUpRequestID, result.Receipt.Replayed)
	return 0
}

func memoryCurateApplyUsage() {
	fmt.Fprintln(os.Stderr, "usage: witself memory curate apply RUN_ID --fence N --plan-revision N --plan-hash SHA256 --idempotency-key KEY --yes")
}

func memoryCurateFinish(action string, args []string) int {
	runID, args := memoryLeadingID(args)
	fs := flag.NewFlagSet("memory curate "+action, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account, realm, agent, endpoint, tokenFile := factConnectionFlags(fs)
	fence := fs.Int64("fence", 0, "current fencing generation")
	reason := fs.String("reason", "", "bounded terminal reason")
	idempotencyKey := fs.String("idempotency-key", "", "fresh retry key for this operation")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if runID == "" && fs.NArg() == 1 {
		runID = strings.TrimSpace(fs.Arg(0))
	} else if fs.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "usage: witself memory curate %s RUN_ID --fence N --idempotency-key KEY\n", action)
		return 2
	}
	if runID == "" || *fence < 1 || strings.TrimSpace(*idempotencyKey) == "" {
		fmt.Fprintf(os.Stderr, "usage: witself memory curate %s RUN_ID --fence N --idempotency-key KEY\n", action)
		return 2
	}
	ctx := context.Background()
	conn, err := connectAgent(ctx, *account, *realm, *agent, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	in := client.FinishMemoryCurationInput{RunID: runID, FencingGeneration: *fence,
		Reason: strings.TrimSpace(*reason), IdempotencyKey: strings.TrimSpace(*idempotencyKey)}
	var result *client.FinishMemoryCurationResult
	if action == "cancel" {
		result, err = client.CancelMemoryCuration(ctx, conn.Endpoint, conn.Token, in)
	} else {
		result, err = client.AbandonMemoryCuration(ctx, conn.Endpoint, conn.Token, in)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %s memory curation: %v\n", action, err)
		return 1
	}
	if *jsonOut {
		return printJSON(result)
	}
	fmt.Printf("%s\trun=%s\tstate=%s\treplayed=%t\n", action, result.Run.ID,
		result.Run.State, result.Receipt.Replayed)
	return 0
}

func memoryCurateRollback(args []string) int {
	runID, args := memoryLeadingID(args)
	fs := flag.NewFlagSet("memory curate rollback", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account, realm, agent, endpoint, tokenFile := factConnectionFlags(fs)
	applyReceipt := fs.String("apply-receipt", "", "exact apply receipt id")
	expectedHeadsFile := fs.String("expected-heads", "", "JSON array of exact apply-produced heads ('-' means stdin)")
	reason := fs.String("reason", "", "bounded rollback reason")
	idempotencyKey := fs.String("idempotency-key", "", "fresh retry key for this rollback")
	yes := fs.Bool("yes", false, "confirm the guarded compensating rollback")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if runID == "" && fs.NArg() == 1 {
		runID = strings.TrimSpace(fs.Arg(0))
	} else if fs.NArg() != 0 {
		memoryCurateRollbackUsage()
		return 2
	}
	if runID == "" || strings.TrimSpace(*applyReceipt) == "" ||
		strings.TrimSpace(*expectedHeadsFile) == "" || strings.TrimSpace(*idempotencyKey) == "" || !*yes {
		memoryCurateRollbackUsage()
		return 2
	}
	heads, err := readMemoryVersionReferences(*expectedHeadsFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: read expected heads: %v\n", err)
		return 2
	}
	ctx := context.Background()
	conn, err := connectAgent(ctx, *account, *realm, *agent, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	result, err := client.RollbackMemoryCuration(ctx, conn.Endpoint, conn.Token, client.RollbackMemoryCurationInput{
		RunID: runID, ApplyReceiptID: strings.TrimSpace(*applyReceipt),
		ExpectedProducedHeads: heads, Reason: strings.TrimSpace(*reason),
		IdempotencyKey: strings.TrimSpace(*idempotencyKey),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: rollback memory curation: %v\n", err)
		return 1
	}
	if *jsonOut {
		return printJSON(result)
	}
	fmt.Printf("rolled-back\trun=%s\treceipt=%s\treplay-request=%s\treplay-generation=%d\treplayed=%t\n",
		result.Run.ID, result.Receipt.ID, result.Receipt.ReplayRequestID,
		result.Receipt.ReplayGeneration, result.Receipt.Replayed)
	return 0
}

func memoryCurateRollbackUsage() {
	fmt.Fprintln(os.Stderr, "usage: witself memory curate rollback RUN_ID --apply-receipt RECEIPT --expected-heads heads.json --idempotency-key KEY --yes")
}

func memoryCurateStatus(args []string) int {
	runID, args := memoryLeadingID(args)
	fs := flag.NewFlagSet("memory curate status", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account, realm, agent, endpoint, tokenFile := factConnectionFlags(fs)
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if runID == "" && fs.NArg() == 1 {
		runID = strings.TrimSpace(fs.Arg(0))
	} else if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: witself memory curate status [RUN_ID]")
		return 2
	}
	ctx := context.Background()
	conn, err := connectAgent(ctx, *account, *realm, *agent, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	status, err := client.GetMemoryCurationStatus(ctx, conn.Endpoint, conn.Token, runID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: get memory curation status: %v\n", err)
		return 1
	}
	if *jsonOut {
		return printJSON(status)
	}
	fmt.Printf("lane\trequest-generation=%d\tfence=%d\tactive-run=%s\n",
		status.Lane.RequestGeneration, status.Lane.FencingGeneration, status.Lane.ActiveRunID)
	if status.Request != nil {
		fmt.Printf("request\t%s\tstate=%s\tgeneration=%d\n", status.Request.ID,
			status.Request.State, status.Request.RequestGeneration)
	}
	if status.Run != nil {
		fmt.Printf("run\t%s\tstate=%s\tfence=%d\tlease=%s\n", status.Run.ID,
			status.Run.State, status.Run.FencingGeneration, formatOptionalTime(status.Run.LeaseExpiresAt))
	}
	return 0
}

func readCurationJSONFile(path string) (json.RawMessage, error) {
	var raw []byte
	var err error
	if strings.TrimSpace(path) == "-" {
		raw, err = io.ReadAll(os.Stdin)
	} else {
		raw, err = os.ReadFile(strings.TrimSpace(path))
	}
	if err != nil {
		return nil, err
	}
	if !json.Valid(raw) {
		return nil, fmt.Errorf("file is not valid JSON")
	}
	return json.RawMessage(raw), nil
}

func readOptionalJSONObject(path string) (json.RawMessage, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	raw, err := readCurationJSONFile(path)
	if err != nil {
		return nil, err
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, fmt.Errorf("file must contain one JSON object")
	}
	return raw, nil
}

func readMemoryVersionReferences(path string) ([]client.MemoryVersionReference, error) {
	raw, err := readCurationJSONFile(path)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	var out []client.MemoryVersionReference
	if err := decoder.Decode(&out); err != nil {
		return nil, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("multiple JSON values")
		}
		return nil, err
	}
	if out == nil {
		out = []client.MemoryVersionReference{}
	}
	return out, nil
}

func formatOptionalTime(value *time.Time) string {
	if value == nil {
		return ""
	}
	return value.Format(time.RFC3339)
}
