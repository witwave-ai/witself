package memorycurator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"time"
	"unicode/utf8"

	"github.com/witwave-ai/witself/internal/client"
)

// PlannerEnvelopeSchemaV1 and MemoryPlanSchemaV1 identify the planner input and
// returned memory-plan contracts.
const (
	PlannerEnvelopeSchemaV1 = "witself.curator-planner.v1"
	MemoryPlanSchemaV1      = "witself.memory-plan.v1"
)

// DefaultMaxPlannerOutputBytes and DefaultMaxPlannerStderrBytes bound child
// process streams.
const (
	DefaultMaxPlannerOutputBytes = 32 << 20
	DefaultMaxPlannerStderrBytes = 64 << 10
)

// ErrInvalidPlannerOutput reports a malformed or policy-invalid plan draft.
var ErrInvalidPlannerOutput = errors.New("invalid curator planner output")

// Planner performs all semantic judgment. Implementations receive no endpoint,
// bearer token, token-file path, or mutation idempotency key.
type Planner interface {
	Plan(context.Context, PlannerEnvelope) (json.RawMessage, error)
}

// PlannerPolicy constrains the schema, operations, sensitivity, and action
// count accepted from a planner.
type PlannerPolicy struct {
	PlanSchema        string   `json:"plan_schema"`
	AllowedOperations []string `json:"allowed_operations"`
	IncludeSensitive  bool     `json:"include_sensitive"`
	MaximumActions    int      `json:"maximum_actions"`
}

// PlannerEnvelope contains only the exact materialized inputs frozen by the
// server plus value-free run coordinates and constraints.
type PlannerEnvelope struct {
	Schema             string                          `json:"schema"`
	RequestID          string                          `json:"request_id"`
	RequestGeneration  int64                           `json:"request_generation"`
	RunID              string                          `json:"run_id"`
	FencingGeneration  int64                           `json:"fencing_generation"`
	LeaseExpiresAt     *time.Time                      `json:"lease_expires_at,omitempty"`
	Policy             PlannerPolicy                   `json:"policy"`
	MaterializedInputs []client.MemoryCurationRunInput `json:"materialized_inputs"`
}

// validatePlanDraft checks the outer plan language before it reaches the API.
// The server remains authoritative for nested semantic, provenance, budget,
// and optimistic-version validation.
func validatePlanDraft(raw []byte) error {
	return validatePlanDraftForLimit(raw, 128)
}

func validatePlanDraftForLimit(raw []byte, maximumActions int) error {
	if len(raw) == 0 || len(raw) > DefaultMaxPlannerOutputBytes || !utf8.Valid(raw) {
		return fmt.Errorf("%w: plan must be nonempty valid UTF-8 JSON within %d bytes", ErrInvalidPlannerOutput, DefaultMaxPlannerOutputBytes)
	}
	if maximumActions < 0 || maximumActions > 128 {
		return fmt.Errorf("%w: invalid maximum action policy", ErrInvalidPlannerOutput)
	}
	if err := rejectDuplicateJSONNames(raw); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidPlannerOutput, err)
	}
	type actionEnvelope struct {
		Ordinal     int64           `json:"ordinal"`
		Operation   string          `json:"operation"`
		Create      json.RawMessage `json:"create,omitempty"`
		Replace     json.RawMessage `json:"replace,omitempty"`
		Supersede   json.RawMessage `json:"supersede,omitempty"`
		Relate      json.RawMessage `json:"relate,omitempty"`
		ProposeFact json.RawMessage `json:"propose_fact,omitempty"`
	}
	type draftEnvelope struct {
		Schema        string           `json:"schema"`
		DraftRevision int64            `json:"draft_revision"`
		Actions       []actionEnvelope `json:"actions"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	var draft draftEnvelope
	if err := decoder.Decode(&draft); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidPlannerOutput, err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidPlannerOutput, err)
	}
	if draft.Schema != MemoryPlanSchemaV1 || draft.DraftRevision < 1 || draft.Actions == nil || len(draft.Actions) > maximumActions {
		return fmt.Errorf("%w: expected schema %q, positive draft_revision, and 0-%d actions", ErrInvalidPlannerOutput, MemoryPlanSchemaV1, maximumActions)
	}
	for index, action := range draft.Actions {
		if action.Ordinal != int64(index+1) {
			return fmt.Errorf("%w: action ordinals must be contiguous from one", ErrInvalidPlannerOutput)
		}
		payloads := map[string]json.RawMessage{
			"create": action.Create, "replace": action.Replace,
			"supersede": action.Supersede, "relate": action.Relate,
			"propose_fact": action.ProposeFact,
		}
		payloadCount := 0
		for _, payload := range payloads {
			if len(payload) != 0 && string(payload) != "null" {
				payloadCount++
			}
		}
		if _, allowed := payloads[action.Operation]; !allowed || payloadCount != 1 || len(payloads[action.Operation]) == 0 || string(payloads[action.Operation]) == "null" {
			return fmt.Errorf("%w: action %d must contain exactly its supported operation payload", ErrInvalidPlannerOutput, index+1)
		}
	}
	return nil
}

// validateAcceptedPlanForLimit validates the normalized server-authored plan
// envelope returned by plan and plan.get. Nested semantic authorization stays
// server-owned, but the runner refuses malformed coordinates or tagged-union
// shapes before apply.
func validateAcceptedPlanForLimit(raw []byte, maximumActions int, planRevision int64) error {
	if len(raw) == 0 || len(raw) > DefaultMaxPlannerOutputBytes || !utf8.Valid(raw) {
		return fmt.Errorf("%w: accepted plan must be nonempty valid UTF-8 JSON within %d bytes", ErrProtocolResponse, DefaultMaxPlannerOutputBytes)
	}
	if maximumActions < 0 || maximumActions > 128 || planRevision < 1 {
		return fmt.Errorf("%w: invalid accepted-plan coordinates", ErrProtocolResponse)
	}
	if err := rejectDuplicateJSONNames(raw); err != nil {
		return fmt.Errorf("%w: accepted plan: %v", ErrProtocolResponse, err)
	}
	type actionEnvelope struct {
		Ordinal     int64           `json:"ordinal"`
		Operation   string          `json:"operation"`
		Create      json.RawMessage `json:"create,omitempty"`
		Replace     json.RawMessage `json:"replace,omitempty"`
		Supersede   json.RawMessage `json:"supersede,omitempty"`
		Relate      json.RawMessage `json:"relate,omitempty"`
		ProposeFact json.RawMessage `json:"propose_fact,omitempty"`
	}
	type planEnvelope struct {
		Schema       string           `json:"schema"`
		PlanRevision int64            `json:"plan_revision"`
		Actions      []actionEnvelope `json:"actions"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	var plan planEnvelope
	if err := decoder.Decode(&plan); err != nil {
		return fmt.Errorf("%w: accepted plan: %v", ErrProtocolResponse, err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return fmt.Errorf("%w: accepted plan: %v", ErrProtocolResponse, err)
	}
	if plan.Schema != MemoryPlanSchemaV1 || plan.PlanRevision != planRevision || plan.Actions == nil || len(plan.Actions) > maximumActions {
		return fmt.Errorf("%w: accepted plan has invalid schema, revision, or action count", ErrProtocolResponse)
	}
	for index, action := range plan.Actions {
		if action.Ordinal != int64(index+1) {
			return fmt.Errorf("%w: accepted-plan action ordinals must be contiguous from one", ErrProtocolResponse)
		}
		payloads := map[string]json.RawMessage{
			"create": action.Create, "replace": action.Replace,
			"supersede": action.Supersede, "relate": action.Relate,
			"propose_fact": action.ProposeFact,
		}
		payloadCount := 0
		for _, payload := range payloads {
			if len(payload) != 0 && string(payload) != "null" {
				payloadCount++
			}
		}
		if _, allowed := payloads[action.Operation]; !allowed || payloadCount != 1 || len(payloads[action.Operation]) == 0 || string(payloads[action.Operation]) == "null" {
			return fmt.Errorf("%w: accepted-plan action %d has an invalid tagged-union payload", ErrProtocolResponse, index+1)
		}
	}
	return nil
}

// semanticallyEqualJSON compares decoded JSON values so harmless object-key
// order and whitespace differences cannot make a fresh plan review fail.
func semanticallyEqualJSON(a, b []byte) bool {
	decode := func(raw []byte) (any, error) {
		if err := rejectDuplicateJSONNames(raw); err != nil {
			return nil, err
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		if err := requireJSONEOF(decoder); err != nil {
			return nil, err
		}
		return value, nil
	}
	aValue, err := decode(a)
	if err != nil {
		return false
	}
	bValue, err := decode(b)
	return err == nil && reflect.DeepEqual(aValue, bValue)
}

func rejectDuplicateJSONNames(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := consumeUniqueJSONValue(decoder); err != nil {
		return err
	}
	return requireJSONEOF(decoder)
}

func consumeUniqueJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("object member name is not a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate object member %q", key)
			}
			seen[key] = struct{}{}
			if err := consumeUniqueJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim('}') {
			return errors.New("object did not terminate")
		}
	case '[':
		for decoder.More() {
			if err := consumeUniqueJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim(']') {
			return errors.New("array did not terminate")
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delimiter)
	}
	return nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing any
	err := decoder.Decode(&trailing)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return err
	}
	return errors.New("unexpected trailing JSON value")
}
