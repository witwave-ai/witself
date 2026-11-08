package localcell_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/localcell"
)

// Capability only reads the token/session and resolves the command: allocate
// one twelfth of the fixture's host/control timeout. After authenticated exit
// has already completed, allow one sixtieth for the final absence observation.
// Both scale with the host timeout passed into each fixture and its host argv.
const nativeWindowsTaskCapabilityBound = nativeServiceHostBound / 12
const nativeWindowsTaskClosureBound = nativeServiceHostBound / 60

// This path is selected only by the Windows manager-required CI invocation. A
// missing native classification premise is an open receipt, never permission to
// register a different logon type or substitute the foreground proof.
func nativeWindowsTaskManagerLifecycle(t *testing.T, started time.Time, pgbin string) {
	t.Helper()
	ctx, cancel := context.WithDeadline(t.Context(), started.Add(nativeServiceProofBound))
	defer cancel()
	capabilityCtx, capabilityCancel := context.WithTimeout(ctx, nativeWindowsTaskCapabilityBound)
	capabilityErr := localcell.WindowsLogonTaskCapability(capabilityCtx)
	capabilityCancel()
	if capabilityErr != nil {
		t.Log("NOT_RUN service_manager_receipt_missing")
		if strings.Contains(capabilityErr.Error(), "query_result_untyped") {
			t.Log("NOT_RUN windows_task_query_result_untyped")
		} else if strings.Contains(capabilityErr.Error(), "interactive_token_unavailable") || strings.Contains(capabilityErr.Error(), "owner_token_refused") {
			t.Log("NOT_RUN windows_task_interactive_token_unavailable")
		} else {
			t.Log("NOT_RUN windows_task_manager_unavailable")
		}
		return
	}
	proof := newNativeServiceProofBudget(ctx, nativeServiceProofBound)
	phase := func(bound time.Duration) context.Context {
		phaseCtx, stop := proof.phase(ctx, bound)
		t.Cleanup(stop)
		return phaseCtx
	}
	stage := func(name string, assertion func(*testing.T)) {
		t.Helper()
		if !t.Run("manager_"+name, assertion) {
			t.Fatal("native task manager stage failed")
		}
		t.Log("PASS service_manager_stage=" + name)
	}
	stage("query_typed", func(t *testing.T) {
		// Only the native capability API may establish this premise; fake
		// manager codes and portable XML tests never feed this receipt.
		if err := localcell.WindowsLogonTaskCapability(phase(nativeWindowsTaskCapabilityBound)); err != nil {
			t.Fatal("native task query classification premise disappeared")
		}
	})
	binaries := buildRealComponentsContext(phase(nativeServiceHostBound), t, pgbin)
	host := filepath.Join(t.TempDir(), lifecycleExecutableName("localcellharness"))
	if err := offlineBuildCommand(phase(nativeServiceHostBound), "build", "-o", host, "../tools/localcellharness").Run(); err != nil {
		t.Fatal("cannot build native task service host")
	}
	adapter := localcell.NewWindowsLogonTaskAdapter()
	starter, ok := adapter.(interface {
		Start(context.Context, localcell.ServiceIdentity) error
	})
	if !ok {
		t.Fatal("native task adapter lacks exact on demand start")
	}
	type installation struct {
		cell       *localcell.Cell
		root       string
		descriptor localcell.ServiceDescriptor
		registered bool
	}
	prepare := func() *installation {
		fixture := &installation{}
		cell, root := disposableCellWithCleanup(t, binaries, nil, func(cleanup context.Context) error {
			if fixture.registered {
				return adapter.Remove(cleanup, fixture.descriptor)
			}
			return nil
		}, func(config *localcell.Config) { config.Timeout = nativeServiceHostBound })
		fixture.cell, fixture.root = cell, root
		if err := cell.Install(phase(nativeServiceHostBound), localcell.InstallOptions{}); err != nil {
			t.Fatal("cannot install native task fixture")
		}
		descriptor := localcell.ServiceDescriptor{Adapter: "windows-logon-task", Scope: "user", Runtime: "local", DesiredState: "running", ResourceProfile: "unconfigured", Payload: localcell.ServicePayload{Host: host, Args: []string{"run", "--root", root, "--pgbin", binaries.PostgresDir, "--server", binaries.Server, "--worker", binaries.Worker, "--timeout", nativeServiceHostBound.String()}}}
		if err := cell.PrepareService(phase(nativeServiceHostBound), descriptor); err != nil {
			t.Fatal("cannot prepare native task descriptor")
		}
		var err error
		fixture.descriptor, err = cell.ServiceDescriptor()
		if err != nil {
			t.Fatal("cannot reopen native task descriptor")
		}
		return fixture
	}
	apply := func(t *testing.T, fixture *installation) {
		plan, err := adapter.Plan(phase(nativeServiceHostBound), fixture.descriptor)
		if err != nil {
			t.Fatal("cannot plan native task registration")
		}
		fixture.registered = true // Cleanup owns partial registration too.
		if err := adapter.Apply(phase(nativeServiceHostBound), plan); err != nil {
			t.Fatal("cannot apply native task registration")
		}
		observation, err := adapter.Inspect(phase(nativeServiceHostBound), fixture.descriptor)
		if err != nil || !observation.Available || !observation.Registered || !observation.Owned {
			t.Fatal("native task query did not prove exact ownership")
		}
	}
	ready := func(t *testing.T, fixture *installation) {
		diagnostic := nativeServiceFailureDiagnostic{goos: "windows", proof: proof, root: fixture.root, logf: t.Logf}
		waitServiceReadyAfter(ctx, t, fixture.cell, time.Now(), 0, &diagnostic)
	}
	primary, bystander := prepare(), prepare()
	apply(t, bystander)
	if err := starter.Start(phase(nativeServiceHostBound), bystander.descriptor); err != nil {
		t.Fatal("cannot start native task bystander")
	}
	ready(t, bystander)
	bystanderHost := nativeServiceHostProcess(t, bystander.root)
	bystanderChildren := nativeServiceChildren(t, bystander.root)
	bystanderRead := nativeServiceWriteWitnessWithTest(ctx, t, proof, bystander.root, assertReady(t, bystander.cell, phase(nativeServiceHostBound)))
	stage("registration", func(t *testing.T) { apply(t, primary) })
	stage("on_demand", func(t *testing.T) {
		if err := starter.Start(phase(nativeServiceHostBound), primary.descriptor); err != nil {
			t.Fatal("cannot start exact native task on demand")
		}
	})
	var read func(*testing.T)
	stage("readiness_write", func(t *testing.T) {
		ready(t, primary)
		read = nativeServiceWriteWitnessWithTest(ctx, t, proof, primary.root, assertReady(t, primary.cell, phase(nativeServiceHostBound)))
	})
	beforeHost := nativeServiceHostProcess(t, primary.root)
	beforeChildren := nativeServiceChildren(t, primary.root)
	closed := nativeServiceEndpointClosureProof(t, primary.root)
	stage("same_boot_restart", func(t *testing.T) {
		status, err := primary.cell.ExitServiceHost(phase(localcell.ServiceHostExitBudget(nativeServiceHostBound)))
		if err != nil || status.HostExit != "exited" {
			t.Fatal("native task host did not exit through authenticated drain")
		}
		assertNativeServiceHostRemoved(phase(nativeWindowsTaskClosureBound), t, primary.cell, primary.root)
		closed()
		if err := primary.cell.SetServiceDesiredState(phase(nativeServiceHostBound), "running"); err != nil {
			t.Fatal("cannot request native task same boot restart")
		}
		primary.descriptor, err = primary.cell.ServiceDescriptor()
		if err != nil || starter.Start(phase(nativeServiceHostBound), primary.descriptor) != nil {
			t.Fatal("cannot restart exact native task")
		}
		ready(t, primary)
		if !nativeServiceFreshProcess("windows", beforeHost, nativeServiceHostProcess(t, primary.root)) {
			t.Fatal("native task host restart lost independent boot binding")
		}
		after := nativeServiceChildren(t, primary.root)
		for role, previous := range beforeChildren {
			if !nativeServiceFreshProcess("windows", previous, after[role]) {
				t.Fatal("native task child restart did not establish fresh identities")
			}
		}
	})
	closed = nativeServiceEndpointClosureProof(t, primary.root)
	stage("persisted_read", func(t *testing.T) { read(t) })
	stage("durable_stop", func(t *testing.T) {
		if _, err := primary.cell.Control(phase(nativeServiceHostBound), "stop"); err != nil {
			t.Fatal("cannot durably stop native task children")
		}
		assertStopped(t, primary.cell, phase(nativeServiceHostBound))
		current, err := primary.cell.ServiceDescriptor()
		if err != nil || current.DesiredState != "stopped" {
			t.Fatal("native task stop did not persist intent")
		}
		if err := starter.Start(phase(nativeServiceHostBound), current); !errors.Is(err, localcell.ErrServiceDesiredState) {
			t.Fatal("native task durable stop did not suppress on demand restart")
		}
	})
	stage("unregister", func(t *testing.T) {
		if err := adapter.Remove(phase(localcell.ServiceHostExitBudget(nativeServiceHostBound)), primary.descriptor); err != nil {
			t.Fatal("cannot remove exact owned native task")
		}
		primary.registered = false
		observation, err := adapter.Inspect(phase(nativeServiceHostBound), primary.descriptor)
		if err != nil || observation.Registered || !observation.Available {
			t.Fatal("native task removal did not prove typed absence")
		}
	})
	stage("resources_closed", func(t *testing.T) {
		closed()
		assertStopped(t, primary.cell, phase(nativeServiceHostBound))
		assertNativeServiceHostRemoved(phase(nativeWindowsTaskClosureBound), t, primary.cell, primary.root)
	})
	stage("bystander_retained", func(t *testing.T) {
		assertReady(t, bystander.cell, phase(nativeServiceHostBound))
		if bystanderHost != nativeServiceHostProcess(t, bystander.root) {
			t.Fatal("native task removal replaced the bystander host")
		}
		for role, previous := range bystanderChildren {
			current := nativeServiceChildren(t, bystander.root)[role]
			if previous.PID != current.PID || previous.StartTime != current.StartTime || previous.BootID != current.BootID {
				t.Fatal("native task removal replaced a bystander child")
			}
		}
		bystanderRead(t)
		for _, name := range []string{"pg", "manifest.json"} {
			if _, err := os.Stat(filepath.Join(primary.root, name)); err != nil {
				t.Fatal("native task unregister lost retained installation data")
			}
		}
	})
	if ctx.Err() != nil {
		t.Fatal("native task manager proof budget exhausted")
	}
	t.Log("PASS service_manager_available")
	t.Log("PASS service_manager_persistence")
	t.Log("PASS service_shutdown_channel")
}
