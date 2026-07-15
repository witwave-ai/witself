package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const maxAgentActivityRequestBytes = 4 * 1024

// AgentActivityRequest is a privacy-safe client hook observation. EventID and
// EventOccurredAt are projection guards only: the server never exposes them as
// peer metadata, and LastActivityAt is always stamped by the store's clock.
type AgentActivityRequest struct {
	Runtime         string    `json:"runtime"`
	LocationID      string    `json:"location_id"`
	Location        string    `json:"location"`
	Event           string    `json:"event"`
	EventID         string    `json:"event_id"`
	EventOccurredAt time.Time `json:"event_occurred_at"`
}

// AgentActivity is the public projection returned after an activity touch.
// It intentionally makes no availability claim.
type AgentActivity struct {
	LastActivityAt time.Time `json:"last_activity_at"`
	LastRuntime    string    `json:"last_runtime"`
	LastLocation   string    `json:"last_location"`
	LastEvent      string    `json:"last_event"`
}

func touchAgentActivityHandler(
	auth PrincipalAuthFunc,
	touch func(context.Context, DomainPrincipal, AgentActivityRequest) (AgentActivity, error),
) http.HandlerFunc {
	return requireDomainPrincipal(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		if p.Kind != PrincipalKindAgent {
			writeJSONError(w, http.StatusForbidden, "only an agent token may record activity")
			return
		}
		if r.URL.RawQuery != "" {
			writeJSONError(w, http.StatusBadRequest, "agent activity does not accept query parameters")
			return
		}
		var in AgentActivityRequest
		if err := decodeStrictAgentActivityJSON(w, r, &in); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid agent activity body")
			return
		}
		normalizeAgentActivityRequest(&in)
		if err := validateAgentActivityRequest(in); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		activity, err := touch(r.Context(), p, in)
		switch {
		case errors.Is(err, ErrBadInput):
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		case errors.Is(err, ErrForbidden):
			writeJSONError(w, http.StatusForbidden, "agent activity access forbidden")
			return
		case errors.Is(err, ErrNotFound):
			writeJSONError(w, http.StatusNotFound, "agent not found")
			return
		case err != nil:
			writeJSONError(w, http.StatusInternalServerError, "could not record agent activity")
			return
		}
		w.Header().Set("Cache-Control", "private, no-store")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": "witself.v0",
			"activity":       activity,
		})
	})
}

func validateAgentActivityRequest(in AgentActivityRequest) error {
	if !validAgentActivityLabel(in.Runtime, 128, false) {
		return errors.New("runtime is invalid")
	}
	if !validAgentActivityLabel(in.Event, 128, false) {
		return errors.New("event is invalid")
	}
	if !validAgentActivityLabel(in.LocationID, 128, false) {
		return errors.New("location_id is invalid")
	}
	if !validAgentActivityLabel(in.Location, 256, true) {
		return errors.New("location is invalid")
	}
	if !validAgentActivityLabel(in.EventID, 128, false) {
		return errors.New("event_id is invalid")
	}
	if in.EventOccurredAt.IsZero() {
		return errors.New("event_occurred_at is required")
	}
	return nil
}

func normalizeAgentActivityRequest(in *AgentActivityRequest) {
	in.Runtime = strings.TrimSpace(in.Runtime)
	in.LocationID = strings.TrimSpace(in.LocationID)
	in.Location = strings.TrimSpace(in.Location)
	in.Event = strings.TrimSpace(in.Event)
	in.EventID = strings.TrimSpace(in.EventID)
	in.EventOccurredAt = in.EventOccurredAt.UTC()
}

func validAgentActivityLabel(value string, maxBytes int, optional bool) bool {
	if value == "" {
		return optional
	}
	if len(value) > maxBytes || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func decodeStrictAgentActivityJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxAgentActivityRequestBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return errors.New("request contains trailing JSON")
	} else if !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}
