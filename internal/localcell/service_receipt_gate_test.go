//go:build !windows

package localcell_test

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/localcell"
)

// The owner-only driver binds real I/O to this default-build, hermetic contract.
// No lifecycle runs merely because this file is built or its tests are selected.
type ownerReceiptPhase1 struct {
	BootID          string    `json:"boot_id"`
	ServerPID       int       `json:"server_pid"`
	ServerStartTime string    `json:"server_start_time"`
	OperatorID      string    `json:"operator_id"`
	RegisteredAt    time.Time `json:"registered_at"`
}

type ownerReceiptPhase2 struct {
	BootIDChanged            bool      `json:"boot_id_changed"`
	AutomaticReadyAfterLogin bool      `json:"automatic_ready_after_login"`
	FreshProcessIdentity     bool      `json:"fresh_process_identity"`
	WriteSurvived            bool      `json:"write_survived"`
	RemovedRetainingData     bool      `json:"removed_retaining_data"`
	StartedAt                time.Time `json:"started_at"`
	CompletedAt              time.Time `json:"completed_at"`
}

type ownerReceiptOperator struct {
	ID   string `json:"id"`
	Name string `json:"display_name"`
}

type ownerReceiptProof struct{ executed, passed bool }

var ownerReceiptProofNames = [...]string{
	"boot_id_changed", "automatic_ready_after_login", "fresh_process_identity",
	"write_survived", "removed_retaining_data",
}

type ownerReceiptOperations struct {
	boot             func() string
	process          func() localcell.ProcessRecord
	operators        func() []ownerReceiptOperator
	captureProcesses func() bool
	operation        func(string) bool
	inventory        func() (map[string]ownerReceiptEntry, error)
	retained         func() bool
	deleteCredential func() error
	publish          func([]byte) error
}

// All phase-2 control/harness operations cross this boundary. Attempted forbidden
// actions invalidate the receipt even if a caller ignores the refused result.
type ownerReceiptActions struct {
	ops       *ownerReceiptOperations
	completed int
	trace     []string
	refused   bool
}

func (a *ownerReceiptActions) call(action string) bool {
	a.trace = append(a.trace, action)
	allowed := action == "observe" && a.completed == 1 ||
		(action == "stop" || action == "remove") && a.completed == 4
	if !allowed {
		a.refused = true
		return false
	}
	return a.ops.operation(action)
}

func ownerReceiptValidate(record ownerReceiptPhase2, proofs [5]ownerReceiptProof, actions *ownerReceiptActions) error {
	values := [...]bool{record.BootIDChanged, record.AutomaticReadyAfterLogin, record.FreshProcessIdentity, record.WriteSurvived, record.RemovedRetainingData}
	for i, value := range values {
		if !proofs[i].executed || !proofs[i].passed || !value {
			return errors.New(ownerReceiptProofNames[i])
		}
	}
	if actions.refused || !slices.Equal(actions.trace, []string{"observe", "stop", "remove"}) {
		return errors.New("receipt_operation_refused")
	}
	return nil
}

func ownerReceiptRunPhase2(before ownerReceiptPhase1, ops ownerReceiptOperations) (ownerReceiptPhase2, error) {
	record := ownerReceiptPhase2{StartedAt: time.Now().UTC()}
	actions := &ownerReceiptActions{ops: &ops}
	var proofs [5]ownerReceiptProof
	var boot string
	checks := []struct {
		result *bool
		proof  func() bool
	}{
		{&record.BootIDChanged, func() bool {
			boot = ops.boot()
			return boot != "" && boot != before.BootID
		}},
		{&record.AutomaticReadyAfterLogin, func() bool { return actions.call("observe") }},
		{&record.FreshProcessIdentity, func() bool {
			after := ops.process()
			return after.PID > 1 && strings.HasPrefix(after.StartTime, "linux:"+boot+":") &&
				(after.PID != before.ServerPID || after.StartTime != before.ServerStartTime)
		}},
		{&record.WriteSurvived, func() bool {
			for _, operator := range ops.operators() {
				if operator.ID == before.OperatorID && operator.Name == lifecycleUnicodeName {
					return true
				}
			}
			return false
		}},
		{&record.RemovedRetainingData, func() bool {
			if !ops.captureProcesses() || !actions.call("stop") {
				return false
			}
			stopped, err := ops.inventory()
			if err != nil || len(stopped) == 0 || !actions.call("remove") {
				return false
			}
			retained, err := ops.inventory()
			return err == nil && ownerReceiptInventoryEqual(stopped, retained) && ops.retained()
		}},
	}
	for i, check := range checks {
		// Direct calls under the parent cannot be filtered as test subtests.
		proofs[i].executed = true
		proofs[i].passed = check.proof()
		*check.result = proofs[i].passed
		if !proofs[i].passed {
			record.CompletedAt = time.Now().UTC()
			return record, errors.New(ownerReceiptProofNames[i])
		}
		actions.completed++
	}
	record.CompletedAt = time.Now().UTC()
	// Validate the stored values, actual execution and operation trace immediately
	// before either irreversible credential deletion or success publication.
	if err := ownerReceiptValidate(record, proofs, actions); err != nil {
		return record, err
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return record, errors.New("receipt_encode_failed")
	}
	if ops.deleteCredential() != nil {
		return record, errors.New("receipt_credential_delete_failed")
	}
	if ops.publish(append(data, '\n')) != nil {
		return record, errors.New("receipt_publish_failed")
	}
	return record, nil
}

func ownerReceiptFixture(t *testing.T) (ownerReceiptPhase1, ownerReceiptOperations, *[]string) {
	t.Helper()
	root := t.TempDir()
	pg := filepath.Join(root, "pg")
	if os.MkdirAll(filepath.Join(pg, "base"), 0o700) != nil ||
		os.WriteFile(filepath.Join(pg, "PG_VERSION"), []byte("16\n"), 0o600) != nil ||
		os.WriteFile(filepath.Join(pg, "base", "row"), []byte("synthetic row"), 0o600) != nil {
		t.Fatal("cannot prepare synthetic receipt")
	}
	var calls []string
	before := ownerReceiptPhase1{BootID: "old", ServerPID: 10, ServerStartTime: "linux:old:1", OperatorID: "synthetic", RegisteredAt: time.Now()}
	ops := ownerReceiptOperations{
		boot: func() string { calls = append(calls, "boot"); return "new" },
		process: func() localcell.ProcessRecord {
			calls = append(calls, "process")
			return localcell.ProcessRecord{PID: 20, StartTime: "linux:new:2"}
		},
		operators: func() []ownerReceiptOperator {
			calls = append(calls, "read")
			return []ownerReceiptOperator{{ID: before.OperatorID, Name: lifecycleUnicodeName}}
		},
		captureProcesses: func() bool { calls = append(calls, "capture"); return true },
		operation:        func(action string) bool { calls = append(calls, action); return true },
		inventory: func() (map[string]ownerReceiptEntry, error) {
			calls = append(calls, "inventory")
			return ownerReceiptInventory(pg)
		},
		retained:         func() bool { calls = append(calls, "retained"); return true },
		deleteCredential: func() error { calls = append(calls, "delete"); return nil },
		publish: func(data []byte) error {
			calls = append(calls, "publish")
			var record ownerReceiptPhase2
			if json.Unmarshal(data, &record) != nil || !record.BootIDChanged || !record.AutomaticReadyAfterLogin ||
				!record.FreshProcessIdentity || !record.WriteSurvived || !record.RemovedRetainingData {
				return errors.New("invalid synthetic receipt")
			}
			return nil
		},
	}
	return before, ops, &calls
}

func TestOwnerReceiptPhase2Contract(t *testing.T) {
	before, ops, calls := ownerReceiptFixture(t)
	record, err := ownerReceiptRunPhase2(before, ops)
	want := []string{"boot", "observe", "process", "read", "capture", "stop", "inventory", "remove", "inventory", "retained", "delete", "publish"}
	if err != nil || !record.RemovedRetainingData || !slices.Equal(*calls, want) {
		t.Fatal("receipt did not execute the complete ordered proof and publication contract")
	}
}

func TestOwnerReceiptSameBootFreshProcessRejected(t *testing.T) {
	before, ops, calls := ownerReceiptFixture(t)
	before.BootID = "new" // The otherwise-valid fixture has a fresh process identity.
	_, err := ownerReceiptRunPhase2(before, ops)
	if err == nil || err.Error() != "boot_id_changed" || !slices.Equal(*calls, []string{"boot"}) {
		t.Fatal("same-boot receipt reached readiness or read-back")
	}
}

func TestOwnerReceiptOperationsRefused(t *testing.T) {
	for _, completed := range []int{0, 1, 2, 3, 4, 5} {
		for _, action := range []string{"start", "run", "install", "repair", "health", "status"} {
			called := false
			ops := ownerReceiptOperations{operation: func(string) bool { called = true; return true }}
			boundary := ownerReceiptActions{ops: &ops, completed: completed}
			if boundary.call(action) || called || !boundary.refused {
				t.Fatal("forbidden receipt operation reached its hook")
			}
			if action != "install" && (ownerReceiptInvokeAllowed(action, func() bool { called = true; return true }) || called) {
				t.Fatal("forbidden receipt invocation reached its executable")
			}
		}
	}
	for _, action := range []string{"install", "remove"} {
		called := false
		if !ownerReceiptInvokeAllowed(action, func() bool { called = true; return true }) || !called ||
			ownerReceiptInvokeAllowed(action, func() bool { return false }) {
			t.Fatal("allowed receipt invocation did not preserve its executable result")
		}
	}
}

func TestOwnerReceiptRemovalDamageRejected(t *testing.T) {
	for _, scenario := range []string{"delete_base", "truncate"} {
		ownerReceiptRequiredRun(t, scenario, func(t *testing.T) {
			before, ops, calls := ownerReceiptFixture(t)
			pg := ownerReceiptSyntheticData(t)
			ops.inventory = func() (map[string]ownerReceiptEntry, error) { return ownerReceiptInventory(pg) }
			operation := ops.operation
			ops.operation = func(action string) bool {
				if action == "remove" {
					var err error
					if scenario == "delete_base" {
						err = os.RemoveAll(filepath.Join(pg, "base"))
					} else {
						err = os.Truncate(filepath.Join(pg, "base", "database", "relation"), 1)
					}
					if err != nil {
						t.Fatal("cannot damage synthetic stopped data")
					}
				}
				return operation(action)
			}
			record, err := ownerReceiptRunPhase2(before, ops)
			if err == nil || err.Error() != "removed_retaining_data" || record.RemovedRetainingData ||
				!slices.Contains(*calls, "remove") || slices.Contains(*calls, "delete") || slices.Contains(*calls, "publish") {
				t.Fatal("damaged stopped data reached credential deletion or publication")
			}
		})
	}
}

// A t.Run success alone is not execution evidence: filtering returns true and
// Skip marks no failure. Both entry and normal return are mandatory here.
func ownerReceiptRequiredRun(t *testing.T, name string, guard func(*testing.T)) {
	t.Helper()
	executed, returned := false, false
	passed := t.Run(name, func(t *testing.T) {
		executed = true
		guard(t)
		returned = !t.Skipped()
	})
	if !executed || !returned || !passed {
		t.Fatal("receipt regression guard did not execute successfully: " + name)
	}
}

// The tagged driver calls this same suite under its parent before touching the
// receipt. A selector cannot silently omit a required regression guard.
func TestOwnerReceiptRegressionGuards(t *testing.T) {
	for _, guard := range []struct {
		name string
		run  func(*testing.T)
	}{
		{"M1_same_boot", TestOwnerReceiptSameBootFreshProcessRejected},
		{"M2_observer_only", func(t *testing.T) {
			TestOwnerReceiptOperationsRefused(t)
			TestOwnerReceiptDriverWiring(t)
		}},
		{"M3_final_evidence", func(t *testing.T) {
			TestOwnerReceiptFinalEvidenceRequired(t)
			TestOwnerReceiptPhase2Contract(t)
		}},
		{"M4_private_credential", func(t *testing.T) {
			TestOwnerReceiptPrivateWrite(t)
			TestOwnerReceiptPrivateWriteRejectsActualPermissions(t)
		}},
		{"M5_cleanup_containment", TestOwnerReceiptCleanupContainment},
		{"M6_exact_unicode", TestOwnerReceiptUnicodeMismatchRejected},
		{"M7_stopped_data", func(t *testing.T) {
			TestOwnerReceiptRemovalDamageRejected(t)
			TestOwnerReceiptDriverWiring(t)
		}},
	} {
		ownerReceiptRequiredRun(t, guard.name, guard.run)
	}
}

func TestOwnerReceiptFinalEvidenceRequired(t *testing.T) {
	for i := range ownerReceiptProofNames {
		for _, defect := range []string{"not_executed", "not_passed", "stored_false"} {
			record := ownerReceiptPhase2{BootIDChanged: true, AutomaticReadyAfterLogin: true, FreshProcessIdentity: true, WriteSurvived: true, RemovedRetainingData: true}
			var proofs [5]ownerReceiptProof
			for j := range proofs {
				proofs[j] = ownerReceiptProof{executed: true, passed: true}
			}
			values := []*bool{&record.BootIDChanged, &record.AutomaticReadyAfterLogin, &record.FreshProcessIdentity, &record.WriteSurvived, &record.RemovedRetainingData}
			switch defect {
			case "not_executed":
				proofs[i].executed = false
			case "not_passed":
				proofs[i].passed = false
			case "stored_false":
				*values[i] = false
			}
			err := ownerReceiptValidate(record, proofs, &ownerReceiptActions{trace: []string{"observe", "stop", "remove"}})
			if err == nil || err.Error() != ownerReceiptProofNames[i] {
				t.Fatal("incomplete evidence accepted or wrong first failure")
			}
		}
	}
}

func TestOwnerReceiptUnicodeMismatchRejected(t *testing.T) {
	before, ops, calls := ownerReceiptFixture(t)
	ops.operators = func() []ownerReceiptOperator {
		return []ownerReceiptOperator{{ID: before.OperatorID, Name: "corrupted nonempty name"}}
	}
	_, err := ownerReceiptRunPhase2(before, ops)
	if err == nil || err.Error() != "write_survived" || slices.Contains(*calls, "stop") || slices.Contains(*calls, "delete") || slices.Contains(*calls, "publish") {
		t.Fatal("corrupted Unicode witness reached removal or publication")
	}
}

func TestOwnerReceiptSelectionProbe(t *testing.T) {
	if len(os.Args) > 0 && os.Args[len(os.Args)-1] == "owner-receipt-guard-probe" {
		TestOwnerReceiptRegressionGuards(t)
		t.Log("receipt regression guards executed")
		return
	}
	if len(os.Args) == 0 || os.Args[len(os.Args)-1] != "owner-receipt-selection-probe" {
		return
	}
	before, ops, calls := ownerReceiptFixture(t)
	before.BootID = "new"
	_, err := ownerReceiptRunPhase2(before, ops)
	if err != nil {
		if !slices.Equal(*calls, []string{"boot"}) {
			t.Fatal("unexpected proof execution")
		}
		t.Fatal("boot_id_changed executed and rejected")
	}
	t.Log("RECEIPT PHASE2 OK")
}

func TestOwnerReceiptFilteredSelection(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal("cannot locate receipt regression executable")
	}
	for _, flags := range [][]string{
		{"-test.run=^TestOwnerReceiptSelectionProbe$"},
		{"-test.run=^TestOwnerReceiptSelectionProbe$/no_proof$"},
		{"-test.run=^TestOwnerReceiptSelectionProbe$", "-test.skip=^TestOwnerReceiptSelectionProbe$/boot_id_changed$"},
		{"-test.run=^TestOwnerReceiptSelectionProbe$", "-test.skip=^TestOwnerReceiptSelectionProbe$/.*$"},
	} {
		args := append(slices.Clone(flags), "-test.v", "--", "owner-receipt-selection-probe")
		output, err := exec.CommandContext(t.Context(), binary, args...).CombinedOutput()
		if err == nil || !strings.Contains(string(output), "boot_id_changed executed and rejected") || strings.Contains(string(output), "RECEIPT PHASE2 OK") {
			t.Fatal("filtered receipt selection bypassed executed proof failure")
		}
	}
	for _, scenario := range []struct {
		flags []string
		pass  bool
	}{
		{[]string{"-test.run=^TestOwnerReceiptSelectionProbe$"}, true},
		{[]string{"-test.run=^TestOwnerReceiptSelectionProbe$/no_guard$"}, false},
		{[]string{"-test.run=^TestOwnerReceiptSelectionProbe$", "-test.skip=^TestOwnerReceiptSelectionProbe$/M3_final_evidence$"}, false},
		{[]string{"-test.run=^TestOwnerReceiptSelectionProbe$", "-test.skip=^TestOwnerReceiptSelectionProbe$/M4_private_credential/actual_0644$"}, false},
	} {
		args := append(slices.Clone(scenario.flags), "-test.v", "--", "owner-receipt-guard-probe")
		output, err := exec.CommandContext(t.Context(), binary, args...).CombinedOutput()
		if (err == nil) != scenario.pass || strings.Contains(string(output), "receipt regression guards executed") != scenario.pass ||
			(!scenario.pass && !strings.Contains(string(output), "receipt regression guard did not execute successfully")) {
			t.Fatal("filtered regression guard selection did not require execution")
		}
	}
}
