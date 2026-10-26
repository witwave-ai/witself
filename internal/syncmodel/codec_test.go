package syncmodel

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

type sourceValue struct {
	TrailingZeros int             `json:"trailing_zeros"`
	Kind          string          `json:"kind"`
	Value         json.RawMessage `json:"value"`
	Bytes         []int           `json:"bytes"`
	Items         []sourceValue   `json:"items"`
	Fields        []sourceField   `json:"fields"`
	Tag           DomainTag       `json:"tag"`
}
type sourceField struct {
	Name  string      `json:"name"`
	Value sourceValue `json:"value"`
}
type vectorFile struct {
	Version int `json:"version"`
	Valid   []struct {
		Name      string      `json:"name"`
		Source    sourceValue `json:"source"`
		Canonical string      `json:"canonical"`
	} `json:"valid"`
	Reject []struct {
		Name        string `json:"name"`
		Wire        string `json:"wire"`
		InvalidUTF8 bool   `json:"invalid_utf8"`
	} `json:"reject"`
}

func sourceAST(t *testing.T, s sourceValue) CanonicalTypedValue {
	t.Helper()
	switch s.Kind {
	case "omitted":
		return CanonicalTypedValue{}
	case "null":
		return NullValue()
	case "bool":
		var b bool
		if json.Unmarshal(s.Value, &b) != nil {
			t.Fatal("bad boolean source")
		}
		return BoolValue(b)
	case "string", "decimal":
		var text string
		if json.Unmarshal(s.Value, &text) != nil {
			t.Fatal("bad scalar source")
		}
		if s.Kind == "string" {
			return StringValue(text)
		}
		v, err := DecimalValue(text + strings.Repeat("0", s.TrailingZeros))
		if err != nil {
			t.Fatal("bad decimal source")
		}
		return v
	case "binary":
		b := make([]byte, len(s.Bytes))
		for i, n := range s.Bytes {
			if n < 0 || n > 255 {
				t.Fatal("bad binary source")
			}
			b[i] = byte(n)
		}
		return BinaryValue(b)
	case "list":
		list := []CanonicalTypedValue{}
		for _, child := range s.Items {
			list = append(list, sourceAST(t, child))
		}
		return ListValue(list...)
	case "object":
		fields := []Field{}
		for _, f := range s.Fields {
			fields = append(fields, Field{f.Name, sourceAST(t, f.Value)})
		}
		return ObjectValue(fields...)
	case "domain":
		var child sourceValue
		if json.Unmarshal(s.Value, &child) != nil {
			t.Fatal("bad domain source")
		}
		return DomainValue(s.Tag, sourceAST(t, child))
	}
	t.Fatal("unknown vector source")
	return CanonicalTypedValue{}
}
func loadVectors(t *testing.T) ([]byte, vectorFile) {
	t.Helper()
	data, err := os.ReadFile("testdata/canonical.v1.json")
	if err != nil {
		t.Fatal("missing frozen vectors")
	}
	var v vectorFile
	if json.Unmarshal(data, &v) != nil || v.Version != 1 {
		t.Fatal("invalid frozen vectors")
	}
	return data, v
}

func runReferenceEncoder(t *testing.T, data []byte, args ...string) []byte {
	t.Helper()
	redact := regexp.MustCompile(`[[:xdigit:]]{64,}`).ReplaceAllString
	interpreter, err := exec.LookPath("python3")
	if err != nil {
		t.Fatalf("independent reference encoder requires python3: %s", redact(err.Error(), "[redacted hex]"))
	}
	cmd := exec.Command(interpreter, append([]string{"testdata/reference_codec_test.py"}, args...)...)
	cmd.Stdin = bytes.NewReader(data)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("independent reference encoder failed: %s; stderr: %s", redact(err.Error(), "[redacted hex]"), redact(stderr.String(), "[redacted hex]"))
	}
	return output
}

// H25: the reference is a separate implementation written from the profile. Its
// Decimal tuple normalization shares no implementation with the Go AST codec.
func TestCanonicalEnvelopeGoldenVectors(t *testing.T) {
	data, vectors := loadVectors(t)
	output := runReferenceEncoder(t, data)
	var reference []string
	if json.Unmarshal(output, &reference) != nil || len(reference) != len(vectors.Valid) {
		t.Fatal("independent encoder count mismatch")
	}
	for i, v := range vectors.Valid {
		t.Run(v.Name, func(t *testing.T) {
			got, err := sourceAST(t, v.Source).MarshalJSON()
			if err != nil || string(got) != v.Canonical || reference[i] != v.Canonical {
				t.Fatal("independent encoders disagree with frozen vector")
			}
			var decoded CanonicalTypedValue
			if err := decoded.UnmarshalJSON(got); err != nil {
				t.Fatal("canonical vector rejected")
			}
			again, err := decoded.MarshalJSON()
			if err != nil || !bytes.Equal(got, again) {
				t.Fatal("canonical round trip changed bytes")
			}
		})
	}
	for _, v := range vectors.Reject {
		t.Run("reject_"+v.Name, func(t *testing.T) {
			wire := []byte(v.Wire)
			if v.InvalidUTF8 {
				wire = []byte{'"', 0xff, '"'}
			}
			var decoded CanonicalTypedValue
			if decoded.UnmarshalJSON(wire) == nil {
				t.Fatal("malformed vector accepted")
			}
		})
	}
	t.Logf("MODEL_PASS H25: %d canonical vectors agree with independent encoder; %d must-reject vectors", len(vectors.Valid), len(vectors.Reject))
}

func TestCanonicalTypedValueUnmarshalJSONByteBoundary(t *testing.T) {
	const prefix = `{"t":"string","v":1,"value":"`
	const suffix = `"}`
	for _, tc := range []struct {
		name string
		size int
	}{
		{"below", MaxCanonicalBytes - 1},
		{"at", MaxCanonicalBytes},
		{"above", MaxCanonicalBytes + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Build exact canonical wire directly: the encoder's own bound must
			// not prevent the oversized input from reaching the public decoder.
			wire := []byte(prefix + strings.Repeat("x", tc.size-len(prefix)-len(suffix)) + suffix)
			if len(wire) != tc.size {
				t.Fatal("decoder boundary fixture has the wrong length")
			}
			var value CanonicalTypedValue
			err := value.UnmarshalJSON(wire)
			if tc.size > MaxCanonicalBytes {
				if err == nil {
					t.Fatal("public typed-value decoder accepted oversized canonical wire")
				}
				return
			}
			if err != nil {
				t.Fatal("public typed-value decoder refused in-bound canonical wire")
			}
			canonical, err := value.MarshalJSON()
			if err != nil || !bytes.Equal(canonical, wire) {
				t.Fatal("decoder boundary input did not preserve canonical wire")
			}
		})
	}
}

func TestCanonicalBoundsAndClosedAST(t *testing.T) {
	for _, v := range []CanonicalTypedValue{{}, ListValue(CanonicalTypedValue{}), ObjectValue(Field{"x", NullValue()}, Field{"x", CanonicalTypedValue{}}), StringValue(string([]byte{0xff})), DomainValue("unknown", NullValue())} {
		if _, err := v.MarshalJSON(); err == nil {
			t.Fatal("invalid AST accepted")
		}
	}
	for _, text := range []string{"NaN", "Infinity", "01", "+1", "1.", ".1", "1e", "1e1000001", "1e-1000001", strings.Repeat("1", 4097)} {
		if _, err := DecimalValue(text); err == nil {
			t.Fatal("invalid decimal accepted")
		}
	}
	for _, value := range []any{float64(1), map[string]any{"x": float32(1)}, []byte{1}, struct{ Bytes []byte }{[]byte{1}}} {
		if _, err := MarshalCanonical(value); err == nil {
			t.Fatal("implicit floating point or byte encoding accepted")
		}
	}
	v := NullValue()
	for range maxValueDepth + 2 {
		v = ListValue(v)
	}
	if _, err := v.MarshalJSON(); err == nil {
		t.Fatal("excessive AST depth accepted")
	}
	for _, n := range []int{MaxCanonicalBytes - 1, MaxCanonicalBytes, MaxCanonicalBytes + 1} {
		_, err := StringValue(strings.Repeat("a", n)).MarshalJSON()
		if err == nil {
			t.Fatal("oversized typed wrapper accepted")
		}
	}
	prefix := len(`{"t":"string","v":1,"value":""}`)
	for _, n := range []int{MaxCanonicalBytes - 1, MaxCanonicalBytes, MaxCanonicalBytes + 1} {
		out, err := StringValue(strings.Repeat("a", n-prefix)).MarshalJSON()
		if n <= MaxCanonicalBytes && (err != nil || len(out) != n) {
			t.Fatal("in-bound encoding refused")
		}
		if n > MaxCanonicalBytes && err == nil {
			t.Fatal("out-of-bound encoding accepted")
		}
	}
}
func TestCanonicalSemanticAlternatives(t *testing.T) {
	var a, b CanonicalTypedValue
	if a.UnmarshalJSON([]byte(` {"value":"\ud83d\ude00\/","t":"string","v":1} `)) != nil || b.UnmarshalJSON([]byte(`{"t":"string","v":1,"value":"😀/"}`)) != nil {
		t.Fatal("valid escaped Unicode rejected")
	}
	first, _ := a.MarshalJSON()
	second, _ := b.MarshalJSON()
	if !bytes.Equal(first, second) {
		t.Fatal("equivalent Unicode did not normalize")
	}
	raw := []byte{1, 2, 3}
	value := BinaryValue(raw)
	raw[0] = 9
	before, _ := value.MarshalJSON()
	if string(before) != `{"t":"binary","v":1,"value":"AQID"}` {
		t.Fatal("binary input alias changed immutable AST")
	}
	fields := []Field{{"a", NullValue()}}
	obj := ObjectValue(fields...)
	fields[0].Name = "changed"
	copied, _ := obj.Fields()
	copied[0].Name = "changed_again"
	encoded, _ := obj.MarshalJSON()
	if !bytes.Contains(encoded, []byte(`"a"`)) {
		t.Fatal("object aliases changed immutable AST")
	}
}
func FuzzCanonicalTypedValue(f *testing.F) {
	f.Add([]byte(`{"t":"null","v":1}`))
	f.Add([]byte(`{"t":"string","v":1,"value":"\ud800"}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		var value CanonicalTypedValue
		if value.UnmarshalJSON(data) != nil {
			return
		}
		encoded, err := value.MarshalJSON()
		if err != nil {
			t.Fatal("decoded value cannot encode")
		}
		var next CanonicalTypedValue
		if next.UnmarshalJSON(encoded) != nil {
			t.Fatal("canonical encoding cannot decode")
		}
	})
}

func TestCanonicalSchemaHelpersRejectLossyInputs(t *testing.T) {
	invalid := string([]byte{0xff})
	for _, value := range []any{invalid, map[string]string{"x": invalid}, map[string]string{invalid: "value"}} {
		if _, err := MarshalCanonical(value); err == nil {
			t.Fatal("malformed Unicode normalized by generic encoder")
		}
	}
	type strictSchema struct {
		Count uint32 `json:"count"`
	}
	for _, raw := range []string{`{"Count":1}`, `{"count":1,"COUNT":2}`, `{}`, `{"count":null}`} {
		value := strictSchema{Count: 7}
		if UnmarshalStrict([]byte(raw), &value) == nil || value.Count != 7 {
			t.Fatal("ambiguous schema accepted or failed decode mutated target")
		}
	}
	var floating struct {
		Value float64 `json:"value"`
	}
	if UnmarshalStrict([]byte(`{"value":9007199254740993}`), &floating) == nil {
		t.Fatal("strict decoder admitted implicit binary64")
	}
	var nativeBinary struct {
		Value []byte `json:"value"`
	}
	if UnmarshalStrict([]byte(`{"value":"AQ=="}`), &nativeBinary) == nil {
		t.Fatal("strict decoder admitted implicit binary encoding")
	}
}

func TestIndependentCompleteEnvelopeGoldenVectors(t *testing.T) {
	data, err := os.ReadFile("testdata/envelopes.v1.json")
	if err != nil {
		t.Fatal("missing envelope vectors")
	}
	var vectors struct {
		Version int `json:"version"`
		Valid   []struct {
			Name      string          `json:"name"`
			Source    json.RawMessage `json:"source"`
			Canonical string          `json:"canonical"`
		} `json:"valid"`
		Reject []struct {
			Name string `json:"name"`
			Wire string `json:"wire"`
		} `json:"reject"`
	}
	if json.Unmarshal(data, &vectors) != nil || vectors.Version != 1 {
		t.Fatal("invalid envelope vector file")
	}
	output := runReferenceEncoder(t, data, "--envelope")
	var reference []string
	if json.Unmarshal(output, &reference) != nil || len(reference) != len(vectors.Valid) {
		t.Fatal("reference envelope count mismatch")
	}
	for i, v := range vectors.Valid {
		t.Run(v.Name, func(t *testing.T) {
			decoded, err := DecodeOperationEnvelope(v.Source)
			if err != nil {
				t.Fatal("valid source envelope refused")
			}
			encoded, err := decoded.CanonicalBytes()
			if err != nil || string(encoded) != v.Canonical || reference[i] != v.Canonical {
				t.Fatal("independent envelope encoders disagree with frozen vector")
			}
		})
	}
	for _, v := range vectors.Reject {
		t.Run("reject_"+v.Name, func(t *testing.T) {
			if _, err := DecodeOperationEnvelope([]byte(v.Wire)); err == nil {
				t.Fatal("malformed frozen envelope accepted")
			}
		})
	}
	if vectors.Valid[1].Canonical != vectors.Valid[2].Canonical || vectors.Valid[2].Canonical == vectors.Valid[3].Canonical {
		t.Fatal("golden set/list semantics drifted")
	}
	t.Logf("MODEL_PASS H25: %d complete envelope vectors and %d must-reject envelope vectors", len(vectors.Valid), len(vectors.Reject))
}
