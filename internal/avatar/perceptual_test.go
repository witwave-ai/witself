package avatar

import (
	"errors"
	"image/color"
	"reflect"
	"strings"
	"testing"
)

func TestPerceptualContinuityToleratesOrdinaryUnlockedEvolution(t *testing.T) {
	pack := BuiltInFlatVectorStylePack()
	parent := humanReferenceSVG
	tests := []struct {
		name  string
		child string
	}{
		{
			name: "expression",
			child: replacePerceptualTestLayer(t, parent, "expression",
				`<circle cx="210" cy="234" r="11" fill="#203247"></circle>`+
					`<circle cx="302" cy="234" r="11" fill="#203247"></circle>`+
					`<path d="M214 282 C238 310 274 310 298 282" fill="none" stroke="#203247" stroke-width="8" stroke-linecap="round"></path>`),
		},
		{
			name: "attire",
			child: replacePerceptualTestLayer(t, parent, "attire",
				`<path d="M190 354 L256 420 L322 354" fill="none" stroke="#7868E6" stroke-width="16" stroke-linecap="round" stroke-linejoin="round"></path>`),
		},
		{
			name: "experience",
			child: replacePerceptualTestLayer(t, parent, "experience",
				`<circle cx="174" cy="206" r="14" fill="#E76F51" stroke="#203247" stroke-width="6"></circle>`+
					`<path d="M184 270 C198 258 212 256 226 264" fill="none" stroke="#203247" stroke-width="6" stroke-linecap="round"></path>`),
		},
		{
			name: "glasses and moustache",
			child: replacePerceptualTestLayer(t, parent, "experience",
				`<circle cx="214" cy="236" r="28" fill="none" stroke="#203247" stroke-width="6"></circle>`+
					`<circle cx="298" cy="236" r="28" fill="none" stroke="#203247" stroke-width="6"></circle>`+
					`<path d="M242 236 L270 236" fill="none" stroke="#203247" stroke-width="6"></path>`+
					`<path d="M226 274 C240 264 250 270 256 282 C262 270 272 264 286 274 C276 296 236 296 226 274 Z" fill="#203247"></path>`),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			metrics, err := ComparePerceptualContinuity([]byte(parent), []byte(test.child), pack)
			t.Logf("metrics: %+v", metrics)
			if err != nil {
				t.Fatalf("ordinary %s evolution rejected: %v; metrics=%+v", test.name, err, metrics)
			}
			if metrics.IdentityMaskPixels < perceptualMinimumMaskPixels {
				t.Fatalf("identity mask too small: %+v", metrics)
			}
		})
	}
}

func TestPerceptualContinuityToleratesAccessoriesAcrossBuiltInForms(t *testing.T) {
	pack := BuiltInFlatVectorStylePack()
	fixtures := make(map[string]string, len(pack.References)+1)
	for _, reference := range pack.References {
		fixtures[string(reference.SubjectForm)] = reference.SVG
	}
	placeholder, err := GeneratePlaceholderSVGForStylePack("agent_accessory", "Juniper", pack)
	if err != nil {
		t.Fatal(err)
	}
	fixtures["placeholder"] = string(placeholder)
	const accessories = `<circle cx="214" cy="236" r="28" fill="none" stroke="#203247" stroke-width="6"></circle>` +
		`<circle cx="298" cy="236" r="28" fill="none" stroke="#203247" stroke-width="6"></circle>` +
		`<path d="M242 236 L270 236" fill="none" stroke="#203247" stroke-width="6"></path>` +
		`<path d="M226 274 C240 264 250 270 256 282 C262 270 272 264 286 274 C276 296 236 296 226 274 Z" fill="#203247"></path>`
	for name, parent := range fixtures {
		t.Run(name, func(t *testing.T) {
			child := replacePerceptualTestLayer(t, parent, "experience", accessories)
			if _, err := SanitizePerceptualV1AvatarBaseline([]byte(child), pack); err != nil {
				t.Fatalf("accessory baseline: %v", err)
			}
			metrics, err := ComparePerceptualContinuity([]byte(parent), []byte(child), pack)
			if err != nil {
				t.Fatalf("ordinary accessories rejected: %v; metrics=%+v", err, metrics)
			}
			if metrics.FocusChangedRatio > PerceptualFocusChangedRatioLimit ||
				metrics.FocusMeanDelta > PerceptualFocusMeanDeltaLimit ||
				metrics.FocusAddedOcclusion > PerceptualFocusAddedOcclusionRatioLimit {
				t.Fatalf("ordinary accessory metrics crossed focus limit: %+v", metrics)
			}
		})
	}
}

func TestPerceptualContinuityRejectsDrasticWholePortraitReplacement(t *testing.T) {
	pack := BuiltInFlatVectorStylePack()
	child := replacePerceptualTestLayer(t, humanReferenceSVG, "experience",
		`<rect x="0" y="0" width="512" height="512" fill="#7868E6"></rect>`)
	metrics, err := ComparePerceptualContinuity([]byte(humanReferenceSVG), []byte(child), pack)
	t.Logf("metrics: %+v", metrics)
	if !errors.Is(err, ErrPerceptualContinuity) {
		t.Fatalf("whole portrait replacement error = %v, metrics=%+v", err, metrics)
	}
	if metrics.WholeChangedRatio <= PerceptualWholeChangedRatioLimit &&
		metrics.WholeMeanDelta <= PerceptualWholeMeanDeltaLimit {
		t.Fatalf("whole portrait metrics did not cross a limit: %+v", metrics)
	}
}

func TestPerceptualContinuityRejectsUnlockedLockedIdentityOcclusion(t *testing.T) {
	pack := BuiltInFlatVectorStylePack()
	child := replacePerceptualTestLayer(t, humanReferenceSVG, "experience",
		`<circle cx="256" cy="230" r="136" fill="#F7FAFC"></circle>`)
	metrics, err := ComparePerceptualContinuity([]byte(humanReferenceSVG), []byte(child), pack)
	t.Logf("metrics: %+v", metrics)
	if !errors.Is(err, ErrPerceptualContinuity) {
		t.Fatalf("locked identity occlusion error = %v, metrics=%+v", err, metrics)
	}
	if metrics.AddedIdentityOcclusion <= PerceptualAddedOcclusionRatioLimit {
		t.Fatalf("front overlay did not cross the visible unlocked-influence limit: %+v", metrics)
	}
}

func TestPerceptualContinuityRejectsWithinFocusMaskDilution(t *testing.T) {
	pack := BuiltInFlatVectorStylePack()
	parent := replacePerceptualTestLayer(t, humanReferenceSVG, "base-identity",
		`<circle cx="256" cy="240" r="175" fill="#DCEAF5" stroke="#203247" stroke-width="8"></circle>`)
	parent = replacePerceptualTestLayer(t, parent, "expression",
		`<circle cx="256" cy="230" r="75" fill="#E9C46A" stroke="#203247" stroke-width="8"></circle>`+
			`<circle cx="228" cy="224" r="9" fill="#203247"></circle>`+
			`<circle cx="284" cy="224" r="9" fill="#203247"></circle>`+
			`<path d="M226 270 C242 286 270 286 286 270" fill="none" stroke="#203247" stroke-width="8" stroke-linecap="round"></path>`)
	child := replacePerceptualTestLayer(t, parent, "expression",
		`<circle cx="256" cy="230" r="75" fill="#7868E6" stroke="#203247" stroke-width="8"></circle>`+
			`<path d="M204 208 L308 250 M204 250 L308 208" fill="none" stroke="#F7FAFC" stroke-width="14" stroke-linecap="round"></path>`)
	if _, err := SanitizePerceptualV1AvatarBaseline([]byte(parent), pack); err != nil {
		t.Fatalf("parent baseline: %v", err)
	}
	if _, err := SanitizePerceptualV1AvatarBaseline([]byte(child), pack); err != nil {
		t.Fatalf("child baseline: %v", err)
	}
	if err := ValidateLockedLayerContinuity([]byte(parent), []byte(child), pack); err != nil {
		t.Fatalf("locked layers changed in dilution fixture: %v", err)
	}
	metrics, err := ComparePerceptualContinuity([]byte(parent), []byte(child), pack)
	if !errors.Is(err, ErrPerceptualContinuity) {
		t.Fatalf("within-focus mask dilution error = %v, metrics=%+v", err, metrics)
	}
	if metrics.WholeChangedRatio > PerceptualWholeChangedRatioLimit ||
		metrics.WholeMeanDelta > PerceptualWholeMeanDeltaLimit ||
		metrics.IdentityChangedRatio > PerceptualIdentityChangedRatioLimit ||
		metrics.IdentityMeanDelta > PerceptualIdentityMeanDeltaLimit ||
		metrics.AddedIdentityOcclusion > PerceptualAddedOcclusionRatioLimit {
		t.Fatalf("fixture no longer isolates the fixed-focus guard: %+v", metrics)
	}
	if metrics.FocusChangedRatio <= PerceptualFocusChangedRatioLimit &&
		metrics.FocusMeanDelta <= PerceptualFocusMeanDeltaLimit {
		t.Fatalf("fixed focus did not cross a continuity limit: %+v", metrics)
	}
}

func TestPerceptualContinuityAllowsUnlockedArtworkHiddenBehindLockedIdentity(t *testing.T) {
	pack := BuiltInFlatVectorStylePack()
	const emptyExperience = `<g id="experience" data-layer="experience"></g>`
	const hiddenExperience = `<g id="experience" data-layer="experience"><rect x="0" y="0" width="512" height="512" fill="#7868E6"></rect></g>`
	child := strings.Replace(humanReferenceSVG, emptyExperience, "", 1)
	child = strings.Replace(child, `<g id="background"`, hiddenExperience+`<g id="background"`, 1)

	metrics, err := ComparePerceptualContinuity([]byte(humanReferenceSVG), []byte(child), pack)
	if err != nil {
		t.Fatalf("pixel-identical artwork behind locked layers was rejected: %v; metrics=%+v", err, metrics)
	}
	if metrics.WholeChangedRatio != 0 || metrics.WholeMeanDelta != 0 ||
		metrics.AddedIdentityOcclusion != 0 {
		t.Fatalf("hidden unlocked artwork changed visible metrics: %+v", metrics)
	}
}

func TestPerceptualContinuityMetricsAreDeterministic(t *testing.T) {
	pack := BuiltInFlatVectorStylePack()
	child := replacePerceptualTestLayer(t, humanReferenceSVG, "expression",
		`<circle cx="212" cy="236" r="12" fill="#203247"></circle>`+
			`<circle cx="300" cy="236" r="12" fill="#203247"></circle>`+
			`<path d="M218 284 C240 306 272 306 294 284" fill="none" stroke="#203247" stroke-width="8" stroke-linecap="round"></path>`)
	first, err := ComparePerceptualContinuity([]byte(humanReferenceSVG), []byte(child), pack)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ComparePerceptualContinuity([]byte(humanReferenceSVG), []byte(child), pack)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("metrics differ: first=%+v second=%+v", first, second)
	}
}

func TestPerceptualCompositeUsesPremultipliedRGBA(t *testing.T) {
	got := perceptualCompositeRGB(color.RGBA{R: 128, A: 128})
	want := [3]byte{251, 125, 126}
	if got != want {
		t.Fatalf("composite = %v, want %v", got, want)
	}
}

func TestPerceptualRenderFailsClosed(t *testing.T) {
	if _, err := renderPerceptualAvatar([]byte(`<svg`)); !errors.Is(err, ErrPerceptualRender) {
		t.Fatalf("malformed render error = %v, want ErrPerceptualRender", err)
	}
	canonical, err := SanitizeSVG([]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 0 32"><circle cx="16" cy="16" r="12" fill="#203247"></circle></svg>`))
	if err != nil {
		t.Fatalf("build canonical pathological SVG: %v", err)
	}
	if _, err := renderPerceptualAvatar(canonical); !errors.Is(err, ErrPerceptualRender) {
		t.Fatalf("zero-width canonical render error = %v, want ErrPerceptualRender", err)
	}
}

func TestPerceptualLayerProjectionPreservesNestedWrappers(t *testing.T) {
	const title = `<title>Human teammate reference</title>`
	wrapped := strings.Replace(humanReferenceSVG, title,
		title+`<g>`, 1)
	wrapped = strings.TrimSuffix(wrapped, `</svg>`) + `</g></svg>`
	metrics, err := ComparePerceptualContinuity([]byte(wrapped), []byte(wrapped), BuiltInFlatVectorStylePack())
	if err != nil {
		t.Fatalf("nested wrapper comparison: %v", err)
	}
	if metrics.IdentityMaskPixels < perceptualMinimumMaskPixels {
		t.Fatalf("nested wrappers hid the selected identity mask: %+v", metrics)
	}
}

func BenchmarkComparePerceptualContinuity(b *testing.B) {
	pack := BuiltInFlatVectorStylePack()
	child := replacePerceptualBenchmarkLayer(humanReferenceSVG, "experience",
		`<circle cx="174" cy="206" r="14" fill="#E76F51" stroke="#203247" stroke-width="6"></circle>`)
	b.ReportAllocs()
	for range b.N {
		if _, err := ComparePerceptualContinuity([]byte(humanReferenceSVG), []byte(child), pack); err != nil {
			b.Fatal(err)
		}
	}
}

func replacePerceptualTestLayer(t *testing.T, svg, layer, content string) string {
	t.Helper()
	start := `<g id="` + layer + `" data-layer="` + layer + `">`
	startAt := strings.Index(svg, start)
	if startAt < 0 {
		t.Fatalf("layer %q start is missing", layer)
	}
	contentAt := startAt + len(start)
	endAt := strings.Index(svg[contentAt:], `</g>`)
	if endAt < 0 {
		t.Fatalf("layer %q end is missing", layer)
	}
	endAt += contentAt
	return svg[:contentAt] + content + svg[endAt:]
}

func replacePerceptualBenchmarkLayer(svg, layer, content string) string {
	start := `<g id="` + layer + `" data-layer="` + layer + `">`
	startAt := strings.Index(svg, start)
	contentAt := startAt + len(start)
	endAt := strings.Index(svg[contentAt:], `</g>`) + contentAt
	return svg[:contentAt] + content + svg[endAt:]
}
