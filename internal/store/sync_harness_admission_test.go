package store

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/witwave-ai/witself/internal/syncharness"
	"github.com/witwave-ai/witself/internal/syncmodel"
)

// All signing/comparison capability is held outside the portable node rows.
// Public verifier rows are the only cryptographic key material in the schema.
type syncHarnessKeys struct {
	current    syncmodel.ComparisonKeyID
	comparison map[string]map[syncmodel.ComparisonKeyID][]byte
	signing    ed25519.PrivateKey
}

func (k *syncHarnessKeys) ComparisonKey(account string, id syncmodel.ComparisonKeyID) ([]byte, error) {
	key, ok := k.comparison[account][id]
	if !ok {
		return nil, errors.New("fixture comparison capability unavailable")
	}
	return bytes.Clone(key), nil
}

func newSyncHarnessKeys() *syncHarnessKeys {
	k := &syncHarnessKeys{current: "comparison_old", comparison: make(map[string]map[syncmodel.ComparisonKeyID][]byte), signing: ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize))}
	for _, account := range []string{syncHarnessAccount, "h_adversary_R"} {
		k.comparison[account] = map[syncmodel.ComparisonKeyID][]byte{"comparison_old": bytes.Repeat([]byte{3}, 32), "comparison_new": bytes.Repeat([]byte{4}, 32)}
	}
	return k
}

func syncHarnessEnvelope(account string) syncmodel.OperationEnvelope {
	agent := account + "_agent_R"
	return syncmodel.OperationEnvelope{
		EnvelopeVersion: 1, SchemaVersion: 1, ProtocolVersion: 1, ID: syncmodel.OperationID{11},
		OriginalIncarnation: syncmodel.Incarnation{AccountID: account, Lifecycle: 1, Restore: syncmodel.RestoreMarker{2}}, Capture: 1,
		Origin: syncmodel.Origin{InstallationID: "installation_R", ReplicaID: "replica_R", WriteLineageID: "lineage_R", OriginSequence: 1}, Kind: "fixture_create",
		Authorization: syncmodel.AuthorizationEvidence{PrincipalKind: syncmodel.PrincipalAgent, PrincipalID: agent, AgentID: &agent, RealmID: account + "_realm_R", AccessProfileID: "fixture_profile", PolicyRevision: "policy_1", EnrollmentGeneration: "enrollment_1", GrantID: "grant_1"},
		Basis:         syncmodel.CausalBasis{Family: "facts_subjects_candidates_aliases", ObjectID: "fixture_object", Generation: syncmodel.GenerationID{3}, ExpectedAbsent: true, Expected: []syncmodel.RevisionID{}, Dependencies: []syncmodel.Dependency{}, DomainBasis: syncmodel.DomainValue(syncmodel.DomainTag("facts_subjects_candidates_aliases"), syncmodel.NullValue())}, Payload: syncmodel.StringValue("synthetic payload"),
	}
}

// This context is supplied by the fixture authenticator independently of input.
func syncHarnessScope(account string) syncmodel.AdmissionScope {
	fixture := syncHarnessEnvelope(account)
	return syncmodel.AdmissionScope{AccountID: account, Origin: fixture.Origin, Authorization: fixture.Authorization, MayCompare: true, MayMutate: true}
}

func seedSyncHarnessAdmission(t *testing.T, node *syncHarnessNode, keys *syncHarnessKeys) {
	t.Helper()
	seedSyncHarnessAdmissionHistory(t, node, keys, false)
}
func seedSyncHarnessAdmissionHistory(t *testing.T, node *syncHarnessNode, keys *syncHarnessKeys, retired bool) {
	t.Helper()
	for _, account := range []string{syncHarnessAccount, "h_adversary_R"} {
		e := syncHarnessEnvelope(account)
		if _, err := node.store.pool.Exec(context.Background(), `INSERT INTO h_incarnations(account_id,lifecycle_epoch,restore_marker,allocation_id,transition_id,authority_installation,state) VALUES($1,1,$2,$3,$4,'installation_R','published')`, account, e.OriginalIncarnation.Restore[:], []byte{1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, []byte{2, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}); err != nil {
			t.Fatal("seed harness incarnation failed")
		}
		var retiredAt *time.Time
		var proof []byte
		if retired {
			when := time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC)
			retiredAt = &when
			commitment, err := syncmodel.InputCommitment(keys, account, "comparison_old", e)
			if err != nil {
				t.Fatal("prepare fixture comparison failed")
			}
			receipt, err := syncHarnessSignReceipt(keys, e, "comparison_old", commitment, 1, false)
			if err != nil {
				t.Fatal("prepare sealed fixture member failed")
			}
			proof = receipt.CanonicalBody
		}
		if _, err := node.store.pool.Exec(context.Background(), `INSERT INTO h_verifier_history(account_id,issuer_id,authority_stream,signing_key_id,public_key,valid_from,retired_at,sealed_prefix_proof) VALUES($1,'fixture_R','account','signing_old',$2,'2026-01-01T00:00:00Z',$3,$4)`, account, []byte(keys.signing.Public().(ed25519.PublicKey)), retiredAt, proof); err != nil {
			t.Fatal("seed public verifier history failed")
		}
	}
}

type syncHarnessAdmissionResult struct {
	receipt  syncmodel.Receipt
	admitted bool
	err      error
}
type syncHarnessAdmissionBarrier struct {
	beforeLock chan int32
	locked     chan struct{}
	proceed    <-chan struct{}
}

// The lock is scoped by physical schema and logical account/operation. Hash
// collision can over-serialize but cannot confer scope: full scope is compared
// before and after locking. This adapter is compiled only into store tests.
func syncHarnessAdmit(ctx context.Context, node *syncHarnessNode, keys *syncHarnessKeys, e syncmodel.OperationEnvelope, scope syncmodel.AdmissionScope, barrier *syncHarnessAdmissionBarrier) (syncmodel.Receipt, bool, error) {
	if err := syncmodel.AuthorizeAdmission(e, scope, true); err != nil {
		return syncmodel.Receipt{}, false, errors.New("fixture admission unauthorized")
	}
	canonical, err := e.CanonicalBytes()
	if err != nil {
		return syncmodel.Receipt{}, false, errors.New("fixture envelope invalid")
	}
	tx, err := node.store.pool.Begin(ctx)
	if err != nil {
		return syncmodel.Receipt{}, false, errors.New("begin fixture admission failed")
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if barrier != nil && barrier.beforeLock != nil {
		var pid int32
		if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
			return syncmodel.Receipt{}, false, errors.New("observe fixture session failed")
		}
		barrier.beforeLock <- pid
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(current_schema() || chr(31) || $1 || chr(31) || encode($2::bytea,'hex'),0))`, scope.AccountID, e.ID[:]); err != nil {
		return syncmodel.Receipt{}, false, errors.New("lock fixture admission failed")
	}
	if barrier != nil && barrier.locked != nil {
		close(barrier.locked)
		select {
		case <-barrier.proceed:
		case <-ctx.Done():
			return syncmodel.Receipt{}, false, errors.New("fixture barrier expired")
		}
	}
	if err := syncmodel.AuthorizeAdmission(e, scope, true); err != nil {
		return syncmodel.Receipt{}, false, errors.New("fixture admission unauthorized")
	}
	var keyID string
	var storedCommitment []byte
	err = tx.QueryRow(ctx, `SELECT comparison_key_id,input_commitment FROM h_operations WHERE account_id=$1 AND operation_id=$2 FOR UPDATE`, scope.AccountID, e.ID[:]).Scan(&keyID, &storedCommitment)
	exists := err == nil
	if err == nil || errors.Is(err, pgx.ErrNoRows) {
		if authErr := syncmodel.AuthorizeAdmission(e, scope, exists); authErr != nil {
			return syncmodel.Receipt{}, false, errors.New("fixture admission unauthorized")
		}
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return syncmodel.Receipt{}, false, errors.New("read fixture admission failed")
	}
	if !exists {
		keyID = string(keys.current)
	}
	commitment, err := syncmodel.InputCommitment(keys, scope.AccountID, syncmodel.ComparisonKeyID(keyID), e)
	if err != nil {
		return syncmodel.Receipt{}, false, errors.New("fixture comparison capability unavailable")
	}
	if exists {
		table := "h_receipts"
		query := `SELECT canonical_body,signed_bytes FROM h_receipts WHERE account_id=$1 AND operation_id=$2`
		args := []any{scope.AccountID, e.ID[:]}
		changed := !bytes.Equal(storedCommitment, commitment[:])
		if changed {
			table = "h_reuse_refusals"
			query = `SELECT canonical_body,signed_bytes FROM h_reuse_refusals WHERE account_id=$1 AND operation_id=$2 AND comparison_key_id=$3 AND conflicting_commitment=$4`
			args = append(args, keyID, commitment[:])
		}
		var body, signature []byte
		if err := tx.QueryRow(ctx, query, args...).Scan(&body, &signature); err == nil {
			receipt, err := syncmodel.DecodeReceipt(body, signature)
			if err != nil {
				return syncmodel.Receipt{}, false, errors.New("stored fixture receipt invalid")
			}
			if err := tx.Commit(ctx); err != nil {
				return syncmodel.Receipt{}, false, errors.New("commit fixture lookup failed")
			}
			return receipt, false, nil
		} else if !errors.Is(err, pgx.ErrNoRows) || table == "h_receipts" {
			return syncmodel.Receipt{}, false, errors.New("read fixture outcome failed")
		}
		// The small admission fixture has one logical operation per account. Its
		// refusal coordinate is allocated under that same transaction lock; this is
		// not the later capture/journal committed-prefix counter.
		var refusalCount int64
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM h_reuse_refusals WHERE account_id=$1 AND operation_id=$2`, scope.AccountID, e.ID[:]).Scan(&refusalCount); err != nil {
			return syncmodel.Receipt{}, false, errors.New("read fixture refusal position failed")
		}
		receipt, err := syncHarnessSignReceipt(keys, e, syncmodel.ComparisonKeyID(keyID), commitment, syncmodel.Ordinal(refusalCount+2), true)
		if err != nil {
			return syncmodel.Receipt{}, false, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO h_reuse_refusals(account_id,operation_id,comparison_key_id,conflicting_commitment,canonical_body,signed_bytes,retain_until) VALUES($1,$2,$3,$4,$5,$6,'2027-01-01T00:00:00Z')`, scope.AccountID, e.ID[:], keyID, commitment[:], receipt.CanonicalBody, receipt.SignedBytes); err != nil {
			return syncmodel.Receipt{}, false, errors.New("persist fixture reuse refusal failed")
		}
		if err := tx.Commit(ctx); err != nil {
			return syncmodel.Receipt{}, false, errors.New("commit fixture reuse refusal failed")
		}
		return receipt, false, nil
	}
	if _, err := tx.Exec(ctx, `INSERT INTO h_operations(account_id,operation_id,original_lifecycle_epoch,original_restore_marker,original_capture_epoch,original_installation,original_replica,original_write_lineage,original_sequence,operation_kind,envelope_version,schema_version,protocol_version,comparison_key_id,input_commitment,admission_state) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,'admitted')`, scope.AccountID, e.ID[:], int64(e.OriginalIncarnation.Lifecycle), e.OriginalIncarnation.Restore[:], int64(e.Capture), e.Origin.InstallationID, e.Origin.ReplicaID, e.Origin.WriteLineageID, int64(e.Origin.OriginSequence), e.Kind, e.EnvelopeVersion, e.SchemaVersion, e.ProtocolVersion, keyID, commitment[:]); err != nil {
		return syncmodel.Receipt{}, false, errors.New("insert fixture admission failed")
	}
	if _, err := tx.Exec(ctx, `INSERT INTO h_operation_payloads(account_id,operation_id,canonical_semantic_envelope) VALUES($1,$2,$3)`, scope.AccountID, e.ID[:], canonical); err != nil {
		return syncmodel.Receipt{}, false, errors.New("persist complete fixture envelope failed")
	}
	// Only a local miniature row is affected; no delivery or cross-store apply.
	if _, err := tx.Exec(ctx, `INSERT INTO h_fixture_objects(account_id,object_id,owner_node,value) VALUES($1,'admitted_fixture','R','synthetic')`, scope.AccountID); err != nil {
		return syncmodel.Receipt{}, false, errors.New("insert miniature fixture effect failed")
	}
	receipt, err := syncHarnessSignReceipt(keys, e, syncmodel.ComparisonKeyID(keyID), commitment, 1, false)
	if err != nil {
		return syncmodel.Receipt{}, false, err
	}
	if err := syncHarnessInsertReceipt(ctx, tx, receipt); err != nil {
		return syncmodel.Receipt{}, false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE h_operations SET admission_state='terminal',receipt_ref=operation_id WHERE account_id=$1 AND operation_id=$2`, scope.AccountID, e.ID[:]); err != nil {
		return syncmodel.Receipt{}, false, errors.New("finalize fixture admission failed")
	}
	if err := tx.Commit(ctx); err != nil {
		return syncmodel.Receipt{}, false, errors.New("commit fixture admission failed")
	}
	return receipt, true, nil
}

func syncHarnessSignReceipt(keys *syncHarnessKeys, e syncmodel.OperationEnvelope, keyID syncmodel.ComparisonKeyID, c syncmodel.Commitment, ordinal syncmodel.Ordinal, reuse bool) (syncmodel.Receipt, error) {
	r := syncmodel.Receipt{Version: 1, AccountID: e.OriginalIncarnation.AccountID, Operation: e.ID, OriginalOrigin: e.Origin, OriginalIncarnation: e.OriginalIncarnation, OriginalCapture: e.Capture, ComparisonKeyID: keyID, InputCommitment: c, IssuerID: "fixture_R", SigningKeyID: "signing_old", AuthorityStream: "account", IssuerIncarnation: syncHarnessEnvelope(e.OriginalIncarnation.AccountID).OriginalIncarnation, IssuerAuthorityEpoch: syncmodel.AuthorityEpoch{Scope: syncmodel.AuthorityAccount, Epoch: 1}, Acceptance: ordinal, Status: syncmodel.StatusAccepted, Reason: syncmodel.ReasonApplied, CausalResult: syncmodel.NullValue()}
	if reuse {
		r.Status = syncmodel.StatusConflict
		r.Reason = syncmodel.ReasonOperationIDReuse
	}
	signed, err := syncmodel.SignReceipt(r, keys.signing)
	if err != nil {
		return syncmodel.Receipt{}, errors.New("sign fixture receipt failed")
	}
	return signed, nil
}

func syncHarnessInsertReceipt(ctx context.Context, tx pgx.Tx, r syncmodel.Receipt) error {
	authority, err := syncmodel.MarshalCanonical(r.IssuerAuthorityEpoch)
	if err != nil {
		return errors.New("encode fixture authority failed")
	}
	causal, err := syncmodel.MarshalCanonical(r.CausalResult)
	if err != nil {
		return errors.New("encode fixture causal result failed")
	}
	_, err = tx.Exec(ctx, `INSERT INTO h_receipts(account_id,operation_id,original_lifecycle_epoch,original_restore_marker,original_capture_epoch,original_installation,original_replica,original_write_lineage,original_sequence,issuer_id,issuer_lifecycle_epoch,issuer_restore_marker,authority_stream,issuer_authority_epoch,acceptance_ordinal,comparison_key_id,input_commitment,status,reason,causal_result,signing_key_id,canonical_body,signed_bytes,settled_at,retain_until) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,'2026-01-01T00:00:00Z','2027-01-01T00:00:00Z')`, r.AccountID, r.Operation[:], int64(r.OriginalIncarnation.Lifecycle), r.OriginalIncarnation.Restore[:], int64(r.OriginalCapture), r.OriginalOrigin.InstallationID, r.OriginalOrigin.ReplicaID, r.OriginalOrigin.WriteLineageID, int64(r.OriginalOrigin.OriginSequence), r.IssuerID, int64(r.IssuerIncarnation.Lifecycle), r.IssuerIncarnation.Restore[:], r.AuthorityStream, string(authority), int64(r.Acceptance), string(r.ComparisonKeyID), r.InputCommitment[:], string(r.Status), string(r.Reason), causal, r.SigningKeyID, r.CanonicalBody, r.SignedBytes)
	if err != nil {
		return errors.New("persist immutable fixture receipt failed")
	}
	return nil
}

func syncHarnessSameReceipt(t *testing.T, a, b syncmodel.Receipt) {
	t.Helper()
	if !bytes.Equal(a.CanonicalBody, b.CanonicalBody) || !bytes.Equal(a.SignedBytes, b.SignedBytes) {
		t.Fatal("stored receipt bytes changed")
	}
}
func syncHarnessCounts(t *testing.T, node *syncHarnessNode, operations, receipts, refusals, objects int) {
	t.Helper()
	for _, item := range []struct {
		table string
		want  int
	}{{"h_operations", operations}, {"h_operation_payloads", operations}, {"h_receipts", receipts}, {"h_reuse_refusals", refusals}, {"h_fixture_objects", objects}} {
		var got int
		if err := node.store.pool.QueryRow(context.Background(), `SELECT count(*) FROM `+item.table).Scan(&got); err != nil || got != item.want {
			t.Errorf("%s count: got %d, want %d", item.table, got, item.want)
		}
	}
}

func syncHarnessWaitForAdmissionLock(ctx context.Context, t *testing.T, node *syncHarnessNode, pid int32) {
	t.Helper()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting bool
		if err := node.store.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE pid=$1 AND locktype='advisory' AND NOT granted)`, pid).Scan(&waiting); err != nil {
			t.Fatal("observe fixture-owned admission lock failed")
		}
		if waiting {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("fixture admission did not wait on lock")
		case <-ticker.C:
		}
	}
}

func TestSyncHarnessOperationIDConcurrentAdmission(t *testing.T) {
	top := newSyncHarnessTopology(t)
	node := top.nodes[2]
	keys := newSyncHarnessKeys()
	seedSyncHarnessAdmission(t, node, keys)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	envelope := syncHarnessEnvelope(syncHarnessAccount)
	scope := syncHarnessScope(syncHarnessAccount)
	firstLocked := make(chan struct{})
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	done := make(chan syncHarnessAdmissionResult, 2)
	go func() {
		r, a, err := syncHarnessAdmit(ctx, node, keys, envelope, scope, &syncHarnessAdmissionBarrier{locked: firstLocked, proceed: release})
		done <- syncHarnessAdmissionResult{r, a, err}
	}()
	select {
	case <-firstLocked:
	case <-ctx.Done():
		t.Fatal("first admission did not acquire lock")
	}
	secondPID := make(chan int32, 1)
	go func() {
		r, a, err := syncHarnessAdmit(ctx, node, keys, envelope, scope, &syncHarnessAdmissionBarrier{beforeLock: secondPID})
		done <- syncHarnessAdmissionResult{r, a, err}
	}()
	var pid int32
	select {
	case pid = <-secondPID:
	case <-ctx.Done():
		t.Fatal("second admission did not start")
	}
	syncHarnessWaitForAdmissionLock(ctx, t, node, pid)
	close(release)
	results := make([]syncHarnessAdmissionResult, 0, 2)
	for range 2 {
		select {
		case r := <-done:
			if r.err != nil {
				t.Fatal(r.err)
			}
			results = append(results, r)
		case <-ctx.Done():
			t.Fatal("concurrent admissions did not finish")
		}
	}
	if results[0].admitted == results[1].admitted {
		t.Fatal("concurrent first-use did not yield exactly one admission")
	}
	syncHarnessSameReceipt(t, results[0].receipt, results[1].receipt)
	syncHarnessCounts(t, node, 1, 1, 0, 1)
	for _, other := range top.nodes {
		if other.id != syncharness.R {
			syncHarnessCounts(t, other, 0, 0, 0, 0)
		}
	}
	if !t.Failed() {
		t.Log("POSTGRES_PASS: observed second first-use waiter on fixture advisory lock; one admission/effect/receipt; original bytes returned; other nodes unchanged")
	}
}

func TestSyncHarnessAdmissionRetryReuseAndReopen(t *testing.T) {
	top := newSyncHarnessTopology(t)
	node := top.nodes[2]
	keys := newSyncHarnessKeys()
	seedSyncHarnessAdmission(t, node, keys)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	e := syncHarnessEnvelope(syncHarnessAccount)
	scope := syncHarnessScope(syncHarnessAccount)
	signing := keys.signing
	keys.signing = nil
	if _, _, err := syncHarnessAdmit(ctx, node, keys, e, scope, nil); err == nil {
		t.Fatal("missing signer admitted an operation")
	}
	syncHarnessCounts(t, node, 0, 0, 0, 0)
	keys.signing = signing
	original, admitted, err := syncHarnessAdmit(ctx, node, keys, e, scope, nil)
	if err != nil || !admitted {
		t.Fatal("initial fixture admission failed")
	}
	readOnly := scope
	readOnly.MayMutate = false
	readReceipt, fresh, readErr := syncHarnessAdmit(ctx, node, keys, e, readOnly, nil)
	if readErr != nil || fresh {
		t.Fatal("authorized equality-only retry was refused")
	}
	syncHarnessSameReceipt(t, original, readReceipt)
	newOperation := e
	newOperation.ID = syncmodel.OperationID{12}
	if _, _, err := syncHarnessAdmit(ctx, node, keys, newOperation, readOnly, nil); err == nil {
		t.Fatal("equality-only access admitted a new operation")
	}
	canonical, err := e.CanonicalBytes()
	if err != nil {
		t.Fatal("fixture envelope invalid")
	}
	var stored []byte
	if err := node.store.pool.QueryRow(ctx, `SELECT canonical_semantic_envelope FROM h_operation_payloads WHERE account_id=$1 AND operation_id=$2`, syncHarnessAccount, e.ID[:]).Scan(&stored); err != nil || !bytes.Equal(canonical, stored) {
		t.Fatal("complete semantic envelope was not retained exactly")
	}
	// Reopen the actual SQL pool; no in-memory receipt cache survives.
	reopened, err := Open(ctx, node.schema.DSN())
	if err != nil {
		t.Fatal("reopen fixture store failed")
	}
	defer closeSyncHarnessStore(t, reopened)
	retryNode := &syncHarnessNode{id: node.id, schema: node.schema, store: reopened}
	keys.current = "comparison_new"
	retry, admitted, err := syncHarnessAdmit(ctx, retryNode, keys, e, scope, nil)
	if err != nil || admitted {
		t.Fatal("reopened exact retry failed")
	}
	syncHarnessSameReceipt(t, original, retry)
	if retry.ComparisonKeyID != "comparison_old" {
		t.Fatal("retry changed original comparison key")
	}
	for _, change := range []string{"payload", "origin", "basis"} {
		t.Run(change, func(t *testing.T) {
			changed := e
			trusted := scope
			switch change {
			case "payload":
				changed.Payload = syncmodel.StringValue("different synthetic payload")
			case "origin":
				changed.Origin.OriginSequence = 2
				trusted.Origin.OriginSequence = 2
			case "basis":
				changed.Basis.Generation = syncmodel.GenerationID{9}
			}
			refusal, admitted, err := syncHarnessAdmit(ctx, retryNode, keys, changed, trusted, nil)
			if err != nil || admitted || refusal.Status != syncmodel.StatusConflict || refusal.Reason != syncmodel.ReasonOperationIDReuse {
				t.Fatal("changed semantic input did not yield stable reuse refusal")
			}
			repeat, admitted, err := syncHarnessAdmit(ctx, retryNode, keys, changed, trusted, nil)
			if err != nil || admitted {
				t.Fatal("reuse refusal retry failed")
			}
			syncHarnessSameReceipt(t, refusal, repeat)
			originalAgain, _, err := syncHarnessAdmit(ctx, retryNode, keys, e, scope, nil)
			if err != nil {
				t.Fatal("canonical outcome unavailable after reuse refusal")
			}
			syncHarnessSameReceipt(t, original, originalAgain)
		})
	}
	delete(keys.comparison[syncHarnessAccount], "comparison_old")
	if _, _, err := syncHarnessAdmit(ctx, retryNode, keys, e, scope, nil); err == nil {
		t.Fatal("missing old comparison capability accepted retry")
	}
	syncHarnessCounts(t, node, 1, 1, 3, 1)
	if !t.Failed() {
		t.Log("POSTGRES_PASS: signer failure rolls back admission/payload/effect; equality-only retry permitted; complete envelope retained; SQL reopen/rotation returns original receipt; payload/origin/basis each has one immutable refusal; missing original comparison key refuses without mutation")
	}
}

func TestSyncHarnessIncarnationDoesNotResetOperationNamespace(t *testing.T) {
	t.Cleanup(func() {
		if !t.Failed() && !t.Skipped() {
			t.Log("POSTGRES_PASS: valid successor incarnation cannot reset account/operation identity; one original effect and exact receipt remain with one reuse refusal; five schemas dropped and pools closed")
		}
	})
	top := newSyncHarnessTopology(t)
	node := top.nodes[2]
	keys := newSyncHarnessKeys()
	seedSyncHarnessAdmission(t, node, keys)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	e := syncHarnessEnvelope(syncHarnessAccount)
	scope := syncHarnessScope(syncHarnessAccount)
	original, admitted, err := syncHarnessAdmit(ctx, node, keys, e, scope, nil)
	if err != nil || !admitted {
		t.Fatal("initial fixture admission failed")
	}
	originalBody, originalSignature := bytes.Clone(original.CanonicalBody), bytes.Clone(original.SignedBytes)
	syncHarnessCounts(t, node, 1, 1, 0, 1)
	marker, err := syncmodel.NewRestoreMarker()
	if err != nil || marker == e.OriginalIncarnation.Restore {
		t.Fatal("fresh successor restore marker unavailable")
	}
	changed := e
	changed.OriginalIncarnation = syncmodel.Incarnation{AccountID: syncHarnessAccount, Lifecycle: 2, Restore: marker}
	if err := changed.Validate(); err != nil {
		t.Fatal("successor fixture envelope invalid")
	}
	allocation, transition := syncmodel.TransferID{3}, syncmodel.TransferID{4}
	if _, err := node.store.pool.Exec(ctx, `INSERT INTO h_incarnations(account_id,lifecycle_epoch,restore_marker,predecessor_epoch,predecessor_marker,allocation_id,transition_id,authority_installation,state) VALUES($1,2,$2,$3,$4,$5,$6,'installation_R','published')`, syncHarnessAccount, marker[:], int64(e.OriginalIncarnation.Lifecycle), e.OriginalIncarnation.Restore[:], allocation[:], transition[:]); err != nil {
		t.Fatal("seed valid successor incarnation failed")
	}
	refusal, admitted, err := syncHarnessAdmit(ctx, node, keys, changed, scope, nil)
	if err != nil {
		t.Fatal("successor-incarnation retry failed before an outcome")
	}
	if admitted || refusal.Status != syncmodel.StatusConflict || refusal.Reason != syncmodel.ReasonOperationIDReuse {
		t.Error("successor incarnation reset account/operation namespace instead of conflict/operation_id_reuse")
	}
	var storedBody, storedSignature []byte
	if err := node.store.pool.QueryRow(ctx, `SELECT canonical_body,signed_bytes FROM h_receipts WHERE account_id=$1 AND operation_id=$2 AND original_lifecycle_epoch=$3 AND original_restore_marker=$4`, syncHarnessAccount, e.ID[:], int64(e.OriginalIncarnation.Lifecycle), e.OriginalIncarnation.Restore[:]).Scan(&storedBody, &storedSignature); err != nil || !bytes.Equal(storedBody, originalBody) || !bytes.Equal(storedSignature, originalSignature) {
		t.Fatal("successor-incarnation retry changed original stored receipt bytes")
	}
	retry, admitted, err := syncHarnessAdmit(ctx, node, keys, e, scope, nil)
	if err != nil || admitted {
		t.Fatal("original operation retry unavailable after incarnation conflict")
	}
	syncHarnessSameReceipt(t, original, retry)
	syncHarnessCounts(t, node, 1, 1, 1, 1)
	var owner, value string
	if err := node.store.pool.QueryRow(ctx, `SELECT owner_node,value FROM h_fixture_objects WHERE account_id=$1 AND object_id='admitted_fixture'`, syncHarnessAccount).Scan(&owner, &value); err != nil || owner != "R" || value != "synthetic" {
		t.Fatal("successor-incarnation retry changed the original miniature effect")
	}
}

func TestSyncHarnessAdmissionAuthenticationAndAccountIsolation(t *testing.T) {
	top := newSyncHarnessTopology(t)
	node := top.nodes[2]
	keys := newSyncHarnessKeys()
	seedSyncHarnessAdmission(t, node, keys)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	e := syncHarnessEnvelope(syncHarnessAccount)
	scope := syncHarnessScope(syncHarnessAccount)
	original, _, err := syncHarnessAdmit(ctx, node, keys, e, scope, nil)
	if err != nil {
		t.Fatal("initial fixture admission failed")
	}
	for _, variant := range []string{"wrong_account", "forged_principal", "comparison_denied", "wrong_origin", "invalid_version", "missing_identity", "oversized_payload"} {
		t.Run(variant, func(t *testing.T) {
			bad := e
			trusted := scope
			switch variant {
			case "wrong_account":
				trusted = syncHarnessScope("h_adversary_R")
			case "forged_principal":
				bad.Authorization.PrincipalID = "forged"
			case "comparison_denied":
				trusted.MayCompare = false
			case "wrong_origin":
				bad.Origin.ReplicaID = "other_replica"
			case "invalid_version":
				bad.ProtocolVersion = 2
			case "missing_identity":
				bad.OriginalIncarnation.AccountID = ""
			case "oversized_payload":
				bad.Payload = syncmodel.StringValue(string(bytes.Repeat([]byte{'x'}, syncmodel.MaxEnvelopeBytes)))
			}
			if _, _, err := syncHarnessAdmit(ctx, node, keys, bad, trusted, nil); err == nil {
				t.Fatal("unauthorized or malformed admission succeeded")
			}
			syncHarnessCounts(t, node, 1, 1, 0, 1)
		})
	}
	// SC-4.9 explicitly permits independent random-ID use in another account.
	// Replaying the first account's envelope is denied above; a fresh authenticated
	// envelope scoped to the second account has an independent canonical receipt.
	independent := syncHarnessEnvelope("h_adversary_R")
	second, admitted, err := syncHarnessAdmit(ctx, node, keys, independent, syncHarnessScope("h_adversary_R"), nil)
	if err != nil || !admitted {
		t.Fatal("independent other-account operation ID was refused")
	}
	if second.Operation != original.Operation || second.AccountID == original.AccountID || second.InputCommitment == original.InputCommitment {
		t.Fatal("account namespace or commitment separation failed")
	}
	syncHarnessCounts(t, node, 2, 2, 0, 2)
	if !t.Failed() {
		t.Log("POSTGRES_PASS: unauthorized replay, forged claims, invalid identity/version/size refuse before disclosure; separately authenticated accounts may independently use the same random operation ID")
	}
}

func TestSyncHarnessHistoricalReceiptMembership(t *testing.T) {
	top := newSyncHarnessTopology(t)
	node := top.nodes[2]
	keys := newSyncHarnessKeys()
	seedSyncHarnessAdmissionHistory(t, node, keys, true)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	e := syncHarnessEnvelope(syncHarnessAccount)
	receipt, _, err := syncHarnessAdmit(ctx, node, keys, e, syncHarnessScope(syncHarnessAccount), nil)
	if err != nil {
		t.Fatal("historical fixture admission failed")
	}
	var public, sealed []byte
	var retired bool
	if err := node.store.pool.QueryRow(ctx, `SELECT public_key,sealed_prefix_proof,retired_at IS NOT NULL FROM h_verifier_history WHERE account_id=$1 AND issuer_id='fixture_R' AND authority_stream='account' AND signing_key_id='signing_old'`, syncHarnessAccount).Scan(&public, &sealed, &retired); err != nil || !retired {
		t.Fatal("read retired public verifier failed")
	}
	// The fixture certifies the stored sealed membership externally; a production
	// deployment must supply an authenticated independent prefix proof adapter.
	commitment, err := syncmodel.InputCommitment(keys, syncHarnessAccount, "comparison_old", e)
	if err != nil {
		t.Fatal("compare historical fixture input failed")
	}
	expected := syncmodel.ReceiptExpectation{OriginalEnvelope: e, ComparisonKeyID: "comparison_old", InputCommitment: commitment, Trust: syncmodel.ReceiptTrust{IssuerID: "fixture_R", SigningKeyID: "signing_old", AuthorityStream: "account", IssuerIncarnation: e.OriginalIncarnation, AuthorityEpoch: syncmodel.AuthorityEpoch{Scope: syncmodel.AuthorityAccount, Epoch: 1}, Retired: true, SealedBodies: [][]byte{sealed}}}
	verified, err := syncmodel.VerifyReceipt(receipt, ed25519.PublicKey(public), expected)
	if err != nil || !verified.SignatureValid || !verified.HistoricalOnly || verified.CurrentAuthority {
		t.Fatal("retired historical member was not historical only")
	}
	forged := receipt
	forged.CanonicalBody = nil
	forged.SignedBytes = nil
	forged.Acceptance = 2
	forged, err = syncmodel.SignReceipt(forged, keys.signing)
	if err != nil {
		t.Fatal("prepare retired-key nonmember failed")
	}
	if _, err := syncmodel.VerifyReceipt(forged, ed25519.PublicKey(public), expected); err == nil {
		t.Fatal("valid retired signature authorized an unsealed outcome")
	}
	expected.Trust.SealedBodies = nil
	if _, err := syncmodel.VerifyReceipt(receipt, ed25519.PublicKey(public), expected); err == nil {
		t.Fatal("retired signature without exact membership verified")
	}
	if !t.Failed() {
		t.Log("POSTGRES_PASS: persisted retired verifier and exact sealed member verify historical only; valid retired-key nonmember and absent membership refuse; no current authority")
	}
}

func TestSyncHarnessAdmissionSchemaConstraintsAndImmutability(t *testing.T) {
	top := newSyncHarnessTopology(t)
	node := top.nodes[2]
	keys := newSyncHarnessKeys()
	seedSyncHarnessAdmission(t, node, keys)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	e := syncHarnessEnvelope(syncHarnessAccount)
	scope := syncHarnessScope(syncHarnessAccount)
	original, _, err := syncHarnessAdmit(ctx, node, keys, e, scope, nil)
	if err != nil {
		t.Fatal("initial fixture admission failed")
	}
	changed := e
	changed.Payload = syncmodel.StringValue("changed fixture")
	if _, _, err := syncHarnessAdmit(ctx, node, keys, changed, scope, nil); err != nil {
		t.Fatal("initial fixture refusal failed")
	}
	for _, table := range []string{"h_incarnations", "h_operations", "h_operation_payloads", "h_receipts", "h_reuse_refusals", "h_verifier_history"} {
		t.Run(table, func(t *testing.T) {
			for _, statement := range []string{`UPDATE ` + table + ` SET account_id=account_id`, `DELETE FROM ` + table} {
				if _, err := node.store.pool.Exec(ctx, statement); err == nil {
					t.Fatal("immutable evidence mutation accepted")
				}
			}
		})
	}
	// Clone rows through PostgreSQL's typed record constructor to challenge checks
	// before primary-key uniqueness, without writing values into diagnostics.
	for _, check := range []struct{ name, table, patch string }{
		{"short_operation", "h_operations", `jsonb_build_object('operation_id',encode(decode('00','hex'),'escape'))`},
		{"long_lifecycle", "h_incarnations", `jsonb_build_object('lifecycle_epoch',9007199254740992)`},
		{"zero_capture", "h_operations", `jsonb_build_object('original_capture_epoch',0)`},
		{"invalid_reason_pair", "h_receipts", `jsonb_build_object('status','accepted','reason','operation_id_reuse')`},
		{"short_signature", "h_receipts", `jsonb_build_object('signed_bytes',encode(decode('00','hex'),'escape'))`},
		{"short_commitment", "h_operations", `jsonb_build_object('input_commitment',encode(decode('00','hex'),'escape'))`},
		{"short_retention", "h_receipts", `jsonb_build_object('retain_until','2026-01-02T00:00:00Z')`},
	} {
		t.Run(check.name, func(t *testing.T) {
			_, err := node.store.pool.Exec(ctx, `INSERT INTO `+check.table+` SELECT (jsonb_populate_record(NULL::`+check.table+`,to_jsonb(r) || `+check.patch+`)).* FROM `+check.table+` r LIMIT 1`)
			var coded interface{ SQLState() string }
			if !errors.As(err, &coded) || coded.SQLState() != "23514" {
				t.Fatal("schema check did not reject malformed evidence")
			}
		})
	}
	for _, check := range []struct{ name, table, patch string }{
		{"cross_account_receipt", "h_receipts", `jsonb_build_object('account_id','h_adversary_R')`},
		{"wrong_refusal_key", "h_reuse_refusals", `jsonb_build_object('comparison_key_id','comparison_new')`},
	} {
		t.Run(check.name, func(t *testing.T) {
			_, err := node.store.pool.Exec(ctx, `INSERT INTO `+check.table+` SELECT (jsonb_populate_record(NULL::`+check.table+`,to_jsonb(r) || `+check.patch+`)).* FROM `+check.table+` r LIMIT 1`)
			var coded interface{ SQLState() string }
			if !errors.As(err, &coded) || coded.SQLState() != "23503" {
				t.Fatal("scope foreign key accepted mismatched evidence")
			}
		})
	}
	after, _, err := syncHarnessAdmit(ctx, node, keys, e, scope, nil)
	if err != nil {
		t.Fatal("canonical receipt unavailable after constraint probes")
	}
	syncHarnessSameReceipt(t, original, after)
	syncHarnessCounts(t, node, 1, 1, 1, 1)
	if !t.Failed() {
		t.Log("POSTGRES_PASS: six evidence tables deny update/delete; typed-ID, lifecycle, capture, commitment, signature, reason-pair and retention constraints reject malformed inserts; original receipt unchanged")
	}
}
