package main

// Control-plane tabs (issue #37). A header row's context pane gets its
// own tab set — Overview (the existing CP summary) and Settings (the
// placement runner's knobs, editable). Edits accumulate in a DRAFT
// copy per CP; nothing writes until the operator applies, which pops a
// diff-confirm modal, PUTs via SetPlacementRunner, and then renders
// the control plane's authoritative response — never the optimistic
// draft. `r` fires a one-shot runner pass.
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

// cpSettingsState is one control plane's placement-runner settings as
// the dashboard knows them: the authoritative config (last read or
// last apply response), an optional draft holding unapplied edits, and
// the async bookkeeping.
type cpSettingsState struct {
	loaded   time.Time
	inflight bool
	applying bool
	cfg      fleet.PlacementRunnerConfig
	err      error
	draft    *fleet.PlacementRunnerConfig // nil = no edits in progress
	lastRun  string                       // one-line summary of the last `r` pass
}

// dirty reports whether the draft differs from the authoritative
// config (PlacementRunnerConfig is all bools/ints — comparable).
func (s cpSettingsState) dirty() bool {
	return s.draft != nil && *s.draft != s.cfg
}

// cpApplyConfirm is the pending apply-confirm modal: which CP, and the
// human-readable diff lines shown for confirmation.
type cpApplyConfirm struct {
	cp    string
	next  fleet.PlacementRunnerConfig
	diffs []string
}

// --- field table -----------------------------------------------------

type cpFieldKind int

const (
	cpFieldBool cpFieldKind = iota
	cpFieldInt
)

// cpField describes one editable row of the Settings form.
type cpField struct {
	name string
	kind cpFieldKind
	getB func(fleet.PlacementRunnerConfig) bool
	setB func(*fleet.PlacementRunnerConfig, bool)
	getI func(fleet.PlacementRunnerConfig) int
	setI func(*fleet.PlacementRunnerConfig, int)
}

var cpFields = []cpField{
	{name: "enabled", kind: cpFieldBool,
		getB: func(c fleet.PlacementRunnerConfig) bool { return c.Enabled },
		setB: func(c *fleet.PlacementRunnerConfig, v bool) { c.Enabled = v }},
	{name: "restore archives", kind: cpFieldBool,
		getB: func(c fleet.PlacementRunnerConfig) bool { return c.RestoreArchives },
		setB: func(c *fleet.PlacementRunnerConfig, v bool) { c.RestoreArchives = v }},
	{name: "restore batch", kind: cpFieldInt,
		getI: func(c fleet.PlacementRunnerConfig) int { return c.RestoreBatch },
		setI: func(c *fleet.PlacementRunnerConfig, v int) { c.RestoreBatch = v }},
	{name: "restore any-region", kind: cpFieldBool,
		getB: func(c fleet.PlacementRunnerConfig) bool { return c.RestoreAnyRegion },
		setB: func(c *fleet.PlacementRunnerConfig, v bool) { c.RestoreAnyRegion = v }},
	{name: "rebalance", kind: cpFieldBool,
		getB: func(c fleet.PlacementRunnerConfig) bool { return c.Rebalance },
		setB: func(c *fleet.PlacementRunnerConfig, v bool) { c.Rebalance = v }},
	{name: "rebalance batch", kind: cpFieldInt,
		getI: func(c fleet.PlacementRunnerConfig) int { return c.RebalanceBatch },
		setI: func(c *fleet.PlacementRunnerConfig, v int) { c.RebalanceBatch = v }},
}

// fieldValue renders one field's value from a config.
func (f cpField) value(c fleet.PlacementRunnerConfig) string {
	if f.kind == cpFieldBool {
		if f.getB(c) {
			return "on"
		}
		return "off"
	}
	return strconv.Itoa(f.getI(c))
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

func (liveDataSource) placementRunner(ctx context.Context, configPath, cp string) (fleet.PlacementRunnerConfig, error) {
	fc, err := fleetClientFor(configPath, cp)
	if err != nil {
		return fleet.PlacementRunnerConfig{}, err
	}
	return fc.GetPlacementRunner(ctx)
}

func (liveDataSource) setPlacementRunner(ctx context.Context, configPath, cp string, cfg fleet.PlacementRunnerConfig) (fleet.PlacementRunnerConfig, error) {
	fc, err := fleetClientFor(configPath, cp)
	if err != nil {
		return fleet.PlacementRunnerConfig{}, err
	}
	return fc.SetPlacementRunner(ctx, cfg)
}

func (liveDataSource) runPlacementRunner(ctx context.Context, configPath, cp string, cfg fleet.PlacementRunnerConfig) (fleet.PlacementRunnerResult, error) {
	fc, err := fleetClientFor(configPath, cp)
	if err != nil {
		return fleet.PlacementRunnerResult{}, err
	}
	return fc.RunPlacementRunner(ctx, cfg)
}

// --- async plumbing ----------------------------------------------------

// cpSettingsMsg carries a placement-runner read back to Update.
type cpSettingsMsg struct {
	cp  string
	cfg fleet.PlacementRunnerConfig
	err error
}

// cpApplyMsg carries a SetPlacementRunner result back. cfg is the
// control plane's authoritative response.
type cpApplyMsg struct {
	cp  string
	cfg fleet.PlacementRunnerConfig
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
		cfg, err := cli.placementRunner(ctx, path, cp)
		return cpSettingsMsg{cp: cp, cfg: cfg, err: err}
	}
}

func (m dashboardModel) cpApplyCmd(cp string, cfg fleet.PlacementRunnerConfig) tea.Cmd {
	ctx, cli, path := m.ctx, m.cli, m.configPath
	return func() tea.Msg {
		got, err := cli.setPlacementRunner(ctx, path, cp, cfg)
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
				m.status = "batch must be a positive number — edit cancelled"
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
		if f.kind == cpFieldBool {
			d := m.draftFor(cp)
			f.setB(d, !f.getB(*d))
		} else {
			m.cpEditing = true
			m.cpEditBuf = ""
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
		m.cpApply = &cpApplyConfirm{cp: cp, next: *s.draft, diffs: diffConfigs(s.cfg, *s.draft)}
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
		return m, m.cpRunCmd(cp, s.cfg), true
	}
	return m, nil, false
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
		return m, m.cpApplyCmd(ap.cp, ap.next)
	case "esc", "q", "n", "ctrl+c":
		m.cpApply = nil
		m.status = "apply cancelled"
		return m, nil
	}
	return m, nil
}

// draftFor returns the CP's draft config, creating it from the
// authoritative config on first edit.
func (m *dashboardModel) draftFor(cp string) *fleet.PlacementRunnerConfig {
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
// authoritative config and the draft for the confirm modal.
func diffConfigs(cur, next fleet.PlacementRunnerConfig) []string {
	var out []string
	for _, f := range cpFields {
		a, b := f.value(cur), f.value(next)
		if a != b {
			out = append(out, fmt.Sprintf("%s: %s → %s", f.name, a, b))
		}
	}
	return out
}

// --- rendering ----------------------------------------------------------

// renderCPSettingsTab paints the Settings form: the placement runner's
// fields with the field cursor, dirty marks on edited fields, the
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
	out = append(out, fitLine("  "+styTitle.Render("placement runner"), w))
	for i, f := range cpFields {
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

// renderCPApplyDialog is the confirm modal's body.
func (c *cpApplyConfirm) render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "APPLY SETTINGS %s\n\n", shortHost(c.cp))
	b.WriteString("these changes drive fleet-wide automation (restores move live customer accounts):\n\n")
	for _, d := range c.diffs {
		b.WriteString("  " + d + "\n")
	}
	b.WriteString("\npress y to apply, esc to cancel.\n")
	return b.String()
}
