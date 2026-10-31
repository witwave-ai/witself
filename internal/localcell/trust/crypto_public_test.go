package trust_test

import (
	"errors"
	"math/big"
	"runtime"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/localcell/trust"
)

func TestPublicKeyFromRSARefusesNegativeModulus(t *testing.T) {
	key := fixturePrivate(t, "root-a").PublicKey
	key.N = new(big.Int).Neg(new(big.Int).Set(key.N))
	if _, err := trust.PublicKeyFromRSA(&key); !errors.Is(err, trust.ReasonInvalidKey) {
		t.Fatal("negative RSA modulus was not refused with invalid_key")
	}
}

func TestVerifySignatureBoundsEncodedInput(t *testing.T) {
	key := fixturePublic(t, "root-a")
	signature := trust.Signature{KeyID: key.KeyID, Signature: strings.Repeat("A", 4<<20)}
	// The public helper must refuse before allocating a decoded copy. The wire
	// envelope already caps its own input, but direct callers need the same bound.
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	err := trust.VerifySignature([]byte("fixture"), key, signature)
	runtime.ReadMemStats(&after)
	if !errors.Is(err, trust.ReasonSignatureLength) {
		t.Fatal("oversized signature was not refused with signature_length")
	}
	if after.TotalAlloc-before.TotalAlloc > 1<<20 {
		t.Fatal("public verifier allocated an oversized decoded signature")
	}
}
