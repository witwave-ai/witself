package gitopsvalues

import (
	"bytes"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestCatalogMemoryAlertsSwitch(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  bool
	}{
		{input: "memory_alerts: true\n", want: true},
		{input: "memory_alerts: false\n"},
		{input: "{}\n"},
	} {
		var switches Switches
		if err := yaml.Unmarshal([]byte(tc.input), &switches); err != nil {
			t.Fatal(err)
		}
		if switches.MemoryAlerts != tc.want {
			t.Errorf("memory_alerts=%v, want %v", switches.MemoryAlerts, tc.want)
		}
	}
	cfg, err := loadCatalog(repoRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	for name, cell := range cfg.Cells {
		if cell.Switches.MemoryAlerts {
			t.Errorf("%s: memory alerts must remain dark", name)
		}
	}
	for _, name := range []string{"civo-sandbox-use1-serving", "civo-sandbox-use1-backup"} {
		cell := cfg.Cells[name]
		cell.Switches.MemoryAlerts = true
		cfg.Cells[name] = cell
		data, err := resolveCell(name, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if !data.MemoryAlerts {
			t.Errorf("%s: catalog opt-in did not reach template data", name)
		}
	}
}

func TestMemoryAlertsTemplates(t *testing.T) {
	for _, name := range []string{"civo-sandbox-use1-serving", "civo-sandbox-use1-backup"} {
		for _, monitoring := range []bool{false, true} {
			for _, enabled := range []bool{false, true} {
				data := templateData{MemoryAlerts: enabled, Monitoring: monitoring}
				var buf bytes.Buffer
				if err := templates.ExecuteTemplate(&buf, name+".yaml.tmpl", data); err != nil {
					t.Fatal(err)
				}
				if got := memoryAlertsEnabled(t, buf.Bytes()); got == nil || *got != enabled {
					t.Errorf("%s: memory alert switch did not render explicit %v with monitoring=%v", name, enabled, monitoring)
				}
			}
		}
	}
}

func TestGeneratedValuesMemoryAlertsRemainDark(t *testing.T) {
	generated, err := generateAll(repoRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range generated {
		enabled := memoryAlertsEnabled(t, body)
		if name == "civo-sandbox-use1-serving" || name == "civo-sandbox-use1-backup" {
			if enabled == nil || *enabled {
				t.Errorf("%s: memory alerts must be explicitly false", name)
			}
		} else if enabled != nil {
			t.Errorf("%s: unrelated fixture gained memory alerts", name)
		}
	}
}

func memoryAlertsEnabled(t *testing.T, body []byte) *bool {
	t.Helper()
	var values struct {
		Platform struct {
			Monitoring struct {
				MemoryAlerts struct {
					Enabled *bool
				} `yaml:"memoryAlerts"`
			}
		}
	}
	if err := yaml.Unmarshal(body, &values); err != nil {
		t.Fatal(err)
	}
	return values.Platform.Monitoring.MemoryAlerts.Enabled
}
