package gitopsvalues

import (
	"fmt"
	"os"
	"path/filepath"

	"go.yaml.in/yaml/v3"
)

const (
	platformValuesRel = ".gitops/charts/platform/values.yaml"
	appsValuesRel     = ".gitops/charts/apps/values.yaml"
)

type chartPins struct {
	CertManager     string
	ExternalDNS     string
	ExternalSecrets string
	KEDA            string
	MetricsServer   string
	CivoPostgres    string
	DefaultChart    string
	DefaultImageTag string
}

func loadChartPins(root string) (chartPins, error) {
	var pins chartPins
	platformRaw, err := os.ReadFile(filepath.Join(root, platformValuesRel))
	if err != nil {
		return pins, fmt.Errorf("read %s: %w", platformValuesRel, err)
	}
	var platform struct {
		Platform struct {
			CertManager struct {
				ChartVersion string `yaml:"chartVersion"`
			} `yaml:"certManager"`
			ExternalDNS struct {
				ChartVersion string `yaml:"chartVersion"`
			} `yaml:"externalDNS"`
			ExternalSecrets struct {
				ChartVersion string `yaml:"chartVersion"`
			} `yaml:"externalSecrets"`
			Keda struct {
				ChartVersion string `yaml:"chartVersion"`
			} `yaml:"keda"`
			MetricsServer struct {
				ChartVersion string `yaml:"chartVersion"`
			} `yaml:"metricsServer"`
		} `yaml:"platform"`
	}
	if err := yaml.Unmarshal(platformRaw, &platform); err != nil {
		return pins, fmt.Errorf("parse %s: %w", platformValuesRel, err)
	}
	pins.CertManager = platform.Platform.CertManager.ChartVersion
	pins.ExternalDNS = platform.Platform.ExternalDNS.ChartVersion
	pins.ExternalSecrets = platform.Platform.ExternalSecrets.ChartVersion
	pins.KEDA = platform.Platform.Keda.ChartVersion
	pins.MetricsServer = platform.Platform.MetricsServer.ChartVersion
	if pins.CertManager == "" || pins.ExternalDNS == "" || pins.ExternalSecrets == "" || pins.KEDA == "" || pins.MetricsServer == "" {
		return pins, fmt.Errorf("%s: missing platform chartVersion pins", platformValuesRel)
	}

	appsRaw, err := os.ReadFile(filepath.Join(root, appsValuesRel))
	if err != nil {
		return pins, fmt.Errorf("read %s: %w", appsValuesRel, err)
	}
	var apps struct {
		Apps struct {
			CivoPostgres struct {
				ChartVersion string `yaml:"chartVersion"`
			} `yaml:"civoPostgres"`
			WitselfServer struct {
				ChartVersion string `yaml:"chartVersion"`
				ImageTag     string `yaml:"imageTag"`
			} `yaml:"witselfServer"`
		} `yaml:"apps"`
	}
	if err := yaml.Unmarshal(appsRaw, &apps); err != nil {
		return pins, fmt.Errorf("parse %s: %w", appsValuesRel, err)
	}
	pins.CivoPostgres = apps.Apps.CivoPostgres.ChartVersion
	pins.DefaultChart = apps.Apps.WitselfServer.ChartVersion
	pins.DefaultImageTag = apps.Apps.WitselfServer.ImageTag
	if pins.CivoPostgres == "" {
		return pins, fmt.Errorf("%s: missing apps.civoPostgres.chartVersion", appsValuesRel)
	}
	return pins, nil
}
