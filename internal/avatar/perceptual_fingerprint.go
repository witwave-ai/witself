package avatar

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"math"
	"math/bits"
)

const (
	// PerceptualContinuityFingerprintVersion identifies the stable binary
	// continuity projection format. A version is decoded only by its exact
	// layout; new layouts must use a new version.
	PerceptualContinuityFingerprintVersion = 1

	perceptualFingerprintHeaderBytes       = 12
	perceptualFingerprintStyleDigestBytes  = sha256.Size
	perceptualFingerprintPixelCount        = PerceptualRenderSize * PerceptualRenderSize
	perceptualFingerprintRGBBytes          = perceptualFingerprintPixelCount * 3
	perceptualFingerprintIdentityMaskBytes = (perceptualFingerprintPixelCount + 7) / 8
	perceptualFingerprintInfluenceBytes    = perceptualFingerprintPixelCount
	perceptualFingerprintPayloadBytes      = perceptualFingerprintStyleDigestBytes +
		perceptualFingerprintRGBBytes + perceptualFingerprintIdentityMaskBytes +
		perceptualFingerprintInfluenceBytes
	perceptualFingerprintChecksumBytes = sha256.Size

	// PerceptualContinuityFingerprintBytes is the exact persisted byte length
	// for the current format. The fixed size makes storage and decoder bounds
	// enforceable before any rendering or allocation proportional to input.
	PerceptualContinuityFingerprintBytes = perceptualFingerprintHeaderBytes +
		perceptualFingerprintPayloadBytes + perceptualFingerprintChecksumBytes
)

const perceptualFingerprintMagic = "WAPF"

var (
	// ErrInvalidPerceptualFingerprint marks an unsupported, corrupt, truncated,
	// style-mismatched, or otherwise non-canonical continuity fingerprint.
	ErrInvalidPerceptualFingerprint = errors.New("invalid avatar perceptual continuity fingerprint")
)

type decodedPerceptualContinuityFingerprint struct {
	styleDigest       []byte
	wholeRGB          []byte
	identityMask      []byte
	unlockedInfluence []byte
}

// BuildPerceptualContinuityFingerprint converts a full parent SVG into the
// exact bounded projection required to compare a later child after historical
// SVG compaction. The fingerprint contains raster-derived avatar content and
// must be protected and lifecycle-managed like the SVG it replaces.
func BuildPerceptualContinuityFingerprint(parent []byte, pack StylePack) (fingerprint []byte, err error) {
	defer func() {
		if recover() != nil {
			fingerprint = nil
			err = fmt.Errorf("%w: build continuity fingerprint", ErrPerceptualRender)
		}
	}()
	if err := ValidatePerceptualV1StylePack(pack); err != nil {
		return nil, err
	}
	parentSVG, err := SanitizeSVGForPerceptualV1StylePack(parent, pack)
	if err != nil {
		return nil, err
	}
	parentFull, identityMask, parentLocked, err := renderPerceptualParentProjection(parentSVG, pack)
	if err != nil {
		return nil, err
	}
	styleDigest, err := perceptualStyleDigest(pack)
	if err != nil {
		return nil, err
	}
	return encodePerceptualContinuityFingerprint(styleDigest, parentFull, identityMask, parentLocked), nil
}

// ValidatePerceptualContinuityFingerprint verifies the exact format, version,
// fixed lengths, reserved fields, and checksum without rendering an SVG.
func ValidatePerceptualContinuityFingerprint(fingerprint []byte) error {
	_, err := decodePerceptualContinuityFingerprint(fingerprint)
	return err
}

// ValidatePerceptualContinuityFingerprintForStyle additionally verifies that
// the fingerprint was built from the exact immutable style-pack content.
func ValidatePerceptualContinuityFingerprintForStyle(fingerprint []byte, pack StylePack) error {
	if err := ValidatePerceptualV1StylePack(pack); err != nil {
		return err
	}
	decoded, err := decodePerceptualContinuityFingerprint(fingerprint)
	if err != nil {
		return err
	}
	return validatePerceptualFingerprintStyle(decoded, pack)
}

func renderPerceptualParentProjection(parentSVG []byte, pack StylePack) (*image.RGBA, *image.RGBA, *image.RGBA, error) {
	parentFull, err := renderPerceptualAvatar(parentSVG)
	if err != nil {
		return nil, nil, nil, err
	}
	lockedIdentity, allLocked, _ := perceptualLayerSelections(pack)
	identitySVG, err := selectPerceptualLayers(parentSVG, pack, lockedIdentity)
	if err != nil {
		return nil, nil, nil, err
	}
	identityMask, err := renderPerceptualAvatar(identitySVG)
	if err != nil {
		return nil, nil, nil, err
	}
	if pixels := countPerceptualMaskPixels(identityMask); pixels < perceptualMinimumMaskPixels {
		return nil, nil, nil, fmt.Errorf("%w: locked identity projection covers %d pixels, want at least %d",
			ErrPerceptualRender, pixels, perceptualMinimumMaskPixels)
	}
	parentLockedSVG, err := selectPerceptualLayers(parentSVG, pack, allLocked)
	if err != nil {
		return nil, nil, nil, err
	}
	parentLocked, err := renderPerceptualAvatar(parentLockedSVG)
	if err != nil {
		return nil, nil, nil, err
	}
	return parentFull, identityMask, parentLocked, nil
}

func encodePerceptualContinuityFingerprint(styleDigest [sha256.Size]byte, parentFull, identityMask, parentLocked *image.RGBA) []byte {
	fingerprint := make([]byte, PerceptualContinuityFingerprintBytes)
	copy(fingerprint[:4], perceptualFingerprintMagic)
	fingerprint[4] = PerceptualContinuityFingerprintVersion
	fingerprint[5] = PerceptualRenderSize
	// Bytes 6-7 are reserved flags and remain zero.
	binary.BigEndian.PutUint32(fingerprint[8:12], perceptualFingerprintPayloadBytes)
	offset := perceptualFingerprintHeaderBytes
	copy(fingerprint[offset:offset+sha256.Size], styleDigest[:])
	offset += sha256.Size
	rgbOffset := offset
	maskOffset := rgbOffset + perceptualFingerprintRGBBytes
	influenceOffset := maskOffset + perceptualFingerprintIdentityMaskBytes
	for y := 0; y < PerceptualRenderSize; y++ {
		for x := 0; x < PerceptualRenderSize; x++ {
			pixel := y*PerceptualRenderSize + x
			rgb := perceptualCompositeRGB(parentFull.RGBAAt(x, y))
			copy(fingerprint[rgbOffset+pixel*3:rgbOffset+pixel*3+3], rgb[:])
			if identityMask.RGBAAt(x, y).A >= perceptualMaskAlpha {
				fingerprint[maskOffset+pixel/8] |= 1 << (7 - uint(pixel%8))
			}
			fingerprint[influenceOffset+pixel] = perceptualVisibleUnlockedInfluence(
				parentFull.RGBAAt(x, y), parentLocked.RGBAAt(x, y))
		}
	}
	checksumOffset := PerceptualContinuityFingerprintBytes - perceptualFingerprintChecksumBytes
	checksum := sha256.Sum256(fingerprint[:checksumOffset])
	copy(fingerprint[checksumOffset:], checksum[:])
	return fingerprint
}

func decodePerceptualContinuityFingerprint(fingerprint []byte) (decodedPerceptualContinuityFingerprint, error) {
	if len(fingerprint) != PerceptualContinuityFingerprintBytes {
		return decodedPerceptualContinuityFingerprint{}, fmt.Errorf("%w: length is %d, want %d",
			ErrInvalidPerceptualFingerprint, len(fingerprint), PerceptualContinuityFingerprintBytes)
	}
	// Clone once so every checksum and projection slice is derived from one
	// stable snapshot even if a caller reuses its input buffer concurrently.
	stable := bytes.Clone(fingerprint)
	if !bytes.Equal(stable[:4], []byte(perceptualFingerprintMagic)) {
		return decodedPerceptualContinuityFingerprint{}, fmt.Errorf("%w: magic does not match", ErrInvalidPerceptualFingerprint)
	}
	if stable[4] != PerceptualContinuityFingerprintVersion {
		return decodedPerceptualContinuityFingerprint{}, fmt.Errorf("%w: unsupported version %d",
			ErrInvalidPerceptualFingerprint, stable[4])
	}
	if stable[5] != PerceptualRenderSize {
		return decodedPerceptualContinuityFingerprint{}, fmt.Errorf("%w: render size is %d, want %d",
			ErrInvalidPerceptualFingerprint, stable[5], PerceptualRenderSize)
	}
	if stable[6] != 0 || stable[7] != 0 {
		return decodedPerceptualContinuityFingerprint{}, fmt.Errorf("%w: reserved flags are nonzero", ErrInvalidPerceptualFingerprint)
	}
	if payloadBytes := binary.BigEndian.Uint32(stable[8:12]); payloadBytes != perceptualFingerprintPayloadBytes {
		return decodedPerceptualContinuityFingerprint{}, fmt.Errorf("%w: payload length is %d, want %d",
			ErrInvalidPerceptualFingerprint, payloadBytes, perceptualFingerprintPayloadBytes)
	}
	checksumOffset := len(stable) - perceptualFingerprintChecksumBytes
	wantChecksum := sha256.Sum256(stable[:checksumOffset])
	if subtle.ConstantTimeCompare(stable[checksumOffset:], wantChecksum[:]) != 1 {
		return decodedPerceptualContinuityFingerprint{}, fmt.Errorf("%w: checksum does not match", ErrInvalidPerceptualFingerprint)
	}
	offset := perceptualFingerprintHeaderBytes
	styleDigest := stable[offset : offset+perceptualFingerprintStyleDigestBytes]
	offset += perceptualFingerprintStyleDigestBytes
	wholeRGB := stable[offset : offset+perceptualFingerprintRGBBytes]
	offset += perceptualFingerprintRGBBytes
	identityMask := stable[offset : offset+perceptualFingerprintIdentityMaskBytes]
	offset += perceptualFingerprintIdentityMaskBytes
	maskPixels := 0
	for _, value := range identityMask {
		maskPixels += bits.OnesCount8(value)
	}
	if maskPixels < perceptualMinimumMaskPixels {
		return decodedPerceptualContinuityFingerprint{}, fmt.Errorf("%w: identity mask covers %d pixels, want at least %d",
			ErrInvalidPerceptualFingerprint, maskPixels, perceptualMinimumMaskPixels)
	}
	unlockedInfluence := stable[offset : offset+perceptualFingerprintInfluenceBytes]
	return decodedPerceptualContinuityFingerprint{
		styleDigest: styleDigest, wholeRGB: wholeRGB,
		identityMask: identityMask, unlockedInfluence: unlockedInfluence,
	}, nil
}

func perceptualStyleDigest(pack StylePack) ([sha256.Size]byte, error) {
	encoded, err := json.Marshal(pack)
	if err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("%w: encode style pack: %v", ErrInvalidPerceptualFingerprint, err)
	}
	return sha256.Sum256(encoded), nil
}

func perceptualDigestEqual(left, right []byte) bool {
	return len(left) == sha256.Size && len(right) == sha256.Size && subtle.ConstantTimeCompare(left, right) == 1
}

func validatePerceptualFingerprintStyle(fingerprint decodedPerceptualContinuityFingerprint, pack StylePack) error {
	styleDigest, err := perceptualStyleDigest(pack)
	if err != nil {
		return err
	}
	if !perceptualDigestEqual(fingerprint.styleDigest, styleDigest[:]) {
		return fmt.Errorf("%w: style pack digest does not match", ErrInvalidPerceptualFingerprint)
	}
	return nil
}

func perceptualMetricsFromFingerprint(parent decodedPerceptualContinuityFingerprint, childFull, childLocked *image.RGBA) PerceptualContinuityMetrics {
	metrics := PerceptualContinuityMetrics{}
	wholeChanged := 0
	identityChanged := 0
	addedOcclusion := 0
	for y := 0; y < PerceptualRenderSize; y++ {
		for x := 0; x < PerceptualRenderSize; x++ {
			pixel := y*PerceptualRenderSize + x
			parentOffset := pixel * 3
			childRGB := perceptualCompositeRGB(childFull.RGBAAt(x, y))
			delta := perceptualRGBDelta(parent.wholeRGB[parentOffset:parentOffset+3], childRGB)
			metrics.WholeMeanDelta += delta
			if delta > PerceptualPixelDeltaLimit {
				wholeChanged++
			}
			if parent.identityMask[pixel/8]&(1<<(7-uint(pixel%8))) == 0 {
				continue
			}
			metrics.IdentityMaskPixels++
			metrics.IdentityMeanDelta += delta
			if delta > PerceptualPixelDeltaLimit {
				identityChanged++
			}
			parentInfluence := parent.unlockedInfluence[pixel]
			childInfluence := perceptualVisibleUnlockedInfluence(
				childFull.RGBAAt(x, y), childLocked.RGBAAt(x, y))
			if childInfluence >= perceptualMinimumInfluence &&
				int(childInfluence)-int(parentInfluence) >= int(perceptualAddedInfluenceStep) {
				addedOcclusion++
			}
		}
	}
	metrics.WholeChangedRatio = float64(wholeChanged) / perceptualFingerprintPixelCount
	metrics.WholeMeanDelta /= perceptualFingerprintPixelCount
	if metrics.IdentityMaskPixels > 0 {
		metrics.IdentityChangedRatio = float64(identityChanged) / float64(metrics.IdentityMaskPixels)
		metrics.IdentityMeanDelta /= float64(metrics.IdentityMaskPixels)
		metrics.AddedIdentityOcclusion = float64(addedOcclusion) / float64(metrics.IdentityMaskPixels)
	}
	return metrics
}

// perceptualVisibleUnlockedInfluence measures how much the final composited
// pixel differs from the same document rendered with only locked layers. This
// makes the stored projection sensitive to visible paint order: unlocked art
// hidden behind an opaque locked identity contributes zero influence instead
// of looking like a false occlusion merely because it has alpha.
func perceptualVisibleUnlockedInfluence(full, locked color.RGBA) uint8 {
	fullRGB := perceptualCompositeRGB(full)
	lockedRGB := perceptualCompositeRGB(locked)
	delta := perceptualRGBDelta(lockedRGB[:], fullRGB)
	value := int(math.Floor(delta*255 + 0.5))
	if value > 255 {
		return 255
	}
	return uint8(value)
}

func perceptualCompositeRGB(value color.RGBA) [3]byte {
	// image.RGBA stores premultiplied color channels. Add only the uncovered
	// background contribution; multiplying the channels by alpha again would
	// underweight translucent artwork. Integer rounding makes fingerprint bytes
	// stable across architectures.
	return [3]byte{
		perceptualCompositeChannel(value.R, value.A, 247),
		perceptualCompositeChannel(value.G, value.A, 250),
		perceptualCompositeChannel(value.B, value.A, 252),
	}
}

func perceptualCompositeChannel(component, alpha, background uint8) byte {
	composited := int(component) + (int(background)*(255-int(alpha))+127)/255
	if composited > 255 {
		return 255
	}
	return byte(composited)
}

func perceptualRGBDelta(parent []byte, child [3]byte) float64 {
	dr := float64(int(parent[0])-int(child[0])) / 255
	dg := float64(int(parent[1])-int(child[1])) / 255
	db := float64(int(parent[2])-int(child[2])) / 255
	return math.Sqrt(0.2126*dr*dr + 0.7152*dg*dg + 0.0722*db*db)
}
