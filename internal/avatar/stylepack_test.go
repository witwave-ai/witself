package avatar

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func TestBuiltInFlatVectorStylePack(t *testing.T) {
	pack := BuiltInFlatVectorStylePack()
	if err := pack.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if pack.ID != DefaultStylePackID || pack.Version != BuiltInStylePackVersion {
		t.Fatalf("identity = %s@%d", pack.ID, pack.Version)
	}
	if pack.Canvas.Width != 512 || pack.Canvas.Height != 512 || pack.Canvas.ViewBox != "0 0 512 512" {
		t.Fatalf("canvas = %#v", pack.Canvas)
	}
	for _, form := range SubjectForms() {
		if !pack.HasSubjectForm(form) {
			t.Errorf("built-in pack does not support %q", form)
		}
	}
	wantReferences := map[SubjectForm]bool{SubjectHuman: false, SubjectAnimal: false, SubjectInsect: false}
	for _, reference := range pack.References {
		if _, ok := wantReferences[reference.SubjectForm]; ok {
			wantReferences[reference.SubjectForm] = true
		}
		if len(reference.SVG) > MaxSVGBytes {
			t.Errorf("%s SVG has %d bytes", reference.ID, len(reference.SVG))
		}
		if err := ValidateSVG([]byte(reference.SVG)); err != nil {
			t.Errorf("%s SVG: %v", reference.ID, err)
		}
	}
	for form, found := range wantReferences {
		if !found {
			t.Errorf("missing %s reference", form)
		}
	}
}

func TestBuiltInFlatVectorStylePackReturnsIndependentSlices(t *testing.T) {
	first := BuiltInFlatVectorStylePack()
	first.Palette[0].Hex = "#000000"
	first.Layers[0].Name = "changed"
	first.SupportedSubjectForms[0] = SubjectSymbolic
	first.References[0].SVG = "changed"

	second := BuiltInFlatVectorStylePack()
	if second.Palette[0].Hex == "#000000" || second.Layers[0].Name == "changed" ||
		second.SupportedSubjectForms[0] == SubjectSymbolic || second.References[0].SVG == "changed" {
		t.Fatal("built-in style-pack slices share mutable backing storage")
	}
}

func TestStylePackValidateRejectsMutations(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*StylePack)
	}{
		{"invalid ID", func(p *StylePack) { p.ID = "Not Safe" }},
		{"overlong ID", func(p *StylePack) { p.ID = "a" + strings.Repeat("b", MaxStylePackIDBytes) }},
		{"zero version", func(p *StylePack) { p.Version = 0 }},
		{"empty description", func(p *StylePack) { p.Description = "" }},
		{"oversize canvas", func(p *StylePack) { p.Canvas.Width = 4096 }},
		{"bad palette", func(p *StylePack) { p.Palette[0].Hex = "red" }},
		{"duplicate palette", func(p *StylePack) { p.Palette[1].Name = p.Palette[0].Name }},
		{"invalid grammar", func(p *StylePack) { p.Grammar.OutlineWidth = 0 }},
		{"invalid element budget", func(p *StylePack) { p.Grammar.MaxElements = 0 }},
		{"duplicate layer", func(p *StylePack) { p.Layers[1].Name = p.Layers[0].Name }},
		{"unsupported form", func(p *StylePack) { p.SupportedSubjectForms[0] = SubjectForm("ghost") }},
		{"duplicate form", func(p *StylePack) { p.SupportedSubjectForms[1] = p.SupportedSubjectForms[0] }},
		{"unsafe reference", func(p *StylePack) { p.References[0].SVG = `<svg><script></script></svg>` }},
		{"digest mismatch", func(p *StylePack) { p.References[0].SHA256 = strings.Repeat("0", 64) }},
		{"missing insect reference", func(p *StylePack) { p.References = p.References[:2] }},
		{"aggregate encoding exceeds persistence limit", func(p *StylePack) {
			p.References[0].SVG = strings.Replace(
				p.References[0].SVG, "</svg>", "<!--"+strings.Repeat("x", MaxStylePackJSONBytes)+"--></svg>", 1,
			)
			digest := sha256.Sum256([]byte(p.References[0].SVG))
			p.References[0].SHA256 = hex.EncodeToString(digest[:])
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pack := BuiltInFlatVectorStylePack()
			test.mutate(&pack)
			if err := pack.Validate(); !errors.Is(err, ErrInvalidStylePack) {
				t.Fatalf("Validate() error = %v, want ErrInvalidStylePack", err)
			}
		})
	}
}

func TestStylePackRefValidate(t *testing.T) {
	valid := StylePackRef{RealmID: "realm_default", StylePackID: DefaultStylePackID, Version: 1}
	if err := valid.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	for _, invalid := range []StylePackRef{
		{RealmID: "", StylePackID: DefaultStylePackID, Version: 1},
		{RealmID: "realm_default", StylePackID: "Not Safe", Version: 1},
		{RealmID: "realm_default", StylePackID: "a" + strings.Repeat("b", MaxStylePackIDBytes), Version: 1},
		{RealmID: "realm_default", StylePackID: DefaultStylePackID, Version: 0},
	} {
		if err := invalid.Validate(); !errors.Is(err, ErrInvalidStylePack) {
			t.Errorf("%#v error = %v, want ErrInvalidStylePack", invalid, err)
		}
	}
}
