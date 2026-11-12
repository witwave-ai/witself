package localcell

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
	"testing"
)

const maintenanceUnixAdapterBlock = `cell, fresh, unlock, err := serviceAdapterAdmission(a.uid, d, a.ARTIFACT)
if err != nil { return err }
defer unlock()
if err := cell.maintenanceFenceAllows(0); err != nil { return err }
if !reflect.DeepEqual(fresh, d) { return serviceSystemdError(ErrServiceOwnershipConflict, "apply:descriptor", "stale_plan") }`

var maintenanceUnixAdapterFiles = map[string]string{
	"systemdUserAdapter":  "service_systemd_adapter.go",
	"launchdAgentAdapter": "service_launchd.go",
}

// These independently written prefixes include every BASE privilege and Inspect
// refusal. The first effect remains after the complete admission and plan check.
var maintenanceUnixAdapterBoundaries = map[string]string{
	"systemdUserAdapter": `if plan.RequiredPrivilege != "user" { return serviceSystemdError(ErrServiceOwnershipConflict, "apply:privilege", "not_user") }
if _, err := a.Inspect(ctx, plan.Spec); err != nil { return err }
d := plan.Spec
` + strings.ReplaceAll(maintenanceUnixAdapterBlock, "ARTIFACT", "unitDir") + `
want, err := RenderSystemdUserUnit(d)`,
	"launchdAgentAdapter": `if plan.RequiredPrivilege != "user" { return serviceSystemdError(ErrServiceOwnershipConflict, "apply:privilege", "not_user") }
d := plan.Spec
obs, err := a.Inspect(ctx, d)
if err != nil && (!errors.Is(err, ErrServiceRegistrationFailed) || obs.Reason != "service_registration_failed") { return err }
owned := err == nil && obs.Registered && obs.Owned
` + strings.ReplaceAll(maintenanceUnixAdapterBlock, "ARTIFACT", "agentDir") + `
if owned { return nil }
want, err := RenderLaunchdAgentPlist(d)`,
}

func maintenanceUnixAdapterCellBinding(fn *ast.FuncDecl) *ast.Ident {
	if fn.Body == nil {
		return nil
	}
	for _, statement := range fn.Body.List {
		assignment, ok := statement.(*ast.AssignStmt)
		if !ok || assignment.Tok != token.DEFINE || len(assignment.Lhs) != 4 || len(assignment.Rhs) != 1 {
			continue
		}
		call, ok := assignment.Rhs[0].(*ast.CallExpr)
		if !ok {
			continue
		}
		callee, ok := call.Fun.(*ast.Ident)
		if ok && (callee.Name == "serviceAdapterAdmission" || (fn.Name.Name == "Remove" && callee.Name == "serviceAdapterRemovalAdmission")) {
			identifier, _ := assignment.Lhs[0].(*ast.Ident)
			return identifier
		}
	}
	return nil
}

func maintenanceUnixAdapterAdmissionOwner(filename string, fn *ast.FuncDecl) string {
	receiverName := updateReceiverName(fn)
	file, known := maintenanceUnixAdapterFiles[receiverName]
	if !known || filename != file || fn.Name.Name != "Apply" || fn.Type.TypeParams != nil || fn.Recv == nil || len(fn.Recv.List) != 1 {
		return "signature"
	}
	receiver := fn.Recv.List[0]
	if len(receiver.Names) != 1 || receiver.Names[0].Name != "a" || receiver.Names[0].Obj == nil || admissionNodeText(receiver.Type) != "*"+receiverName || admissionNodeText(fn.Type) != "func(ctx context.Context, plan ServicePlan) error" {
		return "signature"
	}
	want := admissionStatements(maintenanceUnixAdapterBoundaries[receiverName])
	if fn.Body == nil || len(fn.Body.List) < len(want) {
		return "position"
	}
	for index, statement := range want {
		if admissionNodeText(fn.Body.List[index]) != statement {
			return "position"
		}
	}
	cell := maintenanceUnixAdapterCellBinding(fn)
	if cell == nil || cell.Obj == nil {
		return "binding"
	}
	binding, ok := cell.Obj.Decl.(*ast.AssignStmt)
	if !ok {
		return "binding"
	}
	fresh, freshOK := binding.Lhs[1].(*ast.Ident)
	unlock, unlockOK := binding.Lhs[2].(*ast.Ident)
	if !freshOK || !unlockOK || fresh.Obj == nil || unlock.Obj == nil {
		return "binding"
	}
	counts := map[string]int{}
	valid := true
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.Ident:
			for _, expected := range []*ast.Ident{cell, fresh, unlock} {
				if node.Name == expected.Name {
					counts[expected.Name]++
					valid = valid && node.Obj == expected.Obj
				}
			}
			if node.Name == "serviceAdapterAdmission" {
				counts["helper"]++
			}
		case *ast.SelectorExpr:
			switch node.Sel.Name {
			case "maintenanceFenceAllows":
				counts["fence"]++
			case "own", "ownExisting":
				valid = false
			}
		case *ast.CallExpr:
			if id, ok := node.Fun.(*ast.Ident); ok && id.Name == "New" {
				valid = false
			}
		case *ast.DeferStmt:
			counts["defer"]++
		case *ast.GoStmt:
			valid = false
		}
		return true
	})
	if !valid || counts["cell"] != 2 || counts["fresh"] != 2 || counts["unlock"] != 2 || counts["helper"] != 1 || counts["fence"] != 1 || counts["defer"] != 1 {
		return "continuous_hold"
	}
	return ""
}

// Pin the entire small helper independently from production. This fixes the
// order, unchanged contention identity, descriptor binding, and release paths.
const maintenanceUnixAdapterHelperBody = `if owner == "" { return nil, ServiceDescriptor{}, nil, serviceSystemdError(ErrServiceOwnershipConflict, step+":descriptor", "owner") }
if err := serviceAdapterConfinementForStep(d.Root, artifactDir, step); err != nil { return nil, ServiceDescriptor{}, nil, err }
cell, err := New(Config{Root: d.Root, Timeout: 60 * time.Second})
if err != nil { return nil, ServiceDescriptor{}, nil, serviceSystemdError(ErrServiceOwnershipConflict, step+":root", "unavailable") }
lock, err := cell.own(false)
if err != nil {
 if errors.Is(err, ErrLocked) { return nil, ServiceDescriptor{}, nil, err }
 return nil, ServiceDescriptor{}, nil, serviceSystemdError(ErrServiceOwnershipConflict, step+":root", "unavailable")
}
fresh, err := cell.serviceDescriptorForOwner(owner)
if err != nil {
 _ = lock.Close()
 return nil, ServiceDescriptor{}, nil, serviceSystemdError(ErrServiceOwnershipConflict, step+":descriptor", "unavailable")
}
if !reflect.DeepEqual(windowsTaskImmutable(fresh), windowsTaskImmutable(d)) {
 _ = lock.Close()
 return nil, ServiceDescriptor{}, nil, serviceSystemdError(ErrServiceOwnershipConflict, step+":descriptor", "stale_identity")
}
return cell, fresh, func() { _ = lock.Close() }, nil`

func maintenanceUnixAdapterAdmissionContract(sources map[string]string) []string {
	var failures []string
	for receiver, filename := range maintenanceUnixAdapterFiles {
		file, err := parser.ParseFile(token.NewFileSet(), filename, strings.ReplaceAll(sources[filename], "\r\n", "\n"), 0)
		if err != nil {
			failures = append(failures, receiver+"_parse")
			continue
		}
		methods := map[string]*ast.FuncDecl{}
		owners := 0
		for _, declaration := range file.Decls {
			fn, ok := declaration.(*ast.FuncDecl)
			if !ok {
				ast.Inspect(declaration, func(node ast.Node) bool {
					if selector, ok := node.(*ast.SelectorExpr); ok && selector.Sel.Name == "maintenanceFenceAllows" {
						failures = append(failures, receiver+"_initializer")
					}
					return true
				})
				continue
			}
			if updateReceiverName(fn) == receiver {
				methods[fn.Name.Name] = fn
			}
			if fn.Name.Name == "Apply" && updateReceiverName(fn) == receiver {
				owners++
				if invariant := maintenanceUnixAdapterAdmissionOwner(filename, fn); invariant != "" {
					failures = append(failures, receiver+"_"+invariant)
				}
				if len(updateOrdinaryAdmissions(filename, fn)) != 1 {
					failures = append(failures, receiver+"_admission")
				}
				continue
			}
			removalOwner := fn.Name.Name == "Remove" && updateReceiverName(fn) == receiver && maintenanceRemovalAdmissionOwner(filename, fn) == ""
			ast.Inspect(fn, func(node ast.Node) bool {
				if selector, ok := node.(*ast.SelectorExpr); ok && selector.Sel.Name == "maintenanceFenceAllows" && !removalOwner {
					failures = append(failures, receiver+"_unexpected_fence")
				}
				if identifier, ok := node.(*ast.Ident); ok && identifier.Name == "serviceAdapterAdmission" {
					failures = append(failures, receiver+"_unexpected_helper")
				}
				return true
			})
		}
		if owners != 1 {
			failures = append(failures, receiver+"_owner_count")
		}
		// Follow receiver method calls from Apply, including cleanup closures.
		// Remove and its stop commands remain outside this reachable subgraph.
		visited := map[string]bool{}
		var visit func(string)
		visit = func(name string) {
			if visited[name] || methods[name] == nil {
				return
			}
			visited[name] = true
			ast.Inspect(methods[name].Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				owner, ok := selector.X.(*ast.Ident)
				if !ok || owner.Name != "a" {
					return true
				}
				if selector.Sel.Name == "bootout" {
					failures = append(failures, receiver+"_stop_cleanup")
				}
				if selector.Sel.Name == "command" {
					for _, argument := range call.Args {
						if text := admissionNodeText(argument); text == `"disable"` || text == `"bootout"` {
							failures = append(failures, receiver+"_stop_cleanup")
						}
					}
				}
				visit(selector.Sel.Name)
				return true
			})
		}
		visit("Apply")
	}
	file, err := parser.ParseFile(token.NewFileSet(), "service_adapter_admission.go", strings.ReplaceAll(sources["service_adapter_admission.go"], "\r\n", "\n"), 0)
	if err != nil {
		failures = append(failures, "helper_parse")
	} else {
		owners := 0
		for _, declaration := range file.Decls {
			fn, ok := declaration.(*ast.FuncDecl)
			if !ok || fn.Name.Name != "serviceAdapterAdmissionForStep" {
				continue
			}
			owners++
			want := admissionStatements(maintenanceUnixAdapterHelperBody)
			if fn.Recv != nil || admissionNodeText(fn.Type) != "func(owner string, d ServiceDescriptor, artifactDir, step string) (*Cell, ServiceDescriptor, func(), error)" || fn.Body == nil || len(fn.Body.List) != len(want) {
				failures = append(failures, "helper_order")
				continue
			}
			for index, statement := range want {
				if admissionNodeText(fn.Body.List[index]) != statement {
					failures = append(failures, "helper_order")
					break
				}
			}
		}
		if owners != 1 {
			failures = append(failures, "helper_owner_count")
		}
		ast.Inspect(file, func(node ast.Node) bool {
			if selector, ok := node.(*ast.SelectorExpr); ok && selector.Sel.Name == "maintenanceFenceAllows" {
				failures = append(failures, "helper_fence")
			}
			return true
		})
	}
	sort.Strings(failures)
	return failures
}

func unixServiceDescriptorOwnerSourceContract(t *testing.T) {
	t.Helper()
	sources := admissionReadSources(t)
	for _, ending := range []string{"LF", "CRLF"} {
		t.Run("source_"+ending, func(t *testing.T) {
			wrappers, ownerReaders, lockReaders := 0, 0, 0
			ownerCalls, lockOwnerCalls, helperCalls, nativeUIDs := 0, 0, 0, 0
			bodyMatches := func(fn *ast.FuncDecl, source string) bool {
				want := admissionStatements(source)
				if fn.Body == nil || len(fn.Body.List) != len(want) {
					return false
				}
				for index, statement := range want {
					if admissionNodeText(fn.Body.List[index]) != statement {
						return false
					}
				}
				return true
			}
			for filename, source := range sources {
				source = strings.ReplaceAll(source, "\r\n", "\n")
				if ending == "CRLF" {
					source = strings.ReplaceAll(source, "\n", "\r\n")
				}
				file, err := parser.ParseFile(token.NewFileSet(), filename, source, 0)
				if err != nil {
					t.Fatal("Descriptor source could not parse")
				}
				for _, declaration := range file.Decls {
					fn, _ := declaration.(*ast.FuncDecl)
					owner := ""
					if fn != nil {
						owner = fn.Name.Name
					}
					switch owner {
					case "ServiceDescriptor":
						wrappers++
						if filename != "service_descriptor.go" || updateReceiverName(fn) != "Cell" || !bodyMatches(fn, `var d ServiceDescriptor
if err := c.validateRoot(false); err != nil { return d, err }
return c.readServiceDescriptor()`) {
							t.Fatal("Public descriptor wrapper changed")
						}
					case "serviceDescriptorForOwner":
						ownerReaders++
						if filename != "service_descriptor.go" || updateReceiverName(fn) != "Cell" || !bodyMatches(fn, `var d ServiceDescriptor
if err := c.validateRoot(false); err != nil { return d, err }
return c.readServiceDescriptorForOwner(owner)`) {
							t.Fatal("Owner descriptor wrapper changed")
						}
					case "readServiceDescriptor":
						lockReaders++
						if filename != "service_descriptor.go" || updateReceiverName(fn) != "Cell" || !bodyMatches(fn, `return c.readServiceDescriptorForOwner("")`) {
							t.Fatal("Lock held descriptor wrapper changed")
						}
					}
					ast.Inspect(declaration, func(node ast.Node) bool {
						if call, ok := node.(*ast.CallExpr); ok {
							if selector, ok := call.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "serviceDescriptorForOwner" {
								ownerCalls++
								if filename != "service_adapter_admission.go" || owner != "serviceAdapterAdmissionForStep" || admissionNodeText(call) != "cell.serviceDescriptorForOwner(owner)" {
									t.Fatal("Owner descriptor call escaped admission")
								}
							}
							if selector, ok := call.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "readServiceDescriptorForOwner" {
								lockOwnerCalls++
								want := `c.readServiceDescriptorForOwner("")`
								if owner == "serviceDescriptorForOwner" {
									want = "c.readServiceDescriptorForOwner(owner)"
								}
								if filename != "service_descriptor.go" || fn == nil || updateReceiverName(fn) != "Cell" || (owner != "serviceDescriptorForOwner" && owner != "readServiceDescriptor") || admissionNodeText(call) != want {
									t.Fatal("Lock held owner descriptor call escaped wrappers")
								}
							}
							if identifier, ok := call.Fun.(*ast.Ident); ok && identifier.Name == "serviceAdapterAdmission" {
								helperCalls++
								if fn == nil || maintenanceUnixAdapterAdmissionOwner(filename, fn) != "" || len(call.Args) != 3 || admissionNodeText(call.Args[0]) != "a.uid" {
									t.Fatal("Adapter owner call escaped Apply")
								}
							}
						}
						literal, ok := node.(*ast.CompositeLit)
						if !ok {
							return true
						}
						typ := admissionNodeText(literal.Type)
						if typ != "systemdUserAdapter" && typ != "launchdAgentAdapter" {
							return true
						}
						for _, element := range literal.Elts {
							pair, ok := element.(*ast.KeyValueExpr)
							if !ok || admissionNodeText(pair.Key) != "uid" {
								continue
							}
							nativeUIDs++
							if (typ == "systemdUserAdapter" && filename != "service_systemd_linux.go") || (typ == "launchdAgentAdapter" && filename != "service_launchd_darwin.go") {
								t.Fatal("Adapter owner assignment escaped native constructor")
							}
						}
						return true
					})
				}
			}
			if wrappers != 1 || ownerReaders != 1 || lockReaders != 1 || ownerCalls != 1 || lockOwnerCalls != 2 || helperCalls != 2 || nativeUIDs != 2 {
				t.Fatal("Descriptor source owner inventory changed")
			}
		})
	}
}

func TestUnixServiceMaintenanceAdmissionSourceContract(t *testing.T) {
	sources := admissionReadSources(t)
	for _, ending := range []string{"LF", "CRLF"} {
		t.Run(ending, func(t *testing.T) {
			input := serviceAdmissionClone(sources)
			for name, source := range input {
				input[name] = strings.ReplaceAll(source, "\r\n", "\n")
				if ending == "CRLF" {
					input[name] = strings.ReplaceAll(input[name], "\n", "\r\n")
				}
			}
			if failures := maintenanceUnixAdapterAdmissionContract(input); len(failures) != 0 {
				t.Fatalf("Unix admission source invariants failed %v", failures)
			}
		})
	}
	statement := func(source string) string { return admissionIndent(admissionStatements(source)[0]) }
	gate := statement(maintenanceAdapterAdmissionStatement)
	mutable := statement(`if !reflect.DeepEqual(fresh, d) { return serviceSystemdError(ErrServiceOwnershipConflict, "apply:descriptor", "stale_plan") }`)
	type mutation struct {
		name, filename, owner, invariant string
		change                           func(string) string
	}
	var fixtures []mutation
	for receiver, filename := range maintenanceUnixAdapterFiles {
		replace := func(old, replacement string) func(string) string {
			return func(body string) string { return strings.Replace(body, old, replacement, 1) }
		}
		appendFixture := func(name, invariant string, change func(string) string) {
			fixtures = append(fixtures, mutation{receiver + "_" + name, filename, "Apply", receiver + "_" + invariant, change})
		}
		appendFixture("missing_fence", "position", replace(gate, ""))
		appendFixture("positive_generation", "position", replace("maintenanceFenceAllows(0)", "maintenanceFenceAllows(1)"))
		appendFixture("variable_generation", "position", replace("maintenanceFenceAllows(0)", "maintenanceFenceAllows(generation)"))
		appendFixture("discarded_error", "position", replace(gate, "_ = cell.maintenanceFenceAllows(0)"))
		appendFixture("cell_receiver", "position", replace("cell.maintenanceFenceAllows", "c.maintenanceFenceAllows"))
		appendFixture("helper_wrong_owner", "position", replace("serviceAdapterAdmission(a.uid", "serviceAdapterAdmission(other"))
		appendFixture("helper_wrong_directory", "position", replace("serviceAdapterAdmission(a.uid, d, a.", "serviceAdapterAdmission(a.uid, d, other."))
		appendFixture("missing_defer", "position", replace("defer unlock()", ""))
		appendFixture("wrong_receiver", "owner_count", replace("*"+receiver, "*OtherAdapter"))
		appendFixture("wrong_parameter", "signature", replace("plan ServicePlan", "plan OtherPlan"))
		appendFixture("named_result", "signature", replace(") error {", ") (result error) {"))
		appendFixture("discarded_admission_error", "position", replace(statement("if err != nil { return err }"), statement("if err != nil { return nil }")))
		appendFixture("fence_method_value", "position", replace(gate, "check := cell.maintenanceFenceAllows; _ = check"))
		appendFixture("fence_alias", "position", replace(gate, "alias := cell; if err := alias.maintenanceFenceAllows(0); err != nil { return err }"))
		appendFixture("fence_free_wrapper", "position", replace(gate, "if err := checkFence(cell); err != nil { return err }"))
		appendFixture("manager_before_fence", "position", replace(gate, "_, _ = a.command(ctx, manager, command)\n"+gate))
		appendFixture("file_before_fence", "position", replace(gate, "_, _ = os.OpenFile(path, flags, mode)\n"+gate))

		appendFixture("mutable_before_fence", "position", func(body string) string {
			body = strings.Replace(body, mutable, "", 1)
			return strings.Replace(body, gate, mutable+"\n"+gate, 1)
		})
		appendFixture("missing_mutable", "position", replace(mutable, ""))
		appendFixture("before_privilege", "position", func(body string) string {
			block := strings.ReplaceAll(maintenanceUnixAdapterBlock, "ARTIFACT", "unitDir")
			if receiver == "launchdAgentAdapter" {
				block = strings.ReplaceAll(maintenanceUnixAdapterBlock, "ARTIFACT", "agentDir")
			}
			for _, text := range admissionStatements(block) {
				body = strings.Replace(body, admissionIndent(text), "", 1)
			}
			return strings.Replace(body, "{", "{\n"+block, 1)
		})
		appendFixture("after_directory_creation", "position", func(body string) string {
			return strings.Replace(body, gate, `if err := a.inspectDirectories(true); err != nil { return err }`+"\n"+gate, 1)
		})
		for _, extra := range []struct{ name, text string }{
			{"early_unlock", "unlock()"},
			{"cell_alias", "alias := cell; _ = alias"},
			{"fresh_alias", "alias := fresh; _ = alias"},
			{"cell_reassigned", "cell = other"},
			{"fresh_captured", "_ = func() { _ = fresh }"},
			{"second_helper", "_, _, _, _ = serviceAdapterAdmission(a.uid, d, outside)"},
			{"second_lock", "_, _ = other.own(false)"},
			{"second_cell", "_, _ = New(Config{})"},
			{"late_fence", "_ = cell.maintenanceFenceAllows(0)"},
		} {
			appendFixture(extra.name, "continuous_hold", func(body string) string {
				return strings.TrimSuffix(body, "}") + "\n" + extra.text + "\n}"
			})
		}
		for _, method := range []string{"Remove", "Plan", "Inspect"} {
			for _, extra := range []struct{ name, text, invariant string }{
				{"fence", "_ = cell.maintenanceFenceAllows(0)", "unexpected_fence"},
				{"helper", "_, _, _, _ = serviceAdapterAdmission(a.uid, d, outside)", "unexpected_helper"},
			} {
				fixtures = append(fixtures, mutation{receiver + "_" + method + "_" + extra.name, filename, method, receiver + "_" + extra.invariant, func(body string) string {
					return strings.Replace(body, "{", "{\n"+extra.text, 1)
				}})
			}
		}
		fixtures = append(fixtures, mutation{receiver + "_cleanup_stop", filename, "cleanupPartial", receiver + "_stop_cleanup", func(body string) string {
			return strings.Replace(body, "{", "{\n_, _ = a.command(ctx, manager, \"disable\")", 1)
		}})
		if receiver == "launchdAgentAdapter" {
			appendFixture("owned_before_admission", "position", func(body string) string {
				owned := statement("if owned { return nil }")
				body = strings.Replace(body, owned, "", 1)
				return strings.Replace(body, "cell, fresh, unlock, err :=", owned+"\ncell, fresh, unlock, err :=", 1)
			})
			fixtures = append(fixtures, mutation{receiver + "_cleanup_bootout", filename, "cleanupPartial", receiver + "_stop_cleanup", func(body string) string {
				return strings.Replace(body, "{", "{\n_ = a.bootout(ctx, d)", 1)
			}})
		}
	}
	for _, extra := range []struct{ name, old, replacement string }{
		{"helper_owner_missing", statement(`if owner == "" { return nil, ServiceDescriptor{}, nil, serviceSystemdError(ErrServiceOwnershipConflict, step+":descriptor", "owner") }`), ""},
		{"helper_confinement_missing", statement(`if err := serviceAdapterConfinementForStep(d.Root, artifactDir, step); err != nil { return nil, ServiceDescriptor{}, nil, err }`), ""},
		{"helper_double_new", "cell, err := New", "_, _ = New(Config{})\ncell, err := New"},
		{"helper_no_lock", "lock, err := cell.own(false)", "lock, err := fakeLock, fakeError"},
		{"helper_positive_own", "cell.own(false)", "cell.own(true)"},
		{"helper_wrapped_contention", "if errors.Is(err, ErrLocked) {\n\t\t\treturn nil, ServiceDescriptor{}, nil, err", "if errors.Is(err, ErrLocked) {\n\t\t\treturn nil, ServiceDescriptor{}, nil, ErrServiceOwnershipConflict"},
		{"helper_public_reader", "cell.serviceDescriptorForOwner(owner)", "cell.ServiceDescriptor()"},
		{"helper_no_immutable", statement(`if !reflect.DeepEqual(windowsTaskImmutable(fresh), windowsTaskImmutable(d)) { _ = lock.Close(); return nil, ServiceDescriptor{}, nil, serviceSystemdError(ErrServiceOwnershipConflict, step+":descriptor", "stale_identity") }`), ""},
		{"helper_missing_failure_release", "_ = lock.Close()", "_ = lock"},
		{"helper_return_other_cell", "return cell, fresh, func()", "return other, fresh, func()"},
		{"helper_write", "cell, err := New", "_ = os.Remove(path)\ncell, err := New"},
		{"helper_manager", "cell, err := New", "_, _ = a.command(ctx)\ncell, err := New"},
		{"helper_fence", "fresh, err :=", "_ = cell.maintenanceFenceAllows(0)\nfresh, err :="},
	} {
		fixtures = append(fixtures, mutation{extra.name, "service_adapter_admission.go", "serviceAdapterAdmissionForStep", "helper_order", func(body string) string {
			return strings.Replace(body, extra.old, extra.replacement, 1)
		}})
	}
	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			original := admissionNodeText(serviceAdmissionFunction(t, fixture.filename, sources[fixture.filename], fixture.owner))
			changed := fixture.change(original)
			if changed == original {
				t.Fatal("Unix source mutation missed its raising site")
			}
			for _, ending := range []string{"LF", "CRLF"} {
				t.Run(ending, func(t *testing.T) {
					input := serviceAdmissionClone(sources)
					input[fixture.filename] = admissionMutateFunction(t, sources[fixture.filename], fixture.owner, func(string) string { return changed })
					if ending == "CRLF" {
						input[fixture.filename] = strings.ReplaceAll(strings.ReplaceAll(input[fixture.filename], "\r\n", "\n"), "\n", "\r\n")
					}
					failures := maintenanceUnixAdapterAdmissionContract(input)
					for _, failure := range failures {
						if strings.Contains(failure, "parse") {
							t.Fatal("Unix source mutation is not parse valid")
						}
					}
					if !containsAdmissionFailure(failures, fixture.invariant) {
						t.Fatalf("Unix source mutation missed invariant %s", fixture.invariant)
					}
					if fixture.owner == "Apply" && strings.Contains(changed, "maintenanceFenceAllows") {
						guardInput := map[string]string{"update_fence.go": serviceAdmissionFoundation, fixture.filename: input[fixture.filename]}
						if len(updateFoundationViolations(guardInput)) == 0 {
							t.Fatal("Unix source mutation escaped foundation guard")
						}
					}
				})
			}
		})
	}
}
