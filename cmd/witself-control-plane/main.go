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
	"unicode"

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
	if len(os.Args) > 1 && os.Args[1] == "billing-rollout-inventory" {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		if err := runBillingRolloutInventory(ctx, os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "witself-control-plane: billing rollout inventory: %v\n", err)
			return 1
		}
		return 0
	}
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
//	WITSELF_CP_STRIPE_MODE            exact "test" or "live" key mode (stripe)
//	WITSELF_CP_STRIPE_SECRET_KEY      matching sk_test_/sk_live_ API key (stripe)
//	WITSELF_CP_STRIPE_WEBHOOK_SECRET  whsec_ signing secret       (stripe)
//	WITSELF_CP_STRIPE_SUCCESS_URL     canonical HTTPS checkout success URL
//	WITSELF_CP_STRIPE_CANCEL_URL      canonical HTTPS checkout cancel URL
//	WITSELF_CP_STRIPE_PORTAL_RETURN_URL canonical HTTPS portal return URL
//	WITSELF_CP_STRIPE_PORTAL_CONFIGURATION_ID reviewed safe bpc_ configuration
//	WITSELF_CP_STRIPE_AUTOMATIC_TAX   explicit true/false Stripe Tax switch;
//	  requires an activated Stripe Tax registration set before it is enabled
//	WITSELF_CP_STRIPE_TEST_CLOCK_ID optional clock_ for test-mode acceptance only;
//	                                    allows at most one cohort account
//	WITSELF_CP_BILLING_GENERAL_AVAILABILITY explicit true/false; true opens
//	  billing to every account and requires an empty allowlist and no test clock
//	WITSELF_CP_BILLING_ACCOUNT_ALLOWLIST strict comma-separated account cohort;
//	                                         empty enables no customer mutations
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
	var billingMutationGate cpserver.BillingMutationGateFunc
	switch providerName {
	case "":
		// Providerless/manual mode is intentional. It powers defaults,
		// administrator overrides, seeding, status, and cell enforcement
		// without pretending a customer can purchase or cancel anything.
	case "stripe":
		stripeConfig, gate, err := stripeControlPlaneConfig(catalog)
		if err != nil {
			return err
		}
		sp, err := stripeprovider.New(stripeConfig)
		if err != nil {
			return err
		}
		provider = sp
		billingMutationGate = gate
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
	fitChecker := cpserver.NewBridgeFitChecker(bridgeURL, bridgeToken)
	authenticate := cpserver.CellAuthenticate(cellResolve)
	manager, err := lifecycle.NewManager(lifecycle.Config{
		Catalog:             catalog,
		Providers:           providers,
		Default:             providerName,
		Store:               lifecycle.NewR2Store(blobClient, prefix),
		Applier:             applier,
		Fit:                 fitChecker,
		BillingMutationGate: billingMutationGate,
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
		BillingMutationGate:  billingMutationGate,
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

const maxBillingAllowlistAccounts = 1024

var (
	billingAllowlistAccountIDPattern   = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	stripePortalConfigurationIDPattern = regexp.MustCompile(`^bpc_[A-Za-z0-9]{1,123}$`)
	stripeTestClockIDPattern           = regexp.MustCompile(`^clock_[A-Za-z0-9]{1,123}$`)
	stripeWebhookSecretPattern         = regexp.MustCompile(`^whsec_[A-Za-z0-9]{1,250}$`)
)

// stripeControlPlaneConfig validates every safety-sensitive Stripe setting
// before a provider is constructed or any catalog bootstrap can reach Stripe.
// The returned mutation gate is always non-nil: an empty cohort intentionally
// denies every customer billing mutation while webhook reconciliation and
// billing reads remain available.
func stripeControlPlaneConfig(
	catalog *plans.Catalog,
) (stripeprovider.Config, cpserver.BillingMutationGateFunc, error) {
	mode := os.Getenv("WITSELF_CP_STRIPE_MODE")
	secretKey := os.Getenv("WITSELF_CP_STRIPE_SECRET_KEY")
	if err := validateStripeModeAndKey(mode, secretKey); err != nil {
		return stripeprovider.Config{}, nil, err
	}

	// The webhook secret is mandatory: without it the binary would boot,
	// mint checkout links, take payments, and refuse every webhook delivery.
	webhookSecret := os.Getenv("WITSELF_CP_STRIPE_WEBHOOK_SECRET")
	if webhookSecret == "" || webhookSecret != strings.TrimSpace(webhookSecret) ||
		!stripeWebhookSecretPattern.MatchString(webhookSecret) {
		return stripeprovider.Config{}, nil, errors.New("WITSELF_CP_STRIPE_WEBHOOK_SECRET must be a non-empty canonical whsec_ Stripe signing secret: without a valid secret webhooks are refused and paid activations are lost")
	}

	successURL, err := canonicalHTTPSURLFromEnv("WITSELF_CP_STRIPE_SUCCESS_URL")
	if err != nil {
		return stripeprovider.Config{}, nil, err
	}
	cancelURL, err := canonicalHTTPSURLFromEnv("WITSELF_CP_STRIPE_CANCEL_URL")
	if err != nil {
		return stripeprovider.Config{}, nil, err
	}
	portalReturnURL, err := canonicalHTTPSURLFromEnv("WITSELF_CP_STRIPE_PORTAL_RETURN_URL")
	if err != nil {
		return stripeprovider.Config{}, nil, err
	}

	portalConfigurationID := os.Getenv("WITSELF_CP_STRIPE_PORTAL_CONFIGURATION_ID")
	if portalConfigurationID == "" ||
		portalConfigurationID != strings.TrimSpace(portalConfigurationID) ||
		!stripePortalConfigurationIDPattern.MatchString(portalConfigurationID) {
		return stripeprovider.Config{}, nil, errors.New("WITSELF_CP_STRIPE_PORTAL_CONFIGURATION_ID must be a canonical bpc_ Stripe configuration id")
	}
	testClockID := os.Getenv("WITSELF_CP_STRIPE_TEST_CLOCK_ID")
	if testClockID != "" {
		if mode != "test" {
			return stripeprovider.Config{}, nil, errors.New("WITSELF_CP_STRIPE_TEST_CLOCK_ID is allowed only when WITSELF_CP_STRIPE_MODE=test")
		}
		if testClockID != strings.TrimSpace(testClockID) ||
			!stripeTestClockIDPattern.MatchString(testClockID) {
			return stripeprovider.Config{}, nil, errors.New("WITSELF_CP_STRIPE_TEST_CLOCK_ID must be a canonical clock_ Stripe test-clock id")
		}
	}

	allowedAccounts, err := parseBillingAccountAllowlist(
		os.Getenv("WITSELF_CP_BILLING_ACCOUNT_ALLOWLIST"))
	if err != nil {
		return stripeprovider.Config{}, nil, err
	}
	if testClockID != "" && len(allowedAccounts) > 1 {
		return stripeprovider.Config{}, nil, errors.New(
			"WITSELF_CP_STRIPE_TEST_CLOCK_ID requires at most one account in WITSELF_CP_BILLING_ACCOUNT_ALLOWLIST")
	}
	// General availability is the deliberate end of the cohort. It is explicit
	// and fails closed: absent or false keeps the allowlist as the only way in.
	generalAvailability, err := explicitBoolEnv("WITSELF_CP_BILLING_GENERAL_AVAILABILITY")
	if err != nil {
		return stripeprovider.Config{}, nil, err
	}
	if generalAvailability {
		// A non-empty allowlist alongside GA is contradictory, and the
		// dangerous reading is the reassuring one: an operator would believe
		// billing is still restricted to that cohort when it is open to
		// everyone. Refuse rather than silently widen.
		if len(allowedAccounts) > 0 {
			return stripeprovider.Config{}, nil, errors.New(
				"WITSELF_CP_BILLING_GENERAL_AVAILABILITY=true requires an empty WITSELF_CP_BILLING_ACCOUNT_ALLOWLIST: a cohort alongside general availability reads as a restriction that is not being applied")
		}
		// A test clock rewrites time for every customer it is attached to. That
		// is an acceptance tool for a single sandbox account, never something to
		// run while billing is open to everyone.
		if testClockID != "" {
			return stripeprovider.Config{}, nil, errors.New(
				"WITSELF_CP_STRIPE_TEST_CLOCK_ID must not be set with WITSELF_CP_BILLING_GENERAL_AVAILABILITY=true")
		}
	}
	gate := billingMutationGate(allowedAccounts, generalAvailability)

	// Stripe Tax stays off unless it is explicitly switched on, because
	// calculating tax without an activated registration set fails the purchase.
	automaticTax, err := explicitBoolEnv("WITSELF_CP_STRIPE_AUTOMATIC_TAX")
	if err != nil {
		return stripeprovider.Config{}, nil, err
	}

	return stripeprovider.Config{
		SecretKey:             secretKey,
		WebhookSecret:         webhookSecret,
		Catalog:               catalog,
		SuccessURL:            successURL,
		CancelURL:             cancelURL,
		PortalReturnURL:       portalReturnURL,
		PortalConfigurationID: portalConfigurationID,
		TestClockID:           testClockID,
		AutomaticTax:          automaticTax,
	}, gate, nil
}

func validateStripeModeAndKey(mode, secretKey string) error {
	if mode != "test" && mode != "live" {
		return errors.New("WITSELF_CP_STRIPE_MODE must be exactly test or live")
	}
	if secretKey == "" || secretKey != strings.TrimSpace(secretKey) {
		return errors.New("WITSELF_CP_STRIPE_SECRET_KEY must be a non-empty canonical Stripe secret key")
	}
	wantPrefix := "sk_" + mode + "_"
	if !strings.HasPrefix(secretKey, wantPrefix) || len(secretKey) == len(wantPrefix) {
		return fmt.Errorf("WITSELF_CP_STRIPE_SECRET_KEY must have the exact %s prefix when WITSELF_CP_STRIPE_MODE=%s", wantPrefix, mode)
	}
	return nil
}

func canonicalHTTPSURLFromEnv(name string) (string, error) {
	rawURL := os.Getenv(name)
	if rawURL == "" || rawURL != strings.TrimSpace(rawURL) ||
		strings.IndexFunc(rawURL, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("%s must be a canonical HTTPS URL without credentials, control characters, query, or fragment", name)
	}
	parsed, err := neturl.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" ||
		parsed.Hostname() == "" || parsed.User != nil || parsed.Opaque != "" ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" ||
		parsed.RawFragment != "" || parsed.String() != rawURL {
		return "", fmt.Errorf("%s must be a canonical HTTPS URL without credentials, control characters, query, or fragment", name)
	}
	return rawURL, nil
}

func billingAccountAllowlistGate(
	raw string,
) (cpserver.BillingMutationGateFunc, error) {
	allowed, err := parseBillingAccountAllowlist(raw)
	if err != nil {
		return nil, err
	}
	// Allowlist-only: this builder predates general availability and keeps
	// meaning exactly the pre-GA cohort.
	return billingMutationGate(allowed, false), nil
}

func parseBillingAccountAllowlist(raw string) (map[string]struct{}, error) {
	allowed := make(map[string]struct{})
	if raw != "" {
		if raw != strings.TrimSpace(raw) {
			return nil, errors.New("WITSELF_CP_BILLING_ACCOUNT_ALLOWLIST must be a strict comma-separated list without whitespace")
		}
		parts := strings.Split(raw, ",")
		if len(parts) > maxBillingAllowlistAccounts {
			return nil, fmt.Errorf("WITSELF_CP_BILLING_ACCOUNT_ALLOWLIST may contain at most %d accounts", maxBillingAllowlistAccounts)
		}
		for _, accountID := range parts {
			if accountID == "" || accountID != strings.TrimSpace(accountID) ||
				!billingAllowlistAccountIDPattern.MatchString(accountID) {
				return nil, errors.New("WITSELF_CP_BILLING_ACCOUNT_ALLOWLIST must contain only canonical account ids separated by commas without whitespace")
			}
			if _, exists := allowed[accountID]; exists {
				return nil, fmt.Errorf("WITSELF_CP_BILLING_ACCOUNT_ALLOWLIST contains duplicate account %q", accountID)
			}
			allowed[accountID] = struct{}{}
		}
	}
	return allowed, nil
}

// billingMutationGate decides which accounts may mutate billing. Before general
// availability that is exactly the reviewed cohort; after it, every account. The
// zero configuration — no allowlist, no general availability — admits nobody.
func billingMutationGate(
	allowed map[string]struct{},
	generalAvailability bool,
) cpserver.BillingMutationGateFunc {
	return func(_ context.Context, accountID string) (bool, error) {
		if generalAvailability {
			return true, nil
		}
		_, ok := allowed[accountID]
		return ok, nil
	}
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
		// The AI support assistant's fixed posting identity. Reserved so no
		// human admin credential can post replies that render as the
		// assistant — the published support policy's labeling promise depends
		// on this kind/handle pair being unforgeable in both directions.
		"assistant": true,
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
