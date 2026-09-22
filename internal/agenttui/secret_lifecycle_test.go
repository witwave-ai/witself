package agenttui

import (
	"context"
	"encoding/json"
	"errors"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/witwave-ai/witself/internal/dashboard"
	"strings"
	"testing"
)

func TestSecretRevealCopyAndExpiry(t *testing.T) {
	f := newFake()
	m := testModel(t, f)
	openPanel(t, m, 6)
	exact := "SYNTHETIC\tprivate\x1b[31mvalue"
	var owned []byte
	f.secret = func(_ context.Context, id, field string) ([]byte, error) {
		if id != "secret_preview" || field != "field_user" {
			t.Fatalf("wrong field %s/%s", id, field)
		}
		owned = []byte(exact)
		return owned, nil
	}
	if len(f.privateCalls) != 0 {
		t.Fatal("passive read")
	}
	copied := ""
	m.opts.Copy = func(_ context.Context, s string) error { copied = s; return nil }
	runPrivate(t, m, m.key(key("c")))
	if copied != exact || !allZero(owned) || strings.Contains(m.View(), "SYNTHETIC") {
		t.Fatal("copy changed or displayed plaintext")
	}
	msg := runPrivate(t, m, m.key(key("v")))
	text, _, _, status := m.privateView()
	if text != clean(exact) || status != "visible" || !allZero(owned) {
		t.Fatal("reveal did not sanitize/clear")
	}
	m.Update(privateExpiredMsg{generation: msg.generation - 1})
	if text, _, _, _ := m.privateView(); text == "" {
		t.Fatal("stale timer hid current reveal")
	}
	m.Update(privateExpiredMsg{generation: msg.generation})
	if text, _, _, _ := m.privateView(); text != "" || strings.Contains(m.vp.View(), "SYNTHETIC") {
		t.Fatal("expiry retained reveal")
	}
	runPrivate(t, m, m.key(key("v")))
	m.key(key("]"))
	if text, _, _, _ := m.privateView(); text != "" || strings.Contains(m.vp.View(), "SYNTHETIC") {
		t.Fatal("field change retained reveal")
	}
}

func TestSecretCanceledErrorAndOversizeBuffersCleared(t *testing.T) {
	for _, scenario := range []string{"navigate", "error", "oversize", "close", "refresh"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFake()
			m := testModel(t, f)
			openPanel(t, m, 6)
			started, release := make(chan struct{}), make(chan struct{})
			owned := []byte("PRIVATE_CANARY")
			if scenario == "oversize" {
				owned = make([]byte, (64<<10)+1)
			}
			f.secret = func(context.Context, string, string) ([]byte, error) {
				close(started)
				<-release
				if scenario == "error" {
					return owned, errors.New("private")
				}
				return owned, nil
			}
			cmd := m.privateRead(false)
			done := make(chan tea.Msg, 1)
			go func() { done <- cmd() }()
			<-started
			switch scenario {
			case "navigate":
				m.navigate(1)
			case "close":
				m.Close()
			case "refresh":
				m.key(key("r"))
			}
			close(release)
			m.Update(<-done)
			if !allZero(owned) {
				t.Fatal("secret buffer retained")
			}
			if text, _, _, _ := m.privateView(); text != "" || strings.Contains(m.View(), "PRIVATE_CANARY") || strings.Contains(m.vp.View(), "PRIVATE_CANARY") {
				t.Fatal("stale/failed secret retained")
			}
		})
	}
}

func TestSecretTOTPAndUnavailableClipboardNeverRead(t *testing.T) {
	f := newFake()
	m := testModel(t, f)
	openPanel(t, m, 6)
	if m.key(key("c")) != nil || len(f.privateCalls) != 0 {
		t.Fatal("missing clipboard triggered read")
	}
	fields := m.actionObjects()
	fields[0]["kind"] = "totp"
	m.opts.Copy = func(context.Context, string) error { t.Fatal("TOTP copied"); return nil }
	for _, k := range []string{"v", "c"} {
		if m.key(key(k)) != nil || len(f.privateCalls) != 0 {
			t.Fatal("TOTP seed accessed")
		}
	}
}

func TestSelectedRecordMutationHidesRevealedValue(t *testing.T) {
	for _, panel := range []int{2, 6} {
		m := testModel(t, newFake())
		openPanel(t, m, panel)
		runPrivate(t, m, m.privateRead(false))
		settle(t, m, m.refresh())
		if text, _, _, _ := m.privateView(); text == "" {
			t.Fatal("ordinary poll hid value")
		}
		cmd := m.refresh()
		msg := cmd().(fetchedMsg)
		for _, r := range msg.results {
			if panel == 2 && r.resource == dashboard.ResourceFacts {
				list(r.data, "facts")[0]["updated_at"] = "changed"
			}
			if panel == 6 && r.resource == dashboard.ResourceSecret {
				obj(r.data["secret"])["lifecycle"] = "archived"
			}
		}
		m.Update(msg)
		if text, _, _, _ := m.privateView(); text != "" {
			t.Fatal("mutation retained value")
		}
	}
}

func TestRecreatedFactCannotSatisfyStaleSelection(t *testing.T) {
	f := newFake()
	m := testModel(t, f)
	openPanel(t, m, 2)
	f.reveal = func(_ context.Context, s, p string) (json.RawMessage, error) {
		return encode(object{"fact": object{"id": "replacement", "subject": s, "predicate": p, "value": "PRIVATE_CANARY"}})
	}
	runPrivate(t, m, m.privateRead(false))
	if text, _, _, status := m.privateView(); text != "" || status != "unavailable" {
		t.Fatal("replacement fact revealed under old identity")
	}
}
