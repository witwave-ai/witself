package gitopsvalues

import (
	"fmt"
	"os"

	"go.yaml.in/yaml/v3"
)

func readServerPins(path string, defaults chartPins) (chartVersion, imageTag string, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			if defaults.DefaultChart == "" || defaults.DefaultImageTag == "" {
				return "", "", fmt.Errorf("%s does not exist and apps chart defaults have no chartVersion/imageTag", path)
			}
			// First write of a catalog cell uses the apps chart defaults.
			// scripts/roll-cell.sh then owns those two scalars.
			return defaults.DefaultChart, defaults.DefaultImageTag, nil
		}
		return "", "", err
	}
	var doc struct {
		Apps struct {
			WitselfServer struct {
				ChartVersion string `yaml:"chartVersion"`
				ImageTag     string `yaml:"imageTag"`
			} `yaml:"witselfServer"`
		} `yaml:"apps"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return "", "", fmt.Errorf("parse %s: %w", path, err)
	}
	chartVersion = doc.Apps.WitselfServer.ChartVersion
	imageTag = doc.Apps.WitselfServer.ImageTag
	if chartVersion == "" || imageTag == "" {
		return "", "", fmt.Errorf("%s: apps.witselfServer.chartVersion and imageTag are required (owned by scripts/roll-cell.sh)", path)
	}
	return chartVersion, imageTag, nil
}
