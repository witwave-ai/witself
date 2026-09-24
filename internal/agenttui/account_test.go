package agenttui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
	"github.com/witwave-ai/witself/internal/dashboard"
)

// Execute finite work only. Timers are explicitly injected by each lifecycle test.
func accountSettle(t *testing.T, m *Model, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		return
	}
	msg := cmd()
	switch msg := msg.(type) {
	case tea.BatchMsg:
		for _, child := range msg {
			accountSettle(t, m, child)
		}
	case accountContextMsg, accountDataMsg, accountTicketMsg, fetchedMsg, themeMsg:
		_, next := m.Update(msg)
		accountSettle(t, m, next)
	default:
		t.Fatalf("unexpected account command %T", msg)
	}
}
func accountPress(t *testing.T, m *Model, k string) {
	t.Helper()
	_, cmd := m.Update(key(k))
	accountSettle(t, m, cmd)
}
func authorizeAccount(t *testing.T, m *Model) {
	t.Helper()
	accountSettle(t, m, m.accountAuthority())
	if !m.account.context.available() {
		t.Fatal("fixture authorization rejected")
	}
}
func selectAccount(t *testing.T, m *Model, section string) {
	t.Helper()
	if !m.account.context.available() {
		authorizeAccount(t, m)
	}
	if !m.account.mode {
		accountPress(t, m, "8")
	}
	for i := 0; m.accountSection() != section && i < 6; i++ {
		if m.accountSection() == "access" {
			t.Fatal("section not available")
		}
		accountPress(t, m, "]")
	}
	if m.accountSection() != section {
		t.Fatal("could not select section")
	}
}
func ticketEmpty(t *testing.T, m *Model) {
	t.Helper()
	p := &m.account.ticket
	p.Lock()
	defer p.Unlock()
	if p.id != "" || p.data != nil || p.status != "" {
		t.Fatal("ephemeral thread retained")
	}
	if strings.Contains(m.account.vp.View(), "synthetic support text") {
		t.Fatal("rendered private text retained")
	}
}
func TestAccountAbsentAndStrictContext(t *testing.T) {
	bad := []object{
		{"schema_version": dashboard.AccountSchema, "available": false},
		{"available": true, "account_id": "acct_studio_demo", "operator_id": "op_manager_demo", "role": "account_owner", "sections": accountSections},
	}
	for _, mutate := range []func(object){
		func(o object) { o["schema_version"] = "future" }, func(o object) { delete(o, "operator_id") }, func(o object) { o["operator_id"] = 123 }, func(o object) { o["account_id"] = "acct\x1b[0m" }, func(o object) { o["role"] = "account_member" }, func(o object) { o["sections"] = []string{"overview", "secrets"} }, func(o object) { o["sections"] = []string{"access", "overview"} }, func(o object) { o["sections"] = []string{"overview", "overview"} }, func(o object) { o["role"] = "account_operator" }, func(o object) { o["available"] = "true" },
	} {
		o := demoAccountContext()
		mutate(o)
		bad = append(bad, o)
	}
	for i, o := range bad {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			f := newFake()
			f.read = func(ctx context.Context, r dashboard.ReadRequest) (json.RawMessage, error) {
				if r.Resource == dashboard.ResourceAccountContext {
					return encode(o)
				}
				return f.Source.Read(ctx, r)
			}
			m := testModel(t, f)
			accountSettle(t, m, m.accountAuthority())
			accountPress(t, m, "8")
			if m.account.mode || m.account.context.available() {
				t.Fatal("unverified context exposed Account")
			}
			if strings.Contains(m.View(), "8 Account") || strings.Contains(m.overlayView(120, 500), "Account ·") {
				t.Fatal("unauthorized navigation/help")
			}
			if len(f.calls) != 1 || f.calls[0].Resource != dashboard.ResourceAccountContext {
				t.Fatal("unexpected resource without context")
			}
			openPanel(t, m, 6)
			if m.panel != 6 || len(m.states) != 7 {
				t.Fatal("agent panels stopped working")
			}
		})
	}
	// Transport errors and nil sources are also normal agent-only sessions.
	for _, src := range []Source{nil, newFake()} {
		m := testModel(t, src)
		if f, ok := src.(*fakeSource); ok {
			f.read = func(context.Context, dashboard.ReadRequest) (json.RawMessage, error) {
				return nil, errors.New("PRIVATE_ERROR")
			}
		}
		accountSettle(t, m, m.accountAuthority())
		accountPress(t, m, "8")
		if m.account.mode || strings.Contains(m.View(), "PRIVATE_ERROR") {
			t.Fatal("failed context enabled or leaked")
		}
	}
}
func TestAccountNavigationAndRestrictedRole(t *testing.T) {
	for _, restricted := range []bool{false, true} {
		t.Run(fmt.Sprint(restricted), func(t *testing.T) {
			f := newFake()
			f.read = func(ctx context.Context, r dashboard.ReadRequest) (json.RawMessage, error) {
				if restricted && (r.Resource == dashboard.ResourceAccountContext || r.Resource == dashboard.ResourceAccountAccess) {
					o := demoAccountContext()
					o["role"] = "account_operator"
					o["sections"] = []string{"overview", "clients", "support", "access"}
					return encode(o)
				}
				return f.Source.Read(ctx, r)
			}
			m := testModel(t, f)
			authorizeAccount(t, m)
			accountPress(t, m, "8")
			for _, section := range m.account.context.sections {
				selectAccount(t, m, section)
				if m.account.status != "ready" {
					t.Fatalf("%s: %s", section, m.account.status)
				}
				content := ansi.Strip(m.account.vp.View())
				if !strings.Contains(content, "Account  acct_studio_demo · Read-only") || !strings.Contains(content, "Manager  op_manager_demo") || !strings.Contains(content, m.account.context.role+" · Agent "+m.accountAgentName()) {
					t.Fatalf("scope missing: %s", content)
				}
			}
			for _, k := range []string{"v", "c", "e", "s", "u", "a", "/"} {
				accountPress(t, m, k)
			}
			if len(f.privateCalls) != 0 || m.filterEditing {
				t.Fatal("Account fell through agent actions")
			}
			for _, call := range f.calls {
				if call.Resource != dashboard.ResourceAccountContext && call.Resource != accountResources["overview"] && call.Resource != accountResources["clients"] && call.Resource != accountResources["plan"] && call.Resource != accountResources["billing"] && call.Resource != accountResources["support"] && call.Resource != accountResources["access"] {
					t.Fatalf("unexpected resource %v", call.Resource)
				}
				if call.ID != "" || len(call.Query) != 0 {
					t.Fatal("unexpected arguments")
				}
			}
			if restricted && (f.count(dashboard.ResourceAccountPlan) != 0 || f.count(dashboard.ResourceAccountBilling) != 0 || strings.Contains(m.accountNav(), "Billing")) {
				t.Fatal("restricted role exposed billing")
			}
			for p := 0; p < 7; p++ {
				accountPress(t, m, fmt.Sprint(p+1))
				if m.account.mode || m.panel != p {
					t.Fatal("agent navigation changed")
				}
				accountPress(t, m, "8")
			}
		})
	}
}
func TestAccountSupportPrivateLifecycle(t *testing.T) {
	for _, action := range []string{"esc", "1", "]", "r", "?", "t", "selection", "revoked", "identity", "role", "sections", "close"} {
		t.Run(action, func(t *testing.T) {
			f := newFake()
			m := testModel(t, f)
			selectAccount(t, m, "support")
			if f.count(dashboard.ResourceAccountSupportTicket) != 0 {
				t.Fatal("passive support read body")
			}
			accountPress(t, m, "enter")
			p := &m.account.ticket
			p.Lock()
			data := p.data
			p.Unlock()
			b, _ := json.Marshal(data)
			if !strings.Contains(string(b), "synthetic support text") {
				t.Fatal("Enter did not open body")
			}
			passive, _ := json.Marshal(m.account.data)
			if strings.Contains(string(passive), "synthetic support text") {
				t.Fatal("body in passive cache")
			}
			m.account.vp.GotoBottom()
			if !strings.Contains(m.account.vp.View(), "synthetic support text") {
				t.Fatal("body not rendered")
			}
			switch action {
			case "revoked":
				m.Update(accountContextMsg{generation: m.account.authorityGeneration})
			case "identity", "role", "sections":
				c := m.account.context
				switch action {
				case "identity":
					c.operator = "op_changed"
				case "role":
					c.role = "account_operator"
					c.sections = []string{"overview", "clients", "support", "access"}
				default:
					c.sections = []string{"overview", "access"}
				}
				_, cmd := m.Update(accountContextMsg{generation: m.account.authorityGeneration, context: c})
				if m.account.data != nil || m.account.vp.YOffset != 0 {
					t.Fatal("old projections retained on authority change")
				}
				accountSettle(t, m, cmd)
			case "selection":
				accountPress(t, m, "tab")
				accountPress(t, m, "j")
			case "close":
				m.Close()
			default:
				accountPress(t, m, action)
			}
			ticketEmpty(t, m)
		})
	}
}
func TestAccountRevocationIndependentAndDelayedResults(t *testing.T) {
	for _, resource := range []dashboard.Resource{dashboard.ResourceAccountOverview, dashboard.ResourceAccountSupportTicket} {
		for _, paused := range []bool{false, true} {
			t.Run(fmt.Sprintf("%v/paused=%v", resource, paused), func(t *testing.T) {
				f := newFake()
				var revoke atomic.Bool
				started, release, cancelled := make(chan struct{}), make(chan struct{}), make(chan struct{})
				f.read = func(ctx context.Context, r dashboard.ReadRequest) (json.RawMessage, error) {
					if r.Resource == dashboard.ResourceAccountContext && revoke.Load() {
						return encode(object{"schema_version": dashboard.AccountSchema, "available": false})
					}
					if r.Resource == resource {
						close(started)
						<-release
						if ctx.Err() != nil {
							close(cancelled)
						}
						return f.Source.Read(context.Background(), r)
					}
					return f.Source.Read(ctx, r)
				}
				m := testModel(t, f)
				authorizeAccount(t, m)
				var cmd tea.Cmd
				if resource == dashboard.ResourceAccountOverview {
					_, cmd = m.Update(key("8"))
				} else {
					m.account.mode = true
					m.account.section = 4
					accountSettle(t, m, m.accountRead(false))
					_, cmd = m.Update(key("enter"))
				}
				result := make(chan tea.Msg, 1)
				go func() { result <- cmd() }()
				<-started
				m.paused = paused
				m.busy = true
				revoke.Store(true)
				// Exercise the real Account tick batch, excluding the next timer.
				_, tick := m.Update(accountTickMsg{})
				batch := tick().(tea.BatchMsg)
				for _, child := range batch[1:] {
					accountSettle(t, m, child)
				}
				if m.account.mode || m.account.context.available() || m.account.data != nil {
					t.Fatal("slow read or pause blocked revocation")
				}
				ticketEmpty(t, m)
				close(release)
				m.Update(<-result)
				select {
				case <-cancelled:
				default:
					t.Fatal("work was not cancelled")
				}
				if m.account.context.available() || m.account.data != nil {
					t.Fatal("delayed success restored account")
				}
				ticketEmpty(t, m)
			})
		}
	}
}
func TestAccountOldAuthorityCannotRestoreAfterDenial(t *testing.T) {
	m := testModel(t, newFake())
	authorizeAccount(t, m)
	accountPress(t, m, "8")
	old := accountContextMsg{generation: m.account.authorityGeneration, context: m.account.context}
	m.Update(accountDataMsg{generation: m.account.generation, status: "forbidden"})
	m.Update(old)
	if m.account.context.available() || m.account.mode {
		t.Fatal("old authorization resurrected account")
	}
}
func TestAccountDelayedThreadCannotRestoreAfterSelectionOrClose(t *testing.T) {
	for _, action := range []string{"esc", "r", "1", "selection", "close"} {
		t.Run(action, func(t *testing.T) {
			f := newFake()
			started, release := make(chan struct{}), make(chan struct{})
			f.read = func(ctx context.Context, r dashboard.ReadRequest) (json.RawMessage, error) {
				if r.Resource == dashboard.ResourceAccountSupportTicket {
					close(started)
					<-release
					return f.Source.Read(context.Background(), r)
				}
				return f.Source.Read(ctx, r)
			}
			m := testModel(t, f)
			selectAccount(t, m, "support")
			_, cmd := m.Update(key("enter"))
			done := make(chan tea.Msg, 1)
			go func() { done <- cmd() }()
			<-started
			switch action {
			case "close":
				m.Close()
			case "selection":
				accountPress(t, m, "tab")
				accountPress(t, m, "j")
			default:
				accountPress(t, m, action)
			}
			close(release)
			m.Update(<-done)
			ticketEmpty(t, m)
		})
	}
}
func TestAccountScansExplicitAndBodiesNeverPoll(t *testing.T) {
	f := newFake()
	m := testModel(t, f)
	selectAccount(t, m, "clients")
	if !strings.Contains(m.account.vp.View(), "Not checked") {
		t.Fatal("invented initial scan")
	}
	for i := 0; i < 3; i++ {
		accountPress(t, m, "r")
		accountSettle(t, m, m.accountRead(false))
	}
	if f.count(dashboard.ResourceAccountClientsScan) != 0 {
		t.Fatal("refresh scanned")
	}
	accountPress(t, m, "x")
	if f.count(dashboard.ResourceAccountClientsScan) != 1 || !strings.Contains(m.account.vp.View(), "Checked at") {
		t.Fatal("explicit scan missing")
	}
	accountPress(t, m, "r")
	if str(m.account.data, "status") != "checked" {
		t.Fatal("cached report lost")
	}
	selectAccount(t, m, "support")
	accountPress(t, m, "x")
	accountPress(t, m, "enter")
	for i := 0; i < 3; i++ {
		_, tick := m.Update(accountTickMsg{})
		batch := tick().(tea.BatchMsg)
		for _, child := range batch[1:] {
			accountSettle(t, m, child)
		}
	}
	if f.count(dashboard.ResourceAccountSupportTicket) != 1 || f.count(dashboard.ResourceAccountClientsScan) != 1 {
		t.Fatal("body polling or unintended scan")
	}
}
func TestAccountThreadTooLargeKeepsAccessWithoutRetry(t *testing.T) {
	f := newFake()
	f.read = func(ctx context.Context, r dashboard.ReadRequest) (json.RawMessage, error) {
		if r.Resource == dashboard.ResourceAccountSupportTicket {
			return nil, &dashboard.ReaderError{Code: "response_too_large"}
		}
		return f.Source.Read(ctx, r)
	}
	m := testModel(t, f)
	selectAccount(t, m, "support")
	accountPress(t, m, "enter")
	m.account.vp.GotoBottom()
	text := ansi.Strip(m.account.vp.View())
	for _, want := range []string{"thread exceeds", "4 MiB", "witself account support show", "access remains"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q: %s", want, text)
		}
	}
	for i := 0; i < 2; i++ {
		_, tick := m.Update(accountTickMsg{})
		batch := tick().(tea.BatchMsg)
		for _, child := range batch[1:] {
			accountSettle(t, m, child)
		}
	}
	if !m.account.context.available() || f.count(dashboard.ResourceAccountSupportTicket) != 1 {
		t.Fatal("oversize revoked or retried")
	}
}
func TestAccountDecoderBoundsPrivacyAndMoney(t *testing.T) {
	raw, _ := encode(demoAccountContext())
	c, _ := decodeAccountContext(raw)
	raw = []byte(`{"schema_version":"witself.console.account.v1","available":true,"summary":{"available":true,"next_charge":{"amount_cents":9007199254740993,"currency":"JPY"},"customer_id":"PRIVATE"},"invoices":{"available":true,"entries":[{"amount_cents":0,"currency":"USD","hosted_url":"PRIVATE"},{"currency":"EUR"},{"amount_cents":null,"currency":null}]},"payments":{"available":false,"error":"unavailable","provider_id":"PRIVATE"}}`)
	o, err := decodeAccount(dashboard.ResourceAccountBilling, raw, c, "")
	if err != nil {
		t.Fatal(err)
	}
	if accountAmount(obj(obj(o["summary"])["next_charge"])) != "9007199254740993 cents · JPY" {
		t.Fatal("integer precision lost")
	}
	rows := list(obj(o["invoices"]), "entries")
	wants := []string{"0 cents · USD", "Unknown cents · EUR", "Unknown cents · Unknown"}
	for i, row := range rows {
		if got := accountAmount(row); got != wants[i] {
			t.Fatal(got)
		}
	}
	b, _ := json.Marshal(o)
	if strings.Contains(string(b), "PRIVATE") {
		t.Fatal("unallowlisted field retained")
	}
	for _, amount := range []string{`1.2`, `"0"`, `9223372036854775808`} {
		bad := strings.Replace(string(raw), "9007199254740993", amount, 1)
		if _, err := decodeAccount(dashboard.ResourceAccountBilling, []byte(bad), c, ""); err == nil {
			t.Fatal("invalid amount accepted")
		}
	}
	body := "safe\x1b]52;c;EVIL\a\u009d8;;EVIL\u009c\x1b[2J\u202e\ntext"
	raw, _ = encode(object{"schema_version": dashboard.AccountSchema, "available": true, "ticket": object{"id": "tkt_1"}, "messages": []any{object{"id": "msg_1", "body": body, "attachments": "PRIVATE"}}})
	o, err = decodeAccount(dashboard.ResourceAccountSupportTicket, raw, c, "tkt_1")
	if err != nil {
		t.Fatal(err)
	}
	b, _ = json.Marshal(o)
	if strings.Contains(string(b), "EVIL") || strings.Contains(string(b), "PRIVATE") || strings.Contains(string(b), `\u001b`) {
		t.Fatal("terminal or private payload retained")
	}
	if _, err = decodeAccount(dashboard.ResourceAccountSupportTicket, raw, c, "tkt_other"); err == nil {
		t.Fatal("wrong thread accepted")
	}
	for _, r := range []dashboard.Resource{dashboard.ResourceAccountOverview, dashboard.ResourceAccountSupportTicket} {
		limit := 2 << 20
		if r == dashboard.ResourceAccountSupportTicket {
			limit = 4 << 20
		}
		huge := []byte(`{"schema_version":"witself.console.account.v1","available":true,"unknown":"` + strings.Repeat("x", limit) + `"}`)
		if _, err = decodeAccount(r, huge, c, ""); err == nil {
			t.Fatal("byte cap bypassed")
		}
	}
	tickets := []any{}
	for i := 0; i < 105; i++ {
		tickets = append(tickets, object{"id": fmt.Sprintf("tkt_%d", i), "subject": strings.Repeat("x", 4100), "body": "PRIVATE"})
	}
	raw, _ = encode(object{"schema_version": dashboard.AccountSchema, "available": true, "tickets": tickets})
	o, err = decodeAccount(dashboard.ResourceAccountSupport, raw, c, "")
	if err != nil || len(list(o, "tickets")) != 100 || !flag(o, "truncated") {
		t.Fatal("record/text bounds not enforced")
	}
	b, _ = json.Marshal(o)
	if strings.Contains(string(b), "PRIVATE") {
		t.Fatal("passive body retained")
	}
}
func TestAccountLayoutsAndSyntheticSnapshots(t *testing.T) {
	old := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(old)
	dir := os.Getenv("WITSELF_TUI_ACCOUNT_SNAPSHOT_DIR")
	for _, theme := range themeNames[1:] {
		for _, size := range [][2]int{{80, 24}, {120, 40}} {
			t.Run(fmt.Sprintf("%s/%dx%d", theme, size[0], size[1]), func(t *testing.T) {
				m := testModel(t, NewDemoSource())
				m.theme = theme
				settle(t, m, m.refresh())
				authorizeAccount(t, m)
				m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
				accountPress(t, m, "8")
				for _, section := range accountSections {
					selectAccount(t, m, section)
					view := m.View()
					plain := ansi.Strip(view)
					if section == "support" {
						accountPress(t, m, "enter")
						accountPress(t, m, "esc")
						accountPress(t, m, "j")
						if !strings.Contains(m.account.vp.View(), "Understanding retention") {
							t.Fatal("selected ticket offscreen")
						}
						accountPress(t, m, "k")
					}
					lines := strings.Split(plain, "\n")
					if len(lines) != size[1] {
						t.Fatalf("height %d", len(lines))
					}
					for _, line := range lines {
						if ansi.StringWidth(line) != size[0] {
							t.Fatalf("width %d", ansi.StringWidth(line))
						}
					}
					for _, want := range []string{accountLabels[section], "Account  acct_studio_demo · Read-only", "Manager  op_manager_demo", m.account.context.role + " · Agent " + m.accountAgentName(), "1–7", "q quit"} {
						if !strings.Contains(plain, want) {
							t.Fatalf("%s missing %q\n%s", section, want, plain)
						}
					}
					if size[0] == 80 && !strings.Contains(lines[2], "8 Account") {
						t.Fatal("compact authorized shortcut lost")
					}
					if size[0] == 120 && !strings.Contains(plain, "Account scope") {
						t.Fatal("rail separation lost")
					}
					if dir != "" {
						name := fmt.Sprintf("%s-%dx%d-%s", theme, size[0], size[1], section)
						if err := os.WriteFile(filepath.Join(dir, name+".ansi"), []byte(view), 0600); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(filepath.Join(dir, name+".txt"), []byte(plain), 0600); err != nil {
							t.Fatal(err)
						}
					}
					accountPress(t, m, "tab")
					if m.account.focus != 1 {
						t.Fatal("detail focus missing")
					}
					m.Update(tea.KeyMsg{Type: tea.KeyPgDown})
					offset := m.account.vp.YOffset
					m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
					if m.account.vp.YOffset != offset {
						t.Fatal("resize lost scroll")
					}
					accountPress(t, m, "tab")
					m.account.vp.GotoTop()
				}
				// Authorized Account navigation must coexist with all seven agent panels.
				for p := 0; p < 7; p++ {
					accountPress(t, m, fmt.Sprint(p+1))
					if m.panel != p || len(m.states) != 7 {
						t.Fatal("agent panel regression")
					}
					if !strings.Contains(m.View(), "8") {
						t.Fatal("authorized entry lost")
					}
				}
			})
		}
	}
}
func TestAccountCloseCancelsAuthorityAndSection(t *testing.T) {
	f := newFake()
	var calls atomic.Int32
	started := make(chan struct{}, 2)
	f.read = func(ctx context.Context, _ dashboard.ReadRequest) (json.RawMessage, error) {
		calls.Add(1)
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	m := testModel(t, f)
	raw, _ := encode(demoAccountContext())
	m.account.context, _ = decodeAccountContext(raw)
	m.account.mode = true
	cmds := []tea.Cmd{m.accountAuthority(), m.accountRead(false)}
	done := make(chan tea.Msg, 2)
	for _, cmd := range cmds {
		go func(cmd tea.Cmd) { done <- cmd() }(cmd)
	}
	<-started
	<-started
	m.Close()
	for range cmds {
		select {
		case msg := <-done:
			m.Update(msg)
		case <-time.After(time.Second):
			t.Fatal("close did not cancel account work")
		}
	}
	if m.View() != "" || m.account.data != nil || calls.Load() != 2 {
		t.Fatal("close retained work")
	}
	ticketEmpty(t, m)
}

func TestAccountPassiveScrollSelectionAndPermissionHelp(t *testing.T) {
	m := testModel(t, newFake())
	selectAccount(t, m, "billing")
	m.account.focus = 1
	m.account.vp.HalfPageDown()
	before := m.account.vp.YOffset
	cmd := m.accountRead(false)
	if m.account.vp.YOffset != before {
		t.Fatal("passive refresh clamped scroll while loading")
	}
	accountSettle(t, m, cmd)
	if m.account.vp.YOffset != before {
		t.Fatal("passive refresh lost scroll")
	}
	for _, size := range [][2]int{{120, 40}, {80, 24}, {120, 40}, {80, 24}} {
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		if m.account.focus != 1 || !m.account.mode {
			t.Fatal("resize changed focus/mode")
		}
	}
	c := m.account.context
	c.sections = []string{"overview", "access"}
	_, cmd = m.Update(accountContextMsg{generation: m.account.authorityGeneration, context: c})
	accountSettle(t, m, cmd)
	help := m.overlayView(120, 500)
	if strings.Contains(help, "selected support thread") || strings.Contains(help, "Check this device") {
		t.Fatal("unavailable Account help exposed")
	}
}
func TestAccountContextErrorClearsPausedBody(t *testing.T) {
	f := newFake()
	m := testModel(t, f)
	selectAccount(t, m, "support")
	accountPress(t, m, "enter")
	m.paused = true
	f.read = func(ctx context.Context, r dashboard.ReadRequest) (json.RawMessage, error) {
		if r.Resource == dashboard.ResourceAccountContext {
			return nil, errors.New("PRIVATE transport failure")
		}
		return f.Source.Read(ctx, r)
	}
	accountSettle(t, m, m.accountAuthority())
	ticketEmpty(t, m)
	if m.account.mode || m.account.context.available() || strings.Contains(m.View(), "PRIVATE") {
		t.Fatal("context failure retained paused authority")
	}
}

func TestAccountEnterFencesEarlierMetadataPoll(t *testing.T) {
	f := newFake()
	m := testModel(t, f)
	selectAccount(t, m, "support")
	// This queued response predates the deliberate selection and removes its row.
	poll := m.accountRead(false)
	generation := m.account.generation
	accountPress(t, m, "enter")
	if m.account.generation == generation || m.account.busy {
		t.Fatal("Enter did not cancel old metadata generation")
	}
	stale := poll().(accountDataMsg)
	stale.data = object{"tickets": []any{object{"id": "tkt_other", "subject": "Replacement"}}}
	stale.status = "ready"
	m.Update(stale)
	m.account.ticket.Lock()
	id, status := m.account.ticket.id, m.account.ticket.status
	m.account.ticket.Unlock()
	if id != "tkt_demo_guide" || status != "ready" || str(list(m.account.data, "tickets")[0], "id") != id {
		t.Fatal("old metadata changed private selection")
	}
}
func TestAccountSelectedReadCancellationSettlesLoading(t *testing.T) {
	f := newFake()
	started := make(chan struct{})
	f.read = func(ctx context.Context, r dashboard.ReadRequest) (json.RawMessage, error) {
		if r.Resource == dashboard.ResourceAccountSupportTicket {
			close(started)
			<-ctx.Done()
			return f.Source.Read(context.Background(), r)
		}
		return f.Source.Read(ctx, r)
	}
	m := testModel(t, f)
	selectAccount(t, m, "support")
	_, cmd := m.Update(key("enter"))
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	<-started
	// Simulate the command deadline without a ten-second wall-clock test.
	m.account.ticket.Lock()
	m.account.ticket.cancel()
	m.account.ticket.Unlock()
	m.Update(<-done)
	m.account.ticket.Lock()
	defer m.account.ticket.Unlock()
	if m.account.ticket.status != "unavailable" || m.account.ticket.data != nil {
		t.Fatal("expired read retained loading or private data")
	}
}

func TestAccountDetailSnapshots(t *testing.T) {
	dir := os.Getenv("WITSELF_TUI_ACCOUNT_SNAPSHOT_DIR")
	if dir == "" {
		t.Skip("optional external synthetic snapshot export")
	}
	old := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(old)
	for _, size := range [][2]int{{80, 24}, {120, 40}} {
		for _, section := range []string{"clients", "plan", "billing", "support"} {
			m := testModel(t, NewDemoSource())
			m.theme = "console"
			if size[0] == 120 {
				m.theme = "paper"
			}
			settle(t, m, m.refresh())
			m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
			selectAccount(t, m, section)
			switch section {
			case "clients":
				accountPress(t, m, "x")
			case "support":
				accountPress(t, m, "enter")
			}
			m.account.focus = 1
			m.account.vp.GotoBottom()
			name := fmt.Sprintf("%s-%dx%d-%s-detail", m.theme, size[0], size[1], section)
			view := m.View()
			if err := os.WriteFile(filepath.Join(dir, name+".ansi"), []byte(view), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, name+".txt"), []byte(ansi.Strip(view)), 0600); err != nil {
				t.Fatal(err)
			}
			m.Close()
		}
	}
}
