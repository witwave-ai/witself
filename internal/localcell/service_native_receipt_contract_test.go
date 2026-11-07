package localcell_test

import (
	"bytes"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The behavioral stop fixtures exercise the shared receipt helper. This source
// contract also binds the native-only proof's wrapper to that same helper.
func TestNativeServiceShutdownReceiptBinding(t *testing.T) {
	source := nativeServiceReceiptContractSource(t, "service_native_test.go")
	if !nativeServiceShutdownReceiptBinding(source) {
		t.Fatal("native proof bypassed the shared successful-stop receipt helper")
	}
	const forwarding = "return nativeServiceStopWithShutdownReceipt(runtime.GOOS, root, label, diagnostics.foregroundStderr, t.Logf, diagnosticRun, stopHost)"
	for _, replacement := range []string{
		"return stopHost()",
		strings.Replace(forwarding, ", stopHost)", ", func() error { return nil })", 1),
		strings.Replace(forwarding, "diagnostics.foregroundStderr", "nil", 1),
	} {
		mutant := strings.Replace(string(source), forwarding, replacement, 1)
		if mutant == string(source) || nativeServiceShutdownReceiptBinding([]byte(mutant)) {
			t.Fatal("native shutdown binding survived a detached stop or capture mutation")
		}
	}
}

func nativeServiceShutdownReceiptBinding(source []byte) bool {
	file, err := parser.ParseFile(token.NewFileSet(), "native.go", source, 0)
	if err != nil {
		return false
	}
	bindings, paths := 0, 0
	for _, declaration := range file.Decls {
		proof, ok := declaration.(*ast.FuncDecl)
		if !ok || proof.Name.Name != "TestNativeServiceLifecycle" || proof.Body == nil {
			continue
		}
		for _, statement := range proof.Body.List {
			assignment, ok := statement.(*ast.AssignStmt)
			if !ok || len(assignment.Lhs) != 1 || len(assignment.Rhs) != 1 {
				continue
			}
			switch nativeServiceReceiptNode(assignment.Lhs[0]) {
			case "stopWithReceipt":
				wrapper, ok := assignment.Rhs[0].(*ast.FuncLit)
				if !ok || len(wrapper.Body.List) != 1 || nativeServiceReceiptNode(wrapper.Type) != "func(stopHost func() error) error" ||
					nativeServiceReceiptNode(wrapper.Body.List[0]) != "return nativeServiceStopWithShutdownReceipt(runtime.GOOS, root, label, diagnostics.foregroundStderr, t.Logf, diagnosticRun, stopHost)" {
					return false
				}
				bindings++
			case "stop":
				ast.Inspect(assignment.Rhs[0], func(node ast.Node) bool {
					if call, ok := node.(*ast.CallExpr); ok && nativeServiceReceiptNode(call.Fun) == "stopWithReceipt" && len(call.Args) == 1 {
						if _, ok := call.Args[0].(*ast.FuncLit); ok {
							paths++
						}
					}
					return true
				})
			}
		}
	}
	return bindings == 1 && paths == 2
}

// The checker can enroll a direct literal or a formatted fixed prefix, but a
// variadic Fatal/Error concatenates dynamic values and loses that enrollment.
// Scan both packages, including platform files, without running native hosts.
func TestNativeServiceDiagnosticDirectLiteralContract(t *testing.T) {
	for _, directory := range []string{".", "../tools/localcellharness"} {
		files, err := filepath.Glob(filepath.Join(directory, "*_test.go"))
		if err != nil || len(files) == 0 {
			t.Fatal("cannot enumerate diagnostic contract test sources")
		}
		for _, path := range files {
			if !nativeServiceDiagnosticDirectLiterals(nativeServiceReceiptContractSource(t, path)) {
				t.Errorf("diagnostic source contains a dynamic composite assertion: %s", filepath.Base(path))
			}
		}
	}
}

func TestNativeServiceDiagnosticDirectLiteralMutations(t *testing.T) {
	source := nativeServiceReceiptContractSource(t, "../tools/localcellharness/trust_test.go")
	// Each raising site is replayed independently, including both stage refusals.
	for _, literal := range []string{"stage refused", "activation refused", "committed offline activation refused", "receipt refused", "cannot prepare committed fixture", "empty ownership declaration dispatch refused"} {
		needle := `t.Fatal("` + literal + `")`
		count := strings.Count(string(source), needle)
		want := 1
		if literal == "stage refused" {
			want = 2
		}
		if count != want {
			t.Fatal("harness direct-literal mutation lost a required raising site")
		}
		for start := 0; ; {
			offset := strings.Index(string(source[start:]), needle)
			if offset < 0 {
				break
			}
			offset += start
			mutant := string(source[:offset]) + `t.Fatal("` + literal + `:", err)` + string(source[offset+len(needle):])
			if nativeServiceDiagnosticDirectLiterals([]byte(mutant)) {
				t.Fatal("diagnostic contract accepted a restored composite harness assertion")
			}
			start = offset + len(needle)
		}
	}
}

func nativeServiceDiagnosticDirectLiterals(source []byte) bool {
	file, err := parser.ParseFile(token.NewFileSet(), "diagnostics.go", source, 0)
	if err != nil {
		return false
	}
	valid := true
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) < 2 {
			return true
		}
		method, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || (method.Sel.Name != "Fatal" && method.Sel.Name != "Error") {
			return true
		}
		if literal, ok := call.Args[0].(*ast.BasicLit); ok && literal.Kind == token.STRING {
			valid = false
		}
		return true
	})
	return valid
}

func nativeServiceReceiptContractSource(t *testing.T, path string) []byte {
	t.Helper()
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("cannot read diagnostic source contract")
	}
	return bytes.ReplaceAll(source, []byte("\r\n"), []byte("\n"))
}

func nativeServiceReceiptNode(node ast.Node) string {
	var output bytes.Buffer
	if format.Node(&output, token.NewFileSet(), node) != nil {
		return ""
	}
	return strings.Join(strings.Fields(output.String()), " ")
}
