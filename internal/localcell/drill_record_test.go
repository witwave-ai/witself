package localcell

import (
	"encoding/json"
	"errors"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"strconv"
	"strings"
	"testing"
)

func drillFixture() drillRecord {
	return drillRecord{Format: 1, DrillID: "drill_abcdefghijklmnop", OperationID: "origin_run", Binding: drillBinding{
		InstallationID: "installation_one", CheckpointID: "ckpt_abcdefghijklmnop", InventorySHA256: strings.Repeat("b", 64),
		LayoutVersion: 1, PrincipalLayoutVersion: 0, SchemaVersion: 1, PostgresMajor: "16"}, State: "reserved", CleanupDue: true}
}
func drillAssert(t *testing.T, condition bool, message string) {
	t.Helper()
	if !condition {
		t.Fatal(message)
	}
}
func drillJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	drillAssert(t, err == nil, "drill fixture encoding failed")
	return b
}
func TestDrillRecord(t *testing.T) {
	base := drillFixture()
	states := []string{"reserved", "copied", "verified", "launched", "probed", "stopped"}
	check := func(t *testing.T, err error, reason drillReason) {
		t.Helper()
		drillAssert(t, reason == "" && err == nil || reason != "" && errors.Is(err, drillError{reason}), "drill expected reason absent")
	}
	parse := func(t *testing.T, data []byte, reason drillReason) {
		t.Helper()
		_, err := drillParseRecord(data)
		check(t, err, reason)
	}
	at := func(state, outcome string) drillRecord { r := base; r.State, r.Outcome = state, outcome; return r }
	mutate := func(f func(*drillRecord)) drillRecord { r := base; f(&r); return r }
	t.Run("validator_agreement", func(t *testing.T) {
		probes := []struct {
			value               string
			installation, drill bool
		}{
			{"", false, false}, {"a", true, false}, {strings.Repeat("a", 16), true, false},
			{strings.Repeat("a", 63), true, false}, {strings.Repeat("a", 64), true, false}, {strings.Repeat("a", 65), false, false},
			{strings.Repeat("a", 127), false, false}, {strings.Repeat("a", 128), false, false}, {strings.Repeat("a", 129), false, false},
			{"Z", true, false}, {"5", true, false}, {"_", true, false}, {"-", true, false},
			{" ", false, false}, {".", false, false}, {"/", false, false}, {":", false, false}, {"\x00", false, false},
			{"\n", false, false}, {"valid\n", false, false}, {"é", false, false}, {"界", false, false}, {"\xff", false, false},
			{"wrong_abcdefghijklmnop", true, false}, {"ckptabcdefghijklmnop", true, false}, {"drillabcdefghijklmnop", true, false},
			{"ckpt_abcdefghijklmno", true, false}, {"ckpt_abcdefghijklmnop", true, false}, {"ckpt_abcdefghijklmnopq", true, false},
			{"drill_abcdefghijklmno", true, false}, {"drill_abcdefghijklmnop", true, true}, {"drill_abcdefghijklmnopq", true, false},
			{"ckpt_Abcdefghijklmnop", true, false}, {"drill_Abcdefghijklmnop", true, false},
			{"ckpt_0bcdefghijklmnop", true, false}, {"drill_0bcdefghijklmnop", true, false},
			{"ckpt_1bcdefghijklmnop", true, false}, {"drill_1bcdefghijklmnop", true, false},
			{"ckpt_8bcdefghijklmnop", true, false}, {"drill_8bcdefghijklmnop", true, false},
			{"ckpt_9bcdefghijklmnop", true, false}, {"drill_9bcdefghijklmnop", true, false},
			{"ckpt_abcdefghijklmnop\n", false, false}, {"drill_abcdefghijklmnop\n", false, false},
			{"ckpt_2345672345672345", true, false}, {"drill_2345672345672345", true, true},
		}
		for i, p := range probes {
			if drillValidOperationID(p.value) != validUpdateOperationID(p.value) || drillValidCheckpointID(p.value) != checkpointIDPattern.MatchString(p.value) || drillValidInstallationID(p.value) != p.installation || drillValidID(p.value) != p.drill {
				t.Fatalf("drill validator disagreement at probe %d", i)
			}
		}
	})
	t.Run("zero_on_error", func(t *testing.T) {
		bad := mutate(func(r *drillRecord) { r.Format = 0 })
		for _, r := range []drillRecord{base, bad} {
			for _, call := range []func() (drillRecord, error){
				func() (drillRecord, error) { return drillParseRecord(drillJSON(t, at("reserved", "passed"))) },
				func() (drillRecord, error) { return drillParseRecord([]byte("{")) },
				func() (drillRecord, error) { return drillAdvance(r, r.OperationID, "stopped") },
				func() (drillRecord, error) { return drillStop(r, r.OperationID) },
				func() (drillRecord, error) { return drillFinish(r, r.OperationID, "passed") },
				func() (drillRecord, error) { r.CleanupDue = false; return drillCleanup(r, true) },
			} {
				got, err := call()
				drillAssert(t, err != nil && got == (drillRecord{}), "drill refusal returned record")
			}
			got, err := drillMakeReceipt(r)
			drillAssert(t, err != nil && got == (drillReceipt{}), "drill refusal returned receipt")
			action, err := drillResume(r, "")
			drillAssert(t, err != nil && action == "", "drill refusal returned action")
		}
		for _, data := range [][]byte{[]byte("{"), drillJSON(t, drillReceipt{Format: 1, DrillID: base.DrillID, Binding: base.Binding})} {
			got, err := drillParseReceipt(data)
			drillAssert(t, err != nil && got == (drillReceipt{}), "drill refusal parsed receipt")
		}
	})
	t.Run("reason_identity", func(t *testing.T) {
		parseError := func(data []byte) error { _, err := drillParseRecord(data); return err }
		stepError := func(_ drillRecord, err error) error { return err }
		r := at("stopped", "passed")
		receipt, err := drillMakeReceipt(r)
		check(t, err, "")
		rows := []struct {
			reason drillReason
			err    error
		}{
			{drillTooLarge, parseError([]byte(strings.Repeat(" ", drillMaxBytes+1)))}, {drillMalformed, parseError([]byte("{"))},
			{drillUnknownVersion, drillValidate(mutate(func(r *drillRecord) { r.Format = 0 }))},
			{drillIdentityInvalid, drillValidate(mutate(func(r *drillRecord) { r.DrillID = "!" }))},
			{drillBindingInvalid, drillValidate(mutate(func(r *drillRecord) { r.Binding = drillBinding{} }))},
			{drillStateInvalid, drillValidate(mutate(func(r *drillRecord) { r.State = "unknown" }))},
			{drillOutcomeBeforeStop, drillValidate(at("reserved", "passed"))}, {drillStopRequired, drillValidate(at("launched", "failed"))},
			{drillTransitionRefused, stepError(drillAdvance(base, base.OperationID, "stopped"))},
			{drillStaleResume, stepError(drillAdvance(base, "other_run", "copied"))},
			{drillReceiptBinding, drillCheckReceipt(receipt, at("stopped", "failed"), r.Binding)},
			{drillReceiptStale, drillCheckReceipt(receipt, r, drillBinding{})},
		}
		for i, row := range rows {
			value, ok := row.err.(drillError)
			drillAssert(t, ok && value == (drillError{row.reason}), "drill reason is not error value")
			drillAssert(t, row.err.Error() == rows[0].err.Error(), "drill reason text differs")
			for j, target := range rows {
				drillAssert(t, errors.Is(row.err, drillError{target.reason}) == (i == j), "drill reason identity mismatch")
			}
		}
	})
	t.Run("round_trip", func(t *testing.T) {
		for _, state := range states {
			for _, outcome := range []string{"", "passed", "failed"} {
				r := at(state, outcome)
				if outcome == "passed" && state != "stopped" || outcome == "failed" && (state == "launched" || state == "probed") {
					continue
				}
				for _, due := range []bool{false, true} {
					r.CleanupDue = due
					got, err := drillParseRecord(drillJSON(t, r))
					check(t, err, "")
					drillAssert(t, got == r && string(drillJSON(t, got)) == string(drillJSON(t, r)), "drill round trip changed value")
					if outcome != "" {
						receipt, err := drillMakeReceipt(r)
						check(t, err, "")
						gotReceipt, err := drillParseReceipt(drillJSON(t, receipt))
						check(t, err, "")
						drillAssert(t, gotReceipt == receipt, "drill receipt round trip changed")
					}
				}
			}
		}
	})
	for _, row := range []struct {
		name   string
		reason drillReason
		change func(string) string
	}{
		{"unknown_version", drillUnknownVersion, func(s string) string { return strings.Replace(s, `"format":1`, `"format":2`, 1) }},
		{"duplicate_field", drillMalformed, func(s string) string { return strings.Replace(s, `"format":1`, `"format":1,"format":1`, 1) }},
		{"unknown_field", drillMalformed, func(s string) string { return strings.Replace(s, `"format":1`, `"format":1,"extra":0`, 1) }},
		{"wrong_type", drillMalformed, func(s string) string { return strings.Replace(s, `"format":1`, `"format":"1"`, 1) }},
		{"trailing_data", drillMalformed, func(s string) string { return s + "{}" }},
		{"exact_size", "", func(s string) string { return s + strings.Repeat(" ", drillMaxBytes-len(s)) }},
		{"too_large", drillTooLarge, func(s string) string { return s + strings.Repeat(" ", drillMaxBytes+1-len(s)) }},
		{"missing_cleanup_due", drillMalformed, func(s string) string { return strings.Replace(s, `,"cleanup_due":true`, "", 1) }},
	} {
		t.Run(row.name, func(t *testing.T) {
			got, err := drillParseRecord([]byte(row.change(string(drillJSON(t, base)))))
			check(t, err, row.reason)
			drillAssert(t, err == nil && got == base || err != nil && got == (drillRecord{}), "drill parser result incorrect")
			r := at("stopped", "passed")
			receipt, err := drillMakeReceipt(r)
			check(t, err, "")
			gotReceipt, err := drillParseReceipt([]byte(row.change(string(drillJSON(t, receipt)))))
			check(t, err, row.reason)
			drillAssert(t, err == nil && gotReceipt == receipt || err != nil && gotReceipt == (drillReceipt{}), "drill parser result incorrect")
			if row.name == "duplicate_field" {
				parse(t, []byte(strings.Replace(string(drillJSON(t, base)), `"schema_version":1`, `"schema_version":1,"schema_version":1`, 1)), drillMalformed)
			}
			if row.name == "unknown_version" {
				for _, version := range []int{-1, 0, 3} {
					r := base
					r.Format = version
					parse(t, drillJSON(t, r), drillUnknownVersion)
				}
			}
			if row.name == "wrong_type" {
				var object map[string]any
				drillAssert(t, json.Unmarshal(drillJSON(t, base), &object) == nil, "drill fixture decoding failed")
				for key, old := range object {
					for _, bad := range []any{nil, []any{}} {
						object[key] = bad
						parse(t, drillJSON(t, object), drillMalformed)
					}
					object[key] = old
				}
				binding := object["binding"].(map[string]any)
				for key, old := range binding {
					binding[key] = nil
					parse(t, drillJSON(t, object), drillMalformed)
					binding[key] = old
				}
			}
		})
	}
	t.Run("identity", func(t *testing.T) {
		for _, id := range []string{"", "drill_short", "drill_ABCDEFGHIJKLMNOP", "drill_abcdefghijklmnopx", "drill_abcdefghijklmn01"} {
			r := base
			r.DrillID = id
			parse(t, drillJSON(t, r), drillIdentityInvalid)
		}
		for _, op := range []string{"", "bad.dot", strings.Repeat("a", 129)} {
			r := base
			r.OperationID = op
			parse(t, drillJSON(t, r), drillIdentityInvalid)
			_, err := drillAdvance(base, op, "copied")
			check(t, err, drillIdentityInvalid)
			_, err = drillResume(base, op)
			check(t, err, drillIdentityInvalid)
		}
		r := base
		r.OperationID = strings.Repeat("a", 128)
		parse(t, drillJSON(t, r), "")
	})
	t.Run("binding_incomplete", func(t *testing.T) {
		var object map[string]any
		drillAssert(t, json.Unmarshal(drillJSON(t, base), &object) == nil, "drill fixture decoding failed")
		binding := object["binding"].(map[string]any)
		for key, old := range binding {
			delete(binding, key)
			parse(t, drillJSON(t, object), drillBindingInvalid)
			binding[key] = old
		}
		delete(object, "binding")
		parse(t, drillJSON(t, object), drillBindingInvalid)
		for _, change := range []func(*drillBinding){func(b *drillBinding) { b.InstallationID = "" }, func(b *drillBinding) { b.InstallationID = strings.Repeat("a", 65) }, func(b *drillBinding) { b.InstallationID = "bad.dot" }, func(b *drillBinding) { b.CheckpointID = "bad" }, func(b *drillBinding) { b.InventorySHA256 = strings.Repeat("A", 64) }, func(b *drillBinding) { b.InventorySHA256 = "bad" }, func(b *drillBinding) { b.LayoutVersion = 0 }, func(b *drillBinding) { b.PrincipalLayoutVersion = -1 }, func(b *drillBinding) { b.SchemaVersion = 0 }, func(b *drillBinding) { b.PostgresMajor = "17" }} {
			r := base
			change(&r.Binding)
			parse(t, drillJSON(t, r), drillBindingInvalid)
		}
	})
	t.Run("state_unknown", func(t *testing.T) {
		parse(t, drillJSON(t, mutate(func(r *drillRecord) { r.State = "unknown" })), drillStateInvalid)
		parse(t, drillJSON(t, mutate(func(r *drillRecord) { r.Outcome = "unknown" })), drillStateInvalid)
	})
	t.Run("advance_order", func(t *testing.T) {
		for i, state := range states {
			for j, next := range states {
				r := at(state, "")
				got, err := drillAdvance(r, r.OperationID, next)
				want := drillTransitionRefused
				if j == i+1 {
					want = ""
				}
				check(t, err, want)
				drillAssert(t, want != "" || got.State == next, "drill advance state unchanged")
			}
		}
	})
	t.Run("advance_after_outcome", func(t *testing.T) {
		r := at("reserved", "failed")
		_, err := drillAdvance(r, r.OperationID, "copied")
		check(t, err, drillTransitionRefused)
	})
	t.Run("outcome_before_stop", func(t *testing.T) {
		for _, state := range states[:5] {
			r := at(state, "")
			_, err := drillFinish(r, r.OperationID, "passed")
			check(t, err, drillOutcomeBeforeStop)
			r.Outcome = "passed"
			parse(t, drillJSON(t, r), drillOutcomeBeforeStop)
		}
	})
	t.Run("stop_required", func(t *testing.T) {
		for _, state := range []string{"launched", "probed"} {
			r := at(state, "")
			_, err := drillFinish(r, r.OperationID, "failed")
			check(t, err, drillStopRequired)
			r.Outcome = "failed"
			parse(t, drillJSON(t, r), drillStopRequired)
		}
	})
	t.Run("outcome_once", func(t *testing.T) {
		for _, old := range []string{"passed", "failed"} {
			for _, next := range []string{"", "passed", "failed"} {
				r := at("stopped", old)
				_, err := drillFinish(r, r.OperationID, next)
				check(t, err, drillTransitionRefused)
			}
		}
	})
	t.Run("stale_resume", func(t *testing.T) {
		for i, state := range states[:5] {
			r := at(state, "")
			_, err := drillAdvance(r, "other_run", states[i+1])
			want := drillStaleResume
			if state == "probed" {
				want = ""
			}
			check(t, err, want)
		}
		r := at("stopped", "")
		_, err := drillFinish(r, "other_run", "passed")
		check(t, err, drillStaleResume)
		for _, state := range states {
			r := at(state, "")
			if state == "launched" || state == "probed" {
				var err error
				r, err = drillStop(r, "other_run")
				check(t, err, "")
				drillAssert(t, r.State == "stopped", "drill stop state unchanged")
			}
			_, err := drillFinish(r, "other_run", "failed")
			check(t, err, "")
		}
	})
	t.Run("resume_actions", func(t *testing.T) {
		validity := map[string][6]drillReason{"": {"", "", "", "", "", ""}, "passed": {drillOutcomeBeforeStop, drillOutcomeBeforeStop, drillOutcomeBeforeStop, drillOutcomeBeforeStop, drillOutcomeBeforeStop, ""}, "failed": {"", "", "", drillStopRequired, drillStopRequired, ""}}
		for index, state := range states {
			for _, outcome := range []string{"", "passed", "failed"} {
				for _, same := range []bool{false, true} {
					r := at(state, outcome)
					op := "other_run"
					if same {
						op = r.OperationID
					}
					got, err := drillResume(r, op)
					if reason := validity[outcome][index]; reason != "" {
						check(t, err, reason)
						continue
					}
					check(t, err, "")
					want := "record_failed"
					if outcome != "" {
						want = "none"
					} else if state == "launched" || state == "probed" {
						want = "stop_child"
					} else if same {
						want = "continue"
					}
					drillAssert(t, got == want, "drill resume action incorrect")
				}
			}
		}
	})
	t.Run("cleanup_due", func(t *testing.T) {
		for _, outcome := range []string{"", "passed", "failed"} {
			r := at("stopped", outcome)
			got, err := drillCleanup(r, false)
			check(t, err, "")
			drillAssert(t, !got.CleanupDue && got.Outcome == outcome, "drill cleanup altered outcome")
			_, err = drillCleanup(got, true)
			check(t, err, drillTransitionRefused)
			_, err = drillCleanup(got, false)
			check(t, err, "")
		}
	})
	t.Run("receipt_binding", func(t *testing.T) {
		r := at("stopped", "passed")
		receipt, err := drillMakeReceipt(r)
		check(t, err, "")
		for _, change := range []func(*drillReceipt){func(v *drillReceipt) { v.DrillID = "drill_bcdefghijklmnopq" }, func(v *drillReceipt) { v.Outcome = "failed" }, func(v *drillReceipt) { v.Binding.InstallationID = "installation_two" }, func(v *drillReceipt) { v.Binding.CheckpointID = "ckpt_bcdefghijklmnopq" }, func(v *drillReceipt) { v.Binding.InventorySHA256 = strings.Repeat("c", 64) }, func(v *drillReceipt) { v.Binding.LayoutVersion++ }, func(v *drillReceipt) { v.Binding.PrincipalLayoutVersion++ }, func(v *drillReceipt) { v.Binding.SchemaVersion++ }, func(v *drillReceipt) { v.Binding.PostgresMajor = "17" }} {
			v := receipt
			change(&v)
			check(t, drillCheckReceipt(v, r, r.Binding), drillReceiptBinding)
		}
	})
	t.Run("receipt_stale", func(t *testing.T) {
		r := at("stopped", "passed")
		receipt, err := drillMakeReceipt(r)
		check(t, err, "")
		for _, change := range []func(*drillBinding){func(b *drillBinding) { b.InstallationID = "installation_two" }, func(b *drillBinding) { b.CheckpointID = "ckpt_bcdefghijklmnopq" }, func(b *drillBinding) { b.InventorySHA256 = strings.Repeat("c", 64) }, func(b *drillBinding) { b.LayoutVersion++ }, func(b *drillBinding) { b.PrincipalLayoutVersion++ }, func(b *drillBinding) { b.SchemaVersion++ }, func(b *drillBinding) { b.PostgresMajor = "17" }} {
			present := r.Binding
			change(&present)
			check(t, drillCheckReceipt(receipt, r, present), drillReceiptStale)
		}
	})
	t.Run("receipt_requires_outcome", func(t *testing.T) { _, err := drillMakeReceipt(base); check(t, err, drillTransitionRefused) })
	t.Run("receipt_failed_valid", func(t *testing.T) {
		r := at("reserved", "failed")
		receipt, err := drillMakeReceipt(r)
		check(t, err, "")
		check(t, drillCheckReceipt(receipt, r, r.Binding), "")
		_, err = drillParseReceipt(drillJSON(t, receipt))
		check(t, err, "")
	})
	t.Run("no_claim_wording", func(t *testing.T) {
		for _, name := range []string{"drill_record.go", "drill_record_test.go"} {
			f, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
			drillAssert(t, err == nil, "drill wording source unreadable")
			ast.Inspect(f, func(n ast.Node) bool {
				word := ""
				switch v := n.(type) {
				case *ast.Ident:
					word = v.Name
				case *ast.BasicLit:
					if v.Kind == token.STRING {
						word, _ = strconv.Unquote(v.Value)
					}
				}
				if word == "verified" {
					return true
				}
				for _, bad := range []string{"res" + "tore", "rec" + "over", "ver" + "ified"} {
					if strings.Contains(strings.ToLower(word), bad) {
						t.Error("drill forbidden claim wording")
					}
				}
				return true
			})
		}
	})
	t.Run("no_mutation", func(t *testing.T) {
		for _, state := range states {
			r := at(state, "")
			before := r
			_, _ = drillAdvance(r, r.OperationID, "copied")
			_, _ = drillStop(r, "other_run")
			_, _ = drillFinish(r, r.OperationID, "passed")
			_, _ = drillCleanup(r, false)
			_, _ = drillResume(r, "other_run")
			_, _ = drillMakeReceipt(r)
			drillAssert(t, r == before, "drill input record was changed")
		}
	})
	t.Run("value_free_errors", func(t *testing.T) {
		r := base
		r.DrillID = "DISTINCTIVE_INPUT_ID!"
		_, err := drillParseRecord(drillJSON(t, r))
		drillAssert(t, err != nil, "drill reason unexpectedly nil")
		for _, private := range []string{r.DrillID, base.DrillID, base.Binding.InventorySHA256, base.OperationID} {
			drillAssert(t, !strings.Contains(err.Error(), private), "drill error disclosed input")
		}
		check(t, err, drillIdentityInvalid)
	})
}
func drillContract(sources map[string]string) string {
	for _, source := range sources {
		if strings.Contains(source, "go:"+"linkname") {
			return "linkname"
		}
	}
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	for name, source := range sources {
		f, err := parser.ParseFile(fset, name, source, 0)
		if err != nil {
			return "syntax"
		}
		files[name] = f
	}
	f := files["drill_record.go"]
	if f == nil {
		return "missing"
	}
	allowed := map[string]bool{"bytes": true, "encoding/json": true, "errors": true, "io": true, "strings": true}
	for _, imp := range f.Imports {
		name, _ := strconv.Unquote(imp.Path.Value)
		if !allowed[name] {
			return "imports"
		}
	}

	methods := 0
	for _, decl := range f.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == "init" {
			return "declarations"
		}
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv != nil {
			methods++
			recv, ok := fn.Recv.List[0].Type.(*ast.Ident)
			if fn.Name.Name != "Error" || !ok || recv.Name != "drillError" || len(fn.Recv.List) != 1 {
				return "declarations"
			}
		}
		if gen, ok := decl.(*ast.GenDecl); ok && gen.Tok == token.VAR {
			return "declarations"
		}
	}
	if methods != 1 {
		return "declarations"
	}
	for name, other := range files {
		if name == "drill_record.go" {
			continue
		}
		found := false
		ast.Inspect(other, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && f.Scope.Objects[id.Name] != nil {
				found = true
			}
			return true
		})
		if found {
			return "references"
		}
	}
	info := &types.Info{Uses: map[*ast.Ident]types.Object{}, Types: map[ast.Expr]types.TypeAndValue{}}
	config := types.Config{Importer: importer.Default()}
	if _, err := config.Check("drillcontract", fset, []*ast.File{f}, info); err != nil {
		return "types"
	}
	bad, statements := false, false
	ast.Inspect(f, func(n ast.Node) bool {
		switch n.(type) {
		case *ast.GoStmt:
			statements = true
		case *ast.DeferStmt:
			statements = true
		}
		call, ok := n.(*ast.CallExpr)
		if !ok || info.Types[call.Fun].IsType() {
			return true
		}
		var id *ast.Ident
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			id = fn
		case *ast.SelectorExpr:
			id = fn.Sel
		}
		obj := info.Uses[id]
		if obj == nil {
			bad = true
			return true
		}
		if _, ok := obj.(*types.Builtin); ok {
			bad = bad || obj.Name() != "len" && obj.Name() != "delete"
			return true
		}
		if obj.Pos() >= f.Pos() && obj.Pos() <= f.End() {
			if _, ok := obj.(*types.Func); !ok {
				bad = true
			}
			return true
		}
		if obj.Pkg() == nil || !allowed[obj.Pkg().Path()] {
			bad = true
		}
		return true
	})
	if statements {
		return "statements"
	}
	if bad {
		return "calls"
	}
	return ""
}
func TestDrillRecordSourceContract(t *testing.T) {
	entries, err := os.ReadDir(".")
	drillAssert(t, err == nil, "drill contract directory unreadable")
	sources := map[string]string{}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		drillAssert(t, err == nil, "drill contract source unreadable")
		sources[name] = strings.ReplaceAll(string(b), "\r\n", "\n")
	}
	drillAssert(t, drillContract(sources) == "", "drill source contract rejected")
	for _, row := range []struct {
		name, want string
		change     func(map[string]string)
	}{
		{"imports", "imports", func(s map[string]string) {
			s["drill_record.go"] = strings.Replace(s["drill_record.go"], "import (", "import (\n_ \"os\"", 1)
		}},
		{"references", "references", func(s map[string]string) {
			s["drill_probe.go"] = "package localcell; func drillProbe() { _ = drillFormat }"
		}},
		{"method", "declarations", func(s map[string]string) { s["drill_record.go"] += "\nfunc (drillRecord) drillProbe() {}" }},
		{"pointer_receiver", "declarations", func(s map[string]string) {
			s["drill_record.go"] = strings.Replace(s["drill_record.go"], "(drillError) Error", "(*drillError) Error", 1)
		}},
		{"variable", "declarations", func(s map[string]string) { s["drill_record.go"] += "\nvar drillProbe = 1" }},
		{"operation_reference", "types", func(s map[string]string) {
			s["drill_record.go"] += "\nfunc drillProbe() { _ = validUpdateOperationID }"
		}},
		{"checkpoint_reference", "types", func(s map[string]string) { s["drill_record.go"] += "\nfunc drillProbe() { _ = checkpointIDPattern }" }},
		{"external_call", "types", func(s map[string]string) {
			s["drill_external.go"] = "package localcell; func drillOutsideProbe() {}"
			s["drill_record.go"] += "\nfunc drillProbe() { drillOutsideProbe() }"
		}},
		{"calls", "imports", func(s map[string]string) {
			s["drill_record.go"] = strings.Replace(s["drill_record.go"], "import (", "import (\n\"fmt\"", 1) + "\nfunc drillProbe() { _ = fmt.Sprint(1) }"
		}},
		{"linkname_new_file", "linkname", func(s map[string]string) {
			s["drill_probe.go"] = "package localcell\nimport _ \"unsafe\"\n//go:" + "linkname drillProbe github.com/witwave-ai/witself/internal/localcell.drillStateIndex\nfunc drillProbe(string) int\n"
		}},
		{"linkname_in_file", "linkname", func(s map[string]string) { s["drill_record.go"] += "\n//go:" + "linkname drillStateIndex\n" }},
		{"init", "declarations", func(s map[string]string) { s["drill_record.go"] += "\nfunc init() {}" }},
		{"goroutine", "statements", func(s map[string]string) {
			s["drill_record.go"] += "\nfunc drillProbe() { go drillOther() }; func drillOther() {}"
		}},
		{"deferred", "statements", func(s map[string]string) {
			s["drill_record.go"] += "\nfunc drillProbe() { defer drillOther() }; func drillOther() {}"
		}},
		{"builtin_print", "calls", func(s map[string]string) { s["drill_record.go"] += "\nfunc drillProbe() { print() }" }},
		{"builtin_println", "calls", func(s map[string]string) { s["drill_record.go"] += "\nfunc drillProbe() { println() }" }},
		{"builtin_panic", "calls", func(s map[string]string) { s["drill_record.go"] += "\nfunc drillProbe() { panic(\"x\") }" }},
		{"builtin_rec" + "over", "calls", func(s map[string]string) { s["drill_record.go"] += "\nfunc drillProbe() { _ = rec" + "over() }" }},
	} {
		t.Run(row.name, func(t *testing.T) {
			copySources := map[string]string{}
			for k, v := range sources {
				copySources[k] = v
			}
			row.change(copySources)
			for name, ending := range map[string]string{"lf": "\n", "crlf": "\r\n"} {
				t.Run(name, func(t *testing.T) {
					seed := map[string]string{}
					for k, v := range copySources {
						seed[k] = strings.ReplaceAll(v, "\n", ending)
					}
					drillAssert(t, drillContract(seed) == row.want, "drill contract mutation survived")
					if row.want == "linkname" {
						for k, v := range seed {
							seed[k] = strings.ReplaceAll(v, "go:"+"linkname", "ordinary comment")
						}
						drillAssert(t, drillContract(seed) == "", "drill contract control rejected")
					}
				})
			}
		})
	}
}
