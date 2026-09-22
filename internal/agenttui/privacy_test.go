package agenttui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/witwave-ai/witself/internal/dashboard"
)

func TestUntrustedTextAndNarrowProjections(t *testing.T) {
	hostile := "safe\x1b]52;c;VE9LRU4=\a\x1b[31mred\x1b[0m\u009d0;title\u009c\u009b2J\x00\x7f\u202e\u2066\t界\nnext"
	got := clean(hostile)
	if got != "safered 界\nnext" {
		t.Fatalf("unexpected safe text %q", got)
	}
	for _, r := range got {
		if (unicode.IsControl(r) && r != '\n') || r == 0x202e || r == 0x2066 {
			t.Fatal("unsafe control retained")
		}
	}
	secret := object{"id": "s", "name": hostile, "fields": []any{object{"id": "f\x1b[0m", "name": hostile, "kind": "password", "sensitive": true, "value": "LEAK", "public_value": "LEAK"}}, "plaintext": "LEAK", "ciphertext": "LEAK", "future_secret": "LEAK"}
	email := object{"subject": hostile, "envelope_sender": "test@example.test", "id": "LEAK", "body": "LEAK", "headers": object{"From": "LEAK"}, "mime": "LEAK", "attachments": []any{object{"name": "LEAK"}}, "processing": object{"state": "pending", "claim_id": "LEAK"}, "read_state": object{"state": "unread", "body": "LEAK"}, "raw_size_bytes": object{"body": "LEAK"}, "provider_message_id": "LEAK", "next_cursor": "LEAK"}
	for _, tc := range []struct {
		r dashboard.Resource
		v object
	}{{dashboard.ResourceSecrets, object{"secrets": []any{secret}}}, {dashboard.ResourceSecret, object{"secret": secret, "vault_key": object{"id": "public-binding", "key_material": "LEAK"}}}, {dashboard.ResourceEmailReceived, object{"messages": []any{email}, "next_cursor": "LEAK"}}, {dashboard.ResourceEmailSent, object{"messages": []any{email}}}, {dashboard.ResourceMessages, object{"messages": []any{object{"id": "m", "subject": hostile, "body": "LEAK", "payload": "LEAK"}}}}, {dashboard.ResourceFacts, object{"facts": []any{object{"id": "f", "subject": "self", "predicate": "private", "sensitive": true, "value": "LEAK", "source_ref": "LEAK"}}}}, {dashboard.ResourceFactHistory, object{"assertions": []any{object{"sensitive": true, "value": "LEAK"}}}}} {
		raw, _ := encode(tc.v)
		projected, err := decode(tc.r, raw)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(projected)
		if strings.Contains(string(b), "LEAK") || strings.Contains(string(b), "VE9LRU4") {
			t.Fatalf("private/terminal data survived projection %d: %s", tc.r, b)
		}
	}
	// A forged public value nested in a metadata slot must not become a generic JSON dump.
	p := secret
	raw, _ := encode(object{"secret": p})
	out, _ := decode(dashboard.ResourceSecret, raw)
	if str(list(obj(out["secret"]), "fields")[0], "id") != "" {
		t.Fatal("hostile action identity was normalized into a valid ID")
	}
}
func TestHostileRenderedCellsStayBounded(t *testing.T) {
	f := newFake()
	hostile := "👩🏽‍💻界e\u0301\x1b]0;owned\a\x1b[2J\u202e\t" + strings.Repeat("界", 100)
	f.read = func(ctx context.Context, r dashboard.ReadRequest) (json.RawMessage, error) {
		if r.Resource == dashboard.ResourceSelf {
			s := demoSelf()
			obj(s["identity"])["agent_name"] = hostile
			obj(s["identity"])["realm_name"] = hostile
			return encode(s)
		}
		if r.Resource == dashboard.ResourceFacts {
			return encode(object{"facts": []any{object{"id": "fact_hostile", "subject": "self", "predicate": "label", "value": hostile, "sensitive": false}}})
		}
		return f.Source.Read(ctx, r)
	}
	m := testModel(t, f)
	openPanel(t, m, 2)
	for _, size := range [][2]int{{80, 24}, {120, 40}} {
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		view := m.View()
		if strings.Contains(view, "owned") || strings.Contains(view, "\x1b]0") || strings.Contains(view, "\x1b[2J") || strings.ContainsAny(view, "\u202e\t") {
			t.Fatal("remote terminal control escaped")
		}
		for _, line := range strings.Split(view, "\n") {
			if ansi.StringWidth(line) > size[0] {
				t.Fatal("cell-width overflow")
			}
		}
	}
}
func TestPassiveViewsNeverRevealOrPreview(t *testing.T) {
	f := newFake()
	f.read = func(ctx context.Context, r dashboard.ReadRequest) (json.RawMessage, error) {
		switch r.Resource {
		case dashboard.ResourceFacts:
			rows := demoFacts()
			obj(rows[2])["value"] = "PRIVATE-CANARY"
			return encode(object{"facts": rows})
		case dashboard.ResourceFactHistory:
			return encode(object{"assertions": []any{object{"value": "PRIVATE-CANARY", "source_kind": "user"}}})
		case dashboard.ResourceMessages:
			raw, err := f.Source.Read(ctx, r)
			if err != nil {
				return nil, err
			}
			var o object
			if err := json.Unmarshal(raw, &o); err != nil {
				return nil, err
			}
			for _, msg := range list(o, "messages") {
				msg["body"] = "BODY-CANARY"
				msg["payload"] = "PAYLOAD-CANARY"
			}
			return encode(o)
		}
		return f.Source.Read(ctx, r)
	}
	m := testModel(t, f)
	openPanel(t, m, 2)
	settle(t, m, m.choose(2))
	if m.current().key != "fact_private" {
		t.Fatal("wrong fixture selected")
	}
	for _, panel := range []int{2, 4, 6} {
		openPanel(t, m, panel)
		settle(t, m, m.refresh())
		b, _ := json.Marshal(m.states[panel].data)
		for _, canary := range []string{"PRIVATE-CANARY", "BODY-CANARY", "PAYLOAD-CANARY"} {
			if strings.Contains(string(b), canary) || strings.Contains(m.View(), canary) {
				t.Fatal("passive private leak")
			}
		}
	}
	if len(f.privateCalls) != 0 {
		t.Fatal("passive private read")
	}
}
func TestPrivateFactCopyExactAndDisplaySanitized(t *testing.T) {
	f := newFake()
	exact := "DEMO-value\x1b]52;c;bad\a\x1b[31mRED\x1b[0m\tend\r\n"
	var owned json.RawMessage
	f.reveal = func(_ context.Context, s, p string) (json.RawMessage, error) {
		owned, _ = encode(object{"fact": object{"id": "fact_private", "subject": s, "predicate": p, "value": exact}})
		return owned, nil
	}
	m := testModel(t, f)
	openPanel(t, m, 2)
	settle(t, m, m.choose(2))
	copied := ""
	m.opts.Copy = func(_ context.Context, s string) error { copied = s; return nil }
	msg := runPrivate(t, m, m.privateRead(true))
	if copied != exact {
		t.Fatal("copy used sanitized rather than exact value")
	}
	if !allZero(owned) {
		t.Fatal("raw fact response not cleared")
	}
	if strings.Contains(fmt.Sprintf("%+v", msg), "DEMO-value") || strings.Contains(m.View(), "DEMO-value") {
		t.Fatal("copy displayed value or put it in message")
	}
	runPrivate(t, m, m.privateRead(false))
	text, _, _, status := m.privateView()
	if text != clean(exact) || status != "visible" {
		t.Fatal("safe reveal missing")
	}
	if !allZero(owned) {
		t.Fatal("reveal buffer not cleared")
	}
	m.key(key("v"))
	if text, _, _, _ := m.privateView(); text != "" {
		t.Fatal("hide retained value")
	}
	if strings.Contains(m.vp.View(), "DEMO-value") {
		t.Fatal("hide left value in viewport")
	}
}
func TestSecretMetadataHasNoPassiveValueOperations(t *testing.T) {
	f := newFake()
	m := testModel(t, f)
	openPanel(t, m, 6)
	for _, k := range []string{"[", "]", "tab"} {
		if cmd := m.key(key(k)); cmd != nil {
			settle(t, m, cmd)
		}
	}
	settle(t, m, m.refresh())
	if len(f.privateCalls) != 0 {
		t.Fatal("passive field read")
	}
}
func allZero(b []byte) bool { return bytes.Equal(b, make([]byte, len(b))) }
func TestPrivateStaleAndErrorBuffersCleared(t *testing.T) {
	for _, action := range []string{"hide", "navigate", "refresh", "quit", "error", "cancel"} {
		t.Run(action, func(t *testing.T) {
			f := newFake()
			entered := make(chan context.Context, 1)
			release := make(chan struct{})
			owned, _ := encode(object{"fact": object{"id": "fact_name", "subject": "self", "predicate": "identity/name", "value": "NEVER-retain-this-synthetic-value"}})
			f.reveal = func(ctx context.Context, _, _ string) (json.RawMessage, error) {
				entered <- ctx
				<-release
				if action == "error" {
					return owned, errors.New("private error")
				}
				return owned, nil
			}
			m := testModel(t, f)
			openPanel(t, m, 2)
			cmd := m.privateRead(false)
			done := make(chan tea.Msg, 1)
			go func() { done <- cmd() }()
			ctx := <-entered
			if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 10*time.Second {
				t.Fatal("private read missing deadline")
			}
			switch action {
			case "hide":
				m.key(key("v"))
			case "navigate":
				m.navigate(1)
			case "refresh":
				m.key(key("r"))
			case "quit":
				m.Close()
			case "cancel":
				m.cancel()
			}
			if action != "error" && ctx.Err() == nil {
				t.Fatal("private request was not canceled")
			}
			close(release)
			msg := <-done
			m.Update(msg)
			if !allZero(owned) {
				t.Fatal("returned fact ownership not cleared")
			}
			text, _, _, _ := m.privateView()
			if text != "" || strings.Contains(m.View(), "NEVER-retain") || strings.Contains(m.vp.View(), "NEVER-retain") || strings.Contains(fmt.Sprintf("%+v", msg), "NEVER-retain") {
				t.Fatal("stale or failed private result survived")
			}
		})
	}
}
func TestPrivateManualRefreshAndQuit(t *testing.T) {
	m := testModel(t, newFake())
	openPanel(t, m, 2)
	runPrivate(t, m, m.privateRead(false))
	settle(t, m, m.key(key("r")))
	if text, _, _, _ := m.privateView(); text != "" {
		t.Fatal("refresh retained fact")
	}
	runPrivate(t, m, m.privateRead(false))
	m.Close()
	if strings.Contains(m.vp.View(), "Atlas") {
		t.Fatal("quit retained rendered value")
	}
}
func TestMessagePreviewLifecycleBoundsAndSentRefusal(t *testing.T) {
	f := newFake()
	fail := false
	oversize := false
	var owned json.RawMessage
	f.preview = func(ctx context.Context, _ string) (json.RawMessage, error) {
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 10*time.Second {
			t.Fatal("preview missing deadline")
		}
		if fail {
			return nil, errors.New("body error")
		}
		body := "DELIBERATE BODY\nSecond line"
		if oversize {
			body = strings.Repeat("X", (64<<10)+1)
		}
		owned, _ = encode(object{"body": body, "payload": "NEVER SHOW"})
		return owned, nil
	}
	m := testModel(t, f)
	openPanel(t, m, 4)
	if m.current().title != "Mira" {
		t.Fatal("newest conversation not first")
	}
	if len(m.current().messages) != 3 {
		t.Fatal("inbox and outbox not grouped")
	}
	runPrivate(t, m, m.key(key("v")))
	if !allZero(owned) {
		t.Fatal("preview buffer not cleared")
	}
	settle(t, m, m.refresh())
	if text, _, _, status := m.privateView(); text != "DELIBERATE BODY\nSecond line" || status != "visible" {
		t.Fatal("metadata refresh discarded body")
	}
	if strings.Contains(m.View(), "NEVER SHOW") {
		t.Fatal("preview payload leak")
	}
	m.key(key("]"))
	if text, _, _, _ := m.privateView(); text != "" {
		t.Fatal("message selection retained body")
	}
	n := len(f.privateCalls)
	if m.key(key("v")) != nil || len(f.privateCalls) != n {
		t.Fatal("sent body was requested")
	}
	m.key(key("]"))
	fail = true
	runPrivate(t, m, m.key(key("v")))
	n = len(f.privateCalls)
	settle(t, m, m.refresh())
	if len(f.privateCalls) != n {
		t.Fatal("failed preview retried automatically")
	}
	m.key(key("v"))
	fail = false
	oversize = true
	runPrivate(t, m, m.key(key("v")))
	if text, _, _, status := m.privateView(); text != "" || status != "unavailable" {
		t.Fatal("oversized preview accepted")
	}
	if !allZero(owned) {
		t.Fatal("oversized response not cleared")
	}
}
func TestCopyUnavailableAndPassiveOnlySource(t *testing.T) {
	f := newFake()
	m := testModel(t, f)
	openPanel(t, m, 2)
	if m.key(key("c")) != nil || len(f.privateCalls) != 0 || !strings.Contains(m.notice, "Clipboard unavailable") {
		t.Fatal("unavailable clipboard accessed value")
	}
	// This wrapper deliberately exports only the required Source interface.
	type passiveOnly struct{ Source }
	n := testModel(t, passiveOnly{NewDemoSource()})
	openPanel(t, n, 6)
	if n.key(key("v")) != nil || n.key(key("c")) != nil {
		t.Fatal("secret actions must never be present")
	}
}
