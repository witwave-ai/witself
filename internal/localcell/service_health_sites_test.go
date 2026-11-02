package localcell

import (
	"bytes"
	"context"
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestServiceHealthFailureSubstep(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	c, _ := descriptorTestCell(t)
	for _, site := range []string{"health_lock", "health_tree"} {
		t.Run(site, func(t *testing.T) {
			if site == "health_lock" {
				lock, err := c.own(false)
				if err != nil {
					t.Fatal("cannot hold the fixture operation lock")
				}
				defer func() { _ = lock.Close() }()
			}
			var progress serviceHealthProgress
			err := c.ServiceHealth(withServiceHealthProgress(t.Context(), &progress))
			if err == nil || progress.site != site {
				t.Fatal("health did not retain its failing substep")
			}
			if site == "health_lock" && !errors.Is(err, ErrLocked) {
				t.Fatal("health substep changed lock contention identity")
			}
			var receipt bytes.Buffer
			WriteServiceHostFailure(&receipt, serviceFailureOperation(err, "health"), "supervisor_command")
			if receipt.String() != "native service host site="+site+"\n" {
				t.Fatal("health failure lost its precise substep receipt")
			}
		})
	}
}

func TestServiceHealthProgressVocabulary(t *testing.T) {
	for _, site := range []string{"health_lock", "health_tree", "health_database", "health_credential", "health_probe", "health_ready"} {
		t.Run(site, func(t *testing.T) {
			var progress serviceHealthProgress
			ctx, cancel := context.WithCancel(withServiceHealthProgress(t.Context(), &progress))
			if serviceHealthStep(ctx, site) != site || progress.site != site {
				t.Fatal("health progress lost its closed substep")
			}
			cancel()
			if progress.site != site {
				t.Fatal("health cancellation erased its in-flight substep")
			}
			var receipt bytes.Buffer
			cause := serviceFailureAt(ctx.Err(), serviceHealthSite(progress.site))
			WriteServiceHostFailure(&receipt, serviceFailureOperation(cause, "health"), "supervisor_command")
			if !errors.Is(cause, context.Canceled) || receipt.String() != "native service host site="+site+"\n" {
				t.Fatal("health cancellation lost its precise substep receipt")
			}
		})
	}
	for _, site := range []string{"", "health", "supervisor_health", "health_unknown", "health_tree private", "private/path"} {
		var progress serviceHealthProgress
		ctx := withServiceHealthProgress(t.Context(), &progress)
		if serviceHealthStep(ctx, site) != "unknown" || progress.site != "unknown" {
			t.Fatal("health progress admitted an unreviewed site")
		}
	}
}

func TestServiceHealthShutdownSubstep(t *testing.T) {
	hostBound := serviceHealthTickMeasuredBound(t, time.Second)
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	c, manifest := unitCell(t)
	if runtime.GOOS != "windows" {
		root, err := os.MkdirTemp("/tmp", "lc-health-")
		if err != nil {
			t.Fatal("cannot create short private fixture")
		}
		t.Cleanup(func() { _ = os.RemoveAll(root) })
		c, err = New(Config{Root: root, Timeout: hostBound})
		if err != nil || c.layout() != nil || writeAtomic(c.path("manifest.json"), manifest, nil) != nil {
			t.Fatal("cannot prepare short private fixture")
		}
	}
	c.cfg.Timeout = hostBound
	prepareSupervisorTest(t, c)
	hooks, _, _, _, _ := supervisorTestHooks()
	if runtime.GOOS != "windows" {
		hooks.boot = func() (string, error) { return "synthetic-boot", nil }
	}
	hooks.ticks = make(chan time.Time)
	ctx, cancel := context.WithTimeout(t.Context(), 8*hostBound)
	defer cancel()
	hooks.health = func(opCtx context.Context) error {
		serviceHealthStep(opCtx, "health_database")
		cancel()
		return opCtx.Err()
	}
	stderr, err := os.CreateTemp(t.TempDir(), "health-shutdown")
	if err != nil {
		t.Fatal("cannot capture health shutdown receipt")
	}
	previous := os.Stderr
	os.Stderr = stderr
	t.Cleanup(func() { os.Stderr = previous; _ = stderr.Close() })
	if err := c.Run(ctx, SupervisorPolicy{hooks: &hooks, ShutdownTimeout: hostBound}); err != nil {
		t.Fatal("health cancellation did not complete foreground shutdown")
	}
	os.Stderr = previous
	data, err := os.ReadFile(stderr.Name())
	if err != nil {
		t.Fatal("cannot read health shutdown receipt")
	}
	const receipt = "service_shutdown phase=tick operation=health site=health_database journal_pending=false journal_read_ok=true\n"
	if !bytes.Contains(data, []byte(receipt)) || strings.Count(string(data), "service_shutdown ") != 1 {
		t.Fatal("health shutdown lost its in-flight substep receipt")
	}
}

// Measure a completed no-op tick through the supervisor health seam used by the
// shutdown fixture. Keep the calibration supervisor separate from the host so
// its measured bound is fixed before Run can read the host configuration.
func serviceHealthTickMeasuredBound(t *testing.T, floor time.Duration) time.Duration {
	t.Helper()
	c, _ := unitCell(t)
	d := prepareSupervisorTest(t, c)
	hooks, _, _, _, _ := supervisorTestHooks()
	s := newSupervisor(c, SupervisorPolicy{hooks: &hooks})
	if s.load(d) != nil || s.step(t.Context()) != nil {
		t.Fatal("cannot prepare no-op health tick calibration")
	}
	healthCalls := 0
	s.hooks.health = func(context.Context) error { healthCalls++; return nil }
	started := time.Now()
	if err := s.step(t.Context()); err != nil || healthCalls != 1 {
		t.Fatal("cannot measure no-op health tick")
	}
	measured := time.Since(started)
	bound := max(floor, 20*measured)
	t.Logf("health tick calibration: measured=%s H=%s", measured, bound)
	return bound
}

func TestServiceHealthControlShutdownSubstep(t *testing.T) {
	hostBound := serviceHealthMeasuredBound(t, time.Second)
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	c, manifest := unitCell(t)
	if runtime.GOOS != "windows" {
		root, err := os.MkdirTemp("/tmp", "lc-health-control-")
		if err != nil {
			t.Fatal("cannot create short control health fixture")
		}
		t.Cleanup(func() { _ = os.RemoveAll(root) })
		c, err = New(Config{Root: root, Timeout: hostBound})
		if err != nil || c.layout() != nil || writeAtomic(c.path("manifest.json"), manifest, nil) != nil {
			t.Fatal("cannot prepare short control health fixture")
		}
	}
	c.cfg.Timeout = hostBound
	prepareSupervisorTest(t, c)
	hooks, _, _, _, _ := supervisorTestHooks()
	if runtime.GOOS != "windows" {
		hooks.boot = func() (string, error) { return "synthetic-boot", nil }
	}
	hooks.ticks = make(chan time.Time)
	ctx, cancel := context.WithTimeout(t.Context(), 8*hostBound)
	defer cancel()
	ready := make(chan struct{})
	healthCalls := 0
	hooks.health = func(opCtx context.Context) error {
		healthCalls++
		if healthCalls == 1 {
			close(ready)
			return nil
		}
		serviceHealthStep(opCtx, "health_database")
		cancel()
		// Run propagates host shutdown to its accepted control command. Wait
		// for that cancellation so this proves the control yield path itself.
		<-opCtx.Done()
		return opCtx.Err()
	}
	stderr, err := os.CreateTemp(t.TempDir(), "control-health-shutdown")
	if err != nil {
		t.Fatal("cannot capture control health shutdown receipt")
	}
	previous := os.Stderr
	os.Stderr = stderr
	t.Cleanup(func() { os.Stderr = previous; _ = stderr.Close() })
	done := make(chan struct{})
	var hostErr error
	t.Cleanup(func() {
		cancel()
		// Join before restoring stderr or removing the fixture, including
		// failure paths before the initial tick or command response.
		<-done
	})
	go func() {
		hostErr = c.Run(ctx, SupervisorPolicy{hooks: &hooks, ShutdownTimeout: hostBound})
		close(done)
	}()
	select {
	case <-ready:
	case <-done:
		if hostErr != nil {
			t.Fatal("control health host failed before its initial tick")
		}
		t.Fatal("control health host stopped before its initial tick")
	case <-time.After(8 * c.cfg.Timeout):
		t.Fatal("control health host did not enter its initial tick")
	}
	// Host shutdown may close the response transport; the decisive evidence
	// is the host's completed cancellation and shutdown receipts.
	_, _ = c.Control(t.Context(), "health")
	select {
	case <-done:
		if hostErr != nil {
			t.Fatal("control health cancellation did not complete foreground shutdown")
		}
	case <-time.After(8 * c.cfg.Timeout):
		t.Fatal("control health host did not finish shutdown")
	}
	os.Stderr = previous
	if healthCalls != 2 {
		t.Fatal("control shutdown did not interrupt the accepted health command")
	}
	data, err := os.ReadFile(stderr.Name())
	if err != nil {
		t.Fatal("cannot read control health shutdown receipt")
	}
	const receipt = "native service host site=control_cancelled\n" +
		"service_shutdown phase=control operation=health site=health_database journal_pending=false journal_read_ok=true\n"
	if string(data) != receipt {
		t.Fatal("control health shutdown lost its in-flight substep receipt or duplicated its cancellation receipt")
	}
}
