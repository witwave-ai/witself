package agenttui

import (
	"context"
	"errors"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// ErrBrowserOutcomeUnknown means launch succeeded but its early-exit window elapsed.
// It is informational and contains no URL or launcher error text.
var ErrBrowserOutcomeUnknown = errors.New("browser launched; outcome unknown")

type consoleAction uint8

const (
	consoleNone consoleAction = iota
	consoleCheck
	consoleStart
	consoleOpen
	consoleStop
)

type consoleModel struct {
	status       ConsoleStatus
	known        bool
	busy         bool
	action       consoleAction
	pending      consoleAction
	generation   uint64
	cancel       context.CancelFunc
	interrupted  bool
	notice       string
	noticeAction consoleAction
}

// Command messages deliberately contain only fixed lifecycle metadata. Even a
// misbehaving adapter's error must not carry an access URL into the UI event loop.
type consoleMsg struct {
	generation uint64
	action     consoleAction
	status     ConsoleStatus
	unknown    bool
	failed     bool
}
type consoleTickMsg struct{}

func (m *Model) consoleEnabled() bool { return !m.opts.Demo && m.opts.Console != nil }

func (m *Model) consoleTick() tea.Cmd {
	if !m.consoleEnabled() || m.ctx.Err() != nil {
		return nil
	}
	return tea.Tick(10*time.Second, func(time.Time) tea.Msg { return consoleTickMsg{} })
}

func (m *Model) consoleRun(action consoleAction) tea.Cmd {
	if !m.consoleEnabled() {
		if action != consoleCheck {
			m.notice = "Web console controls are available in a connected workspace."
		}
		return nil
	}
	if m.ctx.Err() != nil || m.closed.Load() {
		return nil
	}
	c := &m.console
	if c.busy {
		// Polling never replaces an intentional action. Coalesce key repeats,
		// but preserve the latest differing intent and serialize all effects.
		if action != consoleCheck {
			if action != c.action || c.pending != consoleNone || c.interrupted {
				c.pending = action
			}
			if action == consoleStop && c.action != consoleStop {
				c.interrupted = true
				c.cancel()
			}
		}
		return nil
	}
	c.busy, c.action, c.interrupted = true, action, false
	c.generation++
	timeout := 20 * time.Second
	if action == consoleCheck {
		timeout = 4 * time.Second
	} else {
		c.notice = ""
		m.notice = consoleProgress(action)
	}
	ctx, cancel := context.WithTimeout(m.ctx, timeout)
	c.cancel = cancel
	controller, generation := m.opts.Console, c.generation
	return func() tea.Msg {
		defer cancel()
		var status ConsoleStatus
		var err error
		switch action {
		case consoleStart:
			status, err = controller.Start(ctx)
		case consoleOpen:
			status, err = controller.Open(ctx)
		case consoleStop:
			status, err = controller.Stop(ctx)
		default:
			status, err = controller.Status(ctx)
		}
		status, valid := safeConsoleStatus(status)
		if err != nil && action == consoleCheck && status.State != ConsoleConflict {
			status = ConsoleStatus{State: ConsoleUnavailable}
		}
		switch action {
		case consoleOpen, consoleStart:
			valid = valid && status.State == ConsoleRunning
		case consoleStop:
			valid = valid && status.State == ConsoleStopped
		}
		return consoleMsg{generation: generation, action: action, status: status, unknown: errors.Is(err, ErrBrowserOutcomeUnknown), failed: (err != nil && !errors.Is(err, ErrBrowserOutcomeUnknown)) || !valid}
	}
}

func safeConsoleStatus(s ConsoleStatus) (ConsoleStatus, bool) {
	switch s.State {
	case ConsoleRunning:
		if s.Port < 1 || s.Port > 65535 {
			return ConsoleStatus{State: ConsoleUnavailable}, false
		}
		return s, true
	case ConsoleUnavailable, ConsoleStopped, ConsoleConflict:
		return ConsoleStatus{State: s.State}, true
	default:
		return ConsoleStatus{State: ConsoleUnavailable}, false
	}
}

func (m *Model) consoleResult(msg consoleMsg) tea.Cmd {
	c := &m.console
	if !c.busy || msg.generation != c.generation || msg.action != c.action {
		return nil
	}
	c.cancel()
	c.cancel, c.busy = nil, false
	c.status, c.known = msg.status, true
	if msg.action != consoleCheck || msg.failed {
		c.notice = consoleOutcome(msg)
		c.noticeAction = msg.action
		if msg.action != consoleCheck {
			m.notice = c.notice
		}
	} else if c.noticeAction == consoleCheck {
		c.notice = ""
	}
	if c.pending != consoleNone {
		next := c.pending
		c.pending = consoleNone
		return m.consoleRun(next)
	}
	return nil
}

func consoleProgress(action consoleAction) string {
	switch action {
	case consoleStart:
		return "Starting web console…"
	case consoleOpen:
		return "Opening web console in your browser…"
	case consoleStop:
		return "Stopping web console…"
	default:
		return "Checking web console…"
	}
}

func consoleOutcome(msg consoleMsg) string {
	if msg.status.State == ConsoleConflict {
		return "A console was found, but its identity could not be verified. It was left untouched."
	}
	if msg.failed {
		if msg.action == consoleOpen && msg.status.State == ConsoleRunning {
			return "Console is running; browser could not be opened. Use Web console: b open."
		}
		if msg.action == consoleStop && msg.status.State == ConsoleRunning {
			return "Console is still running; stop did not complete. Use Web console: x stop."
		}
		return "Web console action did not complete. Use Web console: r check status."
	}
	switch msg.action {
	case consoleOpen:
		if msg.unknown {
			return "Browser launched; opening outcome is unknown."
		}
		return "Web console opened in your browser."
	case consoleStart:
		return "Web console is running. Use Web console: b open."
	case consoleStop:
		return "Web console is off."
	default:
		return ""
	}
}

func (m *Model) consoleKey(k string) tea.Cmd {
	switch k {
	case "esc", "q", "w", "?":
		m.overlay = ""
	case "s":
		return m.consoleRun(consoleStart)
	case "b", "enter":
		return m.consoleRun(consoleOpen)
	case "x":
		return m.consoleRun(consoleStop)
	case "r":
		return m.consoleRun(consoleCheck)
	}
	return nil
}

func (m *Model) consoleBadge() string {
	if !m.consoleEnabled() {
		return ""
	}
	if m.console.busy && m.console.action != consoleCheck {
		return "web …"
	}
	if !m.console.known {
		return "web ?"
	}
	switch m.console.status.State {
	case ConsoleRunning:
		return "web on"
	case ConsoleStopped:
		return "web off"
	case ConsoleConflict:
		return "web conflict"
	default:
		return "web ?"
	}
}

func (m *Model) consoleView(w, h int) string {
	c := m.colors()
	lines := []string{m.style(c.accent).Bold(true).Render("WEB CONSOLE"), "s start   b open browser   x stop", "r check status   Esc close", ""}
	if !m.consoleEnabled() {
		lines = append(lines, "Available in a connected workspace.", "Demo stays entirely offline.")
	} else {
		label, detail := "Status unknown", "Press r to check, or b to start and open."
		if m.console.known {
			switch m.console.status.State {
			case ConsoleStopped:
				label, detail = "○ Off", "Press b to start and open in your browser."
			case ConsoleRunning:
				label = "● Running · existing session"
				detail = "Quitting the TUI leaves this console running. Stop explicitly with x."
				if m.console.status.Owned {
					label = "● Running · started here"
					detail = "Quitting the TUI also stops this console."
				}
			case ConsoleConflict:
				label, detail = "Identity could not be verified", "The existing console has been left untouched."
			case ConsoleUnavailable:
				label, detail = "Status unavailable", "Press r to retry the status check."
			}
		}
		lines = append(lines, m.style(c.cyan).Bold(true).Render(label))
		if m.console.busy {
			lines = append(lines, consoleProgress(m.console.action))
		}
		if m.console.pending != consoleNone {
			lines = append(lines, "Next: "+strings.TrimSuffix(consoleProgress(m.console.pending), "…"))
		}
		lines = append(lines, "", detail, "", "Uses this workspace's verified agent and opens only on your computer.")
		if m.console.notice != "" {
			lines = append(lines, "", m.console.notice)
		}
	}
	wrapped := []string{}
	for _, line := range lines {
		wrapped = append(wrapped, strings.Split(ansi.Wrap(line, max(1, w), ""), "\n")...)
	}
	return crop(strings.Join(wrapped, "\n"), w, h)
}
