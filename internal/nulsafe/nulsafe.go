// Package nulsafe removes NUL characters from user-supplied text and JSON
// before it reaches PostgreSQL. Postgres TEXT columns reject the NUL byte
// and jsonb rejects the JSON NUL escape ("unsupported Unicode escape
// sequence", SQLSTATE 22P05), so a single NUL inside captured content would
// otherwise turn an entire write into a 5xx. Captured terminal output
// legitimately contains NUL, so content fields are sanitized to U+FFFD
// rather than rejected; identifier fields should instead be rejected by
// their callers.
package nulsafe

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// replacement is the Unicode replacement character U+FFFD, the conventional
// stand-in for content a target encoding cannot represent.
const replacement = "�"

// nulEscape is the six-character JSON escape sequence for NUL. The backtick
// literal keeps it as backslash-u-0-0-0-0, never an actual NUL byte.
const nulEscape = `\u0000`

// ContainsString reports whether s contains a NUL byte.
func ContainsString(s string) bool {
	return strings.IndexByte(s, 0) >= 0
}

// ReplaceString returns s with every NUL byte replaced by U+FFFD.
func ReplaceString(s string) string {
	if !ContainsString(s) {
		return s
	}
	return strings.ReplaceAll(s, "\x00", replacement)
}

// SanitizeJSON returns raw with every NUL inside JSON strings — keys and
// values, at any depth — replaced by U+FFFD. When no NUL is present the
// input is returned unchanged with changed=false. When a NUL is found the
// value is re-marshaled canonically: object keys sort, numbers keep their
// exact source text via json.Number, and duplicate keys collapse last-wins
// (the same normalization PostgreSQL jsonb applies on storage).
func SanitizeJSON(raw json.RawMessage) (out json.RawMessage, changed bool, err error) {
	if len(raw) == 0 {
		return raw, false, nil
	}
	// A NUL can only enter a decoded Go string through the JSON NUL escape:
	// an unescaped control byte inside a JSON string is invalid JSON and
	// fails Unmarshal. The escape's hex digits are all zeros and JSON only
	// permits a lowercase 'u', so a byte scan for the literal six-character
	// sequence is a sound pre-filter. An escaped backslash followed by the
	// text u0000 also matches the scan, but the decoder walk below
	// distinguishes it, so a false positive only costs the re-marshal
	// check, never a corruption.
	if !bytes.Contains(raw, []byte(nulEscape)) {
		return raw, false, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, false, fmt.Errorf("invalid JSON: %w", err)
	}
	if decoder.More() {
		return nil, false, fmt.Errorf("invalid JSON: trailing content")
	}
	value, changed = sanitizeValue(value)
	if !changed {
		return raw, false, nil
	}
	canonical, err := marshalCanonical(value)
	if err != nil {
		return nil, false, err
	}
	return canonical, true, nil
}

func sanitizeValue(value any) (any, bool) {
	switch v := value.(type) {
	case string:
		if ContainsString(v) {
			return ReplaceString(v), true
		}
		return v, false
	case map[string]any:
		changed := false
		out := make(map[string]any, len(v))
		for key, item := range v {
			cleanKey := key
			if ContainsString(key) {
				cleanKey = ReplaceString(key)
				changed = true
			}
			cleanItem, itemChanged := sanitizeValue(item)
			if itemChanged {
				changed = true
			}
			out[cleanKey] = cleanItem
		}
		return out, changed
	case []any:
		changed := false
		for i, item := range v {
			cleanItem, itemChanged := sanitizeValue(item)
			if itemChanged {
				changed = true
				v[i] = cleanItem
			}
		}
		return v, changed
	default:
		return value, false
	}
}

// marshalCanonical marshals with sorted object keys and exact number text.
// encoding/json already sorts map[string]any keys and emits json.Number
// verbatim; this wrapper exists so the guarantee is stated and tested here
// rather than assumed at call sites.
func marshalCanonical(value any) (json.RawMessage, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return json.RawMessage(bytes.TrimSuffix(buf.Bytes(), []byte("\n"))), nil
}
