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
	loaded   time.Time
	inflight bool
	applying bool
	cfg      cpConfig
	err      error
	draft    *cpConfig // nil = no edits in progress
	lastRun  string    // one-line summary of the last `r` pass
}

// dirty reports whether the draft differs from the authoritative config.
func (s cpSettingsState) dirty() bool {
	return s.draft != nil && *s.draft != s.cfg
}

// cpApplyConfirm is the pending apply-confirm modal: which CP, the
// draft to write, which sections are dirty, and the diff lines.
type cpApplyConfirm struct {
	cp       string
	next     cpConfig
	sections cpSections
	diffs    []string
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

// validateDraft pre-checks the control plane's own rules so the
// operator hears about a bad combination before the confirm modal, not
// as an HTTP 400 after it.
func validateDraft(c cpConfig) string {
	if c.Reaper.Enabled && c.Reaper.TTLMinutes < 1 {
		return "reaper ttl must be ≥ 1 minute when the reaper is enabled"
	}
	if c.Placement.Strategy == "pinned" && c.Placement.PinnedCell == "" {
		return "pinned strategy needs a pinned cell"
	}
	return ""
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

// cpSettingsMsg carries a settings read back to Update.
type cpSettingsMsg struct {
	cp  string
	cfg cpConfig
	err error
}

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

func (m dashboardModel) cpSettingsCmd(cp string) tea.Cmd {
	ctx, cli, path := m.ctx, m.cli, m.configPath
	return func() tea.Msg {
		cfg, err := cli.readCPConfig(ctx, path, cp)
		return cpSettingsMsg{cp: cp, cfg: cfg, err: err}
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
// maybeHealthProbe for cells.
func (m dashboardModel) maybeCPSettingsLoad() (dashboardModel, tea.Cmd) {
	if !m.onCPSettings() {
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
	return m, m.cpSettingsCmd(cp)
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
	if m.cpEditing {
		switch {
		case key == "esc":
			m.cpEditing, m.cpEditBuf = false, ""
			return m, nil, true
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
		if s.loaded.IsZero() || s.err != nil {
			m.status = "settings not loaded — nothing to edit"
			return m, nil, true
		}
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
		return m, nil, true
	case "x":
		// Discard the draft — back to the authoritative config.
		if s.draft != nil {
			s.draft = nil
			m.cpSettings[cp] = s
			m.status = "draft discarded — settings back to the control plane's values"
		}
		return m, nil, true
	case "a":
		// Apply: pop the diff-confirm modal. Refuse quietly when
		// there's nothing to apply or a write is already in flight.
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
		m.cpApply = &cpApplyConfirm{
			cp:       cp,
			next:     *s.draft,
			sections: dirtySections(s.cfg, *s.draft),
			diffs:    diffConfigs(s.cfg, *s.draft),
		}
		return m, nil, true
	case "r":
		// One-shot runner pass with the AUTHORITATIVE config (never the
		// draft — an unapplied edit must not influence a live pass).
		if s.loaded.IsZero() || s.err != nil {
			m.status = "settings not loaded — cannot run"
			return m, nil, true
		}
		if s.applying {
			m.status = "an apply is in flight — wait for it before running"
			return m, nil, true
		}
		m.status = "runner pass started on " + shortHost(cp) + "…"
		return m, m.cpRunCmd(cp, s.cfg.Runner), true
	}
	return m, nil, false
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
// cancels. Everything else is swallowed while the modal is up.
func (m dashboardModel) handleCPApplyKey(msg tea.KeyMsg) (dashboardModel, tea.Cmd) {
	switch msg.String() {
	case "y":
		ap := m.cpApply
		m.cpApply = nil
		s := m.cpSettings[ap.cp]
		s.applying = true
		m.cpSettings[ap.cp] = s
		m.status = "applying settings to " + shortHost(ap.cp) + "…"
		return m, m.cpApplyCmd(ap.cp, ap.next, ap.sections)
	case "esc", "q", "n", "ctrl+c":
		m.cpApply = nil
		m.status = "apply cancelled"
		return m, nil
	}
	return m, nil
}

// draftFor returns the CP's draft config, creating it from the
// authoritative config on first edit.
func (m *dashboardModel) draftFor(cp string) *cpConfig {
	if m.cpSettings == nil {
		m.cpSettings = map[string]cpSettingsState{}
	}
	s := m.cpSettings[cp]
	if s.draft == nil {
		d := s.cfg
		s.draft = &d
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
