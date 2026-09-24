//go:build darwin || linux

package clientinventory

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pelletier/go-toml/v2"
	"golang.org/x/sys/unix"
)

type fixture struct {
	opts  Options
	paths []string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	home := t.TempDir()
	f := &fixture{opts: Options{Home: home, WitselfHome: filepath.Join(home, ".witself"), DSHHome: filepath.Join(home, ".dsh"), AccountID: "acct_fixture"}}
	if err := os.MkdirAll(f.opts.WitselfHome, 0700); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) write(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
	f.paths = append(f.paths, path)
}

func (f *fixture) recordPath(runtime Runtime) string {
	return filepath.Join(f.opts.WitselfHome, "integrations", string(runtime), "config.json")
}
func (f *fixture) save(t *testing.T, cfg record) {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	f.write(t, f.recordPath(cfg.Runtime), raw, 0600)
}

func (f *fixture) install(t *testing.T, runtime Runtime) record {
	t.Helper()
	names := map[Runtime]string{RuntimeCodex: "codex", RuntimeClaudeCode: "claude", RuntimeGrokBuild: "grok", RuntimeCursor: "cursor-agent", RuntimeOpenClaw: "openclaw", RuntimeAntigravity: "agy", RuntimeCopilot: "copilot", RuntimeDSH: "dsh"}
	cfg := record{SchemaVersion: "witself.capture.v1", Runtime: runtime, AccountID: f.opts.AccountID, RuntimeVersion: "v1.2.3-rc.1", RuntimeCLICommand: filepath.Join(f.opts.Home, ".local", "bin", names[runtime]), MCPCommand: filepath.Join(f.opts.Home, "bin", "witself"), MCPEnvironment: map[string]string{"WITSELF_HOME": f.opts.WitselfHome}, Account: "private-selector", Realm: "private-realm", Agent: "private-agent", InstalledAt: "2026-01-02T03:04:05Z", TokenFile: filepath.Join(f.opts.WitselfHome, "credentials", "token")}
	f.write(t, cfg.RuntimeCLICommand, []byte("#!/bin/sh\necho forbidden > '"+filepath.Join(f.opts.Home, "executed")+"'\nexit 91\n"), 0700)
	switch runtime {
	case RuntimeCodex:
		cfg.RuntimeConfigRoot = filepath.Join(f.opts.Home, ".codex")
		cfg.RuntimeMCPConfigPath = filepath.Join(cfg.RuntimeConfigRoot, "config.toml")
	case RuntimeClaudeCode:
		cfg.RuntimeConfigRoot = filepath.Join(f.opts.Home, ".claude")
		cfg.RuntimeMCPConfigPath = filepath.Join(f.opts.Home, ".claude.json")
	case RuntimeGrokBuild:
		cfg.RuntimeConfigRoot = filepath.Join(f.opts.Home, ".grok")
		cfg.RuntimeMCPConfigPath = filepath.Join(cfg.RuntimeConfigRoot, "config.toml")
	case RuntimeCursor:
		cfg.RuntimeConfigRoot = filepath.Join(f.opts.Home, ".cursor")
		cfg.RuntimeMCPConfigPath = filepath.Join(cfg.RuntimeConfigRoot, "mcp.json")
	case RuntimeDSH:
		cfg.RuntimeConfigRoot = f.opts.DSHHome
		cfg.RuntimeMCPConfigPath = filepath.Join(cfg.RuntimeConfigRoot, "cordis.patch.yml")
	case RuntimeCopilot:
		cfg.RuntimeConfigRoot = filepath.Join(f.opts.Home, ".copilot")
		cfg.RuntimeMCPConfigPath = filepath.Join(cfg.RuntimeConfigRoot, "mcp-config.json")
	case RuntimeAntigravity:
		cfg.RuntimeConfigRoot = filepath.Join(f.opts.Home, ".gemini", "antigravity")
		cfg.RuntimeMCPConfigPath = filepath.Join(cfg.RuntimeConfigRoot, "mcp_config.json")
	}
	if runtime == RuntimeCodex || runtime == RuntimeClaudeCode || runtime == RuntimeGrokBuild || runtime == RuntimeCursor {
		f.provider(t, cfg, nil)
	}
	f.save(t, cfg)
	return cfg
}

func (f *fixture) provider(t *testing.T, cfg record, mutate func(map[string]any)) {
	t.Helper()
	binding := map[string]any{"command": cfg.MCPCommand, "args": []string{"mcp", "serve", "--runtime", string(cfg.Runtime), "--account", cfg.Account, "--realm", cfg.Realm, "--agent", cfg.Agent}, "env": cfg.MCPEnvironment}
	if cfg.Runtime == RuntimeGrokBuild {
		binding["enabled"] = true
	}
	if cfg.Runtime == RuntimeClaudeCode {
		binding["type"] = "stdio"
	}
	if mutate != nil {
		mutate(binding)
	}
	container := "mcpServers"
	if cfg.Runtime == RuntimeCodex || cfg.Runtime == RuntimeGrokBuild {
		container = "mcp_servers"
	}
	doc := map[string]any{container: map[string]any{"witself": binding}}
	var raw []byte
	var err error
	if container == "mcp_servers" {
		raw, err = toml.Marshal(doc)
	} else {
		raw, err = json.Marshal(doc)
	}
	if err != nil {
		t.Fatal(err)
	}
	f.write(t, cfg.RuntimeMCPConfigPath, raw, 0600)
}

func scan(t *testing.T, f *fixture) Report {
	t.Helper()
	r, err := Scan(context.Background(), f.opts)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestAllRuntimesAndNoWrites(t *testing.T) {
	f := newFixture(t)
	for i := len(runtimes) - 1; i >= 0; i-- {
		f.install(t, runtimes[i])
	}
	// A real credential-shaped fixture is never opened by Scan.
	tokenPath := filepath.Join(f.opts.WitselfHome, "credentials", "token")
	f.write(t, tokenPath, []byte("DO-NOT-READ-PRIVATE-TOKEN"), 0600)
	old := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(tokenPath, old, old); err != nil {
		t.Fatal(err)
	}
	type state struct {
		Hash     [32]byte
		Mode     os.FileMode
		Modified time.Time
	}
	before := map[string]state{}
	for _, p := range f.paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		before[p] = state{sha256.Sum256(raw), st.Mode(), st.ModTime()}
	}
	if err := os.Chtimes(tokenPath, old, old); err != nil {
		t.Fatal(err)
	}
	var tokenBefore unix.Stat_t
	if err := unix.Stat(tokenPath, &tokenBefore); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	r := scan(t, f)
	if r.SchemaVersion != SchemaVersion || r.DeviceLabel != "This device" || r.ScanStatus != ScanComplete || r.CheckedAt.Before(start) || r.CheckedAt.After(time.Now()) {
		t.Fatalf("bad report: %+v", r)
	}
	if len(r.Entries) != 8 {
		t.Fatalf("entries=%d", len(r.Entries))
	}
	for i, e := range r.Entries {
		if e.Runtime != runtimes[i] || e.RecordedVersion != "v1.2.3-rc.1" || e.ExecutableStatus != ExecutablePresent || e.EffectiveVerification != EffectiveNotRun || e.InstalledAt == nil {
			t.Fatalf("bad entry: %+v", e)
		}
		if i < 4 && (e.ConfigurationStatus != ConfigurationMatch || e.ConfigurationScope != ScopeMCPRegistration) {
			t.Fatalf("bad match: %+v", e)
		}
		if i >= 4 && (e.ConfigurationStatus != ConfigurationUnsupported || e.ConfigurationScope != ScopeNone) {
			t.Fatalf("bad unsupported: %+v", e)
		}
	}
	var tokenAfter unix.Stat_t
	if err := unix.Stat(tokenPath, &tokenAfter); err != nil {
		t.Fatal(err)
	}
	if tokenBefore.Atim != tokenAfter.Atim {
		t.Fatal("credential accessed")
	}
	for p, want := range before {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if got := (state{sha256.Sum256(raw), st.Mode(), st.ModTime()}); got != want {
			t.Fatal("fixture modified")
		}
	}
	if _, err := os.Stat(filepath.Join(f.opts.Home, "executed")); !os.IsNotExist(err) {
		t.Fatal("provider ran")
	}
	raw, _ := json.Marshal(r)
	for _, private := range []string{f.opts.Home, "private-selector", "private-realm", "private-agent", f.opts.AccountID, "DO-NOT-READ", "token_file", "runtime_cli_command"} {
		if strings.Contains(string(raw), private) {
			t.Fatal("private value in projection")
		}
	}
}

func TestAccountFenceAndOptions(t *testing.T) {
	for _, account := range []string{"", " ", " acct_fixture", "acct_fixture\n", "../account"} {
		t.Run("invalid_"+account, func(t *testing.T) {
			_, err := Scan(context.Background(), Options{AccountID: account})
			if !errors.Is(err, ErrAccountRequired) {
				t.Fatal(err)
			}
		})
	}
	f := newFixture(t)
	cfg := f.install(t, RuntimeCursor)
	provider := cfg.RuntimeMCPConfigPath
	old := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(provider, old, old); err != nil {
		t.Fatal(err)
	}
	var before unix.Stat_t
	if err := unix.Stat(provider, &before); err != nil {
		t.Fatal(err)
	}
	for _, account := range []string{"", "acct_other", " acct_fixture"} {
		cfg.AccountID = account
		f.save(t, cfg)
		r := scan(t, f)
		if len(r.Entries) != 0 || r.ScanStatus != ScanComplete {
			t.Fatalf("foreign row: %+v", r)
		}
	}
	var after unix.Stat_t
	if err := unix.Stat(provider, &after); err != nil {
		t.Fatal(err)
	}
	if before.Atim != after.Atim {
		t.Fatal("foreign provider file read")
	}
	// Wrongly typed path references must also be filtered before full decoding.
	f.write(t, f.recordPath(RuntimeCursor), []byte(`{"account_id":"acct_other","runtime_config_root":{"secret":true}}`), 0600)
	if r := scan(t, f); len(r.Entries) != 0 || r.ScanStatus != ScanComplete {
		t.Fatal("decoded foreign path references")
	}
	for _, home := range []string{"", "relative", "/tmp/../private", "/tmp\nsecret"} {
		opts := f.opts
		opts.Home = home
		_, err := Scan(context.Background(), opts)
		if !errors.Is(err, ErrInvalidOptions) {
			t.Fatal(err)
		}
	}
}

type cancellingContext struct {
	context.Context
	remaining int
}

func (c *cancellingContext) Err() error {
	c.remaining--
	if c.remaining <= 0 {
		return context.Canceled
	}
	return nil
}

func TestCancellationDuringScan(t *testing.T) {
	f := newFixture(t)
	for _, r := range runtimes {
		f.install(t, r)
	}
	ctx := &cancellingContext{Context: context.Background(), remaining: 35}
	r, err := Scan(ctx, f.opts)
	if !errors.Is(err, context.Canceled) || len(r.Entries) != 0 || r.SchemaVersion != "" {
		t.Fatal("partial results escaped cancellation")
	}
}

func TestSubstitutedRecordNeverFollowsLink(t *testing.T) {
	f := newFixture(t)
	p := f.recordPath(RuntimeCodex)
	f.write(t, p, []byte("safe"), 0600)
	secret := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(secret, []byte("PRIVATE"), 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(secret, old, old); err != nil {
		t.Fatal(err)
	}
	var before unix.Stat_t
	if err := unix.Stat(secret, &before); err != nil {
		t.Fatal(err)
	}
	root, err := openDirectory(f.opts.WitselfHome)
	if err != nil {
		t.Fatal(err)
	}
	defer root.close()
	done := make(chan struct{})
	failed := make(chan error, 1)
	go func() {
		defer close(done)
		for range 300 {
			if err := os.Symlink(secret, p+".link"); err != nil {
				failed <- err
				return
			}
			if err := os.Rename(p+".link", p); err != nil {
				failed <- err
				return
			}
			if err := os.WriteFile(p+".new", []byte("safe"), 0600); err != nil {
				failed <- err
				return
			}
			if err := os.Rename(p+".new", p); err != nil {
				failed <- err
				return
			}
		}
	}()
	for range 300 {
		raw, err := root.read(context.Background(), "integrations/codex/config.json", recordLimit)
		if err == nil && string(raw) != "safe" {
			t.Error("substituted content read")
		}
	}
	<-done
	select {
	case err := <-failed:
		t.Fatal(err)
	default:
	}
	var after unix.Stat_t
	if err := unix.Stat(secret, &after); err != nil {
		t.Fatal(err)
	}
	if before.Atim != after.Atim {
		t.Fatal("substitution read credential")
	}
}

func TestScanCursorRecordedVersions(t *testing.T) {
	// This exact recorded build is preserved by TestDetectRuntimeVersion and
	// TestDetectRuntimeVersionUsesCursorAgentBuild in cmd/witself/integration_test.go.
	const existingCursorBuild = "2026.07.16-899851b"
	for _, runtime := range runtimes {
		t.Run(string(runtime), func(t *testing.T) {
			f := newFixture(t)
			cfg := f.install(t, runtime)
			for _, version := range []string{
				existingCursorBuild, "2024.02.29-abcdef0", "2000.02.29-0123456789abcdef",
				"2026.12.31-0000000",
			} {
				cfg.RuntimeVersion = version
				f.save(t, cfg)
				want := ""
				if runtime == RuntimeCursor {
					want = version
				}
				r := scan(t, f)
				if len(r.Entries) != 1 || r.Entries[0].RecordedVersion != want {
					t.Errorf("recorded version %q: got %+v, want %q", version, r.Entries, want)
				}
			}
		})
	}
	t.Run("invalid_cursor_builds", func(t *testing.T) {
		f := newFixture(t)
		cfg := f.install(t, RuntimeCursor)
		for _, version := range []string{
			"2026.02.29-899851b", "2100.02.29-899851b", "2026.04.31-899851b",
			"2026.00.16-899851b", "2026.13.16-899851b", "2026.07.00-899851b",
			"2026.07.32-899851b", "0000.07.16-899851b",
			"2026.7.16-899851b", "2026.07.6-899851b", "26.07.16-899851b",
			"2026.07.16-", "2026.07.16-abcdef", "2026.07.16-0123456789abcdef0",
			"2026.07.16-" + strings.Repeat("a", 48),
			"2026.07.16-899851B", "2026.07.16-899851g", "2026.07.16-899851b.secret",
			"2026.07.16-899851b+private", "v" + existingCursorBuild,
			" " + existingCursorBuild, existingCursorBuild + "\n",
			existingCursorBuild + "\n[0716/234658.202288:ERROR:electron] failure",
			"[0716/234658.202288:ERROR:electron] failure", "secret.example.com",
			"1.2.3+host.example.com", "1.2.3-SECRET", "1.2.3\nsecret",
			"v1.2.3 $(touch x)", "01.2.3", "<script>",
		} {
			cfg.RuntimeVersion = version
			f.save(t, cfg)
			if got := scan(t, f).Entries[0].RecordedVersion; got != "" {
				t.Errorf("unsafe version projected: %q", got)
			}
		}
	})
	t.Run("cursor_semver_preserved", func(t *testing.T) {
		f := newFixture(t)
		cfg := f.install(t, RuntimeCursor)
		for _, version := range []string{"0.0.299", "1.2.3", "v1.2.3", "1.2.3-beta.2", "1.2.3-alpha", "v1.2.3-rc.1"} {
			cfg.RuntimeVersion = version
			f.save(t, cfg)
			if got := scan(t, f).Entries[0].RecordedVersion; got != version {
				t.Errorf("semver lost: got %q, want %q", got, version)
			}
		}
	})
}

func TestVersionsAndTimesAreStrict(t *testing.T) {
	f := newFixture(t)
	cfg := f.install(t, RuntimeCodex)
	for _, version := range []string{"secret.example.com", "1.2.3+host.example.com", "1.2.3-SECRET", "1.2.3\nsecret", "v1.2.3 $(touch x)", "01.2.3", strings.Repeat("1", 1000), "<script>"} {
		cfg.RuntimeVersion = version
		cfg.InstalledAt = "9999-01-01T00:00:00Z"
		f.save(t, cfg)
		e := scan(t, f).Entries[0]
		if e.RecordedVersion != "" || e.InstalledAt != nil {
			t.Fatal("untrusted metadata surfaced")
		}
	}
	for _, version := range []string{"0.0.299", "1.2.3", "v1.2.3", "1.2.3-beta.2", "1.2.3-alpha"} {
		cfg.RuntimeVersion = version
		f.save(t, cfg)
		if scan(t, f).Entries[0].RecordedVersion != version {
			t.Fatal("valid version lost")
		}
	}
}

func TestConfigurationStatuses(t *testing.T) {
	for _, runtime := range runtimes[:4] {
		t.Run(string(runtime), func(t *testing.T) {
			f := newFixture(t)
			cfg := f.install(t, runtime)
			f.provider(t, cfg, func(binding map[string]any) { binding["command"] = "private-error-command" })
			if scan(t, f).Entries[0].ConfigurationStatus != ConfigurationChanged {
				t.Fatal("changed binding missed")
			}
			f.provider(t, cfg, func(binding map[string]any) { binding["enabled"] = false })
			if scan(t, f).Entries[0].ConfigurationStatus != ConfigurationChanged {
				t.Fatal("disabled binding missed")
			}
			if err := os.Remove(cfg.RuntimeMCPConfigPath); err != nil {
				t.Fatal(err)
			}
			if scan(t, f).Entries[0].ConfigurationStatus != ConfigurationIncomplete {
				t.Fatal("missing config missed")
			}
			f.write(t, cfg.RuntimeMCPConfigPath, []byte("PRIVATE-RAW-ERROR"), 0600)
			r := scan(t, f)
			if r.Entries[0].ConfigurationStatus != ConfigurationUnavailable {
				t.Fatal("malformed config missed")
			}
			raw, _ := json.Marshal(r)
			if strings.Contains(string(raw), "PRIVATE") {
				t.Fatal("raw error leaked")
			}
			cfg.RuntimeConfigRoot = filepath.Join(f.opts.Home, "secret-root")
			f.save(t, cfg)
			if scan(t, f).Entries[0].ConfigurationStatus != ConfigurationUnsupported {
				t.Fatal("arbitrary root accepted")
			}
		})
	}
}

func TestNoAmbientSelectionAndDefaults(t *testing.T) {
	f := newFixture(t)
	f.install(t, RuntimeCodex)
	for _, key := range []string{"HOME", "WITSELF_HOME", "DSH_HOME", "CODEX_HOME", "CLAUDE_CONFIG_DIR", "GROK_HOME", "CURSOR_CONFIG_DIR", "PATH"} {
		t.Setenv(key, filepath.Join(f.opts.Home, "poison"))
	}
	f.opts.WitselfHome = ""
	f.opts.DSHHome = ""
	r := scan(t, f)
	if len(r.Entries) != 1 || r.Entries[0].ConfigurationStatus != ConfigurationMatch {
		t.Fatalf("ambient selector used: %+v", r)
	}
}

func TestMalformedBoundedRecords(t *testing.T) {
	cases := map[string][]byte{
		"oversize":          []byte(strings.Repeat(" ", recordLimit+1)),
		"malformed":         []byte("private-error-path"),
		"duplicates":        []byte(`{"account_id":"acct_other","account_id":"acct_fixture"}`),
		"folded_duplicates": []byte(`{"account_id":"acct_other","ACCOUNT_ID":"acct_fixture"}`),
		"deep":              []byte(strings.Repeat("[", 30) + "0" + strings.Repeat("]", 30)),
		"count":             []byte("[" + strings.Repeat("0,", 8193) + "0]"),
		"trailing":          []byte(`{} {}`),
		"wrong_schema":      []byte(`{"account_id":"acct_fixture","runtime":"codex","schema_version":"private"}`),
		"wrong_runtime":     []byte(`{"account_id":"acct_fixture","runtime":"private-runtime","schema_version":"witself.capture.v1"}`),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.write(t, f.recordPath(RuntimeCodex), raw, 0600)
			r := scan(t, f)
			if len(r.Entries) != 0 || r.ScanStatus != ScanPartial {
				t.Fatalf("bad record accepted: %+v", r)
			}
		})
	}
	for _, kind := range []string{"directory", "fifo", "unreadable", "symlink", "hardlink", "ancestor_symlink"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			cfg := f.install(t, RuntimeCodex)
			p := f.recordPath(RuntimeCodex)
			raw, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "directory":
				err = os.Mkdir(p, 0700)
			case "fifo":
				err = unix.Mkfifo(p, 0600)
			case "unreadable":
				err = os.WriteFile(p, raw, 0000)
			case "symlink":
				err = os.Symlink(cfg.TokenFile, p)
			case "hardlink":
				target := filepath.Join(f.opts.Home, "secret")
				f.write(t, target, raw, 0600)
				err = os.Link(target, p)
			case "ancestor_symlink":
				dir := filepath.Dir(p)
				if err = os.Remove(dir); err != nil {
					t.Fatal(err)
				}
				target := filepath.Join(f.opts.Home, "substitute")
				f.write(t, filepath.Join(target, "config.json"), raw, 0600)
				err = os.Symlink(target, dir)
			}
			if err != nil {
				t.Fatal(err)
			}
			r := scan(t, f)
			if len(r.Entries) != 0 || r.ScanStatus != ScanPartial {
				t.Fatalf("unsafe record accepted: %+v", r)
			}
		})
	}
}

func TestHostileReferencesNeverRead(t *testing.T) {
	f := newFixture(t)
	cfg := f.install(t, RuntimeCursor)
	secret := filepath.Join(f.opts.Home, "secret")
	f.write(t, secret, []byte("PRIVATE-SECRET"), 0600)
	for _, target := range []string{secret, filepath.Join(f.opts.Home, "missing-secret"), filepath.Join(f.opts.Home, ".cursor") + "/../secret", cfg.TokenFile} {
		cfg.RuntimeCLICommand = target
		cfg.RuntimeMCPConfigPath = target
		f.save(t, cfg)
		e := scan(t, f).Entries[0]
		if e.ExecutableStatus != ExecutableUnchecked || e.ConfigurationStatus != ConfigurationUnsupported {
			t.Fatalf("path oracle: %+v", e)
		}
	}
	// Explicit credential references to otherwise allowed paths still deny access.
	cfg = f.install(t, RuntimeCursor)
	cfg.TokenFile = cfg.RuntimeMCPConfigPath
	f.save(t, cfg)
	if scan(t, f).Entries[0].ConfigurationStatus != ConfigurationUnsupported {
		t.Fatal("TokenFile read")
	}
	cfg.TokenFile = cfg.RuntimeCLICommand
	f.save(t, cfg)
	if scan(t, f).Entries[0].ExecutableStatus != ExecutableUnchecked {
		t.Fatal("TokenFile probed")
	}
}

func TestTokenFileCrossRecordExclusions(t *testing.T) {
	for _, pair := range [][2]Runtime{{RuntimeCodex, RuntimeCursor}, {RuntimeCursor, RuntimeCodex}, {RuntimeDSH, RuntimeCursor}} {
		for _, target := range []string{"configuration", "executable"} {
			for _, alias := range []bool{false, true} {
				name := string(pair[0]) + "_denies_" + string(pair[1]) + "/" + target
				if alias {
					name += "/normalized_alias"
				}
				t.Run(name, func(t *testing.T) {
					f := newFixture(t)
					f.opts.Home = filepath.Join(f.opts.Home, "caf\u00e9")
					owner := f.install(t, pair[0])
					cfg := f.install(t, pair[1])
					cfg.TokenFile = ""
					f.save(t, cfg)
					path := cfg.RuntimeMCPConfigPath
					if target == "executable" {
						path = cfg.RuntimeCLICommand
					}
					owner.TokenFile = path
					if alias {
						owner.TokenFile = strings.ReplaceAll(filepath.Dir(path), "caf\u00e9", "cafe\u0301") + "/./" + strings.ToUpper(filepath.Base(path))
					}
					f.save(t, owner)
					f.write(t, path, []byte("CROSS-RECORD-TOKEN-CANARY"), 0600)
					old := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
					if err := os.Chtimes(path, old, old); err != nil {
						t.Fatal(err)
					}
					var before, after unix.Stat_t
					if err := unix.Stat(path, &before); err != nil {
						t.Fatal(err)
					}
					r := scan(t, f)
					if r.ScanStatus != ScanComplete || len(r.Entries) != 2 {
						t.Fatalf("unexpected report: %+v", r)
					}
					for _, e := range r.Entries {
						if e.Runtime != cfg.Runtime {
							continue
						}
						if target == "configuration" {
							if e.ConfigurationStatus != ConfigurationUnsupported || e.ConfigurationScope != ScopeNone || e.ExecutableStatus != ExecutablePresent {
								t.Errorf("cross-record credential not excluded: %+v", e)
							}
						} else if e.ExecutableStatus != ExecutableUnchecked || e.ConfigurationStatus != ConfigurationMatch {
							t.Errorf("cross-record credential probed: %+v", e)
						}
					}
					if err := unix.Stat(path, &after); err != nil {
						t.Fatal(err)
					}
					if before.Atim != after.Atim {
						t.Error("cross-record credential was read")
					}
					// Check exclusion before descriptor traversal independently of
					// whether this filesystem updates access times.
					ctx := &cancellingContext{Context: context.Background(), remaining: 100}
					var excluded tokenFileExclusions
					excluded.add(owner.TokenFile)
					if target == "configuration" {
						configurationStatus(ctx, f.opts, cfg, excluded)
					} else {
						executableStatus(ctx, f.opts, cfg, excluded)
					}
					if ctx.remaining != 100 {
						t.Error("cross-record credential reached filesystem traversal")
					}
				})
			}
		}
	}
}

func TestTokenFileExclusionsOnlyFromEligibleRecords(t *testing.T) {
	for _, eligibility := range []string{"eligible", "foreign", "missing_account", "wrong_schema", "wrong_runtime"} {
		t.Run(eligibility, func(t *testing.T) {
			f := newFixture(t)
			cfg := f.install(t, RuntimeCursor)
			for i, runtime := range []Runtime{RuntimeCodex, RuntimeDSH} {
				owner := f.install(t, runtime)
				owner.TokenFile = cfg.RuntimeMCPConfigPath
				if i == 1 {
					owner.TokenFile = cfg.RuntimeCLICommand
				}
				switch eligibility {
				case "foreign":
					owner.AccountID = "acct_other"
				case "missing_account":
					owner.AccountID = ""
				case "wrong_schema":
					owner.SchemaVersion = "unsupported"
				case "wrong_runtime":
					owner.Runtime = RuntimeCursor
				}
				raw, err := json.Marshal(owner)
				if err != nil {
					t.Fatal(err)
				}
				f.write(t, f.recordPath(runtime), raw, 0600)
			}
			r := scan(t, f)
			wantConfiguration, wantExecutable := ConfigurationMatch, ExecutablePresent
			wantRows, wantScan := 1, ScanComplete
			switch eligibility {
			case "eligible":
				wantConfiguration, wantExecutable = ConfigurationUnsupported, ExecutableUnchecked
				wantRows = 3
			case "wrong_schema", "wrong_runtime":
				wantScan = ScanPartial
			}
			if len(r.Entries) != wantRows || r.ScanStatus != wantScan {
				t.Fatalf("unexpected eligibility: %+v", r)
			}
			for _, e := range r.Entries {
				if e.Runtime == cfg.Runtime && (e.ConfigurationStatus != wantConfiguration || e.ExecutableStatus != wantExecutable) {
					t.Fatalf("incorrect exclusion eligibility: %+v", e)
				}
			}
		})
	}
}

func TestTokenFileCaseAliasesExcludedBeforeIO(t *testing.T) {
	for _, target := range []string{"configuration", "executable"} {
		for _, alias := range []string{"basename", "parent", "absolute", "cleaned"} {
			t.Run(target+"/"+alias, func(t *testing.T) {
				f := newFixture(t)
				cfg := f.install(t, RuntimeCursor)
				path := cfg.RuntimeMCPConfigPath
				if target == "executable" {
					path = cfg.RuntimeCLICommand
				}
				switch alias {
				case "basename":
					cfg.TokenFile = filepath.Join(filepath.Dir(path), strings.ToUpper(filepath.Base(path)))
				case "parent":
					parent := filepath.Dir(path)
					cfg.TokenFile = filepath.Join(filepath.Dir(parent), strings.ToUpper(filepath.Base(parent)), filepath.Base(path))
				case "absolute":
					cfg.TokenFile = strings.ToUpper(path)
				case "cleaned":
					cfg.TokenFile = filepath.Dir(path) + "/./" + strings.ToUpper(filepath.Base(path))
				}
				f.save(t, cfg)
				original, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if aliased, err := os.Stat(cfg.TokenFile); err == nil {
					if !os.SameFile(original, aliased) {
						t.Fatal("fixture case alias does not identify the same file")
					}
					t.Log("verified filesystem alias")
				} else if !os.IsNotExist(err) {
					t.Fatal(err)
				}
				old := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
				if err := os.Chtimes(path, old, old); err != nil {
					t.Fatal(err)
				}
				var before, after unix.Stat_t
				if err := unix.Stat(path, &before); err != nil {
					t.Fatal(err)
				}
				e := scan(t, f).Entries[0]
				if target == "configuration" {
					if e.ConfigurationStatus != ConfigurationUnsupported || e.ConfigurationScope != ScopeNone {
						t.Errorf("excluded configuration accepted: %+v", e)
					}
				} else if e.ExecutableStatus != ExecutableUnchecked {
					t.Errorf("excluded executable probed: %+v", e)
				}
				if err := unix.Stat(path, &after); err != nil {
					t.Fatal(err)
				}
				if before.Atim != after.Atim {
					t.Error("excluded file was read")
				}
				// Both descriptor-relative helpers check context before visiting
				// descendants. This also proves early exclusion on filesystems
				// that do not update atime, and for metadata-only executable probes.
				ctx := &cancellingContext{Context: context.Background(), remaining: 100}
				var excluded tokenFileExclusions
				excluded.add(cfg.TokenFile)
				if target == "configuration" {
					configurationStatus(ctx, f.opts, cfg, excluded)
				} else {
					executableStatus(ctx, f.opts, cfg, excluded)
				}
				if ctx.remaining != 100 {
					t.Error("excluded path reached filesystem traversal")
				}
			})
		}
	}
}

func TestTokenFileUnicodeAliasesExcludedBeforeIO(t *testing.T) {
	for _, homeForm := range []string{"NFC", "NFD"} {
		for _, target := range []string{"configuration", "executable"} {
			for _, spelling := range []string{"canonical", "cleaned_case"} {
				t.Run(homeForm+"/"+target+"/"+spelling, func(t *testing.T) {
					f := newFixture(t)
					homeName, aliasName := "caf\u00e9", "cafe\u0301"
					if homeForm == "NFD" {
						homeName, aliasName = aliasName, homeName
					}
					base := f.opts.Home
					f.opts.Home = filepath.Join(base, homeName)
					cfg := f.install(t, RuntimeCursor)
					initial := scan(t, f).Entries[0]
					if initial.ConfigurationStatus != ConfigurationMatch || initial.ExecutableStatus != ExecutablePresent {
						t.Fatalf("fixture was not initially readable: %+v", initial)
					}
					path := cfg.RuntimeMCPConfigPath
					if target == "executable" {
						path = cfg.RuntimeCLICommand
					}
					cfg.TokenFile = filepath.Join(base, aliasName) + strings.TrimPrefix(path, f.opts.Home)
					if spelling == "cleaned_case" {
						cfg.TokenFile = filepath.Dir(cfg.TokenFile) + "/./" + strings.ToUpper(filepath.Base(cfg.TokenFile))
					}
					if strings.EqualFold(filepath.Clean(cfg.TokenFile), filepath.Clean(path)) {
						t.Fatal("fixture must evade case folding alone")
					}
					f.save(t, cfg)
					// Stat only synthetic fixtures to establish the filesystem alias;
					// production must reject using strings, without TokenFile I/O.
					original, err := os.Stat(path)
					if err != nil {
						t.Fatal(err)
					}
					if aliased, err := os.Stat(cfg.TokenFile); err == nil {
						if !os.SameFile(original, aliased) {
							t.Fatal("fixture Unicode alias does not identify the same file")
						}
						t.Log("SameFile=true; EqualFold(Clean)=false")
					} else if !os.IsNotExist(err) {
						t.Fatal(err)
					}
					old := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
					if err := os.Chtimes(path, old, old); err != nil {
						t.Fatal(err)
					}
					var before, after unix.Stat_t
					if err := unix.Stat(path, &before); err != nil {
						t.Fatal(err)
					}
					e := scan(t, f).Entries[0]
					if target == "configuration" {
						if e.ConfigurationStatus != ConfigurationUnsupported || e.ConfigurationScope != ScopeNone {
							t.Errorf("excluded configuration accepted: %+v", e)
						}
					} else if e.ExecutableStatus != ExecutableUnchecked {
						t.Errorf("excluded executable probed: %+v", e)
					}
					if err := unix.Stat(path, &after); err != nil {
						t.Fatal(err)
					}
					if before.Atim != after.Atim {
						t.Error("excluded file was read")
					}
					// The descriptor helpers consult context before traversing
					// descendants, including metadata-only executable probes.
					ctx := &cancellingContext{Context: context.Background(), remaining: 100}
					var excluded tokenFileExclusions
					excluded.add(cfg.TokenFile)
					if target == "configuration" {
						configurationStatus(ctx, f.opts, cfg, excluded)
					} else {
						executableStatus(ctx, f.opts, cfg, excluded)
					}
					if ctx.remaining != 100 {
						t.Error("excluded path reached filesystem traversal")
					}
				})
			}
		}
	}
}

func TestProviderAndExecutableSymlinks(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "fifo", "oversize", "unreadable", "directory", "ancestor"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			cfg := f.install(t, RuntimeCursor)
			if err := os.Remove(cfg.RuntimeMCPConfigPath); err != nil {
				t.Fatal(err)
			}
			var err error
			switch kind {
			case "symlink":
				err = os.Symlink(filepath.Join(f.opts.Home, "secret"), cfg.RuntimeMCPConfigPath)
			case "hardlink":
				target := filepath.Join(f.opts.Home, "secret")
				f.write(t, target, []byte("private"), 0600)
				err = os.Link(target, cfg.RuntimeMCPConfigPath)
			case "fifo":
				err = unix.Mkfifo(cfg.RuntimeMCPConfigPath, 0600)
			case "oversize":
				err = os.WriteFile(cfg.RuntimeMCPConfigPath, []byte(strings.Repeat(" ", providerLimit+1)), 0600)
			case "unreadable":
				err = os.WriteFile(cfg.RuntimeMCPConfigPath, []byte("private"), 0000)
			case "directory":
				err = os.Mkdir(cfg.RuntimeMCPConfigPath, 0700)
			case "ancestor":
				if err = os.Remove(cfg.RuntimeConfigRoot); err != nil {
					t.Fatal(err)
				}
				err = os.Symlink(filepath.Join(f.opts.Home, "secret-root"), cfg.RuntimeConfigRoot)
			}
			if err != nil {
				t.Fatal(err)
			}
			if scan(t, f).Entries[0].ConfigurationStatus != ConfigurationUnavailable {
				t.Fatal("unsafe provider config accepted")
			}
		})
	}
	f := newFixture(t)
	cfg := f.install(t, RuntimeCursor)
	if err := os.Remove(cfg.RuntimeCLICommand); err != nil {
		t.Fatal(err)
	}
	if scan(t, f).Entries[0].ExecutableStatus != ExecutableMissing {
		t.Fatal("missing executable")
	}
	if err := os.Symlink(filepath.Join(f.opts.Home, "secret"), cfg.RuntimeCLICommand); err != nil {
		t.Fatal(err)
	}
	if scan(t, f).Entries[0].ExecutableStatus != ExecutableUnchecked {
		t.Fatal("symlink executable")
	}
}

func TestCancellationAndConcurrentScans(t *testing.T) {
	f := newFixture(t)
	for _, r := range runtimes {
		f.install(t, r)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if r, err := Scan(ctx, f.opts); !errors.Is(err, context.Canceled) || len(r.Entries) != 0 {
		t.Fatal("cancel not honored")
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := Scan(context.Background(), f.opts)
			if err != nil || len(r.Entries) != 8 {
				t.Error("concurrent scan failed")
			}
		}()
	}
	wg.Wait()
}

func TestFixedDiscoveryAndEmptyJSON(t *testing.T) {
	f := newFixture(t)
	f.write(t, filepath.Join(f.opts.WitselfHome, "integrations", "unknown", "config.json"), []byte("private"), 0600)
	f.write(t, filepath.Join(f.opts.DSHHome, "config.json"), []byte("private"), 0600)
	r := scan(t, f)
	raw, _ := json.Marshal(r)
	if len(r.Entries) != 0 || r.ScanStatus != ScanComplete || !strings.Contains(string(raw), `"entries":[]`) {
		t.Fatal("unexpected discovery")
	}
	if err := os.RemoveAll(f.opts.WitselfHome); err != nil {
		t.Fatal(err)
	}
	if scan(t, f).ScanStatus != ScanComplete {
		t.Fatal("absent root not empty")
	}
	if err := os.Symlink(filepath.Join(f.opts.Home, "secret"), f.opts.WitselfHome); err != nil {
		t.Fatal(err)
	}
	if scan(t, f).ScanStatus != ScanUnavailable {
		t.Fatal("unsafe root accepted")
	}
}

func TestProductionImportBoundary(t *testing.T) {
	// An explicit source allowlist prevents hidden execution/network/credential
	// helpers from entering this standalone implementation through imports.
	allowed := map[string]bool{}
	for _, s := range []string{"bytes", "context", "encoding/json", "errors", "io", "os", "path/filepath", "reflect", "regexp", "strings", "time", "golang.org/x/sys/unix", "golang.org/x/text/unicode/norm", "github.com/pelletier/go-toml/v2"} {
		allowed[s] = true
	}
	for _, name := range []string{"inventory.go", "configuration.go", "files.go", "files_unix.go", "files_other.go"} {
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range file.Imports {
			if !allowed[strings.Trim(imp.Path.Value, `"`)] {
				t.Fatalf("forbidden import %s", imp.Path.Value)
			}
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			if id.Name == "os" {
				switch sel.Sel.Name {
				case "NewFile":
				default:
					t.Fatalf("unexpected os operation %s", sel.Sel.Name)
				}
			}
			return true
		})
	}
}

func TestReadRejectsTraversal(t *testing.T) {
	f := newFixture(t)
	root, err := openDirectory(f.opts.Home)
	if err != nil {
		t.Fatal(err)
	}
	defer root.close()
	for _, path := range []string{"../secret", "/absolute", "a//b", "a/./b", "a/../../b", "a\\b", "a\x00b", strings.Repeat("a/", 10) + "b"} {
		if _, err := root.read(context.Background(), path, recordLimit); err == nil {
			t.Fatal("traversal accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := root.read(ctx, "a/b", recordLimit); !errors.Is(err, context.Canceled) {
		t.Fatal("read cancellation lost")
	}
}

func TestReportKeys(t *testing.T) {
	f := newFixture(t)
	f.install(t, RuntimeCodex)
	raw, _ := json.Marshal(scan(t, f))
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	entry := doc["entries"].([]any)[0].(map[string]any)
	want := map[string]bool{"runtime": true, "recorded_version": true, "executable_status": true, "configuration_status": true, "configuration_scope": true, "effective_verification": true, "installed_at": true}
	got := map[string]bool{}
	for k := range entry {
		got[k] = true
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected public keys %v", got)
	}
}
