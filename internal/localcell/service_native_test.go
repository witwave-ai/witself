package localcell_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/localcell"
	"github.com/witwave-ai/witself/internal/store"
)

// One declared host bound drives both the persisted host and its control client.
const nativeServiceHostBound = 60 * time.Second

// TestNativeServiceLifecycle is enrolled in the native Unix and Windows jobs. The
// ordinary hermetic lane records its unavailable inputs without a test skip.
// Reboot/login and two-user isolation remain separate owner receipts.
func TestNativeServiceLifecycle(t *testing.T) {
	proofStarted := time.Now()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		if _, err := localcell.NewServiceAdapter().Plan(t.Context(), localcell.ServiceDescriptor{}); !errors.Is(err, localcell.ErrServiceManagerUnavailable) {
			t.Fatal("unsupported native adapter did not return its capability result")
		}
		t.Log("NOT_RUN service_adapter_unsupported")
		return
	}
	pgbin := os.Getenv("WITSELF_LOCALCELL_PGBIN")
	if pgbin == "" {
		t.Log("NOT_RUN native_postgres_unconfigured")
		return
	}
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	t.Setenv("WITSELF_RUNTIME", "local")
	if runtime.GOOS == "windows" {
		probeCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		diagnostic, _ := nativeServiceCapabilityDiagnostic(probeCtx, localcell.ServiceDescriptor{})
		cancel()
		for _, line := range strings.Split(strings.TrimSpace(sanitizeNativeServiceDiagnostic(string(diagnostic), "", "")), "\n") {
			t.Log(line)
		}
		if err := localcell.NativeServiceControlCapability(); err != nil {
			t.Log("NOT_RUN service_manager_unavailable")
			t.Log("service_boot_identity_unavailable")
			return
		}
	}
	proofCtx, cancelProof := context.WithDeadline(t.Context(), proofStarted.Add(nativeServiceProofBound))
	t.Cleanup(cancelProof)
	proof := newNativeServiceProofBudget(proofCtx, nativeServiceProofBound)
	diagnostics := nativeServiceFailureDiagnostic{goos: runtime.GOOS, uid: os.Getuid(), logf: t.Logf, proof: proof}
	t.Cleanup(func() {
		if t.Failed() {
			diagnostics.logProofBudget()
		}
	})
	diagnostics.enter("setup")
	proofPhase := func(limit time.Duration) context.Context {
		if proofCtx.Err() != nil {
			diagnostics.logProofBudget()
			t.Fatal("native service proof budget exhausted")
		}
		ctx, cancel := proof.phase(proofCtx, limit)
		t.Cleanup(cancel)
		return ctx
	}
	binaries := buildRealComponentsContext(proofPhase(nativeServiceHostBound), t, pgbin)
	var stopForCleanup func(context.Context) error
	cell, root := disposableCellWithCleanup(t, binaries, nil, func(ctx context.Context) error {
		if stopForCleanup != nil {
			return stopForCleanup(ctx)
		}
		return nil
	}, func(config *localcell.Config) {
		config.Timeout = nativeServiceHostBound
	})
	if err := cell.Install(proofPhase(nativeServiceHostBound), localcell.InstallOptions{}); err != nil {
		t.Fatal("cannot install native service fixture")
	}
	host := filepath.Join(t.TempDir(), lifecycleExecutableName("localcellharness"))
	build := offlineBuildCommand(proofPhase(nativeServiceHostBound), "build", "-o", host, "../tools/localcellharness")
	if err := build.Run(); err != nil {
		t.Fatal("cannot build native service host")
	}
	descriptor := localcell.ServiceDescriptor{Adapter: "systemd-user", Scope: "user", Runtime: "local", DesiredState: "running", ResourceProfile: "unconfigured", Payload: localcell.ServicePayload{Host: host, Args: []string{"run", "--root", root, "--pgbin", binaries.PostgresDir, "--server", binaries.Server, "--worker", binaries.Worker, "--timeout", nativeServiceHostBound.String()}}}
	switch runtime.GOOS {
	case "darwin":
		descriptor.Adapter = "launchd-agent"
	case "windows":
		descriptor.Adapter = "foreground"
	}
	if err := cell.PrepareService(proofPhase(nativeServiceHostBound), descriptor); err != nil {
		t.Fatal("cannot prepare native service descriptor")
	}
	descriptor, err := cell.ServiceDescriptor()
	if err != nil {
		t.Fatal("cannot reopen native service descriptor")
	}
	adapter := localcell.NewServiceAdapter()
	diagnosticHome := nativeServiceDiagnosticHome()
	label := "witself-cell-" + descriptor.InstallationID + ".service"
	if runtime.GOOS == "darwin" {
		label = "ai.witwave.witself.cell." + descriptor.InstallationID
	}
	diagnosticRun := func(_ context.Context, name string, args ...string) ([]byte, error) {
		ctx, cancel := proof.phase(proofCtx, 5*time.Second)
		defer cancel()
		if name == "child-capability" {
			return nativeServiceCapabilityDiagnostic(ctx, descriptor)
		}
		return nativeServiceDiagnosticOutput(ctx, name, args...)
	}
	diagnostics.root, diagnostics.home, diagnostics.label, diagnostics.run = root, diagnosticHome, label, diagnosticRun
	logFailure := diagnostics.logFailure
	_, planErr := adapter.Plan(proofPhase(nativeServiceHostBound), descriptor)
	manager := planErr == nil
	if planErr != nil && !errors.Is(planErr, localcell.ErrServiceManagerUnavailable) {
		logFailure(planErr, nil)
		t.Fatal("native service ownership or capability preflight failed")
	}
	var command *exec.Cmd
	var exited <-chan error
	managerActive := false
	stopWithReceipt := func(stopHost func() error) error {
		return nativeServiceStopWithShutdownReceipt(runtime.GOOS, root, label, diagnostics.foregroundStderr, t.Logf, diagnosticRun, stopHost)
	}
	invoke := func(action string) error {
		return nativeServiceInvoke(proofPhase(nativeServiceReadinessBudget(nativeServiceHostBound, 0)), host, root, diagnosticHome, label,
			[]string{"service", action, "--root", root}, t.Logf, diagnosticRun, diagnostics.foregroundStderr)
	}
	start := func() {
		t.Helper()
		if manager {
			managerActive = true // Cleanup also owns a partially failed install.
			if err := invoke("install"); err != nil {
				t.Fatal("native user service install failed")
			}
			if runtime.GOOS == "darwin" {
				nativeServiceLaunchdRegistrationDiagnostics(os.Getuid(), t.Logf, root, diagnosticHome, label, diagnosticRun)
			}
			return
		}
		command = exec.CommandContext(proofCtx, host, "run", "--root", root)
		var err error
		exited, err = diagnostics.startForeground(command)
		if err != nil {
			logFailure(err, diagnostics.foregroundStderr.snapshot())
			command = nil
			t.Fatal("native foreground service host failed to start")
		}
	}
	stop := func(parent context.Context) error {
		stopCtx, cancelStop := context.WithTimeout(parent, nativeServiceCleanupReserve)
		defer cancelStop()
		if manager {
			if !managerActive {
				return nil
			}
			// Remove the exact registration; reapplying it is a fresh host on
			// this boot, with the same durable running fence and private data.
			err := stopWithReceipt(func() error {
				if err := adapter.Remove(stopCtx, descriptor); err != nil {
					return err
				}
				managerActive = false
				return nil
			})
			if err != nil {
				logFailure(err, nil)
			}
			return err
		}
		if command == nil {
			return nil
		}
		return stopWithReceipt(func() error {
			if err := nativeServiceInterruptForeground(command.Process); err != nil {
				logFailure(err, diagnostics.foregroundStderr.snapshot())
				return errors.New("native foreground interrupt failed")
			}
			select {
			case err := <-exited:
				command = nil
				if err != nil {
					logFailure(err, diagnostics.foregroundStderr.snapshot())
				}
				return err
			case <-stopCtx.Done():
				_ = command.Process.Kill()
				err := <-exited
				command = nil
				logFailure(err, diagnostics.foregroundStderr.snapshot())
				return errors.New("native foreground shutdown timed out")
			}
		})
	}
	stopForCleanup = stop
	t.Cleanup(func() {
		if t.Failed() {
			var stderr []byte
			if diagnostics.foregroundStderr != nil {
				stderr = diagnostics.foregroundStderr.snapshot()
			}
			logFailure(errors.New("native service proof failed"), stderr)
		}
	})
	if manager {
		t.Log("PASS service_manager_available")
	} else {
		t.Log("NOT_RUN service_manager_unavailable")
		if runtime.GOOS == "windows" {
			t.Log("native service capability: service_adapter_unsupported")
		}
		if runtime.GOOS == "darwin" {
			logFailure(planErr, nil)
		}
	}
	diagnostics.enter("ready")
	firstStarted := time.Now()
	start()
	firstReady := waitServiceReadyAfter(proofCtx, t, cell, firstStarted, 0, &diagnostics)
	diagnostics.enter("endpoint_closure")
	endpointClosed := nativeServiceEndpointClosureProof(t, root)
	diagnostics.enter("observe")
	before := nativeServiceProcess(t, root)
	hostBefore := nativeServiceHostProcess(t, root)
	childrenBefore := nativeServiceChildren(t, root)
	manifest := assertReady(t, cell, proofPhase(nativeServiceHostBound))
	diagnostics.enter("write_witness")
	read := nativeServiceWriteWitness(proofCtx, t, proof, root, manifest)
	diagnostics.enter("stop")
	if err := stop(proofPhase(nativeServiceCleanupReserve)); err != nil {
		t.Fatal("native service first host did not stop cleanly")
	}
	assertStopped(t, cell, proofPhase(nativeServiceHostBound))
	assertNativeServiceHostRemoved(proofPhase(time.Second), t, cell, root)
	diagnostics.enter("endpoint_closure")
	endpointClosed()
	diagnostics.enter("restart")
	restartStarted := time.Now()
	start()
	waitServiceReadyAfter(proofCtx, t, cell, restartStarted, firstReady, &diagnostics)
	diagnostics.enter("endpoint_closure")
	endpointClosed = nativeServiceEndpointClosureProof(t, root)
	diagnostics.enter("observe")
	after := nativeServiceProcess(t, root)
	if !nativeServiceFreshProcess(runtime.GOOS, before, after) {
		t.Fatal("native service restart did not establish a fresh process on the same boot")
	}
	if !nativeServiceFreshProcess(runtime.GOOS, hostBefore, nativeServiceHostProcess(t, root)) {
		t.Fatal("native service restart did not establish a fresh host on the same boot")
	}
	for component, previous := range childrenBefore {
		if !nativeServiceFreshProcess(runtime.GOOS, previous, nativeServiceChildren(t, root)[component]) {
			t.Fatal("native service restart did not establish fresh child identities")
		}
	}
	diagnostics.enter("write_witness")
	read()
	diagnostics.enter("stop")
	if err := invoke("stop"); err != nil {
		diagnostics.controlErr = err
		t.Fatal("authenticated native service stop failed")
	}
	diagnostics.enter("observe")
	status, err := cell.Control(proofPhase(nativeServiceReadinessBudget(nativeServiceHostBound, 0)), "observe")
	diagnostics.controlErr = err
	if err != nil || status.DesiredState != "stopped" || status.State != "stopped" || status.Healthy {
		t.Fatal("native service durable stop fence was not observed")
	}
	assertStopped(t, cell, proofPhase(nativeServiceHostBound))
	diagnostics.enter("stop")
	if manager {
		if err := stopWithReceipt(func() error {
			if err := invoke("remove"); err != nil {
				return err
			}
			managerActive = false
			return nil
		}); err != nil {
			t.Fatal("native user service removal failed")
		}
	} else if err := stop(proofPhase(nativeServiceCleanupReserve)); err != nil {
		t.Fatal("native foreground final shutdown failed")
	}
	assertNativeServiceHostRemoved(proofPhase(time.Second), t, cell, root)
	diagnostics.enter("endpoint_closure")
	endpointClosed()
	for _, name := range []string{"pg", "manifest.json"} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Fatal("native service removal lost retained data")
		}
	}
	if proofCtx.Err() != nil {
		logFailure(proofCtx.Err(), nil)
		t.Fatal("native service proof budget exhausted")
	}
	if manager {
		t.Log("PASS service_manager_persistence")
	} else {
		t.Log("PASS service_foreground_persistence")
	}
}

func nativeServiceFreshProcess(goos string, before, after localcell.ProcessRecord) bool {
	oldIdentity, newIdentity := strings.Split(before.StartTime, ":"), strings.Split(after.StartTime, ":")
	if len(oldIdentity) != 3 || len(newIdentity) != 3 || oldIdentity[0] != goos || newIdentity[0] != goos ||
		(before.PID == after.PID && before.StartTime == after.StartTime) {
		return false
	}
	if goos == "linux" {
		return oldIdentity[1] == newIdentity[1]
	}
	if goos == "windows" {
		// GetProcessTimes function (processthreadsapi.h): FILETIME contains
		// two DWORDs. Creation time is an incarnation, never boot authority.
		// A restart proof requires a fresh creation time even if the PID changed.
		if before.StartTime == after.StartTime {
			return false
		}
		if before.BootID == "" || before.BootID != after.BootID || !strings.HasPrefix(before.BootID, "windows:") {
			return false
		}
		for _, identity := range [][]string{oldIdentity, newIdentity} {
			for _, word := range identity[1:] {
				value, err := strconv.ParseUint(word, 10, 32)
				if err != nil || strconv.FormatUint(value, 10) != word {
					return false
				}
			}
			if identity[1] == "0" && identity[2] == "0" {
				return false
			}
		}
		return true
	}
	if goos != "darwin" {
		return false
	}
	// process_darwin.go records creation seconds and microseconds. Unlike the
	// Linux boot ID, neither timestamp field must match after a host restart.
	for _, identity := range [][]string{oldIdentity, newIdentity} {
		seconds, secErr := strconv.ParseInt(identity[1], 10, 64)
		microseconds, usecErr := strconv.ParseInt(identity[2], 10, 64)
		if secErr != nil || usecErr != nil || seconds <= 0 || microseconds < 0 || microseconds >= 1_000_000 {
			return false
		}
	}
	return true
}

func TestNativeServiceFreshProcess(t *testing.T) {
	for _, tc := range []struct {
		name, goos, before, after string
		samePID, want             bool
	}{
		{"linux_same_boot", "linux", "linux:boot:1", "linux:boot:2", true, true},
		{"linux_other_boot", "linux", "linux:boot:1", "linux:other:2", false, false},
		{"linux_same_process", "linux", "linux:boot:1", "linux:boot:1", true, false},
		{"darwin_new_second", "darwin", "darwin:10:999999", "darwin:11:0", true, true},
		{"darwin_new_microsecond", "darwin", "darwin:10:1", "darwin:10:2", true, true},
		{"darwin_same_process", "darwin", "darwin:10:1", "darwin:10:1", true, false},
		{"darwin_bad_second", "darwin", "darwin:zero:1", "darwin:11:0", false, false},
		{"darwin_bad_microsecond", "darwin", "darwin:10:1", "darwin:11:1000000", false, false},
		{"wrong_platform", "darwin", "linux:10:1", "darwin:11:0", false, false},
		{"missing_field", "darwin", "darwin:10", "darwin:11:0", false, false},
		{"windows_new_word", "windows", "windows:10:1", "windows:11:0", true, true},
		{"windows_uint32_max", "windows", "windows:4294967295:4294967294", "windows:4294967295:4294967295", true, true},
		{"windows_word_overflow", "windows", "windows:4294967296:1", "windows:11:0", false, false},
		{"windows_negative_word", "windows", "windows:-1:1", "windows:11:0", false, false},
		{"windows_zero_identity", "windows", "windows:0:0", "windows:11:0", false, false},
		{"windows_same_process", "windows", "windows:10:1", "windows:10:1", true, false},
		{"windows_same_start_different_pid", "windows", "windows:10:1", "windows:10:1", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := localcell.ProcessRecord{PID: 10, StartTime: tc.before}
			after := localcell.ProcessRecord{PID: 11, StartTime: tc.after}
			if tc.goos == "windows" {
				before.BootID, after.BootID = "windows:fixture-boot", "windows:fixture-boot"
			}
			if tc.samePID {
				after.PID = before.PID
			}
			if got := nativeServiceFreshProcess(tc.goos, before, after); got != tc.want {
				t.Fatal("native restart identity decision differs from the platform contract")
			}
		})
	}
}

func TestNativeServiceFreshWindowsBootBinding(t *testing.T) {
	before := localcell.ProcessRecord{PID: 10, StartTime: "windows:1:1", BootID: "windows:old-boot"}
	for _, boot := range []string{"", "windows:new-boot", "linux:old-boot"} {
		after := localcell.ProcessRecord{PID: 11, StartTime: "windows:2:1", BootID: boot}
		if nativeServiceFreshProcess("windows", before, after) {
			t.Fatal("fresh Windows incarnation bypassed independent boot binding")
		}
	}
}

func nativeServiceHostProcess(t *testing.T, root string) localcell.ProcessRecord {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "service", "lease.json"))
	var lease struct {
		PID           int    `json:"pid"`
		StartIdentity string `json:"start_identity"`
		BootID        string `json:"boot_id"`
	}
	if err != nil || json.Unmarshal(data, &lease) != nil || lease.PID <= 1 || lease.StartIdentity == "" || lease.BootID == "" {
		t.Fatal("cannot read native host incarnation")
	}
	return localcell.ProcessRecord{PID: lease.PID, StartTime: lease.StartIdentity, BootID: lease.BootID}
}

func nativeServiceChildren(t *testing.T, root string) map[string]localcell.ProcessRecord {
	t.Helper()
	result := make(map[string]localcell.ProcessRecord)
	for _, component := range []string{"postgres", "server", "worker"} {
		data, err := os.ReadFile(filepath.Join(root, "run", component+".json"))
		var record localcell.ProcessRecord
		if err != nil || json.Unmarshal(data, &record) != nil || record.PID <= 1 || record.StartTime == "" {
			t.Fatal("cannot read native child incarnation")
		}
		result[component] = record
	}
	return result
}

func assertNativeServiceHostRemoved(parent context.Context, t *testing.T, cell *localcell.Cell, root string) {
	t.Helper()
	endpoint := "control.sock"
	if runtime.GOOS == "windows" {
		endpoint = "control-endpoint.json"
	}
	if _, err := os.Lstat(filepath.Join(root, "service", endpoint)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("native host retained its control endpoint")
	}
	if !localcell.NativeServiceLeaseReleased(cell) {
		t.Fatal("native host retained its lifetime lease")
	}
	ctx, cancel := context.WithTimeout(parent, time.Second)
	defer cancel()
	if _, err := cell.Control(ctx, "status"); err == nil {
		t.Fatal("native host control remained available after removal")
	}
}

func nativeServiceProcess(t *testing.T, root string) localcell.ProcessRecord {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "run", "server.json"))
	var record localcell.ProcessRecord
	if err != nil || json.Unmarshal(data, &record) != nil || record.Component != "server" || record.PID <= 1 || record.StartTime == "" {
		t.Fatal("cannot read native service process identity")
	}
	return record
}

func waitServiceReadyAfter(parent context.Context, t *testing.T, cell *localcell.Cell, started time.Time, firstReady time.Duration, diagnostics ...*nativeServiceFailureDiagnostic) time.Duration {
	t.Helper()
	budget := nativeServiceReadinessBudget(nativeServiceHostBound, firstReady)
	waitingStarted := time.Now()
	proof := &nativeServiceProofBudget{deadline: time.Now().Add(budget)}
	if len(diagnostics) != 0 {
		proof = diagnostics[0].proof
	}
	ctx, cancel := proof.phase(parent, budget)
	defer cancel()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	var stats nativeServiceReadinessStatistics
	for {
		attemptStarted := time.Now()
		status, err := cell.Control(ctx, "health")
		stats.observe(err, time.Since(waitingStarted), time.Since(attemptStarted))
		if len(diagnostics) != 0 {
			diagnostics[0].controlErr = err
		}
		if err == nil && status.Healthy {
			return time.Since(started)
		}
		if len(diagnostics) != 0 && diagnostics[0].foregroundExit.finished() {
			stats.log(t.Logf, time.Since(waitingStarted), budget)
			t.Fatal("native service foreground host exited before readiness")
		}
		select {
		case <-ctx.Done():
			stats.log(t.Logf, time.Since(waitingStarted), budget)
			t.Fatal("native service authenticated health did not become ready")
		case <-ticker.C:
		}
	}
}

func nativeServiceWriteWitness(parent context.Context, t *testing.T, proof *nativeServiceProofBudget, root string, manifest *localcell.Manifest) func() {
	t.Helper()
	phase := func(limit time.Duration) context.Context {
		ctx, cancel := proof.phase(parent, limit)
		t.Cleanup(cancel)
		return ctx
	}
	db, err := store.Open(phase(nativeServiceHostBound), lifecycleDSN(t, root, manifest))
	if err != nil {
		t.Fatal("cannot open native service proof database")
	}
	owner, err := db.EnsureRootOperator(phase(nativeServiceHostBound), manifest.AccountID)
	if err != nil {
		db.Close()
		t.Fatal("cannot resolve native service proof operator")
	}
	ttl := time.Hour
	bearer, _, _, err := db.CreateOperatorToken(phase(nativeServiceHostBound), manifest.AccountID, owner, "service persistence proof", &ttl)
	db.Close()
	if err != nil {
		t.Fatal("cannot mint native service proof credential")
	}
	body, err := json.Marshal(map[string]string{"display_name": lifecycleUnicodeName, "ttl": "1h"})
	if err != nil {
		t.Fatal("cannot encode native service proof write")
	}
	var created struct {
		Operator struct {
			ID string `json:"id"`
		} `json:"operator"`
	}
	lifecycleJSON(t, manifest, bearer, http.MethodPost, "/v1/operators", body, &created, phase(5*time.Second))
	if created.Operator.ID == "" {
		t.Fatal("native service authenticated write omitted identity")
	}
	return func() {
		var listed struct {
			Operators []struct {
				ID   string `json:"id"`
				Name string `json:"display_name"`
			} `json:"operators"`
		}
		lifecycleJSON(t, manifest, bearer, http.MethodGet, "/v1/operators", nil, &listed, phase(5*time.Second))
		for _, operator := range listed.Operators {
			if operator.ID == created.Operator.ID && operator.Name == lifecycleUnicodeName {
				return
			}
		}
		t.Fatal("authenticated write did not survive native service host restart")
	}
}
