package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/agentemailoutbound"
	"github.com/witwave-ai/witself/internal/store"
)

const (
	defaultAgentEmailOutboundBatchSize       = 10
	defaultAgentEmailOutboundInterval        = 2 * time.Second
	defaultAgentEmailOutboundBatchTimeout    = 30 * time.Second
	defaultAgentEmailOutboundProviderTimeout = 20 * time.Second
	maximumAgentEmailOutboundBatchSize       = 100
	maximumAgentEmailOutboundBatchTimeout    = 5 * time.Minute
	maximumAgentEmailOutboundProviderTimeout = time.Minute
	agentEmailOutboundProvider               = "cloudflare_email_sending"
)

type agentEmailOutboundWorkerConfig struct {
	BatchSize       int
	Interval        time.Duration
	BatchTimeout    time.Duration
	ProviderTimeout time.Duration
}

func defaultAgentEmailOutboundWorkerConfig() agentEmailOutboundWorkerConfig {
	return agentEmailOutboundWorkerConfig{
		BatchSize:       defaultAgentEmailOutboundBatchSize,
		Interval:        defaultAgentEmailOutboundInterval,
		BatchTimeout:    defaultAgentEmailOutboundBatchTimeout,
		ProviderTimeout: defaultAgentEmailOutboundProviderTimeout,
	}
}

func (c agentEmailOutboundWorkerConfig) Validate() error {
	if c.BatchSize < 1 || c.BatchSize > maximumAgentEmailOutboundBatchSize {
		return fmt.Errorf("batch size must be between 1 and %d", maximumAgentEmailOutboundBatchSize)
	}
	if c.Interval < 100*time.Millisecond || c.Interval > 5*time.Minute {
		return errors.New("interval must be between 100ms and 5m")
	}
	if c.BatchTimeout < time.Second || c.BatchTimeout > maximumAgentEmailOutboundBatchTimeout {
		return errors.New("batch timeout must be between 1s and 5m")
	}
	if c.ProviderTimeout < time.Second || c.ProviderTimeout > maximumAgentEmailOutboundProviderTimeout {
		return errors.New("provider timeout must be between 1s and 1m")
	}
	if c.ProviderTimeout >= c.BatchTimeout {
		return errors.New("provider timeout must be shorter than batch timeout")
	}
	return nil
}

type agentEmailOutboundWorkerStore interface {
	ClaimAgentEmailOutbound(context.Context, store.AgentEmailOutboundClaimInput) (store.AgentEmailOutboundDispatch, error)
	StartAgentEmailOutboundProviderCall(context.Context, string, store.AgentEmailOutboundClaimFence) (store.AgentEmailOutboundDispatch, error)
	FinalizeAgentEmailOutbound(context.Context, string, store.FinalizeAgentEmailOutboundInput) (store.AgentEmailOutboundMessage, error)
	RetryAgentEmailOutbound(context.Context, string, store.RetryAgentEmailOutboundInput) (store.AgentEmailOutboundMessage, error)
	MarkAgentEmailOutboundAmbiguous(context.Context, string, store.AmbiguousAgentEmailOutboundInput) (store.AgentEmailOutboundMessage, error)
	ReconcileExhaustedAgentEmailOutbound(context.Context, int) (int64, error)
}

type agentEmailOutboundDispatchClient interface {
	Send(context.Context, agentemailoutbound.Dispatch) (agentemailoutbound.Response, error)
}

type agentEmailOutboundBatchResult struct {
	Claimed           int64
	Accepted          int64
	Delivered         int64
	Retried           int64
	Bounced           int64
	Rejected          int64
	Failed            int64
	Ambiguous         int64
	Canceled          int64
	ExpiredReconciled int64
}

func (r agentEmailOutboundBatchResult) empty() bool {
	return r == (agentEmailOutboundBatchResult{})
}

func runAgentEmailOutboundWorker(
	ctx context.Context,
	st agentEmailOutboundWorkerStore,
	client agentEmailOutboundDispatchClient,
	cfg agentEmailOutboundWorkerConfig,
	onResult func(agentEmailOutboundBatchResult),
	onError func(error),
) error {
	if st == nil || client == nil {
		return errors.New("outbound agent-email worker dependencies are required")
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	run := func() {
		batchCtx, cancel := context.WithTimeout(ctx, cfg.BatchTimeout)
		result, err := processAgentEmailOutboundBatch(batchCtx, st, client, cfg)
		cancel()
		if err != nil {
			if !errors.Is(err, context.Canceled) && onError != nil {
				onError(err)
			}
			return
		}
		if onResult != nil {
			onResult(result)
		}
	}

	run()
	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			run()
		}
	}
}

func processAgentEmailOutboundBatch(
	ctx context.Context,
	st agentEmailOutboundWorkerStore,
	client agentEmailOutboundDispatchClient,
	cfg agentEmailOutboundWorkerConfig,
) (agentEmailOutboundBatchResult, error) {
	if err := cfg.Validate(); err != nil {
		return agentEmailOutboundBatchResult{}, err
	}
	var result agentEmailOutboundBatchResult
	reconciled, err := st.ReconcileExhaustedAgentEmailOutbound(ctx, cfg.BatchSize)
	if err != nil {
		return result, err
	}
	result.ExpiredReconciled = reconciled

	for range cfg.BatchSize {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		claimed, err := st.ClaimAgentEmailOutbound(ctx, store.AgentEmailOutboundClaimInput{})
		if errors.Is(err, store.ErrAgentEmailOutboundEmpty) {
			return result, nil
		}
		if err != nil {
			return result, err
		}
		result.Claimed++
		outcome, err := dispatchClaimedAgentEmail(ctx, st, client, cfg.ProviderTimeout, claimed)
		if err != nil {
			return result, err
		}
		switch outcome {
		case store.AgentEmailOutboundAccepted:
			result.Accepted++
		case store.AgentEmailOutboundDelivered:
			result.Delivered++
		case store.AgentEmailOutboundQueued:
			result.Retried++
		case store.AgentEmailOutboundBounced:
			result.Bounced++
		case store.AgentEmailOutboundRejected:
			result.Rejected++
		case store.AgentEmailOutboundFailed:
			result.Failed++
		case store.AgentEmailOutboundAmbiguous:
			result.Ambiguous++
		case store.AgentEmailOutboundCanceled:
			result.Canceled++
		default:
			return result, fmt.Errorf("outbound agent-email worker returned unknown outcome %q", outcome)
		}
	}
	return result, nil
}

func dispatchClaimedAgentEmail(
	ctx context.Context,
	st agentEmailOutboundWorkerStore,
	client agentEmailOutboundDispatchClient,
	providerTimeout time.Duration,
	claimed store.AgentEmailOutboundDispatch,
) (string, error) {
	sendID := claimed.Message.ID
	started, err := st.StartAgentEmailOutboundProviderCall(ctx, sendID, claimed.Claim)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrFeatureNotEnabled),
			errors.Is(err, store.ErrAgentEmailSendDisabled),
			errors.Is(err, store.ErrAccountNotActive),
			errors.Is(err, store.ErrAgentEmailOutboundForbidden),
			errors.Is(err, store.ErrAgentEmailRecipientSuppressed):
			if started.Message.State == store.AgentEmailOutboundAmbiguous {
				return store.AgentEmailOutboundAmbiguous, nil
			}
			return store.AgentEmailOutboundCanceled, nil
		case errors.Is(err, store.ErrAgentEmailOutboundClaimLost):
			return store.AgentEmailOutboundCanceled, nil
		case errors.Is(err, store.ErrAgentEmailOutboundProviderAlreadyStarted):
			// The store returned the immutable content for a fenced recovery.
			// Replaying this exact envelope asks the adapter's durable receipt;
			// it never creates a fresh logical send.
		default:
			var rateErr *store.AgentEmailOutboundRateLimitError
			if errors.As(err, &rateErr) && rateErr.Retryable {
				retryAfter := max(rateErr.RetryAfter, time.Second)
				return retryAgentEmailOutboundDispatch(
					ctx, st, claimed, time.Now().UTC().Add(retryAfter),
					store.AgentEmailOutboundErrorProviderRateLimited,
					started.Message.State == store.AgentEmailOutboundProviderStarted,
				)
			}
			return "", err
		}
	}

	dispatch := agentemailoutbound.Dispatch{
		SchemaVersion: agentemailoutbound.DispatchSchemaVersion,
		SendID:        started.Message.ID,
		AccountID:     started.Message.AccountID,
		RealmID:       started.Message.RealmID,
		AgentID:       started.Message.OwnerAgentID,
		From:          started.Message.FromAddress,
		ReplyTo:       started.Message.ReplyToAddress,
		To:            started.Message.ToAddress,
		Subject:       started.Message.Subject,
		Text:          started.Text,
		InReplyTo:     started.InReplyTo,
		References:    append([]string(nil), started.References...),
	}
	if err := dispatch.Validate(); err != nil {
		return markAgentEmailOutboundDispatchAmbiguous(
			ctx, st, started, store.AgentEmailOutboundErrorProviderResponseInvalid,
		)
	}

	providerCtx, cancelProvider := context.WithTimeout(ctx, providerTimeout)
	response, sendErr := client.Send(providerCtx, dispatch)
	cancelProvider()

	settleCtx, cancelSettle := agentEmailOutboundSettlementContext(ctx)
	defer cancelSettle()
	if response.Provider != "" && response.Provider != agentEmailOutboundProvider &&
		(sendErr == nil || response.State != agentemailoutbound.StateAmbiguous) {
		return retryAgentEmailOutboundDispatch(
			settleCtx, st, started, time.Now().UTC().Add(time.Second),
			store.AgentEmailOutboundErrorProviderResponseInvalid,
			true,
		)
	}

	switch response.State {
	case agentemailoutbound.StateAccepted, agentemailoutbound.StateQueued:
		_, err = st.FinalizeAgentEmailOutbound(settleCtx, sendID, store.FinalizeAgentEmailOutboundInput{
			Claim: started.Claim, State: store.AgentEmailOutboundAccepted,
			Provider: agentEmailOutboundProvider, ProviderMessageID: response.ProviderMessageID,
		})
		return store.AgentEmailOutboundAccepted, err
	case agentemailoutbound.StateDelivered:
		_, err = st.FinalizeAgentEmailOutbound(settleCtx, sendID, store.FinalizeAgentEmailOutboundInput{
			Claim: started.Claim, State: store.AgentEmailOutboundDelivered,
			Provider: agentEmailOutboundProvider, ProviderMessageID: response.ProviderMessageID,
		})
		return store.AgentEmailOutboundDelivered, err
	case agentemailoutbound.StatePermanentBounce:
		_, err = st.FinalizeAgentEmailOutbound(settleCtx, sendID, store.FinalizeAgentEmailOutboundInput{
			Claim: started.Claim, State: store.AgentEmailOutboundBounced,
			Provider:  agentEmailOutboundProvider,
			ErrorCode: store.AgentEmailOutboundErrorRecipientHardBounce,
		})
		return store.AgentEmailOutboundBounced, err
	case agentemailoutbound.StateRejected:
		_, err = st.FinalizeAgentEmailOutbound(settleCtx, sendID, store.FinalizeAgentEmailOutboundInput{
			Claim: started.Claim, State: store.AgentEmailOutboundRejected,
			Provider:  agentEmailOutboundProvider,
			ErrorCode: store.AgentEmailOutboundErrorProviderRejected,
		})
		return store.AgentEmailOutboundRejected, err
	case agentemailoutbound.StateRetryable:
		retryAfter := time.Duration(response.RetryAfterSeconds) * time.Second
		if retryAfter <= 0 {
			retryAfter = time.Second
		}
		errorCode := store.AgentEmailOutboundErrorProviderFailed
		if strings.Contains(response.ErrorCode, "rate") || strings.Contains(response.ErrorCode, "daily") {
			errorCode = store.AgentEmailOutboundErrorProviderRateLimited
		}
		return retryAgentEmailOutboundDispatch(
			settleCtx, st, started, time.Now().UTC().Add(retryAfter), errorCode, false,
		)
	case agentemailoutbound.StateAmbiguous:
		return retryAgentEmailOutboundDispatch(
			settleCtx, st, started, time.Now().UTC().Add(time.Second),
			agentEmailOutboundAmbiguousError(sendErr),
			true,
		)
	default:
		return retryAgentEmailOutboundDispatch(
			settleCtx, st, started, time.Now().UTC().Add(time.Second),
			store.AgentEmailOutboundErrorProviderResponseInvalid,
			true,
		)
	}
}

func retryAgentEmailOutboundDispatch(
	ctx context.Context,
	st agentEmailOutboundWorkerStore,
	dispatch store.AgentEmailOutboundDispatch,
	retryAt time.Time,
	errorCode string,
	preserveProviderBoundary bool,
) (string, error) {
	msg, err := st.RetryAgentEmailOutbound(
		ctx,
		dispatch.Message.ID,
		store.RetryAgentEmailOutboundInput{
			Claim: dispatch.Claim, RetryAt: retryAt, ErrorCode: errorCode,
			Provider:                 agentEmailOutboundProvider,
			PreserveProviderBoundary: preserveProviderBoundary,
		},
	)
	if err != nil {
		return "", err
	}
	switch msg.State {
	case store.AgentEmailOutboundQueued, store.AgentEmailOutboundProviderStarted:
		return store.AgentEmailOutboundQueued, nil
	case store.AgentEmailOutboundFailed, store.AgentEmailOutboundCanceled,
		store.AgentEmailOutboundAmbiguous:
		return msg.State, nil
	default:
		return "", fmt.Errorf("outbound agent-email retry returned state %q", msg.State)
	}
}

func markAgentEmailOutboundDispatchAmbiguous(
	ctx context.Context,
	st agentEmailOutboundWorkerStore,
	dispatch store.AgentEmailOutboundDispatch,
	errorCode string,
) (string, error) {
	_, err := st.MarkAgentEmailOutboundAmbiguous(
		ctx,
		dispatch.Message.ID,
		store.AmbiguousAgentEmailOutboundInput{
			Claim: dispatch.Claim, Provider: agentEmailOutboundProvider, ErrorCode: errorCode,
		},
	)
	return store.AgentEmailOutboundAmbiguous, err
}

func agentEmailOutboundAmbiguousError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return store.AgentEmailOutboundErrorProviderTimeout
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return store.AgentEmailOutboundErrorProviderTimeout
	}
	if errors.Is(err, agentemailoutbound.ErrInvalidResponse) || err == nil {
		return store.AgentEmailOutboundErrorProviderResponseInvalid
	}
	return store.AgentEmailOutboundErrorProviderConnectionReset
}

func agentEmailOutboundSettlementContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
}
