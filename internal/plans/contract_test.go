package plans

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"slices"
	"strconv"
	"testing"
)

func TestWorkerPlanContractCurrent(t *testing.T) {
	want, err := WorkerValidationContractJSON()
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile("../../infra/cloudflare/control-plane/src/plan-contract.json")
	if err != nil {
		t.Fatalf("read generated Worker plan contract: %v; run make plan-contract", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("Worker plan contract is stale; run make plan-contract")
	}
}

func TestWorkerPlanContractMatchesGoValidators(t *testing.T) {
	contract := WorkerValidationContract()
	if !slices.Equal(contract.FeatureKeys, SupportedFeatureKeys()) {
		t.Fatal("Worker feature keys differ from Go")
	}
	if got := slices.Sorted(maps.Keys(contract.LimitMaximums)); !slices.Equal(got, SupportedLimitKeys()) {
		t.Fatalf("Worker limit keys %v differ from Go %v", got, SupportedLimitKeys())
	}
	if got := slices.Sorted(maps.Keys(contract.PolicyBounds)); !slices.Equal(got, SupportedPolicyKeys()) {
		t.Fatalf("Worker policy keys %v differ from Go %v", got, SupportedPolicyKeys())
	}
	if err := ValidateFeatures(contract.FeatureKeys); err != nil {
		t.Fatalf("Worker features: %v", err)
	}
	for key, maximum := range contract.LimitMaximums {
		t.Run("limit/"+key, func(t *testing.T) {
			for _, value := range []int64{0, maximum} {
				if err := ValidateLimits(map[string]int64{key: value}); err != nil {
					t.Fatalf("exported boundary %d rejected: %v", value, err)
				}
			}
			for _, value := range []int64{-1, maximum + 1} {
				if err := ValidateLimits(map[string]int64{key: value}); err == nil {
					t.Fatalf("value %d outside exported bounds accepted", value)
				}
			}
		})
	}
	for key, bounds := range contract.PolicyBounds {
		t.Run("policy/"+key, func(t *testing.T) {
			for _, value := range []int64{bounds.Minimum, bounds.Maximum} {
				if err := ValidatePolicies(map[string]int64{key: value}); err != nil {
					t.Fatalf("exported boundary %d rejected: %v", value, err)
				}
			}
			for _, value := range []int64{bounds.Minimum - 1, bounds.Maximum + 1} {
				if err := ValidatePolicies(map[string]int64{key: value}); err == nil {
					t.Fatalf("value %d outside exported bounds accepted", value)
				}
			}
		})
	}
}

func TestWorkerPlanContractMatchesCellPlanFitDimensions(t *testing.T) {
	// Inspect the emission sites rather than duplicating their dimension list:
	// adding, removing, reordering, or changing the scope of a cell refusal must
	// update the generated Worker contract even when DB tests cannot run.
	// This test scans only internal/store/plan_fit.go; register any new emission source here.
	parse := func(path string) *ast.File {
		t.Helper()
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		return file
	}
	cell := parse("../store/plan_fit.go")
	constants := make(map[string]string)
	for prefix, file := range map[string]*ast.File{"plans.": parse("plans.go"), "": cell} {
		for _, decl := range file.Decls {
			group, ok := decl.(*ast.GenDecl)
			if !ok || group.Tok != token.CONST {
				continue
			}
			for _, spec := range group.Specs {
				value := spec.(*ast.ValueSpec)
				for i, expr := range value.Values {
					literal, ok := expr.(*ast.BasicLit)
					if !ok || literal.Kind != token.STRING {
						continue
					}
					text, err := strconv.Unquote(literal.Value)
					if err != nil {
						t.Fatal(err)
					}
					constants[prefix+value.Names[i].Name] = text
				}
			}
		}
	}
	constant := func(expr ast.Expr) string {
		t.Helper()
		name := ""
		switch expr := expr.(type) {
		case *ast.Ident:
			name = expr.Name
		case *ast.SelectorExpr:
			if qualifier, ok := expr.X.(*ast.Ident); ok {
				name = qualifier.Name + "." + expr.Sel.Name
			}
		}
		value, ok := constants[name]
		if !ok {
			t.Fatalf("plan-fit emission must use a known string constant; unresolved %q (%T)", name, expr)
		}
		return value
	}
	var emitted []PlanFitDimension
	ast.Inspect(cell, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		method, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		scope := constants["PlanFitScopeAccount"]
		switch method.Sel.Name {
		case "addAccountViolation":
		case "addScopedViolation":
			scope = constant(call.Args[1])
		default:
			return true
		}
		emitted = append(emitted, PlanFitDimension{constant(call.Args[0]), scope})
		return true
	})
	contract := WorkerValidationContract()
	if len(emitted) == 0 || !slices.Equal(contract.PlanFitDimensions, emitted) {
		t.Fatalf("Worker plan-fit dimensions %v differ from cell emissions %v", contract.PlanFitDimensions, emitted)
	}
	seen := make(map[string]bool)
	for _, fit := range contract.PlanFitDimensions {
		if _, ok := contract.LimitMaximums[fit.Dimension]; !ok || seen[fit.Dimension] {
			t.Fatalf("plan-fit dimension %q must be a unique supported limit", fit.Dimension)
		}
		seen[fit.Dimension] = true
	}
}
