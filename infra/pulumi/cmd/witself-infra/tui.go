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
	"net/url"
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
// settingRow is one row in the context pane's settings block: the
// effective value a provisioning verb would apply, and whether it
// came straight from a built-in default (rendered dim) or was set
// explicitly in the config (rendered normal).
type settingRow struct {
	key, value string
	fromEntry  bool // true = came from cell entry or defaults block
}

type cellState struct {
	name         string
	entry        cellEntry
	fleet        *fleet.Cell // nil = not registered
	identity     identity
	err          error  // whoami / fleet error, if any
	controlPlane string // effective control plane URL ("" = self-hosted)
	settings     []settingRow
}

// selfHosted is the group label for cells with no control_plane in
// their effective config (a plain single-machine deploy — no fleet
// registry, no witself-admin visibility). Matches the "self-host vs
// Witself Cloud" split from the project direction.
const selfHosted = "self-hosted"

// groupLabel is the human-readable name for a control-plane URL, or
// "self-hosted" for the empty string. self.witwave.ai gets its
// friendly product name; anything else shows its hostname.
func groupLabel(controlPlane string) string {
	if controlPlane == "" {
		return selfHosted
	}
	host := controlPlane
	if u, err := url.Parse(controlPlane); err == nil && u.Host != "" {
		host = u.Host
	}
	if host == "self.witwave.ai" {
		return "witself cloud"
	}
	return host
}

// groupRank orders groups: default CP first (0), self-hosted last
// (large), other CPs in between (1). Ties resolve alphabetically at
// the call site.
func groupRank(cp, defaultCP string) int {
	switch {
	case cp == defaultCP && defaultCP != "":
		return 0
	case cp == "":
		return 2
	default:
		return 1
	}
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

// statusCellW is the fixed display width of the status column in the
// cells pane. Sized to the widest label ("draining") + the dot + a
// trailing gap so cell names always start at the same column.
const statusCellW = 12

func (c cellState) statusStyled() string {
	label, style := "● live", styOK
	switch c.status() {
	case "draining":
		label, style = "◐ draining", styWarn
	case "absent":
		label, style = "◌ absent", styDim
	case "error":
		label, style = "● error", styErr
	}
	// Pad the PLAIN label first, then style. Padding the styled output
	// would fold the trailing spaces into the SGR run and, more
	// importantly, complicate width math when styles differ between
	// terminals — cheaper and more predictable to pad in plain space.
	if pad := statusCellW - lipgloss.Width(label); pad > 0 {
		label += strings.Repeat(" ", pad)
	}
	return style.Render(label)
}

// effectiveSettings resolves each per-cell provisioning flag through
// the same precedence run() uses: cell entry > defaults > built-in.
// Provenance is recorded so the context pane can dim rows that came
// from a built-in default (nothing set in the config), which lets an
// operator scan for what was deliberately configured.
func effectiveSettings(e cellEntry, d *cellEntry) []settingRow {
	var out []settingRow
	str := func(key, builtin string, cell *string, getDef func(*cellEntry) *string) {
		if cell != nil {
			out = append(out, settingRow{key: key, value: *cell, fromEntry: true})
			return
		}
		if d != nil {
			if v := getDef(d); v != nil {
				out = append(out, settingRow{key: key, value: *v, fromEntry: true})
				return
			}
		}
		if builtin != "" {
			out = append(out, settingRow{key: key, value: builtin, fromEntry: false})
		}
	}
	boolean := func(key string, builtin bool, cell *bool, getDef func(*cellEntry) *bool) bool {
		if cell != nil {
			out = append(out, settingRow{key: key, value: boolLabel(*cell), fromEntry: true})
			return *cell
		}
		if d != nil {
			if v := getDef(d); v != nil {
				out = append(out, settingRow{key: key, value: boolLabel(*v), fromEntry: true})
				return *v
			}
		}
		out = append(out, settingRow{key: key, value: boolLabel(builtin), fromEntry: false})
		return builtin
	}
	// Order: infra shape first (what an operator checks before `up`),
	// then plumbing (channel, backend, domain). GitOps rows only when
	// argocd is on — off means they wouldn't apply.
	str("sizing", "minimal", e.Profile, func(x *cellEntry) *string { return x.Profile })
	str("k8s version", "1.36", e.K8sVersion, func(x *cellEntry) *string { return x.K8sVersion })
	str("db version", "18", e.DBVersion, func(x *cellEntry) *string { return x.DBVersion })
	str("cidr", "10.20.0.0/16", e.CIDR, func(x *cellEntry) *string { return x.CIDR })
	str("ingress", "cloudflare-tunnel", e.Ingress, func(x *cellEntry) *string { return x.Ingress })
	argocdOn := boolean("argocd", false, e.ArgoCD, func(x *cellEntry) *bool { return x.ArgoCD })
	if argocdOn {
		str("gitops repo", "https://github.com/witwave-ai/witself",
			gitField(e.Gitops, gitRepo), func(x *cellEntry) *string { return gitField(x.Gitops, gitRepo) })
		str("gitops rev", "main",
			gitField(e.Gitops, gitRev), func(x *cellEntry) *string { return gitField(x.Gitops, gitRev) })
	}
	str("channel", "experimental", e.Channel, func(x *cellEntry) *string { return x.Channel })
	str("backend", "s3", e.Backend, func(x *cellEntry) *string { return x.Backend })
	str("domain", "cells.witself.witwave.ai", e.Domain, func(x *cellEntry) *string { return x.Domain })
	return out
}

func boolLabel(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// gitField selects one nested gitopsEntry field, or nil when the
// block itself is unset. Kept small — only two of the four sub-fields
// (repo, revision) surface in the context pane today.
type gitFieldKey int

const (
	gitRepo gitFieldKey = iota
	gitRev
)

func gitField(g *gitopsEntry, f gitFieldKey) *string {
	if g == nil {
		return nil
	}
	switch f {
	case gitRepo:
		return g.Repo
	case gitRev:
		return g.Revision
	}
	return nil
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

	// Effective per-cell control plane = entry override, else defaults.
	defaultCP := controlPlane
	states := make([]cellState, 0, len(cfg.Cells))
	for n, entry := range cfg.Cells {
		cp := defaultCP
		if entry.ControlPlane != nil {
			cp = *entry.ControlPlane
		}
		st := cellState{name: n, entry: entry, fleet: byName[n], controlPlane: cp}
		st.settings = effectiveSettings(entry, cfg.Defaults)
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
	// Sort by group so cells with the same control plane are
	// contiguous. Group order: the default control plane first
	// (the one an operator sees in defaults:), other CPs alpha,
	// self-hosted last. Within a group, cells sort by name.
	sort.SliceStable(states, func(i, j int) bool {
		gi, gj := states[i].controlPlane, states[j].controlPlane
		if gi != gj {
			return groupRank(gi, defaultCP) < groupRank(gj, defaultCP)
		}
		return states[i].name < states[j].name
	})
	// Any remaining registered names are orphans — registered with the
	// control plane but not in the local inventory. Their group is
	// the CP they registered with, so they land next to their peers.
	for _, o := range registered {
		if _, still := byName[o.Name]; !still {
			continue
		}
		f := o
		states = append(states, cellState{name: o.Name, fleet: &f, controlPlane: defaultCP})
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
	case authCompletedMsg:
		if msg.err != nil {
			m.status = msg.desc + " failed: " + oneLine(msg.err.Error())
			return m, nil
		}
		m.status = msg.desc + " ✓ — refreshing"
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
	case "a":
		next, cmd := m.startAuth()
		return next, cmd
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
	// Tree layout: group cells by control_plane. m.states is already
	// sorted so members of a group are contiguous — walk and emit a
	// header whenever the group changes. Group headers are NOT
	// selectable (cursor indexes m.states, which has cells only), so
	// j/k moves through cells and skips headers naturally.
	prevGroup := "\x00" // sentinel that no real group can equal
	for i, st := range m.states {
		if st.controlPlane != prevGroup {
			if i > 0 {
				lines = append(lines, "")
			}
			n := 0
			for j := i; j < len(m.states) && m.states[j].controlPlane == st.controlPlane; j++ {
				n++
			}
			header := groupLabel(st.controlPlane)
			if st.controlPlane != "" {
				header = header + " · " + shortHost(st.controlPlane)
			}
			header = fmt.Sprintf("%s  %d cell%s", header, n, plural(n))
			lines = append(lines, styTitle.Render(fitLine(header, cellsContentW)))
			prevGroup = st.controlPlane
		}
		row := fmt.Sprintf("%s %s", st.statusStyled(), st.name)
		if i == m.cursor {
			row = "  ▸ " + row
		} else {
			row = "    " + row
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
		put("control plane", groupLabel(st.controlPlane))
		put("status", st.status())
		if st.fleet != nil {
			if st.fleet.Endpoint != "" {
				put("endpoint", st.fleet.Endpoint)
			}
			if st.fleet.Channel != "" {
				put("channel", st.fleet.Channel)
			}
		}
		// Settings — what a provisioning verb would use. Values from
		// the entry or defaults block render normal; values falling
		// back to a built-in render dim so an operator can scan for
		// what was deliberately configured before pressing `p`/`u`.
		if len(st.settings) > 0 {
			ctxLines = append(ctxLines, "")
			ctxLines = append(ctxLines, styTitle.Render("  settings"))
			for _, row := range st.settings {
				val := row.value
				if !row.fromEntry {
					val = styDim.Render(val)
				}
				label := styDim.Render(fmt.Sprintf("  %-14s ", row.key))
				ctxLines = append(ctxLines, fitLine(label+val, ctxContentW))
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
	hints := " j/k select · a auth · p preview · u up · D destroy · g refresh · q quit "
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

// shortHost returns just the hostname of a URL for the group header —
// enough to disambiguate two control planes without eating pane width
// with the full https://... prefix.
func shortHost(controlPlane string) string {
	if u, err := url.Parse(controlPlane); err == nil && u.Host != "" {
		return u.Host
	}
	return controlPlane
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
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
