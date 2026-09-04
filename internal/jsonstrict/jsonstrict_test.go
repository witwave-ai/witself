package jsonstrict

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestConsumeUniqueValue(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{name: "scalar", raw: `true`},
		{name: "unique nested values", raw: `{"object":{"key":1},"array":[{"key":2}]}`},
		{name: "duplicate root key", raw: `{"key":1,"key":2}`, wantErr: `duplicate JSON object key "key"`},
		{name: "duplicate nested key", raw: `{"nested":{"key":1,"key":2}}`, wantErr: `duplicate JSON object key "key"`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decoder := json.NewDecoder(strings.NewReader(test.raw))
			err := ConsumeUniqueValue(decoder)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("ConsumeUniqueValue() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("ConsumeUniqueValue() error = %v, want containing %q", err, test.wantErr)
			}
		})
	}

	decoder := json.NewDecoder(strings.NewReader(`{"key":1,"key":2}`))
	var duplicate *DuplicateKeyError
	if err := ConsumeUniqueValue(decoder); !errors.As(err, &duplicate) || duplicate.Key != "key" {
		t.Fatalf("ConsumeUniqueValue() duplicate = %#v, error = %v", duplicate, err)
	}
}

func TestConsumeUniqueValuePreservesTerminationErrors(t *testing.T) {
	tests := []struct {
		raw     string
		opening json.Delim
	}{
		{raw: `{ `, opening: '{'},
		{raw: `[ `, opening: '['},
	}
	for _, test := range tests {
		decoder := json.NewDecoder(strings.NewReader(test.raw))
		err := ConsumeUniqueValue(decoder)
		var termination *ContainerTerminationError
		if !errors.As(err, &termination) || termination.Opening != test.opening {
			t.Fatalf("ConsumeUniqueValue(%q) termination = %#v, want opening %q", test.raw, termination, test.opening)
		}
		if !errors.Is(err, io.EOF) || !errors.Is(err, termination.Err) || err.Error() != termination.Err.Error() {
			t.Fatalf("ConsumeUniqueValue(%q) error = %T %v, want preserved decoder EOF", test.raw, err, err)
		}
	}
}

func TestRequireEOF(t *testing.T) {
	t.Run("end", func(t *testing.T) {
		decoder := json.NewDecoder(strings.NewReader(`{"key":1}`))
		var value any
		if err := decoder.Decode(&value); err != nil {
			t.Fatal(err)
		}
		if err := RequireEOF(decoder); err != nil {
			t.Fatalf("RequireEOF() error = %v", err)
		}
	})

	t.Run("second value", func(t *testing.T) {
		decoder := json.NewDecoder(strings.NewReader(`{} []`))
		var value any
		if err := decoder.Decode(&value); err != nil {
			t.Fatal(err)
		}
		if err := RequireEOF(decoder); !errors.Is(err, ErrTrailingValue) {
			t.Fatalf("RequireEOF() error = %v, want ErrTrailingValue", err)
		}
	})

	t.Run("malformed trailing value", func(t *testing.T) {
		decoder := json.NewDecoder(bytes.NewBufferString(`{} {`))
		var value any
		if err := decoder.Decode(&value); err != nil {
			t.Fatal(err)
		}
		if err := RequireEOF(decoder); err == nil || errors.Is(err, ErrTrailingValue) {
			t.Fatalf("RequireEOF() error = %v, want preserved decoder error", err)
		}
	})
}
