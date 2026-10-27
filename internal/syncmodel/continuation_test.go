package syncmodel

import (
	"bytes"
	"crypto/ed25519"
	"strings"
	"testing"
)

func fixtureCursor(t *testing.T) (Cursor, ed25519.PrivateKey, CursorExpectation) {
	t.Helper()
	r, key, _ := fixtureReceipt(t)
	unsigned := Cursor{Version: 1, IssuerID: r.IssuerID, SigningKeyID: r.SigningKeyID, Audience: "destination", Incarnation: r.IssuerIncarnation, ReplicaID: "replica", Enrollment: "enrollment", Authority: r.IssuerAuthorityEpoch, Scope: "account", Catalog: Commitment{1}, Frontiers: []CursorFrontier{{Stream: "empty", Capture: 1}, {Stream: "operations", Capture: 1, Through: 3, Root: Commitment{2}}}, DeletionCoverage: 3, RevocationCoverage: 3, Policy: "policy", Grant: "grant", ExpiresAt: 100}
	c, err := SignCursor(unsigned, key)
	if err != nil {
		t.Fatal(err)
	}
	return c, key, CursorExpectation{Bindings: unsigned, Now: 50, AdmissionVerified: true, ReplicaActive: true, ClockCertain: true}
}

func TestSignedCursorFieldMutation(t *testing.T) {
	cursor, key, expected := fixtureCursor(t)
	public := key.Public().(ed25519.PublicKey)
	if err := VerifyCursor(cursor, public, expected); err != nil {
		t.Fatal("valid fixture cursor refused")
	}
	mutations := map[string]func(*Cursor){
		"issuer":               func(c *Cursor) { c.IssuerID = "other" },
		"key":                  func(c *Cursor) { c.SigningKeyID = "other" },
		"audience":             func(c *Cursor) { c.Audience = "other" },
		"account":              func(c *Cursor) { c.Incarnation.AccountID = "other" },
		"incarnation":          func(c *Cursor) { c.Incarnation.Restore[0]++ },
		"replica":              func(c *Cursor) { c.ReplicaID = "other" },
		"enrollment":           func(c *Cursor) { c.Enrollment = "other" },
		"epoch":                func(c *Cursor) { c.Authority.Epoch++; c.Incarnation.Lifecycle++ },
		"scope":                func(c *Cursor) { c.Scope = "stream"; c.Frontiers = c.Frontiers[1:] },
		"catalog":              func(c *Cursor) { c.Catalog[0]++ },
		"missing_empty_stream": func(c *Cursor) { c.Frontiers = c.Frontiers[1:] },
		"stream":               func(c *Cursor) { c.Frontiers[1].Stream = "other" },
		"capture":              func(c *Cursor) { c.Frontiers[1].Capture++ },
		"watermark":            func(c *Cursor) { c.Frontiers[1].Through++ },
		"root":                 func(c *Cursor) { c.Frontiers[1].Root[0]++ },
		"deletion":             func(c *Cursor) { c.DeletionCoverage++ },
		"revocation":           func(c *Cursor) { c.RevocationCoverage++ },
		"policy":               func(c *Cursor) { c.Policy = "other" },
		"grant":                func(c *Cursor) { c.Grant = "other" },
		"expiry":               func(c *Cursor) { c.ExpiresAt++ },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			changed := cursor
			changed.Frontiers = append([]CursorFrontier(nil), cursor.Frontiers...)
			mutate(&changed)
			if err := VerifyCursor(changed, public, expected); err == nil {
				t.Fatal("mutated signed cursor accepted")
			}
			changed.CanonicalBody, changed.SignedBytes = nil, nil
			changed, err := SignCursor(changed, key)
			if err != nil {
				t.Fatal("cursor mutation fixture not structurally valid")
			}
			if err := VerifyCursor(changed, public, expected); err == nil {
				t.Fatal("legitimately resigned wrong binding accepted")
			}
		})
	}
	for _, mutate := range []func(*CursorExpectation){
		func(e *CursorExpectation) { e.Now = cursor.ExpiresAt },
		func(e *CursorExpectation) { e.AdmissionVerified = false },
		func(e *CursorExpectation) { e.ReplicaActive = false },
		func(e *CursorExpectation) { e.SignerRetired = true },
		func(e *CursorExpectation) { e.ClockCertain = false },
	} {
		bad := expected
		mutate(&bad)
		if VerifyCursor(cursor, public, bad) == nil {
			t.Fatal("signature substituted for admission, freshness or clock certainty")
		}
	}
	bad := cursor
	bad.Version = 2
	if VerifyCursor(bad, public, expected) == nil {
		t.Fatal("unknown cursor version accepted")
	}
	bad = cursor
	bad.SignedBytes = ed25519.Sign(key, receiptMessage(cursor.CanonicalBody))
	if VerifyCursor(bad, public, expected) == nil {
		t.Fatal("receipt domain signature accepted for cursor")
	}
	wrongKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize)).Public().(ed25519.PublicKey)
	if VerifyCursor(cursor, wrongKey, expected) == nil {
		t.Fatal("unknown cursor signing key accepted")
	}
}

func TestCursorStrictCodec(t *testing.T) {
	c, key, expected := fixtureCursor(t)
	decoded, err := DecodeCursor(c.CanonicalBody, c.SignedBytes)
	if err != nil || VerifyCursor(decoded, key.Public().(ed25519.PublicKey), expected) != nil {
		t.Fatal("cursor canonical round trip failed")
	}
	for _, test := range []struct{ from, to string }{
		{`"version":1`, `"version":1,"extra":true`},
		{`"version":1`, `"version":1,"version":1`},
		{`"version":1`, `"version":2`},
		{`"expires_at":"100"`, `"expires_at":100`},
		{`"expires_at":"100"`, `"expires_at":"0100"`},
		{`"audience":"destination",`, ``},
	} {
		bad := strings.Replace(string(c.CanonicalBody), test.from, test.to, 1)
		if bad == string(c.CanonicalBody) {
			t.Fatal("cursor codec mutation did not match")
		}
		if _, err := DecodeCursor([]byte(bad), c.SignedBytes); err == nil {
			t.Fatal("malformed cursor encoding accepted")
		}
	}
}

func fixtureReceiptChain(t *testing.T) ([]ReceiptChainEntry, ReceiptChainExpectation, ed25519.PrivateKey) {
	t.Helper()
	var entries []ReceiptChainEntry
	var receipts []Receipt
	var key ed25519.PrivateKey
	for i := range 3 {
		r, signer, expected := fixtureReceipt(t)
		key = signer
		r.Acceptance = Ordinal(i + 1)
		r.Operation[0] = byte(i + 1)
		r.OriginalOrigin.OriginSequence = Ordinal(i + 1)
		expected.OriginalEnvelope.ID = r.Operation
		expected.OriginalEnvelope.Origin = r.OriginalOrigin
		commitment, err := InputCommitment(fixtureComparisonKeys{}, r.AccountID, r.ComparisonKeyID, expected.OriginalEnvelope)
		if err != nil {
			t.Fatal("chain comparison fixture failed")
		}
		r.InputCommitment, expected.InputCommitment = commitment, commitment
		r.CanonicalBody, r.SignedBytes = nil, nil
		r, err = SignReceipt(r, key)
		if err != nil {
			t.Fatal("chain signing fixture failed")
		}
		entries = append(entries, ReceiptChainEntry{Receipt: r, PublicKey: key.Public().(ed25519.PublicKey), Expectation: expected})
		receipts = append(receipts, r)
	}
	r := receipts[0]
	expected := ReceiptChainExpectation{AccountID: r.AccountID, IssuerID: r.IssuerID, Stream: r.AuthorityStream, Incarnation: r.IssuerIncarnation, Authority: r.IssuerAuthorityEpoch, From: 1, Through: 3, Root: ReceiptChainRoot(Commitment{}, receipts)}
	return entries, expected, key
}

func TestReceiptChainSealedMembership(t *testing.T) {
	entries, expected, key := fixtureReceiptChain(t)
	if err := VerifyReceiptChain(entries, expected); err != nil {
		t.Fatal("complete receipt chain refused", err)
	}
	for _, selected := range [][]ReceiptChainEntry{entries[:2], {entries[0], entries[2]}, {entries[1], entries[0], entries[2]}, {entries[0], entries[0], entries[2]}} {
		if VerifyReceiptChain(selected, expected) == nil {
			t.Fatal("missing, reordered or duplicate receipt chain accepted")
		}
	}
	changed := append([]ReceiptChainEntry(nil), entries...)
	changed[1].Receipt.CanonicalBody, changed[1].Receipt.SignedBytes = nil, nil
	changed[1].Receipt.CausalResult = ObjectValue(Field{Name: "revision", Value: StringValue(RevisionID{8}.String())})
	var err error
	changed[1].Receipt, err = SignReceipt(changed[1].Receipt, key)
	if err != nil {
		t.Fatal("valid replacement signing failed")
	}
	if _, err := VerifyReceipt(changed[1].Receipt, changed[1].PublicKey, changed[1].Expectation); err != nil {
		t.Fatal("counterexample did not retain legitimate signature")
	}
	if VerifyReceiptChain(changed, expected) == nil {
		t.Fatal("same numeric ordinal with changed signed content matched sealed root")
	}
	for i := range entries {
		entries[i].Expectation.Trust.Retired = true
		entries[i].Expectation.Trust.SealedBodies = [][]byte{entries[i].Receipt.CanonicalBody}
	}
	if VerifyReceiptChain(entries, expected) != nil {
		t.Fatal("retired signer historical member refused")
	}
	changed[1].Expectation = entries[1].Expectation
	if _, err := VerifyReceipt(changed[1].Receipt, changed[1].PublicKey, changed[1].Expectation); err == nil {
		t.Fatal("retired signer minted nonmember at allowed ordinal")
	}
	wrong := expected
	wrong.Authority.Epoch++
	if VerifyReceiptChain(entries, wrong) == nil {
		t.Fatal("chain ignored independently expected authority epoch")
	}
}
