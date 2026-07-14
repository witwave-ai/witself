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
