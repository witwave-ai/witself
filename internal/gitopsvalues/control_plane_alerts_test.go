package gitopsvalues

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestCatalogControlPlaneAlertsSwitch(t *testing.T) {
	for _, tc := range []struct {
		name     string
		switches string
		want     bool
		wantErr  string
	}{
		{name: "enabled", switches: "{control_plane_alerts: true}", want: true},
		{name: "omitted", switches: "{}"},
		{name: "misspelled", switches: "{control_plane_alert: true}", wantErr: "field control_plane_alert not found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, catalogRelPath)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			body := "schema: " + catalogSchema + `
gitops:
  repoURL: https://example.invalid/gitops
  targetRevision: main
defaults:
  domain_parent: example.invalid
cells:
  test-cell:
    cloud: civo
    region: nyc1
    switches: ` + tc.switches + "\n"
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := loadCatalog(root)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("loadCatalog error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := cfg.Cells["test-cell"].Switches.ControlPlaneAlerts; got != tc.want {
				t.Errorf("ControlPlaneAlerts = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("committed production explicitly disabled", func(t *testing.T) {
		body, err := os.ReadFile(filepath.Join(repoRoot(t), catalogRelPath))
		if err != nil {
			t.Fatal(err)
		}
		var document yaml.Node
		if err := yaml.Unmarshal(body, &document); err != nil {
			t.Fatal(err)
		}
		if len(document.Content) != 1 {
			t.Fatal("catalog must contain one YAML document")
		}
		node := document.Content[0]
		for _, key := range []string{"cells", "civo-prod-use1-serving", "switches", "control_plane_alerts"} {
			if node.Kind != yaml.MappingNode {
				t.Fatalf("catalog parent of %s must be a mapping", key)
			}
			var next *yaml.Node
			for i := 0; i < len(node.Content); i += 2 {
				if node.Content[i].Value == key {
					next = node.Content[i+1]
					break
				}
			}
			if next == nil {
				t.Fatalf("committed production catalog must explicitly set control_plane_alerts: false; missing %s", key)
			}
			node = next
		}
		if node.Kind != yaml.ScalarNode || node.Tag != "!!bool" || node.Value != "false" {
			t.Fatal("committed production catalog must explicitly set control_plane_alerts: false")
		}
	})
}

func TestProdOverlayControlPlaneAlertsRender(t *testing.T) {
	root := repoRoot(t)
	cfg, err := loadCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	charts, err := loadChartPins(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, monitoring := range []bool{false, true} {
		for _, controlPlaneAlerts := range []bool{false, true} {
			t.Run(fmt.Sprintf("monitoring=%t/control_plane_alerts=%t", monitoring, controlPlaneAlerts), func(t *testing.T) {
				cell := cfg.Cells["civo-prod-use1-serving"]
				cell.Switches.Monitoring = monitoring
				cell.Switches.ControlPlaneAlerts = controlPlaneAlerts
				cfg.Cells["civo-prod-use1-serving"] = cell
				body, err := generateCell(root, "civo-prod-use1-serving", cfg, charts)
				if err != nil {
					t.Fatal(err)
				}
				uptime, entitlement, err := controlPlaneAlertGates(body)
				if err != nil {
					t.Fatal(err)
				}
				want := monitoring && controlPlaneAlerts
				if uptime != want || entitlement != want {
					t.Errorf("production control-plane gates: uptimeProbes=%v entitlementDelivery=%v, want both %v", uptime, entitlement, want)
				}
			})
		}
	}
}

func TestControlPlaneAlertsSingleOwner(t *testing.T) {
	root := repoRoot(t)
	charts, err := loadChartPins(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name                 string
		productionMonitoring bool
		productionAlerts     bool
		servingMonitoring    bool
		want                 []string
		wantErr              bool
	}{
		{name: "committed", want: []string{"civo-sandbox-use1-serving"}},
		{name: "duplicate ownership", productionMonitoring: true, productionAlerts: true, servingMonitoring: true, want: []string{"civo-prod-use1-serving", "civo-sandbox-use1-serving"}, wantErr: true},
		{name: "production monitoring without ownership", productionMonitoring: true, servingMonitoring: true, want: []string{"civo-sandbox-use1-serving"}},
		{name: "ownership moved to production", productionMonitoring: true, productionAlerts: true, want: []string{"civo-prod-use1-serving"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadCatalog(root)
			if err != nil {
				t.Fatal(err)
			}
			if tc.name != "committed" {
				production := cfg.Cells["civo-prod-use1-serving"]
				production.Switches.Monitoring = tc.productionMonitoring
				production.Switches.ControlPlaneAlerts = tc.productionAlerts
				cfg.Cells["civo-prod-use1-serving"] = production
				serving := cfg.Cells["civo-sandbox-use1-serving"]
				serving.Switches.Monitoring = tc.servingMonitoring
				cfg.Cells["civo-sandbox-use1-serving"] = serving
			}
			owners, err := controlPlaneAlertOwners(root, cfg, charts)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(owners, tc.want) {
				t.Errorf("control-plane alert owners = %v, want %v", owners, tc.want)
			}
			err = checkControlPlaneAlertOwners(owners)
			if !tc.wantErr {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil {
				t.Fatal("multiple control-plane alert owners must fail the guard")
			}
			for _, owner := range tc.want {
				if !strings.Contains(err.Error(), owner) {
					t.Errorf("guard error must name owner %s: %v", owner, err)
				}
			}
			const remedy = "Only one cell may scrape the control plane and page on its synthetic-uptime, Stripe and entitlement rules. Move ownership in the same PR: turn the old owner's gates off."
			if !strings.Contains(err.Error(), remedy) {
				t.Errorf("guard error must include ownership remedy: %v", err)
			}
		})
	}
}

func controlPlaneAlertOwners(root string, cfg *Catalog, charts chartPins) ([]string, error) {
	var owners []string
	for name := range cfg.Cells {
		body, err := generateCell(root, name, cfg, charts)
		if err != nil {
			return nil, fmt.Errorf("generate %s: %w", name, err)
		}
		owns, err := ownsControlPlaneAlerts(body)
		if err != nil {
			return nil, fmt.Errorf("parse control-plane alert gates for %s: %w", name, err)
		}
		if owns {
			owners = append(owners, name)
		}
	}
	sort.Strings(owners)
	return owners, nil
}

func checkControlPlaneAlertOwners(owners []string) error {
	if len(owners) > 1 {
		return fmt.Errorf("control-plane alert ownership conflict: Only one cell may scrape the control plane and page on its synthetic-uptime, Stripe and entitlement rules. Move ownership in the same PR: turn the old owner's gates off. Owners: %s", strings.Join(owners, ", "))
	}
	return nil
}

// ownsControlPlaneAlerts reports whether a rendered values body owns the
// control-plane alerts: either gate alone renders control-plane scraping or
// paging rules, so either one makes the cell an owner.
func ownsControlPlaneAlerts(body []byte) (bool, error) {
	uptime, entitlement, err := controlPlaneAlertGates(body)
	if err != nil {
		return false, err
	}
	return uptime || entitlement, nil
}

func TestOwnsControlPlaneAlertsEitherGate(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"both gates on", "platform:\n  monitoring:\n    uptimeProbes:\n      enabled: true\n    entitlementDelivery:\n      enabled: true\n", true},
		{"only uptime probes", "platform:\n  monitoring:\n    uptimeProbes:\n      enabled: true\n    entitlementDelivery:\n      enabled: false\n", true},
		{"only entitlement delivery", "platform:\n  monitoring:\n    uptimeProbes:\n      enabled: false\n    entitlementDelivery:\n      enabled: true\n", true},
		{"both gates off", "platform:\n  monitoring:\n    uptimeProbes:\n      enabled: false\n    entitlementDelivery:\n      enabled: false\n", false},
		{"keys absent", "platform:\n  monitoring: {}\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ownsControlPlaneAlerts([]byte(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("ownsControlPlaneAlerts = %v, want %v", got, tc.want)
			}
		})
	}
}

func controlPlaneAlertGates(body []byte) (uptime, entitlement bool, err error) {
	var values struct {
		Platform struct {
			Monitoring struct {
				UptimeProbes struct {
					Enabled bool `yaml:"enabled"`
				} `yaml:"uptimeProbes"`
				EntitlementDelivery struct {
					Enabled bool `yaml:"enabled"`
				} `yaml:"entitlementDelivery"`
			} `yaml:"monitoring"`
		} `yaml:"platform"`
	}
	if err := yaml.Unmarshal(body, &values); err != nil {
		return false, false, err
	}
	return values.Platform.Monitoring.UptimeProbes.Enabled, values.Platform.Monitoring.EntitlementDelivery.Enabled, nil
}
