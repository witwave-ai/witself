// Command witself-control-plane is the Witself Cloud control plane: the thin
// global service that will own account signup, the account->cell directory, and
// the cell registry. Cells hold all tenant data; this holds routing metadata
// only. It runs as a container on Cloudflare behind a thin Worker front door
// (see infra/cloudflare/control-plane).
//
// This first slice is deliberately bare: health, version, and a root banner —
// enough to stand the deployment up end to end. Signup, the directory, and cell
// registration land in later slices.
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	neturl "net/url"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/witwave-ai/witself/internal/billing"
	"github.com/witwave-ai/witself/internal/billing/lifecycle"
	stripeprovider "github.com/witwave-ai/witself/internal/billing/stripe"
	"github.com/witwave-ai/witself/internal/blob"
	"github.com/witwave-ai/witself/internal/cpserver"
	"github.com/witwave-ai/witself/internal/plans"
	"github.com/witwave-ai/witself/internal/version"
)

func main() {
	os.Exit(run())
}

func run() int {
	if len(os.Args) > 1 && (os.Args[1] == "version" || os.Args[1] == "--version" || os.Args[1] == "-v") {
		fmt.Println(version.String("witself-control-plane"))
		return 0
	}

	addr := os.Getenv("WITSELF_CONTROL_PLANE_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	mux := http.NewServeMux()
	// Bare meta endpoints, matching the cell server's flat (non-enveloped) style.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /v1/version", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"schema_version": "witself.v0",
			"service":        "witself-control-plane",
			"version":        version.Version,
			"commit":         version.Commit,
			"date":           version.Date,
		})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"schema_version": "witself.v0",
				"error":          "not found",
			})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"schema_version": "witself.v0",
			"service":        "witself-control-plane",
			"status":         "bare-bones — signup, directory, and cell registry land in later slices",
		})
	})

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	// Cloudflare Containers stop instances with SIGTERM (then SIGKILL after a
	// grace window); exit cleanly and quickly.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Billing/plan lifecycle (issue #31): mounted when configured, absent
	// otherwise — the bare control plane stays deployable with zero billing.
	if err := setupBilling(ctx, mux); err != nil {
		fmt.Fprintf(os.Stderr, "witself-control-plane: billing: %v\n", err)
		return 1
	}

	errc := make(chan error, 1)
	go func() {
		fmt.Fprintf(os.Stderr, "witself-control-plane: listening on %s\n", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case <-ctx.Done():
	case err := <-errc:
		fmt.Fprintf(os.Stderr, "witself-control-plane: %v\n", err)
		return 1
	}

	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
	fmt.Fprintln(os.Stderr, "witself-control-plane: shut down cleanly")
	return 0
}

// setupBilling wires the account plan lifecycle only when
// WITSELF_CP_PLAN_LIFECYCLE_ENABLED is explicitly true. Billing is optional:
// without a provider, status/defaults/admin overrides and cell enforcement
// work normally while customer subscription mutations remain absent.
// Configuration (all env, 12-factor):
//
//	WITSELF_CP_PLAN_LIFECYCLE_ENABLED  explicit true/false feature gate
//	WITSELF_CP_BRIDGE_URL         directory-owning Worker base URL
//	WITSELF_CP_BRIDGE_TOKEN       shared Worker/container bridge credential
//	WITSELF_CP_BILLING_PROVIDER  "fake" or "stripe"
//	WITSELF_CP_STRIPE_SECRET_KEY      sk_live_/sk_test_ API key   (stripe)
//	WITSELF_CP_STRIPE_WEBHOOK_SECRET  whsec_ signing secret       (stripe)
//	WITSELF_CP_STRIPE_SUCCESS_URL     optional checkout return URLs
//	WITSELF_CP_STRIPE_CANCEL_URL
//	WITSELF_CP_R2_ENDPOINT       https://<account>.r2.cloudflarestorage.com
//	WITSELF_CP_R2_BUCKET         registry bucket (witself-control-plane)
//	WITSELF_CP_R2_ACCESS_KEY     R2 S3 credentials (Object Read & Write)
//	WITSELF_CP_R2_SECRET_KEY
//	WITSELF_CP_R2_PREFIX         key prefix (default "registry/")
func setupBilling(ctx context.Context, mux *http.ServeMux) error {
	enabled, err := explicitBoolEnv("WITSELF_CP_PLAN_LIFECYCLE_ENABLED")
	if err != nil {
		return err
	}
	if !enabled {
		return nil
	}

	bridgeURL := strings.TrimSpace(os.Getenv("WITSELF_CP_BRIDGE_URL"))
	bridgeToken := strings.TrimSpace(os.Getenv("WITSELF_CP_BRIDGE_TOKEN"))
	if bridgeURL == "" || bridgeToken == "" {
		return errors.New("plan lifecycle requires WITSELF_CP_BRIDGE_URL and WITSELF_CP_BRIDGE_TOKEN")
	}
	if err := validateProductionBridgeURL(bridgeURL); err != nil {
		return err
	}

	catalog, err := plans.Load()
	if err != nil {
		return err
	}

	providerName := strings.TrimSpace(os.Getenv("WITSELF_CP_BILLING_PROVIDER"))
	var provider billing.Provider
	var bootstrap func(context.Context) error
	switch providerName {
	case "":
		// Providerless/manual mode is intentional. It powers defaults,
		// administrator overrides, seeding, status, and cell enforcement
		// without pretending a customer can purchase or cancel anything.
	case "stripe":
		// The webhook secret is mandatory: without it the binary would boot
		// cleanly, mint checkout links, take payments — and refuse every
		// webhook delivery, silently losing paid activations once Stripe's
		// ~3-day retry horizon passes. Fail at boot instead.
		webhookSecret := os.Getenv("WITSELF_CP_STRIPE_WEBHOOK_SECRET")
		if webhookSecret == "" {
			return errors.New("WITSELF_CP_STRIPE_WEBHOOK_SECRET is required with the stripe provider (whsec_...): without it webhooks are refused and paid activations are lost")
		}
		sp, err := stripeprovider.New(stripeprovider.Config{
			SecretKey:     os.Getenv("WITSELF_CP_STRIPE_SECRET_KEY"),
			WebhookSecret: webhookSecret,
			Catalog:       catalog,
			SuccessURL:    os.Getenv("WITSELF_CP_STRIPE_SUCCESS_URL"),
			CancelURL:     os.Getenv("WITSELF_CP_STRIPE_CANCEL_URL"),
		})
		if err != nil {
			return err
		}
		provider = sp
		// Self-provision the catalog's products/prices (by lookup_key) so a
		// plans.json change needs no dashboard clicks.
		bootstrap = sp.EnsurePrices
	case "fake":
		// Never put the dev fake behind the production Worker bridge. It can
		// manufacture completed subscriptions and therefore has no place in
		// a providerless/manual rollout.
		return errors.New("WITSELF_CP_BILLING_PROVIDER=fake is not allowed with the production cell plan lifecycle bridge")
	default:
		return fmt.Errorf("unknown WITSELF_CP_BILLING_PROVIDER %q (have: stripe, or empty for manual mode)", providerName)
	}
	if bootstrap != nil {
		// Best-effort: prices also resolve lazily on first checkout, so a
		// provider outage at boot must not crash-loop the whole CP.
		if err := bootstrap(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "witself-control-plane: price bootstrap failed (will resolve lazily): %v\n", err)
		}
	}
	providers := map[string]billing.Provider{}
	if provider != nil {
		providers[providerName] = provider
	}

	blobClient, err := blob.New(blob.Config{
		Endpoint:  os.Getenv("WITSELF_CP_R2_ENDPOINT"),
		Bucket:    os.Getenv("WITSELF_CP_R2_BUCKET"),
		AccessKey: os.Getenv("WITSELF_CP_R2_ACCESS_KEY"),
		SecretKey: os.Getenv("WITSELF_CP_R2_SECRET_KEY"),
	})
	if err != nil {
		return fmt.Errorf("registry: %w (set WITSELF_CP_R2_*)", err)
	}
	prefix := os.Getenv("WITSELF_CP_R2_PREFIX")
	if prefix == "" {
		prefix = "registry/"
	}

	cellResolve := cpserver.BridgeCell(bridgeURL, bridgeToken)
	applier := cpserver.NewBridgeApplier(bridgeURL, bridgeToken)
	authenticate := cpserver.CellAuthenticate(cellResolve)
	manager, err := lifecycle.NewManager(lifecycle.Config{
		Catalog:   catalog,
		Providers: providers,
		Default:   providerName,
		Store:     lifecycle.NewR2Store(blobClient, prefix),
		Applier:   applier,
	})
	if err != nil {
		return err
	}

	adminAuthenticate := bridgeAdminAuthenticator(bridgeToken)
	internalAuthenticate := func(_ context.Context, bearer string) (bool, error) {
		return subtle.ConstantTimeCompare([]byte(bearer), []byte(bridgeToken)) == 1, nil
	}
	observer := cpserver.NewPlanLifecycleObserver(manager.BillingAvailable())
	if err := cpserver.Register(mux, cpserver.Config{
		Manager:              manager,
		Catalog:              catalog,
		Providers:            providers,
		Authenticate:         authenticate,
		AdminAuthenticate:    adminAuthenticate,
		AdminAccountExists:   cpserver.BridgeAccountExists(bridgeURL, bridgeToken),
		LifecycleObserver:    observer,
		InternalAuthenticate: internalAuthenticate,
	}); err != nil {
		return err
	}

	providerLabel := providerName
	if providerLabel == "" {
		providerLabel = "manual"
	}
	// Hosted reconciliation is driven by the Worker's authenticated cron tick.
	// The Worker persists the directory cursor in KV, so container sleep and
	// process restarts cannot reset fleet progress.
	fmt.Fprintf(os.Stderr, "witself-control-plane: plan lifecycle enabled (provider %s, worker-cron scheduled)\n", providerLabel)
	return nil
}

func validateProductionBridgeURL(rawURL string) error {
	parsed, err := neturl.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("WITSELF_CP_BRIDGE_URL must be an HTTPS base URL without credentials, query, or fragment")
	}
	return nil
}

var (
	bridgeAdminIDPattern     = regexp.MustCompile(`^adm_[a-z0-9]{20}$`)
	bridgeAdminHandlePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{1,31}$`)
	bridgeReservedHandles    = map[string]bool{
		"system": true, "control_plane": true, "root": true, "admin": true,
		"fleet": true, "owner": true, "operator": true,
	}
)

func bridgeAdminAuthenticator(bridgeToken string) cpserver.AdminAuthFunc {
	return func(
		_ context.Context,
		bearer, claimedID, claimedHandle string,
	) (lifecycle.AdminActor, bool, error) {
		if subtle.ConstantTimeCompare([]byte(bearer), []byte(bridgeToken)) != 1 {
			return lifecycle.AdminActor{}, false, nil
		}
		adminID := strings.TrimSpace(claimedID)
		handle := strings.TrimSpace(claimedHandle)
		if !bridgeAdminIDPattern.MatchString(adminID) ||
			!bridgeAdminHandlePattern.MatchString(handle) ||
			bridgeReservedHandles[handle] {
			return lifecycle.AdminActor{}, false, nil
		}
		return lifecycle.AdminActor{ID: adminID, Handle: handle}, true, nil
	}
}

func explicitBoolEnv(name string) (bool, error) {
	value := strings.TrimSpace(os.Getenv(name))
	switch {
	case value == "", strings.EqualFold(value, "false"):
		return false, nil
	case strings.EqualFold(value, "true"):
		return true, nil
	default:
		return false, fmt.Errorf("%s must be true or false", name)
	}
}
