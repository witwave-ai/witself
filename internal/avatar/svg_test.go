package avatar

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestSanitizeSVGAcceptsStaticVectorSubset(t *testing.T) {
	raw := []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 512 512" width="512" height="512" role="img" aria-label="Safe portrait">
  <!-- stripped -->
  <title>Safe portrait</title>
  <defs>
    <linearGradient id="paint" x1="0" y1="0" x2="1" y2="1" gradientUnits="objectBoundingBox">
      <stop offset="0%" stop-color="#DCEAF5"></stop>
      <stop offset="100%" stop-color="#2A9D8F"></stop>
    </linearGradient>
    <clipPath id="medallion"><circle cx="256" cy="256" r="220"></circle></clipPath>
  </defs>
  <g id="background" data-layer="background" clip-path="url(#medallion)">
    <rect x="0" y="0" width="512" height="512" fill="url(#paint)"></rect>
  </g>
</svg>`)
	got, err := SanitizeSVG(raw)
	if err != nil {
		t.Fatalf("SanitizeSVG() error = %v", err)
	}
	if bytes.Contains(got, []byte("stripped")) || bytes.Contains(got, []byte("\n")) {
		t.Fatalf("sanitized SVG retained comments or insignificant whitespace: %s", got)
	}
	if len(got) > MaxSVGBytes {
		t.Fatalf("sanitized SVG has %d bytes", len(got))
	}
	if err := ValidateSVG(got); err != nil {
		t.Fatalf("sanitized SVG did not revalidate: %v\n%s", err, got)
	}
}

func TestSanitizeSVGRejectsUnsafeContent(t *testing.T) {
	tests := map[string]string{
		"script":             `<svg><script>alert(1)</script></svg>`,
		"foreign object":     `<svg><foreignObject></foreignObject></svg>`,
		"event handler":      `<svg><circle cx="1" cy="1" r="1" onload="alert(1)"></circle></svg>`,
		"mixed case handler": `<svg><circle cx="1" cy="1" r="1" OnClick="alert(1)"></circle></svg>`,
		"external href":      `<svg><circle cx="1" cy="1" r="1" href="https://example.test/a"></circle></svg>`,
		"xlink namespace":    `<svg xmlns:xlink="http://www.w3.org/1999/xlink"><circle xlink:href="#x"></circle></svg>`,
		"image":              `<svg><image href="data:image/png;base64,AA"></image></svg>`,
		"style element":      `<svg><style>circle{fill:red}</style></svg>`,
		"style attribute":    `<svg><circle style="fill:red"></circle></svg>`,
		"javascript paint":   `<svg><circle fill="javascript:alert(1)"></circle></svg>`,
		"external paint":     `<svg><circle fill="url(https://example.test/a)"></circle></svg>`,
		"escaped protocol":   `<svg aria-label="&#x68;ttps://example.test"><circle></circle></svg>`,
		"external namespace": `<svg xmlns="https://example.test/svg"><circle></circle></svg>`,
		"nested svg":         `<svg><svg></svg></svg>`,
		"animation":          `<svg><animate attributeName="x"></animate></svg>`,
		"doctype":            `<!DOCTYPE svg><svg></svg>`,
		"processing":         `<?xml version="1.0"?><svg></svg>`,
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := SanitizeSVG([]byte(raw)); !errors.Is(err, ErrUnsafeSVG) {
				t.Fatalf("SanitizeSVG() error = %v, want ErrUnsafeSVG", err)
			}
		})
	}
}

func TestSanitizeSVGRejectsInvalidContent(t *testing.T) {
	manyElements := `<svg>` + strings.Repeat(`<circle></circle>`, maxSVGElements) + `</svg>`
	deep := `<svg>` + strings.Repeat(`<g>`, maxSVGDepth) + strings.Repeat(`</g>`, maxSVGDepth) + `</svg>`
	tests := map[string][]byte{
		"empty":                 nil,
		"malformed":             []byte(`<svg><circle></svg>`),
		"wrong root":            []byte(`<g></g>`),
		"second root":           []byte(`<svg></svg><svg></svg>`),
		"unknown element":       []byte(`<svg><metadata></metadata></svg>`),
		"unknown attribute":     []byte(`<svg foo="bar"></svg>`),
		"duplicate attribute":   []byte(`<svg viewBox="0 0 1 1" viewBox="0 0 2 2"></svg>`),
		"duplicate ID":          []byte(`<svg><g id="same"></g><g id="same"></g></svg>`),
		"unresolved paint":      []byte(`<svg><circle fill="url(#missing)"></circle></svg>`),
		"text outside metadata": []byte(`<svg>visible text</svg>`),
		"invalid path":          []byte(`<svg><path d="M0 0;script"></path></svg>`),
		"too many elements":     []byte(manyElements),
		"too deeply nested":     []byte(deep),
		"too large":             bytesOf(' ', MaxSVGBytes+1),
		"invalid UTF-8":         {0xff},
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := SanitizeSVG(raw); !errors.Is(err, ErrInvalidSVG) {
				t.Fatalf("SanitizeSVG() error = %v, want ErrInvalidSVG", err)
			}
		})
	}
}

func TestSVGForStylePackEnforcesCanvasAndLayers(t *testing.T) {
	pack := BuiltInFlatVectorStylePack()
	overBudgetShapes := strings.Repeat(`<circle cx="1" cy="1" r="1"></circle>`, pack.Grammar.MaxElements)
	valid := styleCompliantSVG("512", "512", "0 0 512 512",
		`<g data-layer="background"><rect width="1" height="1" fill="#DCEAF5"></rect></g>`,
		`<g data-layer="base-identity"><circle r="1" fill="#E9C46A"></circle></g>`,
		`<g data-layer="expression"><path d="M0 0 L1 1" fill="none" stroke="#203247"></path></g>`,
	)
	if got, err := SanitizeSVGForStylePack([]byte(valid), pack); err != nil {
		t.Fatalf("valid style SVG: %v", err)
	} else if err := ValidateSVG(got); err != nil {
		t.Fatalf("style-sanitized SVG did not revalidate: %v", err)
	}

	tests := map[string]string{
		"wrong width": styleCompliantSVG("256", "512", "0 0 512 512",
			`<g data-layer="background"></g><g data-layer="base-identity"></g><g data-layer="expression"></g>`),
		"wrong viewbox": styleCompliantSVG("512", "512", "0 0 256 256",
			`<g data-layer="background"></g><g data-layer="base-identity"></g><g data-layer="expression"></g>`),
		"missing required layer": styleCompliantSVG("512", "512", "0 0 512 512",
			`<g data-layer="background"></g><g data-layer="base-identity"></g>`),
		"empty required layer": styleCompliantSVG("512", "512", "0 0 512 512",
			`<g data-layer="background"><rect fill="#DCEAF5"></rect></g><g data-layer="base-identity"><circle fill="#E9C46A"></circle></g><g data-layer="expression"></g>`),
		"non-rendering required layer": styleCompliantSVG("512", "512", "0 0 512 512",
			`<g data-layer="background"><rect fill="none"></rect></g><g data-layer="base-identity"><circle fill="#E9C46A"></circle></g><g data-layer="expression"><circle fill="#203247"></circle></g>`),
		"duplicate layer": styleCompliantSVG("512", "512", "0 0 512 512",
			`<g data-layer="background"></g><g data-layer="base-identity"></g><g data-layer="expression"></g><g data-layer="expression"></g>`),
		"undeclared layer": styleCompliantSVG("512", "512", "0 0 512 512",
			`<g data-layer="background"></g><g data-layer="base-identity"></g><g data-layer="expression"></g><g data-layer="surprise"></g>`),
		"layer on shape": styleCompliantSVG("512", "512", "0 0 512 512",
			`<g data-layer="background"></g><g data-layer="base-identity"></g><circle data-layer="expression"></circle>`),
		"visible geometry outside layer": styleCompliantSVG("512", "512", "0 0 512 512",
			`<circle fill="#DCEAF5"></circle><g data-layer="background"></g><g data-layer="base-identity"></g><g data-layer="expression"></g>`),
		"visible geometry under nested layers": styleCompliantSVG("512", "512", "0 0 512 512",
			`<g data-layer="background"><g data-layer="base-identity"><circle fill="#DCEAF5"></circle></g></g><g data-layer="expression"></g>`),
		"omitted default fill": styleCompliantSVG("512", "512", "0 0 512 512",
			`<g data-layer="background"><rect></rect></g><g data-layer="base-identity"></g><g data-layer="expression"></g>`),
		"omitted default line stroke": styleCompliantSVG("512", "512", "0 0 512 512",
			`<g data-layer="background"><line x1="0" y1="0" x2="1" y2="1"></line></g><g data-layer="base-identity"></g><g data-layer="expression"></g>`),
		"color outside palette": styleCompliantSVG("512", "512", "0 0 512 512",
			`<g data-layer="background"><rect fill="#123456"></rect></g><g data-layer="base-identity"></g><g data-layer="expression"></g>`),
		"disabled gradient": styleCompliantSVG("512", "512", "0 0 512 512",
			`<defs><linearGradient id="paint"><stop offset="0%" stop-color="#DCEAF5"></stop></linearGradient></defs><g data-layer="background"><rect fill="url(#paint)"></rect></g><g data-layer="base-identity"></g><g data-layer="expression"></g>`),
		"over style element budget": styleCompliantSVG("512", "512", "0 0 512 512",
			`<g data-layer="background">`+overBudgetShapes+`</g><g data-layer="base-identity"></g><g data-layer="expression"></g>`),
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if err := ValidateSVG(rawBytes(raw)); err != nil {
				t.Fatalf("test SVG should be generically safe: %v", err)
			}
			if err := ValidateSVGForStylePack([]byte(raw), pack); !errors.Is(err, ErrInvalidSVG) {
				t.Fatalf("ValidateSVGForStylePack() error = %v, want ErrInvalidSVG", err)
			}
		})
	}
}

func TestSVGForStylePackAcceptsExplicitInheritedPaint(t *testing.T) {
	pack := BuiltInFlatVectorStylePack()
	raw := styleCompliantSVG("512", "512", "0 0 512 512",
		`<g data-layer="background" fill="#DCEAF5"><rect width="1" height="1"></rect></g>`,
		`<g data-layer="base-identity" fill="none" stroke="#203247"><path d="M0 0 L1 1"></path><line x1="0" y1="0" x2="1" y2="1"></line></g>`,
		`<g data-layer="expression"><circle r="1" fill="#203247"></circle></g>`,
	)
	if err := ValidateSVGForStylePack([]byte(raw), pack); err != nil {
		t.Fatalf("explicit inherited palette paint was rejected: %v", err)
	}
}

func TestSVGForStylePackRejectsProvablyDegenerateGeometry(t *testing.T) {
	pack := BuiltInFlatVectorStylePack()
	otherRequiredLayers := `<g data-layer="base-identity"><circle r="1" fill="#E9C46A"></circle></g><g data-layer="expression"><circle r="1" fill="#203247"></circle></g>`
	tests := map[string]string{
		"zero radius circle":                   `<circle r="0" fill="#DCEAF5"></circle>`,
		"zero radius ellipse":                  `<ellipse rx="1" ry="0" fill="#DCEAF5"></ellipse>`,
		"zero width rectangle":                 `<rect width="0" height="1" fill="#DCEAF5"></rect>`,
		"identical line endpoints":             `<line x1="1" y1="1" x2="1" y2="1" stroke="#203247"></line>`,
		"polygon has too few distinct points":  `<polygon points="0,0 1,1 0,0" fill="#DCEAF5"></polygon>`,
		"polyline has too few distinct points": `<polyline points="0,0 0,0" fill="none" stroke="#203247"></polyline>`,
		"path has no drawing command":          `<path d="M0 0 Z" fill="#DCEAF5"></path>`,
		"path command has no arguments":        `<path d="M0 0 L" fill="none" stroke="#203247"></path>`,
		"path line is zero length":             `<path d="M0 0 L0 0" fill="none" stroke="#203247"></path>`,
		"path horizontal line is zero length":  `<path d="M1 2 H1" fill="none" stroke="#203247"></path>`,
		"path vertical line is zero length":    `<path d="M1 2 V2" fill="none" stroke="#203247"></path>`,
	}
	for name, geometry := range tests {
		t.Run(name, func(t *testing.T) {
			raw := styleCompliantSVG("512", "512", "0 0 512 512",
				`<g data-layer="background">`+geometry+`</g>`+otherRequiredLayers)
			if err := ValidateSVG([]byte(raw)); err != nil {
				t.Fatalf("test SVG should be generically safe: %v", err)
			}
			if err := ValidateSVGForStylePack([]byte(raw), pack); !errors.Is(err, ErrInvalidSVG) {
				t.Fatalf("ValidateSVGForStylePack() error = %v, want ErrInvalidSVG", err)
			}
		})
	}
}

func TestSVGForStylePackRejectsNonRenderingFillAndStroke(t *testing.T) {
	pack := BuiltInFlatVectorStylePack()
	otherRequiredLayers := `<g data-layer="base-identity"><circle r="1" fill="#E9C46A"></circle></g><g data-layer="expression"><circle r="1" fill="#203247"></circle></g>`
	tests := map[string]string{
		"zero width line stroke":           `<line x1="0" y1="0" x2="1" y2="1" stroke="#203247" stroke-width="0"></line>`,
		"inherited zero width path stroke": `<g stroke="#203247" stroke-width="0"><path d="M0 0 L1 1" fill="none"></path></g>`,
		"fill-only open path":              `<path d="M0 0 L1 1" fill="#DCEAF5"></path>`,
		"fill-only two-point polyline":     `<polyline points="0,0 1,1" fill="#DCEAF5"></polyline>`,
		"fill-only collinear polygon":      `<polygon points="0,0 1,1 2,2" fill="#DCEAF5"></polygon>`,
	}
	for name, geometry := range tests {
		t.Run(name, func(t *testing.T) {
			raw := styleCompliantSVG("512", "512", "0 0 512 512",
				`<g data-layer="background">`+geometry+`</g>`+otherRequiredLayers)
			if err := ValidateSVG([]byte(raw)); err != nil {
				t.Fatalf("test SVG should be generically safe: %v", err)
			}
			if err := ValidateSVGForStylePack([]byte(raw), pack); !errors.Is(err, ErrInvalidSVG) {
				t.Fatalf("ValidateSVGForStylePack() error = %v, want ErrInvalidSVG", err)
			}
		})
	}

	validPolygon := styleCompliantSVG("512", "512", "0 0 512 512",
		`<g data-layer="background"><polygon points="0,0 2,0 0,2" fill="#DCEAF5"></polygon></g>`+otherRequiredLayers)
	if err := ValidateSVGForStylePack([]byte(validPolygon), pack); err != nil {
		t.Fatalf("non-collinear fill-only polygon was rejected: %v", err)
	}
}

func TestSVGForStylePackRejectsGradientPaintBypasses(t *testing.T) {
	pack := BuiltInFlatVectorStylePack()
	pack.Grammar.AllowGradients = true
	tests := map[string]string{
		"omitted stop color": styleCompliantSVG("512", "512", "0 0 512 512",
			`<defs><linearGradient id="paint"><stop offset="0%"></stop></linearGradient></defs><g data-layer="background"><rect fill="url(#paint)"></rect></g><g data-layer="base-identity"></g><g data-layer="expression"></g>`),
		"stop color url": styleCompliantSVG("512", "512", "0 0 512 512",
			`<defs><linearGradient id="paint"><stop offset="0%" stop-color="url(#other)"></stop></linearGradient><linearGradient id="other"><stop offset="0%" stop-color="#DCEAF5"></stop></linearGradient></defs><g data-layer="background"><rect fill="url(#paint)"></rect></g><g data-layer="base-identity"></g><g data-layer="expression"></g>`),
		"paint url targets clip path": styleCompliantSVG("512", "512", "0 0 512 512",
			`<defs><clipPath id="paint"><circle cx="1" cy="1" r="1"></circle></clipPath></defs><g data-layer="background"><rect fill="url(#paint)"></rect></g><g data-layer="base-identity"></g><g data-layer="expression"></g>`),
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if err := ValidateSVG([]byte(raw)); err != nil {
				t.Fatalf("test SVG should be generically safe: %v", err)
			}
			if err := ValidateSVGForStylePack([]byte(raw), pack); !errors.Is(err, ErrInvalidSVG) {
				t.Fatalf("ValidateSVGForStylePack() error = %v, want ErrInvalidSVG", err)
			}
		})
	}
}

func TestSVGForStylePackRejectsTransparentRequiredLayers(t *testing.T) {
	pack := BuiltInFlatVectorStylePack()
	otherRequiredLayers := `<g data-layer="base-identity"><circle r="1" fill="#E9C46A"></circle></g><g data-layer="expression"><path d="M0 0 L1 1" fill="none" stroke="#203247"></path></g>`
	tests := map[string]string{
		"root opacity zero": styleCompliantSVGWithRootAttributes("512", "512", "0 0 512 512", `opacity="0"`,
			`<g data-layer="background"><rect width="1" height="1" fill="#DCEAF5"></rect></g>`+otherRequiredLayers),
		"layer opacity zero": styleCompliantSVG("512", "512", "0 0 512 512",
			`<g data-layer="background" opacity="0"><rect width="1" height="1" fill="#DCEAF5"></rect></g>`+otherRequiredLayers),
		"fill opacity zero": styleCompliantSVG("512", "512", "0 0 512 512",
			`<g data-layer="background"><rect width="1" height="1" fill="#DCEAF5" fill-opacity="0"></rect></g>`+otherRequiredLayers),
		"stroke opacity zero": styleCompliantSVG("512", "512", "0 0 512 512",
			`<g data-layer="background"><rect width="1" height="1" fill="#DCEAF5"></rect></g><g data-layer="base-identity"><circle r="1" fill="#E9C46A"></circle></g><g data-layer="expression"><path d="M0 0 L1 1" fill="none" stroke="#203247" stroke-opacity="0"></path></g>`),
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if err := ValidateSVG([]byte(raw)); err != nil {
				t.Fatalf("test SVG should be generically safe: %v", err)
			}
			if err := ValidateSVGForStylePack([]byte(raw), pack); !errors.Is(err, ErrInvalidSVG) {
				t.Fatalf("ValidateSVGForStylePack() error = %v, want ErrInvalidSVG", err)
			}
		})
	}
}

func TestSVGForStylePackAccountsForPaletteAlpha(t *testing.T) {
	pack := BuiltInFlatVectorStylePack()
	pack.Palette = append(pack.Palette, ColorSpec{Name: "transparent-sky", Hex: "#DCEAF500"})
	raw := styleCompliantSVG("512", "512", "0 0 512 512",
		`<g data-layer="background"><rect width="1" height="1" fill="#DCEAF500"></rect></g>`,
		`<g data-layer="base-identity"><circle r="1" fill="#E9C46A"></circle></g>`,
		`<g data-layer="expression"><circle r="1" fill="#203247"></circle></g>`,
	)
	if err := ValidateSVG(rawBytes(raw)); err != nil {
		t.Fatalf("test SVG should be generically safe: %v", err)
	}
	if err := ValidateSVGForStylePack([]byte(raw), pack); !errors.Is(err, ErrInvalidSVG) {
		t.Fatalf("transparent 8-digit palette paint error = %v, want ErrInvalidSVG", err)
	}
}

func TestSVGForStylePackRequiresVisibleGradientStop(t *testing.T) {
	pack := BuiltInFlatVectorStylePack()
	pack.Grammar.AllowGradients = true
	pack.Palette = append(pack.Palette, ColorSpec{Name: "transparent-sky", Hex: "#DCEAF500"})
	styleLayers := func(expression string) string {
		return `<g data-layer="background"><rect width="1" height="1" fill="#DCEAF5"></rect></g><g data-layer="base-identity"><circle r="1" fill="#E9C46A"></circle></g>` + expression
	}
	tests := map[string]string{
		"zero stop opacity": styleCompliantSVG("512", "512", "0 0 512 512",
			`<defs><linearGradient id="paint"><stop offset="0%" stop-color="#DCEAF5" stop-opacity="0"></stop><stop offset="100%" stop-color="#2A9D8F" stop-opacity="0"></stop></linearGradient></defs>`+
				styleLayers(`<g data-layer="expression"><circle r="1" fill="url(#paint)"></circle></g>`)),
		"transparent stop color": styleCompliantSVG("512", "512", "0 0 512 512",
			`<defs><linearGradient id="paint"><stop offset="0%" stop-color="#DCEAF500"></stop></linearGradient></defs>`+
				styleLayers(`<g data-layer="expression"><circle r="1" fill="url(#paint)"></circle></g>`)),
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if err := ValidateSVG([]byte(raw)); err != nil {
				t.Fatalf("test SVG should be generically safe: %v", err)
			}
			if err := ValidateSVGForStylePack([]byte(raw), pack); !errors.Is(err, ErrInvalidSVG) {
				t.Fatalf("ValidateSVGForStylePack() error = %v, want ErrInvalidSVG", err)
			}
		})
	}

	valid := styleCompliantSVG("512", "512", "0 0 512 512",
		`<defs><linearGradient id="paint"><stop offset="0%" stop-color="#DCEAF5" stop-opacity="0"></stop><stop offset="100%" stop-color="#2A9D8F" stop-opacity="0.5"></stop></linearGradient></defs>`+
			styleLayers(`<g data-layer="expression"><circle r="1" fill="url(#paint)"></circle></g>`))
	if err := ValidateSVGForStylePack([]byte(valid), pack); err != nil {
		t.Fatalf("gradient with a visible stop was rejected: %v", err)
	}
}

func styleCompliantSVG(width, height, viewBox string, content ...string) string {
	return styleCompliantSVGWithRootAttributes(width, height, viewBox, "", content...)
}

func styleCompliantSVGWithRootAttributes(width, height, viewBox, attributes string, content ...string) string {
	if attributes != "" {
		attributes = " " + attributes
	}
	return fmt.Sprintf(`<svg xmlns="%s" viewBox="%s" width="%s" height="%s"%s>%s</svg>`,
		svgNamespace, viewBox, width, height, attributes, strings.Join(content, ""))
}

func rawBytes(value string) []byte { return []byte(value) }
