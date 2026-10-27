package store

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/witwave-ai/witself/internal/syncmodel"
)

// This miniature adapter consumes explicit synthetic fragments; it does not
// reconcile requests, import production messages, or transfer graph authority.
type syncHarnessCompound struct {
	envelope   syncmodel.OperationEnvelope
	scope      syncmodel.AdmissionScope
	manifest   syncmodel.CompoundManifest
	fragments  [][]byte
	dependency string
}

var syncHarnessCompoundKinds = []string{"graph", "candidate", "sibling", "claim", "notification"}

func newSyncHarnessCompound(suffix string, id byte) syncHarnessCompound {
	m := syncmodel.CompoundManifest{
		GroupID: "group_" + suffix, RequestID: "request_" + suffix,
		AuthorityInstallationID: "installation_R", AuthorityEpoch: 1,
		ExpectedGraphRevision: 1, ResultGraphRevision: 2,
		OrderedMembers:    append([]string{}, syncHarnessCompoundKinds...),
		RequiredFragments: append([]string{}, syncHarnessCompoundKinds...),
	}
	c := syncHarnessCompound{manifest: m, dependency: "dependency_" + suffix}
	for _, kind := range syncHarnessCompoundKinds {
		c.fragments = append(c.fragments, []byte("synthetic:"+m.RequestID+":"+kind))
	}
	c.manifest.ManifestCommitment = syncHarnessCompoundCommitment(c.manifest, c.fragments)
	c.envelope = syncHarnessEnvelope(syncHarnessAccount)
	c.envelope.ID = syncmodel.OperationID{id}
	c.envelope.Kind = "fixture_compound"
	c.envelope.Origin.OriginSequence = syncmodel.Ordinal(id)
	c.envelope.Basis.ObjectID = c.manifest.RequestID
	c.envelope.Compound = &c.manifest
	c.scope = syncHarnessScope(syncHarnessAccount)
	c.scope.Origin = c.envelope.Origin
	return c
}

func syncHarnessCompoundCommitment(m syncmodel.CompoundManifest, fragments [][]byte) syncmodel.Commitment {
	m.ManifestCommitment = syncmodel.Commitment{}
	body, _ := syncmodel.MarshalCanonical(m)
	h := sha256.New()
	_, _ = h.Write(body)
	for _, fragment := range fragments {
		// Length framing prevents concatenation ambiguity. Only public synthetic
		// bytes are hashed here, never a private payload or comparison secret.
		_, _ = fmt.Fprintf(h, "%d:", len(fragment))
		_, _ = h.Write(fragment)
	}
	var out syncmodel.Commitment
	copy(out[:], h.Sum(nil))
	return out
}

func seedSyncHarnessCompound(t *testing.T, node *syncHarnessNode, keys *syncHarnessKeys, c syncHarnessCompound) {
	t.Helper()
	ctx := context.Background()
	account := c.envelope.OriginalIncarnation.AccountID
	tx, err := node.store.pool.Begin(ctx)
	if err != nil {
		t.Fatal("begin compound fixture failed")
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := syncHarnessInsertOperation(ctx, tx, keys, c.envelope); err != nil {
		t.Fatal(err)
	}
	body, err := syncmodel.MarshalCanonical(c.manifest)
	if err != nil {
		t.Fatal("encode compound manifest failed")
	}
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO h_graph_authority(account_id,request_id,graph_epoch,graph_revision,authority_installation,state) VALUES($1,$2,1,1,'installation_R','active')`, []any{account, c.manifest.RequestID}},
		{`INSERT INTO h_compounds(account_id,group_id,operation_id,request_id,graph_epoch,expected_revision,result_revision,authority_installation,manifest,required_fragments,dependency_object,state) VALUES($1,$2,$3,$4,1,1,2,'installation_R',$5,$6,$7,'staged')`, []any{account, c.manifest.GroupID, c.envelope.ID[:], c.manifest.RequestID, body, c.manifest.RequiredFragments, c.dependency}},
		{`INSERT INTO h_inbound(account_id,operation_id,group_id,state) VALUES($1,$2,$3,'staged')`, []any{account, c.envelope.ID[:], c.manifest.GroupID}},
		{`UPDATE h_operations SET admission_state='staged' WHERE account_id=$1 AND operation_id=$2`, []any{account, c.envelope.ID[:]}},
		{`INSERT INTO h_fixture_objects(account_id,object_id,owner_node,value) VALUES($1,$2,$3,'synthetic dependency')`, []any{account, c.dependency, string(node.id)}},
	} {
		if _, err := tx.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatal("seed compound fixture failed")
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal("commit compound fixture failed")
	}
}

func syncHarnessStageFragment(ctx context.Context, node *syncHarnessNode, account string, c syncHarnessCompound, index int, fragment []byte) error {
	if len(fragment) > syncmodel.MaxEnvelopeBytes {
		return syncmodel.ErrResourceCapacity
	}
	if index < 0 || index >= len(c.manifest.OrderedMembers) || len(fragment) == 0 {
		return errors.New("invalid synthetic fragment")
	}
	tx, err := node.store.pool.Begin(ctx)
	if err != nil {
		return errors.New("begin fragment staging failed")
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var manifest, operation []byte
	if err := tx.QueryRow(ctx, `SELECT manifest,operation_id FROM h_compounds WHERE account_id=$1 AND group_id=$2 FOR UPDATE`, account, c.manifest.GroupID).Scan(&manifest, &operation); err != nil {
		return errors.New("compound scope unavailable")
	}
	expected, err := syncmodel.MarshalCanonical(c.manifest)
	if err != nil || !bytes.Equal(manifest, expected) {
		return errors.New("compound manifest conflict")
	}
	id := c.manifest.OrderedMembers[index]
	var old []byte
	err = tx.QueryRow(ctx, `SELECT fragment_bytes FROM h_fragments WHERE account_id=$1 AND group_id=$2 AND fragment_id=$3`, account, c.manifest.GroupID, id).Scan(&old)
	if err == nil {
		if !bytes.Equal(old, fragment) {
			return errors.New("altered duplicate fragment")
		}
		return tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return errors.New("read synthetic fragment failed")
	}
	var retained int
	if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(octet_length(fragment_bytes)),0) FROM h_fragments WHERE account_id=$1 AND group_id=$2`, account, c.manifest.GroupID).Scan(&retained); err != nil {
		return errors.New("read compound retained bytes failed")
	}
	if len(fragment) > syncmodel.MaxCompoundBytes-retained {
		return syncmodel.ErrResourceCapacity
	}
	if err := syncHarnessReserveFragmentBytes(ctx, tx, account, operation, len(fragment)); err != nil {
		return err
	}
	commitment := sha256.Sum256(fragment)
	if _, err := tx.Exec(ctx, `INSERT INTO h_fragments(account_id,group_id,fragment_id,member_index,commitment,fragment_bytes) VALUES($1,$2,$3,$4,$5,$6)`, account, c.manifest.GroupID, id, index, commitment[:], fragment); err != nil {
		return errors.New("persist synthetic fragment failed")
	}
	if err := tx.Commit(ctx); err != nil {
		return errors.New("commit fragment staging failed")
	}
	return nil
}

type syncHarnessCompoundBarrier struct {
	messageWritten chan struct{}
	proceed        <-chan struct{}
	crash          bool
}

// Snapshot and incremental entry points both call this exact publication
// transaction. The mode affects diagnostics only, never the visibility boundary.
func syncHarnessPublishCompound(ctx context.Context, node *syncHarnessNode, keys *syncHarnessKeys, c syncHarnessCompound, mode string, barrier *syncHarnessCompoundBarrier) (syncmodel.Receipt, error) {
	if mode != "snapshot" && mode != "incremental" {
		return syncmodel.Receipt{}, errors.New("unsupported miniature publication mode")
	}
	if err := c.manifest.Validate(); err != nil {
		return syncmodel.Receipt{}, errors.New("invalid compound manifest")
	}
	// This miniature domain has exactly these five semantic members. Counting
	// fragments alone cannot prove that the required graph/link member arrived.
	if len(c.manifest.OrderedMembers) != len(syncHarnessCompoundKinds) || len(c.manifest.RequiredFragments) != len(syncHarnessCompoundKinds) {
		return syncmodel.Receipt{}, errors.New("incomplete compound semantic manifest")
	}
	for index, kind := range syncHarnessCompoundKinds {
		if c.manifest.OrderedMembers[index] != kind {
			return syncmodel.Receipt{}, errors.New("compound semantic member order invalid")
		}
		found := false
		for _, required := range c.manifest.RequiredFragments {
			found = found || required == kind
		}
		if !found {
			return syncmodel.Receipt{}, errors.New("compound semantic member missing")
		}
	}
	tx, err := node.store.pool.Begin(ctx)
	if err != nil {
		return syncmodel.Receipt{}, errors.New("begin compound publication failed")
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var state, dependency string
	var stored, operation []byte
	if err := tx.QueryRow(ctx, `SELECT state,manifest,dependency_object,operation_id FROM h_compounds WHERE account_id=$1 AND group_id=$2 FOR UPDATE`, syncHarnessAccount, c.manifest.GroupID).Scan(&state, &stored, &dependency, &operation); err != nil {
		return syncmodel.Receipt{}, errors.New("compound unavailable")
	}
	if !bytes.Equal(operation, c.envelope.ID[:]) {
		return syncmodel.Receipt{}, errors.New("compound operation association conflict")
	}
	var keyID string
	var originalCommitment []byte
	if err := tx.QueryRow(ctx, `SELECT comparison_key_id,input_commitment FROM h_operations WHERE account_id=$1 AND operation_id=$2 FOR UPDATE`, syncHarnessAccount, operation).Scan(&keyID, &originalCommitment); err != nil {
		return syncmodel.Receipt{}, errors.New("compound admission unavailable")
	}
	if err := syncmodel.AuthorizeAdmission(c.envelope, c.scope, true); err != nil || c.scope.AccountID != syncHarnessAccount {
		return syncmodel.Receipt{}, errors.New("compound comparison unauthorized")
	}
	commitment, err := syncmodel.InputCommitment(keys, syncHarnessAccount, syncmodel.ComparisonKeyID(keyID), c.envelope)
	if err != nil {
		return syncmodel.Receipt{}, errors.New("compound original comparison capability unavailable")
	}
	if !bytes.Equal(originalCommitment, commitment[:]) {
		return syncHarnessCompoundReuseRefusal(ctx, tx, keys, c.envelope, syncmodel.ComparisonKeyID(keyID), commitment)
	}
	expected, err := syncmodel.MarshalCanonical(c.manifest)
	if err != nil || !bytes.Equal(stored, expected) || dependency != c.dependency {
		return syncmodel.Receipt{}, errors.New("compound manifest conflict")
	}
	if state != "staged" {
		var body, signature []byte
		if err := tx.QueryRow(ctx, `SELECT canonical_body,signed_bytes FROM h_receipts WHERE account_id=$1 AND operation_id=$2`, syncHarnessAccount, operation).Scan(&body, &signature); err != nil {
			return syncmodel.Receipt{}, errors.New("compound outcome unavailable")
		}
		receipt, err := syncmodel.DecodeReceipt(body, signature)
		if err != nil {
			return syncmodel.Receipt{}, errors.New("compound stored receipt invalid")
		}
		return receipt, tx.Commit(ctx)
	}
	var graphRevision int64
	var installation string
	if err := tx.QueryRow(ctx, `SELECT graph_revision,authority_installation FROM h_graph_authority WHERE account_id=$1 AND request_id=$2 AND graph_epoch=$3 AND state='active' FOR UPDATE`, syncHarnessAccount, c.manifest.RequestID, int64(c.manifest.AuthorityEpoch)).Scan(&graphRevision, &installation); err != nil || graphRevision != int64(c.manifest.ExpectedGraphRevision) || installation != c.manifest.AuthorityInstallationID {
		return syncmodel.Receipt{}, errors.New("compound graph authority stale")
	}
	var dependencyExists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM h_fixture_objects WHERE account_id=$1 AND object_id=$2)`, syncHarnessAccount, dependency).Scan(&dependencyExists); err != nil || !dependencyExists {
		return syncmodel.Receipt{}, errors.New("compound dependency unavailable")
	}
	rows, err := tx.Query(ctx, `SELECT fragment_id,member_index,commitment,fragment_bytes FROM h_fragments WHERE account_id=$1 AND group_id=$2 ORDER BY member_index`, syncHarnessAccount, c.manifest.GroupID)
	if err != nil {
		return syncmodel.Receipt{}, errors.New("read compound fragments failed")
	}
	var fragments [][]byte
	for rows.Next() {
		var id string
		var index int
		var commitment, fragment []byte
		if err := rows.Scan(&id, &index, &commitment, &fragment); err != nil {
			rows.Close()
			return syncmodel.Receipt{}, errors.New("read compound fragment failed")
		}
		actual := sha256.Sum256(fragment)
		if index != len(fragments) || index >= len(c.manifest.OrderedMembers) || id != c.manifest.OrderedMembers[index] || !bytes.Equal(commitment, actual[:]) {
			rows.Close()
			return syncmodel.Receipt{}, errors.New("compound fragment mismatch")
		}
		fragments = append(fragments, fragment)
	}
	rowErr := rows.Err()
	rows.Close()
	if rowErr != nil || len(fragments) != len(c.manifest.RequiredFragments) || syncHarnessCompoundCommitment(c.manifest, fragments) != c.manifest.ManifestCommitment {
		return syncmodel.Receipt{}, errors.New("compound incomplete or commitment mismatch")
	}
	// Notification is intentionally inserted first. Other connections cannot
	// observe this row between writes, and even this transaction marks it inert.
	message := "message_" + c.manifest.GroupID
	if _, err := tx.Exec(ctx, `INSERT INTO h_fixture_messages(account_id,message_id,group_id,request_id,visible,publication_state) VALUES($1,$2,$3,$4,false,'durability_publication_pending')`, syncHarnessAccount, message, c.manifest.GroupID, c.manifest.RequestID); err != nil {
		return syncmodel.Receipt{}, errors.New("install compound message failed")
	}
	if barrier != nil {
		if barrier.messageWritten != nil {
			close(barrier.messageWritten)
			select {
			case <-barrier.proceed:
			case <-ctx.Done():
				return syncmodel.Receipt{}, errors.New("compound barrier expired")
			}
		}
		if barrier.crash {
			return syncmodel.Receipt{}, errors.New("synthetic crash between compound writes")
		}
	}
	for _, kind := range c.manifest.OrderedMembers {
		if _, err := tx.Exec(ctx, `INSERT INTO h_fixture_request_links(account_id,group_id,request_id,message_id,link_kind,visible) VALUES($1,$2,$3,$4,$5,true)`, syncHarnessAccount, c.manifest.GroupID, c.manifest.RequestID, message, kind); err != nil {
			return syncmodel.Receipt{}, errors.New("install compound link failed")
		}
	}
	ordinal, err := syncHarnessNextAcceptance(ctx, tx, syncHarnessAccount)
	if err != nil {
		return syncmodel.Receipt{}, errors.New("finalize compound position failed")
	}
	// The immutable admission key remains authoritative after provider rotation.
	originalKeys := *keys
	originalKeys.current = syncmodel.ComparisonKeyID(keyID)
	receipt, err := syncHarnessInsertAcceptedReceipt(ctx, tx, &originalKeys, c.envelope, commitment, ordinal, false)
	if err != nil {
		return syncmodel.Receipt{}, err
	}
	if err := syncHarnessRecordAction(ctx, tx, syncHarnessAccount, c.envelope.ID[:], "fixture_compound"); err != nil {
		return syncmodel.Receipt{}, err
	}
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE h_graph_authority SET graph_revision=$3 WHERE account_id=$1 AND request_id=$2 AND state='active'`, []any{syncHarnessAccount, c.manifest.RequestID, int64(c.manifest.ResultGraphRevision)}},
		{`UPDATE h_compounds SET state='publication_pending',complete=true,finalized=$3 WHERE account_id=$1 AND group_id=$2`, []any{syncHarnessAccount, c.manifest.GroupID, ordinal}},
		{`UPDATE h_inbound SET state='applied' WHERE account_id=$1 AND operation_id=$2`, []any{syncHarnessAccount, c.envelope.ID[:]}},
		{`UPDATE h_fixture_messages SET visible=true WHERE account_id=$1 AND group_id=$2`, []any{syncHarnessAccount, c.manifest.GroupID}},
	} {
		if _, err := tx.Exec(ctx, statement.sql, statement.args...); err != nil {
			return syncmodel.Receipt{}, errors.New("finalize compound publication failed")
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return syncmodel.Receipt{}, errors.New("commit compound publication failed")
	}
	return receipt, nil
}

// Changed immutable input has its own durable outcome, keyed by the original
// comparison key and new commitment; it never replaces the compound receipt.
// The caller still holds the compound and admitted-operation row locks.
func syncHarnessCompoundReuseRefusal(ctx context.Context, tx pgx.Tx, keys *syncHarnessKeys, e syncmodel.OperationEnvelope, keyID syncmodel.ComparisonKeyID, commitment syncmodel.Commitment) (syncmodel.Receipt, error) {
	var body, signature []byte
	err := tx.QueryRow(ctx, `SELECT canonical_body,signed_bytes FROM h_reuse_refusals WHERE account_id=$1 AND operation_id=$2 AND comparison_key_id=$3 AND conflicting_commitment=$4`, syncHarnessAccount, e.ID[:], string(keyID), commitment[:]).Scan(&body, &signature)
	if err == nil {
		receipt, err := syncmodel.DecodeReceipt(body, signature)
		if err != nil {
			return syncmodel.Receipt{}, errors.New("compound stored reuse refusal invalid")
		}
		return receipt, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return syncmodel.Receipt{}, errors.New("read compound reuse refusal failed")
	}
	var count int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM h_reuse_refusals WHERE account_id=$1 AND operation_id=$2`, syncHarnessAccount, e.ID[:]).Scan(&count); err != nil {
		return syncmodel.Receipt{}, errors.New("read compound reuse refusal count failed")
	}
	receipt, err := syncHarnessSignReceipt(keys, e, keyID, commitment, syncmodel.Ordinal(count+2), true)
	if err != nil {
		return syncmodel.Receipt{}, errors.New("sign compound reuse refusal failed")
	}
	if _, err := tx.Exec(ctx, `INSERT INTO h_reuse_refusals(account_id,operation_id,comparison_key_id,conflicting_commitment,canonical_body,signed_bytes,retain_until) VALUES($1,$2,$3,$4,$5,$6,'2027-01-01T00:00:00Z')`, syncHarnessAccount, e.ID[:], string(keyID), commitment[:], receipt.CanonicalBody, receipt.SignedBytes); err != nil {
		return syncmodel.Receipt{}, errors.New("persist compound reuse refusal failed")
	}
	return receipt, tx.Commit(ctx)
}

// Imported inspection can run in a read-only transaction. It deliberately reads
// only projection tables; request lifecycle reconciliation is not invoked.
func syncHarnessInspectCompound(ctx context.Context, node *syncHarnessNode, c syncHarnessCompound, afterFirstRead ...func()) (messages, links int, pending bool, err error) {
	tx, err := node.store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return 0, 0, false, errors.New("begin inert inspection failed")
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var pendingCount int
	err = tx.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE publication_state='durability_publication_pending') FROM h_fixture_messages WHERE account_id=$1 AND group_id=$2 AND visible`, syncHarnessAccount, c.manifest.GroupID).Scan(&messages, &pendingCount)
	if err == nil {
		if len(afterFirstRead) != 0 && afterFirstRead[0] != nil {
			afterFirstRead[0]()
		}
		err = tx.QueryRow(ctx, `SELECT count(*) FROM h_fixture_request_links WHERE account_id=$1 AND group_id=$2 AND visible`, syncHarnessAccount, c.manifest.GroupID).Scan(&links)
	}
	if err != nil {
		return 0, 0, false, errors.New("read inert compound projection failed")
	}
	return messages, links, pendingCount == messages && messages > 0, tx.Commit(ctx)
}

func syncHarnessClaimOrdinary(ctx context.Context, node *syncHarnessNode, message string) (bool, error) {
	result, err := node.store.pool.Exec(ctx, `UPDATE h_fixture_messages m SET claimed=true WHERE m.account_id=$1 AND m.message_id=$2 AND m.visible AND NOT m.claimed AND NOT EXISTS (SELECT 1 FROM h_fixture_request_links l WHERE l.account_id=m.account_id AND l.message_id=m.message_id AND l.visible)`, syncHarnessAccount, message)
	if err != nil {
		return false, errors.New("ordinary fixture claim failed")
	}
	return result.RowsAffected() == 1, nil
}

// Updating the visible publication state requires the acknowledged authenticated
// witness pin. The inspection path above never repairs or advances this state.
func syncHarnessAcknowledgeCompoundPin(ctx context.Context, node *syncHarnessNode, c syncHarnessCompound) error {
	tx, err := node.store.pool.Begin(ctx)
	if err != nil {
		return errors.New("begin compound pin acknowledgement failed")
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	result, err := tx.Exec(ctx, `UPDATE h_fixture_messages m SET publication_state='published'
 WHERE m.account_id=$1 AND m.group_id=$2 AND m.visible AND EXISTS (
 SELECT 1 FROM h_compounds c JOIN h_publication_outbox p ON p.account_id=c.account_id AND p.operation_id=c.operation_id AND p.through_ordinal=c.finalized
 WHERE c.account_id=m.account_id AND c.group_id=m.group_id AND p.state='pinned' AND p.witness_proof_ref IS NOT NULL)`, syncHarnessAccount, c.manifest.GroupID)
	if err != nil || result.RowsAffected() != 1 {
		return errors.New("compound verified publication pin unavailable")
	}
	if _, err := tx.Exec(ctx, `UPDATE h_compounds SET state='published' WHERE account_id=$1 AND group_id=$2`, syncHarnessAccount, c.manifest.GroupID); err != nil {
		return errors.New("persist compound publication pin failed")
	}
	return tx.Commit(ctx)
}

func syncHarnessAssertCompound(t *testing.T, node *syncHarnessNode, c syncHarnessCompound, visible bool) {
	t.Helper()
	messages, links, pending, err := syncHarnessInspectCompound(context.Background(), node, c)
	if err != nil {
		t.Fatal(err)
	}
	wantMessages, wantLinks, wantAccounting := 0, 0, 0
	if visible {
		wantMessages, wantLinks, wantAccounting = 1, len(c.manifest.OrderedMembers), 1
	}
	if messages != wantMessages || links != wantLinks || pending != visible {
		t.Fatalf("compound projection: messages=%d links=%d pending=%t", messages, links, pending)
	}
	for _, table := range []string{"h_receipts", "h_logical_actions"} {
		var n int
		if err := node.store.pool.QueryRow(context.Background(), `SELECT count(*) FROM `+table+` WHERE account_id=$1 AND operation_id=$2`, syncHarnessAccount, c.envelope.ID[:]).Scan(&n); err != nil || n != wantAccounting {
			t.Fatalf("compound %s count=%d expected=%d", table, n, wantAccounting)
		}
	}
	claimed, err := syncHarnessClaimOrdinary(context.Background(), node, "message_"+c.manifest.GroupID)
	if err != nil || claimed {
		t.Fatal("protocol notification became ordinarily claimable")
	}
}

func TestSyncHarnessCompoundNotificationBeforeLink(t *testing.T) {
	top := newSyncHarnessTopology(t)
	node, keys := top.nodes[2], newSyncHarnessKeys()
	seedSyncHarnessAdmission(t, node, keys)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for modeIndex, mode := range []string{"incremental", "snapshot"} {
		t.Run(mode, func(t *testing.T) {
			c := newSyncHarnessCompound(mode, byte(60+modeIndex))
			seedSyncHarnessCompound(t, node, keys, c)
			// Each delivery is a separate committed batch, with notification first.
			for _, index := range []int{4, 1, 3, 0} {
				if err := syncHarnessStageFragment(ctx, node, syncHarnessAccount, c, index, c.fragments[index]); err != nil {
					t.Fatal(err)
				}
				if _, err := syncHarnessPublishCompound(ctx, node, keys, c, mode, nil); err == nil {
					t.Fatal("incomplete compound published")
				}
				syncHarnessAssertCompound(t, node, c, false)
			}
			if err := syncHarnessStageFragment(ctx, node, syncHarnessAccount, c, 4, c.fragments[4]); err != nil {
				t.Fatal("exact fragment retry failed")
			}
			if err := syncHarnessStageFragment(ctx, node, syncHarnessAccount, c, 4, []byte("altered synthetic fragment")); err == nil {
				t.Fatal("altered duplicate fragment accepted")
			}
			if err := syncHarnessStageFragment(ctx, node, "h_adversary_R", c, 2, c.fragments[2]); err == nil {
				t.Fatal("cross-account fragment attachment accepted")
			}
			if err := syncHarnessStageFragment(ctx, node, syncHarnessAccount, c, 2, c.fragments[2]); err != nil {
				t.Fatal(err)
			}
			if _, err := syncHarnessPublishCompound(ctx, node, keys, c, mode, &syncHarnessCompoundBarrier{crash: true}); err == nil {
				t.Fatal("between-write crash committed")
			}
			syncHarnessAssertCompound(t, node, c, false)
			var rawMessages int
			if err := node.store.pool.QueryRow(ctx, `SELECT count(*) FROM h_fixture_messages WHERE group_id=$1`, c.manifest.GroupID).Scan(&rawMessages); err != nil || rawMessages != 0 {
				t.Fatal("crash left a hidden message row")
			}
			written, release := make(chan struct{}), make(chan struct{})
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			result := make(chan syncHarnessAdmissionResult, 1)
			go func() {
				r, err := syncHarnessPublishCompound(ctx, node, keys, c, mode, &syncHarnessCompoundBarrier{messageWritten: written, proceed: release})
				result <- syncHarnessAdmissionResult{receipt: r, err: err}
			}()
			select {
			case <-written:
			case <-ctx.Done():
				t.Fatal("compound publication barrier not reached")
			}
			// A real second PostgreSQL connection races the uncommitted write.
			syncHarnessAssertCompound(t, node, c, false)
			close(release)
			published := <-result
			if published.err != nil {
				t.Fatal(published.err)
			}
			syncHarnessAssertCompound(t, node, c, true)
			otherMode := "snapshot"
			if mode == otherMode {
				otherMode = "incremental"
			}
			retried, err := syncHarnessPublishCompound(ctx, node, keys, c, otherMode, nil)
			if err != nil {
				t.Fatal(err)
			}
			syncHarnessSameReceipt(t, published.receipt, retried)
			syncHarnessAssertCompound(t, node, c, true)
		})
	}
	// The claim adapter has a positive control: it really claims ordinary work.
	if _, err := node.store.pool.Exec(ctx, `INSERT INTO h_fixture_messages(account_id,message_id,visible,publication_state) VALUES($1,'ordinary_message',true,'ordinary')`, syncHarnessAccount); err != nil {
		t.Fatal("seed ordinary positive control failed")
	}
	if claimed, err := syncHarnessClaimOrdinary(ctx, node, "ordinary_message"); err != nil || !claimed {
		t.Fatal("ordinary positive-control message was not claimable")
	}
	if !t.Failed() {
		t.Log("POSTGRES_PASS: H14 notification-first/split staging, rollback between projection writes, second-connection claim race, shared snapshot/incremental transaction, immutable receipt and one accounting action; projection-only read-only inspection")
	}
}

func TestSyncHarnessCompoundRejectsInvalidPublication(t *testing.T) {
	top := newSyncHarnessTopology(t)
	node, keys := top.nodes[2], newSyncHarnessKeys()
	seedSyncHarnessAdmission(t, node, keys)
	ctx := context.Background()
	for index, fault := range []string{"graph_revision", "graph_epoch", "authority_installation", "missing_dependency", "wrong_account_dependency", "manifest_commitment", "manifest_identity", "missing_required_member"} {
		t.Run(fault, func(t *testing.T) {
			c := newSyncHarnessCompound(fault, byte(70+index))
			if fault == "manifest_commitment" {
				c.manifest.ManifestCommitment = syncmodel.Commitment{91}
			}
			if fault == "missing_required_member" {
				c.manifest.RequiredFragments[4] = "unprovided_member"
				c.manifest.ManifestCommitment = syncHarnessCompoundCommitment(c.manifest, c.fragments)
			}
			c.envelope.Compound = &c.manifest
			seedSyncHarnessCompound(t, node, keys, c)
			for member, fragment := range c.fragments {
				if err := syncHarnessStageFragment(ctx, node, syncHarnessAccount, c, member, fragment); err != nil {
					t.Fatal(err)
				}
			}
			var err error
			switch fault {
			case "graph_revision":
				_, err = node.store.pool.Exec(ctx, `UPDATE h_graph_authority SET graph_revision=2 WHERE account_id=$1 AND request_id=$2`, syncHarnessAccount, c.manifest.RequestID)
			case "graph_epoch":
				_, err = node.store.pool.Exec(ctx, `UPDATE h_graph_authority SET state='fenced' WHERE account_id=$1 AND request_id=$2`, syncHarnessAccount, c.manifest.RequestID)
				if err == nil {
					_, err = node.store.pool.Exec(ctx, `INSERT INTO h_graph_authority(account_id,request_id,graph_epoch,graph_revision,authority_installation,state) VALUES($1,$2,2,1,'installation_R','active')`, syncHarnessAccount, c.manifest.RequestID)
				}
			case "authority_installation":
				_, err = node.store.pool.Exec(ctx, `UPDATE h_graph_authority SET authority_installation='installation_other' WHERE account_id=$1 AND request_id=$2`, syncHarnessAccount, c.manifest.RequestID)
			case "missing_dependency", "wrong_account_dependency":
				_, err = node.store.pool.Exec(ctx, `DELETE FROM h_fixture_objects WHERE account_id=$1 AND object_id=$2`, syncHarnessAccount, c.dependency)
				if err == nil && fault == "wrong_account_dependency" {
					_, err = node.store.pool.Exec(ctx, `INSERT INTO h_fixture_objects(account_id,object_id,owner_node,value) VALUES('h_adversary_R',$1,'R','synthetic wrong scope')`, c.dependency)
				}
			case "manifest_identity":
				c.manifest.ResultGraphRevision = 3
			}
			if err != nil {
				t.Fatal("inject synthetic compound fault failed")
			}
			outcome, publishErr := syncHarnessPublishCompound(ctx, node, keys, c, "incremental", nil)
			if fault == "manifest_identity" {
				// This changes the already-admitted complete envelope. R2 requires
				// a separately keyed reuse refusal, not the original outcome.
				if publishErr != nil || outcome.Status != syncmodel.StatusConflict || outcome.Reason != syncmodel.ReasonOperationIDReuse {
					t.Fatal("altered admitted manifest did not return reuse refusal")
				}
			} else if publishErr == nil {
				t.Fatal("invalid compound publication accepted")
			}
			syncHarnessAssertCompound(t, node, c, false)
		})
	}
	if !t.Failed() {
		t.Log("POSTGRES_PASS: H14 exact graph epoch/revision/authority, required-fragment set, commitment, manifest identity, and scoped dependency failures remain staged and invisible")
	}
}

func TestSyncHarnessCompoundDeferredVisibilityGuard(t *testing.T) {
	top := newSyncHarnessTopology(t)
	node, keys := top.nodes[2], newSyncHarnessKeys()
	seedSyncHarnessAdmission(t, node, keys)
	c := newSyncHarnessCompound("partial_guard", 90)
	seedSyncHarnessCompound(t, node, keys, c)
	other := newSyncHarnessCompound("other_graph", 92)
	seedSyncHarnessCompound(t, node, keys, other)
	ctx := context.Background()
	if _, err := node.store.pool.Exec(ctx, `UPDATE h_inbound SET group_id=$3 WHERE account_id=$1 AND operation_id=$2`, syncHarnessAccount, other.envelope.ID[:], c.manifest.GroupID); err == nil {
		t.Fatal("inbound operation attached to another compound")
	}
	for _, fault := range []string{"message_wrong_request", "link_wrong_request", "link_wrong_group"} {
		t.Run(fault, func(t *testing.T) {
			tx, err := node.store.pool.Begin(ctx)
			if err != nil {
				t.Fatal("begin graph scope fixture failed")
			}
			defer func() { _ = tx.Rollback(ctx) }()
			request := c.manifest.RequestID
			if fault == "message_wrong_request" {
				request = other.manifest.RequestID
			}
			_, err = tx.Exec(ctx, `INSERT INTO h_fixture_messages(account_id,message_id,group_id,request_id,visible,publication_state) VALUES($1,'scoped_message',$2,$3,false,'durability_publication_pending')`, syncHarnessAccount, c.manifest.GroupID, request)
			if fault == "message_wrong_request" {
				if err == nil {
					t.Fatal("message attached to another compound request")
				}
				return
			}
			if err != nil {
				t.Fatal("seed scoped projection message failed")
			}
			group := c.manifest.GroupID
			if fault == "link_wrong_group" {
				group = other.manifest.GroupID
			}
			_, err = tx.Exec(ctx, `INSERT INTO h_fixture_request_links(account_id,group_id,request_id,message_id,link_kind,visible) VALUES($1,$2,$3,'scoped_message','graph',false)`, syncHarnessAccount, group, other.manifest.RequestID)
			if err == nil {
				t.Fatal("link crossed compound request or message scope")
			}
		})
	}
	// This deliberately repeats the unsafe notification-before-link pattern:
	// visible notification committed separately from its graph. SQL must reject it.
	tx, err := node.store.pool.Begin(ctx)
	if err != nil {
		t.Fatal("begin invalid projection fixture failed")
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `INSERT INTO h_fixture_messages(account_id,message_id,group_id,request_id,visible,publication_state) VALUES($1,'partial_message',$2,$3,true,'durability_publication_pending')`, syncHarnessAccount, c.manifest.GroupID, c.manifest.RequestID); err != nil {
		t.Fatal("invalid projection insert failed before deferred boundary")
	}
	if err := tx.Commit(ctx); err == nil {
		t.Fatal("partial visible notification escaped deferred publication guard")
	}
	syncHarnessAssertCompound(t, node, c, false)
	if !t.Failed() {
		t.Log("POSTGRES_PASS: H14 deferred SQL guard rejects a separately committed visible notification before graph/receipt/accounting")
	}
}

func TestSyncHarnessCompoundWitnessPinBeforeExecution(t *testing.T) {
	top := newSyncHarnessTopology(t)
	node, keys := top.nodes[2], newSyncHarnessKeys()
	seedSyncHarnessAdmission(t, node, keys)
	syncHarnessPrepareWitness(t, top)
	c := newSyncHarnessCompound("pin_execution", 91)
	seedSyncHarnessCompound(t, node, keys, c)
	ctx := context.Background()
	for index, fragment := range c.fragments {
		if err := syncHarnessStageFragment(ctx, node, syncHarnessAccount, c, index, fragment); err != nil {
			t.Fatal(err)
		}
	}
	r, err := syncHarnessPublishCompound(ctx, node, keys, c, "snapshot", nil)
	if err != nil {
		t.Fatal(err)
	}
	syncHarnessAssertCompound(t, node, c, true)
	if err := syncHarnessAcknowledgeCompoundPin(ctx, node, c); err == nil {
		t.Fatal("unwitnessed graph became executable")
	}
	public := keys.signing.Public().(ed25519.PublicKey)
	if state, err := syncHarnessPin(ctx, top, node, int64(r.Acceptance), public, true); err != nil || state != "durability_publication_pending" {
		t.Fatal("dropped witness acknowledgement settled compound")
	}
	if err := syncHarnessAcknowledgeCompoundPin(ctx, node, c); err == nil {
		t.Fatal("missing witness acknowledgement settled compound")
	}
	if state, err := syncHarnessPin(ctx, top, node, int64(r.Acceptance), public, false); err != nil || state != "published" {
		t.Fatal("authenticated compound witness retry failed")
	}
	if err := syncHarnessAcknowledgeCompoundPin(ctx, node, c); err != nil {
		t.Fatal(err)
	}
	messages, links, pending, err := syncHarnessInspectCompound(ctx, node, c)
	if err != nil || messages != 1 || links != 5 || pending {
		t.Fatal("verified compound pin did not settle inert projection")
	}
	retried, err := syncHarnessPublishCompound(ctx, node, keys, c, "incremental", nil)
	if err != nil {
		t.Fatal(err)
	}
	syncHarnessSameReceipt(t, r, retried)
	if claimed, err := syncHarnessClaimOrdinary(ctx, node, "message_"+c.manifest.GroupID); err != nil || claimed {
		t.Fatal("pinned protocol notification became ordinary work")
	}
	for _, sql := range []string{
		`UPDATE h_fixture_request_links SET visible=false WHERE account_id=$1 AND group_id=$2 AND link_kind='graph'`,
		`DELETE FROM h_fixture_request_links WHERE account_id=$1 AND group_id=$2 AND link_kind='graph'`,
	} {
		if _, err := node.store.pool.Exec(ctx, sql, syncHarnessAccount, c.manifest.GroupID); err == nil {
			t.Fatal("published projection lost its graph link")
		}
	}
	if !t.Failed() {
		t.Log("POSTGRES_PASS: H14 graph publication remains nonexecutable until authenticated witness ACK; dropped ACK retains pending projection and exact receipt; published graph cannot lose a link")
	}
}

func TestSyncHarnessCompoundRetryAdmissionBinding(t *testing.T) {
	top := newSyncHarnessTopology(t)
	node, keys := top.nodes[2], newSyncHarnessKeys()
	seedSyncHarnessAdmission(t, node, keys)
	ctx := context.Background()
	first, second := newSyncHarnessCompound("retry_first", 101), newSyncHarnessCompound("retry_second", 102)
	for _, c := range []syncHarnessCompound{first, second} {
		seedSyncHarnessCompound(t, node, keys, c)
		for index, fragment := range c.fragments {
			if err := syncHarnessStageFragment(ctx, node, syncHarnessAccount, c, index, fragment); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Admission fixes the comparison key, even when the current provider rotates
	// before publication or a terminal retry.
	keys.current = "comparison_new"
	original, err := syncHarnessPublishCompound(ctx, node, keys, first, "incremental", nil)
	if err != nil || original.ComparisonKeyID != "comparison_old" {
		t.Fatal("compound publication did not preserve admitted comparison key")
	}
	other, err := syncHarnessPublishCompound(ctx, node, keys, second, "snapshot", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, fault := range []string{"payload", "basis", "origin", "authorization", "scope", "comparison_denied", "unauthorized_origin", "unauthorized_authorization", "second_compound_id"} {
		t.Run(fault, func(t *testing.T) {
			changed := first
			wantRefusal := true
			switch fault {
			case "payload":
				changed.envelope.Payload = syncmodel.StringValue("changed synthetic payload")
			case "basis":
				changed.envelope.Basis.ObjectID = "changed_request_basis"
			case "origin":
				changed.envelope.Origin.OriginSequence++
				changed.scope.Origin = changed.envelope.Origin
			case "authorization":
				changed.envelope.Authorization.GrantID = "grant_2"
				changed.scope.Authorization.GrantID = "grant_2"
			case "scope":
				changed.envelope.OriginalIncarnation.AccountID = "h_adversary_R"
				changed.scope = syncHarnessScope("h_adversary_R")
				changed.scope.Origin = changed.envelope.Origin
				changed.envelope.Authorization = changed.scope.Authorization
				wantRefusal = false
			case "comparison_denied":
				changed.scope.MayCompare = false
				wantRefusal = false
			case "unauthorized_origin":
				changed.envelope.Origin.OriginSequence++
				wantRefusal = false
			case "unauthorized_authorization":
				changed.envelope.Authorization.PrincipalID = "other_principal"
				wantRefusal = false
			case "second_compound_id":
				changed.envelope.ID = second.envelope.ID
				wantRefusal = false
			}
			outcome, err := syncHarnessPublishCompound(ctx, node, keys, changed, "snapshot", nil)
			if wantRefusal {
				if err != nil || outcome.Status != syncmodel.StatusConflict || outcome.Reason != syncmodel.ReasonOperationIDReuse || outcome.ComparisonKeyID != "comparison_old" {
					t.Fatal("changed compound input did not produce original-key reuse refusal")
				}
				retried, err := syncHarnessPublishCompound(ctx, node, keys, changed, "incremental", nil)
				if err != nil {
					t.Fatal(err)
				}
				syncHarnessSameReceipt(t, outcome, retried)
			} else if err == nil || len(outcome.CanonicalBody) != 0 || len(outcome.SignedBytes) != 0 {
				t.Fatal("unauthorized or misassociated retry disclosed a compound receipt")
			}
			for _, originalCase := range []struct {
				compound syncHarnessCompound
				receipt  syncmodel.Receipt
			}{{first, original}, {second, other}} {
				retried, err := syncHarnessPublishCompound(ctx, node, keys, originalCase.compound, "incremental", nil)
				if err != nil {
					t.Fatal(err)
				}
				syncHarnessSameReceipt(t, originalCase.receipt, retried)
			}
			for _, table := range []string{"h_receipts", "h_logical_actions", "h_fixture_messages", "h_publication_outbox"} {
				if count := syncHarnessCount(t, node, table); count != 2 {
					t.Fatalf("retry changed %s count: got %d want 2", table, count)
				}
			}
			for _, bucket := range []string{"hour", "day"} {
				var count int
				if err := node.store.pool.QueryRow(ctx, `SELECT COALESCE(sum(count),0) FROM h_rollups WHERE account_id=$1 AND bucket=$2`, syncHarnessAccount, bucket).Scan(&count); err != nil || count != 2 {
					t.Fatal("compound retry changed aggregate accounting")
				}
			}
		})
	}
	if count := syncHarnessCount(t, node, "h_reuse_refusals"); count != 4 {
		t.Fatalf("reuse refusal count: got %d want 4", count)
	}
	oldKey := keys.comparison[syncHarnessAccount]["comparison_old"]
	delete(keys.comparison[syncHarnessAccount], "comparison_old")
	if receipt, err := syncHarnessPublishCompound(ctx, node, keys, first, "snapshot", nil); err == nil || len(receipt.CanonicalBody) != 0 {
		t.Fatal("missing original comparison key disclosed terminal outcome")
	}
	keys.comparison[syncHarnessAccount]["comparison_old"] = oldKey
	if !t.Failed() {
		t.Log("POSTGRES_PASS: compound retries bind stored operation and original comparison key; changed complete input gets stable reuse refusal, unauthorized and other-compound IDs disclose nothing, original receipt bytes/events remain unchanged")
	}
}

func TestSyncHarnessCompoundInspectionConcurrentPublication(t *testing.T) {
	top := newSyncHarnessTopology(t)
	node, keys := top.nodes[2], newSyncHarnessKeys()
	seedSyncHarnessAdmission(t, node, keys)
	c := newSyncHarnessCompound("inspection_snapshot", 103)
	seedSyncHarnessCompound(t, node, keys, c)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for index, fragment := range c.fragments {
		if err := syncHarnessStageFragment(ctx, node, syncHarnessAccount, c, index, fragment); err != nil {
			t.Fatal(err)
		}
	}
	read, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	type inspectionResult struct {
		messages, links int
		pending         bool
		err             error
	}
	result := make(chan inspectionResult, 1)
	go func() {
		messages, links, pending, err := syncHarnessInspectCompound(ctx, node, c, func() {
			close(read)
			select {
			case <-release:
			case <-ctx.Done():
			}
		})
		result <- inspectionResult{messages, links, pending, err}
	}()
	select {
	case <-read:
	case <-ctx.Done():
		t.Fatal("inspection first-read barrier not reached")
	}
	if _, err := syncHarnessPublishCompound(ctx, node, keys, c, "incremental", nil); err != nil {
		t.Fatal(err)
	}
	// Capture exact physical row versions after the writer commits. Inspection
	// must leave both projection tables byte-for-byte and row-version unchanged.
	projectionVersion := func() string {
		var version string
		if err := node.store.pool.QueryRow(ctx, `SELECT jsonb_build_array(
 (SELECT jsonb_agg(jsonb_build_array(xmin::text,ctid::text,to_jsonb(m)) ORDER BY message_id) FROM h_fixture_messages m),
 (SELECT jsonb_agg(jsonb_build_array(xmin::text,ctid::text,to_jsonb(l)) ORDER BY link_kind) FROM h_fixture_request_links l))::text`).Scan(&version); err != nil {
			t.Fatal("read projection row versions failed")
		}
		return version
	}
	before := projectionVersion()
	close(release)
	observed := <-result
	completeBefore := observed.messages == 0 && observed.links == 0
	completeAfter := observed.messages == 1 && observed.links == 5
	if observed.err != nil || !completeBefore && !completeAfter {
		t.Fatalf("inspection crossed publication snapshots: messages=%d links=%d", observed.messages, observed.links)
	}
	if observed.messages != 0 || observed.pending {
		t.Fatal("repeatable-read inspection did not retain its before-publication snapshot")
	}
	messages, links, pending, err := syncHarnessInspectCompound(ctx, node, c)
	if err != nil || messages != 1 || links != 5 || !pending {
		t.Fatal("fresh inspection missed complete committed publication")
	}
	if after := projectionVersion(); after != before {
		t.Fatal("inert inspection changed projection rows")
	}
	if count := syncHarnessCount(t, node, "h_logical_actions"); count != 1 {
		t.Fatal("inert inspection added a logical action")
	}
	if !t.Failed() {
		t.Log("POSTGRES_PASS: barrier commits complete publication between inspection SELECTs; repeatable-read read-only inspection sees 0/0 then fresh inspection 1/5, with zero projection/event writes")
	}
}

func TestSyncHarnessCompoundSQLByteBounds(t *testing.T) {
	top := newSyncHarnessTopology(t)
	node, keys := top.nodes[2], newSyncHarnessKeys()
	seedSyncHarnessAdmission(t, node, keys)
	ctx := context.Background()
	// Every assertion reconciles the scoped reservation with actual retained
	// envelope and fragment bytes, including immediately after an exact retry.
	assertResources := func(t *testing.T, node *syncHarnessNode, c syncHarnessCompound, wantFragments int) {
		t.Helper()
		account := c.envelope.OriginalIncarnation.AccountID
		var pending, retained, envelopes, envelopeBytes, allFragments, reservedFragments int
		if err := node.store.pool.QueryRow(ctx, `SELECT
 (SELECT pending_operations FROM h_resource_budgets WHERE account_id=$1),
 (SELECT retained_bytes FROM h_resource_budgets WHERE account_id=$1),
 (SELECT count(*) FROM h_operation_payloads WHERE account_id=$1),
 (SELECT COALESCE(sum(octet_length(canonical_semantic_envelope)),0) FROM h_operation_payloads WHERE account_id=$1),
 (SELECT COALESCE(sum(octet_length(fragment_bytes)),0) FROM h_fragments WHERE account_id=$1),
 (SELECT fragment_bytes FROM h_resource_reservations WHERE account_id=$1 AND operation_id=$2)`, account, c.envelope.ID[:]).Scan(&pending, &retained, &envelopes, &envelopeBytes, &allFragments, &reservedFragments); err != nil {
			t.Fatal("read compound resource accounting failed")
		}
		if pending != envelopes || retained != envelopeBytes+allFragments || reservedFragments != wantFragments {
			t.Fatalf("compound resource accounting mismatch: pending=%d envelopes=%d retained=%d actual=%d reserved fragments=%d want=%d", pending, envelopes, retained, envelopeBytes+allFragments, reservedFragments, wantFragments)
		}
	}
	stage := func(t *testing.T, node *syncHarnessNode, c syncHarnessCompound) {
		t.Helper()
		account := c.envelope.OriginalIncarnation.AccountID
		seedSyncHarnessCompound(t, node, keys, c)
		assertResources(t, node, c, 0)
		want := 0
		for member, fragment := range c.fragments {
			err := syncHarnessStageFragment(ctx, node, account, c, member, fragment)
			if want+len(fragment) > syncmodel.MaxCompoundBytes {
				if !errors.Is(err, syncmodel.ErrResourceCapacity) {
					t.Fatal("compound crossing did not explicitly refuse resource capacity")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				want += len(fragment)
				assertResources(t, node, c, want)
				if err := syncHarnessStageFragment(ctx, node, account, c, member, fragment); err != nil {
					t.Fatal("exact staged retry consumed additional capacity")
				}
			}
			var retained int
			if err := node.store.pool.QueryRow(ctx, `SELECT COALESCE(sum(octet_length(fragment_bytes)),0) FROM h_fragments WHERE account_id=$1 AND group_id=$2`, account, c.manifest.GroupID).Scan(&retained); err != nil || retained != want {
				t.Fatalf("compound staging changed retained work: got %d want %d", retained, want)
			}
			assertResources(t, node, c, want)
		}
	}
	var atLimit syncHarnessCompound
	for index, total := range []int{syncmodel.MaxCompoundBytes - 1, syncmodel.MaxCompoundBytes, syncmodel.MaxCompoundBytes + 1} {
		t.Run(fmt.Sprintf("bytes_%d", total), func(t *testing.T) {
			c := newSyncHarnessCompound(fmt.Sprintf("byte_bound_%d", index), byte(110+index))
			c.fragments = nil
			c.manifest.OrderedMembers = nil
			c.manifest.RequiredFragments = nil
			for remaining := total; remaining > 0; {
				size := min(remaining, syncmodel.MaxEnvelopeBytes)
				name := fmt.Sprintf("fragment_%02d", len(c.fragments))
				c.fragments = append(c.fragments, bytes.Repeat([]byte{'x'}, size))
				c.manifest.OrderedMembers = append(c.manifest.OrderedMembers, name)
				c.manifest.RequiredFragments = append(c.manifest.RequiredFragments, name)
				remaining -= size
			}
			c.manifest.ManifestCommitment = syncHarnessCompoundCommitment(c.manifest, c.fragments)
			c.envelope.Compound = &c.manifest
			stage(t, node, c)
			if total == syncmodel.MaxCompoundBytes {
				atLimit = c
			}
		})
	}
	if atLimit.manifest.GroupID == "" {
		t.Fatal("at-limit compound control did not complete")
	}
	var primaryPending, primaryBytes int
	if err := node.store.pool.QueryRow(ctx, `SELECT pending_operations,retained_bytes FROM h_resource_budgets WHERE account_id=$1`, syncHarnessAccount).Scan(&primaryPending, &primaryBytes); err != nil {
		t.Fatal("read primary compound budget failed")
	}
	t.Run("same_group_other_account", func(t *testing.T) {
		independent := atLimit
		independent.envelope.OriginalIncarnation.AccountID = "h_adversary_R"
		independent.scope = syncHarnessScope("h_adversary_R")
		independent.scope.Origin = independent.envelope.Origin
		independent.envelope.Authorization = independent.scope.Authorization
		independent.envelope.Compound = &independent.manifest
		stage(t, node, independent)
	})
	t.Run("same_group_other_node", func(t *testing.T) {
		other := top.nodes[0]
		seedSyncHarnessSharedAdmission(t, other, keys)
		independent := atLimit
		independent.envelope.Compound = &independent.manifest
		stage(t, other, independent)
	})
	var afterPending, afterBytes int
	if err := node.store.pool.QueryRow(ctx, `SELECT pending_operations,retained_bytes FROM h_resource_budgets WHERE account_id=$1`, syncHarnessAccount).Scan(&afterPending, &afterBytes); err != nil || afterPending != primaryPending || afterBytes != primaryBytes {
		t.Fatal("another account or node changed primary compound reservations")
	}
	assertResources(t, node, atLimit, syncmodel.MaxCompoundBytes)
	if !t.Failed() {
		t.Log("POSTGRES_PASS: SQL fragment staging admits compound payload N-1/N, refuses N+1 at 1 MiB under the compound row lock, preserves prior fragments, reconciles exact retry reservations, and isolates same-group staging across accounts and nodes")
	}
}

// The snapshot retains actual bytes and every reservation, so a capacity refusal
// cannot pass by preserving only the aggregate while moving another reservation.
type syncHarnessStagingFragmentSnapshot struct {
	memberIndex int
	commitment  []byte
	payload     []byte
}

type syncHarnessStagingReservationSnapshot struct {
	envelopeBytes int
	fragmentBytes int
	pending       bool
}

type syncHarnessStagingSnapshot struct {
	pending      int
	retained     int
	payloads     map[string][]byte
	reservations map[string]syncHarnessStagingReservationSnapshot
	fragments    map[string]syncHarnessStagingFragmentSnapshot
}

func syncHarnessSnapshotStaging(t *testing.T, node *syncHarnessNode, account string) syncHarnessStagingSnapshot {
	t.Helper()
	ctx := context.Background()
	s := syncHarnessStagingSnapshot{
		payloads:     make(map[string][]byte),
		reservations: make(map[string]syncHarnessStagingReservationSnapshot),
		fragments:    make(map[string]syncHarnessStagingFragmentSnapshot),
	}
	if err := node.store.pool.QueryRow(ctx, `SELECT pending_operations,retained_bytes FROM h_resource_budgets WHERE account_id=$1`, account).Scan(&s.pending, &s.retained); err != nil {
		t.Fatal("snapshot staging budget failed")
	}
	rows, err := node.store.pool.Query(ctx, `SELECT operation_id,canonical_semantic_envelope FROM h_operation_payloads WHERE account_id=$1`, account)
	if err != nil {
		t.Fatal("snapshot staging payloads failed")
	}
	for rows.Next() {
		var operation, payload []byte
		if err := rows.Scan(&operation, &payload); err != nil {
			rows.Close()
			t.Fatal("scan staging payload failed")
		}
		s.payloads[string(operation)] = payload
	}
	rows.Close()
	if rows.Err() != nil {
		t.Fatal("read staging payloads failed")
	}
	rows, err = node.store.pool.Query(ctx, `SELECT operation_id,envelope_bytes,fragment_bytes,pending FROM h_resource_reservations WHERE account_id=$1`, account)
	if err != nil {
		t.Fatal("snapshot staging reservations failed")
	}
	reservedTotal, reservedPending := 0, 0
	for rows.Next() {
		var operation []byte
		var r syncHarnessStagingReservationSnapshot
		if err := rows.Scan(&operation, &r.envelopeBytes, &r.fragmentBytes, &r.pending); err != nil {
			rows.Close()
			t.Fatal("scan staging reservation failed")
		}
		s.reservations[string(operation)] = r
		reservedTotal += r.envelopeBytes + r.fragmentBytes
		if r.pending {
			reservedPending++
		}
	}
	rows.Close()
	if rows.Err() != nil {
		t.Fatal("read staging reservations failed")
	}
	rows, err = node.store.pool.Query(ctx, `SELECT f.group_id,f.fragment_id,f.member_index,f.commitment,f.fragment_bytes,c.operation_id
FROM h_fragments f JOIN h_compounds c ON c.account_id=f.account_id AND c.group_id=f.group_id WHERE f.account_id=$1`, account)
	if err != nil {
		t.Fatal("snapshot staged fragments failed")
	}
	fragmentsByOperation := make(map[string]int)
	actualTotal := 0
	for rows.Next() {
		var group, fragment string
		var operation []byte
		var f syncHarnessStagingFragmentSnapshot
		if err := rows.Scan(&group, &fragment, &f.memberIndex, &f.commitment, &f.payload, &operation); err != nil {
			rows.Close()
			t.Fatal("scan staged fragment failed")
		}
		s.fragments[group+"/"+fragment] = f
		fragmentsByOperation[string(operation)] += len(f.payload)
		actualTotal += len(f.payload)
	}
	rows.Close()
	if rows.Err() != nil {
		t.Fatal("read staged fragments failed")
	}
	for operation, payload := range s.payloads {
		actualTotal += len(payload)
		r, exists := s.reservations[operation]
		if !exists || !r.pending || r.envelopeBytes != len(payload) || r.fragmentBytes != fragmentsByOperation[operation] {
			t.Fatal("staging reservation differs from its actual retained payload and fragments")
		}
	}
	if len(s.reservations) != len(s.payloads) || s.pending != len(s.payloads) || s.pending != reservedPending || s.retained != actualTotal || s.retained != reservedTotal {
		t.Fatal("staging budget does not reconcile with actual bytes and reservations")
	}
	return s
}

func syncHarnessSizedCompound(t *testing.T, suffix string, id byte, sizes []int) syncHarnessCompound {
	t.Helper()
	c := newSyncHarnessCompound(suffix, id)
	c.fragments = nil
	c.manifest.OrderedMembers = nil
	c.manifest.RequiredFragments = nil
	for index, size := range sizes {
		if size < 1 || size > syncmodel.MaxEnvelopeBytes {
			t.Fatal("invalid bounded staging fixture fragment size")
		}
		name := fmt.Sprintf("fragment_%02d", index)
		c.fragments = append(c.fragments, bytes.Repeat([]byte{byte('a' + index)}, size))
		c.manifest.OrderedMembers = append(c.manifest.OrderedMembers, name)
		c.manifest.RequiredFragments = append(c.manifest.RequiredFragments, name)
	}
	c.manifest.ManifestCommitment = syncHarnessCompoundCommitment(c.manifest, c.fragments)
	c.envelope.Compound = &c.manifest
	if err := c.manifest.Validate(); err != nil {
		t.Fatal("bounded staging fixture manifest invalid")
	}
	return c
}

func TestSyncHarnessCompoundSQLByteBoundsConcurrentStaging(t *testing.T) {
	top := newSyncHarnessTopology(t)
	node, keys := top.nodes[2], newSyncHarnessKeys()
	seedSyncHarnessAdmission(t, node, keys)
	sizes := make([]int, 18)
	for i := range 15 {
		sizes[i] = syncmodel.MaxEnvelopeBytes
	}
	sizes[15], sizes[16], sizes[17] = syncmodel.MaxEnvelopeBytes-1, 1, 1
	c := syncHarnessSizedCompound(t, "concurrent_byte_boundary", 161, sizes)
	seedSyncHarnessCompound(t, node, keys, c)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for index := range 16 {
		if err := syncHarnessStageFragment(ctx, node, syncHarnessAccount, c, index, c.fragments[index]); err != nil {
			t.Fatal("stage compound N-1 fixture failed")
		}
	}
	before := syncHarnessSnapshotStaging(t, node, syncHarnessAccount)
	if before.reservations[string(c.envelope.ID[:])].fragmentBytes != syncmodel.MaxCompoundBytes-1 {
		t.Fatal("compound concurrency fixture did not reach N-1")
	}

	// Named barrier: both actual adapter calls must be waiting on this fixture's
	// compound row before it is released. Polling observes locks, never chooses
	// an ordering by elapsed time; either one-byte fragment may win.
	compoundCapacityBarrier, err := node.store.pool.Begin(ctx)
	if err != nil {
		t.Fatal("begin compound capacity barrier failed")
	}
	defer func() { _ = compoundCapacityBarrier.Rollback(context.Background()) }()
	var barrierPID int32
	if err := compoundCapacityBarrier.QueryRow(ctx, `SELECT pg_backend_pid() FROM h_compounds WHERE account_id=$1 AND group_id=$2 FOR UPDATE`, syncHarnessAccount, c.manifest.GroupID).Scan(&barrierPID); err != nil {
		t.Fatal("hold compound capacity barrier failed")
	}
	type stagingResult struct {
		index int
		err   error
	}
	atCompoundCapacityBarrier := make(chan struct{})
	done := make(chan stagingResult, 2)
	for _, index := range []int{16, 17} {
		go func() {
			<-atCompoundCapacityBarrier
			done <- stagingResult{index, syncHarnessStageFragment(ctx, node, syncHarnessAccount, c, index, c.fragments[index])}
		}()
	}
	close(atCompoundCapacityBarrier)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		var waiting int
		if err := node.store.pool.QueryRow(ctx, `SELECT count(DISTINCT relation_lock.pid) FROM pg_locks relation_lock
JOIN pg_locks wait_lock ON wait_lock.pid=relation_lock.pid AND NOT wait_lock.granted
WHERE relation_lock.relation='h_compounds'::regclass AND relation_lock.locktype='relation'
  AND relation_lock.mode='RowShareLock' AND relation_lock.pid<>$1`, barrierPID).Scan(&waiting); err != nil {
			t.Fatal("observe compound capacity barrier failed")
		}
		if waiting == 2 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("two staging calls did not reach compound capacity barrier")
		case <-tick.C:
		}
	}
	if err := compoundCapacityBarrier.Commit(ctx); err != nil {
		t.Fatal("release compound capacity barrier failed")
	}
	winner, committed, refused := -1, 0, 0
	for range 2 {
		result := <-done
		switch {
		case result.err == nil:
			winner = result.index
			committed++
		case errors.Is(result.err, syncmodel.ErrResourceCapacity):
			refused++
		default:
			t.Fatal("staging contender returned neither commit nor explicit capacity refusal")
		}
	}
	if committed != 1 || refused != 1 {
		t.Fatal("compound byte boundary did not admit exactly one concurrent fragment")
	}
	// Change exactly the winning byte and its reservation in the expected
	// snapshot. All prior fragment and envelope bytes must remain identical.
	before.retained++
	reservation := before.reservations[string(c.envelope.ID[:])]
	reservation.fragmentBytes++
	before.reservations[string(c.envelope.ID[:])] = reservation
	commitment := sha256.Sum256(c.fragments[winner])
	before.fragments[c.manifest.GroupID+"/"+c.manifest.OrderedMembers[winner]] = syncHarnessStagingFragmentSnapshot{winner, commitment[:], c.fragments[winner]}
	if after := syncHarnessSnapshotStaging(t, node, syncHarnessAccount); !reflect.DeepEqual(after, before) {
		t.Fatal("concurrent staging changed prior fragments, payloads or unrelated reservations")
	}
	if err := syncHarnessStageFragment(ctx, node, syncHarnessAccount, c, winner, c.fragments[winner]); err != nil {
		t.Fatal("exact winning fragment retry consumed capacity")
	}
	loser := 16
	if winner == loser {
		loser = 17
	}
	if err := syncHarnessStageFragment(ctx, node, syncHarnessAccount, c, loser, c.fragments[loser]); !errors.Is(err, syncmodel.ErrResourceCapacity) {
		t.Fatal("refused fragment retry did not explicitly refuse capacity")
	}
	if after := syncHarnessSnapshotStaging(t, node, syncHarnessAccount); !reflect.DeepEqual(after, before) {
		t.Fatal("staging retries changed full boundary snapshot")
	}
	if !t.Failed() {
		t.Log("POSTGRES_PASS: compoundCapacityBarrier observed two fixture-owned lock waits at 1 MiB-1; one actual staging commit, one explicit capacity refusal, exact prior bytes and reservation reconciliation preserved")
	}
}

func TestSyncHarnessCompoundSQLStagingTransportBounds(t *testing.T) {
	top := newSyncHarnessTopology(t)
	node, keys := top.nodes[2], newSyncHarnessKeys()
	seedSyncHarnessAdmission(t, node, keys)
	ctx := context.Background()
	compounds := make([]syncHarnessCompound, 8)
	envelopeBytes := 0
	for index := range compounds {
		sizes := make([]int, 16)
		for member := range sizes {
			sizes[member] = syncmodel.MaxEnvelopeBytes
		}
		if index == len(compounds)-1 {
			sizes[15] -= 2
			sizes = append(sizes, 1, 1)
		}
		compounds[index] = syncHarnessSizedCompound(t, fmt.Sprintf("staging_transport_%d", index), byte(170+index), sizes)
		raw, err := compounds[index].envelope.CanonicalBytes()
		if err != nil {
			t.Fatal("encode staging transport fixture failed")
		}
		envelopeBytes += len(raw)
	}
	last := &compounds[len(compounds)-1]
	// The final compound crosses the global budget by one byte while its own
	// complete payload remains below 1 MiB. Fixed-size commitment encoding keeps
	// its envelope size unchanged when this synthetic fragment is shortened.
	last.fragments[15] = bytes.Repeat([]byte{'p'}, syncmodel.MaxEnvelopeBytes-envelopeBytes-1)
	last.manifest.ManifestCommitment = syncHarnessCompoundCommitment(last.manifest, last.fragments)
	last.envelope.Compound = &last.manifest
	for _, c := range compounds {
		completeBytes := 0
		for _, fragment := range c.fragments {
			completeBytes += len(fragment)
		}
		if completeBytes > syncmodel.MaxCompoundBytes {
			t.Fatal("transport fixture exceeds individual compound bound")
		}
		seedSyncHarnessCompound(t, node, keys, c)
	}
	initial := syncHarnessSnapshotStaging(t, node, syncHarnessAccount)
	if initial.retained != envelopeBytes {
		t.Fatal("staging transport envelope size changed during fixture construction")
	}
	for group, c := range compounds {
		for member, fragment := range c.fragments {
			if group == len(compounds)-1 && member >= 16 {
				break
			}
			if err := syncHarnessStageFragment(ctx, node, syncHarnessAccount, c, member, fragment); err != nil {
				t.Fatal("stage global transport N-1 fixture failed")
			}
		}
	}
	atNMinusOne := syncHarnessSnapshotStaging(t, node, syncHarnessAccount)
	if atNMinusOne.retained != syncmodel.MaxTransportBytes-1 {
		t.Fatal("fragment staging did not reach global transport N-1")
	}
	if err := syncHarnessStageFragment(ctx, node, syncHarnessAccount, *last, 16, last.fragments[16]); err != nil {
		t.Fatal("fragment staging refused global transport N")
	}
	atLimit := syncHarnessSnapshotStaging(t, node, syncHarnessAccount)
	if atLimit.retained != syncmodel.MaxTransportBytes {
		t.Fatal("fragment staging did not reach global transport N")
	}
	for range 2 {
		if err := syncHarnessStageFragment(ctx, node, syncHarnessAccount, *last, 17, last.fragments[17]); !errors.Is(err, syncmodel.ErrResourceCapacity) {
			t.Fatal("fragment staging did not explicitly refuse global transport N+1")
		}
		if after := syncHarnessSnapshotStaging(t, node, syncHarnessAccount); !reflect.DeepEqual(after, atLimit) {
			t.Fatal("global transport refusal changed retained fragments, payloads or reservations")
		}
	}
	if err := syncHarnessStageFragment(ctx, node, syncHarnessAccount, compounds[0], 0, compounds[0].fragments[0]); err != nil {
		t.Fatal("exact fragment retry at global transport capacity failed")
	}
	if after := syncHarnessSnapshotStaging(t, node, syncHarnessAccount); !reflect.DeepEqual(after, atLimit) {
		t.Fatal("global transport retry changed retained fragments, payloads or reservations")
	}
	if !t.Failed() {
		t.Log("POSTGRES_PASS: actual fragment staging reaches global 8 MiB N-1/N/N+1 across eight individually bounded compounds; explicit refusal and exact retry preserve all prior bytes and reconcile every reservation")
	}
}
