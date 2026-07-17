// Package runtimeacceptance defines the provider-neutral live narrative-memory
// acceptance contract. The package contains no provider launcher and no model
// inference: a real interactive client executes the generated prompts, while
// the verifier reduces transcript and backend observations to sanitized,
// versioned evidence.
package runtimeacceptance

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/memoryhydration"
	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

// Public state and evidence schemas plus the suite version.
const (
	StateSchemaVersion    = "witself.runtime-memory-acceptance.state.v1"
	EvidenceSchemaVersion = "witself.runtime-memory-acceptance.evidence.v1"
	SuiteVersion          = "1"

	PhasePreparing = "preparing"
	PhaseReady     = "ready"
)

// Acceptance stage names used by the private manifest and sanitized evidence.
const (
	StageIdentity  = "identity"
	StageCapture   = "explicit-capture"
	StageHistory   = "history-recall"
	StageBroad     = "broad-redaction"
	StageSensitive = "sensitive-exact"
	StageIsolation = "cross-agent-isolation"
)

var supportedRuntimes = map[string]bool{
	transcriptcapture.RuntimeCodex:      true,
	transcriptcapture.RuntimeClaudeCode: true,
	transcriptcapture.RuntimeCursor:     true,
	transcriptcapture.RuntimeGrokBuild:  true,
}

// Identity is the value-free, token-derived principal retained in evidence.
type Identity struct {
	AccountID string `json:"account_id"`
	RealmID   string `json:"realm_id"`
	RealmName string `json:"realm_name"`
	AgentID   string `json:"agent_id"`
	AgentName string `json:"agent_name"`
}

// Build identifies the exact Witself server build under test.
type Build struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
}

// Delivery records the capability-accurate context path for one runtime.
type Delivery struct {
	SessionAutomatic bool   `json:"session_automatic"`
	SessionMode      string `json:"session_mode"`
	RecallAutomatic  bool   `json:"recall_automatic"`
	RecallMode       string `json:"recall_mode"`
}

// Prompt is one exact live-client stage. Prompt text is private run state and
// is never copied into retained evidence.
type Prompt struct {
	Stage      string `json:"stage"`
	Marker     string `json:"marker"`
	NewSession bool   `json:"new_session"`
	Text       string `json:"text"`
}

// FixtureState identifies synthetic resources without retaining their values.
type FixtureState struct {
	BaselineMemoryID  string `json:"baseline_memory_id,omitempty"`
	SensitiveFactID   string `json:"sensitive_fact_id,omitempty"`
	PeerMemoryID      string `json:"peer_memory_id,omitempty"`
	CurationRequestID string `json:"curation_request_id,omitempty"`
}

// PrivateMarkers are synthetic values used to verify exact behavior. They are
// deliberately absent from Evidence and make RunState a mode-0600 artifact.
type PrivateMarkers struct {
	Narrative string `json:"narrative"`
	Sensitive string `json:"sensitive"`
	PeerOnly  string `json:"peer_only"`
}

// RetryKeys make preparation exactly replayable after a partial failure.
type RetryKeys struct {
	BaselineMemory  string `json:"baseline_memory"`
	SensitiveFact   string `json:"sensitive_fact"`
	PeerMemory      string `json:"peer_memory"`
	CurationRequest string `json:"curation_request"`
}

// RunState is the private resumable acceptance manifest.
type RunState struct {
	SchemaVersion            string         `json:"schema_version"`
	SuiteVersion             string         `json:"suite_version"`
	Phase                    string         `json:"phase"`
	RunID                    string         `json:"run_id"`
	PreparedAt               time.Time      `json:"prepared_at"`
	Runtime                  string         `json:"runtime"`
	RuntimeVersion           string         `json:"runtime_version"`
	ConfiguredRuntimeVersion string         `json:"configured_runtime_version"`
	Delivery                 Delivery       `json:"delivery"`
	Witself                  Build          `json:"witself"`
	AccountName              string         `json:"account_name"`
	RealmName                string         `json:"realm_name"`
	AgentName                string         `json:"agent_name"`
	PeerAgentName            string         `json:"peer_agent_name"`
	PeerTokenFile            string         `json:"peer_token_file,omitempty"`
	Identity                 Identity       `json:"identity"`
	PeerIdentity             Identity       `json:"peer_identity"`
	CertificationEligible    bool           `json:"certification_eligible"`
	RehearsalReasons         []string       `json:"rehearsal_reasons"`
	BaselineTranscriptIDs    []string       `json:"baseline_transcript_ids"`
	Fixtures                 FixtureState   `json:"fixtures"`
	Markers                  PrivateMarkers `json:"private_markers"`
	RetryKeys                RetryKeys      `json:"retry_keys"`
	Prompts                  []Prompt       `json:"prompts"`
}

// TranscriptEntry is the minimum visible evidence needed by the verifier.
type TranscriptEntry struct {
	Sequence       int64     `json:"sequence"`
	Role           string    `json:"role"`
	Body           string    `json:"body"`
	Runtime        string    `json:"runtime"`
	RuntimeVersion string    `json:"runtime_version"`
	CreatedAt      time.Time `json:"created_at"`
}

// TranscriptObservation is a live transcript stripped to its acceptance fields.
type TranscriptObservation struct {
	ID      string            `json:"id"`
	Runtime string            `json:"runtime"`
	Entries []TranscriptEntry `json:"entries"`
}

// BackendObservation contains deterministic verification results. It never
// contains a fact value, memory content, transcript body, endpoint, or token.
type BackendObservation struct {
	BindingCurrent              bool     `json:"binding_current"`
	RuntimeVersionCurrent       bool     `json:"runtime_version_current"`
	HooksCurrent                bool     `json:"hooks_current"`
	InstructionsCurrent         bool     `json:"instructions_current"`
	ServerBuildCurrent          bool     `json:"server_build_current"`
	HarnessBuildCurrent         bool     `json:"harness_build_current"`
	ExplicitMemoryIDs           []string `json:"explicit_memory_ids"`
	CurationRunID               string   `json:"curation_run_id,omitempty"`
	CurationApplied             bool     `json:"curation_applied"`
	CurationApplyReceiptID      string   `json:"curation_apply_receipt_id,omitempty"`
	CurationPlanEmptyVerified   bool     `json:"curation_plan_empty_verified"`
	CurationActionCount         int      `json:"curation_action_count"`
	SensitiveFactExact          bool     `json:"sensitive_fact_exact"`
	SensitiveFactBroadRedacted  bool     `json:"sensitive_fact_broad_redacted"`
	PeerCanRecallPeerFixture    bool     `json:"peer_can_recall_peer_fixture"`
	SubjectCanRecallPeerFixture bool     `json:"subject_can_recall_peer_fixture"`
}

// VerificationInput combines live observations for deterministic reduction.
type VerificationInput struct {
	VerifiedAt  time.Time               `json:"verified_at"`
	Backend     BackendObservation      `json:"backend"`
	Transcripts []TranscriptObservation `json:"transcripts"`
}

// ResourceEvidence contains only value-free ids and counts.
type ResourceEvidence struct {
	TranscriptIDs []string `json:"transcript_ids,omitempty"`
	MemoryIDs     []string `json:"memory_ids,omitempty"`
	FactIDs       []string `json:"fact_ids,omitempty"`
	RequestIDs    []string `json:"request_ids,omitempty"`
	RunIDs        []string `json:"run_ids,omitempty"`
	ReceiptIDs    []string `json:"receipt_ids,omitempty"`
	ActionCount   *int     `json:"action_count,omitempty"`
}

// CaseEvidence is one pass/fail result with sanitized diagnostics.
type CaseEvidence struct {
	Name      string           `json:"name"`
	Passed    bool             `json:"passed"`
	Detail    string           `json:"detail"`
	Resources ResourceEvidence `json:"resources,omitempty"`
}

// RuntimeEvidence identifies the real client without retaining local paths.
type RuntimeEvidence struct {
	Name              string   `json:"name"`
	Delivery          Delivery `json:"delivery"`
	ClientVersion     string   `json:"client_version"`
	ConfiguredVersion string   `json:"configured_version"`
	ObservedVersions  []string `json:"observed_versions"`
}

// Evidence is the sanitized retained report for one runtime.
type Evidence struct {
	SchemaVersion         string          `json:"schema_version"`
	SuiteVersion          string          `json:"suite_version"`
	RunID                 string          `json:"run_id"`
	Status                string          `json:"status"`
	CertificationEligible bool            `json:"certification_eligible"`
	PreparedAt            time.Time       `json:"prepared_at"`
	VerifiedAt            time.Time       `json:"verified_at"`
	Runtime               RuntimeEvidence `json:"runtime"`
	Witself               Build           `json:"witself"`
	Identity              Identity        `json:"identity"`
	PeerIdentity          Identity        `json:"peer_identity"`
	Cases                 []CaseEvidence  `json:"cases"`
	Warnings              []string        `json:"warnings,omitempty"`
}

// NewState builds one private, replayable manifest. Resource creation remains
// the CLI's responsibility so this package stays deterministic and testable.
func NewState(runID, runtimeName, runtimeVersion, configuredVersion string, build Build, identity, peer Identity, accountName, peerTokenFile string, certificationEligible bool, rehearsalReasons []string, baselineTranscriptIDs []string, now time.Time) (RunState, error) {
	runtimeName, err := transcriptcapture.NormalizeRuntime(runtimeName)
	if err != nil || !supportedRuntimes[runtimeName] {
		return RunState{}, fmt.Errorf("unsupported runtime %q", runtimeName)
	}
	if strings.TrimSpace(runID) == "" {
		return RunState{}, errors.New("run id is required")
	}
	markers, err := newMarkers()
	if err != nil {
		return RunState{}, err
	}
	capability := memoryhydration.CapabilityFor(runtimeName)
	state := RunState{
		SchemaVersion: StateSchemaVersion, SuiteVersion: SuiteVersion,
		Phase: PhasePreparing, RunID: runID, PreparedAt: now.UTC(),
		Runtime: runtimeName, RuntimeVersion: strings.TrimSpace(runtimeVersion),
		ConfiguredRuntimeVersion: strings.TrimSpace(configuredVersion),
		Delivery: Delivery{
			SessionAutomatic: capability.SessionHydration.Automatic,
			SessionMode:      capability.SessionHydration.Delivery,
			RecallAutomatic:  capability.TaskRecall.Automatic,
			RecallMode:       capability.TaskRecall.Delivery,
		},
		Witself: build, AccountName: accountName, RealmName: identity.RealmName,
		AgentName: identity.AgentName, PeerAgentName: peer.AgentName,
		PeerTokenFile: peerTokenFile, Identity: identity, PeerIdentity: peer,
		CertificationEligible: certificationEligible,
		RehearsalReasons:      append([]string(nil), rehearsalReasons...),
		BaselineTranscriptIDs: sortedUnique(baselineTranscriptIDs),
		Markers:               markers,
		RetryKeys: RetryKeys{
			BaselineMemory:  runID + "-baseline-memory",
			SensitiveFact:   runID + "-sensitive-fact",
			PeerMemory:      runID + "-peer-memory",
			CurationRequest: runID + "-curation-request",
		},
	}
	state.Prompts = promptsFor(state)
	if err := state.Validate(); err != nil {
		return RunState{}, err
	}
	return state, nil
}

func newMarkers() (PrivateMarkers, error) {
	next := func(prefix string) (string, error) {
		var raw [8]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return "", err
		}
		return prefix + hex.EncodeToString(raw[:]), nil
	}
	narrative, err := next("kestrel")
	if err != nil {
		return PrivateMarkers{}, err
	}
	sensitive, err := next("private")
	if err != nil {
		return PrivateMarkers{}, err
	}
	peer, err := next("peer")
	if err != nil {
		return PrivateMarkers{}, err
	}
	return PrivateMarkers{Narrative: narrative, Sensitive: sensitive, PeerOnly: peer}, nil
}

func promptsFor(state RunState) []Prompt {
	marker := func(stage string) string {
		return fmt.Sprintf("[WITSELF-ACCEPTANCE %s %s]", state.RunID, strings.ToUpper(stage))
	}
	predicate := SensitivePredicate(state.RunID)
	tag := AcceptanceTag(state.RunID) + ":peer"
	return []Prompt{
		{Stage: StageIdentity, Marker: marker("identity"), NewSession: true, Text: marker("identity") + "\nReport your authenticated Witself account_id, realm_id, agent_id, and agent_name. Do not infer them from this prompt. Then explain in two sentences how those token-derived fields constrain portable-memory ownership and isolation."},
		{Stage: StageCapture, Marker: marker("capture"), NewSession: true, Text: marker("capture") + "\nRemember this in Witself narrative memory, and do not write it to provider-native memory: for acceptance run " + state.RunID + ", we chose migration codename " + state.Markers.Narrative + " for release notes because it proves portable continuity. Confirm only after the Witself write is durable."},
		{Stage: StageHistory, Marker: marker("history"), NewSession: true, Text: marker("history") + "\nFor acceptance run " + state.RunID + ", we chose a migration codename for release notes. What did we choose, and why? Do not use transcript or conversation history, and do not ask me for the codename."},
		{Stage: StageBroad, Marker: marker("broad"), NewSession: true, Text: marker("broad") + "\nGive a broad, redacted overview of facts and memories for this test agent. Do not target any single sensitive record, and do not print private values."},
		{Stage: StageSensitive, Marker: marker("sensitive"), NewSession: true, Text: marker("sensitive") + "\nRetrieve the exact value of the self fact whose predicate is " + predicate + ". This is an intentional exact lookup of a synthetic private marker; print that value once."},
		{Stage: StageIsolation, Marker: marker("isolation"), NewSession: true, Text: marker("isolation") + "\nUsing only your own default memory scope, search for the peer-only acceptance note tagged " + tag + ". Do not target another agent or request cross-agent access. If nothing matches, say so."},
	}
}

// AcceptanceTag is the stable synthetic tag prefix for one run.
func AcceptanceTag(runID string) string { return "acceptance:" + runID }

// SensitivePredicate is the run-unique synthetic fact address.
func SensitivePredicate(runID string) string { return "acceptance/" + runID + "/private-marker" }

// Prompt returns one stage or false when the stage is unknown.
func (s RunState) Prompt(stage string) (Prompt, bool) {
	for _, prompt := range s.Prompts {
		if prompt.Stage == stage {
			return prompt, true
		}
	}
	return Prompt{}, false
}

// Validate rejects partial, stale, or hand-edited private state.
func (s RunState) Validate() error {
	if s.SchemaVersion != StateSchemaVersion || s.SuiteVersion != SuiteVersion {
		return errors.New("unsupported acceptance state schema")
	}
	if s.Phase != PhasePreparing && s.Phase != PhaseReady {
		return fmt.Errorf("invalid acceptance phase %q", s.Phase)
	}
	if strings.TrimSpace(s.RunID) == "" || !supportedRuntimes[s.Runtime] || s.PreparedAt.IsZero() {
		return errors.New("acceptance state has incomplete run metadata")
	}
	if s.RuntimeVersion == "" || s.ConfiguredRuntimeVersion == "" || s.RuntimeVersion != s.ConfiguredRuntimeVersion {
		return errors.New("acceptance state requires one current, configured runtime version")
	}
	if err := validateIdentity(s.Identity); err != nil {
		return fmt.Errorf("identity: %w", err)
	}
	if err := validateIdentity(s.PeerIdentity); err != nil {
		return fmt.Errorf("peer identity: %w", err)
	}
	if s.Identity.AccountID != s.PeerIdentity.AccountID || s.Identity.RealmID != s.PeerIdentity.RealmID || s.Identity.AgentID == s.PeerIdentity.AgentID {
		return errors.New("peer must be a distinct agent in the same account and realm")
	}
	if strings.TrimSpace(s.AccountName) == "" || s.RealmName != s.Identity.RealmName ||
		s.AgentName != s.Identity.AgentName || s.PeerAgentName != s.PeerIdentity.AgentName {
		return errors.New("acceptance selectors do not match the authenticated identities")
	}
	if s.Markers.Narrative == "" || s.Markers.Sensitive == "" || s.Markers.PeerOnly == "" {
		return errors.New("private synthetic markers are incomplete")
	}
	if s.RetryKeys.BaselineMemory == "" || s.RetryKeys.SensitiveFact == "" || s.RetryKeys.PeerMemory == "" || s.RetryKeys.CurationRequest == "" {
		return errors.New("preparation retry keys are incomplete")
	}
	wantDelivery := memoryhydration.CapabilityFor(s.Runtime)
	if s.Delivery.SessionAutomatic != wantDelivery.SessionHydration.Automatic ||
		s.Delivery.SessionMode != wantDelivery.SessionHydration.Delivery ||
		s.Delivery.RecallAutomatic != wantDelivery.TaskRecall.Automatic ||
		s.Delivery.RecallMode != wantDelivery.TaskRecall.Delivery {
		return errors.New("delivery contract does not match the runtime capability matrix")
	}
	if len(s.Prompts) != 6 {
		return errors.New("acceptance state requires exactly six prompts")
	}
	seen := map[string]bool{}
	for _, prompt := range s.Prompts {
		if prompt.Stage == "" || prompt.Marker == "" || prompt.Text == "" || !prompt.NewSession || seen[prompt.Stage] || !strings.Contains(prompt.Text, prompt.Marker) {
			return fmt.Errorf("invalid or duplicate acceptance prompt %q", prompt.Stage)
		}
		seen[prompt.Stage] = true
	}
	for _, stage := range []string{StageIdentity, StageCapture, StageHistory, StageBroad, StageSensitive, StageIsolation} {
		if !seen[stage] {
			return fmt.Errorf("missing acceptance prompt %q", stage)
		}
	}
	if history, _ := s.Prompt(StageHistory); strings.Contains(history.Text, s.Markers.Narrative) {
		return errors.New("history prompt must not contain the narrative answer")
	}
	if broad, _ := s.Prompt(StageBroad); strings.Contains(broad.Text, s.Markers.Sensitive) {
		return errors.New("broad prompt must not contain the sensitive value")
	}
	if sensitive, _ := s.Prompt(StageSensitive); strings.Contains(sensitive.Text, s.Markers.Sensitive) {
		return errors.New("exact prompt must not contain the sensitive value")
	}
	if isolation, _ := s.Prompt(StageIsolation); strings.Contains(isolation.Text, s.Markers.PeerOnly) {
		return errors.New("isolation prompt must not contain the peer value")
	}
	if s.Phase == PhaseReady && (s.Fixtures.BaselineMemoryID == "" || s.Fixtures.SensitiveFactID == "" || s.Fixtures.PeerMemoryID == "" || s.Fixtures.CurationRequestID == "") {
		return errors.New("ready acceptance state has incomplete fixture ids")
	}
	return nil
}

func validateIdentity(identity Identity) error {
	if identity.AccountID == "" || identity.RealmID == "" || identity.RealmName == "" || identity.AgentID == "" || identity.AgentName == "" {
		return errors.New("all identity fields are required")
	}
	return nil
}

// WritePrivateState atomically persists a mode-0600 manifest. Existing files
// must already be private regular files; symlinks are refused.
func WritePrivateState(path string, state RunState) error {
	if err := state.Validate(); err != nil {
		return err
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&0o077 != 0 {
			return fmt.Errorf("refuse non-private or non-regular state file %s", path)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".runtime-acceptance-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// ReadPrivateState reads only a private regular state file and rejects unknown
// fields or trailing JSON.
func ReadPrivateState(path string) (RunState, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return RunState{}, err
	}
	if !info.Mode().IsRegular() || info.Mode()&0o077 != 0 {
		return RunState{}, fmt.Errorf("acceptance state must be a private mode-0600 regular file: %s", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return RunState{}, err
	}
	defer func() { _ = file.Close() }()
	decoder := json.NewDecoder(io.LimitReader(file, 1024*1024))
	decoder.DisallowUnknownFields()
	var state RunState
	if err := decoder.Decode(&state); err != nil {
		return RunState{}, fmt.Errorf("decode acceptance state: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return RunState{}, fmt.Errorf("decode acceptance state: %w", err)
	}
	if err := state.Validate(); err != nil {
		return RunState{}, err
	}
	return state, nil
}

// Evaluate reduces live observations to retained evidence without copying any
// content-bearing input into the report.
func Evaluate(state RunState, input VerificationInput) (Evidence, error) {
	if err := state.Validate(); err != nil {
		return Evidence{}, err
	}
	if state.Phase != PhaseReady {
		return Evidence{}, errors.New("acceptance preparation is not complete")
	}
	if input.VerifiedAt.IsZero() {
		input.VerifiedAt = time.Now().UTC()
	}
	stageResults := map[string]stageResult{}
	for _, prompt := range state.Prompts {
		stageResults[prompt.Stage] = findStage(prompt, input.Transcripts, state.Runtime, state.RuntimeVersion)
	}

	identityStage := stageResults[StageIdentity]
	identityResponse := strings.Join(identityStage.AssistantBodies, "\n")
	identityResponseOK := identityStage.Found && containsAll(identityResponse,
		state.Identity.AccountID, state.Identity.RealmID, state.Identity.AgentID, state.Identity.AgentName)
	observedVersions := observedProvenance(state.RuntimeVersion, stageResults)
	transcriptRuntimeOK := allStageProvenanceMatches(state.Runtime, stageResults)
	distinctSessionsOK := allStageTranscriptIDsDistinct(stageResults)
	versionObserved := containsString(observedVersions, state.RuntimeVersion)
	identityPass := input.Backend.BindingCurrent && input.Backend.RuntimeVersionCurrent &&
		input.Backend.HooksCurrent && input.Backend.InstructionsCurrent && input.Backend.ServerBuildCurrent && input.Backend.HarnessBuildCurrent &&
		identityResponseOK && transcriptRuntimeOK && distinctSessionsOK && versionObserved

	captureStage := stageResults[StageCapture]
	explicitPass := stageProvenanceMatches(captureStage, state.Runtime) && hasAssistantResponse(captureStage) && len(input.Backend.ExplicitMemoryIDs) > 0

	historyStage := stageResults[StageHistory]
	historyResponse := strings.Join(historyStage.AssistantBodies, "\n")
	historyPass := stageProvenanceMatches(historyStage, state.Runtime) && strings.Contains(historyResponse, state.Markers.Narrative)

	curationPass := input.Backend.CurationApplied && input.Backend.CurationRunID != "" && input.Backend.CurationApplyReceiptID != "" &&
		input.Backend.CurationPlanEmptyVerified && input.Backend.CurationActionCount == 0

	broadStage := stageResults[StageBroad]
	exactStage := stageResults[StageSensitive]
	broadResponse := strings.Join(broadStage.AssistantBodies, "\n")
	exactResponse := strings.Join(exactStage.AssistantBodies, "\n")
	sensitivePass := stageProvenanceMatches(broadStage, state.Runtime) && stageProvenanceMatches(exactStage, state.Runtime) && strings.TrimSpace(broadResponse) != "" &&
		!strings.Contains(broadResponse, state.Markers.Sensitive) &&
		strings.Contains(exactResponse, state.Markers.Sensitive) &&
		input.Backend.SensitiveFactExact && input.Backend.SensitiveFactBroadRedacted

	continuityPass := stageProvenanceMatches(captureStage, state.Runtime) && stageProvenanceMatches(historyStage, state.Runtime) &&
		captureStage.TranscriptID != historyStage.TranscriptID && historyPass

	isolationStage := stageResults[StageIsolation]
	isolationResponse := strings.Join(isolationStage.AssistantBodies, "\n")
	isolationPass := stageProvenanceMatches(isolationStage, state.Runtime) && strings.TrimSpace(isolationResponse) != "" && !strings.Contains(isolationResponse, state.Markers.PeerOnly) &&
		input.Backend.PeerCanRecallPeerFixture && !input.Backend.SubjectCanRecallPeerFixture

	var actionCount *int
	if input.Backend.CurationPlanEmptyVerified {
		value := input.Backend.CurationActionCount
		actionCount = &value
	}
	cases := []CaseEvidence{
		caseResult("identity_binding", identityPass, "evaluates authenticated identity, installed integration, observed client version and per-stage provenance, distinct client sessions, server build, and CLI harness build", identityStage),
		caseResult("explicit_narrative_capture", explicitPass, "the real client created a durable Witself narrative memory", captureStage, input.Backend.ExplicitMemoryIDs...),
		caseResult("history_dependent_recall", historyPass, deliveryDetail(state.Delivery), historyStage),
		{
			Name: "applied_empty_curation_checkpoint", Passed: curationPass,
			Detail: "the exact synthetic checkpoint reached a canonical applied empty plan by verification; no transcript or stage causality is claimed",
			Resources: ResourceEvidence{RequestIDs: compactStrings(state.Fixtures.CurationRequestID),
				RunIDs: compactStrings(input.Backend.CurationRunID), ReceiptIDs: compactStrings(input.Backend.CurationApplyReceiptID), ActionCount: actionCount},
		},
		{
			Name: "sensitive_exact_and_broad_redaction", Passed: sensitivePass,
			Detail:    "the synthetic sensitive fact was available to an intentional exact lookup and absent from broad output",
			Resources: ResourceEvidence{TranscriptIDs: compactStrings(broadStage.TranscriptID, exactStage.TranscriptID), FactIDs: compactStrings(state.Fixtures.SensitiveFactID)},
		},
		{
			Name: "same_agent_cross_session_continuity", Passed: continuityPass,
			Detail:    "a second real client session used the first session's durable narrative",
			Resources: ResourceEvidence{TranscriptIDs: compactStrings(captureStage.TranscriptID, historyStage.TranscriptID), MemoryIDs: sortedUnique(input.Backend.ExplicitMemoryIDs)},
		},
		{
			Name: "cross_agent_isolation", Passed: isolationPass,
			Detail:    "the peer could retrieve its fixture while the tested agent's default scope could not",
			Resources: ResourceEvidence{TranscriptIDs: compactStrings(isolationStage.TranscriptID), MemoryIDs: compactStrings(state.Fixtures.PeerMemoryID)},
		},
	}

	allPassed := true
	for _, item := range cases {
		allPassed = allPassed && item.Passed
	}
	status := "fail"
	if allPassed {
		status = "pass"
		if !state.CertificationEligible {
			status = "pass_rehearsal"
		}
	}
	warnings := append([]string(nil), state.RehearsalReasons...)
	evidence := Evidence{
		SchemaVersion: EvidenceSchemaVersion, SuiteVersion: SuiteVersion,
		RunID: state.RunID, Status: status,
		CertificationEligible: state.CertificationEligible,
		PreparedAt:            state.PreparedAt, VerifiedAt: input.VerifiedAt.UTC(),
		Runtime: RuntimeEvidence{
			Name: state.Runtime, Delivery: state.Delivery,
			ClientVersion: state.RuntimeVersion, ConfiguredVersion: state.ConfiguredRuntimeVersion,
			ObservedVersions: observedVersions,
		},
		Witself: state.Witself, Identity: state.Identity, PeerIdentity: state.PeerIdentity,
		Cases: cases, Warnings: warnings,
	}
	if err := evidence.ValidateSanitized(state.Markers); err != nil {
		return Evidence{}, err
	}
	return evidence, nil
}

type stageResult struct {
	Found                 bool
	TranscriptID          string
	Runtime               string
	UserProvenanceOK      bool
	AssistantProvenanceOK bool
	AssistantBodies       []string
}

func findStage(prompt Prompt, transcripts []TranscriptObservation, runtimeName, runtimeVersion string) stageResult {
	var result stageResult
	for _, transcript := range transcripts {
		for index, entry := range transcript.Entries {
			if entry.Role != "user" {
				continue
			}
			candidate := transcriptcapture.NormalizeUserPromptBody(runtimeName, entry.Body)
			if strings.TrimSpace(candidate) != strings.TrimSpace(prompt.Text) {
				continue
			}
			if result.Found {
				return stageResult{}
			}
			result = stageResult{
				Found: true, TranscriptID: transcript.ID, Runtime: transcript.Runtime,
				UserProvenanceOK:      entry.Runtime == runtimeName && entry.RuntimeVersion == runtimeVersion && !entry.CreatedAt.IsZero(),
				AssistantProvenanceOK: true,
			}
			for _, later := range transcript.Entries[index+1:] {
				if later.Role == "user" {
					break
				}
				if later.Role == "assistant" {
					result.AssistantBodies = append(result.AssistantBodies, later.Body)
					if later.Runtime != runtimeName || later.RuntimeVersion != runtimeVersion || later.CreatedAt.IsZero() {
						result.AssistantProvenanceOK = false
					}
				}
			}
			if len(result.AssistantBodies) == 0 {
				result.AssistantProvenanceOK = false
			}
		}
	}
	return result
}

func caseResult(name string, passed bool, detail string, stage stageResult, memoryIDs ...string) CaseEvidence {
	return CaseEvidence{Name: name, Passed: passed, Detail: detail,
		Resources: ResourceEvidence{TranscriptIDs: compactStrings(stage.TranscriptID), MemoryIDs: sortedUnique(memoryIDs)}}
}

func deliveryDetail(delivery Delivery) string {
	if delivery.RecallAutomatic {
		return "history-dependent context arrived through verified automatic hook hydration without a user search instruction"
	}
	return "managed runtime guidance caused the active client to use self.show and memory.recall without a user search instruction"
}

func hasAssistantResponse(stage stageResult) bool {
	return strings.TrimSpace(strings.Join(stage.AssistantBodies, "\n")) != ""
}

func stageProvenanceMatches(stage stageResult, runtimeName string) bool {
	return stage.Found && stage.Runtime == runtimeName && stage.UserProvenanceOK && stage.AssistantProvenanceOK
}

func observedProvenance(runtimeVersion string, stages map[string]stageResult) []string {
	for _, stage := range stages {
		if !stage.Found || !stage.UserProvenanceOK || !stage.AssistantProvenanceOK {
			return []string{}
		}
	}
	// Retain only the version that was independently detected from the installed
	// executable and required in every stage. Transcript payload fields are
	// otherwise untrusted and must not become a content side channel in evidence.
	return []string{runtimeVersion}
}

func allStageProvenanceMatches(runtimeName string, stages map[string]stageResult) bool {
	for _, stage := range stages {
		if !stage.Found || stage.Runtime != runtimeName || !stage.UserProvenanceOK || !stage.AssistantProvenanceOK {
			return false
		}
	}
	return true
}

func allStageTranscriptIDsDistinct(stages map[string]stageResult) bool {
	seen := make(map[string]bool, len(stages))
	for _, stage := range stages {
		if !stage.Found || stage.TranscriptID == "" || seen[stage.TranscriptID] {
			return false
		}
		seen[stage.TranscriptID] = true
	}
	return len(seen) == 6
}

func containsAll(value string, needles ...string) bool {
	for _, needle := range needles {
		if needle == "" || !strings.Contains(value, needle) {
			return false
		}
	}
	return true
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func compactStrings(values ...string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			out = append(out, value)
		}
	}
	return sortedUnique(out)
}

func sortedUnique(values []string) []string {
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

// ValidateSanitized proves that private synthetic values cannot enter the
// retained report and that every case name is unique.
func (e Evidence) ValidateSanitized(markers PrivateMarkers) error {
	if e.SchemaVersion != EvidenceSchemaVersion || e.SuiteVersion != SuiteVersion || e.RunID == "" {
		return errors.New("incomplete acceptance evidence metadata")
	}
	if e.Status != "pass" && e.Status != "pass_rehearsal" && e.Status != "fail" {
		return fmt.Errorf("invalid acceptance status %q", e.Status)
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	for _, marker := range []string{markers.Narrative, markers.Sensitive, markers.PeerOnly} {
		if marker != "" && strings.Contains(string(raw), marker) {
			return errors.New("retained evidence contains a private synthetic marker")
		}
	}
	seen := map[string]bool{}
	for _, item := range e.Cases {
		if item.Name == "" || seen[item.Name] {
			return errors.New("acceptance evidence has an empty or duplicate case name")
		}
		seen[item.Name] = true
	}
	if len(e.Cases) != 7 {
		return errors.New("acceptance evidence requires exactly seven cases")
	}
	return nil
}
