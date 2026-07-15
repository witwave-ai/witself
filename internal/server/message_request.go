package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
)

const (
	messageRequestSmallBody             = 16 * 1024
	messageRequestLargeBody             = 96 * 1024
	maxMessageRequestOfferWindowSeconds = 15 * 60
	maxMessageRequestLifetimeSeconds    = 7 * 24 * 60 * 60
	maxMessageRequestLeaseSeconds       = 15 * 60
)

func createMessageRequestHandler(
	auth PrincipalAuthFunc,
	create func(context.Context, DomainPrincipal, CreateMessageRequestRequest) (CreateMessageRequestResult, error),
) http.HandlerFunc {
	return messageNoStore(requireDomainPrincipal(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if p.Kind != PrincipalKindAgent {
			writeJSONError(w, http.StatusForbidden, "only an agent token may create a message request")
			return
		}
		var in CreateMessageRequestRequest
		if decodeLimitedJSON(w, r, &in, messageRequestLargeBody) != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		if hasMessageRequestDerivedFields(in.MessageRequestDerivedFields) {
			writeJSONError(w, http.StatusBadRequest, "request routing and identity are derived from the agent token")
			return
		}
		if in.DryRun {
			writeJSONError(w, http.StatusBadRequest, "message request dry-run is not implemented")
			return
		}
		if in.SelectionPolicy != "" && in.SelectionPolicy != "client_ranked" {
			writeJSONError(w, http.StatusBadRequest, "selection_policy must be client_ranked")
			return
		}
		if !messageRequestSecondsWithinConversionBounds(in.OfferWindowSeconds, maxMessageRequestOfferWindowSeconds) {
			writeJSONError(w, http.StatusBadRequest, "offer_window_seconds must be between 0 and 900")
			return
		}
		if !messageRequestSecondsWithinConversionBounds(in.ExpiresInSeconds, maxMessageRequestLifetimeSeconds) {
			writeJSONError(w, http.StatusBadRequest, "expires_in_seconds must be between 0 and 604800")
			return
		}
		in.IdempotencyKey = strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		result, err := create(r.Context(), p, in)
		if writeMessageRequestError(w, err, "could not create message request") {
			return
		}
		result.OpeningMessage = redactMessageProcessingFence(result.OpeningMessage)
		writeMessageRequestJSON(w, http.StatusCreated, map[string]any{
			"request": result.Request, "opening_message": result.OpeningMessage,
		})
	}))
}

func listMessageRequestsHandler(
	auth PrincipalAuthFunc,
	list func(context.Context, DomainPrincipal, MessageRequestListOptions) (MessageRequestPage, error),
) http.HandlerFunc {
	return messageNoStore(requireDomainPrincipal(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if p.Kind != PrincipalKindAgent {
			writeJSONError(w, http.StatusForbidden, "only an agent token may list message requests")
			return
		}
		q := r.URL.Query()
		opts := MessageRequestListOptions{
			State: q.Get("state"), Phase: q.Get("phase"), Role: q.Get("role"), Cursor: q.Get("cursor"),
		}
		if raw := q.Get("limit"); raw != "" {
			limit, err := strconv.Atoi(raw)
			if err != nil {
				writeJSONError(w, http.StatusBadRequest, "limit must be an integer")
				return
			}
			opts.Limit = limit
		}
		page, err := list(r.Context(), p, opts)
		if writeMessageRequestError(w, err, "could not list message requests") {
			return
		}
		if page.Requests == nil {
			page.Requests = []MessageRequest{}
		}
		writeMessageRequestJSON(w, http.StatusOK, map[string]any{
			"requests": page.Requests, "next_cursor": page.NextCursor,
		})
	}))
}

func getMessageRequestHandler(
	auth PrincipalAuthFunc,
	get func(context.Context, DomainPrincipal, string) (MessageRequestDetail, error),
) http.HandlerFunc {
	return messageNoStore(requireDomainPrincipal(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if p.Kind != PrincipalKindAgent {
			writeJSONError(w, http.StatusForbidden, "only an agent token may read a message request")
			return
		}
		requestID := strings.TrimSpace(r.PathValue("request"))
		if requestID == "" {
			writeJSONError(w, http.StatusNotFound, "message request not found")
			return
		}
		detail, err := get(r.Context(), p, requestID)
		if writeMessageRequestError(w, err, "could not read message request") {
			return
		}
		detail = redactMessageRequestDetail(detail)
		writeMessageRequestJSON(w, http.StatusOK, map[string]any{
			"request": detail.Request, "opening_message": detail.OpeningMessage,
			"candidates": detail.Candidates, "offers": detail.Offers,
			"selections": detail.Selections, "claims": detail.Claims,
		})
	}))
}

func messageRequestActionHandler(cfg Config) http.HandlerFunc {
	return messageNoStore(requireDomainPrincipal(cfg.AuthenticatePrincipal, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if p.Kind != PrincipalKindAgent {
			writeJSONError(w, http.StatusForbidden, "only an agent token may act on a message request")
			return
		}
		requestID, operation, ok := strings.Cut(r.PathValue("action"), ":")
		if !ok || strings.TrimSpace(requestID) == "" || !knownMessageRequestAction(operation) {
			writeJSONError(w, http.StatusNotFound, "message request action not found")
			return
		}
		switch operation {
		case "offer":
			handleMessageRequestOffer(w, r, p, requestID, cfg.OfferMessageRequest)
		case "decline":
			handleMessageRequestDecline(w, r, p, requestID, cfg.DeclineMessageRequest)
		case "select":
			handleMessageRequestSelect(w, r, p, requestID, cfg.SelectMessageRequest)
		case "cancel":
			handleMessageRequestCancel(w, r, p, requestID, cfg.CancelMessageRequest)
		case "claim":
			handleMessageRequestClaim(w, r, p, requestID, cfg.ClaimMessageRequest)
		case "renew":
			handleMessageRequestRenew(w, r, p, requestID, cfg.RenewMessageRequest)
		case "release":
			handleMessageRequestRelease(w, r, p, requestID, cfg.ReleaseMessageRequest)
		case "complete":
			handleMessageRequestComplete(w, r, p, requestID, cfg.CompleteMessageRequest)
		}
	}))
}

func knownMessageRequestAction(action string) bool {
	switch action {
	case "offer", "decline", "select", "cancel", "claim", "renew", "release", "complete":
		return true
	default:
		return false
	}
}

func handleMessageRequestOffer(
	w http.ResponseWriter,
	r *http.Request,
	p DomainPrincipal,
	requestID string,
	offer func(context.Context, DomainPrincipal, string, OfferMessageRequestRequest) (OfferMessageRequestResult, error),
) {
	if offer == nil {
		writeJSONError(w, http.StatusNotFound, "message request action not found")
		return
	}
	var in OfferMessageRequestRequest
	if decodeLimitedJSON(w, r, &in, messageRequestLargeBody) != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if hasMessageRequestDerivedFields(in.MessageRequestDerivedFields) || len(in.Kind) != 0 {
		writeJSONError(w, http.StatusBadRequest, "offer routing, kind, and identity are derived from the request and agent token")
		return
	}
	in.IdempotencyKey = strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	result, err := offer(r.Context(), p, requestID, in)
	if writeMessageRequestError(w, err, "could not offer on message request") {
		return
	}
	result.Offer.Message = redactMessageProcessingFence(result.Offer.Message)
	writeMessageRequestJSON(w, http.StatusCreated, map[string]any{
		"request": result.Request, "offer": result.Offer,
	})
}

func handleMessageRequestDecline(
	w http.ResponseWriter,
	r *http.Request,
	p DomainPrincipal,
	requestID string,
	decline func(context.Context, DomainPrincipal, string, DeclineMessageRequestRequest) (MessageRequest, error),
) {
	if decline == nil {
		writeJSONError(w, http.StatusNotFound, "message request action not found")
		return
	}
	var in DeclineMessageRequestRequest
	if decodeLimitedJSON(w, r, &in, messageRequestSmallBody) != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if hasMessageRequestDerivedFields(in.MessageRequestDerivedFields) {
		writeJSONError(w, http.StatusBadRequest, "decline identity is derived from the agent token")
		return
	}
	in.IdempotencyKey = strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	request, err := decline(r.Context(), p, requestID, in)
	if writeMessageRequestError(w, err, "could not decline message request") {
		return
	}
	writeMessageRequestJSON(w, http.StatusOK, map[string]any{"request": request})
}

func handleMessageRequestSelect(
	w http.ResponseWriter,
	r *http.Request,
	p DomainPrincipal,
	requestID string,
	selectRequest func(context.Context, DomainPrincipal, string, SelectMessageRequestRequest) (SelectMessageRequestResult, error),
) {
	if selectRequest == nil {
		writeJSONError(w, http.StatusNotFound, "message request action not found")
		return
	}
	var in SelectMessageRequestRequest
	if decodeLimitedJSON(w, r, &in, messageRequestSmallBody) != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if hasMessageRequestDerivedFields(in.MessageRequestDerivedFields) {
		writeJSONError(w, http.StatusBadRequest, "selection identity is derived from the coordinator token")
		return
	}
	if !messageRequestSecondsWithinConversionBounds(in.ReservationSeconds, maxMessageRequestLeaseSeconds) {
		writeJSONError(w, http.StatusBadRequest, "reservation_seconds must be between 0 and 900")
		return
	}
	in.IdempotencyKey = strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	result, err := selectRequest(r.Context(), p, requestID, in)
	if writeMessageRequestError(w, err, "could not select message request offers") {
		return
	}
	if result.Claims == nil {
		result.Claims = []MessageRequestClaim{}
	}
	writeMessageRequestJSON(w, http.StatusCreated, map[string]any{
		"request": result.Request, "selection": result.Selection, "claims": result.Claims,
	})
}

func handleMessageRequestCancel(
	w http.ResponseWriter,
	r *http.Request,
	p DomainPrincipal,
	requestID string,
	cancel func(context.Context, DomainPrincipal, string) (MessageRequest, error),
) {
	if cancel == nil {
		writeJSONError(w, http.StatusNotFound, "message request action not found")
		return
	}
	var in MessageRequestDerivedFields
	if decodeLimitedJSON(w, r, &in, messageRequestSmallBody) != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if hasMessageRequestDerivedFields(in) {
		writeJSONError(w, http.StatusBadRequest, "cancellation identity is derived from the coordinator token")
		return
	}
	request, err := cancel(r.Context(), p, requestID)
	if writeMessageRequestError(w, err, "could not cancel message request") {
		return
	}
	writeMessageRequestJSON(w, http.StatusOK, map[string]any{"request": request})
}

func handleMessageRequestClaim(
	w http.ResponseWriter,
	r *http.Request,
	p DomainPrincipal,
	requestID string,
	claim func(context.Context, DomainPrincipal, string, ClaimMessageRequestRequest) (MessageRequestClaim, error),
) {
	if claim == nil {
		writeJSONError(w, http.StatusNotFound, "message request action not found")
		return
	}
	var in ClaimMessageRequestRequest
	if decodeLimitedJSON(w, r, &in, messageRequestSmallBody) != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if hasMessageRequestDerivedFields(in.MessageRequestDerivedFields) {
		writeJSONError(w, http.StatusBadRequest, "claim identity is derived from the selected agent token")
		return
	}
	if !messageRequestSecondsWithinConversionBounds(in.LeaseSeconds, maxMessageRequestLeaseSeconds) {
		writeJSONError(w, http.StatusBadRequest, "lease_seconds must be between 0 and 900")
		return
	}
	in.IdempotencyKey = strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	result, err := claim(r.Context(), p, requestID, in)
	if writeMessageRequestError(w, err, "could not claim message request") {
		return
	}
	writeMessageRequestJSON(w, http.StatusOK, map[string]any{"claim": result})
}

func handleMessageRequestRenew(
	w http.ResponseWriter,
	r *http.Request,
	p DomainPrincipal,
	requestID string,
	renew func(context.Context, DomainPrincipal, string, RenewMessageRequestRequest) (MessageRequestClaim, error),
) {
	if renew == nil {
		writeJSONError(w, http.StatusNotFound, "message request action not found")
		return
	}
	var in RenewMessageRequestRequest
	if decodeLimitedJSON(w, r, &in, messageRequestSmallBody) != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if hasMessageRequestDerivedFields(in.MessageRequestDerivedFields) {
		writeJSONError(w, http.StatusBadRequest, "renewal identity is derived from the selected agent token")
		return
	}
	if !messageRequestSecondsWithinConversionBounds(in.LeaseSeconds, maxMessageRequestLeaseSeconds) {
		writeJSONError(w, http.StatusBadRequest, "lease_seconds must be between 0 and 900")
		return
	}
	result, err := renew(r.Context(), p, requestID, in)
	if writeMessageRequestError(w, err, "could not renew message request claim") {
		return
	}
	writeMessageRequestJSON(w, http.StatusOK, map[string]any{"claim": result})
}

func handleMessageRequestRelease(
	w http.ResponseWriter,
	r *http.Request,
	p DomainPrincipal,
	requestID string,
	release func(context.Context, DomainPrincipal, string, ReleaseMessageRequestRequest) (MessageRequestClaim, error),
) {
	if release == nil {
		writeJSONError(w, http.StatusNotFound, "message request action not found")
		return
	}
	var in ReleaseMessageRequestRequest
	if decodeLimitedJSON(w, r, &in, messageRequestSmallBody) != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if hasMessageRequestDerivedFields(in.MessageRequestDerivedFields) {
		writeJSONError(w, http.StatusBadRequest, "release identity is derived from the selected agent token")
		return
	}
	result, err := release(r.Context(), p, requestID, in)
	if writeMessageRequestError(w, err, "could not release message request claim") {
		return
	}
	writeMessageRequestJSON(w, http.StatusOK, map[string]any{"claim": result})
}

func handleMessageRequestComplete(
	w http.ResponseWriter,
	r *http.Request,
	p DomainPrincipal,
	requestID string,
	complete func(context.Context, DomainPrincipal, string, CompleteMessageRequestRequest) (CompleteMessageRequestResult, error),
) {
	if complete == nil {
		writeJSONError(w, http.StatusNotFound, "message request action not found")
		return
	}
	var in CompleteMessageRequestRequest
	if decodeLimitedJSON(w, r, &in, messageRequestLargeBody) != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if hasMessageRequestDerivedFields(in.MessageRequestDerivedFields) || len(in.Kind) != 0 {
		writeJSONError(w, http.StatusBadRequest, "completion routing, kind, and identity are derived from the request and agent token")
		return
	}
	in.IdempotencyKey = strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	result, err := complete(r.Context(), p, requestID, in)
	if writeMessageRequestError(w, err, "could not complete message request claim") {
		return
	}
	result.Message = redactMessageProcessingFence(result.Message)
	writeMessageRequestJSON(w, http.StatusCreated, map[string]any{
		"request": result.Request, "claim": result.Claim, "message": result.Message,
	})
}

func hasMessageRequestDerivedFields(in MessageRequestDerivedFields) bool {
	return len(in.To) != 0 || len(in.From) != 0 || len(in.Sender) != 0 ||
		len(in.Actor) != 0 || len(in.Account) != 0 || len(in.AccountID) != 0 ||
		len(in.Realm) != 0 || len(in.RealmID) != 0 || len(in.Coordinator) != 0 ||
		len(in.CoordinatorID) != 0 || len(in.OpeningMessageID) != 0 ||
		len(in.ThreadID) != 0 || len(in.ReplyToMessageID) != 0 || len(in.CausalDepth) != 0
}

// messageRequestSecondsWithinConversionBounds validates the raw JSON integer
// before cmd/witself-server converts it to time.Duration. Without this fence,
// a sufficiently large integer can wrap during multiplication by time.Second
// and become an otherwise-valid store duration.
func messageRequestSecondsWithinConversionBounds(seconds, upperBound int) bool {
	return seconds >= 0 && seconds <= upperBound
}

func redactMessageRequestDetail(detail MessageRequestDetail) MessageRequestDetail {
	detail.OpeningMessage = redactMessageProcessingFence(detail.OpeningMessage)
	for i := range detail.Offers {
		detail.Offers[i].Message = redactMessageProcessingFence(detail.Offers[i].Message)
	}
	if detail.Candidates == nil {
		detail.Candidates = []MessageRequestCandidate{}
	}
	if detail.Offers == nil {
		detail.Offers = []MessageRequestOffer{}
	}
	if detail.Selections == nil {
		detail.Selections = []MessageRequestSelection{}
	}
	if detail.Claims == nil {
		detail.Claims = []MessageRequestClaim{}
	}
	return detail
}

func writeMessageRequestJSON(w http.ResponseWriter, status int, object map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(status)
	object["schema_version"] = "witself.v0"
	_ = json.NewEncoder(w).Encode(object)
}

func writeMessageRequestError(w http.ResponseWriter, err error, internalMessage string) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, ErrBadInput):
		writeJSONError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrForbidden):
		writeJSONError(w, http.StatusNotFound, "message request not found")
	case errors.Is(err, ErrPlanLimit):
		writeJSONError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, ErrBusy):
		writeJSONError(w, http.StatusConflict, "message request is already claimed")
	case errors.Is(err, ErrConflict), errors.Is(err, ErrIdempotencyConflict):
		writeJSONError(w, http.StatusConflict, "message request state or idempotency key conflicts")
	default:
		writeJSONError(w, http.StatusInternalServerError, internalMessage)
	}
	return true
}

// messageRequestsNoStoreMux also covers method-mismatch and not-found
// responses emitted by ServeMux before a method-specific handler runs.
func messageRequestsNoStoreMux(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/message-requests" || strings.HasPrefix(r.URL.Path, "/v1/message-requests/") {
			w.Header().Set("Cache-Control", "private, no-store")
		}
		next.ServeHTTP(w, r)
	})
}
