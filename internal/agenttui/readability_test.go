package agenttui

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
	"github.com/witwave-ai/witself/internal/dashboard"
)

func TestSummaryFocusVisibleAndRerendered(t *testing.T) {
	for _, size := range [][2]int{{32, 18}, {48, 20}, {60, 24}, {80, 24}, {120, 40}} {
		for _, view := range []int{0, 1} {
			t.Run(fmt.Sprintf("%dx%d/view%d", size[0], size[1], view), func(t *testing.T) {
				f := newFake()
				m := testModel(t, f)
				settle(t, m, m.refresh())
				m.summaryView, m.summaryRow = view, 3
				m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
				m.vp.GotoTop()
				before := m.vp.TotalLineCount()
				m.Update(key("tab"))
				if m.focus != 1 || m.summaryRow != 3 || before != m.vp.TotalLineCount() {
					t.Fatal("Tab changed selection or layout")
				}
				if !strings.HasPrefix(m.footer(), "Scroll") {
					t.Fatal("missing persistent scroll mode")
				}
				m.Update(tea.KeyMsg{Type: tea.KeyDown})
				if m.summaryRow != 3 || m.vp.YOffset != min(1, max(0, m.vp.TotalLineCount()-m.vp.Height)) {
					t.Fatal("scroll mode changed category or failed to scroll")
				}
				// Inspect the actual updated viewport, without another render or
				// Snapshot call that could conceal stale content after Tab.
				m.vp.SetYOffset(m.actionOffsets[3])
				if strings.Contains(ansi.Strip(m.vp.View()), "> MEM") {
					t.Fatal("scroll mode retained category focus marker")
				}
				m.notice = "Private value hidden after 30 seconds."
				m.vp.GotoBottom()
				lines := strings.Split(ansi.Strip(m.View()), "\n")
				if !strings.Contains(lines[len(lines)-2], "Scroll") {
					t.Fatal("focus scrolled out of view")
				}
				if size[0] >= 80 && !strings.Contains(lines[len(lines)-1], m.notice) {
					t.Fatal("focus replaced private lifecycle notice")
				}
				m.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
				m.vp.SetYOffset(m.actionOffsets[3])
				if m.summaryRow != 3 || !strings.Contains(ansi.Strip(m.vp.View()), "> MEM") || !strings.HasPrefix(m.footer(), "Categories") {
					t.Fatal("return to categories did not restore focused row")
				}
				m.Update(tea.KeyMsg{Type: tea.KeyDown})
				if m.summaryRow != 4 || len(f.privateCalls) != 0 || f.count(dashboard.ResourceSummary) != 1 {
					t.Fatal("focus transition changed read/navigation contract")
				}
			})
		}
	}
}

func TestCompactNavigationNamesAndFit(t *testing.T) {
	m := testModel(t, NewDemoSource())
	labels := []string{"Overview", "Transcr.", "Facts", "Memories", "Convs.", "Email", "Secrets"}
	for _, width := range []int{32, 60, 73, 74, 79, 80, 99} {
		for panel := range panelNames {
			m.panel = panel
			view := strings.Split(ansi.Strip(m.Snapshot(width, 24)), "\n")
			nav := view[2]
			if width >= 74 {
				for i, name := range labels {
					if !strings.Contains(nav, fmt.Sprintf("%d %s", i+1, name)) {
						t.Fatalf("width %d lost %s: %s", width, name, nav)
					}
				}
			} else if !strings.Contains(nav, panelNames[panel]) || !strings.Contains(nav, "1–7") {
				t.Fatalf("width %d lost active-panel fallback: %s", width, nav)
			}
			if strings.Contains(nav, "…") {
				t.Fatalf("navigation was truncated: %s", nav)
			}
		}
	}
}

func TestSummaryCompactRowsPreserveQuantitiesAndEveryBin(t *testing.T) {
	for _, large := range []bool{false, true} {
		f := newFake()
		f.read = func(ctx context.Context, r dashboard.ReadRequest) (json.RawMessage, error) {
			if r.Resource != dashboard.ResourceSummary {
				return f.Source.Read(ctx, r)
			}
			s := demoSummary()
			// Exercise both zero and the largest valid total, through the real
			// projection, including a large bounded inventory count.
			for _, i := range []int{1, 5} {
				n := int64(0)
				if large {
					n = summarySafeMax
				}
				s.Categories[i].Activity.Total = &n
				s.Categories[i].Inventory.Count = &n
				for h := range s.Categories[i].Activity.Bins {
					v := int64(0)
					if h == 23 {
						v = n
					}
					s.Categories[i].Activity.Bins[h] = &v
				}
			}
			return encode(object{"summary": s})
		}
		m := testModel(t, f)
		settle(t, m, m.refresh())
		s := obj(m.states[0].data[dashboard.ResourceSummary]["summary"])
		categories := list(s, "categories")
		if len(categories) != 7 {
			t.Fatal("synthetic quantities rejected by projection")
		}
		for _, width := range []int{12, 28, 38, 56, 60, 64, 68, 72, 76, 80, 98, 116} {
			for _, ascii := range []bool{false, true} {
				for _, view := range []int{0, 1} {
					m.summaryView, m.summaryASCII = view, ascii
					m.actionOffsets = nil
					d := &detailWriter{m: m, width: width}
					m.summaryOverview(d)
					units := []string{"ops", "entries", "deliveries", "changes", "accesses", "accepted sends", "sent"}
					for i, category := range categories {
						end := len(d.lines)
						if i < 6 {
							end = m.actionOffsets[i+1]
						}
						row := ansi.Strip(strings.Join(d.lines[m.actionOffsets[i]:end], "\n"))
						compact := strings.Join(strings.Fields(row), "")
						a := obj(category["activity"])
						for _, want := range []string{str(category, "code") + str(category, "label"), str(a, "total") + units[i], summaryGraph(a, view == 1, ascii)} {
							if !strings.Contains(compact, strings.Join(strings.Fields(want), "")) {
								t.Fatalf("width %d view %d large %t lost %q in %s", width, view, large, want, row)
							}
						}
					}
					for _, line := range d.lines {
						if ansi.StringWidth(line) > width {
							t.Fatalf("content needs cropping at width %d: %s", width, line)
						}
					}
				}
			}
		}
	}
}

func TestInventoryEmptyRequiresSuccessfulRead(t *testing.T) {
	for _, tc := range []struct{ status, want string }{
		{"ready", "No items yet."}, {"loading", "Loading inventory"},
		{"unavailable", "Temporarily unavailable"}, {"unsupported", "Unsupported on this cell"},
		{"disabled", "Disabled by current policy"}, {"unenrolled", "Agent is not enrolled"},
		{"stale", "Stale · last loaded data"},
	} {
		for _, filtered := range []bool{false, true} {
			m := testModel(t, NewDemoSource())
			m.panel = 2
			m.states[2].status[dashboard.ResourceFacts] = tc.status
			want := tc.want
			if filtered {
				m.states[2].filter = "no match"
				if tc.status == "ready" {
					want = "No matching items."
				}
			}
			inventory := ansi.Strip(m.inventoryView(40, 18))
			if !strings.Contains(inventory, want) || (tc.status != "ready" && strings.Contains(inventory, "No items")) {
				t.Fatalf("%s filtered=%t: %s", tc.status, filtered, inventory)
			}
			// At narrow widths the detail is the only visible pane.
			m.Update(tea.WindowSizeMsg{Width: 42, Height: 18})
			if view := ansi.Strip(m.View()); !strings.Contains(view, want) {
				t.Fatalf("narrow %s: %s", tc.status, view)
			}
		}
	}
}

func TestSummaryCompactUnavailableAndDisabledAreDistinct(t *testing.T) {
	f := newFake()
	f.read = func(ctx context.Context, r dashboard.ReadRequest) (json.RawMessage, error) {
		if r.Resource != dashboard.ResourceSummary {
			return f.Source.Read(ctx, r)
		}
		s := demoSummary()
		for i, status := range map[int]string{1: "unavailable", 5: "disabled"} {
			s.Categories[i].Activity = dashboard.SummaryActivity{Status: status, Dimension: summaryKinds[i].dimension, Unit: summaryKinds[i].unit}
			s.Categories[i].Inventory.Status = status
			s.Categories[i].Inventory.Count = nil
		}
		return encode(object{"summary": s})
	}
	m := testModel(t, f)
	settle(t, m, m.refresh())
	for _, width := range []int{42, 60, 80, 120} {
		m.Update(tea.WindowSizeMsg{Width: width, Height: 24})
		for i, want := range map[int]string{1: "Unavailable", 5: "Disabled"} {
			m.vp.SetYOffset(m.actionOffsets[i])
			if v := ansi.Strip(m.vp.View()); !strings.Contains(v, want) {
				t.Fatalf("width %d lost %s: %s", width, want, v)
			}
		}
	}
}

func TestHeaderHealthStylesAndFooterControls(t *testing.T) {
	profile := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(profile) })
	m := testModel(t, NewDemoSource())
	settle(t, m, m.refresh())
	for _, theme := range themeNames {
		m.theme = theme
		for _, state := range []string{"ready", "stale", "unavailable"} {
			for _, paused := range []bool{false, true} {
				m.selfStatus, m.paused = state, paused
				color, label, bold := m.colors().accent, "● live", false
				if paused {
					color, label = m.colors().dim, "Ⅱ paused"
				}
				if state != "ready" {
					color, label, bold = m.colors().danger, "! "+state, true
				}
				prefix, _, _ := strings.Cut(m.style(color).Bold(bold).Render("x"), "x")
				header := strings.Split(m.View(), "\n")[0]
				if !strings.Contains(header, prefix+label) {
					t.Fatalf("%s %s paused=%t lost health label/style: %q", theme, state, paused, header)
				}
			}
		}
	}
	for _, width := range []int{32, 42, 48, 60, 80, 120} {
		m.width = width
		for panel := range panelNames {
			m.panel = panel
			footer := m.footer()
			if ansi.StringWidth(footer) > width || !strings.Contains(footer, "? help") || !strings.Contains(footer, "q quit") {
				t.Fatalf("width %d panel %d invalid footer: %s", width, panel, footer)
			}
		}
	}
	m.panel, m.overlay = 0, "help"
	m.helpScroll = 10000
	if !strings.Contains(ansi.Strip(m.overlayView(76, 30)), "https://self.witwave.ai/legal") {
		t.Fatal("Legal disappeared from help")
	}
	m.width = 80
	for _, overlay := range []string{"help", "theme", "console"} {
		m.overlay = overlay
		lines := strings.Split(ansi.Strip(m.View()), "\n")
		if strings.Contains(lines[len(lines)-1], "p pause") || strings.Contains(lines[len(lines)-1], "Enter open") {
			t.Fatalf("%s shows inactive workspace controls", overlay)
		}
	}
}

func TestConsoleRecoveryHintsInBothContexts(t *testing.T) {
	for _, tc := range []struct {
		action  consoleAction
		state   ConsoleState
		control string
	}{
		{consoleStop, ConsoleRunning, "x stop"},
		{consoleOpen, ConsoleRunning, "b open"},
		{consoleCheck, ConsoleUnavailable, "r check status"},
	} {
		m := consoleTestModel(t, &fakeConsole{})
		m.width, m.height = 80, 24
		m.resize()
		msg := consoleMsg{action: tc.action, status: ConsoleStatus{State: tc.state}, failed: true}
		m.notice, m.console.notice = consoleOutcome(msg), consoleOutcome(msg)
		for _, overlay := range []string{"", "console"} {
			m.overlay = overlay
			v := ansi.Strip(m.View())
			if !strings.Contains(v, "Web console: "+tc.control) || strings.Contains(v, "Press w, then") || strings.Contains(v, "Press x") {
				t.Fatalf("overlay %q ambiguous recovery: %s", overlay, v)
			}
		}
	}
}

func TestReadabilityThemeSnapshots(t *testing.T) {
	profile := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(profile) })
	m := testModel(t, NewDemoSource())
	settle(t, m, m.refresh())
	for _, theme := range themeNames {
		m.theme = theme
		for _, size := range [][2]int{{80, 24}, {120, 40}} {
			for _, focus := range []int{0, 1} {
				m.focus = focus
				m.vp.GotoTop()
				v := m.Snapshot(size[0], size[1])
				plain := ansi.Strip(v)
				for _, want := range []string{"Attention", "24 hourly bins", "Current hour partial", "OPS/MEM partial", "per-row scales", "no combined total", "recent inventory bounded", "ops", "entries", "deliveries", "changes", "accesses", "accepted sends", "sent", "? help", "q quit"} {
					if !strings.Contains(plain, want) {
						t.Fatalf("%s %v missing %q", theme, size, want)
					}
				}
				if testing.Verbose() {
					t.Logf("%s %dx%d focus=%d\n%s", theme, size[0], size[1], focus, v)
				}
			}
		}
	}
}
