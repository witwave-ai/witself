package agenttui

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/witwave-ai/witself/internal/dashboard"
)

func TestTranscriptProjectionBothResources(t *testing.T) {
	transcript := object{"id": "tr_safe", "title": "My / custom / slash title", "external_id": "external/original", "created_at": "created", "updated_at": "updated", "unknown": "PRIVATE", "metadata": object{
		"agent_name": "Atlas\x1b]52;c;PRIVATE\a\u202e", "runtime": "claude-code", "initial_cwd": `C:\private-parent\project\`,
		"location": object{"id": "loc_fallback", "name": []any{"PRIVATE"}, "host": "PRIVATE", "os": "PRIVATE"}, "model": "PRIVATE", "arbitrary": object{"value": "PRIVATE"},
	}}
	for _, r := range []dashboard.Resource{dashboard.ResourceTranscripts, dashboard.ResourceTranscript} {
		raw, _ := encode(object{"transcripts": []any{transcript}, "transcript": transcript})
		out, err := decode(r, raw)
		if err != nil {
			t.Fatal(err)
		}
		data := obj(out["transcript"])
		if r == dashboard.ResourceTranscripts {
			data = list(out, "transcripts")[0]
		}
		want := []string{"Atlas", "loc_fallback", "Claude Code", "project", "updated"}
		if got := transcriptColumns(data); !reflect.DeepEqual(got, want) {
			t.Fatalf("columns: %v", got)
		}
		b, _ := json.Marshal(out)
		for _, secret := range []string{"PRIVATE", "private-parent", "initial_cwd", "model", "host"} {
			if strings.Contains(string(b), secret) {
				t.Fatalf("%v retained %q", r, secret)
			}
		}
		if str(data, "title") != transcript["title"] || str(data, "external_id") != "external/original" || str(data, "created_at") != "created" {
			t.Fatal("original identity lost")
		}
	}
	for _, invalid := range []any{nil, 42, true, []any{"Atlas"}, object{"value": "Atlas"}} {
		p := projectTranscript(object{"title": "Atlas / codex / home / guessed", "metadata": object{"agent_name": invalid, "runtime": invalid, "location": invalid, "initial_cwd": invalid}})
		for _, value := range transcriptColumns(p) {
			if value != notRecorded {
				t.Fatalf("coerced %T into %q", invalid, value)
			}
		}
	}
	if got := runtimeLabel("future/client"); got != "future/client" {
		t.Fatal(got)
	}
}
func TestTranscriptWorkspaceBasename(t *testing.T) {
	for _, tc := range []struct {
		in   any
		want string
	}{
		{"/private/parent/project/", "project"}, {`C:\private\parent\project\\`, "project"}, {`\\host\share\project\`, "project"},
		{`\\server\share\`, ""}, {".", ""}, {"..", ""}, {"/", ""}, {"////", ""}, {`C:\`, ""}, {`C:`, ""}, {"", ""}, {"  ", ""}, {nil, ""}, {42, ""},
		{"/private/project\x1b]52;c;/PRIVATE\a", "project"}, {"/private/界e\u0301/", "界e\u0301"},
	} {
		if got := workspaceBase(tc.in); got != tc.want {
			t.Errorf("%q => %q, want %q", tc.in, got, tc.want)
		}
	}
}
func TestTranscriptTableSelectionFilterAndViewport(t *testing.T) {
	f := newFake()
	f.read = func(ctx context.Context, r dashboard.ReadRequest) (json.RawMessage, error) {
		if r.Resource == dashboard.ResourceTranscripts {
			rows := []any{}
			for i := 0; i < 30; i++ {
				rows = append(rows, object{"id": fmt.Sprintf("tr_%02d", i), "title": fmt.Sprintf("Custom / title %02d", i), "external_id": fmt.Sprintf("external-%02d", i), "metadata": object{"agent_name": fmt.Sprintf("Agent%02d", i), "runtime": "codex", "location": object{"name": "Studio"}, "initial_cwd": "/private/workspace"}})
			}
			return encode(object{"transcripts": rows})
		}
		if r.Resource == dashboard.ResourceTranscript {
			return encode(object{"transcript": object{"id": r.ID}, "entries": demoEntries("tr_keyboard")})
		}
		return f.Source.Read(ctx, r)
	}
	m := testModel(t, f)
	openPanel(t, m, 1)
	for _, size := range [][2]int{{80, 24}, {120, 40}, {40, 24}, {20, 12}, {1, 1}} {
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		for _, selection := range []int{0, 14, 29} {
			m.states[1].selected = selection
			m.states[1].key = fmt.Sprintf("tr_%02d", selection)
			for _, focus := range []int{0, 1, 2} {
				m.focus = focus
				m.renderDetail(false)
				_, _, w, h := m.layout()
				ih, dh := m.transcriptHeights(h)
				if m.vp.Height != max(1, dh-2) {
					t.Fatal("viewport height differs from reader")
				}
				body := m.transcriptView(w, h)
				if lipgloss.Height(body) != h || lipgloss.Width(body) > w {
					t.Fatalf("uncropped body %dx%d at %v focus %d", lipgloss.Width(body), lipgloss.Height(body), size, focus)
				}
				if size[0] >= 80 {
					inv := ansi.Strip(m.transcriptInventory(w-4, ih-2))
					for _, label := range transcriptLabels {
						if !strings.Contains(inv, label) {
							t.Fatalf("missing column %s: %s", label, inv)
						}
					}
					if !strings.Contains(inv, fmt.Sprintf("› Agent%02d", selection)) {
						t.Fatalf("selection scrolled out: %s", inv)
					}
					lines := strings.Split(ansi.Strip(m.View()), "\n")
					if !strings.Contains(lines[len(lines)-2], "q quit") {
						t.Fatal("lost quit")
					}
				}
			}
		}
	}
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.focus = 0
	for _, query := range []string{"Codex", "Studio", "workspace", "Custom / title 29", "external-29", "tr_29"} {
		m.states[1].filter = query
		m.reconcileRows()
		if len(m.filtered()) == 0 {
			t.Fatalf("search missed %s", query)
		}
	}
	m.states[1].filter = "private"
	m.reconcileRows()
	if len(m.filtered()) != 0 {
		t.Fatal("full path entered search corpus")
	}
	m.states[1].filter = ""
	m.reconcileRows()
	settle(t, m, m.refresh())
	m.renderDetail(false)
	m.key(key("enter"))
	if m.focus != 1 {
		t.Fatal("Enter did not focus reader")
	}
	m.key(key("tab"))
	if m.focus != 2 {
		t.Fatal("Tab did not focus actions")
	}
	m.key(key("tab"))
	if m.focus != 0 {
		t.Fatal("Tab did not return inventory")
	}
	m.key(key("tab"))
	if m.focus != 1 {
		t.Fatal("Tab did not focus reader")
	}
	m.key(key("esc"))
	if m.focus != 0 {
		t.Fatal("Esc did not return inventory")
	}
}
func TestTranscriptSelectedValuesReachableAndActionsAligned(t *testing.T) {
	m := testModel(t, NewDemoSource())
	openPanel(t, m, 1)
	for _, size := range [][2]int{{80, 24}, {120, 40}, {40, 24}} {
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		row := &m.states[1].rows[m.states[1].selected]
		long := strings.Repeat("界", 60) + "END"
		row.data["title"] = "custom / " + long
		obj(row.data["metadata"])["workspace"] = long
		m.states[1].data[dashboard.ResourceTranscript]["transcript"] = row.data
		m.key(key("enter"))
		m.renderDetail(false)
		m.vp.GotoTop()
		seen := ""
		for {
			seen += ansi.Strip(m.vp.View()) + "\n"
			if m.vp.AtBottom() {
				break
			}
			m.vp.ScrollDown(1)
		}
		for _, want := range []string{"Original title", "External ID", "Transcript ID", "studio/design/042", "tr_wayfinding", "END"} {
			if !strings.Contains(seen, want) {
				t.Fatalf("%v unreachable %s", size, want)
			}
		}
		m.key(key("]"))
		offset := m.actionOffsets[m.sub]
		if offset < m.vp.YOffset || offset >= m.vp.YOffset+m.vp.Height {
			t.Fatalf("entry action outside actual viewport at %v", size)
		}
		m.key(key("]"))
		m.key(key("x"))
		if !m.expanded[3] {
			t.Fatal("JSON action did not expand entry 3")
		}
		m.key(key("x"))
		m.sub = 0
		m.subKey = ""
	}
}

func TestTranscriptEnterReadsAt80AndReturnsToMetadata(t *testing.T) {
	m := testModel(t, NewDemoSource())
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	openPanel(t, m, 1)
	t.Log("80x24 inventory:\n" + ansi.Strip(m.View()))
	row := &m.states[1].rows[m.states[1].selected]
	row.data["title"] = strings.Repeat("A custom / title with many segments ", 20) + "TITLE_END"
	obj(row.data["metadata"])["location"] = strings.Repeat("location ", 40) + "LOCATION_END"
	m.states[1].data[dashboard.ResourceTranscript]["transcript"] = row.data
	m.renderDetail(false)
	m.key(key("enter"))
	t.Log("80x24 reader with long metadata:\n" + ansi.Strip(m.View()))
	if !strings.Contains(ansi.Strip(m.View()), "Can we make the workspace feel quieter") {
		t.Fatal("Enter left the real entry below metadata")
	}
	_, _, _, h := m.layout()
	_, dh := m.transcriptHeights(h)
	if m.vp.Height != dh-2 {
		t.Fatal("Enter reader geometry differs from display")
	}
	m.key(key("g"))
	if !strings.Contains(ansi.Strip(m.vp.View()), "Selected session") {
		t.Fatal("Home did not return to full metadata")
	}
	m.key(key("end"))
	settle(t, m, m.refresh())
	if !m.vp.AtBottom() {
		t.Fatal("reader lost tail position")
	}
	m.key(key("esc"))
	if m.focus != 0 || m.vp.YOffset != 0 {
		t.Fatal("Esc did not restore selected-session summary")
	}
	m.key(key("enter"))
	m.key(key("/"))
	_, dh = m.transcriptHeights(h)
	if m.focus != 0 || m.vp.Height != dh-2 {
		t.Fatal("filter transition left stale geometry")
	}
}

func TestTranscriptSelectedDetailUsesMatchingProjectedMetadata(t *testing.T) {
	inventory := projectTranscript(object{"id": "tr_a", "title": "Custom / title", "external_id": "original", "updated_at": "before", "metadata": object{"agent_name": "Atlas", "initial_cwd": "/private/project"}})
	detail := projectTranscript(object{"id": "tr_a", "updated_at": "after", "created_at": "created", "metadata": object{"runtime": "codex", "location": object{"id": "recorded-location"}}})
	merged := transcriptDetails(inventory, detail)
	if want := []string{"Atlas", "recorded-location", "Codex", "project", "after"}; !reflect.DeepEqual(transcriptColumns(merged), want) {
		t.Fatalf("selected metadata: %v", transcriptColumns(merged))
	}
	if str(merged, "title") != "Custom / title" || str(merged, "external_id") != "original" || str(merged, "created_at") != "created" {
		t.Fatal("original metadata not reachable")
	}
	detail["id"] = "tr_other"
	if got := transcriptDetails(inventory, detail); !reflect.DeepEqual(got, inventory) {
		t.Fatal("different session's detail applied")
	}
}

func TestTranscriptPendingReadFocusGeometry(t *testing.T) {
	for _, control := range []string{"enter", "x"} {
		m := testModel(t, NewDemoSource())
		openPanel(t, m, 1)
		m.resetDetail()
		m.renderDetail(false)
		m.key(key(control))
		_, _, _, h := m.layout()
		_, dh := m.transcriptHeights(h)
		if m.vp.Height != dh-2 {
			t.Fatalf("pending %s reader has stale geometry", control)
		}
		settle(t, m, m.refresh())
		if !strings.Contains(ansi.Strip(m.View()), "Can we make the workspace feel quieter") {
			t.Fatalf("%s did not expose an entry when its request completed", control)
		}
	}
}

func TestTranscriptReviewSharedFocusCycle(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {120, 40}} {
		m := testModel(t, NewDemoSource())
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		openPanel(t, m, 1)
		for _, step := range []struct {
			key   string
			focus int
		}{{"tab", 1}, {"tab", 2}, {"tab", 0}, {"shift+tab", 2}, {"shift+tab", 1}, {"shift+tab", 0}} {
			m.key(key(step.key))
			if m.focus != step.focus {
				t.Fatalf("%v %s: focus=%d want %d", size, step.key, m.focus, step.focus)
			}
			_, _, _, h := m.layout()
			_, dh := m.transcriptHeights(h)
			if m.vp.Height != dh-2 {
				t.Fatal("focus cycle left stale reader geometry")
			}
		}
		m.resetDetail()
		m.renderDetail(false)
		for _, step := range []struct {
			key   string
			focus int
		}{{"tab", 1}, {"tab", 0}, {"shift+tab", 1}, {"shift+tab", 0}} {
			m.key(key(step.key))
			if m.focus != step.focus {
				t.Fatalf("empty actions: %s focus=%d want %d", step.key, m.focus, step.focus)
			}
		}
	}
}

func TestTranscriptReviewReaderPositionAcrossFocus(t *testing.T) {
	for _, exit := range []string{"esc", "shift+tab", "tab"} {
		for _, tail := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/tail=%t", exit, tail), func(t *testing.T) {
				m := testModel(t, NewDemoSource())
				openPanel(t, m, 1)
				m.key(key("enter"))
				if !strings.Contains(ansi.Strip(m.View()), "Can we make the workspace feel quieter") {
					t.Fatal("first Enter did not expose an entry")
				}
				if exit == "tab" {
					m.key(key("tab"))
					if m.focus != 2 {
						t.Fatal("Tab did not enter actions")
					}
				}
				if tail {
					m.key(key("G"))
				} else {
					m.vp.SetYOffset(m.actionOffsets[2] + 1)
					if m.vp.AtBottom() {
						t.Fatal("fixture needs a middle position")
					}
				}
				offset := m.vp.YOffset

				m.key(key(exit))
				if m.focus != 0 {
					t.Fatal("did not return to inventory")
				}
				if !strings.Contains(ansi.Strip(m.vp.View()), "Selected session") {
					t.Fatal("inventory summary is not available")
				}
				settle(t, m, m.refresh())
				m.key(key("enter"))
				if m.vp.YOffset != offset || m.vp.AtBottom() != tail {
					t.Fatalf("reader position lost: offset %d -> %d, tail %t -> %t", offset, m.vp.YOffset, tail, m.vp.AtBottom())
				}
			})
		}
	}
}

func TestTranscriptReviewEnterWhileReadingDoesNotJump(t *testing.T) {
	m := testModel(t, NewDemoSource())
	openPanel(t, m, 1)
	m.key(key("enter"))
	for _, control := range []string{"down", "G", "g"} {
		m.key(key(control))
		offset := m.vp.YOffset
		m.key(key("enter"))
		if m.vp.YOffset != offset {
			t.Fatalf("Enter after %s moved offset %d to %d", control, offset, m.vp.YOffset)
		}
	}
}

func TestTranscriptReviewSavedTailTracksRefreshResizeAndSession(t *testing.T) {
	f := newFake()
	extra := false
	f.read = func(ctx context.Context, r dashboard.ReadRequest) (json.RawMessage, error) {
		if r.Resource == dashboard.ResourceTranscripts && extra {
			rows := demoTranscripts()
			rows[0], rows[1] = rows[1], rows[0]
			return encode(object{"transcripts": rows})
		}
		if r.Resource == dashboard.ResourceTranscript && r.ID == "tr_wayfinding" && extra {
			return encode(object{"transcript": demoTranscripts()[0], "entries": []any{object{"sequence": 8, "role": "assistant", "body": "New entry at the saved live edge."}}})
		}
		return f.Source.Read(ctx, r)
	}
	m := testModel(t, f)
	openPanel(t, m, 1)
	m.key(key("enter"))
	m.key(key("G"))
	m.key(key("esc"))
	extra = true
	settle(t, m, m.refresh())
	if m.current().key != "tr_wayfinding" || m.states[1].selected != 1 {
		t.Fatal("refresh did not retain selected session across reorder")
	}
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	if m.vp.YOffset != 0 {
		t.Fatal("saved tail displaced inventory metadata")
	}
	m.key(key("enter"))
	if !m.vp.AtBottom() || !strings.Contains(ansi.Strip(m.vp.View()), "New entry at the saved live edge.") {
		t.Fatal("return did not follow the saved live tail after refresh and resize")
	}
	_, _, _, h := m.layout()
	_, dh := m.transcriptHeights(h)
	if m.vp.Height != dh-2 {
		t.Fatal("resumed reader geometry differs from displayed reader")
	}
	m.key(key("esc"))
	settle(t, m, m.choose(-1))
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.key(key("enter"))
	if m.current().key != "tr_fieldguide" || !strings.Contains(ansi.Strip(m.vp.View()), "Review the field guide") {
		t.Fatal("selection change reused another session's position instead of opening its entries")
	}
}

func TestTranscriptReaderPositionAcrossPanels(t *testing.T) {
	for _, tail := range []bool{false, true} {
		t.Run(fmt.Sprintf("tail=%t", tail), func(t *testing.T) {
			m := testModel(t, NewDemoSource())
			openPanel(t, m, 1)
			m.key(key("enter"))
			if tail {
				m.key(key("G"))
			} else {
				m.vp.SetYOffset(m.actionOffsets[2] + 1)
			}
			offset := m.vp.YOffset
			settle(t, m, m.key(key("3")))
			settle(t, m, m.key(key("2")))
			m.key(key("enter"))
			if m.vp.YOffset != offset || m.vp.AtBottom() != tail {
				t.Fatalf("panel switch lost reader: offset %d -> %d, tail %t -> %t", offset, m.vp.YOffset, tail, m.vp.AtBottom())
			}
		})
	}
}
