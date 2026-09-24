package agenttui

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/witwave-ai/witself/internal/dashboard"
)

// Account has no panel index. Authority, section reads and selected private
// text have independent cancellation/generation fences from the seven panels.
type accountModel struct {
	context                         accountContext
	mode                            bool
	section, selected, focus        int
	data                            object
	status                          string
	generation, authorityGeneration uint64
	busy, authorityBusy             bool
	cancel, authorityCancel         context.CancelFunc
	vp                              viewport.Model

	// A started authority request is not proof of successful verification.
	verifiedContextGeneration uint64

	ticket accountTicketState
	// Zero uses the production 10s deadline; tests may expire it deterministically.
	ticketTimeout time.Duration
}
type accountTicketState struct {
	sync.Mutex
	generation uint64
	cancel     context.CancelFunc
	id, status string
	data       object
}
type accountTickMsg struct{}
type accountContextMsg struct {
	generation uint64
	context    accountContext
}
type accountDataMsg struct {
	generation, authorityGeneration uint64
	access                          bool
	context                         accountContext
	data                            object
	status                          string
}
type accountTicketMsg struct {
	generation uint64
	forbidden  bool
}

func (m *Model) accountTick() tea.Cmd {
	return tea.Tick(m.opts.PollInterval, func(time.Time) tea.Msg { return accountTickMsg{} })
}

// Context and Access are both fresh authority reads. Order their requests with
// one fence; a later context check remains independent of a slow Access read.
func (m *Model) accountAuthorityFence() uint64 {
	a := &m.account
	a.authorityGeneration++
	if a.authorityCancel != nil {
		a.authorityCancel()
		a.authorityCancel = nil
	}
	a.authorityBusy = false
	return a.authorityGeneration
}
func (m *Model) accountAuthority() tea.Cmd {
	a := &m.account
	if a.authorityBusy || m.closed.Load() {
		return nil
	}
	gen := m.accountAuthorityFence()
	a.authorityBusy = true
	ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
	a.authorityCancel = cancel
	src := m.source
	return func() tea.Msg {
		defer cancel()
		result := accountContextMsg{generation: gen}
		if src == nil {
			return result
		}
		raw, err := src.Read(ctx, dashboard.ReadRequest{Resource: dashboard.ResourceAccountContext})
		defer clear(raw)
		if err == nil && ctx.Err() == nil {
			result.context, _ = decodeAccountContext(raw)
		}
		return result
	}
}
func (m *Model) clearAccountTicket() {
	t := &m.account.ticket
	t.Lock()
	defer t.Unlock()
	t.generation++
	if t.cancel != nil {
		t.cancel()
		t.cancel = nil
	}
	t.data = nil
	t.id = ""
	t.status = ""
	// The rendered viewport is itself private state and must clear synchronously.
	m.account.vp.SetContent("")
	m.account.vp.GotoTop()
}
func (m *Model) clearAccountData() {
	a := &m.account
	a.generation++
	if a.cancel != nil {
		a.cancel()
		a.cancel = nil
	}
	a.busy = false
	a.data = nil
	a.status = ""
	a.selected = 0
	m.clearAccountTicket()
}
func (m *Model) dropAccount() {
	m.accountAuthorityFence()
	m.clearAccountData()
	m.account.context = accountContext{}
	m.account.mode = false
	m.account.section = 0
	m.resize()
	m.renderDetail(false)
}
func (m *Model) accountContextResult(msg accountContextMsg) tea.Cmd {
	a := &m.account
	if msg.generation != a.authorityGeneration {
		return nil
	}
	a.authorityBusy = false
	a.authorityCancel = nil
	if !msg.context.available() || (a.context.available() && a.context.account != msg.context.account) {
		m.dropAccount()
		return nil
	}
	a.verifiedContextGeneration = msg.generation
	if !a.context.same(msg.context) {
		section := m.accountSection()
		m.clearAccountData()
		a.context = msg.context
		a.section = max(0, slices.Index(a.context.sections, section))
		a.focus = 0
		m.resize()
		if a.mode {
			m.renderAccount()
			return m.accountRead(false)
		}
	}
	return nil
}
func (m *Model) accountSection() string {
	a := &m.account
	if !a.context.available() || a.section < 0 || a.section >= len(a.context.sections) {
		return ""
	}
	return a.context.sections[a.section]
}
func accountReadStatus(err error) string {
	if err == nil {
		return "ready"
	}
	var re *dashboard.ReaderError
	if errors.As(err, &re) {
		switch re.Code {
		case "response_too_large":
			return "response_too_large"
		case "forbidden", "unauthorized":
			return "forbidden"
		}
	}
	return "unavailable"
}
func (m *Model) accountRead(scan bool) tea.Cmd {
	a := &m.account
	section := m.accountSection()
	if !a.mode || section == "" || a.busy || m.closed.Load() {
		return nil
	}
	r := accountResources[section]
	if scan {
		if section != "clients" {
			return nil
		}
		r = dashboard.ResourceAccountClientsScan
	}
	a.generation++
	gen := a.generation
	c := a.context
	src := m.source
	access := r == dashboard.ResourceAccountAccess
	var authorityGeneration uint64
	if access {
		authorityGeneration = m.accountAuthorityFence()
	}
	ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
	a.cancel = cancel
	a.busy = true
	if a.data == nil {
		a.status = "loading"
	}
	m.renderAccount()
	return func() tea.Msg {
		defer cancel()
		result := accountDataMsg{generation: gen, authorityGeneration: authorityGeneration, access: access, status: "unavailable"}
		if src == nil {
			return result
		}
		raw, err := src.Read(ctx, dashboard.ReadRequest{Resource: r})
		defer clear(raw)
		result.status = accountReadStatus(err)
		if err == nil && ctx.Err() == nil {
			if access {
				result.context, err = decodeAccountContext(raw)
				result.data = object{}
			} else {
				result.data, err = decodeAccount(r, raw, c, "")
			}
			if err != nil {
				result.status = "unavailable"
			}
		} else if ctx.Err() != nil {
			result.status = "unavailable"
		}
		return result
	}
}
func (m *Model) accountDataResult(msg accountDataMsg) tea.Cmd {
	a := &m.account
	if msg.generation != a.generation || !a.mode || !a.context.available() {
		return nil
	}
	a.busy = false
	a.cancel = nil
	if msg.access {
		if msg.authorityGeneration != a.authorityGeneration {
			// Access displays only the closed identity/permissions in Context.
			// Discard the obsolete result, including errors, and use that current
			// projection only after its verification succeeded. A later check
			// still in flight must settle as retryable, never leave loading idle.
			a.data = nil
			a.status = "unavailable"
			if a.verifiedContextGeneration == a.authorityGeneration {
				a.data = object{}
				a.status = "ready"
			}
			m.renderAccount()
			return nil
		}
		if msg.status != "ready" || !msg.context.available() || msg.context.account != a.context.account {
			m.dropAccount()
			return nil
		}
		if !a.context.same(msg.context) {
			return m.accountContextResult(accountContextMsg{generation: msg.authorityGeneration, context: msg.context})
		}
	}
	if msg.status == "forbidden" {
		m.dropAccount()
		return nil
	}
	// Preserve a metadata selection by ID across passive list reorderings.
	selectedID := ""
	if rows := list(a.data, "tickets"); a.selected < len(rows) {
		selectedID = str(rows[a.selected], "id")
	}
	a.data = msg.data
	if selectedID != "" {
		for i, row := range list(a.data, "tickets") {
			if str(row, "id") == selectedID {
				a.selected = i
				break
			}
		}
	}
	a.status = msg.status
	a.selected = min(a.selected, max(0, len(list(a.data, "tickets"))-1))
	m.renderAccount()
	return nil
}
func (m *Model) accountOpenTicket() tea.Cmd {
	a := &m.account
	if m.accountSection() != "support" || a.status != "ready" {
		return nil
	}
	rows := list(a.data, "tickets")
	if a.selected >= len(rows) {
		return nil
	}
	id := str(rows[a.selected], "id")
	// Freeze the metadata selection while this deliberate thread is open.
	// A poll launched before Enter must not reorder/replace the selected ticket.
	a.generation++
	if a.cancel != nil {
		a.cancel()
		a.cancel = nil
	}
	a.busy = false
	m.clearAccountTicket()
	timeout := a.ticketTimeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(m.ctx, timeout)
	c := a.context
	src := m.source
	t := &a.ticket
	t.Lock()
	gen := t.generation
	t.cancel = cancel
	t.id = id
	t.status = "loading"
	t.Unlock()
	m.renderAccount()
	return func() tea.Msg {
		defer cancel()
		raw, err := src.Read(ctx, dashboard.ReadRequest{Resource: dashboard.ResourceAccountSupportTicket, ID: id})
		defer clear(raw)
		status := accountReadStatus(err)
		var data object
		if err == nil && ctx.Err() == nil {
			data, err = decodeAccount(dashboard.ResourceAccountSupportTicket, raw, c, id)
			if err != nil {
				status = "unavailable"
			}
		}
		t.Lock()
		defer t.Unlock()
		if gen == t.generation && !m.closed.Load() {
			if ctx.Err() != nil {
				data = nil
				status = "unavailable"
			}
			t.data = data
			t.status = status
		}
		return accountTicketMsg{generation: gen, forbidden: status == "forbidden"}
	}
}
func (m *Model) leaveAccount() {
	m.clearAccountData()
	m.account.mode = false
	m.notice = ""
	m.resize()
	m.renderDetail(false)
}
func (m *Model) openAccount() tea.Cmd {
	if !m.account.context.available() {
		return nil
	}
	m.clearPrivate()
	m.vp.SetContent("")
	m.filterEditing = false
	m.notice = ""
	m.account.mode = true
	m.account.focus = 0
	m.resize()
	m.clearAccountData()
	return m.accountRead(false)
}

// Entire Account key path is closed; v/c and agent detail actions never fall through.
func (m *Model) accountKey(k string) tea.Cmd {
	a := &m.account
	switch k {
	case "q":
		m.closeLocked()
		return tea.Quit
	case "1", "2", "3", "4", "5", "6", "7":
		m.leaveAccount()
		return m.navigate(int(k[0] - '1'))
	case "8":
		return nil
	case "?":
		m.clearAccountTicket()
		m.renderAccount()
		m.overlay = "help"
		m.helpScroll = 0
	case "t":
		m.clearAccountTicket()
		m.renderAccount()
		m.overlay = "theme"
		for i, n := range themeNames {
			if n == m.theme {
				m.overlayIndex = i
			}
		}
	case "b", "w":
		m.clearAccountTicket()
		m.renderAccount()
		if k == "w" {
			m.overlay = "console"
			return m.consoleRun(consoleCheck)
		}
		return m.consoleRun(consoleOpen)
	case "p":
		m.paused = !m.paused
	case "r":
		m.clearAccountData()
		return tea.Batch(m.accountAuthority(), m.accountRead(false))
	case "x":
		if m.accountSection() == "clients" && !a.busy {
			m.clearAccountData()
			return m.accountRead(true)
		}
	case "[", "left", "]", "right":
		delta := 1
		if k == "[" || k == "left" {
			delta = -1
		}
		next := max(0, min(a.section+delta, len(a.context.sections)-1))
		if next != a.section {
			m.clearAccountData()
			a.section = next
			a.focus = 0
			return m.accountRead(false)
		}
	case "tab", "shift+tab":
		a.focus = (a.focus + 1) % 2
	case "enter":
		a.focus = 1
		if m.accountSection() == "support" {
			return m.accountOpenTicket()
		}
	case "esc":
		m.clearAccountTicket()
		a.focus = 0
		m.renderAccount()
	case "up", "k", "down", "j":
		delta := 1
		if k == "up" || k == "k" {
			delta = -1
		}
		if a.focus == 0 && m.accountSection() == "support" {
			next := max(0, min(a.selected+delta, len(list(a.data, "tickets"))-1))
			if next != a.selected {
				m.clearAccountTicket()
				a.selected = next
				m.renderAccount()
			}
		} else {
			a.vp.ScrollDown(max(delta, 0))
			a.vp.ScrollUp(max(-delta, 0))
		}
	case "pgdown", "ctrl+d":
		a.vp.HalfPageDown()
	case "pgup", "ctrl+u":
		a.vp.HalfPageUp()
	case "home", "g":
		a.vp.GotoTop()
	case "end", "G":
		a.vp.GotoBottom()
	}
	return nil
}
