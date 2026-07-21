// Command golden-go verifies that the Cloudflare Worker relay golden vector
// matches the production Go canonicalization and Ed25519 verifier.
package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/witwave-ai/witself/internal/agentemail"
)

type vector struct {
	Metadata struct {
		Timestamp    int64  `json:"timestamp"`
		KeyID        string `json:"key_id"`
		EnvelopeFrom string `json:"envelope_from"`
		EnvelopeTo   string `json:"envelope_to"`
		Audience     string `json:"audience"`
		RawSize      int64  `json:"raw_size"`
		RawSHA256    string `json:"raw_sha256"`
	} `json:"metadata"`
	PKCS8Base64     string `json:"pkcs8_base64"`
	PublicKeyBase64 string `json:"public_key_base64"`
	RawBase64       string `json:"raw_base64"`
	CanonicalBase64 string `json:"canonical_base64"`
	SignatureBase64 string `json:"signature_base64"`
}

func decode(name, value string) []byte {
	result, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		panic(fmt.Sprintf("decode %s: %v", name, err))
	}
	return result
}

func main() {
	if len(os.Args) != 2 {
		panic("usage: golden-go <golden-vector.json>")
	}
	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		panic(err)
	}
	var v vector
	if err := json.Unmarshal(data, &v); err != nil {
		panic(err)
	}
	raw := decode("raw", v.RawBase64)
	digest := sha256.Sum256(raw)
	if int64(len(raw)) != v.Metadata.RawSize || fmt.Sprintf("%x", digest[:]) != v.Metadata.RawSHA256 {
		panic("raw body does not match vector metadata")
	}
	metadata := agentemail.RelayMetadata{
		Timestamp:         v.Metadata.Timestamp,
		KeyID:             v.Metadata.KeyID,
		EnvelopeSender:    v.Metadata.EnvelopeFrom,
		EnvelopeRecipient: v.Metadata.EnvelopeTo,
		Audience:          v.Metadata.Audience,
		RawSize:           v.Metadata.RawSize,
		RawSHA256:         v.Metadata.RawSHA256,
	}
	canonical, err := agentemail.CanonicalSignatureInput(metadata)
	if err != nil {
		panic(err)
	}
	if base64.StdEncoding.EncodeToString(canonical) != v.CanonicalBase64 {
		panic("Go canonical bytes do not match Worker vector")
	}
	publicKey := ed25519.PublicKey(decode("public key", v.PublicKeyBase64))
	signature := decode("signature", v.SignatureBase64)
	if _, err := agentemail.VerifyRelay(
		time.Unix(v.Metadata.Timestamp, 0), time.Minute, publicKey, metadata, raw, signature,
	); err != nil {
		panic(fmt.Sprintf("Go relay verifier rejected Worker vector: %v", err))
	}
	parsed, err := x509.ParsePKCS8PrivateKey(decode("PKCS8", v.PKCS8Base64))
	if err != nil {
		panic(err)
	}
	privateKey, ok := parsed.(ed25519.PrivateKey)
	if !ok || !ed25519.Verify(privateKey.Public().(ed25519.PublicKey), canonical, signature) {
		panic("Go PKCS8 key does not match Worker vector")
	}
	fmt.Println("Go verified Cloudflare Worker relay golden vector")
}
