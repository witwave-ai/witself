package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/dashboard"
	"github.com/witwave-ai/witself/internal/local"
)

func dashboardOpenTestSelection(t *testing.T, conn agentConnection) {
	t.Helper()
	if err := local.Save("selected", local.Account{ID: conn.AccountID}, accountFakeBearer); err != nil {
		t.Fatal(err)
	}
	oldConnect, oldLocate, oldLaunch := accountConsoleConnect, accountConsoleLocate, launchBrowser
	t.Cleanup(func() { accountConsoleConnect, accountConsoleLocate, launchBrowser = oldConnect, oldLocate, oldLaunch })
	accountConsoleConnect = func(context.Context, string, string, string, string, string) (agentConnection, error) {
		return conn, nil
	}
	accountConsoleLocate = func(context.Context, string, string) (string, string, error) { return "", conn.Endpoint, nil }
}

func TestDashboardManagerManualOpeningRecovery(t *testing.T) {
	for _, open := range []bool{false, true} {
		name := "without-open"
		if open {
			name = "browser-launch-failure"
		}
		t.Run(name, func(t *testing.T) {
			consoleTestHome(t)
			conn, _, _ := accountTestBackend(t)
			dashboardOpenTestSelection(t, conn)
			launched := make(chan struct{}, 1)
			launchBrowser = func(string) error { launched <- struct{}{}; return errors.New("private launcher detail") }
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			args := []string{"--account", "selected", "--agent", "synthetic"}
			if open {
				args = append(args, "--open")
			}
			var entry dashboard.RegistryEntry
			out, errout, code := captureFactDeleteCLI(t, func() int {
				done := make(chan int, 1)
				go func() { done <- dashboardServe(ctx, args) }()
				deadline := time.Now().Add(5 * time.Second)
				for time.Now().Before(deadline) {
					var err error
					entry, err = dashboard.ReadRegistryInstance(consoleTestIdentity().AgentID)
					if err == nil {
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
				if entry.AccessURL == "" {
					t.Error("standalone session did not become ready")
				} else {
					probe, closeProbe := consoleHTTPClient()
					probe.Transport.(*http.Transport).DisableKeepAlives = true
					response, err := probe.Get(entry.URL)
					if err != nil {
						t.Error("manual unauthenticated visit failed")
					} else {
						raw, _ := io.ReadAll(response.Body)
						_ = response.Body.Close()
						if response.StatusCode != 401 || !strings.Contains(string(raw), "dashboard open") || !strings.Contains(string(raw), "--print-url") {
							t.Error("authentication error omitted manual recovery command")
						}
					}
					closeProbe()
				}
				if open {
					select {
					case <-launched:
					case <-time.After(5 * time.Second):
						t.Error("browser launch was not attempted")
					}
				} else if len(launched) != 0 {
					t.Error("default startup launched a browser")
				}
				cancel()
				select {
				case result := <-done:
					return result
				case <-time.After(8 * time.Second):
					t.Fatal("standalone session did not stop")
					return 1
				}
			})
			if code != 0 || out != "" || !strings.Contains(errout, dashboardOpenHint) {
				t.Fatal("startup did not advertise verified manual opening")
			}
			if open && !strings.Contains(errout, "browser could not open") {
				t.Fatal("browser failure was not reported")
			}
			for _, secret := range []string{entry.AccessURL, accountFakeBearer, consoleFakeToken, "private launcher detail"} {
				if secret != "" && strings.Contains(out+errout, secret) {
					t.Fatal("startup recovery leaked private data")
				}
			}
		})
	}
}

func TestDashboardOpenVerifiesBeforeReveal(t *testing.T) {
	for _, mode := range []string{"reveal", "output-failure", "browser", "launcher-failure", "agent-only-caller", "revoked", "different-operator", "replaced", "missing"} {
		t.Run(mode, func(t *testing.T) {
			consoleTestHome(t)
			conn, manager, revoked := accountTestBackend(t)
			dashboardOpenTestSelection(t, conn)
			mux := http.NewServeMux()
			if err := dashboard.Register(mux, dashboard.Config{Endpoint: conn.Endpoint, BearerToken: conn.Token, AccessToken: strings.Repeat("ab", 16), Identity: consoleTestIdentity(), AccountManager: manager}); err != nil {
				t.Fatal(err)
			}
			var entry dashboard.RegistryEntry
			entry = consoleTestEntry(t, conn, func(w http.ResponseWriter, r *http.Request) {
				if mode == "replaced" && r.URL.Path == "/api/self" {
					replacement := entry
					replacement.StartedAt = entry.StartedAt.Add(time.Second)
					if err := dashboard.WriteRegistryEntry(replacement); err != nil {
						t.Error("replace registry")
					}
				}
				mux.ServeHTTP(w, r)
			})
			entry.Manager = &dashboard.ViewerBinding{AccountID: manager.Identity.AccountID, OperatorID: manager.Identity.OperatorID, Role: manager.Identity.Role}
			entry.ViewerContract = dashboard.ViewerSchema
			if mode == "different-operator" {
				entry.Manager.OperatorID = "op_different"
			}
			if mode != "missing" {
				if err := dashboard.WriteRegistryEntry(entry); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "revoked" {
				revoked.Store(true)
			}
			opened := 0
			launchBrowser = func(url string) error {
				opened++
				if url != entry.AccessURL {
					t.Error("wrong session opened")
				}
				if mode == "launcher-failure" {
					return errors.New("private launcher detail")
				}
				return nil
			}
			args := []string{"open", "--account", "selected", "--agent", "synthetic"}
			if mode != "browser" && mode != "launcher-failure" {
				args = append(args, "--print-url")
			}
			if mode == "agent-only-caller" {
				args = append(args, "--endpoint", conn.Endpoint)
			}
			out, errout, code := captureFactDeleteCLI(t, func() int {
				if mode == "output-failure" {
					if err := os.Stdout.Close(); err != nil {
						t.Fatal("close synthetic stdout pipe")
					}
				}
				return dashboardCmd(args)
			})
			if mode == "reveal" {
				if code != 0 || out != entry.AccessURL+"\n" || errout != "" || opened != 0 {
					t.Fatal("verified deliberate reveal failed")
				}
				// A fresh browser can exchange the revealed URL and read the app.
				read := accountTestLocalRead(t, entry)
				read("/")
				return
			}
			if mode == "browser" {
				if code != 0 || opened != 1 {
					t.Fatal("verified browser opening failed")
				}
			} else if code != 1 {
				t.Fatal("unverified or failed opening succeeded")
			}
			if mode == "output-failure" && errout != "witself: dashboard opening URL could not be written\n" {
				t.Fatal("output failure omitted fixed value-free diagnostic")
			}
			if mode == "launcher-failure" {
				if opened != 1 || !strings.Contains(errout, "--print-url") {
					t.Fatal("launcher failure omitted manual recovery")
				}
			} else if mode != "browser" && opened != 0 {
				t.Fatal("unverified context opened browser")
			}
			if out != "" || strings.Contains(errout, entry.AccessURL) || strings.Contains(errout, accountFakeBearer) || strings.Contains(errout, "private launcher detail") {
				t.Fatal("implicit or refused opening leaked private data")
			}
		})
	}
}
