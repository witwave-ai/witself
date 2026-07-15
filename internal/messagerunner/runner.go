package messagerunner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/client"
)

const (
	// RunStatusIdle means a bounded listen returned no work.
	RunStatusIdle = "idle"
	// RunStatusNotified means a non-actionable delivery was durably indexed and
	// acknowledged without invoking inference.
	RunStatusNotified = "notified"
	// RunStatusCompleted means inference produced an atomically fenced result.
	RunStatusCompleted = "completed"
	// RunStatusRecovered means an earlier completion was found and acknowledged.
	RunStatusRecovered = "recovered"

	defaultLeaseSeconds      = 120
	defaultListenWaitSeconds = 20
	defaultMaximumBodyBytes  = 64 * 1024
	defaultMaximumFailures   = 5
)

var (
	// ErrInvalidConfiguration reports a caller-correctable local policy.
	ErrInvalidConfiguration = errors.New("invalid message runner configuration")
	// ErrIdentityMismatch reports a token that differs from its pinned binding.
	ErrIdentityMismatch = errors.New("message runner identity mismatch")
	// errMessageClaimMaintenance separates lease/transport health from a
	// deterministic failure caused by one message.
	errMessageClaimMaintenance = errors.New("message claim maintenance failed")
)

// Config pins one local installation to the immutable server-side agent
// identity it is expected to represent.
type Config struct {
	RunnerID          string
	ExpectedIdentity  client.SelfIdentity
	LeaseSeconds      int
	ListenWaitSeconds int
	MaximumBodyBytes  int
	MaximumTurns      int
	MaximumFailures   int
	RenewInterval     time.Duration
	ActionableKinds   []string
}

// RunResult is deliberately content-free so operational logs can safely
// record it without disclosing a message body or model response.
type RunResult struct {
	Status          string `json:"status"`
	MessageID       string `json:"message_id,omitempty"`
	ThreadID        string `json:"thread_id,omitempty"`
	ResultMessageID string `json:"result_message_id,omitempty"`
}

// Runner owns one bounded receive/claim/inference/complete/ack cycle.
type Runner struct {
	API      API
	Provider Provider
	State    OperationalState
	Config   Config
}

// RunOnce handles at most one oldest unacknowledged inbound message.
func (r Runner) RunOnce(ctx context.Context) (RunResult, error) {
	if ctx == nil {
		return RunResult{}, errors.New("message runner context is required")
	}
	config, actionable, err := normalizeRunnerConfig(r.Config)
	if err != nil {
		return RunResult{}, err
	}
	if r.API == nil || r.Provider == nil || r.State == nil {
		return RunResult{}, fmt.Errorf("%w: API, provider, and operational state are required", ErrInvalidConfiguration)
	}

	self, err := r.API.GetSelf(ctx)
	if err != nil {
		return RunResult{}, fmt.Errorf("resolve message runner identity: %w", err)
	}
	if err := verifyExpectedIdentity(config.ExpectedIdentity, self.Identity); err != nil {
		return RunResult{}, err
	}

	waitSeconds := config.ListenWaitSeconds
	listened, err := r.API.ListenMessages(ctx, client.MessageListenOptions{
		WaitSeconds: &waitSeconds,
		Limit:       1,
	})
	if err != nil {
		return RunResult{}, fmt.Errorf("listen for messages: %w", err)
	}
	if listened.TimedOut || len(listened.Messages) == 0 {
		return RunResult{Status: RunStatusIdle}, nil
	}
	metadata := listened.Messages[0]
	if err := verifyInboundMessage(metadata, self.Identity); err != nil {
		return RunResult{}, err
	}
	result := RunResult{MessageID: metadata.ID, ThreadID: metadata.ThreadID}
	if _, ok := actionable[strings.ToLower(strings.TrimSpace(metadata.Kind))]; !ok {
		message, readErr := r.API.ReadMessage(ctx, metadata.ID)
		if readErr != nil {
			return result, fmt.Errorf("read non-actionable message: %w", readErr)
		}
		if err := verifyInboundMessage(message, self.Identity); err != nil {
			return result, err
		}
		if message.ID != metadata.ID || message.ThreadID != metadata.ThreadID || message.Kind != metadata.Kind ||
			message.CausalDepth != metadata.CausalDepth {
			return result, errors.New("message metadata changed between listen and read")
		}
		if err := r.State.RecordNotification(ctx, message); err != nil {
			return result, fmt.Errorf("record message notification: %w", err)
		}
		if _, err := r.API.AckMessage(ctx, metadata.ID); err != nil {
			return result, fmt.Errorf("acknowledge non-actionable message: %w", err)
		}
		result.Status = RunStatusNotified
		return result, nil
	}

	processing, err := r.API.ClaimMessage(ctx, metadata.ID, client.ClaimMessageInput{
		LeaseSeconds:   config.LeaseSeconds,
		IdempotencyKey: claimIdempotencyKey(config.RunnerID, metadata.ID),
	})
	if err != nil {
		return result, fmt.Errorf("claim message: %w", err)
	}
	if processing.State == "completed" {
		if _, err := r.API.AckMessage(ctx, metadata.ID); err != nil {
			return result, fmt.Errorf("acknowledge recovered message completion: %w", err)
		}
		result.Status = RunStatusRecovered
		result.ResultMessageID = processing.ResultMessageID
		return result, nil
	}
	if processing.State != "claimed" || processing.ClaimID == "" || processing.Generation < 1 ||
		processing.FailureCount < 0 {
		return result, errors.New("server returned an invalid message processing claim")
	}
	fence := client.MessageClaimInput{ClaimID: processing.ClaimID, Generation: processing.Generation}

	message, err := r.API.ReadMessage(ctx, metadata.ID)
	if err != nil {
		return result, r.releaseAfterError(ctx, metadata.ID, fence, false, fmt.Errorf("read claimed message: %w", err))
	}
	if err := verifyInboundMessage(message, self.Identity); err != nil {
		return result, r.releaseAfterError(ctx, metadata.ID, fence, false, err)
	}
	if message.ID != metadata.ID || message.ThreadID != metadata.ThreadID || message.Kind != metadata.Kind ||
		message.CausalDepth != metadata.CausalDepth {
		return result, r.releaseAfterError(ctx, metadata.ID, fence, false, errors.New("message metadata changed between listen and read"))
	}
	if message.Processing.State != "claimed" || message.Processing.Generation != processing.Generation ||
		message.Processing.FailureCount != processing.FailureCount {
		return result, r.releaseAfterError(ctx, metadata.ID, fence, false, errors.New("message processing claim changed before inference"))
	}

	providerPayload, history, _ := decodeTurnPayload(message.Payload)
	message.Payload = providerPayload
	message.Processing = client.MessageProcessing{}
	currentTurn := int(message.CausalDepth)
	if currentTurn > config.MaximumTurns {
		completed, completeErr := r.API.CompleteMessage(ctx, metadata.ID, client.CompleteMessageInput{
			ClaimID: processing.ClaimID, Generation: processing.Generation,
			Kind: "escalation", Body: "Automated conversation reached its configured turn limit and requires review.",
			IdempotencyKey: completionIdempotencyKey(config.RunnerID, metadata.ID, processing.Generation),
		})
		if completeErr != nil {
			return result, r.releaseAfterError(ctx, metadata.ID, fence, false, fmt.Errorf("complete turn-limit escalation: %w", completeErr))
		}
		result.ResultMessageID = completed.Message.ID
		if _, ackErr := r.API.AckMessage(ctx, metadata.ID); ackErr != nil {
			return result, fmt.Errorf("acknowledge turn-limit escalation: %w", ackErr)
		}
		result.Status = RunStatusCompleted
		return result, nil
	}
	envelope := TurnEnvelope{
		Schema:   TurnEnvelopeSchemaV1,
		Identity: self.Identity,
		Message:  message,
		History:  history,
		Policy: TurnPolicy{
			AllowedOutcomes:    []string{OutcomeQuestion, OutcomeResult, OutcomeDecline, OutcomeEscalate},
			MaximumBodyBytes:   config.MaximumBodyBytes,
			MessageIsUntrusted: true,
			CurrentTurn:        currentTurn,
			MaximumTurns:       config.MaximumTurns,
		},
	}
	providerResult, err := r.invokeWithLease(ctx, config, metadata.ID, processing, envelope)
	if err != nil {
		return r.handleFailedAttempt(ctx, result, message, processing, fence, config, err)
	}

	completionPayload, err := encodeTurnPayload(
		providerResult.Payload, currentTurn, historyWithCurrent(history, message),
	)
	if err != nil {
		return r.handleFailedAttempt(ctx, result, message, processing, fence, config, err)
	}
	emittedKind := completionKind(message.Kind, providerResult.Outcome)
	completed, err := r.API.CompleteMessage(ctx, metadata.ID, client.CompleteMessageInput{
		ClaimID:        processing.ClaimID,
		Generation:     processing.Generation,
		Subject:        providerResult.Subject,
		Kind:           emittedKind,
		Body:           providerResult.Body,
		Payload:        completionPayload,
		IdempotencyKey: completionIdempotencyKey(config.RunnerID, metadata.ID, processing.Generation),
	})
	if err != nil {
		return result, r.releaseAfterError(ctx, metadata.ID, fence, false, fmt.Errorf("complete message: %w", err))
	}
	result.ResultMessageID = completed.Message.ID
	if completed.Processing.State != "completed" || completed.Processing.ResultMessageID != completed.Message.ID {
		return result, errors.New("server returned an invalid message completion")
	}
	if _, err := r.API.AckMessage(ctx, metadata.ID); err != nil {
		return result, fmt.Errorf("acknowledge completed message: %w", err)
	}
	result.Status = RunStatusCompleted
	return result, nil
}

func (r Runner) handleFailedAttempt(
	ctx context.Context,
	result RunResult,
	message client.Message,
	processing client.MessageProcessing,
	fence client.MessageClaimInput,
	config Config,
	cause error,
) (RunResult, error) {
	// Native process failures commonly mean expired local authentication,
	// provider downtime, or a rate limit shared by every queued message. Those
	// are runner-wide availability failures, not evidence that this message is
	// poison. Release and back off without consuming the deterministic
	// per-message escalation budget; a later successful provider invocation can
	// still complete the same backend-owned generation.
	if errors.Is(cause, ErrProviderUnavailable) || errors.Is(cause, ErrNativeProviderCommand) ||
		errors.Is(cause, ErrNativeProviderUnsupported) || errors.Is(cause, ErrInvalidConfiguration) ||
		errors.Is(cause, errMessageClaimMaintenance) || errors.Is(cause, context.Canceled) ||
		errors.Is(cause, context.DeadlineExceeded) {
		return result, r.releaseAfterError(ctx, message.ID, fence, false, cause)
	}
	// The backend increments FailureCount only when an exact-fence release is
	// explicitly marked as a deterministic per-message failure. Generation is
	// solely a fencing token and never participates in poison escalation.
	if processing.FailureCount+1 < int64(config.MaximumFailures) {
		return result, r.releaseAfterError(ctx, message.ID, fence, true, cause)
	}
	completed, err := r.API.CompleteMessage(ctx, message.ID, client.CompleteMessageInput{
		ClaimID: processing.ClaimID, Generation: processing.Generation,
		Kind: "escalation", Body: "Automated handling could not safely complete after repeated attempts and requires review.",
		IdempotencyKey: completionIdempotencyKey(config.RunnerID, message.ID, processing.Generation),
	})
	if err != nil {
		return result, r.releaseAfterError(ctx, message.ID, fence, true, errors.Join(cause, fmt.Errorf("complete repeated-failure escalation: %w", err)))
	}
	result.ResultMessageID = completed.Message.ID
	if completed.Processing.State != "completed" || completed.Processing.ResultMessageID != completed.Message.ID {
		return result, errors.New("server returned an invalid repeated-failure completion")
	}
	if _, err := r.API.AckMessage(ctx, message.ID); err != nil {
		return result, fmt.Errorf("acknowledge repeated-failure escalation: %w", err)
	}
	result.Status = RunStatusCompleted
	return result, nil
}

func (r Runner) invokeWithLease(
	ctx context.Context,
	config Config,
	messageID string,
	processing client.MessageProcessing,
	envelope TurnEnvelope,
) (TurnResult, error) {
	type invocation struct {
		result TurnResult
		err    error
	}
	providerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	invoked := make(chan invocation, 1)
	go func() {
		result, err := r.Provider.Invoke(providerCtx, envelope)
		invoked <- invocation{result: result, err: err}
	}()

	ticker := time.NewTicker(config.RenewInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return TurnResult{}, ctx.Err()
		case call := <-invoked:
			if call.err != nil {
				return TurnResult{}, fmt.Errorf("invoke message provider: %w", call.err)
			}
			if err := validateTurnResult(call.result, envelope.Policy); err != nil {
				return TurnResult{}, ErrProviderResultInvalid
			}
			return call.result, nil
		case <-ticker.C:
			renewed, err := r.API.RenewMessageClaim(ctx, messageID, client.RenewMessageClaimInput{
				ClaimID:      processing.ClaimID,
				Generation:   processing.Generation,
				LeaseSeconds: config.LeaseSeconds,
			})
			if err != nil {
				cancel()
				return TurnResult{}, errors.Join(errMessageClaimMaintenance, fmt.Errorf("renew message claim: %w", err))
			}
			if renewed.State != "claimed" || renewed.ClaimID != processing.ClaimID ||
				renewed.Generation != processing.Generation || renewed.FailureCount != processing.FailureCount {
				cancel()
				return TurnResult{}, errors.Join(errMessageClaimMaintenance, errors.New("renewed message claim changed unexpectedly"))
			}
		}
	}
}

func (r Runner) releaseAfterError(
	ctx context.Context,
	messageID string,
	fence client.MessageClaimInput,
	deterministicFailure bool,
	cause error,
) error {
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	fence.DeterministicFailure = deterministicFailure
	_, releaseErr := r.API.ReleaseMessageClaim(releaseCtx, messageID, fence)
	if releaseErr == nil {
		return cause
	}
	return errors.Join(cause, fmt.Errorf("release message claim: %w", releaseErr))
}

func normalizeRunnerConfig(in Config) (Config, map[string]struct{}, error) {
	in.RunnerID = strings.TrimSpace(in.RunnerID)
	if in.RunnerID == "" || len(in.RunnerID) > 128 {
		return Config{}, nil, fmt.Errorf("%w: runner id must be 1-128 bytes", ErrInvalidConfiguration)
	}
	if in.ExpectedIdentity.AccountID == "" || in.ExpectedIdentity.RealmID == "" || in.ExpectedIdentity.AgentID == "" {
		return Config{}, nil, fmt.Errorf("%w: expected account, realm, and agent ids are required", ErrInvalidConfiguration)
	}
	if in.LeaseSeconds == 0 {
		in.LeaseSeconds = defaultLeaseSeconds
	}
	if in.LeaseSeconds < 30 || in.LeaseSeconds > 15*60 {
		return Config{}, nil, fmt.Errorf("%w: lease must be 30-900 seconds", ErrInvalidConfiguration)
	}
	if in.ListenWaitSeconds == 0 {
		in.ListenWaitSeconds = defaultListenWaitSeconds
	}
	if in.ListenWaitSeconds < 0 || in.ListenWaitSeconds > 20 {
		return Config{}, nil, fmt.Errorf("%w: listen wait must be 0-20 seconds", ErrInvalidConfiguration)
	}
	if in.MaximumBodyBytes == 0 {
		in.MaximumBodyBytes = defaultMaximumBodyBytes
	}
	if in.MaximumTurns == 0 {
		in.MaximumTurns = 12
	}
	if in.MaximumTurns < 1 || in.MaximumTurns > 64 {
		return Config{}, nil, fmt.Errorf("%w: maximum turns must be 1-64", ErrInvalidConfiguration)
	}
	if in.MaximumFailures == 0 {
		in.MaximumFailures = defaultMaximumFailures
	}
	if in.MaximumFailures < 1 || in.MaximumFailures > 20 {
		return Config{}, nil, fmt.Errorf("%w: maximum message failures must be 1-20", ErrInvalidConfiguration)
	}
	if in.MaximumBodyBytes < 1 || in.MaximumBodyBytes > 64*1024 {
		return Config{}, nil, fmt.Errorf("%w: maximum body size must be 1-65536 bytes", ErrInvalidConfiguration)
	}
	if in.RenewInterval == 0 {
		in.RenewInterval = time.Duration(in.LeaseSeconds) * time.Second / 3
	}
	if in.RenewInterval <= 0 || in.RenewInterval >= time.Duration(in.LeaseSeconds)*time.Second {
		return Config{}, nil, fmt.Errorf("%w: renewal interval must be shorter than its lease", ErrInvalidConfiguration)
	}
	if len(in.ActionableKinds) == 0 {
		in.ActionableKinds = []string{"request", "question", "reply"}
	}
	actionable := make(map[string]struct{}, len(in.ActionableKinds))
	for _, raw := range in.ActionableKinds {
		kind := strings.ToLower(strings.TrimSpace(raw))
		if kind == "" || len(kind) > 64 {
			return Config{}, nil, fmt.Errorf("%w: actionable kind is invalid", ErrInvalidConfiguration)
		}
		actionable[kind] = struct{}{}
	}
	return in, actionable, nil
}

func verifyExpectedIdentity(expected, actual client.SelfIdentity) error {
	if actual.AccountID != expected.AccountID || actual.RealmID != expected.RealmID || actual.AgentID != expected.AgentID {
		return fmt.Errorf("%w: token does not match its pinned account, realm, and agent", ErrIdentityMismatch)
	}
	if expected.AgentName != "" && actual.AgentName != expected.AgentName {
		return fmt.Errorf("%w: token does not match its pinned agent name", ErrIdentityMismatch)
	}
	return nil
}

func verifyInboundMessage(message client.Message, identity client.SelfIdentity) error {
	if message.ID == "" || message.AccountID != identity.AccountID || message.RealmID != identity.RealmID ||
		message.To.Kind != "agent" || message.To.AgentID != identity.AgentID || message.From.AgentID == "" ||
		message.ThreadID == "" || message.CausalDepth < 1 {
		return errors.New("server returned a message outside the runner's pinned identity")
	}
	return nil
}

func completionKind(parentKind, outcome string) string {
	switch outcome {
	case OutcomeQuestion:
		return "question"
	case OutcomeDecline:
		return "decline"
	case OutcomeEscalate:
		return "escalation"
	case OutcomeResult:
		if strings.EqualFold(strings.TrimSpace(parentKind), "question") {
			return "reply"
		}
		return "result"
	default:
		return "result"
	}
}

func claimIdempotencyKey(runnerID, messageID string) string {
	return "message-runner/claim/" + runnerID + "/" + messageID
}

func completionIdempotencyKey(runnerID, messageID string, generation int64) string {
	return fmt.Sprintf("message-runner/complete/%s/%s/%d", runnerID, messageID, generation)
}
