package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/memorycurator"
	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

const memoryCurateAutoUsage = "usage: witself memory curate auto enable|disable|status|run|wake|service ..."

const (
	automaticCuratorSupervisorMaxPasses = 4
	automaticCuratorSupervisorTimeout   = 45 * time.Minute
)

func memoryCurateAuto(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, memoryCurateAutoUsage)
		return 2
	}
	switch args[0] {
	case "enable":
		return memoryCurateAutoEnable(args[1:])
	case "disable":
		return memoryCurateAutoDisable(args[1:])
	case "status", "show":
		return memoryCurateAutoStatus(args[1:])
	case "run":
		return memoryCurateAutoRun(args[1:])
	case "wake":
		return memoryCurateAutoWake(args[1:])
	case "service":
		return memoryCurateAutoService(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "witself memory curate auto: unknown subcommand %q\n", args[0])
		return 2
	}
}

func memoryCurateAutoEnable(args []string) int {
	fs := flag.NewFlagSet("memory curate auto enable", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	runtimeFlag := fs.String("runtime", "", "installed runtime binding")
	providerFlag := fs.String("provider", "", "explicit native provider: codex, claude-code, grok-build, or cursor")
	providerPathFlag := fs.String("provider-path", "", "optional native provider executable path")
	model := fs.String("model", "", "optional native provider model")
	policy := fs.String("policy", string(memorycurator.ApplyPolicyPreview), "preview or apply")
	yes := fs.Bool("yes", false, "confirm a standing automatic apply policy")
	allowTranscript := fs.Bool("allow-transcript-content", false, "allow the selected provider to receive unclassified transcript content")
	debounce := fs.Duration("debounce", memorycurator.DefaultAutoDebounce, "quiet period after the newest terminal flush")
	minimumInterval := fs.Duration("minimum-interval", memorycurator.DefaultAutoMinimumInterval, "minimum time between automatic attempts")
	maxRuns := fs.Int("max-runs", memorycurator.DefaultAutoMaxRuns, "maximum bounded queue runs per wake")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || strings.TrimSpace(*runtimeFlag) == "" || strings.TrimSpace(*providerFlag) == "" {
		fmt.Fprintln(os.Stderr, "usage: witself memory curate auto enable --runtime RUNTIME --provider PROVIDER --allow-transcript-content [flags]")
		return 2
	}
	runtimeName, err := transcriptcapture.NormalizeRuntime(*runtimeFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 2
	}
	if !supportsTranscriptHooks(runtimeName) {
		fmt.Fprintf(os.Stderr, "witself: automatic curation is unsupported for %s because it has no transcript hooks\n", runtimeName)
		return 2
	}
	provider, err := parseCuratorNativeProvider(*providerFlag)
	if err != nil || provider == "" {
		if err == nil {
			err = errors.New("--provider is required")
		}
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 2
	}
	applyPolicy := memorycurator.ApplyPolicy(strings.TrimSpace(*policy))
	if applyPolicy != memorycurator.ApplyPolicyPreview && applyPolicy != memorycurator.ApplyPolicyApply {
		fmt.Fprintln(os.Stderr, "witself: --policy must be preview or apply")
		return 2
	}
	if (applyPolicy == memorycurator.ApplyPolicyApply) != *yes {
		fmt.Fprintln(os.Stderr, "witself: --policy apply and --yes must be supplied together")
		return 2
	}
	if !*allowTranscript {
		fmt.Fprintln(os.Stderr, "witself: --allow-transcript-content is required because transcript entries do not yet carry trustworthy sensitivity labels")
		return 2
	}
	if strings.TrimSpace(*model) != "" && !curatorNativeModelPattern.MatchString(strings.TrimSpace(*model)) {
		fmt.Fprintln(os.Stderr, "witself: --model is not a valid native provider model name")
		return 2
	}
	if *debounce%time.Second != 0 || *minimumInterval%time.Second != 0 {
		fmt.Fprintln(os.Stderr, "witself: automation durations must use whole seconds")
		return 2
	}

	binding, err := transcriptcapture.LoadConfig(runtimeName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: load %s integration: %v\n", runtimeName, err)
		return 1
	}
	if strings.TrimSpace(binding.AccountID) == "" || strings.TrimSpace(binding.RealmID) == "" || strings.TrimSpace(binding.AgentID) == "" {
		fmt.Fprintln(os.Stderr, "witself: automatic curation requires a current installed account, realm, and agent identity; reinstall the runtime binding")
		return 1
	}

	providerPath := strings.TrimSpace(*providerPathFlag)
	if providerPath != "" {
		providerPath, err = filepath.Abs(providerPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "witself: resolve provider path: %v\n", err)
			return 2
		}
	}
	probeContext, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	capability, err := (memorycurator.NativePlanner{
		Provider: provider, Path: providerPath, Model: strings.TrimSpace(*model),
	}).Probe(probeContext)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: probe automatic curator provider: %v\n", err)
		return 1
	}
	if !capability.Supported {
		fmt.Fprintf(os.Stderr, "witself: automatic curator provider %s is unavailable: %s\n", provider, capability.UnsupportedReason)
		return 1
	}

	ctx, cancelConnect := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelConnect()
	conn, err := connectAgent(ctx, binding.Account, binding.Realm, binding.Agent, binding.Endpoint, binding.TokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: connect automatic curator: %v\n", err)
		return 1
	}
	preflight, err := client.GetMemoryCurationPreflight(ctx, conn.Endpoint, conn.Token)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: preflight automatic curator: %v\n", err)
		return 1
	}
	if err := validateCuratorRunPreflight(preflight, applyPolicy == memorycurator.ApplyPolicyApply); err != nil {
		fmt.Fprintf(os.Stderr, "witself: preflight automatic curator: %v\n", err)
		return 1
	}
	expected := &curatorRunExpectedBinding{
		AccountID: binding.AccountID, RealmID: binding.RealmID, AgentID: binding.AgentID,
		AccessProfile: "full",
	}
	if err := validateCuratorRunExpectedBinding(preflight, expected); err != nil {
		fmt.Fprintf(os.Stderr, "witself: preflight automatic curator: %v\n", err)
		return 1
	}

	store, err := memorycurator.DefaultAutoStore(binding.AgentID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: initialize automatic curator: %v\n", err)
		return 1
	}
	config, err := store.Enable(memorycurator.AutoSettings{
		AccountID: binding.AccountID, RealmID: binding.RealmID, AgentID: binding.AgentID,
		Provider: provider, ProviderPath: capability.Executable, Model: strings.TrimSpace(*model),
		ApplyPolicy: applyPolicy, AllowTranscriptContent: true,
		Debounce: *debounce, MinimumInterval: *minimumInterval, MaxRuns: *maxRuns,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: enable automatic curator: %v\n", err)
		return 1
	}
	if *jsonOut {
		return printJSON(config)
	}
	fmt.Printf("enabled\tagent=%s\tprovider=%s\tpolicy=%s\tdebounce=%s\tminimum-interval=%s\tmax-runs=%d\n",
		config.AgentID, config.Provider, config.ApplyPolicy, config.Debounce(), config.MinimumInterval(), config.MaxRuns)
	return 0
}

func memoryCurateAutoDisable(args []string) int {
	parsed, code := parseMemoryCurateAutoRuntime("disable", args, false)
	if code != 0 {
		return code
	}
	binding, err := transcriptcapture.LoadConfig(parsed.Runtime)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: load %s integration: %v\n", parsed.Runtime, err)
		return 1
	}
	store, err := memorycurator.DefaultAutoStore(binding.AgentID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: initialize automatic curator: %v\n", err)
		return 1
	}
	if err := store.Disable(); err != nil {
		fmt.Fprintf(os.Stderr, "witself: disable automatic curator: %v\n", err)
		return 1
	}
	inspection, err := store.Inspect()
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: inspect automatic curator: %v\n", err)
		return 1
	}
	if parsed.JSON {
		return printJSON(inspection)
	}
	fmt.Printf("disabled\tagent=%s\tpending-wakes=%d\n", binding.AgentID, inspection.PendingWakeCount)
	return 0
}

func memoryCurateAutoStatus(args []string) int {
	parsed, code := parseMemoryCurateAutoRuntime("status", args, false)
	if code != 0 {
		return code
	}
	binding, err := transcriptcapture.LoadConfig(parsed.Runtime)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: load %s integration: %v\n", parsed.Runtime, err)
		return 1
	}
	store, err := memorycurator.DefaultAutoStore(binding.AgentID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: initialize automatic curator: %v\n", err)
		return 1
	}
	inspection, err := store.Inspect()
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: inspect automatic curator: %v\n", err)
		return 1
	}
	if parsed.JSON {
		return printJSON(inspection)
	}
	if !inspection.Configured {
		fmt.Printf("unconfigured\tagent=%s\tpending-wakes=%d\n", binding.AgentID, inspection.PendingWakeCount)
		return 0
	}
	fmt.Printf("%s\tagent=%s\tprovider=%s\tpolicy=%s\tstate=%s\tpending-wakes=%d\ttotal-runs=%d\n",
		map[bool]string{true: "enabled", false: "disabled"}[inspection.Config.Enabled],
		binding.AgentID, inspection.Config.Provider, inspection.Config.ApplyPolicy,
		inspection.Status.State, inspection.PendingWakeCount, inspection.Status.TotalRuns)
	return 0
}

func memoryCurateAutoRun(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return memoryCurateAutoRunContext(ctx, args, "")
}

func memoryCurateAutoWake(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return memoryCurateAutoRunContext(ctx, args, memorycurator.AutoWakeManualPoll)
}

func memoryCurateAutoRunContext(ctx context.Context, args []string, requestedWake memorycurator.AutoWakeReason) int {
	if ctx == nil {
		fmt.Fprintln(os.Stderr, "witself: automatic curator context is required")
		return 1
	}
	command := "run"
	allowSupervisor := true
	if requestedWake != "" {
		command, allowSupervisor = "wake", false
	}
	parsed, code := parseMemoryCurateAutoRuntime(command, args, allowSupervisor)
	if code != 0 {
		return code
	}
	runtimeName := parsed.Runtime
	binding, err := transcriptcapture.LoadConfig(runtimeName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: load %s integration: %v\n", runtimeName, err)
		return 1
	}
	store, err := memorycurator.DefaultAutoStore(binding.AgentID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: initialize automatic curator: %v\n", err)
		return 1
	}
	inspection, err := store.Inspect()
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: inspect automatic curator: %v\n", err)
		return 1
	}
	if !inspection.Configured || !inspection.Config.Enabled {
		if parsed.JSON {
			return printJSON(memorycurator.AutoRunResult{})
		}
		return 0
	}
	if inspection.Config.AccountID != binding.AccountID || inspection.Config.RealmID != binding.RealmID ||
		inspection.Config.AgentID != binding.AgentID {
		fmt.Fprintln(os.Stderr, "witself: automatic curator configuration does not match the installed runtime binding")
		return 1
	}
	wakeReason := requestedWake
	if parsed.Force {
		wakeReason = memorycurator.AutoWakeScheduledPoll
	}
	if wakeReason != "" {
		if _, err := store.RecordWake(wakeReason); err != nil {
			fmt.Fprintf(os.Stderr, "witself: record automatic curator wake: %v\n", err)
			return 1
		}
	}
	expected := &curatorRunExpectedBinding{
		AccountID: binding.AccountID, RealmID: binding.RealmID, AgentID: binding.AgentID,
		AccessProfile: "full",
	}
	var lastCurationOutcome memorycurator.AutoWorkOutcome
	work := func(workContext context.Context, autoConfig memorycurator.AutoConfig) (memorycurator.AutoWorkResult, error) {
		driverArgs := automaticCuratorDriverArgs(binding, runtimeName, autoConfig)
		result, code := memoryCurateRunForAutomation(workContext, driverArgs, expected)
		if code != 0 {
			return memorycurator.AutoWorkResult{}, memorycurator.NewAutoWorkError(
				memorycurator.AutoFailureCuration, errors.New("automatic curator driver failed"))
		}
		workResult, err := automaticCuratorWorkResult(result, autoConfig)
		if err == nil && (workResult.Outcome == memorycurator.AutoOutcomeApplied || workResult.Outcome == memorycurator.AutoOutcomePreviewed) {
			lastCurationOutcome = workResult.Outcome
		}
		return workResult, err
	}
	var runResult memorycurator.AutoRunResult
	if parsed.Supervise {
		runResult, err = runAutomaticCuratorSupervisor(ctx, store, work)
	} else {
		runResult, err = store.RunPending(ctx, work)
	}
	if errors.Is(err, memorycurator.ErrAutoDisabled) {
		return 0
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: run automatic curator: %v\n", err)
		return 1
	}
	if runResult.Outcome == memorycurator.AutoOutcomeNoWork && lastCurationOutcome != "" {
		runResult.Outcome = lastCurationOutcome
	}
	if parsed.Supervise && runResult.MoreWork && runResult.PendingWakeCount > 0 {
		if err := startBackgroundAutomaticCuratorIfPending(runtimeName, binding); err != nil {
			fmt.Fprintf(os.Stderr, "witself: continue automatic curator: %v\n", err)
			return 1
		}
	}
	if parsed.JSON {
		return printJSON(runResult)
	}
	if runResult.Attempted {
		fmt.Printf("automatic-curator\toutcome=%s\truns=%d\tpending-wakes=%d\n",
			runResult.Outcome, runResult.Runs, runResult.PendingWakeCount)
	}
	return 0
}

func automaticCuratorWorkResult(result memorycurator.Result, config memorycurator.AutoConfig) (memorycurator.AutoWorkResult, error) {
	if result.NoWork {
		return memorycurator.AutoWorkResult{Outcome: memorycurator.AutoOutcomeNoWork}, nil
	}
	if config.ApplyPolicy == memorycurator.ApplyPolicyApply {
		if result.Apply == nil && !automaticCuratorRecoveredApply(result) {
			return memorycurator.AutoWorkResult{}, memorycurator.NewAutoWorkError(
				memorycurator.AutoFailureContract, errors.New("automatic curator returned no apply receipt"))
		}
		return memorycurator.AutoWorkResult{Outcome: memorycurator.AutoOutcomeApplied, MoreWork: true}, nil
	}
	if result.Abandon == nil {
		return memorycurator.AutoWorkResult{}, memorycurator.NewAutoWorkError(
			memorycurator.AutoFailureContract, errors.New("automatic curator returned no preview completion"))
	}
	return memorycurator.AutoWorkResult{Outcome: memorycurator.AutoOutcomePreviewed, MoreWork: true}, nil
}

func automaticCuratorRecoveredApply(result memorycurator.Result) bool {
	return result.Run != nil && result.Run.State == "applied" &&
		strings.TrimSpace(result.Run.ApplyReceiptID) != ""
}

// runAutomaticCuratorSupervisor lets one detached, value-free invocation
// survive bounded transient failures and drain more than one MaxRuns batch.
// RunPending owns all persisted timing and wake acknowledgement decisions;
// this loop only asks it to continue while its durable state says that is safe.
func runAutomaticCuratorSupervisor(
	ctx context.Context,
	store memorycurator.AutoStore,
	work memorycurator.AutoWorkFunc,
) (memorycurator.AutoRunResult, error) {
	ctx, cancel := context.WithTimeout(ctx, automaticCuratorSupervisorTimeout)
	defer cancel()

	var aggregate memorycurator.AutoRunResult
	var lastErr error
	for pass := 0; pass < automaticCuratorSupervisorMaxPasses; pass++ {
		result, err := store.RunPending(ctx, work)
		aggregate.Acquired = aggregate.Acquired || result.Acquired
		aggregate.Attempted = aggregate.Attempted || result.Attempted
		aggregate.Runs += result.Runs
		aggregate.PendingWakeCount = result.PendingWakeCount
		aggregate.MoreWork = result.MoreWork
		aggregate.NextEligibleAt = result.NextEligibleAt
		if result.Outcome != "" && (result.Outcome != memorycurator.AutoOutcomeNoWork || aggregate.Outcome == "") {
			aggregate.Outcome = result.Outcome
		}
		if err == nil {
			lastErr = nil
			if !result.MoreWork || result.PendingWakeCount == 0 || !result.Acquired {
				return aggregate, nil
			}
			continue
		}
		if ctx.Err() != nil || errors.Is(err, memorycurator.ErrAutoDisabled) {
			return aggregate, err
		}
		lastErr = err
		inspection, inspectErr := store.Inspect()
		if inspectErr != nil {
			return aggregate, errors.Join(err, inspectErr)
		}
		if !inspection.Configured || !inspection.Config.Enabled || inspection.PendingWakeCount == 0 ||
			!automaticCuratorFailureRetryable(inspection.Status.LastFailureCode) ||
			inspection.Status.RetryNotBefore == nil {
			return aggregate, err
		}
		// The next RunPending call sleeps until the persisted retry_not_before
		// (and any longer debounce/minimum interval). This is intentionally not
		// a local timer so restart and cancellation retain one source of truth.
	}
	return aggregate, lastErr
}

func automaticCuratorFailureRetryable(code string) bool {
	switch code {
	case memorycurator.AutoFailureWorker, memorycurator.AutoFailurePreflight,
		memorycurator.AutoFailureProviderProbe, memorycurator.AutoFailureCuration:
		return true
	default:
		return false
	}
}

type memoryCurateAutoRuntimeFlags struct {
	Runtime   string
	JSON      bool
	Force     bool
	Supervise bool
}

func parseMemoryCurateAutoRuntime(command string, args []string, allowSupervisor bool) (memoryCurateAutoRuntimeFlags, int) {
	fs := flag.NewFlagSet("memory curate auto "+command, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	runtimeFlag := fs.String("runtime", "", "installed runtime binding")
	jsonOut := jsonFlag(fs)
	var force, supervise *bool
	if allowSupervisor {
		force = fs.Bool("force", false, "record a value-free scheduled wake before running")
		supervise = fs.Bool("supervise", false, "internal detached retry and drain supervisor")
	}
	if err := fs.Parse(args); err != nil {
		return memoryCurateAutoRuntimeFlags{}, 2
	}
	if fs.NArg() != 0 || strings.TrimSpace(*runtimeFlag) == "" {
		fmt.Fprintf(os.Stderr, "usage: witself memory curate auto %s --runtime RUNTIME\n", command)
		return memoryCurateAutoRuntimeFlags{}, 2
	}
	runtimeName, err := transcriptcapture.NormalizeRuntime(*runtimeFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return memoryCurateAutoRuntimeFlags{}, 2
	}
	if !supportsTranscriptHooks(runtimeName) {
		fmt.Fprintf(os.Stderr, "witself: automatic curation is unsupported for %s because it has no transcript hooks\n", runtimeName)
		return memoryCurateAutoRuntimeFlags{}, 2
	}
	result := memoryCurateAutoRuntimeFlags{Runtime: runtimeName, JSON: *jsonOut}
	if allowSupervisor {
		result.Force, result.Supervise = *force, *supervise
	}
	return result, 0
}

func automaticCuratorDriverArgs(binding transcriptcapture.Config, runtimeName string, config memorycurator.AutoConfig) []string {
	args := []string{
		"--account", binding.Account, "--realm", binding.Realm, "--agent", binding.Agent,
		"--provider", string(config.Provider), "--provider-path", config.ProviderPath,
		"--client-runtime", runtimeName,
		"--client-recipe", "witself-auto-curator", "--client-recipe-version", "v1",
		"--json",
	}
	if binding.Endpoint != "" {
		args = append(args, "--endpoint", binding.Endpoint)
	}
	if binding.TokenFile != "" {
		args = append(args, "--token-file", binding.TokenFile)
	}
	if config.Model != "" {
		args = append(args, "--client-model", config.Model)
	}
	if config.ApplyPolicy == memorycurator.ApplyPolicyApply {
		args = append(args, "--apply", "--yes")
	}
	return args
}
