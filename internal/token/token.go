// Package token mints and parses Witself's opaque, kind-prefixed credential
// tokens. The wire form is witself_<kind>_<random>: a recognizable prefix (good
// for secret scanners), a routable kind, and an opaque high-entropy body. All
// authority lives in the server's binding for the token, never in the body —
// so the body carries no claims and reveals nothing.
package token

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
)

// prefix is the fixed leading segment shared by every Witself token.
const prefix = "witself"

// entropyBytes is the random payload size: 256 bits.
const entropyBytes = 32

// Kind classifies a token by what kind of principal it is bound to.
type Kind string

const (
	// KindBootstrap is the single-use operator bootstrap token. It carries no
	// standing authority; it is exchanged once for a durable operator token.
	KindBootstrap Kind = "boot"
	// KindOperator is a durable operator (human/admin) token.
	KindOperator Kind = "opr"
	// KindAgent is a machine agent token, bound to one realm.
	KindAgent Kind = "agt"
	// KindFleet is a control-plane fleet token: it authorizes registering and
	// removing cells in the fleet registry. v0 is one shared fleet token (one
	// party); partner-hosted cells later get per-party credentials.
	KindFleet Kind = "flt"
	// KindProvision is a per-cell provision token: the control plane presents it
	// to a cell to create accounts (the control-plane -> cell trust link). Unset
	// on a cell = account provisioning inert (the self-hosted posture).
	KindProvision Kind = "prv"
	// KindAdmin is a fleet-admin token, authenticated by the control plane
	// against its admin registry (DIRECTORY KV). Minted by the Cloudflare
	// Worker (not this package) — registered here so the prefix namespace
	// has one canonical home. See infra/cloudflare/control-plane.
	KindAdmin Kind = "adm"
)

// New mints a fresh opaque token of the given kind: witself_<kind>_<random>.
func New(kind Kind) (string, error) {
	b := make([]byte, entropyBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	body := base64.RawURLEncoding.EncodeToString(b)
	return prefix + "_" + string(kind) + "_" + body, nil
}

// Parse splits a token into its kind and opaque body, validating the prefix.
// It does not validate the token against any binding — that is the server's job.
func Parse(tok string) (Kind, string, error) {
	parts := strings.SplitN(tok, "_", 3)
	if len(parts) != 3 || parts[0] != prefix || parts[1] == "" || parts[2] == "" {
		return "", "", fmt.Errorf("not a witself token")
	}
	return Kind(parts[1]), parts[2], nil
}
