package avatar

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"image"
	"io"
	"math"
	"strings"

	"github.com/fyne-io/oksvg"
	"github.com/srwiley/rasterx"
)

const (
	// PerceptualRenderSize is deliberately small and fixed. Together with the
	// SVG byte, depth, element, and attribute limits, at most five renders consume
	// a bounded amount of CPU and less than one MiB of image storage per check.
	PerceptualRenderSize = 96

	// PerceptualPixelDeltaLimit ignores smaller antialiasing noise per pixel.
	PerceptualPixelDeltaLimit = 0.12
	// PerceptualWholeChangedRatioLimit bounds materially changed portrait pixels.
	PerceptualWholeChangedRatioLimit = 0.42
	// PerceptualWholeMeanDeltaLimit catches broad lower-contrast repainting.
	PerceptualWholeMeanDeltaLimit = 0.20
	// PerceptualIdentityChangedRatioLimit bounds changed locked-identity pixels.
	PerceptualIdentityChangedRatioLimit = 0.46
	// PerceptualIdentityMeanDeltaLimit catches lower-contrast identity repainting.
	PerceptualIdentityMeanDeltaLimit = 0.24
	// PerceptualAddedOcclusionRatioLimit bounds newly covered locked identity.
	PerceptualAddedOcclusionRatioLimit = 0.30
)

const (
	perceptualMaskAlpha          = uint8(32)
	perceptualMinimumInfluence   = uint8(32)
	perceptualAddedInfluenceStep = uint8(64)
	perceptualMinimumMaskPixels  = 48
)

var (
	// ErrPerceptualContinuity marks a deterministic render comparison that
	// exceeds a stable continuity limit.
	ErrPerceptualContinuity = errors.New("avatar perceptual continuity violation")
	// ErrPerceptualRender marks a fail-closed canonical render failure. A
	// style-valid proposal must render safely before it can be compared.
	ErrPerceptualRender = errors.New("avatar canonical render failed")
)

// PerceptualContinuityMetrics is the bounded, model-free comparison result for
// two same-style avatar versions. Ratios and deltas are normalized to [0,1].
// Callers may record these values in local diagnostics, but they are not an
// authentication or image-similarity score.
type PerceptualContinuityMetrics struct {
	WholeChangedRatio      float64
	WholeMeanDelta         float64
	IdentityChangedRatio   float64
	IdentityMeanDelta      float64
	AddedIdentityOcclusion float64
	IdentityMaskPixels     int
}

// ValidatePerceptualContinuity rejects drastic same-style visual changes that
// structural locked-layer equality alone cannot see, such as an unlocked
// opaque shape painted over the face. It performs no model inference.
func ValidatePerceptualContinuity(parent, child []byte, pack StylePack) error {
	_, err := ComparePerceptualContinuity(parent, child, pack)
	return err
}

// ComparePerceptualContinuity builds the same durable parent fingerprint used
// across compacted history boundaries and compares the child against it. The
// full-parent and fingerprint paths are therefore behaviorally identical.
func ComparePerceptualContinuity(parent, child []byte, pack StylePack) (PerceptualContinuityMetrics, error) {
	fingerprint, err := BuildPerceptualContinuityFingerprint(parent, pack)
	if err != nil {
		return PerceptualContinuityMetrics{}, err
	}
	return ComparePerceptualContinuityFromFingerprint(fingerprint, child, pack)
}

// ValidatePerceptualContinuityFromFingerprint applies the same guard when the
// parent SVG has been compacted but its exact continuity projection remains.
func ValidatePerceptualContinuityFromFingerprint(fingerprint, child []byte, pack StylePack) error {
	_, err := ComparePerceptualContinuityFromFingerprint(fingerprint, child, pack)
	return err
}

// ComparePerceptualContinuityFromFingerprint compares a full style-valid child
// against a strictly decoded, style-bound parent continuity fingerprint.
func ComparePerceptualContinuityFromFingerprint(fingerprint, child []byte, pack StylePack) (PerceptualContinuityMetrics, error) {
	if err := ValidatePerceptualV1StylePack(pack); err != nil {
		return PerceptualContinuityMetrics{}, err
	}
	parent, err := decodePerceptualContinuityFingerprint(fingerprint)
	if err != nil {
		return PerceptualContinuityMetrics{}, err
	}
	childSVG, err := SanitizeSVGForPerceptualV1StylePack(child, pack)
	if err != nil {
		return PerceptualContinuityMetrics{}, err
	}
	if err := validatePerceptualFingerprintStyle(parent, pack); err != nil {
		return PerceptualContinuityMetrics{}, err
	}
	childFull, err := renderPerceptualAvatar(childSVG)
	if err != nil {
		return PerceptualContinuityMetrics{}, err
	}
	_, locked, _ := perceptualLayerSelections(pack)
	childLockedSVG, err := selectPerceptualLayers(childSVG, pack, locked)
	if err != nil {
		return PerceptualContinuityMetrics{}, err
	}
	childLocked, err := renderPerceptualAvatar(childLockedSVG)
	if err != nil {
		return PerceptualContinuityMetrics{}, err
	}

	metrics := perceptualMetricsFromFingerprint(parent, childFull, childLocked)
	if metrics.IdentityMaskPixels < perceptualMinimumMaskPixels {
		return metrics, fmt.Errorf("%w: locked identity projection covers %d pixels, want at least %d",
			ErrInvalidPerceptualFingerprint, metrics.IdentityMaskPixels, perceptualMinimumMaskPixels)
	}
	if metrics.WholeChangedRatio > PerceptualWholeChangedRatioLimit ||
		metrics.WholeMeanDelta > PerceptualWholeMeanDeltaLimit {
		return metrics, fmt.Errorf("%w: whole portrait changed beyond the bounded render limit", ErrPerceptualContinuity)
	}
	if metrics.IdentityChangedRatio > PerceptualIdentityChangedRatioLimit ||
		metrics.IdentityMeanDelta > PerceptualIdentityMeanDeltaLimit {
		return metrics, fmt.Errorf("%w: locked identity changed beyond the bounded render limit", ErrPerceptualContinuity)
	}
	if metrics.AddedIdentityOcclusion > PerceptualAddedOcclusionRatioLimit {
		return metrics, fmt.Errorf("%w: unlocked layers occlude too much locked identity", ErrPerceptualContinuity)
	}
	return metrics, nil
}

func perceptualLayerSelections(pack StylePack) (map[string]bool, map[string]bool, map[string]bool) {
	identity := map[string]bool{}
	locked := map[string]bool{}
	unlocked := map[string]bool{}
	for _, layer := range pack.Layers {
		if layer.LockedByDefault {
			locked[layer.Name] = true
			if !perceptualBackgroundLayer(layer.Name, pack.Canvas.Background) {
				identity[layer.Name] = true
			}
		} else {
			unlocked[layer.Name] = true
		}
	}
	return identity, locked, unlocked
}

func perceptualBackgroundLayer(name, canvasBackground string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	canvasBackground = strings.ToLower(strings.TrimSpace(canvasBackground))
	return name == "background" || name == "backdrop" || name == "canvas" ||
		name == canvasBackground || strings.HasPrefix(name, "background-") ||
		strings.HasSuffix(name, "-background")
}

// selectPerceptualLayers preserves the safe canonical document while setting
// every unselected declared layer to inherited opacity zero. Definitions and
// root metadata stay available to the renderer, and the style validator has
// already guaranteed that visible geometry belongs to exactly one layer.
func selectPerceptualLayers(sanitized []byte, pack StylePack, selected map[string]bool) ([]byte, error) {
	declared := map[string]bool{}
	for _, layer := range pack.Layers {
		declared[layer.Name] = true
	}
	decoder := xml.NewDecoder(bytes.NewReader(sanitized))
	var output bytes.Buffer
	encoder := xml.NewEncoder(&output)
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%w: filter canonical SVG: %v", ErrPerceptualRender, err)
		}
		switch value := token.(type) {
		case xml.StartElement:
			start := xml.StartElement{Name: xml.Name{Local: value.Name.Local}}
			layerName := ""
			for _, attribute := range value.Attr {
				if attribute.Name.Local == "data-layer" {
					layerName = attribute.Value
				}
				start.Attr = append(start.Attr, xml.Attr{
					Name: xml.Name{Local: attribute.Name.Local}, Value: attribute.Value,
				})
			}
			if declared[layerName] && !selected[layerName] {
				replaced := false
				for index := range start.Attr {
					if start.Attr[index].Name.Local == "opacity" {
						start.Attr[index].Value = "0"
						replaced = true
					}
				}
				if !replaced {
					start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "opacity"}, Value: "0"})
				}
			}
			if err := encoder.EncodeToken(start); err != nil {
				return nil, fmt.Errorf("%w: encode filtered SVG: %v", ErrPerceptualRender, err)
			}
		case xml.EndElement:
			if err := encoder.EncodeToken(xml.EndElement{Name: xml.Name{Local: value.Name.Local}}); err != nil {
				return nil, fmt.Errorf("%w: encode filtered SVG: %v", ErrPerceptualRender, err)
			}
		case xml.CharData:
			if err := encoder.EncodeToken(xml.CharData(append([]byte(nil), value...))); err != nil {
				return nil, fmt.Errorf("%w: encode filtered SVG: %v", ErrPerceptualRender, err)
			}
		}
	}
	if err := encoder.Flush(); err != nil {
		return nil, fmt.Errorf("%w: flush filtered SVG: %v", ErrPerceptualRender, err)
	}
	return output.Bytes(), nil
}

func renderPerceptualAvatar(svg []byte) (imageOut *image.RGBA, err error) {
	defer func() {
		if recover() != nil {
			imageOut = nil
			err = fmt.Errorf("%w: renderer rejected canonical SVG", ErrPerceptualRender)
		}
	}()
	icon, err := oksvg.ReadIconStream(bytes.NewReader(svg), oksvg.StrictErrorMode)
	if err != nil {
		return nil, fmt.Errorf("%w: parse canonical SVG: %v", ErrPerceptualRender, err)
	}
	if icon.ViewBox.W <= 0 || icon.ViewBox.H <= 0 ||
		math.IsInf(icon.ViewBox.X, 0) || math.IsInf(icon.ViewBox.Y, 0) ||
		math.IsInf(icon.ViewBox.W, 0) || math.IsInf(icon.ViewBox.H, 0) ||
		math.IsNaN(icon.ViewBox.X) || math.IsNaN(icon.ViewBox.Y) ||
		math.IsNaN(icon.ViewBox.W) || math.IsNaN(icon.ViewBox.H) {
		return nil, fmt.Errorf("%w: unsupported canonical view box", ErrPerceptualRender)
	}
	imageOut = image.NewRGBA(image.Rect(0, 0, PerceptualRenderSize, PerceptualRenderSize))
	scanner := rasterx.NewScannerGV(PerceptualRenderSize, PerceptualRenderSize, imageOut, imageOut.Bounds())
	raster := rasterx.NewDasher(PerceptualRenderSize, PerceptualRenderSize, scanner)
	icon.SetTarget(0, 0, PerceptualRenderSize, PerceptualRenderSize)
	icon.Draw(raster, 1)
	return imageOut, nil
}

func countPerceptualMaskPixels(mask *image.RGBA) int {
	count := 0
	for y := 0; y < PerceptualRenderSize; y++ {
		for x := 0; x < PerceptualRenderSize; x++ {
			if mask.RGBAAt(x, y).A >= perceptualMaskAlpha {
				count++
			}
		}
	}
	return count
}
