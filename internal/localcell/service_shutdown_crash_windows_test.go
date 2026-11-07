//go:build windows

package localcell

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/runtimeconfig"
	"golang.org/x/sys/windows"
)

// This subprocess exits without defers at the exact ownership-transfer fence.
// The replacement process therefore cannot inherit the original host's map of
// retained termination handles, process handles, or launch job.
func TestWindowsShutdownHostCrashHelper(_ *testing.T) {
	args := os.Args
	for len(args) != 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) != 3 {
		return
	}
	budget, err := strconv.ParseInt(args[2], 10, 64)
	if err != nil || budget <= 0 {
		os.Exit(91)
	}
	c, err := New(Config{Root: args[1], Timeout: time.Duration(budget)})
	if err != nil {
		os.Exit(91)
	}
	environment, err := c.baseEnvironment()
	if err != nil {
		os.Exit(92)
	}
	environment = appendEnvironment(environment, "WITSELF_RUNTIME=local", "WITSELF_HOME="+c.path("home"), "DSH_HOME="+c.path("home"))
	log, err := openPrivateFile(c.path("logs", "crash-child.log"), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		os.Exit(93)
	}
	defer func() { _ = log.Close() }()
	windowsLaunchCheckpoint = func(stage string, _ windows.ProcessInformation) error {
		if stage == "transferred" {
			if writeAtomicBytes(c.path("tmp", "host-crashed-before-release"), []byte("transferred"), nil) != nil {
				os.Exit(94)
			}
			os.Exit(90)
		}
		return nil
	}
	if c.launchWindows(os.Args[0], []string{"-test.run=^TestWindowsShutdownChildHelper$", "--", "normal"}, "server", environment, log, nil) != nil {
		os.Exit(95)
	}
	os.Exit(96)
}

func TestWindowsReleaseBarrierHostCrash(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	c, _ := descriptorTestCell(t)
	startup := shutdownFixtureStartup(t, c)
	ctx, cancel := context.WithTimeout(t.Context(), 100*startup)
	defer cancel()
	host := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestWindowsShutdownHostCrashHelper$", "--", c.cfg.Root, strconv.FormatInt(int64(20*startup), 10))
	environment, err := c.baseEnvironment()
	if err != nil {
		t.Fatal("crash host environment unavailable")
	}
	host.Env = appendEnvironment(environment, "WITSELF_RUNTIME=local", "WITSELF_HOME="+c.path("home"), "DSH_HOME="+c.path("home"))
	host.Dir = c.cfg.Root
	host.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	host.Stdout, host.Stderr = io.Discard, io.Discard
	started := time.Now()
	err = host.Run()
	elapsed := time.Since(started)
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 90 {
		t.Fatal("host did not exit at the pre-release crash fence")
	}
	if _, err := os.Stat(c.path("tmp", "host-crashed-before-release")); err != nil {
		t.Fatal("host crash fence witness absent")
	}
	var record ProcessRecord
	if readJSON(c.path("run", "server.json"), &record) != nil || record.LaunchKind != shutdownLaunchPipe || record.LaunchPending {
		t.Fatal("host crash lost its accepted process record")
	}
	// This handle is a test cleanup witness only. It never enters production's
	// launch-time retained-handle registry or grants the replacement force rights.
	handle, alive, err := openVerifiedWindowsProcess(record, windows.PROCESS_TERMINATE)
	if err != nil || !alive {
		t.Fatal("pre-release child did not survive host process exit")
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	witness := retainedLaunchHandle(t, handle)
	shutdownProcesses.Lock()
	_, inherited := shutdownProcesses.entries[shutdownProcessKey(record)]
	shutdownProcesses.Unlock()
	if inherited {
		t.Fatal("replacement unexpectedly inherited launch termination rights")
	}
	if _, err := os.Stat(c.path("tmp", "store-entered")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("crashed host released the child's store barrier")
	}
	replacement, err := New(c.cfg)
	if err != nil {
		t.Fatal("replacement cell unavailable")
	}
	stopCtx, stopCancel := context.WithTimeout(t.Context(), 10*elapsed+time.Second)
	defer stopCancel()
	outcome := replacement.stopChildOutcomeWithBudget(stopCtx, "server", ProcessState{PID: record.PID, Alive: true, Matches: true}, max(2*elapsed, 25*time.Millisecond))
	if !outcome.permitsDrain() || outcome.Kind == serviceStopForced {
		t.Fatal("replacement failed to cancel crash-surviving barrier child")
	}
	assertWindowsProcessExited(t, witness)
	if _, err := os.Stat(c.path("tmp", "store-entered")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("replacement released stale startup intent")
	}
	if _, err := os.Stat(c.path("run", "server.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("verified barrier exit retained a stale process record")
	}
}

func TestWindowsChildDescriptorACLRefusals(t *testing.T) {
	for _, tc := range []struct{ name, grant string }{
		{"everyone", "(A;;FR;;;WD)"},
		// A foreign SID ACE exercises descriptor policy. This remains synthetic
		// evidence, not a second-user or foreign-owner access witness.
		{"foreign-principal", "(A;;FR;;;S-1-5-21-1-2-3-4242)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("WITSELF_HOME", t.TempDir())
			t.Setenv("DSH_HOME", t.TempDir())
			c, _ := descriptorTestCell(t)
			d, path, err := c.prepareShutdownLaunch("server")
			if err != nil {
				t.Fatal("descriptor preparation failed")
			}
			d.PID = os.Getpid()
			d.StartIdentity, _, err = processIdentity(os.Getpid())
			if err != nil || writeAtomic(path, d, nil) != nil {
				t.Fatal("descriptor publication failed")
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			cfg := runtimeconfig.LocalShutdown{Transport: runtimeconfig.LocalShutdownNamedPipe, DescriptorPath: path}
			clean, err := InstallLocalShutdown(ctx, cancel, cfg, "server")
			if err != nil || clean == nil {
				t.Fatal("clean descriptor refused before ACL mutation")
			}
			if clean.Close() != nil || removePrivateFile(path+".ready") != nil {
				t.Fatal("cannot settle clean descriptor witness")
			}
			if !setTestDACL(t, path, tc.grant, false, false) {
				t.Fatal("descriptor ACL mutation unavailable")
			}
			t.Cleanup(func() { setTestDACL(t, path, "", false, false) })
			bad, err := InstallLocalShutdown(ctx, cancel, cfg, "server")
			if bad != nil {
				_ = bad.Close()
			}
			if err == nil || err.Error() != "service_shutdown_refused: shutdown:descriptor" {
				t.Fatal("unsafe descriptor ACL bypassed its refusal site")
			}
			if _, err := os.Stat(filepath.Clean(path) + ".ready"); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("descriptor ACL refusal reached listener readiness")
			}
		})
	}
}
