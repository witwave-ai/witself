package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Resource is the closed set of passive console projections. Sensitive fact
// reveals and message body previews require their separate, explicit methods.
type Resource uint8

const (
	ResourceSelf Resource = iota + 1
	ResourceThemes
	ResourcePreferences
	ResourceTranscripts
	ResourceTranscript
	ResourceMemories
	ResourceMemory
	ResourceMemoryHistory
	ResourceFacts
	ResourceFactHistory
	ResourceMessages
	ResourceEmailAddress
	ResourceEmailStatus
	ResourceEmailReceived
	ResourceEmailSent
	ResourceSecrets
	ResourceSecret
)

// ReadRequest addresses a console resource, never an HTTP method or URL.
// ID is required only for a single resource or its history. Query accepts
// only that resource's existing handler filters; only tag may repeat.
type ReadRequest struct {
	Resource Resource
	ID       string
	Query    url.Values
}

// ReaderError contains only a status and an optional, allow-listed Code:
// feature_not_enabled, unavailable, or unenrolled. Error never includes cell
// response text, request values, endpoint details, or credentials. Status is
// the console handler's status (or a local validation/collection failure).
type ReaderError struct {
	Status int
	Code   string
}

func (*ReaderError) Error() string { return "dashboard reader request failed" }

const (
	readerTimeout          = 10 * time.Second
	maxReaderResponseBytes = 8 * 1024 * 1024
	maxReaderIDBytes       = 256
	maxReaderStringBytes   = 4096
	maxReaderQueryBytes    = 16 * 1024
	maxReaderTags          = 100
)

// Reader invokes private console handlers in process, with no listener,
// registration, session, registry, or polling loop. It shares capability state
// across calls just as Register does. Construct it with NewReader; do not copy.
type Reader struct {
	cfg       Config
	factReads factReadCapability
	secrets   secretsCapability
}

// NewReader validates the fixed cell endpoint without contacting it. Endpoint
// must be an HTTP(S) origin, optionally ending in a slash, with no userinfo,
// query, or fragment. AccessToken is neither required nor retained. The caller
// owns selecting the endpoint and supplying the agent's bearer credential.
func NewReader(cfg Config) (*Reader, error) {
	u, err := url.Parse(cfg.Endpoint)
	if !readerString(cfg.Endpoint, maxReaderStringBytes) || err != nil || u == nil ||
		(u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Hostname() == "" ||
		u.User != nil || u.Opaque != "" || (u.Path != "" && u.Path != "/") ||
		u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || strings.Contains(cfg.Endpoint, "#") ||
		strings.ContainsRune(cfg.Endpoint, '\\') || strings.ContainsFunc(cfg.Endpoint, unicode.IsSpace) {
		return nil, &ReaderError{Status: http.StatusBadRequest}
	}
	if port := u.Port(); strings.HasSuffix(u.Host, ":") || port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, &ReaderError{Status: http.StatusBadRequest}
		}
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaultPollInterval
	}
	cfg.AccessToken = ""
	return &Reader{cfg: cfg}, nil
}

// Read returns the existing handler's JSON projection. Every call has a
// context budget of at most ten seconds and retains at most 8 MiB of projected
// JSON. This bounds projection retention only: the unchanged shared client
// decodes upstream responses, and handlers marshal projections, before Write.
// It is not an upstream decoding or total process-memory bound. Transcript
// inventories and fact histories have no handler pagination option.
func (r *Reader) Read(ctx context.Context, in ReadRequest) (json.RawMessage, error) {
	if r == nil || r.cfg.Endpoint == "" {
		return nil, &ReaderError{Status: http.StatusBadRequest}
	}
	var handler http.Handler
	var path, keys string
	var needsID bool
	limit := 100
	switch in.Resource {
	case ResourceSelf:
		handler, path = selfHandler(r.cfg), "/api/self"
	case ResourceThemes:
		handler, path = http.HandlerFunc(themesHandler), "/api/themes"
	case ResourcePreferences:
		handler, path = prefsHandler(r.cfg), "/api/prefs"
	case ResourceTranscripts:
		handler, path = transcriptsHandler(r.cfg), "/api/transcripts"
	case ResourceTranscript:
		handler, path, needsID = transcriptPageHandler(r.cfg), "/api/transcripts/", true
		keys, limit = "after_sequence limit tail", 500
	case ResourceMemories:
		handler, path = memoriesHandler(r.cfg), "/api/memories"
		keys = "state kind tag cursor limit"
	case ResourceMemory:
		handler, path, needsID = memoryHandler(r.cfg), "/api/memories/", true
	case ResourceMemoryHistory:
		handler, path, needsID = memoryHistoryHandler(r.cfg), "/api/memories/", true
		keys = "cursor limit"
	case ResourceFacts:
		handler, path = factsHandler(r.cfg, &r.factReads), "/api/facts"
		keys = "subject predicate_prefix limit"
	case ResourceFactHistory:
		handler, path, needsID = factHistoryHandler(r.cfg, &r.factReads), "/api/facts/", true
		keys = "subject predicate"
	case ResourceMessages:
		handler, path = messagesHandler(r.cfg), "/api/messages"
		keys = "direction from thread_id kind cursor unread limit"
	case ResourceEmailAddress:
		handler, path = agentEmailAddressHandler(r.cfg), "/api/email/address"
	case ResourceEmailStatus:
		handler, path = agentEmailStatusHandler(r.cfg), "/api/email/status"
	case ResourceEmailReceived:
		handler, path = agentEmailsHandler(r.cfg), "/api/email"
		keys = "unread unacked limit"
	case ResourceEmailSent:
		handler, path = agentEmailSentHandler(r.cfg), "/api/email/sent"
		keys = "limit"
	case ResourceSecrets:
		handler, path = secretsHandler(r.cfg, &r.secrets), "/api/secrets"
		keys = "lifecycle cursor limit"
	case ResourceSecret:
		handler, path, needsID = secretHandler(r.cfg, &r.secrets), "/api/secrets/", true
	default:
		return nil, &ReaderError{Status: http.StatusBadRequest}
	}
	if (needsID && !readerID(in.ID)) || (!needsID && in.ID != "") {
		return nil, &ReaderError{Status: http.StatusBadRequest}
	}
	query, ok := readerQuery(in.Query, keys, limit)
	if !ok {
		return nil, &ReaderError{Status: http.StatusBadRequest}
	}
	if needsID {
		path += url.PathEscape(in.ID)
		if in.Resource == ResourceMemoryHistory || in.Resource == ResourceFactHistory {
			path += "/history"
		}
	}
	return r.invoke(ctx, handler, http.MethodGet, path, in.ID, query, nil)
}

// RevealFact is an explicit exact lookup, including sensitive values. It
// preserves the console's intentional plain-read fallback on old cells;
// unlike broad reads, that fallback can record one delivery usage.
func (r *Reader) RevealFact(ctx context.Context, subject, predicate string) (json.RawMessage, error) {
	if r == nil || !readerString(subject, maxReaderStringBytes) || subject == "" ||
		!readerString(predicate, maxReaderStringBytes) || predicate == "" {
		return nil, &ReaderError{Status: http.StatusBadRequest}
	}
	return r.invoke(ctx, factRevealHandler(r.cfg), http.MethodGet, "/api/fact", "",
		url.Values{"subject": {subject}, "predicate": {predicate}}, nil)
}

// PreviewMessage uses the console's recipient-only peek, including its
// identity check and body size bound, and never falls back to a read.
func (r *Reader) PreviewMessage(ctx context.Context, id string) (json.RawMessage, error) {
	if r == nil || !readerID(id) || !messagePreviewIDPattern.MatchString(id) {
		return nil, &ReaderError{Status: http.StatusBadRequest}
	}
	return r.invoke(ctx, messageBodyHandler(r.cfg), http.MethodGet,
		"/api/messages/"+url.PathEscape(id)+"/body", id, nil, nil)
}

// StoreTheme is the sole mutation: the exact validated console preferences
// schema, forwarded by prefsHandler to the agent's dedicated preferences row.
func (r *Reader) StoreTheme(ctx context.Context, theme string) (json.RawMessage, error) {
	if r == nil || len(theme) > maxPrefsThemeBytes || !prefsThemePattern.MatchString(theme) {
		return nil, &ReaderError{Status: http.StatusBadRequest}
	}
	body, err := json.Marshal(struct {
		Prefs prefsDocument `json:"prefs"`
	}{Prefs: prefsDocument{Schema: prefsSchema, Theme: theme}})
	if err != nil {
		return nil, &ReaderError{Status: http.StatusBadRequest}
	}
	return r.invoke(ctx, prefsHandler(r.cfg), http.MethodPut, "/api/prefs", "", nil, body)
}

func readerString(value string, max int) bool {
	return len(value) <= max && utf8.ValidString(value) &&
		!strings.ContainsFunc(value, unicode.IsControl)
}

func readerID(id string) bool {
	return id != "" && id != "." && id != ".." && readerString(id, maxReaderIDBytes) &&
		!strings.ContainsAny(id, "/\\%?#:") && !strings.ContainsFunc(id, unicode.IsSpace)
}

func readerQuery(in url.Values, keys string, maxLimit int) (url.Values, bool) {
	out := make(url.Values)
	allowed := strings.Fields(keys)
	total := 0
	for key, values := range in {
		if !slices.Contains(allowed, key) ||
			len(values) == 0 || (key != "tag" && len(values) != 1) || len(values) > maxReaderTags {
			return nil, false
		}
		for _, value := range values {
			total += len(key) + len(value)
			if total > maxReaderQueryBytes || !readerString(value, maxReaderStringBytes) {
				return nil, false
			}
			switch key {
			case "limit":
				n, err := strconv.Atoi(value)
				if err != nil || n < 1 || n > maxLimit {
					return nil, false
				}
			case "after_sequence":
				n, err := strconv.ParseInt(value, 10, 64)
				if err != nil || n < 0 {
					return nil, false
				}
			case "tail", "unread", "unacked":
				if _, err := strconv.ParseBool(value); err != nil {
					return nil, false
				}
			}
			out.Add(key, value)
		}
	}
	return out, true
}

func (r *Reader) invoke(ctx context.Context, handler http.Handler, method, path, id string, query url.Values, body []byte) (json.RawMessage, error) {
	if r.cfg.Endpoint == "" || ctx == nil ||
		(method != http.MethodGet && (method != http.MethodPut || path != "/api/prefs")) {
		return nil, &ReaderError{Status: http.StatusBadRequest}
	}
	ctx, cancel := context.WithTimeout(ctx, readerTimeout)
	defer cancel()
	if ctx.Err() != nil {
		return nil, &ReaderError{Status: http.StatusGatewayTimeout}
	}
	target := path
	if len(query) != 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return nil, &ReaderError{Status: http.StatusBadRequest}
	}
	req.SetPathValue("id", id)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	w := &readerResponse{header: make(http.Header)}
	handler.ServeHTTP(w, req)
	if ctx.Err() != nil {
		return nil, &ReaderError{Status: http.StatusGatewayTimeout}
	}
	if w.overflow || !json.Valid(w.body) {
		return nil, &ReaderError{Status: http.StatusBadGateway}
	}
	if w.status < 200 || w.status >= 300 {
		return nil, &ReaderError{Status: w.status, Code: readerErrorCode(w.body)}
	}
	return json.RawMessage(w.body), nil
}

// Only these closed, existing handler availability messages become codes.
// Generic handlers erase typed upstream codes; do not guess from arbitrary
// upstream prose or expose that prose to recover the missing classification.
func readerErrorCode(body []byte) string {
	var envelope struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &envelope) != nil {
		return ""
	}
	switch envelope.Error {
	case "inbound email is not enabled on this account", "outbound email is not enabled on this account", "feature not enabled":
		return "feature_not_enabled"
	case "agent is not enrolled in receive-only email":
		return "unenrolled"
	case "cell does not serve receive-only agent email", "cell does not serve outbound agent email",
		"cell does not serve the sealed secrets plane", "cell does not support observational fact reads":
		return "unavailable"
	default:
		return ""
	}
}

// readerResponse is deliberately only a ResponseWriter: no streaming,
// hijacking, or flushing. Once overflowed it discards all collected bytes.
type readerResponse struct {
	header   http.Header
	status   int
	body     []byte
	overflow bool
}

func (w *readerResponse) Header() http.Header { return w.header }

func (w *readerResponse) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *readerResponse) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.overflow || len(p) > maxReaderResponseBytes-len(w.body) {
		w.body = nil
		w.overflow = true
		return 0, io.ErrShortWrite
	}
	if size := len(w.body) + len(p); size > cap(w.body) {
		capacity := min(maxReaderResponseBytes, max(size, 2*cap(w.body)))
		next := make([]byte, len(w.body), capacity)
		copy(next, w.body)
		w.body = next
	}
	w.body = append(w.body, p...)
	return len(p), nil
}
