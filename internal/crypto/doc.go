// Package crypto owns the sealed-plane envelope: the CMK -> per-realm KEK ->
// per-secret/field DEK hierarchy, AEAD seal/open (XCHACHA20_POLY1305,
// AES_256_GCM), AAD binding, DEK wrap/unwrap, and the reveal machinery for
// `secret reveal` / `totp code`, alongside token hashing and transport. The
// open plane (memories/facts) uses ordinary data-at-rest, not this package.
package crypto
