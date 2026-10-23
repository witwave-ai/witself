package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	// TranscriptRedactedBody replaces the content of an owner-redacted entry.
	TranscriptRedactedBody = "entry content redacted at the account owner's request"
	// TranscriptRedactionReasonOwnerRequest is the server-owned redaction reason.
	TranscriptRedactionReasonOwnerRequest = "owner_request"

	transcriptRedactionPayloadKey = "witself_redaction"
	transcriptRedactionSchema     = "witself.transcript-redaction.v1"
)

var transcriptRedactionReceiptPattern = regexp.MustCompile(`^trd_[0-9a-f]{32}$`)

// Keep this vocabulary aligned with setEventContent, cursorNativeTranscriptMessages,
// finalizePendingWithin, finalizePendingDSH and dshRecoveredTurnSteps in capture.go.
var transcriptCaptureKinds = map[string]struct{}{
	"message.user":              {},
	"message.assistant":         {},
	"agent.thought":             {},
	"tool.call":                 {},
	"tool.result":               {},
	"tool.error":                {},
	"session.started":           {},
	"session.ended":             {},
	"turn.completed":            {},
	"turn.failed":               {},
	"subagent.started":          {},
	"subagent.stopped":          {},
	"compaction.started":        {},
	"compaction.completed":      {},
	"permission.requested":      {},
	"permission.review.started": {},
	"permission.denied":         {},
	"runtime.notification":      {},
	"runtime.event":             {},
}

// TranscriptEntryRedaction is a read-only view of an entry's redaction marker.
type TranscriptEntryRedaction struct {
	ReceiptID  string    `json:"receipt_id"`
	RedactedAt time.Time `json:"redacted_at"`
	ReasonCode string    `json:"reason_code"`
}

// transcriptEntryRedactionProbe can also be populated by SQL projections without
// selecting an entry's original body, whole payload or artifacts into Go.
type transcriptEntryRedactionProbe struct {
	BodyMatches    bool
	ArtifactsEmpty bool
	PayloadKeys    []string
	KindType       string
	KindText       string
	Marker         json.RawMessage
}

// TranscriptEntryRedactionOf recognizes only the complete, content-free marker
// shape. An incomplete or altered marker is ordinary transcript content.
func TranscriptEntryRedactionOf(entry TranscriptEntry) (TranscriptEntryRedaction, bool) {
	if entry.Body != TranscriptRedactedBody {
		return TranscriptEntryRedaction{}, false
	}
	probe := transcriptEntryRedactionProbe{BodyMatches: true}
	var artifacts []json.RawMessage
	probe.ArtifactsEmpty = json.Unmarshal(entry.Artifacts, &artifacts) == nil && artifacts != nil && len(artifacts) == 0
	var payload map[string]json.RawMessage
	if json.Unmarshal(entry.Payload, &payload) != nil || payload == nil {
		return TranscriptEntryRedaction{}, false
	}
	for key := range payload {
		probe.PayloadKeys = append(probe.PayloadKeys, key)
	}
	slices.Sort(probe.PayloadKeys)
	if kind, exists := payload["kind"]; exists {
		kind = bytes.TrimSpace(kind)
		switch kind[0] {
		case '"':
			probe.KindType = "string"
			if json.Unmarshal(kind, &probe.KindText) != nil {
				return TranscriptEntryRedaction{}, false
			}
			if len(probe.KindText) > 64 {
				probe.KindText = probe.KindText[:64]
			}
		case '{':
			probe.KindType = "object"
		case '[':
			probe.KindType = "array"
		case 'n':
			probe.KindType = "null"
		case 't', 'f':
			probe.KindType = "boolean"
		default:
			probe.KindType = "number"
		}
	}
	probe.Marker = payload[transcriptRedactionPayloadKey]
	return transcriptEntryRedactionFromProbe(probe)
}

func transcriptEntryRedactionFromProbe(probe transcriptEntryRedactionProbe) (TranscriptEntryRedaction, bool) {
	if !probe.BodyMatches || !probe.ArtifactsEmpty {
		return TranscriptEntryRedaction{}, false
	}
	switch {
	case slices.Equal(probe.PayloadKeys, []string{transcriptRedactionPayloadKey}):
	case slices.Equal(probe.PayloadKeys, []string{"kind", transcriptRedactionPayloadKey}):
		if probe.KindType != "string" || transcriptCaptureKind(probe.KindText) == "" {
			return TranscriptEntryRedaction{}, false
		}
	default:
		return TranscriptEntryRedaction{}, false
	}
	var marker map[string]json.RawMessage
	if json.Unmarshal(probe.Marker, &marker) != nil || len(marker) != 5 {
		return TranscriptEntryRedaction{}, false
	}
	values := make(map[string]string, 5)
	for _, key := range []string{"schema", "receipt_id", "redacted_at", "reason_code", "actor_kind"} {
		raw := bytes.TrimSpace(marker[key])
		if len(raw) == 0 || raw[0] != '"' {
			return TranscriptEntryRedaction{}, false
		}
		var value string
		if json.Unmarshal(raw, &value) != nil {
			return TranscriptEntryRedaction{}, false
		}
		values[key] = value
	}
	if values["schema"] != transcriptRedactionSchema ||
		!transcriptRedactionReceiptPattern.MatchString(values["receipt_id"]) ||
		values["reason_code"] != TranscriptRedactionReasonOwnerRequest ||
		values["actor_kind"] != "operator" ||
		!strings.HasSuffix(values["redacted_at"], "Z") {
		return TranscriptEntryRedaction{}, false
	}
	redactedAt, err := time.Parse(time.RFC3339Nano, values["redacted_at"])
	if err != nil || redactedAt.UTC().Format(time.RFC3339Nano) != values["redacted_at"] {
		return TranscriptEntryRedaction{}, false
	}
	return TranscriptEntryRedaction{
		ReceiptID: values["receipt_id"], RedactedAt: redactedAt,
		ReasonCode: values["reason_code"],
	}, true
}

func transcriptCaptureKind(raw string) string {
	if _, ok := transcriptCaptureKinds[raw]; ok {
		return raw
	}
	return ""
}

func transcriptRedactionPayload(kind, receiptID string, redactedAt time.Time) (json.RawMessage, error) {
	if !transcriptRedactionReceiptPattern.MatchString(receiptID) {
		return nil, fmt.Errorf("%w: malformed transcript redaction receipt id", ErrTranscriptInputInvalid)
	}
	return json.Marshal(struct {
		Kind   string            `json:"kind,omitempty"`
		Marker map[string]string `json:"witself_redaction"`
	}{
		Kind: transcriptCaptureKind(kind),
		Marker: map[string]string{
			"schema":      transcriptRedactionSchema,
			"receipt_id":  receiptID,
			"redacted_at": redactedAt.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano),
			"reason_code": TranscriptRedactionReasonOwnerRequest,
			"actor_kind":  "operator",
		},
	})
}
