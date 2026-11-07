package localcell_test

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/localcell"
)

func TestNativeServiceProofRowTimeout(t *testing.T) {
	source := nativeServiceProofContractSource(t, "../../.github/workflows/ci.yml")
	row, err := nativeServiceProofWindowsRowTimeout(source)
	if err != nil {
		t.Fatal(err)
	}
	if row != nativeServiceProofRowTimeout {
		t.Fatalf("native proof row timeout equality: declared=%s Windows CI=%s", nativeServiceProofRowTimeout, row)
	}
}

func TestNativeServiceProofRowTimeoutParser(t *testing.T) {
	const valid = "      - name: Prove native service restart or record adapter capability\n        shell: pwsh\n        run: |\n          go test -json ./internal/localcell -run '^TestNativeServiceLifecycle$' -count=1 -timeout=15m > $proofJSON 2>&1\n"
	for _, test := range []struct {
		name, source string
		want         time.Duration
	}{
		{"exact_step", valid, 15 * time.Minute},
		{"CRLF", strings.ReplaceAll(valid, "\n", "\r\n"), 15 * time.Minute},
		{"changed_CI_timeout", strings.Replace(valid, "-timeout=15m", "-timeout=14m", 1), 14 * time.Minute},
		{"missing_step", "", 0},
		{"different_test", strings.Replace(valid, "^TestNativeServiceLifecycle$", "^TestRealLifecycle$", 1), 0},
		{"non_Windows_step", strings.Replace(valid, "shell: pwsh", "shell: bash", 1), 0},
		{"missing_timeout", strings.Replace(valid, "-timeout=15m", "", 1), 0},
		{"invalid_timeout", strings.Replace(valid, "-timeout=15m", "-timeout=invalid", 1), 0},
		{"duplicate_step", valid + valid, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := nativeServiceProofWindowsRowTimeout([]byte(test.source))
			if test.want == 0 {
				if err == nil {
					t.Fatal("native proof row contract accepted a missing or ambiguous Windows timeout")
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatal("native proof row contract did not parse the exact Windows timeout")
			}
		})
	}

}

func nativeServiceProofWindowsRowTimeout(source []byte) (time.Duration, error) {
	text := "\n" + strings.ReplaceAll(string(source), "\r\n", "\n")
	var timeout time.Duration
	matches := 0
	for _, step := range strings.Split(text, "\n      - name: ") {
		if !strings.HasPrefix(step, "Prove native service restart or record adapter capability\n") || !strings.Contains(step, "\n        shell: pwsh\n") {
			continue
		}
		for _, line := range strings.Split(step, "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 || fields[0] != "go" || fields[1] != "test" {
				continue
			}
			native, timeoutCount := false, 0
			var duration string
			for index, field := range fields {
				if field == "-run" && index+1 < len(fields) && strings.Trim(fields[index+1], "\"'") == "^TestNativeServiceLifecycle$" {
					native = true
				}
				if strings.HasPrefix(field, "-timeout=") {
					timeoutCount++
					duration = strings.TrimPrefix(field, "-timeout=")
				}
			}
			if !native {
				continue
			}
			matches++
			var err error
			timeout, err = time.ParseDuration(duration)
			if err != nil || timeout <= 0 || timeoutCount != 1 {
				return 0, fmt.Errorf("native proof Windows step has no unique positive -timeout value")
			}
		}
	}
	if matches != 1 {
		return 0, fmt.Errorf("native proof exact Windows CI step missing or duplicated")
	}
	return timeout, nil
}

func TestNativeServiceProofAccounting(t *testing.T) {
	if nativeServiceCleanupReserve != localcell.ServiceHostExitBudget(nativeServiceHostBound) || nativeServiceCleanupReserve != 2*nativeServiceHostBound+localcell.ServiceShutdownTimeout {
		t.Fatal("native cleanup reserve omitted host-exit admission or changed the drain allocation")
	}
	if nativeServiceProofBound+nativeServiceCleanupReserve+nativeServiceHostBound != nativeServiceProofRowTimeout {
		t.Fatal("native proof bound plus cleanup reserve plus host margin must equal the CI row timeout")
	}
	if nativeServiceProofBound <= 0 {
		t.Fatal("native proof bound must be positive")
	}
	if 4*nativeServiceHostBound > nativeServiceProofBound {
		t.Fatal("native proof bound must retain the path-derived readiness floor of four host bounds")
	}
}

func TestNativeServiceProofCleanupReserveSourceContract(t *testing.T) {
	lifecycle := nativeServiceProofContractSource(t, "lifecycle_integration_test.go")
	native := nativeServiceProofContractSource(t, "service_native_test.go")
	if err := nativeServiceProofCleanupReserveContract(lifecycle, native); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		data []byte
	}{
		{"disposableCell cleanup parent", lifecycle},
		{"foreground stop bound", native},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Change the actual duration expression, preserving the rest of the
			// source. The production file may also declare the constant.
			mutant := bytes.ReplaceAll(test.data, []byte(", nativeServiceCleanupReserve)"), []byte(", 75*time.Second)"))
			mutant = bytes.ReplaceAll(mutant, []byte("time.After(nativeServiceCleanupReserve)"), []byte("time.After(75*time.Second)"))
			if bytes.Equal(mutant, test.data) {
				t.Fatal("native proof cleanup reserve mutation no longer applies")
			}
			left, right := lifecycle, native
			if test.name == "disposableCell cleanup parent" {
				left = mutant
			} else {
				right = mutant
			}
			err := nativeServiceProofCleanupReserveContract(left, right)
			if err == nil || !strings.Contains(err.Error(), test.name) {
				t.Fatal("native proof cleanup reserve contract survived literal restoration at its named site")
			}
		})
	}
	for _, change := range []struct {
		name, before, after string
		lifecycle           bool
	}{
		{"shared_cleanup_parent", "beforeCleanup(ctx)", "beforeCleanup(context.Background())", true},
		{"cleanup_stop_binding", "stopForCleanup = stop", "stopForCleanup = nil", false},
		{"cleanup_parent_forwarding", "return stopForCleanup(ctx)", "return stopForCleanup(context.Background())", false},
		{"manager_parent", "adapter.Remove(stopCtx, descriptor)", "adapter.Remove(context.Background(), descriptor)", false},
	} {
		t.Run(change.name, func(t *testing.T) {
			left, right := lifecycle, native
			target := &right
			if change.lifecycle {
				target = &left
			}
			mutant := bytes.Replace(*target, []byte(change.before), []byte(change.after), 1)
			if bytes.Equal(*target, mutant) {
				t.Fatal("native proof shared cleanup mutation no longer applies")
			}
			*target = mutant
			if nativeServiceProofCleanupReserveContract(left, right) == nil {
				t.Fatal("native proof shared cleanup contract survived detached cleanup wiring")
			}
		})
	}
}

func nativeServiceProofCleanupReserveContract(lifecycle, native []byte) error {
	_, disposable, err := nativeServiceProofFunction(lifecycle, "lifecycle_integration_test.go", "disposableCellWithCleanup")
	if err != nil {
		return err
	}
	cleanupParents := 0
	ast.Inspect(disposable.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if ok && nativeServiceProgressNode(call.Fun) == "context.WithTimeout" && len(call.Args) == 2 && nativeServiceProgressNode(call.Args[0]) == "context.Background()" && nativeServiceProgressNode(call.Args[1]) == "nativeServiceCleanupReserve" {
			cleanupParents++
		}
		return true
	})
	if cleanupParents != 1 {
		return fmt.Errorf("disposableCell cleanup parent must use nativeServiceCleanupReserve exactly once")
	}
	var hostCleanup, repairCleanup, removeCleanup token.Pos
	sharedCalls := 0
	ast.Inspect(disposable.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch nativeServiceProgressNode(call) {
		case "beforeCleanup(ctx)":
			hostCleanup = call.Pos()
			sharedCalls++
		case "cleanupCell.Repair(ctx)":
			repairCleanup = call.Pos()
			sharedCalls++
		case "cleanupCell.Remove(ctx, localcell.RemoveOptions{Purge: true})":
			removeCleanup = call.Pos()
			sharedCalls++
		}
		return true
	})
	if sharedCalls != 3 || hostCleanup == token.NoPos || hostCleanup >= repairCleanup || repairCleanup >= removeCleanup {
		return fmt.Errorf("disposableCell host removal, repair, and removal must share one cleanup parent in order")
	}
	_, proof, err := nativeServiceProofFunction(native, "service_native_test.go", "TestNativeServiceLifecycle")
	if err != nil {
		return err
	}
	stop := nativeServiceProofAssignedClosure(proof, "stop")
	if stop == nil {
		return fmt.Errorf("foreground stop bound has no stop closure")
	}
	stopBounds := 0
	ast.Inspect(stop.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		callee := nativeServiceProgressNode(call.Fun)
		if callee == "context.WithTimeout" && len(call.Args) == 2 && nativeServiceProgressNode(call.Args[0]) == "parent" && nativeServiceProgressNode(call.Args[1]) == "nativeServiceCleanupReserve" {
			stopBounds++
		}
		return true
	})
	if stopBounds != 1 {
		return fmt.Errorf("foreground stop bound must use nativeServiceCleanupReserve exactly once")
	}
	bindings, forwarders, proofStops, managerStops := 0, 0, 0, 0
	ast.Inspect(proof.Body, func(node ast.Node) bool {
		if assignment, ok := node.(*ast.AssignStmt); ok && nativeServiceProgressNode(assignment) == "stopForCleanup = stop" {
			bindings++
		}
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch nativeServiceProgressNode(call) {
		case "stop(proofPhase(nativeServiceCleanupReserve))":
			proofStops++
		case "adapter.Remove(stopCtx, descriptor)":
			managerStops++
		}
		if nativeServiceProgressNode(call.Fun) == "disposableCellWithCleanup" && len(call.Args) >= 4 {
			if callback, ok := call.Args[3].(*ast.FuncLit); ok {
				ast.Inspect(callback.Body, func(child ast.Node) bool {
					if childCall, ok := child.(*ast.CallExpr); ok && nativeServiceProgressNode(childCall) == "stopForCleanup(ctx)" {
						forwarders++
					}
					return true
				})
			}
		}
		return true
	})
	if bindings != 1 || forwarders != 1 || proofStops != 2 || managerStops != 1 {
		return fmt.Errorf("native proof stop and cleanup must forward their respective bounded parents")
	}
	return nil
}

func TestNativeServiceProofContextSourceContract(t *testing.T) {
	source := nativeServiceProofContractSource(t, "service_native_test.go")
	if err := nativeServiceProofContextContract(source); err != nil {
		t.Fatal(err)
	}
	for _, mutation := range [][2]string{
		{"proofStarted := time.Now()", "proofStarted := time.Now().Add(time.Second)"},
		{"context.WithDeadline(t.Context(), proofStarted.Add(nativeServiceProofBound))", "context.WithTimeout(t.Context(), nativeServiceProofBound)"},
		{"proofStarted.Add(nativeServiceProofBound)", "proofStarted.Add(nativeServiceProofRowTimeout)"},
	} {
		mutant := bytes.Replace(source, []byte(mutation[0]), []byte(mutation[1]), 1)
		if bytes.Equal(source, mutant) || nativeServiceProofContextContract(mutant) == nil {
			t.Fatal("native proof context contract survived a detached entry deadline")
		}
	}
	positions, proof, err := nativeServiceProofFunction(source, "service_native_test.go", "TestNativeServiceLifecycle")
	if err != nil {
		t.Fatal(err)
	}
	var observe *ast.CallExpr
	ast.Inspect(proof.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if ok && nativeServiceProgressNode(call.Fun) == "cell.Control" && len(call.Args) == 2 && nativeServiceProgressNode(call.Args[1]) == `"observe"` {
			observe = call
		}
		return true
	})
	if observe == nil {
		t.Fatal("native proof context mutation has no observe Control site")
	}
	start, end := positions.Position(observe.Args[0].Pos()).Offset, positions.Position(observe.Args[0].End()).Offset
	for _, parent := range []string{"t.Context()", "context.Background()"} {
		mutant := append([]byte(nil), source[:start]...)
		mutant = append(mutant, parent...)
		mutant = append(mutant, source[end:]...)
		want := fmt.Sprintf("service_native_test.go:%d", positions.Position(observe.Pos()).Line)
		if err := nativeServiceProofContextContract(mutant); err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "cell.Control") {
			t.Fatal("native proof context contract survived an unbounded observe parent or omitted its source line")
		}
	}
}

func nativeServiceProofContextContract(source []byte) error {
	positions, proof, err := nativeServiceProofFunction(source, "service_native_test.go", "TestNativeServiceLifecycle")
	if err != nil {
		return err
	}
	if len(proof.Body.List) == 0 || nativeServiceProgressNode(proof.Body.List[0]) != "proofStarted := time.Now()" {
		return fmt.Errorf("native proof must capture its bound origin at test entry")
	}
	var boundary token.Pos
	declarations := 0
	for _, statement := range proof.Body.List {
		assignment, ok := statement.(*ast.AssignStmt)
		if !ok {
			continue
		}
		for _, lhs := range assignment.Lhs {
			if nativeServiceProgressNode(lhs) == "proofCtx" {
				declarations++
				if nativeServiceProgressNode(assignment) != "proofCtx, cancelProof := context.WithDeadline(t.Context(), proofStarted.Add(nativeServiceProofBound))" {
					return fmt.Errorf("native proof context must retain the test-entry whole-proof deadline")
				}
				boundary = assignment.End()
			}
		}
	}
	if boundary == token.NoPos || declarations != 1 {
		return fmt.Errorf("native proof context declaration missing or duplicated")
	}
	failureGuards := map[string]int{
		`if proofCtx.Err() != nil { diagnostics.logProofBudget() t.Fatal("native service proof budget exhausted") }`:    0,
		`if proofCtx.Err() != nil { logFailure(proofCtx.Err(), nil) t.Fatal("native service proof budget exhausted") }`: 0,
	}
	ast.Inspect(proof.Body, func(node ast.Node) bool {
		if branch, ok := node.(*ast.IfStmt); ok {
			source := nativeServiceProgressNode(branch)
			if _, found := failureGuards[source]; found {
				failureGuards[source]++
			}
		}
		return true
	})
	for _, count := range failureGuards {
		if count != 1 {
			return fmt.Errorf("native proof phase and completion guards must retain their exact exhaustion diagnostic and fatal text")
		}
	}
	// The complete exemption list is intentional: t.Cleanup closures own
	// post-proof cleanup, and only adapter.Remove(context.Background(), ...)
	// within the stop closure may bypass the proof parent to remove registration.
	var cleanupClosures []*ast.FuncLit
	stop := nativeServiceProofAssignedClosure(proof, "stop")
	ast.Inspect(proof.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if ok && nativeServiceProgressNode(call.Fun) == "t.Cleanup" && len(call.Args) == 1 {
			if closure, ok := call.Args[0].(*ast.FuncLit); ok {
				cleanupClosures = append(cleanupClosures, closure)
			}
		}
		return true
	})
	ast.Inspect(proof.Body, func(node ast.Node) bool {
		if err != nil {
			return false
		}
		call, ok := node.(*ast.CallExpr)
		if !ok || call.Pos() <= boundary {
			return true
		}
		for _, closure := range cleanupClosures {
			if call.Pos() > closure.Pos() && call.End() < closure.End() {
				return true
			}
		}
		for _, argument := range call.Args {
			parent := nativeServiceProgressNode(argument)
			if parent != "t.Context()" && parent != "context.Background()" {
				continue
			}
			if parent == "context.Background()" && nativeServiceProgressNode(call.Fun) == "adapter.Remove" && stop != nil && call.Pos() > stop.Pos() && call.End() < stop.End() {
				continue
			}
			err = fmt.Errorf("%s: native proof call %s passes %s outside the named cleanup exemptions", positions.Position(call.Pos()), nativeServiceProgressNode(call.Fun), parent)
		}
		return true
	})
	return err
}

func nativeServiceProofContractSource(t *testing.T, path string) []byte {
	t.Helper()
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cannot read native proof contract source: %s", path)
	}
	return bytes.ReplaceAll(source, []byte("\r\n"), []byte("\n"))
}

func nativeServiceProofFunction(source []byte, path, name string) (*token.FileSet, *ast.FuncDecl, error) {
	positions := token.NewFileSet()
	file, err := parser.ParseFile(positions, path, bytes.ReplaceAll(source, []byte("\r\n"), []byte("\n")), 0)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot parse native proof contract source: %s", path)
	}
	for _, declaration := range file.Decls {
		if function, ok := declaration.(*ast.FuncDecl); ok && function.Name.Name == name {
			return positions, function, nil
		}
	}
	return nil, nil, fmt.Errorf("native proof contract function missing: %s", name)
}

func nativeServiceProofAssignedClosure(proof *ast.FuncDecl, name string) *ast.FuncLit {
	for _, statement := range proof.Body.List {
		assignment, ok := statement.(*ast.AssignStmt)
		if ok && len(assignment.Lhs) == 1 && len(assignment.Rhs) == 1 && nativeServiceProgressNode(assignment.Lhs[0]) == name {
			closure, _ := assignment.Rhs[0].(*ast.FuncLit)
			return closure
		}
	}
	return nil
}

func TestNativeServiceProofCommandDrainSourceContract(t *testing.T) {
	for _, test := range []struct {
		path, function string
		assignments    int
	}{
		{"service_native_progress_test.go", "startForeground", 1},
		{"service_native_diagnostics_test.go", "nativeServiceInvoke", 1},
		{"service_native_diagnostics_test.go", "nativeServiceDiagnosticOutput", 2},
		{"service_native_diagnostics_test.go", "nativeServiceCapabilityDiagnostic", 1},
		{"lifecycle_integration_test.go", "offlineBuildCommand", 1},
		{"lifecycle_proof_test.go", "requirePostgres16", 1},
	} {
		t.Run(test.function, func(t *testing.T) {
			source := nativeServiceProofContractSource(t, test.path)
			positions, function, err := nativeServiceProofFunction(source, test.path, test.function)
			if err != nil {
				t.Fatal(err)
			}
			var delays []*ast.AssignStmt
			ast.Inspect(function.Body, func(node ast.Node) bool {
				assignment, ok := node.(*ast.AssignStmt)
				if ok && len(assignment.Lhs) == 1 && len(assignment.Rhs) == 1 && strings.HasSuffix(nativeServiceProgressNode(assignment.Lhs[0]), ".WaitDelay") && nativeServiceProgressNode(assignment.Rhs[0]) == "time.Second" {
					delays = append(delays, assignment)
				}
				return true
			})
			if len(delays) != test.assignments {
				t.Fatalf("%s must bound command drain with %d one-second WaitDelay assignments", test.function, test.assignments)
			}
			for _, assignment := range delays {
				start, end := positions.Position(assignment.Pos()).Offset, positions.Position(assignment.End()).Offset
				mutant := append([]byte(nil), source[:start]...)
				mutant = append(mutant, source[end:]...)
				_, changed, err := nativeServiceProofFunction(mutant, test.path, test.function)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Count(nativeServiceProgressNode(changed.Body), ".WaitDelay = time.Second") != test.assignments-1 {
					t.Fatal("native proof command drain removal did not remove its named bound")
				}
			}
		})
	}
}

func TestNativeServiceProofHelperParentsSourceContract(t *testing.T) {
	// Shared legacy delegates retain t.Context for their existing callers.
	// These context-taking variants are the exact helpers called by the proof.
	for _, test := range []struct{ path, function string }{
		{"service_native_test.go", "nativeServiceWriteWitness"},
		{"service_native_test.go", "waitServiceReadyAfter"},
		{"service_native_test.go", "assertNativeServiceHostRemoved"},
		{"lifecycle_integration_test.go", "buildRealComponentsContext"},
		{"lifecycle_integration_test.go", "assertReady"},
		{"lifecycle_integration_test.go", "assertStopped"},
		{"lifecycle_proof_test.go", "requirePostgres16"},
		{"lifecycle_proof_test.go", "lifecycleJSON"},
	} {
		t.Run(test.function, func(t *testing.T) {
			source := nativeServiceProofContractSource(t, test.path)
			positions, function, err := nativeServiceProofFunction(source, test.path, test.function)
			if err != nil {
				t.Fatal(err)
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				for _, argument := range call.Args {
					parent := nativeServiceProgressNode(argument)
					if parent == "t.Context()" || parent == "context.Background()" {
						t.Errorf("%s: native proof helper %s passes an unbounded parent", positions.Position(call.Pos()), test.function)
					}
				}
				return true
			})
		})
	}
}

func TestNativeServiceHostExitBudgetSourceContract(t *testing.T) {
	source := nativeServiceProofContractSource(t, "service_native_proof_budget_test.go")
	check := func(source []byte) bool {
		source = bytes.ReplaceAll(source, []byte("\r\n"), []byte("\n"))
		return bytes.Contains(source, []byte("var nativeServiceCleanupReserve = localcell.ServiceHostExitBudget(nativeServiceHostBound)\nvar nativeServiceProofBound = nativeServiceProofRowTimeout - nativeServiceCleanupReserve - nativeServiceHostBound\n"))
	}
	if !check(source) || !check(bytes.ReplaceAll(source, []byte("\n"), []byte("\r\n"))) {
		t.Fatal("native proof reserve does not select the production transaction allowance within the row bound")
	}
	for _, mutation := range [][2]string{
		{"localcell.ServiceHostExitBudget(nativeServiceHostBound)", "localcell.ServiceShutdownTimeout"},
		{"nativeServiceProofRowTimeout - nativeServiceCleanupReserve - nativeServiceHostBound", "nativeServiceProofRowTimeout - nativeServiceHostBound"},
	} {
		mutant := bytes.Replace(source, []byte(mutation[0]), []byte(mutation[1]), 1)
		if bytes.Equal(mutant, source) || check(mutant) {
			t.Fatal("native proof reserve contract survived omitted admission or cleanup allocation")
		}
	}
}
