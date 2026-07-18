package avatar

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"math"
	"testing"
)

func FuzzSanitizeSVGNeverPanics(f *testing.F) {
	for _, seed := range avatarSVGFuzzSeeds() {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) == 0 || len(raw) > MaxSVGBytes {
			return
		}
		canonical, err := SanitizeSVG(raw)
		if err != nil {
			return
		}
		if len(canonical) == 0 || len(canonical) > MaxSVGBytes {
			t.Fatalf("successful sanitizer returned %d bytes", len(canonical))
		}
		again, err := SanitizeSVG(canonical)
		if err != nil {
			t.Fatalf("canonical SVG was not accepted again: %v", err)
		}
		if !bytes.Equal(canonical, again) {
			t.Fatal("SVG canonicalization is not idempotent")
		}
	})
}

func FuzzSanitizeSVGForStylePackNeverPanics(f *testing.F) {
	pack := BuiltInFlatVectorStylePack()
	for _, seed := range avatarSVGFuzzSeeds() {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) == 0 || len(raw) > MaxSVGBytes {
			return
		}
		canonical, err := SanitizeSVGForStylePack(raw, pack)
		if err != nil {
			return
		}
		if len(canonical) == 0 || len(canonical) > MaxSVGBytes {
			t.Fatalf("successful style sanitizer returned %d bytes", len(canonical))
		}
		again, err := SanitizeSVGForStylePack(canonical, pack)
		if err != nil {
			t.Fatalf("canonical style SVG was not accepted again: %v", err)
		}
		if !bytes.Equal(canonical, again) {
			t.Fatal("style SVG canonicalization is not idempotent")
		}
	})
}

func FuzzSanitizeSVGForPerceptualV1NeverPanics(f *testing.F) {
	for _, seed := range avatarSVGFuzzSeeds() {
		f.Add(seed)
	}
	placeholder, err := GeneratePlaceholderSVG("agent_fuzz", "Juniper")
	if err != nil {
		f.Fatalf("build placeholder seed: %v", err)
	}
	f.Add(placeholder)
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) == 0 || len(raw) > MaxSVGBytes {
			return
		}
		canonical, err := SanitizeSVGForPerceptualV1(raw)
		if err != nil {
			return
		}
		again, err := SanitizeSVGForPerceptualV1(canonical)
		if err != nil {
			t.Fatalf("accepted perceptual-v1 SVG was not accepted again: %v", err)
		}
		if !bytes.Equal(canonical, again) {
			t.Fatal("perceptual-v1 canonicalization is not idempotent")
		}
		if _, err := renderPerceptualAvatar(canonical); err != nil {
			t.Fatalf("profile-accepted SVG did not render: %v", err)
		}
	})
}

func FuzzPerceptualContinuityNeverPanics(f *testing.F) {
	pack := BuiltInFlatVectorStylePack()
	f.Add([]byte(humanReferenceSVG))
	f.Add([]byte(animalReferenceSVG))
	f.Add([]byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`))
	f.Fuzz(func(t *testing.T, child []byte) {
		if len(child) == 0 || len(child) > MaxSVGBytes {
			return
		}
		if _, err := SanitizeSVGForPerceptualV1StylePack(child, pack); err != nil {
			return
		}
		metrics, err := ComparePerceptualContinuity([]byte(humanReferenceSVG), child, pack)
		if err != nil && !errors.Is(err, ErrPerceptualContinuity) {
			t.Fatalf("profile-accepted child failed outside continuity policy: %v", err)
		}
		for name, value := range map[string]float64{
			"whole changed ratio":      metrics.WholeChangedRatio,
			"whole mean delta":         metrics.WholeMeanDelta,
			"identity changed ratio":   metrics.IdentityChangedRatio,
			"identity mean delta":      metrics.IdentityMeanDelta,
			"added identity occlusion": metrics.AddedIdentityOcclusion,
			"focus changed ratio":      metrics.FocusChangedRatio,
			"focus mean delta":         metrics.FocusMeanDelta,
			"focus added occlusion":    metrics.FocusAddedOcclusion,
		} {
			if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1 {
				t.Fatalf("%s is outside [0,1]: %v", name, value)
			}
		}
		if metrics.IdentityMaskPixels < 0 ||
			metrics.IdentityMaskPixels > PerceptualRenderSize*PerceptualRenderSize {
			t.Fatalf("identity mask pixel count is invalid: %d", metrics.IdentityMaskPixels)
		}
		if metrics.FocusPixels != 1968 {
			t.Fatalf("fixed focus pixel count is %d, want 1968", metrics.FocusPixels)
		}
	})
}

func FuzzPerceptualContinuityFingerprintNeverPanics(f *testing.F) {
	valid, err := BuildPerceptualContinuityFingerprint([]byte(humanReferenceSVG), BuiltInFlatVectorStylePack())
	if err != nil {
		f.Fatalf("build valid seed: %v", err)
	}
	f.Add(valid)
	f.Add([]byte(perceptualFingerprintMagic))
	f.Add(make([]byte, PerceptualContinuityFingerprintBytes))
	f.Fuzz(func(t *testing.T, fingerprint []byte) {
		if len(fingerprint) > PerceptualContinuityFingerprintBytes+1 {
			return
		}
		err := ValidatePerceptualContinuityFingerprint(fingerprint)
		if err == nil && len(fingerprint) != PerceptualContinuityFingerprintBytes {
			t.Fatalf("accepted fingerprint has %d bytes", len(fingerprint))
		}
	})
}

func FuzzPerceptualContinuityStructuredFingerprintNeverPanics(f *testing.F) {
	pack := BuiltInFlatVectorStylePack()
	valid, err := BuildPerceptualContinuityFingerprint([]byte(humanReferenceSVG), pack)
	if err != nil {
		f.Fatalf("build valid seed: %v", err)
	}
	f.Add(uint16(0), byte(0))
	f.Add(uint16(perceptualFingerprintIdentityMaskBytes), byte(0xff))
	f.Add(uint16(65535), byte(0x80))
	f.Fuzz(func(t *testing.T, offset uint16, value byte) {
		mutated := bytes.Clone(valid)
		checksumOffset := len(mutated) - perceptualFingerprintChecksumBytes
		payloadOffset := perceptualFingerprintHeaderBytes + perceptualFingerprintStyleDigestBytes
		mutableBytes := checksumOffset - payloadOffset
		index := payloadOffset + int(offset)%mutableBytes
		mutated[index] = value
		checksum := sha256.Sum256(mutated[:checksumOffset])
		copy(mutated[checksumOffset:], checksum[:])
		if err := ValidatePerceptualContinuityFingerprintForStyle(mutated, pack); err != nil {
			return
		}
		metrics, err := ComparePerceptualContinuityFromFingerprint(mutated, []byte(humanReferenceSVG), pack)
		if err != nil && !errors.Is(err, ErrPerceptualContinuity) {
			t.Fatalf("validated structured fingerprint failed comparison contract: %v", err)
		}
		if metrics.IdentityMaskPixels < perceptualMinimumMaskPixels ||
			metrics.IdentityMaskPixels > perceptualMaximumMaskPixels {
			t.Fatalf("validated fingerprint has invalid identity mask size %d", metrics.IdentityMaskPixels)
		}
		if metrics.FocusPixels != 1968 {
			t.Fatalf("validated fingerprint has fixed focus size %d, want 1968", metrics.FocusPixels)
		}
		decoded, err := decodePerceptualContinuityFingerprint(mutated)
		if err != nil {
			t.Fatalf("validated fingerprint did not decode again: %v", err)
		}
		if focusPixels := perceptualFingerprintFocusMaskPixels(decoded.identityMask); focusPixels < perceptualMinimumFocusMaskPixels {
			t.Fatalf("validated fingerprint has only %d identity pixels in fixed focus", focusPixels)
		}
	})
}

func FuzzPerceptualContinuityFingerprintMaskRunsNeverPanics(f *testing.F) {
	pack := BuiltInFlatVectorStylePack()
	valid, err := BuildPerceptualContinuityFingerprint([]byte(humanReferenceSVG), pack)
	if err != nil {
		f.Fatalf("build valid seed: %v", err)
	}
	f.Add(uint16(0), uint16(0), false)
	f.Add(uint16(0), uint16(perceptualMinimumFocusMaskPixels-1), true)
	f.Add(uint16(perceptualFingerprintPixelCount-1), uint16(perceptualFingerprintPixelCount), false)
	f.Add(uint16(65535), uint16(65535), true)
	f.Fuzz(func(t *testing.T, startRaw, runRaw uint16, set bool) {
		mutated := bytes.Clone(valid)
		maskOffset := perceptualFingerprintHeaderBytes +
			perceptualFingerprintStyleDigestBytes + perceptualFingerprintRGBBytes
		mask := mutated[maskOffset : maskOffset+perceptualFingerprintIdentityMaskBytes]
		start := int(startRaw) % perceptualFingerprintPixelCount
		run := int(runRaw) % (perceptualFingerprintPixelCount + 1)
		for step := 0; step < run; step++ {
			pixel := (start + step) % perceptualFingerprintPixelCount
			bit := byte(1 << (7 - uint(pixel%8)))
			if set {
				mask[pixel/8] |= bit
			} else {
				mask[pixel/8] &^= bit
			}
		}
		checksumOffset := len(mutated) - perceptualFingerprintChecksumBytes
		checksum := sha256.Sum256(mutated[:checksumOffset])
		copy(mutated[checksumOffset:], checksum[:])
		if err := ValidatePerceptualContinuityFingerprintForStyle(mutated, pack); err != nil {
			return
		}
		decoded, err := decodePerceptualContinuityFingerprint(mutated)
		if err != nil {
			t.Fatalf("validated run-mutated fingerprint did not decode: %v", err)
		}
		focusPixels := perceptualFingerprintFocusMaskPixels(decoded.identityMask)
		if focusPixels < perceptualMinimumFocusMaskPixels {
			t.Fatalf("validated run-mutated fingerprint has %d focus mask pixels", focusPixels)
		}
		metrics, err := ComparePerceptualContinuityFromFingerprint(mutated, []byte(humanReferenceSVG), pack)
		if err != nil && !errors.Is(err, ErrPerceptualContinuity) {
			t.Fatalf("validated run-mutated fingerprint failed comparison contract: %v", err)
		}
		if metrics.FocusPixels != 1968 {
			t.Fatalf("run-mutated fingerprint has fixed focus size %d, want 1968", metrics.FocusPixels)
		}
	})
}

func perceptualFingerprintFocusMaskPixels(mask []byte) int {
	count := 0
	for pixel := 0; pixel < perceptualFingerprintPixelCount; pixel++ {
		if mask[pixel/8]&(1<<(7-uint(pixel%8))) != 0 &&
			perceptualFocusPixel(pixel%PerceptualRenderSize, pixel/PerceptualRenderSize) {
			count++
		}
	}
	return count
}

func avatarSVGFuzzSeeds() [][]byte {
	return [][]byte{
		[]byte(humanReferenceSVG),
		[]byte(animalReferenceSVG),
		[]byte(insectReferenceSVG),
		[]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32" width="32" height="32"><circle cx="16" cy="16" r="12" fill="#203247"></circle></svg>`),
		[]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32"><path d="M0 0 C1e999 -1e999 32 32 8 8" fill="#203247" transform="scale(1e999)"></path></svg>`),
		[]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1 1" width="32" height="32"><path d="M0 0 C8192 8192 -8192 -8192 1 1" fill="#203247"></path><path d="M0 0 C8192 8192 -8192 -8192 1 1" fill="#203247"></path></svg>`),
		[]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32" width="32" height="32"><path d="M1 1 A0 4 0 0 1 20 20" fill="none" stroke="#203247"></path></svg>`),
		[]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32" width="32" height="32"><circle cx="50%" cy="16" r="12" fill="#20324780"></circle></svg>`),
		[]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32" width="32" height="32"><defs><clipPath id="clip"><circle cx="16" cy="16" r="8"></circle></clipPath></defs><circle cx="16" cy="16" r="12" fill="#203247" clip-path="url(#clip)"></circle></svg>`),
		[]byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`),
		[]byte(`<svg xmlns="http://www.w3.org/2000/svg"><foreignObject><div>no</div></foreignObject></svg>`),
		[]byte(`<svg xmlns="http://www.w3.org/2000/svg"><image href="https://example.invalid/avatar.png"></image></svg>`),
		[]byte(`<!DOCTYPE svg [<!ENTITY x "expanded">]><svg xmlns="http://www.w3.org/2000/svg"><title>&x;</title></svg>`),
		[]byte(`<svg xmlns="http://www.w3.org/2000/svg"><g><g><g><path d="M0,0 L1,1 Z"></path></g></g>`),
		{0xff, 0xfe, 0xfd, '<', 's', 'v', 'g', '>'},
	}
}
