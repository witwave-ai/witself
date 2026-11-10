package gitopsvalues

import (
	"bytes"
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
	tmpl := templates.Lookup("civo-sandbox-use1-backup.yaml.tmpl")
	if tmpl == nil {
		t.Fatal("missing backup overlay template")
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
