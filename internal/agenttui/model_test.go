package agenttui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/witwave-ai/witself/internal/dashboard"
)

// All fixtures are synthetic; no test constructs a live adapter or reads a home.
type fakeSource struct {
	Source
	mu           sync.Mutex
	calls        []dashboard.ReadRequest
	privateCalls []string
	read         func(context.Context, dashboard.ReadRequest) (json.RawMessage, error)
	reveal       func(context.Context, string, string) (json.RawMessage, error)
	preview      func(context.Context, string) (json.RawMessage, error)
	secret       func(context.Context, string, string) ([]byte, error)
	store        func(context.Context, string) (json.RawMessage, error)
}

func newFake() *fakeSource { return &fakeSource{Source: NewDemoSource()} }
func (f *fakeSource) Read(ctx context.Context, r dashboard.ReadRequest) (json.RawMessage, error) {
	q := url.Values{}
	for k, v := range r.Query {
		q[k] = append([]string(nil), v...)
	}
	f.mu.Lock()
	f.calls = append(f.calls, dashboard.ReadRequest{Resource: r.Resource, ID: r.ID, Query: q})
	f.mu.Unlock()
	if f.read != nil {
		return f.read(ctx, r)
	}
	return f.Source.Read(ctx, r)
}
func (f *fakeSource) recordPrivate(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.privateCalls = append(f.privateCalls, s)
}
func (f *fakeSource) RevealFact(ctx context.Context, s, p string) (json.RawMessage, error) {
	f.recordPrivate("fact:" + s + ":" + p)
	if f.reveal != nil {
		return f.reveal(ctx, s, p)
	}
	return f.Source.RevealFact(ctx, s, p)
}
func (f *fakeSource) PreviewMessage(ctx context.Context, id string) (json.RawMessage, error) {
	f.recordPrivate("message:" + id)
	if f.preview != nil {
		return f.preview(ctx, id)
	}
	return f.Source.PreviewMessage(ctx, id)
}
func (f *fakeSource) RevealSecret(ctx context.Context, id, field string) ([]byte, error) {
	f.recordPrivate("secret:" + id + ":" + field)
	if f.secret != nil {
		return f.secret(ctx, id, field)
	}
	return f.Source.(SecretSource).RevealSecret(ctx, id, field)
}
func (f *fakeSource) StoreTheme(ctx context.Context, s string) (json.RawMessage, error) {
	if f.store != nil {
		return f.store(ctx, s)
	}
	return f.Source.StoreTheme(ctx, s)
}
func (f *fakeSource) count(r dashboard.Resource) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.Resource == r {
			n++
		}
	}
	return n
}
func (f *fakeSource) last(r dashboard.Resource) dashboard.ReadRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.calls) - 1; i >= 0; i-- {
		if f.calls[i].Resource == r {
			return f.calls[i]
		}
	}
	return dashboard.ReadRequest{}
}
func testModel(t *testing.T, f Source) *Model {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	t.Setenv("WITSELF_HOME", t.TempDir())
	m := New(context.Background(), f, Options{Demo: true})
	t.Cleanup(m.Close)
	return m
}
func settle(t *testing.T, m *Model, cmd tea.Cmd) {
	t.Helper()
	for n := 0; cmd != nil; n++ {
		if n > 8 {
			t.Fatal("unbounded refresh chain")
		}
		msg := cmd()
		switch msg.(type) {
		case fetchedMsg, themeMsg:
			_, cmd = m.Update(msg)
			if _, ok := msg.(privateMsg); ok {
				return
			} // timer expiry is driven explicitly, never by wall-clock waiting
		default:
			t.Fatalf("unexpected synchronous command: %T", msg)
		}
	}
}
func openPanel(t *testing.T, m *Model, p int) {
	t.Helper()
	if p == m.panel {
		settle(t, m, m.refresh())
	} else {
		settle(t, m, m.navigate(p))
	}
}
func key(k string) tea.KeyMsg {
	switch k {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "ctrl+c":
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	case "backspace":
		return tea.KeyMsg{Type: tea.KeyBackspace}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
}
func runPrivate(t *testing.T, m *Model, cmd tea.Cmd) privateMsg {
	t.Helper()
	if cmd == nil {
		t.Fatal("missing deliberate private read")
	}
	msg, ok := cmd().(privateMsg)
	if !ok {
		t.Fatal("wrong private response type")
	}
	m.Update(msg)
	return msg
}

func TestDemoSevenPanelsDimensionsAndThemes(t *testing.T) {
	for _, theme := range themeNames {
		for _, size := range [][2]int{{80, 24}, {120, 40}, {42, 18}} {
			for p := range panelNames {
				t.Run(fmt.Sprintf("%s/%dx%d/%s", theme, size[0], size[1], panelNames[p]), func(t *testing.T) {
					m := New(context.Background(), NewDemoSource(), Options{Demo: true, Theme: theme})
					defer m.Close()
					openPanel(t, m, p)
					got := m.Snapshot(size[0], size[1])
					lines := strings.Split(got, "\n")
					if len(lines) != size[1] {
						t.Fatalf("height %d, want %d", len(lines), size[1])
					}
					for i, l := range lines {
						if n := ansi.StringWidth(l); n != size[0] {
							t.Fatalf("line %d width %d, want %d", i, n, size[0])
						}
					}
					if len(m.filtered()) == 0 {
						t.Fatal("demo panel has no inventory")
					}
					if m.busy {
						t.Fatal("snapshot left an unexecuted request")
					}
					if !strings.Contains(ansi.Strip(got), "Atlas") {
						t.Fatal("missing agent identity")
					}
				})
			}
		}
	}
}
func TestFilterFocusAndNavigation(t *testing.T) {
	m := testModel(t, newFake())
	openPanel(t, m, 2)
	m.key(key("/"))
	for _, r := range "short walkthrough" {
		settle(t, m, m.key(key(string(r))))
	}
	settle(t, m, m.key(key("enter")))
	if len(m.filtered()) != 1 || m.current().key != "fact_review" {
		t.Fatal("public fact values must be searchable, including spaces")
	}
	m.key(key("tab"))
	if m.focus != 1 {
		t.Fatal("detail not focused")
	}
	settle(t, m, m.navigate(3))
	settle(t, m, m.navigate(2))
	if m.states[2].filter != "short walkthrough" || m.current().key != "fact_review" {
		t.Fatal("lost filter/selection on navigation")
	}
	m.key(key("/"))
	settle(t, m, m.key(key("zzzz")))
	if len(m.filtered()) != 0 || !strings.Contains(m.View(), "No matching items") {
		t.Fatal("missing empty search state")
	}
	settle(t, m, m.key(key("esc")))
	if len(m.filtered()) != 1 {
		t.Fatal("escape did not restore filter")
	}
}
func TestSelectionScrollAndTranscriptIncrementalTail(t *testing.T) {
	f := newFake()
	reordered := false
	extra := false
	f.read = func(ctx context.Context, r dashboard.ReadRequest) (json.RawMessage, error) {
		if r.Resource == dashboard.ResourceTranscripts && reordered {
			rows := demoTranscripts()
			rows[0], rows[1] = rows[1], rows[0]
			return encode(object{"transcripts": rows})
		}
		if r.Resource == dashboard.ResourceTranscript && extra {
			return encode(object{"transcript": object{"id": r.ID}, "entries": []any{object{"sequence": 5, "role": "assistant", "body": "All checks passed."}, object{"sequence": 6, "role": "user", "body": "Keep the view steady."}}})
		}
		return f.Source.Read(ctx, r)
	}
	m := testModel(t, f)
	openPanel(t, m, 1)
	if r := f.last(dashboard.ResourceTranscript); r.Query.Get("tail") != "true" || r.Query.Get("limit") != "200" {
		t.Fatalf("first tail bounds: %+v", r)
	}
	settle(t, m, m.choose(1))
	m.focus = 1
	m.vp.SetYOffset(3)
	offset := m.vp.YOffset
	reordered = true
	extra = true
	settle(t, m, m.refresh())
	if m.current().key != "tr_fieldguide" || m.states[1].selected != 0 || m.focus != 1 || m.vp.YOffset != offset {
		t.Fatal("refresh moved the reader")
	}
	r := f.last(dashboard.ResourceTranscript)
	if r.Query.Get("after_sequence") != "5" || r.Query.Get("tail") != "" {
		t.Fatalf("not incremental: %+v", r)
	}
	es := list(m.states[1].data[dashboard.ResourceTranscript], "entries")
	if len(es) != 6 {
		t.Fatalf("tail did not deduplicate: %d", len(es))
	}
	m.vp.GotoBottom()
	settle(t, m, m.refresh())
	if !m.vp.AtBottom() {
		t.Fatal("live tail lost bottom position")
	}
	m.key(key("["))
	m.key(key("]"))
	m.key(key("]"))
	m.key(key("x"))
	if !m.expanded[3] {
		t.Fatal("JSON expansion not tied to selected entry")
	}
}
func TestPollSingleFlightCancelsAndRejectsStale(t *testing.T) {
	f := newFake()
	entered := make(chan context.Context, 1)
	release := make(chan struct{})
	once := sync.Once{}
	f.read = func(ctx context.Context, r dashboard.ReadRequest) (json.RawMessage, error) {
		blocked := false
		once.Do(func() { blocked = true })
		if blocked {
			entered <- ctx
			<-release
			return encode(demoSelf())
		}
		return f.Source.Read(ctx, r)
	}
	m := testModel(t, f)
	cmd := m.refresh()
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	var ctx context.Context
	select {
	case ctx = <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("read not started")
	}
	for i := 0; i < 5; i++ {
		if m.refresh() != nil {
			t.Fatal("overlapping refresh")
		}
		m.Update(tickMsg{})
	}
	if m.navigate(2) != nil {
		t.Fatal("obsolete fetch must settle before replacement")
	}
	if ctx.Err() == nil {
		t.Fatal("navigation failed to cancel")
	}
	if f.count(dashboard.ResourceSelf) != 1 {
		t.Fatal("poll overlapped")
	}
	close(release)
	msg := <-done
	_, next := m.Update(msg)
	settle(t, m, next)
	if m.panel != 2 || m.selfStatus != "ready" || len(m.states[0].data) != 0 || len(m.filtered()) != 3 {
		t.Fatal("stale completion was installed")
	}
	if m.busy {
		t.Fatal("fetch remained busy")
	}
}
func TestDisabledEmailClearsInboundButSentWorks(t *testing.T) {
	f := newFake()
	disabled := false
	f.read = func(ctx context.Context, r dashboard.ReadRequest) (json.RawMessage, error) {
		if r.Resource == dashboard.ResourceSelf {
			s := demoSelf()
			if disabled {
				obj(s["email_checkpoint"])["enabled"] = false
				obj(obj(s["plan_entitlements"])["features"])["agent_email_receive"] = false
			}
			return encode(s)
		}
		return f.Source.Read(ctx, r)
	}
	m := testModel(t, f)
	openPanel(t, m, 5)
	if len(m.filtered()) != 2 {
		t.Fatal("missing received fixtures")
	}
	n := f.count(dashboard.ResourceEmailReceived)
	disabled = true
	settle(t, m, m.refresh())
	settle(t, m, m.refresh())
	if f.count(dashboard.ResourceEmailReceived) != n || len(m.filtered()) != 0 || m.states[5].data[dashboard.ResourceEmailAddress] != nil {
		t.Fatal("disabled inbox retained data or kept polling")
	}
	if !strings.Contains(m.View(), "Disabled") {
		t.Fatal("missing disabled state")
	}
	settle(t, m, m.switchEmail())
	if len(m.filtered()) != 2 || m.states[5].status[dashboard.ResourceEmailSent] != "ready" {
		t.Fatal("sent coupled to received feature")
	}
	disabled = false
	settle(t, m, m.switchEmail())
	if len(m.filtered()) != 2 || f.count(dashboard.ResourceEmailReceived) != n+1 {
		t.Fatal("inbox did not resume after self enabled it")
	}
}
func TestAvailabilityAndRetryGate(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{{&dashboard.ReaderError{Status: 403, Code: "feature_not_enabled"}, "disabled"}, {&dashboard.ReaderError{Status: 403, Code: "unenrolled"}, "unenrolled"}, {&dashboard.ReaderError{Status: 501}, "unsupported"}, {errors.New("PRIVATE upstream error"), "unavailable"}} {
		t.Run(tc.want, func(t *testing.T) {
			f := newFake()
			f.read = func(ctx context.Context, r dashboard.ReadRequest) (json.RawMessage, error) {
				if r.Resource == dashboard.ResourceSelf {
					return encode(object{})
				}
				if r.Resource == dashboard.ResourceSecrets {
					return nil, tc.err
				}
				return f.Source.Read(ctx, r)
			}
			m := testModel(t, f)
			openPanel(t, m, 6)
			settle(t, m, m.refresh())
			if m.states[6].status[dashboard.ResourceSecrets] != tc.want {
				t.Fatal("wrong availability classification")
			}
			if strings.Contains(m.View(), "PRIVATE") {
				t.Fatal("raw error leak")
			}
			want := 1
			if tc.want == "unavailable" {
				want = 2
			}
			if f.count(dashboard.ResourceSecrets) != want {
				t.Fatal("retry gate not respected")
			}
		})
	}
	f := newFake()
	fail := false
	f.read = func(ctx context.Context, r dashboard.ReadRequest) (json.RawMessage, error) {
		if fail {
			return nil, errors.New("synthetic outage")
		}
		return f.Source.Read(ctx, r)
	}
	m := testModel(t, f)
	openPanel(t, m, 2)
	fail = true
	settle(t, m, m.refresh())
	if m.states[2].status[dashboard.ResourceFacts] != "stale" || m.selfStatus != "stale" || len(m.filtered()) != 3 {
		t.Fatal("temporary outage lost last good view")
	}
}
func TestEvidenceJumpAndSalientOutsideInventory(t *testing.T) {
	f := newFake()
	f.read = func(ctx context.Context, r dashboard.ReadRequest) (json.RawMessage, error) {
		if r.Resource == dashboard.ResourceMemories {
			return encode(object{"items": []any{demoMemories()[1]}})
		}
		return f.Source.Read(ctx, r)
	}
	m := testModel(t, f)
	openPanel(t, m, 0)
	settle(t, m, m.openSalient())
	if m.panel != 3 || m.current().key != "mem_wayfinding" || str(obj(m.states[3].data[dashboard.ResourceMemory]["memory"]), "id") != "mem_wayfinding" {
		t.Fatal("salient link opened wrong memory")
	}
	m.key(key("e"))
	settle(t, m, m.key(key("enter")))
	req := f.last(dashboard.ResourceTranscript)
	if req.ID != "tr_wayfinding" || req.Query.Get("after_sequence") != "3" || req.Query.Get("limit") != "500" {
		t.Fatalf("wrong evidence window: %+v", req)
	}
	if m.evidenceFrom != 4 || m.evidenceUntil != 6 || !strings.Contains(ansi.Strip(m.vp.View()), "Evidence #4–6") {
		t.Fatal("evidence range not shown")
	}
	settle(t, m, m.key(key("esc")))
	if m.panel != 3 || m.current().key != "mem_wayfinding" {
		t.Fatal("evidence return lost memory")
	}
}
func TestThemePreferenceRaceAndLocalFallback(t *testing.T) {
	f := newFake()
	stored := ""
	f.store = func(ctx context.Context, s string) (json.RawMessage, error) {
		stored = s
		return nil, errors.New("private error")
	}
	m := testModel(t, f)
	m.key(key("t"))
	m.overlayIndex = 2
	settle(t, m, m.key(key("enter")))
	m.Update(prefsMsg{revision: 0, theme: "amber"})
	if m.theme != "paper" || stored != "paper" || !strings.Contains(m.notice, "locally") {
		t.Fatal("deliberate theme overwritten or fallback missing")
	}
	for _, s := range []string{"", "PAPER", "paper\n", "../../../x"} {
		if ValidTheme(s) {
			t.Fatal("invalid theme accepted")
		}
	}
	if got := m.Snapshot(80, 24); got != "Snapshots require NewDemoSource." {
		t.Fatal("snapshot called arbitrary source")
	}
}
func TestBoundedTranscriptRetention(t *testing.T) {
	a := []any{}
	for i := 1; i <= 1500; i++ {
		a = append(a, object{"sequence": i, "body": strings.Repeat("z", 3000)})
	}
	// Decode numbers just as real projected JSON arrives.
	raw, _ := encode(object{"entries": a})
	page, err := decode(dashboard.ResourceTranscript, raw)
	if err != nil {
		t.Fatal(err)
	}
	merged := mergeEntries(nil, page)
	entries := list(merged, "entries")
	size := 0
	for _, e := range entries {
		size += len(str(e, "body"))
	}
	if len(entries) > 1000 || size > 2<<20 || num(entries[len(entries)-1], "sequence") != 1500 {
		t.Fatal("retention budget or tail ordering violated")
	}
}
func TestDemoVisualSamples(t *testing.T) {
	if !testing.Verbose() {
		return
	}
	for _, p := range []int{0, 6} {
		m := testModel(t, NewDemoSource())
		openPanel(t, m, p)
		t.Logf("%s 80x24\n%s", panelNames[p], m.Snapshot(80, 24))
	}
	m := testModel(t, NewDemoSource())
	openPanel(t, m, 1)
	t.Logf("Transcripts 120x40\n%s", m.Snapshot(120, 40))
}

func TestEmailAuxiliaryOutageShowsStaleness(t *testing.T) {
	for _, resource := range []dashboard.Resource{dashboard.ResourceEmailAddress, dashboard.ResourceEmailStatus} {
		f := newFake()
		fail := false
		f.read = func(ctx context.Context, r dashboard.ReadRequest) (json.RawMessage, error) {
			if fail && r.Resource == resource {
				return nil, &dashboard.ReaderError{Status: 503}
			}
			return f.Source.Read(ctx, r)
		}
		m := testModel(t, f)
		openPanel(t, m, 5)
		fail = true
		settle(t, m, m.refresh())
		if !strings.Contains(m.vp.View(), "Stale") || m.selfStatus != "ready" {
			t.Fatal("auxiliary stale data presented as current")
		}
	}
}
