package main

// Fleet health graphics: the one-line fleet strip on the dashboard,
// the per-cell account gauges, and the H drill-down view. Everything
// here draws with plain unicode (▁▂▃▅▇ sparklines, █░ bar gauges) —
// no chart dependency, no pixels.
//
// History is SESSION-LOCAL by design: one sample lands per completed
// refresh cycle (startup, manual g, the 60s auto-tick), so trends
// build up while the dashboard runs and reset on restart. There is no
// metrics store behind this yet — if we ever want history that
// survives restarts, the control plane grows a snapshot endpoint and
// these charts seed from it.

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/client"
)

// healthSample is one refresh-cycle observation of the fleet.
type healthSample struct {
	at       time.Time
	accounts int // fleet-wide active accounts
	open     int // non-terminal tickets
	events   int // cumulative live events seen at sample time
}

// sampleCap bounds the in-memory history (~4h at the 60s refresh).
const sampleCap = 240

func appendSample(samples []healthSample, s healthSample) []healthSample {
	samples = append(samples, s)
	if len(samples) > sampleCap {
		samples = samples[len(samples)-sampleCap:]
	}
	return samples
}

// sampleSeries projects one field out of the history for charting.
func sampleSeries(samples []healthSample, f func(healthSample) int) []int {
	out := make([]int, len(samples))
	for i, s := range samples {
		out[i] = f(s)
	}
	return out
}

// eventRates turns cumulative event counts into per-minute rates
// between consecutive samples — the series the event-rate charts draw.
func eventRates(samples []healthSample) []int {
	var out []int
	for i := 1; i < len(samples); i++ {
		mins := samples[i].at.Sub(samples[i-1].at).Minutes()
		if mins <= 0 {
			mins = 1
		}
		d := samples[i].events - samples[i-1].events
		if d < 0 {
			d = 0 // a reseed shrank the counter's basis — never chart negative
		}
		out = append(out, int(float64(d)/mins+0.5))
	}
	return out
}

var sparkRunes = []rune("▁▂▃▄▅▆▇█")

// sparkline draws vals as a unicode trend, newest last, keeping the
// most recent `width` points. Scaled to the window's own min..max so
// small fluctuations stay visible; flat data renders as a low line.
func sparkline(vals []int, width int) string {
	if width <= 0 || len(vals) == 0 {
		return ""
	}
	if len(vals) > width {
		vals = vals[len(vals)-width:]
	}
	lo, hi := vals[0], vals[0]
	for _, v := range vals {
		lo, hi = minInt(lo, v), maxInt(hi, v)
	}
	var b strings.Builder
	for _, v := range vals {
		idx := 0
		if hi > lo {
			idx = (v - lo) * (len(sparkRunes) - 1) / (hi - lo)
		}
		b.WriteRune(sparkRunes[idx])
	}
	return b.String()
}

// barGauge renders val against ceiling as a fixed-width block bar. A
// nonzero value always shows at least one block — "tiny but present"
// must be distinguishable from zero.
func barGauge(val, ceiling, width int) string {
	if width <= 0 {
		return ""
	}
	if ceiling < 1 {
		ceiling = 1
	}
	fill := val * width / ceiling
	if val > 0 && fill == 0 {
		fill = 1
	}
	fill = minInt(fill, width)
	return strings.Repeat("█", fill) + strings.Repeat("░", width-fill)
}

func totalAccounts(cells []client.AdminCell) int {
	n := 0
	for _, c := range cells {
		n += c.AccountCount
	}
	return n
}

// openTicketCounts counts non-terminal tickets, calling out the ones
// where the ball is in our court.
func openTicketCounts(tickets []client.AdminTicket) (open, urgent int) {
	for _, t := range tickets {
		if t.State == "resolved" || t.State == "closed" {
			continue
		}
		open++
		if t.State == "awaiting_admin" {
			urgent++
		}
	}
	return open, urgent
}

// ageBuckets counts open (non-terminal) tickets by how long they've
// been open: <1h, <24h, <3d, older. The last bucket is the one that
// means somebody is being left to rot.
func ageBuckets(tickets []client.AdminTicket, now time.Time) [4]int {
	var b [4]int
	for _, t := range tickets {
		if t.State == "resolved" || t.State == "closed" {
			continue
		}
		age := now.Sub(t.OpenedAt)
		switch {
		case age < time.Hour:
			b[0]++
		case age < 24*time.Hour:
			b[1]++
		case age < 72*time.Hour:
			b[2]++
		default:
			b[3]++
		}
	}
	return b
}

// versionSummary compresses fleet software versions into one phrase —
// skew (mixed versions mid-rollout) is the thing worth shouting about.
func versionSummary(cells []client.AdminCell) string {
	if len(cells) == 0 {
		return "no cells reported"
	}
	counts := map[string]int{}
	unreachable := 0
	for _, c := range cells {
		if c.Version == "" {
			unreachable++
			continue
		}
		counts["v"+oneLine(c.Version)]++
	}
	vs := make([]string, 0, len(counts))
	for v := range counts {
		vs = append(vs, v)
	}
	sort.Strings(vs)
	parts := make([]string, 0, len(vs))
	for _, v := range vs {
		parts = append(parts, fmt.Sprintf("%s ×%d", v, counts[v]))
	}
	out := strings.Join(parts, ", ")
	switch {
	case len(vs) == 0:
		out = "all cells unreachable ⚠"
	case len(vs) == 1 && unreachable == 0:
		out += " — no skew"
	case len(vs) > 1:
		out += " — version skew ⚠ (rollout in progress?)"
	}
	if unreachable > 0 && len(vs) > 0 {
		out += fmt.Sprintf(" · %d unreachable ⚠", unreachable)
	}
	return out
}

// healthStripLine is the one-line fleet glance across the top of the
// dashboard: cell health, account total with session trend, open
// tickets (urgent called out), live event rate.
func (m model) healthStripLine(width int) string {
	dot, cellCount := styDim.Render("●"), fmt.Sprintf("%d cells", len(m.fleetCells))
	if len(m.cells) > 0 {
		ok := 0
		for _, c := range m.cells {
			if c.Status == "ok" {
				ok++
			}
		}
		switch {
		case ok == len(m.cells):
			dot = styOK.Render("●")
		case ok > 0:
			dot = styWarn.Render("●")
		default:
			dot = styErr.Render("●")
		}
		cellCount = fmt.Sprintf("%d/%d cells", ok, len(m.cells))
	}
	accounts := totalAccounts(m.fleetCells)
	open, urgent := openTicketCounts(m.tickets)
	urgentPart := ""
	if urgent > 0 {
		urgentPart = " " + styErr.Render(fmt.Sprintf("(%d urgent)", urgent))
	}
	rates := eventRates(m.samples)
	rateNow := "–"
	if len(rates) > 0 {
		rateNow = fmt.Sprintf("%d", rates[len(rates)-1])
	}
	line := fmt.Sprintf("%s %s · accounts %s %s · open %s%s %s · events %s/m %s",
		dot, styTitle.Render(cellCount),
		styTitle.Render(fmt.Sprintf("%d", accounts)),
		styInfo.Render(sparkline(sampleSeries(m.samples, func(s healthSample) int { return s.accounts }), 8)),
		styTitle.Render(fmt.Sprintf("%d", open)), urgentPart,
		styInfo.Render(sparkline(sampleSeries(m.samples, func(s healthSample) int { return s.open }), 8)),
		styTitle.Render(rateNow), styInfo.Render(sparkline(rates, 8)),
	)
	return fitLine(line, width)
}

// viewHealth is the H drill-down: the whole fleet's shape on one
// screen — load distribution, ticket aging, event rate, growth, and
// version skew.
func (m model) viewHealth() string {
	w := m.width
	if w < 60 {
		w = 100
	}
	contentW := w - 4
	now := m.now()

	var lines []string

	// ── load distribution ──
	lines = append(lines, styTitle.Render("accounts by cell"))
	fleetMax, nameW := 0, 10
	for _, c := range m.fleetCells {
		fleetMax = maxInt(fleetMax, c.AccountCount)
		nameW = maxInt(nameW, len(c.Name))
	}
	nameW = minInt(nameW, 28)
	barW := minInt(maxInt(contentW-nameW-16, 8), 30)
	if len(m.fleetCells) == 0 {
		lines = append(lines, styDim.Render("  (no cells reported yet)"))
	}
	for _, c := range m.fleetCells {
		row := fmt.Sprintf("  %-*s %s %d", nameW,
			truncate(oneLine(c.Name), nameW),
			barGauge(c.AccountCount, fleetMax, barW), c.AccountCount)
		if c.ArchivedCount > 0 {
			row += styWarn.Render(fmt.Sprintf(" · %d archived", c.ArchivedCount))
		}
		lines = append(lines, fitLine(row, contentW))
	}

	// ── ticket aging ──
	lines = append(lines, "", styTitle.Render("open ticket age"))
	buckets := ageBuckets(m.tickets, now)
	bucketMax := 1
	for _, n := range buckets {
		bucketMax = maxInt(bucketMax, n)
	}
	labels := [4]string{"<1h ", "<24h", "<3d ", ">3d "}
	for i, n := range buckets {
		row := fmt.Sprintf("  %s %s %d", labels[i], barGauge(n, bucketMax, 20), n)
		if i == 3 && n > 0 {
			row += " " + styErr.Render("⚠ aging")
		}
		lines = append(lines, fitLine(row, contentW))
	}

	// ── event rate ──
	lines = append(lines, "", styTitle.Render("event rate (per minute, this session)"))
	rates := eventRates(m.samples)
	if len(rates) == 0 {
		lines = append(lines, styDim.Render("  collecting — needs one refresh cycle"))
	} else {
		peak := 0
		for _, r := range rates {
			peak = maxInt(peak, r)
		}
		lines = append(lines, fitLine(fmt.Sprintf("  %s now %d/m · peak %d/m",
			styInfo.Render(sparkline(rates, minInt(contentW-26, 60))),
			rates[len(rates)-1], peak), contentW))
	}

	// ── growth ──
	lines = append(lines, "", styTitle.Render("accounts (this session)"))
	accs := sampleSeries(m.samples, func(s healthSample) int { return s.accounts })
	if len(accs) == 0 {
		lines = append(lines, styDim.Render("  collecting — needs one refresh cycle"))
	} else {
		lines = append(lines, fitLine(fmt.Sprintf("  %s %d now · %+d since open",
			styInfo.Render(sparkline(accs, minInt(contentW-26, 60))),
			accs[len(accs)-1], accs[len(accs)-1]-accs[0]), contentW))
	}

	// ── version skew ──
	lines = append(lines, "", styTitle.Render("software"))
	lines = append(lines, fitLine("  "+versionSummary(m.fleetCells), contentW))

	// Height budget: 6 = 2 border + title inside the box, hint +
	// status below it. Health is a full-screen page by design, so it
	// budgets against the whole terminal.
	h := m.height
	if h < 15 {
		h = 30
	}
	contentH := maxInt(h-6, 6)
	lines = clipLines(lines, contentH, contentW)
	return paneBox("fleet health", lines, contentW, maxInt(len(lines), minInt(10, contentH)), true)
}
