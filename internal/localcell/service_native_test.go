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
	if runtime.GOOS != "linux" {
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
	if err := cell.PrepareService(t.Context(), descriptor); err != nil {
		t.Fatal("cannot prepare native service descriptor")
	}
	descriptor, err := cell.ServiceDescriptor()
	if err != nil {
		t.Fatal("cannot reopen native service descriptor")
	}
	adapter := localcell.NewServiceAdapter()
	_, planErr := adapter.Plan(t.Context(), descriptor)
	manager := planErr == nil
	if planErr != nil && !errors.Is(planErr, localcell.ErrServiceManagerUnavailable) {
		t.Fatal("native service ownership or capability preflight failed")
	}
	var command *exec.Cmd
	var exited chan error
	invoke := func(action string) error {
		cmd := exec.CommandContext(t.Context(), host, "service", action, "--root", root)
		// The user-manager probe needs only the session directory. Child runtime
		// configuration and credentials are constructed by the lifecycle itself.
		cmd.Env = []string{"WITSELF_RUNTIME=local", "XDG_RUNTIME_DIR=" + os.Getenv("XDG_RUNTIME_DIR")}
		cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
		return cmd.Run()
	}
	start := func() {
		t.Helper()
		if manager {
			if err := invoke("install"); err != nil {
				t.Fatal("native user service install failed")
			}
			return
		}
		command = exec.Command(host, "run", "--root", root)
		command.Env = []string{"WITSELF_RUNTIME=local"}
		command.Stdout, command.Stderr = io.Discard, io.Discard
		if err := command.Start(); err != nil {
			t.Fatal("native foreground service host failed to start")
		}
		exited = make(chan error, 1)
		go func() { exited <- command.Wait() }()
	}
	stop := func() error {
		if manager {
			// Disable the exact registration; reapplying it is a fresh host on
			// this boot, with the same durable running fence and private data.
			return adapter.Remove(context.Background(), descriptor)
		}
		if command == nil {
			return nil
		}
		if err := command.Process.Signal(os.Interrupt); err != nil {
			return errors.New("native foreground interrupt failed")
		}
		select {
		case err := <-exited:
			command = nil
			return err
		case <-time.After(75 * time.Second):
			_ = command.Process.Kill()
			<-exited
			command = nil
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
	oldIdentity, newIdentity := strings.Split(before.StartTime, ":"), strings.Split(after.StartTime, ":")
	if len(oldIdentity) != 3 || len(newIdentity) != 3 || oldIdentity[0] != "linux" || newIdentity[0] != "linux" ||
		oldIdentity[1] != newIdentity[1] || (before.PID == after.PID && before.StartTime == after.StartTime) {
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
