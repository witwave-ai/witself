package main

import (
	"testing"

	"github.com/muesli/termenv"
	avatardomain "github.com/witwave-ai/witself/internal/avatar"
)

func FuzzSelfCardRasterizeNeverPanics(f *testing.F) {
	pack := avatardomain.BuiltInFlatVectorStylePack()
	for _, reference := range pack.References {
		f.Add([]byte(reference.SVG))
	}
	f.Add([]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32"><circle cx="16" cy="16" r="12" fill="#203247"></circle></svg>`))
	f.Add([]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32"><path d="M0 0 C1e999 -1e999 32 32 8 8" fill="#203247" transform="scale(1e999)"></path></svg>`))
	f.Add([]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1e999 32"><circle cx="1" cy="1" r="1" fill="#203247"></circle></svg>`))
	f.Add([]byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`))

	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) == 0 || len(raw) > avatardomain.MaxSVGBytes {
			return
		}
		canonical, err := avatardomain.SanitizeSVGForPerceptualV1(raw)
		if err != nil {
			return
		}
		portrait, err := rasterizeSelfCardAvatar(canonical)
		if err != nil {
			t.Fatalf("profile-accepted SVG did not rasterize: %v", err)
		}
		if portrait == nil || portrait.Bounds().Dx() != selfCardRasterSize ||
			portrait.Bounds().Dy() != selfCardRasterSize {
			t.Fatalf("unexpected successful raster bounds: %v", portrait)
		}
		lines, err := selfCardANSIImage(portrait, termenv.TrueColor)
		if err != nil {
			t.Fatalf("render raster as terminal image: %v", err)
		}
		if len(lines) != selfCardRasterSize/2 {
			t.Fatalf("terminal image has %d lines, want %d", len(lines), selfCardRasterSize/2)
		}
	})
}

func TestRasterizeSelfCardAvatarRejectsUnboundedInput(t *testing.T) {
	if _, err := rasterizeSelfCardAvatar(nil); err == nil {
		t.Fatal("empty SVG was accepted")
	}
	if _, err := rasterizeSelfCardAvatar(make([]byte, avatardomain.MaxSVGBytes+1)); err == nil {
		t.Fatal("oversized SVG was accepted")
	}
}
