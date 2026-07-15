package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultMessageListenWaitSeconds     = 20
	maxMessageListenWaitSeconds         = 20
	messageListenPollInterval           = time.Second
	maxConcurrentMessageListens         = 128
	maxConcurrentMessageListensPerAgent = 2
	maxMessageProcessingLeaseSeconds    = 15 * 60
)

// messageListenLimiter bounds the number of long-lived handlers and database
// polling loops owned by one server process. The durable mailbox remains the
// source of truth, so a refused caller can retry without losing delivery.
type messageListenLimiter struct {
	mu      sync.Mutex
	active  int
	byAgent map[string]int
}

func (l *messageListenLimiter) tryAcquire(p DomainPrincipal) bool {
	key := p.AccountID + "\x00" + p.RealmID + "\x00" + p.ID
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.active >= maxConcurrentMessageListens || l.byAgent[key] >= maxConcurrentMessageListensPerAgent {
		return false
	}
	l.active++
	l.byAgent[key]++
	return true
}

func (l *messageListenLimiter) release(p DomainPrincipal) {
	key := p.AccountID + "\x00" + p.RealmID + "\x00" + p.ID
	l.mu.Lock()
	defer l.mu.Unlock()
	l.active--
	if l.byAgent[key] <= 1 {
		delete(l.byAgent, key)
		return
	}
	l.byAgent[key]--
}

func sendMessageHandler(auth PrincipalAuthFunc, send func(context.Context, DomainPrincipal, SendMessageRequest) (Message, error)) http.HandlerFunc {
	return messageNoStore(requireDomainPrincipal(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if p.Kind != PrincipalKindAgent {
			writeJSONError(w, http.StatusForbidden, "only an agent token may send a message")
			return
		}
		var req SendMessageRequest
		if err := decodeLimitedJSON(w, r, &req, 96*1024); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		if len(req.From) != 0 || len(req.Sender) != 0 || len(req.Actor) != 0 ||
			len(req.Account) != 0 || len(req.AccountID) != 0 || len(req.Realm) != 0 ||
			len(req.RealmID) != 0 || len(req.CausalDepth) != 0 {
			writeJSONError(w, http.StatusBadRequest, "sender, account, and realm are derived from the agent token")
			return
		}
		if len(req.To.Realm) != 0 || len(req.To.RealmID) != 0 || len(req.To.AccountID) != 0 {
			writeJSONError(w, http.StatusBadRequest, "this release supports same-realm recipients only")
			return
		}
		if req.DryRun {
			writeJSONError(w, http.StatusBadRequest, "message dry-run is not implemented")
			return
		}
		req.IdempotencyKey = strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		msg, err := send(r.Context(), p, req)
		switch {
		case errors.Is(err, ErrBadInput):
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		case errors.Is(err, ErrNotFound):
			writeJSONError(w, http.StatusNotFound, "message recipient not found in this realm")
			return
		case errors.Is(err, ErrConflict):
			writeJSONError(w, http.StatusConflict, "idempotency key was reused for a different message")
			return
		case errors.Is(err, ErrBusy):
			writeJSONError(w, http.StatusConflict, "message is claimed for processing")
			return
		case errors.Is(err, ErrForbidden):
			writeJSONError(w, http.StatusForbidden, "message access forbidden")
			return
		case err != nil:
			writeJSONError(w, http.StatusInternalServerError, "could not send message")
			return
		}
		msg = redactMessageProcessingFence(msg)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": "witself.v0", "message": msg,
		})
	}))
}

func listMessagesHandler(auth PrincipalAuthFunc, list func(context.Context, DomainPrincipal, MessageListOptions) (MessagePage, error)) http.HandlerFunc {
	return messageNoStore(requireDomainPrincipal(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if p.Kind != PrincipalKindAgent {
			writeJSONError(w, http.StatusForbidden, "only an agent token may list messages")
			return
		}
		q := r.URL.Query()
		opts := MessageListOptions{
			Direction: q.Get("direction"), From: q.Get("from"),
			ThreadID: q.Get("thread_id"), Kind: q.Get("kind"), Cursor: q.Get("cursor"),
		}
		if raw := q.Get("unread"); raw != "" {
			value, err := strconv.ParseBool(raw)
			if err != nil {
				writeJSONError(w, http.StatusBadRequest, "unread must be true or false")
				return
			}
			opts.Unread = value
		}
		if raw := q.Get("limit"); raw != "" {
			value, err := strconv.Atoi(raw)
			if err != nil {
				writeJSONError(w, http.StatusBadRequest, "limit must be an integer")
				return
			}
			opts.Limit = value
		}
		page, err := list(r.Context(), p, opts)
		switch {
		case errors.Is(err, ErrBadInput):
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		case errors.Is(err, ErrForbidden):
			writeJSONError(w, http.StatusForbidden, "message access forbidden")
			return
		case err != nil:
			writeJSONError(w, http.StatusInternalServerError, "could not list messages")
			return
		}
		if page.Messages == nil {
			page.Messages = []Message{}
		}
		for i := range page.Messages {
			page.Messages[i].Body = ""
			page.Messages[i].Payload = nil
			page.Messages[i] = redactMessageProcessingFence(page.Messages[i])
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": "witself.v0", "messages": page.Messages,
			"next_cursor": page.NextCursor,
		})
	}))
}

func messageListenHandler(auth PrincipalAuthFunc, list func(context.Context, DomainPrincipal, MessageListOptions) (MessagePage, error)) http.HandlerFunc {
	limiter := &messageListenLimiter{byAgent: make(map[string]int)}
	return messageNoStore(requireDomainPrincipal(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if p.Kind != PrincipalKindAgent {
			writeJSONError(w, http.StatusForbidden, "only an agent token may listen for messages")
			return
		}
		var req MessageListenRequest
		if err := decodeLimitedJSON(w, r, &req, 16*1024); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		waitSeconds := defaultMessageListenWaitSeconds
		if req.WaitSeconds != nil {
			waitSeconds = *req.WaitSeconds
		}
		if waitSeconds < 0 || waitSeconds > maxMessageListenWaitSeconds {
			writeJSONError(w, http.StatusBadRequest, "wait_seconds must be between 0 and 20")
			return
		}
		if !limiter.tryAcquire(p) {
			w.Header().Set("Retry-After", "1")
			writeJSONError(w, http.StatusTooManyRequests, "too many concurrent message listens")
			return
		}
		defer limiter.release(p)
		opts := MessageListOptions{
			Direction: "inbox", Unacked: true, OldestFirst: true,
			From: req.FromAgent, ThreadID: req.ThreadID, Kind: req.Kind,
			Limit: req.Limit,
		}

		deadline := time.NewTimer(time.Duration(waitSeconds) * time.Second)
		defer deadline.Stop()
		poll := time.NewTicker(messageListenPollInterval)
		defer poll.Stop()
		for {
			page, err := list(r.Context(), p, opts)
			switch {
			case errors.Is(err, ErrBadInput):
				writeJSONError(w, http.StatusBadRequest, err.Error())
				return
			case errors.Is(err, ErrForbidden):
				writeJSONError(w, http.StatusForbidden, "message access forbidden")
				return
			case err != nil:
				writeJSONError(w, http.StatusInternalServerError, "could not listen for messages")
				return
			}
			for i := range page.Messages {
				page.Messages[i].Body = ""
				page.Messages[i].Payload = nil
				page.Messages[i] = redactMessageProcessingFence(page.Messages[i])
			}
			if len(page.Messages) != 0 {
				writeMessageListenResult(w, MessageListenResult{
					Messages: page.Messages,
				})
				return
			}
			if waitSeconds == 0 {
				writeMessageListenResult(w, MessageListenResult{Messages: []Message{}, TimedOut: true})
				return
			}
			select {
			case <-r.Context().Done():
				return
			case <-deadline.C:
				writeMessageListenResult(w, MessageListenResult{Messages: []Message{}, TimedOut: true})
				return
			case <-poll.C:
			}
		}
	}))
}

func writeMessageListenResult(w http.ResponseWriter, result MessageListenResult) {
	if result.Messages == nil {
		result.Messages = []Message{}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"schema_version": "witself.v0",
		"messages":       result.Messages,
		"timed_out":      result.TimedOut,
	})
}

func messageActionHandler(
	auth PrincipalAuthFunc,
	read func(context.Context, DomainPrincipal, string) (Message, error),
	ack func(context.Context, DomainPrincipal, string) (Message, error),
	reply func(context.Context, DomainPrincipal, string, ReplyMessageRequest) (Message, error),
	claim func(context.Context, DomainPrincipal, string, ClaimMessageRequest) (MessageProcessing, error),
	renew func(context.Context, DomainPrincipal, string, RenewMessageClaimRequest) (MessageProcessing, error),
	release func(context.Context, DomainPrincipal, string, MessageClaimRequest) (MessageProcessing, error),
	complete func(context.Context, DomainPrincipal, string, CompleteMessageRequest) (CompleteMessageResult, error),
) http.HandlerFunc {
	return messageNoStore(requireDomainPrincipal(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if p.Kind != PrincipalKindAgent {
			writeJSONError(w, http.StatusForbidden, "only an agent token may act on a message")
			return
		}
		action := r.PathValue("action")
		messageID, operation, ok := strings.Cut(action, ":")
		if !ok || messageID == "" ||
			(operation != "read" && operation != "ack" && operation != "reply" &&
				operation != "claim" && operation != "renew" && operation != "release" &&
				operation != "complete") {
			writeJSONError(w, http.StatusNotFound, "message action not found")
			return
		}
		if operation == "claim" || operation == "renew" || operation == "release" || operation == "complete" {
			handleMessageProcessingAction(w, r, p, messageID, operation, claim, renew, release, complete)
			return
		}
		var msg Message
		var err error
		switch operation {
		case "read":
			if read == nil {
				writeJSONError(w, http.StatusNotFound, "message action not found")
				return
			}
			msg, err = read(r.Context(), p, messageID)
		case "ack":
			if ack == nil {
				writeJSONError(w, http.StatusNotFound, "message action not found")
				return
			}
			msg, err = ack(r.Context(), p, messageID)
		case "reply":
			if reply == nil {
				writeJSONError(w, http.StatusNotFound, "message action not found")
				return
			}
			var req ReplyMessageRequest
			if decodeErr := decodeLimitedJSON(w, r, &req, 96*1024); decodeErr != nil {
				writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
				return
			}
			if len(req.To) != 0 || len(req.ThreadID) != 0 || len(req.ReplyToMessageID) != 0 ||
				len(req.From) != 0 || len(req.Sender) != 0 || len(req.Actor) != 0 ||
				len(req.Account) != 0 || len(req.AccountID) != 0 ||
				len(req.Realm) != 0 || len(req.RealmID) != 0 || len(req.CausalDepth) != 0 {
				writeJSONError(w, http.StatusBadRequest, "reply routing and identity are derived from the parent and agent token")
				return
			}
			req.IdempotencyKey = strings.TrimSpace(r.Header.Get("Idempotency-Key"))
			msg, err = reply(r.Context(), p, messageID, req)
		}
		switch {
		case errors.Is(err, ErrBadInput):
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		case errors.Is(err, ErrNotFound):
			writeJSONError(w, http.StatusNotFound, "message not found")
			return
		case errors.Is(err, ErrConflict):
			writeJSONError(w, http.StatusConflict, "idempotency key was reused for a different message")
			return
		case errors.Is(err, ErrBusy):
			writeJSONError(w, http.StatusConflict, "message is claimed for processing")
			return
		case errors.Is(err, ErrForbidden):
			writeJSONError(w, http.StatusForbidden, "message access forbidden")
			return
		case err != nil:
			writeJSONError(w, http.StatusInternalServerError, "could not update message")
			return
		}
		if operation == "ack" {
			msg.Body = ""
			msg.Payload = nil
		}
		msg = redactMessageProcessingFence(msg)
		w.Header().Set("Content-Type", "application/json")
		if operation == "reply" {
			w.WriteHeader(http.StatusCreated)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": "witself.v0", "message": msg,
		})
	}))
}

func handleMessageProcessingAction(
	w http.ResponseWriter,
	r *http.Request,
	p DomainPrincipal,
	messageID string,
	operation string,
	claim func(context.Context, DomainPrincipal, string, ClaimMessageRequest) (MessageProcessing, error),
	renew func(context.Context, DomainPrincipal, string, RenewMessageClaimRequest) (MessageProcessing, error),
	release func(context.Context, DomainPrincipal, string, MessageClaimRequest) (MessageProcessing, error),
	complete func(context.Context, DomainPrincipal, string, CompleteMessageRequest) (CompleteMessageResult, error),
) {
	var processing MessageProcessing
	var completeResult CompleteMessageResult
	var err error
	switch operation {
	case "claim":
		if claim == nil {
			writeJSONError(w, http.StatusNotFound, "message action not found")
			return
		}
		var req ClaimMessageRequest
		if decodeErr := decodeLimitedJSON(w, r, &req, 16*1024); decodeErr != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		if !messageProcessingSecondsWithinConversionBounds(req.LeaseSeconds) {
			writeJSONError(w, http.StatusBadRequest, "lease_seconds must be between 0 and 900")
			return
		}
		req.IdempotencyKey = strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		processing, err = claim(r.Context(), p, messageID, req)
	case "renew":
		if renew == nil {
			writeJSONError(w, http.StatusNotFound, "message action not found")
			return
		}
		var req RenewMessageClaimRequest
		if decodeErr := decodeLimitedJSON(w, r, &req, 16*1024); decodeErr != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		if !messageProcessingSecondsWithinConversionBounds(req.LeaseSeconds) {
			writeJSONError(w, http.StatusBadRequest, "lease_seconds must be between 0 and 900")
			return
		}
		processing, err = renew(r.Context(), p, messageID, req)
	case "release":
		if release == nil {
			writeJSONError(w, http.StatusNotFound, "message action not found")
			return
		}
		var req MessageClaimRequest
		if decodeErr := decodeLimitedJSON(w, r, &req, 16*1024); decodeErr != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		processing, err = release(r.Context(), p, messageID, req)
	case "complete":
		if complete == nil {
			writeJSONError(w, http.StatusNotFound, "message action not found")
			return
		}
		var req CompleteMessageRequest
		if decodeErr := decodeLimitedJSON(w, r, &req, 96*1024); decodeErr != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		if len(req.To) != 0 || len(req.ThreadID) != 0 || len(req.ReplyToMessageID) != 0 ||
			len(req.From) != 0 || len(req.Sender) != 0 || len(req.Actor) != 0 ||
			len(req.Account) != 0 || len(req.AccountID) != 0 ||
			len(req.Realm) != 0 || len(req.RealmID) != 0 || len(req.CausalDepth) != 0 {
			writeJSONError(w, http.StatusBadRequest, "completion routing and identity are derived from the claimed message and agent token")
			return
		}
		req.IdempotencyKey = strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		completeResult, err = complete(r.Context(), p, messageID, req)
	}
	if writeMessageProcessingError(w, err) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, no-store")
	if operation == "complete" {
		completeResult.Message = redactMessageProcessingFence(completeResult.Message)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": "witself.v0",
			"processing":     completeResult.Processing,
			"message":        completeResult.Message,
		})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"schema_version": "witself.v0",
		"processing":     processing,
	})
}

// messageProcessingSecondsWithinConversionBounds validates the raw JSON
// integer before cmd/witself-server multiplies it by time.Second. Extremely
// large positive or negative integers can otherwise wrap into a valid lease.
func messageProcessingSecondsWithinConversionBounds(seconds int) bool {
	return seconds >= 0 && seconds <= maxMessageProcessingLeaseSeconds
}

func writeMessageProcessingError(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, ErrBadInput):
		writeJSONError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrForbidden):
		writeJSONError(w, http.StatusNotFound, "message not found")
	case errors.Is(err, ErrBusy):
		writeJSONError(w, http.StatusConflict, "message is already claimed for processing")
	case errors.Is(err, ErrConflict), errors.Is(err, ErrIdempotencyConflict):
		writeJSONError(w, http.StatusConflict, "message processing claim is stale or conflicts")
	default:
		writeJSONError(w, http.StatusInternalServerError, "could not update message processing")
	}
	return true
}

func messageNoStore(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "private, no-store")
		next(w, r)
	}
}

// messagingNoStoreMux also covers method-mismatch and not-found responses
// produced by ServeMux before a method-specific messaging handler runs.
func messagingNoStoreMux(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if path == "/v1/messages" || path == "/v1/messages:listen" ||
			strings.HasPrefix(path, "/v1/messages/") {
			w.Header().Set("Cache-Control", "private, no-store")
		}
		next.ServeHTTP(w, r)
	})
}

func redactMessageProcessingFence(msg Message) Message {
	msg.Processing.ClaimID = ""
	msg.Processing.LeaseExpiresAt = nil
	return msg
}
