package agenttui

import (
	"fmt"
	"runtime"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

func TestAccountSpacingMultilineTextAllocations(t *testing.T) {
	profile := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(profile) })
	m := testModel(t, NewDemoSource())
	body := strings.Repeat("X", 8192) + strings.Repeat("\n", 8191) + "Y"
	for _, kind := range []string{"body", "dim", "title", "heading"} {
		t.Run(kind, func(t *testing.T) {
			d := &accountWriter{detailWriter: detailWriter{m: m, width: 76}}
			render := d.body
			style := m.style(m.colors().fg)
			switch kind {
			case "dim":
				render, style = d.dim, m.style(m.colors().dim)
			case "title":
				render, style = func(s string) { d.title(s) }, style.Bold(true)
			case "heading":
				render, style = d.heading, style.Bold(true)
			}
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			render(body)
			runtime.ReadMemStats(&after)
			allocated := after.TotalAlloc - before.TotalAlloc
			// Allow ample room for cleaning, wrapping, and ANSI styling. Styling
			// the whole body first pads it to over 64 MiB before wrapping.
			if allocated > 16<<20 {
				t.Fatalf("16-KiB multiline body allocated %d bytes (limit 16 MiB)", allocated)
			}
			t.Logf("16-KiB multiline body allocated %d bytes", allocated)
			if len(d.lines) != (8192+75)/76+8191 {
				t.Fatalf("source blank lines or wrapped rows changed: %d rows", len(d.lines))
			}
			var recovered strings.Builder
			for i, row := range d.lines {
				if ansi.StringWidth(row) > 76 {
					t.Fatal("body row exceeds pane width")
				}
				plain := ansi.Strip(row)
				if kind == "heading" && i == len(d.lines)-1 {
					plain = strings.TrimRight(plain, " ─")
				}
				recovered.WriteString(plain)
			}
			if recovered.String() != strings.Repeat("X", 8192)+"Y" {
				t.Fatal("body text lost or padded")
			}
			last := d.lines[len(d.lines)-1]
			if kind == "heading" {
				last, _, _ = strings.Cut(last, "  ")
			}
			if last != style.Render("Y") {
				t.Fatal("body foreground styling changed")
			}
		})
	}
}

func TestAccountSpacingSixFullTabsAt80(t *testing.T) {
	m := testModel(t, NewDemoSource())
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	for _, section := range accountSections {
		selectAccount(t, m, section)
		parts := make([]string, 0, 6)
		for _, key := range accountSections {
			label := accountLabels[key]
			if key == section {
				label = "› " + label
			}
			parts = append(parts, label)
		}
		got := ansi.Strip(m.accountNav())
		if got != strings.Join(parts, "  ") || ansi.StringWidth(got) != 75 || m.account.vp.Width != 76 {
			t.Fatalf("full atomic tabs do not fit: %q (pane %d)", got, m.account.vp.Width)
		}
	}
}

func TestAccountSpacingCompactContextAndEachFallback(t *testing.T) {
	profile := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(profile) })
	for _, theme := range themeNames {
		for _, component := range []string{"none", "account", "manager", "role", "agent", "normalized agent"} {
			t.Run(theme+"/"+component, func(t *testing.T) {
				m := testModel(t, NewDemoSource())
				m.theme = theme
				m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
				selectAccount(t, m, "overview")
				m.opts.Agent = "Atlas"
				long := strings.Repeat("界e\u0301👩‍💻", 50) + "Z"
				switch component {
				case "account":
					m.account.context.account = long
				case "manager":
					m.account.context.operator = long
				case "role":
					m.account.context.role = long // Direct rendering probe, not authority.
				case "agent":
					m.opts.Agent = long
				case "normalized agent":
					m.opts.Agent = "  Atlas\nSecond  line"
				}
				d := &detailWriter{m: m, width: 76}
				clipped := m.accountContextStrip(d)
				if clipped != (component != "none") || len(d.lines) != 2 {
					t.Fatalf("incorrect context fallback: %v %q", clipped, d.lines)
				}
				for _, line := range d.lines {
					if ansi.StringWidth(line) > 76 {
						t.Fatalf("context overflow: %q", line)
					}
				}
				if component == "none" {
					want := "Account  acct_studio_demo · Read-only\nManager  op_manager_demo · account_owner · Agent Atlas"
					if got := ansi.Strip(strings.Join(d.lines, "\n")); got != want {
						t.Fatalf("context: %q", got)
					}
					if !strings.Contains(d.lines[0], m.style(m.colors().dim).Render("Account  ")) || !strings.Contains(d.lines[0], m.style(m.colors().fg).Render("acct_studio_demo")) {
						t.Fatal("context label/value hierarchy lost")
					}
				}
				if !strings.Contains(ansi.Strip(d.lines[0]), "Read-only") {
					t.Fatal("read-only meaning clipped at 80")
				}
				m.renderAccount()
				all := accountScrolledText(m)
				if strings.Contains(all, "Full account context") != clipped {
					t.Fatal("fallback visibility incorrect")
				}
				compact := strings.Join(strings.Fields(all), "")
				for _, value := range []string{m.account.context.account, m.account.context.operator, m.account.context.role, m.accountAgentName()} {
					if !strings.Contains(compact, strings.Join(strings.Fields(value), "")) {
						t.Fatalf("identity unreachable: %q", value)
					}
				}
				rows := strings.Split(all, "\n")
				if len(rows) < 4 || strings.TrimSpace(rows[2]) != "" || !strings.HasPrefix(rows[3], "Profile") {
					t.Fatal("body heading not separated from context")
				}
			})
		}
	}
}

func TestAccountSpacingFieldsLosslessAndBounded(t *testing.T) {
	profile := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(profile) })
	m := testModel(t, NewDemoSource())
	for _, theme := range themeNames {
		m.theme = theme
		for _, width := range []int{12, 24, 27, 28, 36, 50, 76, 98} {
			for _, label := range []string{"ID", "名e\u0301", "  A label longer than the stable gutter " + strings.Repeat("界", 25), "First\nSecond", "La\x1b]52;c;EVIL\a\tbel\x00"} {
				for _, value := range []string{" " + strings.Repeat("A", 73) + "Z", "  東京 e\u0301 👩‍💻 " + strings.Repeat("word ", 20) + "  ", "safe\x1b[31m value\x1b[0m\u009d52;c;EVIL\u009cnext\r\x07"} {
					d := &accountWriter{detailWriter: detailWriter{m: m, width: width}}
					d.kv(label, value)
					label = clean(label)
					inline := width >= 28 && !strings.Contains(label, "\n") && ansi.StringWidth(label) <= min(22, max(8, width/3))
					start, firstValue := min(22, max(8, width/3))+2, 0
					if !inline {
						start = 2
						// Recover the entire label without discarding whitespace; only source
						// newlines are compared separately from display wrapping.

						firstValue = strings.Count(ansi.Hardwrap(label, width, true), "\n") + 1
						got := ansi.Strip(strings.Join(d.lines[:firstValue], ""))
						if got != strings.ReplaceAll(label, "\n", "") {
							t.Fatalf("label lost: %q", got)
						}
					}
					var recovered strings.Builder
					for i, row := range d.lines {
						plain := ansi.Strip(row)
						if ansi.StringWidth(row) > width || strings.Contains(plain, "EVIL") {
							t.Fatalf("%s/%d overflow or unsafe: %q", theme, width, row)
						}
						if i < firstValue {
							continue
						}
						if i == 0 && inline {
							recovered.WriteString(ansi.Cut(plain, start, width))
						} else {
							if !strings.HasPrefix(plain, strings.Repeat(" ", start)) {
								t.Fatalf("wrong value indent: %q", plain)
							}
							recovered.WriteString(plain[start:])
						}
					}
					if recovered.String() != clean(value) {
						t.Fatalf("%s/%d lost value: %q want %q", theme, width, recovered.String(), clean(value))
					}
				}
			}
		}
	}
}

func TestAccountSpacingPlanNamesAndCodesStayIndependent(t *testing.T) {
	for _, section := range []string{"plan", "billing"} {
		for _, missing := range []string{"neither", "name", "code", "both"} {
			t.Run(section+"/"+missing, func(t *testing.T) {
				m := testModel(t, NewDemoSource())
				m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
				selectAccount(t, m, section)
				o := m.account.data
				pairs := []struct{ label, name, code string }{{"Current plan", "plan_name", "plan"}, {"Billing plan", "billing_plan_name", "billing_plan"}}
				if section == "billing" {
					o = obj(o["summary"])
					pairs = []struct{ label, name, code string }{{"Billing plan", "billing_plan_name", "billing_plan"}, {"Effective plan", "effective_plan_name", "effective_plan"}}
				}
				for i, pair := range pairs {
					o[pair.name], o[pair.code] = fmt.Sprintf("Name%d", i), fmt.Sprintf("code%d", i)
					if missing == "name" || missing == "both" {
						delete(o, pair.name)
					}
					if missing == "code" || missing == "both" {
						delete(o, pair.code)
					}
				}
				o["pending"] = object{"kind": "downgrade", "plan_name": "Pending", "plan": "pending", "requested": "REQUESTED", "effective": "EFFECTIVE", "expires": "EXPIRES"}
				m.renderAccount()
				all := accountScrolledText(m)
				for _, pair := range pairs {
					want := pair.label + strings.Repeat(" ", 24-len(pair.label)) + accountValue(o, pair.name) + " · code " + accountValue(o, pair.code)
					if !strings.Contains(all, want) {
						t.Fatalf("plan identity conflated: %q\n%s", want, all)
					}
				}
				for _, want := range []string{"Applied plan            code starter", "Pending · code pending", "REQUESTED", "EFFECTIVE", "EXPIRES"} {
					if !strings.Contains(all, want) {
						t.Fatalf("missing %q", want)
					}
				}
			})
		}
	}
}

func TestAccountSpacingDensityAndRecordHierarchy(t *testing.T) {
	for _, theme := range themeNames {
		t.Run(theme, func(t *testing.T) {
			m := testModel(t, NewDemoSource())
			m.theme = theme
			m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
			selectAccount(t, m, "clients")
			accountPress(t, m, "x")
			visible := ansi.Strip(m.account.vp.View())
			for _, want := range []string{"Local check", "Local metadata only · no online", "Checked at", demoTime, "Scan status", "partial", "codex", "Recorded version", "0.1.0", "Executable", "present", "Static configuration", "match", "Configuration scope", "mcp_registration", "Recorded installation", "2026-09-01T09:00:00Z", "Effective verification", "Not run"} {
				if !strings.Contains(visible, want) {
					t.Fatalf("first client requires scrolling for %q:\n%s", want, visible)
				}
			}
			for _, row := range strings.Split(visible, "\n") {
				if strings.HasPrefix(row, "codex") && strings.Contains(row, "─") {
					t.Fatal("client title has group rule")
				}
			}
			selectAccount(t, m, "support")
			visible = ansi.Strip(m.account.vp.View())
			if !strings.Contains(visible, "› Field guide question") || !strings.Contains(visible, "Enter deliberately opens text") {
				t.Fatal("support selection/open hint buried")
			}
			for _, height := range []int{24, 40, 24} {
				m.Update(tea.WindowSizeMsg{Width: 80, Height: height})
				accountPress(t, m, "j")
				if !strings.Contains(ansi.Strip(m.account.vp.View()), "› Understanding retention") {
					t.Fatal("selected title offset wrong")
				}
				selected := ansi.Strip(m.account.vp.View())
				if !strings.Contains(selected, "tkt_demo_limits") || !strings.Contains(selected, "resolved · normal") {
					t.Fatalf("selected ticket metadata stranded below viewport:\n%s", selected)
				}
				accountPress(t, m, "k")
			}
			accountPress(t, m, "enter")
			visible = ansi.Strip(m.account.vp.View())
			body := "How do the account limits apply to this workspace?"
			index := strings.Index(visible, body)
			if index < 0 || strings.Count(visible[:index], "\n") > 7 {
				t.Fatalf("body buried:\n%s", visible)
			}
			all := accountScrolledText(m)
			if strings.Index(all, "Ticket details") < strings.Index(all, body) {
				t.Fatal("metadata precedes body")
			}
			for _, row := range strings.Split(visible, "\n") {
				if strings.HasPrefix(row, "account_operator ·") && strings.Contains(row, "─") {
					t.Fatal("message title has group rule")
				}
			}
		})
	}
}

func TestAccountSpacingStackedExplicitBlankLines(t *testing.T) {
	m := testModel(t, NewDemoSource())
	d := &accountWriter{detailWriter: detailWriter{m: m, width: 76}}
	d.kv("Label longer than the stable gutter", "  alpha\n\nbeta\nlast")
	want := "Label longer than the stable gutter\n    alpha\n  \n  beta\n  last"
	if got := ansi.Strip(strings.Join(d.lines, "\n")); got != want {
		t.Fatalf("stacked whitespace changed: %q", got)
	}
}
