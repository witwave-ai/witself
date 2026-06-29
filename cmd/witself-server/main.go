// Command witself-server is the Witself backend API server. It supports version
// and a serve command that binds the API, health-probe, and metrics listeners.
// The full backend is specified under docs/.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/witwave-ai/witself/internal/server"
	"github.com/witwave-ai/witself/internal/store"
	"github.com/witwave-ai/witself/internal/version"
)

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
		cfg.Ready = st.Ping
		fmt.Fprintln(os.Stderr, "witself-server: database migrated; /readyz gates on it")
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

// dbDSN resolves the Postgres DSN from the environment, preferring the
// WITSELF_-prefixed name and falling back to the conventional DATABASE_URL.
func dbDSN() string {
	if v := os.Getenv("WITSELF_DATABASE_URL"); v != "" {
		return v
	}
	return os.Getenv("DATABASE_URL")
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "witself-server — the Witself backend API server")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  witself-server version    Print version information")
	fmt.Fprintln(w, "  witself-server serve      Run the API, health, and metrics listeners")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Listeners (override with env):")
	fmt.Fprintln(w, "  WITSELF_API_ADDR      default :8080  (/v1 API)")
	fmt.Fprintln(w, "  WITSELF_HEALTH_ADDR   default :8081  (/livez /readyz /startupz)")
	fmt.Fprintln(w, "  WITSELF_METRICS_ADDR  default :9090  (/metrics)")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Database (optional; when set, /readyz gates on it):")
	fmt.Fprintln(w, "  WITSELF_DATABASE_URL  Postgres DSN (falls back to DATABASE_URL)")
}
