package agenttui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// Account presentation is independent of the seven Agent detail views.
type accountWriter struct{ detailWriter }

func (d *accountWriter) line(s string) {
	width := max(1, d.width)
	d.lines = append(d.lines, strings.Split(ansi.Hardwrap(ansi.Wrap(s, width, ""), width, true), "\n")...)
}

func (d *accountWriter) body(s string) { d.styledText(s, d.m.style(d.m.colors().fg)) }
func (d *accountWriter) dim(s string)  { d.styledText(s, d.m.style(d.m.colors().dim)) }

func (d *accountWriter) styledText(s string, style lipgloss.Style) {
	start := len(d.lines)
	d.line(clean(s))
	// Render pads multiline input to its longest source line. Wrap first and
	// style rows independently so bounded messages cannot expand quadratically.
	for i := start; i < len(d.lines); i++ {
		d.lines[i] = style.Render(d.lines[i])
	}
}

func (d *accountWriter) heading(s string) {
	if len(d.lines) > 0 {
		d.line("")
	}
	d.styledText(s, d.m.style(d.m.colors().fg).Bold(true))
	last := len(d.lines) - 1
	if remaining := d.width - ansi.StringWidth(d.lines[last]) - 2; remaining > 0 {
		d.lines[last] += "  " + d.m.style(d.m.colors().border).Render(strings.Repeat("─", remaining))
	}
}

// Records have a lighter title. Return its actual offset for ticket selection.
func (d *accountWriter) title(s string) int {
	offset := len(d.lines)
	d.styledText(s, d.m.style(d.m.colors().fg).Bold(true))
	return offset
}

func (d *accountWriter) kv(label, value string) {
	label, value = clean(label), clean(value)
	if value == "" {
		value = "not reported"
	}
	width := max(1, d.width)
	gutter := min(22, max(8, width/3))
	inline := width >= 28 && !strings.Contains(label, "\n") && ansi.StringWidth(label) <= gutter
	start := gutter + 2
	if !inline {
		// Preserve even leading spaces and long tokens in both stacked rows.
		for _, row := range strings.Split(ansi.Hardwrap(label, width, true), "\n") {
			d.lines = append(d.lines, d.m.style(d.m.colors().dim).Render(row))
		}
		start = min(2, width-1)
	}
	for i, row := range strings.Split(ansi.Hardwrap(value, max(1, width-start), true), "\n") {
		prefix := strings.Repeat(" ", start)
		if i == 0 && inline {
			prefix = d.m.style(d.m.colors().dim).Render(label + strings.Repeat(" ", start-ansi.StringWidth(label)))
		}
		d.lines = append(d.lines, prefix+d.m.style(d.m.colors().fg).Render(row))
	}
}

func (d *accountWriter) plan(label string, o object, name, code string) {
	d.kv(label, accountValue(o, name)+" · code "+accountValue(o, code))
}
