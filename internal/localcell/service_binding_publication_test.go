package localcell

import (
	"bytes"
	"context"
	"errors"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
)

const (
	bindingStateLost   = "state publication changed listener admission"
	bindingOutsideLost = "outside writer stranded the listener"
	bindingJournalLost = "discovery depended on completing an unrelated write"
	bindingSampleLost  = "sampled state revision blocked a current listener"
)

// Compare the complete records. Unix keeps the same live socket object.
type publicationDiscovery struct {
	lease, endpoint []byte
	socket          os.FileInfo
}

func publicationDiscoveryAt(t *testing.T, c *Cell, text string) publicationDiscovery {
	t.Helper()
	var d publicationDiscovery
	var err error
	d.lease, err = os.ReadFile(c.path("service", "lease.json"))
	if err == nil {
		if runtime.GOOS == "windows" {
			d.endpoint, err = os.ReadFile(c.path("service", "control-endpoint.json"))
		} else {
			d.socket, err = os.Lstat(c.path("service", "control.sock"))
		}
	}
	if err != nil {
		t.Fatal(text)
	}
	return d
}

func (d publicationDiscovery) unchanged(other publicationDiscovery) bool {
	return bytes.Equal(d.lease, other.lease) && bytes.Equal(d.endpoint, other.endpoint) &&
		((d.socket == nil && other.socket == nil) || os.SameFile(d.socket, other.socket))
}

func publicationClient(t *testing.T, c *Cell) *Cell {
	t.Helper()
	client, err := New(c.cfg)
	if err != nil {
		t.Fatal("could not prepare an independent control client")
	}
	return client
}

func publicationObserve(ctx context.Context, t *testing.T, client *Cell, discovery publicationDiscovery, text string) {
	t.Helper()
	v, err := client.Control(ctx, "observe")
	if err != nil || v.Generation == "" || !discovery.unchanged(publicationDiscoveryAt(t, client, text)) {
		t.Fatal(text)
	}
}

func TestServiceBindingPublication(t *testing.T) {
	for _, row := range []string{"start", "stop", "repair_stopped", "repair_complete", "fault", "repeat_fault", "desired_maintenance",
		"external_setter", "external_repair", "journal_only", "descriptor_committed", "postcommit_error", "hold_only"} {
		t.Run(row, func(t *testing.T) { publicationWriteRow(t, row) })
	}
	for i, row := range []string{"preflight_crossed", "dial_crossed"} {
		t.Run(row, func(t *testing.T) { publicationCrossedRow(t, i+1) })
	}
}

func publicationWriteRow(t *testing.T, row string) {
	t.Helper()
	// Select a cut in a real operation. The writer waits until released,
	// including on failure through the deferred release and joined run.
	cut := "retired"
	text := bindingStateLost
	switch row {
	case "external_setter", "external_repair":
		text = bindingOutsideLost
	case "journal_only":
		text, cut = bindingJournalLost, "journal"
	case "descriptor_committed", "postcommit_error":
		text, cut = bindingJournalLost, "descriptor"
	case "hold_only":
		text = bindingJournalLost
	}
	entered, resume := make(chan ServiceDescriptor, 1), make(chan struct{})
	release := sync.OnceFunc(func() { close(resume) })
	var armed atomic.Bool
	watch := func(stage string, d ServiceDescriptor) {
		if stage != cut || ((row == "repair_complete" || row == "external_repair") && d.RepairRevision == 0) {
			return
		}
		if armed.CompareAndSwap(true, false) {
			entered <- d
			<-resume
		}
	}
	host := serviceWorkerConfigured(t, "", false, func(c *Cell, _ ServiceDescriptor) { c.observeServiceDescriptor = watch })
	host.ready(t)
	ctx, cancel := context.WithTimeout(t.Context(), serviceControlBudget(host.cell.cfg.Timeout, 3))
	defer cancel()
	defer release()
	probe := publicationClient(t, host.cell)
	writer := host.cell
	if row == "external_setter" || row == "external_repair" {
		writer = publicationClient(t, host.cell)
		writer.observeServiceDescriptor = watch
	}
	if row == "start" {
		if _, err := probe.Control(ctx, "stop"); err != nil {
			t.Fatal(text)
		}
	}
	if row == "repeat_fault" && writer.setServiceFaulted(ctx, true) != nil {
		t.Fatal(text)
	}
	base, err := probe.ServiceDescriptor()
	if err != nil {
		t.Fatal(text)
	}
	discovery := publicationDiscoveryAt(t, probe, text)
	if row == "hold_only" {
		serviceMaintenanceHostPlant(t, writer, "Valid")
		publicationObserve(ctx, t, probe, discovery, text)
		if !errors.Is(writer.SetServiceDesiredState(ctx, "running"), errMaintenanceFenceActive) {
			t.Fatal(text)
		}
		serviceMaintenanceHostClear(t, writer)
		publicationObserve(ctx, t, probe, discovery, text)
		return
	}
	requester := publicationClient(t, host.cell)
	armed.Store(true)
	done := StartJoinedTestRunForTest(t, cancel, host.cell.cfg.Timeout, func() error {
		switch row {
		case "start", "stop":
			_, err := requester.Control(ctx, row)
			return err
		case "repair_stopped", "repair_complete":
			_, err := requester.Control(ctx, "repair")
			return err
		case "external_repair":
			return writer.RepairService(ctx)
		case "fault", "repeat_fault":
			return writer.setServiceFaulted(ctx, true)
		case "desired_maintenance":
			return writer.SetServiceDesiredState(ctx, "maintenance")
		case "external_setter":
			return writer.SetServiceDesiredState(ctx, "stopped")
		default:
			return writer.SetServiceDesiredState(ctx, "running")
		}
	})
	var pending ServiceDescriptor
	select {
	case pending = <-entered:
	case <-done:
		t.Fatal(text)
	case <-ctx.Done():
		t.Fatal(text)
	}
	targetRevision := base.Revision + 1
	if row == "repair_complete" || row == "external_repair" {
		targetRevision++
	}
	d, err := probe.ServiceDescriptor()
	if err != nil || pending.Revision != targetRevision || (cut != "journal" && d.Revision != pending.Revision) || (cut == "journal" && d.Revision != base.Revision) {
		t.Fatal(text)
	}
	switch row {
	case "start":
		if d.DesiredState != "running" || base.DesiredState != "stopped" {
			t.Fatal(text)
		}
	case "stop", "repair_stopped", "external_setter":
		if d.DesiredState != "stopped" || d.RepairRevision != base.RepairRevision {
			t.Fatal(text)
		}
	case "repair_complete", "external_repair":
		if d.Faulted || d.DesiredState != "stopped" || d.RepairRevision != base.RepairRevision+1 {
			t.Fatal(text)
		}
	case "fault", "repeat_fault":
		if !d.Faulted || (row == "repeat_fault" && !base.Faulted) {
			t.Fatal(text)
		}
	case "desired_maintenance":
		if d.DesiredState != "maintenance" {
			t.Fatal(text)
		}
	}
	journal := probe.path("service", "transition.json")
	if cut == "retired" {
		if _, err := os.Lstat(journal); !errors.Is(err, os.ErrNotExist) {
			t.Fatal(text)
		}
	} else {
		var transition ServiceDescriptor
		if readServiceRecordJSON(ctx, journal, &transition) != nil || transition.Revision != pending.Revision || !errors.Is(probe.serviceStartFence(t.Context()), ErrServiceDesiredState) {
			t.Fatal(text)
		}
	}
	publicationObserve(ctx, t, probe, discovery, text)
	if row == "postcommit_error" {
		// Make the actual retire operation fail after descriptor publication.
		if removePrivateFile(journal) != nil {
			t.Fatal(text)
		}
	}
	release()
	select {
	case err = <-done:
	case <-ctx.Done():
		t.Fatal(text)
	}
	if row == "postcommit_error" {
		var site *serviceFailure
		if !errors.As(err, &site) || site.site != "descriptor_write" {
			t.Fatal(text)
		}
	} else if err != nil {
		t.Fatal(text)
	}
	finalRevision := targetRevision
	if row == "repair_stopped" {
		finalRevision++ // The same repair then publishes its completion.
	}
	d, err = probe.ServiceDescriptor()
	if err != nil || d.Revision != finalRevision {
		t.Fatal(text)
	}
	publicationObserve(ctx, t, publicationClient(t, host.cell), discovery, text)
}

// Windows sends the authenticated request through the selected admission
// pass. Other builds exercise both invocations of the shared binding reader
// against private version-two fixture records, without changing Unix discovery.
func publicationCrossedRow(t *testing.T, pass int) {
	t.Helper()
	var client, writer *Cell
	var read func(context.Context) error
	if runtime.GOOS == "windows" {
		host := serviceWorkerStart(t, "", false)
		host.ready(t)
		writer = host.cell
		client = publicationClient(t, writer)
		read = func(ctx context.Context) error {
			v, err := client.Control(ctx, "observe")
			if err == nil && (!v.Healthy || v.State != "running") {
				return errors.New(bindingSampleLost)
			}
			return err
		}
	} else {
		c, d := descriptorTestCell(t)
		start, alive, err := processIdentity(os.Getpid())
		if err != nil || !alive {
			t.Fatal(bindingSampleLost)
		}
		name, err := newServiceControlPipeName(d.InstallationID)
		if err != nil {
			t.Fatal(bindingSampleLost)
		}
		boot := func() (string, error) { return "fixture-boot", nil }
		lease := serviceLeaseRecord{serviceLeaseIdentity: serviceLeaseIdentity{Version: 2, BootID: "fixture-boot", StartIdentity: start, PID: os.Getpid(), InstallationID: d.InstallationID, Owner: d.UID}}
		if c.publishServiceLease(c.path("service", "lease.json"), lease) != nil || c.completeServiceLease(d, name, boot) != nil {
			t.Fatal(bindingSampleLost)
		}
		if _, err := c.publishServiceControlEndpointWithBoot(name, boot); err != nil {
			t.Fatal(bindingSampleLost)
		}
		writer = c
		client = publicationClient(t, writer)
		read = func(context.Context) error {
			for range 2 {
				if _, err := client.readServiceControlEndpointWithBoot(boot); err != nil {
					return err
				}
			}
			return nil
		}
	}
	base, err := writer.ServiceDescriptor()
	if err != nil {
		t.Fatal(bindingSampleLost)
	}
	// An ordinary complete request precedes the selected call.
	ctx, cancel := context.WithTimeout(t.Context(), serviceControlBudget(client.cfg.Timeout, 4))
	defer cancel()
	if read(ctx) != nil {
		t.Fatal(bindingSampleLost)
	}
	sampled, resume := make(chan ServiceDescriptor, 1), make(chan struct{})
	release := sync.OnceFunc(func() { close(resume) })
	defer release()
	var passes atomic.Int32
	client.observeServiceDescriptor = func(stage string, d ServiceDescriptor) {
		if stage == "sampled" && int(passes.Add(1)) == pass {
			sampled <- d
			<-resume
		}
	}
	done := StartJoinedTestRunForTest(t, cancel, client.cfg.Timeout, func() error { return read(ctx) })
	select {
	case d := <-sampled:
		if d.Revision != base.Revision || writer.SetServiceDesiredState(ctx, "running") != nil {
			t.Fatal(bindingSampleLost)
		}
		now, err := writer.ServiceDescriptor()
		if err != nil || now.Revision != d.Revision+1 {
			t.Fatal(bindingSampleLost)
		}
	case <-done:
		t.Fatal(bindingSampleLost)
	case <-ctx.Done():
		t.Fatal(bindingSampleLost)
	}
	release()
	select {
	case err := <-done:
		if err != nil || passes.Load() != 2 {
			t.Fatal(bindingSampleLost)
		}
	case <-ctx.Done():
		t.Fatal(bindingSampleLost)
	}
}
