package billing

import (
	"strings"
	"testing"
)

func TestValidateOperationID(t *testing.T) {
	t.Parallel()

	const (
		lengthError    = "billing operation id must be 1-128 characters"
		characterError = "billing operation id contains unsupported characters"
	)

	tests := []struct {
		name        string
		operationID string
		wantError   string
	}{
		{name: "minimum length", operationID: "a"},
		{name: "maximum length", operationID: strings.Repeat("a", 128)},
		{
			name:        "portable alphabet",
			operationID: "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._:-",
		},
		{name: "empty", operationID: "", wantError: lengthError},
		{name: "over maximum length", operationID: strings.Repeat("a", 129), wantError: lengthError},
		{name: "space", operationID: "operation id", wantError: characterError},
		{name: "slash", operationID: "operation/id", wantError: characterError},
		{name: "at sign", operationID: "operation@id", wantError: characterError},
		{name: "newline", operationID: "operation\nid", wantError: characterError},
		{name: "nul byte", operationID: "operation\x00id", wantError: characterError},
		{name: "non ASCII rune", operationID: "operation-é", wantError: characterError},
		{name: "invalid UTF-8 byte", operationID: "operation-\xff", wantError: characterError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateOperationID(tt.operationID)
			if tt.wantError == "" {
				if err != nil {
					t.Fatalf("ValidateOperationID(%q) returned unexpected error: %v", tt.operationID, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateOperationID(%q) returned nil error; want %q", tt.operationID, tt.wantError)
			}
			if err.Error() != tt.wantError {
				t.Fatalf("ValidateOperationID(%q) error = %q; want %q", tt.operationID, err, tt.wantError)
			}
		})
	}
}

func TestValidateProviderObjectID(t *testing.T) {
	t.Parallel()

	const (
		lengthError    = "billing provider object id must be 1-255 characters"
		characterError = "billing provider object id contains unsupported characters"
	)

	tests := []struct {
		name      string
		objectID  string
		wantError string
	}{
		{name: "minimum length", objectID: "a"},
		{name: "maximum length", objectID: strings.Repeat("a", 255)},
		{
			name:     "portable alphabet",
			objectID: "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._:-",
		},
		{name: "empty", objectID: "", wantError: lengthError},
		{name: "over maximum length", objectID: strings.Repeat("a", 256), wantError: lengthError},
		{name: "space", objectID: "object id", wantError: characterError},
		{name: "slash", objectID: "object/id", wantError: characterError},
		{name: "at sign", objectID: "object@id", wantError: characterError},
		{name: "newline", objectID: "object\nid", wantError: characterError},
		{name: "nul byte", objectID: "object\x00id", wantError: characterError},
		{name: "non ASCII rune", objectID: "object-é", wantError: characterError},
		{name: "invalid UTF-8 byte", objectID: "object-\xff", wantError: characterError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateProviderObjectID(tt.objectID)
			if tt.wantError == "" {
				if err != nil {
					t.Fatalf("ValidateProviderObjectID(%q) returned unexpected error: %v", tt.objectID, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateProviderObjectID(%q) returned nil error; want %q", tt.objectID, tt.wantError)
			}
			if err.Error() != tt.wantError {
				t.Fatalf("ValidateProviderObjectID(%q) error = %q; want %q", tt.objectID, err, tt.wantError)
			}
		})
	}
}
