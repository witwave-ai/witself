package store

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestNormalizeSetFactInput(t *testing.T) {
	in, err := normalizeSetFactInput(SetFactInput{
		Subject:   "me",
		Predicate: "preferences/editor",
		Value:     json.RawMessage(`"vim"`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if in.Subject != "self" || in.ValueType != "string" ||
		in.Cardinality != FactCardinalityOne || in.SourceKind != FactSourceSelf ||
		in.ObservedAt.IsZero() {
		t.Fatalf("normalized input = %#v", in)
	}
}

func TestNormalizeSetFactInputRejectsInvalidShapes(t *testing.T) {
	from := time.Now()
	until := from.Add(-time.Hour)
	confidence := 1.1
	tests := []SetFactInput{
		{Predicate: "Bad Predicate", Value: json.RawMessage(`"x"`)},
		{Predicate: "ok", Value: json.RawMessage(`not-json`)},
		{Predicate: "ok", Value: json.RawMessage(`"x"`), Cardinality: "sometimes"},
		{Predicate: "ok", Value: json.RawMessage(`"x"`), SourceKind: "guessed"},
		{Predicate: "ok", Value: json.RawMessage(`"x"`), Confidence: &confidence},
		{Predicate: "ok", Value: json.RawMessage(`"x"`), ValidFrom: &from, ValidUntil: &until},
	}
	for i, input := range tests {
		if _, err := normalizeSetFactInput(input); !errors.Is(err, ErrFactInputInvalid) {
			t.Errorf("case %d error = %v, want ErrFactInputInvalid", i, err)
		}
	}
}

func TestInferFactValueType(t *testing.T) {
	tests := map[string]string{
		`"x"`:               "string",
		`42`:                "number",
		`true`:              "boolean",
		`[1,2]`:             "list",
		`{"city":"Denver"}`: "object",
		`null`:              "json",
	}
	for raw, want := range tests {
		if got := inferFactValueType(json.RawMessage(raw)); got != want {
			t.Errorf("inferFactValueType(%s) = %q, want %q", raw, got, want)
		}
	}
}

func TestFactPredicateShape(t *testing.T) {
	for _, predicate := range []string{"identity/birth-date", "resources/witself/repository", "custom.value"} {
		if !validFactPredicate(predicate) {
			t.Errorf("validFactPredicate(%q) = false", predicate)
		}
	}
	for _, predicate := range []string{"", "/identity", "identity/", "identity//name", "Identity/name", "has space"} {
		if validFactPredicate(predicate) {
			t.Errorf("validFactPredicate(%q) = true", predicate)
		}
	}
}
