package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/memorycurator"
)

const curatorRunResultSchemaV1 = "witself.curator-run-result.v1"

var curatorRunPrimitives = []string{"create", "replace", "supersede", "relate", "propose_fact"}

var curatorNativeModelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/+@-]{0,127}$`)

type curatorRunSummary struct {
	Schema          string `json:"schema"`
	Status          string `json:"status"`
	LaunchID        string `json:"launch_id,omitempty"`
	Recovered       bool   `json:"recovered,omitempty"`
	RequestID       string `json:"request_id,omitempty"`
	RunID           string `json:"run_id,omitempty"`
	InputCount      int    `json:"input_count,omitempty"`
	PlanRevision    int64  `json:"plan_revision,omitempty"`
	PlanHash        string `json:"plan_hash,omitempty"`
	PlanReceiptID   string `json:"plan_receipt_id,omitempty"`
	ApplyReceiptID  string `json:"apply_receipt_id,omitempty"`
	PreviewRequeued bool   `json:"preview_requeued,omitempty"`
}

type curatorRunExpectedBinding struct {
	AccountID     string
	RealmID       string
	AgentID       string
	AccessProfile string
}

// memoryCurateRun is the trusted process boundary for client-side curation.
// The parent process retains the bearer token and drives the fenced protocol;
// the planner child receives only the frozen planner envelope on stdin.
func memoryCurateRun(args []string) int {
	return memoryCurateRunExecute(context.Background(), args, nil, nil, false)
}

func memoryCurateRunForAutomation(ctx context.Context, args []string, expected *curatorRunExpectedBinding) (memorycurator.Result, int) {
	var observed memorycurator.Result
	code := memoryCurateRunExecute(ctx, args, expected, func(result memorycurator.Result) {
		observed = result
	}, true)
	return observed, code
}

func memoryCurateRunExecute(
	ctx context.Context,
	args []string,
	expected *curatorRunExpectedBinding,
	observe func(memorycurator.Result),
	quiet bool,
) int {
	if ctx == nil {
		fmt.Fprintln(os.Stderr, "witself: memory curator context is required")
		return 1
	}
	fs := flag.NewFlagSet("memory curate run", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account, realm, agent, endpoint, tokenFile := factConnectionFlags(fs)
	requestID := fs.String("request", "", "optional due curation request id (default: oldest due request)")
	resumeID := fs.String("resume", "", "resume a value-free curator launch receipt")
	apply := fs.Bool("apply", false, "apply the accepted plan instead of previewing and requeuing")
	yes := fs.Bool("yes", false, "confirm --apply for this invocation")
	maxMemories := fs.Int("max-memories", 20, "maximum frozen memory inputs")
	maxEvidence := fs.Int("max-evidence", 50, "maximum frozen evidence inputs")
	maxTranscript := fs.Int("max-transcript-entries", 100, "maximum frozen transcript entries")
	pageSize := fs.Int("page-size", 50, "maximum inputs fetched per page")
	lease := fs.Duration("lease", 5*time.Minute, "server run lease duration")
	plannerTimeout := fs.Duration("planner-timeout", 4*time.Minute, "maximum planner command runtime")
	renewBefore := fs.Duration("renew-before", time.Minute, "renew the lease this long before expiry")
	maxActions := fs.Int("max-actions", 32, "maximum actions accepted from the planner")
	maxPlannerOutput := fs.Int("planner-max-output-bytes", memorycurator.DefaultMaxPlannerOutputBytes, "maximum planner stdout bytes")
	maxPlannerStderr := fs.Int("planner-max-stderr-bytes", memorycurator.DefaultMaxPlannerStderrBytes, "maximum planner stderr bytes")
	plannerDir := fs.String("planner-dir", "", "optional planner working directory")
	providerName := fs.String("provider", "", "native planner provider: codex, claude-code, grok-build, or cursor")
	providerPath := fs.String("provider-path", "", "optional native provider executable path")
	runtimeName, model, recipe, recipeVersion := memoryClientFlags(fs)
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	request := strings.TrimSpace(*requestID)
	resume := strings.TrimSpace(*resumeID)
	plannerArgv := fs.Args()
	provider, err := parseCuratorNativeProvider(*providerName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 2
	}
	if provider != "" && len(plannerArgv) != 0 {
		fmt.Fprintln(os.Stderr, "witself: --provider and a planner command are mutually exclusive")
		return 2
	}
	if provider == "" && (len(plannerArgv) == 0 || strings.TrimSpace(plannerArgv[0]) == "") {
		memoryCurateRunUsage()
		return 2
	}
	if provider == "" && flagWasPassed(fs, "provider-path") {
		fmt.Fprintln(os.Stderr, "witself: --provider-path requires --provider")
		return 2
	}
	if provider != "" && flagWasPassed(fs, "planner-dir") {
		fmt.Fprintln(os.Stderr, "witself: --planner-dir is valid only with an explicit planner command")
		return 2
	}
	if provider != "" && strings.TrimSpace(*model) != "" && !curatorNativeModelPattern.MatchString(strings.TrimSpace(*model)) {
		fmt.Fprintln(os.Stderr, "witself: --client-model is not a valid native provider model name")
		return 2
	}
	if request != "" && resume != "" {
		fmt.Fprintln(os.Stderr, "witself: --request and --resume are mutually exclusive")
		return 2
	}
	if *apply != *yes {
		fmt.Fprintln(os.Stderr, "witself: --apply and --yes must be supplied together")
		return 2
	}
	if resume != "" && curatorRunResumeOverrides(fs) {
		fmt.Fprintln(os.Stderr, "witself: --resume uses the persisted caps, timing, provenance, and request; those flags cannot be overridden")
		return 2
	}
	if *maxPlannerOutput < 1 || *maxPlannerOutput > memorycurator.DefaultMaxPlannerOutputBytes ||
		*maxPlannerStderr < 1 || *maxPlannerStderr > memorycurator.DefaultMaxPlannerStderrBytes {
		fmt.Fprintln(os.Stderr, "witself: planner stream limits are outside the supported range")
		return 2
	}
	runtime := strings.TrimSpace(*runtimeName)
	if runtime == "" && provider != "" {
		runtime = string(provider)
	}

	options := memorycurator.Options{
		RequestID: request, ApplyPolicy: memorycurator.ApplyPolicyPreview,
		ApproveApply: *yes, AllowSensitive: false,
		Caps: client.MemoryCurationInputCaps{
			MaxMemories: *maxMemories, MaxEvidence: *maxEvidence,
			MaxTranscriptEntries: *maxTranscript,
		},
		PageSize: *pageSize, MaximumActions: *maxActions,
		LeaseDuration: *lease, PlannerTimeout: *plannerTimeout, RenewBefore: *renewBefore,
		Client: memoryClientProvenance(runtime, strings.TrimSpace(*model),
			strings.TrimSpace(*recipe), strings.TrimSpace(*recipeVersion)),
	}
	if *apply {
		options.ApplyPolicy = memorycurator.ApplyPolicyApply
	}
	if err := validateCuratorRunOptions(options); err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 2
	}

	conn, err := connectAgent(ctx, *account, *realm, *agent, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	preflight, err := client.GetMemoryCurationPreflight(ctx, conn.Endpoint, conn.Token)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: preflight memory curator: %v\n", err)
		return 1
	}
	if err := validateCuratorRunPreflight(preflight, *apply); err != nil {
		fmt.Fprintf(os.Stderr, "witself: preflight memory curator: %v\n", err)
		return 1
	}
	if err := validateCuratorRunExpectedBinding(preflight, expected); err != nil {
		fmt.Fprintf(os.Stderr, "witself: preflight memory curator: %v\n", err)
		return 1
	}

	state, err := memorycurator.DefaultFileStateStore(preflight.Principal.AgentID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: initialize curator state: %v\n", err)
		return 1
	}
	if resume != "" {
		persisted, err := state.Load(resume)
		if err != nil {
			fmt.Fprintf(os.Stderr, "witself: resume curator launch: %v\n", err)
			return 1
		}
		wantPolicy := memorycurator.ApplyPolicyPreview
		if *apply {
			wantPolicy = memorycurator.ApplyPolicyApply
		}
		if persisted.ApplyPolicy != wantPolicy {
			fmt.Fprintf(os.Stderr, "witself: --resume launch uses %s policy; this invocation selected %s\n", persisted.ApplyPolicy, wantPolicy)
			return 2
		}
		if persisted.IncludesSensitive {
			fmt.Fprintln(os.Stderr, "witself: this curator driver never resumes runs containing sensitive inputs")
			return 1
		}
		options = curatorOptionsFromLaunchState(persisted, *yes)
	}
	if err := validateCuratorRunLimits(preflight, options, int64(*maxPlannerOutput)); err != nil {
		fmt.Fprintf(os.Stderr, "witself: preflight memory curator: %v\n", err)
		return 1
	}
	planner, err := initializeCuratorRunPlanner(ctx, provider, strings.TrimSpace(*providerPath), plannerArgv,
		strings.TrimSpace(*plannerDir), options.Client.Model, *maxPlannerOutput, *maxPlannerStderr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: initialize curator planner: %v\n", err)
		return 1
	}

	runner := memorycurator.Runner{
		API:     memorycurator.HTTPAPI{Endpoint: conn.Endpoint, Token: conn.Token},
		Planner: planner,
		State:   state,
	}
	var result memorycurator.Result
	if resume == "" {
		result, err = runner.Run(ctx, options)
	} else {
		result, err = runner.Resume(ctx, resume, options)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: run memory curator: %v\n", err)
		return 1
	}
	if observe != nil {
		observe(result)
	}
	if quiet {
		return 0
	}
	return printCuratorRunResult(result, *jsonOut)
}

func validateCuratorRunExpectedBinding(preflight *client.MemoryCurationPreflight, expected *curatorRunExpectedBinding) error {
	if expected == nil {
		return nil
	}
	if preflight == nil || strings.TrimSpace(expected.AccountID) == "" ||
		strings.TrimSpace(expected.RealmID) == "" || strings.TrimSpace(expected.AgentID) == "" {
		return errors.New("automatic curator requires a complete installed identity binding")
	}
	if preflight.Principal.AccountID != expected.AccountID ||
		preflight.Principal.RealmID != expected.RealmID ||
		preflight.Principal.AgentID != expected.AgentID {
		return errors.New("authenticated curator identity does not match the installed runtime binding")
	}
	if expected.AccessProfile != "" && preflight.Credential.AccessProfile != expected.AccessProfile {
		return fmt.Errorf("automatic curator requires %s credential profile", expected.AccessProfile)
	}
	return nil
}

func memoryCurateRunUsage() {
	fmt.Fprintln(os.Stderr, "usage: witself memory curate run [flags] (--provider codex|claude-code|grok-build|cursor [--provider-path PATH] | -- PLANNER [ARGS...])")
}

func parseCuratorNativeProvider(value string) (memorycurator.NativeProvider, error) {
	provider := memorycurator.NativeProvider(strings.TrimSpace(value))
	switch provider {
	case "":
		return "", nil
	case memorycurator.ProviderCodex, memorycurator.ProviderClaudeCode, memorycurator.ProviderGrokBuild, memorycurator.ProviderCursor:
		return provider, nil
	default:
		return "", fmt.Errorf("--provider must be codex, claude-code, grok-build, or cursor")
	}
}

func initializeCuratorRunPlanner(
	ctx context.Context,
	provider memorycurator.NativeProvider,
	providerPath string,
	plannerArgv []string,
	plannerDir string,
	model string,
	maxOutput int,
	maxStderr int,
) (memorycurator.Planner, error) {
	if provider == "" {
		return memorycurator.CommandPlanner{
			Path: plannerArgv[0], Args: append([]string(nil), plannerArgv[1:]...),
			Dir: plannerDir, MaxOutputBytes: maxOutput, MaxStderrBytes: maxStderr,
		}, nil
	}
	planner := memorycurator.NativePlanner{
		Provider: provider, Path: providerPath, Model: model,
		MaxOutputBytes: maxOutput, MaxStderrBytes: maxStderr,
	}
	// Probe only version/help before Runner can list or claim due work. A CLI
	// that does not advertise every required isolation control fails closed
	// without consuming a curation attempt or receiving frozen inputs.
	capability, err := planner.Probe(ctx)
	if err != nil {
		return nil, fmt.Errorf("probe native provider %s: %w", provider, err)
	}
	if !capability.Supported {
		reason := strings.TrimSpace(capability.UnsupportedReason)
		if reason == "" {
			reason = "installed CLI did not advertise the required safety controls"
		}
		return nil, fmt.Errorf("native provider %s is unavailable: %s", provider, reason)
	}
	return planner, nil
}

func curatorRunResumeOverrides(fs *flag.FlagSet) bool {
	for _, name := range []string{
		"request", "max-memories", "max-evidence", "max-transcript-entries", "page-size",
		"lease", "planner-timeout", "renew-before", "max-actions",
		"client-runtime", "client-model", "client-recipe", "client-recipe-version",
	} {
		if flagWasPassed(fs, name) {
			return true
		}
	}
	return false
}

func validateCuratorRunOptions(options memorycurator.Options) error {
	if options.Caps.MaxMemories < 1 || options.Caps.MaxEvidence < 1 || options.Caps.MaxTranscriptEntries < 1 {
		return errors.New("curator input caps must be positive")
	}
	if options.PageSize < 1 || options.PageSize > 200 {
		return errors.New("--page-size must be between 1 and 200")
	}
	if options.MaximumActions < 1 || options.MaximumActions > 128 {
		return errors.New("--max-actions must be between 1 and 128")
	}
	if options.LeaseDuration <= 0 || options.PlannerTimeout <= 0 || options.RenewBefore < 0 || options.RenewBefore >= options.LeaseDuration {
		return errors.New("curator lease, timeout, or renewal window is invalid")
	}
	if options.LeaseDuration%time.Second != 0 || options.PlannerTimeout%time.Second != 0 || options.RenewBefore%time.Second != 0 {
		return errors.New("curator lease, timeout, and renewal window must use whole seconds")
	}
	return nil
}

func validateCuratorRunPreflight(preflight *client.MemoryCurationPreflight, apply bool) error {
	if preflight == nil || strings.TrimSpace(preflight.Principal.AgentID) == "" {
		return errors.New("server returned no authenticated curator agent identity")
	}
	if preflight.Protocol.PlanSchema != memorycurator.MemoryPlanSchemaV1 ||
		preflight.Protocol.BackendInference || !preflight.Protocol.ClientInferenceRequired {
		return errors.New("server curation inference or plan protocol is incompatible")
	}
	primitives := make(map[string]bool, len(preflight.Protocol.AllowedPrimitives))
	for _, primitive := range preflight.Protocol.AllowedPrimitives {
		primitives[primitive] = true
	}
	for _, required := range curatorRunPrimitives {
		if !primitives[required] {
			return errors.New("server curation primitive set is incompatible")
		}
	}
	p := preflight.Permissions
	if !p.ListRequests || !p.Start || !p.GetRun || !p.GetInputs || !p.Renew || !p.Plan || !p.Abandon {
		return errors.New("credential lacks one or more required curator permissions")
	}
	if apply && !p.Apply {
		return errors.New("credential lacks memory curation apply permission")
	}
	return nil
}

func validateCuratorRunLimits(preflight *client.MemoryCurationPreflight, options memorycurator.Options, maxPlanBytes int64) error {
	if err := validateCuratorRunOptions(options); err != nil {
		return err
	}
	limits := preflight.Limits
	if limits.MaxPageSize < 1 || limits.MaxMemories < 1 || limits.MaxEvidence < 1 ||
		limits.MaxTranscriptEntries < 1 || limits.MinLeaseSeconds < 1 ||
		limits.MaxLeaseSeconds < limits.MinLeaseSeconds || limits.MaxPlanActions < 1 || limits.MaxPlanBytes < 1 {
		return errors.New("server returned invalid curator limits")
	}
	leaseSeconds := int64(options.LeaseDuration / time.Second)
	if options.PageSize > limits.MaxPageSize || options.Caps.MaxMemories > limits.MaxMemories ||
		options.Caps.MaxEvidence > limits.MaxEvidence || options.Caps.MaxTranscriptEntries > limits.MaxTranscriptEntries ||
		leaseSeconds < limits.MinLeaseSeconds || leaseSeconds > limits.MaxLeaseSeconds ||
		options.MaximumActions > limits.MaxPlanActions || maxPlanBytes > limits.MaxPlanBytes {
		return errors.New("requested curator limits exceed the authenticated server preflight")
	}
	return nil
}

func curatorOptionsFromLaunchState(state memorycurator.LaunchState, approveApply bool) memorycurator.Options {
	return memorycurator.Options{
		RequestID: state.RequestID, ApplyPolicy: state.ApplyPolicy, ApproveApply: approveApply,
		AllowSensitive: false, Caps: state.Caps, PageSize: state.PageSize,
		MaximumActions: state.MaximumActions,
		LeaseDuration:  time.Duration(state.LeaseSeconds) * time.Second,
		PlannerTimeout: time.Duration(state.PlannerTimeoutSeconds) * time.Second,
		RenewBefore:    time.Duration(state.RenewBeforeSeconds) * time.Second,
		Client:         state.Client,
	}
}

func printCuratorRunResult(result memorycurator.Result, jsonOut bool) int {
	summary := curatorRunSummary{Schema: curatorRunResultSchemaV1, LaunchID: result.LaunchID, Recovered: result.Recovered}
	if result.NoWork {
		summary.Status = "no_work"
		if jsonOut {
			return printJSON(summary)
		}
		fmt.Println("no-work")
		return 0
	}
	if result.Request != nil {
		summary.RequestID = result.Request.ID
	}
	if result.Run != nil {
		summary.RunID, summary.InputCount = result.Run.ID, result.InputCount
		summary.PlanRevision, summary.PlanHash = result.Run.PlanRevision, result.Run.PlanHash
		summary.ApplyReceiptID = result.Run.ApplyReceiptID
	}
	if result.Plan != nil {
		summary.PlanRevision, summary.PlanHash = result.Plan.Receipt.PlanRevision, result.Plan.Receipt.PlanHash
		summary.PlanReceiptID = result.Plan.Receipt.ID
	}
	if result.Apply != nil {
		summary.ApplyReceiptID = result.Apply.Receipt.ID
	}
	if summary.ApplyReceiptID != "" || result.Run != nil && result.Run.State == "applied" {
		summary.Status = "applied"
	} else {
		summary.Status, summary.PreviewRequeued = "previewed", true
	}
	if jsonOut {
		return printJSON(summary)
	}
	if summary.Status == "applied" {
		fmt.Printf("applied\tlaunch=%s\trequest=%s\trun=%s\tinputs=%d\tplan-revision=%d\tplan-hash=%s\tapply-receipt=%s\n",
			summary.LaunchID, summary.RequestID, summary.RunID, summary.InputCount,
			summary.PlanRevision, summary.PlanHash, summary.ApplyReceiptID)
		return 0
	}
	fmt.Printf("previewed\tlaunch=%s\trequest=%s\trun=%s\tinputs=%d\tplan-revision=%d\tplan-hash=%s\trequeued=true\n",
		summary.LaunchID, summary.RequestID, summary.RunID, summary.InputCount,
		summary.PlanRevision, summary.PlanHash)
	return 0
}
