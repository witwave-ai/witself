package agenttui

import (
	"context"
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

func TestListPanelFocusGeometry(t *testing.T) {
	for panel := 2; panel <= 6; panel++ {
		for _, control := range []string{"enter", "tab"} {
			for _, size := range [][2]int{{80, 24}, {120, 40}} {
				t.Run(fmt.Sprintf("%s/%s/%dx%d", panelNames[panel], control, size[0], size[1]), func(t *testing.T) {
					m := testModel(t, NewDemoSource())
					openPanel(t, m, panel)
					m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
					before := m.View()
					rail, inventory, detail, h := m.layout()
					if inventory == 0 {
						t.Fatal("fixture needs expanded inventory")
					}
					m.key(key(control))
					r, i, d, ch := m.layout()
					ih, dh := m.panelHeights(panel, ch)
					if m.focus != 1 || r != rail || i != 0 || d != size[0]-rail || ih != 5 || dh != h-5 || m.vp.Width != d-4 || m.vp.Height != dh-2 {
						t.Fatalf("incorrect collapsed geometry: focus=%d layout=%d/%d/%d/%d heights=%d/%d viewport=%d/%d", m.focus, r, i, d, ch, ih, dh, m.vp.Width, m.vp.Height)
					}
					if !strings.Contains(ansi.Strip(m.inventoryView(d-4, 3)), "› ") {
						t.Fatal("collapsed inventory lost selected row")
					}
					m.key(key("esc"))
					_, i, d, _ = m.layout()
					if m.focus != 0 || i != inventory || d != detail || m.View() != before {
						t.Fatal("Esc did not restore inventory geometry/render")
					}
				})
			}
		}
	}
}

func TestListReaderPositionAcrossFocusAndPolling(t *testing.T) {
	for panel := 2; panel <= 6; panel++ {
		for _, exit := range []string{"esc", "shift+tab", "tab", "/"} {
			for _, bottom := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/bottom=%t", panelNames[panel], exit, bottom), func(t *testing.T) {
					m := testModel(t, NewDemoSource())
					openPanel(t, m, panel)
					m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
					m.key(key("enter"))
					if exit == "tab" && m.actionCount() > 0 {
						m.key(key("tab"))
					}
					if bottom {
						m.vp.GotoBottom()
					} else {
						m.vp.SetYOffset(3)
					}
					offset := m.vp.YOffset
					if offset == 0 || (!bottom && m.vp.AtBottom()) {
						t.Fatal("fixture needs scrollable detail")
					}
					settle(t, m, m.refresh())
					if m.vp.YOffset != offset {
						t.Fatal("collapsed polling moved reader")
					}
					m.key(key(exit))
					if m.focus != 0 {
						t.Fatal("did not restore inventory focus")
					}
					if exit == "/" {
						if m.readers[panel].offset != offset || !m.filterEditing {
							t.Fatal("filter entry lost saved reader position or editing mode")
						}
						settle(t, m, m.key(key("enter")))
						offset = 0 // Existing filter completion resets detail, even without edits.
					}
					settle(t, m, m.refresh())
					m.key(key("enter"))
					if m.vp.YOffset != offset {
						t.Fatalf("offset lost across focus: %d -> %d", offset, m.vp.YOffset)
					}
					m.key(key("enter"))
					if m.vp.YOffset != offset {
						t.Fatal("repeated Enter moved reader")
					}
					m.Update(tea.WindowSizeMsg{Width: 79, Height: 24})
					if m.vp.YOffset != offset {
						t.Fatal("resize moved unclamped offset")
					}
				})
			}
		}
	}
}

func TestListReaderPositionAcrossPanelsAndSelection(t *testing.T) {
	for panel := 2; panel <= 6; panel++ {
		t.Run(panelNames[panel], func(t *testing.T) {
			m := testModel(t, NewDemoSource())
			m.unread, m.unacked = false, false
			openPanel(t, m, panel)
			m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
			m.key(key("enter"))
			m.vp.SetYOffset(3)
			offset := m.vp.YOffset
			id := m.current().key
			openPanel(t, m, 1)
			openPanel(t, m, panel)
			m.key(key("enter"))
			if m.current().key != id || m.vp.YOffset != offset {
				t.Fatal("panel round trip lost reader position")
			}
			m.key(key("esc"))
			settle(t, m, m.choose(1))
			if m.states[panel].selected != 1 {
				t.Fatal("fixture needs second row")
			}
			m.key(key("enter"))
			if m.vp.YOffset != 0 {
				t.Fatal("new selection reused old reader offset")
			}
			m.resetDetail()
			if m.readers[panel].opened || m.readers[panel].key != "" {
				t.Fatal("reset retained reader identity")
			}
		})
	}
}

func TestCollapsedMemoryEvidenceRoundTrip(t *testing.T) {
	m := testModel(t, NewDemoSource())
	openPanel(t, m, 3)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.key(key("enter"))
	m.vp.SetYOffset(3)
	id := m.current().key
	m.key(key("e"))
	if m.focus != 2 {
		t.Fatal("e did not focus evidence")
	}
	offset := m.vp.YOffset
	settle(t, m, m.key(key("enter")))
	if m.panel != 1 || m.focus != 1 || m.evidenceFrom != 4 || m.evidenceUntil != 6 || m.returnMemory != id {
		t.Fatal("evidence navigation lost its bounds or return target")
	}
	if !strings.Contains(ansi.Strip(m.vp.View()), "▌ #4") {
		t.Fatal("evidence reader lost highlighted entry")
	}
	settle(t, m, m.key(key("esc")))
	if m.panel != 3 || m.focus != 0 || m.current().key != id || m.returnMemory != "" {
		t.Fatal("Esc did not return to memory inventory")
	}
	m.key(key("enter"))
	if m.vp.YOffset != offset {
		t.Fatal("evidence round trip lost memory reader position")
	}
}

func TestCollapsedSecretFocusCopyAndExpiry(t *testing.T) {
	f := newFake()
	m := testModel(t, f)
	openPanel(t, m, 6)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.key(key("enter"))
	f.secret = func(context.Context, string, string) ([]byte, error) { return []byte("SYNTHETIC_COLLAPSED_VALUE"), nil }
	msg := runPrivate(t, m, m.key(key("v")))
	for _, focus := range []int{0, 1, 2, 1} {
		m.setDetailFocus(focus)
		if text, _, _, status := m.privateView(); text != "SYNTHETIC_COLLAPSED_VALUE" || status != "visible" {
			t.Fatal("focus change cleared private state")
		}
	}
	m.Update(privateExpiredMsg{generation: msg.generation - 1})
	if text, _, _, _ := m.privateView(); text == "" {
		t.Fatal("stale expiry cleared active reveal")
	}
	m.Update(privateExpiredMsg(msg))
	if m.focus != 1 || strings.Contains(m.View(), "SYNTHETIC_COLLAPSED_VALUE") || !strings.Contains(ansi.Strip(m.vp.View()), "[value hidden]") || !strings.Contains(m.View(), "Private value hidden after 30 seconds.") {
		t.Fatal("expiry did not rerender hidden state inside collapsed detail")
	}
	copied := ""
	m.opts.Copy = func(_ context.Context, value string) error { copied = value; return nil }
	runPrivate(t, m, m.key(key("c")))
	if copied != "SYNTHETIC_COLLAPSED_VALUE" || strings.Contains(m.View(), copied) || m.focus != 1 {
		t.Fatal("collapsed copy changed exact value, displayed it, or changed focus")
	}
}

func TestListFocusSizeSweep(t *testing.T) {
	m := testModel(t, NewDemoSource())
	for panel := 2; panel <= 6; panel++ {
		openPanel(t, m, panel)
		for _, w := range []int{1, 3, 20, 42, 59, 60, 69, 70, 80, 99, 100, 120} {
			for _, h := range []int{1, 3, 8, 11, 12, 18, 24, 40} {
				for _, focus := range []int{0, 1, 2} {
					m.Update(tea.WindowSizeMsg{Width: w, Height: h})
					m.setDetailFocus(focus)
					if m.vp.Width < 1 || m.vp.Height < 1 {
						t.Fatal("negative viewport dimensions")
					}
					view := strings.Split(ansi.Strip(m.View()), "\n")
					if len(view) != h {
						t.Fatalf("panel %d focus %d at %dx%d: height %d", panel, focus, w, h, len(view))
					}
					for _, line := range view {
						if ansi.StringWidth(line) > w {
							t.Fatalf("panel %d: line exceeds width %d", panel, w)
						}
					}
				}
			}
		}
	}
}

func TestListPrivateStateAcrossFocus(t *testing.T) {
	for _, panel := range []int{2, 3, 4, 6} {
		t.Run(panelNames[panel], func(t *testing.T) {
			m := testModel(t, NewDemoSource())
			openPanel(t, m, panel)
			runPrivate(t, m, m.key(key("v")))
			text, kind, id, status := m.privateView()
			if status != "visible" || text == "" {
				t.Fatal("fixture needs visible private content")
			}
			for _, focus := range []int{1, 2, 0, 1} {
				m.setDetailFocus(focus)
				got, k, i, s := m.privateView()
				if got != text || k != kind || i != id || s != status {
					t.Fatal("focus changed private state")
				}
			}
			m.states[panel].filter = "retained filter"
			m.key(key("esc"))
			if got, _, _, _ := m.privateView(); got != "" {
				t.Fatal("Esc did not hide private content")
			}
			if m.focus != 0 || m.states[panel].filter != "retained filter" {
				t.Fatal("focus reset cleared filter too early")
			}
			settle(t, m, m.key(key("esc")))
			if m.states[panel].filter != "" {
				t.Fatal("second Esc did not clear filter")
			}
		})
	}
}

func TestEmailTabChangeClearsSavedReader(t *testing.T) {
	m := testModel(t, NewDemoSource())
	openPanel(t, m, 5)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.key(key("enter"))
	m.vp.SetYOffset(3)
	m.key(key("esc"))
	settle(t, m, m.key(key("s")))
	m.key(key("enter"))
	if !m.sent || m.vp.YOffset != 0 {
		t.Fatal("Sent reused Received reader position despite identity-free rows")
	}
}
