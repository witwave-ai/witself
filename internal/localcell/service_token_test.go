//go:build !windows

package localcell

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"testing"
)

// This is a public synthetic fixture, never an installation credential. Keeping
// it known lets the assertions detect a leak without including its value in any
// failure output.
func serviceFixtureControlToken() []byte {
	return []byte(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x5a}, 32)))
}

func installServiceFixtureControlToken(t *testing.T, c *Cell) []byte {
	t.Helper()
	token := serviceFixtureControlToken()
	if err := writeAtomicBytes(c.path("service", "control.secret"), token, nil); err != nil {
		t.Fatal("cannot install synthetic control token")
	}
	observed, err := c.serviceControlSecret()
	if err != nil || !bytes.Equal([]byte(observed), token) {
		t.Fatal("synthetic installation did not retain the known control token")
	}
	return token
}

func assertServiceOutputExcludesToken(t *testing.T, label string, output, token []byte) {
	t.Helper()
	if len(output) == 0 {
		t.Errorf("%s observation is empty", label)
	}
	if offset := bytes.Index(output, token); offset >= 0 {
		t.Errorf("control token present in %s (length=%d offset=%d)", label, len(token), offset)
	}
}

func TestServiceRenderedUnitAndOutputsExcludeControlToken(t *testing.T) {
	c, _ := unitCell(t)
	descriptor := prepareSupervisorTest(t, c)
	token := installServiceFixtureControlToken(t, c)
	a, d, manager := systemdFixture(t)
	// Keep the fake manager's portable numeric UID while binding rendering to
	// the actual prepared installation and its present control.secret file.
	d.Root, d.InstallationID, d.Payload = descriptor.Root, descriptor.InstallationID, descriptor.Payload
	manager.descriptor = d
	unit, err := RenderSystemdUserUnit(d)
	if err != nil {
		t.Fatal("cannot render synthetic installation unit")
	}
	assertServiceOutputExcludesToken(t, "rendered unit", unit, token)
	plan, err := a.Plan(t.Context(), d)
	if err != nil {
		t.Fatal("cannot plan synthetic registration")
	}
	if err := a.Apply(t.Context(), plan); err != nil {
		t.Fatal("cannot apply synthetic registration")
	}
	applied, err := os.ReadFile(a.unitPath(d))
	if err != nil || !manager.loaded || len(manager.calls) == 0 {
		t.Fatal("synthetic manager did not observe the applied unit")
	}
	assertServiceOutputExcludesToken(t, "applied unit", applied, token)
	arguments, err := json.Marshal(manager.calls)
	if err != nil {
		t.Fatal("cannot serialize observed manager arguments")
	}
	assertServiceOutputExcludesToken(t, "manager arguments", arguments, token)
	// Inspect the supervisor's actual status and durable restart receipt, not
	// a hand-built sample of their schemas. The Unix sibling checks wire bytes.
	hooks, _, _, _, _ := supervisorTestHooks()
	supervisor := newSupervisor(c, SupervisorPolicy{hooks: &hooks})
	if err := supervisor.load(descriptor); err != nil {
		t.Fatal("cannot load synthetic supervisor")
	}
	status, err := supervisor.command(t.Context(), "status")
	if err != nil || !status.Healthy || status.State != "running" {
		t.Fatal("synthetic supervisor did not produce running status")
	}
	serialized, err := json.Marshal(status)
	if err != nil {
		t.Fatal("cannot serialize observed supervisor status")
	}
	assertServiceOutputExcludesToken(t, "supervisor status", serialized, token)
	receipt, err := os.ReadFile(c.path("service", "supervisor.json"))
	if err != nil {
		t.Fatal("supervisor restart receipt was not written")
	}
	var record supervisorRecord
	if json.Unmarshal(receipt, &record) != nil || record.Version != 1 || record.Attempts != 1 {
		t.Fatal("supervisor restart receipt did not record the observed launch")
	}
	assertServiceOutputExcludesToken(t, "supervisor restart receipt", receipt, token)
}

func TestServiceLoadedUnitWithControlTokenEnvironmentRefused(t *testing.T) {
	// A token-bearing line is refused wherever it sits inside [Service]: before
	// the ExecStart line and immediately after it, for every operation.
	for _, placement := range []string{"before", "after"} {
		t.Run(placement, func(t *testing.T) {
			for _, operation := range []string{"inspect", "plan", "apply", "remove"} {
				t.Run(operation, func(t *testing.T) {
					testServiceLoadedUnitWithControlTokenEnvironmentRefused(t, placement, operation)
				})
			}
		})
	}
}

func testServiceLoadedUnitWithControlTokenEnvironmentRefused(t *testing.T, placement, operation string) {
	{
		{
			c, _ := unitCell(t)
			descriptor := prepareSupervisorTest(t, c)
			token := installServiceFixtureControlToken(t, c)
			a, d, manager := systemdFixture(t)
			// Bind the manager to the installation with the known token while
			// retaining its portable numeric fixture UID.
			d.Root, d.InstallationID, d.Payload = descriptor.Root, descriptor.InstallationID, descriptor.Payload
			manager.descriptor = d
			plan, err := a.Plan(t.Context(), d)
			if err != nil {
				t.Fatal("cannot plan synthetic registration")
			}
			if err := a.Apply(t.Context(), plan); err != nil {
				t.Fatal("cannot apply synthetic registration")
			}
			observation, err := a.Inspect(t.Context(), d)
			if err != nil || !manager.loaded || !observation.Registered || !observation.Owned {
				t.Fatal("synthetic manager did not report the legitimate unit loaded and owned")
			}
			path := a.unitPath(d)
			applied, err := os.ReadFile(path)
			if err != nil {
				t.Fatal("cannot read applied synthetic unit")
			}
			assertServiceOutputExcludesToken(t, "legitimate applied unit", applied, token)
			service := bytes.Index(applied, []byte("\n[Service]\n"))
			execStart := bytes.Index(applied, []byte("\nExecStart="))
			if service < 0 || execStart <= service || bytes.Count(applied, []byte("\nExecStart=")) != 1 || bytes.Contains(applied[service+len("\n[Service]\n"):execStart], []byte("\n[")) {
				t.Fatal("synthetic unit lacks an unambiguous Service ExecStart")
			}
			line := []byte("Environment=WITSELF_CONTROL_TOKEN=" + string(token) + "\n")
			var tampered []byte
			switch placement {
			case "before":
				tampered = bytes.Replace(applied, []byte("\nExecStart="), append(append([]byte("\n"), line...), []byte("ExecStart=")...), 1)
			case "after":
				execEnd := bytes.IndexByte(applied[execStart+1:], '\n')
				if execEnd < 0 {
					t.Fatal("synthetic ExecStart line lacks a terminator")
				}
				afterExec := execStart + 1 + execEnd + 1
				tampered = append(bytes.Clone(applied[:afterExec]), line...)
				tampered = append(tampered, applied[afterExec:]...)
			default:
				t.Fatal("unknown placement")
			}
			if bytes.Count(tampered, line) != 1 || len(tampered) != len(applied)+len(line) {
				t.Fatal("synthetic tampering did not insert exactly one token line")
			}
			if err := os.WriteFile(path, tampered, 0o600); err != nil {
				t.Fatal("cannot tamper with synthetic unit")
			}
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal("cannot observe tampered synthetic unit")
			}
			callsBefore := len(manager.calls)
			switch operation {
			case "inspect":
				observation, err = a.Inspect(t.Context(), d)
				if observation.Owned || observation.Reason != "service_ownership_conflict" {
					t.Error("loaded unit with synthetic control token environment was accepted")
				}
			case "plan":
				var refused ServicePlan
				refused, err = a.Plan(t.Context(), d)
				if refused.Observation.Owned || refused.Observation.Reason != "service_ownership_conflict" {
					t.Error("plan proposed takeover of token-bearing unit")
				}
			case "apply":
				// Use the legitimate pre-tampering plan to exercise revalidation.
				err = a.Apply(t.Context(), plan)
			case "remove":
				err = a.Remove(t.Context(), d)
			}
			if !errors.Is(err, ErrServiceOwnershipConflict) {
				t.Error("token-bearing existing unit did not produce ownership refusal")
			}
			for _, call := range manager.calls[callsBefore:] {
				readOnly := len(call) > 2 && call[0] == a.systemctl && call[2] == "show" ||
					len(call) > 3 && call[0] == a.busctl && (call[3] == "call" || call[3] == "get-property")
				if !readOnly {
					t.Error("ownership conflict caused a mutating manager command")
				}
			}
			if !manager.loaded || manager.disabled {
				t.Error("ownership conflict changed the loaded registration")
			}
			after, err := os.Stat(path)
			if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() || !before.ModTime().Equal(after.ModTime()) {
				t.Error("ownership conflict touched the foreign unit")
			}
			preserved, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(preserved, tampered) {
				t.Error("ownership conflict changed the foreign unit bytes")
			}
		}
	}
}
