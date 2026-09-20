package gitopsvalues

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
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
	if len(generated) != 8 {
		t.Fatalf("generated %d cells, want 8", len(generated))
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
	// The collector group stays off on purpose: WitselfIdentityCapacityAtLimit
	// would fire permanently because every Personal account already holds its
	// single root operator seat (used == cap == 1). Only the sealed-plane group
	// is enabled on the serving cell.
	serving := cfg.Cells["civo-sandbox-use1-serving"]
	if serving.Switches.CollectorAlerts || !serving.Switches.SealedPlaneAlerts {
		t.Fatalf("civo-sandbox-use1-serving switches: collector_alerts=%v sealed_plane_alerts=%v, want false/true",
			serving.Switches.CollectorAlerts, serving.Switches.SealedPlaneAlerts)
	}
	for name, cell := range cfg.Cells {
		if name == "civo-sandbox-use1-serving" {
			// The serving cell enables monitoring and the sealed-plane group,
			// with the collector group off until the
			// identity-capacity rule semantics are fixed.
			if !cell.Switches.Monitoring || cell.Switches.CollectorAlerts || !cell.Switches.SealedPlaneAlerts {
				t.Error("replacement serving cell must enable monitoring and the sealed-plane group only")
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
		hasCollector := strings.Contains(body, "    collectorAlerts:\n      enabled: true\n")
		if cell == "civo-sandbox-use1-serving" {
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
		supported := name == "civo-sandbox-use1-serving" || name == "civo-sandbox-use1-backup"
		if cell.Switches.PostgresBackup && !supported {
			t.Errorf("%s activates postgres_backup; only the two reviewed Civo cells may", name)
		}
	}
}

func TestPostgresBackupSingleCatalogSwitch(t *testing.T) {
	root := repoRoot(t)
	charts, err := loadChartPins(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, cell := range []string{"civo-sandbox-use1-serving", "civo-sandbox-use1-backup"} {
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
					wantMonitoring := cell == "civo-sandbox-use1-serving" && monitoringEnabled
					if monitoring.Enabled != wantMonitoring || monitoring.Alerting.Enabled != wantMonitoring {
						t.Errorf("postgres_backup=%v changed independent monitoring or alerting switch", enabled)
					}
					if cell == "civo-sandbox-use1-backup" && postgres.Metrics.ServiceMonitor.Enabled {
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
		if cell == "civo-sandbox-use1-serving" || cell == "civo-sandbox-use1-backup" {
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
