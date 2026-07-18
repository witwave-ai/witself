package avatar

import (
	"bytes"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"sort"
)

// ErrLockedLayerContinuity marks a same-style evolution that changes a layer
// the selected style pack declares locked by default, or that makes the
// locked layer depend on presentation state outside its normalized subtree.
var ErrLockedLayerContinuity = errors.New("avatar locked-layer continuity violation")

var lockedLayerAncestorPresentationAttributes = map[string]bool{
	"fill": true, "fill-rule": true, "fill-opacity": true,
	"stroke": true, "stroke-width": true, "stroke-linecap": true,
	"stroke-linejoin": true, "stroke-opacity": true,
	"opacity": true, "transform": true, "clip-path": true,
	"vector-effect": true, "preserveAspectRatio": true,
}

var forbiddenLockedLayerDependencyElements = map[string]bool{
	"defs": true, "clipPath": true, "linearGradient": true,
	"radialGradient": true, "stop": true,
}

// ValidateLockedLayerContinuity validates parent and child against the same
// immutable style pack and requires every locked-by-default layer to have the
// same normalized SVG representation, including presentation inherited from
// its ancestors. Paint-server and clipping dependencies inside locked layers
// are forbidden, so changing root/wrapper state or out-of-layer definitions
// cannot preserve source text while changing the locked portrait visually.
func ValidateLockedLayerContinuity(parent, child []byte, pack StylePack) error {
	if err := pack.Validate(); err != nil {
		return err
	}
	parentSVG, err := SanitizeSVG(parent)
	if err != nil {
		return err
	}
	if err := validateStylePackSVG(parentSVG, pack); err != nil {
		return err
	}
	childSVG, err := SanitizeSVG(child)
	if err != nil {
		return err
	}
	if err := validateStylePackSVG(childSVG, pack); err != nil {
		return err
	}
	parentLayers, err := normalizedLockedLayers(parentSVG, pack)
	if err != nil {
		return err
	}
	childLayers, err := normalizedLockedLayers(childSVG, pack)
	if err != nil {
		return err
	}
	for _, layer := range pack.Layers {
		if !layer.LockedByDefault {
			continue
		}
		if !bytes.Equal(parentLayers[layer.Name], childLayers[layer.Name]) {
			return fmt.Errorf("%w: locked layer %q changed", ErrLockedLayerContinuity, layer.Name)
		}
	}
	return nil
}

type continuityFrame struct {
	name        string
	attributes  map[string]string
	lockedLayer string
}

func normalizedLockedLayers(sanitized []byte, pack StylePack) (map[string][]byte, error) {
	locked := map[string]bool{}
	for _, layer := range pack.Layers {
		if layer.LockedByDefault {
			locked[layer.Name] = true
		}
	}
	output := map[string]*bytes.Buffer{}
	stack := []continuityFrame{}
	decoder := xml.NewDecoder(bytes.NewReader(sanitized))
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%w: inspect normalized SVG: %v", ErrLockedLayerContinuity, err)
		}
		switch value := token.(type) {
		case xml.StartElement:
			attributes := map[string]string{}
			for _, attribute := range value.Attr {
				attributes[attribute.Name.Local] = attribute.Value
			}
			lockedLayer := ""
			if len(stack) > 0 {
				lockedLayer = stack[len(stack)-1].lockedLayer
			}
			if candidate := attributes["data-layer"]; locked[candidate] {
				if lockedLayer != "" && lockedLayer != candidate {
					return nil, fmt.Errorf("%w: locked layers cannot be nested", ErrLockedLayerContinuity)
				}
				lockedLayer = candidate
				buffer := output[lockedLayer]
				if buffer == nil {
					buffer = &bytes.Buffer{}
					output[lockedLayer] = buffer
				}
				if err := writeContinuityAncestors(buffer, stack, candidate); err != nil {
					return nil, err
				}
			}
			if lockedLayer != "" {
				if forbiddenLockedLayerDependencyElements[value.Name.Local] {
					return nil, fmt.Errorf("%w: locked layer %q cannot contain <%s> dependencies", ErrLockedLayerContinuity, lockedLayer, value.Name.Local)
				}
				for _, attribute := range value.Attr {
					if attribute.Name.Local == "clip-path" ||
						((attribute.Name.Local == "fill" || attribute.Name.Local == "stroke") &&
							svgInternalURLPattern.MatchString(attribute.Value)) {
						return nil, fmt.Errorf("%w: locked layer %q cannot depend on url references", ErrLockedLayerContinuity, lockedLayer)
					}
				}
				buffer := output[lockedLayer]
				if buffer == nil {
					buffer = &bytes.Buffer{}
					output[lockedLayer] = buffer
				}
				writeContinuityStart(buffer, value)
			}
			stack = append(stack, continuityFrame{
				name: value.Name.Local, attributes: attributes, lockedLayer: lockedLayer,
			})
		case xml.EndElement:
			if len(stack) == 0 || stack[len(stack)-1].name != value.Name.Local {
				return nil, fmt.Errorf("%w: invalid SVG nesting", ErrLockedLayerContinuity)
			}
			frame := stack[len(stack)-1]
			if frame.lockedLayer != "" {
				writeContinuityField(output[frame.lockedLayer], "end")
				writeContinuityField(output[frame.lockedLayer], value.Name.Local)
			}
			stack = stack[:len(stack)-1]
		case xml.CharData:
			if len(stack) > 0 && stack[len(stack)-1].lockedLayer != "" {
				writeContinuityField(output[stack[len(stack)-1].lockedLayer], "text")
				writeContinuityField(output[stack[len(stack)-1].lockedLayer], string(value))
			}
		}
	}
	result := map[string][]byte{}
	for name, buffer := range output {
		result[name] = append([]byte(nil), buffer.Bytes()...)
	}
	return result, nil
}

func writeContinuityAncestors(buffer *bytes.Buffer, stack []continuityFrame, layerName string) error {
	for _, ancestor := range stack {
		attributes := make([]string, 0, len(ancestor.attributes))
		for name, value := range ancestor.attributes {
			if name == "clip-path" || ((name == "fill" || name == "stroke") && svgInternalURLPattern.MatchString(value)) {
				return fmt.Errorf("%w: locked layer %q has an external ancestor dependency", ErrLockedLayerContinuity, layerName)
			}
			if lockedLayerAncestorPresentationAttributes[name] {
				attributes = append(attributes, name)
			}
		}
		if len(attributes) == 0 {
			continue
		}
		sort.Strings(attributes)
		writeContinuityField(buffer, "ancestor")
		writeContinuityField(buffer, ancestor.name)
		for _, name := range attributes {
			writeContinuityField(buffer, name)
			writeContinuityField(buffer, ancestor.attributes[name])
		}
		writeContinuityField(buffer, "ancestor-end")
	}
	return nil
}

func validateLockedLayerDependencySafety(sanitized []byte, pack StylePack) error {
	locked := map[string]bool{}
	for _, layer := range pack.Layers {
		if layer.LockedByDefault {
			locked[layer.Name] = true
		}
	}
	stack := []continuityFrame{}
	decoder := xml.NewDecoder(bytes.NewReader(sanitized))
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%w: inspect locked-layer dependencies: %v", ErrInvalidSVG, err)
		}
		switch value := token.(type) {
		case xml.StartElement:
			attributes := map[string]string{}
			for _, attribute := range value.Attr {
				attributes[attribute.Name.Local] = attribute.Value
			}
			lockedLayer := ""
			if len(stack) > 0 {
				lockedLayer = stack[len(stack)-1].lockedLayer
			}
			if candidate := attributes["data-layer"]; locked[candidate] {
				for _, ancestor := range stack {
					if lockedLayerDependencyAttribute(ancestor.attributes) {
						return fmt.Errorf("%w: locked layer %q cannot depend on ancestor paint servers or clipping", ErrInvalidSVG, candidate)
					}
				}
				lockedLayer = candidate
			}
			if lockedLayer != "" {
				if forbiddenLockedLayerDependencyElements[value.Name.Local] || lockedLayerDependencyAttribute(attributes) {
					return fmt.Errorf("%w: locked layer %q cannot depend on definitions, paint servers, or clipping", ErrInvalidSVG, lockedLayer)
				}
			}
			stack = append(stack, continuityFrame{
				name: value.Name.Local, attributes: attributes, lockedLayer: lockedLayer,
			})
		case xml.EndElement:
			if len(stack) == 0 || stack[len(stack)-1].name != value.Name.Local {
				return fmt.Errorf("%w: invalid locked-layer dependency nesting", ErrInvalidSVG)
			}
			stack = stack[:len(stack)-1]
		}
	}
}

func lockedLayerDependencyAttribute(attributes map[string]string) bool {
	if attributes["clip-path"] != "" {
		return true
	}
	for _, name := range []string{"fill", "stroke"} {
		if svgInternalURLPattern.MatchString(attributes[name]) {
			return true
		}
	}
	return false
}

func writeContinuityStart(buffer *bytes.Buffer, start xml.StartElement) {
	writeContinuityField(buffer, "start")
	writeContinuityField(buffer, start.Name.Local)
	attributes := append([]xml.Attr(nil), start.Attr...)
	sort.Slice(attributes, func(i, j int) bool {
		if attributes[i].Name.Space != attributes[j].Name.Space {
			return attributes[i].Name.Space < attributes[j].Name.Space
		}
		return attributes[i].Name.Local < attributes[j].Name.Local
	})
	for _, attribute := range attributes {
		writeContinuityField(buffer, attribute.Name.Space)
		writeContinuityField(buffer, attribute.Name.Local)
		writeContinuityField(buffer, attribute.Value)
	}
	writeContinuityField(buffer, "attributes-end")
}

func writeContinuityField(buffer *bytes.Buffer, value string) {
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(value)))
	buffer.Write(length[:])
	buffer.WriteString(value)
}
