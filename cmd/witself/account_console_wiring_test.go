package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/agenttui"
	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/clientinventory"
	"github.com/witwave-ai/witself/internal/dashboard"
	"github.com/witwave-ai/witself/internal/local"
)

func accountTestLocalRead(t *testing.T, entry dashboard.RegistryEntry) func(string) []byte {
	t.Helper()
	probe, closeProbe := consoleHTTPClient()
	probe.Transport.(*http.Transport).DisableKeepAlives = true
	t.Cleanup(closeProbe)
	response, err := probe.Get(entry.AccessURL)
	if err != nil {
		t.Fatal("local exchange failed")
	}
	_ = response.Body.Close()
	cookies := response.Cookies()
	if response.StatusCode != 303 || len(cookies) != 1 {
		t.Fatal("local exchange refused")
	}
	return func(path string) []byte {
		t.Helper()
		request, _ := http.NewRequest("GET", entry.URL+strings.TrimPrefix(path, "/"), nil)
		request.AddCookie(cookies[0])
		response, err := probe.Do(request)
		if err != nil {
			t.Fatal("local read failed")
		}
		defer func() { _ = response.Body.Close() }()
		raw, _ := io.ReadAll(io.LimitReader(response.Body, 32768))
		if response.StatusCode != 200 {
			t.Fatal("local read refused")
		}
		return raw
	}
}

func TestAccountConsoleAllScannerConstructors(t *testing.T) {
	for _, rootCase := range []string{"default", "dsh-tilde", "relative", "trailing-slash", "literal-witself-tilde", "dsh-bare-tilde", "dsh-backslash-tilde", "dsh-whitespace"} {
		for _, mode := range []string{"reader", "standalone", "child"} {
			t.Run(rootCase+"/"+mode, func(t *testing.T) {
				isolateAgentTUI(t)
				wantRoots := accountTestRootOverrides(t, rootCase)
				conn, manager, _ := accountTestBackend(t)
				if err := local.Save("selected", local.Account{ID: conn.AccountID}, accountFakeBearer); err != nil {
					t.Fatal(err)
				}
				roots := accountConsoleLocalRoots()
				if roots != wantRoots || !roots.valid() {
					t.Fatal("parent did not normalize provider roots")
				}
				oldConnect, oldLocate, oldScan, oldLaunch := accountConsoleConnect, accountConsoleLocate, accountConsoleScan, launchBrowser
				defer func() {
					accountConsoleConnect, accountConsoleLocate, accountConsoleScan, launchBrowser = oldConnect, oldLocate, oldScan, oldLaunch
				}()
				accountConsoleConnect = func(context.Context, string, string, string, string, string) (agentConnection, error) {
					return conn, nil
				}
				accountConsoleLocate = func(context.Context, string, string) (string, string, error) { return "", conn.Endpoint, nil }
				var scans atomic.Int32
				accountConsoleScan = func(ctx context.Context, o clientinventory.Options) (clientinventory.Report, error) {
					scans.Add(1)
					if o.Home != roots.Home || o.WitselfHome != roots.WitselfHome || o.DSHHome != roots.DSHHome || o.AccountID != conn.AccountID {
						t.Error("constructor lost private scanner roots")
					}
					return clientinventory.Report{SchemaVersion: clientinventory.SchemaVersion, DeviceLabel: "This device", CheckedAt: time.Now(), ScanStatus: clientinventory.ScanComplete, Entries: []clientinventory.Entry{}}, nil
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if mode == "reader" {
					createTUIConsole = func(_ context.Context, _ agentConnection, _ client.SelfIdentity, _ time.Duration, options accountConsoleOptions) agenttui.ConsoleController {
						if options.Manager == nil || *options.Manager != *manager || options.Roots != roots {
							t.Fatal("TUI lost managed child config")
						}
						return &testTUIConsoleCleanup{}
					}
					runAgentTUI = func(ctx context.Context, source agenttui.Source, _ agenttui.Options) error {
						if scans.Load() != 0 {
							t.Fatal("reader startup scan")
						}
						for range 2 {
							if _, err := source.Read(ctx, dashboard.ReadRequest{Resource: dashboard.ResourceAccountClients}); err != nil {
								t.Fatal(err)
							}
						}
						if scans.Load() != 0 {
							t.Fatal("reader passive scan")
						}
						raw, err := source.Read(ctx, dashboard.ReadRequest{Resource: dashboard.ResourceAccountClientsScan})
						if err != nil || !strings.Contains(string(raw), `"scan_status":"complete"`) || scans.Load() != 1 {
							t.Fatal("reader explicit scan missing")
						}
						return nil
					}
					if code := agentTUI(ctx, []string{"--account", "selected", "--agent", "synthetic"}); code != 0 {
						t.Fatal("TUI constructor failed")
					}
					return
				}
				done := make(chan int, 1)
				if mode == "standalone" {
					opened := make(chan string, 1)
					launchBrowser = func(url string) error { opened <- url; return nil }
					go func() {
						done <- dashboardServe(ctx, []string{"--account", "selected", "--agent", "synthetic", "--open"})
					}()
					select {
					case <-opened:
					case <-time.After(5 * time.Second):
						t.Fatal("standalone readiness timeout")
					}
				} else {
					// A managed child must keep the captured roots even with different
					// ambient homes and no ambient manager credential.
					for _, key := range []string{"HOME", "WITSELF_HOME", "DSH_HOME"} {
						t.Setenv(key, t.TempDir())
					}
					input, writer := io.Pipe()
					output, ready := io.Pipe()
					defer func() { _ = writer.Close(); _ = input.Close(); _ = output.Close(); _ = ready.Close() }()
					bootstrap := tuiConsoleBootstrap{Connection: conn, Identity: consoleTestIdentity(), Manager: consoleManagerBootstrap(manager), ScannerRoots: roots, AccessToken: strings.Repeat("cd", 16), Poll: time.Second}
					raw, _ := json.Marshal(bootstrap)
					go func() { done <- runTUIConsoleChild(input, ready) }()
					if _, err := writer.Write(append(raw, '\n')); err != nil {
						t.Fatal(err)
					}
					got := make(chan string, 1)
					go func() { var b [6]byte; _, _ = io.ReadFull(output, b[:]); got <- string(b[:]) }()
					select {
					case value := <-got:
						if value != "ready\n" {
							t.Fatal("bad child readiness")
						}
					case <-time.After(5 * time.Second):
						t.Fatal("child readiness timeout")
					}
					// The pipe remains open until the end; EOF must cancel this child.
					defer func() { _ = writer.Close() }()
					cancel = func() { _ = writer.Close() }
				}
				entry, err := dashboard.ReadRegistryInstance(consoleTestIdentity().AgentID)
				if err != nil || entry.Manager == nil || entry.ViewerContract != dashboard.ViewerSchema {
					t.Fatal("registry lost manager binding")
				}
				raw, err := os.ReadFile(mustAccountRegistryPath(t, entry.AgentID))
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(raw), accountFakeBearer) || strings.Contains(string(raw), consoleFakeToken) || strings.Contains(string(raw), roots.Home) {
					t.Fatal("private bootstrap leaked into registry")
				}
				read := accountTestLocalRead(t, entry)
				read("/api/account/context")
				read("/api/account/clients")
				read("/api/account/clients")
				if scans.Load() != 0 {
					t.Fatal("startup/passive scan")
				}
				raw = read("/api/account/clients/scan")
				if scans.Load() != 1 || !strings.Contains(string(raw), `"scan_status":"complete"`) {
					t.Fatal("explicit scan not wired")
				}
				cancel()
				select {
				case code := <-done:
					if code != 0 {
						t.Fatal("serve shutdown failed")
					}
				case <-time.After(8 * time.Second):
					t.Fatal("serve did not stop")
				}
			})
		}
	}
}
func mustAccountRegistryPath(t *testing.T, id string) string {
	t.Helper()
	path, err := dashboard.RegistryPath(id)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAccountConsoleManagerBootstrapBound(t *testing.T) {
	consoleTestHome(t)
	conn, manager, _ := accountTestBackend(t)
	manager.BearerToken = strings.Repeat("x", consoleBootstrapLimit)
	c := newTUIConsole(context.Background(), conn, consoleTestIdentity(), time.Second, accountConsoleOptions{Manager: manager, Roots: accountConsoleLocalRoots()})
	defer func() { _ = c.Close() }()
	if _, _, err := c.spawn(context.Background()); err != errConsoleStart {
		t.Fatal("oversized manager bootstrap accepted")
	}
	if c.child != nil {
		t.Fatal("oversized bootstrap started child")
	}
}

func TestAccountConsoleChildClaimCannotDowngrade(t *testing.T) {
	consoleTestHome(t)
	conn, manager, _ := accountTestBackend(t)
	manager.Identity.OperatorID = "op_substituted"
	c := newTUIConsole(context.Background(), conn, consoleTestIdentity(), time.Second, accountConsoleOptions{Manager: manager, Roots: accountConsoleLocalRoots()})
	c.command = consoleTestCommand("console-test-child")
	defer func() { _ = c.Close() }()
	if _, err := c.Start(context.Background()); err != errConsoleStart {
		t.Fatal("malformed manager claim did not fail startup")
	}
	if _, err := dashboard.ReadRegistryInstance(consoleTestIdentity().AgentID); !os.IsNotExist(err) {
		t.Fatal("invalid manager started agent-only console")
	}
}

func TestAccountConsoleExplicitModesNeverUseVaultFallback(t *testing.T) {
	for _, mode := range []string{"endpoint", "token-file"} {
		t.Run(mode, func(t *testing.T) {
			isolateAgentTUI(t)
			conn, _, _ := accountTestBackend(t)
			if err := local.Save("selected", local.Account{ID: conn.AccountID}, accountFakeBearer); err != nil {
				t.Fatal(err)
			}
			oldConnect, oldLocate := accountConsoleConnect, accountConsoleLocate
			defer func() { accountConsoleConnect, accountConsoleLocate = oldConnect, oldLocate }()
			args := []string{"--account", "selected", "--agent", "synthetic", "--endpoint", conn.Endpoint}
			if mode == "token-file" {
				args = append(args, "--token-file", "synthetic-only")
				conn.AccountID = ""
				conn.AccountName = ""
			}
			accountConsoleConnect = func(context.Context, string, string, string, string, string) (agentConnection, error) {
				return conn, nil
			}
			accountConsoleLocate = func(context.Context, string, string) (string, string, error) {
				t.Fatal("explicit connection tried manager directory lookup")
				return "", "", nil
			}
			createTUIConsole = func(_ context.Context, c agentConnection, _ client.SelfIdentity, _ time.Duration, options accountConsoleOptions) agenttui.ConsoleController {
				if options.Manager != nil || c.AccountName != "selected" {
					t.Fatal("vault fallback changed manager eligibility or lost vault selector")
				}
				return &testTUIConsoleCleanup{}
			}
			ran := false
			runAgentTUI = func(ctx context.Context, source agenttui.Source, _ agenttui.Options) error {
				ran = true
				raw, err := source.Read(ctx, dashboard.ReadRequest{Resource: dashboard.ResourceAccountContext})
				if err != nil || !strings.Contains(string(raw), `"available":false`) {
					t.Fatal("explicit connection acquired account authority")
				}
				if _, err := source.Read(ctx, dashboard.ReadRequest{Resource: dashboard.ResourceSelf}); err != nil {
					t.Fatal("agent console stopped working")
				}
				return nil
			}
			if agentTUI(context.Background(), args) != 0 || !ran {
				t.Fatal("agent console failed")
			}
		})
	}
}

func TestAccountConsoleLateCapturePreservesChangedManager(t *testing.T) {
	consoleTestHome(t)
	conn, manager, _ := accountTestBackend(t)
	c := newTUIConsole(context.Background(), conn, consoleTestIdentity(), time.Second, accountConsoleOptions{Manager: manager, Roots: accountConsoleLocalRoots()})
	defer func() { _ = c.Close() }()
	// A retained process handle and token are insufficient when the manager
	// binding in its not-yet-captured record has been replaced.
	command, err := consoleTestCommand("console-test-wait")()
	if err != nil {
		t.Fatal(err)
	}
	command.Process = &os.Process{Pid: os.Getppid()}
	child := &tuiConsoleProcess{cmd: command, accessToken: strings.Repeat("ab", 16)}
	entry := consoleTestEntry(t, conn, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("cleanup capture probed HTTP")
		w.WriteHeader(500)
	})
	entry.AccountID = consoleTestIdentity().AccountID
	entry.RealmID = consoleTestIdentity().RealmID
	entry.ViewerContract = dashboard.ViewerSchema
	entry.Manager = &dashboard.ViewerBinding{AccountID: manager.Identity.AccountID, OperatorID: "op_replacement", Role: manager.Identity.Role}
	consoleTestWriteEntry(t, entry)
	c.captureChildEntry(child)
	if child.entry.AgentID != "" {
		t.Fatal("captured replacement manager fence")
	}
	current, err := dashboard.ReadRegistryInstance(entry.AgentID)
	if err != nil || !dashboard.SameRegistryInstance(current, entry) {
		t.Fatal("replacement record changed")
	}
	entry.Manager.OperatorID = manager.Identity.OperatorID
	consoleTestWriteEntry(t, entry)
	c.captureChildEntry(child)
	if !dashboard.SameRegistryInstance(child.entry, entry) {
		t.Fatal("original private manager binding was not captured")
	}
}

// Expected values are independent of the production capture helper. All relative
// paths resolve within this synthetic cwd; no provider or credentials are read.
func accountTestRootOverrides(t *testing.T, name string) accountConsoleRoots {
	t.Helper()
	t.Chdir(t.TempDir())
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal("synthetic cwd unavailable")
	}
	home := filepath.Join(cwd, "home")
	witself := filepath.Join(cwd, "state")
	dsh := filepath.Join(cwd, "dsh")
	for key, value := range map[string]string{"HOME": home, "USERPROFILE": home, "WITSELF_HOME": witself, "DSH_HOME": dsh} {
		t.Setenv(key, value)
	}
	switch name {
	case "default":
		t.Setenv("WITSELF_HOME", "")
		t.Setenv("DSH_HOME", "")
		witself, dsh = filepath.Join(home, ".witself"), filepath.Join(home, ".dsh")
	case "dsh-tilde":
		t.Setenv("DSH_HOME", "~/.dsh")
		dsh = filepath.Join(home, ".dsh")
	case "relative":
		t.Setenv("HOME", "home/./")
		t.Setenv("USERPROFILE", "home/./")
		t.Setenv("WITSELF_HOME", "state/../state/")
		t.Setenv("DSH_HOME", "dsh/../dsh/")
	case "trailing-slash":
		t.Setenv("HOME", home+"/")
		t.Setenv("USERPROFILE", home+"/")
		t.Setenv("WITSELF_HOME", witself+"/")
		t.Setenv("DSH_HOME", dsh+"/")
	case "literal-witself-tilde":
		t.Setenv("WITSELF_HOME", "~/state")
		witself = filepath.Join(cwd, "~", "state")
	case "dsh-bare-tilde":
		t.Setenv("DSH_HOME", "~")
		dsh = home
	case "dsh-backslash-tilde":
		t.Setenv("DSH_HOME", "~\\.dsh")
		dsh = filepath.Join(home, ".dsh")
	case "dsh-whitespace":
		t.Setenv("DSH_HOME", "  ~/.dsh  ")
		dsh = filepath.Join(home, ".dsh")
	default:
		t.Fatal("unknown root fixture")
	}
	return accountConsoleRoots{Home: home, WitselfHome: witself, DSHHome: dsh}
}
