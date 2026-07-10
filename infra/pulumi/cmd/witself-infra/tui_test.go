package main

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// fakeSource lets the model tests skip the control plane and the
// cloud identity API.
type fakeSource struct {
	states []cellState
	err    error
}

func (f fakeSource) load(_ context.Context, _ string) ([]cellState, error) {
	return f.states, f.err
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
	m := dashboardModel{
		ctx:    context.Background(),
		cli:    fakeSource{states: states},
		width:  120,
		height: 24,
		states: states,
		now:    func() time.Time { return time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC) },
	}
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
	m := dashboardModel{
		ctx: context.Background(), cli: fakeSource{states: states},
		width: 120, height: 24, states: states,
		now: func() time.Time { return time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC) },
	}
	// cursor=0 shows the first cell's identity; the second cell's
	// account isn't in the context pane yet.
	if strings.Contains(m.View(), "witwave-lab") {
		t.Fatal("second cell's identity must not render while cursor=0")
	}
	// j = move down; the second cell's identity now shows.
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	m2 := next.(dashboardModel)
	if m2.cursor != 1 {
		t.Fatalf("j: cursor = %d, want 1", m2.cursor)
	}
	if !strings.Contains(m2.View(), "witwave-lab") {
		t.Fatal("moving to gcp cell must show its identity in the context pane")
	}
	// j at the end stays put.
	next, _ = m2.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if next.(dashboardModel).cursor != 1 {
		t.Fatal("j past end must clamp")
	}
	// k moves back.
	next, _ = m2.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	if next.(dashboardModel).cursor != 0 {
		t.Fatal("k must move up")
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
	m := dashboardModel{
		ctx: context.Background(), cli: fakeSource{states: states},
		width: 120, height: 24, states: states,
		now: func() time.Time { return time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC) },
	}
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
