package gitopsvalues

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

type serverPins struct {
	ChartVersion string    `yaml:"chartVersion"`
	ImageTag     string    `yaml:"imageTag"`
	ImageDigest  string    `yaml:"-"`
	DigestNode   yaml.Node `yaml:"imageDigest"`
}

var imageDigestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

func readServerPins(path string, defaults chartPins) (serverPins, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			if defaults.DefaultChart == "" || defaults.DefaultImageTag == "" {
				return serverPins{}, fmt.Errorf("%s does not exist and apps chart defaults have no chartVersion/imageTag", path)
			}
			// First write of a catalog cell uses the apps chart defaults.
			// scripts/roll-cell.sh then supplies release pins through RollCell.
			return serverPins{ChartVersion: defaults.DefaultChart, ImageTag: defaults.DefaultImageTag}, nil
		}
		return serverPins{}, err
	}
	pins, err := parseServerPins(raw)
	if err != nil {
		return serverPins{}, fmt.Errorf("%s: %w", path, err)
	}
	if pins.ChartVersion == "" || pins.ImageTag == "" {
		return serverPins{}, fmt.Errorf("%s: apps.witselfServer.chartVersion and imageTag are required (owned by scripts/roll-cell.sh)", path)
	}
	return pins, nil
}

// ValidateServerImageDigest checks an optional server digest in cell values using
// the same parsing and validation rules as generation and RollCell.
func ValidateServerImageDigest(body []byte) error {
	_, err := parseServerPins(body)
	return err
}

func parseServerPins(raw []byte) (serverPins, error) {
	var doc struct {
		Apps struct {
			WitselfServer serverPins `yaml:"witselfServer"`
		} `yaml:"apps"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return serverPins{}, fmt.Errorf("parse cell values: %w", err)
	}
	pins := doc.Apps.WitselfServer
	if pins.DigestNode.Kind != 0 {
		if pins.DigestNode.Kind != yaml.ScalarNode || pins.DigestNode.Tag != "!!str" {
			return serverPins{}, fmt.Errorf("apps.witselfServer.imageDigest must be a string")
		}
		pins.ImageDigest = pins.DigestNode.Value
		if pins.ImageDigest != "" && !imageDigestPattern.MatchString(pins.ImageDigest) {
			return serverPins{}, fmt.Errorf("apps.witselfServer.imageDigest must be empty or sha256 followed by 64 lowercase hexadecimal characters")
		}
	}
	return pins, nil
}

// addServerDigest preserves template bytes and comments while adding the optional
// pin immediately after the imageTag scalar. YAML coordinates locate the exact
// server mapping, including Civo overlays whose source templates remain frozen.
func addServerDigest(body []byte, digest string) ([]byte, error) {
	if digest == "" {
		return body, nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("parse generated cell values: %w", err)
	}
	if len(doc.Content) != 1 {
		return nil, fmt.Errorf("generated cell values must contain one YAML document")
	}
	node := doc.Content[0]
	for _, key := range []string{"apps", "witselfServer", "imageTag"} {
		var next *yaml.Node
		if node.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(node.Content); i += 2 {
				if node.Content[i].Value == key {
					next = node.Content[i+1]
					break
				}
			}
		}
		if next == nil {
			return nil, fmt.Errorf("generated cell values lack apps.witselfServer.imageTag")
		}
		node = next
	}
	lines := strings.SplitAfter(string(body), "\n")
	if node.Kind != yaml.ScalarNode || node.Style != 0 || node.Line < 1 || node.Line > len(lines) {
		return nil, fmt.Errorf("generated cell imageTag must be a plain scalar")
	}
	line := lines[node.Line-1]
	indent := line[:len(line)-len(strings.TrimLeft(line, " "))]
	lines[node.Line-1] = line + indent + "imageDigest: " + digest + "\n"
	return []byte(strings.Join(lines, "")), nil
}
