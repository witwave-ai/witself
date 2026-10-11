package gitopsvalues

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"sort"
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
	cfg, err := loadCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(generated) != len(cfg.Cells) {
		t.Fatalf("generated %d cells, want %d catalog cells", len(generated), len(cfg.Cells))
	}
	for _, cell := range sortedCells(generated) {
		if _, ok := cfg.Cells[cell]; !ok {
			t.Fatalf("generated cell %s is not in the catalog", cell)
		}
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
	tmpl := templates.Lookup("civo-sandbox-use1-serving.yaml.tmpl")
	if tmpl == nil {
		t.Fatal("missing civo-sandbox-use1-serving overlay template")
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
	// Only the serving cell enables the sealed-plane group.
	// The collector group has its own catalog switch, checked in
	// TestCollectorAlertsFollowCatalogSwitch.
	serving := cfg.Cells["civo-sandbox-use1-serving"]
	if !serving.Switches.SealedPlaneAlerts {
		t.Fatalf("civo-sandbox-use1-serving switches: sealed_plane_alerts=%v, want true",
			serving.Switches.SealedPlaneAlerts)
	}
	for name, cell := range cfg.Cells {
		if name == "civo-sandbox-use1-serving" {
			// The serving cell enables monitoring and the sealed-plane group.
			if !cell.Switches.Monitoring || !cell.Switches.SealedPlaneAlerts {
				t.Error("replacement serving cell must enable monitoring and the sealed-plane group")
			}
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
		if cell == "civo-sandbox-use1-serving" {
			if !hasSealed {
				t.Errorf("%s missing sealedPlaneAlerts block", valuesRel(cell))
			}
			continue
		}
		if hasSealed {
			t.Errorf("%s unexpectedly rendered sealedPlaneAlerts", valuesRel(cell))
		}
	}
}

func TestCatalogPostgresBackupSwitch(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  bool
	}{
		{input: "postgres_backup: true\n", want: true},
		{input: "postgres_backup: false\n"},
		{input: "{}\n"},
	} {
		var switches Switches
		if err := yaml.Unmarshal([]byte(tc.input), &switches); err != nil {
			t.Fatal(err)
		}
		if switches.PostgresBackup != tc.want {
			t.Errorf("decode %q: postgres_backup=%v, want %v", tc.input, switches.PostgresBackup, tc.want)
		}
	}
	cfg, err := loadCatalog(repoRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	for name, cell := range cfg.Cells {
		supported := isCivoRollCell(cfg.Cells[name])
		if cell.Switches.PostgresBackup && !supported {
			t.Errorf("%s activates postgres_backup; only Civo serving and backup cells may", name)
		}
	}
}

func TestPostgresBackupSingleCatalogSwitch(t *testing.T) {
	root := repoRoot(t)
	charts, err := loadChartPins(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, cell := range civoRollCells(t) {
		for _, monitoringEnabled := range []bool{false, true} {
			name := cell + "/monitoring-off"
			if monitoringEnabled {
				name = cell + "/monitoring-on"
			}
			t.Run(name, func(t *testing.T) {
				var disabledValues map[string]any
				for _, enabled := range []bool{false, true} {
					cfg, err := loadCatalog(root)
					if err != nil {
						t.Fatal(err)
					}
					entry := cfg.Cells[cell]
					entry.Switches.PostgresBackup = enabled
					entry.Switches.Monitoring = monitoringEnabled
					cfg.Cells[cell] = entry
					body, err := generateCell(root, cell, cfg, charts)
					if err != nil {
						t.Fatal(err)
					}
					values := decodePostgresBackupValues(t, body)
					postgres := values.Apps.CivoPostgres
					monitoring := values.Platform.Monitoring
					if postgres.Backup.Enabled == nil || *postgres.Backup.Enabled != enabled {
						t.Errorf("backup.enabled must match postgres_backup=%v", enabled)
					}
					if monitoring.PostgresBackupAlerts.Enabled == nil || *monitoring.PostgresBackupAlerts.Enabled != enabled {
						t.Errorf("postgresBackupAlerts.enabled must match postgres_backup=%v", enabled)
					}
					wantMonitoring := entry.Role == "serving" && monitoringEnabled
					if monitoring.Enabled != wantMonitoring || monitoring.Alerting.Enabled != wantMonitoring {
						t.Errorf("postgres_backup=%v changed independent monitoring or alerting switch", enabled)
					}
					if entry.Role == "backup" && postgres.Metrics.ServiceMonitor.Enabled {
						t.Errorf("postgres_backup=%v enabled a ServiceMonitor without a monitoring stack", enabled)
					}
					// Activation changes only the two derived flags. In particular,
					// account snapshots, monitoring resources, and receiver Secrets
					// must stay independent of this database-dump feature.
					var fullValues map[string]any
					if err := yaml.Unmarshal(body, &fullValues); err != nil {
						t.Fatal(err)
					}
					fullValues["apps"].(map[string]any)["civoPostgres"].(map[string]any)["backup"].(map[string]any)["enabled"] = false
					fullValues["platform"].(map[string]any)["monitoring"].(map[string]any)["postgresBackupAlerts"].(map[string]any)["enabled"] = false
					if !enabled {
						disabledValues = fullValues
					} else if !reflect.DeepEqual(fullValues, disabledValues) {
						t.Error("postgres_backup changed values beyond the dump and backup-alert switches")
					}
				}
			})
		}
	}
}

func TestGeneratedValuesPostgresBackupsDefaultOff(t *testing.T) {
	root := repoRoot(t)
	generated, err := generateAll(root)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := loadCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	for cell, body := range generated {
		values := decodePostgresBackupValues(t, body)
		backup := values.Apps.CivoPostgres.Backup.Enabled
		alerts := values.Platform.Monitoring.PostgresBackupAlerts.Enabled
		if isCivoRollCell(cfg.Cells[cell]) {
			want := cfg.Cells[cell].Switches.PostgresBackup
			if backup == nil || alerts == nil {
				t.Errorf("%s must explicitly set both backup and backup alerts", cell)
			} else if *backup != want || *alerts != want {
				t.Errorf("%s backup/backup-alert flags must match its catalog switch (%v)", cell, want)
			}
		} else if backup != nil || alerts != nil {
			t.Errorf("%s unexpectedly includes a PostgreSQL backup switch", cell)
		}
	}
}

type postgresBackupValues struct {
	Apps struct {
		CivoPostgres struct {
			Backup  struct{ Enabled *bool }
			Metrics struct {
				Enabled        bool
				ServiceMonitor struct {
					Enabled bool
					Labels  map[string]string
				} `yaml:"serviceMonitor"`
			}
		} `yaml:"civoPostgres"`
	}
	Platform struct {
		Monitoring struct {
			Enabled              bool
			Alerting             struct{ Enabled bool }
			PostgresBackupAlerts struct{ Enabled *bool } `yaml:"postgresBackupAlerts"`
			Receiver             struct {
				Kind       string
				SecretName string `yaml:"secretName"`
				SecretKey  string `yaml:"secretKey"`
			}
		}
	}
}

func decodePostgresBackupValues(t *testing.T, body []byte) postgresBackupValues {
	t.Helper()
	var values postgresBackupValues
	if err := yaml.Unmarshal(body, &values); err != nil {
		t.Fatalf("decode generated values: %v", err)
	}
	return values
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

// Catalog selection includes every overlapping serving cell in roll contracts.
func isCivoRollCell(cell CellConfig) bool {
	return cell.Cloud == "civo" && (cell.Role == "serving" || cell.Role == "backup")
}

func civoRollCells(t *testing.T) []string {
	t.Helper()
	cfg, err := loadCatalog(repoRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	var cells []string
	for name := range cfg.Cells {
		if isCivoRollCell(cfg.Cells[name]) {
			cells = append(cells, name)
		}
	}
	if len(cells) == 0 {
		t.Fatal("catalog has no Civo roll cells")
	}
	sort.Strings(cells)
	return cells
}

func TestProductionServingOnboarding(t *testing.T) {
	root := repoRoot(t)
	cfg, err := loadCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	cell := cfg.Cells["civo-prod-use1-serving"]
	if cell.Cloud != "civo" || cell.AccountAlias != "prod" || cell.Region != "nyc1" || cell.Role != "serving" || cell.Domain != "" || cell.Switches.DomainDocumentationOnly {
		t.Fatal("production cell identity must derive its real host")
	}
	if cell.Switches.Monitoring || cell.Switches.SealedPlaneAlerts || cell.Switches.PostgresBackup || cell.Switches.MemoryAlerts {
		t.Fatal("production operational switches must remain dark")
	}
	var unprovisioned []string
	for name, entry := range cfg.Cells {
		if entry.Unprovisioned {
			unprovisioned = append(unprovisioned, name)
		}
	}
	sort.Strings(unprovisioned)
	if len(unprovisioned) != 0 {
		t.Fatalf("no catalog cell may be recorded unprovisioned once its first-sync pins have merged; found %v", unprovisioned)
	}
	charts, err := loadChartPins(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, monitoring := range []bool{false, true} {
		cell.Switches.Monitoring = monitoring
		cfg.Cells["civo-prod-use1-serving"] = cell
		body, err := generateCell(root, "civo-prod-use1-serving", cfg, charts)
		if err != nil {
			t.Fatal(err)
		}
		var values map[string]any
		if err := yaml.Unmarshal(body, &values); err != nil {
			t.Fatal(err)
		}
		wants := map[string]any{
			"cell.apiHost":                      "api.civo-prod-use1-serving.cells.witself.witwave.ai",
			"apps.civoPostgres.resourcesPreset": "medium",
			"apps.witselfServer.agentEmail.receiveProduction.audience":   "civo-prod-use1-serving",
			"apps.witselfServer.worker.agentEmailOutbound.dispatchKeyID": "civo-prod-use1-serving-2026-10",
			"apps.witselfServer.backup.validationArchiveOrigin":          "https://self.witwave.ai",
			"platform.monitoring.enabled":                                monitoring,
			"platform.monitoring.uptimeProbes.enabled":                   false,
			"platform.monitoring.entitlementDelivery.enabled":            false,
			// A fresh cluster has no monitoring CRDs until phase 1 of the
			// monitoring rollout; ServiceMonitors would fail the first sync.
			"apps.civoPostgres.metrics.serviceMonitor.enabled":         false,
			"apps.witselfServer.metrics.serviceMonitor.enabled":        false,
			"apps.witselfServer.worker.metrics.serviceMonitor.enabled": false,
		}
		for path, want := range wants {
			var got any = values
			for _, key := range strings.Split(path, ".") {
				fields, ok := got.(map[string]any)
				if !ok {
					t.Fatalf("%s: missing mapping", path)
				}
				got = fields[key]
			}
			if got != want {
				t.Errorf("%s: got %v want %v", path, got, want)
			}
		}
	}
}

const collectorAlertsBlock = "    collectorAlerts:\n      enabled: true\n"

func TestCollectorAlertsFollowCatalogSwitch(t *testing.T) {
	for _, overlay := range []string{"civo-sandbox-use1-serving.yaml.tmpl", "civo-prod-use1-serving.yaml.tmpl"} {
		t.Run(overlay, func(t *testing.T) {
			tmpl := templates.Lookup(overlay)
			if tmpl == nil {
				t.Fatal("missing overlay template")
			}
			for _, tc := range []struct {
				name       string
				monitoring bool
				collectors bool
			}{
				{name: "enabled", monitoring: true, collectors: true},
				{name: "disabled", monitoring: true},
				{name: "monitoring-disabled", collectors: true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					data := templateData{
						Monitoring:               tc.monitoring,
						CollectorAlerts:          tc.collectors,
						ChartVersion:             "0.0.1",
						ImageTag:                 "0.0.1",
						CivoPostgresChartVersion: "1.0.0",
						CertManagerChartVersion:  "v1.0.0",
					}
					var buf bytes.Buffer
					if err := tmpl.Execute(&buf, data); err != nil {
						t.Fatalf("execute overlay: %v", err)
					}
					if tc.monitoring && tc.collectors {
						if strings.Count(buf.String(), collectorAlertsBlock) != 1 {
							t.Error("enabled switch must render exactly one collectorAlerts block")
						}
					} else if strings.Contains(buf.String(), "collectorAlerts") {
						t.Error("disabled monitoring or collector switch rendered collectorAlerts")
					}
				})
			}
		})
	}
	root := repoRoot(t)
	cfg, err := loadCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	generated, err := generateAll(root)
	if err != nil {
		t.Fatal(err)
	}
	for name, cell := range cfg.Cells {
		want := 0
		if cell.Switches.CollectorAlerts {
			want = 1
			if cell.Role != "serving" || !cell.Switches.Monitoring {
				t.Errorf("%s: collector_alerts requires role serving and monitoring", name)
			}
		}
		if got := bytes.Count(generated[name], []byte(collectorAlertsBlock)); got != want {
			t.Errorf("%s: generated %d collectorAlerts blocks, want %d from catalog", name, got, want)
		}
	}
}

func TestCollectorFlipIsOneCatalogLine(t *testing.T) {
	root := repoRoot(t)
	const cellName = "civo-sandbox-use1-serving"
	catalogBytes, err := os.ReadFile(filepath.Join(root, catalogRelPath))
	if err != nil {
		t.Fatal(err)
	}
	var catalog map[string]any
	if err := yaml.Unmarshal(catalogBytes, &catalog); err != nil {
		t.Fatal(err)
	}
	var fields any = catalog
	for _, key := range []string{"cells", cellName, "switches"} {
		mapping, ok := fields.(map[string]any)
		if !ok {
			t.Fatalf("catalog %s must be a mapping", key)
		}
		fields = mapping[key]
	}
	switches, ok := fields.(map[string]any)
	if !ok {
		t.Fatal("serving switches must be a mapping")
	}
	committedSwitch, ok := switches["collector_alerts"].(bool)
	if !ok {
		t.Fatal("serving switches must include an explicit boolean collector_alerts key")
	}
	cfg, err := loadCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	charts, err := loadChartPins(root)
	if err != nil {
		t.Fatal(err)
	}
	cell := cfg.Cells[cellName]
	cell.Switches.CollectorAlerts = !committedSwitch
	cfg.Cells[cellName] = cell
	flipped, err := generateCell(root, cellName, cfg, charts)
	if err != nil {
		t.Fatal(err)
	}
	valuesPath := valuesRel(cellName)
	committed, err := os.ReadFile(filepath.Join(root, valuesPath))
	if err != nil {
		t.Fatal(err)
	}
	enabled, disabled := flipped, committed
	oldLine, newLine := "      collector_alerts: false\n", "      collector_alerts: true\n"
	if committedSwitch {
		enabled, disabled = committed, flipped
		oldLine, newLine = newLine, oldLine
	}
	block := []byte(collectorAlertsBlock)
	if bytes.Count(enabled, block) != 1 || bytes.Contains(disabled, block) {
		t.Fatal("collector flip must add or remove exactly one enabled block")
	}
	if !bytes.Equal(bytes.Replace(enabled, block, nil, 1), disabled) {
		t.Fatal("collector flip changed generated values beyond its two-line block")
	}
	if bytes.Count(catalogBytes, []byte(oldLine)) != 1 {
		t.Fatal("collector flip requires exactly one matching catalog switch line")
	}
	patch, err := os.ReadFile(filepath.Join(root, "scripts/testdata/monitoring-recovery/phase4.patch"))
	if err != nil {
		t.Fatal(err)
	}
	patched := applyCollectorFlipPatch(t, patch, map[string][]byte{
		catalogRelPath: catalogBytes,
		valuesPath:     committed,
	}, committedSwitch)
	if !bytes.Equal(patched[valuesPath], flipped) {
		t.Error("phase4.patch values do not equal the generator's collector flip")
	}
	wantCatalog := bytes.Replace(catalogBytes, []byte(oldLine), []byte(newLine), 1)
	if !bytes.Equal(patched[catalogRelPath], wantCatalog) {
		t.Error("phase4.patch catalog does not equal the single switch-line flip")
	}
}

// Apply by unique hunk context so release pin and roll-state line offsets do not
// matter. No git executable or repository metadata is needed by this test.
func applyCollectorFlipPatch(t *testing.T, patch []byte, current map[string][]byte, reverse bool) map[string][]byte {
	t.Helper()
	result := make(map[string][]byte, len(current))
	sections := []string{""}
	for _, line := range strings.SplitAfter(string(patch), "\n") {
		if strings.HasPrefix(line, "diff --git ") {
			sections = append(sections, strings.TrimPrefix(line, "diff --git "))
		} else {
			sections[len(sections)-1] += line
		}
	}
	if sections[0] != "" || len(sections) != len(current)+1 {
		t.Fatal("phase4.patch must have exactly the catalog and serving values sections")
	}
	for _, section := range sections[1:] {
		lines := strings.SplitAfter(section, "\n")
		var path string
		for candidate := range current {
			if lines[0] == "a/"+candidate+" b/"+candidate+"\n" {
				path = candidate
			}
		}
		if path == "" {
			t.Fatal("phase4.patch contains an unexpected file section")
		}
		if _, exists := result[path]; exists {
			t.Fatal("phase4.patch repeats a file section")
		}
		body := string(current[path])
		var old, replacement strings.Builder
		inHunk := false
		hunks := 0
		apply := func() {
			if !inHunk {
				return
			}
			before, after := old.String(), replacement.String()
			if reverse {
				before, after = after, before
			}
			if before == "" || strings.Count(body, before) != 1 {
				t.Fatalf("phase4.patch %s hunk must match exactly once", path)
			}
			body = strings.Replace(body, before, after, 1)
			old.Reset()
			replacement.Reset()
		}
		for i, line := range lines[1:] {
			if line == "" && i == len(lines)-2 {
				continue // SplitAfter's final empty element is not a patch line.
			}
			if strings.HasPrefix(line, "@@ ") {
				apply()
				inHunk = true
				hunks++
				continue
			}
			if !inHunk {
				if strings.HasPrefix(line, "index ") || line == "--- a/"+path+"\n" || line == "+++ b/"+path+"\n" {
					continue
				}
				t.Fatal("phase4.patch contains unsupported section metadata")
			}
			if !strings.HasSuffix(line, "\n") {
				t.Fatal("phase4.patch hunk line must end with a newline")
			}
			switch line[0] {
			case ' ':
				old.WriteString(line[1:])
				replacement.WriteString(line[1:])
			case '-':
				old.WriteString(line[1:])
			case '+':
				replacement.WriteString(line[1:])
			default:
				t.Fatal("phase4.patch contains an unsupported hunk line")
			}
		}
		if hunks == 0 {
			t.Fatal("phase4.patch section has no hunks")
		}
		apply()
		result[path] = []byte(body)
	}
	return result
}
