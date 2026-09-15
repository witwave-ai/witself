package gitopsvalues

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestCommittedValuesMatchGenerator(t *testing.T) {
	root := repoRoot(t)
	generated, err := generateAll(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(generated) != 9 {
		t.Fatalf("generated %d cells, want 9", len(generated))
	}
	for _, cell := range sortedCells(generated) {
		path := filepath.Join(root, filepath.FromSlash(valuesRel(cell)))
		committed, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if !bytes.Equal(committed, generated[cell]) {
			t.Errorf("%s differs from generated output; run scripts/gitops-cell-values.sh --check", valuesRel(cell))
		}
	}
}

func TestCheckPassesOnCommittedTree(t *testing.T) {
	root := repoRoot(t)
	var buf bytes.Buffer
	if err := Check(root, &buf); err != nil {
		t.Fatalf("check: %v\n%s", err, buf.String())
	}
}

const sealedPlaneAlertsBlock = "    sealedPlaneAlerts:\n      enabled: true\n"

func TestSealedPlaneAlertsTemplate(t *testing.T) {
	tmpl := templates.Lookup("civo-sandbox-usw2-dev.yaml.tmpl")
	if tmpl == nil {
		t.Fatal("missing civo-sandbox-usw2-dev overlay template")
	}
	for _, tc := range []struct {
		name    string
		enabled bool
	}{
		{name: "enabled", enabled: true},
		{name: "disabled", enabled: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := templateData{
				Monitoring:               true,
				SealedPlaneAlerts:        tc.enabled,
				ChartVersion:             "0.0.1",
				ImageTag:                 "0.0.1",
				CivoPostgresChartVersion: "1.0.0",
				CertManagerChartVersion:  "v1.0.0",
			}
			var buf bytes.Buffer
			if err := tmpl.Execute(&buf, data); err != nil {
				t.Fatalf("execute overlay: %v", err)
			}
			got := buf.String()
			if tc.enabled {
				if !strings.Contains(got, sealedPlaneAlertsBlock) {
					t.Fatalf("enabled switch did not render sealedPlaneAlerts block:\n%s", got)
				}
				return
			}
			if strings.Contains(got, "sealedPlaneAlerts") {
				t.Fatalf("disabled switch rendered sealedPlaneAlerts:\n%s", got)
			}
		})
	}
}

func TestCatalogSealedPlaneAlertsSwitch(t *testing.T) {
	var enabled, omitted Switches
	if err := yaml.Unmarshal([]byte("sealed_plane_alerts: true\n"), &enabled); err != nil {
		t.Fatalf("decode enabled switch: %v", err)
	}
	if !enabled.SealedPlaneAlerts {
		t.Fatal("sealed_plane_alerts: true did not set SealedPlaneAlerts")
	}
	if err := yaml.Unmarshal([]byte("{}\n"), &omitted); err != nil {
		t.Fatalf("decode omitted switch: %v", err)
	}
	if omitted.SealedPlaneAlerts {
		t.Fatal("omitted sealed_plane_alerts should stay false")
	}

	cfg, err := loadCatalog(repoRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	// The collector group stays off on purpose: WitselfIdentityCapacityAtLimit
	// would fire permanently because every Personal account already holds its
	// single root operator seat (used == cap == 1). Only the sealed-plane group
	// is enabled on the serving cell.
	serving := cfg.Cells["civo-sandbox-usw2-dev"]
	if serving.Switches.CollectorAlerts || !serving.Switches.SealedPlaneAlerts {
		t.Fatalf("civo-sandbox-usw2-dev switches: collector_alerts=%v sealed_plane_alerts=%v, want false/true",
			serving.Switches.CollectorAlerts, serving.Switches.SealedPlaneAlerts)
	}
	for name, cell := range cfg.Cells {
		if name == "civo-sandbox-usw2-dev" {
			continue
		}
		if cell.Switches.SealedPlaneAlerts {
			t.Errorf("%s unexpectedly sets sealed_plane_alerts", name)
		}
	}
}

func TestGeneratedValuesSealedPlaneAlertsOnlyOnServingCell(t *testing.T) {
	generated, err := generateAll(repoRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, cell := range sortedCells(generated) {
		body := string(generated[cell])
		hasSealed := strings.Contains(body, sealedPlaneAlertsBlock)
		hasCollector := strings.Contains(body, "    collectorAlerts:\n      enabled: true\n")
		if cell == "civo-sandbox-usw2-dev" {
			if !hasSealed {
				t.Errorf("%s missing sealedPlaneAlerts block", valuesRel(cell))
			}
			if hasCollector {
				t.Errorf("%s rendered collectorAlerts, which must stay off until the identity-capacity rule semantics are fixed", valuesRel(cell))
			}
			continue
		}
		if hasSealed {
			t.Errorf("%s unexpectedly rendered sealedPlaneAlerts", valuesRel(cell))
		}
		if hasCollector {
			t.Errorf("%s unexpectedly rendered collectorAlerts", valuesRel(cell))
		}
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find go.mod walking upward")
		}
		dir = parent
	}
}
