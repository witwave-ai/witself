package main

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/witwave-ai/witself/infra/pulumi/internal/fleet"
)

const testCP = "https://cp.test"

// cpSeed builds a model with one CP-grouped live cell (so a real
// header row exists) and the cursor moved onto that header with the
// context pane focused on the Settings tab.
func cpSeed(t *testing.T, src fakeSource) dashboardModel {
	t.Helper()
	acc := true
	if src.states == nil {
		src.states = []cellState{{
			name:         "aws-sandbox-usw2-dev",
			controlPlane: testCP,
			fleet:        &fleet.Cell{Name: "aws-sandbox-usw2-dev", Accepting: &acc},
		}}
	}
	m := dashboardModel{
		ctx:    t.Context(),
		cli:    src,
		width:  120,
		height: 30,
		now:    func() time.Time { return time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC) },
	}
	next, _ := m.Update(loadedMsg{states: src.states})
	m = next.(dashboardModel)
	// Cursor starts on the first cell; k moves to the header.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	m = next.(dashboardModel)
	if m.currentRow().kind != rowHeader {
		t.Fatal("seed: cursor must be on the CP header")
	}
	// Focus the context pane, then → to Settings.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = next.(dashboardModel)
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRight})
	m = next.(dashboardModel)
	if m.cpActiveTab != cpTabSettings {
		t.Fatalf("seed: expected Settings tab, got %d", m.cpActiveTab)
	}
	if cmd == nil {
		t.Fatal("seed: landing on Settings must kick a config read")
	}
	return m
}

// deliver runs the pending settings read result into the model.
func deliverCfg(t *testing.T, m dashboardModel, cfg cpConfig) dashboardModel {
	t.Helper()
	next, _ := m.Update(cpSettingsMsg{cp: testCP, cfg: cfg})
	return next.(dashboardModel)
}

// TestCPHeaderShowsTabs pins the header tab strip: a real CP header
// shows Overview|Settings; the self-hosted group stays untabbed; cell
// rows keep their five tabs.
func TestCPHeaderShowsTabs(t *testing.T) {
	acc := true
	states := []cellState{
		{name: "aws-a", controlPlane: testCP, fleet: &fleet.Cell{Accepting: &acc}},
		{name: "self-z", controlPlane: "", fleet: &fleet.Cell{Accepting: &acc}},
	}
	m := dashboardModel{
		ctx: t.Context(), cli: fakeSource{states: states},
		width: 120, height: 30,
		now: func() time.Time { return time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC) },
	}
	next, _ := m.Update(loadedMsg{states: states})
	m = next.(dashboardModel)

	// Onto the CP header.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	m2 := next.(dashboardModel)
	v := m2.View()
	if !strings.Contains(v, "Settings") {
		t.Fatal("CP header must show the Settings tab")
	}
	if strings.Contains(v, "Kubernetes") {
		t.Fatal("CP header must show CP tabs, not cell tabs")
	}
}

// TestCPSettingsRenderAndEdit pins the form: fields render from the
// loaded config, j/k move the field cursor (NOT the cell cursor),
// space toggles a bool into a dirty draft.
func TestCPSettingsRenderAndEdit(t *testing.T) {
	old := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI)
	defer lipgloss.SetColorProfile(old)

	m := cpSeed(t, fakeSource{})
	m = deliverCfg(t, m, cpConfig{Runner: fleet.PlacementRunnerConfig{Enabled: true, RestoreBatch: 25, RebalanceBatch: 10}, Placement: fleet.PlacementConfig{Strategy: "weighted"}})

	v := m.View()
	for _, want := range []string{"placement runner", "enabled", "restore batch", "25", "rebalance batch", "10"} {
		if !strings.Contains(v, want) {
			t.Errorf("settings form missing %q", want)
		}
	}

	// j moves the field cursor, not the cell cursor.
	before := m.cursor
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	m2 := next.(dashboardModel)
	if m2.cursor != before {
		t.Fatal("j on the settings form must not move the cell cursor")
	}
	if m2.cpFieldSel != 1 {
		t.Fatalf("j must move the field cursor: got %d", m2.cpFieldSel)
	}

	// Space toggles the selected bool (restore archives: false → true).
	next, _ = m2.Update(tea.KeyMsg{Type: tea.KeySpace})
	m3 := next.(dashboardModel)
	s := m3.cpSettings[testCP]
	if !s.dirty() {
		t.Fatal("toggling a bool must create a dirty draft")
	}
	if !s.draft.Runner.RestoreArchives {
		t.Fatal("the toggled field must flip in the draft")
	}
	if s.cfg.Runner.RestoreArchives {
		t.Fatal("the authoritative config must NOT change before apply")
	}
	if !strings.Contains(m3.View(), "unapplied change") {
		t.Fatal("the form must call out unapplied changes")
	}
}

// TestCPApplyFlow pins the whole write path: a → diff modal, y →
// SetPlacementRunner with the draft, response becomes authoritative,
// draft cleared.
func TestCPApplyFlow(t *testing.T) {
	rec := &fakeRunnerRecorder{}
	m := cpSeed(t, fakeSource{rec: rec})
	m = deliverCfg(t, m, cpConfig{Runner: fleet.PlacementRunnerConfig{Enabled: true, RestoreBatch: 25}})

	// Toggle rebalance (field index 4) on.
	m.cpFieldSel = 4
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeySpace})
	m = next.(dashboardModel)

	// a opens the confirm modal with the diff.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	m = next.(dashboardModel)
	if m.cpApply == nil {
		t.Fatal("a with a dirty draft must open the apply modal")
	}
	if len(m.cpApply.diffs) != 1 || !strings.Contains(m.cpApply.diffs[0], "rebalance: off → on") {
		t.Fatalf("modal must carry the diff, got %v", m.cpApply.diffs)
	}
	if !strings.Contains(m.View(), "APPLY SETTINGS") {
		t.Fatal("apply modal must render over the frame")
	}

	// y fires the write.
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	m = next.(dashboardModel)
	if m.cpApply != nil {
		t.Fatal("y must consume the modal")
	}
	if cmd == nil {
		t.Fatal("y must return the apply command")
	}
	msg := cmd() // run the async write synchronously
	applyMsg, ok := msg.(cpApplyMsg)
	if !ok {
		t.Fatalf("apply command must yield cpApplyMsg, got %T", msg)
	}
	if len(rec.setCalls) != 1 || !rec.setCalls[0].Runner.Rebalance {
		t.Fatalf("SetPlacementRunner must receive the draft, got %+v", rec.setCalls)
	}

	// The response becomes authoritative and the draft clears.
	next, _ = m.Update(applyMsg)
	m = next.(dashboardModel)
	s := m.cpSettings[testCP]
	if s.draft != nil {
		t.Fatal("a successful apply must clear the draft")
	}
	if !s.cfg.Runner.Rebalance {
		t.Fatal("the applied config must become authoritative")
	}
	if !strings.Contains(m.status, "✓ settings applied") {
		t.Fatalf("status must confirm the apply: %q", m.status)
	}
}

// TestCPApplyFailureKeepsDraft pins the failure posture: a failed
// write keeps the operator's edits so they can retry or discard.
func TestCPApplyFailureKeepsDraft(t *testing.T) {
	m := cpSeed(t, fakeSource{})
	m = deliverCfg(t, m, cpConfig{Runner: fleet.PlacementRunnerConfig{Enabled: true}})
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeySpace}) // toggle enabled → off
	m = next.(dashboardModel)

	next, _ = m.Update(cpApplyMsg{cp: testCP, err: errFake("cp unreachable")})
	m = next.(dashboardModel)
	s := m.cpSettings[testCP]
	if s.draft == nil {
		t.Fatal("a failed apply must keep the draft")
	}
	if !strings.Contains(m.status, "✗ apply failed") {
		t.Fatalf("status must surface the failure: %q", m.status)
	}
}

// TestCPIntEditAndDiscard pins int editing (enter → digits → enter)
// and x discarding the whole draft.
func TestCPIntEditAndDiscard(t *testing.T) {
	m := cpSeed(t, fakeSource{})
	m = deliverCfg(t, m, cpConfig{Runner: fleet.PlacementRunnerConfig{RestoreBatch: 25}})

	m.cpFieldSel = 2 // restore batch
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(dashboardModel)
	if !m.cpEditing {
		t.Fatal("enter on an int field must start an edit")
	}
	for _, d := range []string{"5", "0"} {
		next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(d)})
		m = next.(dashboardModel)
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(dashboardModel)
	if m.cpEditing {
		t.Fatal("enter must commit the int edit")
	}
	s := m.cpSettings[testCP]
	if s.draft == nil || s.draft.Runner.RestoreBatch != 50 {
		t.Fatalf("draft must hold the typed value, got %+v", s.draft)
	}

	// x discards everything.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	m = next.(dashboardModel)
	if m.cpSettings[testCP].draft != nil {
		t.Fatal("x must discard the draft")
	}
}

// TestCPRunNow pins the one-shot pass: r runs with the AUTHORITATIVE
// config (never the draft) and the result lands in lastRun.
func TestCPRunNow(t *testing.T) {
	rec := &fakeRunnerRecorder{}
	m := cpSeed(t, fakeSource{rec: rec, runResult: fleet.PlacementRunnerResult{
		Restore: &fleet.RestoreResult{Restored: []fleet.RestoredAccount{{}, {}}},
	}})
	m = deliverCfg(t, m, cpConfig{Runner: fleet.PlacementRunnerConfig{Enabled: true, RestoreBatch: 25}})

	// Make a dirty draft first — run must ignore it.
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeySpace}) // enabled → off (draft only)
	m = next.(dashboardModel)

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	m = next.(dashboardModel)
	if cmd == nil {
		t.Fatal("r must fire the runner command")
	}
	msg := cmd()
	if len(rec.runCalls) != 1 {
		t.Fatalf("run must call the client once, got %d", len(rec.runCalls))
	}
	if !rec.runCalls[0].Enabled {
		t.Fatal("run must use the authoritative config, not the dirty draft")
	}
	next, _ = m.Update(msg)
	m = next.(dashboardModel)
	if !strings.Contains(m.cpSettings[testCP].lastRun, "restored 2") {
		t.Fatalf("lastRun must summarize the pass: %q", m.cpSettings[testCP].lastRun)
	}
}

// TestCPSettingsUnreachable pins the read-only failure state.
func TestCPSettingsUnreachable(t *testing.T) {
	m := cpSeed(t, fakeSource{})
	next, _ := m.Update(cpSettingsMsg{cp: testCP, err: errFake("dial tcp: no route")})
	m = next.(dashboardModel)
	v := m.View()
	if !strings.Contains(v, "no route") {
		t.Fatal("the settings tab must surface the read error")
	}
	if !strings.Contains(v, "read-only") {
		t.Fatal("an unreachable CP must render read-only")
	}
	// Editing refuses cleanly.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeySpace})
	m2 := next.(dashboardModel)
	if m2.cpSettings[testCP].draft != nil {
		t.Fatal("editing must refuse while the config is unloaded/errored")
	}
}

// TestCPReaperValidation pins the pre-write guard: enabling the reaper
// without a TTL refuses at apply time with a clear message, before any
// modal or HTTP 400.
func TestCPReaperValidation(t *testing.T) {
	m := cpSeed(t, fakeSource{})
	m = deliverCfg(t, m, cpConfig{Placement: fleet.PlacementConfig{Strategy: "weighted"}})

	// Field 6 = reaper enabled. Toggle on; ttl stays 0.
	m.cpFieldSel = 6
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeySpace})
	m = next.(dashboardModel)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	m = next.(dashboardModel)
	if m.cpApply != nil {
		t.Fatal("apply must refuse an enabled reaper with no ttl")
	}
	if !strings.Contains(m.status, "ttl must be ≥ 1") {
		t.Fatalf("status must explain the validation failure: %q", m.status)
	}

	// Give it a TTL (field 7) and the apply modal opens.
	m.cpFieldSel = 7
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(dashboardModel)
	for _, d := range []string{"2", "4", "0"} {
		next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(d)})
		m = next.(dashboardModel)
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(dashboardModel)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	m = next.(dashboardModel)
	if m.cpApply == nil {
		t.Fatal("apply modal must open once the reaper draft is valid")
	}
	found := false
	for _, d := range m.cpApply.diffs {
		if strings.Contains(d, "reaper · ttl (minutes): 0 → 240") {
			found = true
		}
	}
	if !found {
		t.Fatalf("diff must show the reaper ttl change, got %v", m.cpApply.diffs)
	}
}

// TestCPStrategyCycleAndPinValidation pins the enum field: space
// cycles weighted→pinned, a pinned strategy without a cell refuses
// apply, and cycling the pinned-cell field walks the CP's cells.
func TestCPStrategyCycleAndPinValidation(t *testing.T) {
	m := cpSeed(t, fakeSource{})
	m = deliverCfg(t, m, cpConfig{Placement: fleet.PlacementConfig{Strategy: "weighted"}})

	m.cpFieldSel = 8 // strategy
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeySpace})
	m = next.(dashboardModel)
	s := m.cpSettings[testCP]
	if s.draft.Placement.Strategy != "pinned" {
		t.Fatalf("space must cycle strategy to pinned, got %q", s.draft.Placement.Strategy)
	}

	// Pinned without a cell refuses apply.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	m = next.(dashboardModel)
	if m.cpApply != nil {
		t.Fatal("apply must refuse pinned strategy without a pinned cell")
	}
	if !strings.Contains(m.status, "pinned") {
		t.Fatalf("status must explain: %q", m.status)
	}

	// Cycle the pinned-cell field: options come from this CP's cells.
	m.cpFieldSel = 9
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeySpace})
	m = next.(dashboardModel)
	s = m.cpSettings[testCP]
	if s.draft.Placement.PinnedCell != "aws-sandbox-usw2-dev" {
		t.Fatalf("pinned cell must cycle to the CP's registered cell, got %q", s.draft.Placement.PinnedCell)
	}

	// Now apply opens.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	m = next.(dashboardModel)
	if m.cpApply == nil {
		t.Fatal("apply modal must open for a valid pinned draft")
	}
}

// TestCPApplyWritesOnlyDirtySections pins the blast-radius contract:
// editing only the reaper must write ONLY the reaper section — an
// untouched section can never clobber a concurrent change to it.
func TestCPApplyWritesOnlyDirtySections(t *testing.T) {
	rec := &fakeRunnerRecorder{}
	m := cpSeed(t, fakeSource{rec: rec})
	m = deliverCfg(t, m, cpConfig{
		Runner:    fleet.PlacementRunnerConfig{Enabled: true, RestoreBatch: 25},
		Reaper:    fleet.ReaperConfig{Enabled: true, TTLMinutes: 240},
		Placement: fleet.PlacementConfig{Strategy: "weighted"},
	})

	// Edit only the reaper ttl (field 7).
	m.cpFieldSel = 7
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(dashboardModel)
	for _, d := range []string{"4", "8", "0"} {
		next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(d)})
		m = next.(dashboardModel)
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(dashboardModel)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	m = next.(dashboardModel)
	if m.cpApply == nil {
		t.Fatal("apply modal must open")
	}
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	m = next.(dashboardModel)
	_ = cmd()
	if len(rec.setSections) != 1 {
		t.Fatalf("exactly one write expected, got %d", len(rec.setSections))
	}
	got := rec.setSections[0]
	if !got.Reaper || got.Runner || got.Placement {
		t.Fatalf("only the reaper section must be written, got %+v", got)
	}
	if rec.setCalls[0].Reaper.TTLMinutes != 480 {
		t.Fatalf("written reaper ttl = %d, want 480", rec.setCalls[0].Reaper.TTLMinutes)
	}
}

// TestCPHeaderSurvivesLastCellDestroyed pins the regression: when the
// last cell of a control-plane group goes absent (destroyed), the CP
// header must STAY in the navigation pane — that's precisely when the
// operator needs the CP's Overview/Settings (checking placement state
// mid-rebuild). It previously vanished because headers were derived
// from active cells only.
func TestCPHeaderSurvivesLastCellDestroyed(t *testing.T) {
	acc := true
	live := []cellState{{
		name:         "aws-sandbox-usw2-dev",
		controlPlane: testCP,
		fleet:        &fleet.Cell{Name: "aws-sandbox-usw2-dev", Accepting: &acc},
	}}
	m := dashboardModel{
		ctx: t.Context(), cli: fakeSource{states: live},
		width: 120, height: 30,
		now: func() time.Time { return time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC) },
	}
	next, _ := m.Update(loadedMsg{states: live})
	m = next.(dashboardModel)
	// Park the cursor on the CP header (the state the operator was in).
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	m = next.(dashboardModel)
	if m.currentRow().kind != rowHeader || m.currentRow().cp != testCP {
		t.Fatal("setup: cursor must be on the CP header")
	}

	// The destroy completes; the refresh reloads the cell as absent
	// (deregistered).
	gone := []cellState{{name: "aws-sandbox-usw2-dev", controlPlane: testCP}}
	next, _ = m.Update(loadedMsg{states: gone})
	m = next.(dashboardModel)

	// The header must still exist and be reachable.
	foundHeader := false
	for _, r := range m.rows() {
		if r.kind == rowHeader && r.cp == testCP {
			foundHeader = true
		}
	}
	if !foundHeader {
		t.Fatal("the CP header must survive its last cell going absent")
	}
	// And its tabs must still render: walk the cursor to the header and
	// check the tab strip.
	for m.currentRow().kind != rowHeader {
		next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
		nm := next.(dashboardModel)
		if nm.cursor == m.cursor {
			t.Fatal("could not navigate to the CP header")
		}
		m = nm
	}
	if !strings.Contains(m.View(), "Settings") {
		t.Fatal("the CP header's tabs must still render with zero active cells")
	}
	// The absent cell is still listed below the separator.
	sep, cellBelow := false, false
	for _, r := range m.rows() {
		if r.kind == rowSeparator {
			sep = true
		}
		if sep && r.kind == rowCell {
			cellBelow = true
		}
	}
	if !sep || !cellBelow {
		t.Fatal("the destroyed cell must still list below the separator")
	}
}
