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
	"strings"
	"testing"
)

const perceptualCrossArchitectureMeanDeltaEnvelope = 0.0001

// TestPerceptualContinuityCrossArchitectureGoldens proves that a durable
// perceptual-v1 fingerprint produced on either supported architecture remains
// usable by the other architecture. The canonical rasterizers can differ by a
// few antialiased channel values, but those differences must remain far below
// the material-pixel threshold while a real centered occlusion is still
// rejected.
func TestPerceptualContinuityCrossArchitectureGoldens(t *testing.T) {
	pack := BuiltInFlatVectorStylePack()
	human := perceptualCrossArchitectureHumanReference(t, pack)
	occluded := perceptualCrossArchitectureOccludedHuman(t, human)

	fixtures := []struct {
		name        string
		path        string
		sha256      string
		fingerprint []byte
	}{
		{
			name:   "arm64",
			path:   "testdata/wapf_v1_human_reference.gz.base64",
			sha256: "e9e92503ae73b318a2074712b1ed05066c3dc3caa7feb9f99edf27b2d4df96e3",
		},
		{
			name:   "amd64",
			path:   "testdata/wapf_v1_human_reference_amd64.gz.base64",
			sha256: "b3e2c73f038f2563eb77d6d07ef471c152248ab61bee1103eccb2e2461f43176",
		},
	}

	for index := range fixtures {
		fixtures[index].fingerprint = readPerceptualCrossArchitectureFixture(t, fixtures[index].path)
	}
	assertPerceptualCrossArchitectureFixtureDrift(
		t, fixtures[0].fingerprint, fixtures[1].fingerprint)

	for _, fixture := range fixtures {
		fixture := fixture
		t.Run(fixture.name, func(t *testing.T) {
			fingerprint := fixture.fingerprint
			if len(fingerprint) != PerceptualContinuityFingerprintBytes {
				t.Fatalf("fingerprint length = %d, want %d",
					len(fingerprint), PerceptualContinuityFingerprintBytes)
			}
			digest := sha256.Sum256(fingerprint)
			if got := hex.EncodeToString(digest[:]); got != fixture.sha256 {
				t.Fatalf("fingerprint digest = %s, want %s", got, fixture.sha256)
			}
			if err := ValidatePerceptualContinuityFingerprintForStyle(fingerprint, pack); err != nil {
				t.Fatalf("validate fixture: %v", err)
			}

			metrics, err := ComparePerceptualContinuityFromFingerprint(fingerprint, human, pack)
			if err != nil {
				t.Fatalf("unchanged cross-architecture reference: %v", err)
			}
			assertPerceptualCrossArchitectureUnchanged(t, metrics)

			occludedMetrics, err := ComparePerceptualContinuityFromFingerprint(fingerprint, occluded, pack)
			if !errors.Is(err, ErrPerceptualContinuity) {
				t.Fatalf("occluding child error = %v, want ErrPerceptualContinuity", err)
			}
			if occludedMetrics.FocusChangedRatio <= PerceptualFocusChangedRatioLimit ||
				occludedMetrics.FocusMeanDelta <= PerceptualFocusMeanDeltaLimit ||
				occludedMetrics.FocusAddedOcclusion <= PerceptualFocusAddedOcclusionRatioLimit {
				t.Fatalf("occluding child did not exceed every centered limit: %+v", occludedMetrics)
			}
		})
	}
}

func assertPerceptualCrossArchitectureFixtureDrift(t *testing.T, arm64, amd64 []byte) {
	t.Helper()
	armDecoded, err := decodePerceptualContinuityFingerprint(arm64)
	if err != nil {
		t.Fatalf("decode arm64 fixture: %v", err)
	}
	amdDecoded, err := decodePerceptualContinuityFingerprint(amd64)
	if err != nil {
		t.Fatalf("decode amd64 fixture: %v", err)
	}
	if !bytes.Equal(armDecoded.identityMask, amdDecoded.identityMask) {
		t.Fatal("arm64 and amd64 fixtures have different decoded identity masks")
	}

	maxRGBDelta := maxPerceptualCrossArchitectureByteDelta(
		t, "whole RGB", armDecoded.wholeRGB, amdDecoded.wholeRGB)
	if maxRGBDelta > 4 {
		t.Fatalf("arm64/amd64 whole RGB max channel delta = %d, want <= 4", maxRGBDelta)
	}
	maxInfluenceDelta := maxPerceptualCrossArchitectureByteDelta(
		t, "unlocked influence", armDecoded.unlockedInfluence, amdDecoded.unlockedInfluence)
	if maxInfluenceDelta > 1 {
		t.Fatalf("arm64/amd64 unlocked influence max delta = %d, want <= 1", maxInfluenceDelta)
	}
}

func maxPerceptualCrossArchitectureByteDelta(t *testing.T, name string, left, right []byte) int {
	t.Helper()
	if len(left) != len(right) {
		t.Fatalf("%s lengths = %d/%d", name, len(left), len(right))
	}
	maximum := 0
	for index := range left {
		delta := int(left[index]) - int(right[index])
		if delta < 0 {
			delta = -delta
		}
		if delta > maximum {
			maximum = delta
		}
	}
	return maximum
}

func assertPerceptualCrossArchitectureUnchanged(t *testing.T, metrics PerceptualContinuityMetrics) {
	t.Helper()
	if metrics.WholeChangedRatio != 0 ||
		metrics.IdentityChangedRatio != 0 ||
		metrics.FocusChangedRatio != 0 {
		t.Fatalf("unchanged reference has materially changed pixels: %+v", metrics)
	}
	if metrics.AddedIdentityOcclusion != 0 || metrics.FocusAddedOcclusion != 0 {
		t.Fatalf("unchanged reference has added occlusion: %+v", metrics)
	}
	for name, delta := range map[string]float64{
		"whole":    metrics.WholeMeanDelta,
		"identity": metrics.IdentityMeanDelta,
		"focus":    metrics.FocusMeanDelta,
	} {
		if delta > perceptualCrossArchitectureMeanDeltaEnvelope {
			t.Fatalf("%s mean delta = %g, want <= %g",
				name, delta, perceptualCrossArchitectureMeanDeltaEnvelope)
		}
	}
	if metrics.IdentityMaskPixels != 3738 || metrics.FocusPixels != 1968 {
		t.Fatalf("identity/focus pixels = %d/%d, want 3738/1968",
			metrics.IdentityMaskPixels, metrics.FocusPixels)
	}
}

func perceptualCrossArchitectureHumanReference(t *testing.T, pack StylePack) []byte {
	t.Helper()
	for _, reference := range pack.References {
		if reference.SubjectForm == SubjectHuman {
			return []byte(reference.SVG)
		}
	}
	t.Fatal("built-in style pack has no human reference")
	return nil
}

func perceptualCrossArchitectureOccludedHuman(t *testing.T, human []byte) []byte {
	t.Helper()
	const empty = `<g id="experience" data-layer="experience"></g>`
	const occluded = `<g id="experience" data-layer="experience"><circle cx="256" cy="230" r="136" fill="#F7FAFC"></circle></g>`
	if !bytes.Contains(human, []byte(empty)) {
		t.Fatal("human reference has no empty experience layer")
	}
	return []byte(strings.Replace(string(human), empty, occluded, 1))
}

func readPerceptualCrossArchitectureFixture(t *testing.T, path string) []byte {
	t.Helper()
	encoded, err := os.ReadFile(path)
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
	fingerprint, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	return fingerprint
}
