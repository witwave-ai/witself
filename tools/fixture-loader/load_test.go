package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu     sync.Mutex
	time   time.Time
	sleeps []time.Duration
}

func (c *fakeClock) now() time.Time    { c.mu.Lock(); defer c.mu.Unlock(); return c.time }
func (c *fakeClock) set(now time.Time) { c.mu.Lock(); defer c.mu.Unlock(); c.time = now }
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.time = c.time.Add(d)
}
func (c *fakeClock) sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sleeps = append(c.sleeps, d)
	c.time = c.time.Add(d)
	return nil
}

type harness struct {
	cell     *fakeCell
	d        deps
	o        options
	c        config
	clock    *fakeClock
	out, err bytes.Buffer
	cluster  *clusterFixture
}

func fixtureTokenFile(t *testing.T, value string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "credential")
	if os.WriteFile(path, []byte(value), 0o600) != nil || os.Chmod(path, mode) != nil {
		t.Fatal("cannot create fixture credential file")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != mode {
		t.Fatal("fixture credential mode differs from requested mode")
	}
	return path
}
func newHarness(t *testing.T, e int) *harness {
	t.Helper()
	h := &harness{cell: newFakeCell(t), o: options{e, 150000, 500000}, clock: &fakeClock{time: time.Date(2026, 9, 30, 14, 0, 0, 0, time.UTC)}, cluster: newClusterFixture()}
	h.c = config{account: fakeAccount, agentTokenFile: fixtureTokenFile(t, fakeAgentToken, 0o600), operatorTokenFile: fixtureTokenFile(t, fakeOperatorToken, 0o600), expectCell: "fake-cell", controlPlane: h.cell.server.URL, kubeContext: "fake-context", kubectl: "kubectl", rate: 1000000, maxMeasures: 2, limits: thresholds{90, 5, 70, 75}}
	h.d = deps{now: h.clock.now, sleep: h.clock.sleep, run: h.cluster.run, client: newHTTPClient(), stdout: &h.out, stderr: &h.err}
	return h
}
func markedHarness(t *testing.T, e int) *harness {
	t.Helper()
	h := newHarness(t, e)
	h.cell.addMarker(e)
	return h
}
func (h *harness) args(verb string, extra ...string) []string {
	args := []string{verb}
	if verb != "check" {
		args = append(args, "--account", h.c.account, "--operator-token-file", h.c.operatorTokenFile, "--control-plane", h.c.controlPlane)
	}
	if verb == "mark" || verb == "load" {
		args = append(args, "--agent-token-file", h.c.agentTokenFile)
	}
	if verb == "mark" || verb == "load" || verb == "measure" {
		args = append(args, "--expect-cell", h.c.expectCell)
	}
	if verb == "check" || verb == "load" || verb == "measure" {
		args = append(args, "--kube-context", h.c.kubeContext)
	}
	return append(args, extra...)
}
func (h *harness) run(args ...string) (int, string, string) {
	h.out.Reset()
	h.err.Reset()
	code := cliWithOptions(context.Background(), args, h.d, h.o)
	return code, h.out.String(), h.err.String()
}
func assertResult(t *testing.T, code, want int, stderr, message string) {
	t.Helper()
	if code != want {
		t.Fatalf("exit=%d, want %d", code, want)
	}
	if stderr != message {
		t.Fatal("terminal message differs from exact contract")
	}
}
func assertNoPost(t *testing.T, f *fakeCell) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.requests {
		if r.Method == "POST" {
			t.Fatal("refused operation made a write request")
		}
	}
}
func addLoadTranscript(f *fakeCell, t, e, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := transcriptExternalID(t)
	metadata, _ := json.Marshal(map[string]any{"witself_fixture": "synthetic-rehearsal", "fixture_version": 1, "transcript_index": t})
	f.updates++
	tr := &fakeTranscript{transcript: transcript{ID: fakeID("trn_", len(f.transcripts)+1), AccountID: f.account, ExternalID: id, Title: fmt.Sprintf("synthetic rehearsal transcript %03d", t), Metadata: metadata}, Updated: f.updates}
	for p := range n {
		tr.Entries = append(tr.Entries, generateEntry(f.account, t, p))
	}
	f.transcripts[id] = tr
	_ = e
}

func TestMarkAcceptsOnlyNewEmptyAccounts(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		setup      func(*harness)
		yes        bool
	}{
		{"old", "refused: account is older than 168h; mark accepts only a new account", func(h *harness) { h.cell.createdAt = h.clock.now().Add(-169 * time.Hour) }, true},
		{"occupied", "refused: account already has 1 transcript(s); mark accepts only an empty account", func(h *harness) { addLoadTranscript(h.cell, 1, 10, 0) }, true},
		{"agent", "refused: agent token belongs to a different account than --account", func(h *harness) { h.cell.agentAccount = "acc_bbbbbbbbbbbbbbbb" }, true},
		{"operator", "refused: operator token belongs to a different account than --account", func(h *harness) { h.cell.operatorAccount = "acc_bbbbbbbbbbbbbbbb" }, true},
		{"suspended", "refused: account status is suspended, not active", func(h *harness) { h.cell.status = "suspended" }, true},
		{"confirmation", "refused: mark needs --yes", func(*harness) {}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, 10)
			tc.setup(h)
			args := h.args("mark")
			if tc.yes {
				args = append(args, "--yes")
			}
			code, _, stderr := h.run(args...)
			assertResult(t, code, 3, stderr, tc.want+"\n")
			assertNoPost(t, h.cell)
		})
	}
	h := newHarness(t, 10)
	code, out, stderr := h.run(h.args("mark", "--yes")...)
	assertResult(t, code, 0, stderr, "")
	if !strings.Contains(out, fmt.Sprintf("%-26s%s\n", "marker:", "created")) || len(h.cell.transcripts) != 1 {
		t.Fatal("mark did not create exactly one marker")
	}
	marker := h.cell.transcripts["wfl1-marker"]
	if ok, _, _ := markerValid(marker.transcript, fakeAccount, 10); !ok {
		t.Fatal("created marker does not carry exact metadata")
	}
	h.cell.resetRequests()
	code, out, stderr = h.run(h.args("mark", "--yes")...)
	assertResult(t, code, 0, stderr, "")
	if !strings.Contains(out, "marker:                   already present") {
		t.Fatal("idempotent marker result differs")
	}
	assertNoPost(t, h.cell)
}

func TestVerbsRefuseUnmarkedAccount(t *testing.T) {
	for _, verb := range []string{"load", "measure", "close"} {
		t.Run(verb, func(t *testing.T) {
			h := newHarness(t, 10)
			args := h.args(verb)
			if verb == "load" {
				args = append(args, "--entries", "1")
			}
			code, out, stderr := h.run(args...)
			assertResult(t, code, 3, stderr, "refused: account is not marked synthetic; run mark on a new, empty account first\n")
			assertNoPost(t, h.cell)
			if verb == "load" {
				for _, label := range []string{"postgres volume:", "node memory max:"} {
					if !strings.Contains(out, fmt.Sprintf("%-26snot read\n", label)) {
						t.Fatal("preflight refusal reported a cluster reading that was never taken")
					}
				}
			}
		})
	}
	t.Run("mismatched-marker", func(t *testing.T) {
		h := markedHarness(t, 10)
		h.o.entriesPerTranscript = 11
		code, _, stderr := h.run(h.args("load", "--entries", "1")...)
		assertResult(t, code, 3, stderr, "refused: marker does not match this loader (generator 1, entries per transcript 10)\n")
		assertNoPost(t, h.cell)
	})
}

func TestLoadRefusesWrongPlacement(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		set        func(*harness)
	}{
		{"cell", "refused: directory places the account on cell another-cell, not --expect-cell fake-cell", func(h *harness) { h.cell.cell = "another-cell" }},
		{"scheme", "refused: endpoint must use https", func(h *harness) { h.cell.endpoint = "http://example.invalid" }},
		{"ingress", "refused: kube context does not serve the account's endpoint host", func(h *harness) { h.cluster.ingress = "different.invalid" }},
		{"archived", "refused: account is archived; restore it before this verb", func(h *harness) { h.cell.archived = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := markedHarness(t, 10)
			tc.set(h)
			code, _, stderr := h.run(h.args("load", "--entries", "1")...)
			assertResult(t, code, 3, stderr, tc.want+"\n")
			assertNoPost(t, h.cell)
		})
	}
}

func assertLayout(t *testing.T, f *fakeCell, e, total int) {
	t.Helper()
	if f.entryCount() != total {
		t.Fatal("stored entry count differs from requested count")
	}
	seen := map[string]bool{}
	for _, tr := range f.transcripts {
		if tr.ExternalID == "wfl1-marker" {
			continue
		}
		index, ok := parseTranscriptExternalID(tr.ExternalID)
		if !ok {
			t.Fatal("unexpected load transcript")
		}
		for p, v := range tr.Entries {
			ti, pi, valid := parseEntryExternalID(v.ExternalID, e)
			if !valid || ti != index || pi != p || seen[v.ExternalID] {
				t.Fatal("stored entries are not unique and contiguous")
			}
			seen[v.ExternalID] = true
		}
	}
}
func TestLoadEntriesModeIsIdempotent(t *testing.T) {
	h := markedHarness(t, 5000)
	args := h.args("load", "--entries", "234")
	code, _, stderr := h.run(args...)
	assertResult(t, code, 0, stderr, "")
	assertLayout(t, h.cell, 5000, 234)
	if len(h.cell.transcripts) != 2 {
		t.Fatal("entries did not fit one load transcript")
	}
	h.cell.resetRequests()
	code, out, stderr := h.run(args...)
	assertResult(t, code, 0, stderr, "")
	if !strings.Contains(out, "entries already present") {
		t.Fatal("resume did not report existing entries")
	}
	assertNoPost(t, h.cell)
}
func TestLoadSpansTranscripts(t *testing.T) {
	h := markedHarness(t, 10)
	code, _, stderr := h.run(h.args("load", "--entries", "25")...)
	assertResult(t, code, 0, stderr, "")
	assertLayout(t, h.cell, 10, 25)
	for i, want := range []int{10, 10, 5} {
		if len(h.cell.transcripts[transcriptExternalID(i+1)].Entries) != want {
			t.Fatal("transcript boundary has the wrong entry count")
		}
	}
}
func TestLoadResumesAfterInterrupt(t *testing.T) {
	h := markedHarness(t, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	batches := 0
	h.cell.afterBatch = func() {
		batches++
		if batches == 3 {
			cancel()
		}
	}
	code := cliWithOptions(ctx, h.args("load", "--entries", "45"), h.d, h.o)
	if code != 130 || h.err.String() != "stopped: interrupted\n" {
		t.Fatal("canceled load did not report interruption")
	}
	if h.cell.entryCount() != 30 {
		t.Fatal("interrupt did not preserve three committed batches")
	}
	h.cell.afterBatch = nil
	h.cell.resetRequests()
	code, _, stderr := h.run(h.args("load", "--entries", "45")...)
	assertResult(t, code, 0, stderr, "")
	assertLayout(t, h.cell, 10, 45)
	if h.cell.count("append entries") != 2 {
		t.Fatal("resume did not begin at the existing tail")
	}
}
func TestLoadResumesMidTranscript(t *testing.T) {
	h := markedHarness(t, 150)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.cell.afterBatch = cancel
	code := cliWithOptions(ctx, h.args("load", "--entries", "250"), h.d, h.o)
	assertResult(t, code, 130, h.err.String(), "stopped: interrupted\n")
	assertLayout(t, h.cell, 150, 100)
	if len(h.cell.transcripts) != 2 || len(h.cell.transcripts["wfl1-t001"].Entries) != 100 {
		t.Fatal("interrupt did not leave the first transcript partially filled")
	}
	h.cell.afterBatch = nil
	h.cell.resetRequests()
	code, _, stderr := h.run(h.args("load", "--entries", "250")...)
	assertResult(t, code, 0, stderr, "")
	assertLayout(t, h.cell, 150, 250)
	if len(h.cell.appendStarts) != 2 || h.cell.appendStarts[0] != "wfl1-t001-e0100" || h.cell.appendStarts[1] != "wfl1-t002-e0000" {
		t.Fatal("resume append requests did not start at the existing tail and next transcript")
	}
	if len(h.cell.transcripts) != 3 || len(h.cell.transcripts["wfl1-t001"].Entries) != 150 || len(h.cell.transcripts["wfl1-t002"].Entries) != 100 {
		t.Fatal("resume did not finish with the expected transcript layout")
	}
}
func TestLoadRetriesCommittedButLostResponse(t *testing.T) {
	h := markedHarness(t, 10)
	h.cell.dropBatch = true
	code, _, stderr := h.run(h.args("load", "--entries", "10")...)
	assertResult(t, code, 0, stderr, "")
	assertLayout(t, h.cell, 10, 10)
	if h.cell.count("append entries") != 2 {
		t.Fatal("lost committed response did not cause exactly one retry")
	}
}
func TestLoadStopRules(t *testing.T) {
	for _, status := range []int{400, 403, 409, 413, 429} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			h := markedHarness(t, 10)
			h.cell.fail["append entries"] = []int{status}
			code, _, stderr := h.run(h.args("load", "--entries", "10")...)
			assertResult(t, code, 5, stderr, fmt.Sprintf("stopped: append entries returned HTTP %d\n", status))
			if h.cell.count("append entries") != 1 {
				t.Fatal("client refusal was retried")
			}
		})
	}
	t.Run("redirect", func(t *testing.T) {
		h := markedHarness(t, 10)
		h.cell.redirect["append entries"] = true
		code, _, stderr := h.run(h.args("load", "--entries", "10")...)
		assertResult(t, code, 5, stderr, "stopped: append entries returned HTTP 302\n")
		if h.cell.redirectHits != 0 || h.cell.count("append entries") != 1 {
			t.Fatal("redirect was followed or retried")
		}
	})
	for _, failures := range []int{2, 3} {
		t.Run(fmt.Sprintf("transient-%d", failures), func(t *testing.T) {
			h := markedHarness(t, 10)
			for range failures {
				h.cell.fail["append entries"] = append(h.cell.fail["append entries"], 503)
			}
			code, _, stderr := h.run(h.args("load", "--entries", "10")...)
			if failures == 3 {
				assertResult(t, code, 5, stderr, "stopped: append entries failed 3 times in a row (HTTP 503)\n")
			} else {
				assertResult(t, code, 0, stderr, "")
			}
			if h.cell.count("append entries") != 3 {
				t.Fatal("transient retry count differs")
			}
			s := h.clock.sleeps
			if len(s) < 2 || s[len(s)-2] != 2*time.Second || s[len(s)-1] != 4*time.Second {
				t.Fatal("retry backoff differs from two then four seconds")
			}
		})
	}
	t.Run("echo", func(t *testing.T) {
		h := markedHarness(t, 10)
		h.cell.echoMismatch = true
		code, _, stderr := h.run(h.args("load", "--entries", "10")...)
		assertResult(t, code, 5, stderr, "stopped: unexpected response from append entries\n")
	})
}

func hookThirdProbe(h *harness, change func()) *int {
	probes := new(int)
	h.cluster.beforeCall = func(args []string) {
		if len(args) >= 4 && args[len(args)-3] == "nodes" {
			*probes++
			if *probes == 3 {
				change()
			}
		}
	}
	h.cell.afterBatch = func() { h.clock.advance(16 * time.Second) }
	return probes
}
func TestLoadStopsOnClusterThreshold(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		change     func(*harness)
	}{
		{"volume", "stopped: postgres volume 70.0% reached 70.0%", func(h *harness) {
			h.cluster.summaries["node-a"] = strings.Replace(h.cluster.summaries["node-a"], "7000000000", "3000000000", 1)
		}},
		{"rise", "stopped: node 1 memory rose 5.0 points above its baseline 50.0% (limit 5.0)", func(h *harness) { h.cluster.metrics = strings.Replace(h.cluster.metrics, "50Mi", "55Mi", 1) }},
		{"filesystem", "stopped: node 1 filesystem 75.0% reached 75.0%", func(h *harness) {
			h.cluster.summaries["node-a"] = strings.Replace(h.cluster.summaries["node-a"], "7500000000", "2500000000", 1)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := markedHarness(t, 10)
			probes := hookThirdProbe(h, func() { tc.change(h) })
			code, _, stderr := h.run(h.args("load", "--entries", "50")...)
			assertResult(t, code, 4, stderr, tc.want+"\n")
			if *probes != 3 || h.cell.count("append entries") != 2 {
				t.Fatal("writes continued after the third probe threshold")
			}
		})
	}
	t.Run("staircase-rise", func(t *testing.T) {
		h := markedHarness(t, 10)
		probes := 0
		h.cluster.beforeCall = func(args []string) {
			if len(args) >= 4 && args[len(args)-3] == "nodes" {
				probes++
				switch probes {
				case 2:
					h.cluster.metrics = strings.Replace(h.cluster.metrics, "50Mi", "53Mi", 1)
				case 3:
					h.cluster.metrics = strings.Replace(h.cluster.metrics, "53Mi", "56Mi", 1)
				}
			}
		}
		h.cell.afterBatch = func() { h.clock.advance(16 * time.Second) }
		code, _, stderr := h.run(h.args("load", "--entries", "50")...)
		assertResult(t, code, 4, stderr, "stopped: node 1 memory rose 6.0 points above its baseline 50.0% (limit 5.0)\n")
		if probes != 3 || h.cell.count("append entries") != 2 {
			t.Fatal("memory rise was not compared with the first probe before the third append")
		}
	})
}
func TestLoadStopsOnPostgresRestart(t *testing.T) {
	h := markedHarness(t, 10)
	hookThirdProbe(h, func() { h.cluster.postgres = "node-a\t1\ttrue\tOOMKilled" })
	code, _, stderr := h.run(h.args("load", "--entries", "50")...)
	assertResult(t, code, 6, stderr, "stopped: postgres restarted (restart count 0 -> 1, last reason OOMKilled); review before any resume\n")
	if h.cell.count("append entries") != 2 {
		t.Fatal("writes continued after a database restart")
	}
	t.Run("tail-retry-before-write", func(t *testing.T) {
		h := markedHarness(t, 10)
		addLoadTranscript(h.cell, 1, 10, 5)
		h.cell.fail["read transcript tail"] = []int{503}
		probes := 0
		h.cluster.beforeCall = func(args []string) {
			if len(args) >= 4 && args[len(args)-3] == "nodes" {
				probes++
				if probes == 2 {
					h.cluster.postgres = "node-a\t1\ttrue\tOOMKilled"
				}
			}
		}
		code, _, stderr := h.run(h.args("load", "--entries", "10")...)
		assertResult(t, code, 6, stderr, "stopped: postgres restarted (restart count 0 -> 1, last reason OOMKilled); review before any resume\n")
		assertNoPost(t, h.cell)
	})
}
func TestLoadPreflightProjection(t *testing.T) {
	h := markedHarness(t, 10)
	h.cluster.summaries["node-a"] = strings.Replace(h.cluster.summaries["node-a"], "7000000000", "3900000000", 1)
	code, _, stderr := h.run(h.args("load", "--entries", "1")...)
	assertResult(t, code, 4, stderr, "refused: postgres volume projected at 71.7% after the load, above 70.0%\n")
	assertNoPost(t, h.cell)
}

func TestQuietWindows(t *testing.T) {
	for _, tc := range []struct{ clock, want string }{
		{"23:29:59", ""}, {"01:00:00", ""}, {"02:44:59", ""}, {"04:00:00", ""},
		{"23:30:00", "refused: inside the quiet window 23:30-01:00 UTC (nightly account backups)"},
		{"00:59:59", "refused: inside the quiet window 23:30-01:00 UTC (nightly account backups)"},
		{"02:45:00", "refused: inside the quiet window 02:45-04:00 UTC (nightly PostgreSQL dump)"},
		{"03:59:59", "refused: inside the quiet window 02:45-04:00 UTC (nightly PostgreSQL dump)"},
	} {
		t.Run(tc.clock, func(t *testing.T) {
			now, err := time.Parse(time.RFC3339, "2026-09-30T"+tc.clock+"Z")
			if err != nil {
				t.Fatal("invalid test time")
			}
			got := quietCheck(now, false, false)
			if tc.want == "" {
				if got != nil {
					t.Fatal("quiet-window boundary incorrectly refused")
				}
			} else if got == nil || got.Error() != tc.want {
				t.Fatal("quiet-window message differs")
			}
		})
	}
	for _, tc := range []struct{ start, want string }{
		{"23:29:59", "stopped: quiet window 23:30-01:00 UTC (nightly account backups); resume after 01:00 UTC"},
		{"02:44:59", "stopped: quiet window 02:45-04:00 UTC (nightly PostgreSQL dump); resume after 04:00 UTC"},
	} {
		t.Run("cross-"+tc.start, func(t *testing.T) {
			h := markedHarness(t, 10)
			now, _ := time.Parse(time.RFC3339, "2026-09-30T"+tc.start+"Z")
			h.clock.set(now)
			h.cell.afterBatch = func() { h.clock.advance(2 * time.Second) }
			code, _, stderr := h.run(h.args("load", "--entries", "20")...)
			assertResult(t, code, 4, stderr, tc.want+"\n")
			if h.cell.count("append entries") != 1 {
				t.Fatal("quiet-window crossing did not stop writes")
			}
		})
	}
	for _, tc := range []struct {
		clock  string
		refuse bool
	}{{"02:25:01", true}, {"02:25:00", false}} {
		t.Run("measure-"+tc.clock, func(t *testing.T) {
			h := markedHarness(t, 10)
			now, _ := time.Parse(time.RFC3339, "2026-09-30T"+tc.clock+"Z")
			h.clock.set(now)
			code, _, stderr := h.run(h.args("measure")...)
			if tc.refuse {
				assertResult(t, code, 4, stderr, "refused: a quiet window begins within 20 minutes; measure after 04:00 UTC\n")
				if h.cell.count("export") != 0 {
					t.Fatal("measure started inside the look-ahead window")
				}
			} else {
				assertResult(t, code, 0, stderr, "")
			}
		})
	}
	t.Run("load-measure", func(t *testing.T) {
		h := markedHarness(t, 10)
		h.o.floorBytes = 1
		h.clock.set(time.Date(2026, 9, 30, 2, 25, 1, 0, time.UTC))
		code, _, stderr := h.run(h.args("load", "--founder-archive-bytes", "1")...)
		assertResult(t, code, 4, stderr, "stopped: a quiet window begins within 20 minutes; resume after 04:00 UTC\n")
		assertNoPost(t, h.cell)
	})
}

func TestWriteRateCap(t *testing.T) {
	h := markedHarness(t, 10)
	start := h.clock.now()
	var sent int64
	h.cell.afterBatch = func() {
		sent = 0
		for _, tr := range h.cell.transcripts {
			for _, e := range tr.Entries {
				sent += logicalBytes(e)
			}
		}
		seconds := h.clock.now().Sub(start).Seconds()
		if seconds <= 0 || float64(sent)/seconds > 100000*1.01 {
			t.Error("write rate exceeded the requested cap")
		}
	}
	code, _, stderr := h.run(h.args("load", "--entries", "50", "--max-write-bytes-per-second", "100000")...)
	assertResult(t, code, 0, stderr, "")
	if len(h.clock.sleeps) == 0 {
		t.Fatal("write cap did not request any pacing sleeps")
	}
}

func firstExportBeforeAppend(t *testing.T, f *fakeCell) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.requests {
		op := fakeOp(r.Method, r.Path)
		if op == "export" {
			return
		}
		if op == "append entries" {
			t.Fatal("target-mode resume wrote before its first measure")
		}
	}
	t.Fatal("target-mode resume did not measure")
}
func TestTargetModeMeasuresAndTopsUp(t *testing.T) {
	t.Run("top-up", func(t *testing.T) {
		h := markedHarness(t, 4)
		h.cell.omitRows = 20
		h.cell.omitExports = 1
		code, out, stderr := h.run(h.args("load", "--founder-archive-bytes", "1")...)
		assertResult(t, code, 0, stderr, "")
		if h.cell.exports != 2 || !strings.Contains(out, "target reached (measured)") {
			t.Fatal("scaled top-up did not finish with two measures")
		}
		assertLayout(t, h.cell, 4, h.cell.entryCount())
	})
	t.Run("resume-after-one", func(t *testing.T) {
		h := markedHarness(t, 4)
		h.cell.omitRows = 20
		h.cell.omitExports = 2
		code, _, stderr := h.run(h.args("load", "--founder-archive-bytes", "1", "--max-measures", "1")...)
		if code != 7 || !regexp.MustCompile(`^stopped: measured archive [0-9]+ bytes is below the target 150000 after 1 measure\(s\)\n$`).MatchString(stderr) {
			t.Fatal("last allowed short measure did not stop exactly")
		}
		h.cell.resetRequests()
		code, out, stderr := h.run(h.args("load", "--founder-archive-bytes", "1")...)
		assertResult(t, code, 0, stderr, "")
		firstExportBeforeAppend(t, h.cell)
		if h.cell.exports != 3 || !strings.Contains(out, "target reached (measured)") {
			t.Fatal("resume did not top up and measure again")
		}
	})
	t.Run("no-measure", func(t *testing.T) {
		h := markedHarness(t, 4)
		code, out, stderr := h.run(h.args("load", "--founder-archive-bytes", "1", "--no-measure")...)
		assertResult(t, code, 0, stderr, "")
		if h.cell.exports != 0 || !strings.Contains(out, "estimate reached (not measured)") {
			t.Fatal("unmeasured run overstated completion")
		}
		h.cell.resetRequests()
		code, _, stderr = h.run(h.args("load", "--founder-archive-bytes", "1")...)
		assertResult(t, code, 0, stderr, "")
		firstExportBeforeAppend(t, h.cell)
	})
	t.Run("half-estimate", func(t *testing.T) {
		h := markedHarness(t, 4)
		h.cell.omitRows = 1000
		code, _, stderr := h.run(h.args("load", "--founder-archive-bytes", "1")...)
		if code != 7 || !regexp.MustCompile(`^stopped: measured archive [0-9]+ bytes is less than half of the local estimate [0-9]+; review before any top-up\n$`).MatchString(stderr) {
			t.Fatal("extreme estimator disagreement did not stop exactly")
		}
		if h.cell.exports != 1 {
			t.Fatal("extreme estimator disagreement triggered another export")
		}
		seen := false
		for _, r := range h.cell.requests {
			if fakeOp(r.Method, r.Path) == "export" {
				seen = true
			}
			if seen && fakeOp(r.Method, r.Path) == "append entries" {
				t.Fatal("entries were appended after an unsafe measure")
			}
		}
	})
}

func TestTargetModePostMeasureProbe(t *testing.T) {
	for _, tc := range []struct {
		name, message string
		short, failK2 bool
		change        func(*harness)
		code          int
	}{
		{"target-memory-rise", "", false, false, func(h *harness) {
			h.cluster.metrics = strings.Replace(h.cluster.metrics, "60Mi", "65Mi", 1)
		}, 0},
		{"target-kubectl-failure", "", false, true, nil, 0},
		{"target-postgres-restart", "stopped: postgres restarted (restart count 0 -> 1, last reason OOMKilled); review before any resume\n", false, false, func(h *harness) {
			h.cluster.postgres = "node-a\t1\ttrue\tOOMKilled"
		}, 6},
		{"short-memory-rise", "stopped: node 2 memory rose 5.0 points above its baseline 60.0% (limit 5.0)\n", true, false, func(h *harness) {
			h.cluster.metrics = strings.Replace(h.cluster.metrics, "60Mi", "65Mi", 1)
		}, 4},
		{"short-kubectl-failure", "stopped: kubectl step K2 failed (exit 1)\n", true, true, nil, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := markedHarness(t, 4)
			if tc.short {
				h.cell.omitRows = 20
				h.cell.omitExports = 1
			} else {
				for index := 1; index <= 40; index++ {
					addLoadTranscript(h.cell, index, 4, 4)
				}
			}
			h.cluster.beforeCall = func(args []string) {
				if len(args) >= 4 && args[len(args)-3] == "nodes" && h.cell.count("export") > 0 && tc.change != nil {
					tc.change(h)
				}
			}
			h.d.run = func(ctx context.Context, name string, args []string) ([]byte, int, error) {
				out, code, err := h.cluster.run(ctx, name, args)
				if tc.failK2 && args[len(args)-1] == "/apis/metrics.k8s.io/v1beta1/nodes" && h.cell.count("export") > 0 {
					return nil, 1, nil
				}
				return out, code, err
			}
			code, out, stderr := h.run(h.args("load", "--founder-archive-bytes", "1")...)
			assertResult(t, code, tc.code, stderr, tc.message)
			if h.cell.count("export") != 1 {
				t.Fatal("post-measure result did not stop after exactly one export")
			}
			if !tc.short {
				measured := regexp.MustCompile(`(?m)^measured archive bytes:\s+([0-9]+)$`).FindStringSubmatch(out)
				if len(measured) != 2 {
					t.Fatal("target fixture omitted its measured archive size")
				}
				measuredBytes, err := strconv.ParseInt(measured[1], 10, 64)
				if err != nil || measuredBytes < h.o.floorBytes {
					t.Fatal("target fixture did not reach the target in its first export")
				}
			}
			if tc.code == 0 {
				if !strings.Contains(out, "target reached (measured)") {
					t.Fatal("routine post-measure probe failure overrode the measured target")
				}
			} else if strings.Contains(out, "target reached") {
				t.Fatal("stopped measure incorrectly reported target success")
			}
			if tc.change != nil && tc.code != 6 && !strings.Contains(out, fmt.Sprintf("%-26s60.0%% -> 65.0%%\n", "node memory max:")) {
				t.Fatal("summary did not retain the successful post-measure reading")
			}
			seenExport := false
			for _, req := range h.cell.requests {
				op := fakeOp(req.Method, req.Path)
				if op == "export" {
					seenExport = true
				} else if seenExport && op == "append entries" {
					t.Fatal("post-measure stop appended entries after the export")
				}
			}
		})
	}
}

func TestLoadSummaryRatioUsesEstimateAtMeasure(t *testing.T) {
	h := markedHarness(t, 4)
	code, _, stderr := h.run(h.args("load", "--founder-archive-bytes", "1", "--no-measure")...)
	assertResult(t, code, 0, stderr, "")
	h.cell.resetRequests()
	h.cell.omitRows = 20
	h.cell.omitExports = 1
	h.cell.afterBatch = func() { h.clock.advance(16 * time.Second) }
	postExportProbes := 0
	h.cluster.beforeCall = func(args []string) {
		if len(args) >= 4 && args[len(args)-3] == "nodes" && h.cell.count("export") > 0 {
			postExportProbes++
			if postExportProbes == 2 {
				h.cluster.metrics = strings.Replace(h.cluster.metrics, "50Mi", "55Mi", 1)
			}
		}
	}
	code, out, stderr := h.run(h.args("load", "--founder-archive-bytes", "1")...)
	assertResult(t, code, 4, stderr, "stopped: node 1 memory rose 5.0 points above its baseline 50.0% (limit 5.0)\n")
	firstExportBeforeAppend(t, h.cell)
	if h.cell.count("export") != 1 || h.cell.count("append entries") != 1 || postExportProbes != 2 {
		t.Fatal("ratio regression did not stop during the first top-up after one short measure")
	}
	estimates := regexp.MustCompile(`(?m)^estimated archive bytes:\s+([0-9]+)$`).FindAllStringSubmatch(out, -1)
	measured := regexp.MustCompile(`(?m)^measured archive bytes:\s+([0-9]+)$`).FindStringSubmatch(out)
	if len(estimates) != 2 || len(measured) != 2 {
		t.Fatal("output omitted the plan, measured archive or current estimate")
	}
	estimateAtMeasure, err := strconv.ParseInt(estimates[0][1], 10, 64)
	if err != nil {
		t.Fatal("invalid estimate at first measure")
	}
	finalEstimate, err := strconv.ParseInt(estimates[1][1], 10, 64)
	if err != nil || finalEstimate <= estimateAtMeasure {
		t.Fatal("top-up did not increase the final estimate")
	}
	measuredBytes, err := strconv.ParseInt(measured[1], 10, 64)
	if err != nil || measuredBytes >= h.o.floorBytes {
		t.Fatal("first measure was not below the target")
	}
	wantRatio := fmt.Sprintf("%.3f", float64(measuredBytes)/float64(estimateAtMeasure))
	if wantRatio == fmt.Sprintf("%.3f", float64(measuredBytes)/float64(finalEstimate)) {
		t.Fatal("top-up did not distinguish the two possible summary ratios")
	}
	if !strings.Contains(out, fmt.Sprintf("%-26s%s\n", "measured / estimate:", wantRatio)) {
		t.Fatal("summary ratio used the final estimate instead of the estimate at the measure")
	}
}

func TestCloseRequiresMarkerAndYes(t *testing.T) {
	h := markedHarness(t, 10)
	code, _, stderr := h.run(h.args("close")...)
	assertResult(t, code, 3, stderr, "refused: close needs --yes\n")
	assertNoPost(t, h.cell)
	code, _, stderr = h.run(h.args("close", "--yes")...)
	assertResult(t, code, 0, stderr, "")
	if h.cell.count("close account") != 1 || !h.cell.closeBodyOK || !h.cell.closeBearerOK {
		t.Fatal("close did not use the exact authorized request")
	}
	h.cell.resetRequests()
	h.cell.fail["close account"] = []int{500}
	code, _, stderr = h.run(h.args("close", "--yes")...)
	assertResult(t, code, 5, stderr, "stopped: close account returned HTTP 500\n")
	if h.cell.count("close account") != 1 {
		t.Fatal("close was retried")
	}
}
func TestResumeRejectsForeignLayout(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		set        func(*harness)
	}{
		{"foreign", "refused: unexpected transcripts in the loader agent's list", func(h *harness) {
			addLoadTranscript(h.cell, 1, 10, 0)
			h.cell.transcripts["wfl1-t001"].ExternalID = "foreign"
		}},
		{"gap", "refused: load transcripts are not contiguous; do not resume", func(h *harness) { addLoadTranscript(h.cell, 1, 10, 10); addLoadTranscript(h.cell, 3, 10, 1) }},
		{"short-prefix", "refused: load transcripts are not contiguous; do not resume", func(h *harness) { addLoadTranscript(h.cell, 1, 10, 9); addLoadTranscript(h.cell, 2, 10, 1) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := markedHarness(t, 10)
			tc.set(h)
			code, _, stderr := h.run(h.args("load", "--entries", "25")...)
			assertResult(t, code, 3, stderr, tc.want+"\n")
			assertNoPost(t, h.cell)
		})
	}
	t.Run("entry-cap", func(t *testing.T) {
		h := markedHarness(t, 1)
		code, _, stderr := h.run(h.args("load", "--entries", "91")...)
		assertResult(t, code, 7, stderr, "stopped: target needs more than 90 entries\n")
		if len(h.cell.transcripts) != 91 {
			t.Fatal("transcript cap exceeded the bounded list")
		}
	})
}

func valueFreeWorkflow(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t, 5000)
	var output strings.Builder
	for _, args := range [][]string{h.args("mark", "--yes"), h.args("load", "--entries", "50"), h.args("measure"), h.args("close", "--yes")} {
		code, out, stderr := h.run(args...)
		if code != 0 {
			t.Fatal("value-free workflow failed")
		}
		output.WriteString(out)
		output.WriteString(stderr)
	}
	for _, forbidden := range []string{fakeAgentToken, fakeOperatorToken, fakeOtherToken, "synthetic@example.invalid", "Synthetic Display Name", "ent_", "trn_", generateEntry(fakeAccount, 1, 0).Body[:24]} {
		if strings.Contains(output.String(), forbidden) {
			t.Fatal("output contains a forbidden value")
		}
	}
	if regexp.MustCompile(`[0-9a-fA-F]{64}`).MatchString(output.String()) {
		t.Fatal("output contains a digest")
	}
	return h
}
func TestOutputIsValueFree(t *testing.T) { valueFreeWorkflow(t) }
func assertRoutes(t *testing.T, f *fakeCell) {
	t.Helper()
	for _, r := range f.requests {
		allowed := false
		switch r.Method {
		case "GET":
			allowed = r.Path == "/v1/self" || r.Path == "/v1/account" || r.Path == "/v1/transcripts" || r.Path == "/v1/export" || r.Path == "/v1/directory/"+fakeAccount || regexp.MustCompile(`^/v1/transcripts/trn_[a-z2-7]{16}$`).MatchString(r.Path)
		case "POST":
			allowed = r.Path == "/v1/transcripts" || r.Path == "/v1/accounts/"+fakeAccount+":close" || regexp.MustCompile(`^/v1/transcripts/trn_[a-z2-7]{16}/entries:batch$`).MatchString(r.Path)
		}
		if !allowed {
			t.Fatal("HTTP request fell outside the closed route list")
		}
	}
}
func TestRouteAllowList(t *testing.T) {
	assertRoutes(t, valueFreeWorkflow(t).cell)
	h := markedHarness(t, 4)
	h.cell.omitRows = 20
	h.cell.omitExports = 1
	code, _, stderr := h.run(h.args("load", "--founder-archive-bytes", "1")...)
	assertResult(t, code, 0, stderr, "")
	assertRoutes(t, h.cell)
}

func TestDryRunAlwaysSummarizes(t *testing.T) {
	h := markedHarness(t, 10)
	code, out, stderr := h.run(h.args("load", "--entries", "1", "--dry-run")...)
	assertResult(t, code, 0, stderr, "")
	assertNoPost(t, h.cell)
	if h.cell.count("export") != 0 || !strings.Contains(out, "dry run: nothing written") || !strings.Contains(out, "result:                   done") {
		t.Fatal("dry run omitted its plan or summary or started an export")
	}
}
