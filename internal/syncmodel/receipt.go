package syncmodel

import (
	"bytes"
	"crypto/ed25519"
	"crypto/hmac"
	"errors"
	"reflect"
)

// ReceiptStatus identifies one of the three immutable SC-4.3 terminal outcomes.
type ReceiptStatus string

// ReceiptReason is a closed SC-4.3 reason, distinct from recovery outcomes.
type ReceiptReason string

// StatusAccepted and the remaining constants define the closed SC-4.3 vocabulary.
const (
	StatusAccepted                ReceiptStatus = "accepted"
	StatusRejected                ReceiptStatus = "rejected"
	StatusConflict                ReceiptStatus = "conflict"
	ReasonApplied                 ReceiptReason = "applied"
	ReasonInvalidEnvelope         ReceiptReason = "invalid_envelope"
	ReasonUnauthorized            ReceiptReason = "unauthorized"
	ReasonAuthorizationExpired    ReceiptReason = "authorization_expired"
	ReasonUnsupportedOperation    ReceiptReason = "unsupported_operation"
	ReasonSchemaIncompatible      ReceiptReason = "schema_incompatible"
	ReasonProtocolIncompatible    ReceiptReason = "protocol_incompatible"
	ReasonCapacityRefused         ReceiptReason = "capacity_refused"
	ReasonAccountClosed           ReceiptReason = "account_closed"
	ReasonExpectedVersionMismatch ReceiptReason = "expected_version_mismatch"
	ReasonDependencyUnavailable   ReceiptReason = "dependency_unavailable"
	ReasonIdentityCollision       ReceiptReason = "identity_collision"
	ReasonDeletionConflict        ReceiptReason = "deletion_conflict"
	ReasonAuthorityEpochStale     ReceiptReason = "authority_epoch_stale"
	ReasonIncarnationMismatch     ReceiptReason = "incarnation_mismatch"
	ReasonOperationIDReuse        ReceiptReason = "operation_id_reuse"
)

// ValidateReceiptPair accepts only the sixteen version-one terminal status/reason pairs.
func ValidateReceiptPair(status ReceiptStatus, reason ReceiptReason) error {
	switch status {
	case StatusAccepted:
		if reason == ReasonApplied {
			return nil
		}
	case StatusRejected:
		switch reason {
		case ReasonInvalidEnvelope, ReasonUnauthorized, ReasonAuthorizationExpired, ReasonUnsupportedOperation, ReasonSchemaIncompatible, ReasonProtocolIncompatible, ReasonCapacityRefused, ReasonAccountClosed:
			return nil
		}
	case StatusConflict:
		switch reason {
		case ReasonExpectedVersionMismatch, ReasonDependencyUnavailable, ReasonIdentityCollision, ReasonDeletionConflict, ReasonAuthorityEpochStale, ReasonIncarnationMismatch, ReasonOperationIDReuse:
			return nil
		}
	}
	return errors.New("invalid receipt status reason")
}

// AuthorityScope distinguishes account authority from a graph-specific stream.
type AuthorityScope string

// AuthorityAccount and AuthorityGraph are the supported receipt authority scopes.
const (
	AuthorityAccount AuthorityScope = "account"
	AuthorityGraph   AuthorityScope = "graph"
)

// AuthorityEpoch binds the numeric epoch to its account or graph authority.
type AuthorityEpoch struct {
	Scope    AuthorityScope `json:"scope"`
	ObjectID *string        `json:"object_id"`
	Epoch    Ordinal        `json:"epoch"`
}

// Validate checks the epoch bound and requires a graph identity only for graph scope.
func (a AuthorityEpoch) Validate() error {
	if err := a.Epoch.Validate(); err != nil {
		return err
	}
	switch a.Scope {
	case AuthorityAccount:
		if a.ObjectID != nil || uint64(a.Epoch) > 9007199254740991 {
			return errors.New("invalid account authority epoch")
		}
	case AuthorityGraph:
		if a.ObjectID == nil || !validLabel(*a.ObjectID) || a.Epoch == 0 {
			return errors.New("invalid graph authority epoch")
		}
	default:
		return errors.New("invalid authority scope")
	}
	return nil
}

// Receipt is a value-free immutable terminal outcome with exact canonical signed bytes.
type Receipt struct {
	Version              uint32              `json:"version"`
	AccountID            string              `json:"account_id"`
	Operation            OperationID         `json:"operation_id"`
	OriginalOrigin       Origin              `json:"original_origin"`
	OriginalIncarnation  Incarnation         `json:"original_incarnation"`
	OriginalCapture      CaptureEpoch        `json:"original_capture"`
	ComparisonKeyID      ComparisonKeyID     `json:"comparison_key_id"`
	InputCommitment      Commitment          `json:"input_commitment"`
	IssuerID             string              `json:"issuer_id"`
	SigningKeyID         string              `json:"signing_key_id"`
	AuthorityStream      string              `json:"authority_stream"`
	IssuerIncarnation    Incarnation         `json:"issuer_incarnation"`
	IssuerAuthorityEpoch AuthorityEpoch      `json:"issuer_authority_epoch"`
	Acceptance           Ordinal             `json:"acceptance_ordinal"`
	Status               ReceiptStatus       `json:"status"`
	Reason               ReceiptReason       `json:"reason"`
	CausalResult         CanonicalTypedValue `json:"causal_result"`
	ResultCommitment     *Commitment         `json:"result_commitment"`
	CanonicalBody        []byte              `json:"-"`
	// SignedBytes is the immutable 64-byte Ed25519 signature. Its signed
	// message is the receipt-specific domain prefix followed by CanonicalBody.
	SignedBytes []byte `json:"-"`
}

// Validate checks operation and issuer scope, terminal vocabulary and value-free result shape.
func (r Receipt) Validate() error {
	if r.Version != 1 {
		return errors.New("unsupported receipt version")
	}
	for _, s := range []string{r.AccountID, string(r.ComparisonKeyID), r.IssuerID, r.SigningKeyID, r.AuthorityStream} {
		if !validLabel(s) {
			return errors.New("invalid receipt scope")
		}
	}
	if r.AccountID != r.OriginalIncarnation.AccountID || r.AccountID != r.IssuerIncarnation.AccountID {
		return errors.New("receipt account mismatch")
	}
	for _, f := range []func() error{r.OriginalOrigin.Validate, r.OriginalIncarnation.Validate, r.OriginalCapture.Validate, r.IssuerIncarnation.Validate, r.IssuerAuthorityEpoch.Validate, r.Acceptance.Validate} {
		if err := f(); err != nil {
			return err
		}
	}
	if r.Acceptance == 0 {
		return errors.New("invalid acceptance ordinal")
	}
	if r.IssuerAuthorityEpoch.Scope == AuthorityAccount && uint64(r.IssuerAuthorityEpoch.Epoch) != uint64(r.IssuerIncarnation.Lifecycle) {
		return errors.New("receipt authority epoch mismatch")
	}
	if err := ValidateReceiptPair(r.Status, r.Reason); err != nil {
		return err
	}
	if r.Status != StatusAccepted && r.ResultCommitment != nil {
		return errors.New("refusal has result commitment")
	}
	return validateIdentityBasis(r.CausalResult)
}

const receiptDomain = "witself.sync.receipt.v1\x00"

func receiptMessage(body []byte) []byte {
	b := make([]byte, 0, len(receiptDomain)+len(body))
	b = append(b, receiptDomain...)
	return append(b, body...)
}

// SignReceipt canonicalizes and signs an unsigned fixture receipt with domain separation.
func SignReceipt(r Receipt, key ed25519.PrivateKey) (Receipt, error) {
	if len(key) != ed25519.PrivateKeySize {
		return Receipt{}, errors.New("signing key unavailable")
	}
	if len(r.CanonicalBody) != 0 || len(r.SignedBytes) != 0 {
		return Receipt{}, errors.New("receipt already signed")
	}
	if err := r.Validate(); err != nil {
		return Receipt{}, err
	}
	body, err := MarshalCanonical(r)
	if err != nil {
		return Receipt{}, err
	}
	r.CanonicalBody = body
	r.SignedBytes = ed25519.Sign(key, receiptMessage(body))
	return r, nil
}

// ReceiptExpectation binds verification to the immutable submitted operation
// and an independently obtained authority trust binding.
type ReceiptExpectation struct {
	OriginalEnvelope OperationEnvelope
	ComparisonKeyID  ComparisonKeyID
	InputCommitment  Commitment
	Trust            ReceiptTrust
}

// ReceiptTrust is an independently supplied issuer binding and historical membership proof.
type ReceiptTrust struct {
	IssuerID          string
	SigningKeyID      string
	AuthorityStream   string
	IssuerIncarnation Incarnation
	AuthorityEpoch    AuthorityEpoch
	Retired           bool
	// SealedBodies is fixture evidence authenticated outside node snapshots.
	// Exact content membership is mandatory for retired keys; a numeric cutoff
	// alone is deliberately insufficient.
	SealedBodies [][]byte
}

// ReceiptVerification reports signature evidence without conferring present authority.
type ReceiptVerification struct {
	SignatureValid bool
	HistoricalOnly bool
	// Receipts never independently prove current execution authority.
	CurrentAuthority bool
}

// VerifyReceipt checks exact bytes, signature, submitted-operation binding and issuer trust.
func VerifyReceipt(r Receipt, key ed25519.PublicKey, expected ReceiptExpectation) (ReceiptVerification, error) {
	var out ReceiptVerification
	if len(key) != ed25519.PublicKeySize || len(r.SignedBytes) != ed25519.SignatureSize {
		return out, errors.New("receipt authentication failed")
	}
	if err := r.Validate(); err != nil {
		return out, err
	}
	body, err := MarshalCanonical(r)
	if err != nil {
		return out, err
	}
	if !bytes.Equal(body, r.CanonicalBody) || !ed25519.Verify(key, receiptMessage(r.CanonicalBody), r.SignedBytes) {
		return out, errors.New("receipt authentication failed")
	}
	e := expected.OriginalEnvelope
	if err := e.Validate(); err != nil {
		return out, errors.New("invalid expected operation")
	}
	if r.AccountID != e.OriginalIncarnation.AccountID || r.Operation != e.ID || r.OriginalOrigin != e.Origin || r.OriginalIncarnation != e.OriginalIncarnation || r.OriginalCapture != e.Capture || r.ComparisonKeyID != expected.ComparisonKeyID || !hmac.Equal(r.InputCommitment[:], expected.InputCommitment[:]) {
		return out, errors.New("receipt operation binding failed")
	}
	t := expected.Trust
	a, b := r.IssuerAuthorityEpoch, t.AuthorityEpoch
	if r.IssuerID != t.IssuerID || r.SigningKeyID != t.SigningKeyID || r.AuthorityStream != t.AuthorityStream || r.IssuerIncarnation != t.IssuerIncarnation || a.Scope != b.Scope || a.Epoch != b.Epoch || (a.ObjectID == nil) != (b.ObjectID == nil) {
		return out, errors.New("receipt trust binding failed")
	}
	if a.ObjectID != nil && *a.ObjectID != *b.ObjectID {
		return out, errors.New("receipt trust binding failed")
	}
	if t.Retired {
		member := false
		for _, sealed := range t.SealedBodies {
			if bytes.Equal(sealed, r.CanonicalBody) {
				member = true
				break
			}
		}
		if !member {
			return out, errors.New("historical receipt membership unproven")
		}
	}
	return ReceiptVerification{SignatureValid: true, HistoricalOnly: t.Retired, CurrentAuthority: false}, nil
}

// VerifyHistoricalReceipt additionally requires exact membership in an authenticated sealed prefix.
func VerifyHistoricalReceipt(r Receipt, key ed25519.PublicKey, expected ReceiptExpectation, sealedBodies [][]byte) (ReceiptVerification, error) {
	expected.Trust.Retired = true
	expected.Trust.SealedBodies = sealedBodies
	return VerifyReceipt(r, key, expected)
}

// DecodeReceipt restores exact stored bytes after strict schema and canonical-form validation.
// Signature and trust validation remain separate in VerifyReceipt.
func DecodeReceipt(body, signedBytes []byte) (Receipt, error) {
	var r Receipt
	if len(body) > MaxEnvelopeBytes || len(signedBytes) != ed25519.SignatureSize {
		return r, errors.New("invalid receipt encoding")
	}
	if err := UnmarshalStrict(body, &r); err != nil {
		return Receipt{}, err
	}
	if err := requireWireFields(body, reflect.TypeFor[Receipt]()); err != nil {
		return Receipt{}, err
	}
	if err := r.Validate(); err != nil {
		return Receipt{}, err
	}
	b, err := MarshalCanonical(r)
	if err != nil {
		return Receipt{}, err
	}
	if !bytes.Equal(b, body) {
		return Receipt{}, errors.New("noncanonical receipt body")
	}
	r.CanonicalBody = append([]byte(nil), body...)
	r.SignedBytes = append([]byte(nil), signedBytes...)
	return r, nil
}

// ReuseRefusal binds a separate immutable denial to one conflicting semantic commitment.
type ReuseRefusal struct {
	AccountID        string
	Operation        OperationID
	ComparisonKeyID  ComparisonKeyID
	ConflictingInput Commitment
	Receipt          Receipt
}

// Validate requires a conflict/operation_id_reuse receipt matching the refusal key.
func (r ReuseRefusal) Validate() error {
	if err := r.Receipt.Validate(); err != nil {
		return err
	}
	if r.AccountID != r.Receipt.AccountID || r.Operation != r.Receipt.Operation || r.ComparisonKeyID != r.Receipt.ComparisonKeyID || r.ConflictingInput != r.Receipt.InputCommitment || r.Receipt.Status != StatusConflict || r.Receipt.Reason != ReasonOperationIDReuse {
		return errors.New("invalid reuse refusal")
	}
	return nil
}

// The fixture receipt basis accepts only immutable identity coordinates. It
// deliberately cannot carry arbitrary user strings, binary values or payloads.
func validateIdentityBasis(v CanonicalTypedValue) error {
	if err := v.Validate(); err != nil {
		return err
	}
	if v.Kind() == KindNull {
		return nil
	}
	fields, ok := v.Fields()
	if !ok {
		return errors.New("invalid identity basis")
	}
	for _, f := range fields {
		s, ok := f.Value.Text()
		if !ok {
			return errors.New("invalid identity basis")
		}
		switch f.Name {
		case "generation":
			if _, err := ParseGenerationID(s); err != nil {
				return errors.New("invalid identity basis")
			}
		case "revision":
			if _, err := ParseRevisionID(s); err != nil {
				return errors.New("invalid identity basis")
			}
		case "operation":
			if _, err := ParseOperationID(s); err != nil {
				return errors.New("invalid identity basis")
			}
		default:
			return errors.New("invalid identity basis")
		}
	}
	return nil
}
