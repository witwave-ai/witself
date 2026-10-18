package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// TestExportTransactionApplicationNamePostgres checks this classification against
// the real account-row lock held by each export kind.
var exportNamesLockingAccountRow = map[string]bool{
	"witself-export-account":    true,
	"witself-export-backup":     false,
	"witself-export-evacuation": true,
	"witself-export-self":       false,
}

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
	expression := func(t *testing.T, name string) string {
		t.Helper()
		var expressions []string
		for _, group := range rules.Groups {
			for _, candidate := range group.Rules {
				if candidate.Alert == name {
					expressions = append(expressions, candidate.Expr)
				}
			}
		}
		if len(expressions) != 1 {
			t.Fatalf("found %d %s rules, want exactly one", len(expressions), name)
		}
		return expressions[0]
	}
	matcherForNames := func(prefix string, names []string) string {
		var suffixes []string
		seen := make(map[string]bool)
		for _, name := range names {
			if !strings.HasPrefix(name, prefix) || strings.TrimPrefix(name, prefix) == "" {
				t.Fatalf("invalid application name %q for prefix %q", name, prefix)
			}
			if seen[name] {
				t.Fatalf("duplicate application name %q", name)
			}
			seen[name] = true
			suffixes = append(suffixes, strings.TrimPrefix(name, prefix))
		}
		sort.Strings(suffixes)
		return prefix + "(" + strings.Join(suffixes, "|") + ")"
	}

	// Keep selectors as sets of triples: matcher order is immaterial, but
	// missing, extra, duplicate or malformed matchers must fail the guard.
	type matcher struct{ label, op, value string }
	type matcherSet map[matcher]bool
	selectorRE := regexp.MustCompile(`pg_stat_activity_max_tx_duration\s*\{([^}]*)\}`)
	matcherRE := regexp.MustCompile(`^\s*([a-zA-Z_][a-zA-Z0-9_]*)\s*(=~|!~|!=|=)\s*("(?:[^"\\]|\\.)*")\s*`)
	selectors := func(t *testing.T, expr string) []matcherSet {
		t.Helper()
		var sets []matcherSet
		for _, selector := range selectorRE.FindAllStringSubmatch(expr, -1) {
			set := matcherSet{}
			remaining := selector[1]
			for strings.TrimSpace(remaining) != "" {
				parts := matcherRE.FindStringSubmatch(remaining)
				if parts == nil {
					t.Fatalf("malformed selector matchers %q", remaining)
				}
				value, err := strconv.Unquote(parts[3])
				if err != nil {
					t.Fatal(err)
				}
				entry := matcher{parts[1], parts[2], value}
				if set[entry] {
					t.Fatalf("duplicate selector matcher %v", entry)
				}
				set[entry] = true
				remaining = remaining[len(parts[0]):]
				if remaining == "" {
					break
				}
				if !strings.HasPrefix(remaining, ",") {
					t.Fatalf("missing matcher separator in %q", remaining)
				}
				remaining = remaining[1:]
			}
			sets = append(sets, set)
		}
		return sets
	}
	base := func(extra ...matcher) matcherSet {
		set := matcherSet{
			{"namespace", "=", "witself"}:                  true,
			{"service", "=", "witself-postgresql-metrics"}: true,
		}
		for _, entry := range extra {
			set[entry] = true
		}
		return set
	}

	families := []struct {
		prefix, file, rule string
		names              []string
		matcher            string
	}{
		{prefix: "witself-export-", file: "export.go", rule: "WitselfPostgreSQLExportTransactionAgeHigh", names: exportApplicationNames[:]},
		{prefix: "witself-import-", file: "import.go", rule: "WitselfPostgreSQLImportTransactionAgeHigh", names: importApplicationNames[:]},
	}
	allNames := make(map[string]bool)
	for i := range families {
		family := &families[i]
		family.matcher = matcherForNames(family.prefix, family.names)
		for _, name := range family.names {
			allNames[name] = true
		}
	}
	for _, family := range families {
		t.Run(family.rule, func(t *testing.T) {
			expr := expression(t, family.rule)
			got := selectors(t, expr)
			want := []matcherSet{base(matcher{"application_name", "=~", family.matcher})}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("family rule selectors = %v, want %v", got, want)
			}
			shape := selectorRE.ReplaceAllString(expr, "SERIES")
			if !regexp.MustCompile(`^\s*max\s+by\s*\(\s*namespace\s*,\s*job\s*,\s*instance\s*,\s*application_name\s*\)\s*\(\s*SERIES\s*\)\s*>\s*2700\s*$`).MatchString(shape) {
				t.Error("family rule must directly aggregate its single selector by namespace, job, instance, application_name and compare > 2700")
			}
			// Prometheus fully anchors its RE2 matchers; Go's regexp uses RE2 syntax.
			re := regexp.MustCompile("^(?:" + family.matcher + ")$")
			for _, name := range family.names {
				if !re.MatchString(name) {
					t.Errorf("matcher excludes application name %q", name)
				}
			}
			for _, other := range families {
				if other.prefix == family.prefix {
					continue
				}
				for _, name := range other.names {
					if re.MatchString(name) {
						t.Errorf("matcher includes other family's name %q", name)
					}
				}
			}
			for _, name := range []string{"", "witself-server", "witself-export-other", "witself-export-backup-old", "pg_dump", "pg_dumpall", "witself-import", "witself-import-", "witself-import-other", "witself-import-evacuation-old", "witself-export-import"} {
				if re.MatchString(name) {
					t.Errorf("matcher includes non-family application name %q", name)
				}
			}
		})
	}

	if len(exportNamesLockingAccountRow) != len(exportApplicationNames) {
		t.Fatal("account-row lock classification does not cover exactly the export names")
	}
	for _, name := range exportApplicationNames {
		_, ok := exportNamesLockingAccountRow[name]
		if !ok {
			t.Fatalf("missing account-row lock classification for %q", name)
		}
	}
	t.Run("WitselfPostgreSQLTransactionAgeHigh", func(t *testing.T) {
		expr := expression(t, "WitselfPostgreSQLTransactionAgeHigh")
		exportExclusion := matcher{"application_name", "!~", families[0].matcher}
		importExclusion := matcher{"application_name", "!~", families[1].matcher}
		want := []matcherSet{base(exportExclusion, importExclusion)}
		got := selectors(t, expr)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("general rule selectors = %v, want %v", got, want)
		}
		// The closed export/import sets have distinct prefixes. Their exact
		// positive family selectors and this single complementary selector
		// partition every in-scope series, including missing application names
		// and every wait_event_type. No other series can suppress a Lock waiter.
		shape := selectorRE.ReplaceAllString(expr, "SERIES")
		if !regexp.MustCompile(`^\s*max\s+by\s*\(\s*namespace\s*,\s*job\s*,\s*instance\s*\)\s*\(\s*SERIES\s*\)\s*>\s*300\s*$`).MatchString(shape) {
			t.Error("general rule must directly aggregate its single selector by namespace, job, instance and compare > 300")
		}
	})

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
			if !allNames[got] || got != want {
				t.Errorf("combination %03b = %q, want listed name %q", bits, got, want)
			}
			if exportNamesLockingAccountRow[got] != (!options.backup && !options.self) {
				t.Errorf("combination %03b account-row lock classification differs from export options", bits)
			}
		}
	})
	t.Run("importApplicationName", func(t *testing.T) {
		for bits := 0; bits < 4; bits++ {
			options := accountImportOptions{backupValidation: bits&1 != 0}
			if bits&2 != 0 {
				options.evacuationID = "test-evacuation"
			}
			want := "witself-import-account"
			if options.backupValidation {
				want = "witself-import-validation"
			} else if options.evacuationID != "" {
				want = "witself-import-evacuation"
			}
			if got := importApplicationName(options); !allNames[got] || got != want {
				t.Errorf("combination %02b = %q, want listed name %q", bits, got, want)
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
				for _, family := range families {
					if strings.HasPrefix(value, family.prefix) {
						if !allNames[value] {
							t.Errorf("%s: unlisted application name %q", fset.Position(literal.Pos()), value)
						}
						if path != family.file {
							t.Errorf("%s: %s application name literal belongs only in %s", fset.Position(literal.Pos()), family.prefix, family.file)
						}
					}
				}
				return true
			})
		}
	})
}
