package localcell

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var launchdFixtureStates struct {
	sync.Once
	states map[int]*os.ProcessState
	err    error
}

func launchdFixtureError(t *testing.T, code int, detail string) error {
	t.Helper()
	launchdFixtureStates.Do(func() {
		launchdFixtureStates.states = make(map[int]*os.ProcessState)
		for _, n := range []int{5, 36, 112, 113} {
			host, err := os.Executable()
			if err != nil {
				launchdFixtureStates.err = errors.New("cannot resolve fixture executable")
				return
			}
			cmd := exec.CommandContext(t.Context(), host, "-test.run=^TestLaunchdFixtureExit$", "--", "--launchd-fixture-exit", strconv.Itoa(n))
			cmd.Env = []string{}
			var exitErr *exec.ExitError
			if err := cmd.Run(); !errors.As(err, &exitErr) {
				launchdFixtureStates.err = errors.New("cannot create synthetic exit status")
				return
			}
			launchdFixtureStates.states[n] = exitErr.ProcessState
		}
	})
	if launchdFixtureStates.err != nil {
		t.Fatal(launchdFixtureStates.err)
	}
	return &exec.ExitError{ProcessState: launchdFixtureStates.states[code], Stderr: []byte(detail)}
}

type launchdFixtureManager struct {
	t *testing.T
	a *launchdAgentAdapter
	d ServiceDescriptor
	// calls preserves the existing print/mutation history used by legacy
	// lifecycle assertions; allCalls also records the new read-only query.
	calls    [][]string
	allCalls [][]string
	// Full GUI-domain targets: absent means no override; present false is disabled.
	enabled              map[string]bool
	loaded               bool
	noGUI                bool
	failEnable           bool
	failBootstrap        bool
	failBootout          bool
	mismatch             string
	postMismatch         string
	unobservedInProgress int
	removing             bool
	neverGone            bool
	bootstrapStderr      string
}

// Observed pairs come from reports/LC-S7C1-owner-premise-evidence.md on macOS
// 26.6.2; this remains a hermetic model, not a native registration receipt.
// Explicit fault injections and defensive EINPROGRESS behavior are unobserved.
func (m *launchdFixtureManager) command(ctx context.Context, binary string, args ...string) ([]byte, error) {
	m.allCalls = append(m.allCalls, append([]string{binary}, args...))
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(args) == 0 || args[0] != "print-disabled" {
		m.calls = append(m.calls, append([]string{binary}, args...))
	}
	if binary != "/bin/launchctl" || len(args) < 2 {
		m.t.Fatal("unexpected launchd fixture command")
	}
	if args[0] == "print" && args[1] == m.a.domain() {
		if m.noGUI {
			// reports/LC-S7C1-owner-premise-evidence.md, read-only probes, absent-domain bullet: 112/message.
			return nil, launchdFixtureError(m.t, 112, "Bad request. Could not find domain for user gui: "+m.a.uid)
		}
		// reports/LC-S7C1-owner-premise-evidence.md, read-only probes, logged-in GUI-domain bullet: exit 0.
		return []byte("fixture gui domain\n"), nil
	}
	if args[0] == "print-disabled" {
		if len(args) != 2 || args[1] != m.a.domain() {
			m.t.Fatal("print-disabled must use exactly the owning GUI domain")
		}
		if m.noGUI {
			return nil, launchdFixtureError(m.t, 112, "Bad request. Could not find domain for user gui: "+m.a.uid)
		}
		// Synthetic model of the owner's 2026-11-07 read-only observation on
		// macOS 26.6.2; this is not native no-enable bootstrap evidence.
		var output strings.Builder
		output.WriteString("\n\tdisabled services = {\n")
		var targets []string
		for target := range m.enabled {
			if strings.HasPrefix(target, m.a.domain()+"/") {
				targets = append(targets, target)
			}
		}
		slices.Sort(targets)
		for _, target := range targets {
			state := "disabled"
			if m.enabled[target] {
				state = "enabled"
			}
			fmt.Fprintf(&output, "\t\t%q => %s\n", strings.TrimPrefix(target, m.a.domain()+"/"), state)
		}
		output.WriteString("\t}\n")
		return []byte(output.String()), nil
	}
	if args[0] == "bootstrap" {
		if len(args) != 3 || args[1] != m.a.domain() || args[2] != m.a.plistPath(m.d) {
			m.t.Fatal("bootstrap escaped exact fixture domain or plist")
		}
		enabled, overridden := m.enabled[m.a.target(m.d)]
		if m.loaded || (overridden && !enabled) || m.failBootstrap {
			// reports/LC-S7C1-owner-premise-evidence.md, Bootstrap-path observations,
			// bootstrap(duplicate while loaded) and bootstrap(disabled): 5/message.
			detail := "Bootstrap failed: 5: Input/output error\nTry re-running the command as root for richer errors."
			if m.bootstrapStderr != "" {
				// Synthetic sanitizer fault injection, not an observed diagnostic.
				detail = m.bootstrapStderr
			}
			return nil, launchdFixtureError(m.t, 5, detail)
		}
		// reports/LC-S7C1-owner-premise-evidence.md, Bootstrap-path observations, bootstrap(first) and
		// bootstrap(after re-enable) bullets: exit 0 after per-label enable.
		// Bootstrap with no override is modelled pending native CI.
		m.loaded = true
		m.mismatch = m.postMismatch
		return nil, nil
	}
	if len(args) != 2 || args[1] != m.a.target(m.d) {
		m.t.Fatal("command escaped exact fixture service target")
	}
	switch args[0] {
	case "enable":
		if m.failEnable {
			// launchctl(1), enable/disable; synthetic command failure, unobserved.
			return nil, launchdFixtureError(m.t, 5, "enable denied")
		}
		// reports/LC-S7C1-owner-premise-evidence.md, Bootstrap-path observations, enable(fresh) and
		// enable(clear override) bullets; launchctl(1), enable/disable.
		m.enabled[args[1]] = true
		return nil, nil
	case "bootout":
		if m.failBootout {
			// launchctl(1), bootstrap/bootout; synthetic command failure, unobserved.
			return nil, launchdFixtureError(m.t, 5, "Boot-out failed: 5: Input/output error")
		}
		if m.unobservedInProgress > 0 || m.neverGone {
			// Unobserved; errno.h citation only: Darwin sys/errno.h EINPROGRESS=36.
			// reports/LC-S7C1-owner-premise-evidence.md, Bootstrap-path observations, EINPROGRESS bullet,
			// explicitly reports that this defensive path was not observed.
			m.removing = true
			return nil, launchdFixtureError(m.t, 36, "Boot-out failed: 36: Operation now in progress")
		}
		// reports/LC-S7C1-owner-premise-evidence.md, Bootstrap-path observations, bootout(running) and
		// print gone after bootout bullets: exit 0 and immediate removal.
		m.loaded = false
		return nil, nil
	case "print":
		if m.removing && !m.neverGone {
			// Unobserved delayed-removal model for the errno.h-only path above.
			if m.unobservedInProgress == 0 {
				m.loaded = false
			} else {
				m.unobservedInProgress--
			}
		}
		if !m.loaded {
			// reports/LC-S7C1-owner-premise-evidence.md, read-only probes, missing-service bullet and
			// Bootstrap-path observations, print(after bootout) bullet: 113/message.
			return nil, launchdFixtureError(m.t, 113, fmt.Sprintf("Bad request. Could not find service %q in domain for user gui: %s", serviceLaunchdLabel(m.d), m.a.uid))
		}
		path, program := m.a.plistPath(m.d), m.d.Payload.Host
		args := append([]string{program}, m.d.Payload.Args...)
		switch m.mismatch {
		case "path":
			path += ".foreign"
		case "program":
			program += ".foreign"
		case "argument":
			args[1] += "-foreign"
		case "extra":
			args = append(args, "foreign")
		case "parse":
			return []byte("unexpected output"), nil
		}
		return []byte(m.a.target(m.d) + " = {\n\tpath = " + path + "\n\tprogram = " + program + "\n\targuments = {\n\t\t" + strings.Join(args, "\n\t\t") + "\n\t}\n}\n"), nil
	default:
		m.t.Fatal("unexpected mutating launchd command")
		return nil, errors.New("unexpected command")
	}
}

func launchdFixture(t *testing.T) (*launchdAgentAdapter, ServiceDescriptor, *launchdFixtureManager) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("WITSELF_HOME", home)
	t.Setenv("DSH_HOME", home)
	d := ServiceDescriptor{Version: 1, InstallationID: "fixture-install", UID: "1001", Root: filepath.Join(home, "cell's 日本 space"), Adapter: "launchd-agent", Scope: "user", Payload: ServicePayload{Generation: "generation-1", Host: filepath.Join(home, "host's 日本 space"), Args: []string{"run", "--root", filepath.Join(home, "cell's 日本 space")}}, Runtime: "local", DesiredState: "running"}
	a := &launchdAgentAdapter{uid: d.UID, home: home, agentDir: filepath.Join(home, "Library", "LaunchAgents"), launchctl: "/bin/launchctl", owned: func(string, os.FileInfo) bool { return true }, removalTimeout: 128 * time.Millisecond}
	// Windows executes the adapter's command/lifecycle model with synthetic
	// Unix permissions. Native Unix mode and symlink assertions live separately.
	if runtime.GOOS == "windows" {
		a.permissions = func(info os.FileInfo) os.FileMode {
			if info.IsDir() {
				return os.ModeDir | 0o700
			}
			return info.Mode()&^os.ModePerm | 0o600
		}
	}
	m := &launchdFixtureManager{t: t, a: a, d: d, enabled: make(map[string]bool)}
	// Preserve the legacy lifecycle fixture's enable-before-bootstrap baseline
	// as an explicit override. Absent-state proofs delete this entry.
	m.enabled[a.target(d)] = false
	a.command = m.command
	return a, d, m
}

func launchdInstallFixture(t *testing.T, a *launchdAgentAdapter, d ServiceDescriptor) {
	t.Helper()
	plan, err := a.Plan(t.Context(), d)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Apply(t.Context(), plan); err != nil {
		t.Fatal(err)
	}
}

func launchdRequireError(t *testing.T, err, sentinel error, step string) {
	t.Helper()
	if !errors.Is(err, sentinel) || !strings.Contains(err.Error(), step) {
		t.Fatalf("expected sentinel and step %s; received sanitized refusal: %v", step, err)
	}
}

func launchdRequireNoMutation(t *testing.T, m *launchdFixtureManager) {
	t.Helper()
	for _, call := range m.calls {
		if call[1] != "print" && call[1] != "print-disabled" {
			t.Fatal("refusal issued a manager mutation")
		}
	}
}

func TestLaunchdFixtureExit(_ *testing.T) {
	const marker = "--launchd-fixture-exit"
	if index := slices.Index(os.Args, marker); index >= 0 {
		if index+1 >= len(os.Args) {
			os.Exit(1)
		}
		code, err := strconv.Atoi(os.Args[index+1])
		if err != nil {
			os.Exit(1)
		}
		os.Exit(code)
	}
}

func TestLaunchdCapabilityAndPlanAreSideEffectFree(t *testing.T) {
	for _, tc := range []struct{ name, step string }{{"no_gui", "capability:gui_domain"}, {"gui_other_failure", "capability:gui_domain"}, {"euid_mismatch", "identity"}, {"wrong_tool", "launchctl_path"}, {"no_home", "agent_dir_path"}, {"available", ""}} {
		t.Run(tc.name, func(t *testing.T) {
			a, d, m := launchdFixture(t)
			agentDir := a.agentDir
			switch tc.name {
			case "no_gui":
				m.noGUI = true
			case "gui_other_failure":
				// launchctl(1), print: any failed domain probe must fail closed;
				// this unrelated synthetic error is not the observed absent-domain pair.
				a.command = func(ctx context.Context, binary string, args ...string) ([]byte, error) {
					if args[0] == "print" && args[1] == a.domain() {
						return nil, launchdFixtureError(t, 5, "synthetic GUI probe failure")
					}
					return m.command(ctx, binary, args...)
				}
			case "euid_mismatch":
				a.unavailable = true
			case "wrong_tool":
				a.launchctl = "/fixture/launchctl"
			case "no_home":
				a.home = ""
			}
			plan, err := a.Plan(t.Context(), d)
			if tc.step == "" {
				if err != nil || !plan.Observation.Available || plan.Observation.Registered || !plan.Observation.Owned {
					t.Fatal("empty registration plan unavailable or not owned")
				}
			} else {
				launchdRequireError(t, err, ErrServiceManagerUnavailable, tc.step)
				if plan.Observation.Fallback != serviceForegroundFallback || !strings.Contains(err.Error(), serviceForegroundFallback) {
					t.Fatal("capability refusal omitted explicit foreground fallback")
				}
			}
			if _, err := os.Lstat(agentDir); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("Plan created LaunchAgents")
			}
			launchdRequireNoMutation(t, m)
		})
	}
}

func TestLaunchdFakeMatchesObservedPrintFailures(t *testing.T) {
	a, d, m := launchdFixture(t)
	// reports/LC-S7C1-owner-premise-evidence.md, read-only probes,
	// absent-domain and missing-service bullets: pin each complete observed pair.
	m.noGUI = true
	_, err := m.command(t.Context(), a.launchctl, "print", a.domain())
	launchdRequireObservedFailure(t, err, 112, "Bad request. Could not find domain for user gui: "+d.UID)
	m.noGUI = false
	_, err = m.command(t.Context(), a.launchctl, "print", a.target(d))
	launchdRequireObservedFailure(t, err, 113, fmt.Sprintf("Bad request. Could not find service %q in domain for user gui: %s", serviceLaunchdLabel(d), d.UID))
}

func launchdRequireObservedFailure(t *testing.T, err error, code int, stderr string) {
	t.Helper()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != code || string(exitErr.Stderr) != stderr {
		t.Fatal("fixture diverged from observed exit and stderr pair")
	}
}

func TestLaunchdFakeModelsDisabledAndDuplicateBootstrap(t *testing.T) {
	a, d, m := launchdFixture(t)
	m.enabled[a.target(d)] = false
	// reports/LC-S7C1-owner-premise-evidence.md, Bootstrap-path observations,
	// bootstrap(disabled) and bootstrap(duplicate while loaded): identical pairs.
	const stderr = "Bootstrap failed: 5: Input/output error\nTry re-running the command as root for richer errors."
	m.enabled[a.domain()+"/another-fixture-label"] = true
	_, err := m.command(t.Context(), a.launchctl, "bootstrap", a.domain(), a.plistPath(d))
	launchdRequireObservedFailure(t, err, 5, stderr)
	launchdInstallFixture(t, a, d)
	_, err = m.command(t.Context(), a.launchctl, "bootstrap", a.domain(), a.plistPath(d))
	launchdRequireObservedFailure(t, err, 5, stderr)
}

func TestLaunchdExclusiveCreateRaceRefused(t *testing.T) {
	a, d, m := launchdFixture(t)
	plan, err := a.Plan(t.Context(), d)
	if err != nil {
		t.Fatal(err)
	}
	missing := 0
	a.inspectFile = func(path string) (os.FileInfo, error) {
		info, err := os.Lstat(path)
		if path == a.plistPath(d) && errors.Is(err, os.ErrNotExist) {
			missing++
			if missing == 2 {
				if err := os.WriteFile(path, []byte("racing foreign plist"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}
		return info, err
	}
	launchdRequireError(t, a.Apply(t.Context(), plan), ErrServiceOwnershipConflict, "plist_file:create")
	b, err := os.ReadFile(a.plistPath(d))
	if err != nil || string(b) != "racing foreign plist" {
		t.Fatal("exclusive create overwrote racing file")
	}
	launchdRequireNoMutation(t, m)
}

func TestLaunchdLoadedOwnershipMismatchRefused(t *testing.T) {
	for _, kind := range []string{"path", "program", "argument", "extra", "parse"} {
		t.Run(kind, func(t *testing.T) {
			a, d, m := launchdFixture(t)
			launchdInstallFixture(t, a, d)
			m.mismatch, m.calls = kind, nil
			step := kind
			if kind == "argument" || kind == "extra" {
				step = "arguments"
			}
			_, err := a.Inspect(t.Context(), d)
			launchdRequireError(t, err, ErrServiceOwnershipConflict, "inspect:"+step)
			launchdRequireError(t, a.Remove(t.Context(), d), ErrServiceOwnershipConflict, "remove:ownership")
			if _, err := os.Lstat(a.plistPath(d)); err != nil {
				t.Fatal("unowned loaded plist removed")
			}
			launchdRequireNoMutation(t, m)
		})
	}
}

func TestLaunchdUnloadedPlistIsNotOwnedAndCanRecover(t *testing.T) {
	a, d, m := launchdFixture(t)
	launchdInstallFixture(t, a, d)
	m.loaded = false
	before, err := os.Lstat(a.plistPath(d))
	if err != nil {
		t.Fatal(err)
	}
	obs, err := a.Inspect(t.Context(), d)
	launchdRequireError(t, err, ErrServiceRegistrationFailed, "inspect:not_loaded")
	if !obs.Registered || obs.Owned {
		t.Fatal("unloaded file claimed as owned registration")
	}
	launchdRequireError(t, a.Remove(t.Context(), d), ErrServiceOwnershipConflict, "remove:ownership")
	plan, err := a.Plan(t.Context(), d)
	if err != nil || !plan.Observation.Registered || plan.Observation.Owned {
		t.Fatal("explicit install cannot plan recovery of exact unloaded plist")
	}
	if err := a.Apply(t.Context(), plan); err != nil {
		t.Fatal(err)
	}
	after, err := os.Lstat(a.plistPath(d))
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("recovery did not reuse original exact plist")
	}
}

func TestLaunchdApplyFailureCleansOnlyCreatedIdentity(t *testing.T) {
	for _, failure := range []string{"bootstrap", "enable", "foreign_print", "write", "sync", "replacement"} {
		t.Run(failure, func(t *testing.T) {
			a, d, m := launchdFixture(t)
			if err := os.MkdirAll(a.agentDir, 0o700); err != nil {
				t.Fatal(err)
			}
			syncs := 0
			a.syncDir = func(path string) error {
				syncs++
				if failure == "sync" && syncs == 1 {
					return errors.New("fixture sync refused")
				}
				return syncDirectory(path)
			}
			switch failure {
			case "bootstrap":
				m.failBootstrap = true
			case "enable":
				m.enabled[a.target(d)] = false
				m.failEnable = true
			case "foreign_print":
				m.postMismatch = "argument"
			case "write":
				a.writePlist = func(*os.File, []byte) error { return errors.New("fixture write refused") }
			case "replacement":
				a.command = func(ctx context.Context, binary string, args ...string) ([]byte, error) {
					if args[0] == "bootstrap" {
						if err := os.Rename(a.plistPath(d), a.plistPath(d)+".created"); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(a.plistPath(d), []byte("replacement"), 0o600); err != nil {
							t.Fatal(err)
						}
						// reports/LC-S7C1-owner-premise-evidence.md, Bootstrap-path observations, bootstrap(duplicate while loaded):
						// inject the observed failure pair after an adversarial file replacement.
						return nil, launchdFixtureError(t, 5, "Bootstrap failed: 5: Input/output error\nTry re-running the command as root for richer errors.")
					}
					return m.command(ctx, binary, args...)
				}
			}
			_, planErr := a.Plan(t.Context(), d)
			if planErr != nil {
				t.Fatal(planErr)
			}
			err := a.Apply(t.Context(), ServicePlan{Spec: d, RequiredPrivilege: "user"})
			if !errors.Is(err, ErrServiceRegistrationFailed) {
				t.Fatal("Apply failure lost registration sentinel")
			}
			if failure == "replacement" {
				if b, err := os.ReadFile(a.plistPath(d)); err != nil || string(b) != "replacement" {
					t.Fatal("cleanup removed replacement identity")
				}
			} else if _, err := os.Lstat(a.plistPath(d)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("created plist survived cleanup")
			}
			if failure != "replacement" && syncs == 0 {
				t.Fatal("cleanup did not sync directory")
			}
			if failure == "bootstrap" && syncs < 2 {
				t.Fatal("failed bootstrap cleanup did not sync deletion")
			}
			if failure == "foreign_print" {
				for _, call := range m.calls {
					if call[1] == "bootout" {
						t.Fatal("cleanup booted out foreign registration")
					}
				}
			}
		})
	}
}

func TestLaunchdBootoutObservedRemovalAndDefensiveFailures(t *testing.T) {
	const observedCommandDelay = 10 * time.Millisecond
	for _, mode := range []string{"observed_immediate", "failure", "unobserved_errno36_progress", "deadline", "cancel", "foreign_during_wait", "replacement"} {
		t.Run(mode, func(t *testing.T) {
			a, d, m := launchdFixture(t)
			launchdInstallFixture(t, a, d)
			ctx := t.Context()
			switch mode {
			case "observed_immediate":
				// reports/LC-S7C1-owner-premise-evidence.md, Bootstrap-path observations,
				// bootout(running), print gone after bootout, print(after bootout).
				a.removalTimeout = 100 * observedCommandDelay
				a.command = func(ctx context.Context, binary string, args ...string) ([]byte, error) {
					time.Sleep(observedCommandDelay)
					output, err := m.command(ctx, binary, args...)
					if args[0] == "bootout" && (err != nil || len(output) != 0) {
						t.Fatal("fixture diverged from observed successful bootout")
					}
					return output, err
				}
			case "failure":
				m.failBootout = true
			case "unobserved_errno36_progress":
				// Unobserved; errno.h citation only: Darwin sys/errno.h EINPROGRESS=36.
				m.unobservedInProgress = 3
			case "deadline":
				m.neverGone = true
				a.removalTimeout = 20 * time.Millisecond
			case "cancel":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				a.command = func(ctx context.Context, binary string, args ...string) ([]byte, error) {
					if args[0] == "bootout" {
						m.neverGone = true
						cancel()
					}
					return m.command(ctx, binary, args...)
				}
			case "foreign_during_wait", "replacement":
				a.command = func(ctx context.Context, binary string, args ...string) ([]byte, error) {
					if args[0] == "bootout" {
						if mode == "foreign_during_wait" {
							m.neverGone, m.mismatch = true, "argument"
						} else {
							if err := os.Rename(a.plistPath(d), a.plistPath(d)+".original"); err != nil {
								t.Fatal(err)
							}
							if err := os.WriteFile(a.plistPath(d), []byte("replacement"), 0o600); err != nil {
								t.Fatal(err)
							}
						}
					}
					return m.command(ctx, binary, args...)
				}
			}
			started := time.Now()
			err := a.Remove(ctx, d)
			elapsed := time.Since(started)
			if mode == "observed_immediate" || mode == "unobserved_errno36_progress" {
				if err != nil {
					t.Fatal(err)
				}
				if _, err := os.Lstat(a.plistPath(d)); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("completed bootout retained plist")
				}
				if mode == "observed_immediate" {
					// Allow scheduling and filesystem overhead beyond the five fake
					// commands, while requiring completion before half the removal timeout.
					if elapsed >= a.removalTimeout/2 {
						t.Fatal("immediate removal exceeded proportional command allowance")
					}
					bootout := slices.IndexFunc(m.calls, func(call []string) bool { return call[1] == "bootout" })
					wantAfter := [][]string{{a.launchctl, "print", a.target(d)}, {a.launchctl, "print", a.domain()}}
					if bootout < 0 || !slices.EqualFunc(m.calls[bootout+1:], wantAfter, slices.Equal[[]string]) {
						t.Fatal("observed immediate removal did not finish on first service print")
					}
				}
			} else {
				sentinel := ErrServiceRemovalFailed
				if mode == "foreign_during_wait" || mode == "replacement" {
					sentinel = ErrServiceOwnershipConflict
				}
				if !errors.Is(err, sentinel) {
					t.Fatalf("wrong sanitized removal refusal: %v", err)
				}
				if _, err := os.Lstat(a.plistPath(d)); err != nil {
					t.Fatal("failed removal lost plist")
				}
			}
			if mode == "deadline" && elapsed > 20*a.removalTimeout {
				t.Fatal("removal exceeded proportional deadline allowance")
			}
		})
	}
}

func TestLaunchdCommandStderrIsSanitized(t *testing.T) {
	a, d, m := launchdFixture(t)
	hex, token := strings.Repeat("a", 64), strings.Repeat("Z", 72)
	standardToken := launchdStandardBase64TokenFixture(t)
	m.failBootstrap = true
	m.bootstrapStderr = fmt.Sprintf("fixture %s %s %s %s %s", d.Root, a.home, hex, token, standardToken)
	err := a.Apply(t.Context(), ServicePlan{Spec: d, RequiredPrivilege: "user"})
	launchdRequireError(t, err, ErrServiceRegistrationFailed, "bootstrap")
	for _, private := range []string{d.Root, a.home, hex, token, standardToken} {
		if strings.Contains(err.Error(), private) {
			t.Fatal("command refusal exposed private value")
		}
	}
	for _, redaction := range []string{"<root>", "<home>", "<hex>", "<token>"} {
		if !strings.Contains(err.Error(), redaction) {
			t.Fatal("command refusal lost sanitized diagnostic")
		}
	}
}

func launchdStandardBase64TokenFixture(t *testing.T) string {
	t.Helper()
	raw := bytes.Repeat([]byte{0xfb, 0xff, 0xff}, 13)[:37]
	token := base64.RawStdEncoding.EncodeToString(raw)
	decoded, err := base64.RawStdEncoding.DecodeString(token)
	if err != nil || len(token) != 50 || !strings.Contains(token, "+") || !strings.Contains(token, "/") || !bytes.Equal(decoded, raw) {
		t.Fatal("invalid synthetic standard-base64 token fixture")
	}
	return token
}

func TestLaunchdExitClassificationRequiresCodeAndMessage(t *testing.T) {
	// reports/LC-S7C1-owner-premise-evidence.md, read-only probes,
	// missing-service bullet: exercise the full observed stderr.
	const missing = "Bad request. Could not find service \"fixture-label\" in domain for user gui: 1001"
	if !launchdMissing(launchdFixtureError(t, 113, missing)) {
		t.Fatal("observed missing service refused")
	}
	for _, err := range []error{launchdFixtureError(t, 5, missing), launchdFixtureError(t, 113, "unrelated failure"), errors.New(missing)} {
		if launchdMissing(err) {
			t.Fatal("unrelated failure classified as missing")
		}
	}
}

func TestLaunchdPartialBootstrapCleanupPreservesRecovery(t *testing.T) {
	for _, mode := range []string{"success", "bootout_failure", "canceled_inspection"} {
		t.Run(mode, func(t *testing.T) {
			a, d, m := launchdFixture(t)
			m.failBootout = mode == "bootout_failure"
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			a.command = func(ctx context.Context, binary string, args ...string) ([]byte, error) {
				output, err := m.command(ctx, binary, args...)
				if args[0] == "bootstrap" && err == nil {
					if mode == "canceled_inspection" {
						cancel()
					}
					// reports/LC-S7C1-owner-premise-evidence.md, Bootstrap-path observations, bootstrap(duplicate while loaded):
					// inject the observed failure pair after a synthetic partial bootstrap.
					return nil, launchdFixtureError(t, 5, "Bootstrap failed: 5: Input/output error\nTry re-running the command as root for richer errors.")
				}
				return output, err
			}
			err := a.Apply(ctx, ServicePlan{Spec: d, RequiredPrivilege: "user"})
			launchdRequireError(t, err, ErrServiceRegistrationFailed, "bootstrap")
			bootouts := 0
			for _, call := range m.calls {
				if call[1] == "bootout" {
					bootouts++
				}
			}
			wantBootouts := 1
			if mode == "canceled_inspection" {
				wantBootouts = 0
			}
			if bootouts != wantBootouts {
				t.Fatal("partial owned registration cleanup did not attempt exact bootout")
			}
			if mode != "success" {
				obs, err := a.Inspect(t.Context(), d)
				if err != nil || !obs.Registered || !obs.Owned {
					t.Fatal("failed cleanup lost recoverable ownership record")
				}
				m.failBootout = false
				if err := a.Remove(t.Context(), d); err != nil {
					t.Fatal("explicit removal could not recover partial registration")
				}
			}
			if _, err := os.Lstat(a.plistPath(d)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("successful cleanup retained created plist")
			}
			if m.loaded {
				t.Fatal("successful cleanup retained registration")
			}
		})
	}
}

func TestLaunchdReplacementBeforeBootstrapRefused(t *testing.T) {
	for _, stage := range []string{"persist", "enable"} {
		t.Run(stage, func(t *testing.T) {
			a, d, m := launchdFixture(t)
			m.enabled[a.target(d)] = false
			replace := func() {
				if err := os.Rename(a.plistPath(d), a.plistPath(d)+".original"); err != nil {
					t.Fatal(err)
				}
				// Even matching bytes on a different inode cannot replace the
				// registration identity just created by this Apply attempt.
				want, err := RenderLaunchdAgentPlist(d)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(a.plistPath(d), want, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if stage == "persist" {
				replaced := false
				a.syncDir = func(path string) error {
					if err := syncDirectory(path); err != nil {
						return err
					}
					if path == a.agentDir && !replaced {
						replaced = true
						replace()
					}
					return nil
				}
			} else {
				a.command = func(ctx context.Context, binary string, args ...string) ([]byte, error) {
					output, err := m.command(ctx, binary, args...)
					if args[0] == "enable" {
						replace()
					}
					return output, err
				}
			}
			err := a.Apply(t.Context(), ServicePlan{Spec: d, RequiredPrivilege: "user"})
			if !errors.Is(err, ErrServiceOwnershipConflict) || !errors.Is(err, ErrServiceRegistrationFailed) {
				t.Fatal("replacement did not preserve ownership and registration refusals")
			}
			for _, call := range m.calls {
				if call[1] == "bootstrap" || call[1] == "bootout" {
					t.Fatal("replacement received manager mutation authority")
				}
			}
			if _, err := os.Lstat(a.plistPath(d)); err != nil {
				t.Fatal("replacement identity was deleted")
			}
		})
	}
}
