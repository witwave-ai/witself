package main

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const commandSurfaceDoc = "../../docs/cli-command-surface.md"

func TestCommandSurfaceDispatchMarkers(t *testing.T) {
	doc, sources := commandSurfaceInputs(t)
	if err := checkCommandSurfaceMarkers(doc, sources); err != nil {
		t.Fatal(err)
	}
}

// Exercise drift against copies of the real dispatch sources, without changing
// the worktree or executing commands that could need credentials or write state.
func TestCommandSurfaceDispatchMarkerDrift(t *testing.T) {
	doc, sources := commandSurfaceInputs(t)
	for _, binary := range []string{"witself", "witself-admin"} {
		for _, mutation := range []struct {
			name, before, after string
		}{
			{"added family", `case "help", "--help", "-h":`, "case \"surface-regression\":\n return 0\n case \"help\", \"--help\", \"-h\":"},
			{"removed family", `case "version", "--version", "-v":`, `case "--version", "-v":`},
			{"added alias", `case "version", "--version", "-v":`, `case "version", "--version", "-v", "ver":`},
			{"removed alias", `case "version", "--version", "-v":`, `case "version", "--version":`},
		} {
			t.Run(binary+"/"+mutation.name, func(t *testing.T) {
				changed := strings.Replace(sources[binary], mutation.before, mutation.after, 1)
				if changed == sources[binary] {
					t.Fatal("mutation did not match the actual dispatch source")
				}
				mutated := map[string]string{"witself": sources["witself"], "witself-admin": sources["witself-admin"]}
				mutated[binary] = changed
				if err := checkCommandSurfaceMarkers(doc, mutated); err == nil {
					t.Fatal("dispatch drift was accepted")
				}
			})
		}
	}
	for _, mutation := range []struct {
		name, before, after string
	}{
		{"target marked implemented", "| `witself` | `policy` | target |", "| `witself` | `policy` | implemented |"},
		{"implemented marked target", "| `witself` | `export` | implemented |", "| `witself` | `export` | target |"},
		{"undocumented family section", "## Design Goals", "## `witself surface-regression`\n\n## Design Goals"},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			changed := strings.Replace(doc, mutation.before, mutation.after, 1)
			if changed == doc {
				t.Fatal("mutation did not match the actual document")
			}
			if err := checkCommandSurfaceMarkers(changed, sources); err == nil {
				t.Fatal("documentation drift was accepted")
			}
		})
	}
}

func commandSurfaceInputs(t *testing.T) (string, map[string]string) {
	t.Helper()
	read := func(path string) string {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	sources := make(map[string]string)
	for _, binary := range []string{"witself", "witself-admin"} {
		sources[binary] = read(filepath.Join("..", binary, "main.go"))
	}
	return read(commandSurfaceDoc), sources
}

func checkCommandSurfaceMarkers(doc string, sources map[string]string) error {
	const begin = "<!-- BEGIN COMMAND FAMILY STATUS -->"
	const end = "<!-- END COMMAND FAMILY STATUS -->"
	if strings.Count(doc, begin) != 1 || strings.Count(doc, end) != 1 {
		return fmt.Errorf("%s: expected one command family marker table", commandSurfaceDoc)
	}
	_, table, _ := strings.Cut(doc, begin)
	table, _, ok := strings.Cut(table, end)
	if !ok {
		return fmt.Errorf("%s: command family table end precedes its beginning", commandSurfaceDoc)
	}
	lines := strings.Split(strings.TrimSpace(table), "\n")
	if len(lines) < 3 || lines[0] != "| Binary | Family | Marker | Aliases |" || lines[1] != "|---|---|---|---|" {
		return fmt.Errorf("%s: invalid command family table header", commandSurfaceDoc)
	}
	// Map every spelling (including aliases) to the family's marker.
	markers := make(map[string]map[string]string)
	for binary := range sources {
		markers[binary] = make(map[string]string)
	}
	for _, line := range lines[2:] {
		cells := strings.Split(line, "|")
		if len(cells) != 6 || cells[0] != "" || cells[5] != "" {
			return fmt.Errorf("%s: invalid marker row %q", commandSurfaceDoc, line)
		}
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		binary, family := strings.Trim(cells[1], "`"), strings.Trim(cells[2], "`")
		marker := cells[3]
		if markers[binary] == nil || (marker != "implemented" && marker != "target") || family == "" || strings.ContainsAny(family, " `\t") {
			return fmt.Errorf("%s: invalid binary, family or marker in %q", commandSurfaceDoc, line)
		}
		names := []string{family}
		if cells[4] != "—" {
			if marker == "target" {
				return fmt.Errorf("%s: target family %s %s cannot declare implemented aliases", commandSurfaceDoc, binary, family)
			}
			for _, alias := range strings.Split(cells[4], ",") {
				alias = strings.Trim(strings.TrimSpace(alias), "`")
				if alias == "" || strings.ContainsAny(alias, " `\t") {
					return fmt.Errorf("%s: invalid alias in %q", commandSurfaceDoc, line)
				}
				names = append(names, alias)
			}
		}
		for _, name := range names {
			if _, exists := markers[binary][name]; exists {
				return fmt.Errorf("%s: duplicate marker for %s %s", commandSurfaceDoc, binary, name)
			}
			markers[binary][name] = marker
		}
	}
	var problems []string
	for binary, source := range sources {
		dispatch, err := commandSurfaceDispatch(source)
		if err != nil {
			return fmt.Errorf("%s dispatch: %w", binary, err)
		}
		for name := range dispatch {
			if markers[binary][name] != "implemented" {
				problems = append(problems, fmt.Sprintf("%s %s is dispatched but has no implemented marker", binary, name))
			}
		}
		for name, marker := range markers[binary] {
			if marker == "implemented" && !dispatch[name] {
				problems = append(problems, fmt.Sprintf("%s %s is marked implemented but has no dispatch entry", binary, name))
			}
		}
	}
	// A new family section also needs an explicit marker, even if target-only.
	headings := regexp.MustCompile("(?m)^## `(?P<binary>witself(?:-admin)?) (?P<family>[a-z_][a-z0-9_-]*)(?:[ `])")
	for _, match := range headings.FindAllStringSubmatch(doc, -1) {
		if markers[match[1]][match[2]] == "" {
			problems = append(problems, fmt.Sprintf("%s %s section has no family marker", match[1], match[2]))
		}
	}
	if len(problems) > 0 {
		slices.Sort(problems)
		return fmt.Errorf("%s dispatch markers have drifted:\n%s", commandSurfaceDoc, strings.Join(problems, "\n"))
	}
	return nil
}

// Read only run's switch args[0], not nested verb switches, help strings or
// comments. Fail closed if the dispatch shape changes so the guard gets updated.
func commandSurfaceDispatch(source string) (map[string]bool, error) {
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", source, 0)
	if err != nil {
		return nil, err
	}
	var dispatch *ast.SwitchStmt
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "run" || fn.Recv != nil || fn.Body == nil {
			continue
		}
		for _, stmt := range fn.Body.List {
			sw, ok := stmt.(*ast.SwitchStmt)
			if !ok {
				continue
			}
			index, ok := sw.Tag.(*ast.IndexExpr)
			if !ok {
				continue
			}
			args, argsOK := index.X.(*ast.Ident)
			zero, zeroOK := index.Index.(*ast.BasicLit)
			if !argsOK || args.Name != "args" || !zeroOK || zero.Kind != token.INT || zero.Value != "0" {
				continue
			}
			if dispatch != nil {
				return nil, errors.New("multiple run dispatch switches")
			}
			dispatch = sw
		}
	}
	if dispatch == nil {
		return nil, errors.New("missing run switch args[0]")
	}
	names := make(map[string]bool)
	for _, stmt := range dispatch.Body.List {
		for _, expr := range stmt.(*ast.CaseClause).List {
			literal, ok := expr.(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return nil, errors.New("dispatch case is not a string literal")
			}
			name, err := strconv.Unquote(literal.Value)
			if err != nil || name == "" || names[name] {
				return nil, fmt.Errorf("invalid or duplicate dispatch spelling %q", literal.Value)
			}
			names[name] = true
		}
	}
	if len(names) == 0 {
		return nil, errors.New("empty run dispatch switch")
	}
	return names, nil
}
