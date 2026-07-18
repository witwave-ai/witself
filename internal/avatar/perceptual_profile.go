package avatar

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"unicode"

	"github.com/fyne-io/oksvg"
	"github.com/srwiley/rasterx"
)

const (
	// PerceptualProfileV1 is the exact model-free renderer compatibility
	// profile used by continuity fingerprints and canonical local rendering.
	// It deliberately remains narrower than the released generic SVG subset.
	PerceptualProfileV1 = string(RendererProfilePerceptualV1)

	// PerceptualV1CoordinateLimit bounds source geometry before fixed-point
	// conversion in the canonical renderer.
	PerceptualV1CoordinateLimit = 8192
	// PerceptualV1StrokeWidthLimit bounds stroke expansion and join work.
	PerceptualV1StrokeWidthLimit = 64
	// PerceptualV1PathGroupLimit bounds parsed command groups independently of
	// the byte and element limits.
	PerceptualV1PathGroupLimit = 2048
	// PerceptualV1RenderWorkLimit is a deterministic upper bound on estimated
	// raster line-segment equivalents for one render. A continuity comparison
	// performs no more than five such bounded renders.
	PerceptualV1RenderWorkLimit = 50_000

	perceptualV1MinimumViewBoxExtent = 1
	perceptualV1MaximumRootExtent    = 2048
	perceptualV1MinimumArcRadius     = 1
	perceptualV1MaximumArcRatio      = 64
)

var (
	// ErrPerceptualProfile marks safe, style-valid SVG that is outside the
	// stricter renderer-compatible perceptual-v1 subset.
	ErrPerceptualProfile = errors.New("avatar SVG is incompatible with perceptual-v1")
)

type perceptualV1ViewBox struct {
	x, y, width, height float64
}

// SanitizeSVGForPerceptualV1 preserves the released generic sanitizer while
// adding the exact renderer-compatibility and deterministic-work checks used
// for local canonical rendering when no style pack is available.
func SanitizeSVGForPerceptualV1(raw []byte) ([]byte, error) {
	sanitized, err := SanitizeSVG(raw)
	if err != nil {
		return nil, err
	}
	if err := validatePerceptualV1SVG(sanitized); err != nil {
		return nil, err
	}
	return sanitized, nil
}

// SanitizeSVGForPerceptualV1StylePack additionally enforces the selected
// style pack. It does not perform the identity-mask baseline render; callers
// creating a new baseline must use SanitizePerceptualV1AvatarBaseline.
func SanitizeSVGForPerceptualV1StylePack(raw []byte, pack StylePack) ([]byte, error) {
	sanitized, err := SanitizeSVGForStylePack(raw, pack)
	if err != nil {
		return nil, err
	}
	if err := validatePerceptualV1SVG(sanitized); err != nil {
		return nil, err
	}
	return sanitized, nil
}

// SanitizePerceptualV1AvatarBaseline validates a newly persisted baseline,
// including a fail-closed locked-identity projection. Same-style child
// comparisons can use the cheaper profile sanitizer because structural
// continuity preserves the parent's locked identity.
func SanitizePerceptualV1AvatarBaseline(raw []byte, pack StylePack) ([]byte, error) {
	if err := ValidatePerceptualV1StylePack(pack); err != nil {
		return nil, err
	}
	sanitized, err := SanitizeSVGForPerceptualV1StylePack(raw, pack)
	if err != nil {
		return nil, err
	}
	if _, _, _, err := renderPerceptualParentProjection(sanitized, pack); err != nil {
		return nil, err
	}
	return sanitized, nil
}

// ValidatePerceptualV1StylePack validates references that may become new
// baselines or placeholder sources. Generic StylePack.Validate remains
// backward-compatible for already stored legacy packs.
func ValidatePerceptualV1StylePack(pack StylePack) error {
	if err := pack.Validate(); err != nil {
		return err
	}
	if pack.Grammar.AllowGradients {
		return fmt.Errorf("%w: style packs cannot enable gradients", ErrPerceptualProfile)
	}
	for _, color := range pack.Palette {
		if len(color.Hex) != len("#RRGGBB") {
			return fmt.Errorf("%w: palette color %q uses alpha hex", ErrPerceptualProfile, color.Name)
		}
	}
	for _, reference := range pack.References {
		sanitized, err := SanitizeSVGForPerceptualV1StylePack([]byte(reference.SVG), pack)
		if err == nil {
			_, _, _, err = renderPerceptualParentProjection(sanitized, pack)
		}
		if err != nil {
			return fmt.Errorf("%w: reference %q: %v", ErrPerceptualProfile, reference.ID, err)
		}
	}
	return nil
}

func validatePerceptualV1SVG(sanitized []byte) (err error) {
	defer func() {
		if recover() != nil {
			err = fmt.Errorf("%w: renderer profile parser rejected canonical SVG", ErrPerceptualProfile)
		}
	}()

	viewBox, pathGroups, err := inspectPerceptualV1SVG(sanitized)
	if err != nil {
		return err
	}
	if pathGroups > PerceptualV1PathGroupLimit {
		return fmt.Errorf("%w: path command groups exceed %d", ErrPerceptualProfile, PerceptualV1PathGroupLimit)
	}
	icon, err := oksvg.ReadIconStream(bytes.NewReader(sanitized), oksvg.StrictErrorMode)
	if err != nil {
		return fmt.Errorf("%w: canonical renderer parse failed: %v", ErrPerceptualProfile, err)
	}
	if !finitePerceptualV1Number(icon.ViewBox.X) || !finitePerceptualV1Number(icon.ViewBox.Y) ||
		!finitePerceptualV1Number(icon.ViewBox.W) || !finitePerceptualV1Number(icon.ViewBox.H) ||
		icon.ViewBox.X != viewBox.x || icon.ViewBox.Y != viewBox.y ||
		icon.ViewBox.W != viewBox.width || icon.ViewBox.H != viewBox.height {
		return fmt.Errorf("%w: canonical renderer disagrees with the declared viewBox", ErrPerceptualProfile)
	}

	scaleX := float64(PerceptualRenderSize) / viewBox.width
	scaleY := float64(PerceptualRenderSize) / viewBox.height
	totalWork := 0
	for _, svgPath := range icon.SVGPaths {
		work, workErr := estimatePerceptualV1PathWork(svgPath.Path, scaleX, scaleY)
		if workErr != nil {
			return workErr
		}
		// Conservatively account for both fill and stroke traversals. The
		// renderer skips an absent paint, but the profile never relies on that
		// implementation detail for its upper bound.
		totalWork, err = addPerceptualV1RenderWork(totalWork, work)
		if err != nil {
			return err
		}
	}
	return nil
}

func addPerceptualV1RenderWork(total, pathWork int) (int, error) {
	if total < 0 || pathWork < 0 || pathWork > (PerceptualV1RenderWorkLimit-total)/2 {
		return 0, fmt.Errorf("%w: estimated render work exceeds %d", ErrPerceptualProfile, PerceptualV1RenderWorkLimit)
	}
	return total + pathWork*2, nil
}

func inspectPerceptualV1SVG(sanitized []byte) (perceptualV1ViewBox, int, error) {
	decoder := xml.NewDecoder(bytes.NewReader(sanitized))
	var viewBox perceptualV1ViewBox
	rootSeen := false
	pathGroups := 0
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return perceptualV1ViewBox{}, 0, fmt.Errorf("%w: inspect canonical SVG: %v", ErrPerceptualProfile, err)
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		switch start.Name.Local {
		case "defs", "clipPath", "linearGradient", "radialGradient", "stop":
			return perceptualV1ViewBox{}, 0, fmt.Errorf("%w: <%s> is not supported", ErrPerceptualProfile, start.Name.Local)
		}
		attributes := make(map[string]string, len(start.Attr))
		for _, attribute := range start.Attr {
			attributes[attribute.Name.Local] = attribute.Value
			if err := validatePerceptualV1Attribute(start.Name.Local, attribute.Name.Local, attribute.Value); err != nil {
				return perceptualV1ViewBox{}, 0, err
			}
		}
		if !rootSeen {
			rootSeen = true
			if start.Name.Local != "svg" {
				return perceptualV1ViewBox{}, 0, fmt.Errorf("%w: root must be <svg>", ErrPerceptualProfile)
			}
			var parseErr error
			viewBox, parseErr = parsePerceptualV1ViewBox(attributes["viewBox"])
			if parseErr != nil {
				return perceptualV1ViewBox{}, 0, parseErr
			}
			for _, name := range []string{"width", "height"} {
				value, parseErr := parsePerceptualV1Number(attributes[name])
				if parseErr != nil || value < 1 || value > perceptualV1MaximumRootExtent {
					return perceptualV1ViewBox{}, 0, fmt.Errorf("%w: root %s must be in [1,%d]", ErrPerceptualProfile, name, perceptualV1MaximumRootExtent)
				}
			}
		}
		if data, ok := attributes["d"]; ok {
			groups, pathErr := validatePerceptualV1PathNumbers(data)
			if pathErr != nil {
				return perceptualV1ViewBox{}, 0, pathErr
			}
			if groups > PerceptualV1PathGroupLimit-pathGroups {
				return perceptualV1ViewBox{}, 0, fmt.Errorf("%w: path command groups exceed %d", ErrPerceptualProfile, PerceptualV1PathGroupLimit)
			}
			pathGroups += groups
		}
		if points, ok := attributes["points"]; ok {
			if err := validatePerceptualV1Points(points); err != nil {
				return perceptualV1ViewBox{}, 0, err
			}
		}
	}
	if !rootSeen {
		return perceptualV1ViewBox{}, 0, fmt.Errorf("%w: missing root", ErrPerceptualProfile)
	}
	return viewBox, pathGroups, nil
}

func validatePerceptualV1Attribute(element, name, value string) error {
	switch name {
	case "transform", "gradientTransform", "clip-path", "fill-rule", "vector-effect",
		"gradientUnits", "offset", "stop-color", "stop-opacity":
		return fmt.Errorf("%w: attribute %q is not supported", ErrPerceptualProfile, name)
	case "preserveAspectRatio":
		if element != "svg" || value != "xMidYMid meet" {
			return fmt.Errorf("%w: only the default preserveAspectRatio is supported", ErrPerceptualProfile)
		}
	case "opacity":
		if element == "svg" || element == "g" {
			return fmt.Errorf("%w: root and group opacity are not supported", ErrPerceptualProfile)
		}
		if _, err := parsePerceptualV1Opacity(value); err != nil {
			return err
		}
	case "fill-opacity", "stroke-opacity":
		if _, err := parsePerceptualV1Opacity(value); err != nil {
			return err
		}
	case "fill", "stroke":
		if !perceptualV1Paint(value) {
			return fmt.Errorf("%w: paint %q is not renderer-compatible", ErrPerceptualProfile, value)
		}
	case "width", "height", "r", "rx", "ry":
		number, err := parsePerceptualV1Number(value)
		if err != nil || number <= 0 || number > PerceptualV1CoordinateLimit {
			return fmt.Errorf("%w: attribute %q must be in (0,%d]", ErrPerceptualProfile, name, PerceptualV1CoordinateLimit)
		}
	case "stroke-width":
		number, err := parsePerceptualV1Number(value)
		if err != nil || number < 0 || number > PerceptualV1StrokeWidthLimit {
			return fmt.Errorf("%w: stroke-width must be in [0,%d]", ErrPerceptualProfile, PerceptualV1StrokeWidthLimit)
		}
	case "x", "y", "x1", "y1", "x2", "y2", "cx", "cy":
		number, err := parsePerceptualV1Number(value)
		if err != nil || math.Abs(number) > PerceptualV1CoordinateLimit {
			return fmt.Errorf("%w: attribute %q exceeds coordinate limit %d", ErrPerceptualProfile, name, PerceptualV1CoordinateLimit)
		}
	}
	return nil
}

func perceptualV1Paint(value string) bool {
	if len(value) == len("#RRGGBB") && hexColorPattern.MatchString(value) {
		return true
	}
	switch value {
	case "none", "black", "white":
		return true
	default:
		return false
	}
}

func parsePerceptualV1ViewBox(value string) (perceptualV1ViewBox, error) {
	parts := strings.FieldsFunc(value, func(r rune) bool { return r == ',' || unicode.IsSpace(r) })
	if len(parts) != 4 {
		return perceptualV1ViewBox{}, fmt.Errorf("%w: viewBox must contain four numbers", ErrPerceptualProfile)
	}
	values := make([]float64, len(parts))
	for index, part := range parts {
		number, err := parsePerceptualV1Number(part)
		if err != nil || math.Abs(number) > PerceptualV1CoordinateLimit {
			return perceptualV1ViewBox{}, fmt.Errorf("%w: viewBox exceeds coordinate limit", ErrPerceptualProfile)
		}
		values[index] = number
	}
	if values[0] != 0 || values[1] != 0 {
		return perceptualV1ViewBox{}, fmt.Errorf("%w: viewBox origin must be 0 0", ErrPerceptualProfile)
	}
	if values[2] < perceptualV1MinimumViewBoxExtent || values[2] > PerceptualV1CoordinateLimit ||
		values[3] < perceptualV1MinimumViewBoxExtent || values[3] > PerceptualV1CoordinateLimit ||
		values[2] != values[3] {
		return perceptualV1ViewBox{}, fmt.Errorf("%w: viewBox must be square with extent in [%d,%d]", ErrPerceptualProfile, perceptualV1MinimumViewBoxExtent, PerceptualV1CoordinateLimit)
	}
	return perceptualV1ViewBox{x: values[0], y: values[1], width: values[2], height: values[3]}, nil
}

func parsePerceptualV1Opacity(value string) (float64, error) {
	number, err := parsePerceptualV1Number(value)
	if err != nil || number < 0 || number > 1 {
		return 0, fmt.Errorf("%w: opacity must be in [0,1]", ErrPerceptualProfile)
	}
	return number, nil
}

func parsePerceptualV1Number(value string) (float64, error) {
	if value == "" || strings.HasSuffix(value, "%") {
		return 0, fmt.Errorf("%w: percentages are not supported", ErrPerceptualProfile)
	}
	number, err := strconv.ParseFloat(value, 64)
	if err != nil || !finitePerceptualV1Number(number) {
		return 0, fmt.Errorf("%w: number is not finite", ErrPerceptualProfile)
	}
	return number, nil
}

func finitePerceptualV1Number(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func validatePerceptualV1Points(data string) error {
	parts := strings.FieldsFunc(data, func(r rune) bool { return r == ',' || unicode.IsSpace(r) })
	if len(parts) == 0 || len(parts)%2 != 0 {
		return fmt.Errorf("%w: points must contain coordinate pairs", ErrPerceptualProfile)
	}
	for _, part := range parts {
		number, err := parsePerceptualV1Number(part)
		if err != nil || math.Abs(number) > PerceptualV1CoordinateLimit {
			return fmt.Errorf("%w: point exceeds coordinate limit %d", ErrPerceptualProfile, PerceptualV1CoordinateLimit)
		}
	}
	return nil
}

func validatePerceptualV1PathNumbers(data string) (int, error) {
	compact := strings.Map(func(r rune) rune {
		if r == ',' || unicode.IsSpace(r) {
			return -1
		}
		return r
	}, data)
	tokens := svgPathTokenPattern.FindAllString(data, -1)
	if len(tokens) == 0 || strings.Join(tokens, "") != compact {
		return 0, fmt.Errorf("%w: path contains invalid tokens", ErrPerceptualProfile)
	}
	isCommand := func(token string) bool {
		return len(token) == 1 && strings.ContainsRune("MmLlHhVvCcSsQqTtAaZz", rune(token[0]))
	}
	boundedPoint := func(x, y float64) error {
		if !finitePerceptualV1Number(x) || !finitePerceptualV1Number(y) ||
			math.Abs(x) > PerceptualV1CoordinateLimit || math.Abs(y) > PerceptualV1CoordinateLimit {
			return fmt.Errorf("%w: resolved path coordinate exceeds %d", ErrPerceptualProfile, PerceptualV1CoordinateLimit)
		}
		return nil
	}
	var currentX, currentY, subpathX, subpathY float64
	var priorControlX, priorControlY float64
	lastSegment := byte(0)
	haveCurrent := false
	groups := 0
	for index := 0; index < len(tokens); {
		if !isCommand(tokens[index]) {
			return 0, fmt.Errorf("%w: path numbers must follow a command", ErrPerceptualProfile)
		}
		command := tokens[index][0]
		index++
		start := index
		for index < len(tokens) && !isCommand(tokens[index]) {
			index++
		}
		values := make([]float64, index-start)
		for valueIndex, token := range tokens[start:index] {
			value, err := parsePerceptualV1Number(token)
			if err != nil || math.Abs(value) > PerceptualV1CoordinateLimit {
				return 0, fmt.Errorf("%w: path number exceeds %d", ErrPerceptualProfile, PerceptualV1CoordinateLimit)
			}
			values[valueIndex] = value
		}
		relative := command >= 'a' && command <= 'z'
		upper := command
		if relative {
			upper -= 'a' - 'A'
		}
		arity := map[byte]int{'M': 2, 'L': 2, 'H': 1, 'V': 1, 'C': 6, 'S': 4, 'Q': 4, 'T': 2, 'A': 7, 'Z': 0}[upper]
		if upper == 'Z' {
			if len(values) != 0 || !haveCurrent {
				return 0, fmt.Errorf("%w: invalid close-path command", ErrPerceptualProfile)
			}
			currentX, currentY = subpathX, subpathY
			lastSegment = 'Z'
			groups++
			continue
		}
		if arity == 0 || len(values) == 0 || len(values)%arity != 0 {
			return 0, fmt.Errorf("%w: <%c> path command has invalid arity", ErrPerceptualProfile, command)
		}
		for offset := 0; offset < len(values); offset += arity {
			groups++
			if groups > PerceptualV1PathGroupLimit {
				return 0, fmt.Errorf("%w: path command groups exceed %d", ErrPerceptualProfile, PerceptualV1PathGroupLimit)
			}
			group := values[offset : offset+arity]
			segment := upper
			if upper == 'M' && offset == 0 {
				x, y := group[0], group[1]
				if relative && haveCurrent {
					x, y = currentX+x, currentY+y
				}
				if err := boundedPoint(x, y); err != nil {
					return 0, err
				}
				currentX, currentY, subpathX, subpathY = x, y, x, y
				haveCurrent = true
				lastSegment = 'M'
				continue
			}
			if !haveCurrent {
				return 0, fmt.Errorf("%w: path must begin with a move", ErrPerceptualProfile)
			}
			if upper == 'M' {
				segment = 'L'
			}
			absolutePair := func(x, y float64) (float64, float64) {
				if relative {
					return currentX + x, currentY + y
				}
				return x, y
			}
			targetX, targetY := currentX, currentY
			switch segment {
			case 'L':
				targetX, targetY = absolutePair(group[0], group[1])
			case 'H':
				targetX = group[0]
				if relative {
					targetX += currentX
				}
			case 'V':
				targetY = group[0]
				if relative {
					targetY += currentY
				}
			case 'C':
				control1X, control1Y := absolutePair(group[0], group[1])
				control2X, control2Y := absolutePair(group[2], group[3])
				targetX, targetY = absolutePair(group[4], group[5])
				if err := boundedPoint(control1X, control1Y); err != nil {
					return 0, err
				}
				if err := boundedPoint(control2X, control2Y); err != nil {
					return 0, err
				}
				priorControlX, priorControlY = control2X, control2Y
			case 'S':
				control1X, control1Y := currentX, currentY
				if lastSegment == 'C' || lastSegment == 'S' {
					control1X, control1Y = 2*currentX-priorControlX, 2*currentY-priorControlY
				}
				control2X, control2Y := absolutePair(group[0], group[1])
				targetX, targetY = absolutePair(group[2], group[3])
				if err := boundedPoint(control1X, control1Y); err != nil {
					return 0, err
				}
				if err := boundedPoint(control2X, control2Y); err != nil {
					return 0, err
				}
				priorControlX, priorControlY = control2X, control2Y
			case 'Q':
				controlX, controlY := absolutePair(group[0], group[1])
				targetX, targetY = absolutePair(group[2], group[3])
				if err := boundedPoint(controlX, controlY); err != nil {
					return 0, err
				}
				priorControlX, priorControlY = controlX, controlY
			case 'T':
				controlX, controlY := currentX, currentY
				if lastSegment == 'Q' || lastSegment == 'T' {
					controlX, controlY = 2*currentX-priorControlX, 2*currentY-priorControlY
				}
				targetX, targetY = absolutePair(group[0], group[1])
				if err := boundedPoint(controlX, controlY); err != nil {
					return 0, err
				}
				priorControlX, priorControlY = controlX, controlY
			case 'A':
				rx, ry, rotation := group[0], group[1], group[2]
				if rx < perceptualV1MinimumArcRadius || ry < perceptualV1MinimumArcRadius ||
					rx > PerceptualV1CoordinateLimit || ry > PerceptualV1CoordinateLimit ||
					math.Abs(rotation) > 360 || (group[3] != 0 && group[3] != 1) ||
					(group[4] != 0 && group[4] != 1) ||
					math.Max(rx, ry)/math.Min(rx, ry) > perceptualV1MaximumArcRatio {
					return 0, fmt.Errorf("%w: arc parameters exceed the renderer profile", ErrPerceptualProfile)
				}
				targetX, targetY = absolutePair(group[5], group[6])
				if targetX == currentX && targetY == currentY {
					return 0, fmt.Errorf("%w: zero-span arcs are not supported", ErrPerceptualProfile)
				}
			}
			if err := boundedPoint(targetX, targetY); err != nil {
				return 0, err
			}
			currentX, currentY = targetX, targetY
			lastSegment = segment
		}
	}
	return groups, nil
}

func estimatePerceptualV1PathWork(path rasterx.Path, scaleX, scaleY float64) (int, error) {
	if !finitePerceptualV1Number(scaleX) || !finitePerceptualV1Number(scaleY) || scaleX <= 0 || scaleY <= 0 {
		return 0, fmt.Errorf("%w: invalid renderer scale", ErrPerceptualProfile)
	}
	point := func(x, y int) (float64, float64, error) {
		rawX, rawY := float64(path[x]), float64(path[y])
		if math.Abs(rawX) > PerceptualV1CoordinateLimit*64 || math.Abs(rawY) > PerceptualV1CoordinateLimit*64 {
			return 0, 0, fmt.Errorf("%w: compiled geometry exceeds coordinate limit %d", ErrPerceptualProfile, PerceptualV1CoordinateLimit)
		}
		return rawX * scaleX, rawY * scaleY, nil
	}
	var currentX, currentY, startX, startY float64
	haveCurrent := false
	work := 0
	for index := 0; index < len(path); {
		switch rasterx.PathCommand(path[index]) {
		case rasterx.PathMoveTo:
			if index+2 >= len(path) {
				return 0, fmt.Errorf("%w: truncated compiled move", ErrPerceptualProfile)
			}
			x, y, err := point(index+1, index+2)
			if err != nil {
				return 0, err
			}
			currentX, currentY, startX, startY = x, y, x, y
			haveCurrent = true
			index += 3
		case rasterx.PathLineTo:
			if !haveCurrent || index+2 >= len(path) {
				return 0, fmt.Errorf("%w: invalid compiled line", ErrPerceptualProfile)
			}
			x, y, err := point(index+1, index+2)
			if err != nil {
				return 0, err
			}
			currentX, currentY = x, y
			work++
			index += 3
		case rasterx.PathQuadTo:
			if !haveCurrent || index+4 >= len(path) {
				return 0, fmt.Errorf("%w: invalid compiled quadratic", ErrPerceptualProfile)
			}
			controlX, controlY, err := point(index+1, index+2)
			if err != nil {
				return 0, err
			}
			targetX, targetY, err := point(index+3, index+4)
			if err != nil {
				return 0, err
			}
			work += perceptualV1QuadraticSegments(currentX, currentY, controlX, controlY, targetX, targetY)
			currentX, currentY = targetX, targetY
			index += 5
		case rasterx.PathCubicTo:
			if !haveCurrent || index+6 >= len(path) {
				return 0, fmt.Errorf("%w: invalid compiled cubic", ErrPerceptualProfile)
			}
			control1X, control1Y, err := point(index+1, index+2)
			if err != nil {
				return 0, err
			}
			control2X, control2Y, err := point(index+3, index+4)
			if err != nil {
				return 0, err
			}
			targetX, targetY, err := point(index+5, index+6)
			if err != nil {
				return 0, err
			}
			work += perceptualV1CubicSegments(currentX, currentY, control1X, control1Y, control2X, control2Y, targetX, targetY)
			currentX, currentY = targetX, targetY
			index += 7
		case rasterx.PathClose:
			if !haveCurrent {
				return 0, fmt.Errorf("%w: invalid compiled close", ErrPerceptualProfile)
			}
			currentX, currentY = startX, startY
			work++
			index++
		default:
			return 0, fmt.Errorf("%w: unknown compiled path command", ErrPerceptualProfile)
		}
		if work > PerceptualV1RenderWorkLimit {
			return 0, fmt.Errorf("%w: estimated render work exceeds %d", ErrPerceptualProfile, PerceptualV1RenderWorkLimit)
		}
	}
	return work, nil
}

func perceptualV1QuadraticSegments(ax, ay, bx, by, cx, cy float64) int {
	return perceptualV1SubdivisionCount(perceptualV1DeviationSquared(ax, ay, bx, by, cx, cy))
}

func perceptualV1CubicSegments(ax, ay, bx, by, cx, cy, dx, dy float64) int {
	deviation := perceptualV1DeviationSquared(ax, ay, bx, by, dx, dy)
	if alternate := perceptualV1DeviationSquared(ax, ay, cx, cy, dx, dy); alternate > deviation {
		deviation = alternate
	}
	return perceptualV1SubdivisionCount(deviation)
}

func perceptualV1DeviationSquared(ax, ay, bx, by, cx, cy float64) float64 {
	dx := ax - 2*bx + cx
	dy := ay - 2*by + cy
	return dx*dx + dy*dy
}

func perceptualV1SubdivisionCount(deviationSquared float64) int {
	if deviationSquared < 0.333 {
		return 1
	}
	return 1 + int(math.Sqrt(math.Sqrt(3*deviationSquared)))
}
