package main

// `witself-infra dashboard` — the infra operator dashboard. Reads the
// cell inventory from infra.yaml and the fleet registry from the
// control plane; shows cells on the left, the selected cell's
// identity + context on the right, and a running operation's output
// below. p/u/D run preview/up/destroy as subprocesses (tui_ops.go)
// behind the confirmation rules documented there.
//
// The visual language matches witself-admin's dashboard on purpose:
// thick-bordered panes with bold titles, one-line footer with hints
// on the left and the version tag on the right.

import (
	"context"
	"flag"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	xansi "github.com/charmbracelet/x/ansi"

	"github.com/witwave-ai/witself/infra/pulumi/internal/fleet"
)

var (
	styTitle = lipgloss.NewStyle().Bold(true)
	styDim   = lipgloss.NewStyle().Faint(true)
	styOK    = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	styWarn  = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	styErr   = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
)

// cellState is the merged view of one cell across the config file and
// the fleet registry.
type cellState struct {
	name     string
	entry    cellEntry
	fleet    *fleet.Cell // nil = not registered
	identity identity
	err      error // whoami / fleet error, if any
}

func (c cellState) status() string {
	switch {
	case c.err != nil:
		return "error"
	case c.fleet == nil:
		return "absent"
	case c.fleet.Accepting != nil && !*c.fleet.Accepting:
		return "draining"
	default:
		return "live"
	}
}

func (c cellState) statusStyled() string {
	switch c.status() {
	case "live":
		return styOK.Render("● live")
	case "draining":
		return styWarn.Render("◐ draining")
	case "absent":
		return styDim.Render("◌ absent")
	case "error":
		return styErr.Render("● error")
	}
	return c.status()
}

type dashboardModel struct {
	ctx        context.Context
	cli        cellDataSource
	configPath string
	states     []cellState
	cursor     int
	width      int
	height     int
	loading    bool
	status     string
	now        func() time.Time

	// Slice 4 ops state.
	program        *tea.Program
	op             *opRun
	pending        *confirmDialog
	previewSeen    map[string]bool // cells with a successful preview
	interruptModal bool            // ctrl+c while op running: keep/cancel/detach
}

// cellDataSource is what the model needs from the outside world — a
// tiny interface so the model is testable without a control plane.
type cellDataSource interface {
	load(ctx context.Context, configPath string) ([]cellState, error)
}

// liveDataSource reads infra.yaml, then the control plane, then runs
// whoami on each configured cell.
type liveDataSource struct{}

func (liveDataSource) load(ctx context.Context, configPath string) ([]cellState, error) {
	cfg, _, err := loadInfraConfig(configPath)
	if err != nil {
		return nil, err
	}
	// Fleet lookup: fold registered cells into the merged view so the
	// dashboard can call out orphans (registered but not configured).
	var registered []fleet.Cell
	var controlPlane, tokenFile string
	if d := cfg.Defaults; d != nil {
		if d.ControlPlane != nil {
			controlPlane = *d.ControlPlane
		}
		if d.FleetTokenFile != nil {
			tokenFile = *d.FleetTokenFile
		}
	}
	if controlPlane != "" {
		if fc, ferr := fleet.NewClient(controlPlane, tokenFile); ferr == nil {
			if cells, ferr := fc.ListCells(ctx); ferr == nil {
				registered = cells
			}
		}
	}
	byName := map[string]*fleet.Cell{}
	for i := range registered {
		byName[registered[i].Name] = &registered[i]
	}

	names := make([]string, 0, len(cfg.Cells))
	for n := range cfg.Cells {
		names = append(names, n)
	}
	sort.Strings(names)
	states := make([]cellState, 0, len(names))
	for _, n := range names {
		st := cellState{name: n, entry: cfg.Cells[n], fleet: byName[n]}
		// Silent path: the dashboard iterates every cell every 60s and
		// on every g/opDone; the interactive `aws sso login` fallback
		// would paint the browser banner over the altscreen and
		// contend for stdin. Operators run `witself-infra whoami` (or
		// hit up/preview/destroy — those still refresh) to fix stale
		// SSO.
		id, err := whoamiCellSilent(ctx, n, configPath)
		if err != nil {
			st.err = err
		} else {
			st.identity = id
		}
		delete(byName, n)
		states = append(states, st)
	}
	// Any remaining registered names are orphans — registered with the
	// control plane but not in the local inventory.
	for _, o := range registered {
		if _, still := byName[o.Name]; !still {
			continue
		}
		f := o
		states = append(states, cellState{name: o.Name, fleet: &f})
	}
	return states, nil
}

type loadedMsg struct {
	states []cellState
	err    error
}
type refreshTickMsg struct{}

const autoRefreshInterval = 60 * time.Second

func (m dashboardModel) Init() tea.Cmd {
	return tea.Batch(m.loadCmd(), tickCmd())
}

func tickCmd() tea.Cmd {
	return tea.Tick(autoRefreshInterval, func(time.Time) tea.Msg { return refreshTickMsg{} })
}

func (m dashboardModel) loadCmd() tea.Cmd {
	ctx, cli, path := m.ctx, m.cli, m.configPath
	return func() tea.Msg {
		states, err := cli.load(ctx, path)
		return loadedMsg{states: states, err: err}
	}
}

func (m dashboardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case loadedMsg:
		m.loading = false
		if msg.err != nil {
			m.status = "load failed: " + msg.err.Error()
			return m, nil
		}
		m.states = msg.states
		if m.cursor >= len(m.states) {
			m.cursor = max(len(m.states)-1, 0)
		}
		m.status = fmt.Sprintf("%d cells · %s", len(m.states), m.now().UTC().Format("15:04:05"))
		return m, nil
	case refreshTickMsg:
		return m, tea.Batch(m.loadCmd(), tickCmd())
	case opLineMsg:
		return m, nil // re-render tick; the ring buffer already holds the line
	case opDoneMsg:
		if m.op != nil {
			if msg.err == nil && m.op.kind == opPreview {
				if m.previewSeen == nil {
					m.previewSeen = map[string]bool{}
				}
				m.previewSeen[msg.cell] = true
				m.status = "preview complete on " + msg.cell + " — press u to apply"
			} else if msg.err != nil {
				m.status = m.op.kind.verb() + " on " + msg.cell + " FAILED: " + oneLine(msg.err.Error())
				// Failed preview must NOT arm up; a failed up leaves a
				// partial diff so the last previewed plan is stale too.
				delete(m.previewSeen, msg.cell)
			} else {
				m.status = m.op.kind.verb() + " on " + msg.cell + " succeeded"
				// ANY successful mutation invalidates the previewed
				// plan: an up applied it, a destroy removed everything.
				// A subsequent up must start from a fresh preview.
				if m.op.kind == opUp || m.op.kind == opDestroy {
					delete(m.previewSeen, msg.cell)
				}
			}
			m.op = nil
			// Clear the interrupt modal if the op finished while it
			// was open — the modal's copy of m.op is stale, and every
			// modal-key branch (k/c/d) would otherwise nil-panic.
			m.interruptModal = false
		}
		return m, m.loadCmd()
	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m dashboardModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Ctrl+C safety modal — never quits under a running op.
	if m.interruptModal {
		switch msg.String() {
		case "k":
			m.interruptModal = false
			if m.op != nil {
				m.status = "keeping " + m.op.kind.verb() + " running"
			} else {
				m.status = "op already finished — no action needed"
			}
			return m, nil
		case "c":
			m.interruptModal = false
			if m.op != nil {
				m.op.killOp()
				m.status = "sent SIGKILL to " + m.op.kind.verb() + " on " + m.op.cell
			}
			return m, nil
		case "d":
			// Detach is intentionally unsupported until a proper re-
			// parenting helper lands — see the modal text and
			// opRun.detach for the honest reason.
			if m.op != nil {
				if err := m.op.detach(); err != nil {
					m.interruptModal = false
					m.status = err.Error()
					return m, nil
				}
			}
			return m, tea.Quit
		}
		return m, nil
	}

	// Confirmation dialog captures keys.
	if m.pending != nil {
		return m.handleConfirmKey(msg)
	}

	switch msg.String() {
	case "ctrl+c":
		if m.op != nil {
			m.interruptModal = true
			return m, nil
		}
		return m, tea.Quit
	case "q":
		if m.op != nil {
			m.status = "an op is running — ctrl+c for keep/cancel"
			return m, nil
		}
		return m, tea.Quit
	case "j", "down":
		if m.cursor < len(m.states)-1 {
			m.cursor++
		}
	case "k", "up":
		if m.cursor > 0 {
			m.cursor--
		}
	case "g":
		m.loading = true
		m.status = "refreshing…"
		return m, m.loadCmd()
	case "p", "u", "D":
		return m.startOpKey(msg.String())
	}
	return m, nil
}

func (m dashboardModel) selectedCell() string {
	if m.cursor >= 0 && m.cursor < len(m.states) {
		return m.states[m.cursor].name
	}
	return ""
}

func (m dashboardModel) startOpKey(key string) (tea.Model, tea.Cmd) {
	cell := m.selectedCell()
	if cell == "" {
		m.status = "no cell selected"
		return m, nil
	}
	if m.op != nil {
		m.status = "another op is running — wait or ctrl+c to cancel"
		return m, nil
	}
	var kind opKind
	switch key {
	case "p":
		kind = opPreview
	case "u":
		kind = opUp
	case "D":
		kind = opDestroy
	}
	if kind == opPreview {
		return m.launchOp(kind, cell)
	}
	m.pending = startConfirm(kind, cell, m.previewSeen[cell])
	return m, nil
}

func (m dashboardModel) handleConfirmKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	s := msg.String()
	if s == "esc" {
		m.pending = nil
		m.status = "confirmation dismissed"
		return m, nil
	}
	if s == "y" && m.pending.canConfirm() {
		kind, cell := m.pending.kind, m.pending.cell
		m.pending = nil
		return m.launchOp(kind, cell)
	}
	if m.pending.kind == opDestroy {
		switch {
		case s == "backspace":
			if n := len(m.pending.typed); n > 0 {
				m.pending.typed = m.pending.typed[:n-1]
			}
		case len(s) == 1: // single printable char
			m.pending.typed += s
		}
		if m.pending.typed == m.pending.cell {
			m.pending.err = ""
		} else if len(m.pending.typed) > 0 && !strings.HasPrefix(m.pending.cell, m.pending.typed) {
			m.pending.err = "does not match — press esc to start over"
		}
	}
	return m, nil
}

func (m dashboardModel) launchOp(kind opKind, cell string) (tea.Model, tea.Cmd) {
	op, err := startOp(teaProgram, kind, cell, m.configPath)
	if err != nil {
		m.status = "launch failed: " + err.Error()
		return m, nil
	}
	m.op = op
	m.status = kind.verb() + " running on " + cell
	return m, nil
}

func (m dashboardModel) View() string {
	w, h := m.width, m.height
	// bubbletea sends WindowSizeMsg on startup; skip the first render
	// tick when we haven't heard it yet rather than paint a wrong-size
	// frame that jumps on the next tick.
	if w <= 0 || h <= 0 {
		return ""
	}
	// Minimum viable: hints line ~65 chars wide; top row (contentH ≥ 6
	// + 3 for border/title) + ops (4+3) + footer (1) + newline = 17.
	// Round up for safety.
	if w < 66 || h < 18 {
		return styDim.Render("terminal too small — need at least 66×18")
	}

	// Row budget. paneBox draws contentH + 3 rows (title + content + 2
	// border rows). The frame is: top row of panes (topH+3) + ops pane
	// (opsH+3) + one newline separator + footer row + optional status
	// row = h. So topH + opsH = h − 8.
	//
	// Ops pane is adaptive: a small idle frame keeps the layout stable
	// but hands most of the height to the panes that carry information
	// when nothing is running. It grows to fit the log when an op is
	// live — logs are the whole point at that moment.
	opsH := 4
	if m.op != nil {
		opsH = 8
	}
	// The frame is: top(topH+3) + ops(opsH+3) + footer(1) + status(1
	// when present) rows — see the invariant in the min-size check
	// above. No floor: if this yields < 1, we already refused to draw.
	topH := h - opsH - 8
	// Column budget. paneBox draws contentW + 4 cells (2 padding + 2
	// border). Two panes at outer widths cellsOuter + ctxOuter must
	// equal w exactly, so their contentW args are outer − 4. Cells
	// content is short (cell names ~24 chars) — capping around 44
	// keeps the pane readable and hands the rest to context.
	cellsOuter := min(max(w/3, 30), 44)
	ctxOuter := w - cellsOuter
	cellsContentW, ctxContentW := cellsOuter-4, ctxOuter-4
	opsContentW := w - 4

	// Cells pane
	var lines []string
	if m.loading && len(m.states) == 0 {
		lines = append(lines, styDim.Render("loading inventory…"))
	}
	for i, st := range m.states {
		row := fmt.Sprintf("%s  %s", st.statusStyled(), st.name)
		if i == m.cursor {
			row = "▸ " + row
		} else {
			row = "  " + row
		}
		lines = append(lines, fitLine(row, cellsContentW))
	}
	if len(lines) == 0 {
		lines = append(lines, styDim.Render("no cells configured — `witself-infra config add-cell …`"))
	}
	cellsPane := paneBox("cells · "+fmt.Sprintf("%d", len(m.states)), lines, cellsContentW, topH, true)

	// Context pane
	var ctxLines []string
	if len(m.states) > 0 && m.cursor >= 0 && m.cursor < len(m.states) {
		st := m.states[m.cursor]
		e := st.entry
		put := func(k, v string) {
			if v == "" {
				return
			}
			label := styDim.Render(fmt.Sprintf("  %-14s ", k))
			ctxLines = append(ctxLines, fitLine(label+v, ctxContentW))
		}
		put("cell", st.name)
		if e.Cloud != nil {
			put("cloud", *e.Cloud)
		}
		if e.Region != nil {
			put("region", *e.Region)
		}
		put("status", st.status())
		if st.fleet != nil {
			if st.fleet.Endpoint != "" {
				put("endpoint", st.fleet.Endpoint)
			}
			if st.fleet.Channel != "" {
				put("channel", st.fleet.Channel)
			}
		}
		ctxLines = append(ctxLines, "")
		ctxLines = append(ctxLines, styTitle.Render("  identity"))
		if st.err != nil {
			ctxLines = append(ctxLines, fitLine(styErr.Render("  "+oneLine(st.err.Error())), ctxContentW))
		} else if st.identity.Cloud != "" {
			id := st.identity
			put("profile", id.Profile)
			put("account", id.Account)
			put("tenant", id.Tenant)
			put("actor", id.Actor)
			ok := styOK.Render("✓ matches config pin")
			if !id.OK {
				ok = styErr.Render("✗ pin mismatch")
			}
			ctxLines = append(ctxLines, fitLine("  "+ok, ctxContentW))
			for _, n := range id.Notes {
				ctxLines = append(ctxLines, fitLine(styWarn.Render("  · "+oneLine(n)), ctxContentW))
			}
		}
	} else {
		ctxLines = append(ctxLines, styDim.Render("select a cell to see its identity"))
	}
	contextPane := paneBox("context", ctxLines, ctxContentW, topH, false)

	top := lipgloss.JoinHorizontal(lipgloss.Top, cellsPane, contextPane)

	// Ops log pane below the two top panes. Always rendered (stable
	// layout beats a pane that pops in and out); title carries the
	// running op's cell when one is live.
	var logLines []string
	title := "operations"
	if m.op != nil {
		logLines = m.op.snapshot(opsH)
		for i := range logLines {
			logLines[i] = fitLine(logLines[i], opsContentW)
		}
		title = fmt.Sprintf("operations · %s %s", m.op.kind.verb(), m.op.cell)
	}
	opsPane := "\n" + paneBox(title, logLines, opsContentW, opsH, m.op != nil)

	// Footer: hints left, version tag right. On narrow terminals the
	// version wins over hints (hints are re-learnable; "am I current?"
	// is the question the stamp exists to answer). Same rule the
	// witself-admin dashboard uses.
	hints := " j/k select · p preview · u up · D destroy · g refresh · q quit "
	ver := " witself-infra v" + versionString() + " "
	pad := w - lipgloss.Width(hints) - lipgloss.Width(ver)
	var footer string
	if pad >= 1 {
		footer = hints + strings.Repeat(" ", pad) + ver
	} else {
		footer = fitLine(hints, max(w-lipgloss.Width(ver)-1, 10)) + " " + ver
	}
	status := ""
	if m.status != "" {
		status = "\n" + styDim.Render(" "+m.status)
	}
	rendered := top + opsPane + "\n" + styDim.Render(footer) + status

	// Overlay a dialog when we have one to show. Simple stacked layout
	// (not centered-splice like witself-admin) — this binary's dialogs
	// are safety-critical, not decorative.
	if m.pending != nil {
		return rendered + "\n" + dialogBox(m.pending.render())
	}
	if m.interruptModal && m.op != nil {
		// The "detach and quit" option promises what we can't reliably
		// deliver on POSIX without a re-parenting helper (SIGPIPE kills
		// the child seconds after the parent exits). Offer keep/cancel
		// only until Slice 4b implements a real detach.
		body := fmt.Sprintf(
			"An operation is running: %s %s\n\n[k] keep it running (default)\n[c] cancel it (SIGKILL to the process group)\n\ndetaching a running op is not yet supported — see WITSELF_HOME/logs/infra after the op completes.\n",
			m.op.kind.verb(), m.op.cell)
		return rendered + "\n" + dialogBox(body)
	}
	return rendered
}

func dialogBox(body string) string {
	st := lipgloss.NewStyle().
		Border(lipgloss.ThickBorder()).
		BorderForeground(lipgloss.Color("3")).
		Padding(0, 2)
	return st.Render(body)
}

// paneBox frames one pane with a thick border and bold title —
// same idiom as the witself-admin dashboard.
func paneBox(title string, lines []string, contentW, contentH int, focused bool) string {
	if len(lines) > contentH {
		lines = lines[:contentH]
	}
	for len(lines) < contentH {
		lines = append(lines, "")
	}
	body := styTitle.Render(fitLine(title, contentW)) + "\n" + strings.Join(lines, "\n")
	st := lipgloss.NewStyle().
		Border(lipgloss.ThickBorder()).
		Padding(0, 1).
		Width(contentW + 2)
	if focused {
		st = st.BorderForeground(lipgloss.Color("6"))
	}
	return st.Render(body)
}

func fitLine(s string, width int) string {
	if width <= 1 {
		return ""
	}
	if lipgloss.Width(s) <= width {
		return s
	}
	// ANSI-aware: styled strings (styErr.Render(...), styDim.Render(...))
	// get their color runs preserved and the ellipsis lands OUTSIDE any
	// SGR sequence. Rune-slicing would corrupt escape codes and break
	// terminal rendering.
	return xansi.Truncate(s, width, "…")
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.ReplaceAll(s, "\t", " ")
}

// runDashboard is the `dashboard` subcommand.
func runDashboard(fs *flag.FlagSet) error {
	configPath := fs.Lookup("config").Value.String()
	m := dashboardModel{
		ctx:        context.Background(),
		cli:        liveDataSource{},
		configPath: configPath,
		now:        time.Now,
		loading:    true,
		status:     "loading inventory…",
	}
	// Ops subprocesses push lines via program.Send. Because bubbletea
	// takes the model by VALUE, the model can't hold a live pointer to
	// its own program without a two-step init: build the program with a
	// nil-program model, then update the program's stored model right
	// after via Send. That's fragile — instead, hold the program in a
	// package-level pointer set here so opRun goroutines can reach it.
	m.program = nil
	prog := tea.NewProgram(m, tea.WithAltScreen())
	teaProgram = prog
	_, err := prog.Run()
	return err
}

// teaProgram is the running dashboard's tea.Program handle, set for
// the duration of a Run so opRun goroutines can push messages back.
// Only one dashboard runs at a time.
var teaProgram *tea.Program

func versionString() string { return buildVersion }
