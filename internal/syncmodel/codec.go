package syncmodel

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// CanonicalVersion freezes the profile specified in testdata/profile.v1.md.
const CanonicalVersion = 1

// MaxCanonicalBytes bounds one complete synthetic semantic encoding.
const MaxCanonicalBytes = 65536
const maxValueDepth = 64

// ErrCanonical reports invalid input without carrying payload values.
var ErrCanonical = errors.New("sync model: invalid canonical value")

// ValueKind is closed; the zero value means omitted, distinct from explicit null.
type ValueKind string

// Closed AST variants separate omitted fields from explicit null.
const (
	KindOmitted ValueKind = ""
	KindNull    ValueKind = "null"
	KindBool    ValueKind = "bool"
	KindString  ValueKind = "string"
	KindDecimal ValueKind = "decimal"
	KindBinary  ValueKind = "binary"
	KindList    ValueKind = "list"
	KindObject  ValueKind = "object"
	KindDomain  ValueKind = "domain"
)

// DomainTag selects a reviewed family namespace. Family-specific schemas further
// restrict its content at the envelope boundary.
type DomainTag string

func (tag DomainTag) valid() bool {
	switch tag {
	case "accounts_operators_agents", "realms", "facts_subjects_candidates_aliases", "memories_capture_curation_vectors_deletion", "transcripts", "messages_complete_requests", "inbound_outbound_email", "secrets_vault", "avatars", "preferences_consent", "usage_billing", "backups_exports_imports":
		return true
	}
	return false
}

// Field explicitly distinguishes an omitted field (zero Value) from null.
type Field struct {
	Name  string
	Value CanonicalTypedValue
}

// CanonicalTypedValue has no open interface or float64 variant. Constructors
// copy collection inputs; accessors copy them, keeping semantic bytes stable.
type CanonicalTypedValue struct {
	kind     ValueKind
	text     string
	exponent string
	boolean  bool
	binary   []byte
	list     []CanonicalTypedValue
	fields   []Field
	tag      DomainTag
	child    *CanonicalTypedValue
}

// NullValue constructs an explicit null node.
func NullValue() CanonicalTypedValue { return CanonicalTypedValue{kind: KindNull} }

// BoolValue constructs a boolean node.
func BoolValue(b bool) CanonicalTypedValue { return CanonicalTypedValue{kind: KindBool, boolean: b} }

// StringValue constructs a string node; validation rejects malformed Unicode.
func StringValue(s string) CanonicalTypedValue { return CanonicalTypedValue{kind: KindString, text: s} }

// BinaryValue copies bytes into the explicit unpadded-base64url variant.
func BinaryValue(b []byte) CanonicalTypedValue {
	return CanonicalTypedValue{kind: KindBinary, binary: bytes.Clone(b)}
}

// ListValue copies an ordered sequence; omitted members are invalid.
func ListValue(v ...CanonicalTypedValue) CanonicalTypedValue {
	return CanonicalTypedValue{kind: KindList, list: append([]CanonicalTypedValue{}, v...)}
}

// ObjectValue copies named fields, preserving omitted versus null values.
func ObjectValue(fields ...Field) CanonicalTypedValue {
	return CanonicalTypedValue{kind: KindObject, fields: append([]Field{}, fields...)}
}

// DomainValue wraps a value with a closed family tag.
func DomainValue(tag DomainTag, v CanonicalTypedValue) CanonicalTypedValue {
	return CanonicalTypedValue{kind: KindDomain, tag: tag, child: &v}
}

// Kind returns the closed AST discriminator.
func (v CanonicalTypedValue) Kind() ValueKind { return v.kind }

// DomainTag returns a tag only for a domain node.
func (v CanonicalTypedValue) DomainTag() (DomainTag, bool) { return v.tag, v.kind == KindDomain }

// Fields returns a copy of object fields and whether this is an object.
func (v CanonicalTypedValue) Fields() ([]Field, bool) {
	return append([]Field{}, v.fields...), v.kind == KindObject
}

// DomainContent returns a domain child without exposing mutable collection storage.
func (v CanonicalTypedValue) DomainContent() (CanonicalTypedValue, bool) {
	if v.kind != KindDomain || v.child == nil {
		return CanonicalTypedValue{}, false
	}
	return *v.child, true
}

// Text returns text only for a string node.
func (v CanonicalTypedValue) Text() (string, bool) { return v.text, v.kind == KindString }

var decimalPattern = regexp.MustCompile(`^(-?)(0|[1-9][0-9]*)(?:\.([0-9]+))?(?:[eE]([+-]?[0-9]+))?$`)

// DecimalValue preserves arbitrary decimal precision, normalizing trailing zeros
// into a bounded exponent and all signed zeros to coefficient=0, exponent=0.
func DecimalValue(text string) (CanonicalTypedValue, error) {
	if len(text) > 8192 {
		return CanonicalTypedValue{}, ErrCanonical
	}
	m := decimalPattern.FindStringSubmatch(text)
	if m == nil {
		return CanonicalTypedValue{}, ErrCanonical
	}
	coefficient := strings.TrimLeft(m[2]+m[3], "0")
	exponent := int64(0)
	if coefficient == "" {
		coefficient = "0"
	} else {
		if m[4] != "" {
			var err error
			exponent, err = strconv.ParseInt(m[4], 10, 64)
			if err != nil || exponent < -1008192 || exponent > 1008192 {
				return CanonicalTypedValue{}, ErrCanonical
			}
		}
		exponent -= int64(len(m[3]))
		trimmed := strings.TrimRight(coefficient, "0")
		exponent += int64(len(coefficient) - len(trimmed))
		coefficient = m[1] + trimmed
		if len(trimmed) > 4096 || exponent < -1000000 || exponent > 1000000 {
			return CanonicalTypedValue{}, ErrCanonical
		}
	}
	return CanonicalTypedValue{kind: KindDecimal, text: coefficient, exponent: strconv.FormatInt(exponent, 10)}, nil
}

// Validate checks the AST structure without lossy normalization.
func (v CanonicalTypedValue) Validate() error { _, err := v.wire(0); return err }
func (v CanonicalTypedValue) wire(depth int) (any, error) {
	if depth > maxValueDepth {
		return nil, ErrCanonical
	}
	out := map[string]any{"v": CanonicalVersion, "t": string(v.kind)}
	switch v.kind {
	case KindNull:
	case KindBool:
		out["value"] = v.boolean
	case KindString:
		if !utf8.ValidString(v.text) {
			return nil, ErrCanonical
		}
		out["value"] = v.text
	case KindDecimal:
		checked, err := DecimalValue(v.text + "e" + v.exponent)
		if err != nil || checked.text != v.text || checked.exponent != v.exponent {
			return nil, ErrCanonical
		}
		out["coefficient"] = v.text
		out["exponent"] = v.exponent
	case KindBinary:
		out["value"] = base64.RawURLEncoding.EncodeToString(v.binary)
	case KindList:
		list := make([]any, 0, len(v.list))
		for _, item := range v.list {
			next, err := item.wire(depth + 1)
			if err != nil {
				return nil, err
			}
			list = append(list, next)
		}
		out["items"] = list
	case KindObject:
		fields := map[string]any{}
		seen := map[string]bool{}
		for _, field := range v.fields {
			if !utf8.ValidString(field.Name) || seen[field.Name] {
				return nil, ErrCanonical
			}
			seen[field.Name] = true
			if field.Value.kind == KindOmitted {
				continue
			}
			next, err := field.Value.wire(depth + 1)
			if err != nil {
				return nil, err
			}
			fields[field.Name] = next
		}
		out["fields"] = fields
	case KindDomain:
		if !v.tag.valid() || v.child == nil {
			return nil, ErrCanonical
		}
		next, err := v.child.wire(depth + 1)
		if err != nil {
			return nil, err
		}
		out["tag"] = string(v.tag)
		out["value"] = next
	default:
		return nil, ErrCanonical
	}
	return out, nil
}

// MarshalJSON encodes this AST using the frozen canonical profile.
func (v CanonicalTypedValue) MarshalJSON() ([]byte, error) {
	wire, err := v.wire(0)
	if err != nil {
		return nil, err
	}
	return MarshalCanonical(wire)
}

// UnmarshalJSON decodes the closed profile and assigns only validated AST values.
func (v *CanonicalTypedValue) UnmarshalJSON(data []byte) error {
	wire, err := parseStrict(data)
	if err != nil {
		return err
	}
	next, err := valueFromWire(wire, 0)
	if err != nil {
		return err
	}
	*v = next
	return nil
}
func valueFromWire(wire any, depth int) (CanonicalTypedValue, error) {
	bad := CanonicalTypedValue{}
	if depth > maxValueDepth {
		return bad, ErrCanonical
	}
	m, ok := wire.(map[string]any)
	if !ok || m["v"] != json.Number("1") {
		return bad, ErrCanonical
	}
	t, ok := m["t"].(string)
	if !ok {
		return bad, ErrCanonical
	}
	switch ValueKind(t) {
	case KindNull:
		if len(m) == 2 {
			return NullValue(), nil
		}
	case KindBool:
		b, ok := m["value"].(bool)
		if ok && len(m) == 3 {
			return BoolValue(b), nil
		}
	case KindString:
		s, ok := m["value"].(string)
		if ok && len(m) == 3 {
			return StringValue(s), nil
		}
	case KindBinary:
		s, ok := m["value"].(string)
		if !ok || len(m) != 3 {
			break
		}
		b, err := base64.RawURLEncoding.Strict().DecodeString(s)
		if err == nil && base64.RawURLEncoding.EncodeToString(b) == s {
			return BinaryValue(b), nil
		}
	case KindDecimal:
		c, cok := m["coefficient"].(string)
		e, eok := m["exponent"].(string)
		if !cok || !eok || len(m) != 4 {
			break
		}
		v, err := DecimalValue(c + "e" + e)
		if err == nil && v.text == c && v.exponent == e {
			return v, nil
		}
	case KindList:
		items, ok := m["items"].([]any)
		if !ok || len(m) != 3 {
			break
		}
		values := make([]CanonicalTypedValue, 0, len(items))
		for _, item := range items {
			v, err := valueFromWire(item, depth+1)
			if err != nil {
				return bad, err
			}
			values = append(values, v)
		}
		return ListValue(values...), nil
	case KindObject:
		fields, ok := m["fields"].(map[string]any)
		if !ok || len(m) != 3 {
			break
		}
		values := make([]Field, 0, len(fields))
		names := make([]string, 0, len(fields))
		for name := range fields {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			v, err := valueFromWire(fields[name], depth+1)
			if err != nil {
				return bad, err
			}
			values = append(values, Field{name, v})
		}
		return ObjectValue(values...), nil
	case KindDomain:
		tag, ok := m["tag"].(string)
		if !ok || len(m) != 4 || !DomainTag(tag).valid() {
			break
		}
		child, err := valueFromWire(m["value"], depth+1)
		if err != nil {
			return bad, err
		}
		return DomainValue(DomainTag(tag), child), nil
	}
	return bad, ErrCanonical
}

// MarshalCanonical orders keys by UTF-8 bytes, uses profile JSON string escaping,
// and permits only integral numeric schema versions. Payload numbers use DecimalValue.
func MarshalCanonical(value any) ([]byte, error) {
	if implicitWire(reflect.ValueOf(value), 0) {
		return nil, ErrCanonical
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, ErrCanonical
	}
	parsed, err := parseStrict(data)
	if err != nil {
		return nil, err
	}
	out, err := appendCanonical(nil, parsed)
	if err != nil || len(out) > MaxCanonicalBytes {
		return nil, ErrCanonical
	}
	return out, nil
}

// UnmarshalStrict refuses duplicate decoded field names, unknown struct fields,
// malformed Unicode and trailing JSON before assigning schema-aware values.
func UnmarshalStrict(data []byte, dst any) error {
	if _, err := parseStrict(data); err != nil {
		return err
	}
	target := reflect.ValueOf(dst)
	if !target.IsValid() || target.Kind() != reflect.Pointer || target.IsNil() {
		return ErrCanonical
	}
	if err := requireWireFields(data, target.Type().Elem()); err != nil {
		return ErrCanonical
	}
	next := reflect.New(target.Type().Elem())
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(next.Interface()); err != nil || implicitWire(next.Elem(), 0) {
		return ErrCanonical
	}
	target.Elem().Set(next.Elem())
	return nil
}

func parseStrict(data []byte) (any, error) {
	if len(data) > MaxCanonicalBytes || !utf8.Valid(data) || !validSurrogates(data) {
		return nil, ErrCanonical
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	value, err := readJSON(decoder, 0)
	if err != nil {
		return nil, ErrCanonical
	}
	if _, err = decoder.Token(); err != io.EOF {
		return nil, ErrCanonical
	}
	return value, nil
}
func readJSON(decoder *json.Decoder, depth int) (any, error) {
	if depth > maxValueDepth*3 {
		return nil, ErrCanonical
	}
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	switch token {
	case json.Delim('{'):
		object := map[string]any{}
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			name, ok := key.(string)
			if !ok {
				return nil, ErrCanonical
			}
			if _, exists := object[name]; exists {
				return nil, ErrCanonical
			}
			value, err := readJSON(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			object[name] = value
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return nil, ErrCanonical
		}
		return object, nil
	case json.Delim('['):
		list := []any{}
		for decoder.More() {
			value, err := readJSON(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			list = append(list, value)
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return nil, ErrCanonical
		}
		return list, nil
	default:
		switch token.(type) {
		case nil, bool, string, json.Number:
			return token, nil
		}
		return nil, ErrCanonical
	}
}
func appendCanonical(dst []byte, value any) ([]byte, error) {
	switch value := value.(type) {
	case nil:
		return append(dst, "null"...), nil
	case bool:
		return strconv.AppendBool(dst, value), nil
	case string:
		quoted, _ := json.Marshal(value)
		return append(dst, quoted...), nil
	case json.Number:
		n := string(value)
		if n == "-0" || strings.ContainsAny(n, ".eE+") {
			return nil, ErrCanonical
		}
		return append(dst, n...), nil
	case []any:
		dst = append(dst, '[')
		for i, item := range value {
			if i > 0 {
				dst = append(dst, ',')
			}
			var err error
			dst, err = appendCanonical(dst, item)
			if err != nil {
				return nil, err
			}
		}
		return append(dst, ']'), nil
	case map[string]any:
		names := make([]string, 0, len(value))
		for name := range value {
			names = append(names, name)
		}
		sort.Strings(names)
		dst = append(dst, '{')
		for i, name := range names {
			if i > 0 {
				dst = append(dst, ',')
			}
			quoted, _ := json.Marshal(name)
			dst = append(dst, quoted...)
			dst = append(dst, ':')
			var err error
			dst, err = appendCanonical(dst, value[name])
			if err != nil {
				return nil, err
			}
		}
		return append(dst, '}'), nil
	}
	return nil, ErrCanonical
}

// encoding/json replaces malformed surrogate escapes; this guard rejects them
// before that lossy behavior can collapse distinct untrusted inputs.
func validSurrogates(data []byte) bool {
	inside := false
	for i := 0; i < len(data); i++ {
		if data[i] == '"' {
			inside = !inside
			continue
		}
		if !inside || data[i] != '\\' {
			continue
		}
		i++
		if i >= len(data) {
			return false
		}
		if data[i] != 'u' {
			continue
		}
		if i+4 >= len(data) {
			return false
		}
		n, err := strconv.ParseUint(string(data[i+1:i+5]), 16, 16)
		if err != nil {
			return false
		}
		i += 4
		if n >= 0xdc00 && n <= 0xdfff {
			return false
		}
		if n >= 0xd800 && n <= 0xdbff {
			if i+6 >= len(data) || data[i+1] != '\\' || data[i+2] != 'u' {
				return false
			}
			low, err := strconv.ParseUint(string(data[i+3:i+7]), 16, 16)
			if err != nil || low < 0xdc00 || low > 0xdfff {
				return false
			}
			i += 6
		}
	}
	return true
}

// implicitWire prevents callers from accidentally selecting encoding/json's
// binary64 or byte-slice profile through a schema field or interface value.
func implicitWire(v reflect.Value, depth int) bool {
	if !v.IsValid() {
		return false
	}
	if depth > maxValueDepth*3 {
		return true
	}
	if v.CanInterface() {
		if _, ok := v.Interface().(json.Marshaler); ok {
			return false
		}
	}
	switch v.Kind() {
	case reflect.Float32, reflect.Float64:
		return true
	case reflect.String:
		return !utf8.ValidString(v.String())
	case reflect.Interface, reflect.Pointer:
		if !v.IsNil() {
			return implicitWire(v.Elem(), depth+1)
		}
	case reflect.Slice, reflect.Array:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return true
		}
		for i := 0; i < v.Len(); i++ {
			if implicitWire(v.Index(i), depth+1) {
				return true
			}
		}
	case reflect.Map:
		iter := v.MapRange()
		for iter.Next() {
			if implicitWire(iter.Key(), depth+1) || implicitWire(iter.Value(), depth+1) {
				return true
			}
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() && v.Type().Field(i).Tag.Get("json") != "-" && implicitWire(v.Field(i), depth+1) {
				return true
			}
		}
	}
	return false
}
