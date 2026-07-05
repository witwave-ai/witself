package main

// Modal drill-downs: on terminals with room for it, the event / cell /
// ticket drill-downs float as a centered dialog over the live
// dashboard instead of replacing the whole screen — the tail keeps
// streaming at the edges, and esc reads as closing a window rather
// than navigating back a page. Small terminals keep the full-screen
// fallback.

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	xansi "github.com/charmbracelet/x/ansi"
)

// modalEligible: a dialog only floats when the terminal leaves enough
// of the dashboard readable around it.
func (m model) modalEligible() bool {
	return m.width >= 90 && m.height >= 24
}

// detailFrameW is the outer width of a drill-down frame: ~4/5 of the
// terminal as a dialog, the whole terminal as a full-screen page.
func (m model) detailFrameW() int {
	w := m.width
	if w < 60 {
		w = 100
	}
	if m.modalEligible() {
		return minInt(w*4/5, 110)
	}
	return w
}

// detailMaxContent is the height budget for a drill-down's content
// lines; clipLines cuts to it.
func (m model) detailMaxContent() int {
	h := m.height
	if h < 15 {
		h = 30
	}
	if m.modalEligible() {
		return maxInt(h*4/5-4, 8)
	}
	return maxInt(h-6, 8)
}

// clipLines caps a frame's content, saying what was cut — bubbletea's
// altscreen trims an over-tall frame from the TOP, shearing the title
// and border, so the cut must happen here instead.
func clipLines(lines []string, maxH, width int) []string {
	if len(lines) <= maxH {
		return lines
	}
	dropped := len(lines) - maxH + 1
	return append(lines[:maxH-1],
		styDim.Render(fitLine(fmt.Sprintf("… %d more lines — enlarge the terminal", dropped), width)))
}

// overlayCenter composites a dialog block over the center of a
// background frame, splicing each background line around the dialog.
// ansi.Cut keeps escape sequences intact and treats the text as
// graphemes, so styled panes and wide runes don't tear at the seams;
// explicit SGR resets fence both seams so a styled background run
// can't bleed into the dialog (or the dialog's trailing style into
// the background's right half). Every dialog line is padded to the
// block's width so the dialog reads as a solid rectangle.
func overlayCenter(base, dialog string, termW int) string {
	baseLines := strings.Split(base, "\n")
	dlgLines := strings.Split(dialog, "\n")
	dw := 0
	for _, l := range dlgLines {
		dw = maxInt(dw, lipgloss.Width(l))
	}
	for i, l := range dlgLines {
		if pad := dw - lipgloss.Width(l); pad > 0 {
			dlgLines[i] = l + strings.Repeat(" ", pad)
		}
	}
	x := maxInt((termW-dw)/2, 0)
	y := maxInt((len(baseLines)-len(dlgLines))/2, 0)
	const reset = "\x1b[0m"
	for i, dl := range dlgLines {
		bi := y + i
		if bi >= len(baseLines) {
			break
		}
		bg := baseLines[bi]
		if pad := termW - lipgloss.Width(bg); pad > 0 {
			bg += strings.Repeat(" ", pad)
		}
		left := xansi.Cut(bg, 0, x)
		// Cutting through a wide rune can come up a cell short — repad
		// so the dialog's left edge stays a straight vertical line.
		if pad := x - lipgloss.Width(left); pad > 0 {
			left += strings.Repeat(" ", pad)
		}
		right := xansi.Cut(bg, x+dw, termW)
		// The right seam has the OPPOSITE failure: Cut keeps a wide
		// grapheme straddling the boundary whole, making the segment a
		// cell (or more) too wide — the row would exceed termW and the
		// terminal would chop the pane's right border. Replace the
		// half-covered grapheme with spaces and re-cut past it.
		if over := lipgloss.Width(right) - (termW - x - dw); over > 0 {
			right = strings.Repeat(" ", over) + xansi.Cut(bg, x+dw+over, termW)
		}
		line := left + reset + dl + reset + right
		if pad := termW - lipgloss.Width(line); pad > 0 {
			line += strings.Repeat(" ", pad)
		}
		baseLines[bi] = line
	}
	return strings.Join(baseLines, "\n")
}

// modalDialog renders the active drill-down as a floating dialog
// block ("" when the current mode isn't a drill-down or the terminal
// is too small for one). The dashboard keeps rendering, live, behind
// it.
func (m model) modalDialog() string {
	if !m.modalEligible() {
		return ""
	}
	switch m.mode {
	case modeEventDetail:
		return m.viewEventDetail()
	case modeCellDetail:
		return m.viewCellDetail()
	case modeTicketDetail:
		return m.viewTicketDetail()
	case modeDetail, modeCompose:
		return m.threadDialog()
	}
	return ""
}

// threadDialog frames the ticket thread — and the composer, when open
// — as a dialog. The viewport and composer widths were already sized
// to the dialog frame on resize.
func (m model) threadDialog() string {
	contentW := m.detailFrameW() - 4
	body := m.viewDetail()
	hint := "r reply · i inspect · R resolve · C close · O reopen · esc close · q quit"
	if m.mode == modeCompose {
		body += "\n" + styTitle.Render("Reply as fleet admin") + "\n" + m.composer.View()
		hint = "ctrl+d send · esc cancel"
	}
	var lines []string
	for _, ln := range strings.Split(body, "\n") {
		lines = append(lines, fitLine(ln, contentW))
	}
	lines = append(lines, "", styDim.Render(fitLine(hint, contentW)))
	title := "ticket"
	if m.thread != nil {
		title = "ticket · " + oneLine(m.thread.Ticket.ID)
	}
	return paneBox(title, lines, contentW, len(lines), true)
}
