package syncmodel

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"unicode"
	"unicode/utf8"
)

// EnvelopeVersion identifies the harness semantic-envelope wire profile.
const EnvelopeVersion uint32 = 1

// MaxEnvelopeBytes bounds one complete canonical fixture envelope.
const MaxEnvelopeBytes = 64 * 1024

// Origin retains the immutable installation, replica and write-lineage coordinate.
type Origin struct {
	InstallationID string  `json:"installation_id"`
	ReplicaID      string  `json:"replica_id"`
	WriteLineageID string  `json:"write_lineage_id"`
	OriginSequence Ordinal `json:"origin_sequence"`
}

// Validate checks origin references and the exact sequence bound.
func (o Origin) Validate() error {
	for _, s := range []string{o.InstallationID, o.ReplicaID, o.WriteLineageID} {
		if !validLabel(s) {
			return errors.New("invalid origin")
		}
	}
	return o.OriginSequence.Validate()
}

// PrincipalKind identifies the claimed actor class without granting permission.
type PrincipalKind string

// PrincipalAgent, PrincipalOperator and PrincipalService are the closed actor classes.
const (
	PrincipalAgent    PrincipalKind = "agent"
	PrincipalOperator PrincipalKind = "operator"
	PrincipalService  PrincipalKind = "service"
)

// AuthorizationEvidence is a claimed semantic context. Validate only checks
// shape; callers must authenticate against a separately supplied admission scope.
type AuthorizationEvidence struct {
	PrincipalKind        PrincipalKind `json:"principal_kind"`
	PrincipalID          string        `json:"principal_id"`
	AgentID              *string       `json:"agent_id"`
	RealmID              string        `json:"realm_id"`
	AccessProfileID      string        `json:"access_profile_id"`
	PolicyRevision       string        `json:"policy_revision"`
	EnrollmentGeneration string        `json:"enrollment_generation"`
	GrantID              string        `json:"grant_id"`
}

// Validate checks complete authorization evidence and explicit agent identity.
func (a AuthorizationEvidence) Validate() error {
	if a.PrincipalKind != PrincipalAgent && a.PrincipalKind != PrincipalOperator && a.PrincipalKind != PrincipalService {
		return errors.New("invalid principal kind")
	}
	for _, s := range []string{a.PrincipalID, a.RealmID, a.AccessProfileID, a.PolicyRevision, a.EnrollmentGeneration, a.GrantID} {
		if !validLabel(s) {
			return errors.New("invalid authorization evidence")
		}
	}
	if (a.PrincipalKind == PrincipalAgent) != (a.AgentID != nil) {
		return errors.New("invalid agent identity")
	}
	if a.AgentID != nil && (!validLabel(*a.AgentID) || *a.AgentID != a.PrincipalID) {
		return errors.New("invalid agent identity")
	}
	return nil
}

// OperationEnvelope contains every immutable semantic field retained across retry.
type OperationEnvelope struct {
	EnvelopeVersion     uint32                `json:"envelope_version"`
	SchemaVersion       uint32                `json:"schema_version"`
	ProtocolVersion     uint32                `json:"protocol_version"`
	ID                  OperationID           `json:"operation_id"`
	OriginalIncarnation Incarnation           `json:"original_incarnation"`
	Capture             CaptureEpoch          `json:"capture_epoch"`
	Origin              Origin                `json:"origin"`
	Kind                string                `json:"kind"`
	Authorization       AuthorizationEvidence `json:"authorization"`
	Basis               CausalBasis           `json:"basis"`
	Payload             CanonicalTypedValue   `json:"payload"`
	Compound            *CompoundManifest     `json:"compound"`
}

// Validate checks versions, typed values and causal scope without granting authority.
func (e OperationEnvelope) Validate() error {
	if e.EnvelopeVersion != EnvelopeVersion || e.SchemaVersion != 1 || e.ProtocolVersion != 1 {
		return errors.New("unsupported envelope version")
	}
	if !validLabel(e.Kind) {
		return errors.New("invalid operation kind")
	}
	for _, f := range []func() error{e.OriginalIncarnation.Validate, e.Capture.Validate, e.Origin.Validate, e.Authorization.Validate, e.Basis.Validate, e.Payload.Validate} {
		if err := f(); err != nil {
			return err
		}
	}
	for _, d := range e.Basis.Dependencies {
		if d.AccountID != e.OriginalIncarnation.AccountID || d.RealmID != e.Authorization.RealmID {
			return errors.New("dependency outside operation scope")
		}
	}
	if e.Compound != nil {
		return e.Compound.Validate()
	}
	return nil
}

// CanonicalBytes validates and encodes the envelope, sorting only declared sets.
func (e OperationEnvelope) CanonicalBytes() ([]byte, error) {
	if err := e.Validate(); err != nil {
		return nil, err
	}
	e.Basis = e.Basis.canonical()
	if e.Compound != nil {
		m := e.Compound.canonical()
		e.Compound = &m
	}
	b, err := MarshalCanonical(e)
	if err != nil {
		return nil, err
	}
	if len(b) > MaxEnvelopeBytes {
		return nil, errors.New("envelope exceeds bound")
	}
	return b, nil
}

// DecodeOperationEnvelope rejects ambiguous, incomplete or out-of-bounds wire input.
func DecodeOperationEnvelope(raw []byte) (OperationEnvelope, error) {
	var e OperationEnvelope
	if len(raw) > MaxEnvelopeBytes {
		return e, errors.New("envelope exceeds bound")
	}
	if err := UnmarshalStrict(raw, &e); err != nil {
		return OperationEnvelope{}, err
	}
	if err := requireWireFields(raw, reflect.TypeFor[OperationEnvelope]()); err != nil {
		return OperationEnvelope{}, err
	}
	if err := e.Validate(); err != nil {
		return OperationEnvelope{}, err
	}
	return e, nil
}

// AdmissionScope is independently authenticated fixture context, never copied
// from AuthorizationEvidence. Equality/receipt access is separately authorized.
type AdmissionScope struct {
	AccountID     string
	Origin        Origin
	Authorization AuthorizationEvidence
	MayCompare    bool
	MayMutate     bool
}

// AuthorizeAdmission compares claimed evidence with independently authenticated scope.
// Existing outcomes require comparison access; new admissions also require mutation access.
func AuthorizeAdmission(e OperationEnvelope, s AdmissionScope, existing bool) error {
	if err := e.Validate(); err != nil {
		return err
	}
	if !s.MayCompare || (!existing && !s.MayMutate) || s.AccountID != e.OriginalIncarnation.AccountID || s.Origin != e.Origin {
		return errors.New("admission unauthorized")
	}
	a, b := s.Authorization, e.Authorization
	if a.PrincipalKind != b.PrincipalKind || a.PrincipalID != b.PrincipalID || a.RealmID != b.RealmID || a.AccessProfileID != b.AccessProfileID || a.PolicyRevision != b.PolicyRevision || a.EnrollmentGeneration != b.EnrollmentGeneration || a.GrantID != b.GrantID || (a.AgentID == nil) != (b.AgentID == nil) {
		return errors.New("admission unauthorized")
	}
	if a.AgentID != nil && *a.AgentID != *b.AgentID {
		return errors.New("admission unauthorized")
	}
	return nil
}

// ComparisonKeyID names the protected account key fixed at first admission.
type ComparisonKeyID string

// ComparisonKeyProvider supplies fixture comparison keys outside portable snapshots.
type ComparisonKeyProvider interface {
	ComparisonKey(accountID string, keyID ComparisonKeyID) ([]byte, error)
}

// InputCommitment is account scoped and domain separated. Keys are supplied by
// a fixture provider outside snapshot rows; no fallback trusts client digests.
func InputCommitment(p ComparisonKeyProvider, accountID string, keyID ComparisonKeyID, e OperationEnvelope) (Commitment, error) {
	var out Commitment
	if p == nil || !validLabel(accountID) || !validLabel(string(keyID)) || accountID != e.OriginalIncarnation.AccountID {
		return out, errors.New("invalid comparison scope")
	}
	raw, err := e.CanonicalBytes()
	if err != nil {
		return out, err
	}
	key, err := p.ComparisonKey(accountID, keyID)
	if err != nil || len(key) < 32 {
		return out, errors.New("comparison key unavailable")
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte("witself.sync.input.v1\x00"))
	_, _ = mac.Write(raw)
	copy(out[:], mac.Sum(nil))
	return out, nil
}

// AdmissionState records destination admission progress separately from outbound state.
type AdmissionState string

// AdmissionAdmitted, AdmissionStaged and AdmissionTerminal are the closed admission states.
const (
	AdmissionAdmitted AdmissionState = "admitted"
	AdmissionStaged   AdmissionState = "staged"
	AdmissionTerminal AdmissionState = "terminal"
)

// Admission preserves the original identity, key choice and semantic commitment.
type Admission struct {
	AccountID           string          `json:"account_id"`
	Operation           OperationID     `json:"operation_id"`
	OriginalOrigin      Origin          `json:"original_origin"`
	OriginalIncarnation Incarnation     `json:"original_incarnation"`
	ComparisonKeyID     ComparisonKeyID `json:"comparison_key_id"`
	InputCommitment     Commitment      `json:"input_commitment"`
	State               AdmissionState  `json:"state"`
}

// Validate checks the admission's account binding, original coordinates and state.
func (a Admission) Validate() error {
	if !validLabel(a.AccountID) || a.AccountID != a.OriginalIncarnation.AccountID || !validLabel(string(a.ComparisonKeyID)) {
		return errors.New("invalid admission scope")
	}
	if a.State != AdmissionAdmitted && a.State != AdmissionStaged && a.State != AdmissionTerminal {
		return errors.New("invalid admission state")
	}
	if err := a.OriginalOrigin.Validate(); err != nil {
		return err
	}
	return a.OriginalIncarnation.Validate()
}

func validLabel(s string) bool {
	if s == "" || len(s) > 256 || !utf8.ValidString(s) || strings.TrimSpace(s) != s {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// Require explicit fields even where Go's zero value would otherwise turn a
// missing/null scalar into a plausible identity coordinate. Pointers represent
// the profile's explicitly nullable fields; absence is always distinct.
func requireWireFields(raw []byte, t reflect.Type) error {
	if t.Kind() == reflect.Pointer {
		if string(raw) == "null" {
			return nil
		}
		return requireWireFields(raw, t.Elem())
	}
	if string(raw) == "null" {
		return errors.New("missing required wire value")
	}
	if reflect.PointerTo(t).Implements(reflect.TypeFor[json.Unmarshaler]()) {
		return nil
	}
	switch t.Kind() {
	case reflect.Struct:
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return errors.New("invalid wire object")
		}
		allowed := make(map[string]bool, t.NumField())
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			if name == "-" {
				continue
			}
			if name == "" {
				name = f.Name
			}
			allowed[name] = true
			value, ok := fields[name]
			if !ok {
				return errors.New("missing required wire field")
			}
			if err := requireWireFields(value, f.Type); err != nil {
				return err
			}
		}
		for name := range fields {
			if !allowed[name] {
				return errors.New("unknown wire field")
			}
		}
	case reflect.Slice, reflect.Array:
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return errors.New("invalid wire list")
		}
		for _, item := range items {
			if err := requireWireFields(item, t.Elem()); err != nil {
				return err
			}
		}
	}
	return nil
}
