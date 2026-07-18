package avatar

import (
	"errors"
	"strings"
	"testing"
)

func TestPerceptualV1BuiltInSpeciesAndPlaceholderAreCompatible(t *testing.T) {
	pack := BuiltInFlatVectorStylePack()
	if err := ValidatePerceptualV1StylePack(pack); err != nil {
		t.Fatalf("built-in style profile: %v", err)
	}
	for _, reference := range pack.References {
		reference := reference
		t.Run(string(reference.SubjectForm), func(t *testing.T) {
			canonical, err := SanitizePerceptualV1AvatarBaseline([]byte(reference.SVG), pack)
			if err != nil {
				t.Fatalf("reference profile: %v", err)
			}
			if _, err := renderPerceptualAvatar(canonical); err != nil {
				t.Fatalf("reference render: %v", err)
			}
		})
	}
	placeholder, err := GeneratePlaceholderSVGForStylePack("agent_profile", "Juniper", pack)
	if err != nil {
		t.Fatalf("placeholder profile: %v", err)
	}
	if _, err := renderPerceptualAvatar(placeholder); err != nil {
		t.Fatalf("placeholder render: %v", err)
	}
}

func TestPerceptualV1BuiltInIdentityProjectionsStayWithinBounds(t *testing.T) {
	pack := BuiltInFlatVectorStylePack()
	fixtures := make(map[string][]byte, len(pack.References)+1)
	for _, reference := range pack.References {
		fixtures[string(reference.SubjectForm)] = []byte(reference.SVG)
	}
	placeholder, err := GeneratePlaceholderSVGForStylePack("agent_profile", "Juniper", pack)
	if err != nil {
		t.Fatal(err)
	}
	fixtures["placeholder"] = placeholder
	for name, fixture := range fixtures {
		_, mask, _, err := renderPerceptualParentProjection(fixture, pack)
		if err != nil {
			t.Fatalf("%s projection: %v", name, err)
		}
		maskPixels := countPerceptualMaskPixels(mask)
		focusPixels := countPerceptualFocusMaskPixels(mask)
		if maskPixels < perceptualMinimumMaskPixels || maskPixels > perceptualMaximumMaskPixels {
			t.Fatalf("%s mask pixels = %d", name, maskPixels)
		}
		if focusPixels < perceptualMinimumFocusMaskPixels {
			t.Fatalf("%s focus pixels = %d", name, focusPixels)
		}
		t.Logf("%s mask=%d/%d focus=%d", name, maskPixels,
			PerceptualRenderSize*PerceptualRenderSize, focusPixels)
	}
}

func TestPerceptualV1RejectsRendererMismatchWithoutNarrowingGenericSanitizer(t *testing.T) {
	tests := map[string]string{
		"transform":      `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32" width="32" height="32"><circle cx="16" cy="16" r="12" fill="#203247" transform="translate(1 0)"></circle></svg>`,
		"definition":     `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32" width="32" height="32"><defs></defs><circle cx="16" cy="16" r="12" fill="#203247"></circle></svg>`,
		"clip path":      `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32" width="32" height="32"><defs><clipPath id="clip"><circle cx="16" cy="16" r="8"></circle></clipPath></defs><circle cx="16" cy="16" r="12" fill="#203247" clip-path="url(#clip)"></circle></svg>`,
		"gradient paint": `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32" width="32" height="32"><defs><linearGradient id="paint"><stop offset="0" stop-color="#203247"></stop></linearGradient></defs><rect x="0" y="0" width="32" height="32" fill="url(#paint)"></rect></svg>`,
		"percentage":     `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32" width="32" height="32"><circle cx="50%" cy="16" r="12" fill="#203247"></circle></svg>`,
		"alpha hex":      `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32" width="32" height="32"><circle cx="16" cy="16" r="12" fill="#20324780"></circle></svg>`,
		"fill rule":      `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32" width="32" height="32"><path d="M1 1 L31 1 L16 31 Z" fill="#203247" fill-rule="evenodd"></path></svg>`,
		"vector effect":  `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32" width="32" height="32"><path d="M1 1 L31 31" fill="none" stroke="#203247" stroke-width="2" vector-effect="non-scaling-stroke"></path></svg>`,
		"aspect ratio":   `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32" width="32" height="32" preserveAspectRatio="none"><circle cx="16" cy="16" r="12" fill="#203247"></circle></svg>`,
		"root opacity":   `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32" width="32" height="32" opacity="1"><circle cx="16" cy="16" r="12" fill="#203247"></circle></svg>`,
		"group opacity":  `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32" width="32" height="32"><g opacity="1"><circle cx="16" cy="16" r="12" fill="#203247"></circle></g></svg>`,
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := SanitizeSVG([]byte(raw)); err != nil {
				t.Fatalf("generic sanitizer was narrowed: %v", err)
			}
			if _, err := SanitizeSVGForPerceptualV1([]byte(raw)); !errors.Is(err, ErrPerceptualProfile) {
				t.Fatalf("profile error = %v, want ErrPerceptualProfile", err)
			}
		})
	}
}

func TestPerceptualV1NumericAndArcBounds(t *testing.T) {
	tests := map[string]string{
		"large finite coordinate": `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32" width="32" height="32"><circle cx="1e308" cy="16" r="12" fill="#203247"></circle></svg>`,
		"large stroke":            `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32" width="32" height="32"><path d="M1 1 L31 31" fill="none" stroke="#203247" stroke-width="65"></path></svg>`,
		"tiny viewbox":            `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 0.5 0.5" width="32" height="32"><circle cx="0.25" cy="0.25" r="0.1" fill="#203247"></circle></svg>`,
		"offset viewbox":          `<svg xmlns="http://www.w3.org/2000/svg" viewBox="1 1 32 32" width="32" height="32"><circle cx="16" cy="16" r="12" fill="#203247"></circle></svg>`,
		"nonsquare viewbox":       `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 31" width="32" height="31"><circle cx="16" cy="15" r="12" fill="#203247"></circle></svg>`,
		"zero arc radius":         `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32" width="32" height="32"><path d="M1 1 A0 4 0 0 1 20 20" fill="none" stroke="#203247"></path></svg>`,
		"arc rotation":            `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32" width="32" height="32"><path d="M1 1 A4 4 361 0 1 20 20" fill="none" stroke="#203247"></path></svg>`,
		"arc ratio":               `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32" width="32" height="32"><path d="M1 1 A128 1 0 0 1 20 20" fill="none" stroke="#203247"></path></svg>`,
		"relative overflow":       `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32" width="32" height="32"><path d="M8192 0 l1 0" fill="none" stroke="#203247"></path></svg>`,
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := SanitizeSVG([]byte(raw)); err != nil {
				t.Fatalf("generic sanitizer rejected test fixture: %v", err)
			}
			if _, err := SanitizeSVGForPerceptualV1([]byte(raw)); !errors.Is(err, ErrPerceptualProfile) {
				t.Fatalf("profile error = %v, want ErrPerceptualProfile", err)
			}
		})
	}
}

func TestPerceptualV1RenderWorkBudgetBoundary(t *testing.T) {
	if got, err := addPerceptualV1RenderWork(0, PerceptualV1RenderWorkLimit/2-1); err != nil || got != PerceptualV1RenderWorkLimit-2 {
		t.Fatalf("budget-2 = %d / %v", got, err)
	}
	if got, err := addPerceptualV1RenderWork(0, PerceptualV1RenderWorkLimit/2); err != nil || got != PerceptualV1RenderWorkLimit {
		t.Fatalf("exact budget = %d / %v", got, err)
	}
	if _, err := addPerceptualV1RenderWork(0, PerceptualV1RenderWorkLimit/2+1); !errors.Is(err, ErrPerceptualProfile) {
		t.Fatalf("budget+2 error = %v", err)
	}

	oneCurve := `<path d="M0 0 C8192 8192 -8192 -8192 1 1" fill="#203247"></path>`
	underBudget := `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1 1" width="32" height="32">` + oneCurve + `</svg>`
	overBudget := `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1 1" width="32" height="32">` + strings.Repeat(oneCurve, 2) + `</svg>`
	if _, err := SanitizeSVGForPerceptualV1([]byte(underBudget)); err != nil {
		t.Fatalf("bounded curves: %v", err)
	}
	if _, err := SanitizeSVGForPerceptualV1([]byte(overBudget)); !errors.Is(err, ErrPerceptualProfile) {
		t.Fatalf("over-budget curves error = %v", err)
	}
}

func TestPerceptualV1BaselineFailsClosedOnTinyLockedIdentity(t *testing.T) {
	pack := BuiltInFlatVectorStylePack()
	tiny := replacePerceptualTestLayer(t, humanReferenceSVG, "base-identity",
		`<circle cx="1" cy="1" r="1" fill="#203247"></circle>`)
	if _, err := SanitizePerceptualV1AvatarBaseline([]byte(tiny), pack); !errors.Is(err, ErrPerceptualRender) {
		t.Fatalf("tiny baseline error = %v, want ErrPerceptualRender", err)
	}
}

func TestPerceptualV1BaselineFailsClosedOnOversizedLockedIdentity(t *testing.T) {
	pack := BuiltInFlatVectorStylePack()
	oversized := replacePerceptualTestLayer(t, humanReferenceSVG, "base-identity",
		`<rect x="0" y="0" width="512" height="512" fill="#DCEAF5"></rect>`)
	if _, err := SanitizePerceptualV1AvatarBaseline([]byte(oversized), pack); !errors.Is(err, ErrPerceptualRender) {
		t.Fatalf("oversized baseline error = %v, want ErrPerceptualRender", err)
	}
}

func TestPerceptualV1StylePackRejectsUnsupportedMetadata(t *testing.T) {
	pack := BuiltInFlatVectorStylePack()
	pack.Grammar.AllowGradients = true
	if err := ValidatePerceptualV1StylePack(pack); !errors.Is(err, ErrPerceptualProfile) {
		t.Fatalf("gradient-enabled pack error = %v", err)
	}
	pack = BuiltInFlatVectorStylePack()
	pack.Palette = append(pack.Palette, ColorSpec{Name: "alpha-ink", Hex: "#20324780"})
	// Keep the generic style self-consistent to prove only the new profile
	// rejects the unused alpha palette.
	if err := pack.Validate(); err != nil {
		t.Fatalf("generic style validation was narrowed: %v", err)
	}
	if err := ValidatePerceptualV1StylePack(pack); !errors.Is(err, ErrPerceptualProfile) {
		t.Fatalf("alpha palette error = %v", err)
	}
}

func TestPerceptualV1StylePackRejectsBackgroundOnlyIdentityMask(t *testing.T) {
	pack := BuiltInFlatVectorStylePack()
	for index := range pack.Layers {
		pack.Layers[index].LockedByDefault = pack.Layers[index].Name == "background"
	}
	if err := pack.Validate(); err != nil {
		t.Fatalf("generic style validation was narrowed: %v", err)
	}
	if err := ValidatePerceptualV1StylePack(pack); !errors.Is(err, ErrPerceptualProfile) {
		t.Fatalf("background-only identity error = %v, want ErrPerceptualProfile", err)
	}
}
