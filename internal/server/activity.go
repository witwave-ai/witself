package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/activity"
)

// ActivityQuery selects a bounded UTC hourly reporting window.
type ActivityQuery struct {
	Since, Until time.Time
	Bucket       string
}

// ActivityReport contains recorded quantities and an immutable tracking boundary.
type ActivityReport struct {
	Schema        string       `json:"schema"`
	Catalog       string       `json:"catalog"`
	AccountID     string       `json:"account_id"`
	RealmID       string       `json:"realm_id"`
	AgentID       string       `json:"agent_id"`
	Since         time.Time    `json:"since"`
	Until         time.Time    `json:"until"`
	Bucket        string       `json:"bucket"`
	TrackingSince *time.Time   `json:"tracking_since"`
	Points        []UsagePoint `json:"points"`
	Totals        []UsageTotal `json:"totals"`
	Truncated     bool         `json:"truncated"`
}

// activityContextMux supplies intent only. Authentication and authorization
// still run in the original handlers. Unknown routes explicitly suppress all
// nested operation events; independent memory changes remain transactional.
func activityContextMux(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := activity.WithOperation(r.Context(), requestActivityOperation(r))
		ids, observations := r.Header.Values(activity.RequestIDHeader), r.Header.Values(activity.ObservationHeader)
		// Intent is cooperative telemetry, never a prerequisite for authentication
		// or domain writes (which validate their own Idempotency-Key).
		if len(ids) != 1 || !activity.ValidRequestID(ids[0]) {
			r.Header.Del(activity.RequestIDHeader)
			ids = nil
		}
		if len(observations) != 1 || observations[0] != "1" {
			r.Header.Del(activity.ObservationHeader)
			observations = nil
		}
		if len(ids) == 1 {
			ctx = activity.WithRequestID(ctx, ids[0])
		}
		if len(observations) == 1 {
			ctx = activity.WithObservation(ctx)
		}
		// Preserve ServeMux's Pattern/PathValue writes on the request observed by
		// outer metrics middleware and route-introspection callers.
		*r = *r.WithContext(ctx)
		next.ServeHTTP(w, r)
	})
}
func requestActivityOperation(r *http.Request) string {
	// Bound parsing without echoing any path, query, header or payload value.
	if len(r.URL.Path) > 2048 || len(r.URL.RawQuery) > 16384 {
		return ""
	}
	method, path := r.Method, r.URL.Path
	if method == http.MethodGet {
		switch path {
		case "/v1/facts":
			if strings.TrimSpace(r.URL.Query().Get("predicate")) != "" {
				return "facts.exact"
			}
			return "facts.list"
		case "/v1/fact-occurrences":
			return "facts.upcoming"
		case "/v1/memories":
			return "memories.list"
		case "/v1/transcripts":
			return "transcripts.list"
		case "/v1/messages":
			return "messages.list"
		case "/v1/email":
			return "email.list"
		case "/v1/email/sent":
			return "email.outbox_list"
		case "/v1/secrets":
			return "secrets.inventory"
		}
	}
	if method == http.MethodPost {
		switch path {
		case "/v1/facts":
			return "facts.set"
		case "/v1/fact-candidates":
			return "facts.propose"
		case "/v1/memories":
			return "memories.capture"
		case "/v1/memories:recall":
			return "memories.recall"
		case "/v1/transcripts":
			return "transcripts.create"
		case "/v1/messages":
			return "messages.send"
		case "/v1/email:send":
			return "email.send"
		case "/v1/secrets":
			return "secrets.create"
		}
	}
	if method == http.MethodDelete && path == "/v1/facts" {
		return "facts.delete"
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) < 3 || parts[0] != "v1" {
		return ""
	}
	category, object := parts[1], parts[2]
	id, action, hasAction := strings.Cut(object, ":")
	// Match the existing handlers' normalization, not a more permissive global
	// trim that would turn rejected actions into catalog operations.
	if category == "memories" && hasAction {
		id, action, hasAction = parseMemoryAction(object)
		if !hasAction {
			return ""
		}
	}
	if category == "secrets" {
		id, action, hasAction = strings.Cut(strings.TrimSpace(object), ":")
	}
	if id == "" {
		return ""
	}
	if len(parts) == 3 && hasAction && method == http.MethodPost {
		switch category + ":" + action {
		case "fact-candidates:confirm":
			return "facts.confirm"
		case "fact-candidates:reject":
			return "facts.reject"
		case "memories:forget":
			return "memories.forget"
		case "memories:restore":
			return "memories.restore"
		case "memories:reactivate":
			return "memories.reactivate"
		case "messages:read":
			return "messages.read"
		case "messages:reply":
			return "messages.reply"
		case "email:read":
			return "email.read"
		case "email:reply":
			return "email.reply"
		case "secrets:archive":
			return "secrets.archive"
		case "secrets:restore":
			return "secrets.restore"
		case "secrets:delete":
			return "secrets.delete"
		}
	}
	if len(parts) == 3 && hasAction && method == http.MethodGet && category == "messages" && action == "peek" {
		return "messages.peek"
	}
	if hasAction {
		return ""
	}
	if len(parts) == 3 {
		switch method + " " + category {
		case "GET memories":
			return "memories.get"
		case "GET transcripts":
			return "transcripts.page"
		case "GET secrets":
			return "secrets.show"
		case "PATCH memories":
			return "memories.adjust"
		case "DELETE memories":
			return "memories.delete"
		case "DELETE facts":
			return "facts.delete"
		}
	}
	if len(parts) == 4 && parts[3] != "" {
		switch method + " " + category + "/" + parts[3] {
		case "GET facts/history":
			return "facts.history"
		case "GET memories/history":
			return "memories.history"
		case "POST memories/supersede":
			return "memories.supersede"
		case "POST transcripts/entries", "POST transcripts/entries:batch":
			return "transcripts.append"
		case "POST memory-curation-runs/apply":
			return "memories.curation_apply"
		}
		if method == http.MethodGet && category == "email" && object == "sent" {
			return "email.outbox_detail"
		}
	}
	if len(parts) == 5 && method == http.MethodPost && category == "secrets" && parts[3] == "fields" && strings.HasSuffix(strings.TrimSpace(parts[4]), ":access") {
		return "secrets.access"
	}
	return ""
}
func activityHandler(auth PrincipalAuthFunc, get func(context.Context, DomainPrincipal, ActivityQuery) (ActivityReport, error)) http.HandlerFunc {
	return requireDomainPrincipal(auth, func(w http.ResponseWriter, r *http.Request, p DomainPrincipal) {
		setAuthenticatedNoStoreDefault(w)
		if r.Method != http.MethodGet {
			writeJSONError(w, http.StatusMethodNotAllowed, "activity requires GET")
			return
		}
		if p.Kind != PrincipalKindAgent {
			writeJSONError(w, http.StatusForbidden, "only an agent token may read activity")
			return
		}
		if len(r.URL.RawQuery) > 256 {
			writeJSONError(w, http.StatusBadRequest, "invalid activity query")
			return
		}
		params, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid activity query")
			return
		}
		query := ActivityQuery{Bucket: "hour"}
		for k, vs := range params {
			if len(vs) != 1 {
				err = ErrBadInput
				break
			}
			switch k {
			case "since":
				query.Since, err = time.Parse(time.RFC3339, vs[0])
			case "until":
				query.Until, err = time.Parse(time.RFC3339, vs[0])
			case "group_by":
				if vs[0] != "hour" {
					err = ErrBadInput
				}
			default:
				err = ErrBadInput
			}
			if err != nil {
				break
			}
		}
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid activity query")
			return
		}
		report, err := get(activity.WithObservation(r.Context()), p, query)
		switch {
		case errors.Is(err, ErrBadInput):
			writeJSONError(w, http.StatusBadRequest, "invalid activity query")
			return
		case errors.Is(err, ErrForbidden):
			writeJSONError(w, http.StatusForbidden, "activity access forbidden")
			return
		case err != nil:
			writeJSONError(w, http.StatusInternalServerError, "could not read activity")
			return
		}
		if report.Truncated || report.Schema != activity.Schema || report.Catalog != activity.CatalogVersion {
			writeJSONError(w, http.StatusInternalServerError, "invalid activity report")
			return
		}
		if report.Points == nil {
			report.Points = []UsagePoint{}
		}
		if report.Totals == nil {
			report.Totals = []UsageTotal{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"activity": report})
	})
}
