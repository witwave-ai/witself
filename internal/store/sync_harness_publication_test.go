package store

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/witwave-ai/witself/internal/syncmodel"
)

func seedSyncHarnessSharedAdmission(t *testing.T, node *syncHarnessNode, keys *syncHarnessKeys) {
	t.Helper()
	e := syncHarnessEnvelope(syncHarnessAccount)
	if _, err := node.store.pool.Exec(context.Background(), `INSERT INTO h_incarnations(account_id,lifecycle_epoch,restore_marker,allocation_id,transition_id,authority_installation,state) VALUES($1,1,$2,$3,$4,'installation_R','published')`, syncHarnessAccount, e.OriginalIncarnation.Restore[:], bytes.Repeat([]byte{1}, 16), bytes.Repeat([]byte{2}, 16)); err != nil {
		t.Fatal("seed shared incarnation failed")
	}
	if _, err := node.store.pool.Exec(context.Background(), `INSERT INTO h_verifier_history(account_id,issuer_id,authority_stream,signing_key_id,public_key,valid_from) VALUES($1,'fixture_R','account','signing_old',$2,'2026-01-01')`, syncHarnessAccount, []byte(keys.signing.Public().(ed25519.PublicKey))); err != nil {
		t.Fatal("seed shared verifier failed")
	}
}

// Hash only public synthetic effect descriptors here. These are not a production
// sensitive-data commitment policy. Mutable visibility/pin labels are excluded.
func syncHarnessEffectCommitment(ctx context.Context, tx pgx.Tx, e syncmodel.OperationEnvelope) (syncmodel.Commitment, error) {
	var fields []string
	account := e.OriginalIncarnation.AccountID
	if e.Compound == nil {
		var object, owner, value, deps string
		if err := tx.QueryRow(ctx, `SELECT object_id,owner_node,value,dependencies::text FROM h_fixture_objects WHERE account_id=$1 AND object_id=$2`, account, e.Basis.ObjectID).Scan(&object, &owner, &value, &deps); err != nil {
			return syncmodel.Commitment{}, errors.New("read accepted fixture effect failed")
		}
		fields = []string{"fixture_object_v1", account, object, owner, value, deps}
	} else {
		var manifest []byte
		var links, messages string
		if err := tx.QueryRow(ctx, `SELECT manifest FROM h_compounds WHERE account_id=$1 AND group_id=$2`, account, e.Compound.GroupID).Scan(&manifest); err != nil {
			return syncmodel.Commitment{}, errors.New("read accepted compound manifest failed")
		}
		if err := tx.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(jsonb_build_array(group_id,request_id,message_id,link_kind) ORDER BY link_kind),'[]'::jsonb)::text FROM h_fixture_request_links WHERE account_id=$1 AND group_id=$2`, account, e.Compound.GroupID).Scan(&links); err != nil {
			return syncmodel.Commitment{}, errors.New("read accepted compound links failed")
		}
		if err := tx.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(jsonb_build_array(group_id,request_id,message_id) ORDER BY message_id),'[]'::jsonb)::text FROM h_fixture_messages WHERE account_id=$1 AND group_id=$2`, account, e.Compound.GroupID).Scan(&messages); err != nil {
			return syncmodel.Commitment{}, errors.New("read accepted compound messages failed")
		}
		fields = []string{"fixture_compound_v1", account, e.Compound.GroupID, base64.RawURLEncoding.EncodeToString(manifest), links, messages}
	}
	canonical, err := syncmodel.MarshalCanonical(fields)
	if err != nil {
		return syncmodel.Commitment{}, errors.New("encode accepted effect failed")
	}
	return syncmodel.Commitment(sha256.Sum256(canonical)), nil
}

func syncHarnessNextAcceptance(ctx context.Context, tx pgx.Tx, account string) (int64, error) {
	if _, err := tx.Exec(ctx, `INSERT INTO h_acceptance_counter VALUES($1,'account',0) ON CONFLICT DO NOTHING`, account); err != nil {
		return 0, errors.New("initialize acceptance counter failed")
	}
	var ordinal int64
	if err := tx.QueryRow(ctx, `UPDATE h_acceptance_counter SET last_accepted=last_accepted+1 WHERE account_id=$1 AND issuer_stream='account' RETURNING last_accepted`, account).Scan(&ordinal); err != nil {
		return 0, errors.New("allocate acceptance ordinal failed")
	}
	return ordinal, nil
}

func syncHarnessInsertAcceptedReceipt(ctx context.Context, tx pgx.Tx, keys *syncHarnessKeys, e syncmodel.OperationEnvelope, c syncmodel.Commitment, ordinal int64, refused bool) (syncmodel.Receipt, error) {
	r := syncmodel.Receipt{Version: 1, AccountID: e.OriginalIncarnation.AccountID, Operation: e.ID, OriginalOrigin: e.Origin, OriginalIncarnation: e.OriginalIncarnation, OriginalCapture: e.Capture, ComparisonKeyID: keys.current, InputCommitment: c, IssuerID: "fixture_R", SigningKeyID: "signing_old", AuthorityStream: "account", IssuerIncarnation: e.OriginalIncarnation, IssuerAuthorityEpoch: syncmodel.AuthorityEpoch{Scope: syncmodel.AuthorityAccount, Epoch: 1}, Acceptance: syncmodel.Ordinal(ordinal), Status: syncmodel.StatusAccepted, Reason: syncmodel.ReasonApplied, CausalResult: syncmodel.NullValue()}
	if refused {
		r.Status = syncmodel.StatusConflict
		r.Reason = syncmodel.ReasonExpectedVersionMismatch
	} else {
		result, err := syncHarnessEffectCommitment(ctx, tx, e)
		if err != nil {
			return r, err
		}
		r.ResultCommitment = &result
	}
	signed, err := syncmodel.SignReceipt(r, keys.signing)
	if err != nil {
		return r, errors.New("fixture signing failed")
	}
	// The B insertion helper deliberately predates effect commitments. C inserts
	// the complete immutable receipt row including the accepted result commitment.
	authority, err := syncmodel.MarshalCanonical(signed.IssuerAuthorityEpoch)
	if err != nil {
		return r, err
	}
	causal, err := syncmodel.MarshalCanonical(signed.CausalResult)
	if err != nil {
		return r, err
	}
	var result []byte
	if signed.ResultCommitment != nil {
		result = signed.ResultCommitment[:]
	}
	_, err = tx.Exec(ctx, `INSERT INTO h_receipts(account_id,operation_id,original_lifecycle_epoch,original_restore_marker,original_capture_epoch,original_installation,original_replica,original_write_lineage,original_sequence,issuer_id,issuer_lifecycle_epoch,issuer_restore_marker,authority_stream,issuer_authority_epoch,acceptance_ordinal,comparison_key_id,input_commitment,result_commitment,status,reason,causal_result,signing_key_id,canonical_body,signed_bytes,settled_at,retain_until) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,'2026-01-01','2027-01-01')`, signed.AccountID, signed.Operation[:], int64(signed.OriginalIncarnation.Lifecycle), signed.OriginalIncarnation.Restore[:], int64(signed.OriginalCapture), signed.OriginalOrigin.InstallationID, signed.OriginalOrigin.ReplicaID, signed.OriginalOrigin.WriteLineageID, int64(signed.OriginalOrigin.OriginSequence), signed.IssuerID, int64(signed.IssuerIncarnation.Lifecycle), signed.IssuerIncarnation.Restore[:], signed.AuthorityStream, string(authority), int64(signed.Acceptance), string(signed.ComparisonKeyID), signed.InputCommitment[:], result, string(signed.Status), string(signed.Reason), causal, signed.SigningKeyID, signed.CanonicalBody, signed.SignedBytes)
	if err != nil {
		return r, errors.New("store signed fixture receipt failed")
	}
	if _, err = tx.Exec(ctx, `UPDATE h_operations SET admission_state='terminal',receipt_ref=operation_id WHERE account_id=$1 AND operation_id=$2`, r.AccountID, e.ID[:]); err != nil {
		return r, errors.New("finalize fixture admission failed")
	}
	rows, err := tx.Query(ctx, `SELECT acceptance_ordinal,canonical_body,signed_bytes FROM h_receipts WHERE account_id=$1 AND authority_stream='account' ORDER BY acceptance_ordinal`, r.AccountID)
	if err != nil {
		return r, errors.New("build publication prefix failed")
	}
	var prefix []byte
	var next int64 = 1
	for rows.Next() {
		var n int64
		var body, sig []byte
		if err := rows.Scan(&n, &body, &sig); err != nil {
			rows.Close()
			return r, errors.New("read publication prefix failed")
		}
		if n != next {
			rows.Close()
			return r, errors.New("publication prefix incomplete")
		}
		next++
		prefix = binary.BigEndian.AppendUint64(prefix, uint64(n))
		prefix = binary.BigEndian.AppendUint32(prefix, uint32(len(body)))
		prefix = append(prefix, body...)
		prefix = append(prefix, sig...)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return r, errors.New("read publication prefix failed")
	}
	root := sha256.Sum256(prefix)
	proofKind := "receipt"
	if e.Kind == "fixture_control" {
		proofKind = "control"
	}
	if e.Kind == "fixture_grant" {
		proofKind = "grant"
	}
	if _, err = tx.Exec(ctx, `INSERT INTO h_publication_outbox(account_id,issuer_stream,through_ordinal,prefix_commitment,prefix_members,operation_id,proof_kind) VALUES($1,'account',$2,$3,$4,$5,$6)`, r.AccountID, ordinal, root[:], prefix, e.ID[:], proofKind); err != nil {
		return r, errors.New("store fixture outbox failed")
	}
	return signed, nil
}

// Single-node miniature apply. The logical operation is locked before equality
// access. Retry takes the old comparison key and returns exact old signed bytes.
func syncHarnessApply(ctx context.Context, node *syncHarnessNode, keys *syncHarnessKeys, e syncmodel.OperationEnvelope, refused bool, fault string) (syncmodel.Receipt, error) {
	empty := syncmodel.Receipt{}
	scope := syncHarnessScope(e.OriginalIncarnation.AccountID)
	// Fixture authentication supplies installation/replica/lineage authority;
	// the sequence is the submitted coordinate within that authorized lineage.
	scope.Origin.OriginSequence = e.Origin.OriginSequence
	if err := syncmodel.AuthorizeAdmission(e, scope, true); err != nil {
		return empty, errors.New("fixture authorization refused")
	}
	tx, err := node.store.pool.Begin(ctx)
	if err != nil {
		return empty, errors.New("begin fixture apply failed")
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(current_schema()||chr(31)||$1||chr(31)||encode($2::bytea,'hex'),0))`, e.OriginalIncarnation.AccountID, e.ID[:]); err != nil {
		return empty, errors.New("lock fixture apply failed")
	}
	var key string
	var old, body, sig []byte
	err = tx.QueryRow(ctx, `SELECT o.comparison_key_id,o.input_commitment,r.canonical_body,r.signed_bytes FROM h_operations o JOIN h_receipts r USING(account_id,operation_id) WHERE o.account_id=$1 AND o.operation_id=$2`, e.OriginalIncarnation.AccountID, e.ID[:]).Scan(&key, &old, &body, &sig)
	if err == nil {
		c, err := syncmodel.InputCommitment(keys, e.OriginalIncarnation.AccountID, syncmodel.ComparisonKeyID(key), e)
		if err != nil || !bytes.Equal(old, c[:]) {
			return empty, errors.New("fixture retry comparison refused")
		}
		r, err := syncmodel.DecodeReceipt(body, sig)
		if err != nil {
			return empty, errors.New("fixture stored receipt invalid")
		}
		return r, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return empty, errors.New("read fixture outcome failed")
	}
	if _, err = tx.Exec(ctx, `SELECT h_admit_capture($1)`, e.OriginalIncarnation.AccountID); err != nil {
		return empty, errors.New("capture admission refused")
	}
	c, err := syncHarnessInsertOperation(ctx, tx, keys, e)
	if err != nil {
		return empty, err
	}
	if fault == "after_admission" {
		return empty, errors.New("synthetic crash after admission")
	}
	if !refused {
		if _, err = tx.Exec(ctx, `INSERT INTO h_fixture_objects(account_id,object_id,owner_node,value) VALUES($1,$2,'R','synthetic')`, e.OriginalIncarnation.AccountID, e.Basis.ObjectID); err != nil {
			return empty, errors.New("miniature mutation failed")
		}
	}
	if fault == "after_domain" {
		return empty, errors.New("synthetic crash after domain")
	}
	_, err = syncHarnessSealCapture(ctx, tx, e.OriginalIncarnation.AccountID, e.Basis.ObjectID, e.ID[:])
	if err != nil {
		return empty, err
	}
	if fault == "after_bundle" {
		return empty, errors.New("synthetic crash after bundle")
	}
	if !refused {
		if err := syncHarnessRecordAction(ctx, tx, e.OriginalIncarnation.AccountID, e.ID[:], "fixture_mutation"); err != nil {
			return empty, err
		}
	}
	signingKeys := keys
	if fault == "signing" {
		copyKeys := *keys
		copyKeys.signing = nil
		signingKeys = &copyKeys
	}
	ordinal, err := syncHarnessNextAcceptance(ctx, tx, e.OriginalIncarnation.AccountID)
	if err != nil {
		return empty, err
	}
	r, err := syncHarnessInsertAcceptedReceipt(ctx, tx, signingKeys, e, c, ordinal, refused)
	if err != nil {
		return empty, err
	}
	if refused {
		// The immutable terminal receipt retains the original comparison floor;
		// refused payload work no longer occupies capture capacity. The governed
		// deletion trigger releases the reservation in this same transaction.
		if _, err := tx.Exec(ctx, `INSERT INTO h_payload_retirements(account_id,operation_id,retired_at) VALUES($1,$2,'2026-01-01')`, e.OriginalIncarnation.AccountID, e.ID[:]); err != nil {
			return empty, errors.New("record refused payload retirement failed")
		}
		if _, err := tx.Exec(ctx, `DELETE FROM h_operation_payloads WHERE account_id=$1 AND operation_id=$2`, e.OriginalIncarnation.AccountID, e.ID[:]); err != nil {
			return empty, errors.New("release refused capture capacity failed")
		}
	}
	if fault == "storage" {
		if _, err = tx.Exec(ctx, `INSERT INTO h_publication_outbox(account_id,issuer_stream,through_ordinal,prefix_commitment,prefix_members) VALUES($1,'account',0,$2,$3)`, e.OriginalIncarnation.AccountID, make([]byte, 32), []byte{1}); err == nil {
			return empty, errors.New("fault did not fail")
		}
		return empty, errors.New("synthetic outbox storage failure")
	}
	if fault == "before_commit" {
		return empty, errors.New("synthetic crash before commit")
	}
	if err = tx.Commit(ctx); err != nil {
		return empty, errors.New("commit atomic fixture outcome failed")
	}
	if fault == "after_commit" {
		return empty, errors.New("synthetic lost commit acknowledgement")
	}
	return r, nil
}

func TestSyncHarnessOperationStateCrashMatrix(t *testing.T) {
	top := newSyncHarnessTopology(t)
	node := top.nodes[2]
	keys := newSyncHarnessKeys()
	seedSyncHarnessAdmission(t, node, keys)
	seedSyncHarnessCapture(t, node)
	e := syncHarnessEnvelope(syncHarnessAccount)
	ctx := context.Background()
	for _, fault := range []string{"after_admission", "after_domain", "after_bundle", "signing", "storage", "before_commit"} {
		t.Run(fault, func(t *testing.T) {
			if _, err := syncHarnessApply(ctx, node, keys, e, false, fault); err == nil {
				t.Fatal("fault unexpectedly succeeded")
			}
			for _, table := range []string{"h_operations", "h_operation_payloads", "h_fixture_objects", "h_tx_admissions", "h_tx_touches", "h_bundles", "h_bundle_operations", "h_receipts", "h_logical_actions", "h_rollups", "h_publication_outbox", "h_acceptance_counter"} {
				if syncHarnessCount(t, node, table) != 0 {
					t.Errorf("partial atomic group survived in %s", table)
				}
			}
		})
	}
	if _, err := syncHarnessApply(ctx, node, keys, e, false, "after_commit"); err == nil {
		t.Fatal("lost acknowledgement was not injected")
	}
	r, err := syncHarnessApply(ctx, node, keys, e, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if r.ResultCommitment == nil || r.Status != syncmodel.StatusAccepted || r.Acceptance != 1 {
		t.Fatal("accepted outcome missing effect commitment")
	}
	// Independent expected synthetic materialization: a wrong value, owner,
	// object identity or dependency descriptor changes the result commitment.
	expectedEffect, err := syncmodel.MarshalCanonical([]string{"fixture_object_v1", syncHarnessAccount, e.Basis.ObjectID, "R", "synthetic", "[]"})
	if err != nil {
		t.Fatal("encode expected effect failed")
	}
	expectedCommitment := syncmodel.Commitment(sha256.Sum256(expectedEffect))
	if *r.ResultCommitment != expectedCommitment {
		t.Fatal("receipt does not bind actual accepted effect")
	}
	for _, changed := range []string{"changed value", "changed dependencies"} {
		wrong, err := syncmodel.MarshalCanonical([]string{"fixture_object_v1", syncHarnessAccount, e.Basis.ObjectID, "R", changed, "[]"})
		if err != nil {
			t.Fatal("encode altered effect failed")
		}
		if *r.ResultCommitment == syncmodel.Commitment(sha256.Sum256(wrong)) {
			t.Fatal("result commitment accepted altered effect")
		}
	}

	// Removing signing capability proves retry cannot call the signer again.
	keys.signing = nil
	retry, err := syncHarnessApply(ctx, node, keys, e, false, "")
	if err != nil {
		t.Fatal("retry required signing")
	}
	syncHarnessSameReceipt(t, r, retry)
	for _, table := range []string{"h_operations", "h_fixture_objects", "h_tx_admissions", "h_bundles", "h_receipts", "h_logical_actions", "h_publication_outbox"} {
		if syncHarnessCount(t, node, table) != 1 {
			t.Errorf("atomic accepted group incorrect in %s", table)
		}
	}
	if syncHarnessCount(t, node, "h_rollups") != 2 {
		t.Fatal("accepted action missing hour/day aggregate")
	}
	if !t.Failed() {
		t.Log("POSTGRES_PASS: before-commit fault matrix rolls back mutation/journal/admission/receipt/action/outbox; post-commit retry returns original bytes without signer")
	}
}

func TestSyncHarnessReceiptCommitSignatureRetry(t *testing.T) {
	top := newSyncHarnessTopology(t)
	node := top.nodes[2]
	keys := newSyncHarnessKeys()
	seedSyncHarnessAdmission(t, node, keys)
	seedSyncHarnessCapture(t, node)
	e := syncHarnessEnvelope(syncHarnessAccount)
	r, err := syncHarnessApply(context.Background(), node, keys, e, true, "")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != syncmodel.StatusConflict || r.ResultCommitment != nil {
		t.Fatal("refusal outcome invalid")
	}
	keys.signing = nil
	again, err := syncHarnessApply(context.Background(), node, keys, e, true, "")
	if err != nil {
		t.Fatal(err)
	}
	syncHarnessSameReceipt(t, r, again)
	if syncHarnessCount(t, node, "h_fixture_objects") != 0 || syncHarnessCount(t, node, "h_logical_actions") != 0 || syncHarnessCount(t, node, "h_receipts") != 1 {
		t.Fatal("refusal mutated requested domain or accounting")
	}
	if !t.Failed() {
		t.Log("POSTGRES_PASS: immutable refusal commits without domain mutation; retry returns original signature without signing capability")
	}
}

func TestSyncHarnessRefusalReleasesCaptureCapacity(t *testing.T) {
	top := newSyncHarnessTopology(t)
	node := top.nodes[2]
	keys := newSyncHarnessKeys()
	seedSyncHarnessAdmission(t, node, keys)
	seedSyncHarnessCapture(t, node)
	ctx := context.Background()
	e := syncHarnessEnvelope(syncHarnessAccount)
	checkBudget := func(t *testing.T, receipts, retirements int) {
		t.Helper()
		var pending, retained, payloads, receiptRows, retirementRows, activeReservations int
		if err := node.store.pool.QueryRow(ctx, `SELECT
 (SELECT COALESCE(sum(pending_operations),0) FROM h_resource_budgets WHERE account_id=$1),
 (SELECT COALESCE(sum(retained_bytes),0) FROM h_resource_budgets WHERE account_id=$1),
 (SELECT count(*) FROM h_operation_payloads WHERE account_id=$1),
 (SELECT count(*) FROM h_receipts WHERE account_id=$1),
 (SELECT count(*) FROM h_payload_retirements WHERE account_id=$1),
 (SELECT count(*) FROM h_resource_reservations WHERE account_id=$1 AND (pending OR envelope_bytes<>0 OR fragment_bytes<>0))`, syncHarnessAccount).Scan(&pending, &retained, &payloads, &receiptRows, &retirementRows, &activeReservations); err != nil {
			t.Fatal("inspect refused capture budget failed")
		}
		if pending != 0 || retained != 0 || payloads != 0 || receiptRows != receipts || retirementRows != retirements || activeReservations != 0 {
			t.Fatal("refusal or rollback retained capture capacity or changed outcome evidence")
		}
	}
	for _, fault := range []string{"after_admission", "after_domain", "after_bundle", "signing", "storage", "before_commit"} {
		t.Run(fault, func(t *testing.T) {
			if _, err := syncHarnessApply(ctx, node, keys, e, true, fault); err == nil {
				t.Fatal("refusal crash did not fail")
			}
			checkBudget(t, 0, 0)
			if syncHarnessCount(t, node, "h_operations") != 0 || syncHarnessCount(t, node, "h_resource_reservations") != 0 {
				t.Fatal("refusal rollback retained admission or capacity reservation")
			}
		})
	}
	original, err := syncHarnessApply(ctx, node, keys, e, true, "")
	if err != nil || original.Status != syncmodel.StatusConflict {
		t.Fatal("commit fixture refusal failed")
	}
	checkBudget(t, 1, 1)
	keys.signing = nil
	retry, err := syncHarnessApply(ctx, node, keys, e, true, "")
	if err != nil {
		t.Fatal("retry of retired refused payload failed")
	}
	syncHarnessSameReceipt(t, original, retry)
	checkBudget(t, 1, 1)
	if !t.Failed() {
		t.Log("POSTGRES_PASS: committed refusal releases pending count and retained bytes with governed payload retirement; crash matrix rolls back reservations and retirement, retry preserves exact receipt without capacity reservation")
	}
}

func TestSyncHarnessRefusalReservationAccountIsolation(t *testing.T) {
	top := newSyncHarnessTopology(t)
	node := top.nodes[2]
	keys := newSyncHarnessKeys()
	seedSyncHarnessAdmission(t, node, keys)
	seedSyncHarnessCapture(t, node)
	ctx := context.Background()
	primary := syncHarnessEnvelope(syncHarnessAccount)
	adversary := syncHarnessEnvelope("h_adversary_R")
	adversary.ID = primary.ID
	adversary.Payload = syncmodel.StringValue("adversary retained payload")
	// This helper calls the real insertion adapter for both accounts, reserving
	// their own capacity and retaining their own complete semantic envelopes.
	syncHarnessSeedCaptureOperations(ctx, t, node, keys, primary, adversary)

	type reservationState struct {
		pendingOperations, retainedBytes, envelopeBytes, fragmentBytes int
		pending                                                        bool
		payload                                                        []byte
	}
	readState := func(account string) reservationState {
		t.Helper()
		var state reservationState
		if err := node.store.pool.QueryRow(ctx, `SELECT b.pending_operations,b.retained_bytes,r.envelope_bytes,r.fragment_bytes,r.pending,p.canonical_semantic_envelope
 FROM h_resource_budgets b JOIN h_resource_reservations r USING(account_id)
 LEFT JOIN h_operation_payloads p USING(account_id,operation_id)
 WHERE r.account_id=$1 AND r.operation_id=$2`, account, primary.ID[:]).Scan(&state.pendingOperations, &state.retainedBytes, &state.envelopeBytes, &state.fragmentBytes, &state.pending, &state.payload); err != nil {
			t.Fatal("inspect same-ID account reservation failed")
		}
		return state
	}
	assertSameState := func(scope string, want, got reservationState) {
		t.Helper()
		if got.pendingOperations != want.pendingOperations || got.retainedBytes != want.retainedBytes ||
			got.envelopeBytes != want.envelopeBytes || got.fragmentBytes != want.fragmentBytes ||
			got.pending != want.pending || !bytes.Equal(got.payload, want.payload) {
			t.Fatalf("%s changed capacity reservation or retained payload", scope)
		}
	}
	primaryBefore, adversaryBefore := readState(syncHarnessAccount), readState(adversary.OriginalIncarnation.AccountID)
	for _, fixture := range []struct {
		envelope syncmodel.OperationEnvelope
		state    reservationState
	}{{primary, primaryBefore}, {adversary, adversaryBefore}} {
		canonical, err := fixture.envelope.CanonicalBytes()
		if err != nil || fixture.state.pendingOperations != 1 || fixture.state.retainedBytes != len(canonical) ||
			fixture.state.envelopeBytes != len(canonical) || fixture.state.fragmentBytes != 0 ||
			!fixture.state.pending || !bytes.Equal(fixture.state.payload, canonical) {
			t.Fatal("same-ID account fixture did not retain its own envelope and reservation")
		}
	}
	assertPrimaryEvidence := func(want int) {
		t.Helper()
		var receipts, retirements, objects, actions int
		if err := node.store.pool.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM h_receipts WHERE account_id=$1),
 (SELECT count(*) FROM h_payload_retirements WHERE account_id=$1),
 (SELECT count(*) FROM h_fixture_objects WHERE account_id=$1),
 (SELECT count(*) FROM h_logical_actions WHERE account_id=$1)`, syncHarnessAccount).Scan(&receipts, &retirements, &objects, &actions); err != nil {
			t.Fatal("inspect same-ID refusal evidence failed")
		}
		if receipts != want || retirements != want || objects != 0 || actions != 0 {
			t.Fatal("same-ID refusal changed outcome evidence or requested domain")
		}
	}

	// The fault occurs after governed payload deletion and capacity release, so
	// rollback must restore the primary reservation and preserve the adversary.
	if _, err := syncHarnessApply(ctx, node, keys, primary, true, "before_commit"); err == nil {
		t.Fatal("same-ID refusal rollback fault did not fail")
	}
	assertSameState("rolled-back primary refusal", primaryBefore, readState(syncHarnessAccount))
	assertSameState("adversary after rollback", adversaryBefore, readState(adversary.OriginalIncarnation.AccountID))
	assertPrimaryEvidence(0)

	original, err := syncHarnessApply(ctx, node, keys, primary, true, "")
	if err != nil || original.Status != syncmodel.StatusConflict || original.Reason != syncmodel.ReasonExpectedVersionMismatch {
		t.Fatal("same-ID primary refusal did not commit")
	}
	assertSameState("committed primary refusal", reservationState{}, readState(syncHarnessAccount))
	assertSameState("adversary after primary refusal", adversaryBefore, readState(adversary.OriginalIncarnation.AccountID))
	assertPrimaryEvidence(1)

	keys.signing = nil
	retry, err := syncHarnessApply(ctx, node, keys, primary, true, "")
	if err != nil {
		t.Fatal("same-ID refusal retry required signing or retired payload")
	}
	syncHarnessSameReceipt(t, original, retry)
	assertSameState("retried primary refusal", reservationState{}, readState(syncHarnessAccount))
	assertSameState("adversary after refusal retry", adversaryBefore, readState(adversary.OriginalIncarnation.AccountID))
	assertPrimaryEvidence(1)
	if !t.Failed() {
		t.Log("POSTGRES_PASS: same-ID other-account payload and all reservation fields survive primary refusal, exact retry and rollback; only the committed primary refusal releases capacity")
	}
}

func TestSyncHarnessRetirementReleasesCaptureCapacity(t *testing.T) {
	top := newSyncHarnessTopology(t)
	node := top.nodes[2]
	keys := newSyncHarnessKeys()
	seedSyncHarnessAdmission(t, node, keys)
	seedSyncHarnessCapture(t, node)
	ctx := context.Background()
	e := syncHarnessEnvelope(syncHarnessAccount)
	canonical, err := e.CanonicalBytes()
	if err != nil {
		t.Fatal("encode retirement capacity fixture failed")
	}
	original, err := syncHarnessApply(ctx, node, keys, e, false, "")
	if err != nil || original.Status != syncmodel.StatusAccepted {
		t.Fatal("commit accepted retirement fixture failed")
	}
	syncHarnessAssertResourceUsage(t, node, syncHarnessAccount, 1, len(canonical))
	shortFloor, err := node.store.pool.Begin(ctx)
	if err != nil {
		t.Fatal("begin insufficient retirement capacity floor failed")
	}
	defer func() { _ = shortFloor.Rollback(context.Background()) }()
	if _, err := shortFloor.Exec(ctx, `INSERT INTO h_payload_retirements(account_id,operation_id,retired_at) VALUES($1,$2,'2026-12-31')`, syncHarnessAccount, e.ID[:]); err != nil {
		t.Fatal("stage insufficient retirement capacity floor failed")
	}
	if _, err := shortFloor.Exec(ctx, `DELETE FROM h_operation_payloads WHERE account_id=$1 AND operation_id=$2`, syncHarnessAccount, e.ID[:]); err == nil {
		t.Fatal("retirement below commitment floor released capture capacity")
	}
	if err := shortFloor.Rollback(ctx); err != nil {
		t.Fatal("rollback insufficient retirement capacity floor failed")
	}
	syncHarnessAssertResourceUsage(t, node, syncHarnessAccount, 1, len(canonical))
	if syncHarnessCount(t, node, "h_payload_retirements") != 0 {
		t.Fatal("failed retirement retained authorization")
	}
	tx, err := node.store.pool.Begin(ctx)
	if err != nil {
		t.Fatal("begin governed capacity retirement failed")
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `INSERT INTO h_payload_retirements(account_id,operation_id,retired_at) VALUES($1,$2,'2026-01-02')`, syncHarnessAccount, e.ID[:]); err != nil {
		t.Fatal("authorize governed capacity retirement failed")
	}
	if _, err := tx.Exec(ctx, `DELETE FROM h_operation_payloads WHERE account_id=$1 AND operation_id=$2`, syncHarnessAccount, e.ID[:]); err != nil {
		t.Fatal("retire accepted capture payload failed")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal("commit governed capacity retirement failed")
	}
	syncHarnessAssertResourceUsage(t, node, syncHarnessAccount, 0, 0)
	keys.current = "comparison_new"
	keys.signing = nil
	retry, err := syncHarnessApply(ctx, node, keys, e, false, "")
	if err != nil {
		t.Fatal("retired accepted retry required payload, current key or signing")
	}
	syncHarnessSameReceipt(t, original, retry)
	syncHarnessAssertResourceUsage(t, node, syncHarnessAccount, 0, 0)
	var reserved bool
	if err := node.store.pool.QueryRow(ctx, `SELECT pending OR envelope_bytes<>0 OR fragment_bytes<>0 FROM h_resource_reservations WHERE account_id=$1 AND operation_id=$2`, syncHarnessAccount, e.ID[:]).Scan(&reserved); err != nil || reserved {
		t.Fatal("retired accepted retry reactivated reservation")
	}
	for _, table := range []string{"h_operations", "h_receipts", "h_logical_actions", "h_resource_reservations", "h_payload_retirements"} {
		if syncHarnessCount(t, node, table) != 1 {
			t.Fatalf("retired accepted retry changed immutable evidence in %s", table)
		}
	}
	if !t.Failed() {
		t.Log("POSTGRES_PASS: insufficient retirement floor preserves pending count and envelope bytes; governed accepted retirement releases both; old-key retry after rotation without signer preserves exact receipt and does not reserve or restore payload")
	}
}

func syncHarnessPrepareWitness(t *testing.T, top *syncHarnessTopology) {
	t.Helper()
	if _, err := top.witnessDB.Exec(`ALTER TABLE h_witness_prefixes ADD COLUMN root bytea NOT NULL CHECK(octet_length(root)=32), ADD COLUMN members bytea NOT NULL, ADD COLUMN cas_revision bigint NOT NULL CHECK(cas_revision>0)`); err != nil {
		t.Fatal("prepare witness prefix proof failed")
	}
}

func syncHarnessAuthenticatePrefix(prefix []byte, through int64, public ed25519.PublicKey) bool {
	remaining := prefix
	for ordinal := int64(1); ordinal <= through; ordinal++ {
		if len(remaining) < 12 || binary.BigEndian.Uint64(remaining[:8]) != uint64(ordinal) {
			return false
		}
		size := int(binary.BigEndian.Uint32(remaining[8:12]))
		remaining = remaining[12:]
		if size < 1 || len(remaining) < size+ed25519.SignatureSize {
			return false
		}
		r, err := syncmodel.DecodeReceipt(remaining[:size], remaining[size:size+ed25519.SignatureSize])
		if err != nil || r.Acceptance != syncmodel.Ordinal(ordinal) || r.AccountID != syncHarnessAccount || r.AuthorityStream != "account" || r.IssuerID != "fixture_R" {
			return false
		}
		// W authenticates issuer membership; operation-input authorization remains
		// the admission adapter's responsibility, not a witness side effect.
		expectedEnvelope := syncHarnessEnvelope(syncHarnessAccount)
		expectedEnvelope.ID = r.Operation
		expectedEnvelope.Origin = r.OriginalOrigin
		expectedEnvelope.OriginalIncarnation = r.OriginalIncarnation
		expectedEnvelope.Capture = r.OriginalCapture
		_, verifyErr := syncmodel.VerifyReceipt(r, public, syncmodel.ReceiptExpectation{
			OriginalEnvelope: expectedEnvelope, ComparisonKeyID: r.ComparisonKeyID, InputCommitment: r.InputCommitment,
			Trust: syncmodel.ReceiptTrust{IssuerID: "fixture_R", SigningKeyID: "signing_old", AuthorityStream: "account", IssuerIncarnation: syncHarnessEnvelope(syncHarnessAccount).OriginalIncarnation, AuthorityEpoch: syncmodel.AuthorityEpoch{Scope: syncmodel.AuthorityAccount, Epoch: 1}},
		})
		if verifyErr != nil {
			return false
		}
		remaining = remaining[size+ed25519.SignatureSize:]
	}
	return len(remaining) == 0
}

// W receives an authenticated complete prefix, not an unauthenticated numerical
// maximum. Its CAS revision prevents replacing equal-frontier conflicting data.
func syncHarnessPin(ctx context.Context, top *syncHarnessTopology, node *syncHarnessNode, through int64, public ed25519.PublicKey, dropACK bool) (string, error) {
	var root, members []byte
	if err := node.store.pool.QueryRow(ctx, `SELECT prefix_commitment,prefix_members FROM h_publication_outbox WHERE account_id=$1 AND issuer_stream='account' AND through_ordinal=$2`, syncHarnessAccount, through).Scan(&root, &members); err != nil {
		return "", errors.New("read pending publication failed")
	}
	want := sha256.Sum256(members)
	if !bytes.Equal(root, want[:]) || !syncHarnessAuthenticatePrefix(members, through, public) {
		return "evidence_conflict", nil
	}
	tx, err := top.witnessDB.BeginTx(ctx, nil)
	if err != nil {
		return "", errors.New("begin witness failed")
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(current_schema()||chr(31)||$1||chr(31)||'account',0))`, syncHarnessAccount); err != nil {
		return "", errors.New("lock witness failed")
	}
	var oldOrdinal, revision int64
	var oldRoot, oldMembers []byte
	err = tx.QueryRowContext(ctx, `SELECT ordinal,root,members,cas_revision FROM h_witness_prefixes WHERE account_id=$1 AND issuer_stream='account' FOR UPDATE`, syncHarnessAccount).Scan(&oldOrdinal, &oldRoot, &oldMembers, &revision)
	if err == nil {
		oldDigest := sha256.Sum256(oldMembers)
		if !bytes.Equal(oldRoot, oldDigest[:]) || !syncHarnessAuthenticatePrefix(oldMembers, oldOrdinal, public) {
			return "evidence_conflict", nil
		}
	}
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err = tx.ExecContext(ctx, `INSERT INTO h_witness_prefixes VALUES($1,'account',$2,$3,$4,1)`, syncHarnessAccount, through, root, members); err != nil {
			return "", errors.New("initialize witness failed")
		}
	case err != nil:
		return "", errors.New("read witness failed")
	case oldOrdinal > through:
		if !bytes.HasPrefix(oldMembers, members) {
			return "evidence_conflict", nil
		}
		var localFrontier int64
		var localRoot, localMembers []byte
		if err := node.store.pool.QueryRow(ctx, `SELECT through_ordinal,prefix_commitment,prefix_members FROM h_publication_outbox WHERE account_id=$1 AND issuer_stream='account' ORDER BY through_ordinal DESC LIMIT 1`, syncHarnessAccount).Scan(&localFrontier, &localRoot, &localMembers); err != nil {
			return "", errors.New("read local issuer frontier failed")
		}
		localDigest := sha256.Sum256(localMembers)
		if !bytes.Equal(localRoot, localDigest[:]) || !syncHarnessAuthenticatePrefix(localMembers, localFrontier, public) {
			return "evidence_conflict", nil
		}
		if oldOrdinal > localFrontier {
			if !bytes.HasPrefix(oldMembers, localMembers) {
				return "evidence_conflict", nil
			}
			return "history_gap_unrecoverable", nil
		}
		if !bytes.HasPrefix(localMembers, oldMembers) {
			return "evidence_conflict", nil
		}
		// Older ACK retry under the same authenticated newer prefix is a member,
		// not a history gap. It may settle its existing outbox entry unchanged.
	case oldOrdinal == through && !bytes.Equal(oldRoot, root):
		return "evidence_conflict", nil
	case oldOrdinal < through:
		if !bytes.HasPrefix(members, oldMembers) {
			return "evidence_conflict", nil
		}
		result, err := tx.ExecContext(ctx, `UPDATE h_witness_prefixes SET ordinal=$1,root=$2,members=$3,cas_revision=cas_revision+1 WHERE account_id=$4 AND issuer_stream='account' AND cas_revision=$5`, through, root, members, syncHarnessAccount, revision)
		if err != nil {
			return "", errors.New("witness CAS failed")
		}
		n, err := result.RowsAffected()
		if err != nil || n != 1 {
			return "", errors.New("witness CAS lost")
		}
	}
	if err = tx.Commit(); err != nil {
		return "", errors.New("commit witness failed")
	}
	if dropACK {
		return "durability_publication_pending", nil
	}
	if _, err = node.store.pool.Exec(ctx, `UPDATE h_publication_outbox SET state='pinned',witness_proof_ref=$3 WHERE account_id=$1 AND issuer_stream='account' AND through_ordinal<=$2 AND state='pending'`, syncHarnessAccount, through, fmt.Sprintf("witness_prefix_%d", through)); err != nil {
		return "", errors.New("persist pin acknowledgement failed")
	}
	return "published", nil
}

type syncHarnessPublicationView struct {
	State         string
	CursorThrough int64
	Executable    bool
}

func syncHarnessReadPublication(ctx context.Context, node *syncHarnessNode, kind string) (syncHarnessPublicationView, error) {
	var view syncHarnessPublicationView
	switch kind {
	case "list", "show", "checkpoint", "snapshot", "cursor", "control", "grant":
	default:
		return view, errors.New("unknown fixture projection")
	}
	var pending bool
	if err := node.store.pool.QueryRow(ctx, `SELECT COALESCE(bool_or(state='pending'),false),COALESCE(max(through_ordinal) FILTER (WHERE state='pinned'),0),COALESCE(bool_or(proof_kind=$2 AND state='pinned'),false) AND NOT COALESCE(bool_or(proof_kind=$2 AND state='pending'),false) FROM h_publication_outbox WHERE account_id=$1`, syncHarnessAccount, kind).Scan(&pending, &view.CursorThrough, &view.Executable); err != nil {
		return view, errors.New("read publication projection failed")
	}
	view.State = "published"
	if pending {
		view.State = "durability_publication_pending"
	}
	return view, nil
}

func syncHarnessPublicationProjection(ctx context.Context, node *syncHarnessNode, kind string) (string, error) {
	view, err := syncHarnessReadPublication(ctx, node, kind)
	return view.State, err
}

func TestSyncHarnessPublicationOutboxWitnessBoundary(t *testing.T) {
	top := newSyncHarnessTopology(t)
	node := top.nodes[2]
	keys := newSyncHarnessKeys()
	public := keys.signing.Public().(ed25519.PublicKey)
	seedSyncHarnessAdmission(t, node, keys)
	seedSyncHarnessCapture(t, node)
	syncHarnessPrepareWitness(t, top)
	ctx := context.Background()
	e := syncHarnessEnvelope(syncHarnessAccount)
	r, err := syncHarnessApply(ctx, node, keys, e, false, "")
	if err != nil {
		t.Fatal(err)
	}
	check := func(want string) {
		t.Helper()
		for _, kind := range []string{"list", "show", "checkpoint", "snapshot", "cursor", "control", "grant"} {
			state, err := syncHarnessPublicationProjection(ctx, node, kind)
			if err != nil || state != want {
				t.Errorf("%s projection did not preserve %s", kind, want)
			}
		}
	}
	check("durability_publication_pending")
	for _, kind := range []string{"checkpoint", "snapshot", "cursor", "control", "grant"} {
		view, err := syncHarnessReadPublication(ctx, node, kind)
		if err != nil || view.CursorThrough != 0 || view.Executable {
			t.Fatal("unpinned publication advanced cursor or execution")
		}
	}
	state, err := syncHarnessPin(ctx, top, node, 1, public, true)
	if err != nil || state != "durability_publication_pending" {
		t.Fatal("lost witness ACK did not remain pending")
	}
	check("durability_publication_pending")
	again, err := syncHarnessApply(ctx, node, keys, e, false, "")
	if err != nil {
		t.Fatal(err)
	}
	syncHarnessSameReceipt(t, r, again)
	// Leave ACK(1) dropped. Pinning the later complete prefix must make an
	// eventual ACK(1) retry idempotent rather than falsely report lost history.
	second := e
	second.ID[0] = 12
	second.Origin.OriginSequence = 2
	second.Basis.ObjectID = "second_object"
	second.Kind = "fixture_control"
	if _, err := syncHarnessApply(ctx, node, keys, second, false, ""); err != nil {
		t.Fatal(err)
	}
	check("durability_publication_pending")
	if view, err := syncHarnessReadPublication(ctx, node, "control"); err != nil || view.Executable || view.State != "durability_publication_pending" {
		t.Fatal("unpinned control proof executable")
	}
	if state, err := syncHarnessPin(ctx, top, node, 2, public, true); err != nil || state != "durability_publication_pending" {
		t.Fatal("second lost witness ACK did not remain pending")
	}
	check("durability_publication_pending")
	if view, err := syncHarnessReadPublication(ctx, node, "control"); err != nil || view.CursorThrough != 0 || view.Executable {
		t.Fatal("lost newer ACK settled pending local projection")
	}
	if state, err := syncHarnessPin(ctx, top, node, 1, public, false); err != nil || state != "published" {
		t.Fatal("older ACK under newer authenticated prefix was not idempotent")
	}
	check("durability_publication_pending")
	if view, err := syncHarnessReadPublication(ctx, node, "control"); err != nil || view.CursorThrough != 1 || view.Executable {
		t.Fatal("older ACK settled newer pending proof")
	}
	again, err = syncHarnessApply(ctx, node, keys, e, false, "")
	if err != nil {
		t.Fatal("older receipt retry under newer witness failed")
	}
	syncHarnessSameReceipt(t, r, again)
	if state, err := syncHarnessPin(ctx, top, node, 2, public, false); err != nil || state != "published" {
		t.Fatal("newer witness ACK retry failed")
	}
	check("published")
	for i, kind := range []string{"control", "grant"} {
		if kind == "grant" {
			third := e
			third.ID[0] = 13
			third.Origin.OriginSequence = 3
			third.Basis.ObjectID = "grant_object"
			third.Kind = "fixture_grant"
			if _, err := syncHarnessApply(ctx, node, keys, third, false, ""); err != nil {
				t.Fatal(err)
			}
		}
		// Control already pinned above; grant is still pending.
		view, err := syncHarnessReadPublication(ctx, node, kind)
		if err != nil || view.Executable != (kind == "control") {
			t.Fatal("proof execution crossed witness boundary")
		}
		if kind == "grant" {
			if state, err := syncHarnessPin(ctx, top, node, int64(i+2), public, false); err != nil || state != "published" {
				t.Fatal("grant pin failed")
			}
			view, err = syncHarnessReadPublication(ctx, node, kind)
			if err != nil || !view.Executable {
				t.Fatal("pinned grant did not publish")
			}
		}
		var count int
		if err := node.store.pool.QueryRow(ctx, `SELECT count(*) FROM h_publication_outbox WHERE proof_kind=$1`, kind).Scan(&count); err != nil || count != 1 {
			t.Fatal("control/grant proof not durably represented")
		}
	}
	if state, err := syncHarnessPin(ctx, top, node, 1, public, false); err != nil || state != "published" {
		t.Fatal("older ACK under newer authenticated prefix was not idempotent")
	}
	// A second schema models restoration to the first committed issuer member.
	// W's valid third member is then genuinely ahead of its local frontier.
	restored := top.nodes[0]
	seedSyncHarnessSharedAdmission(t, restored, keys)
	seedSyncHarnessCapture(t, restored)
	if _, err := syncHarnessApply(ctx, restored, keys, e, false, ""); err != nil {
		t.Fatal("prepare older restored issuer failed")
	}
	if state, err := syncHarnessPin(ctx, top, restored, 1, public, false); err != nil || state != "history_gap_unrecoverable" {
		t.Fatal("witness ahead of local issuer frontier was not distinguished")
	}
	// Two independently authenticating prefixes at the same frontier can still
	// disagree. Sign a different accepted result for member 1 and retain the
	// exact original members 2 and 3, rather than merely corrupting a root.
	var originalMembers []byte
	if err := top.witnessDB.QueryRow(`SELECT members FROM h_witness_prefixes WHERE account_id=$1 AND issuer_stream='account'`, syncHarnessAccount).Scan(&originalMembers); err != nil {
		t.Fatal("read original witness prefix failed")
	}
	if !syncHarnessAuthenticatePrefix(originalMembers, 3, public) || r.ResultCommitment == nil {
		t.Fatal("original equal-frontier fixture is not authenticated")
	}
	conflicting := r
	conflictingResult := *r.ResultCommitment
	conflictingResult[0] ^= 1
	conflicting.ResultCommitment = &conflictingResult
	conflicting.CanonicalBody, conflicting.SignedBytes = nil, nil
	conflicting, err = syncmodel.SignReceipt(conflicting, keys.signing)
	if err != nil {
		t.Fatal("sign conflicting witness member failed")
	}
	firstSize := int(binary.BigEndian.Uint32(originalMembers[8:12]))
	conflictingMembers := binary.BigEndian.AppendUint64(nil, 1)
	conflictingMembers = binary.BigEndian.AppendUint32(conflictingMembers, uint32(len(conflicting.CanonicalBody)))
	conflictingMembers = append(conflictingMembers, conflicting.CanonicalBody...)
	conflictingMembers = append(conflictingMembers, conflicting.SignedBytes...)
	conflictingMembers = append(conflictingMembers, originalMembers[12+firstSize+ed25519.SignatureSize:]...)
	if !syncHarnessAuthenticatePrefix(conflictingMembers, 3, public) || bytes.Equal(conflictingMembers, originalMembers) {
		t.Fatal("conflicting equal-frontier fixture is not independently authenticated")
	}
	conflictingRoot := sha256.Sum256(conflictingMembers)
	if _, err := top.witnessDB.Exec(`UPDATE h_witness_prefixes SET root=$1,members=$2 WHERE account_id=$3 AND issuer_stream='account'`, conflictingRoot[:], conflictingMembers, syncHarnessAccount); err != nil {
		t.Fatal("inject authenticated equal-frontier conflict failed")
	}
	if state, err := syncHarnessPin(ctx, top, node, 3, public, false); err != nil || state != "evidence_conflict" {
		t.Fatal("authenticated equal conflicting frontier was not distinguished")
	}
	if _, err := top.witnessDB.Exec(`UPDATE h_witness_prefixes SET root=$1 WHERE account_id=$2 AND issuer_stream='account'`, bytes.Repeat([]byte{9}, 32), syncHarnessAccount); err != nil {
		t.Fatal("inject invalid witness authentication failed")
	}
	if state, err := syncHarnessPin(ctx, top, node, 3, public, false); err != nil || state != "evidence_conflict" {
		t.Fatal("invalid witness authentication was not refused")
	}
	if syncHarnessCount(t, node, "h_logical_actions") != 3 || syncHarnessCount(t, node, "h_receipts") != 3 {
		t.Fatal("witness retry duplicated logical action")
	}
	if !t.Failed() {
		t.Log("POSTGRES_PASS: all local publication projections pending before authenticated prefix CAS; dropped pin ACK retries exact receipt; behind/equal-conflicting/ahead witness cases distinct")
	}
}

func TestSyncHarnessLocalCaptureThenApply(t *testing.T) {
	top := newSyncHarnessTopology(t)
	node := top.nodes[2]
	keys := newSyncHarnessKeys()
	seedSyncHarnessAdmission(t, node, keys)
	seedSyncHarnessCapture(t, node)
	e := syncHarnessEnvelope(syncHarnessAccount)
	e.ID[0] = 41
	e.Basis.ObjectID = "provisional_before_apply"
	if err := syncHarnessLocalCapture(context.Background(), node, keys, e, e.Basis.ObjectID); err != nil {
		t.Fatal(err)
	}
	accepted := syncHarnessEnvelope(syncHarnessAccount)
	accepted.ID[0] = 42
	accepted.Basis.ObjectID = "accepted_after_capture"
	r, err := syncHarnessApply(context.Background(), node, keys, accepted, false, "")
	if err != nil {
		t.Fatal(err)
	}
	var captured, acceptedOrdinal int64
	if err := node.store.pool.QueryRow(context.Background(), `SELECT last_finalized FROM h_finalized_counter WHERE account_id=$1`, syncHarnessAccount).Scan(&captured); err != nil {
		t.Fatal("read capture frontier failed")
	}
	if err := node.store.pool.QueryRow(context.Background(), `SELECT last_accepted FROM h_acceptance_counter WHERE account_id=$1`, syncHarnessAccount).Scan(&acceptedOrdinal); err != nil {
		t.Fatal("read issuer frontier failed")
	}
	if captured != 2 || acceptedOrdinal != 1 || r.Acceptance != 1 || syncHarnessCount(t, node, "h_receipts") != 1 {
		t.Fatal("provisional capture confused terminal issuer prefix")
	}
	syncHarnessPrepareWitness(t, top)
	if state, err := syncHarnessPin(context.Background(), top, node, 1, keys.signing.Public().(ed25519.PublicKey), false); err != nil || state != "published" {
		t.Fatal("accepted issuer prefix after provisional capture did not publish")
	}
	if !t.Failed() {
		t.Log("POSTGRES_PASS: local capture position and accepted issuer position compose without inventing a terminal receipt")
	}
}

func TestSyncHarnessPublicationOutboxForgedSignature(t *testing.T) {
	top := newSyncHarnessTopology(t)
	node := top.nodes[2]
	keys := newSyncHarnessKeys()
	public := keys.signing.Public().(ed25519.PublicKey)
	seedSyncHarnessAdmission(t, node, keys)
	seedSyncHarnessCapture(t, node)
	syncHarnessPrepareWitness(t, top)
	ctx := context.Background()
	e := syncHarnessEnvelope(syncHarnessAccount)
	original, err := syncHarnessApply(ctx, node, keys, e, false, "")
	if err != nil {
		t.Fatal(err)
	}
	var members []byte
	if err := node.store.pool.QueryRow(ctx, `SELECT prefix_members FROM h_publication_outbox WHERE account_id=$1 AND issuer_stream='account' AND through_ordinal=1`, syncHarnessAccount).Scan(&members); err != nil {
		t.Fatal("read committed publication member failed")
	}
	if !syncHarnessAuthenticatePrefix(members, 1, public) {
		t.Fatal("original committed prefix was not authenticated")
	}
	// Preserve the valid framing, account, issuer, ordinal and canonical body.
	// Only the last signature byte differs, and the transport root is recomputed
	// correctly so a hash/framing check cannot stand in for signature validation.
	members[len(members)-1] ^= 1
	root := sha256.Sum256(members)
	tx, err := node.store.pool.Begin(ctx)
	if err != nil {
		t.Fatal("begin forged transport injection failed")
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `ALTER TABLE h_publication_outbox DISABLE TRIGGER h_publication_immutable`); err != nil {
		t.Fatal("open fixture-only corruption seam failed")
	}
	if _, err := tx.Exec(ctx, `UPDATE h_publication_outbox SET prefix_commitment=$2,prefix_members=$3 WHERE account_id=$1 AND issuer_stream='account' AND through_ordinal=1`, syncHarnessAccount, root[:], members); err != nil {
		t.Fatal("inject forged persisted transport member failed")
	}
	if _, err := tx.Exec(ctx, `ALTER TABLE h_publication_outbox ENABLE TRIGGER h_publication_immutable`); err != nil {
		t.Fatal("restore fixture outbox immutability failed")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal("commit forged transport injection failed")
	}
	if state, err := syncHarnessPin(ctx, top, node, 1, public, false); err != nil || state != "evidence_conflict" {
		t.Fatal("forged signature with valid transport root was not refused")
	}
	var witnesses int
	if err := top.witnessDB.QueryRow(`SELECT count(*) FROM h_witness_prefixes`).Scan(&witnesses); err != nil || witnesses != 0 {
		t.Fatal("forged signature changed witness state")
	}
	for _, kind := range []string{"list", "show", "checkpoint", "snapshot", "cursor", "control", "grant"} {
		view, err := syncHarnessReadPublication(ctx, node, kind)
		if err != nil || view.State != "durability_publication_pending" || view.CursorThrough != 0 || view.Executable {
			t.Fatalf("forged signature settled %s projection", kind)
		}
	}
	var pending bool
	if err := node.store.pool.QueryRow(ctx, `SELECT state='pending' AND witness_proof_ref IS NULL FROM h_publication_outbox WHERE account_id=$1 AND issuer_stream='account' AND through_ordinal=1`, syncHarnessAccount).Scan(&pending); err != nil || !pending {
		t.Fatal("forged signature settled local outbox")
	}
	retry, err := syncHarnessApply(ctx, node, keys, e, false, "")
	if err != nil {
		t.Fatal("forged transport changed canonical receipt lookup")
	}
	syncHarnessSameReceipt(t, original, retry)
	if syncHarnessCount(t, node, "h_receipts") != 1 || syncHarnessCount(t, node, "h_logical_actions") != 1 {
		t.Fatal("forged transport changed canonical outcome or event count")
	}
	if !t.Failed() {
		t.Log("POSTGRES_PASS: persisted member with one forged signature byte and recomputed root yields evidence_conflict; witness unchanged, all local publication projections pending and exact canonical receipt preserved")
	}
}
