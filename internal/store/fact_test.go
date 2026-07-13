package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
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

func TestNormalizeSetFactInputAnnualRecurrence(t *testing.T) {
	in, err := normalizeSetFactInput(SetFactInput{
		Predicate:  "identity/birth-date",
		ValueType:  "date",
		Value:      json.RawMessage(`"1990-07-13"`),
		Recurrence: FactRecurrenceAnnual,
	})
	if err != nil {
		t.Fatal(err)
	}
	if in.Recurrence != FactRecurrenceAnnual {
		t.Fatalf("recurrence = %q", in.Recurrence)
	}

	for _, invalid := range []SetFactInput{
		{Predicate: "identity/birth-date", ValueType: "date", Value: json.RawMessage(`"1990-07-13"`), Recurrence: "monthly"},
		{Predicate: "schedule/appointment", ValueType: "datetime", Value: json.RawMessage(`"2026-07-13T18:00:00Z"`), Recurrence: FactRecurrenceAnnual},
	} {
		if _, err := normalizeSetFactInput(invalid); !errors.Is(err, ErrFactInputInvalid) {
			t.Errorf("normalizeSetFactInput(%#v) error = %v, want ErrFactInputInvalid", invalid, err)
		}
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

func TestNormalizeSetFactInputValidatesPrimitiveValueTypes(t *testing.T) {
	tests := []struct {
		valueType string
		value     string
	}{
		{valueType: "string", value: `"x"`},
		{valueType: "number", value: `42.5`},
		{valueType: "boolean", value: `true`},
		{valueType: "list", value: `[1, 2]`},
		{valueType: "object", value: `{ "city": "Denver" }`},
		{valueType: "json", value: `null`},
	}
	for _, tt := range tests {
		t.Run(tt.valueType, func(t *testing.T) {
			in, err := normalizeSetFactInput(SetFactInput{
				Predicate: "test/value", ValueType: tt.valueType,
				Value: json.RawMessage(tt.value),
			})
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(in.Value, []byte(" ")) {
				t.Fatalf("value was not compacted: %s", in.Value)
			}
		})
	}

	for _, tt := range []struct {
		valueType string
		value     string
	}{
		{valueType: "string", value: `123`},
		{valueType: "number", value: `"123"`},
		{valueType: "boolean", value: `0`},
		{valueType: "list", value: `{}`},
		{valueType: "object", value: `[]`},
	} {
		t.Run(tt.valueType+"_rejects_wrong_shape", func(t *testing.T) {
			_, err := normalizeSetFactInput(SetFactInput{
				Predicate: "test/value", ValueType: tt.valueType,
				Value: json.RawMessage(tt.value),
			})
			if !errors.Is(err, ErrFactInputInvalid) {
				t.Fatalf("error = %v, want ErrFactInputInvalid", err)
			}
		})
	}
}

func TestNormalizeSetFactInputNormalizesLogicalValueTypes(t *testing.T) {
	tests := []struct {
		name      string
		valueType string
		value     string
		want      string
	}{
		{name: "date", valueType: "date", value: `"2026-07-12"`, want: `"2026-07-12"`},
		{name: "datetime", valueType: "datetime", value: `"2026-07-12T12:30:00-06:00"`, want: `"2026-07-12T18:30:00Z"`},
		{name: "url", valueType: "url", value: `" HTTPS://Example.COM/a?q=1 "`, want: `"https://example.com/a?q=1"`},
		{name: "email", valueType: "email", value: `"Scott@EXAMPLE.COM"`, want: `"Scott@example.com"`},
		{name: "address text", valueType: "address", value: `" 123 Main St "`, want: `"123 Main St"`},
		{name: "address object", valueType: "address", value: `{ "city": "Denver", "region": "CO" }`, want: `{"city":"Denver","region":"CO"}`},
		{name: "location text", valueType: "location", value: `" Denver, CO "`, want: `"Denver, CO"`},
		{name: "location object", valueType: "location", value: `{ "latitude": 39.7392, "longitude": -104.9903 }`, want: `{"latitude":39.7392,"longitude":-104.9903}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in, err := normalizeSetFactInput(SetFactInput{
				Predicate: "test/value", ValueType: tt.valueType,
				Value: json.RawMessage(tt.value),
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := string(in.Value); got != tt.want {
				t.Fatalf("value = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestNormalizeSetFactInputRejectsInvalidLogicalValues(t *testing.T) {
	for _, tt := range []struct {
		valueType string
		value     string
	}{
		{valueType: "date", value: `"07/12/2026"`},
		{valueType: "datetime", value: `"2026-07-12 18:30:00"`},
		{valueType: "url", value: `"example.com/path"`},
		{valueType: "url", value: `"file:///tmp/private"`},
		{valueType: "email", value: `"Scott <scott@example.com>"`},
		{valueType: "address", value: `"  "`},
		{valueType: "address", value: `{}`},
		{valueType: "location", value: `[]`},
		{valueType: "location", value: `{}`},
	} {
		t.Run(tt.valueType+"_"+tt.value, func(t *testing.T) {
			_, err := normalizeSetFactInput(SetFactInput{
				Predicate: "test/value", ValueType: tt.valueType,
				Value: json.RawMessage(tt.value),
			})
			if !errors.Is(err, ErrFactInputInvalid) {
				t.Fatalf("error = %v, want ErrFactInputInvalid", err)
			}
		})
	}
}

func TestNormalizeSetFactInputPreservesExtensibleTypes(t *testing.T) {
	in, err := normalizeSetFactInput(SetFactInput{
		Predicate: "test/value", ValueType: "custom.identifier",
		Value: json.RawMessage(` { "opaque": true } `),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(in.Value), `{"opaque":true}`; got != want {
		t.Fatalf("value = %s, want %s", got, want)
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

func TestNormalizeFactSubjectAlias(t *testing.T) {
	if got := normalizeFactSubjectAlias("  My   SPouse\t"); got != "my spouse" {
		t.Fatalf("normalizeFactSubjectAlias = %q", got)
	}
	for _, alias := range []string{"my spouse", "Scott's project", "父"} {
		if !validFactSubjectAlias(alias) {
			t.Errorf("validFactSubjectAlias(%q) = false", alias)
		}
	}
	for _, alias := range []string{"", "bad\x00alias", strings.Repeat("a", maxFactSubjectAliasBytes+1)} {
		if validFactSubjectAlias(alias) {
			t.Errorf("validFactSubjectAlias(%q) = true", alias)
		}
	}
}

func TestFactSubjectBuiltInAliases(t *testing.T) {
	for _, subject := range []string{"", "me", "MYSELF", " user "} {
		if got := normalizeFactSubject(subject); got != "self" {
			t.Errorf("normalizeFactSubject(%q) = %q, want self", subject, got)
		}
	}
}
