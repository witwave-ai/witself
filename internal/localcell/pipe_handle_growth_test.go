package localcell

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// samples contains a baseline followed by one sample per closed-listener cycle.
// At least three cycles are needed: two repeated increases can remain after a
// quiet first cycle. A per-iteration leak adds at least iterations handles in
// each of cycles-1 consecutive cycles. Equal positive growth in EVERY cycle
// also rejects smaller, listener-level leaks. Isolated bursts, uneven small
// increases and late releases are allowed; process-wide counts cannot attribute
// growth, or expose a leak hidden by unrelated releases.
func classifyPipeHandleGrowth(samples []uint32, iterations int) string {
	cycles := len(samples) - 1
	if cycles < 3 || iterations <= 0 {
		return "insufficient"
	}
	consecutive, longest := 0, 0
	firstGrowth := int64(samples[1]) - int64(samples[0])
	uniformGrowth, grew := firstGrowth > 0, false
	for cycle := 1; cycle < len(samples); cycle++ {
		// Subtract after widening so a release cannot wrap into a huge leak.
		growth := int64(samples[cycle]) - int64(samples[cycle-1])
		grew = grew || growth > 0
		uniformGrowth = uniformGrowth && growth == firstGrowth
		if growth >= int64(iterations) {
			consecutive++
			longest = max(longest, consecutive)
		} else {
			consecutive = 0
		}
	}
	if longest >= cycles-1 {
		return "per_iteration"
	}
	if uniformGrowth {
		return "unbounded"
	}
	if grew {
		return "bounded"
	}
	return "steady"
}

func TestPipeHandleGrowth(t *testing.T) {
	for _, test := range []struct {
		name       string
		samples    []uint32
		iterations int
		want       string
	}{
		{"steady", []uint32{100, 100, 100, 100}, 15, "steady"},
		{"one_stray_first", []uint32{100, 101, 101, 101}, 15, "bounded"},
		{"one_stray_middle", []uint32{100, 100, 101, 101}, 15, "bounded"},
		{"one_stray_last", []uint32{100, 100, 100, 101}, 15, "bounded"},
		{"uneven_strays", []uint32{100, 101, 104, 106}, 15, "bounded"},
		{"one_large_burst", []uint32{100, 160, 160, 160}, 15, "bounded"},
		{"one_per_iteration", []uint32{100, 115, 130, 145}, 15, "per_iteration"},
		{"starts_after_first", []uint32{100, 100, 115, 130}, 15, "per_iteration"},
		{"repeats_before_quiet", []uint32{100, 115, 130, 130}, 15, "per_iteration"},
		{"uneven_iteration_leak", []uint32{100, 116, 134, 149}, 15, "per_iteration"},
		{"below_iteration_threshold", []uint32{100, 100, 114, 128}, 15, "bounded"},
		{"uniform_below_threshold", []uint32{100, 114, 128, 142}, 15, "unbounded"},
		{"one_per_listener", []uint32{100, 101, 102, 103}, 15, "unbounded"},
		{"pool_per_listener", []uint32{100, 104, 108, 112}, 15, "unbounded"},
		{"drops_only", []uint32{100, 90, 80, 70}, 15, "steady"},
		{"release_between_bursts", []uint32{100, 115, 90, 105}, 15, "bounded"},
		{"release_then_leak", []uint32{100, 90, 105, 120}, 15, "per_iteration"},
		{"large_release", []uint32{^uint32(0), 0, 0, 0}, 15, "steady"},
		{"four_cycles_repeated", []uint32{100, 100, 115, 130, 145}, 15, "per_iteration"},
		{"four_cycles_interrupted", []uint32{100, 115, 115, 130, 145}, 15, "bounded"},
		{"scaled_iterations", []uint32{100, 100, 103, 106}, 3, "per_iteration"},
		{"scaled_threshold", []uint32{100, 100, 103, 106}, 4, "bounded"},
		{"single_cycle_leak", []uint32{100, 115}, 15, "insufficient"},
		{"single_cycle_flat", []uint32{100, 100}, 15, "insufficient"},
		{"two_cycles", []uint32{100, 115, 130}, 15, "insufficient"},
		{"baseline_only", []uint32{100}, 15, "insufficient"},
		{"empty", nil, 15, "insufficient"},
		{"zero_iterations", []uint32{100, 100, 100, 100}, 0, "insufficient"},
		{"negative_iterations", []uint32{100, 100, 100, 100}, -1, "insufficient"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if classifyPipeHandleGrowth(test.samples, test.iterations) != test.want {
				t.Fatal("pipe handle growth classification changed")
			}
		})
	}
}

// Runtime evidence for the diagnostic wiring, even on hosts that cannot run
// the Windows body. Inspect both newline forms without printing source/counts.
func TestPipeHandleGrowthDiagnosticContract(t *testing.T) {
	source, err := os.ReadFile("service_control_windows_test.go")
	if err != nil {
		t.Fatal("cannot read pipe handle diagnostic source")
	}
	lf := strings.ReplaceAll(string(source), "\r\n", "\n")
	for _, source := range []string{lf, strings.ReplaceAll(lf, "\n", "\r\n")} {
		file, err := parser.ParseFile(token.NewFileSet(), "service_control_windows_test.go", source, 0)
		if err != nil {
			t.Fatal("cannot parse pipe handle diagnostic source")
		}
		found, valid := 0, true
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Name.Name != "TestWindowsPrivatePipeDeadlinesAndHandles" {
				continue
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				block, ok := node.(*ast.BlockStmt)
				if !ok {
					return true
				}
				for index, statement := range block.List {
					fatal := pipeHandleDiagnosticCall(statement, "Fatal")
					if fatal == nil || len(fatal.Args) != 1 || !pipeHandleDiagnosticLiteral(fatal.Args[0], "pipe deadline loop leaked handles") {
						continue
					}
					found++
					if index == 0 {
						valid = false
						continue
					}
					log := pipeHandleDiagnosticCall(block.List[index-1], "Logf")
					if log == nil || len(log.Args) != 2 || !pipeHandleDiagnosticLiteral(log.Args[0], "pipe handle growth class=%s") {
						valid = false
						continue
					}
					class, ok := log.Args[1].(*ast.Ident)
					valid = valid && ok && class.Name == "class"
				}
				return true
			})
		}
		if found != 1 || !valid {
			t.Fatal("pipe handle growth lost its class line before the unchanged fatal")
		}
	}
}

func pipeHandleDiagnosticCall(statement ast.Stmt, method string) *ast.CallExpr {
	expression, ok := statement.(*ast.ExprStmt)
	if !ok {
		return nil
	}
	call, ok := expression.X.(*ast.CallExpr)
	if !ok {
		return nil
	}
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != method {
		return nil
	}
	receiver, ok := selector.X.(*ast.Ident)
	if !ok || receiver.Name != "t" {
		return nil
	}
	return call
}

func pipeHandleDiagnosticLiteral(expression ast.Expr, want string) bool {
	literal, ok := expression.(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return false
	}
	value, err := strconv.Unquote(literal.Value)
	return err == nil && value == want
}
