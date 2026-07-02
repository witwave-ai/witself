// Package server runs the witself-server backend listeners. This first slice
// serves a minimal version endpoint on the API listener, Kubernetes-compatible
// health probes on the health listener, and a single Prometheus "up" metric on
// the metrics listener. Domain behavior is specified under docs/ and lands in
// later slices.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/version"
)

// Config holds the listen addresses for the three witself-server listeners.
type Config struct {
	APIAddr     string // public /v1 API
	HealthAddr  string // Kubernetes liveness/readiness/startup probes
	MetricsAddr string // Prometheus metrics

	// Ready, when set, gates /readyz: it returns 200 only when Ready returns
	// nil, else 503. nil means always-ready. Liveness/startup never gate on it.
	Ready func(context.Context) error

	// AccountID, when set, is surfaced as the account block in /v1/capabilities
	// (the seeded default account on single-account backends).
	AccountID string

	// Login, when set, enables POST /v1/auth/bootstrap to exchange a bootstrap
	// token for an operator token.
	Login LoginFunc

	// Authenticate, when set, enables bearer-token auth (e.g. GET /v1/whoami):
	// it resolves an operator token to its principal.
	Authenticate AuthFunc

	// CreateRealm / ListRealms, when set (with Authenticate), enable the
	// operator-authenticated /v1/realms endpoints, scoped to the caller's account.
	CreateRealm func(ctx context.Context, accountID, name string) (Realm, error)
	ListRealms  func(ctx context.Context, accountID string) ([]Realm, error)

	// CreateAgent / ListAgents, when set, enable POST/GET /v1/realms/{realm}/agents.
	CreateAgent func(ctx context.Context, accountID, realmID, name string) (Agent, error)
	ListAgents  func(ctx context.Context, accountID, realmID string) ([]Agent, error)

	// CreateAgentToken, when set, enables POST /v1/agents/{agent}/tokens to mint a
	// durable agent token (returned once).
	CreateAgentToken func(ctx context.Context, accountID, agentID string) (string, error)

	// CreateOperatorToken, when set, enables POST /v1/operators/self/tokens to mint
	// an additional operator token for the authenticated operator (returned once).
	CreateOperatorToken func(ctx context.Context, accountID, operatorID, displayName string, ttl *time.Duration) (string, *time.Time, error)
}

// LoginFunc exchanges a bootstrap token for an operator token. ok is false when
// the token is invalid or already used (-> 401); a non-nil error is a server
// fault (-> 500).
type LoginFunc func(ctx context.Context, bootstrapToken string) (operatorToken, operatorID string, ok bool, err error)

// AuthFunc resolves a bearer token to its operator principal. ok is false when
// the token is missing/invalid (-> 401); a non-nil error is a server fault.
type AuthFunc func(ctx context.Context, token string) (operatorID, accountID string, ok bool, err error)

// Realm is the API view of a realm.
type Realm struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ErrConflict signals a uniqueness conflict (-> 409). The wiring layer returns
// it (e.g. for a duplicate realm name) without coupling the server to the store.
var ErrConflict = errors.New("conflict")

// ErrNotFound signals a missing resource (-> 404), e.g. a realm not in the account.
var ErrNotFound = errors.New("not found")

// Agent is the API view of an agent.
type Agent struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ConfigFromEnv builds a Config from WITSELF_* env vars, defaulting to the
// canonical ports :8080 (api), :8081 (health), and :9090 (metrics).
func ConfigFromEnv() Config {
	return Config{
		APIAddr:     envOr("WITSELF_API_ADDR", ":8080"),
		HealthAddr:  envOr("WITSELF_HEALTH_ADDR", ":8081"),
		MetricsAddr: envOr("WITSELF_METRICS_ADDR", ":9090"),
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Run binds the three listeners, serves until ctx is cancelled (or a listener
// fails), then shuts them down gracefully.
func Run(ctx context.Context, cfg Config) error {
	defs := []struct {
		name, addr string
		handler    http.Handler
	}{
		{"api", cfg.APIAddr, apiMux(cfg)},
		{"health", cfg.HealthAddr, healthMux(cfg.Ready)},
		{"metrics", cfg.MetricsAddr, metricsMux()},
	}

	type running struct {
		name string
		srv  *http.Server
		ln   net.Listener
	}
	var servers []running
	for _, d := range defs {
		ln, err := net.Listen("tcp", d.addr)
		if err != nil {
			for _, r := range servers {
				_ = r.ln.Close()
			}
			return fmt.Errorf("%s listener %s: %w", d.name, d.addr, err)
		}
		servers = append(servers, running{
			name: d.name,
			srv:  &http.Server{Handler: d.handler, ReadHeaderTimeout: 5 * time.Second},
			ln:   ln,
		})
	}

	errc := make(chan error, len(servers))
	for _, r := range servers {
		fmt.Fprintf(os.Stderr, "witself-server: %s listening on %s\n", r.name, r.ln.Addr())
		go func() {
			if err := r.srv.Serve(r.ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- fmt.Errorf("%s: %w", r.name, err)
			}
		}()
	}

	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-errc:
	}

	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, r := range servers {
		_ = r.srv.Shutdown(shutCtx)
	}
	return runErr
}

func apiMux(cfg Config) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/version", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, "{\"schema_version\":\"witself.v0\",\"version\":%q,\"commit\":%q,\"date\":%q}\n",
			version.Version, version.Commit, version.Date)
	})
	mux.HandleFunc("/v1/capabilities", capabilitiesHandler(cfg.AccountID))
	if cfg.Login != nil {
		mux.HandleFunc("POST /v1/auth/bootstrap", bootstrapLoginHandler(cfg.Login))
	}
	if cfg.Authenticate != nil {
		whoami := whoamiHandler(cfg.Authenticate)
		mux.HandleFunc("GET /v1/whoami", whoami)
		mux.HandleFunc("GET /v1/auth/whoami", whoami)
		if cfg.CreateRealm != nil {
			mux.HandleFunc("POST /v1/realms", createRealmHandler(cfg.Authenticate, cfg.CreateRealm))
		}
		if cfg.ListRealms != nil {
			mux.HandleFunc("GET /v1/realms", listRealmsHandler(cfg.Authenticate, cfg.ListRealms))
		}
		if cfg.CreateAgent != nil {
			mux.HandleFunc("POST /v1/realms/{realm}/agents", createAgentHandler(cfg.Authenticate, cfg.CreateAgent))
		}
		if cfg.ListAgents != nil {
			mux.HandleFunc("GET /v1/realms/{realm}/agents", listAgentsHandler(cfg.Authenticate, cfg.ListAgents))
		}
		if cfg.CreateAgentToken != nil {
			mux.HandleFunc("POST /v1/agents/{agent}/tokens", createAgentTokenHandler(cfg.Authenticate, cfg.CreateAgentToken))
		}
		if cfg.CreateOperatorToken != nil {
			mux.HandleFunc("POST /v1/operators/self/tokens", createOperatorTokenHandler(cfg.Authenticate, cfg.CreateOperatorToken))
		}
	}
	return mux
}

// bootstrapLoginHandler exchanges a bootstrap token (JSON {"bootstrap_token"})
// for an operator token, shown once.
func bootstrapLoginHandler(login LoginFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			BootstrapToken string `json:"bootstrap_token"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.BootstrapToken == "" {
			writeJSONError(w, http.StatusBadRequest, "missing bootstrap_token")
			return
		}
		opTok, opID, ok, err := login(r.Context(), req.BootstrapToken)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if !ok {
			writeJSONError(w, http.StatusUnauthorized, "invalid or already-used bootstrap token")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"schema_version": "witself.v0",
			"operator_token": opTok,
			"operator_id":    opID,
		})
	}
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"schema_version": "witself.v0",
		"error":          msg,
	})
}

// backendInfo, feature, and capabilities describe the bare /v1/capabilities
// document. Like /v1/version it is flat — schema_version at the top level, no
// ok/data envelope — because the meta/discovery endpoints stay bare while the
// domain API uses the standard envelope. The feature map is static while
// subsystems are unbuilt and becomes config-driven as they land. backend.kind
// is a configured value (WITSELF_BACKEND_KIND), never something the server
// infers, and it is advisory: each feature is independently gated, so a
// mislabeled kind unlocks nothing — clients should branch on feature flags.
type backendInfo struct {
	Kind       string `json:"kind"`
	Version    string `json:"version"`
	APIVersion string `json:"api_version"`
}

type feature struct {
	Supported bool   `json:"supported"`
	Reason    string `json:"reason,omitempty"`
}

// accountInfo identifies the deployment's account. On single-account backends
// (local, self-managed) it is the seeded default/root account; it is omitted
// when no database is configured.
type accountInfo struct {
	ID string `json:"id"`
}

type capabilities struct {
	SchemaVersion string             `json:"schema_version"`
	Backend       backendInfo        `json:"backend"`
	Account       *accountInfo       `json:"account,omitempty"`
	Principal     any                `json:"principal"` // null until token auth exists
	Features      map[string]feature `json:"features"`
	Limits        map[string]any     `json:"limits"`
}

func capabilitiesHandler(accountID string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		notImpl := feature{Reason: "not_implemented"}
		caps := capabilities{
			SchemaVersion: "witself.v0",
			Backend: backendInfo{
				Kind:       envOr("WITSELF_BACKEND_KIND", "self-hosted"),
				Version:    version.Version,
				APIVersion: "v1",
			},
			Features: map[string]feature{
				"memories":        notImpl,
				"facts":           notImpl,
				"semantic_recall": notImpl,
				"policies":        notImpl,
				"groups":          notImpl,
				"messaging":       notImpl,
				"audit":           notImpl,
			},
			Limits: map[string]any{},
		}
		if accountID != "" {
			caps.Account = &accountInfo{ID: accountID}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(caps)
	}
}

type principal struct {
	operatorID string
	accountID  string
}

// requireOperator authenticates the bearer token and passes the principal to h,
// or writes 401 (missing/invalid) / 500 (server fault).
func requireOperator(auth AuthFunc, h func(http.ResponseWriter, *http.Request, principal)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, ok := bearerToken(r)
		if !ok {
			writeJSONError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		operatorID, accountID, ok, err := auth(r.Context(), tok)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if !ok {
			writeJSONError(w, http.StatusUnauthorized, "invalid token")
			return
		}
		h(w, r, principal{operatorID: operatorID, accountID: accountID})
	}
}

func bearerToken(r *http.Request) (string, bool) {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(prefix) || !strings.HasPrefix(h, prefix) {
		return "", false
	}
	return strings.TrimSpace(h[len(prefix):]), true
}

// whoamiHandler returns the authenticated operator principal, or 401.
func whoamiHandler(auth AuthFunc) http.HandlerFunc {
	return requireOperator(auth, func(w http.ResponseWriter, _ *http.Request, p principal) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": "witself.v0",
			"principal": map[string]string{
				"kind":        "operator",
				"operator_id": p.operatorID,
				"account_id":  p.accountID,
			},
		})
	})
}

func createRealmHandler(auth AuthFunc, create func(ctx context.Context, accountID, name string) (Realm, error)) http.HandlerFunc {
	return requireOperator(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		var req struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
			writeJSONError(w, http.StatusBadRequest, "missing name")
			return
		}
		realm, err := create(r.Context(), p.accountID, req.Name)
		if errors.Is(err, ErrConflict) {
			writeJSONError(w, http.StatusConflict, "realm already exists")
			return
		}
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "could not create realm")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "realm": realm})
	})
}

func listRealmsHandler(auth AuthFunc, list func(ctx context.Context, accountID string) ([]Realm, error)) http.HandlerFunc {
	return requireOperator(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		realms, err := list(r.Context(), p.accountID)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "could not list realms")
			return
		}
		if realms == nil {
			realms = []Realm{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "realms": realms})
	})
}

func createAgentHandler(auth AuthFunc, create func(ctx context.Context, accountID, realmID, name string) (Agent, error)) http.HandlerFunc {
	return requireOperator(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		var req struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
			writeJSONError(w, http.StatusBadRequest, "missing name")
			return
		}
		agent, err := create(r.Context(), p.accountID, r.PathValue("realm"), req.Name)
		switch {
		case errors.Is(err, ErrNotFound):
			writeJSONError(w, http.StatusNotFound, "realm not found")
			return
		case errors.Is(err, ErrConflict):
			writeJSONError(w, http.StatusConflict, "agent already exists")
			return
		case err != nil:
			writeJSONError(w, http.StatusInternalServerError, "could not create agent")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "agent": agent})
	})
}

func listAgentsHandler(auth AuthFunc, list func(ctx context.Context, accountID, realmID string) ([]Agent, error)) http.HandlerFunc {
	return requireOperator(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		agents, err := list(r.Context(), p.accountID, r.PathValue("realm"))
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "could not list agents")
			return
		}
		if agents == nil {
			agents = []Agent{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "agents": agents})
	})
}

func createAgentTokenHandler(auth AuthFunc, create func(ctx context.Context, accountID, agentID string) (string, error)) http.HandlerFunc {
	return requireOperator(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		agentID := r.PathValue("agent")
		tok, err := create(r.Context(), p.accountID, agentID)
		if errors.Is(err, ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "agent not found")
			return
		}
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "could not create token")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"schema_version": "witself.v0",
			"agent_token":    tok,
			"agent_id":       agentID,
		})
	})
}

func createOperatorTokenHandler(auth AuthFunc, create func(ctx context.Context, accountID, operatorID, displayName string, ttl *time.Duration) (string, *time.Time, error)) http.HandlerFunc {
	return requireOperator(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		var req struct {
			DisplayName string `json:"display_name,omitempty"`
			TTL         string `json:"ttl,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeJSONError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		displayName := strings.TrimSpace(req.DisplayName)

		var ttl *time.Duration
		if req.TTL != "" {
			d, err := time.ParseDuration(req.TTL)
			if err != nil || d <= 0 {
				writeJSONError(w, http.StatusBadRequest, "invalid ttl")
				return
			}
			ttl = &d
		}

		tok, expiresAt, err := create(r.Context(), p.accountID, p.operatorID, displayName, ttl)
		if errors.Is(err, ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "operator not found")
			return
		}
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "could not create token")
			return
		}
		out := map[string]string{
			"schema_version": "witself.v0",
			"operator_token": tok,
			"operator_id":    p.operatorID,
			"display_name":   displayName,
		}
		if expiresAt != nil {
			out["expires_at"] = expiresAt.Format(time.RFC3339)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(out)
	})
}

func healthMux(ready func(context.Context) error) http.Handler {
	mux := http.NewServeMux()
	ok := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	}
	// Liveness and startup never gate on dependencies — a DB blip must not
	// restart the pod, only pull it from the load balancer via readiness.
	mux.HandleFunc("/livez", ok)
	mux.HandleFunc("/startupz", ok)
	mux.HandleFunc("/healthz", ok) // convenience alias
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if ready != nil {
			if err := ready(r.Context()); err != nil {
				w.WriteHeader(http.StatusServiceUnavailable)
				fmt.Fprintf(w, "not ready: %v\n", err)
				return
			}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}

func metricsMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = fmt.Fprint(w,
			"# HELP witself_up 1 if the witself-server process is up.\n"+
				"# TYPE witself_up gauge\n"+
				"witself_up 1\n")
	})
	return mux
}
