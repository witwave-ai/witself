package localcell

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func descriptorTestCell(t *testing.T) (*Cell, ServiceDescriptor) {
	t.Helper()
	c, _ := unitCell(t)
	t.Setenv("WITSELF_RUNTIME", "local")
	host, err := os.Executable()
	if err != nil {
		t.Fatal("test executable unavailable")
	}
	d := ServiceDescriptor{Adapter: "foreground", Payload: ServicePayload{Host: host, Args: []string{"run", "--root", c.cfg.Root}}}
	if err := c.PrepareService(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	d, err = c.ServiceDescriptor()
	if err != nil {
		t.Fatal(err)
	}
	return c, d
}

func TestServiceDescriptorExplicitRuntimeAndImmutablePayload(t *testing.T) {
	c, _ := unitCell(t)
	host, err := os.Executable()
	if err != nil {
		t.Fatal("test executable unavailable")
	}
	d := ServiceDescriptor{Adapter: "foreground", Payload: ServicePayload{Host: host, Args: []string{"run", "--root", c.cfg.Root}}}
	for _, runtime := range []string{"", "hosted"} {
		t.Setenv("WITSELF_RUNTIME", runtime)
		if c.PrepareService(t.Context(), d) == nil {
			t.Fatal("service opted in without local runtime")
		}
		if _, err := os.Lstat(c.path("service")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("refused preparation wrote service state")
		}
	}
	t.Setenv("WITSELF_RUNTIME", "local")
	if err := c.PrepareService(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	before, err := c.serviceControlSecret()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.PrepareService(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	after, err := c.serviceControlSecret()
	if err != nil || before != after {
		t.Fatal("idempotent preparation changed the credential")
	}
	d.Payload.Args = append(d.Payload.Args, "--different")
	if !errors.Is(c.PrepareService(t.Context(), d), ErrServiceOwnershipConflict) {
		t.Fatal("changed immutable payload was adopted")
	}
	for _, name := range []string{"descriptor.json", "control.secret"} {
		if err := servicePrivateRecord(c.path("service", name)); err != nil {
			t.Fatal("service state not owner private")
		}
	}
}

func TestServiceDescriptorBindingAndFences(t *testing.T) {
	c, d := descriptorTestCell(t)
	for _, state := range []string{"stopped", "maintenance"} {
		if err := c.SetServiceDesiredState(t.Context(), state); err != nil {
			t.Fatal(err)
		}
		fresh, err := New(c.cfg)
		if err != nil {
			t.Fatal(err)
		}
		if !errors.Is(fresh.Start(t.Context()), ErrServiceDesiredState) {
			t.Fatal("fresh host bypassed durable desired state")
		}
	}
	if err := c.SetServiceDesiredState(t.Context(), "stopped"); err != nil {
		t.Fatal(err)
	}
	if err := c.SetServiceDesiredState(t.Context(), "running"); err != nil {
		t.Fatal(err)
	}
	if err := c.setServiceFaulted(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(c.SetServiceDesiredState(t.Context(), "running"), ErrServiceDesiredState) {
		t.Fatal("start cleared circuit breaker without repair")
	}
	d.InstallationID = "foreign"
	if err := writeAtomic(c.path("service", "descriptor.json"), d, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ServiceDescriptor(); !errors.Is(err, ErrServiceOwnershipConflict) {
		t.Fatal("foreign installation descriptor accepted")
	}
}

func TestServiceDescriptorInterruptedTransitionFailsClosed(t *testing.T) {
	c, d := descriptorTestCell(t)
	d.DesiredState, d.Revision = "maintenance", d.Revision+1
	if err := writeAtomic(c.path("service", "transition.json"), d, nil); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(c.Start(t.Context()), ErrServiceDesiredState) {
		t.Fatal("unacknowledged maintenance transition permitted launch")
	}
	if !errors.Is(c.SetServiceDesiredState(t.Context(), "running"), ErrServiceDesiredState) {
		t.Fatal("explicit start erased pending maintenance")
	}
	s := &supervisor{cell: c}
	if _, err := s.command(t.Context(), "start"); !errors.Is(err, ErrServiceDesiredState) {
		t.Fatal("control start erased pending maintenance")
	}
	var pending ServiceDescriptor
	if err := readJSON(c.path("service", "transition.json"), &pending); err != nil || pending.DesiredState != "maintenance" {
		t.Fatal("refused start mutated maintenance transition")
	}
	if err := c.SetServiceDesiredState(t.Context(), "stopped"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(c.path("service", "transition.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("explicit stopped recovery left transition pending")
	}
}

func TestServiceOwnerAndHostIdentityRefusals(t *testing.T) {
	for _, tc := range []struct {
		uid       string
		effective int
		want      bool
	}{{"501", 501, true}, {"0", 0, false}, {"501", 0, false}, {"501", 502, false}, {"S-1-fixture", -1, true}} {
		if validServiceOwner(tc.uid, tc.effective) != tc.want {
			t.Fatal("elevated or different effective owner accepted")
		}
	}
	c, d := descriptorTestCell(t)
	if err := validateServiceHost(d); err != nil {
		t.Fatal(err)
	}
	d.Payload.Host = c.path("manifest.json")
	if !errors.Is(validateServiceHost(d), ErrServiceOwnershipConflict) {
		t.Fatal("different executable accepted as immutable host")
	}
}

func TestServiceLegacyRemoveEstablishesStoppedFence(t *testing.T) {
	c, _ := descriptorTestCell(t)
	if err := c.Remove(t.Context(), RemoveOptions{}); err != nil {
		t.Fatal(err)
	}
	d, err := c.ServiceDescriptor()
	if err != nil || d.DesiredState != "stopped" {
		t.Fatal("legacy removal left automatic restart enabled")
	}
}

func TestServiceOldBootPIDReuseDiscardsWithoutSignaling(t *testing.T) {
	c, _ := descriptorTestCell(t)
	if err := privateDir(c.path("pg"), true); err != nil {
		t.Fatal(err)
	}
	// The current test process is a live bystander with the recorded PID, but
	// every stored identity belongs to a different boot. No signaling is safe.
	pid := os.Getpid()
	before, alive, err := processIdentity(pid)
	if err != nil || !alive {
		t.Fatal("bystander unavailable")
	}
	for _, component := range []string{"postgres", "server", "worker"} {
		r := ProcessRecord{PID: pid, StartTime: "linux:old-boot:7", Root: c.cfg.Root, Component: component}
		if err := writeAtomic(c.path("run", component+".json"), r, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeAtomicBytes(c.path("pg", "postmaster.pid"), []byte(fmt.Sprintf("%d\n%s\n1\n", pid, c.path("pg"))), nil); err != nil {
		t.Fatal(err)
	}
	if err := c.reconcileOldBoot("linux:old-boot", "linux:new-boot"); err != nil {
		t.Fatal(err)
	}
	after, alive, err := processIdentity(pid)
	if err != nil || !alive || after != before {
		t.Fatal("old boot reconciliation signaled a bystander")
	}
	for _, name := range []string{"run/postgres.json", "run/server.json", "run/worker.json", "pg/postmaster.pid"} {
		if _, err := os.Lstat(filepath.Join(c.cfg.Root, filepath.FromSlash(name))); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("old boot record retained")
		}
	}
}

func TestServiceOldBootRejectsMixedOwnershipBeforeMutation(t *testing.T) {
	c, _ := descriptorTestCell(t)
	for _, component := range []string{"postgres", "server"} {
		start := "linux:old-boot:7"
		if component == "server" {
			start = "linux:new-boot:7"
		}
		if err := writeAtomic(c.path("run", component+".json"), ProcessRecord{PID: os.Getpid(), StartTime: start, Root: c.cfg.Root, Component: component}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if !errors.Is(c.reconcileOldBoot("linux:old-boot", "linux:new-boot"), ErrServiceOwnershipConflict) {
		t.Fatal("mixed boot set adopted")
	}
	if _, err := os.Stat(c.path("run", "postgres.json")); err != nil {
		t.Fatal("reconciliation mutated before validating full set")
	}
}
