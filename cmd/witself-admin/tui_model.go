package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

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
)

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
	cli   *adminCLI
	ctx   context.Context
	watch <-chan client.AdminTicket

	mode    uiMode
	width   int
	height  int
	tickets []client.AdminTicket
	cells   []client.AdminCellStatus
	cursor  int

	// Detail state.
	thread        *client.GetSupportTicketResult
	threadAccount string
	threadTicket  string
	threadView    viewport.Model

	composer textarea.Model

	status  string // transient one-line message (footer)
	loading bool
	now     func() time.Time // injectable clock for age rendering in tests

	// Self-upgrade state. binPath/installVia are resolved at startup;
	// upgradeReadyTag is set once a newer binary is INSTALLED on disk
	// (relaunch deferred while composing); relaunch non-nil tells
	// main() to re-exec after bubbletea releases the terminal.
	binPath         string
	installVia      string
	currentVersion  string
	upgradeReadyTag string
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
		composer: ta,
		now:      time.Now,
		loading:  true,
		status:   "loading fleet tickets…",
	}
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
	return m
}

func (m model) Init() tea.Cmd {
	cmds := []tea.Cmd{m.loadTickets(), m.awaitWatch()}
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
	}
	r.ThreadAccount, r.ThreadTicket = m.threadAccount, m.threadTicket
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

// upsertTicket merges a live watch update into the list, keeping the
// newest-activity-first order the list view promises.
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
	sort.SliceStable(tickets, func(i, j int) bool {
		return tickets[i].LastActivityAt.After(tickets[j].LastActivityAt)
	})
	return tickets
}

func (m model) selected() *client.AdminTicket {
	if m.cursor < 0 || m.cursor >= len(m.tickets) {
		return nil
	}
	return &m.tickets[m.cursor]
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
// re-sort/refresh; falls back to clamping the raw index when the ticket
// vanished from the list.
func (m *model) anchorCursor(id string) {
	if id != "" {
		for i := range m.tickets {
			if m.tickets[i].ID == id {
				m.cursor = i
				return
			}
		}
	}
	if m.cursor >= len(m.tickets) {
		m.cursor = maxInt(len(m.tickets)-1, 0)
	}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.threadView.Width = msg.Width
		m.threadView.Height = maxInt(msg.Height-6, 3)
		m.composer.SetWidth(msg.Width - 2)
		return m, nil

	case ticketsLoadedMsg:
		m.loading = false
		if msg.err != nil {
			m.status = "load failed: " + msg.err.Error()
			return m, nil
		}
		anchor := m.selectedID()
		m.tickets = msg.list.Tickets
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
		m.threadView.SetContent(renderThread(msg.res, m.width))
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
		return m, nil

	case upgradeCheckMsg:
		return m, tea.Batch(
			m.checkUpgrade(),
			tea.Tick(upgradeCheckInterval, func(time.Time) tea.Msg {
				return upgradeCheckMsg{}
			}),
		)

	case upgradeAvailableMsg:
		m.status = "installing update " + msg.tag + " in the background…"
		return m, m.applyUpgrade(msg.tag)

	case upgradeAppliedMsg:
		if msg.err != nil {
			m.status = "self-upgrade failed: " + msg.err.Error()
			return m, nil
		}
		if msg.noop {
			// The channel hasn't caught up to the tag yet (brew tap
			// publish lag). No restart — the next periodic check
			// retries through the normal path.
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
		// If the updated ticket is open in detail, refresh the thread.
		var cmds []tea.Cmd
		if m.mode != modeList && msg.ticket.ID == m.threadTicket {
			cmds = append(cmds, m.loadThread(m.threadAccount, m.threadTicket))
		}
		cmds = append(cmds, m.awaitWatch())
		return m, tea.Batch(cmds...)

	case watchStoppedMsg:
		m.watch = nil
		m.status = "live watch stopped — press g to refresh manually"
		return m, nil

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
			m.status = "reply discarded"
			return m, m.relaunchIfReady()
		case "ctrl+d":
			body := strings.TrimSpace(m.composer.Value())
			if body == "" {
				m.status = "empty reply not sent"
				m.mode = modeDetail
				return m, m.relaunchIfReady()
			}
			cli, ctx := m.cli, m.ctx
			acct, tkt := m.threadAccount, m.threadTicket
			m.composer.Reset()
			m.mode = modeDetail
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
		return m, m.loadTickets()
	case "j", "down":
		if m.mode == modeList && m.cursor < len(m.tickets)-1 {
			m.cursor++
		}
	case "k", "up":
		if m.mode == modeList && m.cursor > 0 {
			m.cursor--
		}
	case "enter":
		if m.mode == modeList {
			if t := m.selected(); t != nil {
				m.threadAccount, m.threadTicket = t.AccountID, t.ID
				m.loading = true
				m.status = "loading thread…"
				return m, m.loadThread(t.AccountID, t.ID)
			}
		}
	case "esc":
		if m.mode == modeDetail {
			m.mode = modeList
			m.thread = nil
			m.status = ""
		}
	case "r":
		if m.mode == modeDetail {
			// Fresh composer per entry — defense in depth against a
			// stale draft leaking across tickets. (The upgrade-resume
			// path restores its draft via SetValue directly, not
			// through this handler.)
			m.composer.Reset()
			m.mode = modeCompose
			m.composer.Focus()
			return m, textarea.Blink
		}
	case "R", "C", "O":
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
// the open thread in detail mode, else the selected row.
func (m model) actionTarget() (accountID, ticketID string) {
	if m.mode != modeList && m.threadAccount != "" {
		return m.threadAccount, m.threadTicket
	}
	if t := m.selected(); t != nil {
		return t.AccountID, t.ID
	}
	return "", ""
}

func (m model) View() string {
	var b strings.Builder
	switch m.mode {
	case modeList:
		b.WriteString(m.viewList())
	case modeDetail:
		b.WriteString(m.viewDetail())
		b.WriteString("\n" + styDim.Render("r reply · R resolve · C close · O reopen · esc back · q quit"))
	case modeCompose:
		b.WriteString(m.viewDetail())
		b.WriteString("\n" + styTitle.Render("Reply as fleet admin") + "\n")
		b.WriteString(m.composer.View())
		b.WriteString("\n" + styDim.Render("ctrl+d send · esc cancel"))
	}
	if m.status != "" {
		// The status line carries raw subprocess stderr / server error
		// text — hostile bytes must not reach the terminal, and a
		// stray newline must not break the footer layout.
		b.WriteString("\n" + styDim.Render(oneLine(m.status)))
	}
	return b.String()
}

func (m model) viewList() string {
	var b strings.Builder
	b.WriteString(styTitle.Render("witself — support") + "  " + styDim.Render(cellSummary(m.cells)) + "\n\n")
	if len(m.tickets) == 0 {
		if m.loading {
			b.WriteString(styDim.Render("  loading…"))
		} else {
			b.WriteString(styDim.Render("  no open tickets across the fleet 🎉"))
		}
		b.WriteString("\n\n" + styDim.Render("g refresh · q quit"))
		return b.String()
	}
	rows := m.height - 6
	if rows < 3 {
		rows = len(m.tickets)
	}
	start := 0
	if m.cursor >= rows {
		start = m.cursor - rows + 1
	}
	for i := start; i < len(m.tickets) && i < start+rows; i++ {
		t := m.tickets[i]
		line := fmt.Sprintf(" %s %-8s %-22s %-14s %s",
			stateBadge(t.State),
			t.Priority,
			truncate(t.ID, 22),
			truncate(t.Cell, 14),
			truncate(oneLine(t.Subject), maxInt(m.width-56, 12)),
		)
		age := styDim.Render(" " + humanAge(m.now().Sub(t.LastActivityAt)))
		if i == m.cursor {
			b.WriteString(stySelected.Render(line) + age + "\n")
		} else {
			b.WriteString(line + age + "\n")
		}
	}
	b.WriteString("\n" + styDim.Render("enter open · j/k move · R resolve · C close · g refresh · q quit"))
	return b.String()
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

// sanitizeText strips C0 control chars + DEL (keeps \n, \t) — the same
// terminal-injection defense the CLIs apply. A hostile ticket body must
// not be able to redraw the TUI.
func sanitizeText(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r < 0x20 || r == 0x7F {
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
