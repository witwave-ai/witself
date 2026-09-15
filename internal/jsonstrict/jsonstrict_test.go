package jsonstrict

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"testing/iotest"
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

func TestConsumeUniqueValueTruncationBoundaries(t *testing.T) {
	tests := []struct {
		raw     string
		opening json.Delim
		wantErr error
	}{
		{raw: `{`, opening: '{', wantErr: io.EOF},
		{raw: `[`, opening: '[', wantErr: io.EOF},
		{raw: "{ \t\r\n", opening: '{', wantErr: io.EOF},
		{raw: "[ \t\r\n", opening: '[', wantErr: io.EOF},
		{raw: `{"a":1 `, opening: '{', wantErr: io.EOF},
		{raw: `[1 `, opening: '[', wantErr: io.EOF},
		{raw: `{"a":[ `, opening: '[', wantErr: io.EOF},
		{raw: `[{"a":1 `, opening: '{', wantErr: io.EOF},
		{raw: "[1" + strings.Repeat(" \t\r\n", 1024), opening: '[', wantErr: io.EOF},
		{raw: `{"a"`, wantErr: io.EOF},
		{raw: `{"a":`, wantErr: io.EOF},
		{raw: `[1, `, wantErr: io.EOF},
		{raw: `{"a":1, `, wantErr: io.EOF},
		{raw: "[1," + strings.Repeat(" \t\r\n", 1024), wantErr: io.EOF},
		{raw: `[tru`, wantErr: io.ErrUnexpectedEOF},
		{raw: `{"a":"x`, wantErr: io.ErrUnexpectedEOF},
		{raw: `{]`, opening: '{'},
		{raw: `[}`, opening: '['},
		{raw: `[1 }`, opening: '['},
		{raw: `[1,}`},
		{raw: `{"a":1,]`},
		{raw: `{, `},
		{raw: `[, `},
		{raw: `[1,, `},
		{raw: "[1\u00a0"},
	}
	for index, test := range tests {
		for _, split := range []bool{false, true} {
			t.Run(fmt.Sprintf("%02d/split=%v", index, split), func(t *testing.T) {
				var reader io.Reader = strings.NewReader(test.raw)
				if split {
					reader = iotest.OneByteReader(reader)
				}
				err := ConsumeUniqueValue(json.NewDecoder(reader))
				var termination *ContainerTerminationError
				if got := errors.As(err, &termination); got != (test.opening != 0) || got && termination.Opening != test.opening {
					t.Fatalf("ConsumeUniqueValue(%q), split=%v: termination = %#v, error = %v", test.raw, split, termination, err)
				}
				if test.wantErr != nil {
					if !errors.Is(err, test.wantErr) || err.Error() != test.wantErr.Error() {
						t.Fatalf("ConsumeUniqueValue(%q), split=%v: error = %T %v, want %v", test.raw, split, err, err, test.wantErr)
					}
				} else {
					var syntax *json.SyntaxError
					if !errors.As(err, &syntax) {
						t.Fatalf("ConsumeUniqueValue(%q), split=%v: error = %T %v, want syntax error", test.raw, split, err, err)
					}
				}
			})
		}
	}
}

func TestConsumeUniqueValuePreservesReaderErrors(t *testing.T) {
	var value any
	unrelatedSyntax := json.Unmarshal([]byte(`{]`), &value)
	wrappedSyntax := fmt.Errorf("synthetic reader: %w", json.Unmarshal([]byte(`{`), &value))
	for _, wantErr := range []error{errors.New("synthetic read failure"), io.ErrUnexpectedEOF, unrelatedSyntax, wrappedSyntax} {
		for _, raw := range []string{`{ `, `[ `, `{"a":1 `, `[1 `} {
			reader := io.MultiReader(strings.NewReader(raw), iotest.ErrReader(wantErr))
			err := ConsumeUniqueValue(json.NewDecoder(reader))
			var termination *ContainerTerminationError
			if !errors.As(err, &termination) || termination.Err != wantErr || !errors.Is(err, wantErr) || err.Error() != wantErr.Error() {
				t.Fatalf("ConsumeUniqueValue(%q) error = %T %v, want preserved reader error %v", raw, err, err, wantErr)
			}
		}
	}
}

func TestUniqueSingleDocumentAdmission(t *testing.T) {
	for _, raw := range []string{`{}`, `[]`, `null`, `{"a":1,"b":[{"a":2},{"a":3}]}`} {
		decoder := json.NewDecoder(iotest.OneByteReader(strings.NewReader(raw)))
		if err := ConsumeUniqueValue(decoder); err != nil {
			t.Fatalf("ConsumeUniqueValue(%q): %v", raw, err)
		}
		if err := RequireEOF(decoder); err != nil {
			t.Fatalf("RequireEOF(%q): %v", raw, err)
		}
	}
	for _, raw := range []string{`{"a":1,"\u0061":2}`, `[{"a":1,"a":2}]`, `{"b":{"a":1,"a":2}}`} {
		var duplicate *DuplicateKeyError
		err := ConsumeUniqueValue(json.NewDecoder(iotest.OneByteReader(strings.NewReader(raw))))
		if !errors.As(err, &duplicate) || duplicate.Key != "a" {
			t.Fatalf("ConsumeUniqueValue(%q) error = %T %v, want duplicate key", raw, err, err)
		}
	}
	for _, raw := range []string{`{} []`, `{} {`, `{} tru`, `{} 0x`, `{} "`, `{} {"a":1,"a":2}`} {
		decoder := json.NewDecoder(iotest.OneByteReader(strings.NewReader(raw)))
		if err := ConsumeUniqueValue(decoder); err != nil {
			t.Fatalf("ConsumeUniqueValue(%q): %v", raw, err)
		}
		if err := RequireEOF(decoder); err == nil {
			t.Fatalf("RequireEOF(%q) accepted a trailing document", raw)
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
