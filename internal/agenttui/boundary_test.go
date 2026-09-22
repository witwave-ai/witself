package agenttui

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/witwave-ai/witself/internal/dashboard"
)

func TestBoundaryControlSequences(t *testing.T) {
	input := "safe\x1b[31m RED\x1b[0m\x1b]52;c;evil\a\x1b]8;;https://evil\x1b\\link\x1b]8;;\x1b\\\u009b31m\u009d52;c;bad\u009c\x00\x7f\twide界\u202e\u2067\nnext"
	got := clean(input)
	for _, bad := range []string{"evil", "bad", "https://", "\x1b", "\u009d", "\u202e", "\t"} {
		if strings.Contains(got, bad) {
			t.Fatalf("unsafe text retained: %q", bad)
		}
	}
	if !strings.Contains(got, "\nnext") || !strings.Contains(got, "wide界") {
		t.Fatal("safe text was lost")
	}
	f := newFake()
	f.read = func(c context.Context, r dashboard.ReadRequest) (json.RawMessage, error) {
		if r.Resource == dashboard.ResourceSelf {
			s := demoSelf()
			s["identity"] = object{"agent_name": input, "realm_name": input}
			return encode(s)
		}
		return f.Source.Read(c, r)
	}
	m := testModel(t, f)
	openPanel(t, m, 0)
	for _, r := range ansi.Strip(m.View()) {
		if unicode.IsControl(r) && r != '\n' {
			t.Fatalf("terminal control survived: %U", r)
		}
	}
}
func FuzzBoundarySanitization(f *testing.F) {
	for _, s := range []string{"a\x1b[31mb", "\u009d52;c;foo\u009c", "\x1bPsecret\x1b\\", "\u202e界\t\n", "\xff"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		for _, r := range clean(s) {
			if unicode.IsControl(r) && r != '\n' {
				t.Fatalf("control %U", r)
			}
			if r >= 0x202a && r <= 0x202e || r >= 0x2066 && r <= 0x206f {
				t.Fatalf("bidi %U", r)
			}
		}
	})
}
func TestBoundaryFactsAndSensitiveHistory(t *testing.T) {
	f := newFake()
	f.read = func(c context.Context, r dashboard.ReadRequest) (json.RawMessage, error) {
		if r.Resource == dashboard.ResourceFacts {
			a := demoFacts()
			obj(a[2])["value"] = "PRIVATE_CANARY"
			return encode(object{"facts": a})
		}
		if r.Resource == dashboard.ResourceFactHistory {
			return encode(object{"assertions": []any{object{"value": "HISTORY_CANARY", "source_kind": "user"}}})
		}
		return f.Source.Read(c, r)
	}
	m := testModel(t, f)
	openPanel(t, m, 2)
	settle(t, m, m.choose(2))
	cache, _ := json.Marshal(m.states[2].data)
	if strings.Contains(string(cache), "PRIVATE_CANARY") || strings.Contains(string(cache), "HISTORY_CANARY") || len(f.privateCalls) != 0 {
		t.Fatal("passive facts leaked or revealed")
	}
	runPrivate(t, m, m.privateRead(false))
	text, _, _, state := m.privateView()
	if !strings.Contains(text, "Copper Finch") || state != "visible" {
		t.Fatal("explicit reveal failed")
	}
	cache, _ = json.Marshal(m.states[2].data)
	if strings.Contains(string(cache), "Copper Finch") {
		t.Fatal("reveal entered cache")
	}
	m.key(key("v"))
	text, _, _, _ = m.privateView()
	if text != "" {
		t.Fatal("hide retained fact")
	}
	runPrivate(t, m, m.privateRead(false))
	settle(t, m, m.refresh())
	text, _, _, _ = m.privateView()
	if text == "" {
		t.Fatal("unchanged passive poll hid deliberate reveal")
	}
}
func TestBoundaryExactCopyWithoutDisplay(t *testing.T) {
	f := newFake()
	m := testModel(t, f)
	openPanel(t, m, 2)
	settle(t, m, m.choose(2))
	if cmd := m.privateRead(true); cmd != nil {
		t.Fatal("copy read without clipboard callback")
	}
	if len(f.privateCalls) != 0 || !strings.Contains(m.notice, "Clipboard unavailable") {
		t.Fatal("missing clipboard explanation")
	}
	want := "exact\tvalue\nwith\x1b[31m data"
	f.reveal = func(_ context.Context, s, p string) (json.RawMessage, error) {
		return encode(object{"fact": object{"id": "fact_private", "subject": s, "predicate": p, "value": want}})
	}
	copied := ""
	m.opts.Copy = func(_ context.Context, s string) error { copied = s; return nil }
	runPrivate(t, m, m.privateRead(true))
	if copied != want {
		t.Fatal("clipboard did not receive exact value")
	}
	text, _, _, _ := m.privateView()
	if text != "" || strings.Contains(m.View(), "exact") {
		t.Fatal("copied value displayed")
	}
}
func TestBoundaryPrivateCancellation(t *testing.T) {
	for _, kind := range []string{"fact", "message"} {
		for _, action := range []string{"hide", "navigate", "close"} {
			t.Run(kind+"/"+action, func(t *testing.T) {
				f := newFake()
				m := testModel(t, f)
				panel := 2
				if kind == "message" {
					panel = 4
				}
				openPanel(t, m, panel)
				if panel == 2 {
					settle(t, m, m.choose(2))
				}
				started, release := make(chan struct{}), make(chan struct{})
				deadlineOK := false
				wait := func(c context.Context) {
					d, ok := c.Deadline()
					deadlineOK = ok && time.Until(d) <= 10*time.Second
					close(started)
					<-release
				}
				f.reveal = func(c context.Context, s, p string) (json.RawMessage, error) {
					wait(c)
					return encode(object{"fact": object{"id": "fact_private", "subject": s, "predicate": p, "value": "LATE_PRIVATE"}})
				}
				f.preview = func(c context.Context, _ string) (json.RawMessage, error) {
					wait(c)
					return encode(object{"body": "LATE_PRIVATE"})
				}
				cmd := m.privateRead(false)
				done := make(chan tea.Msg, 1)
				go func() { done <- cmd() }()
				<-started
				switch action {
				case "hide":
					m.key(key("v"))
				case "navigate":
					m.movePanel(0)
				case "close":
					m.Close()
				}
				if cmd := m.privateRead(false); cmd != nil {
					t.Fatal("overlapping private read")
				}
				close(release)
				msg := <-done
				m.Update(msg)
				text, _, _, _ := m.privateView()
				if !deadlineOK || text != "" || strings.Contains(m.View(), "LATE_PRIVATE") {
					t.Fatal("deadline or stale-private fence failure")
				}
			})
		}
	}
}
func TestBoundaryPreviewPassiveProjectionAndPersistence(t *testing.T) {
	f := newFake()
	f.read = func(c context.Context, r dashboard.ReadRequest) (json.RawMessage, error) {
		raw, e := f.Source.Read(c, r)
		if r.Resource != dashboard.ResourceMessages {
			return raw, e
		}
		var o object
		json.Unmarshal(raw, &o)
		for _, msg := range list(o, "messages") {
			msg["body"] = "PASSIVE_CANARY"
			msg["payload"] = object{"value": "PAYLOAD_CANARY"}
		}
		return encode(o)
	}
	m := testModel(t, f)
	openPanel(t, m, 4)
	cache, _ := json.Marshal(m.states[4].data)
	if strings.Contains(string(cache), "CANARY") || len(f.privateCalls) != 0 {
		t.Fatal("passive message body leak")
	}
	if len(m.filtered()) != 3 {
		t.Fatal("peer/broadcast/multiple groups conflated")
	}
	runPrivate(t, m, m.privateRead(false))
	text, _, _, status := m.privateView()
	if text == "" || status != "visible" {
		t.Fatal("explicit preview missing")
	}
	settle(t, m, m.refresh())
	next, _, _, _ := m.privateView()
	if text != next {
		t.Fatal("metadata refresh cleared preview")
	}
	m.moveAction(1)
	if c := m.privateRead(false); c != nil {
		t.Fatal("sent message allowed preview")
	}
	m.moveAction(-1)
	f.preview = func(context.Context, string) (json.RawMessage, error) {
		return encode(object{"body": strings.Repeat("x", (64<<10)+1)})
	}
	runPrivate(t, m, m.privateRead(false))
	text, _, _, status = m.privateView()
	if text != "" || status != "unavailable" {
		t.Fatal("oversize preview accepted")
	}
	before := len(f.privateCalls)
	settle(t, m, m.refresh())
	if len(f.privateCalls) != before {
		t.Fatal("preview auto retry")
	}
}

// Passive email and secret projections never retain values, even public fields.
func TestBoundaryPassiveSecretAndEmailProjection(t *testing.T) {
	for _, r := range []dashboard.Resource{dashboard.ResourceEmailReceived, dashboard.ResourceEmailSent, dashboard.ResourceSecret} {
		source := object{"id": "PRIVATE_ID", "body": "LEAK_BODY", "header_from": "LEAK_HEADER", "attachment_names": []any{"LEAK_NAME"}, "cursor": "LEAK_CURSOR", "subject": "Safe", "name": "Safe", "fields": []any{object{"name": "username", "kind": "text", "sensitive": false, "value": "LEAK_VALUE", "public_value": "LEAK_PUBLIC"}}}
		raw, _ := encode(object{"messages": []any{source}, "secret": source, "vault_key": object{"id": "public_binding", "ciphertext": "LEAK_CIPHER"}})
		p, e := decode(r, raw)
		if e != nil {
			t.Fatal(e)
		}
		b, _ := json.Marshal(p)
		if strings.Contains(string(b), "LEAK_") {
			t.Fatal("private projection retained unknown fields")
		}
		if r != dashboard.ResourceSecret && strings.Contains(string(b), "PRIVATE_ID") {
			t.Fatal("email identity retained")
		}
	}
}
func TestBoundaryCloseClearsRenderedPrivateState(t *testing.T) {
	m := testModel(t, newFake())
	openPanel(t, m, 2)
	settle(t, m, m.choose(2))
	runPrivate(t, m, m.privateRead(false))
	m.Close()
	text, _, _, _ := m.privateView()
	if text != "" || m.View() != "" || strings.Contains(m.vp.View(), "Copper Finch") {
		t.Fatal("close retained private render")
	}
}
func TestBoundaryInvalidActionIdentity(t *testing.T) {
	raw, _ := encode(object{"facts": []any{object{"id": "good", "subject": "self\x1b[0m", "predicate": "safe", "value": "public"}}})
	o, e := decode(dashboard.ResourceFacts, raw)
	if e != nil {
		t.Fatal(e)
	}
	if str(list(o, "facts")[0], "subject") != "" {
		t.Fatal("sanitization retargeted an exact fact action")
	}
}
