package localcell

import (
	"bytes"
	"context"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestPortsReallocationPublication(t *testing.T) {
	for _, component := range []string{"server", "worker"} {
		for _, outcome := range []string{"reallocated", "exhausted"} {
			t.Run(component+"/"+outcome, func(t *testing.T) {
				c, m := unitCell(t)
				before, err := readManifest(c.cfg.Root)
				if err != nil {
					t.Fatal("cannot read initial valid reallocation manifest")
				}
				initial, err := os.ReadFile(c.path("manifest.json"))
				if err != nil {
					t.Fatal("cannot read initial durable reallocation manifest")
				}
				beforeInfo, err := os.Stat(c.path("manifest.json"))
				if err != nil {
					t.Fatal("cannot stat initial durable reallocation manifest")
				}
				// Windows Stat defers loading file identity until SameFile. Resolve
				// it while the path still names the initial durable manifest.
				if !os.SameFile(beforeInfo, beforeInfo) {
					t.Fatal("cannot resolve initial durable manifest identity")
				}
				assertUnchanged := func() {
					t.Helper()
					observed, err := os.ReadFile(c.path("manifest.json"))
					if err != nil || !bytes.Equal(observed, initial) {
						t.Fatal("unprobed allocation changed the durable manifest")
					}
				}
				attempts := 0
				c.afterPortAllocation = func(candidate Ports) {
					attempts++
					// Allocation has completed, but the candidate has not passed
					// the probe that permits durable publication.
					assertUnchanged()
					if outcome == "exhausted" {
						listener, err := net.Listen("tcp4", addr(*candidate.selected(component)[0]))
						if err != nil {
							t.Fatal("cannot occupy publication candidate port")
						}
						t.Cleanup(func() { _ = listener.Close() })
					}
				}
				err = c.prepareComponentPorts(m, component, false, true)
				if outcome == "exhausted" {
					assertUnchanged()
					if err == nil || err.Error() != "cannot allocate local cell loopback ports" || attempts != 3 {
						t.Fatal("publication exhaustion did not reject three occupied candidates")
					}
					if m.Ports != before.Ports {
						t.Fatal("exhausted publication changed the in-memory manifest ports")
					}
					return
				}
				if err != nil || attempts == 0 {
					t.Fatal("forced component reallocation did not publish a probed candidate")
				}
				afterInfo, err := os.Stat(c.path("manifest.json"))
				if err != nil {
					t.Fatal("cannot stat reallocated durable manifest")
				}
				if !os.SameFile(afterInfo, afterInfo) {
					t.Fatal("cannot resolve reallocated durable manifest identity")
				}
				if os.SameFile(beforeInfo, afterInfo) {
					t.Fatal("port reallocation rewrote the durable manifest in place")
				}
				after, err := readManifest(c.cfg.Root)
				if err != nil || after.Ports != m.Ports {
					t.Fatal("reallocated durable manifest did not match the selected ports")
				}
				for _, peer := range []string{"postgres", "server", "worker"} {
					for i, old := range before.Ports.selected(peer) {
						changed := *after.Ports.selected(peer)[i] != *old
						if changed != (peer == component) {
							t.Fatal("reallocation publication changed the wrong component port set")
						}
					}
				}
			})
		}
	}
}

func TestPostgresPortConfigRecoversInterruptedReallocation(t *testing.T) {
	c, m := unitCell(t)
	ports, err := c.allocateAvailablePorts(Ports{}, "")
	if err != nil {
		t.Fatal("cannot allocate config recovery ports")
	}
	m.Ports = ports
	// A directory at the config target deterministically fails publication
	// after the new manifest has been atomically written.
	if testMkdir(c.path("pg"), 0o700) != nil || testMkdir(c.path("pg", "postgresql.conf"), 0o700) != nil {
		t.Fatal("cannot stage interrupted config write")
	}
	if err := c.prepareComponentPorts(m, "postgres", false, true); err == nil {
		t.Fatal("invalid config target did not fail reallocation publication")
	}
	saved, err := readManifest(c.cfg.Root)
	if err != nil || saved.Ports != m.Ports || saved.Ports.Postgres == ports.Postgres {
		t.Fatal("config interruption did not retain the new durable manifest")
	}
	if os.Remove(c.path("pg", "postgresql.conf")) != nil {
		t.Fatal("cannot release interrupted config target")
	}
	c.afterPortAllocation = func(Ports) { t.Fatal("config recovery unnecessarily reallocated ports") }
	if err := c.prepareComponentPorts(saved, "postgres", false, false); err != nil {
		t.Fatal("stopped preflight did not reconcile the durable PostgreSQL port")
	}
	config, err := os.ReadFile(c.path("pg", "postgresql.conf"))
	if err != nil || !strings.Contains(string(config), "\nport = "+strconv.Itoa(saved.Ports.Postgres)+"\n") {
		t.Fatal("PostgreSQL config did not match the durable manifest after recovery")
	}
}

func TestComponentPortAllocationBoundedAndIsolated(t *testing.T) {
	for _, component := range []string{"postgres", "server", "worker"} {
		t.Run(component, func(t *testing.T) {
			c, m := unitCell(t)
			initial, err := c.allocateAvailablePorts(Ports{}, "")
			if err != nil {
				t.Fatal("cannot allocate initial test ports")
			}
			m.Ports = initial
			attempts := 0
			c.afterPortAllocation = func(candidate Ports) {
				attempts++
				selected := candidate.selected(component)
				listener, err := net.Listen("tcp4", addr(*selected[0]))
				if err != nil {
					t.Fatal("cannot occupy candidate test port")
				}
				t.Cleanup(func() { _ = listener.Close() })
				for i, old := range initial.selected(component) {
					if *selected[i] == *old {
						t.Fatal("reallocation reused a superseded component port")
					}
				}
				for _, peer := range []string{"postgres", "server", "worker"} {
					if peer == component {
						continue
					}
					for i, old := range initial.selected(peer) {
						if *candidate.selected(peer)[i] != *old {
							t.Fatal("component allocation changed a peer's ports")
						}
					}
				}
			}
			if err := c.prepareComponentPorts(m, component, false, true); err == nil ||
				err.Error() != "cannot allocate local cell loopback ports" || attempts != 3 {
				t.Fatal("component allocation did not stop after three occupied candidates")
			}
			if m.Ports != initial {
				t.Fatal("exhausted allocation changed the manifest ports")
			}
			attempts = 0
			if err := c.prepareComponentPorts(m, component, true, true); err != nil || attempts != 0 || m.Ports != initial {
				t.Fatal("running component was reallocated")
			}
		})
	}
}

func TestPortRelaunchMinimumUsesComponentBudget(t *testing.T) {
	c, m := unitCell(t)
	c.cfg.Timeout = time.Hour
	m.MigrationWait = time.Hour
	for _, component := range []string{"server", "worker"} {
		budget := c.readinessTimeout(component, m)
		for _, tc := range []struct {
			name      string
			remaining time.Duration
			want      bool
		}{{"below minimum", budget / 20, false}, {"useful remainder", budget / 5, true}, {"expired", -budget, false}} {
			t.Run(component+"/"+tc.name, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), tc.remaining)
				defer cancel()
				if c.hasRelaunchBudget(ctx, component, m) != tc.want {
					t.Fatal("relaunch minimum did not follow the configured component budget")
				}
			})
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Hour)
	cancel()
	if c.hasRelaunchBudget(ctx, "worker", m) || c.hasRelaunchBudget(context.Background(), "worker", m) {
		t.Fatal("relaunch accepted cancellation or an unbounded context")
	}
}
