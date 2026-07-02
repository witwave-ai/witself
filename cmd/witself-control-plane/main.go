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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

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
