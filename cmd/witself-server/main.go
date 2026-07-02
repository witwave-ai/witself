// Command witself-server is the Witself backend API server. It supports version
// and a serve command that binds the API, health-probe, and metrics listeners.
// The full backend is specified under docs/.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/witwave-ai/witself/internal/server"
	"github.com/witwave-ai/witself/internal/store"
	"github.com/witwave-ai/witself/internal/version"
)

const defaultBootstrapTokenFile = "/.witself/tokens/bootstrap.token"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage(os.Stdout)
		return 0
	}
	switch args[0] {
	case "version", "--version", "-v":
		fmt.Println(version.String("witself-server"))
		return 0
	case "help", "--help", "-h":
		usage(os.Stdout)
		return 0
	case "serve":
		return serve()
	default:
		fmt.Fprintf(os.Stderr, "witself-server: unknown command %q\n\n", args[0])
		usage(os.Stderr)
		return 2
	}
}

func serve() int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg := server.ConfigFromEnv()
	if dsn := dbDSN(); dsn != "" {
		st, err := store.Open(ctx, dsn)
		if err != nil {
			fmt.Fprintf(os.Stderr, "witself-server: database: %v\n", err)
			return 1
		}
		defer st.Close()
		if err := st.Migrate(); err != nil {
			fmt.Fprintf(os.Stderr, "witself-server: %v\n", err)
			return 1
		}
		acctID, err := st.EnsureDefaultAccount(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "witself-server: %v\n", err)
			return 1
		}
		cfg.AccountID = acctID
		oprID, err := st.EnsureRootOperator(ctx, acctID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "witself-server: %v\n", err)
			return 1
		}
		bt, err := bootstrapToken()
		if err != nil {
			fmt.Fprintf(os.Stderr, "witself-server: %v\n", err)
			return 1
		}
		if bt != "" {
			ttl, err := bootstrapTokenTTL()
			if err != nil {
				fmt.Fprintf(os.Stderr, "witself-server: %v\n", err)
				return 1
			}
			if err := st.AdoptBootstrapToken(ctx, acctID, oprID, bt, ttl); err != nil {
				fmt.Fprintf(os.Stderr, "witself-server: %v\n", err)
				return 1
			}
			fmt.Fprintf(os.Stderr, "witself-server: bootstrap token adopted (ttl %s)\n", ttl)
		}
		cfg.Login = func(ctx context.Context, bt string) (string, string, bool, error) {
			ot, oid, err := st.ExchangeBootstrap(ctx, bt)
			if errors.Is(err, store.ErrInvalidBootstrap) {
				return "", "", false, nil
			}
			if err != nil {
				return "", "", false, err
			}
			return ot, oid, true, nil
		}
		cfg.Authenticate = st.AuthenticateOperator
		cfg.CreateRealm = func(ctx context.Context, accountID, name string) (server.Realm, error) {
			r, err := st.CreateRealm(ctx, accountID, name)
			if errors.Is(err, store.ErrRealmExists) {
				return server.Realm{}, server.ErrConflict
			}
			if err != nil {
				return server.Realm{}, err
			}
			return server.Realm{ID: r.ID, Name: r.Name}, nil
		}
		cfg.ListRealms = func(ctx context.Context, accountID string) ([]server.Realm, error) {
			rs, err := st.ListRealms(ctx, accountID)
			if err != nil {
				return nil, err
			}
			out := make([]server.Realm, len(rs))
			for i, r := range rs {
				out[i] = server.Realm{ID: r.ID, Name: r.Name}
			}
			return out, nil
		}
		cfg.DeleteRealm = func(ctx context.Context, accountID, realmID string) error {
			err := st.DeleteRealm(ctx, accountID, realmID)
			switch {
			case errors.Is(err, store.ErrRealmNotFound):
				return server.ErrNotFound
			case errors.Is(err, store.ErrRealmNotEmpty):
				return server.ErrConflict
			default:
				return err
			}
		}
		cfg.CreateAgent = func(ctx context.Context, accountID, realmID, name string) (server.Agent, error) {
			a, err := st.CreateAgent(ctx, accountID, realmID, name)
			switch {
			case errors.Is(err, store.ErrRealmNotFound):
				return server.Agent{}, server.ErrNotFound
			case errors.Is(err, store.ErrAgentExists):
				return server.Agent{}, server.ErrConflict
			case err != nil:
				return server.Agent{}, err
			}
			return server.Agent{ID: a.ID, Name: a.Name}, nil
		}
		cfg.ListAgents = func(ctx context.Context, accountID, realmID string) ([]server.Agent, error) {
			as, err := st.ListAgents(ctx, accountID, realmID)
			if err != nil {
				return nil, err
			}
			out := make([]server.Agent, len(as))
			for i, a := range as {
				out[i] = server.Agent{ID: a.ID, Name: a.Name}
			}
			return out, nil
		}
		cfg.DeleteAgent = func(ctx context.Context, accountID, realmID, agentID string) error {
			err := st.DeleteAgent(ctx, accountID, realmID, agentID)
			if errors.Is(err, store.ErrAgentNotFound) {
				return server.ErrNotFound
			}
			return err
		}
		cfg.CreateAgentToken = func(ctx context.Context, accountID, agentID string) (string, string, error) {
			tok, tokenID, err := st.CreateAgentToken(ctx, accountID, agentID)
			if errors.Is(err, store.ErrAgentNotFound) {
				return "", "", server.ErrNotFound
			}
			return tok, tokenID, err
		}
		cfg.CreateOperatorToken = func(ctx context.Context, accountID, operatorID, displayName string, ttl *time.Duration) (string, string, *time.Time, error) {
			tok, tokenID, expiresAt, err := st.CreateOperatorToken(ctx, accountID, operatorID, displayName, ttl)
			if errors.Is(err, store.ErrOperatorNotFound) {
				return "", "", nil, server.ErrNotFound
			}
			return tok, tokenID, expiresAt, err
		}
		cfg.ListOperators = func(ctx context.Context, accountID string) ([]server.Operator, error) {
			ops, err := st.ListOperators(ctx, accountID)
			if err != nil {
				return nil, err
			}
			out := make([]server.Operator, len(ops))
			for i, op := range ops {
				out[i] = serverOperator(op)
			}
			return out, nil
		}
		cfg.CreateOperator = func(ctx context.Context, accountID, displayName, tokenDisplayName string, ttl *time.Duration) (server.Operator, string, *time.Time, error) {
			op, tok, expiresAt, err := st.CreateOperator(ctx, accountID, displayName, tokenDisplayName, ttl)
			if err != nil {
				return server.Operator{}, "", nil, err
			}
			return serverOperator(op), tok, expiresAt, nil
		}
		cfg.DeleteOperator = func(ctx context.Context, accountID, actorOperatorID, targetOperatorID string) error {
			err := st.DeleteOperator(ctx, accountID, actorOperatorID, targetOperatorID)
			switch {
			case errors.Is(err, store.ErrOperatorNotFound):
				return server.ErrNotFound
			case errors.Is(err, store.ErrCannotDeleteSelf):
				return server.ErrCannotDeleteSelf
			case errors.Is(err, store.ErrCannotDeleteRootOperator):
				return server.ErrCannotDeleteRoot
			case errors.Is(err, store.ErrLastOperator):
				return server.ErrLastOperator
			default:
				return err
			}
		}
		cfg.RevokeToken = func(ctx context.Context, accountID, tokenID string) error {
			err := st.RevokeToken(ctx, accountID, tokenID)
			if errors.Is(err, store.ErrTokenNotFound) {
				return server.ErrNotFound
			}
			return err
		}
		if pt := strings.TrimSpace(os.Getenv("WITSELF_PROVISION_TOKEN")); pt != "" {
			// Account provisioning: the control-plane -> cell trust link. The
			// bootstrap tokens minted per signup are short-lived — the CLI
			// exchanges them within seconds.
			const provisionBootstrapTTL = time.Hour
			cfg.ProvisionToken = pt
			cfg.ProvisionAccount = func(ctx context.Context, email, displayName string) (server.ProvisionedAccount, error) {
				p, err := st.ProvisionAccount(ctx, email, displayName, provisionBootstrapTTL)
				if errors.Is(err, store.ErrAccountEmailExists) {
					return server.ProvisionedAccount{}, server.ErrConflict
				}
				if err != nil {
					return server.ProvisionedAccount{}, err
				}
				return server.ProvisionedAccount{
					AccountID:      p.AccountID,
					OperatorID:     p.OperatorID,
					Email:          p.Email,
					Status:         p.Status,
					BootstrapToken: p.BootstrapToken,
				}, nil
			}
			fmt.Fprintln(os.Stderr, "witself-server: account provisioning enabled (WITSELF_PROVISION_TOKEN set)")
		}
		cfg.Ready = st.Ping
		fmt.Fprintf(os.Stderr, "witself-server: migrated; account %s, root operator %s ready; /readyz gates on it\n", acctID, oprID)
	} else {
		fmt.Fprintln(os.Stderr, "witself-server: no database configured (WITSELF_DATABASE_URL unset); /readyz unconditional")
	}

	if err := server.Run(ctx, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "witself-server: %v\n", err)
		return 1
	}
	fmt.Fprintln(os.Stderr, "witself-server: shut down cleanly")
	return 0
}

func serverOperator(op store.Operator) server.Operator {
	out := server.Operator{
		ID:          op.ID,
		DisplayName: op.DisplayName,
		Role:        op.Role,
		IsRoot:      op.IsRoot,
		CreatedAt:   op.CreatedAt,
		UpdatedAt:   op.UpdatedAt,
		Tokens:      make([]server.OperatorToken, len(op.Tokens)),
	}
	for i, tok := range op.Tokens {
		out.Tokens[i] = server.OperatorToken{
			ID:          tok.ID,
			DisplayName: tok.DisplayName,
			CreatedAt:   tok.CreatedAt,
			ExpiresAt:   tok.ExpiresAt,
		}
	}
	return out
}

// dbDSN resolves the Postgres DSN from the environment, preferring the
// WITSELF_-prefixed name and falling back to the conventional DATABASE_URL.
func dbDSN() string {
	if v := os.Getenv("WITSELF_DATABASE_URL"); v != "" {
		return v
	}
	return os.Getenv("DATABASE_URL")
}

// bootstrapToken resolves first-operator bootstrap material from a token file,
// preferring an explicit path but also checking the deployment well-known path.
// WITSELF_BOOTSTRAP_TOKEN remains as a local/dev fallback.
func bootstrapToken() (string, error) {
	if path := os.Getenv("WITSELF_BOOTSTRAP_TOKEN_FILE"); path != "" {
		return readTokenFile(path, true)
	}
	if tok := strings.TrimSpace(os.Getenv("WITSELF_BOOTSTRAP_TOKEN")); tok != "" {
		return tok, nil
	}
	return readTokenFile(defaultBootstrapTokenFile, false)
}

func readTokenFile(path string, required bool) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if !required && errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("read bootstrap token file %s: %w", path, err)
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return "", fmt.Errorf("bootstrap token file %s is empty", path)
	}
	return tok, nil
}

func bootstrapTokenTTL() (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv("WITSELF_BOOTSTRAP_TOKEN_TTL"))
	if raw == "" {
		raw = "24h"
	}
	ttl, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("parse WITSELF_BOOTSTRAP_TOKEN_TTL: %w", err)
	}
	if ttl <= 0 {
		return 0, fmt.Errorf("WITSELF_BOOTSTRAP_TOKEN_TTL must be positive")
	}
	return ttl, nil
}

func usage(w io.Writer) {
	usageLine(w, "witself-server — the Witself backend API server")
	usageLine(w)
	usageLine(w, "Usage:")
	usageLine(w, "  witself-server version    Print version information")
	usageLine(w, "  witself-server serve      Run the API, health, and metrics listeners")
	usageLine(w)
	usageLine(w, "Listeners (override with env):")
	usageLine(w, "  WITSELF_API_ADDR      default :8080  (/v1 API)")
	usageLine(w, "  WITSELF_HEALTH_ADDR   default :8081  (/livez /readyz /startupz)")
	usageLine(w, "  WITSELF_METRICS_ADDR  default :9090  (/metrics)")
	usageLine(w)
	usageLine(w, "Database (optional; when set, /readyz gates on it):")
	usageLine(w, "  WITSELF_DATABASE_URL  Postgres DSN (falls back to DATABASE_URL)")
	usageLine(w)
	usageLine(w, "Bootstrap (optional first-operator setup):")
	usageLine(w, "  WITSELF_BOOTSTRAP_TOKEN_FILE  token file path (default /.witself/tokens/bootstrap.token)")
	usageLine(w, "  WITSELF_PROVISION_TOKEN       enables POST /v1/accounts (control-plane account provisioning)")
	usageLine(w, "  WITSELF_BOOTSTRAP_TOKEN_TTL   token lifetime after adoption (default 24h)")
}

func usageLine(w io.Writer, args ...any) {
	_, _ = fmt.Fprintln(w, args...)
}
