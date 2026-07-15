package messagerunner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/client"
)

const (
	requestOperationOffer   = "offer"
	requestOperationSelect  = "select"
	requestOperationExecute = "execute"

	requestPhaseCollectingOffers  = "collecting_offers"
	requestPhaseAwaitingSelection = "awaiting_selection"
	requestPhaseAssigned          = "assigned"
	requestWorkPageLimit          = 100
	requestScanSelected           = "selected"
	requestScanPending            = "pending"
	requestScanCoordinator        = "coordinator"
	requestScanPhaseCount         = 3
	requestScanPageBudget         = requestScanPhaseCount
)

type messageRequestScanLane struct {
	operation string
	options   client.MessageRequestListOptions
	visit     func(client.MessageRequest) (bool, RunResult, error)
}

// runMessageRequestOnce handles at most one open-request protocol transition.
// It deliberately runs before the ordinary mailbox so a fanout opening can be
// evaluated before that non-actionable notification is acknowledged.
func (r Runner) runMessageRequestOnce(
	ctx context.Context,
	api MessageRequestAPI,
	identity client.SelfIdentity,
	config Config,
	checkpoint *RequestScanCheckpoint,
) (bool, RunResult, error) {
	if checkpoint == nil {
		return false, RunResult{}, errors.New("message request scan checkpoint is required")
	}
	if checkpoint.Cursors == nil {
		checkpoint.Cursors = map[string]string{}
	}
	lanes := []messageRequestScanLane{
		{
			operation: requestScanSelected,
			options: client.MessageRequestListOptions{
				State: "open", Phase: requestPhaseAssigned, Role: "candidate", Limit: requestWorkPageLimit,
			},
			visit: func(request client.MessageRequest) (bool, RunResult, error) {
				if err := validateMessageRequestListItem(request, identity, requestPhaseAssigned); err != nil {
					return false, RunResult{RequestID: request.ID}, err
				}
				if !containsString(request.SelectedAgentIDs, identity.AgentID) {
					return false, RunResult{}, nil
				}
				detail, err := api.GetMessageRequest(ctx, request.ID)
				if err != nil {
					return false, RunResult{RequestID: request.ID}, fmt.Errorf("read selected message request: %w", err)
				}
				claim, selected, err := selectedRequestClaim(detail, identity)
				if err != nil {
					return false, requestRunResult(detail), err
				}
				if !selected {
					return false, RunResult{}, nil
				}
				result, err := r.executeSelectedMessageRequest(ctx, api, detail, claim, identity, config)
				return true, result, err
			},
		},
		{
			operation: requestScanPending,
			options: client.MessageRequestListOptions{
				State: "open", Phase: requestPhaseCollectingOffers, Role: "candidate", Limit: requestWorkPageLimit,
			},
			visit: func(request client.MessageRequest) (bool, RunResult, error) {
				if err := validateMessageRequestListItem(request, identity, requestPhaseCollectingOffers); err != nil {
					return false, RunResult{RequestID: request.ID}, err
				}
				detail, err := api.GetMessageRequest(ctx, request.ID)
				if err != nil {
					return false, RunResult{RequestID: request.ID}, fmt.Errorf("read pending message request: %w", err)
				}
				isPending, err := pendingRequestCandidate(detail, identity)
				if err != nil {
					return false, requestRunResult(detail), err
				}
				if !isPending {
					return false, RunResult{}, nil
				}
				result, err := r.respondToPendingMessageRequest(ctx, api, detail, identity, config)
				return true, result, err
			},
		},
		{
			operation: requestScanCoordinator,
			options: client.MessageRequestListOptions{
				State: "open", Phase: requestPhaseAwaitingSelection, Role: "coordinator", Limit: requestWorkPageLimit,
			},
			visit: func(request client.MessageRequest) (bool, RunResult, error) {
				if err := validateMessageRequestListItem(request, identity, requestPhaseAwaitingSelection); err != nil {
					return false, RunResult{RequestID: request.ID}, err
				}
				if request.Coordinator.AgentID != identity.AgentID {
					return false, RunResult{RequestID: request.ID}, errors.New("server returned another coordinator's message request")
				}
				if request.OfferCount == 0 {
					return false, RunResult{}, nil
				}
				detail, err := api.GetMessageRequest(ctx, request.ID)
				if err != nil {
					return false, RunResult{RequestID: request.ID}, fmt.Errorf("read message request awaiting selection: %w", err)
				}
				offers, capacity, err := selectableRequestOffers(detail, identity)
				if err != nil {
					return false, requestRunResult(detail), err
				}
				if len(offers) == 0 || capacity == 0 {
					return false, RunResult{}, nil
				}
				result, err := r.selectMessageRequestOffers(ctx, api, detail, offers, capacity, identity, config)
				return true, result, err
			},
		},
	}

	for scanned := 0; scanned < requestScanPageBudget; scanned++ {
		laneIndex := checkpoint.NextPhase
		lane := lanes[laneIndex]
		opts := lane.options
		opts.Cursor = checkpoint.Cursors[lane.operation]
		page, err := api.ListMessageRequests(ctx, opts)
		if err != nil {
			return false, RunResult{}, fmt.Errorf("list %s message requests: %w", lane.operation, err)
		}
		for _, request := range page.Requests {
			handled, result, err := lane.visit(request)
			if err != nil || handled {
				checkpoint.NextPhase = (laneIndex + 1) % requestScanPhaseCount
				return handled, result, err
			}
		}
		next := strings.TrimSpace(page.NextCursor)
		if len(next) > maximumRequestScanCursorBytes {
			return false, RunResult{}, errors.New("server returned an oversized message request cursor")
		}
		if next != "" && next == opts.Cursor {
			return false, RunResult{}, errors.New("server repeated a message request cursor")
		}
		if next == "" {
			delete(checkpoint.Cursors, lane.operation)
		} else {
			checkpoint.Cursors[lane.operation] = next
		}
		checkpoint.NextPhase = (laneIndex + 1) % requestScanPhaseCount
	}
	return false, RunResult{}, nil
}

func (r Runner) respondToPendingMessageRequest(
	ctx context.Context,
	api MessageRequestAPI,
	detail client.MessageRequestDetail,
	identity client.SelfIdentity,
	config Config,
) (RunResult, error) {
	result := requestRunResult(detail)
	envelope := requestTurnEnvelope(detail, identity, config, requestOperationOffer,
		[]string{OutcomeResult, OutcomeDecline}, requestObjectiveBody(detail, config.MaximumBodyBytes))
	providerResult, err := r.invokeRequestProvider(ctx, envelope)
	if err != nil {
		return result, err
	}
	if providerResult.Outcome == OutcomeDecline {
		declined, err := api.DeclineMessageRequest(ctx, detail.Request.ID,
			requestDeclineIdempotencyKey(config.RunnerID, detail.Request.ID))
		if err != nil {
			return result, fmt.Errorf("decline message request: %w", err)
		}
		if err := validateMessageRequest(declined, identity, detail.Request.ID); err != nil {
			return result, err
		}
		if declined.State != "open" {
			return result, errors.New("server returned an invalid declined message request")
		}
		result.Status = RunStatusRequestDeclined
		return result, nil
	}
	offered, err := api.OfferMessageRequest(ctx, detail.Request.ID, client.OfferMessageRequestInput{
		Subject: providerResult.Subject, Body: providerResult.Body, Payload: providerResult.Payload,
		IdempotencyKey: requestOfferIdempotencyKey(config.RunnerID, detail.Request.ID),
	})
	if err != nil {
		return result, fmt.Errorf("offer message request: %w", err)
	}
	if err := validateMessageRequestOffer(offered, detail, identity, providerResult); err != nil {
		return result, err
	}
	result.Status = RunStatusRequestOffered
	result.ResultMessageID = offered.Offer.Message.ID
	return result, nil
}

func (r Runner) selectMessageRequestOffers(
	ctx context.Context,
	api MessageRequestAPI,
	detail client.MessageRequestDetail,
	offers []client.MessageRequestOffer,
	capacity int,
	identity client.SelfIdentity,
	config Config,
) (RunResult, error) {
	result := requestRunResult(detail)
	body := requestSelectionBody(detail, offers, capacity, config.MaximumBodyBytes)
	envelope := requestTurnEnvelope(detail, identity, config, requestOperationSelect,
		[]string{OutcomeResult}, body)
	providerResult, err := r.invokeRequestProvider(ctx, envelope)
	if err != nil {
		return result, err
	}
	selected, err := decodeSelectedRequestAgents(providerResult.Payload, offers, capacity)
	if err != nil {
		return result, ErrProviderResultInvalid
	}
	selection, err := api.SelectMessageRequest(ctx, detail.Request.ID, client.SelectMessageRequestInput{
		SelectedAgentIDs: selected, ReservationSeconds: config.LeaseSeconds,
		IdempotencyKey: requestSelectionIdempotencyKey(config.RunnerID, detail.Request.ID, detail.Request.SelectionGeneration+1),
	})
	if err != nil {
		return result, fmt.Errorf("select message request agents: %w", err)
	}
	if err := validateMessageRequestSelection(selection, detail, identity, selected); err != nil {
		return result, err
	}
	result.Status = RunStatusRequestSelected
	return result, nil
}

func (r Runner) executeSelectedMessageRequest(
	ctx context.Context,
	api MessageRequestAPI,
	detail client.MessageRequestDetail,
	reservation client.MessageRequestClaim,
	identity client.SelfIdentity,
	config Config,
) (RunResult, error) {
	result := requestRunResult(detail)
	claimed, err := api.ClaimMessageRequest(ctx, detail.Request.ID, client.ClaimMessageRequestInput{
		LeaseSeconds:   config.LeaseSeconds,
		IdempotencyKey: requestClaimIdempotencyKey(config.RunnerID, detail.Request.ID, reservation.ClaimID),
	})
	if err != nil {
		return result, fmt.Errorf("claim selected message request: %w", err)
	}
	if err := validateClaimedMessageRequest(claimed, detail.Request.ID, identity, reservation); err != nil {
		return result, err
	}
	failureCount, err := messageRequestDeterministicFailures(detail, claimed, int64(config.MaximumFailures))
	if err != nil {
		return result, r.releaseMessageRequestAfterError(ctx, api, detail.Request.ID, claimed, false, err)
	}
	if failureCount >= int64(config.MaximumFailures) {
		return r.completeSelectedMessageRequest(ctx, api, detail, identity, config, claimed,
			messageRequestEscalation(config), nil, true)
	}
	envelope := requestTurnEnvelope(detail, identity, config, requestOperationExecute,
		[]string{OutcomeResult}, requestObjectiveBody(detail, config.MaximumBodyBytes))
	providerResult, err := r.invokeMessageRequestWithLease(ctx, api, config, detail.Request.ID, claimed, envelope)
	if err != nil {
		deterministic := requestFailureIsDeterministic(err)
		if deterministic && failureCount+1 >= int64(config.MaximumFailures) {
			return r.completeSelectedMessageRequest(ctx, api, detail, identity, config, claimed,
				messageRequestEscalation(config), err, true)
		}
		return result, r.releaseMessageRequestAfterError(ctx, api, detail.Request.ID, claimed,
			deterministic, err)
	}
	return r.completeSelectedMessageRequest(ctx, api, detail, identity, config, claimed, providerResult, nil, false)
}

func messageRequestEscalation(config Config) TurnResult {
	return TurnResult{
		Schema: TurnResultSchemaV1, Outcome: OutcomeResult, Subject: "Automated request escalation",
		Body: truncateUTF8("Automated handling could not safely complete after repeated attempts and requires review.", config.MaximumBodyBytes),
	}
}

func (r Runner) completeSelectedMessageRequest(
	ctx context.Context,
	api MessageRequestAPI,
	detail client.MessageRequestDetail,
	identity client.SelfIdentity,
	config Config,
	claim client.MessageRequestClaim,
	providerResult TurnResult,
	priorCause error,
	deterministicFailure bool,
) (RunResult, error) {
	result := requestRunResult(detail)
	completed, err := api.CompleteMessageRequest(ctx, detail.Request.ID, client.CompleteMessageRequestInput{
		ClaimID: claim.ClaimID, Generation: claim.Generation,
		Subject: providerResult.Subject, Body: providerResult.Body, Payload: providerResult.Payload,
		IdempotencyKey: requestCompletionIdempotencyKey(config.RunnerID, detail.Request.ID, claim.ClaimID, claim.Generation),
	})
	if err != nil {
		cause := fmt.Errorf("complete selected message request: %w", err)
		if priorCause != nil {
			cause = errors.Join(priorCause, cause)
		}
		return result, r.releaseMessageRequestAfterError(ctx, api, detail.Request.ID, claim, deterministicFailure, cause)
	}
	if err := validateCompletedMessageRequest(completed, detail, identity, claim, providerResult); err != nil {
		return result, err
	}
	result.Status = RunStatusRequestCompleted
	result.ResultMessageID = completed.Message.ID
	return result, nil
}

func messageRequestDeterministicFailures(
	detail client.MessageRequestDetail,
	claimed client.MessageRequestClaim,
	limit int64,
) (int64, error) {
	var failures int64
	seen := make(map[string]struct{}, len(detail.Claims)+1)
	currentFound := false
	for _, prior := range detail.Claims {
		if prior.ClaimID == "" || prior.RequestID != detail.Request.ID ||
			prior.Agent.AgentID != claimed.Agent.AgentID || prior.FailureCount < 0 {
			return 0, errors.New("server returned an invalid message request failure history")
		}
		if _, duplicate := seen[prior.ClaimID]; duplicate {
			return 0, errors.New("server returned duplicate message request failure history")
		}
		seen[prior.ClaimID] = struct{}{}
		count := prior.FailureCount
		if prior.ClaimID == claimed.ClaimID {
			count = claimed.FailureCount
			currentFound = true
		}
		if failures >= limit || count >= limit-failures {
			return limit, nil
		}
		failures += count
	}
	if !currentFound {
		if failures >= limit || claimed.FailureCount >= limit-failures {
			return limit, nil
		}
		failures += claimed.FailureCount
	}
	return failures, nil
}

func (r Runner) invokeRequestProvider(ctx context.Context, envelope TurnEnvelope) (TurnResult, error) {
	if err := validateTurnEnvelope(envelope); err != nil {
		return TurnResult{}, fmt.Errorf("validate message request provider envelope: %w", err)
	}
	providerResult, err := r.Provider.Invoke(ctx, envelope)
	if err != nil {
		return TurnResult{}, fmt.Errorf("invoke message request provider: %w", err)
	}
	if err := validateTurnResult(providerResult, envelope.Policy); err != nil {
		return TurnResult{}, ErrProviderResultInvalid
	}
	return providerResult, nil
}

func (r Runner) invokeMessageRequestWithLease(
	ctx context.Context,
	api MessageRequestAPI,
	config Config,
	requestID string,
	claim client.MessageRequestClaim,
	envelope TurnEnvelope,
) (TurnResult, error) {
	if err := validateTurnEnvelope(envelope); err != nil {
		return TurnResult{}, fmt.Errorf("validate message request provider envelope: %w", err)
	}
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
				return TurnResult{}, fmt.Errorf("invoke message request provider: %w", call.err)
			}
			if err := validateTurnResult(call.result, envelope.Policy); err != nil {
				return TurnResult{}, ErrProviderResultInvalid
			}
			return call.result, nil
		case <-ticker.C:
			renewed, err := api.RenewMessageRequest(ctx, requestID, client.RenewMessageRequestInput{
				ClaimID: claim.ClaimID, Generation: claim.Generation, LeaseSeconds: config.LeaseSeconds,
			})
			if err != nil {
				cancel()
				return TurnResult{}, errors.Join(errMessageClaimMaintenance, fmt.Errorf("renew message request claim: %w", err))
			}
			if err := validateRenewedMessageRequest(renewed, claim); err != nil {
				cancel()
				return TurnResult{}, errors.Join(errMessageClaimMaintenance, err)
			}
		}
	}
}

func (r Runner) releaseMessageRequestAfterError(
	ctx context.Context,
	api MessageRequestAPI,
	requestID string,
	claim client.MessageRequestClaim,
	deterministicFailure bool,
	cause error,
) error {
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	_, releaseErr := api.ReleaseMessageRequest(releaseCtx, requestID, client.ReleaseMessageRequestInput{
		ClaimID: claim.ClaimID, Generation: claim.Generation, DeterministicFailure: deterministicFailure,
	})
	if releaseErr == nil {
		return cause
	}
	return errors.Join(cause, fmt.Errorf("release message request claim: %w", releaseErr))
}

func requestFailureIsDeterministic(err error) bool {
	return !errors.Is(err, ErrProviderUnavailable) && !errors.Is(err, ErrNativeProviderCommand) &&
		!errors.Is(err, ErrNativeProviderUnsupported) && !errors.Is(err, ErrInvalidConfiguration) &&
		!errors.Is(err, errMessageClaimMaintenance) && !errors.Is(err, context.Canceled) &&
		!errors.Is(err, context.DeadlineExceeded)
}

func requestTurnEnvelope(
	detail client.MessageRequestDetail,
	identity client.SelfIdentity,
	config Config,
	operation string,
	allowedOutcomes []string,
	body string,
) TurnEnvelope {
	message := detail.OpeningMessage
	message.To = client.MessageRecipient{
		Kind: "agent", AgentID: identity.AgentID, AgentName: identity.AgentName,
	}
	message.Kind = "message_request_" + operation
	message.Body = truncateUTF8(body, config.MaximumBodyBytes)
	message.Payload = boundedRequestOpeningPayload(detail.OpeningMessage.Payload)
	message.CausalDepth = 1
	message.Delivery = client.MessageDelivery{}
	message.ReadState = client.MessageReadState{}
	message.Processing = client.MessageProcessing{}
	return TurnEnvelope{
		Schema: TurnEnvelopeSchemaV1, Identity: identity, Message: message,
		Policy: TurnPolicy{
			AllowedOutcomes: allowedOutcomes, MaximumBodyBytes: config.MaximumBodyBytes,
			MessageIsUntrusted: true, CurrentTurn: 1, MaximumTurns: 1,
			RequestOperation: operation,
		},
	}
}

func boundedRequestOpeningPayload(payload json.RawMessage) json.RawMessage {
	if len(payload) == 0 || len(payload) > maximumProviderPayloadBytes {
		return nil
	}
	var object map[string]any
	if json.Unmarshal(payload, &object) != nil || object == nil {
		return nil
	}
	return append(json.RawMessage(nil), payload...)
}

func requestObjectiveBody(detail client.MessageRequestDetail, maximumBytes int) string {
	return truncateUTF8(detail.OpeningMessage.Body, maximumBytes)
}

func requestSelectionBody(
	detail client.MessageRequestDetail,
	offers []client.MessageRequestOffer,
	capacity int,
	maximumBytes int,
) string {
	ids := make([]string, len(offers))
	for i := range offers {
		ids[i] = offers[i].Agent.AgentID
	}
	encodedIDs, _ := json.Marshal(ids)
	var body strings.Builder
	fmt.Fprintf(&body, "Choose 1-%d agents for this open request. Return only offered IDs in payload.selected_agent_ids.\nEligible offered agent IDs: %s\n\nObjective:\n%s\n\nDurable offers:\n",
		capacity, encodedIDs, truncateUTF8(detail.OpeningMessage.Body, maximumBytes/3))
	remaining := maximumBytes - body.Len()
	perOffer := 0
	if remaining > 0 && len(offers) != 0 {
		perOffer = remaining / len(offers)
	}
	for _, offer := range offers {
		entry := fmt.Sprintf("\nagent_id=%s\nagent_name=%s\nsubject=%s\nbody=%s\n",
			offer.Agent.AgentID, offer.Agent.AgentName, offer.Message.Subject, offer.Message.Body)
		if perOffer > 0 {
			entry = truncateUTF8(entry, perOffer)
		}
		body.WriteString(entry)
	}
	return truncateUTF8(body.String(), maximumBytes)
}

func requestRunResult(detail client.MessageRequestDetail) RunResult {
	return RunResult{
		RequestID: detail.Request.ID, MessageID: detail.OpeningMessage.ID,
		ThreadID: detail.OpeningMessage.ThreadID,
	}
}

func validateMessageRequest(request client.MessageRequest, identity client.SelfIdentity, requestID string) error {
	if request.ID == "" || request.ID != requestID || request.AccountID != identity.AccountID ||
		request.RealmID != identity.RealmID || request.OpeningMessageID == "" ||
		request.Coordinator.AgentID == "" || request.MaxAssignees < 1 || request.MaxAssignees > 8 ||
		request.CandidateCount < 1 || request.SelectionGeneration < 0 {
		return errors.New("server returned a message request outside the runner's pinned identity")
	}
	return nil
}

func validateMessageRequestListItem(request client.MessageRequest, identity client.SelfIdentity, phase string) error {
	if err := validateMessageRequest(request, identity, request.ID); err != nil {
		return err
	}
	if request.State != "open" || request.Phase != phase {
		return errors.New("server returned a message request outside the requested work phase")
	}
	return nil
}

func validateMessageRequestDetail(detail client.MessageRequestDetail, identity client.SelfIdentity) error {
	if err := validateMessageRequest(detail.Request, identity, detail.Request.ID); err != nil {
		return err
	}
	opening := detail.OpeningMessage
	if opening.ID != detail.Request.OpeningMessageID || opening.AccountID != identity.AccountID ||
		opening.RealmID != identity.RealmID || opening.From.AgentID != detail.Request.Coordinator.AgentID ||
		opening.To.Kind != "realm" || opening.To.AgentID != "" || opening.To.Count != detail.Request.CandidateCount ||
		opening.Kind != "open_request" || opening.ThreadID == "" || opening.CausalDepth < 1 {
		return errors.New("server returned an invalid message request opening")
	}
	return nil
}

func pendingRequestCandidate(detail client.MessageRequestDetail, identity client.SelfIdentity) (bool, error) {
	if err := validateMessageRequestDetail(detail, identity); err != nil {
		return false, err
	}
	if detail.Request.State != "open" || detail.Request.Phase != requestPhaseCollectingOffers {
		return false, nil
	}
	pending := 0
	for _, candidate := range detail.Candidates {
		if candidate.Agent.AgentID != identity.AgentID {
			return false, errors.New("server returned another candidate's request detail")
		}
		if candidate.ResponseState == "pending" {
			pending++
		}
	}
	if pending > 1 {
		return false, errors.New("server returned duplicate message request candidates")
	}
	return pending == 1, nil
}

func selectedRequestClaim(
	detail client.MessageRequestDetail,
	identity client.SelfIdentity,
) (client.MessageRequestClaim, bool, error) {
	if err := validateMessageRequestDetail(detail, identity); err != nil {
		return client.MessageRequestClaim{}, false, err
	}
	if detail.Request.State != "open" || detail.Request.Phase != requestPhaseAssigned ||
		!containsString(detail.Request.SelectedAgentIDs, identity.AgentID) {
		return client.MessageRequestClaim{}, false, nil
	}
	selectionGenerations := make(map[string]int64, len(detail.Selections))
	generationOwners := make(map[int64]string, len(detail.Selections))
	for _, selection := range detail.Selections {
		if selection.ID == "" || selection.Generation < 1 ||
			selection.Generation > detail.Request.SelectionGeneration {
			return client.MessageRequestClaim{}, false, errors.New("server returned an invalid message request selection")
		}
		if _, duplicate := selectionGenerations[selection.ID]; duplicate {
			return client.MessageRequestClaim{}, false, errors.New("server returned duplicate message request selections")
		}
		if owner, duplicate := generationOwners[selection.Generation]; duplicate && owner != selection.ID {
			return client.MessageRequestClaim{}, false, errors.New("server returned duplicate message request selection generations")
		}
		selectionGenerations[selection.ID] = selection.Generation
		generationOwners[selection.Generation] = selection.ID
	}
	var selected *client.MessageRequestClaim
	var selectedGeneration int64
	seen := make(map[string]struct{}, len(detail.Claims))
	for _, claim := range detail.Claims {
		if claim.Agent.AgentID != identity.AgentID || claim.RequestID != detail.Request.ID {
			return client.MessageRequestClaim{}, false, errors.New("server returned another agent's message request claim")
		}
		if claim.ClaimID == "" {
			return client.MessageRequestClaim{}, false, errors.New("server returned a message request claim without an id")
		}
		if _, duplicate := seen[claim.ClaimID]; duplicate {
			return client.MessageRequestClaim{}, false, errors.New("server returned duplicate message request claims")
		}
		seen[claim.ClaimID] = struct{}{}
		selectionGeneration, ok := selectionGenerations[claim.SelectionID]
		if !ok {
			return client.MessageRequestClaim{}, false, errors.New("server returned a claim without its immutable selection generation")
		}
		if claim.State == "reserved" || claim.State == "claimed" {
			candidate := claim
			if selected == nil || selectionGeneration > selectedGeneration {
				selected = &candidate
				selectedGeneration = selectionGeneration
				continue
			}
			if selectionGeneration == selectedGeneration {
				return client.MessageRequestClaim{}, false, errors.New("server returned ambiguous active message request claims")
			}
		}
	}
	if selected == nil {
		return client.MessageRequestClaim{}, false, errors.New("server returned an invalid selected message request claim")
	}
	return *selected, true, nil
}

func selectableRequestOffers(
	detail client.MessageRequestDetail,
	identity client.SelfIdentity,
) ([]client.MessageRequestOffer, int, error) {
	if err := validateMessageRequestDetail(detail, identity); err != nil {
		return nil, 0, err
	}
	if detail.Request.State != "open" || detail.Request.Phase != requestPhaseAwaitingSelection ||
		detail.Request.Coordinator.AgentID != identity.AgentID {
		return nil, 0, nil
	}
	usedAgents := make(map[string]struct{}, len(detail.Request.SelectedAgentIDs)+len(detail.Claims))
	usedCapacity := 0
	for _, agentID := range detail.Request.SelectedAgentIDs {
		if _, exists := usedAgents[agentID]; !exists {
			usedAgents[agentID] = struct{}{}
			usedCapacity++
		}
	}
	for _, claim := range detail.Claims {
		if claim.RequestID != detail.Request.ID || claim.Agent.AgentID == "" {
			return nil, 0, errors.New("server returned an invalid coordinator claim detail")
		}
		if claim.State == "completed" {
			if _, exists := usedAgents[claim.Agent.AgentID]; !exists {
				usedAgents[claim.Agent.AgentID] = struct{}{}
				usedCapacity++
			}
		}
	}
	capacity := detail.Request.MaxAssignees - usedCapacity
	if capacity <= 0 {
		return nil, 0, nil
	}
	offeredCandidates := make(map[string]string, len(detail.Candidates))
	candidatesSeen := make(map[string]struct{}, len(detail.Candidates))
	for _, candidate := range detail.Candidates {
		if candidate.Agent.AgentID == "" {
			return nil, 0, errors.New("server returned an invalid message request candidate")
		}
		if _, duplicate := candidatesSeen[candidate.Agent.AgentID]; duplicate {
			return nil, 0, errors.New("server returned duplicate message request candidates")
		}
		candidatesSeen[candidate.Agent.AgentID] = struct{}{}
		if candidate.ResponseState == "offered" {
			offeredCandidates[candidate.Agent.AgentID] = candidate.OfferMessageID
		}
	}
	offers := make([]client.MessageRequestOffer, 0, len(detail.Offers))
	seen := make(map[string]struct{}, len(detail.Offers))
	for _, offer := range detail.Offers {
		agentID := offer.Agent.AgentID
		offerMessageID, isOffered := offeredCandidates[agentID]
		if !isOffered || offerMessageID == "" || offer.Message.ID != offerMessageID ||
			offer.Message.AccountID != identity.AccountID || offer.Message.RealmID != identity.RealmID ||
			offer.Message.From.AgentID != agentID || offer.Message.To.Kind != "agent" ||
			offer.Message.To.AgentID != identity.AgentID ||
			offer.Message.Kind != "offer" || offer.Message.ReplyToMessageID != detail.OpeningMessage.ID {
			return nil, 0, errors.New("server returned an invalid durable message request offer")
		}
		if _, duplicate := seen[agentID]; duplicate {
			return nil, 0, errors.New("server returned duplicate durable message request offers")
		}
		seen[agentID] = struct{}{}
		if _, used := usedAgents[agentID]; !used {
			offers = append(offers, offer)
		}
	}
	if len(seen) != len(offeredCandidates) {
		return nil, 0, errors.New("server returned an incomplete durable message request offer set")
	}
	sort.Slice(offers, func(i, j int) bool { return offers[i].Agent.AgentID < offers[j].Agent.AgentID })
	return offers, capacity, nil
}

func decodeSelectedRequestAgents(
	payload json.RawMessage,
	offers []client.MessageRequestOffer,
	capacity int,
) ([]string, error) {
	var selection struct {
		SelectedAgentIDs []string `json:"selected_agent_ids"`
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if len(payload) == 0 || decoder.Decode(&selection) != nil {
		return nil, errors.New("message request selection payload is invalid")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("message request selection payload has trailing data")
	}
	if len(selection.SelectedAgentIDs) == 0 || len(selection.SelectedAgentIDs) > capacity {
		return nil, errors.New("message request selection exceeds available capacity")
	}
	offered := make(map[string]struct{}, len(offers))
	for _, offer := range offers {
		offered[offer.Agent.AgentID] = struct{}{}
	}
	seen := make(map[string]struct{}, len(selection.SelectedAgentIDs))
	for _, agentID := range selection.SelectedAgentIDs {
		if agentID == "" || strings.TrimSpace(agentID) != agentID {
			return nil, errors.New("message request selection contains an invalid agent id")
		}
		if _, exists := offered[agentID]; !exists {
			return nil, errors.New("message request selection contains a non-offering agent")
		}
		if _, duplicate := seen[agentID]; duplicate {
			return nil, errors.New("message request selection contains a duplicate agent")
		}
		seen[agentID] = struct{}{}
	}
	selected := append([]string(nil), selection.SelectedAgentIDs...)
	sort.Strings(selected)
	return selected, nil
}

func validateMessageRequestOffer(
	offered client.OfferMessageRequestResult,
	detail client.MessageRequestDetail,
	identity client.SelfIdentity,
	providerResult TurnResult,
) error {
	if err := validateMessageRequest(offered.Request, identity, detail.Request.ID); err != nil {
		return err
	}
	message := offered.Offer.Message
	if offered.Request.State != "open" || offered.Offer.Agent.AgentID != identity.AgentID || message.ID == "" ||
		message.AccountID != identity.AccountID || message.RealmID != identity.RealmID ||
		message.From.AgentID != identity.AgentID || message.To.Kind != "agent" ||
		message.To.AgentID != detail.Request.Coordinator.AgentID ||
		message.Kind != "offer" || message.ReplyToMessageID != detail.OpeningMessage.ID ||
		message.ThreadID != detail.OpeningMessage.ThreadID ||
		message.CausalDepth != detail.OpeningMessage.CausalDepth+1 ||
		message.Subject != providerResult.Subject || message.Body != providerResult.Body ||
		!bytes.Equal(message.Payload, providerResult.Payload) {
		return errors.New("server returned an invalid message request offer")
	}
	return nil
}

func validateMessageRequestSelection(
	selected client.SelectMessageRequestResult,
	detail client.MessageRequestDetail,
	identity client.SelfIdentity,
	agentIDs []string,
) error {
	if err := validateMessageRequest(selected.Request, identity, detail.Request.ID); err != nil {
		return err
	}
	want := append([]string(nil), agentIDs...)
	got := append([]string(nil), selected.Selection.SelectedAgentIDs...)
	sort.Strings(want)
	sort.Strings(got)
	if selected.Selection.ID == "" || selected.Selection.Generation != detail.Request.SelectionGeneration+1 ||
		selected.Selection.Coordinator.AgentID != identity.AgentID || !equalStrings(got, want) ||
		selected.Request.State != "open" || selected.Request.Phase != requestPhaseAssigned ||
		selected.Request.SelectionGeneration != selected.Selection.Generation || len(selected.Claims) != len(want) {
		return errors.New("server returned an invalid message request selection")
	}
	for _, agentID := range want {
		if !containsString(selected.Request.SelectedAgentIDs, agentID) {
			return errors.New("server returned a message request without its selected agent")
		}
	}
	seen := make(map[string]struct{}, len(selected.Claims))
	for _, claim := range selected.Claims {
		if claim.ClaimID == "" || claim.RequestID != detail.Request.ID ||
			claim.SelectionID != selected.Selection.ID || claim.State != "reserved" ||
			!containsString(want, claim.Agent.AgentID) {
			return errors.New("server returned an invalid message request reservation")
		}
		if _, duplicate := seen[claim.Agent.AgentID]; duplicate {
			return errors.New("server returned duplicate message request reservations")
		}
		seen[claim.Agent.AgentID] = struct{}{}
	}
	return nil
}

func validateClaimedMessageRequest(
	claim client.MessageRequestClaim,
	requestID string,
	identity client.SelfIdentity,
	reservation client.MessageRequestClaim,
) error {
	if claim.ClaimID == "" || claim.ClaimID != reservation.ClaimID || claim.RequestID != requestID ||
		claim.SelectionID != reservation.SelectionID ||
		claim.Agent.AgentID != identity.AgentID || claim.State != "claimed" || claim.Generation < 1 ||
		claim.FailureCount < 0 || claim.LeaseExpiresAt == nil {
		return errors.New("server returned an invalid claimed message request")
	}
	return nil
}

func validateRenewedMessageRequest(renewed, claim client.MessageRequestClaim) error {
	if renewed.ClaimID != claim.ClaimID || renewed.RequestID != claim.RequestID ||
		renewed.SelectionID != claim.SelectionID || renewed.Agent.AgentID != claim.Agent.AgentID ||
		renewed.State != "claimed" || renewed.Generation != claim.Generation ||
		renewed.FailureCount != claim.FailureCount || renewed.LeaseExpiresAt == nil {
		return errors.New("renewed message request claim changed unexpectedly")
	}
	return nil
}

func validateCompletedMessageRequest(
	completed client.CompleteMessageRequestResult,
	detail client.MessageRequestDetail,
	identity client.SelfIdentity,
	claim client.MessageRequestClaim,
	providerResult TurnResult,
) error {
	if err := validateMessageRequest(completed.Request, identity, detail.Request.ID); err != nil {
		return err
	}
	if completed.Claim.ClaimID != claim.ClaimID || completed.Claim.RequestID != detail.Request.ID ||
		completed.Claim.Agent.AgentID != identity.AgentID || completed.Claim.State != "completed" ||
		completed.Claim.Generation != claim.Generation || completed.Claim.ResultMessageID == "" ||
		completed.Claim.FailureCount != claim.FailureCount ||
		completed.Claim.ResultMessageID != completed.Message.ID || completed.Message.AccountID != identity.AccountID ||
		completed.Message.RealmID != identity.RealmID || completed.Message.From.AgentID != identity.AgentID ||
		completed.Message.To.Kind != "agent" || completed.Message.To.AgentID != detail.Request.Coordinator.AgentID ||
		completed.Message.Kind != "result" ||
		completed.Message.ReplyToMessageID != detail.OpeningMessage.ID ||
		completed.Message.ThreadID != detail.OpeningMessage.ThreadID ||
		completed.Message.CausalDepth != detail.OpeningMessage.CausalDepth+1 ||
		completed.Message.Subject != providerResult.Subject || completed.Message.Body != providerResult.Body ||
		!bytes.Equal(completed.Message.Payload, providerResult.Payload) {
		return errors.New("server returned an invalid completed message request")
	}
	return nil
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func requestOfferIdempotencyKey(runnerID, requestID string) string {
	return "message-runner/request-offer/" + runnerID + "/" + requestID
}

func requestDeclineIdempotencyKey(runnerID, requestID string) string {
	return "message-runner/request-decline/" + runnerID + "/" + requestID
}

func requestSelectionIdempotencyKey(runnerID, requestID string, generation int64) string {
	return fmt.Sprintf("message-runner/request-select/%s/%s/%d", runnerID, requestID, generation)
}

func requestClaimIdempotencyKey(runnerID, requestID, claimID string) string {
	return "message-runner/request-claim/" + runnerID + "/" + requestID + "/" + claimID
}

func requestCompletionIdempotencyKey(runnerID, requestID, claimID string, generation int64) string {
	return fmt.Sprintf("message-runner/request-complete/%s/%s/%s/%d", runnerID, requestID, claimID, generation)
}
