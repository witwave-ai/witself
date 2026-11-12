package localcell

import (
	"fmt"
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"sort"
	"strings"
	"testing"
)

var maintenanceHostAdmissionFiles = map[string]string{
	"ServiceHealth":                 "service_health.go",
	"serviceMaintenanceObservation": "service_maintenance.go",
}

// Independent source pins deliberately distinguish the named health result
// from every ordinary error owner. The observer owns only a negative decision.
var maintenanceHostAdmissionBoundaries = map[string]string{
	"ServiceHealth": `ctx, cancel := c.deadline(ctx)
defer cancel()
site := serviceHealthStep(ctx, "health_lock")
defer func() { result = serviceFailureAt(result, site) }()
lock, err := c.own(false)
if errors.Is(err, ErrLocked) { return err }
if err != nil { return errServiceHealth }
defer func() { _ = lock.Close() }()
site = serviceHealthStep(ctx, "health_tree")
d, err := c.ServiceDescriptor()
if err != nil || d.DesiredState != "running" || d.Faulted { return errServiceHealth }
if err := c.serviceStartFence(); err != nil { return errServiceHealth }
if err := c.serviceLiveTree(); err != nil { return errServiceHealth }
m, err := readManifest(c.cfg.Root)
if err != nil || m.AccountID == "" { return errServiceHealth }
` + maintenanceAdmissionStatement + `
role, password, err := c.healthPrincipal(m)`,
	"serviceMaintenanceObservation": `if err := ctx.Err(); err != nil { return err }
lock, err := c.own(false)
if err != nil { return err }
defer func() { _ = lock.Close() }()
d, err := c.readServiceDescriptor()
if err != nil { return err }
if observed != nil { *observed = d }
` + maintenanceAdmissionStatement + `
return nil`,
}

// Full declarations are intentional for the small new scheduler helpers. A
// late extra branch, receiver alias or result defer cannot hide behind a valid
// leading check. The caller inventory below distinguishes observation from an
// enabling owner and includes production files excluded by the current GOOS.
var maintenanceHostSchedulerPins = map[string]string{
	"observeServiceMaintenance": `func (s *supervisor) observeServiceMaintenance(ctx context.Context) (bool, error) {
return s.observeServiceMaintenanceInto(ctx, nil)
}`,
	"observeServiceMaintenanceInto": `func (s *supervisor) observeServiceMaintenanceInto(ctx context.Context, observed *ServiceDescriptor) (bool, error) {
err := s.cell.serviceMaintenanceObservation(ctx, observed)
if cancellation := serviceControlCancellation(ctx); cancellation != nil { return false, cancellation }
if ctx.Err() != nil { return false, ctx.Err() }
if errors.Is(err, errMaintenanceFenceActive) { s.maintenanceHeld = true; return true, nil }
if err == nil { s.maintenanceHeld = false }
return false, err
}`,
	"holdMaintenance": `func (s *supervisor) holdMaintenance() error {
s.maintenanceHeld, s.maintenanceDisplay = true, true
s.maintenanceBusy = false
if s.pendingStartup != serviceStartupNone && s.startupFaulted {
s.status.State, s.status.Reason = "faulted", "service_circuit_open"
}
s.status.LastStepOutcome = "ok"
return nil
}`,
	"projectedStatus": `func (s *supervisor) projectedStatus() ServiceStatus {
status := s.status
status.Processes = slices.Clone(status.Processes)
if s.maintenanceBusy {
status.State, status.Reason, status.Healthy = "busy", "service_operation_in_progress", false
} else if s.maintenanceDisplay {
status.State, status.Reason, status.Healthy = "maintenance", errMaintenanceFenceActive.Error(), false
status.Restarts = max(0, s.record.Attempts-1)
}
return status
}`,
	"resumeServiceStartup": `func (s *supervisor) resumeServiceStartup(ctx context.Context) error {
if s.pendingStartup == serviceStartupReconcile {
if err := s.call(ctx, "reconcile", func(context.Context) error { return s.hooks.reconcile() }); err != nil {
if ctx.Err() == nil && errors.Is(err, errMaintenanceFenceActive) { return s.holdMaintenance() }
return serviceFailureAt(err, "startup_reconcile")
}
s.pendingStartup = serviceStartupRepair
}
if s.pendingStartup == serviceStartupRepair {
if err := s.call(ctx, "repair", s.hooks.repair); err != nil {
if ctx.Err() == nil && errors.Is(err, errMaintenanceFenceActive) { return s.holdMaintenance() }
return serviceFailureAt(err, "startup_repair")
}
s.pendingStartup = serviceStartupStop
}
if s.pendingStartup == serviceStartupStop {
if err := s.call(ctx, "stop", s.hooks.stop); err != nil { return serviceFailureAt(err, "startup_stop") }
s.pendingStartup = serviceStartupNone
}
return nil
}`,
	"failAfterMaintenanceObservation": `func (s *supervisor) failAfterMaintenanceObservation(ctx context.Context) error {
held, err := s.observeServiceMaintenance(ctx)
if errors.Is(err, ErrLocked) { return s.busy() }
if err != nil { return err }
if held { return s.holdMaintenance() }
return s.failed(ctx)
}`,
	"serviceMaintenanceOnly": `func serviceMaintenanceOnly(err error) bool {
for {
if err == errMaintenanceFenceActive { return true }
failure, ok := err.(*serviceFailure)
if !ok { return false }
err = failure.cause
}
}`,
}

var maintenanceHostSchedulerCallers = map[string]int{
	"service_control.go/serveServiceControlForPlatform/serviceMaintenanceOnly":           1,
	"service_maintenance.go/observeServiceMaintenance/observeServiceMaintenanceInto":     1,
	"service_maintenance.go/observeServiceMaintenanceInto/serviceMaintenanceObservation": 1,
	"service_maintenance.go/resumeServiceStartup/holdMaintenance":                        2,
	"service_maintenance.go/failAfterMaintenanceObservation/observeServiceMaintenance":   1,
	"service_maintenance.go/failAfterMaintenanceObservation/holdMaintenance":             1,
	"service_supervisor.go/Run/serviceMaintenanceOnly":                                   1,
	"service_supervisor.go/Run/resumeServiceStartup":                                     1,
	"service_supervisor.go/tick/observeServiceMaintenance":                               1,
	"service_supervisor.go/tick/resumeServiceStartup":                                    1,
	"service_supervisor.go/step/observeServiceMaintenanceInto":                           1,
	"service_supervisor.go/step/holdMaintenance":                                         5,
	"service_supervisor.go/step/failAfterMaintenanceObservation":                         3,
	"service_supervisor.go/command/projectedStatus":                                      3,
	"service_supervisor.go/command/observeServiceMaintenance":                            1,
}

var maintenanceHostShapePins = map[string]string{
	"ServiceStatus":       "type ServiceStatus struct {\nDesiredState string `json:\"desired_state\"`\nState string `json:\"state\"`\nReason string `json:\"reason,omitempty\"`\nHealthy bool `json:\"healthy\"`\nRestarts int `json:\"restarts\"`\nGeneration string `json:\"generation\"`\nHostExit string `json:\"host_exit,omitempty\"`\nLastStepOutcome string `json:\"last_step_outcome\"`\nProcesses []ServiceProcessIdentity `json:\"processes,omitempty\"`\n}",
	"supervisorRecord":    "type supervisorRecord struct {\nVersion int `json:\"version\"`\nRepairRevision uint64 `json:\"repair_revision\"`\nFailures []time.Time `json:\"failures\"`\nNextStart time.Time `json:\"next_start\"`\nAttempts int `json:\"attempts\"`\n}",
	"serviceStartupPhase": "type serviceStartupPhase uint8",
}

func maintenanceHostSchedulerContract(sources map[string]string) []string {
	var failures []string
	owners, callers := map[string]int{}, map[string]int{}
	protected := map[string]bool{"serviceMaintenanceObservation": true}
	for owner := range maintenanceHostSchedulerPins {
		protected[owner] = true
	}
	for filename, source := range sources {
		if !strings.HasSuffix(filename, ".go") || strings.HasSuffix(filename, "_test.go") {
			continue
		}
		file, err := admissionReadOnlyParse(filename, strings.ReplaceAll(source, "\r\n", "\n"))
		if err != nil {
			failures = append(failures, filename+"_parse")
			continue
		}
		for _, declaration := range file.Decls {
			owner := "initializer"
			var inspected ast.Node = declaration
			if fn, ok := declaration.(*ast.FuncDecl); ok {
				owner = fn.Name.Name
				pin, expected := maintenanceHostSchedulerPins[owner]
				canonical := "service_maintenance.go"
				if supervisorPin, supervisorOwner := maintenanceHostSupervisorPins[owner]; supervisorOwner {
					receiver := "supervisor"
					if owner == "Run" {
						receiver = "Cell"
					}
					if filename == "service_supervisor.go" || updateReceiverName(fn) == receiver {
						pin, expected, canonical = supervisorPin, true, "service_supervisor.go"
					}
				}
				if expected {
					owners[owner]++
					if filename != canonical {
						failures = append(failures, owner+"_file")
					}
					parsed, parseErr := parser.ParseFile(token.NewFileSet(), "pin.go", "package localcell\n"+pin, 0)
					if parseErr != nil || len(parsed.Decls) != 1 {
						failures = append(failures, owner+"_pin_parse")
					} else if admissionNodeText(fn) != admissionNodeText(parsed.Decls[0]) {
						failures = append(failures, owner+"_body")
					}
				}
				if filename == "service_maintenance.go" && !protected[owner] {
					failures = append(failures, owner+"_unexpected_helper")
				}
				inspected = fn.Body
			}
			if group, ok := declaration.(*ast.GenDecl); ok {
				if filename == "service_maintenance.go" {
					switch group.Tok {
					case token.IMPORT:
					case token.TYPE:
						if len(group.Specs) != 1 || group.Specs[0].(*ast.TypeSpec).Name.Name != "serviceStartupPhase" {
							failures = append(failures, "service_maintenance_declaration")
						}
					case token.CONST:
						const phases = "package localcell; const (serviceStartupNone serviceStartupPhase = iota; serviceStartupReconcile; serviceStartupRepair; serviceStartupStop)"
						parsed, parseErr := parser.ParseFile(token.NewFileSet(), "phases.go", phases, 0)
						if parseErr != nil || admissionNodeText(group) != admissionNodeText(parsed.Decls[0]) {
							failures = append(failures, "service_maintenance_phase_shape")
						}
					default:
						failures = append(failures, "service_maintenance_declaration")
					}
				}
				for _, specification := range group.Specs {
					typeSpec, isType := specification.(*ast.TypeSpec)
					if !isType {
						continue
					}
					if pin, expected := maintenanceHostShapePins[typeSpec.Name.Name]; expected {
						owners[typeSpec.Name.Name]++
						parsed, parseErr := parser.ParseFile(token.NewFileSet(), "shape.go", "package localcell\n"+pin, 0)
						if parseErr != nil {
							failures = append(failures, typeSpec.Name.Name+"_pin_parse")
						} else if admissionNodeText(typeSpec) != admissionNodeText(parsed.Decls[0].(*ast.GenDecl).Specs[0]) {
							failures = append(failures, typeSpec.Name.Name+"_shape")
						}
						canonical := "service_supervisor.go"
						if typeSpec.Name.Name == "serviceStartupPhase" {
							canonical = "service_maintenance.go"
						}
						if filename != canonical {
							failures = append(failures, typeSpec.Name.Name+"_file")
						}
					}
				}
			}
			if inspected == nil {
				continue
			}
			ast.Inspect(inspected, func(node ast.Node) bool {
				name := ""
				switch node := node.(type) {
				case *ast.SelectorExpr:
					name = node.Sel.Name
				case *ast.Ident:
					// The only free helper has an unbound package-function use.
					if node.Name == "serviceMaintenanceOnly" && node.Obj == nil {
						name = node.Name
					}
				}
				if protected[name] {
					callers[filename+"/"+owner+"/"+name]++
				}
				return true
			})
		}
	}
	for owner := range maintenanceHostSchedulerPins {
		if owners[owner] != 1 {
			failures = append(failures, owner+"_owner_count")
		}
	}
	for owner := range maintenanceHostSupervisorPins {
		if owners[owner] != 1 {
			failures = append(failures, owner+"_owner_count")
		}
	}
	for owner := range maintenanceHostShapePins {
		if owners[owner] != 1 {
			failures = append(failures, owner+"_owner_count")
		}
	}
	for caller, count := range maintenanceHostSchedulerCallers {
		if callers[caller] != count {
			failures = append(failures, caller+"_caller_count")
		}
	}
	for caller := range callers {
		if _, expected := maintenanceHostSchedulerCallers[caller]; !expected {
			failures = append(failures, caller+"_unexpected_caller")
		}
	}
	sort.Strings(failures)
	return failures
}

func maintenanceHostAdmissionOwner(filename string, fn *ast.FuncDecl) string {
	expectedFile, known := maintenanceHostAdmissionFiles[fn.Name.Name]
	if !known || filename != expectedFile || fn.Type.TypeParams != nil || fn.Recv == nil || len(fn.Recv.List) != 1 {
		return "signature"
	}
	receiver := fn.Recv.List[0]
	pointer, pointerOK := receiver.Type.(*ast.StarExpr)
	if !pointerOK || len(receiver.Names) != 1 || receiver.Names[0].Name != "c" || receiver.Names[0].Obj == nil {
		return "signature"
	}
	typ, typeOK := pointer.X.(*ast.Ident)
	parameters := "func(ctx context.Context)"
	if fn.Name.Name == "serviceMaintenanceObservation" {
		parameters = "func(ctx context.Context, observed *ServiceDescriptor)"
	}
	if !typeOK || typ.Name != "Cell" || admissionNodeText(&ast.FuncType{Params: fn.Type.Params}) != parameters {
		return "signature"
	}
	if fn.Type.Results == nil || len(fn.Type.Results.List) != 1 {
		return "signature"
	}
	result := fn.Type.Results.List[0]
	resultType, resultOK := result.Type.(*ast.Ident)
	if !resultOK || resultType.Name != "error" {
		return "signature"
	}
	if fn.Name.Name == "ServiceHealth" {
		if len(result.Names) != 1 || result.Names[0].Name != "result" || result.Names[0].Obj == nil {
			return "signature"
		}
	} else if len(result.Names) != 0 {
		return "signature"
	}
	want := admissionStatements(maintenanceHostAdmissionBoundaries[fn.Name.Name])
	if fn.Body == nil || len(want) == 0 || len(fn.Body.List) < len(want) {
		return "position"
	}
	if fn.Name.Name == "serviceMaintenanceObservation" && len(fn.Body.List) != len(want) {
		return "position"
	}
	for index, statement := range want {
		if admissionNodeText(fn.Body.List[index]) != statement {
			return "position"
		}
	}
	if fn.Name.Name == "ServiceHealth" {
		if !maintenanceHostHealthResultAndHold(fn) {
			return "continuous_hold_result"
		}
	} else if !maintenanceServiceContinuousHold(fn) {
		return "continuous_hold"
	}
	return ""
}

// Health has five specific defers, including the fixed failure-site wrapper.
// Result and lock bindings may not escape, be rebound or acquire an extra use.
func maintenanceHostHealthResultAndHold(fn *ast.FuncDecl) bool {
	if fn.Body == nil || fn.Recv == nil || len(fn.Recv.List) != 1 || len(fn.Recv.List[0].Names) != 1 || fn.Type.Results == nil || len(fn.Type.Results.List) != 1 || len(fn.Type.Results.List[0].Names) != 1 {
		return false
	}
	receiver := fn.Recv.List[0].Names[0]
	result := fn.Type.Results.List[0].Names[0]
	var lockIdentifier *ast.Ident
	for _, statement := range fn.Body.List {
		assignment, ok := statement.(*ast.AssignStmt)
		if !ok || assignment.Tok != token.DEFINE {
			continue
		}
		for _, left := range assignment.Lhs {
			if identifier, ok := left.(*ast.Ident); ok && identifier.Name == "lock" {
				if lockIdentifier != nil {
					return false
				}
				lockIdentifier = identifier
			}
		}
	}
	if receiver.Obj == nil || result.Obj == nil || lockIdentifier == nil || lockIdentifier.Obj == nil {
		return false
	}
	allowedReceivers := map[*ast.Ident]bool{}
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		if selector, ok := node.(*ast.SelectorExpr); ok {
			if identifier, ok := selector.X.(*ast.Ident); ok && identifier.Obj == receiver.Obj {
				allowedReceivers[identifier] = true
			}
		}
		return true
	})
	owns, locks, closes, results, goroutines := 0, 0, 0, 0, 0
	bindingOK := true
	defers := map[string]int{}
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.Ident:
			if node.Obj == lockIdentifier.Obj {
				locks++
			}
			if node.Name == receiver.Name && (node.Obj != receiver.Obj || !allowedReceivers[node]) {
				bindingOK = false
			}
			if node.Name == result.Name {
				results++
				if node.Obj != result.Obj {
					bindingOK = false
				}
			}
		case *ast.SelectorExpr:
			if node.Sel.Name == "own" || node.Sel.Name == "ownExisting" {
				owns++
			}
			if identifier, ok := node.X.(*ast.Ident); ok && identifier.Obj == lockIdentifier.Obj && node.Sel.Name == "Close" {
				closes++
			}
		case *ast.DeferStmt:
			defers[admissionNodeText(node)]++
		case *ast.GoStmt:
			goroutines++
		}
		return true
	})
	wantDefers := []string{`defer cancel()`, `defer func() { result = serviceFailureAt(result, site) }()`, `defer func() { _ = lock.Close() }()`, `defer st.Close()`, `defer client.CloseIdleConnections()`}
	if len(defers) != len(wantDefers) {
		return false
	}
	for _, statement := range wantDefers {
		if defers[admissionStatements(statement)[0]] != 1 {
			return false
		}
	}
	return owns == 1 && locks == 2 && closes == 1 && results == 2 && goroutines == 0 && bindingOK
}

func maintenanceHostAdmissionContract(sources map[string]string) []string {
	var failures []string
	owners := map[string]int{}
	for filename, source := range sources {
		if !strings.HasSuffix(filename, ".go") || strings.HasSuffix(filename, "_test.go") {
			continue
		}
		source = strings.ReplaceAll(source, "\r\n", "\n")
		for _, canonical := range maintenanceHostAdmissionFiles {
			if filename == canonical {
				for _, line := range strings.Split(source, "\n") {
					line = strings.TrimSpace(line)
					if constraint.IsGoBuild(line) || constraint.IsPlusBuild(line) {
						failures = append(failures, filename+"_signature")
					}
				}
			}
		}
		file, err := admissionReadOnlyParse(filename, source)
		if err != nil {
			failures = append(failures, filename+"_parse")
			continue
		}
		for _, declaration := range file.Decls {
			fn, ok := declaration.(*ast.FuncDecl)
			if !ok {
				continue
			}
			if _, expected := maintenanceHostAdmissionFiles[fn.Name.Name]; !expected {
				continue
			}
			owners[fn.Name.Name]++
			if invariant := maintenanceHostAdmissionOwner(filename, fn); invariant != "" {
				failures = append(failures, fn.Name.Name+"_"+invariant)
			}
			if len(updateOrdinaryAdmissions(filename, fn)) != 1 {
				failures = append(failures, fn.Name.Name+"_literal_zero_direct_error_return")
			}
			references := 0
			ast.Inspect(fn, func(node ast.Node) bool {
				if selector, ok := node.(*ast.SelectorExpr); ok && selector.Sel.Name == "maintenanceFenceAllows" {
					references++
				}
				return true
			})
			if references != 1 {
				failures = append(failures, fn.Name.Name+"_reference_count")
			}
		}
	}
	for owner := range maintenanceHostAdmissionFiles {
		if owners[owner] != 1 {
			failures = append(failures, owner+"_owner_count")
		}
	}
	sort.Strings(failures)
	return failures
}

func TestServiceMaintenanceHostSourceContract(t *testing.T) {
	sources := admissionReadSources(t)
	for _, ending := range []string{"LF", "CRLF"} {
		t.Run(ending, func(t *testing.T) {
			input := serviceAdmissionClone(sources)
			if ending == "CRLF" {
				for filename, source := range input {
					input[filename] = strings.ReplaceAll(strings.ReplaceAll(source, "\r\n", "\n"), "\n", "\r\n")
				}
			}
			if failures := maintenanceServiceAdmissionContract(input); len(failures) != 0 {
				t.Fatalf("Host admission invariant failed %v", failures)
			}
			if failures := updateFoundationViolations(input); len(failures) != 0 {
				t.Fatalf("Host foundation invariant failed %v", failures)
			}
			if failures := maintenanceHostSchedulerContract(input); len(failures) != 0 {
				t.Fatalf("Host scheduler invariant failed %v", failures)
			}
		})
	}
}

func maintenanceHostMutations(owner string) []admissionSourceMutation {
	boundary := admissionStatements(maintenanceHostAdmissionBoundaries[owner])
	gateIndex := len(boundary) - 2
	gate := admissionIndent(admissionStatements(maintenanceAdmissionStatement)[0])
	replaceGate := func(text string) func(string) string {
		return func(source string) string { return strings.Replace(source, gate, text, 1) }
	}
	insert := func(index int, text string) func(string) string {
		return func(source string) string { return serviceAdmissionInsert(source, index, text) }
	}
	mutations := []admissionSourceMutation{
		{"DeleteGate", "reference_count", replaceGate("")},
		{"EarlyReturnAtFunctionStart", "position", insert(0, "if c.cfg.Timeout > 0 { return nil }")},
		{"EarlyReturnImmediatelyBeforeGate", "position", insert(gateIndex, "if c.cfg.Timeout > 0 { return nil }")},
		{"GotoPastGate", "position", replaceGate("if c.cfg.Timeout > 0 { goto admitted }; " + maintenanceAdmissionStatement + "; admitted: {}")},
		{"ReceiverRebinding", "position", insert(0, "c = other")},
		{"DeferredSwallow", "position", insert(0, "defer func() { result = nil }()")},
		{"SwitchReturn", "position", insert(gateIndex, "switch { case c.cfg.Timeout > 0: return nil }")},
		{"SelectReturn", "position", insert(gateIndex, "select { default: return nil }")},
		{"PositiveGeneration", "position", replaceGate(strings.ReplaceAll(maintenanceAdmissionStatement, "Allows(0)", "Allows(7)"))},
		{"HexZeroGeneration", "position", replaceGate(strings.ReplaceAll(maintenanceAdmissionStatement, "Allows(0)", "Allows(0x0)"))},
		{"VariableGeneration", "position", replaceGate(strings.ReplaceAll(maintenanceAdmissionStatement, "Allows(0)", "Allows(generation)"))},
		{"WrappedReturn", "position", replaceGate(strings.ReplaceAll(maintenanceAdmissionStatement, "return err", "return fmt.Errorf(\"extra %w\", err)"))},
		{"IgnoreError", "position", replaceGate("_ = c.maintenanceFenceAllows(0)")},
		{"EarlyUnlockReacquire", "position", insert(gateIndex, "_ = lock.Close(); lock, err = c.own(false); if err != nil { return err }")},
		{"LateDuplicate", "reference_count", insert(len(boundary)-1, maintenanceAdmissionStatement)},
		{"MoveAfterFirstEffect", "position", func(source string) string {
			return admissionEditStatements(source, func(statements []ast.Stmt) []ast.Stmt {
				statements[gateIndex], statements[gateIndex+1] = statements[gateIndex+1], statements[gateIndex]
				return statements
			})
		}},
	}
	if owner == "serviceMaintenanceObservation" {
		mutations = append(mutations, admissionSourceMutation{"NamedResultDeferredSwallow", "signature", func(source string) string {
			return serviceAdmissionInsert(strings.Replace(source, ") error {", ") (result error) {", 1), 0, "defer func() { result = nil }()")
		}})
	} else {
		mutations = append(mutations,
			admissionSourceMutation{"DropResultName", "signature", func(source string) string { return strings.Replace(source, ") (result error) {", ") error {", 1) }},
			admissionSourceMutation{"LateResultSwallow", "continuous_hold_result", insert(len(boundary), "defer func() { result = nil }()")},
			admissionSourceMutation{"LateReceiverRebinding", "continuous_hold_result", insert(len(boundary), "c = other")},
			admissionSourceMutation{"LateLockAlias", "continuous_hold_result", insert(len(boundary), "otherLock := lock; _ = otherLock")},
			admissionSourceMutation{"LateResultAlias", "continuous_hold_result", insert(len(boundary), "otherResult := &result; _ = otherResult")},
			admissionSourceMutation{"LateUnlockReacquire", "continuous_hold_result", insert(len(boundary), "_ = lock.Close(); lock, err = c.own(false); if err != nil { return err }")},
		)
	}
	for index := 0; index < gateIndex; index++ {
		mutations = append(mutations, admissionSourceMutation{fmt.Sprintf("DeletePrelude_%02d", index), "position", func(source string) string {
			return serviceAdmissionEditAt(source, index, func(ast.Stmt) []ast.Stmt { return nil })
		}})
	}
	return mutations
}

func TestServiceMaintenanceHostContractMutations(t *testing.T) {
	sources := admissionReadSources(t)
	if failures := maintenanceServiceAdmissionContract(sources); len(failures) != 0 {
		t.Fatalf("Host mutation control failed %v", failures)
	}
	for _, owner := range []string{"ServiceHealth", "serviceMaintenanceObservation"} {
		t.Run(owner, func(t *testing.T) {
			filename := maintenanceHostAdmissionFiles[owner]
			original := admissionNodeText(serviceAdmissionFunction(t, filename, sources[filename], owner))
			for _, mutation := range maintenanceHostMutations(owner) {
				t.Run(mutation.name, func(t *testing.T) {
					changed := mutation.change(original)
					if changed == original {
						t.Fatal("Host mutation left its named site unchanged")
					}
					for _, ending := range []string{"LF", "CRLF"} {
						t.Run(ending, func(t *testing.T) {
							input := serviceAdmissionClone(sources)
							input[filename] = admissionMutateFunction(t, sources[filename], owner, func(string) string { return changed })
							if ending == "CRLF" {
								input[filename] = strings.ReplaceAll(strings.ReplaceAll(input[filename], "\r\n", "\n"), "\n", "\r\n")
							}
							serviceAdmissionRequireInvariant(t, maintenanceServiceAdmissionContract(input), owner+"_"+mutation.invariant)
						})
					}
				})
			}
			maintenanceHostPredicateFixtures(t, owner, filename, original)
			maintenanceHostInventoryFixtures(t, sources, owner, filename, original)
		})
	}
	maintenanceHostSchedulerMutationFixtures(t, sources)
	maintenanceHostDecisionMutationFixtures(t, sources)
	maintenanceHostRestoreMutationFixtures(t, sources)
	t.Run("UnexpectedInitializer", func(t *testing.T) {
		input := serviceAdmissionClone(sources)
		input["service_maintenance.go"] += "\nvar extra = func() bool { return true }()\n"
		serviceAdmissionRequireInvariant(t, maintenanceHostSchedulerContract(input), "service_maintenance_declaration")
	})
	for _, owner := range []string{"ServiceStatus", "supervisorRecord"} {
		t.Run(owner+"_NoMaintenanceField", func(t *testing.T) {
			input := serviceAdmissionClone(sources)
			filename := "service_supervisor.go"
			original := input[filename]
			input[filename] = strings.Replace(original, "type "+owner+" struct {", "type "+owner+" struct {\nMaintenance bool `json:\"maintenance\"`", 1)
			if input[filename] == original {
				t.Fatal("Shape mutation left the record unchanged")
			}
			serviceAdmissionRequireInvariant(t, maintenanceHostSchedulerContract(input), owner+"_shape")
		})
	}
}

func maintenanceHostRestoreMutationFixtures(t *testing.T, sources map[string]string) {
	t.Helper()
	filename := "service_supervisor.go"
	original := admissionNodeText(serviceAdmissionFunction(t, filename, sources[filename], "step"))
	mutations := []admissionSourceMutation{
		{"SnapshotFailuresAlias", "step_body", func(source string) string {
			return strings.Replace(source, "previousRecord.Failures = slices.Clone(s.record.Failures)", "previousRecord.Failures = s.record.Failures", 1)
		}},
		{"RestoreSaveOmitted", "step_body", func(source string) string {
			return strings.Replace(source, "s.record = previousRecord\n\t\t\tif err := s.save(); err != nil {\n\t\t\t\treturn err\n\t\t\t}", "s.record = previousRecord", 1)
		}},
		{"PublishHoldBeforeRestoreSave", "step_body", func(source string) string {
			return strings.Replace(source, "s.record = previousRecord", "s.record = previousRecord\n_ = s.holdMaintenance()", 1)
		}},
		{"SwallowRestoreSaveFailure", "step_body", func(source string) string {
			return strings.Replace(source, "s.record = previousRecord\n\t\t\tif err := s.save(); err != nil {\n\t\t\t\treturn err\n\t\t\t}", "s.record = previousRecord\n_ = s.save()", 1)
		}},
	}
	for _, field := range []string{"Attempts", "Failures", "NextStart", "Version", "RepairRevision"} {
		mutations = append(mutations, admissionSourceMutation{"OmitRestore" + field, "step_body", func(source string) string {
			return strings.Replace(source, "s.record = previousRecord", "retained := s.record."+field+"\ns.record = previousRecord\ns.record."+field+" = retained", 1)
		}})
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			changed := mutation.change(original)
			if changed == original {
				t.Fatal("Restore source mutation left its named site unchanged")
			}
			for _, ending := range []string{"LF", "CRLF"} {
				t.Run(ending, func(t *testing.T) {
					input := serviceAdmissionClone(sources)
					input[filename] = admissionMutateFunction(t, sources[filename], "step", func(string) string { return changed })
					if ending == "CRLF" {
						input[filename] = strings.ReplaceAll(strings.ReplaceAll(input[filename], "\r\n", "\n"), "\n", "\r\n")
					}
					serviceAdmissionRequireInvariant(t, maintenanceHostSchedulerContract(input), mutation.invariant)
				})
			}
		})
	}
}

// These are parsing source-contract witnesses. They do not claim that a
// bare-return fixture is a compiling product mutant or a runtime kill.
func maintenanceHostDecisionMutationFixtures(t *testing.T, sources map[string]string) {
	t.Helper()
	points := []struct {
		name, owner, file, condition string
		occurrence                   int
	}{
		{"ObserverCancellation", "observeServiceMaintenanceInto", "service_maintenance.go", "cancellation != nil", 0},
		{"ObserverHostCancellation", "observeServiceMaintenanceInto", "service_maintenance.go", "ctx.Err() != nil", 0},
		{"ObserverMaintenance", "observeServiceMaintenanceInto", "service_maintenance.go", "errors.Is(err, errMaintenanceFenceActive)", 0},
		{"HoldRawStartupFault", "holdMaintenance", "service_maintenance.go", "s.pendingStartup != serviceStartupNone && s.startupFaulted", 0},
		{"ProjectionBusy", "projectedStatus", "service_maintenance.go", "s.maintenanceBusy", 0},
		{"ProjectionMaintenance", "projectedStatus", "service_maintenance.go", "s.maintenanceDisplay", 0},
		{"StartupReconcile", "resumeServiceStartup", "service_maintenance.go", "s.pendingStartup == serviceStartupReconcile", 0},
		{"StartupReconcileRefusal", "resumeServiceStartup", "service_maintenance.go", "ctx.Err() == nil && errors.Is(err, errMaintenanceFenceActive)", 0},
		{"StartupRepair", "resumeServiceStartup", "service_maintenance.go", "s.pendingStartup == serviceStartupRepair", 0},
		{"StartupRepairRefusal", "resumeServiceStartup", "service_maintenance.go", "ctx.Err() == nil && errors.Is(err, errMaintenanceFenceActive)", 1},
		{"StartupProtectiveStop", "resumeServiceStartup", "service_maintenance.go", "s.pendingStartup == serviceStartupStop", 0},
		{"CleanupReclassification", "failAfterMaintenanceObservation", "service_maintenance.go", "held", 0},
		{"ShutdownExactCause", "serviceMaintenanceOnly", "service_maintenance.go", "err == errMaintenanceFenceActive", 0},
		{"ShutdownWrapperType", "serviceMaintenanceOnly", "service_maintenance.go", "!ok", 0},
		{"TickPendingPhase", "tick", "service_supervisor.go", "s.pendingStartup != serviceStartupNone", 0},
		{"TickResumeOnlyAbsentOrStop", "tick", "service_supervisor.go", "!held || s.pendingStartup == serviceStartupStop", 0},
		{"StepStopCompletionException", "step", "service_supervisor.go", "s.completingStop", 0},
		{"StepRepairFreeze", "step", "service_supervisor.go", "!frozen && s.record.RepairRevision != d.RepairRevision", 0},
		{"StepClockFreeze", "step", "service_supervisor.go", "!frozen && s.record.NextStart.After(latest)", 0},
		{"StepProtectiveLatch", "step", "service_supervisor.go", "s.blockedServiceShutdown(ctx)", 0},
		{"StepProtectiveCircuit", "step", "service_supervisor.go", "len(s.record.Failures) >= 5 && !d.Faulted", 0},
		{"StepProtectiveDrain", "step", "service_supervisor.go", "d.DesiredState != \"running\" || d.Faulted", 0},
		{"StepHeldQueryReturn", "step", "service_supervisor.go", "held && s.queryStep", 0},
		{"StepHeldQueryLatch", "step", "service_supervisor.go", "s.shutdownBlocked", 0},
		{"StepHeldQueryCircuit", "step", "service_supervisor.go", "len(s.record.Failures) >= 5 || d.Faulted", 0},
		{"StepHeldProjection", "step", "service_supervisor.go", "held", 0},
		{"StepHeldReturn", "step", "service_supervisor.go", "held", 1},
		{"StepPendingBarrier", "step", "service_supervisor.go", "s.pendingStartup != serviceStartupNone", 0},
		{"RunningHealthRefusal", "step", "service_supervisor.go", "errors.Is(err, errMaintenanceFenceActive)", 0},
		{"LateStartRestore", "step", "service_supervisor.go", "errors.Is(err, errMaintenanceFenceActive)", 1},
		{"PostStartHealthRefusal", "step", "service_supervisor.go", "errors.Is(err, errMaintenanceFenceActive)", 2},
		{"CommandHealthProjection", "command", "service_supervisor.go", "action == \"health\" && err == nil && !status.Healthy", 0},
		{"CommandStartBarrier", "command", "service_supervisor.go", "s.pendingStartup != serviceStartupNone", 0},
		{"CommandRepairBarrier", "command", "service_supervisor.go", "s.pendingStartup != serviceStartupNone", 1},
		{"ShutdownRepairFilter", "Run", "service_supervisor.go", "serviceMaintenanceOnly(repairErr)", 0},
	}
	forms := []string{"BypassCheck", "EarlyReturnAtFunctionStart", "EarlyReturnBeforeCheck", "GotoPastCheck", "ReceiverRebinding", "NamedResultDeferredSwallow", "SwitchReturn", "SelectReturn", "DeletePrelude", "MoveAfterFirstEffect", "EarlyUnlockReacquire", "LateDuplicate"}
	for _, point := range points {
		t.Run(point.name, func(t *testing.T) {
			for _, form := range forms {
				t.Run(form, func(t *testing.T) {
					original := admissionNodeText(serviceAdmissionFunction(t, point.file, sources[point.file], point.owner))
					changed := maintenanceHostMutateDecision(t, original, point.condition, point.occurrence, form)
					for _, ending := range []string{"LF", "CRLF"} {
						t.Run(ending, func(t *testing.T) {
							input := serviceAdmissionClone(sources)
							input[point.file] = admissionMutateFunction(t, sources[point.file], point.owner, func(string) string { return changed })
							if ending == "CRLF" {
								input[point.file] = strings.ReplaceAll(strings.ReplaceAll(input[point.file], "\r\n", "\n"), "\n", "\r\n")
							}
							serviceAdmissionRequireInvariant(t, maintenanceHostSchedulerContract(input), point.owner+"_body")
						})
					}
				})
			}
		})
	}
}

func maintenanceHostMutateDecision(t *testing.T, original, condition string, occurrence int, form string) string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "decision.go", "package localcell\n"+original, 0)
	if err != nil {
		t.Fatal("Decision mutation control did not parse")
	}
	fn := file.Decls[0].(*ast.FuncDecl)
	var target *ast.IfStmt
	seen := 0
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		if conditional, ok := node.(*ast.IfStmt); ok && admissionNodeText(conditional.Cond) == condition {
			if seen == occurrence {
				target = conditional
			}
			seen++
		}
		return true
	})
	if target == nil {
		t.Fatalf("Decision mutation target missing %s", condition)
	}
	statements := func(source string) []ast.Stmt {
		parsed, parseErr := parser.ParseFile(token.NewFileSet(), "statements.go", "package localcell; func fixture() {"+source+"}", 0)
		if parseErr != nil {
			t.Fatal("Decision fixture statements did not parse")
		}
		return parsed.Decls[0].(*ast.FuncDecl).Body.List
	}
	wrap := func(before, after []ast.Stmt) {
		replacement := &ast.BlockStmt{List: append(append(before, target), after...)}
		replaced := false
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			if replaced {
				return false
			}
			switch node := node.(type) {
			case *ast.BlockStmt:
				for index, statement := range node.List {
					if statement == target {
						node.List[index], replaced = replacement, true
						return false
					}
				}
			case *ast.IfStmt:
				if node.Else == target {
					node.Else, replaced = replacement, true
					return false
				}
			case *ast.CaseClause:
				for index, statement := range node.Body {
					if statement == target {
						node.Body[index], replaced = replacement, true
						return false
					}
				}
			case *ast.CommClause:
				for index, statement := range node.Body {
					if statement == target {
						node.Body[index], replaced = replacement, true
						return false
					}
				}
			}
			return true
		})
		if !replaced {
			t.Fatal("Decision mutation could not replace its statement")
		}
	}
	switch form {
	case "BypassCheck":
		target.Cond = ast.NewIdent("false")
	case "EarlyReturnAtFunctionStart":
		fn.Body.List = append(statements("return"), fn.Body.List...)
	case "EarlyReturnBeforeCheck":
		wrap(statements("return"), nil)
	case "GotoPastCheck":
		jump := statements("goto skipped; skipped: {}")
		wrap(jump[:1], jump[1:])
	case "ReceiverRebinding":
		binding := "s = other"
		switch fn.Name.Name {
		case "Run":
			binding = "c = other"
		case "serviceMaintenanceOnly":
			binding = "err = nil"
		}
		wrap(statements(binding), nil)
	case "NamedResultDeferredSwallow":
		wrap(statements("defer func() { result = nil }()"), nil)
	case "SwitchReturn":
		wrap(statements("switch { default: return }"), nil)
	case "SelectReturn":
		wrap(statements("select { default: return }"), nil)
	case "DeletePrelude":
		if len(target.Body.List) == 0 {
			t.Fatal("Decision mutation has no body prelude")
		}
		target.Body.List = target.Body.List[1:]
	case "MoveAfterFirstEffect":
		if len(target.Body.List) == 0 {
			t.Fatal("Decision mutation has no first body statement")
		}
		first := target.Body.List[0]
		target.Body.List = target.Body.List[1:]
		wrap([]ast.Stmt{first}, nil)
	case "EarlyUnlockReacquire":
		wrap(statements("_ = lock.Close(); lock, err = c.own(false)"), nil)
	case "LateDuplicate":
		wrap(nil, []ast.Stmt{target})
	default:
		t.Fatal("Decision mutation form is unknown")
	}
	changed := admissionNodeText(fn)
	if changed == original {
		t.Fatal("Decision mutation left the source unchanged")
	}
	return changed
}

func maintenanceHostSchedulerMutationFixtures(t *testing.T, sources map[string]string) {
	t.Helper()
	filename := "service_maintenance.go"
	for _, owner := range []string{"observeServiceMaintenance", "observeServiceMaintenanceInto", "holdMaintenance", "projectedStatus", "resumeServiceStartup", "failAfterMaintenanceObservation", "serviceMaintenanceOnly"} {
		t.Run(owner, func(t *testing.T) {
			original := admissionNodeText(serviceAdmissionFunction(t, filename, sources[filename], owner))
			mutations := []admissionSourceMutation{
				{"EarlyReturnAtFunctionStart", "body", func(source string) string { return serviceAdmissionInsert(source, 0, "return") }},
				{"ReceiverOrInputRebinding", "body", func(source string) string {
					statement := "s = other"
					if owner == "serviceMaintenanceOnly" {
						statement = "err = nil"
					}
					return serviceAdmissionInsert(source, 0, statement)
				}},
				{"DeferredSwallow", "body", func(source string) string {
					return serviceAdmissionInsert(source, 0, "defer func() { result = nil }()")
				}},
				{"SwitchReturn", "body", func(source string) string { return serviceAdmissionInsert(source, 0, "switch { default: return }") }},
				{"SelectReturn", "body", func(source string) string { return serviceAdmissionInsert(source, 0, "select { default: return }") }},
				{"GotoPastDecision", "body", func(source string) string {
					return admissionEditStatements(source, func(statements []ast.Stmt) []ast.Stmt {
						jump, _ := parser.ParseFile(token.NewFileSet(), "jump.go", "package localcell; func jump() { goto skipped; skipped: {} }", 0)
						parts := jump.Decls[0].(*ast.FuncDecl).Body.List
						result := []ast.Stmt{parts[0]}
						result = append(result, statements...)
						return append(result, parts[1])
					})
				}},
			}
			fn := serviceAdmissionFunction(t, filename, "package localcell\n"+original, owner)
			for index := range fn.Body.List {
				mutations = append(mutations,
					admissionSourceMutation{fmt.Sprintf("DeleteStatement_%02d", index), "body", func(source string) string {
						return serviceAdmissionEditAt(source, index, func(ast.Stmt) []ast.Stmt { return nil })
					}},
					admissionSourceMutation{fmt.Sprintf("EarlyReturnBeforeStatement_%02d", index), "body", func(source string) string {
						return serviceAdmissionInsert(source, index, "return")
					}},
				)
			}
			for _, mutation := range mutations {
				t.Run(mutation.name, func(t *testing.T) {
					changed := mutation.change(original)
					if changed == original {
						t.Fatal("Scheduler mutation left its named site unchanged")
					}
					for _, ending := range []string{"LF", "CRLF"} {
						t.Run(ending, func(t *testing.T) {
							input := serviceAdmissionClone(sources)
							input[filename] = admissionMutateFunction(t, sources[filename], owner, func(string) string { return changed })
							if ending == "CRLF" {
								input[filename] = strings.ReplaceAll(strings.ReplaceAll(input[filename], "\r\n", "\n"), "\n", "\r\n")
							}
							serviceAdmissionRequireInvariant(t, maintenanceHostSchedulerContract(input), owner+"_"+mutation.invariant)
						})
					}
				})
			}
			for _, fixture := range []struct{ name, target, prefix, invariant string }{
				{"BuildConstrainedDuplicate", "service_duplicate_windows.go", "", "_owner_count"},
				{"BuildTagDuplicate", "service_duplicate.go", "//go:build maintenanceproof\n\n", "_owner_count"},
				{"SoleOwnerWrongFile", "other.go", "", "_file"},
			} {
				t.Run(fixture.name, func(t *testing.T) {
					input := serviceAdmissionClone(sources)
					input[fixture.target] = fixture.prefix + "package localcell\n" + original
					if fixture.name == "SoleOwnerWrongFile" {
						input[filename] = admissionMutateFunction(t, sources[filename], owner, func(string) string { return "" })
					}
					serviceAdmissionRequireInvariant(t, maintenanceHostSchedulerContract(input), owner+fixture.invariant)
				})
			}
			t.Run("AdapterCaller", func(t *testing.T) {
				input := serviceAdmissionClone(sources)
				input["service_adapter_windows.go"] = "package localcell\nfunc adapter(s *supervisor) { _ = s." + owner + " }"
				serviceAdmissionRequireInvariant(t, maintenanceHostSchedulerContract(input), "service_adapter_windows.go/adapter/"+owner+"_unexpected_caller")
			})
		})
	}
}

func maintenanceHostPredicateFixtures(t *testing.T, owner, filename, original string) {
	t.Helper()
	type predicateFixture struct{ name, file, source, invariant string }
	fixtures := []predicateFixture{
		{"PredicateAtAnyIndex", filename, serviceAdmissionInsert(original, 0, "return nil"), "position"},
		{"DropFilenameCheck", "other.go", original, "signature"},
		{"DropReceiverCheck", filename, strings.Replace(original, "*Cell", "*Other", 1), "signature"},
		{"DropBindingCheck", filename, strings.Replace(original, "(c *Cell)", "(other *Cell)", 1), "signature"},
		{"DropParameterCheck", filename, strings.Replace(original, "ctx context.Context", "ctx context.Context, extra int", 1), "signature"},
		{"ValueReceiver", filename, strings.Replace(original, "*Cell", "Cell", 1), "signature"},
	}
	if owner == "serviceMaintenanceObservation" {
		fixtures = append(fixtures,
			predicateFixture{"BaseObservationParameters", filename, strings.Replace(original, ", observed *ServiceDescriptor", "", 1), "signature"},
			predicateFixture{"ValueObservationParameter", filename, strings.Replace(original, "observed *ServiceDescriptor", "observed ServiceDescriptor", 1), "signature"},
		)
	} else {
		fixtures = append(fixtures, predicateFixture{"HealthObservationParameters", filename, strings.Replace(original, "ctx context.Context", "ctx context.Context, observed *ServiceDescriptor", 1), "signature"})
	}
	for _, fixture := range fixtures {
		t.Run("Guard_"+fixture.name, func(t *testing.T) {
			for _, ending := range []string{"LF", "CRLF"} {
				t.Run(ending, func(t *testing.T) {
					source := "package localcell\n" + fixture.source
					if ending == "CRLF" {
						source = strings.ReplaceAll(source, "\n", "\r\n")
					}
					fn := serviceAdmissionFunction(t, fixture.file, source, owner)
					if got := maintenanceHostAdmissionOwner(fixture.file, fn); got != fixture.invariant {
						t.Fatalf("Host predicate invariant changed %s", got)
					}
					if len(updateOrdinaryAdmissions(fixture.file, fn)) != 0 {
						t.Fatal("Host predicate accepted a rejected owner")
					}
				})
			}
		})
	}
}

func maintenanceHostInventoryFixtures(t *testing.T, sources map[string]string, owner, filename, original string) {
	t.Helper()
	for _, fixture := range []struct {
		name, target, prefix, invariant string
		move                            bool
	}{
		{"BuildConstrainedDuplicateOwner", "service_duplicate_windows.go", "", owner + "_owner_count", false},
		{"BuildTagDuplicateOwner", "service_duplicate.go", "//go:build serviceproof\n\n", owner + "_owner_count", false},
		{"SoleOwnerWrongFile", "other.go", "", owner + "_signature", true},
		{"CanonicalBuildTag", filename, "//go:build serviceproof\n\n", filename + "_signature", false},
		{"CanonicalLegacyBuildTag", filename, "// +build serviceproof\n\n", filename + "_signature", false},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			for _, ending := range []string{"LF", "CRLF"} {
				t.Run(ending, func(t *testing.T) {
					input := serviceAdmissionClone(sources)
					input[fixture.target] = fixture.prefix + "package localcell\n" + original
					if fixture.move {
						input[filename] = "package localcell\n"
					}
					if ending == "CRLF" {
						for name, source := range input {
							input[name] = strings.ReplaceAll(strings.ReplaceAll(source, "\r\n", "\n"), "\n", "\r\n")
						}
					}
					serviceAdmissionRequireInvariant(t, maintenanceHostAdmissionContract(input), fixture.invariant)
				})
			}
		})
	}
	for _, witness := range []struct{ name, invariant string }{
		{"OwnerReferenceCount", owner + "_reference_count"},
		{"FileReferenceCount", filename + "_reference_count"},
		{"GlobalReferenceCount", "global_count"},
	} {
		t.Run(witness.name, func(t *testing.T) {
			for _, ending := range []string{"LF", "CRLF"} {
				t.Run(ending, func(t *testing.T) {
					input := serviceAdmissionClone(sources)
					input[filename] = admissionMutateFunction(t, sources[filename], owner, func(source string) string {
						return serviceAdmissionInsert(source, len(admissionStatements(maintenanceHostAdmissionBoundaries[owner]))-1, maintenanceAdmissionStatement)
					})
					if ending == "CRLF" {
						input[filename] = strings.ReplaceAll(strings.ReplaceAll(input[filename], "\r\n", "\n"), "\n", "\r\n")
					}
					serviceAdmissionRequireInvariant(t, maintenanceServiceAdmissionContract(input), witness.invariant)
				})
			}
		})
	}
}

// Independent fixed declarations pin the phase, protective and rollback ordering.
var maintenanceHostSupervisorPins = map[string]string{
	"Run": `func (c *Cell) Run(ctx context.Context, policy SupervisorPolicy) (result error) {
	if os.Getenv("WITSELF_RUNTIME") != "local" {
		return errors.New("localcell: service_requires_local_runtime")
	}
	if err := serviceControlCapability(); err != nil {
		return err
	}
	d, err := c.ServiceDescriptor()
	if err != nil {
		return serviceFailureAt(err, "initial_descriptor")
	}
	if err := validateServiceHost(d); err != nil {
		return err
	}
	s := newSupervisor(c, policy)
	acquireLease := c.acquireServiceLease
	if policy.hooks != nil {
		acquireLease = func(d ServiceDescriptor) (*operationLock, error) {
			return c.acquireServiceLeaseWithBoot(d, s.hooks.boot)
		}
	}
	lease, err := acquireLease(d)
	if err != nil {
		return err
	}
	defer func() { _ = lease.Close() }()
	commands := make(chan serviceCommand)
	closeControl, err := c.serveServiceControl(commands)
	if err != nil {
		return serviceFailureFallback(err, "control_transport")
	}
	defer closeControl()
	if err := s.load(d); err != nil {
		return err
	}
	shutdown := policy.ShutdownTimeout
	maximumShutdown := 30 * time.Second
	if runtime.GOOS == "windows" {
		maximumShutdown = ServiceShutdownTimeout
	}
	if shutdown <= 0 || shutdown > maximumShutdown {
		shutdown = maximumShutdown
	}
	defer func() {
		if s.exitDrained || s.shutdownBlocked {
			return
		}
		if ctx.Err() != nil {
			progress, journalErr := c.readJournal()
			// Only fixed supervisor labels and journal shape cross stderr.
			if s.operation == "health" {
				fmt.Fprintf(os.Stderr, "service_shutdown phase=%s operation=health site=%s journal_pending=%t journal_read_ok=%t\n",
					s.phase, serviceHealthSite(s.healthProgress.site), progress.pending != nil || progress.tornTail, journalErr == nil)
			} else {
				fmt.Fprintf(os.Stderr, "service_shutdown phase=%s operation=%s journal_pending=%t journal_read_ok=%t\n",
					s.phase, s.operation, progress.pending != nil || progress.tornTail, journalErr == nil)
			}
			if errors.Is(result, ctx.Err()) {
				result = nil
			}
		}
		stopCtx, cancel := s.hooks.shutdownContext(context.Background(), shutdown)
		defer cancel()
		// A canceled lifecycle operation may have left a pending journal entry.
		// Settle it before Stop attempts to begin another operation, retaining
		// the lifetime lease and the same bounded, signal-independent context.
		repairErr := retryServiceContention(stopCtx, s.hooks.repair)
		stopErr := retryServiceContention(stopCtx, s.hooks.stop)
		if serviceMaintenanceOnly(repairErr) {
			repairErr = nil
		}
		result = errors.Join(result, serviceFailureAt(repairErr, "shutdown_repair"), serviceFailureAt(stopErr, "shutdown_stop"))
	}()
	s.phase = "startup"
	s.pendingStartup = serviceStartupReconcile
	if err := retryServiceContention(ctx, s.resumeServiceStartup); err != nil {
		return err
	}
	var ticker *time.Ticker
	ticks := s.hooks.ticks
	if ticks == nil {
		ticker = time.NewTicker(time.Second)
		defer ticker.Stop()
		ticks = ticker.C
	}
	if err := s.tick(ctx); err != nil {
		return err
	}
	for {
		runDone := ctx.Done()
		select {
		case <-runDone:
			return nil
		case <-ticks:
			if err := s.tick(ctx); err != nil {
				return err
			}
		case command := <-commands:
			if ctx.Err() != nil {
				return nil
			}
			s.phase = "control"
			commandCtx, cancel := context.WithCancel(command.ctx)
			commandCtx = context.WithValue(commandCtx, serviceControlHostContextKey{}, ctx)
			if command.exit != nil {
				commandCtx = context.WithValue(commandCtx, serviceHostExitKey{}, *command.exit)
			}
			go func() {
				select {
				case <-runDone:
					cancel()
				case <-commandCtx.Done():
				}
			}()
			status, err := s.command(commandCtx, command.action)
			cancel()
			WriteServiceHostFailure(os.Stderr, err, "supervisor_command")
			command.reply <- serviceReply{status: status, err: err}
			if command.action == "exit-host" && err == nil {
				exited := s.finishHostExitCommand(ctx, command)
				if command.completed != nil {
					disposition := "refused"
					if exited {
						disposition = "drained"
						if s.exitDrainResult != nil {
							disposition = "forced"
						}
					}
					command.completed <- disposition
					select {
					case <-command.finalized:
					case <-command.ctx.Done():
					}
				}
				cancel()
				if exited {
					return s.exitDrainResult
				}
			}
			if ctx.Err() == nil {
				s.phase, s.operation = "idle", "none"
			}
		}
	}
}`,
	"load": `func (s *supervisor) load(d ServiceDescriptor) (result error) {
	defer func() { result = serviceFailureFallback(result, "supervisor_load") }()
	if err := servicePrivateRecord(s.cell.path("service", "supervisor.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := readJSON(s.cell.path("service", "supervisor.json"), &s.record); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if s.record.Version != 0 && s.record.Version != 1 {
		return errors.New("localcell: invalid_supervisor_record")
	}
	if s.record.Attempts < 0 || len(s.record.Failures) > 5 {
		return errors.New("localcell: invalid_supervisor_record")
	}
	if s.record.RepairRevision != d.RepairRevision {
		s.record = supervisorRecord{RepairRevision: d.RepairRevision}
	}
	s.record.Version = 1
	s.status = ServiceStatus{DesiredState: d.DesiredState, State: d.DesiredState, Generation: d.Payload.Generation, LastStepOutcome: "not_run"}
	s.startupFaulted = d.Faulted || len(s.record.Failures) >= 5
	s.rememberProcesses(d)
	return nil
}`,
	"tick": `func (s *supervisor) tick(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.phase = "tick"
	if s.pendingStartup != serviceStartupNone {
		held, err := s.observeServiceMaintenance(ctx)
		if errors.Is(err, ErrLocked) {
			s.maintenanceDisplay = false
			return s.busy()
		}
		if err != nil {
			return err
		}
		if !held || s.pendingStartup == serviceStartupStop {
			if err := s.resumeServiceStartup(ctx); err != nil {
				if errors.Is(err, ErrLocked) {
					s.maintenanceDisplay = false
					return s.busy()
				}
				return err
			}
		}
	}
	err := s.step(ctx)
	if ctx.Err() == nil {
		s.phase, s.operation = "idle", "none"
	}
	return err
}`,
	"step": `func (s *supervisor) step(ctx context.Context) (result error) {
	previousStatus := s.status
	previousHeld, previousDisplay := s.maintenanceHeld, s.maintenanceDisplay
	previousBusy := s.maintenanceBusy
	s.maintenanceDisplay = false
	s.maintenanceBusy = false
	defer func() {
		if cancellation := serviceControlCancellation(ctx); cancellation != nil {
			s.status = previousStatus
			s.maintenanceHeld, s.maintenanceDisplay = previousHeld, previousDisplay
			s.maintenanceBusy = previousBusy
			var located *serviceFailure
			if !errors.As(result, &located) || located.site != "control_cancelled" || !errors.Is(result, ctx.Err()) {
				result = cancellation
			}
			return
		}
		s.status.LastStepOutcome = "ok"
		if result != nil {
			s.status.LastStepOutcome, s.status.Healthy = "failed", false
			s.maintenanceDisplay = false
		}
		if s.maintenanceDisplay {
			s.status.HostExit = previousStatus.HostExit
		}
	}()
	var d ServiceDescriptor
	var err error
	held := false
	if s.completingStop {
		d, err = s.cell.ServiceDescriptor()
		if cancellation := serviceControlCancellation(ctx); cancellation != nil {
			return cancellation
		}
		if err != nil {
			return serviceFailureAt(err, "supervisor_traversal")
		}
	} else {
		held, err = s.observeServiceMaintenanceInto(ctx, &d)
		if cancellation := serviceControlCancellation(ctx); cancellation != nil {
			return cancellation
		}
		if errors.Is(err, ErrLocked) {
			d, err = s.cell.ServiceDescriptor()
			if cancellation := serviceControlCancellation(ctx); cancellation != nil {
				return cancellation
			}
			if err != nil {
				return serviceFailureAt(err, "supervisor_traversal")
			}
			defer s.rememberProcesses(d)
			return s.busy()
		}
		if err != nil {
			return serviceFailureAt(err, "supervisor_traversal")
		}
	}
	defer s.rememberProcesses(d)
	frozen := held || s.pendingStartup != serviceStartupNone
	if !frozen && s.record.RepairRevision != d.RepairRevision {
		s.record = supervisorRecord{Version: 1, RepairRevision: d.RepairRevision}
		if err := s.save(); err != nil {
			return err
		}
	}
	// Wall time may move backward across a host restart. Retain the retry
	// fence, while limiting any single wait to the configured sixty-second cap.
	latest := s.hooks.now().Add(s.hooks.maxBackoff)
	if cancellation := serviceControlCancellation(ctx); cancellation != nil {
		return cancellation
	}
	if !frozen && s.record.NextStart.After(latest) {
		s.record.NextStart = latest
		if err := s.save(); err != nil {
			return err
		}
	}
	s.status = ServiceStatus{DesiredState: d.DesiredState, State: d.DesiredState, Restarts: max(0, s.record.Attempts-1), Generation: d.Payload.Generation}
	if held {
		defer func() {
			if result == nil && s.status.State != "busy" && !s.maintenanceBusy {
				_ = s.holdMaintenance()
			}
		}()
	}
	if held && s.queryStep {
		if s.shutdownBlocked {
			s.status.State, s.status.Reason, s.status.Healthy = "faulted", "service_stop_refused", false
		} else if len(s.record.Failures) >= 5 || d.Faulted {
			s.status.State, s.status.Reason = "faulted", "service_circuit_open"
		}
		return nil
	}
	if s.blockedServiceShutdown(ctx) {
		return nil
	}
	// A crash can occur after the fifth failure is synced but before the
	// descriptor is marked faulted. Preserve that circuit decision on restart.
	if len(s.record.Failures) >= 5 && !d.Faulted {
		if err := s.fault(ctx, "service_circuit_open"); err != nil {
			return err
		}
		d.Faulted = true
	}
	if d.Faulted {
		s.status.State, s.status.Reason = "faulted", "service_circuit_open"
	}
	if d.DesiredState != "running" || d.Faulted {
		if s.running {
			if err := s.call(ctx, "stop", s.hooks.stop); err != nil {
				if errors.Is(err, ErrLocked) {
					return s.busy()
				}
				return err
			}
			s.running = false
		}
		if !held && !s.completingStop && s.pendingStartup != serviceStartupNone {
			return s.busy()
		}
		return nil
	}
	if held {
		return s.holdMaintenance()
	}
	if s.pendingStartup != serviceStartupNone {
		return s.busy()
	}
	if s.running {
		if err := s.call(ctx, "health", s.hooks.health); err == nil {
			s.status.Healthy = true
			return nil
		} else if errors.Is(err, ErrLocked) {
			return s.busy()
		} else if serviceControlCancellation(ctx) != nil {
			return err
		} else if errors.Is(err, errMaintenanceFenceActive) {
			return s.holdMaintenance()
		}
		if err := s.call(ctx, "stop", s.hooks.stop); err != nil {
			if serviceControlCancellation(ctx) != nil {
				return err
			}
			if errors.Is(err, ErrLocked) {
				return s.busy()
			}
			return s.fault(ctx, "service_stop_refused")
		}
		s.running = false
		return s.failAfterMaintenanceObservation(ctx)
	}
	now := s.hooks.now()
	if cancellation := serviceControlCancellation(ctx); cancellation != nil {
		return cancellation
	}
	if now.Before(s.record.NextStart) {
		s.status.State = "backoff"
		return nil
	}
	previousRecord := s.record
	previousRecord.Failures = slices.Clone(s.record.Failures)
	s.record.Attempts++
	if err := s.save(); err != nil {
		return err
	}
	if err := s.call(ctx, "start", s.hooks.start); err != nil {
		if serviceControlCancellation(ctx) != nil {
			return err
		}
		if errors.Is(err, ErrLocked) {
			s.record.Attempts--
			if err := s.save(); err != nil {
				return err
			}
			return s.busy()
		}
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, errMaintenanceFenceActive) {
			// Completed-call compensation only. A crash before this save can
			// retain the provisional attempt, but never adds a failure.
			s.record = previousRecord
			if err := s.save(); err != nil {
				return err
			}
			return s.holdMaintenance()
		}
		if err := s.call(ctx, "stop", s.hooks.stop); err != nil {
			if errors.Is(err, ErrLocked) {
				s.running = true
				return s.busy()
			}
			return s.fault(ctx, "service_stop_refused")
		}
		return s.failAfterMaintenanceObservation(ctx)
	}
	if err := s.call(ctx, "health", s.hooks.health); err != nil {
		if serviceControlCancellation(ctx) != nil {
			return err
		}
		if errors.Is(err, errMaintenanceFenceActive) {
			s.running = true
			return s.holdMaintenance()
		}
		if errors.Is(err, ErrLocked) {
			s.running = true
			return s.busy()
		}
		if err := s.call(ctx, "stop", s.hooks.stop); err != nil {
			if errors.Is(err, ErrLocked) {
				s.running = true
				return s.busy()
			}
			return s.fault(ctx, "service_stop_refused")
		}
		return s.failAfterMaintenanceObservation(ctx)
	}
	s.running, s.status.Healthy = true, true
	s.status.State, s.status.Restarts = "running", max(0, s.record.Attempts-1)
	return nil
}
`,
	"busy": `func (s *supervisor) busy() error {
	if s.status.State == "faulted" && (s.maintenanceHeld || s.pendingStartup != serviceStartupNone) {
		s.maintenanceBusy = true
		return nil
	}
	s.status.State, s.status.Reason, s.status.Healthy = "busy", "service_operation_in_progress", false
	return nil
}`,
	"command": `func (s *supervisor) command(ctx context.Context, action string) (ServiceStatus, error) {
	// Only the host's health bound can produce an unhealthy verdict. A caller
	// asking to wait less cancels the command instead, preserving the previous
	// completed status and leaving subsequent decisions to the periodic tick.
	if bound := serviceControlHealthBound(ctx, s.cell.cfg.Timeout); bound < s.cell.cfg.Timeout {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, bound)
		defer cancel()
	}
	ctx = context.WithValue(ctx, serviceControlCommandKey{}, true)
	if err := serviceControlCancellation(ctx); err != nil {
		return s.projectedStatus(), err
	}
	if s.shutdownBlocked && (action == "stop" || action == "start" || action == "exit-host") {
		return s.status, hostExitRefused("drain")
	}
	var err error
	switch action {
	case "exit-host":
		return s.acceptHostExit(ctx)
	case "observe":
		// Return the last completed supervisor snapshot, including its health
		// result. Periodic supervision alone refreshes this observational view.
		return s.projectedStatus(), nil
	case "status", "health":
		s.queryStep = true
		err = s.step(ctx)
		s.queryStep = false
	case "stop":
		err = s.cell.SetServiceDesiredState(ctx, "stopped")
		if err == nil {
			err = s.call(ctx, "stop", s.hooks.stop)
			if err == nil {
				s.running = false
			}
		}
		if err == nil {
			s.completingStop = true
			err = s.step(ctx)
			s.completingStop = false
		}
	case "start":
		d, readErr := s.cell.ServiceDescriptor()
		if readErr != nil {
			err = readErr
		} else if d.Faulted || d.DesiredState == "maintenance" {
			err = ErrServiceControlRefused
		} else {
			err = s.cell.SetServiceDesiredState(ctx, "running")
			if err == nil {
				if s.pendingStartup != serviceStartupNone {
					err = errServiceControlDeferred
				} else {
					err = s.step(ctx)
				}
			}
		}
	case "repair":
		if s.pendingStartup != serviceStartupNone {
			err = s.cell.SetServiceDesiredState(ctx, "stopped")
			if err == nil {
				var held bool
				held, err = s.observeServiceMaintenance(ctx)
				if err == nil {
					err = errServiceControlDeferred
					if held {
						err = errMaintenanceFenceActive
					}
				}
			}
			break
		}
		err = s.cell.RepairService(ctx)
		if err == nil {
			s.shutdownBlocked, s.shutdownFaultPending = false, false
			s.running = false
			err = s.step(ctx)
		}
	default:
		err = ErrServiceControlRefused
	}
	status := s.projectedStatus()
	if action == "health" && err == nil && !status.Healthy {
		if status.State == "busy" {
			// Contention prevented a decision; it is not a child failure.
			err = errServiceControlDeferred
		} else {
			err = errServiceUnhealthy
		}
	}
	return status, serviceFailureFallback(err, "supervisor_command")
}`,
}

// These read-cost contracts cover every platform without preparing a cell.
func TestServiceMaintenanceDescriptorObservationContract(t *testing.T) {
	sources := admissionReadSources(t)
	fn := serviceAdmissionFunction(t, "service_maintenance.go", strings.ReplaceAll(sources["service_maintenance.go"], "\r\n", "\n"), "serviceMaintenanceObservation")
	if invariant := maintenanceHostAdmissionOwner("service_maintenance.go", fn); invariant != "" {
		t.Fatalf("Descriptor observation invariant failed %s", invariant)
	}
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		if selector, ok := node.(*ast.SelectorExpr); ok && selector.Sel.Name == "ServiceDescriptor" {
			t.Error("Descriptor observation repeats root validation")
		}
		return true
	})
}

func TestServiceMaintenanceDescriptorReaderContract(t *testing.T) {
	sources := admissionReadSources(t)
	source := strings.ReplaceAll(sources["service_descriptor.go"], "\r\n", "\n")
	fn := serviceAdmissionFunction(t, "service_descriptor.go", source, "ServiceDescriptor")
	const wrapper = `package localcell
func (c *Cell) ServiceDescriptor() (ServiceDescriptor, error) {
var d ServiceDescriptor
if err := c.validateRoot(false); err != nil { return d, err }
return c.readServiceDescriptor()
}`
	want := serviceAdmissionFunction(t, "wrapper.go", wrapper, "ServiceDescriptor")
	if admissionNodeText(fn) != admissionNodeText(want) {
		t.Fatal("Descriptor public reader validation changed")
	}
	reader := serviceAdmissionFunction(t, "service_descriptor.go", source, "readServiceDescriptor")
	ast.Inspect(reader.Body, func(node ast.Node) bool {
		name := ""
		switch node := node.(type) {
		case *ast.SelectorExpr:
			name = node.Sel.Name
		case *ast.Ident:
			name = node.Name
		}
		switch name {
		case "validateRoot":
			t.Error("Descriptor private reader repeats root validation")
		case "own":
			t.Error("Descriptor private reader acquires the operation lock")
		case "ownExisting":
			t.Error("Descriptor private reader reacquires an existing operation lock")
		case "acquireLock":
			t.Error("Descriptor private reader acquires a raw lock")
		}
		return true
	})
}

func TestServiceMaintenanceDescriptorReaderInventory(t *testing.T) {
	sources := admissionReadSources(t)
	callers := map[string]int{}
	for filename, source := range sources {
		file, err := parser.ParseFile(token.NewFileSet(), filename, strings.ReplaceAll(source, "\r\n", "\n"), 0)
		if err != nil {
			t.Fatal("Descriptor caller inventory source did not parse")
		}
		for _, declaration := range file.Decls {
			owner := "initializer"
			if fn, ok := declaration.(*ast.FuncDecl); ok {
				owner = fn.Name.Name
			}
			ast.Inspect(declaration, func(node ast.Node) bool {
				if selector, ok := node.(*ast.SelectorExpr); ok && selector.Sel.Name == "readServiceDescriptor" {
					callers[filename+"/"+owner]++
				}
				return true
			})
		}
	}
	if len(callers) != 2 || callers["service_descriptor.go/ServiceDescriptor"] != 1 || callers["service_maintenance.go/serviceMaintenanceObservation"] != 1 {
		t.Fatal("Descriptor private reader caller inventory changed")
	}
	fn := serviceAdmissionFunction(t, "service_maintenance.go", strings.ReplaceAll(sources["service_maintenance.go"], "\r\n", "\n"), "serviceMaintenanceObservation")
	want := admissionStatements(`if err := ctx.Err(); err != nil { return err }
lock, err := c.own(false)
if err != nil { return err }
defer func() { _ = lock.Close() }()
d, err := c.readServiceDescriptor()`)
	if len(fn.Body.List) < len(want) {
		t.Fatal("Descriptor observation lock prelude is incomplete")
	}
	for index, statement := range want {
		if admissionNodeText(fn.Body.List[index]) != statement {
			t.Fatal("Descriptor observation reads outside its lock prelude")
		}
	}
	if !maintenanceServiceContinuousHold(fn) {
		t.Fatal("Descriptor observation releases its continuous lock")
	}
}

func TestServiceMaintenanceDescriptorStepContract(t *testing.T) {
	sources := admissionReadSources(t)
	fn := serviceAdmissionFunction(t, "service_supervisor.go", strings.ReplaceAll(sources["service_supervisor.go"], "\r\n", "\n"), "step")
	want := serviceAdmissionFunction(t, "step_pin.go", "package localcell\n"+maintenanceHostSupervisorPins["step"], "step")
	if admissionNodeText(fn) != admissionNodeText(want) {
		t.Fatal("Descriptor step read ordering changed")
	}
	reads, observations := 0, 0
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		if selector, ok := node.(*ast.SelectorExpr); ok {
			switch selector.Sel.Name {
			case "ServiceDescriptor":
				reads++
			case "observeServiceMaintenanceInto":
				observations++
			}
		}
		return true
	})
	if reads != 2 || observations != 1 {
		t.Fatal("Descriptor step read inventory changed")
	}
}
