package syncharness

import (
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestHostedCommandDependencyBoundary(t *testing.T) {
	for _, command := range []string{"witself-server", "witself-worker", "witself"} {
		t.Run(command, func(t *testing.T) {
			cmd := exec.Command("go", "list", "-deps", "./cmd/"+command)
			cmd.Dir = "../.."
			cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off")
			out, err := cmd.Output()
			if err != nil {
				t.Fatal("hosted command dependency inspection failed")
			}
			for _, dependency := range strings.Fields(string(out)) {
				for _, forbidden := range []string{"/internal/syncharness", "/internal/syncmodel"} {
					if strings.HasSuffix(dependency, forbidden) || strings.Contains(dependency, forbidden+"/") {
						t.Fatal("synthetic synchronization package entered hosted dependency graph")
					}
				}
			}
		})
	}
}

func TestHarnessSupportDependencyBoundary(t *testing.T) {
	for _, pkg := range []string{"syncharness", "testenv"} {
		t.Run(pkg, func(t *testing.T) {
			cmd := exec.Command("go", "list", "-deps", "./internal/"+pkg)
			cmd.Dir = "../.."
			cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off")
			out, err := cmd.Output()
			if err != nil {
				t.Fatal("test support dependency inspection failed")
			}
			for _, dependency := range strings.Fields(string(out)) {
				if strings.HasSuffix(dependency, "/internal/store") || strings.HasSuffix(dependency, "/internal/server") {
					t.Fatal("test support imports a production store or server adapter")
				}
			}
		})
	}
}

// Boundary checks avoid fingerprints so unrelated migrations and catalog changes need no harness baseline update.
func TestHarnessMigrationBoundary(t *testing.T) {
	harnessSQL := filepath.Join("..", "store", "testdata", "sync_harness", "schema_v1.sql")
	ddl, err := os.ReadFile(harnessSQL)
	if err != nil {
		t.Fatal("harness SQL missing from test-only fixture directory")
	}
	tablePattern := regexp.MustCompile(`(?i)CREATE\s+TABLE\s+(h_[a-z0-9_]+)`)
	tables := tablePattern.FindAllSubmatch(ddl, -1)
	if len(tables) == 0 {
		t.Fatal("harness fixture table inventory missing")
	}
	forbidden := []string{"sync_harness", "syncharness", `\bh_witness_prefixes\b`}
	for _, table := range tables {
		forbidden = append(forbidden, `\b`+regexp.QuoteMeta(string(table[1]))+`\b`)
	}
	harnessReference := regexp.MustCompile(`(?i)` + strings.Join(forbidden, "|"))
	count := 0
	err = filepath.WalkDir(filepath.Join("..", "store", "migrations"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || filepath.Ext(path) != ".sql" {
			return nil
		}
		count++
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if harnessReference.MatchString(filepath.ToSlash(path)) || harnessReference.Match(body) {
			t.Error("production migration contains or references harness SQL")
		}
		return nil
	})
	if err != nil || count == 0 {
		t.Fatal("production migration inspection failed")
	}
}

func TestHarnessFeatureCatalogBoundary(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "featurestatus", "catalog.json"))
	if err != nil {
		t.Fatal("read feature catalog failed")
	}
	var catalog struct {
		Features []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"features"`
	}
	if err := json.Unmarshal(data, &catalog); err != nil || len(catalog.Features) == 0 {
		t.Fatal("invalid feature catalog")
	}
	for _, feature := range catalog.Features {
		if strings.Contains(strings.ToLower(feature.ID+" "+feature.Name), "harness") {
			t.Error("synthetic harness entered the feature catalog")
		}
	}
}
