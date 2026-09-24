package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
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

const dashboardOpenUsage = "usage: witself dashboard open [--account NAME] [--realm NAME] [--agent NAME] [--endpoint URL --token-file FILE] [--print-url]"

const dashboardOpenHint = "run witself dashboard open with the same account, realm, and agent selectors; add --print-url to deliberately reveal the opening URL for a manual browser"

func dashboardCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, dashboardServeUsage)
		fmt.Fprintln(os.Stderr, dashboardStatusUsage)
		fmt.Fprintln(os.Stderr, dashboardStopUsage)
		fmt.Fprintln(os.Stderr, dashboardOpenUsage)
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
	case "open":
		return dashboardOpen(context.Background(), args[1:])
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
		fmt.Fprintln(os.Stderr, "witself: local dashboard operation could not complete")
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
		statuses = append(statuses, dashboardStatusEntry{RegistryEntry: dashboard.PublicRegistryEntry(entry), Live: dashboard.EntryLive(entry)})
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

// signalDashboard requests shutdown of a process the caller has just proven to be
// a live dashboard via dashboard.EntryLive's marker-header probe and the
// owner of its registry entry via dashboard.EntryOwned's access-token probe.
// A var only so tests can stub delivery instead of interrupting themselves.
var signalDashboard = signalDashboardProcess

// dashboardStop gracefully stops registered dashboards. Like status it is
// purely local — a registry scan plus the same liveness verdict serve uses —
// and it requests shutdown only after dashboard.EntryLive confirms both the PID
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
		fmt.Fprintln(os.Stderr, "witself: local dashboard operation could not complete")
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
		result := dashboardStopEntry{RegistryEntry: dashboard.PublicRegistryEntry(entry)}
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
	if err := signalDashboard(entry); err != nil {
		return errors.New("dashboard process could not be signaled")
	}
	deadline := time.Now().Add(dashboardStopWait)
	for {
		if dashboardEntryReleased(entry) && !dashboard.EntryLive(entry) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("dashboard for agent %q (pid %d) is still serving %s after %s",
				entry.AgentName, entry.PID, dashboardStopWait, dashboardStopRequestName)
		}
		time.Sleep(dashboardStopPoll)
	}
}

// dashboardEntryReleased reports whether the signaled serve's registry entry
// is gone (graceful shutdown removes it) or belongs to another exact instance.
func dashboardEntryReleased(entry dashboard.RegistryEntry) bool {
	current, err := dashboard.ReadRegistryEntry(entry.AgentID)
	if err != nil {
		return errors.Is(err, os.ErrNotExist)
	}
	return !dashboard.SameRegistryInstance(current, entry)
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

	conn, err := accountConsoleConnect(ctx, *account, *realm, *agent, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "witself: local dashboard operation could not complete")
		return 1
	}
	self, err := client.GetSelf(ctx, conn.Endpoint, conn.Token, client.SelfOptions{})
	if err != nil {
		fmt.Fprintln(os.Stderr, "witself: local dashboard operation could not complete")
		return 1
	}
	if err := verifySelfCardConnection(conn, self.Identity); err != nil {
		fmt.Fprintln(os.Stderr, "witself: local dashboard operation could not complete")
		return 1
	}

	if _, live, err := dashboard.LiveRegistryEntry(self.Identity.AgentID); err != nil {
		fmt.Fprintln(os.Stderr, "witself: local dashboard operation could not complete")
		return 1
	} else if live {
		fmt.Fprintln(os.Stderr, "witself: a dashboard already occupies this agent session; stop it explicitly before restarting")
		return 1
	}

	accessToken, err := newDashboardAccessToken()
	if err != nil {
		fmt.Fprintln(os.Stderr, "witself: local dashboard operation could not complete")
		return 1
	}
	listener, err := listenDashboard(*port, dashboard.DefaultPort(self.Identity.AgentID))
	if err != nil {
		fmt.Fprintln(os.Stderr, "witself: local dashboard operation could not complete")
		return 1
	}

	manager := resolveAccountConsoleManager(ctx, conn, self.Identity, accountConsoleExplicit(fs), accountConsoleLocate)
	roots := accountConsoleLocalRoots()
	cfg := dashboard.Config{
		AccountManager: manager, AccountClientsScan: newAccountConsoleScanner(ctx, roots, manager),
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
	return serveDashboardLifecycle(ctx, listener, cfg, entry, openBrowser, os.Stderr, nil)
}

// A non-nil ready callback selects the private managed lifecycle: no banner,
// no CLI browser wrapper, and no replacement of an unverified registry record.
func serveDashboardLifecycle(ctx context.Context, listener net.Listener, cfg dashboard.Config, entry dashboard.RegistryEntry, openBrowser bool, output io.Writer, ready func() error) int {
	defer func() { _ = listener.Close() }()
	port := listenerPort(listener)
	entry.SchemaVersion = dashboard.RegistrySchemaVersion
	entry.AccountID, entry.RealmID = cfg.Identity.AccountID, cfg.Identity.RealmID
	entry.Endpoint, _ = normalizeConsoleEndpoint(cfg.Endpoint)
	entry.Port = port
	entry.PID = os.Getpid()
	entry.URL = fmt.Sprintf("http://127.0.0.1:%d/", port)
	entry.AccessURL = fmt.Sprintf("http://127.0.0.1:%d/?token=%s", port, cfg.AccessToken)
	entry.StartedAt = time.Now().UTC()
	entry.ViewerContract = dashboard.ViewerSchema
	entry.Manager = nil
	if cfg.AccountManager != nil {
		p := cfg.AccountManager.Identity
		entry.Manager = &dashboard.ViewerBinding{AccountID: p.AccountID, OperatorID: p.OperatorID, Role: p.Role}
	}

	// Windows has no Process.Signal(SIGINT). Register its private shutdown
	// event before publishing the registry entry; Unix retains signal shutdown.
	ctx, releaseStop, err := registerDashboardStop(ctx, entry)
	if err != nil {
		_ = listener.Close()
		_, _ = fmt.Fprintln(output, "witself: dashboard shutdown registration failed")
		return 1
	}
	defer releaseStop()

	mux := http.NewServeMux()
	if err := dashboard.Register(mux, cfg); err != nil {
		_ = listener.Close()
		_, _ = fmt.Fprintln(output, "witself: local dashboard operation could not complete")
		return 1
	}
	srv := &http.Server{
		Handler:           mux,
		ErrorLog:          log.New(output, "", 0),
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
		_ = srv.Close()
	}

	// Claim the registry slot only once this process answers on its port:
	// the claim is lock-serialized against any racing serve and the loser's
	// liveness probe of the winner must see the marker header. Two serves
	// racing past the pre-bind check therefore resolve to exactly one
	// registered survivor; the loser backs off without deleting its entry.
	var claimed bool
	if ready != nil {
		claimed, err = dashboard.ClaimManagedRegistryEntry(ctx, entry)
	} else {
		_, claimed, err = dashboard.ClaimRegistryEntry(entry)
	}
	if err != nil {
		backOff()
		_, _ = fmt.Fprintln(output, "witself: local dashboard operation could not complete")
		return 1
	}
	if !claimed {
		backOff()
		_, _ = fmt.Fprintln(output, "witself: a dashboard already occupies this agent session; stop it explicitly before restarting")
		return 1
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if _, err := dashboard.ReleaseRegistryInstance(releaseCtx, entry); err != nil {
			_, _ = fmt.Fprintln(output, "witself: dashboard registry cleanup could not complete")
		}
	}()
	if ready != nil {
		if ctx.Err() != nil || ready() != nil {
			backOff()
			return 1
		}
	}
	bannerURL := entry.AccessURL
	if entry.Manager != nil {
		bannerURL = entry.URL
	}
	_, _ = fmt.Fprintf(output, "witself dashboard: serving agent %s on %s\n", entry.AgentName, bannerURL)
	if entry.Manager != nil && ready == nil {
		_, _ = fmt.Fprintln(output, "witself dashboard:", dashboardOpenHint)
	}
	if openBrowser && ready == nil {
		if cfg.AccountManager != nil {
			if _, err := client.RevalidateAccountConsoleManager(ctx, *cfg.AccountManager); err != nil {
				_, _ = fmt.Fprintln(output, "witself: account console authority could not be verified; browser was not opened")
				backOff()
				return 1
			}
		}
		// This process may open its own freshly verified context.
		if err := launchBrowser(entry.AccessURL); err != nil {
			_, _ = fmt.Fprintln(output, "witself dashboard: browser could not open")
			_, _ = fmt.Fprintln(output, "witself dashboard:", dashboardOpenHint)
		}
	}

	select {
	case <-ctx.Done():
	case <-errc:
		_, _ = fmt.Fprintln(output, "witself: local dashboard operation could not complete")
		return 1
	}
	defer func() { _ = srv.Close() }()
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		_, _ = fmt.Fprintln(output, "witself dashboard: shut down with connections still open")
		return 0
	}
	_, _ = fmt.Fprintln(output, "witself dashboard: shut down cleanly")
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
