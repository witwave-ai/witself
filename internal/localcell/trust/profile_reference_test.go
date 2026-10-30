package trust_test

// This independent test oracle implements the strict JSON profile from
// docs/research/local-cell/component-set-trust.md. It uses an iterative container
// stack and a separate literal schema table, never package profile/parser helpers
// or reflection over production document types. Semantic trust decisions are not
// JSON-profile decisions and are tested separately by the frozen replay.
import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/witwave-ai/witself/internal/localcell/trust"
)

type referenceFrame struct {
	object  bool
	keyNext bool
	names   map[string]bool
}

func referenceWireProfile(b []byte) string {
	if !utf8.Valid(b) || !referenceEscapes(b) {
		return "profile_violation"
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	stack := []referenceFrame{}
	started, finished := false, false
	for {
		token, err := d.Token()
		if err == io.EOF {
			if finished {
				return ""
			}
			return "invalid_json"
		}
		if err != nil {
			return "invalid_json"
		}
		if finished {
			return "invalid_json"
		}
		if !started {
			if token != json.Delim('{') {
				return "profile_violation"
			}
			started = true
		}
		if s, ok := token.(string); ok {
			if len(s) > 4096 {
				return "profile_violation"
			}
			if len(stack) > 0 && stack[len(stack)-1].object && stack[len(stack)-1].keyNext {
				f := &stack[len(stack)-1]
				if f.names[s] {
					return "duplicate_key"
				}
				f.names[s] = true
				f.keyNext = false
				continue
			}
		}
		if delim, ok := token.(json.Delim); ok {
			if delim == '{' || delim == '[' {
				if len(stack) == 8 {
					return "profile_violation"
				}
				if len(stack) > 0 && stack[len(stack)-1].object {
					stack[len(stack)-1].keyNext = true
				}
				stack = append(stack, referenceFrame{object: delim == '{', keyNext: delim == '{', names: map[string]bool{}})
			} else {
				if len(stack) == 0 {
					return "invalid_json"
				}
				stack = stack[:len(stack)-1]
				if len(stack) == 0 {
					finished = true
				}
			}
		} else if len(stack) > 0 && stack[len(stack)-1].object {
			stack[len(stack)-1].keyNext = true
		}
	}
}

func referenceEscapes(b []byte) bool {
	inString := false
	for i := 0; i < len(b); i++ {
		if b[i] == '"' {
			inString = !inString
			continue
		}
		if !inString || b[i] != '\\' {
			continue
		}
		i++
		if i >= len(b) {
			return false
		}
		if b[i] != 'u' {
			continue
		}
		if i+4 >= len(b) {
			return false
		}
		n, e := strconv.ParseUint(string(b[i+1:i+5]), 16, 16)
		if e != nil {
			return false
		}
		i += 4
		if n >= 0xdc00 && n <= 0xdfff {
			return false
		}
		if n >= 0xd800 && n <= 0xdbff {
			if i+6 >= len(b) || b[i+1] != '\\' || b[i+2] != 'u' {
				return false
			}
			low, e := strconv.ParseUint(string(b[i+3:i+7]), 16, 16)
			if e != nil || low < 0xdc00 || low > 0xdfff {
				return false
			}
			i += 6
		}
	}
	return true
}

// Literal schemas transcribed from the normative document. '?' marks optional
// fields, '[]' arrays. Unknown fields are allowed only at the document root.
var referenceSchemas = map[string]map[string]string{
	"component_set": {"format_version": "uint", "critical": "[]string", "kind": "string", "root_sequence": "uint", "delegation_sequence": "uint", "release_sequence": "uint", "release_tag": "string", "channel": "string", "issued_at": "string", "expires_at": "string", "source": "source", "provenance": "provenance", "license": "string", "sbom": "string", "rollback": "rollback", "platforms": "[]platform"},
	"delegation":    {"format_version": "uint", "critical": "[]string", "kind": "string", "root_sequence": "uint", "delegation_sequence": "uint", "issued_at": "string", "expires_at": "string", "keys": "[]public_key", "revoked_key_ids": "[]string"},
	"root_rotation": {"format_version": "uint", "critical": "[]string", "kind": "string", "root_sequence": "uint", "new_sequence": "uint", "issued_at": "string", "expires_at": "string", "new_root": "public_key"},
	"envelope":      {"format_version": "uint", "kind": "string", "algorithm": "string", "signatures": "[]signature"},
	"signature":     {"key_id": "string", "signature": "string"},
	"public_key":    {"key_id": "string", "spki": "string"},
	"source":        {"repository": "string", "commit": "string"},
	"provenance":    {"checksums_sha256": "string", "sigstore_bundle": "string"},
	"rollback":      {"allowed_targets": "[]uint"},
	"platform":      {"os": "string", "arch": "string", "min_os": "string", "glibc_floor": "?string", "components": "[]component"},
	"component":     {"role": "string", "version": "string", "archive": "archive", "inventory": "[]entry", "depends_on": "[]string", "postgresql": "?postgresql", "required_extensions": "[]extension", "schema": "schema", "ownership": "ownership"},
	"archive":       {"name": "string", "size": "uint", "sha256": "string", "format": "string"},
	"entry":         {"path": "string", "size": "uint", "sha256": "string", "mode": "string", "executable": "bool"},
	"postgresql":    {"major": "uint", "version": "string", "distribution": "string", "build": "string", "runtime_libraries": "[]string", "data_layout_version": "uint"},
	"extension":     {"name": "string", "version": "string", "abi": "string"},
	"schema":        {"target": "uint", "accepted_min": "uint", "accepted_max": "uint"},
	"ownership":     {"package_owned_paths": "[]string", "private_payload_paths": "[]string"},
}

func referenceShape(raw []byte, schema string, top bool) string {
	if top {
		limit := 16384
		if schema == "component_set" {
			limit = 262144
		}
		if schema == "envelope" {
			limit = 8192
		}
		if len(raw) > limit {
			if schema == "envelope" {
				return "envelope_too_large"
			}
			return "document_too_large"
		}
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return "invalid_document"
	}
	if strings.HasPrefix(schema, "[]") {
		var entries []json.RawMessage
		if raw[0] != '[' || json.Unmarshal(raw, &entries) != nil {
			return "invalid_document"
		}
		for _, entry := range entries {
			if r := referenceShape(entry, schema[2:], false); r != "" {
				return r
			}
		}
		return ""
	}
	switch schema {
	case "uint":
		if raw[0] < '0' || raw[0] > '9' {
			return "profile_violation"
		}
		for _, b := range raw {
			if b < '0' || b > '9' {
				return "profile_violation"
			}
		}
		if len(raw) > 1 && raw[0] == '0' {
			return "profile_violation"
		}
		if _, e := strconv.ParseUint(string(raw), 10, 64); e != nil {
			return "profile_violation"
		}
		return ""
	case "string":
		if raw[0] != '"' {
			return "invalid_document"
		}
		return ""
	case "bool":
		if !bytes.Equal(raw, []byte("true")) && !bytes.Equal(raw, []byte("false")) {
			return "invalid_document"
		}
		return ""
	}
	var object map[string]json.RawMessage
	if raw[0] != '{' || json.Unmarshal(raw, &object) != nil {
		return "invalid_document"
	}
	fields := referenceSchemas[schema]
	for name, value := range object {
		typ, known := fields[name]
		if !known {
			if top && schema != "envelope" {
				continue
			}
			return "unknown_field"
		}
		if r := referenceShape(value, strings.TrimPrefix(typ, "?"), false); r != "" {
			return r
		}
	}
	for name, typ := range fields {
		if _, ok := object[name]; !ok && !strings.HasPrefix(typ, "?") {
			return "invalid_document"
		}
	}
	if top {
		if string(bytes.TrimSpace(object["format_version"])) != "1" {
			return "invalid_document"
		}
		if schema != "envelope" {
			var critical []string
			if json.Unmarshal(object["critical"], &critical) != nil {
				return "invalid_document"
			}
			seen := map[string]bool{}
			for _, name := range critical {
				if _, ok := fields[name]; !ok {
					return "unknown_critical"
				}
				if seen[name] {
					return "invalid_document"
				}
				seen[name] = true
			}
		}
	}
	return ""
}

func TestIndependentProfileReference(t *testing.T) {
	for _, c := range frozenIndex(t).Cases {
		t.Run(c.Name, func(t *testing.T) {
			for _, part := range []struct{ file, schema string }{{c.Document, c.Kind}, {c.Envelope, "envelope"}} {
				b := vectorBytes(t, part.file)
				want := referenceWireProfile(b)
				got := ""
				if err := trust.StrictProfile(b); err != nil {
					got = err.Error()
				}
				if got != want {
					t.Fatalf("wire profile disagreement: package=%s reference=%s", got, want)
				}
				if want != "" {
					continue
				}
				shape := referenceShape(b, part.schema, true)
				var err error
				switch part.schema {
				case "component_set":
					_, err = trust.ParseComponentSet(b)
				case "delegation":
					_, err = trust.ParseDelegation(b)
				case "root_rotation":
					_, err = trust.ParseRootRotation(b)
				default:
					_, err = trust.ParseEnvelope(b)
				}
				if shape != "" && (err == nil || err.Error() != shape) {
					t.Fatalf("schema profile disagreement: package=%v reference=%s", err, shape)
				}
				if shape == "" && (errors.Is(err, trust.ReasonProfileViolation) || errors.Is(err, trust.ReasonUnknownField) || errors.Is(err, trust.ReasonUnknownCritical) || errors.Is(err, trust.ReasonDuplicateKey)) {
					t.Fatalf("package rejected a reference-valid profile: %v", err)
				}
			}
		})
	}
}
