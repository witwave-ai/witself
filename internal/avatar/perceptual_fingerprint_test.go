package avatar

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"reflect"
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
	current, err := BuildPerceptualContinuityFingerprint([]byte(humanReferenceSVG), pack)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(current, historical) {
		t.Fatal("current WAPF v1 builder differs from the durable historical fixture; introduce a new format version")
	}
	if err := ValidatePerceptualContinuityFromFingerprint(
		historical, []byte(humanReferenceSVG), pack); err != nil {
		t.Fatalf("historical WAPF v1 comparator rejected its unchanged child: %v", err)
	}
	occluded := replacePerceptualTestLayer(t, humanReferenceSVG, "experience",
		`<circle cx="256" cy="230" r="136" fill="#F7FAFC"></circle>`)
	if err := ValidatePerceptualContinuityFromFingerprint(
		historical, []byte(occluded), pack); !errors.Is(err, ErrPerceptualContinuity) {
		t.Fatalf("historical WAPF v1 comparator accepted an occluding child: %v", err)
	}
}

func TestPerceptualContinuityFingerprintIdentityMaskBoundary(t *testing.T) {
	pack := BuiltInFlatVectorStylePack()
	base, err := BuildPerceptualContinuityFingerprint([]byte(humanReferenceSVG), pack)
	if err != nil {
		t.Fatal(err)
	}
	withMaskPixels := func(count int) []byte {
		fingerprint := bytes.Clone(base)
		maskOffset := perceptualFingerprintHeaderBytes +
			perceptualFingerprintStyleDigestBytes + perceptualFingerprintRGBBytes
		mask := fingerprint[maskOffset : maskOffset+perceptualFingerprintIdentityMaskBytes]
		clear(mask)
		for pixel := 0; pixel < count; pixel++ {
			mask[pixel/8] |= 1 << (7 - uint(pixel%8))
		}
		checksumOffset := len(fingerprint) - perceptualFingerprintChecksumBytes
		checksum := sha256.Sum256(fingerprint[:checksumOffset])
		copy(fingerprint[checksumOffset:], checksum[:])
		return fingerprint
	}
	if err := ValidatePerceptualContinuityFingerprint(withMaskPixels(perceptualMinimumMaskPixels - 1)); !errors.Is(err, ErrInvalidPerceptualFingerprint) {
		t.Fatalf("47-pixel mask error = %v", err)
	}
	exact := withMaskPixels(perceptualMinimumMaskPixels)
	if err := ValidatePerceptualContinuityFingerprintForStyle(exact, pack); err != nil {
		t.Fatalf("48-pixel mask: %v", err)
	}
	metrics, err := ComparePerceptualContinuityFromFingerprint(exact, []byte(humanReferenceSVG), pack)
	if err != nil {
		t.Fatalf("48-pixel comparison: %v", err)
	}
	if metrics.IdentityMaskPixels != perceptualMinimumMaskPixels {
		t.Fatalf("identity pixels = %d, want %d", metrics.IdentityMaskPixels, perceptualMinimumMaskPixels)
	}
}

func TestPerceptualContinuityFingerprintSpeciesAndPlaceholderGoldens(t *testing.T) {
	pack := BuiltInFlatVectorStylePack()
	placeholder, err := GeneratePlaceholderSVGForStylePack("agent_golden", "Juniper", pack)
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]struct {
		svg    []byte
		sha256 string
	}{
		"human": {svg: []byte(humanReferenceSVG), sha256: "e9e92503ae73b318a2074712b1ed05066c3dc3caa7feb9f99edf27b2d4df96e3"},
		"animal": {svg: []byte(animalReferenceSVG), sha256: "4555628152f89d026a2594acfbb47ffab2b85eab60cae12851242b9133758880"},
		"insect": {svg: []byte(insectReferenceSVG), sha256: "083ef47412a4697dcfd95b73694c5cbf89e65bf484318335c4c6df0e96e681ae"},
		"placeholder": {svg: placeholder, sha256: "68813f9e8ccce13a6d4adaf8fe7e922b2ebe20314b2e8326cc2b3610d483e5a2"},
	}
	for name, test := range tests {
		fingerprint, err := BuildPerceptualContinuityFingerprint(test.svg, pack)
		if err != nil {
			t.Fatalf("%s fingerprint: %v", name, err)
		}
		digest := sha256.Sum256(fingerprint)
		if got := hex.EncodeToString(digest[:]); got != test.sha256 {
			t.Fatalf("%s fingerprint digest = %s, want %s", name, got, test.sha256)
		}
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
