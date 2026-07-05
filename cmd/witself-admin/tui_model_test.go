package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/witwave-ai/witself/internal/client"
)

func mkTicket(id, state string, activity time.Time) client.AdminTicket {
	return client.AdminTicket{
		SupportTicket: client.SupportTicket{
			ID:             id,
			AccountID:      "acc_1",
			State:          state,
			Priority:       "normal",
			Subject:        "subject " + id,
			LastActivityAt: activity,
		},
		Cell: "aws-sandbox-usw2-dev",
	}
}

// TestUpsertTicketOrdering pins the live-update merge under the
// stage-grouped order: replaced in place by id, inserted when new,
// re-sorted so needs-attention stages float and finished work sinks —
// even when the finished ticket has the freshest activity.
func TestUpsertTicketOrdering(t *testing.T) {
	t0 := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	tickets := []client.AdminTicket{
		mkTicket("tkt_b", "awaiting_admin", t0.Add(-1*time.Hour)),
		mkTicket("tkt_a", "awaiting_admin", t0.Add(-2*time.Hour)),
	}
	// New same-stage ticket with the freshest activity lands on top.
	tickets = upsertTicket(tickets, mkTicket("tkt_c", "awaiting_admin", t0))
	if tickets[0].ID != "tkt_c" || len(tickets) != 3 {
		t.Fatalf("insert: order = %v", ids(tickets))
	}
	// A ticket updated to resolved SINKS below the active group, even
	// though its activity is the newest — stage outranks recency.
	tickets = upsertTicket(tickets, mkTicket("tkt_a", "resolved", t0.Add(time.Minute)))
	if len(tickets) != 3 {
		t.Fatalf("update duplicated: %v", ids(tickets))
	}
	last := tickets[len(tickets)-1]
	if last.ID != "tkt_a" || last.State != "resolved" {
		t.Fatalf("resolved ticket must sink to the bottom: %v (last=%s/%s)", ids(tickets), last.ID, last.State)
	}
	if tickets[0].ID != "tkt_c" {
		t.Fatalf("active group must stay on top: %v", ids(tickets))
	}
}

// TestStateFilterCycle pins the f-key filter: all → active → resolved
// → closed → all, with the cursor following the selected ticket when
// it survives the narrowing and all actions operating on the filtered
// view.
func TestStateFilterCycle(t *testing.T) {
	t0 := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	m := newModel(t.Context(), &adminCLI{bin: "/nonexistent"}, nil)
	m.loading = false
	m.width, m.height = 100, 30
	m.tickets = []client.AdminTicket{
		mkTicket("tkt_active", "awaiting_admin", t0),
		mkTicket("tkt_done", "resolved", t0.Add(-time.Hour)),
	}
	sortTickets(m.tickets)

	// Unfiltered: both visible, active first.
	if got := m.visibleTickets(); len(got) != 2 || got[0].ID != "tkt_active" {
		t.Fatalf("unfiltered order: %v", ids(got))
	}
	// f → active: only the active ticket remains, selectable.
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("f")})
	m2 := next.(model)
	if m2.stateFilter != "active" || len(m2.visibleTickets()) != 1 {
		t.Fatalf("active filter: %q %v", m2.stateFilter, ids(m2.visibleTickets()))
	}
	if m2.selected().ID != "tkt_active" {
		t.Fatalf("selection after filter: %v", m2.selected().ID)
	}
	// f → resolved.
	next, _ = m2.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("f")})
	m3 := next.(model)
	if m3.stateFilter != "resolved" || m3.selected() == nil || m3.selected().ID != "tkt_done" {
		t.Fatalf("resolved filter: %q sel=%v", m3.stateFilter, m3.selected())
	}
	// Title reflects the narrowing.
	if !strings.Contains(m3.viewList(), "filter: resolved") {
		t.Fatal("filter must show in the pane title")
	}
	// f → closed (none) → selection nil.
	next, _ = m3.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("f")})
	m4 := next.(model)
	if m4.selected() != nil {
		t.Fatal("empty filter must yield no selection")
	}
	// f → back to all.
	next, _ = m4.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("f")})
	m5 := next.(model)
	if m5.stateFilter != "" || len(m5.visibleTickets()) != 2 {
		t.Fatalf("cycle back to all: %q", m5.stateFilter)
	}
}

func ids(ts []client.AdminTicket) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.ID
	}
	return out
}

// TestListNavigation pins j/k cursor movement bounds and quit keys.
func TestListNavigation(t *testing.T) {
	m := newModel(t.Context(), &adminCLI{bin: "/nonexistent"}, nil)
	t0 := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	m.tickets = []client.AdminTicket{
		mkTicket("tkt_1", "awaiting_admin", t0),
		mkTicket("tkt_2", "awaiting_admin", t0.Add(-time.Hour)),
	}
	m.loading = false

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	m = next.(model)
	if m.cursor != 1 {
		t.Fatalf("j: cursor = %d, want 1", m.cursor)
	}
	// Lower bound: j at the end stays put.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	m = next.(model)
	if m.cursor != 1 {
		t.Fatalf("j at end: cursor = %d, want 1", m.cursor)
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	m = next.(model)
	if m.cursor != 0 {
		t.Fatalf("k: cursor = %d, want 0", m.cursor)
	}
	// q quits.
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd == nil {
		t.Fatal("q should produce a quit command")
	}
}

// TestWatchUpdateFlow pins that a live watch message upserts the list
// and re-arms the channel pump.
func TestWatchUpdateFlow(t *testing.T) {
	ch := make(chan client.AdminTicket, 1)
	m := newModel(t.Context(), &adminCLI{bin: "/nonexistent"}, ch)
	t0 := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	m.tickets = []client.AdminTicket{mkTicket("tkt_1", "awaiting_admin", t0)}

	next, cmd := m.Update(watchTicketMsg{ticket: mkTicket("tkt_2", "awaiting_admin", t0.Add(time.Minute))})
	m = next.(model)
	if len(m.tickets) != 2 || m.tickets[0].ID != "tkt_2" {
		t.Fatalf("watch upsert: %v", ids(m.tickets))
	}
	if cmd == nil {
		t.Fatal("watch message must re-arm the pump (nil cmd)")
	}
}

// TestSanitizeText pins the terminal-injection defense in the thread
// renderer — the same contract the CLIs enforce.
func TestSanitizeText(t *testing.T) {
	in := "\x1b[2J\x1b[Hpwned\x07 but\ttabs\nand newlines live"
	got := sanitizeText(in)
	if strings.ContainsAny(got, "\x1b\x07") {
		t.Fatalf("escape chars survived: %q", got)
	}
	if !strings.Contains(got, "\t") || !strings.Contains(got, "\n") {
		t.Fatalf("tab/newline should survive: %q", got)
	}
}

// TestCellSummary pins the degraded-fleet phrasing the footer shows.
func TestCellSummary(t *testing.T) {
	ok := client.AdminCellStatus{Name: "cell-a", Status: "ok"}
	bad := client.AdminCellStatus{Name: "cell-b", Status: "timeout"}
	if got := cellSummary([]client.AdminCellStatus{ok}); got != "1/1 cells ok" {
		t.Errorf("all ok: %q", got)
	}
	got := cellSummary([]client.AdminCellStatus{ok, bad})
	if !strings.Contains(got, "1/2 cells ok") || !strings.Contains(got, "cell-b") {
		t.Errorf("degraded: %q", got)
	}
	if got := cellSummary(nil); got != "no cells reported" {
		t.Errorf("empty: %q", got)
	}
}

// TestHumanAge pins the compact age column.
func TestHumanAge(t *testing.T) {
	tests := map[time.Duration]string{
		30 * time.Second:    "now",
		5 * time.Minute:     "5m",
		3 * time.Hour:       "3h",
		72 * time.Hour:      "3d",
		30 * 24 * time.Hour: "30d",
	}
	for d, want := range tests {
		if got := humanAge(d); got != want {
			t.Errorf("humanAge(%v) = %q, want %q", d, got, want)
		}
	}
}

// TestComposeDiscard pins that esc leaves compose without sending and
// an empty ctrl+d refuses to send.
func TestComposeDiscard(t *testing.T) {
	m := newModel(t.Context(), &adminCLI{bin: "/nonexistent"}, nil)
	m.mode = modeCompose
	m.threadAccount, m.threadTicket = "acc_1", "tkt_1"

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m2 := next.(model)
	if m2.mode != modeDetail {
		t.Fatalf("esc: mode = %v, want detail", m2.mode)
	}

	m.mode = modeCompose
	m.composer.SetValue("   ")
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
	m3 := next.(model)
	if m3.mode != modeDetail || cmd != nil {
		t.Fatalf("empty ctrl+d must not send: mode=%v cmd=%v", m3.mode, cmd)
	}
}

// TestNewerVersion pins the self-upgrade trigger rule: strictly-newer
// semver only, and anything unparseable (dev builds, garbage tags)
// fails safe toward NOT upgrading.
func TestNewerVersion(t *testing.T) {
	tests := []struct {
		current, latest string
		want            bool
	}{
		{"0.0.94", "v0.0.95", true},
		{"0.0.94", "v0.0.94", false},
		{"0.0.95", "v0.0.94", false},
		{"0.0.94", "v0.1.0", true},
		{"0.9.9", "v1.0.0", true},
		{"dev", "v99.0.0", false},       // source builds never self-upgrade
		{"0.0.94", "nightly", false},    // unparseable tag
		{"", "v0.0.95", false},          // no current version baked in
		{"0.0.94", "v0.0.95-rc1", true}, // prerelease suffix stripped
	}
	for _, tc := range tests {
		if got := newerVersion(tc.current, tc.latest); got != tc.want {
			t.Errorf("newerVersion(%q, %q) = %v, want %v", tc.current, tc.latest, got, tc.want)
		}
	}
}

// TestInstallMethod pins the channel-detection rule the upgrade path
// routes on.
func TestInstallMethod(t *testing.T) {
	tests := map[string]string{
		"/opt/homebrew/Cellar/witself-admin/0.0.94/bin/witself-admin": "brew",
		"/usr/local/Cellar/witself-admin/0.0.94/bin/witself-admin":    "brew",
		"/usr/local/bin/witself-admin":                                "binary",
		"/Users/scott/go/bin/witself-admin":                           "binary",
	}
	for path, want := range tests {
		if got := installMethod(path); got != want {
			t.Errorf("installMethod(%q) = %q, want %q", path, got, want)
		}
	}
}

// TestResumeStateRoundTrip pins the --resume wire format: whatever the
// old process snapshots, the new process must restore bit-for-bit —
// especially a multi-line draft with quotes and unicode.
func TestResumeStateRoundTrip(t *testing.T) {
	in := resumeState{
		Mode:          "compose",
		Cursor:        3,
		ThreadAccount: "acc_1",
		ThreadTicket:  "tkt_abc",
		Draft:         "line one\nline \"two\" — π ✓\n\ttabbed",
		UpgradedTo:    "v0.0.95",
	}
	got, err := decodeResumeState(in.encode())
	if err != nil {
		t.Fatalf("round-trip: %v", err)
	}
	if got != in {
		t.Fatalf("round-trip mismatch:\n got  %+v\n want %+v", got, in)
	}
	if _, err := decodeResumeState("not-hex!"); err == nil {
		t.Fatal("garbage resume must error, not panic")
	}
}

// TestThreadLoadDoesNotYankComposer pins the draft-protection rule: a
// live-watch refresh landing while the operator is typing must not
// switch modes or touch the draft.
func TestThreadLoadDoesNotYankComposer(t *testing.T) {
	m := newModel(t.Context(), &adminCLI{bin: "/nonexistent"}, nil)
	m.mode = modeCompose
	m.threadAccount, m.threadTicket = "acc_1", "tkt_1"
	m.composer.SetValue("precious draft")

	next, _ := m.Update(threadLoadedMsg{res: client.GetSupportTicketResult{}})
	m2 := next.(model)
	if m2.mode != modeCompose {
		t.Fatalf("thread refresh yanked composer: mode = %v", m2.mode)
	}
	if m2.composer.Value() != "precious draft" {
		t.Fatalf("draft lost: %q", m2.composer.Value())
	}
}

// TestUpgradeDeferredWhileComposing pins the restart-safety rule: an
// installed upgrade waits out the composer, then fires on exit with
// the draft in the snapshot.
func TestUpgradeDeferredWhileComposing(t *testing.T) {
	m := newModel(t.Context(), &adminCLI{bin: "/nonexistent"}, nil)
	m = m.withSelfUpgrade("/usr/local/bin/witself-admin", "0.0.94")
	m.mode = modeCompose
	m.threadAccount, m.threadTicket = "acc_1", "tkt_1"
	m.composer.SetValue("mid-thought")

	// Upgrade lands while composing → deferred, no quit.
	next, cmd := m.Update(upgradeAppliedMsg{tag: "v0.0.95"})
	m2 := next.(model)
	if m2.relaunch != nil || cmd != nil {
		t.Fatal("upgrade applied mid-compose must defer, not quit")
	}
	if m2.upgradeReadyTag != "v0.0.95" {
		t.Fatalf("deferred tag = %q", m2.upgradeReadyTag)
	}

	// esc leaves compose → relaunch fires with the (discarded-draft)
	// snapshot. Draft is only preserved when the upgrade interrupts
	// compose directly, not when the user abandons the reply.
	next, cmd = m2.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m3 := next.(model)
	if m3.relaunch == nil || cmd == nil {
		t.Fatal("leaving compose with a staged upgrade must relaunch")
	}
	if m3.relaunch.UpgradedTo != "v0.0.95" || m3.relaunch.ThreadTicket != "tkt_1" {
		t.Fatalf("snapshot = %+v", m3.relaunch)
	}
}

// TestUpgradeImmediateInList pins that an installed upgrade applies
// right away when the operator is just looking at a settled list (no
// action in flight, not composing).
func TestUpgradeImmediateInList(t *testing.T) {
	m := newModel(t.Context(), &adminCLI{bin: "/nonexistent"}, nil)
	m = m.withSelfUpgrade("/usr/local/bin/witself-admin", "0.0.94")
	m.loading = false // list is settled; nothing in flight

	next, cmd := m.Update(upgradeAppliedMsg{tag: "v0.0.95"})
	m2 := next.(model)
	if m2.relaunch == nil || cmd == nil {
		t.Fatal("upgrade in list mode must relaunch immediately")
	}
	if m2.relaunch.Mode != "list" || m2.relaunch.UpgradedTo != "v0.0.95" {
		t.Fatalf("snapshot = %+v", m2.relaunch)
	}
}

// TestThreadLoadErrorKeepsView pins the fix for the review's ejection
// finding: a failed BACKGROUND thread refresh must leave the operator
// where they are (detail or compose), surfacing only a status line.
func TestThreadLoadErrorKeepsView(t *testing.T) {
	m := newModel(t.Context(), &adminCLI{bin: "/nonexistent"}, nil)
	m.mode = modeDetail
	m.threadAccount, m.threadTicket = "acc_1", "tkt_1"

	next, _ := m.Update(threadLoadedMsg{err: errFake})
	m2 := next.(model)
	if m2.mode != modeDetail {
		t.Fatalf("failed refresh ejected from detail: mode = %v", m2.mode)
	}
	if !strings.Contains(m2.status, "show failed") {
		t.Fatalf("status = %q", m2.status)
	}
}

// TestEscActuallyDiscardsDraft pins the fix for the stale-draft leak:
// after esc the composer must be empty, so a later 'r' on a different
// ticket cannot send ticket A's text to ticket B's customer.
func TestEscActuallyDiscardsDraft(t *testing.T) {
	m := newModel(t.Context(), &adminCLI{bin: "/nonexistent"}, nil)
	m.mode = modeCompose
	m.threadAccount, m.threadTicket = "acc_1", "tkt_a"
	m.composer.SetValue("ticket A secrets")

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m2 := next.(model)
	if got := m2.composer.Value(); got != "" {
		t.Fatalf("draft survived esc: %q", got)
	}
}

// TestCursorAnchorsAcrossResort pins the wrong-ticket-action fix: a
// live watch update that re-sorts the list must keep the SELECTION on
// the same ticket id, not the same index.
func TestCursorAnchorsAcrossResort(t *testing.T) {
	t0 := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	m := newModel(t.Context(), &adminCLI{bin: "/nonexistent"}, nil)
	m.tickets = []client.AdminTicket{
		mkTicket("tkt_x", "awaiting_admin", t0),
		mkTicket("tkt_y", "awaiting_admin", t0.Add(-time.Hour)),
	}
	m.cursor = 1 // operator highlighted tkt_y, about to press R

	// tkt_z lands with the freshest activity: list becomes z, x, y.
	next, _ := m.Update(watchTicketMsg{ticket: mkTicket("tkt_z", "awaiting_admin", t0.Add(time.Minute))})
	m2 := next.(model)
	if got := m2.selectedID(); got != "tkt_y" {
		t.Fatalf("selection drifted to %q after re-sort, want tkt_y", got)
	}
}

// TestRenderThreadSanitizesAuthor pins that server-controlled author
// fields can't smuggle terminal escapes through the thread header.
func TestRenderThreadSanitizesAuthor(t *testing.T) {
	res := client.GetSupportTicketResult{
		Messages: []client.SupportTicketMessage{{
			AuthorKind: "fleet_admin",
			AuthorID:   "mal\x1b[2Jlory",
			Body:       "hello",
		}},
	}
	out := renderThread(res, 0)
	if strings.Contains(out, "\x1b[2J") {
		t.Fatalf("escape sequence survived the author header: %q", out)
	}
}

// errFake is a reusable sentinel for failure-path tests.
var errFake = fmt.Errorf("subprocess exploded")

func mkEvent(id, verb string, at time.Time) client.AdminEvent {
	return client.AdminEvent{
		ID: id, AccountID: "acc_1", OccurredAt: at,
		ActorKind: "control_plane", Verb: verb,
		Metadata: []byte(`{"k":"v"}`), Cell: "aws-sandbox-usw2-dev",
	}
}

// TestAppendEvent pins the events-tail semantics: newest last (tail -f
// order), duplicate ids dropped (the watch stream re-emits around its
// high-water mark), oldest rolled off past the cap.
func TestAppendEvent(t *testing.T) {
	t0 := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	var tail []client.AdminEvent
	tail = appendEvent(tail, mkEvent("evt_1", "account.activated", t0))
	tail = appendEvent(tail, mkEvent("evt_2", "recovery.requested", t0.Add(time.Minute)))
	if len(tail) != 2 || tail[1].ID != "evt_2" {
		t.Fatalf("order: %v", tail)
	}
	// Duplicate dropped.
	tail = appendEvent(tail, mkEvent("evt_2", "recovery.requested", t0.Add(time.Minute)))
	if len(tail) != 2 {
		t.Fatalf("duplicate not dropped: %d entries", len(tail))
	}
	// Cap rolls the OLDEST off.
	for i := 0; i < eventTailCap+10; i++ {
		tail = appendEvent(tail, mkEvent(fmt.Sprintf("evt_x%d", i), "account.activated", t0.Add(time.Duration(i)*time.Second)))
	}
	if len(tail) != eventTailCap {
		t.Fatalf("cap: %d entries, want %d", len(tail), eventTailCap)
	}
	if tail[0].ID == "evt_1" {
		t.Fatal("oldest entry survived past the cap")
	}
}

// TestEventsSeedReversal pins that the newest-first seed renders as a
// tail (newest LAST) so live appends continue the same direction.
func TestEventsSeedReversal(t *testing.T) {
	t0 := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	m := newModel(t.Context(), &adminCLI{bin: "/nonexistent"}, nil)
	seed := []client.AdminEvent{ // newest first, as the server returns
		mkEvent("evt_new", "recovery.requested", t0.Add(time.Hour)),
		mkEvent("evt_old", "account.activated", t0),
	}
	next, _ := m.Update(eventsSeedMsg{events: seed})
	m2 := next.(model)
	if len(m2.events) != 2 || m2.events[0].ID != "evt_old" || m2.events[1].ID != "evt_new" {
		t.Fatalf("seed order: %v", m2.events)
	}
	// A live append lands after the seed's newest.
	next, _ = m2.Update(watchEventMsg{event: mkEvent("evt_live", "token.revoked", t0.Add(2*time.Hour))})
	m3 := next.(model)
	if m3.events[len(m3.events)-1].ID != "evt_live" {
		t.Fatalf("live append order: %v", m3.events)
	}
}

// TestRenderEventLineSanitizes pins injection defense on the event
// tail — actor ids and metadata are server-side strings.
func TestRenderEventLineSanitizes(t *testing.T) {
	e := mkEvent("evt_1", "recovery.requested", time.Now())
	e.ActorID = "mal\x1b[2Jlory"
	e.Metadata = []byte("{\"x\":\"\x07bell\"}")
	out := renderEventLine(e, 120)
	if strings.Contains(out, "\x1b[2J") || strings.Contains(out, "\x07") {
		t.Fatalf("escape survived event line: %q", out)
	}
}

// TestViewListRendersPanes smoke-tests the master view: all three pane
// headers present, ticket + cell + event content rendered.
func TestViewListRendersPanes(t *testing.T) {
	t0 := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	m := newModel(t.Context(), &adminCLI{bin: "/nonexistent"}, nil)
	m.loading = false
	m.width, m.height = 120, 40
	m.fleetCells = []client.AdminCell{{
		Name: "aws-sandbox-usw2-dev", Cloud: "aws", Region: "us-west-2",
		Accepting: true, AccountCount: 12,
	}}
	m.tickets = []client.AdminTicket{mkTicket("tkt_1", "awaiting_admin", t0)}
	m.events = []client.AdminEvent{mkEvent("evt_1", "recovery.requested", t0)}
	m.now = func() time.Time { return t0.Add(time.Minute) }

	out := m.viewList()
	for _, want := range []string{"cells", "support", "events", "12 accounts", "tkt_1", "recovery.requested"} {
		if !strings.Contains(out, want) {
			t.Errorf("view missing %q", want)
		}
	}
}

// TestUpgradeBadgeLifecycle pins the footer upgrade light: lit yellow
// while installing or waiting on a lagging channel, green when
// installed-and-deferred, dark otherwise, and reflected in View().
func TestUpgradeBadgeLifecycle(t *testing.T) {
	m := newModel(t.Context(), &adminCLI{bin: "/nonexistent"}, nil)
	m = m.withSelfUpgrade("/usr/local/bin/witself-admin", "0.0.96")
	m.loading = false
	if m.upgradeBadge() != "" {
		t.Fatal("badge must be dark with no update in flight")
	}

	// Check found a newer tag → installing.
	next, _ := m.Update(upgradeAvailableMsg{tag: "v0.0.97"})
	m2 := next.(model)
	if !strings.Contains(m2.upgradeBadge(), "installing") {
		t.Fatalf("installing badge: %q", m2.upgradeBadge())
	}
	if !strings.Contains(m2.View(), "installing") {
		t.Fatal("badge must render in View()")
	}

	// Channel lag → waiting light stays on.
	next, _ = m2.Update(upgradeAppliedMsg{tag: "v0.0.97", noop: true})
	m3 := next.(model)
	if !strings.Contains(m3.upgradeBadge(), "waiting for") {
		t.Fatalf("channel-wait badge: %q", m3.upgradeBadge())
	}

	// Installed while composing → green "restarts when idle".
	m4 := m2
	m4.mode = modeCompose
	next, _ = m4.Update(upgradeAppliedMsg{tag: "v0.0.97"})
	m5 := next.(model)
	if !strings.Contains(m5.upgradeBadge(), "restarts when idle") {
		t.Fatalf("ready badge: %q", m5.upgradeBadge())
	}

	// Failure → light goes dark (status line carries the error).
	next, _ = m2.Update(upgradeAppliedMsg{tag: "v0.0.97", err: errFake})
	m6 := next.(model)
	if m6.upgradeBadge() != "" {
		t.Fatalf("badge must clear on failure: %q", m6.upgradeBadge())
	}
}

// TestTabCyclesFocus pins the pane cycle: support → events → cells →
// support, and shift+tab the reverse.
func TestTabCyclesFocus(t *testing.T) {
	m := newModel(t.Context(), &adminCLI{bin: "/nonexistent"}, nil)
	if m.focus != paneSupport {
		t.Fatalf("default focus = %v, want support", m.focus)
	}
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m2 := next.(model)
	if m2.focus != paneEvents {
		t.Fatalf("tab: focus = %v, want events", m2.focus)
	}
	next, _ = m2.Update(tea.KeyMsg{Type: tea.KeyTab})
	m3 := next.(model)
	if m3.focus != paneCells {
		t.Fatalf("tab tab: focus = %v, want cells", m3.focus)
	}
	next, _ = m3.Update(tea.KeyMsg{Type: tea.KeyTab})
	m4 := next.(model)
	if m4.focus != paneSupport {
		t.Fatalf("full cycle: focus = %v, want support", m4.focus)
	}
	next, _ = m4.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	m5 := next.(model)
	if m5.focus != paneCells {
		t.Fatalf("shift+tab: focus = %v, want cells", m5.focus)
	}
}

// TestStateKeysRequireSupportFocus pins the wrong-pane guard: R aimed
// at the events tail must not mutate the support selection.
func TestStateKeysRequireSupportFocus(t *testing.T) {
	t0 := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	m := newModel(t.Context(), &adminCLI{bin: "/nonexistent"}, nil)
	m.loading = false
	m.tickets = []client.AdminTicket{mkTicket("tkt_1", "awaiting_admin", t0)}
	m.focus = paneEvents

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("R")})
	m2 := next.(model)
	if cmd != nil || m2.loading {
		t.Fatal("R with events focus must be a no-op")
	}
}

// TestEventScrollPause pins tail-pause semantics: k scrolls up and
// holds the view steady as new events arrive; j returns toward live.
func TestEventScrollPause(t *testing.T) {
	t0 := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	m := newModel(t.Context(), &adminCLI{bin: "/nonexistent"}, nil)
	m.loading = false
	m.focus = paneEvents
	for i := 0; i < 10; i++ {
		m.events = append(m.events, mkEvent(fmt.Sprintf("evt_%d", i), "account.activated", t0.Add(time.Duration(i)*time.Minute)))
	}

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	m2 := next.(model)
	if m2.eventScroll != 1 {
		t.Fatalf("k: eventScroll = %d, want 1", m2.eventScroll)
	}
	// New event while paused → offset grows so the view holds still.
	next, _ = m2.Update(watchEventMsg{event: mkEvent("evt_live", "recovery.requested", t0.Add(time.Hour))})
	m3 := next.(model)
	if m3.eventScroll != 2 {
		t.Fatalf("paused + new event: eventScroll = %d, want 2", m3.eventScroll)
	}
	// j steps back toward live.
	next, _ = m3.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	m4 := next.(model)
	if m4.eventScroll != 1 {
		t.Fatalf("j: eventScroll = %d, want 1", m4.eventScroll)
	}
	// Paused view is labeled.
	m4.width, m4.height = 100, 30
	if !strings.Contains(m4.viewList(), "paused") {
		t.Fatal("paused tail must be labeled in the events title")
	}
}

// TestAutoRefresh pins the background reload: re-arms itself, always
// reloads cells+tickets, reseeds events ONLY when the live stream is
// dead, and never touches m.loading (so it can't defer an upgrade).
func TestAutoRefresh(t *testing.T) {
	ch := make(chan client.AdminEvent)
	m := newModel(t.Context(), &adminCLI{bin: "/nonexistent"}, nil)
	m = m.withEventWatch(ch)
	m.loading = false

	next, cmd := m.Update(autoRefreshMsg{})
	m2 := next.(model)
	if cmd == nil {
		t.Fatal("auto-refresh must batch reload commands + re-arm")
	}
	if m2.loading {
		t.Fatal("auto-refresh must not set loading")
	}
}

// TestCellsPaneShowsVersion pins the running-software column: reported
// versions render (sanitized), unreachable cells say so loudly.
func TestCellsPaneShowsVersion(t *testing.T) {
	m := newModel(t.Context(), &adminCLI{bin: "/nonexistent"}, nil)
	m.loading = false
	m.width, m.height = 120, 40
	m.fleetCells = []client.AdminCell{
		{Name: "cell-a", Cloud: "aws", Region: "us-west-2", Accepting: true, AccountCount: 3, Version: "0.0.98"},
		{Name: "cell-b", Cloud: "aws", Region: "eu-west-1", Accepting: true, AccountCount: 1},
	}
	out := m.viewList()
	if !strings.Contains(out, "v0.0.98") {
		t.Fatal("reported version missing from cells pane")
	}
	if !strings.Contains(out, "unreachable") {
		t.Fatal("unreachable cell must be labeled")
	}
}

// TestEventDrillDown pins the events-pane drill-down: enter on the
// selection opens a detail view with pretty-printed metadata; esc
// returns to the dashboard. The detail is a COPY — the live tail
// moving underneath must not change what the operator is reading.
func TestEventDrillDown(t *testing.T) {
	t0 := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	m := newModel(t.Context(), &adminCLI{bin: "/nonexistent"}, nil)
	m.loading = false
	m.width, m.height = 100, 30
	m.focus = paneEvents
	m.events = []client.AdminEvent{
		mkEvent("evt_old", "account.activated", t0),
		mkEvent("evt_new", "recovery.requested", t0.Add(time.Minute)),
	}

	// k selects the older event, enter drills in.
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	m2 := next.(model)
	next, _ = m2.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m3 := next.(model)
	if m3.mode != modeEventDetail || m3.detailEvent == nil {
		t.Fatalf("enter must open event detail: mode=%v", m3.mode)
	}
	if m3.detailEvent.ID != "evt_old" {
		t.Fatalf("detail = %s, want the selected evt_old", m3.detailEvent.ID)
	}
	view := m3.View()
	for _, want := range []string{"evt_old", "account.activated", `"k": "v"`} {
		if !strings.Contains(view, want) {
			t.Errorf("detail view missing %q", want)
		}
	}
	// Live tail moves — detail must not.
	next, _ = m3.Update(watchEventMsg{event: mkEvent("evt_live", "token.revoked", t0.Add(time.Hour))})
	m4 := next.(model)
	if m4.detailEvent.ID != "evt_old" {
		t.Fatal("live tail mutated the open detail")
	}
	// esc returns.
	next, _ = m4.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m5 := next.(model)
	if m5.mode != modeList || m5.detailEvent != nil {
		t.Fatalf("esc must return to list: mode=%v", m5.mode)
	}
}

// TestCellDrillDown pins the cells-pane drill-down and its content.
func TestCellDrillDown(t *testing.T) {
	m := newModel(t.Context(), &adminCLI{bin: "/nonexistent"}, nil)
	m.loading = false
	m.width, m.height = 100, 30
	m.focus = paneCells
	m.fleetCells = []client.AdminCell{{
		Name: "aws-sandbox-usw2-dev", Cloud: "aws", Region: "us-west-2",
		Endpoint: "https://api.example", Accepting: true,
		HasProvisionToken: true, AccountCount: 3, Version: "0.0.98",
	}}

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m2 := next.(model)
	if m2.mode != modeCellDetail || m2.detailCell == nil {
		t.Fatalf("enter must open cell detail: mode=%v", m2.mode)
	}
	view := m2.View()
	for _, want := range []string{"aws-sandbox-usw2-dev", "https://api.example", "v0.0.98", "3"} {
		if !strings.Contains(view, want) {
			t.Errorf("cell detail missing %q", want)
		}
	}
	next, _ = m2.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m3 := next.(model)
	if m3.mode != modeList || m3.detailCell != nil {
		t.Fatalf("esc must return to list: mode=%v", m3.mode)
	}
}

// TestCellsPaneShowsArchived pins the archived-count display: shown in
// the pane stat line when non-zero, always present in cell detail.
func TestCellsPaneShowsArchived(t *testing.T) {
	m := newModel(t.Context(), &adminCLI{bin: "/nonexistent"}, nil)
	m.loading = false
	m.width, m.height = 120, 40
	m.fleetCells = []client.AdminCell{{
		Name: "cell-a", Cloud: "aws", Region: "us-west-2",
		Accepting: true, AccountCount: 12, ArchivedCount: 2, Version: "0.0.99",
	}}
	if !strings.Contains(m.viewList(), "2 archived") {
		t.Fatal("archived count missing from cells pane")
	}
	// Zero archived stays quiet in the pane…
	m.fleetCells[0].ArchivedCount = 0
	if strings.Contains(m.viewList(), "archived") {
		t.Fatal("zero archived should not clutter the pane")
	}
	// …but the drill-down always states it.
	cp := m.fleetCells[0]
	m.detailCell = &cp
	m.mode = modeCellDetail
	if !strings.Contains(m.View(), "archived") {
		t.Fatal("cell detail must always show the archived row")
	}
}

// TestTicketRecordInspector pins the 'i' drill-down: full ticket
// record (SLA timestamps included) from the list or from the thread,
// with esc returning to wherever it was opened from.
func TestTicketRecordInspector(t *testing.T) {
	t0 := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	m := newModel(t.Context(), &adminCLI{bin: "/nonexistent"}, nil)
	m.loading = false
	m.width, m.height = 100, 30
	tk := mkTicket("tkt_1", "resolved", t0)
	tk.FirstResponseAt = &t0
	m.tickets = []client.AdminTicket{tk}

	// From the list: i opens the record, esc returns to the list.
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("i")})
	m2 := next.(model)
	if m2.mode != modeTicketDetail || m2.detailTicket == nil {
		t.Fatalf("i from list: mode=%v", m2.mode)
	}
	view := m2.View()
	for _, want := range []string{"tkt_1", "first response", "aws-sandbox-usw2-dev", "subject tkt_1"} {
		if !strings.Contains(view, want) {
			t.Errorf("record view missing %q", want)
		}
	}
	next, _ = m2.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m3 := next.(model); m3.mode != modeList {
		t.Fatalf("esc from list-opened inspector: mode=%v", m3.mode)
	}

	// From the thread: i opens, esc returns to the THREAD.
	m.mode = modeDetail
	m.thread = &client.GetSupportTicketResult{Ticket: tk.SupportTicket}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("i")})
	m4 := next.(model)
	if m4.mode != modeTicketDetail {
		t.Fatalf("i from thread: mode=%v", m4.mode)
	}
	next, _ = m4.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m5 := next.(model); m5.mode != modeDetail {
		t.Fatalf("esc must return to the thread: mode=%v", m5.mode)
	}
}

// TestVersionInFooter pins the subdued version stamp on the dashboard
// footer — the operator's at-a-glance "am I current?" check.
func TestVersionInFooter(t *testing.T) {
	m := newModel(t.Context(), &adminCLI{bin: "/nonexistent"}, nil)
	m = m.withSelfUpgrade("/usr/local/bin/witself-admin", "0.0.101")
	m.loading = false
	m.width, m.height = 120, 40
	if !strings.Contains(m.viewList(), "witself-admin v0.0.101") {
		t.Fatal("running version missing from the dashboard footer")
	}
	// Source builds say dev rather than pretending.
	m2 := newModel(t.Context(), &adminCLI{bin: "/nonexistent"}, nil)
	m2.loading = false
	m2.width, m2.height = 120, 40
	if !strings.Contains(m2.viewList(), "witself-admin vdev") {
		t.Fatal("dev build must stamp vdev")
	}
}
