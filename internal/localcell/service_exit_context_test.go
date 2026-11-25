//go:build linux

package localcell

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func requireExitLeaseCut(t *testing.T, err, cause error) {
	t.Helper()
	requireServiceLease(t, serviceLeaseCut(err), "service lease read lost its cut")
	requireServiceLease(t, errors.Is(err, cause), "service lease read lost its context cause")
	for _, ordinary := range []error{ErrServiceControlRefused, ErrServiceOwnershipConflict, ErrServiceHostActive, ErrLocked, os.ErrNotExist, unix.EWOULDBLOCK} {
		requireServiceLease(t, !errors.Is(err, ordinary), "interrupted service read classified the host or tree")
	}
}

func requireExitPhase(t *testing.T, err error, phase string, accepted bool) {
	t.Helper()
	var exit *serviceHostExitError
	requireServiceLease(t, errors.As(err, &exit), "exit lost its phase diagnostic")
	requireServiceLease(t, exit.phase == phase, "exit poll lost its phase")
	requireServiceLease(t, exit.accepted == accepted, "exit changed the accepted disposition")
}

func TestLeaseServiceExitAndCache(t *testing.T) {
	t.Run("exit_poll", leaseExitAfterACK)
	for _, row := range []struct{ name, file, phase string }{
		{"host_lock", ".service-host.lock", ""}, {"lifetime", ".service-host.lock", ""},
		{"identity_descriptor", "service/descriptor.json", "identity_tree"}, {"identity_record", "service/lease.json", "lease_validate"},
		{"accept", "service/lease.json", "lease_validate"}, {"poll_lock", ".service-host.lock", "lease_lock"},
		{"poll_child", "run/worker.json", "observe"},
	} {
		t.Run(row.name, func(t *testing.T) {
			c, d := descriptorTestCell(t)
			lock, err := c.acquireServiceLease(t.Context(), d)
			requireServiceLease(t, err == nil, "cannot seed the lifetime lease")
			requireServiceLease(t, lock.Close() == nil, "cannot release the initial host")
			envelope, lease, err := c.newHostExitEnvelope(t.Context())
			requireServiceLease(t, err == nil, "cannot seed the exit identity")
			observer, err := c.retainServiceExitHost(t.Context(), lease)
			requireServiceLease(t, err == nil, "cannot retain the fixture process")
			defer observer.close()
			if row.name == "poll_child" {
				requireServiceLease(t, writeAtomic(c.path(row.file), ProcessRecord{}, nil) == nil, "cannot seed the unresolved child")
			}
			mode := unix.F_WRLCK
			if row.file == ".service-host.lock" {
				mode = unix.F_RDLCK
			}
			hostInfo, err := os.Stat(c.path(".service-host.lock"))
			requireServiceLease(t, err == nil, "cannot identify the lifetime lock")
			recordInfo, err := os.Stat(c.path("service", "lease.json"))
			requireServiceLease(t, err == nil, "cannot identify the initial host record")
			holder := leaseTaken(t, leaseHolder(t, "retaken", mode), c.path(row.file))
			leaseWait.ceiling = 2 * time.Second
			ctx, cancel := context.WithTimeout(t.Context(), leaseWait.ceiling/8)
			defer cancel()
			deadline, _ := ctx.Deadline()
			switch row.name {
			case "host_lock":
				var held bool
				held, err = c.serviceHostLeaseHeld(ctx)
				requireServiceLease(t, !held, "interrupted host probe claimed a competing owner")
			case "lifetime":
				lock, err = c.acquireServiceLease(ctx, d)
				if lock != nil {
					_ = lock.Close()
					t.Fatal("interrupted lifetime admission returned a lock")
				}
			case "accept":
				s := newSupervisor(c, SupervisorPolicy{})
				var status ServiceStatus
				status, err = s.acceptHostExit(context.WithValue(ctx, serviceHostExitKey{}, envelope))
				requireServiceLease(t, status.HostExit == "", "interrupted revalidation acknowledged exit")
				requireServiceLease(t, s.status.HostExit == "", "interrupted revalidation changed host disposition")
			case "poll_lock", "poll_child":
				err = c.waitServiceHostExit(ctx, lease, observer)
			default:
				var status ServiceStatus
				status, err = c.ExitServiceHost(ctx)
				requireServiceLease(t, status.HostExit == "", "interrupted preflight acknowledged exit")
			}
			elapsed := time.Since(deadline)
			requireServiceLease(t, holder.broken(), "exit did not reach the held lease")
			holder.close()
			requireExitLeaseCut(t, err, context.DeadlineExceeded)
			requireServiceLease(t, elapsed < leaseWait.ceiling/2, "exit returned too late after its deadline")
			if row.phase != "" {
				requireExitPhase(t, err, row.phase, row.name == "poll_lock" || row.name == "poll_child")
			}
			after, err := os.Stat(c.path("service", "lease.json"))
			requireServiceLease(t, err == nil, "interrupted exit removed the host record")
			requireServiceLease(t, os.SameFile(recordInfo, after), "interrupted exit republished the host record")
			after, err = os.Stat(c.path(".service-host.lock"))
			requireServiceLease(t, err == nil, "interrupted exit removed the host lock")
			requireServiceLease(t, os.SameFile(hostInfo, after), "interrupted exit replaced the host lock")
			held, err := c.serviceHostLeaseHeld(t.Context())
			requireServiceLease(t, err == nil, "cannot probe the host after the cut")
			requireServiceLease(t, !held, "interrupted operation retained the lifetime lock")
		})
	}
}

func leaseExitAfterACK(t *testing.T) {
	c, commands := hostExitSocketFixture(t)
	ctx, hold, finish := adapterOperationLease(t, "kept", unix.F_WRLCK, false)
	client, cancel := context.WithCancel(ctx)
	var status ServiceStatus
	done := StartJoinedTestRunForTest(t, cancel, c.cfg.Timeout, func() (err error) {
		status, err = c.ExitServiceHost(client)
		return err
	})
	select {
	case command := <-commands:
		// Admission has validated the signed challenge. Only the client's
		// revalidation after the accepted reply can break this lease.
		hold(c.path("service", "lease.json"))
		command.reply <- serviceReply{}
		command.completed <- "drained"
	case <-time.After(c.cfg.Timeout):
		t.Fatal("signed exit did not reach admission")
	}
	err := <-done
	finish()
	requireExitLeaseCut(t, err, context.Canceled)
	requireExitPhase(t, err, "lease_validate", true)
	requireServiceLease(t, status.HostExit == "accepted", "accepted exit was reported as complete")
	held, err := c.serviceHostLeaseHeld(t.Context())
	requireServiceLease(t, err == nil, "cannot recheck accepted host")
	requireServiceLease(t, held, "accepted response released the host lease")
}

func TestExitRemovalLease(t *testing.T) {
	for _, row := range []struct {
		name, path string
		mode       int
		fenced     bool
	}{
		{"descriptor", "service/descriptor.json", unix.F_WRLCK, false}, {"probe", ".service-host.lock", unix.F_RDLCK, false},
		{"owner", ".owner.lock", unix.F_RDLCK, true}, {"operation", "run/operation.lock", unix.F_RDLCK, true},
		{"recheck", ".service-host.lock", unix.F_RDLCK, true}, {"child", "run/worker.json", unix.F_WRLCK, true},
		{"postmaster", "pg/postmaster.pid", unix.F_WRLCK, true},
	} {
		t.Run(row.name, func(t *testing.T) {
			c, d := descriptorTestCell(t)
			serviceRemovalSystemdHostFile(t, c)
			if row.name == "postmaster" {
				requireServiceLease(t, os.Mkdir(c.path("pg"), 0o700) == nil, "cannot prepare the postmaster directory")
			}
			if row.name == "child" || row.name == "postmaster" {
				requireServiceLease(t, os.WriteFile(c.path(row.path), nil, 0o600) == nil, "cannot plant removal evidence")
			}
			ctx, hold, finish := adapterOperationLease(t, "kept", row.mode, false)
			if row.fenced {
				// The writer still holds the operation lock when it retires
				// the transition. Arm the open on the first subsequent walk.
				c.observeServiceDescriptor = func(stage string, _ ServiceDescriptor) {
					if stage == "retired" {
						c.observeRootWalk = func(bool) {
							c.observeRootWalk = nil
							hold(c.path(row.path))
						}
					}
				}
			} else {
				hold(c.path(row.path))
			}
			err := c.exitServiceHostForRemoval(ctx)
			finish()
			requireExitLeaseCut(t, err, context.Canceled)
			after, err := c.ServiceDescriptor()
			requireServiceLease(t, err == nil, "cannot read stopped intent after removal cut")
			if row.fenced {
				requireServiceLease(t, after.DesiredState == "stopped", "removal lost its committed stopped intent")
				requireServiceLease(t, after.Revision == d.Revision+1, "removal repeated or lost the stopped fence")
			} else {
				requireServiceLease(t, after.DesiredState == d.DesiredState, "failed probe published stopped intent")
				requireServiceLease(t, after.Revision == d.Revision, "failed probe republished the descriptor")
			}
			_, err = os.Stat(c.path(row.path))
			requireServiceLease(t, err == nil, "removal erased unresolved evidence")
			serviceRemovalLocksFree(t, c)
		})
	}
}

func TestExitEnvelopeAdmissionLease(t *testing.T) {
	c, commands := hostExitSocketFixture(t, 150*time.Millisecond)
	delivered := hostExitHealthyResponder(t, commands)
	envelope, _, err := c.newHostExitEnvelope(t.Context())
	requireServiceLease(t, err == nil, "cannot prepare signed envelope")
	body, err := json.Marshal(envelope)
	requireServiceLease(t, err == nil, "cannot encode signed envelope")
	h := leaseTaken(t, leaseHolder(t, "kept", unix.F_WRLCK), c.path("service", "lease.json"))
	leaseWait.ceiling = 2 * time.Second
	started := time.Now()
	code, _, detail := hostExitRawRequest(t, c, body)
	elapsed := time.Since(started)
	requireServiceLease(t, h.broken(), "authenticated admission missed the leased identity")
	h.close()
	requireServiceLease(t, elapsed < leaseWait.ceiling/2, "authenticated admission outlived its own bound")
	// The cut is the admission's own deadline, answered as at the enqueue and
	// the reply; an invalid envelope keeps its 409 (TestServiceHostExitEnvelopeUnix).
	requireServiceLease(t, code == http.StatusRequestTimeout, "admission answered its own cut as an envelope refusal")
	requireServiceLease(t, detail == serviceHostExitAdmissionTimeout, "admission changed its timeout detail")
	requireServiceLease(t, delivered.Load() == 0, "failed signed admission delivered the exit")
	code, receipt, _ := hostExitRawRequest(t, c, body)
	requireServiceLease(t, code == http.StatusOK, "failed validation consumed the signed challenge")
	requireServiceLease(t, receipt.Disposition == "accepted", "fresh validation did not acknowledge exit")
}

func TestExitDrainDiagnosticLease(t *testing.T) {
	c, _ := descriptorTestCell(t)
	path := c.path("journal", "operations.jsonl")
	requireServiceLease(t, c.journal(t.Context(), "stop", "operation", "started") == nil, "cannot seed pending diagnostic")
	ctx, hold, finish := adapterOperationLease(t, "kept", unix.F_WRLCK, false)
	hooks, _, _, _, _ := supervisorTestHooks()
	stops := 0
	hooks.stop = func(context.Context) error { stops++; hold(path); return nil }
	s := newSupervisor(c, SupervisorPolicy{hooks: &hooks})
	s.running, s.status.HostExit = true, "accepted"
	output := serviceControlJoinCapture(t)
	delivered := make(chan struct{})
	close(delivered)
	exited := s.finishHostExit(ctx, delivered)
	finish()
	requireServiceLease(t, exited, "diagnostic cut reversed completed drain")
	requireServiceLease(t, s.exitDrained, "diagnostic cut lost drain completion")
	requireServiceLease(t, !s.running, "diagnostic cut revived the host tree")
	requireServiceLease(t, s.exitDrainResult == nil, "diagnostic cut replaced successful stop")
	requireServiceLease(t, s.status.HostExit == "accepted", "diagnostic cut changed accepted disposition")
	requireServiceLease(t, stops == 1, "diagnostic cut repeated the child stop")
	requireServiceLease(t, output() == "service_shutdown phase=control operation=none journal_pending=false journal_read_ok=false\n", "cut diagnostic reported a readable journal")
	progress, err := c.readJournal(t.Context())
	requireServiceLease(t, err == nil, "cannot read diagnostic journal after release")
	requireServiceLease(t, progress.pending != nil, "cut diagnostic erased the pending operation")
}

func TestExitLifetimeForeignCut(t *testing.T) {
	for _, phase := range []string{"live", "ended"} {
		t.Run(phase, func(t *testing.T) {
			c, _ := descriptorTestCell(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			foreign := &leaseWaitCut{cause: context.DeadlineExceeded}
			refusal := errors.New("synthetic refusal")
			h, _, _, _, _ := supervisorTestHooks()
			h.boot = func() (string, error) {
				if phase == "ended" {
					cancel()
				}
				return "", foreign
			}
			requireServiceLease(t, c.Run(ctx, SupervisorPolicy{hooks: &h}) == foreign, "host consumed a boot cut from another operation")
			for _, err := range []error{foreign, context.Canceled, errors.Join(foreign, context.Canceled)} {
				requireServiceLease(t, serviceContextReadFailure(ctx, err, refusal) == refusal, "exit treated a foreign cut as the operation ending")
			}
		})
	}
}

// Run stopped at the kernel's break of a lease on its lifetime lock stops
// cleanly: no flock, no boot read, no tree hook, no record or endpoint written.
// While Run is live, the lease kept to the ceiling keeps the fixed refusal.
func TestExitRunLifetimeLease(t *testing.T) {
	for _, mode := range []string{"cancelled", "live"} {
		t.Run(mode, func(t *testing.T) {
			c, d := descriptorTestCell(t)
			lock, err := c.acquireServiceLease(t.Context(), d)
			requireServiceLease(t, err == nil, "cannot seed the lifetime lease")
			requireServiceLease(t, lock.Close() == nil, "cannot release the seeded host")
			record, err := os.Stat(c.path("service", "lease.json"))
			requireServiceLease(t, err == nil, "cannot identify the seeded host record")
			h, _, starts, stops, _ := supervisorTestHooks()
			boots, repairs := 0, 0
			h.boot = func() (string, error) { boots++; return serviceBootID() }
			h.repair = func(context.Context) error { repairs++; return nil }
			holder := leaseTaken(t, leaseHolder(t, "kept", unix.F_RDLCK), c.path(".service-host.lock"))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			broken := make(chan time.Time, 1)
			if mode == "cancelled" {
				leaseWait.ceiling = 2 * time.Second
				go func() { at, _ := leaseContextBroken(holder); cancel(); broken <- at }()
			}
			err = c.Run(ctx, SupervisorPolicy{hooks: &h})
			returned := time.Now()
			requireServiceLease(t, holder.broken(), "host never opened its leased lifetime lock")
			holder.close()
			if mode == "cancelled" {
				requireServiceLease(t, err == nil, "host did not stop cleanly at its own lifetime cut")
			} else {
				requireServiceLease(t, err != nil && err.Error() == "localcell: service_lease_unavailable", "live host lost its lifetime refusal")
			}
			requireServiceLease(t, boots == 0, "host read its boot without the lifetime lock")
			requireServiceLease(t, *starts == 0, "host started the tree without the lifetime lock")
			requireServiceLease(t, *stops == 0, "host stopped the tree without the lifetime lock")
			requireServiceLease(t, repairs == 0, "host repaired the tree without the lifetime lock")
			after, err := os.Stat(c.path("service", "lease.json"))
			requireServiceLease(t, err == nil, "host removed its lease record")
			requireServiceLease(t, os.SameFile(record, after), "host republished its lease record")
			_, err = os.Lstat(c.path("service", "control-endpoint.json"))
			requireServiceLease(t, errors.Is(err, os.ErrNotExist), "host published its control endpoint")
			held, err := c.serviceHostLeaseHeld(t.Context())
			requireServiceLease(t, err == nil, "cannot probe the host after Run")
			requireServiceLease(t, !held, "host kept the lifetime lock")
			if mode == "cancelled" {
				requireServiceLease(t, returned.Sub(<-broken) < leaseWait.ceiling/2, "host waited past half the lease ceiling after its stop")
			}
		})
	}
}

// Cancel the outer operation on its first check after the observation read.
// The reader sees only the real, shorter deadline in its derived context.
type exitPollContext struct {
	context.Context
	holder *writerLease
	cancel context.CancelFunc
}

func (ctx exitPollContext) Err() error {
	if ctx.holder.broken() {
		ctx.cancel()
	}
	return ctx.Context.Err()
}

func TestExitPollDeadlineThenCancel(t *testing.T) {
	c, d := descriptorTestCell(t)
	lock, err := c.acquireServiceLease(t.Context(), d)
	requireServiceLease(t, err == nil, "cannot seed the observed host")
	defer func() { _ = lock.Close() }()
	_, lease, err := c.newHostExitEnvelope(t.Context())
	requireServiceLease(t, err == nil, "cannot seed the observation identity")
	holder := leaseTaken(t, leaseHolder(t, "kept", unix.F_WRLCK), c.path("service", "control.secret"))
	leaseWait.ceiling = 2 * time.Second
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	err = c.waitServiceHostExit(exitPollContext{ctx, holder, cancel}, lease, &unixServiceExitHost{lease})
	requireServiceLease(t, holder.broken(), "observation missed the held lease")
	requireServiceLease(t, !serviceLeaseCut(err), "a shorter observation cut ended the outer operation")
	requireServiceLease(t, errors.Is(err, context.Canceled), "observation hid the subsequent outer cancellation")
	requireExitPhase(t, err, "observe", true)
}

func TestExitRemovalAfterStopLease(t *testing.T) {
	c, commands := hostExitSocketFixture(t)
	initial, err := c.ServiceDescriptor()
	requireServiceLease(t, err == nil, "cannot read removal intent")
	h, _, _, stops, _ := supervisorTestHooks()
	s := newSupervisor(c, SupervisorPolicy{hooks: &h})
	requireServiceLease(t, s.load(t.Context(), initial) == nil, "cannot load the removal supervisor")
	ctx, hold, finish := adapterOperationLease(t, "kept", unix.F_WRLCK, false)
	client, cancel := context.WithCancel(ctx)
	done := StartJoinedTestRunForTest(t, cancel, c.cfg.Timeout, func() error { return c.exitServiceHostForRemoval(client) })
	select {
	case command := <-commands:
		requireServiceLease(t, command.action == "stop", "removal skipped its authenticated stop")
		status, stopErr := s.command(command.ctx, "stop")
		requireServiceLease(t, stopErr == nil, "removal failed before its first stop")
		hold(c.path("service", "lease.json"))
		command.reply <- serviceReply{status: status}
	case <-time.After(c.cfg.Timeout):
		t.Fatal("removal did not request stop")
	}
	err = <-done
	finish()
	requireExitLeaseCut(t, err, context.Canceled)
	requireExitPhase(t, err, "lease_validate", false)
	requireServiceLease(t, *stops == 1, "interrupted removal repeated its stop")
	after, err := c.ServiceDescriptor()
	requireServiceLease(t, err == nil, "cannot read removal fence")
	requireServiceLease(t, after.DesiredState == "stopped", "unacknowledged exit lost stopped intent")
	requireServiceLease(t, after.Revision == initial.Revision+1, "unacknowledged exit changed the stopped revision")
	requireServiceLease(t, s.status.HostExit == "", "unacknowledged exit changed the host disposition")
}
