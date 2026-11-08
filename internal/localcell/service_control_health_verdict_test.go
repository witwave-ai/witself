package localcell

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

const serviceHealthVerdictFloor = time.Second

// The host timeout starts before the hook measures elapsed, so scheduling in that gap can look like an early cut.
const serviceHealthVerdictEarlyCutDivisor = 10

type serviceHealthVerdictObservation struct {
	verdict       error
	stops         int
	running       bool
	failures      int
	nextStartZero bool
	state         string
	elapsed       time.Duration
	hostBound     time.Duration
}

func serviceHealthVerdictFields(o serviceHealthVerdictObservation) []string {
	var fields []string
	if !errors.Is(o.verdict, errServiceUnhealthy) {
		fields = append(fields, "verdict")
	}
	if o.stops != 1 {
		fields = append(fields, "stops")
	}
	if o.running {
		fields = append(fields, "running")
	}
	if o.failures != 1 {
		fields = append(fields, "failures")
	}
	if o.nextStartZero {
		fields = append(fields, "next_start")
	}
	if o.state != "backoff" {
		fields = append(fields, "state")
	}
	if o.elapsed < o.hostBound-o.hostBound/serviceHealthVerdictEarlyCutDivisor {
		fields = append(fields, "elapsed")
	}
	return fields
}

func serviceHealthVerdictReport(fields []string, observed serviceHealthVerdictObservation) string {
	return fmt.Sprintf("host bound expiry failed fields=%s host_bound_ms=%d elapsed_ms=%d early_cut_ms=%d stops=%d failures=%d verdict_class=%s", strings.Join(fields, ","), observed.hostBound.Milliseconds(), observed.elapsed.Milliseconds(), max(0, observed.hostBound-observed.elapsed).Milliseconds(), observed.stops, observed.failures, serviceHealthVerdictClass(observed.verdict))
}

func serviceHealthVerdictClass(verdict error) string {
	switch {
	case errors.Is(verdict, errServiceUnhealthy):
		return "unhealthy"
	case errors.Is(verdict, errServiceControlDeferred):
		return "deferred"
	case verdict == nil:
		return "nil"
	default:
		return "other"
	}
}

func TestServiceHealthVerdictFields(t *testing.T) {
	valid := serviceHealthVerdictObservation{
		verdict: errServiceUnhealthy, stops: 1, failures: 1,
		state: "backoff", elapsed: time.Second, hostBound: time.Second,
	}
	if len(serviceHealthVerdictFields(valid)) != 0 {
		t.Fatal("complete host verdict reported a failed field")
	}
	for _, test := range []struct {
		name   string
		mutate func(*serviceHealthVerdictObservation)
	}{
		{"verdict", func(o *serviceHealthVerdictObservation) { o.verdict = nil }},
		{"stops", func(o *serviceHealthVerdictObservation) { o.stops = 0 }},
		{"running", func(o *serviceHealthVerdictObservation) { o.running = true }},
		{"failures", func(o *serviceHealthVerdictObservation) { o.failures = 0 }},
		{"next_start", func(o *serviceHealthVerdictObservation) { o.nextStartZero = true }},
		{"state", func(o *serviceHealthVerdictObservation) { o.state = "" }},
		{"elapsed", func(o *serviceHealthVerdictObservation) { o.elapsed = 899 * time.Millisecond }},
	} {
		t.Run(test.name, func(t *testing.T) {
			observation := valid
			test.mutate(&observation)
			fields := serviceHealthVerdictFields(observation)
			if !reflect.DeepEqual(fields, []string{test.name}) {
				t.Fatal("host verdict diagnostic lost an isolated failed field")
			}
			if !strings.HasPrefix(serviceHealthVerdictReport(fields, observation), "host bound expiry failed fields="+test.name+" ") {
				t.Fatal("host verdict report lost an isolated failed field")
			}
		})
	}
	for _, test := range []struct {
		name    string
		elapsed time.Duration
		want    []string
	}{
		{"tolerance_boundary", 900 * time.Millisecond, nil},
		{"below_tolerance_boundary", 900*time.Millisecond - time.Nanosecond, []string{"elapsed"}},
		{"inside_tolerance", 999 * time.Millisecond, nil},
		{"at_host_bound", time.Second, nil},
		{"after_host_bound", time.Second + time.Nanosecond, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			observation := valid
			observation.elapsed = test.elapsed
			if !reflect.DeepEqual(serviceHealthVerdictFields(observation), test.want) {
				t.Fatal("host verdict elapsed tolerance boundary changed")
			}
		})
	}
	invalid := serviceHealthVerdictObservation{running: true, nextStartZero: true, hostBound: time.Second}
	want := []string{"verdict", "stops", "running", "failures", "next_start", "state", "elapsed"}
	if !reflect.DeepEqual(serviceHealthVerdictFields(invalid), want) {
		t.Fatal("host verdict diagnostic lost the ordered closed field list")
	}
}

func TestServiceHealthVerdictClass(t *testing.T) {
	for _, test := range []struct {
		name    string
		verdict error
		want    string
	}{
		{"unhealthy", errServiceUnhealthy, "unhealthy"},
		{"deferred", errServiceControlDeferred, "deferred"},
		{"nil", nil, "nil"},
		{"other", context.Canceled, "other"},
		{"wrapped_unhealthy", errors.Join(context.Canceled, errServiceUnhealthy), "unhealthy"},
		{"wrapped_deferred", errors.Join(context.Canceled, errServiceControlDeferred), "deferred"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if serviceHealthVerdictClass(test.verdict) != test.want {
				t.Fatal("host verdict class escaped its closed classification")
			}
		})
	}
}

func TestServiceHealthVerdictSourceContract(t *testing.T) {
	health, verdict := serviceHealthVerdictContractSources(t)
	if serviceHealthVerdictFloor != time.Second {
		t.Fatal("host verdict floor is not one second")
	}
	if serviceHealthVerdictEarlyCutDivisor != 10 {
		t.Fatal("host verdict early cut divisor is not ten")
	}
	if reason := serviceHealthVerdictSourceContract(health, verdict); reason != "" {
		t.Fatal(reason)
	}
	if reason := serviceHealthVerdictSourceContract(bytes.ReplaceAll(health, []byte("\n"), []byte("\r\n")), bytes.ReplaceAll(verdict, []byte("\n"), []byte("\r\n"))); reason != "" {
		t.Fatal("host verdict contract rejected CRLF source")
	}
}

func TestServiceHealthVerdictSourceContractMutations(t *testing.T) {
	health, verdict := serviceHealthVerdictContractSources(t)
	for _, test := range []struct {
		name   string
		helper bool
		before string
		after  string
	}{
		{"floor_literal", false, "floor := serviceHealthVerdictFloor", "floor := 100 * time.Millisecond"},
		{"half_second_constant", true, "const serviceHealthVerdictFloor = time.Second", "const serviceHealthVerdictFloor = 500 * time.Millisecond"},
		{"old_constant", true, "const serviceHealthVerdictFloor = time.Second", "const serviceHealthVerdictFloor = 100 * time.Millisecond"},
		{"missing_state_name", true, `fields = append(fields, "state")`, `fields = append(fields, "")`},
		{"missing_state_check", true, "\tif o.state != \"backoff\" {\n\t\tfields = append(fields, \"state\")\n\t}\n", ""},
		{"old_fatal", false, `t.Error(serviceHealthVerdictReport(fields, observed))`, `t.Error("host bound expiry lost unhealthy verdict, stop, or backoff")`},
		{"raw_error", true, "serviceHealthVerdictClass(observed.verdict))", "observed.verdict.Error())"},
		{"caller_floor", false, "floor = 2 * time.Second", "floor = time.Second"},
		{"tolerance_removed", true, "o.elapsed < o.hostBound-o.hostBound/serviceHealthVerdictEarlyCutDivisor", "o.elapsed < o.hostBound"},
		{"half_bound_tolerance", true, "const serviceHealthVerdictEarlyCutDivisor = 10", "const serviceHealthVerdictEarlyCutDivisor = 2"},
		{"wrong_side_tolerance", true, "o.elapsed < o.hostBound-o.hostBound/serviceHealthVerdictEarlyCutDivisor", "o.elapsed < o.hostBound+o.hostBound/serviceHealthVerdictEarlyCutDivisor"},
		{"missing_early_cut", true, "elapsed_ms=%d early_cut_ms=%d", "elapsed_ms=%d"},
		{"filtered_state_after_collection", false, "fields := serviceHealthVerdictFields(observed)\n", "fields := serviceHealthVerdictFields(observed)\nfor index, field := range fields {\nif field == \"state\" {\nfields = append(fields[:index], fields[index+1:]...)\nbreak\n}\n}\n"},
		{"resliced_fields_after_collection", false, "fields := serviceHealthVerdictFields(observed)\n", "fields := serviceHealthVerdictFields(observed)\nfields = fields[1:]\n"},
		{"host_floor_reassigned", false, "hostBound := serviceHealthMeasuredBound(t, floor)", "if !tighter {\nfloor = 100 * time.Millisecond\n}\nhostBound := serviceHealthMeasuredBound(t, floor)"},
		{"literal_calibration_floor", false, "hostBound := serviceHealthMeasuredBound(t, floor)", "hostBound := serviceHealthMeasuredBound(t, 100*time.Millisecond)"},
	} {
		t.Run(test.name, func(t *testing.T) {
			healthCopy, verdictCopy := string(health), string(verdict)
			target := &healthCopy
			if test.helper {
				target = &verdictCopy
			}
			mutated := strings.Replace(*target, test.before, test.after, 1)
			if mutated == *target {
				t.Fatal("host verdict source mutation did not apply")
			}
			*target = mutated
			if serviceHealthVerdictSourceContract([]byte(healthCopy), []byte(verdictCopy)) == "" {
				t.Fatal("host verdict source contract accepted a mutation")
			}
			if serviceHealthVerdictSourceContract(bytes.ReplaceAll([]byte(healthCopy), []byte("\n"), []byte("\r\n")), bytes.ReplaceAll([]byte(verdictCopy), []byte("\n"), []byte("\r\n"))) == "" {
				t.Fatal("host verdict source contract accepted a CRLF mutation")
			}
		})
	}
}

func serviceHealthVerdictContractSources(t *testing.T) ([]byte, []byte) {
	t.Helper()
	health, healthErr := os.ReadFile("service_control_health_test.go")
	verdict, verdictErr := os.ReadFile("service_control_health_verdict_test.go")
	if healthErr != nil || verdictErr != nil {
		t.Fatal("cannot read host verdict contract sources")
	}
	// Normalize before source mutations as well as parsing for Windows checkouts.
	return bytes.ReplaceAll(health, []byte("\r\n"), []byte("\n")), bytes.ReplaceAll(verdict, []byte("\r\n"), []byte("\n"))
}

func serviceHealthVerdictSourceContract(healthSource, verdictSource []byte) string {
	healthSource = bytes.ReplaceAll(healthSource, []byte("\r\n"), []byte("\n"))
	verdictSource = bytes.ReplaceAll(verdictSource, []byte("\r\n"), []byte("\n"))
	positions := token.NewFileSet()
	health, healthErr := parser.ParseFile(positions, "health.go", healthSource, 0)
	verdict, verdictErr := parser.ParseFile(positions, "verdict.go", verdictSource, 0)
	if healthErr != nil || verdictErr != nil {
		return "host verdict source does not parse"
	}
	nodeText := func(node ast.Node) string {
		var text bytes.Buffer
		if format.Node(&text, positions, node) != nil {
			return ""
		}
		return text.String()
	}
	function := func(file *ast.File, name string) *ast.FuncDecl {
		for _, declaration := range file.Decls {
			if candidate, ok := declaration.(*ast.FuncDecl); ok && candidate.Name.Name == name {
				return candidate
			}
		}
		return nil
	}
	row := function(health, "TestServiceControlCallerBoundYieldsActualHost")
	collector := function(verdict, "serviceHealthVerdictFields")
	classifier := function(verdict, "serviceHealthVerdictClass")
	reporter := function(verdict, "serviceHealthVerdictReport")
	if row == nil || collector == nil || classifier == nil || reporter == nil {
		return "host verdict row or diagnostic helper is missing"
	}
	constantOK, divisorOK := false, false
	for _, declaration := range verdict.Decls {
		group, ok := declaration.(*ast.GenDecl)
		if !ok || group.Tok != token.CONST {
			continue
		}
		for _, spec := range group.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if ok && len(value.Names) == 1 && value.Names[0].Name == "serviceHealthVerdictFloor" && len(value.Values) == 1 {
				constantOK = nodeText(value.Values[0]) == "time.Second"
			}
			if ok && len(value.Names) == 1 && value.Names[0].Name == "serviceHealthVerdictEarlyCutDivisor" && len(value.Values) == 1 {
				divisorOK = nodeText(value.Values[0]) == "10"
			}
		}
	}
	if !constantOK {
		return "host verdict floor constant is not one second"
	}
	if !divisorOK {
		return "host verdict early cut divisor is not ten"
	}
	rowNodes := make(map[string]bool)
	var host *ast.BlockStmt
	hostBranches := 0
	ast.Inspect(row.Body, func(node ast.Node) bool {
		if node == nil {
			return true
		}
		rowNodes[nodeText(node)] = true
		if conditional, ok := node.(*ast.IfStmt); ok && nodeText(conditional.Cond) == "tighter" && conditional.Else != nil {
			host, _ = conditional.Else.(*ast.BlockStmt)
			hostBranches++
		}
		return true
	})
	if reason := serviceHealthVerdictFloorFlow(row, nodeText); reason != "" {
		return reason
	}
	if !rowNodes["context.WithTimeout(t.Context(), 8*max(hostBound, callerBound))"] {
		return "host verdict outer context changed"
	}
	if host == nil || hostBranches != 1 {
		return "host verdict diagnostic branch is missing"
	}
	conditions := []struct{ predicate, field string }{
		{"!errors.Is(o.verdict, errServiceUnhealthy)", "verdict"},
		{"o.stops != 1", "stops"},
		{"o.running", "running"},
		{"o.failures != 1", "failures"},
		{"o.nextStartZero", "next_start"},
		{`o.state != "backoff"`, "state"},
		{"o.elapsed < o.hostBound-o.hostBound/serviceHealthVerdictEarlyCutDivisor", "elapsed"},
	}
	if len(collector.Body.List) != len(conditions)+2 || nodeText(collector.Body.List[0]) != "var fields []string" || nodeText(collector.Body.List[len(collector.Body.List)-1]) != "return fields" {
		return "host verdict diagnostic lost its seven field checks"
	}
	for index, condition := range conditions {
		check, ok := collector.Body.List[index+1].(*ast.IfStmt)
		if !ok || check.Init != nil || check.Else != nil || nodeText(check.Cond) != condition.predicate || len(check.Body.List) != 1 || nodeText(check.Body.List[0]) != "fields = append(fields, "+strconv.Quote(condition.field)+")" {
			return "host verdict diagnostic predicate or closed field changed"
		}
	}
	// The formatter is deliberately a single expression: no filtering or writes
	// can intervene between its supplied fields and the exact closed diagnostic.
	const diagnostic = `return fmt.Sprintf("host bound expiry failed fields=%s host_bound_ms=%d elapsed_ms=%d early_cut_ms=%d stops=%d failures=%d verdict_class=%s", strings.Join(fields, ","), observed.hostBound.Milliseconds(), observed.elapsed.Milliseconds(), max(0, observed.hostBound-observed.elapsed).Milliseconds(), observed.stops, observed.failures, serviceHealthVerdictClass(observed.verdict))`
	if len(reporter.Body.List) != 1 || nodeText(reporter.Body.List[0]) != diagnostic {
		return "host verdict diagnostic format or safe arguments changed"
	}
	// Match direct statements, in order, instead of finding expressions anywhere
	// in the AST. Adjacency rejects even nested writes/redeclarations of fields
	// between collection and reporting; comments were stripped during parsing.
	hostAnchors := []string{
		`observed := serviceHealthVerdictObservation{
	verdict: verdict, stops: *stops, running: s.running,
	failures: len(s.record.Failures), nextStartZero: s.record.NextStart.IsZero(),
	state: s.status.State, elapsed: elapsed, hostBound: hostBound,
}`,
		"fields := serviceHealthVerdictFields(observed)",
		`if len(fields) > 0 {
	t.Error(serviceHealthVerdictReport(fields, observed))
} else {
	observed.state = ""
	if fields := serviceHealthVerdictFields(observed); !reflect.DeepEqual(fields, []string{"state"}) || !strings.HasPrefix(serviceHealthVerdictReport(fields, observed), "host bound expiry failed fields=state ") {
		t.Error("host bound diagnostic lost state check")
	}
}`,
	}
	if len(host.List) != len(hostAnchors) || !serviceHealthVerdictStatementAnchors(host.List, hostAnchors, nodeText) {
		return "host verdict collection-to-report flow changed"
	}
	// Pin classification to predicates and literal returns, never error text.
	if len(classifier.Body.List) != 1 {
		return "host verdict class is not closed"
	}
	classSwitch, ok := classifier.Body.List[0].(*ast.SwitchStmt)
	if !ok || classSwitch.Init != nil || classSwitch.Tag != nil || len(classSwitch.Body.List) != 4 {
		return "host verdict class is not closed"
	}
	for index, expected := range []struct{ condition, class string }{
		{"errors.Is(verdict, errServiceUnhealthy)", "unhealthy"},
		{"errors.Is(verdict, errServiceControlDeferred)", "deferred"},
		{"verdict == nil", "nil"},
		{"", "other"},
	} {
		clause, ok := classSwitch.Body.List[index].(*ast.CaseClause)
		if !ok || len(clause.Body) != 1 || nodeText(clause.Body[0]) != "return "+strconv.Quote(expected.class) {
			return "host verdict class is not closed"
		}
		if expected.condition == "" {
			if len(clause.List) != 0 {
				return "host verdict class is not closed"
			}
		} else if len(clause.List) != 1 || nodeText(clause.List[0]) != expected.condition {
			return "host verdict class is not closed"
		}
	}
	return ""
}

// Pin the actual t.Run body, not a matching nested block or a decoy expression.
// The four anchors form its complete prefix, so no second duration or write to
// floor can intervene between initialization, caller override and calibration.
func serviceHealthVerdictFloorFlow(row *ast.FuncDecl, nodeText func(ast.Node) string) string {
	var body *ast.BlockStmt
	rows := 0
	ast.Inspect(row.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || nodeText(call.Fun) != "t.Run" || len(call.Args) != 2 || nodeText(call.Args[0]) != "name" {
			return true
		}
		if callback, ok := call.Args[1].(*ast.FuncLit); ok {
			body = callback.Body
			rows++
		}
		return false
	})
	floorAnchors := []string{
		"floor := serviceHealthVerdictFloor",
		"if tighter {\n\tfloor = 2 * time.Second\n}",
		"hostBound := serviceHealthMeasuredBound(t, floor)",
		`if hostBound < serviceHealthVerdictFloor {
	t.Fatalf("host bound below floor host_bound_ms=%d floor_ms=%d", hostBound.Milliseconds(), serviceHealthVerdictFloor.Milliseconds())
}`,
	}
	if rows != 1 || body == nil || !serviceHealthVerdictStatementAnchors(body.List, floorAnchors, nodeText) {
		return "host verdict floor-to-calibration flow changed"
	}
	return ""
}

func serviceHealthVerdictStatementAnchors(statements []ast.Stmt, anchors []string, nodeText func(ast.Node) string) bool {
	if len(statements) < len(anchors) {
		return false
	}
	for index, anchor := range anchors {
		// Formatting can retain a blank separator where a stripped comment was.
		if strings.ReplaceAll(nodeText(statements[index]), "\n\n", "\n") != anchor {
			return false
		}
	}
	return true
}

func TestServiceHealthVerdictFloorSourceContract(t *testing.T) {
	health, _ := serviceHealthVerdictContractSources(t)
	for _, source := range [][]byte{health, bytes.ReplaceAll(health, []byte("\n"), []byte("\r\n"))} {
		positions := token.NewFileSet()
		file, err := parser.ParseFile(positions, "health.go", bytes.ReplaceAll(source, []byte("\r\n"), []byte("\n")), 0)
		if err != nil {
			t.Fatal("host verdict floor source does not parse")
		}
		found := false
		for _, declaration := range file.Decls {
			row, ok := declaration.(*ast.FuncDecl)
			if !ok || row.Name.Name != "TestServiceControlCallerBoundYieldsActualHost" {
				continue
			}
			found = true
			if reason := serviceHealthVerdictFloorFlow(row, func(node ast.Node) string {
				var text bytes.Buffer
				if format.Node(&text, positions, node) != nil {
					return ""
				}
				return text.String()
			}); reason != "" {
				t.Fatal(reason)
			}
		}
		if !found {
			t.Fatal("host verdict floor row is missing")
		}
	}
}
