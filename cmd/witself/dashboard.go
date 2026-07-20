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
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/dashboard"
	"github.com/witwave-ai/witself/internal/version"
)

const dashboardServeUsage = "usage: witself dashboard serve [--account NAME] [--realm NAME] [--agent NAME] " +
	"[--endpoint URL --token-file FILE] [--port PORT] [--poll DURATION] [--open]"

const dashboardStatusUsage = "usage: witself dashboard status [--agent NAME] [--account NAME] [--realm NAME] [--json]"

const dashboardStopUsage = "usage: witself dashboard stop [--agent NAME] [--account NAME] [--realm NAME] [--json]"

func dashboardCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, dashboardServeUsage)
		fmt.Fprintln(os.Stderr, dashboardStatusUsage)
		fmt.Fprintln(os.Stderr, dashboardStopUsage)
		return 2
	}
	switch args[0] {
	case "serve":
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		return dashboardServe(ctx, args[1:])
	case "status":
		return dashboardStatus(args[1:])
	case "stop":
		return dashboardStop(args[1:])
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

// dashboardStopWait and dashboardStopPoll bound how long stop waits for a
// signaled dashboard to shut down and release its registry entry. Vars only
// so tests can lower them.
var (
	dashboardStopWait = 5 * time.Second
	dashboardStopPoll = 100 * time.Millisecond
)

// signalDashboard delivers SIGINT to a PID the caller has just proven to be
// a live dashboard via dashboard.EntryLive's marker-header probe and the
// owner of its registry entry via dashboard.EntryOwned's access-token probe.
// A var only so tests can stub delivery instead of interrupting themselves.
var signalDashboard = func(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return process.Signal(syscall.SIGINT)
}

// dashboardStop gracefully stops registered dashboards. Like status it is
// purely local — a registry scan plus the same liveness verdict serve uses —
// and it signals SIGINT only after dashboard.EntryLive confirms both the PID
// and the marker-header probe of the recorded port AND dashboard.EntryOwned
// proves the answering dashboard minted this entry's access token. The
// marker alone proves only "some dashboard": after a crash, another agent's
// serve can occupy the recorded port while the recorded PID is reused by an
// unrelated process, and that PID must never be signaled. Stopping when
// nothing is live and owned is a friendly no-op.
func dashboardStop(args []string) int {
	fs := flag.NewFlagSet("dashboard stop", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	agent := fs.String("agent", "", "only dashboards serving this agent name")
	account := fs.String("account", "", "only dashboards under this local account name")
	realm := fs.String("realm", "", "only dashboards in this realm")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, dashboardStopUsage)
		return 2
	}

	entries, err := dashboard.ListRegistryEntries()
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	type dashboardStopEntry struct {
		dashboard.RegistryEntry
		Live    bool `json:"live"`
		Stopped bool `json:"stopped"`
	}
	code := 0
	results := make([]dashboardStopEntry, 0, len(entries))
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
		result := dashboardStopEntry{RegistryEntry: entry}
		if dashboard.EntryLive(entry) && dashboard.EntryOwned(entry) {
			result.Live = true
			if err := stopDashboardEntry(entry); err != nil {
				fmt.Fprintf(os.Stderr, "witself: %v\n", err)
				code = 1
			} else {
				result.Stopped = true
			}
		}
		results = append(results, result)
	}
	if *jsonOut {
		if printCode := printJSON(map[string]any{"dashboards": results}); printCode != 0 {
			return printCode
		}
		return code
	}
	stopped := false
	for _, result := range results {
		if result.Stopped {
			fmt.Printf("stopped dashboard for agent %s (pid %d, port %d)\n", result.AgentName, result.PID, result.Port)
			stopped = true
		}
	}
	if !stopped && code == 0 {
		fmt.Println("no live dashboard to stop")
	}
	return code
}

// stopDashboardEntry signals one just-proven-live-and-owned dashboard and polls briefly
// until it stops answering with the marker header and releases its registry
// entry (serve releases just before process exit), so a reported stop means
// the slot is genuinely free for the next serve.
func stopDashboardEntry(entry dashboard.RegistryEntry) error {
	if err := signalDashboard(entry.PID); err != nil {
		return fmt.Errorf("signal dashboard pid %d: %w", entry.PID, err)
	}
	deadline := time.Now().Add(dashboardStopWait)
	for {
		if dashboardEntryReleased(entry) && !dashboard.EntryLive(entry) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("dashboard for agent %q (pid %d) is still serving %s after SIGINT",
				entry.AgentName, entry.PID, dashboardStopWait)
		}
		time.Sleep(dashboardStopPoll)
	}
}

// dashboardEntryReleased reports whether the signaled serve's registry entry
// is gone (graceful shutdown removes it) or already belongs to another PID.
func dashboardEntryReleased(entry dashboard.RegistryEntry) bool {
	current, err := dashboard.ReadRegistryEntry(entry.AgentID)
	if err != nil {
		return errors.Is(err, os.ErrNotExist)
	}
	return current.PID != entry.PID
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
	open := fs.Bool("open", false, "open the tokened access URL in the OS browser once serving")
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
	return serveDashboard(ctx, listener, cfg, entry, *open)
}

// launchBrowser starts the OS default browser at url without waiting for it;
// the reaper goroutine only prevents a zombie. A var so tests can stub the
// launch, and it runs solely when --open was passed, so tests (and every
// default serve) never spawn a browser.
var launchBrowser = func(url string) error {
	var opener string
	switch runtime.GOOS {
	case "darwin":
		opener = "open"
	case "linux":
		opener = "xdg-open"
	default:
		return fmt.Errorf("no browser launcher for %s", runtime.GOOS)
	}
	command := exec.Command(opener, url)
	if err := command.Start(); err != nil {
		return err
	}
	go func() { _ = command.Wait() }()
	return nil
}

// serveDashboard runs the dashboard HTTP lifecycle on an already-bound
// loopback listener until ctx is canceled, registering the serve in
// ~/.witself/dashboards for local discovery and removing it on shutdown.
func serveDashboard(ctx context.Context, listener net.Listener, cfg dashboard.Config, entry dashboard.RegistryEntry, openBrowser bool) int {
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
	if openBrowser {
		// Fire-and-forget: a launch failure only warns — the banner URL still
		// works — and nothing waits on the browser process.
		if err := launchBrowser(entry.AccessURL); err != nil {
			fmt.Fprintf(os.Stderr, "witself dashboard: warning: open browser: %v\n", err)
		}
	}

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
