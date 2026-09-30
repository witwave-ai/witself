package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/export"
)

const fakeAccount = "acc_aaaaaaaaaaaaaaaa"
const fakeAgentToken = "test-agent-token-not-real"
const fakeOperatorToken = "test-operator-token-not-real"
const fakeOtherToken = "test-other-token-not-real"
const fakeRealm = "rlm_aaaaaaaaaaaaaaaa"
const fakeAgent = "agt_aaaaaaaaaaaaaaaa"

type fakeRequest struct{ Method, Path string }
type fakeTranscript struct {
	transcript
	Entries []entry
	Updated int
}

type fakeCell struct {
	mu                                                                                          sync.Mutex
	exporting                                                                                   atomic.Bool
	server                                                                                      *httptest.Server
	account, status, cell, endpoint, agentAccount, operatorAccount                              string
	createdAt                                                                                   time.Time
	archived                                                                                    bool
	transcripts                                                                                 map[string]*fakeTranscript
	requests                                                                                    []fakeRequest
	appendStarts                                                                                []string
	fail                                                                                        map[string][]int
	redirect                                                                                    map[string]bool
	redirectHits                                                                                int
	dropBatch, dropExport, corruptChunk, wrongManifest, missingTrailer, shortBody, echoMismatch bool
	omitRows, omitExports, exports, updates                                                     int
	afterBatch                                                                                  func()
	closeBodyOK, closeBearerOK                                                                  bool
}

func newFakeCell(t *testing.T) *fakeCell {
	t.Helper()
	f := &fakeCell{account: fakeAccount, status: "active", cell: "fake-cell", createdAt: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC), agentAccount: fakeAccount, operatorAccount: fakeAccount, transcripts: map[string]*fakeTranscript{}, fail: map[string][]int{}, redirect: map[string]bool{}}
	f.server = httptest.NewServer(http.HandlerFunc(f.serveHTTP))
	f.endpoint = f.server.URL
	t.Cleanup(f.server.Close)
	return f
}

func fakeID(prefix string, n int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz234567"
	b := []byte(strings.Repeat("a", 16))
	for i := 15; n > 0 && i >= 0; i-- {
		b[i] = alphabet[n%32]
		n /= 32
	}
	return prefix + string(b)
}

func (f *fakeCell) addMarker(e int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	metadata, _ := json.Marshal(map[string]any{"witself_fixture": "synthetic-rehearsal", "fixture_version": 1, "generator": 1, "entries_per_transcript": e, "account_id": f.account, "agent_id": fakeAgent, "agent_name": "loader"})
	f.updates++
	f.transcripts["wfl1-marker"] = &fakeTranscript{transcript: transcript{ID: fakeID("trn_", len(f.transcripts)+1), AccountID: f.account, ExternalID: "wfl1-marker", Title: "witself fixture loader marker (synthetic rehearsal account)", Metadata: metadata}, Updated: f.updates}
}

func fakeOp(method, path string) string {
	switch {
	case strings.HasPrefix(path, "/v1/directory/"):
		return "directory lookup"
	case strings.HasSuffix(path, ":close"):
		return "close account"
	case path == "/v1/self":
		return "read self"
	case path == "/v1/account":
		return "read account"
	case path == "/v1/transcripts" && method == http.MethodGet:
		return "list transcripts"
	case path == "/v1/transcripts" && method == http.MethodPost:
		return "create transcript"
	case strings.HasSuffix(path, "/entries:batch"):
		return "append entries"
	case strings.HasPrefix(path, "/v1/transcripts/"):
		return "read transcript tail"
	case path == "/v1/export":
		return "export"
	default:
		return "unknown"
	}
}
func (f *fakeCell) count(op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.requests {
		if fakeOp(r.Method, r.Path) == op {
			n++
		}
	}
	return n
}
func (f *fakeCell) entryCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, tr := range f.transcripts {
		n += len(tr.Entries)
	}
	return n
}
func (f *fakeCell) resetRequests() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = nil
	f.appendStarts = nil
}
func fakeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func fakeDrop(w http.ResponseWriter) {
	h, ok := w.(http.Hijacker)
	if !ok {
		return
	}
	conn, _, err := h.Hijack()
	if err == nil {
		_ = conn.Close()
	}
}

func (f *fakeCell) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if fakeOp(r.Method, r.URL.Path) == "export" {
		if !f.exporting.CompareAndSwap(false, true) {
			f.mu.Lock()
			f.requests = append(f.requests, fakeRequest{r.Method, r.URL.Path})
			f.mu.Unlock()
			w.WriteHeader(409)
			return
		}
		defer f.exporting.Store(false)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, fakeRequest{r.Method, r.URL.Path})
	if r.URL.Path == "/redirect-target" {
		f.redirectHits++
		w.WriteHeader(200)
		return
	}
	op := fakeOp(r.Method, r.URL.Path)
	if statuses := f.fail[op]; len(statuses) > 0 {
		status := statuses[0]
		f.fail[op] = statuses[1:]
		if status != 0 {
			w.WriteHeader(status)
			return
		}
	}
	if f.redirect[op] {
		f.redirect[op] = false
		w.Header().Set("Location", f.server.URL+"/redirect-target")
		w.WriteHeader(302)
		return
	}
	if op == "directory lookup" {
		if f.archived {
			fakeJSON(w, 200, map[string]any{"archived": map[string]any{}})
		} else {
			fakeJSON(w, 200, map[string]any{"cell": map[string]any{"cell": f.cell, "endpoint": f.endpoint}})
		}
		return
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	isAgent, isOperator := token == fakeAgentToken, token == fakeOperatorToken
	account := f.account
	if isAgent {
		account = f.agentAccount
	}
	if isOperator {
		account = f.operatorAccount
	}
	if !isAgent && !isOperator && token != fakeOtherToken {
		w.WriteHeader(401)
		return
	}
	if token == fakeOtherToken {
		isAgent = true
		account = "acc_bbbbbbbbbbbbbbbb"
	}
	switch op {
	case "read account":
		if !isOperator {
			w.WriteHeader(403)
			return
		}
		fakeJSON(w, 200, map[string]any{"account": map[string]any{"id": account, "status": f.status, "created_at": f.createdAt, "email": "synthetic@example.invalid", "display_name": "Synthetic Display Name"}})
	case "read self":
		if !isAgent {
			w.WriteHeader(403)
			return
		}
		fakeJSON(w, 200, map[string]any{"identity": map[string]any{"account_id": account, "agent_id": fakeAgent, "realm_id": fakeRealm, "agent_name": "loader", "realm_name": "loader"}})
	case "list transcripts":
		if f.status != "active" {
			w.WriteHeader(403)
			return
		}
		all := make([]*fakeTranscript, 0, len(f.transcripts))
		for _, tr := range f.transcripts {
			all = append(all, tr)
		}
		sort.Slice(all, func(i, j int) bool {
			if all[i].Updated == all[j].Updated {
				return all[i].ID > all[j].ID
			}
			return all[i].Updated > all[j].Updated
		})
		out := make([]transcript, 0, len(all))
		for _, tr := range all {
			if len(out) == 100 {
				break
			}
			out = append(out, tr.transcript)
		}
		fakeJSON(w, 200, map[string]any{"transcripts": out})
	case "create transcript":
		if !isAgent || f.status != "active" {
			w.WriteHeader(403)
			return
		}
		var req transcript
		if json.NewDecoder(io.LimitReader(r.Body, 65537)).Decode(&req) != nil {
			w.WriteHeader(400)
			return
		}
		if existing := f.transcripts[req.ExternalID]; existing != nil {
			fakeJSON(w, 201, map[string]any{"transcript": existing.transcript})
			return
		}
		var metadata map[string]any
		if json.Unmarshal(req.Metadata, &metadata) != nil || metadata == nil {
			w.WriteHeader(400)
			return
		}
		metadata["agent_id"] = fakeAgent
		metadata["agent_name"] = "loader"
		req.Metadata, _ = json.Marshal(metadata)
		req.ID = fakeID("trn_", len(f.transcripts)+1)
		req.AccountID = account
		f.updates++
		f.transcripts[req.ExternalID] = &fakeTranscript{transcript: req, Updated: f.updates}
		fakeJSON(w, 201, map[string]any{"transcript": req})
	case "append entries":
		f.appendEntries(w, r, isAgent, account)
	case "read transcript tail":
		tr := f.transcriptForPath(r.URL.Path)
		if !isAgent || f.status != "active" {
			w.WriteHeader(403)
			return
		}
		if tr == nil {
			w.WriteHeader(404)
			return
		}
		limit := len(tr.Entries)
		if raw := r.URL.Query().Get("limit"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 1 || n > 500 {
				w.WriteHeader(400)
				return
			}
			limit = min(limit, n)
		}
		var entries []map[string]any
		for i := range limit {
			index := i
			if r.URL.Query().Get("tail") == "true" {
				index = len(tr.Entries) - i - 1
			}
			b, _ := json.Marshal(tr.Entries[index])
			v := map[string]any{}
			_ = json.Unmarshal(b, &v)
			v["account_id"] = tr.AccountID
			entries = append(entries, v)
		}
		fakeJSON(w, 200, map[string]any{"transcript": tr.transcript, "entries": entries})
	case "export":
		if !isOperator {
			w.WriteHeader(403)
			return
		}
		f.exports++
		if f.dropExport {
			f.dropExport = false
			fakeDrop(w)
			return
		}
		data, err := f.archive(r.Context())
		if err != nil {
			w.WriteHeader(500)
			return
		}
		w.Header().Set("X-Witself-Export-Purpose", "self")
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		if f.shortBody && len(data) > 10 {
			data = data[:len(data)-10]
		}
		w.WriteHeader(200)
		_, _ = w.Write(data)
	case "close account":
		if !isOperator {
			w.WriteHeader(403)
			return
		}
		data, _ := io.ReadAll(io.LimitReader(r.Body, 1024))
		f.closeBodyOK = string(data) == `{"reason":"synthetic rehearsal fixture"}`
		f.closeBearerOK = r.Header.Get("Authorization") == "Bearer "+fakeOperatorToken
		fakeJSON(w, 200, map[string]any{"ok": true})
	default:
		w.WriteHeader(404)
	}
}
func (f *fakeCell) transcriptForPath(path string) *fakeTranscript {
	parts := strings.Split(path, "/")
	if len(parts) < 4 {
		return nil
	}
	for _, tr := range f.transcripts {
		if tr.ID == parts[3] {
			return tr
		}
	}
	return nil
}
func validFakeEntry(e entry) bool {
	if e.Role != "user" && e.Role != "assistant" && e.Role != "system" && e.Role != "tool" {
		return false
	}
	if len(e.Body) > 65536 || len(e.Payload) > 16384 || len(e.ExternalID) > 512 || len(e.Model) > 256 {
		return false
	}
	if e.Body == "" && len(e.Payload) == 0 {
		return false
	}
	if len(e.Payload) > 0 {
		var object map[string]any
		if json.Unmarshal(e.Payload, &object) != nil || object == nil {
			return false
		}
	}
	return true
}
func (f *fakeCell) appendEntries(w http.ResponseWriter, r *http.Request, isAgent bool, account string) {
	if !isAgent || f.status != "active" {
		w.WriteHeader(403)
		return
	}
	tr := f.transcriptForPath(r.URL.Path)
	if tr == nil {
		w.WriteHeader(404)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, (8<<20)+1))
	if err != nil || len(body) > 8<<20 {
		fakeJSON(w, 400, map[string]any{"error": "invalid JSON body"})
		return
	}
	var req struct {
		Entries []entry `json:"entries"`
	}
	if json.Unmarshal(body, &req) != nil || len(req.Entries) == 0 || len(req.Entries) > 100 {
		w.WriteHeader(400)
		return
	}
	f.appendStarts = append(f.appendStarts, req.Entries[0].ExternalID)
	pending := make([]entry, 0, len(req.Entries))
	for _, e := range req.Entries {
		if !validFakeEntry(e) {
			w.WriteHeader(400)
			return
		}
		found := false
		for _, existing := range append(tr.Entries, pending...) {
			if existing.ExternalID == e.ExternalID {
				a, _ := json.Marshal(existing)
				b, _ := json.Marshal(e)
				if !bytes.Equal(a, b) {
					w.WriteHeader(409)
					return
				}
				found = true
				break
			}
		}
		if !found {
			pending = append(pending, e)
		}
	}
	tr.Entries = append(tr.Entries, pending...)
	f.updates++
	tr.Updated = f.updates
	if f.afterBatch != nil {
		f.afterBatch()
	}
	if f.dropBatch {
		f.dropBatch = false
		fakeDrop(w)
		return
	}
	out := make([]map[string]any, 0, len(req.Entries))
	for i, e := range req.Entries {
		b, _ := json.Marshal(e)
		v := map[string]any{}
		_ = json.Unmarshal(b, &v)
		v["account_id"] = account
		v["id"] = fakeID("ent_", i+1)
		out = append(out, v)
	}
	if f.echoMismatch && len(out) > 0 {
		out[0]["external_id"] = "unexpected"
	}
	fakeJSON(w, 201, map[string]any{"entries": out})
}

type fakeRows struct {
	rows [][]byte
	next int
}

func (*fakeRows) Table() string { return "transcript_entries" }
func (s *fakeRows) Next(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.next == len(s.rows) {
		return nil, nil
	}
	b := s.rows[s.next]
	s.next++
	return b, nil
}
func (f *fakeCell) archive(ctx context.Context) ([]byte, error) {
	keys := make([]string, 0, len(f.transcripts))
	for k := range f.transcripts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var rows [][]byte
	for _, key := range keys {
		tr := f.transcripts[key]
		var index int
		_, _ = fmt.Sscanf(key, "wfl1-t%03d", &index)
		for p, e := range tr.Entries {
			rows = append(rows, emulatedRow(e, f.account, fakeRealm, fakeAgent, tr.ID, index, p, 5000))
		}
	}
	if f.omitRows > 0 && (f.omitExports == 0 || f.exports <= f.omitExports) {
		keep := len(rows) - f.omitRows
		if keep < 0 {
			keep = 0
		}
		rows = rows[:keep]
	}
	account := f.account
	if f.wrongManifest {
		account = "acc_bbbbbbbbbbbbbbbb"
	}
	var buf bytes.Buffer
	err := export.Write(ctx, &buf, export.Manifest{SchemaVersion: 98, AccountID: account, Cell: f.cell, Status: f.status, Purpose: export.PurposeSelf, ExportedAt: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)}, []export.RowSource{&fakeRows{rows: rows}})
	if err != nil {
		return nil, err
	}
	data := buf.Bytes()
	if f.corruptChunk || f.missingTrailer {
		return rewriteFakeArchive(data, f.corruptChunk, f.missingTrailer)
	}
	return data, nil
}
func rewriteFakeArchive(data []byte, corrupt, omitTrailer bool) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer func() { _ = gz.Close() }()
	var out bytes.Buffer
	writer := gzip.NewWriter(&out)
	tw := tar.NewWriter(writer)
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		if h.Name == "checksums.json" && omitTrailer {
			continue
		}
		if strings.HasSuffix(h.Name, ".ndjson") && corrupt && len(b) > 0 {
			b[0] = '['
			corrupt = false
		}
		if err := tw.WriteHeader(h); err != nil {
			return nil, err
		}
		if _, err := tw.Write(b); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
