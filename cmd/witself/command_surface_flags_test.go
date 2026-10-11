package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	commandSurfaceProbeA            = "--witself-surface-probe-a="
	commandSurfaceProbeB            = "--witself-surface-probe-b="
	commandSurfaceCommonBegin       = "<!-- BEGIN COMMON FLAGS -->"
	commandSurfaceCommonEnd         = "<!-- END COMMON FLAGS -->"
	commandSurfaceFlagHeaderProblem = `flag table header must be "| Flag | Description |" or "| Target flag | Description |"`
)

// billing dispatches leading flags to billing show (cmd/witself/billing.go:20-24).
var commandSurfaceDefaultVerbGroups = []string{"billing"}

// run dispatches version directly without a FlagSet (cmd/witself/main.go:132-134).
const commandSurfaceNoFlagSetCommand = "version"

type commandSurfaceProbe func([]string) (string, error)
type commandSurfaceFlagInput struct {
	Doc, File         string
	Families          map[string]string
	DefaultVerbGroups []string
	Probe             commandSurfaceProbe
}
type commandSurfaceFlagReport struct {
	Problems                                                                        []string
	ResolvedHeadings, ShippedFlagsVerified, TargetFlagsChecked, CommonFlagsVerified int
}
type commandSurfaceFlagRow struct {
	Line  int
	Names []string
	Text  string
}
type commandSurfaceFlagTable struct {
	Line, Heading  int
	Target, Common bool
	Rows           []commandSurfaceFlagRow
}
type commandSurfaceFlagHeading struct {
	Line            int
	Path            []string
	Family          string
	Target, Command bool
	Tables          []int
}
type commandSurfaceParsed struct {
	Headings     []commandSurfaceFlagHeading
	Tables       []commandSurfaceFlagTable
	CommonBlocks int
}

// Keep original line positions while applying the same fence rule as the
// existing family marker guard. A longer matching closing fence is valid.
func commandSurfaceOutsideFences(doc string) []string {
	lines := strings.Split(doc, "\n")
	pattern := regexp.MustCompile("^ {0,3}(`{3,}|~{3,})(.*)$")
	fence := ""
	for i, line := range lines {
		match := pattern.FindStringSubmatch(line)
		if fence != "" {
			if match != nil && match[1][0] == fence[0] && len(match[1]) >= len(fence) && strings.TrimSpace(match[2]) == "" {
				fence = ""
			}
			lines[i] = ""
		} else if match != nil {
			fence = match[1]
			lines[i] = ""
		}
	}
	return lines
}

func commandSurfaceFlagFamilies(doc string) map[string]string {
	families := make(map[string]string)
	active := false
	for _, line := range commandSurfaceOutsideFences(doc) {
		switch line {
		case "<!-- BEGIN COMMAND FAMILY STATUS -->":
			active = true
		case "<!-- END COMMAND FAMILY STATUS -->":
			active = false
		default:
			if !active {
				continue
			}
			cells := commandSurfaceFlagCells(line)
			if len(cells) >= 3 && strings.TrimSpace(cells[0]) == "`witself`" {
				name := strings.Trim(strings.TrimSpace(cells[1]), "`")
				marker := strings.TrimSpace(cells[2])
				if marker == "implemented" || marker == "target" {
					families[name] = marker
				}
			}
		}
	}
	return families
}

// A GitHub table splits even inside a code span; only a preceding backslash
// protects a pipe. The first and last empty cells are structural, not content.
func commandSurfaceFlagCells(line string) []string {
	var cells []string
	start := 0
	for i := 0; i < len(line); i++ {
		if line[i] == '|' && (i == 0 || line[i-1] != '\\') {
			cells = append(cells, strings.ReplaceAll(line[start:i], `\|`, "|"))
			start = i + 1
		}
	}
	cells = append(cells, strings.ReplaceAll(line[start:], `\|`, "|"))
	if strings.HasPrefix(line, "|") {
		cells = cells[1:]
	}
	if len(cells) > 0 && strings.TrimSpace(cells[len(cells)-1]) == "" {
		cells = cells[:len(cells)-1]
	}
	return cells
}

func commandSurfaceFlagNames(row string) []string {
	cells := commandSurfaceFlagCells(row)
	if len(cells) == 0 {
		return nil
	}
	spans := regexp.MustCompile("`([^`]+)`").FindAllStringSubmatch(cells[0], -1)
	flagName := regexp.MustCompile(`^--?([A-Za-z][A-Za-z0-9_-]*)$`)
	var names []string
	for _, span := range spans {
		for _, part := range strings.FieldsFunc(span[1], func(r rune) bool { return r == ',' || r == '/' }) {
			words := strings.Fields(part)
			if len(words) == 0 {
				continue
			}
			if match := flagName.FindStringSubmatch(words[0]); match != nil {
				names = append(names, match[1])
			}
		}
	}
	return names
}

func commandSurfaceParse(doc, file string, families map[string]string) (commandSurfaceParsed, []string) {
	var parsed commandSurfaceParsed
	var problems []string
	lines := commandSurfaceOutsideFences(doc)
	headingPattern := regexp.MustCompile("^ {0,3}#{1,6} ")
	commandPattern := regexp.MustCompile("^ {0,3}#{2,6} `witself ([^`]+)`(.*)$")
	firstWord := regexp.MustCompile(`^[a-z_][a-z0-9_-]*$`)
	laterWord := regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	delimiter := regexp.MustCompile(`^\|( *:?-+:? *\|)+ *$`)
	heading := -1
	common := false
	beginCount, endCount := 0, 0
	badOrder := false
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if line == commandSurfaceCommonBegin {
			beginCount++
			common = true
			continue
		}
		if line == commandSurfaceCommonEnd {
			endCount++
			if !common {
				badOrder = true
			}
			common = false
			continue
		}
		if headingPattern.MatchString(line) {
			h := commandSurfaceFlagHeading{Line: i + 1}
			if match := commandPattern.FindStringSubmatch(line); match != nil {
				for _, word := range strings.Fields(match[1]) {
					valid := laterWord.MatchString(word)
					if len(h.Path) == 0 {
						valid = firstWord.MatchString(word)
					}
					if !valid {
						break
					}
					h.Path = append(h.Path, word)
				}
				if len(h.Path) > 0 {
					h.Family = h.Path[0]
					if marker := families[h.Family]; marker != "" {
						h.Command = true
						h.Target = marker == "target" || strings.Contains(match[2], " (target; not implemented)") || strings.Contains(match[2], " (roadmap; not implemented)")
					}
				}
			}
			parsed.Headings = append(parsed.Headings, h)
			heading = len(parsed.Headings) - 1
		}
		if !strings.HasPrefix(line, "|") || i+1 >= len(lines) || !delimiter.MatchString(lines[i+1]) {
			continue
		}
		cells := commandSurfaceFlagCells(line)
		if len(cells) == 0 || !strings.Contains(strings.ToLower(cells[0]), "flag") {
			continue
		}
		table := commandSurfaceFlagTable{Line: i + 1, Heading: heading, Target: strings.Contains(strings.ToLower(cells[0]), "target"), Common: common}
		if (line != "| Flag | Description |" && line != "| Target flag | Description |") || lines[i+1] != "|---|---|" {
			problems = append(problems, fmt.Sprintf("%s:%d: %s", file, i+1, commandSurfaceFlagHeaderProblem))
		}
		i += 2
		for ; i < len(lines) && strings.HasPrefix(lines[i], "|"); i++ {
			row := commandSurfaceFlagRow{Line: i + 1, Names: commandSurfaceFlagNames(lines[i]), Text: lines[i]}
			if len(row.Names) == 0 {
				problems = append(problems, fmt.Sprintf("%s:%d: flag table row names no flag", file, i+1))
			}
			table.Rows = append(table.Rows, row)
		}
		i--
		if heading >= 0 {
			parsed.Headings[heading].Tables = append(parsed.Headings[heading].Tables, len(parsed.Tables))
		}
		parsed.Tables = append(parsed.Tables, table)
	}
	parsed.CommonBlocks = beginCount
	if beginCount != endCount || beginCount > 1 || endCount > 1 || common || badOrder {
		problems = append(problems, file+": expected at most one common flags block")
	}
	return parsed, problems
}

func commandSurfaceHeadingResolved(command, path []string) bool {
	return len(command) > 0 && slices.Equal(command, path)
}

func commandSurfaceHasLine(output, want string) bool {
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSuffix(line, "\r") == want {
			return true
		}
	}
	return false
}
func commandSurfaceUndefined(name string) string { return "flag provided but not defined: -" + name }

func commandSurfaceResolve(probe commandSurfaceProbe, defaultVerbGroups, words []string) ([]string, error) {
	if len(words) == 0 {
		return nil, nil
	}
	start := 1
	if len(words) >= 2 && slices.Contains(defaultVerbGroups, words[0]) {
		start = 2
	}
	for n := start; n <= len(words); n++ {
		path := append([]string(nil), words[:n]...)
		output, err := probe(append(path, commandSurfaceProbeA))
		if err != nil {
			return nil, err
		}
		if commandSurfaceHasLine(output, commandSurfaceUndefined("witself-surface-probe-a")) {
			return words[:n], nil
		}
	}
	return nil, nil
}

// Inconclusive output is a K9 finding; a transport error aborts the entire check.
type commandSurfaceInconclusiveError struct {
	path []string
	name string
}

func (e *commandSurfaceInconclusiveError) Error() string {
	return fmt.Sprintf("witself %s flag --%s: probe output is inconclusive", strings.Join(e.path, " "), e.name)
}
func commandSurfaceFlagState(probe commandSurfaceProbe, path []string, name string) (bool, error) {
	if name == "h" || name == "help" {
		return true, nil
	}
	argv := append(append([]string(nil), path...), "--"+name+"=", commandSurfaceProbeA)
	output, err := probe(argv)
	if err != nil {
		return false, err
	}
	if commandSurfaceHasLine(output, commandSurfaceUndefined(name)) {
		return false, nil
	}
	if commandSurfaceHasLine(output, commandSurfaceUndefined("witself-surface-probe-a")) {
		return true, nil
	}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.HasPrefix(line, `invalid value "" for flag -`+name+": ") || strings.HasPrefix(line, `invalid boolean value "" for -`+name+": ") {
			return true, nil
		}
	}
	return false, &commandSurfaceInconclusiveError{path: path, name: name}
}

func checkCommandSurfaceFlags(in commandSurfaceFlagInput) (report commandSurfaceFlagReport) {
	parsed, problems := commandSurfaceParse(in.Doc, in.File, in.Families)
	report.Problems = append(report.Problems, problems...)
	defer func() { slices.Sort(report.Problems); report.Problems = slices.Compact(report.Problems) }()
	add := func(line int, message string) {
		report.Problems = append(report.Problems, fmt.Sprintf("%s:%d: %s", in.File, line, message))
	}
	fail := func(err error) { report.Problems = append(report.Problems, "probe failed: "+err.Error()) }
	orderChecked := make(map[string]bool)
	orderOK := make(map[string]bool)
	documented := make(map[string][]string)
	for _, table := range parsed.Tables {
		if table.Target || table.Common {
			continue
		}
		if table.Heading < 0 || !parsed.Headings[table.Heading].Command {
			add(table.Line, "shipped flag table is not under an implemented witself command heading")
		}
	}
	for _, h := range parsed.Headings {
		if !h.Command || len(h.Tables) == 0 || in.Families[h.Family] == "target" {
			continue
		}
		shipped := false
		for _, index := range h.Tables {
			if !parsed.Tables[index].Target && !parsed.Tables[index].Common {
				shipped = true
			}
		}
		command, err := commandSurfaceResolve(in.Probe, in.DefaultVerbGroups, h.Path)
		if err != nil {
			fail(err)
			return report
		}
		resolved := commandSurfaceHeadingResolved(command, h.Path)
		path := strings.Join(h.Path, " ")
		if h.Target {
			if resolved {
				add(h.Line, "witself "+path+" is marked target but parses flags")
			}
			continue
		}
		if !resolved {
			if !shipped && path == commandSurfaceNoFlagSetCommand {
				continue
			}
			tableKind := "flag"
			if shipped {
				tableKind = "shipped flag"
			}
			message := "witself " + path + " has a " + tableKind + " table but does not parse flags"
			if len(command) > 0 {
				message += "; the probe resolved at witself " + strings.Join(command, " ")
			}
			add(h.Line, message)
			continue
		}
		report.ResolvedHeadings++
		if shipped {
			documented[path] = h.Path
		}
		if !orderChecked[path] {
			output, err := in.Probe(append(append([]string(nil), h.Path...), commandSurfaceProbeB, commandSurfaceProbeA))
			if err != nil {
				fail(err)
				return report
			}
			orderChecked[path] = true
			orderOK[path] = commandSurfaceHasLine(output, commandSurfaceUndefined("witself-surface-probe-b")) && !commandSurfaceHasLine(output, commandSurfaceUndefined("witself-surface-probe-a"))
			if !orderOK[path] {
				report.Problems = append(report.Problems, "witself "+path+" does not parse flags in order; the probe cannot prove flags")
			}
		}
		if !orderOK[path] {
			continue
		}
		for _, index := range h.Tables {
			table := parsed.Tables[index]
			if table.Common {
				continue
			}
			for _, row := range table.Rows {
				for _, name := range row.Names {
					if name == "h" || name == "help" {
						continue
					}
					defined, err := commandSurfaceFlagState(in.Probe, h.Path, name)
					if err != nil {
						var inconclusive *commandSurfaceInconclusiveError
						if errors.As(err, &inconclusive) {
							report.Problems = append(report.Problems, err.Error())
							continue
						}
						fail(err)
						return report
					}
					if table.Target {
						if defined {
							add(row.Line, "witself "+path+" lists target flag --"+name+" but the flag exists")
						} else {
							report.TargetFlagsChecked++
						}
					} else {
						if !defined {
							add(row.Line, "witself "+path+" has no flag --"+name)
						} else {
							report.ShippedFlagsVerified++
						}
					}
				}
			}
		}
	}
	paths := make([]string, 0, len(documented))
	for path := range documented {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	for _, table := range parsed.Tables {
		if !table.Common {
			continue
		}
		for _, row := range table.Rows {
			for _, name := range row.Names {
				if name == "h" || name == "help" {
					continue
				}
				found := false
				for _, path := range paths {
					if !orderOK[path] {
						continue
					}
					defined, err := commandSurfaceFlagState(in.Probe, documented[path], name)
					if err != nil {
						var inconclusive *commandSurfaceInconclusiveError
						if errors.As(err, &inconclusive) {
							report.Problems = append(report.Problems, err.Error())
							continue
						}
						fail(err)
						return report
					}
					if defined {
						found = true
						break
					}
				}
				if found {
					report.CommonFlagsVerified++
				} else {
					add(row.Line, "common flag --"+name+" is not defined on any documented command")
				}
			}
		}
	}
	return report
}

type commandSurfaceFakeCLI struct {
	Args    [][]string
	Outputs []string
}

func (fake *commandSurfaceFakeCLI) Probe(argv []string) (string, error) {
	fake.Args = append(fake.Args, append([]string(nil), argv...))
	var output bytes.Buffer
	if len(argv) == 0 {
		return "", nil
	}
	args := argv[1:]
	fs := flag.NewFlagSet(argv[0], flag.ContinueOnError)
	fs.SetOutput(&output)
	unknown := false
	switch argv[0] {
	case "alpha":
		fs.String("one", "", "")
		fs.Bool("two", false, "")
		fs.Func("three", "", func(value string) error {
			if value == "" {
				return errors.New("empty value")
			}
			return nil
		})
		fs.Int("four", 0, "")
	case "beta":
		if len(args) == 0 || args[0] != "gamma" {
			output.WriteString("witself beta: unknown subcommand\n")
			unknown = true
		} else {
			args = args[1:]
			fs.String("five", "", "")
		}
	case "eps":
		if len(args) == 0 || strings.HasPrefix(args[0], "-") {
			output.WriteString("unknown runtime\n")
			unknown = true
		} else {
			args = args[1:]
			fs.Bool("six", false, "")
		}
	case "zeta":
		fs.String("seven", "", "")
		args = append([]string(nil), args...)
		slices.Reverse(args)
	case "ver":
		output.WriteString("ver 1\n")
		unknown = true
	case "mute":
		fs.SetOutput(io.Discard)
		fs.String("eight", "", "")
	case "delta":
		verb := ""
		if len(args) > 0 {
			if strings.HasPrefix(args[0], "-") {
				verb = "show"
			} else {
				verb = args[0]
				args = args[1:]
			}
		}
		switch verb {
		case "show":
			fs.Bool("json", false, "")
		case "list":
			fs.Int("limit", 0, "")
		default:
			output.WriteString("witself delta: unknown subcommand\n")
			unknown = true
		}
	case "_hooks":
		fs.String("ten", "", "")
	default:
		output.WriteString("unknown command\n")
		unknown = true
	}
	if !unknown {
		_ = fs.Parse(args)
	}
	fake.Outputs = append(fake.Outputs, output.String())
	return output.String(), nil
}
func commandSurfaceFixtureTable(target bool, names ...string) string {
	header := "Flag"
	if target {
		header = "Target flag"
	}
	table := "| " + header + " | Description |\n|---|---|\n"
	for _, name := range names {
		table += "| `" + name + "` | x |\n"
	}
	return table
}
func commandSurfaceFakeInput(doc string, probe commandSurfaceProbe) commandSurfaceFlagInput {
	families := make(map[string]string)
	for _, name := range []string{"alpha", "beta", "eps", "zeta", "ver", "mute", "delta", "_hooks"} {
		families[name] = "implemented"
	}
	return commandSurfaceFlagInput{Doc: doc, File: "fixture.md", Families: families, DefaultVerbGroups: []string{"delta"}, Probe: probe}
}
func commandSurfaceWantProblems(t *testing.T, report commandSurfaceFlagReport, want ...string) {
	t.Helper()
	if len(report.Problems) != len(want) {
		t.Fatalf("problems = %q, want %q", report.Problems, want)
	}
	for _, message := range want {
		found := false
		for _, problem := range report.Problems {
			if strings.Contains(problem, message) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("problems = %q, missing %q", report.Problems, message)
		}
	}
}
func commandSurfaceFixtureHeading(path string) string { return "### `witself " + path + "`\n\n" }

func TestCommandSurfaceFlagProbeRules(t *testing.T) {
	t.Run("a_defined_and_builtin", func(t *testing.T) {
		fake := &commandSurfaceFakeCLI{}
		doc := commandSurfaceFixtureHeading("alpha") + commandSurfaceFixtureTable(false, "--one", "--two", "--three", "--four", "-h", "--help")
		commandSurfaceWantProblems(t, checkCommandSurfaceFlags(commandSurfaceFakeInput(doc, fake.Probe)))
		for _, argv := range fake.Args {
			if slices.Contains(argv, "--help=") || slices.Contains(argv, "--h=") {
				t.Fatalf("built-in help was probed: %q", argv)
			}
		}
	})
	t.Run("b_missing", func(t *testing.T) {
		fake := &commandSurfaceFakeCLI{}
		doc := commandSurfaceFixtureHeading("alpha") + commandSurfaceFixtureTable(false, "--nine")
		got := checkCommandSurfaceFlags(commandSurfaceFakeInput(doc, fake.Probe)).Problems
		want := []string{"fixture.md:5: witself alpha has no flag --nine"}
		if !slices.Equal(got, want) {
			t.Fatalf("problems = %q, want %q", got, want)
		}
	})
	t.Run("c_target_row", func(t *testing.T) {
		fake := &commandSurfaceFakeCLI{}
		doc := commandSurfaceFixtureHeading("alpha") + commandSurfaceFixtureTable(true, "--one")
		commandSurfaceWantProblems(t, checkCommandSurfaceFlags(commandSurfaceFakeInput(doc, fake.Probe)), "witself alpha lists target flag --one but the flag exists")
	})
	t.Run("d_verbs", func(t *testing.T) {
		for _, test := range []struct {
			path, suffix string
			want         []string
		}{
			{"beta gamma", "", nil},
			{"beta omega", "", []string{"witself beta omega has a shipped flag table but does not parse flags"}},
			{"beta omega", " (target; not implemented)", nil},
			{"beta gamma", " (target; not implemented)", []string{"witself beta gamma is marked target but parses flags"}},
		} {
			t.Run(test.path+test.suffix, func(t *testing.T) {
				fake := &commandSurfaceFakeCLI{}
				doc := "### `witself " + test.path + "`" + test.suffix + "\n\n" + commandSurfaceFixtureTable(false, "--five")
				commandSurfaceWantProblems(t, checkCommandSurfaceFlags(commandSurfaceFakeInput(doc, fake.Probe)), test.want...)
			})
		}
	})
	t.Run("e_runtime", func(t *testing.T) {
		fake := &commandSurfaceFakeCLI{}
		doc := commandSurfaceFixtureHeading("eps codex") + commandSurfaceFixtureTable(false, "--six")
		commandSurfaceWantProblems(t, checkCommandSurfaceFlags(commandSurfaceFakeInput(doc, fake.Probe)))
		if len(fake.Args) < 2 || !slices.Equal(fake.Args[0], []string{"eps", commandSurfaceProbeA}) || !slices.Equal(fake.Args[1], []string{"eps", "codex", commandSurfaceProbeA}) {
			t.Fatalf("resolution order = %q", fake.Args)
		}
	})
	t.Run("f_order", func(t *testing.T) {
		fake := &commandSurfaceFakeCLI{}
		doc := commandSurfaceFixtureHeading("zeta") + commandSurfaceFixtureTable(false, "--seven")
		commandSurfaceWantProblems(t, checkCommandSurfaceFlags(commandSurfaceFakeInput(doc, fake.Probe)), "witself zeta does not parse flags in order; the probe cannot prove flags")
		for _, argv := range fake.Args {
			if slices.Contains(argv, "--seven=") {
				t.Fatalf("flag checked after order failure: %q", argv)
			}
		}
	})
	t.Run("g_muted", func(t *testing.T) {
		fake := &commandSurfaceFakeCLI{}
		doc := commandSurfaceFixtureHeading("mute") + commandSurfaceFixtureTable(false, "--eight")
		commandSurfaceWantProblems(t, checkCommandSurfaceFlags(commandSurfaceFakeInput(doc, fake.Probe)), "witself mute has a shipped flag table but does not parse flags")
	})
	t.Run("h_inconclusive", func(t *testing.T) {
		fake := &commandSurfaceFakeCLI{}
		probe := func(argv []string) (string, error) {
			if slices.Contains(argv, "--four=") {
				return "garbage", nil
			}
			return fake.Probe(argv)
		}
		doc := commandSurfaceFixtureHeading("alpha") + commandSurfaceFixtureTable(false, "--four")
		commandSurfaceWantProblems(t, checkCommandSurfaceFlags(commandSurfaceFakeInput(doc, probe)), "witself alpha flag --four: probe output is inconclusive")
	})
	t.Run("i_common", func(t *testing.T) {
		for _, test := range []struct {
			name string
			want []string
		}{
			{"--two", nil}, {"--zzz", []string{"common flag --zzz is not defined on any documented command"}},
		} {
			fake := &commandSurfaceFakeCLI{}
			doc := commandSurfaceCommonBegin + "\n" + commandSurfaceFixtureTable(false, test.name) + commandSurfaceCommonEnd + "\n" + commandSurfaceFixtureHeading("alpha") + commandSurfaceFixtureTable(false, "--one")
			commandSurfaceWantProblems(t, checkCommandSurfaceFlags(commandSurfaceFakeInput(doc, fake.Probe)), test.want...)
		}
		t.Run("order_failure", func(t *testing.T) {
			fake := &commandSurfaceFakeCLI{}
			doc := commandSurfaceCommonBegin + "\n" + commandSurfaceFixtureTable(false, "--zzz") + commandSurfaceCommonEnd + "\n## `witself zeta`\n\n" + commandSurfaceFixtureTable(false, "--seven")
			want := []string{
				"fixture.md:4: common flag --zzz is not defined on any documented command",
				"witself zeta does not parse flags in order; the probe cannot prove flags",
			}
			if got := checkCommandSurfaceFlags(commandSurfaceFakeInput(doc, fake.Probe)).Problems; !slices.Equal(got, want) {
				t.Fatalf("problems = %q, want %q", got, want)
			}
		})
	})
	t.Run("j_placement_and_blocks", func(t *testing.T) {
		table := commandSurfaceFixtureTable(false, "--two")
		documented := commandSurfaceFixtureHeading("alpha") + commandSurfaceFixtureTable(false, "--one")
		for _, test := range []struct {
			name, doc string
			want      []string
		}{
			{"plain", "## Plain\n" + table, []string{"shipped flag table is not under an implemented witself command heading"}},
			{"common", "## Plain\n" + commandSurfaceCommonBegin + "\n" + table + commandSurfaceCommonEnd + "\n" + documented, nil},
			{"duplicate", commandSurfaceCommonBegin + "\n" + table + commandSurfaceCommonEnd + "\n" + commandSurfaceCommonBegin + "\n" + table + commandSurfaceCommonEnd + "\n" + documented, []string{"expected at most one common flags block"}},
			{"reversed", commandSurfaceCommonEnd + "\n" + commandSurfaceCommonBegin + "\n", []string{"expected at most one common flags block"}},
			{"unfinished", commandSurfaceCommonBegin + "\n", []string{"expected at most one common flags block"}},
		} {
			t.Run(test.name, func(t *testing.T) {
				fake := &commandSurfaceFakeCLI{}
				commandSurfaceWantProblems(t, checkCommandSurfaceFlags(commandSurfaceFakeInput(test.doc, fake.Probe)), test.want...)
			})
		}
	})
	t.Run("k_invalid_values_define_flags", func(t *testing.T) {
		fake := &commandSurfaceFakeCLI{}
		for _, test := range []struct{ name, prefix string }{{"three", `invalid value "" for flag -three: `}, {"two", `invalid boolean value "" for -two: `}} {
			defined, err := commandSurfaceFlagState(fake.Probe, []string{"alpha"}, test.name)
			if err != nil || !defined {
				t.Fatalf("%s defined = %v, error = %v", test.name, defined, err)
			}
			output := fake.Outputs[len(fake.Outputs)-1]
			if !strings.Contains(output, test.prefix) || strings.Contains(output, "flag provided but not defined: -witself-surface-probe-a") {
				t.Fatalf("unexpected invalid-value output: %q", output)
			}
		}
	})
	t.Run("l_shortest_prefix", func(t *testing.T) {
		fake := &commandSurfaceFakeCLI{}
		doc := commandSurfaceFixtureHeading("alpha extra") + commandSurfaceFixtureTable(false, "--one")
		commandSurfaceWantProblems(t, checkCommandSurfaceFlags(commandSurfaceFakeInput(doc, fake.Probe)), "witself alpha extra has a shipped flag table but does not parse flags; the probe resolved at witself alpha")
		for _, argv := range fake.Args {
			if len(argv) >= 2 && argv[0] == "alpha" && argv[1] == "extra" {
				t.Fatalf("probe reached positional word: %q", argv)
			}
		}
	})
	t.Run("m_default_verb", func(t *testing.T) {
		t.Run("nested", func(t *testing.T) {
			fake := &commandSurfaceFakeCLI{}
			doc := commandSurfaceFixtureHeading("delta list") + commandSurfaceFixtureTable(false, "--limit")
			commandSurfaceWantProblems(t, checkCommandSurfaceFlags(commandSurfaceFakeInput(doc, fake.Probe)))
			for _, argv := range fake.Args {
				if slices.Equal(argv, []string{"delta", commandSurfaceProbeA}) {
					t.Fatal("default group resolved before nested verb")
				}
			}
		})
		t.Run("bare", func(t *testing.T) {
			fake := &commandSurfaceFakeCLI{}
			doc := commandSurfaceFixtureHeading("delta") + commandSurfaceFixtureTable(false, "--limit")
			commandSurfaceWantProblems(t, checkCommandSurfaceFlags(commandSurfaceFakeInput(doc, fake.Probe)), "witself delta has no flag --limit")
		})
		t.Run("without_exception", func(t *testing.T) {
			fake := &commandSurfaceFakeCLI{}
			doc := commandSurfaceFixtureHeading("delta list") + commandSurfaceFixtureTable(false, "--limit")
			in := commandSurfaceFakeInput(doc, fake.Probe)
			in.DefaultVerbGroups = nil
			commandSurfaceWantProblems(t, checkCommandSurfaceFlags(in), "witself delta list has a shipped flag table but does not parse flags; the probe resolved at witself delta")
		})
	})
	t.Run("n_probe_error", func(t *testing.T) {
		fake := &commandSurfaceFakeCLI{}
		probe := func(argv []string) (string, error) {
			if slices.Equal(argv, []string{"beta", "gamma", commandSurfaceProbeA}) {
				return "", errors.New("boom")
			}
			return fake.Probe(argv)
		}
		doc := "### `witself beta gamma` (target; not implemented)\n\n" + commandSurfaceFixtureTable(false, "--five")
		got := checkCommandSurfaceFlags(commandSurfaceFakeInput(doc, probe)).Problems
		if !slices.Equal(got, []string{"probe failed: boom"}) {
			t.Fatalf("problems = %q, want only probe failed: boom", got)
		}
	})
	t.Run("o_underscore_family", func(t *testing.T) {
		for _, test := range []struct {
			name string
			want []string
		}{{"--ten", nil}, {"--eleven", []string{"witself _hooks has no flag --eleven"}}} {
			fake := &commandSurfaceFakeCLI{}
			doc := commandSurfaceFixtureHeading("_hooks") + commandSurfaceFixtureTable(false, test.name)
			commandSurfaceWantProblems(t, checkCommandSurfaceFlags(commandSurfaceFakeInput(doc, fake.Probe)), test.want...)
		}
	})
	t.Run("p_target_only_tables", func(t *testing.T) {
		for _, test := range []struct {
			path string
			want []string
		}{
			{"beta gamma", nil},
			{"beta omega", []string{"witself beta omega has a flag table but does not parse flags"}},
			{"ver", []string{"witself ver has a flag table but does not parse flags"}},
		} {
			t.Run(test.path, func(t *testing.T) {
				fake := &commandSurfaceFakeCLI{}
				doc := commandSurfaceFixtureHeading(test.path) + commandSurfaceFixtureTable(true, "--future")
				report := checkCommandSurfaceFlags(commandSurfaceFakeInput(doc, fake.Probe))
				commandSurfaceWantProblems(t, report, test.want...)
				if test.want == nil && (report.ResolvedHeadings != 1 || report.TargetFlagsChecked != 1) {
					t.Fatalf("target-only heading was not counted: %+v", report)
				}
			})
		}
	})
}

func TestCommandSurfaceDocParsing(t *testing.T) {
	families := map[string]string{"realm": "implemented", "memory": "implemented", "_managed-hooks": "implemented", "alpha": "implemented", "future": "target"}
	t.Run("cells", func(t *testing.T) {
		for _, test := range []struct {
			cell string
			want []string
		}{
			{"`--client-runtime`, `--client-model`", []string{"client-runtime", "client-model"}},
			{"`-o, --out PATH`", []string{"o", "out"}},
			{"`--occurred-from RFC3339` / `--occurred-until RFC3339`", []string{"occurred-from", "occurred-until"}},
			{"`--a/--b`", []string{"a", "b"}},
			{"`--group-by hour\\|day`", []string{"group-by"}},
			{"`--type card|bank`", nil},
			{"--no-code-span", nil},
		} {
			t.Run(test.cell, func(t *testing.T) {
				doc := "## Plain\n| Target flag | Description |\n|---|---|\n| " + test.cell + " | x |\n"
				parsed, problems := commandSurfaceParse(doc, "fixture.md", families)
				if len(parsed.Tables) != 1 || len(parsed.Tables[0].Rows) != 1 {
					t.Fatalf("table parsing = %+v", parsed.Tables)
				}
				if names := parsed.Tables[0].Rows[0].Names; !slices.Equal(names, test.want) {
					t.Fatalf("names = %q, want %q", names, test.want)
				}
				var want []string
				if len(test.want) == 0 {
					want = []string{"fixture.md:4: flag table row names no flag"}
				}
				if !slices.Equal(problems, want) {
					t.Fatalf("problems = %q, want %q", problems, want)
				}
			})
		}
	})
	t.Run("headers", func(t *testing.T) {
		for _, test := range []struct {
			header, delimiter string
			table, invalid    bool
		}{
			{"| Flag | Description |", "|---|---|", true, false},
			{"| Target flag | Description |", "|---|---|", true, false},
			{"| Flags | Description |", "|---|---|", true, true},
			{"| Flag | Description | Default |", "|---|---|---|", true, true},
			{"| Flag | Description |", "|:--|:--|", true, true},
			{"| FLAG | Description |", "|---|---|", true, true},
			{"| Name | Description |", "|---|---|", false, false},
		} {
			t.Run(test.header+test.delimiter, func(t *testing.T) {
				doc := "## Plain\n" + test.header + "\n" + test.delimiter + "\n| `--one` | x |\n"
				parsed, problems := commandSurfaceParse(doc, "fixture.md", families)
				count := 0
				if test.table {
					count = 1
				}
				if len(parsed.Tables) != count {
					t.Fatalf("table count = %d, want %d", len(parsed.Tables), count)
				}
				var want []string
				if test.invalid {
					want = []string{"fixture.md:2: " + commandSurfaceFlagHeaderProblem}
				}
				if !slices.Equal(problems, want) {
					t.Fatalf("problems = %q, want %q", problems, want)
				}
			})
		}
	})
	t.Run("headings", func(t *testing.T) {
		for _, test := range []struct {
			heading         string
			path            []string
			target, command bool
		}{
			{"### `witself realm show NAME_OR_ID`", []string{"realm", "show"}, false, true},
			{"### `witself realm show NAME_OR_ID` (target; not implemented)", []string{"realm", "show"}, true, true},
			{"### `witself realm show NAME_OR_ID` (roadmap; not implemented)", []string{"realm", "show"}, true, true},
			{"## `witself memory vector profile create|list` / `vector set`", []string{"memory", "vector", "profile"}, false, true},
			{"## `witself _managed-hooks`", []string{"_managed-hooks"}, false, true},
			{"#### `witself-admin cells list`", nil, false, false},
			{"## `witself unknown show`", []string{"unknown", "show"}, false, false},
			{"## `witself NAME`", nil, false, false},
			{"## `witself future show`", []string{"future", "show"}, true, true},
			{"# Plain", nil, false, false},
		} {
			t.Run(test.heading, func(t *testing.T) {
				parsed, problems := commandSurfaceParse(test.heading+"\n"+commandSurfaceFixtureTable(false, "--one"), "fixture.md", families)
				if len(problems) != 0 || len(parsed.Headings) != 1 {
					t.Fatalf("parsed = %+v, problems = %q", parsed, problems)
				}
				h := parsed.Headings[0]
				if !slices.Equal(h.Path, test.path) || h.Target != test.target || h.Command != test.command || h.Line != 1 {
					t.Fatalf("heading = %+v, want path %q, target %v, command %v", h, test.path, test.target, test.command)
				}
				if len(h.Tables) != 1 || parsed.Tables[h.Tables[0]].Heading != 0 {
					t.Fatal("nearest heading does not own table")
				}
			})
		}
		doc := commandSurfaceFixtureHeading("alpha") + "# Plain\n" + commandSurfaceFixtureTable(false, "--one")
		parsed, _ := commandSurfaceParse(doc, "fixture.md", families)
		if len(parsed.Tables) != 1 || parsed.Tables[0].Heading != 1 || parsed.Headings[1].Command {
			t.Fatal("plain heading did not replace command context")
		}
	})
	t.Run("fences", func(t *testing.T) {
		for _, fence := range []string{"```", "~~~~"} {
			t.Run(fence, func(t *testing.T) {
				doc := fence + "markdown\n" + commandSurfaceFixtureHeading("alpha") + commandSurfaceFixtureTable(false, "--one") + fence + "\n## Outside\n"
				parsed, problems := commandSurfaceParse(doc, "fixture.md", families)
				if len(problems) != 0 || len(parsed.Tables) != 0 || len(parsed.Headings) != 1 || parsed.Headings[0].Command {
					t.Fatalf("fenced content was parsed: %+v; %q", parsed, problems)
				}
			})
		}
		// A different character or trailing text cannot close an open fence.
		doc := "````markdown\n```\n~~~~\n```` trailing\n" + commandSurfaceFixtureHeading("alpha") + "`````\n## Outside\n"
		parsed, _ := commandSurfaceParse(doc, "fixture.md", families)
		if len(parsed.Headings) != 1 || parsed.Headings[0].Command {
			t.Fatalf("invalid fence termination: %+v", parsed.Headings)
		}
	})
	t.Run("family_markers", func(t *testing.T) {
		doc := "<!-- BEGIN COMMAND FAMILY STATUS -->\n| Binary | Family | Marker | Aliases |\n|---|---|---|---|\n| `witself` | `alpha` | implemented | — |\n| `witself` | `future` | target | — |\n| `witself-admin` | `realm` | implemented | — |\n<!-- END COMMAND FAMILY STATUS -->\n"
		got := commandSurfaceFlagFamilies(doc)
		if len(got) != 2 || got["alpha"] != "implemented" || got["future"] != "target" {
			t.Fatalf("families = %v", got)
		}
	})
}

const commandSurfaceProbeMarker = "WITSELF-SURFACE-PROBE "
const commandSurfaceProbeOutputLimit = 64 * 1024

type commandSurfaceProbeReply struct {
	Ready   bool   `json:"ready,omitempty"`
	Code    int    `json:"code"`
	Output  string `json:"output"`
	Timeout bool   `json:"timeout,omitempty"`
}

// commandSurfaceProbeHead drains every byte while retaining only bounded output.
type commandSurfaceProbeHead struct {
	data      []byte
	truncated bool
}

func (b *commandSurfaceProbeHead) Write(p []byte) (int, error) {
	n := len(p)
	remaining := commandSurfaceProbeOutputLimit - len(b.data)
	if len(p) > remaining {
		p = p[:remaining]
		b.truncated = true
	}
	b.data = append(b.data, p...)
	return n, nil
}

func (b *commandSurfaceProbeHead) String() string {
	if b.truncated {
		return string(b.data) + "…[truncated]"
	}
	return string(b.data)
}

func TestCommandSurfaceProbeHelper(t *testing.T) {
	if os.Getenv("WITSELF_COMMAND_SURFACE_PROBE_HELPER") != "1" {
		return
	}
	input, output := os.Stdin, os.Stdout
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	os.Stdin = null
	defer func() {
		os.Stdin = input
		_ = null.Close()
	}()
	timeout := 10 * time.Second
	if value := os.Getenv("WITSELF_COMMAND_SURFACE_PROBE_TIMEOUT"); value != "" {
		timeout, err = time.ParseDuration(value)
		if err != nil || timeout <= 0 {
			t.Fatalf("invalid probe timeout: %q", value)
		}
	}
	reply := func(result any) {
		t.Helper()
		data, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fmt.Fprintf(output, "\n%s%s\n", commandSurfaceProbeMarker, data); err != nil {
			t.Fatal(err)
		}
	}
	reply(map[string]bool{"ready": true})
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for scanner.Scan() {
		var argv []string
		if err := json.Unmarshal(scanner.Bytes(), &argv); err != nil {
			t.Fatal(err)
		}
		reader, writer, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		originalOut, originalErr := os.Stdout, os.Stderr
		os.Stdout, os.Stderr = writer, writer
		var captured commandSurfaceProbeHead
		drained := make(chan error, 1)
		go func() {
			_, copyErr := io.Copy(&captured, reader)
			_ = reader.Close()
			drained <- copyErr
		}()
		result := make(chan int, 1)
		go func() {
			if os.Getenv("WITSELF_COMMAND_SURFACE_PROBE_TEST_HOOKS") == "1" && len(argv) == 1 {
				switch argv[0] {
				case "__witself_surface_probe_sleep__":
					<-make(chan struct{})
				case "__witself_surface_probe_fail__":
					t.Errorf("deliberate probe helper failure")
					// Cleanup runs after stdin closes, proving Finish walks after exit.
					t.Cleanup(func() {
						if err := os.WriteFile(filepath.Join(os.Getenv("WITSELF_HOME"), "exit"), []byte("fixture"), 0o600); err != nil {
							t.Error(err)
						}
					})
					result <- 0
					return
				}
			}
			result <- run(argv)
		}()
		timer := time.NewTimer(timeout)
		select {
		case code := <-result:
			timer.Stop()
			os.Stdout, os.Stderr = originalOut, originalErr
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			if err := <-drained; err != nil {
				t.Fatal(err)
			}
			reply(commandSurfaceProbeReply{Code: code, Output: captured.String()})
		case <-timer.C:
			reply(map[string]bool{"timeout": true})
			os.Exit(3)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
}

// commandSurfaceProbeTail retains diagnostics from both exec and stdout readers.
type commandSurfaceProbeTail struct {
	mu   sync.Mutex
	data []byte
}

func (b *commandSurfaceProbeTail) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	if len(p) >= commandSurfaceProbeOutputLimit {
		b.data = append(b.data[:0], p[len(p)-commandSurfaceProbeOutputLimit:]...)
	} else {
		if excess := len(b.data) + len(p) - commandSurfaceProbeOutputLimit; excess > 0 {
			copy(b.data, b.data[excess:])
			b.data = b.data[:len(b.data)-excess]
		}
		b.data = append(b.data, p...)
	}
	return n, nil
}

func (b *commandSurfaceProbeTail) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.data)
}

type commandSurfaceProbeEvent struct {
	reply commandSurfaceProbeReply
	err   error
}

type commandSurfaceProbeSession struct {
	probeMu        sync.Mutex
	mu             sync.Mutex
	cmd            *exec.Cmd
	stdin          io.WriteCloser
	root           string
	helperTimeout  time.Duration
	events         chan commandSurfaceProbeEvent
	done           chan struct{}
	stderrTail     commandSurfaceProbeTail
	stdoutTail     commandSurfaceProbeTail
	cache          map[string]string
	failure        error
	requestsSent   int
	waitErr        error
	exitCode       int
	helperTimedOut bool
	killed         bool
	finished       bool
	finishProblems []string
	finishErr      error
}

func commandSurfaceProbeArgs(coverMode, coverDir string) []string {
	args := []string{"-test.run=^TestCommandSurfaceProbeHelper$", "-test.count=1"}
	if coverMode != "" {
		args = append(args, "-test.gocoverdir="+coverDir)
	}
	return args
}

func commandSurfaceProbeExecutable(executable func() (string, error)) (string, error) {
	name, err := executable()
	if err != nil {
		return "", err
	}
	return filepath.Abs(name)
}

func commandSurfaceProbeEnv(root string, parent []string, goos string) []string {
	parentValues := make(map[string]string)
	for _, entry := range parent {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			if goos == "windows" && strings.EqualFold(key, "PATH") {
				key = "PATH"
			}
			parentValues[key] = value
		}
	}
	values := map[string]string{
		"PATH":                                 parentValues["PATH"],
		"HOME":                                 filepath.Join(root, "home"),
		"WITSELF_HOME":                         filepath.Join(root, "home", ".witself"),
		"XDG_CONFIG_HOME":                      filepath.Join(root, "xdg", "config"),
		"XDG_DATA_HOME":                        filepath.Join(root, "xdg", "data"),
		"XDG_STATE_HOME":                       filepath.Join(root, "xdg", "state"),
		"XDG_CACHE_HOME":                       filepath.Join(root, "xdg", "cache"),
		"TMPDIR":                               filepath.Join(root, "tmp"),
		"HTTP_PROXY":                           "http://127.0.0.1:9",
		"HTTPS_PROXY":                          "http://127.0.0.1:9",
		"ALL_PROXY":                            "http://127.0.0.1:9",
		"http_proxy":                           "http://127.0.0.1:9",
		"https_proxy":                          "http://127.0.0.1:9",
		"all_proxy":                            "http://127.0.0.1:9",
		"NO_PROXY":                             "",
		"no_proxy":                             "",
		"WITSELF_CONTROL_PLANE":                "http://127.0.0.1:9",
		"WITSELF_ENDPOINT":                     "http://127.0.0.1:9",
		"WITSELF_LEGAL_URL":                    "http://127.0.0.1:9",
		"WITSELF_COMMAND_SURFACE_PROBE_HELPER": "1",
	}
	if goos == "windows" {
		values["USERPROFILE"] = filepath.Join(root, "home")
		values["APPDATA"] = filepath.Join(root, "xdg", "appdata")
		values["LOCALAPPDATA"] = filepath.Join(root, "xdg", "localappdata")
		values["SystemRoot"] = parentValues["SystemRoot"]
	}
	result := make([]string, 0, len(values))
	for key, value := range values {
		result = append(result, key+"="+value)
	}
	sort.Strings(result)
	return result
}

func startCommandSurfaceProbeSession(t *testing.T, helperTimeout time.Duration, extraEnv ...string) *commandSurfaceProbeSession {
	t.Helper()
	root := t.TempDir()
	// Coverage exit hooks must write outside every directory the tripwire watches.
	coverDir := t.TempDir()
	for _, dir := range []string{"home/.witself", "xdg/config", "xdg/data", "xdg/state", "xdg/cache", "tmp", "work"} {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(dir)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if runtime.GOOS == "windows" {
		for _, dir := range []string{"xdg/appdata", "xdg/localappdata"} {
			if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(dir)), 0o700); err != nil {
				t.Fatal(err)
			}
		}
	}
	executable, err := commandSurfaceProbeExecutable(os.Executable)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, commandSurfaceProbeArgs(testing.CoverMode(), coverDir)...)
	cmd.Dir = filepath.Join(root, "work")
	cmd.Env = append(commandSurfaceProbeEnv(root, os.Environ(), runtime.GOOS), "WITSELF_COMMAND_SURFACE_PROBE_TIMEOUT="+helperTimeout.String())
	cmd.Env = append(cmd.Env, extraEnv...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		_ = stdin.Close()
		t.Fatal(err)
	}
	s := &commandSurfaceProbeSession{
		cmd: cmd, stdin: stdin, root: root, helperTimeout: helperTimeout,
		events: make(chan commandSurfaceProbeEvent, 16), done: make(chan struct{}),
		cache: make(map[string]string), exitCode: -1,
	}
	cmd.Stdout, cmd.Stderr = stdoutWriter, &s.stderrTail
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
		t.Fatal(err)
	}
	_ = stdoutWriter.Close()
	readDone := make(chan struct{})
	processDone := make(chan struct{})
	go func() {
		defer close(readDone)
		defer func() { _ = stdoutReader.Close() }()
		scanner := bufio.NewScanner(stdoutReader)
		scanner.Buffer(make([]byte, 4096), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			index := strings.LastIndex(line, commandSurfaceProbeMarker)
			if index < 0 {
				_, _ = s.stdoutTail.Write([]byte(line + "\n"))
				continue
			}
			var result commandSurfaceProbeReply
			err := json.Unmarshal([]byte(line[index+len(commandSurfaceProbeMarker):]), &result)
			event := commandSurfaceProbeEvent{reply: result, err: err}
			// Preserve the final reply even when Wait observes exit first.
			select {
			case s.events <- event:
			default:
				select {
				case s.events <- event:
				case <-processDone:
					return
				}
			}
		}
		if err := scanner.Err(); err != nil {
			select {
			case s.events <- commandSurfaceProbeEvent{err: err}:
			case <-processDone:
			}
		}
	}()
	go func() {
		waitErr := cmd.Wait()
		close(processDone)
		<-readDone
		s.mu.Lock()
		s.waitErr = waitErr
		s.exitCode = cmd.ProcessState.ExitCode()
		s.mu.Unlock()
		close(s.done)
	}()
	t.Cleanup(func() {
		// Explicit Finish callers assert expected failures; cleanup only reaps.
		_, _ = s.Finish()
	})
	timer := time.NewTimer(60 * time.Second)
	defer timer.Stop()
	select {
	case event := <-s.events:
		if event.err != nil || !event.reply.Ready {
			s.Kill()
			s.waitForExit(10 * time.Second)
			t.Fatalf("probe helper exited before ready: %v\n%s", event.err, s.diagnostics())
		}
	case <-s.done:
		t.Fatalf("probe helper exited before ready\n%s", s.diagnostics())
	case <-timer.C:
		s.Kill()
		s.waitForExit(10 * time.Second)
		t.Fatalf("probe helper timed out waiting for ready\n%s", s.diagnostics())
	}
	return s
}

func (s *commandSurfaceProbeSession) diagnostics() string {
	return "stderr:\n" + s.stderrTail.String() + "\nstdout:\n" + s.stdoutTail.String()
}

func (s *commandSurfaceProbeSession) waitForExit(timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-s.done:
		return true
	case <-timer.C:
		return false
	}
}

func (s *commandSurfaceProbeSession) fail(argv []string, reason string) (string, error) {
	s.failure = fmt.Errorf("probe %q %s\n%s", argv, reason, s.diagnostics())
	return "", s.failure
}

func (s *commandSurfaceProbeSession) Probe(argv []string) (string, error) {
	s.probeMu.Lock()
	defer s.probeMu.Unlock()
	if s.failure != nil {
		return "", s.failure
	}
	data, err := json.Marshal(argv)
	if err != nil {
		return s.fail(argv, "exited: cannot encode request")
	}
	key := string(data)
	if output, ok := s.cache[key]; ok {
		return output, nil
	}
	s.mu.Lock()
	s.requestsSent++
	s.mu.Unlock()
	timer := time.NewTimer(3*s.helperTimeout + 5*time.Second)
	defer timer.Stop()
	written := make(chan error, 1)
	go func() {
		_, writeErr := s.stdin.Write(append(data, '\n'))
		written <- writeErr
	}()
	select {
	case err := <-written:
		if err != nil {
			s.Kill()
			s.waitForExit(10 * time.Second)
			return s.fail(argv, "exited: "+err.Error())
		}
	case <-timer.C:
		s.Kill()
		s.waitForExit(10 * time.Second)
		return s.fail(argv, "timed out writing request")
	}
	var event commandSurfaceProbeEvent
	select {
	case event = <-s.events:
	case <-s.done:
		// A final reply can arrive just before the child exits.
		select {
		case event = <-s.events:
		default:
			s.Kill()
			return s.fail(argv, "exited before replying")
		}
	case <-timer.C:
		s.Kill()
		s.waitForExit(10 * time.Second)
		return s.fail(argv, "timed out waiting for reply")
	}
	if event.err != nil || event.reply.Ready {
		s.Kill()
		s.waitForExit(10 * time.Second)
		return s.fail(argv, fmt.Sprintf("exited: invalid reply: %v", event.err))
	}
	if event.reply.Timeout {
		s.helperTimedOut = true
		if !s.waitForExit(10 * time.Second) {
			s.Kill()
			s.waitForExit(10 * time.Second)
		}
		return s.fail(argv, "timed out")
	}
	s.cache[key] = event.reply.Output
	return event.reply.Output, nil
}

func (s *commandSurfaceProbeSession) RequestsSent() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requestsSent
}

func (s *commandSurfaceProbeSession) Finish() ([]string, error) {
	s.probeMu.Lock()
	defer s.probeMu.Unlock()
	if s.finished {
		return s.finishProblems, s.finishErr
	}
	_ = s.stdin.Close()
	if !s.waitForExit(30 * time.Second) {
		s.Kill()
		s.waitForExit(10 * time.Second)
	}
	s.mu.Lock()
	waitErr, exitCode, killed := s.waitErr, s.exitCode, s.killed
	s.mu.Unlock()
	if killed || (waitErr != nil && (!s.helperTimedOut || exitCode != 3)) {
		s.finishErr = fmt.Errorf("probe helper exited uncleanly (code %d): %v\n%s", exitCode, waitErr, s.diagnostics())
	}
	s.finishProblems = commandSurfaceProbeTripwire(s.root)
	s.finished = true
	return s.finishProblems, s.finishErr
}

func (s *commandSurfaceProbeSession) ExitCode() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exitCode
}

func (s *commandSurfaceProbeSession) Root() string { return s.root }

func (s *commandSurfaceProbeSession) Kill() {
	if err := s.cmd.Process.Kill(); err == nil {
		s.mu.Lock()
		s.killed = true
		s.mu.Unlock()
	}
}

func commandSurfaceProbeTripwire(root string) []string {
	var paths, problems []string
	for _, dir := range []string{"home", "xdg", "tmp", "work"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			if !entry.IsDir() {
				relative, err := filepath.Rel(root, path)
				if err != nil {
					return err
				}
				paths = append(paths, filepath.ToSlash(relative))
			}
			return nil
		})
		if err != nil {
			problems = append(problems, "probe session walk failed: "+err.Error())
		}
	}
	if len(paths) > 0 {
		sort.Strings(paths)
		problems = append(problems, "probe session created files: "+strings.Join(paths, ", "))
	}
	return problems
}

func TestCommandSurfaceProbeEnvironment(t *testing.T) {
	root := t.TempDir()
	parent := []string{"WITSELF_TOKEN=x", "WITSELF_ACCOUNT=y", "WITSELF_TEST_REQUIRE_DATABASE=1", "HOME=/real", "PATH=/p", "GOCOVERDIR=/c", "FOO=bar", `SystemRoot=C:\W`}
	base := []string{
		"ALL_PROXY", "HOME", "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "PATH", "TMPDIR",
		"WITSELF_COMMAND_SURFACE_PROBE_HELPER", "WITSELF_CONTROL_PLANE", "WITSELF_ENDPOINT", "WITSELF_HOME", "WITSELF_LEGAL_URL",
		"XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME",
		"all_proxy", "http_proxy", "https_proxy", "no_proxy",
	}
	for _, test := range []struct {
		name, goos, path string
		parent           []string
	}{
		{"linux", "linux", "/p", parent},
		{"windows", "windows", "/p", parent},
		{"windows_mixed_case_path", "windows", `C:\p`, []string{`Path=C:\p`, `SystemRoot=C:\W`}},
	} {
		t.Run(test.name, func(t *testing.T) {
			env := commandSurfaceProbeEnv(root, test.parent, test.goos)
			if !sort.StringsAreSorted(env) {
				t.Fatalf("environment is not sorted: %v", env)
			}
			values := make(map[string]string)
			var keys []string
			for _, entry := range env {
				key, value, ok := strings.Cut(entry, "=")
				if !ok {
					t.Fatalf("invalid environment entry %q", entry)
				}
				keys = append(keys, key)
				values[key] = value
			}
			wantKeys := append([]string(nil), base...)
			if test.goos == "windows" {
				wantKeys = append(wantKeys, "USERPROFILE", "APPDATA", "LOCALAPPDATA", "SystemRoot")
				if values["SystemRoot"] != `C:\W` || values["USERPROFILE"] != filepath.Join(root, "home") || values["APPDATA"] != filepath.Join(root, "xdg", "appdata") || values["LOCALAPPDATA"] != filepath.Join(root, "xdg", "localappdata") {
					t.Fatalf("wrong Windows environment: %v", values)
				}
			}
			sort.Strings(wantKeys)
			if !reflect.DeepEqual(keys, wantKeys) {
				t.Fatalf("environment keys = %v, want %v", keys, wantKeys)
			}
			if values["PATH"] != test.path || values["HOME"] != filepath.Join(root, "home") || values["WITSELF_HOME"] != filepath.Join(root, "home", ".witself") {
				t.Fatalf("wrong home or PATH: %v", values)
			}
			for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy", "WITSELF_CONTROL_PLANE", "WITSELF_ENDPOINT", "WITSELF_LEGAL_URL"} {
				if values[key] != "http://127.0.0.1:9" {
					t.Errorf("%s = %q", key, values[key])
				}
			}
			for key, path := range map[string]string{"XDG_CONFIG_HOME": "xdg/config", "XDG_DATA_HOME": "xdg/data", "XDG_STATE_HOME": "xdg/state", "XDG_CACHE_HOME": "xdg/cache", "TMPDIR": "tmp"} {
				if values[key] != filepath.Join(root, filepath.FromSlash(path)) {
					t.Errorf("%s = %q", key, values[key])
				}
			}
			if values["NO_PROXY"] != "" || values["no_proxy"] != "" || values["WITSELF_COMMAND_SURFACE_PROBE_HELPER"] != "1" {
				t.Fatalf("wrong proxy bypass or helper environment: %v", values)
			}
		})
	}
	wantArgs := []string{"-test.run=^TestCommandSurfaceProbeHelper$", "-test.count=1"}
	if got := commandSurfaceProbeArgs("", "/d"); !reflect.DeepEqual(got, wantArgs) {
		t.Fatalf("non-coverage args = %v", got)
	}
	if got := commandSurfaceProbeArgs("atomic", "/d"); !reflect.DeepEqual(got, append(wantArgs, "-test.gocoverdir=/d")) {
		t.Fatalf("coverage args = %v", got)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	got, err := commandSurfaceProbeExecutable(func() (string, error) { return "witself.test", nil })
	if err != nil || got != filepath.Join(cwd, "witself.test") {
		t.Fatalf("probe executable = %q, %v", got, err)
	}
}

func TestCommandSurfaceProbeTripwire(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"home/.witself", "xdg/cache", "tmp", "work"} {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(dir)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if problems := commandSurfaceProbeTripwire(root); len(problems) != 0 {
		t.Fatalf("directories triggered tripwire: %v", problems)
	}
	for _, path := range []string{"work/y", "home/.witself/x", "tmp/t", "xdg/cache/c"} {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(path)), []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"probe session created files: home/.witself/x, tmp/t, work/y, xdg/cache/c"}
	if problems := commandSurfaceProbeTripwire(root); !reflect.DeepEqual(problems, want) {
		t.Fatalf("tripwire = %v, want %v", problems, want)
	}
}

func TestCommandSurfaceProbeSessionTripwire(t *testing.T) {
	const helperTimeout = 10 * time.Second
	s := startCommandSurfaceProbeSession(t, helperTimeout)
	if want := filepath.Join(s.Root(), "work"); s.cmd.Dir != want {
		t.Errorf("child working directory = %q, want %q", s.cmd.Dir, want)
	}
	wantEnv := append(commandSurfaceProbeEnv(s.Root(), os.Environ(), runtime.GOOS), "WITSELF_COMMAND_SURFACE_PROBE_TIMEOUT="+helperTimeout.String())
	if !slices.Equal(s.cmd.Env, wantEnv) {
		t.Error("child environment differs from the exact isolated environment plus timeout")
	}
	if err := os.WriteFile(filepath.Join(s.Root(), "home", ".witself", "x"), []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	problems, err := s.Finish()
	want := []string{"probe session created files: home/.witself/x"}
	if err != nil || !reflect.DeepEqual(problems, want) {
		t.Fatalf("Finish = %v, %v; want %v, nil", problems, err, want)
	}
}

func TestCommandSurfaceProbeTimeout(t *testing.T) {
	const helperTimeout = time.Second
	s := startCommandSurfaceProbeSession(t, helperTimeout, "WITSELF_COMMAND_SURFACE_PROBE_TEST_HOOKS=1")
	result := make(chan error, 1)
	go func() {
		_, err := s.Probe([]string{"__witself_surface_probe_sleep__"})
		result <- err
	}()
	guard := time.NewTimer(30 * helperTimeout)
	defer guard.Stop()
	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "timed out") {
			t.Fatalf("sleep probe error = %v", err)
		}
	case <-guard.C:
		s.Kill()
		t.Fatal("sleep probe did not return within guard deadline")
	}
	requests := s.RequestsSent()
	go func() {
		_, err := s.Probe([]string{"version"})
		result <- err
	}()
	secondGuard := time.NewTimer(time.Second)
	defer secondGuard.Stop()
	select {
	case err := <-result:
		if s.RequestsSent() != requests {
			t.Fatalf("failed session sent another request: before=%d after=%d", requests, s.RequestsSent())
		}
		if err == nil || !strings.Contains(err.Error(), "timed out") {
			t.Fatalf("second probe error = %v", err)
		}
	case <-secondGuard.C:
		s.Kill()
		t.Fatal("failed session did not short-circuit")
	}
	problems, err := s.Finish()
	if err != nil || len(problems) != 0 || s.ExitCode() != 3 {
		t.Fatalf("Finish = %v, %v; exit=%d", problems, err, s.ExitCode())
	}
}

func TestCommandSurfaceProbeHelperFailure(t *testing.T) {
	s := startCommandSurfaceProbeSession(t, 10*time.Second, "WITSELF_COMMAND_SURFACE_PROBE_TEST_HOOKS=1")
	if _, err := s.Probe([]string{"__witself_surface_probe_fail__"}); err != nil {
		t.Fatal(err)
	}
	problems, err := s.Finish()
	want := []string{"probe session created files: home/.witself/exit"}
	if !slices.Equal(problems, want) || err == nil || !strings.Contains(err.Error(), "deliberate probe helper failure") || s.ExitCode() == 0 {
		t.Fatalf("Finish = %v, %v; exit=%d", problems, err, s.ExitCode())
	}
}

const commandSurfaceFlagPreamble = "Under a command heading in an implemented family that is not marked target, tables headed `Flag` list shipped flags and tables headed `Target flag` list target flags. Every flag table in a target family or under a target heading is target. A command heading that ends with `(target; not implemented)` describes a verb the CLI does not dispatch. `TestCommandSurfaceDocumentedFlagsExist` checks these tables and headings against the built CLI."

func commandSurfaceReadFlagDoc(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(commandSurfaceDoc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func commandSurfaceCheckRealFlags(doc string, probe commandSurfaceProbe) commandSurfaceFlagReport {
	return checkCommandSurfaceFlags(commandSurfaceFlagInput{Doc: doc, File: commandSurfaceDoc, Families: commandSurfaceFlagFamilies(doc), DefaultVerbGroups: commandSurfaceDefaultVerbGroups, Probe: probe})
}

func commandSurfaceFinishFlags(t *testing.T, session *commandSurfaceProbeSession) {
	t.Helper()
	problems, err := session.Finish()
	if err != nil {
		t.Error(err)
	}
	for _, problem := range problems {
		t.Error(problem)
	}
}

func TestCommandSurfaceDocumentedFlagsExist(t *testing.T) {
	started := time.Now()
	doc := commandSurfaceReadFlagDoc(t)
	session := startCommandSurfaceProbeSession(t, 10*time.Second)
	defer commandSurfaceFinishFlags(t, session)
	report := commandSurfaceCheckRealFlags(doc, session.Probe)
	for _, problem := range report.Problems {
		t.Error(problem)
	}
	parsed, _ := commandSurfaceParse(doc, commandSurfaceDoc, commandSurfaceFlagFamilies(doc))
	if parsed.CommonBlocks != 1 || strings.Count(doc, "<!-- BEGIN COMMON FLAGS -->") != 1 || strings.Count(doc, "<!-- END COMMON FLAGS -->") != 1 {
		t.Error("expected exactly one common flags block")
	}
	var common, target []string
	afterCommon := false
	for _, table := range parsed.Tables {
		if table.Common && !table.Target {
			for _, row := range table.Rows {
				common = append(common, row.Names...)
			}
			afterCommon = true
		} else if afterCommon && table.Target {
			for _, row := range table.Rows {
				target = append(target, row.Names...)
			}
			break
		}
	}
	if want := []string{"account", "endpoint", "token-file", "realm", "agent", "json"}; !slices.Equal(common, want) {
		t.Errorf("common flags = %v, want %v", common, want)
	}
	if want := []string{"quiet", "config", "profile", "store-file", "no-input"}; !slices.Equal(target, want) {
		t.Errorf("global target flags = %v, want %v", target, want)
	}
	if strings.Count(doc, commandSurfaceFlagPreamble) != 1 {
		t.Error("expected exact flag-table preamble once")
	}
	resolve := func(path string, want []string) []string {
		t.Helper()
		got, err := commandSurfaceResolve(session.Probe, commandSurfaceDefaultVerbGroups, strings.Fields(path))
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("resolve %s = %v, %v; want %v", path, got, err, want)
		}
		return got
	}
	resolve("export", []string{"export"})
	resolve("realm show", nil)
	for _, verb := range []string{"show", "invoices", "payments", "portal", "setup"} {
		resolve("billing "+verb, []string{"billing", verb})
	}
	invoicePath := []string{"billing", "invoices", "show"}
	prefix := resolve(strings.Join(invoicePath, " "), []string{"billing", "invoices"})
	if commandSurfaceHeadingResolved(prefix, invoicePath) {
		t.Error("billing invoices show heading must not resolve at billing invoices")
	}
	for _, group := range commandSurfaceDefaultVerbGroups {
		resolve(group, []string{group})
	}
	resolve("install codex", []string{"install", "codex"})
	for _, anchor := range []struct {
		path, name string
		defined    bool
	}{
		{"export", "force", true}, {"export", "witself-surface-bogus", false}, {"billing invoices", "pdf", true},
	} {
		got, err := commandSurfaceFlagState(session.Probe, strings.Fields(anchor.path), anchor.name)
		if err != nil || got != anchor.defined {
			t.Errorf("%s --%s defined = %v, %v; want %v", anchor.path, anchor.name, got, err, anchor.defined)
		}
	}
	for _, path := range []string{"auth login", "account create", "realm delete", "billing show", "billing invoices", "billing payments", "billing portal", "billing setup", "self show", "usage", "memory recall", "fact set", "password generate", "secret list", "message send", "agent delete", "token create", "export", "mcp serve", "dashboard serve"} {
		found := false
		for _, heading := range parsed.Headings {
			if !heading.Command || heading.Target || strings.Join(heading.Path, " ") != path {
				continue
			}
			for _, index := range heading.Tables {
				if !parsed.Tables[index].Target {
					found = true
				}
			}
		}
		if !found {
			t.Errorf("must-resolve heading %s lacks a non-target heading with a shipped flag table", path)
		}
		resolve(path, strings.Fields(path))
	}
	if report.ResolvedHeadings < 45 {
		t.Errorf("resolved headings = %d, want >= 45", report.ResolvedHeadings)
	}
	if report.ShippedFlagsVerified < 240 {
		t.Errorf("shipped flags = %d, want >= 240", report.ShippedFlagsVerified)
	}
	if report.TargetFlagsChecked < 120 {
		t.Errorf("target flags = %d, want >= 120", report.TargetFlagsChecked)
	}
	if report.CommonFlagsVerified != 6 {
		t.Errorf("common flags = %d, want 6", report.CommonFlagsVerified)
	}
	t.Logf("ResolvedHeadings=%d ShippedFlagsVerified=%d TargetFlagsChecked=%d CommonFlagsVerified=%d; wall time=%s", report.ResolvedHeadings, report.ShippedFlagsVerified, report.TargetFlagsChecked, report.CommonFlagsVerified, time.Since(started))
}

func TestCommandSurfaceDocumentedFlagDrift(t *testing.T) {
	doc := commandSurfaceReadFlagDoc(t)
	session := startCommandSurfaceProbeSession(t, 10*time.Second)
	defer commandSurfaceFinishFlags(t, session)
	replaceAfter := func(t *testing.T, doc, anchor, before, after string) string {
		t.Helper()
		index := strings.Index(doc, anchor)
		if index < 0 {
			t.Fatal("mutation did not match: anchor")
		}
		changed := doc[:index] + strings.Replace(doc[index:], before, after, 1)
		if changed == doc {
			t.Fatal("mutation did not match")
		}
		return changed
	}
	cases := []struct{ name, anchor, before, after, want string }{
		{"a", "## `witself export`", "| Flag | Description |\n|---|---|", "| Flag | Description |\n|---|---|\n| `--surface-regression` | x |", "witself export has no flag --surface-regression"},
		{"b", "### `witself account create`", "| Target flag | Description |", "| Flag | Description |", "witself account create has no flag --legal-name"},
		{"d", "### `witself realm show", " (target; not implemented)", "", "witself realm show has a shipped flag table but does not parse flags"},
		{"e", "### `witself memory recall QUERY`", "### `witself memory recall QUERY`", "### `witself memory recall QUERY` (target; not implemented)", "witself memory recall is marked target but parses flags"},
		{"f", "<!-- BEGIN COMMON FLAGS -->", "| Flag | Description |\n|---|---|", "| Flag | Description |\n|---|---|\n| `--surface-regression` | x |", "common flag --surface-regression is not defined on any documented command"},
		{"g", "### `witself account create`", "| Flag | Description |", "| Flags | Description |", "flag table header must be \"| Flag | Description |\" or \"| Target flag | Description |\""},
		{"h", "## Design Goals", "## Design Goals", "## Design Goals\n\n| Flag | Description |\n|---|---|\n| `--json` | x |\n", "shipped flag table is not under an implemented witself command heading"},
		{"i", "### `witself billing invoices`", "### `witself billing invoices`", "### `witself billing invoices` (target; not implemented)", "witself billing invoices is marked target but parses flags"},
		{"j", "## `witself usage`", "`--group-by hour\\|day`", "`--group-by hour|day`", "flag table row names no flag"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			changed := replaceAfter(t, doc, tc.anchor, tc.before, tc.after)
			commandSurfaceRequireProblem(t, commandSurfaceCheckRealFlags(changed, session.Probe).Problems, tc.want)
		})
	}
	t.Run("c", func(t *testing.T) {
		const anchor = "### `witself account create`"
		start := strings.Index(doc, anchor)
		if start < 0 {
			t.Fatal("mutation did not match: anchor")
		}
		rest := doc[start:]
		rowStart := strings.Index(rest, "| `--email EMAIL`")
		if rowStart < 0 {
			t.Fatal("mutation did not match: row")
		}
		rowEnd := strings.IndexByte(rest[rowStart:], '\n')
		if rowEnd < 0 {
			t.Fatal("mutation did not match: row end")
		}
		row := rest[rowStart : rowStart+rowEnd+1]
		changed := replaceAfter(t, doc, anchor, row, "")
		const header = "| Target flag | Description |\n|---|---|\n"
		changed = replaceAfter(t, changed, anchor, header, header+row)
		commandSurfaceRequireProblem(t, commandSurfaceCheckRealFlags(changed, session.Probe).Problems, "witself account create lists target flag --email but the flag exists")
	})
}

func commandSurfaceRequireProblem(t *testing.T, problems []string, want string) {
	t.Helper()
	for _, problem := range problems {
		if strings.Contains(problem, want) {
			return
		}
	}
	t.Errorf("missing problem %q in %v", want, problems)
}
