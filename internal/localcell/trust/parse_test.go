package trust

import (
	"bytes"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

func TestStrictProfilePublicBoundary(t *testing.T) {
	cases := []struct {
		name, raw string
		want      Reason
	}{
		{"duplicate_top", `{"a":1,"a":2}`, ReasonDuplicateKey},
		{"duplicate_nested", `{"a":{"b":1,"b":2}}`, ReasonDuplicateKey},
		{"duplicate_escaped", `{"a":1,"\u0061":2}`, ReasonDuplicateKey},
		{"invalid_utf8", string([]byte{'{', '"', 'a', '"', ':', '"', 255, '"', '}'}), ReasonProfileViolation},
		{"unpaired_high", `{"a":"\ud800"}`, ReasonProfileViolation},
		{"unpaired_low", `{"a":"\udc00"}`, ReasonProfileViolation},
		{"long_string", `{"a":"` + strings.Repeat("x", 4097) + `"}`, ReasonProfileViolation},
		{"long_key", `{"` + strings.Repeat("x", 4097) + `":1}`, ReasonProfileViolation},
		{"depth_n_plus_one", `{"a":` + strings.Repeat("[", 8) + `0` + strings.Repeat("]", 8) + `}`, ReasonProfileViolation},
		{"trailing_value", `{} {}`, ReasonInvalidJSON},
		{"top_array", `[]`, ReasonProfileViolation},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := StrictProfile([]byte(tc.raw)); !errors.Is(err, tc.want) {
				t.Fatalf("profile reason: %v", err)
			}
		})
	}
	for _, raw := range []string{`{"a":"\ud83d\ude00"}`, `{"a":"` + strings.Repeat("x", 4096) + `"}`, `{"a":` + strings.Repeat("[", 7) + `0` + strings.Repeat("]", 7) + `}`, `{"a":"\\ud800"}`} {
		if err := StrictProfile([]byte(raw)); err != nil {
			t.Fatalf("valid profile refused: %v", err)
		}
	}
}

func TestStrictSchemaForwardCompatibilityAndRequiredFields(t *testing.T) {
	type nested struct {
		Count uint64 `json:"count"`
	}
	type document struct {
		FormatVersion int      `json:"format_version"`
		Critical      []string `json:"critical"`
		Nested        nested   `json:"nested"`
	}
	valid := []byte(`{"format_version":1,"critical":[],"nested":{"count":1},"future":{"opaque":true}}`)
	var out document
	if err := parseDocument(valid, MaxComponentSetBytes, &out, true); err != nil {
		t.Fatalf("forward compatible field: %v", err)
	}
	cases := []struct {
		name string
		raw  []byte
		want Reason
	}{
		{"unknown_critical", bytes.Replace(valid, []byte(`"critical":[]`), []byte(`"critical":["future"]`), 1), ReasonUnknownCritical},
		{"unknown_nested", bytes.Replace(valid, []byte(`"count":1`), []byte(`"count":1,"future":true`), 1), ReasonUnknownField},
		{"float", bytes.Replace(valid, []byte(`"count":1`), []byte(`"count":1.0`), 1), ReasonProfileViolation},
		{"exponent", bytes.Replace(valid, []byte(`"count":1`), []byte(`"count":1e0`), 1), ReasonProfileViolation},
		{"negative", bytes.Replace(valid, []byte(`"count":1`), []byte(`"count":-1`), 1), ReasonProfileViolation},
		{"overflow", bytes.Replace(valid, []byte(`"count":1`), []byte(`"count":18446744073709551616`), 1), ReasonProfileViolation},
		{"null_array", bytes.Replace(valid, []byte(`"critical":[]`), []byte(`"critical":null`), 1), ReasonInvalidDocument},
		{"missing", bytes.Replace(valid, []byte(`"count":1`), nil, 1), ReasonInvalidDocument},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := parseDocument(tc.raw, MaxComponentSetBytes, &out, true); !errors.Is(err, tc.want) {
				t.Fatalf("schema reason: %v", err)
			}
		})
	}
}

func TestTrustStandardLibraryDependencyBoundary(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", "-json", ".")
	raw, err := cmd.Output()
	if err != nil {
		t.Fatal("dependency listing failed")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	count := 0
	for dec.More() {
		var dependency struct {
			ImportPath string
			Standard   bool
		}
		if dec.Decode(&dependency) != nil {
			t.Fatal("dependency listing invalid")
		}
		count++
		if !dependency.Standard && dependency.ImportPath != "github.com/witwave-ai/witself/internal/localcell/trust" {
			t.Fatal("nonstandard dependency entered trust package")
		}
	}
	if count == 0 {
		t.Fatal("empty dependency listing")
	}
}
