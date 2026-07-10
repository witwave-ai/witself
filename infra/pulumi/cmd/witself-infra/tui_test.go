package main

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// fakeSource lets the model tests skip the control plane and the
// cloud identity API.
type fakeSource struct {
	states        []cellState
	controlPlanes map[string]controlPlaneInfo
	err           error
}

func (f fakeSource) load(_ context.Context, _ string) (loadResult, error) {
	return loadResult{states: f.states, controlPlanes: f.controlPlanes}, f.err
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
		"cells · 2",
		"aws-sandbox-usw2-dev",
		"gcp-lab-euw1-dev",
		"context",
		"123456789012",
		"witwave-sandbox",
		"matches config pin",
	} {
		if !strings.Contains(v, want) {
			t.Errorf("View() missing %q", want)
		}
	}
}

// TestDashboardCursorMovement pins j/k navigation between cells and
// the cursor's effect on the context pane.
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

// TestDestroyRequiresTypedCellName pins the destroy safety rule: the
// dialog only becomes confirmable once the operator has typed the
// full cell name, no shortcuts.
func TestDestroyRequiresTypedCellName(t *testing.T) {
	c := startConfirm(opDestroy, "aws-sandbox-usw2-dev", false)
	if c.canConfirm() {
		t.Fatal("destroy must not be confirmable with an empty typed field")
	}
	c.typed = "aws-sandbox"
	if c.canConfirm() {
		t.Fatal("destroy must not be confirmable on a prefix match")
	}
	c.typed = "aws-sandbox-usw2-dev"
	if !c.canConfirm() {
		t.Fatal("destroy must be confirmable once the name matches exactly")
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
	if !m2.previewSeen["aws-sandbox-usw2-dev"] {
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
		previewSeen: map[string]bool{"aws-sandbox-usw2-dev": true},
		op:          &opRun{kind: opDestroy, cell: "aws-sandbox-usw2-dev"},
	}
	next, _ := m.Update(opDoneMsg{cell: "aws-sandbox-usw2-dev", err: nil})
	m2 := next.(dashboardModel)
	if m2.previewSeen["aws-sandbox-usw2-dev"] {
		t.Fatal("successful destroy must invalidate previewSeen — else the next u applies a stale plan")
	}
}

// TestFailedPreviewDoesNotArmUp pins the fix: a preview that ERRORED
// must not leave previewSeen true, so up still refuses.
func TestFailedPreviewDoesNotArmUp(t *testing.T) {
	m := dashboardModel{
		ctx: context.Background(), cli: fakeSource{},
		now:         func() time.Time { return time.Time{} },
		previewSeen: map[string]bool{"aws-sandbox-usw2-dev": true},
		op:          &opRun{kind: opPreview, cell: "aws-sandbox-usw2-dev"},
	}
	next, _ := m.Update(opDoneMsg{cell: "aws-sandbox-usw2-dev", err: errFake("provider blew up")})
	m2 := next.(dashboardModel)
	if m2.previewSeen["aws-sandbox-usw2-dev"] {
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
