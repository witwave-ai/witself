package localcell

import (
	"go/ast"
	"go/token"
	"sort"
	"strings"
	"testing"
)

// This is a whole-body pin, not a search for an admission anywhere in a branch.
const maintenanceTransitionSetterBody = `d, err := c.serviceDescriptor(ctx)
if err != nil { return err }
if d.Faulted && state == "running" { return ErrServiceDesiredState }
if state == "running" {
 if d.DesiredState == "maintenance" { return ErrServiceDesiredState }
 if _, err := os.Lstat(c.path("service", "transition.json")); !errors.Is(err, os.ErrNotExist) { return ErrServiceDesiredState }
 ` + maintenanceReleasedAdmissionStatement + `
}
d.DesiredState, d.Revision = state, d.Revision+1
return c.writeServiceDescriptor(d)`

var maintenanceTransitionCallerPrefixes = map[string]string{
	"SetServiceDesiredState": `if !validServiceDesiredState(state) { return ErrServiceDesiredState }
if err := ctx.Err(); err != nil { return err }
lock, err := c.own(ctx, false)
if err != nil { return err }
defer func() { _ = lock.Close() }()
return c.setServiceDesiredStateLocked(ctx, state)`,
	"stopWithServiceFence": `ctx, cancel := c.serviceShutdownDeadline(ctx)
defer cancel()
lock, err := c.own(context.Background(), false)
if err != nil { return err }
defer func() { _ = lock.Close() }()
if fence {
 if err := c.setServiceDesiredStateLocked(context.Background(), "stopped"); err != nil && !errors.Is(err, os.ErrNotExist) { return err }
}`,
	"Remove": `ctx, cancel := c.serviceShutdownDeadline(ctx)
defer cancel()
if err := c.exitServiceHostForRemoval(ctx); err != nil { return err }
lock, err := c.ownExisting(context.Background())
if err != nil { return err }
defer func() { _ = lock.Close() }()
` + maintenanceAdmissionStatement + `
if err := c.setServiceDesiredStateLocked(context.Background(), "stopped"); err != nil && !errors.Is(err, os.ErrNotExist) { return err }`,
}

func maintenanceTransitionSetterOwner(file string, fn *ast.FuncDecl) string {
	if file != "service_descriptor.go" || fn.Name.Name != "setServiceDesiredStateLocked" || fn.Type.TypeParams != nil || fn.Recv == nil || len(fn.Recv.List) != 1 {
		return "signature"
	}
	receiver := fn.Recv.List[0]
	pointer, pointerOK := receiver.Type.(*ast.StarExpr)
	if !pointerOK || len(receiver.Names) != 1 || receiver.Names[0].Name != "c" || receiver.Names[0].Obj == nil {
		return "signature"
	}
	typ, typeOK := pointer.X.(*ast.Ident)
	if !typeOK || typ.Name != "Cell" || admissionNodeText(&ast.FuncType{Params: fn.Type.Params}) != "func(ctx context.Context, state string)" {
		return "signature"
	}
	if fn.Type.Results == nil || len(fn.Type.Results.List) != 1 || len(fn.Type.Results.List[0].Names) != 0 {
		return "signature"
	}
	result, resultOK := fn.Type.Results.List[0].Type.(*ast.Ident)
	if !resultOK || result.Name != "error" {
		return "signature"
	}
	want := admissionStatements(maintenanceTransitionSetterBody)
	if fn.Body == nil || len(fn.Body.List) != 6 {
		return "position"
	}
	for index, statement := range want {
		if admissionNodeText(fn.Body.List[index]) != statement {
			return "position"
		}
	}
	if !maintenanceTransitionSetterInherits(fn) {
		return "inherited_hold"
	}
	return ""
}

func maintenanceTransitionSetterInherits(fn *ast.FuncDecl) bool {
	if fn.Body == nil {
		return false
	}
	valid := true
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.SelectorExpr:
			if node.Sel.Name == "own" || node.Sel.Name == "ownExisting" {
				valid = false
			}
		case *ast.Ident:
			if node.Name == "lock" {
				valid = false
			}
		case *ast.DeferStmt, *ast.GoStmt:
			valid = false
		}
		return true
	})
	return valid
}

func maintenanceTransitionCallerHold(fn *ast.FuncDecl) bool {
	want, known := maintenanceTransitionCallerPrefixes[fn.Name.Name]
	if !known || fn.Body == nil || fn.Type.TypeParams != nil || fn.Recv == nil || len(fn.Recv.List) != 1 {
		return false
	}
	receiver := fn.Recv.List[0]
	pointer, pointerOK := receiver.Type.(*ast.StarExpr)
	if !pointerOK || len(receiver.Names) != 1 || receiver.Names[0].Name != "c" || receiver.Names[0].Obj == nil {
		return false
	}
	typ, typeOK := pointer.X.(*ast.Ident)
	if !typeOK || typ.Name != "Cell" {
		return false
	}
	parameters := map[string]string{
		"SetServiceDesiredState": "ctx context.Context, state string",
		"stopWithServiceFence":   "ctx context.Context, fence bool",
		"Remove":                 "ctx context.Context, options RemoveOptions",
	}
	if admissionNodeText(fn.Type) != "func("+parameters[fn.Name.Name]+") error" {
		return false
	}
	statements := admissionStatements(want)
	if len(fn.Body.List) < len(statements) {
		return false
	}
	for index, statement := range statements {
		if admissionNodeText(fn.Body.List[index]) != statement {
			return false
		}
	}
	// The two lifecycle callers also defer cancellation. Its exact statement and
	// position are pinned above; remove only that statement from this shallow
	// view so the existing lock-identity/one-deferred-close proof is unchanged.
	copyFn, copyBody := *fn, *fn.Body
	copyFn.Body = &copyBody
	if fn.Name.Name != "SetServiceDesiredState" {
		copyBody.List = append(append([]ast.Stmt{}, fn.Body.List[:1]...), fn.Body.List[2:]...)
	}
	return maintenanceServiceContinuousHold(&copyFn)
}

func maintenanceTransitionDirectReceiver(fn *ast.FuncDecl, expression ast.Expr, name string) bool {
	if fn == nil || fn.Recv == nil || len(fn.Recv.List) != 1 || len(fn.Recv.List[0].Names) != 1 {
		return false
	}
	receiver := fn.Recv.List[0].Names[0]
	identifier, ok := expression.(*ast.Ident)
	return ok && receiver.Name == name && receiver.Obj != nil && identifier.Obj == receiver.Obj
}

// Count selections across all production files, including platform files and
// package initializers. A method value is a reference but never an approved call.
func maintenanceTransitionContract(sources map[string]string) []string {
	var failures []string
	counts := map[string]int{}
	ownerCounts := map[string]int{}
	setterOwners := 0
	for filename, source := range sources {
		if !strings.HasSuffix(filename, ".go") || strings.HasSuffix(filename, "_test.go") {
			continue
		}
		file, err := admissionReadOnlyParse(filename, strings.ReplaceAll(source, "\r\n", "\n"))
		if err != nil {
			failures = append(failures, filename+"_parse")
			continue
		}
		parents := map[ast.Node]ast.Node{}
		var stack []ast.Node
		ast.Inspect(file, func(node ast.Node) bool {
			if node == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			if len(stack) > 0 {
				parents[node] = stack[len(stack)-1]
			}
			stack = append(stack, node)
			return true
		})
		for _, declaration := range file.Decls {
			if fn, ok := declaration.(*ast.FuncDecl); ok && fn.Name.Name == "setServiceDesiredStateLocked" {
				setterOwners++
				if invariant := maintenanceTransitionSetterOwner(filename, fn); invariant != "" {
					failures = append(failures, "setServiceDesiredStateLocked_"+invariant)
				}
				if !maintenanceTransitionSetterInherits(fn) {
					failures = append(failures, "setServiceDesiredStateLocked_inherited_hold")
				}
				if len(updateOrdinaryAdmissions(filename, fn)) != 1 {
					failures = append(failures, "setServiceDesiredStateLocked_literal_zero_direct_error_return")
				}
				references := 0
				ast.Inspect(fn, func(node ast.Node) bool {
					if selector, ok := node.(*ast.SelectorExpr); ok && selector.Sel.Name == "maintenanceFenceAllows" {
						references++
					}
					return true
				})
				if references != 1 {
					failures = append(failures, "setServiceDesiredStateLocked_reference_count")
				}
			}
		}
		ast.Inspect(file, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			method := selector.Sel.Name
			if method != "setServiceDesiredStateLocked" && method != "writeServiceDescriptor" && method != "setServiceFaulted" && method != "commitServiceFault" {
				return true
			}
			counts[method]++
			var owner *ast.FuncDecl
			closure := false
			for ancestor := parents[selector]; ancestor != nil; ancestor = parents[ancestor] {
				if _, ok := ancestor.(*ast.FuncLit); ok {
					closure = true
				}
				if fn, ok := ancestor.(*ast.FuncDecl); ok {
					owner = fn
					break
				}
			}
			if owner != nil {
				ownerCounts[method+":"+filename+":"+owner.Name.Name]++
			}
			call, direct := parents[selector].(*ast.CallExpr)
			valid := direct && call.Fun == selector && !call.Ellipsis.IsValid() && owner != nil && !closure
			if valid {
				switch method {
				case "setServiceDesiredStateLocked":
					expectedFile := "lifecycle.go"
					if owner.Name.Name == "SetServiceDesiredState" {
						expectedFile = "service_descriptor.go"
					}
					valid = filename == expectedFile && len(call.Args) == 2 && maintenanceTransitionDirectReceiver(owner, selector.X, "c") && maintenanceTransitionCallerHold(owner)
					if valid && owner.Name.Name == "SetServiceDesiredState" {
						argument, ok := call.Args[1].(*ast.Ident)
						valid = ok && leaseWaitOwnContext(owner, call.Args[0]) && len(owner.Type.Params.List) == 2 && len(owner.Type.Params.List[1].Names) == 1 &&
							argument.Obj != nil && argument.Obj == owner.Type.Params.List[1].Names[0].Obj && argument.Name == "state"
					} else if valid {
						argument, ok := call.Args[1].(*ast.BasicLit)
						valid = ok && admissionNodeText(call.Args[0]) == "context.Background()" && argument.Kind == token.STRING && argument.Value == `"stopped"`
					}
				case "writeServiceDescriptor":
					valid = filename == "service_descriptor.go" && len(call.Args) == 1 && maintenanceTransitionDirectReceiver(owner, selector.X, "c") &&
						(owner.Name.Name == "PrepareService" || owner.Name.Name == "setServiceDesiredStateLocked" || owner.Name.Name == "commitServiceFault" || owner.Name.Name == "RepairService")
				case "setServiceFaulted", "commitServiceFault":
					if method == "commitServiceFault" && filename == "service_descriptor.go" {
						// The generic writer passes its original argument through without a receipt.
						valid = owner.Name.Name == "setServiceFaulted" && maintenanceTransitionDirectReceiver(owner, selector.X, "c") &&
							len(owner.Body.List) == 2 && admissionNodeText(owner.Body.List[0]) == admissionStatements("if err := ctx.Err(); err != nil { return err }")[0] &&
							admissionNodeText(owner.Body.List[1]) == "return c.commitServiceFault(ctx, faulted, nil)" &&
							admissionNodeText(owner.Type) == "func(ctx context.Context, faulted bool) error"
						break
					}
					cell, ok := selector.X.(*ast.SelectorExpr)
					valid = ok && cell.Sel.Name == "cell" && maintenanceTransitionDirectReceiver(owner, cell.X, "s") &&
						((method == "setServiceFaulted" && filename == "service_supervisor.go" && owner.Name.Name == "fault" && len(call.Args) == 2) ||
							(method == "commitServiceFault" && filename == "service_shutdown_supervisor.go" && owner.Name.Name == "commitShutdownFault" &&
								len(call.Args) == 3 && admissionNodeText(call.Args[2]) == "&s.shutdownFaultCommit"))
					if valid {
						argument, ok := call.Args[1].(*ast.Ident)
						valid = leaseWaitOwnContext(owner, call.Args[0]) && ok && argument.Name == "true" && argument.Obj == nil
					}
				}
			}
			if !valid {
				failures = append(failures, method+"_caller_shape")
			}
			return true
		})
	}
	if setterOwners != 1 {
		failures = append(failures, "setServiceDesiredStateLocked_owner_count")
	}
	for method, expected := range map[string]int{"setServiceDesiredStateLocked": 3, "writeServiceDescriptor": 4, "setServiceFaulted": 1, "commitServiceFault": 2} {
		if counts[method] != expected {
			failures = append(failures, method+"_caller_count")
		}
	}
	for method, owners := range map[string][]string{
		"setServiceDesiredStateLocked": {"service_descriptor.go:SetServiceDesiredState", "lifecycle.go:stopWithServiceFence", "lifecycle.go:Remove"},
		"writeServiceDescriptor":       {"service_descriptor.go:PrepareService", "service_descriptor.go:setServiceDesiredStateLocked", "service_descriptor.go:commitServiceFault", "service_descriptor.go:RepairService"},
		"setServiceFaulted":            {"service_supervisor.go:fault"},
		"commitServiceFault":           {"service_descriptor.go:setServiceFaulted", "service_shutdown_supervisor.go:commitShutdownFault"},
	} {
		for _, owner := range owners {
			if ownerCounts[method+":"+owner] != 1 {
				failures = append(failures, method+"_caller_inventory")
			}
		}
	}
	sort.Strings(failures)
	return failures
}

func TestServiceMaintenanceTransitionContractFixtures(t *testing.T) {
	sources := admissionReadSources(t)
	for name, source := range sources {
		sources[name] = strings.ReplaceAll(source, "\r\n", "\n")
	}
	if failures := maintenanceServiceAdmissionContract(sources); len(failures) != 0 {
		t.Fatalf("unchanged control: %v", failures)
	}
	gate := admissionIndent(admissionStatements(maintenanceReleasedAdmissionStatement)[0])
	nestedGate := admissionIndent(gate)
	type fixture struct {
		name, file, owner, invariant string
		change                       func(string) string
	}
	replace := func(before, after string) func(string) string {
		return func(source string) string { return strings.Replace(source, before, after, 1) }
	}
	setter := "setServiceDesiredStateLocked"
	fixtures := []fixture{
		{"W3DeleteGate", "service_descriptor.go", setter, setter + "_reference_count", replace(nestedGate, "")},
		{"W3DuplicateGate", "service_descriptor.go", setter, setter + "_reference_count", replace(nestedGate, nestedGate+"\n\t"+gate)},
		{"W3LiteralOne", "service_descriptor.go", setter, setter + "_literal_zero_direct_error_return", replace("ctx, 0)", "ctx, 1)")},
		{"W3HoistTopLevel", "service_descriptor.go", setter, setter + "_position", func(source string) string {
			return serviceAdmissionInsert(strings.Replace(source, nestedGate, "", 1), 3, maintenanceReleasedAdmissionStatement)
		}},
		{"W3BeforeMaintenanceCheck", "service_descriptor.go", setter, setter + "_position", func(source string) string {
			return strings.Replace(strings.Replace(source, nestedGate, "", 1), "if state == \"running\" {", "if state == \"running\" {\n"+gate, 1)
		}},
		{"W3WrongBranch", "service_descriptor.go", setter, setter + "_position", replace(`if state == "running" {`, `if state != "stopped" {`)},
		{"W3Else", "service_descriptor.go", setter, setter + "_position", replace("\n\td.DesiredState, d.Revision =", " else { return nil }\n\td.DesiredState, d.Revision =")},
		{"W3Own", "service_descriptor.go", setter, setter + "_inherited_hold", func(source string) string {
			return serviceAdmissionInsert(source, 0, "lock, err := c.own(context.Background(), false); if err != nil { return err }; defer func() { _ = lock.Close() }()")
		}},
		{"W3LockIdentifier", "service_descriptor.go", setter, setter + "_inherited_hold", func(source string) string { return serviceAdmissionInsert(source, 0, "lock := 0; _ = lock") }},
		{"W3Go", "service_descriptor.go", setter, setter + "_inherited_hold", func(source string) string { return serviceAdmissionInsert(source, 0, "go func() {}()") }},
		{"W3Defer", "service_descriptor.go", setter, setter + "_inherited_hold", func(source string) string { return serviceAdmissionInsert(source, 0, "defer func() {}()") }},
		{"W3Bodyless", "service_descriptor.go", setter, setter + "_position", func(source string) string { return source[:strings.Index(source, " {")] }},
		{"W3WrongReceiver", "service_descriptor.go", setter, setter + "_signature", replace("(c *Cell)", "(other *Cell)")},
		{"W3NamedResult", "service_descriptor.go", setter, setter + "_signature", replace(") error {", ") (result error) {")},
		{"W3WrongParameter", "service_descriptor.go", setter, setter + "_signature", replace("state string)", "state interface{})")},
		{"SetterVariableLifecycleCall", "lifecycle.go", "stopWithServiceFence", setter + "_caller_shape", replace(`setServiceDesiredStateLocked(context.Background(), "stopped")`, `setServiceDesiredStateLocked(ctx, state)`)},
		{"SetterVariableRemoveCall", "lifecycle.go", "Remove", setter + "_caller_shape", replace(`setServiceDesiredStateLocked(context.Background(), "stopped")`, `setServiceDesiredStateLocked(ctx, state)`)},
		{"SetterCallerWrongReceiver", "service_descriptor.go", "SetServiceDesiredState", setter + "_caller_shape", replace("(c *Cell)", "(c *Other)")},
		{"SetterCallerEarlyUnlock", "service_descriptor.go", "SetServiceDesiredState", setter + "_caller_shape", replace("return c.setServiceDesiredStateLocked(ctx, state)", "_ = lock.Close(); return c.setServiceDesiredStateLocked(ctx, state)")},
		{"SetterCallerReceiverAlias", "service_descriptor.go", "SetServiceDesiredState", setter + "_caller_shape", replace("return c.setServiceDesiredStateLocked(ctx, state)", "other := c; return other.setServiceDesiredStateLocked(ctx, state)")},
		{"SetterCallerStateLiteral", "service_descriptor.go", "SetServiceDesiredState", setter + "_caller_shape", replace("setServiceDesiredStateLocked(ctx, state)", `setServiceDesiredStateLocked(context.Background(), "stopped")`)},
		{"W8LiteralOne", "service_descriptor.go", "RepairService", "RepairService_literal_zero_direct_error_return", replace("ctx, 0)", "ctx, 1)")},
		{"W8DeleteGate", "service_descriptor.go", "RepairService", "RepairService_reference_count", replace(gate, "")},
		{"W8BeforeDescriptorRead", "service_descriptor.go", "RepairService", "RepairService_position", func(source string) string {
			return serviceAdmissionInsert(strings.Replace(source, gate, "", 1), 6, maintenanceReleasedAdmissionStatement)
		}},
		{"W8BeforeOwn", "service_descriptor.go", "RepairService", "RepairService_position", func(source string) string {
			return serviceAdmissionInsert(strings.Replace(source, gate, "", 1), 3, maintenanceReleasedAdmissionStatement)
		}},
		{"W8OuterOwn", "service_descriptor.go", "RepairService", "RepairService_continuous_hold", func(source string) string {
			return serviceAdmissionInsert(source, 0, "outer, err := c.own(context.Background(), false); if err != nil { return err }; defer func() { _ = outer.Close() }()")
		}},
		{"W8AssignmentBeforeGate", "service_descriptor.go", "RepairService", "RepairService_position", func(source string) string {
			return admissionEditStatements(source, func(statements []ast.Stmt) []ast.Stmt {
				statements[8], statements[9] = statements[9], statements[8]
				return statements
			})
		}},
		{"WriterAlias", "service_descriptor.go", "commitServiceFault", "writeServiceDescriptor_caller_shape", replace("if err := c.writeServiceDescriptor(d);", "writer := c.writeServiceDescriptor; if err := writer(d);")},
		{"FaultFalse", "service_supervisor.go", "fault", "setServiceFaulted_caller_shape", replace("setServiceFaulted(ctx, true)", "setServiceFaulted(ctx, false)")},
		{"FaultVariable", "service_shutdown_supervisor.go", "commitShutdownFault", "commitServiceFault_caller_shape", replace("commitServiceFault(ctx, true,", "commitServiceFault(ctx, faulted,")},
		{"FaultAlias", "service_supervisor.go", "fault", "setServiceFaulted_caller_shape", replace("err = s.cell.setServiceFaulted(ctx, true)", "fault := s.cell.setServiceFaulted; err = fault(ctx, true)")},
		{"GenericFaultOverride", "service_descriptor.go", "setServiceFaulted", "commitServiceFault_caller_shape", replace("commitServiceFault(ctx, faulted, nil)", "commitServiceFault(ctx, false, nil)")},
		{"RefusalReceiptDropped", "service_shutdown_supervisor.go", "commitShutdownFault", "commitServiceFault_caller_shape", replace("&s.shutdownFaultCommit", "nil")},
	}
	for _, extra := range []struct{ name, file, text, invariant string }{
		{"FourthSetterCaller", "service_descriptor.go", `func (c *Cell) fourthSetter() error { return c.setServiceDesiredStateLocked(context.Background(), "stopped") }`, setter + "_caller_count"},
		{"SetterMethodValue", "service_descriptor.go", `var setterAlias = (*Cell).setServiceDesiredStateLocked`, setter + "_caller_shape"},
		{"SetterDuplicateOwner", "duplicate_windows.go", `func (c *Cell) setServiceDesiredStateLocked(ctx context.Context, state string) error { return nil }`, setter + "_owner_count"},
		{"FifthWriterCaller", "service_descriptor.go", `func (c *Cell) fifthWriter(d ServiceDescriptor) error { return c.writeServiceDescriptor(d) }`, "writeServiceDescriptor_caller_count"},
		{"ThirdFaultCaller", "service_descriptor.go", `func (c *Cell) clearFault(ctx context.Context) error { return c.setServiceFaulted(ctx, false) }`, "setServiceFaulted_caller_count"},
		{"ExtraFaultCommitCaller", "service_descriptor.go", `func (c *Cell) clearFault(ctx context.Context) error { return c.commitServiceFault(ctx, false, nil) }`, "commitServiceFault_caller_count"},
		{"NinthGateDescriptor", "service_descriptor.go", `func (c *Cell) ninthGate() error { ` + maintenanceAdmissionStatement + `; return nil }`, "service_descriptor.go_reference_count"},
		{"NinthGateLifecycle", "lifecycle.go", `func (c *Cell) ninthGate() error { ` + maintenanceAdmissionStatement + `; return nil }`, "lifecycle.go_reference_count"},
		{"NinthGateBoot", "service_boot.go", `func (c *Cell) ninthGate() error { ` + maintenanceAdmissionStatement + `; return nil }`, "service_boot.go_reference_count"},
	} {
		fixtures = append(fixtures, fixture{extra.name, extra.file, "", extra.invariant, func(source string) string {
			if source == "" {
				source = "package localcell\n"
			}
			return source + "\n" + extra.text
		}})
	}
	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			input := serviceAdmissionClone(sources)
			if fixture.owner == "" {
				input[fixture.file] = fixture.change(input[fixture.file])
			} else {
				input[fixture.file] = admissionMutateFunction(t, input[fixture.file], fixture.owner, fixture.change)
			}
			if input[fixture.file] == sources[fixture.file] {
				t.Fatal("fixture did not change its named site")
			}
			for _, ending := range []string{"LF", "CRLF"} {
				t.Run(ending, func(t *testing.T) {
					candidate := serviceAdmissionClone(input)
					if ending == "CRLF" {
						for name, source := range candidate {
							candidate[name] = strings.ReplaceAll(strings.ReplaceAll(source, "\r\n", "\n"), "\n", "\r\n")
						}
					}
					serviceAdmissionRequireInvariant(t, maintenanceServiceAdmissionContract(candidate), fixture.invariant)
					if fixture.name != "W3DeleteGate" && fixture.name != "W8DeleteGate" && fixture.name != "W3Bodyless" && len(updateFoundationViolations(candidate)) == 0 {
						t.Fatal("foundation guard approved a broken transition or closure")
					}
				})
			}
		})
	}
}

func TestServiceMaintenanceTransitionCallerInventoryFixtures(t *testing.T) {
	for _, method := range []string{"setServiceDesiredStateLocked", "writeServiceDescriptor", "setServiceFaulted", "commitServiceFault"} {
		t.Run(method, func(t *testing.T) {
			input := admissionReadSources(t)
			for name, source := range input {
				input[name] = strings.ReplaceAll(source, "\r\n", "\n")
			}
			switch method {
			case "setServiceDesiredStateLocked":
				input["lifecycle.go"] = admissionMutateFunction(t, input["lifecycle.go"], "stopWithServiceFence", func(source string) string {
					return strings.Replace(source, `c.setServiceDesiredStateLocked(context.Background(), "stopped")`, "nil", 1)
				})
				input["lifecycle.go"] = admissionMutateFunction(t, input["lifecycle.go"], "Remove", func(source string) string {
					return serviceAdmissionInsert(source, 8, `_ = c.setServiceDesiredStateLocked(context.Background(), "stopped")`)
				})
			case "writeServiceDescriptor":
				input["service_descriptor.go"] = admissionMutateFunction(t, input["service_descriptor.go"], "commitServiceFault", func(source string) string {
					return strings.Replace(source, "if err := c.writeServiceDescriptor(d); err != nil {", "if err != nil {", 1)
				})
				input["service_descriptor.go"] = admissionMutateFunction(t, input["service_descriptor.go"], "PrepareService", func(source string) string {
					return strings.Replace(source, "return c.writeServiceDescriptor(d)", "_ = c.writeServiceDescriptor(d); return c.writeServiceDescriptor(d)", 1)
				})
			case "setServiceFaulted":
				input["service_supervisor.go"] = admissionMutateFunction(t, input["service_supervisor.go"], "fault", func(source string) string {
					return strings.Replace(source, "s.cell.setServiceFaulted(ctx, true)", "nil", 1)
				})
				input["service_shutdown_supervisor.go"] = admissionMutateFunction(t, input["service_shutdown_supervisor.go"], "persistServiceShutdownFault", func(source string) string {
					return serviceAdmissionInsert(source, 0, "_ = s.cell.setServiceFaulted(ctx, true)")
				})
			case "commitServiceFault":
				input["service_shutdown_supervisor.go"] = admissionMutateFunction(t, input["service_shutdown_supervisor.go"], "commitShutdownFault", func(source string) string {
					return strings.Replace(source, "err := s.cell.commitServiceFault(ctx, true, &s.shutdownFaultCommit)", "var err error", 1)
				})
				input["service_descriptor.go"] = admissionMutateFunction(t, input["service_descriptor.go"], "setServiceFaulted", func(source string) string {
					return serviceAdmissionInsert(source, 0, "_ = c.commitServiceFault(ctx, faulted, nil)")
				})
			}
			for _, ending := range []string{"LF", "CRLF"} {
				t.Run(ending, func(t *testing.T) {
					candidate := serviceAdmissionClone(input)
					if ending == "CRLF" {
						for name, source := range candidate {
							candidate[name] = strings.ReplaceAll(strings.ReplaceAll(source, "\r\n", "\n"), "\n", "\r\n")
						}
					}
					failures := maintenanceTransitionContract(candidate)
					if containsAdmissionFailure(failures, method+"_caller_count") {
						t.Fatal("redistribution changed total reference count")
					}
					serviceAdmissionRequireInvariant(t, failures, method+"_caller_inventory")
					if len(updateFoundationViolations(candidate)) == 0 {
						t.Fatal("foundation guard approved a redistributed caller inventory")
					}
				})
			}
		})
	}
}
