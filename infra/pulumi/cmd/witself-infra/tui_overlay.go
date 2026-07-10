package main

// Modal dialogs float centered over the dashboard so the base frame
// keeps its shape. Stacking the dialog under the frame overflowed the
// terminal and bubbletea's altscreen sheared the top border to fit —
// port of the well-tested overlay from witself-admin.

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	xansi "github.com/charmbracelet/x/ansi"
)

// overlayCenter composites a dialog block over the center of a
// background frame. ansi.Cut preserves escape sequences and treats
// text as graphemes, so styled panes and wide runes don't tear at the
// seams; explicit SGR resets fence both seams so a styled background
// run can't bleed into the dialog (or the dialog's trailing style into
// the background's right half). Every dialog line is padded to the
// block's width so the dialog reads as a solid rectangle.
func overlayCenter(base, dialog string, termW int) string {
	baseLines := strings.Split(base, "\n")
	dlgLines := strings.Split(dialog, "\n")
	dw := 0
	for _, l := range dlgLines {
		dw = max(dw, lipgloss.Width(l))
	}
	for i, l := range dlgLines {
		if pad := dw - lipgloss.Width(l); pad > 0 {
			dlgLines[i] = l + strings.Repeat(" ", pad)
		}
	}
	x := max((termW-dw)/2, 0)
	y := max((len(baseLines)-len(dlgLines))/2, 0)
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
		// The right seam has the opposite failure: Cut keeps a wide
		// grapheme straddling the boundary whole, making the segment a
		// cell too wide — the row would exceed termW and the terminal
		// would chop the pane's right border. Replace the half-covered
		// grapheme with spaces and re-cut past it.
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
