package main

// Control-plane tabs (issue #37). A header row's context pane gets its
// own tab set — Overview (the existing CP summary) and Settings: every
// runtime-configurable knob the control plane exposes today, as one
// editable form in three sections:
//
//   placement runner — the cron pass (enabled, restore/rebalance + batches)
//   reaper           — pending-account sweep (enabled, ttl minutes)
//   placement        — strategy weighted|pinned (+ pinned cell)
//
// Edits accumulate in a DRAFT copy per CP; nothing writes until the
// operator applies, which pops a diff-confirm modal, POSTs ONLY the
// dirty sections, and then renders the control plane's authoritative
// responses — never the optimistic draft. `r` fires a one-shot runner
// pass with the authoritative runner config.
//
// Key model: while the context pane holds focus on the Settings tab,
// j/k move the FIELD cursor (you're editing a form, not scanning
// cells); everywhere else j/k keep moving the cell cursor as before.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/witwave-ai/witself/infra/pulumi/internal/fleet"
)

// cpTab identifies one tab of a control-plane header's context pane.
type cpTab int

const (
	cpTabOverview cpTab = iota
	cpTabSettings
)

var cpTabNames = []string{"Overview", "Settings"}

// cpSettingsTTL is how stale a loaded config may get before landing on
// the Settings tab re-reads it.
const cpSettingsTTL = 15 * time.Second

// cpConfig bundles every runtime-configurable CP setting the Settings
// tab edits. All fields are comparable, so drafts diff with ==.
type cpConfig struct {
	Runner    fleet.PlacementRunnerConfig
	Reaper    fleet.ReaperConfig
	Placement fleet.PlacementConfig
}

// cpSections flags which sections of a cpConfig an apply should write —
// only the dirty ones go over the wire, so an untouched section can
// never clobber a concurrent change to it.
type cpSections struct {
	Runner    bool
	Reaper    bool
	Placement bool
}

// dirtySections reports which sections differ between the
// authoritative config and the draft.
func dirtySections(cur, next cpConfig) cpSections {
	return cpSections{
		Runner:    cur.Runner != next.Runner,
		Reaper:    cur.Reaper != next.Reaper,
		Placement: cur.Placement != next.Placement,
	}
}

// cpSettingsState is one control plane's settings as the dashboard
// knows them: the authoritative config (last read or last apply
// response), an optional draft holding unapplied edits, and the async
// bookkeeping.
type cpSettingsState struct {
	loaded       time.Time
	inflight     bool
	applying     bool
	runInflight  bool // a `r` runner pass is in flight
	cfg          cpConfig
	preDraftBase cpConfig // cfg snapshot when the draft was forked — preserves mid-apply edits by field
	err          error
	draft        *cpConfig // nil = no edits in progress
	lastRun      string    // one-line summary of the last `r` pass
	// readGen fences stale settings reads: bumps on any state-changing
	// event (apply queued, apply landed, `x` discard, edit start). A
	// cpSettingsMsg carries the gen it was fired against; if it doesn't
	// match at deliver time the response is dropped (its snapshot is
	// older than what happened while it was in flight).
	readGen int
}

// dirty reports whether the draft differs from the authoritative config.
func (s cpSettingsState) dirty() bool {
	return s.draft != nil && *s.draft != s.cfg
}

// cpApplyConfirm is the pending apply-confirm modal: which CP, the
// draft to write, which sections are dirty, and the diff lines. base
// is the cfg snapshot the diff was computed against — the same
// snapshot 'y' must still be looking at; a background read that
// changed cfg while the modal was open invalidates the confirmation
// and the operator is re-prompted.
type cpApplyConfirm struct {
	cp       string
	next     cpConfig
	base     cpConfig
	sections cpSections
	diffs    []string
}

// cpRunConfirm is the pending confirm modal for `r` (one-shot runner
// pass) — a fleet-wide account-moving action, so one keypress is not
// enough. Shows what the pass would do (restore? rebalance? batch
// sizes) and refuses when the runner is server-disabled or when a
// pass is already in flight.
type cpRunConfirm struct {
	cp    string
	cfg   fleet.PlacementRunnerConfig
	lines []string
}

// cpDiscardConfirm is the pending confirm modal for `x` — asymmetric
// with apply otherwise, and losing all edits on one keypress is a
// real footgun.
type cpDiscardConfirm struct {
	cp    string
	count int // how many field changes are about to be lost
}

// cpQuitConfirm is the pending confirm modal for `q` with a dirty
// draft — unapplied edits are in memory only, quitting drops them.
type cpQuitConfirm struct {
	cp    string
	count int
}

// --- field table -----------------------------------------------------

type cpFieldKind int

const (
	cpFieldBool cpFieldKind = iota
	cpFieldInt
	cpFieldEnum // space/enter cycles through options
)

// cpField describes one editable row of the Settings form. section
// groups fields under their header and maps to the wire write.
type cpField struct {
	section string
	name    string
	kind    cpFieldKind
	getB    func(cpConfig) bool
	setB    func(*cpConfig, bool)
	getI    func(cpConfig) int
	setI    func(*cpConfig, int)
	getS    func(cpConfig) string
	setS    func(*cpConfig, string)
	// options returns the enum's choices; for pinned cell it's the
	// CP's registered cells, so it needs the model + CP.
	options func(m *dashboardModel, cp string) []string
}

var cpFields = []cpField{
	{section: "placement runner", name: "enabled", kind: cpFieldBool,
		getB: func(c cpConfig) bool { return c.Runner.Enabled },
		setB: func(c *cpConfig, v bool) { c.Runner.Enabled = v }},
	{section: "placement runner", name: "restore archives", kind: cpFieldBool,
		getB: func(c cpConfig) bool { return c.Runner.RestoreArchives },
		setB: func(c *cpConfig, v bool) { c.Runner.RestoreArchives = v }},
	{section: "placement runner", name: "restore batch", kind: cpFieldInt,
		getI: func(c cpConfig) int { return c.Runner.RestoreBatch },
		setI: func(c *cpConfig, v int) { c.Runner.RestoreBatch = v }},
	{section: "placement runner", name: "restore any-region", kind: cpFieldBool,
		getB: func(c cpConfig) bool { return c.Runner.RestoreAnyRegion },
		setB: func(c *cpConfig, v bool) { c.Runner.RestoreAnyRegion = v }},
	{section: "placement runner", name: "rebalance", kind: cpFieldBool,
		getB: func(c cpConfig) bool { return c.Runner.Rebalance },
		setB: func(c *cpConfig, v bool) { c.Runner.Rebalance = v }},
	{section: "placement runner", name: "rebalance batch", kind: cpFieldInt,
		getI: func(c cpConfig) int { return c.Runner.RebalanceBatch },
		setI: func(c *cpConfig, v int) { c.Runner.RebalanceBatch = v }},

	{section: "reaper", name: "enabled", kind: cpFieldBool,
		getB: func(c cpConfig) bool { return c.Reaper.Enabled },
		setB: func(c *cpConfig, v bool) { c.Reaper.Enabled = v }},
	{section: "reaper", name: "ttl (minutes)", kind: cpFieldInt,
		getI: func(c cpConfig) int { return c.Reaper.TTLMinutes },
		setI: func(c *cpConfig, v int) { c.Reaper.TTLMinutes = v }},

	{section: "placement", name: "strategy", kind: cpFieldEnum,
		getS:    func(c cpConfig) string { return c.Placement.Strategy },
		setS:    func(c *cpConfig, v string) { c.Placement.Strategy = v },
		options: func(*dashboardModel, string) []string { return []string{"weighted", "pinned"} }},
	{section: "placement", name: "pinned cell", kind: cpFieldEnum,
		getS:    func(c cpConfig) string { return c.Placement.PinnedCell },
		setS:    func(c *cpConfig, v string) { c.Placement.PinnedCell = v },
		options: func(m *dashboardModel, cp string) []string { return m.cpGroupCells(cp) }},
}

// value renders one field's value from a config.
func (f cpField) value(c cpConfig) string {
	switch f.kind {
	case cpFieldBool:
		if f.getB(c) {
			return "on"
		}
		return "off"
	case cpFieldInt:
		return strconv.Itoa(f.getI(c))
	default:
		s := f.getS(c)
		if s == "" {
			return "—"
		}
		return s
	}
}

// cpGroupCells lists the registered cells in this CP's group — the
// candidates for a pin. Falls back to every configured cell in the
// group when none are registered yet.
func (m *dashboardModel) cpGroupCells(cp string) []string {
	var registered, all []string
	for _, st := range m.states {
		if st.controlPlane != cp {
			continue
		}
		all = append(all, st.name)
		if st.fleet != nil {
			registered = append(registered, st.name)
		}
	}
	if len(registered) > 0 {
		return registered
	}
	return all
}

// The control plane clamps these server-side (control-plane
// normalizePlacementRunnerConfig): pre-validate so the operator sees a
// clear message rather than a silent server rewrite.
const (
	restoreBatchMin, restoreBatchMax     = 1, 10
	rebalanceBatchMin, rebalanceBatchMax = 1, 5
)

// validateDraft pre-checks the control plane's own rules so the
// operator hears about a bad combination before the confirm modal, not
// as an HTTP 400 (or a silent clamp) after it.
func validateDraft(c cpConfig) string {
	if c.Reaper.Enabled && c.Reaper.TTLMinutes < 1 {
		return "reaper ttl must be ≥ 1 minute when the reaper is enabled"
	}
	if c.Placement.Strategy == "pinned" && c.Placement.PinnedCell == "" {
		return "pinned strategy needs a pinned cell"
	}
	if b := c.Runner.RestoreBatch; b < restoreBatchMin || b > restoreBatchMax {
		return fmt.Sprintf("restore batch must be %d..%d (control plane clamps beyond this)", restoreBatchMin, restoreBatchMax)
	}
	if b := c.Runner.RebalanceBatch; b < rebalanceBatchMin || b > rebalanceBatchMax {
		return fmt.Sprintf("rebalance batch must be %d..%d (control plane clamps beyond this)", rebalanceBatchMin, rebalanceBatchMax)
	}
	return ""
}

// mergeDraftEdits returns the draft edits (draft ⊕ base) — every field
// the operator touched in draft is carried forward onto base. Used
// after apply lands to preserve edits the operator made DURING the
// write (against the response as the new base), instead of silently
// dropping them. A field is "touched" if its draft value differs from
// the CFG the draft was forked from (preDraftBase).
func mergeDraftEdits(preDraftBase, draft, base cpConfig) (cpConfig, bool) {
	out := base
	changed := false
	if draft.Runner != preDraftBase.Runner {
		out.Runner = draft.Runner
		changed = changed || draft.Runner != base.Runner
	}
	if draft.Reaper != preDraftBase.Reaper {
		out.Reaper = draft.Reaper
		changed = changed || draft.Reaper != base.Reaper
	}
	if draft.Placement != preDraftBase.Placement {
		out.Placement = draft.Placement
		changed = changed || draft.Placement != base.Placement
	}
	return out, changed
}

// --- live data source ------------------------------------------------

// fleetClientFor builds a fleet client for a CP URL, resolving the
// token file from the config's defaults — the same resolution the
// load/probe paths use.
func fleetClientFor(configPath, cp string) (*fleet.Client, error) {
	cfg, _, err := loadInfraConfig(configPath)
	if err != nil {
		return nil, err
	}
	tokenFile := ""
	if d := cfg.Defaults; d != nil && d.FleetTokenFile != nil {
		tokenFile = *d.FleetTokenFile
	}
	return fleet.NewClient(cp, tokenFile)
}

// readCPConfig fetches all three settings sections.
func (liveDataSource) readCPConfig(ctx context.Context, configPath, cp string) (cpConfig, error) {
	fc, err := fleetClientFor(configPath, cp)
	if err != nil {
		return cpConfig{}, err
	}
	var out cpConfig
	if out.Runner, err = fc.GetPlacementRunner(ctx); err != nil {
		return cpConfig{}, err
	}
	if out.Reaper, err = fc.GetReaper(ctx); err != nil {
		return cpConfig{}, err
	}
	if out.Placement, err = fc.GetPlacement(ctx); err != nil {
		return cpConfig{}, err
	}
	return out, nil
}

// writeCPConfig POSTs only the dirty sections and returns the merged
// authoritative result (server responses for written sections, the
// passed config for untouched ones).
func (liveDataSource) writeCPConfig(ctx context.Context, configPath, cp string, cfg cpConfig, sections cpSections) (cpConfig, error) {
	fc, err := fleetClientFor(configPath, cp)
	if err != nil {
		return cpConfig{}, err
	}
	out := cfg
	if sections.Runner {
		if out.Runner, err = fc.SetPlacementRunner(ctx, cfg.Runner); err != nil {
			return cpConfig{}, err
		}
	}
	if sections.Reaper {
		if out.Reaper, err = fc.SetReaper(ctx, cfg.Reaper); err != nil {
			return cpConfig{}, err
		}
	}
	if sections.Placement {
		if out.Placement, err = fc.SetPlacement(ctx, cfg.Placement); err != nil {
			return cpConfig{}, err
		}
	}
	return out, nil
}

func (liveDataSource) runPlacementRunner(ctx context.Context, configPath, cp string, cfg fleet.PlacementRunnerConfig) (fleet.PlacementRunnerResult, error) {
	fc, err := fleetClientFor(configPath, cp)
	if err != nil {
		return fleet.PlacementRunnerResult{}, err
	}
	return fc.RunPlacementRunner(ctx, cfg)
}

// --- async plumbing ----------------------------------------------------

// cpSettingsMsg carries a settings read back to Update. gen is the
// readGen the read was fired against — a stale read (gen doesn't match
// the current state's readGen at deliver time) is dropped so it can't
// clobber an intervening apply or a discarded draft. purpose flags
// what kicked the read so the handler can dispatch the follow-up
// (background TTL vs pre-apply diff vs post-apply catch-up).
type cpSettingsMsg struct {
	cp      string
	cfg     cpConfig
	err     error
	gen     int
	purpose cpReadPurpose
}

type cpReadPurpose int

const (
	cpReadBackground cpReadPurpose = iota
	cpReadPreApply
	cpReadPostApply
)

// cpApplyMsg carries a write result back. cfg is authoritative.
type cpApplyMsg struct {
	cp  string
	cfg cpConfig
	err error
}

// cpRunMsg carries a one-shot runner pass result back.
type cpRunMsg struct {
	cp  string
	res fleet.PlacementRunnerResult
	err error
}

func (m dashboardModel) cpSettingsCmd(cp string, gen int, purpose cpReadPurpose) tea.Cmd {
	ctx, cli, path := m.ctx, m.cli, m.configPath
	return func() tea.Msg {
		cfg, err := cli.readCPConfig(ctx, path, cp)
		return cpSettingsMsg{cp: cp, cfg: cfg, err: err, gen: gen, purpose: purpose}
	}
}

func (m dashboardModel) cpApplyCmd(cp string, cfg cpConfig, sections cpSections) tea.Cmd {
	ctx, cli, path := m.ctx, m.cli, m.configPath
	return func() tea.Msg {
		got, err := cli.writeCPConfig(ctx, path, cp, cfg, sections)
		return cpApplyMsg{cp: cp, cfg: got, err: err}
	}
}

func (m dashboardModel) cpRunCmd(cp string, cfg fleet.PlacementRunnerConfig) tea.Cmd {
	ctx, cli, path := m.ctx, m.cli, m.configPath
	return func() tea.Msg {
		res, err := cli.runPlacementRunner(ctx, path, cp, cfg)
		return cpRunMsg{cp: cp, res: res, err: err}
	}
}

// onCPSettings reports whether the Settings tab of a real (non
// self-hosted) control-plane header is the active context view.
func (m dashboardModel) onCPSettings() bool {
	r := m.currentRow()
	return r.kind == rowHeader && r.cp != "" && m.cpActiveTab == cpTabSettings
}

// maybeCPSettingsLoad kicks an async settings read when the Settings
// tab is showing a CP whose config is missing or stale — the analog of
// maybeHealthProbe for cells. Any live modal (apply/run/discard/quit)
// blocks it — the modal's snapshot is what will be acted on.
func (m dashboardModel) maybeCPSettingsLoad() (dashboardModel, tea.Cmd) {
	if !m.onCPSettings() || m.cpApply != nil || m.cpRun != nil || m.cpDiscard != nil || m.cpQuit != nil {
		return m, nil
	}
	cp := m.currentRow().cp
	s := m.cpSettings[cp]
	if s.inflight || s.applying || (!s.loaded.IsZero() && m.now().Sub(s.loaded) < cpSettingsTTL) {
		return m, nil
	}
	if m.cpSettings == nil {
		m.cpSettings = map[string]cpSettingsState{}
	}
	s.inflight = true
	m.cpSettings[cp] = s
	return m, m.cpSettingsCmd(cp, s.readGen, cpReadBackground)
}

// --- key handling ------------------------------------------------------

// handleCPSettingsKey processes keys while the context pane holds
// focus on a CP Settings tab. Returns handled=false for keys that
// should fall through to the normal handler (tab/esc focus moves,
// ←/→ tab switching, quit, …).
func (m dashboardModel) handleCPSettingsKey(msg tea.KeyMsg) (dashboardModel, tea.Cmd, bool) {
	cp := m.currentRow().cp
	s := m.cpSettings[cp]
	key := msg.String()

	// An in-progress int edit captures digits/backspace/enter/esc.
	// ctrl+c is honored — it cancels the edit and lets the normal
	// interrupt/quit path run afterward (mid-edit stay in the form).
	if m.cpEditing {
		switch {
		case key == "esc":
			m.cpEditing, m.cpEditBuf = false, ""
			return m, nil, true
		case key == "ctrl+c":
			// Abandon the edit and let ctrl+c fall through — the operator's
			// escape hatch stays live.
			m.cpEditing, m.cpEditBuf = false, ""
			return m, nil, false
		case key == "enter":
			if v, err := strconv.Atoi(m.cpEditBuf); err == nil && v > 0 {
				d := m.draftFor(cp)
				cpFields[m.cpFieldSel].setI(d, v)
			} else {
				m.status = "value must be a positive number — edit cancelled"
			}
			m.cpEditing, m.cpEditBuf = false, ""
			return m, nil, true
		case key == "backspace":
			if len(m.cpEditBuf) > 0 {
				m.cpEditBuf = m.cpEditBuf[:len(m.cpEditBuf)-1]
			}
			return m, nil, true
		case len(key) == 1 && key[0] >= '0' && key[0] <= '9':
			if len(m.cpEditBuf) < 5 {
				m.cpEditBuf += key
			}
			return m, nil, true
		default:
			return m, nil, true // swallow everything else mid-edit
		}
	}

	// Everything below assumes settings are LOADED — an "unloaded" or
	// "errored" state is read-only. This centralises the guard that
	// enter/space had and extends it to a, x, r.
	editReady := !s.loaded.IsZero() && s.err == nil

	switch key {
	case "j", "down":
		if m.cpFieldSel < len(cpFields)-1 {
			m.cpFieldSel++
		}
		return m, nil, true
	case "k", "up":
		if m.cpFieldSel > 0 {
			m.cpFieldSel--
		}
		return m, nil, true
	case "enter", " ":
		if !editReady {
			m.status = "settings not loaded — nothing to edit"
			return m, nil, true
		}
		// Editing DURING an apply is allowed — the response handler
		// rebases mid-apply edits onto the authoritative response
		// (mergeDraftEdits) so they aren't silently lost.
		f := cpFields[m.cpFieldSel]
		switch f.kind {
		case cpFieldBool:
			d := m.draftFor(cp)
			f.setB(d, !f.getB(*d))
		case cpFieldInt:
			m.cpEditing = true
			m.cpEditBuf = ""
		case cpFieldEnum:
			opts := f.options(&m, cp)
			if len(opts) == 0 {
				m.status = "no choices available for " + f.name
				return m, nil, true
			}
			d := m.draftFor(cp)
			f.setS(d, nextOption(opts, f.getS(*d)))
		}
		s = m.cpSettings[cp]
		s.readGen++ // any edit fences reads in flight
		m.cpSettings[cp] = s
		return m, nil, true
	case "x":
		if s.draft == nil {
			return m, nil, true
		}
		m.cpDiscard = &cpDiscardConfirm{cp: cp, count: len(diffConfigs(s.cfg, *s.draft))}
		return m, nil, true
	case "a":
		if !editReady {
			m.status = "settings not loaded — cannot apply"
			return m, nil, true
		}
		if !s.dirty() {
			m.status = "no unapplied changes"
			return m, nil, true
		}
		if s.applying {
			m.status = "an apply is already in flight"
			return m, nil, true
		}
		if why := validateDraft(*s.draft); why != "" {
			m.status = "✗ " + why
			return m, nil, true
		}
		// Re-read authoritatively before opening the modal — a stale
		// diff would let 'y' silently revert changes another operator
		// made server-side. The modal only opens on the read landing
		// (cpReadPreApply handler).
		s.inflight = true
		s.readGen++
		m.cpSettings[cp] = s
		m.status = "checking control plane for changes before apply…"
		return m, m.cpSettingsCmd(cp, s.readGen, cpReadPreApply), true
	case "r":
		if !editReady {
			m.status = "settings not loaded — cannot run"
			return m, nil, true
		}
		if s.applying {
			m.status = "an apply is in flight — wait for it before running"
			return m, nil, true
		}
		if s.runInflight {
			m.status = "a runner pass is already in flight on " + shortHost(cp)
			return m, nil, true
		}
		if !s.cfg.Runner.Enabled {
			m.status = "runner is disabled — enable and apply, or press again to confirm a one-shot pass"
			// Fall through: still open the confirm so the operator can
			// force it deliberately; the modal spells out the fleet-wide
			// impact so a single reflex 'r' doesn't move accounts.
		}
		m.cpRun = &cpRunConfirm{
			cp:    cp,
			cfg:   s.cfg.Runner,
			lines: runnerPassPreview(s.cfg.Runner),
		}
		return m, nil, true
	}
	return m, nil, false
}

// runnerPassPreview describes what a one-shot runner pass would do, so
// the confirm modal is concrete rather than "run something."
func runnerPassPreview(r fleet.PlacementRunnerConfig) []string {
	var out []string
	if r.RestoreArchives {
		out = append(out, fmt.Sprintf("restore archived accounts (up to %d, any-region: %t)", r.RestoreBatch, r.RestoreAnyRegion))
	}
	if r.Rebalance {
		out = append(out, fmt.Sprintf("rebalance live accounts across cells (up to %d)", r.RebalanceBatch))
	}
	if len(out) == 0 {
		out = append(out, "runner has no steps enabled — the pass will be a no-op")
	}
	return out
}

// nextOption cycles to the choice after cur (wrapping); an unknown or
// empty cur lands on the first option.
func nextOption(opts []string, cur string) string {
	for i, o := range opts {
		if o == cur {
			return opts[(i+1)%len(opts)]
		}
	}
	return opts[0]
}

// handleCPApplyKey drives the apply-confirm modal: y writes, esc/q/n
// cancels. Every other key is swallowed while the modal is up so it
// can't leak into cell navigation.
func (m dashboardModel) handleCPApplyKey(msg tea.KeyMsg) (dashboardModel, tea.Cmd) {
	switch msg.String() {
	case "y":
		ap := m.cpApply
		m.cpApply = nil
		s := m.cpSettings[ap.cp]
		// The base the modal's diff was computed against MUST still be
		// what the state carries — a background read landing between
		// modal-open and 'y' would silently change the meaning of the
		// confirmation. maybeCPSettingsLoad blocks while a modal is up,
		// so this should always hold; check anyway.
		if s.cfg != ap.base {
			m.status = "config changed while the modal was open — re-open apply to see the new diff"
			return m, nil
		}
		s.applying = true
		s.readGen++
		m.cpSettings[ap.cp] = s
		m.status = "applying settings to " + shortHost(ap.cp) + "…"
		return m, m.cpApplyCmd(ap.cp, ap.next, ap.sections)
	case "esc", "q", "n", "ctrl+c":
		m.cpApply = nil
		m.status = "apply cancelled"
		return m, nil
	}
	return m, nil // swallow everything else
}

// handleCPRunKey drives the run-now confirm modal.
func (m dashboardModel) handleCPRunKey(msg tea.KeyMsg) (dashboardModel, tea.Cmd) {
	switch msg.String() {
	case "y":
		rc := m.cpRun
		m.cpRun = nil
		s := m.cpSettings[rc.cp]
		s.runInflight = true
		m.cpSettings[rc.cp] = s
		m.status = "runner pass started on " + shortHost(rc.cp) + "…"
		return m, m.cpRunCmd(rc.cp, rc.cfg)
	case "esc", "q", "n", "ctrl+c":
		m.cpRun = nil
		m.status = "run cancelled"
		return m, nil
	}
	return m, nil
}

// handleCPDiscardKey drives the discard-draft confirm modal.
func (m dashboardModel) handleCPDiscardKey(msg tea.KeyMsg) (dashboardModel, tea.Cmd) {
	switch msg.String() {
	case "y":
		dc := m.cpDiscard
		m.cpDiscard = nil
		s := m.cpSettings[dc.cp]
		if s.draft != nil {
			s.draft = nil
			s.readGen++
			m.cpSettings[dc.cp] = s
			m.status = "draft discarded — settings back to the control plane's values"
		}
		return m, nil
	case "esc", "q", "n", "ctrl+c":
		m.cpDiscard = nil
		return m, nil
	}
	return m, nil
}

// handleCPQuitKey drives the quit-with-dirty-draft confirm modal.
func (m dashboardModel) handleCPQuitKey(msg tea.KeyMsg) (dashboardModel, tea.Cmd) {
	switch msg.String() {
	case "y":
		return m, tea.Quit
	case "esc", "q", "n", "ctrl+c":
		m.cpQuit = nil
		return m, nil
	}
	return m, nil
}

// draftFor returns the CP's draft config, creating it from the
// authoritative config on first edit. Also snapshots preDraftBase so
// mid-apply edits can be re-applied against the response (mergeDraftEdits).
func (m *dashboardModel) draftFor(cp string) *cpConfig {
	if m.cpSettings == nil {
		m.cpSettings = map[string]cpSettingsState{}
	}
	s := m.cpSettings[cp]
	if s.draft == nil {
		d := s.cfg
		s.draft = &d
		s.preDraftBase = s.cfg
		m.cpSettings[cp] = s
	}
	return s.draft
}

// diffConfigs renders the field-by-field changes between the
// authoritative config and the draft for the confirm modal, prefixed
// by section so "enabled" is unambiguous.
func diffConfigs(cur, next cpConfig) []string {
	var out []string
	for _, f := range cpFields {
		a, b := f.value(cur), f.value(next)
		if a != b {
			out = append(out, fmt.Sprintf("%s · %s: %s → %s", f.section, f.name, a, b))
		}
	}
	return out
}

// --- rendering ----------------------------------------------------------

// renderCPSettingsTab paints the Settings form: every section's fields
// with the field cursor, dirty marks on edited fields, the
// unapplied-changes note, and the last run's summary.
func (m dashboardModel) renderCPSettingsTab(cp string, w int) []string {
	s := m.cpSettings[cp]
	var out []string

	switch {
	case s.err != nil:
		out = append(out, fitLine(styErr.Render("  ✗ "+oneLine(s.err.Error())), w))
		out = append(out, fitLine(styDim.Render("  read-only until the control plane is reachable"), w))
		return out
	case s.loaded.IsZero() && s.inflight:
		out = append(out, fitLine(styDim.Render("  "+m.spinGlyph()+" loading settings…"), w))
		return out
	case s.loaded.IsZero():
		out = append(out, fitLine(styDim.Render("  — settings not loaded yet"), w))
		return out
	}

	shown := s.cfg
	if s.draft != nil {
		shown = *s.draft
	}
	prevSection := ""
	for i, f := range cpFields {
		if f.section != prevSection {
			if prevSection != "" {
				out = append(out, "")
			}
			out = append(out, fitLine("  "+styTitle.Render(f.section), w))
			prevSection = f.section
		}
		marker := "  "
		if i == m.cpFieldSel && m.focus == focusContext {
			marker = styPlan.Render("▸") + " "
		}
		val := f.value(shown)
		if f.kind == cpFieldBool {
			if f.getB(shown) {
				val = styOK.Render("● on")
			} else {
				val = styDim.Render("○ off")
			}
		}
		// An in-progress int edit shows the buffer with a cursor.
		if m.cpEditing && i == m.cpFieldSel {
			val = m.cpEditBuf + "▏"
		}
		// Dirty mark: this field differs from the authoritative config.
		dirtyMark := ""
		if s.draft != nil && f.value(s.cfg) != f.value(*s.draft) {
			dirtyMark = " " + styPlan.Render("◆")
		}
		out = append(out, fitLine(fmt.Sprintf("  %s%-19s %s%s", marker, f.name, val, dirtyMark), w))
	}

	out = append(out, "")
	switch {
	case s.applying:
		out = append(out, fitLine("  "+styPlan.Render(m.spinGlyph()+" applying…"), w))
	case s.dirty():
		n := len(diffConfigs(s.cfg, *s.draft))
		out = append(out, fitLine("  "+styPlan.Render(fmt.Sprintf("◆ %d unapplied change(s)", n))+styDim.Render("  a apply · x discard"), w))
	default:
		out = append(out, fitLine(styDim.Render("  enter/space edit · r run now"), w))
	}
	if s.lastRun != "" {
		out = append(out, fitLine(styDim.Render("  last run: ")+s.lastRun, w))
	}
	out = append(out, fitLine(styDim.Render("  loaded "+humanAge(m.now(), s.loaded)+" ago"), w))
	return out
}

// render is the confirm modal's body.
func (c *cpApplyConfirm) render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "APPLY SETTINGS %s\n\n", shortHost(c.cp))
	b.WriteString("these changes drive fleet-wide automation (restores and the reaper act on live accounts):\n\n")
	for _, d := range c.diffs {
		b.WriteString("  " + d + "\n")
	}
	b.WriteString("\npress y to apply, esc to cancel.\n")
	return b.String()
}

func (c *cpRunConfirm) render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "RUN PLACEMENT PASS %s\n\n", shortHost(c.cp))
	b.WriteString("a one-shot pass moves LIVE customer accounts across cells:\n\n")
	for _, l := range c.lines {
		b.WriteString("  · " + l + "\n")
	}
	b.WriteString("\npress y to run now, esc to cancel.\n")
	return b.String()
}

func (c *cpDiscardConfirm) render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "DISCARD DRAFT %s\n\n", shortHost(c.cp))
	fmt.Fprintf(&b, "throw away %d unapplied change(s) — the form snaps back to the control plane's values.\n\n", c.count)
	b.WriteString("press y to discard, esc to keep.\n")
	return b.String()
}

func (c *cpQuitConfirm) render() string {
	var b strings.Builder
	b.WriteString("QUIT WITH UNAPPLIED CHANGES\n\n")
	fmt.Fprintf(&b, "%d unapplied change(s) on %s will be dropped — they live in memory only.\n\n", c.count, shortHost(c.cp))
	b.WriteString("press y to quit anyway, esc to stay.\n")
	return b.String()
}
