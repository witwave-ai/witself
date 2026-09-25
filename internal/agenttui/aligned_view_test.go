package agenttui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
	"github.com/witwave-ai/witself/internal/dashboard"
)

func TestAlignedFieldsReflowWithoutLosingContent(t *testing.T) {
	m := testModel(t, NewDemoSource())
	for _, width := range []int{12, 27, 28, 36, 50, 76, 98} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			d := &detailWriter{m: m, width: width}
			labels := []string{"ID", "名e\u0301", "A label far longer than the gutter", "Line one\nLine two"}
			values := []string{"alpha", "beta", "gamma", "delta"}
			column := -1
			for i, label := range labels {
				start := len(d.lines)
				d.kv(label, values[i])
				rows := strings.Split(ansi.Strip(strings.Join(d.lines[start:], "\n")), "\n")
				last := rows[len(rows)-1]
				index := strings.Index(last, values[i])
				if index < 0 {
					t.Fatalf("lost value: %q", rows)
				}
				got := ansi.StringWidth(last[:index])
				if column < 0 {
					column = got
				} else if got != column {
					t.Fatalf("value columns differ for %q: %d, %d", label, column, got)
				}
				if i >= 2 && len(rows) < 2 {
					t.Fatal("long/multiline label did not stand alone")
				}
				if !strings.Contains(strings.Join(strings.Fields(strings.Join(rows, "")), ""), strings.Join(strings.Fields(label+values[i]), "")) {
					t.Fatalf("field data lost: %q", rows)
				}
			}
			if width == 50 && column > 20 || width >= 76 && (column < 20 || column > 24) {
				t.Fatalf("impractical gutter at %d: %d", width, column)
			}
			start := len(d.lines)
			value := "東京 e\u0301 👩‍💻 " + strings.Repeat("wrap ", 16) + "\n\nnext line\nlast"
			d.kv("Value", value)
			rows := d.lines[start:]
			var recovered strings.Builder
			for i, row := range rows {
				plain := ansi.Strip(row)
				if width < 28 && i == 0 {
					continue // label occupies its own line
				}
				prefix := plain[:min(len(plain), column)]
				if i > 0 && strings.TrimSpace(prefix) != "" {
					t.Fatalf("continuation escaped value column: %q", plain)
				}
				recovered.WriteString(plain[len(prefix):])
			}
			if strings.Join(strings.Fields(recovered.String()), "") != strings.Join(strings.Fields(value), "") {
				t.Fatalf("wrapped value lost Unicode/content: %q", recovered.String())
			}
			if !strings.Contains(ansi.Strip(strings.Join(rows, "\n")), "\n"+strings.Repeat(" ", column)+"\n") {
				t.Fatal("explicit blank value line was lost")
			}
			for _, row := range d.lines {
				if ansi.StringWidth(row) > width || strings.Contains(row, "…") {
					t.Fatalf("field requires cropping: %q", row)
				}
			}
		})
	}
}

func TestAlignedFieldsSanitizeBothColumnsAndStyleValues(t *testing.T) {
	profile := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(profile) })
	m := testModel(t, NewDemoSource())
	for _, theme := range themeNames {
		m.theme = theme
		d := &detailWriter{m: m, width: 76}
		d.kv("La\x1b]52;c;EVIL\a\tbel\x00", "safe\x1b[31m value\x1b[0m\n\u009d52;c;EVIL\u009cnext\r\x07")
		plain := ansi.Strip(strings.Join(d.lines, "\n"))
		if !strings.Contains(plain, "La bel") || !strings.Contains(plain, "safe value") || !strings.Contains(plain, "next") || strings.Contains(plain, "EVIL") || strings.ContainsAny(plain, "\r\x00\x07\x1b\u009d") {
			t.Fatalf("unsanitized field: %q", plain)
		}
		if !strings.Contains(d.lines[0], m.style(m.colors().fg).Render("safe value")) || !strings.Contains(d.lines[1], m.style(m.colors().fg).Render("next")) {
			t.Fatal("value inherited label or untrusted foreground")
		}
	}
}

func TestAlignedNavigationAndAtomicAccountTabs(t *testing.T) {
	m := testModel(t, NewDemoSource())
	for _, authorized := range []bool{false, true} {
		if authorized {
			authorizeAccount(t, m)
		}
		for panel := range panelNames {
			m.panel = panel
			m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
			nav := ansi.Strip(m.compactNav())
			for i, name := range []string{"Overview", "Transcr.", "Facts", "Memories", "Convs.", "Email", "Secrets"} {
				if !strings.Contains(nav, fmt.Sprintf("%d %s", i+1, name)) {
					t.Fatalf("missing panel shortcut: %s", nav)
				}
			}
			if strings.Contains(nav, "Account") != authorized || !strings.Contains(nav, fmt.Sprintf("›%d", panel+1)) || ansi.StringWidth(nav) > 80 {
				t.Fatalf("scope/selection/width: %s", nav)
			}
		}
	}
	for width := 28; width <= 120; width++ {
		m.Update(tea.WindowSizeMsg{Width: width, Height: 24})
		for _, section := range accountSections {
			selectAccount(t, m, section)
			for _, line := range strings.Split(ansi.Strip(m.accountNav()), "\n") {
				if ansi.StringWidth(line) > m.account.vp.Width || strings.HasSuffix(strings.TrimSpace(line), "›") || strings.Contains(line, "·") {
					t.Fatalf("split/orphan subsection at %d: %q", width, line)
				}
			}
			if !strings.Contains(ansi.Strip(m.accountNav()), "› "+accountLabels[section]) {
				t.Fatalf("selected subsection split at %d", width)
			}
		}
		accountPress(t, m, "1")
		m.account.section = 0
	}
}

func accountScrolledText(m *Model) string {
	old := m.account.vp.YOffset
	defer m.account.vp.SetYOffset(old)
	var text strings.Builder
	for i := 0; i < m.account.vp.TotalLineCount(); i += max(1, m.account.vp.Height) {
		m.account.vp.SetYOffset(i)
		rows := strings.Split(ansi.Strip(m.account.vp.View()), "\n")
		// The final viewport clamps upward; do not duplicate that overlap.
		text.WriteString(strings.Join(rows[i-m.account.vp.YOffset:], "\n") + "\n")
	}
	return text.String()
}

func TestAlignedLeadingSpaceValueFitsAndSurvivesViewport(t *testing.T) {
	m := testModel(t, NewDemoSource())
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	selectAccount(t, m, "overview")
	value := " " + strings.Repeat("A", 51) + "Z"
	if m.account.vp.Width != 76 {
		t.Fatalf("unexpected viewport width: %d", m.account.vp.Width)
	}
	t.Run("row width and lossless wrapping", func(t *testing.T) {
		d := &detailWriter{m: m, width: m.account.vp.Width}
		d.kv("Display name", value)
		var recovered strings.Builder
		for _, row := range d.lines {
			if ansi.StringWidth(row) > d.width {
				t.Errorf("aligned row overflows %d cells: %q", d.width, row)
			}
			recovered.WriteString(ansi.Strip(row)[24:])
		}
		if recovered.String() != value {
			t.Fatalf("wrapped value changed: %q", recovered.String())
		}
	})
	t.Run("account viewport recovery", func(t *testing.T) {
		obj(m.account.data["account"])["display_name"] = value
		m.renderAccount()
		all := strings.Join(strings.Fields(accountScrolledText(m)), "")
		if !strings.Contains(all, strings.TrimSpace(value)) {
			t.Fatalf("full display name, including final Z, is unreachable: %q", all)
		}
	})
}

func TestAlignedAccountDensityAndExplicitReads(t *testing.T) {
	f := newFake()
	m := testModel(t, f)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	selectAccount(t, m, "clients")
	if f.count(dashboard.ResourceAccountClientsScan) != 0 {
		t.Fatal("presentation triggered scan")
	}
	accountPress(t, m, "x")
	visible := ansi.Strip(m.account.vp.View())
	for _, want := range []string{"Local metadata only", "no online", "codex", "Recorded version", "0.1.0", "Static configuration", "match", "Configuration scope", "mcp_registration", "Recorded installation", "Effective verification", "Not run"} {
		if !strings.Contains(visible, want) {
			t.Fatalf("first client data buried at 80x24: %q\n%s", want, visible)
		}
	}
	t.Logf("Clients at 80x24 after explicit x:\n%s", ansi.Strip(m.View()))
	selectAccount(t, m, "support")
	if !strings.Contains(ansi.Strip(m.account.vp.View()), "› Field guide question") || !strings.Contains(m.View(), "Enter") {
		t.Fatal("support selection or explicit open hint missing")
	}
	for _, k := range []string{"tab", "tab", "j", "k"} {
		accountPress(t, m, k)
	}
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	if f.count(dashboard.ResourceAccountSupportTicket) != 0 || len(f.privateCalls) != 0 {
		t.Fatal("render/focus/selection/resize implicitly fetched private data")
	}
	accountPress(t, m, "enter")
	visible = ansi.Strip(m.account.vp.View())
	if !strings.Contains(visible, "How do the account limits apply to this workspace?") || !strings.Contains(m.View(), "Thread scrolling") {
		t.Fatalf("body or scrolling hint buried: %s", visible)
	}
	t.Logf("Support at 80x24 after explicit Enter:\n%s", ansi.Strip(m.View()))
	all := accountScrolledText(m)
	for _, want := range []string{"Ticket details", "Subject", "Synthetic support thread", "State", "Category", "Priority", "Opened", "Last activity", "First response", "Resolved", "Closed", "synthetic support text"} {
		if !strings.Contains(all, want) {
			t.Fatalf("thread scroll lost %q", want)
		}
	}
	if strings.Contains(valueText(m.account.data), "How do the account limits") || f.count(dashboard.ResourceAccountSupportTicket) != 1 {
		t.Fatal("body cached passively or read more than once")
	}
	accountPress(t, m, "tab")
	if !strings.Contains(m.View(), "Ticket selection") {
		t.Fatal("footer misrepresents selection focus with an open thread")
	}
	accountPress(t, m, "j")
	ticketEmpty(t, m)
	accountPress(t, m, "tab")
	if !strings.Contains(m.View(), "Ticket list scrolling") {
		t.Fatal("unopened list described as thread scrolling")
	}
}

func TestAlignedAccountDimensionsAndFullContext(t *testing.T) {
	m := testModel(t, NewDemoSource())
	authorizeAccount(t, m)
	for _, size := range [][2]int{{80, 24}, {120, 40}, {80, 24}} {
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		for _, section := range accountSections {
			selectAccount(t, m, section)
			_, _, width, height := m.layout()
			content := m.accountNav() + "\n" + m.account.vp.View()
			if lipgloss.Height(content) != height-2 || lipgloss.Width(content) > width-4 {
				t.Fatalf("%v/%s requires early cropping: %dx%d", size, section, lipgloss.Width(content), lipgloss.Height(content))
			}
			box := m.box(content, width, height, true)
			if lipgloss.Width(box) != width || lipgloss.Height(box) != height {
				t.Fatal("pane overflow before final crop")
			}
		}
		accountPress(t, m, "1")
		m.account.section = 0
	}
	selectAccount(t, m, "overview")
	m.account.context.account = "account_" + strings.Repeat("long_", 30)
	m.account.context.operator = "operator_" + strings.Repeat("manager_", 30)
	m.opts.Agent = "agent_" + strings.Repeat("selected_", 30)
	m.renderAccount()
	d := &detailWriter{m: m, width: m.account.vp.Width}
	if !m.accountContextStrip(d) || len(d.lines) != 2 {
		t.Fatal("long context did not stay compact")
	}
	all := strings.Join(strings.Fields(accountScrolledText(m)), "")
	for _, want := range []string{m.account.context.account, m.account.context.operator, m.account.context.role, m.accountAgentName()} {
		if !strings.Contains(all, want) {
			t.Fatalf("full context unreachable: %s", want)
		}
	}
}

func TestAlignedFocusAndPaperReset(t *testing.T) {
	profile := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(profile) })
	m := testModel(t, NewDemoSource())
	for _, theme := range themeNames {
		m.theme = theme
		if ansi.Strip(m.box("content", 30, 5, true)) == ansi.Strip(m.box("content", 30, 5, false)) {
			t.Fatal("pane focus relies solely on color")
		}
		selectAccount(t, m, "overview")
		m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
		view := m.View()
		base := lipgloss.NewStyle().Background(lipgloss.Color(m.colors().bg)).Foreground(lipgloss.Color(m.colors().fg))
		prefix, _, _ := strings.Cut(base.Render("x"), "x")
		for _, after := range strings.Split(strings.TrimSuffix(view, "\x1b[0m"), "\x1b[0m")[1:] {
			if !strings.HasPrefix(after, prefix) {
				t.Fatal("nested reset lost workspace background/foreground")
			}
		}
	}
}
