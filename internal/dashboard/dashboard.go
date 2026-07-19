// Package dashboard serves the local, read-only, per-agent web dashboard
// mounted by `witself dashboard serve` (ADR 0004). Every route is a thin
// authenticated proxy over the public /v1 read API using the agent's own
// token: self digests and transcripts use observational reads, messages use
// the passive metadata-only list, broad memory reads stay redacted, and the
// avatar SVG passes the same canonical sanitizer-and-hash gate as
// `witself self card`. The package mounts routes onto a caller-owned mux
// (the cpserver.Register idiom); the binary owns the listener and lifecycle.
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
	mux.Handle("GET /api/events", secure(cfg, session, eventsHandler(cfg, sem)))
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

// eventsHandler streams server-sent events by cursor-polling the cell — the
// backend has no push path (ADR 0004). Each connection owns only local state;
// the shared semaphore bounds fan-out. The client seeds the transcript cursor
// with after_sequence (its highest rendered entry) so the stream starts at
// the live edge instead of replaying the whole transcript.
func eventsHandler(cfg Config, sem chan struct{}) http.Handler {
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
			lastSeen = emitEvents(ctx, cfg, w, flusher, transcriptID, includeMessages, includeMemories, lastSeen)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	})
}

// emitEvents sends one poll tick: the observational self digest, the passive
// mailbox pages and the redacted memory page when requested, then every
// transcript entry after lastSeen, draining full-size upstream pages until
// the live edge (bounded by maxSSEPagesPerTick) so an active transcript is
// never rate-limited to one page per tick. Upstream failures end the tick;
// the next tick retries from the same cursor.
func emitEvents(ctx context.Context, cfg Config, w io.Writer, flusher http.Flusher, transcriptID string, includeMessages, includeMemories bool, lastSeen int64) int64 {
	if digest, observational, err := fetchSelf(ctx, cfg); err == nil {
		writeSSE(w, flusher, "self", "", cfg.selfEnvelope(digest, observational))
	}
	if includeMessages {
		emitMessagesEvent(ctx, cfg, w, flusher)
	}
	if includeMemories {
		emitMemoriesEvent(ctx, cfg, w, flusher)
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
