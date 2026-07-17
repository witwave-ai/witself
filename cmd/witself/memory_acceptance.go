package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/id"
	"github.com/witwave-ai/witself/internal/local"
	"github.com/witwave-ai/witself/internal/runtimeacceptance"
	"github.com/witwave-ai/witself/internal/store"
	"github.com/witwave-ai/witself/internal/transcriptcapture"
	"github.com/witwave-ai/witself/internal/version"
)

const memoryAcceptanceUsage = "usage: witself memory acceptance prepare|prompts|verify ..."

var (
	releaseVersionPattern = regexp.MustCompile(`^v?(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)(?:-(?:0|[1-9][0-9]*|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9][0-9]*|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*))*)?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)
	releaseCommitPattern  = regexp.MustCompile(`^[0-9a-f]{7,40}$`)
)

func memoryAcceptance(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, memoryAcceptanceUsage)
		return 2
	}
	switch args[0] {
	case "prepare":
		return memoryAcceptancePrepare(args[1:])
	case "prompts", "show":
		return memoryAcceptancePrompts(args[1:])
	case "verify":
		return memoryAcceptanceVerify(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "witself memory acceptance: unknown subcommand %q\n", args[0])
		return 2
	}
}

func memoryAcceptancePrepare(args []string) int {
	fs := flag.NewFlagSet("memory acceptance prepare", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	runtimeRaw := fs.String("runtime", "", "codex|claude-code|cursor|grok-build")
	account := accountFlag(fs)
	realm := fs.String("realm", "", "installed local realm name")
	agent := fs.String("agent", "", "installed runtime agent name")
	peerAgent := fs.String("peer-agent", "", "distinct synthetic peer agent in the same realm")
	peerTokenFile := fs.String("peer-token-file", "", "optional peer token file when it is not in local managed credentials")
	statePath := fs.String("state", "", "private resumable state path (default: ~/.witself/acceptance/RUN_ID.json)")
	rehearsal := fs.Bool("rehearsal", false, "allow a non-release build or nonempty test agent; evidence cannot close the certification gate")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || strings.TrimSpace(*runtimeRaw) == "" {
		fmt.Fprintln(os.Stderr, "usage: witself memory acceptance prepare --runtime RUNTIME --peer-agent NAME [flags]")
		return 2
	}
	runtimeName, err := transcriptcapture.NormalizeRuntime(*runtimeRaw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 2
	}

	var state runtimeacceptance.RunState
	resume := false
	if strings.TrimSpace(*statePath) != "" {
		if _, statErr := os.Lstat(*statePath); statErr == nil {
			state, err = runtimeacceptance.ReadPrivateState(*statePath)
			if err != nil {
				fmt.Fprintf(os.Stderr, "witself: read acceptance state: %v\n", err)
				return 1
			}
			resume = true
			if state.Runtime != runtimeName {
				fmt.Fprintln(os.Stderr, "witself: --runtime does not match the existing acceptance state")
				return 2
			}
		} else if !errors.Is(statErr, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "witself: inspect acceptance state: %v\n", statErr)
			return 1
		}
	}

	cfg, currentVersion, hooksCurrent, instructionsCurrent, err := acceptanceIntegrationPreflight(runtimeName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: acceptance integration preflight: %v\n", err)
		return 1
	}
	if resume {
		if state.RuntimeVersion != currentVersion || state.ConfiguredRuntimeVersion != cfg.RuntimeVersion {
			fmt.Fprintln(os.Stderr, "witself: runtime version changed after acceptance preparation; start a new run after reinstalling the integration")
			return 1
		}
		if (*account != "" && *account != state.AccountName) ||
			(*realm != "" && *realm != state.RealmName) ||
			(*agent != "" && *agent != state.AgentName) ||
			(*peerAgent != "" && *peerAgent != state.PeerAgentName) {
			fmt.Fprintln(os.Stderr, "witself: selectors do not match the existing acceptance state")
			return 2
		}
		*account, *realm, *agent, *peerAgent = state.AccountName, state.RealmName, state.AgentName, state.PeerAgentName
		if *peerTokenFile == "" {
			*peerTokenFile = state.PeerTokenFile
		}
	} else {
		if *account == "" {
			*account = cfg.Account
		}
		if *realm == "" {
			*realm = cfg.Realm
		}
		if *agent == "" {
			*agent = cfg.AgentName
		}
		if strings.TrimSpace(*peerAgent) == "" {
			fmt.Fprintln(os.Stderr, "witself: --peer-agent is required for cross-agent isolation")
			return 2
		}
		if *account != cfg.Account || *realm != cfg.Realm || *agent != cfg.AgentName {
			fmt.Fprintln(os.Stderr, "witself: account, realm, and agent must exactly match the installed runtime binding")
			return 2
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, self, err := acceptanceRuntimeConnection(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: acceptance runtime identity/token: %v\n", err)
		return 1
	}
	peerConn, peerSelf, err := acceptancePeerConnection(ctx, conn, *account, *realm, *peerAgent, *peerTokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: acceptance peer identity/token: %v\n", err)
		return 1
	}
	if err := acceptanceSameRealm(self.Identity, peerSelf.Identity); err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	build, err := fetchAcceptanceServerBuild(ctx, conn.Endpoint)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: read server build: %v\n", err)
		return 1
	}
	if resume {
		if !acceptanceIdentityEqual(state.Identity, acceptanceIdentity(self.Identity)) ||
			!acceptanceIdentityEqual(state.PeerIdentity, acceptanceIdentity(peerSelf.Identity)) || state.Witself != build {
			fmt.Fprintln(os.Stderr, "witself: authenticated identity, peer, or Witself server build changed after preparation; start a new run")
			return 1
		}
		if state.Phase == runtimeacceptance.PhaseReady {
			return printAcceptancePrepared(state, *statePath, *jsonOut)
		}
	} else {
		if self.MemoryCheckpoint != nil && self.MemoryCheckpoint.Pending {
			fmt.Fprintln(os.Stderr, "witself: the test agent already has a pending memory checkpoint; use a fresh isolated synthetic agent")
			return 1
		}
		reasons := []string{}
		if self.Index.Counts["memories"] != 0 {
			reasons = append(reasons, "test agent had existing narrative memories at preparation time")
		}
		if !acceptanceReleaseBuild(build) {
			reasons = append(reasons, "Witself server is not a release-identifiable build")
		}
		if !acceptanceReleasePair(build, currentAcceptanceCLIBuild()) {
			reasons = append(reasons, "Witself CLI is not a matching release build")
		}
		if *rehearsal {
			reasons = append(reasons, "operator selected rehearsal mode")
		}
		if len(reasons) != 0 && !*rehearsal {
			fmt.Fprintf(os.Stderr, "witself: certification requires a released server and a fresh synthetic agent: %s; pass --rehearsal only for a non-certifying dry run\n", strings.Join(reasons, "; "))
			return 1
		}
		transcripts, err := client.ListTranscripts(ctx, conn.Endpoint, conn.Token)
		if err != nil {
			fmt.Fprintf(os.Stderr, "witself: list baseline transcripts: %v\n", err)
			return 1
		}
		baselineIDs := make([]string, 0, len(transcripts))
		for _, transcript := range transcripts {
			baselineIDs = append(baselineIDs, transcript.ID)
		}
		runID, err := id.New("mra")
		if err != nil {
			fmt.Fprintf(os.Stderr, "witself: generate acceptance run id: %v\n", err)
			return 1
		}
		state, err = runtimeacceptance.NewState(
			runID, runtimeName, currentVersion, cfg.RuntimeVersion, build,
			acceptanceIdentity(self.Identity), acceptanceIdentity(peerSelf.Identity),
			*account, strings.TrimSpace(*peerTokenFile), len(reasons) == 0, reasons, baselineIDs, time.Now().UTC(),
		)
		if err != nil {
			fmt.Fprintf(os.Stderr, "witself: create acceptance state: %v\n", err)
			return 1
		}
		if *statePath == "" {
			*statePath, err = defaultAcceptanceStatePath(runID)
			if err != nil {
				fmt.Fprintf(os.Stderr, "witself: acceptance state path: %v\n", err)
				return 1
			}
		}
		if err := runtimeacceptance.WritePrivateState(*statePath, state); err != nil {
			fmt.Fprintf(os.Stderr, "witself: write initial acceptance state: %v\n", err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "acceptance state: %s (private, resumable)\n", *statePath)
	}
	if !hooksCurrent || !instructionsCurrent {
		fmt.Fprintln(os.Stderr, "witself: installed hooks or managed instructions became stale during preparation")
		return 1
	}

	if err := prepareAcceptanceFixtures(ctx, *statePath, &state, conn, peerConn); err != nil {
		fmt.Fprintf(os.Stderr, "witself: prepare acceptance fixtures: %v\n", err)
		fmt.Fprintf(os.Stderr, "witself: preparation is resumable with --state %s\n", *statePath)
		return 1
	}
	return printAcceptancePrepared(state, *statePath, *jsonOut)
}

func prepareAcceptanceFixtures(ctx context.Context, statePath string, state *runtimeacceptance.RunState, conn, peerConn agentConnection) error {
	write := func() error { return runtimeacceptance.WritePrivateState(statePath, *state) }
	if state.Fixtures.BaselineMemoryID == "" {
		result, err := client.CaptureMemory(ctx, conn.Endpoint, conn.Token, client.CaptureMemoryInput{
			Content: "Acceptance run " + state.RunID + " has one already-complete baseline note; it requires no semantic curation change.",
			Kind:    "note", Tags: []string{runtimeacceptance.AcceptanceTag(state.RunID), "acceptance:baseline"},
			Evidence:       []client.MemoryEvidenceInput{{State: "unavailable", UnavailableReason: "synthetic_acceptance_fixture"}},
			CaptureReason:  "acceptance_fixture",
			Client:         client.MemoryClientProvenance{Runtime: "witself-cli", Recipe: "runtime-acceptance", RecipeVersion: runtimeacceptance.SuiteVersion},
			IdempotencyKey: state.RetryKeys.BaselineMemory,
		})
		if err != nil {
			return fmt.Errorf("baseline memory: %w", err)
		}
		state.Fixtures.BaselineMemoryID = result.Memory.ID
		if err := write(); err != nil {
			return err
		}
	}
	if state.Fixtures.SensitiveFactID == "" {
		value, _ := json.Marshal(state.Markers.Sensitive)
		fact, err := client.SetFact(ctx, conn.Endpoint, conn.Token, client.SetFactInput{
			Subject: "self", Predicate: runtimeacceptance.SensitivePredicate(state.RunID),
			ValueType: "string", Value: value, Cardinality: "one", Sensitive: true,
			SourceKind: "agent", SourceRef: state.RunID,
			IdempotencyKey: state.RetryKeys.SensitiveFact,
		})
		if err != nil {
			return fmt.Errorf("sensitive fact: %w", err)
		}
		state.Fixtures.SensitiveFactID = fact.ID
		if err := write(); err != nil {
			return err
		}
	}
	if state.Fixtures.PeerMemoryID == "" {
		result, err := client.CaptureMemory(ctx, peerConn.Endpoint, peerConn.Token, client.CaptureMemoryInput{
			Content: "Peer-only acceptance marker " + state.Markers.PeerOnly + " belongs only to the peer fixture.",
			Kind:    "note", Tags: []string{runtimeacceptance.AcceptanceTag(state.RunID) + ":peer"}, Sensitive: true,
			Evidence:       []client.MemoryEvidenceInput{{State: "unavailable", UnavailableReason: "synthetic_acceptance_fixture"}},
			CaptureReason:  "acceptance_fixture",
			Client:         client.MemoryClientProvenance{Runtime: "witself-cli", Recipe: "runtime-acceptance", RecipeVersion: runtimeacceptance.SuiteVersion},
			IdempotencyKey: state.RetryKeys.PeerMemory,
		})
		if err != nil {
			return fmt.Errorf("peer memory: %w", err)
		}
		state.Fixtures.PeerMemoryID = result.Memory.ID
		if err := write(); err != nil {
			return err
		}
	}
	if state.Fixtures.CurationRequestID == "" {
		result, err := client.RequestMemoryCuration(ctx, conn.Endpoint, conn.Token, acceptanceCurationRequestInput(*state))
		if err != nil {
			return fmt.Errorf("curation request: %w", err)
		}
		state.Fixtures.CurationRequestID = result.Request.ID
		if err := write(); err != nil {
			return err
		}
	}
	state.Phase = runtimeacceptance.PhaseReady
	return write()
}

func acceptanceCurationRequestInput(state runtimeacceptance.RunState) client.RequestMemoryCurationInput {
	due := state.PreparedAt.UTC()
	return client.RequestMemoryCurationInput{
		Scope:         client.MemoryCurationScope{Sources: []string{"memory"}, MemoryStates: []string{"active"}, MaxMemories: 1},
		CoalescingKey: "runtime-acceptance." + state.RunID,
		TriggerReason: "runtime_acceptance", Priority: 100, DueAt: &due, MaxAttempts: 3,
		IdempotencyKey: state.RetryKeys.CurationRequest,
	}
}

func printAcceptancePrepared(state runtimeacceptance.RunState, statePath string, jsonOut bool) int {
	if jsonOut {
		return printJSON(map[string]any{
			"schema_version": runtimeacceptance.StateSchemaVersion,
			"run_id":         state.RunID, "runtime": state.Runtime,
			"state_path": statePath, "certification_eligible": state.CertificationEligible,
			"prompts": state.Prompts,
		})
	}
	fmt.Printf("prepared\trun=%s\truntime=%s\tdelivery=%s\tcertification-eligible=%t\n",
		state.RunID, state.Runtime, state.Delivery.RecallMode, state.CertificationEligible)
	fmt.Printf("state\t%s\n", statePath)
	printAcceptancePromptText(state.Prompts)
	return 0
}

func memoryAcceptancePrompts(args []string) int {
	fs := flag.NewFlagSet("memory acceptance prompts", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	statePath := fs.String("state", "", "private acceptance state path")
	stage := fs.String("stage", "", "optional single stage")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || strings.TrimSpace(*statePath) == "" {
		fmt.Fprintln(os.Stderr, "usage: witself memory acceptance prompts --state FILE [--stage STAGE] [--json]")
		return 2
	}
	state, err := runtimeacceptance.ReadPrivateState(*statePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: read acceptance state: %v\n", err)
		return 1
	}
	prompts := state.Prompts
	if strings.TrimSpace(*stage) != "" {
		prompt, ok := state.Prompt(strings.TrimSpace(*stage))
		if !ok {
			fmt.Fprintf(os.Stderr, "witself: unknown stage %q\n", *stage)
			return 2
		}
		prompts = []runtimeacceptance.Prompt{prompt}
	}
	if *jsonOut {
		return printJSON(map[string]any{"run_id": state.RunID, "runtime": state.Runtime, "prompts": prompts})
	}
	printAcceptancePromptText(prompts)
	return 0
}

func printAcceptancePromptText(prompts []runtimeacceptance.Prompt) {
	for index, prompt := range prompts {
		fmt.Printf("\nstage %d/%d: %s (start a new %s session)\n%s\n", index+1, len(prompts), prompt.Stage, "client", prompt.Text)
	}
}

func memoryAcceptanceVerify(args []string) int {
	fs := flag.NewFlagSet("memory acceptance verify", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	statePath := fs.String("state", "", "private acceptance state path")
	outPath := fs.String("out", "", "write sanitized evidence JSON to this path")
	wait := fs.Duration("wait", 30*time.Second, "maximum time to wait for transcript flush and foreground curation")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || strings.TrimSpace(*statePath) == "" || *wait < 0 || *wait > 5*time.Minute {
		fmt.Fprintln(os.Stderr, "usage: witself memory acceptance verify --state FILE [--out EVIDENCE.json] [--wait DURATION]")
		return 2
	}
	state, err := runtimeacceptance.ReadPrivateState(*statePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: read acceptance state: %v\n", err)
		return 1
	}
	if state.Phase != runtimeacceptance.PhaseReady {
		fmt.Fprintln(os.Stderr, "witself: acceptance preparation is incomplete; rerun prepare with this state file")
		return 1
	}

	deadline := time.Now().Add(*wait)
	var evidence runtimeacceptance.Evidence
	for {
		evidence, err = collectAcceptanceEvidence(context.Background(), state)
		if err == nil && (evidence.Status == "pass" || evidence.Status == "pass_rehearsal") {
			break
		}
		if time.Now().After(deadline) || *wait == 0 {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: verify acceptance: %v\n", err)
		return 1
	}
	raw, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: encode acceptance evidence: %v\n", err)
		return 1
	}
	if strings.TrimSpace(*outPath) != "" {
		if err := writeAcceptanceEvidence(*outPath, append(raw, '\n')); err != nil {
			fmt.Fprintf(os.Stderr, "witself: write acceptance evidence: %v\n", err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "sanitized evidence: %s\n", *outPath)
	} else {
		fmt.Println(string(raw))
	}
	if evidence.Status == "fail" {
		failed := []string{}
		for _, item := range evidence.Cases {
			if !item.Passed {
				failed = append(failed, item.Name)
			}
		}
		fmt.Fprintf(os.Stderr, "acceptance failed: %s\n", strings.Join(failed, ", "))
		return 1
	}
	fmt.Fprintf(os.Stderr, "acceptance %s: %s on Witself %s (%s)\n", evidence.Status, evidence.Runtime.Name, evidence.Witself.Version, evidence.Witself.Commit)
	return 0
}

func writeAcceptanceEvidence(path string, raw []byte) error {
	return writeFileAtomic(path, raw, 0o600)
}

func collectAcceptanceEvidence(ctx context.Context, state runtimeacceptance.RunState) (runtimeacceptance.Evidence, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cfg, currentVersion, hooksCurrent, instructionsCurrent, err := acceptanceIntegrationPreflight(state.Runtime)
	if err != nil {
		return runtimeacceptance.Evidence{}, err
	}
	conn, self, err := acceptanceRuntimeConnection(ctx, cfg)
	if err != nil {
		return runtimeacceptance.Evidence{}, err
	}
	peerConn, peerSelf, err := acceptancePeerConnection(ctx, conn, state.AccountName, state.RealmName, state.PeerAgentName, state.PeerTokenFile)
	if err != nil {
		return runtimeacceptance.Evidence{}, err
	}
	build, err := fetchAcceptanceServerBuild(ctx, conn.Endpoint)
	if err != nil {
		return runtimeacceptance.Evidence{}, err
	}
	transcripts, err := collectAcceptanceTranscripts(ctx, conn, state)
	if err != nil {
		return runtimeacceptance.Evidence{}, err
	}

	backend := runtimeacceptance.BackendObservation{
		BindingCurrent: acceptanceIdentityEqual(state.Identity, acceptanceIdentity(self.Identity)) &&
			acceptanceIdentityEqual(state.PeerIdentity, acceptanceIdentity(peerSelf.Identity)),
		RuntimeVersionCurrent: currentVersion == state.RuntimeVersion && cfg.RuntimeVersion == state.ConfiguredRuntimeVersion,
		HooksCurrent:          hooksCurrent, InstructionsCurrent: instructionsCurrent,
		ServerBuildCurrent:  build == state.Witself,
		HarnessBuildCurrent: !state.CertificationEligible || acceptanceReleasePair(build, currentAcceptanceCLIBuild()),
		CurationActionCount: -1,
	}
	explicit, err := client.RecallMemories(ctx, conn.Endpoint, conn.Token, client.MemoryRecallInput{Query: state.Markers.Narrative, IncludeSensitive: true, Limit: 20})
	if err != nil {
		return runtimeacceptance.Evidence{}, fmt.Errorf("verify explicit capture: %w", err)
	}
	for _, hit := range explicit.Hits {
		if strings.Contains(hit.Memory.Content, state.Markers.Narrative) && hit.Memory.ID != state.Fixtures.BaselineMemoryID {
			backend.ExplicitMemoryIDs = append(backend.ExplicitMemoryIDs, hit.Memory.ID)
		}
	}
	backend.ExplicitMemoryIDs = uniqueSortedAcceptanceStrings(backend.ExplicitMemoryIDs)

	request, requestErr := client.GetMemoryCurationRequest(ctx, conn.Endpoint, conn.Token, state.Fixtures.CurationRequestID)
	if requestErr == nil {
		runID := strings.TrimSpace(request.ClaimedRunID)
		if runID == "" {
			runID = strings.TrimSpace(request.ReplayRunID)
		}
		backend.CurationRunID = runID
		if runID != "" {
			run, runErr := client.GetMemoryCurationRun(ctx, conn.Endpoint, conn.Token, runID)
			if runErr == nil {
				backend.CurationApplied = request.State == "fulfilled" && run.State == "applied" &&
					run.RequestID == state.Fixtures.CurationRequestID && run.ApplyReceiptID != "" && run.AppliedAt != nil
				if backend.CurationApplied {
					backend.CurationApplyReceiptID = run.ApplyReceiptID
				}
				empty, emptyErr := store.AcceptMemoryCurationPlan(store.MemoryCurationPlanDraft{
					Schema: store.MemoryCurationPlanSchemaV1, DraftRevision: 1,
					Actions: []store.MemoryCurationPlanAction{},
				}, store.MemoryCurationPlanAcceptOptions{PlanRevision: run.PlanRevision})
				if emptyErr == nil && run.PlanSchema == store.MemoryCurationPlanSchemaV1 &&
					run.PlanHash == empty.PlanHash {
					backend.CurationPlanEmptyVerified = true
					backend.CurationActionCount = empty.Preview.ActionCount
				}
			}
		}
	}

	exactFact, exactErr := client.GetFactObservational(ctx, conn.Endpoint, conn.Token, "self", runtimeacceptance.SensitivePredicate(state.RunID))
	if exactErr == nil {
		var value string
		backend.SensitiveFactExact = exactFact.Sensitive && json.Unmarshal(exactFact.Value, &value) == nil && value == state.Markers.Sensitive
	}
	broadFacts, broadErr := client.ListFacts(ctx, conn.Endpoint, conn.Token, client.FactListOptions{
		Subject: "self", PredicatePrefix: "acceptance/" + state.RunID + "/", Limit: 20, Observational: true,
	})
	if broadErr == nil {
		for _, fact := range broadFacts {
			if fact.ID == state.Fixtures.SensitiveFactID && fact.Sensitive && string(fact.Value) == "null" {
				backend.SensitiveFactBroadRedacted = true
			}
		}
	}
	peerRecall, peerErr := client.RecallMemories(ctx, peerConn.Endpoint, peerConn.Token, client.MemoryRecallInput{Query: state.Markers.PeerOnly, IncludeSensitive: true, Limit: 20})
	if peerErr == nil {
		for _, hit := range peerRecall.Hits {
			if hit.Memory.ID == state.Fixtures.PeerMemoryID && strings.Contains(hit.Memory.Content, state.Markers.PeerOnly) {
				backend.PeerCanRecallPeerFixture = true
			}
		}
	}
	subjectRecall, subjectErr := client.RecallMemories(ctx, conn.Endpoint, conn.Token, client.MemoryRecallInput{Query: state.Markers.PeerOnly, IncludeSensitive: true, Limit: 20})
	if subjectErr == nil {
		for _, hit := range subjectRecall.Hits {
			if hit.Memory.ID == state.Fixtures.PeerMemoryID || strings.Contains(hit.Memory.Content, state.Markers.PeerOnly) {
				backend.SubjectCanRecallPeerFixture = true
			}
		}
	}

	return runtimeacceptance.Evaluate(state, runtimeacceptance.VerificationInput{
		VerifiedAt: time.Now().UTC(), Backend: backend, Transcripts: transcripts,
	})
}

func collectAcceptanceTranscripts(ctx context.Context, conn agentConnection, state runtimeacceptance.RunState) ([]runtimeacceptance.TranscriptObservation, error) {
	items, err := client.ListTranscripts(ctx, conn.Endpoint, conn.Token)
	if err != nil {
		return nil, err
	}
	baseline := map[string]bool{}
	for _, id := range state.BaselineTranscriptIDs {
		baseline[id] = true
	}
	sort.Slice(items, func(i, j int) bool { return items[i].UpdatedAt.After(items[j].UpdatedAt) })
	observations := make([]runtimeacceptance.TranscriptObservation, 0, 8)
	for _, item := range items {
		if baseline[item.ID] || item.CreatedAt.Before(state.PreparedAt.Add(-time.Minute)) {
			continue
		}
		var metadata struct {
			Runtime string `json:"runtime"`
		}
		if json.Unmarshal(item.Metadata, &metadata) != nil || metadata.Runtime != state.Runtime {
			continue
		}
		detail, err := client.GetTranscript(ctx, conn.Endpoint, conn.Token, item.ID)
		if err != nil {
			return nil, err
		}
		observation := runtimeacceptance.TranscriptObservation{ID: item.ID, Runtime: metadata.Runtime}
		if len(detail.Entries) > 2000 {
			return nil, fmt.Errorf("acceptance transcript %s exceeds the 2000-entry verification cap", item.ID)
		}
		for _, entry := range detail.Entries {
			var payload struct {
				Provenance struct {
					Runtime        string `json:"runtime"`
					RuntimeVersion string `json:"runtime_version"`
				} `json:"provenance"`
			}
			_ = json.Unmarshal(entry.Payload, &payload)
			observation.Entries = append(observation.Entries, runtimeacceptance.TranscriptEntry{
				Sequence: entry.Sequence, Role: entry.Role, Body: entry.Body,
				Runtime: payload.Provenance.Runtime, RuntimeVersion: payload.Provenance.RuntimeVersion,
				CreatedAt: entry.CreatedAt,
			})
		}
		observations = append(observations, observation)
		if len(observations) == 64 {
			break
		}
	}
	return observations, nil
}

func acceptanceIntegrationPreflight(runtimeName string) (transcriptcapture.Config, string, bool, bool, error) {
	cfg, err := transcriptcapture.LoadConfig(runtimeName)
	if err != nil {
		return transcriptcapture.Config{}, "", false, false, fmt.Errorf("missing or unreadable %s integration config: %w; run `witself install %s`", runtimeName, err, runtimeName)
	}
	if cfg.Runtime != runtimeName || cfg.Account == "" || cfg.Realm == "" || cfg.AgentID == "" || cfg.AgentName == "" || cfg.RuntimeVersion == "" {
		return transcriptcapture.Config{}, "", false, false, fmt.Errorf("%s integration config has incomplete identity or version; reinstall it", runtimeName)
	}
	runtimeCLI, err := findRuntimeCLI(runtimeName)
	if err != nil {
		return transcriptcapture.Config{}, "", false, false, err
	}
	currentVersion := detectRuntimeVersion(runtimeCLI)
	if currentVersion == "" {
		return transcriptcapture.Config{}, "", false, false, fmt.Errorf("%s client version could not be detected", runtimeName)
	}
	if currentVersion != cfg.RuntimeVersion {
		return transcriptcapture.Config{}, "", false, false, fmt.Errorf("%s client is %s but the installed binding records %s; rerun `witself install %s`", runtimeName, currentVersion, cfg.RuntimeVersion, runtimeName)
	}
	hooks, err := snapshotRuntimeHooks(runtimeName)
	if err != nil {
		return transcriptcapture.Config{}, "", false, false, err
	}
	wantManaged := cfg.HookMode == transcriptcapture.HookModeManaged
	hooksCurrent := hooks.userPresent != wantManaged
	if supportsManagedHooks(runtimeName) {
		hooksCurrent = hooksCurrent && hooks.managedPresent == wantManaged
	}
	if !hooksCurrent {
		return transcriptcapture.Config{}, "", false, false, fmt.Errorf("%s hooks do not match the installed %s hook mode; reinstall it", runtimeName, cfg.HookMode)
	}
	instructionsCurrent, err := acceptanceInstructionsCurrent(runtimeName)
	if err != nil {
		return transcriptcapture.Config{}, "", false, false, err
	}
	if !instructionsCurrent {
		return transcriptcapture.Config{}, "", false, false, fmt.Errorf("%s managed memory-routing instructions are missing or stale; reinstall it", runtimeName)
	}
	return cfg, currentVersion, hooksCurrent, instructionsCurrent, nil
}

func acceptanceInstructionsCurrent(runtimeName string) (bool, error) {
	spec, displayName, managed, err := runtimeMemoryRoutingSpec(runtimeName)
	if err != nil {
		return false, err
	}
	if !managed {
		return true, nil
	}
	spec, err = normalizeManagedInstructionsSpec(spec)
	if err != nil {
		return false, fmt.Errorf("inspect %s instructions: %w", displayName, err)
	}
	snapshot, err := readManagedInstructionsSnapshot(spec)
	if err != nil {
		return false, fmt.Errorf("inspect %s instructions: %w", displayName, err)
	}
	if !snapshot.existed {
		return false, nil
	}
	if spec.exclusive {
		if err := validateExclusiveManagedInstructionsContent(snapshot.data, spec, true); err != nil {
			return false, err
		}
	}
	_, changed, err := upsertManagedInstructionsBlock(snapshot.data, spec)
	return !changed, err
}

func acceptanceRuntimeConnection(ctx context.Context, cfg transcriptcapture.Config) (agentConnection, client.SelfDigest, error) {
	conn, err := connectAgent(ctx, cfg.Account, cfg.Realm, cfg.AgentName, cfg.Endpoint, cfg.TokenFile)
	if err != nil {
		return agentConnection{}, client.SelfDigest{}, err
	}
	self, err := client.GetSelf(ctx, conn.Endpoint, conn.Token, client.SelfOptions{
		IncludeCounts: true, IncludeCheckpoint: true, IncludeMessageCheckpoint: true,
	})
	if err != nil {
		return agentConnection{}, client.SelfDigest{}, err
	}
	if self.Identity.AccountID != cfg.AccountID || self.Identity.RealmID != cfg.RealmID || self.Identity.AgentID != cfg.AgentID ||
		self.Identity.RealmName != cfg.Realm || self.Identity.AgentName != cfg.AgentName {
		return agentConnection{}, client.SelfDigest{}, errors.New("authenticated identity does not match the installed runtime binding; reinstall with the intended token")
	}
	return conn, self, nil
}

func acceptancePeerConnection(ctx context.Context, subject agentConnection, account, realm, peerAgent, peerTokenFile string) (agentConnection, client.SelfDigest, error) {
	conn, err := connectAgent(ctx, account, realm, peerAgent, subject.Endpoint, peerTokenFile)
	if err != nil {
		return agentConnection{}, client.SelfDigest{}, err
	}
	self, err := client.GetSelf(ctx, conn.Endpoint, conn.Token, client.SelfOptions{})
	if err != nil {
		return agentConnection{}, client.SelfDigest{}, err
	}
	if self.Identity.AgentName != peerAgent || self.Identity.RealmName != realm {
		return agentConnection{}, client.SelfDigest{}, errors.New("peer token does not authenticate as the requested peer and realm")
	}
	return conn, self, nil
}

func acceptanceSameRealm(identity, peer client.SelfIdentity) error {
	if identity.AccountID != peer.AccountID || identity.RealmID != peer.RealmID || identity.AgentID == peer.AgentID {
		return errors.New("acceptance peer must be a distinct agent in the same account and realm")
	}
	return nil
}

func acceptanceIdentity(value client.SelfIdentity) runtimeacceptance.Identity {
	return runtimeacceptance.Identity{
		AccountID: value.AccountID, RealmID: value.RealmID, RealmName: value.RealmName,
		AgentID: value.AgentID, AgentName: value.AgentName,
	}
}

func acceptanceIdentityEqual(left, right runtimeacceptance.Identity) bool { return left == right }

func fetchAcceptanceServerBuild(ctx context.Context, endpoint string) (runtimeacceptance.Build, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(endpoint, "/")+"/v1/version", nil)
	if err != nil {
		return runtimeacceptance.Build{}, err
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return runtimeacceptance.Build{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return runtimeacceptance.Build{}, fmt.Errorf("version endpoint returned %s", resp.Status)
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 64*1024))
	decoder.DisallowUnknownFields()
	var out struct {
		SchemaVersion string `json:"schema_version"`
		Version       string `json:"version"`
		Commit        string `json:"commit"`
		Date          string `json:"date"`
	}
	if err := decoder.Decode(&out); err != nil {
		return runtimeacceptance.Build{}, err
	}
	if out.SchemaVersion != "witself.v0" || out.Version == "" || out.Commit == "" || out.Date == "" {
		return runtimeacceptance.Build{}, errors.New("version endpoint returned incomplete build identity")
	}
	return runtimeacceptance.Build{Version: out.Version, Commit: out.Commit, Date: out.Date}, nil
}

func acceptanceReleaseBuild(build runtimeacceptance.Build) bool {
	_, dateErr := time.Parse(time.RFC3339, build.Date)
	return releaseVersionPattern.MatchString(build.Version) && releaseCommitPattern.MatchString(build.Commit) && dateErr == nil
}

func currentAcceptanceCLIBuild() runtimeacceptance.Build {
	return runtimeacceptance.Build{Version: version.Version, Commit: version.Commit, Date: version.Date}
}

func acceptanceReleasePair(server, cli runtimeacceptance.Build) bool {
	return acceptanceReleaseBuild(server) && acceptanceReleaseBuild(cli) &&
		server.Version == cli.Version && server.Commit == cli.Commit
}

func defaultAcceptanceStatePath(runID string) (string, error) {
	home, err := local.Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "acceptance", runID+".json"), nil
}

func uniqueSortedAcceptanceStrings(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	sort.Strings(out)
	if out == nil {
		return []string{}
	}
	return out
}
