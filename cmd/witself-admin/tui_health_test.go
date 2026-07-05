package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/witwave-ai/witself/internal/client"
)

func TestSparkline(t *testing.T) {
	if got := sparkline(nil, 8); got != "" {
		t.Errorf("empty input: %q", got)
	}
	if got := sparkline([]int{1, 2, 3}, 0); got != "" {
		t.Errorf("zero width: %q", got)
	}
	// Full ramp maps onto the full rune ladder.
	if got := sparkline([]int{0, 1, 2, 3, 4, 5, 6, 7}, 8); got != "▁▂▃▄▅▆▇█" {
		t.Errorf("ramp: %q", got)
	}
	// Flat data renders as a low steady line, not a div-by-zero.
	if got := sparkline([]int{5, 5, 5}, 8); got != "▁▁▁" {
		t.Errorf("flat: %q", got)
	}
	// Only the newest `width` points are kept.
	got := sparkline([]int{9, 9, 9, 9, 0, 0, 4, 8}, 4)
	if len([]rune(got)) != 4 || got != "▁▁▄█" {
		t.Errorf("window: %q", got)
	}
}

func TestBarGauge(t *testing.T) {
	if got := barGauge(0, 10, 10); got != "░░░░░░░░░░" {
		t.Errorf("zero: %q", got)
	}
	if got := barGauge(10, 10, 10); got != "██████████" {
		t.Errorf("full: %q", got)
	}
	// Tiny-but-present is distinguishable from zero.
	if got := barGauge(1, 1000, 10); got != "█░░░░░░░░░" {
		t.Errorf("min fill: %q", got)
	}
	// Overflow clamps instead of shearing the layout.
	if got := barGauge(20, 10, 10); got != "██████████" {
		t.Errorf("overflow: %q", got)
	}
	if got := barGauge(3, 0, 4); len([]rune(got)) != 4 {
		t.Errorf("max<1 must still render fixed width: %q", got)
	}
}

func TestEventRates(t *testing.T) {
	t0 := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	samples := []healthSample{
		{at: t0, events: 0},
		{at: t0.Add(time.Minute), events: 5},
		{at: t0.Add(3 * time.Minute), events: 9}, // 4 events over 2 min
		{at: t0.Add(4 * time.Minute), events: 3}, // counter shrank — never negative
	}
	got := eventRates(samples)
	if len(got) != 3 || got[0] != 5 || got[1] != 2 || got[2] != 0 {
		t.Fatalf("rates = %v", got)
	}
}

func TestAgeBuckets(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	mk := func(state string, age time.Duration) client.AdminTicket {
		tk := mkTicket("tkt", state, now)
		tk.OpenedAt = now.Add(-age)
		return tk
	}
	got := ageBuckets([]client.AdminTicket{
		mk("open", 30*time.Minute),
		mk("awaiting_admin", 5*time.Hour),
		mk("awaiting_customer", 40*time.Hour),
		mk("open", 100*time.Hour),
		mk("resolved", 200*time.Hour), // terminal — not counted
		mk("closed", time.Minute),     // terminal — not counted
	}, now)
	if got != [4]int{1, 1, 1, 1} {
		t.Fatalf("buckets = %v", got)
	}
}

func TestVersionSummary(t *testing.T) {
	cell := func(v string) client.AdminCell { return client.AdminCell{Name: "c", Version: v} }
	if got := versionSummary(nil); got != "no cells reported" {
		t.Errorf("empty: %q", got)
	}
	if got := versionSummary([]client.AdminCell{cell("0.0.98"), cell("0.0.98")}); got != "v0.0.98 ×2 — no skew" {
		t.Errorf("no skew: %q", got)
	}
	got := versionSummary([]client.AdminCell{cell("0.0.98"), cell("0.0.97")})
	if !strings.Contains(got, "version skew") {
		t.Errorf("skew: %q", got)
	}
	got = versionSummary([]client.AdminCell{cell("0.0.98"), cell("")})
	if !strings.Contains(got, "1 unreachable") {
		t.Errorf("unreachable: %q", got)
	}
	if got := versionSummary([]client.AdminCell{cell("")}); !strings.Contains(got, "all cells unreachable") {
		t.Errorf("all unreachable: %q", got)
	}
}

// TestHealthSampling pins the sampling contract: one sample lands per
// cells refresh, carrying fleet account totals, open-ticket counts,
// and the cumulative live-event counter; the cap holds.
func TestHealthSampling(t *testing.T) {
	m := newModel(t.Context(), &adminCLI{bin: "/nonexistent"}, nil)
	m.loading = false // first ticket load has landed — sampling armed
	t0 := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return t0 }
	m.tickets = []client.AdminTicket{
		mkTicket("tkt_1", "awaiting_admin", t0),
		mkTicket("tkt_2", "resolved", t0),
	}
	m.eventsSeen = 7
	next, _ := m.Update(cellsLoadedMsg{cells: []client.AdminCell{
		{Name: "c1", AccountCount: 5},
		{Name: "c2", AccountCount: 7},
	}})
	m2 := next.(model)
	if len(m2.samples) != 1 {
		t.Fatalf("samples = %d", len(m2.samples))
	}
	s := m2.samples[0]
	if s.accounts != 12 || s.open != 1 || s.events != 7 || !s.at.Equal(t0) {
		t.Fatalf("sample = %+v", s)
	}
	// A live event bumps the counter only when it's genuinely new.
	e := client.AdminEvent{ID: "ev_1", Verb: "account.created", OccurredAt: t0}
	next, _ = m2.Update(watchEventMsg{event: e})
	m3 := next.(model)
	next, _ = m3.Update(watchEventMsg{event: e}) // duplicate — dropped
	m4 := next.(model)
	if m4.eventsSeen != 8 {
		t.Fatalf("eventsSeen = %d, want 8", m4.eventsSeen)
	}
	// Cap: history never grows past sampleCap.
	for i := 0; i < sampleCap+10; i++ {
		m4.samples = appendSample(m4.samples, healthSample{at: t0})
	}
	if len(m4.samples) != sampleCap {
		t.Fatalf("cap: %d", len(m4.samples))
	}
}

// TestFleetStripRendersAndHides pins the strip's auto-hide: present on
// a normal terminal, gone on a short one where the working panes need
// the rows.
func TestFleetStripRendersAndHides(t *testing.T) {
	m := newModel(t.Context(), &adminCLI{bin: "/nonexistent"}, nil)
	m.loading = false
	t0 := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return t0 }
	m.fleetCells = []client.AdminCell{{Name: "c1", AccountCount: 5, Version: "0.0.98"}}
	m.width, m.height = 100, 40
	if v := m.viewList(); !strings.Contains(v, "accounts 5") {
		t.Fatal("tall terminal must show the fleet strip")
	}
	m.height = 18
	if v := m.viewList(); strings.Contains(v, "accounts 5") {
		t.Fatal("short terminal must hide the fleet strip")
	}
	// The cells pane gauge survives either way.
	if v := m.viewList(); !strings.Contains(v, "█") {
		t.Fatal("cells pane must render the load gauge")
	}
}

// TestHealthDrilldown pins the H view: enter from the list, all five
// sections render, esc returns.
func TestHealthDrilldown(t *testing.T) {
	m := newModel(t.Context(), &adminCLI{bin: "/nonexistent"}, nil)
	m.loading = false
	t0 := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return t0 }
	m.width, m.height = 100, 40
	m.fleetCells = []client.AdminCell{
		{Name: "aws-sandbox-usw2-dev", AccountCount: 12, ArchivedCount: 2, Version: "0.0.98"},
	}
	tk := mkTicket("tkt_1", "awaiting_admin", t0)
	tk.OpenedAt = t0.Add(-100 * time.Hour) // deep in the >3d bucket
	m.tickets = []client.AdminTicket{tk}
	m.samples = []healthSample{
		{at: t0.Add(-time.Minute), accounts: 10, open: 1, events: 0},
		{at: t0, accounts: 12, open: 1, events: 6},
	}

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("H")})
	m2 := next.(model)
	if m2.mode != modeHealth {
		t.Fatalf("mode = %v", m2.mode)
	}
	v := m2.View()
	for _, want := range []string{
		"fleet health", "accounts by cell", "aws-sandbox-usw2-dev", "2 archived",
		"open ticket age", "⚠ aging",
		"event rate", "now 6/m",
		"+2 since open",
		"software", "no skew",
	} {
		if !strings.Contains(v, want) {
			t.Errorf("health view missing %q", want)
		}
	}
	next, _ = m2.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m3 := next.(model); m3.mode != modeList {
		t.Fatalf("esc: mode = %v", m3.mode)
	}
}

// TestSupportClipKeepsCursorVisible pins the separator-clipping fix: a
// stage boundary inflates the rendered line count past the frame, and
// the clip must sacrifice the BOTTOM rows, never the cursor's row at
// the top. (Before the fix, paneBox trimmed from the top and the
// selection bar vanished while keys still acted on the invisible row.)
func TestSupportClipKeepsCursorVisible(t *testing.T) {
	m := newModel(t.Context(), &adminCLI{bin: "/nonexistent"}, nil)
	m.loading = false
	t0 := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return t0 }
	m.width, m.height = 120, 18 // short: strip hidden, few support rows
	// Two stages → one separator line between the groups.
	m.tickets = []client.AdminTicket{}
	for i := 0; i < 6; i++ {
		m.tickets = append(m.tickets, mkTicket(fmt.Sprintf("tkt_adm_%d", i), "awaiting_admin", t0.Add(-time.Duration(i)*time.Minute)))
	}
	for i := 0; i < 6; i++ {
		m.tickets = append(m.tickets, mkTicket(fmt.Sprintf("tkt_res_%d", i), "resolved", t0.Add(-time.Duration(i)*time.Minute)))
	}
	sortTickets(m.tickets)
	m.cursor = 0
	m.focus = paneSupport
	if v := m.viewList(); !strings.Contains(v, "tkt_adm_0") {
		t.Fatal("cursor row at the top must stay visible when separators overflow the frame")
	}
}

// TestActionTargetIsWhatYouSee pins the R/C/O targeting rule: state
// keys act on the ticket ON SCREEN, or nothing. A stale thread from an
// earlier drill-down must never be mutated from the health, event, or
// cell views.
func TestActionTargetIsWhatYouSee(t *testing.T) {
	m := newModel(t.Context(), &adminCLI{bin: "/nonexistent"}, nil)
	t0 := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	m.tickets = []client.AdminTicket{mkTicket("tkt_row", "awaiting_admin", t0)}
	// Simulate an earlier thread drill-down that left coordinates behind.
	m.threadAccount, m.threadTicket = "acc_stale", "tkt_stale"

	m.mode = modeHealth
	if acct, _ := m.actionTarget(); acct != "" {
		t.Fatalf("health view must have no action target, got %q", acct)
	}
	m.mode = modeEventDetail
	if acct, _ := m.actionTarget(); acct != "" {
		t.Fatalf("event detail must have no action target, got %q", acct)
	}
	m.mode = modeDetail
	if acct, tkt := m.actionTarget(); acct != "acc_stale" || tkt != "tkt_stale" {
		t.Fatalf("detail must target the open thread, got %s/%s", acct, tkt)
	}
	// The record inspector targets the INSPECTED ticket, not the stale
	// thread.
	insp := mkTicket("tkt_inspected", "open", t0)
	m.mode = modeTicketDetail
	m.detailTicket = &insp
	if _, tkt := m.actionTarget(); tkt != "tkt_inspected" {
		t.Fatalf("inspector must target the inspected ticket, got %q", tkt)
	}
	m.mode = modeList
	m.focus = paneSupport
	if _, tkt := m.actionTarget(); tkt != "tkt_row" {
		t.Fatalf("list must target the selected row, got %q", tkt)
	}
}

// TestHealthViewStaysPut pins the review findings around modeHealth vs
// the stale-thread machinery: background ticket activity must not yank
// the operator out of the health view, and an upgrade snapshot taken
// there must restore to health — with the resume consumed — even when
// an earlier drill-down left thread coordinates behind.
func TestHealthViewStaysPut(t *testing.T) {
	m := newModel(t.Context(), &adminCLI{bin: "/nonexistent"}, nil)
	m.loading = false
	t0 := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return t0 }
	// An earlier drill-down left stale thread coordinates behind.
	m.threadAccount, m.threadTicket = "acc_stale", "tkt_stale"
	m.mode = modeHealth

	// Live activity on the stale ticket: no thread reload, no mode flip.
	next, cmd := m.Update(watchTicketMsg{ticket: mkTicket("tkt_stale", "awaiting_admin", t0)})
	m2 := next.(model)
	if m2.mode != modeHealth {
		t.Fatalf("watch activity must not leave health view: mode=%v", m2.mode)
	}
	if cmd != nil {
		if msg := cmd(); msg != nil {
			if _, isLoad := msg.(threadLoadedMsg); isLoad {
				t.Fatal("watch activity in health view must not reload the stale thread")
			}
		}
	}

	// Upgrade snapshot from health: no thread coordinates ride along.
	r := m2.snapshotView("v9.9.9")
	if r.Mode != "health" || r.ThreadAccount != "" || r.ThreadTicket != "" {
		t.Fatalf("health snapshot = %+v, must not carry stale thread coords", r)
	}
	// Restore: back in health, upgraded status shown, resume CONSUMED.
	fresh := newModel(t.Context(), &adminCLI{bin: "/nonexistent"}, nil).withResume(r)
	if fresh.mode != modeHealth {
		t.Fatalf("restore mode = %v", fresh.mode)
	}
	if fresh.resume != nil {
		t.Fatal("health resume must be consumed at restore — a dangling one corrupts a later thread load")
	}
	if !strings.Contains(fresh.status, "v9.9.9") {
		t.Fatalf("status = %q, want upgraded stamp", fresh.status)
	}
}

// TestCellsClipKeepsSelectionVisible pins the cells-pane clip fix: with
// the fleet strip eating 4 rows, a fleet taller than the window must
// still show the selected cell's highlighted row — at the top of the
// list, not just the bottom.
func TestCellsClipKeepsSelectionVisible(t *testing.T) {
	m := newModel(t.Context(), &adminCLI{bin: "/nonexistent"}, nil)
	m.loading = false
	t0 := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return t0 }
	m.width, m.height = 100, 30
	for i := 0; i < 5; i++ {
		m.fleetCells = append(m.fleetCells, client.AdminCell{
			Name: fmt.Sprintf("cell-%d", i), AccountCount: i + 1, Version: "0.0.98",
		})
	}
	m.focus = paneCells
	m.cellCursor = 0
	if v := m.viewList(); !strings.Contains(v, "cell-0") {
		t.Fatal("selected cell at the TOP of an overflowing fleet must stay visible")
	}
	m.cellCursor = 4
	if v := m.viewList(); !strings.Contains(v, "cell-4") {
		t.Fatal("selected cell at the BOTTOM of an overflowing fleet must stay visible")
	}
}

// TestHealthViewFitsTerminal pins the height budget: the H view clips
// its content to the terminal instead of overflowing (bubbletea's
// altscreen would shear the TOP of an over-tall frame).
func TestHealthViewFitsTerminal(t *testing.T) {
	m := newModel(t.Context(), &adminCLI{bin: "/nonexistent"}, nil)
	m.loading = false
	t0 := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return t0 }
	m.width, m.height = 100, 18
	for i := 0; i < 12; i++ {
		m.fleetCells = append(m.fleetCells, client.AdminCell{
			Name: fmt.Sprintf("cell-%d", i), AccountCount: i + 1, Version: "0.0.98",
		})
	}
	m.mode = modeHealth
	v := m.viewHealth()
	if got := len(strings.Split(v, "\n")); got > m.height-2 {
		t.Fatalf("health view renders %d lines on an %d-row terminal", got, m.height)
	}
	if !strings.Contains(v, "more lines") {
		t.Fatal("clipped health view must say content was cut")
	}
	if !strings.Contains(v, "fleet health") {
		t.Fatal("title must survive the clip")
	}
}

// TestNoSampleBeforeFirstTicketLoad pins the fabricated-surge fix: a
// cells load that lands before the first ticket load must not record
// open=0 into the history.
func TestNoSampleBeforeFirstTicketLoad(t *testing.T) {
	m := newModel(t.Context(), &adminCLI{bin: "/nonexistent"}, nil) // loading=true
	t0 := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return t0 }
	next, _ := m.Update(cellsLoadedMsg{cells: []client.AdminCell{{Name: "c1", AccountCount: 5}}})
	if m2 := next.(model); len(m2.samples) != 0 {
		t.Fatalf("sampled during initial load: %d samples (open=0 would fake a surge)", len(m2.samples))
	}
}

// TestEventsSeenPastCap pins the rate-counter fix: the counter keeps
// counting genuinely new events after the tail hits its cap.
func TestEventsSeenPastCap(t *testing.T) {
	m := newModel(t.Context(), &adminCLI{bin: "/nonexistent"}, nil)
	t0 := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	total := eventTailCap + 50
	var mdl tea.Model = m
	for i := 0; i < total; i++ {
		mdl, _ = mdl.(model).Update(watchEventMsg{event: client.AdminEvent{
			ID: fmt.Sprintf("ev_%d", i), Verb: "account.created", OccurredAt: t0,
		}})
	}
	if got := mdl.(model).eventsSeen; got != total {
		t.Fatalf("eventsSeen = %d, want %d (froze at the tail cap)", got, total)
	}
}
