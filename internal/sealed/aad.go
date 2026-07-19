package sealed

import (
	"encoding/binary"
	"unicode/utf8"
)

const (
	// AADVersionV1 identifies the canonical binary AAD encoder in this package.
	AADVersionV1 uint32 = 1

	// ValueEncodingUTF8 identifies a persisted UTF-8 string value.
	ValueEncodingUTF8 = "utf8"
	// ValueEncodingJSON identifies a persisted JSON value.
	ValueEncodingJSON = "json"
	// ValueEncodingBinary identifies a persisted opaque binary value.
	// Encoding is authenticated but is not a security primitive.
	ValueEncodingBinary = "binary"
)

// ValueDomain is an allowlisted authenticated-data domain.
type ValueDomain string

const (
	// FieldValueDomain separates ordinary sealed field values from other
	// authenticated payloads.
	FieldValueDomain ValueDomain = "witself/sealed-field/v1"
	// TOTPPayloadDomain separates sealed TOTP enrollment payloads from ordinary
	// field values.
	TOTPPayloadDomain = ValueDomain("witself/totp-payload/v1")
	dekWrapDomain     = "witself/dek-wrap/v1"
)

// FieldScope contains the immutable database coordinates common to a field
// value and its wrapped DEK. Mutable names, tags, timestamps, tokens, cells,
// and cloud providers are intentionally absent.
type FieldScope struct {
	Domain       ValueDomain
	AccountID    string
	RealmID      string
	OwnerAgentID string
	SecretID     string
	FieldID      string
}

// ValueAADBinding contains every value-envelope coordinate authenticated by
// the v1 field or TOTP AEAD.
type ValueAADBinding struct {
	FieldScope
	DEKID         string
	ValueVersion  uint64
	DEKGeneration uint64
	ValueEncoding string
	AEADAlgorithm string
}

// DEKWrapAADBinding contains every coordinate authenticated while wrapping a
// field-generation DEK under an AVK.
type DEKWrapAADBinding struct {
	FieldScope
	DEKID              string
	DEKGeneration      uint64
	WrappingKeyID      string
	WrappingKeyVersion uint64
	WrapRevision       uint64
	WrapAlgorithm      string
}

// CanonicalValueAAD encodes the v1 value binding. The fixed domain is followed
// by unsigned 32-bit length-prefixed UTF-8 strings and big-endian uint64
// counters in the exact order documented by ADR 0003.
func CanonicalValueAAD(binding ValueAADBinding) ([]byte, error) {
	if err := validateValueAADBinding(binding); err != nil {
		return nil, err
	}
	out := make([]byte, 0, 256)
	out = append(out, string(binding.Domain)...)
	out = appendString(out, binding.AccountID)
	out = appendString(out, binding.RealmID)
	out = appendString(out, binding.OwnerAgentID)
	out = appendString(out, binding.SecretID)
	out = appendString(out, binding.FieldID)
	out = appendString(out, binding.DEKID)
	out = binary.BigEndian.AppendUint64(out, binding.ValueVersion)
	out = binary.BigEndian.AppendUint64(out, binding.DEKGeneration)
	out = appendString(out, binding.ValueEncoding)
	out = appendString(out, binding.AEADAlgorithm)
	return out, nil
}

// CanonicalDEKWrapAAD encodes the independent v1 DEK-wrap binding.
func CanonicalDEKWrapAAD(binding DEKWrapAADBinding) ([]byte, error) {
	if err := validateDEKWrapAADBinding(binding); err != nil {
		return nil, err
	}
	out := make([]byte, 0, 256)
	out = append(out, dekWrapDomain...)
	out = appendString(out, binding.AccountID)
	out = appendString(out, binding.RealmID)
	out = appendString(out, binding.OwnerAgentID)
	out = appendString(out, binding.SecretID)
	out = appendString(out, binding.FieldID)
	out = appendString(out, binding.DEKID)
	out = binary.BigEndian.AppendUint64(out, binding.DEKGeneration)
	out = appendString(out, string(binding.Domain))
	out = appendString(out, binding.WrappingKeyID)
	out = binary.BigEndian.AppendUint64(out, binding.WrappingKeyVersion)
	out = binary.BigEndian.AppendUint64(out, binding.WrapRevision)
	out = appendString(out, binding.WrapAlgorithm)
	return out, nil
}

func appendString(dst []byte, value string) []byte {
	dst = binary.BigEndian.AppendUint32(dst, uint32(len(value)))
	return append(dst, value...)
}

func validateFieldScope(scope FieldScope) error {
	if scope.Domain != FieldValueDomain && scope.Domain != TOTPPayloadDomain {
		return ErrInvalidBinding
	}
	for _, check := range []struct {
		value  string
		prefix string
	}{
		{scope.AccountID, "acc"},
		{scope.RealmID, "realm"},
		{scope.OwnerAgentID, "agent"},
		{scope.SecretID, "sec"},
		{scope.FieldID, "fld"},
	} {
		if !validPrefixedID(check.value, check.prefix) {
			return ErrInvalidBinding
		}
	}
	return nil
}

func validateValueAADBinding(binding ValueAADBinding) error {
	if err := validateFieldScope(binding.FieldScope); err != nil ||
		!validPrefixedID(binding.DEKID, "dek") ||
		binding.ValueVersion == 0 || binding.DEKGeneration == 0 ||
		!validValueEncoding(binding.ValueEncoding) ||
		binding.AEADAlgorithm != AES256GCMAlgorithm {
		return ErrInvalidBinding
	}
	return nil
}

func validateDEKWrapAADBinding(binding DEKWrapAADBinding) error {
	if err := validateFieldScope(binding.FieldScope); err != nil ||
		!validPrefixedID(binding.DEKID, "dek") ||
		binding.DEKGeneration == 0 ||
		!validPrefixedID(binding.WrappingKeyID, "avk") ||
		binding.WrappingKeyVersion == 0 || binding.WrapRevision == 0 ||
		binding.WrapAlgorithm != AES256GCMAlgorithm {
		return ErrInvalidBinding
	}
	return nil
}

func validValueEncoding(value string) bool {
	return value == ValueEncodingUTF8 || value == ValueEncodingJSON || value == ValueEncodingBinary
}

func validPrefixedID(value, prefix string) bool {
	if !utf8.ValidString(value) || len(value) != len(prefix)+1+16 ||
		value[:len(prefix)+1] != prefix+"_" {
		return false
	}
	for i := len(prefix) + 1; i < len(value); i++ {
		c := value[i]
		if (c < 'a' || c > 'z') && (c < '2' || c > '7') {
			return false
		}
	}
	return true
}
