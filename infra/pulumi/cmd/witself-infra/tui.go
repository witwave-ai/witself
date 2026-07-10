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

// rowKind names what m.cursor is pointing at right now — a cell or a
// control-plane header. Selection is uniform: j/k move through both,
// and the context pane dispatches on the selected row.
type rowKind int

const (
	rowCell rowKind = iota
	rowHeader
)

// row is one selectable line in the cells pane. Headers surface CP
// metadata; cells surface cell state.
type row struct {
	kind    rowKind
	cellIdx int    // when kind == rowCell: index into m.states
	cp      string // when kind == rowHeader: the control-plane URL
}

type dashboardModel struct {
	ctx           context.Context
	cli           cellDataSource
	configPath    string
	states        []cellState
	controlPlanes map[string]controlPlaneInfo
	cursor        int
	width         int
	height        int
	loading       bool
	status        string
	now           func() time.Time

	// Slice 4 ops state.
	program        *tea.Program
	op             *opRun
	lastOp         *opRun // the most recently completed op; kept so its tail is still scrollable
	opsScroll      int    // rows scrolled back from the live tail; 0 = follow
	pending        *confirmDialog
	previewSeen    map[string]bool // cells with a successful preview
	interruptModal bool            // ctrl+c while op running: keep/cancel/detach
	spinnerFrame   int             // advances every spinnerInterval while m.op != nil
}

// controlPlaneInfo carries per-CP metadata for the header context
// view — enough to answer "which fleet, is it reachable, how many
// cells does it know about" without a second network call.
type controlPlaneInfo struct {
	url        string
	tokenFile  string // resolved path or ""
	reachable  bool   // ListCells returned without error
	err        error  // nil when reachable, else the ListCells failure
	registered int    // cells the CP knows about
	configured int    // cells in the local infra.yaml belonging to this CP
	// Account counts across every cell this CP manages. Fetched via
	// GetPlacementStatus; hasAccounts is false when we couldn't reach
	// the endpoint (renders as "—" rather than a misleading 0).
	hasAccounts bool
	liveAccts   int // live accounts across all cells
	archived    int // accounts sitting in R2 awaiting placement
	blocked     int // archived accounts no eligible cell can accept
}

// loadResult carries everything one refresh brings back: the merged
// cell states AND per-CP metadata for header rows.
type loadResult struct {
	states        []cellState
	controlPlanes map[string]controlPlaneInfo
}

// cellDataSource is what the model needs from the outside world — a
// tiny interface so the model is testable without a control plane.
type cellDataSource interface {
	load(ctx context.Context, configPath string) (loadResult, error)
}

// liveDataSource reads infra.yaml, then the control plane, then runs
// whoami on each configured cell.
type liveDataSource struct{}

func (liveDataSource) load(ctx context.Context, configPath string) (loadResult, error) {
	cfg, _, err := loadInfraConfig(configPath)
	if err != nil {
		return loadResult{}, err
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
	cps := map[string]controlPlaneInfo{}
	if controlPlane != "" {
		info := controlPlaneInfo{url: controlPlane, tokenFile: tokenFile}
		if fc, ferr := fleet.NewClient(controlPlane, tokenFile); ferr == nil {
			cells, ferr := fc.ListCells(ctx)
			if ferr == nil {
				registered = cells
				info.reachable = true
				info.registered = len(cells)
			} else {
				info.err = ferr
			}
			// Placement status: live/archived/blocked account counts
			// across the CP. limit=1 keeps the sample lists tiny —
			// we only want the totals for the header context view.
			// Non-fatal on error; the header just shows "—".
			if ps, perr := fc.GetPlacementStatus(ctx, 1); perr == nil {
				info.hasAccounts = true
				info.liveAccts = ps.Live.Total
				info.archived = ps.Archived.Total
				info.blocked = len(ps.Archived.Blocked)
			}
		} else {
			info.err = ferr
		}
		cps[controlPlane] = info
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
	// Count configured cells per CP for the header context view.
	for _, st := range states {
		info := cps[st.controlPlane]
		info.url = st.controlPlane
		info.configured++
		cps[st.controlPlane] = info
	}
	return loadResult{states: states, controlPlanes: cps}, nil
}

type loadedMsg struct {
	states        []cellState
	controlPlanes map[string]controlPlaneInfo
	err           error
}
type refreshTickMsg struct{}
type spinnerTickMsg struct{}

const autoRefreshInterval = 60 * time.Second

// spinnerInterval controls how fast the running-op indicator throbs on
// the cell row and ops-pane title. 100ms is the standard bubbles/spin
// rate — fast enough to read as "alive," slow enough to not flicker.
const spinnerInterval = 100 * time.Millisecond

// spinnerFrames is the classic bubbletea braille spinner. Each frame
// is one cell wide, so swapping mid-column doesn't shift the layout.
var spinnerFrames = []string{"⣾", "⣽", "⣻", "⢿", "⡿", "⣟", "⣯", "⣷"}

func spinnerCmd() tea.Cmd {
	return tea.Tick(spinnerInterval, func(time.Time) tea.Msg { return spinnerTickMsg{} })
}

// opSpinnerLabel returns the fixed-width spinner cell for the running
// op — statusCellW cells wide so cell names still line up. Colored by
// kind: preview (green, read-only), up (yellow, mutating), destroy
// (red, irreversible) — same accents the ops pane already uses.
func opSpinnerLabel(kind opKind, frame int) string {
	ch := spinnerFrames[frame%len(spinnerFrames)]
	label := ch + " " + kind.verb()
	if pad := statusCellW - lipgloss.Width(label); pad > 0 {
		label += strings.Repeat(" ", pad)
	}
	var style lipgloss.Style
	switch kind {
	case opPreview:
		style = styOK
	case opUp:
		style = styWarn
	case opDestroy:
		style = styErr
	default:
		style = styTitle
	}
	return style.Render(label)
}

func (m dashboardModel) Init() tea.Cmd {
	return tea.Batch(m.loadCmd(), tickCmd())
}

func tickCmd() tea.Cmd {
	return tea.Tick(autoRefreshInterval, func(time.Time) tea.Msg { return refreshTickMsg{} })
}

func (m dashboardModel) loadCmd() tea.Cmd {
	ctx, cli, path := m.ctx, m.cli, m.configPath
	return func() tea.Msg {
		res, err := cli.load(ctx, path)
		return loadedMsg{states: res.states, controlPlanes: res.controlPlanes, err: err}
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
		firstLoad := len(m.states) == 0
		m.states = msg.states
		m.controlPlanes = msg.controlPlanes
		// Cursor indexes rows() (headers + cells). On the FIRST load,
		// jump past the initial header so the operator can immediately
		// act on a cell; on subsequent loads preserve position for a
		// user who has navigated to a header on purpose. Clamp against
		// the new row count regardless.
		rows := m.rows()
		if m.cursor >= len(rows) {
			m.cursor = max(len(rows)-1, 0)
		}
		if firstLoad {
			for i, r := range rows {
				if r.kind == rowCell {
					m.cursor = i
					break
				}
			}
		}
		m.status = fmt.Sprintf("%d cells · %s", len(m.states), m.now().UTC().Format("15:04:05"))
		return m, nil
	case refreshTickMsg:
		return m, tea.Batch(m.loadCmd(), tickCmd())
	case spinnerTickMsg:
		// Advance the throb frame; keep ticking only while an op is
		// running. Once m.op clears (opDoneMsg), the loop stops on its
		// own without any explicit stop signal.
		if m.op == nil {
			return m, nil
		}
		m.spinnerFrame++
		return m, spinnerCmd()
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
				// Auth-error nudge: if the op's tail contains a
				// recognizable "your credentials are expired" pattern,
				// tell the operator to press `a` — the alternative is
				// a manual `aws sso login` in another window and a
				// blind guess about what went wrong.
				if looksLikeAuthFailure(m.op.snapshot(20)) {
					m.status = m.op.kind.verb() + " on " + msg.cell + " failed on auth — press `a` to log in and try again"
				}
			} else {
				m.status = m.op.kind.verb() + " on " + msg.cell + " succeeded"
				// ANY successful mutation invalidates the previewed
				// plan: an up applied it, a destroy removed everything.
				// A subsequent up must start from a fresh preview.
				if m.op.kind == opUp || m.op.kind == opDestroy {
					delete(m.previewSeen, msg.cell)
				}
			}
			m.lastOp = m.op
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
		if m.cursor < len(m.rows())-1 {
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
	case "pgup":
		return m.scrollOps(+1), nil
	case "pgdown":
		return m.scrollOps(-1), nil
	case "home":
		// End of history — oldest lines. Rare want for a live log,
		// but symmetric with End (below) and cheap.
		if op := m.opsSource(); op != nil {
			m.opsScroll = op.maxScroll(m.opsViewH())
		}
	case "end":
		m.opsScroll = 0
	}
	return m, nil
}

// opsSource picks which op the pane is looking at: the running one if
// there is one, else the last completed one. Same rule as the render.
func (m dashboardModel) opsSource() *opRun {
	if m.op != nil {
		return m.op
	}
	return m.lastOp
}

// opsViewH returns the ops pane's content height (rows). Matches the
// adaptive rule the View uses so scroll math and paint math agree.
func (m dashboardModel) opsViewH() int {
	if m.op != nil {
		return 8
	}
	return 4
}

// scrollOps steps the ops-pane view by `pages` view-heights (positive
// = further into history, negative = back toward tail). Clamped so we
// never scroll past the oldest kept line or below the live tail.
func (m dashboardModel) scrollOps(pages int) dashboardModel {
	op := m.opsSource()
	if op == nil {
		return m
	}
	view := m.opsViewH()
	m.opsScroll += pages * view
	if m.opsScroll < 0 {
		m.opsScroll = 0
	}
	if maxOff := op.maxScroll(view); m.opsScroll > maxOff {
		m.opsScroll = maxOff
	}
	return m
}

// rows builds the flat selectable list: one header per control-plane
// group followed by that group's cells. m.states is already sorted so
// same-group cells are contiguous.
func (m dashboardModel) rows() []row {
	var out []row
	prev := "\x00" // sentinel — no real CP URL can equal
	for i, st := range m.states {
		if st.controlPlane != prev {
			out = append(out, row{kind: rowHeader, cp: st.controlPlane})
			prev = st.controlPlane
		}
		out = append(out, row{kind: rowCell, cellIdx: i})
	}
	return out
}

// currentRow returns the row under the cursor, or a zero row when the
// inventory is empty.
func (m dashboardModel) currentRow() row {
	rows := m.rows()
	if m.cursor < 0 || m.cursor >= len(rows) {
		return row{}
	}
	return rows[m.cursor]
}

// selectedCell returns the cell name the cursor points at, or "" when
// the cursor is on a header row (or the inventory is empty).
func (m dashboardModel) selectedCell() string {
	r := m.currentRow()
	if r.kind != rowCell || r.cellIdx < 0 || r.cellIdx >= len(m.states) {
		return ""
	}
	return m.states[r.cellIdx].name
}

// selectedState returns the cell state the cursor points at, or nil
// when on a header.
func (m dashboardModel) selectedState() *cellState {
	r := m.currentRow()
	if r.kind != rowCell || r.cellIdx < 0 || r.cellIdx >= len(m.states) {
		return nil
	}
	return &m.states[r.cellIdx]
}

func (m dashboardModel) startOpKey(key string) (tea.Model, tea.Cmd) {
	cell := m.selectedCell()
	if cell == "" {
		if m.currentRow().kind == rowHeader {
			m.status = "select a cell (j/k) — provisioning verbs don't apply to control planes"
		} else {
			m.status = "no cell selected"
		}
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
	// Up refuses upstream of the confirm dialog when preview hasn't
	// succeeded — matches the dimmed hint in the footer and makes the
	// "why nothing happened" message explicit rather than hiding the
	// answer inside a dialog the operator would have to open first.
	if kind == opUp && !m.previewSeen[cell] {
		m.status = "run `p` (preview) first — up won't fire until you've seen the diff for " + cell
		return m, nil
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
	// New op: reset scroll to the live tail and drop the previous
	// completed op's tail — the operator's asking to watch this run,
	// not the last one.
	m.op = op
	m.lastOp = nil
	m.opsScroll = 0
	m.spinnerFrame = 0
	if op.logPath != "" {
		m.status = kind.verb() + " running on " + cell + " · logs " + op.logPath
	} else {
		m.status = kind.verb() + " running on " + cell
	}
	// Kick off the throb loop. spinnerTickMsg self-terminates when
	// m.op clears on opDoneMsg.
	return m, spinnerCmd()
}

// looksLikeAuthFailure scans an op's log tail for phrases the three
// cloud CLIs print when credentials are expired or missing. Keeping
// it phrase-based (not error-code based) so it survives Pulumi's
// error-wrapping — the operator's error message often mentions
// "authorization" long before Pulumi's own error struct does.
func looksLikeAuthFailure(tail []string) bool {
	// Case-insensitive prefix scan against the phrases each cloud CLI
	// prints when credentials are missing or expired. Broad phrases
	// (SSO token, sso session) catch AWS's own wording plus Pulumi's
	// wrapping ("failed to refresh cached SSO token").
	markers := []string{
		"sso token",
		"sso session",
		"aws sso login",
		"unable to locate credentials",
		"expiredtoken",
		"reauthentication is needed",
		"gcloud auth", // covers `gcloud auth login`, `application-default login`
		"application default credentials",
		"authenticationfailed",
		"az login",
	}
	for _, line := range tail {
		low := strings.ToLower(line)
		for _, m := range markers {
			if strings.Contains(low, m) {
				return true
			}
		}
	}
	return false
}

// footerHints builds the key-hint line with letters dimmed when the
// action they trigger isn't currently available. The rules match what
// the handlers themselves enforce, so the visual state and the
// keyboard behavior can never disagree: an op is running (freezes
// everything except q/ctrl+c), or the cursor is on a header (verbs
// don't apply), or up needs a preview first.
func (m dashboardModel) footerHints() string {
	// Parts is a list of (letter, label, enabled) — rendered as
	// "letter label", the letter dim when disabled, joined by " · ".
	type part struct {
		letter, label string
		enabled       bool
	}
	// Default availability: cell-focused actions require a cell row,
	// no op currently running, and (for `a`) an auth error on the cell.
	stp := m.selectedState()
	haveCell := stp != nil
	idle := m.op == nil
	authRelevant := idle && haveCell && stp.err != nil
	previewOK := haveCell && m.previewSeen[stp.name]
	// Ops scroll is available whenever there's something in the ops
	// pane — during a running op or after one completed. The `end`
	// hint is redundant when already at tail (opsScroll == 0), but
	// harmless — hint density matters less than discoverability here.
	scrollable := m.opsSource() != nil
	parts := []part{
		{"j/k", "select", true},
		{"a", "auth", authRelevant},
		{"p", "preview", idle && haveCell},
		{"u", "up", idle && haveCell && previewOK},
		{"D", "destroy", idle && haveCell},
		{"g", "refresh", idle},
		{"PgUp/Dn", "log", scrollable},
		{"q", "quit", true},
	}
	segs := make([]string, 0, len(parts))
	for _, p := range parts {
		text := p.letter + " " + p.label
		if !p.enabled {
			text = styDim.Render(text)
		}
		segs = append(segs, text)
	}
	return strings.Join(segs, " · ")
}

// renderContext builds the right-hand pane content based on what the
// cursor is on. A header row shows fleet metadata (URL, token file,
// reachability, cell counts); a cell row shows the cell's full
// context (config, settings, identity, fleet state). Empty inventory
// gets a hint.
func (m dashboardModel) renderContext(ctxContentW int) []string {
	var ctxLines []string
	put := func(k, v string) {
		if v == "" {
			return
		}
		label := styDim.Render(fmt.Sprintf("  %-14s ", k))
		ctxLines = append(ctxLines, fitLine(label+v, ctxContentW))
	}
	if len(m.states) == 0 {
		ctxLines = append(ctxLines, styDim.Render("no cells configured — `witself-infra config add-cell …`"))
		return ctxLines
	}
	r := m.currentRow()
	switch r.kind {
	case rowHeader:
		// Control-plane header context.
		info, hasInfo := m.controlPlanes[r.cp]
		if !hasInfo {
			info = controlPlaneInfo{url: r.cp}
		}
		put("control plane", groupLabel(r.cp))
		if r.cp != "" {
			put("url", r.cp)
			if info.tokenFile != "" {
				put("token file", info.tokenFile)
			}
			reach := styOK.Render("✓ reachable")
			if !info.reachable {
				reach = styErr.Render("✗ unreachable")
			}
			ctxLines = append(ctxLines, fitLine("  "+reach, ctxContentW))
			if !info.reachable && info.err != nil {
				ctxLines = append(ctxLines, fitLine(styWarn.Render("  · "+oneLine(info.err.Error())), ctxContentW))
			}
		} else {
			ctxLines = append(ctxLines, fitLine(styDim.Render("  (no control plane — plain self-hosted deploys)"), ctxContentW))
		}
		ctxLines = append(ctxLines, "")
		ctxLines = append(ctxLines, styTitle.Render("  cells"))
		put("configured", fmt.Sprintf("%d", info.configured))
		if r.cp != "" {
			put("registered", fmt.Sprintf("%d", info.registered))
			// Orphans: registered but not configured locally. Not a
			// bug per se, but worth calling out — the operator can
			// bring them into inventory or purge them from the fleet.
			orphans := info.registered - info.configured
			if orphans > 0 {
				put("orphans", fmt.Sprintf("%d (registered but not in local infra.yaml)", orphans))
			}
		}
		// Accounts across all cells this CP manages: live customers
		// running today, plus anything sitting in the R2 archive
		// awaiting placement. Blocked accounts are the ones no
		// eligible cell can currently accept — worth calling out in
		// red because they need operator attention.
		if r.cp != "" {
			ctxLines = append(ctxLines, "")
			ctxLines = append(ctxLines, styTitle.Render("  accounts"))
			if !info.hasAccounts {
				put("status", styDim.Render("(placement status unavailable)"))
			} else {
				put("live", fmt.Sprintf("%d", info.liveAccts))
				put("archived", fmt.Sprintf("%d awaiting placement", info.archived))
				if info.blocked > 0 {
					label := styDim.Render(fmt.Sprintf("  %-14s ", "blocked"))
					val := styErr.Render(fmt.Sprintf("%d no eligible cell", info.blocked))
					ctxLines = append(ctxLines, fitLine(label+val, ctxContentW))
				}
			}
		}
		return ctxLines
	case rowCell:
		st := m.states[r.cellIdx]
		e := st.entry
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
		if len(st.settings) > 0 {
			ctxLines = append(ctxLines, "")
			ctxLines = append(ctxLines, styTitle.Render("  settings"))
			for _, srow := range st.settings {
				val := srow.value
				if !srow.fromEntry {
					val = styDim.Render(val)
				}
				label := styDim.Render(fmt.Sprintf("  %-14s ", srow.key))
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
	}
	return ctxLines
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
	// Tree layout: iterate rows() so the render mirrors the selection
	// model. Each row is either a group header (selectable, styled as
	// a header) or a cell (selectable, indented under its header).
	rows := m.rows()
	for i, r := range rows {
		selected := i == m.cursor
		switch r.kind {
		case rowHeader:
			// Blank spacer above every header except the first, to
			// separate groups.
			if i > 0 {
				lines = append(lines, "")
			}
			// Count how many cells belong to this group.
			n := 0
			for j := i + 1; j < len(rows) && rows[j].kind == rowCell; j++ {
				n++
			}
			label := groupLabel(r.cp)
			if r.cp != "" {
				label = label + " · " + shortHost(r.cp)
			}
			label = fmt.Sprintf("%s  %d cell%s", label, n, plural(n))
			// Bold always; the cursor arrow calls out selection.
			text := styTitle.Render(label)
			if selected {
				text = "▸ " + text
			} else {
				text = "  " + text
			}
			lines = append(lines, fitLine(text, cellsContentW))
		case rowCell:
			st := m.states[r.cellIdx]
			// Throb: swap the static status marker for a live spinner on
			// the cell currently being provisioned/de-provisioned. Same
			// fixed statusCellW width so nothing shifts around it.
			var marker string
			if m.op != nil && m.op.cell == st.name {
				marker = opSpinnerLabel(m.op.kind, m.spinnerFrame)
			} else {
				marker = st.statusStyled()
			}
			text := fmt.Sprintf("%s %s", marker, st.name)
			if selected {
				text = "  ▸ " + text
			} else {
				text = "    " + text
			}
			lines = append(lines, fitLine(text, cellsContentW))
		}
	}
	if len(lines) == 0 {
		lines = append(lines, styDim.Render("no cells configured — `witself-infra config add-cell …`"))
	}
	cellsPane := paneBox("cells · "+fmt.Sprintf("%d", len(m.states)), lines, cellsContentW, topH, true)

	// Context pane — dispatches on the row under the cursor. A header
	// row renders the control-plane summary; a cell row renders the
	// cell's full context (identity, settings, fleet state).
	ctxLines := m.renderContext(ctxContentW)
	contextPane := paneBox("context", ctxLines, ctxContentW, topH, false)

	top := lipgloss.JoinHorizontal(lipgloss.Top, cellsPane, contextPane)

	// Ops log pane below the two top panes. Renders the live op if
	// there is one, else the last completed op's tail so its output
	// stays scrollable until you launch something else. Scroll offset
	// travels back through the ring buffer; new lines land at the
	// bottom without yanking the view when scrolled up.
	var logLines []string
	title := "operations"
	active := m.op
	if active == nil {
		active = m.lastOp
	}
	if active != nil {
		logLines = active.tailFrom(m.opsScroll, opsH)
		for i := range logLines {
			logLines[i] = fitLine(logLines[i], opsContentW)
		}
		state := active.kind.verb()
		if active.isDone() {
			state += " (done)"
		} else {
			// Same throb frame as the cell row — reads as one indicator
			// on two surfaces, not two flickers out of sync.
			state = spinnerFrames[m.spinnerFrame%len(spinnerFrames)] + " " + state
		}
		title = fmt.Sprintf("operations · %s %s", state, active.cell)
		if m.opsScroll > 0 {
			title += fmt.Sprintf(" · scrolled %d lines (End to follow)", m.opsScroll)
		}
	}
	opsPane := "\n" + paneBox(title, logLines, opsContentW, opsH, m.op != nil)

	// Footer: hints left, version tag right. Keys whose actions aren't
	// currently available get their letter dimmed inline so an operator
	// can see WHY nothing happens ("u is dim → I need to press p
	// first") without pressing the key and reading the status line.
	// On narrow terminals the version wins over hints (hints are
	// re-learnable; "am I current?" isn't).
	hints := " " + m.footerHints() + " "
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

	// Float dialogs over the middle of the frame instead of stacking
	// below it. Stacking overflowed the terminal on tall dialogs (the
	// destroy typing prompt, the up confirmation) and bubbletea's
	// altscreen sheared the top border to compensate. overlayCenter
	// splices ANSI-safe, so pane borders stay intact around the box.
	// Dialog width is capped to fit the terminal so word-wrap kicks in
	// on narrow screens instead of overflowing the frame.
	dlgContentW := min(max(m.width-8, 30), 76)
	if m.pending != nil {
		return overlayCenter(rendered, dialogBox(m.pending.render(), dlgContentW), m.width)
	}
	if m.interruptModal && m.op != nil {
		// The "detach and quit" option promises what we can't reliably
		// deliver on POSIX without a re-parenting helper (SIGPIPE kills
		// the child seconds after the parent exits). Offer keep/cancel
		// only until Slice 4b implements a real detach.
		body := fmt.Sprintf(
			"An operation is running: %s %s\n\n[k] keep it running (default)\n[c] cancel it (SIGKILL to the process group)\n\ndetaching a running op is not yet supported — see WITSELF_HOME/logs/infra after the op completes.\n",
			m.op.kind.verb(), m.op.cell)
		return overlayCenter(rendered, dialogBox(body, dlgContentW), m.width)
	}
	return rendered
}

// dialogBox frames a confirmation body with a thick yellow border.
// contentW is the max inner width; lipgloss word-wraps longer lines so
// a 130-char destroy warning still fits inside an 80-column terminal.
func dialogBox(body string, contentW int) string {
	st := lipgloss.NewStyle().
		Border(lipgloss.ThickBorder()).
		BorderForeground(lipgloss.Color("3")).
		Padding(0, 2).
		Width(contentW)
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
