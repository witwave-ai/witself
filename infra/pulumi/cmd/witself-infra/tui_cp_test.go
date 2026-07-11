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
	m = deliverCfg(t, m, cpConfig{Runner: fleet.PlacementRunnerConfig{Enabled: true, RestoreBatch: 4, RebalanceBatch: 1}, Placement: fleet.PlacementConfig{Strategy: "weighted"}})

	v := m.View()
	for _, want := range []string{"placement runner", "enabled", "restore batch", "4", "rebalance batch", "1"} {
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

// pressApply presses 'a' AND delivers the pre-apply re-read that now
// gates the confirm modal. The re-read carries the current cfg so the
// diff is unchanged (the interesting concurrent-write case is a
// separate test).
func pressApply(t *testing.T, m dashboardModel) dashboardModel {
	t.Helper()
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	m = next.(dashboardModel)
	s := m.cpSettings[testCP]
	next, _ = m.Update(cpSettingsMsg{cp: testCP, cfg: s.cfg, gen: s.readGen, purpose: cpReadPreApply})
	return next.(dashboardModel)
}

// TestCPApplyFlow pins the write path: a → pre-apply re-read → diff
// modal → y → SetPlacementRunner with the draft → response becomes
// authoritative → draft cleared.
func TestCPApplyFlow(t *testing.T) {
	rec := &fakeRunnerRecorder{}
	m := cpSeed(t, fakeSource{rec: rec})
	m = deliverCfg(t, m, cpConfig{Runner: fleet.PlacementRunnerConfig{Enabled: true, RestoreBatch: 4, RebalanceBatch: 1}, Placement: fleet.PlacementConfig{Strategy: "weighted"}})

	// Toggle rebalance (field index 4) on.
	m.cpFieldSel = 4
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeySpace})
	m = next.(dashboardModel)

	m = pressApply(t, m)
	if m.cpApply == nil {
		t.Fatal("a with a dirty draft must open the apply modal (after re-read)")
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
	msg := cmd()
	applyMsg, ok := msg.(cpApplyMsg)
	if !ok {
		t.Fatalf("apply command must yield cpApplyMsg, got %T", msg)
	}
	if len(rec.setCalls) != 1 || !rec.setCalls[0].Runner.Rebalance {
		t.Fatalf("SetPlacementRunner must receive the draft, got %+v", rec.setCalls)
	}

	next, _ = m.Update(applyMsg)
	m = next.(dashboardModel)
	s := m.cpSettings[testCP]
	if s.draft != nil {
		t.Fatal("a successful apply with no mid-apply edits must clear the draft")
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
	m = deliverCfg(t, m, cpConfig{Runner: fleet.PlacementRunnerConfig{Enabled: true, RestoreBatch: 4, RebalanceBatch: 1}})
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
	m = deliverCfg(t, m, cpConfig{Runner: fleet.PlacementRunnerConfig{RestoreBatch: 4, RebalanceBatch: 1}})

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

	// x opens a discard-confirm modal now (asymmetric-with-apply footgun fix).
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	m = next.(dashboardModel)
	if m.cpDiscard == nil {
		t.Fatal("x must open the discard-confirm modal, not delete instantly")
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	m = next.(dashboardModel)
	if m.cpSettings[testCP].draft != nil {
		t.Fatal("y on the discard modal must clear the draft")
	}
}

// TestCPRunNow pins the one-shot pass: r runs with the AUTHORITATIVE
// config (never the draft) and the result lands in lastRun.
func TestCPRunNow(t *testing.T) {
	rec := &fakeRunnerRecorder{}
	m := cpSeed(t, fakeSource{rec: rec, runResult: fleet.PlacementRunnerResult{
		Restore: &fleet.RestoreResult{Restored: []fleet.RestoredAccount{{}, {}}},
	}})
	m = deliverCfg(t, m, cpConfig{Runner: fleet.PlacementRunnerConfig{Enabled: true, RestoreBatch: 4, RebalanceBatch: 1}})

	// Make a dirty draft first — run must ignore it.
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeySpace}) // enabled → off (draft only)
	m = next.(dashboardModel)

	// r now opens a confirm modal (unconfirmed-mutation fix).
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	m = next.(dashboardModel)
	if m.cpRun == nil {
		t.Fatal("r must open the run-confirm modal, not fire instantly")
	}
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	m = next.(dashboardModel)
	if cmd == nil {
		t.Fatal("y on the run modal must fire the runner command")
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
	m = deliverCfg(t, m, cpConfig{Runner: fleet.PlacementRunnerConfig{RestoreBatch: 4, RebalanceBatch: 1}, Placement: fleet.PlacementConfig{Strategy: "weighted"}})

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
	m = pressApply(t, m)
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
	m = deliverCfg(t, m, cpConfig{Runner: fleet.PlacementRunnerConfig{RestoreBatch: 4, RebalanceBatch: 1}, Placement: fleet.PlacementConfig{Strategy: "weighted"}})

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
	m = pressApply(t, m)
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
		Runner:    fleet.PlacementRunnerConfig{Enabled: true, RestoreBatch: 4, RebalanceBatch: 1},
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
	m = pressApply(t, m)
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

// TestCPApplyReReadsBeforeDiff pins the stale-write fix: pressing 'a'
// re-reads authoritatively, so a concurrent server-side change lands
// in the modal's diff (or shifts what the operator sees) before y.
func TestCPApplyReReadsBeforeDiff(t *testing.T) {
	m := cpSeed(t, fakeSource{})
	m = deliverCfg(t, m, cpConfig{Runner: fleet.PlacementRunnerConfig{Enabled: true, RestoreBatch: 4, RebalanceBatch: 1}, Placement: fleet.PlacementConfig{Strategy: "weighted"}})

	// Operator toggles enabled off in a draft.
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeySpace})
	m = next.(dashboardModel)
	// a fires the re-read; modal doesn't open yet.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	m = next.(dashboardModel)
	if m.cpApply != nil {
		t.Fatal("modal must NOT open before the pre-apply re-read lands")
	}
	if !m.cpSettings[testCP].inflight {
		t.Fatal("a must mark a read in flight")
	}
	// Meanwhile another operator changed RebalanceBatch on the server.
	// The re-read carries the FRESH cfg. The modal must now include the
	// server-side change in its diff (or highlight it), not the stale one.
	s := m.cpSettings[testCP]
	fresh := s.cfg
	fresh.Runner.RebalanceBatch = 5 // concurrent change
	next, _ = m.Update(cpSettingsMsg{cp: testCP, cfg: fresh, gen: s.readGen, purpose: cpReadPreApply})
	m = next.(dashboardModel)
	if m.cpApply == nil {
		t.Fatal("modal must open after the re-read lands")
	}
	// The diff must now show BOTH the operator's toggle AND the concurrent change.
	haveEnabled, haveBatch := false, false
	for _, d := range m.cpApply.diffs {
		if strings.Contains(d, "enabled: on → off") {
			haveEnabled = true
		}
		if strings.Contains(d, "rebalance batch: 5 → 1") {
			haveBatch = true
		}
	}
	if !haveEnabled {
		t.Fatal("modal must still show the operator's edit")
	}
	if !haveBatch {
		t.Fatal("modal must surface the concurrent server-side change so 'y' isn't silent")
	}
}

// TestCPReadFailureKeepsCfg pins that a failed background read does
// NOT overwrite the last-good authoritative config with a zero struct.
func TestCPReadFailureKeepsCfg(t *testing.T) {
	m := cpSeed(t, fakeSource{})
	good := cpConfig{Runner: fleet.PlacementRunnerConfig{Enabled: true, RestoreBatch: 4, RebalanceBatch: 1}, Placement: fleet.PlacementConfig{Strategy: "weighted"}}
	m = deliverCfg(t, m, good)
	s := m.cpSettings[testCP]
	// A failed refresh arrives.
	next, _ := m.Update(cpSettingsMsg{cp: testCP, err: errFake("cp unreachable"), gen: s.readGen, purpose: cpReadBackground})
	m = next.(dashboardModel)
	s = m.cpSettings[testCP]
	if s.cfg != good {
		t.Fatalf("failed read must NOT clobber cfg, got %+v", s.cfg)
	}
	if s.err == nil {
		t.Fatal("failed read must record the error")
	}
	// 'a' now refuses (not editReady) rather than posting the zero draft.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	m = next.(dashboardModel)
	if m.cpApply != nil {
		t.Fatal("apply must refuse in an error state — no dangerous diff-against-zero modal")
	}
}

// TestCPStaleReadDropped pins the gen fence: a read fired against an
// older gen is dropped so an intervening apply/discard can't be
// silently reverted.
func TestCPStaleReadDropped(t *testing.T) {
	m := cpSeed(t, fakeSource{})
	orig := cpConfig{Runner: fleet.PlacementRunnerConfig{Enabled: true, RestoreBatch: 4, RebalanceBatch: 1}, Placement: fleet.PlacementConfig{Strategy: "weighted"}}
	m = deliverCfg(t, m, orig)
	s := m.cpSettings[testCP]
	staleGen := s.readGen
	// Discard fires readGen++ (state-changing event).
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeySpace})
	m = next.(dashboardModel)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")}) // opens confirm
	m = next.(dashboardModel)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")}) // confirms discard
	m = next.(dashboardModel)
	// A stale read (gen from before the discard) lands with different data.
	stale := orig
	stale.Runner.Enabled = false
	next, _ = m.Update(cpSettingsMsg{cp: testCP, cfg: stale, gen: staleGen, purpose: cpReadBackground})
	m = next.(dashboardModel)
	if m.cpSettings[testCP].cfg != orig {
		t.Fatalf("stale read must be dropped, cfg = %+v", m.cpSettings[testCP].cfg)
	}
}

// TestCPRunNeedsConfirm pins the confirm gate on 'r': one keypress can
// no longer fire an account-moving pass.
func TestCPRunNeedsConfirm(t *testing.T) {
	rec := &fakeRunnerRecorder{}
	m := cpSeed(t, fakeSource{rec: rec})
	m = deliverCfg(t, m, cpConfig{Runner: fleet.PlacementRunnerConfig{Enabled: true, RestoreArchives: true, RestoreBatch: 4, RebalanceBatch: 1}, Placement: fleet.PlacementConfig{Strategy: "weighted"}})

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	m = next.(dashboardModel)
	if m.cpRun == nil {
		t.Fatal("r must open a confirm modal, not fire directly")
	}
	if len(rec.runCalls) != 0 {
		t.Fatal("no runner pass may fire before the confirm")
	}
	// esc cancels.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = next.(dashboardModel)
	if m.cpRun != nil {
		t.Fatal("esc must cancel the run modal")
	}
}

// TestCPRunInflightGuard pins that repeat r presses can't fire
// concurrent passes.
func TestCPRunInflightGuard(t *testing.T) {
	rec := &fakeRunnerRecorder{}
	m := cpSeed(t, fakeSource{rec: rec})
	m = deliverCfg(t, m, cpConfig{Runner: fleet.PlacementRunnerConfig{Enabled: true, RestoreArchives: true, RestoreBatch: 4, RebalanceBatch: 1}, Placement: fleet.PlacementConfig{Strategy: "weighted"}})

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	m = next.(dashboardModel)
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	m = next.(dashboardModel)
	_ = cmd // dispatched
	// Second r while in flight refuses cleanly.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	m = next.(dashboardModel)
	if m.cpRun != nil {
		t.Fatal("r must refuse while a pass is in flight, not open another modal")
	}
	if !strings.Contains(m.status, "already in flight") {
		t.Fatalf("status must explain the refusal: %q", m.status)
	}
}

// TestCPDiscardNeedsConfirm pins the x-confirm modal.
func TestCPDiscardNeedsConfirm(t *testing.T) {
	m := cpSeed(t, fakeSource{})
	m = deliverCfg(t, m, cpConfig{Runner: fleet.PlacementRunnerConfig{Enabled: true, RestoreBatch: 4, RebalanceBatch: 1}, Placement: fleet.PlacementConfig{Strategy: "weighted"}})
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeySpace})
	m = next.(dashboardModel)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	m = next.(dashboardModel)
	if m.cpDiscard == nil {
		t.Fatal("x must open the discard-confirm modal")
	}
	// esc keeps the draft.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = next.(dashboardModel)
	if m.cpSettings[testCP].draft == nil {
		t.Fatal("esc on the discard modal must keep the draft")
	}
}

// TestCPQuitGuardOnDirtyDraft pins that q refuses to quit silently
// with unapplied fleet-automation edits.
func TestCPQuitGuardOnDirtyDraft(t *testing.T) {
	m := cpSeed(t, fakeSource{})
	m = deliverCfg(t, m, cpConfig{Runner: fleet.PlacementRunnerConfig{Enabled: true, RestoreBatch: 4, RebalanceBatch: 1}, Placement: fleet.PlacementConfig{Strategy: "weighted"}})
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeySpace}) // dirty
	m = next.(dashboardModel)
	// Un-focus so 'q' hits the normal handler.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc}) // esc backs focus out
	m = next.(dashboardModel)
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	m = next.(dashboardModel)
	if cmd != nil {
		t.Fatal("q with a dirty draft must NOT quit directly")
	}
	if m.cpQuit == nil {
		t.Fatal("q must open a quit-confirm modal instead of dropping edits")
	}
}

// TestCPBatchClampValidation pins the pre-write clamp guard.
func TestCPBatchClampValidation(t *testing.T) {
	m := cpSeed(t, fakeSource{})
	m = deliverCfg(t, m, cpConfig{Runner: fleet.PlacementRunnerConfig{Enabled: true, RestoreBatch: 4, RebalanceBatch: 1}, Placement: fleet.PlacementConfig{Strategy: "weighted"}})
	// Edit restore batch to 50 (out of server 1..10 range).
	m.cpFieldSel = 2
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(dashboardModel)
	for _, d := range []string{"5", "0"} {
		next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(d)})
		m = next.(dashboardModel)
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(dashboardModel)
	// Apply refuses.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	m = next.(dashboardModel)
	if m.cpApply != nil {
		t.Fatal("apply must refuse out-of-clamp values before the modal")
	}
	if !strings.Contains(m.status, "restore batch must be") {
		t.Fatalf("status must explain: %q", m.status)
	}
}

// TestCPMidApplyEditsSurvive pins that edits made DURING an apply are
// preserved (rebased onto the response) rather than silently dropped.
func TestCPMidApplyEditsSurvive(t *testing.T) {
	m := cpSeed(t, fakeSource{})
	m = deliverCfg(t, m, cpConfig{Runner: fleet.PlacementRunnerConfig{Enabled: true, RestoreBatch: 4, RebalanceBatch: 1}, Placement: fleet.PlacementConfig{Strategy: "weighted"}})
	// Toggle enabled off (draft).
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeySpace})
	m = next.(dashboardModel)
	m = pressApply(t, m)
	// Fire y.
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	m = next.(dashboardModel)
	_ = cmd
	// While apply is in flight, operator edits ANOTHER field.
	m.cpFieldSel = 8 // strategy
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeySpace})
	m = next.(dashboardModel)
	// Response lands with the first change applied.
	response := m.cpSettings[testCP].cfg
	response.Runner.Enabled = false
	next, _ = m.Update(cpApplyMsg{cp: testCP, cfg: response})
	m = next.(dashboardModel)
	s := m.cpSettings[testCP]
	if s.cfg.Runner.Enabled {
		t.Fatal("response must become authoritative")
	}
	if s.draft == nil {
		t.Fatal("mid-apply strategy edit must survive on the draft")
	}
	if s.draft.Placement.Strategy != "pinned" {
		t.Fatalf("mid-apply edit must persist, got strategy=%q", s.draft.Placement.Strategy)
	}
}

// TestCPCursorFollowsCPHeaderAcrossRefresh pins the cursor-yank fix:
// a mid-session load must keep the cursor on the CP header the
// operator was editing settings on, not drop them onto the first cell.
func TestCPCursorFollowsCPHeaderAcrossRefresh(t *testing.T) {
	m := cpSeed(t, fakeSource{})
	if m.currentRow().kind != rowHeader {
		t.Fatal("seed: cursor must be on the CP header")
	}
	// A refresh delivers the same states — but rows() is rebuilt from
	// scratch. Cursor must stay on the header, not fall onto the cell.
	acc := true
	states := []cellState{{name: "aws-sandbox-usw2-dev", controlPlane: testCP, fleet: &fleet.Cell{Accepting: &acc}}}
	next, _ := m.Update(loadedMsg{states: states})
	m = next.(dashboardModel)
	if m.currentRow().kind != rowHeader || m.currentRow().cp != testCP {
		t.Fatalf("cursor must stick to the CP header across refresh, kind=%d cp=%q", m.currentRow().kind, m.currentRow().cp)
	}
}

// TestCPSelfHostedArrowsInert pins that ←/→ on the self-hosted header
// (renders untabbed) don't silently mutate cpActiveTab.
func TestCPSelfHostedArrowsInert(t *testing.T) {
	acc := true
	states := []cellState{{name: "self-a", controlPlane: "", fleet: &fleet.Cell{Accepting: &acc}}}
	m := dashboardModel{
		ctx: t.Context(), cli: fakeSource{states: states},
		width: 120, height: 30,
		now: func() time.Time { return time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC) },
	}
	next, _ := m.Update(loadedMsg{states: states})
	m = next.(dashboardModel)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	m = next.(dashboardModel)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = next.(dashboardModel)
	before := m.cpActiveTab
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRight})
	m = next.(dashboardModel)
	if m.cpActiveTab != before {
		t.Fatalf("→ on a self-hosted (untabbed) header must NOT mutate cpActiveTab, got %d → %d", before, m.cpActiveTab)
	}
}
