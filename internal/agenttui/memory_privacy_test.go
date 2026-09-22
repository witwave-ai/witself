package agenttui

import (
	"context"
	"encoding/json"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/witwave-ai/witself/internal/dashboard"
	"strings"
	"testing"
)

func TestSensitiveMemoryRequiresDeliberateRead(t *testing.T) {
	for _, scenario := range []string{"initial", "becomes-private", "detail-race"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFake()
			private := scenario != "becomes-private"
			var owned json.RawMessage
			f.read = func(ctx context.Context, r dashboard.ReadRequest) (json.RawMessage, error) {
				switch r.Resource {
				case dashboard.ResourceMemories:
					return encode(object{"items": []any{object{"id": "mem_private", "sensitive": private && scenario != "detail-race", "redacted": private && scenario != "detail-race"}}})
				case dashboard.ResourceMemory:
					owned, _ = encode(object{"memory": object{"id": "mem_private", "sensitive": private, "content": "PRIVATE_CANARY", "tags": []any{"PRIVATE_CANARY"}, "evidence": []any{object{"external_locator": "PRIVATE_CANARY"}}}})
					return owned, nil
				case dashboard.ResourceMemoryHistory:
					return encode(object{"versions": []any{}})
				}
				return f.Source.Read(ctx, r)
			}
			m := testModel(t, f)
			openPanel(t, m, 3)
			if scenario == "becomes-private" {
				private = true
				settle(t, m, m.refresh())
			}
			b, _ := json.Marshal(m.states[3].data)
			if strings.Contains(string(b), "PRIVATE_CANARY") || strings.Contains(m.View(), "PRIVATE_CANARY") {
				t.Fatal("passive sensitive memory leak")
			}
			if scenario == "initial" && (f.count(dashboard.ResourceMemory) != 0 || f.count(dashboard.ResourceMemoryHistory) != 0) {
				t.Fatal("private detail fetched automatically")
			}
			runPrivate(t, m, m.key(key("v")))
			text, kind, _, status := m.privateView()
			if text != "PRIVATE_CANARY" || kind != "memory" || status != "visible" || !allZero(owned) {
				t.Fatal("deliberate memory reveal or buffer cleanup failed")
			}
			m.key(key("v"))
			if strings.Contains(m.vp.View(), "PRIVATE_CANARY") {
				t.Fatal("hide retained memory")
			}
		})
	}
}

func TestTinyTerminalOverlaysAndRecovery(t *testing.T) {
	m := testModel(t, NewDemoSource())
	for _, size := range [][2]int{{80, 1}, {80, 6}, {120, 5}, {1, 1}, {120, 40}} {
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		for _, overlay := range []string{"", "help", "theme"} {
			m.overlay = overlay
			for i := 0; i < 150; i++ {
				m.helpScroll = i
				_ = m.View()
			}
		}
	}
}

func TestStaleSalientMetadataDoesNotAuthorizeDetailRefresh(t *testing.T) {
	f := newFake()
	fail := false
	f.read = func(ctx context.Context, r dashboard.ReadRequest) (json.RawMessage, error) {
		if fail && (r.Resource == dashboard.ResourceSelf || r.Resource == dashboard.ResourceMemories) {
			return nil, &dashboard.ReaderError{Status: 503}
		}
		return f.Source.Read(ctx, r)
	}
	m := testModel(t, f)
	openPanel(t, m, 0)
	settle(t, m, m.openSalient())
	before := f.count(dashboard.ResourceMemory)
	history := f.count(dashboard.ResourceMemoryHistory)
	fail = true
	settle(t, m, m.refresh())
	if f.count(dashboard.ResourceMemory) != before || f.count(dashboard.ResourceMemoryHistory) != history {
		t.Fatal("stale salient row triggered detail fetch")
	}
}

func TestPinnedMemoryRevisionHidesReveal(t *testing.T) {
	f := newFake()
	private := false
	f.read = func(ctx context.Context, r dashboard.ReadRequest) (json.RawMessage, error) {
		if r.Resource == dashboard.ResourceMemories {
			return encode(object{"items": []any{demoMemories()[1]}})
		}
		if r.Resource == dashboard.ResourceSelf {
			s := demoSelf()
			if private {
				list(s, "salient_memories")[0]["sensitive"] = true
			}
			return encode(s)
		}
		return f.Source.Read(ctx, r)
	}
	m := testModel(t, f)
	openPanel(t, m, 0)
	settle(t, m, m.openSalient())
	runPrivate(t, m, m.privateRead(false))
	private = true
	settle(t, m, m.refresh())
	if text, _, _, _ := m.privateView(); text != "" {
		t.Fatal("pinned memory mutation retained reveal")
	}
}
