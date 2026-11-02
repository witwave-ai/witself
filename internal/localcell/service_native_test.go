package localcell_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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

// TestNativeServiceLifecycle is enrolled in the existing Unix native job. The
// ordinary hermetic lane records its unavailable inputs without a test skip.
// Reboot/login and two-user isolation remain separate owner receipts.
func TestNativeServiceLifecycle(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
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
	binaries := buildRealComponents(t, pgbin)
	cell, root := disposableCell(t, binaries, nil)
	if err := cell.Install(t.Context(), localcell.InstallOptions{}); err != nil {
		t.Fatal("cannot install native service fixture")
	}
	host := filepath.Join(t.TempDir(), lifecycleExecutableName("localcellharness"))
	build := offlineBuildCommand(t.Context(), "build", "-o", host, "../tools/localcellharness")
	if err := build.Run(); err != nil {
		t.Fatal("cannot build native service host")
	}
	descriptor := localcell.ServiceDescriptor{Adapter: "systemd-user", Scope: "user", Runtime: "local", DesiredState: "running", ResourceProfile: "unconfigured", Payload: localcell.ServicePayload{Host: host, Args: []string{"run", "--root", root, "--pgbin", binaries.PostgresDir, "--server", binaries.Server, "--worker", binaries.Worker, "--timeout", "60s"}}}
	if runtime.GOOS == "darwin" {
		descriptor.Adapter = "launchd-agent"
	}
	if err := cell.PrepareService(t.Context(), descriptor); err != nil {
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
	diagnosticRun := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "child-capability" {
			return nativeServiceCapabilityDiagnostic(ctx, descriptor)
		}
		return nativeServiceDiagnosticOutput(ctx, name, args...)
	}
	logFailure := func(err error, stderr []byte) {
		t.Helper()
		nativeServiceFailureDiagnostics(t.Logf, root, diagnosticHome, label, err, stderr, diagnosticRun)
	}
	_, planErr := adapter.Plan(t.Context(), descriptor)
	manager := planErr == nil
	if planErr != nil && !errors.Is(planErr, localcell.ErrServiceManagerUnavailable) {
		logFailure(planErr, nil)
		t.Fatal("native service ownership or capability preflight failed")
	}
	var command *exec.Cmd
	var exited chan error
	var foregroundStderr *nativeServiceDiagnosticBuffer
	invoke := func(action string) error {
		return nativeServiceInvoke(t.Context(), host, root, diagnosticHome, label,
			[]string{"service", action, "--root", root}, t.Logf, diagnosticRun)
	}
	start := func() {
		t.Helper()
		if manager {
			if err := invoke("install"); err != nil {
				t.Fatal("native user service install failed")
			}
			if runtime.GOOS == "darwin" {
				nativeServiceLaunchdRegistrationDiagnostics(os.Getuid(), t.Logf, root, diagnosticHome, label, diagnosticRun)
			}
			return
		}
		command = exec.Command(host, "run", "--root", root)
		command.Env = nativeServiceChildEnvironment(os.LookupEnv)
		foregroundStderr = &nativeServiceDiagnosticBuffer{}
		command.Stdout, command.Stderr = io.Discard, foregroundStderr
		if err := command.Start(); err != nil {
			logFailure(err, foregroundStderr.snapshot())
			command = nil
			t.Fatal("native foreground service host failed to start")
		}
		exited = make(chan error, 1)
		go func() { exited <- command.Wait() }()
	}
	stop := func() error {
		if manager {
			// Remove the exact registration; reapplying it is a fresh host on
			// this boot, with the same durable running fence and private data.
			err := adapter.Remove(context.Background(), descriptor)
			if err != nil {
				logFailure(err, nil)
			}
			return err
		}
		if command == nil {
			return nil
		}
		if err := command.Process.Signal(os.Interrupt); err != nil {
			logFailure(err, foregroundStderr.snapshot())
			return errors.New("native foreground interrupt failed")
		}
		select {
		case err := <-exited:
			command = nil
			if err != nil {
				logFailure(err, foregroundStderr.snapshot())
			}
			return err
		case <-time.After(75 * time.Second):
			_ = command.Process.Kill()
			err := <-exited
			command = nil
			logFailure(err, foregroundStderr.snapshot())
			return errors.New("native foreground shutdown timed out")
		}
	}
	t.Cleanup(func() {
		if err := stop(); err != nil {
			t.Error("native service cleanup failed")
		}
	})
	if manager {
		t.Log("PASS service_manager_available")
	} else {
		t.Log("NOT_RUN service_manager_unavailable")
		if runtime.GOOS == "darwin" {
			logFailure(planErr, nil)
		}
	}
	start()
	waitServiceReady(t, cell)
	before := nativeServiceProcess(t, root)
	manifest := assertReady(t, cell)
	read := nativeServiceWriteWitness(t, root, manifest)
	if err := stop(); err != nil {
		t.Fatal("native service first host did not stop cleanly")
	}
	assertStopped(t, cell)
	start()
	waitServiceReady(t, cell)
	after := nativeServiceProcess(t, root)
	if !nativeServiceFreshProcess(runtime.GOOS, before, after) {
		t.Fatal("native service restart did not establish a fresh process on the same boot")
	}
	read()
	if err := invoke("stop"); err != nil {
		t.Fatal("authenticated native service stop failed")
	}
	if manager {
		if err := invoke("remove"); err != nil {
			t.Fatal("native user service removal failed")
		}
	} else if err := stop(); err != nil {
		t.Fatal("native foreground final shutdown failed")
	}
	for _, name := range []string{"pg", "manifest.json"} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Fatal("native service removal lost retained data")
		}
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := localcell.ProcessRecord{PID: 10, StartTime: tc.before}
			after := localcell.ProcessRecord{PID: 11, StartTime: tc.after}
			if tc.samePID {
				after.PID = before.PID
			}
			if got := nativeServiceFreshProcess(tc.goos, before, after); got != tc.want {
				t.Fatal("native restart identity decision differs from the platform contract")
			}
		})
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

func waitServiceReady(t *testing.T, cell *localcell.Cell) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if status, err := cell.Control(ctx, "health"); err == nil && status.Healthy {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("native service authenticated health did not become ready")
		case <-ticker.C:
		}
	}
}

func nativeServiceWriteWitness(t *testing.T, root string, manifest *localcell.Manifest) func() {
	t.Helper()
	db, err := store.Open(t.Context(), lifecycleDSN(t, root, manifest))
	if err != nil {
		t.Fatal("cannot open native service proof database")
	}
	owner, err := db.EnsureRootOperator(t.Context(), manifest.AccountID)
	if err != nil {
		db.Close()
		t.Fatal("cannot resolve native service proof operator")
	}
	ttl := time.Hour
	bearer, _, _, err := db.CreateOperatorToken(t.Context(), manifest.AccountID, owner, "service persistence proof", &ttl)
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
	lifecycleJSON(t, manifest, bearer, http.MethodPost, "/v1/operators", body, &created)
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
		lifecycleJSON(t, manifest, bearer, http.MethodGet, "/v1/operators", nil, &listed)
		for _, operator := range listed.Operators {
			if operator.ID == created.Operator.ID && operator.Name == lifecycleUnicodeName {
				return
			}
		}
		t.Fatal("authenticated write did not survive native service host restart")
	}
}
