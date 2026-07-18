package avatar

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	svgNamespace    = "http://www.w3.org/2000/svg"
	maxSVGDepth     = 32
	maxSVGElements  = 512
	maxSVGTextBytes = 512
)

var (
	// ErrInvalidSVG marks malformed, unsupported, or over-limit SVG input.
	ErrInvalidSVG = errors.New("invalid avatar SVG")
	// ErrUnsafeSVG marks SVG input with executable or externally loadable
	// content. Callers can use errors.Is to distinguish a security rejection.
	ErrUnsafeSVG = errors.New("unsafe avatar SVG")

	allowedSVGElements = map[string]bool{
		"svg": true, "g": true, "title": true, "desc": true, "defs": true,
		"clipPath": true, "linearGradient": true, "radialGradient": true,
		"stop": true, "circle": true, "ellipse": true, "rect": true,
		"path": true, "polygon": true, "polyline": true, "line": true,
	}
	unsafeSVGElements = map[string]bool{
		"script": true, "foreignobject": true, "style": true, "image": true,
		"use": true, "a": true, "iframe": true, "object": true, "embed": true,
		"audio": true, "video": true, "canvas": true, "set": true,
		"animate": true, "animatemotion": true, "animatetransform": true,
		"discard": true, "filter": true,
	}
	allowedSVGAttributes = map[string]bool{
		"xmlns": true, "id": true, "data-layer": true,
		"viewBox": true, "width": true, "height": true,
		"role": true, "aria-label": true, "preserveAspectRatio": true,
		"x": true, "y": true, "x1": true, "y1": true, "x2": true, "y2": true,
		"cx": true, "cy": true, "r": true, "rx": true, "ry": true,
		"d": true, "points": true, "fill": true, "fill-rule": true,
		"stroke": true, "stroke-width": true, "stroke-linecap": true,
		"stroke-linejoin": true, "stroke-opacity": true, "fill-opacity": true,
		"opacity": true, "transform": true, "clip-path": true,
		"gradientUnits": true, "gradientTransform": true, "offset": true,
		"stop-color": true, "stop-opacity": true, "vector-effect": true,
	}
	unsafeSVGAttributes = map[string]bool{
		"href": true, "src": true, "style": true, "content": true,
	}
	svgIDPattern          = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.:-]{0,63}$`)
	svgNumberPattern      = regexp.MustCompile(`^-?(?:[0-9]+(?:\.[0-9]+)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?%?$`)
	svgNumberListPattern  = regexp.MustCompile(`^[0-9eE+.,\-\t\n\r ]+$`)
	svgPathPattern        = regexp.MustCompile(`^[0-9eE+.,\-\t\n\r MmLlHhVvCcSsQqTtAaZz]+$`)
	svgPathTokenPattern   = regexp.MustCompile(`[MmLlHhVvCcSsQqTtAaZz]|[-+]?(?:[0-9]+(?:\.[0-9]+)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?`)
	svgTransformPattern   = regexp.MustCompile(`^(?:(?:matrix|translate|scale|rotate|skewX|skewY)[\t\n\r ]*\([0-9eE+.,\-\t\n\r ]+\)[\t\n\r ]*)+$`)
	svgInternalURLPattern = regexp.MustCompile(`^url\(#[A-Za-z][A-Za-z0-9_.:-]{0,63}\)$`)
)

// ValidateSVG checks whether raw belongs to the deliberately small, static SVG
// subset accepted for avatars. The subset contains vector geometry, gradients,
// and internal clipping, but no scripts, CSS, animation, embedded documents,
// external references, or event handlers.
func ValidateSVG(raw []byte) error {
	_, err := SanitizeSVG(raw)
	return err
}

// ValidateSVGForStylePack validates both the generic safe SVG subset and the
// selected pack's fixed canvas and named-layer contract.
func ValidateSVGForStylePack(raw []byte, pack StylePack) error {
	_, err := SanitizeSVGForStylePack(raw, pack)
	return err
}

// SanitizeSVGForStylePack sanitizes raw and then enforces style-specific
// structure. This guard is independent of autonomy policy: self-managed agents
// cannot bypass canvas or required-layer constraints.
func SanitizeSVGForStylePack(raw []byte, pack StylePack) ([]byte, error) {
	if err := pack.Validate(); err != nil {
		return nil, err
	}
	sanitized, err := SanitizeSVG(raw)
	if err != nil {
		return nil, err
	}
	if err := validateStylePackSVG(sanitized, pack); err != nil {
		return nil, err
	}
	return sanitized, nil
}

// SanitizeSVG validates and re-encodes raw into a canonical safe subset. It
// strips comments and insignificant whitespace; unsafe content is rejected,
// never merely removed.
func SanitizeSVG(raw []byte) ([]byte, error) {
	if len(raw) == 0 || len(raw) > MaxSVGBytes || !utf8.Valid(raw) {
		return nil, fmt.Errorf("%w: SVG must contain 1-%d bytes of valid UTF-8", ErrInvalidSVG, MaxSVGBytes)
	}
	decoder := xml.NewDecoder(bytes.NewReader(raw))
	decoder.Strict = true
	var output bytes.Buffer
	encoder := xml.NewEncoder(&output)
	depth := 0
	elements := 0
	rootSeen := false
	stack := []string{}
	textBytes := []int{}
	ids := map[string]bool{}
	references := []string{}

	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%w: malformed XML: %v", ErrInvalidSVG, err)
		}
		switch value := token.(type) {
		case xml.StartElement:
			elements++
			if elements > maxSVGElements {
				return nil, fmt.Errorf("%w: SVG exceeds %d elements", ErrInvalidSVG, maxSVGElements)
			}
			if depth >= maxSVGDepth {
				return nil, fmt.Errorf("%w: SVG exceeds depth %d", ErrInvalidSVG, maxSVGDepth)
			}
			name := value.Name.Local
			lowerName := strings.ToLower(name)
			if unsafeSVGElements[lowerName] {
				return nil, fmt.Errorf("%w: element <%s> is forbidden", ErrUnsafeSVG, name)
			}
			if !allowedSVGElements[name] {
				return nil, fmt.Errorf("%w: element <%s> is not in the safe subset", ErrInvalidSVG, name)
			}
			if value.Name.Space != "" && value.Name.Space != svgNamespace {
				return nil, fmt.Errorf("%w: element <%s> uses an external namespace", ErrUnsafeSVG, name)
			}
			if depth == 0 {
				if rootSeen || name != "svg" {
					return nil, fmt.Errorf("%w: root element must be <svg>", ErrInvalidSVG)
				}
				rootSeen = true
			} else if name == "svg" {
				return nil, fmt.Errorf("%w: nested <svg> elements are forbidden", ErrUnsafeSVG)
			}

			clean := xml.StartElement{Name: xml.Name{Local: name}}
			if depth == 0 {
				clean.Attr = append(clean.Attr, xml.Attr{Name: xml.Name{Local: "xmlns"}, Value: svgNamespace})
			}
			seenAttributes := map[string]bool{}
			for _, attribute := range value.Attr {
				attributeName := attribute.Name.Local
				lowerAttributeName := strings.ToLower(attributeName)
				attributeKey := attribute.Name.Space + "\x00" + attributeName
				if seenAttributes[attributeKey] {
					return nil, fmt.Errorf("%w: duplicate attribute %q", ErrInvalidSVG, attributeName)
				}
				seenAttributes[attributeKey] = true
				if attribute.Name.Space == "xmlns" || (attributeName == "xmlns" && attribute.Name.Space != "") {
					return nil, fmt.Errorf("%w: prefixed namespaces are forbidden", ErrUnsafeSVG)
				}
				if attributeName == "xmlns" {
					if depth != 0 || attribute.Value != svgNamespace {
						return nil, fmt.Errorf("%w: external namespace declaration", ErrUnsafeSVG)
					}
					continue
				}
				if attribute.Name.Space != "" {
					return nil, fmt.Errorf("%w: namespaced attribute %q is forbidden", ErrUnsafeSVG, attributeName)
				}
				if strings.HasPrefix(lowerAttributeName, "on") {
					return nil, fmt.Errorf("%w: event handler %q is forbidden", ErrUnsafeSVG, attributeName)
				}
				if unsafeSVGAttributes[lowerAttributeName] {
					return nil, fmt.Errorf("%w: attribute %q is forbidden", ErrUnsafeSVG, attributeName)
				}
				if !allowedSVGAttributes[attributeName] {
					return nil, fmt.Errorf("%w: attribute %q is not in the safe subset", ErrInvalidSVG, attributeName)
				}
				if len(attribute.Value) > 1024 {
					return nil, fmt.Errorf("%w: attribute %q is too long", ErrInvalidSVG, attributeName)
				}
				if containsUnsafeProtocol(attribute.Value) {
					return nil, fmt.Errorf("%w: attribute %q contains an unsafe protocol", ErrUnsafeSVG, attributeName)
				}
				reference, err := validateSVGAttribute(attributeName, attribute.Value)
				if err != nil {
					return nil, err
				}
				if attributeName == "id" {
					if ids[attribute.Value] {
						return nil, fmt.Errorf("%w: duplicate element ID %q", ErrInvalidSVG, attribute.Value)
					}
					ids[attribute.Value] = true
				}
				if reference != "" {
					references = append(references, reference)
				}
				clean.Attr = append(clean.Attr, xml.Attr{Name: xml.Name{Local: attributeName}, Value: attribute.Value})
			}
			if err := encoder.EncodeToken(clean); err != nil {
				return nil, fmt.Errorf("%w: encode start element: %v", ErrInvalidSVG, err)
			}
			stack = append(stack, name)
			textBytes = append(textBytes, 0)
			depth++

		case xml.EndElement:
			if depth == 0 || len(stack) == 0 {
				return nil, fmt.Errorf("%w: unexpected closing element", ErrInvalidSVG)
			}
			name := stack[len(stack)-1]
			if value.Name.Local != name {
				return nil, fmt.Errorf("%w: mismatched closing element", ErrInvalidSVG)
			}
			if err := encoder.EncodeToken(xml.EndElement{Name: xml.Name{Local: name}}); err != nil {
				return nil, fmt.Errorf("%w: encode end element: %v", ErrInvalidSVG, err)
			}
			stack = stack[:len(stack)-1]
			textBytes = textBytes[:len(textBytes)-1]
			depth--

		case xml.CharData:
			trimmed := strings.TrimSpace(string(value))
			if trimmed == "" {
				continue
			}
			if len(stack) == 0 || (stack[len(stack)-1] != "title" && stack[len(stack)-1] != "desc") {
				return nil, fmt.Errorf("%w: text is only allowed in <title> and <desc>", ErrInvalidSVG)
			}
			textBytes[len(textBytes)-1] += len(trimmed)
			if textBytes[len(textBytes)-1] > maxSVGTextBytes || !displaySafeText(trimmed) {
				return nil, fmt.Errorf("%w: title or description text is invalid", ErrInvalidSVG)
			}
			if err := encoder.EncodeToken(xml.CharData([]byte(trimmed))); err != nil {
				return nil, fmt.Errorf("%w: encode text: %v", ErrInvalidSVG, err)
			}

		case xml.Comment:
			// Comments are not executable and are intentionally omitted from the
			// canonical sanitized representation.
		case xml.Directive, xml.ProcInst:
			return nil, fmt.Errorf("%w: XML directives and processing instructions are forbidden", ErrUnsafeSVG)
		default:
			return nil, fmt.Errorf("%w: unsupported XML token %T", ErrInvalidSVG, token)
		}
	}
	if !rootSeen || depth != 0 {
		return nil, fmt.Errorf("%w: SVG document is incomplete", ErrInvalidSVG)
	}
	for _, reference := range references {
		if !ids[reference] {
			return nil, fmt.Errorf("%w: unresolved internal reference %q", ErrInvalidSVG, reference)
		}
	}
	if err := encoder.Flush(); err != nil {
		return nil, fmt.Errorf("%w: encode SVG: %v", ErrInvalidSVG, err)
	}
	if output.Len() > MaxSVGBytes {
		return nil, fmt.Errorf("%w: sanitized SVG exceeds %d bytes", ErrInvalidSVG, MaxSVGBytes)
	}
	return output.Bytes(), nil
}

func validateSVGAttribute(name, value string) (string, error) {
	invalid := func() (string, error) {
		return "", fmt.Errorf("%w: invalid value for attribute %q", ErrInvalidSVG, name)
	}
	switch name {
	case "id":
		if !svgIDPattern.MatchString(value) {
			return invalid()
		}
	case "data-layer":
		if !styleIdentifierPattern.MatchString(value) {
			return invalid()
		}
	case "viewBox", "points":
		if !svgNumberListPattern.MatchString(value) {
			return invalid()
		}
	case "width", "height", "x", "y", "x1", "y1", "x2", "y2", "cx", "cy", "r", "rx", "ry", "stroke-width", "offset":
		if !svgNumberPattern.MatchString(value) {
			return invalid()
		}
	case "opacity", "stroke-opacity", "fill-opacity", "stop-opacity":
		if !svgNumberPattern.MatchString(value) || strings.HasSuffix(value, "%") {
			return invalid()
		}
		number, err := strconv.ParseFloat(value, 64)
		if err != nil || number < 0 || number > 1 {
			return invalid()
		}
	case "d":
		if value == "" || !svgPathPattern.MatchString(value) {
			return invalid()
		}
	case "fill", "stroke", "stop-color":
		if svgInternalURLPattern.MatchString(value) {
			return internalURLID(value), nil
		}
		if !safePaint(value) {
			return invalid()
		}
	case "clip-path":
		if !svgInternalURLPattern.MatchString(value) {
			return invalid()
		}
		return internalURLID(value), nil
	case "transform", "gradientTransform":
		if !svgTransformPattern.MatchString(value) {
			return invalid()
		}
	case "role":
		if value != "img" && value != "presentation" {
			return invalid()
		}
	case "aria-label":
		if value == "" || len(value) > 256 || !displaySafeText(value) {
			return invalid()
		}
	case "preserveAspectRatio":
		switch value {
		case "none", "xMinYMin meet", "xMidYMin meet", "xMaxYMin meet", "xMinYMid meet", "xMidYMid meet", "xMaxYMid meet", "xMinYMax meet", "xMidYMax meet", "xMaxYMax meet", "xMinYMin slice", "xMidYMin slice", "xMaxYMin slice", "xMinYMid slice", "xMidYMid slice", "xMaxYMid slice", "xMinYMax slice", "xMidYMax slice", "xMaxYMax slice":
		default:
			return invalid()
		}
	case "fill-rule", "clip-rule":
		if value != "nonzero" && value != "evenodd" {
			return invalid()
		}
	case "stroke-linecap":
		if value != "butt" && value != "round" && value != "square" {
			return invalid()
		}
	case "stroke-linejoin":
		if value != "miter" && value != "round" && value != "bevel" {
			return invalid()
		}
	case "gradientUnits":
		if value != "userSpaceOnUse" && value != "objectBoundingBox" {
			return invalid()
		}
	case "vector-effect":
		if value != "non-scaling-stroke" {
			return invalid()
		}
	default:
		return invalid()
	}
	return "", nil
}

func safePaint(value string) bool {
	if hexColorPattern.MatchString(value) {
		return true
	}
	switch value {
	case "none", "currentColor", "transparent", "black", "white":
		return true
	default:
		return false
	}
}

func internalURLID(value string) string {
	return strings.TrimSuffix(strings.TrimPrefix(value, "url(#"), ")")
}

func containsUnsafeProtocol(value string) bool {
	var compact strings.Builder
	compact.Grow(len(value))
	for _, r := range strings.ToLower(value) {
		if !unicode.IsSpace(r) && !unicode.IsControl(r) {
			compact.WriteRune(r)
		}
	}
	normalized := compact.String()
	for _, forbidden := range []string{"javascript:", "vbscript:", "data:", "file:", "http:", "https:", "ftp:", "//", `\\`} {
		if strings.Contains(normalized, forbidden) {
			return true
		}
	}
	return false
}

func displaySafeText(value string) bool {
	if !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return false
		}
	}
	return true
}

func validateStylePackSVG(sanitized []byte, pack StylePack) error {
	decoder := xml.NewDecoder(bytes.NewReader(sanitized))
	rootSeen := false
	elementCount := 0
	layers := map[string]int{}
	visibleGeometryByLayer := map[string]int{}
	knownLayers := map[string]LayerSpec{}
	palette := map[string]bool{}
	idKinds := map[string]string{}
	paintReferences := map[string]string{}
	clipReferences := map[string]string{}
	gradientIDs := map[string]bool{}
	visibleGradientStops := map[string]bool{}
	type geometryVisibility struct {
		layerName      string
		opacity        float64
		fill           string
		stroke         string
		fillOpacity    float64
		strokeOpacity  float64
		strokeWidth    float64
		fillSet        bool
		strokeSet      bool
		fillArea       bool
		strokeDrawable bool
	}
	geometry := []geometryVisibility{}
	type styleContext struct {
		name          string
		layerName     string
		layerCount    int
		inDefinition  bool
		gradientID    string
		fill          string
		stroke        string
		opacity       float64
		fillOpacity   float64
		strokeOpacity float64
		strokeWidth   float64
		fillSet       bool
		strokeSet     bool
	}
	stack := []styleContext{}
	for _, layer := range pack.Layers {
		knownLayers[layer.Name] = layer
	}
	for _, color := range pack.Palette {
		palette[strings.ToLower(color.Hex)] = true
	}
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("%w: inspect sanitized SVG: %v", ErrInvalidSVG, err)
		}
		switch value := token.(type) {
		case xml.StartElement:
			start := value
			elementCount++
			if elementCount > pack.Grammar.MaxElements {
				return fmt.Errorf("%w: SVG exceeds style-pack limit of %d elements", ErrInvalidSVG, pack.Grammar.MaxElements)
			}
			if !pack.Grammar.AllowGradients && (start.Name.Local == "linearGradient" || start.Name.Local == "radialGradient") {
				return fmt.Errorf("%w: gradients are disabled by style pack", ErrInvalidSVG)
			}

			attributes := map[string]string{}
			for _, attribute := range start.Attr {
				attributes[attribute.Name.Local] = attribute.Value
			}
			if !rootSeen {
				rootSeen = true
				if attributes["viewBox"] != pack.Canvas.ViewBox ||
					attributes["width"] != strconv.Itoa(pack.Canvas.Width) ||
					attributes["height"] != strconv.Itoa(pack.Canvas.Height) {
					return fmt.Errorf("%w: root canvas must be width=%d height=%d viewBox=%q", ErrInvalidSVG, pack.Canvas.Width, pack.Canvas.Height, pack.Canvas.ViewBox)
				}
			}

			context := styleContext{
				name: start.Name.Local, opacity: 1,
				fillOpacity: 1, strokeOpacity: 1, strokeWidth: 1,
			}
			if len(stack) > 0 {
				parent := stack[len(stack)-1]
				context.layerName = parent.layerName
				context.layerCount = parent.layerCount
				context.inDefinition = parent.inDefinition
				context.gradientID = parent.gradientID
				context.fill, context.fillSet = parent.fill, parent.fillSet
				context.stroke, context.strokeSet = parent.stroke, parent.strokeSet
				context.opacity = parent.opacity
				context.fillOpacity = parent.fillOpacity
				context.strokeOpacity = parent.strokeOpacity
				context.strokeWidth = parent.strokeWidth
			}
			switch start.Name.Local {
			case "defs", "clipPath", "linearGradient", "radialGradient":
				context.inDefinition = true
			}
			if start.Name.Local == "linearGradient" || start.Name.Local == "radialGradient" {
				context.gradientID = attributes["id"]
				if context.gradientID == "" {
					return fmt.Errorf("%w: gradients must declare an ID", ErrInvalidSVG)
				}
				gradientIDs[context.gradientID] = true
			}

			for _, attribute := range start.Attr {
				switch attribute.Name.Local {
				case "id":
					idKinds[attribute.Value] = start.Name.Local
				case "fill", "stroke":
					paint := strings.ToLower(attribute.Value)
					if paint != "none" && !svgInternalURLPattern.MatchString(attribute.Value) && !palette[paint] {
						return fmt.Errorf("%w: paint %q is outside style-pack palette", ErrInvalidSVG, attribute.Value)
					}
					if svgInternalURLPattern.MatchString(attribute.Value) {
						paintReferences[internalURLID(attribute.Value)] = attribute.Name.Local
					}
					if attribute.Name.Local == "fill" {
						context.fill, context.fillSet = attribute.Value, true
					} else {
						context.stroke, context.strokeSet = attribute.Value, true
					}
				case "stop-color":
					// stop-color defaults to black and does not accept paint
					// servers. Requiring a direct palette color prevents both
					// the omitted-default and url(#non-gradient) bypasses.
					if !palette[strings.ToLower(attribute.Value)] {
						return fmt.Errorf("%w: gradient stop color %q is outside style-pack palette", ErrInvalidSVG, attribute.Value)
					}
				case "clip-path":
					clipReferences[internalURLID(attribute.Value)] = start.Name.Local
				case "opacity":
					value, err := strconv.ParseFloat(attribute.Value, 64)
					if err != nil {
						return fmt.Errorf("%w: invalid opacity", ErrInvalidSVG)
					}
					context.opacity *= value
				case "fill-opacity":
					value, err := strconv.ParseFloat(attribute.Value, 64)
					if err != nil {
						return fmt.Errorf("%w: invalid fill-opacity", ErrInvalidSVG)
					}
					context.fillOpacity = value
				case "stroke-opacity":
					value, err := strconv.ParseFloat(attribute.Value, 64)
					if err != nil {
						return fmt.Errorf("%w: invalid stroke-opacity", ErrInvalidSVG)
					}
					context.strokeOpacity = value
				case "stroke-width":
					value, err := strconv.ParseFloat(strings.TrimSuffix(attribute.Value, "%"), 64)
					if err != nil || value < 0 {
						return fmt.Errorf("%w: invalid stroke-width", ErrInvalidSVG)
					}
					context.strokeWidth = value
				case "data-layer":
					if start.Name.Local != "g" {
						return fmt.Errorf("%w: data-layer %q must be on a <g> element", ErrInvalidSVG, attribute.Value)
					}
					if _, ok := knownLayers[attribute.Value]; !ok {
						return fmt.Errorf("%w: layer %q is not declared by style pack", ErrInvalidSVG, attribute.Value)
					}
					layers[attribute.Value]++
					if layers[attribute.Value] > 1 {
						return fmt.Errorf("%w: layer %q occurs more than once", ErrInvalidSVG, attribute.Value)
					}
					if context.layerCount == 0 {
						context.layerName = attribute.Value
					}
					context.layerCount++
				}
			}

			if start.Name.Local == "stop" {
				if _, ok := attributes["stop-color"]; !ok {
					return fmt.Errorf("%w: gradient stops must declare stop-color", ErrInvalidSVG)
				}
				if context.gradientID == "" {
					return fmt.Errorf("%w: gradient stops must belong to an identified gradient", ErrInvalidSVG)
				}
				stopOpacity := 1.0
				if rawOpacity, ok := attributes["stop-opacity"]; ok {
					value, err := strconv.ParseFloat(rawOpacity, 64)
					if err != nil {
						return fmt.Errorf("%w: invalid stop-opacity", ErrInvalidSVG)
					}
					stopOpacity = value
				}
				if stopOpacity > 0 && stylePaintAlpha(attributes["stop-color"]) > 0 {
					visibleGradientStops[context.gradientID] = true
				}
			}
			properties := svgGeometryProperties{}
			if isVisibleSVGGeometry(start.Name.Local) {
				var err error
				properties, err = validateNondegenerateSVGGeometry(start.Name.Local, attributes)
				if err != nil {
					return err
				}
			}
			if isVisibleSVGGeometry(start.Name.Local) && !context.inDefinition {
				if context.layerCount != 1 {
					return fmt.Errorf("%w: visible <%s> geometry must be under exactly one declared layer", ErrInvalidSVG, start.Name.Local)
				}
				if isFillableSVGGeometry(start.Name.Local) && !context.fillSet {
					return fmt.Errorf("%w: visible <%s> geometry must declare or inherit fill", ErrInvalidSVG, start.Name.Local)
				}
				if start.Name.Local == "line" && !context.strokeSet {
					return fmt.Errorf("%w: visible <line> geometry must declare or inherit stroke", ErrInvalidSVG)
				}
				hasVisibleFill := isFillableSVGGeometry(start.Name.Local) &&
					context.fillSet && !strings.EqualFold(context.fill, "none")
				hasVisibleStroke := context.strokeSet &&
					!strings.EqualFold(context.stroke, "none")
				if !hasVisibleFill && !hasVisibleStroke {
					return fmt.Errorf("%w: <%s> geometry has no visible fill or stroke", ErrInvalidSVG, start.Name.Local)
				}
				geometry = append(geometry, geometryVisibility{
					layerName: context.layerName, opacity: context.opacity,
					fill: context.fill, stroke: context.stroke,
					fillOpacity:   context.fillOpacity,
					strokeOpacity: context.strokeOpacity,
					strokeWidth:   context.strokeWidth,
					fillSet:       context.fillSet, strokeSet: context.strokeSet,
					fillArea:       properties.fillArea,
					strokeDrawable: properties.strokeDrawable,
				})
			}
			stack = append(stack, context)

		case xml.EndElement:
			if len(stack) == 0 || stack[len(stack)-1].name != value.Name.Local {
				return fmt.Errorf("%w: invalid style element nesting", ErrInvalidSVG)
			}
			stack = stack[:len(stack)-1]
		}
	}
	for id, attribute := range paintReferences {
		kind := idKinds[id]
		if kind != "linearGradient" && kind != "radialGradient" {
			return fmt.Errorf("%w: %s paint url(#%s) must reference a gradient", ErrInvalidSVG, attribute, id)
		}
	}
	for id := range gradientIDs {
		if !visibleGradientStops[id] {
			return fmt.Errorf("%w: gradient %q has no visible stop", ErrInvalidSVG, id)
		}
	}
	for id, element := range clipReferences {
		if idKinds[id] != "clipPath" {
			return fmt.Errorf("%w: <%s> clip-path url(#%s) must reference a clipPath", ErrInvalidSVG, element, id)
		}
	}
	for _, item := range geometry {
		if item.opacity <= 0 {
			continue
		}
		fillVisible := item.fillArea && item.fillSet &&
			item.fillOpacity > 0 && stylePaintVisible(item.fill, visibleGradientStops)
		strokeVisible := item.strokeDrawable && item.strokeSet && item.strokeWidth > 0 && item.strokeOpacity > 0 &&
			stylePaintVisible(item.stroke, visibleGradientStops)
		if fillVisible || strokeVisible {
			visibleGeometryByLayer[item.layerName]++
		}
	}
	for _, layer := range pack.Layers {
		if layer.Required && layers[layer.Name] != 1 {
			return fmt.Errorf("%w: required layer %q is missing", ErrInvalidSVG, layer.Name)
		}
		if layer.Required && visibleGeometryByLayer[layer.Name] == 0 {
			return fmt.Errorf("%w: required layer %q contains no visible geometry", ErrInvalidSVG, layer.Name)
		}
	}
	if err := validateLockedLayerDependencySafety(sanitized, pack); err != nil {
		return err
	}
	return nil
}

func isVisibleSVGGeometry(name string) bool {
	switch name {
	case "circle", "ellipse", "rect", "path", "polygon", "polyline", "line":
		return true
	default:
		return false
	}
}

func isFillableSVGGeometry(name string) bool {
	return isVisibleSVGGeometry(name) && name != "line"
}

type svgGeometryProperties struct {
	fillArea       bool
	strokeDrawable bool
}

func validateNondegenerateSVGGeometry(name string, attributes map[string]string) (svgGeometryProperties, error) {
	positive := func(attribute string) (float64, error) {
		raw := strings.TrimSuffix(attributes[attribute], "%")
		if raw == "" {
			return 0, nil
		}
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return 0, fmt.Errorf("%w: <%s> has invalid %s", ErrInvalidSVG, name, attribute)
		}
		return value, nil
	}
	coordinate := func(attribute string) (float64, error) {
		return positive(attribute)
	}
	switch name {
	case "circle":
		radius, err := positive("r")
		if err != nil || radius <= 0 {
			return svgGeometryProperties{}, fmt.Errorf("%w: <circle> radius must be positive", ErrInvalidSVG)
		}
		return svgGeometryProperties{fillArea: true, strokeDrawable: true}, nil
	case "ellipse":
		rx, errX := positive("rx")
		ry, errY := positive("ry")
		if errX != nil || errY != nil || rx <= 0 || ry <= 0 {
			return svgGeometryProperties{}, fmt.Errorf("%w: <ellipse> radii must be positive", ErrInvalidSVG)
		}
		return svgGeometryProperties{fillArea: true, strokeDrawable: true}, nil
	case "rect":
		width, errWidth := positive("width")
		height, errHeight := positive("height")
		if errWidth != nil || errHeight != nil || width <= 0 || height <= 0 {
			return svgGeometryProperties{}, fmt.Errorf("%w: <rect> width and height must be positive", ErrInvalidSVG)
		}
		return svgGeometryProperties{fillArea: true, strokeDrawable: true}, nil
	case "line":
		x1, errX1 := coordinate("x1")
		y1, errY1 := coordinate("y1")
		x2, errX2 := coordinate("x2")
		y2, errY2 := coordinate("y2")
		if errX1 != nil || errY1 != nil || errX2 != nil || errY2 != nil ||
			(x1 == x2 && y1 == y2) {
			return svgGeometryProperties{}, fmt.Errorf("%w: <line> endpoints must be distinct", ErrInvalidSVG)
		}
		return svgGeometryProperties{strokeDrawable: true}, nil
	case "polygon", "polyline":
		minimum := 2
		if name == "polygon" {
			minimum = 3
		}
		parts := strings.FieldsFunc(attributes["points"], func(r rune) bool {
			return r == ',' || unicode.IsSpace(r)
		})
		if len(parts)%2 != 0 {
			return svgGeometryProperties{}, fmt.Errorf("%w: <%s> points must contain coordinate pairs", ErrInvalidSVG, name)
		}
		distinct := map[[2]float64]bool{}
		points := make([][2]float64, 0, len(parts)/2)
		for index := 0; index < len(parts); index += 2 {
			x, errX := strconv.ParseFloat(parts[index], 64)
			y, errY := strconv.ParseFloat(parts[index+1], 64)
			if errX != nil || errY != nil {
				return svgGeometryProperties{}, fmt.Errorf("%w: <%s> contains an invalid point", ErrInvalidSVG, name)
			}
			point := [2]float64{x, y}
			distinct[point] = true
			points = append(points, point)
		}
		if len(distinct) < minimum {
			return svgGeometryProperties{}, fmt.Errorf("%w: <%s> requires at least %d distinct points", ErrInvalidSVG, name, minimum)
		}
		return svgGeometryProperties{
			fillArea:       name == "polygon" && pointsAreNoncollinear(points),
			strokeDrawable: true,
		}, nil
	case "path":
		drawable, err := validatePathData(attributes["d"])
		if err != nil {
			return svgGeometryProperties{}, err
		}
		return svgGeometryProperties{strokeDrawable: drawable}, nil
	}
	return svgGeometryProperties{}, nil
}

func pointsAreNoncollinear(points [][2]float64) bool {
	if len(points) < 3 {
		return false
	}
	origin := points[0]
	for first := 1; first < len(points)-1; first++ {
		for second := first + 1; second < len(points); second++ {
			cross := (points[first][0]-origin[0])*(points[second][1]-origin[1]) -
				(points[first][1]-origin[1])*(points[second][0]-origin[0])
			if cross != 0 {
				return true
			}
		}
	}
	return false
}

func validatePathData(data string) (bool, error) {
	compact := strings.Map(func(r rune) rune {
		if r == ',' || unicode.IsSpace(r) {
			return -1
		}
		return r
	}, data)
	tokens := svgPathTokenPattern.FindAllString(data, -1)
	if len(tokens) == 0 || strings.Join(tokens, "") != compact {
		return false, fmt.Errorf("%w: <path> contains invalid tokens", ErrInvalidSVG)
	}
	isCommand := func(token string) bool {
		return len(token) == 1 && strings.ContainsRune("MmLlHhVvCcSsQqTtAaZz", rune(token[0]))
	}
	parseNumbers := func(raw []string) ([]float64, error) {
		values := make([]float64, len(raw))
		for index, token := range raw {
			value, err := strconv.ParseFloat(token, 64)
			if err != nil {
				return nil, err
			}
			values[index] = value
		}
		return values, nil
	}
	var currentX, currentY, subpathX, subpathY float64
	haveCurrent := false
	drawingCommand := false
	nonzeroSegment := false
	for index := 0; index < len(tokens); {
		if !isCommand(tokens[index]) {
			return false, fmt.Errorf("%w: <path> numbers must follow a command", ErrInvalidSVG)
		}
		command := tokens[index][0]
		index++
		start := index
		for index < len(tokens) && !isCommand(tokens[index]) {
			index++
		}
		values, err := parseNumbers(tokens[start:index])
		if err != nil {
			return false, fmt.Errorf("%w: <path> contains an invalid number", ErrInvalidSVG)
		}
		relative := command >= 'a' && command <= 'z'
		upper := command
		if relative {
			upper -= 'a' - 'A'
		}
		arity := map[byte]int{'M': 2, 'L': 2, 'H': 1, 'V': 1, 'C': 6, 'S': 4, 'Q': 4, 'T': 2, 'A': 7, 'Z': 0}[upper]
		if upper == 'Z' {
			if len(values) != 0 || !haveCurrent {
				return false, fmt.Errorf("%w: invalid close-path command", ErrInvalidSVG)
			}
			if currentX != subpathX || currentY != subpathY {
				nonzeroSegment = true
			}
			currentX, currentY = subpathX, subpathY
			continue
		}
		if arity == 0 || len(values) == 0 || len(values)%arity != 0 {
			return false, fmt.Errorf("%w: <%c> path command has invalid arity", ErrInvalidSVG, command)
		}
		for offset := 0; offset < len(values); offset += arity {
			group := values[offset : offset+arity]
			if upper == 'M' && offset == 0 {
				x, y := group[0], group[1]
				if relative && haveCurrent {
					x, y = currentX+x, currentY+y
				}
				currentX, currentY, subpathX, subpathY = x, y, x, y
				haveCurrent = true
				continue
			}
			if !haveCurrent {
				return false, fmt.Errorf("%w: <path> must begin with a move command", ErrInvalidSVG)
			}
			segmentCommand := upper
			if upper == 'M' {
				segmentCommand = 'L'
			}
			drawingCommand = true
			targetX, targetY := currentX, currentY
			switch segmentCommand {
			case 'L', 'T':
				targetX, targetY = group[0], group[1]
			case 'H':
				targetX = group[0]
			case 'V':
				targetY = group[0]
			case 'C':
				targetX, targetY = group[4], group[5]
			case 'S', 'Q':
				targetX, targetY = group[2], group[3]
			case 'A':
				if group[0] < 0 || group[1] < 0 || (group[3] != 0 && group[3] != 1) || (group[4] != 0 && group[4] != 1) {
					return false, fmt.Errorf("%w: invalid arc parameters", ErrInvalidSVG)
				}
				targetX, targetY = group[5], group[6]
			}
			if relative {
				targetX, targetY = currentX+targetX, currentY+targetY
			}
			if targetX != currentX || targetY != currentY {
				nonzeroSegment = true
			}
			currentX, currentY = targetX, targetY
		}
	}
	if !haveCurrent || !drawingCommand {
		return false, fmt.Errorf("%w: <path> must contain a drawing command beyond move or close", ErrInvalidSVG)
	}
	if !nonzeroSegment {
		return false, fmt.Errorf("%w: <path> contains only zero-length segments", ErrInvalidSVG)
	}
	return true, nil
}

func stylePaintVisible(paint string, visibleGradients map[string]bool) bool {
	if paint == "" || strings.EqualFold(paint, "none") {
		return false
	}
	if svgInternalURLPattern.MatchString(paint) {
		return visibleGradients[internalURLID(paint)]
	}
	return stylePaintAlpha(paint) > 0
}

func stylePaintAlpha(paint string) float64 {
	if !hexColorPattern.MatchString(paint) {
		return 1
	}
	if len(paint) != len("#RRGGBBAA") {
		return 1
	}
	alpha, err := strconv.ParseUint(paint[len(paint)-2:], 16, 8)
	if err != nil {
		return 0
	}
	return float64(alpha) / 255
}
