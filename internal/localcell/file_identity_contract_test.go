package localcell

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

type fileIdentityCount struct {
	calls, pins int
}

var fileIdentityExpected = map[string]fileIdentityCount{
	"installer_principal_credentials.go": {3, 1},
	"service_launchd.go":                 {4, 0},
	"service_systemd_adapter.go":         {1, 0},
	"service_windows_task.go":            {5, 1},
	"trust/activation.go":                {1, 0},
	"trust/activation_unix.go":           {1, 0},
	"trust/channel_windows.go":           {4, 0},
	"trust/path_unix.go":                 {2, 0},
	"trust/path_windows.go":              {4, 0},
	"trust/private_record_unix.go":       {5, 0},
	"update_record.go":                   {2, 1},
}

type fileIdentitySite struct {
	file, receiver, function, variable string
}

var fileIdentitySites = []fileIdentitySite{
	{"installer_principal_credentials.go", "*Cell", "credentialPassword", "before"},
	{"update_record.go", "*Cell", "appendUpdateJournalEntry", "info"},
	{"service_windows_task.go", "", "readWindowsTaskRecord", "before"},
}

type fileIdentityProblem struct {
	file, check, detail string
}

const fileIdentityInventoryGuidance = "A pathname FileInfo is resolved lazily on Windows; a comparison whose pathname side can be replaced before it is first used must be pinned by a checked self-comparison before the open; update the table only after the new call has been classified."

// Both checks below operate only on supplied bytes. Parsing every selected file
// deliberately ignores build constraints, including foreign-platform sources.
func fileIdentityInventory(sources map[string][]byte, expected map[string]fileIdentityCount) (map[string]fileIdentityCount, []fileIdentityProblem) {
	measured := make(map[string]fileIdentityCount)
	var problems []fileIdentityProblem
	for _, name := range fileIdentitySourceNames(sources) {
		file, imports, problem := fileIdentityParse(name, sources[name])
		if problem != nil {
			problems = append(problems, *problem)
			continue
		}
		var count fileIdentityCount
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || !fileIdentityPackageCall(call, imports["os"], "SameFile") {
				return true
			}
			count.calls++
			if len(call.Args) == 2 {
				left, leftOK := call.Args[0].(*ast.Ident)
				right, rightOK := call.Args[1].(*ast.Ident)
				if leftOK && rightOK && left.Name == right.Name {
					count.pins++
				}
			}
			return true
		})
		if count.calls != 0 {
			measured[name] = count
		}
	}
	names := make(map[string]bool)
	for name := range measured {
		names[name] = true
	}
	for name := range expected {
		names[name] = true
	}
	var ordered []string
	for name := range names {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)
	for _, name := range ordered {
		got, gotOK := measured[name]
		want, wantOK := expected[name]
		if !gotOK || !wantOK || got != want {
			problems = append(problems, fileIdentityProblem{name, "inventory",
				fmt.Sprintf("got calls/self-pins %d/%d, expected %d/%d; %s", got.calls, got.pins, want.calls, want.pins, fileIdentityInventoryGuidance)})
		}
	}
	return measured, problems
}

func fileIdentityPinnedSites(sources map[string][]byte, sites []fileIdentitySite) []fileIdentityProblem {
	var problems []fileIdentityProblem
	for _, site := range sites {
		problem := func(rule, detail string) {
			problems = append(problems, fileIdentityProblem{site.file, rule, site.function + ": " + detail})
		}
		source, ok := sources[site.file]
		if !ok {
			problem("site", "registered source is absent")
			continue
		}
		file, imports, parseProblem := fileIdentityParse(site.file, source)
		if parseProblem != nil {
			problems = append(problems, *parseProblem)
			continue
		}
		var functions []*ast.FuncDecl
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if ok && function.Name.Name == site.function && fileIdentityReceiver(function) == site.receiver {
				functions = append(functions, function)
			}
		}
		if len(functions) != 1 || functions[0].Body == nil {
			problem("site", "expected exactly one registered function body")
			continue
		}
		body := functions[0].Body
		var inspections []*ast.AssignStmt
		var calls, pins []*ast.CallExpr
		var conditions []*ast.IfStmt
		ast.Inspect(body, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.AssignStmt:
				for index, left := range node.Lhs {
					if !fileIdentityIdentifier(left, site.variable) {
						continue
					}
					// A single multi-result Lstat call assigns FileInfo to the
					// first identifier; the other result is its error.
					if index < len(node.Rhs) {
						call, ok := node.Rhs[index].(*ast.CallExpr)
						if ok && fileIdentityPackageCall(call, imports["os"], "Lstat") {
							inspections = append(inspections, node)
						}
					}
				}
			case *ast.CallExpr:
				calls = append(calls, node)
				if fileIdentityPackageCall(node, imports["os"], "SameFile") && fileIdentitySelfPin(node, site.variable) {
					pins = append(pins, node)
				}
			case *ast.IfStmt:
				conditions = append(conditions, node)
			}
			return true
		})
		if len(inspections) != 1 {
			problem("R1", "expected exactly one Lstat assignment to the registered variable")
		}
		if len(pins) != 1 || len(inspections) == 1 && pins[0].Pos() <= inspections[0].End() {
			problem("R2", "expected exactly one self-pin after the Lstat assignment")
		}
		if len(pins) != 1 {
			continue
		}
		pin := pins[0]
		guarded := false
		for _, condition := range conditions {
			statements := condition.Body.List
			if len(statements) == 0 {
				continue
			}
			_, returns := statements[len(statements)-1].(*ast.ReturnStmt)
			if returns && fileIdentityReturningPin(condition.Cond, pin) {
				guarded = true
			}
		}
		if !guarded {
			problem("R3", "self-pin must be negated as a whole condition or top-level OR operand in an if ending in return")
		}
		intervening, beforePin, afterPin := false, false, false
		for _, call := range calls {
			if len(inspections) == 1 && call.Pos() >= inspections[0].End() && call.Pos() < pin.Pos() &&
				!fileIdentityPackageCall(call, imports["errors"], "Is") {
				intervening = true
			}
			if call == pin || !fileIdentityPackageCall(call, imports["os"], "SameFile") || !fileIdentityMentions(call.Args, site.variable) {
				continue
			}
			if call.Pos() < pin.Pos() {
				beforePin = true
			}
			if call.Pos() > pin.Pos() && len(call.Args) == 2 && !fileIdentitySelfPin(call, site.variable) {
				afterPin = true
			}
		}
		if intervening {
			problem("R4", "only errors.Is may be called between the Lstat assignment and the self-pin")
		}
		if beforePin || !afterPin {
			problem("R5", "identity comparisons must follow the self-pin and include a different operand")
		}
	}
	return problems
}

func fileIdentityParse(name string, source []byte) (*ast.File, map[string]string, *fileIdentityProblem) {
	file, err := parser.ParseFile(token.NewFileSet(), name, source, 0)
	if err != nil {
		return nil, nil, &fileIdentityProblem{name, "parse", "cannot parse source"}
	}
	imports := make(map[string]string)
	for _, imported := range file.Imports {
		path, err := strconv.Unquote(imported.Path.Value)
		if err != nil {
			return nil, nil, &fileIdentityProblem{name, "parse", "cannot parse import path"}
		}
		if path != "os" && path != "errors" {
			continue
		}
		local := path
		if imported.Name != nil {
			local = imported.Name.Name
		}
		if path == "os" && local == "." {
			return nil, nil, &fileIdentityProblem{name, "inventory", "dot import of os is forbidden"}
		}
		if local != "." && local != "_" {
			imports[path] = local
		}
	}
	return file, imports, nil
}

func fileIdentityPackageCall(call *ast.CallExpr, imported, name string) bool {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	return ok && imported != "" && selector.Sel.Name == name && fileIdentityIdentifier(selector.X, imported)
}

func fileIdentityIdentifier(expression ast.Expr, name string) bool {
	identifier, ok := expression.(*ast.Ident)
	return ok && identifier.Name == name
}

func fileIdentitySelfPin(call *ast.CallExpr, variable string) bool {
	return len(call.Args) == 2 && fileIdentityIdentifier(call.Args[0], variable) && fileIdentityIdentifier(call.Args[1], variable)
}

func fileIdentityMentions(expressions []ast.Expr, variable string) bool {
	found := false
	for _, expression := range expressions {
		ast.Inspect(expression, func(node ast.Node) bool {
			if identifier, ok := node.(*ast.Ident); ok && identifier.Name == variable {
				found = true
			}
			return true
		})
	}
	return found
}

func fileIdentityReceiver(function *ast.FuncDecl) string {
	if function.Recv == nil || len(function.Recv.List) != 1 {
		return ""
	}
	expression := function.Recv.List[0].Type
	prefix := ""
	if pointer, ok := expression.(*ast.StarExpr); ok {
		prefix = "*"
		expression = pointer.X
	}
	if identifier, ok := expression.(*ast.Ident); ok {
		return prefix + identifier.Name
	}
	return "?"
}

func fileIdentityReturningPin(expression ast.Expr, pin *ast.CallExpr) bool {
	switch expression := expression.(type) {
	case *ast.ParenExpr:
		return fileIdentityReturningPin(expression.X, pin)
	case *ast.BinaryExpr:
		return expression.Op == token.LOR &&
			(fileIdentityReturningPin(expression.X, pin) || fileIdentityReturningPin(expression.Y, pin))
	case *ast.UnaryExpr:
		operand := expression.X
		for {
			parenthesized, ok := operand.(*ast.ParenExpr)
			if !ok {
				break
			}
			operand = parenthesized.X
		}
		return expression.Op == token.NOT && operand == pin
	default:
		return false
	}
}

func fileIdentitySourcePath(name string) bool {
	if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
		return false
	}
	parts := strings.Split(name, "/")
	for _, directory := range parts[:len(parts)-1] {
		if directory == "testdata" || strings.HasPrefix(directory, ".") {
			return false
		}
	}
	return true
}

func fileIdentitySourceNames(sources map[string][]byte) []string {
	var names []string
	for name := range sources {
		if fileIdentitySourcePath(name) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func fileIdentityReadSources(t *testing.T) map[string][]byte {
	t.Helper()
	sources := make(map[string][]byte)
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("cannot walk source path %s", filepath.ToSlash(path))
		}
		if entry.IsDir() {
			if path != "." && (entry.Name() == "testdata" || strings.HasPrefix(entry.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		name := filepath.ToSlash(path)
		if !fileIdentitySourcePath(name) {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("cannot read source path %s", name)
		}
		sources[name] = data
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return sources
}

func TestFileIdentitySourceContract(t *testing.T) {
	sources := fileIdentityReadSources(t)
	_, inventory := fileIdentityInventory(sources, fileIdentityExpected)
	problems := append(inventory, fileIdentityPinnedSites(sources, fileIdentitySites)...)
	for _, problem := range problems {
		t.Errorf("%s %s: %s", problem.file, problem.check, problem.detail)
	}
}

func TestFileIdentitySourceContractRejectsMutations(t *testing.T) {
	sources := fileIdentityReadSources(t)
	const credential = "installer_principal_credentials.go"
	const windows = "service_windows_task.go"
	const journal = "update_record.go"
	const moveAnchor = `if err != nil || !os.SameFile(before, before) || !privatePrincipalCredential(path, before) {
		// Windows FileInfo resolves its file ID lazily from the pathname. The
		// self-comparison pins it before the open, so a replacement that takes
		// the pathname afterwards fails the identity check below.
		return "", ErrLocalPrincipalCredentialsRefused
	}
	f, err := openPrincipalCredentialFile(path, os.O_RDONLY, 0)
	if err != nil {
		return "", ErrLocalPrincipalCredentialsRefused
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(before, opened) || !privatePrincipalCredential(path, opened) {`
	moveReplacement := strings.Replace(moveAnchor, " || !os.SameFile(before, before)", "", 1)
	moveReplacement = strings.Replace(moveReplacement, "!os.SameFile(before, opened)", "!os.SameFile(before, before) || !os.SameFile(before, opened)", 1)
	for _, mutant := range []struct {
		name, file, anchor, replacement string
		rules                           []string
		inventory, inventoryClean       bool
		add, remove                     bool
	}{
		{"credential_pin_removed", credential, " || !os.SameFile(before, before)", "", []string{"R2"}, true, false, false, false},
		{"credential_pin_moved", credential, moveAnchor, moveReplacement, []string{"R4"}, false, true, false, false},
		{"credential_wrong_identifier", credential, "os.SameFile(before, before)", "os.SameFile(opened, opened)", []string{"R2"}, false, false, false, false},
		{"credential_pin_discarded", credential,
			"if err != nil || !os.SameFile(before, before) || !privatePrincipalCredential(path, before) {",
			"os.SameFile(before, before)\n\tif err != nil || !privatePrincipalCredential(path, before) {",
			[]string{"R3"}, false, false, false, false},
		{"windows_pin_no_return", windows,
			"if !os.SameFile(before, before) {\n\t\treturn nil, nil, errors.New(\"task record identity\")\n\t}",
			"if !os.SameFile(before, before) {\n\t\t_ = before\n\t}", []string{"R3"}, false, false, false, false},
		{"windows_intervening_stat", windows, "if !os.SameFile(before, before) {",
			"os.Stat(path)\n\tif !os.SameFile(before, before) {", []string{"R4"}, false, false, false, false},
		{"journal_comparison_removed", journal, " || info != nil && !os.SameFile(info, opened)", "", []string{"R5"}, false, false, false, false},
		{"credential_second_lstat", credential, "f, err := openPrincipalCredentialFile(path, os.O_RDONLY, 0)",
			"before, err = os.Lstat(path)\n\tf, err := openPrincipalCredentialFile(path, os.O_RDONLY, 0)", []string{"R1"}, false, false, false, false},
		{"inventory_unlisted_file", "identity_unlisted.go", "", "package localcell\nimport \"os\"\nfunc f(a, b os.FileInfo) bool { return os.SameFile(a, b) }",
			nil, true, false, true, false},
		{"inventory_stale_entry", "service_systemd_adapter.go", "", "", nil, true, false, false, true},
		{"inventory_call_removed", "service_launchd.go", "!os.SameFile(info, opened)", "false", nil, true, false, false, false},
	} {
		t.Run(mutant.name, func(t *testing.T) {
			changed := make(map[string][]byte, len(sources))
			for name, data := range sources {
				changed[name] = data
			}
			switch {
			case mutant.add:
				if _, exists := changed[mutant.file]; exists {
					t.Fatal("new-file mutation path already exists")
				}
				changed[mutant.file] = []byte(mutant.replacement)
			case mutant.remove:
				if _, exists := changed[mutant.file]; !exists {
					t.Fatal("removed-file mutation path is absent")
				}
				delete(changed, mutant.file)
			default:
				// Normalize once before all multi-line anchor matching; Windows
				// checkouts carry CRLF while these literals use LF.
				original := bytes.ReplaceAll(changed[mutant.file], []byte("\r\n"), []byte("\n"))
				if bytes.Count(original, []byte(mutant.anchor)) != 1 {
					t.Fatal("mutation anchor must occur exactly once")
				}
				changed[mutant.file] = bytes.Replace(original, []byte(mutant.anchor), []byte(mutant.replacement), 1)
			}
			_, inventory := fileIdentityInventory(changed, fileIdentityExpected)
			pins := fileIdentityPinnedSites(changed, fileIdentitySites)
			for _, problem := range append(inventory, pins...) {
				if problem.check == "parse" {
					t.Fatal("mutation must parse; a syntax error is not a contract kill")
				}
			}
			if mutant.inventory && !fileIdentityHasProblem(inventory, mutant.file, "inventory") {
				t.Fatal("inventory did not reject mutated file")
			}
			if mutant.inventoryClean && len(inventory) != 0 {
				t.Fatal("count-preserving mutation changed the inventory verdict")
			}
			for _, rule := range mutant.rules {
				if !fileIdentityHasProblem(pins, mutant.file, rule) {
					t.Errorf("pinned-site check did not reject mutated file under %s", rule)
				}
			}
		})
	}
}

func fileIdentityHasProblem(problems []fileIdentityProblem, file, check string) bool {
	for _, problem := range problems {
		if problem.file == file && problem.check == check {
			return true
		}
	}
	return false
}

func TestFileIdentitySourceContractParsing(t *testing.T) {
	for _, sample := range []struct {
		name, source string
		want         fileIdentityCount
		problem      string
	}{
		{"os_alias", "package p\nimport filesystem \"os\"\nfunc f(a, b filesystem.FileInfo) { filesystem.SameFile(a, b); filesystem.SameFile(a, a) }", fileIdentityCount{2, 1}, ""},
		{"os_dot_import", "package p\nimport . \"os\"\nfunc f(a, b FileInfo) { SameFile(a, b) }", fileIdentityCount{}, "inventory"},
		{"comments_and_strings", "package p\nimport \"os\"\n// os.SameFile(a, b)\nvar s = \"os.SameFile(a, b)\"\nvar r = `os.SameFile(a, b)`", fileIdentityCount{}, ""},
		{"other_package_selector", "package p\nimport other \"example.com/other\"\nfunc f() { other.SameFile(a, b) }", fileIdentityCount{}, ""},
		{"parse_error", "package p\nfunc broken(", fileIdentityCount{}, "parse"},
	} {
		t.Run(sample.name, func(t *testing.T) {
			expected := make(map[string]fileIdentityCount)
			if sample.want.calls != 0 {
				expected["sample.go"] = sample.want
			}
			measured, problems := fileIdentityInventory(map[string][]byte{"sample.go": []byte(sample.source)}, expected)
			if measured["sample.go"] != sample.want {
				t.Fatal("synthetic inventory row differs from expected counts")
			}
			if sample.problem == "" && len(problems) != 0 || sample.problem != "" && !fileIdentityHasProblem(problems, "sample.go", sample.problem) {
				t.Fatal("synthetic source produced an unexpected contract verdict")
			}
		})
	}
	t.Run("crlf", func(t *testing.T) {
		sources := fileIdentityReadSources(t)
		const name = "installer_principal_credentials.go"
		lf := bytes.ReplaceAll(sources[name], []byte("\r\n"), []byte("\n"))
		lfSources := map[string][]byte{name: lf}
		crlfSources := map[string][]byte{name: bytes.ReplaceAll(lf, []byte("\n"), []byte("\r\n"))}
		expected := map[string]fileIdentityCount{name: fileIdentityExpected[name]}
		lfCounts, lfProblems := fileIdentityInventory(lfSources, expected)
		crlfCounts, crlfProblems := fileIdentityInventory(crlfSources, expected)
		if !reflect.DeepEqual(lfCounts, crlfCounts) || !reflect.DeepEqual(lfProblems, crlfProblems) || len(lfProblems) != 0 {
			t.Fatal("CRLF changed the registered inventory row or verdict")
		}
		lfPins := fileIdentityPinnedSites(lfSources, fileIdentitySites[:1])
		crlfPins := fileIdentityPinnedSites(crlfSources, fileIdentitySites[:1])
		if !reflect.DeepEqual(lfPins, crlfPins) || len(lfPins) != 0 {
			t.Fatal("CRLF changed the registered pinned-site verdict")
		}
	})
	t.Run("path_filter", func(t *testing.T) {
		sources := map[string][]byte{
			"ordinary_test.go":          []byte("invalid excluded source"),
			"testdata/example.go":       []byte("invalid excluded source"),
			"trust/testdata/example.go": []byte("invalid excluded source"),
			".hidden/example.go":        []byte("invalid excluded source"),
			"trust/.hidden/example.go":  []byte("invalid excluded source"),
			"trust/path_windows.go":     []byte("//go:build windows\n\npackage trust\nimport \"os\"\nfunc f(a, b os.FileInfo) { os.SameFile(a, b) }"),
		}
		expected := map[string]fileIdentityCount{"trust/path_windows.go": {1, 0}}
		measured, problems := fileIdentityInventory(sources, expected)
		if len(problems) != 0 || !reflect.DeepEqual(measured, expected) {
			t.Fatal("source path filter did not exclude fixtures and tests or include Windows source")
		}
	})
}
