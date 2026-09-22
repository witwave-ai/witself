package agenttui

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/witwave-ai/witself/internal/dashboard"
)

const summarySafeMax int64 = 1<<53 - 1

var summaryKinds = []struct {
	key, label, code, unit, dimension, dark, light string
	panel                                          int
}{
	{"transactions", "Operations", "OPS", "recorded operations", "operation", "#72a8ff", "#365ca1", -1},
	{"transcripts", "Transcripts", "TRN", "entries recorded", "transcript_entry_write", "#55cfdf", "#00677c", 1},
	{"facts", "Facts", "FCT", "recorded deliveries", "fact_returned", "#b0bed0", "#59616a", 2},
	{"memories", "Memories", "MEM", "memory changes", "memory_change", "#bd9aff", "#7551a2", 3},
	{"secrets", "Secrets", "SEC", "recorded accesses", "secret_read", "#edbf67", "#85540d", 6},
	{"email", "Email", "EML", "accepted sends", "email_sent", "#75d19c", "#257245", 5},
	{"messages", "Messages", "MSG", "messages sent", "message_sent", "#f199c5", "#a04373", 4},
}

func summaryKind(key string) int {
	for i, k := range summaryKinds {
		if k.key == key {
			return i
		}
	}
	return -1
}

// Rebuild from typed fields, then reapply a closed vocabulary before data enters
// the model. A Source may be supplied by callers other than dashboard.Reader.
func projectSummary(raw json.RawMessage) (object, error) {
	var envelope struct {
		Summary dashboard.Summary `json:"summary"`
	}
	bad := func() (object, error) { return nil, fmt.Errorf("invalid summary") }
	if json.Unmarshal(raw, &envelope) != nil {
		return bad()
	}
	s := envelope.Summary
	w := s.Window
	if s.Schema != "witself.agent-summary.v2" || s.GeneratedAt.IsZero() || w.Since.IsZero() || s.RefreshAfterSeconds != 30 || !w.Until.Equal(s.GeneratedAt.Truncate(time.Second)) ||
		w.Bucket != "hour" || w.Timezone != "UTC" || !w.PartialCurrentBucket ||
		!w.Since.Equal(w.Until.UTC().Truncate(time.Hour).Add(-23*time.Hour)) ||
		len(s.Categories) != len(summaryKinds) || len(s.Recent) > 12 || len(s.Checkpoints) != 4 {
		return bad()
	}
	s.RefreshAfterSeconds = 30
	seen := map[string]bool{}
	categories := make([]dashboard.SummaryCategory, len(summaryKinds))
	for _, c := range s.Categories {
		i := summaryKind(c.Key)
		if i < 0 || seen[c.Key] {
			return bad()
		}
		seen[c.Key] = true
		k := summaryKinds[i]
		c.Label, c.Code = k.label, k.code
		inv := &c.Inventory
		if !summaryStatus(inv.Status) && (c.Key != "transactions" || inv.Status != "not_applicable") {
			return bad()
		}
		if c.Key == "transactions" && (inv.Status != "not_applicable" || inv.Count != nil || inv.Exact) {
			return bad()
		}
		expectExact := c.Key == "facts" || c.Key == "memories"
		inv.Label = "recent records"
		if c.Key == "transactions" {
			inv.Label = "activity only"
		}
		if c.Key == "facts" {
			inv.Label = "facts"
		}
		if c.Key == "memories" {
			inv.Label = "active memories"
		}
		if inv.Status == "available" {
			if inv.Count == nil || *inv.Count < 0 || *inv.Count > summarySafeMax || c.Key == "transactions" || inv.Exact != expectExact {
				return bad()
			}
		} else {
			inv.Count = nil
		}
		a := &c.Activity
		if !dashboard.ValidSummaryActivity(*a, c.Key, w) {
			return bad()
		}

		a.Unit, a.Dimension = k.unit, k.dimension
		categories[i] = c
	}
	s.Categories = categories
	for _, r := range s.Recent {
		if summaryKind(r.Key) < 0 || r.At.IsZero() || r.At.After(s.GeneratedAt) || !summaryAction(r.Key, r.Action) {
			return bad()
		}
	}
	labels := map[string]string{"memory": "Memory curation", "message": "Messaging", "email": "Email", "avatar": "Avatar"}
	seen = map[string]bool{}
	for i := range s.Checkpoints {
		c := &s.Checkpoints[i]
		if labels[c.Key] == "" || seen[c.Key] {
			return bad()
		}
		seen[c.Key] = true
		c.Label = labels[c.Key]
		switch c.Status {
		case "pending", "clear", "disabled", "unavailable":
		default:
			return bad()
		}
	}
	// Marshal the closed projection only; unknown JSON fields never survive.
	b, _ := json.Marshal(s)
	var out object
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.UseNumber()
	if d.Decode(&out) != nil {
		return bad()
	}
	return object{"summary": out}, nil
}

func summaryStatus(s string) bool { return s == "available" || s == "unavailable" || s == "disabled" }
func summaryAction(key, action string) bool {
	switch key {
	case "transcripts":
		return action == "transcript updated" || action == "transcript created"
	case "memories":
		return action == "memory updated" || action == "memory created"
	case "secrets":
		return action == "secret updated" || action == "secret created"
	case "email":
		return action == "email received"
	case "messages":
		return action == "message received"
	}
	return false
}

func (m *Model) summaryKey(k string) (bool, tea.Cmd) {
	view := -1
	switch k {
	case "o":
		view = 0
	case "l":
		view = 1
	case "e":
		view = 2
	case "d":
		view = 3
	}
	if view >= 0 {
		m.clearPrivate()
		m.summaryView = view
		m.focus = 0
		m.resize()
		m.vp.GotoTop()
		m.renderDetail(false)
		return true, nil
	}
	if m.summaryView == 3 {
		return false, nil
	}
	switch k {
	case "a":
		m.summaryASCII = !m.summaryASCII
		m.renderDetail(false)
	case "j", "down", "k", "up":
		if m.summaryView == 2 {
			if k == "k" || k == "up" {
				m.vp.ScrollUp(1)
			} else {
				m.vp.ScrollDown(1)
			}
			return true, nil
		}
		if m.focus != 0 {
			return false, nil
		}
		delta := 1
		if k == "k" || k == "up" {
			delta = -1
		}
		m.summaryRow = max(0, min(len(summaryKinds)-1, m.summaryRow+delta))
		m.renderDetail(false)
		if m.summaryRow < len(m.actionOffsets) {
			line := m.actionOffsets[m.summaryRow]
			if line < m.vp.YOffset || line >= m.vp.YOffset+m.vp.Height {
				m.vp.SetYOffset(max(0, line-2))
			}
		}
	case "enter":
		if m.summaryView == 2 {
			return true, nil
		}
		if p := summaryKinds[m.summaryRow].panel; p >= 0 {
			return true, m.navigate(p)
		}
		m.notice = "Operations: recorded reads and writes; records are counted separately below."
	case "tab", "shift+tab":
		m.focus = 1 - m.focus
	case "/":
		m.notice = "Enter opens a category. Use d for workspace details and salient memories."
	default:
		return false, nil
	}
	return true, nil
}

func (m *Model) categoryColor(i int) string {
	if m.theme == "paper" {
		return summaryKinds[i].light
	}
	return summaryKinds[i].dark
}

func (m *Model) summaryOverview(d *detailWriter) {
	tabs := []string{"o Overview", "l Timeline", "e Recent updates", "d Details"}
	for i := range tabs {
		style := m.style(m.colors().dim)
		if i == m.summaryView {
			style = m.style(m.colors().accent).Bold(true)
		}
		tabs[i] = style.Render(tabs[i])
	}
	d.line(strings.Join(tabs, "   "))
	d.line("")
	s := obj(m.states[0].data[dashboard.ResourceSummary]["summary"])
	status := m.states[0].status[dashboard.ResourceSummary]
	if len(s) == 0 {
		d.heading("Workspace activity")
		d.body("Summary " + first(status, "loading") + ".")
		d.dim("Inventory and activity appear when the passive summary is available.")
		d.dim("Use 2–7 to open a category, or d for workspace details.")
		return
	}
	if status != "ready" {
		d.line(m.style(m.colors().danger).Render("STALE · showing the last successful summary"))
	}
	d.dim("Recorded activity · 24 hourly buckets · UTC")
	d.dim("Current hour is partial · each row has its own unit")
	d.dim("OPS/MEM totals: recorded portion only · ? before tracking")
	w := obj(s["window"])
	d.dim(summaryTime(str(w, "since"), "Jan 02 15:04") + " → " + summaryTime(str(w, "until"), "Jan 02 15:04") + " UTC")
	d.line("")
	if m.summaryView == 2 {
		m.summaryUpdates(d, s, 12)
	} else {
		if m.summaryView == 0 && d.width >= 64 {
			iw := 22
			if d.width < 88 {
				iw = 12
			}
			d.dim(fit("   CATEGORY", 19) + fit("INVENTORY", iw) + fit("RECORDED ACTIVITY", 27) + "QTY")
		}
		for i, c := range list(s, "categories") {
			m.actionOffsets = append(m.actionOffsets, len(d.lines))
			mark := "  "
			if i == m.summaryRow {
				mark = "> "
			}
			color := m.categoryColor(i)
			label := m.style(color).Bold(true).Render(mark + str(c, "code") + " " + str(c, "label"))
			inv := obj(c["inventory"])
			inventory := str(inv, "status")
			if i == 0 {
				inventory = "activity only"
			}
			if inventory == "available" {
				inventory = str(inv, "count") + " " + str(inv, "label")
			}
			a := obj(c["activity"])
			quantity := summaryStateLabel(str(a, "status"))
			graph := summaryStateLabel(str(a, "status"))
			switch quantity {
			case "available":
				quantity = str(a, "total") + " " + str(a, "unit")
				graph = summaryGraph(a, m.summaryView == 1, m.summaryASCII)
			case "Disabled":
				graph = "history disabled"
			}
			if m.summaryView == 1 {
				d.line(label)
				d.line("  " + m.style(color).Render(graph) + "  " + quantity)
			} else if d.width >= 88 {
				d.line(fit(label, 19) + fit(inventory, 22) + fit(m.style(color).Render(graph), 27) + quantity)
			} else if d.width >= 64 {
				shortInventory := str(inv, "status")
				if i == 0 {
					shortInventory = "activity"
				}
				if shortInventory == "available" {
					shortInventory = str(inv, "count")
					if !flag(inv, "exact") {
						shortInventory += " recent"
					}
				}
				q := "—"
				if str(a, "status") == "available" {
					q = str(a, "total")
				}
				d.line(fit(label, 19) + fit(shortInventory, 12) + fit(m.style(color).Render(graph), 27) + q)
			} else {
				d.line(label + "  " + m.style(m.colors().dim).Render(inventory))
				d.line("  " + m.style(color).Render(graph))
				if str(a, "status") == "available" {
					d.dim("  " + quantity)
				}
			}
		}
		selected := list(s, "categories")[m.summaryRow]
		activity := obj(selected["activity"])
		if str(activity, "status") == "available" {
			d.dim(str(selected, "label") + ": " + str(activity, "total") + " " + str(activity, "unit"))
			measures := []string{}
			for _, b := range list(activity, "breakdown") {
				measures = append(measures, summaryMeasureLabel(str(b, "dimension"))+": "+str(b, "total"))
			}
			if len(measures) > 0 {
				d.dim(strings.Join(measures, " · "))
			}
			coverage := obj(activity["coverage"])
			if len(coverage) > 0 {
				d.body("Recorded portion of this window; tracking since " + summaryTime(str(coverage, "tracking_since"), "Jan 02 15:04:05") + " UTC; earlier bins unknown.")
				if flag(coverage, "partial_first_bucket") {
					d.body("First tracked hour is partial.")
				}
				d.body("Current hour is partial; older clients may omit activity.")
			}
		}
		d.line("")
		if m.summaryView == 1 {
			d.dim("Left → right: " + summaryTime(str(w, "since"), "15h") + " → " + summaryTime(str(w, "until"), "15h") + " UTC")
			legend := "? unknown   · 0   ░ 1–2   ▒ 3–5   ▓ 6–9   █ 10+ per hour"
			if m.summaryASCII {
				legend = "? unknown   . 0   : 1–2   o 3–5   O 6–9   # 10+ per hour"
			}
			d.dim(legend)
		} else {
			d.dim("Sparklines scale per row · recent-record counts are bounded pages")
		}
		d.dim("↑↓ choose · Enter open · Tab scroll · a ASCII graphs")
		m.summaryAttention(d, s)
		if m.summaryView == 0 {
			m.summaryUpdates(d, s, 4)
		}
	}
	d.line("")
	d.dim("Collected " + summaryTime(str(s, "generated_at"), "Jan 02 15:04:05") + " UTC · cache 30s")
	d.dim("Recorded quantities may omit unmetered activity. No combined total.")
}

func summaryTime(s, format string) string {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return "unavailable"
	}
	return t.UTC().Format(format)
}

func summaryGraph(a object, timeline, ascii bool) string {
	values, _ := a["bins"].([]any)
	if len(values) != 24 {
		return "history unavailable"
	}
	ns := make([]int64, len(values))
	var peak int64
	for i, v := range values {
		n, _ := strconv.ParseInt(fmt.Sprint(v), 10, 64)
		ns[i] = n
		peak = max(peak, n)
	}
	chars := []rune("·▁▂▃▄▅▆▇█")
	if ascii {
		chars = []rune("._:-=+*#@")
	}
	if timeline {
		chars = []rune("·░▒▓█")
		if ascii {
			chars = []rune(".:oO#")
		}
	}
	var b strings.Builder
	for i, n := range ns {
		if values[i] == nil {
			b.WriteByte('?')
			if timeline {
				b.WriteByte(' ')
			}
			continue
		}
		level := 0
		if timeline {
			switch {
			case n >= 10:
				level = 4
			case n >= 6:
				level = 3
			case n >= 3:
				level = 2
			case n > 0:
				level = 1
			}
		} else if n > 0 && peak > 0 {
			level = int((n*8 + peak - 1) / peak)
		}
		b.WriteRune(chars[level])
		if timeline {
			b.WriteByte(' ')
		}
	}
	return strings.TrimSpace(b.String())
}

func (m *Model) summaryAttention(d *detailWriter, s object) {
	d.heading("Needs attention")
	known, pending := 0, 0
	for _, c := range list(s, "checkpoints") {
		switch str(c, "status") {
		case "pending":
			pending++
			known++
			d.line(m.style(m.colors().accent).Render("* " + str(c, "label") + " · work pending"))
		case "clear", "disabled":
			known++
		default:
			d.dim(str(c, "label") + " · unavailable")
		}
	}
	if pending == 0 && known == 4 {
		d.dim("No pending work reported.")
	}
	if pending == 0 && known < 4 {
		d.dim("Pending work could not be fully checked.")
	}
}

func (m *Model) summaryUpdates(d *detailWriter, s object, limit int) {
	d.heading("Updates from loaded records")
	updates := list(s, "recent")
	if len(updates) == 0 {
		d.dim("No updates in the loaded records.")
	}
	for _, r := range updates[:min(limit, len(updates))] {
		i := summaryKind(str(r, "key"))
		if i < 0 {
			continue
		}
		d.line(m.style(m.colors().dim).Render(summaryTime(str(r, "at"), "Jan 02 15:04")) + "  " + m.style(m.categoryColor(i)).Bold(true).Render(summaryKinds[i].code) + "  " + str(r, "action"))
	}
	d.dim("Sample of recent records; earlier changes and deleted records are absent.")
}

func demoSummary() dashboard.Summary {
	now, _ := time.Parse(time.RFC3339, demoTime)
	s := dashboard.Summary{Schema: "witself.agent-summary.v2", GeneratedAt: now, RefreshAfterSeconds: 30, Window: dashboard.SummaryWindow{Since: now.Truncate(time.Hour).Add(-23 * time.Hour), Until: now, Bucket: "hour", Timezone: "UTC", PartialCurrentBucket: true}, Recent: []dashboard.SummaryUpdate{}, Checkpoints: []dashboard.SummaryCheckpoint{{Key: "memory", Label: "Memory curation", Status: "pending"}, {Key: "message", Label: "Messaging", Status: "pending"}, {Key: "email", Label: "Email", Status: "clear"}, {Key: "avatar", Label: "Avatar", Status: "clear"}}}
	counts := []int64{0, 3, 3, 3, 2, 2, 3}
	for i, k := range summaryKinds {
		n := counts[i]
		c := dashboard.SummaryCategory{Key: k.key, Label: k.label, Code: k.code, Inventory: dashboard.SummaryInventory{Status: "available", Count: &n, Label: "recent records", Exact: i == 2 || i == 3}, Activity: dashboard.SummaryActivity{Status: "unavailable", Unit: k.unit, Dimension: k.dimension}}
		if i == 0 {
			c.Inventory.Status = "not_applicable"
			c.Inventory.Count = nil
		}
		if i != 0 && i != 3 {
			c.Activity.Status = "available"
			c.Activity.Bins = make([]*int64, 24)
			var total int64
			for h := 0; h < 24; h++ {
				v := int64((h*(i+2) + i) % 13)
				if h < 5 || h > 12 && h < 18 {
					v = 0
				}
				c.Activity.Bins[h] = &v
				total += v
			}
			c.Activity.Total = &total
		}
		if i == 0 || i == 3 {
			tracking := s.Window.Since.Add(6*time.Hour + 17*time.Minute)
			c.Activity.Status = "available"
			c.Activity.Coverage = &dashboard.SummaryCoverage{TrackingSince: &tracking, PartialFirstBucket: true}
			c.Activity.Bins = make([]*int64, 24)
			total := int64(0)
			names := []string{"operation_read", "operation_write", "operation_read_record", "operation_write_record"}
			units := []string{"operation", "operation", "record", "record"}
			if i == 3 {
				names = []string{"memory_created", "memory_revised", "memory_archived", "memory_restored", "memory_deleted"}
				units = []string{"change", "change", "change", "change", "change"}
			}
			for j, name := range names {
				n := int64(j + 1)
				c.Activity.Breakdown = append(c.Activity.Breakdown, dashboard.SummaryMeasure{Dimension: name, Unit: units[j], Total: &n})
				if i == 3 || j < 2 {
					total += n
				}
			}
			for h := 6; h < 24; h++ {
				n := int64(0)
				if h == 6 {
					n = total
				}
				c.Activity.Bins[h] = &n
			}
			c.Activity.Total = &total
		}
		s.Categories = append(s.Categories, c)
	}
	for i, r := range []struct{ key, action string }{{"transcripts", "transcript updated"}, {"memories", "memory updated"}, {"email", "email received"}, {"messages", "message received"}, {"secrets", "secret updated"}} {
		s.Recent = append(s.Recent, dashboard.SummaryUpdate{Key: r.key, At: now.Add(-time.Duration(i) * 7 * time.Minute), Action: r.action})
	}
	return s
}

func summaryStateLabel(status string) string {
	switch status {
	case "not_tracked":
		return "Not tracked yet"
	case "server_update_needed":
		return "Server update needed"
	case "unavailable":
		return "Unavailable"
	case "disabled":
		return "Disabled"
	}
	return status
}
func summaryMeasureLabel(d string) string {
	switch d {
	case "operation_read":
		return "Reads"
	case "operation_write":
		return "Writes"
	case "operation_read_record":
		return "Records read"
	case "operation_write_record":
		return "Records written"
	case "memory_created":
		return "Created"
	case "memory_revised":
		return "Revised"
	case "memory_archived":
		return "Archived"
	case "memory_restored":
		return "Restored"
	case "memory_deleted":
		return "Deleted"
	}
	return ""
}
