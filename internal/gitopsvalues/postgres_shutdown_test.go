package gitopsvalues

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestPostgresShutdownBackupOnly(t *testing.T) {
	root := repoRoot(t)
	cfg, err := loadCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	generated, err := generateAll(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range sortedCells(generated) {
		t.Run(name, func(t *testing.T) {
			data, err := resolveCell(name, cfg)
			if err != nil {
				t.Fatal(err)
			}
			var values struct {
				Apps struct {
					CivoPostgres struct {
						Shutdown *postgresShutdown `yaml:"shutdown"`
					} `yaml:"civoPostgres"`
				} `yaml:"apps"`
			}
			if err := yaml.Unmarshal(generated[name], &values); err != nil {
				t.Fatal(err)
			}
			var want *postgresShutdown
			if name == "civo-sandbox-use1-backup" {
				want = &postgresShutdown{Mode: "fast", TimeoutSeconds: 180, GracePeriodSeconds: 240}
			}
			if !reflect.DeepEqual(data.PostgresShutdown, want) {
				t.Error("resolved shutdown must enable fast/180/240 on the backup cell only")
			}
			if !reflect.DeepEqual(values.Apps.CivoPostgres.Shutdown, want) {
				t.Error("generated shutdown must enable fast/180/240 on the backup cell only")
			}
		})
	}
}

func TestPostgresShutdownTemplateOptional(t *testing.T) {
	for _, name := range civoRollCells(t) {
		t.Run(name, func(t *testing.T) {
			testPostgresShutdownTemplateOptional(t, name+".yaml.tmpl")
		})
	}
}

func testPostgresShutdownTemplateOptional(t *testing.T, name string) {
	t.Helper()
	tmpl := templates.Lookup(name)
	if tmpl == nil {
		t.Fatal("missing Civo overlay template")
	}
	render := func(shutdown *postgresShutdown) []byte {
		t.Helper()
		var output bytes.Buffer
		if err := tmpl.Execute(&output, templateData{PostgresShutdown: shutdown}); err != nil {
			t.Fatal(err)
		}
		// Embedded templates inherit checkout line endings on Windows.
		return bytes.ReplaceAll(output.Bytes(), []byte("\r\n"), []byte("\n"))
	}
	absent := render(nil)
	if bytes.Contains(absent, []byte("shutdown:")) {
		t.Fatal("absent shutdown setting rendered a shutdown block")
	}
	// Distinct values prove the overlay consumes the typed fields rather than
	// hard-coding the one rollout's timing into its YAML.
	present := render(&postgresShutdown{Mode: "fast", TimeoutSeconds: 90, GracePeriodSeconds: 120})
	block := []byte("    shutdown:\n      mode: fast\n      timeoutSeconds: 90\n      gracePeriodSeconds: 120\n")
	if bytes.Count(present, block) != 1 {
		t.Fatal("present shutdown setting did not render its exact typed fields once")
	}
	if !bytes.Equal(bytes.Replace(present, block, nil, 1), absent) {
		t.Fatal("shutdown setting changed bytes outside its own block")
	}
}

func TestCatalogPostgresFastShutdownSwitch(t *testing.T) {
	const validationError = `: cell "private-cell-marker" sets postgres_fast_shutdown but does not deploy in-cluster Civo PostgreSQL`
	for _, tc := range []struct {
		name          string
		cloud         string
		overlay       string
		switches      string
		unprovisioned bool
		want          bool
		wantError     string
	}{
		{name: "enabled", cloud: "civo", overlay: "civo-sandbox-use1-backup.yaml.tmpl", switches: "{postgres_fast_shutdown: true}", want: true},
		{name: "disabled", cloud: "civo", overlay: "civo-sandbox-use1-backup.yaml.tmpl", switches: "{postgres_fast_shutdown: false}"},
		{name: "absent", cloud: "civo", overlay: "civo-sandbox-use1-backup.yaml.tmpl", switches: "{}"},
		{name: "serving", cloud: "civo", overlay: "civo-sandbox-use1-serving.yaml.tmpl", switches: "{postgres_fast_shutdown: true}", want: true},
		{name: "production serving", cloud: "civo", overlay: "civo-prod-use1-serving.yaml.tmpl", switches: "{postgres_fast_shutdown: true}", want: true},
		// postgres_backup permits first-sync preparation. The new switch has
		// the same provision-state policy when the overlay deploys PostgreSQL.
		{name: "unprovisioned database", cloud: "civo", overlay: "civo-prod-use1-serving.yaml.tmpl", switches: "{postgres_fast_shutdown: true}", unprovisioned: true, want: true},
		{name: "aws", cloud: "aws", overlay: "civo-sandbox-use1-backup.yaml.tmpl", switches: "{postgres_fast_shutdown: true}", wantError: validationError},
		{name: "azure", cloud: "azure", overlay: "civo-sandbox-use1-backup.yaml.tmpl", switches: "{postgres_fast_shutdown: true}", wantError: validationError},
		{name: "gcp", cloud: "gcp", overlay: "civo-sandbox-use1-backup.yaml.tmpl", switches: "{postgres_fast_shutdown: true}", wantError: validationError},
		{name: "unknown cloud", cloud: "unknown", overlay: "civo-sandbox-use1-backup.yaml.tmpl", switches: "{postgres_fast_shutdown: true}", wantError: validationError},
		{name: "no overlay", cloud: "civo", switches: "{postgres_fast_shutdown: true}", wantError: validationError},
		{name: "unknown overlay", cloud: "civo", overlay: "missing.yaml.tmpl", switches: "{postgres_fast_shutdown: true}", wantError: validationError},
		{name: "non-database overlay", cloud: "civo", overlay: "gcp.yaml.tmpl", switches: "{postgres_fast_shutdown: true}", wantError: validationError},
		{name: "disabled non-database", cloud: "gcp", switches: "{postgres_fast_shutdown: false}"},
		{name: "absent non-database", cloud: "gcp", switches: "{}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, catalogRelPath)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			unprovisioned := "false"
			if tc.unprovisioned {
				unprovisioned = "true"
			}
			body := "schema: " + catalogSchema + `
gitops:
  repoURL: https://example.invalid/gitops
  targetRevision: main
defaults:
  domain_parent: example.invalid
cells:
  private-cell-marker:
    cloud: ` + tc.cloud + `
    region: nyc1
    overlay: "` + tc.overlay + `"
    unprovisioned: ` + unprovisioned + `
    switches: ` + tc.switches + "\n"
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := loadCatalog(root)
			if tc.wantError != "" {
				if err == nil || err.Error() != path+tc.wantError {
					t.Fatal("catalog must reject unsupported shutdown with the exact error naming the catalog and the cell")
				}
				return
			}
			if err != nil {
				t.Fatal("catalog rejected a supported shutdown switch state")
			}
			if cfg.Cells["private-cell-marker"].Switches.PostgresFastShutdown != tc.want {
				t.Fatal("catalog shutdown switch did not retain its boolean/default value")
			}
		})
	}
}

func TestPostgresShutdownSingleCatalogSwitch(t *testing.T) {
	root := repoRoot(t)
	charts, err := loadChartPins(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range civoRollCells(t) {
		t.Run(name, func(t *testing.T) {
			cfg, err := loadCatalog(root)
			if err != nil {
				t.Fatal(err)
			}
			var disabled []byte
			for _, enabled := range []bool{false, true} {
				entry := cfg.Cells[name]
				entry.Switches.PostgresFastShutdown = enabled
				cfg.Cells[name] = entry
				data, err := resolveCell(name, cfg)
				if err != nil {
					t.Fatal(err)
				}
				var want *postgresShutdown
				if enabled {
					want = &postgresShutdown{Mode: "fast", TimeoutSeconds: 180, GracePeriodSeconds: 240}
				}
				if !reflect.DeepEqual(data.PostgresShutdown, want) {
					t.Error("resolved shutdown must depend only on the catalog switch with fixed fast/180/240 settings")
				}
				body, err := generateCell(root, name, cfg, charts)
				if err != nil {
					t.Fatal(err)
				}
				body = bytes.ReplaceAll(body, []byte("\r\n"), []byte("\n"))
				block := []byte("    shutdown:\n      mode: fast\n      timeoutSeconds: 180\n      gracePeriodSeconds: 240\n")
				if !enabled {
					if bytes.Contains(body, []byte("shutdown:")) {
						t.Fatal("disabled switch must omit the generated shutdown block")
					}
					disabled = body
					continue
				}
				if bytes.Count(body, block) != 1 {
					t.Fatal("enabled switch must generate exactly one fixed fast/180/240 shutdown block")
				}
				if !bytes.Equal(bytes.Replace(body, block, nil, 1), disabled) {
					t.Fatal("shutdown switch changed generated bytes outside its own block")
				}
			}
		})
	}

	t.Run("different identity", func(t *testing.T) {
		cfg, err := loadCatalog(root)
		if err != nil {
			t.Fatal(err)
		}
		entry := cfg.Cells["civo-sandbox-use1-backup"]
		entry.AccountAlias, entry.Role = "fixture", "database"
		entry.Switches.PostgresFastShutdown = true
		cfg.Cells["civo-fixture-use1-database"] = entry
		data, err := resolveCell("civo-fixture-use1-database", cfg)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(data.PostgresShutdown, &postgresShutdown{Mode: "fast", TimeoutSeconds: 180, GracePeriodSeconds: 240}) {
			t.Fatal("shutdown switch must work independently of cell name, account and role")
		}
	})
}

func TestPostgresShutdownSourceContract(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "internal/gitopsvalues/identity.go"))
	if err != nil {
		t.Fatal(err)
	}
	// Keep source contracts independent of checkout line endings.
	raw = bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
	file, err := parser.ParseFile(token.NewFileSet(), "identity.go", raw, 0)
	if err != nil {
		t.Fatal(err)
	}
	branches := 0
	assignments := 0
	ast.Inspect(file, func(node ast.Node) bool {
		if assignment, ok := node.(*ast.AssignStmt); ok {
			for _, lhs := range assignment.Lhs {
				if target, ok := lhs.(*ast.Ident); ok && target.Name == "shutdown" {
					assignments++
				}
			}
		}
		branch, ok := node.(*ast.IfStmt)
		if !ok {
			return true
		}
		constructsShutdown := false
		ast.Inspect(branch, func(inner ast.Node) bool {
			literal, ok := inner.(*ast.CompositeLit)
			if ok {
				kind, ok := literal.Type.(*ast.Ident)
				constructsShutdown = constructsShutdown || ok && kind.Name == "postgresShutdown"
			}
			return true
		})
		if !constructsShutdown {
			return true
		}
		branches++
		flag, ok := branch.Cond.(*ast.SelectorExpr)
		if !ok || flag.Sel.Name != "PostgresFastShutdown" {
			t.Error("shutdown construction must be guarded only by entry.Switches.PostgresFastShutdown; cell-name comparisons are forbidden")
			return true
		}
		switches, ok := flag.X.(*ast.SelectorExpr)
		if !ok || switches.Sel.Name != "Switches" {
			t.Error("shutdown construction must read the catalog switches")
			return true
		}
		entry, ok := switches.X.(*ast.Ident)
		if !ok || entry.Name != "entry" || branch.Init != nil || branch.Else != nil {
			t.Error("shutdown construction must have one direct catalog-switch guard")
		}
		return true
	})
	if branches != 1 {
		t.Fatal("shutdown construction must have exactly one catalog-switch branch")
	}
	if assignments != 1 {
		t.Fatal("shutdown must be assigned exactly once by the catalog-switch branch; later cell-name overrides are forbidden")
	}
}

func TestPostgresShutdownServingWrapper(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Fatal("NOT RUN: helm is required for the serving shutdown wrapper contract")
	}
	root := repoRoot(t)
	charts, err := loadChartPins(root)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := loadCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	type primarySettings struct {
		Grace int            `yaml:"terminationGracePeriodSeconds"`
		Hooks map[string]any `yaml:"lifecycleHooks"`
	}
	renderPrimary := func(t *testing.T, name string) primarySettings {
		t.Helper()
		body, err := generateCell(root, name, cfg, charts)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "values.yaml")
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("helm", "template", "shutdown-test", filepath.Join(root, ".gitops/charts/apps"), "--values", path, "--show-only", "templates/civo-postgres.yaml")
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatal("helm template failed for generated shutdown fixture")
		}
		var application struct {
			Spec struct {
				Source struct {
					Helm struct {
						Values string `yaml:"values"`
					} `yaml:"helm"`
				} `yaml:"source"`
			} `yaml:"spec"`
		}
		if err := yaml.Unmarshal(output, &application); err != nil {
			t.Fatal("cannot parse PostgreSQL Application")
		}
		var child struct {
			Primary primarySettings `yaml:"primary"`
		}
		if err := yaml.Unmarshal([]byte(application.Spec.Source.Helm.Values), &child); err != nil {
			t.Fatal("cannot parse PostgreSQL child values")
		}
		return child.Primary
	}
	backup := renderPrimary(t, "civo-sandbox-use1-backup")
	if backup.Grace != 240 || len(backup.Hooks) != 1 || backup.Hooks["preStop"] == nil {
		t.Fatal("backup baseline must render 240 seconds and exactly one preStop hook")
	}
	for _, name := range civoRollCells(t) {
		if cfg.Cells[name].Role != "serving" {
			continue
		}
		t.Run(name, func(t *testing.T) {
			entry := cfg.Cells[name]
			entry.Switches.PostgresFastShutdown = false
			cfg.Cells[name] = entry
			disabled := renderPrimary(t, name)
			if disabled.Grace != 0 || len(disabled.Hooks) != 0 {
				t.Fatal("disabled serving fixture must omit shutdown overrides")
			}
			entry.Switches.PostgresFastShutdown = true
			cfg.Cells[name] = entry
			enabled := renderPrimary(t, name)
			if !reflect.DeepEqual(enabled, backup) {
				t.Fatal("enabled serving fixture must render the backup cell's identical grace and preStop hook")
			}
		})
	}
}
