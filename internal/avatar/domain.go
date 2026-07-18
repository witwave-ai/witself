// Package avatar defines the model-free domain contract for agent avatars.
package avatar

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// MaxDescriptionBytes bounds a stored, human-readable avatar description.
	MaxDescriptionBytes = 4 * 1024
	// MaxSpecJSONBytes bounds compact structured visual specifications. The
	// database constrains PostgreSQL's jsonb::text representation to 16 KiB;
	// jsonb adds separator spaces that encoding/json omits. Keeping 1 KiB of
	// headroom makes every accepted 512-node spec persistable.
	MaxSpecJSONBytes = 15 * 1024
	// MaxSpecJSONDepth bounds adversarial nesting independently of byte size.
	MaxSpecJSONDepth = 16
	// MaxSpecJSONNodes bounds the number of values in a visual specification.
	MaxSpecJSONNodes = 512
	// MaxSVGBytes bounds source and sanitized SVG avatar assets.
	MaxSVGBytes = 64 * 1024
)

var (
	// ErrInvalidSubjectForm marks an unsupported avatar subject form.
	ErrInvalidSubjectForm = errors.New("invalid avatar subject form")
	// ErrInvalidAutonomyPolicy marks an unsupported avatar autonomy policy.
	ErrInvalidAutonomyPolicy = errors.New("invalid avatar autonomy policy")
	// ErrInvalidStatus marks an unsupported avatar lifecycle status.
	ErrInvalidStatus = errors.New("invalid avatar status")
	// ErrInvalidEventType marks an unsupported avatar lifecycle event.
	ErrInvalidEventType = errors.New("invalid avatar event type")
	// ErrInvalidDescription marks an invalid or over-limit avatar description.
	ErrInvalidDescription = errors.New("invalid avatar description")
	// ErrInvalidSpecJSON marks invalid or over-limit avatar visual-spec JSON.
	ErrInvalidSpecJSON = errors.New("invalid avatar spec JSON")
)

// SubjectForm describes what an avatar depicts independently of its visual
// style. A realm can therefore render humans, animals, and insects through one
// consistent style pack.
type SubjectForm string

// SubjectHuman and the other SubjectForm constants identify the supported
// kinds of subjects an avatar may depict.
const (
	SubjectHuman           SubjectForm = "human"
	SubjectAnimal          SubjectForm = "animal"
	SubjectInsect          SubjectForm = "insect"
	SubjectAnthropomorphic SubjectForm = "anthropomorphic"
	SubjectHybrid          SubjectForm = "hybrid"
	SubjectRobot           SubjectForm = "robot"
	SubjectSymbolic        SubjectForm = "symbolic"
)

var subjectForms = []SubjectForm{
	SubjectHuman,
	SubjectAnimal,
	SubjectInsect,
	SubjectAnthropomorphic,
	SubjectHybrid,
	SubjectRobot,
	SubjectSymbolic,
}

// SubjectForms returns the supported forms in stable presentation order.
func SubjectForms() []SubjectForm {
	return append([]SubjectForm(nil), subjectForms...)
}

// Valid reports whether f is a supported subject form.
func (f SubjectForm) Valid() bool {
	for _, candidate := range subjectForms {
		if f == candidate {
			return true
		}
	}
	return false
}

// Validate returns an error when f is not a supported subject form.
func (f SubjectForm) Validate() error {
	if !f.Valid() {
		return fmt.Errorf("%w: %q", ErrInvalidSubjectForm, f)
	}
	return nil
}

// AutonomyPolicy controls who may activate an avatar proposal. Creative
// self-selection and authorization remain separate concerns.
type AutonomyPolicy string

// AutonomyOperatorOnly and the other AutonomyPolicy constants identify who
// may activate an avatar proposal.
const (
	AutonomyOperatorOnly     AutonomyPolicy = "operator_only"
	AutonomyAgentProposes    AutonomyPolicy = "agent_proposes"
	AutonomyAgentSelfManaged AutonomyPolicy = "agent_self_managed"
)

var autonomyPolicies = []AutonomyPolicy{
	AutonomyOperatorOnly,
	AutonomyAgentProposes,
	AutonomyAgentSelfManaged,
}

// AutonomyPolicies returns the supported policies in increasing autonomy.
func AutonomyPolicies() []AutonomyPolicy {
	return append([]AutonomyPolicy(nil), autonomyPolicies...)
}

// DefaultAutonomyPolicy returns the low-risk v1 default. Self-management
// controls activation authority only; it never bypasses style, locked-trait,
// payload, or SVG safety validation.
func DefaultAutonomyPolicy() AutonomyPolicy {
	return AutonomyAgentSelfManaged
}

// Valid reports whether p is a supported autonomy policy.
func (p AutonomyPolicy) Valid() bool {
	for _, candidate := range autonomyPolicies {
		if p == candidate {
			return true
		}
	}
	return false
}

// Validate returns an error when p is not a supported autonomy policy.
func (p AutonomyPolicy) Validate() error {
	if !p.Valid() {
		return fmt.Errorf("%w: %q", ErrInvalidAutonomyPolicy, p)
	}
	return nil
}

// Status is the persisted state of an agent's avatar profile.
type Status string

// StatusPlaceholder and the other Status constants identify persisted avatar
// lifecycle states.
const (
	StatusPlaceholder      Status = "placeholder"
	StatusGenerationDue    Status = "generation_due"
	StatusProposed         Status = "proposed"
	StatusActive           Status = "active"
	StatusEvolutionDue     Status = "evolution_due"
	StatusRejected         Status = "rejected"
	StatusGenerationFailed Status = "generation_failed"
	StatusArchived         Status = "archived"
)

var statuses = []Status{
	StatusPlaceholder,
	StatusGenerationDue,
	StatusProposed,
	StatusActive,
	StatusEvolutionDue,
	StatusRejected,
	StatusGenerationFailed,
	StatusArchived,
}

// Statuses returns all supported profile states.
func Statuses() []Status {
	return append([]Status(nil), statuses...)
}

// Valid reports whether s is a supported profile state.
func (s Status) Valid() bool {
	for _, candidate := range statuses {
		if s == candidate {
			return true
		}
	}
	return false
}

// Validate returns an error when s is not a supported profile state.
func (s Status) Validate() error {
	if !s.Valid() {
		return fmt.Errorf("%w: %q", ErrInvalidStatus, s)
	}
	return nil
}

// EventType is the stable outbound lifecycle-event vocabulary. Hooks consume
// profile state; events notify integrations after durable transitions.
type EventType string

// EventGenerationRequested and the other EventType constants identify stable
// outbound avatar lifecycle events.
const (
	EventGenerationRequested EventType = "avatar.generation.requested"
	EventProposed            EventType = "avatar.proposed"
	EventActivated           EventType = "avatar.activated"
	EventEvolved             EventType = "avatar.evolved"
	EventRejected            EventType = "avatar.rejected"
	EventGenerationFailed    EventType = "avatar.generation.failed"
	EventRolledBack          EventType = "avatar.rolled_back"
	EventReset               EventType = "avatar.reset"
	EventPolicyChanged       EventType = "avatar.policy.changed"
	EventStyleChanged        EventType = "avatar.style.changed"
)

var eventTypes = []EventType{
	EventGenerationRequested,
	EventProposed,
	EventActivated,
	EventEvolved,
	EventRejected,
	EventGenerationFailed,
	EventRolledBack,
	EventReset,
	EventPolicyChanged,
	EventStyleChanged,
}

// EventTypes returns all supported lifecycle events.
func EventTypes() []EventType {
	return append([]EventType(nil), eventTypes...)
}

// Valid reports whether e is a supported lifecycle event.
func (e EventType) Valid() bool {
	for _, candidate := range eventTypes {
		if e == candidate {
			return true
		}
	}
	return false
}

// Validate returns an error when e is not a supported lifecycle event.
func (e EventType) Validate() error {
	if !e.Valid() {
		return fmt.Errorf("%w: %q", ErrInvalidEventType, e)
	}
	return nil
}

// NormalizeDescription trims and validates a human-readable avatar
// description. Newlines and tabs are allowed, but other control characters are
// rejected so the value remains safe to display across clients.
func NormalizeDescription(description string) (string, error) {
	if !utf8.ValidString(description) {
		return "", fmt.Errorf("%w: description is not valid UTF-8", ErrInvalidDescription)
	}
	description = strings.TrimSpace(description)
	if description == "" {
		return "", fmt.Errorf("%w: description must not be empty", ErrInvalidDescription)
	}
	if len(description) > MaxDescriptionBytes {
		return "", fmt.Errorf("%w: description exceeds %d bytes", ErrInvalidDescription, MaxDescriptionBytes)
	}
	for _, r := range description {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return "", fmt.Errorf("%w: description contains a control character", ErrInvalidDescription)
		}
	}
	return description, nil
}

// ValidateDescription validates description without changing it.
func ValidateDescription(description string) error {
	_, err := NormalizeDescription(description)
	return err
}

// NormalizeSpecJSON validates and returns a canonical compact JSON object.
// Visual specs intentionally remain extensible, but are bounded by byte size,
// depth, node count, and unique object member names.
func NormalizeSpecJSON(raw []byte) ([]byte, error) {
	if len(raw) == 0 || len(raw) > MaxSpecJSONBytes || !utf8.Valid(raw) {
		return nil, fmt.Errorf("%w: spec must contain 1-%d bytes of valid UTF-8", ErrInvalidSpecJSON, MaxSpecJSONBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	nodes := 0
	value, err := decodeSpecValue(decoder, 1, &nodes)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidSpecJSON, err)
	}
	if _, ok := value.(map[string]any); !ok {
		return nil, fmt.Errorf("%w: spec must be a JSON object", ErrInvalidSpecJSON)
	}
	if token, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("%w: unexpected trailing token %v", ErrInvalidSpecJSON, token)
		}
		return nil, fmt.Errorf("%w: trailing data: %v", ErrInvalidSpecJSON, err)
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidSpecJSON, err)
	}
	if len(canonical) > MaxSpecJSONBytes {
		return nil, fmt.Errorf("%w: canonical spec exceeds %d bytes", ErrInvalidSpecJSON, MaxSpecJSONBytes)
	}
	return canonical, nil
}

// ValidateSpecJSON validates a structured visual specification.
func ValidateSpecJSON(raw []byte) error {
	_, err := NormalizeSpecJSON(raw)
	return err
}

func decodeSpecValue(decoder *json.Decoder, depth int, nodes *int) (any, error) {
	if depth > MaxSpecJSONDepth {
		return nil, fmt.Errorf("maximum depth is %d", MaxSpecJSONDepth)
	}
	*nodes++
	if *nodes > MaxSpecJSONNodes {
		return nil, fmt.Errorf("maximum node count is %d", MaxSpecJSONNodes)
	}
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delim, isDelim := token.(json.Delim)
	if !isDelim {
		switch value := token.(type) {
		case nil, bool, string:
			return token, nil
		case json.Number:
			// PostgreSQL jsonb expands exponent notation to its exact decimal
			// representation. A tiny input such as 1e20000 can therefore exceed
			// the database byte constraint after parsing. Plain JSON decimals do
			// not have that unbounded representation change.
			if strings.ContainsAny(value.String(), "eE") {
				return nil, errors.New("exponent-form numbers are not supported")
			}
			return value, nil
		default:
			return nil, fmt.Errorf("unsupported JSON token %T", token)
		}
	}
	switch delim {
	case '{':
		object := map[string]any{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, errors.New("object member name is not a string")
			}
			if _, exists := object[key]; exists {
				return nil, fmt.Errorf("duplicate object member %q", key)
			}
			member, err := decodeSpecValue(decoder, depth+1, nodes)
			if err != nil {
				return nil, err
			}
			object[key] = member
		}
		closeToken, err := decoder.Token()
		if err != nil || closeToken != json.Delim('}') {
			return nil, errors.New("unterminated object")
		}
		return object, nil
	case '[':
		array := []any{}
		for decoder.More() {
			member, err := decodeSpecValue(decoder, depth+1, nodes)
			if err != nil {
				return nil, err
			}
			array = append(array, member)
		}
		closeToken, err := decoder.Token()
		if err != nil || closeToken != json.Delim(']') {
			return nil, errors.New("unterminated array")
		}
		return array, nil
	default:
		return nil, fmt.Errorf("unexpected delimiter %q", delim)
	}
}
