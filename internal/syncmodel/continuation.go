package syncmodel

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"reflect"
)

const cursorDomain = "witself.sync.cursor.v1\x00"

// CursorFrontier binds one complete applied stream prefix to its exact content.
type CursorFrontier struct {
	Stream  string       `json:"stream"`
	Capture CaptureEpoch `json:"capture"`
	Through Ordinal      `json:"through"`
	Root    Commitment   `json:"root"`
}

// Cursor is a synthetic versioned continuation proof. Its complete sorted vector
// includes empty streams. The independent expectation supplies current admission
// and the authenticated required catalog; signing alone grants neither.
type Cursor struct {
	Version            uint32           `json:"version"`
	IssuerID           string           `json:"issuer_id"`
	SigningKeyID       string           `json:"signing_key_id"`
	Audience           string           `json:"audience"`
	Incarnation        Incarnation      `json:"incarnation"`
	ReplicaID          string           `json:"replica_id"`
	Enrollment         string           `json:"enrollment_generation"`
	Authority          AuthorityEpoch   `json:"authority_epoch"`
	Scope              string           `json:"scope"`
	Catalog            Commitment       `json:"stream_catalog"`
	Frontiers          []CursorFrontier `json:"frontiers"`
	DeletionCoverage   Ordinal          `json:"deletion_coverage"`
	RevocationCoverage Ordinal          `json:"revocation_coverage"`
	Policy             string           `json:"policy"`
	Grant              string           `json:"grant"`
	ExpiresAt          Ordinal          `json:"expires_at"`
	CanonicalBody      []byte           `json:"-"`
	SignedBytes        []byte           `json:"-"`
}

// Validate checks closed scope, full vector shape and exact counter bounds.
func (c Cursor) Validate() error {
	if c.Version != 1 || c.Incarnation.Validate() != nil || c.Authority.Validate() != nil || c.ExpiresAt == 0 || c.ExpiresAt.Validate() != nil || c.DeletionCoverage.Validate() != nil || c.RevocationCoverage.Validate() != nil {
		return errors.New("invalid cursor")
	}
	for _, label := range []string{c.IssuerID, c.SigningKeyID, c.Audience, c.ReplicaID, c.Enrollment, c.Policy, c.Grant} {
		if !validLabel(label) {
			return errors.New("invalid cursor binding")
		}
	}
	if (c.Scope != "account" && c.Scope != "stream") || len(c.Frontiers) == 0 || len(c.Frontiers) > 64 || (c.Scope == "stream" && len(c.Frontiers) != 1) {
		return errors.New("invalid cursor scope")
	}
	if c.Authority.Scope == AuthorityAccount && uint64(c.Authority.Epoch) != uint64(c.Incarnation.Lifecycle) {
		return errors.New("cursor authority epoch mismatch")
	}
	for i, frontier := range c.Frontiers {
		if !validLabel(frontier.Stream) || frontier.Capture.Validate() != nil || frontier.Through.Validate() != nil || (i > 0 && c.Frontiers[i-1].Stream >= frontier.Stream) {
			return errors.New("invalid cursor frontier vector")
		}
	}
	return nil
}

func cursorMessage(body []byte) []byte {
	return append(append(make([]byte, 0, len(cursorDomain)+len(body)), cursorDomain...), body...)
}

// SignCursor signs the exact complete fixture proof in a distinct receipt domain.
func SignCursor(c Cursor, key ed25519.PrivateKey) (Cursor, error) {
	if len(key) != ed25519.PrivateKeySize || len(c.CanonicalBody) != 0 || len(c.SignedBytes) != 0 {
		return Cursor{}, errors.New("cursor signing unavailable")
	}
	if err := c.Validate(); err != nil {
		return Cursor{}, err
	}
	body, err := MarshalCanonical(c)
	if err != nil {
		return Cursor{}, err
	}
	c.CanonicalBody = body
	c.SignedBytes = ed25519.Sign(key, cursorMessage(body))
	return c, nil
}

// CursorExpectation must be selected independently of the presented cursor.
// Bindings includes the full catalog and frontier vector, plus exact grant and
// coverage requirements. Admission cannot be inferred from a valid signature.
type CursorExpectation struct {
	Bindings          Cursor
	Now               Ordinal
	AdmissionVerified bool
	ReplicaActive     bool
	SignerRetired     bool
	ClockCertain      bool
}

// VerifyCursor checks signed bytes first, then independent admission, expiry and
// every expected binding. Historical receipt keys never authorize continuation.
func VerifyCursor(c Cursor, key ed25519.PublicKey, expected CursorExpectation) error {
	if len(key) != ed25519.PublicKeySize || len(c.SignedBytes) != ed25519.SignatureSize || c.Validate() != nil {
		return errors.New("cursor authentication failed")
	}
	body, err := MarshalCanonical(c)
	if err != nil || !bytes.Equal(body, c.CanonicalBody) || !ed25519.Verify(key, cursorMessage(c.CanonicalBody), c.SignedBytes) {
		return errors.New("cursor authentication failed")
	}
	if !expected.AdmissionVerified || !expected.ReplicaActive || expected.SignerRetired || !expected.ClockCertain || expected.Now.Validate() != nil || expected.Now >= c.ExpiresAt {
		return errors.New("cursor continuation denied")
	}
	if expected.Bindings.Validate() != nil {
		return errors.New("invalid cursor expectation")
	}
	want, err := MarshalCanonical(expected.Bindings)
	if err != nil || !bytes.Equal(body, want) {
		return errors.New("cursor binding mismatch")
	}
	return nil
}

// DecodeCursor validates strict canonical wire bytes without asserting trust.
func DecodeCursor(body, signature []byte) (Cursor, error) {
	var c Cursor
	if len(body) > MaxEnvelopeBytes || len(signature) != ed25519.SignatureSize {
		return c, errors.New("invalid cursor encoding")
	}
	if err := UnmarshalStrict(body, &c); err != nil {
		return Cursor{}, err
	}
	if err := requireWireFields(body, reflect.TypeFor[Cursor]()); err != nil {
		return Cursor{}, err
	}
	if err := c.Validate(); err != nil {
		return Cursor{}, err
	}
	canonical, err := MarshalCanonical(c)
	if err != nil || !bytes.Equal(canonical, body) {
		return Cursor{}, errors.New("noncanonical cursor")
	}
	c.CanonicalBody, c.SignedBytes = append([]byte(nil), body...), append([]byte(nil), signature...)
	return c, nil
}

// ReceiptChainEntry pairs a receipt with independently selected trust and exact
// operation expectations. The verifier supplies expected ordinal/epoch even if
// the caller omitted their optional standalone-receipt checks.
type ReceiptChainEntry struct {
	Receipt     Receipt
	PublicKey   ed25519.PublicKey
	Expectation ReceiptExpectation
}

// ReceiptChainExpectation is the authenticated prefix boundary selected outside
// the restored node. The previous root supports a proved contiguous suffix; it
// is never inferred from the presented entries. This model is not a deployed
// witness or a trust-distribution policy.
type ReceiptChainExpectation struct {
	AccountID, IssuerID, Stream string
	Incarnation                 Incarnation
	Authority                   AuthorityEpoch
	From, Through               Ordinal
	PreviousRoot, Root          Commitment
}

func receiptChainStep(previous Commitment, receipt Receipt) Commitment {
	h := sha256.New()
	_, _ = h.Write([]byte("witself.sync.receipt-chain.v1\x00"))
	_, _ = h.Write(previous[:])
	_, _ = h.Write(receipt.CanonicalBody)
	_, _ = h.Write(receipt.SignedBytes)
	var result Commitment
	copy(result[:], h.Sum(nil))
	return result
}

// ReceiptChainRoot is a fixture construction helper, not verification. Roots are
// commitments to value-free signed evidence and never printed diagnostics.
func ReceiptChainRoot(previous Commitment, receipts []Receipt) Commitment {
	for _, receipt := range receipts {
		previous = receiptChainStep(previous, receipt)
	}
	return previous
}

// VerifyReceiptChain checks every operation, exact acceptance coordinate,
// authority scope, contiguous position and complete content root. A legitimate
// retired key re-signing other content at an allowed ordinal still fails.
func VerifyReceiptChain(entries []ReceiptChainEntry, expected ReceiptChainExpectation) error {
	if !validOrdinalRange(OrdinalRange{expected.From, expected.Through}) || !validLabel(expected.AccountID) || !validLabel(expected.IssuerID) || !validLabel(expected.Stream) || expected.Incarnation.Validate() != nil || expected.Incarnation.AccountID != expected.AccountID || expected.Authority.Validate() != nil || len(entries) == 0 || len(entries) > 1024 || uint64(len(entries)) != uint64(expected.Through-expected.From)+1 {
		return errors.New("invalid receipt chain expectation")
	}
	root := expected.PreviousRoot
	for i, entry := range entries {
		receipt := entry.Receipt
		if receipt.AccountID != expected.AccountID || receipt.IssuerID != expected.IssuerID || receipt.AuthorityStream != expected.Stream || receipt.IssuerIncarnation != expected.Incarnation {
			return errors.New("receipt chain scope mismatch")
		}
		ordinal := expected.From + Ordinal(i)
		expectation := entry.Expectation
		if (expectation.ExpectedAcceptance != nil && *expectation.ExpectedAcceptance != ordinal) || (expectation.ExpectedIssuerAuthorityEpoch != nil && !sameAuthorityEpoch(*expectation.ExpectedIssuerAuthorityEpoch, expected.Authority)) {
			return errors.New("receipt chain expectation conflict")
		}
		expectation.ExpectedAcceptance = &ordinal
		expectation.ExpectedIssuerAuthorityEpoch = &expected.Authority
		if _, err := VerifyReceipt(receipt, entry.PublicKey, expectation); err != nil {
			return err
		}
		root = receiptChainStep(root, receipt)
	}
	if root != expected.Root {
		return errors.New("receipt chain membership unproven")
	}
	return nil
}
