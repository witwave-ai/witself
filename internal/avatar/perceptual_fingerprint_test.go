package avatar

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestPerceptualContinuityFingerprintV1GoldenDurability(t *testing.T) {
	encoded, err := os.ReadFile("testdata/wapf_v1_human_reference.gz.base64")
	if err != nil {
		t.Fatal(err)
	}
	compressed, err := base64.StdEncoding.DecodeString(string(encoded))
	if err != nil {
		t.Fatal(err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	historical, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if len(historical) != PerceptualContinuityFingerprintBytes {
		t.Fatalf("historical WAPF v1 length = %d, want %d",
			len(historical), PerceptualContinuityFingerprintBytes)
	}
	digest := sha256.Sum256(historical)
	if got, want := hex.EncodeToString(digest[:]), "e9e92503ae73b318a2074712b1ed05066c3dc3caa7feb9f99edf27b2d4df96e3"; got != want {
		t.Fatalf("historical WAPF v1 digest = %s, want %s", got, want)
	}
	pack := BuiltInFlatVectorStylePack()
	if err := ValidatePerceptualContinuityFingerprintForStyle(historical, pack); err != nil {
		t.Fatalf("decode historical WAPF v1: %v", err)
	}
	metrics, err := ComparePerceptualContinuityFromFingerprint(
		historical, []byte(humanReferenceSVG), pack)
	if err != nil {
		t.Fatalf("historical WAPF v1 comparator rejected its unchanged child: %v", err)
	}
	if metrics.WholeChangedRatio != 0 || metrics.IdentityChangedRatio != 0 ||
		metrics.FocusChangedRatio != 0 || metrics.AddedIdentityOcclusion != 0 ||
		metrics.FocusAddedOcclusion != 0 {
		t.Fatalf("historical WAPF v1 unchanged ratios crossed the portability envelope: %+v", metrics)
	}
	const portabilityMeanDeltaLimit = 0.0001
	if metrics.WholeMeanDelta > portabilityMeanDeltaLimit ||
		metrics.IdentityMeanDelta > portabilityMeanDeltaLimit ||
		metrics.FocusMeanDelta > portabilityMeanDeltaLimit {
		t.Fatalf("historical WAPF v1 unchanged mean delta crossed %g: %+v",
			portabilityMeanDeltaLimit, metrics)
	}
	if metrics.IdentityMaskPixels != 3738 || metrics.FocusPixels != 1968 {
		t.Fatalf("historical WAPF v1 mask/focus pixels = %d/%d, want 3738/1968",
			metrics.IdentityMaskPixels, metrics.FocusPixels)
	}
	occluded := replacePerceptualTestLayer(t, humanReferenceSVG, "experience",
		`<circle cx="256" cy="230" r="136" fill="#F7FAFC"></circle>`)
	if err := ValidatePerceptualContinuityFromFingerprint(
		historical, []byte(occluded), pack); !errors.Is(err, ErrPerceptualContinuity) {
		t.Fatalf("historical WAPF v1 comparator accepted an occluding child: %v", err)
	}
}

func TestPerceptualV1PolicyGolden(t *testing.T) {
	exactFloat := func(value float64) string {
		return strconv.FormatFloat(value, 'g', -1, 64)
	}
	gotPolicy := fmt.Sprintf(
		"wapf=%d;render=%d;pixel=%s;whole=%s/%s;identity=%s/%s;occlusion=%s;mask=%d-%d;focus=%d,%d/%d,%d;focus-mask=%d;focus-limits=%s/%s/%s",
		PerceptualContinuityFingerprintVersion, PerceptualRenderSize,
		exactFloat(PerceptualPixelDeltaLimit), exactFloat(PerceptualWholeChangedRatioLimit),
		exactFloat(PerceptualWholeMeanDeltaLimit), exactFloat(PerceptualIdentityChangedRatioLimit),
		exactFloat(PerceptualIdentityMeanDeltaLimit), exactFloat(PerceptualAddedOcclusionRatioLimit),
		perceptualMinimumMaskPixels, perceptualMaximumMaskPixels,
		perceptualFocusCenterX2, perceptualFocusCenterY2,
		perceptualFocusHorizontalRadius2, perceptualFocusVerticalRadius2,
		perceptualMinimumFocusMaskPixels, exactFloat(PerceptualFocusChangedRatioLimit),
		exactFloat(PerceptualFocusMeanDeltaLimit), exactFloat(PerceptualFocusAddedOcclusionRatioLimit))
	const wantPolicy = "wapf=1;render=96;pixel=0.12;whole=0.42/0.2;identity=0.46/0.24;occlusion=0.3;mask=48-6144;focus=96,86/48,52;focus-mask=1152;focus-limits=0.26/0.13/0.3"
	if gotPolicy != wantPolicy {
		t.Fatalf("perceptual-v1 policy changed without a new profile/fingerprint version:\n got %s\nwant %s", gotPolicy, wantPolicy)
	}

	bitmap := make([]byte, perceptualFingerprintIdentityMaskBytes)
	focusPixels := 0
	for pixel := 0; pixel < perceptualFingerprintPixelCount; pixel++ {
		if !perceptualFocusPixel(pixel%PerceptualRenderSize, pixel/PerceptualRenderSize) {
			continue
		}
		bitmap[pixel/8] |= 1 << (7 - uint(pixel%8))
		focusPixels++
	}
	if focusPixels != 1968 {
		t.Fatalf("perceptual-v1 focus pixels = %d, want 1968", focusPixels)
	}
	digest := sha256.Sum256(bitmap)
	if got, want := hex.EncodeToString(digest[:]), "dff28b03717654128c1f0bb49a09fdc2b6e7a5b2caae1b537117575952e918e6"; got != want {
		t.Fatalf("perceptual-v1 focus bitmap digest = %s, want %s", got, want)
	}
}

func TestPerceptualContinuityFingerprintIdentityMaskBoundary(t *testing.T) {
	pack := BuiltInFlatVectorStylePack()
	base, err := BuildPerceptualContinuityFingerprint([]byte(humanReferenceSVG), pack)
	if err != nil {
		t.Fatal(err)
	}
	withMaskPixels := func(focusCount, totalCount int) []byte {
		fingerprint := bytes.Clone(base)
		maskOffset := perceptualFingerprintHeaderBytes +
			perceptualFingerprintStyleDigestBytes + perceptualFingerprintRGBBytes
		mask := fingerprint[maskOffset : maskOffset+perceptualFingerprintIdentityMaskBytes]
		clear(mask)
		set := 0
		for pixel := 0; pixel < perceptualFingerprintPixelCount && set < focusCount; pixel++ {
			if !perceptualFocusPixel(pixel%PerceptualRenderSize, pixel/PerceptualRenderSize) {
				continue
			}
			mask[pixel/8] |= 1 << (7 - uint(pixel%8))
			set++
		}
		for pixel := 0; pixel < perceptualFingerprintPixelCount && set < totalCount; pixel++ {
			if mask[pixel/8]&(1<<(7-uint(pixel%8))) != 0 {
				continue
			}
			mask[pixel/8] |= 1 << (7 - uint(pixel%8))
			set++
		}
		checksumOffset := len(fingerprint) - perceptualFingerprintChecksumBytes
		checksum := sha256.Sum256(fingerprint[:checksumOffset])
		copy(fingerprint[checksumOffset:], checksum[:])
		return fingerprint
	}
	if err := ValidatePerceptualContinuityFingerprint(withMaskPixels(
		perceptualMinimumFocusMaskPixels-1, perceptualMinimumFocusMaskPixels-1)); !errors.Is(err, ErrInvalidPerceptualFingerprint) {
		t.Fatalf("undersized focus mask error = %v", err)
	}
	exact := withMaskPixels(perceptualMinimumFocusMaskPixels, perceptualMinimumFocusMaskPixels)
	if err := ValidatePerceptualContinuityFingerprintForStyle(exact, pack); err != nil {
		t.Fatalf("minimum focus mask: %v", err)
	}
	metrics, err := ComparePerceptualContinuityFromFingerprint(exact, []byte(humanReferenceSVG), pack)
	if err != nil {
		t.Fatalf("minimum focus comparison: %v", err)
	}
	if metrics.IdentityMaskPixels != perceptualMinimumFocusMaskPixels || metrics.FocusPixels != 1968 {
		t.Fatalf("identity/fixed-focus pixels = %d/%d, want %d/1968", metrics.IdentityMaskPixels,
			metrics.FocusPixels, perceptualMinimumFocusMaskPixels)
	}
	if err := ValidatePerceptualContinuityFingerprint(withMaskPixels(
		perceptualMinimumFocusMaskPixels, perceptualMaximumMaskPixels)); err != nil {
		t.Fatalf("maximum mask: %v", err)
	}
	if err := ValidatePerceptualContinuityFingerprint(withMaskPixels(
		perceptualMinimumFocusMaskPixels, perceptualMaximumMaskPixels+1)); !errors.Is(err, ErrInvalidPerceptualFingerprint) {
		t.Fatalf("oversized mask error = %v", err)
	}
}

func TestPerceptualContinuityFingerprintSpeciesAndPlaceholderBehavior(t *testing.T) {
	pack := BuiltInFlatVectorStylePack()
	placeholder, err := GeneratePlaceholderSVGForStylePack("agent_golden", "Juniper", pack)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		svg  []byte
	}{
		{name: "human", svg: []byte(humanReferenceSVG)},
		{name: "animal", svg: []byte(animalReferenceSVG)},
		{name: "insect", svg: []byte(insectReferenceSVG)},
		{name: "placeholder", svg: placeholder},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			first, err := BuildPerceptualContinuityFingerprint(test.svg, pack)
			if err != nil {
				t.Fatalf("first fingerprint: %v", err)
			}
			second, err := BuildPerceptualContinuityFingerprint(test.svg, pack)
			if err != nil {
				t.Fatalf("second fingerprint: %v", err)
			}
			if !bytes.Equal(first, second) {
				t.Fatal("same-process fingerprint construction is not deterministic")
			}
			if err := ValidatePerceptualContinuityFingerprint(first); err != nil {
				t.Fatalf("strict fingerprint validation: %v", err)
			}
			if err := ValidatePerceptualContinuityFingerprintForStyle(first, pack); err != nil {
				t.Fatalf("style-bound fingerprint validation: %v", err)
			}
			metrics, err := ComparePerceptualContinuityFromFingerprint(first, test.svg, pack)
			if err != nil {
				t.Fatalf("unchanged policy outcome: %v", err)
			}
			if metrics.WholeChangedRatio != 0 || metrics.WholeMeanDelta != 0 ||
				metrics.IdentityChangedRatio != 0 || metrics.IdentityMeanDelta != 0 ||
				metrics.AddedIdentityOcclusion != 0 || metrics.FocusChangedRatio != 0 ||
				metrics.FocusMeanDelta != 0 || metrics.FocusAddedOcclusion != 0 {
				t.Fatalf("unchanged same-process metrics are nonzero: %+v", metrics)
			}
			if metrics.IdentityMaskPixels < perceptualMinimumMaskPixels ||
				metrics.IdentityMaskPixels > perceptualMaximumMaskPixels || metrics.FocusPixels != 1968 {
				t.Fatalf("unchanged mask/focus pixels are invalid: %+v", metrics)
			}
			occluded := replacePerceptualTestLayer(t, string(test.svg), "experience",
				`<circle cx="256" cy="230" r="136" fill="#F7FAFC"></circle>`)
			if err := ValidatePerceptualContinuityFromFingerprint(
				first, []byte(occluded), pack); !errors.Is(err, ErrPerceptualContinuity) {
				t.Fatalf("occluding policy outcome = %v, want ErrPerceptualContinuity", err)
			}
		})
	}
}

func TestPerceptualContinuityFingerprintIsDeterministicAndStrict(t *testing.T) {
	pack := BuiltInFlatVectorStylePack()
	first, err := BuildPerceptualContinuityFingerprint([]byte(humanReferenceSVG), pack)
	if err != nil {
		t.Fatal(err)
	}
	second, err := BuildPerceptualContinuityFingerprint([]byte(humanReferenceSVG), pack)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != PerceptualContinuityFingerprintBytes {
		t.Fatalf("fingerprint length = %d, want %d", len(first), PerceptualContinuityFingerprintBytes)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("identical parent and style produced different fingerprints")
	}
	if err := ValidatePerceptualContinuityFingerprint(first); err != nil {
		t.Fatalf("validate fingerprint: %v", err)
	}
	if err := ValidatePerceptualContinuityFingerprintForStyle(first, pack); err != nil {
		t.Fatalf("validate fingerprint style: %v", err)
	}

	mutations := map[string]func([]byte) []byte{
		"empty": func([]byte) []byte { return nil },
		"truncated": func(value []byte) []byte {
			return value[:len(value)-1]
		},
		"extra": func(value []byte) []byte {
			return append(value, 0)
		},
		"magic": func(value []byte) []byte {
			value[0] ^= 0xff
			return value
		},
		"version": func(value []byte) []byte {
			value[4]++
			return value
		},
		"render size": func(value []byte) []byte {
			value[5]--
			return value
		},
		"reserved flags": func(value []byte) []byte {
			value[6] = 1
			return value
		},
		"payload length": func(value []byte) []byte {
			value[11]--
			return value
		},
		"payload checksum": func(value []byte) []byte {
			value[perceptualFingerprintHeaderBytes+perceptualFingerprintStyleDigestBytes] ^= 1
			return value
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			candidate := mutate(bytes.Clone(first))
			if err := ValidatePerceptualContinuityFingerprint(candidate); !errors.Is(err, ErrInvalidPerceptualFingerprint) {
				t.Fatalf("error = %v, want ErrInvalidPerceptualFingerprint", err)
			}
		})
	}
}

func TestPerceptualContinuityFullParentAndFingerprintAreEquivalent(t *testing.T) {
	pack := BuiltInFlatVectorStylePack()
	fingerprint, err := BuildPerceptualContinuityFingerprint([]byte(humanReferenceSVG), pack)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		child string
	}{
		{
			name: "ordinary",
			child: replacePerceptualTestLayer(t, humanReferenceSVG, "expression",
				`<circle cx="212" cy="236" r="12" fill="#203247"></circle>`+
					`<circle cx="300" cy="236" r="12" fill="#203247"></circle>`+
					`<path d="M218 284 C240 306 272 306 294 284" fill="none" stroke="#203247" stroke-width="8" stroke-linecap="round"></path>`),
		},
		{
			name: "rejected occlusion",
			child: replacePerceptualTestLayer(t, humanReferenceSVG, "experience",
				`<circle cx="256" cy="230" r="136" fill="#F7FAFC"></circle>`),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fullMetrics, fullErr := ComparePerceptualContinuity([]byte(humanReferenceSVG), []byte(test.child), pack)
			fingerprintMetrics, fingerprintErr := ComparePerceptualContinuityFromFingerprint(fingerprint, []byte(test.child), pack)
			if !reflect.DeepEqual(fullMetrics, fingerprintMetrics) {
				t.Fatalf("metrics differ: full=%+v fingerprint=%+v", fullMetrics, fingerprintMetrics)
			}
			if (fullErr == nil) != (fingerprintErr == nil) ||
				errors.Is(fullErr, ErrPerceptualContinuity) != errors.Is(fingerprintErr, ErrPerceptualContinuity) {
				t.Fatalf("errors differ: full=%v fingerprint=%v", fullErr, fingerprintErr)
			}
		})
	}
}

func TestPerceptualContinuityFingerprintIsBoundToExactStyle(t *testing.T) {
	pack := BuiltInFlatVectorStylePack()
	fingerprint, err := BuildPerceptualContinuityFingerprint([]byte(humanReferenceSVG), pack)
	if err != nil {
		t.Fatal(err)
	}
	other := BuiltInFlatVectorStylePack()
	other.Version++
	if err := ValidatePerceptualContinuityFingerprintForStyle(fingerprint, other); !errors.Is(err, ErrInvalidPerceptualFingerprint) {
		t.Fatalf("style-only validation error = %v, want ErrInvalidPerceptualFingerprint", err)
	}
	_, err = ComparePerceptualContinuityFromFingerprint(fingerprint, []byte(humanReferenceSVG), other)
	if !errors.Is(err, ErrInvalidPerceptualFingerprint) || !strings.Contains(err.Error(), "style pack digest") {
		t.Fatalf("style mismatch error = %v, want fingerprint style refusal", err)
	}
}
