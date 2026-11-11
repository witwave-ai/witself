package localcell

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
	"testing"
)

type updateDeclarationKey struct {
	file, receiver, name string
}

func updateReceiverName(fn *ast.FuncDecl) string {
	if fn.Recv == nil {
		return ""
	}
	receiver := fn.Recv.List[0].Type
	for {
		switch node := receiver.(type) {
		case *ast.StarExpr:
			receiver = node.X
		case *ast.ParenExpr:
			receiver = node.X
		case *ast.IndexExpr:
			receiver = node.X
		case *ast.IndexListExpr:
			receiver = node.X
		case *ast.Ident:
			return node.Name
		default:
			return "<unknown>"
		}
	}

}

// updateFoundationViolations reads source, not the build-selected package: dark
// foundations must remain unreachable on every supported operating system.
func updateFoundationViolations(sourceText map[string]string) []string {
	var violations []string
	sources := make(map[string]*ast.File)
	helpers := make(map[string]bool)
	inventory := map[updateDeclarationKey]bool{
		{"files.go", "Cell", "readUpdateJournal"}:  true,
		{"files.go", "", "validUpdateJournalStep"}: true,
		{"types.go", "updateState", "valid"}:       true,
	}
	for key := range inventory {
		helpers[key.name] = true
	}
	isUpdateFile := func(name string) bool {
		return name == "update_record.go" || name == "update_fence.go"
	}
	for name, source := range sourceText {
		if strings.HasSuffix(name, "_test.go") || !strings.HasSuffix(name, ".go") {
			continue
		}
		// Windows checkouts use CRLF. Normalize before any source contracts run.
		source = strings.ReplaceAll(source, "\r\n", "\n")
		file, err := parser.ParseFile(token.NewFileSet(), name, source, 0)
		if err != nil {
			violations = append(violations, name+": cannot parse local cell source")
			continue
		}
		sources[name] = file
		if !isUpdateFile(name) {
			continue
		}
		for _, declaration := range file.Decls {
			switch declaration := declaration.(type) {
			case *ast.FuncDecl:
				inventory[updateDeclarationKey{name, updateReceiverName(declaration), declaration.Name.Name}] = true
				helpers[declaration.Name.Name] = true
				if declaration.Name.IsExported() {
					violations = append(violations, name+": update helper became public")
				}
				if declaration.Name.Name == "init" {
					violations = append(violations, name+": update foundations contain init")
				}
			case *ast.GenDecl:
				for _, spec := range declaration.Specs {
					var names []*ast.Ident
					switch spec := spec.(type) {
					case *ast.ValueSpec:
						names = spec.Names
					case *ast.TypeSpec:
						names = []*ast.Ident{spec.Name}
					}
					for _, identifier := range names {
						inventory[updateDeclarationKey{name, "", identifier.Name}] = true
						helpers[identifier.Name] = true
						if identifier.IsExported() {
							violations = append(violations, name+": update declaration became public")
						}
					}
				}
			}
		}
	}
	if !isUpdateFilePresent(sources) {
		violations = append(violations, "update helper inventory is empty")
	}
	for name, file := range sources {
		// The parser resolves lexical bindings even for snippets without imports
		// or complete package types. Register declarations inside functions only;
		// a package variable alias is never mistaken for a permitted local.
		locals := make(map[ast.Node]bool)
		registerLocals := func(node ast.Node) {
			ast.Inspect(node, func(child ast.Node) bool {
				var identifiers []*ast.Ident
				switch child := child.(type) {
				case *ast.Field:
					identifiers = child.Names
				case *ast.ValueSpec:
					identifiers = child.Names
				case *ast.TypeSpec:
					identifiers = []*ast.Ident{child.Name}
				case *ast.AssignStmt:
					if child.Tok == token.DEFINE {
						for _, expression := range child.Lhs {
							if identifier, ok := expression.(*ast.Ident); ok {
								identifiers = append(identifiers, identifier)
							}
						}
					}
				case *ast.RangeStmt:
					if child.Tok == token.DEFINE {
						for _, expression := range []ast.Expr{child.Key, child.Value} {
							if identifier, ok := expression.(*ast.Ident); ok {
								identifiers = append(identifiers, identifier)
							}
						}
					}
				}
				for _, identifier := range identifiers {
					if identifier.Obj != nil {
						if declaration, ok := identifier.Obj.Decl.(ast.Node); ok {
							locals[declaration] = true
						}
					}
				}
				return true
			})
		}
		ast.Inspect(file, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.FuncDecl:
				registerLocals(node)
			case *ast.FuncLit:
				registerLocals(node)
			case *ast.TypeSpec:
				if strings.HasPrefix(node.Name.Name, "Update") {
					violations = append(violations, name+": public update type was reintroduced")
				}
			}
			return true
		})
		inspect := func(node ast.Node, owner string, admissions map[*ast.SelectorExpr]bool) {
			ast.Inspect(node, func(child ast.Node) bool {
				switch child := child.(type) {
				case *ast.SelectorExpr:
					if admissions[child] {
						return false
					}
					if helpers[child.Sel.Name] {
						violations = append(violations, fmt.Sprintf("%s: %s selects update helper %s", name, owner, child.Sel.Name))
						if child.Sel.Name == "maintenanceFenceAllows" {
							for _, declaration := range file.Decls {
								fn, ok := declaration.(*ast.FuncDecl)
								if !ok || fn.Name.Name != owner {
									continue
								}
								invariant := maintenanceAdmissionOwner(name, fn)
								if invariant == "" {
									invariant = "literal_zero_direct_error_return"
								}
								violations = append(violations, owner+"_"+invariant)
							}
						}
					}
				case *ast.Ident:
					var declaration ast.Node
					if child.Obj != nil {
						declaration, _ = child.Obj.Decl.(ast.Node)
					}
					if helpers[child.Name] && !locals[declaration] {
						violations = append(violations, fmt.Sprintf("%s: %s references update helper %s", name, owner, child.Name))
					}
				}
				return true
			})
		}
		for _, declaration := range file.Decls {
			switch declaration := declaration.(type) {
			case *ast.FuncDecl:
				if declaration.Name.Name == "Update" || declaration.Name.Name == "repairUpdate" {
					violations = append(violations, name+": public update or Repair bridge was reintroduced")
				}
				if inventory[updateDeclarationKey{name, updateReceiverName(declaration), declaration.Name.Name}] {
					continue
				}
				if helpers[declaration.Name.Name] {
					violations = append(violations, name+": non-inventoried declaration reuses update helper "+declaration.Name.Name)
				}
				if declaration.Body != nil {
					inspect(declaration.Body, declaration.Name.Name, updateOrdinaryAdmissions(name, declaration))
				}
			case *ast.GenDecl:
				if isUpdateFile(name) {
					// A foundation initializer may bind a function value as a seam,
					// but a call there would run update code at package load with
					// no shipped reference for the checks below to find.
					for _, spec := range declaration.Specs {
						value, ok := spec.(*ast.ValueSpec)
						if !ok {
							continue
						}
						for _, initializer := range value.Values {
							ast.Inspect(initializer, func(child ast.Node) bool {
								switch child.(type) {
								case *ast.FuncLit:
									return false
								case *ast.CallExpr:
									violations = append(violations, name+": update foundations run code at package initialization")
								}
								return true
							})
						}
					}
					continue
				}
				for _, spec := range declaration.Specs {
					if value, ok := spec.(*ast.ValueSpec); ok {
						for _, initializer := range value.Values {
							inspect(initializer, "package initializer", nil)
						}
					}
				}
			}
		}
	}
	sort.Strings(violations)
	return violations
}

func isUpdateFilePresent(sources map[string]*ast.File) bool {
	for _, name := range []string{"update_record.go", "update_fence.go"} {
		if file := sources[name]; file != nil && len(file.Decls) > 0 {
			return true
		}
	}
	return false
}

// Keep update execution unreachable. Only four ordinary maintenance admissions
// are permitted; their exact lock position is checked by the admission contract.
func TestUpdateFoundationsRemainUnwired(t *testing.T) {
	sources := admissionReadSources(t)
	if failures := maintenanceAdmissionOwnerInventory(sources); len(failures) != 0 {
		t.Fatalf("update foundations are wired: %s", strings.Join(failures, "; "))
	}
	for _, ending := range []string{"LF", "CRLF"} {
		t.Run(ending, func(t *testing.T) {
			if ending == "CRLF" {
				for name, source := range sources {
					sources[name] = strings.ReplaceAll(strings.ReplaceAll(source, "\r\n", "\n"), "\n", "\r\n")
				}
			}
			if violations := updateFoundationViolations(sources); len(violations) != 0 {
				t.Fatalf("update foundations are wired: %s", strings.Join(violations, "; "))
			}
		})
	}
}

func TestUpdateFoundationsGuardFixtures(t *testing.T) {
	const foundation = `package localcell
type maintenanceFence struct{}
const updateGuardLimit = 1
func (c *Cell) maintenanceFenceAllows(generation int) error { return nil }
`
	cases := []struct {
		name, shipped, extraFoundation string
		flagged                        bool
	}{
		{"shipped_alias_M13", `var reviewFenceGate = (*Cell).maintenanceFenceAllows; func (c *Cell) Start() { _ = reviewFenceGate(c, 0) }`, "", true},
		{"foundation_alias_called_from_shipped", `func (c *Cell) Start() { _ = reviewFenceGate(c, 0) }`, `var reviewFenceGate = (*Cell).maintenanceFenceAllows`, true},
		{"free_valid_MV", `func valid(c *Cell) error { return c.maintenanceFenceAllows(0) }; func (c *Cell) Start() { _ = valid(c) }`, "", true},
		{"other_receiver_reuses_helper", `func (other Other) maintenanceFenceAllows(generation int) error { return nil }`, "", true},
		{"same_receiver_wrong_file", `func (c *Cell) readUpdateJournal() {}`, "", true},
		{"initializer_calls_helper", `var result = (&Cell{}).maintenanceFenceAllows(0)`, "", true},
		{"initializer_method_value", `var result = (&Cell{}).maintenanceFenceAllows`, "", true},
		{"initializer_function_literal", `var result = func(c *Cell) error { return c.maintenanceFenceAllows(0) }`, "", true},
		{"body_method_value", `func shipped(c *Cell) { result := c.maintenanceFenceAllows; _ = result }`, "", true},
		{"body_direct_call", `func shipped(c *Cell) { _ = c.maintenanceFenceAllows(0) }`, "", true},
		{"foundation_init", "", `func init() {}`, true},
		{"foundation_initializer_call", "", `var ran = (&Cell{}).maintenanceFenceAllows(0)`, true},
		{"foundation_initializer_invoked_literal", "", `var ran = func() error { return nil }()`, true},
		{"package_constant_reference", `const shipped = updateGuardLimit`, "", true},
		{"package_type_reference", `func shipped() { _ = maintenanceFence{} }`, "", true},
		{"local_valid", `func shipped() bool { var valid bool; valid = true; return valid }`, "", false},
		{"helper_named_parameter", `func shipped(maintenanceFenceAllows func(int) error) error { return maintenanceFenceAllows(0) }`, "", false},
		{"initializer_local_parameter", `var result = func(maintenanceFenceAllows func(int) error) error { return maintenanceFenceAllows(0) }`, "", false},
		{"local_short_declaration", `func shipped() { maintenanceFenceAllows := func() {}; maintenanceFenceAllows() }`, "", false},
		{"helpers_call_helpers", "", `func privateHelper(c *Cell) error { return c.maintenanceFenceAllows(0) }`, false},
		{"foundation_seam_value", "", `var gate = (*Cell).maintenanceFenceAllows`, false},
		{"foundation_seam_literal", "", `var gate = func(c *Cell) error { return c.maintenanceFenceAllows(0) }`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sources := map[string]string{
				"update_fence.go": foundation + tc.extraFoundation,
				"shipped.go":      "package localcell\n" + tc.shipped,
			}
			for _, ending := range []string{"LF", "CRLF"} {
				t.Run(ending, func(t *testing.T) {
					if ending == "CRLF" {
						for name, source := range sources {
							sources[name] = strings.ReplaceAll(source, "\n", "\r\n")
						}
					}
					violations := updateFoundationViolations(sources)
					// A fixture that does not parse would be flagged for the wrong reason.
					if strings.Contains(strings.Join(violations, "; "), "cannot parse") {
						t.Fatalf("fixture does not parse: %v", violations)
					}
					if got := len(violations) != 0; got != tc.flagged {
						t.Fatalf("flagged = %v, want %v: %v", got, tc.flagged, violations)
					}
				})
			}
		})
	}
}

// updateOrdinaryAdmissions exempts only one direct, literal-zero admission in
// each explicitly owned lifecycle body. It never exempts the body itself.
func updateOrdinaryAdmissions(file string, fn *ast.FuncDecl) map[*ast.SelectorExpr]bool {
	if maintenanceAdmissionOwner(file, fn) != "" {
		return nil
	}
	receiver := fn.Recv.List[0]
	var admitted []*ast.SelectorExpr
	for _, statement := range fn.Body.List {
		conditional, ok := statement.(*ast.IfStmt)
		if !ok || conditional.Else != nil || len(conditional.Body.List) != 1 {
			continue
		}
		assignment, ok := conditional.Init.(*ast.AssignStmt)
		if !ok || assignment.Tok != token.DEFINE || len(assignment.Lhs) != 1 || len(assignment.Rhs) != 1 {
			continue
		}
		errIdent, ok := assignment.Lhs[0].(*ast.Ident)
		if !ok || errIdent.Name != "err" || errIdent.Obj == nil {
			continue
		}
		call, ok := assignment.Rhs[0].(*ast.CallExpr)
		if !ok || len(call.Args) != 1 || call.Ellipsis.IsValid() {
			continue
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "maintenanceFenceAllows" {
			continue
		}
		owner, ok := selector.X.(*ast.Ident)
		if !ok || owner.Obj == nil || owner.Obj != receiver.Names[0].Obj {
			continue
		}
		generation, ok := call.Args[0].(*ast.BasicLit)
		if !ok || generation.Kind != token.INT || generation.Value != "0" {
			continue
		}
		condition, ok := conditional.Cond.(*ast.BinaryExpr)
		if !ok || condition.Op != token.NEQ {
			continue
		}
		left, leftOK := condition.X.(*ast.Ident)
		right, rightOK := condition.Y.(*ast.Ident)
		returned, returnOK := conditional.Body.List[0].(*ast.ReturnStmt)
		if !leftOK || !rightOK || left.Obj != errIdent.Obj || right.Name != "nil" || right.Obj != nil || !returnOK || len(returned.Results) != 1 {
			continue
		}
		result, ok := returned.Results[0].(*ast.Ident)
		if !ok || result.Obj != errIdent.Obj {
			continue
		}
		admitted = append(admitted, selector)
	}
	if len(admitted) != 1 {
		return nil
	}
	return map[*ast.SelectorExpr]bool{admitted[0]: true}
}
