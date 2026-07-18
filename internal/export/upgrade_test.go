package export

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func TestSchema25FactIdempotencyUpgrade(t *testing.T) {
	upgrade := UpgraderFor(25)
	if upgrade == nil {
		t.Fatal("schema 25 upgrader is not registered")
	}
	assertion, err := upgrade("fact_assertions", map[string]any{"id": "fas_1"})
	if err != nil {
		t.Fatal(err)
	}
	if assertion["idempotency_key"] != "" || assertion["idempotency_fingerprint"] != "" {
		t.Fatalf("assertion defaults = %#v", assertion)
	}
	candidate, err := upgrade("fact_candidates", map[string]any{"id": "fcand_1"})
	if err != nil {
		t.Fatal(err)
	}
	if candidate["idempotency_key"] != "" || candidate["idempotency_fingerprint"] != "" || candidate["decision_idempotency_key"] != "" || candidate["decision_assertion_id"] != nil {
		t.Fatalf("candidate defaults = %#v", candidate)
	}
	other, err := upgrade("agents", map[string]any{"id": "agent_1"})
	if err != nil || len(other) != 1 || other["id"] != "agent_1" {
		t.Fatalf("unrelated row = %#v / %v", other, err)
	}
}

func TestSchema26FactDeletionUpgrade(t *testing.T) {
	upgrade := UpgraderFor(26)
	if upgrade == nil {
		t.Fatal("schema 26 upgrader is not registered")
	}
	fact, err := upgrade("facts", map[string]any{
		"id": "fact_1", "resolved_assertion_id": "fas_1",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"delete_receipt_id", "delete_idempotency_key_hash",
		"deleted_prior_assertion_id", "deleted_candidate_revision",
	} {
		if fact[key] != "" {
			t.Fatalf("%s = %#v, want empty", key, fact[key])
		}
	}
	for _, key := range []string{"deleted_assertion_count", "deleted_candidate_count", "deleted_usage_count", "deleted_mutation_key_count"} {
		if fact[key] != 0 {
			t.Fatalf("%s = %#v, want zero", key, fact[key])
		}
	}
	for _, key := range []string{"deleted_at", "deleted_by_agent_id", "recreated_at", "replacement_fact_id"} {
		if fact[key] != nil {
			t.Fatalf("%s = %#v, want nil", key, fact[key])
		}
	}
	other, err := upgrade("fact_assertions", map[string]any{"id": "fas_1"})
	if err != nil || len(other) != 1 {
		t.Fatalf("unrelated row = %#v / %v", other, err)
	}
}

func TestSchema27FactDeletionActivationPreservesRows(t *testing.T) {
	upgrade := UpgraderFor(27)
	if upgrade == nil {
		t.Fatal("schema 27 identity upgrader is not registered")
	}
	input := map[string]any{
		"id":                         "fact_1",
		"deleted_at":                 "2026-07-14T00:00:00Z",
		"deleted_candidate_revision": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"deleted_assertion_count":    json.Number("9007199254740993"),
	}
	want := map[string]any{
		"id":                         "fact_1",
		"deleted_at":                 "2026-07-14T00:00:00Z",
		"deleted_candidate_revision": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"deleted_assertion_count":    json.Number("9007199254740993"),
	}
	got, err := upgrade("facts", input)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("schema 27 identity upgrade changed row: got %#v, want %#v", got, want)
	}
	other, err := upgrade("agents", map[string]any{"id": "agent_1"})
	if err != nil || !reflect.DeepEqual(other, map[string]any{"id": "agent_1"}) {
		t.Fatalf("schema 27 identity upgrade changed unrelated row: %#v / %v", other, err)
	}
}

func TestSchema28NarrativeMemoryUpgradePreservesRows(t *testing.T) {
	upgrade := UpgraderFor(28)
	if upgrade == nil {
		t.Fatal("schema 28 identity upgrader is not registered")
	}
	input := map[string]any{
		"id":            "transcript_1",
		"account_id":    "acc_1",
		"next_sequence": json.Number("9007199254740993"),
	}
	want := map[string]any{
		"id":            "transcript_1",
		"account_id":    "acc_1",
		"next_sequence": json.Number("9007199254740993"),
	}
	got, err := upgrade("transcript_conversations", input)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("schema 28 identity upgrade changed row: got %#v, want %#v", got, want)
	}
}

func TestSchema29MemoryCurationUpgradeAddsExistingTableDefaults(t *testing.T) {
	upgrade := UpgraderFor(29)
	if upgrade == nil {
		t.Fatal("schema 29 curation upgrader is not registered")
	}
	candidate, err := upgrade("fact_candidates", map[string]any{"id": "fcand_1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"curation_run_id", "curation_action_id"} {
		if candidate[field] != nil {
			t.Fatalf("%s = %#v, want nil", field, candidate[field])
		}
	}
	for _, field := range []string{"withdrawal_reason", "withdrawal_idempotency_key", "withdrawal_request_hash"} {
		if candidate[field] != "" {
			t.Fatalf("%s = %#v, want empty", field, candidate[field])
		}
	}
	relation, err := upgrade("memory_relations", map[string]any{"id": "mrel_1"})
	if err != nil || relation["reverted_by_action_id"] != nil {
		t.Fatalf("relation defaults = %#v / %v", relation, err)
	}
}

func TestSchema30TokenProfileUpgradeDefaultsToFull(t *testing.T) {
	upgrade := UpgraderFor(30)
	if upgrade == nil {
		t.Fatal("schema 30 token-profile upgrader is not registered")
	}
	token, err := upgrade("tokens", map[string]any{"id": "tok_1", "kind": "agent"})
	if err != nil {
		t.Fatal(err)
	}
	if token["access_profile"] != "full" {
		t.Fatalf("access_profile = %#v, want full", token["access_profile"])
	}
	other, err := upgrade("agents", map[string]any{"id": "agent_1"})
	if err != nil || len(other) != 1 || other["id"] != "agent_1" {
		t.Fatalf("unrelated row = %#v / %v", other, err)
	}
}

func TestSchema32MessageReplyUpgradePreservesRows(t *testing.T) {
	upgrade := UpgraderFor(32)
	if upgrade == nil {
		t.Fatal("schema 32 reply-causality upgrader is not registered")
	}
	input := map[string]any{
		"id":            "msg_1",
		"account_id":    "acc_1",
		"realm_id":      "rlm_1",
		"from_agent_id": "agt_1",
		"to_agent_id":   "agt_2",
		"thread_id":     "thr_1",
	}
	want := map[string]any{
		"id":            "msg_1",
		"account_id":    "acc_1",
		"realm_id":      "rlm_1",
		"from_agent_id": "agt_1",
		"to_agent_id":   "agt_2",
		"thread_id":     "thr_1",
	}
	got, err := upgrade("agent_messages", input)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("schema 32 identity upgrade changed message: got %#v, want %#v", got, want)
	}
}

func TestSchema33MessageProcessingUpgradeDefaultsAvailable(t *testing.T) {
	upgrade := UpgraderFor(33)
	if upgrade == nil {
		t.Fatal("schema 33 message-processing upgrader is not registered")
	}
	row, err := upgrade("agent_message_deliveries", map[string]any{
		"message_id": "msg_1", "recipient_agent_id": "agt_1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if row["processing_state"] != "available" || row["processing_generation"] != 0 ||
		row["claim_id"] != nil || row["claim_key_hash"] != "" ||
		row["lease_expires_at"] != nil || row["completed_at"] != nil ||
		row["complete_key_hash"] != "" || row["result_message_id"] != nil {
		t.Fatalf("schema 33 delivery defaults = %#v", row)
	}
	other, err := upgrade("agent_messages", map[string]any{"id": "msg_1"})
	if err != nil || len(other) != 1 || other["id"] != "msg_1" {
		t.Fatalf("unrelated row = %#v / %v", other, err)
	}
}

func TestSchema34MessageCausalDepthUpgradeDefaultsForGraphNormalization(t *testing.T) {
	upgrade := UpgraderFor(34)
	if upgrade == nil {
		t.Fatal("schema 34 message-causal-depth upgrader is not registered")
	}
	row, err := upgrade("agent_messages", map[string]any{
		"id": "msg_reply", "reply_to_message_id": "msg_parent",
	})
	if err != nil {
		t.Fatal(err)
	}
	if row["causal_depth"] != 1 {
		t.Fatalf("legacy causal depth placeholder = %#v, want 1", row["causal_depth"])
	}
	other, err := upgrade("agent_message_deliveries", map[string]any{"message_id": "msg_reply"})
	if err != nil || len(other) != 1 {
		t.Fatalf("unrelated row = %#v / %v", other, err)
	}
}

func TestSchema35MessageFailureCountUpgradeDefaultsToZero(t *testing.T) {
	upgrade := UpgraderFor(35)
	if upgrade == nil {
		t.Fatal("schema 35 message-failure-count upgrader is not registered")
	}
	row, err := upgrade("agent_message_deliveries", map[string]any{
		"message_id": "msg_1", "processing_generation": 9,
	})
	if err != nil {
		t.Fatal(err)
	}
	if row["failure_count"] != 0 || row["processing_generation"] != 9 {
		t.Fatalf("schema 35 delivery defaults = %#v", row)
	}
	other, err := upgrade("agent_messages", map[string]any{"id": "msg_1"})
	if err != nil || len(other) != 1 || other["id"] != "msg_1" {
		t.Fatalf("unrelated row = %#v / %v", other, err)
	}
}

func TestSchema36MessageAudienceUpgradeDefaultsToDirect(t *testing.T) {
	upgrade := UpgraderFor(36)
	if upgrade == nil {
		t.Fatal("schema 36 message-audience upgrader is not registered")
	}
	row, err := upgrade("agent_messages", map[string]any{
		"id": "msg_1", "to_agent_id": "agent_2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if row["audience_kind"] != "agent" || row["audience_fingerprint"] != "" ||
		row["to_agent_id"] != "agent_2" {
		t.Fatalf("schema 36 audience defaults = %#v", row)
	}
	other, err := upgrade("agent_message_deliveries", map[string]any{"message_id": "msg_1"})
	if err != nil || len(other) != 1 {
		t.Fatalf("unrelated row = %#v / %v", other, err)
	}
}

func TestSchema49AvatarUpgradePreservesExistingRows(t *testing.T) {
	upgrade := UpgraderFor(49)
	if upgrade == nil {
		t.Fatal("schema 49 avatar upgrader is not registered")
	}
	row := map[string]any{"id": "agent_1", "name": "atlas"}
	got, err := upgrade("agents", row)
	if err != nil {
		t.Fatal(err)
	}
	if got["id"] != "agent_1" || got["name"] != "atlas" || len(got) != 2 {
		t.Fatalf("avatar upgrader changed a pre-avatar row: %#v", got)
	}
}

func TestUpgradeRowPreservesLargeIntegers(t *testing.T) {
	const exact = "9007199254740993"
	upgraded, err := upgradeRow("agents", []byte(`{"id":"agent_1","sequence":`+exact+`}`), 25, 26)
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(upgraded))
	decoder.UseNumber()
	var row map[string]any
	if err := decoder.Decode(&row); err != nil {
		t.Fatal(err)
	}
	if got := row["sequence"].(json.Number).String(); got != exact {
		t.Fatalf("large integer changed during upgrade: got %s, want %s", got, exact)
	}
}

func TestUpgradeRowRejectsTrailingJSON(t *testing.T) {
	_, err := upgradeRow("agents", []byte(`{"id":"agent_1"}{"id":"agent_2"}`), 25, 26)
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("trailing JSON error = %v, want ErrCorrupt", err)
	}
}

func TestUpgradeRowRejectsNull(t *testing.T) {
	_, err := upgradeRow("fact_assertions", []byte(`null`), 25, 26)
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("null row error = %v, want ErrCorrupt", err)
	}
}
