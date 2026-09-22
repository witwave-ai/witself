package agenttui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

type fakeConsole struct {
	calls []consoleAction
	run   func(context.Context, consoleAction) (ConsoleStatus, error)
}

func (f *fakeConsole) call(ctx context.Context, action consoleAction) (ConsoleStatus, error) {
	f.calls = append(f.calls, action)
	if f.run != nil {
		return f.run(ctx, action)
	}
	if action == consoleStop || action == consoleCheck {
		return ConsoleStatus{State: ConsoleStopped}, nil
	}
	return ConsoleStatus{State: ConsoleRunning, Owned: true, Port: 8123}, nil
}
func (f *fakeConsole) Status(ctx context.Context) (ConsoleStatus, error) {
	return f.call(ctx, consoleCheck)
}
func (f *fakeConsole) Start(ctx context.Context) (ConsoleStatus, error) {
	return f.call(ctx, consoleStart)
}
func (f *fakeConsole) Open(ctx context.Context) (ConsoleStatus, error) {
	return f.call(ctx, consoleOpen)
}
func (f *fakeConsole) Stop(ctx context.Context) (ConsoleStatus, error) {
	return f.call(ctx, consoleStop)
}
func (f *fakeConsole) Close() error { panic("application owns controller Close") }

func consoleTestModel(t *testing.T, controller *fakeConsole) *Model {
	t.Helper()
	m := testModel(t, newFake())
	m.opts.Demo = false
	m.opts.Console = controller
	return m
}

func runConsole(t *testing.T, m *Model, cmd tea.Cmd) tea.Cmd {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected a console command")
	}
	msg := cmd()
	if _, ok := msg.(consoleMsg); !ok {
		t.Fatalf("unexpected message %T", msg)
	}
	_, next := m.Update(msg)
	return next
}

func TestConsoleDemoAndUnavailableStayOffline(t *testing.T) {
	for _, demo := range []bool{true, false} {
		f := &fakeConsole{}
		m := consoleTestModel(t, f)
		m.opts.Demo = demo
		if !demo {
			m.opts.Console = nil
		}
		if m.consoleTick() != nil || m.consoleRun(consoleCheck) != nil {
			t.Fatal("unavailable controller scheduled work")
		}
		for _, k := range []string{"b", "w", "s", "b", "x", "r"} {
			_, cmd := m.Update(key(k))
			if cmd != nil {
				t.Fatalf("key %q scheduled console work", k)
			}
		}
		if len(f.calls) != 0 {
			t.Fatal("offline UI called controller")
		}
	}
}

func TestConsoleKeysRespectFilterAndOverlay(t *testing.T) {
	f := &fakeConsole{}
	m := consoleTestModel(t, f)
	openPanel(t, m, 5)
	m.filterEditing = true
	for _, k := range []string{"b", "w", "s", "x"} {
		m.Update(key(k)) // don't execute unrelated data refresh
	}
	if m.states[5].filter != "bwsx" || m.overlay != "" || m.console.busy {
		t.Fatal("console keys escaped filter input")
	}
	m.filterEditing = false
	_, cmd := m.Update(key("w"))
	runConsole(t, m, cmd)
	for _, k := range []string{"s", "b", "x", "r"} {
		_, cmd = m.Update(key(k))
		runConsole(t, m, cmd)
	}
	if m.sent || m.expanded[5] || m.overlay != "console" {
		t.Fatal("console keys changed the underlying panel")
	}
	m.Update(key("esc"))
	if m.overlay != "" {
		t.Fatal("escape did not dismiss console")
	}
}

func TestConsoleSingleFlightPreservesIntentAcrossNavigation(t *testing.T) {
	f := &fakeConsole{}
	m := consoleTestModel(t, f)
	check := m.consoleRun(consoleCheck)
	_, cmd := m.Update(key("b"))
	if cmd != nil || m.console.pending != consoleOpen {
		t.Fatal("open was not queued behind status")
	}
	m.Update(key("2")) // data-view generation must not invalidate console
	open := runConsole(t, m, check)
	if open == nil || m.console.action != consoleOpen {
		t.Fatal("queued open was lost")
	}
	m.Update(key("b"))
	m.consoleRun(consoleCheck)
	if next := runConsole(t, m, open); next != nil {
		t.Fatal("repeated open spawned another browser")
	}
	if !slices.Equal(f.calls, []consoleAction{consoleCheck, consoleOpen}) || m.console.status.State != ConsoleRunning {
		t.Fatal("console lifecycle did not settle")
	}
	m.Update(consoleMsg{generation: 1, action: consoleCheck, status: ConsoleStatus{State: ConsoleStopped}})
	if m.console.status.State != ConsoleRunning {
		t.Fatal("stale status overwrote running state")
	}
}

func TestConsoleStopCancelsPendingStartAndLatestIntentWins(t *testing.T) {
	f := &fakeConsole{}
	m := consoleTestModel(t, f)
	start := m.consoleRun(consoleStart)
	f.run = func(ctx context.Context, action consoleAction) (ConsoleStatus, error) {
		if action == consoleStart {
			if ctx.Err() == nil {
				t.Fatal("stop did not cancel startup")
			}
			return ConsoleStatus{State: ConsoleStopped}, ctx.Err()
		}
		return ConsoleStatus{State: ConsoleRunning, Owned: true, Port: 1234}, nil
	}
	m.consoleRun(consoleStop)
	m.consoleRun(consoleOpen)
	open := runConsole(t, m, start)
	runConsole(t, m, open)
	if !slices.Equal(f.calls, []consoleAction{consoleStart, consoleOpen}) {
		t.Fatal("latest intent did not replace queued stop")
	}
}

func TestConsolePartialSuccessAndPrivateErrorBoundary(t *testing.T) {
	const hostile = "synthetic-private-error-http://127.0.0.1/?token=DO-NOT-RENDER"
	f := &fakeConsole{run: func(context.Context, consoleAction) (ConsoleStatus, error) {
		return ConsoleStatus{State: ConsoleRunning, Owned: true, Port: 1234}, errors.New(hostile)
	}}
	m := consoleTestModel(t, f)
	cmd := m.consoleRun(consoleOpen)
	msg := cmd()
	if strings.Contains(fmt.Sprint(msg), hostile) {
		t.Fatal("raw error entered a UI message")
	}
	m.Update(msg)
	if m.console.status.State != ConsoleRunning || !m.console.status.Owned || !strings.Contains(m.notice, "browser could not") {
		t.Fatal("browser failure lost running console")
	}
	m.overlay = "console"
	if strings.Contains(m.View(), hostile) || !strings.Contains(m.View(), "started here") {
		t.Fatal("unsafe or misleading console presentation")
	}
	runConsole(t, m, m.consoleRun(consoleCheck))
	if m.console.status.State != ConsoleUnavailable {
		t.Fatal("failed status probe advertised a running console")
	}
}

func TestConsoleRejectsInvalidAndContradictoryStatus(t *testing.T) {
	for _, status := range []ConsoleStatus{
		{State: ConsoleRunning, Port: 0}, {State: ConsoleRunning, Port: 65536},
		{State: ConsoleState(255), Owned: true, Port: 1000}, {State: ConsoleStopped},
	} {
		f := &fakeConsole{run: func(context.Context, consoleAction) (ConsoleStatus, error) { return status, nil }}
		m := consoleTestModel(t, f)
		runConsole(t, m, m.consoleRun(consoleOpen))
		if m.console.status.State == ConsoleRunning || m.console.status.Owned || strings.Contains(m.notice, "opened in") {
			t.Fatal("invalid status accepted as success")
		}
	}
}

func TestConsoleStatusRecoveryClearsCheckFailure(t *testing.T) {
	f := &fakeConsole{run: func(context.Context, consoleAction) (ConsoleStatus, error) {
		return ConsoleStatus{State: ConsoleUnavailable}, errors.New("synthetic failure")
	}}
	m := consoleTestModel(t, f)
	runConsole(t, m, m.consoleRun(consoleCheck))
	if m.console.notice == "" {
		t.Fatal("check failure was not reported")
	}
	f.run = nil
	runConsole(t, m, m.consoleRun(consoleCheck))
	if m.console.notice != "" || m.console.status.State != ConsoleStopped {
		t.Fatal("successful check retained obsolete failure")
	}
}

func TestConsoleEntryHidesVisibleAndInflightPrivateValues(t *testing.T) {
	for _, k := range []string{"b", "w"} {
		m := consoleTestModel(t, &fakeConsole{})
		openPanel(t, m, 2)
		m.private.Lock()
		m.private.text, m.private.status = "SYNTHETIC-REVEAL-CANARY", "visible"
		generation := m.private.generation
		ctx, cancel := context.WithCancel(context.Background())
		m.private.cancel = cancel
		m.private.Unlock()
		m.Update(key(k))
		m.Update(privateMsg{generation: generation})
		text, _, _, _ := m.privateView()
		if text != "" || ctx.Err() == nil || strings.Contains(m.View(), "SYNTHETIC-REVEAL-CANARY") {
			t.Fatalf("%s did not clear/fence private state", k)
		}
	}
}

func TestConsoleQuitCancelsOperationsWithoutTakingApplicationOwnership(t *testing.T) {
	f := &fakeConsole{run: func(ctx context.Context, _ consoleAction) (ConsoleStatus, error) {
		if ctx.Err() == nil {
			t.Fatal("quit did not cancel operation")
		}
		return ConsoleStatus{State: ConsoleStopped}, ctx.Err()
	}}
	m := consoleTestModel(t, f)
	cmd := m.consoleRun(consoleOpen)
	m.Update(key("q"))
	m.Close()
	m.Update(cmd())
	if m.View() != "" || m.console.known {
		t.Fatal("late operation changed closed UI")
	}
}

func TestConsoleOverlayControlsFitCompactTerminals(t *testing.T) {
	for _, width := range []int{42, 80, 120} {
		m := consoleTestModel(t, &fakeConsole{})
		m.Update(tea.WindowSizeMsg{Width: width, Height: 18})
		m.overlay = "console"
		runConsole(t, m, m.consoleRun(consoleOpen))
		view := ansi.Strip(m.View())
		for _, label := range []string{"s start", "b open", "x stop", "Esc close", "started here"} {
			if !strings.Contains(view, label) {
				t.Fatalf("width %d missing %q", width, label)
			}
		}
		for _, line := range strings.Split(view, "\n") {
			if ansi.StringWidth(line) > width {
				t.Fatalf("width %d overflow", width)
			}
		}
	}
}
