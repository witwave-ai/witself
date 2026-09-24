package agenttui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

func (d *detailWriter) heading(s string) {
	if len(d.lines) > 0 && d.m.height > 24 {
		d.line("")
	}
	s = single(s)
	title := d.m.style(d.m.colors().fg).Bold(true).Render(s)
	if remaining := d.width - ansi.StringWidth(s) - 2; remaining > 0 {
		title += "  " + d.m.style(d.m.colors().border).Render(strings.Repeat("─", remaining))
	}
	d.line(title)
}

// A pane-wide gutter keeps every field's value at the same display column.
// Narrow panes reserve most of their width for values; long labels stand alone.
func (d *detailWriter) kv(label, value string) {
	label, value = clean(label), clean(value)
	if value == "" {
		value = "not reported"
	}
	width := max(1, d.width)
	gutter := min(22, max(8, width/3))
	start := gutter + 2
	if width < 28 {
		start = min(2, width-1)
	}
	inline := width >= 28 && !strings.Contains(label, "\n") && ansi.StringWidth(label) <= gutter
	if !inline {
		d.dim(label)
	}
	valueWidth := max(1, width-start)
	// Wrap can exceed its limit when leading spaces precede a full-width word.
	// Bound those rows without dropping content before adding the gutter.
	wrapped := ansi.Hardwrap(ansi.Wrap(value, valueWidth, ""), valueWidth, true)
	rows := strings.Split(wrapped, "\n")
	for i, row := range rows {
		prefix := strings.Repeat(" ", start)
		if i == 0 && inline {
			prefix = d.m.style(d.m.colors().dim).Render(label + strings.Repeat(" ", start-ansi.StringWidth(label)))
		}
		d.lines = append(d.lines, prefix+d.m.style(d.m.colors().fg).Render(row))
	}
}

func (m *Model) compactNav() string {
	labels := []string{"Overview", "Transcr.", "Facts", "Memories", "Convs.", "Email", "Secrets"}
	available := m.account.context.available()
	if available {
		labels = append(labels, "Account")
	}
	build := func(short bool) string {
		parts := make([]string, 0, len(labels))
		for i, label := range labels {
			gap := " "
			if short {
				gap = ""
			}
			text := fmt.Sprintf("%d%s%s", i+1, gap, label)
			style := m.style(m.colors().dim)
			if i == m.panel && !m.account.mode || i == 7 && m.account.mode {
				text = "›" + text
				style = m.style(m.colors().accent).Bold(true)
			}
			parts = append(parts, style.Render(text))
		}
		return strings.Join(parts, " ")
	}
	// Keep familiar full labels when they fit, then remove number/label gaps.
	for _, short := range []bool{false, true} {
		if nav := build(short); ansi.StringWidth(nav) <= m.width && m.width >= 74 {
			return nav
		}
	}
	active := fmt.Sprintf("›%d %s · 1–7", m.panel+1, panelNames[m.panel])
	if m.account.mode {
		active = "›8 Account · 1–7"
	} else if available {
		active += " | 8 Account"
	}
	return active
}

func (m *Model) accountNav() string {
	width := max(1, m.account.vp.Width)
	lines, line := []string{}, ""
	for _, key := range m.account.context.sections {
		label := accountLabels[key]
		style := m.style(m.colors().dim)
		if key == m.accountSection() {
			label = "› " + label
			style = m.style(m.colors().accent).Bold(true)
		}
		item := style.Render(label)
		if line == "" {
			line = item
		} else if ansi.StringWidth(line)+2+ansi.StringWidth(item) > width {
			lines = append(lines, line)
			line = item
		} else {
			line += "  " + item
		}
	}
	return strings.Join(append(lines, line), "\n")
}

func (m *Model) accountAgentName() string {
	return first(str(obj(m.self["identity"]), "agent_name"), m.opts.Agent, "Agent")
}

// The compact strip is a view of verified manager context, never authority.
// If a value needs abbreviation, its full form is also rendered in scroll.
func (m *Model) accountContextStrip(d *detailWriter) bool {
	clipped := false
	compact := func(value string) string {
		text := single(value)
		clipped = clipped || text != value
		return text
	}
	account := compact(m.account.context.account)
	manager := compact(m.account.context.operator)
	role := compact(m.account.context.role)
	agent := compact(m.accountAgentName())
	for _, row := range []struct{ label, value, suffix string }{
		{"Account", account, " · Read-only"},
		{"Manager", manager + " · " + role + " · Agent " + agent, ""},
	} {
		label := row.label + "  "
		room := d.width - ansi.StringWidth(label+row.suffix)
		// Reserve Read-only on the account row at ordinary terminal widths.
		// Any clipped or normalized component is repeated losslessly in scroll.
		if room < 1 {
			clipped = true
			d.lines = append(d.lines, m.style(m.colors().dim).Render(ansi.Truncate(label+row.value+row.suffix, max(1, d.width), "…")))
			continue
		}
		clipped = clipped || ansi.StringWidth(row.value) > room
		d.lines = append(d.lines, m.style(m.colors().dim).Render(label)+
			m.style(m.colors().fg).Render(ansi.Truncate(row.value, room, "…"))+
			m.style(m.colors().dim).Render(row.suffix))
	}
	return clipped
}

func (m *Model) accountFocusHint() string {
	if m.accountSection() != "support" {
		return "Scroll · ↑↓/PgUp/PgDn"
	}
	if m.account.focus == 0 {
		return "Ticket selection · ↑↓ choose"
	}
	m.account.ticket.Lock()
	open := m.account.ticket.id != ""
	m.account.ticket.Unlock()
	if open {
		return "Thread scrolling · ↑↓/PgUp/PgDn"
	}
	return "Ticket list scrolling · ↑↓/PgUp/PgDn"
}
