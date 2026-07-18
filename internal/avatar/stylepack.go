package avatar

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const (
	// DefaultStylePackID is the stable identity of the built-in team portrait
	// system. Versions are immutable once published.
	DefaultStylePackID = "witself-flat-portrait"
	// BuiltInStylePackVersion is the current built-in portrait-system version.
	BuiltInStylePackVersion = 1
	// MaxStylePackJSONBytes stays below the Postgres JSONB text constraint with
	// room for JSONB's normalized separator whitespace. This makes every
	// domain-valid style persistable instead of surfacing a database 500 after
	// otherwise successful validation.
	MaxStylePackJSONBytes = 60 * 1024
	// MaxStylePackIDBytes mirrors the persisted avatar_style_packs ID bound.
	MaxStylePackIDBytes = 128
)

var (
	// ErrInvalidStylePack marks malformed style-pack metadata.
	ErrInvalidStylePack    = errors.New("invalid avatar style pack")
	styleIdentifierPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[-_][a-z0-9]+)*$`)
	hexColorPattern        = regexp.MustCompile(`^#[0-9A-Fa-f]{6}(?:[0-9A-Fa-f]{2})?$`)
)

// StylePackRef binds an immutable style-pack version to a realm. Persisted
// profiles use this reference rather than silently following future versions.
type StylePackRef struct {
	RealmID     string `json:"realm_id"`
	StylePackID string `json:"style_pack_id"`
	Version     int    `json:"version"`
}

// Validate checks a realm-scoped style-pack reference.
func (r StylePackRef) Validate() error {
	if !styleIdentifierPattern.MatchString(r.RealmID) {
		return fmt.Errorf("%w: invalid realm ID %q", ErrInvalidStylePack, r.RealmID)
	}
	if !validStylePackID(r.StylePackID) {
		return fmt.Errorf("%w: invalid style-pack ID %q", ErrInvalidStylePack, r.StylePackID)
	}
	if r.Version < 1 {
		return fmt.Errorf("%w: version must be positive", ErrInvalidStylePack)
	}
	return nil
}

// CanvasSpec defines the consistent framing shared by a style pack.
type CanvasSpec struct {
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	ViewBox    string `json:"view_box"`
	Crop       string `json:"crop"`
	Background string `json:"background"`
}

// ColorSpec is a named palette entry.
type ColorSpec struct {
	Name string `json:"name"`
	Hex  string `json:"hex"`
}

// LayerSpec defines an interoperable named SVG layer.
type LayerSpec struct {
	Name            string `json:"name"`
	Required        bool   `json:"required"`
	LockedByDefault bool   `json:"locked_by_default"`
}

// VisualGrammar captures model-neutral art-direction metadata.
type VisualGrammar struct {
	Composition      string  `json:"composition"`
	ShapeLanguage    string  `json:"shape_language"`
	Shading          string  `json:"shading"`
	OutlineWidth     float64 `json:"outline_width"`
	ExpressionRange  string  `json:"expression_range"`
	ComplexityBudget string  `json:"complexity_budget"`
	MaxElements      int     `json:"max_elements"`
	AllowGradients   bool    `json:"allow_gradients"`
}

// StyleReference is a safe canonical example for one subject form. SVG is
// stored as a compile-time string and accompanied by its SHA-256 digest so a
// persisted copy can verify provenance.
type StyleReference struct {
	ID          string      `json:"id"`
	SubjectForm SubjectForm `json:"subject_form"`
	Description string      `json:"description"`
	SVG         string      `json:"svg"`
	SHA256      string      `json:"sha256"`
}

// StylePack is immutable art-direction metadata. Realm persistence and active
// selection live outside this model-free package.
type StylePack struct {
	ID                    string           `json:"id"`
	Version               int              `json:"version"`
	Name                  string           `json:"name"`
	Description           string           `json:"description"`
	Canvas                CanvasSpec       `json:"canvas"`
	Palette               []ColorSpec      `json:"palette"`
	Grammar               VisualGrammar    `json:"grammar"`
	Layers                []LayerSpec      `json:"layers"`
	SupportedSubjectForms []SubjectForm    `json:"supported_subject_forms"`
	References            []StyleReference `json:"references"`
}

// Validate checks metadata, reference integrity, and reference SVG safety.
func (p StylePack) Validate() error {
	if !validStylePackID(p.ID) {
		return fmt.Errorf("%w: invalid ID %q", ErrInvalidStylePack, p.ID)
	}
	if p.Version < 1 {
		return fmt.Errorf("%w: version must be positive", ErrInvalidStylePack)
	}
	if p.Name == "" || len(p.Name) > 128 || !displaySafeText(p.Name) {
		return fmt.Errorf("%w: name must contain 1-128 bytes", ErrInvalidStylePack)
	}
	if err := ValidateDescription(p.Description); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidStylePack, err)
	}
	if p.Canvas.Width < 1 || p.Canvas.Width > 2048 || p.Canvas.Height < 1 || p.Canvas.Height > 2048 ||
		!validViewBox(p.Canvas.ViewBox) || !styleIdentifierPattern.MatchString(p.Canvas.Crop) ||
		!styleIdentifierPattern.MatchString(p.Canvas.Background) {
		return fmt.Errorf("%w: incomplete or out-of-range canvas", ErrInvalidStylePack)
	}
	if len(p.Palette) == 0 || len(p.Palette) > 32 {
		return fmt.Errorf("%w: palette must contain 1-32 colors", ErrInvalidStylePack)
	}
	seenNames := map[string]bool{}
	for _, color := range p.Palette {
		if !styleIdentifierPattern.MatchString(color.Name) || !hexColorPattern.MatchString(color.Hex) || seenNames[color.Name] {
			return fmt.Errorf("%w: invalid or duplicate palette entry %q", ErrInvalidStylePack, color.Name)
		}
		seenNames[color.Name] = true
	}
	if !validStyleMetadata(p.Grammar.Composition) || !validStyleMetadata(p.Grammar.ShapeLanguage) ||
		!validStyleMetadata(p.Grammar.Shading) ||
		p.Grammar.OutlineWidth <= 0 || p.Grammar.OutlineWidth > 32 || p.Grammar.ExpressionRange == "" ||
		!validStyleMetadata(p.Grammar.ExpressionRange) || !validStyleMetadata(p.Grammar.ComplexityBudget) ||
		p.Grammar.MaxElements < 1 || p.Grammar.MaxElements > maxSVGElements {
		return fmt.Errorf("%w: incomplete or out-of-range visual grammar", ErrInvalidStylePack)
	}
	if len(p.Layers) == 0 || len(p.Layers) > 32 {
		return fmt.Errorf("%w: layers must contain 1-32 entries", ErrInvalidStylePack)
	}
	seenNames = map[string]bool{}
	for _, layer := range p.Layers {
		if !styleIdentifierPattern.MatchString(layer.Name) || seenNames[layer.Name] {
			return fmt.Errorf("%w: invalid or duplicate layer %q", ErrInvalidStylePack, layer.Name)
		}
		seenNames[layer.Name] = true
	}
	if len(p.SupportedSubjectForms) == 0 || len(p.SupportedSubjectForms) > len(subjectForms) {
		return fmt.Errorf("%w: invalid supported subject-form count", ErrInvalidStylePack)
	}
	seenForms := map[SubjectForm]bool{}
	for _, form := range p.SupportedSubjectForms {
		if err := form.Validate(); err != nil || seenForms[form] {
			return fmt.Errorf("%w: invalid or duplicate subject form %q", ErrInvalidStylePack, form)
		}
		seenForms[form] = true
	}
	if len(p.References) == 0 || len(p.References) > len(subjectForms) {
		return fmt.Errorf("%w: invalid reference count", ErrInvalidStylePack)
	}
	seenNames = map[string]bool{}
	seenReferenceForms := map[SubjectForm]bool{}
	for _, reference := range p.References {
		if !styleIdentifierPattern.MatchString(reference.ID) || seenNames[reference.ID] {
			return fmt.Errorf("%w: invalid or duplicate reference ID %q", ErrInvalidStylePack, reference.ID)
		}
		if !seenForms[reference.SubjectForm] || seenReferenceForms[reference.SubjectForm] {
			return fmt.Errorf("%w: unsupported or duplicate reference subject form %q", ErrInvalidStylePack, reference.SubjectForm)
		}
		if err := ValidateDescription(reference.Description); err != nil {
			return fmt.Errorf("%w: reference %q: %v", ErrInvalidStylePack, reference.ID, err)
		}
		sanitized, err := SanitizeSVG([]byte(reference.SVG))
		if err != nil {
			return fmt.Errorf("%w: reference %q: %v", ErrInvalidStylePack, reference.ID, err)
		}
		if err := validateStylePackSVG(sanitized, p); err != nil {
			return fmt.Errorf("%w: reference %q: %v", ErrInvalidStylePack, reference.ID, err)
		}
		digest := sha256.Sum256([]byte(reference.SVG))
		if reference.SHA256 != hex.EncodeToString(digest[:]) {
			return fmt.Errorf("%w: reference %q digest mismatch", ErrInvalidStylePack, reference.ID)
		}
		seenNames[reference.ID] = true
		seenReferenceForms[reference.SubjectForm] = true
	}
	for _, required := range []SubjectForm{SubjectHuman, SubjectAnimal, SubjectInsect} {
		if !seenReferenceForms[required] {
			return fmt.Errorf("%w: missing %s reference", ErrInvalidStylePack, required)
		}
	}
	encoded, err := json.Marshal(p)
	if err != nil || len(encoded) > MaxStylePackJSONBytes {
		return fmt.Errorf("%w: encoded style pack exceeds %d bytes", ErrInvalidStylePack, MaxStylePackJSONBytes)
	}
	return nil
}

func validStylePackID(value string) bool {
	return len(value) <= MaxStylePackIDBytes && styleIdentifierPattern.MatchString(value)
}

// BuiltInFlatVectorStylePack returns a fresh copy of the built-in v1 metadata.
// Callers may modify the returned slices without changing future results.
func BuiltInFlatVectorStylePack() StylePack {
	pack := StylePack{
		ID:          DefaultStylePackID,
		Version:     BuiltInStylePackVersion,
		Name:        "Witself Flat Portrait",
		Description: "A friendly flat-vector head-and-shoulders portrait system that keeps human, animal, insect, and imaginative agent identities visually recognizable as one team.",
		Canvas: CanvasSpec{
			Width: 512, Height: 512, ViewBox: "0 0 512 512",
			Crop: "head-and-shoulders", Background: "circular-medallion",
		},
		Palette: []ColorSpec{
			{Name: "ink", Hex: "#203247"},
			{Name: "paper", Hex: "#F7FAFC"},
			{Name: "sky", Hex: "#DCEAF5"},
			{Name: "lavender", Hex: "#E5E1FA"},
			{Name: "blush", Hex: "#FBE5DD"},
			{Name: "mint", Hex: "#E1F2ED"},
			{Name: "teal", Hex: "#2A9D8F"},
			{Name: "gold", Hex: "#E9C46A"},
			{Name: "ochre", Hex: "#D49A32"},
			{Name: "coral", Hex: "#E76F51"},
			{Name: "violet", Hex: "#7868E6"},
			{Name: "sand", Hex: "#F0C8A0"},
			{Name: "umber", Hex: "#C89B7B"},
			{Name: "earth", Hex: "#9B6B4C"},
		},
		Grammar: VisualGrammar{
			Composition:      "centered head-and-shoulders portrait inside a circular team medallion",
			ShapeLanguage:    "rounded geometric silhouettes with clear facial landmarks",
			Shading:          "flat fills with at most one secondary shadow shape",
			OutlineWidth:     8,
			ExpressionRange:  "calm, curious, warm, focused, or quietly playful",
			ComplexityBudget: "no more than 120 visible vector elements and 64 KiB of sanitized SVG",
			MaxElements:      120,
			AllowGradients:   false,
		},
		Layers: []LayerSpec{
			{Name: "background", Required: true, LockedByDefault: true},
			{Name: "base-identity", Required: true, LockedByDefault: true},
			{Name: "attire", Required: false, LockedByDefault: false},
			{Name: "expression", Required: true, LockedByDefault: false},
			{Name: "experience", Required: false, LockedByDefault: false},
		},
		SupportedSubjectForms: SubjectForms(),
	}
	pack.References = []StyleReference{
		newStyleReference("human-reference", SubjectHuman, "A centered human teammate with a calm expression, geometric hair, and the shared navy outline and medallion framing.", humanReferenceSVG),
		newStyleReference("animal-reference", SubjectAnimal, "A centered fox teammate using the same crop, outline, facial proportions, and medallion framing as the human portrait.", animalReferenceSVG),
		newStyleReference("insect-reference", SubjectInsect, "A centered bee teammate with readable antennae and compound-eye cues rendered through the same friendly portrait grammar.", insectReferenceSVG),
	}
	return pack
}

func newStyleReference(id string, form SubjectForm, description, svg string) StyleReference {
	digest := sha256.Sum256([]byte(svg))
	return StyleReference{
		ID: id, SubjectForm: form, Description: description, SVG: svg,
		SHA256: hex.EncodeToString(digest[:]),
	}
}

// HasSubjectForm reports whether a pack supports form.
func (p StylePack) HasSubjectForm(form SubjectForm) bool {
	return slices.Contains(p.SupportedSubjectForms, form)
}

func validStyleMetadata(value string) bool {
	return value != "" && len(value) <= 1024 && displaySafeText(value)
}

func validViewBox(value string) bool {
	fields := strings.Fields(strings.ReplaceAll(value, ",", " "))
	if len(fields) != 4 {
		return false
	}
	for index, field := range fields {
		if !svgNumberPattern.MatchString(field) || strings.HasSuffix(field, "%") {
			return false
		}
		number, err := strconv.ParseFloat(field, 64)
		if err != nil || (index >= 2 && number <= 0) {
			return false
		}
	}
	return true
}

const humanReferenceSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 512 512" width="512" height="512" role="img" aria-label="Flat vector human teammate portrait"><title>Human teammate reference</title><g id="background" data-layer="background"><rect x="0" y="0" width="512" height="512" fill="#F7FAFC"></rect><circle cx="256" cy="256" r="220" fill="#DCEAF5"></circle></g><g id="base-identity" data-layer="base-identity"><path d="M112 472 C124 374 176 334 256 334 C336 334 388 374 400 472 Z" fill="#2A9D8F" stroke="#203247" stroke-width="8" stroke-linejoin="round"></path><circle cx="256" cy="230" r="116" fill="#E9C46A" stroke="#203247" stroke-width="8"></circle><path d="M148 218 C150 126 202 102 260 106 C330 108 368 160 362 224 C332 184 292 164 246 164 C204 164 172 182 148 218 Z" fill="#7868E6" stroke="#203247" stroke-width="8" stroke-linejoin="round"></path></g><g id="attire" data-layer="attire"><path d="M214 342 L256 382 L298 342" fill="none" stroke="#F7FAFC" stroke-width="12" stroke-linecap="round" stroke-linejoin="round"></path></g><g id="expression" data-layer="expression"><circle cx="214" cy="236" r="10" fill="#203247"></circle><circle cx="298" cy="236" r="10" fill="#203247"></circle><path d="M218 286 C240 304 272 304 294 286" fill="none" stroke="#203247" stroke-width="8" stroke-linecap="round"></path></g><g id="experience" data-layer="experience"></g></svg>`

const animalReferenceSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 512 512" width="512" height="512" role="img" aria-label="Flat vector animal teammate portrait"><title>Animal teammate reference</title><g id="background" data-layer="background"><rect x="0" y="0" width="512" height="512" fill="#F7FAFC"></rect><circle cx="256" cy="256" r="220" fill="#DCEAF5"></circle></g><g id="base-identity" data-layer="base-identity"><path d="M112 472 C124 374 176 334 256 334 C336 334 388 374 400 472 Z" fill="#2A9D8F" stroke="#203247" stroke-width="8" stroke-linejoin="round"></path><path d="M158 214 L138 92 L222 148 C246 140 266 140 290 148 L374 92 L354 214 C374 246 366 306 332 330 C290 360 222 360 180 330 C146 306 138 246 158 214 Z" fill="#E76F51" stroke="#203247" stroke-width="8" stroke-linejoin="round"></path><path d="M158 214 L150 124 L210 168" fill="#E9C46A" stroke="#203247" stroke-width="8" stroke-linejoin="round"></path><path d="M354 214 L362 124 L302 168" fill="#E9C46A" stroke="#203247" stroke-width="8" stroke-linejoin="round"></path><ellipse cx="256" cy="278" rx="52" ry="42" fill="#F7FAFC"></ellipse></g><g id="attire" data-layer="attire"><path d="M214 352 L256 390 L298 352" fill="none" stroke="#F7FAFC" stroke-width="12" stroke-linecap="round" stroke-linejoin="round"></path></g><g id="expression" data-layer="expression"><circle cx="208" cy="238" r="11" fill="#203247"></circle><circle cx="304" cy="238" r="11" fill="#203247"></circle><path d="M246 272 L256 282 L266 272 Z" fill="#203247"></path><path d="M224 300 C244 318 268 318 288 300" fill="none" stroke="#203247" stroke-width="8" stroke-linecap="round"></path></g><g id="experience" data-layer="experience"></g></svg>`

const insectReferenceSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 512 512" width="512" height="512" role="img" aria-label="Flat vector insect teammate portrait"><title>Insect teammate reference</title><g id="background" data-layer="background"><rect x="0" y="0" width="512" height="512" fill="#F7FAFC"></rect><circle cx="256" cy="256" r="220" fill="#DCEAF5"></circle></g><g id="base-identity" data-layer="base-identity"><path d="M112 472 C124 374 176 334 256 334 C336 334 388 374 400 472 Z" fill="#2A9D8F" stroke="#203247" stroke-width="8" stroke-linejoin="round"></path><path d="M218 136 C196 96 166 80 140 82" fill="none" stroke="#203247" stroke-width="8" stroke-linecap="round"></path><path d="M294 136 C316 96 346 80 372 82" fill="none" stroke="#203247" stroke-width="8" stroke-linecap="round"></path><circle cx="138" cy="82" r="12" fill="#7868E6" stroke="#203247" stroke-width="6"></circle><circle cx="374" cy="82" r="12" fill="#7868E6" stroke="#203247" stroke-width="6"></circle><circle cx="256" cy="236" r="116" fill="#E9C46A" stroke="#203247" stroke-width="8"></circle><path d="M164 210 C190 182 220 168 256 168 C292 168 322 182 348 210" fill="none" stroke="#203247" stroke-width="28"></path><path d="M166 280 C194 304 222 316 256 316 C290 316 318 304 346 280" fill="none" stroke="#203247" stroke-width="24"></path></g><g id="attire" data-layer="attire"><path d="M214 352 L256 390 L298 352" fill="none" stroke="#F7FAFC" stroke-width="12" stroke-linecap="round" stroke-linejoin="round"></path></g><g id="expression" data-layer="expression"><ellipse cx="210" cy="238" rx="18" ry="24" fill="#7868E6" stroke="#203247" stroke-width="6"></ellipse><ellipse cx="302" cy="238" rx="18" ry="24" fill="#7868E6" stroke="#203247" stroke-width="6"></ellipse><path d="M224 284 C244 302 268 302 288 284" fill="none" stroke="#203247" stroke-width="8" stroke-linecap="round"></path></g><g id="experience" data-layer="experience"></g></svg>`
