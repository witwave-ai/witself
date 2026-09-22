package agenttui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

func TestPanelsFitBeforeOuterCropAndKeepFooter(t *testing.T) {
	m := testModel(t, NewDemoSource())
	for _, size := range [][2]int{{80, 24}, {120, 40}} {
		m.width, m.height = size[0], size[1]
		m.resize()
		for panel := range panelNames {
			openPanel(t, m, panel)
			_, iw, dw, h := m.layout()
			for _, box := range []string{m.box(m.inventoryView(iw-4, h-2), iw, h, true), m.box(m.vp.View(), dw, h, true)} {
				if lipgloss.Height(box) != h {
					t.Fatalf("%s box height=%d, want %d", panelNames[panel], lipgloss.Height(box), h)
				}
			}
			view := strings.Split(ansi.Strip(m.View()), "\n")
			if !strings.Contains(view[len(view)-2], "q quit") {
				t.Fatalf("footer cropped at %dx%d/%s: %q", size[0], size[1], panelNames[panel], view[len(view)-2])
			}
		}
	}
}
