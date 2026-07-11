package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// writeFixtureLog drops a per-cell op log under a temp WITSELF_HOME so
// the Logs tab has something to list without touching the real home.
func writeFixtureLog(t *testing.T, home, cell, verb, ts, body string) {
	t.Helper()
	dir := filepath.Join(home, "logs", "infra")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	name := cell + "-" + verb + "-" + ts + ".log"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestCellOpLogsListing pins the discovery: only this cell's logs,
// newest first, verb + timestamp parsed from the name.
func TestCellOpLogsListing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITSELF_HOME", home)
	writeFixtureLog(t, home, "aws-sandbox-use1-dev", "preview", "20260711T020000Z", "old preview\n")
	writeFixtureLog(t, home, "aws-sandbox-use1-dev", "up", "20260711T030000Z", "newer up\n")
	// A different cell's log must not leak in.
	writeFixtureLog(t, home, "gcp-sandbox-usw2-dev", "up", "20260711T040000Z", "other cell\n")

	logs := cellOpLogs("aws-sandbox-use1-dev")
	if len(logs) != 2 {
		t.Fatalf("expected 2 logs for the cell, got %d", len(logs))
	}
	if logs[0].verb != "up" || logs[1].verb != "preview" {
		t.Fatalf("logs must be newest-first: %+v", logs)
	}
}

// TestLogsTabRenders pins the tab body: the op list, the selected
// log's content, and the empty-state.
func TestLogsTabRenders(t *testing.T) {
	old := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI)
	defer lipgloss.SetColorProfile(old)

	home := t.TempDir()
	t.Setenv("WITSELF_HOME", home)
	writeFixtureLog(t, home, "aws-sandbox-use1-dev", "up", "20260711T030000Z",
		"# witself-infra up aws-sandbox-use1-dev\nprovisioning VPC\ncreating EKS cluster\nregistered with control plane\n")

	states := []cellState{{name: "aws-sandbox-use1-dev", entry: cellEntry{Cloud: strPtr("aws")}}}
	m := seedModel(states, 120, 30)
	m.now = func() time.Time { return time.Date(2026, 7, 11, 3, 30, 0, 0, time.UTC) }
	m.activeTab = tabLogs

	v := m.View()
	if !strings.Contains(v, "operations") {
		t.Error("Logs tab must show the operations header")
	}
	if !strings.Contains(v, "up") || !strings.Contains(v, "30m ago") {
		t.Errorf("Logs tab must list the op with its age: %s", stripANSIForTest(v))
	}
	if !strings.Contains(v, "creating EKS cluster") {
		t.Errorf("Logs tab must show the selected log's content: %s", stripANSIForTest(v))
	}

	// A cell with no logs shows the empty state.
	empty := seedModel([]cellState{{name: "gcp-sandbox-use1-dev", entry: cellEntry{Cloud: strPtr("gcp")}}}, 120, 30)
	empty.activeTab = tabLogs
	if !strings.Contains(empty.View(), "no operations logged") {
		t.Error("a cell with no logs must show the empty state")
	}
}

// TestLogsSelectionAndScroll pins the browse keys: `[` steps to an
// older log, `]` back, and PgUp scrolls the selected log while `end`
// returns to the tail.
func TestLogsSelectionAndScroll(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITSELF_HOME", home)
	writeFixtureLog(t, home, "aws-sandbox-use1-dev", "preview", "20260711T020000Z", "preview line\n")
	writeFixtureLog(t, home, "aws-sandbox-use1-dev", "up", "20260711T030000Z",
		strings.Repeat("log line\n", 200))

	states := []cellState{{name: "aws-sandbox-use1-dev", entry: cellEntry{Cloud: strPtr("aws")}}}
	m := seedModel(states, 120, 30)
	m.activeTab = tabLogs
	m.focus = focusContext

	// `[` selects the older (preview) log.
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("[")})
	m2 := next.(dashboardModel)
	if m2.logSel != 1 {
		t.Fatalf("`[` must step to the older log: logSel = %d", m2.logSel)
	}
	// `]` back to newest.
	next, _ = m2.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("]")})
	m3 := next.(dashboardModel)
	if m3.logSel != 0 {
		t.Fatalf("`]` must step back to the newest log: logSel = %d", m3.logSel)
	}
	// PgUp scrolls the (200-line) up log; End returns to the tail.
	next, _ = m3.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	m4 := next.(dashboardModel)
	if m4.opsScroll == 0 {
		t.Fatal("PgUp on the Logs tab must scroll the selected log")
	}
	next, _ = m4.Update(tea.KeyMsg{Type: tea.KeyEnd})
	if m5 := next.(dashboardModel); m5.opsScroll != 0 {
		t.Fatalf("End must return to the tail: opsScroll = %d", m5.opsScroll)
	}
}
