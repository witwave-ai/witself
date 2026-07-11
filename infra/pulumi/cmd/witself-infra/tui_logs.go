package main

// The Logs tab: per-cell operation logs. Every op tee's its output to
// $WITSELF_HOME/logs/infra/<cell>-<verb>-<UTC>.log (openOpLog), so a
// cell's history is just the files whose name starts with the cell.
// The tab lists them newest-first and shows the selected one's tail,
// scrollable. A live op on the cell streams from its in-memory ring
// buffer (fresher than the still-being-written file).

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// opLogFile is one persisted op log discovered on disk.
type opLogFile struct {
	path string
	verb string
	when time.Time
	size int64
}

// cellOpLogs lists a cell's op logs, newest first. The trailing hyphen
// in the prefix keeps a cell from matching a longer-named sibling
// (…-dev- won't match …-devtest-…).
func cellOpLogs(cell string) []opLogFile {
	dir := filepath.Join(witselfHomeDir(), "logs", "infra")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	prefix := cell + "-"
	var out []opLogFile
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".log") {
			continue
		}
		rem := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".log")
		verb, ts, ok := strings.Cut(rem, "-")
		if !ok {
			continue
		}
		when, _ := time.Parse("20060102T150405Z", ts)
		var size int64
		if info, err := e.Info(); err == nil {
			size = info.Size()
		}
		out = append(out, opLogFile{path: filepath.Join(dir, name), verb: verb, when: when, size: size})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].when.After(out[j].when) })
	return out
}

// readLogFile reads a persisted log into lines, stripping the trailing
// newline. Errors surface as a single visible line rather than an
// empty view.
func readLogFile(path string) []string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return []string{"(cannot read log: " + oneLine(err.Error()) + ")"}
	}
	return strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
}

// tailLines returns lines ending `offset` back from the newest,
// clipped to at most n rows. offset=0 is the bottom (freshest).
func tailLines(lines []string, offset, n int) []string {
	total := len(lines)
	if total == 0 || n <= 0 {
		return nil
	}
	if offset < 0 {
		offset = 0
	}
	end := total - offset
	if end <= 0 {
		return nil
	}
	start := end - n
	if start < 0 {
		start = 0
	}
	return lines[start:end]
}

// humanSize is a compact byte count for the log list: "5KB", "21KB".
func humanSize(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%dB", n)
	case n < 1024*1024:
		return fmt.Sprintf("%dKB", n/1024)
	default:
		return fmt.Sprintf("%dMB", n/(1024*1024))
	}
}

// selectedLogLines returns the full line set for the log the Logs tab
// is viewing: the in-memory ring buffer when a live/last op for this
// cell owns that file (fresher, no partial-read race), else the file.
func (m dashboardModel) selectedLogLines(cell string, logs []opLogFile, sel int) []string {
	if sel < 0 || sel >= len(logs) {
		return nil
	}
	path := logs[sel].path
	if m.op != nil && m.op.cell == cell && m.op.logPath == path {
		return m.op.snapshot(opLineCap)
	}
	if m.lastOp != nil && m.lastOp.cell == cell && m.lastOp.logPath == path {
		return m.lastOp.snapshot(opLineCap)
	}
	return readLogFile(path)
}

// renderLogsTab paints the per-cell log browser: a header, a short list
// of recent ops (the selected one marked), a divider, then the
// selected log's tail sized to fill the pane. `h` is the pane's content
// height so the tail fills exactly the rows the list leaves.
func (m dashboardModel) renderLogsTab(st cellState, w, h int) []string {
	logs := cellOpLogs(st.name)
	if len(logs) == 0 {
		return []string{"", styDim.Render("  no operations logged for this cell yet — run p / u / D")}
	}
	sel := m.clampLogSel(len(logs))

	var out []string
	out = append(out, fitLine("  "+styTitle.Render("operations")+styDim.Render("  [ ] pick · PgUp/Dn scroll"), w))

	const maxList = 4
	shown := min(len(logs), maxList)
	for i := 0; i < shown; i++ {
		lg := logs[i]
		marker := "    "
		style := styDim
		if i == sel {
			marker = "  " + styPlan.Render("▸") + " "
			style = styTitle
		}
		row := fmt.Sprintf("%-8s %5s ago  %6s", lg.verb, humanAge(m.now(), lg.when), humanSize(lg.size))
		out = append(out, fitLine(marker+style.Render(row), w))
	}
	if len(logs) > maxList {
		out = append(out, fitLine(styDim.Render(fmt.Sprintf("    … %d older", len(logs)-maxList)), w))
	}
	out = append(out, styDim.Render("  "+strings.Repeat("─", max(w-4, 0))))

	// Tail fills whatever rows the list left. Reserve what we've already
	// emitted plus a couple so the border never clips the last line.
	tailRows := max(h-len(out)-1, 3)
	lines := m.selectedLogLines(st.name, logs, sel)
	title := fmt.Sprintf("%s · %s", logs[sel].verb, logs[sel].when.Format("15:04:05"))
	scrolled := ""
	if m.opsScroll > 0 {
		scrolled = styDim.Render(fmt.Sprintf("  (scrolled %d · End to follow)", m.opsScroll))
	}
	out = append(out, fitLine("  "+styDim.Render(title)+scrolled, w))
	for _, l := range tailLines(lines, m.opsScroll, tailRows-1) {
		out = append(out, fitLine(l, w))
	}
	return out
}

// clampLogSel keeps the selection in range for the current log count.
func (m dashboardModel) clampLogSel(n int) int {
	if n == 0 {
		return 0
	}
	return max(0, min(m.logSel, n-1))
}
