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
	"net/http/httptrace"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/witwave-ai/witself/internal/tokenfile"
)

func newHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableKeepAlives = true
	return &http.Client{Transport: handshakeTransport{transport}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// TLS returns some handshake failures without an exported error type. The trace
// records the phase so classification does not inspect private error text.
type handshakeTransport struct{ base http.RoundTripper }
type handshakeFailure struct{ cause error }

func (e *handshakeFailure) Error() string { return "TLS handshake failed" }
func (e *handshakeFailure) Unwrap() error { return e.cause }
func (t handshakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var failed atomic.Bool
	trace := &httptrace.ClientTrace{TLSHandshakeDone: func(_ tls.ConnectionState, err error) { failed.Store(err != nil) }}
	traced := req.Clone(httptrace.WithClientTrace(req.Context(), trace))
	resp, err := t.base.RoundTrip(traced)
	if err != nil && failed.Load() {
		return resp, &handshakeFailure{err}
	}
	return resp, err
}

func errorClass(err error) string {
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return "timeout"
	}
	var handshake *handshakeFailure
	var cv *tls.CertificateVerificationError
	var ua x509.UnknownAuthorityError
	var hn x509.HostnameError
	var ci x509.CertificateInvalidError
	var rh tls.RecordHeaderError
	var ae tls.AlertError
	if errors.As(err, &handshake) || errors.As(err, &cv) || errors.As(err, &ua) || errors.As(err, &hn) || errors.As(err, &ci) || errors.As(err, &rh) || errors.As(err, &ae) {
		return "tls"
	}
	return "connection"
}
func httpFailure(op string, status int, err error) error {
	if status != 0 {
		return failure(5, "stopped: %s returned HTTP %d", op, status)
	}
	return failure(5, "stopped: %s failed (%s)", op, errorClass(err))
}

type callError struct {
	op     string
	status int
	class  string
}

func (e *callError) Error() string {
	if e.status != 0 {
		return fmt.Sprintf("stopped: %s returned HTTP %d", e.op, e.status)
	}
	return fmt.Sprintf("stopped: %s failed (%s)", e.op, e.class)
}
func (e *callError) Unwrap() error   { return failure(5, "%s", e.Error()) }
func (e *callError) retryable() bool { return e.status == 0 || e.status >= 500 }
func (e *callError) label() string {
	if e.status != 0 {
		return fmt.Sprintf("HTTP %d", e.status)
	}
	return e.class
}
func requestJSON(ctx context.Context, d deps, method, base, path, token, op string, timeout time.Duration, body, out any, want int) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		if err != nil {
			return failure(5, "stopped: unexpected response from %s", op)
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, bytes.NewReader(data))
	if err != nil {
		return &callError{op: op, class: errorClass(err)}
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return &callError{op: op, class: errorClass(err)}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || want != 0 && resp.StatusCode != want {
		return &callError{op: op, status: resp.StatusCode}
	}
	if out == nil {
		_, err = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		if err != nil {
			return &callError{op: op, class: errorClass(err)}
		}
		return nil
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, 16<<20))
	if decodeErr := dec.Decode(out); decodeErr != nil {
		var networkError net.Error
		if errors.Is(decodeErr, context.DeadlineExceeded) || errors.Is(decodeErr, io.ErrUnexpectedEOF) || errors.As(decodeErr, &networkError) {
			return &callError{op: op, class: errorClass(decodeErr)}
		}
		return failure(5, "stopped: unexpected response from %s", op)
	}
	var extra any
	if decodeErr := dec.Decode(&extra); !errors.Is(decodeErr, io.EOF) {
		var networkError net.Error
		if errors.Is(decodeErr, context.DeadlineExceeded) || errors.Is(decodeErr, io.ErrUnexpectedEOF) || errors.As(decodeErr, &networkError) {
			return &callError{op: op, class: errorClass(decodeErr)}
		}
		return failure(5, "stopped: unexpected response from %s", op)
	}
	return nil
}
func safeEndpoint(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", failure(3, "refused: endpoint must use https")
	}
	host := u.Hostname()
	loopback := host == "127.0.0.1" || host == "::1" || host == "localhost"
	if u.Scheme != "https" && (u.Scheme != "http" || !loopback) {
		return "", failure(3, "refused: endpoint must use https")
	}
	return strings.TrimRight(raw, "/"), nil
}
func readToken(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", failure(3, "refused: cannot read token file %s", safeText(path))
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", failure(3, "refused: token file %s must not be readable by group or others", safeText(path))
	}
	token, err := tokenfile.Read(path, tokenfile.Options{})
	if err != nil {
		return "", failure(3, "refused: cannot read token file %s", safeText(path))
	}
	return token, nil
}

var longHex = regexp.MustCompile(`[a-fA-F0-9]{64}`)
var privateIdentifier = regexp.MustCompile(`\b(?:ent|trn)_[a-z2-7]{16}`)
var safeWord = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,128}$`)

func safeText(v string) string {
	if strings.ContainsAny(v, "\r\n\t") || longHex.MatchString(v) || privateIdentifier.MatchString(v) {
		return "redacted"
	}
	return v
}
func responseWord(v string) string {
	if !safeWord.MatchString(v) {
		return "unknown"
	}
	if strings.Contains(v, "ent_") || strings.Contains(v, "trn_") {
		return "redacted"
	}
	return safeText(v)
}

type accountInfo struct {
	ID        string    `json:"id"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}
type transcript struct {
	ID         string          `json:"id"`
	AccountID  string          `json:"account_id"`
	ExternalID string          `json:"external_id"`
	Title      string          `json:"title"`
	Metadata   json.RawMessage `json:"metadata"`
}
type session struct {
	endpoint, cell, agentToken, operatorToken, realm, agent string
	account                                                 accountInfo
	transcripts                                             []transcript
}

const selfPath = "/v1/self?observational=true&include_facts=false&include_salient=false&include_counts=false&include_checkpoint=false&include_message_checkpoint=false&include_email_checkpoint=false&include_avatar_checkpoint=false&include_plan_entitlements=false"

var transcriptIDPattern = regexp.MustCompile(`^trn_[a-z2-7]{16}$`)

func listTranscripts(ctx context.Context, c config, d deps, s session, token string) ([]transcript, error) {
	var out struct {
		Transcripts []transcript `json:"transcripts"`
	}
	err := requestJSON(ctx, d, "GET", s.endpoint, "/v1/transcripts", token, "list transcripts", 30*time.Second, nil, &out, 200)
	if err != nil {
		return nil, err
	}
	if len(out.Transcripts) > 100 {
		return nil, failure(5, "stopped: unexpected response from list transcripts")
	}
	for _, tr := range out.Transcripts {
		if !transcriptIDPattern.MatchString(tr.ID) || tr.AccountID != c.account {
			return nil, failure(5, "stopped: unexpected response from list transcripts")
		}
	}
	return out.Transcripts, nil
}
