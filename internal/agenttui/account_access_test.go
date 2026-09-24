package agenttui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/witwave-ai/witself/internal/dashboard"
)

func TestAccountFirstAccessSupersededByContext(t *testing.T) {
	for _, reply := range []string{"success", "error", "forbidden"} {
		for _, check := range []string{"verified", "in-flight", "verified-then-in-flight", "in-flight-unavailable", "in-flight-revoked", "unavailable", "revoked", "verified-then-unavailable", "verified-then-revoked", "role", "operator", "sections"} {
			t.Run(reply+"/"+check, func(t *testing.T) {
				f := newFake()
				current := demoAccountContext()
				accessReply := reply
				contextError := false
				f.read = func(ctx context.Context, r dashboard.ReadRequest) (json.RawMessage, error) {
					switch r.Resource {
					case dashboard.ResourceAccountAccess:
						switch accessReply {
						case "error":
							return nil, errors.New("PRIVATE_ACCESS_ERROR")
						case "forbidden":
							return nil, &dashboard.ReaderError{Code: "forbidden"}
						}
						return encode(current)
					case dashboard.ResourceAccountContext:
						if contextError {
							return nil, errors.New("PRIVATE_CONTEXT_ERROR")
						}
						return encode(current)
					}
					return f.Source.Read(ctx, r)
				}
				m := testModel(t, f)
				selectAccount(t, m, "support")
				_, first := m.Update(key("]"))
				if first == nil || m.accountSection() != "access" || m.account.data != nil || m.account.status != "loading" {
					t.Fatal("fixture did not start first Access without cached data")
				}
				old := first().(accountDataMsg) // Hold delivery until after newer checks.
				old.data = object{"obsolete": "PRIVATE_OBSOLETE"}
				accessReply = "success"
				if strings.HasPrefix(check, "verified-then-") {
					accountSettle(t, m, m.accountAuthority())
				}
				switch check {
				case "unavailable", "verified-then-unavailable", "in-flight-unavailable":
					contextError = true
				case "revoked", "verified-then-revoked", "in-flight-revoked":
					current["available"] = false
				case "role":
					current["role"] = "account_operator"
					current["sections"] = []string{"overview", "clients", "support", "access"}
				case "operator":
					current["operator_id"] = "op_fresh"
				case "sections":
					current["sections"] = []string{"overview", "access"}
				}
				newer := m.accountAuthority()
				inFlight := strings.Contains(check, "in-flight")
				if !inFlight {
					accountSettle(t, m, newer)
				}
				before := m.account.context
				_, next := m.Update(old)
				if next != nil || m.account.busy || !m.account.context.same(before) {
					t.Fatal("obsolete Access changed authority, stayed busy, or queued an unbounded retry")
				}
				if m.account.data["obsolete"] != nil || strings.Contains(m.View(), "PRIVATE") {
					t.Fatal("obsolete Access data or errors survived")
				}
				switch check {
				case "unavailable", "revoked", "verified-then-unavailable", "verified-then-revoked":
					if m.account.context.available() || m.account.mode || m.account.data != nil || strings.Contains(m.View(), "8 Account") {
						t.Fatal("failed authority retained Account")
					}
					return
				case "in-flight", "verified-then-in-flight", "in-flight-unavailable", "in-flight-revoked":
					if m.account.status != "unavailable" || m.account.data != nil || !m.account.authorityBusy || !strings.Contains(m.account.vp.View(), "Unavailable") {
						t.Fatal("a started check was treated as current successful verification")
					}
					accountSettle(t, m, newer)
					if check == "in-flight-unavailable" || check == "in-flight-revoked" {
						m.Update(old)
						if m.account.context.available() || m.account.mode || m.account.data != nil {
							t.Fatal("late failed Context retained or restored Access")
						}
						return
					}
				default:
					if m.account.status != "ready" || m.account.data == nil || !strings.Contains(m.account.vp.View(), "Available read sections") {
						t.Fatalf("verified Context did not settle Access: %s", m.account.status)
					}
				}
				// Every surviving state supports a fresh deliberate read, even paused.
				m.paused = true
				beforeReads := f.count(dashboard.ResourceAccountAccess)
				accountPress(t, m, "r")
				if m.account.status != "ready" || m.account.busy || !m.account.context.same(before) || f.count(dashboard.ResourceAccountAccess) != beforeReads+1 {
					t.Fatal("fresh deliberate refresh did not recover Access")
				}
				m.Update(old)
				if m.account.status != "ready" || !m.account.context.same(before) || m.account.data["obsolete"] != nil {
					t.Fatal("old completion changed deliberate refresh")
				}
			})
		}
	}
}

func TestAccountSlowFirstAccessIndependentTicks(t *testing.T) {
	for _, paused := range []bool{false, true} {
		for _, reply := range []string{"success", "error", "forbidden"} {
			t.Run(fmt.Sprintf("paused=%t/%s", paused, reply), func(t *testing.T) {
				f := newFake()
				m := testModel(t, f)
				selectAccount(t, m, "support")
				started := make(chan context.Context, 1)
				release := make(chan struct{})
				t.Cleanup(func() {
					select {
					case <-release:
					default:
						close(release)
					}
				})
				f.read = func(ctx context.Context, r dashboard.ReadRequest) (json.RawMessage, error) {
					if r.Resource == dashboard.ResourceAccountAccess {
						started <- ctx
						<-release
						switch reply {
						case "error":
							return nil, errors.New("PRIVATE_ACCESS_ERROR")
						case "forbidden":
							return nil, &dashboard.ReaderError{Code: "forbidden"}
						}
					}
					return f.Source.Read(ctx, r)
				}
				_, first := m.Update(key("]"))
				done := make(chan tea.Msg, 1)
				go func() { done <- first() }()
				var accessContext context.Context
				select {
				case accessContext = <-started:
				case <-time.After(5 * time.Second):
					t.Fatal("Access did not start")
				}
				m.paused = paused
				before := f.count(dashboard.ResourceAccountContext)
				// Inject three periodic ticks while Access remains blocked. Do not
				// run the timer command: ordering is deterministic without sleeps.
				for i := 0; i < 3; i++ {
					_, tick := m.Update(accountTickMsg{})
					batch := tick().(tea.BatchMsg)
					for _, child := range batch[1:] {
						accountSettle(t, m, child)
					}
					if f.count(dashboard.ResourceAccountContext) != before+i+1 || !m.account.busy || accessContext.Err() != nil || f.count(dashboard.ResourceAccountAccess) != 1 {
						t.Fatal("slow Access or pause blocked independent checks or duplicated reads")
					}
				}
				close(release)
				select {
				case msg := <-done:
					m.Update(msg)
				case <-time.After(5 * time.Second):
					t.Fatal("released Access did not finish")
				}
				if m.account.busy || m.account.status != "ready" || m.account.data == nil || !m.account.context.available() || strings.Contains(m.account.vp.View(), "Loading") {
					t.Fatalf("completed first Access did not use verified Context: %s", m.account.status)
				}
			})
		}
	}
}
