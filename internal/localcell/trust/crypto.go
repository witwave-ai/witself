package trust

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
)

func decodeBase64(value string) ([]byte, error) {
	raw, err := base64.StdEncoding.Strict().DecodeString(value)
	if err != nil || base64.StdEncoding.EncodeToString(raw) != value {
		return nil, ReasonInvalidEncoding
	}
	return raw, nil
}

// PublicKeyFromRSA encodes and identifies an exact-profile RSA public key.
func PublicKeyFromRSA(key *rsa.PublicKey) (PublicKey, error) {
	if key == nil || key.N == nil || key.N.Sign() <= 0 || key.N.BitLen() != 3072 || key.E != 65537 {
		return PublicKey{}, ReasonInvalidKey
	}
	der, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		return PublicKey{}, ReasonInvalidKey
	}
	hash := sha256.Sum256(der)
	return PublicKey{KeyID: hex.EncodeToString(hash[:]), SPKI: base64.StdEncoding.EncodeToString(der)}, nil
}

// ParsePublicKey validates canonical SPKI encoding, identifier and RSA parameters.
func ParsePublicKey(key PublicKey) (*rsa.PublicKey, error) {
	if len(key.SPKI) > 4096 || !digestPattern.MatchString(key.KeyID) {
		return nil, ReasonInvalidKey
	}
	der, err := decodeBase64(key.SPKI)
	if err != nil {
		return nil, err
	}
	parsed, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, ReasonInvalidKey
	}
	rsaKey, ok := parsed.(*rsa.PublicKey)
	if !ok {
		return nil, ReasonInvalidKey
	}
	canonical, err := PublicKeyFromRSA(rsaKey)
	if err != nil {
		return nil, err
	}
	if canonical != key {
		return nil, ReasonInvalidKey
	}
	return rsaKey, nil
}

// ParseEnvelope validates the detached envelope before any document interpretation.
func ParseEnvelope(raw []byte) (Envelope, error) {
	var e Envelope
	if err := parseDocument(raw, MaxEnvelopeBytes, &e, false); err != nil {
		return e, err
	}
	if e.FormatVersion != 1 {
		return e, ReasonInvalidDocument
	}
	if e.Algorithm != Algorithm {
		return e, ReasonWrongAlgorithm
	}
	n := 1
	switch e.Kind {
	case "component_set", "delegation":
	case "root_rotation":
		n = 2
	default:
		return e, ReasonWrongKind
	}
	if len(e.Signatures) != n {
		return e, ReasonSignatureCount
	}
	seen := map[string]bool{}
	for _, sig := range e.Signatures {
		if !digestPattern.MatchString(sig.KeyID) {
			return e, ReasonInvalidKey
		}
		if seen[sig.KeyID] {
			return e, ReasonSignatureCount
		}
		seen[sig.KeyID] = true
		signature, err := decodeBase64(sig.Signature)
		if err != nil {
			return e, err
		}
		if len(signature) != 384 {
			return e, ReasonSignatureLength
		}
	}
	return e, nil
}

// VerifySignature authenticates the exact raw document bytes.
func VerifySignature(raw []byte, key PublicKey, sig Signature) error {
	if len(sig.Signature) != 512 {
		return ReasonSignatureLength
	}
	public, err := ParsePublicKey(key)
	if err != nil {
		return err
	}
	if sig.KeyID != key.KeyID {
		return ReasonWrongSigner
	}
	signature, err := decodeBase64(sig.Signature)
	if err != nil {
		return err
	}
	if len(signature) != 384 {
		return ReasonSignatureLength
	}
	hash := sha256.Sum256(raw)
	if err = rsa.VerifyPKCS1v15(public, crypto.SHA256, hash[:], signature); err != nil {
		return ReasonSignatureInvalid
	}
	return nil
}

// SignDocument is offline fixture tooling; it never selects a production key.
func SignDocument(raw []byte, kind string, keys ...*rsa.PrivateKey) ([]byte, error) {
	n := 1
	if kind == "root_rotation" {
		n = 2
	} else if kind != "delegation" && kind != "component_set" {
		return nil, ReasonWrongKind
	}
	if len(keys) != n {
		return nil, ReasonSignatureCount
	}
	envelope := Envelope{FormatVersion: 1, Kind: kind, Algorithm: Algorithm, Signatures: []Signature{}}
	hash := sha256.Sum256(raw)
	for _, key := range keys {
		if key == nil {
			return nil, ReasonInvalidKey
		}
		public, err := PublicKeyFromRSA(&key.PublicKey)
		if err != nil {
			return nil, err
		}
		signature, err := rsa.SignPKCS1v15(nil, key, crypto.SHA256, hash[:])
		if err != nil {
			return nil, ReasonSignatureInvalid
		}
		envelope.Signatures = append(envelope.Signatures, Signature{KeyID: public.KeyID, Signature: base64.StdEncoding.EncodeToString(signature)})
	}
	return json.Marshal(envelope)
}
