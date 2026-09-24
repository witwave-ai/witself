package agenttui

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/witwave-ai/witself/internal/dashboard"
)

type palette struct{ bg, panel, fg, dim, accent, cyan, border, danger string }

func (m *Model) colors() palette {
	switch m.theme {
	case "paper":
		return palette{"#f6f4ef", "#fffdf8", "#2b2b28", "#65635b", "#1d7a4f", "#0f6f8f", "#dcd8cd", "#b3364a"}
	case "midnight":
		return palette{"#0b1220", "#111a2e", "#d7e1f2", "#8fa1bd", "#7fb1ff", "#5bd6ea", "#22304a", "#f2708a"}
	case "amber":
		return palette{"#0a0700", "#140e02", "#ffb000", "#b08830", "#ffd75e", "#ff9430", "#3a2a08", "#ff6b52"}
	case "high-contrast":
		return palette{"#000000", "#101010", "#ffffff", "#c8c8c8", "#ffd700", "#00e5ff", "#ffffff", "#ff8389"}
	default:
		return palette{"#0c1117", "#101823", "#c9d6e2", "#8b9bad", "#3ddc84", "#4dd7e8", "#1f2a36", "#ff6b81"} // auto: sensible dark default, without terminal probes
	}
}
func (m *Model) style(color string) lipgloss.Style {
	return lipgloss.NewStyle().Foreground(lipgloss.Color(color))
}
func fit(s string, w int) string {
	if w <= 0 {
		return ""
	}
	s = ansi.Truncate(s, w, "…")
	return s + strings.Repeat(" ", max(0, w-ansi.StringWidth(s)))
}
func (m *Model) layout() (rail, inventory, detail, height int) {
	top := 2
	if m.width < 100 {
		top++
	}
	height = max(1, m.height-top-2)
	if m.width >= 100 {
		rail = 18
	}
	remaining := m.width - rail
	if m.account.mode || (m.panel == 0 && m.summaryView != 3) {
		return rail, 0, remaining, height
	}
	if remaining < 60 {
		return rail, 0, remaining, height
	}
	inventory = min(32, max(24, remaining/3))
	detail = remaining - inventory
	return
}
func (m *Model) resize() {
	_, _, dw, h := m.layout()
	m.vp.Width = max(1, dw-4)
	m.vp.Height = max(1, h-2)
	m.account.vp.Width = max(1, dw-4)
	nav := ansi.Wrap(m.accountNav(), m.account.vp.Width, "")
	m.account.vp.Height = max(1, h-3-strings.Count(nav, "\n")-1)
}
func (m *Model) box(content string, w, h int, active bool) string {
	if w <= 0 {
		return ""
	}
	c := m.colors()
	border := c.border
	if active {
		border = c.cyan
	}
	if w < 4 || h < 3 {
		return crop(content, w, h)
	}
	// Lipgloss Width includes horizontal padding, but excludes borders.
	// Reserve two cells for borders and two more for the inner padding.
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color(border)).Padding(0, 1).Width(w - 2).Height(h - 2).Background(lipgloss.Color(c.bg)).Render(crop(content, w-4, h-2))
}
func crop(s string, w, h int) string {
	if w <= 0 || h <= 0 {
		return ""
	}
	ls := strings.Split(s, "\n")
	if len(ls) > h {
		ls = ls[:h]
	}
	for i := range ls {
		ls[i] = fit(ls[i], w)
	}
	for len(ls) < h {
		ls = append(ls, strings.Repeat(" ", max(0, w)))
	}
	return strings.Join(ls, "\n")
}

// View renders the workspace for the current terminal dimensions.
func (m *Model) View() string {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	return m.view()
}
func (m *Model) view() string {
	if m.closed.Load() {
		return ""
	}
	c := m.colors()
	id := obj(m.self["identity"])
	name := single(first(str(id, "agent_name"), m.opts.Agent, "Agent"))
	realm := single(first(str(id, "realm_name"), m.opts.Realm, "default"))
	initials := []rune(strings.ToUpper(name))
	monogram := "A"
	if len(initials) > 0 {
		monogram = string(initials[0])
	}
	badge := lipgloss.NewStyle().Foreground(lipgloss.Color(c.bg)).Background(lipgloss.Color(c.accent)).Bold(true).Render(" " + monogram + " ")
	brand := badge + " " + m.style(c.fg).Bold(true).Render("witself") + m.style(c.dim).Render(" / agent workspace")
	if m.width < 60 {
		brand = badge + " " + m.style(c.fg).Bold(true).Render("witself")
	}
	status := "● live"
	statusColor := c.accent
	if m.paused {
		status = "Ⅱ paused"
		statusColor = c.dim
	} else if m.busy {
		status = "◌ refreshing"
	}
	if m.selfStatus == "stale" || m.selfStatus == "unavailable" {
		status = "! " + m.selfStatus
		statusColor = c.danger
		if m.paused {
			status += " · paused"
		}
	}
	if m.opts.Demo {
		status += " · DEMO"
	}
	if web := m.consoleBadge(); web != "" {
		status += " · " + web
	}
	status += fmt.Sprintf(" · %s", m.opts.PollInterval)
	right := m.style(statusColor).Bold(m.selfStatus == "stale" || m.selfStatus == "unavailable").Render(status)
	gap := max(1, m.width-ansi.StringWidth(brand)-ansi.StringWidth(right))
	header := fit(brand+strings.Repeat(" ", gap)+right, m.width)
	identity := m.style(c.fg).Render(" "+name) + m.style(c.dim).Render("  /  "+realm+"  ·  "+single(first(str(id, "agent_id"), "identity pending")))
	version := first(single(m.opts.Version), single(str(m.self, "dashboard_version")))
	if version != "" {
		identity += m.style(c.dim).Render("  ·  v" + strings.TrimPrefix(version, "v"))
	}
	header += "\n" + fit(identity, m.width)
	rw, iw, dw, h := m.layout()
	if rw == 0 {
		labels := []string{"Overview", "Transcr.", "Facts", "Memories", "Convs.", "Email", "Secrets"}
		nav := []string{}
		for i, l := range labels {
			s := fmt.Sprintf("%d %s", i+1, l)
			if i == m.panel && !m.account.mode {
				s = m.style(c.accent).Bold(true).Render(s)
			} else {
				s = m.style(c.dim).Render(s)
			}
			nav = append(nav, s)
		}
		compact := " " + strings.Join(nav, "  ")
		if m.account.context.available() {
			compact += "  | 8 Account"
		}
		if ansi.StringWidth(compact) > m.width {
			active := fmt.Sprintf(" %d / 7  %s    1–7 switch", m.panel+1, panelNames[m.panel])
			if m.account.mode {
				active = " Account · 1–7 agent panels"
			}
			if m.account.context.available() {
				active += " | 8 Account"
			}
			if ansi.StringWidth(active) > m.width {
				active = fmt.Sprintf("%d %s · 1–7", m.panel+1, panelNames[m.panel])
				if m.account.context.available() {
					active += " | 8 Account"
				}
			}
			header += "\n" + fit(active, m.width)
		} else {
			header += "\n" + fit(compact, m.width)
		}
	}
	var body string
	if m.overlay != "" {
		body = m.box(m.overlayView(m.width-4, h-2), m.width, h, true)
	} else if m.account.mode {
		nav := ansi.Wrap(m.accountNav(), max(1, dw-4), "")
		body = m.box(m.style(c.accent).Render(nav)+"\n\n"+m.account.vp.View(), dw, h, true)
		if rw > 0 {
			body = lipgloss.JoinHorizontal(lipgloss.Top, m.railView(rw, h), body)
		}
	} else {
		inventory := ""
		if iw > 0 {
			inventory = m.box(m.inventoryView(iw-4, h-2), iw, h, m.focus == 0)
		}
		detail := m.box(m.vp.View(), dw, h, m.focus != 0 || iw == 0)
		body = lipgloss.JoinHorizontal(lipgloss.Top, inventory, detail)
		if rw > 0 {
			body = lipgloss.JoinHorizontal(lipgloss.Top, m.railView(rw, h), body)
		}
	}
	footer := m.footer()
	note := m.notice
	if note == "" {
		note = "r refresh · p pause · t theme · b browser · ? Legal"
		if m.overlay != "" {
			note = "Agent workspace · Legal: https://self.witwave.ai/legal"
		} else if m.panel == 0 && m.summaryView < 2 {
			note = "Enter open · a ASCII · " + note
		}
	}
	if m.account.mode && m.overlay == "" {
		focus := "Scroll"
		if m.account.focus == 0 && m.accountSection() == "support" {
			focus = "Ticket selection"
		}
		note = focus + " · Tab focus · r refresh · p pause display · ? help"
	}
	if m.filterEditing {
		note = "Type to filter · Enter keep · Esc cancel · Ctrl+U clear"
	}
	view := header + "\n" + body + "\n" + m.style(c.dim).Render(fit(footer, m.width)) + "\n" + m.style(c.cyan).Render(fit(note, m.width))
	base := lipgloss.NewStyle().Background(lipgloss.Color(c.bg)).Foreground(lipgloss.Color(c.fg))
	view = base.Render(crop(view, m.width, m.height))
	// Nested SGR resets restore the terminal's own palette, not an outer
	// Lipgloss style. Reassert the workspace defaults so Paper stays readable
	// in a dark terminal and unstyled spans do not create background patches.
	prefix, _, _ := strings.Cut(base.Render("x"), "x")
	if prefix != "" {
		view = strings.NewReplacer("\x1b[0m", "\x1b[0m"+prefix, "\x1b[m", "\x1b[0m"+prefix).Replace(view) + "\x1b[0m"
	}
	return view
}
func (m *Model) railView(w, h int) string {
	c := m.colors()
	lines := []string{"", m.style(c.dim).Render(" WORKSPACE"), ""}
	for i, n := range panelNames {
		s := fmt.Sprintf(" %d  %s", i+1, n)
		if i == m.panel && !m.account.mode {
			s = lipgloss.NewStyle().Foreground(lipgloss.Color(c.accent)).Background(lipgloss.Color(c.panel)).Bold(true).Render(fit(s, w-1))
		} else {
			s = m.style(c.dim).Render(s)
		}
		lines = append(lines, s, "")
	}
	if m.account.context.available() {
		for len(lines) < h-6 {
			lines = append(lines, "")
		}
		lines = append(lines, m.style(c.dim).Render(" ─ Account scope"), m.style(c.accent).Bold(m.account.mode).Render(" 8  Account"))
	}
	for len(lines) < h-3 {
		lines = append(lines, "")
	}
	lines = append(lines, m.style(c.dim).Render(" t  "+m.theme), m.style(c.dim).Render(" ?  Keyboard help"))
	return crop(strings.Join(lines, "\n"), w, h)
}
func (m *Model) inventoryView(w, h int) string {
	c := m.colors()
	s := &m.states[m.panel]
	rows := m.filtered()
	title := panelNames[m.panel]
	if m.panel == 0 {
		title = "Salient memories"
	}
	if m.panel == 5 {
		title = "Received"
		if m.sent {
			title = "Sent"
		}
		title += " · s switch"
	}
	lines := []string{m.style(c.fg).Bold(true).Render(fit(title, w))}
	filter := "/ Search " + strings.ToLower(panelNames[m.panel])
	if s.filter != "" || m.filterEditing {
		filter = "/ " + s.filter
		if m.filterEditing {
			filter += "▏"
		}
	}
	lines = append(lines, m.style(c.dim).Render(fit(filter, w)), "")
	state := s.status[primary(m.panel, m.sent)]
	if m.panel == 0 {
		state = m.selfStatus
	}
	if state != "ready" && state != "" && len(rows) > 0 {
		lines = append(lines, m.style(c.cyan).Render(fit(stateLabel(state), w)), "")
	}
	if len(rows) == 0 {
		empty := inventoryEmptyLabel(state, s.filter != "")
		lines = append(lines, m.style(c.dim).Render(empty))
		return crop(strings.Join(lines, "\n"), w, h)
	}
	stride := 3
	if h < 18 {
		stride = 2
	}
	count := max(1, (h-len(lines)-1)/stride)
	start := max(0, s.selected-count+1)
	end := min(len(rows), start+count)
	for i := start; i < end; i++ {
		r := rows[i]
		mark := "  "
		style := m.style(c.fg)
		if i == s.selected {
			mark = "› "
			style = style.Foreground(lipgloss.Color(c.accent)).Bold(true)
		}
		lines = append(lines, style.Render(fit(mark+r.title, w)), m.style(c.dim).Render(fit("  "+r.subtitle, w)))
		if stride == 3 {
			lines = append(lines, "")
		}
	}
	for len(lines) < h-1 {
		lines = append(lines, "")
	}
	lines = append(lines, m.style(c.dim).Render(fmt.Sprintf("%d / %d  ·  %s", s.selected+1, len(rows), map[bool]string{true: "inventory focus", false: "Tab to focus"}[m.focus == 0])))
	return crop(strings.Join(lines, "\n"), w, h)
}
func inventoryEmptyLabel(state string, filtered bool) string {
	if state == "ready" {
		if filtered {
			return "No matching items."
		}
		return "No items yet."
	}
	if state == "" || state == "loading" {
		return "Loading inventory…"
	}
	return stateLabel(state)
}
func stateLabel(s string) string {
	switch s {
	case "ready":
		return "Ready"
	case "loading", "":
		return "Loading…"
	case "disabled":
		return "Disabled by current policy"
	case "unsupported":
		return "Unsupported on this cell"
	case "unenrolled":
		return "Agent is not enrolled"
	case "stale":
		return "Stale · last loaded data"
	default:
		return "Temporarily unavailable"
	}
}
func (m *Model) footer() string {
	if m.overlay == "console" {
		return " s start  b open  x stop  r check  Esc close  Ctrl+C quit"
	}
	if m.overlay == "theme" {
		return " ↑↓ choose  Enter save  Esc cancel  Ctrl+C quit"
	}
	if m.overlay != "" {
		return " ↑↓/PgUp/PgDn scroll  Esc close  Ctrl+C quit"
	}
	if m.account.mode {
		if m.accountSection() == "clients" {
			return "[ ] section · x Check this device · 1–7 agent · t theme · q quit"
		}
		if m.accountSection() == "support" {
			return "[ ] section · Enter thread · Esc clear · 1–7 agent · t theme · q quit"
		}
		return "[ ] section · ↑↓/PgUp/PgDn scroll · 1–7 agent · t theme · q quit"
	}
	if m.filterEditing {
		return " / filtering  Enter keep  Esc cancel  Ctrl+C quit"
	}
	if m.panel == 0 && m.summaryView != 3 {
		mode := m.summaryFocusHint()
		full := mode + "  o/l/e/d views  w web  ? help  q quit"
		if ansi.StringWidth(full) <= m.width {
			return full
		}
		short := "Categories"
		if m.summaryView == 2 || m.focus != 0 {
			short = "Scroll"
		}
		compact := short + " · ↑↓ " + map[bool]string{true: "scroll", false: "choose"}[short == "Scroll"]
		if m.summaryView != 2 {
			compact += " · Tab focus"
		}
		if ansi.StringWidth(compact+"  ? help  q quit") <= m.width {
			return compact + "  ? help  q quit"
		}
		return short + "  ? help  q quit"
	}
	actions := "Enter detail"
	switch m.panel {
	case 1:
		actions = "[ ] entry  x expand"
	case 2:
		actions = "v reveal/hide  c copy"
	case 3:
		actions = "v reveal/hide  e evidence"
	case 4:
		actions = "[ ] message  v body"
	case 5:
		actions = "s mailbox"
		if !m.sent {
			actions += "  u unread  a unacked"
		}
	case 6:
		actions = "[ ] field  v reveal  c copy"
	}
	if m.panel == 0 {
		actions = "o/l/e/d views  Enter memory"
	}
	suffix := "  w web  ? help  q quit"
	full := " Tab focus  / filter  " + actions + suffix
	if ansi.StringWidth(full) <= m.width {
		return full
	}
	if ansi.StringWidth(actions+suffix) <= m.width {
		return actions + suffix
	}
	return "? help  q quit  w web"
}
func (m *Model) overlayView(w, h int) string {
	if w <= 0 || h <= 0 {
		return ""
	}
	c := m.colors()
	if m.overlay == "console" {
		return m.consoleView(w, h)
	}
	if m.overlay == "theme" {
		ls := []string{m.style(c.accent).Bold(true).Render("Choose your atmosphere"), "Shared with the web console. Auto uses a dark default.", ""}
		for i, n := range themeNames {
			prefix := "  "
			if i == m.overlayIndex {
				prefix = "› "
			}
			s := prefix + n
			if n == m.theme {
				s += "  ✓"
			}
			if i == m.overlayIndex {
				s = m.style(c.accent).Bold(true).Render(s)
			}
			ls = append(ls, s, "")
		}
		ls = append(ls, "↑↓ choose · Enter save · Esc cancel")
		return crop(strings.Join(ls, "\n"), w, h)
	}
	help := []string{
		"KEYBOARD / EVERY CONTROL", "", "Workspace", "1–7              Open one of seven panels", "Tab / Shift+Tab  Inventory → detail → detail actions", "j/k or ↑/↓       Move rows, scroll detail, or select action", "Enter            Focus detail / activate selected action", "Esc              Hide private value, go back, or clear filter", "/                Edit inventory filter; Enter keep, Esc cancel", "Ctrl+U           Clear filter while editing", "r                Refresh current panel and identity", "p                Pause / resume live refresh", "t                Choose and save a shared theme", "b                Start or reuse the web console and open browser", "w                Web console status and start / stop controls", "?                Open this help; ↑↓ / PgUp / PgDn scroll", "q / Ctrl+C       Quit and cancel in-flight requests", "", "Summary", "o / l / e        Overview / Timeline / Recent updates", "d                Workspace details and salient memories", "a                Toggle plain ASCII graph characters", "Tab              Switch category selection and scrolling", "Enter            Open the selected category", "", "Web console (w)", "s / b            Start / open in browser", "x / r            Stop / check status", "                 TUI-owned consoles stop when the TUI exits", "                 Existing consoles stay running until explicit stop", "                 Demo never starts a console or opens a browser", "", "Reading", "PgUp / PgDn      Scroll half a page", "Ctrl+U / Ctrl+D  Scroll half a page (outside filter)", "g / Home         Top of detail", "G / End          Bottom of detail / latest transcript entries", "[ / ]            Previous / next detail action", "", "Transcripts", "x / Enter        Expand or collapse selected JSON entry", "[ / ]            Select entry; evidence range is highlighted", "Esc              Return from evidence to its memory", "", "Facts", "v                Explicitly reveal / hide only selected fact", "c                Copy exact fact without displaying it", "                 Requires the application's clipboard callback", "                 Sensitive assertion history always stays hidden", "", "Memories", "v                Reveal / hide sensitive memory content", "e                Focus evidence; ↑↓ selects a locator", "Enter            Open selected transcript evidence", "                 Other locators remain plain text", "", "Conversations", "[ / ]            Select a received or sent message", "v / Enter        Explicitly show / hide selected received body", "                 Sent bodies and all payloads remain unavailable", "", "Email", "s                Switch Received / Sent", "u                Toggle received unread-only filter", "a                Toggle received unacknowledged-only filter", "                 Metadata only; sender claims are unverified", "", "Secrets", "[ / ]            Select one secret field", "v / Enter        Reveal / hide selected non-TOTP field", "c                Copy exact field without displaying it", "                 Revealed values auto-hide after 30 seconds", "                 Hide, navigation, refresh, and quit clear values", "                 TOTP seeds are never available", "", "The seven agent panels use the selected agent's authority.", "Themes save preferences; secret access records a value-free receipt.", "Legal: https://self.witwave.ai/legal",
	}
	if m.account.context.available() {
		accountHelp := []string{"Account · read-only manager scope", "8                Open Account; 1–7 return to agent panels", "[ / ] or ← / →   Switch permitted subsections", "↑↓ / PgUp/PgDn   Scroll details", "r                Refresh cached section", "p                Pause display; authority checks continue", "t / b / w        Theme / browser / web console controls", "Account uses the current verified manager, distinct from the agent.", ""}
		if slices.Contains(m.account.context.sections, "support") {
			accountHelp = append(accountHelp, "Tab              Ticket selection / detail scrolling", "Enter            Deliberately open selected support thread", "Esc              Clear selected thread and rendered text", "Refresh or leaving also clears the selected thread.")
		}
		if slices.Contains(m.account.context.sections, "clients") {
			accountHelp = append(accountHelp, "x                Check this device (Clients only; explicit scan)")
		}
		if m.account.mode {
			help = append(accountHelp, help...)
		} else {
			help = append(help, accountHelp...)
		}
	}
	lines := []string{}
	for _, l := range help {
		lines = append(lines, strings.Split(ansi.Wrap(l, max(1, w), ""), "\n")...)
	}
	m.helpScroll = min(m.helpScroll, max(0, len(lines)-h))
	return crop(strings.Join(lines[m.helpScroll:], "\n"), w, h)
}

type detailWriter struct {
	m     *Model
	lines []string
	width int
}

func (d *detailWriter) line(s string) {
	d.lines = append(d.lines, strings.Split(ansi.Wrap(s, max(1, d.width), ""), "\n")...)
}
func (d *detailWriter) body(s string) { d.line(clean(s)) }
func (d *detailWriter) dim(s string)  { d.line(d.m.style(d.m.colors().dim).Render(clean(s))) }
func (d *detailWriter) heading(s string) {
	if len(d.lines) > 0 {
		d.line("")
	}
	d.line(d.m.style(d.m.colors().cyan).Bold(true).Render(single(s)))
}
func (d *detailWriter) kv(label, value string) {
	if value == "" {
		value = "not reported"
	}
	d.line(d.m.style(d.m.colors().dim).Render(label+"  ") + clean(value))
}
func (d *detailWriter) fields(o object, fields string) {
	for _, k := range strings.Fields(fields) {
		if v, ok := o[k]; ok && v != nil {
			d.kv(strings.ReplaceAll(k, "_", " "), valueText(v))
		}
	}
}
func (d *detailWriter) action(i int, label string, highlight bool) {
	for len(d.m.actionOffsets) <= i {
		d.m.actionOffsets = append(d.m.actionOffsets, 0)
	}
	d.m.actionOffsets[i] = len(d.lines)
	mark := "  "
	style := d.m.style(d.m.colors().fg)
	if i == d.m.sub {
		mark = "› "
		style = style.Foreground(lipgloss.Color(d.m.colors().accent)).Bold(true)
	}
	if highlight {
		mark = "▌ "
		style = style.Foreground(lipgloss.Color(d.m.colors().cyan)).Bold(true)
	}
	d.line(style.Render(mark + single(label)))
}
func (m *Model) revealAction() {
	if m.sub < len(m.actionOffsets) {
		line := m.actionOffsets[m.sub]
		if line < m.vp.YOffset || line >= m.vp.YOffset+m.vp.Height {
			m.vp.SetYOffset(max(0, line-2))
		}
	}
}
func (m *Model) renderDetail(follow bool) {
	if m.account.mode {
		m.renderAccount()
		return
	}
	old := m.vp.YOffset
	m.actionOffsets = nil
	d := &detailWriter{m: m, width: m.vp.Width}
	s := &m.states[m.panel]
	row := m.current()
	if m.panel == 0 {
		m.overview(d)
	} else {
		title := panelNames[m.panel]
		if row != nil {
			title = row.title
			if m.panel == 3 && !flag(row.data, "redacted") {
				title = first(strings.Split(str(row.data, "content"), "\n")[0], title)
			}
			title = ansi.Truncate(title, max(20, m.vp.Width*2-2), "…")
		}
		d.heading(title)
		state := s.status[primary(m.panel, m.sent)]
		if state != "ready" {
			d.dim(stateLabel(state))
		}
		if m.panel == 5 {
			m.emailSummary(d)
		}
		if row == nil {
			d.line("")
			d.body(inventoryEmptyLabel(state, s.filter != ""))
		} else {
			switch m.panel {
			case 1:
				m.transcriptDetail(d)
			case 2:
				m.factDetail(d, *row)
			case 3:
				m.memoryDetail(d)
			case 4:
				m.conversationDetail(d, *row)
			case 5:
				m.emailDetail(d, row.data)
			case 6:
				m.secretDetail(d)
			}
		}
	}
	m.vp.SetContent(strings.Join(d.lines, "\n"))
	if follow {
		m.vp.GotoBottom()
	} else {
		m.vp.SetYOffset(old)
	}
}
func (m *Model) overview(d *detailWriter) {
	if m.summaryView != 3 {
		m.summaryOverview(d)
		return
	}
	d.heading("A clear view of your workspace")
	d.dim("o overview · l timeline · e updates · Enter opens a salient memory")
	counts := obj(obj(m.self["index"])["counts"])
	d.line("")
	names := []string{"transcripts", "facts", "memories", "messages", "secrets"}
	labels := []string{"2 Logs", "3 Facts", "4 Memories", "5 Chats", "7 Secrets"}
	cols := min(5, max(1, d.width/13))
	cw := max(1, (d.width-(cols-1))/cols)
	for start := 0; start < len(names); start += cols {
		cards := []string{}
		for i := start; i < min(start+cols, len(names)); i++ {
			n := first(str(counts, names[i]), "—")
			cards = append(cards, fit(m.style(m.colors().accent).Bold(true).Render(n), cw)+"\n"+fit(m.style(m.colors().dim).Render(labels[i]), cw))
		}
		d.line(lipgloss.JoinHorizontal(lipgloss.Top, cards...))
		d.line("")
	}
	d.heading("Capacity")
	for _, x := range []struct{ k, label string }{{"memory_capacity", "Memory"}, {"fact_capacity", "Facts"}} {
		d.kv(x.label, capacityText(obj(m.self[x.k]), false))
	}
	d.heading("Checkpoints")
	pending := false
	for _, x := range []struct{ k, label string }{{"memory_checkpoint", "Memory curation"}, {"message_checkpoint", "Messaging"}, {"email_checkpoint", "Email"}, {"avatar_checkpoint", "Avatar lifecycle"}} {
		cp := obj(m.self[x.k])
		if flag(cp, "pending") {
			pending = true
			d.kv("◌ "+x.label, first(str(cp, "reason"), "pending"))
		} else if flag(cp, "unavailable") {
			d.kv(x.label, "unavailable")
		} else if b, ok := cp["enabled"].(bool); ok && !b {
			d.kv(x.label, "disabled")
		}
	}
	if !pending {
		d.dim("No pending work reported.")
	}
	d.heading("Enforced plan")
	e, ok := m.self["plan_entitlements"]
	plan := obj(e)
	if !ok {
		d.body("Pre-feature cell · applied entitlement projection is not available.")
	} else {
		switch str(plan, "state") {
		case "unmanaged":
			d.body("Unmanaged · no applied plan snapshot; no plan is implied.")
		case "applied":
			d.body(str(plan, "enforced_plan_id") + " · applied")
			d.dim("Source: cell-applied snapshot")
			for _, f := range []string{"memory", "facts", "secrets", "messaging", "collaboration", "agent_email_receive", "agent_email_send"} {
				v, known := obj(plan["features"])[f].(bool)
				label := "unavailable"
				if known {
					label = "disabled"
					if v {
						label = "enabled"
					}
				}
				d.kv(strings.ReplaceAll(f, "_", " "), label)
			}
			d.heading("Retention")
			ret := obj(plan["retention_days"])
			for _, k := range []string{"transcript_retention_days", "message_retention_days", "agent_email_retention_days"} {
				v, exists := ret[k]
				s := "unavailable"
				if exists && v == nil {
					s = "indefinite"
				} else if num(ret, k) > 0 {
					s = str(ret, k) + " days"
				}
				d.kv(strings.ReplaceAll(strings.TrimSuffix(k, "_retention_days"), "_", " "), s)
			}
		default:
			d.body("Unavailable · cell-applied entitlement status could not be loaded.")
		}
	}
	d.heading("Read compatibility")
	if v, ok := m.self["observational"].(bool); ok {
		if v {
			d.body("Observational hooks available. These passive views do not record usage.")
		} else {
			d.body("Self has no observational hooks; plain self reads are in use. Other panels retain their own compatibility checks.")
		}
	} else {
		d.body("Observational compatibility has not been reported.")
	}
	d.dim("Agent authority only. No billing or administration controls.")
	d.heading("Legal")
	d.body("https://self.witwave.ai/legal")
}
func capacityText(c object, bytes bool) string {
	if len(c) == 0 || flag(c, "unavailable") {
		return "unavailable"
	}
	f := func(k string) string {
		if bytes {
			return byteSize(num(c, k))
		}
		return first(str(c, k), "0")
	}
	if flag(c, "unlimited") {
		return f("used") + " used · unlimited"
	}
	state := "available"
	if flag(c, "over_limit") {
		state = "over limit"
	} else if flag(c, "at_limit") {
		state = "at limit"
	} else if flag(c, "near_limit") {
		state = "near limit"
	}
	return f("used") + " / " + f("max") + " · " + state + " · " + f("remaining") + " remaining"
}
func byteSize(n int) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	if n < 1024*1024 {
		return fmt.Sprintf("%.1f KiB", float64(n)/1024)
	}
	return fmt.Sprintf("%.1f MiB", float64(n)/(1024*1024))
}
func (m *Model) detailState(d *detailWriter, r dashboard.Resource) bool {
	s := m.states[m.panel].status[r]
	if s != "ready" {
		d.dim(stateLabel(s))
		return m.states[m.panel].data[r] != nil
	}
	return true
}
func (m *Model) transcriptDetail(d *detailWriter) {
	d.dim("Live tail · [ ] select entry · x expand/collapse JSON")
	if m.evidenceFrom > 0 {
		d.body(fmt.Sprintf("Evidence #%d–%d · Esc returns to memory", m.evidenceFrom, m.evidenceUntil))
	}
	if !m.detailState(d, dashboard.ResourceTranscript) {
		return
	}
	page := m.states[1].data[dashboard.ResourceTranscript]
	d.fields(obj(page["transcript"]), "id external_id updated_at")
	entries := list(page, "entries")
	if len(entries) == 0 {
		d.body("No transcript entries yet.")
	}
	for i, e := range entries {
		d.line("")
		seq := num(e, "sequence")
		anchored := m.evidenceFrom > 0 && seq >= m.evidenceFrom && seq <= m.evidenceUntil
		d.action(i, fmt.Sprintf("#%d  %s", seq, str(e, "role")), anchored)
		body := str(e, "body")
		var structured map[string]any
		if strings.HasPrefix(strings.TrimSpace(body), "{") && json.Unmarshal([]byte(body), &structured) == nil && structured != nil {
			keys := make([]string, 0, len(structured))
			for k := range structured {
				keys = append(keys, clean(k))
			}
			sort.Strings(keys)
			summary := str(obj(structured), "tool_name")
			if summary == "" {
				summary = "{ " + strings.Join(keys[:min(len(keys), 4)], ", ") + " }"
			}
			d.dim("◇ " + summary + " · x " + map[bool]string{true: "collapse", false: "expand"}[m.expanded[seq]])
			if m.expanded[seq] {
				b, _ := json.MarshalIndent(structured, "", "  ")
				d.body(string(b))
			}
		} else {
			d.body(body)
		}
	}
}
func (m *Model) factDetail(d *detailWriter, row item) {
	f := row.data
	d.dim("v reveal / hide  ·  c copy without display")
	d.heading("Current value")
	text, kind, key, status := m.privateView()
	if kind == "fact" && key == row.key && status != "" {
		if status == "visible" {
			d.body(text)
		} else {
			d.dim("Exact reveal: " + status + " · v hide; v again retries")
		}
	} else if flag(f, "sensitive") || flag(f, "redacted") {
		d.body("[sensitive value hidden]")
	} else {
		d.body(valueText(f["value"]))
	}
	d.heading("Fact details")
	d.fields(f, "id subject predicate value_type cardinality source_kind source_ref confidence sensitive usage_count created_at updated_at observed_at")
	d.heading("Assertion history")
	if flag(f, "sensitive") || flag(f, "redacted") {
		d.dim("Sensitive history values always remain redacted.")
	}
	if !m.detailState(d, dashboard.ResourceFactHistory) {
		return
	}
	assertions := list(m.states[2].data[dashboard.ResourceFactHistory], "assertions")
	if len(assertions) == 0 {
		d.dim("No assertions reported.")
	}
	for _, a := range assertions {
		v := "[redacted]"
		if !flag(f, "sensitive") && !flag(f, "redacted") && !flag(a, "sensitive") && !flag(a, "redacted") {
			v = valueText(a["value"])
		}
		d.body(v)
		d.dim(str(a, "source_kind") + " · confidence " + str(a, "confidence") + " · " + first(str(a, "observed_at"), str(a, "created_at")))
		d.line("")
	}
}
func (m *Model) memoryDetail(d *detailWriter) {
	d.dim("e focus evidence  ·  ↑↓ select  ·  Enter open transcript")
	if !m.detailState(d, dashboard.ResourceMemory) {
		return
	}
	mem := obj(m.states[3].data[dashboard.ResourceMemory]["memory"])
	d.line("")
	text, kind, key, status := m.privateView()
	if kind == "memory" && key == str(mem, "id") && status == "visible" {
		d.body(text)
	} else if kind == "memory" && key == str(mem, "id") && status != "" {
		d.dim("Private content: " + status)
	} else if flag(mem, "redacted") || flag(mem, "sensitive") {
		d.body("[sensitive memory hidden · v to reveal]")
	} else {
		d.body(str(mem, "content"))
	}
	d.heading("Memory details")
	d.fields(mem, "id kind state state_reason salience version origin sensitive tags links created_at updated_at occurred_from occurred_until")
	d.heading("Evidence")
	es := list(mem, "evidence")
	if len(es) == 0 {
		d.dim("No evidence locators.")
	}
	for i, e := range es {
		label := str(e, "transcript_id")
		if label != "" {
			label += fmt.Sprintf(" #%d–%d", num(e, "entry_from_sequence"), max(num(e, "entry_from_sequence"), num(e, "entry_until_sequence")))
		} else {
			label = first(str(e, "source_memory_id"), str(e, "message_id"), str(e, "import_artifact_id"), str(e, "external_locator"), str(e, "id"))
		}
		d.action(i, label, false)
		d.dim("  " + str(e, "role") + " · " + str(e, "state"))
		d.fields(e, "unavailable_reason unresolvable_reason")
	}
	d.heading("Version history")
	if !m.detailState(d, dashboard.ResourceMemoryHistory) {
		return
	}
	vs := list(m.states[3].data[dashboard.ResourceMemoryHistory], "versions")
	if len(vs) == 0 {
		d.dim("No versions reported.")
	}
	for _, v := range vs {
		d.body("v" + str(v, "version") + " · " + str(v, "operation") + " · " + str(v, "state"))
		d.dim(str(v, "created_at"))
	}
}
func (m *Model) conversationDetail(d *detailWriter, row item) {
	d.dim("[ ] select message · v show / hide received body")
	text, kind, key, status := m.privateView()
	for i, msg := range row.messages {
		d.line("")
		d.action(i, strings.ToUpper(str(msg, "_dir"))+" · "+first(str(msg, "subject"), "(no subject)"), false)
		from, to := obj(msg["from"]), obj(msg["to"])
		d.kv("from", first(str(from, "agent_name"), str(from, "agent_id"), "unknown"))
		dest := first(str(to, "agent_name"), str(to, "agent_id"), str(to, "kind"))
		if str(to, "kind") == "agents" {
			dest += " · " + str(to, "count") + " agents"
		}
		d.kv("to", dest)
		d.dim(str(msg, "kind") + " · " + str(msg, "created_at"))
		d.dim("read " + first(str(obj(msg["read_state"]), "state"), "unknown") + " · delivery " + first(str(obj(msg["delivery"]), "state"), "unknown"))
		if str(msg, "_dir") == "sent" {
			d.dim("[sent body unavailable]")
		} else if kind == "message" && key == str(msg, "id") && status != "" {
			if status == "visible" {
				d.body(text)
				d.dim("v Hide body")
			} else {
				d.dim("Body " + status + " · v hide; v again retries")
			}
		} else {
			d.dim("[body hidden] · select then v Show body")
		}
	}
}
func (m *Model) emailSummary(d *detailWriter) {
	d.dim("s Received / Sent  ·  metadata only")
	if m.sent {
		d.dim("Sent remains available independently of inbound email.")
		return
	}
	d.dim(fmt.Sprintf("u unread only: %t · a unacknowledged only: %t", m.unread, m.unacked))
	s := m.states[5]
	a := obj(s.data[dashboard.ResourceEmailAddress]["address"])
	d.heading("Receive address")
	if len(a) == 0 {
		d.dim(stateLabel(s.status[dashboard.ResourceEmailAddress]))
	} else {
		if s.status[dashboard.ResourceEmailAddress] != "ready" {
			d.dim(stateLabel(s.status[dashboard.ResourceEmailAddress]))
		}
		d.fields(a, "address receive_state agent_receive_state realm_receive_state")
	}
	status := s.data[dashboard.ResourceEmailStatus]
	d.heading("Account-wide attachment capacity")
	if s.status[dashboard.ResourceEmailStatus] != "ready" {
		d.dim(stateLabel(s.status[dashboard.ResourceEmailStatus]))
	}
	d.body(capacityText(obj(status["attachment_capacity"]), true))
	if status != nil {
		d.kv("maximum raw message", byteSize(num(status, "maximum_raw_bytes")))
	}
}
func (m *Model) emailDetail(d *detailWriter, e object) {
	if m.sent {
		d.heading("Delivery lifecycle")
		d.fields(e, emailSentFields)
		return
	}
	d.heading("Received metadata")
	d.body("Sender identity and provider signals are unverified.")
	d.fields(e, emailReceivedFields)
	d.heading("Read / acknowledgement")
	d.fields(obj(e["read_state"]), "state read_at acked_at code_consumed_at")
	d.heading("Processing")
	d.fields(obj(e["processing"]), "state failure_count completed_at")
	if str(e, "payload_retention_state") == "omitted_capacity" {
		d.heading("Retention warning")
		d.body("Attachment payload omitted because account-wide capacity is full.")
	}
	if flag(e, "possible_duplicate") {
		d.body("Possible duplicate.")
	}
}
func (m *Model) secretDetail(d *detailWriter) {
	d.dim("[ ] select field · v reveal/hide · c copy without display")
	if !m.detailState(d, dashboard.ResourceSecret) {
		return
	}
	page := m.states[6].data[dashboard.ResourceSecret]
	s := obj(page["secret"])
	d.heading("Fields")
	text, kind, key, status := m.privateView()
	fields := list(s, "fields")
	if len(fields) == 0 {
		d.dim("No fields reported.")
	}
	for i, f := range fields {
		label := "public field"
		if flag(f, "sensitive") {
			label = "sensitive"
		}
		d.action(i, first(str(f, "name"), str(f, "id"))+" · "+str(f, "kind")+" · "+label, false)
		if strings.EqualFold(str(f, "kind"), "totp") {
			d.dim("  TOTP seed · reveal and copy unavailable")
			continue
		}
		if kind == "secret" && key == m.states[6].key+"/"+str(f, "id") && status != "" {
			if status == "visible" {
				d.body(text)
				d.dim("  v hide · automatically hidden after 30 seconds")
			} else {
				d.dim("  " + status + " · v hide; v again retries")
			}
		} else {
			d.dim("  [value hidden] · v reveal · c copy")
		}
	}
	d.heading("Inventory details")
	d.fields(s, "id name description template lifecycle tags field_count sensitive_field_count created_at updated_at archived_at")
	d.heading("Public vault binding")
	d.fields(obj(page["vault_key"]), "id key_version algorithm fingerprint lifecycle_state created_at")
}
