package avatar

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
)

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
