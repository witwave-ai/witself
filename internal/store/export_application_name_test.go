package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestExportApplicationNamesMatchPostgreSQLAlertRules(t *testing.T) {
	body, err := os.ReadFile("../../.gitops/charts/platform/files/postgresql.rules.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var rules struct {
		Groups []struct {
			Rules []struct {
				Alert string `yaml:"alert"`
				Expr  string `yaml:"expr"`
			} `yaml:"rules"`
		} `yaml:"groups"`
	}
	if err := yaml.Unmarshal(body, &rules); err != nil {
		t.Fatal(err)
	}

	const prefix = "witself-export-"
	names := make(map[string]bool, len(exportApplicationNames))
	var suffixes []string
	for _, name := range exportApplicationNames {
		if !strings.HasPrefix(name, prefix) || strings.TrimPrefix(name, prefix) == "" {
			t.Fatalf("invalid export application name %q", name)
		}
		if names[name] {
			t.Fatalf("duplicate export application name %q", name)
		}
		names[name] = true
		suffixes = append(suffixes, strings.TrimPrefix(name, prefix))
	}
	sort.Strings(suffixes)
	wantMatcher := prefix + "(" + strings.Join(suffixes, "|") + ")"
	for _, rule := range []struct {
		name     string
		operator string
	}{
		{"WitselfPostgreSQLTransactionAgeHigh", "!~"},
		{"WitselfPostgreSQLExportTransactionAgeHigh", "=~"},
	} {
		t.Run(rule.name, func(t *testing.T) {
			var expressions []string
			for _, group := range rules.Groups {
				for _, candidate := range group.Rules {
					if candidate.Alert == rule.name {
						expressions = append(expressions, candidate.Expr)
					}
				}
			}
			if len(expressions) != 1 {
				t.Fatalf("found %d rules, want exactly one", len(expressions))
			}
			matcherRE := regexp.MustCompile(`application_name\s*` + rule.operator + `\s*"([^"\\]*(?:\\.[^"\\]*)*)"`)
			matches := matcherRE.FindAllStringSubmatch(expressions[0], -1)
			if len(matches) != 1 {
				t.Fatalf("found %d application_name%s matchers, want exactly one", len(matches), rule.operator)
			}
			matcher := matches[0][1]
			if matcher != wantMatcher {
				t.Errorf("matcher set = %q, want %q", matcher, wantMatcher)
			}
			// Prometheus fully anchors its RE2 matchers; Go's regexp uses RE2 syntax.
			re, err := regexp.Compile("^(?:" + matcher + ")$")
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range exportApplicationNames {
				if !re.MatchString(name) {
					t.Errorf("matcher excludes export application name %q", name)
				}
			}
			for _, name := range []string{"", "witself-server", "witself-export-other", "witself-export-backup-old", "pg_dump", "pg_dumpall"} {
				if re.MatchString(name) {
					t.Errorf("matcher includes non-export application name %q", name)
				}
			}
		})
	}

	t.Run("selector", func(t *testing.T) {
		for bits := 0; bits < 8; bits++ {
			options := accountExportOptions{backup: bits&1 != 0, self: bits&2 != 0}
			if bits&4 != 0 {
				options.evacuationID = "test-evacuation"
			}
			want := "witself-export-account"
			switch {
			case options.backup:
				want = "witself-export-backup"
			case options.self:
				want = "witself-export-self"
			case options.evacuationID != "":
				want = "witself-export-evacuation"
			}
			got := exportApplicationName(options)
			if !names[got] {
				t.Errorf("combination %03b returned unlisted name %q", bits, got)
			}
			if got != want {
				t.Errorf("combination %03b = %q, want %q", bits, got, want)
			}
		}
	})

	t.Run("source_literals", func(t *testing.T) {
		paths, err := filepath.Glob("*.go")
		if err != nil {
			t.Fatal(err)
		}
		fset := token.NewFileSet()
		for _, path := range paths {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			ast.Inspect(file, func(node ast.Node) bool {
				literal, ok := node.(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					return true
				}
				value, err := strconv.Unquote(literal.Value)
				if err != nil {
					t.Errorf("%s: %v", fset.Position(literal.Pos()), err)
					return true
				}
				if strings.HasPrefix(value, prefix) {
					if !names[value] {
						t.Errorf("%s: unlisted export application name %q", fset.Position(literal.Pos()), value)
					}
					if path != "export.go" {
						t.Errorf("%s: export application name literal belongs only in export.go", fset.Position(literal.Pos()))
					}
				}
				return true
			})
		}
	})
}
