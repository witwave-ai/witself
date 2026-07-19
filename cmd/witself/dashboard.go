package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/dashboard"
	"github.com/witwave-ai/witself/internal/version"
)

const dashboardServeUsage = "usage: witself dashboard serve [--account NAME] [--realm NAME] [--agent NAME] " +
	"[--endpoint URL --token-file FILE] [--port PORT] [--poll DURATION]"

const dashboardStatusUsage = "usage: witself dashboard status [--agent NAME] [--account NAME] [--realm NAME] [--json]"

func dashboardCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, dashboardServeUsage)
		fmt.Fprintln(os.Stderr, dashboardStatusUsage)
		return 2
	}
	switch args[0] {
	case "serve":
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		return dashboardServe(ctx, args[1:])
	case "status":
		return dashboardStatus(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "witself dashboard: unknown subcommand %q\n", args[0])
		return 2
	}
}

// dashboardStatus lists the registered local dashboards. It is purely local —
// a registry directory scan plus the same liveness verdict serve uses — so it
// needs no cell round-trip and no token file.
func dashboardStatus(args []string) int {
	fs := flag.NewFlagSet("dashboard status", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	agent := fs.String("agent", "", "only dashboards serving this agent name")
	account := fs.String("account", "", "only dashboards under this local account name")
	realm := fs.String("realm", "", "only dashboards in this realm")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, dashboardStatusUsage)
		return 2
	}

	entries, err := dashboard.ListRegistryEntries()
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	type dashboardStatusEntry struct {
		dashboard.RegistryEntry
		Live bool `json:"live"`
	}
	statuses := make([]dashboardStatusEntry, 0, len(entries))
	for _, entry := range entries {
		if *agent != "" && entry.AgentName != *agent {
			continue
		}
		if *account != "" && entry.Account != *account {
			continue
		}
		if *realm != "" && entry.Realm != *realm {
			continue
		}
		statuses = append(statuses, dashboardStatusEntry{RegistryEntry: entry, Live: dashboard.EntryLive(entry)})
	}
	if *jsonOut {
		return printJSON(map[string]any{"dashboards": statuses})
	}

	w, flush := tableWriter("agent\trealm\tport\tpid\tstarted (UTC)\tstate\turl")
	for _, status := range statuses {
		state, open := "stale", "-"
		if status.Live {
			state = "live"
			if status.AccessURL != "" {
				open = status.AccessURL
			}
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%s\t%s\t%s\n",
			tabSafe(status.AgentName), tabSafe(status.Realm), status.Port, status.PID,
			status.StartedAt.UTC().Format(time.RFC3339), state, tabSafe(open))
	}
	flush()
	return 0
}

// dashboardServe resolves the agent connection and hands a bound loopback
// listener to serveDashboard. It takes ctx so tests can drive the full flow
// in-process against httptest backends and cancel instead of signaling.
func dashboardServe(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("dashboard serve", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account, realm, agent, endpoint, tokenFile := factConnectionFlags(fs)
	port := fs.Int("port", 0, "listen port on 127.0.0.1 (0 = derived from the agent id)")
	poll := fs.Duration("poll", 2*time.Second, "cell poll interval for live updates")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, dashboardServeUsage)
		return 2
	}

	conn, err := connectAgent(ctx, *account, *realm, *agent, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	self, err := client.GetSelf(ctx, conn.Endpoint, conn.Token, client.SelfOptions{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	if err := verifySelfCardConnection(conn, self.Identity); err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}

	if entry, live, err := dashboard.LiveRegistryEntry(self.Identity.AgentID); err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	} else if live {
		fmt.Fprintf(os.Stderr, "witself: a dashboard for agent %q is already serving on %s (pid %d); stop it first\n",
			entry.AgentName, entry.URL, entry.PID)
		return 1
	}

	accessToken, err := newDashboardAccessToken()
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	listener, err := listenDashboard(*port, dashboard.DefaultPort(self.Identity.AgentID))
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}

	cfg := dashboard.Config{
		Endpoint:     conn.Endpoint,
		BearerToken:  conn.Token,
		AccessToken:  accessToken,
		Identity:     self.Identity,
		Version:      version.Version,
		PollInterval: *poll,
	}
	entry := dashboard.RegistryEntry{
		AgentID:   self.Identity.AgentID,
		AgentName: self.Identity.AgentName,
		Account:   conn.AccountName,
		Realm:     self.Identity.RealmName,
	}
	return serveDashboard(ctx, listener, cfg, entry)
}

// serveDashboard runs the dashboard HTTP lifecycle on an already-bound
// loopback listener until ctx is canceled, registering the serve in
// ~/.witself/dashboards for local discovery and removing it on shutdown.
func serveDashboard(ctx context.Context, listener net.Listener, cfg dashboard.Config, entry dashboard.RegistryEntry) int {
	port := listenerPort(listener)
	entry.Port = port
	entry.PID = os.Getpid()
	entry.URL = fmt.Sprintf("http://127.0.0.1:%d/", port)
	entry.AccessURL = fmt.Sprintf("http://127.0.0.1:%d/?token=%s", port, cfg.AccessToken)
	entry.StartedAt = time.Now().UTC()

	mux := http.NewServeMux()
	if err := dashboard.Register(mux, cfg); err != nil {
		_ = listener.Close()
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		// Derive every request context from the serve ctx so signal-driven
		// shutdown also ends open SSE streams; otherwise Shutdown waits its
		// full timeout for connections that never go idle.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	errc := make(chan error, 1)
	go func() {
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()
	backOff := func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}

	// Claim the registry slot only once this process answers on its port:
	// the claim is lock-serialized against any racing serve and the loser's
	// liveness probe of the winner must see the marker header. Two serves
	// racing past the pre-bind check therefore resolve to exactly one
	// registered survivor; the loser backs off without deleting its entry.
	survivor, claimed, err := dashboard.ClaimRegistryEntry(entry)
	if err != nil {
		backOff()
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	if !claimed {
		backOff()
		fmt.Fprintf(os.Stderr, "witself: a dashboard for agent %q is already serving on %s (pid %d); stop it first\n",
			survivor.AgentName, survivor.URL, survivor.PID)
		return 1
	}
	defer func() {
		if err := dashboard.ReleaseRegistryEntry(entry.AgentID, entry.PID); err != nil {
			fmt.Fprintf(os.Stderr, "witself: warning: remove dashboard registry entry: %v\n", err)
		}
	}()
	fmt.Fprintf(os.Stderr, "witself dashboard: serving agent %s on %s\n",
		entry.AgentName, entry.AccessURL)

	select {
	case <-ctx.Done():
	case err := <-errc:
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		fmt.Fprintf(os.Stderr, "witself dashboard: shut down with connections still open: %v\n", err)
		return 0
	}
	fmt.Fprintln(os.Stderr, "witself dashboard: shut down cleanly")
	return 0
}

// listenDashboard binds the loopback listener. An explicitly requested port
// binds exactly; the derived default falls back to the next free port
// (+1..+20), then an ephemeral one, so a busy machine still serves.
func listenDashboard(requested, derived int) (net.Listener, error) {
	if requested != 0 {
		return net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", requested))
	}
	for offset := 0; offset <= 20; offset++ {
		listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", derived+offset))
		if err == nil {
			return listener, nil
		}
	}
	return net.Listen("tcp", "127.0.0.1:0")
}

func listenerPort(listener net.Listener) int {
	if addr, ok := listener.Addr().(*net.TCPAddr); ok {
		return addr.Port
	}
	return 0
}

// newDashboardAccessToken mints the per-process 32-hex-char URL token that
// guards the local HTTP surface.
func newDashboardAccessToken() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate dashboard access token: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}
