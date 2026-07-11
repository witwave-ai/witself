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
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os/exec"
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
	// styPlan marks "previewed — up armed": cyan, deliberately outside
	// the green/yellow/red state palette because it's workflow state,
	// not cell health. Matches the focused-pane border accent.
	styPlan = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	// styOrange sits between yellow (transitional) and red (down) on the
	// health scale — "degraded but still serving". 256-color 208.
	styOrange = lipgloss.NewStyle().Foreground(lipgloss.Color("208"))
)

// healthLevel is the four-plus-unknown scale the Health tab paints.
// The ordering matters: worst wins when a line aggregates several
// sub-signals.
type healthLevel int

const (
	healthUnknown  healthLevel = iota // dim — not probed / not applicable
	healthGood                        // green — healthy
	healthWarn                        // yellow — transitional / in flight
	healthDegraded                    // orange — degraded but serving
	healthBad                         // red — down / failed
)

func (l healthLevel) style() lipgloss.Style {
	switch l {
	case healthGood:
		return styOK
	case healthWarn:
		return styWarn
	case healthDegraded:
		return styOrange
	case healthBad:
		return styErr
	default:
		return styDim
	}
}

// dot is the colored bullet for a health line. Unknown uses a hollow
// ring to read as "no reading yet" rather than a lit indicator.
func (l healthLevel) dot() string {
	glyph := "●"
	if l == healthUnknown {
		glyph = "◌"
	}
	return l.style().Render(glyph)
}

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
	previewSeen    map[string]planEntry // cell → its last passed preview (time + config fingerprint)
	planPath       string               // persisted plan state; "" disables persistence (tests)
	configFinger   string               // fingerprint of the loaded config; binds armed plans to it
	interruptModal bool                 // ctrl+c while op running: keep/cancel/detach
	spinnerFrame   int                  // advances every spinnerInterval while m.op != nil
	opGen          int                  // increments on each launchOp; ticks tag their generation

	// Context-pane tabs. focus governs which pane the arrow keys drive;
	// activeTab is global state so it sticks as the operator moves the
	// cell cursor up and down.
	focus     paneFocus
	activeTab contextTab

	// reach caches the last control-plane reachability probe per cell,
	// for the Health tab. Probes fire only while that tab is showing a
	// cell — no background cost when you're not looking.
	reach map[string]cellReach
	// health caches the last cell-health subprocess report per cell
	// (Kubernetes / Database / Argo). Heavier than reach, so it runs on
	// a longer cadence (healthReportTTL).
	health map[string]cellHealthState
	// healthFrame advances while a Health-tab probe is in flight so the
	// "checking…" lines animate; healthAnimGen fences stale ticks the
	// same way opGen does for the op spinner.
	healthFrame   int
	healthAnimGen int
	// lastLoad is when the inventory (credentials/fleet data) last
	// refreshed — the age shown for those lines.
	lastLoad time.Time
}

// cellHealthState is one cell's cached subsystem-health report plus the
// in-flight flag for its subprocess probe.
type cellHealthState struct {
	probed   time.Time
	inflight bool
	report   cellHealthReport
	err      error
}

// cellReach is one cell's cached reachability state: the last probe's
// result plus whether one is in flight right now.
type cellReach struct {
	probed   time.Time
	inflight bool
	res      reachResult
	err      error
}

// reachTTL is how long a reachability probe stays fresh before a tab
// re-entry or cursor move re-probes. Short — this is a live health
// view — but long enough to avoid re-probing on every keystroke.
const reachTTL = 15 * time.Second

// healthReportTTL is the cadence for the heavier cell-health
// subprocess. Longer than reachTTL because each run selects a Pulumi
// stack and reaches the cluster — not something to repeat on a hover.
const healthReportTTL = 45 * time.Second

// Background probing keeps live cells' health warm so the Health tab
// shows fresh data instead of a "checking…" wait. bgProbeInterval is
// the sweep cadence; bgHealthTTL is how stale a cell's report may get
// before the sweep refreshes it. Reach (cheap) refreshes every sweep;
// health (a Pulumi stack select) is rate-limited to one probe per
// sweep across the whole fleet so we never spawn six at once.
const (
	bgProbeInterval = 30 * time.Second
	bgHealthTTL     = 60 * time.Second
)

// paneFocus names which pane the arrow keys currently drive. Cells is
// the default — j/k always move the cell cursor regardless — but tab
// shifts focus to the context pane so ←/→ pick its tab.
type paneFocus int

const (
	focusCells paneFocus = iota
	focusContext
)

// contextTab identifies one tab of the context pane. Overview holds
// the identity/settings/fleet content; Kubernetes and Database are
// scaffolding for later; Health aggregates per-cell health lines.
type contextTab int

const (
	tabOverview contextTab = iota
	tabKubernetes
	tabDatabase
	tabHealth
)

// contextTabs is the ordered tab bar. The index into this slice is
// what activeTab holds and what ←/→ step through.
var contextTabs = []struct {
	tab  contextTab
	name string
}{
	{tabOverview, "Overview"},
	{tabKubernetes, "Kubernetes"},
	{tabDatabase, "Database"},
	{tabHealth, "Health"},
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
// cell states, per-CP metadata for header rows, and a fingerprint of
// the config file that produced it (so a persisted preview can be
// bound to the exact config it was run against).
type loadResult struct {
	states        []cellState
	controlPlanes map[string]controlPlaneInfo
	configFinger  string
}

// reachResult is one control-plane reachability probe of a cell: did
// the Worker's GET on <cell>/v1/version succeed, and what did it see.
type reachResult struct {
	ok      bool
	reason  string
	version string
	status  int
}

// cellDataSource is what the model needs from the outside world — a
// tiny interface so the model is testable without a control plane.
// probe asks the control plane to reach one cell (used by the Health
// tab); its view of reachability is authoritative because the Worker,
// not the operator's local resolver, is what serves customers.
type cellDataSource interface {
	load(ctx context.Context, configPath string) (loadResult, error)
	probe(ctx context.Context, configPath, cell string) (reachResult, error)
	// probeHealth runs the cell-health subprocess and returns its
	// per-subsystem report (Kubernetes / Database / Argo). Heavier than
	// probe — it selects the Pulumi stack and reaches the cluster — so
	// the model runs it on a longer cadence.
	probeHealth(ctx context.Context, configPath, cell string) (cellHealthReport, error)
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
	return loadResult{states: states, controlPlanes: cps, configFinger: configFileFingerprint(configPath)}, nil
}

// probe asks the cell's control plane to reach it. It rebuilds a fleet
// client per call — cheap, and it keeps the dashboard's probe path
// stateless like its load path. A cell with no control plane (plain
// self-hosted) can't be probed this way; that surfaces as an error the
// Health line renders as unknown.
func (liveDataSource) probe(ctx context.Context, configPath, cell string) (reachResult, error) {
	cfg, _, err := loadInfraConfig(configPath)
	if err != nil {
		return reachResult{}, err
	}
	var cp, tokenFile string
	if d := cfg.Defaults; d != nil {
		if d.ControlPlane != nil {
			cp = *d.ControlPlane
		}
		if d.FleetTokenFile != nil {
			tokenFile = *d.FleetTokenFile
		}
	}
	// A per-cell control-plane override wins over the default.
	if e, ok := cfg.Cells[cell]; ok && e.ControlPlane != nil {
		cp = *e.ControlPlane
	}
	if cp == "" {
		return reachResult{}, fmt.Errorf("cell has no control plane — nothing to probe")
	}
	fc, err := fleet.NewClient(cp, tokenFile)
	if err != nil {
		return reachResult{}, err
	}
	pr, err := fc.Probe(ctx, cell)
	if err != nil {
		return reachResult{}, err
	}
	return reachResult{ok: pr.OK, reason: pr.Reason, version: pr.CellVersion, status: pr.CellStatus}, nil
}

// probeHealth shells out to `witself-infra cell-health -cell X` and
// decodes its JSON report. The subprocess owns the heavy Pulumi
// stack-select + cluster reach; the dashboard just orchestrates it.
// A non-zero exit with un-parseable stdout surfaces as an error the
// Health lines render as "probe failed".
func (liveDataSource) probeHealth(ctx context.Context, configPath, cell string) (cellHealthReport, error) {
	cmd, err := spawnCommand(ctx, []string{"cell-health", "-cell", cell, "-config", configPath})
	if err != nil {
		return cellHealthReport{}, err
	}
	out, err := cmd.Output()
	if err != nil {
		detail := ""
		if ee, ok := err.(*exec.ExitError); ok {
			detail = strings.TrimSpace(string(ee.Stderr))
		}
		if detail != "" {
			return cellHealthReport{}, fmt.Errorf("%v: %s", err, oneLine(detail))
		}
		return cellHealthReport{}, err
	}
	var rep cellHealthReport
	if err := json.Unmarshal(out, &rep); err != nil {
		return cellHealthReport{}, fmt.Errorf("decode cell-health report: %w", err)
	}
	return rep, nil
}

type loadedMsg struct {
	states        []cellState
	controlPlanes map[string]controlPlaneInfo
	configFinger  string
	err           error
}

// probeResultMsg carries one async reachability probe back to Update.
type probeResultMsg struct {
	cell string
	res  reachResult
	err  error
}

// healthResultMsg carries one async cell-health subprocess report back.
type healthResultMsg struct {
	cell   string
	report cellHealthReport
	err    error
}

// healthAnimTickMsg drives the "checking…" animation on the Health tab.
type healthAnimTickMsg struct{ gen int }

// bgProbeTickMsg drives the background probe sweep that keeps live
// cells' health warm regardless of which tab is showing.
type bgProbeTickMsg struct{}
type refreshTickMsg struct{}

// spinnerTickMsg carries the op generation that scheduled it. On
// wake, the handler drops any tick whose gen doesn't match the current
// op's — this closes the "ticker adopts the next op" race where a
// stale tick from a just-finished op could fire against a freshly
// launched op inside the 100ms tail window and fork a second loop.
type spinnerTickMsg struct{ gen int }

const autoRefreshInterval = 60 * time.Second

// spinnerInterval controls how fast the running-op indicator throbs on
// the cell row and ops-pane title. 100ms is the standard bubbles/spin
// rate — fast enough to read as "alive," slow enough to not flicker.
const spinnerInterval = 100 * time.Millisecond

// spinnerFrames is the classic bubbletea braille spinner. Each frame
// is one cell wide, so swapping mid-column doesn't shift the layout.
var spinnerFrames = []string{"⣾", "⣽", "⣻", "⢿", "⡿", "⣟", "⣯", "⣷"}

func spinnerCmdGen(gen int) tea.Cmd {
	return tea.Tick(spinnerInterval, func(time.Time) tea.Msg { return spinnerTickMsg{gen: gen} })
}

// opKindStyle picks the accent used for both the cell-row spinner and
// the ops-pane title spinner — green for preview (read-only), yellow
// for up (mutating), red for destroy (irreversible). Kept as a single
// helper so the two surfaces can't drift out of sync.
func opKindStyle(kind opKind) lipgloss.Style {
	switch kind {
	case opPreview:
		return styOK
	case opUp:
		return styWarn
	case opDestroy:
		return styErr
	}
	return styTitle
}

// opSpinnerLabel returns the fixed-width spinner cell for the running
// op — statusCellW cells wide so cell names still line up.
func opSpinnerLabel(kind opKind, frame int) string {
	ch := spinnerFrames[frame%len(spinnerFrames)]
	label := ch + " " + kind.verb()
	if pad := statusCellW - lipgloss.Width(label); pad > 0 {
		label += strings.Repeat(" ", pad)
	}
	return opKindStyle(kind).Render(label)
}

// opSpinnerRune returns just the current braille frame, colored by
// kind — for the ops-pane title, where we don't want the verb (it's
// already right after) but still want the same accent as the cell row.
func opSpinnerRune(kind opKind, frame int) string {
	return opKindStyle(kind).Render(spinnerFrames[frame%len(spinnerFrames)])
}

func (m dashboardModel) Init() tea.Cmd {
	return tea.Batch(m.loadCmd(), tickCmd(), bgProbeCmd())
}

func tickCmd() tea.Cmd {
	return tea.Tick(autoRefreshInterval, func(time.Time) tea.Msg { return refreshTickMsg{} })
}

func bgProbeCmd() tea.Cmd {
	return tea.Tick(bgProbeInterval, func(time.Time) tea.Msg { return bgProbeTickMsg{} })
}

func (m dashboardModel) loadCmd() tea.Cmd {
	ctx, cli, path := m.ctx, m.cli, m.configPath
	return func() tea.Msg {
		res, err := cli.load(ctx, path)
		return loadedMsg{states: res.states, controlPlanes: res.controlPlanes, configFinger: res.configFinger, err: err}
	}
}

func (m dashboardModel) probeCmd(cell string) tea.Cmd {
	ctx, cli, path := m.ctx, m.cli, m.configPath
	return func() tea.Msg {
		res, err := cli.probe(ctx, path, cell)
		return probeResultMsg{cell: cell, res: res, err: err}
	}
}

func (m dashboardModel) healthCmd(cell string) tea.Cmd {
	ctx, cli, path := m.ctx, m.cli, m.configPath
	return func() tea.Msg {
		rep, err := cli.probeHealth(ctx, path, cell)
		return healthResultMsg{cell: cell, report: rep, err: err}
	}
}

// maybeHealthProbe kicks the probes the Health tab needs for the
// selected cell — the cheap reachability probe (reachTTL) and the
// heavier subsystem report (healthReportTTL) — each only when its
// cached result is missing or stale and nothing is already in flight.
// Called after any key that could change the tab or the selected cell,
// so probing is on-demand: no background cost when Health isn't up.
func (m dashboardModel) maybeHealthProbe() (dashboardModel, tea.Cmd) {
	if m.activeTab != tabHealth {
		return m, nil
	}
	stp := m.selectedState()
	if stp == nil {
		return m, nil
	}
	var cmds []tea.Cmd
	if r := m.reach[stp.name]; !r.inflight && (r.probed.IsZero() || m.now().Sub(r.probed) >= reachTTL) {
		if m.reach == nil {
			m.reach = map[string]cellReach{}
		}
		r.inflight = true
		m.reach[stp.name] = r
		cmds = append(cmds, m.probeCmd(stp.name))
	}
	if h := m.health[stp.name]; !h.inflight && (h.probed.IsZero() || m.now().Sub(h.probed) >= healthReportTTL) {
		if m.health == nil {
			m.health = map[string]cellHealthState{}
		}
		h.inflight = true
		m.health[stp.name] = h
		cmds = append(cmds, m.healthCmd(stp.name))
	}
	if len(cmds) == 0 {
		return m, nil
	}
	// Something is now in flight — start (or restart) the checking-state
	// animation so the "checking…" lines pulse until results land.
	m.healthAnimGen++
	cmds = append(cmds, healthAnimCmd(m.healthAnimGen))
	return m, tea.Batch(cmds...)
}

func healthAnimCmd(gen int) tea.Cmd {
	return tea.Tick(spinnerInterval, func(time.Time) tea.Msg { return healthAnimTickMsg{gen: gen} })
}

// backgroundSweep refreshes live cells' probes off-tab so their data
// is already warm when the operator opens the Health tab. It re-probes
// reachability (cheap) for every registered cell whose reading is
// stale, and refreshes at most ONE cell's health report per sweep —
// the stalest — so a fleet of cells never spawns a burst of Pulumi
// stack selects at once. The sweep re-arms itself regardless of the
// active tab. Only registered cells (st.fleet != nil) are live enough
// to be worth probing.
func (m dashboardModel) backgroundSweep() (tea.Model, tea.Cmd) {
	if m.reach == nil {
		m.reach = map[string]cellReach{}
	}
	if m.health == nil {
		m.health = map[string]cellHealthState{}
	}
	var cmds []tea.Cmd

	// One health probe may already be running (background or on-demand);
	// don't start a second — serialize the expensive path.
	healthBusy := false
	for _, h := range m.health {
		if h.inflight {
			healthBusy = true
			break
		}
	}

	staleHealth := "" // the stalest live cell whose report needs a refresh
	var stalest time.Time
	for _, st := range m.states {
		if st.fleet == nil {
			continue // not registered — nothing live to probe
		}
		// Reachability: cheap, refresh every stale cell.
		if r := m.reach[st.name]; !r.inflight && (r.probed.IsZero() || m.now().Sub(r.probed) >= reachTTL) {
			r.inflight = true
			m.reach[st.name] = r
			cmds = append(cmds, m.probeCmd(st.name))
		}
		// Health: pick the single stalest candidate this sweep.
		if !healthBusy {
			if h := m.health[st.name]; !h.inflight && (h.probed.IsZero() || m.now().Sub(h.probed) >= bgHealthTTL) {
				if staleHealth == "" || h.probed.Before(stalest) {
					staleHealth, stalest = st.name, h.probed
				}
			}
		}
	}
	if staleHealth != "" {
		h := m.health[staleHealth]
		h.inflight = true
		m.health[staleHealth] = h
		cmds = append(cmds, m.healthCmd(staleHealth))
	}

	cmds = append(cmds, bgProbeCmd()) // keep the sweep going
	return m, tea.Batch(cmds...)
}

// healthAnimating reports whether the selected cell has a probe in
// flight while the Health tab is up — the condition that keeps the
// checking-state animation ticking.
func (m dashboardModel) healthAnimating() bool {
	if m.activeTab != tabHealth {
		return false
	}
	stp := m.selectedState()
	if stp == nil {
		return false
	}
	return m.reach[stp.name].inflight || m.health[stp.name].inflight
}

// spinGlyph is the current animation frame for the checking state.
func (m dashboardModel) spinGlyph() string {
	return spinnerFrames[m.healthFrame%len(spinnerFrames)]
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
		m.lastLoad = m.now()
		// Rebind plan state to the freshly-read config: if the file
		// changed since a preview armed a cell, its fingerprint no longer
		// matches and planArmed drops it — so an edit that could retarget
		// a cell forces a re-preview.
		m.configFinger = msg.configFinger
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
		// Advance the throb frame; keep ticking only while THIS op's
		// generation is still current. Guards against a stale tick from
		// a just-finished op adopting a freshly launched one during the
		// 100ms tail window and forking a duplicate loop.
		if m.op == nil || msg.gen != m.opGen {
			return m, nil
		}
		m.spinnerFrame++
		return m, spinnerCmdGen(m.opGen)
	case probeResultMsg:
		if m.reach == nil {
			m.reach = map[string]cellReach{}
		}
		m.reach[msg.cell] = cellReach{probed: m.now(), res: msg.res, err: msg.err}
		return m, nil
	case healthResultMsg:
		if m.health == nil {
			m.health = map[string]cellHealthState{}
		}
		m.health[msg.cell] = cellHealthState{probed: m.now(), report: msg.report, err: msg.err}
		return m, nil
	case bgProbeTickMsg:
		return m.backgroundSweep()
	case healthAnimTickMsg:
		// Advance the checking-state frame while a probe is still in
		// flight; the loop self-terminates when everything has landed.
		if msg.gen != m.healthAnimGen || !m.healthAnimating() {
			return m, nil
		}
		m.healthFrame++
		return m, healthAnimCmd(m.healthAnimGen)
	case opLineMsg:
		return m, nil // re-render tick; the ring buffer already holds the line
	case opDoneMsg:
		if m.op != nil {
			if msg.err == nil && m.op.kind == opPreview {
				if m.previewSeen == nil {
					m.previewSeen = map[string]planEntry{}
				}
				// Stamp the plan with the config fingerprint it was run
				// against — planArmed refuses to fire `u` if the config
				// later changes or a different -config is loaded.
				m.previewSeen[msg.cell] = planEntry{At: m.now(), ConfigFinger: m.configFinger}
				m.status = "✓ preview complete on " + msg.cell + " — press u to apply"
			} else if msg.err != nil {
				m.status = "✗ " + m.op.kind.verb() + " on " + msg.cell + " FAILED: " + oneLine(msg.err.Error())
				// Failed preview must NOT arm up; a failed up leaves a
				// partial diff so the last previewed plan is stale too.
				delete(m.previewSeen, msg.cell)
				// Auth-error nudge: if the op's tail contains a
				// recognizable "your credentials are expired" pattern,
				// tell the operator to press `a` — the alternative is
				// a manual `aws sso login` in another window and a
				// blind guess about what went wrong.
				if looksLikeAuthFailure(m.op.snapshot(20)) {
					m.status = "✗ " + m.op.kind.verb() + " on " + msg.cell + " failed on auth — press `a` to log in and try again"
				}
			} else {
				// Explicit outcome per verb so the operator sees the
				// state change in words as well as in the marker refresh
				// (loadCmd fires below and swaps ◌ absent → ● live).
				switch m.op.kind {
				case opUp:
					m.status = "✓ up complete on " + msg.cell + " — cell is now live"
				case opDestroy:
					m.status = "✓ destroy complete on " + msg.cell + " — cell decommissioned"
				default:
					m.status = "✓ " + m.op.kind.verb() + " complete on " + msg.cell
				}
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
			// Every branch above may have mutated the plan map (armed,
			// invalidated, consumed) — persist so a restart remembers.
			savePlanState(m.planPath, m.previewSeen, m.now())
		}
		return m, m.loadCmd()
	case authCompletedMsg:
		if msg.err != nil {
			m.status = "✗ " + msg.desc + " failed: " + oneLine(msg.err.Error())
			return m, nil
		}
		m.status = "✓ " + msg.desc + " — refreshing"
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
	case "tab", "shift+tab":
		// Toggle which pane the arrow keys drive. j/k keep moving the
		// cell cursor either way, so the active tab sticks as you scan
		// cells with the context pane focused.
		if m.focus == focusCells {
			m.focus = focusContext
		} else {
			m.focus = focusCells
		}
	case "esc":
		// esc backs focus out of the context pane to the cells list.
		if m.focus == focusContext {
			m.focus = focusCells
		}
	case "left", "h":
		if m.focus == focusContext && int(m.activeTab) > 0 {
			m.activeTab--
		}
	case "right", "l":
		if m.focus == focusContext && int(m.activeTab) < len(contextTabs)-1 {
			m.activeTab++
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
	// Any key that reaches here may have changed the tab or the selected
	// cell — kick a reachability probe if the Health tab now needs one.
	m, cmd := m.maybeHealthProbe()
	return m, cmd
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
	if kind == opUp && !m.planArmed(cell) {
		if age, had := m.planAge(cell); had && age < previewTTL {
			// Within the TTL but not armed → the config fingerprint moved.
			m.status = "the config for " + cell + " changed since its preview — run `p` again"
		} else if had {
			m.status = "the preview for " + cell + " has expired (older than " + previewTTL.String() + ") — run `p` again"
		} else {
			m.status = "run `p` (preview) first — up won't fire until you've seen the diff for " + cell
		}
		return m, nil
	}
	if kind == opPreview {
		return m.launchOp(kind, cell)
	}
	m.pending = startConfirm(kind, cell, m.planArmed(cell))
	return m, nil
}

func (m dashboardModel) handleConfirmKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	s := msg.String()
	// esc / ctrl+c / q always dismiss — operators habitually press q
	// to quit a dialog, and none of them collide with the destroy
	// confirmation word ("yes" has no q).
	if s == "esc" || s == "ctrl+c" || s == "q" {
		m.pending = nil
		m.status = "confirmation dismissed"
		return m, nil
	}
	// Fire: `y` clears the up-confirm; destroy needs the typed word
	// plus enter (the extra beat is deliberate for the irreversible
	// verb). A bare first `y` on destroy falls through to the typing
	// branch below — canConfirm is false until the word is complete.
	if (s == "y" || s == "enter") && m.pending.canConfirm() {
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
		if len(m.pending.typed) > 0 && !strings.HasPrefix(destroyConfirmWord, m.pending.typed) {
			m.pending.err = "type `" + destroyConfirmWord + "` to confirm — esc to cancel"
		} else {
			m.pending.err = ""
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
	m.opGen++ // fence any stale spinnerTickMsg from a prior op
	if op.logPath != "" {
		m.status = kind.verb() + " running on " + cell + " · logs " + op.logPath
	} else {
		m.status = kind.verb() + " running on " + cell
	}
	// Kick off the throb loop tagged with this op's generation. Ticks
	// from previous ops are no-ops (their gen won't match m.opGen).
	return m, spinnerCmdGen(m.opGen)
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
		"reauthentication failed",
		"gcloud auth", // covers `gcloud auth login`, `application-default login`
		"application default credentials",
		"application-default", // gcloud's dotted CLI path in error banners
		"gcp adc",             // our own whoami/gitops-wait wording
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
	// Auth is available on any cell while idle — re-running a login is
	// harmless, and credentials can go stale between refreshes (ADC's
	// reauth window) without the dashboard having noticed yet.
	authRelevant := idle && haveCell
	previewOK := haveCell && m.planArmed(stp.name)
	// Ops scroll is available whenever there's something in the ops
	// pane — during a running op or after one completed. The `end`
	// hint is redundant when already at tail (opsScroll == 0), but
	// harmless — hint density matters less than discoverability here.
	scrollable := m.opsSource() != nil
	// ←/→ only do anything once the context pane holds focus; dim them
	// until then so the footer mirrors what the keys actually do.
	parts := []part{
		{"j/k", "select", true},
		{"tab", "pane", true},
		{"←/→", "tab", m.focus == focusContext},
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

// ctxPut appends one "  label  value" row to a context slice, or
// returns it unchanged for an empty value. The right-hand pane uses
// this everywhere so the label column stays aligned.
func ctxPut(lines []string, w int, k, v string) []string {
	if v == "" {
		return lines
	}
	label := styDim.Render(fmt.Sprintf("  %-14s ", k))
	return append(lines, fitLine(label+v, w))
}

// renderContext builds the right-hand pane content based on what the
// cursor is on. A header row shows fleet metadata (no tabs — that's a
// control-plane view, not a cell). A cell row shows a tab bar plus the
// active tab's body. Empty inventory gets a hint.
func (m dashboardModel) renderContext(w int) []string {
	if len(m.states) == 0 {
		return []string{styDim.Render("no cells configured — `witself-infra config add-cell …`")}
	}
	r := m.currentRow()
	switch r.kind {
	case rowHeader:
		return m.renderCPContext(r, w)
	case rowCell:
		st := m.states[r.cellIdx]
		lines := []string{m.renderTabBar(w), ""}
		switch m.activeTab {
		case tabOverview:
			lines = append(lines, m.renderOverviewTab(st, w)...)
		case tabKubernetes:
			lines = append(lines, styDim.Render("  no Kubernetes details yet"))
		case tabDatabase:
			lines = append(lines, styDim.Render("  no database details yet"))
		case tabHealth:
			lines = append(lines, m.renderHealthTab(st, w)...)
		}
		return lines
	}
	return nil
}

// renderTabBar draws the tab strip. The active tab is bold, and cyan
// when the context pane holds focus (so ←/→ read as live); inactive
// tabs are dim. A trailing hint tells the operator how to drive it.
func (m dashboardModel) renderTabBar(w int) string {
	segs := make([]string, 0, len(contextTabs))
	for _, t := range contextTabs {
		switch {
		case t.tab == m.activeTab && m.focus == focusContext:
			segs = append(segs, styPlan.Bold(true).Underline(true).Render(t.name))
		case t.tab == m.activeTab:
			segs = append(segs, styTitle.Underline(true).Render(t.name))
		default:
			segs = append(segs, styDim.Render(t.name))
		}
	}
	bar := " " + strings.Join(segs, "  ")
	if m.focus != focusContext {
		bar += styDim.Render("   (tab to focus)")
	} else {
		bar += styDim.Render("   ←/→ switch")
	}
	return fitLine(bar, w)
}

// renderCPContext is the control-plane header view: URL, token file,
// reachability, cell counts, and fleet account totals.
func (m dashboardModel) renderCPContext(r row, w int) []string {
	info, hasInfo := m.controlPlanes[r.cp]
	if !hasInfo {
		info = controlPlaneInfo{url: r.cp}
	}
	var lines []string
	lines = ctxPut(lines, w, "control plane", groupLabel(r.cp))
	if r.cp != "" {
		lines = ctxPut(lines, w, "url", r.cp)
		lines = ctxPut(lines, w, "token file", info.tokenFile)
		reach := styOK.Render("✓ reachable")
		if !info.reachable {
			reach = styErr.Render("✗ unreachable")
		}
		lines = append(lines, fitLine("  "+reach, w))
		if !info.reachable && info.err != nil {
			lines = append(lines, fitLine(styWarn.Render("  · "+oneLine(info.err.Error())), w))
		}
	} else {
		lines = append(lines, fitLine(styDim.Render("  (no control plane — plain self-hosted deploys)"), w))
	}
	lines = append(lines, "", styTitle.Render("  cells"))
	lines = ctxPut(lines, w, "configured", fmt.Sprintf("%d", info.configured))
	if r.cp != "" {
		lines = ctxPut(lines, w, "registered", fmt.Sprintf("%d", info.registered))
		// Orphans: registered but not configured locally. Not a bug per
		// se, but worth calling out — the operator can bring them into
		// inventory or purge them from the fleet.
		if orphans := info.registered - info.configured; orphans > 0 {
			lines = ctxPut(lines, w, "orphans", fmt.Sprintf("%d (registered but not in local infra.yaml)", orphans))
		}
	}
	// Accounts across all cells this CP manages: live customers running
	// today, plus anything in the R2 archive awaiting placement. Blocked
	// accounts are the ones no eligible cell can accept — red, they need
	// operator attention.
	if r.cp != "" {
		lines = append(lines, "", styTitle.Render("  accounts"))
		if !info.hasAccounts {
			lines = ctxPut(lines, w, "status", styDim.Render("(placement status unavailable)"))
		} else {
			// A stacked load bar: live (green) fills first, then archived
			// (yellow) awaiting placement, then blocked (red). At a glance
			// the operator sees how full the fleet is and how much of that
			// is stuck.
			total := info.liveAccts + info.archived + info.blocked
			bar := stackedBar(24,
				barSeg{info.liveAccts, styOK},
				barSeg{info.archived, styWarn},
				barSeg{info.blocked, styErr})
			lines = append(lines, fitLine("  "+bar+styDim.Render(fmt.Sprintf("  %d total", total)), w))
			lines = ctxPut(lines, w, "live", styOK.Render(fmt.Sprintf("%d", info.liveAccts)))
			lines = ctxPut(lines, w, "archived", styWarn.Render(fmt.Sprintf("%d", info.archived))+styDim.Render(" awaiting placement"))
			if info.blocked > 0 {
				label := styDim.Render(fmt.Sprintf("  %-14s ", "blocked"))
				lines = append(lines, fitLine(label+styErr.Render(fmt.Sprintf("%d no eligible cell", info.blocked)), w))
			}
		}
	}
	return lines
}

// barSeg is one colored segment of a stacked bar.
type barSeg struct {
	n     int
	style lipgloss.Style
}

// stackedBar draws proportional colored segments filling `width` cells,
// remainder dim. An all-zero total renders empty. The last non-empty
// segment absorbs rounding so the bar always sums to exactly width.
func stackedBar(width int, segs ...barSeg) string {
	total := 0
	for _, s := range segs {
		total += s.n
	}
	if total <= 0 || width <= 0 {
		return styDim.Render(strings.Repeat("░", max(width, 0)))
	}
	var b strings.Builder
	used := 0
	lastNonEmpty := -1
	for i, s := range segs {
		if s.n > 0 {
			lastNonEmpty = i
		}
	}
	for i, s := range segs {
		if s.n == 0 {
			continue
		}
		seg := s.n * width / total
		if i == lastNonEmpty {
			seg = width - used // absorb rounding
		}
		if seg < 0 {
			seg = 0
		}
		b.WriteString(s.style.Render(strings.Repeat("█", seg)))
		used += seg
	}
	if used < width {
		b.WriteString(styDim.Render(strings.Repeat("░", width-used)))
	}
	return b.String()
}

// renderOverviewTab is the cell's identity, config, settings, and
// fleet state — the content the right pane showed before it became
// tabbed.
func (m dashboardModel) renderOverviewTab(st cellState, w int) []string {
	e := st.entry
	var lines []string
	lines = ctxPut(lines, w, "cell", st.name)
	if e.Cloud != nil {
		lines = ctxPut(lines, w, "cloud", *e.Cloud)
	}
	if e.Region != nil {
		lines = ctxPut(lines, w, "region", *e.Region)
	}
	lines = ctxPut(lines, w, "control plane", groupLabel(st.controlPlane))
	lines = ctxPut(lines, w, "status", st.status())
	// Plan state: what the ◆ in the cells pane means and what to press
	// next either way. Age shown so the operator can judge how stale the
	// approved diff is getting. A plan can fail to arm two ways — the
	// TTL lapsed, or the config changed under it — and the message says
	// which so "press p again" doesn't feel arbitrary.
	if m.planArmed(st.name) {
		age, _ := m.planAge(st.name)
		lines = ctxPut(lines, w, "plan", styPlan.Render(fmt.Sprintf("◆ previewed %s ago — press u to apply", age.Round(time.Minute))))
	} else if age, ok := m.planAge(st.name); ok {
		if age < previewTTL {
			lines = ctxPut(lines, w, "plan", styDim.Render("config changed since preview — press p again"))
		} else {
			lines = ctxPut(lines, w, "plan", styDim.Render("preview expired — press p again"))
		}
	} else {
		lines = ctxPut(lines, w, "plan", styDim.Render("none — press p to preview"))
	}
	if st.fleet != nil {
		lines = ctxPut(lines, w, "endpoint", st.fleet.Endpoint)
		lines = ctxPut(lines, w, "channel", st.fleet.Channel)
	}
	if len(st.settings) > 0 {
		lines = append(lines, "", styTitle.Render("  settings"))
		for _, srow := range st.settings {
			val := srow.value
			if !srow.fromEntry {
				val = styDim.Render(val)
			}
			label := styDim.Render(fmt.Sprintf("  %-14s ", srow.key))
			lines = append(lines, fitLine(label+val, w))
		}
	}
	lines = append(lines, "", styTitle.Render("  identity"))
	if st.err != nil {
		lines = append(lines, fitLine(styErr.Render("  "+oneLine(st.err.Error())), w))
	} else if st.identity.Cloud != "" {
		id := st.identity
		lines = ctxPut(lines, w, "profile", id.Profile)
		lines = ctxPut(lines, w, "account", id.Account)
		lines = ctxPut(lines, w, "tenant", id.Tenant)
		lines = ctxPut(lines, w, "actor", id.Actor)
		ok := styOK.Render("✓ matches config pin")
		if !id.OK {
			ok = styErr.Render("✗ pin mismatch")
		}
		lines = append(lines, fitLine("  "+ok, w))
		for _, n := range id.Notes {
			lines = append(lines, fitLine(styWarn.Render("  · "+oneLine(n)), w))
		}
	}
	return lines
}

// healthRow pairs a label with its resolved subsystem reading for the
// Health board.
type healthRow struct {
	label string
	sh    subsystemHealth
}

// renderHealthTab paints the Health board: a status-pip summary line
// (one colored dot per subsystem) with a rolled-up verdict, then a
// connection tree of the cell's subsystems, each with its indicator,
// an optional fill gauge (nodes Ready, apps Healthy), and detail.
func (m dashboardModel) renderHealthTab(st cellState, w int) []string {
	credLvl, credMsg := cellCredentialHealth(st)
	fleetLvl, fleetMsg := cellFleetHealth(st)
	reachLvl, reachMsg := m.cellReachHealth(st)
	rows := []healthRow{
		{"credentials", subsystemHealth{Level: credLvl, Detail: credMsg}},
		{"fleet", subsystemHealth{Level: fleetLvl, Detail: fleetMsg}},
		{"reachability", subsystemHealth{Level: reachLvl, Detail: reachMsg}},
		{"kubernetes", m.cellSubsystem(st, func(r cellHealthReport) subsystemHealth { return r.Kubernetes })},
		{"database", m.cellSubsystem(st, func(r cellHealthReport) subsystemHealth { return r.Database })},
		{"workloads", m.cellSubsystem(st, func(r cellHealthReport) subsystemHealth { return r.Argo })},
	}

	levels := make([]healthLevel, len(rows))
	for i, r := range rows {
		levels[i] = r.sh.Level
	}
	overall := rollupLevel(levels...)

	var out []string
	// Summary: the cell name, a row of one pip per subsystem, and the
	// rolled-up verdict — the whole cell's health at a glance.
	out = append(out, fitLine("  "+healthPips(levels)+"  "+styTitle.Render(st.name), w))
	out = append(out, fitLine("  "+styDim.Render("overall")+"  "+overall.style().Render("● "+overallWord(overall)), w))
	// Data freshness: background sweeps keep these warm, so say how old
	// each source is. reach and the cluster report refresh on their own
	// clocks; the credential/fleet lines ride the inventory load.
	fresh := fmt.Sprintf("reach %s · cluster %s · fleet %s ago",
		humanAge(m.now(), m.reach[st.name].probed),
		humanAge(m.now(), m.health[st.name].probed),
		humanAge(m.now(), m.lastLoad))
	out = append(out, fitLine("  "+styDim.Render(fresh), w))
	out = append(out, "")

	// Connection tree: the cell branching to each subsystem.
	for i, r := range rows {
		branch := "├─"
		if i == len(rows)-1 {
			branch = "└─"
		}
		seg := "  " + styDim.Render(branch) + " " + r.sh.Level.dot() + " " + fmt.Sprintf("%-13s", r.label)
		if r.sh.Total > 0 {
			seg += gaugeBar(r.sh.Have, r.sh.Total, 6, r.sh.Level.style()) + "  "
		}
		seg += r.sh.Level.style().Render(r.sh.Detail)
		out = append(out, fitLine(seg, w))
	}
	return out
}

// cellSubsystem reads one subsystem line (with its gauge counts) from
// the cached cell-health report: unknown/"checking…" before or during
// a probe, red if the subprocess itself failed, else the report's own
// reading.
func (m dashboardModel) cellSubsystem(st cellState, pick func(cellHealthReport) subsystemHealth) subsystemHealth {
	h := m.health[st.name]
	switch {
	case h.inflight:
		return subsystemHealth{Level: healthUnknown, Detail: m.spinGlyph() + " checking…"}
	case h.probed.IsZero():
		return subsystemHealth{Level: healthUnknown, Detail: "— not probed yet"}
	case h.err != nil:
		return subsystemHealth{Level: healthBad, Detail: "✗ probe failed: " + oneLine(h.err.Error())}
	default:
		return pick(h.report)
	}
}

// rollupLevel returns the worst non-unknown level — the whole-cell
// verdict. All-unknown (nothing probed) stays unknown.
func rollupLevel(levels ...healthLevel) healthLevel {
	worst := healthUnknown
	for _, l := range levels {
		if l != healthUnknown && l > worst {
			worst = l
		}
	}
	return worst
}

func overallWord(l healthLevel) string {
	switch l {
	case healthGood:
		return "all good"
	case healthWarn:
		return "settling"
	case healthDegraded:
		return "degraded"
	case healthBad:
		return "needs attention"
	default:
		return "unprobed"
	}
}

// humanAge is a compact "how long ago" for a probe timestamp: "8s",
// "3m", "1h". A zero time (never probed) reads "never".
func humanAge(now, then time.Time) string {
	if then.IsZero() {
		return "never"
	}
	d := now.Sub(then)
	switch {
	case d < 0:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
}

// healthPips renders one colored dot per subsystem — a compact status
// strip you can read in a glance before diving into the tree.
func healthPips(levels []healthLevel) string {
	var b strings.Builder
	for _, l := range levels {
		b.WriteString(l.dot())
	}
	return b.String()
}

// gaugeBar draws a fill meter: `▰▰▰▱▱▱ 3/6`, the filled part in the
// level's color and the remainder dim. Integer-rounded so a single
// down node visibly drops the bar.
func gaugeBar(have, total, width int, style lipgloss.Style) string {
	if total <= 0 || width <= 0 {
		return ""
	}
	filled := (have*width + total/2) / total
	if filled < 0 {
		filled = 0
	}
	if filled > width {
		filled = width
	}
	return style.Render(strings.Repeat("▰", filled)) +
		styDim.Render(strings.Repeat("▱", width-filled)) +
		" " + style.Render(fmt.Sprintf("%d/%d", have, total))
}

// cellReachHealth turns the cached reachability probe into a health
// line. No probe yet reads unknown; an in-flight probe reads
// "checking…"; a transport error (or unreachable-cell reason) reads
// red/orange with the diagnostic; a clean probe reads green with the
// witself-server version the Worker saw.
func (m dashboardModel) cellReachHealth(st cellState) (healthLevel, string) {
	r := m.reach[st.name] // zero value when never probed
	switch {
	case r.inflight:
		return healthUnknown, m.spinGlyph() + " checking…"
	case r.probed.IsZero():
		return healthUnknown, "— not probed yet"
	case r.err != nil:
		return healthBad, "✗ " + oneLine(r.err.Error())
	case r.res.ok:
		v := "reachable"
		if r.res.version != "" {
			v += " · witself-server " + r.res.version
		}
		return healthGood, "✓ " + v
	default:
		// The control plane reached the fleet but the cell isn't serving
		// yet — DNS/TLS/HTTP not ready. Degraded, not dead.
		reason := r.res.reason
		if reason == "" {
			reason = fmt.Sprintf("HTTP %d", r.res.status)
		}
		return healthDegraded, oneLine(reason)
	}
}

// cellCredentialHealth reads the cloud identity check that runs every
// refresh: a load error means the CLI couldn't authenticate, a pin
// mismatch means we're pointed at the wrong account, otherwise good.
func cellCredentialHealth(st cellState) (healthLevel, string) {
	switch {
	case st.err != nil:
		return healthBad, "✗ " + oneLine(st.err.Error())
	case st.identity.Cloud == "":
		return healthUnknown, "— no identity yet"
	case !st.identity.OK:
		return healthBad, "✗ account pin mismatch"
	default:
		return healthGood, "✓ valid"
	}
}

// cellFleetHealth reads the control-plane registration: registered and
// accepting is good, registered-but-draining is transitional, and an
// absent fleet record is a warning (not necessarily an error — the
// cell may simply not be provisioned yet).
func cellFleetHealth(st cellState) (healthLevel, string) {
	if st.fleet == nil {
		return healthWarn, "not registered"
	}
	if st.fleet.Accepting != nil && !*st.fleet.Accepting {
		return healthWarn, "draining"
	}
	return healthGood, "registered · accepting"
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
			// Plan column: ◆ when a successful preview has armed `u` for
			// this cell, blank otherwise. Two cells wide on every row so
			// names stay aligned whether or not the mark is present.
			plan := "  "
			if m.planArmed(st.name) {
				plan = styPlan.Render("◆") + " "
			}
			text := fmt.Sprintf("%s %s%s", marker, plan, st.name)
			// Cells share the headers' two-column cursor gutter — the
			// bold header text and per-group counts carry the hierarchy,
			// so a deeper indent bought nothing but truncated names.
			if selected {
				text = "▸ " + text
			} else {
				text = "  " + text
			}
			lines = append(lines, fitLine(text, cellsContentW))
		}
	}
	if len(lines) == 0 {
		lines = append(lines, styDim.Render("no cells configured — `witself-infra config add-cell …`"))
	}
	// No pane title: the group headers already carry names and counts
	// ("witself cloud · self.witwave.ai  6 cells"), so a "cells · 6"
	// banner was pure repetition — the row goes to content instead.
	// The focused pane gets the cyan border; focus governs which pane
	// the arrow keys drive.
	cellsPane := paneBox("", lines, cellsContentW, topH+1, m.focus == focusCells)

	// Context pane — also untitled: its first content row is the tab
	// bar, which names what you're looking at (Overview/Kubernetes/…)
	// the way the group headers name the cells pane. topH+1 keeps its
	// outer height flush with the cells pane beside it.
	ctxLines := m.renderContext(ctxContentW)
	contextPane := paneBox("", ctxLines, ctxContentW, topH+1, m.focus == focusContext)

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
			// Same frame AND color as the cell row — reads as one
			// indicator on two surfaces, not two flickers with mismatched
			// accents.
			state = opSpinnerRune(active.kind, m.spinnerFrame) + " " + state
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
	// Status line style is dispatched by prefix so the OP-completion
	// signal actually pops instead of blending into every other dim
	// message. "✓ " → green (success), "✗ " → red (failure), else the
	// default dim informational tone.
	status := ""
	if m.status != "" {
		switch {
		case strings.HasPrefix(m.status, "✓ "):
			status = "\n" + styOK.Render(" "+m.status)
		case strings.HasPrefix(m.status, "✗ "):
			status = "\n" + styErr.Render(" "+m.status)
		default:
			status = "\n" + styDim.Render(" "+m.status)
		}
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
// same idiom as the witself-admin dashboard. An empty title skips the
// title row entirely, handing that row to content; callers pairing an
// untitled pane beside a titled one pass contentH+1 so the outer
// heights still match.
func paneBox(title string, lines []string, contentW, contentH int, focused bool) string {
	if len(lines) > contentH {
		lines = lines[:contentH]
	}
	for len(lines) < contentH {
		lines = append(lines, "")
	}
	body := strings.Join(lines, "\n")
	if title != "" {
		body = styTitle.Render(fitLine(title, contentW)) + "\n" + body
	}
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
	planPath := planStatePath()
	m := dashboardModel{
		ctx:         context.Background(),
		cli:         liveDataSource{},
		configPath:  configPath,
		now:         time.Now,
		loading:     true,
		status:      "loading inventory…",
		planPath:    planPath,
		previewSeen: loadPlanState(planPath, time.Now()),
		// Seed the fingerprint eagerly so persisted plans validate on the
		// very first render, before the async load's loadedMsg arrives.
		configFinger: configFileFingerprint(configPath),
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
