package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strings"
	"time"
)

const markerTitle = "witself fixture loader marker (synthetic rehearsal account)"

func markerMetadata(account string, e int) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"witself_fixture": "synthetic-rehearsal", "fixture_version": 1, "generator": 1, "entries_per_transcript": e, "account_id": account})
	return b
}
func markerValid(tr transcript, account string, e int) (bool, int, int) {
	var m struct {
		Kind      string `json:"witself_fixture"`
		Version   int    `json:"fixture_version"`
		Generator int    `json:"generator"`
		Entries   int    `json:"entries_per_transcript"`
		Account   string `json:"account_id"`
	}
	err := json.Unmarshal(tr.Metadata, &m)
	return err == nil && tr.ExternalID == "wfl1-marker" && tr.Title == markerTitle && m.Kind == "synthetic-rehearsal" && m.Version == 1 && m.Generator == 1 && m.Entries == e && m.Account == account, m.Generator, m.Entries
}
func requireMarker(ts []transcript, account string, e int) error {
	for _, tr := range ts {
		if tr.ExternalID == "wfl1-marker" {
			ok, g, n := markerValid(tr, account, e)
			if !ok {
				return failure(3, "refused: marker does not match this loader (generator %d, entries per transcript %d)", g, n)
			}
			return nil
		}
	}
	return failure(3, "refused: account is not marked synthetic; run mark on a new, empty account first")
}
func preflight(ctx context.Context, c config, d deps, o options) (session, error) {
	s := session{}
	cp, err := safeEndpoint(c.controlPlane)
	if err != nil {
		return s, err
	}
	c.controlPlane = cp
	s.operatorToken, err = readToken(c.operatorTokenFile)
	if err != nil {
		return s, err
	}
	if c.verb == "mark" || c.verb == "load" {
		s.agentToken, err = readToken(c.agentTokenFile)
		if err != nil {
			return s, err
		}
	}
	var directory struct {
		Cell struct {
			Cell     string `json:"cell"`
			Endpoint string `json:"endpoint"`
		} `json:"cell"`
		Archived json.RawMessage `json:"archived"`
	}
	err = requestJSON(ctx, d, "GET", cp, "/v1/directory/"+c.account, "", "directory lookup", 15*time.Second, nil, &directory, 200)
	if err != nil {
		return s, err
	}
	if len(directory.Archived) > 0 && string(directory.Archived) != "null" {
		return s, failure(3, "refused: account is archived; restore it before this verb")
	}
	s.cell = directory.Cell.Cell
	if c.verb != "close" && s.cell != c.expectCell {
		return s, failure(3, "refused: directory places the account on cell %s, not --expect-cell %s", responseWord(s.cell), safeText(c.expectCell))
	}
	s.endpoint, err = safeEndpoint(directory.Cell.Endpoint)
	if err != nil {
		return s, err
	}
	var ar struct {
		Account accountInfo `json:"account"`
	}
	err = requestJSON(ctx, d, "GET", s.endpoint, "/v1/account", s.operatorToken, "read account", 30*time.Second, nil, &ar, 200)
	if err != nil {
		return s, err
	}
	s.account = ar.Account
	if ar.Account.ID != c.account {
		return s, failure(3, "refused: operator token belongs to a different account than --account")
	}
	if ar.Account.Status != "active" {
		return s, failure(3, "refused: account status is %s, not active", responseWord(ar.Account.Status))
	}
	if c.verb == "mark" || c.verb == "load" {
		var sr struct {
			Identity struct {
				Account string `json:"account_id"`
				Realm   string `json:"realm_id"`
				Agent   string `json:"agent_id"`
			} `json:"identity"`
		}
		err = requestJSON(ctx, d, "GET", s.endpoint, selfPath, s.agentToken, "read self", 30*time.Second, nil, &sr, 200)
		if err != nil {
			return s, err
		}
		if sr.Identity.Account != c.account {
			return s, failure(3, "refused: agent token belongs to a different account than --account")
		}
		s.realm = sr.Identity.Realm
		s.agent = sr.Identity.Agent
		if s.realm == "" || s.agent == "" {
			return s, failure(5, "stopped: unexpected response from read self")
		}
	}
	if c.verb == "load" || c.verb == "measure" {
		hosts, er := ingressHosts(ctx, c, d)
		if er != nil {
			return s, er
		}
		u, _ := url.Parse(s.endpoint)
		match := false
		for _, host := range hosts {
			if host == u.Hostname() {
				match = true
			}
		}
		if !match {
			return s, failure(3, "refused: kube context does not serve the account's endpoint host")
		}
	}
	if c.verb != "mark" {
		token := s.operatorToken
		if c.verb == "load" {
			token = s.agentToken
		}
		s.transcripts, err = listTranscripts(ctx, c, d, s, token)
		if err != nil {
			return s, err
		}
		if err = requireMarker(s.transcripts, c.account, o.entriesPerTranscript); err != nil {
			return s, err
		}
	}
	return s, nil
}
func createTranscript(ctx context.Context, c config, d deps, s session, external, title string, metadata json.RawMessage) (transcript, error) {
	var out struct {
		Transcript transcript `json:"transcript"`
	}
	body := struct {
		ExternalID string          `json:"external_id"`
		Title      string          `json:"title"`
		Metadata   json.RawMessage `json:"metadata"`
	}{external, title, metadata}
	err := requestJSON(ctx, d, "POST", s.endpoint, "/v1/transcripts", s.agentToken, "create transcript", 30*time.Second, body, &out, 201)
	if err != nil {
		return transcript{}, err
	}
	tr := out.Transcript
	if tr.AccountID != c.account || tr.Title != title || tr.ExternalID != external || !transcriptIDPattern.MatchString(tr.ID) {
		return transcript{}, failure(5, "stopped: unexpected response from create transcript")
	}
	return tr, nil
}
func mark(ctx context.Context, c config, d deps, o options) error {
	s, err := preflight(ctx, c, d, o)
	if err != nil {
		return err
	}
	if !c.yes {
		return failure(3, "refused: mark needs --yes")
	}
	age := d.now().Sub(s.account.CreatedAt)
	if age > 168*time.Hour {
		return failure(3, "refused: account is older than 168h; mark accepts only a new account")
	}
	if age < 0 {
		return failure(3, "refused: account creation time is in the future")
	}
	ts, err := listTranscripts(ctx, c, d, s, s.operatorToken)
	if err != nil {
		return err
	}
	result := "already present"
	switch len(ts) {
	case 0:
		tr, er := createTranscript(ctx, c, d, s, "wfl1-marker", markerTitle, markerMetadata(c.account, o.entriesPerTranscript))
		if er != nil {
			return er
		}
		if ok, _, _ := markerValid(tr, c.account, o.entriesPerTranscript); !ok {
			return failure(5, "stopped: unexpected response from create transcript")
		}
		result = "created"
	case 1:
		if ok, _, _ := markerValid(ts[0], c.account, o.entriesPerTranscript); !ok {
			return failure(3, "refused: account already has 1 transcript(s); mark accepts only an empty account")
		}
	default:
		return failure(3, "refused: account already has %d transcript(s); mark accepts only an empty account", len(ts))
	}
	kv(d, "account:", c.account)
	kv(d, "cell:", responseWord(s.cell))
	kv(d, "account age:", fmt.Sprintf("%dh", int64(age.Hours())))
	kv(d, "transcripts before:", len(ts))
	kv(d, "marker:", result)
	_, _ = fmt.Fprintln(d.stdout, "ok")
	return nil
}
func closeAccount(ctx context.Context, c config, d deps, o options) error {
	s, err := preflight(ctx, c, d, o)
	if err != nil {
		return err
	}
	if !c.yes {
		return failure(3, "refused: close needs --yes")
	}
	cp, _ := safeEndpoint(c.controlPlane)
	err = requestJSON(ctx, d, "POST", cp, "/v1/accounts/"+c.account+":close", s.operatorToken, "close account", 60*time.Second, map[string]string{"reason": "synthetic rehearsal fixture"}, nil, 200)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(d.stdout, "account %s closed (synthetic rehearsal fixture)\nok\n", c.account)
	return nil
}
func quietCheck(now time.Time, during, measure bool) error {
	u := now.UTC()
	minute := u.Hour()*60 + u.Minute()
	sec := time.Duration(minute)*time.Minute + time.Duration(u.Second())*time.Second + time.Duration(u.Nanosecond())
	windows := []struct {
		start, end    int
		label, reason string
	}{{1410, 60, "23:30-01:00", "nightly account backups"}, {165, 240, "02:45-04:00", "nightly PostgreSQL dump"}}
	for _, w := range windows {
		inside := minute >= w.start && minute < w.end
		if w.start > w.end {
			inside = minute >= w.start || minute < w.end
		}
		end := fmt.Sprintf("%02d:%02d", w.end/60, w.end%60)
		if inside {
			if during {
				return failure(4, "stopped: quiet window %s UTC (%s); resume after %s UTC", w.label, w.reason, end)
			}
			return failure(4, "refused: inside the quiet window %s UTC (%s)", w.label, w.reason)
		}
		until := time.Duration(w.start)*time.Minute - sec
		if until < 0 {
			until += 24 * time.Hour
		}
		if measure && until < 20*time.Minute {
			if during {
				return failure(4, "stopped: a quiet window begins within 20 minutes; resume after %s UTC", end)
			}
			return failure(4, "refused: a quiet window begins within 20 minutes; measure after %s UTC", end)
		}
	}
	return nil
}
func maxMemory(r readings) float64 {
	var m float64
	for _, n := range r.nodes {
		m = math.Max(m, n.memory)
	}
	return m
}
func maxFilesystem(r readings) float64 {
	var m float64
	for _, n := range r.nodes {
		m = math.Max(m, n.fs)
	}
	return m
}

type loadRun struct {
	ctx                            context.Context
	c                              config
	d                              deps
	o                              options
	s                              session
	baseline, current              readings
	lastProbe, start, lastProgress time.Time
	estimator                      *archiveEstimator
	transcripts                    map[int]transcript
	present, sent                  int64
	measures                       int
	measured                       int64
	measuredEstimate               int64
	target, goal                   int64
	reason                         string
	writeStarted                   bool
}

func (r *loadRun) probe(during bool) error {
	v, err := probe(r.ctx, r.c, r.d, during)
	if err != nil {
		return err
	}
	r.current = v
	r.lastProbe = r.d.now()
	return evaluateReadings(v, r.baseline, r.c.limits, true)
}
func (r *loadRun) beforeBatch() error {
	if r.ctx.Err() != nil {
		return failure(130, "stopped: interrupted")
	}
	if err := quietCheck(r.d.now(), true, false); err != nil {
		return err
	}
	if r.d.now().Sub(r.lastProbe) >= 15*time.Second {
		return r.probe(true)
	}
	return nil
}
func (r *loadRun) retry(op string, fn func() error) error {
	for attempt := 0; attempt < 3; attempt++ {
		if r.ctx.Err() != nil {
			return failure(130, "stopped: interrupted")
		}
		err := fn()
		if err == nil {
			return nil
		}
		var ce *callError
		if !errors.As(err, &ce) || !ce.retryable() {
			return err
		}
		if attempt == 2 {
			return failure(5, "stopped: %s failed 3 times in a row (%s)", op, ce.label())
		}
		if r.d.sleep(r.ctx, time.Duration(2<<attempt)*time.Second) != nil {
			return failure(130, "stopped: interrupted")
		}
		if err = quietCheck(r.d.now(), true, false); err != nil {
			return err
		}
		if err = r.probe(r.writeStarted); err != nil {
			return err
		}
	}
	return nil
}
func (r *loadRun) resume() error {
	indices := []int{}
	for _, tr := range r.s.transcripts {
		if tr.ExternalID == "wfl1-marker" {
			continue
		}
		n, ok := parseTranscriptExternalID(tr.ExternalID)
		if !ok {
			return failure(3, "refused: unexpected transcripts in the loader agent's list")
		}
		if _, exists := r.transcripts[n]; exists {
			return failure(3, "refused: load transcripts are not contiguous; do not resume")
		}
		r.transcripts[n] = tr
		indices = append(indices, n)
	}
	sort.Ints(indices)
	for i, n := range indices {
		if n != i+1 {
			return failure(3, "refused: load transcripts are not contiguous; do not resume")
		}
		var out struct {
			Entries []struct {
				ExternalID string `json:"external_id"`
				AccountID  string `json:"account_id"`
			} `json:"entries"`
		}
		tr := r.transcripts[n]
		err := r.retry("read transcript tail", func() error {
			return requestJSON(r.ctx, r.d, "GET", r.s.endpoint, "/v1/transcripts/"+tr.ID+"?tail=true&limit=1", r.s.agentToken, "read transcript tail", 30*time.Second, nil, &out, 200)
		})
		if err != nil {
			return err
		}
		pos := -1
		if len(out.Entries) > 1 {
			return failure(5, "stopped: unexpected response from read transcript tail")
		}
		if len(out.Entries) == 1 {
			t, p, ok := parseEntryExternalID(out.Entries[0].ExternalID, r.o.entriesPerTranscript)
			if !ok || t != n || out.Entries[0].AccountID != r.c.account {
				return failure(3, "refused: load transcripts are not contiguous; do not resume")
			}
			pos = p
		}
		if i < len(indices)-1 && pos != r.o.entriesPerTranscript-1 {
			return failure(3, "refused: load transcripts are not contiguous; do not resume")
		}
		r.present = int64((n-1)*r.o.entriesPerTranscript + pos + 1)
	}
	_, _ = fmt.Fprintf(r.d.stdout, "recomputing the estimate for %d existing entries\n", r.present)
	for i := int64(0); i < r.present; i++ {
		if r.ctx.Err() != nil {
			return failure(130, "stopped: interrupted")
		}
		t, p := int(i)/r.o.entriesPerTranscript+1, int(i)%r.o.entriesPerTranscript
		e := generateEntry(r.c.account, t, p)
		if err := r.estimator.add(emulatedRow(e, r.c.account, r.s.realm, r.s.agent, r.transcripts[t].ID, t, p, r.o.entriesPerTranscript)); err != nil {
			return failure(5, "stopped: local estimate failed")
		}
		if (p+1)%100 == 0 || p == r.o.entriesPerTranscript-1 {
			if err := r.estimator.flush(); err != nil {
				return failure(5, "stopped: local estimate failed")
			}
		}
	}
	if err := r.estimator.flush(); err != nil {
		return failure(5, "stopped: local estimate failed")
	}
	return nil
}
func (r *loadRun) remainingLogical() (int64, error) {
	count := r.c.entries - r.present
	if r.c.entries > 0 {
		var n int64
		for i := int64(0); i < count; i++ {
			idx := r.present + i
			t, p := int(idx)/r.o.entriesPerTranscript+1, int(idx)%r.o.entriesPerTranscript
			n += logicalBytes(generateEntry(r.c.account, t, p))
		}
		return n, nil
	}
	if r.estimator.estimate() >= r.goal {
		return 0, nil
	}
	cal := newArchiveEstimator()
	defer func() { _ = cal.close() }()
	var n int64
	sampleCount := min(2000, 90*r.o.entriesPerTranscript-int(r.present))
	if sampleCount <= 0 {
		return 0, failure(7, "stopped: target needs more than %d entries", 90*r.o.entriesPerTranscript)
	}
	for i := 0; i < sampleCount; i++ {
		idx := int(r.present) + i
		t, p := idx/r.o.entriesPerTranscript+1, idx%r.o.entriesPerTranscript
		e := generateEntry(r.c.account, t, p)
		n += logicalBytes(e)
		if err := cal.add(emulatedRow(e, r.c.account, r.s.realm, r.s.agent, "trn_aaaaaaaaaaaaaaaa", t, p, r.o.entriesPerTranscript)); err != nil {
			return 0, failure(5, "stopped: local estimate failed")
		}
		if (i+1)%100 == 0 {
			if err := cal.flush(); err != nil {
				return 0, failure(5, "stopped: local estimate failed")
			}
		}
	}
	if err := cal.flush(); err != nil {
		return 0, failure(5, "stopped: local estimate failed")
	}
	return int64(math.Ceil(float64(r.goal-r.estimator.estimate()) * float64(n) / float64(cal.gzipBytes()))), nil
}
func (r *loadRun) plan() error {
	remaining, err := r.remainingLogical()
	if err != nil {
		return err
	}
	projected := projectedVolume(r.current, remaining)
	kv(r.d, "account:", r.c.account)
	kv(r.d, "cell:", responseWord(r.s.cell))
	kv(r.d, "kube context:", safeText(r.c.kubeContext))
	mode := "target"
	if r.c.entries > 0 {
		mode = "entries"
	}
	kv(r.d, "mode:", mode)
	kv(r.d, "entries present:", r.present)
	kv(r.d, "estimated archive bytes:", r.estimator.estimate())
	if r.c.entries > 0 {
		kv(r.d, "target archive bytes:", "none (entries mode)")
		kv(r.d, "goal archive bytes:", "none (entries mode)")
	} else {
		kv(r.d, "target archive bytes:", r.target)
		kv(r.d, "goal archive bytes:", r.goal)
	}
	measures := r.c.maxMeasures
	if r.c.noMeasure || r.c.entries > 0 {
		measures = 0
	}
	kv(r.d, "measures allowed:", measures)
	kv(r.d, "logical bytes to add:", fmt.Sprintf("%d (estimate)", remaining))
	kv(r.d, "postgres volume:", fmt.Sprintf("%.1f%% now, %.1f%% projected (stop at %.1f%%)", r.current.pvc, projected, r.c.limits.pvc))
	kv(r.d, "node memory:", fmt.Sprintf("%s (stop at %.1f%%, rise limit %.1f points)", nodePercentages(r.current, true), r.c.limits.memory, r.c.limits.rise))
	kv(r.d, "node filesystem:", fmt.Sprintf("%s (stop at %.1f%%)", nodePercentages(r.current, false), r.c.limits.fs))
	kv(r.d, "minimum duration:", fmt.Sprintf("%.1f minutes at %d bytes per second", float64(remaining)/float64(r.c.rate)/60, r.c.rate))
	if projected >= r.c.limits.pvc {
		return failure(4, "refused: postgres volume projected at %.1f%% after the load, above %.1f%%", projected, r.c.limits.pvc)
	}
	return nil
}
func (r *loadRun) batch() error {
	if err := r.beforeBatch(); err != nil {
		return err
	}
	t, p := int(r.present)/r.o.entriesPerTranscript+1, int(r.present)%r.o.entriesPerTranscript
	if t > 90 {
		return failure(7, "stopped: target needs more than %d entries", 90*r.o.entriesPerTranscript)
	}
	entries := []entry{}
	var logical int64
	batchBytes := len(`{"entries":[]}`)
	for pos := p; pos < r.o.entriesPerTranscript && len(entries) < 100; pos++ {
		if r.c.entries > 0 && r.present+int64(len(entries)) >= r.c.entries {
			break
		}
		e := generateEntry(r.c.account, t, pos)
		data, _ := json.Marshal(e)
		added := len(data)
		if len(entries) > 0 {
			added++
		}
		if batchBytes+added > 1048576 {
			break
		}
		batchBytes += added
		entries = append(entries, e)
		logical += logicalBytes(e)
	}
	need := time.Duration(float64(r.sent+logical)/float64(r.c.rate)*float64(time.Second)) - r.d.now().Sub(r.start)
	if need > 0 {
		if r.d.sleep(r.ctx, need) != nil {
			return failure(130, "stopped: interrupted")
		}
	}
	// Sleeping and transcript creation can cross a quiet window or probe cadence.
	if err := r.beforeBatch(); err != nil {
		return err
	}
	tr, exists := r.transcripts[t]
	if !exists {
		metadata, _ := json.Marshal(map[string]any{"witself_fixture": "synthetic-rehearsal", "fixture_version": 1, "transcript_index": t})
		r.writeStarted = true
		err := r.retry("create transcript", func() error {
			var er error
			tr, er = createTranscript(r.ctx, r.c, r.d, r.s, transcriptExternalID(t), fmt.Sprintf("synthetic rehearsal transcript %03d", t), metadata)
			return er
		})
		if err != nil {
			return err
		}
		r.transcripts[t] = tr
	}
	if err := r.beforeBatch(); err != nil {
		return err
	}
	var out struct {
		Entries []struct {
			ExternalID string `json:"external_id"`
			AccountID  string `json:"account_id"`
		} `json:"entries"`
	}
	r.writeStarted = true
	err := r.retry("append entries", func() error {
		wait := time.Duration(float64(r.sent+logical)/float64(r.c.rate)*float64(time.Second)) - r.d.now().Sub(r.start)
		if wait > 0 {
			if r.d.sleep(r.ctx, wait) != nil {
				return failure(130, "stopped: interrupted")
			}
		}
		if er := r.beforeBatch(); er != nil {
			return er
		}
		r.sent += logical
		return requestJSON(r.ctx, r.d, "POST", r.s.endpoint, "/v1/transcripts/"+tr.ID+"/entries:batch", r.s.agentToken, "append entries", 60*time.Second, struct {
			Entries []entry `json:"entries"`
		}{entries}, &out, 201)
	})
	if err != nil {
		return err
	}
	if len(out.Entries) != len(entries) {
		return failure(5, "stopped: unexpected response from append entries")
	}
	for i, e := range out.Entries {
		if e.ExternalID != entries[i].ExternalID || e.AccountID != r.c.account {
			return failure(5, "stopped: unexpected response from append entries")
		}
	}
	for i, e := range entries {
		if err = r.estimator.add(emulatedRow(e, r.c.account, r.s.realm, r.s.agent, tr.ID, t, p+i, r.o.entriesPerTranscript)); err != nil {
			return failure(5, "stopped: local estimate failed")
		}
	}
	if err = r.estimator.flush(); err != nil {
		return failure(5, "stopped: local estimate failed")
	}
	r.present += int64(len(entries))
	if r.d.now().Sub(r.lastProgress) >= 60*time.Second {
		_, _ = fmt.Fprintf(r.d.stdout, "progress: entries=%d transcripts=%d estimated_archive_bytes=%d logical_bytes_sent=%d elapsed_seconds=%d node_memory_max=%.1f%% postgres_volume=%.1f%% node_filesystem_max=%.1f%%\n", r.present, len(r.transcripts), r.estimator.estimate(), r.sent, int64(r.d.now().Sub(r.start).Seconds()), maxMemory(r.current), r.current.pvc, maxFilesystem(r.current))
		r.lastProgress = r.d.now()
	}
	return nil
}
func (r *loadRun) loop() error {
	if r.c.entries > 0 {
		if r.present >= r.c.entries {
			r.reason = "entries already present"
			return nil
		}
		for r.present < r.c.entries {
			if err := r.batch(); err != nil {
				return err
			}
		}
		r.reason = "entries reached"
		return nil
	}
	for {
		for r.estimator.estimate() < r.goal {
			if err := r.batch(); err != nil {
				return err
			}
		}
		if r.c.noMeasure {
			r.reason = "estimate reached (not measured)"
			return nil
		}
		if err := r.beforeBatch(); err != nil {
			return err
		}
		// Refresh all readings immediately before the export's disk precheck.
		if err := r.probe(true); err != nil {
			return err
		}
		if err := quietCheck(r.d.now(), true, true); err != nil {
			return err
		}
		if err := checkFilesystemSpace(r.current, r.estimator.estimate(), r.c.limits.fs, true); err != nil {
			return err
		}
		r.measures++
		e := r.estimator.estimate()
		m, err := measureArchive(r.ctx, r.c, r.d, r.s, r.current, e, r.measures, r.c.maxMeasures, true)
		if err != nil {
			return err
		}
		r.measured = m
		r.measuredEstimate = e
		err = r.probe(true)
		// A completed target remains successful unless PostgreSQL restarted.
		var stop *toolError
		if errors.As(err, &stop) && stop.code == 6 {
			return err
		}
		if m >= r.target {
			r.reason = "target reached (measured)"
			return nil
		}
		if err != nil {
			return err
		}
		if 2*m < e {
			return failure(7, "stopped: measured archive %d bytes is less than half of the local estimate %d; review before any top-up", m, e)
		}
		if r.measures == r.c.maxMeasures {
			return failure(7, "stopped: measured archive %d bytes is below the target %d after %d measure(s)", m, r.target, r.measures)
		}
		_, g, err := targetBytes(r.c.founder, r.o.floorBytes, r.o.ceilingBytes)
		if err != nil {
			return err
		}
		r.goal = e + ((g-m)*e+m-1)/m
	}
}
func (r *loadRun) summary(err error) {
	result := "done"
	reason := r.reason
	if err != nil {
		result = "stopped"
		reason = strings.TrimPrefix(err.Error(), "stopped: ")
	}
	kv(r.d, "result:", result)
	kv(r.d, "reason:", reason)
	kv(r.d, "entries:", r.present)
	kv(r.d, "transcripts:", len(r.transcripts))
	kv(r.d, "logical bytes sent:", r.sent)
	kv(r.d, "estimated archive bytes:", r.estimator.estimate())
	kv(r.d, "measures:", r.measures)
	if r.measured > 0 {
		kv(r.d, "measured archive bytes:", r.measured)
		kv(r.d, "measured / estimate:", fmt.Sprintf("%.3f", float64(r.measured)/float64(r.measuredEstimate)))
	} else {
		kv(r.d, "measured archive bytes:", "not measured")
		kv(r.d, "measured / estimate:", "not measured")
	}
	kv(r.d, "elapsed seconds:", int64(r.d.now().Sub(r.start).Seconds()))
	if r.lastProbe.IsZero() {
		kv(r.d, "postgres volume:", "not read")
		kv(r.d, "node memory max:", "not read")
	} else {
		kv(r.d, "postgres volume:", fmt.Sprintf("%.1f%% -> %.1f%%", r.baseline.pvc, r.current.pvc))
		kv(r.d, "node memory max:", fmt.Sprintf("%.1f%% -> %.1f%%", maxMemory(r.baseline), maxMemory(r.current)))
	}
}
func load(ctx context.Context, c config, d deps, o options) (err error) {
	r := loadRun{ctx: ctx, c: c, d: d, o: o, estimator: newArchiveEstimator(), transcripts: map[int]transcript{}, start: d.now(), lastProgress: d.now()}
	defer func() {
		if ctx.Err() != nil {
			err = failure(130, "stopped: interrupted")
		}
		r.summary(err)
		_ = r.estimator.close()
	}()
	if c.entries == 0 {
		r.target, r.goal, err = targetBytes(c.founder, o.floorBytes, o.ceilingBytes)
		if err != nil {
			return err
		}
	}
	r.s, err = preflight(ctx, c, d, o)
	if err != nil {
		return err
	}
	if err = quietCheck(d.now(), false, false); err != nil {
		return err
	}
	r.baseline, err = probe(ctx, c, d, false)
	if err != nil {
		return err
	}
	r.current = r.baseline
	r.lastProbe = d.now()
	if err = evaluateReadings(r.current, r.baseline, c.limits, false); err != nil {
		return err
	}
	if err = r.resume(); err != nil {
		return err
	}
	if err = r.plan(); err != nil {
		return err
	}
	if c.dryRun {
		r.reason = "dry run: nothing written"
		_, _ = fmt.Fprintln(d.stdout, "dry run: nothing written")
		return nil
	}
	r.start = d.now()
	r.lastProgress = r.start
	return r.loop()
}
func measure(ctx context.Context, c config, d deps, o options) error {
	s, err := preflight(ctx, c, d, o)
	if err != nil {
		return err
	}
	if err = quietCheck(d.now(), false, true); err != nil {
		return err
	}
	r, err := probe(ctx, c, d, false)
	if err != nil {
		return err
	}
	limits := thresholds{memory: math.Inf(1), rise: math.Inf(1), pvc: math.Inf(1), fs: c.limits.fs}
	if err = evaluateReadings(r, r, limits, false); err != nil {
		return err
	}
	// Operator-only measurement cannot regenerate agent rows. Reserve the largest
	// permitted archive goal rather than underestimating the temporary spool.
	estimate := o.ceilingBytes + (o.ceilingBytes*3+99)/100
	_, err = measureArchive(ctx, c, d, s, r, estimate, 1, 1, false)
	if err != nil {
		return err
	}
	// Once the archive is verified, only an observed restart changes the result.
	after, err := probe(ctx, c, d, true)
	if err != nil {
		return nil
	}
	err = evaluateReadings(after, r, limits, true)
	var stop *toolError
	if errors.As(err, &stop) && stop.code == 6 {
		return err
	}
	return nil
}
