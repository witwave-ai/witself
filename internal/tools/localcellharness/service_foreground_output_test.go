package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func foregroundOutputLines(captured []byte) []string {
	if len(captured) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(captured), "\n"), "\n")
}

// Valid only after the foreground fixture's completed stopped commands: desired
// and durable state stopped, clean journal, no pending startup or maintenance.
// service_supervisor.go:324-350 retains tick on cancellation; :504-517 returns
// before start/health and skips stop when running is false. Only the clock hook
// at :464 runs, without an operation label. Startup calls are excluded by :328.
// TestForegroundStoppedReceiptProofContract ties this set to the portable proof.
var stoppedForegroundShutdownReceipts = []string{
	"service_shutdown phase=idle operation=none journal_pending=false journal_read_ok=true\n",
	"service_shutdown phase=tick operation=none journal_pending=false journal_read_ok=true\n",
}

// An abandoned readiness probe can disconnect between named-pipe connection
// and the Windows peer check. Accept that diagnostic family and at most one
// closed lease-publication diagnostic; resource/connection setup failures and
// host failure receipts still fail this test.
// The receipt itself remains unique, last, newline-terminated, and byte-exact.
func foregroundShutdownFailure(captured []byte) string {
	if len(captured) == 0 {
		return "foreground shutdown receipt missing"
	}
	if !bytes.HasSuffix(captured, []byte("\n")) {
		return "foreground shutdown receipt lacks final newline or has trailing bytes"
	}
	lines := foregroundOutputLines(captured)
	leasePublishSeen := false
	for _, line := range lines[:len(lines)-1] {
		if regexp.MustCompile(`^native service host lease_publish class=(sharing_violation|access_denied|validation|other) attempts=[1-9][0-9]* elapsed_ms=[0-9]+$`).MatchString(line) {
			if leasePublishSeen {
				return "foreground shutdown emitted duplicate lease publication diagnostics"
			}
			leasePublishSeen = true
			continue
		}
		if !regexp.MustCompile(`^service_control accept=rejected step=(peer:pid|peer:identity|peer:sid)$`).MatchString(line) {
			return "foreground shutdown emitted unexpected output before receipt"
		}
	}
	receipt := lines[len(lines)-1] + "\n"
	if !regexp.MustCompile(`^service_shutdown phase=(startup|tick|control|idle) operation=(reconcile|repair|stop|start|health|none) journal_pending=(true|false) journal_read_ok=(true|false)\n$`).MatchString(receipt) {
		return "foreground shutdown receipt has invalid grammar or extra bytes"
	}
	if !slices.Contains(stoppedForegroundShutdownReceipts, receipt) {
		return "foreground shutdown receipt has unexpected field values"
	}
	return ""
}

func TestForegroundOutputLines(t *testing.T) {
	for _, test := range []struct {
		name, input string
		want        []string
	}{
		{"empty", "", nil},
		{"one", "service_manager_unavailable\n", []string{"service_manager_unavailable"}},
		{"all_after_rejected", "unrecognized\nservice_manager_unavailable\n", []string{"unrecognized", "service_manager_unavailable"}},
		{"blank_and_partial", "\nservice_manager_unavailable\npartial", []string{"", "service_manager_unavailable", "partial"}},
		{"preserve_cr", "service_manager_unavailable\r\n", []string{"service_manager_unavailable\r"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if !slices.Equal(foregroundOutputLines([]byte(test.input)), test.want) {
				t.Fatal("foreground output scan lost, normalized, or added a line")
			}
		})
	}
}

func TestForegroundShutdownOutput(t *testing.T) {
	const receipt = "service_shutdown phase=idle operation=none journal_pending=false journal_read_ok=true\n"
	for _, test := range []struct {
		name, output string
		accepted     bool
	}{
		{"receipt_only", receipt, true},
		{"lease_publish_sharing", "native service host lease_publish class=sharing_violation attempts=12 elapsed_ms=2175\n" + receipt, true},
		{"lease_publish_access", "native service host lease_publish class=access_denied attempts=2 elapsed_ms=25\n" + receipt, true},
		{"lease_publish_validation", "native service host lease_publish class=validation attempts=1 elapsed_ms=0\n" + receipt, true},
		{"lease_publish_other", "native service host lease_publish class=other attempts=1 elapsed_ms=0\n" + receipt, true},
		{"lease_publish_unknown_class", "native service host lease_publish class=none attempts=1 elapsed_ms=0\n" + receipt, false},
		{"lease_publish_duplicate", strings.Repeat("native service host lease_publish class=other attempts=1 elapsed_ms=0\n", 2) + receipt, false},
		{"abandoned_peer_pid", "service_control accept=rejected step=peer:pid\n" + receipt, true},
		{"abandoned_peer_identity", "service_control accept=rejected step=peer:identity\n" + receipt, true},
		{"abandoned_peer_sid", "service_control accept=rejected step=peer:sid\n" + receipt, true},
		{"repeated_abandoned_probes", strings.Repeat("service_control accept=rejected step=peer:pid\n", 3) + receipt, true},
		{"missing", "", false},
		{"only_diagnostic", "service_control accept=rejected step=peer:pid\n", false},
		{"duplicate_receipt", receipt + receipt, false},
		{"receipt_not_last", receipt + "service_control accept=rejected step=peer:pid\n", false},
		{"missing_final_newline", strings.TrimSuffix(receipt, "\n"), false},
		{"trailing_bytes", receipt + "partial", false},
		{"blank_before", "\n" + receipt, false},
		{"blank_after", receipt + "\n", false},
		{"private_before", "private /synthetic/home\n" + receipt, false},
		{"native_failure", "native service host site=supervisor_command\n" + receipt, false},
		{"duplicate_handle_failure", "service_control accept=rejected step=duplicate\n" + receipt, false},
		{"connection_setup_failure", "service_control accept=rejected step=conn\n" + receipt, false},
		{"unflushed_stop", "service_control action=stop response=unflushed\n" + receipt, false},
		{"unknown_peer", "service_control accept=rejected step=peer:other\n" + receipt, false},
		{"peer_extra_bytes", "service_control accept=rejected step=peer:pid extra\n" + receipt, false},
		{"receipt_extra_bytes", "extra " + receipt, false},
		{"interrupted_tick", strings.Replace(receipt, "phase=idle", "phase=tick", 1), true},
		{"interrupted_tick_after_peer", "service_control accept=rejected step=peer:pid\n" + strings.Replace(receipt, "phase=idle", "phase=tick", 1), true},
		{"unexpected_phase", strings.Replace(receipt, "phase=idle", "phase=control", 1), false},
		{"missing_field", strings.Replace(receipt, " journal_pending=false", "", 1), false},
		{"crlf_receipt", strings.ReplaceAll(receipt, "\n", "\r\n"), false},
		{"crlf_peer", "service_control accept=rejected step=peer:pid\r\n" + receipt, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if (foregroundShutdownFailure([]byte(test.output)) == "") != test.accepted {
				t.Fatal("foreground shutdown output violated closed diagnostic prefixes and one exact final receipt")
			}
		})
	}
}

// Exhaust the existing grammar independently of the acceptance table. A new
// operation, phase, pending journal or failed read must not broaden the set.
func TestForegroundStoppedReceiptClosedSet(t *testing.T) {
	for _, phase := range []string{"startup", "tick", "control", "idle"} {
		for _, operation := range []string{"reconcile", "repair", "stop", "start", "health", "none"} {
			for _, pending := range []bool{false, true} {
				for _, readOK := range []bool{false, true} {
					name := fmt.Sprintf("%s_%s_pending=%t_read=%t", phase, operation, pending, readOK)
					t.Run(name, func(t *testing.T) {
						receipt := fmt.Sprintf("service_shutdown phase=%s operation=%s journal_pending=%t journal_read_ok=%t\n", phase, operation, pending, readOK)
						want := (phase == "idle" || phase == "tick") && operation == "none" && !pending && readOK
						failure := foregroundShutdownFailure([]byte(receipt))
						if (failure == "") != want {
							t.Fatal("stopped foreground receipt escaped its closed field set")
						}
						if !want && failure != "foreground shutdown receipt has unexpected field values" {
							t.Fatal("wrong receipt fields lost their fixed value-free rejection")
						}
					})
				}
			}
		}
	}
	t.Run("health_site", func(t *testing.T) {
		if foregroundShutdownFailure([]byte("service_shutdown phase=tick operation=health site=health_lock journal_pending=false journal_read_ok=true\n")) == "" {
			t.Fatal("stopped foreground accepted an unreachable health site")
		}
	})
}

// Follow the existing AST source-contract style: read the other package's
// literal proof rows without exporting test helpers into production. Parse both
// line endings and compare the exact set, including journal fields and newline.
func TestForegroundStoppedReceiptProofContract(t *testing.T) {
	source, err := os.ReadFile("../../localcell/service_stopped_tick_receipt_test.go")
	if err != nil {
		t.Fatal("cannot read stopped tick receipt proof")
	}
	lf := strings.ReplaceAll(string(source), "\r\n", "\n")
	for _, row := range []struct{ name, source string }{
		{"lf", lf}, {"crlf", strings.ReplaceAll(lf, "\n", "\r\n")},
	} {
		t.Run(row.name, func(t *testing.T) {
			file, err := parser.ParseFile(token.NewFileSet(), "stopped-proof", row.source, 0)
			if err != nil {
				t.Fatal("cannot parse stopped tick receipt proof")
			}
			var expected []string
			found := 0
			ast.Inspect(file, func(node ast.Node) bool {
				declaration, ok := node.(*ast.ValueSpec)
				if !ok || len(declaration.Names) != 1 || declaration.Names[0].Name != "stoppedTickReceiptCases" {
					return true
				}
				found++
				if len(declaration.Values) != 1 {
					t.Fatal("stopped tick proof lost its literal case table")
				}
				table, ok := declaration.Values[0].(*ast.CompositeLit)
				if !ok || len(table.Elts) == 0 {
					t.Fatal("stopped tick proof lost its literal case rows")
				}
				for _, element := range table.Elts {
					entry, ok := element.(*ast.CompositeLit)
					if !ok || len(entry.Elts) != 4 {
						t.Fatal("stopped tick proof case shape changed")
					}
					values := make([]string, 4)
					for index, field := range entry.Elts {
						literal, ok := field.(*ast.BasicLit)
						if !ok || literal.Kind != token.STRING {
							t.Fatal("stopped tick proof labels must remain literal")
						}
						values[index], err = strconv.Unquote(literal.Value)
						if err != nil {
							t.Fatal("cannot decode stopped tick proof label")
						}
					}
					expected = append(expected, "service_shutdown phase="+values[2]+" operation="+values[3]+" journal_pending=false journal_read_ok=true\n")
				}
				return false
			})
			slices.Sort(expected)
			expected = slices.Compact(expected)
			actual := slices.Clone(stoppedForegroundShutdownReceipts)
			slices.Sort(actual)
			if found != 1 || !slices.Equal(actual, expected) {
				t.Fatal("foreground acceptance drifted from the stopped tick cancellation proof")
			}
		})
	}
}

func TestWindowsForegroundShutdownOutputContract(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "service_subprocess_windows_test.go", nil, 0)
	if err != nil {
		t.Fatal("cannot parse Windows foreground output source")
	}
	fixture, err := parser.ParseFile(token.NewFileSet(), "contract", `package main; func contract() {
		receipt := captured.Bytes()
		if len(foregroundOutputLines(receipt)) > 1 {
			windowsLogForegroundExit(t, command, receipt)
		}
		if failure := foregroundShutdownFailure(receipt); failure != "" {
			if len(foregroundOutputLines(receipt)) <= 1 {
				windowsLogForegroundExit(t, command, receipt)
			}
			t.Fatal(failure)
		}
	}`, 0)
	if err != nil {
		t.Fatal("invalid foreground output contract")
	}
	expected := fixture.Decls[0].(*ast.FuncDecl).Body
	matches := 0
	ast.Inspect(file, func(node ast.Node) bool {
		block, ok := node.(*ast.BlockStmt)
		if !ok {
			return true
		}
		for index, statement := range block.List {
			if foregroundSource(t, statement) != "receipt := captured.Bytes()" {
				continue
			}
			matches++
			actual := &ast.BlockStmt{List: block.List[index:]}
			if foregroundSource(t, actual) != foregroundSource(t, expected) {
				t.Fatal("Windows foreground multiline output lost guarded logging before exact receipt validation")
			}
		}
		return true
	})
	if matches != 1 {
		t.Fatal("Windows foreground output contract lost its unique assertion site")
	}
}
