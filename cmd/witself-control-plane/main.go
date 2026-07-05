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
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/witwave-ai/witself/internal/billing"
	"github.com/witwave-ai/witself/internal/billing/fake"
	"github.com/witwave-ai/witself/internal/billing/lifecycle"
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

// setupBilling wires the plan lifecycle when WITSELF_CP_BILLING_PROVIDER is
// set. Configuration (all env, 12-factor):
//
//	WITSELF_CP_BILLING_PROVIDER  "fake" (Stripe lands as "stripe" later)
//	WITSELF_CP_R2_ENDPOINT       https://<account>.r2.cloudflarestorage.com
//	WITSELF_CP_R2_BUCKET         registry bucket (witself-control-plane)
//	WITSELF_CP_R2_ACCESS_KEY     R2 S3 credentials (Object Read & Write)
//	WITSELF_CP_R2_SECRET_KEY
//	WITSELF_CP_R2_PREFIX         key prefix (default "registry/")
//	WITSELF_CP_DEV_TOKEN         INTERIM auth: the one bearer the plan verbs
//	                             accept, for any account. Dev-only until the
//	                             cell-introspection AuthFunc lands (the CP
//	                             validating operator tokens against the
//	                             account's cell via the directory). Refuses
//	                             to start billing without it — there is no
//	                             default-open mode.
//	WITSELF_CP_RECONCILE         sweep interval (Go duration, default 1m)
func setupBilling(ctx context.Context, mux *http.ServeMux) error {
	providerName := os.Getenv("WITSELF_CP_BILLING_PROVIDER")
	if providerName == "" {
		return nil // billing not configured; the bare CP is a valid deployment
	}

	catalog, err := plans.Load()
	if err != nil {
		return err
	}

	var provider billing.Provider
	switch providerName {
	case "fake":
		// Headless dev provider: every action completes instantly, no
		// partner account required. The Stripe provider replaces this by
		// name — records pin their provider, so the cutover is this env var.
		// The dev token doubles as the fake's webhook signature: the route
		// is public, and an unsigned fake would accept forged entitlement
		// events from anonymous callers.
		provider = fake.New(fake.Config{Prices: catalog.Prices(), WebhookSecret: os.Getenv("WITSELF_CP_DEV_TOKEN")})
	default:
		return fmt.Errorf("unknown WITSELF_CP_BILLING_PROVIDER %q (have: fake)", providerName)
	}
	providers := map[string]billing.Provider{providerName: provider}

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

	devToken := os.Getenv("WITSELF_CP_DEV_TOKEN")
	if devToken == "" {
		return errors.New("WITSELF_CP_DEV_TOKEN is required while billing is enabled (interim auth until cell introspection lands)")
	}

	manager, err := lifecycle.NewManager(lifecycle.Config{
		Catalog:   catalog,
		Providers: providers,
		Default:   providerName,
		Store:     lifecycle.NewR2Store(blobClient, prefix),
		Applier:   unwiredApplier{},
	})
	if err != nil {
		return err
	}
	if err := cpserver.Register(mux, cpserver.Config{
		Manager:   manager,
		Catalog:   catalog,
		Providers: providers,
		Authenticate: func(_ context.Context, _ string, bearer string) (bool, error) {
			return subtle.ConstantTimeCompare([]byte(bearer), []byte(devToken)) == 1, nil
		},
	}); err != nil {
		return err
	}

	interval := time.Minute
	if v := os.Getenv("WITSELF_CP_RECONCILE"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("WITSELF_CP_RECONCILE: %w", err)
		}
		interval = d
	}
	go cpserver.RunReconciler(ctx, manager, interval, func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, format+"\n", args...)
	})
	// Dev-harness: real partners announce period ends by webhook; the fake
	// has no scheduler, so the binary drives its due downgrades on the same
	// cadence — otherwise a "scheduled" downgrade would never take effect.
	if fakeP, ok := provider.(*fake.Fake); ok {
		go func() {
			t := time.NewTicker(interval)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					if events := fakeP.ApplyDue(); len(events) > 0 {
						if err := manager.OnEvents(ctx, providerName, events); err != nil {
							fmt.Fprintf(os.Stderr, "witself-control-plane: fake period-end: %v\n", err)
						}
					}
				}
			}
		}()
	}
	fmt.Fprintf(os.Stderr, "witself-control-plane: billing enabled (provider %s, reconcile %s)\n", providerName, interval)
	return nil
}

// unwiredApplier is the placeholder cell push: it fails loudly so records
// truthfully show Entitled != Applied (and Reconcile keeps retrying) until
// the cell-apply client — the CP calling POST /v1/accounts/{id}:plan on the
// account's cell — lands. Pretending to succeed would hide a real gap.
type unwiredApplier struct{}

func (unwiredApplier) Apply(context.Context, string, string, map[string]int64, []string) error {
	return errors.New("cell apply not wired yet")
}
