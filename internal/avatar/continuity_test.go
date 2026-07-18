package avatar

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateLockedLayerContinuityAllowsNormalizedLockedSourceAndUnlockedChanges(t *testing.T) {
	pack := BuiltInFlatVectorStylePack()
	parent := humanReferenceSVG
	attributeReordered := strings.Replace(parent,
		`<rect x="0" y="0" width="512" height="512" fill="#F7FAFC"></rect>`,
		`<rect fill="#F7FAFC" height="512" width="512" y="0" x="0"></rect>`, 1)
	if err := ValidateLockedLayerContinuity([]byte(parent), []byte(attributeReordered), pack); err != nil {
		t.Fatalf("attribute-order-only change was rejected: %v", err)
	}

	expressionChanged := strings.Replace(parent,
		`<circle cx="214" cy="236" r="10" fill="#203247"></circle>`,
		`<circle cx="214" cy="236" r="12" fill="#203247"></circle>`, 1)
	if expressionChanged == parent {
		t.Fatal("expression fixture replacement did not apply")
	}
	if err := ValidateLockedLayerContinuity([]byte(parent), []byte(expressionChanged), pack); err != nil {
		t.Fatalf("unlocked expression change was rejected: %v", err)
	}
}

func TestValidateLockedLayerContinuityRejectsLockedSourceChange(t *testing.T) {
	pack := BuiltInFlatVectorStylePack()
	child := strings.Replace(humanReferenceSVG, `r="220" fill="#DCEAF5"`, `r="210" fill="#DCEAF5"`, 1)
	if child == humanReferenceSVG {
		t.Fatal("locked fixture replacement did not apply")
	}
	if err := ValidateLockedLayerContinuity([]byte(humanReferenceSVG), []byte(child), pack); !errors.Is(err, ErrLockedLayerContinuity) {
		t.Fatalf("locked layer change error = %v, want ErrLockedLayerContinuity", err)
	}
}

func TestValidateLockedLayerContinuityComparesAncestorPresentation(t *testing.T) {
	pack := BuiltInFlatVectorStylePack()
	wrapBackground := func(attribute string) string {
		child := strings.Replace(humanReferenceSVG,
			`<g id="background" data-layer="background">`,
			`<g `+attribute+`><g id="background" data-layer="background">`, 1)
		return strings.Replace(child,
			`</g><g id="base-identity" data-layer="base-identity">`,
			`</g></g><g id="base-identity" data-layer="base-identity">`, 1)
	}
	tests := map[string]string{
		"root transform": strings.Replace(humanReferenceSVG,
			`width="512" height="512" role="img"`,
			`width="512" height="512" transform="translate(1 0)" role="img"`, 1),
		"root opacity": strings.Replace(humanReferenceSVG,
			`width="512" height="512" role="img"`,
			`width="512" height="512" opacity="0.5" role="img"`, 1),
		"wrapper transform":               wrapBackground(`transform="translate(1 0)"`),
		"wrapper inherited fill":          wrapBackground(`fill="#203247"`),
		"wrapper inherited paint opacity": wrapBackground(`fill-opacity="0.5"`),
	}
	for name, child := range tests {
		t.Run(name, func(t *testing.T) {
			if child == humanReferenceSVG {
				t.Fatal("fixture replacement did not apply")
			}
			if err := ValidateSVGForStylePack([]byte(child), pack); err != nil {
				t.Fatalf("fixture must pass ordinary style validation: %v", err)
			}
			if err := ValidateLockedLayerContinuity([]byte(humanReferenceSVG), []byte(child), pack); !errors.Is(err, ErrLockedLayerContinuity) {
				t.Fatalf("ancestor bypass error = %v, want ErrLockedLayerContinuity", err)
			}
		})
	}
	stableRootPresentation := strings.Replace(humanReferenceSVG,
		`width="512" height="512" role="img"`,
		`width="512" height="512" opacity="0.5" role="img"`, 1)
	stableRootExpression := strings.Replace(stableRootPresentation,
		`<circle cx="214" cy="236" r="10" fill="#203247"></circle>`,
		`<circle cx="214" cy="236" r="12" fill="#203247"></circle>`, 1)
	if err := ValidateLockedLayerContinuity([]byte(stableRootPresentation), []byte(stableRootExpression), pack); err != nil {
		t.Fatalf("unchanged normalized ancestor presentation was rejected: %v", err)
	}
}

func TestValidateLockedLayerContinuityRejectsOutOfLayerDependencies(t *testing.T) {
	pack := BuiltInFlatVectorStylePack()
	clipChild := strings.Replace(humanReferenceSVG, `</title>`,
		`</title><defs><clipPath id="lockedClip"><circle cx="256" cy="256" r="200"></circle></clipPath></defs>`, 1)
	clipChild = strings.Replace(clipChild,
		`<g id="background" data-layer="background">`,
		`<g id="background" data-layer="background" clip-path="url(#lockedClip)">`, 1)
	if err := ValidateSVGForStylePack([]byte(clipChild), pack); !errors.Is(err, ErrInvalidSVG) {
		t.Fatalf("locked clip dependency error = %v, want ErrInvalidSVG", err)
	}
	if err := ValidateLockedLayerContinuity([]byte(humanReferenceSVG), []byte(clipChild), pack); !errors.Is(err, ErrInvalidSVG) {
		t.Fatalf("clip dependency continuity error = %v, want ErrInvalidSVG", err)
	}

	gradientPack := BuiltInFlatVectorStylePack()
	gradientPack.Grammar.AllowGradients = true
	gradientChild := strings.Replace(humanReferenceSVG, `</title>`,
		`</title><defs><linearGradient id="lockedPaint"><stop offset="0%" stop-color="#DCEAF5"></stop><stop offset="100%" stop-color="#2A9D8F"></stop></linearGradient></defs>`, 1)
	gradientChild = strings.Replace(gradientChild, `fill="#DCEAF5"`, `fill="url(#lockedPaint)"`, 1)
	if err := ValidateSVGForStylePack([]byte(gradientChild), gradientPack); !errors.Is(err, ErrInvalidSVG) {
		t.Fatalf("locked gradient dependency error = %v, want ErrInvalidSVG", err)
	}
	if err := ValidateLockedLayerContinuity([]byte(humanReferenceSVG), []byte(gradientChild), gradientPack); !errors.Is(err, ErrInvalidSVG) {
		t.Fatalf("gradient dependency continuity error = %v, want ErrInvalidSVG", err)
	}
}

func TestValidateLockedLayerContinuityValidatesBothDocuments(t *testing.T) {
	pack := BuiltInFlatVectorStylePack()
	degenerate := strings.Replace(humanReferenceSVG, `r="220" fill="#DCEAF5"`, `r="0" fill="#DCEAF5"`, 1)
	if err := ValidateLockedLayerContinuity([]byte(humanReferenceSVG), []byte(degenerate), pack); !errors.Is(err, ErrInvalidSVG) {
		t.Fatalf("invalid child error = %v, want ErrInvalidSVG", err)
	}
}
