package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
)

func sendMessageHandler(auth PrincipalAuthFunc, send func(context.Context, DomainPrincipal, SendMessageRequest) (Message, error)) http.HandlerFunc {
	return requireDomainPrincipal(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if p.Kind != PrincipalKindAgent {
			writeJSONError(w, http.StatusForbidden, "only an agent token may send a message")
			return
		}
		var req SendMessageRequest
		if err := decodeLimitedJSON(w, r, &req, 96*1024); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		if len(req.From) != 0 || len(req.Sender) != 0 || len(req.Actor) != 0 || len(req.Realm) != 0 || len(req.RealmID) != 0 {
			writeJSONError(w, http.StatusBadRequest, "sender and realm are derived from the agent token")
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
		case errors.Is(err, ErrForbidden):
			writeJSONError(w, http.StatusForbidden, "message access forbidden")
			return
		case err != nil:
			writeJSONError(w, http.StatusInternalServerError, "could not send message")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": "witself.v0", "message": msg,
		})
	})
}

func listMessagesHandler(auth PrincipalAuthFunc, list func(context.Context, DomainPrincipal, MessageListOptions) (MessagePage, error)) http.HandlerFunc {
	return requireDomainPrincipal(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
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
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": "witself.v0", "messages": page.Messages,
			"next_cursor": page.NextCursor,
		})
	})
}

func messageActionHandler(
	auth PrincipalAuthFunc,
	read func(context.Context, DomainPrincipal, string) (Message, error),
	ack func(context.Context, DomainPrincipal, string) (Message, error),
) http.HandlerFunc {
	return requireDomainPrincipal(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if p.Kind != PrincipalKindAgent {
			writeJSONError(w, http.StatusForbidden, "only an agent token may read or acknowledge a message")
			return
		}
		action := r.PathValue("action")
		messageID, operation, ok := strings.Cut(action, ":")
		if !ok || messageID == "" || (operation != "read" && operation != "ack") {
			writeJSONError(w, http.StatusNotFound, "message action not found")
			return
		}
		var msg Message
		var err error
		if operation == "read" {
			msg, err = read(r.Context(), p, messageID)
		} else {
			msg, err = ack(r.Context(), p, messageID)
		}
		switch {
		case errors.Is(err, ErrBadInput):
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		case errors.Is(err, ErrNotFound):
			writeJSONError(w, http.StatusNotFound, "message not found")
			return
		case errors.Is(err, ErrForbidden):
			writeJSONError(w, http.StatusForbidden, "message access forbidden")
			return
		case err != nil:
			writeJSONError(w, http.StatusInternalServerError, "could not update message")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": "witself.v0", "message": msg,
		})
	})
}
