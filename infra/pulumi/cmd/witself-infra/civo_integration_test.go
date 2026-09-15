package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCivoEffectiveSettingsAreProviderAware(t *testing.T) {
	cloud := "civo"
	backend := "local"
	adminCIDR := "203.0.113.7/32"
	rows := effectiveSettings(cellEntry{
		Cloud:         &cloud,
		Backend:       &backend,
		CivoAdminCIDR: &adminCIDR,
	}, nil)

	values := map[string]string{}
	for _, row := range rows {
		values[row.key] = row.value
	}
	if got := values["k8s version"]; got != "latest stable (Civo default)" {
		t.Fatalf("k8s version = %q", got)
	}
	if got := values["ingress"]; got != "Civo DNS · Traefik NodePort" {
		t.Fatalf("ingress = %q", got)
	}
	if got := values["profile"]; got != "minimal development" {
		t.Fatalf("profile = %q", got)
	}
	prod := "prod"
	prodRows := effectiveSettings(cellEntry{
		Cloud:         &cloud,
		Backend:       &backend,
		Profile:       &prod,
		CivoAdminCIDR: &adminCIDR,
	}, nil)
	prodValues := map[string]string{}
	for _, row := range prodRows {
		prodValues[row.key] = row.value
	}
	if got := prodValues["profile"]; got != "prod (2 nodes)" {
		t.Fatalf("prod profile = %q", got)
	}
	unknown := "other"
	unknownRows := effectiveSettings(cellEntry{Cloud: &cloud, Profile: &unknown}, nil)
	unknownValues := map[string]string{}
	for _, row := range unknownRows {
		unknownValues[row.key] = row.value
	}
	if got := unknownValues["profile"]; got != unknown {
		t.Fatalf("unknown profile = %q, want visible raw value", got)
	}
	if got := values["database"]; got != "in-cluster PostgreSQL · persistent volume" {
		t.Fatalf("database = %q", got)
	}
	if got := values["node size"]; got != "g4s.kube.medium" {
		t.Fatalf("node size = %q", got)
	}
	if got := values["admin cidr"]; got != adminCIDR {
		t.Fatalf("admin cidr = %q", got)
	}
	for _, ignored := range []string{"db version", "cidr", "domain"} {
		if _, ok := values[ignored]; ok {
			t.Errorf("Civo TUI must not render ignored setting %q", ignored)
		}
	}
}

func TestConfigAddCivoCellRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "infra.yaml")
	tokenPath := filepath.Join(dir, "civo.token")
	if err := os.WriteFile(tokenPath, []byte("test-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fs := newTestFlagSet()
	if err := fs.Parse([]string{
		"-cloud", "civo",
		"-account-alias", "sandbox",
		"-region", "nyc1",
		"-role", "dev",
		"-backend", "local",
		"-civo-admin-cidr", "203.0.113.7/32",
		"-civo-node-size", "g4s.kube.medium",
		"-civo-token-file", tokenPath,
		"-civo-expected-account-id", "account-123",
		"-control-plane", "https://self.example.com",
		"-argocd",
		"-backup-validation-target",
	}); err != nil {
		t.Fatal(err)
	}
	if err := configAddCell(fs, path); err != nil {
		t.Fatal(err)
	}

	resolved := newTestFlagSet()
	if err := resolved.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if err := applyCellConfig(resolved, "civo-sandbox-use1-dev", path); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"cloud":                    "civo",
		"region":                   "nyc1",
		"backend":                  "local",
		"profile":                  "minimal",
		"civo-admin-cidr":          "203.0.113.7/32",
		"civo-node-size":           "g4s.kube.medium",
		"civo-token-file":          tokenPath,
		"civo-expected-account-id": "account-123",
		"control-plane":            "https://self.example.com",
		"argocd":                   "true",
		"backup-validation-target": "true",
	} {
		if got := resolved.Lookup(name).Value.String(); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

func TestConfigAddCivoProdProfileRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "infra.yaml")
	fs := newTestFlagSet()
	if err := fs.Parse([]string{
		"-cloud", "civo",
		"-account-alias", "sandbox",
		"-region", "nyc1",
		"-role", "prod",
		"-backend", "local",
		"-profile", "prod",
		"-civo-admin-cidr", "203.0.113.7/32",
	}); err != nil {
		t.Fatal(err)
	}
	if err := configAddCell(fs, path); err != nil {
		t.Fatal(err)
	}

	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), "profile: prod") {
		t.Fatalf("infra.yaml did not persist prod profile:\n%s", contents)
	}

	resolved := newTestFlagSet()
	if err := resolved.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if err := applyCellConfig(resolved, "civo-sandbox-use1-prod", path); err != nil {
		t.Fatal(err)
	}
	if got := resolved.Lookup("profile").Value.String(); got != "prod" {
		t.Fatalf("profile = %q, want prod", got)
	}
}

func TestConfigCivoK8sVersionRoundTrip(t *testing.T) {
	const cellName = "civo-sandbox-use1-backup"
	for _, tc := range []struct {
		name    string
		version string
		present bool
	}{
		{name: "absent"},
		{name: "pinned", version: "1.35.0-k3s1", present: true},
		{name: "explicit empty", present: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `version: 1
cells:
  civo-sandbox-use1-backup:
    cloud: civo
    account_alias: sandbox
    region: nyc1
    role: backup
    backend: local
    civo_node_size: g4s.kube.medium
    civo_admin_cidr: 203.0.113.7/32
`
			if tc.present {
				body += "    k8s_version: \"" + tc.version + "\"\n"
			}
			path := writeConfig(t, body)
			checkVersion := func() {
				t.Helper()
				cfg, _, err := loadInfraConfig(path)
				if err != nil {
					t.Fatal(err)
				}
				version := cfg.Cells[cellName].K8sVersion
				if (version != nil) != tc.present || (version != nil && *version != tc.version) {
					t.Fatalf("loaded version = %v, want present=%t value=%q", version, tc.present, tc.version)
				}
				// Each up/preview/cell-health invocation resolves a fresh flag
				// set from the inventory before constructing stack config.
				resolved := newTestFlagSet()
				if err := applyCellConfig(resolved, cellName, path); err != nil {
					t.Fatal(err)
				}
				if got := resolved.Lookup("k8s-version").Value.String(); got != tc.version {
					t.Fatalf("resolved version = %q, want %q", got, tc.version)
				}
			}
			checkVersion()
			// Automation health loads the same record without rewriting it.
			// This fixture has no control plane, so no network call is needed.
			if _, err := configuredHealthTargets(path, cellName, nil); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(raw) != body {
				t.Fatal("config resolution or health changed the cell inventory")
			}
			// Adding another record rewrites the inventory through the real
			// serializer; an existing pin (or its absence) must survive.
			added := newTestFlagSet()
			if err := added.Parse([]string{"-cloud", "aws", "-role", "dev"}); err != nil {
				t.Fatal(err)
			}
			if err := configAddCell(added, path); err != nil {
				t.Fatal(err)
			}
			checkVersion()
		})
	}
}

func TestConfigAddCivoCellRejectsIncompleteProviderConfig(t *testing.T) {
	for _, args := range [][]string{
		{"-cloud", "civo", "-region", "nyc1", "-backend", "local"},
		{"-cloud", "civo", "-region", "nyc1", "-civo-admin-cidr", "203.0.113.7/32"},
		{"-cloud", "civo", "-region", "nyc1", "-backend", "local", "-civo-admin-cidr", "203.0.113.7/32", "-profile", "other"},
		{"-cloud", "civo", "-region", "nyc1", "-backend", "local", "-civo-admin-cidr", "203.0.113.7/32", "-channel", "stable"},
		{"-cloud", "civo", "-region", "nyc1", "-backend", "local", "-civo-admin-cidr", "203.0.113.7/32", "-control-plane", "https://self.example.com"},
		{"-cloud", "civo", "-region", "nyc1", "-backend", "local", "-civo-admin-cidr", "not-a-cidr"},
	} {
		fs := newTestFlagSet()
		if err := fs.Parse(args); err != nil {
			t.Fatal(err)
		}
		if err := configAddCell(fs, filepath.Join(t.TempDir(), "infra.yaml")); err == nil {
			t.Fatalf("configAddCell(%v) unexpectedly succeeded", args)
		}
	}
}

func TestCivoRunAcceptsProdAndRejectsOtherProfiles(t *testing.T) {
	err := run([]string{
		"preview",
		"-cloud", "civo",
		"-region", "nyc1",
		"-backend", "local",
		"-profile", "prod",
	})
	if err == nil || !strings.Contains(err.Error(), "-civo-admin-cidr is required") {
		t.Fatalf("prod profile reached error %v, want admin CIDR validation", err)
	}

	err = run([]string{
		"preview",
		"-cloud", "civo",
		"-region", "nyc1",
		"-backend", "local",
		"-profile", "other",
	})
	if err == nil || !strings.Contains(err.Error(), "supports only -profile minimal or prod") {
		t.Fatalf("other profile error = %v", err)
	}
}

func TestResolveCivoTokenFileSecurity(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.token")
	if err := os.WriteFile(good, []byte("file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	token, err := resolveCivoToken(good)
	if err != nil || token != "file-token" {
		t.Fatalf("resolve secure token = %q, %v", token, err)
	}

	broad := filepath.Join(dir, "broad.token")
	if err := os.WriteFile(broad, []byte("file-token\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveCivoToken(broad); err == nil || !strings.Contains(err.Error(), "permissions") {
		t.Fatalf("world-readable token file error = %v", err)
	}
}

func TestConfigAddCivoRejectsTokenValueAsFileReference(t *testing.T) {
	path := filepath.Join(t.TempDir(), "infra.yaml")
	fs := newTestFlagSet()
	if err := fs.Parse([]string{
		"-cloud", "civo", "-region", "nyc1", "-backend", "local",
		"-civo-admin-cidr", "203.0.113.7/32",
		"-civo-token-file", "this-is-a-token-not-a-file",
	}); err != nil {
		t.Fatal(err)
	}
	if err := configAddCell(fs, path); err == nil {
		t.Fatal("token value passed as a file reference unexpectedly persisted")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("config file exists after rejected token value: %v", err)
	}
}

func TestWhoamiCivoAuthenticatesTokenWithoutPersistingIt(t *testing.T) {
	oldBase := civoAPIBaseURL
	t.Cleanup(func() { civoAPIBaseURL = oldBase })
	t.Setenv("CIVO_TOKEN", "test-only-token")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v2/auth/exchange" {
			t.Errorf("method/path = %s %q", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-only-token" {
			t.Errorf("authorization header = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"account_id":"account-123","access_token":"must-not-escape"}`))
	}))
	defer server.Close()
	civoAPIBaseURL = server.URL

	region := "nyc1"
	id, err := whoamiCivo(context.Background(), cellEntry{Region: &region})
	if err != nil {
		t.Fatal(err)
	}
	if id.Cloud != "civo" || id.Profile != region || id.Account != "account-123" || !id.OK {
		t.Fatalf("identity = %#v", id)
	}
	for _, field := range []string{id.Profile, id.Account, id.Actor, strings.Join(id.Notes, " ")} {
		if strings.Contains(field, "test-only-token") {
			t.Fatal("Civo token leaked into identity")
		}
	}
}

func TestWhoamiCivoRejectsWrongAccountPin(t *testing.T) {
	oldBase := civoAPIBaseURL
	t.Cleanup(func() { civoAPIBaseURL = oldBase })
	t.Setenv("CIVO_TOKEN", "test-only-token")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"account_id":"actual-account","access_token":"discard-me"}`))
	}))
	defer server.Close()
	civoAPIBaseURL = server.URL

	want := "expected-account"
	id, err := whoamiCivo(context.Background(), cellEntry{
		SecurityContext: &securityContext{Civo: &civoContext{ExpectedAccountID: &want}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if id.OK || id.Account != "actual-account" || !strings.Contains(strings.Join(id.Notes, " "), want) {
		t.Fatalf("identity = %#v", id)
	}
}

func TestBareCivoPreviewRejectsWrongAccountBeforeStateSetup(t *testing.T) {
	oldBase := civoAPIBaseURL
	t.Cleanup(func() { civoAPIBaseURL = oldBase })
	t.Setenv("CIVO_TOKEN", "test-only-token")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"account_id":"actual-account","access_token":"discard-me"}`))
	}))
	defer server.Close()
	civoAPIBaseURL = server.URL

	stateDir := filepath.Join(t.TempDir(), "state")
	err := run([]string{
		"preview",
		"-cloud", "civo",
		"-region", "nyc1",
		"-backend", "local",
		"-state-dir", stateDir,
		"-civo-admin-cidr", "203.0.113.7/32",
		"-civo-expected-account-id", "wrong-account",
	})
	if err == nil || !strings.Contains(err.Error(), "wrong-account") {
		t.Fatalf("wrong account error = %v", err)
	}
	if _, statErr := os.Stat(stateDir); !os.IsNotExist(statErr) {
		t.Fatalf("state dir created before account mismatch: %v", statErr)
	}
}

func TestCivoCellRejectsSecurityContextOverride(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	configPath := writeConfig(t, `version: 1
cells:
  civo-sandbox-use1-dev:
    cloud: civo
    account_alias: sandbox
    region: nyc1
    role: dev
    backend: local
    state_dir: `+stateDir+`
    civo_admin_cidr: 203.0.113.7/32
`)
	err := run([]string{
		"preview",
		"-cell", "civo-sandbox-use1-dev",
		"-config", configPath,
		"-civo-expected-account-id", "override-account",
	})
	if err == nil || !strings.Contains(err.Error(), "cannot override -cell security context") {
		t.Fatalf("security override error = %v", err)
	}
	if _, statErr := os.Stat(stateDir); !os.IsNotExist(statErr) {
		t.Fatalf("state dir created before security override rejection: %v", statErr)
	}
}

func TestAuthCommandExplainsCivoTokenFlow(t *testing.T) {
	cloud := "civo"
	cmd, description := authCommand(context.Background(), cellState{
		entry: cellEntry{Cloud: &cloud},
	})
	if cmd != nil {
		t.Fatal("Civo must not invent an interactive login command")
	}
	if !strings.Contains(description, "CIVO_TOKEN") {
		t.Fatalf("description = %q", description)
	}
}

func TestCivoBootstrapInitializesOnlyLocalState(t *testing.T) {
	t.Setenv("CIVO_TOKEN", "")
	stateDir := filepath.Join(t.TempDir(), "state")
	if err := run([]string{
		"bootstrap",
		"-cloud", "civo",
		"-region", "nyc1",
		"-backend", "local",
		"-state-dir", stateDir,
	}); err != nil {
		t.Fatal(err)
	}
	passphrasePath := filepath.Join(stateDir, "passphrase")
	contents, err := os.ReadFile(passphrasePath)
	if err != nil {
		t.Fatalf("local backend passphrase not initialized: %v", err)
	}
	if strings.TrimSpace(string(contents)) == "" {
		t.Fatal("local backend passphrase is empty")
	}
	info, err := os.Stat(passphrasePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("passphrase permissions = %o, want 600", info.Mode().Perm())
	}
}
