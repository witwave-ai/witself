package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/activity"
)

// AgentActivityInput is one privacy-safe, locally ordered runtime hook
// observation. The backend uses EventID and EventOccurredAt only to make its
// per-runtime/location projection retry-safe; it stamps LastActivityAt itself.
type AgentActivityInput struct {
	Runtime         string    `json:"runtime"`
	LocationID      string    `json:"location_id"`
	Location        string    `json:"location"`
	Event           string    `json:"event"`
	EventID         string    `json:"event_id"`
	EventOccurredAt time.Time `json:"event_occurred_at"`
}

// AgentActivity is the public agent activity projection. It intentionally
// carries neither availability nor the internal ordering/idempotency fields.
type AgentActivity struct {
	LastActivityAt time.Time `json:"last_activity_at"`
	LastRuntime    string    `json:"last_runtime"`
	LastLocation   string    `json:"last_location"`
	LastEvent      string    `json:"last_event"`
}

// TouchAgentActivity records one hook observation using the authenticated
// agent's own identity.
func TouchAgentActivity(ctx context.Context, endpoint, token string, in AgentActivityInput) (AgentActivity, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return AgentActivity{}, err
	}
	var out struct {
		Activity AgentActivity `json:"activity"`
	}
	url := strings.TrimRight(endpoint, "/") + "/v1/self/activity"
	if err := doJSON(ctx, http.MethodPost, url, token, body, &out); err != nil {
		return AgentActivity{}, err
	}
	return out.Activity, nil
}

type activityRetryScopeKey struct{}

// WithActivityRetryScope gives one logical CLI/MCP invocation a stable seed.
// Exact HTTP retries reuse an ID; distinct routes, queries, or bodies do not.
// Scope only one invocation, never a session or a background service.
func WithActivityRetryScope(ctx context.Context) context.Context {
	if _, ok := ctx.Value(activityRetryScopeKey{}).(string); ok {
		return ctx
	}
	return context.WithValue(ctx, activityRetryScopeKey{}, activity.NewRequestID())
}
func activityRetryID(ctx context.Context, method, url string, body []byte) string {
	seed, _ := ctx.Value(activityRetryScopeKey{}).(string)
	if seed == "" {
		return activity.NewRequestID()
	}
	h := sha256.New()
	for _, part := range []string{seed, method, url} {
		_, _ = fmt.Fprintf(h, "%d:%s", len(part), part)
	}
	_, _ = fmt.Fprintf(h, "%d:", len(body))
	_, _ = h.Write(body)
	return hex.EncodeToString(h.Sum(nil)[:24])
}
