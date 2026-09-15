// Package jsonstrict provides structural checks that encoding/json does not
// apply by default.
package jsonstrict

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// ErrTrailingValue reports that a decoder contains more than one JSON value.
var ErrTrailingValue = errors.New("unexpected trailing JSON value")

// DuplicateKeyError identifies a repeated object key.
type DuplicateKeyError struct {
	Key string
}

func (e *DuplicateKeyError) Error() string {
	return fmt.Sprintf("duplicate JSON object key %q", e.Key)
}

// ContainerTerminationError identifies the container whose closing delimiter
// could not be consumed. Err retains the token error after parser EOF
// normalization, so callers that do not need container-specific wording retain
// their established error contract.
type ContainerTerminationError struct {
	Opening json.Delim
	Err     error
}

func (e *ContainerTerminationError) Error() string { return e.Err.Error() }

func (e *ContainerTerminationError) Unwrap() error { return e.Err }

// ConsumeUniqueValue consumes one JSON value from decoder and rejects duplicate
// object keys at every nesting level.
func ConsumeUniqueValue(decoder *json.Decoder) error {
	token, err := nextToken(decoder)
	if err != nil {
		return err
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for {
			more, err := containerMore(decoder, delimiter)
			if err != nil {
				return err
			}
			if !more {
				break
			}
			keyToken, err := nextToken(decoder)
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("JSON object key is not a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return &DuplicateKeyError{Key: key}
			}
			seen[key] = struct{}{}
			if err := ConsumeUniqueValue(decoder); err != nil {
				return err
			}
		}
		closing, err := nextToken(decoder)
		if err != nil {
			return &ContainerTerminationError{Opening: delimiter, Err: err}
		}
		if closing != json.Delim('}') {
			return &ContainerTerminationError{
				Opening: delimiter,
				Err:     errors.New("object did not terminate"),
			}
		}
	case '[':
		for {
			more, err := containerMore(decoder, delimiter)
			if err != nil {
				return err
			}
			if !more {
				break
			}
			if err := ConsumeUniqueValue(decoder); err != nil {
				return err
			}
		}
		closing, err := nextToken(decoder)
		if err != nil {
			return &ContainerTerminationError{Opening: delimiter, Err: err}
		}
		if closing != json.Delim(']') {
			return &ContainerTerminationError{
				Opening: delimiter,
				Err:     errors.New("array did not terminate"),
			}
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delimiter)
	}
	return nil
}

// containerMore preserves the distinction between a missing closing delimiter
// and a missing token after a comma. Some encoding/json implementations return
// true from More after failed lookahead, retaining a syntax error that bypasses
// Token's EOF handling. Successful lookahead leaves a non-whitespace byte in
// Buffered; whitespace alone cannot start a token. A buffered comma still
// counts as more: Token may need to read beyond the current buffer for its value.
func containerMore(decoder *json.Decoder, opening json.Delim) (bool, error) {
	if !decoder.More() {
		return false, nil
	}
	if bufferedToken(decoder, false) {
		return true, nil
	}
	_, err := nextToken(decoder)
	if err == nil {
		return false, errors.New("JSON lookahead did not retain a token")
	}
	return false, &ContainerTerminationError{Opening: opening, Err: err}
}

// nextToken restores Token's EOF contract when More has retained a parser EOF
// as a SyntaxError. Only the exact unexpected-end error with no buffered token
// qualifies; incomplete strings/literals, malformed separators and unrelated
// reader errors retain their original errors. The structural callers use byte
// readers; a reader supplying this exact parser error is indistinguishable from
// parser EOF.
func nextToken(decoder *json.Decoder) (json.Token, error) {
	token, err := decoder.Token()
	if syntax, ok := err.(*json.SyntaxError); ok && syntax.Error() == "unexpected end of JSON input" && !bufferedToken(decoder, true) {
		return nil, io.EOF
	}
	return token, err
}

func bufferedToken(decoder *json.Decoder, skipComma bool) bool {
	buffered := decoder.Buffered()
	var b [1]byte
	for {
		n, err := buffered.Read(b[:])
		if n == 0 {
			return err != io.EOF
		}
		switch b[0] {
		case ' ', '\t', '\r', '\n':
		case ',':
			if !skipComma {
				return true
			}
			skipComma = false
		default:
			return true
		}
	}
}

// RequireEOF verifies that decoder contains no second JSON value. Decoder
// errors are returned unchanged so callers retain the most precise failure.
func RequireEOF(decoder *json.Decoder) error {
	var trailing any
	err := decoder.Decode(&trailing)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return err
	}
	return ErrTrailingValue
}
