// Package sealed implements the client-side cryptographic boundary for
// Witself's agent-owned sealed plane. It never performs network or storage
// operations and never exposes raw vault or data-encryption keys.
package sealed

import "errors"

var (
	// ErrIntegrity deliberately collapses wrong-key, wrong-binding, malformed
	// envelope, and authentication failures into one value-free result.
	ErrIntegrity = errors.New("sealed material failed integrity verification")

	// ErrInvalidBinding identifies caller-supplied envelope coordinates that
	// cannot form canonical authenticated data.
	ErrInvalidBinding = errors.New("sealed binding is invalid")

	// ErrInvalidKeyEncoding identifies a malformed or unsupported local AVK
	// record. It never includes record bytes or key material.
	ErrInvalidKeyEncoding = errors.New("agent vault key record is invalid")

	// ErrKeyDisclosure prevents generic serialization from accidentally
	// exporting raw AVK material.
	ErrKeyDisclosure = errors.New("agent vault key cannot be serialized generically")

	// ErrInvalidPasswordPolicy identifies a password request that cannot meet
	// its requested character-class guarantees.
	ErrInvalidPasswordPolicy = errors.New("password generation policy is invalid")

	// ErrInvalidTOTPPayload identifies malformed or unsupported TOTP setup
	// material without echoing any seed, URI, label, or generated code.
	ErrInvalidTOTPPayload = errors.New("totp payload is invalid")

	// ErrTOTPDisclosure prevents generic serialization of a private TOTP
	// payload. EncodeTOTPPayload is the deliberate plaintext-to-envelope path.
	ErrTOTPDisclosure = errors.New("totp payload cannot be serialized generically")

	// ErrInvalidTOTPTime identifies a time that cannot form a bounded RFC 6238
	// moving factor or expiration timestamp.
	ErrInvalidTOTPTime = errors.New("totp generation time is invalid")

	// ErrRandomSource identifies failure of the operating system's
	// cryptographic random source without exposing any value being processed.
	ErrRandomSource = errors.New("cryptographic random source unavailable")

	// ErrInvalidEnrollment identifies malformed, unsupported, or non-canonical
	// AVK enrollment material. It never includes public-key or secret bytes.
	ErrInvalidEnrollment = errors.New("agent vault key enrollment material is invalid")

	// ErrEnrollmentIntegrity deliberately collapses a wrong recipient, wrong
	// pairing secret, changed immutable binding, malformed package, and AEAD
	// authentication failure into one value-free result.
	ErrEnrollmentIntegrity = errors.New("agent vault key enrollment failed integrity verification")

	// ErrEnrollmentDisclosure prevents generic serialization from accidentally
	// exporting an enrollment private key, pairing secret, or consume proof.
	ErrEnrollmentDisclosure = errors.New("agent vault key enrollment secret cannot be serialized generically")

	// ErrInvalidRecovery identifies a malformed, unsupported, non-canonical, or
	// caller-invalid AVK recovery package without exposing package contents.
	ErrInvalidRecovery = errors.New("agent vault key recovery package is invalid")

	// ErrRecoveryIntegrity deliberately collapses wrong-passphrase, wrong-scope,
	// ciphertext, metadata, and recovered-key verification failures.
	ErrRecoveryIntegrity = errors.New("agent vault key recovery failed integrity verification")

	// ErrInvalidRecoveryPassphrase identifies only a violated public length
	// policy and never includes the passphrase or any derived material.
	ErrInvalidRecoveryPassphrase = errors.New("agent vault key recovery passphrase does not satisfy policy")
)
