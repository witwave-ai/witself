package main

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	xansi "github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/witwave-ai/witself/internal/client"
)

// TestOverlayCenterSplices pins the seam math: dialog centered, every
// composited line exactly terminal width, background intact on both
// sides and untouched above/below.
func TestOverlayCenterSplices(t *testing.T) {
	base := strings.TrimRight(strings.Repeat(strings.Repeat("B", 40)+"\n", 10), "\n")
	dialog := "DDDD\nDD"
	out := overlayCenter(base, dialog, 40)
	lines := strings.Split(out, "\n")
	if len(lines) != 10 {
		t.Fatalf("line count changed: %d", len(lines))
	}
	for i, l := range lines {
		if w := lipgloss.Width(l); w != 40 {
			t.Fatalf("line %d width = %d, want 40: %q", i, w, l)
		}
	}
	// Dialog block: 4 wide, 2 tall, centered at x=18, y=4.
	row := xansi.Strip(lines[4])
	if row[:18] != strings.Repeat("B", 18) || row[18:22] != "DDDD" || row[22:] != strings.Repeat("B", 18) {
		t.Fatalf("seam row 4: %q", row)
	}
	// Short dialog line padded to the block width — solid rectangle.
	row = xansi.Strip(lines[5])
	if row[18:22] != "DD  " {
		t.Fatalf("dialog line not padded: %q", row)
	}
	// Rows outside the dialog untouched.
	if xansi.Strip(lines[0]) != strings.Repeat("B", 40) {
		t.Fatalf("row above dialog modified: %q", lines[0])
	}
}

// TestOverlayCenterStyledAndWide pins the two tear risks: a styled
// background run cut at the seam must not bleed into the dialog, and
// wide runes cut mid-character must not shift the dialog's edges.
func TestOverlayCenterStyledAndWide(t *testing.T) {
	old := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI)
	defer lipgloss.SetColorProfile(old)

	styled := styErr.Render(strings.Repeat("X", 40))
	wide := strings.Repeat("日", 20) // 40 cells of width-2 runes
	base := styled + "\n" + wide + "\n" + styled + "\n" + wide
	out := overlayCenter(base, "DDDDD\nDDDDD", 40)
	for i, l := range strings.Split(out, "\n") {
		if w := lipgloss.Width(l); w != 40 {
			t.Fatalf("line %d width = %d, want 40: %q", i, w, l)
		}
	}
	// The dialog row over the styled line: an SGR reset must fence the
	// seam before the dialog content starts.
	rows := strings.Split(out, "\n")
	dlgRow := rows[1] // y = (4-2)/2 = 1
	if !strings.Contains(dlgRow, "\x1b[0m") {
		t.Fatalf("no reset fencing the seam: %q", dlgRow)
	}
	if !strings.Contains(xansi.Strip(dlgRow), "DDDDD") {
		t.Fatalf("dialog content missing: %q", dlgRow)
	}
}

// TestDrilldownRendersAsModal pins the headline behavior: on a roomy
// terminal a drill-down floats over the dashboard (both visible), and
// a small terminal falls back to the full-screen page.
func TestDrilldownRendersAsModal(t *testing.T) {
	t0 := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	m := newModel(t.Context(), &adminCLI{bin: "/nonexistent"}, nil)
	m.loading = false
	m.now = func() time.Time { return t0 }
	m.width, m.height = 120, 40
	m.fleetCells = []client.AdminCell{{Name: "aws-sandbox-usw2-dev", AccountCount: 4, Version: "0.0.105"}}
	m.tickets = []client.AdminTicket{mkTicket("tkt_1", "awaiting_admin", t0)}
	e := mkEvent("evt_1", "recovery.requested", t0)
	m.detailEvent = &e
	m.mode = modeEventDetail

	v := m.View()
	if !strings.Contains(v, "event · recovery.requested") {
		t.Fatal("dialog content missing")
	}
	if !strings.Contains(v, "cells ·") || !strings.Contains(v, "events") {
		t.Fatal("dashboard must stay visible behind the dialog")
	}
	// Small terminal: full-screen fallback, no dashboard behind.
	m.width, m.height = 80, 20
	v = m.View()
	if !strings.Contains(v, "event · recovery.requested") {
		t.Fatal("fallback lost the detail content")
	}
	if strings.Contains(v, "cells ·") {
		t.Fatal("small terminal must use the full-screen page, not a modal")
	}
}

// TestThreadModalWithComposer pins the thread dialog: conversation and
// reply composer float together over the live dashboard.
func TestThreadModalWithComposer(t *testing.T) {
	t0 := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	m := newModel(t.Context(), &adminCLI{bin: "/nonexistent"}, nil)
	m.loading = false
	m.now = func() time.Time { return t0 }
	m.fleetCells = []client.AdminCell{{Name: "aws-sandbox-usw2-dev", AccountCount: 4, Version: "0.0.105"}}
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = next.(model)
	m.thread = &client.GetSupportTicketResult{Ticket: client.SupportTicket{
		ID: "tkt_dlg", Subject: "modal check", State: "awaiting_admin",
		OpenedAt: t0, LastActivityAt: t0,
	}}
	m.mode = modeCompose
	m.composer.Focus()

	v := m.View()
	if !strings.Contains(v, "ticket · tkt_dlg") {
		t.Fatal("thread dialog title missing")
	}
	if !strings.Contains(v, "Reply as fleet admin") {
		t.Fatal("composer must render inside the dialog")
	}
	if !strings.Contains(v, "cells ·") {
		t.Fatal("dashboard must stay visible behind the composer dialog")
	}
}

// TestModalClipsTallDetail pins the height budget: an event with a
// huge metadata payload must clip inside the dialog, not overflow the
// screen.
func TestModalClipsTallDetail(t *testing.T) {
	t0 := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	m := newModel(t.Context(), &adminCLI{bin: "/nonexistent"}, nil)
	m.loading = false
	m.now = func() time.Time { return t0 }
	m.width, m.height = 120, 24 // smallest modal-eligible height
	e := mkEvent("evt_big", "account.created", t0)
	big := `{"a":1` + strings.Repeat(`,"k":"vvvvvvvv"`, 80) + `}`
	e.Metadata = []byte(big)
	m.detailEvent = &e
	m.mode = modeEventDetail

	v := m.View()
	if got := len(strings.Split(v, "\n")); got > m.height {
		t.Fatalf("modal view renders %d lines on a %d-row terminal", got, m.height)
	}
	if !strings.Contains(v, "more lines") {
		t.Fatal("clipped dialog must say content was cut")
	}
}

// TestOverlayWideRuneStraddlesRightSeam pins the right-seam fix: a
// width-2 rune bisected by the dialog's right edge must not make the
// row a cell too wide (the terminal would chop the pane's right
// border down the whole dialog band).
func TestOverlayWideRuneStraddlesRightSeam(t *testing.T) {
	// termW=40, dialog 5 wide → x=17, right seam at cell 22. "x" then
	// 日 runs put a rune across cells 21-22 — the straddle parity.
	wide := "x" + strings.Repeat("日", 19) + " " // 40 cells
	base := strings.Repeat(wide+"\n", 4)
	base = strings.TrimRight(base, "\n")
	out := overlayCenter(base, "DDDDD\nDDDDD", 40)
	for i, l := range strings.Split(out, "\n") {
		if w := lipgloss.Width(l); w != 40 {
			t.Fatalf("line %d width = %d, want exactly 40: %q", i, w, l)
		}
	}
	// The spliced rows carry the dialog, with the half-covered rune
	// blanked — never a >termW row for the terminal to chop.
	if !strings.Contains(xansi.Strip(strings.Split(out, "\n")[1]), "DDDDD") {
		t.Fatal("dialog content missing from the straddle row")
	}
}

// TestModalFrameFitsWithBadgeAndStatus pins the one-line footer: with
// the upgrade light lit AND a status message set, the modal frame must
// still fit the terminal (a second footer row would shear the top
// border).
func TestModalFrameFitsWithBadgeAndStatus(t *testing.T) {
	t0 := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	m := newModel(t.Context(), &adminCLI{bin: "/nonexistent"}, nil)
	m.loading = false
	m.now = func() time.Time { return t0 }
	m.width, m.height = 90, 24 // smallest modal-eligible terminal
	m.upgradePhase, m.upgradeTag = "ready", "v9.9.9"
	m.status = "3 tickets · 1/1 cells ok"
	e := mkEvent("evt_1", "account.created", t0)
	m.detailEvent = &e
	m.mode = modeEventDetail

	v := m.View()
	if got := len(strings.Split(v, "\n")); got > m.height {
		t.Fatalf("modal frame is %d lines on a %d-row terminal", got, m.height)
	}
	if !strings.Contains(v, "v9.9.9") || !strings.Contains(v, "3 tickets") {
		t.Fatal("badge and status must both survive on the one-line footer")
	}
}

// TestComposeFitsSmallTerminal pins the full-screen fallback budget:
// opening the composer on a small terminal must not push the ticket
// head off the top of the screen.
func TestComposeFitsSmallTerminal(t *testing.T) {
	t0 := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	m := newModel(t.Context(), &adminCLI{bin: "/nonexistent"}, nil)
	m.loading = false
	m.now = func() time.Time { return t0 }
	next, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 20}) // below modal threshold
	m = next.(model)
	m.thread = &client.GetSupportTicketResult{Ticket: client.SupportTicket{
		ID: "tkt_small", Subject: "narrow check", State: "awaiting_admin",
		OpenedAt: t0, LastActivityAt: t0,
	}}
	m.threadView.SetContent(strings.Repeat("message line\n", 40))
	m.mode = modeDetail
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	m = next.(model)
	if m.mode != modeCompose {
		t.Fatalf("mode = %v", m.mode)
	}
	v := m.View()
	if got := len(strings.Split(v, "\n")); got > m.height {
		t.Fatalf("compose view is %d lines on a %d-row terminal — head shears off", got, m.height)
	}
	if !strings.Contains(v, "narrow check") {
		t.Fatal("ticket head must stay visible while composing")
	}
}
