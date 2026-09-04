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
// could not be consumed. It preserves the decoder's underlying error text and
// identity so callers that do not need container-specific wording retain their
// established error contract.
type ContainerTerminationError struct {
	Opening json.Delim
	Err     error
}

func (e *ContainerTerminationError) Error() string { return e.Err.Error() }

func (e *ContainerTerminationError) Unwrap() error { return e.Err }

// ConsumeUniqueValue consumes one JSON value from decoder and rejects duplicate
// object keys at every nesting level.
func ConsumeUniqueValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
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
		for decoder.More() {
			keyToken, err := decoder.Token()
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
		closing, err := decoder.Token()
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
		for decoder.More() {
			if err := ConsumeUniqueValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
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
