package main

import (
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

type mcpInputSchemaPatch func(*jsonschema.Schema) error

// mcpInputSchema preserves SDK inference except for explicitly checked patches.
func mcpInputSchema[In any](patches ...mcpInputSchemaPatch) *jsonschema.Schema {
	typ := reflect.TypeFor[In]()
	schema, err := jsonschema.ForType(typ, &jsonschema.ForOptions{})
	if err != nil {
		panic(fmt.Sprintf("MCP input schema %v path <root>: %v", typ, err))
	}
	for _, patch := range patches {
		if err := patch(schema); err != nil {
			panic(fmt.Sprintf("MCP input schema %v: %v", typ, err))
		}
	}
	return schema
}

func mcpNonEmptyArray(path ...string) mcpInputSchemaPatch {
	return func(root *jsonschema.Schema) error {
		property, err := mcpInputArrayProperty(root, path)
		if err != nil {
			return err
		}
		property.MinItems = jsonschema.Ptr(1)
		return nil
	}
}

func mcpNonNullArray(path ...string) mcpInputSchemaPatch {
	return func(root *jsonschema.Schema) error {
		property, err := mcpInputArrayProperty(root, path)
		if err != nil {
			return err
		}
		property.Type = "array"
		property.Types = nil
		return nil
	}
}

func mcpInputArrayProperty(root *jsonschema.Schema, path []string) (*jsonschema.Schema, error) {
	dotted := strings.Join(path, ".")
	if len(path) == 0 {
		return nil, fmt.Errorf("path %q: array property path is empty", dotted)
	}
	property := root
	for _, name := range path {
		if property == nil || property.Properties[name] == nil {
			return nil, fmt.Errorf("path %q: property is missing", dotted)
		}
		property = property.Properties[name]
	}
	nullableArray := property.Type == "" && slices.Equal(property.Types, []string{"null", "array"})
	plainArray := property.Type == "array" && property.Types == nil
	if !nullableArray && !plainArray {
		return nil, fmt.Errorf("path %q: expected an inferred nullable or plain array", dotted)
	}
	if property.MinItems != nil {
		return nil, fmt.Errorf("path %q: minItems is already set", dotted)
	}
	return property, nil
}
