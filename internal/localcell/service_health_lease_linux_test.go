package localcell

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Each read ServiceHealth and the installer's health credential thread holds a
// kept lease. The operation cancelled at the kernel's break returns its typed
// cut, no verdict and nothing written; a live operation keeps the parent's
// refusal at the ceiling; a released lease completes the ordinary read. The
// credential and final-tree reads are held after database (and API) authorisation.
// The manifest row meets the descriptor's identity read, the first open of that
// file; the start fence, the second manifest read and the tree's record loop
// reopen files already read and are pinned by the lease contract instead.
func TestServiceHealthLease(t *testing.T) {
	for _, row := range []struct {
		name, file string
		reads, api int
		live       error
	}{
		{"lock", "run/operation.lock", 0, 0, errServiceHealth}, {"descriptor", "service/descriptor.json", 0, 0, errServiceHealth},
		{"first_tree", "pg/postmaster.pid", 0, 0, errServiceHealth}, {"descriptor_manifest", "manifest.json", 0, 0, errServiceHealth},
		{"maintenance", "journal/maintenance.json", 0, 0, errMaintenanceFenceActive}, {"principal", "run/postgres.password", 0, 0, errServiceHealth},
		{"credential", "service/api.secret", 1, 0, errServiceHealth}, {"final_tree", "pg/postmaster.pid", 1, 1, errServiceHealth},
		{"installer_principal", "run/postgres.password", 0, 0, errServiceHealth}, {"installer_credential", "service/api.secret", 1, 0, errServiceHealth},
	} {
		t.Run(row.name, func(t *testing.T) {
			c, _ := serviceMaintenanceHostHealthCell(t)
			mode := unix.F_WRLCK
			if row.name == "lock" {
				mode = unix.F_RDLCK
			}
			holder := leaseHolder(t, "kept", mode)
			f := plantHealthLease(t, c, holder, row.name == "final_tree")
			run := c.ServiceHealth
			if strings.HasPrefix(row.name, "installer_") {
				run = func(ctx context.Context) error { return c.prepareSelectedHealthCredential(ctx, f.m) }
			}
			released := error(nil)
			if row.name == "maintenance" {
				serviceMaintenanceHostPlant(t, c, "Valid")
				released = errMaintenanceFenceActive
			}
			snapshot := healthLeaseFiles(t, c)
			arm := func() {
				if row.name == "final_tree" {
					f.arm.Store(true)
				} else {
					leaseTaken(t, holder, c.path(row.file))
				}
			}
			c.cfg.Timeout = 4 * time.Second

			leaseWait.ceiling = 2 * time.Second
			reads, api := f.counts()
			arm()
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- run(ctx) }()
			at, broken := leaseContextBroken(holder)
			cancel()
			err := <-done
			returned := time.Since(at)
			holder.close()
			requireServiceLease(t, broken, "health never broke the selected lease")
			requireExitLeaseCut(t, err, context.Canceled)
			requireServiceLease(t, !errors.Is(err, errServiceHealth), "interrupted health reported a verdict")
			requireServiceLease(t, !errors.Is(err, errMaintenanceFenceActive), "interrupted health reported maintenance")
			requireServiceLease(t, returned < leaseWait.ceiling/2, "health waited to the lease ceiling after cancellation")
			nowReads, nowAPI := f.counts()
			requireServiceLease(t, nowReads-reads == row.reads, "cut changed the database authorisations before the read")
			requireServiceLease(t, nowAPI-api == row.api, "cut changed the API authorisations before the read")
			requireServiceLease(t, reflect.DeepEqual(healthLeaseFiles(t, c), snapshot), "cut health changed the cell")

			leaseWait.ceiling = 200 * time.Millisecond
			reads, api = f.counts()
			arm()
			started := time.Now()
			err = run(t.Context())
			elapsed := time.Since(started)
			holder.close()
			requireServiceLease(t, errors.Is(err, row.live), "live health lost the parent's ceiling refusal")
			requireServiceLease(t, !serviceLeaseCut(err), "live health reported a cut")
			requireServiceLease(t, elapsed >= leaseWait.ceiling/2, "live health refused before its lease budget")
			nowReads, nowAPI = f.counts()
			requireServiceLease(t, nowReads-reads == row.reads, "live refusal changed the database authorisations")
			requireServiceLease(t, nowAPI-api == row.api, "live refusal changed the API authorisations")
			requireServiceLease(t, reflect.DeepEqual(healthLeaseFiles(t, c), snapshot), "live refusal changed the cell")

			requireServiceLease(t, errors.Is(run(t.Context()), released), "released lease did not complete the ordinary health read")
		})
	}
}

type healthLeaseFixture struct {
	m      *Manifest
	wire   *installerMaintenanceWire
	whoami *atomic.Int32
	arm    *atomic.Bool
}

func (f healthLeaseFixture) counts() (int, int) {
	f.wire.mu.Lock()
	defer f.wire.mu.Unlock()
	return f.wire.healthReads, int(f.whoami.Load())
}

// healthLeaseFiles reads what a health read may never write behind a cut.
func healthLeaseFiles(t *testing.T, c *Cell) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	for _, name := range []string{"manifest.json", "service/descriptor.json", "service/api.secret", "run/postgres.password", "pg/postmaster.pid"} {
		data, err := os.ReadFile(c.path(name))
		requireServiceLease(t, err == nil, "cannot read the health fixture state")
		files[name] = bytes.Clone(data)
	}
	return files
}

// plantHealthLease routes the health fixture to a test-only PostgreSQL wire and
// three loopback HTTP servers, with a cached API identity. When final is set,
// an armed worker readiness probe takes the lease on the postmaster record, so
// it is held after both authorisations and before the final tree check.
func plantHealthLease(t *testing.T, c *Cell, holder *writerLease, final bool) healthLeaseFixture {
	t.Helper()
	m, err := readManifest(t.Context(), c.cfg.Root)
	requireServiceLease(t, err == nil, "cannot read health fixture manifest")
	requireServiceLease(t, c.makePassword() == nil, "cannot prepare health fixture principal")
	credential, err := c.serviceAPICredential(t.Context(), &installerHealthStore{}, m)
	requireServiceLease(t, err == nil, "cannot prepare cached health identity")
	// The existing test-only PostgreSQL wire speaks pgx's simple protocol.
	profile := filepath.Join(t.TempDir(), "pg_service.conf")
	requireServiceLease(t, os.WriteFile(profile, []byte("[health]\ndefault_query_exec_mode=simple_protocol\n"), 0600) == nil, "cannot select the health fixture wire protocol")
	t.Setenv("PGSERVICE", "health")
	t.Setenv("PGSERVICEFILE", profile)
	f := healthLeaseFixture{m: m, wire: newInstallerMaintenanceWire(t), whoami: new(atomic.Int32), arm: new(atomic.Bool)}
	f.wire.mu.Lock()
	f.wire.healthAccount = m.AccountID
	f.wire.mu.Unlock()
	address, err := net.ResolveTCPAddr("tcp", f.wire.endpoint)
	requireServiceLease(t, err == nil, "cannot locate the health fixture store")
	m.Ports.Postgres = address.Port
	server := func(handler http.HandlerFunc) int {
		srv := httptest.NewServer(handler)
		t.Cleanup(srv.Close)
		return srv.Listener.Addr().(*net.TCPAddr).Port
	}
	m.Ports.ServerAPI = server(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/auth/whoami" || r.URL.Query().Get("challenge") == "" || r.Header.Get("Authorization") != "Bearer "+credential.Bearer {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f.whoami.Add(1)
		_, _ = fmt.Fprintf(w, `{"principal":{"kind":"operator","account_id":%q,"operator_id":%q}}`, credential.AccountID, credential.OperatorID)
	})
	m.Ports.ServerHealth = server(func(http.ResponseWriter, *http.Request) {})
	m.Ports.WorkerHealth = server(func(w http.ResponseWriter, _ *http.Request) {
		if final && f.arm.Swap(false) {
			if holder.take(c.path("pg", "postmaster.pid")); !holder.do(func(uintptr) {}) {
				t.Error("this kernel or sandbox refused the final health lease")
				w.WriteHeader(http.StatusServiceUnavailable)
			}
		}
	})
	requireServiceLease(t, writeAtomic(c.path("manifest.json"), m, nil) == nil, "cannot route health to the test servers")
	return f
}

// Both supervisor health branches return the operation's own cut before the
// circuit writes; a health deadline that cut the read while the operation was
// live keeps the ordinary failure accounting, and the same cut after the
// operation ended keeps the parent's stop-refusal path.
func TestSupervisorHealthLeaseCut(t *testing.T) {
	for _, phase := range []struct {
		name   string
		starts int
	}{{"running", 0}, {"post_start", 1}} {
		for _, end := range []string{"operation", "hook_deadline", "hook_deadline_then_stop"} {
			t.Run(phase.name+"/"+end, func(t *testing.T) {
				f := newServiceMaintenanceHostFixture(t)
				s := f.s
				s.running = phase.starts == 0
				s.maintenanceHeld, s.maintenanceDisplay = true, true
				prior := s.status
				requireServiceLease(t, s.save() == nil, "cannot seed the durable health circuit")
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				holder := leaseHolder(t, "kept", unix.F_RDLCK)
				leaseWait.ceiling = 2 * time.Second
				f.cell.cfg.Timeout = leaseWait.ceiling / 20
				if end == "operation" {
					f.cell.cfg.Timeout = leaseWait.ceiling
				}
				var healthErr error
				s.hooks.health = func(healthCtx context.Context) error {
					leaseTaken(t, holder, f.cell.path("run", "operation.lock"))
					defer holder.close()
					if end == "operation" {
						cancelAtServiceLeaseBreak(t, holder, cancel)
					}
					healthErr = f.cell.ServiceHealth(healthCtx)
					if end == "hook_deadline_then_stop" {
						cancel()
					}
					// A sibling carrying the operation's cause must not make a foreign cut its own.
					return errors.Join(healthErr, context.Canceled)
				}
				err := s.step(ctx)

				want := context.DeadlineExceeded
				if end == "operation" {
					want = context.Canceled
				}
				requireExitLeaseCut(t, healthErr, want)
				requireServiceLease(t, *f.starts == phase.starts, "health changed the admitted starts")
				requireServiceLease(t, s.record.Attempts == phase.starts, "health changed the admitted attempts")
				var disk supervisorRecord
				requireServiceLease(t, readJSON(t.Context(), f.cell.path("service", "supervisor.json"), &disk) == nil, "cannot read the durable health circuit")
				requireServiceLease(t, reflect.DeepEqual(disk, s.record), "health circuit memory diverged from disk")
				after, readErr := f.cell.ServiceDescriptor()
				requireServiceLease(t, readErr == nil, "health left an unreadable descriptor")
				switch end {
				case "operation":
					requireServiceLease(t, serviceLeaseCut(err), "step lost the operation's own health cut")
					requireServiceLease(t, errors.Is(err, context.Canceled), "step lost its own cut's cause")
					requireServiceLease(t, reflect.DeepEqual(s.status, prior), "cut health replaced the last completed status")
					requireServiceLease(t, s.maintenanceHeld, "cut health lost the previous hold")
					requireServiceLease(t, s.maintenanceDisplay, "cut health lost the previous hold display")
					requireServiceLease(t, s.running, "cut health forgot the started tree")
				case "hook_deadline":
					requireServiceLease(t, err == nil, "foreign health deadline ended the step")
					requireServiceLease(t, s.status.State == "backoff", "independent health deadline lost its backoff")
					requireServiceLease(t, !s.running, "stopped unhealthy tree remained running")
				default:
					requireServiceLease(t, err == nil, "ended foreign deadline returned an error")
					requireServiceLease(t, s.status.Reason == "service_stop_refused", "ended foreign deadline lost its stop refusal")
					requireServiceLease(t, s.running == (phase.starts == 0), "ended foreign deadline changed the running bit")
				}
				stops, failures := 0, 0
				if end == "hook_deadline" {
					stops, failures = 1, 1
				}
				requireServiceLease(t, *f.stops == stops, "health changed the stop hook's count")
				requireServiceLease(t, len(s.record.Failures) == failures, "health changed the circuit's failures")
				requireServiceLease(t, after.Faulted == (end == "hook_deadline_then_stop"), "health changed the durable stop-refusal decision")
			})
		}
	}
}
