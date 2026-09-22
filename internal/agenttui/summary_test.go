package agenttui

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/witwave-ai/witself/internal/dashboard"
)

func TestSummaryProjectionRejectsMalformedAndDropsContent(t *testing.T) {
	raw, _ := encode(object{"summary": demoSummary()})
	var envelope map[string]any
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	s := envelope["summary"].(map[string]any)
	s["private_content"] = "PRIVATE_CANARY"
	categories := s["categories"].([]any)
	categories[1].(map[string]any)["label"] = "PRIVATE_CANARY"
	categories[1].(map[string]any)["title"] = "PRIVATE_CANARY"
	raw, _ = json.Marshal(envelope)
	p, err := decode(dashboard.ResourceSummary, raw)
	if err != nil {
		t.Fatal(err)
	}
	safe, _ := json.Marshal(p)
	if strings.Contains(string(safe), "PRIVATE_CANARY") {
		t.Fatal("summary retained source content")
	}
	for _, tc := range []struct {
		name   string
		change func(*dashboard.Summary)
	}{
		{"schema", func(s *dashboard.Summary) { s.Schema = "unknown" }},
		{"missing category", func(s *dashboard.Summary) { s.Categories = s.Categories[:6] }},
		{"duplicate category", func(s *dashboard.Summary) { s.Categories[0] = s.Categories[1] }},
		{"invented zero", func(s *dashboard.Summary) { s.Categories[1].Inventory.Count = nil }},
		{"negative count", func(s *dashboard.Summary) { v := int64(-1); s.Categories[1].Inventory.Count = &v }},
		{"overflow", func(s *dashboard.Summary) { v := summarySafeMax + 1; s.Categories[1].Inventory.Count = &v }},
		{"wrong dimension", func(s *dashboard.Summary) { s.Categories[1].Activity.Dimension = "email_received" }},
		{"short history", func(s *dashboard.Summary) { s.Categories[1].Activity.Bins = s.Categories[1].Activity.Bins[:23] }},
		{"negative bin", func(s *dashboard.Summary) { s.Categories[1].Activity.Bins[0] = -1 }},
		{"wrong total", func(s *dashboard.Summary) { *s.Categories[1].Activity.Total++ }},
		{"private verb", func(s *dashboard.Summary) { s.Recent[0].Action = "PRIVATE_CANARY" }},
		{"unknown checkpoint", func(s *dashboard.Summary) { s.Checkpoints[0].Key = "untrusted" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := demoSummary()
			tc.change(&s)
			raw, _ := encode(object{"summary": s})
			if _, err := decode(dashboard.ResourceSummary, raw); err == nil {
				t.Fatal("accepted malformed summary")
			}
		})
	}
}

func TestSummaryViewsNavigationAndPrivateBoundary(t *testing.T) {
	f := newFake()
	m := testModel(t, f)
	settle(t, m, m.refresh())
	if f.count(dashboard.ResourceSummary) != 1 {
		t.Fatal("overview did not fetch summary")
	}
	for _, key := range []string{"o", "l", "e"} {
		m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
		m.vp.GotoBottom()
		if strings.Contains(ansi.Strip(m.View()), "Wayfinding should feel") {
			t.Fatal("summary showed salient content")
		}
		m.vp.GotoTop()
	}
	if len(f.privateCalls) != 0 {
		t.Fatal("summary performed private reads")
	}
	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("o")})
	m.summaryRow = 4
	settle(t, m, m.key(tea.KeyMsg{Type: tea.KeyEnter}))
	if m.panel != 6 {
		t.Fatal("selected Secrets did not open")
	}
	before := f.count(dashboard.ResourceSummary)
	settle(t, m, m.refresh())
	if f.count(dashboard.ResourceSummary) != before {
		t.Fatal("summary fetched on other panel")
	}
	settle(t, m, m.navigate(0))
	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	_, iw, _, _ := m.layout()
	if iw == 0 {
		t.Fatal("details lost salient memory inventory")
	}
	settle(t, m, m.key(tea.KeyMsg{Type: tea.KeyEnter}))
	if m.panel != 3 {
		t.Fatal("details lost salient-memory navigation")
	}
}

func TestSummaryRefreshPreservesViewAndShowsStaleRecovery(t *testing.T) {
	f := newFake()
	m := testModel(t, f)
	settle(t, m, m.refresh())
	m.summaryView = 1
	m.summaryRow = 5
	f.read = func(ctx context.Context, r dashboard.ReadRequest) (json.RawMessage, error) {
		if r.Resource == dashboard.ResourceSummary {
			return nil, errors.New("PRIVATE_ERROR")
		}
		return f.Source.Read(ctx, r)
	}
	settle(t, m, m.refresh())
	if m.summaryView != 1 || m.summaryRow != 5 {
		t.Fatal("refresh moved summary selection")
	}
	m.vp.GotoTop()
	v := ansi.Strip(m.View())
	if !strings.Contains(v, "STALE") || strings.Contains(v, "PRIVATE_ERROR") {
		t.Fatal("stale status missing or leaked upstream error")
	}
	f.read = nil
	settle(t, m, m.refresh())
	m.vp.GotoTop()
	if strings.Contains(ansi.Strip(m.View()), "STALE") {
		t.Fatal("did not recover summary")
	}
}

func TestSummaryScrollIgnoresHiddenSalientSelection(t *testing.T) {
	f := newFake()
	m := testModel(t, f)
	settle(t, m, m.refresh())
	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")})
	m.vp.Height = 4
	selected := m.states[0].key
	before := f.count(dashboard.ResourceSummary)
	if cmd := m.key(tea.KeyMsg{Type: tea.KeyDown}); cmd != nil {
		t.Fatal("updates arrow started refresh")
	}
	if m.states[0].key != selected || f.count(dashboard.ResourceSummary) != before || m.vp.YOffset != 1 {
		t.Fatal("updates arrow did not only scroll")
	}
	m.summaryView = 1
	m.renderDetail(false)
	m.vp.SetYOffset(7)
	f.read = func(ctx context.Context, r dashboard.ReadRequest) (json.RawMessage, error) {
		if r.Resource == dashboard.ResourceSelf {
			s := demoSelf()
			s["salient_memories"] = []any{}
			return encode(s)
		}
		return f.Source.Read(ctx, r)
	}
	settle(t, m, m.refresh())
	if m.vp.YOffset != 7 {
		t.Fatalf("hidden salient update reset summary scroll: %d", m.vp.YOffset)
	}
}

func TestSummaryLayoutsAndASCII(t *testing.T) {
	m := testModel(t, NewDemoSource())
	settle(t, m, m.refresh())
	for _, theme := range themeNames {
		m.theme = theme
		for _, size := range [][2]int{{32, 20}, {60, 24}, {80, 24}, {120, 40}, {180, 50}} {
			m.width, m.height = size[0], size[1]
			for view := 0; view < 4; view++ {
				m.summaryView = view
				m.resize()
				m.renderDetail(false)
				lines := strings.Split(m.View(), "\n")
				if len(lines) != size[1] {
					t.Fatalf("height %v view%d", size, view)
				}
				for _, line := range lines {
					if ansi.StringWidth(line) > size[0] {
						t.Fatalf("overflow %v view%d", size, view)
					}
				}
			}
		}
	}
	raw, _ := encode(object{"summary": demoSummary()})
	o, _ := decode(dashboard.ResourceSummary, raw)
	a := obj(list(obj(o["summary"]), "categories")[1]["activity"])
	for _, timeline := range []bool{false, true} {
		for _, r := range summaryGraph(a, timeline, true) {
			if r > 127 {
				t.Fatal("non-ASCII graph")
			}
		}
	}
}
