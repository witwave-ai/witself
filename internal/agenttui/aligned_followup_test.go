package agenttui

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/witwave-ai/witself/internal/dashboard"
)

func TestReviewAlignedLeadingSpaceThroughAccountProjection(t *testing.T) {
	f := newFake()
	value := " " + strings.Repeat("A", 51) + "Z"
	f.read = func(ctx context.Context, r dashboard.ReadRequest) (json.RawMessage, error) {
		if r.Resource == dashboard.ResourceAccountOverview {
			return encode(object{"schema_version": dashboard.AccountSchema, "available": true, "account": object{"id": "acct_studio_demo", "display_name": value}})
		}
		return f.Source.Read(ctx, r)
	}
	m := testModel(t, f)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	selectAccount(t, m, "overview")
	if m.account.status != "ready" {
		t.Fatal("projection rejected reproduction")
	}
	if !strings.Contains(accountScrolledText(m), "Z") {
		t.Fatal("last display-name character cannot be recovered by scrolling")
	}
}

func TestAlignedSupportFooterKeepsHelpVisible(t *testing.T) {
	for _, width := range []int{80, 120} {
		m := testModel(t, NewDemoSource())
		m.Update(tea.WindowSizeMsg{Width: width, Height: 24})
		selectAccount(t, m, "support")
		for _, key := range []string{"", "tab", "esc", "enter", "tab", "j", "tab"} {
			if key != "" {
				accountPress(t, m, key)
			}
			lines := strings.Split(ansi.Strip(m.View()), "\n")
			note := lines[len(lines)-1]
			if !strings.Contains(note, "?") || strings.Contains(note, "…") {
				t.Fatalf("width %d after %q hides help or focus controls: %q", width, key, note)
			}
		}
	}
}
