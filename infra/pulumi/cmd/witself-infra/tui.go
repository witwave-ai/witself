package main

// `witself-infra dashboard` — the read-only infra dashboard (Slice 3
// of the arc). Reads the cell inventory from infra.yaml and the
// fleet registry from the control plane, and shows a two-pane view:
// cells list on the left, the selected cell's identity + context on
// the right. No mutations here — provisioning verbs (slice 4) hang
// off this same view later.
//
// The visual language matches witself-admin's dashboard on purpose:
// paneBox with a thick focused border, tab-cycled focus, one-line
// footer with hints on the left and the version tag on the right.

import (
	"context"
	"flag"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

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
		id, err := whoamiCell(ctx, n, configPath)
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
			m.cursor = maxInt(len(m.states)-1, 0)
		}
		m.status = fmt.Sprintf("%d cells · %s", len(m.states), m.now().UTC().Format("15:04:05"))
		return m, nil
	case refreshTickMsg:
		return m, tea.Batch(m.loadCmd(), tickCmd())
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
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
		}
	}
	return m, nil
}

func (m dashboardModel) View() string {
	w, h := m.width, m.height
	if w < 60 {
		w = 100
	}
	if h < 15 {
		h = 30
	}
	footerH := 1
	contentH := maxInt(h-footerH-3, 6) // -3 = pane border + title
	cellsW := minInt(maxInt(w/2, 30), 60)
	contextW := w - cellsW - 2 // account for JoinHorizontal spacing

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
		lines = append(lines, fitLineInfra(row, cellsW-2))
	}
	if len(lines) == 0 {
		lines = append(lines, styDim.Render("no cells configured — `witself-infra config add-cell …`"))
	}
	cellsPane := paneBoxInfra("cells · "+fmt.Sprintf("%d", len(m.states)), lines, cellsW-2, contentH, true)

	// Context pane
	var ctxLines []string
	if len(m.states) > 0 && m.cursor >= 0 && m.cursor < len(m.states) {
		st := m.states[m.cursor]
		e := st.entry
		put := func(k, v string) {
			if v == "" {
				return
			}
			ctxLines = append(ctxLines, styDim.Render(fmt.Sprintf("  %-14s ", k))+v)
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
			ctxLines = append(ctxLines, styErr.Render("  "+oneLineInfra(st.err.Error())))
		} else if st.identity.Cloud != "" {
			id := st.identity
			put = func(k, v string) {
				if v == "" {
					return
				}
				ctxLines = append(ctxLines, styDim.Render(fmt.Sprintf("  %-14s ", k))+v)
			}
			put("profile", id.Profile)
			put("account", id.Account)
			put("tenant", id.Tenant)
			put("actor", id.Actor)
			ok := styOK.Render("✓ matches config pin")
			if !id.OK {
				ok = styErr.Render("✗ pin mismatch")
			}
			ctxLines = append(ctxLines, "  "+ok)
			for _, n := range id.Notes {
				ctxLines = append(ctxLines, styWarn.Render("  · "+oneLineInfra(n)))
			}
		}
	} else {
		ctxLines = append(ctxLines, styDim.Render("select a cell to see its identity"))
	}
	contextPane := paneBoxInfra("context", ctxLines, contextW-2, contentH, false)

	top := lipgloss.JoinHorizontal(lipgloss.Top, cellsPane, contextPane)

	// Footer: hints left, version tag right.
	hints := " j/k select · g refresh · q quit "
	ver := " witself-infra v" + versionString() + " "
	pad := w - lipgloss.Width(hints) - lipgloss.Width(ver)
	var footer string
	if pad >= 1 {
		footer = hints + strings.Repeat(" ", pad) + ver
	} else {
		footer = hints + " " + ver
	}
	status := ""
	if m.status != "" {
		status = "\n" + styDim.Render(" "+m.status)
	}
	return top + "\n" + styDim.Render(footer) + status
}

// paneBoxInfra frames one pane with a thick border and bold title —
// same idiom as the witself-admin dashboard.
func paneBoxInfra(title string, lines []string, contentW, contentH int, focused bool) string {
	if len(lines) > contentH {
		lines = lines[:contentH]
	}
	for len(lines) < contentH {
		lines = append(lines, "")
	}
	body := styTitle.Render(fitLineInfra(title, contentW)) + "\n" + strings.Join(lines, "\n")
	st := lipgloss.NewStyle().
		Border(lipgloss.ThickBorder()).
		Padding(0, 1).
		Width(contentW + 2)
	if focused {
		st = st.BorderForeground(lipgloss.Color("6"))
	}
	return st.Render(body)
}

func fitLineInfra(s string, width int) string {
	if width <= 1 {
		return ""
	}
	if lipgloss.Width(s) <= width {
		return s
	}
	// Simple width-aware truncation — ANSI-agnostic is fine here; the
	// styled prefixes fit the pane by construction.
	runes := []rune(s)
	if len(runes) <= width {
		return s
	}
	return string(runes[:width-1]) + "…"
}

func oneLineInfra(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.ReplaceAll(s, "\t", " ")
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
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
	_, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
	return err
}

// versionString is a shim: witself-infra doesn't inject its version
// via ldflags today (an open item), so the footer shows "dev" until
// that lands. Kept as a function so the fix is one edit.
func versionString() string { return "dev" }
