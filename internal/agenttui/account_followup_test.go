package agenttui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/witwave-ai/witself/internal/dashboard"
	"github.com/witwave-ai/witself/internal/plans"
)

func decodeTestPlan(t *testing.T, src object) (object, error) {
	t.Helper()
	src["schema_version"], src["available"] = dashboard.AccountSchema, true
	raw, err := encode(src)
	if err != nil {
		t.Fatal(err)
	}
	return decodeAccount(dashboard.ResourceAccountPlan, raw, accountContext{}, "")
}

func TestAccountPlanLimitPresenceAndValidation(t *testing.T) {
	for _, source := range []string{"limits", "limit_defaults"} {
		for _, tc := range []struct {
			name    string
			present bool
			value   any
			want    string
		}{
			{name: "absent", want: "Unknown"}, {name: "null", present: true, want: "Unknown"},
			{"empty", true, object{}, "No plan cap"}, {"zero", true, object{"agents": 0}, "0 count"},
			{"positive", true, object{"agents": 5}, "5 count"},
			{"unrecognized", true, object{"PRIVATE_UNKNOWN": "PRIVATE"}, "No plan cap"},
		} {
			t.Run(source+"/"+tc.name, func(t *testing.T) {
				src := object{source + "_units": object{"agents": "count"}}
				if tc.present {
					src[source] = tc.value
				}
				out, err := decodeTestPlan(t, src)
				if err != nil {
					t.Fatal(err)
				}
				if _, present := out[source]; present != tc.present {
					t.Fatal("source presence lost")
				}
				if got := accountLimitValue(out, source, "agents"); got != tc.want {
					t.Fatalf("got %q, want %q", got, tc.want)
				}
				for _, key := range plans.SupportedLimitKeys() {
					if key == "agents" {
						continue
					}
					want := "No plan cap"
					if !tc.present || tc.value == nil {
						want = "Unknown"
					}
					if got := accountLimitValue(out, source, key); got != want {
						t.Fatalf("%s = %q, want %q", key, got, want)
					}
				}
			})
		}
		invalid := []any{object{"agents": nil}, object{"agents": -1}, object{"agents": json.Number("9007199254740992")}, object{"agents": json.Number("9223372036854775808")}, object{"agents": 0.5}, object{"agents": "PRIVATE"}, object{"agents": true}, "PRIVATE", []any{}}
		for dimension, maximum := range plans.WorkerValidationContract().LimitMaximums {
			invalid = append(invalid, object{dimension: maximum + 1})
		}
		for i, value := range invalid {
			t.Run(fmt.Sprintf("%s/invalid-%d", source, i), func(t *testing.T) {
				if out, err := decodeTestPlan(t, object{source: value}); err == nil || out != nil {
					t.Fatal("invalid cap became a displayable plan")
				}
			})
		}
	}
}

func TestAccountRootProjectedPlans(t *testing.T) {
	// Production-shaped output supplied by root from corrected core a5ef5926.
	// The core projection helper omits the enclosing response schema in these data.
	raw, err := os.ReadFile("testdata/account_projected_plans.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures map[string]json.RawMessage
	if err = json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatal(err)
	}
	for name, raw := range fixtures {
		t.Run(name, func(t *testing.T) {
			var src object
			d := json.NewDecoder(strings.NewReader(string(raw)))
			d.UseNumber()
			if err := d.Decode(&src); err != nil {
				t.Fatal(err)
			}
			out, err := decodeTestPlan(t, src)
			if err != nil {
				t.Fatal(err)
			}
			for _, source := range []string{"limits", "limit_defaults"} {
				for _, key := range plans.SupportedLimitKeys() {
					want := "Unknown"
					if src[source] != nil {
						want = "No plan cap"
						unit := str(obj(src[source+"_units"]), key)
						if unit == "" {
							t.Fatalf("fixture lacks unit for %s", key)
						}
						if _, exists := obj(src[source])[key]; exists {
							want = str(obj(src[source]), key) + " " + unit
						}
					}
					if got := accountLimitValue(out, source, key); got != want {
						t.Fatalf("%s/%s = %q, want %q", source, key, got, want)
					}
				}
			}
			m := testModel(t, newFake())
			selectAccount(t, m, "plan")
			m.account.data = out
			m.account.vp.Width = 120
			m.account.vp.Height = 1000
			m.renderAccount()
			view := ansi.Strip(m.account.vp.View())
			for _, key := range plans.SupportedLimitKeys() {
				if !strings.Contains(view, strings.ReplaceAll(key, "_", " ")) {
					t.Fatalf("dimension hidden: %s", key)
				}
			}
			if dir := os.Getenv("WITSELF_TUI_ACCOUNT_SNAPSHOT_DIR"); dir != "" {
				if err := os.WriteFile(filepath.Join(dir, "projected-plan-"+name+".txt"), []byte(view), 0600); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestAccountRetentionPresence(t *testing.T) {
	for _, source := range []string{"message_retention", "email_retention", "transcript_retention"} {
		for _, tc := range []struct {
			name  string
			value object
			want  string
		}{
			{"absent", object{}, "Unknown"}, {"null", object{"effective_days": nil}, "Indefinite"},
			{"zero", object{"effective_days": 0}, "Unknown"}, {"negative", object{"effective_days": -1}, "Unknown"},
			{"valid", object{"effective_days": 30}, "30 days"}, {"unsafe", object{"effective_days": json.Number("9007199254740992")}, "Unknown"},
		} {
			t.Run(source+"/"+tc.name, func(t *testing.T) {
				tc.value["default_days"], tc.value["overridden"] = nil, true
				out, err := decodeTestPlan(t, object{source: tc.value})
				if err != nil {
					t.Fatal(err)
				}
				retention := obj(out[source])
				if got := accountRetentionValue(retention, "effective_days"); got != tc.want {
					t.Fatalf("got %q, want %q", got, tc.want)
				}
				if accountRetentionValue(retention, "default_days") != "Indefinite" || !flag(retention, "overridden") {
					t.Fatal("default or override lost")
				}
				m := testModel(t, newFake())
				selectAccount(t, m, "plan")
				m.account.data = out
				m.account.vp.Height = 1000
				m.renderAccount()
				view := ansi.Strip(m.account.vp.View())
				for _, want := range []string{"Effective", tc.want, "Default", "Indefinite", "Overridden"} {
					if !strings.Contains(view, want) {
						t.Fatalf("missing %s", want)
					}
				}
			})
		}
		for _, key := range []string{"effective_days", "default_days"} {
			for _, bad := range []any{"30", true, 1.5, json.Number("9223372036854775808")} {
				if out, err := decodeTestPlan(t, object{source: object{key: bad}}); err == nil || out != nil {
					t.Fatal("malformed retention accepted")
				}
			}
		}
		for _, src := range []object{{}, {source: nil}, {source: object{}}} {
			out, err := decodeTestPlan(t, src)
			if err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"effective_days", "default_days"} {
				if accountRetentionValue(obj(out[source]), key) != "Unknown" {
					t.Fatal("missing source/property became indefinite")
				}
			}
		}
	}
}

func TestAccountFreshAccessAuthority(t *testing.T) {
	for _, change := range []string{"role", "operator", "sections", "revoked", "malformed", "wrong-account", "error"} {
		t.Run(change, func(t *testing.T) {
			f := newFake()
			m := testModel(t, f)
			selectAccount(t, m, "access")
			// Start an older context check. Access must fence even its queued result.
			old := m.accountAuthority()
			oldResult := old()
			f.read = func(ctx context.Context, r dashboard.ReadRequest) (json.RawMessage, error) {
				if r.Resource != dashboard.ResourceAccountAccess {
					return f.Source.Read(ctx, r)
				}
				c := demoAccountContext()
				switch change {
				case "role":
					c["role"] = "account_operator"
					c["sections"] = []string{"overview", "clients", "support", "access"}
				case "operator":
					c["operator_id"] = "op_changed"
				case "sections":
					c["sections"] = []string{"overview", "access"}
				case "revoked":
					c["available"] = false
				case "malformed":
					delete(c, "role")
				case "wrong-account":
					c["account_id"] = "acct_other"
				case "error":
					return nil, errors.New("PRIVATE error")
				}
				return encode(c)
			}
			read := m.accountRead(false)
			// Seed old private state to verify reconciliation itself discards it.
			m.account.ticket.Lock()
			m.account.ticket.id = "old"
			m.account.ticket.status = "ready"
			m.account.ticket.data = object{"body": "PRIVATE"}
			m.account.ticket.Unlock()
			m.account.data = object{"private": "PRIVATE"}
			m.account.vp.SetContent("PRIVATE")
			_, next := m.Update(read())
			ticketEmpty(t, m)
			if m.account.data != nil || strings.Contains(m.account.vp.View(), "PRIVATE") {
				t.Fatal("old private projection survived authority change")
			}
			switch change {
			case "role", "operator", "sections":
				if !m.account.context.available() {
					t.Fatal("valid authority was not reconciled")
				}
				if change == "role" && (m.account.context.role != "account_operator" || slices.Contains(m.account.context.sections, "billing")) {
					t.Fatal("stale owner navigation")
				}
				if change == "operator" && m.account.context.operator != "op_changed" {
					t.Fatal("stale operator")
				}
				if change == "sections" && len(m.account.context.sections) != 2 {
					t.Fatal("stale sections")
				}
				accountSettle(t, m, next)
			default:
				if m.account.context.available() || m.account.mode || strings.Contains(m.View(), "8 Account") {
					t.Fatal("invalid Access retained authority")
				}
			}
			before := m.account.context
			m.Update(oldResult)
			if !m.account.context.same(before) {
				t.Fatal("older context restored old authority")
			}
		})
	}
}

func TestAccountDelayedAccessCannotRestoreNewerContext(t *testing.T) {
	for _, change := range []string{"unchanged", "role", "revoked", "malformed"} {
		t.Run(change, func(t *testing.T) {
			f := newFake()
			m := testModel(t, f)
			selectAccount(t, m, "access")
			old := m.accountRead(false)
			oldResult := old()
			f.read = func(ctx context.Context, r dashboard.ReadRequest) (json.RawMessage, error) {
				if r.Resource != dashboard.ResourceAccountContext {
					return f.Source.Read(ctx, r)
				}
				c := demoAccountContext()
				switch change {
				case "role":
					c["role"] = "account_operator"
					c["sections"] = []string{"overview", "clients", "support", "access"}
				case "revoked":
					c["available"] = false
				case "malformed":
					c["sections"] = nil
				}
				return encode(c)
			}
			accountSettle(t, m, m.accountAuthority())
			before := m.account.context
			m.Update(oldResult)
			if !m.account.context.same(before) {
				t.Fatal("delayed Access restored older authority")
			}
			if m.account.busy {
				t.Fatal("stale Access left request busy")
			}
		})
	}
}

func TestAccountClientsRepeatScanIsIgnored(t *testing.T) {
	for _, scan := range []bool{false, true} {
		t.Run(fmt.Sprint(scan), func(t *testing.T) {
			f := newFake()
			m := testModel(t, f)
			selectAccount(t, m, "clients")
			started, release := make(chan context.Context, 1), make(chan struct{})
			f.read = func(ctx context.Context, r dashboard.ReadRequest) (json.RawMessage, error) {
				started <- ctx
				<-release
				return f.Source.Read(ctx, r)
			}
			cmd := m.accountRead(scan)
			done := make(chan tea.Msg, 1)
			go func() { done <- cmd() }()
			ctx := <-started
			generation := m.account.generation
			for i := 0; i < 3; i++ {
				_, repeat := m.Update(key("x"))
				if repeat != nil {
					t.Fatal("repeat scan queued")
				}
			}
			if ctx.Err() != nil || m.account.generation != generation {
				t.Fatal("repeat scan cancelled current request")
			}
			close(release)
			m.Update(<-done)
			want := 0
			if scan {
				want = 1
			}
			if f.count(dashboard.ResourceAccountClientsScan) != want {
				t.Fatal("duplicate scan")
			}
		})
	}
}

func TestAccountTicketDeadlineAndObsoleteResults(t *testing.T) {
	for _, action := range []string{"current", "selection", "revoked", "close"} {
		t.Run(action, func(t *testing.T) {
			f := newFake()
			m := testModel(t, f)
			selectAccount(t, m, "support")
			m.account.ticketTimeout = -time.Nanosecond // A real expired deadline, no sleeps.
			var expired context.Context
			f.read = func(ctx context.Context, r dashboard.ReadRequest) (json.RawMessage, error) {
				if r.Resource != dashboard.ResourceAccountSupportTicket {
					return f.Source.Read(ctx, r)
				}
				if ctx.Err() != nil {
					expired = ctx
					// A misbehaving source may return success/body after expiry.
					return f.Source.Read(context.Background(), r)
				}
				return f.Source.Read(ctx, r)
			}
			_, old := m.Update(key("enter"))
			switch action {
			case "selection":
				accountPress(t, m, "esc")
				accountPress(t, m, "j")
			case "revoked":
				m.dropAccount()
			case "close":
				m.Close()
			}
			if action == "selection" {
				m.account.ticketTimeout = 0
				accountPress(t, m, "enter")
			}
			m.Update(old())
			if expired == nil || !errors.Is(expired.Err(), context.DeadlineExceeded) {
				t.Fatal("fixture did not expire deadline")
			}
			m.account.ticket.Lock()
			id, status, data := m.account.ticket.id, m.account.ticket.status, m.account.ticket.data
			m.account.ticket.Unlock()
			switch action {
			case "current":
				if id == "" || status != "unavailable" || data != nil || strings.Contains(m.account.vp.View(), "Loading") {
					t.Fatal("deadline left loading or body")
				}
			case "selection":
				if id != "tkt_demo_limits" || status != "ready" || data == nil {
					t.Fatal("obsolete deadline altered new selection")
				}
			default:
				ticketEmpty(t, m)
			}
		})
	}
}

func TestAccountSupportRequiresFreshEnterAfterClear(t *testing.T) {
	for _, action := range []string{"r", "esc", "1", "?", "t", "w", "b", "role"} {
		t.Run(action, func(t *testing.T) {
			f := newFake()
			m := testModel(t, f)
			selectAccount(t, m, "support")
			accountPress(t, m, "enter")
			before := f.count(dashboard.ResourceAccountSupportTicket)
			if action == "role" {
				c := m.account.context
				c.role = "account_admin"
				_, cmd := m.Update(accountContextMsg{generation: m.account.authorityGeneration, context: c})
				accountSettle(t, m, cmd)
			} else {
				// Console commands use only the synthetic controller/no-controller path.
				_, cmd := m.Update(key(action))
				if action != "w" && action != "b" {
					accountSettle(t, m, cmd)
				}
			}
			ticketEmpty(t, m)
			m.overlay = ""
			// Use actual independent tick work, omitting only the next timer.
			for i := 0; i < 2; i++ {
				_, cmd := m.Update(accountTickMsg{})
				batch := cmd().(tea.BatchMsg)
				for _, child := range batch[1:] {
					accountSettle(t, m, child)
				}
			}
			selectAccount(t, m, "support")
			if f.count(dashboard.ResourceAccountSupportTicket) != before {
				t.Fatal("body reread without Enter")
			}
			ticketEmpty(t, m)
			accountPress(t, m, "enter")
			if f.count(dashboard.ResourceAccountSupportTicket) != before+1 {
				t.Fatal("fresh Enter failed")
			}
		})
	}
}

func TestAccountEffectiveCapOverridePreservesDefault(t *testing.T) {
	for _, effective := range []object{{}, {"agents": 0}} {
		out, err := decodeTestPlan(t, object{"limits": effective, "limit_defaults": object{"agents": 5}, "limits_units": object{"agents": "count"}, "limit_defaults_units": object{"agents": "count"}})
		if err != nil {
			t.Fatal(err)
		}
		want := "No plan cap"
		if len(effective) > 0 {
			want = "0 count"
		}
		if accountLimitValue(out, "limits", "agents") != want || accountLimitValue(out, "limit_defaults", "agents") != "5 count" {
			t.Fatal("effective override coalesced with default")
		}
	}
	out, err := decodeTestPlan(t, object{"transcript_retention": object{"effective_days": nil, "default_days": 30, "overridden": true}})
	if err != nil {
		t.Fatal(err)
	}
	retention := obj(out["transcript_retention"])
	if accountRetentionValue(retention, "effective_days") != "Indefinite" || accountRetentionValue(retention, "default_days") != "30 days" || !flag(retention, "overridden") {
		t.Fatal("retention override coalesced with default")
	}
}

func TestAccountTickFreshAccessWinsEarlierContext(t *testing.T) {
	f := newFake()
	m := testModel(t, f)
	selectAccount(t, m, "access")
	f.read = func(ctx context.Context, r dashboard.ReadRequest) (json.RawMessage, error) {
		if r.Resource == dashboard.ResourceAccountAccess {
			c := demoAccountContext()
			c["role"] = "account_operator"
			c["sections"] = []string{"overview", "clients", "support", "access"}
			return encode(c)
		}
		return f.Source.Read(ctx, r)
	}
	_, cmd := m.Update(accountTickMsg{})
	batch := cmd().(tea.BatchMsg)
	for _, child := range batch[1:] {
		accountSettle(t, m, child)
	}
	if m.account.context.role != "account_operator" || m.accountSection() != "access" || m.account.status != "ready" {
		t.Fatal("tick masked fresh Access authority")
	}
}
