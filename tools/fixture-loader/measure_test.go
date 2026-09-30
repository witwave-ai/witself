package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func measureFixture(t *testing.T) (*fakeCell, config, deps, session, readings, *bytes.Buffer) {
	t.Helper()
	f := newFakeCell(t)
	f.addMarker(5000)
	tr := &fakeTranscript{transcript: transcript{ID: fakeID("trn_", 2), AccountID: fakeAccount, ExternalID: "wfl1-t001", Title: "synthetic rehearsal transcript 001"}}
	for p := 0; p < 6; p++ {
		tr.Entries = append(tr.Entries, generateEntry(fakeAccount, 1, p))
	}
	f.transcripts[tr.ExternalID] = tr
	out := new(bytes.Buffer)
	d := deps{now: func() time.Time { return time.Date(2026, 9, 30, 14, 0, 0, 0, time.UTC) }, client: newHTTPClient(), stdout: out, stderr: io.Discard, sleep: func(context.Context, time.Duration) error {
		t.Fatal("measure unexpectedly requested a sleep")
		return nil
	}}
	c := config{account: fakeAccount, limits: thresholds{90, 5, 70, 75}}
	s := session{endpoint: f.server.URL, cell: "fake-cell", operatorToken: fakeOperatorToken, realm: fakeRealm, agent: fakeAgent}
	r := readings{nodes: []nodeReading{{name: "node-one", fs: 20, fsUsed: 2 << 30, fsCapacity: 10 << 30}}, ready: true}
	return f, c, d, s, r, out
}

func TestMeasureReportsArchive(t *testing.T) {
	f, c, d, s, r, out := measureFixture(t)
	data, err := f.archive(context.Background())
	if err != nil {
		t.Fatal("building expected archive failed")
	}
	var ndjson int
	tr := f.transcripts["wfl1-t001"]
	for p, e := range tr.Entries {
		ndjson += len(emulatedRow(e, f.account, fakeRealm, fakeAgent, tr.ID, 1, p, 5000)) + 1
	}
	n, err := measureArchive(context.Background(), c, d, s, r, 100000, 1, 1, false)
	if err != nil || n != int64(len(data)) {
		t.Fatal("measure did not report the complete archive")
	}
	var expected strings.Builder
	for _, line := range []struct {
		key   string
		value any
	}{
		{"measure:", "1 of 1"}, {"archive bytes:", len(data)}, {"archive purpose:", "self"}, {"manifest cell:", "fake-cell"}, {"manifest schema:", 98}, {"transcript_entries rows:", 6}, {"ndjson bytes:", ndjson}, {"compression ratio:", fmt.Sprintf("%.2f", float64(ndjson)/float64(len(data)))}, {"checksums:", "ok"}, {"export seconds:", 0}, {"download seconds:", 0},
	} {
		_, _ = fmt.Fprintf(&expected, "%-26s%v\n", line.key, line.value)
	}
	if out.String() != expected.String() {
		t.Fatal("measure block differs from the required exact lines")
	}
	if f.count("export") != 1 {
		t.Fatal("measure did not make exactly one export")
	}
}

func TestMeasureIgnoresLoadMemoryAndVolumeLimits(t *testing.T) {
	h := markedHarness(t, 10)
	addLoadTranscript(h.cell, 1, 10, 6)
	h.cluster.metrics = strings.Replace(h.cluster.metrics, "50Mi", "91Mi", 1)
	h.cluster.summaries["node-a"] = strings.Replace(h.cluster.summaries["node-a"], "7000000000", "2900000000", 1)
	code, _, stderr := h.run(h.args("measure")...)
	assertResult(t, code, 0, stderr, "")
	if h.cell.count("export") != 1 {
		t.Fatal("standalone measure did not export exactly once above load-only limits")
	}
}

func TestMeasureStopsOnPostExportRestart(t *testing.T) {
	h := markedHarness(t, 10)
	addLoadTranscript(h.cell, 1, 10, 6)
	probes := 0
	h.cluster.beforeCall = func(args []string) {
		if len(args) >= 4 && args[len(args)-3] == "nodes" {
			probes++
			if probes == 2 {
				h.cluster.postgres = "node-a\t1\ttrue\tOOMKilled"
			}
		}
	}
	code, _, stderr := h.run(h.args("measure")...)
	assertResult(t, code, 6, stderr, "stopped: postgres restarted (restart count 0 -> 1, last reason OOMKilled); review before any resume\n")
	if probes != 2 || h.cell.count("export") != 1 {
		t.Fatal("standalone measure did not detect the restart after exactly one export")
	}
}

func TestMeasureIgnoresPostExportMemoryRise(t *testing.T) {
	h := markedHarness(t, 10)
	addLoadTranscript(h.cell, 1, 10, 6)
	probes := 0
	h.cluster.beforeCall = func(args []string) {
		if len(args) >= 4 && args[len(args)-3] == "nodes" {
			probes++
			if probes == 2 {
				h.cluster.metrics = strings.Replace(h.cluster.metrics, "50Mi", "56Mi", 1)
			}
		}
	}
	code, _, stderr := h.run(h.args("measure")...)
	assertResult(t, code, 0, stderr, "")
	if probes != 2 || h.cell.count("export") != 1 {
		t.Fatal("standalone measure did not finish after the post-export memory rise")
	}
}

func TestMeasureDetectsDamage(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		set          func(*fakeCell)
	}{
		{"chunk", "checksums", func(f *fakeCell) { f.corruptChunk = true }},
		{"length", "content-length", func(f *fakeCell) { f.shortBody = true }},
		{"manifest", "manifest", func(f *fakeCell) { f.wrongManifest = true }},
		{"trailer", "trailer", func(f *fakeCell) { f.missingTrailer = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c, d, s, r, _ := measureFixture(t)
			tc.set(f)
			_, err := measureArchive(context.Background(), c, d, s, r, 100000, 1, 1, false)
			var stop *toolError
			if !errors.As(err, &stop) || stop.code != 5 || stop.msg != "stopped: archive check failed ("+tc.reason+")" {
				t.Fatal("archive damage did not produce the exact required refusal")
			}
			if f.count("export") != 1 {
				t.Fatal("damaged export was retried")
			}
		})
	}
}

func TestMeasureIsNeverRetried(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		set        func(*fakeCell)
	}{
		{"http", "stopped: export returned HTTP 503", func(f *fakeCell) { f.fail["export"] = []int{503} }},
		{"connection", "stopped: export failed (connection)", func(f *fakeCell) { f.dropExport = true }},
		{"redirect", "stopped: export returned HTTP 302", func(f *fakeCell) { f.redirect["export"] = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c, d, s, r, _ := measureFixture(t)
			tc.set(f)
			_, err := measureArchive(context.Background(), c, d, s, r, 100000, 1, 1, false)
			var stop *toolError
			if !errors.As(err, &stop) || stop.code != 5 || stop.msg != tc.want {
				t.Fatal("export failure did not retain the required class")
			}
			if f.count("export") != 1 || f.redirectHits != 0 {
				t.Fatal("export was retried or followed a redirect")
			}
		})
	}
}

func TestHTTPFailureClassificationIsValueFree(t *testing.T) {
	for _, tc := range []struct {
		name, class string
		err         error
	}{
		{"deadline", "timeout", fmt.Errorf("wrapped: %w", context.DeadlineExceeded)},
		{"network timeout", "timeout", &net.DNSError{IsTimeout: true}},
		{"certificate", "tls", &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}},
		{"hostname", "tls", x509.HostnameError{}},
		{"certificate validity", "tls", x509.CertificateInvalidError{}},
		{"handshake record", "tls", tls.RecordHeaderError{}},
		{"handshake alert", "tls", tls.AlertError(40)},
		{"connection", "connection", errors.New(fakeOtherToken)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := httpFailure("export", 0, tc.err)
			var stop *toolError
			if !errors.As(err, &stop) || stop.code != 5 || stop.msg != "stopped: export failed ("+tc.class+")" {
				t.Fatal("transport error was not classified without its private text")
			}
		})
	}
	for _, status := range []int{302, 400, 403, 409, 413, 429, 503} {
		err := httpFailure("export", status, errors.New(fakeOtherToken))
		var stop *toolError
		if !errors.As(err, &stop) || stop.code != 5 || stop.msg != fmt.Sprintf("stopped: export returned HTTP %d", status) {
			t.Fatal("HTTP status did not take precedence over private transport text")
		}
	}
}

func TestHTTPCallsNeverFollowRedirects(t *testing.T) {
	for _, tc := range []struct{ op, method, path string }{
		{"directory lookup", http.MethodGet, "/v1/directory/" + fakeAccount},
		{"read account", http.MethodGet, "/v1/account"},
		{"read self", http.MethodGet, selfPath},
		{"list transcripts", http.MethodGet, "/v1/transcripts"},
		{"create transcript", http.MethodPost, "/v1/transcripts"},
		{"append entries", http.MethodPost, "/v1/transcripts/" + fakeID("trn_", 2) + "/entries:batch"},
		{"read transcript tail", http.MethodGet, "/v1/transcripts/" + fakeID("trn_", 2) + "?tail=true&limit=1"},
		{"close account", http.MethodPost, "/v1/accounts/" + fakeAccount + ":close"},
	} {
		t.Run(tc.op, func(t *testing.T) {
			f := newFakeCell(t)
			f.redirect[tc.op] = true
			d := deps{client: newHTTPClient()}
			var out map[string]any
			err := requestJSON(context.Background(), d, tc.method, f.server.URL, tc.path, fakeAgentToken, tc.op, 30*time.Second, nil, &out, 200)
			if err == nil || err.Error() != "stopped: "+tc.op+" returned HTTP 302" {
				t.Fatal("redirect did not retain its exact status message")
			}
			if f.count(tc.op) != 1 || f.redirectHits != 0 {
				t.Fatal("HTTP operation followed a redirect")
			}
		})
	}
}

func TestMeasureRefusesUnexpectedHeaders(t *testing.T) {
	for _, name := range []string{"purpose", "length"} {
		t.Run(name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Method != http.MethodGet || r.URL.Path != "/v1/export" {
					t.Error("measure called an unexpected route")
				}
				if name == "purpose" {
					w.Header().Set("X-Witself-Export-Purpose", "backup")
					w.Header().Set("Content-Length", "0")
					w.WriteHeader(200)
					return
				}
				w.Header().Set("X-Witself-Export-Purpose", "self")
				w.WriteHeader(200)
				w.(http.Flusher).Flush()
				_, _ = io.WriteString(w, "x")
			}))
			defer server.Close()
			d := deps{now: func() time.Time { return time.Date(2026, 9, 30, 14, 0, 0, 0, time.UTC) }, client: newHTTPClient(), stdout: io.Discard, stderr: io.Discard}
			c := config{account: fakeAccount, limits: thresholds{90, 5, 70, 75}}
			s := session{endpoint: server.URL, operatorToken: fakeOperatorToken}
			r := readings{nodes: []nodeReading{{fsCapacity: 1 << 30}}, ready: true}
			_, err := measureArchive(context.Background(), c, d, s, r, 0, 1, 1, false)
			var stop *toolError
			if !errors.As(err, &stop) || stop.code != 5 || stop.msg != "stopped: unexpected response from export" {
				t.Fatal("invalid export headers did not produce the exact refusal")
			}
			if requests.Load() != 1 {
				t.Fatal("invalid export headers caused a retry")
			}
		})
	}
}

type fixtureRoundTripper func(*http.Request) (*http.Response, error)

func (f fixtureRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestMarkRefusesUnexpectedCreateResponse(t *testing.T) {
	for _, name := range []string{"account", "title", "metadata"} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, 10)
			base := h.d.client.Transport
			h.d.client.Transport = fixtureRoundTripper(func(req *http.Request) (*http.Response, error) {
				if req.Method != http.MethodPost || req.URL.Path != "/v1/transcripts" {
					return base.RoundTrip(req)
				}
				tr := transcript{ID: fakeID("trn_", 1), AccountID: fakeAccount, ExternalID: "wfl1-marker", Title: markerTitle, Metadata: markerMetadata(fakeAccount, 10)}
				switch name {
				case "account":
					tr.AccountID = "acc_bbbbbbbbbbbbbbbb"
				case "title":
					tr.Title = "unexpected"
				case "metadata":
					tr.Metadata = json.RawMessage(`{"generator":2}`)
				}
				data, _ := json.Marshal(map[string]any{"transcript": tr})
				return &http.Response{StatusCode: 201, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(data)), ContentLength: int64(len(data)), Request: req}, nil
			})
			code, _, stderr := h.run(h.args("mark", "--yes")...)
			assertResult(t, code, 5, stderr, "stopped: unexpected response from create transcript\n")
		})
	}
}

func TestTruncatedJSONResponseRetainsTransportClass(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(201)
		_, _ = io.WriteString(w, `{"entries":[`)
	}))
	defer server.Close()
	var out map[string]any
	err := requestJSON(context.Background(), deps{client: newHTTPClient()}, http.MethodPost, server.URL, "/v1/transcripts/"+fakeID("trn_", 1)+"/entries:batch", fakeAgentToken, "append entries", 30*time.Second, nil, &out, 201)
	var call *callError
	if !errors.As(err, &call) || call.class != "connection" || !call.retryable() || call.Error() != "stopped: append entries failed (connection)" {
		t.Fatal("cutoff response lost its retryable transport classification")
	}
}

func TestTLSHandshakePhaseIsValueFree(t *testing.T) {
	client := newHTTPClient()
	client.Transport = handshakeTransport{fixtureRoundTripper(func(req *http.Request) (*http.Response, error) {
		httptrace.ContextClientTrace(req.Context()).TLSHandshakeDone(tls.ConnectionState{}, errors.New(fakeOtherToken))
		return nil, errors.New(fakeOtherToken)
	})}
	var out map[string]any
	err := requestJSON(context.Background(), deps{client: client}, "GET", "https://localhost", "/v1/account", fakeOperatorToken, "read account", time.Second, nil, &out, 200)
	if err == nil || err.Error() != "stopped: read account failed (tls)" {
		t.Fatal("opaque handshake failure lost its safe TLS classification")
	}
}
