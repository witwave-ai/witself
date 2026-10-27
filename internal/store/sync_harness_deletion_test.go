package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/syncmodel"
)

func TestSyncHarnessPayloadRetirementCommitmentFloor(t *testing.T) {
	top := newSyncHarnessTopology(t)
	node := top.nodes[2]
	ctx := context.Background()
	keys := newSyncHarnessKeys()
	seedSyncHarnessAdmission(t, node, keys)
	envelope := syncHarnessEnvelope(syncHarnessAccount)
	original, _, err := syncHarnessAdmit(ctx, node, keys, envelope, syncHarnessScope(syncHarnessAccount), nil)
	if err != nil {
		t.Fatal(err)
	}
	deleted := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	floor := deleted.Add(37 * 24 * time.Hour)
	generation := syncmodel.GenerationID{3}
	deletionGeneration := syncmodel.GenerationID{4}
	insertKnowledge := `INSERT INTO h_deletion_knowledge(account_id,family,object_id,generation,deletion_generation,accepted_delete_id,issuer_stream,frontier,evidence_ref,deleted_at,retain_until) VALUES($1,'fixture','fixture_object',$2,$3,$4,'account',1,'fixture_evidence',$5,$6)`
	if _, err := node.store.pool.Exec(ctx, insertKnowledge, syncHarnessAccount, generation[:], deletionGeneration[:], envelope.ID[:], deleted, floor.Add(-time.Second)); err == nil {
		t.Fatal("deletion knowledge accepted below offline floor")
	}
	if _, err := node.store.pool.Exec(ctx, insertKnowledge, syncHarnessAccount, generation[:], deletionGeneration[:], envelope.ID[:], deleted, floor); err != nil {
		t.Fatal("persist deletion knowledge failed")
	}
	for _, kind := range []string{"identity", "evidence", "key"} {
		if _, err := node.store.pool.Exec(ctx, `INSERT INTO h_retained_evidence(account_id,evidence_kind,evidence_id,retain_until) VALUES($1,$2,'fixture_reference',$3)`, syncHarnessAccount, kind, floor); err != nil {
			t.Fatal("retain fixture evidence failed")
		}
		if _, err := node.store.pool.Exec(ctx, `INSERT INTO h_evidence_pins(account_id,evidence_kind,evidence_id,pin_owner,reason) VALUES($1,$2,'fixture_reference','unresolved_operation','retry')`, syncHarnessAccount, kind); err != nil {
			t.Fatal("pin unresolved evidence failed")
		}
		var collected bool
		if err := node.store.pool.QueryRow(ctx, `SELECT h_collect_evidence($1,$2,'fixture_reference',$3)`, syncHarnessAccount, kind, floor.Add(400*24*time.Hour)).Scan(&collected); err != nil || collected {
			t.Fatal("retention collected unresolved referenced evidence")
		}
	}
	if _, err := node.store.pool.Exec(ctx, `INSERT INTO h_evidence_pins(account_id,evidence_kind,evidence_id,pin_owner,reason) VALUES('h_adversary_R','key','fixture_reference','wrong_account','retry')`); err == nil {
		t.Fatal("evidence pin crossed account scope")
	}
	// The governed path is distinct from ordinary immutable-payload DELETE.
	if _, err := node.store.pool.Exec(ctx, `DELETE FROM h_operation_payloads WHERE account_id=$1`, syncHarnessAccount); err == nil {
		t.Fatal("ungoverned payload deletion succeeded")
	}
	shortFloor, err := node.store.pool.Begin(ctx)
	if err != nil {
		t.Fatal("begin insufficient retirement floor failed")
	}
	if _, err := shortFloor.Exec(ctx, `INSERT INTO h_payload_retirements(account_id,operation_id,retired_at) VALUES($1,$2,'2026-12-31')`, syncHarnessAccount, envelope.ID[:]); err != nil {
		_ = shortFloor.Rollback(ctx)
		t.Fatal("stage insufficient retirement floor failed")
	}
	_, floorErr := shortFloor.Exec(ctx, `DELETE FROM h_operation_payloads WHERE account_id=$1 AND operation_id=$2`, syncHarnessAccount, envelope.ID[:])
	_ = shortFloor.Rollback(ctx)
	if floorErr == nil || syncHarnessCount(t, node, "h_operation_payloads") != 1 || syncHarnessCount(t, node, "h_payload_retirements") != 0 {
		t.Fatal("insufficient retirement floor did not preserve payload atomically")
	}
	tx, err := node.store.pool.Begin(ctx)
	if err != nil {
		t.Fatal("begin payload retirement failed")
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `INSERT INTO h_payload_retirements(account_id,operation_id,retired_at) VALUES($1,$2,$3)`, syncHarnessAccount, envelope.ID[:], deleted); err != nil {
		t.Fatal("record governed retirement failed")
	}
	if _, err := tx.Exec(ctx, `DELETE FROM h_operation_payloads WHERE account_id=$1 AND operation_id=$2`, syncHarnessAccount, envelope.ID[:]); err != nil {
		t.Fatal("retire payload failed")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal("commit payload retirement failed")
	}
	// Current key rotation must still select the original stored comparison key.
	keys.current = "comparison_new"
	retried, fresh, err := syncHarnessAdmit(ctx, node, keys, envelope, syncHarnessScope(syncHarnessAccount), nil)
	if err != nil || fresh {
		t.Fatal("retired original retry failed")
	}
	syncHarnessSameReceipt(t, original, retried)
	changed := envelope
	changed.Payload = syncmodel.StringValue("different synthetic payload")
	refusal, _, err := syncHarnessAdmit(ctx, node, keys, changed, syncHarnessScope(syncHarnessAccount), nil)
	if err != nil || refusal.Reason != syncmodel.ReasonOperationIDReuse {
		t.Fatal("retired input reuse did not conflict")
	}
	delete(keys.comparison[syncHarnessAccount], "comparison_old")
	if _, _, err := syncHarnessAdmit(ctx, node, keys, envelope, syncHarnessScope(syncHarnessAccount), nil); err == nil {
		t.Fatal("missing original comparison capability accepted retry")
	}
	var payloads, pins, knowledge int
	if err := node.store.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM h_operation_payloads),(SELECT count(*) FROM h_evidence_pins WHERE released_at IS NULL),(SELECT count(*) FROM h_deletion_knowledge)`).Scan(&payloads, &pins, &knowledge); err != nil || payloads != 0 || pins != 3 || knowledge != 1 {
		t.Fatal("retirement discarded evidence or retained payload")
	}
	var stored []byte
	var retainUntil time.Time
	if err := node.store.pool.QueryRow(ctx, `SELECT input_commitment,retain_until FROM h_receipts WHERE account_id=$1 AND operation_id=$2`, syncHarnessAccount, envelope.ID[:]).Scan(&stored, &retainUntil); err != nil || !bytes.Equal(stored, original.InputCommitment[:]) || retainUntil.Before(floor) {
		t.Fatal("retirement lost commitment floor")
	}
	// Inspect value-free records as bytes but never emit them in diagnostics.
	for _, table := range []string{"h_operations", "h_receipts", "h_reuse_refusals", "h_deletion_knowledge", "h_evidence_pins", "h_retained_evidence", "h_payload_retirements"} {
		var leaked bool
		if err := node.store.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+table+` r WHERE strpos(row_to_json(r)::text,'synthetic payload')>0)`).Scan(&leaked); err != nil || leaked {
			t.Fatal("value-free retained row contains payload")
		}
	}
	// bytea is rendered as hex by row_to_json, so inspect the byte columns
	// independently. Never put these bytes or their hashes in a failure message.
	payload := []byte("synthetic payload")
	unkeyedPayload := sha256.Sum256(payload)
	canonicalEnvelope, err := envelope.CanonicalBytes()
	if err != nil {
		t.Fatal("encode leak-check fixture failed")
	}
	unkeyedEnvelope := sha256.Sum256(canonicalEnvelope)
	for table, columns := range map[string][]string{
		"h_operations":     {"input_commitment"},
		"h_receipts":       {"input_commitment", "result_commitment", "causal_result", "canonical_body", "signed_bytes"},
		"h_reuse_refusals": {"conflicting_commitment", "canonical_body", "signed_bytes"},
	} {
		for _, column := range columns {
			rows, err := node.store.pool.Query(ctx, `SELECT `+column+` FROM `+table)
			if err != nil {
				t.Fatal("read value-free binary evidence failed")
			}
			for rows.Next() {
				var value []byte
				if err := rows.Scan(&value); err != nil {
					rows.Close()
					t.Fatal("decode binary evidence failed")
				}
				if bytes.Contains(value, payload) || bytes.Contains(value, unkeyedPayload[:]) || bytes.Contains(value, unkeyedEnvelope[:]) {
					rows.Close()
					t.Fatal("retained binary evidence contains payload or unkeyed input hash")
				}
			}
			rowErr := rows.Err()
			rows.Close()
			if rowErr != nil {
				t.Fatal("inspect binary evidence failed")
			}
		}
	}
	// Only the keyed comparison commitment remains; no extra ciphertext/hash copy.
	var forbidden int
	if err := node.store.pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema=current_schema() AND table_name IN ('h_operations','h_receipts','h_reuse_refusals','h_deletion_knowledge','h_evidence_pins') AND (column_name LIKE '%ciphertext%' OR column_name LIKE '%hash%' OR column_name LIKE '%payload%')`).Scan(&forbidden); err != nil || forbidden != 0 {
		t.Fatal("dedup evidence includes forbidden retained content")
	}
	// Explicit release permits collection only after its time floor.
	if _, err := node.store.pool.Exec(ctx, `UPDATE h_evidence_pins SET released_at=$1 WHERE account_id=$2`, floor, syncHarnessAccount); err != nil {
		t.Fatal("release fixture pins failed")
	}
	for _, kind := range []string{"identity", "evidence", "key"} {
		var collected bool
		if err := node.store.pool.QueryRow(ctx, `SELECT h_collect_evidence($1,$2,'fixture_reference',$3)`, syncHarnessAccount, kind, floor.Add(-time.Second)).Scan(&collected); err != nil || collected {
			t.Fatal("released evidence collected below floor")
		}
		if err := node.store.pool.QueryRow(ctx, `SELECT h_collect_evidence($1,$2,'fixture_reference',$3)`, syncHarnessAccount, kind, floor).Scan(&collected); err != nil || !collected {
			t.Fatal("released expired evidence did not collect")
		}
	}
	if !t.Failed() {
		t.Log("POSTGRES_PASS: payload retirement preserves keyed comparison, exact receipt, deletion floor and unresolved identity/evidence/key pins; missing old key refuses")
	}
}
