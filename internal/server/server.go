// Package server runs the witself-server backend listeners. This first slice
// serves a minimal version endpoint on the API listener, Kubernetes-compatible
// health probes on the health listener, and a single Prometheus "up" metric on
// the metrics listener. Domain behavior is specified under docs/ and lands in
// later slices.
package server

import (
	"context"
	"crypto/subtle"
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
	DeleteRealm func(ctx context.Context, accountID, realmID string) error

	// CreateAgent / ListAgents, when set, enable POST/GET /v1/realms/{realm}/agents.
	CreateAgent func(ctx context.Context, accountID, realmID, name string) (Agent, error)
	ListAgents  func(ctx context.Context, accountID, realmID string) ([]Agent, error)
	DeleteAgent func(ctx context.Context, accountID, realmID, agentID string) error

	// CreateAgentToken, when set, enables POST /v1/agents/{agent}/tokens to mint a
	// durable agent token (returned once, with the agent's name for client-side
	// file naming).
	CreateAgentToken func(ctx context.Context, accountID, agentID string) (token, tokenID, agentName string, err error)

	// CreateOperatorToken, when set, enables POST /v1/operators/self/tokens to mint
	// an additional operator token for the authenticated operator (returned once).
	CreateOperatorToken func(ctx context.Context, accountID, operatorID, displayName string, ttl *time.Duration) (token string, tokenID string, expiresAt *time.Time, err error)

	// Operator lifecycle endpoints manage named operator principals.
	ListOperators  func(ctx context.Context, accountID string) ([]Operator, error)
	CreateOperator func(ctx context.Context, accountID, displayName, tokenDisplayName string, ttl *time.Duration) (Operator, string, *time.Time, error)
	DeleteOperator func(ctx context.Context, accountID, actorOperatorID, targetOperatorID string) error

	// RevokeToken, when set, enables POST /v1/tokens/{token}:revoke.
	RevokeToken func(ctx context.Context, accountID, tokenID string) error

	// CloseAccount, when set, enables POST /v1/account:close — the permanent,
	// owner-only tombstone of the authenticated operator's account.
	CloseAccount func(ctx context.Context, accountID, operatorID, reason string) error

	// GetAccount, when set, enables GET /v1/account — the authenticated
	// operator's account lifecycle record. Reachable at any status: a pending
	// account checks here whether its activation gates have passed.
	GetAccount func(ctx context.Context, accountID string) (AccountRecord, error)

	// RenameAccount, when set, enables POST /v1/account:rename — the
	// owner-only change of the account's server-side display name.
	RenameAccount func(ctx context.Context, accountID, operatorID, displayName string) error

	// ProvisionToken + ProvisionAccount, when both set, enable POST /v1/accounts:
	// the control-plane -> cell trust link that creates a new (non-default)
	// account with its root operator and a one-shot bootstrap token. Self-hosted
	// cells never set the token, so the route is not even mounted there.
	ProvisionToken   string
	ProvisionAccount func(ctx context.Context, email, displayName string) (ProvisionedAccount, error)

	// ReapAccount, when set (with the provisioning pair), enables POST
	// /v1/accounts/{id}:reap — the control plane's expiry sweep closing an
	// account that never activated. Same trust link and same cloud-only gating
	// as provisioning; the only-if-pending guard lives in the implementation,
	// which returns ErrConflict for an activated account.
	ReapAccount func(ctx context.Context, accountID string) (reaped bool, err error)

	// ActivateAccount, when set (with the provisioning pair), enables POST
	// /v1/accounts/{id}:activate — the control plane flipping a pending
	// account to active after its verification gate passes. The mirror of
	// ReapAccount: only-if-pending, idempotent when already active
	// (activated=false), ErrConflict when the account is closed or otherwise
	// ineligible.
	ActivateAccount func(ctx context.Context, accountID string) (activated bool, err error)

	// AccountContact, when set (with the provisioning pair), enables POST
	// /v1/accounts/{id}:contact — the control plane reading an account's
	// contact email when no operator token exists (the recovery request).
	// Read-only, machine-authorized.
	AccountContact func(ctx context.Context, accountID string) (AccountRecord, error)

	// RecoverAccount, when set (with the provisioning pair), enables POST
	// /v1/accounts/{id}:recover — after the control plane verifies inbox
	// control, the cell rotates the root operator's credentials: all live
	// root tokens die and a fresh one-shot bootstrap token is returned.
	// Only-if-active; ErrConflict otherwise.
	RecoverAccount func(ctx context.Context, accountID string) (ProvisionedAccount, error)

	// UpdateAccountEmail, when set (with the provisioning pair), enables POST
	// /v1/accounts/{id}:update-email — the control plane committing an email
	// change after proving the new inbox can receive. The acting operator id
	// travels in the body; the store enforces owner-only and active-only.
	// ErrNotAccountOwner -> 403, ErrConflict -> 409. Also serves the undo
	// variant ({undo:true, expected_current, new_email}) — the control plane
	// applies it after the 48-hour undo link is clicked.
	UpdateAccountEmail func(ctx context.Context, accountID, operatorID, newEmail string) error
	UndoAccountEmail   func(ctx context.Context, accountID, expectedCurrent, newEmail string) error

	// SuspendAccountOwner, when set, enables POST /v1/account:suspend — the
	// owner freezing every write on the account (reads and status still
	// work). ErrConflict when the account is not active.
	SuspendAccountOwner func(ctx context.Context, accountID, operatorID, reason string) error

	// SuspendAccountSystem, when set (with the provisioning pair), enables
	// POST /v1/accounts/{id}:suspend — machine-initiated suspension with a
	// category ("evacuation"). Idempotent; preserves an existing suspension's
	// category. ErrConflict for pending accounts.
	SuspendAccountSystem func(ctx context.Context, accountID, category, reason string) error

	// StreamAccountExport, when set (with the provisioning pair), enables
	// POST /v1/accounts/{id}:export — streaming the account's complete
	// logical archive. Refuses unless the account is suspended or closed
	// (ErrConflict): the write freeze is what makes the snapshot consistent.
	// Errors after the first byte can only be signaled in-stream; the
	// archive's trailing checksums entry is the truncation detector.
	StreamAccountExport func(ctx context.Context, accountID string, w io.Writer) error

	// ResumeAccountOwner, when set, enables POST /v1/account:resume — the
	// owner un-freezing a self-suspended account. Refuses to un-suspend a
	// fleet-admin/migration/etc. suspension (ErrCannotSelfResume -> 403); a
	// not-suspended account gets ErrConflict.
	ResumeAccountOwner func(ctx context.Context, accountID, operatorID string) error
}

// AccountRecord is the API view of an account's lifecycle record.
type AccountRecord struct {
	ID              string     `json:"id"`
	Email           string     `json:"email,omitempty"`
	DisplayName     string     `json:"display_name,omitempty"`
	Status          string     `json:"status"`
	CreatedAt       time.Time  `json:"created_at"`
	ClosedAt        *time.Time `json:"closed_at,omitempty"`
	ClosedReason    string     `json:"closed_reason,omitempty"`
	SuspendedAt     *time.Time `json:"suspended_at,omitempty"`
	SuspendedFor    string     `json:"suspended_for,omitempty"`
	SuspendedReason string     `json:"suspended_reason,omitempty"`
}

// ProvisionedAccount is the API view of a freshly provisioned account. The
// bootstrap token is returned exactly once; the new owner exchanges it for an
// operator token via the ordinary POST /v1/auth/bootstrap.
type ProvisionedAccount struct {
	AccountID      string `json:"account_id"`
	OperatorID     string `json:"operator_id"`
	Email          string `json:"email"`
	Status         string `json:"status"`
	BootstrapToken string `json:"bootstrap_token"`
}

// LoginFunc exchanges a bootstrap token for an operator token. ok is false when
// the token is invalid or already used (-> 401); a non-nil error is a server
// fault (-> 500).
type LoginFunc func(ctx context.Context, bootstrapToken string) (operatorToken, operatorID string, ok bool, err error)

// AuthFunc resolves a bearer token to its operator principal, including the
// account's lifecycle status ("pending"/"active"/"closed") — status is part of
// the principal so handlers can gate on it without a second lookup. ok is
// false when the token is missing/invalid (-> 401); a non-nil error is a
// server fault.
type AuthFunc func(ctx context.Context, token string) (operatorID, accountID, accountStatus string, ok bool, err error)

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

// ErrNotAccountOwner signals an owner-only action attempted by a non-owner (-> 403).
var ErrNotAccountOwner = errors.New("only the account owner may do this")

// ErrEmailChangedSinceUndo signals a stale undo link — the current email no
// longer matches what the undo token snapshotted (a subsequent legitimate
// change ran first) so the revert must not roll back the newer state.
var ErrEmailChangedSinceUndo = errors.New("email has changed since this undo was issued")

// ErrAccountNotSuspended signals a resume attempt against an account that is
// not currently suspended.
var ErrAccountNotSuspended = errors.New("account is not suspended")

// ErrCannotSelfResume signals the owner trying to un-suspend a suspension
// they did not initiate (fleet-admin, migration, etc.).
var ErrCannotSelfResume = errors.New("this suspension is not owner-resumable")

// ErrAccountNotActive signals a store-level refusal to mint credentials for a
// non-active account (-> 403). requireOperator normally refuses first; this
// surfaces the race where a close commits while the request is in flight.
var ErrAccountNotActive = errors.New("account is not active")

// ErrCannotCloseDefault signals an attempt to close the deployment's seeded
// default account (-> 403).
var ErrCannotCloseDefault = errors.New("the default account cannot be closed")

// Agent is the API view of an agent.
type Agent struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// OperatorToken is safe token metadata shown in operator listings.
type OperatorToken struct {
	ID          string     `json:"id"`
	DisplayName string     `json:"display_name"`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
}

// Operator is the API view of a human/admin operator principal.
type Operator struct {
	ID          string          `json:"id"`
	DisplayName string          `json:"display_name"`
	Role        string          `json:"role"`
	IsRoot      bool            `json:"is_root"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
	Tokens      []OperatorToken `json:"tokens"`
}

var (
	// ErrLastOperator signals a rejected delete that would leave the account
	// without any live operator.
	ErrLastOperator = errors.New("last operator")
	// ErrCannotDeleteSelf signals a rejected self-delete.
	ErrCannotDeleteSelf = errors.New("cannot delete self")
	// ErrCannotDeleteRoot signals a rejected root operator delete.
	ErrCannotDeleteRoot = errors.New("cannot delete root operator")
)

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
		_, _ = fmt.Fprintf(w, "{\"schema_version\":\"witself.v0\",\"version\":%q,\"commit\":%q,\"date\":%q}\n",
			version.Version, version.Commit, version.Date)
	})
	mux.HandleFunc("/v1/capabilities", capabilitiesHandler(cfg.AccountID))
	if cfg.Login != nil {
		mux.HandleFunc("POST /v1/auth/bootstrap", bootstrapLoginHandler(cfg.Login))
	}
	if cfg.ProvisionToken != "" && cfg.ProvisionAccount != nil {
		mux.HandleFunc("POST /v1/accounts", provisionAccountHandler(cfg.ProvisionToken, cfg.ProvisionAccount))
		if cfg.ReapAccount != nil || cfg.ActivateAccount != nil || cfg.AccountContact != nil || cfg.RecoverAccount != nil || cfg.UpdateAccountEmail != nil || cfg.SuspendAccountSystem != nil || cfg.StreamAccountExport != nil {
			mux.HandleFunc("POST /v1/accounts/", accountLifecycleHandler(cfg))
		}
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
		if cfg.DeleteRealm != nil {
			mux.HandleFunc("DELETE /v1/realms/{realm}", deleteRealmHandler(cfg.Authenticate, cfg.DeleteRealm))
		}
		if cfg.CreateAgent != nil {
			mux.HandleFunc("POST /v1/realms/{realm}/agents", createAgentHandler(cfg.Authenticate, cfg.CreateAgent))
		}
		if cfg.ListAgents != nil {
			mux.HandleFunc("GET /v1/realms/{realm}/agents", listAgentsHandler(cfg.Authenticate, cfg.ListAgents))
		}
		if cfg.DeleteAgent != nil {
			mux.HandleFunc("DELETE /v1/realms/{realm}/agents/{agent}", deleteAgentHandler(cfg.Authenticate, cfg.DeleteAgent))
		}
		if cfg.CreateAgentToken != nil {
			mux.HandleFunc("POST /v1/agents/{agent}/tokens", createAgentTokenHandler(cfg.Authenticate, cfg.CreateAgentToken))
		}
		if cfg.CreateOperatorToken != nil {
			mux.HandleFunc("POST /v1/operators/self/tokens", createOperatorTokenHandler(cfg.Authenticate, cfg.CreateOperatorToken))
		}
		if cfg.ListOperators != nil {
			mux.HandleFunc("GET /v1/operators", listOperatorsHandler(cfg.Authenticate, cfg.ListOperators))
		}
		if cfg.CreateOperator != nil {
			mux.HandleFunc("POST /v1/operators", createOperatorHandler(cfg.Authenticate, cfg.CreateOperator))
		}
		if cfg.DeleteOperator != nil {
			mux.HandleFunc("DELETE /v1/operators/{operator}", deleteOperatorHandler(cfg.Authenticate, cfg.DeleteOperator))
		}
		if cfg.RevokeToken != nil {
			mux.HandleFunc("POST /v1/tokens/", revokeTokenHandler(cfg.Authenticate, cfg.RevokeToken))
		}
		if cfg.CloseAccount != nil {
			mux.HandleFunc("POST /v1/account:close", closeAccountHandler(cfg.Authenticate, cfg.CloseAccount))
		}
		if cfg.GetAccount != nil {
			mux.HandleFunc("GET /v1/account", getAccountHandler(cfg.Authenticate, cfg.GetAccount))
		}
		if cfg.RenameAccount != nil {
			mux.HandleFunc("POST /v1/account:rename", renameAccountHandler(cfg.Authenticate, cfg.RenameAccount))
		}
		if cfg.SuspendAccountOwner != nil {
			mux.HandleFunc("POST /v1/account:suspend", suspendAccountHandler(cfg.Authenticate, cfg.SuspendAccountOwner))
		}
		if cfg.ResumeAccountOwner != nil {
			mux.HandleFunc("POST /v1/account:resume", resumeAccountHandler(cfg.Authenticate, cfg.ResumeAccountOwner))
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
	operatorID    string
	accountID     string
	accountStatus string
}

// requireOperator authenticates the bearer token and passes the principal to
// h, or writes 401 (missing/invalid) / 500 (server fault). It also requires
// the account to be ACTIVE (-> 403 otherwise): a pending account can do
// nothing until its activation gates pass, and a suspended account can do
// nothing until it resumes. The exceptions — check status, close, suspend,
// resume — use requireOperatorAnyStatus instead. The refusal message names
// the current status verbatim so a suspended owner sees "account is
// suspended" and knows to reach for `ws account resume`.
func requireOperator(auth AuthFunc, h func(http.ResponseWriter, *http.Request, principal)) http.HandlerFunc {
	return requireOperatorAnyStatus(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		if p.accountStatus != "active" {
			writeJSONError(w, http.StatusForbidden,
				fmt.Sprintf("account is %s — this action requires an active account", p.accountStatus))
			return
		}
		h(w, r, p)
	})
}

// requireOperatorAnyStatus authenticates without gating on account status. Use
// only for the endpoints a not-yet-active or suspended account must still
// reach: checking its own status, closing itself, and (owner-initiated)
// suspending or resuming.
func requireOperatorAnyStatus(auth AuthFunc, h func(http.ResponseWriter, *http.Request, principal)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, ok := bearerToken(r)
		if !ok {
			writeJSONError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		operatorID, accountID, accountStatus, ok, err := auth(r.Context(), tok)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if !ok {
			writeJSONError(w, http.StatusUnauthorized, "invalid token")
			return
		}
		h(w, r, principal{operatorID: operatorID, accountID: accountID, accountStatus: accountStatus})
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

// provisionAccountHandler creates a new account on this cell. It is authorized
// by the pre-shared provision token (the control plane's credential), never by
// operator/agent tokens — provisioning is instance-level authority, above any
// account.
func provisionAccountHandler(provisionToken string, provision func(ctx context.Context, email, displayName string) (ProvisionedAccount, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, ok := bearerToken(r)
		if !ok || subtle.ConstantTimeCompare([]byte(tok), []byte(provisionToken)) != 1 {
			writeJSONError(w, http.StatusUnauthorized, "invalid provision token")
			return
		}
		var req struct {
			Email       string `json:"email"`
			DisplayName string `json:"display_name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		req.Email = strings.TrimSpace(strings.ToLower(req.Email))
		if req.Email == "" || !strings.Contains(req.Email, "@") {
			writeJSONError(w, http.StatusBadRequest, "valid email required")
			return
		}
		if req.DisplayName == "" {
			req.DisplayName = req.Email
		}
		acct, err := provision(r.Context(), req.Email, req.DisplayName)
		if errors.Is(err, ErrConflict) {
			writeJSONError(w, http.StatusConflict, "an account with this email already exists")
			return
		}
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "could not provision account")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": "witself.v0",
			"account":        acct,
		})
	}
}

// accountLifecycleHandler serves the provision-token-authorized lifecycle
// verbs on POST /v1/accounts/{id}:reap|:activate|:contact|:recover —
// instance-level authority, the same trust link as provisioning, so only the
// control plane can call them.
//
// reap: 200 whether it reaped just now or found the account already closed
// (idempotent); 409 when the account activated first (the sweep's view was
// stale — the cell is truth); 404 for an unknown id.
//
// activate: 200 whether it activated just now or was already active (an
// idempotent second click); 409 when the account is closed or otherwise
// ineligible (the link outlived its account); 404 for an unknown id.
//
// contact: 200 with the account's email and status — the read the recovery
// request needs when no operator token exists.
//
// recover: 200 with a fresh root-bound bootstrap token after rotating every
// live root credential; 409 unless the account is active.
func accountLifecycleHandler(cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, ok := bearerToken(r)
		if !ok || subtle.ConstantTimeCompare([]byte(tok), []byte(cfg.ProvisionToken)) != 1 {
			writeJSONError(w, http.StatusUnauthorized, "invalid provision token")
			return
		}
		if accountID, ok := pathActionID(r.URL.Path, "/v1/accounts/", "reap"); ok && cfg.ReapAccount != nil {
			reaped, err := cfg.ReapAccount(r.Context(), accountID)
			switch {
			case errors.Is(err, ErrNotFound):
				writeJSONError(w, http.StatusNotFound, "account not found")
				return
			case errors.Is(err, ErrConflict):
				writeJSONError(w, http.StatusConflict, "account is active")
				return
			case err != nil:
				writeJSONError(w, http.StatusInternalServerError, "could not reap account")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version": "witself.v0",
				"account_id":     accountID,
				"status":         "closed",
				"reaped":         reaped,
			})
			return
		}
		if accountID, ok := pathActionID(r.URL.Path, "/v1/accounts/", "activate"); ok && cfg.ActivateAccount != nil {
			activated, err := cfg.ActivateAccount(r.Context(), accountID)
			switch {
			case errors.Is(err, ErrNotFound):
				writeJSONError(w, http.StatusNotFound, "account not found")
				return
			case errors.Is(err, ErrConflict):
				writeJSONError(w, http.StatusConflict, "account cannot be activated")
				return
			case err != nil:
				writeJSONError(w, http.StatusInternalServerError, "could not activate account")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version": "witself.v0",
				"account_id":     accountID,
				"status":         "active",
				"activated":      activated,
			})
			return
		}
		if accountID, ok := pathActionID(r.URL.Path, "/v1/accounts/", "contact"); ok && cfg.AccountContact != nil {
			rec, err := cfg.AccountContact(r.Context(), accountID)
			switch {
			case errors.Is(err, ErrNotFound):
				writeJSONError(w, http.StatusNotFound, "account not found")
				return
			case err != nil:
				writeJSONError(w, http.StatusInternalServerError, "could not read account")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version": "witself.v0",
				"account_id":     rec.ID,
				"email":          rec.Email,
				"status":         rec.Status,
			})
			return
		}
		if accountID, ok := pathActionID(r.URL.Path, "/v1/accounts/", "update-email"); ok && cfg.UpdateAccountEmail != nil {
			var req struct {
				OperatorID      string `json:"operator_id"`
				NewEmail        string `json:"new_email"`
				Undo            bool   `json:"undo"`
				ExpectedCurrent string `json:"expected_current"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
				return
			}
			req.NewEmail = strings.TrimSpace(strings.ToLower(req.NewEmail))
			req.ExpectedCurrent = strings.TrimSpace(strings.ToLower(req.ExpectedCurrent))
			if req.NewEmail == "" || !strings.Contains(req.NewEmail, "@") {
				writeJSONError(w, http.StatusBadRequest, "a valid new_email required")
				return
			}
			var err error
			if req.Undo {
				if req.ExpectedCurrent == "" || cfg.UndoAccountEmail == nil {
					writeJSONError(w, http.StatusBadRequest, "expected_current required for undo")
					return
				}
				err = cfg.UndoAccountEmail(r.Context(), accountID, req.ExpectedCurrent, req.NewEmail)
			} else {
				if req.OperatorID == "" {
					writeJSONError(w, http.StatusBadRequest, "operator_id required")
					return
				}
				err = cfg.UpdateAccountEmail(r.Context(), accountID, req.OperatorID, req.NewEmail)
			}
			switch {
			case errors.Is(err, ErrNotFound):
				writeJSONError(w, http.StatusNotFound, "account not found")
				return
			case errors.Is(err, ErrNotAccountOwner):
				writeJSONError(w, http.StatusForbidden, "only the account owner can change the email")
				return
			case errors.Is(err, ErrConflict):
				writeJSONError(w, http.StatusConflict, "account is not active")
				return
			case errors.Is(err, ErrEmailChangedSinceUndo):
				writeJSONError(w, http.StatusConflict, "the email has changed since this undo link was issued")
				return
			case err != nil:
				writeJSONError(w, http.StatusInternalServerError, "could not update email")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{
				"schema_version": "witself.v0",
				"account_id":     accountID,
				"email":          req.NewEmail,
			})
			return
		}
		if accountID, ok := pathActionID(r.URL.Path, "/v1/accounts/", "recover"); ok && cfg.RecoverAccount != nil {
			acct, err := cfg.RecoverAccount(r.Context(), accountID)
			switch {
			case errors.Is(err, ErrNotFound):
				writeJSONError(w, http.StatusNotFound, "account not found")
				return
			case errors.Is(err, ErrConflict):
				writeJSONError(w, http.StatusConflict, "account is not active")
				return
			case err != nil:
				writeJSONError(w, http.StatusInternalServerError, "could not recover account")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version": "witself.v0",
				"account":        acct,
			})
			return
		}
		if accountID, ok := pathActionID(r.URL.Path, "/v1/accounts/", "suspend"); ok && cfg.SuspendAccountSystem != nil {
			var req struct {
				For    string `json:"for"`
				Reason string `json:"reason"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.For == "" {
				writeJSONError(w, http.StatusBadRequest, "a suspension category (for) is required")
				return
			}
			err := cfg.SuspendAccountSystem(r.Context(), accountID, req.For, strings.TrimSpace(req.Reason))
			switch {
			case errors.Is(err, ErrNotFound):
				writeJSONError(w, http.StatusNotFound, "account not found")
				return
			case errors.Is(err, ErrCannotCloseDefault):
				writeJSONError(w, http.StatusForbidden, "the deployment's default account cannot be suspended")
				return
			case errors.Is(err, ErrConflict):
				writeJSONError(w, http.StatusConflict, "account is pending — not suspendable")
				return
			case err != nil:
				writeJSONError(w, http.StatusInternalServerError, "could not suspend account")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{
				"schema_version": "witself.v0",
				"account_id":     accountID,
				"status":         "suspended",
			})
			return
		}
		if accountID, ok := pathActionID(r.URL.Path, "/v1/accounts/", "export"); ok && cfg.StreamAccountExport != nil {
			// Preconditions surface as JSON errors; once streaming begins,
			// the archive's own trailing checksums are the integrity story.
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("X-Witself-Export-Format", "1")
			err := cfg.StreamAccountExport(r.Context(), accountID, w)
			switch {
			case errors.Is(err, ErrNotFound):
				writeJSONError(w, http.StatusNotFound, "account not found")
				return
			case errors.Is(err, ErrConflict):
				writeJSONError(w, http.StatusConflict, "account must be suspended (or closed) to export")
				return
			case err != nil:
				// Headers may already be sent; the truncated stream fails
				// the trailing-checksum verification downstream.
				return
			}
			return
		}
		// Deliberately distinct from the handlers' "account not found": the
		// control plane treats that exact string as authoritative and this
		// one as retryable, so a cell that doesn't serve an action yet never
		// burns a verification link or a reap candidate.
		writeJSONError(w, http.StatusNotFound, "unknown account action")
	}
}

// suspendAccountHandler freezes every write on the account at the owner's
// request. Owner-only; the seeded default account is refused; only-if-active.
// Reachable at any status the caller reaches with (a pending caller gets a
// 409, not a 403) — the auth gate lets it through, the store adjudicates.
func suspendAccountHandler(auth AuthFunc, suspend func(ctx context.Context, accountID, operatorID, reason string) error) http.HandlerFunc {
	return requireOperatorAnyStatus(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		var req struct {
			Reason string `json:"reason"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req) // body optional
		err := suspend(r.Context(), p.accountID, p.operatorID, strings.TrimSpace(req.Reason))
		switch {
		case errors.Is(err, ErrNotFound):
			writeJSONError(w, http.StatusNotFound, "account not found")
			return
		case errors.Is(err, ErrNotAccountOwner):
			writeJSONError(w, http.StatusForbidden, "only the account owner can suspend the account")
			return
		case errors.Is(err, ErrCannotCloseDefault):
			writeJSONError(w, http.StatusForbidden, "the deployment's default account cannot be suspended")
			return
		case errors.Is(err, ErrAccountNotActive):
			writeJSONError(w, http.StatusConflict, "account is not active — nothing to suspend")
			return
		case err != nil:
			writeJSONError(w, http.StatusInternalServerError, "could not suspend account")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"schema_version": "witself.v0",
			"account_id":     p.accountID,
			"status":         "suspended",
		})
	})
}

// resumeAccountHandler undoes an owner-initiated suspension. Owner-only, and
// refuses to un-suspend a suspension initiated by a different authority
// (fleet-admin, migration, ...) — the authority that suspended is the one
// that resumes.
func resumeAccountHandler(auth AuthFunc, resume func(ctx context.Context, accountID, operatorID string) error) http.HandlerFunc {
	return requireOperatorAnyStatus(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		err := resume(r.Context(), p.accountID, p.operatorID)
		switch {
		case errors.Is(err, ErrNotFound):
			writeJSONError(w, http.StatusNotFound, "account not found")
			return
		case errors.Is(err, ErrNotAccountOwner):
			writeJSONError(w, http.StatusForbidden, "only the account owner can resume the account")
			return
		case errors.Is(err, ErrCannotSelfResume):
			writeJSONError(w, http.StatusForbidden, "this suspension was not initiated by you — the authority that suspended must resume")
			return
		case errors.Is(err, ErrAccountNotSuspended):
			writeJSONError(w, http.StatusConflict, "account is not suspended")
			return
		case err != nil:
			writeJSONError(w, http.StatusInternalServerError, "could not resume account")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"schema_version": "witself.v0",
			"account_id":     p.accountID,
			"status":         "active",
		})
	})
}

// closeAccountHandler permanently closes the authenticated operator's account:
// tombstone + revoke every live credential. Owner-only; the seeded default
// account is refused. Idempotent. Not status-gated: a pending account can
// always be abandoned by its owner.
func closeAccountHandler(auth AuthFunc, closeAccount func(ctx context.Context, accountID, operatorID, reason string) error) http.HandlerFunc {
	return requireOperatorAnyStatus(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		var req struct {
			Reason string `json:"reason"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req) // body optional
		err := closeAccount(r.Context(), p.accountID, p.operatorID, req.Reason)
		switch {
		case errors.Is(err, ErrNotAccountOwner):
			writeJSONError(w, http.StatusForbidden, "only the account owner can close the account")
			return
		case errors.Is(err, ErrCannotCloseDefault):
			writeJSONError(w, http.StatusForbidden, "the deployment's default account cannot be closed")
			return
		case err != nil:
			writeJSONError(w, http.StatusInternalServerError, "could not close account")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"schema_version": "witself.v0",
			"account_id":     p.accountID,
			"status":         "closed",
		})
	})
}

// getAccountHandler returns the authenticated operator's account lifecycle
// record. Deliberately not status-gated: a pending account's owner checks here
// whether activation has happened yet.
func getAccountHandler(auth AuthFunc, get func(ctx context.Context, accountID string) (AccountRecord, error)) http.HandlerFunc {
	return requireOperatorAnyStatus(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		rec, err := get(r.Context(), p.accountID)
		if errors.Is(err, ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "account not found")
			return
		}
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "could not read account")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "account": rec})
	})
}

// renameAccountHandler changes the account's server-side display name.
// Owner-only, active-only (requireOperator's gate covers the latter).
func renameAccountHandler(auth AuthFunc, rename func(ctx context.Context, accountID, operatorID, displayName string) error) http.HandlerFunc {
	return requireOperator(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		var req struct {
			DisplayName string `json:"display_name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		req.DisplayName = strings.TrimSpace(req.DisplayName)
		if req.DisplayName == "" {
			writeJSONError(w, http.StatusBadRequest, "missing display_name")
			return
		}
		err := rename(r.Context(), p.accountID, p.operatorID, req.DisplayName)
		switch {
		case errors.Is(err, ErrNotAccountOwner):
			writeJSONError(w, http.StatusForbidden, "only the account owner can rename the account")
			return
		case err != nil:
			writeJSONError(w, http.StatusInternalServerError, "could not rename account")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"schema_version": "witself.v0",
			"account_id":     p.accountID,
			"display_name":   req.DisplayName,
		})
	})
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

func deleteRealmHandler(auth AuthFunc, deleteRealm func(ctx context.Context, accountID, realmID string) error) http.HandlerFunc {
	return requireOperator(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		err := deleteRealm(r.Context(), p.accountID, r.PathValue("realm"))
		switch {
		case errors.Is(err, ErrNotFound):
			writeJSONError(w, http.StatusNotFound, "realm not found")
			return
		case errors.Is(err, ErrConflict):
			writeJSONError(w, http.StatusConflict, "realm is not empty")
			return
		case err != nil:
			writeJSONError(w, http.StatusInternalServerError, "could not delete realm")
			return
		}
		w.WriteHeader(http.StatusNoContent)
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

func deleteAgentHandler(auth AuthFunc, deleteAgent func(ctx context.Context, accountID, realmID, agentID string) error) http.HandlerFunc {
	return requireOperator(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		err := deleteAgent(r.Context(), p.accountID, r.PathValue("realm"), r.PathValue("agent"))
		switch {
		case errors.Is(err, ErrNotFound):
			writeJSONError(w, http.StatusNotFound, "agent not found")
			return
		case err != nil:
			writeJSONError(w, http.StatusInternalServerError, "could not delete agent")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func createAgentTokenHandler(auth AuthFunc, create func(ctx context.Context, accountID, agentID string) (string, string, string, error)) http.HandlerFunc {
	return requireOperator(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		agentID := r.PathValue("agent")
		tok, tokenID, agentName, err := create(r.Context(), p.accountID, agentID)
		if errors.Is(err, ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "agent not found")
			return
		}
		if errors.Is(err, ErrAccountNotActive) {
			writeJSONError(w, http.StatusForbidden, "account is not active")
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
			"token_id":       tokenID,
			"agent_id":       agentID,
			"agent_name":     agentName,
		})
	})
}

func createOperatorTokenHandler(auth AuthFunc, create func(ctx context.Context, accountID, operatorID, displayName string, ttl *time.Duration) (string, string, *time.Time, error)) http.HandlerFunc {
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

		tok, tokenID, expiresAt, err := create(r.Context(), p.accountID, p.operatorID, displayName, ttl)
		if errors.Is(err, ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "operator not found")
			return
		}
		if errors.Is(err, ErrAccountNotActive) {
			writeJSONError(w, http.StatusForbidden, "account is not active")
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
			"token_id":       tokenID,
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

func listOperatorsHandler(auth AuthFunc, list func(ctx context.Context, accountID string) ([]Operator, error)) http.HandlerFunc {
	return requireOperator(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		operators, err := list(r.Context(), p.accountID)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "could not list operators")
			return
		}
		if operators == nil {
			operators = []Operator{}
		}
		for i := range operators {
			if operators[i].Tokens == nil {
				operators[i].Tokens = []OperatorToken{}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "operators": operators})
	})
}

func createOperatorHandler(auth AuthFunc, create func(ctx context.Context, accountID, displayName, tokenDisplayName string, ttl *time.Duration) (Operator, string, *time.Time, error)) http.HandlerFunc {
	return requireOperator(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		var req struct {
			DisplayName      string `json:"display_name"`
			TokenDisplayName string `json:"token_display_name,omitempty"`
			TTL              string `json:"ttl,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		displayName := strings.TrimSpace(req.DisplayName)
		if displayName == "" {
			writeJSONError(w, http.StatusBadRequest, "missing display_name")
			return
		}
		tokenDisplayName := strings.TrimSpace(req.TokenDisplayName)
		if tokenDisplayName == "" {
			tokenDisplayName = displayName
		}

		var ttl *time.Duration
		if req.TTL != "" {
			d, err := time.ParseDuration(req.TTL)
			if err != nil || d <= 0 {
				writeJSONError(w, http.StatusBadRequest, "invalid ttl")
				return
			}
			ttl = &d
		}

		operator, token, expiresAt, err := create(r.Context(), p.accountID, displayName, tokenDisplayName, ttl)
		if errors.Is(err, ErrAccountNotActive) {
			writeJSONError(w, http.StatusForbidden, "account is not active")
			return
		}
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "could not create operator")
			return
		}
		out := map[string]any{
			"schema_version":   "witself.v0",
			"operator":         operator,
			"operator_token":   token,
			"token_expires_at": expiresAt,
		}
		if expiresAt == nil {
			delete(out, "token_expires_at")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(out)
	})
}

func deleteOperatorHandler(auth AuthFunc, deleteOperator func(ctx context.Context, accountID, actorOperatorID, targetOperatorID string) error) http.HandlerFunc {
	return requireOperator(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		err := deleteOperator(r.Context(), p.accountID, p.operatorID, r.PathValue("operator"))
		switch {
		case errors.Is(err, ErrNotFound):
			writeJSONError(w, http.StatusNotFound, "operator not found")
			return
		case errors.Is(err, ErrCannotDeleteSelf):
			writeJSONError(w, http.StatusConflict, "cannot delete the authenticated operator")
			return
		case errors.Is(err, ErrCannotDeleteRoot):
			writeJSONError(w, http.StatusConflict, "cannot delete the root operator")
			return
		case errors.Is(err, ErrLastOperator):
			writeJSONError(w, http.StatusConflict, "cannot delete the last operator")
			return
		case err != nil:
			writeJSONError(w, http.StatusInternalServerError, "could not delete operator")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func revokeTokenHandler(auth AuthFunc, revoke func(ctx context.Context, accountID, tokenID string) error) http.HandlerFunc {
	return requireOperator(auth, func(w http.ResponseWriter, r *http.Request, p principal) {
		tokenID, ok := tokenActionID(r.URL.Path, "revoke")
		if !ok {
			writeJSONError(w, http.StatusNotFound, "token not found")
			return
		}
		err := revoke(r.Context(), p.accountID, tokenID)
		switch {
		case errors.Is(err, ErrNotFound):
			writeJSONError(w, http.StatusNotFound, "token not found")
			return
		case err != nil:
			writeJSONError(w, http.StatusInternalServerError, "could not revoke token")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func tokenActionID(path, action string) (string, bool) {
	return pathActionID(path, "/v1/tokens/", action)
}

// pathActionID extracts the id from "{prefix}{id}:{action}" (or the
// "/{action}" spelling), refusing empty or multi-segment ids.
func pathActionID(path, prefix, action string) (string, bool) {
	if !strings.HasPrefix(path, prefix) {
		return "", false
	}
	rest := strings.TrimPrefix(path, prefix)
	for _, suffix := range []string{":" + action, "/" + action} {
		if strings.HasSuffix(rest, suffix) {
			id := strings.TrimSuffix(rest, suffix)
			if id != "" && !strings.Contains(id, "/") {
				return id, true
			}
		}
	}
	return "", false
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
				_, _ = fmt.Fprintf(w, "not ready: %v\n", err)
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
