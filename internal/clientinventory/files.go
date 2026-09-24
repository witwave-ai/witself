package clientinventory

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

var (
	errMissing = errors.New("missing")
	errUnsafe  = errors.New("unavailable")
)

// Reject excessive nesting, element counts and duplicate (including folded)
// keys before typed decoding. This also bounds unknown record fields.
func boundedJSON(raw []byte) bool {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	remaining := 8192
	var value func(int) bool
	value = func(depth int) bool {
		remaining--
		if remaining < 0 || depth > 24 {
			return false
		}
		tok, err := d.Token()
		if err != nil {
			return false
		}
		delim, ok := tok.(json.Delim)
		if !ok {
			return true
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return false
				}
				s, ok := key.(string)
				if !ok {
					return false
				}
				s = strings.ToLower(s)
				if seen[s] {
					return false
				}
				seen[s] = true
				if !value(depth + 1) {
					return false
				}
			}
			end, err := d.Token()
			return err == nil && end == json.Delim('}')
		case '[':
			for d.More() {
				if !value(depth + 1) {
					return false
				}
			}
			end, err := d.Token()
			return err == nil && end == json.Delim(']')
		default:
			return false
		}
	}
	if !value(0) {
		return false
	}
	_, err := d.Token()
	return err == io.EOF
}
