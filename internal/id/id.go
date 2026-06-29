// Package id generates prefixed unique identifiers like acc_<random> (and later
// realm_, agent_, mem_, …). The body is crypto-random, base32-encoded, lowercased.
package id

import (
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"strings"
)

// enc is unpadded standard base32; we lowercase the output for readable ids.
var enc = base32.StdEncoding.WithPadding(base32.NoPadding)

// entropyBytes is the random body size: 80 bits.
const entropyBytes = 10

// New returns a prefixed identifier, e.g. New("acc") -> "acc_kf3n…".
func New(prefix string) (string, error) {
	b := make([]byte, entropyBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	return prefix + "_" + strings.ToLower(enc.EncodeToString(b)), nil
}
