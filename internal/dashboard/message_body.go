package dashboard

import (
	"errors"
	"net/http"
	"regexp"

	"github.com/witwave-ai/witself/internal/client"
)

// Match the existing messaging body limit without importing the store into
// this presentation boundary. The limit is bytes, not Unicode code points.
const maxMessagePreviewBodyBytes = 64 * 1024

var messagePreviewIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

// messageBodyHandler is an explicit, recipient-only observation. Neither the
// passive list nor the event stream calls it. PeekMessage uses the configured
// agent's credentials and never falls back to a state-changing read.
func messageBodyHandler(cfg Config) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		id := r.PathValue("id")
		if !messagePreviewIDPattern.MatchString(id) || r.URL.RawQuery != "" || r.URL.ForceQuery {
			writeJSONError(w, http.StatusBadRequest, "invalid message preview request")
			return
		}
		message, err := client.PeekMessage(r.Context(), cfg.Endpoint, cfg.BearerToken, id)
		if err != nil {
			status := http.StatusBadGateway
			if errors.Is(err, client.ErrNotFound) {
				status = http.StatusNotFound
			} else if errors.Is(err, client.ErrForbidden) {
				status = http.StatusForbidden
			}
			// Upstream error text can contain content or connection details.
			// A missing route/message is local to this preview, not a global
			// capability answer and never authority to call another endpoint.
			writeJSONError(w, status, "message body is unavailable")
			return
		}
		if message.ID != id || len(message.Body) > maxMessagePreviewBodyBytes {
			writeJSONError(w, http.StatusBadGateway, "message body is unavailable")
			return
		}
		// The existing client wire type omits empty bodies. Preserve its empty
		// string as a valid preview, while projecting no payload or metadata.
		writeJSON(w, struct {
			Body string `json:"body"`
		}{Body: message.Body})
	})
}
