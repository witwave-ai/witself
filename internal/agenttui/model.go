package agenttui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/witwave-ai/witself/internal/dashboard"
)

// Source is the only authority boundary. Private reads require explicit keys.
type Source interface {
	Read(context.Context, dashboard.ReadRequest) (json.RawMessage, error)
	RevealFact(context.Context, string, string) (json.RawMessage, error)
	PreviewMessage(context.Context, string) (json.RawMessage, error)
	StoreTheme(context.Context, string) (json.RawMessage, error)
}

// Options configures presentation. Copy must honor cancellation. It receives
// the exact selected value without sanitization. Applications own clipboard
// clearing and sync policy; the UI never emits OSC52 or prints a fallback.
type Options struct {
	Agent, Realm, Version, Theme string
	PollInterval                 time.Duration
	Demo                         bool
	Copy                         func(context.Context, string) error
	// Console is optional. The caller owns its Close lifecycle after the TUI exits.
	Console ConsoleController
}

var panelNames = []string{"Overview", "Transcripts", "Facts", "Memories", "Conversations", "Email", "Secrets"}
var themeNames = []string{"auto", "console", "paper", "midnight", "amber", "high-contrast"}

// ValidTheme reports whether s names a supported theme.
func ValidTheme(s string) bool {
	for _, n := range themeNames {
		if s == n {
			return true
		}
	}
	return false
}

type panelState struct {
	rows             []item
	data             map[dashboard.Resource]object
	status           map[dashboard.Resource]string
	selected         int
	key, filter, pin string
	scroll           int
}
type privateState struct {
	sync.Mutex
	generation              uint64
	text, kind, key, status string
	cancel                  context.CancelFunc
}

// Model implements tea.Model. Run it with tea.WithAltScreen() in the caller;
// Bubble Tea then restores the terminal on normal quit, interrupt, and errors.
type Model struct {
	stateMu                       sync.Mutex
	ctx                           context.Context
	cancel                        context.CancelFunc
	source                        Source
	opts                          Options
	closed                        atomic.Bool
	private                       privateState
	privateBusy                   bool
	console                       consoleModel
	account                       accountModel
	states                        [7]panelState
	self                          object
	selfStatus                    string
	summaryView, summaryRow       int
	summaryASCII                  bool
	panel, focus, sub             int // focus: inventory, detail scroll, detail actions
	subKey                        string
	width, height                 int
	vp                            viewport.Model
	generation                    uint64
	busy, wanted, paused, started bool
	fetchCancel                   context.CancelFunc
	blocked                       map[dashboard.Resource]string
	theme                         string
	themeRevision                 uint64
	themeSaving                   bool
	savedTheme                    string
	overlay                       string
	overlayIndex, helpScroll      int
	filterEditing                 bool
	filterBefore                  string
	notice                        string
	sent, unread, unacked         bool
	expanded                      map[int]bool
	evidenceFrom, evidenceUntil   int
	returnMemory                  string
	actionOffsets                 []int
	transcriptReader              transcriptReaderPosition
}

var _ tea.Model = (*Model)(nil)

// New creates a terminal workspace using the supplied console source.
func New(ctx context.Context, src Source, o Options) *Model {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	if o.PollInterval <= 0 {
		o.PollInterval = 2 * time.Second
	}
	if o.PollInterval < 250*time.Millisecond {
		o.PollInterval = 250 * time.Millisecond
	}
	theme := o.Theme
	if !ValidTheme(theme) {
		theme = "auto"
	}
	m := &Model{ctx: ctx, cancel: cancel, source: src, opts: o, width: 80, height: 24, theme: theme, self: object{}, blocked: map[dashboard.Resource]string{}, expanded: map[int]bool{}, vp: viewport.New(40, 15), selfStatus: "loading"}
	m.account.vp = viewport.New(76, 15)
	m.summaryRow = 1 // Start on the first category with a defined activity metric.
	if ValidTheme(o.Theme) {
		m.themeRevision = 1
	}
	for i := range m.states {
		m.states[i].data = map[dashboard.Resource]object{}
		m.states[i].status = map[dashboard.Resource]string{}
	}
	m.resize()
	return m
}

// Close cancels all outstanding calls and discards ephemeral private values.
// It is idempotent and safe for the caller's deferred cleanup.
func (m *Model) Close() {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	m.closeLocked()
}
func (m *Model) closeLocked() {
	if m.closed.Swap(true) {
		return
	}
	m.cancel()
	m.clearAccountData()
	m.account.authorityGeneration++
	if m.account.authorityCancel != nil {
		m.account.authorityCancel()
	}
	m.console.generation++
	if m.console.cancel != nil {
		m.console.cancel()
	}
	m.clearPrivate()
	m.vp.SetContent("")
}
func (m *Model) clearPrivate() {
	m.private.Lock()
	defer m.private.Unlock()
	m.private.generation++
	if m.private.cancel != nil {
		m.private.cancel()
		m.private.cancel = nil
	}
	m.private.text = ""
	m.private.key = ""
	m.private.kind = ""
	m.private.status = ""
}
func (m *Model) privateView() (string, string, string, string) {
	m.private.Lock()
	defer m.private.Unlock()
	return m.private.text, m.private.kind, m.private.key, m.private.status
}

// Init starts the initial refresh, preference read, and polling timer.
func (m *Model) Init() tea.Cmd {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	if m.started || m.closed.Load() {
		return nil
	}
	m.started = true
	rev := m.themeRevision
	src := m.source
	ctx := m.ctx
	prefs := func() tea.Msg {
		if src == nil {
			return prefsMsg{revision: rev}
		}
		c, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		raw, err := src.Read(c, dashboard.ReadRequest{Resource: dashboard.ResourcePreferences})
		if err != nil {
			return prefsMsg{revision: rev}
		}
		o, _ := decode(dashboard.ResourcePreferences, raw)
		return prefsMsg{revision: rev, theme: str(obj(o["prefs"]), "theme")}
	}
	return tea.Batch(m.tick(), m.refresh(), prefs, m.consoleTick(), m.consoleRun(consoleCheck), m.accountTick(), m.accountAuthority())
}

type tickMsg struct{}

func (m *Model) tick() tea.Cmd {
	return tea.Tick(m.opts.PollInterval, func(time.Time) tea.Msg { return tickMsg{} })
}

type prefsMsg struct {
	revision uint64
	theme    string
}
type themeMsg struct {
	theme string
	err   bool
}
type readResult struct {
	resource dashboard.Resource
	data     object
	status   string
}
type fetchedMsg struct {
	generation uint64
	results    []readResult
}
type privateMsg struct {
	generation uint64
}

func errorState(err error) string {
	if err == nil {
		return "ready"
	}
	var re *dashboard.ReaderError
	if errors.As(err, &re) {
		switch re.Code {
		case "feature_not_enabled", "feature_disabled", "disabled":
			return "disabled"
		case "not_enrolled", "unenrolled", "agent_not_enrolled":
			return "unenrolled"
		case "unsupported", "not_supported":
			return "unsupported"
		}
		if re.Status == 501 {
			return "unsupported"
		}
		if re.Status == 403 {
			return "disabled"
		}
	}
	return "unavailable"
}
func enabled(self object, r dashboard.Resource) (bool, bool) {
	feature := ""
	checkpoint := ""
	switch r {
	case dashboard.ResourceFacts, dashboard.ResourceFactHistory:
		feature = "facts"
	case dashboard.ResourceMemories, dashboard.ResourceMemory, dashboard.ResourceMemoryHistory:
		feature = "memory"
		checkpoint = "memory_checkpoint"
	case dashboard.ResourceMessages:
		feature = "messaging"
		checkpoint = "message_checkpoint"
	case dashboard.ResourceSecrets, dashboard.ResourceSecret:
		feature = "secrets"
	case dashboard.ResourceEmailAddress, dashboard.ResourceEmailStatus, dashboard.ResourceEmailReceived:
		feature = "agent_email_receive"
		checkpoint = "email_checkpoint"
	case dashboard.ResourceEmailSent:
		feature = "agent_email_send"
	}
	if checkpoint != "" {
		c := obj(self[checkpoint])
		if !flag(c, "unavailable") {
			if b, ok := c["enabled"].(bool); ok {
				return b, true
			}
		}
	}
	e := obj(self["plan_entitlements"])
	if str(e, "state") == "applied" {
		if b, ok := obj(e["features"])[feature].(bool); ok {
			return b, true
		}
	}
	return false, false
}
func primary(panel int, sent bool) dashboard.Resource {
	switch panel {
	case 0:
		return dashboard.ResourceSelf
	case 1:
		return dashboard.ResourceTranscripts
	case 2:
		return dashboard.ResourceFacts
	case 3:
		return dashboard.ResourceMemories
	case 4:
		return dashboard.ResourceMessages
	case 5:
		if sent {
			return dashboard.ResourceEmailSent
		}
		return dashboard.ResourceEmailReceived
	default:
		return dashboard.ResourceSecrets
	}
}
func (m *Model) current() *item {
	rows := m.filtered()
	s := &m.states[m.panel]
	if len(rows) == 0 {
		return nil
	}
	s.selected = max(0, min(s.selected, len(rows)-1))
	return &rows[s.selected]
}
func (m *Model) filtered() []item {
	s := &m.states[m.panel]
	if s.filter == "" {
		return s.rows
	}
	out := []item{}
	q := strings.ToLower(single(s.filter))
	for _, r := range s.rows {
		if strings.Contains(r.search, q) {
			out = append(out, r)
		}
	}
	return out
}
func (m *Model) refresh() tea.Cmd {
	if m.closed.Load() {
		return nil
	}
	if m.busy {
		m.wanted = true
		return nil
	}
	if m.source == nil {
		m.selfStatus = "unavailable"
		m.states[m.panel].status[primary(m.panel, m.sent)] = "unavailable"
		return nil
	}
	m.busy = true
	m.wanted = false
	ctx, cancel := context.WithCancel(m.ctx)
	m.fetchCancel = cancel
	gen, panel, sent, unread, unacked := m.generation, m.panel, m.sent, m.unread, m.unacked
	src := m.source
	knownSelf := m.self
	locked := map[dashboard.Resource]string{}
	for k, v := range m.blocked {
		locked[k] = v
	}
	var selected object
	selectedID := ""
	if row := m.current(); row != nil {
		selected = row.data
		selectedID = row.key
	}
	after := 0
	for _, entry := range list(m.states[panel].data[dashboard.ResourceTranscript], "entries") {
		after = max(after, num(entry, "sequence"))
	}
	from := m.evidenceFrom
	return func() tea.Msg {
		defer cancel()
		out := fetchedMsg{generation: gen}
		selfReady := false
		read := func(r dashboard.Resource, id string, q url.Values) object {
			if ctx.Err() != nil {
				return nil
			}
			if r != dashboard.ResourceSelf {
				b, known := enabled(knownSelf, r)
				if known && !b {
					out.results = append(out.results, readResult{resource: r, status: "disabled"})
					return nil
				}
				if reason := locked[r]; reason != "" && (!selfReady || !known || !b) {
					out.results = append(out.results, readResult{resource: r, status: reason})
					return nil
				}
			}
			callCtx, done := context.WithTimeout(ctx, 10*time.Second)
			raw, err := src.Read(callCtx, dashboard.ReadRequest{Resource: r, ID: id, Query: q})
			if callCtx.Err() != nil {
				err = callCtx.Err()
			}
			done()
			state := errorState(err)
			var data object
			if err == nil {
				data, err = decode(r, raw)
				if err != nil {
					state = "unavailable"
				}
			}
			// Assertion values stay out of the cache whenever sensitivity is unknown.
			if r == dashboard.ResourceFactHistory && (selected == nil || flag(selected, "sensitive") || flag(selected, "redacted")) {
				for _, a := range list(data, "assertions") {
					delete(a, "value")
				}
			}
			out.results = append(out.results, readResult{r, data, state})
			return data
		}
		if s := read(dashboard.ResourceSelf, "", nil); s != nil {
			knownSelf = s
			selfReady = true
		}
		q := url.Values{"limit": {"100"}}
		switch panel {
		case 0:
			read(dashboard.ResourceSummary, "", nil)
		case 1:
			read(dashboard.ResourceTranscripts, "", nil)
			if selectedID != "" {
				tq := url.Values{"limit": {"200"}}
				if after > 0 {
					tq.Set("after_sequence", strconv.Itoa(after))
				} else if from > 0 {
					tq.Set("after_sequence", strconv.Itoa(max(0, from-1)))
					tq.Set("limit", "500")
				} else {
					tq.Set("tail", "true")
				}
				read(dashboard.ResourceTranscript, selectedID, tq)
			}
		case 2:
			f := read(dashboard.ResourceFacts, "", q)
			if f != nil {
				selected = nil
				for _, v := range list(f, "facts") {
					if str(v, "id") == selectedID {
						selected = v
						break
					}
				}
			}
			if selectedID != "" && selected != nil {
				read(dashboard.ResourceFactHistory, selectedID, url.Values{"subject": {str(selected, "subject")}, "predicate": {str(selected, "predicate")}})
			}
		case 3:
			inventory := read(dashboard.ResourceMemories, "", q)
			selected = nil
			for _, mem := range list(inventory, "items") {
				if str(mem, "id") == selectedID {
					selected = mem
					break
				}
			}
			if selected == nil && selfReady {
				for _, mem := range list(knownSelf, "salient_memories") {
					if str(mem, "id") == selectedID {
						selected = mem
						break
					}
				}
			}
			if selectedID != "" && selected != nil {
				if flag(selected, "sensitive") || flag(selected, "redacted") {
					out.results = append(out.results, readResult{dashboard.ResourceMemory, object{"memory": selected}, "ready"}, readResult{dashboard.ResourceMemoryHistory, object{}, "ready"})
				} else {
					read(dashboard.ResourceMemory, selectedID, nil)
					read(dashboard.ResourceMemoryHistory, selectedID, url.Values{"limit": {"50"}})
				}
			}
		case 4:
			a := read(dashboard.ResourceMessages, "", url.Values{"direction": {"inbox"}, "limit": {"100"}})
			aIndex := len(out.results) - 1
			b := read(dashboard.ResourceMessages, "", url.Values{"direction": {"outbox"}, "limit": {"100"}})
			if a != nil && b != nil {
				msgs := []any{}
				for _, dir := range []struct {
					d    object
					name string
				}{{a, "received"}, {b, "sent"}} {
					for _, v := range list(dir.d, "messages") {
						v["_dir"] = dir.name
						msgs = append(msgs, v)
					}
				}
				out.results = out.results[:aIndex]
				out.results = append(out.results, readResult{dashboard.ResourceMessages, object{"messages": msgs}, "ready"})
			} else if aIndex >= 0 && aIndex < len(out.results) {
				failure := out.results[aIndex]
				if a != nil && len(out.results) > aIndex+1 {
					failure = out.results[aIndex+1]
				}
				if failure.status == "ready" {
					failure = readResult{resource: dashboard.ResourceMessages, status: "unavailable"}
				}
				out.results = append(out.results[:aIndex], failure)
			}

		case 5:
			if sent {
				read(dashboard.ResourceEmailSent, "", q)
			} else {
				read(dashboard.ResourceEmailAddress, "", nil)
				read(dashboard.ResourceEmailStatus, "", nil)
				if unread {
					q.Set("unread", "true")
				}
				if unacked {
					q.Set("unacked", "true")
				}
				read(dashboard.ResourceEmailReceived, "", q)
			}
		case 6:
			read(dashboard.ResourceSecrets, "", q)
			if selectedID != "" {
				read(dashboard.ResourceSecret, selectedID, nil)
			}
		}
		return out
	}
}
func (m *Model) invalidate() {
	m.generation++
	if m.fetchCancel != nil {
		m.fetchCancel()
	}
	m.clearPrivate()
	m.sub = 0
	m.subKey = ""
	m.expanded = map[int]bool{}
}
func (m *Model) navigate(panel int) tea.Cmd {
	if !m.movePanel(panel) {
		return nil
	}
	return m.refresh()
}
func (m *Model) movePanel(panel int) bool {
	if panel < 0 || panel >= 7 || panel == m.panel {
		return false
	}
	if m.panel == 1 && m.focus != 0 {
		m.setDetailFocus(0)
	}
	m.states[m.panel].scroll = m.vp.YOffset
	m.invalidate()
	m.panel = panel
	m.focus = 0
	m.filterEditing = false
	m.evidenceFrom = 0
	m.evidenceUntil = 0
	m.returnMemory = ""
	m.vp.SetYOffset(m.states[panel].scroll)
	m.resize()
	m.renderDetail(false)
	return true
}
func (m *Model) choose(delta int) tea.Cmd {
	s := &m.states[m.panel]
	rows := m.filtered()
	if len(rows) == 0 {
		return nil
	}
	next := max(0, min(s.selected+delta, len(rows)-1))
	if next == s.selected {
		return nil
	}
	s.pin = ""
	m.invalidate()
	s.selected = next
	s.key = rows[next].key
	m.resetDetail()
	m.renderDetail(false)
	return m.refresh()
}
func (m *Model) resetDetail() {
	if m.panel == 1 {
		m.transcriptReader = transcriptReaderPosition{}
	}
	s := &m.states[m.panel]
	for _, r := range []dashboard.Resource{dashboard.ResourceTranscript, dashboard.ResourceFactHistory, dashboard.ResourceMemory, dashboard.ResourceMemoryHistory, dashboard.ResourceSecret} {
		delete(s.data, r)
		delete(s.status, r)
	}
	m.evidenceFrom = 0
	m.evidenceUntil = 0
	m.vp.GotoTop()
}
func (m *Model) reconcileRows() {
	s := &m.states[m.panel]
	oldKey := s.key
	s.rows = makeRows(m.panel, s.data, m.self, m.sent)
	if s.pin != "" {
		found := false
		for _, r := range s.rows {
			if r.key == s.pin {
				found = true
				break
			}
		}
		if !found {
			s.rows = append(s.rows, makeItem(s.pin, s.pin, "Linked detail", object{"id": s.pin}))
		}
	}
	rows := m.filtered()
	if m.panel != 5 && oldKey != "" {
		for i, r := range rows {
			if r.key == oldKey {
				s.selected = i
				break
			}
		}
	}
	s.selected = max(0, min(s.selected, len(rows)-1))
	s.key = ""
	if len(rows) > 0 {
		s.key = rows[s.selected].key
	}
}
func mergeEntries(old, inc object) object {
	entries := map[int]object{}
	for _, page := range []object{old, inc} {
		for _, e := range list(page, "entries") {
			if num(e, "sequence") > 0 {
				entries[num(e, "sequence")] = e
			}
		}
	}
	seqs := make([]int, 0, len(entries))
	for seq := range entries {
		seqs = append(seqs, seq)
	}
	sort.Ints(seqs)
	start := max(0, len(seqs)-1000)
	total := 0
	for i := len(seqs) - 1; i >= start; i-- {
		total += len(str(entries[seqs[i]], "body"))
		if total > 2<<20 {
			start = i + 1
			break
		}
	}
	a := []any{}
	for _, seq := range seqs[start:] {
		a = append(a, entries[seq])
	}
	return object{"transcript": inc["transcript"], "entries": a}
}

func (m *Model) applyFetch(msg fetchedMsg) tea.Cmd {
	m.busy = false
	m.fetchCancel = nil
	if msg.generation != m.generation {
		return m.refresh()
	}
	s := &m.states[m.panel]
	oldKey := s.key
	oldPrivateRevision := m.privateRevision()
	atBottom := m.vp.AtBottom()
	hadEntries := len(list(s.data[dashboard.ResourceTranscript], "entries")) > 0
	for _, r := range msg.results {
		if r.resource == dashboard.ResourceSelf {
			m.selfStatus = r.status
			if r.status == "ready" {
				m.self = r.data
			} else if len(m.self) > 0 {
				m.selfStatus = "stale"
			}
			continue
		}
		s.status[r.resource] = r.status
		switch r.status {
		case "ready":
			delete(m.blocked, r.resource)
			if r.resource == dashboard.ResourceTranscript {
				s.data[r.resource] = mergeEntries(s.data[r.resource], r.data)
			} else {
				s.data[r.resource] = r.data
			}
		case "disabled", "unenrolled", "unsupported":
			delete(s.data, r.resource)
			m.blocked[r.resource] = r.status
		default:
			if s.data[r.resource] != nil {
				s.status[r.resource] = "stale"
			}
		}
	}
	// Self policy is authoritative even when a lane is not the active email tab.
	for i := range m.states {
		for r := range m.states[i].data {
			if b, known := enabled(m.self, r); known && !b {
				delete(m.states[i].data, r)
				m.states[i].status[r] = "disabled"
				m.blocked[r] = "disabled"
			}
		}
	}
	if b, known := enabled(m.self, primary(m.panel, m.sent)); known && !b {
		m.clearPrivate()
	}
	m.reconcileRows()
	if !bytes.Equal(oldPrivateRevision, m.privateRevision()) {
		m.clearPrivate()
	}
	changed := oldKey != s.key
	if changed && (m.panel != 0 || m.summaryView == 3) {
		m.clearPrivate()
		m.sub = 0
		m.subKey = ""
		m.resetDetail()
	}
	oldSub := m.subKey
	m.reconcileSub()
	if oldSub != "" && oldSub != m.subKey {
		m.clearPrivate()
	}
	m.renderDetail(m.panel == 1 && m.focus != 0 && hadEntries && atBottom && m.evidenceFrom == 0)
	if m.panel == 1 && m.focus != 0 && !hadEntries {
		if m.evidenceFrom > 0 {
			for i, entry := range m.actionObjects() {
				seq := num(entry, "sequence")
				if seq >= m.evidenceFrom && seq <= m.evidenceUntil {
					m.sub, m.subKey = i, actionKey(entry)
					break
				}
			}
			m.renderDetail(false)
		}
		m.revealAction()
		if m.actionCount() > 0 {
			m.transcriptReader.opened = true
			m.transcriptReader.key = s.key
		}
	}
	if m.wanted {
		return m.refresh()
	}
	if changed && s.key != "" && (m.panel == 1 || m.panel == 2 || m.panel == 3 || m.panel == 6) {
		return m.refresh()
	}
	return nil
}

// Private values survive a passive poll only while the selected record's
// relevant metadata remains unchanged. Usage counters do not affect identity.
func (m *Model) privateRevision() []byte {
	row := m.current()
	if row == nil {
		return nil
	}
	var metadata object
	switch m.panel {
	case 2:
		metadata = pick(row.data, "id subject predicate updated_at sensitive redacted value")
	case 3:
		metadata = object{"row": pick(row.data, "id updated_at version sensitive redacted state"), "detail": pick(obj(m.states[3].data[dashboard.ResourceMemory]["memory"]), "id updated_at version sensitive redacted state")}
	case 6:
		metadata = secret(obj(m.states[6].data[dashboard.ResourceSecret]["secret"]))
	default:
		return nil
	}
	raw, _ := json.Marshal(metadata)
	return raw
}

func (m *Model) storeTheme() tea.Cmd {
	if m.themeSaving || m.source == nil {
		return nil
	}
	m.themeSaving = true
	theme := m.theme
	src := m.source
	ctx := m.ctx
	return func() tea.Msg {
		c, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		_, err := src.StoreTheme(c, theme)
		return themeMsg{theme: theme, err: err != nil || c.Err() != nil}
	}
}

// Private values never ride in tea.Msg or enter passive caches. Only a generation
// notification reaches Update. Owned buffers are wiped on every return path.
func (m *Model) privateRead(copyOnly bool) tea.Cmd {
	row := m.current()
	if row == nil || m.closed.Load() {
		return nil
	}
	kind, key, subject, predicate, fieldID := "", "", "", "", ""
	if m.panel == 2 {
		kind = "fact"
		subject = str(row.data, "subject")
		predicate = str(row.data, "predicate")
		key = row.key
		if subject == "" || predicate == "" || key == "" {
			return nil
		}
	}
	if m.panel == 3 && !copyOnly {
		kind, key = "memory", row.key
	}
	if m.panel == 4 && !copyOnly {
		messages := row.messages
		if m.sub >= len(messages) {
			return nil
		}
		msg := messages[m.sub]
		if str(msg, "_dir") != "received" {
			m.notice = "Sent messages expose metadata only."
			return nil
		}
		kind = "message"
		key = str(msg, "id")
	}
	if m.panel == 6 {
		fields := m.actionObjects()
		if m.sub >= len(fields) {
			m.notice = "Select a loaded field with [ and ]."
			return nil
		}
		field := fields[m.sub]
		fieldID = str(field, "id")
		if strings.EqualFold(str(field, "kind"), "totp") {
			m.notice = "TOTP enrollment seeds cannot be revealed or copied."
			return nil
		}
		if fieldID == "" {
			return nil
		}
		if _, ok := m.source.(SecretSource); !ok {
			m.notice = "Secret reveal is unavailable in this adapter."
			return nil
		}
		kind = "secret"
		key = row.key + "/" + fieldID
	}
	if kind == "" || key == "" {
		return nil
	}
	if copyOnly && m.opts.Copy == nil {
		m.notice = "Clipboard unavailable: no safe clipboard callback is configured."
		return nil
	}
	_, currentKind, currentKey, currentStatus := m.privateView()
	if !copyOnly && currentKind == kind && currentKey == key && currentStatus != "" {
		m.clearPrivate()
		m.renderDetail(false)
		return nil
	}
	if m.privateBusy {
		m.notice = "A private read is finishing; try again when it settles."
		return nil
	}
	m.clearPrivate()
	m.privateBusy = true
	ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
	m.private.Lock()
	gen := m.private.generation
	m.private.cancel = cancel
	m.private.kind = kind
	m.private.key = key
	m.private.status = "loading"
	m.private.Unlock()
	src := m.source
	copyFn := m.opts.Copy
	secretID := row.key
	m.renderDetail(false)
	return func() tea.Msg {
		defer cancel()
		finish := func(display, status string) tea.Msg {
			m.private.Lock()
			defer m.private.Unlock()
			if m.private.generation == gen && !m.closed.Load() {
				m.private.text = display
				m.private.status = status
			}
			return privateMsg{generation: gen}
		}
		if src == nil {
			return finish("", "unavailable")
		}
		var exact string
		if kind == "secret" {
			raw, err := src.(SecretSource).RevealSecret(ctx, secretID, fieldID)
			defer clear(raw)
			if err != nil || ctx.Err() != nil || len(raw) > 64<<10 {
				return finish("", "unavailable")
			}
			exact = string(raw)
		} else {
			var raw json.RawMessage
			var err error
			switch kind {
			case "fact":
				raw, err = src.RevealFact(ctx, subject, predicate)
			case "memory":
				raw, err = src.Read(ctx, dashboard.ReadRequest{Resource: dashboard.ResourceMemory, ID: key})
			default:
				raw, err = src.PreviewMessage(ctx, key)
			}
			defer clear(raw)
			if err != nil || ctx.Err() != nil || len(raw) > 8<<20 || !json.Valid(raw) {
				return finish("", "unavailable")
			}
			var o object
			d := json.NewDecoder(bytes.NewReader(raw))
			d.UseNumber()
			if d.Decode(&o) != nil {
				return finish("", "unavailable")
			}
			switch kind {
			case "memory":
				mem := obj(o["memory"])
				b, ok := mem["content"].(string)
				if !ok || str(mem, "id") != key || flag(mem, "redacted") {
					return finish("", "unavailable")
				}
				exact = b
			case "message":
				b, ok := o["body"].(string)
				if !ok {
					return finish("", "unavailable")
				}
				exact = b
			default:
				f := obj(o["fact"])
				if str(f, "id") != key || str(f, "subject") != subject || str(f, "predicate") != predicate {
					return finish("", "unavailable")
				}
				v, ok := f["value"]
				if !ok {
					return finish("", "unavailable")
				}
				exact = exactValue(v)
			}
			if len(exact) > 64<<10 {
				return finish("", "unavailable")
			}
		}
		m.private.Lock()
		valid := m.private.generation == gen && !m.closed.Load() && ctx.Err() == nil
		m.private.Unlock()
		if !valid {
			return privateMsg{generation: gen}
		}
		if copyOnly {
			if copyFn == nil || copyFn(ctx, exact) != nil || ctx.Err() != nil {
				return finish("", "copy failed")
			}
			return finish("", "copied")
		}
		return finish(clean(exact), "visible")
	}
}
func exactValue(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// Update applies terminal events and generation-fenced source results.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	if m.closed.Load() {
		return m, nil
	}
	switch msg := msg.(type) {
	case accountTickMsg:
		authority := m.accountAuthority()
		var read tea.Cmd
		m.account.ticket.Lock()
		selected := m.account.ticket.id != ""
		m.account.ticket.Unlock()
		if !m.paused && !selected && m.account.status != "response_too_large" {
			read = m.accountRead(false)
		}
		return m, tea.Batch(m.accountTick(), authority, read)
	case accountContextMsg:
		return m, m.accountContextResult(msg)
	case accountDataMsg:
		return m, m.accountDataResult(msg)
	case accountTicketMsg:
		m.account.ticket.Lock()
		valid := msg.generation == m.account.ticket.generation
		m.account.ticket.Unlock()
		if valid {
			if msg.forbidden {
				m.dropAccount()
			} else {
				m.renderAccount()
			}
		}
		return m, nil
	case consoleTickMsg:
		return m, tea.Batch(m.consoleTick(), m.consoleRun(consoleCheck))
	case consoleMsg:
		return m, m.consoleResult(msg)
	case tea.WindowSizeMsg:
		m.width = max(1, msg.Width)
		m.height = max(1, msg.Height)
		m.resize()
		m.renderDetail(false)
	case tickMsg:
		if m.ctx.Err() != nil {
			m.closeLocked()
			return m, tea.Quit
		}
		var cmd tea.Cmd
		if !m.paused && !m.busy {
			cmd = m.refresh()
		}
		return m, tea.Batch(m.tick(), cmd)
	case fetchedMsg:
		return m, m.applyFetch(msg)
	case prefsMsg:
		if m.themeRevision == msg.revision && m.themeRevision == 0 && ValidTheme(msg.theme) {
			m.theme = msg.theme
			m.renderDetail(false)
		}
	case themeMsg:
		m.themeSaving = false
		m.savedTheme = msg.theme
		if msg.err {
			m.notice = "Theme kept locally; saving the shared preference failed."
		} else {
			m.notice = "Theme saved to shared console preferences."
		}
		if msg.theme != m.theme {
			return m, m.storeTheme()
		}
	case privateMsg:
		m.privateBusy = false
		m.private.Lock()
		valid := msg.generation == m.private.generation
		expire := valid && m.private.status == "visible" && m.private.kind != "message"
		if valid {
			switch m.private.status {
			case "copied":
				m.notice = "Copied without display. Clipboard may sync; the application owns clearing."
				m.private.status = ""
			case "copy failed":
				m.notice = "Clipboard copy failed; no value was displayed."
				m.private.status = ""
			}
		}
		m.private.Unlock()
		m.renderDetail(false)
		if expire {
			return m, tea.Tick(30*time.Second, func(time.Time) tea.Msg { return privateExpiredMsg(msg) })
		}
	case privateExpiredMsg:
		m.private.Lock()
		valid := msg.generation == m.private.generation
		m.private.Unlock()
		if valid {
			m.clearPrivate()
			m.notice = "Private value hidden after 30 seconds."
			m.renderDetail(false)
		}
	case tea.KeyMsg:
		return m, m.key(msg)
	}
	return m, nil
}
func (m *Model) key(msg tea.KeyMsg) tea.Cmd {
	k := msg.String()
	if k == "ctrl+c" {
		m.closeLocked()
		return tea.Quit
	}
	if m.filterEditing {
		s := &m.states[m.panel]
		switch k {
		case "enter":
			m.filterEditing = false
		case "esc":
			s.filter = m.filterBefore
			m.filterEditing = false
		case "backspace", "ctrl+h":
			r := []rune(s.filter)
			if len(r) > 0 {
				s.filter = string(r[:len(r)-1])
			}
		case "ctrl+u":
			s.filter = ""
		default:
			if msg.Type == tea.KeyRunes && len(s.filter) < 512 {
				s.filter += strings.ReplaceAll(clean(string(msg.Runes)), "\n", " ")
			}
		}
		m.invalidate()
		s.selected = 0
		m.reconcileRows()
		m.resetDetail()
		m.renderDetail(false)
		return m.refresh()
	}
	if m.overlay == "console" {
		return m.consoleKey(k)
	}
	if m.overlay != "" {
		switch k {
		case "esc", "q", "?":
			m.overlay = ""
		case "up", "k":
			if m.overlay == "theme" {
				m.overlayIndex = max(0, m.overlayIndex-1)
			} else {
				m.helpScroll = max(0, m.helpScroll-1)
			}
		case "down", "j":
			if m.overlay == "theme" {
				m.overlayIndex = min(len(themeNames)-1, m.overlayIndex+1)
			} else {
				m.helpScroll++
			}
		case "pgdown", "ctrl+d":
			m.helpScroll += max(1, m.height-10)
		case "pgup", "ctrl+u":
			m.helpScroll = max(0, m.helpScroll-max(1, m.height-10))
		case "enter":
			if m.overlay == "theme" {
				m.theme = themeNames[m.overlayIndex]
				m.themeRevision++
				m.overlay = ""
				m.renderDetail(false)
				return m.storeTheme()
			}
		}
		return nil
	}
	if m.account.mode {
		return m.accountKey(k)
	}
	if k == "8" {
		return m.openAccount()
	}
	if m.panel == 0 {
		if handled, cmd := m.summaryKey(k); handled {
			return cmd
		}
	}
	switch k {
	case "b", "w":
		m.clearPrivate()
		m.renderDetail(false)
		if k == "w" {
			m.overlay = "console"
			return m.consoleRun(consoleCheck)
		}
		return m.consoleRun(consoleOpen)
	case "q":
		m.closeLocked()
		return tea.Quit
	case "1", "2", "3", "4", "5", "6", "7":
		p, _ := strconv.Atoi(k)
		return m.navigate(p - 1)
	case "?":
		m.overlay = "help"
		m.helpScroll = 0
	case "t":
		m.overlay = "theme"
		for i, n := range themeNames {
			if m.theme == n {
				m.overlayIndex = i
			}
		}
	case "p":
		m.paused = !m.paused
		if m.paused {
			m.notice = "Live refresh paused."
		} else {
			m.notice = "Live refresh resumed."
			return m.refresh()
		}
	case "r":
		m.clearPrivate()
		m.renderDetail(false)
		return m.refresh()
	case "/":
		m.filterEditing = true
		m.filterBefore = m.states[m.panel].filter
		if m.panel == 1 {
			m.setDetailFocus(0)
		} else {
			m.focus = 0
		}
	case "tab":
		focus := (m.focus + 1) % 3
		if m.actionCount() == 0 && focus == 2 {
			focus = 0
		}
		m.setDetailFocus(focus)
		if m.focus == 2 {
			m.revealAction()
		}
	case "shift+tab":
		focus := (m.focus + 2) % 3
		if m.actionCount() == 0 && focus == 2 {
			focus = 1
		}
		m.setDetailFocus(focus)
		if m.focus == 2 {
			m.revealAction()
		}
	case "esc":
		m.clearPrivate()
		if m.returnMemory != "" {
			id := m.returnMemory
			m.movePanel(3)
			m.selectID(id)
			return m.refresh()
		}
		if m.focus != 0 {
			m.setDetailFocus(0)
			return nil
		} else if m.states[m.panel].filter != "" {
			m.states[m.panel].filter = ""
			m.reconcileRows()
			m.resetDetail()
			return m.refresh()
		}
		m.renderDetail(false)
	case "enter":
		if m.panel == 0 {
			return m.openSalient()
		}
		if m.focus == 2 {
			return m.activateAction()
		}
		firstRead := m.panel == 1 && m.focus == 0 &&
			(!m.transcriptReader.opened || m.transcriptReader.key != m.states[1].key)
		m.setDetailFocus(1)
		if firstRead {
			m.revealAction()
		}
	case "v":
		return m.privateRead(false)
	case "c":
		if m.panel == 2 || m.panel == 6 {
			return m.privateRead(true)
		}
	case "e":
		if m.panel == 3 {
			m.focus = 2
			m.renderDetail(false)
			m.revealAction()
		}
	case "x":
		if m.panel == 1 {
			m.focus = 2
			if m.actionCount() == 0 {
				m.renderDetail(false)
			}
			return m.activateAction()
		}
	case "[":
		return m.moveAction(-1)
	case "]":
		return m.moveAction(1)
	case "s":
		if m.panel == 5 {
			return m.switchEmail()
		}
	case "u", "a":
		if m.panel == 5 && !m.sent {
			if k == "u" {
				m.unread = !m.unread
			} else {
				m.unacked = !m.unacked
			}
			m.invalidate()
			m.states[5].rows = nil
			delete(m.states[5].data, dashboard.ResourceEmailReceived)
			m.states[5].selected = 0
			m.renderDetail(false)
			return m.refresh()
		}
	case "j", "down", "k", "up":
		d := 1
		if k == "k" || k == "up" {
			d = -1
		}
		if m.focus == 0 {
			return m.choose(d)
		}
		if m.focus == 2 {
			return m.moveAction(d)
		}
		m.vp.ScrollDown(max(d, 0))
		m.vp.ScrollUp(max(-d, 0))
	case "pgdown", "ctrl+d":
		m.vp.HalfPageDown()
	case "pgup", "ctrl+u":
		m.vp.HalfPageUp()
	case "home", "g":
		m.vp.GotoTop()
	case "end", "G":
		m.vp.GotoBottom()
	}
	return nil
}
func (m *Model) switchEmail() tea.Cmd {
	m.invalidate()
	m.sent = !m.sent
	m.states[5].selected = 0
	m.states[5].key = ""
	m.reconcileRows()
	m.vp.GotoTop()
	m.renderDetail(false)
	return m.refresh()
}
func (m *Model) selectID(id string) {
	s := &m.states[m.panel]
	s.filter = ""
	s.key = id
	s.pin = id
	for i, r := range s.rows {
		if r.key == id {
			s.selected = i
			return
		}
	}
	s.rows = append(s.rows, makeItem(id, id, "Loading detail", object{"id": id}))
	s.selected = len(s.rows) - 1
}
func (m *Model) openSalient() tea.Cmd {
	r := m.current()
	if r == nil || r.key == "" {
		return nil
	}
	id := r.key
	m.movePanel(3)
	m.selectID(id)
	m.resetDetail()
	return m.refresh()
}
func (m *Model) actionCount() int {
	r := m.current()
	if r == nil {
		return 0
	}
	switch m.panel {
	case 1:
		return len(list(m.states[1].data[dashboard.ResourceTranscript], "entries"))
	case 3:
		return len(list(obj(m.states[3].data[dashboard.ResourceMemory]["memory"]), "evidence"))
	case 4:
		return len(r.messages)
	case 6:
		return len(list(obj(m.states[6].data[dashboard.ResourceSecret]["secret"]), "fields"))
	}
	return 0
}
func (m *Model) actionObjects() []object {
	if r := m.current(); r != nil {
		switch m.panel {
		case 1:
			return list(m.states[1].data[dashboard.ResourceTranscript], "entries")
		case 3:
			return list(obj(m.states[3].data[dashboard.ResourceMemory]["memory"]), "evidence")
		case 4:
			return r.messages
		case 6:
			return list(obj(m.states[6].data[dashboard.ResourceSecret]["secret"]), "fields")
		}
	}
	return nil
}
func actionKey(o object) string {
	if dir := str(o, "_dir"); dir != "" {
		return dir + ":" + str(o, "id")
	}
	if id := first(str(o, "id"), str(o, "sequence")); id != "" {
		return id
	}
	if id := str(o, "transcript_id"); id != "" {
		return id + ":" + str(o, "entry_from_sequence")
	}
	return first(str(o, "external_locator"), str(o, "source_memory_id"), str(o, "message_id"))
}
func (m *Model) reconcileSub() {
	old := m.subKey
	a := m.actionObjects()
	if m.subKey != "" {
		for i, o := range a {
			if actionKey(o) == m.subKey {
				m.sub = i
				break
			}
		}
	}
	m.sub = max(0, min(m.sub, len(a)-1))
	if len(a) > 0 {
		m.subKey = actionKey(a[m.sub])
	} else {
		m.subKey = ""
	}
	if old != "" && m.subKey != old {
		m.clearPrivate()
	}
}
func (m *Model) moveAction(d int) tea.Cmd {
	a := m.actionObjects()
	if len(a) == 0 {
		return nil
	}
	next := max(0, min(m.sub+d, len(a)-1))
	if next != m.sub {
		m.clearPrivate()
	}
	m.sub = next
	m.subKey = actionKey(a[m.sub])
	m.focus = 2
	m.renderDetail(false)
	m.revealAction()
	return nil
}
func (m *Model) activateAction() tea.Cmd {
	a := m.actionObjects()
	if m.sub >= len(a) {
		return nil
	}
	o := a[m.sub]
	switch m.panel {
	case 1:
		seq := num(o, "sequence")
		m.expanded[seq] = !m.expanded[seq]
		m.renderDetail(false)
		m.revealAction()
	case 3:
		id := str(o, "transcript_id")
		if id == "" {
			m.notice = "This evidence locator is plain text; external links never open automatically."
			return nil
		}
		memoryID := m.states[3].key
		from := max(0, num(o, "entry_from_sequence"))
		until := max(from, num(o, "entry_until_sequence"))
		m.movePanel(1)
		m.selectID(id)
		m.resetDetail()
		m.evidenceFrom = from
		m.evidenceUntil = until
		m.returnMemory = memoryID
		m.focus = 1
		m.renderDetail(false)
		return m.refresh()
	case 4, 6:
		return m.privateRead(false)
	}
	return nil
}

// Snapshot produces a deterministic, synchronous view ONLY for the package's
// closed synthetic demo source. It never writes a file or calls a real adapter.
func (m *Model) Snapshot(width, height int) string {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	if _, ok := m.source.(*demoSource); !ok {
		return "Snapshots require NewDemoSource."
	}
	m.width = max(1, width)
	m.height = max(1, height)
	m.resize()
	cmd := m.refresh()
	for i := 0; cmd != nil && i < 8; i++ {
		msg, ok := cmd().(fetchedMsg)
		if !ok {
			break
		}
		cmd = m.applyFetch(msg)
	}
	m.renderDetail(false)
	return m.view()
}
