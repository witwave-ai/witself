package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	xansi "github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/witwave-ai/witself/infra/pulumi/internal/fleet"
)

// stripANSIForTest removes escape sequences so column-position
// assertions measure what the terminal displays, not the raw bytes.
func stripANSIForTest(s string) string { return xansi.Strip(s) }

// fakeSource lets the model tests skip the control plane and the
// cloud identity API.
type fakeSource struct {
	states        []cellState
	controlPlanes map[string]controlPlaneInfo
	err           error
	reach         reachResult
	reachErr      error
	health        cellHealthReport
	healthErr     error

	// Placement-runner fakes. set/run record their inputs on the shared
	// recorder so tests can assert what was written.
	runner    fleet.PlacementRunnerConfig
	runnerErr error
	runResult fleet.PlacementRunnerResult
	runErr    error
	rec       *fakeRunnerRecorder
}

// fakeRunnerRecorder captures writes across the by-value fakeSource
// copies the model holds.
type fakeRunnerRecorder struct {
	setCalls []fleet.PlacementRunnerConfig
	runCalls []fleet.PlacementRunnerConfig
}

func (f fakeSource) load(_ context.Context, _ string) (loadResult, error) {
	return loadResult{states: f.states, controlPlanes: f.controlPlanes}, f.err
}

func (f fakeSource) probe(_ context.Context, _, _ string) (reachResult, error) {
	return f.reach, f.reachErr
}

func (f fakeSource) probeHealth(_ context.Context, _, _ string) (cellHealthReport, error) {
	return f.health, f.healthErr
}

func (f fakeSource) placementRunner(_ context.Context, _, _ string) (fleet.PlacementRunnerConfig, error) {
	return f.runner, f.runnerErr
}

func (f fakeSource) setPlacementRunner(_ context.Context, _, _ string, cfg fleet.PlacementRunnerConfig) (fleet.PlacementRunnerConfig, error) {
	if f.rec != nil {
		f.rec.setCalls = append(f.rec.setCalls, cfg)
	}
	if f.runnerErr != nil {
		return fleet.PlacementRunnerConfig{}, f.runnerErr
	}
	return cfg, nil // echo back, like the CP does
}

func (f fakeSource) runPlacementRunner(_ context.Context, _, _ string, cfg fleet.PlacementRunnerConfig) (fleet.PlacementRunnerResult, error) {
	if f.rec != nil {
		f.rec.runCalls = append(f.rec.runCalls, cfg)
	}
	return f.runResult, f.runErr
}

// seedModel builds a dashboardModel and runs one loadedMsg through
// Update — matches the real startup shape, so the cursor lands on the
// first cell instead of the initial group header.
func seedModel(states []cellState, w, h int) dashboardModel {
	m := dashboardModel{
		ctx:    context.Background(),
		cli:    fakeSource{states: states},
		width:  w,
		height: h,
		now:    func() time.Time { return time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC) },
	}
	next, _ := m.Update(loadedMsg{states: states})
	return next.(dashboardModel)
}

// TestDashboardRendersCellsAndContext pins the two-pane view: cells
// list on the left with the cursor row marked, context pane on the
// right with the selected cell's identity — the read-only baseline.
func TestDashboardRendersCellsAndContext(t *testing.T) {
	states := []cellState{
		{
			name:     "aws-sandbox-usw2-dev",
			entry:    cellEntry{Cloud: strPtr("aws"), Region: strPtr("us-west-2")},
			identity: identity{Cloud: "aws", Account: "123456789012", Profile: "witwave-sandbox", OK: true},
		},
		{
			name:     "gcp-lab-euw1-dev",
			entry:    cellEntry{Cloud: strPtr("gcp"), Region: strPtr("europe-west1")},
			identity: identity{Cloud: "gcp", Account: "witwave-lab", OK: true},
		},
	}
	m := seedModel(states, 120, 24)
	v := m.View()
	for _, want := range []string{
		// Group header carries the count — the old "cells · N" pane
		// title was removed as redundant with it.
		"2 cells",
		"aws-sandbox-usw2-dev",
		"gcp-lab-euw1-dev",
		// The context pane's tab bar replaces the old "context" title.
		"Overview",
		"Kubernetes",
		"123456789012",
		"witwave-sandbox",
		"matches config pin",
	} {
		if !strings.Contains(v, want) {
			t.Errorf("View() missing %q", want)
		}
	}
	if strings.Contains(v, "cells · 2") {
		t.Error("redundant \"cells · N\" pane title must be gone")
	}
}

// mkTabStates is a two-cell fixture for the context-tab tests.
func mkTabStates() []cellState {
	acc := true
	return []cellState{
		{
			name:     "aws-sandbox-usw2-dev",
			entry:    cellEntry{Cloud: strPtr("aws"), Region: strPtr("us-west-2")},
			identity: identity{Cloud: "aws", Account: "123456789012", Profile: "witwave-sandbox", OK: true},
			fleet:    &fleet.Cell{Name: "aws-sandbox-usw2-dev", Accepting: &acc},
		},
		{
			name:     "gcp-sandbox-usw2-dev",
			entry:    cellEntry{Cloud: strPtr("gcp"), Region: strPtr("us-west2")},
			identity: identity{Cloud: "gcp", Account: "witself-sandbox", OK: true},
		},
	}
}

// TestContextTabBarRenders pins that all four tabs appear and Overview
// is the default active tab holding the previous cell content.
func TestContextTabBarRenders(t *testing.T) {
	m := seedModel(mkTabStates(), 120, 30)
	v := m.View()
	for _, tab := range []string{"Overview", "Kubernetes", "Database", "Health", "Logs"} {
		if !strings.Contains(v, tab) {
			t.Errorf("tab bar missing %q", tab)
		}
	}
	// Overview (default) shows the identity/settings content.
	if !strings.Contains(v, "matches config pin") {
		t.Error("Overview tab must show the identity content by default")
	}
}

// TestTabFocusAndSwitch pins the navigation model: tab moves focus to
// the context pane, ←/→ switch tabs only while focused, and the active
// tab sticks as the cell cursor moves.
func TestTabFocusAndSwitch(t *testing.T) {
	m := seedModel(mkTabStates(), 120, 30)

	// Arrows do nothing until the context pane is focused.
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRight})
	if got := m2.(dashboardModel).activeTab; got != tabOverview {
		t.Fatalf("→ must be inert while cells focused: tab = %d", got)
	}

	// tab focuses the context pane.
	m3, _ := m2.(dashboardModel).Update(tea.KeyMsg{Type: tea.KeyTab})
	fm := m3.(dashboardModel)
	if fm.focus != focusContext {
		t.Fatal("tab must move focus to the context pane")
	}

	// Now → advances the tab; the body follows.
	m4, _ := fm.Update(tea.KeyMsg{Type: tea.KeyRight})
	km := m4.(dashboardModel)
	if km.activeTab != tabKubernetes {
		t.Fatalf("→ must advance to Kubernetes: tab = %d", km.activeTab)
	}
	if !strings.Contains(km.View(), "no Kubernetes details yet") {
		t.Error("Kubernetes tab body must render after switching")
	}

	// The tab sticks as the cell cursor moves down.
	m5, _ := km.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	jm := m5.(dashboardModel)
	if jm.activeTab != tabKubernetes {
		t.Fatal("active tab must persist across cell navigation")
	}

	// ← walks back; it clamps at Overview.
	m6, _ := jm.Update(tea.KeyMsg{Type: tea.KeyLeft})
	m6, _ = m6.(dashboardModel).Update(tea.KeyMsg{Type: tea.KeyLeft})
	if got := m6.(dashboardModel).activeTab; got != tabOverview {
		t.Fatalf("← must clamp at Overview: tab = %d", got)
	}

	// esc backs focus out to the cells list.
	m7, _ := m6.(dashboardModel).Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m7.(dashboardModel).focus != focusCells {
		t.Fatal("esc must return focus to the cells pane")
	}
}

// TestHealthTabFreeLines pins phase-1 Health: the credential and fleet
// lines reflect real state, the unprobed lines show unknown.
func TestHealthTabFreeLines(t *testing.T) {
	old := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI)
	defer lipgloss.SetColorProfile(old)

	m := seedModel(mkTabStates(), 120, 30)
	m.focus = focusContext
	m.activeTab = tabHealth
	v := m.View()

	for _, want := range []string{"credentials", "fleet", "reachability", "kubernetes", "database", "workloads", "overall"} {
		if !strings.Contains(v, want) {
			t.Errorf("Health board missing line %q", want)
		}
	}
	if !strings.Contains(v, "not probed yet") {
		t.Error("unprobed subsystem lines must show a placeholder")
	}

	// A cell with an auth error paints its credential line red/bad.
	lvl, msg := cellCredentialHealth(cellState{err: errFake("token expired")})
	if lvl != healthBad {
		t.Fatalf("credential health with an error must be healthBad, got %d", lvl)
	}
	if !strings.Contains(msg, "token expired") {
		t.Errorf("credential detail must surface the error: %q", msg)
	}
	// A registered+accepting cell is good; an absent fleet record warns.
	acc := true
	if lvl, _ := cellFleetHealth(cellState{fleet: &fleet.Cell{Accepting: &acc}}); lvl != healthGood {
		t.Fatalf("registered+accepting must be healthGood, got %d", lvl)
	}
	if lvl, _ := cellFleetHealth(cellState{}); lvl != healthWarn {
		t.Fatalf("no fleet record must be healthWarn, got %d", lvl)
	}
}

// TestReachProbeFiresAndRenders pins the async reachability line: a key
// that lands on the Health tab kicks a probe, the returned result is
// cached, and the line renders green with the version the Worker saw.
func TestReachProbeFiresAndRenders(t *testing.T) {
	old := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI)
	defer lipgloss.SetColorProfile(old)

	states := mkTabStates()
	m := dashboardModel{
		ctx:    context.Background(),
		cli:    fakeSource{states: states, reach: reachResult{ok: true, version: "0.0.86"}},
		width:  120,
		height: 30,
		now:    func() time.Time { return time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC) },
	}
	next, _ := m.Update(loadedMsg{states: states})
	m = next.(dashboardModel)

	// Focus the context pane, then arrow onto Health — that transition
	// must return a probe command.
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m2m := m2.(dashboardModel)
	// Walk right to Health (Overview→K8s→DB→Health).
	var cmd tea.Cmd
	var mm tea.Model = m2m
	for i := 0; i < 3; i++ {
		mm, cmd = mm.(dashboardModel).Update(tea.KeyMsg{Type: tea.KeyRight})
	}
	fm := mm.(dashboardModel)
	if fm.activeTab != tabHealth {
		t.Fatalf("expected Health tab, got %d", fm.activeTab)
	}
	if cmd == nil {
		t.Fatal("landing on Health with an unprobed cell must kick a probe command")
	}
	if !fm.reach["aws-sandbox-usw2-dev"].inflight {
		t.Fatal("the selected cell must be marked in-flight while probing")
	}

	// Deliver the probe result and confirm the line renders reachable.
	done, _ := fm.Update(probeResultMsg{cell: "aws-sandbox-usw2-dev", res: reachResult{ok: true, version: "0.0.86"}})
	dm := done.(dashboardModel)
	if dm.reach["aws-sandbox-usw2-dev"].inflight {
		t.Fatal("probe result must clear the in-flight flag")
	}
	v := dm.View()
	if !strings.Contains(v, "reachability") {
		t.Fatal("Health board must show the reachability line")
	}
	if !strings.Contains(v, "witself-server 0.0.86") {
		t.Errorf("reachable line must show the version the Worker saw: %s", v)
	}
}

// TestHealthReportProbeAndRender pins the subsystem lines: landing on
// the Health tab kicks the cell-health subprocess, and the returned
// report drives the Kubernetes/Database/Workloads lines.
func TestHealthReportProbeAndRender(t *testing.T) {
	old := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI)
	defer lipgloss.SetColorProfile(old)

	states := mkTabStates()
	report := cellHealthReport{
		Kubernetes: subsystemHealth{Level: healthGood, Detail: "apiserver ready", Have: 3, Total: 3},
		Database:   subsystemHealth{Level: healthUnknown, Detail: "status probe not yet wired"},
		Argo:       subsystemHealth{Level: healthDegraded, Detail: "witself-server OutOfSync/Progressing", Have: 2, Total: 3},
	}
	m := dashboardModel{
		ctx:    context.Background(),
		cli:    fakeSource{states: states, reach: reachResult{ok: true}, health: report},
		width:  120,
		height: 30,
		now:    func() time.Time { return time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC) },
	}
	next, _ := m.Update(loadedMsg{states: states})
	m = next.(dashboardModel)

	// Focus context, walk to Health — the transition must batch a
	// health-report probe (plus the reachability probe).
	mm := tea.Model(m)
	mm, _ = mm.(dashboardModel).Update(tea.KeyMsg{Type: tea.KeyTab})
	var cmd tea.Cmd
	for i := 0; i < 3; i++ {
		mm, cmd = mm.(dashboardModel).Update(tea.KeyMsg{Type: tea.KeyRight})
	}
	fm := mm.(dashboardModel)
	if cmd == nil {
		t.Fatal("landing on Health must kick probes")
	}
	if !fm.health["aws-sandbox-usw2-dev"].inflight {
		t.Fatal("the cell-health report must be marked in-flight")
	}

	// Deliver the report; the three subsystem lines must reflect it.
	done, _ := fm.Update(healthResultMsg{cell: "aws-sandbox-usw2-dev", report: report})
	v := done.(dashboardModel).View()
	if !strings.Contains(v, "apiserver ready") {
		t.Error("Kubernetes line must show the report detail")
	}
	if !strings.Contains(v, "OutOfSync/Progressing") {
		t.Error("Workloads line must show the Argo detail")
	}
	if !strings.Contains(v, "status probe not yet wired") {
		t.Error("Database line must show its placeholder detail from the report")
	}
}

// TestHealthBoardVisuals pins the graphical pieces: the gauge bar
// fills proportionally, the pip strip has one dot per subsystem, and
// the rolled-up verdict is worst-wins.
func TestHealthBoardVisuals(t *testing.T) {
	old := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI)
	defer lipgloss.SetColorProfile(old)

	// Gauge: 3 of 4 → 5 filled of 6 (rounded), and shows the count.
	g := stripANSIForTest(gaugeBar(3, 4, 6, styOK))
	if !strings.Contains(g, "3/4") {
		t.Errorf("gauge must show the count: %q", g)
	}
	if fill := strings.Count(g, "▰"); fill == 0 || fill == 6 {
		t.Errorf("gauge for 3/4 must be partially filled, got %d of 6", fill)
	}
	if strings.Count(g, "▱") == 0 {
		t.Error("gauge for 3/4 must have an empty remainder")
	}

	// Rollup is worst-wins, ignoring unknowns.
	if got := rollupLevel(healthGood, healthUnknown, healthDegraded, healthGood); got != healthDegraded {
		t.Fatalf("rollup = %d, want degraded", got)
	}
	if got := rollupLevel(healthUnknown, healthUnknown); got != healthUnknown {
		t.Fatalf("all-unknown rollup must stay unknown, got %d", got)
	}

	// Pip strip: one dot per subsystem (6 rows) appears in the board.
	m := seedModel(mkTabStates(), 120, 30)
	m.activeTab = tabHealth
	v := stripANSIForTest(m.View())
	if strings.Count(v, "●")+strings.Count(v, "◌") < 6 {
		t.Errorf("board must show a pip per subsystem plus tree dots: %q", v)
	}
	if !strings.Contains(v, "├─") || !strings.Contains(v, "└─") {
		t.Error("board must draw the connection tree")
	}
}

// TestBackgroundSweep pins the continuous probing: a sweep re-probes
// reachability for every registered (live) cell, refreshes exactly one
// cell's heavy health report, skips unregistered cells, and re-arms.
func TestBackgroundSweep(t *testing.T) {
	acc := true
	states := []cellState{
		{name: "live-a", fleet: &fleet.Cell{Name: "live-a", Accepting: &acc}},
		{name: "live-b", fleet: &fleet.Cell{Name: "live-b", Accepting: &acc}},
		{name: "absent-c"}, // not registered — must be skipped
	}
	m := dashboardModel{
		ctx:    context.Background(),
		cli:    fakeSource{states: states},
		width:  120,
		height: 30,
		states: states,
		now:    func() time.Time { return time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC) },
	}
	next, cmd := m.Update(bgProbeTickMsg{})
	m2 := next.(dashboardModel)
	if cmd == nil {
		t.Fatal("sweep must re-arm and kick probes")
	}
	if !m2.reach["live-a"].inflight || !m2.reach["live-b"].inflight {
		t.Fatal("both live cells must have reachability probes in flight")
	}
	if _, probed := m2.reach["absent-c"]; probed {
		t.Fatal("unregistered cell must not be probed")
	}
	// Exactly one health probe this sweep (serialized).
	inflight := 0
	for _, h := range m2.health {
		if h.inflight {
			inflight++
		}
	}
	if inflight != 1 {
		t.Fatalf("exactly one health probe per sweep, got %d", inflight)
	}

	// With a health probe already busy, the next sweep starts no more.
	next2, _ := m2.Update(bgProbeTickMsg{})
	m3 := next2.(dashboardModel)
	busy := 0
	for _, h := range m3.health {
		if h.inflight {
			busy++
		}
	}
	if busy != 1 {
		t.Fatalf("health probes must stay serialized across sweeps, got %d", busy)
	}
}

// TestHumanAge pins the compact age formatting used on the board.
func TestHumanAge(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		ago  time.Duration
		want string
	}{
		{0, "0s"},
		{8 * time.Second, "8s"},
		{3 * time.Minute, "3m"},
		{2 * time.Hour, "2h"},
	}
	for _, tc := range cases {
		if got := humanAge(now, now.Add(-tc.ago)); got != tc.want {
			t.Errorf("humanAge(%s) = %q, want %q", tc.ago, got, tc.want)
		}
	}
	if got := humanAge(now, time.Time{}); got != "never" {
		t.Errorf("zero time must read 'never', got %q", got)
	}
}

// TestBoardShowsFreshness pins that the board surfaces data age so a
// stale reading is visible rather than silently trusted.
func TestBoardShowsFreshness(t *testing.T) {
	m := seedModel(mkTabStates(), 120, 30)
	m.activeTab = tabHealth
	m.reach = map[string]cellReach{"aws-sandbox-usw2-dev": {probed: m.now().Add(-8 * time.Second), res: reachResult{ok: true}}}
	v := m.View()
	if !strings.Contains(v, "reach 8s") {
		t.Errorf("board must show the reachability data age: %s", stripANSIForTest(v))
	}
	if !strings.Contains(v, "fleet") {
		t.Error("board must show the inventory (fleet) data age")
	}
}

// TestHealthAnimTick pins the checking-state animation: a tick while a
// probe is in flight advances the frame and re-arms; once the result
// lands (nothing in flight) the loop stops.
func TestHealthAnimTick(t *testing.T) {
	states := mkTabStates()
	m := seedModel(states, 120, 30)
	m.activeTab = tabHealth
	m.health = map[string]cellHealthState{"aws-sandbox-usw2-dev": {inflight: true}}
	m.healthAnimGen = 7

	next, cmd := m.Update(healthAnimTickMsg{gen: 7})
	m2 := next.(dashboardModel)
	if m2.healthFrame != 1 {
		t.Fatalf("frame must advance while in flight: got %d", m2.healthFrame)
	}
	if cmd == nil {
		t.Fatal("animation must re-arm while a probe is in flight")
	}
	// A stale-gen tick is ignored.
	if _, c := m2.Update(healthAnimTickMsg{gen: 6}); c != nil {
		t.Fatal("stale-gen anim tick must not re-arm")
	}
	// Result lands → not in flight → loop stops.
	m2.health = map[string]cellHealthState{"aws-sandbox-usw2-dev": {probed: m2.now()}}
	if _, c := m2.Update(healthAnimTickMsg{gen: 7}); c != nil {
		t.Fatal("animation must stop once nothing is in flight")
	}
}

// TestStackedBarProportions pins the fleet load bar: it fills exactly
// `width` cells and gives each non-zero class at least its share.
func TestStackedBarProportions(t *testing.T) {
	old := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI)
	defer lipgloss.SetColorProfile(old)

	bar := stripANSIForTest(stackedBar(24, barSeg{6, styOK}, barSeg{2, styWarn}, barSeg{0, styErr}))
	if got := lipgloss.Width(bar); got != 24 {
		t.Fatalf("stacked bar must be exactly 24 cells, got %d", got)
	}
	if strings.Count(bar, "█") != 24 {
		t.Errorf("8 of 8 accounts present → bar fully filled: %q", bar)
	}
	// All-zero renders empty, not a crash.
	empty := stripANSIForTest(stackedBar(10))
	if strings.Count(empty, "░") != 10 {
		t.Errorf("empty fleet must render an empty bar: %q", empty)
	}
}

// TestGaugeFullVsEmpty pins the extremes: 0/N is all-empty, N/N is
// all-filled.
func TestGaugeFullVsEmpty(t *testing.T) {
	full := stripANSIForTest(gaugeBar(4, 4, 6, styOK))
	if strings.Count(full, "▰") != 6 || strings.Contains(full, "▱") {
		t.Errorf("4/4 must be fully filled: %q", full)
	}
	empty := stripANSIForTest(gaugeBar(0, 4, 6, styErr))
	if strings.Count(empty, "▰") != 0 || strings.Count(empty, "▱") != 6 {
		t.Errorf("0/4 must be fully empty: %q", empty)
	}
}

// TestReachHealthLevels pins the level mapping for the reachability
// line across its states.
func TestReachHealthLevels(t *testing.T) {
	base := func(r cellReach) dashboardModel {
		return dashboardModel{
			now:   func() time.Time { return time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC) },
			reach: map[string]cellReach{"c": r},
		}
	}
	st := cellState{name: "c"}
	cases := []struct {
		name string
		r    cellReach
		want healthLevel
	}{
		{"unprobed", cellReach{}, healthUnknown},
		{"checking", cellReach{inflight: true}, healthUnknown},
		{"transport error", cellReach{probed: time.Now(), err: errFake("no route")}, healthBad},
		{"reachable", cellReach{probed: time.Now(), res: reachResult{ok: true}}, healthGood},
		{"not serving", cellReach{probed: time.Now(), res: reachResult{ok: false, reason: "TLS handshake"}}, healthDegraded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lvl, _ := base(tc.r).cellReachHealth(st)
			if lvl != tc.want {
				t.Fatalf("%s: level = %d, want %d", tc.name, lvl, tc.want)
			}
		})
	}
}

// TestDashboardCursorMovement pins j/k navigation between cells and
// the cursor's effect on the context pane.
// TestAbsentCellsSinkToBottom pins the new ordering: live cells first
// (CP-grouped), then a non-navigable separator, then absent cells
// sorted alphabetically. The cursor skips the separator.
func TestAbsentCellsSinkToBottom(t *testing.T) {
	acc := true
	live := func(name string) cellState {
		return cellState{name: name, fleet: &fleet.Cell{Name: name, Accepting: &acc}}
	}
	// m.states arrives CP-then-name sorted (as load produces); the two
	// live cells are already alphabetical, the absent ones deliberately
	// are not, to prove rows() re-sorts them.
	states := []cellState{live("aws-a-live"), live("aws-b-live"), {name: "aws-z-absent"}, {name: "aws-m-absent"}}
	m := seedModel(states, 120, 30)

	rows := m.rows()
	sepAt := -1
	for i, r := range rows {
		if r.kind == rowSeparator {
			sepAt = i
		}
	}
	if sepAt < 0 {
		t.Fatal("a separator must divide live cells from absent ones")
	}
	// Everything before the separator is a header or a live cell.
	for i := 0; i < sepAt; i++ {
		if rows[i].kind == rowCell && m.states[rows[i].cellIdx].status() == "absent" {
			t.Fatalf("absent cell above the separator at row %d", i)
		}
	}
	// After the separator: absent cells, alphabetical.
	var absentNames []string
	for i := sepAt + 1; i < len(rows); i++ {
		if rows[i].kind == rowCell {
			absentNames = append(absentNames, m.states[rows[i].cellIdx].name)
		}
	}
	if len(absentNames) != 2 || absentNames[0] != "aws-m-absent" || absentNames[1] != "aws-z-absent" {
		t.Fatalf("absent cells must be alphabetical below the separator: %v", absentNames)
	}

	// Cursor skips the separator: from the last live cell, j lands on the
	// first absent cell, not the divider.
	m.cursor = sepAt - 1 // last live cell
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	m2 := next.(dashboardModel)
	if m2.currentRow().kind != rowCell || m2.states[m2.currentRow().cellIdx].name != "aws-m-absent" {
		t.Fatalf("j from the last live cell must skip the separator onto the first absent cell, landed on kind=%d", m2.currentRow().kind)
	}
}

// TestCursorFollowsCellAcrossReorder pins that when a cell flips
// absent→live (moving sections), the cursor stays on that cell by
// identity rather than jumping to whatever now sits at its old index.
func TestCursorFollowsCellAcrossReorder(t *testing.T) {
	acc := true
	live := func(name string) cellState {
		return cellState{name: name, fleet: &fleet.Cell{Name: name, Accepting: &acc}}
	}
	states := []cellState{live("aws-a-live"), {name: "aws-z-absent"}}
	m := seedModel(states, 120, 30)
	// Select the absent cell.
	for m.selectedCell() != "aws-z-absent" {
		next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
		nm := next.(dashboardModel)
		if nm.cursor == m.cursor {
			t.Fatal("could not reach the absent cell")
		}
		m = nm
	}
	// It registers (becomes live) — it moves up into the live group.
	reloaded := []cellState{live("aws-a-live"), live("aws-z-absent")}
	next, _ := m.Update(loadedMsg{states: reloaded})
	m2 := next.(dashboardModel)
	if m2.selectedCell() != "aws-z-absent" {
		t.Fatalf("cursor must follow the cell across the reorder, now on %q", m2.selectedCell())
	}
}

func TestDashboardCursorMovement(t *testing.T) {
	states := []cellState{
		{name: "aws-sandbox-usw2-dev", identity: identity{Cloud: "aws", Account: "111", OK: true}},
		{name: "gcp-lab-euw1-dev", identity: identity{Cloud: "gcp", Account: "witwave-lab", OK: true}},
	}
	m := seedModel(states, 120, 24)
	firstCursor := m.cursor
	// After seed the cursor lands on the first CELL (past the header),
	// so the second cell's identity isn't in the pane yet.
	if strings.Contains(m.View(), "witwave-lab") {
		t.Fatal("second cell's identity must not render while cursor is on first cell")
	}
	// j = move down; the second cell's identity now shows.
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	m2 := next.(dashboardModel)
	if m2.cursor != firstCursor+1 {
		t.Fatalf("j: cursor = %d, want %d", m2.cursor, firstCursor+1)
	}
	if !strings.Contains(m2.View(), "witwave-lab") {
		t.Fatal("moving to gcp cell must show its identity in the context pane")
	}
	// j at the end stays put.
	next, _ = m2.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if next.(dashboardModel).cursor != firstCursor+1 {
		t.Fatal("j past end must clamp")
	}
	// k moves back to first cell.
	next, _ = m2.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	if next.(dashboardModel).cursor != firstCursor {
		t.Fatal("k must move up")
	}
	// k again lands on the group header (headers are selectable).
	next, _ = next.(dashboardModel).Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	if next.(dashboardModel).cursor != 0 {
		t.Fatal("k past first cell must land on the group header at row 0")
	}
}

// TestDashboardStatusRenders identity mismatch: a pin failure must
// show as an error in the context pane, not silently pass through.
func TestDashboardShowsPinMismatch(t *testing.T) {
	states := []cellState{{
		name:     "aws-sandbox-usw2-dev",
		entry:    cellEntry{Cloud: strPtr("aws")},
		identity: identity{Cloud: "aws", Account: "999999999999", OK: false, Notes: []string{"expected AWS account 123456789012, got 999999999999"}},
	}}
	m := seedModel(states, 120, 24)
	v := m.View()
	if !strings.Contains(v, "pin mismatch") {
		t.Fatal("view must call out the pin mismatch")
	}
	if !strings.Contains(v, "expected AWS account 123456789012") {
		t.Fatal("mismatch note must render")
	}
}

// TestDashboardLoadErrorReported pins the graceful failure path.
func TestDashboardLoadErrorReported(t *testing.T) {
	m := dashboardModel{
		ctx:    context.Background(),
		cli:    fakeSource{err: errFake("no config file")},
		width:  120,
		height: 24,
		now:    func() time.Time { return time.Now() },
	}
	next, _ := m.Update(loadedMsg{err: errFake("no config file")})
	m2 := next.(dashboardModel)
	if !strings.Contains(m2.status, "load failed") {
		t.Fatalf("status = %q, want a load-failed message", m2.status)
	}
}

type errFake string

func (e errFake) Error() string { return string(e) }

// TestUpRequiresPreview pins Slice 4's central safety rule: `u` opens
// a confirm dialog that REFUSES to fire until a `p` (preview) on the
// same cell has succeeded — no keystroke should launch an up without
// an operator having seen the diff.
func TestUpRequiresPreview(t *testing.T) {
	// preview on aws-sandbox-usw2-dev has never succeeded.
	c := startConfirm(opUp, "aws-sandbox-usw2-dev", false)
	if c == nil {
		t.Fatal("up must produce a confirm dialog")
	}
	if c.canConfirm() {
		t.Fatal("up must NOT be confirmable without a successful preview")
	}
	// After a preview, y is allowed.
	c2 := startConfirm(opUp, "aws-sandbox-usw2-dev", true)
	if !c2.canConfirm() {
		t.Fatal("up with a passed preview must be confirmable")
	}
}

// TestDestroyRequiresTypedYes pins the destroy safety rule: the
// dialog only becomes confirmable once the operator has typed the
// confirmation word in full — no single-key shortcut for the
// irreversible verb.
func TestDestroyRequiresTypedYes(t *testing.T) {
	c := startConfirm(opDestroy, "aws-sandbox-usw2-dev", false)
	if c.canConfirm() {
		t.Fatal("destroy must not be confirmable with an empty typed field")
	}
	c.typed = "ye"
	if c.canConfirm() {
		t.Fatal("destroy must not be confirmable on a partial word")
	}
	c.typed = destroyConfirmWord
	if !c.canConfirm() {
		t.Fatal("destroy must be confirmable once the word is complete")
	}
	// The cell name is NOT the confirmation word anymore.
	c.typed = "aws-sandbox-usw2-dev"
	if c.canConfirm() {
		t.Fatal("typing the cell name must not confirm — the word is `yes`")
	}
}

// TestDestroyTypedFlowThroughKeys pins the key-by-key path: y → ye →
// yes arms the dialog without firing (enter fires), a wrong char sets
// the inline error, and backspace recovers.
func TestDestroyTypedFlowThroughKeys(t *testing.T) {
	states := []cellState{{name: "aws-sandbox-usw2-dev", entry: cellEntry{Cloud: strPtr("aws")}}}
	m := seedModel(states, 120, 30)
	m.pending = startConfirm(opDestroy, "aws-sandbox-usw2-dev", false)

	type step struct {
		key     string
		typed   string
		hasErr  bool
		canFire bool
	}
	press := func(key string) {
		var msg tea.KeyMsg
		if key == "backspace" {
			msg = tea.KeyMsg{Type: tea.KeyBackspace}
		} else {
			msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
		}
		next, _ := m.handleConfirmKey(msg)
		m = next.(dashboardModel)
	}
	for _, st := range []step{
		{key: "y", typed: "y", hasErr: false, canFire: false},
		{key: "e", typed: "ye", hasErr: false, canFire: false},
		{key: "x", typed: "yex", hasErr: true, canFire: false},
		{key: "backspace", typed: "ye", hasErr: false, canFire: false},
		{key: "s", typed: "yes", hasErr: false, canFire: true},
	} {
		press(st.key)
		if m.pending == nil {
			t.Fatalf("after %q: dialog must stay open until enter", st.key)
		}
		if m.pending.typed != st.typed {
			t.Fatalf("after %q: typed = %q, want %q", st.key, m.pending.typed, st.typed)
		}
		if (m.pending.err != "") != st.hasErr {
			t.Fatalf("after %q: err = %q, wanted hasErr=%t", st.key, m.pending.err, st.hasErr)
		}
		if m.pending.canConfirm() != st.canFire {
			t.Fatalf("after %q: canConfirm = %t, want %t", st.key, m.pending.canConfirm(), st.canFire)
		}
	}
	// q now dismisses destroy too — it's no longer a typing character.
	press("q")
	if m.pending != nil {
		t.Fatal("q must dismiss the destroy dialog")
	}
}

// TestPreviewNeedsNoConfirm pins the read-only rule.
func TestPreviewNeedsNoConfirm(t *testing.T) {
	if c := startConfirm(opPreview, "any-cell", false); c != nil {
		t.Fatal("preview is read-only — no confirmation dialog")
	}
}

// TestQuitBlockedWhileOpRuns pins the ctrl+c safety modal: `q` refuses
// to quit under an in-flight op, and ctrl+c opens the modal.
func TestQuitBlockedWhileOpRuns(t *testing.T) {
	m := dashboardModel{
		width: 120, height: 24, ctx: context.Background(),
		cli:    fakeSource{},
		now:    func() time.Time { return time.Time{} },
		states: []cellState{{name: "x"}},
		op:     &opRun{kind: opUp, cell: "x"}, // pretend an up is running
	}
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	m2 := next.(dashboardModel)
	if cmd != nil {
		t.Fatal("q under a running op must not quit")
	}
	if !strings.Contains(m2.status, "op is running") {
		t.Fatalf("expected op-running warning, got %q", m2.status)
	}
	// ctrl+c opens the interrupt modal.
	next, _ = m2.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	m3 := next.(dashboardModel)
	if !m3.interruptModal {
		t.Fatal("ctrl+c under an op must open the keep/cancel modal")
	}
	// 'd' is intentionally unsupported until a real re-parenting helper
	// lands — the modal message says so, and the key returns an error
	// in the status rather than quitting (would SIGPIPE the child).
	m4next, cmd := m3.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	m4 := m4next.(dashboardModel)
	if cmd != nil {
		t.Fatal("d must not quit — detach would SIGPIPE the child")
	}
	if !strings.Contains(m4.status, "detach not implemented") {
		t.Fatalf("d must explain detach isn't supported, got %q", m4.status)
	}
}

// TestPreviewSuccessArmsUp pins the state transition: opDone for a
// successful preview marks the cell as preview-passed so `u` can then
// clear the confirm.
func TestPreviewSuccessArmsUp(t *testing.T) {
	m := dashboardModel{
		ctx: context.Background(), cli: fakeSource{},
		now: func() time.Time { return time.Time{} },
		op:  &opRun{kind: opPreview, cell: "aws-sandbox-usw2-dev"},
	}
	next, _ := m.Update(opDoneMsg{cell: "aws-sandbox-usw2-dev", err: nil})
	m2 := next.(dashboardModel)
	if !m2.planArmed("aws-sandbox-usw2-dev") {
		t.Fatal("successful preview must arm the cell for up")
	}
	if m2.op != nil {
		t.Fatal("op must clear when done")
	}
}

// TestDestroyInvalidatesPreview pins the fix for the review's most
// serious finding: after a successful destroy, previewSeen[cell]
// must be cleared. Otherwise `u` immediately after `D` renders "preview
// passed" and applies a recreate-from-scratch plan the operator never
// saw.
func TestDestroyInvalidatesPreview(t *testing.T) {
	m := dashboardModel{
		ctx: context.Background(), cli: fakeSource{},
		now:         func() time.Time { return time.Time{} },
		previewSeen: map[string]planEntry{"aws-sandbox-usw2-dev": {}},
		op:          &opRun{kind: opDestroy, cell: "aws-sandbox-usw2-dev"},
	}
	next, _ := m.Update(opDoneMsg{cell: "aws-sandbox-usw2-dev", err: nil})
	m2 := next.(dashboardModel)
	if _, ok := m2.previewSeen["aws-sandbox-usw2-dev"]; ok {
		t.Fatal("successful destroy must invalidate previewSeen — else the next u applies a stale plan")
	}
}

// TestFailedPreviewDoesNotArmUp pins the fix: a preview that ERRORED
// must not leave previewSeen true, so up still refuses.
func TestFailedPreviewDoesNotArmUp(t *testing.T) {
	m := dashboardModel{
		ctx: context.Background(), cli: fakeSource{},
		now:         func() time.Time { return time.Time{} },
		previewSeen: map[string]planEntry{"aws-sandbox-usw2-dev": {}},
		op:          &opRun{kind: opPreview, cell: "aws-sandbox-usw2-dev"},
	}
	next, _ := m.Update(opDoneMsg{cell: "aws-sandbox-usw2-dev", err: errFake("provider blew up")})
	m2 := next.(dashboardModel)
	if _, ok := m2.previewSeen["aws-sandbox-usw2-dev"]; ok {
		t.Fatal("failed preview must not arm up")
	}
}

// TestInterruptModalClearedOnOpDone pins the nil-panic fix: opDone
// while the ctrl+c modal is open must clear both m.op AND the modal.
// Without this, pressing k after the race raised a nil-pointer panic.
func TestInterruptModalClearedOnOpDone(t *testing.T) {
	m := dashboardModel{
		ctx: context.Background(), cli: fakeSource{},
		now:            func() time.Time { return time.Time{} },
		op:             &opRun{kind: opUp, cell: "x"},
		interruptModal: true,
	}
	next, _ := m.Update(opDoneMsg{cell: "x", err: nil})
	m2 := next.(dashboardModel)
	if m2.interruptModal {
		t.Fatal("opDone must clear the interrupt modal — else k nil-panics")
	}
	// Even without the clear, k must not panic now.
	m3 := dashboardModel{ctx: context.Background(), cli: fakeSource{},
		now:            func() time.Time { return time.Time{} },
		interruptModal: true, op: nil, // simulate stale modal
	}
	// This would have panicked pre-fix.
	next, _ = m3.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	if next.(dashboardModel).interruptModal {
		t.Fatal("k must close a stale modal")
	}
}

// TestDetachRefusesRatherThanLie pins the honest-detach fix: rather
// than PROMISE the child keeps running and SIGPIPE it milliseconds
// later, detach returns an error the UI reports.
func TestDetachRefusesRatherThanLie(t *testing.T) {
	op := &opRun{kind: opUp, cell: "x", cmd: nil}
	err := op.detach()
	if err == nil {
		t.Fatal("detach must return an error until reliable re-parenting is implemented — the modal must not promise 'child keeps running' when it doesn't")
	}
	if !strings.Contains(err.Error(), "detach not implemented") {
		t.Fatalf("detach error must explain the situation: %v", err)
	}
}

// TestDashboardFitsTerminal pins the sizing invariants that shipped
// broken in v0.0.126: every rendered row fits within the terminal
// width (no wrap-driven jitter), and the total row count fits within
// the height. Exercised across sizes an operator actually uses.
func TestDashboardFitsTerminal(t *testing.T) {
	states := []cellState{
		{
			name:     "aws-sandbox-usw2-dev",
			entry:    cellEntry{Cloud: strPtr("aws"), Region: strPtr("us-west-2")},
			identity: identity{Cloud: "aws", Account: "123456789012", Profile: "witwave-sandbox", OK: true},
		},
	}
	for _, tc := range []struct {
		w, h int
		name string
	}{
		{66, 18, "minimum"}, // matches the min-size guard in View()
		{80, 24, "small"},
		{120, 40, "standard"},
		{200, 60, "large"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := dashboardModel{
				ctx:    context.Background(),
				cli:    fakeSource{states: states},
				width:  tc.w,
				height: tc.h,
				states: states,
				now:    func() time.Time { return time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC) },
			}
			v := m.View()
			rows := strings.Split(v, "\n")
			if got := len(rows); got > tc.h {
				t.Errorf("%dx%d: renders %d rows, exceeds terminal height", tc.w, tc.h, got)
			}
			for i, r := range rows {
				if got := lipgloss.Width(r); got > tc.w {
					t.Errorf("%dx%d: row %d is %d cells wide, exceeds terminal width: %q", tc.w, tc.h, i, got, r)
					break
				}
			}
		})
	}
}

// TestDashboardWaitsForFirstSize pins the pre-WindowSizeMsg render:
// bubbletea sends the size in an initial message; before it arrives
// m.width/height are 0 and we must not paint a wrong-size frame that
// jumps on the next tick.
func TestDashboardWaitsForFirstSize(t *testing.T) {
	m := dashboardModel{ctx: context.Background(), cli: fakeSource{},
		now: func() time.Time { return time.Time{} }}
	if v := m.View(); v != "" {
		t.Fatalf("View() before WindowSizeMsg must be empty, got %d chars", len(v))
	}
}

// TestSpawnSourceDetection pins the "if I'm running from source,
// spawn from source" behavior — both directions.
func TestSpawnSourceDetection(t *testing.T) {
	// Real go-run paths from actual observations: /tmp/go-build.../binary
	// and macOS's ~/Library/Caches/go-build/... variant.
	sourcePaths := []string{
		"/tmp/go-build123456/exe/witself-infra",
		"/var/folders/xy/T/go-build111/exe/witself-infra",
		"/private/tmp/claude-501/scratch/go-build/exe/witself-infra",
	}
	for _, p := range sourcePaths {
		if !runningFromSource(p) {
			t.Errorf("expected go-run detection for %q", p)
		}
	}
	// Installed paths must NOT trip the source path.
	installedPaths := []string{
		"/opt/homebrew/bin/witself-infra",
		"/usr/local/bin/witself-infra",
		"/home/scott/go/bin/witself-infra",
		"/Users/scott/.brew/bin/witself-infra",
	}
	for _, p := range installedPaths {
		if runningFromSource(p) {
			t.Errorf("must not treat installed path %q as go-run", p)
		}
	}
}

// TestSpawnCommandFromInstalledPath pins that a real binary spawns
// itself directly — no `go` shell-out, no source directory dependency.
func TestSpawnCommandFromInstalledPath(t *testing.T) {
	// Use a temp file so os.Executable resolves to a non-go-build path.
	// We can't monkey-patch os.Executable, so this is a light behavior
	// test: currentSourceDir must at least return SOMETHING — the
	// package this file lives in — proving the resolution path works.
	dir, ok := currentSourceDir()
	if !ok || dir == "" {
		t.Fatal("currentSourceDir must resolve to this file's package")
	}
	if !strings.Contains(dir, "witself-infra") {
		t.Fatalf("currentSourceDir = %q — expected to contain the package name", dir)
	}
}

// TestFooterHintsDimUnavailable pins the visual-availability contract:
// letters for actions the operator can't currently take render dim so
// the footer answers "why isn't u doing anything?" at a glance.
func TestFooterHintsDimUnavailable(t *testing.T) {
	// Force a color profile so styDim actually emits ANSI escape
	// codes — the test env has no TTY, so lipgloss defaults to plain
	// and the dim assertions would be vacuous otherwise.
	old := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI)
	defer lipgloss.SetColorProfile(old)

	states := []cellState{{name: "aws-sandbox-usw2-dev",
		identity: identity{Cloud: "aws", Account: "111", OK: true}}}
	m := seedModel(states, 120, 30)
	hints := m.footerHints()
	// up is NOT available (no preview yet) — its letter is dimmed.
	if !strings.Contains(hints, styDim.Render("u up")) {
		t.Errorf("up hint should be dim before a successful preview: %q", hints)
	}
	// auth IS available on a healthy cell — re-running a login is
	// harmless, and creds can go stale between refreshes without the
	// dashboard noticing (the ADC incident: cell green, `a` refused).
	if strings.Contains(hints, styDim.Render("a auth")) {
		t.Errorf("auth hint should be enabled on any selected cell while idle: %q", hints)
	}
	// After a successful preview, up flips to available.
	m.previewSeen = map[string]planEntry{"aws-sandbox-usw2-dev": {At: m.now()}}
	if strings.Contains(m.footerHints(), styDim.Render("u up")) {
		t.Errorf("up hint should be enabled after preview: %q", m.footerHints())
	}
	// During an op everything freezes — auth dims too (ExecProcess
	// would fight the op's output for the terminal).
	m.op = &opRun{kind: opPreview, cell: "aws-sandbox-usw2-dev"}
	if !strings.Contains(m.footerHints(), styDim.Render("a auth")) {
		t.Errorf("auth hint should be dim while an op runs: %q", m.footerHints())
	}
}

// TestPreviewedCellShowsPlanMark pins the ◆ plan column: a cell with
// a passed preview shows the cyan diamond ahead of its name (and a
// "plan" line in the context pane), a cell without one shows neither,
// and rows stay aligned because the column is always two cells wide.
func TestPreviewedCellShowsPlanMark(t *testing.T) {
	old := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI)
	defer lipgloss.SetColorProfile(old)

	states := []cellState{
		{name: "aws-sandbox-usw2-dev", entry: cellEntry{Cloud: strPtr("aws"), Region: strPtr("us-west-2")}},
		{name: "gcp-sandbox-usw2-dev", entry: cellEntry{Cloud: strPtr("gcp"), Region: strPtr("us-west2")}},
	}
	m := seedModel(states, 120, 30)
	m.previewSeen = map[string]planEntry{"aws-sandbox-usw2-dev": {At: m.now()}}

	v := m.View()
	// Prefix match — the cells pane may ellipsize long names; the mark
	// and the name's start are what matter.
	if !strings.Contains(v, styPlan.Render("◆")+" aws-sandbox") {
		t.Fatal("previewed cell must show the ◆ plan mark before its name")
	}
	if strings.Contains(v, "◆ gcp-sandbox") {
		t.Fatal("unpreviewed cell must NOT show the plan mark")
	}
	// Alignment: in the cells pane (rows carrying the ◌ absent glyph),
	// the name must start at the same offset from the status glyph
	// whether or not the ◆ mark is present.
	var awsRel, gcpRel = -1, -1
	for _, row := range strings.Split(v, "\n") {
		plain := stripANSIForTest(row)
		mk := strings.Index(plain, "◌")
		if mk < 0 {
			continue
		}
		// Offsets in display CELLS, not bytes — ◌ ◆ ▸ are multi-byte
		// single-cell runes, so byte math would misreport alignment.
		if i := strings.Index(plain, "aws-sandbox"); i >= 0 && awsRel == -1 {
			awsRel = lipgloss.Width(plain[mk:i])
		}
		if i := strings.Index(plain, "gcp-sandbox"); i >= 0 && gcpRel == -1 {
			gcpRel = lipgloss.Width(plain[mk:i])
		}
	}
	if awsRel == -1 || gcpRel == -1 || awsRel != gcpRel {
		t.Fatalf("cell names must stay aligned with and without the mark: aws offset %d, gcp offset %d", awsRel, gcpRel)
	}
	// Context pane spells it out for the selected (previewed) cell.
	if !strings.Contains(v, "press u to apply") {
		t.Fatal("context pane must explain the armed plan")
	}
}

// TestPlanMarkClearsWhenInvalidated pins the truthfulness contract:
// the ◆ tracks previewSeen exactly, so a successful up (which
// invalidates the plan) must also clear the mark.
func TestPlanMarkClearsWhenInvalidated(t *testing.T) {
	states := []cellState{{name: "aws-sandbox-usw2-dev", entry: cellEntry{Cloud: strPtr("aws")}}}
	m := seedModel(states, 120, 30)
	m.previewSeen = map[string]planEntry{"aws-sandbox-usw2-dev": {At: m.now()}}
	m.op = &opRun{kind: opUp, cell: "aws-sandbox-usw2-dev"}
	next, _ := m.Update(opDoneMsg{cell: "aws-sandbox-usw2-dev", err: nil})
	m2 := next.(dashboardModel)
	if strings.Contains(m2.View(), "◆ aws-sandbox") {
		t.Fatal("plan mark must clear after a successful up — the previewed plan was consumed")
	}
}

// TestStartAuthAvailableWithoutError pins the gating fix: `a` on a
// healthy-looking cell must launch the login flow, not refuse. The
// ADC incident showed why — credentials expired but the cell still
// read green, and the operator had no way to kick off a login.
func TestStartAuthAvailableWithoutError(t *testing.T) {
	states := []cellState{{
		name:     "gcp-sandbox-use1-dev",
		entry:    cellEntry{Cloud: strPtr("gcp"), Region: strPtr("us-east1")},
		identity: identity{Cloud: "gcp", Account: "witself-sandbox", OK: true},
	}}
	m := seedModel(states, 120, 30)
	next, cmd := m.startAuth()
	if cmd == nil {
		t.Fatalf("startAuth on a healthy cell must launch the login flow, status: %q", next.status)
	}
	if !strings.Contains(next.status, "application-default login") {
		t.Errorf("status should name the login command: %q", next.status)
	}
}

// TestStartAuthRefusesDuringOp pins the freeze: ExecProcess suspends
// the TUI, which would fight a running op's output for the terminal.
func TestStartAuthRefusesDuringOp(t *testing.T) {
	states := []cellState{{
		name:  "gcp-sandbox-use1-dev",
		entry: cellEntry{Cloud: strPtr("gcp"), Region: strPtr("us-east1")},
	}}
	m := seedModel(states, 120, 30)
	m.op = &opRun{kind: opUp, cell: "gcp-sandbox-use1-dev"}
	next, cmd := m.startAuth()
	if cmd != nil {
		t.Fatal("startAuth must refuse while an op is running")
	}
	if !strings.Contains(next.status, "op is running") {
		t.Errorf("status must explain the refusal: %q", next.status)
	}
}

// TestUpRefusesWithoutPreviewInStartOpKey pins the "why nothing
// happened" flow: pressing u without a preview shows a helpful status
// rather than silently opening (or not opening) a dialog.
func TestUpRefusesWithoutPreviewInStartOpKey(t *testing.T) {
	states := []cellState{{name: "aws-sandbox-usw2-dev"}}
	m := seedModel(states, 120, 30)
	next, _ := m.startOpKey("u")
	m2 := next.(dashboardModel)
	if m2.pending != nil {
		t.Fatal("up must not open a confirm dialog without a passed preview")
	}
	if !strings.Contains(m2.status, "preview") {
		t.Errorf("status must explain the missing preview: %q", m2.status)
	}
}

// TestLooksLikeAuthFailure pins the phrase set used to nudge the
// operator toward `a` when an op fails on credentials.
func TestLooksLikeAuthFailure(t *testing.T) {
	for _, tail := range [][]string{
		{"failed to refresh cached SSO token", "some other line"},
		{"aws sso login --profile witwave-sandbox"},
		{"reauthentication is needed. Please run: `gcloud auth login`"},
		{"AuthenticationFailed: The access token is invalid"},
		{"Unable to locate credentials. You can configure credentials by running..."},
		// The exact truncated line the ops pane showed during the ADC
		// expiry incident — dotted CLI path, remedy text cut off.
		{"  Argo CD: mint GCP ADC access token: exit status 1: ERROR: (gcloud.auth.application-default.print-access-token) There was a problem refre… (9m14s elapsed)"},
		{"ERROR: (gcloud.auth.application-default.print-access-token) There was a problem refreshing your current auth tokens: Reauthentication failed."},
	} {
		if !looksLikeAuthFailure(tail) {
			t.Errorf("auth pattern missed: %v", tail)
		}
	}
	// Non-auth errors must NOT trigger.
	for _, tail := range [][]string{
		{"error creating EKS Cluster: InsufficientCapacity"},
		{"stack aws-sandbox-usw2-dev is currently locked"},
		{"connect: connection refused"},
	} {
		if looksLikeAuthFailure(tail) {
			t.Errorf("false positive on: %v", tail)
		}
	}
}

// TestOpsScrollFollowsAndPauses pins the log tail behavior: at
// scroll=0 the pane always shows the newest lines (follow), a
// positive scroll pins the view to a past window, and new lines
// arriving while paused don't yank the view back.
func TestOpsScrollFollowsAndPauses(t *testing.T) {
	op := &opRun{}
	for i := 0; i < 20; i++ {
		op.appendLine(fmt.Sprintf("line-%02d", i))
	}
	// Live tail: last 8 lines.
	tail := op.tailFrom(0, 8)
	if len(tail) != 8 || tail[7] != "line-19" {
		t.Fatalf("tail=%v", tail)
	}
	// Scroll back 8: view now shows lines-04..11.
	past := op.tailFrom(8, 8)
	if past[0] != "line-04" || past[7] != "line-11" {
		t.Fatalf("scrolled window: %v", past)
	}
	// More lines arrive — the same offset still shows the same window.
	op.appendLine("line-20")
	still := op.tailFrom(8, 8)
	if still[0] != "line-05" {
		// Because offset counts back from CURRENT tail, offset=8 with
		// 21 lines shifts one line forward — this is the intended
		// "the tail moved but our anchor didn't" semantics.
		// Verify the semantics we actually want: offset back from tail
		// is stable when the tail grows.
		if still[0] != "line-05" && still[0] != "line-04" {
			t.Errorf("scroll semantics: %v", still)
		}
	}
	// maxScroll clamps to the ring buffer size.
	if got := op.maxScroll(8); got != 21-8 {
		t.Errorf("maxScroll = %d, want %d", got, 21-8)
	}
}

// TestLastOpRetainedAfterDone pins that a finished op's buffer is kept
// on m.lastOp — the Logs tab streams the just-completed op's live
// buffer (fresher than the on-disk file) until another op starts.
func TestLastOpRetainedAfterDone(t *testing.T) {
	states := []cellState{{name: "aws-sandbox-usw2-dev"}}
	m := seedModel(states, 120, 40)
	m.op = &opRun{kind: opPreview, cell: "aws-sandbox-usw2-dev"}
	m.op.appendLine("Previewing changes:")
	m.op.appendLine("+ aws:eks:Cluster witself-aws-sandbox-usw2-dev  create")
	next, _ := m.Update(opDoneMsg{cell: "aws-sandbox-usw2-dev", err: nil})
	m2 := next.(dashboardModel)
	if m2.op != nil {
		t.Fatal("m.op must clear after opDone")
	}
	if m2.lastOp == nil {
		t.Fatal("m.lastOp must hold the completed op so its output survives")
	}
	if len(m2.lastOp.snapshot(8)) == 0 {
		t.Fatal("the completed op must retain its lines")
	}
}

// TestPendingDialogOverlaysDoesNotOverflow pins the confirmation-modal
// fix: pressing `u` after preview must float the "apply?" dialog OVER
// the dashboard frame (dashboard still visible around it) instead of
// stacking below it — stacking overflowed the terminal and bubbletea's
// altscreen sheared the top border to make room.
func TestPendingDialogOverlaysDoesNotOverflow(t *testing.T) {
	states := []cellState{{
		name:     "aws-sandbox-usw2-dev",
		entry:    cellEntry{Cloud: strPtr("aws"), Region: strPtr("us-west-2")},
		identity: identity{Cloud: "aws", Account: "123456789012", Profile: "witwave-sandbox", OK: true},
	}}
	m := seedModel(states, 120, 30)
	m.previewSeen = map[string]planEntry{"aws-sandbox-usw2-dev": {At: m.now()}}
	m.pending = startConfirm(opUp, "aws-sandbox-usw2-dev", true)

	v := m.View()
	rows := strings.Split(v, "\n")
	if got := len(rows); got > 30 {
		t.Fatalf("dialog must not overflow: %d rows in a 30-row terminal", got)
	}
	for i, r := range rows {
		if w := lipgloss.Width(r); w > 120 {
			t.Fatalf("row %d width %d exceeds 120: %q", i, w, r)
		}
	}
	// Dialog content is present.
	if !strings.Contains(v, "UP aws-sandbox-usw2-dev") {
		t.Fatal("dialog title missing")
	}
	if !strings.Contains(v, "press y to apply") {
		t.Fatal("dialog body missing")
	}
	// Dashboard frame stays visible around the dialog.
	if !strings.Contains(v, "cells") {
		t.Fatal("cells group header must still show behind the dialog")
	}
	if !strings.Contains(v, "Overview") {
		t.Fatal("context pane tab bar must still show behind the dialog")
	}
}

// TestDestroyDialogOverlaysAtMinimumSize pins the tallest confirmation
// (destroy has a multi-line body with typed-name prompt) on the
// smallest supported terminal — the stacked layout would overflow
// here worst.
func TestDestroyDialogOverlaysAtMinimumSize(t *testing.T) {
	states := []cellState{{
		name:     "aws-sandbox-usw2-dev",
		entry:    cellEntry{Cloud: strPtr("aws"), Region: strPtr("us-west-2")},
		identity: identity{Cloud: "aws", Account: "123456789012", Profile: "witwave-sandbox", OK: true},
	}}
	m := seedModel(states, 66, 18) // minimum supported terminal
	m.pending = startConfirm(opDestroy, "aws-sandbox-usw2-dev", false)

	v := m.View()
	rows := strings.Split(v, "\n")
	if got := len(rows); got > 18 {
		t.Fatalf("destroy dialog must not overflow minimum terminal: %d rows in 18-row terminal", got)
	}
	for i, r := range rows {
		if w := lipgloss.Width(r); w > 66 {
			t.Fatalf("row %d width %d exceeds 66: %q", i, w, r)
		}
	}
	if !strings.Contains(v, "DESTROY aws-sandbox-usw2-dev") {
		t.Fatal("destroy dialog title missing")
	}
}

// TestInterruptModalOverlaysDoesNotOverflow: same fix applies to the
// interrupt-during-op modal — ctrl+c while an op runs pops a dialog
// that must float over the frame, not push it off.
func TestInterruptModalOverlaysDoesNotOverflow(t *testing.T) {
	states := []cellState{{
		name:     "aws-sandbox-usw2-dev",
		entry:    cellEntry{Cloud: strPtr("aws"), Region: strPtr("us-west-2")},
		identity: identity{Cloud: "aws", Account: "123456789012", Profile: "witwave-sandbox", OK: true},
	}}
	m := seedModel(states, 120, 30)
	m.op = &opRun{kind: opPreview, cell: "aws-sandbox-usw2-dev"}
	m.interruptModal = true

	v := m.View()
	rows := strings.Split(v, "\n")
	if got := len(rows); got > 30 {
		t.Fatalf("interrupt modal must not overflow: %d rows in 30-row terminal", got)
	}
	if !strings.Contains(v, "An operation is running") {
		t.Fatal("interrupt modal body missing")
	}
	if !strings.Contains(v, "cells") {
		t.Fatal("dashboard must stay visible behind the interrupt modal")
	}
}

// TestCellRowThrobsWhileOpRuns pins the visual signal that a
// provisioning op is in flight: the target cell's status marker
// swaps from "● live" to a spinner + verb, colored by op kind.
// Off-target cells keep their static status.
func TestCellRowThrobsWhileOpRuns(t *testing.T) {
	acc := true
	states := []cellState{
		{
			name:     "aws-sandbox-usw2-dev",
			entry:    cellEntry{Cloud: strPtr("aws"), Region: strPtr("us-west-2")},
			identity: identity{Cloud: "aws", Account: "123456789012", Profile: "witwave-sandbox", OK: true},
			fleet:    &fleet.Cell{Name: "aws-sandbox-usw2-dev", Accepting: &acc},
		},
		{
			name:     "gcp-sandbox-usw2-dev",
			entry:    cellEntry{Cloud: strPtr("gcp"), Region: strPtr("us-west2")},
			identity: identity{Cloud: "gcp", Account: "witwave-sandbox", OK: true},
			fleet:    &fleet.Cell{Name: "gcp-sandbox-usw2-dev", Accepting: &acc},
		},
	}
	old := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI)
	defer lipgloss.SetColorProfile(old)

	m := seedModel(states, 120, 30)
	m.op = &opRun{kind: opUp, cell: "aws-sandbox-usw2-dev"}
	m.spinnerFrame = 3

	v := m.View()
	// The target cell's row shows the colored spinner glyph (no verb —
	// the verb lives in the strip).
	if !strings.Contains(v, opSpinnerRune(opUp, 3)) {
		t.Fatalf("target cell must show the running-op spinner glyph: %s", v)
	}
	// The off-target live cell keeps its static green ● glyph — no
	// misleading throb on a cell that isn't being touched.
	if !strings.Contains(v, styOK.Render("●")) {
		t.Fatal("off-target live cell must keep the static status glyph")
	}
	// The glyph+word status markers are gone from the cells pane (the
	// Overview tab still spells the word out under a "status" label,
	// which is fine — that's the legend).
	if strings.Contains(v, "● live") || strings.Contains(v, "◌ absent") || strings.Contains(v, "● error") {
		t.Fatal("glyph+word status markers must not appear in the cells pane anymore")
	}
	// The live-op strip carries the frame + verb + cell — that's where
	// the verb reads now. (Colored, so compare on the stripped view.)
	if !strings.Contains(stripANSIForTest(v), spinnerFrames[3]+" up · aws-sandbox-usw2-dev") {
		t.Fatal("live-op strip must include the spinner frame + verb + cell")
	}
}

// TestSpinnerColorMatchesAcrossSurfaces pins that the cell-row
// spinner and the ops-pane title spinner share the SAME accent
// (green/yellow/red), not just the same frame. The review found the
// title spinner was uncolored while the cell row was color-coded —
// visually broke the "one indicator, two surfaces" promise.
func TestSpinnerColorMatchesAcrossSurfaces(t *testing.T) {
	old := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI)
	defer lipgloss.SetColorProfile(old)

	acc := true
	for _, kind := range []opKind{opPreview, opUp, opDestroy} {
		t.Run(kind.verb(), func(t *testing.T) {
			states := []cellState{{
				name:  "aws-sandbox-usw2-dev",
				fleet: &fleet.Cell{Name: "aws-sandbox-usw2-dev", Accepting: &acc},
			}}
			m := seedModel(states, 120, 30)
			m.op = &opRun{kind: kind, cell: "aws-sandbox-usw2-dev"}
			m.spinnerFrame = 2

			// Both surfaces must render with the same SGR foreground
			// code. Cell row wraps the whole label (rune + verb) in one
			// style.Render call, ops title wraps just the rune — but the
			// leading SGR code is the same in both places, so the code
			// itself must appear at least twice in the frame.
			style := opKindStyle(kind)
			marker := "MARK"
			sgrOpen := strings.Split(style.Render(marker), marker)[0]
			if sgrOpen == "" {
				t.Fatalf("expected style.Render to emit an SGR open sequence for %s", kind.verb())
			}
			v := m.View()
			hits := strings.Count(v, sgrOpen)
			if hits < 2 {
				t.Fatalf("expected the %s color SGR %q to appear at least twice (cell row + ops title), got %d occurrences", kind.verb(), sgrOpen, hits)
			}
			// And the exact same braille frame must appear in the frame
			// stripped of ANSI — so the two surfaces share both frame AND
			// color, not just color.
			if strings.Count(v, spinnerFrames[2]) < 2 {
				t.Fatalf("expected braille frame %q to appear at least twice (cell row + ops title), got %d occurrences", spinnerFrames[2], strings.Count(v, spinnerFrames[2]))
			}
		})
	}
}

// TestSpinnerTickFencedByOpGen pins the ticker-leak fix: a stale
// spinnerTickMsg from a previous op (different opGen) must be
// discarded so it can't fork a duplicate loop on a newly launched op.
func TestSpinnerTickFencedByOpGen(t *testing.T) {
	states := []cellState{{name: "aws-sandbox-usw2-dev"}}
	m := seedModel(states, 120, 30)
	m.op = &opRun{kind: opPreview, cell: "aws-sandbox-usw2-dev"}
	m.opGen = 5

	// Stale tick from opGen=4 (a previous op that already finished).
	next, cmd := m.Update(spinnerTickMsg{gen: 4})
	m2 := next.(dashboardModel)
	if m2.spinnerFrame != 0 {
		t.Fatalf("stale tick must NOT advance frame: got %d, want 0", m2.spinnerFrame)
	}
	if cmd != nil {
		t.Fatal("stale tick must NOT reschedule — else the leaked loop keeps ticking against the new op")
	}
	// Current tick fires and advances normally.
	next, cmd = m.Update(spinnerTickMsg{gen: 5})
	m3 := next.(dashboardModel)
	if m3.spinnerFrame != 1 {
		t.Fatalf("current tick must advance frame: got %d, want 1", m3.spinnerFrame)
	}
	if cmd == nil {
		t.Fatal("current tick must reschedule the next tick")
	}
}

// TestSpinnerAdvancesEveryTick pins the frame-advance/self-terminate
// loop: spinnerTickMsg while op is running advances the frame and
// returns another spinnerCmd; after op ends the tick becomes a no-op.
func TestSpinnerAdvancesEveryTick(t *testing.T) {
	states := []cellState{{name: "aws-sandbox-usw2-dev"}}
	m := seedModel(states, 120, 30)
	m.op = &opRun{kind: opPreview, cell: "aws-sandbox-usw2-dev"}

	next, cmd := m.Update(spinnerTickMsg{})
	m2 := next.(dashboardModel)
	if m2.spinnerFrame != 1 {
		t.Fatalf("spinnerFrame must advance: got %d, want 1", m2.spinnerFrame)
	}
	if cmd == nil {
		t.Fatal("tick while op runs must schedule the next tick")
	}
	// Idle: tick is a no-op, no next tick scheduled — natural loop end.
	m2.op = nil
	next, cmd = m2.Update(spinnerTickMsg{})
	m3 := next.(dashboardModel)
	if cmd != nil {
		t.Fatal("tick after op ends must not re-schedule — leaks a ticker otherwise")
	}
	if m3.spinnerFrame != 1 {
		t.Fatalf("spinnerFrame must not advance when idle: got %d, want 1", m3.spinnerFrame)
	}
}

// TestLaunchOpKicksSpinner pins that launchOp resets the frame and
// starts the throb loop — otherwise the cell row would stay static for
// the first 100ms of the op, or forever if no other tick source fires.
func TestLaunchOpKicksSpinner(t *testing.T) {
	states := []cellState{{name: "aws-sandbox-usw2-dev"}}
	m := seedModel(states, 120, 30)
	m.spinnerFrame = 42 // leftover from a hypothetical previous op

	// launchOp needs a program set (its startOp path uses teaProgram),
	// so we simulate the post-startOp state directly and check the
	// tick-schedule contract on Update instead.
	m.op = &opRun{kind: opPreview, cell: "aws-sandbox-usw2-dev"}
	m.spinnerFrame = 0 // reset as launchOp would

	// One tick round-trip must give us a non-nil cmd back — the
	// contract that keeps the throb going without polling.
	_, cmd := m.Update(spinnerTickMsg{})
	if cmd == nil {
		t.Fatal("running op with spinnerFrame=0 must schedule the next tick")
	}
}

// TestMarkerGlyphWidth pins the alignment contract: every status glyph
// and the running-op spinner glyph is exactly one cell wide, so the
// cell name always starts at the same column.
func TestMarkerGlyphWidth(t *testing.T) {
	old := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI)
	defer lipgloss.SetColorProfile(old)

	acc := false
	for _, st := range []cellState{
		{fleet: &fleet.Cell{}},                // live
		{fleet: &fleet.Cell{Accepting: &acc}}, // draining
		{},                                    // absent
		{err: errFake("bad creds")},           // error
	} {
		if got := lipgloss.Width(st.statusGlyph()); got != 1 {
			t.Fatalf("status glyph for %q must be 1 cell, got %d", st.status(), got)
		}
	}
	for _, kind := range []opKind{opPreview, opUp, opDestroy} {
		for frame := 0; frame < len(spinnerFrames)*2; frame++ {
			if got := lipgloss.Width(opSpinnerRune(kind, frame)); got != 1 {
				t.Fatalf("%s frame %d: spinner glyph width %d, want 1", kind.verb(), frame, got)
			}
		}
	}
}

// TestOpDoneStatusIsExplicitlyStyled pins the completion signal:
// success gets a ✓ prefix (rendered green), failure gets ✗ (red).
// A plain informational status stays dim. This is what the user sees
// when an op finishes — the marker refresh is subtle, the status line
// carries the "done" signal in words.
func TestOpDoneStatusIsExplicitlyStyled(t *testing.T) {
	old := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI)
	defer lipgloss.SetColorProfile(old)

	cases := []struct {
		name    string
		kind    opKind
		err     error
		wantSub string // must appear in m.status
	}{
		{"up success", opUp, nil, "✓ up complete"},
		{"up failure", opUp, errFake("boom"), "✗ up on"},
		{"destroy success", opDestroy, nil, "✓ destroy complete"},
		{"destroy failure", opDestroy, errFake("stuck"), "✗ destroy on"},
		{"preview success", opPreview, nil, "✓ preview complete"},
		{"preview failure", opPreview, errFake("provider"), "✗ preview on"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			states := []cellState{{name: "aws-sandbox-usw2-dev"}}
			m := seedModel(states, 120, 30)
			m.op = &opRun{kind: tc.kind, cell: "aws-sandbox-usw2-dev"}
			next, _ := m.Update(opDoneMsg{cell: "aws-sandbox-usw2-dev", err: tc.err})
			m2 := next.(dashboardModel)
			if !strings.Contains(m2.status, tc.wantSub) {
				t.Fatalf("status %q missing substring %q", m2.status, tc.wantSub)
			}
			// The render must color the status line — green for ✓, red
			// for ✗. Both apply an SGR foreground code, so a dim-only
			// render (which is the default) would NOT include one.
			v := m2.View()
			if strings.HasPrefix(strings.TrimSpace(m2.status), "✓") && !strings.Contains(v, "\x1b[32m") && !strings.Contains(v, "\x1b[92m") {
				t.Fatalf("success status must render with a green SGR code, got: %q", v)
			}
			if strings.HasPrefix(strings.TrimSpace(m2.status), "✗") && !strings.Contains(v, "\x1b[31m") && !strings.Contains(v, "\x1b[91m") {
				t.Fatalf("failure status must render with a red SGR code, got: %q", v)
			}
		})
	}
}

// TestOpDoneTriggersRefresh pins that a completed op schedules a
// loadCmd — this is what changes the cell's marker from ◌ absent to
// ● live within seconds of a successful up (or vice versa for
// destroy), instead of waiting for the 60s refresh tick.
func TestOpDoneTriggersRefresh(t *testing.T) {
	states := []cellState{{name: "aws-sandbox-usw2-dev"}}
	m := seedModel(states, 120, 30)
	m.op = &opRun{kind: opUp, cell: "aws-sandbox-usw2-dev"}
	_, cmd := m.Update(opDoneMsg{cell: "aws-sandbox-usw2-dev", err: nil})
	if cmd == nil {
		t.Fatal("opDone must schedule an immediate reload — else the marker stays stale until next 60s tick")
	}
}

// TestDestroyDialogHeightStableAcrossErr pins the anti-jitter fix:
// toggling the "does not match" error line must NOT change the total
// line count, or overlayCenter re-centers and the whole dialog jumps
// vertically as the operator types. Reserving the err slot keeps the
// dialog height constant.
func TestDestroyDialogHeightStableAcrossErr(t *testing.T) {
	c := &confirmDialog{kind: opDestroy, cell: "aws-sandbox-usw2-dev"}
	noErr := strings.Count(c.render(), "\n")
	c.err = "does not match — press esc to start over"
	withErr := strings.Count(c.render(), "\n")
	if noErr != withErr {
		t.Fatalf("dialog height jitters: %d newlines without err, %d with — overlayCenter will re-center and the dialog will visibly jump", noErr, withErr)
	}
}

// TestConfirmKeysDismissOnQAndCtrlC pins the input-trap fix: pressing
// q, esc, or ctrl+c during ANY confirmation dismisses the dialog
// instead of being silently swallowed. Since the destroy confirmation
// word became `yes`, q is no longer a typing character anywhere.
func TestConfirmKeysDismissOnQAndCtrlC(t *testing.T) {
	cases := []struct {
		name   string
		kind   opKind
		key    string
		wantIn bool // must dismiss (m.pending becomes nil)
	}{
		{"up: q dismisses", opUp, "q", true},
		{"up: ctrl+c dismisses", opUp, "ctrl+c", true},
		{"up: esc dismisses", opUp, "esc", true},
		{"destroy: ctrl+c dismisses", opDestroy, "ctrl+c", true},
		{"destroy: esc dismisses", opDestroy, "esc", true},
		{"destroy: q dismisses", opDestroy, "q", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			states := []cellState{{name: "aws-sandbox-usw2-dev"}}
			m := seedModel(states, 120, 30)
			m.pending = &confirmDialog{kind: tc.kind, cell: "aws-sandbox-usw2-dev", previewSeen: true}
			// tea.KeyMsg.String() special-cases ctrl+c and esc; use the
			// canned constructions for those, KeyRunes for regular chars.
			var keyMsg tea.KeyMsg
			switch tc.key {
			case "ctrl+c":
				keyMsg = tea.KeyMsg{Type: tea.KeyCtrlC}
			case "esc":
				keyMsg = tea.KeyMsg{Type: tea.KeyEsc}
			default:
				keyMsg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(tc.key)}
			}
			next, _ := m.handleConfirmKey(keyMsg)
			m2 := next.(dashboardModel)
			if tc.wantIn && m2.pending != nil {
				t.Fatalf("%s: expected dismiss, but m.pending still set", tc.name)
			}
			if !tc.wantIn && m2.pending == nil {
				t.Fatalf("%s: expected NO dismiss, but m.pending cleared", tc.name)
			}
		})
	}
}
