// Command demo renders the real terminal workspace and serves the real local
// Agent Console using closed synthetic fixtures only. It never loads a user
// profile, registry, credential, endpoint setting, or account manager.
package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/witwave-ai/witself/internal/agenttui"
	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/dashboard"
	"github.com/witwave-ai/witself/internal/dashboard/stubcell"
)

var demoIdentity = client.SelfIdentity{
	AccountID: "acct_studio_demo", AgentID: "agent_atlas_demo", AgentName: "Atlas",
	RealmID: "realm_studio_demo", RealmName: "studio",
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdin, os.Stdout); err != nil {
		// The access URL is private process plumbing, never diagnostic text.
		fmt.Fprintln(os.Stderr, "product screenshot demo: failed")
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
	flags := flag.NewFlagSet("product-screenshot-demo", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	ansiOut := flags.String("ansi-out", "", "destination for the synthetic terminal snapshot")
	if err := flags.Parse(args); err != nil || *ansiOut == "" || flags.NArg() != 0 {
		return errors.New("invalid arguments")
	}
	view, err := terminalSnapshot(ctx)
	if err != nil {
		return err
	}
	if err := os.WriteFile(*ansiOut, []byte(view), 0o600); err != nil {
		return errors.New("snapshot write failed")
	}
	url, closeDemo, err := startDemo(ctx)
	if err != nil {
		return err
	}
	defer closeDemo()
	if err := json.NewEncoder(stdout).Encode(struct {
		URL string `json:"url"`
	}{url}); err != nil {
		return errors.New("announcement failed")
	}
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, stdin)
		close(done)
	}()
	select {
	case <-ctx.Done():
	case <-done:
	}
	return nil
}

func terminalSnapshot(ctx context.Context) (string, error) {
	profile := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(profile)
	model := agenttui.New(ctx, agenttui.NewDemoSource(), agenttui.Options{
		Agent: "Atlas", Realm: "studio", Version: "demo", Theme: "console", Demo: true,
	})
	defer model.Close()
	_, cmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'4'}})
	for i := 0; cmd != nil && i < 8; i++ {
		_, cmd = model.Update(cmd())
	}
	if cmd != nil || ctx.Err() != nil {
		return "", errors.New("terminal fixture did not settle")
	}
	return model.Snapshot(120, 40), nil
}

func randomToken() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", errors.New("synthetic session creation failed")
	}
	return hex.EncodeToString(raw[:]), nil
}

// startDemo owns two ephemeral loopback listeners. Neither endpoint nor any
// authorization material can be supplied by the caller or environment.
func startDemo(ctx context.Context) (string, func(), error) {
	ctx, cancel := context.WithCancel(ctx)
	bearer, err := randomToken()
	if err != nil {
		cancel()
		return "", nil, err
	}
	access, err := randomToken()
	if err != nil {
		cancel()
		return "", nil, err
	}
	upstream, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		cancel()
		return "", nil, errors.New("fixture listener failed")
	}
	console, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		_ = upstream.Close()
		cancel()
		return "", nil, errors.New("console listener failed")
	}
	mux := http.NewServeMux()
	if err := dashboard.Register(mux, dashboard.Config{
		Endpoint: "http://" + upstream.Addr().String(), BearerToken: bearer,
		AccessToken: access, Identity: demoIdentity, Version: "demo",
	}); err != nil {
		_ = upstream.Close()
		_ = console.Close()
		cancel()
		return "", nil, errors.New("console registration failed")
	}
	newServer := func(handler http.Handler) *http.Server {
		return &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second,
			IdleTimeout: 30 * time.Second, MaxHeaderBytes: 32 << 10,
			BaseContext: func(net.Listener) context.Context { return ctx }}
	}
	fixtureServer := newServer(demoCell(bearer))
	consoleServer := newServer(mux)
	go func() { _ = fixtureServer.Serve(upstream) }()
	go func() { _ = consoleServer.Serve(console) }()
	closeDemo := func() {
		cancel()
		_ = consoleServer.Close()
		_ = fixtureServer.Close()
	}
	return "http://" + console.Addr().String() + "/?token=" + access, closeDemo, nil
}

// demoCell adapts only the shared demo's self and memory projections to the
// public API wire shape. Everything else remains the existing safe stub cell.
// The actual dashboard handlers, API client decoding, and browser rendering
// all run unchanged; no HTML, DOM, or production package seams are substituted.
func demoCell(bearer string) http.Handler {
	source := agenttui.NewDemoSource()
	fallback := stubcell.New(stubcell.Config{BearerToken: bearer, Identity: demoIdentity})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fail := func(status int) {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"error":"demo resource unavailable"}`)
		}
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+bearer)) != 1 {
			fail(http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodGet {
			fail(http.StatusMethodNotAllowed)
			return
		}
		request := dashboard.ReadRequest{Query: r.URL.Query()}
		switch {
		case r.URL.Path == "/v1/self":
			request.Resource = dashboard.ResourceSelf
		case r.URL.Path == "/v1/memories":
			request.Resource = dashboard.ResourceMemories
		case strings.HasPrefix(r.URL.Path, "/v1/memories/"):
			parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/memories/"), "/")
			if len(parts) > 2 || parts[0] == "" || (len(parts) == 2 && parts[1] != "history") {
				fail(http.StatusNotFound)
				return
			}
			request.Resource, request.ID = dashboard.ResourceMemory, parts[0]
			// The demo history reader is intentionally generic. Validate the
			// requested memory exists before allowing that history projection.
			if _, err := source.Read(r.Context(), request); err != nil {
				fail(http.StatusNotFound)
				return
			}
			if len(parts) == 2 {
				request.Resource = dashboard.ResourceMemoryHistory
			}
		default:
			fallback.ServeHTTP(w, r)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		body, err := source.Read(ctx, request)
		if err != nil {
			fail(http.StatusNotFound)
			return
		}
		if request.Resource == dashboard.ResourceSelf {
			var digest client.SelfDigest
			if json.Unmarshal(body, &digest) != nil {
				fail(http.StatusInternalServerError)
				return
			}
			digest.SchemaVersion, digest.Identity = "witself.v0", demoIdentity
			if digest.PlanEntitlements != nil {
				digest.PlanEntitlements.SchemaVersion = "witself.agent-entitlements.v1"
			}
			body, err = json.Marshal(digest)
			if err != nil {
				fail(http.StatusInternalServerError)
				return
			}
		}
		_, _ = w.Write(body)
	})
}
