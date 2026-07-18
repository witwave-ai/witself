package avatar

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// MaxSeedAgentIDBytes bounds the identity input used for placeholders.
	MaxSeedAgentIDBytes = 256
	// MaxSeedAgentNameBytes bounds the name input used for placeholders.
	MaxSeedAgentNameBytes = 512
)

// ErrInvalidPlaceholderSeed marks an invalid deterministic placeholder seed.
var ErrInvalidPlaceholderSeed = errors.New("invalid avatar placeholder seed")

var placeholderBackgrounds = []string{"#DCEAF5", "#E5E1FA", "#FBE5DD", "#E1F2ED"}
var placeholderAccents = []string{"#2A9D8F", "#E76F51", "#7868E6", "#D49A32"}
var placeholderFaces = []string{"#E9C46A", "#F0C8A0", "#C89B7B", "#9B6B4C"}

// GeneratePlaceholderSVG creates a deterministic, safe flat-vector portrait
// from an agent's stable ID and realm-scoped name. Neither input is embedded in
// the SVG; only a one-way digest-derived identifier and closed-set visual
// choices are used. The placeholder therefore works without model access.
func GeneratePlaceholderSVG(agentID, agentName string) ([]byte, error) {
	digest, err := placeholderSeedDigest(agentID, agentName)
	if err != nil {
		return nil, err
	}
	seedID := "avatar-seed-" + hex.EncodeToString(digest[:6])
	background := placeholderBackgrounds[int(digest[6])%len(placeholderBackgrounds)]
	accent := placeholderAccents[int(digest[7])%len(placeholderAccents)]
	face := placeholderFaces[int(digest[8])%len(placeholderFaces)]
	headRX := 92 + int(digest[9])%21
	eyeRadius := 8 + int(digest[10])%5
	shoulderInset := 104 + int(digest[11])%17
	mouthY := 282 + int(digest[12])%13

	raw := []byte(fmt.Sprintf(`<svg xmlns="%s" id="%s" viewBox="0 0 512 512" width="512" height="512" role="img" aria-label="Deterministic agent avatar placeholder"><title>Agent avatar placeholder</title><desc>A model-free flat-vector placeholder awaiting the agent's initial avatar.</desc><g id="background" data-layer="background"><rect x="0" y="0" width="512" height="512" fill="#F7FAFC"></rect><circle cx="256" cy="256" r="220" fill="%s"></circle></g><g id="base-identity" data-layer="base-identity"><path d="M%d 472 C124 374 176 334 256 334 C336 334 388 374 %d 472 Z" fill="%s" stroke="#203247" stroke-width="8" stroke-linejoin="round"></path><ellipse cx="256" cy="230" rx="%d" ry="116" fill="%s" stroke="#203247" stroke-width="8"></ellipse><path d="M152 218 C158 132 204 106 256 106 C308 106 354 132 360 218 C330 180 294 164 256 164 C218 164 182 180 152 218 Z" fill="%s" stroke="#203247" stroke-width="8" stroke-linejoin="round"></path></g><g id="attire" data-layer="attire"><path d="M214 348 L256 390 L298 348" fill="none" stroke="#F7FAFC" stroke-width="12" stroke-linecap="round" stroke-linejoin="round"></path></g><g id="expression" data-layer="expression"><circle cx="214" cy="238" r="%d" fill="#203247"></circle><circle cx="298" cy="238" r="%d" fill="#203247"></circle><path d="M220 %d C240 302 272 302 292 %d" fill="none" stroke="#203247" stroke-width="8" stroke-linecap="round"></path></g><g id="experience" data-layer="experience"></g></svg>`,
		svgNamespace, seedID, background, shoulderInset, 512-shoulderInset, accent,
		headRX, face, accent, eyeRadius, eyeRadius, mouthY, mouthY,
	))
	return SanitizeSVG(raw)
}

// GeneratePlaceholderSVGForStylePack returns a deterministic model-free
// placeholder that truthfully follows pack. The built-in style keeps its
// seed-derived visual variations. An arbitrary operator-authored pack cannot
// be varied safely without a renderer, so its canonical human reference is
// used as the neutral fallback instead of falsely labeling the built-in art as
// custom-style output.
func GeneratePlaceholderSVGForStylePack(agentID, agentName string, pack StylePack) ([]byte, error) {
	if err := pack.Validate(); err != nil {
		return nil, err
	}
	if pack.ID == DefaultStylePackID && pack.Version == BuiltInStylePackVersion {
		placeholder, err := GeneratePlaceholderSVG(agentID, agentName)
		if err != nil {
			return nil, err
		}
		return SanitizeSVGForStylePack(placeholder, pack)
	}
	// Validate both seed inputs even though custom-pack fallback rendering is
	// deliberately neutral. This keeps the API's identity-input contract
	// consistent and prevents a later renderer from silently accepting inputs
	// rejected by the built-in path.
	if _, err := placeholderSeedDigest(agentID, agentName); err != nil {
		return nil, err
	}
	for _, reference := range pack.References {
		if reference.SubjectForm == SubjectHuman {
			return SanitizeSVGForStylePack([]byte(reference.SVG), pack)
		}
	}
	return nil, fmt.Errorf("%w: style pack has no human placeholder reference", ErrInvalidStylePack)
}

func placeholderSeedDigest(agentID, agentName string) ([sha256.Size]byte, error) {
	agentID = strings.TrimSpace(agentID)
	agentName = strings.TrimSpace(agentName)
	if err := validateSeedPart("agent ID", agentID, MaxSeedAgentIDBytes); err != nil {
		return [sha256.Size]byte{}, err
	}
	if err := validateSeedPart("agent name", agentName, MaxSeedAgentNameBytes); err != nil {
		return [sha256.Size]byte{}, err
	}
	return sha256.Sum256([]byte(agentID + "\x00" + agentName)), nil
}

func validateSeedPart(name, value string, maximum int) error {
	if value == "" || len(value) > maximum || !utf8.ValidString(value) {
		return fmt.Errorf("%w: %s must contain 1-%d bytes of valid UTF-8", ErrInvalidPlaceholderSeed, name, maximum)
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return fmt.Errorf("%w: %s contains a control character", ErrInvalidPlaceholderSeed, name)
		}
	}
	return nil
}
