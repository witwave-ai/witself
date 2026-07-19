// Package dashboard serves the local, read-only, per-agent web dashboard
// mounted by `witself dashboard serve` (ADR 0004). Every route is a thin
// authenticated proxy over the public /v1 read API using the agent's own
// token: self digests, transcripts, and fact lists use observational reads,
// messages use the passive metadata-only list, broad memory and fact reads
// stay redacted (a sensitive fact value appears only in the single-fact
// user-initiated reveal response), and the
// avatar SVG passes the same canonical sanitizer-and-hash gate as
// `witself self card`. Sealed secrets are metadata only: the proxy uses the
// two public GET routes and rebuilds every payload as an allow-list
// projection, so secret material — plaintext, ciphertext, wrapped DEKs, key
// bytes — never reaches the browser. The package mounts routes onto a
// caller-owned mux (the cpserver.Register idiom); the binary owns the
// listener and lifecycle.
package dashboard

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/witwave-ai/witself/internal/avatar"
	"github.com/witwave-ai/witself/internal/client"
)

//go:embed static
var staticFS embed.FS

const (
	// accessCookiePrefix is completed with the listener port so dashboards
	// for different agents (different ports, same 127.0.0.1 cookie host —
	// RFC 6265 has no port isolation) never clobber each other's session.
	accessCookiePrefix  = "witself_dashboard_"
	defaultPollInterval = 2 * time.Second

	// markerHeader tags every response so the registry liveness probe can
	// tell a real dashboard from an unrelated process that reused its PID
	// or port. It carries no secret.
	markerHeader = "Witself-Dashboard"

	// contentSecurityPolicy locks the page to same-origin assets only; all
	// UI files are embedded, so nothing may load from anywhere else.
	// frame-ancestors 'none' (default-src never falls back for it) plus
	// X-Frame-Options in secure() keep the authenticated app out of iframes:
	// same-site is port-blind on loopback, so any other 127.0.0.1 listener's
	// page could otherwise embed the dashboard with the session cookie
	// attached.
	contentSecurityPolicy = "default-src 'self'; img-src 'self' data:; " +
		"style-src 'self'; script-src 'self'; connect-src 'self'; " +
		"frame-ancestors 'none'"

	// sseTranscriptPageLimit is the upstream page size the poll loop asks
	// for (the server maximum), and maxSSEPagesPerTick bounds how many of
	// those pages one tick may drain so a hot transcript cannot starve the
	// self digest; the next tick resumes from the advanced cursor.
	sseTranscriptPageLimit = 500
	maxSSEPagesPerTick     = 20

	// sseMessagePageLimit is the newest-first first-page size the messages
	// tick asks for in each mailbox direction (the server maximum). The
	// mailbox cursor only pages backward in time, so incremental fetch is
	// impossible: every tick re-reads the first page and the browser dedupes
	// by message id.
	sseMessagePageLimit = 100

	// sseMemoryPageLimit is the first-page size the memories tick asks for,
	// matching the memories view's own fetch. Every tick re-reads the first
	// page and the browser skips identical frames.
	sseMemoryPageLimit = 100

	// sseFactPageLimit is the first-page size the facts tick asks for,
	// matching the facts view's own fetch. Every tick re-reads the first
	// page and the browser skips identical frames.
	sseFactPageLimit = 100

	// sseSecretPageLimit is the first-page size the secrets tick asks for
	// (the server maximum), matching the secrets view's own fetch. The tick
	// exists because the sealed plane's list and get reads are pure metadata
	// SELECTs — unlike the field :access route, they write no audit event
	// and no usage row — so polling cannot spam the value-free ledger.
	sseSecretPageLimit = 100
)

// maxSSEConnections caps concurrent /api/events streams. A var only so tests
// can lower it; Register snapshots it into the connection semaphore.
var maxSSEConnections = 8

// Config carries everything Register needs to proxy one agent's read surface.
type Config struct {
	// Endpoint and BearerToken reach the agent's cell exactly like the CLI.
	Endpoint    string
	BearerToken string
	// AccessToken guards the local HTTP surface: ?token=<AccessToken> is
	// accepted once and exchanged for an HttpOnly SameSite=Strict session
	// cookie holding a distinct per-process random value. The URL token
	// itself is never stored in the cookie: loopback cookies are host-only
	// (never port-isolated, RFC 6265), so the browser attaches them to any
	// other 127.0.0.1 listener the operator visits, and that value must not
	// be the printed credential.
	AccessToken string
	Identity    client.SelfIdentity
	Version     string
	// PollInterval bounds the SSE poll loop; zero means 2s.
	PollInterval time.Duration
}

// Register mounts every dashboard route onto mux. All routes require the
// loopback Host header and the access token, and set no-store plus a
// same-origin content-security policy.
func Register(mux *http.ServeMux, cfg Config) error {
	if mux == nil {
		return fmt.Errorf("dashboard: mux is required")
	}
	if cfg.Endpoint == "" || cfg.AccessToken == "" {
		return fmt.Errorf("dashboard: Endpoint and AccessToken are required")
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaultPollInterval
	}
	session, err := newSessionSecret()
	if err != nil {
		return err
	}
	sem := make(chan struct{}, maxSSEConnections)
	factReads := &factReadCapability{}
	secrets := &secretsCapability{}
	mux.Handle("GET /{$}", secure(cfg, session, http.HandlerFunc(indexHandler)))
	mux.Handle("GET /static/", secure(cfg, session, http.FileServerFS(staticFS)))
	mux.Handle("GET /api/self", secure(cfg, session, selfHandler(cfg)))
	mux.Handle("GET /api/themes", secure(cfg, session, http.HandlerFunc(themesHandler)))
	mux.Handle("GET /api/avatar.svg", secure(cfg, session, avatarHandler(cfg)))
	mux.Handle("GET /api/transcripts", secure(cfg, session, transcriptsHandler(cfg)))
	mux.Handle("GET /api/transcripts/{id}", secure(cfg, session, transcriptPageHandler(cfg)))
	mux.Handle("GET /api/memories", secure(cfg, session, memoriesHandler(cfg)))
	mux.Handle("GET /api/memories/{id}", secure(cfg, session, memoryHandler(cfg)))
	mux.Handle("GET /api/memories/{id}/history", secure(cfg, session, memoryHistoryHandler(cfg)))
	mux.Handle("GET /api/messages", secure(cfg, session, messagesHandler(cfg)))
	mux.Handle("GET /api/facts", secure(cfg, session, factsHandler(cfg, factReads)))
	mux.Handle("GET /api/facts/{id}/history", secure(cfg, session, factHistoryHandler(cfg, factReads)))
	// The reveal is query-addressed like the upstream exact read
	// (GET /v1/facts?subject&predicate): a /api/facts/{subject}/{predicate}
	// path shape would let the literal /history pattern above shadow any
	// fact whose predicate is the single segment "history", which the
	// server's predicate grammar permits.
	mux.Handle("GET /api/fact", secure(cfg, session, factRevealHandler(cfg)))
	mux.Handle("GET /api/secrets", secure(cfg, session, secretsHandler(cfg, secrets)))
	mux.Handle("GET /api/secrets/{id}", secure(cfg, session, secretHandler(cfg, secrets)))
	mux.Handle("GET /api/events", secure(cfg, session, eventsHandler(cfg, sem, factReads, secrets)))
	mux.Handle("/", secure(cfg, session, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSONError(w, http.StatusNotFound, "not found")
	})))
	return nil
}

// newSessionSecret mints the random per-process value the browser holds in
// its session cookie after the one-time ?token= exchange. Restarting the
// serve invalidates every outstanding cookie.
func newSessionSecret() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("dashboard: generate session secret: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

// secure is the middleware on every route: response hardening headers, the
// loopback Host pin, and the access-token gate. The session cookie name is
// scoped to the listener port so concurrently served agents (distinct ports,
// shared 127.0.0.1 cookie host) never overwrite each other's session, and a
// cookie minted for another port is never accepted here.
func secure(cfg Config, session string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Cache-Control", "private, no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set(markerHeader, RegistrySchemaVersion)
		port, ok := requestPort(r)
		if !ok || !hostAllowed(r, port) {
			writeJSONError(w, http.StatusForbidden, "forbidden host")
			return
		}
		if !fetchSiteAllowed(r) {
			writeJSONError(w, http.StatusForbidden, "cross-site request refused")
			return
		}
		cookieName := accessCookiePrefix + port
		if token := r.URL.Query().Get("token"); token != "" {
			if !tokenMatches(token, cfg.AccessToken) {
				writeJSONError(w, http.StatusUnauthorized, "invalid access token")
				return
			}
			http.SetCookie(w, &http.Cookie{
				Name:     cookieName,
				Value:    session,
				Path:     "/",
				HttpOnly: true,
				SameSite: http.SameSiteStrictMode,
			})
			clean := *r.URL
			query := clean.Query()
			query.Del("token")
			clean.RawQuery = query.Encode()
			http.Redirect(w, r, clean.RequestURI(), http.StatusSeeOther)
			return
		}
		cookie, err := r.Cookie(cookieName)
		if err != nil || !tokenMatches(cookie.Value, session) {
			writeJSONError(w, http.StatusUnauthorized,
				"missing or invalid access token (open the ?token= URL printed at startup)")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requestPort resolves the port this request actually arrived on, preferring
// the connection's local address and falling back to the Host header when
// the transport provides none.
func requestPort(r *http.Request) (string, bool) {
	if addr, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok && addr != nil {
		if _, port, err := net.SplitHostPort(addr.String()); err == nil {
			return port, true
		}
	}
	if _, port, err := net.SplitHostPort(r.Host); err == nil {
		return port, true
	}
	return "", false
}

// hostAllowed pins the Host header to the loopback listener so DNS-rebinding
// pages cannot reach the dashboard through an attacker-controlled name, and
// the Host port must match the port the connection arrived on.
func hostAllowed(r *http.Request, listenPort string) bool {
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil {
		return false
	}
	host = strings.ToLower(host)
	if host != "127.0.0.1" && host != "localhost" {
		return false
	}
	return port == listenPort
}

// fetchSiteAllowed rejects requests a browser marks as coming from another
// site. SameSite=Strict does not close this hole: same-site ignores ports
// (RFC 6265), so a page on any other loopback port sends credentialed
// requests that could hold every SSE slot and drive authenticated upstream
// polling. Only browser-internal navigations ("none"), the dashboard's own
// pages ("same-origin"), and clients that send no fetch metadata (the CLI,
// the registry probe, curl) may pass; the token/cookie gate still applies to
// all of them.
func fetchSiteAllowed(r *http.Request) bool {
	site := r.Header.Get("Sec-Fetch-Site")
	return site == "" || site == "same-origin" || site == "none"
}

func tokenMatches(candidate, expected string) bool {
	if expected == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(candidate), []byte(expected)) == 1
}

func indexHandler(w http.ResponseWriter, r *http.Request) {
	http.ServeFileFS(w, r, staticFS, "static/index.html")
}

// themesHandler lists the embedded theme packs so the UI builds its picker
// from whatever shipped: adding a theme is dropping a CSS file into
// static/themes, never a JS or HTML edit (ADR 0004).
func themesHandler(w http.ResponseWriter, _ *http.Request) {
	entries, err := staticFS.ReadDir("static/themes")
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "embedded themes unavailable")
		return
	}
	names := []string{}
	for _, entry := range entries {
		if name, found := strings.CutSuffix(entry.Name(), ".css"); found && !entry.IsDir() {
			names = append(names, name)
		}
	}
	writeJSON(w, map[string]any{"themes": names})
}

// selfEnvelope is the observational self digest plus dashboard metadata for
// the status bar. Observational is false only after the 501 degrade path.
type selfEnvelope struct {
	client.SelfDigest
	Observational  bool   `json:"observational"`
	Version        string `json:"dashboard_version,omitempty"`
	PollIntervalMS int64  `json:"poll_interval_ms,omitempty"`
}

// dashboardSelfOptions is the exact bounded digest the dashboard renders:
// no fact values, salient memories, counts, and the value-free checkpoints.
var dashboardSelfOptions = client.SelfOptions{
	Observational:            true,
	IncludeFacts:             false,
	IncludeSalient:           true,
	IncludeCounts:            true,
	IncludeCheckpoint:        true,
	IncludeMessageCheckpoint: true,
	IncludeAvatarCheckpoint:  true,
}

// fetchSelf reads the observational self digest, degrading exactly once to a
// plain read when the cell has no observational hook wired (501).
func fetchSelf(ctx context.Context, cfg Config) (client.SelfDigest, bool, error) {
	opts := dashboardSelfOptions
	digest, err := client.GetSelf(ctx, cfg.Endpoint, cfg.BearerToken, opts)
	if err == nil {
		return digest, true, nil
	}
	if !isObservationalUnavailable(err) {
		return client.SelfDigest{}, false, err
	}
	opts.Observational = false
	digest, err = client.GetSelf(ctx, cfg.Endpoint, cfg.BearerToken, opts)
	if err != nil {
		return client.SelfDigest{}, false, err
	}
	return digest, false, nil
}

// isObservationalUnavailable matches the cell's 501 for unwired observational
// hooks. The client surfaces the server's error text ("observational ... are
// unavailable") or the bare status line when the body carries no error field.
func isObservationalUnavailable(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "observational") || strings.Contains(message, "501")
}

func (cfg Config) selfEnvelope(digest client.SelfDigest, observational bool) selfEnvelope {
	return selfEnvelope{
		SelfDigest:     digest,
		Observational:  observational,
		Version:        cfg.Version,
		PollIntervalMS: cfg.PollInterval.Milliseconds(),
	}
}

func selfHandler(cfg Config) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		digest, observational, err := fetchSelf(r.Context(), cfg)
		if err != nil {
			writeUpstreamError(w, err)
			return
		}
		writeJSON(w, cfg.selfEnvelope(digest, observational))
	})
}

// avatarHandler serves the active avatar SVG behind the exact gate
// `witself self card` applies: the wire payload must already be in canonical
// sanitized form and match its recorded SHA-256, and only the locally
// sanitized bytes are ever written to the browser.
func avatarHandler(cfg Config) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		view, err := client.GetSelfAvatar(r.Context(), cfg.Endpoint, cfg.BearerToken)
		if err != nil {
			writeUpstreamError(w, err)
			return
		}
		if view == nil || view.Active == nil || view.Active.SVG == "" {
			writeJSONError(w, http.StatusNotFound, "no active avatar payload")
			return
		}
		wire := []byte(view.Active.SVG)
		sanitized, err := avatar.SanitizeSVG(wire)
		if err != nil {
			writeJSONError(w, http.StatusBadGateway, "active avatar SVG failed sanitization")
			return
		}
		if !bytes.Equal(sanitized, wire) {
			writeJSONError(w, http.StatusBadGateway, "active avatar SVG is not in canonical sanitized form")
			return
		}
		expectedHash, err := hex.DecodeString(view.Active.SVGSHA256)
		if err != nil || len(expectedHash) != sha256.Size {
			writeJSONError(w, http.StatusBadGateway, "active avatar SVG hash is not a SHA-256 digest")
			return
		}
		actualHash := sha256.Sum256(sanitized)
		if subtle.ConstantTimeCompare(expectedHash, actualHash[:]) != 1 {
			writeJSONError(w, http.StatusBadGateway, "active avatar SVG hash does not match its sanitized content")
			return
		}
		w.Header().Set("Content-Type", "image/svg+xml")
		_, _ = w.Write(sanitized)
	})
}

func transcriptsHandler(cfg Config) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		transcripts, err := client.ListTranscripts(r.Context(), cfg.Endpoint, cfg.BearerToken)
		if err != nil {
			writeUpstreamError(w, err)
			return
		}
		if transcripts == nil {
			transcripts = []client.Transcript{}
		}
		writeJSON(w, map[string]any{"transcripts": transcripts})
	})
}

func transcriptPageHandler(cfg Config) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		opts := client.TranscriptPageOptions{Observational: true}
		query := r.URL.Query()
		if raw := query.Get("after_sequence"); raw != "" {
			value, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || value < 0 {
				writeJSONError(w, http.StatusBadRequest, "after_sequence must be a non-negative integer")
				return
			}
			opts.AfterSequence = value
		}
		if raw := query.Get("limit"); raw != "" {
			value, err := strconv.Atoi(raw)
			if err != nil || value < 0 {
				writeJSONError(w, http.StatusBadRequest, "limit must be a non-negative integer")
				return
			}
			opts.Limit = value
		}
		if raw := query.Get("tail"); raw != "" {
			value, err := strconv.ParseBool(raw)
			if err != nil {
				writeJSONError(w, http.StatusBadRequest, "tail must be a boolean")
				return
			}
			opts.Tail = value
		}
		page, err := client.GetTranscriptPage(r.Context(), cfg.Endpoint, cfg.BearerToken, r.PathValue("id"), opts)
		if err != nil {
			writeUpstreamError(w, err)
			return
		}
		writeJSON(w, page)
	})
}

// memoriesHandler proxies the redacted-by-default broad memory list. It never
// sets include_sensitive; private memory values stay out of the browser.
func memoriesHandler(cfg Config) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		opts := client.MemoryListOptions{
			State:  query.Get("state"),
			Kind:   query.Get("kind"),
			Tags:   query["tag"],
			Cursor: query.Get("cursor"),
		}
		if raw := query.Get("limit"); raw != "" {
			value, err := strconv.Atoi(raw)
			if err != nil || value < 0 {
				writeJSONError(w, http.StatusBadRequest, "limit must be a non-negative integer")
				return
			}
			opts.Limit = value
		}
		page, err := client.ListMemories(r.Context(), cfg.Endpoint, cfg.BearerToken, opts)
		if err != nil {
			writeUpstreamError(w, err)
			return
		}
		writeJSON(w, page)
	})
}

func memoryHandler(cfg Config) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		memory, err := client.GetMemory(r.Context(), cfg.Endpoint, cfg.BearerToken, r.PathValue("id"))
		if err != nil {
			writeUpstreamError(w, err)
			return
		}
		writeJSON(w, map[string]any{"memory": memory})
	})
}

func memoryHistoryHandler(cfg Config) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		opts := client.MemoryHistoryOptions{Cursor: query.Get("cursor")}
		if raw := query.Get("limit"); raw != "" {
			value, err := strconv.Atoi(raw)
			if err != nil || value < 0 {
				writeJSONError(w, http.StatusBadRequest, "limit must be a non-negative integer")
				return
			}
			opts.Limit = value
		}
		page, err := client.GetMemoryHistory(r.Context(), cfg.Endpoint, cfg.BearerToken, r.PathValue("id"), opts)
		if err != nil {
			writeUpstreamError(w, err)
			return
		}
		writeJSON(w, page)
	})
}

// messagesHandler proxies the passive metadata-only mailbox list. The
// dashboard never calls message read or listen: viewing must not mark
// anything read or compete for the agent's listen slots.
func messagesHandler(cfg Config) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		opts := client.MessageListOptions{
			Direction: query.Get("direction"),
			From:      query.Get("from"),
			ThreadID:  query.Get("thread_id"),
			Kind:      query.Get("kind"),
			Cursor:    query.Get("cursor"),
		}
		if raw := query.Get("unread"); raw != "" {
			value, err := strconv.ParseBool(raw)
			if err != nil {
				writeJSONError(w, http.StatusBadRequest, "unread must be a boolean")
				return
			}
			opts.Unread = value
		}
		if raw := query.Get("limit"); raw != "" {
			value, err := strconv.Atoi(raw)
			if err != nil || value < 0 {
				writeJSONError(w, http.StatusBadRequest, "limit must be a non-negative integer")
				return
			}
			opts.Limit = value
		}
		page, err := client.ListMessages(r.Context(), cfg.Endpoint, cfg.BearerToken, opts)
		if err != nil {
			writeUpstreamError(w, err)
			return
		}
		page.Messages = stripMessageBodies(page.Messages)
		writeJSON(w, page)
	})
}

// stripMessageBodies enforces the metadata-only message guarantee locally:
// the passive list should never carry bodies, but the proxy does not rely on
// server-side stripping (another cell version could regress it) and zeroes
// Body and Payload before anything reaches the browser.
func stripMessageBodies(messages []client.Message) []client.Message {
	for i := range messages {
		messages[i].Body = ""
		messages[i].Payload = nil
	}
	return messages
}

// factReadCapability memoizes one client.ProbeObservationalFactReads answer
// per serve. Cells released before the observational fact-read parameter
// ignore it and silently run the plain usage-recording read path instead of
// answering 501, so every broad fact surface — the list, the SSE facts tick,
// and the history sensitivity probe — asks this gate before its first read.
// Only a definitive answer is cached; a probe failure is returned and the
// next caller retries.
type factReadCapability struct {
	mu        sync.Mutex
	resolved  bool
	supported bool
}

func (c *factReadCapability) observationalSupported(ctx context.Context, cfg Config) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.resolved {
		return c.supported, nil
	}
	supported, err := client.ProbeObservationalFactReads(ctx, cfg.Endpoint, cfg.BearerToken)
	if err != nil {
		return false, err
	}
	c.resolved, c.supported = true, supported
	return supported, nil
}

// factsHandler proxies the broad fact inventory as an observational read and
// never sets include_sensitive. The plain (non-observational) list records
// ranking-eligible search usage on every returned fact (store.ListFacts), so
// a cell without observational fact reads is NOT degraded the way fetchSelf
// degrades: silently perturbing usage ranking on every render would break
// the "viewing does not perturb the agent" rule. The UI renders a clear 501
// instead, whether the cell reports one itself (parameter known, hooks
// unwired) or predates the parameter entirely and would have ignored it (the
// capability gate).
func factsHandler(cfg Config, factReads *factReadCapability) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		opts := client.FactListOptions{
			Subject:         query.Get("subject"),
			PredicatePrefix: query.Get("predicate_prefix"),
			Observational:   true,
		}
		if raw := query.Get("limit"); raw != "" {
			value, err := strconv.Atoi(raw)
			if err != nil || value < 0 {
				writeJSONError(w, http.StatusBadRequest, "limit must be a non-negative integer")
				return
			}
			opts.Limit = value
		}
		supported, err := factReads.observationalSupported(r.Context(), cfg)
		if err != nil {
			writeUpstreamError(w, err)
			return
		}
		if !supported {
			writeJSONError(w, http.StatusNotImplemented, "cell does not support observational fact reads")
			return
		}
		facts, err := client.ListFacts(r.Context(), cfg.Endpoint, cfg.BearerToken, opts)
		if err != nil {
			if isObservationalUnavailable(err) {
				writeJSONError(w, http.StatusNotImplemented, "cell does not support observational fact reads")
				return
			}
			writeUpstreamError(w, err)
			return
		}
		writeJSON(w, map[string]any{"facts": redactSensitiveFacts(facts)})
	})
}

// redactSensitiveFacts enforces the sensitive-value redaction locally,
// mirroring stripMessageBodies: the redacted upstream list should already
// carry null values for sensitive facts, but the proxy does not rely on
// server-side redaction (another cell version could regress it) and zeroes
// every sensitive value and source ref before a broad payload — list
// responses and SSE frames — reaches the browser. The single-fact reveal
// endpoint is the only response that may carry a sensitive value.
func redactSensitiveFacts(facts []client.Fact) []client.Fact {
	if facts == nil {
		facts = []client.Fact{}
	}
	for i := range facts {
		if facts[i].Sensitive {
			facts[i].Value = json.RawMessage(`null`)
			facts[i].SourceRef = ""
		}
	}
	return facts
}

// factRevealHandler is the user-initiated eye-icon reveal: one exact
// observational read that returns the value (sensitive included — this is
// the one response that may carry it) without recording delivery usage. On
// cells that answer the observational read with 501 it falls back to the
// plain exact read, and a cell that predates the parameter ignores it and
// runs that plain read directly: either way a reveal is an intentional,
// per-fact exact lookup the user clicked for, so recording one legitimate
// delivery usage is acceptable there — unlike the broad list, which re-polls
// on every render.
func factRevealHandler(cfg Config) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		subject, predicate := query.Get("subject"), query.Get("predicate")
		if subject == "" || predicate == "" {
			writeJSONError(w, http.StatusBadRequest, "subject and predicate are required")
			return
		}
		fact, err := client.GetFactObservational(r.Context(), cfg.Endpoint, cfg.BearerToken, subject, predicate)
		if err != nil && isObservationalUnavailable(err) {
			fact, err = client.GetFact(r.Context(), cfg.Endpoint, cfg.BearerToken, subject, predicate)
		}
		if err != nil {
			writeUpstreamError(w, err)
			return
		}
		writeJSON(w, map[string]any{"fact": fact})
	})
}

// factHistoryHandler serves the drill-in assertion history. The upstream
// history read records no usage but does NOT redact sensitive assertion
// values, so the proxy forwards values only after proving the fact is
// non-sensitive: an exact observational read (usage-free) of the
// subject/predicate the browser names for this id. On any doubt — missing
// params, a cell without observational fact reads (which would record a
// delivery on the probe), an id mismatch, or a sensitive fact — every
// assertion value is zeroed; v1 has no per-assertion reveal.
func factHistoryHandler(cfg Config, factReads *factReadCapability) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		factID := r.PathValue("id")
		assertions, err := client.GetFactHistory(r.Context(), cfg.Endpoint, cfg.BearerToken, factID)
		if err != nil {
			writeUpstreamError(w, err)
			return
		}
		if assertions == nil {
			assertions = []client.FactAssertion{}
		}
		query := r.URL.Query()
		if !factProvenNonSensitive(r.Context(), cfg, factReads, factID, query.Get("subject"), query.Get("predicate")) {
			for i := range assertions {
				assertions[i].Value = json.RawMessage(`null`)
				assertions[i].SourceRef = ""
			}
		}
		writeJSON(w, map[string]any{"assertions": assertions})
	})
}

func factProvenNonSensitive(ctx context.Context, cfg Config, factReads *factReadCapability, factID, subject, predicate string) bool {
	if subject == "" || predicate == "" {
		return false
	}
	if supported, err := factReads.observationalSupported(ctx, cfg); err != nil || !supported {
		return false
	}
	fact, err := client.GetFactObservational(ctx, cfg.Endpoint, cfg.BearerToken, subject, predicate)
	if err != nil || fact == nil {
		return false
	}
	return fact.ID == factID && !fact.Sensitive
}

// secretsUnavailableRecheck is how long one memoized /v1/secrets 404 keeps
// the secrets surface in the unavailable state before the next read may
// probe again. A var only so tests can lower it.
var secretsUnavailableRecheck = time.Minute

// secretsCapability memoizes whether this cell serves the sealed secrets
// plane. Cells released before v0.0.187 have no /v1/secrets routes and
// answer 404, which the UI must render as "sealed plane not available on
// this cell" rather than a generic error. Any definitive secrets read
// settles the answer; a transport failure is returned and the next caller
// retries. A negative answer expires after secretsUnavailableRecheck: the
// client maps every upstream 404 to ErrNotFound — including one minted by
// fronting infrastructure mid-deploy — so "no sealed plane" is re-proven
// once a window rather than disabling the pane for the process lifetime.
type secretsCapability struct {
	mu        sync.Mutex
	resolved  bool
	supported bool
	deniedAt  time.Time
}

func (c *secretsCapability) note(supported bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resolved, c.supported = true, supported
	if !supported {
		c.deniedAt = time.Now()
	}
}

// status reports the memoized answer; ok is false when no usable answer
// exists — never resolved, or a negative past its recheck window.
func (c *secretsCapability) status() (supported, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.resolved || (!c.supported && time.Since(c.deniedAt) >= secretsUnavailableRecheck) {
		return false, false
	}
	return c.supported, true
}

// knownUnavailable reports a fresh memoized negative answer without probing,
// so the SSE tick on a pre-sealed-plane cell polls once per recheck window
// instead of every 2s.
func (c *secretsCapability) knownUnavailable() bool {
	supported, ok := c.status()
	return ok && !supported
}

// available resolves cell support, probing with one bounded list read when
// no usable memoized answer exists. The probe is side-effect-free — the
// sealed plane's list is a pure metadata SELECT with no audit or usage row —
// and runs outside the mutex: it may take the full client timeout, and
// holding the lock would stall every SSE tick and secrets response behind
// one slow upstream. Concurrent probes are harmless duplicates of the same
// read.
func (c *secretsCapability) available(ctx context.Context, cfg Config) (bool, error) {
	if supported, ok := c.status(); ok {
		return supported, nil
	}
	_, err := client.ListSecrets(ctx, cfg.Endpoint, cfg.BearerToken, client.SecretListOptions{Limit: 1})
	if errors.Is(err, client.ErrNotFound) {
		c.note(false)
		return false, nil
	}
	if err != nil {
		return false, err
	}
	c.note(true)
	return true, nil
}

// writeSecretsUnavailable is the distinguishable pre-sealed-plane state the
// UI renders as a clean note instead of an error.
func writeSecretsUnavailable(w http.ResponseWriter) {
	writeJSONError(w, http.StatusNotImplemented, "cell does not serve the sealed secrets plane")
}

// sanitizedSecret is the only secret shape this proxy ever writes to the
// browser: sealed metadata, rebuilt field-by-field from the client
// projection. The client type already excludes encrypted material, but the
// proxy does not rely on that (a future client field or a misbehaving cell
// could regress it): anything not named here — ciphertext, sealed field
// payloads, wrapped DEKs, key bytes, nonces, AAD, and even explicitly
// public field values, which a cell that mislabels a sensitive field could
// leak through — is gone by construction, mirroring stripMessageBodies and
// redactSensitiveFacts.
type sanitizedSecret struct {
	ID             string                 `json:"id"`
	Name           string                 `json:"name"`
	Description    string                 `json:"description,omitempty"`
	Template       string                 `json:"template,omitempty"`
	Tags           []string               `json:"tags"`
	Lifecycle      string                 `json:"lifecycle"`
	FieldCount     int                    `json:"field_count"`
	SensitiveCount int                    `json:"sensitive_field_count"`
	Fields         []sanitizedSecretField `json:"fields"`
	CreatedAt      time.Time              `json:"created_at"`
	UpdatedAt      time.Time              `json:"updated_at"`
	ArchivedAt     *time.Time             `json:"archived_at,omitempty"`
}

// sanitizedSecretField is name, kind, and the sensitivity flag — never a
// value slot of any kind.
type sanitizedSecretField struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Kind      string `json:"kind,omitempty"`
	Sensitive bool   `json:"sensitive"`
}

// sanitizedVaultKey carries public AVK-epoch identifiers only. The private
// key is client custody and has no HTTP representation; the fingerprint and
// version are public identity, not key material.
type sanitizedVaultKey struct {
	ID             string    `json:"id"`
	KeyVersion     int64     `json:"key_version"`
	Algorithm      string    `json:"algorithm,omitempty"`
	Fingerprint    string    `json:"fingerprint,omitempty"`
	LifecycleState string    `json:"lifecycle_state,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

func sanitizeSecrets(secrets []client.Secret) []sanitizedSecret {
	out := make([]sanitizedSecret, 0, len(secrets))
	for _, secret := range secrets {
		out = append(out, sanitizeSecret(secret))
	}
	return out
}

func sanitizeSecret(secret client.Secret) sanitizedSecret {
	fields := make([]sanitizedSecretField, 0, len(secret.Fields))
	for _, field := range secret.Fields {
		fields = append(fields, sanitizedSecretField{
			ID:        field.ID,
			Name:      field.Name,
			Kind:      field.Kind,
			Sensitive: field.Sensitive,
		})
	}
	tags := secret.Tags
	if tags == nil {
		tags = []string{}
	}
	return sanitizedSecret{
		ID:             secret.ID,
		Name:           secret.Name,
		Description:    secret.Description,
		Template:       secret.Template,
		Tags:           tags,
		Lifecycle:      secret.Lifecycle,
		FieldCount:     len(fields),
		SensitiveCount: secret.SensitiveCount,
		Fields:         fields,
		CreatedAt:      secret.CreatedAt,
		UpdatedAt:      secret.UpdatedAt,
		ArchivedAt:     secret.ArchivedAt,
	}
}

// secretsHandler proxies the sealed-plane metadata list — one of exactly two
// upstream secrets reads this dashboard performs (GET /v1/secrets and
// GET /v1/secrets/{id}; never a POST, lifecycle :action, or field :access,
// which delivers encrypted material and records audit/usage). Fields are
// requested so the list can show counts, then rebuilt through the
// allow-list projection.
func secretsHandler(cfg Config, secrets *secretsCapability) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		opts := client.SecretListOptions{
			Lifecycle:     query.Get("lifecycle"),
			Cursor:        query.Get("cursor"),
			IncludeFields: true,
		}
		if raw := query.Get("limit"); raw != "" {
			value, err := strconv.Atoi(raw)
			if err != nil || value < 0 {
				writeJSONError(w, http.StatusBadRequest, "limit must be a non-negative integer")
				return
			}
			opts.Limit = value
		}
		page, err := client.ListSecrets(r.Context(), cfg.Endpoint, cfg.BearerToken, opts)
		if err != nil {
			if errors.Is(err, client.ErrNotFound) {
				secrets.note(false)
				writeSecretsUnavailable(w)
				return
			}
			writeUpstreamError(w, err)
			return
		}
		secrets.note(true)
		writeJSON(w, map[string]any{"secrets": sanitizeSecrets(page.Items), "next_cursor": page.NextCursor})
	})
}

// secretHandler proxies one secret's sealed metadata plus the public
// identifiers of the current vault-key binding (a pure read; absence or
// failure just omits the binding). A 404 here is ambiguous — missing secret
// or missing route — so the capability gate disambiguates: only a cell
// proven to lack /v1/secrets entirely gets the unavailable state.
func secretHandler(cfg Config, secrets *secretsCapability) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secret, err := client.GetSecret(r.Context(), cfg.Endpoint, cfg.BearerToken, r.PathValue("id"))
		if err != nil {
			if errors.Is(err, client.ErrNotFound) {
				if supported, availErr := secrets.available(r.Context(), cfg); availErr == nil && !supported {
					writeSecretsUnavailable(w)
					return
				}
			}
			writeUpstreamError(w, err)
			return
		}
		secrets.note(true)
		out := map[string]any{"secret": sanitizeSecret(*secret)}
		if key, keyErr := client.GetCurrentVaultKey(r.Context(), cfg.Endpoint, cfg.BearerToken); keyErr == nil && key != nil {
			out["vault_key"] = sanitizedVaultKey{
				ID:             key.ID,
				KeyVersion:     key.KeyVersion,
				Algorithm:      key.Algorithm,
				Fingerprint:    key.Fingerprint,
				LifecycleState: key.LifecycleState,
				CreatedAt:      key.CreatedAt,
			}
		}
		writeJSON(w, out)
	})
}

// eventsHandler streams server-sent events by cursor-polling the cell — the
// backend has no push path (ADR 0004). Each connection owns only local state;
// the shared semaphore bounds fan-out. The client seeds the transcript cursor
// with after_sequence (its highest rendered entry) so the stream starts at
// the live edge instead of replaying the whole transcript.
func eventsHandler(cfg Config, sem chan struct{}, factReads *factReadCapability, secrets *secretsCapability) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		transcriptID := query.Get("transcript")
		var includeMessages bool
		if raw := query.Get("messages"); raw != "" {
			value, err := strconv.ParseBool(raw)
			if err != nil {
				writeJSONError(w, http.StatusBadRequest, "messages must be a boolean")
				return
			}
			includeMessages = value
		}
		var includeMemories bool
		if raw := query.Get("memories"); raw != "" {
			value, err := strconv.ParseBool(raw)
			if err != nil {
				writeJSONError(w, http.StatusBadRequest, "memories must be a boolean")
				return
			}
			includeMemories = value
		}
		var includeFacts bool
		if raw := query.Get("facts"); raw != "" {
			value, err := strconv.ParseBool(raw)
			if err != nil {
				writeJSONError(w, http.StatusBadRequest, "facts must be a boolean")
				return
			}
			includeFacts = value
		}
		var includeSecrets bool
		if raw := query.Get("secrets"); raw != "" {
			value, err := strconv.ParseBool(raw)
			if err != nil {
				writeJSONError(w, http.StatusBadRequest, "secrets must be a boolean")
				return
			}
			includeSecrets = value
		}
		var lastSeen int64
		if raw := query.Get("after_sequence"); raw != "" {
			value, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || value < 0 {
				writeJSONError(w, http.StatusBadRequest, "after_sequence must be a non-negative integer")
				return
			}
			lastSeen = value
		}
		// Transcript events carry their cursor as the SSE id, so a browser
		// auto-reconnect resumes at the live edge instead of the originally
		// seeded after_sequence. The header is browser-managed; anything
		// unparsable is ignored rather than rejected.
		if raw := r.Header.Get("Last-Event-ID"); raw != "" {
			if value, err := strconv.ParseInt(raw, 10, 64); err == nil && value > lastSeen {
				lastSeen = value
			}
		}
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
		default:
			writeJSONError(w, http.StatusTooManyRequests, "too many live connections")
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			writeJSONError(w, http.StatusInternalServerError, "streaming unsupported")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		ctx := r.Context()
		ticker := time.NewTicker(cfg.PollInterval)
		defer ticker.Stop()
		for {
			lastSeen = emitEvents(ctx, cfg, factReads, secrets, w, flusher, transcriptID, includeMessages, includeMemories, includeFacts, includeSecrets, lastSeen)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	})
}

// emitEvents sends one poll tick: the observational self digest, the passive
// mailbox pages and the redacted memory and fact pages when requested, then
// every transcript entry after lastSeen, draining full-size upstream pages
// until the live edge (bounded by maxSSEPagesPerTick) so an active transcript
// is never rate-limited to one page per tick. Upstream failures end the tick;
// the next tick retries from the same cursor.
func emitEvents(ctx context.Context, cfg Config, factReads *factReadCapability, secrets *secretsCapability, w io.Writer, flusher http.Flusher, transcriptID string, includeMessages, includeMemories, includeFacts, includeSecrets bool, lastSeen int64) int64 {
	if digest, observational, err := fetchSelf(ctx, cfg); err == nil {
		writeSSE(w, flusher, "self", "", cfg.selfEnvelope(digest, observational))
	}
	if includeMessages {
		emitMessagesEvent(ctx, cfg, w, flusher)
	}
	if includeMemories {
		emitMemoriesEvent(ctx, cfg, w, flusher)
	}
	if includeFacts {
		emitFactsEvent(ctx, cfg, factReads, w, flusher)
	}
	if includeSecrets {
		emitSecretsEvent(ctx, cfg, secrets, w, flusher)
	}
	if transcriptID == "" {
		return lastSeen
	}
	for range maxSSEPagesPerTick {
		if ctx.Err() != nil {
			return lastSeen
		}
		page, err := client.GetTranscriptPage(ctx, cfg.Endpoint, cfg.BearerToken, transcriptID, client.TranscriptPageOptions{
			AfterSequence: lastSeen,
			Limit:         sseTranscriptPageLimit,
			Observational: true,
		})
		if err != nil || len(page.Entries) == 0 {
			return lastSeen
		}
		next := page.NextAfterSequence
		if last := page.Entries[len(page.Entries)-1].Sequence; next < last {
			next = last
		}
		if next <= lastSeen {
			// No forward progress; stop rather than re-emit the same page
			// every tick.
			return lastSeen
		}
		writeSSE(w, flusher, "transcript", strconv.FormatInt(next, 10), page)
		lastSeen = next
		if len(page.Entries) < sseTranscriptPageLimit {
			return lastSeen
		}
	}
	return lastSeen
}

// emitMessagesEvent polls the passive metadata-only mailbox list — never
// :read or :listen, which mutate read-state or consume listen slots — one
// newest-first first page per direction, and emits both pages as one
// "messages" event for the browser's thread grouping. Either upstream
// failure skips the event; the next tick retries.
func emitMessagesEvent(ctx context.Context, cfg Config, w io.Writer, flusher http.Flusher) {
	pages := map[string][]client.Message{}
	for _, direction := range []string{"inbox", "outbox"} {
		page, err := client.ListMessages(ctx, cfg.Endpoint, cfg.BearerToken, client.MessageListOptions{
			Direction: direction,
			Limit:     sseMessagePageLimit,
		})
		if err != nil {
			return
		}
		if page.Messages == nil {
			page.Messages = []client.Message{}
		}
		pages[direction] = stripMessageBodies(page.Messages)
	}
	writeSSE(w, flusher, "messages", "", pages)
}

// emitMemoriesEvent polls the first page of the redacted-by-default broad
// memory list — never include_sensitive — so the memories surface
// live-updates like every other surface. The browser skips identical frames;
// an upstream failure skips the event and the next tick retries.
func emitMemoriesEvent(ctx context.Context, cfg Config, w io.Writer, flusher http.Flusher) {
	page, err := client.ListMemories(ctx, cfg.Endpoint, cfg.BearerToken, client.MemoryListOptions{Limit: sseMemoryPageLimit})
	if err != nil {
		return
	}
	if page.Items == nil {
		page.Items = []client.Memory{}
	}
	writeSSE(w, flusher, "memories", "", page)
}

// emitFactsEvent polls the first page of the observational fact list — never
// include_sensitive, and never the plain list, whose search deliveries are
// ranking-eligible. The capability gate keeps that guarantee on cells that
// predate the observational parameter and would silently run the plain list;
// such a cell — like one that 501s — simply gets no facts tick rather than a
// silently perturbing fallback. Sensitive values are zeroed before the
// frame; the browser skips identical frames, and an upstream failure skips
// the event so the next tick retries.
func emitFactsEvent(ctx context.Context, cfg Config, factReads *factReadCapability, w io.Writer, flusher http.Flusher) {
	if supported, err := factReads.observationalSupported(ctx, cfg); err != nil || !supported {
		return
	}
	facts, err := client.ListFacts(ctx, cfg.Endpoint, cfg.BearerToken, client.FactListOptions{
		Limit:         sseFactPageLimit,
		Observational: true,
	})
	if err != nil {
		return
	}
	writeSSE(w, flusher, "facts", "", map[string]any{"facts": redactSensitiveFacts(facts)})
}

// emitSecretsEvent polls the first page of the sealed-plane metadata list.
// The tick is safe by determination: the sealed plane's list and get reads
// are pure SELECTs — only the field :access POST (which this dashboard never
// calls) records audit and usage — so a 2s poll writes nothing anywhere.
// Every frame passes through the same allow-list projection as the list
// route; a pre-sealed-plane cell 404s once, is memoized, and gets no
// further tick until the negative answer's recheck window lapses.
func emitSecretsEvent(ctx context.Context, cfg Config, secrets *secretsCapability, w io.Writer, flusher http.Flusher) {
	if secrets.knownUnavailable() {
		return
	}
	page, err := client.ListSecrets(ctx, cfg.Endpoint, cfg.BearerToken, client.SecretListOptions{
		Limit:         sseSecretPageLimit,
		IncludeFields: true,
	})
	if err != nil {
		if errors.Is(err, client.ErrNotFound) {
			secrets.note(false)
		}
		return
	}
	secrets.note(true)
	writeSSE(w, flusher, "secrets", "", map[string]any{"secrets": sanitizeSecrets(page.Items), "next_cursor": page.NextCursor})
}

// writeSSE emits one server-sent event; a non-empty id becomes the SSE id
// field browsers replay in Last-Event-ID on reconnect.
func writeSSE(w io.Writer, flusher http.Flusher, event, id string, payload any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	if id != "" {
		if _, err := fmt.Fprintf(w, "id: %s\n", id); err != nil {
			return
		}
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, raw); err != nil {
		return
	}
	flusher.Flush()
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

// writeUpstreamError maps cell read failures onto the local surface: missing
// resources stay 404, upstream input validation stays 400 (browser-supplied
// params the proxy passes through), and everything else is a bad gateway
// (the browser's own authentication was already accepted; 401 here would
// mean the cookie).
func writeUpstreamError(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	switch {
	case errors.Is(err, client.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, client.ErrBadRequest):
		status = http.StatusBadRequest
	}
	writeJSONError(w, status, err.Error())
}
