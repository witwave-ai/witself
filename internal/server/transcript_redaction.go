package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ErrAccountMoveInProgress is a conflict whose public reason is safe to expose.
var ErrAccountMoveInProgress = fmt.Errorf("%w: account move in progress", ErrConflict)

// RedactTranscriptEntriesRequest selects a preview or a guarded owner redaction.
type RedactTranscriptEntriesRequest struct {
	TranscriptID                 string
	FromSequence                 int64
	UntilSequence                int64
	ExpectedRedactionSetRevision string
	IdempotencyKey               string
	Apply                        bool
}

// TranscriptRedactionEntry describes an entry without exposing its content.
type TranscriptRedactionEntry struct {
	EntryID   string    `json:"entry_id"`
	Sequence  int64     `json:"sequence"`
	Role      string    `json:"role"`
	Kind      string    `json:"kind,omitempty"`
	Redacted  bool      `json:"redacted"`
	CreatedAt time.Time `json:"created_at"`
}

// TranscriptRedactionReceipt reports value-free redaction impact and outcome.
type TranscriptRedactionReceipt struct {
	TranscriptID              string                     `json:"transcript_id"`
	FromSequence              int64                      `json:"from_sequence"`
	UntilSequence             int64                      `json:"until_sequence"`
	RedactionSetRevision      string                     `json:"redaction_set_revision"`
	EntryCount                int64                      `json:"entry_count"`
	RedactableCount           int64                      `json:"redactable_count"`
	AlreadyRedactedCount      int64                      `json:"already_redacted_count"`
	PartialCaptureEvent       bool                       `json:"partial_capture_event"`
	MemoryEvidenceCount       int64                      `json:"memory_evidence_count"`
	EvidenceMemoryIDs         []string                   `json:"evidence_memory_ids"`
	CurationRunsToCancel      int64                      `json:"curation_runs_to_cancel"`
	CancelledCurationRunCount int64                      `json:"cancelled_curation_run_count"`
	Entries                   []TranscriptRedactionEntry `json:"entries"`
	ReceiptID                 string                     `json:"receipt_id,omitempty"`
	RedactedCount             int64                      `json:"redacted_count"`
	RedactedAt                *time.Time                 `json:"redacted_at,omitempty"`
	Applied                   bool                       `json:"applied"`
	Replayed                  bool                       `json:"replayed"`
}

func redactTranscriptEntriesHandler(auth PrincipalAuthFunc, redact func(context.Context, DomainPrincipal, RedactTranscriptEntriesRequest) (TranscriptRedactionReceipt, error)) http.HandlerFunc {
	return transcriptRedactionSafetyHandler(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		dryRun := false
		if _, supplied := r.URL.Query()["dry_run"]; supplied {
			var err error
			dryRun, err = strconv.ParseBool(r.URL.Query().Get("dry_run"))
			if err != nil {
				writeJSONError(w, http.StatusBadRequest, "dry_run must be true or false")
				return
			}
		}
		var body *struct {
			FromSequence                 int64   `json:"from_sequence"`
			UntilSequence                int64   `json:"until_sequence"`
			ExpectedRedactionSetRevision *string `json:"expected_redaction_set_revision"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&body); err != nil || body == nil {
			writeJSONError(w, http.StatusBadRequest, "invalid transcript redaction request")
			return
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			writeJSONError(w, http.StatusBadRequest, "invalid transcript redaction request")
			return
		}
		req := RedactTranscriptEntriesRequest{
			TranscriptID:   strings.TrimSpace(r.PathValue("transcript")),
			FromSequence:   body.FromSequence,
			UntilSequence:  body.UntilSequence,
			IdempotencyKey: strings.TrimSpace(r.Header.Get("Idempotency-Key")),
			Apply:          !dryRun,
		}
		if body.ExpectedRedactionSetRevision != nil {
			req.ExpectedRedactionSetRevision = strings.TrimSpace(*body.ExpectedRedactionSetRevision)
		}
		// Previews carry only the selector and cannot accept mutation guards.
		if !req.Apply && (body.ExpectedRedactionSetRevision != nil || len(r.Header.Values("Idempotency-Key")) != 0) {
			writeJSONError(w, http.StatusBadRequest, "redaction preview accepts no mutation guards")
			return
		}
		if req.Apply && (req.ExpectedRedactionSetRevision == "" || req.IdempotencyKey == "") {
			writeJSONError(w, http.StatusBadRequest, "expected_redaction_set_revision and Idempotency-Key are required")
			return
		}
		receipt, err := redact(r.Context(), p, req)
		if writeTranscriptRedactionError(w, err) {
			return
		}
		w.Header().Set("Cache-Control", "private, no-store")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "redaction": receipt})
	})
}

// transcriptRedactionSafetyHandler admits the owner's harm-reducing operation
// while suspended. The store checks the live operator's account-owner authority.
func transcriptRedactionSafetyHandler(auth PrincipalAuthFunc, next func(http.ResponseWriter, *http.Request, DomainPrincipal)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setAuthenticatedNoStoreDefault(w)
		token, ok := bearerToken(r)
		if !ok {
			writeJSONError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		p, ok, err := auth(r.Context(), token)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if !ok {
			writeJSONError(w, http.StatusUnauthorized, "invalid token")
			return
		}
		if !admitAccountRequest(w, r, p.AccountID) {
			return
		}
		if effectiveAccessProfile(p) != AccessProfileFull {
			writeJSONError(w, http.StatusForbidden, "credential profile is not authorized for this route")
			return
		}
		if p.AccountStatus != "active" && p.AccountStatus != "suspended" {
			writeJSONError(w, http.StatusForbidden,
				fmt.Sprintf("account is %s — this action requires an active or suspended account", p.AccountStatus))
			return
		}
		if p.Kind != PrincipalKindOperator {
			writeJSONError(w, http.StatusForbidden, "only the account owner's operator token may redact transcript entries")
			return
		}
		next(w, r, p)
	}
}

func writeTranscriptRedactionError(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, ErrBadInput):
		writeJSONError(w, http.StatusBadRequest, badInputMessage("invalid transcript redaction request", err))
	case errors.Is(err, ErrForbidden):
		writeJSONError(w, http.StatusForbidden, "transcript redaction forbidden")
	case errors.Is(err, ErrNotFound):
		writeJSONError(w, http.StatusNotFound, "transcript not found")
	case errors.Is(err, ErrAccountMoveInProgress):
		writeJSONError(w, http.StatusConflict, "account move in progress")
	case errors.Is(err, ErrConflict):
		writeJSONError(w, http.StatusConflict, "transcript entries changed since redaction preview")
	case errors.Is(err, ErrTranscriptEntriesRedacted):
		writeJSONError(w, http.StatusGone, "every entry in the range is already redacted")
	default:
		writeJSONError(w, http.StatusInternalServerError, "could not redact transcript entries")
	}
	return true
}
