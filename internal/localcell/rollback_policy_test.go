package localcell

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// rollbackSourceContract checks syntax without loading or executing any target.
// Inputs are normalized so the same mutations exercise LF and Windows checkouts.
func rollbackSourceContract(sources map[string]string) []string {
	var failures []string
	fail := func(reason string) { failures = append(failures, reason) }
	files := make(map[string]*ast.File, len(sources))
	for name, source := range sources {
		file, err := parser.ParseFile(token.NewFileSet(), name, strings.ReplaceAll(source, "\r\n", "\n"), 0)
		if err != nil {
			fail("source parse failed")
			continue
		}
		files[name] = file
	}
	file := files["rollback_policy.go"]
	if file == nil {
		return append(failures, "production source missing")
	}
	if len(file.Imports) != 0 {
		fail("production import forbidden")
	}
	functions := map[string]*ast.FuncDecl{}
	for _, declaration := range file.Decls {
		switch declaration := declaration.(type) {
		case *ast.FuncDecl:
			if declaration.Recv != nil {
				fail("production method forbidden")
			} else {
				functions[declaration.Name.Name] = declaration
			}
			if declaration.Name.Name == "init" {
				fail("production init forbidden")
			}
		case *ast.GenDecl:
			if declaration.Tok == token.VAR {
				fail("production variable forbidden")
			}
		}
	}
	names := map[string]bool{}
	declarations := map[*ast.Ident]bool{}
	declare := func(identifier *ast.Ident) {
		if identifier == nil || declarations[identifier] {
			return
		}
		declarations[identifier] = true
		names[identifier.Name] = true
		if !strings.HasPrefix(identifier.Name, "rollback") {
			fail("declaration prefix forbidden")
		}
	}
	ast.Inspect(file, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.FuncDecl:
			declare(node.Name)
		case *ast.TypeSpec:
			declare(node.Name)
		case *ast.ValueSpec:
			for _, name := range node.Names {
				declare(name)
			}
		case *ast.Field:
			for _, name := range node.Names {
				declare(name)
			}
		case *ast.StructType:
			for _, field := range node.Fields.List {
				if len(field.Names) == 0 {
					declare(rollbackEmbeddedFieldName(field.Type))
				}
			}
		case *ast.LabeledStmt:
			declare(node.Label)
		case *ast.Ident:
			if node.Obj != nil && node.Obj.Pos() == node.Pos() {
				declare(node)
			}
		}
		return true
	})
	verifiedReferences := 0
	ast.Inspect(file, func(node ast.Node) bool {
		if identifier, ok := node.(*ast.Ident); ok && identifier.Name == "rollbackEvidenceVerified" && !declarations[identifier] {
			verifiedReferences++
		}
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		identifier, ok := call.Fun.(*ast.Ident)
		if ok {
			if function := functions[identifier.Name]; function != nil && identifier.Obj != nil && identifier.Obj.Decl == function {
				return true
			}
			if _, builtin := types.Universe.Lookup(identifier.Name).(*types.Builtin); builtin && identifier.Obj == nil {
				return true
			}
		}
		fail("production outside call forbidden")
		return true
	})
	if verifiedReferences != 1 {
		fail("verified reference count invalid")
	}
	for name, other := range files {
		if name == "rollback_policy.go" {
			continue
		}
		ast.Inspect(other, func(node ast.Node) bool {
			if identifier, ok := node.(*ast.Ident); ok && names[identifier.Name] {
				fail("production external reference forbidden")
			}
			return true
		})
	}
	return failures
}

func rollbackEmbeddedFieldName(expression ast.Expr) *ast.Ident {
	switch expression := expression.(type) {
	case *ast.Ident:
		return expression
	case *ast.StarExpr:
		return rollbackEmbeddedFieldName(expression.X)
	case *ast.SelectorExpr:
		return expression.Sel
	case *ast.IndexExpr:
		return rollbackEmbeddedFieldName(expression.X)
	case *ast.IndexListExpr:
		return rollbackEmbeddedFieldName(expression.X)
	}
	return nil
}

type rollbackInputs struct {
	target  rollbackTarget
	applied rollbackApplied
	policy  rollbackPolicy
}

func rollbackBaseline() rollbackInputs {
	return rollbackInputs{
		target:  rollbackTarget{rollbackEvidenceVerified, 9, "linux", "amd64", 12, true, 3, 1, 1, "16", "distribution/extension", true, true, true},
		applied: rollbackApplied{"linux", "amd64", 12, true, 3, 1, 1, "16", "distribution/extension", 10},
		policy:  rollbackPolicy{true, 10, []uint64{9}},
	}
}

type rollbackScenario struct {
	name   string
	reason rollbackReason
	change func(*rollbackInputs)
}

func rollbackScenarios(t *testing.T) []rollbackScenario {
	t.Helper()
	rows := []rollbackScenario{
		{"allowed/baseline", rollbackAllowed, func(*rollbackInputs) {}},
		{"allowed/sync_unselected", rollbackAllowed, func(x *rollbackInputs) {
			x.target.rollbackSyncSelected, x.applied.rollbackSyncSelected = false, false
			x.target.rollbackSyncSchema, x.applied.rollbackSyncSchema = 0, 0
		}},
		{"allowed/legacy_principal", rollbackAllowed, func(x *rollbackInputs) { x.target.rollbackPrincipalLayout, x.applied.rollbackPrincipalLayout = 0, 0 }},
		{"allowed/abi_limit", rollbackAllowed, func(x *rollbackInputs) {
			x.target.rollbackABI, x.applied.rollbackABI = strings.Repeat("x", 128), strings.Repeat("x", 128)
		}},
		{"allowed/list_limit", rollbackAllowed, func(x *rollbackInputs) {
			x.policy.rollbackTargets = make([]uint64, 1024)
			x.policy.rollbackTargets[1023] = 9
		}},
		{"allowed/listed_zero_sequence", rollbackAllowed, func(x *rollbackInputs) { x.target.rollbackSequence = 0; x.policy.rollbackTargets = []uint64{0} }},
		{"allowed/minimum_high_water", rollbackAllowed, func(x *rollbackInputs) {
			x.target.rollbackSequence, x.applied.rollbackHighWater, x.policy.rollbackSequence, x.policy.rollbackTargets = 0, 1, 1, []uint64{0}
		}},
		{"schema_older/core", rollbackSchemaOlder, func(x *rollbackInputs) { x.target.rollbackCoreSchema-- }},
		{"schema_newer/core", rollbackSchemaNewer, func(x *rollbackInputs) { x.target.rollbackCoreSchema++ }},
		{"sync_schema/target_unselected", rollbackSyncSchema, func(x *rollbackInputs) { x.target.rollbackSyncSelected = false }},
		{"sync_schema/applied_unselected", rollbackSyncSchema, func(x *rollbackInputs) { x.applied.rollbackSyncSelected = false }},
		{"sync_schema/version_mismatch", rollbackSyncSchema, func(x *rollbackInputs) { x.target.rollbackSyncSchema++ }},
		{"sync_schema/target_zero", rollbackSyncSchema, func(x *rollbackInputs) { x.target.rollbackSyncSchema = 0 }},
		{"sync_schema/applied_zero", rollbackSyncSchema, func(x *rollbackInputs) { x.applied.rollbackSyncSchema = 0 }},
		{"sync_schema/equal_zero", rollbackSyncSchema, func(x *rollbackInputs) { x.target.rollbackSyncSchema, x.applied.rollbackSyncSchema = 0, 0 }},
		{"sync_schema/equal_negative", rollbackSyncSchema, func(x *rollbackInputs) { x.target.rollbackSyncSchema, x.applied.rollbackSyncSchema = -1, -1 }},
		{"sync_schema/unselected_target_nonzero", rollbackSyncSchema, func(x *rollbackInputs) {
			x.target.rollbackSyncSelected, x.applied.rollbackSyncSelected = false, false
			x.applied.rollbackSyncSchema = 0
		}},
		{"sync_schema/unselected_applied_nonzero", rollbackSyncSchema, func(x *rollbackInputs) {
			x.target.rollbackSyncSelected, x.applied.rollbackSyncSelected = false, false
			x.target.rollbackSyncSchema = 0
		}},
		{"platform/os", rollbackPlatform, func(x *rollbackInputs) { x.target.rollbackOS = "darwin" }},
		{"platform/architecture", rollbackPlatform, func(x *rollbackInputs) { x.target.rollbackArch = "arm64" }},
		{"pg_major/target", rollbackPGMajor, func(x *rollbackInputs) { x.target.rollbackPostgreSQLMajor = "17" }},
		{"pg_major/applied", rollbackPGMajor, func(x *rollbackInputs) { x.applied.rollbackPostgreSQLMajor = "17" }},
		{"pg_major/equal_other", rollbackPGMajor, func(x *rollbackInputs) {
			x.target.rollbackPostgreSQLMajor, x.applied.rollbackPostgreSQLMajor = "17", "17"
		}},
		{"pg_major/exact_spelling", rollbackPGMajor, func(x *rollbackInputs) {
			x.target.rollbackPostgreSQLMajor, x.applied.rollbackPostgreSQLMajor = "16.0", "16.0"
		}},
		{"abi/different_byte", rollbackABI, func(x *rollbackInputs) { x.target.rollbackABI = "distribution/Extension" }},
		{"abi/target_prefix", rollbackABI, func(x *rollbackInputs) { x.target.rollbackABI += "/extra" }},
		{"abi/applied_prefix", rollbackABI, func(x *rollbackInputs) { x.applied.rollbackABI += "/extra" }},
		{"abi/opaque_bytes", rollbackABI, func(x *rollbackInputs) { x.target.rollbackABI += "\x00" }},
		{"abi/different_last_byte", rollbackABI, func(x *rollbackInputs) {
			x.target.rollbackABI, x.applied.rollbackABI = strings.Repeat("x", 127)+"a", strings.Repeat("x", 127)+"b"
		}},
		{"layout/layout", rollbackLayout, func(x *rollbackInputs) { x.target.rollbackLayout++ }},
		{"layout/principal", rollbackLayout, func(x *rollbackInputs) { x.target.rollbackPrincipalLayout++ }},
		{"incomplete/evidence_missing", rollbackIncomplete, func(x *rollbackInputs) { x.target.rollbackEvidence = rollbackEvidenceMissing }},
		{"incomplete/evidence_unknown", rollbackIncomplete, func(x *rollbackInputs) { x.target.rollbackEvidence = "unknown" }},
		{"incomplete/server", rollbackIncomplete, func(x *rollbackInputs) { x.target.rollbackServer = false }},
		{"incomplete/worker", rollbackIncomplete, func(x *rollbackInputs) { x.target.rollbackWorker = false }},
		{"incomplete/postgresql", rollbackIncomplete, func(x *rollbackInputs) { x.target.rollbackPostgreSQL = false }},
		{"incomplete/target_os", rollbackIncomplete, func(x *rollbackInputs) { x.target.rollbackOS = "" }},
		{"incomplete/target_arch", rollbackIncomplete, func(x *rollbackInputs) { x.target.rollbackArch = "" }},
		{"incomplete/applied_os", rollbackIncomplete, func(x *rollbackInputs) { x.applied.rollbackOS = "" }},
		{"incomplete/applied_arch", rollbackIncomplete, func(x *rollbackInputs) { x.applied.rollbackArch = "" }},
		{"incomplete/target_abi_empty", rollbackIncomplete, func(x *rollbackInputs) { x.target.rollbackABI = "" }},
		{"incomplete/applied_abi_empty", rollbackIncomplete, func(x *rollbackInputs) { x.applied.rollbackABI = "" }},
		{"incomplete/target_abi_long", rollbackIncomplete, func(x *rollbackInputs) { x.target.rollbackABI = strings.Repeat("x", 129) }},
		{"incomplete/applied_abi_long", rollbackIncomplete, func(x *rollbackInputs) { x.applied.rollbackABI = strings.Repeat("x", 129) }},
		{"incomplete/target_abi_byte_limit", rollbackIncomplete, func(x *rollbackInputs) { x.target.rollbackABI = strings.Repeat("é", 65) }},
		{"incomplete/applied_abi_byte_limit", rollbackIncomplete, func(x *rollbackInputs) { x.applied.rollbackABI = strings.Repeat("é", 65) }},
		{"incomplete/target_core", rollbackIncomplete, func(x *rollbackInputs) { x.target.rollbackCoreSchema = 0 }},
		{"incomplete/applied_core", rollbackIncomplete, func(x *rollbackInputs) { x.applied.rollbackCoreSchema = 0 }},
		{"incomplete/target_layout", rollbackIncomplete, func(x *rollbackInputs) { x.target.rollbackLayout = 0 }},
		{"incomplete/applied_layout", rollbackIncomplete, func(x *rollbackInputs) { x.applied.rollbackLayout = 0 }},
		{"incomplete/high_water", rollbackIncomplete, func(x *rollbackInputs) { x.applied.rollbackHighWater = 0 }},
		{"incomplete/target_principal", rollbackIncomplete, func(x *rollbackInputs) { x.target.rollbackPrincipalLayout = -1 }},
		{"incomplete/applied_principal", rollbackIncomplete, func(x *rollbackInputs) { x.applied.rollbackPrincipalLayout = -1 }},
		{"incomplete/target_pg_empty", rollbackIncomplete, func(x *rollbackInputs) { x.target.rollbackPostgreSQLMajor = "" }},
		{"incomplete/applied_pg_empty", rollbackIncomplete, func(x *rollbackInputs) { x.applied.rollbackPostgreSQLMajor = "" }},
		{"incomplete/list_limit", rollbackIncomplete, func(x *rollbackInputs) { x.policy.rollbackTargets = make([]uint64, 1025) }},
		{"current_policy/not_permitted", rollbackCurrentPolicy, func(x *rollbackInputs) { x.policy.rollbackPermitted = false }},
		{"current_policy/not_listed", rollbackCurrentPolicy, func(x *rollbackInputs) { x.policy.rollbackTargets = []uint64{8} }},
		{"current_policy/nil_list", rollbackCurrentPolicy, func(x *rollbackInputs) { x.policy.rollbackTargets = nil }},
		{"current_policy/empty_list", rollbackCurrentPolicy, func(x *rollbackInputs) { x.policy.rollbackTargets = []uint64{} }},
		{"current_policy/equal_sequence", rollbackCurrentPolicy, func(x *rollbackInputs) { x.target.rollbackSequence = 10; x.policy.rollbackTargets = []uint64{10} }},
		{"current_policy/above_sequence", rollbackCurrentPolicy, func(x *rollbackInputs) { x.target.rollbackSequence = 11; x.policy.rollbackTargets = []uint64{11} }},
		{"current_policy/policy_ahead", rollbackCurrentPolicy, func(x *rollbackInputs) { x.policy.rollbackSequence++ }},
		{"high_water/older_policy", rollbackHighWater, func(x *rollbackInputs) { x.policy.rollbackSequence-- }},
		{"high_water/zero_policy", rollbackHighWater, func(x *rollbackInputs) { x.policy.rollbackSequence = 0 }},
	}
	byName := map[string]rollbackScenario{}
	for _, row := range rows {
		byName[row.name] = row
	}
	// Schema older/newer are mutually exclusive alternatives within rule 8.
	for _, pair := range [][2]string{
		{"incomplete/server", "high_water/older_policy"}, {"high_water/older_policy", "current_policy/not_permitted"},
		{"current_policy/not_permitted", "platform/os"}, {"platform/os", "pg_major/equal_other"},
		{"pg_major/equal_other", "abi/different_byte"}, {"abi/different_byte", "layout/layout"},
		{"layout/layout", "schema_older/core"}, {"layout/layout", "schema_newer/core"},
		{"schema_older/core", "sync_schema/version_mismatch"}, {"schema_newer/core", "sync_schema/version_mismatch"},
	} {
		first, second := byName[pair[0]], byName[pair[1]]
		rows = append(rows, rollbackScenario{"precedence/" + string(first.reason) + "_before_" + string(second.reason), first.reason, func(x *rollbackInputs) { first.change(x); second.change(x) }})
	}
	// Reflection inventories every input field; this map supplies a one-field
	// perturbation and its expected refusal, so added/forgotten fields fail.
	fields := map[string]rollbackScenario{
		"target.rollbackEvidence": byName["incomplete/evidence_missing"], "target.rollbackSequence": {reason: rollbackCurrentPolicy, change: func(x *rollbackInputs) { x.target.rollbackSequence = 8 }},
		"target.rollbackOS": byName["platform/os"], "target.rollbackArch": byName["platform/architecture"],
		"target.rollbackCoreSchema": byName["schema_newer/core"], "target.rollbackSyncSelected": byName["sync_schema/target_unselected"],
		"target.rollbackSyncSchema": byName["sync_schema/version_mismatch"], "target.rollbackLayout": byName["layout/layout"],
		"target.rollbackPrincipalLayout": byName["layout/principal"], "target.rollbackPostgreSQLMajor": byName["pg_major/target"],
		"target.rollbackABI": byName["abi/different_byte"], "target.rollbackServer": byName["incomplete/server"],
		"target.rollbackWorker": byName["incomplete/worker"], "target.rollbackPostgreSQL": byName["incomplete/postgresql"],
		"applied.rollbackOS":              {reason: rollbackPlatform, change: func(x *rollbackInputs) { x.applied.rollbackOS = "darwin" }},
		"applied.rollbackArch":            {reason: rollbackPlatform, change: func(x *rollbackInputs) { x.applied.rollbackArch = "arm64" }},
		"applied.rollbackCoreSchema":      {reason: rollbackSchemaOlder, change: func(x *rollbackInputs) { x.applied.rollbackCoreSchema++ }},
		"applied.rollbackSyncSelected":    byName["sync_schema/applied_unselected"],
		"applied.rollbackSyncSchema":      {reason: rollbackSyncSchema, change: func(x *rollbackInputs) { x.applied.rollbackSyncSchema++ }},
		"applied.rollbackLayout":          {reason: rollbackLayout, change: func(x *rollbackInputs) { x.applied.rollbackLayout++ }},
		"applied.rollbackPrincipalLayout": {reason: rollbackLayout, change: func(x *rollbackInputs) { x.applied.rollbackPrincipalLayout++ }},
		"applied.rollbackPostgreSQLMajor": byName["pg_major/applied"], "applied.rollbackABI": byName["abi/applied_prefix"],
		"applied.rollbackHighWater": {reason: rollbackHighWater, change: func(x *rollbackInputs) { x.applied.rollbackHighWater++ }},
		"policy.rollbackPermitted":  byName["current_policy/not_permitted"], "policy.rollbackSequence": byName["current_policy/policy_ahead"],
		"policy.rollbackTargets": byName["current_policy/not_listed"],
	}
	baseline := rollbackBaseline()
	for label, value := range map[string]any{"target": baseline.target, "applied": baseline.applied, "policy": baseline.policy} {
		typ := reflect.TypeOf(value)
		for index := range typ.NumField() {
			name := label + "." + typ.Field(index).Name
			row, ok := fields[name]
			if !ok || row.change == nil || row.reason == rollbackAllowed {
				t.Fatal("single field coverage missing")
			}
			row.name = "single_field/" + name
			rows = append(rows, row)
			delete(fields, name)
		}
	}
	if len(fields) != 0 {
		t.Fatal("single field coverage obsolete")
	}
	return rows
}

func TestRollbackPolicy(t *testing.T) {
	rows := rollbackScenarios(t)
	for _, group := range []string{"allowed", "schema_older", "schema_newer", "sync_schema", "platform", "pg_major", "abi", "layout", "incomplete", "current_policy", "high_water", "precedence", "single_field"} {
		t.Run(group, func(t *testing.T) {
			for _, row := range rows {
				parts := strings.SplitN(row.name, "/", 2)
				if parts[0] != group {
					continue
				}
				t.Run(parts[1], func(t *testing.T) {
					input := rollbackBaseline()
					row.change(&input)
					if group == "single_field" {
						before, after, changed := reflect.ValueOf(rollbackBaseline()), reflect.ValueOf(input), 0
						changedField := ""
						for i := range before.NumField() {
							for j := range before.Field(i).NumField() {
								if !rollbackFieldsEqual(before.Field(i).Field(j), after.Field(i).Field(j)) {
									changed++
									changedField = before.Type().Field(i).Name + "." + before.Field(i).Type().Field(j).Name
								}
							}
						}
						if changed != 1 || changedField != parts[1] {
							t.Fatal("single field perturbation invalid")
						}
					}
					decision := rollbackEvaluate(input.target, input.applied, input.policy)
					if decision.rollbackReason != row.reason || decision.rollbackAllowed != (row.reason == rollbackAllowed) {
						t.Fatalf("rollback decision mismatch: expected reason %s, received reason %s", row.reason, decision.rollbackReason)
					}
				})
			}
		})
	}
	t.Run("retained_high_water", func(t *testing.T) {
		for _, row := range rows {
			t.Run(row.name, func(t *testing.T) {
				input := rollbackBaseline()
				row.change(&input)
				decision := rollbackEvaluate(input.target, input.applied, input.policy)
				if decision.rollbackHighWater != input.applied.rollbackHighWater {
					t.Fatal("rollback retained high-water mismatch")
				}
			})
		}
	})
	t.Run("no_mutation", func(t *testing.T) {
		decisionType := reflect.TypeOf(rollbackDecision{})
		for index := range decisionType.NumField() {
			switch decisionType.Field(index).Type.Kind() {
			case reflect.Bool, reflect.String, reflect.Uint64:
			default:
				t.Fatal("rollback decision retains reference-capable field")
			}
		}
		for _, row := range rows {
			t.Run(row.name, func(t *testing.T) {
				input := rollbackBaseline()
				row.change(&input)
				before := input
				before.policy.rollbackTargets = slices.Clone(input.policy.rollbackTargets)
				decision := rollbackEvaluate(input.target, input.applied, input.policy)
				if !reflect.DeepEqual(input, before) {
					t.Fatal("rollback inputs mutated during decision")
				}
				copyDecision := decision
				for index := range input.policy.rollbackTargets {
					input.policy.rollbackTargets[index]++
				}
				if !reflect.DeepEqual(decision, copyDecision) {
					t.Fatal("rollback decision aliases caller list")
				}
			})
		}
	})
}

func rollbackFieldsEqual(left, right reflect.Value) bool {
	if left.Kind() != reflect.Slice {
		return left.Equal(right)
	}
	if left.Len() != right.Len() || left.IsNil() != right.IsNil() {
		return false
	}
	for index := range left.Len() {
		if left.Index(index).Uint() != right.Index(index).Uint() {
			return false
		}
	}
	return true
}

func TestRollbackPolicySourceContract(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal("contract directory read failed")
	}
	sources := map[string]string{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		source, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatal("contract source read failed")
		}
		sources[name] = strings.ReplaceAll(string(source), "\r\n", "\n")
	}
	if failures := rollbackSourceContract(sources); len(failures) != 0 {
		t.Fatalf("source contract violations: %v", failures)
	}
	mutations := []struct {
		name, addition, external, want string
	}{
		{"import", "", "", "production import forbidden"},
		{"method", "\nfunc (rollbackReceiver rollbackTarget) rollbackMethod() {}\n", "", "production method forbidden"},
		{"variable", "\nvar rollbackVariable int\n", "", "production variable forbidden"},
		{"outside_call", "\nfunc rollbackOutside() { externalCall() }\n", "", "production outside call forbidden"},
		{"external_reference", "", "package localcell\nvar unrelated rollbackTarget\n", "production external reference forbidden"},
		{"second_verified_reference", "\nconst rollbackDuplicate = rollbackEvidenceVerified\n", "", "verified reference count invalid"},
		{"init", "\nfunc init() {}\n", "", "production init forbidden"},
		{"field_prefix", "\ntype rollbackInvalid struct { invalid int }\n", "", "declaration prefix forbidden"},
		{"embedded_field_prefix", "\ntype rollbackEmbedded struct { Manifest }\n", "", "declaration prefix forbidden"},
		{"local_prefix", "\nfunc rollbackInvalid() { invalid := 1; _ = invalid }\n", "", "declaration prefix forbidden"},
		{"shadowed_builtin", "\nfunc rollbackInvalid() { len := func() {}; len() }\n", "", "production outside call forbidden"},
		{"function_value_call", "\nfunc rollbackInvalid(rollbackCallback func()) { rollbackCallback() }\n", "", "production outside call forbidden"},
	}
	for _, ending := range []string{"LF", "CRLF"} {
		t.Run(ending, func(t *testing.T) {
			copySources := func() map[string]string {
				result := make(map[string]string, len(sources))
				for name, source := range sources {
					result[name] = source
				}
				return result
			}
			normalize := func(inputs map[string]string) {
				if ending == "CRLF" {
					for name, source := range inputs {
						inputs[name] = strings.ReplaceAll(source, "\n", "\r\n")
					}
				}
			}
			baseline := copySources()
			normalize(baseline)
			if failures := rollbackSourceContract(baseline); len(failures) != 0 {
				t.Fatalf("source baseline violations: %v", failures)
			}
			for _, mutation := range mutations {
				t.Run(mutation.name, func(t *testing.T) {
					inputs := copySources()
					inputs["rollback_policy.go"] += mutation.addition
					if mutation.name == "import" {
						inputs["rollback_policy.go"] = strings.Replace(inputs["rollback_policy.go"], "package localcell", "package localcell\nimport _ \"os\"", 1)
					}
					if mutation.external != "" {
						inputs["rollback_probe.go"] = mutation.external
					}
					normalize(inputs)
					failures := rollbackSourceContract(inputs)
					if slices.Contains(failures, "source parse failed") || !slices.Contains(failures, mutation.want) {
						t.Fatalf("source mutation survived: %s", mutation.name)
					}
				})
			}
		})
	}
}
