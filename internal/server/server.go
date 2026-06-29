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
	"net"
	"net/http"
	"os"
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
		{"api", cfg.APIAddr, apiMux()},
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

func apiMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/version", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, "{\"schema_version\":\"witself.v0\",\"version\":%q,\"commit\":%q,\"date\":%q}\n",
			version.Version, version.Commit, version.Date)
	})
	mux.HandleFunc("/v1/capabilities", capabilitiesHandler)
	return mux
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

type capabilities struct {
	SchemaVersion string             `json:"schema_version"`
	Backend       backendInfo        `json:"backend"`
	Principal     any                `json:"principal"` // null until token auth exists
	Features      map[string]feature `json:"features"`
	Limits        map[string]any     `json:"limits"`
}

func capabilitiesHandler(w http.ResponseWriter, _ *http.Request) {
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
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(caps)
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
