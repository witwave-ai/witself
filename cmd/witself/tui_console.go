package main

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/witwave-ai/witself/internal/agenttui"
	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/dashboard"
)

const (
	tuiConsoleChildArgument = "_managed-tui-console"
	consoleStartupTimeout   = 12 * time.Second
	consoleActionTimeout    = 5 * time.Second
)

const tuiConsoleBrowserArgument = "_managed-tui-console-open"

var (
	errConsoleUnavailable = errors.New("web console is unavailable")
	errConsoleConflict    = errors.New("web console context conflicts or cannot be verified; stop it explicitly before restarting")
	errConsoleStart       = errors.New("web console could not start")
	errConsoleStop        = errors.New("web console could not stop")
	errConsoleOpen        = errors.New("web console browser could not open")
	errConsoleCanceled    = errors.New("web console action canceled")
)

// tuiConsole never exposes private discovery or process state through Tea.
// gate serializes actions, but cancellation and Close do not wait to cancel an
// in-flight startup. The retained child lifetime derives only from the parent.
type tuiConsole struct {
	authority func(context.Context, dashboard.AccountManagerIdentity) error
	manager   *dashboard.AccountManager
	roots     accountConsoleRoots
	parent    context.Context
	cancel    context.CancelFunc
	gate      chan struct{}
	conn      agentConnection
	identity  client.SelfIdentity
	poll      time.Duration
	closed    bool
	closeErr  error
	child     *tuiConsoleProcess
	command   func() (*exec.Cmd, error)
	opener    func(context.Context, string) error
	signal    func(dashboard.RegistryEntry) error
}

type tuiConsoleProcess struct {
	accessToken string
	cmd         *exec.Cmd
	pipe        io.WriteCloser
	done        chan struct{}
	entry       dashboard.RegistryEntry
	once        sync.Once
}

var _ agenttui.ConsoleController = (*tuiConsole)(nil)

func newTUIConsole(ctx context.Context, conn agentConnection, identity client.SelfIdentity, poll time.Duration, account accountConsoleOptions) *tuiConsole {
	parent, cancel := context.WithCancel(ctx)
	c := &tuiConsole{parent: parent, cancel: cancel, gate: make(chan struct{}, 1), conn: conn, identity: identity, poll: poll,
		opener: openTUIConsoleBrowser, signal: signalDashboardProcess}
	c.roots = account.Roots
	if account.Manager != nil {
		m := *account.Manager
		c.manager = &m
	}
	c.command = func() (*exec.Cmd, error) {
		executable, err := os.Executable()
		if err != nil {
			return nil, errConsoleStart
		}
		return exec.Command(executable, tuiConsoleChildArgument), nil
	}
	go func() { <-parent.Done(); _ = c.Close() }()
	return c
}

// BindAccountAuthority connects discovery to the same private Reader used by the TUI.
// Called once during model construction, before any console actions.
func (c *tuiConsole) BindAccountAuthority(check func(context.Context, dashboard.AccountManagerIdentity) error) {
	c.authority = check
}

func (c *tuiConsole) action(ctx context.Context, timeout time.Duration) (context.Context, func(), error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	stop := context.AfterFunc(c.parent, cancel)
	release := func() { stop(); cancel() }
	select {
	case c.gate <- struct{}{}:
		if ctx.Err() != nil || c.parent.Err() != nil || c.closed {
			<-c.gate
			release()
			return nil, nil, errConsoleCanceled
		}
		return ctx, func() { <-c.gate; release() }, nil
	case <-ctx.Done():
		release()
		return nil, nil, errConsoleCanceled
	}
}

func consoleUnavailable() agenttui.ConsoleStatus {
	return agenttui.ConsoleStatus{State: agenttui.ConsoleUnavailable}
}

func (c *tuiConsole) Status(ctx context.Context) (agenttui.ConsoleStatus, error) {
	ctx, release, err := c.action(ctx, consoleActionTimeout)
	if err != nil {
		return consoleUnavailable(), err
	}
	defer release()
	status, _, err := c.discover(ctx)
	return status, err
}

func (c *tuiConsole) Start(ctx context.Context) (agenttui.ConsoleStatus, error) {
	ctx, release, err := c.action(ctx, consoleStartupTimeout)
	if err != nil {
		return consoleUnavailable(), err
	}
	defer release()
	return c.start(ctx)
}

func (c *tuiConsole) start(ctx context.Context) (agenttui.ConsoleStatus, error) {
	// A retained child's completed process handle is sufficient authority to
	// reap its exact discovery record. Upstream health is irrelevant here.
	if c.child != nil {
		select {
		case <-c.child.done:
			if err := c.stopChild(); err != nil {
				return consoleUnavailable(), err
			}
		default:
		}
	}
	status, _, err := c.discover(ctx)
	if err != nil || status.State != agenttui.ConsoleStopped {
		return status, err
	}
	if c.child != nil {
		if err := c.stopChild(); err != nil {
			return consoleUnavailable(), err
		}
	}
	if ctx.Err() != nil {
		return consoleUnavailable(), errConsoleCanceled
	}
	child, ready, err := c.spawn(ctx)
	if err != nil {
		return consoleUnavailable(), err
	}
	c.child = child
	select {
	case ok := <-ready:
		if ok && ctx.Err() == nil && c.parent.Err() == nil {
			status, entry, err := c.discover(ctx)
			if err == nil && status.State == agenttui.ConsoleRunning && entry.PID == child.cmd.Process.Pid && strings.HasSuffix(entry.AccessURL, "?token="+child.accessToken) {
				child.entry = entry
				status.Owned = true
				return status, nil
			}
		}
	case <-ctx.Done():
	case <-c.parent.Done():
	}
	// EOF, malformed readiness, cancellation and race loss all reap only the
	// retained child. Never signal a PID discovered from a registry record.
	if err := c.stopChild(); err != nil {
		return consoleUnavailable(), err
	}
	if ctx.Err() != nil || c.parent.Err() != nil {
		return consoleUnavailable(), errConsoleCanceled
	}
	// A race winner may be reused only after a fresh token and identity proof.
	status, _, err = c.discover(ctx)
	if err == nil && status.State == agenttui.ConsoleRunning {
		return status, nil
	}
	if err != nil {
		return status, err
	}
	return status, errConsoleStart
}

func (c *tuiConsole) Open(ctx context.Context) (agenttui.ConsoleStatus, error) {
	ctx, release, err := c.action(ctx, consoleStartupTimeout)
	if err != nil {
		return consoleUnavailable(), err
	}
	defer release()
	status, err := c.start(ctx)
	if err != nil {
		return status, err
	}
	status, entry, err := c.discover(ctx)
	if err != nil {
		return status, err
	}
	launchCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	matched, err := dashboard.WithRegistryInstance(launchCtx, entry, func() error {
		return launchCtx.Err()
	})
	if err != nil {
		return status, errConsoleOpen
	}
	if !matched {
		status, _, err = c.discover(ctx)
		if err != nil {
			return status, err
		}
		return status, errConsoleConflict
	}
	// The exact instance was rechecked immediately before opening. Do not
	// hold its discovery lock while waiting on an OS association handler:
	// independent shutdown must still be able to release its registry record.
	// The access token is scoped to that instance; a replacement cannot adopt it.
	if launchCtx.Err() != nil {
		return status, errConsoleOpen
	}
	if err := c.opener(launchCtx, entry.AccessURL); errors.Is(err, agenttui.ErrBrowserOutcomeUnknown) {
		return status, agenttui.ErrBrowserOutcomeUnknown
	} else if err != nil {
		return status, errConsoleOpen
	}
	return status, nil
}

func (c *tuiConsole) Stop(ctx context.Context) (agenttui.ConsoleStatus, error) {
	ctx, release, err := c.action(ctx, consoleActionTimeout)
	if err != nil {
		return consoleUnavailable(), err
	}
	defer release()
	if c.child != nil {
		// Stop our retained process even when its cell is down, its token was
		// revoked, or its discovery record changed. Cleanup never removes a
		// successor, and this action does not signal that successor's process.
		if err := c.stopChild(); err != nil {
			return consoleUnavailable(), err
		}
		if _, err := dashboard.ReadRegistryInstance(c.identity.AgentID); errors.Is(err, os.ErrNotExist) {
			return agenttui.ConsoleStatus{State: agenttui.ConsoleStopped}, nil
		}
		status, _, err := c.discover(ctx)
		return status, err
	}
	status, entry, err := c.discover(ctx)
	if err != nil || status.State != agenttui.ConsoleRunning {
		return status, err
	}
	matched, err := dashboard.WithRegistryInstance(ctx, entry, func() error {
		if ctx.Err() != nil {
			return errConsoleCanceled
		}
		return c.signal(entry)
	})
	if err != nil {
		return status, errConsoleStop
	}
	if !matched {
		status, _, err = c.discover(ctx)
		return status, err
	}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		current, readErr := dashboard.ReadRegistryInstance(entry.AgentID)
		if errors.Is(readErr, os.ErrNotExist) || (readErr == nil && !dashboard.SameRegistryInstance(current, entry)) {
			// A successor is not this stop's target. Report its actual verified state.
			status, _, err = c.discover(ctx)
			return status, err
		}
		if readErr != nil {
			return consoleUnavailable(), errConsoleUnavailable
		}
		select {
		case <-ctx.Done():
			return consoleUnavailable(), errConsoleStop
		case <-ticker.C:
		}
	}
}

func (c *tuiConsole) owns(entry dashboard.RegistryEntry) bool {
	if c.child == nil || c.child.entry.AgentID == "" {
		return false
	}
	select {
	case <-c.child.done:
		return false
	default:
	}
	return dashboard.SameRegistryInstance(c.child.entry, entry)
}

func (p *tuiConsoleProcess) closePipe() { p.once.Do(func() { _ = p.pipe.Close() }) }

func (c *tuiConsole) captureChildEntry(child *tuiConsoleProcess) {
	if child.entry.AgentID != "" {
		return
	}
	// This also runs after Wait: process liveness is deliberately unnecessary.
	// The retained handle plus the parent's unique private bootstrap token bind
	// the record to this child even if publication raced cancellation.
	entry, err := dashboard.ReadRegistryInstance(c.identity.AgentID)
	if err == nil && c.validEntry(entry) && consoleRegistryManagerMatches(entry, c.manager) && entry.PID == child.cmd.Process.Pid &&
		entry.AccountID == c.identity.AccountID && entry.RealmID == c.identity.RealmID &&
		strings.HasSuffix(entry.AccessURL, "?token="+child.accessToken) {
		child.entry = entry
	}
}

// stopChild gracefully closes the lifetime pipe, then forcibly cleans up only
// the retained process handle if necessary. Wait always runs, including failures.
func (c *tuiConsole) stopChild() error {
	child := c.child
	if child == nil {
		return nil
	}
	// Retain the exact private fence even if readiness failed after publication.
	// The bootstrap token binds it to this process, without registry-only ownership.
	c.captureChildEntry(child)
	child.closePipe()
	select {
	case <-child.done:
	case <-time.After(6 * time.Second):
		_ = child.cmd.Process.Kill()
		select {
		case <-child.done:
		case <-time.After(2 * time.Second):
			return errConsoleStop
		}
	}
	c.captureChildEntry(child)
	if child.entry.AgentID != "" {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := dashboard.ReleaseRegistryInstance(ctx, child.entry)
		cancel()
		if err != nil {
			// Keep the already-reaped child's exact fence so a later explicit
			// action can retry after a transient registry-lock failure.
			return errConsoleStop
		}
	}
	c.child = nil
	return nil
}

func (c *tuiConsole) Close() error {
	c.cancel()
	c.gate <- struct{}{}
	defer func() { <-c.gate }()
	if c.closed {
		return c.closeErr
	}
	c.closed = true
	c.closeErr = c.stopChild()
	return c.closeErr
}
