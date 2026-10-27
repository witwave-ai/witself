package syncmodel

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"strings"
	"testing"
)

func fixtureReceipt(t *testing.T) (Receipt, ed25519.PrivateKey, ReceiptExpectation) {
	t.Helper()
	e := fixtureEnvelope()
	commitment, err := InputCommitment(fixtureComparisonKeys{}, e.OriginalIncarnation.AccountID, "comparison-fixture", e)
	if err != nil {
		t.Fatal("fixture comparison", err)
	}
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{5}, ed25519.SeedSize))
	r := Receipt{Version: 1, AccountID: e.OriginalIncarnation.AccountID, Operation: e.ID, OriginalOrigin: e.Origin, OriginalIncarnation: e.OriginalIncarnation, OriginalCapture: e.Capture, ComparisonKeyID: "comparison-fixture", InputCommitment: commitment, IssuerID: "issuer-fixture", SigningKeyID: "signing-fixture", AuthorityStream: "account-fixture-stream", IssuerIncarnation: e.OriginalIncarnation, IssuerAuthorityEpoch: AuthorityEpoch{Scope: AuthorityAccount, Epoch: 1}, Acceptance: 1, Status: StatusAccepted, Reason: ReasonApplied, CausalResult: NullValue()}
	r, err = SignReceipt(r, key)
	if err != nil {
		t.Fatal("fixture signing", err)
	}
	expect := ReceiptExpectation{OriginalEnvelope: e, ComparisonKeyID: r.ComparisonKeyID, InputCommitment: commitment, Trust: ReceiptTrust{IssuerID: r.IssuerID, SigningKeyID: r.SigningKeyID, AuthorityStream: r.AuthorityStream, IssuerIncarnation: r.IssuerIncarnation, AuthorityEpoch: r.IssuerAuthorityEpoch}}
	return r, key, expect
}

func TestReceiptClosedStatusReasonPairs(t *testing.T) {
	pairs := map[ReceiptStatus][]ReceiptReason{StatusAccepted: {ReasonApplied}, StatusRejected: {ReasonInvalidEnvelope, ReasonUnauthorized, ReasonAuthorizationExpired, ReasonUnsupportedOperation, ReasonSchemaIncompatible, ReasonProtocolIncompatible, ReasonCapacityRefused, ReasonAccountClosed}, StatusConflict: {ReasonExpectedVersionMismatch, ReasonDependencyUnavailable, ReasonIdentityCollision, ReasonDeletionConflict, ReasonAuthorityEpochStale, ReasonIncarnationMismatch, ReasonOperationIDReuse}}
	count := 0
	for status, reasons := range pairs {
		for _, reason := range reasons {
			count++
			if err := ValidateReceiptPair(status, reason); err != nil {
				t.Fatal(err)
			}
			for other := range pairs {
				if other != status && ValidateReceiptPair(other, reason) == nil {
					t.Fatal("reason crossed terminal status")
				}
			}
		}
	}
	if count != 16 {
		t.Fatal("closed terminal pair count drifted")
	}
	for _, pair := range [][2]string{{"accepted", "unknown"}, {"pending", "applied"}, {"accepted", "recovery_complete"}, {"conflict", "evidence_conflict"}, {"rejected", "durability_publication_pending"}} {
		if ValidateReceiptPair(ReceiptStatus(pair[0]), ReceiptReason(pair[1])) == nil {
			t.Fatal("unknown or recovery pair accepted")
		}
	}
}

func TestReceiptExactSignedBytesAndPayloadFreeBasis(t *testing.T) {
	r, key, expect := fixtureReceipt(t)
	public := key.Public().(ed25519.PublicKey)
	result, err := VerifyReceipt(r, public, expect)
	if err != nil || !result.SignatureValid || result.CurrentAuthority {
		t.Fatal("receipt failed or granted authority")
	}
	stored, err := DecodeReceipt(r.CanonicalBody, r.SignedBytes)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored.CanonicalBody, r.CanonicalBody) || !bytes.Equal(stored.SignedBytes, r.SignedBytes) {
		t.Fatal("stored receipt changed bytes")
	}
	if _, err := VerifyReceipt(stored, public, expect); err != nil {
		t.Fatal(err)
	}
	if _, err := SignReceipt(stored, key); err == nil {
		t.Fatal("immutable receipt resigned")
	}
	if strings.Contains(string(r.CanonicalBody), "synthetic-private-input") {
		t.Fatal("receipt contains payload")
	}
	for _, value := range []CanonicalTypedValue{StringValue("private"), BinaryValue([]byte("private")), ObjectValue(Field{Name: "payload", Value: StringValue("private")}), ObjectValue(Field{Name: "revision", Value: StringValue("private")})} {
		bad := r
		bad.CausalResult = value
		if bad.Validate() == nil {
			t.Fatal("payload-bearing receipt basis accepted")
		}
	}
	good := r
	good.CausalResult = ObjectValue(Field{Name: "revision", Value: StringValue(RevisionID{4}.String())})
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := r
	bad.Status = StatusRejected
	bad.Reason = ReasonUnauthorized
	bad.ResultCommitment = &Commitment{}
	if bad.Validate() == nil {
		t.Fatal("refusal result materialization accepted")
	}
}

func TestCursorReceiptFieldMutation(t *testing.T) {
	r, key, expect := fixtureReceipt(t)
	public := key.Public().(ed25519.PublicKey)
	mutations := map[string]func(*Receipt){
		"version": func(r *Receipt) { r.Version = 2 }, "account": func(r *Receipt) { r.AccountID = "other" }, "operation": func(r *Receipt) { r.Operation[0]++ }, "origin": func(r *Receipt) { r.OriginalOrigin.OriginSequence++ }, "capture": func(r *Receipt) { r.OriginalCapture++ }, "original_incarnation": func(r *Receipt) { r.OriginalIncarnation.Restore[0]++ }, "comparison_key": func(r *Receipt) { r.ComparisonKeyID = "other" }, "commitment": func(r *Receipt) { r.InputCommitment[0]++ }, "issuer": func(r *Receipt) { r.IssuerID = "other" }, "signer": func(r *Receipt) { r.SigningKeyID = "other" }, "stream": func(r *Receipt) { r.AuthorityStream = "other" }, "issuer_incarnation": func(r *Receipt) { r.IssuerIncarnation.Restore[0]++ }, "authority_epoch": func(r *Receipt) { r.IssuerAuthorityEpoch.Epoch++ }, "ordinal": func(r *Receipt) { r.Acceptance++ }, "status": func(r *Receipt) { r.Status = StatusRejected; r.Reason = ReasonUnauthorized }, "reason": func(r *Receipt) { r.Reason = ReasonOperationIDReuse }, "causal_result": func(r *Receipt) {
			r.CausalResult = ObjectValue(Field{Name: "revision", Value: StringValue(RevisionID{8}.String())})
		}, "result_commitment": func(r *Receipt) { r.ResultCommitment = &Commitment{7} },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			bad := r
			mutate(&bad)
			if _, err := VerifyReceipt(bad, public, expect); err == nil {
				t.Fatal("mutated receipt verified")
			}
		})
	}
	bad := r
	bad.SignedBytes = append([]byte(nil), r.SignedBytes...)
	bad.SignedBytes[0] ^= 1
	if _, err := VerifyReceipt(bad, public, expect); err == nil {
		t.Fatal("mutated signature verified")
	}
	bad = r
	bad.CanonicalBody = append([]byte(nil), r.CanonicalBody...)
	bad.CanonicalBody = append(bad.CanonicalBody, ' ')
	if _, err := VerifyReceipt(bad, public, expect); err == nil {
		t.Fatal("mutated canonical bytes verified")
	}
	wrongPublic := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, ed25519.SeedSize)).Public().(ed25519.PublicKey)
	if _, err := VerifyReceipt(r, wrongPublic, expect); err == nil {
		t.Fatal("wrong trust key verified")
	}
	// Even a valid new signature cannot bind the receipt to a different immutable
	// submitted operation or independently selected trust scope.
	for _, mutate := range []func(*ReceiptExpectation){func(e *ReceiptExpectation) { e.OriginalEnvelope.ID[0]++ }, func(e *ReceiptExpectation) { e.OriginalEnvelope.Origin.OriginSequence++ }, func(e *ReceiptExpectation) { e.OriginalEnvelope.Capture++ }, func(e *ReceiptExpectation) { e.OriginalEnvelope.OriginalIncarnation.Restore[0]++ }, func(e *ReceiptExpectation) { e.InputCommitment[0]++ }, func(e *ReceiptExpectation) { e.ComparisonKeyID = "other" }, func(e *ReceiptExpectation) { e.Trust.IssuerID = "other" }, func(e *ReceiptExpectation) { e.Trust.SigningKeyID = "other" }, func(e *ReceiptExpectation) { e.Trust.AuthorityStream = "other" }, func(e *ReceiptExpectation) { e.Trust.IssuerIncarnation.Restore[0]++ }, func(e *ReceiptExpectation) { e.Trust.AuthorityEpoch.Epoch++ }} {
		bad := expect
		mutate(&bad)
		if _, err := VerifyReceipt(r, public, bad); err == nil {
			t.Fatal("wrong submitted operation or trust scope verified")
		}
	}
}

func TestReceiptConsistentFieldMutation(t *testing.T) {
	changeAccount := func(r *Receipt) {
		// The three account fields must agree for a structurally valid receipt.
		r.AccountID = "other-account"
		r.OriginalIncarnation.AccountID = r.AccountID
		r.IssuerIncarnation.AccountID = r.AccountID
	}
	graphAuthority := func(r *Receipt) {
		objectID := "graph-fixture"
		r.IssuerAuthorityEpoch = AuthorityEpoch{Scope: AuthorityGraph, ObjectID: &objectID, Epoch: 1}
	}
	mutations := []struct {
		name    string
		prepare func(*Receipt)
		mutate  func(*Receipt)
	}{
		{"account_id", nil, changeAccount},
		{"operation_id", nil, func(r *Receipt) { r.Operation[0]++ }},
		{"original_origin.installation_id", nil, func(r *Receipt) { r.OriginalOrigin.InstallationID = "other-installation" }},
		{"original_origin.replica_id", nil, func(r *Receipt) { r.OriginalOrigin.ReplicaID = "other-replica" }},
		{"original_origin.write_lineage_id", nil, func(r *Receipt) { r.OriginalOrigin.WriteLineageID = "other-lineage" }},
		{"original_origin.origin_sequence", nil, func(r *Receipt) { r.OriginalOrigin.OriginSequence++ }},
		{"original_incarnation.account_id", nil, changeAccount},
		{"original_incarnation.lifecycle_epoch", nil, func(r *Receipt) { r.OriginalIncarnation.Lifecycle++ }},
		{"original_incarnation.restore_marker", nil, func(r *Receipt) { r.OriginalIncarnation.Restore[0]++ }},
		{"original_capture", nil, func(r *Receipt) { r.OriginalCapture++ }},
		{"comparison_key_id", nil, func(r *Receipt) { r.ComparisonKeyID = "other-comparison" }},
		{"input_commitment", nil, func(r *Receipt) { r.InputCommitment[0]++ }},
		{"issuer_id", nil, func(r *Receipt) { r.IssuerID = "other-issuer" }},
		{"signing_key_id", nil, func(r *Receipt) { r.SigningKeyID = "other-signer" }},
		{"authority_stream", nil, func(r *Receipt) { r.AuthorityStream = "other-stream" }},
		{"issuer_incarnation.account_id", nil, changeAccount},
		{"issuer_incarnation.lifecycle_epoch", graphAuthority, func(r *Receipt) { r.IssuerIncarnation.Lifecycle++ }},
		{"issuer_incarnation.restore_marker", nil, func(r *Receipt) { r.IssuerIncarnation.Restore[0]++ }},
		// Graph scope requires an object; account scope forbids one. Keep both
		// fields valid when switching scope, and test object identity separately.
		{"issuer_authority_epoch.scope", nil, graphAuthority},
		{"issuer_authority_epoch.object_id", graphAuthority, func(r *Receipt) {
			objectID := "other-graph"
			r.IssuerAuthorityEpoch.ObjectID = &objectID
		}},
		{"issuer_authority_epoch.epoch", graphAuthority, func(r *Receipt) { r.IssuerAuthorityEpoch.Epoch++ }},
		{"acceptance_ordinal", nil, func(r *Receipt) { r.Acceptance++ }},
		// The status and reason form a closed pair. A status change must change
		// its reason, while a rejected baseline permits a reason-only change.
		{"status", nil, func(r *Receipt) { r.Status = StatusRejected; r.Reason = ReasonUnauthorized }},
		{"reason", func(r *Receipt) { r.Status = StatusRejected; r.Reason = ReasonUnauthorized }, func(r *Receipt) { r.Reason = ReasonAuthorizationExpired }},
		{"causal_result", nil, func(r *Receipt) {
			r.CausalResult = ObjectValue(Field{Name: "revision", Value: StringValue(RevisionID{8}.String())})
		}},
		{"result_commitment", nil, func(r *Receipt) { r.ResultCommitment = &Commitment{7} }},
	}
	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			original, key, expect := fixtureReceipt(t)
			if tc.prepare != nil {
				original.CanonicalBody, original.SignedBytes = nil, nil
				tc.prepare(&original)
				var err error
				original, err = SignReceipt(original, key)
				if err != nil {
					t.Fatal("prepared fixture signing failed", err)
				}
				expect.Trust.IssuerIncarnation = original.IssuerIncarnation
				expect.Trust.AuthorityEpoch = original.IssuerAuthorityEpoch
			}
			public := key.Public().(ed25519.PublicKey)
			if _, err := VerifyReceipt(original, public, expect); err != nil {
				t.Fatal("original receipt failed verification", err)
			}
			bad := original
			tc.mutate(&bad)
			if err := bad.Validate(); err != nil {
				t.Fatal("mutation was not structurally valid", err)
			}
			var err error
			bad.CanonicalBody, err = MarshalCanonical(bad)
			if err != nil {
				t.Fatal("mutation canonical encoding failed", err)
			}
			if bytes.Equal(bad.CanonicalBody, original.CanonicalBody) {
				t.Fatal("mutation did not change the canonical body")
			}
			if !bytes.Equal(bad.SignedBytes, original.SignedBytes) {
				t.Fatal("mutation changed the original signature")
			}
			// Bypass struct validation and operation/trust expectations as well:
			// the original signature itself must reject the consistent new body.
			if ed25519.Verify(public, receiptMessage(bad.CanonicalBody), bad.SignedBytes) {
				t.Fatal("original signature authenticated the mutated canonical body")
			}
			if _, err := VerifyReceipt(bad, public, expect); err == nil {
				t.Fatal("consistent mutated receipt verified")
			}
		})
	}
	// Version one is the only supported version; no different version can
	// satisfy Validate. Keep the fail-closed check and test its signature input
	// independently below, without weakening the production version guard.
	t.Run("version", func(t *testing.T) {
		r, key, expect := fixtureReceipt(t)
		r.Version = 2
		var err error
		r.CanonicalBody, err = MarshalCanonical(r)
		if err != nil {
			t.Fatal("version mutation encoding failed", err)
		}
		if r.Validate() == nil {
			t.Fatal("unsupported receipt version validated")
		}
		if ed25519.Verify(key.Public().(ed25519.PublicKey), receiptMessage(r.CanonicalBody), r.SignedBytes) {
			t.Fatal("original signature authenticated a changed version")
		}
		if _, err := VerifyReceipt(r, key.Public().(ed25519.PublicKey), expect); err == nil {
			t.Fatal("unsupported receipt version verified")
		}
	})
}

func TestReceiptSignatureInputBindsEveryCanonicalField(t *testing.T) {
	r, key, _ := fixtureReceipt(t)
	public := key.Public().(ed25519.PublicKey)
	if !ed25519.Verify(public, receiptMessage(r.CanonicalBody), r.SignedBytes) {
		t.Fatal("signature did not verify the exact canonical body")
	}
	// This check is independent of receipt decoding, validation, and external
	// expectations. Omitting any bound leaf must change the signed message,
	// including a null causal_result and the sole supported version value.
	paths := []string{
		"version", "account_id", "operation_id",
		"original_origin.installation_id", "original_origin.replica_id", "original_origin.write_lineage_id", "original_origin.origin_sequence",
		"original_incarnation.account_id", "original_incarnation.lifecycle_epoch", "original_incarnation.restore_marker",
		"original_capture", "comparison_key_id", "input_commitment", "issuer_id", "signing_key_id", "authority_stream",
		"issuer_incarnation.account_id", "issuer_incarnation.lifecycle_epoch", "issuer_incarnation.restore_marker",
		"issuer_authority_epoch.scope", "issuer_authority_epoch.object_id", "issuer_authority_epoch.epoch",
		"acceptance_ordinal", "status", "reason", "causal_result", "result_commitment",
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(r.CanonicalBody, &fields); err != nil {
				t.Fatal("fixture body decoding failed", err)
			}
			parts := strings.Split(path, ".")
			if len(parts) == 1 {
				if _, present := fields[path]; !present {
					t.Fatal("receipt leaf missing from fixture")
				}
				delete(fields, path)
			} else {
				var nested map[string]json.RawMessage
				if err := json.Unmarshal(fields[parts[0]], &nested); err != nil {
					t.Fatal("fixture nested body decoding failed", err)
				}
				if _, present := nested[parts[1]]; !present {
					t.Fatal("nested receipt leaf missing from fixture")
				}
				delete(nested, parts[1])
				var err error
				fields[parts[0]], err = json.Marshal(nested)
				if err != nil {
					t.Fatal("nested mutation encoding failed", err)
				}
			}
			body, err := json.Marshal(fields)
			if err != nil {
				t.Fatal("mutation encoding failed", err)
			}
			if ed25519.Verify(public, receiptMessage(body), r.SignedBytes) {
				t.Fatal("signature verified with a canonical receipt leaf removed")
			}
		})
	}
}

func TestRetiredSignerRequiresSealedContentMembership(t *testing.T) {
	r, key, expect := fixtureReceipt(t)
	public := key.Public().(ed25519.PublicKey)
	result, err := VerifyHistoricalReceipt(r, public, expect, [][]byte{r.CanonicalBody})
	if err != nil || !result.HistoricalOnly || result.CurrentAuthority {
		t.Fatal("retired signer did not remain historical")
	}
	if _, err := VerifyHistoricalReceipt(r, public, expect, nil); err == nil {
		t.Fatal("retired signer accepted without membership")
	}
	forged := r
	forged.CanonicalBody = nil
	forged.SignedBytes = nil
	forged.Acceptance = 1
	forged.CausalResult = ObjectValue(Field{Name: "revision", Value: StringValue(RevisionID{8}.String())})
	forged, err = SignReceipt(forged, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyHistoricalReceipt(forged, public, expect, [][]byte{r.CanonicalBody}); err == nil {
		t.Fatal("old key minted a nonmember under the same ordinal")
	}
}

func TestOperationReuseRefusalKeepsOriginalReceipt(t *testing.T) {
	original, key, expected := fixtureReceipt(t)
	body := append([]byte(nil), original.CanonicalBody...)
	signature := append([]byte(nil), original.SignedBytes...)
	changed := expected.OriginalEnvelope
	changed.Payload = StringValue("changed-input")
	commitment, err := InputCommitment(fixtureComparisonKeys{}, changed.OriginalIncarnation.AccountID, expected.ComparisonKeyID, changed)
	if err != nil {
		t.Fatal(err)
	}
	refusal := original
	refusal.CanonicalBody = nil
	refusal.SignedBytes = nil
	refusal.InputCommitment = commitment
	refusal.Acceptance = 2
	refusal.Status = StatusConflict
	refusal.Reason = ReasonOperationIDReuse
	refusal, err = SignReceipt(refusal, key)
	if err != nil {
		t.Fatal(err)
	}
	r := ReuseRefusal{AccountID: original.AccountID, Operation: original.Operation, ComparisonKeyID: original.ComparisonKeyID, ConflictingInput: commitment, Receipt: refusal}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	expected.OriginalEnvelope = changed
	expected.InputCommitment = commitment
	if _, err := VerifyReceipt(refusal, key.Public().(ed25519.PublicKey), expected); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, original.CanonicalBody) || !bytes.Equal(signature, original.SignedBytes) {
		t.Fatal("reuse changed canonical receipt")
	}
	r.ConflictingInput = original.InputCommitment
	if r.Validate() == nil {
		t.Fatal("refusal key did not bind conflicting input")
	}
}

func TestReceiptExpectedAcceptanceAndAuthorityCoordinates(t *testing.T) {
	r, key, expected := fixtureReceipt(t)
	ordinal, epoch := r.Acceptance, r.IssuerAuthorityEpoch
	expected.ExpectedAcceptance, expected.ExpectedIssuerAuthorityEpoch = &ordinal, &epoch
	public := key.Public().(ed25519.PublicKey)
	if _, err := VerifyReceipt(r, public, expected); err != nil {
		t.Fatal("matching expected coordinates refused")
	}
	for _, mutate := range []func(*Receipt){
		func(r *Receipt) { r.Acceptance++ },
		func(r *Receipt) { r.IssuerAuthorityEpoch.Epoch++; r.IssuerIncarnation.Lifecycle++ },
	} {
		changed := r
		changed.CanonicalBody, changed.SignedBytes = nil, nil
		mutate(&changed)
		changed, err := SignReceipt(changed, key)
		if err != nil {
			t.Fatal("legitimate issuer mutation was not structurally valid")
		}
		// Update key trust to the legitimate issuer's new current epoch. The
		// independent expected acceptance coordinates still name the original.
		expectation := expected
		expectation.Trust.IssuerIncarnation = changed.IssuerIncarnation
		expectation.Trust.AuthorityEpoch = changed.IssuerAuthorityEpoch
		if _, err := VerifyReceipt(changed, public, expectation); err == nil {
			t.Fatal("legitimate re-sign at different expected coordinates verified")
		}
		expectation.ExpectedAcceptance, expectation.ExpectedIssuerAuthorityEpoch = nil, nil
		if _, err := VerifyReceipt(changed, public, expectation); err != nil {
			t.Fatal("optional coordinate checks changed legacy trust verification")
		}
	}
	invalid := r
	invalid.SignedBytes = append([]byte(nil), r.SignedBytes...)
	invalid.SignedBytes[0] ^= 1
	wrongOrdinal := ordinal + 1
	expected.ExpectedAcceptance = &wrongOrdinal
	if _, err := VerifyReceipt(invalid, public, expected); err == nil || err.Error() != "receipt authentication failed" {
		t.Fatal("coordinate comparison ran before signature authentication")
	}
}
