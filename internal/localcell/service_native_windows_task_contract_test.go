package localcell_test

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
)

// Native execution is Windows-only. This portable contract kills removal of the
// explicit selection and capability-before-side-effects gates, including CRLF.
func TestNativeWindowsTaskManagerPremiseSourceContract(t *testing.T) {
	source := nativeServiceProofContractSource(t, "service_native_windows_task_test.go")
	check := func(data []byte) bool {
		text := string(bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n")))
		capability := strings.Index(text, "capabilityErr := localcell.WindowsLogonTaskCapability(capabilityCtx)")
		refusal := strings.Index(text, "if capabilityErr != nil {")
		missing := strings.Index(text, `t.Log("NOT_RUN service_manager_receipt_missing")`)
		untyped := strings.Index(text, `t.Log("NOT_RUN windows_task_query_result_untyped")`)
		end := strings.Index(text, "\t\treturn\n\t}\n\tproof :=")
		build := strings.Index(text, "binaries := buildRealComponentsContext(")
		return capability >= 0 && refusal > capability && missing > refusal && untyped > missing && end > untyped && build > end &&
			strings.Contains(text, "if !t.Run(\"manager_\"+name, assertion) {\n\t\t\tt.Fatal(\"native task manager stage failed\")\n\t\t}\n\t\tt.Log(\"PASS service_manager_stage=\" + name)")
	}
	if !check(source) || !check(bytes.ReplaceAll(source, []byte("\n"), []byte("\r\n"))) {
		t.Fatal("native task proof bypassed its unavailable premise or child assertion gate")
	}
	for _, mutation := range [][2]string{
		{"if capabilityErr != nil {", "if false {"},
		{"\t\treturn\n\t}\n\tproof :=", "\t}\n\tproof :="},
		{"if !t.Run(\"manager_\"+name, assertion) {", "if false {"},
	} {
		mutant := bytes.Replace(source, []byte(mutation[0]), []byte(mutation[1]), 1)
		if bytes.Equal(source, mutant) || check(mutant) {
			t.Fatal("native task premise mutation survived the static raising site")
		}
	}
	selection := nativeServiceProofContractSource(t, "service_native_test.go")
	needle := []byte("if runtime.GOOS == \"windows\" && os.Getenv(\"WITSELF_LOCALCELL_SERVICE_ADAPTER\") == \"windows-logon-task\" {\n\t\tnativeWindowsTaskManagerLifecycle(t, proofStarted, pgbin)\n\t\treturn\n\t}")
	selected := func(data []byte) bool {
		return bytes.Count(bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n")), needle) == 1
	}
	if !selected(selection) || !selected(bytes.ReplaceAll(selection, []byte("\n"), []byte("\r\n"))) {
		t.Fatal("native task proof lost explicit Windows manager selection")
	}
	for _, mutation := range [][2]string{
		{`runtime.GOOS == "windows" && `, ""},
		{` && os.Getenv("WITSELF_LOCALCELL_SERVICE_ADAPTER") == "windows-logon-task"`, ""},
		{"nativeWindowsTaskManagerLifecycle(t, proofStarted, pgbin)\n\t\treturn", "nativeWindowsTaskManagerLifecycle(t, proofStarted, pgbin)"},
	} {
		mutant := bytes.Replace(selection, []byte(mutation[0]), []byte(mutation[1]), 1)
		if bytes.Equal(selection, mutant) || selected(mutant) {
			t.Fatal("native task manager selection mutation survived the static raising site")
		}
	}
}

func nativeWindowsTaskStage(source []byte, name string) *ast.BlockStmt {
	source = bytes.ReplaceAll(source, []byte("\r\n"), []byte("\n"))
	file, err := parser.ParseFile(token.NewFileSet(), "native.go", source, 0)
	if err != nil {
		return nil
	}
	var stage *ast.BlockStmt
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != "nativeWindowsTaskManagerLifecycle" || function.Body == nil {
			continue
		}
		for _, statement := range function.Body.List {
			expression, ok := statement.(*ast.ExprStmt)
			if !ok {
				continue
			}
			call, ok := expression.X.(*ast.CallExpr)
			if !ok || nativeServiceProgressNode(call.Fun) != "stage" || len(call.Args) != 2 {
				continue
			}
			label, ok := call.Args[0].(*ast.BasicLit)
			if !ok {
				continue
			}
			value, err := strconv.Unquote(label.Value)
			if err != nil || value != name {
				continue
			}
			callback, ok := call.Args[1].(*ast.FuncLit)
			if !ok || stage != nil {
				return nil
			}
			stage = callback.Body
		}
	}
	return stage
}

func nativeWindowsTaskPrimaryWitnessContract(source []byte) bool {
	body := nativeWindowsTaskStage(source, "readiness_write")
	if body == nil || len(body.List) != 2 ||
		nativeServiceProgressNode(body.List[0]) != "ready(t, primary)" ||
		nativeServiceProgressNode(body.List[1]) != "read = nativeServiceWriteWitnessWithTest(ctx, t, proof, primary.root, assertReady(t, primary.cell, phase(nativeServiceHostBound)))" {
		return false
	}
	read := nativeWindowsTaskStage(source, "persisted_read")
	return read != nil && len(read.List) == 1 && nativeServiceProgressNode(read.List[0]) == "read(t)"
}

func nativeWindowsTaskCleanupContract(source []byte) bool {
	body := nativeWindowsTaskStage(source, "unregister")
	if body == nil || len(body.List) < 2 {
		return false
	}
	guard, ok := body.List[0].(*ast.IfStmt)
	return ok && guard.Else == nil &&
		nativeServiceProgressNode(guard.Init) == "err := adapter.Remove(phase(localcell.ServiceHostExitBudget(nativeServiceHostBound)), primary.descriptor)" &&
		nativeServiceProgressNode(guard.Cond) == "err != nil" &&
		len(guard.Body.List) == 1 && nativeServiceProgressNode(guard.Body.List[0]) == `t.Fatal("cannot remove exact owned native task")` &&
		nativeServiceProgressNode(body.List[1]) == "primary.registered = false"
}

func nativeWindowsTaskRetainedDataContract(source []byte) bool {
	body := nativeWindowsTaskStage(source, "bystander_retained")
	if body == nil || len(body.List) == 0 {
		return false
	}
	loop, ok := body.List[len(body.List)-1].(*ast.RangeStmt)
	if !ok || nativeServiceProgressNode(loop.Key) != "_" || nativeServiceProgressNode(loop.Value) != "name" ||
		nativeServiceProgressNode(loop.X) != `[]string{"pg", "manifest.json"}` || len(loop.Body.List) != 1 {
		return false
	}
	guard, ok := loop.Body.List[0].(*ast.IfStmt)
	return ok && guard.Else == nil &&
		nativeServiceProgressNode(guard.Init) == "_, err := os.Stat(filepath.Join(primary.root, name))" &&
		nativeServiceProgressNode(guard.Cond) == "err != nil" && len(guard.Body.List) == 1 &&
		nativeServiceProgressNode(guard.Body.List[0]) == `t.Fatal("native task unregister lost retained installation data")`
}

func nativeWindowsTaskBodyContractTest(t *testing.T, check func([]byte) bool, from, to, refusal string) {
	t.Helper()
	source := nativeServiceProofContractSource(t, "service_native_windows_task_test.go")
	if !check(source) || !check(bytes.ReplaceAll(source, []byte("\n"), []byte("\r\n"))) {
		t.Fatal(refusal)
	}
	mutant := bytes.Replace(source, []byte(from), []byte(to), 1)
	if bytes.Equal(source, mutant) || check(mutant) {
		t.Fatal("native task body contract accepted its named mutation")
	}
}

func TestNativeWindowsTaskPrimaryWitnessSourceContract(t *testing.T) {
	nativeWindowsTaskBodyContractTest(t, nativeWindowsTaskPrimaryWitnessContract,
		"read = nativeServiceWriteWitnessWithTest(ctx, t, proof, primary.root, assertReady(t, primary.cell, phase(nativeServiceHostBound)))",
		"read = func(*testing.T) {}", "native task primary authenticated witness bypassed")
}

func TestNativeWindowsTaskCleanupSourceContract(t *testing.T) {
	nativeWindowsTaskBodyContractTest(t, nativeWindowsTaskCleanupContract,
		"if err := adapter.Remove(phase(localcell.ServiceHostExitBudget(nativeServiceHostBound)), primary.descriptor); err != nil {",
		"if err := adapter.Remove(phase(localcell.ServiceHostExitBudget(nativeServiceHostBound)), primary.descriptor); false && err != nil {",
		"native task unregister cleanup error guard bypassed")
}

func TestNativeWindowsTaskRetainedDataSourceContract(t *testing.T) {
	nativeWindowsTaskBodyContractTest(t, nativeWindowsTaskRetainedDataContract,
		"if _, err := os.Stat(filepath.Join(primary.root, name)); err != nil {",
		"if _, err := os.Stat(filepath.Join(primary.root, name)); false && err != nil {",
		"native task retained installation data guard bypassed")
}

func TestNativeWindowsTaskProportionalBounds(t *testing.T) {
	if nativeWindowsTaskCapabilityBound <= 0 || nativeWindowsTaskCapabilityBound*12 != nativeServiceHostBound ||
		nativeWindowsTaskClosureBound <= 0 || nativeWindowsTaskClosureBound*60 != nativeServiceHostBound {
		t.Fatal("native task capability and closure bounds lost host timing proportions")
	}
	source := string(nativeServiceProofContractSource(t, "service_native_windows_task_test.go"))
	for _, fragment := range []string{
		"const nativeWindowsTaskCapabilityBound = nativeServiceHostBound / 12",
		"const nativeWindowsTaskClosureBound = nativeServiceHostBound / 60",
		"context.WithTimeout(ctx, nativeWindowsTaskCapabilityBound)",
		"localcell.WindowsLogonTaskCapability(phase(nativeWindowsTaskCapabilityBound))",
	} {
		if strings.Count(source, fragment) != 1 {
			t.Fatal("native task fixture lost a named proportional deadline")
		}
	}
	if strings.Count(source, "assertNativeServiceHostRemoved(phase(nativeWindowsTaskClosureBound),") != 2 {
		t.Fatal("native task absence observations lost their proportional bound")
	}
}
