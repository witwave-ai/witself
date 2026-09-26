package agenttui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

const notRecorded = "Not recorded"

// Capture metadata is not a free-form display surface. Only strings in these
// slots survive projection; initial_cwd is reduced before entering any cache.
func transcriptString(v any) string {
	s, _ := v.(string)
	return single(s)
}
func workspaceBase(v any) string {
	s, ok := v.(string)
	if !ok {
		return ""
	}
	s = strings.TrimRight(strings.ReplaceAll(strings.TrimSpace(clean(s)), "\\", "/"), "/")
	if strings.HasPrefix(s, "//") && len(strings.FieldsFunc(s, func(r rune) bool { return r == '/' })) <= 2 {
		return ""
	}
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	if s == "." || s == ".." || len(s) == 2 && s[1] == ':' {
		return ""
	}
	return transcriptString(s)
}
func projectTranscript(t object) object {
	p := object{}
	for _, key := range []string{"id", "title", "external_id", "created_at", "updated_at"} {
		if s, ok := t[key].(string); ok {
			p[key] = clean(s)
		}
	}
	if id, ok := t["id"].(string); ok && (clean(id) != id || strings.ContainsAny(id, "\n\t\r")) {
		p["id"] = ""
	}
	metadata := obj(t["metadata"])
	location := obj(metadata["location"])
	p["metadata"] = object{
		"agent_name": transcriptString(metadata["agent_name"]),
		"runtime":    transcriptString(metadata["runtime"]),
		"location":   first(transcriptString(location["name"]), transcriptString(location["id"])),
		"workspace":  workspaceBase(metadata["initial_cwd"]),
	}
	return p
}
func runtimeLabel(s string) string {
	switch strings.ToLower(s) {
	case "codex":
		return "Codex"
	case "claude", "claude-code", "claude_code":
		return "Claude Code"
	case "gemini", "gemini-cli":
		return "Gemini CLI"
	case "dsh":
		return "DSH"
	case "cursor":
		return "Cursor"
	case "copilot":
		return "Copilot"
	case "grok":
		return "Grok"
	case "grok-build":
		return "Grok Build"
	case "openclaw":
		return "OpenClaw"
	case "antigravity":
		return "Antigravity"
	default:
		return first(s, notRecorded)
	}
}
func transcriptColumns(t object) []string {
	md := obj(t["metadata"])
	return []string{first(str(md, "agent_name"), notRecorded), first(str(md, "location"), notRecorded), runtimeLabel(str(md, "runtime")), first(str(md, "workspace"), notRecorded), first(str(t, "updated_at"), notRecorded)}
}

var transcriptLabels = []string{"Agent", "Location", "AI client", "Workspace", "Updated"}

// Kept as a thin wrapper for the existing transcript geometry contract tests.
func (m *Model) transcriptView(w, h int) string { return m.panelView(w, h) }

func (m *Model) transcriptHeights(h int) (inventory, detail int) {
	return m.panelHeights(1, h)
}
func (m *Model) transcriptInventory(w, h int) string {
	s := &m.states[1]
	rows := m.filtered()
	filter := "/ filter"
	if s.filter != "" || m.filterEditing {
		filter = "/ " + s.filter
	}
	heading := "Transcripts · " + filter
	if m.evidenceFrom > 0 {
		heading = fmt.Sprintf("Evidence #%d–%d · Esc returns to memory", m.evidenceFrom, m.evidenceUntil)
	}
	lines := []string{fit(heading, w)}
	status := s.status[primary(1, false)]
	if len(rows) == 0 {
		return crop(strings.Join(append(lines, inventoryEmptyLabel(status, s.filter != "")), "\n"), w, h)
	}
	if status != "ready" {
		if m.evidenceFrom > 0 {
			lines[0] = fit(heading+" · "+stateLabel(status), w)
		} else {
			lines[0] = fit("Transcripts · "+stateLabel(status)+" · "+filter, w)
		}
	}
	widths := []int{max(5, w*14/100), max(8, w*18/100), max(9, w*18/100), 0, max(10, w*21/100)}
	widths[3] = max(1, w-10-widths[0]-widths[1]-widths[2]-widths[4])
	cells := func(values []string) string {
		out := make([]string, len(values))
		for i, v := range values {
			out[i] = fit(single(v), widths[i])
		}
		return strings.Join(out, "  ")
	}
	stacked := w < 66
	stride := 1
	if stacked {
		stride = 5
	} else {
		lines = append(lines, "  "+cells(transcriptLabels))
	}
	// A focused reader leaves room for one selected row; compact stacked rows
	// remain fully reachable in the scrollable selected-session details.
	count := max(1, (h-len(lines))/stride)
	start := max(0, s.selected-count+1)
	for i := start; i < min(len(rows), start+count); i++ {
		values := transcriptColumns(rows[i].data)
		mark := "  "
		style := m.style(m.colors().fg)
		if i == s.selected {
			mark = "› "
			style = style.Foreground(lipgloss.Color(m.colors().accent)).Bold(true)
		}
		if stacked {
			for j, v := range values {
				lines = append(lines, style.Render(fit(mark+transcriptLabels[j]+": "+single(v), w)))
			}
		} else {
			lines = append(lines, style.Render(mark+cells(values)))
		}
	}
	return crop(strings.Join(lines, "\n"), w, h)
}

// Use only same-session projected fields. Missing detail fields fall back to
// the recorded inventory, including while reading an incremental tail.
func transcriptDetails(inventory, detail object) object {
	if str(detail, "id") == "" || str(detail, "id") != str(inventory, "id") {
		return inventory
	}
	out := object{}
	for _, key := range []string{"id", "title", "external_id", "created_at", "updated_at"} {
		out[key] = first(str(detail, key), str(inventory, key))
	}
	md := object{}
	for _, key := range []string{"agent_name", "runtime", "location", "workspace"} {
		md[key] = first(str(obj(detail["metadata"]), key), str(obj(inventory["metadata"]), key))
	}
	out["metadata"] = md
	return out
}

func (m *Model) transcriptMetadata(d *detailWriter, t object) {
	for i, v := range transcriptColumns(t) {
		d.kv(transcriptLabels[i], v)
	}
	for _, field := range []struct{ key, label string }{{"created_at", "Created"}, {"title", "Original title"}, {"external_id", "External ID"}, {"id", "Transcript ID"}} {
		d.kv(field.label, first(str(t, field.key), notRecorded))
	}
}
func (m *Model) transcriptFooter() string {
	text := "↑↓ select · Enter read · Tab focus · / filter · ? help · q quit"
	if m.focus != 0 {
		text = "↑↓ scroll · g details · [ ] entry · x JSON · Esc back · ? help · q quit"
	}
	if m.focus == 2 {
		text = "↑↓ entry · Enter/x JSON · Tab focus · Esc back · ? help · q quit"
	}
	if ansi.StringWidth(text) > m.width {
		text = "Tab focus · ? help · q quit"
	}
	if ansi.StringWidth(text) > m.width {
		text = "? help · q quit"
	}
	return text
}
