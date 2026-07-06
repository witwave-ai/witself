package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	reflowtrunc "github.com/muesli/reflow/truncate"

	"github.com/witwave-ai/witself/internal/client"
)

// UI modes. list is the master fullscreen view (the support pane);
// detail is the drill-down thread; compose overlays a reply editor on
// top of detail.
type uiMode int

const (
	modeList uiMode = iota
	modeDetail
	modeCompose
	modeEventDetail  // drill-down on one audit event
	modeCellDetail   // drill-down on one fleet cell
	modeTicketDetail // drill-down on one ticket's full record (i)
	modeHealth       // fleet health charts (H)
)

// Dashboard panes, in tab-cycle order. The focused pane carries the
// accent border and receives j/k.
type paneID int

const (
	paneCells paneID = iota
	paneSupport
	paneEvents
	paneCount // sentinel for modular cycling
)

// autoRefreshInterval drives the background reload of everything the
// watch streams don't cover (cell account counts, fan-out health) and
// re-seeds any pane whose live stream has died. Manual g still works
// and is instant; this just means nobody HAS to press it.
const autoRefreshInterval = 60 * time.Second

// Styles — one palette, defined once. Kept minimal so the TUI renders
// sanely on both dark and light terminals.
var (
	styTitle    = lipgloss.NewStyle().Bold(true)
	styDim      = lipgloss.NewStyle().Faint(true)
	styErr      = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	styOK       = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	styWarn     = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	styInfo     = lipgloss.NewStyle().Foreground(lipgloss.Color("4"))
	stySelected = lipgloss.NewStyle().Reverse(true)
)

// stateBadge renders a fixed-width, color-coded ticket state.
func stateBadge(state string) string {
	switch state {
	case "awaiting_admin":
		return styErr.Render("ADMIN") // the ball is in OUR court — loud
	case "awaiting_customer":
		return styInfo.Render("CUST ")
	case "resolved":
		return styOK.Render("RESLV")
	case "closed":
		return styDim.Render("CLOSD")
	default:
		return styWarn.Render("OPEN ")
	}
}

// plainStateBadge is stateBadge without color codes, for the selected
// row's reverse-video render.
func plainStateBadge(state string) string {
	switch state {
	case "awaiting_admin":
		return "ADMIN"
	case "awaiting_customer":
		return "CUST "
	case "resolved":
		return "RESLV"
	case "closed":
		return "CLOSD"
	default:
		return "OPEN "
	}
}

// Messages flowing into Update.
type (
	ticketsLoadedMsg struct {
		list client.AdminTicketList
		err  error
	}
	threadLoadedMsg struct {
		res client.GetSupportTicketResult
		err error
	}
	actionDoneMsg struct {
		label string
		err   error
	}
	watchTicketMsg  struct{ ticket client.AdminTicket }
	watchStoppedMsg struct{}

	// Fleet panes: cells strip + events tail.
	cellsLoadedMsg struct {
		cells []client.AdminCell
		err   error
	}
	eventsSeedMsg struct {
		events []client.AdminEvent
		err    error
	}
	watchEventMsg         struct{ event client.AdminEvent }
	eventsWatchStoppedMsg struct{}
	autoRefreshMsg        struct{}

	// Self-upgrade lifecycle: periodic check → newer tag found →
	// background install → re-exec with resume state. noop means the
	// channel reported success without actually delivering the target
	// version (brew tap lagging the GitHub tag) — retried at the next
	// periodic check, never via restart.
	upgradeCheckMsg     struct{}
	upgradeAvailableMsg struct{ tag string }
	upgradeAppliedMsg   struct {
		tag  string
		noop bool
		err  error
	}
)

// model is the whole TUI state. Update is a pure transition function
// over it (bubbletea's Elm shape), which is what makes the keyboard
// behavior unit-testable without a terminal.
type model struct {
	cli        *adminCLI
	ctx        context.Context
	watch      <-chan client.AdminTicket
	eventWatch <-chan client.AdminEvent

	mode    uiMode
	width   int
	height  int
	tickets []client.AdminTicket
	cells   []client.AdminCellStatus
	cursor  int

	// stateFilter narrows the support pane: "" (everything) →
	// "active" (open/awaiting_*) → "resolved" → "closed". Cycled
	// with f; the cursor and all actions operate on the FILTERED
	// view.
	stateFilter string

	// Fleet panes (list mode is the master view: cells strip on top,
	// support in the middle, events tail on the bottom).
	fleetCells []client.AdminCell
	events     []client.AdminEvent // newest LAST — renders like tail -f

	// Pane focus + per-pane selection. eventScroll doubles as the
	// events SELECTION, counted UP from the live tail (0 = newest,
	// pinned live; >0 = an older event is selected and the view is
	// paused, holding steady as new events arrive). cellCursor selects
	// a cell. The focused pane renders its selection highlighted;
	// enter drills into whatever is selected.
	focus       paneID
	eventScroll int
	cellCursor  int

	// Drill-down targets (copies — the live tail keeps moving under
	// them, the detail view must not). ticketInfoReturn remembers
	// whether the ticket-record inspector was opened from the list or
	// from inside the thread, so esc goes back to the right place.
	detailEvent      *client.AdminEvent
	detailCell       *client.AdminCell
	detailTicket     *client.AdminTicket
	ticketInfoReturn uiMode

	// Detail state.
	thread        *client.GetSupportTicketResult
	threadAccount string
	threadTicket  string
	threadView    viewport.Model

	composer textarea.Model

	status  string // transient one-line message (footer)
	loading bool
	now     func() time.Time // injectable clock for age rendering in tests

	// Session-local health history feeding the fleet strip and the H
	// drill-down: one sample per completed refresh cycle. eventsSeen
	// counts live watch arrivals cumulatively; per-sample deltas give
	// the event rate.
	samples    []healthSample
	eventsSeen int

	// Self-upgrade state. binPath/installVia are resolved at startup;
	// upgradeReadyTag is set once a newer binary is INSTALLED on disk
	// (relaunch deferred while composing); relaunch non-nil tells
	// main() to re-exec after bubbletea releases the terminal.
	// upgradePhase drives the persistent footer light:
	// "" (no update) → "installing" → "ready" | "channel-wait".
	binPath         string
	installVia      string
	currentVersion  string
	upgradeReadyTag string
	upgradePhase    string
	upgradeTag      string
	relaunch        *resumeState

	// resume carries the pre-upgrade view to restore; consumed once.
	resume *resumeState
}

func newModel(ctx context.Context, cli *adminCLI, watch <-chan client.AdminTicket) model {
	ta := textarea.New()
	ta.Placeholder = "Write your reply… (ctrl+d to send, esc to cancel)"
	ta.CharLimit = 64 * 1024
	return model{
		cli:      cli,
		ctx:      ctx,
		watch:    watch,
		mode:     modeList,
		focus:    paneSupport,
		composer: ta,
		now:      time.Now,
		loading:  true,
		status:   "loading fleet tickets…",
	}
}

// withEventWatch attaches the live fleet-event stream feeding the
// events tail pane.
func (m model) withEventWatch(ch <-chan client.AdminEvent) model {
	m.eventWatch = ch
	return m
}

// withSelfUpgrade arms the periodic release check. binPath is the
// running executable; ver the ldflags-injected version.
func (m model) withSelfUpgrade(binPath, ver string) model {
	m.binPath = binPath
	m.installVia = installMethod(binPath)
	m.currentVersion = ver
	return m
}

// withResume seeds the view snapshot a self-upgrade re-exec carried
// over. Thread coordinates land on the model HERE (not in Init) —
// Init's receiver is a value, so mutations there are discarded by
// bubbletea; only its returned commands survive.
func (m model) withResume(r *resumeState) model {
	m.resume = r
	if r != nil && r.ThreadAccount != "" {
		m.threadAccount, m.threadTicket = r.ThreadAccount, r.ThreadTicket
	}
	// The health view has no load dependency — restore it directly and
	// CONSUME the resume. Leaving it armed would let a much-later
	// thread load replay the stale cursor and a bogus "upgraded"
	// status (the one-shot consumer lives on the thread-load path,
	// which a health restore never triggers).
	if r != nil && r.Mode == "health" {
		m.mode = modeHealth
		if r.UpgradedTo != "" {
			m.status = "upgraded to " + r.UpgradedTo + " ✓"
		}
		m.resume = nil
	}
	return m
}

func (m model) Init() tea.Cmd {
	cmds := []tea.Cmd{
		m.loadTickets(), m.awaitWatch(),
		m.loadCells(), m.seedEvents(), m.awaitEventWatch(),
		tea.Tick(autoRefreshInterval, func(time.Time) tea.Msg { return autoRefreshMsg{} }),
	}
	if m.upgradeEligible() {
		// First check shortly after startup (don't block first paint),
		// then every upgradeCheckInterval.
		cmds = append(cmds, tea.Tick(10*time.Second, func(time.Time) tea.Msg {
			return upgradeCheckMsg{}
		}))
	}
	// Restore the pre-upgrade view: the ticket list is loading; the
	// thread reload re-enters detail (and compose, if a draft rode
	// along) when it lands. Coordinates were seeded by withResume.
	if r := m.resume; r != nil && r.ThreadAccount != "" {
		cmds = append(cmds, m.loadThread(r.ThreadAccount, r.ThreadTicket))
	}
	return tea.Batch(cmds...)
}

// upgradeEligible: only release builds self-upgrade — a source build
// ("dev") must never clobber itself with a release binary.
func (m model) upgradeEligible() bool {
	return m.currentVersion != "" && m.currentVersion != "dev" && m.binPath != ""
}

// checkUpgrade polls GitHub for a newer release.
func (m model) checkUpgrade() tea.Cmd {
	ctx, cur := m.ctx, m.currentVersion
	return func() tea.Msg {
		tag, err := latestReleaseTag(ctx)
		if err != nil || !newerVersion(cur, tag) {
			return nil // quiet on failure — next tick tries again
		}
		return upgradeAvailableMsg{tag: tag}
	}
}

// applyUpgrade installs the newer release through the same channel
// that installed this binary. Runs in the background; the UI stays
// interactive throughout.
//
// Deliberately NOT the UI context: quitting the dashboard must not
// SIGKILL a brew process mid-install (CommandContext's default cancel
// is Kill, fired by main's deferred cancel) — a bounded background
// context lets an in-flight upgrade finish on its own terms.
func (m model) applyUpgrade(tag string) tea.Cmd {
	method, bin := m.installVia, m.binPath
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		if err := doUpgrade(ctx, method, tag, bin); err != nil {
			return upgradeAppliedMsg{tag: tag, err: err}
		}
		// Channel success ≠ target installed: a no-op brew upgrade
		// (tap formula lagging the tag) exits 0 with the old binary
		// still on disk. Re-exec would restart-loop until the tap
		// caught up — verify before declaring victory.
		if !verifyInstalledVersion(ctx, bin, tag) {
			return upgradeAppliedMsg{tag: tag, noop: true}
		}
		return upgradeAppliedMsg{tag: tag}
	}
}

// snapshotView captures exactly what the operator is looking at so the
// post-upgrade process can put them right back — including a half-typed
// reply draft.
func (m model) snapshotView(tag string) *resumeState {
	r := &resumeState{
		Cursor:     m.cursor,
		UpgradedTo: tag,
	}
	switch m.mode {
	case modeList:
		r.Mode = "list"
	case modeDetail:
		r.Mode = "detail"
	case modeCompose:
		r.Mode = "compose"
		r.Draft = m.composer.Value()
	case modeHealth:
		r.Mode = "health"
	}
	// Thread coordinates ride along only when a thread is actually on
	// screen — esc-from-detail leaves them set, and carrying the stale
	// pair through a health-view upgrade would reload that old thread
	// on restore and clobber the health view with modeDetail.
	if r.Mode != "health" {
		r.ThreadAccount, r.ThreadTicket = m.threadAccount, m.threadTicket
	}
	return r
}

// loadTickets refreshes the fleet snapshot.
func (m model) loadTickets() tea.Cmd {
	cli, ctx := m.cli, m.ctx
	return func() tea.Msg {
		list, err := cli.listTickets(ctx)
		return ticketsLoadedMsg{list: list, err: err}
	}
}

// loadThread fetches one ticket's thread for the detail pane.
func (m model) loadThread(accountID, ticketID string) tea.Cmd {
	cli, ctx := m.cli, m.ctx
	return func() tea.Msg {
		res, err := cli.showTicket(ctx, accountID, ticketID)
		return threadLoadedMsg{res: res, err: err}
	}
}

// loadCells refreshes the fleet-cells strip.
func (m model) loadCells() tea.Cmd {
	cli, ctx := m.cli, m.ctx
	return func() tea.Msg {
		cells, err := cli.cells(ctx)
		return cellsLoadedMsg{cells: cells, err: err}
	}
}

// seedEvents primes the events tail with the most recent fleet
// activity; live updates then append via the event watch stream.
func (m model) seedEvents() tea.Cmd {
	cli, ctx := m.cli, m.ctx
	return func() tea.Msg {
		events, err := cli.eventsSeed(ctx, 30)
		return eventsSeedMsg{events: events, err: err}
	}
}

// awaitEventWatch pumps the fleet-event stream — same idiom as
// awaitWatch below.
func (m model) awaitEventWatch() tea.Cmd {
	ch := m.eventWatch
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		e, ok := <-ch
		if !ok {
			return eventsWatchStoppedMsg{}
		}
		return watchEventMsg{event: e}
	}
}

// awaitWatch blocks on the next live update from the watch stream.
// Re-issued after every received message — the bubbletea idiom for
// pumping a channel.
func (m model) awaitWatch() tea.Cmd {
	ch := m.watch
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		t, ok := <-ch
		if !ok {
			return watchStoppedMsg{}
		}
		return watchTicketMsg{ticket: t}
	}
}

// eventTailCap bounds the in-memory events tail. Old entries roll off
// the top exactly like a terminal scrollback.
const eventTailCap = 200

// appendEvent adds a live event to the tail (newest last), dropping
// duplicates by id (the watch stream can re-emit around its
// high-water mark) and rolling the oldest off past the cap. The added
// flag says whether e was genuinely new — callers must NOT infer that
// from length growth, which stops at the cap and would freeze both the
// event-rate counters and the paused-scroll anchor exactly when the
// fleet is busiest.
func appendEvent(events []client.AdminEvent, e client.AdminEvent) ([]client.AdminEvent, bool) {
	for i := range events {
		if events[i].ID == e.ID {
			return events, false
		}
	}
	events = append(events, e)
	if len(events) > eventTailCap {
		events = events[len(events)-eventTailCap:]
	}
	return events, true
}

// stateRank orders the support pane by lifecycle stage: what needs the
// admin's attention floats to the top, finished work sinks to the
// bottom. Within a stage, newest activity first.
func stateRank(state string) int {
	switch state {
	case "awaiting_admin":
		return 0 // the ball is in our court — top
	case "open":
		return 1
	case "awaiting_customer":
		return 2
	case "resolved":
		return 3
	case "closed":
		return 4 // terminal — bottom
	default:
		return 5
	}
}

// sortTickets applies the pane order: stage first, then newest
// activity within the stage.
func sortTickets(tickets []client.AdminTicket) {
	sort.SliceStable(tickets, func(i, j int) bool {
		ri, rj := stateRank(tickets[i].State), stateRank(tickets[j].State)
		if ri != rj {
			return ri < rj
		}
		return tickets[i].LastActivityAt.After(tickets[j].LastActivityAt)
	})
}

// upsertTicket merges a live watch update into the list, keeping the
// stage-grouped, newest-activity order the list view promises.
func upsertTicket(tickets []client.AdminTicket, t client.AdminTicket) []client.AdminTicket {
	replaced := false
	for i := range tickets {
		if tickets[i].ID == t.ID {
			tickets[i] = t
			replaced = true
			break
		}
	}
	if !replaced {
		tickets = append(tickets, t)
	}
	sortTickets(tickets)
	return tickets
}

// visibleTickets is the support pane's working set: the full list
// narrowed by the stage filter, order preserved (stage-grouped,
// newest first). The cursor, selection, and all actions index THIS
// slice — filtering must never let a keystroke land on a hidden row.
func (m model) visibleTickets() []client.AdminTicket {
	if m.stateFilter == "" {
		return m.tickets
	}
	var out []client.AdminTicket
	for _, t := range m.tickets {
		switch m.stateFilter {
		case "active":
			if t.State != "resolved" && t.State != "closed" {
				out = append(out, t)
			}
		case "resolved", "closed":
			if t.State == m.stateFilter {
				out = append(out, t)
			}
		}
	}
	return out
}

func (m model) selected() *client.AdminTicket {
	vis := m.visibleTickets()
	if m.cursor < 0 || m.cursor >= len(vis) {
		return nil
	}
	return &vis[m.cursor]
}

// selectedEvent resolves the events-pane selection: eventScroll counts
// up from the live tail, so 0 = newest. nil when the tail is empty.
func (m model) selectedEvent() *client.AdminEvent {
	if len(m.events) == 0 {
		return nil
	}
	idx := len(m.events) - 1 - minInt(m.eventScroll, len(m.events)-1)
	return &m.events[idx]
}

// supportRowHighlighted mirrors the cells/events rule: a pane shows
// its selection ONLY while it holds focus. The cursor itself persists
// so tabbing back lands where you left off — it just goes visually
// quiet when another window is active.
func (m model) supportRowHighlighted(i int) bool {
	return i == m.cursor && m.focus == paneSupport
}

// selectedID names the ticket currently under the cursor ("" when the
// list is empty) so a re-sort can restore the SELECTION, not the index.
func (m model) selectedID() string {
	if t := m.selected(); t != nil {
		return t.ID
	}
	return ""
}

// anchorCursor re-points the cursor at the ticket id it was on before a
// re-sort/refresh/filter change; falls back to clamping the raw index
// when the ticket vanished from the (filtered) view.
func (m *model) anchorCursor(id string) {
	vis := m.visibleTickets()
	if id != "" {
		for i := range vis {
			if vis[i].ID == id {
				m.cursor = i
				return
			}
		}
	}
	if m.cursor >= len(vis) {
		m.cursor = maxInt(len(vis)-1, 0)
	}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.syncThreadFrame()
		return m, nil

	case ticketsLoadedMsg:
		m.loading = false
		if msg.err != nil {
			m.status = "load failed: " + msg.err.Error()
			return m, nil
		}
		anchor := m.selectedID()
		m.tickets = msg.list.Tickets
		sortTickets(m.tickets)
		m.cells = msg.list.Cells
		m.anchorCursor(anchor)
		m.status = fmt.Sprintf("%d tickets · %s", len(m.tickets), cellSummary(m.cells))
		return m, nil

	case threadLoadedMsg:
		m.loading = false
		if msg.err != nil {
			// Stay where the user is: a failed BACKGROUND refresh
			// (watch-triggered or post-action) must not eject them
			// from detail or the composer. The user-initiated
			// enter-from-list path is still in modeList here anyway.
			m.status = "show failed: " + msg.err.Error()
			return m, nil
		}
		m.thread = &msg.res
		m.threadView.SetContent(renderThread(msg.res, m.threadView.Width))
		m.threadView.GotoBottom()
		// A live-watch refresh must not yank the operator out of the
		// composer (or discard a draft) — only promote to detail from
		// the list.
		if m.mode != modeCompose {
			m.mode = modeDetail
		}
		m.status = ""
		// One-shot resume restore: re-enter compose with the draft
		// that rode through the upgrade.
		if r := m.resume; r != nil {
			m.cursor = minInt(r.Cursor, maxInt(len(m.tickets)-1, 0))
			if r.Mode == "compose" {
				m.mode = modeCompose
				m.composer.SetValue(r.Draft)
				m.composer.Focus()
			}
			if r.UpgradedTo != "" {
				m.status = "upgraded to " + r.UpgradedTo + " ✓"
			}
			m.resume = nil
		}
		// Re-sync AFTER the mode settled (compose reserves more rows in
		// the full-screen fallback) and keep the newest message in view.
		m.syncThreadFrame()
		m.threadView.GotoBottom()
		return m, nil

	case upgradeCheckMsg:
		return m, tea.Batch(
			m.checkUpgrade(),
			tea.Tick(upgradeCheckInterval, func(time.Time) tea.Msg {
				return upgradeCheckMsg{}
			}),
		)

	case upgradeAvailableMsg:
		m.upgradePhase, m.upgradeTag = "installing", msg.tag
		m.status = "installing update " + msg.tag + " in the background…"
		return m, m.applyUpgrade(msg.tag)

	case upgradeAppliedMsg:
		if msg.err != nil {
			m.upgradePhase, m.upgradeTag = "", ""
			m.status = "self-upgrade failed: " + msg.err.Error()
			return m, nil
		}
		if msg.noop {
			// The channel hasn't caught up to the tag yet (brew tap
			// publish lag). No restart — the next periodic check
			// retries through the normal path; the footer light stays
			// on so the operator knows an update is pending.
			m.upgradePhase = "channel-wait"
			m.status = "update " + msg.tag + " published but not yet available via " + m.installVia + " — will retry"
			return m, nil
		}
		if m.mode == modeCompose || m.loading {
			// Never restart under the operator's fingers, and never
			// while an action (reply send, state change) is in
			// flight — the re-exec's teardown would kill that
			// subprocess and lose the action. relaunchIfReady fires
			// on compose-exit and on actionDoneMsg.
			m.upgradeReadyTag = msg.tag
			m.upgradePhase = "ready"
			m.status = "update " + msg.tag + " installed — restarting when current work settles"
			return m, nil
		}
		m.relaunch = m.snapshotView(msg.tag)
		return m, tea.Quit

	case actionDoneMsg:
		m.loading = false
		if msg.err != nil {
			m.status = msg.label + " failed: " + msg.err.Error()
			return m, nil
		}
		m.status = msg.label + " ✓"
		// A deferred self-upgrade applies as soon as the in-flight
		// action has landed safely.
		if cmd := m.relaunchIfReady(); cmd != nil {
			return m, cmd
		}
		// Refresh whichever view we're in so the state change shows.
		cmds := []tea.Cmd{m.loadTickets()}
		if m.mode == modeDetail && m.threadAccount != "" {
			cmds = append(cmds, m.loadThread(m.threadAccount, m.threadTicket))
		}
		return m, tea.Batch(cmds...)

	case watchTicketMsg:
		// Re-anchor the cursor by ticket ID across the re-sort: a live
		// update must never slide a DIFFERENT ticket under the
		// operator's finger between seeing a row and pressing R/C/O.
		anchor := m.selectedID()
		m.tickets = upsertTicket(m.tickets, msg.ticket)
		m.anchorCursor(anchor)
		m.status = fmt.Sprintf("activity on %s (%s)", msg.ticket.ID, msg.ticket.State)
		// If the updated ticket is open in detail (or behind the
		// composer), refresh the thread. Only THOSE modes — the old
		// `mode != modeList` proxy also matched the health/event/cell
		// views, whose stale thread coordinates would then reload and
		// yank the operator into modeDetail without a keystroke.
		var cmds []tea.Cmd
		if (m.mode == modeDetail || m.mode == modeCompose) && msg.ticket.ID == m.threadTicket {
			cmds = append(cmds, m.loadThread(m.threadAccount, m.threadTicket))
		}
		cmds = append(cmds, m.awaitWatch())
		return m, tea.Batch(cmds...)

	case watchStoppedMsg:
		m.watch = nil
		m.status = "live watch stopped — press g to refresh manually"
		return m, nil

	case cellsLoadedMsg:
		if msg.err != nil {
			m.status = "cells load failed: " + msg.err.Error()
			return m, nil
		}
		topologyChanged := cellTopologyChanged(m.fleetCells, msg.cells)
		eventBackfillChanged := cellEventBackfillChanged(m.fleetCells, msg.cells)
		m.fleetCells = msg.cells
		// One health sample per refresh cycle. Cells and tickets load
		// in the same batch, so the ticket counts here are at worst one
		// message behind — fine for a trend line. But NOT before the
		// first ticket load lands (m.loading): sampling open=0 then
		// would fabricate a ticket surge in the strip sparkline.
		if !m.loading {
			open, _ := openTicketCounts(m.tickets)
			m.samples = appendSample(m.samples, healthSample{
				at:       m.now(),
				accounts: totalAccounts(msg.cells),
				open:     open,
				events:   m.eventsSeen,
			})
		}
		if topologyChanged || eventBackfillChanged {
			// The events watch process polls the current fleet, but the
			// in-memory tail is only append-only. Topology changes can
			// leave old rows from a removed cell, while restore/import can
			// backfill account_events with original timestamps older than
			// the watch stream's high-water mark. Reseed when either shape
			// changes so the events pane converges with cells and support
			// without requiring a full dashboard restart.
			return m, m.seedEvents()
		}
		return m, nil

	case eventsSeedMsg:
		if msg.err != nil {
			// Non-fatal: the pane just stays empty until the watch
			// stream (or a refresh) fills it.
			m.status = "events load failed: " + msg.err.Error()
			return m, nil
		}
		// Seed arrives newest-first; the tail renders oldest-first
		// (newest at the bottom, like tail -f), so reverse.
		evs := make([]client.AdminEvent, len(msg.events))
		for i, e := range msg.events {
			evs[len(msg.events)-1-i] = e
		}
		m.events = evs
		return m, nil

	case watchEventMsg:
		var added bool
		m.events, added = appendEvent(m.events, msg.event)
		if added {
			m.eventsSeen++
		}
		// A paused (scrolled-up) tail holds its view steady while new
		// events arrive below — classic tail -f pause semantics.
		if m.eventScroll > 0 && added {
			m.eventScroll = minInt(m.eventScroll+1, len(m.events)-1)
		}
		return m, m.awaitEventWatch()

	case eventsWatchStoppedMsg:
		m.eventWatch = nil
		m.status = "event stream stopped — auto-refresh covers the tail"
		return m, nil

	case autoRefreshMsg:
		// Background refresh of everything the watch streams don't
		// carry: cell account counts + fan-out health always; ticket
		// list re-sync; events re-seed ONLY when the live stream is
		// dead (a reseed would clobber the tail the stream maintains).
		// Deliberately does not touch m.loading — an auto tick must
		// never defer an installed upgrade or dim the UI.
		cmds := []tea.Cmd{
			m.loadCells(),
			m.loadTickets(),
			tea.Tick(autoRefreshInterval, func(time.Time) tea.Msg { return autoRefreshMsg{} }),
		}
		if m.eventWatch == nil {
			cmds = append(cmds, m.seedEvents())
		}
		return m, tea.Batch(cmds...)

	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	// Delegate remaining messages to the focused sub-component.
	var cmd tea.Cmd
	switch m.mode {
	case modeCompose:
		m.composer, cmd = m.composer.Update(msg)
	case modeDetail:
		m.threadView, cmd = m.threadView.Update(msg)
	}
	return m, cmd
}

func (m model) handleKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Compose mode captures nearly everything for the textarea.
	if m.mode == modeCompose {
		switch key.String() {
		case "esc":
			// Actually discard — the status message must not lie. A
			// stale draft surviving into the NEXT ticket's composer
			// is a cross-ticket information leak waiting for a
			// reflexive ctrl+d.
			m.composer.Reset()
			m.mode = modeDetail
			m.syncThreadFrame()
			m.status = "reply discarded"
			return m, m.relaunchIfReady()
		case "ctrl+d":
			body := strings.TrimSpace(m.composer.Value())
			if body == "" {
				m.status = "empty reply not sent"
				m.mode = modeDetail
				m.syncThreadFrame()
				return m, m.relaunchIfReady()
			}
			cli, ctx := m.cli, m.ctx
			acct, tkt := m.threadAccount, m.threadTicket
			m.composer.Reset()
			m.mode = modeDetail
			m.syncThreadFrame()
			m.loading = true
			m.status = "sending reply…"
			send := func() tea.Msg {
				return actionDoneMsg{label: "reply", err: cli.reply(ctx, acct, tkt, body)}
			}
			// A deferred upgrade restarts AFTER the reply lands — the
			// send must not be lost to the re-exec.
			return m, send
		case "ctrl+c":
			return m, tea.Quit
		}
		var cmd tea.Cmd
		m.composer, cmd = m.composer.Update(key)
		return m, cmd
	}

	switch key.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "g":
		m.loading = true
		m.status = "refreshing…"
		return m, tea.Batch(m.loadTickets(), m.loadCells(), m.seedEvents())
	case "tab", "shift+tab":
		if m.mode == modeList {
			step := 1
			if key.String() == "shift+tab" {
				step = int(paneCount) - 1
			}
			m.focus = paneID((int(m.focus) + step) % int(paneCount))
		}
	case "f":
		// Cycle the support stage filter: all → active → resolved →
		// closed → all. Selection follows the same ticket when it
		// survives the narrowing.
		if m.mode == modeList && m.focus == paneSupport {
			anchor := m.selectedID()
			switch m.stateFilter {
			case "":
				m.stateFilter = "active"
			case "active":
				m.stateFilter = "resolved"
			case "resolved":
				m.stateFilter = "closed"
			default:
				m.stateFilter = ""
			}
			m.anchorCursor(anchor)
		}
	case "j", "down":
		if m.mode == modeList {
			switch m.focus {
			case paneSupport:
				if m.cursor < len(m.visibleTickets())-1 {
					m.cursor++
				}
			case paneEvents:
				if m.eventScroll > 0 {
					m.eventScroll-- // toward live; 0 = newest selected
				}
			case paneCells:
				if m.cellCursor < len(m.fleetCells)-1 {
					m.cellCursor++
				}
			}
		}
	case "k", "up":
		if m.mode == modeList {
			switch m.focus {
			case paneSupport:
				if m.cursor > 0 {
					m.cursor--
				}
			case paneEvents:
				if m.eventScroll < len(m.events)-1 {
					m.eventScroll++
				}
			case paneCells:
				if m.cellCursor > 0 {
					m.cellCursor--
				}
			}
		}
	case "enter":
		if m.mode != modeList {
			break
		}
		switch m.focus {
		case paneSupport:
			if t := m.selected(); t != nil {
				m.threadAccount, m.threadTicket = t.AccountID, t.ID
				m.loading = true
				m.status = "loading thread…"
				return m, m.loadThread(t.AccountID, t.ID)
			}
		case paneEvents:
			if e := m.selectedEvent(); e != nil {
				cp := *e
				m.detailEvent = &cp
				m.mode = modeEventDetail
			}
		case paneCells:
			if m.cellCursor >= 0 && m.cellCursor < len(m.fleetCells) {
				cp := m.fleetCells[m.cellCursor]
				m.detailCell = &cp
				m.mode = modeCellDetail
			}
		}
	case "H":
		if m.mode == modeList {
			m.mode = modeHealth
		}
	case "esc":
		switch m.mode {
		case modeDetail:
			m.mode = modeList
			m.thread = nil
			m.status = ""
		case modeEventDetail:
			m.mode = modeList
			m.detailEvent = nil
		case modeCellDetail:
			m.mode = modeList
			m.detailCell = nil
		case modeTicketDetail:
			m.mode = m.ticketInfoReturn
			m.detailTicket = nil
		case modeHealth:
			m.mode = modeList
		}
	case "i":
		// Inspect the ticket RECORD (timestamps, category, SLA fields,
		// correlation, metadata) — the thread view shows the
		// conversation; this shows the object.
		switch {
		case m.mode == modeList && m.focus == paneSupport:
			if t := m.selected(); t != nil {
				cp := *t
				m.detailTicket = &cp
				m.ticketInfoReturn = modeList
				m.mode = modeTicketDetail
			}
		case m.mode == modeDetail && m.thread != nil:
			cp := client.AdminTicket{SupportTicket: m.thread.Ticket}
			// Cell isn't on the thread payload — recover it from the
			// list row when present.
			for _, lt := range m.tickets {
				if lt.ID == cp.ID {
					cp.Cell = lt.Cell
					break
				}
			}
			m.detailTicket = &cp
			m.ticketInfoReturn = modeDetail
			m.mode = modeTicketDetail
		}
	case "r":
		if m.mode == modeDetail {
			// Fresh composer per entry — defense in depth against a
			// stale draft leaking across tickets. (The upgrade-resume
			// path restores its draft via SetValue directly, not
			// through this handler.)
			m.composer.Reset()
			m.mode = modeCompose
			m.syncThreadFrame()
			m.composer.Focus()
			return m, textarea.Blink
		}
	case "R", "C", "O":
		// In the master view, state keys act on the SUPPORT selection —
		// refuse when another pane holds focus so a keystroke aimed at
		// the events tail can't mutate a ticket the operator isn't
		// looking at.
		if m.mode == modeList && m.focus != paneSupport {
			return m, nil
		}
		target := map[string]string{
			"R": "resolved", "C": "closed", "O": "awaiting_admin",
		}[key.String()]
		acct, tkt := m.actionTarget()
		if acct == "" {
			return m, nil
		}
		cli, ctx := m.cli, m.ctx
		m.loading = true
		m.status = "setting state → " + target + "…"
		return m, func() tea.Msg {
			return actionDoneMsg{label: target, err: cli.setState(ctx, acct, tkt, target)}
		}
	}

	// Let the viewport scroll on unhandled keys in detail mode.
	if m.mode == modeDetail {
		var cmd tea.Cmd
		m.threadView, cmd = m.threadView.Update(key)
		return m, cmd
	}
	return m, nil
}

// syncThreadFrame sizes the thread viewport + composer to the frame
// they render in: a centered dialog on roomy terminals (budget
// reserves ~12 rows for head, composer, and hints in either mode),
// the full screen on small ones — where compose mode must reserve the
// composer block's rows or the ticket head shears off the top while
// the operator types. Called on resize and on every detail/compose
// mode flip.
func (m *model) syncThreadFrame() {
	frameW := m.detailFrameW()
	m.threadView.Width = frameW - 4
	m.composer.SetWidth(frameW - 6)
	if m.modalEligible() {
		m.threadView.Height = maxInt(m.detailMaxContent()-12, 3)
		return
	}
	h := maxInt(m.height-6, 3)
	if m.mode == modeCompose {
		h = maxInt(m.height-14, 3)
	}
	m.threadView.Height = h
}

// relaunchIfReady triggers the deferred post-upgrade restart once the
// operator is out of the composer. Returns nil when no upgrade is
// staged. NOTE: mutation happens via the returned model in Update —
// this method is called on the post-transition receiver.
func (m *model) relaunchIfReady() tea.Cmd {
	if m.upgradeReadyTag == "" {
		return nil
	}
	m.relaunch = m.snapshotView(m.upgradeReadyTag)
	m.upgradeReadyTag = ""
	return tea.Quit
}

// actionTarget resolves which ticket a state-change key applies to:
// the ticket ON SCREEN, or nothing. The old fallback to the last
// opened thread meant R/C/O in the health/event/cell views could
// silently mutate a ticket the operator wasn't looking at.
func (m model) actionTarget() (accountID, ticketID string) {
	switch m.mode {
	case modeList:
		if t := m.selected(); t != nil {
			return t.AccountID, t.ID
		}
	case modeDetail, modeCompose:
		if m.threadAccount != "" {
			return m.threadAccount, m.threadTicket
		}
	case modeTicketDetail:
		// The record inspector shows detailTicket — act on THAT, not
		// on whatever thread happened to be open earlier.
		if m.detailTicket != nil {
			return m.detailTicket.AccountID, m.detailTicket.ID
		}
	}
	return "", ""
}

func cellTopologyChanged(oldCells, newCells []client.AdminCell) bool {
	if len(oldCells) != len(newCells) {
		return true
	}
	old := make([]string, len(oldCells))
	for i, c := range oldCells {
		old[i] = c.Name + "\x00" + c.Endpoint
	}
	newNames := make([]string, len(newCells))
	for i, c := range newCells {
		newNames[i] = c.Name + "\x00" + c.Endpoint
	}
	sort.Strings(old)
	sort.Strings(newNames)
	for i := range old {
		if old[i] != newNames[i] {
			return true
		}
	}
	return false
}

func cellEventBackfillChanged(oldCells, newCells []client.AdminCell) bool {
	if len(oldCells) == 0 && len(newCells) == 0 {
		return false
	}
	old := make(map[string]client.AdminCell, len(oldCells))
	for _, c := range oldCells {
		old[c.Name] = c
	}
	for _, c := range newCells {
		prev, ok := old[c.Name]
		if !ok {
			continue
		}
		if prev.AccountCount != c.AccountCount || prev.ArchivedCount != c.ArchivedCount {
			return true
		}
	}
	return false
}

func (m model) View() string {
	// Drill-downs float over the live dashboard when the terminal has
	// room; the full-screen switch below is the fallback. The safety
	// valve: a dialog taller than the screen can't be centered — fall
	// through instead of shearing it.
	if dialog := m.modalDialog(); dialog != "" && lipgloss.Height(dialog) <= m.height-1 {
		base := m.viewList()
		if extra := m.footerExtra(); extra != "" {
			base += "\n" + extra
		}
		return overlayCenter(base, dialog, m.width)
	}
	var b strings.Builder
	switch m.mode {
	case modeList:
		b.WriteString(m.viewList())
	case modeDetail:
		b.WriteString(m.viewDetail())
		b.WriteString("\n" + styDim.Render("r reply · i inspect · R resolve · C close · O reopen · esc back · q quit"))
	case modeCompose:
		b.WriteString(m.viewDetail())
		b.WriteString("\n" + styTitle.Render("Reply as fleet admin") + "\n")
		b.WriteString(m.composer.View())
		b.WriteString("\n" + styDim.Render("ctrl+d send · esc cancel"))
	case modeEventDetail:
		b.WriteString(m.viewEventDetail())
	case modeCellDetail:
		b.WriteString(m.viewCellDetail())
	case modeTicketDetail:
		b.WriteString(m.viewTicketDetail())
	case modeHealth:
		b.WriteString(m.viewHealth())
		b.WriteString("\n" + styDim.Render("esc back · g refresh · q quit"))
	}
	if extra := m.footerExtra(); extra != "" {
		b.WriteString("\n" + extra)
	}
	return b.String()
}

// footerExtra is the badge+status footer — ONE line, not two. The
// list view already fills the screen; a second appended row pushes
// the frame past the terminal height and bubbletea shears the TOP
// border row to make room. The status text carries raw subprocess
// stderr / server error text — hostile bytes must not reach the
// terminal, and a stray newline must not break the footer layout.
//
// The status ALWAYS falls back to the fleet summary — an empty
// footer would blink out when a transient message clears (a ticket
// drill-down finishing, a refresh landing) and reappear on the next
// auto-tick, which reads as "the footer disappeared."
func (m model) footerExtra() string {
	badge := m.upgradeBadge()
	text := m.status
	if text == "" {
		text = m.fleetSummary()
	}
	status := ""
	if text != "" {
		status = styDim.Render(oneLine(text))
	}
	switch {
	case badge != "" && status != "":
		return badge + "  " + status
	case badge != "":
		return badge
	default:
		return status
	}
}

// fleetSummary is the persistent one-line health baseline shown when
// no transient status is active. Same wording ticketsLoadedMsg uses —
// derived so a stale value can't linger past a re-sync.
func (m model) fleetSummary() string {
	if m.loading || len(m.tickets)+len(m.cells) == 0 {
		return ""
	}
	return fmt.Sprintf("%d tickets · %s", len(m.tickets), cellSummary(m.cells))
}

// upgradeBadge is the persistent "upgrade light" pinned to the bottom
// of every view — unlike the transient status line, it stays lit for
// as long as an update is in flight, waiting on its channel, or
// installed-and-deferring, so the operator always knows the dashboard
// is (or is about to be) mid-upgrade.
func (m model) upgradeBadge() string {
	switch m.upgradePhase {
	case "installing":
		return styWarn.Render("●") + styDim.Render(" upgrade "+m.upgradeTag+" installing…")
	case "channel-wait":
		return styWarn.Render("●") + styDim.Render(" upgrade "+m.upgradeTag+" available — waiting for "+m.installVia)
	case "ready":
		return styOK.Render("●") + styDim.Render(" upgrade "+m.upgradeTag+" ready — restarts when idle")
	}
	return ""
}

// viewList is the master fullscreen dashboard: two bordered windows on
// top (cells | support) and the significant-event tail spanning the
// full width along the bottom (display-only, renders like tail -f:
// newest at the bottom). Support is the interactive window — cursor,
// drill-down, state keys — and carries the highlighted border.
func (m model) viewList() string {
	w, h := m.width, m.height
	if w < 60 {
		w = 100 // degenerate/unknown terminal — render something sane
	}
	if h < 15 {
		h = 30
	}

	// Height budget: every paneBox adds 2 border rows + 1 title row.
	// footer (hints + status) = 2. Events get about a quarter of the
	// screen; the top row gets the rest.
	footerH := 2
	eventRows := maxInt((h-footerH)/4, 4)
	topRows := maxInt(h-footerH-eventRows-6 /* 2×(border rows) + 2×(title) */, 4)
	// Fleet health strip: 4 rows (2 border + title + one line).
	// Auto-hides on short terminals — the working panes keep priority,
	// same spirit as the footer's version-over-hints rule.
	showStrip := topRows >= 12
	if showStrip {
		topRows -= 4
	}

	// Width budget: cells window ~1/3 (clamped), support the rest.
	// paneBox outer width = content + 2 border + 2 padding.
	cellsOuter := w / 3
	if cellsOuter < 30 {
		cellsOuter = 30
	}
	if cellsOuter > 46 {
		cellsOuter = 46
	}
	supportOuter := maxInt(w-cellsOuter, 40)
	cellsW, supportW, eventsW := cellsOuter-4, supportOuter-4, w-4

	// ── cells window ──────────────────────────────────────
	var cellLines []string
	if len(m.fleetCells) == 0 {
		cellLines = append(cellLines, styDim.Render("(no cells reported yet)"))
	}
	// Three lines per cell: name (with accepting/draining dot),
	// placement/version, counts. Anything denser overflows the narrow
	// cells window and fitLine silently truncates the tail.
	const cellPaneLines = 3
	fleetMax := 0
	for _, c := range m.fleetCells {
		fleetMax = maxInt(fleetMax, c.AccountCount)
	}
	for i, c := range m.fleetCells {
		dot := styOK.Render("●") // accepting
		if !c.Accepting {
			dot = styWarn.Render("●") // draining
		}
		version := styErr.Render("unreachable")
		if c.Version != "" {
			version = styDim.Render("v" + oneLine(c.Version))
		}
		nameLine := fitLine(dot+" "+oneLine(c.Name), cellsW)
		if m.focus == paneCells && i == m.cellCursor {
			nameLine = selectedLine("● "+oneLine(c.Name), cellsW)
		}
		countLine := "  " + styTitle.Render(fmt.Sprintf("%d accounts", c.AccountCount))
		if c.ArchivedCount > 0 {
			countLine += styWarn.Render(fmt.Sprintf(" · %d archived", c.ArchivedCount))
		}
		if fleetMax > 0 {
			// Load gauge relative to the fleet's biggest cell — imbalance
			// visible without reading the numbers.
			countLine += " " + styDim.Render(barGauge(c.AccountCount, fleetMax, 8))
		}
		cellLines = append(cellLines,
			nameLine,
			fitLine(fmt.Sprintf("  %s/%s · %s", c.Cloud, c.Region, version), cellsW),
			fitLine(countLine, cellsW),
		)
	}
	// Clip to the frame HERE, keeping the selected cell's block in
	// view — paneBox clips overflow from the TOP, which would drop the
	// highlight whenever the cursor sits near the top of a fleet list
	// taller than the window (same rule as the support pane below).
	if len(cellLines) > topRows {
		lo := minInt(
			maxInt(m.cellCursor*cellPaneLines+cellPaneLines-topRows, 0),
			len(cellLines)-topRows,
		)
		cellLines = cellLines[lo : lo+topRows]
	}

	// ── support window ────────────────────────────────────
	// Stage-grouped (needs-us first, closed last), stage filter
	// applied, selection rendered PLAIN under reverse-video —
	// embedded color resets would otherwise cancel the highlight
	// mid-line and make the selection invisible.
	vis := m.visibleTickets()
	var supLines []string
	if len(vis) == 0 {
		switch {
		case m.loading:
			supLines = append(supLines, styDim.Render("loading…"))
		case m.stateFilter != "":
			supLines = append(supLines, styDim.Render("no "+m.stateFilter+" tickets — f cycles the filter"))
		default:
			supLines = append(supLines, styDim.Render("no open tickets across the fleet 🎉"))
		}
	} else {
		start := 0
		if m.cursor >= topRows {
			start = m.cursor - topRows + 1
		}
		prevRank := -1
		if start > 0 {
			prevRank = stateRank(vis[start-1].State)
		}
		cursorLine := 0 // index of the cursor's row within supLines
		for i := start; i < len(vis) && i < start+topRows; i++ {
			t := vis[i]
			// Stage boundary marker when a new group begins (skipped
			// under a single-stage filter — it would be noise).
			if r := stateRank(t.State); r != prevRank && m.stateFilter == "" && prevRank != -1 {
				supLines = append(supLines, styDim.Render(fitLine("· · ·", supportW)))
			}
			prevRank = stateRank(t.State)
			if i == m.cursor {
				cursorLine = len(supLines)
			}
			age, prio, id, subject := supportCols(t, m.now(), supportW)
			if m.supportRowHighlighted(i) {
				plain := fmt.Sprintf("%s  %s  %s  %s  %s",
					plainStateBadge(t.State), age, prio, id, subject)
				supLines = append(supLines, selectedLine(plain, supportW))
				continue
			}
			prioStyled := prio
			switch t.Priority {
			case "urgent":
				prioStyled = styErr.Render(prio)
			case "high":
				prioStyled = styWarn.Render(prio)
			}
			line := fmt.Sprintf("%s  %s  %s  %s  %s",
				stateBadge(t.State), styDim.Render(age), prioStyled, id, subject)
			supLines = append(supLines, fitLine(line, supportW))
		}
		// Clip to the frame HERE, keeping the cursor's row in view:
		// separator lines inflate the count past topRows, and paneBox's
		// own clip-from-top would drop the topmost rows — including the
		// selection itself when the cursor sits near the top.
		if len(supLines) > topRows {
			lo := 0
			if cursorLine >= topRows {
				lo = cursorLine - topRows + 1
			}
			supLines = supLines[lo : lo+topRows]
		}
	}

	// ── events window (full width, bottom) ────────────────
	var evLines []string
	eventsTitle := "events"
	if len(m.events) == 0 {
		evLines = append(evLines, styDim.Render("(quiet)"))
	} else {
		selIdx := len(m.events) - 1 - minInt(m.eventScroll, len(m.events)-1)
		end := selIdx + 1
		if tail := len(m.events) - end; tail < eventRows-1 {
			// Keep the selection visible but let the window fill with
			// newer lines below it when there's room.
			end = minInt(len(m.events), selIdx+eventRows)
		}
		start := maxInt(end-eventRows, 0)
		for i := start; i < end; i++ {
			line := fitLine(renderEventLine(m.events[i]), eventsW)
			if m.focus == paneEvents && i == selIdx {
				line = selectedLine(plainEventLine(m.events[i]), eventsW)
			}
			evLines = append(evLines, line)
		}
		if m.eventScroll > 0 {
			eventsTitle = fmt.Sprintf("events · paused (%d newer — j to resume)", m.eventScroll)
		}
	}

	cellsTitle := fmt.Sprintf("cells · %d", len(m.fleetCells))
	if len(m.cells) > 0 {
		// Fan-out health from the last ticket refresh, when we have it.
		cellsTitle = "cells · " + cellSummary(m.cells)
	}
	supportTitle := fmt.Sprintf("support · %d tickets", len(m.tickets))
	if m.stateFilter != "" {
		supportTitle = fmt.Sprintf("support · %d/%d · filter: %s", len(vis), len(m.tickets), m.stateFilter)
	}
	cellsBox := paneBox(cellsTitle, cellLines, cellsW, topRows, m.focus == paneCells)
	supportBox := paneBox(supportTitle, supLines, supportW, topRows, m.focus == paneSupport)
	eventsBox := paneBox(eventsTitle, evLines, eventsW, eventRows, m.focus == paneEvents)

	top := lipgloss.JoinHorizontal(lipgloss.Top, cellsBox, supportBox)
	hints := "tab focus · j/k move · enter open · i inspect · f filter · H health · R resolve · C close · g refresh · q quit"
	ver := m.versionTag()
	// Version sits right-aligned in the corner — subdued, always
	// visible. If the terminal is too narrow for both, the version
	// wins over the hint tail (hints are re-learnable; "am I current?"
	// is the question the stamp exists to answer).
	var footer string
	if pad := w - lipgloss.Width(hints) - lipgloss.Width(ver); pad >= 1 {
		footer = hints + strings.Repeat(" ", pad) + ver
	} else {
		footer = fitLine(hints, maxInt(w-lipgloss.Width(ver)-1, 10)) + " " + ver
	}
	rows := make([]string, 0, 4)
	if showStrip {
		rows = append(rows, paneBox("fleet", []string{m.healthStripLine(w - 4)}, w-4, 1, false))
	}
	rows = append(rows, top, eventsBox, styDim.Render(footer))
	return lipgloss.JoinVertical(lipgloss.Left, rows...)
}

// versionTag is the subdued running-version stamp pinned to the
// dashboard footer — the at-a-glance answer to "am I on the latest?"
// alongside the upgrade light (dark light + current tag = yes).
func (m model) versionTag() string {
	v := m.currentVersion
	if v == "" {
		v = "dev"
	}
	return "witself-admin v" + v
}

// paneBox frames one dashboard window with a thick border, a bold
// title row, and a fixed content size (lines clipped to the newest /
// padded with blanks so the frame never jitters as data flows).
// focused windows get the accent border color.
func paneBox(title string, lines []string, contentW, contentH int, focused bool) string {
	if len(lines) > contentH {
		lines = lines[len(lines)-contentH:]
	}
	for len(lines) < contentH {
		lines = append(lines, "")
	}
	body := styTitle.Render(fitLine(title, contentW)) + "\n" + strings.Join(lines, "\n")
	st := lipgloss.NewStyle().
		Border(lipgloss.ThickBorder()).
		Padding(0, 1).
		Width(contentW + 2)
	if focused {
		st = st.BorderForeground(lipgloss.Color("6"))
	}
	return st.Render(body)
}

// fitLine truncates a (possibly styled) line to the window's content
// width, ANSI-aware, so an over-long row can never wrap and shear the
// border frame.
func fitLine(s string, width int) string {
	if width <= 1 {
		return ""
	}
	return reflowtrunc.StringWithTail(s, uint(width), "…")
}

// selectedLine renders a row in reverse video padded out to the full
// pane width, so the highlight reads as a solid bar to the panel edge
// instead of stopping at the last character. Input must be plain text
// (no ANSI) — an embedded SGR reset would cut the reverse short.
func selectedLine(plain string, width int) string {
	line := fitLine(plain, width)
	if pad := width - lipgloss.Width(line); pad > 0 {
		line += strings.Repeat(" ", pad)
	}
	return stySelected.Render(line)
}

// Support-pane column widths — fixed, most relevant on the LEFT:
// state(5) age(4) priority(6) id(20), then the subject fills whatever
// width remains. Age is right-aligned so "2h" and "45m" line up.
const (
	supAgeW   = 4
	supPrioW  = 6
	supIDW    = 20
	supFixedW = 5 + supAgeW + supPrioW + supIDW + 4*2 // + column gaps
)

// supportCols builds one ticket row's fixed-width column values,
// shared by the styled and reverse-video renders so the selection can
// never drift out of alignment with its neighbors.
func supportCols(t client.AdminTicket, now time.Time, width int) (age, prio, id, subject string) {
	return fmt.Sprintf("%*s", supAgeW, humanAge(now.Sub(t.LastActivityAt))),
		fmt.Sprintf("%-*s", supPrioW, truncate(oneLine(t.Priority), supPrioW)),
		fmt.Sprintf("%-*s", supIDW, truncate(t.ID, supIDW)),
		truncate(oneLine(t.Subject), maxInt(width-supFixedW, 10))
}

// Event-tail column widths — fixed, so the pane reads as a table
// instead of a ragged stream. Verb is sized to the longest verb we
// emit ("account.support_policy_changed", 30). Metadata is NOT a
// column: the raw JSON swallowed the useful fields; enter drills into
// the full record when the payload matters.
const (
	evVerbW = 30
	evAcctW = 20
	evCellW = 20
)

// securityVerb picks which verbs get the loud color in the tail — a
// recovery attempt, suspension, or revocation must jump out of the
// stream.
func securityVerb(verb string) bool {
	return strings.HasPrefix(verb, "recovery.") ||
		strings.HasPrefix(verb, "account.suspended") ||
		verb == "token.revoked" ||
		verb == "account.email.changed" ||
		verb == "account.support_policy_changed"
}

// eventCols builds the fixed-width column values one tail row shows:
// time, verb, account, cell, actor. Shared by the styled and plain
// renders so the selection row can never drift out of alignment with
// its neighbors.
func eventCols(e client.AdminEvent) (ts, verb, acct, cell, actor string) {
	actor = e.ActorKind
	if e.ActorID != "" {
		actor += ":" + oneLine(e.ActorID)
	}
	return e.OccurredAt.UTC().Format("15:04:05"),
		fmt.Sprintf("%-*s", evVerbW, truncate(oneLine(e.Verb), evVerbW)),
		fmt.Sprintf("%-*s", evAcctW, truncate(oneLine(e.AccountID), evAcctW)),
		fmt.Sprintf("%-*s", evCellW, truncate(oneLine(e.Cell), evCellW)),
		actor
}

// renderEventLine renders one audit event for the tail pane. Fields
// are padded BEFORE styling — ANSI bytes inside a %-*s would break the
// column grid.
func renderEventLine(e client.AdminEvent) string {
	ts, verb, acct, cell, actor := eventCols(e)
	styledVerb := styInfo.Render(verb)
	if securityVerb(e.Verb) {
		styledVerb = styErr.Render(verb)
	}
	return fmt.Sprintf(" %s  %s  %s  %s  %s",
		styDim.Render(ts), styledVerb, acct,
		styDim.Render(cell), styDim.Render(actor))
}

// plainEventLine is renderEventLine without per-field styling, for the
// reverse-video selection row (reverse over embedded color codes
// renders inconsistently across terminals).
func plainEventLine(e client.AdminEvent) string {
	ts, verb, acct, cell, actor := eventCols(e)
	return fmt.Sprintf(" %s  %s  %s  %s  %s", ts, verb, acct, cell, actor)
}

// viewEventDetail is the drill-down on one audit event: every field
// plus pretty-printed metadata, in a full-width framed window.
func (m model) viewEventDetail() string {
	e := m.detailEvent
	if e == nil {
		return styDim.Render("no event selected")
	}
	contentW := m.detailFrameW() - 4

	actor := e.ActorKind
	if e.ActorID != "" {
		actor += ":" + oneLine(e.ActorID)
	}
	lines := []string{
		fitLine(styDim.Render("id        ")+oneLine(e.ID), contentW),
		fitLine(styDim.Render("occurred  ")+e.OccurredAt.UTC().Format(time.RFC3339Nano), contentW),
		fitLine(styDim.Render("cell      ")+oneLine(e.Cell), contentW),
		fitLine(styDim.Render("account   ")+oneLine(e.AccountID), contentW),
		fitLine(styDim.Render("actor     ")+actor, contentW),
		"",
		styDim.Render("metadata"),
	}
	pretty := &bytes.Buffer{}
	if err := json.Indent(pretty, []byte(e.Metadata), "", "  "); err != nil {
		pretty.Reset()
		pretty.WriteString(string(e.Metadata))
	}
	for _, ln := range strings.Split(sanitizeText(pretty.String()), "\n") {
		lines = append(lines, fitLine("  "+ln, contentW))
	}
	title := "event · " + oneLine(e.Verb)
	lines = clipLines(lines, m.detailMaxContent()-2, contentW)
	lines = appendHint(lines, "esc close · q quit", contentW)
	return paneBox(title, lines, contentW, maxInt(len(lines), 8), true)
}

// appendHint pins a key-hint line INSIDE a detail box, matching the
// ticket-thread dialog's look — a subdued spacer then the hint. Kept
// inside the frame so the box reads as a self-contained dialog.
func appendHint(lines []string, hint string, contentW int) []string {
	return append(lines, "", styDim.Render(fitLine(hint, contentW)))
}

// viewTicketDetail is the drill-down on one ticket's RECORD — every
// field including the SLA timestamps and correlation, complementing
// the thread view (which shows the conversation).
func (m model) viewTicketDetail() string {
	t := m.detailTicket
	if t == nil {
		return styDim.Render("no ticket selected")
	}
	contentW := m.detailFrameW() - 4
	ts := func(p *time.Time) string {
		if p == nil {
			return styDim.Render("—")
		}
		return p.UTC().Format(time.RFC3339)
	}
	lines := []string{
		fitLine(styDim.Render("subject         ")+oneLine(t.Subject), contentW),
		fitLine(styDim.Render("account         ")+oneLine(t.AccountID), contentW),
		fitLine(styDim.Render("cell            ")+oneLine(t.Cell), contentW),
		fitLine(styDim.Render("state           ")+stateBadge(t.State)+" "+t.State, contentW),
		fitLine(styDim.Render("category        ")+t.Category, contentW),
		fitLine(styDim.Render("priority        ")+t.Priority, contentW),
		fitLine(styDim.Render("opened          ")+t.OpenedAt.UTC().Format(time.RFC3339)+" by "+t.OpenedByKind+":"+oneLine(t.OpenedByID), contentW),
		fitLine(styDim.Render("first response  ")+ts(t.FirstResponseAt), contentW),
		fitLine(styDim.Render("resolved        ")+ts(t.ResolvedAt), contentW),
		fitLine(styDim.Render("closed          ")+ts(t.ClosedAt), contentW),
		fitLine(styDim.Render("last activity   ")+t.LastActivityAt.UTC().Format(time.RFC3339), contentW),
		fitLine(styDim.Render("last message    ")+oneLine(t.LastMessageID), contentW),
	}
	if len(t.Correlation) > 0 && string(t.Correlation) != "[]" {
		lines = append(lines, "", styDim.Render("correlation"))
		lines = append(lines, prettyJSONLines(t.Correlation, contentW)...)
	}
	if len(t.Metadata) > 0 && string(t.Metadata) != "{}" {
		lines = append(lines, "", styDim.Render("metadata"))
		lines = append(lines, prettyJSONLines(t.Metadata, contentW)...)
	}
	lines = clipLines(lines, m.detailMaxContent()-2, contentW)
	lines = appendHint(lines, "esc close · R resolve · C close · O reopen · q quit", contentW)
	return paneBox("ticket · "+oneLine(t.ID), lines, contentW, maxInt(len(lines), 8), true)
}

// prettyJSONLines indents raw JSON for a detail panel, sanitized and
// width-fitted; falls back to the raw string when it isn't valid JSON.
func prettyJSONLines(raw []byte, width int) []string {
	pretty := &bytes.Buffer{}
	if err := json.Indent(pretty, raw, "", "  "); err != nil {
		pretty.Reset()
		pretty.Write(raw)
	}
	var out []string
	for _, ln := range strings.Split(sanitizeText(pretty.String()), "\n") {
		out = append(out, fitLine("  "+ln, width))
	}
	return out
}

// viewCellDetail is the drill-down on one fleet cell.
func (m model) viewCellDetail() string {
	c := m.detailCell
	if c == nil {
		return styDim.Render("no cell selected")
	}
	contentW := m.detailFrameW() - 4
	accepting := "accepting new accounts"
	if !c.Accepting {
		accepting = "draining (not accepting)"
	}
	version := "unreachable"
	if c.Version != "" {
		version = "v" + oneLine(c.Version)
	}
	provision := "not linked to the control plane"
	if c.HasProvisionToken {
		provision = "provision token on file"
	}
	lines := []string{
		fitLine(styDim.Render("cloud/region  ")+fmt.Sprintf("%s / %s", c.Cloud, c.Region), contentW),
		fitLine(styDim.Render("endpoint      ")+oneLine(c.Endpoint), contentW),
		fitLine(styDim.Render("software      ")+version, contentW),
		fitLine(styDim.Render("placement     ")+accepting, contentW),
		fitLine(styDim.Render("trust link    ")+provision, contentW),
		fitLine(styDim.Render("accounts      ")+styTitle.Render(fmt.Sprintf("%d", c.AccountCount)), contentW),
		fitLine(styDim.Render("archived      ")+fmt.Sprintf("%d (evacuated from this cell, awaiting placement)", c.ArchivedCount), contentW),
	}
	lines = appendHint(lines, "esc close · q quit", contentW)
	return paneBox("cell · "+oneLine(c.Name), lines, contentW, maxInt(len(lines), 6), true)
}

func (m model) viewDetail() string {
	if m.thread == nil {
		return styDim.Render("loading thread…")
	}
	t := m.thread.Ticket
	head := fmt.Sprintf("%s %s  %s\n%s · %s · %s\n",
		stateBadge(t.State),
		styTitle.Render(oneLine(t.Subject)),
		styDim.Render(t.ID),
		styDim.Render(t.AccountID),
		t.Priority,
		styDim.Render("opened "+t.OpenedAt.UTC().Format("2006-01-02 15:04")),
	)
	return head + "\n" + m.threadView.View()
}

// renderThread lays out the message history for the viewport.
func renderThread(res client.GetSupportTicketResult, width int) string {
	var b strings.Builder
	for _, msg := range res.Messages {
		// Author fields are server-controlled — same injection defense
		// as the body.
		who := oneLine(msg.AuthorKind)
		if msg.AuthorID != "" {
			who += ":" + oneLine(msg.AuthorID)
		}
		hdr := fmt.Sprintf("── %s  %s ", who, msg.PostedAt.UTC().Format("2006-01-02 15:04"))
		if pad := width - lipgloss.Width(hdr) - 1; pad > 0 {
			hdr += strings.Repeat("─", pad)
		}
		style := styInfo
		if msg.AuthorKind == "fleet_admin" {
			style = styOK
		}
		b.WriteString(style.Render(hdr) + "\n")
		b.WriteString(sanitizeText(msg.Body) + "\n\n")
	}
	return b.String()
}

// sanitizeText strips C0 control chars, DEL, and the C1 range (keeps
// \n, \t) — the terminal-injection defense for everything
// customer-controlled. C1 matters: U+009B is a single-rune CSI that
// C1-honoring terminals execute exactly like ESC-[, so filtering only
// C0 leaves the door open. A hostile ticket body must not be able to
// redraw the TUI or cut a reverse-video run short.
func sanitizeText(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r < 0x20 || (r >= 0x7F && r <= 0x9F) {
			return -1
		}
		return r
	}, s)
}

func oneLine(s string) string {
	s = strings.ReplaceAll(sanitizeText(s), "\n", " ")
	return strings.ReplaceAll(s, "\t", " ")
}

func truncate(s string, n int) string {
	if n <= 1 || len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// humanAge renders a compact "how long ago" for the list column.
func humanAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// cellSummary compresses the fan-out health into one phrase.
func cellSummary(cells []client.AdminCellStatus) string {
	if len(cells) == 0 {
		return "no cells reported"
	}
	ok := 0
	for _, c := range cells {
		if c.Status == "ok" {
			ok++
		}
	}
	if ok == len(cells) {
		return fmt.Sprintf("%d/%d cells ok", ok, len(cells))
	}
	var bad []string
	for _, c := range cells {
		if c.Status != "ok" {
			bad = append(bad, c.Name)
		}
	}
	return fmt.Sprintf("%d/%d cells ok — degraded: %s", ok, len(cells), strings.Join(bad, ", "))
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
