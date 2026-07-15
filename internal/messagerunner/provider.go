package messagerunner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/client"
)

const (
	// TurnEnvelopeSchemaV1 identifies trusted-parent input to an inference child.
	TurnEnvelopeSchemaV1 = "witself.message-turn.v1"
	// TurnResultSchemaV1 identifies content-only inference child output.
	TurnResultSchemaV1 = "witself.message-turn-result.v1"

	// OutcomeQuestion requests one bounded clarification turn.
	OutcomeQuestion = "question"
	// OutcomeResult reports successful text-only completion.
	OutcomeResult = "result"
	// OutcomeDecline reports a request that cannot be handled safely.
	OutcomeDecline = "decline"
	// OutcomeProgress reports non-terminal progress for adapters that support it.
	OutcomeProgress = "progress"
	// OutcomeEscalate requests trusted human or tool-capable handling.
	OutcomeEscalate = "escalate"

	// DefaultProviderTimeout bounds one inference child invocation.
	DefaultProviderTimeout = 4 * time.Minute
	// DefaultProviderOutputBytes caps retained provider stdout.
	DefaultProviderOutputBytes = 256 * 1024
	// DefaultProviderStderrBytes caps retained, never-returned provider stderr.
	DefaultProviderStderrBytes = 64 * 1024
)

var (
	// ErrProviderUnavailable identifies host-wide or provider-wide availability
	// failures that must never count as deterministic poison for one message.
	ErrProviderUnavailable = errors.New("message provider is unavailable")
	// ErrProviderOutputInvalid is a fixed, value-free boundary error for child
	// output that cannot be decoded as exactly one strict result object.
	ErrProviderOutputInvalid = errors.New("message provider returned invalid output")
	// ErrProviderResultInvalid is a fixed, value-free boundary error for a
	// decoded result that violates the parent-authored turn policy.
	ErrProviderResultInvalid = errors.New("message provider returned an invalid result")
)

type providerEnvironmentProfile string

const (
	providerEnvironmentGeneric providerEnvironmentProfile = "generic"
	providerEnvironmentClaude  providerEnvironmentProfile = "claude-code"
	providerEnvironmentGrok    providerEnvironmentProfile = "grok-build"
)

// TurnEnvelope is the only data passed from the trusted runner parent to an
// inference child. It never contains the Witself bearer token or token path.
// Message body and payload remain explicitly untrusted data.
type TurnEnvelope struct {
	Schema   string              `json:"schema"`
	Identity client.SelfIdentity `json:"identity"`
	Message  client.Message      `json:"message"`
	History  []TurnHistoryEntry  `json:"history,omitempty"`
	Policy   TurnPolicy          `json:"policy"`
}

// TurnPolicy carries bounded, non-authority execution constraints.
type TurnPolicy struct {
	AllowedOutcomes    []string `json:"allowed_outcomes"`
	MaximumBodyBytes   int      `json:"maximum_body_bytes"`
	MessageIsUntrusted bool     `json:"message_is_untrusted"`
	CurrentTurn        int      `json:"current_turn"`
	MaximumTurns       int      `json:"maximum_turns"`
}

// TurnResult contains content only. Routing, identity, claim, and authority
// fields are deliberately absent and remain owned by the trusted parent.
type TurnResult struct {
	Schema  string          `json:"schema"`
	Outcome string          `json:"outcome"`
	Subject string          `json:"subject,omitempty"`
	Body    string          `json:"body"`
	Payload json.RawMessage `json:"payload,omitempty"`
	Model   string          `json:"model,omitempty"`
}

// Provider produces one content-only conversational turn.
type Provider interface {
	Invoke(context.Context, TurnEnvelope) (TurnResult, error)
}

// CommandProvider runs one explicitly configured adapter executable without a
// shell. The adapter receives one JSON envelope on stdin and must emit exactly
// one strict TurnResult JSON object on stdout. Every WITSELF_* environment
// value is removed so credentials retained by the parent cannot be inherited.
type CommandProvider struct {
	Executable     string
	Args           []string
	Dir            string
	Env            []string
	Timeout        time.Duration
	MaxOutputBytes int
	MaxStderrBytes int
}

// Invoke runs one configured adapter under the sanitized provider boundary.
func (p CommandProvider) Invoke(ctx context.Context, envelope TurnEnvelope) (TurnResult, error) {
	if ctx == nil {
		return TurnResult{}, errors.New("message provider context is required")
	}
	if strings.TrimSpace(p.Executable) == "" {
		return TurnResult{}, errors.New("message provider executable is required")
	}
	if err := validateTurnEnvelope(envelope); err != nil {
		return TurnResult{}, err
	}
	providerEnvelope := envelope
	providerEnvelope.Message.Processing = client.MessageProcessing{}
	raw, err := json.Marshal(providerEnvelope)
	if err != nil {
		return TurnResult{}, fmt.Errorf("encode message turn: %w", err)
	}
	timeout := p.Timeout
	if timeout == 0 {
		timeout = DefaultProviderTimeout
	}
	if timeout < time.Second || timeout > 30*time.Minute {
		return TurnResult{}, errors.New("message provider timeout must be between 1s and 30m")
	}
	maxOutput := p.MaxOutputBytes
	if maxOutput == 0 {
		maxOutput = DefaultProviderOutputBytes
	}
	maxStderr := p.MaxStderrBytes
	if maxStderr == 0 {
		maxStderr = DefaultProviderStderrBytes
	}
	if maxOutput < 1 || maxOutput > 4*1024*1024 || maxStderr < 1 || maxStderr > 1024*1024 {
		return TurnResult{}, errors.New("message provider stream limit is invalid")
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := exec.CommandContext(runCtx, p.Executable, p.Args...)
	configureProviderProcess(command)
	command.Dir = p.Dir
	command.Env = messageProviderEnvironment(providerEnvironmentGeneric, os.Environ(), p.Env)
	command.Stdin = bytes.NewReader(append(raw, '\n'))
	stdout := &limitedBuffer{limit: maxOutput}
	stderr := &limitedBuffer{limit: maxStderr}
	command.Stdout, command.Stderr = stdout, stderr
	runErr := command.Run()
	if runCtx.Err() != nil {
		return TurnResult{}, runCtx.Err()
	}
	if stdout.exceeded {
		return TurnResult{}, errors.New("message provider output exceeded its limit")
	}
	if stderr.exceeded {
		return TurnResult{}, errors.New("message provider stderr exceeded its limit")
	}
	if runErr != nil {
		// Stderr may echo message content and is intentionally never returned.
		return TurnResult{}, fmt.Errorf("message provider command failed: %w", runErr)
	}
	var result TurnResult
	decoder := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return TurnResult{}, ErrProviderOutputInvalid
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return TurnResult{}, ErrProviderOutputInvalid
	}
	if err := validateTurnResult(result, envelope.Policy); err != nil {
		return TurnResult{}, ErrProviderResultInvalid
	}
	return result, nil
}

func validateTurnEnvelope(envelope TurnEnvelope) error {
	if envelope.Schema != TurnEnvelopeSchemaV1 || envelope.Identity.AccountID == "" ||
		envelope.Identity.RealmID == "" || envelope.Identity.AgentID == "" || envelope.Message.ID == "" ||
		envelope.Message.AccountID != envelope.Identity.AccountID ||
		envelope.Message.RealmID != envelope.Identity.RealmID ||
		envelope.Message.To.AgentID != envelope.Identity.AgentID ||
		!envelope.Policy.MessageIsUntrusted || envelope.Policy.MaximumBodyBytes < 1 ||
		envelope.Policy.MaximumBodyBytes > 64*1024 || len(envelope.Policy.AllowedOutcomes) == 0 ||
		envelope.Policy.CurrentTurn < 1 || envelope.Policy.MaximumTurns < envelope.Policy.CurrentTurn ||
		envelope.Policy.MaximumTurns > 64 || len(envelope.History) > maximumHistoryEntries {
		return errors.New("message turn envelope is incomplete")
	}
	seen := make(map[string]struct{}, len(envelope.Policy.AllowedOutcomes))
	for _, outcome := range envelope.Policy.AllowedOutcomes {
		if !supportedOutcome(outcome) {
			return fmt.Errorf("unsupported allowed message outcome %q", outcome)
		}
		if _, ok := seen[outcome]; ok {
			return fmt.Errorf("duplicate allowed message outcome %q", outcome)
		}
		seen[outcome] = struct{}{}
	}
	return nil
}

func validateTurnResult(result TurnResult, policy TurnPolicy) error {
	if result.Schema != TurnResultSchemaV1 {
		return fmt.Errorf("unsupported message provider result schema %q", result.Schema)
	}
	if !supportedOutcome(result.Outcome) {
		return fmt.Errorf("unsupported message provider outcome %q", result.Outcome)
	}
	allowed := false
	for _, outcome := range policy.AllowedOutcomes {
		allowed = allowed || outcome == result.Outcome
	}
	if !allowed {
		return fmt.Errorf("message provider outcome %q is not allowed", result.Outcome)
	}
	if strings.TrimSpace(result.Body) == "" || len(result.Body) > policy.MaximumBodyBytes || len(result.Subject) > 256 {
		return errors.New("message provider result content is invalid")
	}
	if len(result.Payload) != 0 {
		if len(result.Payload) > maximumProviderPayloadBytes {
			return errors.New("message provider payload exceeds its limit")
		}
		var object map[string]any
		if err := json.Unmarshal(result.Payload, &object); err != nil || object == nil {
			return errors.New("message provider payload must be a JSON object")
		}
	}
	return nil
}

func supportedOutcome(outcome string) bool {
	switch outcome {
	case OutcomeQuestion, OutcomeResult, OutcomeDecline, OutcomeProgress, OutcomeEscalate:
		return true
	default:
		return false
	}
}

func messageProviderEnvironment(profile providerEnvironmentProfile, base, extra []string) []string {
	out := make([]string, 0, len(base)+len(extra)+1)
	indices := map[string]int{}
	add := func(entry string, explicit bool) {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || strings.HasPrefix(strings.ToUpper(key), "WITSELF_") {
			return
		}
		if !explicit && !providerBaseEnvironmentAllowed(profile, key) {
			return
		}
		if index, exists := indices[key]; exists {
			out[index] = entry
			return
		}
		indices[key] = len(out)
		out = append(out, entry)
	}
	for _, entry := range base {
		add(entry, false)
	}
	for _, entry := range extra {
		add(entry, true)
	}
	// The reserved session marker is parent-authored after all filtering.
	return append(out, "WITSELF_MESSAGE_RUNNER_SESSION=1")
}

func providerBaseEnvironmentAllowed(profile providerEnvironmentProfile, key string) bool {
	upper := strings.ToUpper(key)
	switch upper {
	case "PATH", "TMPDIR", "TMP", "TEMP",
		"LANG", "LANGUAGE", "TERM", "COLORTERM", "NO_COLOR",
		"SSL_CERT_FILE", "SSL_CERT_DIR":
		return true
	}
	if strings.HasPrefix(upper, "LC_") {
		return true
	}
	switch profile {
	case providerEnvironmentClaude:
		return upper == "ANTHROPIC_API_KEY" || upper == "ANTHROPIC_AUTH_TOKEN" ||
			upper == "CLAUDE_CODE_OAUTH_TOKEN" || upper == "ANTHROPIC_BASE_URL"
	case providerEnvironmentGrok:
		return upper == "XAI_API_KEY"
	default:
		return false
	}
}

type limitedBuffer struct {
	buffer   bytes.Buffer
	limit    int
	exceeded bool
}

func (b *limitedBuffer) Bytes() []byte {
	return b.buffer.Bytes()
}

func (b *limitedBuffer) String() string {
	return b.buffer.String()
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.exceeded {
		return len(p), nil
	}
	remaining := b.limit - b.buffer.Len()
	if remaining <= 0 {
		b.exceeded = true
		return len(p), nil
	}
	if len(p) > remaining {
		_, _ = b.buffer.Write(p[:remaining])
		b.exceeded = true
		return len(p), nil
	}
	_, _ = b.buffer.Write(p)
	return len(p), nil
}
