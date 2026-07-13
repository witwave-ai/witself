package store

import (
	"bytes"
	"encoding/json"
	"net/mail"
	"net/url"
	"strings"
	"time"
)

type factJSONKind string

const (
	factJSONKindString  factJSONKind = "string"
	factJSONKindNumber  factJSONKind = "number"
	factJSONKindBoolean factJSONKind = "boolean"
	factJSONKindList    factJSONKind = "list"
	factJSONKindObject  factJSONKind = "object"
	factJSONKindNull    factJSONKind = "null"
)

type factStringFormat string

const (
	factStringFormatNone     factStringFormat = ""
	factStringFormatTrim     factStringFormat = "trim"
	factStringFormatDate     factStringFormat = "date"
	factStringFormatDateTime factStringFormat = "datetime"
	factStringFormatURL      factStringFormat = "url"
	factStringFormatEmail    factStringFormat = "email"
)

// factValueTypeSpec is deliberately data, not executable validation supplied
// by callers. Adding a built-in type selects JSON shapes and one of the small,
// audited string formats interpreted below; custom types remain JSON values.
type factValueTypeSpec struct {
	Kinds           []factJSONKind
	StringFormat    factStringFormat
	RequireNonEmpty bool
}

var builtInFactValueTypes = map[string]factValueTypeSpec{
	"string":   {Kinds: []factJSONKind{factJSONKindString}},
	"number":   {Kinds: []factJSONKind{factJSONKindNumber}},
	"boolean":  {Kinds: []factJSONKind{factJSONKindBoolean}},
	"list":     {Kinds: []factJSONKind{factJSONKindList}},
	"object":   {Kinds: []factJSONKind{factJSONKindObject}},
	"json":     {Kinds: []factJSONKind{factJSONKindString, factJSONKindNumber, factJSONKindBoolean, factJSONKindList, factJSONKindObject, factJSONKindNull}},
	"date":     {Kinds: []factJSONKind{factJSONKindString}, StringFormat: factStringFormatDate},
	"datetime": {Kinds: []factJSONKind{factJSONKindString}, StringFormat: factStringFormatDateTime},
	"url":      {Kinds: []factJSONKind{factJSONKindString}, StringFormat: factStringFormatURL},
	"email":    {Kinds: []factJSONKind{factJSONKindString}, StringFormat: factStringFormatEmail},
	"address": {
		Kinds:           []factJSONKind{factJSONKindString, factJSONKindObject},
		StringFormat:    factStringFormatTrim,
		RequireNonEmpty: true,
	},
	"location": {
		Kinds:           []factJSONKind{factJSONKindString, factJSONKindObject},
		StringFormat:    factStringFormatTrim,
		RequireNonEmpty: true,
	},
}

func normalizeFactValue(valueType string, value json.RawMessage) (json.RawMessage, error) {
	if !json.Valid(value) {
		return nil, ErrFactInputInvalid
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, value); err != nil {
		return nil, ErrFactInputInvalid
	}
	normalized := json.RawMessage(compact.Bytes())
	spec, ok := builtInFactValueTypes[valueType]
	if !ok {
		return normalized, nil
	}

	kind := factValueJSONKind(normalized)
	if !containsFactJSONKind(spec.Kinds, kind) {
		return nil, ErrFactInputInvalid
	}
	if kind == factJSONKindObject && spec.RequireNonEmpty && bytes.Equal(normalized, []byte(`{}`)) {
		return nil, ErrFactInputInvalid
	}
	if kind != factJSONKindString || spec.StringFormat == factStringFormatNone {
		return normalized, nil
	}

	var text string
	if err := json.Unmarshal(normalized, &text); err != nil {
		return nil, ErrFactInputInvalid
	}
	text = strings.TrimSpace(text)
	if spec.RequireNonEmpty && text == "" {
		return nil, ErrFactInputInvalid
	}

	switch spec.StringFormat {
	case factStringFormatTrim:
	case factStringFormatDate:
		parsed, err := time.Parse(time.DateOnly, text)
		if err != nil {
			return nil, ErrFactInputInvalid
		}
		text = parsed.Format(time.DateOnly)
	case factStringFormatDateTime:
		parsed, err := time.Parse(time.RFC3339, text)
		if err != nil {
			return nil, ErrFactInputInvalid
		}
		text = parsed.UTC().Format(time.RFC3339Nano)
	case factStringFormatURL:
		parsed, err := url.Parse(text)
		if err != nil {
			return nil, ErrFactInputInvalid
		}
		parsed.Scheme = strings.ToLower(parsed.Scheme)
		if !parsed.IsAbs() || parsed.Host == "" || parsed.User != nil ||
			(parsed.Scheme != "http" && parsed.Scheme != "https") {
			return nil, ErrFactInputInvalid
		}
		parsed.Host = strings.ToLower(parsed.Host)
		text = parsed.String()
	case factStringFormatEmail:
		parsed, err := mail.ParseAddress(text)
		if err != nil || parsed.Name != "" || parsed.Address != text {
			return nil, ErrFactInputInvalid
		}
		at := strings.LastIndexByte(parsed.Address, '@')
		if at <= 0 || at == len(parsed.Address)-1 {
			return nil, ErrFactInputInvalid
		}
		text = parsed.Address[:at+1] + strings.ToLower(parsed.Address[at+1:])
	default:
		return nil, ErrFactInputInvalid
	}

	out, err := json.Marshal(text)
	if err != nil {
		return nil, ErrFactInputInvalid
	}
	return json.RawMessage(out), nil
}

func factValueJSONKind(value json.RawMessage) factJSONKind {
	switch value[0] {
	case '"':
		return factJSONKindString
	case '{':
		return factJSONKindObject
	case '[':
		return factJSONKindList
	case 't', 'f':
		return factJSONKindBoolean
	case 'n':
		return factJSONKindNull
	default:
		return factJSONKindNumber
	}
}

func containsFactJSONKind(kinds []factJSONKind, target factJSONKind) bool {
	for _, kind := range kinds {
		if kind == target {
			return true
		}
	}
	return false
}
